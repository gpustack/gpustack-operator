"""The strategy entry: upstream identity and the environment contract."""

import json
import os
import subprocess
import sys
import unittest
from pathlib import Path

import mxutil

from gpustack_model_transfer.adapters.modelexpress import (
    ENV_MANIFEST,
    ENV_MANIFEST_SHA256,
    ENV_POLICY,
    GpustackLoadStrategy,
    PluginUnavailableError,
    StrategyRecoveryError,
    build_plugin,
)

_PACKAGE_ROOT = str(Path(__file__).resolve().parents[2])


def _run_python(code):
    done = subprocess.run(
        [sys.executable, "-c", code, _PACKAGE_ROOT],
        capture_output=True,
        text=True,
        timeout=60,
        check=False,
    )
    return done


class TestUpstreamIdentity(unittest.TestCase):
    """The raised error IS the chain's own, when the chain is present."""

    def test_without_modelexpress_the_terminal_error_stands_alone(self):
        code = (
            "import sys, json;"
            "sys.path.insert(0, sys.argv[1]);"
            "from gpustack_model_transfer.adapters.modelexpress.policy import"
            " StrategyRecoveryError as ours;"
            "json.dump([issubclass(ours, Exception),"
            " ours.__bases__ == (Exception,)], sys.stdout)"
        )
        done = _run_python(code)
        self.assertEqual(done.returncode, 0, done.stderr)
        standalone, plain_base = json.loads(done.stdout)
        self.assertTrue(standalone)
        self.assertTrue(plain_base)

    def test_ours_subclasses_the_upstream_terminal_error_when_importable(self):
        code = (
            "import sys, types, json;"
            "up = type('UpstreamTerminal', (Exception,), {});"
            "fake = types.ModuleType('modelexpress');"
            "fake.StrategyRecoveryError = up;"
            "sys.modules['modelexpress'] = fake;"
            "sys.path.insert(0, sys.argv[1]);"
            "from gpustack_model_transfer.adapters.modelexpress.policy import"
            " StrategyRecoveryError as ours;"
            "caught = False\n"
            "try:\n"
            "    raise ours('terminal')\n"
            "except up:\n"
            "    caught = True\n"
            "json.dump([issubclass(ours, up), caught], sys.stdout)"
        )
        done = _run_python(code)
        self.assertEqual(done.returncode, 0, done.stderr)
        subclassed, caught_upstream = json.loads(done.stdout)
        self.assertTrue(subclassed)
        self.assertTrue(caught_upstream)

    def test_the_entry_subclasses_the_upstream_load_strategy_when_importable(self):
        code = (
            "import sys, types, json\n"
            "class LoadStrategy:\n"
            "    pass\n"
            "fake = types.ModuleType('modelexpress')\n"
            "fake.LoadStrategy = LoadStrategy\n"
            "sys.modules['modelexpress'] = fake\n"
            "sys.path.insert(0, sys.argv[1])\n"
            "from gpustack_model_transfer.adapters.modelexpress.entry import"
            " GpustackLoadStrategy as strategy\n"
            "instance = strategy()\n"
            "json.dump([issubclass(strategy, LoadStrategy),"
            " isinstance(instance, LoadStrategy),"
            " strategy.__name__], sys.stdout)"
        )
        done = _run_python(code)
        self.assertEqual(done.returncode, 0, done.stderr)
        subclassed, instantiates, name = json.loads(done.stdout)
        self.assertTrue(subclassed)
        self.assertTrue(instantiates)
        self.assertEqual(name, "GpustackLoadStrategy")


class EnvSnapshotTestCase(unittest.TestCase):
    """Isolate the process environment this module's tests mutate."""

    def setUp(self):
        self._saved_env = dict(os.environ)
        self.addCleanup(self._restore_env)

    def _restore_env(self):
        os.environ.clear()
        os.environ.update(self._saved_env)


class TestEnvironmentContract(EnvSnapshotTestCase):
    def test_the_env_names_are_the_declared_contract(self):
        self.assertEqual(ENV_MANIFEST, "GPUSTACK_MODEL_TRANSFER_MANIFEST")
        self.assertEqual(ENV_MANIFEST_SHA256, "GPUSTACK_MODEL_TRANSFER_MANIFEST_SHA256")
        self.assertEqual(ENV_POLICY, "GPUSTACK_MODEL_TRANSFER_POLICY")

    def test_a_plugin_builds_from_the_environment(self):
        from gpustack_model_transfer.adapters.modelexpress import GpustackLoadPlugin

        env = {
            ENV_MANIFEST: str(mxutil.manifest_path()),
            ENV_MANIFEST_SHA256: mxutil.manifest_digest(),
        }
        plugin = build_plugin(env)
        self.addCleanup(plugin._loader._close_reader)
        self.assertIsInstance(plugin, GpustackLoadPlugin)

    def test_the_policy_defaults_to_require_and_accepts_prefer(self):
        env = {
            ENV_MANIFEST: str(mxutil.manifest_path()),
            ENV_MANIFEST_SHA256: mxutil.manifest_digest(),
        }
        require_plugin = build_plugin(dict(env))
        self.addCleanup(require_plugin._loader._close_reader)
        self.assertEqual(require_plugin._policy, "require")
        env[ENV_POLICY] = "prefer"
        prefer_plugin = build_plugin(env)
        self.addCleanup(prefer_plugin._loader._close_reader)
        self.assertEqual(prefer_plugin._policy, "prefer")

    def test_an_unknown_policy_is_refused(self):
        env = {
            ENV_MANIFEST: str(mxutil.manifest_path()),
            ENV_MANIFEST_SHA256: mxutil.manifest_digest(),
            ENV_POLICY: "auto",
        }
        with self.assertRaises(ValueError):
            build_plugin(env)

    def test_a_missing_artifact_environment_names_what_is_missing(self):
        with self.assertRaises(PluginUnavailableError) as caught:
            build_plugin({})
        self.assertIn(ENV_MANIFEST, str(caught.exception))
        self.assertIn(ENV_MANIFEST_SHA256, str(caught.exception))


class TestStrategySurface(EnvSnapshotTestCase):
    def test_construction_takes_no_arguments(self):
        self.assertEqual(GpustackLoadStrategy().name, "gpustack")

    def test_missing_environment_is_terminal_before_reader_creation(self):
        strategy = GpustackLoadStrategy()
        with self.assertRaises(StrategyRecoveryError):
            strategy.is_available(None)
        self.assertIsNone(strategy._native)

    def test_load_without_prepared_context_is_terminal(self):
        strategy = GpustackLoadStrategy()
        with self.assertRaises(StrategyRecoveryError):
            strategy.load(None, None)
        self.assertIsNone(strategy._native)
        strategy.close()

    def test_finalize_without_a_load_is_terminal(self):
        strategy = GpustackLoadStrategy()
        with self.assertRaises(StrategyRecoveryError):
            strategy.finalize(None, None)
        self.assertIsNone(strategy._native)


if __name__ == "__main__":
    unittest.main()
