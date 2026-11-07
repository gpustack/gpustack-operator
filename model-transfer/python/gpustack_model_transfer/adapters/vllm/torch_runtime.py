"""Torch-backed CPU reference fixture, not a native vLLM loader.

Nothing in this module may import torch or vllm at module scope: the
adapter must import on a CPU-only stdlib checkout, and a GPU host learns
whether its prerequisites exist through probe_host_driver(), which reports
what is missing instead of crashing on the import.
"""

from dataclasses import dataclass

_DTYPE_TO_TORCH = {
    "F32": "float32",
    "F16": "float16",
    "BF16": "bfloat16",
    "I64": "int64",
}


class PrerequisiteUnavailableError(Exception):
    """The host lacks torch, vllm, or a CUDA device; the reason says which."""


@dataclass(frozen=True)
class HostReadiness:
    """What the host has, and what is missing for the real load path."""

    torch_available: bool
    vllm_available: bool
    cuda_available: bool
    missing: tuple


def probe_host_readiness() -> HostReadiness:
    """Check torch, vllm, and CUDA without importing them eagerly."""
    missing = []
    torch_available = _importable("torch")
    vllm_available = _importable("vllm")
    cuda_available = False
    if not torch_available:
        missing.append("torch")
    if not vllm_available:
        missing.append("vllm")
    if torch_available:
        import torch

        cuda_available = bool(torch.cuda.is_available())
        if not cuda_available:
            missing.append("cuda")
    return HostReadiness(
        torch_available=torch_available,
        vllm_available=vllm_available,
        cuda_available=cuda_available,
        missing=tuple(missing),
    )


def probe_host_driver():
    """Refuse real-host acceptance: this fixture builds only a CPU handle."""
    return None, "CPU reference fixture does not load a native vLLM model"


def _importable(name: str) -> bool:
    import importlib.util

    try:
        return importlib.util.find_spec(name) is not None
    except (ImportError, ValueError):
        return False


class TorchRuntimeDriver:
    """The reference ModelDriver surface implemented with CPU torch tensors.

    Every torch import happens inside a method, so constructing or probing
    this module on a CPU-only host costs nothing and refuses cleanly.
    """

    def __init__(self):
        self._torch = None
        self._models = {}
        # Mutation bookkeeping, kept exactly as the reference runtime
        # keeps it: a fallback decision reads these counters to decide
        # whether the engine's parameters were already touched.
        self.stores_done = 0
        self.last_model = None

    def _require_torch(self):
        if self._torch is None:
            try:
                import torch
            except ImportError as exc:
                raise PrerequisiteUnavailableError(
                    f"torch is required on a real host: {exc}"
                ) from None
            self._torch = torch
        return self._torch

    def build_model(self, plan, options):
        torch = self._require_torch()
        from gpustack_model_transfer.adapters.vllm.runtime import ModelHandle

        model = ModelHandle(tie_word_embeddings=options.tie_word_embeddings)
        storages = {}
        for entry in plan.entries:
            if entry.role in ("tied-alias", "engine-metadata"):
                continue
            dtype = getattr(torch, _DTYPE_TO_TORCH[entry.tensor.dtype])
            shape = tuple(entry.tensor.shape)
            # A zero-element tensor keeps its dtype and shape with no data.
            buffer = torch.empty(shape, dtype=dtype)
            storages[entry.tensor.name] = buffer
            model.parameters[entry.tensor.name] = buffer
        if options.tie_word_embeddings:
            for alias_name, target in plan.tied_aliases:
                model.parameters[alias_name] = storages[target]
        if options.engine_config is not None:
            model.connector_config = options.engine_config
        self._models[id(model)] = model
        self.last_model = model
        return model

    def store(self, model, entry, payload, ownership="owned") -> str:
        torch = self._require_torch()
        buffer = model.parameters[entry.tensor.name]
        if payload:
            dtype = str(buffer.dtype).replace("torch.", "")
            raw = torch.frombuffer(bytearray(payload), dtype=_torch_dtype(torch, dtype))
            buffer.copy_(raw.reshape(buffer.shape))
        model.mutated = True
        self.stores_done += 1
        self.last_model = model
        return "owned"

    def copy_to_owned(self, model, entry, payload) -> None:
        self.store(model, entry, payload)

    def store_engine_metadata(self, model, entry, payload) -> None:
        from gpustack_model_transfer.adapters.vllm.runtime import decode_values

        values = decode_values(entry.tensor.dtype, bytes(payload))
        model.engine_metadata[entry.tensor.name] = values[0] if values else None

    def finalize(self, model) -> None:
        # Fixture bookkeeping only; no engine finalization occurs here.
        model.finalization_count += 1

    def forward(self, model, inputs) -> list:
        raise PrerequisiteUnavailableError(
            "forward equivalence runs on the engine host"
        )

    def profile_kv_capacity(self, model) -> int:
        raise PrerequisiteUnavailableError("KV profiling runs on the engine host")


def _torch_dtype(torch, name: str):
    mapping = {
        "float32": torch.float32,
        "float16": torch.float16,
        "bfloat16": torch.bfloat16,
        "int64": torch.int64,
    }
    return mapping[name]


__all__ = [
    "HostReadiness",
    "PrerequisiteUnavailableError",
    "TorchRuntimeDriver",
    "probe_host_driver",
    "probe_host_readiness",
]
