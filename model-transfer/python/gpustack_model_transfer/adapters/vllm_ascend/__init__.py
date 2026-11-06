"""vLLM-Ascend reference fixtures and shared native-plugin metadata.

The real loader is registered by the shared vLLM plugin. Importing this
package does not import torch, torch_npu, or vllm_ascend.
"""

from gpustack_model_transfer.adapters.vllm_ascend.constants import (
    ASCEND_ENGINE_CONFIG,
    CONNECTOR_ENV_KEYS,
    CONTRACT_NAME,
    CONTRACT_VERSION,
    DEFAULT_VISIBILITY_STREAM,
    IMAGE_READER_LABEL,
    INTEGRATION_ROUTE,
    LOADER_CLASS_NAME,
    LOADER_ENTRY_POINT,
    LOADER_ENTRY_POINT_GROUP,
    LOADER_LOAD_FORMAT,
    PINNED_UPSTREAM_COMMIT,
    PLATFORM_PACKAGE,
    SUPPORTED_CHECKPOINT_FORMAT,
    SUPPORTED_DTYPES,
    image_integration_metadata,
)
from gpustack_model_transfer.adapters.vllm_ascend.loader import (
    LoadPlan,
    LoadPlanEntry,
    LoadReport,
    ModelTransferAscendLoader,
    ResourceLedger,
    build_load_plan,
    profile_kv_capacity,
)
from gpustack_model_transfer.adapters.vllm_ascend.ownership import (
    BorrowedContract,
    LoadOptions,
    TensorOwnership,
)
from gpustack_model_transfer.adapters.vllm_ascend.tolerances import (
    DEFAULT_FORWARD_TOLERANCE,
    DEFAULT_PARAMETER_TOLERANCE,
    ToleranceSpec,
)

# The model_load_suite invokes adapter.ModelTransferModelLoader uniformly across engines.
ModelTransferModelLoader = ModelTransferAscendLoader

__all__ = [
    "ASCEND_ENGINE_CONFIG",
    "CONNECTOR_ENV_KEYS",
    "CONTRACT_NAME",
    "CONTRACT_VERSION",
    "DEFAULT_FORWARD_TOLERANCE",
    "DEFAULT_PARAMETER_TOLERANCE",
    "DEFAULT_VISIBILITY_STREAM",
    "IMAGE_READER_LABEL",
    "INTEGRATION_ROUTE",
    "LOADER_CLASS_NAME",
    "LOADER_ENTRY_POINT",
    "LOADER_ENTRY_POINT_GROUP",
    "LOADER_LOAD_FORMAT",
    "PINNED_UPSTREAM_COMMIT",
    "PLATFORM_PACKAGE",
    "SUPPORTED_CHECKPOINT_FORMAT",
    "SUPPORTED_DTYPES",
    "BorrowedContract",
    "LoadOptions",
    "LoadPlan",
    "LoadPlanEntry",
    "LoadReport",
    "ModelTransferAscendLoader",
    "ModelTransferModelLoader",
    "ResourceLedger",
    "TensorOwnership",
    "ToleranceSpec",
    "build_load_plan",
    "image_integration_metadata",
    "profile_kv_capacity",
]
