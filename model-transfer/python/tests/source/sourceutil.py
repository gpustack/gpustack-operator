"""Shared helpers for the source worker tests: fixture location and a fake
broker implementing the control wire the Go server serves."""

import hashlib
import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

FIXTURE_DIR = (
    Path(__file__).resolve().parents[3] / "contracts" / "fixtures" / "tiny_dense"
)


def fixture_dir() -> Path:
    if not FIXTURE_DIR.is_dir():
        raise AssertionError(f"missing fixture directory: {FIXTURE_DIR}")
    return FIXTURE_DIR


def manifest_digest() -> str:
    return hashlib.sha256((fixture_dir() / "manifest.json").read_bytes()).hexdigest()


class FakeBroker:
    """A scriptable stand-in for the node's source broker control service.

    Rules say which reply each endpoint gives; every request is recorded so
    a test can assert exactly what the worker sent. Secrets are accepted and
    never logged.
    """

    def __init__(self, rules=None):
        self.requests = []
        self.rules = rules or {}
        broker = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass  # keep the test output clean

            def do_POST(self):
                length = int(self.headers.get("Content-Length", "0"))
                body = json.loads(self.rfile.read(length) or b"{}")
                broker.requests.append({"path": self.path, "body": body})
                status, answer = broker.reply(self.path, body)
                payload = json.dumps(answer).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.lease_id = "lease-1"

    @property
    def url(self) -> str:
        host, port = self.server.server_address[:2]
        return f"{host}:{port}"

    def reply(self, path, body):
        rule = self.rules.get(path, {})
        if callable(rule):
            return rule(body)
        if "status" in rule:
            return rule["status"], rule.get("body", {})
        return 200, rule.get(
            "body",
            {
                "leaseID": self.lease_id,
                "expiresAt": "2031-01-01T00:00:00Z",
                "action": "none",
            },
        )

    def posts(self, path):
        return [r["body"] for r in self.requests if r["path"] == path]

    def close(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)


def tensor_payload(name: str) -> bytes:
    """The golden fixture's bytes for one tensor, as published on disk."""
    manifest = json.loads((fixture_dir() / "manifest.json").read_bytes())
    tensors = {t["name"]: t for t in manifest["tensors"]}
    blob = (fixture_dir() / tensors[name]["file"]).read_bytes()
    return blob[tensors[name]["data_begin"] : tensors[name]["data_end"]]
