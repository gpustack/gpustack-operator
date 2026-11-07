"""CPU-injected tests for the real source supervisor and ownership protocol."""

import gc
import hashlib
import json
import threading
import time
import weakref
from pathlib import Path

import pytest

from gpustack_model_transfer.source.backend import SourceRegistration
from gpustack_model_transfer.source.control import ControlError
from gpustack_model_transfer.source.fill import resolve_tree_file
from gpustack_model_transfer.source.supervisor import SourceSupervisor
from gpustack_model_transfer.source.workorder import parse_assignment

FIXTURE = Path(__file__).resolve().parents[3] / "contracts/fixtures/tiny_dense"


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
    def __init__(self, size):
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
    def __init__(self, control, *, slow=False):
        assert control.native_ack
        self.slow = slow
        self.releases = 0

    def allocate(self, name, size):
        if self.slow:
            time.sleep(0.15)
        return Block(size)

    def release(self, block):
        self.releases += 1


class Backend:
    def __init__(self, control, fail_revoke=False):
        assert control.native_ack
        self.releases = 0
        self.fail_revoke = fail_revoke

    def start(self):
        pass

    def register(self, blocks):
        return SourceRegistration(
            "nixl", {name: object() for name in blocks}, dict(blocks)
        )

    def disconnect(self, registration):
        pass

    def revoke(self, registration):
        self.releases += 1
        if self.fail_revoke:
            raise RuntimeError("unknown")
        registration.handles.clear()


class Control:
    def __init__(self, generation):
        self.generation = generation
        self.worker_id = "pod-uid"
        self.worker_instance_id = "instance-1"
        self.native_ack = False
        self.start_calls = 0
        self.heartbeat_calls = 0
        self.ack_calls = 0
        self.filled_data = None
        self.active = 0
        self.unknown = 0
        self.action = "none"
        self.lose_start = False
        self.lose_ack = False
        self.stale = False

    def register(self):
        return "lease"

    def native_start(self):
        self.start_calls += 1
        if self.lose_start and self.start_calls == 1:
            raise ControlError(0, "unavailable", "response lost")
        self.native_ack = True

    def heartbeat(self, **kwargs):
        if kwargs.get("drain_requested"):
            self.action = "drain"
        self.heartbeat_calls += 1
        if self.stale:
            raise ControlError(409, "stale-generation", "stale")
        return {
            "action": self.action,
            "activeSessions": self.active,
            "unknownSessions": self.unknown,
        }

    def filled(self, registrations, descriptors):
        self.filled_data = (registrations, descriptors)

    def drain_ack(self, native_released):
        assert native_released is True
        self.ack_calls += 1
        if self.lose_ack and self.ack_calls == 1:
            raise ControlError(0, "unavailable", "response lost")

    def quarantine(self, reason):
        pass

    def close(self):
        pass


class Bootstrap:
    def __init__(self, orders):
        self.orders = orders

    def assignments(self):
        return self.orders


def eventually(condition, timeout=3):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if condition():
            return
        time.sleep(0.005)
    raise AssertionError("condition timed out")


def supervisor(orders, *, slow=False, lose_start=False, fail_revoke=False):
    controls, backends, allocators = {}, {}, {}

    def control_for(assignment):
        c = Control(assignment.generation)
        c.lose_start = lose_start
        controls[assignment.generation] = c
        return c

    def backend_for(assignment):
        b = Backend(controls[assignment.generation], fail_revoke)
        backends[assignment.generation] = b
        return b

    def allocator_for(assignment):
        a = Allocator(controls[assignment.generation], slow=slow)
        allocators[assignment.generation] = a
        return a

    s = SourceSupervisor(
        Bootstrap(orders),
        control_factory=control_for,
        backend_factory=backend_for,
        allocator_factory=allocator_for,
        worker_id="pod-uid",
        backend="nixl",
        poll_interval=0.01,
    )
    return s, controls, backends, allocators


def test_fill_two_generations_and_drain_after_sessions_and_lost_ack():
    s, controls, backends, allocators = supervisor(
        [order("one"), order("two")], slow=True, lose_start=True
    )
    assert s.poll_once() == 2
    assert s.poll_once() == 0
    eventually(lambda: all(w.wait_until_filled(0) for w in s.workers.values()))
    for generation, worker in s.workers.items():
        c = controls[generation]
        assert c.start_calls == 2
        assert c.heartbeat_calls >= 3
        assert (
            bytes(next(iter(worker.blocks.values())).data)
            == (FIXTURE / "checkpoint.safetensors").read_bytes()
        )
        count, descriptors = c.filled_data
        assert count == 1
        assert descriptors[0]["length"] == order()["totalBytes"]
        assert descriptors[0]["workerUID"] == "pod-uid"
        assert worker.fill_report.header_and_gap_bytes > 0
        c.active, c.action, c.lose_ack = 1, "drain", True
    eventually(lambda: all(w.active_sessions == 1 for w in s.workers.values()))
    assert all(b.releases == 0 for b in backends.values())
    for c in controls.values():
        c.active, c.unknown = 0, 1
    eventually(lambda: all(w.unknown_sessions == 1 for w in s.workers.values()))
    assert all(b.releases == 0 for b in backends.values())
    for c in controls.values():
        c.unknown = 0
    eventually(lambda: all(w.state == "drained" for w in s.workers.values()))
    assert all(c.ack_calls == 2 for c in controls.values())
    assert all(b.releases == 1 for b in backends.values())
    assert all(a.releases == 1 for a in allocators.values())


def test_revoke_failure_and_stale_retain_owned_blocks():
    s, controls, _, _ = supervisor([order("one"), order("two")], fail_revoke=True)
    s.poll_once()
    eventually(lambda: all(w.wait_until_filled(0) for w in s.workers.values()))
    refs = [weakref.ref(next(iter(w.blocks.values()))) for w in s.workers.values()]
    controls["one"].action = "drain"
    controls["two"].stale = True
    eventually(lambda: all(w.state == "quarantined" for w in s.workers.values()))
    assert all(c.ack_calls == 0 for c in controls.values())
    for w in s.workers.values():
        w.close()
    gc.collect()
    assert all(ref() is not None for ref in refs)
    for w in s.workers.values():
        w._stop_event.set()


def test_refuse_bad_orders_without_native_owners_and_conflicting_revision():
    malformed = order("bad")
    malformed["manifestSHA256"] = "0" * 64
    s, controls, _, _ = supervisor([malformed, order("same")])
    assert s.poll_once() == 1
    assert "bad" not in controls
    other = order("same")
    other["digest"] = "sha256:" + "b" * 64
    s.bootstrap.orders = [other]
    assert s.poll_once() == 0
    eventually(lambda: s.workers["same"].wait_until_filled(0))
    controls["same"].action = "drain"
    eventually(lambda: s.workers["same"].state == "drained")


def test_hash_identity_and_symlink_rejected(tmp_path):
    a = parse_assignment(dict(order(), residentBudgetBytes=4 << 20), backend="hixl")
    assert a.digest != "sha256:" + a.manifest_sha256
    (tmp_path / "dir").symlink_to(FIXTURE, target_is_directory=True)
    with pytest.raises(Exception):
        resolve_tree_file(str(tmp_path), "dir/checkpoint.safetensors")


def test_ancillary_file_is_not_charged_or_registered(tmp_path):
    data = order()
    manifest = json.loads(data["manifestJSON"])
    manifest["files"].append(
        {
            "path": "config.json",
            "size": 3,
        }
    )
    raw = json.dumps(manifest, separators=(",", ":")).encode()
    data["manifestJSON"] = raw.decode()
    data["manifestSHA256"] = hashlib.sha256(raw).hexdigest()
    s, controls, _, _ = supervisor([data])
    assert s.poll_once() == 1
    eventually(lambda: s.workers[data["generation"]].wait_until_filled(0))
    assert len(s.workers[data["generation"]].blocks) == 1
    controls[data["generation"]].action = "drain"
    eventually(lambda: s.workers[data["generation"]].state == "drained")


def test_fake_torch_pinned_owner_reads_directly(tmp_path):
    import ctypes
    import os

    from gpustack_model_transfer.source.memory import TorchPinnedOwner

    class Tensor:
        def __init__(self, size):
            self.storage = ctypes.create_string_buffer(size)

        def is_pinned(self):
            return True

        def data_ptr(self):
            return ctypes.addressof(self.storage)

    class Torch:
        uint8 = object()

        @staticmethod
        def empty(size, *, dtype, pin_memory):
            assert dtype is Torch.uint8 and pin_memory is True
            return Tensor(size)

    owner = TorchPinnedOwner(Torch)
    block = owner.allocate("fixture", 4)
    path = tmp_path / "input"
    path.write_bytes(b"data")
    fd = os.open(path, os.O_RDONLY)
    try:
        assert os.readv(fd, [block.view(0, 4)]) == 4
    finally:
        os.close(fd)
    assert block.read_bytes(0, 4) == b"data"
    from gpustack_model_transfer.backends.nixl.runtime import _buffer_extent

    assert _buffer_extent(block) == (4, "DRAM", 0)
    owner.release(block)


def test_fake_acl_context_is_bound_and_shared_until_last_owner():
    import ctypes

    from gpustack_model_transfer.source.memory import AclHostOwner

    class Function:
        def __init__(self, call):
            self.call = call

        def __call__(self, *args):
            return self.call(*args)

    class Library:
        def __init__(self):
            self.calls = []
            self.storage = {}
            for name in (
                "aclInit",
                "aclrtSetDevice",
                "aclrtSetCurrentContext",
                "aclrtDestroyContext",
                "aclrtResetDevice",
                "aclFinalize",
                "aclrtFreeHost",
            ):
                setattr(
                    self, name, Function(lambda *args, name=name: self.record(name))
                )
            self.aclrtCreateContext = Function(self.create)
            self.aclrtMallocHost = Function(self.alloc)

        def record(self, name):
            self.calls.append(name)
            return 0

        def create(self, ptr, device):
            self.calls.append("create")
            ctypes.cast(ptr, ctypes.POINTER(ctypes.c_void_p))[0] = ctypes.c_void_p(42)
            return 0

        def alloc(self, ptr, size):
            buf = ctypes.create_string_buffer(size.value)
            address = ctypes.addressof(buf)
            self.storage[address] = buf
            ctypes.cast(ptr, ctypes.POINTER(ctypes.c_void_p))[0] = ctypes.c_void_p(
                address
            )
            return 0

    lib = Library()
    first = AclHostOwner(loader=lambda _: lib)
    ready, release = threading.Event(), threading.Event()
    errors = []

    def other_generation():
        try:
            second = AclHostOwner(loader=lambda _: lib)
            block = second.allocate("test", 4)
            block.view(0, 4)[:] = b"test"
            ready.set()
            assert release.wait(2)
            assert block.read_bytes(0, 4) == b"test"
            second.release(block)
            second.dispose()
        except BaseException as exc:
            errors.append(exc)
            ready.set()

    thread = threading.Thread(target=other_generation)
    thread.start()
    assert ready.wait(2)
    first.dispose()
    assert "aclrtDestroyContext" not in lib.calls
    release.set()
    thread.join(timeout=2)
    assert not thread.is_alive() and not errors
    assert lib.calls.count("aclrtSetCurrentContext") == 2
    assert lib.calls.count("aclrtDestroyContext") == 1
    assert lib.calls.count("aclFinalize") == 1


def test_source_module_refuses_missing_identity_in_new_process():
    import os
    import subprocess
    import sys

    env = os.environ.copy()
    env.pop("KUBERNETES_POD_UID", None)
    process = subprocess.run(
        [sys.executable, "-m", "gpustack_model_transfer.source"],
        env=env,
        capture_output=True,
        text=True,
        check=False,
        timeout=10,
    )
    assert process.returncode == 2
    assert "KUBERNETES_POD_UID" in process.stderr


def test_hixl_partial_registration_retains_all_owned_memory():
    from gpustack_model_transfer.source.backend import (
        HixlSourceBackend,
        SourceStateRetainedError,
    )

    class Engine:
        def __init__(self):
            self.calls = 0

        def initialize(self, address, options):
            return 0

        def register_mem(self, desc, kind):
            self.calls += 1
            return (0, 42) if self.calls == 1 else (1, 0)

    engine = Engine()
    backend = HixlSourceBackend(
        engine_factory=lambda: engine, engine_address="127.0.0.1:1000"
    )
    backend.start()
    blocks = {"one": Block(3), "two": Block(4)}
    refs = [weakref.ref(block) for block in blocks.values()]
    with pytest.raises(SourceStateRetainedError) as error:
        backend.register(blocks)
    assert error.value.retainment.handles == {"one": 42}
    del blocks
    gc.collect()
    assert all(ref() is not None for ref in refs)
    assert error.value.retainment.backend is backend


def fake_process_smoke():
    s, controls, _, _ = supervisor([order()])
    assert s.poll_once() == 1
    eventually(lambda: s.workers["generation-1"].wait_until_filled(0))
    controls["generation-1"].action = "drain"
    eventually(lambda: s.workers["generation-1"].state == "drained")
    print("fake-supervisor-drained")


def test_explicit_fake_helper_starts_a_supervisor_in_a_new_process():
    import subprocess
    import sys

    result = subprocess.run(
        [sys.executable, __file__, "--fake-supervisor-smoke"],
        capture_output=True,
        text=True,
        check=False,
        timeout=10,
    )
    assert result.returncode == 0, result.stderr
    assert "fake-supervisor-drained" in result.stdout


if __name__ == "__main__":
    import sys

    if sys.argv[1:] == ["--fake-supervisor-smoke"]:
        fake_process_smoke()


def test_real_nixl_registration_adapter_uses_owned_full_extent():
    import ctypes

    from gpustack_model_transfer.backends.nixl.runtime import RealNixlAPI
    from gpustack_model_transfer.source.backend import NixlSourceBackend
    from gpustack_model_transfer.source.memory import HostBlock

    class Pinned(HostBlock):
        def __init__(self):
            super().__init__("file", 8, None)
            self.storage = ctypes.create_string_buffer(8)

        def data_ptr(self):
            return ctypes.addressof(self.storage)

    class Agent:
        def __init__(self):
            self.calls = []

        def register_memory(self, descriptors, kind):
            self.calls.append((descriptors, kind))
            return "handle"

        def deregister_memory(self, handle):
            self.calls.append(handle)

    class API(RealNixlAPI):
        def __init__(self):
            super().__init__()
            self._nixl_api = object()
            self.agent = Agent()

        def create_agent(self, name, settings):
            assert settings == {"enable_listen_thread": False}
            return self.agent

    api = API()
    backend = NixlSourceBackend(api)
    backend.start()
    block = Pinned()
    registration = backend.register({"file": block})
    assert api.agent.calls[0] == ([(block.data_ptr(), 8, 0, "")], "DRAM")
    backend.revoke(registration)
    assert api.agent.calls[-1] == "handle"
    assert registration.handles == {}


def test_missing_assignment_does_not_release_live_generation():
    s, controls, backends, _ = supervisor([order()])
    s.poll_once()
    eventually(lambda: s.workers["generation-1"].wait_until_filled(0))
    s.bootstrap.orders = []
    s.poll_once()
    assert "generation-1" in s.workers
    assert backends["generation-1"].releases == 0
    assert not s.stop(timeout=0.01)
    controls["generation-1"].action = "drain"
    eventually(lambda: s.workers["generation-1"].state == "drained")


def test_drain_during_allocation_releases_then_acknowledges():
    s, controls, backends, allocators = supervisor([order()], slow=True)
    s.poll_once()
    eventually(lambda: controls["generation-1"].native_ack)
    controls["generation-1"].action = "drain"
    eventually(lambda: s.workers["generation-1"].state == "drained")
    assert backends["generation-1"].releases == 0
    assert allocators["generation-1"].releases == 1
    assert controls["generation-1"].ack_calls == 1


def test_stale_during_deregistration_does_not_free_or_ack():
    s, controls, backends, allocators = supervisor([order()])
    s.poll_once()
    eventually(lambda: s.workers["generation-1"].wait_until_filled(0))
    entered, resume = threading.Event(), threading.Event()
    backend = backends["generation-1"]
    original_revoke = backend.revoke

    def revoke(registration):
        entered.set()
        assert resume.wait(2)
        original_revoke(registration)

    backend.revoke = revoke
    controls["generation-1"].action = "drain"
    assert entered.wait(2)
    controls["generation-1"].stale = True
    eventually(lambda: s.workers["generation-1"]._unknown_event.is_set())
    resume.set()
    eventually(lambda: s.workers["generation-1"].state == "quarantined")
    s.threads["generation-1"].join(timeout=2)
    assert allocators["generation-1"].releases == 0
    assert controls["generation-1"].ack_calls == 0
    s.workers["generation-1"]._stop_event.set()


def test_truncated_file_never_reports_filled(tmp_path):
    target = tmp_path / "checkpoint.safetensors"
    payload = bytearray((FIXTURE / "checkpoint.safetensors").read_bytes())
    del payload[-1]
    target.write_bytes(payload)
    s, controls, _, _ = supervisor([order(tree=tmp_path)])
    s.poll_once()
    worker = s.workers["generation-1"]
    eventually(lambda: worker.state == "quarantined")
    assert controls["generation-1"].filled_data is None
    assert worker.blocks
    worker._stop_event.set()


def test_hixl_source_factory_requires_no_torch(monkeypatch):
    import sys

    from gpustack_model_transfer.source.__main__ import build_factories

    monkeypatch.setitem(sys.modules, "torch", None)
    monkeypatch.setitem(sys.modules, "torch_npu", None)
    backend, _, _ = build_factories(
        {
            "GPUSTACK_MODEL_TRANSFER_BACKEND": "hixl",
            "GPUSTACK_MODEL_TRANSFER_VENDOR_SCOPE": "ascend",
            "KUBERNETES_NODE_NAME": "node",
            "KUBERNETES_POD_IP": "127.0.0.1",
        }
    )
    assert backend == "hixl"


@pytest.mark.parametrize("outcome", ["recover", "drain", "close"])
def test_memory_refusal_keeps_generation_retriable_and_drainable(monkeypatch, outcome):
    from gpustack_model_transfer.source.native_worker import NativeSourceWorker

    available = threading.Event()
    checked = threading.Event()

    def admit(worker):
        checked.set()
        return available.is_set()

    monkeypatch.setattr(NativeSourceWorker, "_admit_dynamic_window", admit)
    source, controls, backends, allocators = supervisor([order()])
    source.poll_once()
    worker = source.workers["generation-1"]
    try:
        assert checked.wait(2)
        assert worker.state == "starting"
        assert source.threads["generation-1"].is_alive()
        assert not backends and not allocators
        assert controls["generation-1"].start_calls == 0
        if outcome == "close":
            worker.close()
            eventually(lambda: worker.state == "stopped")
            assert controls["generation-1"].ack_calls == 0
            return
        if outcome == "recover":
            available.set()
            eventually(lambda: worker.state == "serving")
            assert controls["generation-1"].start_calls == 1
        controls["generation-1"].action = "drain"
        eventually(lambda: worker.state == "drained")
        assert controls["generation-1"].ack_calls == 1
    finally:
        controls["generation-1"].action = "drain"
        worker._abort_fill_requested.set()
        source.stop(timeout=2)
        worker.close()


@pytest.mark.parametrize("value", [True, 1.0, "1", [], None])
def test_work_order_source_generation_requires_integer(value):
    payload = order()
    payload["sourceGeneration"] = value
    from gpustack_model_transfer.core.errors import InvalidDescriptorError

    with pytest.raises(InvalidDescriptorError, match="source generation"):
        parse_assignment(payload, backend="nixl")


@pytest.mark.parametrize(
    "namespace", [None, {}, "x" * 254, "bad\nnamespace", "bad\x7fnamespace"]
)
def test_workorder_namespace_is_log_safe(namespace):
    from gpustack_model_transfer.core.errors import InvalidDescriptorError

    payload = order()
    payload["namespace"] = namespace
    with pytest.raises(InvalidDescriptorError):
        parse_assignment(payload, backend="nixl")


def test_failed_start_does_not_starve_other_assignments_or_retries():
    s, controls, _, _ = supervisor([order("one"), order("two")])
    original = s._control_factory
    attempts = []

    def failing_once(assignment):
        attempts.append(assignment.generation)
        if assignment.generation == "one" and attempts.count("one") == 1:
            raise RuntimeError("temporary factory failure")
        return original(assignment)

    s._control_factory = failing_once
    try:
        s.poll_once()
        assert "one" not in s.workers
        assert "two" in s.workers
        s.poll_once()
        eventually(
            lambda: (
                len(s.workers) == 2
                and all(w.wait_until_filled(0) for w in s.workers.values())
            )
        )
        assert attempts.count("one") == 2
    finally:
        for control in controls.values():
            control.action = "drain"
        assert s.stop()
