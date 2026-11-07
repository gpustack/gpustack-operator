"""Private broker-to-source operation journal and loopback HTTP service."""

import base64
import binascii
import hashlib
import hmac
import ipaddress
import json
import threading
from collections import deque
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from gpustack_model_transfer.core.budgets import MAX_WINDOW_BYTES
from gpustack_model_transfer.core.errors import ModelTransferError
from gpustack_model_transfer.source.backend import (
    _NATIVE_STAGES,
    SourcePrePostError,
)


def _extract_native_stage(exc):
    """Pull the bounded stage off a failed native call, defensively.

    Extraction and the vocabulary comparison must never raise — a hostile
    exception object cannot prevent the operation from being marked unknown
    — so any surprise yields None. An exact built-in str check is required:
    a str subclass may override __eq__ to raise inside the tuple comparison.
    """
    stage = None
    try:
        stage = getattr(exc, "gpustack_native_stage", None)
    except BaseException:
        stage = None
    if type(stage) is not str or stage not in _NATIVE_STAGES:
        return None
    return stage


MAX_BODY = 1 << 20
# The source accepts every window the client contract allows, so a reader
# configured above the old 8 GiB default is not refused here: the ceiling
# is the same 1 TiB contract ceiling the client's own budget enforces.
MAX_WINDOW = MAX_WINDOW_BYTES
MAX_JOBS = 1 << 20
MAX_JOB_BYTES = 32 << 20
MAX_SESSIONS = 4096
_COMMON = {
    "sourcePodUID",
    "workerInstanceID",
    "generation",
    "leaseID",
    "sessionID",
    "requestID",
}
_OWNER = {"targetPodUID", "targetInstanceID", "registrationID", "windowID"}
_SUBMIT = (
    _COMMON
    | _OWNER
    | {"tensor", "file", "fileOffset", "tensorOffset", "length", "target"}
)


def _unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON key")
        result[key] = value
    return result


class Refused(Exception):
    """A request was rejected before a native post."""


class ObservationUnavailable(Refused):
    """A status request may have overtaken the original submission."""


def _text(value):
    if not isinstance(value, str) or not value or len(value) > 512:
        raise Refused("invalid identifier")
    return value


def _number(value, *, positive=False):
    if isinstance(value, bool) or not isinstance(value, int) or value < int(positive):
        raise Refused("invalid numeric field")
    return value


def _fields(body, expected):
    if not isinstance(body, dict) or set(body) != expected:
        raise Refused("invalid request fields")
    for key in _COMMON:
        _text(body[key])


def _common(body):
    return tuple(
        body[key]
        for key in (
            "sourcePodUID",
            "workerInstanceID",
            "generation",
            "leaseID",
            "sessionID",
        )
    )


def _owner(body):
    return tuple(
        _text(body[key])
        for key in ("targetPodUID", "targetInstanceID", "registrationID", "windowID")
    )


class OperationRegistry:
    """The incarnation-local journal; native calls run on each generation thread."""

    def __init__(self, supervisor):
        self.supervisor = supervisor
        self.lock = threading.Lock()
        self.jobs = {}
        self.job_bytes = 0
        self.sessions = {}
        self.windows = {}
        self.queues = {}
        self._session_closes = {}
        self._page_waiters = {}
        self._manifest_tables = {}

    def retire(self, worker):
        """Reclaim receipts only after the broker retired this native generation."""
        if worker.state not in ("drained", "stopped") or (
            worker._native_attempted and not worker._release_done
        ):
            return False
        generation = worker.control.generation
        with self.lock:
            jobs = [
                key for key, job in self.jobs.items() if job["common"][2] == generation
            ]
            sessions = [key for key in self.sessions if key[2] == generation]
            if any(self.jobs[key]["state"] != "acknowledged" for key in jobs) or any(
                self.sessions[key]["state"] != "closed" for key in sessions
            ):
                return False
            for key in jobs:
                del self.jobs[key]
            for key in sessions:
                del self.sessions[key]
            self.queues.pop(worker, None)
            self._session_closes.pop(worker, None)
            self._page_waiters.pop(worker, None)
            self._manifest_tables.pop(id(worker.assignment.manifest), None)
            return True

    def _tables(self, manifest):
        key = id(manifest)
        cached = self._manifest_tables.get(key)
        if cached is not None and cached[0] is manifest:
            return cached[1], cached[2], cached[3], cached[4]
        from gpustack_model_transfer.core.tensor_index import tensor_tables

        by_name, by_file, index_by_file = tensor_tables(manifest)
        file_sizes = {f.path: f.size for f in manifest.files}
        self._manifest_tables[key] = (
            manifest,
            by_name,
            by_file,
            index_by_file,
            file_sizes,
        )
        return by_name, by_file, index_by_file, file_sizes

    @staticmethod
    def _valid_source_range(tensor, offset, length, by_file, index_by_file):
        if offset <= tensor.byte_size and length <= tensor.byte_size - offset:
            return True
        if offset != 0 or length <= tensor.byte_size:
            return False
        items = by_file.get(tensor.file, ())
        idx = index_by_file.get((tensor.file, tensor.name), -1)
        if idx < 0:
            return False
        target_end = tensor.data_begin + length
        cur_end = tensor.data_end
        for nxt in items[idx + 1 :]:
            if cur_end >= target_end:
                break
            if nxt.data_begin != cur_end:
                return False
            cur_end = nxt.data_end
        return cur_end == target_end

    def _worker(self, body):
        worker = self.supervisor.workers.get(body["generation"])
        if worker is None:
            raise Refused("unknown source incarnation")
        if (
            body["sourcePodUID"] != worker.control.worker_id
            or body["workerInstanceID"] != worker.worker_instance_id
            or body["leaseID"] != worker.control.lease_id
        ):
            raise Refused("source incarnation mismatch")
        return worker

    def freeze(self):
        """Permanently stop new generation and operation admission for this boot."""
        with self.supervisor._lock, self.lock:
            self.supervisor._admission_closed.set()
        return {
            "workerID": self.supervisor.worker_id,
            "workerInstanceID": self.supervisor.worker_instance_id,
            "draining": True,
        }

    def runtime(self):
        """Snapshot process ownership without inventing an empty result."""
        from gpustack_model_transfer.backends.hixl.runtime import (
            retained_registrations as hixl_retained,
        )
        from gpustack_model_transfer.backends.nixl.runtime import (
            retained_registrations as nixl_retained,
        )
        from gpustack_model_transfer.source import memory
        from gpustack_model_transfer.source.backend import retained_source_states

        with self.supervisor._lock:
            if (
                self.supervisor._starting
                or not self.supervisor.worker_instance_id
                or not self.supervisor._admission_closed.is_set()
            ):
                raise Refused("runtime snapshot is unavailable")
            workers = tuple(self.supervisor.workers.values())
            with self.lock:
                unknown = sum(job["state"] == "unknown" for job in self.jobs.values())
        generations = sorted(
            worker.control.generation
            for worker in workers
            if worker.state not in ("drained", "stopped")
        )
        retained = {
            worker.control.generation
            for worker in workers
            if worker.state == "quarantined"
        }
        source_retained = retained_source_states()
        for item in source_retained:
            matched = False
            for worker in workers:
                if (
                    item.owner is worker
                    or (item.owner is not None and item.owner is worker.allocator)
                    or (item.backend is not None and item.backend is worker.backend)
                ):
                    retained.add(worker.control.generation)
                    matched = True
            if not matched:
                unknown += 1
        # Backend retainments may not expose a reliable generation; count them as unknown.
        unknown += len(nixl_retained()) + len(hixl_retained())
        if memory._ACL_BROKEN is not None or (
            memory._ACL_RUNTIME is not None and not generations
        ):
            unknown += 1
        return {
            "workerID": self.supervisor.worker_id,
            "workerInstanceID": self.supervisor.worker_instance_id,
            "generations": generations,
            "nativeOperations": sum(worker.native_operations for worker in workers),
            "unknownOperations": unknown,
            "retainedGenerations": sorted(retained),
        }

    def submit(self, body):
        _fields(body, _SUBMIT)
        common = _common(body)
        owner = _owner(body)
        tensor_name = _text(body["tensor"])
        file_name = _text(body["file"])
        offset = _number(body["tensorOffset"])
        file_offset = _number(body["fileOffset"])
        length = _number(body["length"], positive=True)
        if length > MAX_WINDOW:
            raise Refused("window exceeds limit")
        target = body["target"]
        if not isinstance(target, dict) or set(target) != {
            "backend",
            "address",
            "length",
            "metadata",
            "endpoint",
        }:
            raise Refused("invalid target")
        if (
            target["backend"] not in ("nixl", "hixl")
            or _number(target["address"], positive=True) > (1 << 64) - 1
        ):
            raise Refused("invalid target address")
        if (
            _number(target["length"], positive=True) > MAX_WINDOW
            or length > target["length"]
            or target["address"] + target["length"] > 1 << 64
        ):
            raise Refused("invalid target extent")
        if (
            not isinstance(target["metadata"], str)
            or len(target["metadata"]) > MAX_BODY // 2
            or not isinstance(target["endpoint"], str)
            or len(target["endpoint"]) > 512
        ):
            raise Refused("invalid target rendezvous")
        if (target["backend"] == "nixl" and not target["metadata"]) or (
            target["backend"] == "hixl" and not target["endpoint"]
        ):
            raise Refused("target rendezvous missing")
        if target["backend"] == "nixl":
            try:
                base64.b64decode(target["metadata"], validate=True)
            except (ValueError, binascii.Error):
                raise Refused("invalid target metadata") from None
        else:
            try:
                host, port = target["endpoint"].rsplit(":", 1)
                ipaddress.ip_address(host.strip("[]"))
                if not 1 <= int(port) <= 65535:
                    raise ValueError
            except ValueError:
                raise Refused("invalid target endpoint") from None
        encoded = json.dumps(body, sort_keys=True, separators=(",", ":")).encode()
        fingerprint = hashlib.sha256(encoded).digest()
        worker = self._worker(body)
        if target["backend"] != worker.assignment.backend:
            raise Refused("backend mismatch")
        try:
            with self.lock:
                if worker.state in ("drained", "stopped"):
                    raise Refused("source is not accepting operations")
                by_name, by_file, index_by_file, file_sizes = self._tables(
                    worker.assignment.manifest
                )
            tensor = by_name[tensor_name]
            with worker._lock:
                block = worker.blocks.get(file_name)
                restoring = worker._blocks_idle
            if block is not None:
                block_size = block.byte_size
            elif (restoring or worker.assignment.paged) and file_name in file_sizes:
                block_size = file_sizes[file_name]
            else:
                raise KeyError(file_name)
        except (KeyError, ValueError, ModelTransferError):
            raise Refused("unknown tensor") from None
        if (
            tensor.file != file_name
            or not self._valid_source_range(
                tensor, offset, length, by_file, index_by_file
            )
            or file_offset != tensor.data_begin + offset
            or file_offset > block_size
            or length > block_size - file_offset
        ):
            raise Refused("source range mismatch")
        key = body["requestID"]
        with self.lock:
            existing = self.jobs.get(key)
            if existing is not None:
                if existing["fingerprint"] != fingerprint:
                    raise Refused("conflicting request id")
                return self._view(existing)
            if self.supervisor._admission_closed.is_set():
                raise Refused("source admission is frozen")
            session = self.sessions.get(common)
            if session is not None and (
                session["state"] != "open" or session["owner"] != owner[:2]
            ):
                raise Refused("session is closing or owner differs")
            if (
                len(self.jobs) >= MAX_JOBS
                or self.job_bytes + len(encoded) > MAX_JOB_BYTES
                or (session is None and len(self.sessions) >= MAX_SESSIONS)
            ):
                raise Refused("journal full")
            if owner in self.windows:
                raise Refused("window already owned")
            with worker._lock:
                if (
                    worker.state != "serving"
                    or worker._drain_requested.is_set()
                    or worker._unknown_event.is_set()
                ):
                    raise Refused("source is not accepting operations")
                worker.native_operations += 1
            self.sessions.setdefault(
                common, {"state": "open", "owner": owner[:2], "worker": worker}
            )
            job = {
                "fingerprint": fingerprint,
                "request_id": key,
                "body": body,
                "byte_size": len(encoded),
                "worker": worker,
                "common": common,
                "owner": owner,
                "state": "dispatching",
                "verdict": "",
                "native": None,
                "ack": False,
                "terminal_event": threading.Event(),
                "ack_event": threading.Event(),
            }
            self.jobs[key] = job
            self.job_bytes += len(encoded)
            self.queues.setdefault(worker, deque()).append(key)
            self.windows[owner] = key
            if hasattr(worker, "wake"):
                worker.wake()
            return self._view(job)

    def status(self, body):
        _fields(body, _COMMON)
        with self.lock:
            job = self.jobs.get(body["requestID"])
            if job is None:
                self._worker(body)
                # A timed-out submit may still arrive on another HTTP thread.
                # Absence cannot establish whether that request will post.
                raise ObservationUnavailable("unknown operation")
            if job["common"] != _common(body):
                raise Refused("unknown operation")
            return self._view(job)

    def acknowledge(self, body):
        _fields(body, _COMMON | _OWNER)
        with self.lock:
            job = self.jobs.get(body["requestID"])
            if (
                job is None
                or job["common"] != _common(body)
                or job["owner"] != _owner(body)
            ):
                raise Refused("unknown operation owner")
            if job["state"] in ("acknowledged", "releasing"):
                return self._view(job)
            if job["state"] != "terminal":
                raise Refused("operation not terminal")
            job["ack"] = True
            worker = job["worker"]
            if hasattr(worker, "wake"):
                worker.wake()
            return self._view(job)

    def close_session(self, body):
        _fields(body, _COMMON)
        worker = self._worker(body)
        common = _common(body)
        with self.lock:
            if common not in self.sessions and len(self.sessions) >= MAX_SESSIONS:
                raise Refused("journal full")
            session = self.sessions.setdefault(common, {"state": "open", "owner": None})
            if session["state"] == "closed":
                return {"state": "closed"}
            if session["state"] == "unknown":
                return {"state": "closing"}
            if session["state"] == "open":
                session["state"] = "closing"
            if any(
                j["common"] == common and j["state"] != "acknowledged"
                for j in self.jobs.values()
            ):
                return {"state": "closing"}
            close_peers = getattr(worker.backend, "close_session_connections", None)
            if not callable(close_peers) or session["owner"] is None:
                session["state"] = "closed"
                session["worker"] = None
                return {"state": "closed"}
            if "close_event" not in session:
                with worker._lock:
                    if worker._unknown_event.is_set():
                        session["state"] = "unknown"
                        return {"state": "closing"}
                    worker.native_operations += 1
                session["close_event"] = threading.Event()
                session["worker"] = worker
                self._session_closes.setdefault(worker, deque()).append(common)
                worker.wake()
            event = session["close_event"]
        # Native teardown runs on the generation thread, never this HTTP thread.
        # Stay below the private broker client's five-second request timeout.
        event.wait(4.0)
        with self.lock:
            return {"state": "closed" if session["state"] == "closed" else "closing"}

    def _step_session_close(self, worker):
        with self.lock:
            queue = self._session_closes.get(worker)
            if not queue:
                return False
            common = queue.popleft()
            if not queue:
                self._session_closes.pop(worker, None)
            session = self.sessions[common]
            if session["state"] != "closing":
                return True
            session["state"] = "releasing"
        try:
            worker.backend.close_session_connections(common)
            with self.lock:
                if session["state"] != "unknown":
                    session["state"] = "closed"
                    session["worker"] = None
                    with worker._lock:
                        worker.native_operations -= 1
                session["close_event"].set()
        except BaseException as exc:
            with self.lock:
                session["state"] = "unknown"
                session["close_event"].set()
            worker.report_unknown(
                "native-session-close-unknown", stage=_extract_native_stage(exc)
            )
        return True

    @staticmethod
    def _view(job):
        return {
            "requestID": job["request_id"],
            "state": {"posting": "dispatching", "releasing": "terminal"}.get(
                job["state"], job["state"]
            ),
            "verdict": job["verdict"],
        }

    def mark_unknown(self, worker):
        """A stale or failed owner cannot publish a known operation result."""
        with self.lock:
            for session in self.sessions.values():
                if session.get("worker") is worker and session["state"] != "closed":
                    session["state"] = "unknown"
                    event = session.get("close_event")
                    if event is not None:
                        event.set()
            for job in self.jobs.values():
                if job["worker"] is worker and job["state"] != "acknowledged":
                    job["state"] = "unknown"
                    for ev_name in ("terminal_event", "ack_event"):
                        ev = job.get(ev_name)
                        if ev is not None:
                            ev.set()

    def wait_for_event(self, request_id, event_name, timeout_s):
        with self.lock:
            job = self.jobs.get(request_id)
            if job is None:
                return None
            ev = job.get(event_name)
        if ev is not None and timeout_s > 0:
            ev.wait(timeout_s)
        with self.lock:
            job = self.jobs.get(request_id)
            return self._view(job) if job is not None else None

    def step(self, worker):
        """Return whether a native step advanced on its owning thread."""
        if self._step_session_close(worker):
            return True
        worker_lock = getattr(worker, "_lock", None)
        restoring = False
        if worker_lock is not None:
            with worker_lock:
                restoring = getattr(worker, "_blocks_idle", False)
                if restoring and not worker._drain_requested.is_set():
                    return False
        with self.lock:
            queue = self.queues.get(worker)
            if not queue:
                return False
            waiter = self._page_waiters.get(worker)
            if waiter is not None and self.jobs[waiter]["state"] != "dispatching":
                self._page_waiters.pop(worker)
                waiter = None
            todo = None
            for _ in range(len(queue)):
                key = queue.popleft()
                job = self.jobs[key]
                if job["state"] == "acknowledged":
                    continue
                queue.append(key)
                if job["state"] == "dispatching" and not restoring:
                    # Drain existing operations before serving the page miss.
                    # New resident requests must not keep that drain open forever.
                    if waiter is not None and key != waiter:
                        continue
                    needs_swap = getattr(worker.backend, "needs_window_swap", None)
                    has_peers = getattr(worker.backend, "has_active_peers", None)
                    if (
                        callable(needs_swap)
                        and callable(has_peers)
                        and has_peers()
                        and needs_swap(worker.registration, job["body"]["file"])
                    ):
                        self._page_waiters[worker] = key
                        waiter = key
                        continue
                if job["state"] in ("dispatching", "submitted") or (
                    job["state"] == "terminal" and job["ack"]
                ):
                    todo = job
                    break
            if todo is None:
                return False
            state = todo["state"]
            body = todo["body"]
            if state == "dispatching":
                self._page_waiters.pop(worker, None)
                todo["state"] = "posting"
            elif state == "terminal":
                todo["state"] = "releasing"
        try:
            if state == "dispatching":
                if restoring:
                    raise SourcePrePostError("source drained before restoring")
                arguments = (
                    worker.registration,
                    body["file"],
                    body["fileOffset"],
                    body["length"],
                    body["target"],
                )
                session_write = getattr(worker.backend, "post_session_write", None)
                if callable(session_write):
                    native = session_write(*arguments, todo["common"], todo["owner"])
                else:
                    native = worker.backend.post_write(*arguments)
                with self.lock:
                    todo["native"] = native
                    if todo["state"] != "unknown":
                        todo["state"] = "submitted"
            elif state == "submitted":
                verdict = worker.backend.poll_write(todo["native"])
                if verdict in ("succeeded", "failed"):
                    with self.lock:
                        if todo["state"] != "unknown":
                            todo["verdict"] = verdict
                            todo["state"] = "terminal"
                            ev = todo.get("terminal_event")
                            if ev is not None:
                                ev.set()
                elif verdict != "pending":
                    raise RuntimeError("unrecognized native verdict")
                else:
                    return False
            else:
                if todo["native"] is not None:
                    worker.backend.close_write(todo["native"])
                with self.lock:
                    if todo["state"] == "unknown":
                        return False
                    self.windows.pop(todo["owner"], None)
                    self.job_bytes -= todo["byte_size"]
                    todo["body"] = None
                    todo["native"] = None
                    todo["worker"] = None
                    todo["state"] = "acknowledged"
                    ev = todo.get("ack_event")
                    if ev is not None:
                        ev.set()
                    queue = self.queues.get(worker)
                    if queue is not None:
                        queue.remove(todo["request_id"])
                        if not queue:
                            self.queues.pop(worker)
                            self._page_waiters.pop(worker, None)
                    with worker._lock:
                        worker.native_operations -= 1
        except SourcePrePostError as exc:
            stage = _extract_native_stage(exc)
            if state != "dispatching":
                with self.lock:
                    todo["state"] = "unknown"
                    for ev_name in ("terminal_event", "ack_event"):
                        ev = todo.get(ev_name)
                        if ev is not None:
                            ev.set()
                worker.report_unknown("native-operation-unknown", stage=stage)
                return False
            with self.lock:
                if todo["state"] != "unknown":
                    todo["verdict"] = "not-posted"
                    todo["state"] = "terminal"
                    ev = todo.get("terminal_event")
                    if ev is not None:
                        ev.set()
        except BaseException as exc:
            stage = _extract_native_stage(exc)
            with self.lock:
                todo["state"] = "unknown"
                for ev_name in ("terminal_event", "ack_event"):
                    ev = todo.get(ev_name)
                    if ev is not None:
                        ev.set()
            worker.report_unknown("native-operation-unknown", stage=stage)
        return True

    def has_pending(self, worker):
        """Whether native completion still needs a short polling interval."""
        with self.lock:
            return bool(self._session_closes.get(worker)) or any(
                self.jobs[key]["state"] in ("dispatching", "submitted")
                or (self.jobs[key]["state"] == "terminal" and self.jobs[key]["ack"])
                for key in self.queues.get(worker, ())
            )


class PrivateServer(ThreadingHTTPServer):
    daemon_threads = True
    # Queue one bounded burst matching the existing active handler limit.
    request_queue_size = 64

    def __init__(self, registry, bootstrap_file, address=("127.0.0.1", 9252)):
        self.registry = registry
        self.bootstrap_file = bootstrap_file
        self.slots = threading.BoundedSemaphore(64)
        super().__init__(address, PrivateHandler)

    def handle_error(self, request, client_address):
        # Request data may carry private descriptors; never print traceback.
        return None

    def process_request(self, request, client_address):
        if not self.slots.acquire(blocking=False):
            self.shutdown_request(request)
            return
        try:
            super().process_request(request, client_address)
        except BaseException:
            self.slots.release()
            raise

    def process_request_thread(self, request, client_address):
        try:
            super().process_request_thread(request, client_address)
        finally:
            self.slots.release()


class PrivateHandler(BaseHTTPRequestHandler):
    def setup(self):
        super().setup()
        self.connection.settimeout(5.0)

    def log_message(self, *args):
        pass

    def do_GET(self):
        try:
            if self.path != "/v1/runtime":
                raise Refused("unknown path")
            with open(self.server.bootstrap_file, encoding="utf-8") as handle:
                token = handle.read(513).strip()
            supplied = self.headers.get("Authorization", "")
            if (
                not token
                or len(token) > 512
                or not hmac.compare_digest("Bearer " + token, supplied)
            ):
                self.send_error(403)
                return
            raw = json.dumps(
                self.server.registry.runtime(), separators=(",", ":")
            ).encode()
            self.send_response(200)
        except (Refused, ValueError, TypeError, OSError):
            raw = b'{"kind":"refused"}'
            self.send_response(409)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def _freeze_runtime(self):
        try:
            with open(self.server.bootstrap_file, encoding="utf-8") as handle:
                token = handle.read(513).strip()
            supplied = self.headers.get("Authorization", "")
            if (
                not token
                or len(token) > 512
                or not hmac.compare_digest("Bearer " + token, supplied)
            ):
                self.send_error(403)
                return
            length = int(self.headers.get("Content-Length", "0"))
            if length != 0:
                raise Refused("unexpected body")
            raw = json.dumps(
                self.server.registry.freeze(), separators=(",", ":")
            ).encode()
            self.send_response(200)
        except (Refused, ValueError, TypeError, OSError):
            raw = b'{"kind":"refused"}'
            self.send_response(409)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_POST(self):
        if self.path == "/v1/runtime/drain":
            self._freeze_runtime()
            return
        try:
            with open(self.server.bootstrap_file, encoding="utf-8") as handle:
                token = handle.read(513).strip()
            supplied = self.headers.get("Authorization", "")
            if (
                not token
                or len(token) > 512
                or not hmac.compare_digest(token, supplied)
            ):
                self.send_error(403)
                return
            length = int(self.headers.get("Content-Length", "0"))
            if length <= 0 or length > MAX_BODY:
                raise Refused("invalid body length")
            body = json.loads(self.rfile.read(length), object_pairs_hook=_unique_object)
            handlers = {
                "/v1/operations": self.server.registry.submit,
                "/v1/operations/status": self.server.registry.status,
                "/v1/operations/ack": self.server.registry.acknowledge,
                "/v1/sessions/close": self.server.registry.close_session,
            }
            if self.path not in handlers:
                raise Refused("unknown path")
            response = handlers[self.path](body)
            wait_header = self.headers.get("X-GPUStack-Wait-Ms", "")
            if wait_header and self.path in ("/v1/operations", "/v1/operations/ack"):
                try:
                    wait_ms = min(max(int(wait_header), 0), 500)
                except ValueError:
                    wait_ms = 0
                if wait_ms > 0 and isinstance(body, dict) and "requestID" in body:
                    ev_name = (
                        "terminal_event"
                        if self.path == "/v1/operations"
                        else "ack_event"
                    )
                    updated = self.server.registry.wait_for_event(
                        body["requestID"], ev_name, wait_ms / 1000.0
                    )
                    if updated is not None:
                        response = updated
            raw = json.dumps(response, separators=(",", ":")).encode()
            self.send_response(200)
        except ObservationUnavailable:
            raw = b'{"kind":"observation-unavailable"}'
            self.send_response(503)
        except (Refused, ValueError, TypeError, OSError):
            raw = b'{"kind":"refused"}'
            self.send_response(409)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)
