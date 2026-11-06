"""Release order, KV profiling, and the resource ledger."""

import unittest

import sglangutil as fixtureutil

from gpustack_model_transfer.core.budgets import ReadBudgets
from gpustack_model_transfer.core.capabilities import local_backend_capabilities
from gpustack_model_transfer.core.errors import OperationStateError
from gpustack_model_transfer.core.local_reader import LocalFileReader


def load_report(**option_kwargs):
    from gpustack_model_transfer.adapters.sglang import (
        LoadOptions,
        ModelTransferSGLangLoader,
    )

    reader = LocalFileReader(fixtureutil.manifest_path())
    loader = ModelTransferSGLangLoader(
        reader=reader,
        capabilities=local_backend_capabilities(),
        budgets=ReadBudgets(window_bytes=32, max_inflight=2, owned_output_bytes=4096),
    )
    from gpustack_model_transfer.adapters.sglang.runtime import ReferenceRuntime

    driver = ReferenceRuntime(fixtureutil.manifest_dir())
    return loader.load(driver, LoadOptions(**option_kwargs)), driver


class TestReleaseOrder(unittest.TestCase):
    def test_load_only_bytes_return_to_zero_and_session_closes(self):
        report, _driver = load_report()
        self.assertEqual(report.ledger.load_only_bytes, 0)
        self.assertTrue(report.session_released)
        self.assertTrue(report.ledger.session_closed)

    def test_retained_parameter_bytes_are_reported_once(self):
        report, _driver = load_report()
        expected = sum(
            len(fixtureutil.tensor_payload(name))
            for name in fixtureutil.parameter_names()
        )
        self.assertEqual(report.ledger.retained_parameter_bytes, expected)


class TestProfileKVCapacity(unittest.TestCase):
    def test_profiling_requires_a_released_state(self):
        from gpustack_model_transfer.adapters.sglang import (
            ResourceLedger,
            profile_kv_capacity,
        )

        report, driver = load_report()
        with self.assertRaises(OperationStateError):
            profile_kv_capacity(
                driver, report.model, ResourceLedger(load_only_bytes=1024)
            )
        with self.assertRaises(OperationStateError):
            profile_kv_capacity(
                driver,
                report.model,
                ResourceLedger(session_closed=False),
            )

    def test_profiling_requires_exactly_once_finalization(self):
        from gpustack_model_transfer.adapters.sglang import (
            ResourceLedger,
            profile_kv_capacity,
        )

        report, driver = load_report()
        report.model.finalization_count = 2
        ledger = ResourceLedger(session_closed=True)
        with self.assertRaises(OperationStateError):
            profile_kv_capacity(driver, report.model, ledger)

    def test_profiling_answers_once_and_matches_the_native_baseline(self):
        from gpustack_model_transfer.adapters.sglang import profile_kv_capacity

        report, driver = load_report()
        capacity = profile_kv_capacity(driver, report.model, report.ledger)
        native = driver.build_native_model()
        self.assertEqual(capacity, driver.profile_kv_capacity(native))
        with self.assertRaises(OperationStateError):
            profile_kv_capacity(driver, report.model, report.ledger)


if __name__ == "__main__":
    unittest.main()
