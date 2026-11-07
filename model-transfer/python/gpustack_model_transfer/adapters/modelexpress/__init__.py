"""The optional ModelExpress inference-strategy adapter.

Missing ModelExpress and torch packages are tolerated. When ModelExpress is
installed, importing the entry discovers its strategy base and may import
transitive dependencies. The plugin drives the qualified native engine adapter (the vLLM ModelTransferModelLoader),
the runtime probe reports what a host is missing, and the pinned image
integration lives in constants. The portable reader stays independent of
ModelExpress internal context types — nothing in gpustack_model_transfer.core
imports anything here, and nothing here reaches into ModelExpress
internals beyond the reviewed plugin hook.
"""

from gpustack_model_transfer.adapters.modelexpress.chain import (
    CompositionError,
    apply_offline_guards,
    authorize_earlier,
    build_chain,
    check_composition,
    offline_guards_active,
)
from gpustack_model_transfer.adapters.modelexpress.constants import (
    BUILTIN_CHAIN,
    IMAGE_PLUGIN_LABEL,
    INSERT_BEFORE,
    LOAD_PLUGIN_VARIABLE,
    OFFLINE_GUARDS,
    PINNED_COMMIT,
    POLICY_PREFER,
    POLICY_REQUIRE,
    QUALIFIED_ACCELERATORS,
    image_integration_metadata,
)
from gpustack_model_transfer.adapters.modelexpress.entry import (
    ENV_MANIFEST,
    ENV_MANIFEST_SHA256,
    ENV_PEER_P2P,
    ENV_PEERS_CONFIG,
    ENV_POLICY,
    GpustackLoadStrategy,
    build_plugin,
    peer_replica_endpoints,
    register_plugin,
)
from gpustack_model_transfer.adapters.modelexpress.group import (
    GroupDisagreementError,
    LoadGroupCoordinator,
    RankProposal,
)
from gpustack_model_transfer.adapters.modelexpress.plugin import (
    STRATEGY_NAME,
    GpustackLoadPlugin,
    PluginResult,
    preserve_terminal_error,
)
from gpustack_model_transfer.adapters.modelexpress.policy import (
    FallbackDecision,
    PluginUnavailableError,
    StrategyRecoveryError,
    WriterRefitFence,
    decide_fallback,
    route_downgrade,
)
from gpustack_model_transfer.adapters.modelexpress.publication import (
    PublicationRefusedError,
    checkpoint_identity,
    publication_allowed,
    refuse_publication,
)
from gpustack_model_transfer.adapters.modelexpress.runtime import (
    EntryReadiness,
    accelerator_claimed,
    probe_entry,
    refuse_cann,
)

__all__ = [
    "BUILTIN_CHAIN",
    "ENV_MANIFEST",
    "ENV_MANIFEST_SHA256",
    "ENV_PEERS_CONFIG",
    "ENV_PEER_P2P",
    "ENV_POLICY",
    "IMAGE_PLUGIN_LABEL",
    "INSERT_BEFORE",
    "LOAD_PLUGIN_VARIABLE",
    "OFFLINE_GUARDS",
    "PINNED_COMMIT",
    "POLICY_PREFER",
    "POLICY_REQUIRE",
    "QUALIFIED_ACCELERATORS",
    "STRATEGY_NAME",
    "CompositionError",
    "EntryReadiness",
    "FallbackDecision",
    "GpustackLoadPlugin",
    "GpustackLoadStrategy",
    "GroupDisagreementError",
    "LoadGroupCoordinator",
    "PluginResult",
    "PluginUnavailableError",
    "PublicationRefusedError",
    "RankProposal",
    "StrategyRecoveryError",
    "WriterRefitFence",
    "accelerator_claimed",
    "apply_offline_guards",
    "authorize_earlier",
    "build_chain",
    "build_plugin",
    "check_composition",
    "checkpoint_identity",
    "decide_fallback",
    "image_integration_metadata",
    "offline_guards_active",
    "peer_replica_endpoints",
    "preserve_terminal_error",
    "probe_entry",
    "publication_allowed",
    "refuse_cann",
    "refuse_publication",
    "register_plugin",
    "route_downgrade",
]
