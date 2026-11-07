"""Private source journal: replays, ranges and ownership without native devices."""

import http.client
import json
import threading

import pytest
from test_native_source import eventually, order, supervisor

from gpustack_model_transfer.source.transfer_service import PrivateServer, Refused


class FakeWrite:
    def __init__(self, *, fail=False):
        self.posts = 0
        self.closes = 0
        self.fail = fail

    def post_write(self, registration, name, offset, length, target):
        self.posts += 1
        if self.fail:
            raise RuntimeError("may have posted")
        return bytes(registration.blocks[name].data[offset : offset + length])

    def poll_write(self, operation):
        return "succeeded"

    def close_write(self, operation):
        self.closes += 1


class StepWorker:
    """Minimal identity-hashable stand-in for the registry's step loop."""

    def __init__(self, backend, registration, reasons):
        self.backend = backend
        self.registration = registration
        self.native_operations = 1
        self.reasons = reasons

    def report_unknown(self, reason, stage=None):
        self.reasons.append(reason)


def setup(fail=False):
    source, controls, backends, _ = supervisor([order()])
    source.poll_once()
    eventually(lambda: source.workers["generation-1"].wait_until_filled(0))
    worker = source.workers["generation-1"]
    worker.control.lease_id = "lease"
    backend = backends["generation-1"]
    writer = FakeWrite(fail=fail)
    backend.post_write, backend.poll_write, backend.close_write = (
        writer.post_write,
        writer.poll_write,
        writer.close_write,
    )
    tensor = worker.assignment.manifest.tensors[0]
    body = {
        "sourcePodUID": "pod-uid",
        "workerInstanceID": "instance-1",
        "generation": "generation-1",
        "leaseID": "lease",
        "sessionID": "session",
        "requestID": "request",
        "targetPodUID": "target",
        "targetInstanceID": "process",
        "registrationID": "registration",
        "windowID": "window",
        "tensor": tensor.name,
        "file": tensor.file,
        "fileOffset": tensor.data_begin + 1,
        "tensorOffset": 1,
        "length": 4,
        "target": {
            "backend": "nixl",
            "address": 123,
            "length": 16,
            "metadata": "dGFyZ2V0",
            "endpoint": "",
        },
    }
    return source, controls, worker, writer, body


@pytest.mark.parametrize(
    ("count_limit", "byte_adjustment", "accepted"),
    [(1, 0, True), (0, 0, False), (1, -1, False)],
)
def test_journal_bounds_preserve_replays_and_ownership(
    monkeypatch, count_limit, byte_adjustment, accepted
):
    import gpustack_model_transfer.source.transfer_service as service

    source, _controls, worker, _writer, body = setup()
    encoded_size = len(json.dumps(body, sort_keys=True, separators=(",", ":")).encode())
    monkeypatch.setattr(service, "MAX_JOBS", count_limit)
    monkeypatch.setattr(service, "MAX_JOB_BYTES", encoded_size + byte_adjustment)
    try:
        if not accepted:
            with pytest.raises(Refused, match="journal full"):
                source.operations.submit(body)
            assert worker.native_operations == 0
            assert not source.operations.windows
            return
        source.operations.submit(body)
        assert source.operations.submit(body)["requestID"] == body["requestID"]
        another = dict(body, requestID="second", windowID="other")
        with pytest.raises(Refused, match="journal full"):
            source.operations.submit(another)
        assert worker.native_operations == 1
        assert len(source.operations.windows) == 1
    finally:
        worker.report_unknown("test-finished")
        source.stop(timeout=1)


def common(body):
    keys = (
        "sourcePodUID",
        "workerInstanceID",
        "generation",
        "leaseID",
        "sessionID",
        "requestID",
    )
    return {k: body[k] for k in keys}


def owner(body):
    keys = ("targetPodUID", "targetInstanceID", "registrationID", "windowID")
    return {k: body[k] for k in keys}


def test_range_replay_window_exclusion_and_session_tombstone():
    source, controls, worker, writer, body = setup()
    registry = source.operations
    bad = dict(body, fileOffset=body["fileOffset"] + 1)
    with pytest.raises(Refused):
        registry.submit(bad)
    assert writer.posts == 0
    assert registry.submit(body)["state"] == "dispatching"
    assert registry.submit(body)["state"] in ("dispatching", "submitted", "terminal")
    with pytest.raises(Refused):
        registry.submit(
            dict(
                body,
                requestID="foreign",
                targetPodUID="foreign",
                windowID="other-window",
            )
        )
    with pytest.raises(Refused):
        registry.submit(dict(body, requestID="another"))
    with pytest.raises(Refused):
        registry.submit(dict(body, length=3))
    eventually(lambda: registry.status(common(body))["state"] == "terminal")
    assert writer.posts == 1
    assert (
        registry.close_session(dict(common(body), requestID="close"))["state"]
        == "closing"
    )
    assert registry.acknowledge(common(body) | owner(body))["state"] == "terminal"
    eventually(lambda: registry.status(common(body))["state"] == "acknowledged")
    assert writer.closes == 1
    assert registry.acknowledge(common(body) | owner(body))["state"] == "acknowledged"
    assert (
        registry.close_session(dict(common(body), requestID="close"))["state"]
        == "closed"
    )
    with pytest.raises(Refused):
        registry.submit(dict(body, requestID="new"))
    controls["generation-1"].action = "drain"
    eventually(lambda: worker.state == "drained")


def test_ambiguous_post_retains_window_and_blocks_drain():
    source, controls, worker, writer, body = setup(fail=True)
    source.operations.submit(body)
    eventually(lambda: source.operations.status(common(body))["state"] == "unknown")
    assert writer.posts == 1
    assert worker.state == "quarantined" and worker.native_operations == 1
    controls["generation-1"].action = "drain"
    assert worker.blocks and controls["generation-1"].ack_calls == 0
    assert source.operations.submit(body)["state"] == "unknown"
    worker._stop_event.set()


def test_private_http_requires_bootstrap_and_exact_incarnation(tmp_path):
    source, controls, worker, _, body = setup()
    path = tmp_path / "bootstrap"
    path.write_text("private-token")
    server = PrivateServer(source.operations, str(path), ("127.0.0.1", 0))
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    connection = http.client.HTTPConnection(*server.server_address)
    try:
        connection.request(
            "POST", "/v1/operations", json.dumps(body), {"Authorization": "wrong"}
        )
        response = connection.getresponse()
        assert response.status == 403
        response.read()
        connection.request(
            "POST",
            "/v1/operations",
            json.dumps(dict(body, workerInstanceID="old")),
            {"Authorization": "private-token"},
        )
        response = connection.getresponse()
        assert response.status == 409
        response.read()
    finally:
        connection.close()
        server.shutdown()
        server.server_close()
        thread.join()
    controls["generation-1"].action = "drain"
    eventually(lambda: worker.state == "drained")


def test_runtime_snapshot_uses_bearer_and_actual_process_instance(tmp_path):
    source, controls, worker, _, body = setup()
    source.worker_instance_id = "instance-1"
    path = tmp_path / "bootstrap"
    path.write_text("private-token")
    server = PrivateServer(source.operations, str(path), ("127.0.0.1", 0))
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    connection = http.client.HTTPConnection(*server.server_address)
    try:
        connection.request(
            "GET", "/v1/runtime", headers={"Authorization": "private-token"}
        )
        response = connection.getresponse()
        assert response.status == 403
        response.read()
        connection.request(
            "GET", "/v1/runtime", headers={"Authorization": "Bearer private-token"}
        )
        response = connection.getresponse()
        assert response.status == 409
        response.read()
        connection.request(
            "POST",
            "/v1/runtime/drain",
            headers={"Authorization": "Bearer private-token", "Content-Length": "0"},
        )
        response = connection.getresponse()
        assert response.status == 200
        assert json.loads(response.read())["draining"] is True
        with pytest.raises(Refused):
            source.operations.submit(body)
        source.bootstrap.orders.append(order("late"))
        assert source.poll_once() == 0
        connection.request(
            "GET", "/v1/runtime", headers={"Authorization": "Bearer private-token"}
        )
        response = connection.getresponse()
        assert response.status == 200
        snapshot = json.loads(response.read())
        assert snapshot["workerID"] == "pod-uid"
        assert snapshot["workerInstanceID"] == "instance-1"
        assert snapshot["generations"] == [body["generation"]]
        assert snapshot["nativeOperations"] == 0
        assert set(snapshot) == {
            "workerID",
            "workerInstanceID",
            "generations",
            "nativeOperations",
            "unknownOperations",
            "retainedGenerations",
        }
    finally:
        connection.close()
        server.shutdown()
        server.server_close()
        thread.join()
    controls["generation-1"].action = "drain"
    eventually(lambda: worker.state == "drained")


def test_explicit_prepost_refusal_has_terminal_receipt_and_can_close():
    from gpustack_model_transfer.source.backend import SourcePrePostError

    source, controls, worker, writer, body = setup()

    def refuse(*args):
        raise SourcePrePostError("refused before post")

    worker.backend.post_write = refuse
    source.operations.submit(body)
    eventually(lambda: source.operations.status(common(body))["state"] == "terminal")
    assert source.operations.status(common(body))["verdict"] == "not-posted"
    assert writer.posts == 0
    source.operations.acknowledge(common(body) | owner(body))
    eventually(
        lambda: source.operations.status(common(body))["state"] == "acknowledged"
    )
    assert not worker.native_operations
    controls["generation-1"].action = "drain"
    eventually(lambda: worker.state == "drained")


def test_distinct_generations_and_target_owners_hold_separate_jobs():
    source, controls, backends, _ = supervisor([order("one"), order("two")])
    source.poll_once()
    eventually(
        lambda: all(worker.wait_until_filled(0) for worker in source.workers.values())
    )
    bodies = []
    for generation in ("one", "two"):
        worker = source.workers[generation]
        worker.control.lease_id = "lease"
        writer = FakeWrite()
        backends[generation].post_write = writer.post_write
        backends[generation].poll_write = writer.poll_write
        backends[generation].close_write = writer.close_write
        tensor = worker.assignment.manifest.tensors[0]
        bodies.append(
            {
                "sourcePodUID": "pod-uid",
                "workerInstanceID": "instance-1",
                "generation": generation,
                "leaseID": "lease",
                "sessionID": "session-" + generation,
                "requestID": "op-" + generation,
                "targetPodUID": "pod-" + generation,
                "targetInstanceID": "process-" + generation,
                "registrationID": "registration-" + generation,
                "windowID": "window-" + generation,
                "tensor": tensor.name,
                "file": tensor.file,
                "fileOffset": tensor.data_begin,
                "tensorOffset": 0,
                "length": 4,
                "target": {
                    "backend": "nixl",
                    "address": 123,
                    "length": 4,
                    "metadata": "dGFyZ2V0",
                    "endpoint": "",
                },
            }
        )
    for body in bodies:
        source.operations.submit(body)
    eventually(
        lambda: all(
            source.operations.status(common(b))["state"] == "terminal" for b in bodies
        )
    )
    assert all(source.workers[g].native_operations == 1 for g in ("one", "two"))
    for body in bodies:
        source.operations.acknowledge(common(body) | owner(body))
    eventually(
        lambda: all(
            source.operations.status(common(b))["state"] == "acknowledged"
            for b in bodies
        )
    )
    for control in controls.values():
        control.action = "drain"
    eventually(
        lambda: all(worker.state == "drained" for worker in source.workers.values())
    )


def test_stale_while_native_post_returns_never_restores_submitted_state():
    source, controls, worker, _, body = setup()
    entered, resume = threading.Event(), threading.Event()
    handle = object()

    def post(*args):
        entered.set()
        assert resume.wait(2)
        return handle

    worker.backend.post_write = post
    source.operations.submit(body)
    assert entered.wait(2)
    controls["generation-1"].stale = True
    eventually(lambda: worker.state == "quarantined")
    resume.set()
    source.threads["generation-1"].join(timeout=2)
    job = source.operations.jobs[body["requestID"]]
    assert job["state"] == "unknown" and job["native"] is handle
    assert worker.native_operations == 1
    worker._stop_event.set()


def test_disconnect_failure_keeps_terminal_job_and_window_unknown():
    source, controls, worker, _, body = setup()

    def fail_close(operation):
        raise RuntimeError("unknown disconnect")

    worker.backend.close_write = fail_close
    source.operations.submit(body)
    eventually(lambda: source.operations.status(common(body))["state"] == "terminal")
    source.operations.acknowledge(common(body) | owner(body))
    eventually(lambda: source.operations.status(common(body))["state"] == "unknown")
    assert worker.state == "quarantined" and worker.native_operations == 1
    assert source.operations.windows
    assert controls["generation-1"].ack_calls == 0
    worker._stop_event.set()


def test_empty_runtime_proof_requires_source_freeze_and_fences_old_poll(monkeypatch):
    from gpustack_model_transfer.backends.hixl import runtime as hixl_runtime
    from gpustack_model_transfer.backends.nixl import runtime as nixl_runtime
    from gpustack_model_transfer.source import backend, memory

    source, controls, _, _ = supervisor([order()])
    source.worker_instance_id = "instance-1"
    monkeypatch.setattr(backend, "retained_source_states", lambda: ())
    monkeypatch.setattr(nixl_runtime, "retained_registrations", lambda: ())
    monkeypatch.setattr(hixl_runtime, "retained_registrations", lambda: ())
    monkeypatch.setattr(memory, "_ACL_BROKEN", None)
    monkeypatch.setattr(memory, "_ACL_RUNTIME", None)
    with pytest.raises(Refused):
        source.operations.runtime()
    assert source.operations.freeze()["draining"] is True
    assert source.poll_once() == 0
    assert not controls
    assert source.operations.runtime() == {
        "workerID": "pod-uid",
        "workerInstanceID": "instance-1",
        "generations": [],
        "nativeOperations": 0,
        "unknownOperations": 0,
        "retainedGenerations": [],
    }


def test_hixl_unknown_failures_emit_distinct_fixed_stage_logs(caplog):
    from collections import deque
    from types import SimpleNamespace

    from gpustack_model_transfer.source.backend import (
        HixlSourceBackend,
        SourceRegistration,
    )
    from gpustack_model_transfer.source.transfer_service import OperationRegistry

    class Engine:
        def __init__(self, fail_at, mode):
            self.fail_at = fail_at
            self.mode = mode
            self.posts = 0

        def connect(self, endpoint):
            if self.fail_at == "connect":
                if self.mode == "raise":
                    raise RuntimeError("connect refused")
                return 1
            return 0

        def transfer_async(self, endpoint, direction, descriptors):
            self.posts += 1
            if self.fail_at == "submit":
                if self.mode == "raise":
                    raise RuntimeError("post exploded")
                return 1, 777
            return 0, 7

    def exercise(fail_at, mode):
        engine = Engine(fail_at, mode)
        backend = HixlSourceBackend()
        backend._engine = engine
        backend._hixl = SimpleNamespace(
            SUCCESS=0,
            TransferOp=SimpleNamespace(WRITE="WRITE"),
            TransferOpDesc=lambda *args: args,
        )
        block = SimpleNamespace(data_ptr=lambda: 1000, byte_size=128)
        reasons = []
        worker = StepWorker(
            backend, SourceRegistration("hixl", {"file": 1}, {"file": block}), reasons
        )
        registry = OperationRegistry(None)
        owner = ("target", "process", "registration", "window")
        registry.windows[owner] = "request"
        registry.jobs["request"] = {
            "state": "dispatching",
            "body": {
                "file": "file",
                "fileOffset": 0,
                "length": 4,
                "target": {"endpoint": "peer", "address": 2000, "length": 128},
            },
            "owner": owner,
            "common": ("source", "process", "generation", "lease", "session"),
            "worker": worker,
            "native": None,
            "verdict": "",
        }
        registry.queues[worker] = deque(["request"])
        with caplog.at_level("WARNING"):
            registry.step(worker)
        logs = [record.getMessage() for record in caplog.records]
        caplog.clear()
        return engine, registry, worker, reasons, logs

    stages = []
    for fail_at, mode in (
        ("connect", "status"),
        ("submit", "status"),
        ("connect", "raise"),
    ):
        engine, registry, worker, reasons, logs = exercise(fail_at, mode)
        job = registry.jobs["request"]
        assert engine.posts == (0 if fail_at == "connect" else 1)
        assert job["state"] == "unknown"
        assert job["verdict"] == "" and job["native"] is None
        assert job["worker"] is worker
        assert job["body"] is not None
        assert registry.windows == {
            ("target", "process", "registration", "window"): "request"
        }
        assert registry.queues[worker] == deque(["request"])
        assert worker.native_operations == 1
        assert reasons == ["native-operation-unknown"]
        assert len(logs) == 1
        assert logs[0].startswith("hixl source operation failed at stage ")
        assert "peer" not in logs[0] and "2000" not in logs[0] and "777" not in logs[0]
        stages.append(logs[0].rsplit("stage ", 1)[-1])
    assert stages == ["connect", "submit", "connect"]


@pytest.mark.parametrize(
    "method,path",
    [("GET", "/v1/runtime"), ("POST", "/v1/runtime/drain"), ("POST", "/v1/operations")],
)
def test_non_ascii_authorization_is_forbidden(tmp_path, method, path):
    token = tmp_path / "bootstrap"
    token.write_text("private-token")
    server = PrivateServer(None, str(token), ("127.0.0.1", 0))
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    connection = http.client.HTTPConnection(*server.server_address)
    try:
        connection.request(method, path, headers={"Authorization": "Bearer café"})
        response = connection.getresponse()
        assert response.status == 403
        response.read()
    finally:
        connection.close()
        server.shutdown()
        server.server_close()
        thread.join()
