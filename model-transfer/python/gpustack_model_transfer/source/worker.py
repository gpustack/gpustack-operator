"""One generation-bound source worker.

A worker serves the manifest and tensor ranges of exactly one verified
published tree, bound to one immutable generation. It registers with the
node's broker, heartbeats its lease, obeys a drain action by stopping
serving and acknowledging the disconnect, and quarantines itself when an
operation's outcome cannot be proven. It never writes NodeModelStore status:
the broker's ledger is its only report.
"""

import logging
import time

from gpustack_model_transfer.core.budgets import ReadBudgets
from gpustack_model_transfer.core.capabilities import local_backend_capabilities
from gpustack_model_transfer.core.errors import OperationStateError
from gpustack_model_transfer.core.local_reader import LocalFileReader
from gpustack_model_transfer.core.session import ReaderSession
from gpustack_model_transfer.source.control import ControlError

logger = logging.getLogger("gpustack_model_transfer.source")

# The worker's own read budgets: generous windows, one operation in flight.
WINDOW_BYTES = 1 << 30
OWNED_OUTPUT_BYTES = 1 << 30


class SourceWorker:
    """The source side of one generation, over a verified local tree."""

    def __init__(
        self,
        control,
        manifest_path,
        poll_interval: float = 0.1,
        registrations: int = 1,
        active_sessions: int = 0,
    ):
        self.control = control
        self.manifest_path = manifest_path
        self.poll_interval = poll_interval
        self.registrations = registrations
        self.active_sessions = active_sessions
        self.state = "starting"
        self._session = None
        self._lease_id = None

    def start(self) -> None:
        """Register, verify the published tree, and report the fill.

        The manifest is digest-verified before anything is served, exactly
        as the reader contract requires; a worker that cannot verify its
        tree never reports itself filled.
        """
        self._lease_id = self.control.register()
        reader = LocalFileReader(self.manifest_path)
        self._session = ReaderSession(
            identity=reader.identity(),
            capabilities=local_backend_capabilities(),
            budgets=ReadBudgets(
                window_bytes=WINDOW_BYTES,
                max_inflight=1,
                owned_output_bytes=OWNED_OUTPUT_BYTES,
            ),
            reader=reader,
        )
        try:
            self.control.filled(registrations=self.registrations)
        except BaseException:
            self._session.close()
            self._session = None
            raise
        self._set_state("serving")

    def serve(self, tensor: str, offset: int, length: int) -> bytes:
        """Serve one tensor range from the verified tree."""
        self._require_serving()
        output = self._session.read_range(tensor, offset, length)
        return output.payload

    def once(self) -> str:
        """Run one heartbeat cycle and return what happened.

        The return value is the broker's action once it applies: "none",
        "drain", or "stale" when the generation is gone. A stale generation
        stops the worker; it never quarantines, because nothing of its own
        failed. A draining worker keeps heartbeating — each heartbeat
        renews the lease the final acknowledgement still needs, and the
        broker answers "drain" again until the acknowledgement lands —
        and retries the acknowledgement on every cycle.
        """
        if self.state not in ("serving", "draining"):
            return self.state
        resident = 0
        if self._session is not None:
            resident = sum(t.byte_size for t in self._session.descriptors)
        try:
            answer = self.control.heartbeat(
                resident_bytes=resident,
                registrations=self.registrations,
                active_sessions=self.active_sessions,
            )
        except ControlError as exc:
            if exc.kind == "stale-generation":
                self._set_state("stopped")
                return "stale"
            raise
        action = answer.get("action", "none")
        if action == "drain":
            self._drain()
            return "drain"
        return "none"

    def run(self, stop_event) -> str:
        """Heartbeat until drained, stopped, or the event is set.

        A drain order whose acknowledgement is refused (sessions still
        active) keeps the loop alive: the worker keeps heartbeating and
        retries the acknowledgement on later cycles instead of exiting
        mid-drain.
        """
        result = self.state
        while not stop_event.is_set():
            result = self.once()
            if result == "stale" or self.state in (
                "drained",
                "stopped",
                "quarantined",
            ):
                return result
            time.sleep(self.poll_interval)
        return result

    def report_unknown(self, reason: str) -> None:
        """Quarantine the generation: an operation's outcome is unknown.

        The registration and buffers stay held until a qualified fence
        applies; the worker stops serving immediately.
        """
        self.control.quarantine(reason)
        self._set_state("quarantined")

    def redacted_status(self) -> dict:
        """A status view carrying no secret and no path."""
        return {"generation": self.control.generation, "state": self.state}

    def close(self) -> None:
        if self._session is not None:
            self._session.close()
            self._session = None
        self.control.close()

    def _drain(self) -> None:
        self._set_state("draining")
        try:
            # The reference worker holds no native memory, so its
            # acknowledgement truthfully claims no native release.
            self.control.drain_ack(native_released=False)
        except ControlError as exc:
            if exc.kind == "operation-state":
                # Sessions are still draining on the broker side; the next
                # drain acknowledgement is tried on a later cycle.
                return
            raise
        self._set_state("drained")

    def _require_serving(self) -> None:
        if self.state != "serving" or self._session is None:
            raise OperationStateError(
                f"the worker is {self.state} and serves no ranges"
            )

    def _set_state(self, state: str) -> None:
        if state != self.state:
            logger.debug(
                "generation %s: %s -> %s", self.control.generation, self.state, state
            )
        self.state = state


__all__ = ["SourceWorker"]
