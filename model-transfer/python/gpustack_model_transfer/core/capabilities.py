"""Backend capabilities and transport selection.

A capability record answers three separate questions: which transport
options were requested, which one was applied, and which backend was
independently observed as effective. An option the backend rejected is
reported with its reason, never silently dropped, and an unobserved
effective backend stays "unknown" instead of inheriting the request.
"""

from dataclasses import dataclass

from gpustack_model_transfer.core.errors import ModelTransferError

OWNERSHIP_OWNED = "owned"
OWNERSHIP_BORROWED = "borrowed"
_KNOWN_OWNERSHIP = (OWNERSHIP_OWNED, OWNERSHIP_BORROWED)


class InvalidCapabilitiesError(ModelTransferError):
    code = "invalid-capabilities"


@dataclass(frozen=True)
class RejectedOption:
    """A requested transport option the backend refused, with its reason."""

    option: str
    reason: str


@dataclass(frozen=True)
class TransportSelection:
    """Control-plane and data-plane endpoints, selected separately."""

    control: str
    payload: tuple


def _is_power_of_two(value: int) -> bool:
    return (
        not isinstance(value, bool)
        and isinstance(value, int)
        and value >= 1
        and value & (value - 1) == 0
    )


@dataclass(frozen=True)
class BackendCapabilities:
    """What one backend can do, as reported after negotiation.

    requested, applied, and effective keep the requested/applied/effective
    backend distinction; effective_observed records whether the effective
    backend was independently observed or only assumed. transport carries
    the separately selected control and payload paths.
    """

    requested: tuple
    applied: str
    effective: str
    effective_observed: bool
    rejected: tuple
    formats: tuple
    memory_types: tuple
    max_request_bytes: int
    alignment_bytes: int
    tensor_ownership: str
    device_visibility: tuple
    transport: TransportSelection

    def validate(self) -> None:
        def fail(message: str):
            raise InvalidCapabilitiesError(message)

        if not self.requested or not all(self.requested):
            fail("at least one transport option must be requested")
        if self.applied not in self.requested:
            fail(f"applied option {self.applied!r} was not requested")
        rejected_options = set()
        for rejected in self.rejected:
            if not isinstance(rejected, RejectedOption):
                fail("rejected entries must be RejectedOption values")
            if not rejected.option or not rejected.reason:
                fail("rejected options carry an option and a reason")
            if rejected.option not in self.requested:
                fail(f"rejected option {rejected.option!r} was not requested")
            rejected_options.add(rejected.option)
        if self.applied in rejected_options:
            fail(f"option {self.applied!r} is both applied and rejected")
        if self.effective_observed:
            if not self.effective or self.effective == "unknown":
                fail("an observed effective backend must be named")
        elif self.effective != "unknown":
            fail("an unobserved effective backend must stay unknown")
        for field in ("formats", "memory_types", "device_visibility"):
            if not getattr(self, field):
                fail(f"{field} must not be empty")
        if (
            isinstance(self.max_request_bytes, bool)
            or not isinstance(self.max_request_bytes, int)
            or self.max_request_bytes < 1
        ):
            fail("max_request_bytes must be a positive integer")
        if not _is_power_of_two(self.alignment_bytes):
            fail("alignment_bytes must be a power of two")
        if self.tensor_ownership not in _KNOWN_OWNERSHIP:
            fail(f"unknown tensor ownership: {self.tensor_ownership!r}")
        if not isinstance(self.transport, TransportSelection):
            fail("transport must be a TransportSelection")
        if not self.transport.control or not self.transport.payload:
            fail("control and payload selection must both be non-empty")


def local_backend_capabilities() -> BackendCapabilities:
    """Capabilities of the local verified-file reference reader.

    The local backend reads bytes from a verified file on the same host: no
    vendor runtime, no device memory, one effective backend that is directly
    observed.
    """
    return BackendCapabilities(
        requested=("local",),
        applied="local",
        effective="local",
        effective_observed=True,
        rejected=(),
        formats=("safetensors",),
        memory_types=("host",),
        max_request_bytes=8 << 30,
        alignment_bytes=1,
        tensor_ownership=OWNERSHIP_OWNED,
        device_visibility=("host",),
        transport=TransportSelection(
            control="local://verified-manifest", payload=("local://file",)
        ),
    )


__all__ = [
    "BackendCapabilities",
    "InvalidCapabilitiesError",
    "RejectedOption",
    "TransportSelection",
    "local_backend_capabilities",
]
