"""The plugin registry: registration identity, terminal wrapper, results."""

import os
import sys
import types
import unittest
from abc import ABC


class TestStrategyRegistration(unittest.TestCase):
    """The upstream registry knows our strategy class by the plugin name."""

    def setUp(self):
        self._saved = dict(os.environ)
        for name in ("MX_LOAD_PLUGIN", "MX_ARTIFACT_TRANSFER"):
            os.environ.pop(name, None)

    def tearDown(self):
        os.environ.clear()
        os.environ.update(self._saved)

    def _install_registry(self):
        """Install a fake upstream plugin registry; return the plugins dict.

        The registry validates exactly what the pinned upstream's
        register_plugin validates, so identity and subclass rules hold.
        """

        def register_module(name, **attrs):
            module = types.ModuleType(name)
            module.__dict__.update(attrs)
            sys.modules[name] = module
            self.addCleanup(sys.modules.pop, name, None)
            return module

        import inspect

        plugins = {}

        def register_plugin(name, strategy_class):
            if name in plugins:
                raise ValueError(f"Plugin {name!r} is already registered")
            if (
                not inspect.isclass(strategy_class)
                or not issubclass(strategy_class, self.LoadStrategy)
                or inspect.isabstract(strategy_class)
            ):
                raise TypeError("must be a concrete LoadStrategy class")
            if getattr(strategy_class, "name", None) != name:
                raise ValueError("name mismatch")
            inspect.signature(strategy_class).bind()
            plugins[name] = strategy_class

        class LoadStrategy(ABC):  # noqa: B024 — mirrors the upstream base
            pass

        self.LoadStrategy = LoadStrategy
        register_module("modelexpress", __path__=[])
        register_module(
            "modelexpress.plugins",
            register_plugin=register_plugin,
            plugins=plugins,
        )
        register_module(
            "modelexpress.load_strategy",
            execute_load_strategies=lambda model, ctx, strategies: model,
            LoadStrategy=LoadStrategy,
        )
        register_module("modelexpress.load_strategy.base", LoadStrategy=LoadStrategy)
        return plugins

    def _fresh_plugin_module(self):
        import importlib

        sys.modules.pop("gpustack_model_transfer.adapters.modelexpress.plugin", None)
        module = importlib.import_module(
            "gpustack_model_transfer.adapters.modelexpress.plugin"
        )
        self.addCleanup(
            sys.modules.pop,
            "gpustack_model_transfer.adapters.modelexpress.plugin",
            None,
        )
        return module

    def test_registration_is_idempotent_without_evicting_modules(self):
        """Repeated registration reuses one class object, whatever the order."""
        import importlib

        plugins = self._install_registry()

        # A bootstrap that ran before modelexpress was importable holds
        # the fallback-bound class.
        early = importlib.import_module(
            "gpustack_model_transfer.adapters.modelexpress.plugin"
        )
        early_class = early.GpustackLoadStrategy

        from gpustack_model_transfer.adapters.modelexpress.registration import (
            register_strategy_with_modelexpress,
        )

        first = register_strategy_with_modelexpress()
        second = register_strategy_with_modelexpress()
        self.assertIs(first, second)
        self.assertIs(first, early_class)
        self.assertIn("gpustack", plugins)
        self.assertTrue(issubclass(plugins["gpustack"], self.LoadStrategy))
        self.assertTrue(issubclass(early_class, self.LoadStrategy))

    def test_register_after_bootstrap_and_loader_hooks_stays_one_class(self):
        """bootstrap + general_plugins + loader hook all land one class."""
        import importlib

        self._install_registry()
        early = importlib.import_module(
            "gpustack_model_transfer.adapters.modelexpress.plugin"
        )
        from gpustack_model_transfer.adapters.modelexpress import registration
        from gpustack_model_transfer.adapters.modelexpress.entry import (
            register_plugin as entry_register,
        )

        registration.register_strategy_with_modelexpress()
        registration.register_strategy_with_modelexpress()
        self.assertIs(entry_register(), early.GpustackLoadStrategy)

    def test_a_foreign_strategy_under_the_name_is_refused(self):
        plugins = self._install_registry()

        class ForeignStrategy(self.LoadStrategy):
            name = "gpustack"

        plugins["gpustack"] = ForeignStrategy
        self._fresh_plugin_module()
        from gpustack_model_transfer.adapters.modelexpress.registration import (
            register_strategy_with_modelexpress,
        )

        with self.assertRaises(ValueError):
            register_strategy_with_modelexpress()
        self.assertIs(plugins["gpustack"], ForeignStrategy)

    def test_registration_without_upstream_raises_a_clear_error(self):
        from gpustack_model_transfer.adapters.modelexpress.registration import (
            register_strategy_with_modelexpress,
        )

        with self.assertRaises(ModuleNotFoundError):
            register_strategy_with_modelexpress()

    def test_registration_without_the_plugin_module_is_refused(self):
        def fake_module(name, **attrs):
            module = types.ModuleType(name)
            module.__dict__.update(attrs)
            sys.modules[name] = module
            self.addCleanup(sys.modules.pop, name, None)
            return module

        fake_module("modelexpress", __path__=[])
        # A registry without register_plugin is not a supported surface.
        fake_module("modelexpress.plugins", plugins={})
        from gpustack_model_transfer.adapters.modelexpress.registration import (
            register_strategy_with_modelexpress,
        )

        with self.assertRaises(TypeError):
            register_strategy_with_modelexpress()


class TestTerminalErrorPreservation(unittest.TestCase):
    def test_the_wrapper_preserves_the_terminal_error(self):
        from gpustack_model_transfer.adapters.modelexpress import (
            StrategyRecoveryError,
            preserve_terminal_error,
        )

        @preserve_terminal_error
        def strategy_body():
            raise StrategyRecoveryError("forbidden fallback")

        with self.assertRaises(StrategyRecoveryError) as caught:
            strategy_body()
        self.assertEqual(str(caught.exception), "forbidden fallback")

    def test_the_wrapper_does_not_convert_terminal_into_recoverable(self):
        from gpustack_model_transfer.adapters.modelexpress import (
            StrategyRecoveryError,
            preserve_terminal_error,
        )

        @preserve_terminal_error
        def strategy_body():
            raise StrategyRecoveryError("terminal")

        recovered = False
        try:
            strategy_body()
        except StrategyRecoveryError:
            recovered = True
        except Exception:
            recovered = False
        self.assertTrue(recovered)

    def test_the_wrapper_keeps_ordinary_failures_ordinary(self):
        from gpustack_model_transfer.adapters.modelexpress import (
            preserve_terminal_error,
        )

        @preserve_terminal_error
        def strategy_body():
            raise RuntimeError("an ordinary engine failure")

        with self.assertRaises(RuntimeError):
            strategy_body()


class TestExpectedMissConstruction(unittest.TestCase):
    """strategy_failed stays classified against a mismatched upstream."""

    def _install_upstream(self, failed_class):
        """Bind the probe to one upstream StrategyFailed definition."""

        def register_module(name, **attrs):
            module = types.ModuleType(name)
            module.__dict__.update(attrs)
            sys.modules[name] = module
            self.addCleanup(sys.modules.pop, name, None)
            return module

        register_module("modelexpress", __path__=[])
        register_module("modelexpress.adapter", StrategyFailed=failed_class)
        return failed_class

    def test_an_upstream_class_that_rejects_mutated_keeps_its_identity(self):
        """The chain catches its own class: a stand-in would escape uncaught."""

        class RigidStrategyFailed(RuntimeError):
            def __init__(self, message):
                super().__init__(message)

        installed = self._install_upstream(RigidStrategyFailed)
        from gpustack_model_transfer.adapters.modelexpress.plugin import (
            strategy_failed,
        )

        error = strategy_failed("no ready source", mutated=True)
        self.assertIsInstance(error, installed)
        self.assertTrue(error.mutated)

    def test_an_upstream_class_without_a_usable_signature_falls_back_to_the_local(
        self,
    ):
        """No upstream constructor is callable: the local class is last resort."""

        class ArglessStrategyFailed(RuntimeError):
            def __init__(self):
                super().__init__()

        self._install_upstream(ArglessStrategyFailed)
        from gpustack_model_transfer.adapters.modelexpress.plugin import (
            StrategyFailed,
            strategy_failed,
        )

        error = strategy_failed("no ready source", mutated=True)
        self.assertIsInstance(error, StrategyFailed)
        self.assertTrue(error.mutated)

    def test_an_accepting_upstream_class_is_used_directly(self):
        class UpstreamStrategyFailed(RuntimeError):
            def __init__(self, message, *, mutated=False):
                super().__init__(message)
                self.mutated = mutated

        installed = self._install_upstream(UpstreamStrategyFailed)
        from gpustack_model_transfer.adapters.modelexpress.plugin import (
            strategy_failed,
        )

        error = strategy_failed("no ready source", mutated=True)
        self.assertIsInstance(error, installed)
        self.assertTrue(error.mutated)


class TestRegistrationFailurePolicy(unittest.TestCase):
    """register()'s failed arm: advisory under Prefer, enforcing under Require."""

    def setUp(self):
        self._saved = dict(os.environ)
        os.environ.pop("MX_LOAD_PLUGIN", None)
        os.environ.pop("GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY", None)

    def tearDown(self):
        os.environ.clear()
        os.environ.update(self._saved)

    def _failed_arm(self):
        from gpustack_model_transfer.adapters.modelexpress.registration import (
            _handle_registration_failure,
        )

        _handle_registration_failure(ValueError("a failed arm"))

    def test_a_prefer_failure_is_logged_not_raised(self):
        with self.assertLogs(
            "gpustack_model_transfer.adapters.modelexpress.registration",
            level="ERROR",
        ):
            self._failed_arm()

    def test_a_require_failure_is_reraised(self):
        os.environ["GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY"] = "Require"
        with self.assertRaises(ValueError):
            self._failed_arm()


class TestPluginResult(unittest.TestCase):
    def test_the_result_publishes_only_through_the_strategy(self):
        from gpustack_model_transfer.adapters.modelexpress import PluginResult

        self.assertFalse(PluginResult(model=object()).publication_allowed())
        self.assertTrue(
            PluginResult(model=object(), publishable=True).publication_allowed()
        )

    def test_the_expected_miss_carries_the_mutation_flag(self):
        from gpustack_model_transfer.adapters.modelexpress.plugin import StrategyFailed

        clean = StrategyFailed("no ready source", mutated=False)
        tainted = StrategyFailed("mid-stream failure", mutated=True)
        self.assertFalse(clean.mutated)
        self.assertTrue(tainted.mutated)


if __name__ == "__main__":
    unittest.main()
