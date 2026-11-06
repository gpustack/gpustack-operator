"""The pinned loader extension contract and lazy-import hygiene."""

import subprocess
import sys
import unittest
from pathlib import Path

import fixtureutil

MODEL_TRANSFER_ROOT = Path(__file__).resolve().parents[2]


class TestLoaderExtensionContract(unittest.TestCase):
    def test_load_format_and_entry_route_are_pinned(self):
        from gpustack_model_transfer.adapters import vllm

        self.assertEqual(vllm.LOADER_LOAD_FORMAT, "model-transfer")
        self.assertEqual(vllm.LOADER_CLASS_NAME, "NativeModelTransferLoader")
        # The route names where an engine image finds the loader: entry
        # point group, module, and attribute.
        self.assertEqual(vllm.LOADER_ENTRY_POINT_GROUP, "vllm.general_plugins")
        self.assertEqual(
            vllm.LOADER_ENTRY_POINT,
            "gpustack_model_transfer.adapters.vllm.plugin:register",
        )

    def test_checkpoint_format_and_dtypes_are_pinned(self):
        from gpustack_model_transfer.adapters import vllm

        self.assertEqual(vllm.SUPPORTED_CHECKPOINT_FORMAT, "safetensors")
        for dtype in ("F32", "F16", "BF16", "I64"):
            self.assertIn(dtype, vllm.SUPPORTED_DTYPES)
        # Every dtype the fixture uses must be supported, so the pinned
        # contract cannot silently lag the golden fixture.
        manifest = fixtureutil.manifest_dict()
        for tensor in manifest["tensors"]:
            self.assertIn(tensor["dtype"], vllm.SUPPORTED_DTYPES, tensor["name"])

    def test_image_integration_metadata_is_declared(self):
        from gpustack_model_transfer.adapters import vllm

        metadata = vllm.image_integration_metadata()
        self.assertIsInstance(metadata, dict)
        for key in ("reader", "load-format", "contract"):
            self.assertIn(key, metadata)
        self.assertEqual(metadata["load-format"], vllm.LOADER_LOAD_FORMAT)
        self.assertIn("gpustack-model-transfer", metadata["contract"])

    def test_importing_the_adapter_pulls_in_no_torch_and_no_vllm(self):
        # The adapter must import on a CPU-only stdlib checkout. A fresh
        # interpreter proves no lazy module leaked in at import time.
        code = (
            "import sys; import gpustack_model_transfer.adapters.vllm; "
            "assert 'torch' not in sys.modules, 'torch imported'; "
            "assert 'vllm' not in sys.modules, 'vllm imported'; "
            "print('clean')"
        )
        proc = subprocess.run(
            [sys.executable, "-c", code],
            capture_output=True,
            text=True,
            env={"PYTHONPATH": str(MODEL_TRANSFER_ROOT), "PATH": "/usr/bin:/bin"},
            timeout=120,
        )
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertIn("clean", proc.stdout)


class TestToleranceSpec(unittest.TestCase):
    def test_predeclared_defaults(self):
        from gpustack_model_transfer.adapters.vllm.tolerances import (
            DEFAULT_FORWARD_TOLERANCE,
            DEFAULT_PARAMETER_TOLERANCE,
            ToleranceSpec,
        )

        self.assertEqual(DEFAULT_PARAMETER_TOLERANCE, ToleranceSpec(rtol=0.0, atol=0.0))
        self.assertEqual(DEFAULT_FORWARD_TOLERANCE, ToleranceSpec(rtol=1e-5, atol=1e-6))

    def test_negative_or_nan_tolerance_refused(self):
        from gpustack_model_transfer.adapters.vllm.tolerances import ToleranceSpec

        for spec in (
            ToleranceSpec(rtol=-1.0, atol=0.0),
            ToleranceSpec(rtol=0.0, atol=float("nan")),
        ):
            with self.assertRaises(ValueError):
                spec.validate()

    def test_within_compares_elementwise_with_nan_bitwise_rule(self):
        from gpustack_model_transfer.adapters.vllm.tolerances import ToleranceSpec

        exact = ToleranceSpec(rtol=0.0, atol=0.0)
        loose = ToleranceSpec(rtol=1e-5, atol=1e-6)
        nan = float("nan")
        self.assertTrue(exact.within([1.0, 2.0], [1.0, 2.0]))
        self.assertFalse(exact.within([1.0], [1.0, 2.0]))
        self.assertFalse(exact.within([1.0 + 1e-9], [1.0]))
        self.assertTrue(loose.within([1.0 + 1e-9], [1.0]))
        # Identical NaN bit patterns match; distinct payloads do not.
        self.assertTrue(exact.within([nan], [nan]))
        self.assertFalse(exact.within([nan], [0.0]))

    def test_within_rejects_when_any_element_is_outside_tolerance(self):
        from gpustack_model_transfer.adapters.vllm.tolerances import ToleranceSpec

        loose = ToleranceSpec(rtol=1e-5, atol=1e-6)
        self.assertFalse(loose.within([1.0, 1.0], [1.0, 1.1]))


if __name__ == "__main__":
    unittest.main()
