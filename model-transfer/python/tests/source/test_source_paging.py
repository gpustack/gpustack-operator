"""Bounded source residency and verification with injected native runtimes."""

import ctypes
import hashlib
import threading
from dataclasses import replace
from types import SimpleNamespace

import pytest
from test_fill import make_tree
from test_idle_blocks import Control
from test_native_operations import common, owner

from gpustack_model_transfer.core.descriptors import FileDescriptor, TensorDescriptor
from gpustack_model_transfer.core.manifest import Manifest
from gpustack_model_transfer.source import memory
from gpustack_model_transfer.source.backend import (
    HixlSourceBackend,
    NixlSourceBackend,
    SourcePrePostError,
    SourceStateRetainedError,
)
from gpustack_model_transfer.source.fill import fill_from_tree
from gpustack_model_transfer.source.native_worker import NativeSourceWorker
from gpustack_model_transfer.source.transfer_service import OperationRegistry


def test_initial_fill_does_not_open_nonresident_files(tmp_path, monkeypatch):
    from gpustack_model_transfer.source import fill

    manifest, blocks, payloads = make_tree(tmp_path, count=6)
    resident = {"0.safetensors": blocks["0.safetensors"]}
    opened = []
    original = fill.resolve_tree_file

    def record(root, path):
        opened.append(path)
        return original(root, path)

    monkeypatch.setattr(fill, "resolve_tree_file", record)
    report = fill_from_tree(manifest, str(tmp_path), resident, resident_only=True)
    assert opened == ["0.safetensors"]
    assert report.files_filled == 1
    assert report.bytes_filled == 80
    assert resident["0.safetensors"].data == payloads["0.safetensors"]


MIB = 1 << 20


class Storage:
    """Track actual injected allocator storage, including freed references."""

    def __init__(self):
        self.live = 0
        self.peak = 0
        self.frees = 0
        self.fail_free = False

    def allocate(self, size):
        self.live += size
        self.peak = max(self.peak, self.live)
        return ctypes.create_string_buffer(size)

    def free(self, size):
        if self.fail_free:
            raise RuntimeError("free outcome unknown")
        self.live -= size
        self.frees += 1


class Function:
    def __init__(self, call):
        self.call = call

    def __call__(self, *args):
        return self.call(*args)


class AclLibrary:
    def __init__(self, storage):
        self.storage = storage
        self.allocations = {}
        for name in (
            "aclInit",
            "aclrtSetDevice",
            "aclrtSetCurrentContext",
            "aclrtDestroyContext",
            "aclrtResetDevice",
            "aclFinalize",
        ):
            setattr(self, name, Function(lambda *args: 0))
        self.aclrtCreateContext = Function(self.context)
        self.aclrtMallocHost = Function(self.allocate)
        self.aclrtFreeHost = Function(self.free)

    def context(self, output, device):
        ctypes.cast(output, ctypes.POINTER(ctypes.c_void_p))[0] = ctypes.c_void_p(42)
        return 0

    def allocate(self, output, size):
        raw = self.storage.allocate(size.value)
        address = ctypes.addressof(raw)
        self.allocations[address] = raw
        ctypes.cast(output, ctypes.POINTER(ctypes.c_void_p))[0] = ctypes.c_void_p(
            address
        )
        return 0

    def free(self, address):
        self.storage.free(len(self.allocations[address.value]))
        del self.allocations[address.value]
        return 0


class FakeNixlBackend:
    def __init__(self):
        self.registered = []
        self.payloads = []
        self.fail_revoke = False
        self.fail_register = False

    def create_agent(self, name, settings):
        return object()

    def register_memory(self, agent, blocks):
        if self.fail_register:
            raise RuntimeError("registration outcome unknown")
        self.registered.append(tuple(blocks))
        return SimpleNamespace(handles=dict.fromkeys(blocks, 1), blocks=blocks)

    def revoke_registration(self, agent, registration):
        if self.fail_revoke:
            raise RuntimeError("deregistration outcome unknown")
        registration.handles.clear()
        registration.blocks.clear()

    def connect_peer(self, agent, metadata):
        return object()

    def submit_source_write(
        self, agent, registration, connection, name, target, length, local_offset
    ):
        payload = registration.blocks[name].read_bytes(local_offset, length)
        self.payloads.append(payload)
        return SimpleNamespace(released=True)

    def poll(self, agent, operation):
        return SimpleNamespace(state="succeeded")

    def disconnect_peer(self, agent, connection):
        pass


class FakeHixlEngine:
    def __init__(self):
        self.registered = []
        self.payloads = []
        self.connected = []
        self.disconnected = []
        self.fail_revoke = False
        self.fail_register = False

    def initialize(self, address, options):
        return 0

    def register_mem(self, descriptor, mem_type):
        if self.fail_register:
            return 1, 0
        self.registered.append(descriptor)
        return 0, len(self.registered)

    def deregister_mem(self, handle):
        return 1 if self.fail_revoke else 0

    def connect(self, endpoint):
        self.connected.append(endpoint)
        return 0

    def transfer_async(self, endpoint, direction, descriptors):
        address, _, length = descriptors[0]
        self.payloads.append(ctypes.string_at(address, length))
        return 0, len(self.payloads)

    def get_transfer_status(self, operation):
        return 0, "completed"

    def disconnect(self, endpoint):
        self.disconnected.append(endpoint)
        return 0

    def finalize(self):
        return 0


def checkpoint(tmp_path, size=4 * MIB):
    manifest = Manifest(2, "artifact", 1, (), (), "0" * 64)
    files, tensors = [], []
    for index in range(6):
        name = f"{index}.safetensors"
        payload = bytes([index + 1]) * size
        (tmp_path / name).write_bytes(payload)
        files.append(FileDescriptor(name, size))
        tensors.append(
            TensorDescriptor(
                name,
                name,
                "U8",
                (size,),
                0,
                size,
            )
        )
    return replace(manifest, files=tuple(files), tensors=tuple(tensors))


@pytest.fixture(params=["nixl", "hixl"])
def paged_worker(request, tmp_path, monkeypatch):
    manifest = checkpoint(tmp_path)
    storage = Storage()
    name = request.param
    assignment = SimpleNamespace(
        manifest=manifest,
        tree_path=str(tmp_path),
        backend=name,
        total_bytes=24 * MIB,
        resident_budget_bytes=(12 if name == "hixl" else 10) * MIB,
        paged=True,
        registrations=6,
    )
    if name == "nixl":

        class CudaRuntime:
            def __init__(self):
                self.sizes = {}

            def cudaHostRegister(self, address, size, flags):
                self.sizes[address] = size
                storage.live += size
                storage.peak = max(storage.peak, storage.live)
                return 0

            def cudaHostUnregister(self, address):
                storage.free(self.sizes.pop(address))
                return 0

        cuda = CudaRuntime()
        torch = SimpleNamespace(cuda=SimpleNamespace(cudart=lambda: cuda))
        allocator = memory.TorchPinnedOwner(torch, cache=False)
        runtime = FakeNixlBackend()
        backend = NixlSourceBackend(runtime)
    else:
        monkeypatch.setattr(memory, "_ACL_RUNTIME", None)
        monkeypatch.setattr(memory, "_ACL_BROKEN", None)
        library = AclLibrary(storage)
        allocator = memory.AclHostOwner(loader=lambda _: library)
        runtime = FakeHixlEngine()
        backend = HixlSourceBackend(lambda: runtime)
    control = Control()
    control.lease_id = "lease"
    worker = NativeSourceWorker(
        control,
        assignment,
        backend=backend,
        allocator=allocator,
        worker_instance_id="instance",
        poll_interval=0.01,
    )
    worker.start()
    worker._stop_event.set()
    worker._control_thread.join(timeout=2)
    assert worker.state == "serving"
    if name == "hixl":
        backend._hixl = SimpleNamespace(
            SUCCESS=0,
            MemDesc=lambda addr, size: SimpleNamespace(address=addr, size=size),
            MemType=SimpleNamespace(MEM_HOST="HOST"),
            TransferOp=SimpleNamespace(WRITE="WRITE"),
            TransferOpDesc=lambda *args: args,
            TransferStatus=SimpleNamespace(COMPLETED="completed", FAILED="failed"),
        )
    yield worker, runtime, storage
    # These are CPU allocations only. A production quarantine needs an external fence.
    if worker.state != "quarantined" and not backend._paging_failed:
        worker.active_sessions = worker.unknown_sessions = worker.native_operations = 0
        worker.release_idle_blocks()
        if hasattr(allocator, "dispose"):
            allocator.dispose()


def transfer(worker, name, length=32):
    target = {
        "metadata": "dGFyZ2V0",
        "endpoint": "127.0.0.1:9000",
        "address": 4096,
        "length": length,
    }
    return worker.backend.post_write(worker.registration, name, 0, length, target)


@pytest.mark.parametrize("order", [(0, 1, 2, 3, 4, 5), (5, 1, 4, 0, 3, 2)])
def test_all_transfers_stay_within_pinned_budget(paged_worker, order):
    worker, runtime, storage = paged_worker
    assert worker.fill_report.files_filled == 2
    assert worker.fill_report.tensors_filled == 2
    assert set(worker.blocks) == {"0.safetensors", "1.safetensors"}
    assert worker.control.filled_data[0] == 2
    for index in order:
        operation = transfer(worker, f"{index}.safetensors")
        assert worker.backend.poll_write(operation) == "succeeded"
        assert runtime.payloads[-1] == bytes([index + 1]) * 32
        worker.backend.close_write(operation)
        assert storage.live == sum(
            getattr(b, "allocation_bytes", b.byte_size) for b in worker.blocks.values()
        )
    assert storage.peak <= worker.assignment.resident_budget_bytes
    assert storage.frees > 0


@pytest.mark.parametrize("failure", ["revoke", "free", "register", "truncate"])
def test_failed_page_in_retains_state_and_never_posts(
    paged_worker, monkeypatch, failure
):
    worker, runtime, storage = paged_worker
    original = dict(worker.blocks)
    before = len(runtime.registered)
    if failure == "revoke":
        runtime.fail_revoke = True
    elif failure == "free":

        def fail_free(block):
            raise RuntimeError("free outcome unknown")

        monkeypatch.setattr(worker.allocator, "release", fail_free)
    elif failure == "register":
        runtime.fail_register = True
    else:
        from pathlib import Path

        with (Path(worker.assignment.tree_path) / "5.safetensors").open(
            "r+b"
        ) as handle:
            handle.truncate(1)
    with pytest.raises(SourceStateRetainedError) as caught:
        transfer(worker, "5.safetensors")
    assert not runtime.payloads
    assert caught.value.retainment.backend is worker.backend
    assert caught.value.retainment.owner is worker.allocator
    assert caught.value.retainment.blocks
    assert len(runtime.registered) == before
    if failure in ("revoke", "free"):
        assert worker.blocks == original
        assert storage.frees == 0
    assert storage.peak <= worker.assignment.resident_budget_bytes


def test_idle_restore_reads_only_initial_resident_batch(paged_worker, monkeypatch):
    worker, _, storage = paged_worker
    operation = transfer(worker, "5.safetensors")
    worker.backend.close_write(operation)
    worker.active_sessions = worker.unknown_sessions = 0
    assert worker.release_idle_blocks()
    assert storage.live == 0
    assert not worker.blocks
    opened = []
    from gpustack_model_transfer.source import fill

    original = fill.resolve_tree_file

    def record(tree, path):
        opened.append(path)
        return original(tree, path)

    monkeypatch.setattr(fill, "resolve_tree_file", record)
    worker._rebuild_blocks()
    assert worker.state == "serving"
    assert not worker._blocks_idle
    assert set(opened) == {"0.safetensors", "1.safetensors"}
    assert storage.live == (12 if worker.assignment.backend == "hixl" else 8) * MIB
    assert storage.peak <= worker.assignment.resident_budget_bytes


def test_active_operations_hold_window_until_acknowledged(paged_worker):
    worker, _, storage = paged_worker
    operations = [transfer(worker, "0.safetensors") for _ in range(2)]
    assert worker.backend._in_use_counts == {"0.safetensors": 2}
    for operation in operations:
        assert worker.backend.poll_write(operation) == "succeeded"
        with pytest.raises(SourcePrePostError):
            transfer(worker, "5.safetensors")
        assert storage.frees == 0
        worker.backend.close_write(operation)
    operation = transfer(worker, "5.safetensors")
    worker.backend.close_write(operation)
    assert storage.frees == 2


def request_body(worker, index, request, session):
    name = f"{index}.safetensors"
    port = 10000 + int.from_bytes(hashlib.sha256(request.encode()).digest()[:2]) % 50000
    return {
        "sourcePodUID": worker.control.worker_id,
        "workerInstanceID": "instance",
        "generation": worker.control.generation,
        "leaseID": "lease",
        "sessionID": session,
        "requestID": request,
        "targetPodUID": "target",
        "targetInstanceID": session,
        "registrationID": "registration",
        "windowID": request,
        "tensor": name,
        "file": name,
        "fileOffset": 0,
        "tensorOffset": 0,
        "length": 32,
        "target": {
            "backend": worker.assignment.backend,
            "address": 4096,
            "length": 32,
            "metadata": "dGFyZ2V0",
            "endpoint": f"127.0.0.1:{port}",
        },
    }


def test_scheduler_drains_two_sessions_before_nonresident_dispatch(paged_worker):
    worker, runtime, storage = paged_worker
    supervisor = SimpleNamespace(
        workers={worker.control.generation: worker}, _admission_closed=threading.Event()
    )
    registry = OperationRegistry(supervisor)
    worker.operations = registry
    bodies = [
        request_body(worker, index, f"request-{i}", f"session-{i}")
        for i, index in enumerate([0, 1, 5])
    ]
    for body in bodies:
        registry.submit(body)
    for _ in range(12):
        registry.step(worker)
    assert len(runtime.payloads) == 2
    assert registry.status(common(bodies[2]))["state"] == "dispatching"
    assert storage.frees == 0
    for body in bodies[:2]:
        registry.acknowledge({**common(body), **owner(body)})
        for _ in range(8):
            registry.step(worker)
    assert runtime.payloads == [bytes([1]) * 32, bytes([2]) * 32, bytes([6]) * 32]
    assert registry.status(common(bodies[2]))["state"] == "terminal"
    registry.acknowledge({**common(bodies[2]), **owner(bodies[2])})
    registry.step(worker)
    assert worker.native_operations == 0
    assert storage.peak <= worker.assignment.resident_budget_bytes


@pytest.mark.parametrize("session_count", [1, 2])
def test_two_windows_remain_usable_across_page_replacement(paged_worker, session_count):
    worker, runtime, storage = paged_worker
    registry = OperationRegistry(
        SimpleNamespace(
            workers={worker.control.generation: worker},
            _admission_closed=threading.Event(),
        )
    )
    worker.operations = registry
    last_requests = {}
    indices = [0, 1, 0, 1, 4, 5, 4, 5, 0, 1]
    for number, index in enumerate(indices):
        window = number % 2
        session = f"session-{window % session_count}"
        body = request_body(worker, index, f"request-{number}", session)
        body["windowID"] = f"window-{window}"
        body["target"]["endpoint"] = f"127.0.0.1:{1234 + window}"
        registry.submit(body)
        registry.step(worker)
        registry.step(worker)
        assert registry.status(common(body))["verdict"] == "succeeded"
        registry.acknowledge({**common(body), **owner(body)})
        registry.step(worker)
        assert registry.status(common(body))["state"] == "acknowledged"
        last_requests[session] = body
    assert runtime.payloads == [bytes([index + 1]) * 32 for index in indices]
    assert worker.native_operations == 0
    assert storage.peak <= worker.assignment.resident_budget_bytes
    if worker.assignment.backend == "hixl":
        assert len(runtime.connected) == 6
        assert len(runtime.disconnected) == 4
    for body in last_requests.values():
        results = []
        worker._wake_event.clear()
        thread = threading.Thread(
            target=lambda body=body, results=results: results.append(
                registry.close_session(common(body))
            )
        )
        thread.start()
        try:
            if worker.assignment.backend == "hixl":
                assert worker._wake_event.wait(2)
                registry.step(worker)
        finally:
            thread.join(timeout=5)
        assert not thread.is_alive()
        assert results == [{"state": "closed"}]
    assert worker.native_operations == 0
    if worker.assignment.backend == "hixl":
        assert runtime.disconnected == runtime.connected


@pytest.mark.parametrize(
    "backend,admitted,unified,legacy,want",
    [
        ("nixl", None, "0", "", 24),
        ("nixl", 10, "0", "", 10),
        ("nixl", 16, str(10 * MIB), "", 10),
        ("hixl", 16, str(10 * MIB), str(8 * MIB), 8),
        ("hixl", 10, "0", str(16 * MIB), 10),
    ],
)
def test_work_order_uses_smallest_resident_ceiling(
    tmp_path, monkeypatch, backend, admitted, unified, legacy, want
):
    import json
    from dataclasses import asdict

    from test_native_source import order

    from gpustack_model_transfer.source import workorder

    monkeypatch.setattr(workorder, "MIN_RESIDENT_BYTES", MIB)
    monkeypatch.setenv("GPUSTACK_MODEL_TRANSFER_SOURCE_RESIDENT_BUDGET_BYTES", unified)
    monkeypatch.setenv("GPUSTACK_MODEL_TRANSFER_HIXL_RESIDENT_BUDGET_BYTES", legacy)
    manifest = asdict(checkpoint(tmp_path))
    del manifest["manifest_sha256"]
    raw = json.dumps(manifest)
    payload = dict(
        order(),
        artifactUID="artifact",
        sourceGeneration=1,
        manifestJSON=raw,
        manifestSHA256=hashlib.sha256(raw.encode()).hexdigest(),
        totalBytes=24 * MIB,
        registrations=6,
    )
    if admitted is not None:
        payload["residentBudgetBytes"] = admitted * MIB
    assignment = workorder.parse_assignment(payload, backend=backend)
    assert assignment.resident_budget_bytes == want * MIB
    assert assignment.paged == (want < 24)


@pytest.mark.parametrize("value", [True, -1, 0, "1024", 1, 1 << 40])
def test_invalid_admitted_budget_refused_before_native_allocation(value):
    from test_native_source import order

    from gpustack_model_transfer.core.errors import InvalidDescriptorError
    from gpustack_model_transfer.source import workorder

    with pytest.raises(InvalidDescriptorError):
        workorder.parse_assignment(
            dict(order(), residentBudgetBytes=value), backend="nixl"
        )


@pytest.mark.parametrize("value", ["typo", "-1"])
def test_invalid_unified_setting_refused(monkeypatch, value):
    from test_native_source import order

    from gpustack_model_transfer.core.errors import InvalidDescriptorError
    from gpustack_model_transfer.source import workorder

    monkeypatch.setenv("GPUSTACK_MODEL_TRANSFER_SOURCE_RESIDENT_BUDGET_BYTES", value)
    with pytest.raises(InvalidDescriptorError):
        workorder.parse_assignment(order(), backend="nixl")


def test_page_in_failure_quarantines_worker_and_holds_operation(paged_worker):
    worker, runtime, storage = paged_worker
    runtime.fail_revoke = True
    registry = OperationRegistry(
        SimpleNamespace(
            workers={worker.control.generation: worker},
            _admission_closed=threading.Event(),
        )
    )
    worker.operations = registry
    body = request_body(worker, 5, "failed", "session")
    registry.submit(body)
    registry.step(worker)
    assert worker.state == "quarantined"
    assert worker.native_operations == 1
    assert registry.status(common(body))["state"] == "unknown"
    assert storage.frees == 0
    assert worker._retention.blocks


@pytest.mark.parametrize("stage", ["pin", "unpin"])
@pytest.mark.parametrize("failure", ["status", "raise"])
def test_uncached_host_owner_retains_ambiguous_runtime_state(stage, failure):
    class Runtime:
        def cudaHostRegister(self, address, size, flags):
            return self.result("pin")

        def cudaHostUnregister(self, address):
            return self.result("unpin")

        def result(self, current):
            if current != stage:
                return 0
            if failure == "raise":
                raise RuntimeError("unknown native outcome")
            return 1

    runtime = Runtime()
    allocator = memory.TorchPinnedOwner(
        SimpleNamespace(cuda=SimpleNamespace(cudart=lambda: runtime)), cache=False
    )
    from gpustack_model_transfer.core.errors import SourceUnavailableError

    if stage == "pin":
        with pytest.raises((RuntimeError, SourceUnavailableError)):
            allocator.allocate("file", MIB)
        assert len(allocator._uncertain_allocations) == 1
        block = allocator._uncertain_allocations[0]
    else:
        block = allocator.allocate("file", MIB)
        with pytest.raises((RuntimeError, SourceUnavailableError)):
            allocator.release(block)
    assert block.address != 0
    assert not block.mapping.closed
    # The injected runtime never pinned this CPU mapping.
    block.mapping.close()


def test_waiting_page_miss_prevents_new_resident_bypass(paged_worker):
    worker, runtime, _ = paged_worker
    supervisor = SimpleNamespace(
        workers={worker.control.generation: worker}, _admission_closed=threading.Event()
    )
    registry = OperationRegistry(supervisor)
    worker.operations = registry
    bodies = [
        request_body(worker, index, f"fair-{i}", f"fair-session-{i}")
        for i, index in enumerate([0, 5, 1])
    ]
    registry.submit(bodies[0])
    for _ in range(4):
        registry.step(worker)
    registry.submit(bodies[1])
    for _ in range(4):
        registry.step(worker)
    registry.submit(bodies[2])
    for _ in range(8):
        registry.step(worker)
    assert runtime.payloads == [bytes([1]) * 32]
    for i, expected in enumerate([6, 2]):
        registry.acknowledge({**common(bodies[i]), **owner(bodies[i])})
        for _ in range(12):
            registry.step(worker)
        assert runtime.payloads[-1] == bytes([expected]) * 32
        assert len(runtime.payloads) == i + 2
    registry.acknowledge({**common(bodies[2]), **owner(bodies[2])})
    registry.step(worker)
    assert worker.native_operations == 0


def test_page_swap_reuses_overlapping_published_blocks(paged_worker, monkeypatch):
    from gpustack_model_transfer.source import fill

    worker, _, storage = paged_worker
    operation = transfer(worker, "5.safetensors")
    worker.backend.close_write(operation)
    retained = worker.blocks["5.safetensors"]
    opened = []
    original = fill.resolve_tree_file

    def record(tree, path):
        opened.append(path)
        return original(tree, path)

    monkeypatch.setattr(fill, "resolve_tree_file", record)
    operation = transfer(worker, "4.safetensors")
    worker.backend.close_write(operation)
    assert worker.blocks["5.safetensors"] is retained
    assert opened == ["4.safetensors"]
    assert storage.peak <= worker.assignment.resident_budget_bytes
