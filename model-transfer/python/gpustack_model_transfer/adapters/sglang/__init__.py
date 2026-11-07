"""The native SGLang weight-loading adapter over the model-transfer local reader.

Importing this package needs neither torch nor sglang: those are imported
lazily, inside torch_runtime's methods, so a CPU-only stdlib checkout can
probe, plan, preflight, shard, and test the whole load path. The pinned
extension route lives in constants; the load path lives in loader; the
driver surface an engine host implements lives in runtime; the sharding
plans and load-group lifecycle live in sharding.
"""

from gpustack_model_transfer.adapters.sglang.constants import (
    CONTRACT_NAME,
    CONTRACT_VERSION,
    IMAGE_READER_LABEL,
    INTEGRATION_ROUTE,
    LOADER_CLASS_NAME,
    LOADER_ENTRY_POINT,
    LOADER_ENTRY_POINT_GROUP,
    LOADER_LOAD_FORMAT,
    NIXL_ENGINE_CONFIG,
    PINNED_UPSTREAM_COMMIT,
    SUPPORTED_CHECKPOINT_FORMAT,
    SUPPORTED_DTYPES,
    image_integration_metadata,
)
from gpustack_model_transfer.adapters.sglang.loader import (
    LoadPlan,
    LoadPlanEntry,
    LoadReport,
    ModelTransferSGLangLoader,
    ResourceLedger,
    build_load_plan,
    profile_kv_capacity,
)
from gpustack_model_transfer.adapters.sglang.ownership import (
    BorrowedContract,
    LoadOptions,
    TensorOwnership,
)
from gpustack_model_transfer.adapters.sglang.sharding import (
    STRATEGY_COLUMN,
    STRATEGY_REPLICATE,
    STRATEGY_ROW,
    LoadGroupFailure,
    LoadGroupReport,
    ShardAssignment,
    ShardingPlan,
    ShardSlice,
    ShardView,
    build_sharding_plan,
    classify,
    load_group,
)
from gpustack_model_transfer.adapters.sglang.tolerances import (
    DEFAULT_FORWARD_TOLERANCE,
    DEFAULT_PARAMETER_TOLERANCE,
    ToleranceSpec,
)

# The suite-facing loader name: the same class, under the name an engine
# image's integration point resolves for either adapter family.
ModelTransferModelLoader = ModelTransferSGLangLoader

__all__ = [
    "CONTRACT_NAME",
    "CONTRACT_VERSION",
    "DEFAULT_FORWARD_TOLERANCE",
    "DEFAULT_PARAMETER_TOLERANCE",
    "IMAGE_READER_LABEL",
    "INTEGRATION_ROUTE",
    "LOADER_CLASS_NAME",
    "LOADER_ENTRY_POINT",
    "LOADER_ENTRY_POINT_GROUP",
    "LOADER_LOAD_FORMAT",
    "NIXL_ENGINE_CONFIG",
    "PINNED_UPSTREAM_COMMIT",
    "STRATEGY_COLUMN",
    "STRATEGY_REPLICATE",
    "STRATEGY_ROW",
    "SUPPORTED_CHECKPOINT_FORMAT",
    "SUPPORTED_DTYPES",
    "BorrowedContract",
    "LoadGroupFailure",
    "LoadGroupReport",
    "LoadOptions",
    "LoadPlan",
    "LoadPlanEntry",
    "LoadReport",
    "ModelTransferModelLoader",
    "ModelTransferSGLangLoader",
    "ResourceLedger",
    "ShardAssignment",
    "ShardSlice",
    "ShardView",
    "ShardingPlan",
    "TensorOwnership",
    "ToleranceSpec",
    "build_load_plan",
    "build_sharding_plan",
    "classify",
    "image_integration_metadata",
    "load_group",
    "profile_kv_capacity",
]
