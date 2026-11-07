"""Lazy registration for the explicit ModelExpress load format."""


def register():
    """Import ModelExpress only for the explicitly selected integration entry."""
    import os

    if os.environ.get("GPUSTACK_MODEL_TRANSFER_LOAD_FORMAT") != "gpustack-modelexpress":
        return
    from vllm.model_executor.model_loader import register_model_loader

    from gpustack_model_transfer.adapters.modelexpress.vllm import (
        LOAD_FORMAT,
        GpustackModelExpressLoader,
    )

    register_model_loader(LOAD_FORMAT)(GpustackModelExpressLoader)
