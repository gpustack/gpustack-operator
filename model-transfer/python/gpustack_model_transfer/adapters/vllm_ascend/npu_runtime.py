"""NPU tensor fixture, not a native vLLM-Ascend model loader.

Nothing in this module may import torch, torch_npu, or vllm_ascend at
module scope: the adapter must import on a CPU-only stdlib checkout, and
an Ascend host learns whether its stack exists through probe_host_driver(),
which reports what is missing instead of crashing on the import.

Readiness and the fixture's finalization counter are not model-load acceptance.
"""

from dataclasses import dataclass

_DTYPE_TO_TORCH = {
    "F32": "float32",
    "F16": "float16",
    "BF16": "bfloat16",
    "I64": "int64",
}


class PrerequisiteUnavailableError(Exception):
    """The host lacks torch, torch_npu, or vllm_ascend; the reason says which."""


@dataclass(frozen=True)
class HostReadiness:
    """Host availability only; it does not establish native model loading."""

    torch_available: bool
    torch_npu_available: bool
    vllm_ascend_available: bool
    npu_available: bool
    missing: tuple

    @property
    def ready(self) -> bool:
        return not self.missing


def probe_host_readiness() -> HostReadiness:
    """Check the Ascend stack and its devices without importing eagerly."""
    missing = []
    torch_available = _importable("torch")
    torch_npu_available = _importable("torch_npu")
    vllm_ascend_available = _importable("vllm_ascend")
    npu_available = False
    if not torch_available:
        missing.append("torch")
    if not torch_npu_available:
        missing.append("torch_npu")
    if not vllm_ascend_available:
        missing.append("vllm_ascend")
    if torch_available and torch_npu_available:
        import torch

        try:
            import torch_npu  # noqa: F401  (registers the npu backend)

            npu_available = bool(torch.npu.is_available())
        except ImportError as exc:
            missing.append(f"torch_npu import: {exc}")
        else:
            if not npu_available:
                missing.append("npu")
    return HostReadiness(
        torch_available=torch_available,
        torch_npu_available=torch_npu_available,
        vllm_ascend_available=vllm_ascend_available,
        npu_available=npu_available,
        missing=tuple(missing),
    )


def probe_host_driver():
    """Refuse real-host acceptance: this fixture builds no native model."""
    return None, "NPU tensor fixture does not load a native vLLM-Ascend model"


def default_visibility_fence(stream: str = "npu:0"):
    """Return the Ascend visibility fence for one stream.

    The fence records an event on the stream after a borrowed tensor's
    transfer completes, so the consuming kernels' reads are ordered behind
    the transfer's writes before the release is consulted. It exists only
    on an Ascend host; importing it costs nothing.
    """

    # "npu:N" names the device the stream lives on; a bare stream name
    # means device 0. The fence synchronizes that device, not every NPU
    # the process can see.
    device = 0
    if ":" in stream:
        try:
            device = int(stream.rsplit(":", 1)[1])
        except ValueError:
            raise ValueError("malformed NPU stream device") from None
        if device < 0:
            raise ValueError("NPU stream device must be non-negative")

    def fence(name: str) -> None:
        # The host-side wait that orders the transfer's writes ahead of the
        # consuming kernels' reads on the stream. A per-stream ACL event
        # record is the qualified form; the device synchronize is the
        # honest floor an engine host can always deliver.
        try:
            import torch  # noqa: F401  (torch_npu registers onto torch)
            import torch_npu
        except ImportError as exc:
            raise PrerequisiteUnavailableError(
                f"torch and torch_npu are required to fence on an Ascend host: {exc}"
            ) from None
        torch_npu.npu.synchronize(device)

    return fence


def _importable(name: str) -> bool:
    import importlib.util

    try:
        return importlib.util.find_spec(name) is not None
    except (ImportError, ValueError):
        return False


class NPURuntimeDriver:
    """Reference handle backed by torch_npu tensors, not a native engine model.

    Every vendor import happens inside a method, so constructing or probing
    this module on a CPU-only host costs nothing and refuses cleanly. The
    fixture does not perform engine-specific postprocessing or inference.
    """

    def __init__(self, device: int | None = None):
        self._torch = None
        self._torch_npu = None
        self.last_model = None
        # The NPU this driver allocates on: the caller's assignment when
        # given, else the device visibility the process arrived with, else
        # 0. It is resolved once and honored everywhere.
        self._device = device

    @staticmethod
    def _resolve_device(explicit: int | None) -> int:
        """Resolve the NPU index: explicit, then visibility, then 0."""

        if explicit is not None:
            return explicit
        from gpustack_model_transfer.envs import ENVS

        visible = ENVS.ASCEND_RT_VISIBLE_DEVICES
        if visible:
            first = visible.split(",")[0].strip()
            if first.isdigit():
                return int(first)
        return 0

    def _require_torch(self):
        if self._torch is None:
            try:
                import torch
            except ImportError as exc:
                raise PrerequisiteUnavailableError(
                    f"torch is required on an Ascend host: {exc}"
                ) from None
            self._torch = torch
        return self._torch

    def _require_torch_npu(self):
        # torch must be imported before torch_npu: the npu backend
        # registers itself onto the running torch.
        self._require_torch()
        if self._torch_npu is None:
            try:
                import torch_npu
            except ImportError as exc:
                raise PrerequisiteUnavailableError(
                    f"torch_npu is required on an Ascend host: {exc}"
                ) from None
            self._torch_npu = torch_npu
        return self._torch_npu

    def build_model(self, plan, options):
        torch = self._require_torch()
        torch_npu = self._require_torch_npu()
        from gpustack_model_transfer.adapters.vllm_ascend.runtime import ModelHandle

        # NPU device and context initialization: the ACL context the
        # storages are allocated on is bound before the first tensor, on
        # the device this driver was assigned, not a hardcoded one.
        device = self._resolve_device(self._device)
        torch_npu.npu.set_device(device)
        model = ModelHandle(tie_word_embeddings=options.tie_word_embeddings)
        storages = {}
        for entry in plan.entries:
            if entry.role in ("tied-alias", "engine-metadata"):
                continue
            dtype = getattr(torch, _DTYPE_TO_TORCH[entry.tensor.dtype])
            shape = tuple(entry.tensor.shape)
            # A zero-element tensor keeps its dtype and shape with no data.
            buffer = torch.empty(shape, dtype=dtype, device=f"npu:{device}")
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
        torch = self._require_torch()
        buffer = model.parameters[entry.tensor.name]
        if not payload:
            # A zero-element tensor keeps its empty storage: frombuffer
            # refuses a zero-byte buffer, and there is nothing to copy.
            model.mutated = True
            return "owned"
        dtype = str(buffer.dtype).replace("torch.", "")
        raw = torch.frombuffer(bytearray(payload), dtype=_torch_dtype(torch, dtype))
        buffer.copy_(raw.reshape(buffer.shape))
        model.mutated = True
        return "owned"

    def copy_to_owned(self, model, entry, payload) -> None:
        self.store(model, entry, payload)

    def store_engine_metadata(self, model, entry, payload) -> None:
        from gpustack_model_transfer.adapters.vllm_ascend.runtime import decode_values

        values = decode_values(entry.tensor.dtype, bytes(payload))
        model.engine_metadata[entry.tensor.name] = values[0] if values else None

    def finalize(self, model) -> None:
        """Run fixture transformations and increment fixture bookkeeping.

        This is not vLLM-Ascend finalization or proof of KV readiness.
        """
        from gpustack_model_transfer.adapters.vllm_ascend.runtime import (
            FINALIZATION_HOOKS,
        )

        if model.finalization_count == 0:
            for hook in FINALIZATION_HOOKS:
                self._run_finalization_hook(model, hook)
        model.finalization_count += 1

    def _run_finalization_hook(self, model, hook: str) -> None:
        torch_npu = self._require_torch_npu()
        replacements = {}
        for name, buffer in model.parameters.items():
            if hook == "contiguous" and not buffer.is_contiguous():
                # contiguous() returns a new tensor; the replacement must
                # be written back or the parameter stays non-contiguous.
                if id(buffer) not in replacements:
                    replacements[id(buffer)] = buffer.contiguous()
                model.parameters[name] = replacements[id(buffer)]
            elif hook == "cast":
                # Storage dtypes were already the engine's compute dtypes;
                # the cast pass is a no-op unless the plan says otherwise.
                continue
            elif hook == "format":
                # Vendor storage formats are applied by vllm_ascend's own
                # weight post-processing; nothing to do beyond admission.
                continue
        _ = torch_npu

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
    "NPURuntimeDriver",
    "PrerequisiteUnavailableError",
    "default_visibility_fence",
    "probe_host_driver",
    "probe_host_readiness",
]
