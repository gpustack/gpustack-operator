"""Typed NIXL backend errors, each naming its own rule.

Every refusal a caller must distinguish carries its own class and code:
the runtime being absent or mismatched, an endpoint the route refuses, a
memory lock too small to register, and a transfer the backend refused.
All subclass the core ModelTransferError so they compose with the reader contract's
error handling.
"""

from gpustack_model_transfer.core.errors import ModelTransferError


class RuntimeUnavailableError(ModelTransferError):
    """The vendor runtime is absent; the reason says which piece."""

    code = "runtime-unavailable"


class VersionMismatchError(ModelTransferError):
    """The runtime's version is not one this backend was pinned against."""

    code = "version-mismatch"


class UnqualifiedEndpointError(ModelTransferError):
    """An endpoint the route refuses: a wildcard, an unset variable, or a
    device selection that would reach beyond the granted rails."""

    code = "unqualified-endpoint"


class MemlockInsufficientError(ModelTransferError):
    """The service memory lock cannot cover the registered budgets."""

    code = "memlock-insufficient"


class TransferRefusedError(ModelTransferError):
    """The backend refused a transfer at its own boundary."""

    code = "transfer-refused"


class RegistrationExpiredError(ModelTransferError):
    """A registration a backend reports as no longer honored.

    Only a backend that can detect expiry raises this; the pinned nixl
    1.5.0 surface has no renewal call and reports no expiry, so the real
    wrapper never raises it.
    """

    code = "registration-expired"


class UnsupportedSettingError(ModelTransferError):
    """A per-agent setting the pinned runtime cannot scope to one agent.

    The backend never writes a process-wide environment variable to
    pretend per-agent isolation; a setting it cannot honor exactly is
    refused with the pin named.
    """

    code = "unsupported-setting"


class FenceRequiredError(ModelTransferError):
    """Normal cleanup refused: the operation is not proven terminal.

    Releasing a still-active native transfer requests a cancel, and a
    cancel is not a qualified fence; cleanup of a pending or UNKNOWN
    operation needs external, documented fence evidence.
    """

    code = "fence-required"


__all__ = [
    "FenceRequiredError",
    "MemlockInsufficientError",
    "RegistrationExpiredError",
    "RuntimeUnavailableError",
    "TransferRefusedError",
    "UnqualifiedEndpointError",
    "UnsupportedSettingError",
    "VersionMismatchError",
]
