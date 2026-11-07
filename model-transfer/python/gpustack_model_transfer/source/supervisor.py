"""The source supervisor: many generations, one process, one wire.

The supervisor polls the Pod-loopback assignment endpoint, validates
every work order BEFORE any native owner exists, and runs one native
worker per admitted generation — concurrent generations of different
artifacts are admitted separately, with per-generation accounting and
no digest deduplication unless backing memory is actually shared. Work
orders are idempotent by revision: the same (generation, manifest
digest, digest) tuple again is a no-op. Repeated malformed orders never
leak a native owner, because validation refuses them before the
backend, allocator, or worker exist.

Cancellation honors retained unknowns: a stop event stops polling and
drains what can be drained, but a quarantined generation holding native
memory keeps it held — the process does not exit-and-forget.
"""

import hashlib
import json
import logging
import threading

from gpustack_model_transfer.core.errors import error_code
from gpustack_model_transfer.source.native_worker import NativeSourceWorker
from gpustack_model_transfer.source.workorder import parse_assignment

logger = logging.getLogger("gpustack_model_transfer.source")


class SourceSupervisor:
    """Runs the source holder: assignments in, native generations out."""

    def __init__(
        self,
        bootstrap,
        *,
        control_factory,
        backend_factory,
        allocator_factory,
        worker_id: str,
        backend: str,
        poll_interval: float = 0.2,
        max_generations: int = 8,
    ):
        self.bootstrap = bootstrap
        self._control_factory = control_factory
        self._backend_factory = backend_factory
        self._allocator_factory = allocator_factory
        self.worker_id = worker_id
        self.worker_instance_id = ""
        self.backend = backend
        self.poll_interval = poll_interval
        self.max_generations = max_generations
        self.workers: dict = {}
        self.threads: dict = {}
        self._revisions: set = set()
        self._order_hashes = {}
        self._lock = threading.Lock()
        self._shutdown = threading.Event()
        self._admission_closed = threading.Event()
        self.refused_orders = 0
        self._starting = 0
        from gpustack_model_transfer.source.transfer_service import OperationRegistry

        self.operations = OperationRegistry(self)

    def poll_once(self) -> int:
        """One assignment poll; start what is new, skip what is known.

        Returns the number of newly started generations. A malformed
        order is counted and logged redacted, with no native owner
        created; a control-wire failure propagates to the caller's backoff.
        """
        assignments = self.bootstrap.assignments()
        offered = {
            item.get("generation")
            for item in assignments
            if isinstance(item, dict) and isinstance(item.get("generation"), str)
        }
        with self._lock:
            for generation, worker in list(self.workers.items()):
                thread = self.threads[generation]
                if (
                    generation not in offered
                    and worker.state in ("drained", "stopped")
                    and not thread.is_alive()
                ):
                    if not self.operations.retire(worker):
                        continue
                    worker.close()
                    del self.workers[generation]
                    del self.threads[generation]
                    self._order_hashes.pop(generation, None)
                    self._revisions.discard(worker.assignment.revision())
        started = 0
        for payload in assignments:
            try:
                fingerprint = hashlib.sha256(
                    json.dumps(payload, sort_keys=True).encode()
                ).digest()
                with self._lock:
                    if (
                        isinstance(payload, dict)
                        and payload.get("generation") in self.workers
                        and self._order_hashes.get(payload["generation"]) == fingerprint
                    ):
                        continue
                fetch = getattr(self.bootstrap, "manifest_page", None)
                assignment = parse_assignment(
                    payload,
                    backend=self.backend,
                    page_reader=(
                        lambda page, fetch=fetch, payload=payload: fetch(payload, page)
                    )
                    if fetch is not None
                    else None,
                )
            except Exception as exc:
                self.refused_orders += 1
                logger.warning(
                    "a work order was refused (%s); no native owner was created",
                    error_code(exc),
                )
                continue
            revision = assignment.revision()
            with self._lock:
                if self._admission_closed.is_set():
                    continue
                if revision in self._revisions:
                    continue
                if assignment.generation in self.workers:
                    logger.warning(
                        "generation %s: conflicting revision refused",
                        assignment.generation,
                    )
                    continue
                if len(self.workers) >= self.max_generations:
                    logger.warning(
                        "a work order was refused: this holder already "
                        "runs its generation bound"
                    )
                    continue
                self._revisions.add(revision)
                self._starting += 1
            try:
                self._start_worker(assignment)
                with self._lock:
                    self._order_hashes[assignment.generation] = fingerprint
            finally:
                with self._lock:
                    self._starting -= 1
            started += 1
        return started

    def _start_worker(self, assignment) -> NativeSourceWorker:
        control = self._control_factory(assignment)
        worker = NativeSourceWorker(
            control,
            assignment,
            backend=lambda: self._backend_factory(assignment),
            allocator=lambda: self._allocator_factory(assignment),
            worker_instance_id=control.worker_instance_id,
            poll_interval=self.poll_interval,
        )
        worker.operations = self.operations
        with self._lock:
            self.workers[assignment.generation] = worker

        def run_generation():
            try:
                worker.start()
                worker.run(self._shutdown)
            except BaseException:
                worker.report_unknown("worker-failure")

        thread = threading.Thread(
            target=run_generation,
            name=f"source-gen-{assignment.generation}",
            daemon=True,
        )
        self.threads[assignment.generation] = thread
        thread.start()
        return worker

    def statuses(self) -> list:
        with self._lock:
            workers = list(self.workers.values())
        return [worker.redacted_status() for worker in workers]

    def stop(self, timeout: float = 10.0) -> bool:
        """Drain every worker and stop polling; quarantined holders keep
        their memory. Return true only when every worker is drained or stopped."""
        self._shutdown.set()
        for thread in list(self.threads.values()):
            thread.join(timeout=timeout)
        return all(
            worker.state in ("drained", "stopped") for worker in self.workers.values()
        )


__all__ = ["SourceSupervisor"]
