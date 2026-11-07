"""The generation-bound source worker over a verified published tree."""

import logging
import unittest
from io import StringIO

import sourceutil

from gpustack_model_transfer.source import control as control_module
from gpustack_model_transfer.source.worker import SourceWorker


def new_worker(broker, **worker_overrides):
    control = control_module.ControlClient(
        broker.url,
        worker_id="source-worker-a",
        generation="7",
        secret="worker-secret-1",
    )
    fields = {
        "control": control,
        "manifest_path": sourceutil.fixture_dir() / "manifest.json",
        "poll_interval": 0.01,
    }
    fields.update(worker_overrides)
    return SourceWorker(**fields)


class TestSourceWorkerLifecycle(unittest.TestCase):
    def setUp(self):
        self.broker = sourceutil.FakeBroker()
        self.addCleanup(self.broker.close)
        self.logs = StringIO()
        handler = logging.StreamHandler(self.logs)
        logger = logging.getLogger("gpustack_model_transfer.source")
        logger.addHandler(handler)
        logger.setLevel(logging.DEBUG)
        self.addCleanup(logger.removeHandler, handler)

    def test_start_registers_verifies_and_fills(self):
        worker = new_worker(self.broker)
        self.addCleanup(worker.close)
        worker.start()
        self.assertEqual(worker.state, "serving")
        registered = self.broker.posts("/v1/register")
        self.assertEqual(registered[-1]["generation"], "7")
        filled = self.broker.posts("/v1/filled")
        self.assertTrue(filled)

    def test_serve_returns_verified_fixture_bytes(self):
        worker = new_worker(self.broker)
        self.addCleanup(worker.close)
        worker.start()
        payload = worker.serve("model.norm.weight", 0, 12)
        expected = sourceutil.tensor_payload("model.norm.weight")
        self.assertEqual(payload, expected)
        # A partial range serves without claiming verification.
        partial = worker.serve("model.norm.weight", 0, 4)
        self.assertEqual(partial, expected[:4])

    def test_serve_refuses_out_of_bounds(self):
        from gpustack_model_transfer.core import errors

        worker = new_worker(self.broker)
        self.addCleanup(worker.close)
        worker.start()
        with self.assertRaises(errors.RangeOverflowError):
            worker.serve("model.norm.weight", 8, 16)

    def test_heartbeat_once_keeps_serving(self):
        worker = new_worker(self.broker)
        self.addCleanup(worker.close)
        worker.start()
        action = worker.once()
        self.assertEqual(action, "none")
        self.assertEqual(worker.state, "serving")

    def test_drain_action_stops_serving_and_acks(self):
        self.broker.rules["/v1/heartbeat"] = {
            "body": {"action": "drain", "expiresAt": "2031-01-01T00:00:00Z"}
        }
        worker = new_worker(self.broker)
        self.addCleanup(worker.close)
        worker.start()
        action = worker.once()
        self.assertEqual(action, "drain")
        self.assertEqual(worker.state, "drained")
        self.assertTrue(self.broker.posts("/v1/drain-ack"))
        # A drained worker no longer serves.
        from gpustack_model_transfer.core import errors

        with self.assertRaises(errors.OperationStateError):
            worker.serve("model.norm.weight", 0, 12)

    def test_a_refused_drain_acknowledgement_retries_on_later_cycles(self):
        # The broker refuses the acknowledgement while sessions drain and
        # keeps answering "drain"; the worker must retry on every cycle
        # and keep its lease alive, not exit mid-drain.
        self.broker.rules["/v1/heartbeat"] = {
            "body": {"action": "drain", "expiresAt": "2031-01-01T00:00:00Z"}
        }

        def drain_ack_reply(body):
            if len(self.broker.posts("/v1/drain-ack")) < 2:
                return 409, {
                    "kind": "operation-state",
                    "reason": "sessions are still draining",
                }
            return 200, {"expiresAt": "2031-01-01T00:00:00Z"}

        self.broker.rules["/v1/drain-ack"] = drain_ack_reply
        worker = new_worker(self.broker)
        self.addCleanup(worker.close)
        worker.start()
        self.assertEqual(worker.once(), "drain")
        self.assertEqual(worker.state, "draining")
        self.assertEqual(worker.once(), "drain")
        self.assertEqual(worker.state, "drained")
        acknowledged = self.broker.posts("/v1/drain-ack")
        self.assertGreaterEqual(len(acknowledged), 2)
        # The retrying cycles kept heartbeating, so the lease stayed alive.
        self.assertGreaterEqual(len(self.broker.posts("/v1/heartbeat")), 2)

    def test_run_loops_through_a_pending_drain_to_drained(self):
        import threading
        import time

        self.broker.rules["/v1/heartbeat"] = {
            "body": {"action": "drain", "expiresAt": "2031-01-01T00:00:00Z"}
        }

        def drain_ack_reply(body):
            if len(self.broker.posts("/v1/drain-ack")) < 3:
                return 409, {
                    "kind": "operation-state",
                    "reason": "sessions are still draining",
                }
            return 200, {"expiresAt": "2031-01-01T00:00:00Z"}

        self.broker.rules["/v1/drain-ack"] = drain_ack_reply
        worker = new_worker(self.broker)
        self.addCleanup(worker.close)
        worker.start()
        stop = threading.Event()
        done = []
        thread = threading.Thread(
            target=lambda: done.append(worker.run(stop)), daemon=True
        )
        thread.start()
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline and not done:
            time.sleep(0.01)
        thread.join(timeout=5)
        self.assertTrue(done, "the worker never finished the drain")
        self.assertEqual(done[0], "drain")
        self.assertEqual(worker.state, "drained")
        stop.set()

    def test_stale_generation_stops_the_worker(self):
        self.broker.rules["/v1/heartbeat"] = {
            "status": 409,
            "body": {
                "kind": "stale-generation",
                "reason": "the generation does not exist",
            },
        }
        worker = new_worker(self.broker)
        self.addCleanup(worker.close)
        worker.start()
        action = worker.once()
        self.assertEqual(action, "stale")
        self.assertEqual(worker.state, "stopped")
        from gpustack_model_transfer.core import errors

        with self.assertRaises(errors.OperationStateError):
            worker.serve("model.norm.weight", 0, 12)

    def test_unknown_operation_quarantines_and_retains(self):
        worker = new_worker(self.broker)
        self.addCleanup(worker.close)
        worker.start()
        worker.report_unknown("backend state not terminal at deadline")
        self.assertEqual(worker.state, "quarantined")
        sent = self.broker.posts("/v1/quarantine")
        self.assertEqual(sent[-1]["reason"], "backend state not terminal at deadline")
        from gpustack_model_transfer.core import errors

        with self.assertRaises(errors.OperationStateError):
            worker.serve("model.norm.weight", 0, 12)

    def test_status_is_redacted(self):
        worker = new_worker(self.broker)
        self.addCleanup(worker.close)
        worker.start()
        status = worker.redacted_status()
        self.assertEqual(status["generation"], "7")
        self.assertEqual(status["state"], "serving")
        self.assertNotIn("worker-secret-1", str(status))
        self.assertNotIn("secret", status)

    def test_logs_never_carry_the_secret(self):
        worker = new_worker(self.broker)
        self.addCleanup(worker.close)
        worker.start()
        worker.once()
        worker.report_unknown("probe")
        worker.close()
        self.assertNotIn("worker-secret-1", self.logs.getvalue())

    def test_run_drains_through_the_loop(self):
        import threading
        import time

        def heartbeat_reply(body):
            if len(self.broker.posts("/v1/heartbeat")) >= 2:
                return 200, {"action": "drain", "expiresAt": "2031-01-01T00:00:00Z"}
            return 200, {"action": "none", "expiresAt": "2031-01-01T00:00:00Z"}

        self.broker.rules["/v1/heartbeat"] = heartbeat_reply
        worker = new_worker(self.broker)
        self.addCleanup(worker.close)
        worker.start()
        stop = threading.Event()
        done = []
        thread = threading.Thread(
            target=lambda: done.append(worker.run(stop)), daemon=True
        )
        thread.start()
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline and not done:
            time.sleep(0.01)
        thread.join(timeout=5)
        self.assertTrue(done)
        self.assertEqual(worker.state, "drained")
        stop.set()

    def test_start_closes_session_when_filled_fails(self):
        self.broker.rules["/v1/filled"] = {
            "status": 409,
            "body": {
                "kind": "operation-state",
                "reason": "only a filling generation starts serving",
            },
        }
        worker = new_worker(self.broker, registrations=2, active_sessions=3)
        self.addCleanup(worker.close)
        with self.assertRaises(control_module.ControlError):
            worker.start()
        self.assertIsNone(worker._session)
        self.assertEqual(worker.state, "starting")

    def test_heartbeat_forwards_configured_registrations_and_sessions(self):
        worker = new_worker(self.broker, registrations=2, active_sessions=3)
        self.addCleanup(worker.close)
        worker.start()
        filled = self.broker.posts("/v1/filled")
        self.assertEqual(filled[-1]["registrations"], 2)
        worker.once()
        beats = self.broker.posts("/v1/heartbeat")
        self.assertEqual(beats[-1]["registrations"], 2)
        self.assertEqual(beats[-1]["activeSessions"], 3)


if __name__ == "__main__":
    unittest.main()
