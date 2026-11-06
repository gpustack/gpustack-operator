"""Reader sessions: reads, budgets, completion, cancel, release.

A session is opened against one artifact identity, one capability record,
one budget set, and one reader. Reads produce owned outputs whose
`complete` flag states whether the whole tensor was returned. A partial
range cannot be committed as a whole tensor. Content checks belong to the cache. An ambiguous or
unclassified failure leaves its operation UNKNOWN and retained; cancel and
release are idempotent; a prior operation's success never changes another
operation's retention; close refuses to release the reader while any
operation is retained.
"""

import uuid
from dataclasses import dataclass
from typing import Protocol

from gpustack_model_transfer.core.budgets import WindowSlots
from gpustack_model_transfer.core.capabilities import BackendCapabilities
from gpustack_model_transfer.core.descriptors import MAX_INT64
from gpustack_model_transfer.core.errors import (
    AmbiguousReadError,
    AuthenticationError,
    CapacityExhaustedError,
    InvalidDescriptorError,
    ModelTransferError,
    OperationStateError,
    RangeOverflowError,
    StaleGenerationError,
)
from gpustack_model_transfer.core.errors import error_code as kind_of
from gpustack_model_transfer.core.identity import ArtifactIdentity
from gpustack_model_transfer.core.operation import Operation, OpState


class Reader(Protocol):
    """The transport-independent surface a session reads through."""

    manifest: object

    def read_bytes(self, file_path: str, begin: int, end: int) -> bytes: ...


@dataclass(frozen=True)
class ReadOutput:
    """One owned output: fresh bytes plus the operation that produced them.

    complete is True only for a whole tensor with the declared byte count.
    """

    tensor: object
    offset: int
    payload: bytes
    op_id: str
    op: Operation
    complete: bool = False


class ReaderSession:
    """A load attempt bound to one artifact identity and generation."""

    def __init__(
        self,
        identity: ArtifactIdentity,
        capabilities: BackendCapabilities,
        budgets,
        reader: Reader,
    ):
        identity.validate()
        capabilities.validate()
        budgets.validate()
        manifest_identity = reader.manifest.identity()
        if (
            manifest_identity.artifact_uid != identity.artifact_uid
            or manifest_identity.manifest_sha256 != identity.manifest_sha256
        ):
            raise AuthenticationError(
                "reader manifest does not match the artifact identity"
            )
        if manifest_identity.source_generation != identity.source_generation:
            raise StaleGenerationError(
                f"manifest generation "
                f"{manifest_identity.source_generation} != session "
                f"generation {identity.source_generation}"
            )
        self.identity = identity
        self.capabilities = capabilities
        self.budgets = budgets
        self.reader = reader
        self._operations = {}
        self._slots = WindowSlots(budgets.max_inflight)
        self._closed = False

    @property
    def descriptors(self):
        return self.reader.manifest.tensors

    @property
    def operations(self):
        return dict(self._operations)

    def read_tensor(self, name: str) -> ReadOutput:
        tensor = self.reader.manifest.tensor(name)
        return self._read("read_tensor", tensor, 0, tensor.byte_size, complete=True)

    def read_range(self, name: str, offset: int, length: int) -> ReadOutput:
        tensor = self.reader.manifest.tensor(name)
        for value in (offset, length):
            if isinstance(value, bool) or not isinstance(value, int):
                raise InvalidDescriptorError("range offset and length must be integers")
        if offset < 0 or length < 0:
            raise RangeOverflowError(
                f"negative range [{offset}, +{length}) for {name!r}"
            )
        if offset > MAX_INT64 or length > MAX_INT64:
            raise RangeOverflowError(
                f"range [{offset}, +{length}) exceeds the int64 range"
            )
        if offset + length > tensor.byte_size:
            raise RangeOverflowError(
                f"range [{offset}, +{length}) exceeds tensor byte size "
                f"{tensor.byte_size}"
            )
        alignment = self.capabilities.alignment_bytes
        if alignment > 1 and (offset % alignment != 0 or length % alignment != 0):
            raise InvalidDescriptorError(
                f"range [{offset}, +{length}) for {name!r} is not aligned "
                f"to {alignment} bytes"
            )
        covers_all = offset == 0 and length == tensor.byte_size
        return self._read(
            "read_range",
            tensor,
            offset,
            length,
            complete=covers_all,
        )

    def _read(
        self, kind: str, tensor, offset: int, length: int, complete: bool
    ) -> ReadOutput:
        if self._closed:
            raise OperationStateError("session is closed")
        if length > self.budgets.owned_output_bytes:
            # Refuse before any read or allocation: the owned-output budget,
            # separate from the window budget, bounds the tensor.
            raise CapacityExhaustedError(
                f"tensor {tensor.name!r} needs {length} bytes above the "
                f"owned-output budget {self.budgets.owned_output_bytes}"
            )
        if length > self.capabilities.max_request_bytes:
            raise CapacityExhaustedError(
                f"tensor {tensor.name!r} needs {length} bytes above backend "
                f"max_request_bytes {self.capabilities.max_request_bytes}"
            )
        operation = Operation(
            op_id=uuid.uuid4().hex,
            kind=kind,
            tensor=tensor.name,
            offset=offset,
            length=length,
        )
        self._operations[operation.op_id] = operation
        slot = self._slots.acquire()
        self._slots.bind(slot, operation.op_id)
        chunks = []
        try:
            position = offset
            while position < offset + length:
                end = min(position + self.budgets.window_bytes, offset + length)
                chunk = self.reader.read_bytes(
                    tensor.file, tensor.data_begin + position, tensor.data_begin + end
                )
                if len(chunk) != end - position:
                    raise InvalidDescriptorError(
                        "reader returned an incorrect byte count"
                    )
                chunks.append(chunk)
                position = end
        except AmbiguousReadError as exc:
            operation.mark_unknown(str(exc))
            raise
        except ModelTransferError as exc:
            operation.mark_failed(kind_of(exc), str(exc))
            self._finish_slot(operation)
            raise
        except Exception as exc:
            # An unclassified failure has an unknown outcome: retain the
            # operation and its slot until a qualified fence resolves it.
            operation.mark_unknown(f"unclassified failure: {type(exc).__name__}: {exc}")
            raise
        payload = b"".join(chunks)
        operation.mark_succeeded()
        self._finish_slot(operation)
        return ReadOutput(
            tensor=tensor,
            offset=offset,
            payload=payload,
            op_id=operation.op_id,
            op=operation,
            complete=complete,
        )

    def _finish_slot(self, operation: Operation) -> None:
        if operation.state.terminal:
            self._slots.mark_terminal(operation.op_id)
            slot = self._slots.slot_of(operation.op_id)
            if slot is not None:
                self._slots.free(slot, operation.op_id)

    def cancel(self, op_id: str) -> None:
        operation = self._require_operation(op_id)
        operation.mark_cancelled()
        self._finish_slot(operation)

    def resolve(
        self,
        op_id: str,
        target: str,
        error_code: str | None = None,
        error_message: str | None = None,
    ) -> None:
        """Resolve a retained UNKNOWN operation through a qualified fence."""
        operation = self._require_operation(op_id)
        operation.resolve_unknown(
            OpState(target), error_code=error_code, error_message=error_message
        )
        self._finish_slot(operation)

    def release(self, op_id: str) -> None:
        operation = self._require_operation(op_id)
        if operation.state == OpState.UNKNOWN or operation.retained:
            raise OperationStateError(
                f"operation {op_id!r} is retained until a qualified fence resolves it"
            )
        if not operation.state.terminal:
            raise OperationStateError(
                f"operation {op_id!r} is {operation.state.value}; cancel or "
                f"complete it before release"
            )

    def _require_operation(self, op_id: str) -> Operation:
        operation = self._operations.get(op_id)
        if operation is None:
            raise OperationStateError(f"unknown operation: {op_id!r}")
        return operation

    def close(self) -> None:
        """Cancel pending work and release the reader.

        Refuses, leaving everything open, while any operation is retained
        (UNKNOWN): a fence must resolve those first. Idempotent once it has
        succeeded.
        """
        if self._closed:
            return
        retained = sorted(
            op.op_id
            for op in self._operations.values()
            if op.retained or op.state == OpState.UNKNOWN
        )
        if retained:
            raise OperationStateError(
                f"cannot close: retained UNKNOWN operations {retained}"
            )
        for operation in self._operations.values():
            if operation.state == OpState.PENDING:
                operation.mark_cancelled()
            self._finish_slot(operation)
        close = getattr(self.reader, "close", None)
        if close is not None:
            close()
        self._closed = True


__all__ = ["ReadOutput", "ReaderSession"]
