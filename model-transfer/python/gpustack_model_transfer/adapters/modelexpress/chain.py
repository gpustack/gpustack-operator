"""Chain composition: where the plugin sits, and what that permits.

The reviewed built-in chain cannot be reordered or removed through the
single-plugin hook, so correctness is a property of where the plugin is
inserted and of what remains reachable around it. A composition is valid
only when the plugin is inserted before "rdma", every reachable earlier
P2P strategy is disabled or explicitly authorized under GPUStack
identity, and every reachable later strategy preserves GPUStack identity,
authorization, and delivery constraints. Anything else is refused with
the strategy named.

The offline guards ride a valid composition as the second boundary: if
the terminal error fails to halt the chain, the environment still says
the hub is offline, so no later strategy can quietly download a full
checkpoint.
"""

import os

from gpustack_model_transfer.adapters.modelexpress.constants import (
    BUILTIN_CHAIN,
    EARLIER_STRATEGIES,
    INSERT_BEFORE,
    LATER_STRATEGIES,
    OFFLINE_GUARDS,
)


class CompositionError(Exception):
    """A chain composition the route refuses, with the cause named."""


def build_chain(enabled: dict) -> tuple:
    """Return the composed chain with the GPUStack plugin placed.

    enabled maps each built-in strategy to whether it is reachable in
    this process's configuration. The plugin is inserted immediately
    before "rdma"; the returned tuple is the order the chain would run.
    """
    plugin = "gpustack"
    chain = []
    inserted = False
    for name in BUILTIN_CHAIN:
        if name == INSERT_BEFORE and not inserted:
            chain.append(plugin)
            inserted = True
        if enabled.get(name, False):
            chain.append(name)
    if not inserted:
        # "rdma" missing from the chain: insert at the end, still before
        # nothing, which the later-strategy rules then judge.
        chain.append(plugin)
    return tuple(chain)


def authorize_earlier(strategy: str, authorized: dict) -> bool:
    """Say whether one reachable earlier strategy is permitted.

    An earlier P2P strategy is permitted only when it was explicitly
    authorized under GPUStack identity for this composition; disabled
    strategies are simply absent from the chain.
    """
    return bool(authorized.get(strategy, False))


def check_composition(enabled: dict, authorized: dict | None = None) -> tuple:
    """Validate one composition; return the composed chain or raise.

    enabled maps built-in strategy names to reachability in this
    process; authorized maps earlier-strategy names to an explicit
    GPUStack-identity authorization record. A reachable earlier strategy
    without one is a refusal, and so is any reachable later strategy:
    the reviewed later strategies (server_cache among them) bypass
    GPUStack delivery constraints, so their reachability under either
    policy is a composition the route refuses rather than audited.
    """
    authorized = authorized or {}
    for name in EARLIER_STRATEGIES:
        if enabled.get(name, False) and not authorize_earlier(name, authorized):
            raise CompositionError(
                f"the built-in {name!r} strategy is reachable before the "
                "GPUStack plugin and was not explicitly authorized under "
                "GPUStack identity; disable it or authorize it for this "
                "composition"
            )
    for name in LATER_STRATEGIES:
        if enabled.get(name, False):
            raise CompositionError(
                f"the built-in {name!r} strategy is reachable after the "
                "GPUStack plugin and does not preserve GPUStack identity "
                "and delivery constraints; disable it for this composition"
            )
    return build_chain(enabled)


def apply_offline_guards(env: dict | None = None) -> dict:
    """Return the environment with the offline guards applied.

    A new mapping is returned; the caller's mapping and the process
    environment are untouched here. The guards are the second boundary
    behind the terminal error, not a replacement for it.
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


__all__ = [
    "CompositionError",
    "apply_offline_guards",
    "authorize_earlier",
    "build_chain",
    "check_composition",
    "offline_guards_active",
]
