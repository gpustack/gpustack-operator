"""CPU helper contract and opt-in actual Go/Python integration entrypoint.

A local fake broker here checks the helper only. Cross-language acceptance is
reported solely when the separately owned real Go integration test passes.
"""

import ctypes
import hashlib
import http.client
import json
import subprocess
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from multiprocessing import shared_memory
from pathlib import Path

from gpustack_model_transfer.client_metadata import load_view, verify_layout
from gpustack_model_transfer.core.manifest import parse_manifest

HELPER = Path(__file__).with_name("go_fixture.py")


def send(process, message):
    process.stdin.write(json.dumps(message) + "\n")
    process.stdin.flush()
    result = process.stdout.readline()
    assert result, "fixture exited without a JSON response"
    return json.loads(result)


def launch():
    return subprocess.Popen(
        [sys.executable, str(HELPER)],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        bufsize=1,
    )


def prepare(root):
    process = launch()
    try:
        result = send(process, {"mode": "prepare", "root": str(root)})
        assert process.wait(timeout=5) == 0
        assert result["event"] == "prepared"
        return result["fixtures"]
    finally:
        process.stdin.close()
        process.stdout.close()
        process.stderr.close()


def test_cpu_fixture_prepares_two_independent_authenticated_artifacts(tmp_path):
    fixtures = prepare(tmp_path / "fixture")
    assert len(fixtures) == 2
    assert fixtures[0]["namespace"] != fixtures[1]["namespace"]
    assert fixtures[0]["artifactUID"] != fixtures[1]["artifactUID"]
    assert fixtures[0]["digest"] == fixtures[1]["digest"]
    for item in fixtures:
        raw = item["manifestJSON"].encode()
        assert hashlib.sha256(raw).hexdigest() == item["manifestSHA256"]
        manifest = parse_manifest(raw, item["manifestSHA256"])
        assert manifest.artifact_uid == item["artifactUID"]
        assert manifest.source_generation == item["sourceGeneration"]
        layouts, hashes, listing_verified = load_view(item["modelPath"], item["digest"])
        verify_layout(manifest, layouts, hashes, listing_verified)
        referenced = {tensor.file for tensor in manifest.tensors}
        assert referenced == {"checkpoint.safetensors"}
        assert any(
            file.path == "config.json" and file.size > 0 for file in manifest.files
        )
        assert item["registrations"] == len(referenced)
        assert (
            item["totalBytes"]
            == (Path(item["treePath"]) / "checkpoint.safetensors").stat().st_size
        )
        first = manifest.tensors[0]
        blob = (Path(item["treePath"]) / first.file).read_bytes()
        assert (
            hashlib.sha256(blob[first.data_begin : first.data_end]).hexdigest()
            == item["expectedFirstTensorSHA256"]
        )


class FixtureControl:
    """Fake Go control used only to self-test the subprocess harness."""

    def __init__(self, fixtures, token, worker):
        self.token = token
        self.worker = worker
        self.orders = []
        self.events = []
        self.answered_acks = []
        self.draining = False
        for index, item in enumerate(fixtures):
            self.orders.append(
                {
                    **{
                        key: item[key]
                        for key in (
                            "namespace",
                            "digest",
                            "artifactUID",
                            "sourceGeneration",
                            "manifestSHA256",
                            "manifestJSON",
                            "treePath",
                            "totalBytes",
                            "registrations",
                        )
                    },
                    "generation": f"generation-{index + 1}",
                    "workerSecret": "secret-" + str(index + 1),
                }
            )
        broker = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def answer(self, value, status=200):
                raw = json.dumps(value).encode()
                self.send_response(status)
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def do_GET(self):
                if (
                    self.path != "/v1/assignments"
                    or self.headers.get("Authorization") != broker.token
                ):
                    self.answer({}, 403)
                    return
                self.answer({"assignments": [] if broker.draining else broker.orders})

            def do_POST(self):
                body = json.loads(
                    self.rfile.read(int(self.headers.get("Content-Length", "0")))
                )
                assert (
                    body["workerID"] == broker.worker
                    and body["workerInstanceID"] == "instance-1"
                )
                broker.events.append((self.path, body))
                if self.path == "/v1/register":
                    self.answer({"leaseID": "lease-" + body["generation"]})
                elif self.path == "/v1/heartbeat":
                    self.answer(
                        {
                            "action": "drain" if broker.draining else "none",
                            "activeSessions": 0,
                            "unknownSessions": 0,
                        }
                    )
                elif self.path in (
                    "/v1/native-start",
                    "/v1/filled",
                    "/v1/drain-ack",
                    "/v1/quarantine",
                ):
                    self.answer({})
                    if self.path == "/v1/drain-ack":
                        # Recorded after the answer. The request entry proves
                        # only that the control received it; this entry
                        # proves that the control answered it.
                        broker.answered_acks.append((self.path, body))
                else:
                    self.answer({}, 404)

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    @property
    def address(self):
        return f"127.0.0.1:{self.server.server_port}"

    def close(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=3)


def wait_until(predicate):
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(0.01)
    raise AssertionError("fixture did not reach expected state")


def source_workers(process):
    """The source process's own report of its workers."""
    answer = send(process, {"op": "status"})
    assert answer["ok"]
    return answer["workers"]


def source_worker_states(process):
    """The source process's own report: generation to lifecycle state."""
    return {worker["generation"]: worker["state"] for worker in source_workers(process)}


def answered_generations(broker):
    """The generations whose drain acknowledgment the control answered."""
    return {body["generation"] for _path, body in broker.answered_acks}


def source_drained(process):
    """Every worker still listed is drained.

    The report can be empty: the control returns no assignments while it
    drains, and the supervisor reaps a worker whose state is drained or
    stopped. An empty report then means the worker may have been reaped,
    not that it drained. The answered drain acknowledgments are required
    separately; this report is not the evidence on its own.
    """
    states = source_worker_states(process)
    return all(state == "drained" for state in states.values())


def test_cpu_source_helper_uses_real_python_control_and_private_server(tmp_path):
    fixtures = prepare(tmp_path / "fixture")
    token_path = tmp_path / "bootstrap"
    token_path.write_text("private-token")
    broker = FixtureControl(fixtures, "private-token", "source-pod")
    process = launch()
    region = None
    try:
        ready = send(
            process,
            {
                "mode": "source",
                "bootstrapFile": str(token_path),
                "controlAddress": broker.address,
                "workerID": "source-pod",
                "workerInstanceID": "instance-1",
            },
        )
        assert ready["event"] == "ready"
        wait_until(lambda: len([e for e in broker.events if e[0] == "/v1/filled"]) == 2)
        assert len([e for e in broker.events if e[0] == "/v1/native-start"]) == 2
        for path, body in broker.events:
            if path == "/v1/filled":
                assert body["registrations"] == 1
                assert [item["name"] for item in body["descriptors"]] == [
                    "checkpoint.safetensors"
                ]
        source_status = send(process, {"op": "status"})
        assert {item["state"] for item in source_status["workers"]} == {"serving"}
        region = shared_memory.SharedMemory(create=True, size=16)
        raw = fixtures[0]["manifestJSON"].encode()
        tensor = parse_manifest(raw).tensors[0]
        import base64

        body = {
            "sourcePodUID": "source-pod",
            "workerInstanceID": "instance-1",
            "generation": "generation-1",
            "leaseID": "lease-generation-1",
            "sessionID": "session",
            "requestID": "operation",
            "targetPodUID": "target-pod",
            "targetInstanceID": "target-process",
            "registrationID": "registration",
            "windowID": "window",
            "tensor": tensor.name,
            "file": tensor.file,
            "fileOffset": tensor.data_begin,
            "tensorOffset": 0,
            "length": 16,
            "target": {
                "backend": "nixl",
                "address": ctypes.addressof(ctypes.c_char.from_buffer(region.buf)),
                "length": 16,
                "metadata": base64.b64encode(region.name.encode()).decode(),
                "endpoint": "",
            },
        }
        common = {
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
        owner = {
            key: body[key]
            for key in (
                "targetPodUID",
                "targetInstanceID",
                "registrationID",
                "windowID",
            )
        }
        host, port = ready["privateAddress"].split(":")

        def post(path, payload, expected=200):
            conn = http.client.HTTPConnection(host, int(port), timeout=3)
            try:
                conn.request(
                    "POST",
                    path,
                    json.dumps(payload),
                    {"Authorization": "private-token"},
                )
                answer = conn.getresponse()
                result = json.loads(answer.read())
                assert answer.status == expected
                return result
            finally:
                conn.close()

        assert post("/v1/operations", body)["state"] in ("dispatching", "submitted")
        wait_until(lambda: post("/v1/operations/status", common)["state"] == "terminal")
        source_file = Path(fixtures[0]["treePath"]) / tensor.file
        assert (
            bytes(region.buf[:16])
            == source_file.read_bytes()[tensor.data_begin : tensor.data_begin + 16]
        )
        post("/v1/operations/ack", common | owner)
        wait_until(
            lambda: post("/v1/operations/status", common)["state"] == "acknowledged"
        )

        # A fresh operation is admitted before freeze, then refused after freeze.
        second = body | {"requestID": "operation-before-freeze"}
        second_common = common | {"requestID": second["requestID"]}
        assert post("/v1/operations", second)["state"] in ("dispatching", "submitted")
        wait_until(
            lambda: post("/v1/operations/status", second_common)["state"] == "terminal"
        )
        post("/v1/operations/ack", second_common | owner)
        wait_until(
            lambda: (
                post("/v1/operations/status", second_common)["state"] == "acknowledged"
            )
        )
        conn = http.client.HTTPConnection(host, int(port), timeout=3)
        try:
            conn.request(
                "POST",
                "/v1/runtime/drain",
                headers={"Authorization": "Bearer private-token"},
            )
            answer = conn.getresponse()
            assert answer.status == 200 and json.loads(answer.read())["draining"]
        finally:
            conn.close()
        refused = post(
            "/v1/operations", body | {"requestID": "operation-after-freeze"}, 409
        )
        assert refused["kind"] == "refused"
        assert (
            post("/v1/sessions/close", common | {"requestID": "close"})["state"]
            == "closed"
        )
        # The evidence is the acknowledgment the control answered, not the
        # request it recorded before answering. This runs on the CPU fake:
        # the worker's own report is an observation of the control order,
        # not a fence. The report can also be empty, because the control
        # returns no assignments while it drains and the supervisor reaps a
        # worker whose state is drained or stopped. Reaping alone proves
        # nothing, so the answered acknowledgments are what this waits for.
        # The wait names both generations, so a retry of one generation
        # cannot stand in for the other generation's acknowledgment.
        states = source_worker_states(process)
        assert states
        assert all(state != "drained" for state in states.values())
        assert broker.answered_acks == []
        broker.draining = True
        wait_until(
            lambda: answered_generations(broker) >= {"generation-1", "generation-2"}
        )
        assert all(body["nativeReleased"] for _path, body in broker.answered_acks)
        wait_until(lambda: source_drained(process))
        assert send(process, {"op": "stop"}) == {"ok": True, "stopped": True}
        assert process.wait(timeout=5) == 0
    finally:
        if process.poll() is None:
            process.terminate()
            process.wait(timeout=5)
        if region is not None:
            region.close()
            region.unlink()
        process.stdin.close()
        process.stdout.close()
        process.stderr.close()
        broker.close()


def engine_env(root, fixture, tls):
    suffix = fixture["namespace"][-1]
    token = root / ("token-" + suffix)
    token.write_text("token-" + suffix)
    config = root / ("endpoints-" + suffix + ".json")
    config.write_text(
        json.dumps(
            {
                "version": 1,
                "endpoints": [
                    {
                        "manufacturer": "nvidia",
                        "address": f"127.0.0.1:{tls.server.server_port}",
                        "serverName": "broker.test",
                        "caFile": str(tls.cert),
                        "podUID": "source-pod",
                        "podName": "source",
                        "podNamespace": "system",
                        "nodeName": "node",
                    }
                ],
            }
        )
    )
    return {
        "GPUSTACK_MODEL_TRANSFER_BACKEND": "nixl",
        "GPUSTACK_MODEL_TRANSFER_ARTIFACT": fixture["artifactName"],
        "GPUSTACK_MODEL_TRANSFER_ARTIFACT_UID": fixture["artifactUID"],
        "GPUSTACK_MODEL_TRANSFER_MANIFEST_DIGEST": fixture["digest"],
        "GPUSTACK_MODEL_TRANSFER_MODEL_PATH": fixture["modelPath"],
        "GPUSTACK_MODEL_TRANSFER_TOKEN": str(token),
        "GPUSTACK_MODEL_TRANSFER_CONFIG": str(config),
        "KUBERNETES_POD_UID": "target-" + suffix,
        "KUBERNETES_POD_NAME": "engine-" + suffix,
        "KUBERNETES_POD_NAMESPACE": fixture["namespace"],
        "KUBERNETES_POD_IP": "127.0.0.1",
    }


def test_python_fixture_selftest_two_owners_with_fake_go_router(tmp_path):
    """Fake router tests fixture plumbing; this is not real Go evidence."""
    import runpy

    FakeTLSBroker = runpy.run_path(str(Path(__file__).with_name("go_fake_broker.py")))[
        "FakeTLSBroker"
    ]
    fixtures = prepare(tmp_path / "fixtures")
    bootstrap = tmp_path / "bootstrap"
    bootstrap.write_text("private-token")
    control = FixtureControl(fixtures, "private-token", "source-pod")
    source = launch()
    engines = []
    tls = None
    try:
        ready = send(
            source,
            {
                "mode": "source",
                "bootstrapFile": str(bootstrap),
                "controlAddress": control.address,
                "workerID": "source-pod",
                "workerInstanceID": "instance-1",
            },
        )
        wait_until(
            lambda: (
                len([event for event in control.events if event[0] == "/v1/filled"])
                == 2
            )
        )
        tls = FakeTLSBroker(
            tmp_path, fixtures, ready["privateAddress"], "private-token"
        )
        for fixture in fixtures:
            engine = launch()
            engines.append(engine)
            assert (
                send(
                    engine,
                    {"mode": "engine", "env": engine_env(tmp_path, fixture, tls)},
                )["event"]
                == "ready"
            )
            probe_result = send(engine, {"op": "probe"})
            assert probe_result["ok"], (probe_result, [path for path, _ in tls.calls])
            assert send(engine, {"op": "start"})["ok"]
            result = send(engine, {"op": "fill", "tensorIndex": 0})
            assert (
                result["ok"]
                and result["sha256"] == fixture["expectedFirstTensorSHA256"]
            )
            assert result["copies"] == 3 and result["syncs"] == 3
            assert send(engine, {"op": "close"})["closed"]
            assert send(engine, {"op": "exit"})["exit"]
            assert engine.wait(timeout=5) == 0
        control.draining = True
        # The request entry proves only receipt: the control records it
        # before answering. The answered set names both generations, and
        # the source's own report proves the workers reached drained or
        # were reaped before stop is sent.
        wait_until(
            lambda: answered_generations(control) >= {"generation-1", "generation-2"}
        )
        assert all(body["nativeReleased"] for _path, body in control.answered_acks)
        wait_until(lambda: source_drained(source))
        assert send(source, {"op": "stop"})["stopped"]
        assert source.wait(timeout=5) == 0
    finally:
        for process in [source, *engines]:
            if process.poll() is None:
                process.terminate()
                process.wait(timeout=5)
            process.stdin.close()
            process.stdout.close()
            process.stderr.close()
        if tls is not None:
            tls.close()
        control.close()


def test_fixture_selftest_survives_delayed_drain_ack_responses(tmp_path, monkeypatch):
    """A delayed acknowledgment response must not satisfy the stop wait."""
    original = BaseHTTPRequestHandler.end_headers

    def delayed_end_headers(handler):
        if handler.path == "/v1/drain-ack":
            time.sleep(0.25)
        return original(handler)

    monkeypatch.setattr(BaseHTTPRequestHandler, "end_headers", delayed_end_headers)
    test_python_fixture_selftest_two_owners_with_fake_go_router(tmp_path)


def test_cpu_fixture_ambiguous_post_holds_source_and_target(tmp_path):
    """A fake router exercises the fixture, not real Go or native hardware."""
    import runpy

    FakeTLSBroker = runpy.run_path(str(Path(__file__).with_name("go_fake_broker.py")))[
        "FakeTLSBroker"
    ]
    fixtures = prepare(tmp_path / "fixtures")
    bootstrap = tmp_path / "bootstrap"
    bootstrap.write_text("private-token")
    control = FixtureControl(fixtures, "private-token", "source-pod")
    source, engine = launch(), launch()
    tls = None
    try:
        ready = send(
            source,
            {
                "mode": "source",
                "bootstrapFile": str(bootstrap),
                "controlAddress": control.address,
                "workerID": "source-pod",
                "workerInstanceID": "instance-1",
                "postMode": "ambiguous",
            },
        )
        assert ready["event"] == "ready"
        wait_until(
            lambda: len([e for e in control.events if e[0] == "/v1/filled"]) == 2
        )
        tls = FakeTLSBroker(
            tmp_path, fixtures, ready["privateAddress"], "private-token"
        )
        assert (
            send(
                engine,
                {"mode": "engine", "env": engine_env(tmp_path, fixtures[0], tls)},
            )["event"]
            == "ready"
        )
        assert send(engine, {"op": "probe"})["ok"]
        assert send(engine, {"op": "start"})["ok"]
        assert send(engine, {"op": "fill"}) == {
            "ok": False,
            "kind": "SourceUnavailableError",
        }
        assert send(engine, {"op": "inspect"}) == {
            "ok": True,
            "hasSession": True,
            "unknown": True,
            "inflight": True,
            "windowClosed": False,
        }
        assert send(engine, {"op": "abort"}) == {"ok": True, "retainedUnknown": True}
        assert send(engine, {"op": "close"})["ok"] is False
        assert send(engine, {"op": "exit"}) == {"ok": False, "kind": "session-held"}
        wait_until(
            lambda: any(
                path == "/v1/quarantine" and body["generation"] == "generation-1"
                for path, body in control.events
            )
        )

        host, port = ready["privateAddress"].split(":")

        def runtime(method, path):
            connection = http.client.HTTPConnection(host, int(port), timeout=3)
            try:
                connection.request(
                    method, path, headers={"Authorization": "Bearer private-token"}
                )
                response = connection.getresponse()
                return response.status, json.loads(response.read())
            finally:
                connection.close()

        assert runtime("GET", "/v1/runtime")[0] == 409
        status, frozen = runtime("POST", "/v1/runtime/drain")
        assert status == 200
        assert frozen == {
            "workerID": "source-pod",
            "workerInstanceID": "instance-1",
            "draining": True,
        }
        status, snapshot = runtime("GET", "/v1/runtime")
        assert status == 200
        assert snapshot["workerInstanceID"] == "instance-1"
        assert "generation-1" in snapshot["generations"]
        assert "generation-1" in snapshot["retainedGenerations"]
        assert snapshot["nativeOperations"] == 1
        assert snapshot["unknownOperations"] == 1

        # Replay reaches the retained receipt without releasing its window.
        assert len(tls.operations) == 1
        _, request = next(iter(tls.operations.values()))

        def submit(payload):
            connection = http.client.HTTPConnection(host, int(port), timeout=3)
            try:
                connection.request(
                    "POST",
                    "/v1/operations",
                    json.dumps(payload),
                    {"Authorization": "private-token"},
                )
                response = connection.getresponse()
                return response.status, json.loads(response.read())
            finally:
                connection.close()

        status, replay = submit(request)
        assert status == 200 and replay["state"] == "unknown"
        assert runtime("GET", "/v1/runtime")[1]["nativeOperations"] == 1

        control.draining = True
        wait_until(
            lambda: any(
                path == "/v1/drain-ack" and body["generation"] == "generation-2"
                for path, body in control.answered_acks
            )
        )
        assert not any(
            path == "/v1/drain-ack" and body["generation"] == "generation-1"
            for path, body in control.events
        )
        assert send(source, {"op": "stop"}) == {
            "ok": False,
            "kind": "native-state-held",
        }
    finally:
        # These are fake CPU owners; killing them is cleanup, not release evidence.
        for process in (source, engine):
            if process.poll() is None:
                process.terminate()
                process.wait(timeout=5)
            process.stdin.close()
            process.stdout.close()
            process.stderr.close()
        if tls is not None:
            tls.close()
        control.close()
