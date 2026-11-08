"""Optional import hook for engines without entry-point plugin discovery.

The bootstrap runs at interpreter start through the packaged .pth. It
parses the command line and the environment through the import leaf
(gpustack_model_transfer.modelexpress_route) — never the adapter
package, ModelExpress, or torch — so a process that did not ask for the
route pays nothing. When the route is requested, it applies the launch
env defaults, arms the lazy import hook so SGLang's load format surfaces
register themselves when their modules are first imported, and arms the
strategy route: the runtime patch and the plugin registration. The vLLM
load-format registry is not armed here, because registering it would
import vllm.model_executor.model_loader — dragging vLLM and torch into
every requested process, including an SGLang engine — and every vLLM
process arms it anyway through vLLM's general_plugins entry, which
fires inside vLLM processes only.
"""

import importlib.abc
import importlib.machinery
import logging
import os
import sys

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
    """Allow the model-transfer load formats in SGLang ServerArgs CLI choices."""
    from gpustack_model_transfer.adapters.modelexpress.constants import (
        LOAD_FORMAT_ALIASES,
    )

    model_transfer_choices = ("model-transfer", *LOAD_FORMAT_ALIASES)
    add_choices = getattr(mod, "add_load_format_choices", None)
    if callable(add_choices):
        values = list(model_transfer_choices)
        try:
            add_choices(values)
        except TypeError:
            for value in values:
                try:
                    add_choices(value)
                except Exception as exc:  # noqa: PERF203 — one refused value must not abort the rest
                    _LOG.debug(
                        "SGLang choices registration skipped %r (%s)",
                        value,
                        type(exc).__name__,
                    )
        except Exception as exc:
            _LOG.debug(
                "SGLang choices registration unavailable (%s)", type(exc).__name__
            )
    choices_list = getattr(mod, "LOAD_FORMAT_CHOICES", None)
    if isinstance(choices_list, list):
        for value in model_transfer_choices:
            if value not in choices_list:
                choices_list.append(value)

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
        wanted = list(model_transfer_choices)
        for action in getattr(parser, "_actions", ()):
            if "--load-format" in getattr(action, "option_strings", ()):
                choices = getattr(action, "choices", None)
                if isinstance(choices, list):
                    for value in wanted:
                        if value not in choices:
                            choices.append(value)
                elif isinstance(choices, tuple):
                    action.choices = choices + tuple(
                        value for value in wanted if value not in choices
                    )
                elif isinstance(choices, set):
                    choices.update(wanted)
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


def _install_finder() -> None:
    global _INSTALLED
    if _INSTALLED:
        return
    _INSTALLED = True
    sys.meta_path.insert(0, _PostImportFinder())


def _register_engines() -> None:
    """Arm the strategy route, best effort.

    Only called when this process's own command line or environment
    asked for the route. The vLLM load-format registry step is left to
    the vLLM general_plugins entry, which fires inside vLLM processes
    only: registering it here would import vllm.model_executor.model_loader
    and drag vLLM and torch into an SGLang engine process at interpreter
    start, a heavy import that buys nothing there.

    Under Prefer a failed arm is a warning, since an advisory arm must
    never break interpreter start. Under Require the failure is
    re-raised: CPython's site processing catches it, prints the error
    to stderr, and continues, so interpreter start itself survives —
    the enforcement boundary is the engine entry, not the interpreter.
    Every engine weight load re-arms the route at its own entry (the
    vLLM general-plugins hook, the engine loader constructions), and
    each of those re-raises under Require, so no engine weight load
    starts against an unenforced route while non-engine processes that
    merely inherit the route environment keep running.
    """
    try:
        from gpustack_model_transfer.adapters.modelexpress.registration import (
            register_strategy_route,
        )

        register_strategy_route()
    except Exception as exc:
        try:
            from gpustack_model_transfer.adapters.modelexpress.policy import (
                delivery_policy,
            )

            strict = not delivery_policy().allow_fallback
        except Exception:
            # The policy module lives in the same package whose import may
            # be the very failure being handled: an unreadable policy is
            # advisory, like the Prefer default, and must not turn a
            # swallowed arm failure into an interpreter-start crash.
            strict = False
        if strict:
            _LOG.error(
                "ModelExpress registration failed under the Require policy (%s); "
                "engine weight loads will refuse to run unenforced",
                type(exc).__name__,
                exc_info=True,
            )
            raise
        _LOG.warning(
            "ModelExpress registration failed at bootstrap (%s)",
            type(exc).__name__,
            exc_info=True,
        )


def bootstrap(argv=None, environ=None) -> bool:
    """Recognize the ModelExpress request and arm the engine integrations.

    argv defaults to sys.argv and environ to os.environ, so a caller can
    ask about a hypothetical command line. The recognition imports only
    the route leaf: a process that did not request the ModelExpress
    format must not import the adapter package, ModelExpress, or torch.
    The recognition is hypothetical, the arming is not: applying the
    launch env, arming the import finder, and registering the strategy
    route touch this process, so they run only against the real command
    line and environment — an injected argv or environ gets the answer
    and nothing else. Returns whether the process requested the route.
    """
    from gpustack_model_transfer import modelexpress_route as route

    real = argv is None and environ is None
    source = environ if environ is not None else os.environ
    args = sys.argv if argv is None else argv
    requested = route.argv_requests_gpustack_modelexpress(args) or (
        route.env_requests_gpustack_modelexpress(source)
    )
    if not real:
        return requested
    if requested:
        route.apply_launch_env(source)
        _install_finder()
        _register_engines()
    elif str(source.get("GPUSTACK_MODEL_TRANSFER_LOAD_FORMAT", "")) == "model-transfer":
        _install_finder()
    return requested


def install() -> None:
    """The .pth entry point: run the bootstrap against the real process."""
    bootstrap()


__all__ = [
    "bootstrap",
    "install",
]
