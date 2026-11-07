"""The control client against the node's source broker."""

import unittest

import sourceutil

from gpustack_model_transfer.source.control import ControlClient, ControlError


def client(broker, secret="worker-secret-1"):
    return ControlClient(
        broker.url, worker_id="source-worker-a", generation="7", secret=secret
    )


class TestControlClient(unittest.TestCase):
    def setUp(self):
        self.broker = sourceutil.FakeBroker()
        self.addCleanup(self.broker.close)

    def test_register_returns_lease_and_names_the_instance(self):
        control = client(self.broker)
        self.addCleanup(control.close)
        lease_id = control.register()
        self.assertEqual(lease_id, "lease-1")
        sent = self.broker.posts("/v1/register")
        self.assertEqual(sent[-1]["workerID"], "source-worker-a")
        self.assertEqual(sent[-1]["generation"], "7")
        self.assertEqual(sent[-1]["secret"], "worker-secret-1")
        # Every control message names the worker instance that sent it:
        # a fresh random id per native worker boot.
        self.assertTrue(sent[-1]["workerInstanceID"])

    def test_native_start_is_announced_before_native_work(self):
        control = client(self.broker)
        self.addCleanup(control.close)
        control.register()
        answer = control.native_start()
        self.assertEqual(answer.get("action"), "none")
        self.assertTrue(control.native_started)
        sent = self.broker.posts("/v1/native-start")
        self.assertEqual(len(sent), 1)
        self.assertEqual(sent[-1]["workerInstanceID"], control.worker_instance_id)
        self.assertEqual(sent[-1]["leaseID"], "lease-1")

    def test_heartbeat_carries_figures_and_returns_action(self):
        control = client(self.broker)
        self.addCleanup(control.close)
        control.register()
        result = control.heartbeat(
            resident_bytes=4096, registrations=2, active_sessions=1
        )
        self.assertEqual(result["action"], "none")
        sent = self.broker.posts("/v1/heartbeat")
        self.assertEqual(sent[-1]["residentBytes"], 4096)
        self.assertEqual(sent[-1]["registrations"], 2)
        self.assertEqual(sent[-1]["activeSessions"], 1)
        self.assertEqual(sent[-1]["leaseID"], "lease-1")
        self.assertIn("workerInstanceID", sent[-1])

    def test_filled_carries_descriptors_and_drain_ack_claims_release(self):
        control = client(self.broker)
        self.addCleanup(control.close)
        control.register()
        control.filled(
            registrations=1,
            descriptors=[
                {
                    "name": "model.safetensors",
                    "addr": 1,
                    "length": 4096,
                    "memoryType": "HOST",
                    "backend": "nixl",
                    "workerUID": "pod-uid-1",
                }
            ],
        )
        control.drain_ack(native_released=True)
        control.quarantine("unknown operation outcome")
        filled = self.broker.posts("/v1/filled")
        self.assertEqual(len(filled), 1)
        self.assertEqual(filled[-1]["registrations"], 1)
        self.assertEqual(filled[-1]["descriptors"][0]["addr"], 1)
        acked = self.broker.posts("/v1/drain-ack")
        self.assertEqual(len(acked), 1)
        self.assertTrue(acked[-1]["nativeReleased"])
        sent = self.broker.posts("/v1/quarantine")
        self.assertEqual(sent[-1]["reason"], "unknown operation outcome")

    def test_transport_failure_reads_as_unavailable(self):
        # A control address nothing answers: the wire failure is a
        # typed, retryable unavailable, not a broker refusal.
        control = ControlClient(
            "127.0.0.1:1",  # nothing listens there
            worker_id="source-worker-a",
            generation="7",
            secret="worker-secret-1",
            timeout=0.2,
        )
        self.addCleanup(control.close)
        with self.assertRaises(ControlError) as ctx:
            control.register()
        self.assertTrue(ctx.exception.transport)
        self.assertEqual(ctx.exception.status, 0)

    def test_refusal_raises_typed_error(self):
        self.broker.rules["/v1/register"] = {
            "status": 403,
            "body": {"kind": "authentication", "reason": "worker mismatch"},
        }
        control = client(self.broker, secret="wrong")
        self.addCleanup(control.close)
        with self.assertRaises(ControlError) as ctx:
            control.register()
        self.assertEqual(ctx.exception.status, 403)
        self.assertEqual(ctx.exception.kind, "authentication")
        # The refusal carries the reason, never the secret it was asked with.
        self.assertNotIn("wrong", str(ctx.exception))

    def test_repr_and_logs_never_carry_the_secret(self):
        control = client(self.broker)
        self.addCleanup(control.close)
        self.assertNotIn("worker-secret-1", repr(control))
        self.assertNotIn("worker-secret-1", str(control))

    def test_close_is_idempotent(self):
        control = client(self.broker)
        control.register()
        control.close()
        control.close()


if __name__ == "__main__":
    unittest.main()
