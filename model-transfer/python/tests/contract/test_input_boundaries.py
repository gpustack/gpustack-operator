"""Input failures preserve typed errors, file ownership and engine preconditions."""

import errno
import importlib
import os
from concurrent.futures import ThreadPoolExecutor
from contextlib import closing
from dataclasses import replace
from threading import Barrier
from types import SimpleNamespace

import fixtureutil
import pytest

from gpustack_model_transfer.core.errors import (
    InvalidDescriptorError,
    OperationStateError,
    SourceUnavailableError,
)
from gpustack_model_transfer.core.local_reader import LocalFileReader
from gpustack_model_transfer.core.manifest import parse_manifest
from gpustack_model_transfer.core.manifest_pages import local_page_reader


def test_manifest_invalid_utf8_is_a_descriptor_error():
    with pytest.raises(InvalidDescriptorError, match="not valid JSON"):
        parse_manifest(b"\xff")


@pytest.mark.parametrize("kind", ["missing", "symlink"])
def test_local_page_open_failure_is_typed(tmp_path, kind):
    page = {"sha256": "0" * 64, "size": 2}
    if kind == "symlink":
        target = tmp_path / "target"
        target.write_bytes(b"[]")
        (tmp_path / (page["sha256"] + ".json")).symlink_to(target)
    with pytest.raises(SourceUnavailableError, match="cannot open a manifest page"):
        local_page_reader(tmp_path)(page)


def test_local_page_wrap_failure_closes_raw_descriptor(tmp_path, monkeypatch):
    page = {"sha256": "0" * 64, "size": 2}
    (tmp_path / (page["sha256"] + ".json")).write_bytes(b"[]")
    opened = []

    def refuse_wrap(fd, mode):
        opened.append(fd)
        raise OSError("wrap failed")

    monkeypatch.setattr(os, "fdopen", refuse_wrap)
    with pytest.raises(OSError, match="wrap failed"):
        local_page_reader(tmp_path)(page)
    assert len(opened) == 1
    with pytest.raises(OSError) as failure:
        os.fstat(opened[0])
    assert failure.value.errno == errno.EBADF


def test_concurrent_local_ranges_do_not_share_a_cursor():
    with closing(LocalFileReader(fixtureutil.manifest_path())) as reader:
        path = reader.manifest.files[0].path
        handle = reader._handles[path]
        barrier = Barrier(2)

        class InterleavedFile:
            def fileno(self):
                return handle.fileno()

            def seek(self, offset):
                handle.seek(offset)
                barrier.wait(timeout=5)

            def read(self, size):
                return handle.read(size)

            def close(self):
                handle.close()

        reader._handles[path] = InterleavedFile()
        ranges = [(0, 8), (16, 24)]
        with ThreadPoolExecutor(max_workers=2) as pool:
            futures = [pool.submit(reader.read_bytes, path, a, b) for a, b in ranges]
            actual = [future.result(timeout=5) for future in futures]
        blob = (fixtureutil.fixture_dir() / path).read_bytes()
        assert actual == [blob[a:b] for a, b in ranges]


@pytest.mark.parametrize("engine", ["vllm", "sglang", "vllm_ascend"])
def test_header_cannot_hide_a_tensor_from_the_manifest(engine):
    metadata = importlib.import_module(
        f"gpustack_model_transfer.adapters.{engine}.metadata"
    )
    with closing(LocalFileReader(fixtureutil.manifest_path())) as reader:
        reader.manifest = replace(reader.manifest, tensors=reader.manifest.tensors[:-1])
        with pytest.raises(InvalidDescriptorError, match="undeclared tensors"):
            metadata.probe_checkpoint_metadata(reader)


@pytest.mark.parametrize("engine", ["vllm", "sglang", "vllm_ascend"])
def test_kv_profile_requires_observed_finalization(engine):
    loader = importlib.import_module(
        f"gpustack_model_transfer.adapters.{engine}.loader"
    )
    ledger = loader.ResourceLedger()
    ledger.session_closed = True
    with pytest.raises(OperationStateError, match="finalized exactly once"):
        loader.profile_kv_capacity(None, SimpleNamespace(), ledger)
    assert not ledger.kv_profiled


def test_empty_torch_store_preserves_mutation_bookkeeping():
    from gpustack_model_transfer.adapters.vllm.torch_runtime import TorchRuntimeDriver

    driver = TorchRuntimeDriver()
    driver._torch = SimpleNamespace()
    buffer = object()
    model = SimpleNamespace(parameters={"empty": buffer}, mutated=False)
    entry = SimpleNamespace(tensor=SimpleNamespace(name="empty"))
    assert driver.store(model, entry, b"") == "owned"
    assert model.parameters["empty"] is buffer
    assert model.mutated
    assert driver.stores_done == 1
    assert driver.last_model is model


def test_npu_contiguous_finalization_preserves_tied_identity():
    from gpustack_model_transfer.adapters.vllm_ascend.npu_runtime import (
        NPURuntimeDriver,
    )

    class Buffer:
        def __init__(self, contiguous=False):
            self.ready = contiguous
            self.copies = 0

        def is_contiguous(self):
            return self.ready

        def contiguous(self):
            self.copies += 1
            return Buffer(True)

    original = Buffer()
    model = SimpleNamespace(parameters={"embedding": original, "head": original})
    driver = NPURuntimeDriver()
    driver._torch = SimpleNamespace()
    driver._torch_npu = SimpleNamespace()
    driver._run_finalization_hook(model, "contiguous")
    assert model.parameters["embedding"] is model.parameters["head"]
    assert model.parameters["head"].is_contiguous()
    assert original.copies == 1


def test_acceptance_help_exits_successfully(capsys):
    from gpustack_model_transfer.acceptance.runner import main

    assert main(["--help"]) == 0
    assert "usage:" in capsys.readouterr().out


def test_modelexpress_metadata_checks_do_not_allocate_a_runtime():
    from gpustack_model_transfer.acceptance import modelexpress_suite

    calls = []

    def unavailable():
        calls.append(True)
        raise RuntimeError("runtime unavailable")

    results = modelexpress_suite.run({}, runtime=unavailable)
    assert [result.status for result in results[:2]] == ["PASS", "PASS"]
    assert all(result.status == "NO-GO" for result in results[2:])
    assert len(calls) == len(results) - 2


@pytest.mark.parametrize("engine", ["vllm", "sglang", "vllm_ascend"])
def test_manifest_undeclared_file_is_rejected_before_header_io(engine, monkeypatch):
    metadata = importlib.import_module(
        f"gpustack_model_transfer.adapters.{engine}.metadata"
    )
    with closing(LocalFileReader(fixtureutil.manifest_path())) as reader:
        tensors = list(reader.manifest.tensors)
        tensors[0] = replace(tensors[0], file="undeclared.safetensors")
        reader.manifest = replace(reader.manifest, tensors=tuple(tensors))
        reads = []
        monkeypatch.setattr(reader, "read_bytes", lambda *args: reads.append(args))
        with pytest.raises(InvalidDescriptorError, match="references undeclared file"):
            metadata.probe_checkpoint_metadata(reader)
        assert not reads


def test_sglang_store_uses_the_allocated_parameter_dtype():
    from gpustack_model_transfer.adapters.sglang.torch_runtime import TorchRuntimeDriver

    observed, copies = [], []
    dtype = object()
    raw = SimpleNamespace(reshape=lambda shape: ("reshaped", shape))

    def frombuffer(payload, *, dtype):
        observed.append((payload, dtype))
        return raw

    driver = TorchRuntimeDriver()
    driver._torch = SimpleNamespace(frombuffer=frombuffer)
    parameter = SimpleNamespace(dtype=dtype, shape=(1,), copy_=copies.append)
    model = SimpleNamespace(parameters={"weight": parameter}, mutated=False)
    entry = SimpleNamespace(tensor=SimpleNamespace(name="weight"))
    assert driver.store(model, entry, b"abcd") == "owned"
    assert observed == [(bytearray(b"abcd"), dtype)]
    assert copies == [("reshaped", (1,))]
    assert model.mutated
