"""Typed HIXL backend errors, each naming its own rule.

Every refusal a caller must distinguish carries its own class and code:
the vendor stack being absent, a version this route was not pinned
against, a transport option the binding rejected, a session used out of
its documented order, and a transfer the backend refused.
"""

from gpustack_model_transfer.core.errors import ModelTransferError


class RuntimeUnavailableError(ModelTransferError):
    """The vendor stack is absent; the reason says which piece."""

    code = "runtime-unavailable"


class VersionMismatchError(ModelTransferError):
    """The runtime's version is not the one this route was pinned against."""

    code = "version-mismatch"


class TransportOptionRejectedError(ModelTransferError):
    """The binding rejected a requested transport option. The option is
    reported, never silently dropped: an omitted option must not be read
    as the requested RDMA path being in force."""

    code = "transport-option-rejected"


class SessionOrderError(ModelTransferError):
    """The session was used outside its documented order: register before
    connect, disconnect before deregister, unless the relaxed fabric mode
    was explicitly proven and enabled for this backend."""

    code = "session-order"


class TransferRefusedError(ModelTransferError):
    """The backend refused a transfer at its own boundary."""

    code = "transfer-refused"


class SourceNpuContextRequiredError(ModelTransferError):
    """A transfer needs a source NPU context this route has not qualified
    an accelerator-free alternative for."""

    code = "source-npu-context-required"


__all__ = [
    "RuntimeUnavailableError",
    "SessionOrderError",
    "SourceNpuContextRequiredError",
    "TransferRefusedError",
    "TransportOptionRejectedError",
    "VersionMismatchError",
]
