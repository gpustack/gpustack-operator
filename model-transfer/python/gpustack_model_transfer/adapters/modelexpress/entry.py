"""The strategy entry surface, kept for the plugin-file route.

The strategy now lives in plugin.py and follows the pinned upstream
LoadStrategy ABI. This module re-exports it for callers that import the
entry by its historical path, including the MX_PLUGIN_DIR plugin file
route, whose entry must return a concrete strategy class. Both surfaces
resolve through the live strategy module at call time, so every caller
in one process holds the same object registration holds.
"""

from gpustack_model_transfer.adapters.modelexpress.constants import (
    LOAD_FORMAT,
    LOAD_FORMAT_ALIASES,
    PLUGIN_NAME,
)

_STRATEGY_MODULE = "gpustack_model_transfer.adapters.modelexpress.plugin"


def _live_strategy_module():
    import importlib
    import sys

    return sys.modules.get(_STRATEGY_MODULE) or importlib.import_module(
        _STRATEGY_MODULE
    )


def __getattr__(name):
    if name in ("STRATEGY_NAME", "GpustackLoadStrategy", "register_plugin"):
        return getattr(_live_strategy_module(), name)
    raise AttributeError(f"module {__name__!r} has no attribute {name!r}")


def __dir__():
    return sorted(
        set(globals()) | {"STRATEGY_NAME", "GpustackLoadStrategy", "register_plugin"}
    )


__all__ = [  # noqa: F822 — the strategy names resolve through __getattr__
    "LOAD_FORMAT",
    "LOAD_FORMAT_ALIASES",
    "PLUGIN_NAME",
    "STRATEGY_NAME",
    "GpustackLoadStrategy",
    "register_plugin",
]
