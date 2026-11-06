"""vLLM adapter and local reference fixtures.

Importing this package needs neither torch nor vllm: those are imported
lazily, inside torch_runtime's methods, so a CPU-only stdlib checkout can
probe, plan, preflight, and test the whole load path. The pinned extension
route lives in plugin and native. The loader and runtime modules are reference
fixtures and do not create a native vLLM model.
"""

from gpustack_model_transfer.adapters.vllm.constants import (
    CONTRACT_NAME,
    CONTRACT_VERSION,
    IMAGE_READER_LABEL,
    LOADER_CLASS_NAME,
    LOADER_ENTRY_POINT,
    LOADER_ENTRY_POINT_GROUP,
    LOADER_LOAD_FORMAT,
    SUPPORTED_CHECKPOINT_FORMAT,
    SUPPORTED_DTYPES,
    image_integration_metadata,
)
from gpustack_model_transfer.adapters.vllm.loader import (
    LoadPlan,
    LoadPlanEntry,
    LoadReport,
    ModelTransferModelLoader,
    ResourceLedger,
    build_load_plan,
    profile_kv_capacity,
)
from gpustack_model_transfer.adapters.vllm.ownership import (
    BorrowedContract,
    LoadOptions,
    TensorOwnership,
)
from gpustack_model_transfer.adapters.vllm.tolerances import (
    DEFAULT_FORWARD_TOLERANCE,
    DEFAULT_PARAMETER_TOLERANCE,
    ToleranceSpec,
)

__all__ = [
    "CONTRACT_NAME",
    "CONTRACT_VERSION",
    "DEFAULT_FORWARD_TOLERANCE",
    "DEFAULT_PARAMETER_TOLERANCE",
    "IMAGE_READER_LABEL",
    "LOADER_CLASS_NAME",
    "LOADER_ENTRY_POINT",
    "LOADER_ENTRY_POINT_GROUP",
    "LOADER_LOAD_FORMAT",
    "SUPPORTED_CHECKPOINT_FORMAT",
    "SUPPORTED_DTYPES",
    "BorrowedContract",
    "LoadOptions",
    "LoadPlan",
    "LoadPlanEntry",
    "LoadReport",
    "ModelTransferModelLoader",
    "ResourceLedger",
    "TensorOwnership",
    "ToleranceSpec",
    "build_load_plan",
    "image_integration_metadata",
    "profile_kv_capacity",
]
