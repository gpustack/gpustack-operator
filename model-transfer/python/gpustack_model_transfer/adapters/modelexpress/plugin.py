"""The GPUStack plugin: one strategy, driven by the native adapter.

The plugin is what a pinned image registers through the single-plugin
hook. Its load drives the qualified native engine adapter (the vLLM
ModelTransferModelLoader) exactly once: the reader session, the plan preflight,
the tensor application, and the engine's postprocessing all run through
that adapter, and this plugin never re-implements any of them.

The wrapper preserves the chain's terminal error. An ordinary failure of
the plugin's own work is recoverable exactly when the policy says so,
and never converts a terminal StrategyRecoveryError into a recoverable
one — that conversion is the bug that would let a later strategy
silently download a full checkpoint. The result is non-publishable:
finalize and publication cannot expose the loaded model as an external
Stage-2 P2P source implicitly.
"""

from dataclasses import dataclass

from gpustack_model_transfer.adapters.modelexpress.chain import (
    check_composition,
    offline_guards_active,
)
from gpustack_model_transfer.adapters.modelexpress.constants import (
    OFFLINE_GUARDS,
    POLICY_PREFER,
    POLICY_REQUIRE,
)
from gpustack_model_transfer.adapters.modelexpress.group import (
    GroupDisagreementError,
    RankProposal,
)
from gpustack_model_transfer.adapters.modelexpress.policy import (
    PluginUnavailableError,
    StrategyRecoveryError,
    WriterRefitFence,
    decide_fallback,
    route_downgrade,
)

# The strategy name the chain knows this plugin by.
STRATEGY_NAME = "gpustack"


@dataclass
class PluginResult:
    """What one plugin load produced.

    publishable is always False: a live processed-tensor model is never
    an external Stage-2 source, and the publication gate reads this
    field rather than trusting the caller not to publish.
    """

    model: object = None
    strategy: str = STRATEGY_NAME
    finalize_ran: bool = False
    routed_fallback: str = ""
    publishable: bool = False
    terminal_reason: str = ""

    def publication_allowed(self) -> bool:
        return self.publishable


class GpustackLoadPlugin:
    """The plugin a pinned image registers through the hook.

    loader is the qualified native engine adapter (an ModelTransferModelLoader);
    driver is its engine driver; composition describes the built-in
    chain this process actually runs; policy is the cluster's remote
    delivery policy. group, with rank, optionally attaches the loading
    group this rank belongs to: attached, every load is gated on the
    group's agreement before any parameter is mutated.
    """

    def __init__(
        self,
        loader,
        driver,
        composition: dict,
        policy: str,
        group=None,
        rank=None,
        *,
        publishable: bool = False,
    ):
        if policy not in (POLICY_REQUIRE, POLICY_PREFER):
            raise ValueError(f"unknown remote policy {policy!r}")
        if group is not None and rank is None:
            raise ValueError(
                "a loading group needs this rank's id; pass rank with group"
            )
        self._loader = loader
        self._driver = driver
        self._policy = policy
        self._group = group
        self._rank = rank
        self._publishable = bool(publishable)
        self._group_refusal = ""
        self.fence = WriterRefitFence(allow_peer_publication=self._publishable)
        self.chain = check_composition(
            composition.get("enabled", {}),
            composition.get("authorized", {}),
        )
        self.invocations = 0

    def load(self, options=None) -> PluginResult:
        """Run one load through the native adapter, exactly once per call.

        The exactly-once contract is per load: the adapter finalizes the
        model once, this plugin counts its own invocations, and the
        result records what happened truthfully. When a loading group
        is attached, every rank must agree on backend availability and
        the chosen strategy before any rank mutates a parameter. Under
        Require remote the offline guards hold the process environment
        as the second boundary behind the terminal error, and the
        writer fence spans exactly the adapter's own write.
        """
        self.invocations += 1
        if STRATEGY_NAME not in self.chain:
            return self._terminate("the GPUStack plugin is not in the composed chain")
        if not self._group_agrees():
            return self._group_refused()
        self._enforce_offline_guards()
        self.fence.begin_write(STRATEGY_NAME)
        try:
            report = self._loader.load(self._driver, options)
        except StrategyRecoveryError:
            # The terminal error passes through untouched: converting it
            # into a recoverable failure would reopen forbidden fallback.
            raise
        except PluginUnavailableError as exc:
            return self._terminate(str(exc))
        except Exception as exc:
            mutated = self._engine_mutated()
            decision = decide_fallback(
                self._policy,
                mutated=mutated,
                plugin_present=True,
            )
            if decision.terminal:
                raise StrategyRecoveryError(decision.reason) from exc
            routed = route_downgrade(decision)
            return PluginResult(
                strategy=STRATEGY_NAME,
                routed_fallback=routed,
            )
        finally:
            self.fence.finish(STRATEGY_NAME)
        return PluginResult(
            model=report.model,
            strategy=STRATEGY_NAME,
            finalize_ran=report.finalization_count == 1,
            publishable=self._publishable,
        )

    def finalize(self, result: PluginResult) -> PluginResult:
        """Finalize is the engine adapter's, already run exactly once."""
        if result.model is None:
            raise StrategyRecoveryError("no loaded model stands to finalize")
        if not result.finalize_ran:
            raise StrategyRecoveryError(
                "the engine adapter did not finalize exactly once"
            )
        return result

    def _terminate(self, reason: str) -> PluginResult:
        decision = decide_fallback(self._policy, mutated=False, plugin_present=False)
        if decision.terminal or self._policy == POLICY_REQUIRE:
            raise StrategyRecoveryError(reason)
        routed = route_downgrade(decision)
        return PluginResult(strategy=STRATEGY_NAME, routed_fallback=routed)

    def _engine_mutated(self) -> bool:
        """True once any parameter store ran, not merely once a model was
        built: building allocates storages without mutating a parameter,
        and a fallback decision hangs on the difference."""
        stores = getattr(self._driver, "stores_done", None)
        if stores is not None:
            return stores > 0
        model = getattr(self._driver, "last_model", None)
        if model is not None and getattr(model, "mutated", False):
            return True
        # Drivers that track neither counter still keep their built
        # models: any mutated model there is partial parameter mutation.
        models = getattr(self._driver, "_models", None)
        if isinstance(models, dict):
            return any(getattr(m, "mutated", False) for m in models.values())
        return False

    def _group_agrees(self) -> bool:
        """Gate the load on the loading group's agreement, if attached.

        Returns False when the group refused (the refusal stays on
        self._group_refusal for _group_refused to report); True when the
        group agreed or no coordinator is attached. A coordinator is
        scoped to one load: proposing into it twice is a refusal.
        """
        self._group_refusal = ""
        if self._group is None:
            return True
        try:
            self._group.propose(RankProposal(self._rank, True, STRATEGY_NAME))
            self._group.agree()
        except GroupDisagreementError as exc:
            self._group_refusal = str(exc)
            return False
        return True

    def _group_refused(self) -> PluginResult:
        """Report a refused group before any parameter was mutated."""
        decision = decide_fallback(self._policy, mutated=False, plugin_present=True)
        reason = f"the loading group did not agree: {self._group_refusal}"
        if decision.terminal:
            raise StrategyRecoveryError(reason)
        return PluginResult(
            strategy=STRATEGY_NAME, routed_fallback=route_downgrade(decision)
        )

    def _enforce_offline_guards(self) -> None:
        """Hold the offline guards in the process env under Require remote.

        The guards are the second boundary behind the terminal error: a
        later strategy must not quietly download a full checkpoint even
        if the first boundary failed to halt the chain. They stay set
        after this load; restoring them would undo the boundary.
        """
        if self._policy != POLICY_REQUIRE:
            return
        if offline_guards_active():
            return
        import os

        for guard in OFFLINE_GUARDS:
            name, _, value = guard.partition("=")
            os.environ[name] = value


def preserve_terminal_error(func):
    """The wrapper contract, as a decorator the tests can hold to.

    A terminal StrategyRecoveryError raised inside the wrapped callable
    re-raises unchanged; anything else is an ordinary recoverable
    failure the policy decides. The chain's own hook would do this; the
    pinned image's wrapper must, and this is the shape it must keep.
    """

    def wrapper(*args, **kwargs):
        try:
            return func(*args, **kwargs)
        except StrategyRecoveryError:
            raise
        except PluginUnavailableError as exc:
            raise StrategyRecoveryError(str(exc)) from exc

    wrapper.__name__ = getattr(func, "__name__", "wrapped")
    return wrapper


__all__ = [
    "STRATEGY_NAME",
    "GpustackLoadPlugin",
    "PluginResult",
    "preserve_terminal_error",
]
