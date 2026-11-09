"""The ModelExpress model-load suite: the strategy path, fail-closed.

This suite exercises what the native model-load suite does — through the
ModelExpress entry: the strategy drives the qualified native engine
adapter through the topology-checked load entry, the delivery policy
shapes the chain, the terminal error survives the wrapper, the loading
group agrees, and a completed load publishes for Peer D2D while a
failed one never does. Without the ModelExpress entry on the host (the
package, the plugin hook, torch, CUDA) every capability is UNRUN: this
optional path fails closed, and a missing prerequisite is never a
skipped pass. The native and ModelExpress entry paths are tested
separately; one passing never qualifies the other.
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
    "publication-after-load",
    "accelerator-claims",
)


# The remote backends that may carry the ModelExpress entry. The
# entry's P2P plane rides NIXL; HIXL engines keep the model-transfer
# native transport through the same strategy.
_QUALIFIED_BACKENDS = {"nixl"}


def _backend_reason(backend) -> str:
    """'' when the requested backend may carry this entry, else UNRUN reason."""
    import importlib

    from gpustack_model_transfer.adapters.modelexpress.constants import PINNED_COMMIT

    if backend not in _QUALIFIED_BACKENDS:
        return (
            f"the ModelExpress entry's P2P plane is NIXL-qualified "
            f"({PINNED_COMMIT}); backend {backend!r} has no reviewed P2P pair"
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
    if metadata["insert-before"] != "server-cache":
        raise AssertionError("the insertion point moved out of the metadata")
    return (
        f"external strategy registered through {LOAD_PLUGIN_VARIABLE} at "
        f"{PINNED_COMMIT}"
    )


def _check_chain_composition(context, pair):
    from gpustack_model_transfer.adapters.modelexpress import (
        build_chain,
        check_composition,
    )

    # Peer D2D first, the GPUStack delivery second, the local strategies
    # behind them — under either spelling of the cache strategy's name.
    chain = check_composition(build_chain({"rdma": True, "server_cache": True}))
    if chain.index("rdma") > chain.index("gpustack"):
        raise AssertionError("Peer D2D does not run first")
    if chain.index("server-cache") < chain.index("gpustack"):
        raise AssertionError("a local fallback runs ahead of the delivery")
    chain = check_composition(build_chain({"rdma": True, "server-cache": True}))
    if chain.index("server-cache") < chain.index("gpustack"):
        raise AssertionError("a local fallback runs ahead of the delivery")
    return "Peer D2D, then the delivery, then the local fallbacks"


class _StrategyPair:
    """One check's strategy fixtures: the driver owns the model.

    The adapter double stands in for the pinned upstream engine adapter,
    the native loader stub records which entries ran, and the delivery
    policy is set for the duration of the check.
    """

    def __init__(self, pair, policy):
        import os
        from types import SimpleNamespace

        _, driver = pair
        self.driver = driver
        self.events = []
        self.model_calls = []
        self.adapter = SimpleNamespace(
            engine_name="vllm",
            vllm_config=SimpleNamespace(),
            load_config=SimpleNamespace(),
            model_config=SimpleNamespace(model="tiny"),
            finalize_ran=False,
        )
        self.adapter.after_native_load = self._after_native_load
        # A fixture model the consume hook can drive without a torch
        # runtime: the record is what the checks read.
        self.model = SimpleNamespace(
            load_weights=lambda weights_iter: self.model_calls.append(
                "model.load_weights"
            )
        )
        self._native = self._stub_native_module()
        self._saved_env = dict(os.environ)
        os.environ["GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY"] = policy

    def _after_native_load(self, result):
        self.events.append("after_native_load")
        self.adapter.finalize_ran = True
        # The driver's finalization bookkeeping lives on the model, the
        # same record the real runtime driver reads.
        model = result.model
        model.finalization_count = getattr(model, "finalization_count", 0) + 1
        return result

    def _stub_native_module(self):
        import sys
        import types

        events = self.events

        class StubVllmLoader:
            probe_error = None
            preload_error = None
            write_error = None

            def __init__(self, load_config):
                self.load_config = load_config

            def download_model(self, model_config):
                events.append("probe")
                if StubVllmLoader.probe_error is not None:
                    raise StubVllmLoader.probe_error

            def load_initialized_model(
                self, model, vllm_config, model_config, *, consume_weights=None
            ):
                events.append("load_initialized_model")
                if consume_weights is None:
                    raise AssertionError("the topology-checked entry ran bare")
                if StubVllmLoader.preload_error is not None:
                    # Mirror the real native loader: a pre-write failure
                    # poisons the process itself, so the strategy's
                    # restore path is what must clear it.
                    module._process_unusable = True
                    raise StubVllmLoader.preload_error
                consume_weights(iter(()))
                if StubVllmLoader.write_error is not None:
                    raise StubVllmLoader.write_error

            def load_weights(self, model, model_config):
                events.append("load_weights")

        module = types.ModuleType("gpustack_model_transfer.adapters.vllm.native")
        module.NativeModelTransferLoader = StubVllmLoader
        module._process_unusable = False
        self._saved_module = sys.modules.get(module.__name__)
        sys.modules[module.__name__] = module
        self._stub_module = module
        self._stub = StubVllmLoader
        return module

    def close(self):
        import os
        import sys

        os.environ.clear()
        os.environ.update(self._saved_env)
        # Restore the prior sys.modules entry instead of popping: removing
        # an already-imported real module would re-execute its body on the
        # next import and discard its module-level state mid-suite.
        if self._saved_module is not None:
            sys.modules[self._stub_module.__name__] = self._saved_module
        else:
            sys.modules.pop(self._stub_module.__name__, None)


def _open_strategy(pair, policy):
    """The strategy, its context, and its fixtures for one check."""
    from types import SimpleNamespace

    from gpustack_model_transfer.adapters.modelexpress.plugin import (
        GpustackLoadStrategy,
    )

    fixtures = _StrategyPair(pair, policy)
    try:
        strategy = GpustackLoadStrategy()
        ctx = SimpleNamespace(
            adapter=fixtures.adapter,
            result=SimpleNamespace(model=fixtures.model),
            p2p_enabled=True,
            nixl_manager=None,
        )
        return strategy, ctx, fixtures
    except Exception:
        # The policy variable the fixture wrote must not outlive a failed
        # construction: a later check would silently run under the leak.
        fixtures.close()
        raise


def _check_native_adapter_exactly_once(context, pair):
    from gpustack_model_transfer.core.errors import DeniedContentError

    strategy, ctx, fixtures = _open_strategy(pair, "require")
    try:
        fixtures._stub.probe_error = None
        result = strategy.load(ctx.result, ctx)
        if result.model is None:
            raise AssertionError("no model was loaded")
        if not fixtures.adapter.finalize_ran:
            raise AssertionError("the engine adapter did not finalize exactly once")
        driver = fixtures.driver
        if driver.model_finalization_count(result.model) != 1:
            raise AssertionError("finalization did not run exactly once")
        if fixtures.events != ["probe", "load_initialized_model", "after_native_load"]:
            raise AssertionError(f"the load path ran {fixtures.events}")
        # A write-time failure reports mutation exactly once. The raised
        # class is the upstream StrategyFailed when modelexpress is
        # importable and the local fallback otherwise, so the check
        # classifies by the mutated flag rather than the class identity.
        fixtures._stub.write_error = DeniedContentError("engine refused mid-load")
        fixtures.adapter.finalize_ran = False
        raised = False
        try:
            strategy.load(ctx.result, ctx)
        except Exception as exc:
            if getattr(exc, "mutated", None) is None:
                raise AssertionError(
                    f"the load terminated with {type(exc).__name__!r}, "
                    "not the chain's expected miss"
                ) from exc
            raised = True
            if not exc.mutated:
                raise AssertionError(
                    "a write-time failure reported no mutation"
                ) from exc
        if not raised:
            raise AssertionError("a write-time failure did not terminate the load")
    finally:
        fixtures.close()
    return "tensor application and postprocessing ran through the native adapter once"


def _check_require_terminal(context, pair):
    from gpustack_model_transfer.core.errors import SourceUnavailableError

    strategy, ctx, fixtures = _open_strategy(pair, "require")
    try:
        fixtures._stub.probe_error = SourceUnavailableError("no ready source")
        try:
            strategy.load(ctx.result, ctx)
        except Exception as exc:
            if getattr(exc, "mutated", None) is None:
                raise AssertionError(
                    f"the load terminated with {type(exc).__name__!r}, "
                    "not the chain's expected miss"
                ) from exc
            if exc.mutated:
                raise AssertionError("a probe miss reported mutation") from exc
            return "Require's terminal miss carries no mutation"
        raise AssertionError("Require fell through without the expected miss")
    finally:
        fixtures.close()


def _check_prefer_downgrade(context, pair):
    from gpustack_model_transfer.core.errors import SourceUnavailableError

    strategy, ctx, fixtures = _open_strategy(pair, "prefer")
    try:
        # The miss happens after the probe but before the first tensor
        # write, so the process-usability restore path genuinely runs and
        # the poison assertion below is exercisable.
        fixtures._stub.probe_error = None
        fixtures._stub.preload_error = SourceUnavailableError(
            "the source vanished after the probe"
        )
        try:
            strategy.load(ctx.result, ctx)
        except Exception as exc:
            if getattr(exc, "mutated", None) is None:
                raise AssertionError(
                    f"the load terminated with {type(exc).__name__!r}, "
                    "not the chain's expected miss"
                ) from exc
            if exc.mutated:
                raise AssertionError("a pre-mutation miss reported mutation") from exc
            if fixtures._native._process_unusable:
                raise AssertionError(
                    "a pre-mutation miss poisoned the process"
                ) from exc
            return "the permitted downgrade is a clean miss the chain routes"
        raise AssertionError("Prefer did not report the expected miss")
    finally:
        fixtures.close()


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
        return (
            "the group agrees on backend and strategy, and refuses "
            "disagreement; TP-group coordination belongs to the native "
            "load path"
        )
    raise AssertionError("a disagreeing group was allowed to proceed")


def _check_publication_after_load(context, pair):
    from gpustack_model_transfer.adapters.modelexpress import (
        PluginResult,
        PublicationRefusedError,
        WriterRefitFence,
        publication_allowed,
        refuse_publication,
    )

    strategy, ctx, fixtures = _open_strategy(pair, "prefer")
    try:
        result = strategy.load(ctx.result, ctx)
        # Peer publication authorized, no writer active: the fence's
        # publishable state, so the completed load is what the gate reads.
        fence = WriterRefitFence(allow_peer_publication=True)
        if not result.publishable:
            raise AssertionError("a completed strategy load is not publishable")
        if not publication_allowed(
            PluginResult(model=result.model, publishable=result.publishable), fence
        ):
            raise AssertionError("a completed load was refused publication")
        try:
            refuse_publication(PluginResult(model=result.model), fence)
        except PublicationRefusedError:
            return "a completed load publishes; an unloaded result never does"
        raise AssertionError("an unloaded result was publishable")
    finally:
        fixtures.close()


def _check_accelerator_claims(context, pair):
    from gpustack_model_transfer.adapters.modelexpress import (
        QUALIFIED_ACCELERATORS,
        image_integration_metadata,
    )

    metadata = image_integration_metadata()
    if metadata["accelerators"] != ",".join(QUALIFIED_ACCELERATORS):
        raise AssertionError("the qualification record moved out of the metadata")
    return (
        "the entry records its qualification targets; the native transport "
        "follows the engine's backend"
    )


_CHECKS = (
    ("plugin-route", _check_plugin_route),
    ("chain-composition", _check_chain_composition),
    ("native-adapter-exactly-once", _check_native_adapter_exactly_once),
    ("require-terminal-fallback", _check_require_terminal),
    ("prefer-routed-downgrade", _check_prefer_downgrade),
    ("terminal-error-preservation", _check_terminal_preservation),
    ("loading-group-agreement", _check_group_agreement),
    ("publication-after-load", _check_publication_after_load),
    ("accelerator-claims", _check_accelerator_claims),
)


__all__ = ["CAPABILITIES", "run"]
