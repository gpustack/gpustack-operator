"""Session-owned HIXL connections, exercised through the source journal."""

import threading
from types import SimpleNamespace

import pytest

from gpustack_model_transfer.core.descriptors import FileDescriptor, TensorDescriptor
from gpustack_model_transfer.core.manifest import Manifest
from gpustack_model_transfer.source.backend import HixlSourceBackend, SourceRegistration
from gpustack_model_transfer.source.transfer_service import OperationRegistry, Refused


class Engine:
    def __init__(self, *, fail_disconnect=False, fail_post=False):
        self.calls = []
        self.fail_disconnect = fail_disconnect
        self.fail_post = fail_post

    def connect(self, endpoint):
        self.calls.append(("connect", endpoint, threading.get_ident()))
        return 0

    def transfer_async(self, endpoint, direction, descriptors):
        self.calls.append(("write", endpoint, threading.get_ident()))
        return int(self.fail_post), len(self.calls)

    def get_transfer_status(self, handle):
        return 0, "completed"

    def disconnect(self, endpoint):
        self.calls.append(("disconnect", endpoint, threading.get_ident()))
        return int(self.fail_disconnect)

    def deregister_mem(self, handle):
        self.calls.append(("revoke", handle, threading.get_ident()))
        return 0


class Worker:
    def __init__(self, engine):
        self.backend = HixlSourceBackend()
        self.backend._engine = engine
        self.backend._hixl = SimpleNamespace(
            SUCCESS=0,
            TransferOp=SimpleNamespace(WRITE="WRITE"),
            TransferStatus=SimpleNamespace(COMPLETED="completed", FAILED="failed"),
            TransferOpDesc=lambda *args: args,
        )
        block = SimpleNamespace(data_ptr=lambda: 1000, byte_size=64)
        self.blocks = {"file": block}
        self.registration = SourceRegistration("hixl", {"file": 1}, self.blocks)
        manifest = Manifest(
            2,
            "artifact",
            1,
            (FileDescriptor("file", 64),),
            (TensorDescriptor("tensor", "file", "U8", (64,), 0, 64),),
            "a" * 64,
        )
        self.assignment = SimpleNamespace(
            backend="hixl", manifest=manifest, paged=False
        )
        self.control = SimpleNamespace(worker_id="source", lease_id="lease")
        self.worker_instance_id = "source-process"
        self._lock = threading.Lock()
        self._drain_requested = threading.Event()
        self._drain_started = False
        self._release_done = False
        self._unknown_event = threading.Event()
        self._blocks_idle = False
        self.woken = threading.Event()
        self.native_operations = 0
        self.state = "serving"
        self.registry = OperationRegistry(
            SimpleNamespace(
                workers={"generation": self}, _admission_closed=threading.Event()
            )
        )

    def wake(self):
        self.woken.set()

    def report_unknown(self, reason, stage=None):
        self.state = "quarantined"
        self._unknown_event.set()
        self.registry.mark_unknown(self)


def request(number=0, **changes):
    body = {
        "sourcePodUID": "source",
        "workerInstanceID": "source-process",
        "generation": "generation",
        "leaseID": "lease",
        "sessionID": "session",
        "requestID": f"request-{number}",
        "targetPodUID": "target",
        "targetInstanceID": "target-process",
        "registrationID": "registration",
        "windowID": "window",
        "tensor": "tensor",
        "file": "file",
        "fileOffset": 0,
        "tensorOffset": 0,
        "length": 8,
        "target": {
            "backend": "hixl",
            "address": 2000,
            "length": 64,
            "metadata": "",
            "endpoint": "127.0.0.1:1234",
        },
    }
    return body | changes


def common(body):
    return {
        key: body[key]
        for key in (
            "sourcePodUID",
            "workerInstanceID",
            "generation",
            "leaseID",
            "sessionID",
            "requestID",
        )
    }


def acknowledgement(body):
    return common(body) | {
        key: body[key]
        for key in (
            "targetPodUID",
            "targetInstanceID",
            "registrationID",
            "windowID",
        )
    }


def transfer(worker, body):
    registry = worker.registry
    registry.submit(body)
    registry.step(worker)
    registry.step(worker)
    answer = registry.status(common(body))
    assert answer["state"] == "terminal"
    registry.acknowledge(
        common(body)
        | {
            key: body[key]
            for key in (
                "targetPodUID",
                "targetInstanceID",
                "registrationID",
                "windowID",
            )
        }
    )
    registry.step(worker)
    assert registry.status(common(body))["state"] == "acknowledged"
    return answer["verdict"]


def close_session(worker, body):
    result = []
    worker.woken.clear()
    thread = threading.Thread(
        target=lambda: result.append(worker.registry.close_session(common(body)))
    )
    thread.start()
    assert worker.woken.wait(1)
    worker.registry.step(worker)
    thread.join(timeout=2)
    assert not thread.is_alive()
    return result[0]


@pytest.mark.parametrize("count", [2, 5])
def test_same_window_reuses_one_connection_until_session_close(count):
    engine = Engine()
    worker = Worker(engine)
    for number in range(count):
        assert transfer(worker, request(number)) == "succeeded"
    assert [call[0] for call in engine.calls].count("connect") == 1
    assert [call[0] for call in engine.calls].count("disconnect") == 0
    assert worker.native_operations == 0
    assert close_session(worker, request()) == {"state": "closed"}
    assert [call[0] for call in engine.calls].count("disconnect") == 1
    assert worker.registry.close_session(common(request())) == {"state": "closed"}
    assert worker.native_operations == 0
    assert all(call[2] == threading.get_ident() for call in engine.calls)


def test_session_disconnect_failure_retains_native_ownership():
    engine = Engine(fail_disconnect=True)
    worker = Worker(engine)
    assert transfer(worker, request()) == "succeeded"
    assert close_session(worker, request()) == {"state": "closing"}
    assert worker.state == "quarantined"
    assert worker.native_operations == 1
    assert worker.registration.handles and worker.blocks
    assert worker.registry.close_session(common(request())) == {"state": "closing"}
    assert [call[0] for call in engine.calls].count("disconnect") == 1


@pytest.mark.parametrize(
    "changes",
    [
        {"registrationID": "different-registration"},
        {"windowID": "different-window"},
        {"sessionID": "different-session"},
        {"target": request()["target"] | {"address": 3000}},
    ],
)
def test_existing_endpoint_cannot_be_reused_with_a_different_owner(changes):
    engine = Engine()
    worker = Worker(engine)
    assert transfer(worker, request()) == "succeeded"
    assert transfer(worker, request(1, **changes)) == "not-posted"
    assert [call[0] for call in engine.calls] == ["connect", "write"]
    assert close_session(worker, request()) == {"state": "closed"}


def test_registration_revocation_disconnects_idle_peers_before_deregistration():
    engine = Engine()
    worker = Worker(engine)
    assert transfer(worker, request()) == "succeeded"
    worker.backend.revoke(worker.registration)
    assert [call[0] for call in engine.calls] == [
        "connect",
        "write",
        "disconnect",
        "revoke",
    ]
    worker.registration = SourceRegistration("hixl", {"file": 2}, worker.blocks)
    assert transfer(worker, request(1)) == "succeeded"
    assert [call[0] for call in engine.calls].count("connect") == 2
    assert close_session(worker, request()) == {"state": "closed"}


def test_unknown_post_retains_connection_and_blocks_session_close():
    engine = Engine(fail_post=True)
    worker = Worker(engine)
    worker.registry.submit(request())
    worker.registry.step(worker)
    assert worker.registry.status(common(request()))["state"] == "unknown"
    assert worker.registry.close_session(common(request())) == {"state": "closing"}
    assert worker.native_operations == 1
    assert worker.backend.has_active_peers()
    assert [call[0] for call in engine.calls] == ["connect", "write"]


def test_session_close_waits_for_consumer_acknowledgement():
    engine = Engine()
    worker = Worker(engine)
    body = request()
    worker.registry.submit(body)
    worker.registry.step(worker)
    worker.registry.step(worker)
    assert worker.registry.close_session(common(body)) == {"state": "closing"}
    assert [call[0] for call in engine.calls] == ["connect", "write"]
    worker.registry.acknowledge(
        common(body)
        | {
            key: body[key]
            for key in (
                "targetPodUID",
                "targetInstanceID",
                "registrationID",
                "windowID",
            )
        }
    )
    worker.registry.step(worker)
    assert close_session(worker, body) == {"state": "closed"}
    assert worker.native_operations == 0


@pytest.mark.parametrize("sessions", [1, 2, 8])
def test_concurrent_session_closes_release_each_window_once(sessions):
    engine = Engine()
    worker = Worker(engine)
    bodies = [
        request(
            2 * session + window,
            sessionID=f"session-{session}",
            windowID=f"window-{window}",
            target=request()["target"]
            | {"endpoint": f"127.0.0.1:{1234 + 2 * session + window}"},
        )
        for session in range(sessions)
        for window in range(2)
    ]
    for body in bodies:
        assert transfer(worker, body) == "succeeded"
    results = []
    barrier = threading.Barrier(sessions + 1)

    def close(body):
        barrier.wait(timeout=2)
        results.append(worker.registry.close_session(common(body)))

    threads = [threading.Thread(target=close, args=(body,)) for body in bodies[::2]]
    worker.woken.clear()
    for thread in threads:
        thread.start()
    barrier.wait(timeout=2)
    try:
        for _ in range(sessions):
            assert worker.woken.wait(2)
            worker.woken.clear()
            while worker.registry.step(worker):
                pass
            if len(results) == sessions:
                break
            with worker.registry.lock:
                if all(
                    s["state"] == "closed" for s in worker.registry.sessions.values()
                ):
                    break
    finally:
        for thread in threads:
            thread.join(timeout=5)
    assert all(not thread.is_alive() for thread in threads)
    assert results == [{"state": "closed"}] * sessions
    assert worker.native_operations == 0
    disconnected = [call[1] for call in engine.calls if call[0] == "disconnect"]
    assert sorted(disconnected) == sorted(body["target"]["endpoint"] for body in bodies)
    assert all(call[2] == threading.get_ident() for call in engine.calls)


def test_session_close_timeout_keeps_ownership_until_native_thread_finishes():
    engine = Engine()
    worker = Worker(engine)
    body = request()
    assert transfer(worker, body) == "succeeded"
    assert worker.registry.close_session(common(body)) == {"state": "closing"}
    assert worker.native_operations == 1
    assert worker.registration.handles and worker.blocks
    assert [call[0] for call in engine.calls] == ["connect", "write"]
    with pytest.raises(Refused, match="session is closing"):
        worker.registry.submit(request(1))
    worker.registry.step(worker)
    for _ in range(3):
        assert worker.registry.close_session(common(body)) == {"state": "closed"}
    assert worker.native_operations == 0
    assert [call[0] for call in engine.calls].count("disconnect") == 1


def test_late_acknowledgement_does_not_release_the_next_use_of_a_window():
    engine = Engine()
    worker = Worker(engine)
    first, second = request(), request(1)
    assert transfer(worker, first) == "succeeded"
    worker.registry.submit(second)
    worker.registry.step(worker)
    worker.registry.step(worker)
    worker.registry.acknowledge(acknowledgement(first))
    worker.registry.step(worker)
    assert worker.registry.status(common(second))["state"] == "terminal"
    assert worker.native_operations == 1
    assert worker.registry.close_session(common(second)) == {"state": "closing"}
    assert [call[0] for call in engine.calls] == ["connect", "write", "write"]
    worker.registry.acknowledge(acknowledgement(second))
    worker.registry.step(worker)
    assert close_session(worker, second) == {"state": "closed"}
    assert worker.native_operations == 0
