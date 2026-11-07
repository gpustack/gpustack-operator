"""The pinned ModelExpress integration route and its composition rules.

ModelExpress is an optional external entry path over the same reader
contract: a qualified external inference strategy, registered through the
one supported plugin hook, driving the native engine adapter. These
constants pin the reviewed commit (never a release tag), name the hook,
and record the built-in chain order the composition rules reason about.

The reviewed entry is a CUDA qualification target. It provides no CANN
accelerator adapter, and no claim here may say otherwise.
"""

from gpustack_model_transfer.core import versioning

# The environment variable a plugin-capable build exposes: the reviewed
# insertion point. A build without it has no supported route for us.
LOAD_PLUGIN_VARIABLE = "MX_LOAD_PLUGIN"

# The exact plugin-capable upstream revision the route was reviewed
# against. Availability is never inferred from an older release tag.
PINNED_COMMIT = "modelexpress@9410f3efbaa7aae57917d670de731a79f36684f3"

# The reviewed built-in chain, in order. The hook inserts one plugin; it
# cannot reorder or remove these. GPUStack's plugin must sit before
# "rdma"; everything earlier than the insertion point is P2P consumption
# that must be disabled or explicitly authorized, and everything later
# must preserve GPUStack identity, authorization, and delivery
# constraints or the composition is refused.
BUILTIN_CHAIN = ("p2p", "rdma", "server_cache")

# Where the GPUStack plugin must be inserted relative to the chain.
INSERT_BEFORE = "rdma"

# The strategies whose reachability the composition must authorize
# explicitly, because each one bypasses GPUStack's source identity.
EARLIER_STRATEGIES = ("p2p",)
LATER_STRATEGIES = ("server_cache",)

# The offline guards that back the terminal error under Require: a
# second boundary so a later strategy cannot quietly download a full
# checkpoint even if the first boundary failed to halt the chain.
OFFLINE_GUARDS = ("HF_HUB_OFFLINE=1", "TRANSFORMERS_OFFLINE=1")

# The remote delivery policies, mirroring the cluster setting.
POLICY_PREFER = "prefer"
POLICY_REQUIRE = "require"

# The reader contract this adapter was built against.
CONTRACT_NAME = versioning.CONTRACT_NAME
CONTRACT_VERSION = versioning.CONTRACT_VERSION

# Image integration: the label an engine image carries to declare that it
# bundles the pinned plugin route.
IMAGE_PLUGIN_LABEL = "gpustack-model-transfer-modelexpress-plugin"

# The accelerator targets this entry is qualified against. CUDA only: the
# reviewed ModelExpress entry provides no CANN accelerator adapter.
QUALIFIED_ACCELERATORS = ("cuda",)


def image_integration_metadata() -> dict:
    """Return the image integration metadata for the pinned route."""
    return {
        "plugin-label": IMAGE_PLUGIN_LABEL,
        "hook": LOAD_PLUGIN_VARIABLE,
        "upstream": PINNED_COMMIT,
        "contract": f"{CONTRACT_NAME}/{CONTRACT_VERSION}",
        "engine": "modelexpress",
        "accelerators": ",".join(QUALIFIED_ACCELERATORS),
    }


__all__ = [
    "BUILTIN_CHAIN",
    "CONTRACT_NAME",
    "CONTRACT_VERSION",
    "EARLIER_STRATEGIES",
    "IMAGE_PLUGIN_LABEL",
    "INSERT_BEFORE",
    "LATER_STRATEGIES",
    "LOAD_PLUGIN_VARIABLE",
    "OFFLINE_GUARDS",
    "PINNED_COMMIT",
    "POLICY_PREFER",
    "POLICY_REQUIRE",
    "QUALIFIED_ACCELERATORS",
    "image_integration_metadata",
]
