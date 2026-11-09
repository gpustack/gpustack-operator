"""The strategy entry: upstream identity and the registration surface."""

import json
import subprocess
import sys
import unittest
from pathlib import Path

from gpustack_model_transfer.adapters.modelexpress import (
    STRATEGY_NAME,
    GpustackLoadStrategy,
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
    """The strategy carries the upstream base when the package is present."""

    def test_without_modelexpress_the_strategy_stands_alone(self):
        code = (
            "import sys, json;"
            "sys.path.insert(0, sys.argv[1]);"
            "from gpustack_model_transfer.adapters.modelexpress.entry import"
            " GpustackLoadStrategy as strategy;"
            "json.dump([strategy.__mro__[1].__name__,"
            " strategy().name], sys.stdout)"
        )
        done = _run_python(code)
        self.assertEqual(done.returncode, 0, done.stderr)
        base_name, name = json.loads(done.stdout)
        self.assertEqual(base_name, "_FallbackLoadStrategy")
        self.assertEqual(name, "gpustack")

    def test_the_entry_subclasses_the_upstream_load_strategy_when_importable(self):
        code = (
            "import sys, types, json\n"
            "class LoadStrategy:\n"
            "    pass\n"
            "base_mod = types.ModuleType('modelexpress.load_strategy.base')\n"
            "base_mod.LoadStrategy = LoadStrategy\n"
            "chain_mod = types.ModuleType('modelexpress.load_strategy')\n"
            "mx = types.ModuleType('modelexpress')\n"
            "mx.__path__ = []\n"
            "sys.modules['modelexpress'] = mx\n"
            "sys.modules['modelexpress.load_strategy'] = chain_mod\n"
            "sys.modules['modelexpress.load_strategy.base'] = base_mod\n"
            "sys.path.insert(0, sys.argv[1])\n"
            "# The packaged bootstrap pre-imports the adapter at interpreter\n"
            "# start; re-importing after the upstream package exists must\n"
            "# bind the real base.\n"
            "for name in list(sys.modules):\n"
            "    if name.startswith('gpustack_model_transfer.adapters.modelexpress'):\n"
            "        del sys.modules[name]\n"
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


class TestStrategySurface(unittest.TestCase):
    def test_construction_takes_no_arguments(self):
        self.assertEqual(GpustackLoadStrategy().name, STRATEGY_NAME)
        self.assertEqual(STRATEGY_NAME, "gpustack")

    def test_the_plugin_file_entry_returns_the_concrete_class_when_bound(self):
        import sys

        from gpustack_model_transfer.adapters.modelexpress.entry import (
            register_plugin,
        )

        if not hasattr(
            sys.modules["gpustack_model_transfer.adapters.modelexpress.plugin"],
            "_STRATEGY_BASE",
        ):
            self.fail("the strategy module lost its base binding")
        bound = (
            sys.modules[
                "gpustack_model_transfer.adapters.modelexpress.plugin"
            ]._STRATEGY_BASE
            is not None
        )
        if bound:
            self.assertIs(register_plugin(), GpustackLoadStrategy)
            return
        from gpustack_model_transfer.adapters.modelexpress.policy import (
            PluginUnavailableError,
        )

        with self.assertRaises(PluginUnavailableError):
            register_plugin()

    def test_the_module_imports_without_modelexpress_or_torch(self):
        code = (
            "import sys, json;"
            "sys.path.insert(0, sys.argv[1]);"
            "import gpustack_model_transfer.adapters.modelexpress as m;"
            "json.dump(sorted(x for x in sys.modules"
            "    if x.split('.')[0] in ('modelexpress', 'torch')), sys.stdout)"
        )
        done = _run_python(code)
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertEqual(done.stdout.strip(), "[]")


if __name__ == "__main__":
    unittest.main()
