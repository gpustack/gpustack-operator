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
