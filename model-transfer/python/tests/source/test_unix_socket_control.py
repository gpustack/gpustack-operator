"""Unix domain socket control and private source service tests."""

from __future__ import annotations

import base64
import ctypes
import hashlib
import json
import os
import socket
import socketserver
import stat
import tempfile
import threading
from http.server import BaseHTTPRequestHandler
from pathlib import Path

import pytest

from gpustack_model_transfer import envs
from gpustack_model_transfer.source import __main__ as source_main
from gpustack_model_transfer.source import control as control_mod
from gpustack_model_transfer.source.backend import SourceRegistration
from gpustack_model_transfer.source.control import BootstrapClient, ControlClient
from gpustack_model_transfer.source.supervisor import SourceSupervisor
from gpustack_model_transfer.source.transfer_service import PrivateServer

FIXTURE = Path(__file__).resolve().parents[3] / "contracts/fixtures/tiny_dense"


@pytest.fixture
def short_sock_dir(tmp_path):
    probe = tmp_path / "w.sock"
    if len(str(probe)) <= 96:
        yield tmp_path
        return
    with tempfile.TemporaryDirectory(dir="/tmp", prefix="gmt-") as short_dir:
        yield Path(short_dir)


class _UnixBrokerServer(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True


def _start_unix_broker(sock_path: Path, bootstrap_token: str, assignment_order: dict):
    posts: list[tuple[str, dict]] = []
    page_body = '{"tensors":[]}'
    page_sha = hashlib.sha256(page_body.encode()).hexdigest()

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def address_string(self):
            return "unix"

        def _write_json(self, status: int, obj: dict):
            raw = json.dumps(obj, separators=(",", ":")).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def do_GET(self):
            if self.headers.get("Authorization") != bootstrap_token:
                self._write_json(403, {"kind": "authentication", "reason": "bad token"})
                return
            if self.path == "/v1/assignments":
                self._write_json(200, {"assignments": [assignment_order]})
                return
            self._write_json(404, {"kind": "unknown-path"})

        def do_POST(self):
            length = int(self.headers.get("Content-Length", "0"))
            body = json.loads(self.rfile.read(length) or b"{}")
            posts.append((self.path, body))
            if self.path == "/v1/manifest-pages":
                if self.headers.get("Authorization") != bootstrap_token:
                    self._write_json(403, {"kind": "authentication"})
                    return
                self._write_json(
                    200,
                    {
                        "manifestSHA256": body["manifestSHA256"],
                        "pageSHA256": page_sha,
                        "pageJSON": page_body,
                    },
                )
                return
            self._write_json(
                200,
                {
                    "leaseID": "lease-uds-1",
                    "expiresAt": "2031-01-01T00:00:00Z",
                    "activeSessions": 0,
                    "unknownSessions": 0,
                    "action": "none",
                },
            )

    server = _UnixBrokerServer(str(sock_path), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server, thread, posts, page_sha, page_body


def test_worker_address_env_registered_and_trimmed(monkeypatch):
    monkeypatch.delenv("GPUSTACK_MODEL_TRANSFER_WORKER_ADDRESS", raising=False)
    assert envs.GPUSTACK_MODEL_TRANSFER_WORKER_ADDRESS is None
    assert "GPUSTACK_MODEL_TRANSFER_WORKER_ADDRESS" in dir(envs)
    view = envs.get_envs(
        {"GPUSTACK_MODEL_TRANSFER_WORKER_ADDRESS": "  unix:///run/mt/worker.sock  "}
    )
    assert view.GPUSTACK_MODEL_TRANSFER_WORKER_ADDRESS == "unix:///run/mt/worker.sock"
    assert (
        envs.get_envs(
            {"GPUSTACK_MODEL_TRANSFER_WORKER_ADDRESS": "   "}
        ).GPUSTACK_MODEL_TRANSFER_WORKER_ADDRESS
        is None
    )


@pytest.mark.parametrize(
    "bad_addr", ["unix://", "unix://relative/control.sock", "unix:///"]
)
def test_invalid_unix_addresses_rejected(bad_addr, tmp_path):
    with pytest.raises(ValueError):
        control_mod._open_connection(bad_addr, timeout=1.0)
    token = tmp_path / "bootstrap"
    token.write_text("tok")
    with pytest.raises(ValueError):
        PrivateServer(None, str(token), bad_addr)


def test_uds_bootstrap_and_control_roundtrip(short_sock_dir):
    sock_path = short_sock_dir / "c.sock"
    bootstrap_file = short_sock_dir / "bootstrap"
    bootstrap_file.write_text("bootstrap-uds-token\n")
    raw_manifest = (FIXTURE / "manifest.json").read_bytes()
    manifest_sha = hashlib.sha256(raw_manifest).hexdigest()
    order = {"generation": "gen-uds", "manifestSHA256": manifest_sha}
    server, thread, posts, page_sha, page_body = _start_unix_broker(
        sock_path, "bootstrap-uds-token", order
    )
    uds_addr = f"unix://{sock_path}"
    try:
        bootstrap = BootstrapClient(uds_addr, str(bootstrap_file), timeout=2.0)
        assert bootstrap.assignments() == [order]
        assert bootstrap.manifest_page(order, {"sha256": page_sha}) == page_body.encode(
            "utf-8"
        )

        control = ControlClient(
            uds_addr,
            worker_id="pod-1",
            generation="gen-uds",
            secret="sec-1",
            worker_instance_id="inst-uds",
            timeout=2.0,
        )
        assert control.register() == "lease-uds-1"
        assert control.native_start()["action"] == "none"
        hb = control.heartbeat(resident_bytes=1024, registrations=1)
        assert hb["action"] == "none"
        control.filled(
            1,
            [
                {
                    "name": "checkpoint.safetensors",
                    "addr": 4096,
                    "length": 16,
                    "memoryType": "HOST",
                    "backend": "hixl",
                    "workerUID": "pod-1",
                }
            ],
        )
        control.drain_ack(native_released=True)
        control.quarantine("hixl-connect")
        assert [p[0] for p in posts] == [
            "/v1/manifest-pages",
            "/v1/register",
            "/v1/native-start",
            "/v1/heartbeat",
            "/v1/filled",
            "/v1/drain-ack",
            "/v1/quarantine",
        ]
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=3)
        if sock_path.exists():
            sock_path.unlink()


def test_uds_private_server_operations_runtime_and_drain(short_sock_dir):
    sock_path = short_sock_dir / "w.sock"
    bootstrap_file = short_sock_dir / "bootstrap"
    bootstrap_file.write_text("private-uds-token")

    # Create a stale unix socket at sock_path first to verify unlink-before-bind.
    stale_srv = socketserver.UnixStreamServer(str(sock_path), BaseHTTPRequestHandler)
    stale_srv.server_close()
    assert sock_path.exists()

    raw_manifest = (FIXTURE / "manifest.json").read_bytes()
    manifest_obj = json.loads(raw_manifest)
    order = {
        "generation": "gen-1",
        "workerSecret": "secret",
        "digest": "sha256:" + "a" * 64,
        "namespace": "test",
        "artifactUID": manifest_obj["artifact_uid"],
        "sourceGeneration": manifest_obj["source_generation"],
        "manifestSHA256": hashlib.sha256(raw_manifest).hexdigest(),
        "manifestJSON": raw_manifest.decode(),
        "treePath": str(FIXTURE),
        "totalBytes": sum(f["size"] for f in manifest_obj["files"]),
        "registrations": len(manifest_obj["files"]),
    }

    class _Block:
        def __init__(self, size):
            self.buf = (ctypes.c_ubyte * size)()
            self.byte_size = size

        def data_ptr(self):
            return ctypes.addressof(self.buf)

        def write_bytes(self, offset, data):
            ctypes.memmove(self.data_ptr() + offset, data, len(data))

        def view(self, offset, length):
            return memoryview(self.buf)[offset : offset + length].cast("B")

    class _Backend:
        def start(self):
            pass

        def register(self, blocks):
            return SourceRegistration("nixl", dict.fromkeys(blocks, 1), dict(blocks))

        def disconnect(self, reg):
            pass

        def revoke(self, reg):
            reg.handles.clear()

        def post_write(self, reg, name, offset, length, target):
            return "op-1"

        def poll_write(self, op):
            return "succeeded"

        def close_write(self, op):
            pass

    class _Control:
        generation = "gen-1"
        worker_id = "pod-uid"
        worker_instance_id = "inst-1"
        lease_id = "lease-1"
        action = "none"

        def register(self):
            return "lease-1"

        def native_start(self):
            return {"action": "none"}

        def heartbeat(self, **kw):
            return {
                "action": self.action,
                "activeSessions": 0,
                "unknownSessions": 0,
            }

        def filled(self, count, descs):
            pass

        def drain_ack(self, native_released):
            pass

        def quarantine(self, reason):
            pass

        def close(self):
            pass

    ctrl = _Control()
    sup = SourceSupervisor(
        type("Boot", (), {"assignments": lambda self: [order]})(),
        control_factory=lambda _a: ctrl,
        backend_factory=lambda _a: _Backend(),
        allocator_factory=lambda _a: type(
            "Alloc",
            (),
            {
                "allocate": lambda self, _n, s: _Block(s),
                "release": lambda self, _b: None,
            },
        )(),
        worker_id="pod-uid",
        backend="nixl",
        poll_interval=0.01,
    )
    sup.worker_instance_id = "inst-1"
    assert sup.poll_once() == 1
    worker = sup.workers["gen-1"]
    assert worker.wait_until_filled(3)

    uds_addr = f"unix://{sock_path}"
    server = PrivateServer(sup.operations, str(bootstrap_file), uds_addr)
    mode = stat.S_IMODE(os.lstat(sock_path).st_mode)
    assert mode == 0o600
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()

    def request_uds(
        method: str, path: str, body: dict | None = None, *, bearer=False, wait_ms=0
    ):
        conn = control_mod._open_connection(uds_addr, timeout=2.0)
        try:
            headers = {
                "Authorization": (
                    "Bearer private-uds-token" if bearer else "private-uds-token"
                )
            }
            raw_body = None
            if body is not None:
                raw_body = json.dumps(body)
                headers["Content-Type"] = "application/json"
            else:
                headers["Content-Length"] = "0"
            if wait_ms:
                headers["X-GPUStack-Wait-Ms"] = str(wait_ms)
            conn.request(method, path, body=raw_body, headers=headers)
            resp = conn.getresponse()
            return resp.status, json.loads(resp.read() or b"{}")
        finally:
            conn.close()

    try:
        tensor = worker.assignment.manifest.tensors[0]
        common = {
            "sourcePodUID": "pod-uid",
            "workerInstanceID": "inst-1",
            "generation": "gen-1",
            "leaseID": "lease-1",
            "sessionID": "sess-1",
            "requestID": "req-1",
        }
        owner = {
            "targetPodUID": "t-pod",
            "targetInstanceID": "t-inst",
            "registrationID": "reg-1",
            "windowID": "win-1",
        }
        submit_body = (
            common
            | owner
            | {
                "tensor": tensor.name,
                "file": tensor.file,
                "fileOffset": tensor.data_begin,
                "tensorOffset": 0,
                "length": 4,
                "target": {
                    "backend": "nixl",
                    "address": 4096,
                    "length": 16,
                    "metadata": base64.b64encode(b"target").decode(),
                    "endpoint": "",
                },
            }
        )
        status, op_resp = request_uds(
            "POST", "/v1/operations", submit_body, wait_ms=500
        )
        assert status == 200
        if op_resp["state"] != "terminal":
            while op_resp["state"] != "terminal":
                status, op_resp = request_uds("POST", "/v1/operations/status", common)
                assert status == 200
        assert op_resp["verdict"] == "succeeded"

        status, _ack_resp = request_uds(
            "POST", "/v1/operations/ack", common | owner, wait_ms=500
        )
        assert status == 200

        status, drain_resp = request_uds("POST", "/v1/runtime/drain", bearer=True)
        assert status == 200
        assert drain_resp["draining"] is True

        status, rt_resp = request_uds("GET", "/v1/runtime", bearer=True)
        assert status == 200
        assert rt_resp["workerID"] == "pod-uid"
        assert rt_resp["workerInstanceID"] == "inst-1"
        assert rt_resp["generations"] == ["gen-1"]
    finally:
        ctrl.action = "drain"
        sup.stop(timeout=3)
        server.shutdown()
        server.server_close()
        thread.join(timeout=3)

    assert not sock_path.exists()


def test_uds_private_server_refuses_active_socket_and_reclaims_stale_socket(
    short_sock_dir,
):
    from types import SimpleNamespace

    from gpustack_model_transfer.source.transfer_service import OperationRegistry

    sock_path = short_sock_dir / "active.sock"
    bootstrap_file = short_sock_dir / "bootstrap"
    bootstrap_file.write_text("active-uds-token")

    # 1) Dead stale socket on disk is reclaimed cleanly.
    stale_srv = socketserver.UnixStreamServer(str(sock_path), BaseHTTPRequestHandler)
    stale_inode = (os.lstat(sock_path).st_dev, os.lstat(sock_path).st_ino)
    stale_srv.server_close()
    assert sock_path.exists()

    closed_ev = threading.Event()
    closed_ev.set()
    registry = OperationRegistry(
        SimpleNamespace(
            workers={},
            worker_id="pod-uid",
            worker_instance_id="inst-active",
            _starting=0,
            _admission_closed=closed_ev,
            _lock=threading.Lock(),
        )
    )
    uds_addr = f"unix://{sock_path}"
    first = PrivateServer(registry, str(bootstrap_file), uds_addr)
    first_thread = threading.Thread(target=first.serve_forever, daemon=True)
    first_thread.start()
    try:
        active_st = os.lstat(sock_path)
        active_inode = (active_st.st_dev, active_st.st_ino)
        assert active_inode != stale_inode

        # 2) Starting a second PrivateServer on the active socket fails closed
        #    without unlinking or detaching the first listener.
        with pytest.raises(OSError, match="unix socket already active"):
            PrivateServer(registry, str(bootstrap_file), uds_addr)

        after_st = os.lstat(sock_path)
        assert (after_st.st_dev, after_st.st_ino) == active_inode

        conn = control_mod._open_connection(uds_addr, timeout=2.0)
        try:
            conn.request(
                "GET",
                "/v1/runtime",
                headers={"Authorization": "Bearer active-uds-token"},
            )
            resp = conn.getresponse()
            body = json.loads(resp.read())
            assert resp.status == 200
            assert body["workerInstanceID"] == "inst-active"
        finally:
            conn.close()
    finally:
        first.shutdown()
        first.server_close()
        first_thread.join(timeout=3)

    assert not sock_path.exists()


def test_source_main_accepts_uds_broker_and_worker_addresses(
    short_sock_dir, monkeypatch
):
    control_sock = short_sock_dir / "ctrl.sock"
    worker_sock = short_sock_dir / "wrk.sock"
    bootstrap_file = short_sock_dir / "bootstrap"
    bootstrap_file.write_text("main-uds-token")

    server, thread, _posts, _psha, _pbody = _start_unix_broker(
        control_sock, "main-uds-token", {}
    )
    server.RequestHandlerClass.do_GET = lambda self: (
        self.send_response(200),
        self.send_header("Content-Type", "application/json"),
        self.send_header("Content-Length", "18"),
        self.end_headers(),
        self.wfile.write(b'{"assignments":[]}'),
    )
    stop_event = threading.Event()
    observed_worker_mode = []

    orig_poll = SourceSupervisor.poll_once

    def poll_and_stop(self):
        if worker_sock.exists():
            observed_worker_mode.append(stat.S_IMODE(os.lstat(worker_sock).st_mode))
        stop_event.set()
        return orig_poll(self)

    monkeypatch.setattr(SourceSupervisor, "poll_once", poll_and_stop)
    env = {
        "KUBERNETES_POD_UID": "pod-uds-uid",
        "KUBERNETES_NODE_NAME": "node-a",
        "KUBERNETES_POD_IP": "10.0.0.5",
        "GPUSTACK_MODEL_TRANSFER_BACKEND": "hixl",
        "GPUSTACK_MODEL_TRANSFER_VENDOR_SCOPE": "ascend",
        "GPUSTACK_MODEL_TRANSFER_BROKER_ADDRESS": f"unix://{control_sock}",
        "GPUSTACK_MODEL_TRANSFER_WORKER_ADDRESS": f"unix://{worker_sock}",
        "GPUSTACK_MODEL_TRANSFER_BOOTSTRAP_FILE": str(bootstrap_file),
    }
    try:
        rc = source_main.main(environ=env, stop_event=stop_event)
        assert rc == 0
        assert observed_worker_mode == [0o600]
        assert not worker_sock.exists()
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=3)
        if control_sock.exists():
            control_sock.unlink()


def test_parse_private_address_aligns_loopback_and_rejects_port_zero():
    from gpustack_model_transfer.source.control import parse_private_address

    assert parse_private_address("127.0.0.1:9252") == ("127.0.0.1", 9252)
    assert parse_private_address("http://127.0.0.1:9252") == ("127.0.0.1", 9252)
    assert parse_private_address("localhost:9252") == ("localhost", 9252)
    assert parse_private_address("http://localhost:9252") == ("localhost", 9252)
    assert parse_private_address("[::1]:9252") == ("::1", 9252)
    assert parse_private_address("http://[::1]:9252") == ("::1", 9252)
    assert parse_private_address("unix:///run/mt/worker.sock") == (
        "unix:///run/mt/worker.sock"
    )

    for bad in (
        "127.0.0.1:0",
        "http://127.0.0.1:0",
        "localhost:0",
        "[::1]:0",
        "127.0.0.1:09252",
        "127.0.0.1:9252/path",
        "https://127.0.0.1:9252",
        "10.0.0.1:9252",
        "example.com:9252",
        "unix://relative.sock",
        "unix:///tmp/../run/worker.sock",
        "unix:///run/./worker.sock",
        "unix:///run/worker.sock?x=1",
        "unix:///run/worker.sock#frag",
    ):
        with pytest.raises(ValueError):
            parse_private_address(bad)


def test_source_main_rejects_port_zero_and_private_server_accepts_http_loopback(
    short_sock_dir,
):
    bootstrap_file = short_sock_dir / "bootstrap"
    bootstrap_file.write_text("loopback-token")

    env = {
        "KUBERNETES_POD_UID": "pod-loop-uid",
        "KUBERNETES_NODE_NAME": "node-a",
        "KUBERNETES_POD_IP": "10.0.0.5",
        "GPUSTACK_MODEL_TRANSFER_BACKEND": "hixl",
        "GPUSTACK_MODEL_TRANSFER_VENDOR_SCOPE": "ascend",
        "GPUSTACK_MODEL_TRANSFER_BROKER_ADDRESS": "127.0.0.1:9250",
        "GPUSTACK_MODEL_TRANSFER_WORKER_ADDRESS": "127.0.0.1:0",
        "GPUSTACK_MODEL_TRANSFER_BOOTSTRAP_FILE": str(bootstrap_file),
    }
    assert source_main.main(environ=env) == 2

    # Reserve a free port briefly, then bind PrivateServer via http://127.0.0.1:<port>.
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as probe:
        probe.bind(("127.0.0.1", 0))
        port = probe.getsockname()[1]
    srv = PrivateServer(None, str(bootstrap_file), f"http://127.0.0.1:{port}")
    try:
        assert srv.server_address == ("127.0.0.1", port)
    finally:
        srv.server_close()
