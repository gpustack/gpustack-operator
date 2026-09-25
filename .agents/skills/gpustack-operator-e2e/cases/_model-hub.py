"""A test model hub for the node-delivery cases, and a forward proxy, on the Python standard library.

It serves what the operator reads of the Hugging Face Hub: the revision, tree and whoami endpoints and
file downloads with byte ranges. Repositories are declared in REPOS (JSON), their content generated
from their names and SEED, so a case knows every digest without shipping files, and a new SEED gives
every repository new bytes: a node cache left by an earlier run then holds none of them. It can
throttle, corrupt and refuse, and it records every revision, tree and file request it answers, which a
case reads back.

Usage: python3 _model-hub.py hub   (REPOS, SEED, PORT, TLS_CERT, TLS_KEY from the environment)
       python3 _model-hub.py proxy (PORT)
Not a case; the cases deploy it through _model-hub-lib.sh.
"""
import hashlib
import json
import os
import re
import select
import socket
import ssl
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, unquote, urlparse

BLOCK = 1 << 16
SEED = os.environ.get("SEED", "")


def block_of(seed):
    out = b""
    counter = 0
    while len(out) < BLOCK:
        out += hashlib.sha256(f"{seed}/{counter}".encode()).digest()
        counter += 1
    return out[:BLOCK]


class Repo:
    def __init__(self, name, spec):
        self.name = name
        self.commit = hashlib.sha1(f"{SEED}/{name}".encode()).hexdigest()
        self.token = spec.get("token", "")
        self.files = {}
        for path, f in sorted(spec["files"].items()):
            size, lfs = int(f["size"]), bool(f.get("lfs", False))
            blk = block_of(f"{SEED}/{name}/{path}")
            sha256, sha1 = hashlib.sha256(), hashlib.sha1()
            sha1.update(f"blob {size}\0".encode())
            left = size
            while left > 0:
                chunk = blk[: min(left, BLOCK)]
                sha256.update(chunk)
                sha1.update(chunk)
                left -= len(chunk)
            self.files[path] = {"size": size, "lfs": lfs, "block": blk,
                                "sha256": sha256.hexdigest(), "gitsha1": sha1.hexdigest()}

    def byte_range(self, path, start, end):
        f = self.files[path]
        pos = start
        while pos <= end:
            off = pos % BLOCK
            n = min(BLOCK - off, end - pos + 1)
            yield f["block"][off:off + n]
            pos += n


STATE = {"throttle": 0, "corrupt": set(), "deny": set()}
LOG = []
LOCK = threading.Lock()
REPOS = {}


def record(entry):
    with LOCK:
        LOG.append(entry)


class Hub(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def reply(self, code, body=b"", ctype="application/json", headers=None):
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        for k, v in (headers or {}).items():
            self.send_header(k, v)
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)

    def auth_hash(self):
        auth = self.headers.get("Authorization", "")
        return hashlib.sha256(auth.encode()).hexdigest()[:16] if auth else ""

    def authorized(self, repo):
        if repo.token == "":
            return True
        return self.headers.get("Authorization", "") == "Bearer " + repo.token

    def do_POST(self):
        u = urlparse(self.path)
        q = parse_qs(u.query)
        if u.path == "/_control":
            with LOCK:
                if "throttle" in q:
                    STATE["throttle"] = int(q["throttle"][0])
                for key in ("corrupt", "deny"):
                    if key in q:
                        STATE[key] = set(x for x in q[key][0].split(",") if x)
            return self.reply(200, b"{}")
        if u.path == "/_reset":
            with LOCK:
                LOG.clear()
            return self.reply(200, b"{}")
        return self.reply(404)

    def do_HEAD(self):
        self.do_GET()

    def do_GET(self):
        u = urlparse(self.path)
        path = unquote(u.path)
        if path == "/_log":
            with LOCK:
                body = "\n".join(json.dumps(e) for e in LOG).encode()
            return self.reply(200, body, "application/x-ndjson")
        if path == "/_manifest":
            out = {n: {"commit": r.commit, "files": {p: {"size": f["size"], "sha256": f["sha256"]}
                                                     for p, f in r.files.items()}} for n, r in REPOS.items()}
            return self.reply(200, json.dumps(out).encode())
        if path == "/api/whoami-v2":
            return self.reply(200 if self.headers.get("Authorization") else 401, b"{}")

        m = re.match(r"^/api/models/(.+?)/(revision|tree)/([^/]+)$", path)
        if m:
            repo = REPOS.get(m.group(1))
            entry = {"path": path, "range": "", "auth": self.auth_hash(), "remote": self.client_address[0],
                     "method": self.command, "time": time.time()}
            if repo is None or not self.authorized(repo) or repo.name in STATE["deny"]:
                entry["status"] = 401
                record(entry)
                return self.reply(401, b"{}")
            if m.group(2) == "revision":
                entry["status"] = 200
                record(entry)
                return self.reply(200, json.dumps({"sha": repo.commit}).encode())
            if m.group(3) != repo.commit:
                entry["status"] = 404
                record(entry)
                return self.reply(404, b"{}", headers={"X-Error-Code": "RevisionNotFound"})
            entry["status"] = 200
            record(entry)
            tree = []
            for p, f in repo.files.items():
                e = {"type": "file", "path": p, "size": f["size"], "oid": f["gitsha1"]}
                if f["lfs"]:
                    e["lfs"] = {"oid": f["sha256"], "size": f["size"]}
                tree.append(e)
            return self.reply(200, json.dumps(tree).encode())

        m = re.match(r"^/(.+?)/resolve/([0-9a-f]{40})/(.+)$", path)
        if not m:
            return self.reply(404, b"{}")
        repo = REPOS.get(m.group(1))
        entry = {"path": path, "range": self.headers.get("Range", ""), "auth": self.auth_hash(),
                 "remote": self.client_address[0], "method": self.command, "time": time.time()}
        if repo is None or m.group(2) != repo.commit or m.group(3) not in repo.files:
            entry["status"] = 404
            record(entry)
            return self.reply(404, b"{}", headers={"X-Error-Code": "EntryNotFound"})
        if not self.authorized(repo) or repo.name in STATE["deny"]:
            entry["status"] = 401
            record(entry)
            return self.reply(401, b"{}")
        f = repo.files[m.group(3)]
        size = f["size"]
        start, end, code = 0, size - 1, 200
        rg = re.match(r"bytes=(\d+)-(\d*)", self.headers.get("Range", ""))
        if rg:
            start = int(rg.group(1))
            end = min(int(rg.group(2)), size - 1) if rg.group(2) else size - 1
            if start >= size or end < start:
                # Unsatisfiable, as the Hub answers it, so a client that asks past the end is told so.
                entry["status"] = 416
                record(entry)
                return self.reply(416, b"{}", headers={"Content-Range": f"bytes */{size}"})
            code = 206
        entry["status"] = code
        record(entry)
        self.send_response(code)
        self.send_header("Content-Length", str(max(end - start + 1, 0)))
        self.send_header("Accept-Ranges", "bytes")
        if code == 206:
            self.send_header("Content-Range", f"bytes {start}-{end}/{size}")
        self.end_headers()
        if self.command == "HEAD":
            return
        corrupt = repo.name in STATE["corrupt"]
        sent = start
        try:
            for chunk in repo.byte_range(m.group(3), start, end):
                for i in range(0, len(chunk), 16384):
                    piece = chunk[i:i + 16384]
                    if corrupt and sent <= size // 2 < sent + len(piece):
                        b = bytearray(piece)
                        b[size // 2 - sent] ^= 0xFF
                        piece = bytes(b)
                    self.wfile.write(piece)
                    sent += len(piece)
                    rate = STATE["throttle"]
                    if rate > 0:
                        time.sleep(len(piece) / rate)
        except (BrokenPipeError, ConnectionResetError):
            pass


class Proxy(BaseHTTPRequestHandler):
    """A forward proxy that tunnels CONNECT and records each tunnel's target."""

    def log_message(self, *args):
        pass

    def do_GET(self):
        if self.path == "/_log":
            with LOCK:
                body = "\n".join(json.dumps(e) for e in LOG).encode()
            self.send_response(200)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        self.send_response(405)
        self.end_headers()

    def do_CONNECT(self):
        host, _, port = self.path.rpartition(":")
        record({"connect": self.path, "remote": self.client_address[0], "time": time.time()})
        try:
            upstream = socket.create_connection((host, int(port)), timeout=30)
        except OSError:
            self.send_response(502)
            self.end_headers()
            return
        self.send_response(200, "Connection established")
        self.end_headers()
        conns = [self.connection, upstream]
        try:
            while True:
                readable, _, _ = select.select(conns, [], [], 300)
                if not readable:
                    break
                for c in readable:
                    data = c.recv(65536)
                    if not data:
                        return
                    (upstream if c is self.connection else self.connection).sendall(data)
        finally:
            upstream.close()


def main():
    role = sys.argv[1]
    port = int(os.environ.get("PORT", "8080"))
    if role == "proxy":
        ThreadingHTTPServer(("", port), Proxy).serve_forever()
        return
    for name, spec in json.loads(os.environ["REPOS"]).items():
        REPOS[name] = Repo(name, spec)
    server = ThreadingHTTPServer(("", port), Hub)
    if os.environ.get("TLS_CERT"):
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        ctx.load_cert_chain(os.environ["TLS_CERT"], os.environ["TLS_KEY"])
        server.socket = ctx.wrap_socket(server.socket, server_side=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
