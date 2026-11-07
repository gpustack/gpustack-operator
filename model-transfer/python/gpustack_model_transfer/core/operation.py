"""Per-operation state: completion, cancellation, and UNKNOWN retention.

Every submitted operation owns its state. PENDING operations complete,
fail, are cancelled, or become UNKNOWN at a deadline. UNKNOWN is a
first-class retained state: its resources are held until a qualified fence
resolves it, and no other operation's success changes that. Cancel is
idempotent and never resurrects a decided operation.
"""

from dataclasses import dataclass, field
from enum import Enum

from gpustack_model_transfer.core.errors import OperationStateError


class OpState(Enum):
    PENDING = "pending"
    SUCCEEDED = "succeeded"
    FAILED = "failed"
    CANCELLED = "cancelled"
    UNKNOWN = "unknown"

    @property
    def terminal(self) -> bool:
        return self in (OpState.SUCCEEDED, OpState.FAILED, OpState.CANCELLED)


@dataclass
class Operation:
    """One submitted read with its own state machine."""

    op_id: str
    kind: str
    tensor: str
    offset: int
    length: int
    state: OpState = OpState.PENDING
    error_code: str | None = None
    error_message: str | None = None
    retained: bool = field(default=False)

    def mark_succeeded(self) -> None:
        if self.state == OpState.SUCCEEDED:
            return
        if self.state not in (OpState.PENDING, OpState.UNKNOWN):
            raise OperationStateError(
                f"cannot succeed {self.kind} {self.op_id} from {self.state.value}"
            )
        self.state = OpState.SUCCEEDED
        self.retained = False

    def mark_failed(self, error_code: str, error_message: str) -> None:
        if self.state == OpState.FAILED:
            return
        if self.state not in (OpState.PENDING, OpState.UNKNOWN):
            raise OperationStateError(
                f"cannot fail {self.kind} {self.op_id} from {self.state.value}"
            )
        self.state = OpState.FAILED
        self.error_code = error_code
        self.error_message = error_message
        self.retained = False

    def mark_cancelled(self) -> None:
        # Idempotent: cancelling a decided operation is a no-op, and an
        # UNKNOWN operation stays retained because its outcome is unknown.
        if self.state == OpState.PENDING:
            self.state = OpState.CANCELLED

    def mark_unknown(self, reason: str) -> None:
        if self.state != OpState.PENDING:
            raise OperationStateError(
                f"cannot mark {self.kind} {self.op_id} unknown from {self.state.value}"
            )
        self.state = OpState.UNKNOWN
        self.error_message = reason
        self.retained = True

    def resolve_unknown(
        self,
        target: OpState,
        error_code: str | None = None,
        error_message: str | None = None,
    ) -> None:
        if self.state != OpState.UNKNOWN:
            raise OperationStateError(
                f"only UNKNOWN operations resolve, {self.op_id} is {self.state.value}"
            )
        if target not in (OpState.SUCCEEDED, OpState.FAILED):
            raise OperationStateError(
                f"UNKNOWN resolves to a terminal state, not {target.value}"
            )
        self.state = target
        self.error_code = error_code
        self.error_message = error_message
        self.retained = False

    def can_release(self) -> bool:
        return self.state.terminal and not self.retained


__all__ = ["OpState", "Operation", "OperationStateError"]
