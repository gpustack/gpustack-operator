"""Lazy SGLang plugin registration for --load-format model-transfer.

The same registration also makes the standard ModelExpress load formats
(`modelexpress` and the `mx` shorthand) work on SGLang: the formats are
added to SGLang's LoadFormat enum, and get_model_loader resolves them to
a loader that initializes the model and hands it to the pinned upstream
MxModelLoader strategy chain.
"""

import inspect
import logging
import os

from gpustack_model_transfer.envs import ENVS
from gpustack_model_transfer.modelexpress_route import LOAD_FORMAT_ALIASES

_LOG = logging.getLogger(__name__)

# The load formats this plugin answers to, and what resolves them. The
# ModelExpress entries derive from the route leaf's alias table — the
# single source of truth an import-safe module owns — so the CLI
# choices, the enum members, and the loader dispatch cannot drift from
# what the route recognizes.
_MODEL_TRANSFER_FORMAT = "model-transfer"
_ENUM_MEMBERS = {
    _MODEL_TRANSFER_FORMAT: "MODEL_TRANSFER",
    **{value: value.upper().replace("-", "_") for value in LOAD_FORMAT_ALIASES},
}
_MODELEXPRESS_FORMATS = tuple(
    value for value in _ENUM_MEMBERS if value != _MODEL_TRANSFER_FORMAT
)


def _choices_registration_log(exc: Exception):
    """Debug for a container without SGLang, warning for anything else.

    This plugin is the only thing that makes the ModelExpress formats
    exist on SGLang, so a registration that failed for any reason other
    than SGLang being absent must be visible instead of surfacing later
    as an unrelated `invalid choice` error.
    """
    if isinstance(exc, ModuleNotFoundError) and exc.name == "sglang":
        return _LOG.debug
    return _LOG.warning


def register() -> None:
    """Register the model-transfer load formats with SGLang's model_loader."""
    if ENVS.GPUSTACK_MODEL_TRANSFER_BACKEND == "hixl":
        timeout_str = ENVS.GPUSTACK_MODEL_TRANSFER_TIMEOUT_STR
        ENVS.setdefault("HCCL_EXEC_TIMEOUT", timeout_str)
        ENVS.setdefault("HCCL_CONNECT_TIMEOUT", timeout_str)

    try:
        import sglang.srt.arg_groups.choices as choices_mod

        add_choices = getattr(choices_mod, "add_load_format_choices", None)
        if callable(add_choices):
            values = list(_ENUM_MEMBERS)
            try:
                add_choices(values)
            except TypeError:
                registered = 0
                for value in values:
                    try:
                        add_choices(value)
                        registered += 1
                    except Exception as exc:  # noqa: PERF203 — one refused value must not abort the rest
                        _LOG.debug(
                            "SGLang choices registration skipped %r (%s)",
                            value,
                            type(exc).__name__,
                        )
                if registered < len(values):
                    # The plugin is the only thing that makes these
                    # formats exist on SGLang: a partial registration
                    # turns `--load-format modelexpress` into a later
                    # unrelated `invalid choice` unless it is visible.
                    _LOG.warning(
                        "SGLang choices registration incomplete: "
                        "%d of %d load formats registered",
                        registered,
                        len(values),
                    )
            except Exception as exc:
                _choices_registration_log(exc)(
                    "SGLang choices registration unavailable (%s)",
                    type(exc).__name__,
                )
    except Exception as exc:
        _choices_registration_log(exc)(
            "SGLang choices registration unavailable (%s)", type(exc).__name__
        )

    try:
        from enum import Enum

        import sglang.srt.configs.load_config as load_config_mod

        load_format_cls = getattr(load_config_mod, "LoadFormat", None)
        if (
            load_format_cls is not None
            and isinstance(load_format_cls, type)
            and issubclass(load_format_cls, Enum)
        ):
            for value, member_name in _ENUM_MEMBERS.items():
                _extend_load_format_enum(load_format_cls, member_name, value)
    except Exception as exc:
        log = (
            _LOG.debug
            if isinstance(exc, ModuleNotFoundError) and exc.name == "sglang"
            else _LOG.error
        )
        log("SGLang enum registration unavailable (%s)", type(exc).__name__)

    try:
        import sglang.srt.model_loader as ml_pkg
        import sglang.srt.model_loader.loader as loader_mod
        from gpustack_model_transfer.adapters.sglang.native import (
            NativeSGLangModelTransferLoader,
        )

        orig_get_model_loader = getattr(loader_mod, "get_model_loader", None)
        if not callable(orig_get_model_loader):
            raise TypeError("SGLang get_model_loader is not callable")
        if not getattr(
            orig_get_model_loader, "_gpustack_model_transfer_wrapped", False
        ):

            def _patched_get_model_loader(load_config, *args, **kwargs):
                fmt = getattr(load_config, "load_format", None)
                fmt_val = getattr(fmt, "value", fmt)
                if isinstance(fmt_val, str):
                    fmt_val = fmt_val.strip()
                if fmt_val == _MODEL_TRANSFER_FORMAT or fmt == _MODEL_TRANSFER_FORMAT:
                    return NativeSGLangModelTransferLoader(load_config)
                if isinstance(fmt_val, str) and fmt_val.lower() in (
                    _MODELEXPRESS_FORMATS
                ):
                    return ModelExpressSGLangLoader(load_config)
                return orig_get_model_loader(load_config, *args, **kwargs)

            _patched_get_model_loader._gpustack_model_transfer_wrapped = True
            loader_mod.get_model_loader = _patched_get_model_loader
            if hasattr(ml_pkg, "get_model_loader"):
                ml_pkg.get_model_loader = _patched_get_model_loader
            import sys

            lmu_mod = sys.modules.get(
                "sglang.srt.model_executor.model_runner_components.load_model_utils"
            )
            if lmu_mod is not None and hasattr(lmu_mod, "get_model_loader"):
                lmu_mod.get_model_loader = _patched_get_model_loader
    except Exception as exc:
        log = (
            _LOG.debug
            if isinstance(exc, ModuleNotFoundError) and exc.name == "sglang"
            else _LOG.error
        )
        log("SGLang loader registration unavailable (%s)", type(exc).__name__)


def _extend_load_format_enum(load_format_cls, member_name: str, value: str) -> None:
    """Add one load format to SGLang's LoadFormat enum, idempotently.

    aenum's extend_enum is used when installed; the fallback rebuilds
    the member by hand so pickling and value lookup keep working.
    """
    if member_name in load_format_cls.__members__ or any(
        m.value == value for m in load_format_cls
    ):
        return
    try:
        from aenum import extend_enum  # type: ignore[import-not-found]

        extend_enum(load_format_cls, member_name, value)
        return
    except Exception:
        pass
    if issubclass(load_format_cls, str):
        member = str.__new__(load_format_cls, value)
    else:
        member = object.__new__(load_format_cls)
    member._name_ = member_name
    member._value_ = value
    load_format_cls._member_map_[member_name] = member
    load_format_cls._member_names_.append(member_name)
    load_format_cls._value2member_map_[value] = member
    type.__setattr__(load_format_cls, member_name, member)


class ModelExpressSGLangLoader:
    """The SGLang loader for --load-format modelexpress (and mx).

    SGLang has no upstream plugin hook for the format, so this loader
    bridges the two: it initializes the model on the target device the
    way SGLang's own loader does, then hands it to the pinned upstream
    MxModelLoader, whose strategy chain runs Peer D2D, the GPUStack
    source delivery, and the local strategies per the delivery policy.
    """

    def __init__(self, load_config):
        self.load_config = load_config
        from gpustack_model_transfer.adapters.modelexpress.launch import (
            apply_launch_env,
        )

        apply_launch_env(os.environ)
        # The shared route carries the delivery-policy failure handling:
        # an unresolvable arm refuses under Require and degrades to a
        # logged warning under Prefer, instead of an opaque import error
        # escaping loader construction.
        from gpustack_model_transfer.adapters.modelexpress.registration import (
            register_strategy_route,
        )

        register_strategy_route()

    def load_model(self, *, model_config, device_config):
        """Initialize the model the way SGLang's own load path does.

        The third argument of SGLang's _initialize_model is the
        quantization config, not the device config, and the empty model
        must be built under the model's configured dtype and on the
        target device — the same sequence the pinned upstream adapter
        runs on a retry reinitialization.
        """
        import torch

        try:
            from sglang.srt.model_loader.loader import (
                _get_quantization_config,
                _initialize_model,
            )
            from sglang.srt.model_loader.utils import set_default_torch_dtype
        except ImportError as exc:
            raise ImportError(
                "the SGLang model-loader internals the ModelExpress loader "
                f"needs are missing ({exc}); the installed SGLang version "
                "is not supported"
            ) from exc

        # The internals are private: validate their shapes here, at the
        # boundary, so a drifted SGLang version fails with this message
        # instead of an opaque TypeError deep in model initialization.
        for internal, arity in (
            (_get_quantization_config, 2),
            (_initialize_model, 3),
        ):
            try:
                signature = inspect.signature(internal)
            except (TypeError, ValueError) as exc:
                raise ImportError(
                    "the SGLang model-loader internals the ModelExpress "
                    f"loader needs cannot be inspected ({exc}); the "
                    "installed SGLang version is not supported"
                ) from exc
            positional = sum(
                1
                for parameter in signature.parameters.values()
                if parameter.kind
                in (
                    inspect.Parameter.POSITIONAL_ONLY,
                    inspect.Parameter.POSITIONAL_OR_KEYWORD,
                    inspect.Parameter.VAR_POSITIONAL,
                )
            )
            if positional < arity:
                raise ImportError(
                    "the SGLang model-loader internals the ModelExpress "
                    f"loader needs changed shape ({getattr(internal, '__name__', internal)} "
                    "no longer takes the expected arguments); the installed "
                    "SGLang version is not supported"
                )

        quant_config = _get_quantization_config(model_config, self.load_config)
        target_device = torch.device(device_config.device)
        with set_default_torch_dtype(model_config.dtype), target_device:
            model = _initialize_model(
                model_config,
                self.load_config,
                quant_config,
            )
        from modelexpress.engines.sglang.loader import MxModelLoader

        return MxModelLoader(self.load_config).load_model(
            model=model,
            model_config=model_config,
            device_config=device_config,
        )


__all__ = [
    "ModelExpressSGLangLoader",
    "register",
]
