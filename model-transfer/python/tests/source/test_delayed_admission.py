"""A status observation may overtake a private submit after its caller times out."""

import http.client
import json
import threading
from types import SimpleNamespace

import pytest
from test_native_operations import common, owner
from test_source_paging import paged_worker as paged_worker
from test_source_paging import request_body

from gpustack_model_transfer.source.transfer_service import (
    OperationRegistry,
    PrivateServer,
    Refused,
)


@pytest.fixture
def source_http(paged_worker, tmp_path):
    worker, runtime, storage = paged_worker
    registry = OperationRegistry(
        SimpleNamespace(
            workers={worker.control.generation: worker},
            _admission_closed=threading.Event(),
        )
    )
    worker.operations = registry
    token = tmp_path / "private-token"
    token.write_text("test-token", encoding="utf-8")
    server = PrivateServer(registry, str(token), ("127.0.0.1", 0))
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()

    def post(path, body, timeout=2):
        connection = http.client.HTTPConnection(*server.server_address, timeout=timeout)
        try:
            connection.request(
                "POST",
                path,
                json.dumps(body),
                {"Authorization": "test-token", "Content-Type": "application/json"},
            )
            response = connection.getresponse()
            return response.status, json.loads(response.read())
        finally:
            connection.close()

    try:
        yield worker, runtime, storage, registry, post
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)
        assert not thread.is_alive()


def test_status_overtakes_timed_out_submit_without_deciding_native_outcome(
    source_http, monkeypatch
):
    worker, runtime, _storage, registry, post = source_http
    body = request_body(worker, 0, "delayed", "session")
    entered, resume, admitted = (threading.Event() for _ in range(3))
    original_submit = registry.submit
    calls, failures = [], []

    def delayed_submit(payload):
        calls.append(payload)
        entered.set()
        assert resume.wait(2)
        try:
            return original_submit(payload)
        finally:
            admitted.set()

    def submit():
        try:
            post("/v1/operations", body, timeout=0.05)
        except TimeoutError as exc:
            failures.append(type(exc))

    monkeypatch.setattr(registry, "submit", delayed_submit)
    caller = threading.Thread(target=submit, daemon=True)
    caller.start()
    try:
        assert entered.wait(2)
        caller.join(timeout=2)
        assert not caller.is_alive()
        assert failures == [TimeoutError]
        status, response = post("/v1/operations/status", common(body))
        assert status == 503, response
        assert not runtime.payloads
        assert not registry.jobs
    finally:
        resume.set()
        caller.join(timeout=2)
        assert admitted.wait(2)

    assert post("/v1/operations/status", common(body))[1]["state"] == "dispatching"
    for _ in range(4):
        registry.step(worker)
    status, response = post("/v1/operations/status", common(body))
    assert status == 200
    assert response["state"] == "terminal"
    assert response["verdict"] == "succeeded"
    assert post("/v1/operations/ack", common(body) | owner(body))[0] == 200
    registry.step(worker)
    assert post("/v1/operations/status", common(body))[1]["state"] == "acknowledged"
    assert len(calls) == 1
    assert runtime.payloads == [bytes([1]) * 32]
    assert worker.native_operations == 0


@pytest.mark.parametrize(
    "observation,expected_status,expected_state",
    [
        ("absent", 503, None),
        ("native_unknown", 200, "unknown"),
        ("foreign_owner", 409, None),
        ("foreign_incarnation", 409, None),
    ],
)
def test_status_distinguishes_absence_from_native_unknown_and_foreign_owner(
    source_http, observation, expected_status, expected_state
):
    worker, runtime, storage, registry, post = source_http
    body = request_body(worker, 0, "observed", "session")
    query = common(body)
    if observation == "foreign_incarnation":
        query["workerInstanceID"] = "replacement-instance"
    elif observation != "absent":
        registry.submit(body)
        if observation == "native_unknown":
            registry.mark_unknown(worker)
        else:
            query["sessionID"] = "foreign-session"
    resident = storage.live
    for _ in range(3):
        status, response = post("/v1/operations/status", query)
        assert status == expected_status
        assert response.get("state") == expected_state
    assert not runtime.payloads
    assert storage.live == resident
    assert storage.frees == 0
    if observation == "native_unknown":
        with pytest.raises(Refused, match="operation not terminal"):
            registry.acknowledge(common(body) | owner(body))
        assert worker.native_operations == 1
