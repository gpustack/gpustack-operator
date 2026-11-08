"""The vLLM loader entry: the standard formats with the runtime patch.

Registration binds `--load-format modelexpress` (and `mx`) to the pinned
upstream MxModelLoader — a BaseModelLoader subclass, as vLLM's loader
registry requires — after installing the delivery-policy runtime patch
and the GPUStack strategy. This module keeps the manual wrapper for
callers that construct the loader themselves: it installs the same
pieces and then loads through the upstream base, whose strategy chain
runs Peer D2D first, the GPUStack source delivery second, and the local
strategies behind them per the delivery policy. Subclassing the pinned
upstream base keeps this wrapper a BaseModelLoader too, so a caller may
isinstance-check it or hand it to vLLM's loader registry like the
upstream class. Importing this module imports the pinned upstream
package, like every caller that would construct a vLLM loader for it.
"""

import importlib
import os

from gpustack_model_transfer.adapters.modelexpress.constants import (
    LOAD_FORMAT,
    LOAD_FORMAT_ALIASES,
)
from gpustack_model_transfer.modelexpress_route import apply_launch_env

_MX_VLLM_LOADER_MODULE = "modelexpress.engines.vllm.loader"


def _mx_model_loader():
    """The pinned upstream MxModelLoader, imported on first use."""
    module = importlib.import_module(_MX_VLLM_LOADER_MODULE)
    return module.MxModelLoader


class GpustackMxModelLoader(_mx_model_loader()):
    """The manual load entry that keeps ModelExpress's real strategy chain.

    The upstream loader owns the load; this subclass only makes sure the
    GPUStack pieces are in place before it runs: the launch env defaults,
    the runtime patch, and the strategy registration. It is a hook for
    manual construction — the registry-bound loader is the upstream
    MxModelLoader itself, registered by this package's general-plugins
    entry.
    """

    def __init__(self, load_config):
        apply_launch_env(os.environ)
        from gpustack_model_transfer.adapters.modelexpress.registration import (
            register_strategy_with_modelexpress,
        )
        from gpustack_model_transfer.adapters.modelexpress.runtime import (
            install_modelexpress_runtime_patch,
        )

        install_modelexpress_runtime_patch()
        register_strategy_with_modelexpress()
        super().__init__(load_config)


__all__ = [
    "LOAD_FORMAT",
    "LOAD_FORMAT_ALIASES",
    "GpustackMxModelLoader",
]
