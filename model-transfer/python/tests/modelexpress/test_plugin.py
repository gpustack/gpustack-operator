"""The plugin: exactly-once through the native adapter, terminal honesty."""

import os
import unittest

import mxutil

from gpustack_model_transfer.adapters.modelexpress import (
    POLICY_PREFER,
    POLICY_REQUIRE,
    GpustackLoadPlugin,
    PluginResult,
    StrategyRecoveryError,
    preserve_terminal_error,
)
from gpustack_model_transfer.adapters.modelexpress.chain import CompositionError
from gpustack_model_transfer.adapters.vllm.runtime import ReferenceRuntime
from gpustack_model_transfer.core.budgets import ReadBudgets
from gpustack_model_transfer.core.capabilities import local_backend_capabilities
from gpustack_model_transfer.core.local_reader import LocalFileReader


class EnvSnapshotTestCase(unittest.TestCase):
    """Isolate the process environment: require-loads hold the offline
    guards in it, and one test's guards must not reach another test."""

    def setUp(self):
        self._saved_env = dict(os.environ)
        for name in ("HF_HUB_OFFLINE", "TRANSFORMERS_OFFLINE"):
            os.environ.pop(name, None)
        self.addCleanup(self._restore_env)

    def _restore_env(self):
        os.environ.clear()
        os.environ.update(self._saved_env)


def clean_composition():
    return {
        "enabled": {"p2p": False, "rdma": True, "server_cache": False},
        "authorized": {},
    }


def make_loader_driver(**driver_kwargs):
    from gpustack_model_transfer.adapters.vllm import ModelTransferModelLoader
    from gpustack_model_transfer.adapters.vllm.runtime import ReferenceRuntime

    reader = LocalFileReader(str(mxutil.manifest_path()))
    loader = ModelTransferModelLoader(
        reader=reader,
        capabilities=local_backend_capabilities(),
        budgets=ReadBudgets(window_bytes=32, max_inflight=2, owned_output_bytes=4096),
    )
    return loader, ReferenceRuntime(str(mxutil.fixture_dir()), **driver_kwargs)


def open_plugin(policy=POLICY_REQUIRE, composition=None, **driver_kwargs):
    loader, driver = make_loader_driver(**driver_kwargs)
    try:
        plugin = GpustackLoadPlugin(
            loader, driver, composition or clean_composition(), policy
        )
    except Exception:
        loader._close_reader()
        raise
    return plugin, loader, driver


class TestCompositionRefusal(unittest.TestCase):
    def test_a_bad_composition_is_refused_at_construction(self):
        with self.assertRaises(CompositionError):
            open_plugin(
                composition={
                    "enabled": {"p2p": True, "rdma": True, "server_cache": False},
                }
            )

    def test_an_unknown_policy_is_refused(self):
        with self.assertRaises(ValueError):
            open_plugin(policy="auto")


class TestRequireRemote(EnvSnapshotTestCase):
    def test_a_healthy_load_finalizes_once_and_is_non_publishable(self):
        plugin, _loader, driver = open_plugin(policy=POLICY_REQUIRE)
        result = plugin.load(None)
        self.assertIsNotNone(result.model)
        self.assertTrue(result.finalize_ran)
        self.assertFalse(result.publication_allowed())
        self.assertEqual(driver.model_finalization_count(result.model), 1)
        plugin.finalize(result)

    def test_the_native_adapter_runs_exactly_once_per_load(self):
        plugin, loader, _driver = open_plugin(policy=POLICY_REQUIRE)
        plugin.load(None)
        self.assertEqual(plugin.invocations, 1)
        # The engine driver's finalize count is the adapter's record: one.
        self.assertEqual(plugin.invocations, 1)
        _ = loader

    def test_a_missing_plugin_terminates_before_another_strategy(self):
        plugin = GpustackLoadPlugin.__new__(GpustackLoadPlugin)
        plugin._loader = None
        plugin._driver = None
        plugin._policy = POLICY_REQUIRE
        plugin.chain = ("rdma",)  # the plugin is absent from the chain
        plugin.fence = None
        plugin.invocations = 0
        with self.assertRaises(StrategyRecoveryError) as caught:
            plugin.load(None)
        self.assertIn("not in the composed chain", str(caught.exception))

    def test_a_mid_load_failure_terminates_under_require(self):
        from gpustack_model_transfer.core.errors import DeniedContentError

        loader, driver = make_loader_driver(
            fail_store_after=0, store_error=DeniedContentError("engine refused")
        )
        plugin = GpustackLoadPlugin(loader, driver, clean_composition(), POLICY_REQUIRE)
        with self.assertRaises(StrategyRecoveryError):
            plugin.load(None)


class TestPreferRemote(EnvSnapshotTestCase):
    def test_a_pre_mutation_failure_routes_the_declared_downgrade(self):
        from gpustack_model_transfer.core.errors import DeniedContentError

        loader, driver = make_loader_driver(
            fail_store_after=0, store_error=DeniedContentError("engine refused")
        )
        plugin = GpustackLoadPlugin(loader, driver, clean_composition(), POLICY_PREFER)
        result = plugin.load(None)
        self.assertIsNone(result.model)
        self.assertEqual(result.routed_fallback, "verified-node-delivery")
        self.assertFalse(result.publication_allowed())

    def test_a_post_mutation_failure_is_terminal_under_prefer(self):
        from gpustack_model_transfer.core.errors import DeniedContentError

        # Fails on the second store: the first store mutated the model.
        loader, driver = make_loader_driver(
            fail_store_after=1, store_error=DeniedContentError("late refusal")
        )
        plugin = GpustackLoadPlugin(loader, driver, clean_composition(), POLICY_PREFER)
        self.addCleanup(loader._close_reader)
        with self.assertRaises(StrategyRecoveryError) as caught:
            plugin.load(None)
        self.assertIn("partially mutated", str(caught.exception))


class TestTerminalErrorPreservation(unittest.TestCase):
    def test_the_wrapper_preserves_the_terminal_error(self):
        @preserve_terminal_error
        def strategy_body():
            raise StrategyRecoveryError("forbidden fallback")

        with self.assertRaises(StrategyRecoveryError) as caught:
            strategy_body()
        self.assertEqual(str(caught.exception), "forbidden fallback")

    def test_the_wrapper_does_not_convert_terminal_into_recoverable(self):
        @preserve_terminal_error
        def strategy_body():
            raise StrategyRecoveryError("terminal")

        recovered = False
        try:
            strategy_body()
        except StrategyRecoveryError:
            recovered = True  # the terminal error stayed terminal
        except Exception:
            recovered = False  # it became an ordinary recoverable failure
        self.assertTrue(recovered)

    def test_the_wrapper_keeps_ordinary_failures_ordinary(self):
        @preserve_terminal_error
        def strategy_body():
            raise RuntimeError("an ordinary engine failure")

        with self.assertRaises(RuntimeError):
            strategy_body()

    def test_a_plugin_load_that_raises_terminal_keeps_it_terminal(self):
        class TerminalLoader:
            def load(self, driver, options):
                raise StrategyRecoveryError("forbidden by policy")

        plugin = GpustackLoadPlugin(
            TerminalLoader(), None, clean_composition(), POLICY_PREFER
        )
        with self.assertRaises(StrategyRecoveryError):
            plugin.load(None)


class TestPluginResult(unittest.TestCase):
    def test_the_result_is_never_publishable(self):
        result = PluginResult(model=object())
        self.assertFalse(result.publication_allowed())


class CounterlessRuntime(ReferenceRuntime):
    """The pre-fix TorchRuntimeDriver shape, for the mutation contract.

    Models are tracked, but neither mutation counter is: stores strip
    the counters again, so the only honest mutation signal left is the
    tracked models' own mutated flag.
    """

    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self._models = {}

    def store(self, model, entry, payload, ownership=None):
        self._models[id(model)] = model
        result = ReferenceRuntime.store(self, model, entry, payload, ownership)
        self.__dict__.pop("stores_done", None)
        self.__dict__.pop("last_model", None)
        return result


class TestMutationDetection(EnvSnapshotTestCase):
    def test_a_counterless_driver_with_mutated_models_is_detected(self):
        # The review's scenario: a mid-load failure after partial
        # mutation on a driver that tracks neither counter. Prefer must
        # not downgrade over a partially mutated model.
        from gpustack_model_transfer.adapters.vllm import ModelTransferModelLoader
        from gpustack_model_transfer.core.budgets import ReadBudgets
        from gpustack_model_transfer.core.capabilities import local_backend_capabilities
        from gpustack_model_transfer.core.errors import DeniedContentError
        from gpustack_model_transfer.core.local_reader import LocalFileReader

        reader = LocalFileReader(str(mxutil.manifest_path()))
        loader = ModelTransferModelLoader(
            reader=reader,
            capabilities=local_backend_capabilities(),
            budgets=ReadBudgets(
                window_bytes=32, max_inflight=2, owned_output_bytes=4096
            ),
        )
        driver = CounterlessRuntime(str(mxutil.fixture_dir()))
        driver.fail_store_after = 1
        driver.store_error = DeniedContentError("late refusal")
        plugin = GpustackLoadPlugin(loader, driver, clean_composition(), POLICY_PREFER)
        self.addCleanup(loader._close_reader)
        with self.assertRaises(StrategyRecoveryError) as caught:
            plugin.load(None)
        self.assertIn("partially mutated", str(caught.exception))

    def test_a_counterless_driver_before_mutation_is_clean_state(self):
        driver = CounterlessRuntime(str(mxutil.fixture_dir()))
        plugin = GpustackLoadPlugin.__new__(GpustackLoadPlugin)
        plugin._driver = driver
        self.assertFalse(plugin._engine_mutated())

    def test_the_torch_driver_tracks_the_mutation_counters(self):
        from gpustack_model_transfer.adapters.vllm.torch_runtime import (
            TorchRuntimeDriver,
        )

        driver = TorchRuntimeDriver()
        self.assertEqual(driver.stores_done, 0)
        self.assertIsNone(driver.last_model)
        plugin = GpustackLoadPlugin.__new__(GpustackLoadPlugin)
        plugin._driver = driver
        self.assertFalse(plugin._engine_mutated())
        # The counter path is the wired one: one store flips the answer.
        driver.stores_done = 1
        self.assertTrue(plugin._engine_mutated())


class TestWriterFenceWiring(EnvSnapshotTestCase):
    def test_a_successful_load_spans_the_fence_and_releases_it(self):
        plugin, _loader, _driver = open_plugin(policy=POLICY_REQUIRE)
        plugin.load(None)
        self.assertTrue(plugin.fence.readable())
        self.assertFalse(plugin.fence.publishable())

    def test_a_failed_load_releases_the_fence(self):
        from gpustack_model_transfer.core.errors import DeniedContentError

        loader, driver = make_loader_driver(
            fail_store_after=0, store_error=DeniedContentError("engine refused")
        )
        plugin = GpustackLoadPlugin(loader, driver, clean_composition(), POLICY_REQUIRE)
        with self.assertRaises(StrategyRecoveryError):
            plugin.load(None)
        self.assertTrue(plugin.fence.readable())

    def test_a_load_while_another_writer_holds_the_fence_is_terminal(self):
        plugin, loader, _driver = open_plugin(policy=POLICY_REQUIRE)
        self.addCleanup(loader._close_reader)
        plugin.fence.begin_write("rank-7")
        with self.assertRaises(StrategyRecoveryError) as caught:
            plugin.load(None)
        self.assertIn("rank-7", str(caught.exception))
        self.assertIn("one writer", str(caught.exception))
        plugin.fence.finish("rank-7")


class TestLoadingGroupWiring(EnvSnapshotTestCase):
    def test_a_group_without_a_rank_id_is_refused_at_construction(self):
        from gpustack_model_transfer.adapters.modelexpress import LoadGroupCoordinator

        loader, driver = make_loader_driver()
        self.addCleanup(loader._close_reader)
        with self.assertRaises(ValueError) as caught:
            GpustackLoadPlugin(
                loader,
                driver,
                clean_composition(),
                POLICY_REQUIRE,
                group=LoadGroupCoordinator(expected_size=2),
            )
        self.assertIn("rank", str(caught.exception))

    def test_an_agreeing_group_proceeds_to_the_load(self):
        from gpustack_model_transfer.adapters.modelexpress import (
            LoadGroupCoordinator,
            RankProposal,
        )

        loader, driver = make_loader_driver()
        self.addCleanup(loader._close_reader)
        group = LoadGroupCoordinator(expected_size=2)
        group.propose(RankProposal(1, True, "gpustack"))
        plugin = GpustackLoadPlugin(
            loader, driver, clean_composition(), POLICY_REQUIRE, group=group, rank=0
        )
        result = plugin.load(None)
        self.assertIsNotNone(result.model)
        self.assertEqual(len(group.proposals), 2)

    def test_a_disagreeing_group_terminates_under_require_before_any_mutation(self):
        from gpustack_model_transfer.adapters.modelexpress import LoadGroupCoordinator

        loader, driver = make_loader_driver()
        self.addCleanup(loader._close_reader)
        group = LoadGroupCoordinator(expected_size=2)
        plugin = GpustackLoadPlugin(
            loader, driver, clean_composition(), POLICY_REQUIRE, group=group, rank=0
        )
        with self.assertRaises(StrategyRecoveryError) as caught:
            plugin.load(None)
        self.assertIn("loading group did not agree", str(caught.exception))
        self.assertIn("missing ranks [1]", str(caught.exception))
        self.assertEqual(driver.stores_done, 0)
        self.assertFalse(driver.built)

    def test_a_disagreeing_group_routes_the_downgrade_under_prefer(self):
        from gpustack_model_transfer.adapters.modelexpress import LoadGroupCoordinator

        loader, driver = make_loader_driver()
        self.addCleanup(loader._close_reader)
        group = LoadGroupCoordinator(expected_size=2)
        plugin = GpustackLoadPlugin(
            loader, driver, clean_composition(), POLICY_PREFER, group=group, rank=0
        )
        result = plugin.load(None)
        self.assertEqual(result.routed_fallback, "verified-node-delivery")
        self.assertEqual(driver.stores_done, 0)
        self.assertFalse(driver.built)


class TestOfflineGuardWiring(EnvSnapshotTestCase):
    def test_a_require_load_holds_the_guards_in_the_process_environment(self):
        plugin, _loader, _driver = open_plugin(policy=POLICY_REQUIRE)
        plugin.load(None)
        self.assertEqual(os.environ.get("HF_HUB_OFFLINE"), "1")
        self.assertEqual(os.environ.get("TRANSFORMERS_OFFLINE"), "1")

    def test_a_prefer_load_does_not_touch_the_environment(self):
        plugin, _loader, _driver = open_plugin(policy=POLICY_PREFER)
        plugin.load(None)
        self.assertNotIn("HF_HUB_OFFLINE", os.environ)
        self.assertNotIn("TRANSFORMERS_OFFLINE", os.environ)


if __name__ == "__main__":
    unittest.main()
