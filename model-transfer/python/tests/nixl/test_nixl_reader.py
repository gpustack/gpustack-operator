"""The NIXL reader: windows, states, quarantine, accounting, renewal."""

import json
import unittest

import nixlutil

from gpustack_model_transfer.backends.nixl import (
    NixlReader,
    qualify_endpoints,
)
from gpustack_model_transfer.backends.nixl.errors import TransferRefusedError
from gpustack_model_transfer.backends.nixl.memory import MemoryLedger
from gpustack_model_transfer.backends.nixl.testing import (
    SUBMIT_NONTERMINAL,
    SUBMIT_REFUSE_PRE,
    FakeNixlAPI,
)
from gpustack_model_transfer.core.budgets import ReadBudgets
from gpustack_model_transfer.core.errors import (
    AmbiguousReadError,
    OperationStateError,
    RangeOverflowError,
)
from gpustack_model_transfer.core.operation import OpState


def fixture_source_files() -> dict:
    """The fixture's real checkpoint bytes, keyed as the reader names them."""
    blob = (nixlutil.fixture_dir() / "checkpoint.safetensors").read_bytes()
    return {"checkpoint.safetensors": blob}


def open_reader(
    api=None, ledger=None, budgets=None, poll_deadline_seconds=0.05, **budget_kwargs
):
    api = api or FakeNixlAPI(source_files=fixture_source_files())
    manifest = nixlutil.load_manifest()
    budgets = budgets or ReadBudgets(
        window_bytes=nixlutil.default_limits(**budget_kwargs)["window_bytes"],
        max_inflight=nixlutil.default_limits(**budget_kwargs)["max_inflight"],
        owned_output_bytes=nixlutil.default_limits(**budget_kwargs)[
            "owned_output_bytes"
        ],
    )
    selection = qualify_endpoints("tcp://10.0.0.1:9100", ["mlx5_0:1"], {})
    reader = NixlReader(
        manifest,
        api,
        {"source": "peer"},
        budgets,
        selection,
        ledger=ledger,
        poll_deadline_seconds=poll_deadline_seconds,
    )
    return reader, api, manifest


class TestFixedWindowReuse(unittest.TestCase):
    def test_a_multi_window_tensor_serves_byte_exact_twice(self):
        import hashlib

        reader, _api, manifest = open_reader()
        tensor = max(manifest.tensors, key=lambda t: t.byte_size)
        first = reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
        second = reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
        self.assertEqual(len(first), tensor.byte_size)
        self.assertEqual(first, second)
        # The bytes are the fixture's own slice, byte for byte, and the
        # digest of what the reader returned is the digest of the slice.
        blob = fixture_source_files()[tensor.file]
        expected = blob[tensor.data_begin : tensor.data_end]
        self.assertEqual(first, expected)
        self.assertEqual(
            hashlib.sha256(first).hexdigest(), hashlib.sha256(expected).hexdigest()
        )
        reader.close()

    def test_a_smaller_trailing_window_does_not_leak_the_previous_tail(self):
        reader, _api, manifest = open_reader()
        tensor = manifest.tensors[0]
        begin = tensor.data_begin
        # Fill a window completely, then read a shorter span from the same
        # start: the returned bytes must be the shorter slice alone.
        full = reader.read_bytes(tensor.file, begin, begin + 32)
        short = reader.read_bytes(tensor.file, begin, begin + 8)
        self.assertEqual(short, full[:8])
        blob = fixture_source_files()[tensor.file]
        self.assertEqual(short, blob[begin : begin + 8])
        reader.close()

    def test_windows_are_fixed_and_few(self):
        reader, _api, _manifest = open_reader(max_inflight=2)
        self.assertEqual(len(reader.windows), 2)
        self.assertEqual(len(reader.windows[0]), 32)
        reader.close()

    def test_range_checks_match_the_manifest(self):
        reader, _api, manifest = open_reader()
        tensor = manifest.tensors[0]
        size = manifest.file(tensor.file).size
        with self.assertRaises(RangeOverflowError):
            reader.read_bytes(tensor.file, size - 2, size + 1)
        with self.assertRaises(RangeOverflowError):
            reader.read_bytes(tensor.file, -1, 4)
        with self.assertRaises(RangeOverflowError):
            reader.read_bytes(tensor.file, 4, 2)
        reader.close()

    def test_close_is_idempotent_and_closes_the_reader(self):
        reader, _api, manifest = open_reader()
        tensor = manifest.tensors[0]
        reader.close()
        reader.close()
        with self.assertRaises(OperationStateError):
            reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)


class TestPerOperationStates(unittest.TestCase):
    def test_pre_submission_refusal_fails_cleanly_and_releases_the_slot(self):
        api = FakeNixlAPI(
            submit_mode=SUBMIT_REFUSE_PRE, source_files=fixture_source_files()
        )
        # One window slot: a refusal that kept it would wedge every later
        # read behind capacity exhaustion.
        reader, _api, manifest = open_reader(api=api, max_inflight=1)
        tensor = manifest.tensors[0]
        with self.assertRaises(TransferRefusedError):
            reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
        operation = list(reader.operations.values())[-1]
        self.assertEqual(operation.state, OpState.FAILED)
        self.assertEqual(operation.error_code, "pre-submission")
        # The same reader, slot released and bytes deactivated, serves a
        # healthy read after the failure mode is cleared.
        api.submit_mode = "ok"
        payload = reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
        blob = fixture_source_files()[tensor.file]
        self.assertEqual(payload, blob[tensor.data_begin : tensor.data_end])
        self.assertEqual(reader.ledger.active_bytes, 0)
        reader.close()

    def test_nonterminal_submission_is_unknown_and_retained(self):
        api = FakeNixlAPI(submit_mode=SUBMIT_NONTERMINAL)
        reader, _api, manifest = open_reader(api=api)
        tensor = manifest.tensors[0]
        with self.assertRaises(AmbiguousReadError):
            reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
        unknown = [
            op for op in reader.operations.values() if op.state == OpState.UNKNOWN
        ]
        self.assertEqual(len(unknown), 1)
        self.assertTrue(unknown[0].retained)
        with self.assertRaises(OperationStateError):
            reader.close()

    def test_a_qualified_fence_releases_the_retained_window(self):
        api = FakeNixlAPI(submit_mode=SUBMIT_NONTERMINAL)
        reader, _api, manifest = open_reader(api=api)
        tensor = manifest.tensors[0]
        with self.assertRaises(AmbiguousReadError):
            reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
        op_id = next(
            op.op_id for op in reader.operations.values() if op.state == OpState.UNKNOWN
        )
        reader.resolve(op_id, "failed", error_code="fence-confirmed")
        reader.close()

    def test_each_operation_keeps_its_own_state(self):
        reader, _api, manifest = open_reader()
        tensor = manifest.tensors[0]
        reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
        operations = list(reader.operations.values())
        self.assertGreaterEqual(len(operations), 1)
        for operation in operations:
            self.assertEqual(operation.state, OpState.SUCCEEDED)
            self.assertFalse(operation.retained)
        reader.close()


class TestRegistrationRenewal(unittest.TestCase):
    def test_an_expired_registration_is_renewed_in_place(self):
        api = FakeNixlAPI(
            expire_registration_after=1, source_files=fixture_source_files()
        )
        reader, _api, manifest = open_reader(api=api)
        tensor = manifest.tensors[0]
        first = reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
        second = reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
        self.assertEqual(first, second)
        self.assertGreaterEqual(reader.registration_renewals, 1)
        self.assertGreaterEqual(api.renewals, 1)
        reader.close()


class TestMemoryAccounting(unittest.TestCase):
    def test_the_ledger_tracks_reservation_activity_and_release(self):
        ledger = MemoryLedger()
        reader, _api, manifest = open_reader(ledger=ledger, max_inflight=2)
        reservation = 32 * 2
        self.assertEqual(ledger.reserved_bytes, reservation)
        tensor = manifest.tensors[0]
        reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
        self.assertEqual(ledger.active_bytes, 0, "in-flight bytes survived")
        self.assertEqual(ledger.peak_bytes, 32)
        reader.close()
        self.assertEqual(ledger.reserved_bytes, 0)

    def test_snapshot_reports_the_peak_reservation(self):
        # Regression: snapshot() once read self.peak_reserved, which never
        # existed — the property is peak_reserved_bytes — so every report
        # call crashed with AttributeError.
        ledger = MemoryLedger()
        ledger.reserve(64)
        ledger.reserve(32)
        self.assertEqual(ledger.snapshot()["peak_reserved_bytes"], 96)
        ledger.release_reservation(96)
        self.assertEqual(ledger.snapshot()["peak_reserved_bytes"], 96)

    def test_device_runtime_bytes_are_recorded_separately(self):
        api = FakeNixlAPI(device_memory_bytes_value=1 << 20)
        ledger = MemoryLedger()
        reader, _api, _manifest = open_reader(api=api, ledger=ledger)
        ledger.observe_device_runtime(api.device_memory_bytes(reader._agent))
        self.assertEqual(ledger.device_runtime_bytes, 1 << 20)
        reader.close()

    def test_activation_above_the_reservation_is_refused(self):
        ledger = MemoryLedger()
        ledger.reserve(64)
        with self.assertRaises(Exception):
            ledger.activate(128)


class TestFakeSourceTargetFlow(unittest.TestCase):
    """The fake's source/target primitives model the source-push and
    target-pull between connected agents — the flow the source supervisor
    wires — with bytes landing exactly where the direction says."""

    def connected_agents(self):
        api = FakeNixlAPI()
        source = api.create_agent("source", {})
        target = api.create_agent("target", {})
        source_registration = api.register_memory(
            source, {"window-0": bytearray(b"source window bytes")}
        )
        target_registration = api.register_memory(target, {"in": bytearray(64)})
        metadata = api.agent_metadata(target)
        connection = api.connect_peer(source, metadata)
        region = json.loads(metadata.decode())["regions"][0]
        remote = {
            "addr": region["addr"],
            "len": region["len"],
            "mem_type": region["mem_type"],
        }
        return api, source, source_registration, target_registration, connection, remote

    def test_a_source_write_pushes_window_bytes_into_the_peer_region(self):
        (
            api,
            source,
            source_registration,
            target_registration,
            connection,
            remote,
        ) = self.connected_agents()
        operation = api.submit_source_write(
            source, source_registration, connection, "window-0", remote, 14
        )
        outcome = api.poll(source, operation)
        self.assertEqual(outcome.state, "succeeded")
        landing = target_registration.buffers["in"]
        self.assertEqual(bytes(landing[:14]), b"source window ")
        self.assertEqual(bytes(landing[14:]), bytes(64 - 14), "landing ran past length")

    def test_a_target_read_pulls_the_peer_bytes_into_the_window(self):
        # The target-initiated direction: the TARGET imports the SOURCE's
        # metadata and pulls the source's published bytes into its own
        # registered window.
        api = FakeNixlAPI()
        source = api.create_agent("source", {})
        target = api.create_agent("target", {})
        api.register_memory(source, {"window-0": bytearray(b"source window bytes")})
        target_registration = api.register_memory(target, {"in": bytearray(64)})
        metadata = api.agent_metadata(source)
        connection = api.connect_peer(target, metadata)
        region = json.loads(metadata.decode())["regions"][0]
        remote = {
            "addr": region["addr"],
            "len": region["len"],
            "mem_type": region["mem_type"],
        }
        operation = api.submit_target_read(
            target, target_registration, connection, "in", remote, 6
        )
        outcome = api.poll(target, operation)
        self.assertEqual(outcome.state, "succeeded")
        pulled = target_registration.buffers["in"]
        self.assertEqual(bytes(pulled[:6]), b"source")

    def test_a_disconnected_peer_refuses_further_transfers(self):
        (
            api,
            source,
            source_registration,
            _target_registration,
            connection,
            remote,
        ) = self.connected_agents()
        api.disconnect_peer(source, connection)
        with self.assertRaises(ValueError):
            api.submit_source_write(
                source, source_registration, connection, "window-0", remote, 6
            )

    def test_release_operation_releases_exactly_once(self):
        from gpustack_model_transfer.backends.nixl.errors import FenceRequiredError

        (
            api,
            source,
            source_registration,
            _target_registration,
            connection,
            remote,
        ) = self.connected_agents()
        operation = api.submit_target_read(
            source, source_registration, connection, "window-0", remote, 6
        )
        # A pending operation's release is a cancel, not a qualified
        # fence: normal cleanup is refused until evidence is provided.
        with self.assertRaises(FenceRequiredError):
            api.release_operation(source, operation)
        api.release_operation(source, operation, external_fence="host abort record #21")
        with self.assertRaises(ValueError):
            api.release_operation(
                source, operation, external_fence="host abort record #21"
            )
        with self.assertRaises(ValueError):
            api.poll(source, operation)


if __name__ == "__main__":
    unittest.main()


def test_terminal_operation_history_is_bounded():
    reader, _, _ = open_reader()
    tensor = reader.manifest.tensors[0]
    try:
        for _ in range(20):
            reader.read_bytes(tensor.file, tensor.data_begin, tensor.data_end)
        assert len(reader.operations) == 1
        assert not reader._slots._terminal_ops
    finally:
        reader.close()
