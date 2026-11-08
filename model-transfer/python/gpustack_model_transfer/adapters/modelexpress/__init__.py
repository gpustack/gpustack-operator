"""The optional ModelExpress inference-strategy adapter.

Missing ModelExpress and torch packages are tolerated. The light surface
(chain composition, launch recognition, runtime probe, constants) is
imported eagerly and stays free of every heavy dependency; the strategy
and its publication gate bind their upstream base on first use, so a
process that imports only the constants never pays for ModelExpress or
torch. The strategy follows the pinned upstream LoadStrategy ABI, the
runtime patch routes the upstream chain through the delivery-policy
filter, and the pinned image integration lives in constants. The
portable reader stays independent of ModelExpress internal context
types — nothing in gpustack_model_transfer.core imports anything here,
and nothing here reaches into ModelExpress internals beyond the
reviewed plugin hook.
"""

from gpustack_model_transfer.adapters.modelexpress.chain import (
    CompositionError,
    apply_offline_guards,
    build_chain,
    check_composition,
    filter_chain_strategies,
    offline_guards_active,
    place_gpustack_strategy,
    strategy_name,
)
from gpustack_model_transfer.adapters.modelexpress.constants import (
    BUILTIN_CHAIN,
    DELIVERY_POLICY_VARIABLE,
    FALLBACK_STRATEGIES,
    IMAGE_PLUGIN_LABEL,
    INSERT_BEFORE,
    LOAD_FORMAT,
    LOAD_FORMAT_ALIASES,
    LOAD_PLUGIN_BEFORE_VARIABLE,
    LOAD_PLUGIN_VARIABLE,
    OFFLINE_GUARDS,
    PINNED_COMMIT,
    PLUGIN_NAME,
    POLICY_PREFER,
    POLICY_REQUIRE,
    QUALIFIED_ACCELERATORS,
    image_integration_metadata,
)
from gpustack_model_transfer.adapters.modelexpress.group import (
    GroupDisagreementError,
    LoadGroupCoordinator,
    RankProposal,
)
from gpustack_model_transfer.adapters.modelexpress.launch import (
    apply_launch_env,
    argv_requests_gpustack_modelexpress,
    env_requests_gpustack_modelexpress,
)
from gpustack_model_transfer.adapters.modelexpress.policy import (
    DeliveryPolicy,
    FallbackDecision,
    PluginUnavailableError,
    StrategyRecoveryError,
    WriterRefitFence,
    decide_fallback,
    delivery_policy,
    route_downgrade,
)
from gpustack_model_transfer.adapters.modelexpress.registration import (
    register,
    register_modelexpress_with_vllm,
    register_strategy_route,
    register_strategy_with_modelexpress,
    register_vllm_load_formats,
)
from gpustack_model_transfer.adapters.modelexpress.runtime import (
    EntryReadiness,
    install_modelexpress_runtime_patch,
    probe_entry,
)

_LAZY = {
    "STRATEGY_NAME": (
        "gpustack_model_transfer.adapters.modelexpress.plugin",
        "STRATEGY_NAME",
    ),
    "MAIN_ROLE": ("gpustack_model_transfer.adapters.modelexpress.plugin", "MAIN_ROLE"),
    "GpustackLoadStrategy": (
        "gpustack_model_transfer.adapters.modelexpress.plugin",
        "GpustackLoadStrategy",
    ),
    "PluginResult": (
        "gpustack_model_transfer.adapters.modelexpress.plugin",
        "PluginResult",
    ),
    "preserve_terminal_error": (
        "gpustack_model_transfer.adapters.modelexpress.plugin",
        "preserve_terminal_error",
    ),
    "register_plugin": (
        "gpustack_model_transfer.adapters.modelexpress.plugin",
        "register_plugin",
    ),
    "PublicationRefusedError": (
        "gpustack_model_transfer.adapters.modelexpress.publication",
        "PublicationRefusedError",
    ),
    "checkpoint_identity": (
        "gpustack_model_transfer.adapters.modelexpress.publication",
        "checkpoint_identity",
    ),
    "publication_allowed": (
        "gpustack_model_transfer.adapters.modelexpress.publication",
        "publication_allowed",
    ),
    "refuse_publication": (
        "gpustack_model_transfer.adapters.modelexpress.publication",
        "refuse_publication",
    ),
}


def __getattr__(name):
    target = _LAZY.get(name)
    if target is None:
        raise AttributeError(f"module {__name__!r} has no attribute {name!r}")
    import importlib

    value = getattr(importlib.import_module(target[0]), target[1])
    globals()[name] = value
    return value


def __dir__():
    return sorted(set(globals()) | set(_LAZY))


__all__ = [
    "BUILTIN_CHAIN",
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
    "CompositionError",
    "DeliveryPolicy",
    "EntryReadiness",
    "FallbackDecision",
    "GroupDisagreementError",
    "LoadGroupCoordinator",
    "PluginUnavailableError",
    "RankProposal",
    "StrategyRecoveryError",
    "WriterRefitFence",
    "apply_launch_env",
    "apply_offline_guards",
    "argv_requests_gpustack_modelexpress",
    "build_chain",
    "check_composition",
    "decide_fallback",
    "delivery_policy",
    "env_requests_gpustack_modelexpress",
    "filter_chain_strategies",
    "image_integration_metadata",
    "install_modelexpress_runtime_patch",
    "offline_guards_active",
    "place_gpustack_strategy",
    "probe_entry",
    "register",
    "register_modelexpress_with_vllm",
    "register_strategy_route",
    "register_strategy_with_modelexpress",
    "register_vllm_load_formats",
    "route_downgrade",
    "strategy_name",
]
# The strategy and publication names are deliberately absent from
# __all__: a star import resolves every __all__ entry through
# __getattr__, which would eagerly import the plugin and publication
# modules — and with the upstream package installed, torch with them —
# against this package's light-import promise. Plain attribute access
# and dir() still reach every lazy name.
