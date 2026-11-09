"""The fallback policy: Require terminates, Prefer downgrades honestly.

ModelExpress's own chain treats a strategy failure as recoverable and
falls through to the next strategy. GPUStack's policy must not: under
Require, a missing, unavailable, or skipped plugin is terminal — the
chain's own terminal error, StrategyRecoveryError, so no later strategy
runs and nothing downloads a full checkpoint. Under Prefer, a downgrade
is allowed only before the model was mutated, and only through GPUStack's
declared verified-delivery policy — never an unqualified server-cache,
P2P, or native path.

A failure after mutation is terminal under either policy: a partially
mutated model is not clean fallback state, and reporting it as one is
the lie this module exists to prevent.
"""

import logging
import os
from dataclasses import dataclass

from gpustack_model_transfer.adapters.modelexpress.constants import (
    DELIVERY_POLICY_VARIABLE,
    POLICY_PREFER,
    POLICY_REQUIRE,
)

_LOG = logging.getLogger(__name__)

# Where ModelExpress's own terminal error may live, in the order the
# pinned tree was reviewed against. The first module that defines a
# suitable StrategyRecoveryError wins.
_UPSTREAM_MODULES = (
    "modelexpress.adapter",
    "modelexpress.load_strategy",
    "modelexpress",
    "modelexpress.errors",
    "modelexpress.core.errors",
)

_RECOVERY_DOC = """The chain's terminal error: fallback is forbidden past this point.

ModelExpress defines this class; when the package is importable this
error IS ModelExpress's own (a dynamic subclass), so the chain's
except StrategyRecoveryError catches what this adapter raises and the
generic recoverable handler never sees it. Without the package, the
adapter defines an identical standalone one, and the plugin wrapper
preserves whichever it sees. A wrapper that converts it into an ordinary
recoverable failure reopens the forbidden fallback path.
"""


def _upstream_recovery_base():
    """ModelExpress's own StrategyRecoveryError, when importable."""
    import importlib

    for module_name in _UPSTREAM_MODULES:
        try:
            module = importlib.import_module(module_name)
        except Exception:
            continue
        base = getattr(module, "StrategyRecoveryError", None)
        if isinstance(base, type) and issubclass(base, BaseException):
            return base
    return None


_RECOVERY_BASE = _upstream_recovery_base()

if _RECOVERY_BASE is None:

    class StrategyRecoveryError(Exception):
        pass

    StrategyRecoveryError.__doc__ = _RECOVERY_DOC
else:
    StrategyRecoveryError = type(
        "StrategyRecoveryError",
        (_RECOVERY_BASE,),
        {"__doc__": _RECOVERY_DOC, "__module__": __name__},
    )


class PluginUnavailableError(Exception):
    """The GPUStack plugin is missing, unavailable, or was skipped."""


@dataclass(frozen=True)
class FallbackDecision:
    """What the policy says about one failure.

    terminal is True when the failure must halt the chain through the
    terminal error. routed names the GPUStack declared delivery path a
    permitted downgrade must take; an empty routing means no fallback is
    permitted at all.
    """

    terminal: bool
    reason: str
    routed: str = ""


def decide_fallback(
    policy: str,
    *,
    mutated: bool,
    plugin_present: bool,
    routed: str = "verified-node-delivery",
) -> FallbackDecision:
    """Decide one failure's outcome under the cluster policy.

    mutated is the engine model's taint state at the failure: a failure
    before any parameter store ran leaves clean state a declared
    fallback may replace; a failure after mutation does not, under
    either policy.
    """
    if not plugin_present:
        if policy == POLICY_REQUIRE:
            return FallbackDecision(
                terminal=True,
                reason="the GPUStack plugin is missing or was skipped, and "
                "Require remote terminates before another strategy runs",
            )
        return FallbackDecision(
            terminal=False,
            reason="the GPUStack plugin is missing; Prefer remote permits a "
            "declared fallback",
            routed=routed,
        )
    if mutated:
        return FallbackDecision(
            terminal=True,
            reason="the model was partially mutated: a partially changed "
            "model is not clean fallback state",
        )
    if policy == POLICY_REQUIRE:
        return FallbackDecision(
            terminal=True,
            reason="the GPUStack plugin failed under Require remote; the "
            "chain terminates rather than fall through",
        )
    return FallbackDecision(
        terminal=False,
        reason="the GPUStack plugin failed before mutation; Prefer remote "
        "permits a declared fallback",
        routed=routed,
    )


def route_downgrade(decision: FallbackDecision) -> str:
    """The delivery path a permitted downgrade must take.

    An unqualified server-cache, P2P, or native path is never the
    answer: the only permitted downgrade routes through GPUStack's
    declared verified-delivery policy, and a decision without a routing
    has no permitted downgrade at all.
    """
    if decision.terminal:
        raise StrategyRecoveryError(f"fallback is forbidden: {decision.reason}")
    if not decision.routed:
        raise StrategyRecoveryError(
            "no declared delivery route was authorized for the downgrade"
        )
    return decision.routed


class WriterRefitFence:
    """The boundary between checkpoint identity and live generations.

    A raw checkpoint's identity is its verified manifest digest; a live
    processed-tensor generation is a mutable runtime thing with a writer
    and a refit state. Registering a live generation as though it were
    checkpoint content would leak one tenant's processed tensors into
    another's identity space, so the fence keeps the two apart: live
    generations are never publishable, and a refit must complete under
    the writer's identity before anything downstream may read.
    """

    def __init__(self, *, allow_peer_publication: bool = False):
        self._writer = None
        self._refitting = False
        self._allow_peer_publication = bool(allow_peer_publication)

    def begin_write(self, writer: str) -> None:
        if self._writer is not None:
            raise StrategyRecoveryError(
                f"a writer is already active ({self._writer!r}); live "
                "processed-tensor generations have one writer at a time"
            )
        self._writer = writer

    def begin_refit(self, writer: str) -> None:
        self.begin_write(writer)
        self._refitting = True

    def finish(self, writer: str) -> None:
        if self._writer != writer:
            raise StrategyRecoveryError(
                f"only the active writer may finish ({self._writer!r})"
            )
        self._writer = None
        self._refitting = False

    def readable(self) -> bool:
        return self._writer is None and not self._refitting

    def publishable(self) -> bool:
        # External Stage-2 publication is disabled by default; when peer
        # replica P2P is explicitly authorized for a ModelDeployment,
        # publication is allowed once the active write/refit has finished.
        return self._allow_peer_publication and self.readable()


@dataclass(frozen=True)
class DeliveryPolicy:
    """What the cluster's delivery policy permits the load chain to do.

    allow_fallback is False under Require: the chain may use Peer D2D
    (rdma) and the GPUStack source delivery (gpustack) only, and a
    failure of both is a terminal load failure. allow_fallback is True
    under Prefer: the built-in local strategies stay reachable, so a
    seed replica without a ready peer or ready source loads from the
    local cache and then publishes itself as a source.
    """

    allow_fallback: bool = True

    @property
    def name(self) -> str:
        return POLICY_PREFER if self.allow_fallback else POLICY_REQUIRE


def delivery_policy(env: dict | None = None) -> DeliveryPolicy:
    """Read the delivery policy from the environment.

    Only "Require" (case-insensitive) closes the fallback path by name;
    every other value, including an unset variable, opens it. An
    unrecognized explicit value fails closed instead: the operator set
    the variable to something, and the value that most plausibly
    intended — forbidding the local fallbacks that download full
    checkpoints past GPUStack's verified source identity — is the
    strictest reading, so the warning names the value and the accepted
    spellings and the chain runs as Require until the spelling is
    fixed. The route-request gate keeps that strictness off any process
    that never asked for the route at all.
    """
    source = os.environ if env is None else env
    raw = str(source.get(DELIVERY_POLICY_VARIABLE, "") or "").strip().lower()
    if raw == POLICY_REQUIRE:
        return DeliveryPolicy(allow_fallback=False)
    if raw == POLICY_PREFER:
        return DeliveryPolicy(allow_fallback=True)
    if raw:
        _LOG.warning(
            "unrecognized %s=%r; expected %r or %r; failing closed as %r",
            DELIVERY_POLICY_VARIABLE,
            raw,
            POLICY_REQUIRE,
            POLICY_PREFER,
            POLICY_REQUIRE,
        )
        return DeliveryPolicy(allow_fallback=False)
    return DeliveryPolicy(allow_fallback=True)


__all__ = [
    "DeliveryPolicy",
    "FallbackDecision",
    "PluginUnavailableError",
    "StrategyRecoveryError",
    "WriterRefitFence",
    "decide_fallback",
    "delivery_policy",
    "route_downgrade",
]
