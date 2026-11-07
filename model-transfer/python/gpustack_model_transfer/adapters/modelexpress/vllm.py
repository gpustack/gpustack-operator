"""Explicit vLLM loader entry that keeps ModelExpress's real strategy chain."""

from gpustack_model_transfer.adapters.modelexpress.entry import validate_plugin
from gpustack_model_transfer.adapters.modelexpress.policy import StrategyRecoveryError
from modelexpress.engines.vllm.loader import MxModelLoader

LOAD_FORMAT = "gpustack-modelexpress"


class GpustackModelExpressLoader(MxModelLoader):
    """Cold remote loading without ModelExpress's default disk download path."""

    def __init__(self, load_config):
        validate_plugin()
        if getattr(load_config, "load_format", None) != LOAD_FORMAT:
            raise StrategyRecoveryError(
                "the explicit GPUStack ModelExpress load format is required"
            )
        super().__init__(load_config)

    def download_model(self, model_config):
        """Probe authenticated metadata without downloading weight files."""
        validate_plugin()
        from gpustack_model_transfer.adapters.vllm.native import (
            NativeModelTransferLoader,
        )

        try:
            NativeModelTransferLoader(self.load_config).download_model(model_config)
        except Exception as exc:
            raise StrategyRecoveryError(
                "ModelExpress remote metadata probe failed"
            ) from exc

    def load_model(self, vllm_config, model_config, prefix=""):
        validate_plugin()
        if getattr(vllm_config, "speculative_config", None) is not None:
            raise StrategyRecoveryError("speculative loading is not supported")
        return super().load_model(vllm_config, model_config, prefix)

    def _load_model(
        self, vllm_config, model_config, prefix, ctx, is_speculative_draft, model=None
    ):
        # Disable upstream cache installation and publication before model setup.
        validate_plugin()
        if is_speculative_draft or model is not None:
            raise StrategyRecoveryError("only cold main-model loading is supported")
        ctx.p2p_enabled = False
        return super()._load_model(
            vllm_config, model_config, prefix, ctx, is_speculative_draft, model
        )

    def _run_load_strategy_chain(self, model, ctx):
        validate_plugin()
        ctx.p2p_enabled = False
        return super()._run_load_strategy_chain(model, ctx)

    def load_weights(self, model, model_config):
        raise StrategyRecoveryError(
            "standalone ModelExpress weight loading is not supported"
        )

    def reload_model(self, vllm_config, model_config, model, prefix=""):
        raise StrategyRecoveryError("ModelExpress remote reload is not supported")
