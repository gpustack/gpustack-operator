"""Idle host-RAM reclamation and the dynamic source window on the native
source worker."""

import hashlib
import json
import threading
import time
from dataclasses import replace
from pathlib import Path
from types import SimpleNamespace

import pytest

from gpustack_model_transfer.source.backend import SourceRegistration
from gpustack_model_transfer.source.memory import (
    MIN_SOURCE_BUFFER_BYTES,
    compute_dynamic_source_buffer,
)
from gpustack_model_transfer.source.native_worker import NativeSourceWorker
from gpustack_model_transfer.source.workorder import parse_assignment

FIXTURE = Path(__file__).resolve().parents[3] / "contracts/fixtures/tiny_dense"
GIB = 1 << 30
MIB = 1 << 20


def order(generation="generation-1", tree=FIXTURE):
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


class Block:
    def __init__(self, name, size):
        self.name = name
        self.data = bytearray(size)
        self.byte_size = size

    def data_ptr(self):
        return 12345

    def write_bytes(self, offset, data):
        self.data[offset : offset + len(data)] = data

    def view(self, offset, length):
        return memoryview(self.data)[offset : offset + length]

    def read_bytes(self, offset, length):
        return bytes(self.data[offset : offset + length])


class Allocator:
    def __init__(self):
        self.allocations = 0
        self.releases = 0

    def allocate(self, name, size):
        self.allocations += 1
        return Block(name, size)

    def release(self, block):
        self.releases += 1


class Backend:
    def __init__(self):
        self.registrations = 0
        self.disconnects = 0
        self.revokes = 0

    def start(self):
        pass

    def register(self, blocks):
        self.registrations += 1
        return SourceRegistration(
            "nixl", {name: object() for name in blocks}, dict(blocks)
        )

    def disconnect(self, registration):
        self.disconnects += 1

    def revoke(self, registration):
        self.revokes += 1
        registration.handles.clear()


class Control:
    def __init__(self, generation="generation-1"):
        self.generation = generation
        self.worker_id = "pod-uid"
        self.worker_instance_id = "instance-1"
        self.active = 0
        self.unknown = 0
        self.action = "none"
        self.filled_data = None
        self.acks = []

    def register(self):
        return "lease"

    def native_start(self):
        return None

    def heartbeat(self, **kwargs):
        return {
            "action": self.action,
            "activeSessions": self.active,
            "unknownSessions": self.unknown,
        }

    def filled(self, registrations, descriptors):
        self.filled_data = (registrations, descriptors)

    def drain_ack(self, native_released):
        self.acks.append(native_released)

    def quarantine(self, reason):
        pass

    def close(self):
        pass


def eventually(condition, timeout=3):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if condition():
            return
        time.sleep(0.005)
    raise AssertionError("condition timed out")


def new_worker(monkeypatch, avail_ram=None, *, max_memory_bytes=None):
    control = Control()
    allocator = Allocator()
    backend = Backend()
    assignment = parse_assignment(order(), backend="nixl")
    if max_memory_bytes is not None:
        monkeypatch.setenv(
            "GPUSTACK_MODEL_TRANSFER_SOURCE_MAX_MEMORY_BYTES", str(max_memory_bytes)
        )
    if avail_ram is not None:
        monkeypatch.setattr(
            "gpustack_model_transfer.source.native_worker.available_ram",
            lambda path=None: avail_ram,
        )
    worker = NativeSourceWorker(
        control,
        assignment,
        backend=lambda: backend,
        allocator=lambda: allocator,
        worker_instance_id="instance-1",
        poll_interval=0.01,
    )
    worker.operations = SimpleNamespace(
        step=lambda _: False,
        has_pending=lambda _: False,
        mark_unknown=lambda _: None,
    )
    return worker, control, allocator, backend


def test_idle_release_and_restore_follow_session_transitions(monkeypatch):
    worker, control, allocator, backend = new_worker(monkeypatch)
    try:
        worker.start()
        assert worker.state == "serving"
        assert worker.blocks

        # A session arrives and drains away: the idle transition is the
        # release signal, and the serving loop performs it.
        control.active = 1
        eventually(lambda: worker.active_sessions == 1)
        assert not worker.release_idle_blocks(), "a busy generation keeps its blocks"
        control.active = 0
        eventually(lambda: worker.active_sessions == 0)
        assert worker.once() == "serving"
        assert worker._blocks_idle
        assert worker.blocks == {}
        assert worker.registration is None
        assert backend.disconnects == 1
        assert backend.revokes == 1
        assert allocator.releases == len(worker.assignment.manifest.files)

        # The next session re-allocates, refills, and re-registers on the
        # generation thread, and the resident figure returns.
        control.active = 1
        eventually(lambda: worker.active_sessions == 1)
        assert worker.once() == "serving"
        assert not worker._blocks_idle
        assert worker.blocks
        assert worker.registration is not None
        assert backend.registrations == 2
        assert allocator.allocations == 2 * len(worker.assignment.manifest.files)

        # The generation drains again: the resident figure falls once more.
        control.active = 0
        eventually(lambda: worker.active_sessions == 0)
        assert worker.once() == "serving"
        assert worker._blocks_idle
    finally:
        worker.close()


def test_an_explicit_release_signal_is_the_same_path(monkeypatch):
    worker, control, _, backend = new_worker(monkeypatch)
    try:
        worker.start()
        assert worker.state == "serving"
        control.active = 1
        eventually(lambda: worker.active_sessions == 1)
        control.active = 0
        eventually(lambda: worker.active_sessions == 0)
        assert worker._had_sessions
        assert worker.release_idle_blocks()
        assert worker._blocks_idle
        assert backend.revokes == 1
        # A second release is a no-op, not a double free.
        assert not worker.release_idle_blocks()
        assert backend.revokes == 1
    finally:
        worker.close()


def test_a_drain_request_keeps_the_blocks_for_the_drain_path(monkeypatch):
    worker, control, _allocator, _backend = new_worker(monkeypatch)
    try:
        worker.start()
        control.active = 1
        eventually(lambda: worker.active_sessions == 1)
        control.active = 0
        eventually(lambda: worker.active_sessions == 0)
        control.action = "drain"
        eventually(lambda: worker._drain_requested.is_set())
        worker.once()
        assert worker.state in ("draining", "drained")
        assert not worker._blocks_idle, "the drain path owns the release"
    finally:
        worker.close()


def test_a_window_under_the_floor_refuses_the_rdma_source(monkeypatch):
    worker, control, _allocator, backend = new_worker(
        monkeypatch, avail_ram=GIB, max_memory_bytes=8 * GIB
    )
    thread = threading.Thread(target=worker.start, daemon=True)
    try:
        thread.start()
        eventually(lambda: worker.active_sessions == 0)
        assert worker.state == "starting", "the floor waits at 1 GiB available"
        assert thread.is_alive()
        assert not worker.blocks
        assert control.filled_data is None
        assert backend.registrations == 0
        control.action = "drain"
        thread.join(timeout=2)
        assert not thread.is_alive()
        worker.once()
        assert worker.state == "drained"
        assert control.acks == [True]
    finally:
        worker._abort_fill_requested.set()
        thread.join(timeout=2)
        worker.close()


def test_a_window_over_the_floor_serves(monkeypatch):
    worker, control, _allocator, _backend = new_worker(
        monkeypatch, avail_ram=20 * GIB, max_memory_bytes=8 * GIB
    )
    try:
        worker.start()
        assert worker.state == "serving"
        assert worker.blocks
        assert control.filled_data is not None
    finally:
        worker.close()


def test_an_unreadable_memory_figure_gates_nothing(monkeypatch):
    worker, _control, _allocator, _backend = new_worker(
        monkeypatch, avail_ram=None, max_memory_bytes=8 * GIB
    )
    try:
        worker.start()
        assert worker.state == "serving"
        assert worker.blocks
    finally:
        worker.close()


@pytest.mark.parametrize(
    ("avail_ram", "max_bytes", "min_bytes", "want_window", "want_ok"),
    [
        (20 * GIB, 8 * GIB, GIB, 8 * GIB, True),
        (10 * GIB, 8 * GIB, GIB, 6 * GIB, True),
        (5 * GIB, 8 * GIB, GIB, 8 * GIB * 3 // 4 * 3 // 4, True),
        (1200 * MIB, 8 * GIB, GIB, 1092 * MIB, True),
        (1030 * MIB, 8 * GIB, GIB, GIB, True),
        (GIB, 8 * GIB, GIB, 0, False),
        (512 * MIB, 8 * GIB, GIB, 0, False),
        (None, 8 * GIB, GIB, 0, False),
        (4 * GIB, 512 * MIB, 512 * MIB, GIB, True),
        (2 * GIB, 8 * GIB, 512 * MIB, 1944 * MIB, True),
    ],
)
def test_compute_dynamic_source_buffer_mirrors_go(
    avail_ram, max_bytes, min_bytes, want_window, want_ok
):
    window, ok = compute_dynamic_source_buffer(avail_ram, max_bytes, min_bytes)
    assert ok is want_ok
    assert window == want_window


def test_min_source_buffer_floor_is_one_gib():
    assert MIN_SOURCE_BUFFER_BYTES == 1 << 30


@pytest.mark.parametrize("stage", ["allocate", "fill", "register", "descriptors"])
def test_restore_failure_retains_every_acquired_block(monkeypatch, stage):
    worker, _control, allocator, backend = new_worker(monkeypatch)
    try:
        worker.start()
        eventually(lambda: worker.active_sessions == 0)
        assert worker.release_idle_blocks()
        acquired = []
        if stage == "allocate":
            descriptor = worker.assignment.manifest.files[0]
            monkeypatch.setattr(
                "gpustack_model_transfer.source.native_worker.resident_files",
                lambda _, budget, **kwargs: (
                    descriptor,
                    replace(descriptor, path="second.safetensors"),
                ),
            )
        allocate = allocator.allocate
        register = backend.register

        def failing_allocate(name, size):
            if stage == "allocate" and acquired:
                raise RuntimeError("allocation failed")
            block = allocate(name, size)
            acquired.append(block)
            return block

        def failing_fill(*args, **kwargs):
            if stage == "fill":
                raise RuntimeError("fill failed")

        def failing_register(blocks):
            if stage == "register":
                raise RuntimeError("register failed")
            result = register(blocks)
            if stage == "descriptors":
                result.descriptors = lambda _: []
            return result

        allocator.allocate = failing_allocate
        backend.register = failing_register
        monkeypatch.setattr(
            "gpustack_model_transfer.source.native_worker.fill_from_tree", failing_fill
        )
        worker._rebuild_blocks()
        assert worker.state == "quarantined"
        assert acquired
        assert set(worker.blocks.values()) == set(acquired)
        assert set(worker._retention.owner.blocks.values()) == set(acquired)
        assert worker._blocks_idle
        assert allocator.releases == len(worker.assignment.manifest.files)
    finally:
        worker.close()


@pytest.mark.parametrize("signal", ["unknown", "drain"])
def test_restore_interrupt_stops_before_registration(monkeypatch, signal):
    worker, _control, allocator, backend = new_worker(monkeypatch)
    try:
        worker.start()
        eventually(lambda: worker.active_sessions == 0)
        assert worker.release_idle_blocks()
        allocate = allocator.allocate

        def interrupted_allocate(name, size):
            block = allocate(name, size)
            if signal == "unknown":
                worker._unknown_event.set()
            else:
                worker._drain_requested.set()
            return block

        allocator.allocate = interrupted_allocate
        worker._rebuild_blocks()
        assert worker.state == "quarantined"
        assert worker._blocks_idle
        assert len(worker.blocks) == 1
        assert backend.registrations == 1
    finally:
        worker.close()


@pytest.mark.parametrize("draining", [False, True])
def test_operation_submitted_while_idle_queues_and_rebuilds_blocks(
    monkeypatch, draining
):
    from gpustack_model_transfer.source.transfer_service import OperationRegistry

    worker, control, _allocator, backend = new_worker(monkeypatch)
    backend.post_write = lambda reg, name, off, length, target: bytes(
        reg.blocks[name].data[off : off + length]
    )
    backend.poll_write = lambda op: "succeeded"
    backend.close_write = lambda op: None
    supervisor = SimpleNamespace(
        workers={"generation-1": worker},
        worker_id="pod-uid",
        worker_instance_id="instance-1",
        _admission_closed=SimpleNamespace(is_set=lambda: False),
        _lock=worker._lock,
    )
    registry = OperationRegistry(supervisor)
    worker.operations = registry
    try:
        worker.start()
        worker.control.lease_id = "lease"
        control.active = 1
        eventually(lambda: worker.active_sessions == 1)
        control.active = 0
        eventually(lambda: worker.active_sessions == 0)
        assert worker.release_idle_blocks()
        assert worker._blocks_idle
        assert worker.blocks == {}

        tensor = worker.assignment.manifest.tensors[0]
        body = {
            "sourcePodUID": "pod-uid",
            "workerInstanceID": "instance-1",
            "generation": "generation-1",
            "leaseID": "lease",
            "sessionID": "session-after-idle",
            "requestID": "req-after-idle",
            "targetPodUID": "target",
            "targetInstanceID": "process",
            "registrationID": "registration",
            "windowID": "window",
            "tensor": tensor.name,
            "file": tensor.file,
            "fileOffset": tensor.data_begin,
            "tensorOffset": 0,
            "length": 4,
            "target": {
                "backend": "nixl",
                "address": 123,
                "length": 16,
                "metadata": "dGFyZ2V0",
                "endpoint": "",
            },
        }
        reply = registry.submit(body)
        assert reply["state"] == "dispatching"
        assert not registry.step(worker)
        if draining:
            worker._drain_requested.set()
            assert registry.step(worker)
            assert registry.jobs[body["requestID"]]["verdict"] == "not-posted"
            assert registry.jobs[body["requestID"]]["native"] is None
            assert backend.registrations == 1
            ack = {
                key: body[key]
                for key in (
                    "sourcePodUID",
                    "workerInstanceID",
                    "generation",
                    "leaseID",
                    "sessionID",
                    "requestID",
                    "targetPodUID",
                    "targetInstanceID",
                    "registrationID",
                    "windowID",
                )
            }
            registry.acknowledge(ack)
            assert registry.step(worker)
            assert worker.native_operations == 0
            return
        assert worker.once() == "serving"
        assert not worker._blocks_idle
        assert worker.blocks
        assert worker.registration is not None
        assert worker.once() == "serving"
        assert (
            registry.status(
                {
                    k: body[k]
                    for k in (
                        "sourcePodUID",
                        "workerInstanceID",
                        "generation",
                        "leaseID",
                        "sessionID",
                        "requestID",
                    )
                }
            )["state"]
            == "terminal"
        )
    finally:
        worker.close()
