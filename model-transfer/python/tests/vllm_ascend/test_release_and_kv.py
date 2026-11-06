"""Load-only resources released before KV profiling; the Ascend connector untouched."""

import copy
import os
import unittest

import ascendutil as fixtureutil

from gpustack_model_transfer.adapters.vllm_ascend import (
    DEFAULT_VISIBILITY_STREAM,
    LoadOptions,
    ModelTransferAscendLoader,
    ResourceLedger,
    TensorOwnership,
    profile_kv_capacity,
)
from gpustack_model_transfer.adapters.vllm_ascend.constants import (
    ASCEND_ENGINE_CONFIG,
    CONNECTOR_ENV_KEYS,
)
from gpustack_model_transfer.adapters.vllm_ascend.runtime import ReferenceRuntime
from gpustack_model_transfer.core.budgets import ReadBudgets
from gpustack_model_transfer.core.capabilities import local_backend_capabilities
from gpustack_model_transfer.core.errors import OperationStateError
from gpustack_model_transfer.core.local_reader import LocalFileReader


def default_budgets():
    return ReadBudgets(window_bytes=32, max_inflight=2, owned_output_bytes=4096)


def ascend_engine_config():
    return copy.deepcopy(ASCEND_ENGINE_CONFIG)


def load_with(driver, engine_config=None):
    reader = LocalFileReader(
        fixtureutil.manifest_path(), expected_sha256=fixtureutil.manifest_digest()
    )
    loader = ModelTransferAscendLoader(
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
        self.assertTrue(report.ledger.session_closed)

    def test_kv_capacity_matches_the_native_baseline(self):
        driver = ReferenceRuntime(fixtureutil.manifest_dir())
        report = load_with(driver)
        capacity = profile_kv_capacity(driver, report.model, report.ledger)
        native_driver = ReferenceRuntime(fixtureutil.manifest_dir())
        native = native_driver.build_native_model()
        self.assertEqual(capacity, native_driver.profile_kv_capacity(native))
        self.assertGreater(capacity, 0)

    def test_profiling_demands_a_released_finalized_state(self):
        driver = ReferenceRuntime(fixtureutil.manifest_dir())
        model = driver.build_native_model()
        # Staging bytes still held.
        with self.assertRaises(OperationStateError):
            profile_kv_capacity(driver, model, ResourceLedger(load_only_bytes=1024))
        # The reader session is still open.
        with self.assertRaises(OperationStateError):
            profile_kv_capacity(driver, model, ResourceLedger(session_closed=False))
        # The model never finalized, or finalized more than once: an
        # Ascend finalization (contiguous, cast, format) that ran twice is
        # not a state whose capacity answer means anything.
        unfinalized = driver.build_native_model()
        unfinalized.finalization_count = 0
        with self.assertRaises(OperationStateError):
            profile_kv_capacity(driver, unfinalized, ResourceLedger())
        double = driver.build_native_model()
        double.finalization_count = 2
        with self.assertRaises(OperationStateError):
            profile_kv_capacity(driver, double, ResourceLedger())

    def test_profiling_twice_is_refused(self):
        driver = ReferenceRuntime(fixtureutil.manifest_dir())
        report = load_with(driver)
        profile_kv_capacity(driver, report.model, report.ledger)
        with self.assertRaises(OperationStateError):
            profile_kv_capacity(driver, report.model, report.ledger)


class TestAscendConnectorCoexistence(unittest.TestCase):
    def test_connector_config_and_ascend_environment_survive_the_load(self):
        engine_config = ascend_engine_config()
        snapshot = copy.deepcopy(engine_config)
        saved = {key: os.environ.get(key) for key in CONNECTOR_ENV_KEYS}
        os.environ["ASCEND_RT_VISIBLE_DEVICES"] = "0,1"
        os.environ["HCCL_IF_IP"] = "10.0.0.5"
        os.environ["HIXL_BACKEND"] = "ACLNN"
        os.environ["GPUSTACK_MODEL_TRANSFER_TEST_MARKER"] = "present"
        before = {key: os.environ[key] for key in CONNECTOR_ENV_KEYS}

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
                "HixlConnector",
            )
            after = {key: os.environ[key] for key in CONNECTOR_ENV_KEYS}
            self.assertEqual(
                after, before, "the loader touched the Ascend process settings"
            )
            self.assertEqual(after["ASCEND_RT_VISIBLE_DEVICES"], "0,1")
            self.assertEqual(after["HCCL_IF_IP"], "10.0.0.5")
            self.assertEqual(after["HIXL_BACKEND"], "ACLNN")
            self.assertEqual(
                os.environ["GPUSTACK_MODEL_TRANSFER_TEST_MARKER"], "present"
            )
            # The connector stays intact for inference after the load.
            self.assertTrue(driver.forward(report.model, [1, 2]))
        finally:
            for key, value in saved.items():
                if value is None:
                    os.environ.pop(key, None)
                else:
                    os.environ[key] = value
            del os.environ["GPUSTACK_MODEL_TRANSFER_TEST_MARKER"]

    def test_engine_config_is_optional(self):
        driver = ReferenceRuntime(fixtureutil.manifest_dir())
        report = load_with(driver)
        self.assertIsNone(report.engine_config)
        self.assertTrue(driver.forward(report.model, [0]))

    def test_borrowed_visibility_defaults_to_the_npu_stream(self):
        # The declared Ascend visibility stream is npu:0: an event record
        # on that stream is what makes a transferred tensor visible to the
        # consuming kernels before the fence returns.
        self.assertEqual(DEFAULT_VISIBILITY_STREAM, "npu:0")


if __name__ == "__main__":
    unittest.main()
