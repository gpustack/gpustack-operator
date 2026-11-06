"""Native vLLM loader registration and admitted checkpoint properties."""

from gpustack_model_transfer.core import versioning

# The load format an engine passes to select this loader, e.g.
# LLM(..., load_format="model-transfer").
LOADER_LOAD_FORMAT = "model-transfer"

# The class registered by the general plugin.
LOADER_CLASS_NAME = "NativeModelTransferLoader"

# Where the loader is registered: entry point group and the module:attr
# target an engine image resolves at startup.
LOADER_ENTRY_POINT_GROUP = "vllm.general_plugins"
LOADER_ENTRY_POINT = "gpustack_model_transfer.adapters.vllm.plugin:register"

# The checkpoint format the authenticated reader is required to verify, and the
# dtypes it assembles accurately. Dense safetensors only: quantized
# payloads are refused by policy in metadata.py, not approximated here.
SUPPORTED_CHECKPOINT_FORMAT = "safetensors"
SUPPORTED_DTYPES = ("F32", "F16", "BF16", "I64")

# The reader contract this loader was built against.
CONTRACT_NAME = versioning.CONTRACT_NAME
CONTRACT_VERSION = versioning.CONTRACT_VERSION

# Reader identity advertised by this route.
IMAGE_READER_LABEL = "gpustack-model-transfer-authenticated-reader"


def image_integration_metadata() -> dict:
    """Return the image integration metadata for the pinned route."""
    return {
        "reader": IMAGE_READER_LABEL,
        "load-format": LOADER_LOAD_FORMAT,
        "contract": f"{CONTRACT_NAME}/{CONTRACT_VERSION}",
        "engine": "vllm",
        "checkpoint-format": SUPPORTED_CHECKPOINT_FORMAT,
        "entry-point": LOADER_ENTRY_POINT,
    }


__all__ = [
    "CONTRACT_NAME",
    "CONTRACT_VERSION",
    "IMAGE_READER_LABEL",
    "LOADER_CLASS_NAME",
    "LOADER_ENTRY_POINT",
    "LOADER_ENTRY_POINT_GROUP",
    "LOADER_LOAD_FORMAT",
    "SUPPORTED_CHECKPOINT_FORMAT",
    "SUPPORTED_DTYPES",
    "image_integration_metadata",
]
