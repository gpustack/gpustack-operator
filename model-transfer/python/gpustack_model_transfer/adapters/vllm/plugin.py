"""Lazy vLLM general plugin, shared by CUDA and vLLM-Ascend."""


def register() -> None:
    """Register the loader once vLLM has loaded its general plugins."""
    from gpustack_model_transfer.adapters.vllm.native import NativeModelTransferLoader
    from gpustack_model_transfer.envs import ENVS
    from vllm.model_executor.model_loader import register_model_loader

    if ENVS.GPUSTACK_MODEL_TRANSFER_BACKEND == "hixl":
        timeout_str = ENVS.GPUSTACK_MODEL_TRANSFER_TIMEOUT_STR
        # The collective stream must outlive a full remote fill: a broadcast
        # whose ranks queue faster than the coordinator transfers hits the
        # collective timeout unless it matches the transfer timeout.
        ENVS.setdefault("HCCL_EXEC_TIMEOUT", timeout_str)
        ENVS.setdefault("HCCL_CONNECT_TIMEOUT", timeout_str)

    register_model_loader("model-transfer")(NativeModelTransferLoader)
