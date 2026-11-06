"""Test HIXL source local-only registration with CPU fakes.

The flag excludes a region from the HIXL server export list. It does not
establish hardware isolation or hide the address in the filled report.

These tests run on the CPU with injected bindings. They are not a hardware
qualification: no pinned HIXL library is loaded here.
"""

import gc
import hashlib
import json
import logging
import weakref
from pathlib import Path
from types import SimpleNamespace

import pytest

from gpustack_model_transfer.source.backend import (
    HixlSourceBackend,
    SourceLocalOnlyRefusedError,
    SourceStateRetainedError,
)
from gpustack_model_transfer.source.native_worker import NativeSourceWorker
from gpustack_model_transfer.source.workorder import parse_assignment

FIXTURE = Path(__file__).resolve().parents[3] / "contracts/fixtures/tiny_dense"

# A pinned allocation's address is a large opaque value. The fake returns it
# as the handle, so a wrapper that truncates it or treats it as small fails
# here instead of in the cluster.
ADDRESS = 0x7F0000000000

# The binding's own text must never travel: it can hold a credential, a path
# or an address.
SECRET = "credential=hixl-token-9f3 path=/home/user/.hixl/token addr=0x7ffd12"


class Block:
    """A source block with its own address, like the pinned allocations."""

    def __init__(self, name, address, size):
        self.name = name
        self.address = address
        self.byte_size = size
        self.data = bytearray(size)

    def data_ptr(self):
        return self.address

    def view(self, offset, length):
        return memoryview(self.data)[offset : offset + length]

    def write_bytes(self, offset, data):
        self.data[offset : offset + len(data)] = data

    def read_bytes(self, offset, length):
        return bytes(self.data[offset : offset + length])


class MemDesc:
    """The pinned binding's MemDesc shape, including local_only."""

    def __init__(self, addr, length):
        self.addr = addr
        self.len = length
        self.local_only = False


class MemType:
    MEM_HOST = "MEM_HOST"


class Engine:
    """The pinned engine's shape: initialize plus register_mem.

    With local_only the pinned server returns the memory address as the
    handle, so the fake does the same. A descriptor without the flag gets the
    server-allocated counter handle the pinned server hands out.
    """

    def __init__(self):
        self.calls = []
        self.next_handle = 100

    def initialize(self, address, options):
        self.calls.append(("initialize", address, dict(options)))
        return 0

    def register_mem(self, desc, mem_type):
        self.calls.append(("register_mem", desc, mem_type))
        if getattr(desc, "local_only", None) is True:
            return (0, desc.addr)
        self.next_handle += 1
        return (0, self.next_handle)

    def registrations(self):
        return [call for call in self.calls if call[0] == "register_mem"]


def binding(engine, mem_desc=MemDesc):
    """The pinned module's shape around an engine."""
    return SimpleNamespace(
        Hixl=lambda: engine,
        SUCCESS=0,
        MemDesc=mem_desc,
        MemType=MemType,
    )


def source_backend(engine, mem_desc=MemDesc):
    """A source backend whose registration path is the vendor one.

    The binding is injected the way the existing client tests inject it,
    so the registration under test is the one production takes.
    """
    backend = HixlSourceBackend(
        engine_factory=lambda: engine, engine_address="127.0.0.1:1000"
    )
    backend.start()
    backend._hixl = binding(engine, mem_desc)
    return backend


class BindingBackend(HixlSourceBackend):
    """The source backend with its binding injected at start.

    The worker starts the backend itself, so the binding is installed in the
    same start call the worker makes.
    """

    def __init__(self, engine, mem_desc=MemDesc):
        super().__init__(engine_factory=lambda: engine, engine_address="127.0.0.1:1000")
        self._engine_fake = engine
        self._mem_desc = mem_desc

    def start(self, engine_address: str = ""):
        engine = super().start(engine_address)
        self._hixl = binding(self._engine_fake, self._mem_desc)
        return engine


class RefusingMemDesc:
    """A binding whose descriptor refuses the flag, with its own text."""

    def __init__(self, addr, length):
        self.addr = addr
        self.len = length

    def __setattr__(self, name, value):
        if name == "local_only":
            raise RuntimeError(SECRET)
        super().__setattr__(name, value)


class NoLocalOnly:
    """A binding whose MemDesc has no local_only, as a pybind11 class."""

    __slots__ = ("addr", "len")

    def __init__(self, addr, length):
        self.addr = addr
        self.len = length


class DropsLocalOnly:
    """A binding that accepts the write and keeps the old value."""

    def __init__(self, addr, length):
        self.addr = addr
        self.len = length
        self.local_only = False

    def __setattr__(self, name, value):
        if name == "local_only":
            return
        super().__setattr__(name, value)


class RaisesOnRead:
    """A binding whose descriptor raises when the flag is read back."""

    def __init__(self, addr, length):
        self.addr = addr
        self.len = length
        self.local_only = False

    def __getattribute__(self, name):
        if name == "local_only":
            raise RuntimeError(SECRET)
        return object.__getattribute__(self, name)


def order(generation="generation-1", tree=FIXTURE):
    """One source order over the pinned fixture tree."""
    raw = (FIXTURE / "manifest.json").read_bytes()
    manifest = json.loads(raw)
    return {
        "generation": generation,
        "workerSecret": "secret",
        "digest": "sha256:" + "a" * 64,
        "namespace": "test",
        "artifactUID": manifest["artifact_uid"],
        "sourceGeneration": manifest["source_generation"],
        "manifestSHA256": hashlib.sha256(raw).hexdigest(),
        "manifestJSON": raw.decode(),
        "treePath": str(tree),
        "totalBytes": sum(f["size"] for f in manifest["files"]),
        "registrations": len(manifest["files"]),
    }


class WorkerControl:
    """The control client's shape, recording the quarantine reason."""

    def __init__(self, generation):
        self.generation = generation
        self.worker_id = "pod-uid"
        self.quarantine_reason = None

    def register(self):
        return "lease"

    def native_start(self):
        return None

    def heartbeat(self, **kwargs):
        return {"action": "none", "activeSessions": 0, "unknownSessions": 0}

    def filled(self, registrations, descriptors):
        raise AssertionError("a refused registration must not report filled")

    def drain_ack(self, native_released):
        raise AssertionError("a refused registration must not acknowledge a drain")

    def quarantine(self, reason):
        self.quarantine_reason = reason

    def close(self):
        pass


class WorkerAllocator:
    """An allocator that hands out one pinned block per file."""

    def __init__(self):
        self.blocks = []

    def allocate(self, name, size):
        block = Block(name, ADDRESS + len(self.blocks) * 0x1000, size)
        self.blocks.append(block)
        return block

    def release(self, block):
        pass

    def dispose(self):
        pass


def test_the_source_registers_its_blocks_as_local_only():
    engine = Engine()
    backend = source_backend(engine)
    blocks = {"a": Block("a", ADDRESS, 8), "b": Block("b", ADDRESS + 0x1000, 16)}

    registration = backend.register(blocks)

    registrations = engine.registrations()
    assert len(registrations) == 2
    for (_kind, desc, mem_type), name in zip(registrations, ["a", "b"], strict=True):
        assert desc.local_only is True
        assert desc.addr == blocks[name].data_ptr()
        assert desc.len == blocks[name].byte_size
        assert mem_type is MemType.MEM_HOST
    assert registration.handles == {"a": ADDRESS, "b": ADDRESS + 0x1000}


def test_the_generic_path_stays_compatible_and_claims_no_qualification():
    """The generic path has no binding, so it keeps its own descriptor.

    It is an explicit test injection. It carries no local-only flag and
    simulates no native qualification.
    """
    engine = Engine()
    backend = HixlSourceBackend(
        engine_factory=lambda: engine, engine_address="127.0.0.1:1000"
    )
    backend.start()

    backend.register({"a": Block("a", ADDRESS, 8)})

    _kind, desc, mem_type = engine.registrations()[0]
    assert desc == (ADDRESS, 8)
    assert mem_type == "MEM_HOST"


def test_a_binding_without_the_property_fails_closed_before_the_native_call():
    engine = Engine()
    backend = source_backend(engine, NoLocalOnly)

    with pytest.raises(SourceLocalOnlyRefusedError):
        backend.register({"a": Block("a", ADDRESS, 8)})

    assert engine.registrations() == []


def test_a_binding_that_drops_the_property_fails_closed():
    engine = Engine()
    backend = source_backend(engine, DropsLocalOnly)

    with pytest.raises(SourceLocalOnlyRefusedError):
        backend.register({"a": Block("a", ADDRESS, 8)})

    assert engine.registrations() == []


def test_a_later_block_without_the_flag_retains_the_earlier_registration():
    made = []

    def mem_desc(addr, length):
        if made:
            return NoLocalOnly(addr, length)
        made.append(addr)
        return MemDesc(addr, length)

    engine = Engine()
    backend = source_backend(engine, mem_desc)
    blocks = {"a": Block("a", ADDRESS, 8), "b": Block("b", ADDRESS + 0x1000, 16)}
    refs = [weakref.ref(block) for block in blocks.values()]

    with pytest.raises(SourceLocalOnlyRefusedError) as error:
        backend.register(blocks)

    assert error.value.retainment.handles == {"a": ADDRESS}
    assert error.value.retainment.backend is backend
    assert len(engine.registrations()) == 1
    del blocks
    gc.collect()
    assert all(ref() is not None for ref in refs)


def test_the_first_block_refusal_names_the_reason_and_keeps_the_state():
    """A refused flag is a known outcome, so the reason is specific."""
    engine = Engine()
    backend = source_backend(engine, RefusingMemDesc)
    blocks = {"a": Block("a", ADDRESS, 8)}

    with pytest.raises(SourceLocalOnlyRefusedError) as error:
        backend.register(blocks)

    assert error.value.retainment.detail == SourceLocalOnlyRefusedError.__name__
    assert error.value.retainment.handles == {}
    assert error.value.retainment.blocks.keys() == {"a"}
    assert error.value.retainment.backend is backend
    assert engine.registrations() == []


def test_a_dropped_flag_reports_the_same_refusal():
    """A binding that keeps the old value refused the flag as well."""
    engine = Engine()
    backend = source_backend(engine, DropsLocalOnly)
    blocks = {"a": Block("a", ADDRESS, 8)}

    with pytest.raises(SourceLocalOnlyRefusedError) as error:
        backend.register(blocks)

    assert error.value.retainment.detail == SourceLocalOnlyRefusedError.__name__
    assert error.value.retainment.blocks.keys() == {"a"}
    assert engine.registrations() == []


def test_a_binding_whose_getter_raises_takes_the_same_refusal():
    """The flag could not be read back, so the binding did not retain it."""
    engine = Engine()
    backend = source_backend(engine, RaisesOnRead)
    blocks = {"a": Block("a", ADDRESS, 8)}

    with pytest.raises(SourceLocalOnlyRefusedError) as error:
        backend.register(blocks)

    assert error.value.retainment.detail == SourceLocalOnlyRefusedError.__name__
    assert error.value.retainment.blocks.keys() == {"a"}
    assert engine.registrations() == []
    assert SECRET not in str(error.value)
    assert SECRET not in _error_chain_text(error.value)


def test_a_later_block_refusal_keeps_the_earlier_handle_and_the_reason():
    """The earlier registration really happened, so its handle stays."""
    made = []

    def mem_desc(addr, length):
        if made:
            return RefusingMemDesc(addr, length)
        made.append(addr)
        return MemDesc(addr, length)

    engine = Engine()
    backend = source_backend(engine, mem_desc)
    blocks = {"a": Block("a", ADDRESS, 8), "b": Block("b", ADDRESS + 0x1000, 16)}

    with pytest.raises(SourceLocalOnlyRefusedError) as error:
        backend.register(blocks)

    assert error.value.retainment.detail == SourceLocalOnlyRefusedError.__name__
    assert error.value.retainment.handles == {"a": ADDRESS}
    assert len(engine.registrations()) == 1


def test_the_binding_text_never_reaches_the_retained_error():
    """The binding's own text stays inside the binding."""
    engine = Engine()
    backend = source_backend(engine, RefusingMemDesc)

    with pytest.raises(SourceStateRetainedError) as error:
        backend.register({"a": Block("a", ADDRESS, 8)})

    assert isinstance(error.value, SourceLocalOnlyRefusedError)
    assert SECRET not in str(error.value)
    assert SECRET not in error.value.retainment.detail
    assert SECRET not in _error_chain_text(error.value)


def _error_chain_text(exc):
    """Every message in one error's chain, cause and context included."""
    parts = []
    seen = set()
    while exc is not None and id(exc) not in seen:
        seen.add(id(exc))
        parts.append(str(exc))
        exc = exc.__cause__ or exc.__context__
    return "\n".join(parts)


def test_the_source_worker_reports_the_reason_and_hides_the_binding_text(
    caplog,
):
    """The worker path names the reason in its log and its quarantine."""
    engine = Engine()
    control = WorkerControl("generation-1")
    allocator = WorkerAllocator()
    assignment = parse_assignment(
        dict(order(), residentBudgetBytes=4 << 20), backend="hixl"
    )
    backend = BindingBackend(engine, RefusingMemDesc)

    with caplog.at_level(logging.WARNING, logger="gpustack_model_transfer.source"):
        worker = NativeSourceWorker(
            control,
            assignment,
            backend=lambda: backend,
            allocator=lambda: allocator,
            worker_instance_id="instance-1",
            poll_interval=0.01,
        )
        worker.start()
        worker._stop_event.set()

    assert worker.state == "quarantined"
    assert control.quarantine_reason == SourceLocalOnlyRefusedError.__name__
    assert worker._retention.detail == SourceLocalOnlyRefusedError.__name__
    assert SourceLocalOnlyRefusedError.__name__ in caplog.text
    assert SECRET not in caplog.text
    assert SECRET not in control.quarantine_reason
    assert SECRET not in worker._retention.detail


def test_the_consumer_registration_keeps_its_own_descriptor(monkeypatch):
    """The consumer registers its own window; the flag is the source's."""
    from gpustack_model_transfer import client_native
    from gpustack_model_transfer.backends.hixl import runtime

    class ConsumerMemDesc:
        def __init__(self, addr, length):
            self.addr = addr
            self.len = length
            self.local_only = False

    registered = []

    class ConsumerEngine:
        def initialize(self, endpoint, options):
            return 0

        def register_mem(self, desc, mem_type):
            registered.append((desc, mem_type))
            return (0, 777)

    class Owner:
        def ensure_current(self):
            pass

        def allocate(self, name, size):
            return SimpleNamespace(data_ptr=lambda: 2 << 20, registration_bytes=size)

    hixl = SimpleNamespace(
        Hixl=ConsumerEngine,
        SUCCESS=0,
        MemDesc=ConsumerMemDesc,
        MemType=SimpleNamespace(MEM_HOST="host"),
    )
    monkeypatch.setattr(client_native, "BorrowedAclOwner", Owner)
    monkeypatch.setattr(
        runtime, "load_runtime", lambda: SimpleNamespace(_require=lambda: hixl)
    )

    target = client_native.TargetNative("hixl", "127.0.0.1", 8)
    target.start()

    desc, mem_type = registered[0]
    assert desc.local_only is False
    assert mem_type == "host"
