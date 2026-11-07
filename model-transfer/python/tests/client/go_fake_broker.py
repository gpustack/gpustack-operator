"""CPU-only fixture router, not the Go integration acceptance broker."""

import http.client
import json
import ssl
import subprocess
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlsplit


class FakeTLSBroker:
    def __init__(self, root, fixtures, private_address, token):
        self.fixtures = {item["artifactUID"]: item for item in fixtures}
        self.private_address = private_address
        self.token = token
        self.sessions = {}
        self.operations = {}
        self.calls = []
        broker = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def send(self, payload, status=200):
                raw = json.dumps(payload).encode()
                self.send_response(status)
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def body(self):
                return json.loads(
                    self.rfile.read(int(self.headers.get("Content-Length", "0")))
                    or b"{}"
                )

            def private(self, path, payload):
                host, port = broker.private_address.split(":")
                conn = http.client.HTTPConnection(host, int(port), timeout=5)
                try:
                    conn.request(
                        "POST",
                        path,
                        json.dumps(payload),
                        {"Authorization": broker.token},
                    )
                    response = conn.getresponse()
                    value = json.loads(response.read())
                    if response.status != 200:
                        raise ValueError("private refusal")
                    return value
                finally:
                    conn.close()

            def authorized(self, session_id, uid, instance):
                session = broker.sessions.get(session_id)
                if (
                    session
                    and session["podUID"] == uid
                    and session["targetInstanceID"] == instance
                    and self.headers.get("Authorization")
                    == "Bearer token-" + session["suffix"]
                ):
                    return session
                return None

            def common(self, session, request_id):
                return {
                    "sourcePodUID": "source-pod",
                    "workerInstanceID": "instance-1",
                    "generation": session["generation"],
                    "leaseID": "lease-" + session["generation"],
                    "sessionID": session["sessionID"],
                    "requestID": request_id,
                }

            def do_POST(self):
                try:
                    body = self.body()
                    broker.calls.append((self.path, body))
                    if self.path == "/v1/sessions":
                        item = broker.fixtures[body["artifactUID"]]
                        suffix = item["namespace"][-1]
                        if (
                            body["podNamespace"] != item["namespace"]
                            or self.headers.get("Authorization")
                            != "Bearer token-" + suffix
                        ):
                            self.send({"kind": "authentication"}, 403)
                            return
                        session_id = "session-" + body["requestID"]
                        generation = "generation-1" if suffix == "a" else "generation-2"
                        broker.sessions[session_id] = {
                            **body,
                            "suffix": suffix,
                            "sessionID": session_id,
                            "generation": generation,
                            "fixture": item,
                        }
                        self.send(
                            {
                                "sessionID": session_id,
                                "requestID": body["requestID"],
                                "digest": item["digest"],
                                "artifactUID": item["artifactUID"],
                                "sourceGeneration": item["sourceGeneration"],
                                "manifestSHA256": item["manifestSHA256"],
                                "manifestJSON": item["manifestJSON"],
                                "generation": generation,
                                "sourcePodUID": "source-pod",
                                "workerInstanceID": "instance-1",
                            }
                        )
                    elif self.path.endswith("/closing"):
                        session_id = self.path.split("/")[-2]
                        session = self.authorized(
                            session_id, body["podUID"], body["targetInstanceID"]
                        )
                        if not session:
                            self.send({"kind": "authentication"}, 403)
                            return
                        result = self.private(
                            "/v1/sessions/close",
                            self.common(session, body["requestID"]),
                        )
                        self.send(
                            {"state": "closing"}
                            if result["state"] == "closed"
                            else result
                        )
                    elif self.path == "/v1/transfers":
                        session = self.authorized(
                            body["sessionID"], body["podUID"], body["targetInstanceID"]
                        )
                        if not session:
                            self.send({"kind": "authentication"}, 403)
                            return
                        manifest = json.loads(session["fixture"]["manifestJSON"])
                        tensor = next(
                            item
                            for item in manifest["tensors"]
                            if item["name"] == body["tensor"]
                        )
                        private = self.common(session, body["requestID"])
                        private.update(
                            {
                                "targetPodUID": body["podUID"],
                                "targetInstanceID": body["targetInstanceID"],
                                "registrationID": body["registrationID"],
                                "windowID": body["windowID"],
                                "tensor": body["tensor"],
                                "file": tensor["file"],
                                "fileOffset": tensor["data_begin"]
                                + body["tensorOffset"],
                                "tensorOffset": body["tensorOffset"],
                                "length": body["length"],
                                "target": body["target"],
                            }
                        )
                        broker.operations[body["requestID"]] = (session, private)
                        result = self.private("/v1/operations", private)
                        self.send({**result, "sessionID": body["sessionID"]})
                    elif self.path.endswith("/ack"):
                        request_id = self.path.split("/")[-2]
                        session, original = broker.operations[request_id]
                        if not self.authorized(
                            body["sessionID"], body["podUID"], body["targetInstanceID"]
                        ):
                            self.send({"kind": "authentication"}, 403)
                            return
                        keys = (
                            "sourcePodUID",
                            "workerInstanceID",
                            "generation",
                            "leaseID",
                            "sessionID",
                            "requestID",
                            "targetPodUID",
                            "targetInstanceID",
                            "registrationID",
                            "windowID",
                        )
                        result = self.private(
                            "/v1/operations/ack", {key: original[key] for key in keys}
                        )
                        self.send({**result, "sessionID": session["sessionID"]})
                    else:
                        self.send({"kind": "unknown"}, 404)
                except Exception:
                    self.send({"kind": "fixture-error"}, 409)

            def do_GET(self):
                try:
                    parsed = urlsplit(self.path)
                    request_id = parsed.path.split("/")[-1]
                    query = parse_qs(parsed.query)
                    session, _original = broker.operations[request_id]
                    if not self.authorized(
                        query["sessionID"][0],
                        query["podUID"][0],
                        query["targetInstanceID"][0],
                    ):
                        self.send({"kind": "authentication"}, 403)
                        return
                    result = self.private(
                        "/v1/operations/status", self.common(session, request_id)
                    )
                    self.send({**result, "sessionID": session["sessionID"]})
                except Exception:
                    self.send({"kind": "fixture-error"}, 409)

            def do_DELETE(self):
                try:
                    body = self.body()
                    broker.calls.append((self.path, body))
                    session_id = self.path.split("/")[-1]
                    if (
                        not self.authorized(
                            session_id, body["podUID"], body["targetInstanceID"]
                        )
                        or body.get("targetReleased") is not True
                    ):
                        self.send({"kind": "authentication"}, 403)
                        return
                    self.send({"state": "closed"})
                except Exception:
                    self.send({"kind": "fixture-error"}, 409)

        self.cert = root / "cert.pem"
        key = root / "key.pem"
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
                str(self.cert),
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
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(self.cert, key)
        self.server.socket = context.wrap_socket(self.server.socket, server_side=True)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def close(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=3)
