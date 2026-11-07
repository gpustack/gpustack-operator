"""Load-only resources released before KV profiling; NIXL connector untouched."""

import copy
import os
import unittest

import fixtureutil

from gpustack_model_transfer.adapters.vllm import (
    LoadOptions,
    ModelTransferModelLoader,
    ResourceLedger,
    TensorOwnership,
    profile_kv_capacity,
)
from gpustack_model_transfer.adapters.vllm.runtime import ReferenceRuntime
from gpustack_model_transfer.core.budgets import ReadBudgets
from gpustack_model_transfer.core.capabilities import local_backend_capabilities
from gpustack_model_transfer.core.errors import OperationStateError
from gpustack_model_transfer.core.local_reader import LocalFileReader


def default_budgets():
    return ReadBudgets(window_bytes=32, max_inflight=2, owned_output_bytes=4096)


def nixl_engine_config():
    return {
        "kv_transfer_config": {
            "kv_connector": "NixlConnector",
            "kv_role": "kv_both",
            "kv_buffer_device": "cuda",
        },
        "ec_connector_config": {
            "ec_connector": "NixlConnector",
            "ec_role": "ec_both",
        },
    }


def load_with(driver, engine_config=None):
    reader = LocalFileReader(
        fixtureutil.manifest_path(), expected_sha256=fixtureutil.manifest_digest()
    )
    loader = ModelTransferModelLoader(
        reader=reader,
        capabilities=local_backend_capabilities(),
        budgets=default_budgets(),
    )
    options = LoadOptions(
        ownership=TensorOwnership.OWNED,
        tie_word_embeddings=False,
        engine_config=engine_config,
    )
    return loader.load(driver, options)


class TestReleaseBeforeKvProfiling(unittest.TestCase):
    def test_load_only_resources_are_gone_when_load_returns(self):
        driver = ReferenceRuntime(fixtureutil.manifest_dir())
        report = load_with(driver)
        self.assertEqual(report.ledger.load_only_bytes, 0)
        self.assertGreater(
            report.ledger.peak_staging_bytes,
            0,
            "the ledger must have seen staging bytes",
        )
        self.assertEqual(
            report.ledger.retained_parameter_bytes,
            sum(
                len(fixtureutil.tensor_payload(name))
                for name in fixtureutil.parameter_names()
            ),
        )
        self.assertTrue(report.session_released)

    def test_kv_capacity_matches_the_native_baseline(self):
        driver = ReferenceRuntime(fixtureutil.manifest_dir())
        report = load_with(driver)
        capacity = profile_kv_capacity(driver, report.model, report.ledger)
        native_driver = ReferenceRuntime(fixtureutil.manifest_dir())
        native = native_driver.build_native_model()
        self.assertEqual(capacity, native_driver.profile_kv_capacity(native))
        self.assertGreater(capacity, 0)

    def test_profiling_refused_while_load_only_resources_are_held(self):
        driver = ReferenceRuntime(fixtureutil.manifest_dir())
        model = driver.build_native_model()
        ledger = ResourceLedger(load_only_bytes=1024)
        with self.assertRaises(OperationStateError):
            profile_kv_capacity(driver, model, ledger)
        with self.assertRaises(OperationStateError):
            profile_kv_capacity(driver, model, ResourceLedger(session_closed=False))
        unfinalized = driver.build_native_model()
        unfinalized.finalization_count = 0
        with self.assertRaises(OperationStateError):
            profile_kv_capacity(driver, unfinalized, ResourceLedger())

    def test_profiling_twice_is_refused(self):
        driver = ReferenceRuntime(fixtureutil.manifest_dir())
        report = load_with(driver)
        profile_kv_capacity(driver, report.model, report.ledger)
        with self.assertRaises(OperationStateError):
            profile_kv_capacity(driver, report.model, report.ledger)


class TestNixlConnectorCoexistence(unittest.TestCase):
    def test_connector_config_and_environment_survive_the_load(self):
        engine_config = nixl_engine_config()
        snapshot = copy.deepcopy(engine_config)
        saved_ucx = os.environ.get("UCX_NET_DEVICES")
        saved_nixl = os.environ.get("NIXL_BACKEND")
        os.environ["UCX_NET_DEVICES"] = "mlx5_0:1"
        os.environ["NIXL_BACKEND"] = "UCX"
        os.environ["GPUSTACK_MODEL_TRANSFER_TEST_MARKER"] = "present"
        env_keys = sorted(
            key for key in os.environ if key.startswith(("UCX_", "NIXL_"))
        )
        before = {key: os.environ[key] for key in env_keys}

        try:
            driver = ReferenceRuntime(fixtureutil.manifest_dir())
            report = load_with(driver, engine_config=engine_config)
            self.assertEqual(
                copy.deepcopy(engine_config),
                snapshot,
                "the loader rewrote the engine configuration",
            )
            self.assertEqual(
                report.engine_config["kv_transfer_config"]["kv_connector"],
                "NixlConnector",
            )
            after = {
                key: os.environ[key]
                for key in sorted(
                    key for key in os.environ if key.startswith(("UCX_", "NIXL_"))
                )
            }
            self.assertEqual(
                after, before, "the loader touched UCX/NIXL process settings"
            )
            self.assertEqual(after.get("UCX_NET_DEVICES"), "mlx5_0:1")
            self.assertEqual(after.get("NIXL_BACKEND"), "UCX")
            self.assertEqual(
                os.environ["GPUSTACK_MODEL_TRANSFER_TEST_MARKER"], "present"
            )
            # The connector stays intact for inference after the load.
            self.assertTrue(driver.forward(report.model, [1, 2]))
        finally:
            if saved_ucx is None:
                os.environ.pop("UCX_NET_DEVICES", None)
            else:
                os.environ["UCX_NET_DEVICES"] = saved_ucx
            if saved_nixl is None:
                os.environ.pop("NIXL_BACKEND", None)
            else:
                os.environ["NIXL_BACKEND"] = saved_nixl
            del os.environ["GPUSTACK_MODEL_TRANSFER_TEST_MARKER"]

    def test_engine_config_is_optional(self):
        driver = ReferenceRuntime(fixtureutil.manifest_dir())
        report = load_with(driver)
        self.assertIsNone(report.engine_config)
        self.assertTrue(driver.forward(report.model, [0]))


if __name__ == "__main__":
    unittest.main()
