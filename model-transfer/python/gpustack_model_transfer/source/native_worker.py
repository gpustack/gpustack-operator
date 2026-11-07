"""Own one generation and retain uncertain native state until process fencing."""

import logging
import threading
import time
from contextlib import suppress

from gpustack_model_transfer.core.budgets import align_down_2m
from gpustack_model_transfer.envs import get_envs
from gpustack_model_transfer.source.backend import retain_source_state
from gpustack_model_transfer.source.control import ControlError
from gpustack_model_transfer.source.fill import (
    FillAborted,
    fill_from_tree,
    resident_files,
)
from gpustack_model_transfer.source.memory import (
    MIN_SOURCE_BUFFER_BYTES,
    available_ram,
    compute_dynamic_source_buffer,
    total_ram,
)

logger = logging.getLogger("gpustack_model_transfer.source")

# Mirrors the backend's bounded stage vocabulary: the only values that may
# upgrade a quarantine reason to a stage-coded string.
_NATIVE_STAGES = ("connect", "submit", "poll", "disconnect")


def _stage_reason(reason, stage):
    """The quarantine wire reason for a report.

    The exact stage-coded string replaces the legacy reason only when a
    bounded HIXL stage was reported; every other reason — and any forged or
    out-of-vocabulary stage — passes through unchanged. The exact built-in
    str check keeps a hostile str subclass from raising inside the
    vocabulary comparison.
    """
    if type(stage) is str and stage in _NATIVE_STAGES:
        return "hixl-" + stage
    return reason


class NativeSourceWorker:
    """Run all native calls on the generation thread and heartbeat independently."""

    def __init__(
        self,
        control,
        assignment,
        *,
        backend,
        allocator,
        worker_instance_id,
        poll_interval=0.2,
        chunk_bytes=1 << 20,
    ):
        self.control = control
        self.assignment = assignment
        self._backend_factory = backend if callable(backend) else lambda: backend
        self._allocator_factory = (
            allocator if callable(allocator) else lambda: allocator
        )
        self.backend = None
        self.allocator = None
        self.worker_instance_id = worker_instance_id
        self.poll_interval = poll_interval
        self.chunk_bytes = chunk_bytes
        self.state = "starting"
        self.registration = None
        self.blocks = {}
        self.fill_report = None
        self.active_sessions = None
        self.unknown_sessions = None
        self.native_operations = 0
        self.operations = None
        self._drain_requested = threading.Event()
        self._abort_fill_requested = threading.Event()
        self._stop_event = threading.Event()
        self._wake_event = threading.Event()
        self._unknown_event = threading.Event()
        self._filled_event = threading.Event()
        self._control_thread = None
        self._lock = threading.Lock()
        self._release_done = False
        self._native_attempted = False
        self._retention = None
        self._blocks_idle = False
        self._had_sessions = False
        self._idle_since = None
        self.idle_grace_seconds = 0.0 if poll_interval < 0.05 else 60.0

    def _retry(self, call):
        delay = 0.05
        while not self._stop_event.is_set() and self.state != "quarantined":
            try:
                return call()
            except ControlError as exc:  # noqa: PERF203
                if not exc.transport:
                    raise
                self._stop_event.wait(delay)
                delay = min(delay * 2, 2.0)
        raise ControlError(0, "unavailable", "stopped")

    def _admit_dynamic_window(self):
        """Refuse to build the RDMA transfer when the node's available RAM
        cannot back even the configured floor: the client then falls back
        to its local or peer-to-peer path. A zero ceiling derives one
        sixteenth of total RAM floored at the minimum buffer; an unreadable
        memory figure, or a caller that set no ceiling variable, gates
        nothing."""
        envs = get_envs()
        raw_max = envs.GPUSTACK_MODEL_TRANSFER_SOURCE_MAX_MEMORY_BYTES_RAW
        if raw_max is None or raw_max == "":
            return True
        avail = available_ram()
        if avail is None:
            return True
        min_bytes = max(
            align_down_2m(envs.GPUSTACK_MODEL_TRANSFER_SOURCE_MIN_MEMORY_BYTES or 0),
            MIN_SOURCE_BUFFER_BYTES,
        )
        max_bytes = align_down_2m(
            envs.GPUSTACK_MODEL_TRANSFER_SOURCE_MAX_MEMORY_BYTES or 0
        )
        if max_bytes <= 0:
            total = total_ram()
            max_bytes = max(align_down_2m((total // 16) if total else 0), min_bytes)
        _window, ok = compute_dynamic_source_buffer(avail, max_bytes, min_bytes)
        if not ok:
            logger.debug(
                "generation %s: available RAM cannot back the %s byte "
                "source floor; refusing the RDMA source so clients "
                "fall back",
                self.control.generation,
                min_bytes,
            )
            return False
        return True

    def start(self):
        try:
            self._retry(self.control.register)
            self._control_thread = threading.Thread(
                target=self._control_loop, daemon=True
            )
            self._control_thread.start()
            while not self._admit_dynamic_window():
                if self._stop_event.is_set():
                    self.state = "stopped"
                    return
                if self._abort_fill_requested.wait(max(self.poll_interval, 1.0)):
                    return
            self._retry(self.control.native_start)
            with self._lock:
                if self._abort_fill_requested.is_set():
                    return
                self._native_attempted = True
            self.allocator = self._allocator_factory()
            self.backend = self._backend_factory()
            if hasattr(self.backend, "allocator"):
                self.backend.allocator = self.allocator
            self.backend.start()
            configure = getattr(self.backend, "configure_paging", None)
            if configure is not None:
                configure(
                    self.assignment,
                    self.allocator,
                    self.blocks,
                    self._lock,
                    self._restore_aborted,
                )
            with self._lock:
                if self._abort_fill_requested.is_set():
                    return
                self.state = "filling"
            for desc in resident_files(
                self.assignment.manifest,
                self.assignment.resident_budget_bytes,
                backend=self.assignment.backend,
            ):
                if self._abort_fill_requested.is_set():
                    return
                block = self.allocator.allocate(desc.path, desc.size)
                with self._lock:
                    self.blocks[desc.path] = block
            self.fill_report = fill_from_tree(
                self.assignment.manifest,
                self.assignment.tree_path,
                self.blocks,
                self.chunk_bytes,
                self._abort_fill_requested.is_set,
                resident_only=self.assignment.paged,
            )
            if self._abort_fill_requested.is_set():
                return
            self.registration = self.backend.register(self.blocks)
            descriptors = self.registration.descriptors(self.control.worker_id)
            if len(descriptors) != len(self.blocks):
                raise ValueError("registration count mismatch")
            if self._abort_fill_requested.is_set():
                return
            self._retry(lambda: self.control.filled(len(descriptors), descriptors))
            self._filled_event.set()
            if self.state != "quarantined":
                self.state = "serving"
        except BaseException as exc:
            if (
                (isinstance(exc, FillAborted) or not self._native_attempted)
                and self._drain_requested.is_set()
                and self.state != "quarantined"
                and self.registration is None
            ):
                self.state = "draining"
                return
            if self._native_attempted:
                self._quarantine(type(exc).__name__)
            else:
                self.state = "stopped"
            logger.warning(
                "generation %s: start failed (%s)",
                self.control.generation,
                type(exc).__name__,
            )

    def _control_loop(self):
        delay = self.poll_interval
        while not self._stop_event.is_set():
            try:
                with self._lock:
                    resident = sum(
                        getattr(b, "allocation_bytes", b.byte_size)
                        for b in self.blocks.values()
                    )
                    registrations = len(self.blocks) if self.registration else 0
                    operations = self.native_operations
                answer = self.control.heartbeat(
                    resident_bytes=resident,
                    registrations=registrations,
                    native_operations=operations,
                )
                active, unknown, action = (
                    answer.get("activeSessions"),
                    answer.get("unknownSessions"),
                    answer.get("action"),
                )
                if any(
                    isinstance(x, bool) or not isinstance(x, int) or x < 0
                    for x in (active, unknown)
                ) or action not in ("none", "drain"):
                    raise ControlError(0, "invalid-response", "invalid heartbeat")
                with self._lock:
                    if active > 0:
                        self._had_sessions = True
                    self.active_sessions, self.unknown_sessions = active, unknown
                    if action == "drain":
                        self._drain_requested.set()
                        self._abort_fill_requested.set()
                        self._wake_event.set()
                delay = self.poll_interval
            except ControlError as exc:
                if exc.kind == "stale-generation":
                    self._unknown_event.set()
                with self._lock:
                    self.active_sessions = self.unknown_sessions = None
                    if exc.kind == "stale-generation":
                        self._abort_fill_requested.set()
                        retain = self._native_attempted and not self._release_done
                        if not retain:
                            self.state = "stopped"
                if exc.kind == "stale-generation":
                    if retain:
                        self._quarantine("stale-generation")
                    return
                delay = min(max(delay * 2, 0.05), 2.0)
            self._stop_event.wait(delay)

    def drain(self):
        if self.state in ("quarantined", "drained", "stopped"):
            return
        self.state = "draining"
        with self._lock:
            if (
                not self._drain_requested.is_set()
                or self.active_sessions != 0
                or self.unknown_sessions != 0
                or self.native_operations != 0
            ):
                return
            # The broker drain action must prohibit new sessions before it reports zero.
            self.native_operations = 1
        try:
            if not self._release_done:
                if self.registration is not None:
                    self._check_not_unknown()
                    self.backend.disconnect(self.registration)
                    self._check_not_unknown()
                    self.backend.revoke(self.registration)
                if self.backend is not None and hasattr(self.backend, "finalize"):
                    self._check_not_unknown()
                    self.backend.finalize()
                for name in list(self.blocks):
                    self._check_not_unknown()
                    self.allocator.release(self.blocks[name])
                    with self._lock:
                        del self.blocks[name]
                if self.allocator is not None and hasattr(self.allocator, "dispose"):
                    self._check_not_unknown()
                    self.allocator.dispose()
                self._check_not_unknown()
                self._release_done = True
        except BaseException as exc:
            self._quarantine(type(exc).__name__)
            return
        finally:
            self.native_operations = 0
        try:
            self._retry(lambda: self.control.drain_ack(native_released=True))
        except ControlError as exc:
            if exc.kind == "stale-generation" and self._release_done:
                self.state = "stopped"
            return
        self.state = "drained"
        self._stop_event.set()
        self._wake_event.set()

    def release_idle_blocks(self):
        """Release the generation's registered host RAM while nothing reads
        it, and keep the generation able to rebuild on the next work.

        This is the lower-water mark behind a finished transfer: the
        sessions drained to zero, no native operation is in flight, and the
        broker still holds the generation serving. The registration is
        revoked and every block freed, so the resident figure falls to
        about zero; a later session re-allocates, refills, and re-registers
        on demand. The release is the generation thread's job — a caller
        may request it explicitly, and the serving loop runs it when the
        heartbeat shows the idle transition.
        """
        if self.state != "serving":
            return False
        with self._lock:
            if (
                self._drain_requested.is_set()
                or self._unknown_event.is_set()
                or self._blocks_idle
                or self.active_sessions != 0
                or self.unknown_sessions != 0
                or self.native_operations != 0
                or self.registration is None
            ):
                return False
            # Reserve the native-operation slot like a drain does, so no
            # new operation is submitted against the vanishing
            # registration.
            self.native_operations += 1
            self._blocks_idle = True
        try:
            self._check_not_unknown()
            self.backend.disconnect(self.registration)
            self._check_not_unknown()
            self.backend.revoke(self.registration)
            for name in list(self.blocks):
                self._check_not_unknown()
                block = self.blocks[name]
                self.allocator.release(block)
                with self._lock:
                    self.blocks.pop(name, None)
            with self._lock:
                self.registration = None
        except BaseException as exc:
            self._quarantine(type(exc).__name__)
            return False
        finally:
            with self._lock:
                self.native_operations -= 1
        return True

    def _rebuild_blocks(self):
        """Re-allocate, refill, and re-register the generation's blocks
        after an idle release, on this generation thread, so pending
        operations are served again. All native state stays on this thread
        and a failure quarantines exactly like a start failure."""
        with self._lock:
            if (
                not self._blocks_idle
                or self.state != "serving"
                or self._restore_aborted()
            ):
                return
        try:
            allocator = self.allocator
            backend = self.backend
            for desc in resident_files(
                self.assignment.manifest,
                self.assignment.resident_budget_bytes,
                backend=self.assignment.backend,
            ):
                if self._restore_aborted():
                    raise FillAborted("source restore interrupted")
                block = allocator.allocate(desc.path, desc.size)
                # Publish each acquired owner before the next fallible call.
                with self._lock:
                    self.blocks[desc.path] = block
            fill_from_tree(
                self.assignment.manifest,
                self.assignment.tree_path,
                self.blocks,
                self.chunk_bytes,
                self._restore_aborted,
                resident_only=True,
            )
            if self._restore_aborted():
                raise FillAborted("source restore interrupted")
            registration = backend.register(self.blocks)
            with self._lock:
                self.registration = registration
            descriptors = registration.descriptors(self.control.worker_id)
            if len(descriptors) != len(self.blocks):
                raise ValueError("registration count mismatch")
            with self._lock:
                if self._restore_aborted():
                    raise FillAborted("source restore interrupted")
                self._blocks_idle = False
        except BaseException as exc:
            self._quarantine(type(exc).__name__)

    def _restore_aborted(self):
        return self._unknown_event.is_set() or self._drain_requested.is_set()

    def _maintain_blocks(self):
        """Run on the generation thread between operations: rebuild after an
        idle release when work or sessions returned, and release when the
        last session drained away."""
        with self._lock:
            idle = self._blocks_idle
            stopped = self._drain_requested.is_set() or self._unknown_event.is_set()
            busy = stopped or self.active_sessions != 0 or self.native_operations != 0
        if (
            idle
            and not stopped
            and self.operations is not None
            and self.operations.has_pending(self)
        ):
            self._idle_since = None
            self._rebuild_blocks()
            return
        if (
            idle
            and not stopped
            and self.active_sessions is not None
            and self.active_sessions > 0
        ):
            self._idle_since = None
            self._rebuild_blocks()
            return
        if busy:
            self._idle_since = None
            return
        if (
            not idle
            and self.state == "serving"
            and self._had_sessions
            and self.registration is not None
        ):
            now = time.monotonic()
            if self._idle_since is None:
                self._idle_since = now
            if now - self._idle_since >= self.idle_grace_seconds:
                self._idle_since = None
                self.release_idle_blocks()

    def _check_not_unknown(self):
        if self._unknown_event.is_set():
            raise RuntimeError("native state became unknown")

    def _quarantine(self, reason, stage=None):
        self._unknown_event.set()
        if self.operations is not None:
            self.operations.mark_unknown(self)
        if self._retention is None:
            handles = getattr(self.registration, "handles", {})
            self._retention = retain_source_state(
                handles, self.blocks, reason, owner=self, backend=self.backend
            )
        self.state = "quarantined"
        with suppress(Exception):
            self.control.quarantine(_stage_reason(reason, stage))

    def report_unknown(self, reason, stage=None):
        # The reason stays the legacy literal unless a bounded HIXL stage was
        # reported; the stage-coded string is the only upgrade, so non-HIXL
        # and legacy callers keep their current semantics.
        self._quarantine("unknown", stage)

    def once(self):
        wake = getattr(self, "_wake_event", None)
        if wake is not None:
            wake.clear()
        progress = False
        if self.operations is not None and self.state not in (
            "drained",
            "stopped",
            "quarantined",
        ):
            self._maintain_blocks()
            if self.state in ("drained", "stopped", "quarantined"):
                return self.state
            for _ in range(64):
                if not self.operations.step(self):
                    break
                progress = True
        if self.state in ("drained", "stopped", "quarantined"):
            return self.state
        if self._drain_requested.is_set():
            self.drain()
        delay = self.poll_interval
        if progress:
            delay = 0
        elif self.operations is not None and self.operations.has_pending(self):
            delay = min(delay, 0.002)
        if wake is not None:
            wake.wait(delay)
        else:
            self._stop_event.wait(delay)
        return self.state

    def run(self):
        """Serve until the broker authorizes drain or ownership is quarantined."""
        while self.state not in ("drained", "stopped", "quarantined"):
            self.once()
        return self.state

    def close(self):
        if self._native_attempted and not self._release_done:
            self._quarantine("close-with-native-state")
            return
        self._stop_event.set()
        self._wake_event.set()
        if self._control_thread is not None:
            self._control_thread.join(timeout=5)
        self.control.close()

    def wake(self):
        """Wake the serving loop so a new operation is stepped immediately."""
        self._wake_event.set()

    def redacted_status(self):
        return {
            "generation": self.control.generation,
            "state": self.state,
            "backend": self.assignment.backend,
            "registrations": len(self.blocks),
            "activeSessions": self.active_sessions,
            "unknownSessions": self.unknown_sessions,
            "nativeOperations": self.native_operations,
        }

    def wait_until_filled(self, timeout=None):
        return self._filled_event.wait(timeout)
