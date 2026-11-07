"""Target-owned staging memory; vendor imports occur only after session open."""

import base64
import ctypes
import os
import socket
import threading
import uuid
from types import SimpleNamespace

from gpustack_model_transfer.core.budgets import (
    MAX_WINDOW_BYTES,
    MEMORY_ALIGNMENT_BYTES,
    align_down_2m,
    align_up_2m,
    hixl_allocation_bytes,
)
from gpustack_model_transfer.core.errors import SourceUnavailableError
from gpustack_model_transfer.source.memory import AclBlock, TorchPinnedOwner

MAX_WINDOW = MAX_WINDOW_BYTES


class BorrowedAclOwner:
    """Host allocations under the calling engine's current ACL context."""

    def __init__(self, loader=None):
        loader = loader or ctypes.CDLL
        try:
            self.lib = loader("libascendcl.so")
            self.lib.aclrtGetCurrentContext.argtypes = [ctypes.POINTER(ctypes.c_void_p)]
            self.lib.aclrtGetCurrentContext.restype = ctypes.c_int
            self.lib.aclrtMallocHost.argtypes = [
                ctypes.POINTER(ctypes.c_void_p),
                ctypes.c_size_t,
            ]
            self.lib.aclrtMallocHost.restype = ctypes.c_int
            self.lib.aclrtFreeHost.argtypes = [ctypes.c_void_p]
            self.lib.aclrtFreeHost.restype = ctypes.c_int
            context = ctypes.c_void_p()
            if (
                int(self.lib.aclrtGetCurrentContext(ctypes.byref(context))) != 0
                or not context.value
            ):
                raise SourceUnavailableError("engine ACL context is unavailable")
            self.context = context.value
            self.uncertain_allocations = []
        except (OSError, AttributeError):
            raise SourceUnavailableError("engine ACL runtime unavailable") from None

    def ensure_current(self):
        context = ctypes.c_void_p()
        if (
            int(self.lib.aclrtGetCurrentContext(ctypes.byref(context))) != 0
            or context.value != self.context
        ):
            raise SourceUnavailableError("engine ACL context changed")

    def allocate(self, name, size):
        self.ensure_current()
        registration_bytes = align_up_2m(size)
        allocation_bytes = hixl_allocation_bytes(size)
        pointer = ctypes.c_void_p()
        status = int(
            self.lib.aclrtMallocHost(
                ctypes.byref(pointer), ctypes.c_size_t(allocation_bytes)
            )
        )
        if status != 0 or not pointer.value:
            if pointer.value:
                self.uncertain_allocations.append(pointer.value)
            raise SourceUnavailableError("ACL host allocation failed")
        block = AclBlock(name, size, self, align_up_2m(pointer.value))
        block.allocation_address = pointer.value
        block.allocation_bytes = allocation_bytes
        block.registration_bytes = registration_bytes
        if registration_bytes > size:
            ctypes.memset(block.address + size, 0, registration_bytes - size)
        return block

    def release(self, block):
        self.ensure_current()
        if (
            block.address
            and int(self.lib.aclrtFreeHost(ctypes.c_void_p(block.allocation_address)))
            != 0
        ):
            raise SourceUnavailableError("ACL host free outcome unknown")
        block.address = 0


class TargetNative:
    """One reusable registered window in pinned host memory, with strong
    ownership of all state.
    """

    supports_window_offset = True
    requires_synchronization = True
    registration_alignment_bytes = MEMORY_ALIGNMENT_BYTES

    def __init__(self, backend, pod_ip, size=MAX_WINDOW):
        if (
            backend not in ("nixl", "hixl")
            or isinstance(size, bool)
            or not isinstance(size, int)
            or not 0 < size <= MAX_WINDOW
        ):
            raise SourceUnavailableError("invalid target backend or window size")
        if size >= MEMORY_ALIGNMENT_BYTES:
            size = align_down_2m(size)
        if backend == "hixl":
            # One aligned region plus address padding must fit the slot budget.
            size = max(size, 2 * MEMORY_ALIGNMENT_BYTES) - MEMORY_ALIGNMENT_BYTES
        self.backend = backend
        self.pod_ip = pod_ip
        self.size = size
        self.owner = None
        self.block = None
        self.api = None
        self.agent = None
        self.registration = None
        self.registration_id = uuid.uuid4().hex
        self.window_id = uuid.uuid4().hex
        self.endpoint = ""
        self.metadata = ""
        self.closed = False
        self.thread_id = None
        self._pending_devices = {}

    def _check_thread(self):
        if self.thread_id != threading.get_ident():
            raise SourceUnavailableError("native target called from a different thread")
        if self.backend == "hixl" and self.owner is not None:
            self.owner.ensure_current()

    def _initialize_endpoint(self):
        """Bind a loopback endpoint and initialize the HIXL target on it.

        Sibling TP ranks in one Pod pick ephemeral ports at the same
        moment, so the picked port can be re-taken before initialize
        binds it; each retry picks a fresh port, and a run that keeps
        losing the race is an error, not a hang.
        """
        hixl = self.api._require()
        last_error = None
        for _ in range(3):
            with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as probe:
                probe.bind((self.pod_ip, 0))
                self.endpoint = f"{self.pod_ip}:{probe.getsockname()[1]}"
            self.agent = SimpleNamespace(client=hixl.Hixl())
            status = self.agent.client.initialize(self.endpoint, {"BufferPool": "0:0"})
            if int(status) == int(hixl.SUCCESS):
                return
            last_error = int(status)
        raise SourceUnavailableError(
            f"HIXL target initialization outcome unknown (last status {last_error})"
        )

    def start(self):
        self.thread_id = threading.get_ident()
        if self.backend == "nixl":
            from gpustack_model_transfer.backends.nixl.runtime import load_runtime

            rdma_devices = os.environ.get("GPUSTACK_RDMA_DEVICES", "").strip()
            if rdma_devices and not os.environ.get("UCX_NET_DEVICES", "").strip():
                ucx_devs = [
                    dev.strip() if ":" in dev else f"{dev.strip()}:1"
                    for dev in rdma_devices.split(",")
                    if dev.strip()
                ]
                if ucx_devs:
                    os.environ["UCX_NET_DEVICES"] = ",".join(ucx_devs)

            self.owner = TorchPinnedOwner()
            self.block = self.owner.allocate("window", self.size)
            self.api = load_runtime()
            self.agent = self.api.create_agent(
                "model-transfer-target-" + uuid.uuid4().hex,
                {"enable_listen_thread": False},
            )
            self.registration = self.api.register_memory(
                self.agent, {"window": self.block}
            )
            # Export target metadata only. Source metadata is never requested.
            self.metadata = base64.b64encode(
                self.api.agent_metadata(self.agent)
            ).decode("ascii")
        else:
            from gpustack_model_transfer.backends.hixl.runtime import load_runtime

            self.owner = BorrowedAclOwner()
            self.block = self.owner.allocate("window", self.size)
            self.api = load_runtime()
            self._initialize_endpoint()
            hixl = self.api._require()
            self.registration = SimpleNamespace(
                handles={}, windows={}, buffers={"window": self.block}
            )
            status, handle = self.agent.client.register_mem(
                hixl.MemDesc(self.block.data_ptr(), self.block.registration_bytes),
                hixl.MemType.MEM_HOST,
            )
            if handle:
                self.registration.handles["window"] = handle
            if int(status) != int(hixl.SUCCESS) or not handle:
                raise SourceUnavailableError("HIXL target registration outcome unknown")
            self.registration.windows["window"] = {
                "addr": self.block.data_ptr(),
                "len": self.size,
                "type": "HOST",
            }
        return self

    def descriptor(self):
        self._check_thread()
        if self.registration is None or self.block is None:
            raise SourceUnavailableError("target registration unavailable")
        return {
            "backend": self.backend,
            "address": self.block.data_ptr(),
            "length": self.size,
            "metadata": self.metadata,
            "endpoint": self.endpoint,
        }

    def copy_to(self, target, offset, length, window_offset=0):
        """Copy on the caller's stream; synchronize before reusing host bytes.

        CUDA consumers on that stream may immediately use the target. Other
        streams need the caller's usual stream dependency. Borrowed ACL memory
        keeps the synchronous NPU path until its stream contract is qualified.
        """
        self._check_thread()
        import torch

        device = target.device
        if (
            device.type not in ("cuda", "hip", "npu", "privateuseone", "cpu")
            or not target.is_contiguous()
        ):
            raise SourceUnavailableError("target must be a contiguous supported tensor")
        target_bytes = target.view(torch.uint8).reshape(-1)
        if (
            offset < 0
            or window_offset < 0
            or length <= 0
            or offset + length > target_bytes.numel()
            or window_offset + length > self.size
        ):
            raise SourceUnavailableError("target tensor range invalid")
        host_tensor = getattr(self.block, "tensor", None)
        if host_tensor is not None:
            src = host_tensor[window_offset : window_offset + length]
        else:
            src = torch.frombuffer(
                self.block.view(window_offset, length), dtype=torch.uint8
            )
        # Record ownership before enqueueing: even a failed copy may have work
        # in flight. Retain every destination device until a successful fence.
        if device.type != "cpu":
            self._pending_devices[(device.type, getattr(device, "index", None))] = (
                device
            )
        target_bytes[offset : offset + length].copy_(src, non_blocking=True)
        if device.type in ("npu", "privateuseone"):
            self.synchronize(target)

    def synchronize(self, target):
        """Fence all devices that may still read this window, on its owner thread."""
        self._check_thread()
        devices = dict(self._pending_devices)
        device = target.device
        devices[(device.type, getattr(device, "index", None))] = device
        for key, pending in devices.items():
            self._synchronize_device(pending)
            self._pending_devices.pop(key, None)

    def _synchronize_device(self, device):
        import torch

        if device.type in ("cuda", "hip") and hasattr(torch, "cuda"):
            torch.cuda.synchronize(device)
        elif device.type == "hip" and hasattr(torch, "hip"):
            torch.hip.synchronize(device)
        elif device.type in ("npu", "privateuseone") and hasattr(torch, "npu"):
            torch.npu.synchronize(device)
        elif device.type == "cpu":
            pass
        else:
            raise SourceUnavailableError("target device is not supported")

    def close(self):
        if self.closed:
            return
        self._check_thread()
        for key, device in list(self._pending_devices.items()):
            self._synchronize_device(device)
            del self._pending_devices[key]
        if self.registration is not None:
            if self.backend == "nixl":
                self.api.revoke_registration(self.agent, self.registration)
            else:
                self.api.deregister(self.agent, self.registration)
        if self.backend == "hixl" and self.agent is not None:
            result = self.agent.client.finalize()
            if result is not None and int(result) != 0:
                raise SourceUnavailableError("HIXL finalize outcome unknown")
        if self.block is not None:
            self.owner.release(self.block)
        self.closed = True
