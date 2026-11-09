"""Chain composition: plugin placement and the delivery-policy filter.

The pinned upstream builds its strategy chain in one fixed order and
inserts the single registered plugin at the configured point. GPUStack's
route asks the plugin to sit before the local fallback strategies, so
Peer D2D (rdma) runs first and the verified source delivery (gpustack)
second. What the route changes beyond placement is reachability: the
cluster's remote delivery policy decides whether the built-in local
strategies stay in the chain at all.

Under Require, only rdma and gpustack remain: a failure of both is a
terminal load failure, because a silent local fallback would deliver
weights that never passed GPUStack's source identity. Under Prefer, the
full chain stays: a seed replica without a ready peer or ready source
falls through to the local cache and then publishes itself as a source
for later replicas.
"""

import logging
import os

from gpustack_model_transfer.adapters.modelexpress.constants import (
    BUILTIN_CHAIN,
    FALLBACK_STRATEGIES,
    INSERT_BEFORE,
    OFFLINE_GUARDS,
    PLUGIN_NAME,
)
from gpustack_model_transfer.adapters.modelexpress.policy import (
    DeliveryPolicy,
    delivery_policy,
)
from gpustack_model_transfer.modelexpress_route import requested

_LOG = logging.getLogger(__name__)


class CompositionError(Exception):
    """A chain composition the route refuses, with the cause named."""


def strategy_name(strategy) -> str:
    """The chain name a strategy instance or class answers to."""
    return str(getattr(strategy, "name", "") or "")


def _enabled(enabled: dict, name: str) -> bool:
    """Say whether a built-in strategy is reachable under either spelling."""
    return bool(enabled.get(name) or enabled.get(name.replace("-", "_")))


def build_chain(enabled: dict) -> tuple:
    """Return the composed chain with the GPUStack plugin placed.

    enabled maps each built-in strategy to whether it is reachable in
    this process's configuration, keyed by the upstream strategy name
    or its underscore spelling. The plugin is inserted immediately
    before the configured insertion point; the returned tuple is the
    order the chain would run.
    """
    chain = []
    for name in BUILTIN_CHAIN:
        if name == INSERT_BEFORE:
            chain.append(PLUGIN_NAME)
        if _enabled(enabled, name):
            chain.append(name)
    return tuple(chain)


def check_composition(chain) -> tuple:
    """Validate one composed chain of strategy names; return it or raise.

    chain is the ordered strategy names the composition produced —
    build_chain's output, or any chain assembled from upstream strategy
    names. The invariant is placement: the plugin must run after Peer
    D2D (rdma) and before every local fallback strategy. A fallback
    strategy reachable ahead of the plugin would deliver weights that
    bypassed GPUStack's source identity, and an rdma strategy placed
    behind it would never be tried; either is refused with the cause
    named.
    """
    names = [str(getattr(item, "name", item)) for item in chain]
    if PLUGIN_NAME not in names:
        raise CompositionError(
            f"the {PLUGIN_NAME!r} plugin is not in the composed chain"
        )
    plugin_index = names.index(PLUGIN_NAME)
    for index, name in enumerate(names):
        if name == "rdma" and index > plugin_index:
            raise CompositionError(
                "the built-in 'rdma' strategy runs after the GPUStack "
                "plugin; Peer D2D must be tried first"
            )
        if name in FALLBACK_STRATEGIES and index < plugin_index:
            raise CompositionError(
                f"the built-in {name!r} strategy runs before the GPUStack "
                "plugin; local fallback must stay behind verified source "
                "delivery"
            )
    return tuple(names)


def apply_offline_guards(env: dict | None = None) -> dict:
    """Return the environment with the offline guards applied.

    A new mapping is returned; the caller's mapping and the process
    environment are untouched here. The guards are the second boundary
    behind the terminal error, not a replacement for it: hub download
    libraries check them before reaching the network, so a strategy the
    first boundary failed to halt cannot quietly fetch a full checkpoint
    past GPUStack's verified source identity.
    """
    target = dict(env if env is not None else os.environ)
    for guard in OFFLINE_GUARDS:
        name, _, value = guard.partition("=")
        target[name] = value
    return target


def offline_guards_active(env: dict | None = None) -> bool:
    """Say whether every offline guard is set in the environment."""
    source = os.environ if env is None else env
    for guard in OFFLINE_GUARDS:
        name, _, value = guard.partition("=")
        if source.get(name) != value:
            return False
    return True


def place_gpustack_strategy(strategies: list) -> list:
    """Return the chain with the GPUStack strategy inserted if missing.

    The plugin hook normally places the strategy; when the process was
    configured without the hook variable, the strategy would be absent
    and the chain would fall straight to the local strategies. This
    inserts a fresh strategy immediately before the first local
    fallback strategy, or ahead of an rdma strategy that arrives after
    everything else, so the ordering invariant still holds.
    """
    names = [strategy_name(strategy) for strategy in strategies]
    if PLUGIN_NAME in names:
        return strategies
    from gpustack_model_transfer.adapters.modelexpress.plugin import (
        GpustackLoadStrategy,
    )

    strategy = GpustackLoadStrategy()
    if "rdma" in names and names[0] != "rdma":
        # Peer D2D must be tried before any local strategy. A build that
        # composed rdma behind a fallback is normalized to the documented
        # order rather than inheriting the misordering, where the Require
        # strip would leave the plugin ahead of Peer D2D.
        rdma_position = names.index("rdma")
        strategies = [strategies[rdma_position]] + [
            item
            for position, item in enumerate(strategies)
            if position != rdma_position
        ]
        names = ["rdma"] + [name for name in names if name != "rdma"]
        _LOG.warning(
            "the upstream chain placed 'rdma' after local strategies; "
            "normalized to Peer D2D first"
        )
    index = len(strategies)
    for position, name in enumerate(names):
        if name in FALLBACK_STRATEGIES:
            index = position
            break
    else:
        for position, name in enumerate(names):
            if name == "rdma":
                index = position + 1
                break
    inserted = list(strategies)
    inserted.insert(index, strategy)
    return inserted


def filter_chain_strategies(strategies: list, policy: DeliveryPolicy | None = None):
    """Apply the GPUStack delivery policy to one upstream strategy chain.

    The plugin is guaranteed present, placed after rdma and before the
    local fallback strategies, whenever this process asked for the
    route — through the command line or the environment. A chain from a
    process that never asked for the plugin is returned unchanged under
    either policy: stripping the fallbacks without placing the plugin
    would leave a chain with no strategy able to serve the load. In a
    requested process under Require the fallback strategies are
    stripped, leaving rdma and gpustack only; under Prefer the full
    chain is kept.
    """
    current = policy if policy is not None else delivery_policy()
    strategies = list(strategies)
    asked = requested()
    if not asked:
        return strategies
    strategies = place_gpustack_strategy(strategies)
    if not current.allow_fallback:
        strategies = [
            strategy
            for strategy in strategies
            if strategy_name(strategy) in ("rdma", PLUGIN_NAME)
        ]
    # The chain this function composed must satisfy the placement
    # invariant; a live violation is an internal bug and fails loudly
    # rather than load weights through a misordered chain.
    check_composition([strategy_name(strategy) for strategy in strategies])
    return strategies


__all__ = [
    "CompositionError",
    "apply_offline_guards",
    "build_chain",
    "check_composition",
    "filter_chain_strategies",
    "offline_guards_active",
    "place_gpustack_strategy",
    "strategy_name",
]
