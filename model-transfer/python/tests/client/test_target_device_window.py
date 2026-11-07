"""Target window tests: 2 MiB alignment for size and address, HIXL endpoint
port retry, and session native creation, all against fakes — no accelerator
is needed."""

import sys
from types import SimpleNamespace

import pytest

import gpustack_model_transfer.client_native as client_native
from gpustack_model_transfer.client import EngineSession
from gpustack_model_transfer.client_native import TargetNative
from gpustack_model_transfer.core.budgets import MEMORY_ALIGNMENT_BYTES
from gpustack_model_transfer.core.errors import SourceUnavailableError
from gpustack_model_transfer.source.memory import AclBlock, TorchPinnedOwner


class FakeBorrowedAclOwner:
    def ensure_current(self):
        pass

    def allocate(self, name, size):
        return AclBlock(name, size, self, 16 << 20)

    def release(self, block):
        block.address = 0


class FakeHixlRuntime:
    """The HIXL runtime surface the target native touches, with counters."""

    def __init__(self, initialize_failures=0):
        self.SUCCESS = 0
        self.MemType = SimpleNamespace(MEM_HOST="HOST", MEM_DEVICE="DEVICE")
        self.MemDesc = lambda address, size: (address, size)
        self.initialize_failures = initialize_failures
        self.initialize_attempts = []
        self.registrations = []
        self.deregistered = []
        self.finalized = 0

    def _require(self):
        return self

    def deregister(self, agent, registration):
        self.deregistered.append(registration)
        return self.SUCCESS

    def Hixl(self):
        runtime = self

        class Client:
            def initialize(self, endpoint, options):
                runtime.initialize_attempts.append(endpoint)
                if len(runtime.initialize_attempts) <= runtime.initialize_failures:
                    return 5
                return runtime.SUCCESS

            def register_mem(self, descriptor, mem_type):
                runtime.registrations.append((descriptor, mem_type))
                return runtime.SUCCESS, f"handle-{len(runtime.registrations)}"

            def deregister(self, registration):
                runtime.deregistered.append(registration)
                return runtime.SUCCESS

            def finalize(self):
                runtime.finalized += 1
                return runtime.SUCCESS

        return Client()


def _install_hixl_runtime(monkeypatch, runtime):
    import gpustack_model_transfer.backends.hixl.runtime as hixl_runtime

    monkeypatch.setattr(hixl_runtime, "load_runtime", lambda: runtime)
    monkeypatch.setattr(client_native, "BorrowedAclOwner", FakeBorrowedAclOwner)


@pytest.mark.parametrize("error", [None, PermissionError, ValueError])
def test_memlock_raise_is_best_effort_before_native_start(monkeypatch, error):
    runtime = FakeHixlRuntime()
    _install_hixl_runtime(monkeypatch, runtime)
    native = TargetNative("hixl", "127.0.0.1", 4 << 20)
    calls = []

    def setrlimit(kind, limits):
        assert native.block is None
        assert not runtime.initialize_attempts
        calls.append((kind, limits))
        if error is not None:
            raise error("cannot raise memlock")

    resource = SimpleNamespace(RLIMIT_MEMLOCK=8, RLIM_INFINITY=-1, setrlimit=setrlimit)
    monkeypatch.setitem(sys.modules, "resource", resource)
    try:
        assert native.start() is native
        assert calls == [(resource.RLIMIT_MEMLOCK, (-1, -1))]
        assert len(runtime.registrations) == 1
    finally:
        native.close()


def test_hixl_endpoint_initialization_retries_a_lost_port_race(monkeypatch):
    runtime = FakeHixlRuntime(initialize_failures=2)
    _install_hixl_runtime(monkeypatch, runtime)
    native = TargetNative("hixl", "127.0.0.1", 4 << 20)
    native.start()
    assert len(runtime.initialize_attempts) == 3, "two lost races, third port wins"
    assert len(set(runtime.initialize_attempts)) == 3, "each retry picks a fresh port"
    native.close()


def test_hixl_endpoint_initialization_gives_up_after_three_attempts(monkeypatch):
    runtime = FakeHixlRuntime(initialize_failures=99)
    _install_hixl_runtime(monkeypatch, runtime)
    native = TargetNative("hixl", "127.0.0.1", 4 << 20)
    with pytest.raises(SourceUnavailableError):
        native.start()
    assert len(runtime.initialize_attempts) == 3


def test_target_native_aligns_size_to_2_mib(monkeypatch):
    runtime = FakeHixlRuntime()
    _install_hixl_runtime(monkeypatch, runtime)
    # 3996241408 is not 2 MiB aligned; TargetNative aligns size down to 2 MiB.
    native = TargetNative("hixl", "127.0.0.1", 3996241408)
    assert native.size % MEMORY_ALIGNMENT_BYTES == 0
    assert (
        native.size + MEMORY_ALIGNMENT_BYTES
        == (3996241408 // MEMORY_ALIGNMENT_BYTES) * MEMORY_ALIGNMENT_BYTES
    )
    native.start()
    descriptor, mem_type = runtime.registrations[0]
    assert mem_type == "HOST"
    assert descriptor[0] % MEMORY_ALIGNMENT_BYTES == 0
    assert descriptor[1] == native.size
    assert descriptor[1] % MEMORY_ALIGNMENT_BYTES == 0
    native.close()


def test_torch_pinned_owner_aligns_address_to_2_mib(monkeypatch):
    class SliceableFakeHostTensor:
        def __init__(self, size, base_ptr):
            self.size = size
            self._base_ptr = base_ptr
            self._numpy = bytearray(size)

        def data_ptr(self):
            return self._base_ptr

        def is_pinned(self):
            return True

        def is_contiguous(self):
            return True

        def numpy(self):
            return self._numpy

        def __getitem__(self, item):
            assert isinstance(item, slice)
            view = SliceableFakeHostTensor(
                item.stop - item.start, self._base_ptr + item.start
            )
            view._numpy = memoryview(self._numpy)[item.start : item.stop]
            return view

    class UnalignedPinnedTorch:
        uint8 = "uint8"

        def empty(self, size, dtype=None, pin_memory=False):
            assert pin_memory is True
            # Simulate an unaligned pinned host pointer offset by 512 bytes.
            return SliceableFakeHostTensor(size, (16 << 20) + 512)

    monkeypatch.setitem(sys.modules, "torch", UnalignedPinnedTorch())
    owner = TorchPinnedOwner()
    block = owner.allocate("w", 4 << 20)
    assert block.byte_size == 4 << 20
    assert block.data_ptr() % MEMORY_ALIGNMENT_BYTES == 0


class RecordingFactory:
    """Records every native the session asks for and answers with fakes."""

    def __init__(self, start_error=None):
        self.calls = []
        self.start_error = start_error
        self.supports_window_offset = True

    def __call__(self, backend, pod_ip, size):
        self.calls.append((backend, pod_ip, size))
        factory = self

        class FakeNative:
            def __init__(self_inner):
                self_inner.size = size
                self_inner.registration_id = "registration"
                self_inner.window_id = "window"

            def start(self_inner):
                if factory.start_error is not None:
                    raise factory.start_error

            def close(self_inner):
                pass

        return FakeNative()


def _session(env, factory, max_tensor=1 << 20, window=4 << 30):
    session = EngineSession.__new__(EngineSession)
    session.reader = SimpleNamespace(
        env=env,
        backend="hixl",
        pod_ip="127.0.0.1",
        native_factory=factory,
    )
    session._max_tensor_bytes = max_tensor
    session.window_bytes = window
    return session


def test_session_creates_host_window_with_aligned_window_bytes():
    factory = RecordingFactory()
    session = _session({}, factory)
    native = session._create_native()
    assert len(factory.calls) == 1
    backend, pod_ip, size = factory.calls[0]
    assert (backend, pod_ip, size) == ("hixl", "127.0.0.1", 4 << 30)
    assert native.size == 4 << 30
