"""The runtime seam: lazy imports, the version pin, the native mapping.

The mapping tests run RealNixlAPI against a stub of the pinned nixl 1.5.0
_api module (tag v1.5.0, commit 1683cf3b7f3d11674c03c5e861cea22339876c96):
the stub carries the pinned surface and the pinned answer shapes — typed
exceptions for refusals, "DONE"/"PROC" strings from transfer and
check_xfer_state, opaque handle objects, bytes metadata and connection
tokens — so each test proves one fact about how the wrapper maps onto the
real native calls. No vendor package is installed or imported.
"""

import subprocess
import sys
import unittest
from pathlib import Path

from gpustack_model_transfer.backends.nixl.runtime import (
    PINNED_COMMIT,
    RegistrationRetainedError,
    _address_of,
    _buffer_extent,
)


class TestLazyImports(unittest.TestCase):
    def test_importing_the_backend_pulls_in_no_vendor_runtime(self):
        code = (
            "import sys, json;"
            "sys.path.insert(0, sys.argv[1]);"
            "import gpustack_model_transfer.backends.nixl as b;"
            "json.dump(sorted(m for m in sys.modules"
            "    if m.split('.')[0] in ('nixl', 'torch', 'nixl_pybind11')), sys.stdout)"
        )
        done = subprocess.run(
            [sys.executable, "-c", code, str(Path(__file__).resolve().parents[2])],
            capture_output=True,
            text=True,
            timeout=60,
            check=False,
        )
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertEqual(
            done.stdout.strip(), "[]", "a module import pulled a vendor runtime in"
        )


class TestVersionPin(unittest.TestCase):
    def test_default_pin_is_the_reviewed_release(self):
        from gpustack_model_transfer.backends.nixl import (
            DEFAULT_VERSION,
            SUPPORTED_VERSIONS,
        )

        self.assertEqual(DEFAULT_VERSION, "1.5.0")
        self.assertEqual(SUPPORTED_VERSIONS, frozenset({"1.5.0"}))

    def test_the_pinned_commit_is_the_v150_tag(self):
        # Verified at the primary source: `git rev-parse v1.5.0` in the
        # ai-dynamo/nixl checkout answers exactly this commit; the
        # repository HEAD examined alongside it (9e7800fc) is newer and
        # was not assumed to match.
        self.assertEqual(PINNED_COMMIT, "1683cf3b7f3d11674c03c5e861cea22339876c96")

    def test_matching_version_loads(self):
        from gpustack_model_transfer.backends.nixl import load_runtime
        from gpustack_model_transfer.backends.nixl.testing import FakeNixlAPI

        api = load_runtime(api=FakeNixlAPI(version="1.5.0"))
        self.assertEqual(api.version, "1.5.0")

    def test_mismatched_version_is_a_typed_refusal(self):
        from gpustack_model_transfer.backends.nixl import (
            VersionMismatchError,
            load_runtime,
        )
        from gpustack_model_transfer.backends.nixl.testing import FakeNixlAPI

        with self.assertRaises(VersionMismatchError):
            load_runtime(api=FakeNixlAPI(version="1.4.2"))
        with self.assertRaises(VersionMismatchError):
            load_runtime(api=FakeNixlAPI(version="2.0.0"))

    def test_an_explicitly_qualified_version_is_accepted(self):
        from gpustack_model_transfer.backends.nixl import load_runtime
        from gpustack_model_transfer.backends.nixl.testing import FakeNixlAPI

        api = load_runtime(
            api=FakeNixlAPI(version="1.6.0"), supported_versions={"1.6.0"}
        )
        self.assertEqual(api.version, "1.6.0")

    def test_missing_runtime_is_a_typed_refusal(self):
        from gpustack_model_transfer.backends.nixl import (
            RuntimeUnavailableError,
            load_runtime,
        )

        with self.assertRaises(RuntimeUnavailableError):
            load_runtime()


class TestProbeRuntime(unittest.TestCase):
    def test_probe_reports_what_is_missing_on_this_host(self):
        from gpustack_model_transfer.backends.nixl import probe_runtime

        readiness = probe_runtime()
        # This checkout has no nixl and no CUDA: the probe says so without
        # raising, and an absent runtime is never a healthy one.
        self.assertIsInstance(readiness.missing, tuple)
        if "nixl" in readiness.missing:
            self.assertFalse(readiness.ready)


class TestAddressOf(unittest.TestCase):
    """The address a registration pins is the data address, never a
    temporary copy's."""

    def test_a_writable_host_buffer_answers_without_raising(self):
        buffer = bytearray(64)
        buffer[0] = 0x7B
        address = _address_of(buffer)
        self.assertIsInstance(address, int)
        self.assertGreater(address, 0)

    def test_a_writable_memoryview_answers_the_same_address(self):
        from ctypes import addressof, c_char

        buffer = bytearray(32)
        self.assertEqual(
            _address_of(memoryview(buffer)), addressof(c_char.from_buffer(buffer))
        )

    def test_a_tensor_answers_through_data_ptr(self):
        class FakeTensor:
            def data_ptr(self) -> int:
                return 0xDEADBEEF

        self.assertEqual(_address_of(FakeTensor()), 0xDEADBEEF)

    def test_a_readonly_buffer_is_refused(self):
        with self.assertRaises(TypeError):
            _address_of(memoryview(b"readonly"))

    def test_bytes_are_refused_without_making_a_copy(self):
        # A bytes snapshot has no writable storage to register. Taking a
        # temporary copy's address would pin memory freed on return — a
        # dangling registration — so bytes are refused outright, never
        # copied to an ephemeral address.
        with self.assertRaises(TypeError):
            _address_of(b"0123456789abcdef")

    def test_the_address_names_the_data_not_the_object_header(self):
        first = bytearray(b"x")
        second = bytearray(b"y")
        self.assertNotEqual(_address_of(first), _address_of(second))


def tensor_like(
    device_type,
    *,
    nbytes=None,
    numel=None,
    element_size=None,
    contiguous=True,
    device_index=0,
):
    """A minimal tensor stand-in for the buffer contract."""

    class Device:
        type = device_type

    class StubTensor:
        device = Device()

        def data_ptr(self):
            return 0x12340000

        def is_contiguous(self):
            return contiguous

        def get_device(self):
            return device_index

    stub = StubTensor()
    if nbytes is not None:
        stub.nbytes = nbytes
    if numel is not None:
        stub.numel = lambda: numel
    if element_size is not None:
        stub.element_size = lambda: element_size
    return stub


class TestBufferExtent(unittest.TestCase):
    """The registration pins the true byte extent and the true memory
    class: nbytes, never len(); the device contract, never the module."""

    def test_a_cpu_tensor_registers_as_dram_with_its_byte_extent(self):
        # len() of a 10-element tensor is 10; the byte extent is 40. The
        # device contract — device.type == "cpu" — decides DRAM, not the
        # pointer's existence and not the module that produced it.
        extent, mem_type, device_id = _buffer_extent(
            tensor_like("cpu", nbytes=40, numel=10, element_size=4)
        )
        self.assertEqual(extent, 40)
        self.assertEqual(mem_type, "DRAM")
        self.assertEqual(device_id, 0)

    def test_a_tensor_without_nbytes_multiplies_numel_by_element_size(self):
        extent, mem_type, _device_id = _buffer_extent(
            tensor_like("cpu", numel=3, element_size=2)
        )
        self.assertEqual(extent, 6)
        self.assertEqual(mem_type, "DRAM")

    def test_a_cuda_tensor_registers_as_vram_with_its_device_index(self):
        extent, mem_type, device_id = _buffer_extent(
            tensor_like("cuda", nbytes=512, device_index=3)
        )
        self.assertEqual(extent, 512)
        self.assertEqual(mem_type, "VRAM")
        self.assertEqual(device_id, 3)

    def test_an_unknown_device_type_is_refused_not_classified(self):
        with self.assertRaises(TypeError):
            _buffer_extent(tensor_like("xpu", nbytes=64))

    def test_a_noncontiguous_tensor_is_refused(self):
        with self.assertRaises(TypeError):
            _buffer_extent(tensor_like("cpu", nbytes=64, contiguous=False))

    def test_an_empty_tensor_is_refused(self):
        with self.assertRaises(ValueError):
            _buffer_extent(tensor_like("cpu", nbytes=0, numel=0, element_size=4))

    def test_a_byte_view_is_dram_sized_by_its_byte_count(self):
        buffer = bytearray(32)
        extent, mem_type, device_id = _buffer_extent(memoryview(buffer))
        self.assertEqual(extent, 32)
        self.assertEqual(mem_type, "DRAM")
        self.assertEqual(device_id, 0)

    def test_a_non_byte_memoryview_is_refused(self):
        # A view of int32 items: len() counts items, not bytes, so the
        # byte extent would be guessed.
        import array

        view = memoryview(array.array("i", [1, 2, 3, 4]))
        with self.assertRaises(TypeError):
            _buffer_extent(view)

    def test_a_bytearray_is_dram(self):
        self.assertEqual(_buffer_extent(bytearray(16)), (16, "DRAM", 0))


class StubNixlApiModule:
    """The pinned binding's shape (nixl 1.5.0, commit 1683cf3b) for
    mapping tests: nixl_agent(name, config), register_memory /
    deregister_memory with typed-exception refusals, get_xfer_descs,
    initialize_xfer + transfer + check_xfer_state + release_xfer_handle
    with "DONE"/"PROC" strings and opaque handle objects, bytes metadata
    and connection tokens, and the module names the surface gate checks."""

    class Config:
        def __init__(self, **kwargs):
            self.kwargs = kwargs

    class SyncMode:
        NIXL_THREAD_SYNC_NONE = "SYNC_NONE"
        NIXL_THREAD_SYNC_STRICT = "SYNC_STRICT"
        NIXL_THREAD_SYNC_RW = "SYNC_RW"
        NIXL_THREAD_SYNC_DEFAULT = "SYNC_DEFAULT"

    class RegHandle:
        def __init__(self, index):
            self.index = index

    class XferHandle:
        def __init__(self, index):
            self.index = index

    class DescList:
        def __init__(self, mem_type, descs):
            self.mem_type = mem_type
            self.descs = list(descs)

    class nixl_agent_config(Config):
        """The pinned module-level configuration class, under its
        pinned name."""

    nixl_thread_sync_t = SyncMode

    class Bindings:
        """The binding's status errors identify native errors, not proof
        that every transfer using a buffer has stopped."""

        class nixlBackendError(RuntimeError):
            pass

        class nixlInvalidParamError(RuntimeError):
            pass

        class nixlRemoteDisconnectError(RuntimeError):
            pass

        class nixlNotSupportedError(RuntimeError):
            pass

        class nixlNotPostedError(RuntimeError):
            pass

        class nixlNotFoundError(RuntimeError):
            pass

        class nixlMismatchError(RuntimeError):
            pass

        class nixlNotAllowedError(RuntimeError):
            pass

        class nixlRepostActiveError(RuntimeError):
            pass

        class nixlUnknownError(RuntimeError):
            pass

        class nixlCancelledError(RuntimeError):
            pass

        class nixlNoTelemetryError(RuntimeError):
            pass

        NIXL_SUCCESS = 0
        NIXL_IN_PROG = 1

    nixlBind = Bindings

    class nixl_agent:
        def __init__(self, name, config):
            self.name = name
            self.config = config
            self.calls = []
            self.next_reg = 100
            self.next_transfer = 500
            self.transfer_state = "PROC"
            self.status_answer = "PROC"
            self.status_raises = None
            self.transfer_raises = None
            self.transfer_side_effect_then_raises = None
            self.posted_effectfully = []
            self.release_fails = None
            self.register_fail_from = None
            self.deregister_fail_first = None
            self.deregister_refuse_index = None
            self.released = []

        def register_memory(self, descs, mem_type=None, backends=None):
            if (
                self.register_fail_from is not None
                and len([c for c in self.calls if c[0] == "register_memory"])
                >= self.register_fail_from
            ):
                raise RuntimeError("nixlBackendError: registration refused")
            self.calls.append(("register_memory", list(descs), mem_type))
            self.next_reg += 1
            return StubNixlApiModule.RegHandle(self.next_reg)

        def deregister_memory(self, reg_descs, backends=None):
            self.calls.append(("deregister_memory", reg_descs))
            if (
                self.deregister_refuse_index is not None
                and reg_descs.index == self.deregister_refuse_index
            ):
                raise RuntimeError("nixlBackendError: deregistration refused")
            if self.deregister_fail_first is not None:
                failures = self.deregister_fail_first
                self.deregister_fail_first = None
                if failures > 0:
                    raise RuntimeError("nixlBackendError: deregistration refused")

        def get_xfer_descs(self, descs, mem_type=None):
            self.calls.append(("get_xfer_descs", list(descs), mem_type))
            return StubNixlApiModule.DescList(mem_type, descs)

        def initialize_xfer(
            self,
            operation,
            local_descs,
            remote_descs,
            remote_agent,
            notif_msg=b"",
            backends=None,
        ):
            self.calls.append(
                (
                    "initialize_xfer",
                    operation,
                    local_descs,
                    remote_descs,
                    remote_agent,
                    notif_msg,
                )
            )
            self.next_transfer += 1
            return StubNixlApiModule.XferHandle(self.next_transfer)

        def transfer(self, handle, notif_msg=b""):
            self.calls.append(("transfer", handle, notif_msg))
            if self.transfer_side_effect_then_raises is not None:
                # A post whose submission took effect (recorded here,
                # observable in the stub) and whose call still failed —
                # exactly the ambiguity the seam must not lie about.
                error = self.transfer_side_effect_then_raises
                self.transfer_side_effect_then_raises = None
                self.posted_effectfully.append(handle)
                raise error
            if self.transfer_raises is not None:
                raise self.transfer_raises
            return self.transfer_state

        def check_xfer_state(self, handle):
            self.calls.append(("check_xfer_state", handle))
            if self.status_raises is not None:
                raise self.status_raises
            return self.status_answer

        def release_xfer_handle(self, handle):
            self.calls.append(("release_xfer_handle", handle))
            if self.release_fails is not None:
                failure = self.release_fails
                self.release_fails = None
                raise failure
            self.released.append(handle)

        def get_agent_metadata(self):
            self.calls.append(("get_agent_metadata",))
            return b"stub-agent-metadata"

        def add_remote_agent(self, metadata):
            self.calls.append(("add_remote_agent", metadata))
            return b"stub-remote-agent"

        def remove_remote_agent(self, agent):
            self.calls.append(("remove_remote_agent", agent))


def real_api():
    """A RealNixlAPI bound to a fresh stub module, for mapping tests."""
    from gpustack_model_transfer.backends.nixl.runtime import RealNixlAPI

    stub = StubNixlApiModule()
    api = RealNixlAPI()
    api._nixl_api = stub
    return api, stub.nixl_agent("agent-under-test", stub.Config())


class TestRealApiSurface(unittest.TestCase):
    """The real wrapper maps onto the pinned binding, or refuses named."""

    def test_a_binding_missing_the_pinned_surface_is_a_typed_refusal(self):
        from gpustack_model_transfer.backends.nixl.runtime import (
            RealNixlAPI,
            VersionMismatchError,
        )

        class Missing:
            pass

        api = RealNixlAPI()
        with self.assertRaises(VersionMismatchError) as caught:
            api._check_surface(Missing)
        self.assertIn("nixl_agent", str(caught.exception))
        self.assertIn(PINNED_COMMIT[:12], str(caught.exception))

        class Partial:
            nixl_agent = object
            nixl_agent_config = object
            nixl_thread_sync_t = object

        with self.assertRaises(VersionMismatchError) as caught:
            api._check_surface(Partial)
        self.assertIn("register_memory", str(caught.exception))

    def test_create_agent_constructs_the_pinned_agent_with_a_config(self):
        api, agent = real_api()
        settings = {
            "backends": ["UCX"],
            "enable_prog_thread": True,
            "enable_listen_thread": False,
            "num_threads": 2,
            "sync_mode": "STRICT",
        }
        agent = api.create_agent("source-agent", settings)
        self.assertIsInstance(agent.config, StubNixlApiModule.Config)
        self.assertEqual(agent.config.kwargs["backends"], ["UCX"])
        self.assertEqual(agent.config.kwargs["enable_prog_thread"], True)
        self.assertEqual(agent.config.kwargs["enable_listen_thread"], False)
        self.assertEqual(agent.config.kwargs["num_threads"], 2)
        self.assertEqual(
            agent.config.kwargs["sync_mode"],
            StubNixlApiModule.SyncMode.NIXL_THREAD_SYNC_STRICT,
        )
        self.assertEqual(agent.name, "source-agent")

    def test_a_ucx_selection_cannot_be_scoped_per_agent_and_is_refused(self):
        import os

        api, _agent = real_api()
        from gpustack_model_transfer.backends.nixl.errors import (
            UnsupportedSettingError,
        )

        before = dict(os.environ)
        for key in ("UCX_TLS", "UCX_NET_DEVICES", "UCX_POSIX_USE_PROC_EVENT"):
            with self.subTest(key=key):
                with self.assertRaises(UnsupportedSettingError) as caught:
                    api.create_agent("source-agent", {key: "rc"})
                self.assertIn(key, str(caught.exception))
                self.assertIn(
                    "never writes a process-wide environment", str(caught.exception)
                )
        # The refusal mutated nothing: the server environment stays
        # authoritative exactly as it was.
        self.assertEqual(dict(os.environ), before)

    def test_a_supported_setting_is_applied_without_touching_the_environment(self):
        import os

        api, _agent = real_api()
        before = dict(os.environ)
        api.create_agent("source-agent", {"backends": ["UCX"], "num_threads": 1})
        self.assertEqual(dict(os.environ), before)

    def test_a_mistyped_setting_is_refused(self):
        api, _agent = real_api()
        from gpustack_model_transfer.backends.nixl.errors import (
            UnsupportedSettingError,
        )

        with self.assertRaises(UnsupportedSettingError):
            api.create_agent("source-agent", {"num_threads": "two"})
        with self.assertRaises(UnsupportedSettingError):
            api.create_agent("source-agent", {"enable_prog_thread": 1})
        with self.assertRaises(UnsupportedSettingError):
            api.create_agent("source-agent", {"sync_mode": "EVERYTHING"})

    def test_a_wrong_sync_mode_value_is_refused_not_stringified_through(self):
        api, _agent = real_api()
        with self.assertRaises(Exception):
            api.create_agent("source-agent", {"sync_mode": 7})

    def test_the_real_wrapper_invents_no_renewal(self):
        # The pinned surface has no registration-renewal call and reports
        # no expiry, so the real wrapper carries no such method and never
        # raises RegistrationExpiredError; the reader's renew path is
        # exercised by backends that can renew, such as the fake.
        from gpustack_model_transfer.backends.nixl.runtime import RealNixlAPI

        self.assertFalse(hasattr(RealNixlAPI, "renew_registration"))


class TestRealRegistration(unittest.TestCase):
    """Registration is address-based, per window, and rollback-exact."""

    def test_each_window_is_registered_with_its_true_descriptor(self):
        api, agent = real_api()
        window = bytearray(64)
        registration = api.register_memory(agent, {"window-0": window})
        ((_kind, descs, mem_type),) = [
            call for call in agent.calls if call[0] == "register_memory"
        ]
        self.assertEqual(len(descs), 1)
        addr, extent, device_id, meta = descs[0]
        self.assertEqual(extent, 64)
        self.assertEqual(device_id, 0)
        self.assertEqual(meta, "")
        self.assertEqual(mem_type, "DRAM")
        self.assertEqual(addr, _address_of(window))
        # The registration holds the native handle objects themselves,
        # never stringified copies of them.
        handle = registration.handles["window-0"]
        self.assertIsInstance(handle, StubNixlApiModule.RegHandle)
        self.assertEqual(
            registration.windows["window-0"],
            {"addr": addr, "len": 64, "mem_type": "DRAM", "dev_id": 0},
        )
        # The strong buffer reference keeps the pinned address real.
        self.assertIs(registration.buffers["window-0"], window)

    def test_bytes_are_refused_without_a_registration(self):
        api, agent = real_api()
        with self.assertRaises(TypeError):
            api.register_memory(agent, {"window-0": b"0123456789abcdef"})
        self.assertEqual(
            [call for call in agent.calls if call[0] == "register_memory"], []
        )

    def test_a_cpu_tensor_window_is_registered_as_dram_at_its_byte_extent(self):
        api, agent = real_api()
        api.register_memory(agent, {"w": tensor_like("cpu", nbytes=40)})
        ((_kind, descs, mem_type),) = [
            call for call in agent.calls if call[0] == "register_memory"
        ]
        self.assertEqual(descs[0][1], 40)
        self.assertEqual(mem_type, "DRAM")

    def test_registration_rolls_back_earlier_handles_on_a_later_refusal(self):
        api, agent = real_api()
        agent_class = type(agent)
        original = agent_class.register_memory

        def failing(self, descs, mem_type=None, backends=None):
            if len([c for c in self.calls if c[0] == "register_memory"]) >= 1:
                raise RuntimeError("nixlBackendError: registration refused")
            return original(self, descs, mem_type, backends)

        agent_class.register_memory = failing
        try:
            with self.assertRaises(RuntimeError):
                api.register_memory(agent, {"w0": bytearray(16), "w1": bytearray(16)})
        finally:
            agent_class.register_memory = original
        rollbacks = [call for call in agent.calls if call[0] == "deregister_memory"]
        self.assertEqual(len(rollbacks), 1)
        self.assertEqual(rollbacks[0][1].index, 101)

    def test_registration_rollback_refusal_retains_handles_and_buffers(self):
        api, agent = real_api()
        agent_class = type(agent)
        original = agent_class.register_memory

        def failing(self, descs, mem_type=None, backends=None):
            if len([c for c in self.calls if c[0] == "register_memory"]) >= 1:
                raise RuntimeError("nixlBackendError: registration refused")
            return original(self, descs, mem_type, backends)

        agent_class.register_memory = failing
        stuck_original = agent_class.deregister_memory

        def stuck(self, reg_descs, backends=None):
            raise RuntimeError("nixlBackendError: deregistration refused")

        agent_class.deregister_memory = stuck
        from gpustack_model_transfer.backends.nixl.runtime import (
            release_retained,
            retained_registrations,
        )

        window = bytearray(16)
        before = len(retained_registrations())
        try:
            with self.assertRaises(RegistrationRetainedError) as caught:
                api.register_memory(agent, {"w0": window, "w1": bytearray(16)})
        finally:
            agent_class.register_memory = original
            agent_class.deregister_memory = stuck_original
        retainment = caught.exception.retainment
        # The exact native handle and a strong buffer reference are held
        # by a live quarantine owner: naming the leak would retain nothing.
        self.assertIn("w0", retainment.handles)
        self.assertIs(retainment.buffers["w0"], window)
        self.assertIs(retainment.agent, agent)
        self.assertEqual(len(retained_registrations()), before + 1)
        self.assertTrue(
            release_retained(retainment, external_fence="host cleanup record #3")
        )

    def test_rollback_keeps_no_stale_handle_after_a_succeeded_undo(self):
        # Three windows: the third registration refuses, the rollback
        # undoes w1, and the w0 undo is refused. The retainment must hold
        # ONLY the still-owned w0 — never the already-released w1 — and
        # a retry disposal must not double-deregister anything.
        from gpustack_model_transfer.backends.nixl.runtime import release_retained

        api, agent = real_api()
        agent.register_fail_from = 2
        agent.deregister_refuse_index = 101
        with self.assertRaises(RegistrationRetainedError) as caught:
            api.register_memory(
                agent,
                {"w0": bytearray(8), "w1": bytearray(8), "w2": bytearray(8)},
            )
        retainment = caught.exception.retainment
        self.assertEqual(set(retainment.handles), {"w0"})
        self.assertEqual(retainment.handles["w0"].index, 101)
        # w1 was undone exactly once; w0 was attempted exactly once.
        deregisters = [
            call[1].index for call in agent.calls if call[0] == "deregister_memory"
        ]
        self.assertEqual(deregisters, [102, 101])
        self.assertTrue(
            release_retained(retainment, external_fence="host cleanup record #5")
        )

    def test_a_rollback_never_mutates_the_callers_buffer_dicts(self):
        api, agent = real_api()
        agent.register_fail_from = 1
        w0 = bytearray(8)
        w1 = bytearray(8)
        buffers = {"w0": w0, "w1": w1}
        with self.assertRaises(RuntimeError):
            api.register_memory(agent, buffers)
        # w0's completed registration was rolled back exactly once…
        deregisters = [
            call[1].index for call in agent.calls if call[0] == "deregister_memory"
        ]
        self.assertEqual(deregisters, [101])
        # …and the caller's own dict is untouched: the rollback kept its
        # bookkeeping in copies.
        self.assertEqual(set(buffers), {"w0", "w1"})
        self.assertIs(buffers["w0"], w0)
        self.assertIs(buffers["w1"], w1)

    def test_a_retained_registration_drops_freely_after_genuine_cleanup(self):
        from gpustack_model_transfer.backends.nixl.runtime import release_retained

        api, agent = real_api()
        agent.deregister_fail_first = 1
        registration = api.register_memory(agent, {"window-0": bytearray(32)})
        with self.assertRaises(RegistrationRetainedError) as caught:
            api.revoke_registration(agent, registration)
        retainment = caught.exception.retainment
        self.assertIs(retainment.registration, registration)
        self.assertIs(retainment.agent, agent)
        # The fence's disposal path is an ordinary retry of the revoke:
        # it empties the registration's handles…
        api.revoke_registration(agent, registration)
        self.assertEqual(registration.handles, {})
        # …and the record then drops without any evidence, because the
        # state is genuinely gone — no quarantine owner is stranded.
        self.assertTrue(release_retained(retainment))

    def test_revoke_refuses_a_foreign_agent(self):
        api, agent = real_api()
        registration = api.register_memory(agent, {"window-0": bytearray(32)})
        _other_api, other_agent = real_api()
        with self.assertRaises(ValueError):
            api.revoke_registration(other_agent, registration)

    def test_a_deregistration_refusal_retains_and_a_retry_frees_exactly_once(self):
        api, agent = real_api()
        agent.deregister_fail_first = 1
        registration = api.register_memory(agent, {"window-0": bytearray(32)})
        with self.assertRaises(RegistrationRetainedError) as caught:
            api.revoke_registration(agent, registration)
        # The refused handle stays on the registration: a retry cannot
        # double-free, and the refusal cannot silently drop it.
        self.assertIn("window-0", registration.handles)
        self.assertIn("window-0", caught.exception.retainment.handles)
        api.revoke_registration(agent, registration)
        self.assertEqual(registration.handles, {})
        deregisters = [call for call in agent.calls if call[0] == "deregister_memory"]
        self.assertEqual(len(deregisters), 2)

    def test_revoke_after_success_leaves_nothing_to_double_free(self):
        api, agent = real_api()
        registration = api.register_memory(agent, {"window-0": bytearray(32)})
        api.revoke_registration(agent, registration)
        deregisters = [call for call in agent.calls if call[0] == "deregister_memory"]
        api.revoke_registration(agent, registration)
        self.assertEqual(
            [call for call in agent.calls if call[0] == "deregister_memory"],
            deregisters,
        )


def submit(api, agent, registration, direction="WRITE", **overrides):
    """One source- or target-side submission the way the source service
    makes it: the peer's published descriptor and a window of ours."""
    connection = overrides.get("connection", b"stub-remote-agent")
    remote = overrides.get(
        "remote", {"addr": 0x5000, "len": 64, "mem_type": "DRAM", "device_id": 0}
    )
    length = overrides.get("length", 16)
    submit_direction = (
        api.submit_source_write if direction == "WRITE" else api.submit_target_read
    )
    return submit_direction(
        agent,
        registration,
        connection,
        overrides.get("window", "window-0"),
        remote,
        length,
    )


class TestRealTransfers(unittest.TestCase):
    """The source-initiated WRITE, the target READ, and the honest
    refusal of the legacy consumer-side WRITE."""

    def test_the_legacy_consumer_write_is_refused_honestly(self):
        from gpustack_model_transfer.backends.nixl.errors import TransferRefusedError

        api, agent = real_api()
        registration = api.register_memory(agent, {"window-0": bytearray(32)})
        with self.assertRaises(TransferRefusedError) as caught:
            api.submit_write(
                agent,
                registration,
                "window-0",
                {"file": "checkpoint.safetensors", "offset": 0},
                16,
            )
        # The refusal is the answer: nothing was initialized or posted on
        # the native surface.
        self.assertEqual(
            [
                call
                for call in agent.calls
                if call[0] in ("initialize_xfer", "transfer")
            ],
            [],
        )
        self.assertIn("submit_source_write", str(caught.exception))

    def test_a_source_write_maps_onto_the_pinned_combined_call(self):
        api, agent = real_api()
        window = bytearray(32)
        registration = api.register_memory(agent, {"window-0": window})
        operation = submit(api, agent, registration, direction="WRITE")
        initialize = [call for call in agent.calls if call[0] == "initialize_xfer"]
        self.assertEqual(len(initialize), 1)
        _kind, op, local, remote, remote_agent, notif = initialize[0]
        self.assertEqual(op, "WRITE")
        # The local descriptor is the window's own registration record,
        # bounded by the transfer length.
        self.assertEqual(local.descs, [(_address_of(window), 16, 0)])
        self.assertEqual(local.mem_type, "DRAM")
        # The remote descriptor is the peer's published region, unguessed.
        self.assertEqual(remote.descs, [(0x5000, 16, 0)])
        self.assertEqual(remote.mem_type, "DRAM")
        # The connection token travels as connect_peer returned it; the
        # handle is opaque, never stringified.
        self.assertEqual(remote_agent, b"stub-remote-agent")
        self.assertEqual(notif, b"")
        self.assertNotIsInstance(operation, str)
        self.assertIsInstance(operation.handle, StubNixlApiModule.XferHandle)

    def test_a_target_read_maps_onto_the_pinned_combined_call(self):
        api, agent = real_api()
        registration = api.register_memory(agent, {"window-0": bytearray(32)})
        submit(api, agent, registration, direction="READ")
        _kind, op, _local, _remote, _remote_agent, _notif = next(
            call for call in agent.calls if call[0] == "initialize_xfer"
        )
        self.assertEqual(op, "READ")

    def test_a_transfer_over_the_window_is_refused_before_any_native_call(self):
        api, agent = real_api()
        registration = api.register_memory(agent, {"window-0": bytearray(32)})
        with self.assertRaises(ValueError):
            submit(api, agent, registration, length=64)
        self.assertEqual(
            [call for call in agent.calls if call[0] == "initialize_xfer"], []
        )

    def test_a_transfer_over_the_peer_region_is_refused(self):
        api, agent = real_api()
        registration = api.register_memory(agent, {"window-0": bytearray(32)})
        with self.assertRaises(ValueError):
            submit(
                api,
                agent,
                registration,
                remote={"addr": 0x5000, "len": 8, "mem_type": "DRAM"},
                length=16,
            )

    def test_a_file_shaped_remote_is_refused(self):
        api, agent = real_api()
        registration = api.register_memory(agent, {"window-0": bytearray(32)})
        with self.assertRaises(ValueError):
            submit(
                api,
                agent,
                registration,
                remote={"file": "checkpoint.safetensors", "offset": 0},
            )

    def test_a_remote_without_a_published_memory_class_is_refused(self):
        api, agent = real_api()
        registration = api.register_memory(agent, {"window-0": bytearray(32)})
        with self.assertRaises(ValueError):
            submit(api, agent, registration, remote={"addr": 0x5000, "len": 64})

    def test_an_unconnected_token_is_refused(self):
        api, agent = real_api()
        registration = api.register_memory(agent, {"window-0": bytearray(32)})
        with self.assertRaises(ValueError):
            submit(api, agent, registration, connection="")
        with self.assertRaises(ValueError):
            submit(api, agent, registration, connection=17)

    def test_an_unknown_window_is_refused(self):
        api, agent = real_api()
        registration = api.register_memory(agent, {"window-0": bytearray(32)})
        with self.assertRaises(ValueError):
            submit(api, agent, registration, window="window-9")

    def test_an_initialize_refusal_is_the_proven_pre_submission_failure(self):
        from gpustack_model_transfer.backends.nixl.errors import TransferRefusedError
        from gpustack_model_transfer.backends.nixl.runtime import (
            retained_registrations,
        )

        api, agent = real_api()
        registration = api.register_memory(agent, {"window-0": bytearray(32)})
        original = type(agent).initialize_xfer

        def refusing(self, *args, **kwargs):
            raise RuntimeError("nixlBackendError: descriptor mismatch")

        type(agent).initialize_xfer = refusing
        try:
            before = len(retained_registrations())
            with self.assertRaises(TransferRefusedError) as caught:
                submit(api, agent, registration)
        finally:
            type(agent).initialize_xfer = original
        # The request was never created, so nothing could have posted:
        # this, and only this, is a proven pre-submission refusal.
        self.assertIn("nothing was posted", str(caught.exception))
        self.assertEqual(len(retained_registrations()), before)
        self.assertEqual(
            [call for call in agent.calls if call[0] == "release_xfer_handle"], []
        )

    def test_a_post_exception_after_side_effects_is_ambiguous_and_retained(self):
        from gpustack_model_transfer.backends.nixl.runtime import (
            AmbiguousPostError,
            release_retained,
            retained_registrations,
        )

        api, agent = real_api()
        registration = api.register_memory(agent, {"window-0": bytearray(32)})
        # The post's submission took effect (the stub records it) and the
        # call still raised — the case that must never be reported as a
        # clean pre-submission refusal.
        agent.transfer_side_effect_then_raises = RuntimeError(
            "nixlBackendError: post refused"
        )
        before = len(retained_registrations())
        with self.assertRaises(AmbiguousPostError) as caught:
            submit(api, agent, registration)
        # No release is attempted: a cancel must never be disguised as
        # pre-submission cleanup of a possibly-submitted transfer.
        self.assertEqual(
            [call for call in agent.calls if call[0] == "release_xfer_handle"], []
        )
        self.assertEqual(len(agent.posted_effectfully), 1)
        # The complete graph — operation, agent, registration, strong
        # buffers — is retained process-wide.
        retainment = retained_registrations()[-1]
        self.assertEqual(len(retained_registrations()), before + 1)
        self.assertIs(retainment.handles["operation"], caught.exception.operation)
        self.assertIs(retainment.agent, agent)
        self.assertIs(retainment.registration, registration)
        self.assertIs(retainment.buffers["window-0"], registration.buffers["window-0"])
        self.assertTrue(
            release_retained(
                retainment, external_fence="host-verified abort record #417"
            )
        )

    def test_the_ambiguous_post_error_is_not_a_model_transfer_error(self):
        # The reader classifies ModelTransferError from a submit as a
        # pre-submission refusal and frees the window; an ambiguous post
        # must therefore NOT be one. Subclassing RuntimeError routes it
        # to the reader's unclassified branch: operation UNKNOWN, window
        # retained.
        from gpustack_model_transfer.backends.nixl.runtime import AmbiguousPostError
        from gpustack_model_transfer.core.errors import ModelTransferError

        self.assertTrue(issubclass(AmbiguousPostError, RuntimeError))
        self.assertFalse(issubclass(AmbiguousPostError, ModelTransferError))


class TestRealPoll(unittest.TestCase):
    """Terminal observations release exactly once; everything else is
    retained."""

    def test_a_proc_answer_is_pending_and_retains_the_handle(self):
        api, agent = real_api()
        registration = api.register_memory(agent, {"window-0": bytearray(32)})
        operation = submit(api, agent, registration)
        outcome = api.poll(agent, operation)
        self.assertEqual(outcome.state, "pending")
        self.assertEqual(
            [call for call in agent.calls if call[0] == "release_xfer_handle"], []
        )

    def test_a_done_answer_succeeds_and_releases_exactly_once(self):
        api, agent = real_api()
        registration = api.register_memory(agent, {"window-0": bytearray(32)})
        operation = submit(api, agent, registration)
        api.poll(agent, operation)
        agent.status_answer = "DONE"
        outcome = api.poll(agent, operation)
        self.assertEqual(outcome.state, "succeeded")
        releases = [call for call in agent.calls if call[0] == "release_xfer_handle"]
        self.assertEqual(len(releases), 1)
        # The handle is gone: another poll has nothing to observe.
        with self.assertRaises(ValueError):
            api.poll(agent, operation)

    def test_a_done_at_post_skips_the_status_call_and_releases_once(self):
        api, agent = real_api()
        agent.transfer_state = "DONE"
        agent.status_raises = StubNixlApiModule.Bindings.nixlNotFoundError(
            "status must not be queried after completed post"
        )
        registration = api.register_memory(agent, {"window-0": bytearray(32)})
        operation = submit(api, agent, registration)
        outcome = api.poll(agent, operation)
        self.assertEqual(outcome.state, "succeeded")
        status_calls = [call for call in agent.calls if call[0] == "check_xfer_state"]
        self.assertEqual(status_calls, [])
        self.assertEqual(len(agent.released), 1)

    def test_status_exceptions_retain_active_transfers_without_cleanup(self):
        from gpustack_model_transfer.backends.nixl.errors import FenceRequiredError
        from gpustack_model_transfer.backends.nixl.runtime import (
            release_retained,
            retained_registrations,
        )

        # NOT_FOUND can be returned before checkXfer, leaving native status
        # IN_PROG. Other error codes also do not prove all requests drained.
        error_names = (
            "nixlNotPostedError",
            "nixlInvalidParamError",
            "nixlBackendError",
            "nixlNotFoundError",
            "nixlMismatchError",
            "nixlNotAllowedError",
            "nixlRepostActiveError",
            "nixlUnknownError",
            "nixlNotSupportedError",
            "nixlRemoteDisconnectError",
            "nixlCancelledError",
            "nixlNoTelemetryError",
        )
        for name in error_names:
            with self.subTest(error=name):
                api, agent = real_api()
                window = bytearray(32)
                registration = api.register_memory(agent, {"window-0": window})
                operation = submit(api, agent, registration)
                native_handle = operation.handle
                native_registrations = dict(registration.handles)
                error_type = getattr(StubNixlApiModule.Bindings, name)
                error = error_type("status observation failed")
                agent.status_raises = error
                before = len(retained_registrations())
                try:
                    with self.assertRaises(error_type) as caught:
                        api.poll(agent, operation)
                    self.assertIs(caught.exception, error)
                    self.assertFalse(operation.terminal)
                    self.assertFalse(operation.released)
                    self.assertIs(operation.handle, native_handle)
                    self.assertEqual(registration.handles, native_registrations)
                    self.assertIs(registration.buffers["window-0"], window)
                    records = retained_registrations()[before:]
                    self.assertTrue(records)
                    self.assertIs(records[0].handles["operation"], operation)
                    self.assertIs(records[0].registration, registration)
                    self.assertIs(records[0].agent, agent)
                    with self.assertRaises(FenceRequiredError):
                        api.release_operation(agent, operation)
                    self.assertEqual(
                        [
                            call
                            for call in agent.calls
                            if call[0]
                            in (
                                "release_xfer_handle",
                                "deregister_memory",
                                "remove_remote_agent",
                            )
                        ],
                        [],
                    )
                finally:
                    for record in retained_registrations()[before:]:
                        release_retained(
                            record, external_fence="test-only stub disposal"
                        )

    def test_an_unclassified_observation_error_holds_and_retains_the_graph(self):
        self._assert_observation_error_retains_graph(
            RuntimeError("observation transport broke")
        )

    def test_not_found_retains_the_graph_after_the_caller_drops_it(self):
        self._assert_observation_error_retains_graph(
            StubNixlApiModule.Bindings.nixlNotFoundError(
                "remote metadata expired while transfer remained active"
            )
        )

    def test_process_control_exceptions_retain_before_propagating(self):
        for error in (KeyboardInterrupt(), SystemExit(7)):
            with self.subTest(error=type(error).__name__):
                self._assert_observation_error_retains_graph(error)

    def _assert_observation_error_retains_graph(self, error):
        # A status observation error is not a drain verdict: no release
        # is attempted, the complete graph is retained, and the
        # exception re-raises so the caller keeps the window as UNKNOWN.
        import gc
        import weakref

        from gpustack_model_transfer.backends.nixl.runtime import (
            release_retained,
            retained_registrations,
        )

        api, agent = real_api()
        window = bytearray(32)
        registration = api.register_memory(agent, {"window-0": window})
        operation = submit(api, agent, registration)
        agent.status_raises = error
        before = len(retained_registrations())
        with self.assertRaises(type(error)) as caught:
            api.poll(agent, operation)
        self.assertIs(caught.exception, error)
        self.assertFalse(operation.terminal)
        self.assertFalse(operation.released)
        self.assertEqual(
            [call for call in agent.calls if call[0] == "release_xfer_handle"], []
        )
        self.assertEqual(len(retained_registrations()), before + 1)
        retainment = retained_registrations()[-1]
        self.assertIs(retainment.handles["operation"], operation)
        self.assertIs(retainment.agent, agent)
        self.assertIs(retainment.registration, registration)
        # The graph survives the caller dropping every reference. (A
        # bytearray cannot be weak-referenced; its liveness travels with
        # the registration, which the quarantine owner keeps alive.)
        agent_ref = weakref.ref(agent)
        registration_ref = weakref.ref(registration)
        operation_ref = weakref.ref(operation)
        del agent, registration, operation
        gc.collect()
        self.assertIsNotNone(agent_ref())
        self.assertIsNotNone(registration_ref())
        self.assertIsNotNone(operation_ref())
        self.assertIs(registration_ref().buffers["window-0"], window)
        # Dropping the record while the state is undisposed needs
        # external fence evidence; with it, the graph is truly released.
        with self.assertRaises(ValueError):
            release_retained(retainment)
        self.assertTrue(
            release_retained(retainment, external_fence="host abort record #42")
        )
        del retainment
        gc.collect()
        self.assertIsNone(agent_ref())
        self.assertIsNone(registration_ref())
        self.assertIsNone(operation_ref())

    def test_retention_uses_operation_identity_until_the_record_is_removed(self):
        from copy import copy

        from gpustack_model_transfer.backends.nixl.runtime import (
            release_retained,
            retain_operation,
            retained_registrations,
        )

        api, agent = real_api()
        registration = api.register_memory(agent, {"window-0": bytearray(32)})
        operation = submit(api, agent, registration)
        # Equal handle and graph fields do not make a distinct operation
        # the same owner. This is a retention test, not a native repost.
        other_operation = copy(operation)
        before = len(retained_registrations())
        try:
            first = retain_operation(operation, "first observation failed")
            self.assertIs(retain_operation(operation, "retry failed"), first)
            self.assertEqual(first.detail, "first observation failed")
            self.assertEqual(len(retained_registrations()), before + 1)
            other = retain_operation(other_operation, "other observation failed")
            self.assertIsNot(other, first)
            self.assertIs(other.handles["operation"], other_operation)
            self.assertEqual(len(retained_registrations()), before + 2)
            self.assertTrue(
                release_retained(first, external_fence="test-only stub disposal")
            )
            renewed = retain_operation(operation, "new observation failed")
            self.assertIsNot(renewed, first)
            self.assertIs(renewed.handles["operation"], operation)
            self.assertEqual(len(retained_registrations()), before + 2)
            self.assertIs(retain_operation(other_operation, "retry"), other)
        finally:
            for record in retained_registrations()[before:]:
                release_retained(record, external_fence="test-only stub disposal")

    def test_repeated_status_errors_retain_until_explicit_completion(self):
        from gpustack_model_transfer.backends.nixl.runtime import (
            release_retained,
            retained_registrations,
        )

        for state, expected in (("DONE", "succeeded"), ("ERR", "failed")):
            with self.subTest(final_state=state):
                api, agent = real_api()
                registration = api.register_memory(agent, {"window-0": bytearray(32)})
                operation = submit(api, agent, registration)
                agent.status_raises = StubNixlApiModule.Bindings.nixlNotFoundError(
                    "remote metadata expired"
                )
                before = len(retained_registrations())
                try:
                    for _ in range(3):
                        with self.assertRaises(type(agent.status_raises)):
                            api.poll(agent, operation)
                        self.assertFalse(operation.terminal)
                        self.assertFalse(operation.released)
                        self.assertEqual(agent.released, [])
                        self.assertEqual(len(retained_registrations()), before + 1)
                    agent.status_raises = None
                    self.assertEqual(api.poll(agent, operation).state, "pending")
                    self.assertFalse(operation.terminal)
                    agent.status_answer = state
                    self.assertEqual(api.poll(agent, operation).state, expected)
                    self.assertTrue(operation.terminal)
                    self.assertEqual(agent.released, [operation.handle])
                    for record in retained_registrations()[before:]:
                        self.assertTrue(release_retained(record))
                finally:
                    for record in retained_registrations()[before:]:
                        release_retained(
                            record, external_fence="test-only stub disposal"
                        )

    def test_observation_error_preserves_prior_completion_when_release_was_refused(
        self,
    ):
        from gpustack_model_transfer.backends.nixl.runtime import (
            release_retained,
            retained_registrations,
        )

        api, agent = real_api()
        registration = api.register_memory(agent, {"window-0": bytearray(32)})
        operation = submit(api, agent, registration)
        agent.status_answer = "DONE"
        agent.release_fails = RuntimeError("release refused")
        before = len(retained_registrations())
        try:
            self.assertEqual(api.poll(agent, operation).state, "succeeded")
            self.assertTrue(operation.terminal)
            self.assertFalse(operation.released)
            agent.status_raises = RuntimeError("observation unavailable")
            with self.assertRaises(RuntimeError):
                api.poll(agent, operation)
            self.assertTrue(operation.terminal)
            self.assertFalse(operation.released)
            releases = [c for c in agent.calls if c[0] == "release_xfer_handle"]
            self.assertEqual(len(releases), 1)
            api.release_operation(agent, operation)
            self.assertTrue(operation.released)
            for record in retained_registrations()[before:]:
                self.assertTrue(release_retained(record))
        finally:
            for record in retained_registrations()[before:]:
                release_retained(record, external_fence="test-only stub disposal")

    def test_status_error_never_attempts_a_refused_release(self):
        from gpustack_model_transfer.backends.nixl.errors import FenceRequiredError
        from gpustack_model_transfer.backends.nixl.runtime import (
            release_retained,
            retained_registrations,
        )

        api, agent = real_api()
        registration = api.register_memory(agent, {"window-0": bytearray(32)})
        operation = submit(api, agent, registration)
        agent.status_raises = StubNixlApiModule.Bindings.nixlBackendError(
            "one native request failed while another remained active"
        )
        release_failure = RuntimeError("native release refused")
        agent.release_fails = release_failure
        before = len(retained_registrations())
        try:
            with self.assertRaises(type(agent.status_raises)):
                api.poll(agent, operation)
            self.assertFalse(operation.terminal)
            self.assertIs(agent.release_fails, release_failure)
            with self.assertRaises(FenceRequiredError):
                api.release_operation(agent, operation)
            self.assertEqual(
                [call for call in agent.calls if call[0] == "release_xfer_handle"], []
            )
            with self.assertRaises(RuntimeError):
                api.release_operation(
                    agent, operation, external_fence="test-only completed stub fence"
                )
            self.assertFalse(operation.released)
            api.release_operation(
                agent, operation, external_fence="test-only completed stub fence"
            )
            self.assertTrue(operation.released)
            for record in retained_registrations()[before:]:
                self.assertTrue(release_retained(record))
        finally:
            for record in retained_registrations()[before:]:
                release_retained(record, external_fence="test-only stub disposal")

    def test_an_err_answer_at_post_is_terminal_failed_and_releases_once(self):
        api, agent = real_api()
        agent.transfer_state = "ERR"
        registration = api.register_memory(agent, {"window-0": bytearray(32)})
        operation = submit(api, agent, registration)
        outcome = api.poll(agent, operation)
        self.assertEqual(outcome.state, "failed")
        self.assertIn("ERR at post", outcome.detail)
        self.assertEqual(len(agent.released), 1)

    def test_an_unknown_posted_word_holds_without_touching_the_handle(self):
        api, agent = real_api()
        agent.transfer_state = "MAYBE"
        registration = api.register_memory(agent, {"window-0": bytearray(32)})
        operation = submit(api, agent, registration)
        outcome = api.poll(agent, operation)
        self.assertEqual(outcome.state, "pending")
        status_calls = [call for call in agent.calls if call[0] == "check_xfer_state"]
        self.assertEqual(status_calls, [])
        self.assertEqual(
            [call for call in agent.calls if call[0] == "release_xfer_handle"], []
        )

    def test_a_done_at_post_with_a_refused_release_succeeds_and_retains(self):
        # The consumer ownership line: DONE is the backend's completion,
        # so the window's bytes are final and success is reported — but
        # the retained HANDLE stays recorded until disposed.
        from gpustack_model_transfer.backends.nixl.runtime import (
            release_retained,
            retained_registrations,
        )

        api, agent = real_api()
        agent.transfer_state = "DONE"
        registration = api.register_memory(agent, {"window-0": bytearray(32)})
        operation = submit(api, agent, registration)
        agent.release_fails = RuntimeError("nixlBackendError: cannot cancel")
        before = len(retained_registrations())
        outcome = api.poll(agent, operation)
        self.assertEqual(outcome.state, "succeeded")
        self.assertIn("retained", outcome.detail)
        self.assertEqual(len(retained_registrations()), before + 1)
        # A retry through the terminal bookkeeping path disposes it.
        api.release_operation(agent, operation)
        self.assertTrue(release_retained(retained_registrations()[-1]))

    def test_poll_refuses_a_caller_chosen_id(self):
        api, agent = real_api()
        with self.assertRaises(ValueError):
            api.poll(agent, "op-1")

    def test_poll_refuses_a_foreign_agent(self):
        api, agent = real_api()
        registration = api.register_memory(agent, {"window-0": bytearray(32)})
        operation = submit(api, agent, registration)
        _other_api, other_agent = real_api()
        with self.assertRaises(ValueError):
            api.poll(other_agent, operation)

    def test_release_operation_refuses_a_foreign_agent(self):
        api, agent = real_api()
        registration = api.register_memory(agent, {"window-0": bytearray(32)})
        operation = submit(api, agent, registration)
        _other_api, other_agent = real_api()
        with self.assertRaises(ValueError):
            api.release_operation(
                other_agent,
                operation,
                external_fence="host abort record #7",
            )

    def test_release_operation_refuses_a_double_release(self):
        api, agent = real_api()
        registration = api.register_memory(agent, {"window-0": bytearray(32)})
        operation = submit(api, agent, registration)
        # The terminal poll releases the handle inside itself; a second
        # release, through any path, is refused.
        agent.status_answer = "DONE"
        api.poll(agent, operation)
        with self.assertRaises(ValueError):
            api.release_operation(agent, operation)
        self.assertEqual(len(agent.released), 1)

    def test_release_operation_on_a_pending_handle_needs_a_qualified_fence(self):
        from gpustack_model_transfer.backends.nixl.errors import FenceRequiredError

        api, agent = real_api()
        registration = api.register_memory(agent, {"window-0": bytearray(32)})
        operation = submit(api, agent, registration)
        # Normal cleanup of a pending operation is refused: a release
        # would request a cancel, and a cancel is not a qualified fence.
        with self.assertRaises(FenceRequiredError):
            api.release_operation(agent, operation)
        self.assertEqual(
            [call for call in agent.calls if call[0] == "release_xfer_handle"], []
        )
        # With documented external fence evidence the disposal proceeds.
        api.release_operation(agent, operation, external_fence="host abort record #9")
        self.assertEqual(len(agent.released), 1)
        with self.assertRaises(ValueError):
            api.poll(agent, operation)

    def test_a_terminal_operation_releases_as_bookkeeping_without_evidence(self):
        # Once a terminal verdict is observed, the release is ordinary
        # bookkeeping of a finished request: a retry needs no external
        # fence evidence, even after a first refused release retained it.
        from gpustack_model_transfer.backends.nixl.runtime import (
            release_retained,
            retained_registrations,
        )

        api, agent = real_api()
        registration = api.register_memory(agent, {"window-0": bytearray(32)})
        operation = submit(api, agent, registration)
        agent.status_answer = "DONE"
        agent.release_fails = RuntimeError("nixlBackendError: cannot cancel")
        outcome = api.poll(agent, operation)
        # DONE is the backend's completion, so success is reported even
        # though the handle could not be freed; the handle record is
        # retained until disposed.
        self.assertEqual(outcome.state, "succeeded")
        self.assertIn("retained", outcome.detail)
        retainment = retained_registrations()[-1]
        self.assertFalse(operation.released)
        api.release_operation(agent, operation)
        self.assertTrue(operation.released)
        self.assertEqual(len(agent.released), 1)
        self.assertTrue(release_retained(retainment))

    def test_a_refused_fence_release_is_retained(self):
        from gpustack_model_transfer.backends.nixl.runtime import (
            release_retained,
            retained_registrations,
        )

        api, agent = real_api()
        registration = api.register_memory(agent, {"window-0": bytearray(32)})
        operation = submit(api, agent, registration)
        agent.release_fails = RuntimeError("nixlBackendError: cannot cancel")
        before = len(retained_registrations())
        with self.assertRaises(RuntimeError):
            api.release_operation(
                agent, operation, external_fence="host abort record #11"
            )
        self.assertEqual(len(retained_registrations()), before + 1)
        retainment = retained_registrations()[-1]
        self.assertIs(retainment.handles["operation"], operation)
        self.assertTrue(
            release_retained(retainment, external_fence="host abort record #12")
        )


class TestRealPeerMetadata(unittest.TestCase):
    """Metadata and peer primitives carry the pinned answers untouched,
    with the facts stated and nothing claimed beyond them."""

    def test_metadata_is_the_native_bytes_unparsed(self):
        api, agent = real_api()
        self.assertEqual(api.agent_metadata(agent), b"stub-agent-metadata")
        self.assertEqual(
            [call for call in agent.calls if call[0] == "get_agent_metadata"],
            [("get_agent_metadata",)],
        )

    def test_connect_peer_imports_the_bytes_and_returns_the_token(self):
        api, agent = real_api()
        token = api.connect_peer(agent, b"peer-metadata-bytes")
        # The pinned loadRemoteMD binding answers with the remote agent's
        # name as bytes; the wrapper passes it back untouched — no
        # decoding, no stringification.
        self.assertEqual(token, b"stub-remote-agent")
        self.assertEqual(
            [call for call in agent.calls if call[0] == "add_remote_agent"],
            [("add_remote_agent", b"peer-metadata-bytes")],
        )

    def test_connect_peer_refuses_non_bytes(self):
        api, agent = real_api()
        with self.assertRaises(ValueError):
            api.connect_peer(agent, "peer-metadata-as-str")

    def test_disconnect_peer_uses_the_pinned_invalidation(self):
        api, agent = real_api()
        api.disconnect_peer(agent, b"stub-remote-agent")
        self.assertEqual(
            [call for call in agent.calls if call[0] == "remove_remote_agent"],
            [("remove_remote_agent", b"stub-remote-agent")],
        )


if __name__ == "__main__":
    unittest.main()
