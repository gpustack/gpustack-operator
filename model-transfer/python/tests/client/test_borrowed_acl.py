"""Borrowed engine ACL ownership never initializes or resets the runtime."""

import ctypes

import pytest

from gpustack_model_transfer.client_native import BorrowedAclOwner
from gpustack_model_transfer.core.errors import SourceUnavailableError


class Function:
    def __init__(self, fn):
        self.fn = fn

    def __call__(self, *args):
        return self.fn(*args)


class ACL:
    def __init__(self):
        self.context = 123
        self.calls = []
        self.allocations = {}
        self.aclrtGetCurrentContext = Function(self.current)
        self.aclrtMallocHost = Function(self.allocate)
        self.aclrtFreeHost = Function(self.free)

    def current(self, output):
        self.calls.append("current")
        ctypes.cast(output, ctypes.POINTER(ctypes.c_void_p))[0] = ctypes.c_void_p(
            self.context
        )
        return 0

    def allocate(self, output, length):
        self.calls.append("allocate")
        raw = ctypes.create_string_buffer(length.value)
        address = ctypes.addressof(raw)
        self.allocations[address] = raw
        ctypes.cast(output, ctypes.POINTER(ctypes.c_void_p))[0] = ctypes.c_void_p(
            address
        )
        return 0

    def free(self, address):
        self.calls.append("free")
        del self.allocations[address.value]
        return 0


def test_borrowed_acl_rejects_changed_context_and_never_resets():
    acl = ACL()
    owner = BorrowedAclOwner(loader=lambda _: acl)
    block = owner.allocate("window", 8)
    block.view(0, 8)[:] = b"payload!"
    acl.context = 456
    with pytest.raises(SourceUnavailableError):
        owner.release(block)
    assert block.address and len(acl.allocations) == 1
    acl.context = 123
    owner.release(block)
    assert not block.address and not acl.allocations
    assert "free" in acl.calls
    assert not any(name in acl.calls for name in ("aclInit", "reset", "finalize"))


def test_failed_acl_allocation_retains_any_returned_pointer():
    acl = ACL()
    original = acl.allocate

    def fail_after_alloc(output, length):
        original(output, length)
        return 1

    acl.aclrtMallocHost = Function(fail_after_alloc)
    owner = BorrowedAclOwner(loader=lambda _: acl)
    with pytest.raises(SourceUnavailableError):
        owner.allocate("window", 8)
    assert len(owner.uncertain_allocations) == 1
    assert owner.uncertain_allocations[0] in acl.allocations


def test_borrowed_acl_releases_mmap_block_after_context_change(monkeypatch):
    monkeypatch.setenv(
        "ASCEND_GLOBAL_RESOURCE_CONFIG",
        '{"comm_resource_config.protocol_desc":["roce:host"]}',
    )
    acl = ACL()
    owner = BorrowedAclOwner(loader=lambda _: acl)
    block = owner.allocate("window", 4096)
    mapping = block.mapping
    assert block.allocator_kind == "mmap"
    assert mapping is not None and not mapping.closed

    acl.context = 456
    with pytest.raises(SourceUnavailableError):
        owner.ensure_current()

    owner.release(block)
    assert mapping.closed
    assert block.address == 0
    assert "free" not in acl.calls


def test_target_native_close_releases_mmap_tail_after_acl_context_change(monkeypatch):
    import threading
    from types import SimpleNamespace

    from gpustack_model_transfer.client_native import TargetNative

    monkeypatch.setenv(
        "ASCEND_GLOBAL_RESOURCE_CONFIG",
        '{"comm_resource_config.protocol_desc":["roce:host"]}',
    )
    acl = ACL()
    owner = BorrowedAclOwner(loader=lambda _: acl)
    block = owner.allocate("window", 4096)
    mapping = block.mapping
    assert block.allocator_kind == "mmap"
    assert mapping is not None and not mapping.closed

    target = TargetNative("hixl", "127.0.0.1", 4 << 20)
    target.thread_id = threading.get_ident()
    target.owner = owner
    target.block = block

    acl.context = 456
    with pytest.raises(SourceUnavailableError):
        owner.ensure_current()

    # Cross-thread close is still rejected.
    cross_thread_err = []

    def call_from_other_thread():
        try:
            target.close()
        except SourceUnavailableError as exc:
            cross_thread_err.append(str(exc))

    other = threading.Thread(target=call_from_other_thread)
    other.start()
    other.join(timeout=2)
    assert cross_thread_err == ["native target called from a different thread"]
    assert not mapping.closed
    assert not target.closed

    # If native HIXL agent state still remains, switched ACL context is still refused.
    target.agent = SimpleNamespace(client=SimpleNamespace(finalize=lambda: 0))
    with pytest.raises(SourceUnavailableError, match="engine ACL context changed"):
        target.close()
    assert not mapping.closed
    assert not target.closed

    # Once pending devices, registration, and agent are clear, mmap-only tail closes cleanly.
    target.agent = None
    target.close()
    assert target.closed is True
    assert mapping.closed
    assert block.address == 0
    assert "free" not in acl.calls
