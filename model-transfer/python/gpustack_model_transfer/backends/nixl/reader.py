"""Reference reader for injected NIXL window and safety qualification.

Production engine transfers use client_native.TargetNative and published
address descriptors. This file-path WRITE seam requires an injected API;
RealNixlAPI deliberately refuses file-path writes and per-agent UCX settings.

One reader serves a verified manifest's byte ranges through fixed,
reusable, registered windows: window_bytes sized, at most max_inflight
outstanding, a window reused only after
its operation reached a terminal state, registrations renewed when the
backend reports them expired.

Every submitted operation owns its state and its window slot. A refusal
before submission fails cleanly and frees the slot; a submitted operation
that will not reach a terminal state within the poll deadline becomes
UNKNOWN and RETAINS its slot — and the reader cannot close over a retained
operation, because releasing a window whose transfer may still be in
flight is exactly what quarantine forbids.

The transfers are source-initiated WRITEs into the local windows: the
measured direction, a CANDIDATE the safety gate must qualify, never an
immutability answer. read_bytes hands out owned bytes copied out of the
window; a window is never aliased into a caller's output.
"""

import time
import uuid

from gpustack_model_transfer.backends.nixl.endpoints import require_memlock
from gpustack_model_transfer.backends.nixl.errors import RegistrationExpiredError
from gpustack_model_transfer.backends.nixl.memory import MemoryLedger
from gpustack_model_transfer.core.budgets import WindowSlots
from gpustack_model_transfer.core.errors import (
    AmbiguousReadError,
    ModelTransferError,
    OperationStateError,
    RangeOverflowError,
    SourceUnavailableError,
)
from gpustack_model_transfer.core.operation import Operation, OpState

# The default observation window for one submitted operation before its
# outcome is declared UNKNOWN: long enough for a real poll loop, short
# enough that a hung transfer cannot wedge the reader silently.
DEFAULT_POLL_DEADLINE_SECONDS = 30.0
_POLL_INTERVAL_SECONDS = 0.001


class TransferWindow:
    """One fixed, reusable, registered window buffer."""

    def __init__(self, name: str, size: int):
        self.name = name
        self.size = size
        self.buffer = bytearray(size)

    def __len__(self) -> int:
        return self.size


class NixlReader:
    """A Reader-protocol implementation over one NIXL agent.

    The reader owns its windows, their registration, and their memory
    reservation for its lifetime; close() releases them, and refuses to
    while any operation is retained as UNKNOWN.
    """

    def __init__(
        self,
        manifest,
        api,
        target: dict,
        budgets,
        selection,
        ledger: MemoryLedger | None = None,
        *,
        poll_deadline_seconds: float = DEFAULT_POLL_DEADLINE_SECONDS,
        memlock_limit: int | None = None,
        now=None,
    ):
        self.manifest = manifest
        self._api = api
        self._target = dict(target)
        self._budgets = budgets
        self._selection = selection
        self.ledger = ledger if ledger is not None else MemoryLedger()
        self._poll_deadline = poll_deadline_seconds
        self._now = now or time.monotonic

        # The registration budget must fit the service memory lock before
        # any registration: a pin that cannot be made is a refusal here,
        # not a failure mid-transfer. memlock_limit, when given, stands in
        # for the host limit (tests, or a caller that already read it).
        require_memlock(
            budgets.window_bytes * budgets.max_inflight,
            soft_limit=memlock_limit,
        )

        self._agent = api.create_agent(
            "model-transfer-reader", selection.agent_settings()
        )
        self.windows = tuple(
            TransferWindow(f"window-{index}", budgets.window_bytes)
            for index in range(budgets.max_inflight)
        )
        self._registration = api.register_memory(
            self._agent,
            {window.name: window.buffer for window in self.windows},
        )
        self.ledger.reserve(budgets.window_bytes * budgets.max_inflight)
        self.registration_renewals = 0

        self._slots = WindowSlots(budgets.max_inflight)
        self.operations: dict[str, Operation] = {}
        self._op_handles = {}
        self._op_windows: dict[str, TransferWindow] = {}
        self._op_active_bytes: dict[str, int] = {}
        self._closed = False

    # -- the Reader protocol ------------------------------------------

    def read_bytes(self, file_path: str, begin: int, end: int) -> bytes:
        """Serve [begin, end) of one file through the fixed windows."""
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
            step = min(self._budgets.window_bytes, end - position)
            chunks.append(self._read_window(file_path, position, position + step))
            position += step
        return b"".join(chunks)

    def close(self) -> None:
        """Release windows, registration, and the memory reservation.

        Refuses while any operation is retained as UNKNOWN: a qualified
        fence must resolve those first, because the memory a possibly
        in-flight transfer writes into cannot be handed back.
        """
        if self._closed:
            return
        retained = sorted(
            op_id
            for op_id, operation in self.operations.items()
            if operation.retained or operation.state == OpState.UNKNOWN
        )
        if retained:
            raise OperationStateError(
                f"cannot close the reader: retained UNKNOWN operations "
                f"{retained} hold their windows until a qualified fence"
            )
        self._api.revoke_registration(self._agent, self._registration)
        self.ledger.release_reservation(
            self._budgets.window_bytes * self._budgets.max_inflight
        )
        self._closed = True

    # -- the per-operation and fence surface ---------------------------

    def resolve(
        self,
        op_id: str,
        target: str,
        error_code: str | None = None,
        error_message: str | None = None,
        *,
        external_fence: str | None = None,
    ) -> None:
        """Release the native handle after a documented external fence.

        A failed native release leaves the operation and window UNKNOWN.
        """
        operation = self.operations.get(op_id)
        if operation is None:
            raise OperationStateError(f"unknown operation: {op_id!r}")
        try:
            target_state = OpState(target)
        except ValueError:
            raise OperationStateError("unknown resolution target") from None
        if operation.state != OpState.UNKNOWN or target_state not in (
            OpState.SUCCEEDED,
            OpState.FAILED,
        ):
            raise OperationStateError(
                "resolution requires UNKNOWN and a terminal target"
            )
        handle = self._op_handles.get(op_id)
        if handle is not None:
            release = getattr(self._api, "release_operation", None)
            if not callable(release):
                raise OperationStateError(
                    "runtime cannot release the retained native handle"
                )
            release(self._agent, handle, external_fence=external_fence)
            del self._op_handles[op_id]
        operation.resolve_unknown(
            target_state, error_code=error_code, error_message=error_message
        )
        self._deactivate(op_id)
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
        self.operations = {
            key: value
            for key, value in self.operations.items()
            if not value.state.terminal
        }
        slot = self._slots.acquire()
        self.operations[operation.op_id] = operation
        window = self.windows[slot]
        self._slots.bind(slot, operation.op_id)
        self._op_windows[operation.op_id] = window
        active_span = min(length, window.size)
        self._op_active_bytes[operation.op_id] = active_span
        self.ledger.activate(active_span)

        submitted = False
        try:
            op_handle = self._submit(window, file_path, begin, length)
            submitted = True
            self._op_handles[operation.op_id] = op_handle
            outcome = self._await(operation, op_handle)
            self._op_handles.pop(operation.op_id, None)
        except AmbiguousReadError:
            # _await already marked the operation UNKNOWN and it is
            # retained: nothing here may release or re-mark it.
            raise
        except ModelTransferError as exc:
            if submitted:
                # A refusal after submission has an unknown outcome.
                operation.mark_unknown(f"post-submission failure: {exc}")
                raise
            operation.mark_failed("pre-submission", str(exc))
            self._deactivate(operation.op_id)
            self._release_window(operation.op_id)
            raise
        except Exception as exc:  # unclassified: unknown outcome, retained
            operation.mark_unknown(f"unclassified failure: {type(exc).__name__}: {exc}")
            raise

        if outcome.state == "failed":
            operation.mark_failed("transfer-failed", outcome.detail)
            self._deactivate(operation.op_id)
            self._release_window(operation.op_id)
            raise SourceUnavailableError(
                f"the backend failed the transfer: {outcome.detail}"
            )
        # succeeded: copy out of the window into owned bytes, then free.
        payload = bytes(window.buffer[:length])
        operation.mark_succeeded()
        self._deactivate(operation.op_id)
        self._release_window(operation.op_id)
        return payload

    def _submit(self, window: TransferWindow, file_path: str, begin: int, length: int):
        target = dict(self._target)
        target["file"] = file_path
        target["offset"] = begin
        try:
            return self._api.submit_write(
                self._agent, self._registration, window.name, target, length
            )
        except RegistrationExpiredError:
            # A fixed window's registration is renewed in place, not
            # re-registered per read; the renewal is counted.
            self._api.renew_registration(self._agent, self._registration)
            self.registration_renewals += 1
            return self._api.submit_write(
                self._agent, self._registration, window.name, target, length
            )

    def _await(self, operation: Operation, op_handle) -> object:
        deadline = self._now() + self._poll_deadline
        while True:
            outcome = self._api.poll(self._agent, op_handle)
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

    def _deactivate(self, op_id: str) -> None:
        """Return one operation's active bytes to the reservation."""
        active = self._op_active_bytes.pop(op_id, None)
        if active is not None:
            self.ledger.deactivate(active)

    def _release_window(self, op_id: str) -> None:
        window = self._op_windows.pop(op_id, None)
        if window is None:
            return
        operation = self.operations[op_id]
        if not operation.state.terminal:
            raise OperationStateError(
                f"window for operation {op_id} is not reusable until the "
                "operation is terminal"
            )
        self._slots.mark_terminal(op_id)
        for index, candidate in enumerate(self.windows):
            if candidate is window:
                self._slots.free(index, op_id)
                return


__all__ = ["DEFAULT_POLL_DEADLINE_SECONDS", "NixlReader", "TransferWindow"]
