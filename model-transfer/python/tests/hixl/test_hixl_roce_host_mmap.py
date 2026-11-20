"""Real CPU anonymous mmap tests for Ascend A5 Host RoCE host owners."""

from __future__ import annotations

import ctypes
import gc
import json
import mmap
import sys
import threading
import weakref
from types import SimpleNamespace

import pytest

import gpustack_model_transfer.client_native as client_native
from gpustack_model_transfer.backends.hixl import runtime as hixl_runtime
from gpustack_model_transfer.client_native import BorrowedAclOwner, TargetNative
from gpustack_model_transfer.core.budgets import (
    MEMORY_ALIGNMENT_BYTES,
    align_up_2m,
    hixl_allocation_bytes,
)
from gpustack_model_transfer.core.errors import SourceUnavailableError
from gpustack_model_transfer.source import memory as source_memory
from gpustack_model_transfer.source.backend import HixlSourceBackend
from gpustack_model_transfer.source.memory import AclHostOwner


@pytest.fixture(autouse=True)
def clean_ascend_comm_env(monkeypatch):
    monkeypatch.delenv("ASCEND_LOCAL_COMM_RES", raising=False)
    monkeypatch.delenv("ASCEND_GLOBAL_RESOURCE_CONFIG", raising=False)


class _Func:
    def __init__(self, fn):
        self.fn = fn

    def __call__(self, *args):
        return self.fn(*args)


class RecordingAclLib:
    """Fake ACL runtime library recording host malloc/free calls."""

    def __init__(self, *, forbid_malloc: bool = False):
        self.context = 0xABC
        self.forbid_malloc = forbid_malloc
        self.calls: list[str] = []
        self.allocations: dict[int, ctypes.Array] = {}
        self.aclInit = _Func(self._acl_init)
        self.aclrtSetDevice = _Func(self._set_device)
        self.aclrtCreateContext = _Func(self._create_context)
        self.aclrtSetCurrentContext = _Func(self._set_current_context)
        self.aclrtGetCurrentContext = _Func(self._get_current_context)
        self.aclrtDestroyContext = _Func(self._destroy_context)
        self.aclrtResetDevice = _Func(self._reset_device)
        self.aclFinalize = _Func(self._finalize)
        self.aclrtMallocHost = _Func(self._malloc_host)
        self.aclrtFreeHost = _Func(self._free_host)

    def _acl_init(self, _cfg):
        self.calls.append("aclInit")
        return 0

    def _set_device(self, _dev):
        self.calls.append("aclrtSetDevice")
        return 0

    def _create_context(self, out_ptr, _dev):
        self.calls.append("aclrtCreateContext")
        ctypes.cast(out_ptr, ctypes.POINTER(ctypes.c_void_p))[0] = ctypes.c_void_p(
            self.context
        )
        return 0

    def _set_current_context(self, _ctx):
        self.calls.append("aclrtSetCurrentContext")
        return 0

    def _get_current_context(self, out_ptr):
        self.calls.append("aclrtGetCurrentContext")
        ctypes.cast(out_ptr, ctypes.POINTER(ctypes.c_void_p))[0] = ctypes.c_void_p(
            self.context
        )
        return 0

    def _destroy_context(self, _ctx):
        self.calls.append("aclrtDestroyContext")
        return 0

    def _reset_device(self, _dev):
        self.calls.append("aclrtResetDevice")
        return 0

    def _finalize(self):
        self.calls.append("aclFinalize")
        return 0

    def _malloc_host(self, out_ptr, length):
        self.calls.append("aclrtMallocHost")
        if self.forbid_malloc:
            raise AssertionError("aclrtMallocHost must not run under roce:host")
        size = int(getattr(length, "value", length))
        buf = (ctypes.c_ubyte * size)()
        ctypes.memset(ctypes.addressof(buf), 0xCD, size)
        addr = ctypes.addressof(buf)
        self.allocations[addr] = buf
        ctypes.cast(out_ptr, ctypes.POINTER(ctypes.c_void_p))[0] = ctypes.c_void_p(addr)
        return 0

    def _free_host(self, ptr):
        self.calls.append("aclrtFreeHost")
        addr = int(getattr(ptr, "value", ptr))
        del self.allocations[addr]
        return 0


def _make_owner(kind: str, acl: RecordingAclLib):
    if kind == "source":
        return AclHostOwner(loader=lambda _: acl)
    return BorrowedAclOwner(loader=lambda _: acl)


def _dispose_owner(kind: str, owner) -> None:
    if kind == "source":
        owner.dispose()


class TestRoceHostAnonymousMmap:
    @pytest.mark.parametrize("owner_kind", ["source", "borrowed"])
    @pytest.mark.parametrize(
        "byte_size",
        [1, 4097, (2 << 20), (2 << 20) + 17],
    )
    def test_roce_host_allocates_aligned_anonymous_mmap_and_zeros_padding_tail(
        self, monkeypatch, owner_kind, byte_size
    ):
        monkeypatch.setenv(
            "ASCEND_GLOBAL_RESOURCE_CONFIG",
            json.dumps({"comm_resource_config.protocol_desc": ["roce:host"]}),
        )
        acl = RecordingAclLib(forbid_malloc=True)
        owner = _make_owner(owner_kind, acl)
        block = owner.allocate("weights.safetensors", byte_size)
        try:
            assert block.allocator_kind == "mmap"
            assert isinstance(block.mapping, mmap.mmap)
            assert not block.mapping.closed
            assert block.byte_size == byte_size
            assert block.registration_bytes == align_up_2m(byte_size)
            assert block.allocation_bytes == hixl_allocation_bytes(byte_size)
            assert 0 <= block.aligned_offset < MEMORY_ALIGNMENT_BYTES
            assert block.data_ptr() == block.address == block.aligned_address
            assert block.data_ptr() % MEMORY_ALIGNMENT_BYTES == 0
            assert block.allocation_address + block.aligned_offset == block.data_ptr()
            assert (
                block.aligned_offset + block.registration_bytes
                <= block.allocation_bytes
            )

            # Write payload and check that registration padding tail is zero-filled.
            sample = b"A5-ROCE" if byte_size >= 7 else b"Z"
            block.write_bytes(0, sample)
            assert block.read_bytes(0, len(sample)) == sample
            tail_len = block.registration_bytes - byte_size
            if tail_len > 0:
                assert ctypes.string_at(
                    block.data_ptr() + byte_size, min(tail_len, 4096)
                ) == bytes(min(tail_len, 4096))
                assert ctypes.string_at(
                    block.data_ptr() + block.registration_bytes - min(tail_len, 64),
                    min(tail_len, 64),
                ) == bytes(min(tail_len, 64))

            view = block.view(0, len(sample))
            assert view.obj is block.mapping
            assert bytes(view) == sample
            view.release()

            with pytest.raises(ValueError):
                block.write_bytes(byte_size, b"!")
            with pytest.raises(ValueError):
                block.view(byte_size, 1)

            assert "aclrtMallocHost" not in acl.calls
            assert "aclrtFreeHost" not in acl.calls
        finally:
            owner.release(block)
            _dispose_owner(owner_kind, owner)
        assert block.address == 0
        assert "aclrtFreeHost" not in acl.calls

    @pytest.mark.parametrize("owner_kind", ["source", "borrowed"])
    @pytest.mark.parametrize(
        "protocol_cfg",
        [
            "",
            json.dumps({"comm_resource_config.protocol_desc": ["ub_ctp:device"]}),
            json.dumps({"comm_resource_config.protocol_desc": ["ub_ctp:host"]}),
            json.dumps({"comm_resource_config.protocol_desc": ["ub_rtp:device"]}),
            json.dumps({"comm_resource_config.protocol_desc": ["uboe:device"]}),
        ],
    )
    def test_non_roce_host_keeps_aclrt_malloc_host(
        self, monkeypatch, owner_kind, protocol_cfg
    ):
        if protocol_cfg:
            monkeypatch.setenv("ASCEND_GLOBAL_RESOURCE_CONFIG", protocol_cfg)
        acl = RecordingAclLib()
        owner = _make_owner(owner_kind, acl)
        block = owner.allocate("window", 64)
        try:
            assert block.allocator_kind == "acl"
            assert block.mapping is None
            assert "aclrtMallocHost" in acl.calls
            assert len(acl.allocations) == 1
        finally:
            owner.release(block)
            _dispose_owner(owner_kind, owner)
        assert "aclrtFreeHost" in acl.calls
        assert not acl.allocations

    def test_source_and_target_register_aligned_mmap_and_copy_nonzero_offset_frombuffer(
        self, monkeypatch, tmp_path
    ):
        ep_file = tmp_path / "ub_endpoint_npu_0.json"
        ep_file.write_text('{"phy":0}')
        monkeypatch.setenv(
            "ASCEND_GLOBAL_RESOURCE_CONFIG",
            json.dumps(
                {
                    "comm_resource_config.protocol_desc": ["roce:host"],
                    "local_comm_res_path": str(ep_file),
                }
            ),
        )
        acl = RecordingAclLib(forbid_malloc=True)
        monkeypatch.setattr(
            client_native, "BorrowedAclOwner", lambda: BorrowedAclOwner(lambda _: acl)
        )

        events: list[str] = []
        registered: list[tuple[int, int, str]] = []

        class Client:
            def initialize(self, endpoint, options):
                events.append("initialize")
                return 0

            def register_mem(self, desc, mem_type):
                events.append("register")
                registered.append((desc[0], desc[1], mem_type))
                return 0, 88

            def deregister_mem(self, handle):
                events.append("deregister")
                return 0

            def finalize(self):
                events.append("finalize")
                return 0

        fake_hixl = SimpleNamespace(
            Hixl=Client,
            SUCCESS=0,
            MemDesc=lambda addr, length: (addr, length),
            MemType=SimpleNamespace(MEM_HOST="MEM_HOST", MEM_DEVICE="MEM_DEVICE"),
            TransferOp=SimpleNamespace(READ="READ", WRITE="WRITE"),
            TransferStatus=SimpleNamespace(
                COMPLETED="COMPLETED", FAILED="FAILED", TIMEOUT="TIMEOUT"
            ),
        )
        real_api = hixl_runtime.RealHixlAPI()
        real_api._hixl = fake_hixl
        monkeypatch.setattr(hixl_runtime, "load_runtime", lambda: real_api)

        captured_views = []

        class FakeTensor:
            def __init__(self, buf, dev_type="npu"):
                self.buf = buf
                self.device = SimpleNamespace(type=dev_type, index=0)

            def is_contiguous(self):
                return True

            def view(self, _dtype):
                return self

            def reshape(self, _shape):
                return self

            def numel(self):
                return len(self.buf)

            def __getitem__(self, slc):
                return FakeTensor(memoryview(self.buf)[slc], self.device.type)

            def copy_(self, src, *, non_blocking=False):
                assert non_blocking is True
                self.buf[:] = src.buf

        def frombuffer(buffer, *, dtype):
            assert dtype == "uint8"
            captured_views.append(buffer)
            return FakeTensor(buffer, "cpu")

        fake_torch = SimpleNamespace(
            uint8="uint8",
            frombuffer=frombuffer,
            npu=SimpleNamespace(synchronize=lambda _dev: events.append("synchronize")),
        )
        monkeypatch.setitem(sys.modules, "torch", fake_torch)

        native = TargetNative("hixl", "127.0.0.1", 6 << 20)
        native.start()
        mapping = native.block.mapping
        assert isinstance(mapping, mmap.mmap)
        assert registered == [
            (native.block.data_ptr(), align_up_2m(native.size), "MEM_HOST")
        ]
        assert native.descriptor()["address"] == native.block.data_ptr()
        assert native.descriptor()["length"] == native.size

        # Simulate RDMA write directly into registered virtual address + nonzero window_offset.
        window_offset = 12345
        payload = b"safetensors-roce-host-window-slice"
        ctypes.memmove(native.block.data_ptr() + window_offset, payload, len(payload))

        dst_backing = bytearray(64)
        dst = FakeTensor(dst_backing, "npu")
        native.copy_to(dst, 8, len(payload), window_offset=window_offset)
        assert bytes(dst_backing[8 : 8 + len(payload)]) == payload
        assert len(captured_views) == 1
        assert captured_views[0].obj is mapping

        # Release the captured buffer reference so close() can unmap cleanly,
        # and verify mmap.close() runs strictly after synchronize/deregister/finalize.
        native._pending_devices[("npu", 0)] = SimpleNamespace(type="npu", index=0)
        orig_release = native.owner.release

        def tracked_release(blk):
            assert not mapping.closed, "mmap closed before owner.release()"
            assert events[-3:] == ["synchronize", "deregister", "finalize"]
            events.append("release")
            return orig_release(blk)

        native.owner.release = tracked_release
        captured_views.clear()
        dst = None
        gc.collect()
        native.close()
        assert mapping.closed
        assert events[-4:] == ["synchronize", "deregister", "finalize", "release"]

        # Source backend registration also pins aligned_address with registration_bytes.
        source_owner = AclHostOwner(loader=lambda _: acl)
        source_block = source_owner.allocate("model.safetensors", (2 << 20) + 99)
        source_descs = []
        source_backend = HixlSourceBackend()
        source_backend._engine = SimpleNamespace(
            register_mem=lambda desc, mtype: (
                source_descs.append((desc.addr, desc.len, mtype, desc.local_only)) or 0,
                55,
            )
        )
        source_backend._hixl = SimpleNamespace(
            MemDesc=lambda addr, length: SimpleNamespace(addr=addr, len=length),
            MemType=SimpleNamespace(MEM_HOST="MEM_HOST"),
            SUCCESS=0,
        )
        try:
            reg = source_backend.register({"model.safetensors": source_block})
            assert source_descs == [
                (source_block.data_ptr(), 4 << 20, "MEM_HOST", True)
            ]
            assert reg.descriptors("worker-1") == [
                {
                    "name": "model.safetensors",
                    "addr": source_block.data_ptr(),
                    "length": (2 << 20) + 99,
                    "memoryType": "HOST",
                    "backend": "hixl",
                    "workerUID": "worker-1",
                }
            ]
        finally:
            source_owner.release(source_block)
            source_owner.dispose()

    @pytest.mark.parametrize("owner_kind", ["source", "borrowed"])
    def test_release_dispatches_by_recorded_kind_across_env_flips_and_is_idempotent(
        self, monkeypatch, owner_kind
    ):
        acl = RecordingAclLib()
        owner = _make_owner(owner_kind, acl)
        roce_owner = None
        try:
            # 1) Allocate ACL block when roce:host is False.
            acl_block = owner.allocate("acl.bin", 128)
            assert acl_block.allocator_kind == "acl"
            assert len(acl.allocations) == 1

            # 2) Flip env to roce:host=True and allocate mmap block from a fresh owner.
            monkeypatch.setenv(
                "ASCEND_GLOBAL_RESOURCE_CONFIG",
                json.dumps({"comm_resource_config": {"protocol_desc": "roce:host"}}),
            )
            roce_owner = _make_owner(owner_kind, acl)
            mmap_block = roce_owner.allocate("mmap.bin", 128)
            assert mmap_block.allocator_kind == "mmap"
            mapping = mmap_block.mapping
            assert isinstance(mapping, mmap.mmap)

            # Release acl_block while roce:host is STILL True -> must call aclrtFreeHost once.
            free_before = acl.calls.count("aclrtFreeHost")
            owner.release(acl_block)
            owner.release(acl_block)
            assert acl.calls.count("aclrtFreeHost") == free_before + 1
            assert not acl.allocations

            # 3) Flip env back to ub_ctp:host (roce:host=False) and release mmap_block twice ->
            # must close mmap and never call aclrtFreeHost.
            monkeypatch.setenv(
                "ASCEND_GLOBAL_RESOURCE_CONFIG",
                json.dumps({"comm_resource_config.protocol_desc": ["ub_ctp:host"]}),
            )
            roce_owner.release(mmap_block)
            assert mapping.closed
            roce_owner.release(mmap_block)
            assert acl.calls.count("aclrtFreeHost") == free_before + 1
        finally:
            if roce_owner is not None:
                _dispose_owner(owner_kind, roce_owner)
            _dispose_owner(owner_kind, owner)

    @pytest.mark.parametrize("owner_kind", ["source", "borrowed"])
    @pytest.mark.parametrize("initial_roce", [True, False])
    def test_owner_protocol_snapshot_ignores_post_init_env_mutation(
        self, monkeypatch, owner_kind, initial_roce
    ):
        roce_cfg = json.dumps({"comm_resource_config.protocol_desc": ["roce:host"]})
        ub_cfg = json.dumps({"comm_resource_config.protocol_desc": ["ub_ctp:host"]})
        monkeypatch.setenv(
            "ASCEND_GLOBAL_RESOURCE_CONFIG", roce_cfg if initial_roce else ub_cfg
        )
        acl = RecordingAclLib(forbid_malloc=initial_roce)
        owner = _make_owner(owner_kind, acl)
        try:
            # Flip env to the opposite protocol after owner initialization.
            monkeypatch.setenv(
                "ASCEND_GLOBAL_RESOURCE_CONFIG", ub_cfg if initial_roce else roce_cfg
            )
            block = owner.allocate("weights.bin", 256)
            try:
                assert block.allocator_kind == ("mmap" if initial_roce else "acl")
            finally:
                owner.release(block)
        finally:
            _dispose_owner(owner_kind, owner)

    def test_worker_idle_rebuild_and_target_native_preserve_single_protocol_snapshot(
        self, monkeypatch, tmp_path
    ):
        import hashlib
        from pathlib import Path

        from gpustack_model_transfer.source.native_worker import NativeSourceWorker
        from gpustack_model_transfer.source.workorder import parse_assignment

        roce_cfg = json.dumps({"comm_resource_config.protocol_desc": ["roce:host"]})
        ub_cfg = json.dumps({"comm_resource_config.protocol_desc": ["ub_ctp:host"]})
        assert hixl_runtime.options_use_roce_host(
            hixl_runtime.build_hixl_init_options(
                {"ASCEND_GLOBAL_RESOURCE_CONFIG": roce_cfg}
            )
        )
        assert not hixl_runtime.options_use_roce_host(
            hixl_runtime.build_hixl_init_options(
                {"ASCEND_GLOBAL_RESOURCE_CONFIG": ub_cfg}
            )
        )

        fixture = Path(__file__).resolve().parents[3] / "contracts/fixtures/tiny_dense"
        raw_manifest = (fixture / "manifest.json").read_bytes()
        manifest_obj = json.loads(raw_manifest)
        assignment = parse_assignment(
            {
                "generation": "gen-roce-snapshot",
                "workerSecret": "secret",
                "digest": "sha256:" + "a" * 64,
                "namespace": "test",
                "artifactUID": manifest_obj["artifact_uid"],
                "sourceGeneration": manifest_obj["source_generation"],
                "manifestSHA256": hashlib.sha256(raw_manifest).hexdigest(),
                "manifestJSON": raw_manifest.decode(),
                "treePath": str(fixture),
                "totalBytes": sum(f["size"] for f in manifest_obj["files"]),
                "registrations": len(manifest_obj["files"]),
                "residentBudgetBytes": 4 << 20,
            },
            backend="hixl",
        )
        monkeypatch.setenv("ASCEND_GLOBAL_RESOURCE_CONFIG", roce_cfg)
        acl = RecordingAclLib(forbid_malloc=True)
        seen_opts = []
        backend = HixlSourceBackend(
            engine_factory=lambda: SimpleNamespace(
                initialize=lambda addr, opts: seen_opts.append(dict(opts)) or 0,
                register_mem=lambda desc, mtype: (0, 21),
                deregister_mem=lambda handle: 0,
                finalize=lambda: 0,
            ),
            engine_address="127.0.0.1:29800",
        )

        # Mutate env between preflight() and allocator construction/idle rebuild!
        def flip_on_native_start():
            monkeypatch.setenv("ASCEND_GLOBAL_RESOURCE_CONFIG", ub_cfg)

        state = {"active": 0, "action": "none"}
        control = SimpleNamespace(
            generation="gen-roce-snapshot",
            worker_id="pod-uid",
            worker_instance_id="inst-snap",
            register=lambda: "lease",
            native_start=flip_on_native_start,
            heartbeat=lambda **kw: {
                "action": state["action"],
                "activeSessions": state["active"],
                "unknownSessions": 0,
            },
            filled=lambda count, descs: None,
            drain_ack=lambda native_released: None,
            quarantine=lambda reason: None,
            close=lambda: None,
        )
        worker = NativeSourceWorker(
            control,
            assignment,
            backend=lambda: backend,
            allocator=lambda: AclHostOwner(loader=lambda _: acl),
            worker_instance_id="inst-snap",
            poll_interval=0.01,
        )
        try:
            worker.start()
            worker._stop_event.set()
            if worker._control_thread is not None:
                worker._control_thread.join(timeout=2)
            assert worker.state == "serving"
            assert hixl_runtime.options_use_roce_host(seen_opts[0]) is True
            assert all(b.allocator_kind == "mmap" for b in worker.blocks.values())

            worker.active_sessions = 0
            worker.unknown_sessions = 0
            assert worker.release_idle_blocks() is True
            assert worker.blocks == {}

            worker._rebuild_blocks()
            assert worker.state == "serving"
            assert worker.blocks
            assert all(b.allocator_kind == "mmap" for b in worker.blocks.values())
            assert "aclrtMallocHost" not in acl.calls
        finally:
            worker.active_sessions = 0
            worker.unknown_sessions = 0
            worker._drain_requested.set()
            worker.drain()
            worker.close()

        # TargetNative also aligns owner._use_roce_host with the single build_hixl_init_options() snapshot.
        monkeypatch.setenv("ASCEND_GLOBAL_RESOURCE_CONFIG", ub_cfg)
        target_acl = RecordingAclLib(forbid_malloc=True)

        def make_borrowed():
            owner_inst = BorrowedAclOwner(loader=lambda _: target_acl)
            # Flip env to roce:host AFTER BorrowedAclOwner() ran so build_hixl_init_options() sees roce:host;
            # TargetNative must align owner._use_roce_host with the resolved init options.
            monkeypatch.setenv("ASCEND_GLOBAL_RESOURCE_CONFIG", roce_cfg)
            return owner_inst

        target_init_opts = []
        fake_hixl = SimpleNamespace(
            Hixl=lambda: SimpleNamespace(
                initialize=lambda ep, opts: target_init_opts.append(dict(opts)) or 0,
                register_mem=lambda desc, mtype: (0, 31),
                deregister_mem=lambda h: 0,
                finalize=lambda: 0,
            ),
            SUCCESS=0,
            MemDesc=lambda a, n: (a, n),
            MemType=SimpleNamespace(MEM_HOST="MEM_HOST", MEM_DEVICE="MEM_DEVICE"),
            TransferOp=SimpleNamespace(READ="READ", WRITE="WRITE"),
            TransferStatus=SimpleNamespace(
                COMPLETED="COMPLETED", FAILED="FAILED", TIMEOUT="TIMEOUT"
            ),
        )
        real_api = hixl_runtime.RealHixlAPI()
        real_api._hixl = fake_hixl
        monkeypatch.setattr(client_native, "BorrowedAclOwner", make_borrowed)
        monkeypatch.setattr(hixl_runtime, "load_runtime", lambda: real_api)

        target = TargetNative("hixl", "127.0.0.1", 4 << 20)
        target.start()
        try:
            assert hixl_runtime.options_use_roce_host(target_init_opts[0]) is True
            assert target.block.allocator_kind == "mmap"
            assert "aclrtMallocHost" not in target_acl.calls
        finally:
            target.close()

    @pytest.mark.parametrize("owner_kind", ["source", "borrowed"])
    def test_gc_safety_and_quarantine_on_live_view_buffer_error(
        self, monkeypatch, owner_kind
    ):
        monkeypatch.setenv(
            "ASCEND_GLOBAL_RESOURCE_CONFIG",
            json.dumps({"comm_resource_config.protocol_desc": ["roce:host"]}),
        )
        acl = RecordingAclLib(forbid_malloc=True)
        owner = _make_owner(owner_kind, acl)
        block = owner.allocate("quarantine.bin", 4096)
        block.write_bytes(100, b"pinned-live-view")
        mapping = block.mapping
        mapping_ref = weakref.ref(mapping)

        # Hold a live exported memoryview (as torch.frombuffer does).
        live_view = block.view(100, 16)
        assert bytes(live_view) == b"pinned-live-view"

        # Release while view is alive triggers real BufferError -> quarantined, not dropped!
        with pytest.raises(SourceUnavailableError):
            owner.release(block)
        assert not mapping.closed
        assert block.address != 0
        assert block.mapping is mapping
        uncertain = (
            owner._uncertain_allocations
            if owner_kind == "source"
            else owner.uncertain_allocations
        )
        assert block in uncertain
        assert block in source_memory._RETAINED_MMAP_BLOCKS

        # Even after GC, mapping and view remain valid and readable.
        gc.collect()
        assert mapping_ref() is mapping
        assert bytes(live_view) == b"pinned-live-view"

        # Once the exported buffer is released, retrying release succeeds and clears quarantine.
        live_view.release()
        del live_view
        owner.release(block)
        assert mapping.closed
        assert block.address == 0
        assert block.mapping is None
        assert block not in uncertain
        assert block not in source_memory._RETAINED_MMAP_BLOCKS
        # Subsequent release is idempotent.
        owner.release(block)
        _dispose_owner(owner_kind, owner)

    def test_target_native_and_session_quarantine_when_mmap_close_refused(
        self, monkeypatch, tmp_path
    ):
        from gpustack_model_transfer import client as client_mod
        from gpustack_model_transfer.client import EngineSession

        ep_file = tmp_path / "ub_endpoint_npu_0.json"
        ep_file.write_text('{"phy":0}')
        monkeypatch.setenv(
            "ASCEND_GLOBAL_RESOURCE_CONFIG",
            json.dumps(
                {
                    "comm_resource_config.protocol_desc": ["roce:host"],
                    "local_comm_res_path": str(ep_file),
                }
            ),
        )
        acl = RecordingAclLib(forbid_malloc=True)
        monkeypatch.setattr(
            client_native, "BorrowedAclOwner", lambda: BorrowedAclOwner(lambda _: acl)
        )

        class Client:
            def initialize(self, endpoint, options):
                return 0

            def register_mem(self, desc, mem_type):
                return 0, 99

            def deregister_mem(self, handle):
                return 0

            def finalize(self):
                return 0

        fake_hixl = SimpleNamespace(
            Hixl=Client,
            SUCCESS=0,
            MemDesc=lambda addr, length: (addr, length),
            MemType=SimpleNamespace(MEM_HOST="MEM_HOST", MEM_DEVICE="MEM_DEVICE"),
            TransferOp=SimpleNamespace(READ="READ", WRITE="WRITE"),
            TransferStatus=SimpleNamespace(
                COMPLETED="COMPLETED", FAILED="FAILED", TIMEOUT="TIMEOUT"
            ),
        )
        real_api = hixl_runtime.RealHixlAPI()
        real_api._hixl = fake_hixl
        monkeypatch.setattr(hixl_runtime, "load_runtime", lambda: real_api)

        native = TargetNative("hixl", "127.0.0.1", 4 << 20)
        native.start()
        mapping = native.block.mapping
        live_view = native.block.view(64, 32)

        session = EngineSession.__new__(EngineSession)
        session.reader = SimpleNamespace(_close_phase=lambda *a, **kw: None)
        session.session_id = "s-roce-quarantine"
        session.close_request = "req-1"
        session.closed = False
        session.unknown = False
        session.inflight = None
        session._closing_started = False
        session._target_released = False
        session._close_uncertain = False
        session._windows = []
        session._native_owners = [native]
        session._prefetch_executor = None
        session.native = native
        session.targets = []
        session._lock = threading.Lock()
        session._closing = threading.Event()

        try:
            with pytest.raises(
                SourceUnavailableError, match="target release outcome unknown"
            ):
                session.close()
            assert session.unknown is True
            assert not native.closed
            assert not mapping.closed
            assert native.block in native.owner.uncertain_allocations
        finally:
            live_view.release()
            del live_view
            native.close()
            client_mod._release_retained(session)
        assert mapping.closed
        assert native.closed

    def test_exported_view_keeps_mmap_alive_across_owner_and_block_gc(
        self, monkeypatch
    ):
        monkeypatch.setenv(
            "ASCEND_GLOBAL_RESOURCE_CONFIG",
            json.dumps({"comm_resource_config.protocol_desc": ["roce:host"]}),
        )
        acl = RecordingAclLib(forbid_malloc=True)
        owner = BorrowedAclOwner(loader=lambda _: acl)
        block = owner.allocate("gc-window", 8192)
        block.write_bytes(256, b"survives-gc-via-buffer-protocol")
        mapping_ref = weakref.ref(block.mapping)
        block_ref = weakref.ref(block)

        exported_view = block.view(256, 31)
        del block
        del owner
        gc.collect()

        assert block_ref() is None
        assert mapping_ref() is not None
        assert not mapping_ref().closed
        assert bytes(exported_view) == b"survives-gc-via-buffer-protocol"
        exported_view.release()
        del exported_view
        gc.collect()
        assert mapping_ref() is None

    def test_native_source_worker_quarantines_when_mmap_close_refused(
        self, monkeypatch, tmp_path
    ):
        from pathlib import Path

        from gpustack_model_transfer.source.backend import (
            release_source_retained,
            retained_source_states,
        )
        from gpustack_model_transfer.source.native_worker import NativeSourceWorker
        from gpustack_model_transfer.source.workorder import parse_assignment

        fixture = Path(__file__).resolve().parents[3] / "contracts/fixtures/tiny_dense"
        raw_manifest = (fixture / "manifest.json").read_bytes()
        manifest_obj = json.loads(raw_manifest)
        import hashlib

        assignment = parse_assignment(
            {
                "generation": "gen-roce-quarantine",
                "workerSecret": "secret",
                "digest": "sha256:" + "a" * 64,
                "namespace": "test",
                "artifactUID": manifest_obj["artifact_uid"],
                "sourceGeneration": manifest_obj["source_generation"],
                "manifestSHA256": hashlib.sha256(raw_manifest).hexdigest(),
                "manifestJSON": raw_manifest.decode(),
                "treePath": str(fixture),
                "totalBytes": sum(f["size"] for f in manifest_obj["files"]),
                "registrations": len(manifest_obj["files"]),
                "residentBudgetBytes": 4 << 20,
            },
            backend="hixl",
        )
        monkeypatch.setenv(
            "ASCEND_GLOBAL_RESOURCE_CONFIG",
            json.dumps({"comm_resource_config.protocol_desc": ["roce:host"]}),
        )
        acl = RecordingAclLib(forbid_malloc=True)
        owner = AclHostOwner(loader=lambda _: acl)
        backend = HixlSourceBackend(
            engine_factory=lambda: SimpleNamespace(
                initialize=lambda addr, opts: 0,
                register_mem=lambda desc, mtype: (0, 11),
                deregister_mem=lambda handle: 0,
                finalize=lambda: 0,
            ),
            engine_address="127.0.0.1:29600",
        )
        acks = []
        quarantines = []
        control = SimpleNamespace(
            generation="gen-roce-quarantine",
            worker_id="pod-uid",
            register=lambda: "lease",
            native_start=lambda: None,
            heartbeat=lambda **kw: {
                "action": "none",
                "activeSessions": 0,
                "unknownSessions": 0,
            },
            filled=lambda count, descs: None,
            drain_ack=lambda native_released: acks.append(native_released),
            quarantine=lambda reason: quarantines.append(reason),
            close=lambda: None,
        )
        worker = NativeSourceWorker(
            control,
            assignment,
            backend=backend,
            allocator=owner,
            worker_instance_id="inst-1",
            poll_interval=0.01,
        )
        before_retained = set(retained_source_states())
        worker.start()
        worker._stop_event.set()
        if worker._control_thread is not None:
            worker._control_thread.join(timeout=2)
        assert worker.state == "serving"
        live_block = next(iter(worker.blocks.values()))
        assert live_block.allocator_kind == "mmap"
        live_view = live_block.view(0, 16)

        worker.active_sessions = 0
        worker.unknown_sessions = 0
        worker._drain_requested.set()
        try:
            worker.drain()
            assert worker.state == "quarantined"
            assert acks == []
            assert quarantines == ["SourceUnavailableError"]
            assert not live_block.mapping.closed
            assert live_block in owner._uncertain_allocations
        finally:
            live_view.release()
            del live_view
            owner.release(live_block)
            owner.dispose()
            for item in retained_source_states():
                if item not in before_retained:
                    release_source_retained(item, external_fence="test-cleanup")
