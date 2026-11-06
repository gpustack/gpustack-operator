"""Reference drivers and CPU fixture model, not a native vLLM model.

ReferenceRuntime uses bytearrays and a small deterministic network for tests.
TorchRuntimeDriver is also a fixture; it does not create or finalize vLLM.
"""

import struct
from dataclasses import dataclass, field

from gpustack_model_transfer.adapters.vllm.ownership import TensorOwnership
from gpustack_model_transfer.core.errors import DeniedContentError
from gpustack_model_transfer.core.manifest import load_manifest

_DECODE_FORMATS = {
    "F32": ("<", 4, "f"),
    "F16": ("<", 2, "e"),
    "I64": ("<", 8, "q"),
}


def decode_values(dtype: str, payload: bytes) -> list:
    """Decode verified bytes into plain Python numbers.

    BF16 is the one dtype struct cannot name: its two bytes are the high
    half of an F32, so they pad into an F32 and decode from there.
    """
    if dtype == "BF16":
        if len(payload) % 2:
            raise ValueError("BF16 payload is not a whole number of elements")
        expanded = bytearray()
        for index in range(0, len(payload), 2):
            expanded += b"\x00\x00" + payload[index : index + 2]
        return list(struct.unpack(f"<{len(payload) // 2}f", bytes(expanded)))
    if dtype not in _DECODE_FORMATS:
        raise ValueError(f"unsupported dtype for decoding: {dtype!r}")
    endian, width, code = _DECODE_FORMATS[dtype]
    if len(payload) % width:
        raise ValueError(f"{dtype} payload is not a whole number of elements")
    count = len(payload) // width
    if count == 0:
        return []
    return list(struct.unpack(f"{endian}{count}{code}", payload))


class ParameterBuffer:
    """One parameter storage: dtype, shape, and the raw bytes."""

    def __init__(self, name: str, dtype: str, shape: tuple):
        self.name = name
        self.dtype = dtype
        self.shape = tuple(shape)
        self.data = bytearray()

    def __repr__(self) -> str:
        return (
            f"ParameterBuffer({self.name!r}, {self.dtype}, "
            f"{self.shape}, {len(self.data)}B)"
        )


@dataclass
class ModelHandle:
    """Reference fixture state, not a vLLM model.

    mutated is the taint flag: once any parameter store has run, a failed
    load leaves the process in a state only a verified reset may reuse.
    """

    parameters: dict = field(default_factory=dict)
    tie_word_embeddings: bool = False
    mutated: bool = False
    finalization_count: int = 0
    engine_metadata: dict = field(default_factory=dict)
    connector_config: dict = None
    built_by: str = "model-transfer"


class ReferenceRuntime:
    """The stdlib reference driver for tests and the injected suite mode.

    corrupt_norm, when true, perturbs the reference model's norm parameter so
    the equivalence comparison has a way to fail; a comparison that cannot
    fail proves nothing.
    """

    def __init__(
        self,
        root_dir,
        *,
        device_budget_bytes: int = 1 << 16,
        tie_word_embeddings: bool = False,
        consumer_retains: bool = False,
        fail_store_after: int | None = None,
        store_error: Exception | None = None,
        corrupt_norm: bool = False,
        record_events: list | None = None,
    ):
        self.root_dir = str(root_dir)
        self.manifest = load_manifest(f"{self.root_dir}/manifest.json")
        self.device_budget_bytes = device_budget_bytes
        self.tie_word_embeddings = tie_word_embeddings
        self.consumer_retains = consumer_retains
        self.fail_store_after = fail_store_after
        self.store_error = store_error
        self.corrupt_norm = corrupt_norm
        self.events = record_events if record_events is not None else []
        self.built = False
        self.stores_done = 0
        self.last_model = None

    # -- the driver surface the loader drives --------------------------

    def build_model(self, plan, options) -> ModelHandle:
        """Create every parameter storage, with tying applied up front."""
        model = ModelHandle(tie_word_embeddings=options.tie_word_embeddings)
        storages = {}
        for entry in plan.entries:
            if entry.role in ("tied-alias", "engine-metadata"):
                continue
            buffer = ParameterBuffer(
                entry.tensor.name, entry.tensor.dtype, entry.tensor.shape
            )
            model.parameters[entry.tensor.name] = buffer
            storages[entry.tensor.name] = buffer
        if options.tie_word_embeddings:
            for alias_name, target in plan.tied_aliases:
                # lm_head.weight is not its own storage: it shares the
                # embedding's, exactly the native tied-embedding shape.
                model.parameters[alias_name] = storages[target]
        if options.engine_config is not None:
            model.connector_config = options.engine_config
        self.built = True
        self.last_model = model
        self.events.append("build")
        return model

    def store(
        self,
        model: ModelHandle,
        entry,
        payload: bytes,
        ownership: TensorOwnership = TensorOwnership.OWNED,
    ) -> str:
        """Place verified bytes into the named parameter storage."""
        self.events.append(f"store:{entry.tensor.name}")
        if (
            self.fail_store_after is not None
            and self.stores_done >= self.fail_store_after
        ):
            raise self.store_error
        buffer = model.parameters.get(entry.tensor.name)
        if buffer is None:
            raise DeniedContentError(
                f"the model has no parameter for tensor {entry.tensor.name!r}"
            )
        if ownership is TensorOwnership.BORROWED and self.consumer_retains:
            # The consumer keeps the bytes: borrowed cannot be honored.
            self.stores_done += 1
            return "fell-back-to-owned"
        buffer.data = bytearray(payload)
        self.stores_done += 1
        model.mutated = True
        return "owned"

    def copy_to_owned(self, model: ModelHandle, entry, payload: bytes) -> None:
        """Copy a retained borrowed tensor into owned storage."""
        buffer = model.parameters[entry.tensor.name]
        buffer.data = bytearray(payload)
        model.mutated = True

    def store_engine_metadata(self, model: ModelHandle, entry, payload: bytes) -> None:
        values = decode_values(entry.tensor.dtype, payload)
        model.engine_metadata[entry.tensor.name] = values[0] if values else None

    def finalize(self, model: ModelHandle) -> None:
        """The engine's process_weights_after_loading, once per load."""
        model.finalization_count += 1
        self.events.append("finalize")

    def forward(self, model: ModelHandle, inputs: list) -> list:
        """A small deterministic network over the loaded parameters."""
        embed = decode_values(
            "F32", bytes(model.parameters["model.embed_tokens.weight"].data)
        )
        width = model.parameters["model.embed_tokens.weight"].shape[1]
        vocab = model.parameters["model.embed_tokens.weight"].shape[0]
        q_proj = self._matrix(model, "model.layers.0.attn.q_proj.weight")
        norm = decode_values("F32", bytes(model.parameters["model.norm.weight"].data))
        head = self._matrix(model, "lm_head.weight")
        rows = []
        for token in inputs:
            row = embed[token * width : (token + 1) * width]
            row = [
                sum(row[idx] * q_proj[idx][b] for idx in range(width))
                for b in range(width)
            ]
            row = [row[b] * norm[b] for b in range(width)]
            rows.append(row)
        logits = []
        for row in rows:
            logits.append(
                [sum(row[b] * head[v][b] for b in range(width)) for v in range(vocab)]
            )
        return [value for row in logits for value in row]

    def profile_kv_capacity(self, model: ModelHandle) -> int:
        """Usable KV bytes: the device budget minus resident parameters."""
        retained = sum(len(buffer.data) for buffer in self._distinct_storages(model))
        return self.device_budget_bytes - retained

    # -- helpers the tests and the equivalence checks use --------------

    def build_native_model(self) -> ModelHandle:
        """Build the legacy-named reference fixture from local checkpoint bytes."""
        model = ModelHandle(
            tie_word_embeddings=self.tie_word_embeddings,
            finalization_count=1,
            built_by="native",
        )
        for tensor in self.manifest.tensors:
            if tensor.name.startswith("metadata."):
                continue
            if self.tie_word_embeddings and tensor.name == "lm_head.weight":
                continue
            with open(f"{self.root_dir}/{tensor.file}", "rb") as handle:
                handle.seek(tensor.data_begin)
                payload = handle.read(tensor.data_end - tensor.data_begin)
            buffer = ParameterBuffer(tensor.name, tensor.dtype, tensor.shape)
            buffer.data = bytearray(payload)
            model.parameters[tensor.name] = buffer
        if self.tie_word_embeddings and "model.embed_tokens.weight" in model.parameters:
            model.parameters["lm_head.weight"] = model.parameters[
                "model.embed_tokens.weight"
            ]
        if self.corrupt_norm:
            self.perturb_parameter(model, "model.norm.weight")
        self.last_model = model
        return model

    def model_parameter_bytes(self, model: ModelHandle, name: str) -> bytes:
        return bytes(model.parameters[name].data)

    def parameter_values(self, model: ModelHandle, name: str) -> list:
        buffer = model.parameters[name]
        return decode_values(buffer.dtype, bytes(buffer.data))

    def alias_tied(self, model: ModelHandle) -> bool:
        head = model.parameters.get("lm_head.weight")
        embed = model.parameters.get("model.embed_tokens.weight")
        return head is embed

    def model_finalization_count(self, model: ModelHandle) -> int:
        return model.finalization_count

    def model_mutated(self, model: ModelHandle) -> bool:
        return model.mutated

    def perturb_parameter(self, model: ModelHandle, name: str) -> None:
        buffer = model.parameters[name]
        values = decode_values(buffer.dtype, bytes(buffer.data))
        perturbed = [value * 1.5 for value in values]
        self._encode_into(buffer, perturbed)

    def _matrix(self, model: ModelHandle, name: str) -> list:
        buffer = model.parameters[name]
        flat = decode_values(buffer.dtype, bytes(buffer.data))
        rows, columns = buffer.shape
        return [flat[row * columns : (row + 1) * columns] for row in range(rows)]

    def _encode_into(self, buffer: ParameterBuffer, values: list) -> None:
        if buffer.dtype == "F32":
            buffer.data = bytearray(struct.pack(f"<{len(values)}f", *values))
        elif buffer.dtype == "F16":
            buffer.data = bytearray(struct.pack(f"<{len(values)}e", *values))
        else:
            raise ValueError(f"cannot encode {buffer.dtype} in the reference")

    def _distinct_storages(self, model: ModelHandle) -> list:
        seen = []
        for buffer in model.parameters.values():
            if all(buffer is not other for other in seen):
                seen.append(buffer)
        return seen


__all__ = [
    "ModelHandle",
    "ParameterBuffer",
    "ReferenceRuntime",
    "decode_values",
]
