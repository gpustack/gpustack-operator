"""The ModelExpress launch surface: recognition and the launch env.

The definitions live in the import leaf (gpustack_model_transfer
.modelexpress_route) that the packaged bootstrap reads without importing
this package; this module re-exports them for callers that already own
the adapter import.
"""

from gpustack_model_transfer.modelexpress_route import (
    LOAD_FORMAT,
    LOAD_FORMAT_ALIASES,
    apply_launch_env,
    argv_requests_gpustack_modelexpress,
    env_requests_gpustack_modelexpress,
    load_format_values,
    main,
    requested,
    requested_load_format,
)

__all__ = [
    "LOAD_FORMAT",
    "LOAD_FORMAT_ALIASES",
    "apply_launch_env",
    "argv_requests_gpustack_modelexpress",
    "env_requests_gpustack_modelexpress",
    "load_format_values",
    "main",
    "requested",
    "requested_load_format",
]
