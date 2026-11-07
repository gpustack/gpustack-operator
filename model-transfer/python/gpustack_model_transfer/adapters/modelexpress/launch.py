"""Launch the explicit remote ModelExpress vLLM entry after validating its hook."""

import os
import runpy
import sys

from gpustack_model_transfer.adapters.modelexpress.entry import (
    validate_environment,
    validate_plugin,
)
from gpustack_model_transfer.adapters.modelexpress.policy import StrategyRecoveryError

LOAD_FORMAT = "gpustack-modelexpress"


def validate_launch(args):
    """Reject missing or conflicting loader selection before vLLM starts."""
    validate_environment()
    formats = []
    for index, arg in enumerate(args):
        if arg == "--load-format":
            formats.append(args[index + 1] if index + 1 < len(args) else None)
        elif arg.startswith("--load-format="):
            formats.append(arg.split("=", 1)[1])
    if formats != [LOAD_FORMAT]:
        raise StrategyRecoveryError(
            "select exactly one --load-format gpustack-modelexpress"
        )
    selected = os.environ.get("VLLM_PLUGINS")
    if selected is not None:
        plugins = [item.strip() for item in selected.split(",") if item.strip()]
        if "gpustack_modelexpress" not in plugins:
            raise StrategyRecoveryError(
                "VLLM_PLUGINS must include gpustack_modelexpress"
            )
        os.environ["VLLM_PLUGINS"] = ",".join(plugins)
    validate_plugin()


def main():
    args = sys.argv[1:]
    if args[:1] == ["--"]:
        args = args[1:]
    validate_launch(args)
    sys.argv = ["vllm.entrypoints.openai.api_server", *args]
    runpy.run_module("vllm.entrypoints.openai.api_server", run_name="__main__")


if __name__ == "__main__":
    main()
