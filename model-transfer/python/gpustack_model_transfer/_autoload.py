"""Optional import hook for engines without entry-point plugin discovery (e.g. SGLang)."""

import importlib.abc
import importlib.machinery
import logging
import sys

from gpustack_model_transfer.envs import ENVS

_INSTALLED = False
_LOG = logging.getLogger(__name__)

_SGLANG_TARGETS = frozenset(
    {
        "sglang.srt.arg_groups.choices",
        "sglang.srt.configs.load_config",
        "sglang.srt.model_loader.loader",
        "sglang.srt.server_args",
    }
)


def _patch_server_args(mod) -> None:
    """Allow --load-format model-transfer in SGLang ServerArgs CLI choices if present."""
    add_choices = getattr(mod, "add_load_format_choices", None)
    if callable(add_choices):
        try:
            add_choices(["model-transfer"])
        except TypeError:
            try:
                add_choices("model-transfer")
            except Exception as exc:
                _LOG.debug(
                    "SGLang choices registration unavailable (%s)", type(exc).__name__
                )
        except Exception as exc:
            _LOG.debug(
                "SGLang choices registration unavailable (%s)", type(exc).__name__
            )
    choices_list = getattr(mod, "LOAD_FORMAT_CHOICES", None)
    if isinstance(choices_list, list) and "model-transfer" not in choices_list:
        choices_list.append("model-transfer")

    server_args_cls = getattr(mod, "ServerArgs", None)
    if server_args_cls is None:
        return
    orig_add_cli_args = getattr(server_args_cls, "add_cli_args", None)
    if not callable(orig_add_cli_args) or getattr(
        orig_add_cli_args, "_gpustack_model_transfer_wrapped", False
    ):
        return

    def _wrapped_add_cli_args(parser, *args, **kwargs):
        res = orig_add_cli_args(parser, *args, **kwargs)
        for action in getattr(parser, "_actions", ()):
            if "--load-format" in getattr(action, "option_strings", ()):
                choices = getattr(action, "choices", None)
                if isinstance(choices, list) and "model-transfer" not in choices:
                    choices.append("model-transfer")
                elif isinstance(choices, tuple) and "model-transfer" not in choices:
                    action.choices = (*choices, "model-transfer")
                elif isinstance(choices, set) and "model-transfer" not in choices:
                    choices.add("model-transfer")
        return res

    _wrapped_add_cli_args._gpustack_model_transfer_wrapped = True
    server_args_cls.add_cli_args = staticmethod(_wrapped_add_cli_args)


class _PostImportFinder(importlib.abc.MetaPathFinder):
    def __init__(self):
        self._active = set()

    def find_spec(self, fullname, path, target=None):
        if fullname not in _SGLANG_TARGETS or fullname in self._active:
            return None
        self._active.add(fullname)
        try:
            spec = importlib.machinery.PathFinder.find_spec(fullname, path, target)
        finally:
            self._active.discard(fullname)
        if spec is None or spec.loader is None:
            return None
        orig_exec = spec.loader.exec_module

        def _exec_module(module):
            orig_exec(module)
            if fullname in ("sglang.srt.server_args", "sglang.srt.arg_groups.choices"):
                _patch_server_args(module)
            else:
                from gpustack_model_transfer.adapters.sglang.plugin import register

                register()

        spec.loader.exec_module = _exec_module
        return spec


def install() -> None:
    global _INSTALLED
    if _INSTALLED:
        return
    if ENVS.get("GPUSTACK_MODEL_TRANSFER_LOAD_FORMAT") != "model-transfer":
        return
    _INSTALLED = True
    sys.meta_path.insert(0, _PostImportFinder())
