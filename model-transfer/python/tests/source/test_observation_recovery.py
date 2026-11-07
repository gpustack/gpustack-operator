"""Observation loss must not change native ownership or duplicate transfers."""

import http.client
import json
import socket
import threading
from contextlib import contextmanager
from types import SimpleNamespace

import pytest
from test_native_operations import common, owner
from test_source_paging import paged_worker as paged_worker
from test_source_paging import request_body

from gpustack_model_transfer.source.backend import SourceUnavailableError
from gpustack_model_transfer.source.transfer_service import (
    OperationRegistry,
    PrivateHandler,
    PrivateServer,
    Refused,
)


def registry_for(worker):
    registry = OperationRegistry(
        SimpleNamespace(
            workers={worker.control.generation: worker},
            _admission_closed=threading.Event(),
        )
    )
    worker.operations = registry
    return registry


def step_many(registry, worker, count=12):
    return sum(bool(registry.step(worker)) for _ in range(count))


def ack_body(body):
    return common(body) | owner(body)


@contextmanager
def private_http(registry, tmp_path):
    token = tmp_path / "private-token"
    token.write_text("test-token", encoding="utf-8")
    server = PrivateServer(registry, str(token), ("127.0.0.1", 0))
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()

    def post(path, body):
        connection = http.client.HTTPConnection(*server.server_address, timeout=2)
        try:
            connection.request(
                "POST",
                path,
                json.dumps(body),
                {"Authorization": "test-token", "Content-Type": "application/json"},
            )
            response = connection.getresponse()
            assert response.status == 200
            return json.loads(response.read())
        finally:
            connection.close()

    try:
        yield post
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)
        assert not thread.is_alive()


@pytest.mark.parametrize(
    "lost_path", ["/v1/operations", "/v1/operations/status", "/v1/operations/ack"]
)
def test_lost_http_reply_recovers_same_operation(
    paged_worker, tmp_path, monkeypatch, lost_path
):
    worker, runtime, _storage = paged_worker
    registry = registry_for(worker)
    body = request_body(worker, 0, "observed", "session")
    dropped = []
    original_end_headers = PrivateHandler.end_headers

    def drop_reply(handler):
        if handler.path == lost_path and not dropped:
            dropped.append(handler.path)
            handler.connection.shutdown(socket.SHUT_RDWR)
            raise ConnectionResetError("injected response loss after journal update")
        return original_end_headers(handler)

    monkeypatch.setattr(PrivateHandler, "end_headers", drop_reply)
    requests = [
        ("/v1/operations", body),
        ("/v1/operations/status", common(body)),
        ("/v1/operations/ack", ack_body(body)),
    ]
    with private_http(registry, tmp_path) as post:
        for path, payload in requests:
            if path == lost_path:
                with pytest.raises(http.client.RemoteDisconnected):
                    post(path, payload)
            response = post(path, payload)
            assert response["requestID"] == body["requestID"]
            assert response["state"] != "unknown"
            step_many(registry, worker)
        assert post("/v1/operations/status", common(body))["state"] == "acknowledged"
        assert post("/v1/operations/ack", ack_body(body))["state"] == "acknowledged"
        assert post("/v1/operations", body)["state"] == "acknowledged"
    assert dropped == [lost_path]
    assert runtime.payloads == [bytes([1]) * 32]
    assert worker.native_operations == 0
    assert not worker.backend.has_active_peers()


@pytest.mark.parametrize("paged_worker", ["nixl"], indirect=True)
def test_failed_transfer_requires_matching_ack_before_page_replacement(
    paged_worker, monkeypatch
):
    worker, runtime, storage = paged_worker
    registry = registry_for(worker)
    failed = request_body(worker, 0, "failed", "departed-session")
    page = request_body(worker, 5, "page", "healthy-session")
    newer = request_body(worker, 1, "newer", "healthy-session")
    original_poll = runtime.poll
    monkeypatch.setattr(runtime, "poll", lambda *_: SimpleNamespace(state="failed"))
    registry.submit(failed)
    step_many(registry, worker)
    assert registry.status(common(failed))["verdict"] == "failed"
    monkeypatch.setattr(runtime, "poll", original_poll)
    for body in (page, newer):
        registry.submit(body)
    assert step_many(registry, worker) == 0
    with pytest.raises(Refused, match="unknown operation owner"):
        registry.acknowledge(ack_body(failed) | {"targetInstanceID": "replacement"})
    with pytest.raises(Refused, match="operation not terminal"):
        registry.acknowledge(ack_body(page))
    assert registry.close_session(common(failed)) == {"state": "closing"}
    assert step_many(registry, worker) == 0
    assert runtime.payloads == [bytes([1]) * 32]
    assert storage.frees == 0
    assert worker.backend.has_active_peers()

    for body, expected_payloads in (
        (failed, [1, 6]),
        (page, [1, 6, 2]),
        (newer, [1, 6, 2]),
    ):
        for _ in range(2):
            assert registry.acknowledge(ack_body(body))["state"] == "terminal"
        step_many(registry, worker)
        assert registry.acknowledge(ack_body(body))["state"] == "acknowledged"
        assert runtime.payloads == [bytes([value]) * 32 for value in expected_payloads]
    assert worker.native_operations == 0
    assert not worker.backend.has_active_peers()
    assert registry.close_session(common(failed)) == {"state": "closed"}


@pytest.mark.parametrize("paged_worker", ["nixl"], indirect=True)
@pytest.mark.parametrize("native_boundary", ["poll", "disconnect_peer"])
def test_native_unknown_cannot_recover_by_retrying_observation(
    paged_worker, monkeypatch, native_boundary
):
    worker, runtime, storage = paged_worker
    registry = registry_for(worker)
    body = request_body(worker, 0, "uncertain", "session")
    registry.submit(body)
    assert registry.step(worker)
    if native_boundary == "disconnect_peer":
        step_many(registry, worker)
        registry.acknowledge(ack_body(body))
    original = getattr(runtime, native_boundary)

    def fail_native(*_args):
        raise RuntimeError("native ownership outcome unknown")

    monkeypatch.setattr(runtime, native_boundary, fail_native)
    registry.step(worker)
    monkeypatch.setattr(runtime, native_boundary, original)
    for _ in range(3):
        assert registry.status(common(body))["state"] == "unknown"
        assert registry.submit(body)["state"] == "unknown"
        with pytest.raises(Refused, match="operation not terminal"):
            registry.acknowledge(ack_body(body))
    assert registry.close_session(common(body)) == {"state": "closing"}
    assert not worker.release_idle_blocks()
    with pytest.raises(SourceUnavailableError, match="hold the registration"):
        worker.backend.revoke(worker.registration)
    assert worker.native_operations == 1
    assert worker.backend.has_active_peers()
    assert storage.frees == 0
    assert runtime.payloads == [bytes([1]) * 32]
