"""Tied consumers cannot keep storage released by their embedding peer."""

import importlib
from dataclasses import replace
from pathlib import Path

import pytest

from gpustack_model_transfer.core.budgets import ReadBudgets
from gpustack_model_transfer.core.capabilities import local_backend_capabilities
from gpustack_model_transfer.core.local_reader import LocalFileReader


@pytest.mark.parametrize(
    "adapter,loader_name",
    [
        ("vllm", "ModelTransferModelLoader"),
        ("vllm_ascend", "ModelTransferAscendLoader"),
        ("sglang", "ModelTransferSGLangLoader"),
    ],
)
@pytest.mark.parametrize("alias_first", [False, True])
def test_borrowed_alias_keeps_owned_embedding(adapter, loader_name, alias_first):
    prefix = "gpustack_model_transfer.adapters." + adapter
    ownership = importlib.import_module(prefix + ".ownership")
    runtime = importlib.import_module(prefix + ".runtime")
    loader_type = getattr(importlib.import_module(prefix + ".loader"), loader_name)
    root = Path(__file__).resolve().parents[3] / "contracts/fixtures/tiny_dense"
    reader = LocalFileReader(root / "manifest.json")
    head, embed = "lm_head.weight", "model.embed_tokens.weight"
    reader.manifest = replace(
        reader.manifest,
        tensors=tuple(
            sorted(
                reader.manifest.tensors,
                key=lambda t: (t.name != head) if alias_first else (t.name == head),
            )
        ),
    )
    borrowed = set()
    fenced, released, reads = [], [], []
    read_bytes = reader.read_bytes

    def record_read(name, begin, end):
        reads.append((name, begin, end))
        return read_bytes(name, begin, end)

    reader.read_bytes = record_read

    class RecyclingRuntime(runtime.ReferenceRuntime):
        def store(self, model, entry, payload, ownership):
            if ownership.value == "borrowed":
                borrowed.add(entry.tensor.name)
            return super().store(model, entry, payload, ownership)

    driver = RecyclingRuntime(root)

    def release(name):
        released.append(name)
        if name == embed and name in borrowed:
            driver.last_model.parameters[embed].data[:] = bytes(
                len(driver.last_model.parameters[embed].data)
            )
        return name != head

    capabilities = replace(local_backend_capabilities(), tensor_ownership="borrowed")
    loader = loader_type(
        reader,
        capabilities,
        ReadBudgets(window_bytes=32, max_inflight=2, owned_output_bytes=4096),
    )
    report = loader.load(
        driver,
        ownership.LoadOptions(
            ownership=ownership.TensorOwnership.BORROWED,
            tie_word_embeddings=True,
            borrowed=ownership.BorrowedContract("device:0", fenced.append, release),
        ),
    )
    tensor = reader.manifest.tensor(embed)
    expected = (root / tensor.file).read_bytes()[tensor.data_begin : tensor.data_end]
    assert bytes(report.model.parameters[head].data) == expected
    assert report.model.parameters[head] is report.model.parameters[embed]
    assert embed in report.borrowed_fallbacks
    assert head not in report.borrowed_fallbacks
    assert head not in fenced and head not in released
    assert embed in fenced and embed in released
    head_tensor = reader.manifest.tensor(head)
    assert not any(
        name == head_tensor.file
        and begin < head_tensor.data_end
        and end > head_tensor.data_begin
        for name, begin, end in reads
    )
    assert any(
        name == tensor.file and begin < tensor.data_end and end > tensor.data_begin
        for name, begin, end in reads
    )
