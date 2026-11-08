"""The ModelExpress integration route: formats, plugin placement, policies.

ModelExpress is an optional external entry path over the same reader
contract: a qualified external inference strategy, registered through the
one supported plugin hook, driving the native engine adapter. The route
constants live in one import leaf (gpustack_model_transfer.modelexpress_route)
that the packaged bootstrap reads without importing this package; this
module re-exports them alongside the qualification metadata.

The reviewed entry is qualified against CUDA, and its qualification is a
record, not a refusal: the strategy's native transport follows whatever
engine backend the process runs, and an accelerator family is never
refused on the entry's behalf.
"""

from gpustack_model_transfer.core import versioning
from gpustack_model_transfer.modelexpress_route import (
    INSERT_BEFORE,
    LOAD_FORMAT,
    LOAD_FORMAT_ALIASES,
    PLUGIN_NAME,
)

# The environment variable a plugin-capable build exposes: the reviewed
# insertion point. A build without it has no supported route for us.
LOAD_PLUGIN_VARIABLE = "MX_LOAD_PLUGIN"

# The companion variable naming where the plugin hook inserts the
# strategy. It names the built-in strategy exactly as the pinned upstream
# names it: "server-cache".
LOAD_PLUGIN_BEFORE_VARIABLE = "MX_LOAD_PLUGIN_BEFORE"

# The cluster's remote delivery policy for the ModelExpress route.
# "Require" permits only Peer D2D and the GPUStack source delivery;
# "Prefer" (the default) keeps the built-in local fallback strategies.
DELIVERY_POLICY_VARIABLE = "GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY"

# The exact plugin-capable upstream revision the route was reviewed
# against. Availability is never inferred from an older release tag.
PINNED_COMMIT = "modelexpress@9410f3efbaa7aae57917d670de731a79f36684f3"

# The reviewed built-in chain, in order, as the pinned upstream builds
# and names it. The hook inserts one plugin; it cannot reorder or remove
# these.
BUILTIN_CHAIN = (
    "rdma",
    "server-cache",
    "instant_tensor",
    "model_streamer",
    "gds",
    "default",
)

# The local fallback strategies the Require delivery policy strips:
# each one loads from local disk or downloads a full checkpoint,
# bypassing GPUStack's verified source identity. Both spellings of the
# cache strategy are listed because the reviewed upstream names it
# "server-cache" while earlier revisions used "server_cache".
FALLBACK_STRATEGIES = (
    "server_cache",
    "server-cache",
    "instant_tensor",
    "model_streamer",
    "gds",
    "default",
)

# The remote delivery policies, mirroring the cluster setting.
POLICY_PREFER = "prefer"
POLICY_REQUIRE = "require"

# The second boundary behind the Require terminal error: hub download
# libraries check these before reaching the network, so a strategy the
# first boundary failed to halt cannot quietly fetch a full checkpoint.
OFFLINE_GUARDS = ("HF_HUB_OFFLINE=1", "TRANSFORMERS_OFFLINE=1")

# The reader contract this adapter was built against.
CONTRACT_NAME = versioning.CONTRACT_NAME
CONTRACT_VERSION = versioning.CONTRACT_VERSION

# Image integration: the label an engine image carries to declare that it
# bundles the pinned plugin route.
IMAGE_PLUGIN_LABEL = "gpustack-model-transfer-modelexpress-plugin"

# The accelerator families the entry's review covered. A qualification
# record, never a refusal: the strategy's native transport follows the
# engine backend the process actually runs.
QUALIFIED_ACCELERATORS = ("cuda",)


def image_integration_metadata() -> dict:
    """Return the image integration metadata for the pinned route."""
    return {
        "plugin-label": IMAGE_PLUGIN_LABEL,
        "hook": LOAD_PLUGIN_VARIABLE,
        "insert-before": INSERT_BEFORE,
        "upstream": PINNED_COMMIT,
        "contract": f"{CONTRACT_NAME}/{CONTRACT_VERSION}",
        "engine": "modelexpress",
        "accelerators": ",".join(QUALIFIED_ACCELERATORS),
    }


__all__ = [
    "BUILTIN_CHAIN",
    "CONTRACT_NAME",
    "CONTRACT_VERSION",
    "DELIVERY_POLICY_VARIABLE",
    "FALLBACK_STRATEGIES",
    "IMAGE_PLUGIN_LABEL",
    "INSERT_BEFORE",
    "LOAD_FORMAT",
    "LOAD_FORMAT_ALIASES",
    "LOAD_PLUGIN_BEFORE_VARIABLE",
    "LOAD_PLUGIN_VARIABLE",
    "OFFLINE_GUARDS",
    "PINNED_COMMIT",
    "PLUGIN_NAME",
    "POLICY_PREFER",
    "POLICY_REQUIRE",
    "QUALIFIED_ACCELERATORS",
    "image_integration_metadata",
]
