"""CPU checks for bounded HIXL registration padding, not native acceptance."""

import ctypes
from types import SimpleNamespace

import pytest

from gpustack_model_transfer.client_native import BorrowedAclOwner, TargetNative
from gpustack_model_transfer.core.budgets import align_down_2m
from gpustack_model_transfer.source import memory
from gpustack_model_transfer.source.backend import HixlSourceBackend
from gpustack_model_transfer.source.fill import resident_files
from gpustack_model_transfer.source.workorder import resident_budget


class Runtime:
    def __init__(self):
        self.storage = {}
        self.context = 1
        self.lib = SimpleNamespace(
            aclrtSetCurrentContext=lambda _: 0,
            aclrtMallocHost=self.allocate,
            aclrtFreeHost=self.free,
        )

    def allocate(self, pointer, size):
        data = (ctypes.c_ubyte * size.value)()
        ctypes.memset(ctypes.addressof(data), 0xA5, size.value)
        self.storage[ctypes.addressof(data)] = data
        ctypes.cast(pointer, ctypes.POINTER(ctypes.c_void_p))[0] = ctypes.addressof(
            data
        )
        return 0

    def free(self, pointer):
        del self.storage[pointer.value]
        return 0


@pytest.mark.parametrize(
    ("size", "padded"),
    [
        (22 << 20, 22 << 20),
        ((22 << 20) + 1, 24 << 20),
        ((2 << 20) + 1, 4 << 20),
        (1, 2 << 20),
    ],
)
def test_registration_padding_preserves_file_extent(monkeypatch, size, padded):
    runtime = Runtime()
    monkeypatch.setattr(memory, "_acquire_acl_runtime", lambda *_: runtime)
    monkeypatch.setattr(memory, "_release_acl_runtime", lambda _: None)
    owner = memory.AclHostOwner()
    block = owner.allocate("weights", size)
    descriptors = []
    backend = HixlSourceBackend()
    backend._engine = SimpleNamespace(
        register_mem=lambda desc, _: (descriptors.append(desc) or 0, 1)
    )
    backend._hixl = SimpleNamespace(
        MemDesc=lambda address, length: SimpleNamespace(addr=address, len=length),
        MemType=SimpleNamespace(MEM_HOST=1),
        SUCCESS=0,
    )
    try:
        registration = backend.register({"weights": block})
        assert len(runtime.storage[block.allocation_address]) == padded + (2 << 20)
        assert block.data_ptr() % (2 << 20) == 0
        assert block.allocation_address <= block.data_ptr()
        assert (
            block.data_ptr() + padded
            <= block.allocation_address + block.allocation_bytes
        )
        assert block.byte_size == size
        assert descriptors[0].len == padded
        assert descriptors[0].local_only is True
        assert registration.descriptors("worker")[0]["length"] == size
        assert ctypes.string_at(block.data_ptr() + size, padded - size) == bytes(
            padded - size
        )
        with pytest.raises(ValueError):
            block.write_bytes(size, b"x")
    finally:
        owner.release(block)
        owner.dispose()
    assert not runtime.storage


@pytest.mark.parametrize("size", [1, (2 << 20) + 1, (4 << 20) + 123])
def test_reference_window_registers_owned_aligned_extent(size):
    from gpustack_model_transfer.backends.hixl.reader import TransferWindow
    from gpustack_model_transfer.backends.hixl.runtime import RealHixlAPI

    window = TransferWindow("window", size)
    calls = []
    api = RealHixlAPI.__new__(RealHixlAPI)
    api._hixl = SimpleNamespace(
        MemDesc=lambda address, length: (address, length),
        MemType=SimpleNamespace(MEM_HOST=1, MEM_DEVICE=2),
        SUCCESS=0,
    )
    agent = SimpleNamespace(
        client=SimpleNamespace(
            register_mem=lambda desc, kind: (calls.append(desc) or 0, 1)
        )
    )
    registration = api.register_memory(agent, {"window": window})
    address, length = calls[0]
    assert address % (2 << 20) == length % (2 << 20) == 0
    assert registration.windows["window"]["len"] == size
    assert window.allocation_address <= address
    assert address + length <= window.allocation_address + window.allocation_bytes
    assert ctypes.string_at(address + size, length - size) == bytes(length - size)


@pytest.mark.parametrize("failure", ["connect", "register"])
def test_reference_constructor_retains_unknown_registrations(failure):
    from gpustack_model_transfer.backends.hixl.memory import MemoryLedger
    from gpustack_model_transfer.backends.hixl.reader import HixlReader
    from gpustack_model_transfer.backends.hixl.runtime import (
        RealHixlAPI,
        RegistrationRetainedError,
        release_retained,
        retained_registrations,
    )
    from gpustack_model_transfer.core.budgets import ReadBudgets

    handles = []

    def register(desc, kind):
        if failure == "register" and handles:
            return 1, None
        handles.append(len(handles) + 1)
        return 0, handles[-1]

    api = RealHixlAPI.__new__(RealHixlAPI)
    api._hixl = SimpleNamespace(
        SUCCESS=0,
        MemDesc=lambda *args: args,
        MemType=SimpleNamespace(MEM_HOST=1, MEM_DEVICE=2),
    )
    agent = SimpleNamespace(
        client=SimpleNamespace(
            register_mem=register,
            deregister_mem=lambda _: 1,
            connect=lambda _: 1,
        )
    )
    api.create_agent = lambda *args: agent
    ledger = MemoryLedger()
    before = retained_registrations()
    try:
        with pytest.raises(RegistrationRetainedError):
            HixlReader(
                None, api, {"endpoint": "peer"}, ReadBudgets(32, 2, 4096), ledger=ledger
            )
        added = [item for item in retained_registrations() if item not in before]
        assert len(added) == 1
        assert set(added[0].handles.values()) == set(handles)
        assert len(added[0].buffers) >= len(handles)
        assert ledger.reserved_bytes == 8 << 20
        assert added[0].owners[0].ledger is ledger
    finally:
        for item in retained_registrations():
            if item not in before:
                release_retained(item)


@pytest.mark.parametrize("budget,files", [(6 << 20, 1), (12 << 20, 2)])
def test_paging_counts_backing_not_payload(budget, files):
    manifest = SimpleNamespace(
        tensors=[SimpleNamespace(file=name + ".safetensors") for name in ("a", "b")],
        files=[
            SimpleNamespace(path=name + ".safetensors", size=(2 << 20) + 1)
            for name in ("a", "b")
        ],
    )
    assert len(resident_files(manifest, budget, backend="hixl")) == files


@pytest.mark.parametrize("configured,want", [("1073741825", 1 << 30), ("1", 1 << 30)])
def test_source_budget_normalizes_user_values(monkeypatch, configured, want):
    monkeypatch.setenv(
        "GPUSTACK_MODEL_TRANSFER_SOURCE_RESIDENT_BUDGET_BYTES", configured
    )
    assert resident_budget(8 << 30, 8 << 30, "hixl") == want


@pytest.mark.parametrize(
    "budget", [1, (4 << 20) - 1, (4 << 20) + 1, (16 << 20) + 12345]
)
def test_target_backing_stays_inside_normalized_budget(budget):
    runtime = Runtime()
    owner = SimpleNamespace(
        lib=runtime.lib, ensure_current=lambda: None, uncertain_allocations=[]
    )
    native = TargetNative("hixl", "127.0.0.1", budget)
    block = BorrowedAclOwner.allocate(owner, "window", native.size)
    try:
        assert block.address % (2 << 20) == 0
        assert block.registration_bytes % (2 << 20) == 0
        assert block.allocation_bytes <= max(4 << 20, align_down_2m(budget))
        assert block.allocation_address <= block.address
        assert (
            block.address + block.registration_bytes
            <= block.allocation_address + block.allocation_bytes
        )
    finally:
        BorrowedAclOwner.release(owner, block)
    assert not runtime.storage
