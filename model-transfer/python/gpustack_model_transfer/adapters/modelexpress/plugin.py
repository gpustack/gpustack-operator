"""The GPUStack load strategy for the ModelExpress chain.

The strategy follows the pinned upstream LoadStrategy ABI: availability
is checked without touching the model, a source-readiness probe runs
before any weight is applied, and a failure reports whether the model
was mutated so the chain knows whether it may move on or must reinit.

Streaming reloads and speculative draft passes are refused outright:
reload post-processing would run twice around this strategy's hooks, and
a draft pass answers for the main artifact's digest, so loading through
this strategy would deliver the wrong weights into the draft model.

The probe is the contract that makes the three-tier chain work: when no
ModelPrefetch source is ready, `download_model` fails without having
mutated anything, the strategy raises the chain's expected miss with
mutated=False, and under Prefer the chain continues to the local cache
strategy cleanly. Under Require the fallback strategies were stripped
ahead of time, so the same miss terminates the load.

Failures are classified by whether tensor writes had started. A failure
before the first write — topology refused, coordination lost, the
source vanishing between probe and plan — is reported with mutated=False
and, under Prefer, leaves the process's native load path clean. Once
writes started the model is tainted: the strategy reports mutated=True,
and under Require it also marks the native vLLM process unusable rather
than let anything reuse the half-loaded model.

Tensor-parallel coordination belongs to the native load path:
`download_model` broadcasts the probe across the TP group and
`load_initialized_model` coordinates the group before the first write.
Upstream RdmaStrategy is evaluated per rank before this strategy is
entered, and the pinned upstream has no cross-rank agreement between
strategies — a rank whose Peer D2D attempt succeeded leaves the chain
while its peers fall through to this one. That divergence is a known
limitation inherited from the pinned upstream, and this strategy adds
no collective of its own to paper over it.
"""

import logging
from abc import ABC
from contextlib import nullcontext, suppress
from dataclasses import dataclass

from gpustack_model_transfer.adapters.modelexpress.chain import (
    offline_guards_active,
)
from gpustack_model_transfer.adapters.modelexpress.constants import (
    OFFLINE_GUARDS,
    PLUGIN_NAME,
)
from gpustack_model_transfer.adapters.modelexpress.policy import (
    DeliveryPolicy,
    delivery_policy,
)

_LOG = logging.getLogger(__name__)

# The strategy name the chain knows this plugin by.
STRATEGY_NAME = PLUGIN_NAME

# The engines whose adapter this strategy can drive.
SUPPORTED_ENGINES = ("vllm", "sglang")

# The roles this strategy serves: the main load only. Speculative draft
# passes answer for a different artifact digest and load through their
# own path.
MAIN_ROLE = "main"


@dataclass
class PluginResult:
    """What one plugin load produced.

    The publication gate reads publishable from this record: a strategy
    that finished loading marks the result publishable so the chain
    registers its tensors and publishes them for Peer D2D; a result the
    strategy never completed stays unpublished.
    """

    model: object = None
    strategy: str = STRATEGY_NAME
    finalize_ran: bool = False
    routed_fallback: str = ""
    publishable: bool = False
    terminal_reason: str = ""

    def publication_allowed(self) -> bool:
        return self.publishable


def preserve_terminal_error(func):
    """The wrapper contract, as a decorator the tests can hold to.

    A terminal StrategyRecoveryError raised inside the wrapped callable
    re-raises unchanged; anything else is an ordinary recoverable
    failure the policy decides.
    """
    from gpustack_model_transfer.adapters.modelexpress.policy import (
        PluginUnavailableError,
        StrategyRecoveryError,
    )

    def wrapper(*args, **kwargs):
        try:
            return func(*args, **kwargs)
        except StrategyRecoveryError:
            raise
        except PluginUnavailableError as exc:
            raise StrategyRecoveryError(str(exc)) from exc

    wrapper.__name__ = getattr(func, "__name__", "wrapped")
    return wrapper


# ---------------------------------------------------------------------------
# Upstream ABI resolution: every binding works without modelexpress
# importable, so unit tests and import-time probes never pay for it.
# ---------------------------------------------------------------------------


def _upstream_strategy_base():
    """ModelExpress's own LoadStrategy base, when importable."""
    import importlib

    for module_name in (
        "modelexpress.load_strategy.base",
        "modelexpress.load_strategy",
        "modelexpress",
    ):
        try:
            module = importlib.import_module(module_name)
        except Exception:
            continue
        base = getattr(module, "LoadStrategy", None)
        if isinstance(base, type):
            return base
    return None


class _FallbackLoadStrategy(ABC):  # noqa: B024 — ABC lineage keeps the layout compatible
    """Local stand-in for the upstream LoadStrategy base.

    It carries the upstream defaults this strategy relies on: prepare
    and abort are no-ops for a cold load, and finalize runs post
    processing and then registers the loaded model for publication.
    Without the upstream module there is no NIXL registration to run,
    so the registration step is the adapter's own source finalization,
    guarded by the P2P publication state. The ABC lineage is deliberate:
    it keeps the object layout identical to the upstream base, so the
    registration can rebind this class in place when the upstream
    package becomes importable.
    """

    name = ""
    requires = ()

    def prepare(self, result, ctx) -> None:
        return None

    def post_process(self, result, ctx) -> None:
        return None

    def finalize(self, result, ctx) -> None:
        self.post_process(result, ctx)
        if not getattr(ctx, "p2p_enabled", False):
            return
        if not getattr(result, "publishable", False):
            return
        nixl_manager = getattr(ctx, "nixl_manager", None)
        if nixl_manager is None:
            return
        finalize_as_source = getattr(
            getattr(ctx, "adapter", None), "finalize_as_source", None
        )
        if callable(finalize_as_source):
            finalize_as_source(result, nixl_manager)

    def abort(self, result, ctx) -> None:
        return None

    def is_available(self, ctx) -> bool:
        return True

    def rollback(self, ctx) -> None:
        return None

    def load(self, result, ctx):
        raise NotImplementedError


_STRATEGY_BASE = _upstream_strategy_base()
_BASE = _STRATEGY_BASE if _STRATEGY_BASE is not None else _FallbackLoadStrategy


def _upstream_strategy_failed():
    """The chain's own expected-miss error, when importable.

    The class identity matters: the real chain catches its own
    StrategyFailed to distinguish a clean miss from a mutated failure,
    so a raise through a local stand-in would lose that distinction.
    """
    import importlib

    for module_name in (
        "modelexpress.adapter",
        "modelexpress.load_strategy",
        "modelexpress",
    ):
        try:
            module = importlib.import_module(module_name)
        except Exception:
            continue
        base = getattr(module, "StrategyFailed", None)
        if isinstance(base, type) and issubclass(base, BaseException):
            return base
    return None


class StrategyFailed(RuntimeError):
    """The expected-miss error used when upstream is not importable."""

    def __init__(self, message: str, *, mutated: bool = False):
        super().__init__(message)
        self.mutated = mutated


def strategy_failed(message: str, *, mutated: bool = False) -> Exception:
    """Build the chain's expected-miss error for one failure."""
    base = _upstream_strategy_failed()
    if base is None:
        base = StrategyFailed
    try:
        return base(message, mutated=mutated)
    except TypeError:
        pass
    try:
        # Keep the upstream class identity: the chain catches its own
        # StrategyFailed, so a local stand-in would escape uncaught and
        # turn a classified miss into an unhandled load failure.
        error = base(message)
    except TypeError:
        # No upstream signature is usable; the local class at least
        # carries the classified miss the chain's own loop never sees.
        return StrategyFailed(message, mutated=mutated)
    with suppress(AttributeError):
        # A slotted upstream class cannot carry the flag; the instance
        # is still the class the chain catches and routes by policy.
        error.mutated = mutated
    return error


def adapter_engine_name(adapter) -> str:
    """The engine an adapter drives.

    The pinned upstream adapter classes are the authority; their module
    path is the fallback for adapters re-exported from other packages.
    """
    import importlib

    explicit = str(getattr(adapter, "engine_name", "") or "").strip().lower()
    if explicit:
        return explicit
    for module_name, class_name, engine in (
        ("modelexpress.engines.vllm.adapter", "VllmAdapter", "vllm"),
        ("modelexpress.engines.sglang.adapter", "SglangAdapter", "sglang"),
    ):
        try:
            module = importlib.import_module(module_name)
        except Exception:
            continue
        base = getattr(module, class_name, None)
        if isinstance(base, type) and isinstance(adapter, base):
            return engine
    module = getattr(type(adapter), "__module__", "") or ""
    if ".engines.vllm." in module:
        return "vllm"
    if ".engines.sglang." in module:
        return "sglang"
    return ""


def _capture_tensor_attrs_factory():
    """The tensor-capture context factory for SGLang loads, when importable.

    The shared tensor utilities carry the capture helper this strategy
    wraps SGLang weight application in. When neither import location is
    present the load runs plain: publication simply discovers fewer
    tensors.
    """
    import importlib

    for module_name in (
        "modelexpress.tensor_utils",
        "modelexpress.engines.sglang.tensor_wrapper",
    ):
        try:
            module = importlib.import_module(module_name)
        except Exception:
            continue
        factory = getattr(module, "capture_tensor_attrs", None)
        if callable(factory):
            return factory
    return None


def _native_loader(engine: str, load_config):
    """The qualified native loader for one engine's load configuration."""
    if engine == "vllm":
        from gpustack_model_transfer.adapters.vllm.native import (
            NativeModelTransferLoader,
        )

        return NativeModelTransferLoader(load_config)
    if engine == "sglang":
        from gpustack_model_transfer.adapters.sglang.native import (
            NativeSGLangModelTransferLoader,
        )

        return NativeSGLangModelTransferLoader(load_config)
    # An adapter the chain never classified is a clean miss, so the
    # chain's mutated=False contract holds for it too.
    raise strategy_failed(f"unsupported engine {engine!r}", mutated=False)


def _native_process_module(engine: str):
    if engine == "vllm":
        from gpustack_model_transfer.adapters.vllm import native as vllm_native

        return vllm_native
    if engine == "sglang":
        from gpustack_model_transfer.adapters.sglang import native as sglang_native

        return sglang_native
    return None


def _native_process_unusable(engine: str) -> bool:
    """The native loader's poison flag; False when the engine is unknown."""
    module = _native_process_module(engine)
    return bool(getattr(module, "_process_unusable", False))


def _poison_native_process(engine: str) -> None:
    """Mark the native process unusable after a mutated failure.

    Both native loaders already set their own flag on any weight
    application failure; this is the strategy-side half of the same
    boundary for the terminal path. Under Require there is no fallback
    strategy left to reinit the model, so nothing may reuse this
    process's native loader against a half-applied model.
    """
    module = _native_process_module(engine)
    if module is not None:
        module._process_unusable = True


def _restore_native_process(engine: str, unusable_before: bool) -> None:
    """Undo a pre-write failure's native poison under Prefer.

    Nothing was written and no transfer reached a peer, so the refusal
    this attempt may have set is not earned: the flag goes back to the
    value the attempt found. An earlier attempt's poison predates this
    attempt and is not this strategy's to clear — a half-applied model
    must never be reused.
    """
    module = _native_process_module(engine)
    if module is not None:
        module._process_unusable = unusable_before


class GpustackLoadStrategy(_BASE):
    """Deliver verified source weights into a Peer-D2D-capable model.

    load() runs in phases, ordered by when the model becomes tainted.
    The reload and draft guards refuse loads this strategy must not
    serve. The probe resolves the source configuration and checks
    readiness without touching the model; the weight application then
    runs through the engine's own load path — the vLLM native entry
    that proves topology and coordinates the TP group first. A failure
    before the first tensor write is a clean miss (mutated=False); once
    writes started, the model is tainted and the failure is
    mutated=True. On success the result is marked publishable so the
    chain's finalize registers the tensors and publishes them to the
    ModelExpress metadata server.
    """

    name = STRATEGY_NAME
    _instantiations = 0

    def __init__(self):
        type(self)._instantiations += 1
        self._writes_started = False

    def _enforce_offline_guards(self, policy: DeliveryPolicy) -> None:
        """Hold the offline guards in the process env under Require.

        The guards are the second boundary behind the terminal error: a
        later strategy must not quietly download a full checkpoint even
        if the first boundary failed to halt the chain. They stay set
        after this load; restoring them would undo the boundary.
        """
        if policy.allow_fallback or offline_guards_active():
            return
        import os

        for guard in OFFLINE_GUARDS:
            name, _, value = guard.partition("=")
            os.environ[name] = value

    def is_available(self, ctx) -> bool:
        adapter = getattr(ctx, "adapter", None)
        if adapter is None:
            return False
        if adapter_engine_name(adapter) not in SUPPORTED_ENGINES:
            return False
        result = getattr(ctx, "result", None)
        if result is not None:
            return getattr(result, "model", None) is not None
        # A real ModelExpress LoadContext carries no result envelope at
        # availability time; the adapter's configuration pair is the
        # signal that this strategy could drive the load.
        load_config = getattr(adapter, "load_config", None)
        model_config = getattr(adapter, "model_config", None)
        return load_config is not None and model_config is not None

    def load(self, result, ctx):
        adapter = getattr(ctx, "adapter", None)
        engine = adapter_engine_name(adapter)
        policy: DeliveryPolicy = delivery_policy()
        if getattr(ctx, "is_reload", False):
            raise strategy_failed(
                "streaming reload loads through the engine's own reload path",
                mutated=False,
            )
        role = str(getattr(ctx, "p2p_role", MAIN_ROLE) or MAIN_ROLE)
        if role != MAIN_ROLE:
            raise strategy_failed(
                f"the {role!r} speculative draft pass answers for a different "
                "artifact digest and loads through its own strategy",
                mutated=False,
            )
        # The second Require boundary goes up before the first tensor
        # path can run: from here on, hub downloads stay impossible even
        # if a later failure escapes the terminal error.
        self._enforce_offline_guards(policy)
        loader = _native_loader(engine, getattr(adapter, "load_config", None))
        model_config = getattr(adapter, "model_config", None)
        try:
            # The probe resolves the source configuration and checks
            # readiness without touching the model or the process.
            loader.download_model(model_config)
        except Exception as exc:
            raise strategy_failed(str(exc), mutated=False) from exc
        self._writes_started = False
        process_unusable_before = _native_process_unusable(engine)
        try:
            self._apply_weights(loader, result, ctx, engine)
        except Exception as exc:
            if not self._writes_started:
                # Pre-plan failure: topology refused, coordination lost,
                # or the source vanished between probe and plan. Nothing
                # was written, so under Prefer the process keeps the
                # usability this attempt found it with.
                if policy.allow_fallback:
                    _restore_native_process(engine, process_unusable_before)
                raise strategy_failed(str(exc), mutated=False) from exc
            if not policy.allow_fallback:
                _poison_native_process(engine)
            raise strategy_failed(str(exc), mutated=True) from exc
        result.publishable = True
        result.strategy_name = self.name
        return result

    def _apply_weights(self, loader, result, ctx, engine: str) -> None:
        """Apply the weights and run the engine's post-load processing."""
        adapter = ctx.adapter
        model_config = adapter.model_config
        if engine == "vllm":
            self._load_vllm_weights(loader, result, adapter, model_config)
            return
        self._load_sglang_weights(loader, result, ctx, model_config)

    def _load_vllm_weights(self, loader, result, adapter, model_config) -> None:
        """Fill the initialized model through the topology-checked entry.

        The bare load_weights entry refuses a standalone reload because
        no topology proof or TP coordination ran; the initialized-model
        entry runs both before the first tensor write. The consume hook
        marks the write boundary the failure classifier reads.
        """
        self._writes_started = False

        def consume(weights_iter):
            self._writes_started = True
            return result.model.load_weights(weights_iter)

        loader.load_initialized_model(
            result.model,
            adapter.vllm_config,
            model_config,
            consume_weights=consume,
        )
        after_native_load = getattr(adapter, "after_native_load", None)
        if callable(after_native_load):
            after_native_load(result)

    def _load_sglang_weights(self, loader, result, ctx, model_config) -> None:
        """Stream the weights and run the engine's post-iteration hook.

        The write boundary is the engine's own load_weights callback, so
        the classifier wraps it for the duration of the load and then
        restores the lookup exactly as it found it: an attribute that
        lived on the instance is put back, and one resolved from the
        class is removed so class lookup resumes.
        """
        self._writes_started = False
        model = result.model
        original_load_weights = model.load_weights
        was_instance_attribute = "load_weights" in model.__dict__

        def wrapped_load_weights(weights_iter):
            self._writes_started = True
            return original_load_weights(weights_iter)

        try:
            # The patch is applied inside the try: a BaseException landing
            # between the assignment and entering the block would otherwise
            # leave the model bound to the stale wrapper for good.
            model.load_weights = wrapped_load_weights
            capture = None
            if getattr(ctx, "p2p_enabled", False):
                factory = _capture_tensor_attrs_factory()
                if factory is not None:
                    try:
                        capture = factory(getattr(ctx, "accelerator_backend", None))
                    except Exception as exc:
                        _LOG.debug(
                            "the tensor capture factory failed (%s)",
                            type(exc).__name__,
                        )
                        capture = None
            with capture if capture is not None else nullcontext():
                loader.load_weights(model, model_config)
            after_weight_iter_load = getattr(
                getattr(ctx, "adapter", None), "after_weight_iter_load", None
            )
            if callable(after_weight_iter_load):
                after_weight_iter_load(result)
        finally:
            if was_instance_attribute:
                model.load_weights = original_load_weights
            else:
                model.__dict__.pop("load_weights", None)


def register_plugin():
    """Return the concrete strategy class for MX_PLUGIN_DIR discovery.

    The live module's binding decides: re-imports after the upstream
    package became importable rebind their own copy of this module, and
    an entry surface that captured an older copy must still report the
    class callers of this process register.
    """
    import sys

    base = _STRATEGY_BASE
    live = sys.modules.get(__name__)
    if live is not None and getattr(live, "_STRATEGY_BASE", None) is not None:
        base = live._STRATEGY_BASE
    if base is None and live is not None:
        # A fresh import bound the stand-in when modelexpress was not yet
        # importable; the upstream base may be importable by now, and
        # this discovery route never runs the registration rebind.
        base = live._upstream_strategy_base()
        if base is not None:
            try:
                GpustackLoadStrategy.__bases__ = (base,)
            except TypeError:
                # Keep _STRATEGY_BASE on the stand-in: committing the
                # incompatible base here would make a second discovery
                # call skip the probe and silently return a class that
                # never followed the upstream ABI.
                base = None
            else:
                live._STRATEGY_BASE = base
    if base is None:
        from gpustack_model_transfer.adapters.modelexpress.policy import (
            PluginUnavailableError,
        )

        raise PluginUnavailableError("ModelExpress LoadStrategy is unavailable")
    return GpustackLoadStrategy


__all__ = [
    "MAIN_ROLE",
    "STRATEGY_NAME",
    "SUPPORTED_ENGINES",
    "GpustackLoadStrategy",
    "PluginResult",
    "StrategyFailed",
    "adapter_engine_name",
    "preserve_terminal_error",
    "register_plugin",
    "strategy_failed",
]
