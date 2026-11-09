"""Registration: the standard load formats and the strategy plugin.

Three registrations make `--load-format modelexpress` (and `mx`) work
on a GPUStack-managed engine:

1. The load formats are registered with the engine's loader registry,
   so selecting the format resolves the pinned upstream loader.
2. The runtime patch routes the upstream strategy chain through the
   GPUStack delivery-policy filter.
3. The GPUStack strategy is registered with the upstream plugin
   registry, so the chain's plugin hook can place it.

Every registration is idempotent and repeats safely across the callers
that fire in one process: the packaged bootstrap, the engine's
general-plugins entry, and the loader hooks. The general-plugins entry
is a no-op unless this process actually asked for the route, so a
plain `--load-format model-transfer` deployment never depends on any of
this. A registration failure is logged under Prefer, never raised into
engine startup; under Require it is re-raised, because a route that
failed to arm must not pretend to be enforcing. The vLLM load-format
registry is deliberately left out of the packaged bootstrap: importing
vllm.model_executor.model_loader at interpreter start would drag vLLM
and torch into processes that never use them (an SGLang engine, for
example), and every vLLM process arms it through vLLM's own
general_plugins entry instead.
"""

import logging

_LOG = logging.getLogger(__name__)

_MX_PLUGIN_MODULE = "modelexpress.plugins"
_PLUGIN_MODULE = "gpustack_model_transfer.adapters.modelexpress.plugin"


def _canonical_strategy_class():
    """The one strategy class object every caller of this process holds.

    The packaged bootstrap may have imported the strategy module before
    the upstream package was importable, leaving its base bound to the
    local stand-in. Rather than evicting the module from sys.modules —
    which would mint a second class object and break identity for every
    caller that already holds the first one — the existing class is
    rebound in place to the upstream base, and every later caller
    reuses that same object.
    """
    import importlib

    plugin_module = importlib.import_module(_PLUGIN_MODULE)
    cls = plugin_module.GpustackLoadStrategy
    bound = plugin_module._STRATEGY_BASE
    upstream = plugin_module._upstream_strategy_base()
    if upstream is not None and bound is not upstream:
        instantiated = getattr(cls, "_instantiations", 0)
        if instantiated:
            # Instances made against the stand-in base keep their old
            # MRO; only later constructions resolve through the upstream
            # base. That mixed state is worth naming, not hiding.
            _LOG.warning(
                "the GPUStack strategy was instantiated %d time(s) before "
                "the upstream base became importable; existing instances "
                "keep the stand-in behavior",
                instantiated,
            )
        try:
            cls.__bases__ = (upstream,)
        except TypeError as exc:
            raise TypeError(
                "the installed ModelExpress strategy base has an object "
                "layout incompatible with the packaged bootstrap's stand-in "
                f"({exc}); the pinned commit no longer matches this upstream "
                "package, so the route must be re-qualified"
            ) from exc
        plugin_module._STRATEGY_BASE = upstream
    return cls


def register_strategy_with_modelexpress() -> type:
    """Register the GPUStack strategy with the upstream plugin registry.

    Idempotent by identity: a repeated call finds the already-registered
    class object and returns it unchanged, so a caller can take the
    return value as the one registered class. A name already taken by a
    different strategy is refused: silently keeping someone else's
    strategy would let the chain place a foreign loader under the
    GPUStack name.
    """
    import importlib

    from gpustack_model_transfer.adapters.modelexpress.constants import PLUGIN_NAME

    plugins = importlib.import_module(_MX_PLUGIN_MODULE)
    register_plugin = getattr(plugins, "register_plugin", None)
    if not callable(register_plugin):
        raise TypeError("the ModelExpress plugin registry exposes no register_plugin")
    strategy_class = _canonical_strategy_class()
    registered = getattr(plugins, "plugins", {}).get(PLUGIN_NAME)
    if registered is not None:
        existing = registered if isinstance(registered, type) else type(registered)
        if existing is strategy_class:
            return strategy_class
        raise ValueError(
            f"Plugin {PLUGIN_NAME!r} is already registered by another strategy"
        )
    register_plugin(PLUGIN_NAME, strategy_class)
    return strategy_class


def register_vllm_load_formats() -> None:
    """Register the standard load formats with vLLM's loader registry.

    The registered loader is the pinned upstream MxModelLoader, which is
    a BaseModelLoader subclass as vLLM's registry requires; this route
    only makes sure the runtime patch and the strategy are in place
    before it runs. A format vLLM (or the pinned upstream) already
    registered is left alone.
    """
    import vllm.model_executor.model_loader as model_loader
    from vllm.model_executor.model_loader import register_model_loader

    from gpustack_model_transfer.adapters.modelexpress.constants import (
        LOAD_FORMAT_ALIASES,
    )
    from modelexpress.engines.vllm.loader import MxModelLoader

    registered = getattr(model_loader, "_LOAD_FORMAT_TO_MODEL_LOADER", None) or {}
    for load_format in LOAD_FORMAT_ALIASES:
        existing = registered.get(load_format)
        if existing is not None:
            if existing is not MxModelLoader:
                # A foreign loader on a GPUStack format name would run
                # outside the delivery-policy chain entirely; say so
                # instead of silently deferring to it.
                _LOG.warning(
                    "load format %r is already registered to %r, not the "
                    "pinned MxModelLoader",
                    load_format,
                    getattr(existing, "__name__", existing),
                )
            continue
        register_model_loader(load_format)(MxModelLoader)


def _modelexpress_available() -> bool:
    """Say whether the upstream package resolves, without importing it."""
    import importlib
    import importlib.util

    try:
        available = importlib.util.find_spec("modelexpress") is not None
    except (ImportError, ValueError):
        # A stubbed or namespace modelexpress module can carry no spec;
        # importing decides.
        try:
            importlib.import_module("modelexpress")
        except Exception:
            return False
        return True
    return available


def register_strategy_route() -> bool:
    """Arm the runtime patch and the strategy, without engine registries.

    The vLLM load-format registry step is deliberately excluded: the
    packaged bootstrap calls this at interpreter start in processes that
    may run SGLang, and importing vllm.model_executor.model_loader there
    would drag vLLM and torch into a process that never touches them.
    vLLM processes arm the registry through the general_plugins entry
    (register), which vLLM fires inside its own processes only.

    A failure resolves through the delivery policy here, so every caller
    of this route — the packaged bootstrap, the general_plugins entry,
    the engine loader constructions — carries the same contract without
    repeating the handler. Returns whether the route was armed; a False
    under Prefer means the caller should treat the route as absent.
    """
    import importlib

    if not _modelexpress_available():
        _LOG.debug("modelexpress is not installed; registration skipped")
        return False
    try:
        importlib.import_module(_MX_PLUGIN_MODULE)
        install_modelexpress_runtime_patch()
        register_strategy_with_modelexpress()
    except Exception as exc:
        _handle_registration_failure(exc)
        return False
    return True


def register() -> None:
    """The vllm.general_plugins entry: make the formats usable.

    Returns without touching anything unless this process actually asked
    for the route — the command line selected the ModelExpress format,
    or the plugin hook variable is set — so a plain `--load-format
    model-transfer` deployment keeps its previous behavior even when the
    wheel and the upstream package are both installed. Registration is
    idempotent. A failure is logged rather than raised under Prefer:
    vLLM propagates plugin exceptions into engine startup, and an
    advisory entry must never be a crash there. Under Require the same
    failure is re-raised instead: the policy exists to forbid the local
    fallback, and a process that skipped enforcement would reload
    straight from the hub as if the route were armed, so failing startup
    is the honest outcome. The vLLM-not-installed case stays quiet under
    either policy: the strategy is already registered, and only the
    registry step of a registry this process never uses was skipped.
    """
    from gpustack_model_transfer.modelexpress_route import requested

    if not requested():
        return
    if not register_strategy_route():
        # The strategy arm resolved its own failure by the policy (an
        # error log under Prefer, a raise under Require); without it the
        # vLLM registry step is pointless.
        return
    try:
        register_vllm_load_formats()
    except Exception as exc:
        if _missing_module(exc) == "vllm":
            # The upstream package is installed in a container without
            # vLLM (an SGLang engine, for example): the strategy already
            # registered and only the vLLM registry step was skipped, so
            # startup must not look like a failure.
            _LOG.debug(
                "vLLM is not installed; the ModelExpress load formats "
                "were not registered with it (%s)",
                type(exc).__name__,
            )
            _LOG.debug("ModelExpress registration partial completion", exc_info=True)
            return
        _handle_registration_failure(exc)


def _handle_registration_failure(exc: Exception) -> None:
    """Log one registration arm's failure; re-raise it under Require.

    Under Prefer the route is advisory and the engine must start, so the
    arm's failure is an error log. Under Require the arm is the delivery
    enforcement itself: swallowing the failure would let the chain fall
    through to the very strategies the policy forbids, so the failure
    propagates into engine startup.
    """
    from gpustack_model_transfer.adapters.modelexpress.policy import delivery_policy

    if not delivery_policy().allow_fallback:
        raise exc
    _LOG.error("ModelExpress registration unavailable (%s)", type(exc).__name__)
    _LOG.debug("ModelExpress registration failure", exc_info=True)


def _missing_module(exc: BaseException) -> str:
    """The module a ModuleNotFoundError in the failure chain names, if any."""
    seen = set()
    current: BaseException | None = exc
    while current is not None and id(current) not in seen:
        seen.add(id(current))
        if isinstance(current, ModuleNotFoundError):
            name = current.name or ""
            if not name:
                message = str(current)
                if "No module named" in message:
                    parts = message.split("'")
                    if len(parts) > 1:
                        name = parts[1]
            if name:
                return name.split(".")[0]
        current = current.__cause__ or current.__context__
    return ""


def register_modelexpress_with_vllm() -> None:
    """Ensure the formats, the patch, and the strategy are all active."""
    register()


def install_modelexpress_runtime_patch() -> None:
    """Re-exported so one registration import covers the whole setup."""
    from gpustack_model_transfer.adapters.modelexpress.runtime import (
        install_modelexpress_runtime_patch as install,
    )

    install()


__all__ = [
    "install_modelexpress_runtime_patch",
    "register",
    "register_modelexpress_with_vllm",
    "register_strategy_route",
    "register_strategy_with_modelexpress",
    "register_vllm_load_formats",
]
