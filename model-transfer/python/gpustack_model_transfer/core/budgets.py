"""Read budgets and window-slot lifecycle.

The tensor budget and the window budget are separate limits: a tensor may
exceed one transfer window when it fits the owned-output budget. A window
slot belongs to one operation and is not reusable until that operation is
terminal, so a nonterminal operation never shares a window with another.
"""

import os
import threading
from dataclasses import dataclass

from gpustack_model_transfer.core.errors import (
    BudgetError,
    CapacityExhaustedError,
    OperationStateError,
)
from gpustack_model_transfer.envs import get_envs

# Contract ceilings for the reader. The window ceiling bounds one transfer
# window at 1 TiB: a configured value from the 1 GiB floor up to it is
# legal — 16 GiB, 64 GiB, 128 GiB included — so a larger window never needs
# a code change, and a configured value below the floor silently resolves
# up to it.
MAX_WINDOW_BYTES = 1 << 40
DEFAULT_MAX_WINDOW_BYTES = 8 << 30
MIN_WINDOW_BYTES = 1 << 30
MIN_ADAPTIVE_WINDOW_BYTES = 256 << 20
MEMORY_ALIGNMENT_BYTES = 2 << 20
MAX_INFLIGHT_OPERATIONS = 256
DEFAULT_TRANSFER_WORKERS = 4
ENV_TRANSFER_WORKERS = "GPUSTACK_MODEL_TRANSFER_WORKERS"
ENV_MAX_WINDOW_BYTES = "GPUSTACK_MODEL_TRANSFER_MAX_WINDOW_BYTES"
MIN_SOURCE_WINDOW_BYTES = 1 << 30
DEFAULT_MIN_SOURCE_WINDOW_BYTES = MIN_SOURCE_WINDOW_BYTES
# Legacy name retained for callers; the protocol floor ignores this setting.
ENV_MIN_SOURCE_WINDOW_BYTES = "GPUSTACK_MODEL_TRANSFER_MIN_SOURCE_WINDOW_BYTES"


def align_down_2m(size: int) -> int:
    """Align a byte ceiling or capacity down to the 2 MiB boundary."""
    if size <= 0:
        return 0
    return (size // MEMORY_ALIGNMENT_BYTES) * MEMORY_ALIGNMENT_BYTES


def align_up_2m(size: int) -> int:
    """Align a needed byte extent up to the 2 MiB boundary."""
    if size <= 0:
        return 0
    return (
        (size + MEMORY_ALIGNMENT_BYTES - 1) // MEMORY_ALIGNMENT_BYTES
    ) * MEMORY_ALIGNMENT_BYTES


def hixl_allocation_bytes(size: int) -> int:
    """Backing extent for an aligned HIXL region, including address padding."""
    return align_up_2m(size) + MEMORY_ALIGNMENT_BYTES if size > 0 else 0


def min_source_window_bytes(env=None) -> int:
    """Resolve the minimum source residency window floor W_min.

    The floor is fixed at the 1 GiB protocol window: no environment variable
    moves it.
    """
    _ = env
    return DEFAULT_MIN_SOURCE_WINDOW_BYTES


def detect_cpu_threads() -> int:
    """Detect available CPU threads for the current process, or fallback to 4."""
    sched_getaffinity = getattr(os, "sched_getaffinity", None)
    if callable(sched_getaffinity):
        try:
            affinity = sched_getaffinity(0)
            if affinity and len(affinity) > 0:
                return len(affinity)
        except OSError:
            pass
    try:
        count = os.cpu_count()
        if isinstance(count, int) and count > 0:
            return count
    except Exception:
        pass
    return DEFAULT_TRANSFER_WORKERS


def transfer_workers(env=None) -> int:
    """Resolve worker/slot concurrency from env or available CPU threads."""
    raw = get_envs(env).GPUSTACK_MODEL_TRANSFER_WORKERS_RAW
    if raw is None or raw == "":
        return min(max(1, detect_cpu_threads()), MAX_INFLIGHT_OPERATIONS)
    if isinstance(raw, bool):
        raise BudgetError(f"{ENV_TRANSFER_WORKERS} must be a positive integer")
    try:
        value = int(str(raw).strip())
    except (TypeError, ValueError):
        raise BudgetError(
            f"{ENV_TRANSFER_WORKERS} must be a positive integer, got {raw!r}"
        ) from None
    if not 1 <= value <= MAX_INFLIGHT_OPERATIONS:
        raise BudgetError(
            f"{ENV_TRANSFER_WORKERS} must be in [1, {MAX_INFLIGHT_OPERATIONS}], "
            f"got {value}"
        )
    return value


def max_window_bytes(env=None) -> int:
    """Resolve the maximum broker transfer window ceiling from env or default.

    A configured value below the 1 GiB floor silently resolves up to it: an
    under-configured window means the floor, not an error. Values above the
    contract ceiling stay a misconfiguration.
    """
    raw = get_envs(env).GPUSTACK_MODEL_TRANSFER_MAX_WINDOW_BYTES_RAW
    if raw is None or raw == "":
        return DEFAULT_MAX_WINDOW_BYTES
    if isinstance(raw, bool):
        raise BudgetError(f"{ENV_MAX_WINDOW_BYTES} must be a positive integer")
    try:
        value = int(str(raw).strip())
    except (TypeError, ValueError):
        raise BudgetError(
            f"{ENV_MAX_WINDOW_BYTES} must be a positive integer, got {raw!r}"
        ) from None
    if value > MAX_WINDOW_BYTES:
        raise BudgetError(
            f"{ENV_MAX_WINDOW_BYTES} must be at most {MAX_WINDOW_BYTES}, got {value}"
        )
    return max(align_down_2m(value), MIN_WINDOW_BYTES)


# The TP load modes. "broadcast" is the default: rank 0 reads and the
# distributed broadcast hands the tensors to the other ranks. "all" reads
# on every rank, so the ranks share one window budget between them.
TP_LOAD_MODE_BROADCAST = "broadcast"
TP_LOAD_MODE_ALL = "all"
_TP_LOAD_MODE_ALIASES = {
    "all": TP_LOAD_MODE_ALL,
    "all_tp": TP_LOAD_MODE_ALL,
    "all-tp": TP_LOAD_MODE_ALL,
    "all_tps": TP_LOAD_MODE_ALL,
    "broadcast": TP_LOAD_MODE_BROADCAST,
    "tp0": TP_LOAD_MODE_BROADCAST,
}
ENV_TP_LOAD_MODE = "GPUSTACK_MODEL_TRANSFER_TP_MODE"


def tp_load_mode(env=None) -> str:
    """Resolve the TP load mode from env, defaulting to broadcast.

    Aliases normalize: "all_tp", "all-tp" and "all_tps" mean "all";
    "tp0" means the default "broadcast". Any other value is a
    misconfiguration, not a fallback.
    """
    raw = get_envs(env).GPUSTACK_MODEL_TRANSFER_TP_MODE
    if raw is None or raw == "":
        return TP_LOAD_MODE_BROADCAST
    mode = _TP_LOAD_MODE_ALIASES.get(raw)
    if mode is None:
        raise BudgetError(
            f"{ENV_TP_LOAD_MODE} must be one of "
            f"{sorted(set(_TP_LOAD_MODE_ALIASES.values()))}, got {raw!r}"
        )
    return mode


def effective_window_bytes(
    env=None, tp_size: int = 1, tp_mode: str | None = None
) -> int:
    """Resolve the per-rank window a TP group shares.

    Under "all" every rank reads for itself, so the configured window
    ceiling is one budget the ranks share: each rank takes an even share,
    aligned down to 2 MiB and floored at the 1 GiB minimum. Every other
    shape gives each rank the whole ceiling, exactly as the broadcast path
    has always loaded.
    """
    mode = tp_mode or tp_load_mode(env)
    if mode == TP_LOAD_MODE_ALL and tp_size > 1:
        return max(align_down_2m(max_window_bytes(env) // tp_size), MIN_WINDOW_BYTES)
    return max_window_bytes(env)


@dataclass(frozen=True)
class ReadBudgets:
    """The three session budgets.

    window_bytes bounds one transfer window (up to MAX_WINDOW_BYTES per slot),
    max_inflight bounds concurrent operations (up to MAX_INFLIGHT_OPERATIONS), and owned_output_bytes bounds one owned output tensor.
    """

    window_bytes: int
    max_inflight: int
    owned_output_bytes: int

    def validate(self) -> None:
        for name, value in (
            ("window_bytes", self.window_bytes),
            ("max_inflight", self.max_inflight),
            ("owned_output_bytes", self.owned_output_bytes),
        ):
            if isinstance(value, bool) or not isinstance(value, int):
                raise BudgetError(f"{name} must be an integer")
        if not 1 <= self.window_bytes <= MAX_WINDOW_BYTES:
            raise BudgetError(
                f"window_bytes must be in [1, {MAX_WINDOW_BYTES}], got "
                f"{self.window_bytes}"
            )
        if not 1 <= self.max_inflight <= MAX_INFLIGHT_OPERATIONS:
            raise BudgetError(
                f"max_inflight must be in [1, {MAX_INFLIGHT_OPERATIONS}], "
                f"got {self.max_inflight}"
            )
        if self.owned_output_bytes < 1:
            raise BudgetError("owned_output_bytes must be positive")


class WindowSlots:
    """Window slots, each bound to at most one operation until it is terminal."""

    def __init__(self, count: int):
        self.count = count
        self._free = set(range(count))
        self._binding = {}  # slot index -> op_id
        self._terminal_ops = set()
        self._lock = threading.Lock()

    def acquire(self) -> int:
        with self._lock:
            if not self._free:
                raise CapacityExhaustedError("no free transfer window slot")
            return self._free.pop()

    def bind(self, slot: int, op_id: str) -> None:
        with self._lock:
            if slot in self._binding:
                raise OperationStateError(f"slot {slot} already bound")
            self._binding[slot] = op_id

    def mark_terminal(self, op_id: str) -> None:
        with self._lock:
            self._terminal_ops.add(op_id)

    def free(self, slot: int, op_id: str) -> bool:
        with self._lock:
            bound = self._binding.get(slot)
            if bound is None:
                raise OperationStateError(f"slot {slot} is not bound")
            if bound != op_id:
                raise OperationStateError(
                    f"slot {slot} belongs to operation {bound!r}, not {op_id!r}"
                )
            if op_id not in self._terminal_ops:
                raise OperationStateError(
                    f"slot {slot} is not reusable until operation {op_id!r} is terminal"
                )
            del self._binding[slot]
            self._terminal_ops.discard(op_id)
            self._free.add(slot)
            return True

    def slot_of(self, op_id: str):
        with self._lock:
            for slot, bound in self._binding.items():
                if bound == op_id:
                    return slot
            return None


__all__ = [
    "DEFAULT_MAX_WINDOW_BYTES",
    "DEFAULT_TRANSFER_WORKERS",
    "ENV_MAX_WINDOW_BYTES",
    "ENV_TP_LOAD_MODE",
    "ENV_TRANSFER_WORKERS",
    "MAX_INFLIGHT_OPERATIONS",
    "MAX_WINDOW_BYTES",
    "MEMORY_ALIGNMENT_BYTES",
    "MIN_ADAPTIVE_WINDOW_BYTES",
    "MIN_WINDOW_BYTES",
    "TP_LOAD_MODE_ALL",
    "TP_LOAD_MODE_BROADCAST",
    "BudgetError",
    "CapacityExhaustedError",
    "OperationStateError",
    "ReadBudgets",
    "WindowSlots",
    "align_down_2m",
    "align_up_2m",
    "detect_cpu_threads",
    "effective_window_bytes",
    "max_window_bytes",
    "tp_load_mode",
    "transfer_workers",
]
