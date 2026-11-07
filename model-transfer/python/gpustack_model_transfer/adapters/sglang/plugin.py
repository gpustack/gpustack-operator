"""Lazy SGLang plugin registration for --load-format model-transfer."""

import logging

from gpustack_model_transfer.envs import ENVS

_LOG = logging.getLogger(__name__)


def register() -> None:
    """Register the model-transfer load format with SGLang's model_loader."""
    if ENVS.GPUSTACK_MODEL_TRANSFER_BACKEND == "hixl":
        timeout_str = ENVS.GPUSTACK_MODEL_TRANSFER_TIMEOUT_STR
        ENVS.setdefault("HCCL_EXEC_TIMEOUT", timeout_str)
        ENVS.setdefault("HCCL_CONNECT_TIMEOUT", timeout_str)

    try:
        import sglang.srt.arg_groups.choices as choices_mod

        add_choices = getattr(choices_mod, "add_load_format_choices", None)
        if callable(add_choices):
            try:
                add_choices(["model-transfer"])
            except TypeError:
                add_choices("model-transfer")
    except Exception as exc:
        _LOG.debug("SGLang choices registration unavailable (%s)", type(exc).__name__)

    try:
        from enum import Enum

        import sglang.srt.configs.load_config as load_config_mod

        load_format_cls = getattr(load_config_mod, "LoadFormat", None)
        if (
            load_format_cls is not None
            and isinstance(load_format_cls, type)
            and issubclass(load_format_cls, Enum)
            and "MODEL_TRANSFER" not in load_format_cls.__members__
            and not any(m.value == "model-transfer" for m in load_format_cls)
        ):
            try:
                from aenum import extend_enum  # type: ignore[import-not-found]

                extend_enum(load_format_cls, "MODEL_TRANSFER", "model-transfer")
            except Exception:
                if issubclass(load_format_cls, str):
                    member = str.__new__(load_format_cls, "model-transfer")
                else:
                    member = object.__new__(load_format_cls)
                member._name_ = "MODEL_TRANSFER"
                member._value_ = "model-transfer"
                load_format_cls._member_map_["MODEL_TRANSFER"] = member
                load_format_cls._member_names_.append("MODEL_TRANSFER")
                load_format_cls._value2member_map_["model-transfer"] = member
                type.__setattr__(load_format_cls, "MODEL_TRANSFER", member)
    except Exception as exc:
        _LOG.debug("SGLang enum registration unavailable (%s)", type(exc).__name__)

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

            def _wrapped_get_model_loader(load_config, *args, **kwargs):
                fmt = getattr(load_config, "load_format", None)
                fmt_val = getattr(fmt, "value", fmt)
                if fmt_val == "model-transfer" or fmt == "model-transfer":
                    return NativeSGLangModelTransferLoader(load_config)
                return orig_get_model_loader(load_config, *args, **kwargs)

            _wrapped_get_model_loader._gpustack_model_transfer_wrapped = True
            loader_mod.get_model_loader = _wrapped_get_model_loader
            if hasattr(ml_pkg, "get_model_loader"):
                ml_pkg.get_model_loader = _wrapped_get_model_loader
            import sys

            lmu_mod = sys.modules.get(
                "sglang.srt.model_executor.model_runner_components.load_model_utils"
            )
            if lmu_mod is not None and hasattr(lmu_mod, "get_model_loader"):
                lmu_mod.get_model_loader = _wrapped_get_model_loader
    except Exception as exc:
        log = (
            _LOG.debug
            if isinstance(exc, ModuleNotFoundError) and exc.name == "sglang"
            else _LOG.error
        )
        log("SGLang loader registration unavailable (%s)", type(exc).__name__)
