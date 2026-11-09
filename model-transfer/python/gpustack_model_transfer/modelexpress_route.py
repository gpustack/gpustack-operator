"""The ModelExpress route's recognition and launch env, as an import leaf.

The packaged bootstrap runs in every interpreter that has the wheel, so
this module deliberately imports nothing from the adapter package, from
ModelExpress, or from any engine runtime: parsing a command line and
defaulting three environment variables must never drag torch into a
model-transfer helper process. The adapter package re-exports this
surface; this leaf is the definition.
"""

import os

# The standard ModelExpress load formats this route answers to. The
# first is the canonical name; the second is the upstream shorthand.
LOAD_FORMAT = "modelexpress"
LOAD_FORMAT_ALIASES = ("modelexpress", "mx")

# The strategy name the chain knows the GPUStack plugin by, and where
# the plugin hook inserts it: before the upstream cache strategy, so
# the chain runs RdmaStrategy first (Peer D2D), then GpustackLoadStrategy
# (verified source delivery), then ServerCacheStrategy and DefaultStrategy.
PLUGIN_NAME = "gpustack"
INSERT_BEFORE = "server-cache"

# The recognized flag spellings, with and without the equals form.
_LOAD_FORMAT_FLAGS = ("--load-format", "--load_format")
_PLUGIN_HOOK_VARIABLE = "MX_LOAD_PLUGIN"
_PLUGIN_BEFORE_VARIABLE = "MX_LOAD_PLUGIN_BEFORE"
_ARTIFACT_TRANSFER_VARIABLE = "MX_ARTIFACT_TRANSFER"
_SERVER_ADDRESS_VARIABLE = "MX_SERVER_ADDRESS"
_LOAD_FORMAT_VARIABLE = "GPUSTACK_MODEL_TRANSFER_LOAD_FORMAT"


def _normalized(value) -> str:
    return str(value or "").strip().lower()


def load_format_values(argv) -> list:
    """Every --load-format value on the command line, in order."""
    values = []
    args = list(argv)
    for index, arg in enumerate(args):
        if arg in _LOAD_FORMAT_FLAGS:
            values.append(args[index + 1] if index + 1 < len(args) else None)
        for flag in _LOAD_FORMAT_FLAGS:
            if arg.startswith(flag + "="):
                values.append(arg.split("=", 1)[1])
    return values


def requested_load_format(argv) -> str:
    """The effective --load-format selection: the last flag wins."""
    values = [value for value in load_format_values(argv) if value is not None]
    return _normalized(values[-1]) if values else ""


def argv_requests_gpustack_modelexpress(argv) -> bool:
    """True when the command line selects the ModelExpress load format."""
    return requested_load_format(argv) in LOAD_FORMAT_ALIASES


def env_requests_gpustack_modelexpress(environ=None) -> bool:
    """True when the environment selects the GPUStack ModelExpress plugin.

    Both the plugin hook (MX_LOAD_PLUGIN=gpustack) and the
    operator-injected load-format variable GPUSTACK_MODEL_TRANSFER_
    LOAD_FORMAT are recognized, so the strategy registers no matter
    which channel carried the request. The load-format variable carries
    one of LOAD_FORMAT_ALIASES ('modelexpress' or 'mx'), so engine
    subprocesses that inherit no vLLM command line still see the
    selection.
    """
    source = os.environ if environ is None else environ
    if _normalized(source.get(_PLUGIN_HOOK_VARIABLE)) == PLUGIN_NAME:
        return True
    load_format = source.get(_LOAD_FORMAT_VARIABLE, "")
    return _normalized(load_format) in LOAD_FORMAT_ALIASES


def requested(environ=None, argv=None) -> bool:
    """True when this process asked for the ModelExpress route at all.

    The single gate for every optional registration: the packaged
    bootstrap, the engine general-plugins entry, and the loader hooks
    all check this before importing or touching anything.
    """
    import sys

    if argv is None:
        argv = sys.argv
    source = os.environ if environ is None else environ
    return argv_requests_gpustack_modelexpress(argv) or (
        env_requests_gpustack_modelexpress(source)
    )


def apply_launch_env(env=None) -> dict:
    """Apply the plugin hook defaults to the environment, in place.

    The hook variables name the GPUStack plugin and its insertion point
    before the upstream cache strategy. The artifact transfer switch is
    only defaulted when a ModelExpress metadata server is configured:
    publishing to no server is not a mode, it is a misconfiguration.
    """
    target = os.environ if env is None else env
    target.setdefault(_PLUGIN_HOOK_VARIABLE, PLUGIN_NAME)
    target.setdefault(_PLUGIN_BEFORE_VARIABLE, INSERT_BEFORE)
    if _normalized(target.get(_SERVER_ADDRESS_VARIABLE)):
        target.setdefault(_ARTIFACT_TRANSFER_VARIABLE, "1")
    return target


def main():
    """Launch the vLLM API server under the ModelExpress launch env."""
    import runpy
    import sys

    args = sys.argv[1:]
    if args[:1] == ["--"]:
        args = args[1:]
    if requested():
        apply_launch_env()
    sys.argv = ["vllm.entrypoints.openai.api_server", *args]
    runpy.run_module("vllm.entrypoints.openai.api_server", run_name="__main__")


if __name__ == "__main__":
    main()


__all__ = [
    "INSERT_BEFORE",
    "LOAD_FORMAT",
    "LOAD_FORMAT_ALIASES",
    "PLUGIN_NAME",
    "apply_launch_env",
    "argv_requests_gpustack_modelexpress",
    "env_requests_gpustack_modelexpress",
    "load_format_values",
    "main",
    "requested",
    "requested_load_format",
]
