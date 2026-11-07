"""The HIXL reader: windows, ordering, states, ledger, real payloads."""

import hashlib
import unittest

import hixlutil

from gpustack_model_transfer.backends.hixl import (
    DIRECTION_READ,
    DIRECTION_WRITE,
    HixlReader,
)
from gpustack_model_transfer.backends.hixl.errors import TransferRefusedError
from gpustack_model_transfer.backends.hixl.memory import MemoryLedger
from gpustack_model_transfer.backends.hixl.testing import (
    SUBMIT_NONTERMINAL,
    SUBMIT_REFUSE_PRE,
    SUBMIT_TIMEOUT_PRE,
    FakeHixlAPI,
)
from gpustack_model_transfer.core.budgets import ReadBudgets
from gpustack_model_transfer.core.errors import (
    AmbiguousReadError,
    OperationStateError,
    RangeOverflowError,
)
from gpustack_model_transfer.core.operation import OpState


def open_reader(
    api=None,
    ledger=None,
    direction=DIRECTION_WRITE,
    poll_deadline_seconds=0.05,
    peer=None,
    **budget_kwargs,
):
    api = api or FakeHixlAPI()
    manifest = hixlutil.load_manifest()
    if peer is None:
        hixlutil.publish_regions(api, manifest)
        peer = hixlutil.peer(manifest)
    limits = hixlutil.default_limits(**budget_kwargs)
    budgets = ReadBudgets(
        window_bytes=limits["window_bytes"],
        max_inflight=limits["max_inflight"],
        owned_output_bytes=limits["owned_output_bytes"],
    )
    return HixlReader(
        manifest,
        api,
        peer,
        budgets,
        direction=direction,
        poll_deadline_seconds=poll_deadline_seconds,
        ledger=ledger,
    )


class TestSessionWiring(unittest.TestCase):
    def test_registration_precedes_connection_in_construction(self):
        reader = open_reader()
        api = reader._api
        self.assertEqual(len(api.registrations), 1)
        self.assertEqual(len(api.connections), 1)
        reader.close()

    def test_close_disconnects_before_deregistration(self):
        reader = open_reader()
        api = reader._api
        reader.close()
        self.assertEqual(api.disconnects, 1)
        self.assertEqual(api.deregistrations, 1)
        self.assertEqual(reader.session.phase, "deregistered")

    def test_read_direction_submits_reads(self):
        reader = open_reader(direction=DIRECTION_READ)
        manifest = reader.manifest
        tensor = manifest.tensors[0]
        reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
        submission = reader._api.agents[0].submissions[0]
        self.assertEqual(submission["direction"], "read")
        self.assertIn("addr", submission["source"])
        self.assertNotIn("file", submission["source"])
        reader.close()

    def test_an_unknown_direction_is_refused_at_construction(self):
        with self.assertRaises(ValueError):
            open_reader(direction="both")


class TestPartialConstruction(unittest.TestCase):
    """A reader that fails to build strands nothing at the backend."""

    def test_a_failed_connect_deregisters_and_releases(self):
        from gpustack_model_transfer.backends.hixl.errors import TransferRefusedError

        api = FakeHixlAPI(connect_error=TransferRefusedError("the rendezvous refused"))
        hixlutil.publish_regions(api)
        ledger = MemoryLedger()
        with self.assertRaises(TransferRefusedError):
            open_reader(api=api, ledger=ledger)
        self.assertEqual(len(api.registrations), 1)
        self.assertEqual(api.deregistrations, 1, "the registration leaked")
        self.assertEqual(api.disconnects, 0)
        self.assertEqual(ledger.reserved_bytes, 0, "the reservation leaked")
        self.assertEqual(ledger.active_bytes, 0)

    def test_a_failed_registration_releases_the_reservation(self):
        from gpustack_model_transfer.backends.hixl.errors import TransferRefusedError

        api = FakeHixlAPI(register_error=TransferRefusedError("the pinning refused"))
        ledger = MemoryLedger()
        with self.assertRaises(TransferRefusedError):
            open_reader(api=api, ledger=ledger)
        self.assertEqual(api.registrations, [])
        self.assertEqual(api.connections, [])
        self.assertEqual(api.deregistrations, 0)
        self.assertEqual(ledger.reserved_bytes, 0, "the reservation leaked")


class TestReaderNegotiation(unittest.TestCase):
    """The reader carries its own requested/applied/effective record."""

    def test_unanswered_options_are_rejections_and_stay_functional_only(self):
        api = FakeHixlAPI()
        hixlutil.publish_regions(api)
        manifest = hixlutil.load_manifest()
        limits = hixlutil.default_limits()
        reader = HixlReader(
            manifest,
            api,
            hixlutil.peer(manifest),
            ReadBudgets(
                window_bytes=limits["window_bytes"],
                max_inflight=limits["max_inflight"],
                owned_output_bytes=limits["owned_output_bytes"],
            ),
            transport_options=("hixl_rdma", "tcp"),
        )
        record = reader.capabilities.negotiation
        record.validate()
        self.assertEqual(record.requested_transport_options, ("hixl_rdma", "tcp"))
        self.assertEqual(record.applied_transport_options, ())
        self.assertEqual(
            tuple(item.option for item in record.rejected),
            ("hixl_rdma", "tcp"),
        )
        self.assertTrue(reader.functional_only)
        reader.close()

    def test_answered_options_are_applied_on_the_reader_record(self):
        api = FakeHixlAPI(agent_applied_transport_options=("tcp",))
        hixlutil.publish_regions(api)
        manifest = hixlutil.load_manifest()
        limits = hixlutil.default_limits()
        reader = HixlReader(
            manifest,
            api,
            hixlutil.peer(manifest),
            ReadBudgets(
                window_bytes=limits["window_bytes"],
                max_inflight=limits["max_inflight"],
                owned_output_bytes=limits["owned_output_bytes"],
            ),
            transport_options=("hixl_rdma", "tcp"),
        )
        record = reader.capabilities.negotiation
        self.assertEqual(record.applied_transport_options, ("tcp",))
        self.assertEqual(record.rejected[0].option, "hixl_rdma")
        # Applied or not, the effective backend is unobserved: the
        # reader stays functional-only and claims no required path.
        self.assertTrue(reader.functional_only)
        reader.close()

    def test_the_default_reader_records_an_empty_negotiation(self):
        reader = open_reader()
        self.assertEqual(
            reader.capabilities.negotiation.requested_transport_options, ()
        )
        self.assertTrue(reader.functional_only)
        reader.close()


class TestWindowServing(unittest.TestCase):
    def test_a_multi_window_tensor_serves_the_real_fixture_slice(self):
        reader = open_reader()
        manifest = reader.manifest
        tensor = max(manifest.tensors, key=lambda t: t.byte_size)
        payload = reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
        blob = hixlutil.source_files()[tensor.file]
        expected = blob[tensor.data_begin : tensor.data_end]
        self.assertEqual(len(payload), tensor.byte_size)
        self.assertEqual(payload, expected)
        self.assertEqual(
            hashlib.sha256(payload).hexdigest(),
            hashlib.sha256(expected).hexdigest(),
        )
        reader.close()

    def test_a_second_read_serves_identical_bytes(self):
        reader = open_reader()
        manifest = reader.manifest
        tensor = manifest.tensors[0]
        first = reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
        second = reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
        self.assertEqual(first, second)
        reader.close()

    def test_a_shorter_trailing_window_does_not_leak_a_stale_tail(self):
        reader = open_reader()
        manifest = reader.manifest
        tensor = manifest.tensors[0]
        begin = tensor.data_begin
        full = reader.read_bytes(tensor.file, begin, begin + 32)
        short = reader.read_bytes(tensor.file, begin, begin + 8)
        self.assertEqual(short, full[:8])
        blob = hixlutil.source_files()[tensor.file]
        self.assertEqual(short, blob[begin : begin + 8])
        reader.close()

    def test_range_checks_match_the_manifest(self):
        reader = open_reader()
        manifest = reader.manifest
        tensor = manifest.tensors[0]
        size = manifest.file(tensor.file).size
        with self.assertRaises(RangeOverflowError):
            reader.read_bytes(tensor.file, size - 2, size + 1)
        with self.assertRaises(RangeOverflowError):
            reader.read_bytes(tensor.file, -1, 4)
        reader.close()

    def test_close_is_idempotent(self):
        reader = open_reader()
        manifest = reader.manifest
        tensor = manifest.tensors[0]
        reader.close()
        reader.close()
        with self.assertRaises(OperationStateError):
            reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)


class TestPerOperationStates(unittest.TestCase):
    def test_a_pre_submission_refusal_fails_cleanly_and_releases(self):
        api = FakeHixlAPI(submit_mode=SUBMIT_REFUSE_PRE)
        reader = open_reader(api=api, max_inflight=1)
        manifest = reader.manifest
        tensor = manifest.tensors[0]
        with self.assertRaises(TransferRefusedError):
            reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
        flight = list(reader.in_flight.values())[-1]
        self.assertEqual(flight.operation.state, OpState.FAILED)
        self.assertEqual(flight.operation.error_code, "pre-submission")
        # The same single-slot reader serves once the fault is cleared.
        api.submit_mode = "ok"
        payload = reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
        blob = hixlutil.source_files()[tensor.file]
        self.assertEqual(payload, blob[tensor.data_begin : tensor.data_end])
        self.assertEqual(reader.ledger.active_bytes, 0)
        reader.close()

    def test_a_pre_submission_timeout_fails_like_a_refusal(self):
        api = FakeHixlAPI(submit_mode=SUBMIT_TIMEOUT_PRE)
        reader = open_reader(api=api, max_inflight=1)
        manifest = reader.manifest
        tensor = manifest.tensors[0]
        with self.assertRaises(TransferRefusedError):
            reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
        flight = list(reader.in_flight.values())[-1]
        self.assertEqual(flight.operation.state, OpState.FAILED)
        self.assertEqual(reader.ledger.active_bytes, 0)
        reader.close()

    def test_a_nonterminal_submission_is_unknown_and_retained(self):
        api = FakeHixlAPI(submit_mode=SUBMIT_NONTERMINAL)
        reader = open_reader(api=api)
        manifest = reader.manifest
        tensor = manifest.tensors[0]
        with self.assertRaises(AmbiguousReadError):
            reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
        retained = [
            flight
            for flight in reader.in_flight.values()
            if flight.operation.state == OpState.UNKNOWN
        ]
        self.assertEqual(len(retained), 1)
        self.assertTrue(retained[-1].operation.retained)
        # The quarantine holds the bytes: they were never deactivated.
        self.assertFalse(retained[-1]._resolved)
        with self.assertRaises(OperationStateError):
            reader.close()

    def test_a_qualified_fence_resolves_and_releases_the_bytes(self):
        api = FakeHixlAPI(submit_mode=SUBMIT_NONTERMINAL)
        ledger = MemoryLedger()
        reader = open_reader(api=api, ledger=ledger, max_inflight=1)
        manifest = reader.manifest
        tensor = manifest.tensors[0]
        with self.assertRaises(AmbiguousReadError):
            reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
        self.assertGreater(ledger.active_bytes, 0, "the quarantine holds bytes")
        op_id = next(
            flight.operation.op_id
            for flight in reader.in_flight.values()
            if flight.operation.state == OpState.UNKNOWN
        )
        reader.resolve(op_id, "failed", error_code="fence-confirmed")
        self.assertEqual(ledger.active_bytes, 0, "the fence released the bytes")
        reader.close()

    def test_a_prior_success_never_eases_a_later_timeout(self):
        api = FakeHixlAPI()
        ledger = MemoryLedger()
        reader = open_reader(api=api, ledger=ledger, max_inflight=1)
        manifest = reader.manifest
        tensor = manifest.tensors[0]
        # A healthy read first.
        reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
        self.assertEqual(ledger.active_bytes, 0)
        # Then the fault appears: the earlier success changes nothing
        # about this timeout's retention.
        api.submit_mode = SUBMIT_NONTERMINAL
        with self.assertRaises(AmbiguousReadError):
            reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
        retained = [
            flight
            for flight in reader.in_flight.values()
            if flight.operation.state == OpState.UNKNOWN
        ]
        self.assertEqual(len(retained), 1)
        self.assertFalse(retained[-1]._resolved)
        self.assertGreater(ledger.active_bytes, 0)
        with self.assertRaises(OperationStateError):
            reader.close()
        op_id = retained[-1].operation.op_id
        reader.resolve(op_id, "failed", error_code="fence-confirmed")
        reader.close()


class TestLedgerAccounting(unittest.TestCase):
    def test_reservation_activity_and_release_are_tracked(self):
        ledger = MemoryLedger()
        reader = open_reader(ledger=ledger, max_inflight=2)
        self.assertEqual(ledger.reserved_bytes, 8 << 20)
        manifest = reader.manifest
        tensor = manifest.tensors[0]
        reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
        self.assertEqual(ledger.active_bytes, 0)
        self.assertGreater(ledger.peak_bytes, 0)
        reader.close()
        self.assertEqual(ledger.reserved_bytes, 0)

    def test_device_runtime_bytes_are_recorded_separately(self):
        api = FakeHixlAPI(device_memory_bytes_value=2 << 20)
        ledger = MemoryLedger()
        reader = open_reader(api=api, ledger=ledger)
        ledger.observe_device_runtime(api.device_memory_bytes(reader._session_agent))
        self.assertEqual(ledger.device_runtime_bytes, 2 << 20)
        reader.close()


if __name__ == "__main__":
    unittest.main()


class TestTeardownAndPollRecovery(unittest.TestCase):
    def test_close_resumes_after_deregistration_failure(self):
        api = FakeHixlAPI()
        reader = open_reader(api=api)
        original = api.deregister

        def refuse(*args):
            raise RuntimeError("retained")

        api.deregister = refuse
        with self.assertRaises(RuntimeError):
            reader.close()
        self.assertEqual(api.disconnects, 1)
        self.assertGreater(reader.ledger.reserved_bytes, 0)
        api.deregister = original
        reader.close()
        reader.close()
        self.assertEqual(api.disconnects, 1)
        self.assertEqual(api.deregistrations, 1)
        self.assertEqual(reader.ledger.reserved_bytes, 0)

    def test_poll_exception_is_observable_as_retained_unknown(self):
        api = FakeHixlAPI()
        reader = open_reader(api=api)

        def fail_poll(*args, **kwargs):
            raise RuntimeError("status unavailable")

        api.poll = fail_poll
        tensor = reader.manifest.tensors[0]
        with self.assertRaises(AmbiguousReadError):
            reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
        flight = next(iter(reader.in_flight.values()))
        self.assertEqual(flight.operation.state, OpState.UNKNOWN)
        self.assertGreater(reader.ledger.active_bytes, 0)
        with self.assertRaises(OperationStateError):
            reader.close()
        reader.resolve(flight.operation.op_id, "failed")
        reader.close()
        self.assertEqual(reader.ledger.reserved_bytes, 0)


def test_terminal_operation_history_is_bounded():
    reader = open_reader()
    tensor = reader.manifest.tensors[0]
    try:
        for _ in range(20):
            reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
        assert len(reader.in_flight) == 1
        assert not reader._slots._terminal_ops
    finally:
        reader.close()


def test_unknown_operation_survives_a_later_read():
    import pytest

    from gpustack_model_transfer.backends.hixl.testing import SUBMIT_OK

    api = FakeHixlAPI(submit_mode=SUBMIT_NONTERMINAL)
    reader = open_reader(api=api, max_inflight=2, poll_deadline_seconds=0)
    tensor = reader.manifest.tensors[0]
    with pytest.raises(AmbiguousReadError):
        reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
    flight = next(iter(reader.in_flight.values()))
    api.submit_mode = SUBMIT_OK
    try:
        reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
        assert reader.in_flight[flight.operation.op_id] is flight
        assert flight.operation.state == OpState.UNKNOWN
        with pytest.raises(OperationStateError):
            reader.close()
    finally:
        reader.resolve(flight.operation.op_id, "failed", error_code="test-fence")
        reader.close()


def test_fake_unknown_operation_is_pending():
    api = FakeHixlAPI()
    assert api.poll(None, "absent-operation").state == "pending"
