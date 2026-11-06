"""The pinned vLLM-Ascend loader extension contract and image integration route."""

import hashlib
import json
import os
import subprocess
import sys
import unittest
from pathlib import Path
from unittest import mock

import ascendutil as fixtureutil

MODEL_TRANSFER_PYTHON_ROOT = Path(__file__).resolve().parents[2]


class TestLoaderExtensionContract(unittest.TestCase):
    def test_load_format_and_entry_route_are_pinned(self):
        from gpustack_model_transfer.adapters import vllm_ascend

        self.assertEqual(vllm_ascend.LOADER_LOAD_FORMAT, "model-transfer")
        self.assertEqual(vllm_ascend.LOADER_CLASS_NAME, "NativeModelTransferLoader")
        # vLLM-Ascend is a vLLM platform plugin, so the loader rides
        # vLLM's own registry; the entry point names this package.
        self.assertEqual(vllm_ascend.LOADER_ENTRY_POINT_GROUP, "vllm.general_plugins")
        self.assertEqual(
            vllm_ascend.LOADER_ENTRY_POINT,
            "gpustack_model_transfer.adapters.vllm.plugin:register",
        )
        self.assertEqual(vllm_ascend.INTEGRATION_ROUTE, "platform-plugin")
        self.assertEqual(vllm_ascend.PLATFORM_PACKAGE, "vllm_ascend")

    def test_checkpoint_format_and_dtypes_are_pinned(self):
        from gpustack_model_transfer.adapters import vllm_ascend

        self.assertEqual(vllm_ascend.SUPPORTED_CHECKPOINT_FORMAT, "safetensors")
        for dtype in ("F32", "F16", "BF16", "I64"):
            self.assertIn(dtype, vllm_ascend.SUPPORTED_DTYPES)
        # Every dtype the fixture uses must be supported, so the pinned
        # contract cannot silently lag the golden fixture.
        manifest = fixtureutil.manifest_dict()
        for tensor in manifest["tensors"]:
            self.assertIn(tensor["dtype"], vllm_ascend.SUPPORTED_DTYPES, tensor["name"])

    def test_ascend_connector_environment_keys_are_declared(self):
        from gpustack_model_transfer.adapters import vllm_ascend

        # The process environment a load must never disturb: device
        # visibility, the HCCL interface address, and the backend choice.
        for key in ("ASCEND_RT_VISIBLE_DEVICES", "HCCL_IF_IP", "HIXL_BACKEND"):
            self.assertIn(key, vllm_ascend.CONNECTOR_ENV_KEYS)
        self.assertEqual(
            vllm_ascend.DEFAULT_VISIBILITY_STREAM,
            "npu:0",
            "a borrowed tensor becomes visible on the NPU stream",
        )

    def test_ascend_connector_config_shape_is_declared(self):
        from gpustack_model_transfer.adapters import vllm_ascend

        config = vllm_ascend.ASCEND_ENGINE_CONFIG
        self.assertEqual(config["kv_transfer_config"]["kv_connector"], "HixlConnector")
        self.assertEqual(config["kv_transfer_config"]["kv_role"], "kv_both")
        self.assertEqual(config["kv_transfer_config"]["kv_buffer_device"], "npu")
        self.assertEqual(config["ec_connector_config"]["ec_connector"], "HixlConnector")

    def test_image_integration_metadata_is_declared(self):
        from gpustack_model_transfer.adapters import vllm_ascend

        metadata = vllm_ascend.image_integration_metadata()
        self.assertIsInstance(metadata, dict)
        for key in ("reader", "load-format", "contract", "engine", "integration"):
            self.assertIn(key, metadata)
        self.assertEqual(metadata["load-format"], vllm_ascend.LOADER_LOAD_FORMAT)
        self.assertEqual(metadata["engine"], "vllm-ascend")
        self.assertEqual(metadata["integration"], "platform-plugin")
        # The metadata rides image labels, so it must survive JSON.
        json.loads(json.dumps(metadata))

    def test_importing_the_adapter_pulls_in_no_npu_stack(self):
        # The adapter must import on a CPU-only stdlib checkout: torch,
        # torch_npu, and vllm_ascend are lazy, inside npu_runtime's methods.
        code = (
            "import sys; import gpustack_model_transfer.adapters.vllm_ascend; "
            "assert 'torch' not in sys.modules, 'torch imported'; "
            "assert 'torch_npu' not in sys.modules, 'torch_npu imported'; "
            "assert 'vllm_ascend' not in sys.modules, 'vllm_ascend imported'; "
            "print('clean')"
        )
        proc = subprocess.run(
            [sys.executable, "-c", code],
            capture_output=True,
            text=True,
            env={
                "PYTHONPATH": str(MODEL_TRANSFER_PYTHON_ROOT),
                "PATH": "/usr/bin:/bin",
            },
            timeout=120,
        )
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertIn("clean", proc.stdout)


class TestToleranceSpec(unittest.TestCase):
    def test_predeclared_defaults(self):
        from gpustack_model_transfer.adapters.vllm_ascend.tolerances import (
            DEFAULT_FORWARD_TOLERANCE,
            DEFAULT_PARAMETER_TOLERANCE,
            ToleranceSpec,
        )

        self.assertEqual(DEFAULT_PARAMETER_TOLERANCE, ToleranceSpec(rtol=0.0, atol=0.0))
        self.assertEqual(DEFAULT_FORWARD_TOLERANCE, ToleranceSpec(rtol=1e-5, atol=1e-6))

    def test_negative_or_nan_tolerance_refused(self):
        from gpustack_model_transfer.adapters.vllm_ascend.tolerances import (
            ToleranceSpec,
        )

        for spec in (
            ToleranceSpec(rtol=-1.0, atol=0.0),
            ToleranceSpec(rtol=0.0, atol=float("nan")),
        ):
            with self.assertRaises(ValueError):
                spec.validate()

    def test_within_compares_elementwise_with_nan_bitwise_rule(self):
        from gpustack_model_transfer.adapters.vllm_ascend.tolerances import (
            ToleranceSpec,
        )

        exact = ToleranceSpec(rtol=0.0, atol=0.0)
        loose = ToleranceSpec(rtol=1e-5, atol=1e-6)
        nan = float("nan")
        self.assertTrue(exact.within([1.0, 2.0], [1.0, 2.0]))
        self.assertFalse(exact.within([1.0], [1.0, 2.0]))
        self.assertFalse(exact.within([1.0 + 1e-9], [1.0]))
        self.assertTrue(loose.within([1.0 + 1e-9], [1.0]))
        self.assertTrue(exact.within([nan], [nan]))
        self.assertFalse(exact.within([nan], [0.0]))

    def test_digest_helpers_of_the_fixture_agree(self):
        # The manifest digest the tests pin is the bytes' sha256.
        self.assertEqual(
            fixtureutil.manifest_digest(),
            hashlib.sha256(fixtureutil.manifest_bytes()).hexdigest(),
        )


if __name__ == "__main__":
    unittest.main()


class TestPinnedUpstream(unittest.TestCase):
    def test_upstream_commit_is_pinned_and_carried(self):
        from gpustack_model_transfer.adapters.vllm_ascend import (
            PINNED_UPSTREAM_COMMIT,
            image_integration_metadata,
        )

        self.assertEqual(
            PINNED_UPSTREAM_COMMIT,
            "vllm-ascend@31f9175f324182466877d0ab9006e75ea6a401f3",
        )
        self.assertEqual(
            image_integration_metadata()["upstream"], PINNED_UPSTREAM_COMMIT
        )


class TestDeviceResolution(unittest.TestCase):
    def test_device_honors_assignment_then_visibility_then_zero(self):
        from gpustack_model_transfer.adapters.vllm_ascend.npu_runtime import (
            NPURuntimeDriver,
        )

        resolve = NPURuntimeDriver._resolve_device
        self.assertEqual(resolve(3), 3, "an explicit assignment wins")
        with mock.patch.dict(os.environ, {"ASCEND_RT_VISIBLE_DEVICES": "4,5"}):
            self.assertEqual(resolve(None), 4, "the first visible device follows")
        with mock.patch.dict(os.environ, {"ASCEND_RT_VISIBLE_DEVICES": ""}):
            self.assertEqual(resolve(None), 0, "no visibility means device 0")
        with mock.patch.dict(os.environ, {"ASCEND_RT_VISIBLE_DEVICES": "npu-anything"}):
            self.assertEqual(
                resolve(None), 0, "a non-numeric visibility is not a device index"
            )
