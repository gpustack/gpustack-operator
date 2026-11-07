"""Pinned host memory for the source holder: real allocations, owned.

The source fills verified safetensors bytes into pinned host memory it
owns, then registers that exact memory with the native backend. Two
allocation owners exist, one per vendor scope:

- NVIDIA source: owned mappings pinned through Torch's CUDA/HIP runtime.
- Ascend source: ACL page-locked host memory (aclrtMallocHost) under a
  process-wide, refcounted ACL runtime the HIXL backend shares. The
  source image intentionally ships no Torch/torch_npu, so the ACL path
  never imports either.

No plain bytearray is ever labelled pinned: a block reports its own
address through data_ptr() and knows the owner that allocated it, so a
quarantine retainment can keep the allocation alive exactly as long as
the native registration that points into it.

Both owners import their vendor lazily, inside a method: a host without
the runtime refuses visibly (typed SourceUnavailableError) instead of
crashing on the import. An injected module is an explicit Python
constructor or test-helper argument only — never an environment or CLI
production fallback.
"""

import ctypes
import mmap
import threading

from gpustack_model_transfer.core.budgets import (
    MEMORY_ALIGNMENT_BYTES,
    align_down_2m,
    align_up_2m,
    hixl_allocation_bytes,
)
from gpustack_model_transfer.core.errors import SourceUnavailableError

_FILL_CHUNK_BYTES = 1 << 20

# The ACL runtime is process-wide and refcounted: aclInit and the device
# context are shared by every HIXL generation in this process, and neither
# is ever reset or finalized while one more generation or one quarantined
# retainment still holds a reference. A per-generation process inside one
# DaemonSet is equally valid; within one process, this refcount is the
# lifetime contract.
_ACL_LOCK = threading.Lock()
_ACL_RUNTIME = None
_ACL_BROKEN = None


class HostBlock:
    """One pinned host allocation: address, true extent, writer, owner.

    write_bytes lands bounded chunks directly into the pinned memory —
    the fill path never materializes a whole file in Python bytes first.
    The block satisfies the backend buffer contract (data_ptr, nbytes,
    cpu device, contiguity) so the native registration pins exactly this
    allocation.
    """

    def __init__(self, name: str, byte_size: int, owner):
        self.name = name
        self.byte_size = byte_size
        self.owner = owner
        self._device = HostDevice()

    @property
    def device(self):
        return self._device

    @property
    def nbytes(self) -> int:
        return self.byte_size

    def is_contiguous(self) -> bool:
        return True

    def data_ptr(self) -> int:
        raise NotImplementedError

    def write_bytes(self, offset: int, data) -> None:
        raise NotImplementedError

    def view(self, offset: int, length: int):
        if offset < 0 or length < 0 or offset + length > self.byte_size:
            raise ValueError("buffer range exceeds allocation")
        return memoryview(
            (ctypes.c_ubyte * length).from_address(self.data_ptr() + offset)
        ).cast("B")

    def read_bytes(self, offset: int, length: int) -> bytes:
        return bytes(self.view(offset, length))


class HostDevice:
    type = "cpu"


class TorchPinnedOwner:
    """Allocate pinned host memory through Torch's CUDA/HIP runtime.

    Targets use cached pinned tensors by default. Sources set cache=False
    to register and free owned mappings without retaining pinned cache pages.
    Vendor imports stay lazy; tests can inject a runtime explicitly.
    """

    def __init__(self, torch_module=None, *, cache=True):
        self._torch = torch_module
        self._cache = cache
        self._uncertain_allocations = []

    def _require_torch(self):
        if self._torch is None:
            try:
                import torch as torch_module
            except ImportError as exc:
                raise SourceUnavailableError(
                    "the NVIDIA source needs torch for pinned host "
                    f"memory and torch is unavailable: {exc}"
                ) from None
            self._torch = torch_module
        return self._torch

    def allocate(self, name: str, byte_size: int) -> HostBlock:
        torch = self._require_torch()
        if byte_size <= 0:
            raise ValueError(f"allocation must be positive, got {byte_size}")
        if not self._cache:
            return self._allocate_uncached(name, byte_size, torch.cuda.cudart())
        try:
            tensor = torch.empty(byte_size, dtype=torch.uint8, pin_memory=True)
            if (
                byte_size >= MEMORY_ALIGNMENT_BYTES
                and int(tensor.data_ptr()) % MEMORY_ALIGNMENT_BYTES != 0
                and hasattr(tensor, "__getitem__")
            ):
                tensor = None
                padded = torch.empty(
                    byte_size + MEMORY_ALIGNMENT_BYTES,
                    dtype=torch.uint8,
                    pin_memory=True,
                )
                pad_offset = (-int(padded.data_ptr())) % MEMORY_ALIGNMENT_BYTES
                tensor = padded[pad_offset : pad_offset + byte_size]
        except Exception as exc:
            raise SourceUnavailableError(
                f"pinned host allocation of {byte_size} bytes failed: "
                f"{type(exc).__name__}"
            ) from None
        if not tensor.is_pinned():
            raise SourceUnavailableError("Torch returned unpinned host memory")
        return TorchBlock(name, byte_size, self, tensor)

    def _allocate_uncached(self, name, byte_size, runtime):
        # The source must return pinned pages on eviction. Torch's caching
        # allocator rounds and retains freed storage, so register an owned,
        # page-aligned mapping through its CUDA/HIP runtime instead.
        mapping = mmap.mmap(-1, byte_size)
        address = ctypes.addressof(ctypes.c_ubyte.from_buffer(mapping))
        block = RegisteredHostBlock(name, byte_size, self, mapping, address, runtime)
        try:
            status = int(runtime.cudaHostRegister(address, byte_size, 0))
            if status != 0:
                raise SourceUnavailableError("source host registration failed")
        except BaseException:
            self._uncertain_allocations.append(block)
            raise
        return block

    def release(self, block) -> None:
        if isinstance(block, RegisteredHostBlock):
            if block.address == 0:
                return
            if int(block.runtime.cudaHostUnregister(block.address)) != 0:
                raise SourceUnavailableError("source host unregister failed")
            block.mapping.close()
            block.address = 0
            return
        # The tensor's storage frees with the block; dropping the last
        # reference returns the pinned pages to the CUDA caching allocator.
        block.tensor = None


class RegisteredHostBlock(HostBlock):
    """Source-owned pinned mapping, retained on any ambiguous native release."""

    def __init__(self, name, byte_size, owner, mapping, address, runtime):
        super().__init__(name, byte_size, owner)
        self.mapping = mapping
        self.address = address
        self.runtime = runtime

    def data_ptr(self):
        if self.address == 0:
            raise RuntimeError("this pinned block was already released")
        return self.address

    def write_bytes(self, offset, data):
        self.view(offset, len(data))[:] = data


class TorchBlock(HostBlock):
    def __init__(self, name: str, byte_size: int, owner, tensor):
        super().__init__(name, byte_size, owner)
        self.tensor = tensor

    def data_ptr(self) -> int:
        if self.tensor is None:
            raise RuntimeError("this pinned block was already released")
        return int(self.tensor.data_ptr())

    def write_bytes(self, offset: int, data) -> None:
        if self.tensor is None:
            raise RuntimeError("this pinned block was already released")
        if offset < 0 or offset + len(data) > self.byte_size:
            raise ValueError(
                f"write [{offset}, {offset + len(data)}) exceeds block {self.byte_size}"
            )
        torch = self.owner._require_torch()
        view = torch.frombuffer(bytearray(data), dtype=torch.uint8)
        self.tensor[offset : offset + len(data)].copy_(view)

    def read_bytes(self, offset: int, length: int) -> bytes:
        if self.tensor is None:
            raise RuntimeError("this pinned block was already released")
        return bytes(self.view(offset, length))


class AclHostOwner:
    """Allocates page-locked host memory through the ACL runtime.

    The runtime (aclInit, aclrtSetDevice, aclrtCreateContext) is acquired
    once per process and refcounted; every allocation and every
    quarantined retainment holds one reference, so a device or context is
    never reset or finalized underneath another generation. The
    libascendcl handle comes from ctypes by default; tests inject a
    loader explicitly.
    """

    def __init__(self, device_id: int = 0, loader=None):
        if (
            isinstance(device_id, bool)
            or not isinstance(device_id, int)
            or device_id < 0
        ):
            raise ValueError(
                f"ACL device id must be a non-negative int, got {device_id!r}"
            )
        self._device_id = device_id
        self._loader = loader
        self._uncertain_allocations = []
        self._runtime = _acquire_acl_runtime(self._device_id, loader)
        try:
            self.bind()
        except BaseException:
            _release_acl_runtime(self._runtime)
            self._runtime = None
            raise

    def bind(self):
        status = int(
            self._runtime.lib.aclrtSetCurrentContext(
                ctypes.c_void_p(self._runtime.context)
            )
        )
        if status != 0:
            raise SourceUnavailableError("aclrtSetCurrentContext failed")

    def allocate(self, name: str, byte_size: int) -> HostBlock:
        if byte_size <= 0:
            raise ValueError(f"allocation must be positive, got {byte_size}")
        registration_bytes = align_up_2m(byte_size)
        allocation_bytes = hixl_allocation_bytes(byte_size)
        host_pointer = ctypes.c_void_p()
        status = int(
            self._runtime.lib.aclrtMallocHost(
                ctypes.byref(host_pointer), ctypes.c_size_t(allocation_bytes)
            )
        )
        if status != 0 or not host_pointer.value:
            if host_pointer.value:
                self._uncertain_allocations.append(host_pointer.value)
            raise SourceUnavailableError(
                f"aclrtMallocHost of {byte_size} bytes failed with status {status}"
            )
        address = align_up_2m(host_pointer.value)
        block = AclBlock(name, byte_size, self, address)
        block.allocation_address = host_pointer.value
        block.allocation_bytes = allocation_bytes
        block.registration_bytes = registration_bytes
        if registration_bytes > byte_size:
            ctypes.memset(address + byte_size, 0, registration_bytes - byte_size)
        return block

    def release(self, block) -> None:
        if block.address == 0:
            return
        status = int(
            self._runtime.lib.aclrtFreeHost(ctypes.c_void_p(block.allocation_address))
        )
        if status != 0:
            # A refused free leaves the allocation owned: the address
            # stays on the block and the runtime reference stays held,
            # exactly like a refused native deregistration.
            raise SourceUnavailableError(
                f"aclrtFreeHost refused with status {status}; the "
                "allocation stays owned"
            )
        block.address = 0

    def dispose(self) -> None:
        _release_acl_runtime(self._runtime)
        self._runtime = None


class AclBlock(HostBlock):
    def __init__(self, name: str, byte_size: int, owner, address: int):
        super().__init__(name, byte_size, owner)
        self.address = address
        self.allocation_address = address
        self.allocation_bytes = byte_size
        self.registration_bytes = byte_size

    def data_ptr(self) -> int:
        if self.address == 0:
            raise RuntimeError("this pinned block was already released")
        return self.address

    def write_bytes(self, offset: int, data) -> None:
        if self.address == 0:
            raise RuntimeError("this pinned block was already released")
        if offset < 0 or offset + len(data) > self.byte_size:
            raise ValueError(
                f"write [{offset}, {offset + len(data)}) exceeds block {self.byte_size}"
            )
        ctypes.memmove(self.address + offset, data, len(data))

    def read_bytes(self, offset: int, length: int) -> bytes:
        if self.address == 0:
            raise RuntimeError("this pinned block was already released")
        return ctypes.string_at(self.address + offset, length)


class _AclRuntime:
    """One process-wide ACL runtime: lib handle, device, context, refs."""

    def __init__(self, lib, device_id: int):
        self.lib = lib
        self.device_id = device_id
        self.references = 1
        self.context = None
        self.device_set = False
        self.broken = False
        status = int(lib.aclInit(None))
        if status != 0:
            raise SourceUnavailableError(f"aclInit failed with status {status}")
        status = int(lib.aclrtSetDevice(device_id))
        if status != 0:
            raise SourceUnavailableError(
                f"aclrtSetDevice({device_id}) failed with status {status}"
            )
        self.device_set = True
        context = ctypes.c_void_p()
        status = int(lib.aclrtCreateContext(ctypes.byref(context), device_id))
        if status != 0:
            raise SourceUnavailableError(
                f"aclrtCreateContext on device {device_id} failed with status {status}"
            )
        self.context = context.value

    def unref(self) -> None:
        """Drop one reference; tear the runtime down only at zero.

        The teardown runs in the reverse of setup — destroy context, reset
        device, finalize — and every step's refusal leaves the runtime
        alive and referenced, because finalizing a runtime something may
        still use is exactly the lifetime bug this class exists to prevent.
        """
        self.references -= 1
        if self.references > 0:
            return
        failures = []
        if self.context is not None:
            if int(self.lib.aclrtDestroyContext(ctypes.c_void_p(self.context))) != 0:
                failures.append("aclrtDestroyContext")
            else:
                self.context = None
        if not failures and self.device_set:
            if int(self.lib.aclrtResetDevice(self.device_id)) != 0:
                failures.append("aclrtResetDevice")
            else:
                self.device_set = False
        if not failures and int(self.lib.aclFinalize()) != 0:
            failures.append("aclFinalize")
        if failures:
            # Keep the runtime referenced: a half-torn-down ACL process
            # state must never be handed to the next generation.
            self.references = 1
            self.broken = True
            raise SourceUnavailableError("ACL teardown refused: " + ", ".join(failures))


def _acquire_acl_runtime(device_id: int, loader=None) -> _AclRuntime:
    global _ACL_RUNTIME, _ACL_BROKEN
    with _ACL_LOCK:
        if _ACL_BROKEN is not None:
            raise SourceUnavailableError("ACL runtime setup is quarantined")
        if _ACL_RUNTIME is not None:
            if _ACL_RUNTIME.broken:
                raise SourceUnavailableError("ACL runtime teardown is quarantined")
            if _ACL_RUNTIME.device_id != device_id:
                raise SourceUnavailableError(
                    f"this process already runs ACL on device "
                    f"{_ACL_RUNTIME.device_id}; device {device_id} needs "
                    "its own source process"
                )
            _ACL_RUNTIME.references += 1
            return _ACL_RUNTIME
        if loader is None:
            try:
                lib = ctypes.CDLL("libascendcl.so")
            except OSError as exc:
                raise SourceUnavailableError(
                    "the Ascend source needs the ACL runtime "
                    f"(libascendcl.so) and it is unavailable: {exc}"
                ) from None
        else:
            lib = loader("libascendcl.so")
        lib.aclInit.argtypes = [ctypes.c_char_p]
        lib.aclInit.restype = ctypes.c_int
        lib.aclrtSetDevice.argtypes = [ctypes.c_int]
        lib.aclrtSetDevice.restype = ctypes.c_int
        lib.aclrtCreateContext.argtypes = [
            ctypes.POINTER(ctypes.c_void_p),
            ctypes.c_int,
        ]
        lib.aclrtCreateContext.restype = ctypes.c_int
        lib.aclrtSetCurrentContext.argtypes = [ctypes.c_void_p]
        lib.aclrtSetCurrentContext.restype = ctypes.c_int
        lib.aclrtDestroyContext.argtypes = [ctypes.c_void_p]
        lib.aclrtDestroyContext.restype = ctypes.c_int
        lib.aclrtResetDevice.argtypes = [ctypes.c_int]
        lib.aclrtResetDevice.restype = ctypes.c_int
        lib.aclFinalize.argtypes = []
        lib.aclFinalize.restype = ctypes.c_int
        lib.aclrtMallocHost.argtypes = [
            ctypes.POINTER(ctypes.c_void_p),
            ctypes.c_size_t,
        ]
        lib.aclrtMallocHost.restype = ctypes.c_int
        lib.aclrtFreeHost.argtypes = [ctypes.c_void_p]
        lib.aclrtFreeHost.restype = ctypes.c_int
        try:
            _ACL_RUNTIME = _AclRuntime(lib, device_id)
        except BaseException:
            _ACL_BROKEN = lib
            _ACL_RUNTIME = None
            raise
        return _ACL_RUNTIME


def _release_acl_runtime(runtime: _AclRuntime) -> None:
    global _ACL_RUNTIME
    with _ACL_LOCK:
        runtime.unref()
        if runtime.references <= 0:
            _ACL_RUNTIME = None


# The smallest source buffer this node still serves: below it the dynamic
# window refuses, and a client falls back to its local or peer-to-peer path.
MIN_SOURCE_BUFFER_BYTES = 1 << 30

_DEFAULT_MEMINFO_PATH = "/proc/meminfo"


def _meminfo_field_bytes(path: str, field: str) -> int | None:
    """Read one kB figure out of a meminfo-format file, in bytes; None when
    the file, the field, or the figure is unusable."""
    try:
        with open(path, encoding="ascii", errors="replace") as handle:
            for line in handle:
                if not line.startswith(field):
                    continue
                parts = line[len(field) :].split()
                if len(parts) != 2 or parts[1] != "kB":
                    return None
                kb = int(parts[0])
                if kb <= 0:
                    return None
                return kb * 1024
    except OSError:
        return None
    return None


def total_ram(path: str | None = None) -> int | None:
    """Read the node's total RAM, in bytes; None when unusable."""
    return _meminfo_field_bytes(path or _DEFAULT_MEMINFO_PATH, "MemTotal:")


def available_ram(path: str | None = None) -> int | None:
    """Read the node's available RAM, in bytes; None when unusable."""
    return _meminfo_field_bytes(path or _DEFAULT_MEMINFO_PATH, "MemAvailable:")


def _has_quarter_headroom(avail: int, window: int) -> bool:
    if avail <= window:
        return False
    return avail - window > window // 4


def compute_dynamic_source_buffer(
    avail_ram: int | None, max_memory_bytes: int, min_memory_bytes: int
) -> tuple[int, bool]:
    """Resolve the transfer buffer ceiling this node can offer right now out
    of its available RAM, mirroring the broker's Go answer.

    The ceiling starts at max_memory_bytes, floored at min_memory_bytes,
    which is itself floored at 1 GiB. More than a quarter of the ceiling
    free above it keeps the ceiling. Otherwise it shrinks by a quarter per
    step until either the headroom recovers or the available RAM at least
    clears the step plain; the floor caps the shrink. When even the floor
    does not fit — or the available RAM cannot be read — the window is
    refused and the caller declines to build an RDMA transfer.
    """
    min_memory_bytes = align_down_2m(min_memory_bytes)
    if min_memory_bytes < MIN_SOURCE_BUFFER_BYTES:
        min_memory_bytes = MIN_SOURCE_BUFFER_BYTES
    max_memory_bytes = align_down_2m(max_memory_bytes)
    if max_memory_bytes < min_memory_bytes:
        max_memory_bytes = min_memory_bytes
    if avail_ram is None or avail_ram <= 0:
        return 0, False
    if avail_ram <= min_memory_bytes:
        return 0, False
    if _has_quarter_headroom(avail_ram, max_memory_bytes):
        return max_memory_bytes, True
    new_max = max_memory_bytes
    while new_max > min_memory_bytes:
        new_max = max(align_down_2m(new_max * 3 // 4), min_memory_bytes)
        if _has_quarter_headroom(avail_ram, new_max) or avail_ram > new_max:
            break
    if avail_ram <= new_max:
        return 0, False
    return new_max, True


__all__ = [
    "MIN_SOURCE_BUFFER_BYTES",
    "_FILL_CHUNK_BYTES",
    "AclBlock",
    "AclHostOwner",
    "HostBlock",
    "TorchBlock",
    "TorchPinnedOwner",
    "available_ram",
    "compute_dynamic_source_buffer",
    "total_ram",
]
