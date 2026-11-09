"""The runtime probe and the chain runtime patch.

ModelExpress is optional: the adapter imports without it, a host learns
what is missing through the probe, and the strategy's native transport
follows whatever engine backend the process runs — the entry records
its qualification targets instead of refusing an accelerator family.
The runtime patch wires GPUStack's delivery policy into the upstream
strategy chain: the chain builds its strategy list and hands it to the
shared executor, so the patch filters that list once, before any
strategy runs.
"""

import importlib.util
import inspect
import logging
from dataclasses import dataclass

from gpustack_model_transfer.adapters.modelexpress.constants import (
    LOAD_PLUGIN_VARIABLE,
)


@dataclass(frozen=True)
class EntryReadiness:
    """What the host has for the ModelExpress entry path."""

    modelexpress_available: bool
    plugin_hook_exposed: bool
    torch_available: bool
    cuda_available: bool
    missing: tuple

    @property
    def ready(self) -> bool:
        return not self.missing


def probe_entry(
    env: dict | None = None, cuda_available: bool | None = None
) -> EntryReadiness:
    """Probe the entry path without importing ModelExpress.

    env is the process environment as a mapping (tests inject); the
    plugin hook is exposed when the pinned variable is set. cuda_available,
    when given, stands in for the device probe.
    """
    import os

    source = os.environ if env is None else env
    missing = []
    modelexpress_available = _importable("modelexpress")
    torch_available = _importable("torch")
    if cuda_available is None:
        cuda_available = torch_available and _cuda_available()
    if not modelexpress_available:
        missing.append("modelexpress")
    if not torch_available:
        missing.append("torch")
    if not cuda_available:
        missing.append("cuda")
    plugin_hook_exposed = bool(source.get(LOAD_PLUGIN_VARIABLE))
    if not plugin_hook_exposed:
        missing.append(LOAD_PLUGIN_VARIABLE)
    return EntryReadiness(
        modelexpress_available=modelexpress_available,
        plugin_hook_exposed=plugin_hook_exposed,
        torch_available=torch_available,
        cuda_available=bool(cuda_available),
        missing=tuple(missing),
    )


def _resolve_skip_by_policy(reason: str) -> None:
    """Resolve one patch skip — install-time or call-time — by the policy.

    Under Require the patch is the enforcement itself: an unwrapped or
    unfiltered executor would reach the local strategies the policy
    forbids, so the skip fails through the chain's terminal error
    instead of bypassing the policy. Under Prefer the patch is advisory
    and stays transparent, with the skip logged.
    """
    from gpustack_model_transfer.adapters.modelexpress.policy import (
        StrategyRecoveryError,
        delivery_policy,
    )

    if not delivery_policy().allow_fallback:
        raise StrategyRecoveryError(f"the delivery policy cannot be enforced: {reason}")
    logging.getLogger(__name__).warning(
        "the ModelExpress delivery policy was not applied (%s)", reason
    )


def install_modelexpress_runtime_patch() -> None:
    """Route the upstream strategy chain through the delivery-policy filter.

    The upstream chain resolves its executor through module globals at
    call time, so wrapping the executor applies the filter to
    LoadStrategyChain.run and to direct executor calls alike. Idempotent:
    the wrapper marks itself and a second install is a no-op. An
    installation-time skip — the chain module or its executor missing —
    resolves through the delivery policy exactly like a call-time skip:
    under Require the arm fails loudly rather than leave the route
    unfiltered, under Prefer it is a warning. The rebind is effective
    because the pinned upstream resolves its executor through module
    globals at call time; a caller that bound the executor with a
    from-import before installing would bypass the filter, and the
    pinned upstream never does — installs must precede any engine
    import that would from-import it.
    """
    from gpustack_model_transfer.adapters.modelexpress.chain import (
        filter_chain_strategies,
    )

    try:
        module = importlib.import_module("modelexpress.load_strategy")
    except Exception as exc:
        _resolve_skip_by_policy(
            f"the ModelExpress strategy chain is unavailable ({type(exc).__name__})"
        )
        return
    executor = getattr(module, "execute_load_strategies", None)
    if not callable(executor):
        _resolve_skip_by_policy("the ModelExpress chain executor is not callable")
        return
    if getattr(executor, "_gpustack_model_transfer_filtered", False):
        return

    # The executor is fixed once installed: probe its signature once and
    # cache the strategies position instead of re-inspecting it on every
    # filtered call — this sits directly on the model-load path.
    try:
        parameters = list(inspect.signature(executor).parameters)
        strategies_position = (
            parameters.index("strategies") if "strategies" in parameters else None
        )
    except (TypeError, ValueError):
        strategies_position = None

    def _filtered_execute_load_strategies(*args, **kwargs):
        # Forward generically: the pinned upstream calls this as
        # (model, ctx, strategies). When the strategies argument cannot
        # be identified, the delivery policy decides — Prefer passes the
        # chain through untouched, and Require fails the load instead of
        # silently reaching the strategies the policy forbids.
        if "strategies" in kwargs:
            kwargs["strategies"] = filter_chain_strategies(kwargs["strategies"])
            return executor(*args, **kwargs)
        if strategies_position is None:
            _resolve_skip_by_policy(
                "the executor's strategies argument could not be identified"
            )
            return executor(*args, **kwargs)
        if strategies_position >= len(args):
            _resolve_skip_by_policy("the executor was called without its strategies")
            return executor(*args, **kwargs)
        args = (
            *args[:strategies_position],
            filter_chain_strategies(args[strategies_position]),
            *args[strategies_position + 1 :],
        )
        return executor(*args, **kwargs)

    _filtered_execute_load_strategies._gpustack_model_transfer_filtered = True
    module.execute_load_strategies = _filtered_execute_load_strategies


def _importable(name: str) -> bool:
    try:
        return importlib.util.find_spec(name) is not None
    except (ImportError, ValueError):
        return False


def _cuda_available() -> bool:
    try:
        import torch

        return bool(torch.cuda.is_available())
    except Exception:
        return False


__all__ = [
    "EntryReadiness",
    "install_modelexpress_runtime_patch",
    "probe_entry",
]
