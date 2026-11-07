"""Native loader for SGLang's runtime model_loader, delegating weight mapping to the model."""

import re
from contextlib import suppress

import torch

from gpustack_model_transfer.adapters import engine_reader
from gpustack_model_transfer.adapters.descriptor_pages import (
    broadcast_descriptors,
    checked_descriptors,
)
from gpustack_model_transfer.core.budgets import tp_load_mode

_DTYPES = {
    "F64": "float64",
    "F32": "float32",
    "F16": "float16",
    "BF16": "bfloat16",
    "F8_E4M3": "float8_e4m3fn",
    "F8_E5M2": "float8_e5m2",
    "F8_E8M0": "float8_e8m0fnu",
    "F8_E4M3FNUZ": "float8_e4m3fnuz",
    "F8_E5M2FNUZ": "float8_e5m2fnuz",
    "I64": "int64",
    "I32": "int32",
    "I16": "int16",
    "I8": "int8",
    "U64": "uint64",
    "U32": "uint32",
    "U16": "uint16",
    "U8": "uint8",
    "BOOL": "bool",
    "C64": "complex64",
}
# In PyTorch on AMD ROCm (HIP), GPU tensors report `device.type == "cuda"` (with
# `torch.version.hip` set), while custom/out-of-tree HIP backends may report
# `"hip"`. Ascend NPU reports `"npu"` (or `"privateuseone"`). CPU parameters are
# also allowed, both for pure-CPU execution and for hybrid models that place a
# subset of weights on CPU (for example DeepSeek Engram tables).
_ACCELERATOR_DEVICE_TYPES = frozenset({"cuda", "hip", "npu", "privateuseone"})
_SUPPORTED_DEVICE_TYPES = _ACCELERATOR_DEVICE_TYPES | {"cpu"}
_process_unusable = False


class UnsupportedEngineLayout(ValueError):
    """The native SGLang loader cannot safely load this checkpoint."""


class ProcessLoadUnusable(RuntimeError):
    """A prior load failed and this process must be replaced."""


def _check_process() -> None:
    if _process_unusable:
        raise ProcessLoadUnusable(
            "a prior model-transfer load failed; restart the process"
        )


def _torch_dtype(descriptor_dtype: str):
    if not isinstance(descriptor_dtype, str) or not descriptor_dtype.strip():
        raise UnsupportedEngineLayout(f"unsupported dtype: {descriptor_dtype}")
    attr = _DTYPES.get(descriptor_dtype)
    if attr is not None and hasattr(torch, attr):
        return getattr(torch, attr)
    lower = descriptor_dtype.lower()
    candidate = getattr(torch, lower, None)
    if isinstance(candidate, torch.dtype):
        return candidate
    with suppress(Exception):
        from safetensors.torch import _getdtype

        return _getdtype(descriptor_dtype)
    raise UnsupportedEngineLayout(f"unsupported dtype: {descriptor_dtype}")


def _active_descriptors(model, descriptors):
    """Filter out checkpoint tensors this process never instantiates.

    The model's native mapper drops unused prefixes, and a pipeline stage
    only instantiates its own subset of transformer layers, so checkpoint
    tensors for other stages' layers are skipped instead of refused.
    """
    mapper = getattr(model, "hf_to_vllm_mapper", None)
    native_map_name = getattr(mapper, "_map_name", None)
    named_parameters = getattr(model, "named_parameters", None)
    present_layers = {
        m.group(1)
        for name, _ in (
            named_parameters(remove_duplicate=False)
            if callable(named_parameters)
            else ()
        )
        if (m := re.search(r"(?:^|\.)(?:layers|h|blocks)\.(\d+)\.", name))
    }
    for descriptor in descriptors:
        if callable(native_map_name) and native_map_name(descriptor.name) is None:
            continue
        match = re.search(r"(?:^|\.)(?:layers|h|blocks)\.(\d+)\.", descriptor.name)
        if present_layers and match and match.group(1) not in present_layers:
            continue
        yield descriptor


def _confirm(reported, delivered) -> None:
    """Verify that the engine confirmed loading weights when it returns a report."""
    if not delivered:
        return
    if reported is not None and next(iter(reported), None) is None:
        raise RuntimeError("engine did not confirm any loaded weights")


def _target_device(parameters):
    """Select the target device that receives each transferred tensor.

    Accelerator parameters (CUDA, ROCm HIP, Ascend NPU) within a worker must
    reside on a single accelerator device. Models may also place a subset of
    parameters on CPU (such as CPU-offloaded Engram or embedding tables) or
    run entirely on CPU; when an accelerator device is present it is used for
    the temporary transfer buffer, and otherwise CPU is used.
    """
    accelerator_devices = {}
    cpu_device = None
    count = 0
    for parameter in parameters:
        count += 1
        if getattr(parameter, "is_meta", False) or not parameter.is_contiguous():
            raise UnsupportedEngineLayout("unsupported target layout")
        device = parameter.device
        dtype_name = getattr(device, "type", None)
        if dtype_name not in _SUPPORTED_DEVICE_TYPES:
            raise UnsupportedEngineLayout(f"unsupported target device: {dtype_name}")
        if dtype_name == "cpu":
            if cpu_device is None:
                cpu_device = device
        else:
            key = (dtype_name, getattr(device, "index", None))
            accelerator_devices.setdefault(key, device)
    if count == 0:
        raise UnsupportedEngineLayout("empty or unknown parameter layout")
    if len(accelerator_devices) > 1:
        raise UnsupportedEngineLayout("the model spans several accelerator devices")
    if accelerator_devices:
        return next(iter(accelerator_devices.values()))
    return cpu_device


def _plan(model, model_config, descriptors):
    """Validate transport and target invariants before any transfer starts."""
    parameters = (
        parameter for _, parameter in model.named_parameters(remove_duplicate=False)
    )
    if not descriptors:
        raise UnsupportedEngineLayout("empty or unknown parameter layout")
    for desc in descriptors:
        _torch_dtype(desc.dtype)
    return _target_device(parameters)


def _tp_coordination():
    """Select the TP collective, its global source rank, and the group size.

    The size is the reader's TP share input: under the "all" TP load mode
    the ranks split one window budget, so they read at one TP group's
    even share each. A lone rank (or no distributed at all) is size 1.
    """
    distributed = getattr(torch, "distributed", None)
    if (
        distributed is None
        or not distributed.is_available()
        or not distributed.is_initialized()
        or distributed.get_world_size() <= 1
    ):
        return False, True, None, 0, 1
    for mod_name in (
        "sglang.srt.distributed",
        "sglang.srt.distributed.parallel_state",
    ):
        with suppress(Exception):
            mod = __import__(mod_name, fromlist=["get_tp_group"])
            get_tp_group = getattr(mod, "get_tp_group", None)
            if callable(get_tp_group):
                tp_group = get_tp_group()
                if tp_group.world_size <= 1:
                    # A lone TP rank transfers alone: under PP > 1 the WORLD
                    # group spans several pipeline stages and would load
                    # every stage's layers into every rank.
                    return False, True, None, 0, 1
                if tp_group.device_group is not None:
                    return (
                        True,
                        tp_group.rank_in_group == 0,
                        tp_group.device_group,
                        tp_group.ranks[0],
                        tp_group.world_size,
                    )
    return (
        True,
        distributed.get_rank() == 0,
        distributed.group.WORLD,
        0,
        distributed.get_world_size(),
    )


def _synchronize_device(device) -> None:
    dtype_name = getattr(device, "type", None)
    if dtype_name == "cpu":
        return
    if (
        dtype_name in ("cuda", "hip")
        and hasattr(torch, "cuda")
        and hasattr(torch.cuda, "synchronize")
    ):
        torch.cuda.synchronize(device)
        return
    if (
        dtype_name in ("npu", "privateuseone")
        and hasattr(torch, "npu")
        and hasattr(torch.npu, "synchronize")
    ):
        torch.npu.synchronize(device)
        return
    raise UnsupportedEngineLayout("device has no supported completion fence")


class NativeSGLangModelTransferLoader:
    """SGLang runtime loader that streams verified remote tensors into model.load_weights."""

    def __init__(self, load_config):
        self.load_config = load_config
        self._provider = None
        self._provider_tp_size = None

    def _reader(self, tp_size=1):
        if self._provider is None:
            self._provider = engine_reader.create_reader_provider(
                self.load_config, tp_size=tp_size
            )
            self._provider_tp_size = tp_size
        elif self._provider_tp_size != tp_size:
            raise UnsupportedEngineLayout(
                "tensor-parallel size changed after reader creation"
            )
        return self._provider

    def download_model(self, model_config) -> None:
        _check_process()
        distributed, coordinator, group, src, tp_world_size = _tp_coordination()
        # The probes stay on the coordinator even when every rank will
        # read: one probe answers the layout question for all of them.
        tp_size = tp_world_size if distributed and tp_load_mode() == "all" else 1
        if not distributed:
            self._reader(tp_size).probe(model_config)
            return
        result = [None, None]
        if coordinator:
            try:
                result = [
                    True,
                    checked_descriptors(self._reader(tp_size).probe(model_config)),
                ]
            except BaseException as exc:
                torch.distributed.broadcast_object_list(
                    [False, str(exc)], src=src, group=group
                )
                raise
        broadcast_descriptors(
            result[1],
            lambda items: torch.distributed.broadcast_object_list(
                items, src=src, group=group
            ),
            coordinator=coordinator,
        )

    def load_weights(self, model, model_config) -> None:
        global _process_unusable
        _check_process()
        session = None
        try:
            distributed, coordinator, group, src, tp_world_size = _tp_coordination()
            all_ranks = distributed and tp_load_mode() == "all"
            tp_size = tp_world_size if all_ranks else 1
            if not distributed or all_ranks:
                reader = self._reader(tp_size)
                descriptors = checked_descriptors(reader.probe(model_config))
                device = _plan(model, model_config, descriptors)
                session = reader.start(model_config)
            else:
                result = [None, None]
                if coordinator:
                    try:
                        descriptors = checked_descriptors(
                            self._reader(tp_size).probe(model_config)
                        )
                        device = _plan(model, model_config, descriptors)
                        session = self._reader(tp_size).start(model_config)
                        result = [True, descriptors]
                    except BaseException as exc:
                        torch.distributed.broadcast_object_list(
                            [False, str(exc)], src=src, group=group
                        )
                        raise
                descriptors = broadcast_descriptors(
                    result[1],
                    lambda items: torch.distributed.broadcast_object_list(
                        items, src=src, group=group
                    ),
                    coordinator=coordinator,
                )
                if not coordinator:
                    device = _plan(model, model_config, descriptors)
            active_descriptors = _active_descriptors(model, descriptors)
            consumed = 0
            exhausted = False

            def weights():
                nonlocal consumed, exhausted
                for descriptor in active_descriptors:
                    target = torch.empty(
                        descriptor.shape,
                        dtype=_torch_dtype(descriptor.dtype),
                        device=device,
                    )
                    if all_ranks or coordinator:
                        session.fill(descriptor, target)
                    if distributed and not all_ranks:
                        broadcast_target = (
                            target.view(torch.uint8)
                            if getattr(torch, "uint8", None) is not None
                            and callable(getattr(target, "view", None))
                            else target
                        )
                        torch.distributed.broadcast(
                            broadcast_target, src=src, group=group
                        )
                        _synchronize_device(device)
                    elif all_ranks:
                        _synchronize_device(device)
                    consumed += 1
                    yield descriptor.name, target
                exhausted = True

            # Delegate parameter name mapping, QKV/MLP fusion, and TP sharding
            # directly to SGLang's own model.load_weights implementation.
            loaded = model.load_weights(weights())
            if not exhausted:
                raise RuntimeError("engine did not consume the complete checkpoint")
            _confirm(loaded, consumed)
            if session is not None:
                session.close()
                session = None
        except BaseException:
            # Even preflight refusal is fatal: peers may already be transferring.
            _process_unusable = True
            if session is not None:
                with suppress(BaseException):
                    session.abort()
            raise

    def load_model(self, *, model_config, device_config):
        global _process_unusable
        _check_process()
        try:
            from sglang.srt.model_loader.loader import DefaultModelLoader

            default_loader = DefaultModelLoader(self.load_config)
            orig_postprocess = getattr(default_loader, "postprocess_weights", None)

            def _load_weights_and_postprocess(model, _weights_iter, target_device):
                self.load_weights(model, model_config)
                if callable(orig_postprocess):
                    orig_postprocess(model, target_device)

            default_loader.load_weights = self.load_weights
            default_loader.download_model = self.download_model
            if hasattr(default_loader, "load_weights_and_postprocess"):
                default_loader._get_all_weights = lambda *_args, **_kwargs: iter(())
                default_loader.load_weights_and_postprocess = (
                    _load_weights_and_postprocess
                )
            return default_loader.load_model(
                model_config=model_config,
                device_config=device_config,
            )
        except BaseException:
            _process_unusable = True
            raise


NativeModelTransferLoader = NativeSGLangModelTransferLoader

__all__ = [
    "NativeModelTransferLoader",
    "NativeSGLangModelTransferLoader",
    "ProcessLoadUnusable",
    "UnsupportedEngineLayout",
]
