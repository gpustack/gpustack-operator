"""The pinned SGLang loader extension contract and image integration route.

These constants are the route an engine image is built and configured
against: the load format name, the loader class and its entry point, the
one checkpoint format and the dtypes this route accepts. The route is the
versioned-patch route against the pinned upstream commit: SGLang has no
stable public loader registry at the reviewed revision, so the image owns
a maintained patch that registers this loader. Changing a value here
changes the pinned route, and the tests pin these values back.
"""

from gpustack_model_transfer.core import versioning

# The load format an engine passes to select this loader, matching SGLang's
# --load-format option.
LOADER_LOAD_FORMAT = "model-transfer"

# The loader class an image imports for the pinned route.
LOADER_CLASS_NAME = "ModelTransferSGLangLoader"

# Where the maintained patch registers the loader: SGLang's model loader
# module and the module:attr target the patched startup resolves.
LOADER_ENTRY_POINT_GROUP = "sglang.srt.model_loader"
LOADER_ENTRY_POINT = "gpustack_model_transfer.adapters.sglang:ModelTransferSGLangLoader"

# The integration route the image maintenance contract names. The spec
# requires choosing one: the private-loader, remote-backend, or
# versioned-patch route. This adapter pins the versioned-patch route.
INTEGRATION_ROUTE = "versioned-patch"

# The upstream SGLang revision the maintained patch is carried against, the
# commit the spec's source review pinned.
PINNED_UPSTREAM_COMMIT = "sglang@8fabf5e65c0e0bf9698551c2e6cfef0672e9084a"

# The checkpoint format this route reads through the local reader, and the
# dtypes it assembles accurately. Dense safetensors only: quantized
# payloads are refused by policy in metadata.py, not approximated here.
SUPPORTED_CHECKPOINT_FORMAT = "safetensors"
SUPPORTED_DTYPES = ("F32", "F16", "BF16", "I64")

# The reader contract this loader was built against.
CONTRACT_NAME = versioning.CONTRACT_NAME
CONTRACT_VERSION = versioning.CONTRACT_VERSION

# Image integration: the label an engine image carries to declare that it
# bundles the local reader route.
IMAGE_READER_LABEL = "gpustack-model-transfer-local-reader"

# The connector configuration shape this engine preserves untouched through
# a load; the acceptance suite loads with it and asserts nothing moved.
NIXL_ENGINE_CONFIG = {
    "kv_connector": "NixlConnector",
    "kv_role": "kv_both",
    "ec_connector": "NixlConnector",
    "ec_role": "ec_both",
}


def image_integration_metadata() -> dict:
    """Return the image integration metadata for the pinned route."""
    return {
        "reader": IMAGE_READER_LABEL,
        "load-format": LOADER_LOAD_FORMAT,
        "contract": f"{CONTRACT_NAME}/{CONTRACT_VERSION}",
        "engine": "sglang",
        "route": INTEGRATION_ROUTE,
        "upstream": PINNED_UPSTREAM_COMMIT,
        "checkpoint-format": SUPPORTED_CHECKPOINT_FORMAT,
        "entry-point": LOADER_ENTRY_POINT,
    }


__all__ = [
    "CONTRACT_NAME",
    "CONTRACT_VERSION",
    "IMAGE_READER_LABEL",
    "INTEGRATION_ROUTE",
    "LOADER_CLASS_NAME",
    "LOADER_ENTRY_POINT",
    "LOADER_ENTRY_POINT_GROUP",
    "LOADER_LOAD_FORMAT",
    "NIXL_ENGINE_CONFIG",
    "PINNED_UPSTREAM_COMMIT",
    "SUPPORTED_CHECKPOINT_FORMAT",
    "SUPPORTED_DTYPES",
    "image_integration_metadata",
]
