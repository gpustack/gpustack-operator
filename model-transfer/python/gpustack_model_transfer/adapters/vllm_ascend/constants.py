"""The pinned vLLM-Ascend loader extension contract and image integration route.

vLLM-Ascend is a vLLM platform plugin: it rides vLLM's own loader
registry, and the NPU stack behind it — torch_npu, the ACL runtime, HCCL —
is reached only through npu_runtime's lazy imports. These constants are the
route an engine image is built and configured against; changing a value
here changes the pinned route, and the tests pin these values back.
"""

from gpustack_model_transfer.core import versioning

# The load format an engine passes to select this loader, matching the
# --load-format option vLLM-Ascend inherits from vLLM.
LOADER_LOAD_FORMAT = "model-transfer"

# The class registered by the shared general plugin.
LOADER_CLASS_NAME = "NativeModelTransferLoader"

# Where the loader is registered: vLLM-Ascend is a platform plugin, so the
# registry is vLLM's own, and the entry point names this package.
LOADER_ENTRY_POINT_GROUP = "vllm.general_plugins"
LOADER_ENTRY_POINT = "gpustack_model_transfer.adapters.vllm.plugin:register"

# The integration route the image maintenance contract names. SGLang needs
# a maintained patch; vLLM-Ascend integrates as the platform plugin it
# already is, against the pinned platform package below.
INTEGRATION_ROUTE = "platform-plugin"
PLATFORM_PACKAGE = "vllm_ascend"
# The upstream vLLM-Ascend revision the platform plugin is pinned against,
# the commit the spec's source review pinned.
PINNED_UPSTREAM_COMMIT = "vllm-ascend@31f9175f324182466877d0ab9006e75ea6a401f3"

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

# The NPU stream a borrowed tensor becomes visible on before its fence
# returns: an event record on this stream orders the transfer's writes
# ahead of the consuming kernels' reads.
DEFAULT_VISIBILITY_STREAM = "npu:0"

# The process environment a load must never disturb: device visibility,
# the HCCL interface address, and the transfer backend choice. An engine
# process that arrived with these set owns them; the adapter records the
# engine configuration untouched and writes none of these.
CONNECTOR_ENV_KEYS = ("ASCEND_RT_VISIBLE_DEVICES", "HCCL_IF_IP", "HIXL_BACKEND")

# The KV/EC connector configuration shape this route preserves untouched
# through a load: the Ascend transfer backend, both roles, NPU buffers.
ASCEND_ENGINE_CONFIG = {
    "kv_transfer_config": {
        "kv_connector": "HixlConnector",
        "kv_role": "kv_both",
        "kv_buffer_device": "npu",
    },
    "ec_connector_config": {
        "ec_connector": "HixlConnector",
        "ec_role": "ec_both",
    },
}


def image_integration_metadata() -> dict:
    """Return the image integration metadata for the pinned route."""
    return {
        "reader": IMAGE_READER_LABEL,
        "load-format": LOADER_LOAD_FORMAT,
        "contract": f"{CONTRACT_NAME}/{CONTRACT_VERSION}",
        "engine": "vllm-ascend",
        "platform-package": PLATFORM_PACKAGE,
        "integration": INTEGRATION_ROUTE,
        "upstream": PINNED_UPSTREAM_COMMIT,
        "checkpoint-format": SUPPORTED_CHECKPOINT_FORMAT,
        "entry-point": LOADER_ENTRY_POINT,
        "visibility-stream": DEFAULT_VISIBILITY_STREAM,
    }


__all__ = [
    "ASCEND_ENGINE_CONFIG",
    "CONNECTOR_ENV_KEYS",
    "CONTRACT_NAME",
    "CONTRACT_VERSION",
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
    "image_integration_metadata",
]
