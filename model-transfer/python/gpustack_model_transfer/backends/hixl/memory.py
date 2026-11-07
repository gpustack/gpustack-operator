"""Memory accounting for the CANN route: active, reserved, peak, device.

The same four counters the qualification answer needs, owned by this
backend package: the HIXL route imports nothing from the NIXL backend,
and the NIXL backend imports nothing from here — sibling transports stay
independent, each with its own copy of the transport-independent
accounting contract. Reserved is what the windows and registrations
claim; active is what is in flight right now; peak is the high-water
mark; device is the device-wide runtime allocation measured OUTSIDE any
framework allocator, because matching live tensor bytes alone never
bounded a backend whose runtime reserves pools of its own.

Counter attribution is not a copy proof: port counters say which rail
the bytes used, and nothing about staging. This module records facts
separately so a report cannot blur them.
"""

import ctypes
from dataclasses import dataclass, field

from gpustack_model_transfer.core.budgets import align_up_2m, hixl_allocation_bytes


class AlignedHostBuffer:
    """Own aligned host storage while exposing only its logical payload."""

    def __init__(self, size: int):
        if size <= 0:
            raise ValueError("a registered host buffer must be non-empty")
        self.size_bytes = size
        self.registration_bytes = align_up_2m(size)
        self.allocation_bytes = hixl_allocation_bytes(size)
        self._storage = bytearray(self.allocation_bytes)
        self.allocation_address = ctypes.addressof(
            ctypes.c_char.from_buffer(self._storage)
        )
        self.address = align_up_2m(self.allocation_address)
        begin = self.address - self.allocation_address
        self.buffer = memoryview(self._storage)[begin : begin + size]

    def data_ptr(self) -> int:
        return self.address

    def __len__(self) -> int:
        return self.size_bytes

    def __getitem__(self, key):
        return self.buffer[key]

    def __setitem__(self, key, value):
        self.buffer[key] = value


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
            "peak_reserved_bytes": self._peak_reserved,
            "device_runtime_bytes": self.device_runtime_bytes,
        }


def _require_non_negative(name: str, value: int) -> None:
    if not isinstance(value, int) or isinstance(value, bool) or value < 0:
        raise MemoryAccountingError(
            f"{name} needs a non-negative byte count, got {value!r}"
        )


__all__ = [
    "MemoryAccountingError",
    "MemoryLedger",
]
