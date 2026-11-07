"""The real-host driver: torch and sglang, imported lazily, inside methods.

Nothing in this module may import torch or sglang at module scope: the
adapter must import on a CPU-only stdlib checkout, and a GPU host learns
whether its prerequisites exist through probe_host_driver(), which reports
what is missing instead of crashing on the import.
"""

from dataclasses import dataclass

from gpustack_model_transfer.adapters.sglang.ownership import TensorOwnership

_DTYPE_TO_TORCH = {
    "F32": "float32",
    "F16": "float16",
    "BF16": "bfloat16",
    "I64": "int64",
}


class PrerequisiteUnavailableError(Exception):
    """The host lacks torch, sglang, or a CUDA device; the reason says which."""


@dataclass(frozen=True)
class HostReadiness:
    """What the host has, and what is missing for the real load path."""

    torch_available: bool
    sglang_available: bool
    cuda_available: bool
    missing: tuple


def probe_host_readiness() -> HostReadiness:
    """Check torch, sglang, and CUDA without importing them eagerly."""
    missing = []
    torch_available = _importable("torch")
    sglang_available = _importable("sglang")
    cuda_available = False
    if not torch_available:
        missing.append("torch")
    if not sglang_available:
        missing.append("sglang")
    if torch_available:
        import torch

        cuda_available = bool(torch.cuda.is_available())
        if not cuda_available:
            missing.append("cuda")
    return HostReadiness(
        torch_available=torch_available,
        sglang_available=sglang_available,
        cuda_available=cuda_available,
        missing=tuple(missing),
    )


def probe_host_driver():
    """Return a real-host driver, or (None, reason) when unready.

    The acceptance suite treats (None, reason) as UNRUN for every
    capability: missing prerequisites fail the run rather than becoming a
    skipped test counted as a pass.
    """
    readiness = probe_host_readiness()
    if readiness.missing:
        return None, ("missing prerequisites: " + ", ".join(readiness.missing))
    return TorchRuntimeDriver(), ""


def _importable(name: str) -> bool:
    import importlib.util

    try:
        return importlib.util.find_spec(name) is not None
    except (ImportError, ValueError):
        return False


class TorchRuntimeDriver:
    """The ModelDriver surface implemented on torch and SGLang.

    Every torch import happens inside a method, so constructing or probing
    this module on a CPU-only host costs nothing and refuses cleanly.
    """

    def __init__(self):
        self._torch = None
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
        from gpustack_model_transfer.adapters.sglang.runtime import ModelHandle

        model = ModelHandle(tie_word_embeddings=options.tie_word_embeddings)
        storages = {}
        for entry in plan.entries:
            if entry.role in ("tied-alias", "engine-metadata"):
                continue
            dtype = getattr(torch, _DTYPE_TO_TORCH[entry.tensor.dtype])
            shape = tuple(entry.tensor.shape)
            # A zero-element tensor keeps its dtype and shape with no data.
            # Parameters live on the target device: this route gates on
            # CUDA, and staging a parameter on the host would double the
            # resident footprint the budgets admitted.
            buffer = torch.empty(shape, dtype=dtype, device="cuda")
            storages[entry.tensor.name] = buffer
            model.parameters[entry.tensor.name] = buffer
        if options.tie_word_embeddings:
            for alias_name, target in plan.tied_aliases:
                model.parameters[alias_name] = storages[target]
        if options.engine_config is not None:
            model.connector_config = options.engine_config
        self.last_model = model
        return model

    def store(self, model, entry, payload, ownership="owned") -> str:
        if ownership is TensorOwnership.BORROWED:
            return "fell-back-to-owned"
        torch = self._require_torch()
        buffer = model.parameters[entry.tensor.name]
        if not payload:
            # A zero-element tensor keeps its empty storage: frombuffer
            # refuses a zero-byte buffer, and there is nothing to copy.
            model.mutated = True
            return "owned"
        # frombuffer exposes writable storage; copy the immutable payload before
        # giving Torch a view, then copy into the owned device parameter.
        raw = torch.frombuffer(bytearray(payload), dtype=buffer.dtype)
        buffer.copy_(raw.reshape(buffer.shape))
        model.mutated = True
        return "owned"

    def copy_to_owned(self, model, entry, payload) -> None:
        self.store(model, entry, payload)

    def store_engine_metadata(self, model, entry, payload) -> None:
        from gpustack_model_transfer.adapters.sglang.runtime import decode_values

        values = decode_values(entry.tensor.dtype, bytes(payload))
        model.engine_metadata[entry.tensor.name] = values[0] if values else None

    def finalize(self, model) -> None:
        # SGLang's post-load weight processing; real finalization is
        # engine-version specific and stays on the host.
        model.finalization_count += 1

    def forward(self, model, inputs) -> list:
        raise PrerequisiteUnavailableError(
            "forward equivalence runs on the engine host"
        )

    def profile_kv_capacity(self, model) -> int:
        raise PrerequisiteUnavailableError("KV profiling runs on the engine host")


__all__ = [
    "HostReadiness",
    "PrerequisiteUnavailableError",
    "TorchRuntimeDriver",
    "probe_host_driver",
    "probe_host_readiness",
]
