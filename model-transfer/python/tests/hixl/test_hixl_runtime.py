"""Tests for the HIXL runtime seam: version gate, addressing, mapping."""

import unittest


class TestVersionPin(unittest.TestCase):
    def test_the_pinned_baseline_is_the_reviewed_release(self):
        from gpustack_model_transfer.backends.hixl import (
            PINNED_COMMIT,
            PINNED_VERSION,
            SUPPORTED_VERSIONS,
        )

        # The gate certifies the wheel by its installed distribution
        # version (0.0.1, built from the pinned commit); the CANN release
        # the review ran against lives in the commit, not in an attribute.
        self.assertEqual(PINNED_VERSION, "0.0.1")
        self.assertEqual(PINNED_COMMIT, "cd4c5e86f5ca5a654226b0a677eb07915540e03c")
        self.assertEqual(SUPPORTED_VERSIONS, frozenset({"0.0.1"}))

    def test_a_matching_version_loads(self):
        from gpustack_model_transfer.backends.hixl import load_runtime
        from gpustack_model_transfer.backends.hixl.testing import FakeHixlAPI

        api = load_runtime(api=FakeHixlAPI())
        self.assertEqual(api.version, "0.0.1")

    def test_a_mismatched_version_is_a_typed_refusal(self):
        from gpustack_model_transfer.backends.hixl import (
            PINNED_COMMIT,
            VersionMismatchError,
            load_runtime,
        )
        from gpustack_model_transfer.backends.hixl.testing import FakeHixlAPI

        for version in ("0.0.2", "9.2.0", "1.0.0"):
            with self.subTest(version=version):
                with self.assertRaises(VersionMismatchError) as caught:
                    load_runtime(api=FakeHixlAPI(version=version))
                self.assertIn(PINNED_COMMIT[:12], str(caught.exception))

    def test_a_missing_stack_is_a_typed_refusal(self):
        from gpustack_model_transfer.backends.hixl import (
            RuntimeUnavailableError,
            load_runtime,
        )

        with self.assertRaises(RuntimeUnavailableError):
            load_runtime()

    def test_probe_reports_what_is_missing_on_this_host(self):
        from gpustack_model_transfer.backends.hixl import probe_runtime

        readiness = probe_runtime()
        if "hixl" in readiness.missing:
            self.assertFalse(readiness.ready)
            self.assertFalse(readiness.npu_available)


class TestAddressOf(unittest.TestCase):
    """The address a registration pins is the data address, never a
    temporary copy's."""

    def test_a_writable_host_buffer_answers_without_raising(self):
        from gpustack_model_transfer.backends.hixl.runtime import _address_of

        buffer = bytearray(64)
        buffer[0] = 0x5A
        address = _address_of(buffer)
        self.assertIsInstance(address, int)
        self.assertGreater(address, 0)

    def test_a_writable_memoryview_answers_the_same_address(self):
        from ctypes import addressof, c_char

        from gpustack_model_transfer.backends.hixl.runtime import _address_of

        buffer = bytearray(32)
        self.assertEqual(
            _address_of(memoryview(buffer)), addressof(c_char.from_buffer(buffer))
        )

    def test_an_npu_tensor_answers_through_data_ptr(self):
        from gpustack_model_transfer.backends.hixl.runtime import _address_of

        class FakeNpuTensor:
            def data_ptr(self) -> int:
                return 0xCAFEBABE

        self.assertEqual(_address_of(FakeNpuTensor()), 0xCAFEBABE)

    def test_a_readonly_view_is_refused(self):
        from gpustack_model_transfer.backends.hixl.runtime import _address_of

        with self.assertRaises(TypeError):
            _address_of(memoryview(b"readonly"))

    def test_bytes_are_refused_like_any_readonly_source(self):
        from gpustack_model_transfer.backends.hixl.runtime import _address_of

        # A read-only snapshot has no writable storage to register, and a
        # temporary copy's address is freed on return: a dangling pointer
        # is not a stable copied address, so bytes are refused outright.
        with self.assertRaises(TypeError):
            _address_of(b"0123456789abcdef")


class StubHixlModule:
    """The pinned binding's shape (hixl 0.0.1, commit cd4c5e86) for
    mapping tests: Hixl() plus initialize, one register_mem call per
    buffer returning (status, handle), status-returning
    connect/disconnect/deregister_mem, transfer_async/
    get_transfer_status, and the name set the surface check requires."""

    SUCCESS = 0

    class MemDesc:
        def __init__(self, addr, length):
            self.addr = addr
            self.len = length

    class MemType:
        MEM_HOST = "MEM_HOST"
        MEM_DEVICE = "MEM_DEVICE"

    class TransferOp:
        READ = "READ"
        WRITE = "WRITE"

    class TransferStatus:
        WAITING = "WAITING"
        COMPLETED = "COMPLETED"
        TIMEOUT = "TIMEOUT"
        FAILED = "FAILED"

    class TransferOpDesc:
        def __init__(self, local_addr, remote_addr, length):
            self.local_addr = local_addr
            self.remote_addr = remote_addr
            self.len = length

    class Hixl:
        def __init__(self):
            self.calls = []
            self.next_handle = 1000

        def initialize(self, local_engine, options):
            self.calls.append(("initialize", local_engine, dict(options)))
            return StubHixlModule.SUCCESS

        def register_mem(self, desc, mem_type):
            self.calls.append(("register_mem", desc, mem_type))
            self.next_handle += 1
            return (StubHixlModule.SUCCESS, self.next_handle)

        def deregister_mem(self, handle):
            self.calls.append(("deregister_mem", handle))
            return StubHixlModule.SUCCESS

        def connect(self, remote_engine, timeout_in_millis=1000):
            self.calls.append(("connect", remote_engine))
            return StubHixlModule.SUCCESS

        def disconnect(self, remote_engine, timeout_in_millis=1000):
            self.calls.append(("disconnect", remote_engine))
            return StubHixlModule.SUCCESS

        def transfer_async(self, remote_engine, op, op_descs):
            self.calls.append(("transfer_async", remote_engine, op, list(op_descs)))
            return (StubHixlModule.SUCCESS, len(self.calls))

        def get_transfer_status(self, request):
            self.calls.append(("get_transfer_status", request))
            return (StubHixlModule.SUCCESS, StubHixlModule.TransferStatus.COMPLETED)


def real_hixl_api_on(module):
    """A RealHixlAPI bound to a stub module, for refusal-shape tests."""
    from gpustack_model_transfer.backends.hixl.runtime import RealHixlAPI

    api = RealHixlAPI()
    api._hixl = module()
    return api


class TestRealApiSurface(unittest.TestCase):
    """The real wrapper maps onto the pinned binding, or refuses named."""

    def api(self, module=None):
        from gpustack_model_transfer.backends.hixl.runtime import RealHixlAPI

        api = RealHixlAPI()
        api._hixl = module if module is not None else StubHixlModule()
        return api

    def test_a_binding_missing_the_pinned_surface_is_a_typed_refusal(self):
        from gpustack_model_transfer.backends.hixl.runtime import (
            PINNED_COMMIT,
            RealHixlAPI,
            VersionMismatchError,
        )

        class Missing:
            pass

        api = RealHixlAPI()
        with self.assertRaises(VersionMismatchError) as caught:
            api._check_surface(Missing)
        self.assertIn("Hixl", str(caught.exception))
        self.assertIn(PINNED_COMMIT[:12], str(caught.exception))

        class Partial:
            class Hixl:
                def connect(self, remote_engine, timeout_in_millis=1000):
                    return None

        with self.assertRaises(VersionMismatchError) as caught:
            api._check_surface(Partial())
        self.assertIn("register_mem", str(caught.exception))

    def test_create_agent_requires_a_local_engine_identity(self):
        api = self.api()
        with self.assertRaises(ValueError):
            api.create_agent("reader", {})

    def test_create_agent_initializes_with_the_local_engine(self):
        api = self.api()
        agent = api.create_agent("reader", {"local_engine": "reader@node-a"})
        self.assertEqual(agent.client.calls[0], ("initialize", "reader@node-a", {}))
        self.assertEqual(agent.name, "reader")

    def test_create_agent_refuses_a_failed_initialize(self):
        class Refusing(StubHixlModule):
            class Hixl(StubHixlModule.Hixl):
                def initialize(self, local_engine, options):
                    return 3

        with self.assertRaises(RuntimeError):
            real_hixl_api_on(Refusing).create_agent(
                "reader", {"local_engine": "reader@node-a"}
            )

    def test_initialize_options_must_be_a_str_map(self):
        api = self.api()
        with self.assertRaises(ValueError):
            api.create_agent(
                "reader", {"local_engine": "e", "initialize_options": {"k": 1}}
            )

    def test_registration_registers_each_window_with_its_own_handle(self):
        api = self.api()
        agent = api.create_agent("reader", {"local_engine": "reader@node-a"})
        registration = api.register_memory(
            agent, {"window-0": bytearray(64), "window-1": bytearray(32)}
        )
        register_calls = [
            call for call in agent.client.calls if call[0] == "register_mem"
        ]
        self.assertEqual(len(register_calls), 2)
        first_desc, first_type = register_calls[0][1], register_calls[0][2]
        self.assertIsInstance(first_desc, StubHixlModule.MemDesc)
        self.assertEqual(first_desc.len, 64)
        self.assertEqual(first_type, StubHixlModule.MemType.MEM_HOST)
        self.assertEqual(sorted(registration.handles.values()), [1001, 1002])
        self.assertEqual(registration.windows["window-0"]["len"], 64)
        self.assertEqual(registration.windows["window-0"]["type"], "HOST")

    def test_a_cpu_tensor_registers_as_host_memory_with_true_extent(self):
        # A CPU tensor also answers data_ptr(); the device contract, not
        # the pointer's existence, decides the memory class, and the
        # extent is numel*element_size, never len().
        api = self.api()

        class FakeCpuTensor:
            device = type("Device", (), {"type": "cpu"})()

            def data_ptr(self):
                return 0x1111

            def numel(self):
                return 10

            def element_size(self):
                return 4

            def is_contiguous(self):
                return True

        agent = api.create_agent("reader", {"local_engine": "reader@node-a"})
        registration = api.register_memory(agent, {"w": FakeCpuTensor()})
        kind, desc, mem_type = agent.client.calls[1]
        self.assertEqual(kind, "register_mem")
        self.assertEqual(desc.len, 40)
        self.assertEqual(mem_type, StubHixlModule.MemType.MEM_HOST)
        self.assertEqual(
            registration.windows["w"], {"addr": 0x1111, "len": 40, "type": "HOST"}
        )

    def test_a_device_tensor_registers_as_device_memory(self):
        api = self.api()

        class FakeNpuTensor:
            device = type("Device", (), {"type": "npu"})()

            def data_ptr(self):
                return 0x2222

            def numel(self):
                return 3

            def element_size(self):
                return 2

            def is_contiguous(self):
                return True

        agent = api.create_agent("reader", {"local_engine": "reader@node-a"})
        registration = api.register_memory(agent, {"w": FakeNpuTensor()})
        _kind, _desc, mem_type = agent.client.calls[1]
        self.assertEqual(mem_type, StubHixlModule.MemType.MEM_DEVICE)
        self.assertEqual(registration.windows["w"]["type"], "DEVICE")

    def test_a_noncontiguous_tensor_is_refused(self):
        class Strided:
            device = type("Device", (), {"type": "cpu"})()

            def data_ptr(self):
                return 0x3333

            def numel(self):
                return 4

            def element_size(self):
                return 2

            def is_contiguous(self):
                return False

        with self.assertRaises(TypeError):
            self.api().register_memory(None, {"w": Strided()})

    def test_registration_rolls_back_earlier_handles_on_a_later_refusal(self):
        api = self.api()

        class HalfRefusing(StubHixlModule):
            class Hixl(StubHixlModule.Hixl):
                def register_mem(self, desc, mem_type):
                    if len([c for c in self.calls if c[0] == "register_mem"]) >= 1:
                        return (2, 0)
                    self.calls.append(("register_mem", desc, mem_type))
                    return (StubHixlModule.SUCCESS, 7001)

        api = real_hixl_api_on(HalfRefusing)
        agent = api.create_agent("reader", {"local_engine": "reader@node-a"})
        with self.assertRaises(RuntimeError):
            api.register_memory(agent, {"w0": bytearray(16), "w1": bytearray(16)})
        rollbacks = [call for call in agent.client.calls if call[0] == "deregister_mem"]
        self.assertEqual(rollbacks, [("deregister_mem", 7001)])

    def test_registration_rollback_refusal_retains_handles_and_buffers(self):
        from gpustack_model_transfer.backends.hixl.runtime import (
            RegistrationRetainedError,
            release_retained,
            retained_registrations,
        )

        api = self.api()

        class Stuck(StubHixlModule):
            class Hixl(StubHixlModule.Hixl):
                def register_mem(self, desc, mem_type):
                    if len([c for c in self.calls if c[0] == "register_mem"]) >= 1:
                        return (2, 0)
                    self.calls.append(("register_mem", desc, mem_type))
                    return (StubHixlModule.SUCCESS, 7001)

                def deregister_mem(self, handle):
                    return 9

        api = real_hixl_api_on(Stuck)
        agent = api.create_agent("reader", {"local_engine": "reader@node-a"})
        window = bytearray(16)
        before = len(retained_registrations())
        with self.assertRaises(RegistrationRetainedError) as caught:
            api.register_memory(agent, {"w0": window, "w1": bytearray(16)})
        retainment = caught.exception.retainment
        # The exact native handle and a strong buffer reference are held
        # by a live quarantine owner: naming the leak would retain nothing.
        self.assertEqual(retainment.handles, {"w0": 7001})
        self.assertIs(retainment.buffers["w0"], window)
        self.assertEqual(len(retained_registrations()), before + 1)
        self.assertTrue(release_retained(retainment))

    def test_deregister_refusal_retains_the_handle_for_a_retry(self):
        from gpustack_model_transfer.backends.hixl.runtime import (
            RegistrationRetainedError,
        )

        api = self.api()

        class Refusing(StubHixlModule):
            class Hixl(StubHixlModule.Hixl):
                deregister_calls = 0

                def deregister_mem(self, handle):
                    type(self).deregister_calls += 1
                    if type(self).deregister_calls == 1:
                        return 8
                    return StubHixlModule.SUCCESS

        api = real_hixl_api_on(Refusing)
        agent = api.create_agent("reader", {"local_engine": "reader@node-a"})
        registration = api.register_memory(agent, {"w0": bytearray(16)})
        with self.assertRaises(RegistrationRetainedError) as caught:
            api.deregister(agent, registration)
        # The refused handle stays on the registration: a retry cannot
        # double-free, and the refusal cannot silently drop it.
        self.assertEqual(registration.handles, {"w0": 1001})
        self.assertIn("w0", caught.exception.retainment.handles)
        api.deregister(agent, registration)
        self.assertEqual(registration.handles, {})

    def test_a_register_refusal_is_a_loud_error(self):
        class Refusing(StubHixlModule):
            class Hixl(StubHixlModule.Hixl):
                def register_mem(self, desc, mem_type):
                    return (2, 0)

        api = real_hixl_api_on(Refusing)
        agent = api.create_agent("reader", {"local_engine": "reader@node-a"})
        with self.assertRaises(RuntimeError):
            api.register_memory(agent, {"window-0": bytearray(32)})

    def test_connect_checks_status_and_returns_the_engine_identity(self):
        api = self.api()
        agent = api.create_agent("reader", {"local_engine": "reader@node-a"})
        connection = api.connect(agent, {"endpoint": "hixl://source:9700"})
        self.assertEqual(connection, "hixl://source:9700")

        class Refusing(StubHixlModule):
            class Hixl(StubHixlModule.Hixl):
                def connect(self, remote_engine, timeout_in_millis=1000):
                    return 4

        refusing = real_hixl_api_on(Refusing)
        refusing_agent = refusing.create_agent(
            "reader", {"local_engine": "reader@node-a"}
        )
        with self.assertRaises(RuntimeError):
            refusing.connect(refusing_agent, {"endpoint": "hixl://source:9700"})

    def test_read_submits_the_pinned_pull_shape(self):
        api = self.api()
        agent = api.create_agent("reader", {"local_engine": "reader@node-a"})
        registration = api.register_memory(agent, {"window-0": bytearray(32)})
        connection = api.connect(agent, {"endpoint": "hixl://source:9700"})
        handle = api.submit_read(
            agent,
            registration,
            connection,
            "window-0",
            {"addr": 0x500000000000, "length": 8},
            8,
        )
        kind, remote_engine, op, descs = agent.client.calls[-1]
        self.assertEqual(kind, "transfer_async")
        self.assertEqual(remote_engine, "hixl://source:9700")
        self.assertEqual(op, StubHixlModule.TransferOp.READ)
        self.assertEqual(len(descs), 1)
        self.assertEqual(descs[0].local_addr, registration.windows["window-0"]["addr"])
        self.assertEqual(descs[0].remote_addr, 0x500000000000)
        self.assertEqual(descs[0].len, 8)
        self.assertIsInstance(handle, str)

    def test_write_is_refused_on_the_pinned_binding(self):
        from gpustack_model_transfer.backends.hixl.errors import TransferRefusedError

        api = self.api()
        agent = api.create_agent("reader", {"local_engine": "reader@node-a"})
        registration = api.register_memory(agent, {"window-0": bytearray(32)})
        connection = api.connect(agent, {"endpoint": "hixl://source:9700"})
        with self.assertRaises(TransferRefusedError):
            api.submit_write(
                agent,
                registration,
                connection,
                "window-0",
                {"addr": 0x500000000000, "length": 8},
                8,
            )
        self.assertFalse(
            [call for call in agent.client.calls if call[0] == "transfer_async"]
        )

    def test_a_source_without_an_address_is_refused(self):
        api = self.api()
        agent = api.create_agent("reader", {"local_engine": "reader@node-a"})
        registration = api.register_memory(agent, {"window-0": bytearray(32)})
        connection = api.connect(agent, {"endpoint": "hixl://source:9700"})
        with self.assertRaises(ValueError):
            api.submit_read(
                agent,
                registration,
                connection,
                "window-0",
                {"file": "checkpoint.safetensors", "offset": 0},
                8,
            )

    def test_a_transfer_over_the_window_is_refused(self):
        api = self.api()
        agent = api.create_agent("reader", {"local_engine": "reader@node-a"})
        registration = api.register_memory(agent, {"window-0": bytearray(32)})
        connection = api.connect(agent, {"endpoint": "hixl://source:9700"})
        with self.assertRaises(ValueError):
            api.submit_read(
                agent,
                registration,
                connection,
                "window-0",
                {"addr": 0x500000000000, "length": 8},
                64,
            )

    def test_a_backend_transfer_refusal_is_a_typed_refusal(self):
        from gpustack_model_transfer.backends.hixl.errors import TransferRefusedError

        class Refusing(StubHixlModule):
            class Hixl(StubHixlModule.Hixl):
                def transfer_async(self, remote_engine, op, op_descs):
                    return (5, 0)

        agent = real_hixl_api_on(Refusing).create_agent(
            "reader", {"local_engine": "reader@node-a"}
        )
        registration = real_hixl_api_on(Refusing).register_memory(
            agent, {"window-0": bytearray(32)}
        )
        with self.assertRaises(TransferRefusedError):
            real_hixl_api_on(Refusing).submit_read(
                agent,
                registration,
                "hixl://source:9700",
                "window-0",
                {"addr": 0x500000000000, "length": 8},
                8,
            )

    def test_poll_maps_the_completion_answer(self):
        from gpustack_model_transfer.backends.hixl.runtime import TransferOutcome

        api = real_hixl_api_on(StubHixlModule)
        agent = api.create_agent("reader", {"local_engine": "reader@node-a"})
        outcome = api.poll(agent, "7")
        self.assertIsInstance(outcome, TransferOutcome)
        self.assertEqual(outcome.state, "succeeded")

    def test_poll_keeps_a_timeout_unresolved_and_retained(self):
        # The pinned interface releases a request only on COMPLETED or
        # FAILED; a TIMEOUT stays non-terminal so the caller keeps the
        # operation and its buffers retained until a qualified fence.
        api = self.api()

        class Timing(StubHixlModule):
            class Hixl(StubHixlModule.Hixl):
                def get_transfer_status(self, request):
                    return (
                        StubHixlModule.SUCCESS,
                        StubHixlModule.TransferStatus.TIMEOUT,
                    )

        api = real_hixl_api_on(Timing)
        agent = api.create_agent("reader", {"local_engine": "reader@node-a"})
        outcome = api.poll(agent, "7")
        self.assertEqual(outcome.state, "pending")
        self.assertIn("not released", outcome.detail)

    def test_poll_maps_an_unresolved_enum_to_pending(self):
        api = self.api()

        class Odd(StubHixlModule):
            class Hixl(StubHixlModule.Hixl):
                def get_transfer_status(self, request):
                    return (StubHixlModule.SUCCESS, "SOMETHING_NEW")

        api = real_hixl_api_on(Odd)
        agent = api.create_agent("reader", {"local_engine": "reader@node-a"})
        outcome = api.poll(agent, "7")
        self.assertEqual(outcome.state, "pending")

    def test_poll_maps_waiting_to_pending(self):
        class Waiting(StubHixlModule):
            class Hixl(StubHixlModule.Hixl):
                def get_transfer_status(self, request):
                    return (
                        StubHixlModule.SUCCESS,
                        StubHixlModule.TransferStatus.WAITING,
                    )

        waiting_api = real_hixl_api_on(Waiting)
        waiting_agent = waiting_api.create_agent(
            "reader", {"local_engine": "reader@node-a"}
        )
        self.assertEqual(waiting_api.poll(waiting_agent, "7").state, "pending")

    def test_poll_refuses_a_failed_observation_instead_of_inventing_state(self):
        api = self.api()

        class Blind(StubHixlModule):
            class Hixl(StubHixlModule.Hixl):
                def get_transfer_status(self, request):
                    return (6, StubHixlModule.TransferStatus.WAITING)

        api = real_hixl_api_on(Blind)
        agent = api.create_agent("reader", {"local_engine": "reader@node-a"})
        with self.assertRaises(RuntimeError):
            api.poll(agent, "7")

    def test_a_non_numeric_operation_id_is_refused(self):
        api = self.api()
        with self.assertRaises(ValueError):
            api.poll(None, "op-1")

    def test_teardown_uses_the_pinned_disconnect_and_deregister_mem(self):
        api = self.api()
        agent = api.create_agent("reader", {"local_engine": "reader@node-a"})
        registration = api.register_memory(agent, {"window-0": bytearray(32)})
        connection = api.connect(agent, {"endpoint": "hixl://source:9700"})
        api.disconnect(agent, connection)
        window_handle = registration.handles["window-0"]
        api.deregister(agent, registration)
        # A successful deregistration releases its handle exactly once.
        self.assertEqual(registration.handles, {})
        disconnects = [call for call in agent.client.calls if call[0] == "disconnect"]
        deregisters = [
            call for call in agent.client.calls if call[0] == "deregister_mem"
        ]
        self.assertEqual(disconnects, [("disconnect", "hixl://source:9700")])
        self.assertEqual(deregisters, [("deregister_mem", window_handle)])


if __name__ == "__main__":
    unittest.main()
