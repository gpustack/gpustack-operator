"""Local TLS and explicit fake-native tests of the engine reader."""

import ctypes
import hashlib
import json
import os
import ssl
import subprocess
import threading
import time
from contextlib import suppress
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from types import SimpleNamespace

import pytest

from gpustack_model_transfer.client import EngineReader, EngineSession
from gpustack_model_transfer.client_metadata import load_view
from gpustack_model_transfer.core.budgets import ReadBudgets
from gpustack_model_transfer.core.errors import SourceUnavailableError

FIXTURE = Path(__file__).resolve().parents[3] / "contracts/fixtures/tiny_dense"


def metadata_view(root, extra_metadata=None):
    raw_file = (FIXTURE / "checkpoint.safetensors").read_bytes()
    length = int.from_bytes(raw_file[:8], "little")
    header = raw_file[: 8 + length]
    config = b'{"model_type":"fixture"}'
    original = json.loads((FIXTURE / "manifest.json").read_bytes())
    original["files"].append(
        {
            "path": "config.json",
            "size": len(config),
        }
    )
    listing_entries = [
        {
            "path": "checkpoint.safetensors",
            "size": len(raw_file),
            "digest": "sha256:" + hashlib.sha256(raw_file).hexdigest(),
        },
        {
            "path": "config.json",
            "size": len(config),
            "digest": "sha256:" + hashlib.sha256(config).hexdigest(),
        },
    ]
    for name, data in (extra_metadata or {}).items():
        original["files"].append({"path": name, "size": len(data)})
        listing_entries.append(
            {
                "path": name,
                "size": len(data),
                "digest": "sha256:" + hashlib.sha256(data).hexdigest(),
            }
        )
    manifest = json.dumps(original, separators=(",", ":")).encode()
    encoded = b"gpustack-manifest v1\n" + b"".join(
        f"{item['digest']} {item['size']} {item['path']}\n".encode()
        for item in sorted(listing_entries, key=lambda item: item["path"])
    )
    canonical = "sha256:" + hashlib.sha256(encoded).hexdigest()
    listing = json.dumps({"digest": canonical, "entries": listing_entries}).encode()
    contents = {
        "checkpoint.safetensors": header,
        "config.json": config,
        "manifest.json": listing,
        **(extra_metadata or {}),
    }
    files = []
    for name, data in contents.items():
        (root / name).write_bytes(data)
        entry = {
            "name": name,
            "size": len(data),
            "sha256": hashlib.sha256(data).hexdigest(),
        }
        if name.endswith(".safetensors"):
            entry["truncated"] = True
        files.append(entry)
    (root / "view.json").write_text(
        json.dumps(
            {
                "kind": "gpustack-model-transfer-metadata-view/v1",
                "artifactDigest": canonical,
                "incomplete": True,
                "files": files,
            }
        )
    )
    return raw_file, manifest, canonical


@pytest.mark.parametrize("tampered", [False, True], ids=["published", "tampered"])
def test_modelslim_metadata_is_authenticated(tmp_path, tampered):
    from gpustack_model_transfer.core.errors import AuthenticationError

    name = "quant_model_description.json"
    data = b'{"model_quant_type":"W8A8_DYNAMIC"}'
    _, _, canonical = metadata_view(tmp_path, {name: data})
    if tampered:
        altered = b'{"model_quant_type":"W4A8"}'
        (tmp_path / name).write_bytes(altered)
        view = json.loads((tmp_path / "view.json").read_text())
        item = next(item for item in view["files"] if item["name"] == name)
        item["size"] = len(altered)
        item["sha256"] = hashlib.sha256(altered).hexdigest()
        (tmp_path / "view.json").write_text(json.dumps(view))
        with pytest.raises(AuthenticationError, match="published listing"):
            load_view(str(tmp_path), canonical)
    else:
        _, hashes, verified = load_view(str(tmp_path), canonical)
        assert verified
        assert name in hashes
        assert (tmp_path / name).read_bytes() == data


class FakeWindow:
    def __init__(self, backend, pod_ip, size, *, memory_mode="host", device=None):
        self.size = min(size, 8)
        self.storage = ctypes.create_string_buffer(self.size)
        self.block = self
        self.registration_id, self.window_id = "registration", "window"
        self.events = []
        self.closed = False

    def start(self):
        self.events.append("register")

    def view(self, offset, size):
        return memoryview(
            (ctypes.c_ubyte * size).from_address(
                ctypes.addressof(self.storage) + offset
            )
        ).cast("B")

    def descriptor(self):
        return {
            "backend": "nixl",
            "address": ctypes.addressof(self.storage),
            "length": self.size,
            "metadata": "dGFyZ2V0",
            "endpoint": "",
        }

    def copy_to(self, target, offset, length):
        target.data[offset : offset + length] = self.view(0, length)
        self.events.extend(("copy", "synchronize"))

    def synchronize(self, target):
        self.events.append("synchronize")

    def close(self):
        self.events.append("deregister")
        self.closed = True


class TLSBroker:
    def __init__(self, root, payload, manifest, canonical):
        self.calls = []
        self.results = {}
        self.sessions = {}
        self.payload, self.manifest, self.canonical = payload, manifest, canonical
        self.manifest_obj = json.loads(manifest)
        self.manifest_pages = {}
        self.lose_ack_once = False
        self.lose_open_once = False
        self.lose_open_count = 0
        self.lose_close_count = 0
        self.lose_transfer_once = False
        self.drop_status = False
        self.expected_token = "projected-token"
        self.source_instance = "source-instance"
        self.unknown = False
        self.pre_admission_refusal = False
        self.capacity_refusals = 0
        self.refuse_prepost = False
        broker = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def receive(self):
                assert (
                    self.headers.get("Authorization")
                    == "Bearer " + broker.expected_token
                )
                return json.loads(
                    self.rfile.read(int(self.headers.get("Content-Length", "0")))
                    or b"{}"
                )

            def answer(self, data):
                raw = json.dumps(data).encode()
                self.send_response(200)
                self.send_header("Content-Length", str(len(raw)))
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(raw)

            def do_POST(self):
                body = self.receive()
                broker.calls.append((self.path, body))
                if self.path == "/v1/sessions":
                    if broker.capacity_refusals > 0:
                        broker.capacity_refusals -= 1
                        raw = json.dumps(
                            {
                                "requestID": body["requestID"],
                                "admission": "not-created",
                                "kind": "capacity-exhausted",
                                "retryAfter": 0.05,
                            }
                        ).encode()
                        self.send_response(409)
                        self.send_header("Retry-After", "0.05")
                        self.send_header("Content-Length", str(len(raw)))
                        self.end_headers()
                        self.wfile.write(raw)
                        return
                    if broker.pre_admission_refusal:
                        raw = json.dumps(
                            {
                                "requestID": body["requestID"],
                                "admission": "not-created",
                                "kind": "source-unavailable",
                            }
                        ).encode()
                        self.send_response(409)
                        self.send_header("Content-Length", str(len(raw)))
                        self.end_headers()
                        self.wfile.write(raw)
                        return
                    sid = "s-" + body["requestID"]
                    broker.sessions[sid] = body
                    if broker.lose_open_once or broker.lose_open_count:
                        broker.lose_open_once = False
                        broker.lose_open_count = max(0, broker.lose_open_count - 1)
                        self.connection.shutdown(2)
                        self.connection.close()
                        return
                    self.answer(
                        {
                            "sessionID": sid,
                            "requestID": body["requestID"],
                            "digest": broker.canonical,
                            "artifactUID": broker.manifest_obj["artifact_uid"],
                            "sourceGeneration": 1,
                            "manifestSHA256": hashlib.sha256(
                                broker.manifest
                            ).hexdigest(),
                            "manifestJSON": broker.manifest.decode(),
                            "generation": "generation-1",
                            "sourcePodUID": "source-pod",
                            "workerInstanceID": broker.source_instance,
                        }
                    )
                elif self.path == "/v1/manifest-pages":
                    self.answer(
                        {
                            "manifestSHA256": body["manifestSHA256"],
                            "pageSHA256": body["pageSHA256"],
                            "pageJSON": broker.manifest_pages[body["pageSHA256"]],
                        }
                    )
                elif self.path.endswith("/closing"):
                    self.answer({"state": "closing"})
                elif self.path == "/v1/transfers":
                    tensor = next(
                        t
                        for t in broker.manifest_obj["tensors"]
                        if t["name"] == body["tensor"]
                    )
                    begin = tensor["data_begin"] + body["tensorOffset"]
                    if not broker.refuse_prepost:
                        ctypes.memmove(
                            body["target"]["address"],
                            broker.payload[begin : begin + body["length"]],
                            body["length"],
                        )
                    state = "unknown" if broker.unknown else "terminal"
                    broker.results[body["requestID"]] = {
                        "requestID": body["requestID"],
                        "sessionID": body["sessionID"],
                        "state": state,
                        "verdict": (
                            ""
                            if broker.unknown
                            else "not-posted"
                            if broker.refuse_prepost
                            else "succeeded"
                        ),
                    }
                    if broker.lose_transfer_once:
                        broker.lose_transfer_once = False
                        self.connection.shutdown(2)
                        self.connection.close()
                        return
                    self.answer(broker.results[body["requestID"]])
                elif self.path.endswith("/ack"):
                    request_id = self.path.split("/")[-2]
                    broker.results[request_id]["state"] = "acknowledged"
                    if broker.lose_ack_once:
                        broker.lose_ack_once = False
                        self.connection.shutdown(2)
                        self.connection.close()
                        return
                    self.answer(broker.results[request_id])
                else:
                    raise AssertionError(self.path)

            def do_GET(self):
                self.receive()
                if broker.drop_status:
                    raw = b'{"kind":"unknown"}'
                    self.send_response(409)
                    self.send_header("Content-Length", str(len(raw)))
                    self.end_headers()
                    self.wfile.write(raw)
                    return
                request_id = self.path.split("/")[-1].split("?")[0]
                self.answer(broker.results[request_id])

            def do_DELETE(self):
                body = self.receive()
                assert body["targetReleased"] is True
                broker.calls.append((self.path, body))
                if broker.lose_close_count:
                    broker.lose_close_count -= 1
                    self.connection.shutdown(2)
                    self.connection.close()
                    return
                self.answer({"state": "closed"})

        cert, key = root / "cert.pem", root / "key.pem"
        subprocess.run(
            [
                "openssl",
                "req",
                "-x509",
                "-newkey",
                "rsa:2048",
                "-nodes",
                "-keyout",
                str(key),
                "-out",
                str(cert),
                "-days",
                "1",
                "-subj",
                "/CN=broker.test",
                "-addext",
                "subjectAltName=DNS:broker.test",
            ],
            check=True,
            capture_output=True,
        )
        self.cert = cert
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(cert, key)
        self.server.socket = context.wrap_socket(self.server.socket, server_side=True)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def close(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)


def make_reader(tmp_path, native_factory=FakeWindow):
    view = tmp_path / "view"
    view.mkdir()
    payload, manifest, canonical = metadata_view(view)
    broker = TLSBroker(tmp_path, payload, manifest, canonical)
    token = tmp_path / "token"
    token.write_text("projected-token")
    config = tmp_path / "endpoints.json"
    config.write_text(
        json.dumps(
            {
                "version": 1,
                "endpoints": [
                    {
                        "manufacturer": "nvidia",
                        "address": f"127.0.0.1:{broker.server.server_port}",
                        "serverName": "broker.test",
                        "caFile": str(broker.cert),
                        "podUID": "source-pod",
                        "podName": "source",
                        "podNamespace": "system",
                        "nodeName": "node",
                    }
                ],
            }
        )
    )
    env = {
        "GPUSTACK_MODEL_TRANSFER_BACKEND": "nixl",
        "GPUSTACK_MODEL_TRANSFER_ARTIFACT": "artifact",
        "GPUSTACK_MODEL_TRANSFER_ARTIFACT_UID": "model-fixture-tiny-dense",
        "GPUSTACK_MODEL_TRANSFER_MANIFEST_DIGEST": canonical,
        "GPUSTACK_MODEL_TRANSFER_MODEL_PATH": str(view),
        "GPUSTACK_MODEL_TRANSFER_TOKEN": str(token),
        "GPUSTACK_MODEL_TRANSFER_CONFIG": str(config),
        "KUBERNETES_POD_UID": "target-pod",
        "KUBERNETES_POD_NAME": "target",
        "KUBERNETES_POD_NAMESPACE": "tenant",
        "KUBERNETES_POD_IP": "127.0.0.1",
    }
    reader = EngineReader(
        env, native_factory=native_factory, target_validator=lambda d, t: True
    )
    return reader, broker, SimpleNamespace(model=str(view)), payload


@pytest.mark.parametrize("drip", ["headers", "body"])
def test_absolute_request_deadline_bounds_slow_tls_response(
    tmp_path, monkeypatch, drip
):
    from gpustack_model_transfer.client_transport import BrokerTransport, WireUnknown

    _reader, broker, _model, _payload = make_reader(tmp_path)

    def slow(handler):
        handler.receive()
        try:
            if drip == "headers":
                handler.wfile.write(b"HTTP/1.0 200 OK\r\nX-Slow: ")
            else:
                handler.send_response(200)
                handler.send_header("Content-Length", "1000")
                handler.end_headers()
            for _ in range(100):
                handler.wfile.write(b" ")
                handler.wfile.flush()
                time.sleep(0.02)
        except OSError:
            pass

    monkeypatch.setattr(broker.server.RequestHandlerClass, "do_POST", slow)
    transport = BrokerTransport(
        {
            "address": f"127.0.0.1:{broker.server.server_port}",
            "serverName": "broker.test",
            "caFile": str(broker.cert),
        },
        str(tmp_path / "token"),
    )
    try:
        started = time.monotonic()
        with pytest.raises(WireUnknown):
            transport.request("POST", "/v1/transfers", {}, deadline=started + 0.15)
        assert time.monotonic() - started < 0.5
        assert getattr(transport._tls_local, "conn", None) is None
    finally:
        transport._close_connection()
        broker.close()


@pytest.mark.parametrize("protocol", ["HTTP/1.0", "HTTP/1.1"])
def test_reused_tls_connection_receives_each_requests_remaining_budget(
    tmp_path, monkeypatch, protocol
):
    from gpustack_model_transfer.client_transport import BrokerTransport, WireUnknown

    _reader, broker, _model, _payload = make_reader(tmp_path)
    calls = []

    def answer(handler):
        handler.receive()
        calls.append(handler.connection)
        if len(calls) > 1:
            time.sleep(0.08)
        handler.answer({"ok": True})

    handler = broker.server.RequestHandlerClass
    monkeypatch.setattr(handler, "protocol_version", protocol)
    monkeypatch.setattr(handler, "do_POST", answer)
    transport = BrokerTransport(
        {
            "address": f"127.0.0.1:{broker.server.server_port}",
            "serverName": "broker.test",
            "caFile": str(broker.cert),
        },
        str(tmp_path / "token"),
    )
    try:
        assert transport.request(
            "POST", "/v1/transfers", {}, deadline=time.monotonic() + 0.06
        ) == {"ok": True}
        assert transport.request(
            "POST", "/v1/transfers", {}, deadline=time.monotonic() + 0.5
        ) == {"ok": True}
        assert transport.request("POST", "/v1/transfers", {}) == {"ok": True}
        if protocol == "HTTP/1.1":
            assert calls[0] is calls[1] is calls[2]
        with pytest.raises(WireUnknown):
            transport.request(
                "POST", "/v1/transfers", {}, deadline=time.monotonic() - 1
            )
        assert len(calls) == 3
    finally:
        transport._close_connection()
        broker.close()


def test_operation_deadline_retains_native_owner_after_lost_post_reply(
    tmp_path, monkeypatch
):
    reader, broker, model, _ = make_reader(tmp_path)
    handler = broker.server.RequestHandlerClass
    original_answer = handler.answer
    submitted = threading.Event()

    def delayed_answer(handler, data):
        if handler.path == "/v1/transfers":
            submitted.set()
            time.sleep(0.15)
        with suppress(OSError):
            original_answer(handler, data)

    monkeypatch.setattr(handler, "answer", delayed_answer)
    try:
        descriptor = next(d for d in reader.probe(model) if d.name == "lm_head.weight")
        session = reader.start(model)
        reader.timeout = 0.06
        target = SimpleNamespace(
            data=bytearray(48), device=SimpleNamespace(type="cuda")
        )
        with pytest.raises(SourceUnavailableError):
            session.fill(descriptor, target)
        assert submitted.is_set()
        assert session.unknown and session.inflight is not None
        assert not session.native.closed
        assert len([1 for path, _ in broker.calls if path == "/v1/transfers"]) == 1
        assert not any(path.endswith("/ack") for path, _ in broker.calls)
        with pytest.raises(SourceUnavailableError, match="retains unknown"):
            session.close()
        assert not session.native.closed
    finally:
        broker.close()


def test_real_tls_metadata_probe_and_verified_chunked_fill(tmp_path):
    created = []

    def factory(*args, **kwargs):
        item = FakeWindow(*args, **kwargs)
        created.append(item)
        return item

    reader, broker, model, payload = make_reader(tmp_path, factory)
    try:
        descs = reader.probe(model)
        assert not created
        assert broker.calls[0][1]["sourceGeneration"] == 0
        session = reader.start(model)
        assert not created
        broker.lose_ack_once = True
        descriptor = next(d for d in descs if d.name == "lm_head.weight")
        tensor = reader.manifest.tensor(descriptor.name)
        target = SimpleNamespace(
            data=bytearray(tensor.byte_size), device=SimpleNamespace(type="cuda")
        )
        session.fill(descriptor, target)
        assert bytes(target.data) == payload[tensor.data_begin : tensor.data_end]
        assert created[0].events == ["register"] + [
            x for _ in range(6) for x in ("copy", "synchronize")
        ]
        session.close()
        assert created[0].closed
        assert broker.calls[-1][1]["targetReleased"] is True
        assert broker.calls[3][1]["sourceGeneration"] == 1
    finally:
        broker.close()


def test_owned_output_budget_refuses_before_native_start(tmp_path):
    reader, broker, model, _ = make_reader(
        tmp_path, lambda *a: pytest.fail("native factory above owned-output budget")
    )
    reader.budgets = ReadBudgets(8, 1, 1)
    try:
        with pytest.raises(SourceUnavailableError):
            reader.probe(model)
        assert all(path.startswith("/v1/sessions") for path, _ in broker.calls)
        assert broker.calls[-1][1]["targetReleased"] is True
    finally:
        broker.close()


def test_configured_window_bounds_actual_transfer_chunks(tmp_path):
    reader, broker, model, payload = make_reader(tmp_path)
    reader.budgets = ReadBudgets(4, 1, 1024)
    try:
        descriptors = reader.probe(model)
        session = reader.start(model)
        descriptor = next(d for d in descriptors if d.name == "lm_head.weight")
        tensor = reader.manifest.tensor(descriptor.name)
        target = SimpleNamespace(
            data=bytearray(tensor.byte_size), device=SimpleNamespace(type="cuda")
        )
        session.fill(descriptor, target)
        assert session.native.size == 4
        transfers = [body for path, body in broker.calls if path == "/v1/transfers"]
        assert len(transfers) == 12
        assert all(request["length"] <= 4 for request in transfers)
        assert bytes(target.data) == payload[tensor.data_begin : tensor.data_end]
        session.close()
    finally:
        broker.close()


@pytest.mark.parametrize(
    "options",
    [
        {"window_bytes": 0},
        {"window_bytes": True},
        {"max_inflight": 257},
        {"owned_output_bytes": -1},
        {"unrecognized": 1},
        [1],
    ],
)
def test_factory_refuses_invalid_budgets_before_environment(options):
    from gpustack_model_transfer.client import create_engine_reader
    from gpustack_model_transfer.core.errors import BudgetError

    with pytest.raises((SourceUnavailableError, BudgetError)):
        create_engine_reader(SimpleNamespace(model_loader_extra_config=options))


def test_unknown_result_retains_target_and_never_releases(tmp_path):
    reader, broker, model, _ = make_reader(tmp_path)
    try:
        descs = reader.probe(model)
        session = reader.start(model)
        broker.unknown = True
        descriptor = next(d for d in descs if d.name == "lm_head.weight")
        target = SimpleNamespace(
            data=bytearray(48), device=SimpleNamespace(type="cuda")
        )
        with pytest.raises(SourceUnavailableError):
            session.fill(descriptor, target)
        assert session.unknown and session.inflight and session.targets == [target]
        session.abort()
        assert not session.native.closed
    finally:
        broker.close()


def test_probe_refuses_payload_and_no_native_factory(tmp_path):
    reader, broker, model, _ = make_reader(
        tmp_path, lambda *a: pytest.fail("native factory during probe")
    )
    try:
        with open(os.path.join(model.model, "checkpoint.safetensors"), "ab") as file:
            file.write(b"payload")
        with pytest.raises(SourceUnavailableError):
            reader.probe(model)
        assert not broker.calls
    finally:
        broker.close()


def test_session_open_loss_replays_same_request_and_refreshes_token(tmp_path):
    reader, broker, model, _ = make_reader(tmp_path)
    try:
        broker.lose_open_once = True
        reader.probe(model)
        requests = [body for path, body in broker.calls if path == "/v1/sessions"]
        assert len(requests) == 2 and requests[0] == requests[1]
        Path(reader.token_file).write_text("refreshed-token")
        broker.expected_token = "refreshed-token"
        session = reader.start(model)
        session.close()
    finally:
        broker.close()


def test_changed_source_incarnation_refuses_before_native_start(tmp_path):
    reader, broker, model, _ = make_reader(
        tmp_path, lambda *a: pytest.fail("native was constructed")
    )
    try:
        reader.probe(model)
        broker.source_instance = "replacement"
        with pytest.raises(SourceUnavailableError):
            reader.start(model)
        assert not any(path == "/v1/transfers" for path, _ in broker.calls)
    finally:
        broker.close()


def test_untrusted_tls_name_refuses_probe(tmp_path):
    reader, broker, model, _ = make_reader(tmp_path)
    try:
        reader.endpoints[0]["serverName"] = "wrong.test"
        with pytest.raises(SourceUnavailableError):
            reader.probe(model)
        assert not broker.calls
    finally:
        broker.close()


def test_real_factory_is_importable_without_vendor_modules(monkeypatch, tmp_path):
    from gpustack_model_transfer.adapters.engine_reader import create_reader_provider

    reader, broker, _, _ = make_reader(tmp_path)
    env = {
        "GPUSTACK_MODEL_TRANSFER_BACKEND": reader.backend,
        "GPUSTACK_MODEL_TRANSFER_ARTIFACT": reader.artifact,
        "GPUSTACK_MODEL_TRANSFER_ARTIFACT_UID": reader.artifact_uid,
        "GPUSTACK_MODEL_TRANSFER_MANIFEST_DIGEST": reader.digest,
        "GPUSTACK_MODEL_TRANSFER_MODEL_PATH": reader.model_path,
        "GPUSTACK_MODEL_TRANSFER_TOKEN": reader.token_file,
        "GPUSTACK_MODEL_TRANSFER_CONFIG": str(tmp_path / "endpoints.json"),
        "KUBERNETES_POD_UID": reader.pod_uid,
        "KUBERNETES_POD_NAME": reader.pod_name,
        "KUBERNETES_POD_NAMESPACE": reader.pod_namespace,
        "KUBERNETES_POD_IP": reader.pod_ip,
    }
    try:
        for name, value in env.items():
            monkeypatch.setenv(name, value)
        assert isinstance(create_reader_provider(object()), EngineReader)
    finally:
        broker.close()


def test_open_response_loss_can_resume_original_request_id(tmp_path):
    reader, broker, model, _ = make_reader(tmp_path)
    try:
        broker.lose_open_count = 3
        with pytest.raises(SourceUnavailableError):
            reader.probe(model)
        old = reader.probe_request["requestID"]
        assert reader.pending_opens
        reader.probe(model)
        requests = [body for path, body in broker.calls if path == "/v1/sessions"]
        assert len(requests) == 4
        assert {request["requestID"] for request in requests} == {old}
        assert not reader.pending_opens
    finally:
        broker.close()


def test_final_close_response_loss_retries_without_double_native_cleanup(tmp_path):
    reader, broker, model, _ = make_reader(tmp_path)
    try:
        descriptor = next(d for d in reader.probe(model) if d.name == "lm_head.weight")
        session = reader.start(model)
        target = SimpleNamespace(
            data=bytearray(48), device=SimpleNamespace(type="cuda")
        )
        session.fill(descriptor, target)
        broker.lose_close_count = 3
        with pytest.raises(SourceUnavailableError):
            session.close()
        assert session.native.closed and session.native.events.count("deregister") == 1
        assert not session.closed and not session.unknown
        session.close()
        assert session.closed and session.native.events.count("deregister") == 1
        final_requests = [
            body
            for path, body in broker.calls
            if path.startswith("/v1/sessions/") and not path.endswith("/closing")
        ]
        assert len({item["requestID"] for item in final_requests[-4:]}) == 1
    finally:
        broker.close()


def test_live_session_graph_survives_python_gc(tmp_path):
    import gc
    import weakref

    reader, broker, model, _ = make_reader(tmp_path)
    try:
        descriptor = next(d for d in reader.probe(model) if d.name == "lm_head.weight")
        session = reader.start(model)
        target = SimpleNamespace(
            data=bytearray(48), device=SimpleNamespace(type="cuda")
        )
        session.fill(descriptor, target)
        reference = weakref.ref(session)
        del session
        gc.collect()
        assert reference() is not None and reference().targets == [target]
        reference().close()
    finally:
        broker.close()


def test_lost_transfer_response_uses_status_without_reposting(tmp_path):
    reader, broker, model, payload = make_reader(tmp_path)
    try:
        descriptor = next(d for d in reader.probe(model) if d.name == "lm_head.weight")
        session = reader.start(model)
        broker.lose_transfer_once = True
        target = SimpleNamespace(
            data=bytearray(48), device=SimpleNamespace(type="cuda")
        )
        session.fill(descriptor, target)
        tensor = reader.manifest.tensor(descriptor.name)
        assert bytes(target.data) == payload[tensor.data_begin : tensor.data_end]
        requests = [body for path, body in broker.calls if path == "/v1/transfers"]
        assert len(requests) == 6
        assert len({body["requestID"] for body in requests}) == 6
        session.close()
    finally:
        broker.close()


def test_probe_close_response_loss_resumes_same_session_close(tmp_path):
    reader, broker, model, _ = make_reader(tmp_path)
    try:
        broker.lose_close_count = 3
        with pytest.raises(SourceUnavailableError):
            reader.probe(model)
        assert len(reader._metadata_closes) == 1
        session_id, record = next(iter(reader._metadata_closes.items()))
        close_request = record["requestID"]
        reader.probe(model)
        attempts = [
            body for path, body in broker.calls if path == "/v1/sessions/" + session_id
        ]
        assert len(attempts) == 4
        assert {body["requestID"] for body in attempts} == {close_request}
        assert not reader._metadata_closes
    finally:
        broker.close()


def test_prepost_refusal_closes_without_unknown_or_post_retry(tmp_path):
    reader, broker, model, _ = make_reader(tmp_path)
    try:
        descriptor = next(d for d in reader.probe(model) if d.name == "lm_head.weight")
        session = reader.start(model)
        broker.refuse_prepost = True
        target = SimpleNamespace(
            data=bytearray(48), device=SimpleNamespace(type="cuda")
        )
        with pytest.raises(SourceUnavailableError):
            session.fill(descriptor, target)
        assert not session.unknown and session.inflight is None
        assert bytes(target.data) == bytes(48)
        session.abort()
        assert session.closed
        assert len([1 for path, _ in broker.calls if path == "/v1/transfers"]) == 1
    finally:
        broker.close()


def test_probe_fails_over_only_after_definite_pre_admission_refusal(tmp_path):
    reader, first, model, payload = make_reader(tmp_path)
    second_dir = tmp_path / "second"
    second_dir.mkdir()
    second = TLSBroker(second_dir, payload, first.manifest, first.canonical)
    first.pre_admission_refusal = True
    reader.endpoints.append(
        {
            "manufacturer": "nvidia",
            "address": f"127.0.0.1:{second.server.server_port}",
            "serverName": "broker.test",
            "caFile": str(second.cert),
            "podUID": "source-pod",
            "podName": "second",
            "podNamespace": "system",
            "nodeName": "other",
        }
    )
    try:
        assert reader.probe(model)
        assert len(first.calls) == 1
        assert reader.endpoint["podName"] == "second"
        session = reader.start(model)
        session.close()
        assert len(first.calls) == 1
    finally:
        first.close()
        second.close()


def test_may_have_admitted_then_connect_failure_never_fails_over(tmp_path):
    from gpustack_model_transfer.client_transport import WirePreAdmission, WireUnknown

    reader, broker, model, _ = make_reader(tmp_path)
    first = reader.endpoints[0]
    second = dict(first, podName="other")
    reader.endpoints.append(second)
    attempts = []

    class ScriptedTransport:
        def __init__(self, endpoint, token_file):
            attempts.append(("construct", endpoint["podName"]))

        def request(self, method, path, body, *, deadline=None):
            attempts.append(("post", body["requestID"]))
            if len([entry for entry in attempts if entry[0] == "post"]) == 1:
                raise WireUnknown("lost response")
            raise WirePreAdmission("connection failed")

    reader._transport_factory = ScriptedTransport
    try:
        with pytest.raises(SourceUnavailableError):
            reader.probe(model)
        assert [item for item in attempts if item[0] == "construct"] == [
            ("construct", first["podName"])
        ]
        posts = [item[1] for item in attempts if item[0] == "post"]
        assert len(posts) == 2 and len(set(posts)) == 1
        assert reader.pending_opens and reader.poisoned
        assert reader.endpoint is first
    finally:
        broker.close()


def test_tampered_config_refused_against_authenticated_manifest(tmp_path):
    reader, broker, model, _ = make_reader(
        tmp_path, lambda *args: pytest.fail("native during probe")
    )
    try:
        root = Path(model.model)
        bad = b'{"model_type":"altered"}'
        (root / "config.json").write_bytes(bad)
        view = json.loads((root / "view.json").read_text())
        item = next(item for item in view["files"] if item["name"] == "config.json")
        item["size"], item["sha256"] = len(bad), hashlib.sha256(bad).hexdigest()
        (root / "view.json").write_text(json.dumps(view))
        with pytest.raises(SourceUnavailableError):
            reader.probe(model)
        assert not any(path == "/v1/transfers" for path, _ in broker.calls)
    finally:
        broker.close()


def test_lost_post_then_missing_source_record_never_reposts(tmp_path):
    reader, broker, model, _ = make_reader(tmp_path)
    try:
        descriptor = next(d for d in reader.probe(model) if d.name == "lm_head.weight")
        session = reader.start(model)
        broker.lose_transfer_once = True
        broker.drop_status = True
        target = SimpleNamespace(
            data=bytearray(48), device=SimpleNamespace(type="cuda")
        )
        with pytest.raises(SourceUnavailableError):
            session.fill(descriptor, target)
        assert session.unknown and session.inflight is not None
        assert len([1 for path, _ in broker.calls if path == "/v1/transfers"]) == 1
        assert not session.native.closed
    finally:
        broker.close()


def test_forged_listing_digest_is_refused_before_session_open(tmp_path):
    reader, broker, model, _ = make_reader(
        tmp_path, lambda *args: pytest.fail("native during probe")
    )
    try:
        root = Path(model.model)
        listing = json.loads((root / "manifest.json").read_text())
        listing["entries"][0]["size"] += 1
        encoded = json.dumps(listing).encode()
        (root / "manifest.json").write_bytes(encoded)
        view = json.loads((root / "view.json").read_text())
        item = next(item for item in view["files"] if item["name"] == "manifest.json")
        item["size"], item["sha256"] = len(encoded), hashlib.sha256(encoded).hexdigest()
        (root / "view.json").write_text(json.dumps(view))
        with pytest.raises(SourceUnavailableError):
            reader.probe(model)
        assert not broker.calls
    finally:
        broker.close()


def test_dynamic_model_window_sizing_and_live_transfer_adaptation(tmp_path):
    class CoalescingFakeWindow(FakeWindow):
        supports_window_offset = True

        def __init__(self, backend, pod_ip, size, *, memory_mode="host", device=None):
            self.size = size
            self.storage = ctypes.create_string_buffer(self.size)
            self.block = self
            self.registration_id, self.window_id = "registration", "window"
            self.events = []
            self.closed = False

        def copy_to(self, target, offset, length, window_offset=0):
            target.data[offset : offset + length] = self.view(window_offset, length)
            self.events.extend(("copy", "synchronize"))

    reader, broker, model, payload = make_reader(tmp_path, CoalescingFakeWindow)
    reader.budgets = ReadBudgets(4 << 30, 1, 4 << 30)
    reader.timeout = 10.0
    try:
        descriptors = reader.probe(model)
        session = reader.start(model)
        total_span = sum(t.byte_size for t in reader.manifest.tensors)
        assert session.window_bytes == total_span
        assert session.adaptive_span_bytes == total_span
        targets = []
        for desc in descriptors:
            tensor = reader.manifest.tensor(desc.name)
            target = SimpleNamespace(
                data=bytearray(tensor.byte_size), device=SimpleNamespace(type="cuda")
            )
            session.fill(desc, target)
            targets.append((tensor, target))
        assert session.native.size == total_span
        transfers = [body for path, body in broker.calls if path == "/v1/transfers"]
        assert len(transfers) == 1
        assert transfers[0]["length"] == total_span
        for tensor, target in targets:
            assert bytes(target.data) == payload[tensor.data_begin : tensor.data_end]
        # Verify adaptive step-down under high transfer latency and step-up on recovery
        session.native.size = 1024 << 20
        session.adaptive_span_bytes = 1024 << 20
        session._update_adaptive_window(1024 << 20, 5.0)
        assert session.adaptive_span_bytes == 512 << 20
        session._update_adaptive_window(512 << 20, 0.2)
        assert session.adaptive_span_bytes > 512 << 20
        session.close()
    finally:
        broker.close()


def _second_broker(tmp_path, name):
    """One more identical TLS source: same fixture bytes, same digest."""
    second_dir = tmp_path / name
    second_dir.mkdir()
    payload, manifest, canonical = metadata_view(second_dir)
    return TLSBroker(second_dir, payload, manifest, canonical)


def test_probe_fails_over_from_a_capacity_refusal_to_a_free_peer(tmp_path):
    reader, first, model, _ = make_reader(tmp_path)
    second = _second_broker(tmp_path, "second-view")
    first.capacity_refusals = 1
    reader.endpoints.append(
        {
            "manufacturer": "nvidia",
            "address": f"127.0.0.1:{second.server.server_port}",
            "serverName": "broker.test",
            "caFile": str(second.cert),
            "podUID": "source-pod",
            "podName": "second",
            "podNamespace": "system",
            "nodeName": "other",
        }
    )
    try:
        assert reader.probe(model)
        assert reader.endpoint["podName"] == "second"
        assert first.calls[-1][0] == "/v1/sessions"
    finally:
        first.close()
        second.close()


def test_a_full_node_retries_after_the_named_wait(tmp_path, monkeypatch):
    import time as time_module

    reader, broker, model, _ = make_reader(tmp_path)
    reader.timeout = 10.0
    sleeps = []
    real_sleep = time_module.sleep

    def recording_sleep(seconds):
        sleeps.append(seconds)
        real_sleep(seconds)

    monkeypatch.setattr(time_module, "sleep", recording_sleep)
    broker.capacity_refusals = 2
    try:
        assert reader.probe(model)
        assert len(sleeps) == 2, "each full sweep of named waits sleeps once"
        assert 0 < sleeps[0] <= 0.05
        opens = [body for path, body in broker.calls if path == "/v1/sessions"]
        assert len(opens) == 3, "each refused attempt opens again after the wait"
    finally:
        broker.close()


def test_a_busy_peer_with_no_named_wait_is_not_retried(tmp_path):
    reader, broker, model, _ = make_reader(tmp_path)
    reader.endpoints = reader.endpoints[:1]
    reader.timeout = 0.5
    broker.capacity_refusals = 10_000
    try:
        started = time.monotonic()
        with pytest.raises(SourceUnavailableError):
            reader.probe(model)
        assert time.monotonic() - started < 5, "the sweep ends with the deadline"
    finally:
        broker.close()


@pytest.mark.parametrize("paged", [False, True], ids=["inline", "paged"])
def test_start_switches_peers_when_the_pinned_one_is_full(tmp_path, paged):
    reader, first, model, _ = make_reader(tmp_path)
    second = _second_broker(tmp_path, "start-second-view")
    if paged:
        use_paged_manifest(first)
        use_paged_manifest(second)
    second.source_instance = "second-instance"
    reader.endpoints.append(
        {
            "manufacturer": "nvidia",
            "address": f"127.0.0.1:{second.server.server_port}",
            "serverName": "broker.test",
            "caFile": str(second.cert),
            "podUID": "source-pod",
            "podName": "second",
            "podNamespace": "system",
            "nodeName": "other",
        }
    )
    try:
        assert reader.probe(model)
        assert reader.endpoint["podName"] == "source"
        first.capacity_refusals = 1
        session = reader.start(model)
        assert reader.endpoint["podName"] == "second"
        assert session.manifest.identity() == reader.manifest.identity()
        assert session.identity[2] == "second-instance"
        assert session.identity == reader.identity
        session.close()
        assert any(
            body.get("requestID")
            for path, body in first.calls
            if path == "/v1/sessions"
        ), "the pinned peer was asked first"
    finally:
        first.close()
        second.close()


@pytest.mark.parametrize("paged", [False, True], ids=["inline", "paged"])
def test_start_refuses_a_peer_that_changes_its_incarnation_in_place(tmp_path, paged):
    reader, broker, model, _ = make_reader(tmp_path)
    if paged:
        use_paged_manifest(broker)
    try:
        assert reader.probe(model)
        pinned = reader.identity
        broker.source_instance = "a-new-instance"
        with pytest.raises(SourceUnavailableError):
            reader.start(model)
        assert reader.identity == pinned, "a same-peer new incarnation is never adopted"
    finally:
        broker.close()


def use_paged_manifest(broker):
    manifest = dict(broker.manifest_obj)
    pages = []
    for tensor in sorted(manifest["tensors"], key=lambda t: t["data_begin"]):
        raw = json.dumps([tensor], separators=(",", ":"))
        digest = hashlib.sha256(raw.encode()).hexdigest()
        broker.manifest_pages[digest] = raw
        pages.append(
            {
                "sha256": digest,
                "size": len(raw.encode()),
                "count": 1,
                "file": tensor["file"],
                "begin": tensor["data_begin"],
                "end": tensor["data_end"],
            }
        )
    manifest["tensors"] = []
    manifest["tensor_pages"] = pages
    broker.manifest_obj = manifest
    broker.manifest = json.dumps(manifest, separators=(",", ":")).encode()


def test_paged_probe_and_start_accept_independent_verified_indexes(tmp_path):
    from gpustack_model_transfer.core.tensor_index import TensorIndex

    reader, broker, model, _ = make_reader(tmp_path)
    use_paged_manifest(broker)
    try:
        reader.probe(model)
        probed = reader.manifest
        session = reader.start(model)
        assert isinstance(probed.tensors, TensorIndex)
        assert isinstance(session.manifest.tensors, TensorIndex)
        assert probed.tensors is not session.manifest.tensors
        assert probed.identity() == session.manifest.identity()
        pages = [body for path, body in broker.calls if path == "/v1/manifest-pages"]
        assert len(pages) == 2 * len(broker.manifest_pages)
        session.close()
    finally:
        broker.close()


@pytest.mark.parametrize(
    "change",
    ["encoding", "artifact", "generation", "contract", "files", "page"],
)
def test_paged_start_refuses_changed_verified_content(tmp_path, change):
    reader, broker, model, _ = make_reader(tmp_path)
    use_paged_manifest(broker)
    try:
        reader.probe(model)
        pinned = reader.identity
        if change == "encoding":
            broker.manifest += b" "
        elif change == "page":
            digest = next(iter(broker.manifest_pages))
            broker.manifest_pages[digest] += " "
        else:
            manifest = json.loads(broker.manifest)
            if change == "artifact":
                manifest["artifact_uid"] = "different-artifact"
            elif change == "generation":
                manifest["source_generation"] += 1
            elif change == "contract":
                manifest["contract_version"] += 1
            else:
                manifest["files"][0]["size"] += 1
            broker.manifest_obj = manifest
            broker.manifest = json.dumps(manifest).encode()
        with pytest.raises(SourceUnavailableError):
            reader.start(model)
        assert reader.identity == pinned
        assert not any(path == "/v1/transfers" for path, _ in broker.calls)
    finally:
        broker.close()


def _window_session(reader_window, max_needed, supports_span=False, source_window=None):
    session = object.__new__(EngineSession)
    session.reader = SimpleNamespace(
        budgets=SimpleNamespace(window_bytes=reader_window),
        native_factory=SimpleNamespace(supports_window_offset=supports_span),
        timeout=30,
    )
    session._max_tensor_bytes = max_needed
    session._max_contiguous_span = max_needed
    session._source_window_bytes = source_window
    return session


def test_the_session_window_clamps_to_the_source_window():
    gib, mib = 1 << 30, 1 << 20
    # A client budget larger than the source buffer clamps to it, floored
    # at the adaptive minimum, so transfers stay in bounds and chunked.
    assert (
        _window_session(
            16 * gib, 4 * gib, source_window=1 * gib
        )._resolve_window_bytes()
        == 1 * gib
    )
    assert (
        _window_session(
            128 * gib, 4 * gib, source_window=512 * mib
        )._resolve_window_bytes()
        == 512 * mib
    )
    # A named buffer under the adaptive floor never goes below it.
    assert (
        _window_session(
            8 * gib, 4 * gib, source_window=100 * mib
        )._resolve_window_bytes()
        == 256 * mib
    )
    # No named buffer, or one at least as large as the budget, changes nothing.
    assert _window_session(8 * gib, 4 * gib)._resolve_window_bytes() == 4 * gib
    assert (
        _window_session(1 * gib, 4 * gib, source_window=4 * gib)._resolve_window_bytes()
        == 1 * gib
    )
    # The window still covers only what the model needs.
    assert (
        _window_session(
            8 * gib, 128 * mib, source_window=1 * gib
        )._resolve_window_bytes()
        == 128 * mib
    )


def test_an_invalid_source_window_figure_is_refused(tmp_path):
    reader, broker, model, _ = make_reader(
        tmp_path, lambda *args: pytest.fail("native during probe")
    )
    try:
        reader.probe(model)
        session_id = "s-probe"
        good = {
            "sessionID": session_id,
            "requestID": reader.probe_request["requestID"],
            "generation": "generation-1",
            "sourcePodUID": "source-pod",
            "workerInstanceID": broker.source_instance,
            "manifestSHA256": hashlib.sha256(broker.manifest).hexdigest(),
            "manifestJSON": broker.manifest.decode(),
            "digest": broker.canonical,
            "artifactUID": broker.manifest_obj["artifact_uid"],
            "sourceGeneration": 1,
        }
        layouts, hashes, listing_verified = load_view(reader.model_path, reader.digest)
        for bad in (True, -1, "big", (1 << 40) + 1):
            response = dict(good, sourceWindowBytes=bad)
            with pytest.raises(SourceUnavailableError):
                reader._validate(response, layouts, hashes, listing_verified)
    finally:
        broker.close()


@pytest.mark.parametrize("path", ["checkpoint.safetensors", "config.json"])
def test_manifest_file_size_must_match_authenticated_listing(tmp_path, path):
    reader, broker, model, _ = make_reader(tmp_path)
    try:
        for item in broker.manifest_obj["files"]:
            if item["path"] == path:
                item["size"] += 1
        broker.manifest = json.dumps(broker.manifest_obj).encode()
        with pytest.raises(SourceUnavailableError):
            reader.probe(model)
        assert not any(path == "/v1/transfers" for path, _ in broker.calls)
    finally:
        broker.close()


def test_target_storage_size_rejected_before_native_allocation(tmp_path):
    from gpustack_model_transfer.client import _target_valid

    reader, broker, model, _ = make_reader(
        tmp_path, lambda *args: pytest.fail("native allocation before size check")
    )
    try:
        descriptor = next(d for d in reader.probe(model) if d.dtype == "F32")
        session = reader.start(model)
        reader.target_validator = _target_valid
        target = SimpleNamespace(
            shape=descriptor.shape,
            dtype="torch.float32",
            device=SimpleNamespace(type="cpu"),
            is_contiguous=lambda: True,
            numel=lambda: 1,
            element_size=lambda: 1,
        )
        with pytest.raises(SourceUnavailableError):
            session.fill(descriptor, target)
        assert not any(path == "/v1/transfers" for path, _ in broker.calls)
        session.close()
    finally:
        broker.close()


@pytest.mark.parametrize("phase", ["open", "page", "closing", "release"])
def test_session_requests_share_absolute_transport_deadlines(
    tmp_path, monkeypatch, phase
):
    import gpustack_model_transfer.client as client
    from gpustack_model_transfer.client_transport import WireUnknown

    reader, broker, _, _ = make_reader(tmp_path)
    now, calls = [0.0], []
    reader.timeout = 1.0

    def sleep(delay):
        now[0] += delay

    def request(method, path, body, *, deadline=None):
        calls.append((path, deadline))
        assert deadline == 1.0
        now[0] = deadline
        raise WireUnknown("response deadline")

    monkeypatch.setattr(
        client, "time", SimpleNamespace(monotonic=lambda: now[0], sleep=sleep)
    )
    reader.transport = SimpleNamespace(request=request)
    try:
        with pytest.raises(WireUnknown):
            if phase == "open":
                reader._open("weight", 0, deadline=1.0)
            elif phase == "page":
                reader._manifest_page(
                    {
                        "sessionID": "session",
                        "sourcePodUID": "source",
                        "workerInstanceID": "worker",
                        "generation": "generation",
                        "manifestSHA256": "manifest",
                    },
                    {"sha256": "page"},
                    deadline=1.0,
                )
            else:
                reader._close_phase("session", "close", finalize=phase == "release")
        assert now[0] == 1.0 and len(calls) == 1
        if phase == "open":
            assert reader.pending_opens and reader.poisoned
        elif phase != "page":
            assert reader.pending_closes and reader.poisoned
    finally:
        broker.close()


@pytest.mark.parametrize(
    "raw", ["0", "-1", "18O0", "NaN", "Infinity", "-Infinity", True]
)
def test_transfer_timeout_rejects_invalid_values(raw):
    from gpustack_model_transfer.client import transfer_timeout
    from gpustack_model_transfer.core.errors import SourceUnavailableError

    with pytest.raises(SourceUnavailableError):
        transfer_timeout({"GPUSTACK_MODEL_TRANSFER_TIMEOUT": raw})


@pytest.mark.parametrize(
    "raw,seconds,hccl", [(None, 1800, "1800"), ("0.5", 0.5, "1"), ("2.1", 2.1, "3")]
)
def test_transfer_timeout_and_collective_timeout_agree(raw, seconds, hccl):
    from gpustack_model_transfer.envs import get_envs

    env = get_envs({"GPUSTACK_MODEL_TRANSFER_TIMEOUT": raw})
    assert seconds == env.GPUSTACK_MODEL_TRANSFER_TIMEOUT
    assert hccl == env.GPUSTACK_MODEL_TRANSFER_TIMEOUT_STR
