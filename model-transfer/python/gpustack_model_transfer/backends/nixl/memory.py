"""Memory accounting for the NIXL route: active, reserved, peak, device.

Four numbers a qualification answer needs, kept apart because they answer
different questions. Reserved is what the windows and registrations claim;
active is what is in flight right now; peak is the high-water mark; device
is the device-wide runtime allocation measured OUTSIDE any framework
allocator, because matching live tensor bytes alone never bounded a
backend whose runtime reserves pools of its own.

Counter attribution is not a copy proof: port counters say which rail the
bytes used, and nothing about staging. This module records both facts
separately so a report cannot blur them.
"""

from dataclasses import dataclass, field


class MemoryAccountingError(Exception):
    """The ledger was asked to describe an impossible state."""


@dataclass
class MemoryLedger:
    """The four counters, updated at the boundaries that move them."""

    reserved_bytes: int = 0
    active_bytes: int = 0
    peak_bytes: int = 0
    device_runtime_bytes: int = 0
    _peak_reserved: int = field(default=0, repr=False)

    def reserve(self, byte_count: int) -> None:
        """Claim bytes for windows and registrations, before use."""
        _require_non_negative("reserve", byte_count)
        self.reserved_bytes += byte_count
        self._peak_reserved = max(self._peak_reserved, self.reserved_bytes)
        self.peak_bytes = max(self.peak_bytes, self.active_bytes)

    def release_reservation(self, byte_count: int) -> None:
        _require_non_negative("release_reservation", byte_count)
        if byte_count > self.reserved_bytes:
            raise MemoryAccountingError(
                f"releasing {byte_count} reserved bytes with only "
                f"{self.reserved_bytes} held"
            )
        self.reserved_bytes -= byte_count

    def activate(self, byte_count: int) -> None:
        """Mark reserved bytes as in flight."""
        _require_non_negative("activate", byte_count)
        if byte_count > self.reserved_bytes - self.active_bytes:
            raise MemoryAccountingError(
                f"activating {byte_count} bytes above the reservation "
                f"({self.reserved_bytes} reserved, {self.active_bytes} active)"
            )
        self.active_bytes += byte_count
        self.peak_bytes = max(self.peak_bytes, self.active_bytes)

    def deactivate(self, byte_count: int) -> None:
        _require_non_negative("deactivate", byte_count)
        if byte_count > self.active_bytes:
            raise MemoryAccountingError(
                f"deactivating {byte_count} bytes with only {self.active_bytes} active"
            )
        self.active_bytes -= byte_count

    def observe_device_runtime(self, byte_count: int) -> int:
        """Record the device-wide runtime allocation measurement."""
        _require_non_negative("observe_device_runtime", byte_count)
        self.device_runtime_bytes = byte_count
        return byte_count

    @property
    def peak_reserved_bytes(self) -> int:
        return self._peak_reserved

    def snapshot(self) -> dict:
        """The four counters as a reportable record."""
        return {
            "reserved_bytes": self.reserved_bytes,
            "active_bytes": self.active_bytes,
            "peak_bytes": self.peak_bytes,
            "peak_reserved_bytes": self.peak_reserved_bytes,
            "device_runtime_bytes": self.device_runtime_bytes,
        }


@dataclass(frozen=True)
class CounterAttribution:
    """Port counters, labeled with what they do and do not prove."""

    rail: str
    counters: dict
    # Counters attribute the probe's traffic to this rail. They do not
    # prove the transfer path: zero-copy, the absence of host staging, and
    # any direct-path claim need independent backend observations.
    attributes_traffic: bool = True
    proves_transfer_path: bool = False

    def validate(self) -> None:
        if self.proves_transfer_path:
            raise MemoryAccountingError(
                "port counters attribute traffic to a rail; they never "
                "prove the transfer path, and a record claiming so is wrong"
            )


def _require_non_negative(name: str, value: int) -> None:
    if not isinstance(value, int) or isinstance(value, bool) or value < 0:
        raise MemoryAccountingError(
            f"{name} needs a non-negative byte count, got {value!r}"
        )


__all__ = [
    "CounterAttribution",
    "MemoryAccountingError",
    "MemoryLedger",
]
