"""The HIXL reader: registered windows in the consuming process.

Target memory is registered where it is consumed — in this process — per
the documented CANN ordering, and the session wraps the agent so register
precedes connect and disconnect precedes deregister.

Every operation is an InFlightOperation that owns its terminal state, its
window slot, and its active-byte accounting. The stale-replay cleanup
model is the point: a submitted operation that will not reach a terminal
state within the poll deadline becomes UNKNOWN and RETAINS its window and
its bytes until a qualified fence resolves it through resolve(), which is
also where the bytes are deactivated. A prior successful operation never
makes a later timeout eligible for normal cleanup — the accounting is per
operation, and nothing about one operation's state touches another's.
"""

import time
import uuid

from gpustack_model_transfer.backends.hixl.memory import AlignedHostBuffer, MemoryLedger
from gpustack_model_transfer.backends.hixl.runtime import (
    HixlAPI,
    RegistrationRetainedError,
    retain_registration,
)
from gpustack_model_transfer.backends.hixl.session import (
    HixlCapabilities,
    HixlSession,
    negotiate_transport,
)
from gpustack_model_transfer.core.budgets import (
    MEMORY_ALIGNMENT_BYTES,
    WindowSlots,
    align_down_2m,
    hixl_allocation_bytes,
)
from gpustack_model_transfer.core.errors import (
    AmbiguousReadError,
    OperationStateError,
    RangeOverflowError,
    SourceUnavailableError,
)
from gpustack_model_transfer.core.operation import Operation, OpState

DEFAULT_POLL_DEADLINE_SECONDS = 30.0
_POLL_INTERVAL_SECONDS = 0.001

# The direction this route submits by default: the measured
# source-initiated WRITE, a candidate the safety gate must qualify.
DIRECTION_WRITE = "write"
DIRECTION_READ = "read"


class TransferWindow(AlignedHostBuffer):
    """One fixed, reusable, registered window buffer."""

    def __init__(self, name: str, size: int):
        super().__init__(size)
        self.name = name
        self.size = size

    def __len__(self) -> int:
        return self.size


class InFlightOperation:
    """One submitted operation with its own terminal-state lifecycle.

    deactivate_bytes is called when the operation's window bytes return
    to the reservation: on a clean pre-submission refusal, on failure, on
    success, and — the case the stale-replay model got wrong before —
    inside resolve(), when a qualified fence turns a retained UNKNOWN into
    a terminal state. The bytes of an unresolved UNKNOWN stay active:
    that is the quarantine, not a leak.
    """

    def __init__(
        self, operation: Operation, window: TransferWindow, ledger: MemoryLedger
    ):
        self.operation = operation
        self.window = window
        self.ledger = ledger
        ledger.activate(min(operation.length, window.size_bytes))
        self._resolved = False

    @property
    def retained(self) -> bool:
        return self.operation.retained or self.operation.state == OpState.UNKNOWN

    def mark_failed(self, code: str, message: str) -> None:
        self.operation.mark_failed(code, message)
        self.deactivate_bytes()

    def mark_succeeded(self) -> None:
        self.operation.mark_succeeded()
        self.deactivate_bytes()

    def deactivate_bytes(self) -> None:
        """Return this operation's active bytes exactly once."""
        if self._resolved:
            return
        self._resolved = True
        self.ledger.deactivate(min(self.operation.length, self.window.size_bytes))

    def resolve(
        self,
        target: str,
        error_code: str | None = None,
        error_message: str | None = None,
    ) -> None:
        """Resolve a retained UNKNOWN through a qualified fence.

        The fence's resolution is the only path that may release a
        retained window, and releasing it is also returning its bytes.
        """
        self.operation.resolve_unknown(
            OpState(target), error_code=error_code, error_message=error_message
        )
        self.deactivate_bytes()


class HixlReader:
    """A Reader-protocol implementation over one HIXL session."""

    def __init__(
        self,
        manifest,
        api: HixlAPI,
        peer: dict,
        budgets,
        *,
        direction: str = DIRECTION_WRITE,
        poll_deadline_seconds: float = DEFAULT_POLL_DEADLINE_SECONDS,
        ledger: MemoryLedger | None = None,
        transport_options: tuple = (),
        now=None,
    ):
        if direction not in (DIRECTION_WRITE, DIRECTION_READ):
            raise ValueError(f"unknown transfer direction {direction!r}")
        self.manifest = manifest
        self._api = api
        self._peer = dict(peer)
        self._budgets = budgets
        self._direction = direction
        self._poll_deadline = poll_deadline_seconds
        self.ledger = ledger if ledger is not None else MemoryLedger()
        self._now = now or time.monotonic

        # Reserve first: the reservation is pure accounting, so a budget
        # that cannot hold the windows fails before any backend resource
        # exists, and nothing can leak.
        ceiling = max(2 * MEMORY_ALIGNMENT_BYTES, align_down_2m(budgets.window_bytes))
        self._window_bytes = min(budgets.window_bytes, ceiling - MEMORY_ALIGNMENT_BYTES)
        self._reserved_bytes = (
            hixl_allocation_bytes(self._window_bytes) * budgets.max_inflight
        )
        self.ledger.reserve(self._reserved_bytes)

        # Register in the consuming process, then connect: the documented
        # order, enforced by the session wrapper. A failure anywhere
        # after the reservation unwinds what the earlier calls opened,
        # so a half-built reader never strands a registration, a
        # connection, or the reservation itself.
        self._session_agent = None
        self.session = None
        self.windows = ()
        self._registration = None
        self._connection = None
        try:
            self._session_agent = api.create_agent(
                "model-transfer-hixl-reader",
                {"transport_options": list(transport_options)},
            )
            self.negotiation = self._negotiate(transport_options)
            self.capabilities = HixlCapabilities(negotiation=self.negotiation)
            self.session = HixlSession(self._session_agent)
            self.windows = tuple(
                TransferWindow(f"window-{index}", self._window_bytes)
                for index in range(budgets.max_inflight)
            )
            self._registration = api.register_memory(
                self._session_agent,
                {window.name: window for window in self.windows},
            )
            self.session.register(self._registration)
            self._connection = api.connect(self._session_agent, dict(peer))
            self.session.connect(self._connection)
        except RegistrationRetainedError as exc:
            exc.retainment.owners += (self,)
            raise
        except BaseException:
            self._unwind_partial_construction()
            raise

        self._slots = WindowSlots(budgets.max_inflight)
        self.in_flight: dict[str, InFlightOperation] = {}
        self._closed = False

    def _negotiate(self, transport_options: tuple):
        """Build the reader's negotiation record at agent creation.

        The binding answers through the agent it returned: options it
        applied are echoed as applied, and everything else is an explicit
        rejection ("the binding did not answer"), because silence about
        a transport option is how a missing RDMA path gets called one.
        """
        applied = tuple(
            getattr(self._session_agent, "applied_transport_options", ()) or ()
        )
        answers = {
            option: (True if option in applied else "the binding did not answer")
            for option in transport_options
        }
        return negotiate_transport(tuple(transport_options), answers)

    @property
    def functional_only(self) -> bool:
        """True when the negotiated record cannot claim the required
        transport path; requested, applied, and effective stay apart."""
        return self.capabilities.negotiation.functional_only

    def _unwind_partial_construction(self) -> None:
        """Close what a failed construction managed to open.

        An uncertain cleanup retains every window and its reservation.
        """
        try:
            if self._connection is not None:
                self._api.disconnect(self._session_agent, self._connection)
            if self._registration is not None:
                self._api.deregister(self._session_agent, self._registration)
        except RegistrationRetainedError as exc:
            exc.retainment.owners += (self,)
            raise
        except BaseException as exc:
            retained = retain_registration(
                getattr(self._registration, "handles", {}),
                {window.name: window for window in self.windows},
                "HIXL reader construction cleanup outcome unknown",
            )
            retained.owners = (self,)
            raise RegistrationRetainedError(retained, retained.detail) from exc
        self.ledger.release_reservation(self._reserved_bytes)

    # -- the Reader protocol -------------------------------------------

    def read_bytes(self, file_path: str, begin: int, end: int) -> bytes:
        """Serve [begin, end) of one file through the registered windows."""
        if self._closed:
            raise OperationStateError("the reader is closed")
        descriptor = self.manifest.file(file_path)
        if begin < 0 or end < begin or end > descriptor.size:
            raise RangeOverflowError(
                f"range [{begin}, {end}) exceeds file size {descriptor.size}"
            )
        chunks = []
        position = begin
        while position < end:
            step = min(self._window_bytes, end - position)
            chunks.append(self._read_window(file_path, position, position + step))
            position += step
        return b"".join(chunks)

    def close(self) -> None:
        """Disconnect, then deregister: the documented teardown order.

        Refuses while any operation is retained as UNKNOWN: a qualified
        fence must resolve those first, because the memory a possibly
        in-flight transfer touches cannot be handed back.
        """
        if self._closed:
            return
        retained = sorted(
            op_id for op_id, flight in self.in_flight.items() if flight.retained
        )
        if retained:
            raise OperationStateError(
                f"cannot close the reader: retained UNKNOWN operations "
                f"{retained} hold their windows until a qualified fence"
            )
        self._api.disconnect(self._session_agent, self._connection)
        self.session.disconnect(self._connection)
        self._api.deregister(self._session_agent, self._registration)
        self.session.deregister(self._registration)
        self.ledger.release_reservation(self._reserved_bytes)
        self._closed = True

    # -- the fence surface ----------------------------------------------

    def resolve(
        self,
        op_id: str,
        target: str,
        error_code: str | None = None,
        error_message: str | None = None,
    ) -> None:
        """Resolve one retained UNKNOWN operation through a qualified
        fence; the resolve is also the bytes' release."""
        flight = self.in_flight.get(op_id)
        if flight is None:
            raise OperationStateError(f"unknown operation: {op_id!r}")
        flight.resolve(target, error_code=error_code, error_message=error_message)
        self._release_window(op_id)

    def _read_window(self, file_path: str, begin: int, end: int) -> bytes:
        length = end - begin
        operation = Operation(
            op_id=uuid.uuid4().hex,
            kind="read_range",
            tensor=file_path,
            offset=begin,
            length=length,
        )
        slot = self._slots.acquire()
        window = self.windows[slot]
        self._slots.bind(slot, operation.op_id)
        flight = InFlightOperation(operation, window, self.ledger)
        self.in_flight[operation.op_id] = flight

        submitted = False
        try:
            op_handle = self._submit(window, file_path, begin, length)
            submitted = True
            outcome = self._await(operation, op_handle)
        except AmbiguousReadError:
            # The operation is UNKNOWN and retained: its window and its
            # active bytes stay held until a qualified fence resolves it.
            raise
        except Exception as exc:
            if submitted:
                operation.mark_unknown(
                    f"post-submission failure: {type(exc).__name__}: {exc}"
                )
                raise
            flight.mark_failed("pre-submission", str(exc))
            self._release_window(operation.op_id)
            raise

        if outcome.state == "failed":
            flight.mark_failed("transfer-failed", outcome.detail)
            self._release_window(operation.op_id)
            raise SourceUnavailableError(
                f"the backend failed the transfer: {outcome.detail}"
            )
        payload = bytes(window.buffer[:length])
        flight.mark_succeeded()
        self._release_window(operation.op_id)
        return payload

    def _submit(
        self, window: TransferWindow, file_path: str, begin: int, length: int
    ) -> str:
        region = self._peer_region(file_path)
        if begin + length > region["len"]:
            raise RangeOverflowError(
                f"range [{begin}, {begin + length}) exceeds the peer's "
                f"published region for {file_path!r} ({region['len']} bytes)"
            )
        source = {"addr": int(region["addr"]) + begin, "length": length}
        submit = (
            self._api.submit_write
            if self._direction == DIRECTION_WRITE
            else self._api.submit_read
        )
        return submit(
            self._session_agent,
            self._registration,
            self._connection,
            window.name,
            source,
            length,
        )

    def _peer_region(self, file_path: str) -> dict:
        """The peer's published registration for one checkpoint file.

        The peer descriptor carries the remote base address of each
        registered file region; the vendor library moves addresses, so
        the reader submits addresses, never file names.
        """
        regions = self._peer.get("regions") or {}
        region = regions.get(file_path)
        if region is None or "addr" not in region or "len" not in region:
            raise SourceUnavailableError(
                f"the peer publishes no registered region for {file_path!r}: "
                "a transfer needs its remote address"
            )
        return region

    def _await(self, operation: Operation, op_handle: str):
        deadline = self._now() + self._poll_deadline
        while True:
            outcome = self._api.poll(self._session_agent, op_handle)
            if outcome.state in ("succeeded", "failed"):
                return outcome
            if self._now() >= deadline:
                operation.mark_unknown(
                    "the operation did not reach a terminal state within "
                    "the poll deadline; retained until a qualified fence"
                )
                raise AmbiguousReadError(
                    f"operation {operation.op_id} is nonterminal at its "
                    f"deadline and is retained as UNKNOWN"
                )
            time.sleep(_POLL_INTERVAL_SECONDS)

    def _release_window(self, op_id: str) -> None:
        flight = self.in_flight.get(op_id)
        if flight is None:
            return
        if not flight.operation.state.terminal:
            raise OperationStateError(
                f"window for operation {op_id} is not reusable until the "
                "operation is terminal"
            )
        self._slots.mark_terminal(op_id)
        for index, candidate in enumerate(self.windows):
            if candidate is flight.window:
                self._slots.free(index, op_id)
                return


__all__ = [
    "DEFAULT_POLL_DEADLINE_SECONDS",
    "DIRECTION_READ",
    "DIRECTION_WRITE",
    "HixlReader",
    "InFlightOperation",
    "TransferWindow",
]
