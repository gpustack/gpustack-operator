"""The ModelExpress model-load suite: the strategy path, fail-closed.

This suite exercises what the native model-load suite does — through the
ModelExpress entry: the plugin drives the qualified native engine
adapter, the composition rules hold, the terminal error survives the
wrapper, the group agrees, and the result is never publishable. Without
the ModelExpress entry on the host (the package, the plugin hook, torch,
CUDA) every capability is UNRUN: this optional path fails closed, and a
missing prerequisite is never a skipped pass. The native and
ModelExpress entry paths are tested separately; one passing never
qualifies the other.
"""

from gpustack_model_transfer.acceptance.report import CheckResult

CAPABILITIES = (
    "plugin-route",
    "chain-composition",
    "native-adapter-exactly-once",
    "require-terminal-fallback",
    "prefer-routed-downgrade",
    "terminal-error-preservation",
    "loading-group-agreement",
    "non-publication",
    "accelerator-claims",
)


# The remote backends that may carry the ModelExpress entry. The
# reviewed entry is a CUDA qualification target: it provides no CANN
# accelerator adapter, so hixl is refused rather than audited.
_QUALIFIED_BACKENDS = {"nixl"}


def _backend_reason(backend) -> str:
    """'' when the requested backend may carry this entry, else UNRUN reason."""
    import importlib

    from gpustack_model_transfer.adapters.modelexpress.constants import PINNED_COMMIT

    if backend not in _QUALIFIED_BACKENDS:
        return (
            f"the ModelExpress entry is a CUDA qualification target "
            f"({PINNED_COMMIT}); backend {backend!r} is not a qualified pair"
        )
    try:
        module = importlib.import_module(
            f"gpustack_model_transfer.backends.{backend}.runtime"
        )
        readiness = module.probe_runtime()
    except Exception as exc:
        return f"the {backend} runtime probe failed: {exc}"
    if readiness.missing:
        return f"missing backend prerequisites ({backend}): " + ", ".join(
            readiness.missing
        )
    return ""


def run(context, runtime=None, backend=None) -> list:
    """Run every capability for the ModelExpress entry path.

    runtime, when given, is a zero-argument factory producing a fresh
    (loader, driver) pair per capability — the reference pair in tests;
    an injected runtime always keeps the existing behavior. Without it
    the host entry is probed and, under a requested remote backend, the
    backend runtime is probed too: a missing piece on either side, or an
    unqualified (backend, entry) pair such as hixl, UNRUNs every
    capability. The current default driver probe deliberately returns UNRUN:
    the portable Torch driver is a reference fixture, not a native ModelExpress
    qualification driver. Injected runtimes exercise these checks in tests;
    enabling a real-host route requires a qualified driver and failure hooks.
    """
    if runtime is None:
        from gpustack_model_transfer.adapters.modelexpress.runtime import probe_entry

        if backend is None:
            backend = context["config"]["backend"]
        reasons = []
        backend_reason = "" if backend == "local" else _backend_reason(backend)
        if backend_reason:
            reasons.append(backend_reason)
        readiness = probe_entry()
        if readiness.missing:
            reasons.append("entry: " + ", ".join(readiness.missing))
        if reasons:
            return [
                CheckResult(
                    capability,
                    "UNRUN",
                    "missing prerequisites: " + "; ".join(reasons),
                )
                for capability in CAPABILITIES
            ]
        factory, reason = _real_host_factory(context)
        if factory is None:
            return [
                CheckResult(capability, "UNRUN", reason) for capability in CAPABILITIES
            ]
        runtime = factory

    results = []
    for capability, check in _CHECKS:
        try:
            pair = (
                None
                if check in (_check_plugin_route, _check_chain_composition)
                else runtime()
            )
        except Exception as exc:
            # A factory that cannot build its pair is a failed capability,
            # never a crash of the run.
            results.append(
                CheckResult(capability, "NO-GO", f"{type(exc).__name__}: {exc}")
            )
            continue
        results.append(_run_check(capability, check, context, pair))
    return results


def _real_host_factory(context):
    """Build the real-host (loader, driver) factory, or (None, reason).

    The loader is the vLLM ModelTransferModelLoader over the verified artifact
    reader — the same local reader every engine's model-load suite
    uses; the remote backend dimension is exercised by the backend and
    safety suites against their own runtimes. The driver is the real
    torch/vLLM one, freshly constructed per capability.
    """
    from gpustack_model_transfer.adapters.vllm.torch_runtime import probe_host_driver

    driver, reason = probe_host_driver()
    if driver is None:
        return None, reason

    from gpustack_model_transfer.adapters.vllm import ModelTransferModelLoader
    from gpustack_model_transfer.adapters.vllm.torch_runtime import TorchRuntimeDriver
    from gpustack_model_transfer.core.budgets import ReadBudgets
    from gpustack_model_transfer.core.capabilities import local_backend_capabilities
    from gpustack_model_transfer.core.local_reader import LocalFileReader

    artifact = context["config"]["artifact"]
    limits = context["config"]["limits"]

    def factory():
        reader = LocalFileReader(
            context["manifest_path"], expected_sha256=artifact["manifest_sha256"]
        )
        loader = ModelTransferModelLoader(
            reader=reader,
            capabilities=local_backend_capabilities(),
            budgets=ReadBudgets(
                window_bytes=limits["window_bytes"],
                max_inflight=limits["max_inflight"],
                owned_output_bytes=limits["owned_output_bytes"],
            ),
        )
        return loader, TorchRuntimeDriver()

    return factory, ""


def _run_check(capability, check, context, pair) -> CheckResult:
    """One capability's verdict: a failed check is NO-GO, never a skip."""
    try:
        detail = check(context, pair)
        return CheckResult(capability, "PASS", detail)
    except Exception as exc:
        return CheckResult(capability, "NO-GO", f"{type(exc).__name__}: {exc}")
    finally:
        if isinstance(pair, tuple) and pair:
            close = getattr(pair[0], "_close_reader", None)
            if callable(close):
                close()


def _clean_composition():
    from gpustack_model_transfer.adapters.modelexpress.chain import check_composition

    return check_composition({"p2p": False, "rdma": True, "server_cache": False})


def _open_plugin(pair, policy):
    from gpustack_model_transfer.adapters.modelexpress import GpustackLoadPlugin

    loader, driver = pair
    composition = {
        "enabled": {"p2p": False, "rdma": True, "server_cache": False},
        "authorized": {},
    }
    return GpustackLoadPlugin(loader, driver, composition, policy)


def _check_plugin_route(context, pair):
    import json

    from gpustack_model_transfer.adapters.modelexpress import (
        LOAD_PLUGIN_VARIABLE,
        PINNED_COMMIT,
        image_integration_metadata,
    )

    metadata = image_integration_metadata()
    json.loads(json.dumps(metadata))
    if metadata["upstream"] != PINNED_COMMIT:
        raise AssertionError("the pinned commit moved out of the metadata")
    if metadata["hook"] != LOAD_PLUGIN_VARIABLE:
        raise AssertionError("the hook moved out of the metadata")
    return (
        f"external strategy registered through {LOAD_PLUGIN_VARIABLE} at "
        f"{PINNED_COMMIT}"
    )


def _check_chain_composition(context, pair):
    from gpustack_model_transfer.adapters.modelexpress import check_composition
    from gpustack_model_transfer.adapters.modelexpress.chain import CompositionError

    chain = _clean_composition()
    if chain.index("gpustack") > chain.index("rdma"):
        raise AssertionError("the plugin is not inserted before rdma")
    # Every bypassing composition is refused, each with its cause named.
    for enabled in (
        {"p2p": True, "rdma": True, "server_cache": False},
        {"p2p": False, "rdma": True, "server_cache": True},
    ):
        try:
            check_composition(enabled)
        except CompositionError:
            continue
        raise AssertionError(f"the composition {enabled} was accepted")
    return "the composition refuses reachable earlier and later bypasses"


def _check_native_adapter_exactly_once(context, pair):
    plugin = _open_plugin(pair, "require")
    result = plugin.load(None)
    if result.model is None:
        raise AssertionError("no model was loaded")
    if not result.finalize_ran:
        raise AssertionError("the engine adapter did not finalize exactly once")
    driver = pair[1]
    if driver.model_finalization_count(result.model) != 1:
        raise AssertionError("finalization did not run exactly once")
    plugin.finalize(result)
    return "tensor application and postprocessing ran through the native adapter once"


def _check_require_terminal(context, pair):
    from gpustack_model_transfer.adapters.modelexpress import StrategyRecoveryError
    from gpustack_model_transfer.core.errors import DeniedContentError

    loader, driver = pair
    driver.fail_store_after = 0
    driver.store_error = DeniedContentError("engine refused mid-load")
    plugin = _open_plugin((loader, driver), "require")
    try:
        plugin.load(None)
    except StrategyRecoveryError as exc:
        return f"Require remote terminated the chain: {exc}"
    raise AssertionError("Require remote fell through to another strategy")


def _check_prefer_downgrade(context, pair):
    from gpustack_model_transfer.core.errors import DeniedContentError

    loader, driver = pair
    driver.fail_store_after = 0
    driver.store_error = DeniedContentError("engine refused pre-mutation")
    plugin = _open_plugin((loader, driver), "prefer")
    result = plugin.load(None)
    if result.routed_fallback != "verified-node-delivery":
        raise AssertionError(f"the downgrade routed through {result.routed_fallback!r}")
    return (
        "the permitted downgrade routed through the declared verified-delivery policy"
    )


def _check_terminal_preservation(context, pair):
    from gpustack_model_transfer.adapters.modelexpress import (
        StrategyRecoveryError,
        preserve_terminal_error,
    )

    @preserve_terminal_error
    def body():
        raise StrategyRecoveryError("forbidden fallback")

    try:
        body()
    except StrategyRecoveryError:
        pass
    except Exception as exc:
        raise AssertionError(
            f"the wrapper converted the terminal error: {type(exc).__name__}"
        ) from None
    else:
        raise AssertionError("the terminal error vanished")
    return "the plugin wrapper preserves the chain's terminal error"


def _check_group_agreement(context, pair):
    from gpustack_model_transfer.adapters.modelexpress import (
        STRATEGY_NAME,
        GroupDisagreementError,
        LoadGroupCoordinator,
        RankProposal,
    )

    coordinator = LoadGroupCoordinator(expected_size=2)
    coordinator.propose(RankProposal(0, True, STRATEGY_NAME))
    coordinator.propose(RankProposal(1, True, STRATEGY_NAME))
    coordinator.agree()
    coordinator = LoadGroupCoordinator(expected_size=2)
    coordinator.propose(RankProposal(0, True, STRATEGY_NAME))
    coordinator.propose(RankProposal(1, True, "native"))
    try:
        coordinator.agree()
    except GroupDisagreementError:
        return "the group agrees on backend and strategy, and refuses disagreement"
    raise AssertionError("a disagreeing group was allowed to proceed")


def _check_non_publication(context, pair):
    from gpustack_model_transfer.adapters.modelexpress import (
        PublicationRefusedError,
        WriterRefitFence,
        refuse_publication,
    )

    plugin = _open_plugin(pair, "require")
    result = plugin.load(None)
    fence = WriterRefitFence()
    try:
        refuse_publication(result, fence)
    except PublicationRefusedError:
        return "the loaded model is never an external Stage-2 source"
    raise AssertionError("a live loaded model was publishable")


def _check_accelerator_claims(context, pair):
    from gpustack_model_transfer.adapters.modelexpress import (
        accelerator_claimed,
        refuse_cann,
    )

    if not accelerator_claimed("cuda") or accelerator_claimed("cann"):
        raise AssertionError("the accelerator claim set is wrong")
    try:
        refuse_cann()
    except ValueError as exc:
        return f"CANN claims are refused: {exc}"
    raise AssertionError("a CANN claim was accepted")


_CHECKS = (
    ("plugin-route", _check_plugin_route),
    ("chain-composition", _check_chain_composition),
    ("native-adapter-exactly-once", _check_native_adapter_exactly_once),
    ("require-terminal-fallback", _check_require_terminal),
    ("prefer-routed-downgrade", _check_prefer_downgrade),
    ("terminal-error-preservation", _check_terminal_preservation),
    ("loading-group-agreement", _check_group_agreement),
    ("non-publication", _check_non_publication),
    ("accelerator-claims", _check_accelerator_claims),
)


__all__ = ["CAPABILITIES", "run"]
