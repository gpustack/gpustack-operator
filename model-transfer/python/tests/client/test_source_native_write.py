"""The pinned source primitives post WRITE from the selected file offset."""

from types import SimpleNamespace

import pytest

from gpustack_model_transfer.backends.nixl.runtime import RealNixlAPI
from gpustack_model_transfer.core.errors import SourceUnavailableError
from gpustack_model_transfer.source.backend import HixlSourceBackend, SourceRegistration


def test_nixl_source_write_uses_local_offset_and_bounds():
    class Agent:
        def __init__(self):
            self.descriptors = []
            self.directions = []

        def get_xfer_descs(self, data, kind):
            self.descriptors.append((data, kind))
            return data

        def initialize_xfer(self, direction, local, remote, connection, notif_msg):
            self.directions.append(direction)
            return object()

        def transfer(self, handle):
            return "PROC"

    api = RealNixlAPI()
    api._nixl_api = object()
    agent = Agent()
    registration = SimpleNamespace(
        windows={"file": {"addr": 1000, "len": 100, "dev_id": 0, "mem_type": "DRAM"}},
        buffers={},
        agent=agent,
    )
    remote = {"addr": 2000, "len": 32, "mem_type": "DRAM", "device_id": 0}
    api.submit_source_write(
        agent, registration, b"target", "file", remote, 12, local_offset=21
    )
    assert agent.directions == ["WRITE"]
    assert agent.descriptors[0] == ([(1021, 12, 0)], "DRAM")
    with pytest.raises(ValueError):
        api.submit_source_write(
            agent, registration, b"target", "file", remote, 12, local_offset=90
        )
    assert agent.directions == ["WRITE"]


def test_hixl_source_write_is_submitted_on_source_engine():
    calls = []

    class Engine:
        def connect(self, endpoint):
            calls.append(("connect", endpoint))
            return 0

        def transfer_async(self, endpoint, direction, descriptors):
            calls.append(("post", endpoint, direction, descriptors))
            return 0, 7

        def get_transfer_status(self, handle):
            return 0, "completed"

        def disconnect(self, endpoint):
            calls.append(("disconnect", endpoint))
            return 0

    api = SimpleNamespace(
        SUCCESS=0,
        TransferOp=SimpleNamespace(WRITE="WRITE"),
        TransferStatus=SimpleNamespace(COMPLETED="completed", FAILED="failed"),
        TransferOpDesc=lambda source, target, size: (source, target, size),
    )
    backend = HixlSourceBackend()
    backend._engine = Engine()
    backend._hixl = api
    block = SimpleNamespace(data_ptr=lambda: 1000, byte_size=128)
    registration = SourceRegistration("hixl", {"file": 1}, {"file": block})
    operation = backend.post_write(
        registration,
        "file",
        20,
        12,
        {"endpoint": "127.0.0.1:1234", "address": 2000, "length": 128},
    )
    assert calls[1] == ("post", "127.0.0.1:1234", "WRITE", [(1020, 2000, 12)])
    assert backend.poll_write(operation) == "succeeded"
    backend.close_write(operation)
    assert calls[-1] == ("disconnect", "127.0.0.1:1234")


def test_hixl_source_runtime_initialization_does_not_need_torch(monkeypatch):
    import sys

    from gpustack_model_transfer.backends.hixl import runtime

    class Engine:
        def initialize(self, address, options):
            assert address == "127.0.0.1:1234"
            return 0

        def finalize(self):
            return None

    fake_module = SimpleNamespace(Hixl=Engine)

    def refuse_version_check():
        raise AssertionError("source startup must not inspect the version")

    monkeypatch.setattr(runtime, "_hixl_version", refuse_version_check)
    monkeypatch.setattr(runtime.RealHixlAPI, "_require", lambda self: fake_module)
    monkeypatch.setitem(sys.modules, "torch", None)
    monkeypatch.setitem(sys.modules, "torch_npu", None)
    backend = HixlSourceBackend(engine_address="127.0.0.1:1234")
    backend.start()
    backend.finalize()


def test_hixl_failed_post_keeps_returned_handle_and_peer():
    class Engine:
        def connect(self, endpoint):
            return 0

        def transfer_async(self, endpoint, direction, descriptors):
            return 1, 777

    api = SimpleNamespace(
        SUCCESS=0,
        TransferOp=SimpleNamespace(WRITE="WRITE"),
        TransferOpDesc=lambda *args: args,
    )
    backend = HixlSourceBackend()
    backend._engine = Engine()
    backend._hixl = api
    block = SimpleNamespace(data_ptr=lambda: 1000, byte_size=128)
    registration = SourceRegistration("hixl", {"file": 1}, {"file": block})
    with pytest.raises(Exception):
        backend.post_write(
            registration,
            "file",
            1,
            4,
            {"endpoint": "127.0.0.1:1234", "address": 2000, "length": 128},
        )
    assert backend._uncertain_operations == [777]
    assert backend._peers == ["127.0.0.1:1234"]


def _hixl_write_backend(engine):
    api = SimpleNamespace(
        SUCCESS=0,
        TransferOp=SimpleNamespace(WRITE="WRITE"),
        TransferStatus=SimpleNamespace(COMPLETED="completed", FAILED="failed"),
        TransferOpDesc=lambda *args: args,
    )
    backend = HixlSourceBackend()
    backend._engine = engine
    backend._hixl = api
    block = SimpleNamespace(data_ptr=lambda: 1000, byte_size=128)
    registration = SourceRegistration("hixl", {"file": 1}, {"file": block})
    return backend, registration


def _write_target():
    return {"endpoint": "127.0.0.1:1234", "address": 2000, "length": 128}


_HIXL_STAGES = ("connect", "submit", "poll", "disconnect")


class _StageEngine:
    """Fake HIXL engine: one stage fails by status or by raising."""

    def __init__(self, fail_at, mode, error=None):
        self.fail_at = fail_at
        self.mode = mode
        self.error = error
        self.posts = 0

    def connect(self, endpoint):
        if self.fail_at == "connect":
            if self.mode == "raise":
                raise self.error
            return 1
        return 0

    def transfer_async(self, endpoint, direction, descriptors):
        self.posts += 1
        if self.fail_at == "submit":
            if self.mode == "raise":
                raise self.error
            return 1, 777
        return 0, 7

    def get_transfer_status(self, handle):
        if self.fail_at == "poll":
            if self.mode == "raise":
                raise self.error
            return 1, None
        return 0, "completed"

    def disconnect(self, endpoint):
        if self.fail_at == "disconnect":
            if self.mode == "raise":
                raise self.error
            return 1
        return 0


def _drive_write(backend, registration, fail_at):
    operation = backend.post_write(registration, "file", 0, 4, _write_target())
    if fail_at == "poll":
        backend.poll_write(operation)
    elif fail_at == "disconnect":
        backend.close_write(operation)


def test_hixl_stage_labels_distinguish_connect_from_submit_failure(caplog):
    class Engine:
        def __init__(self, fail_connect):
            self.fail_connect = fail_connect
            self.posts = 0

        def connect(self, endpoint):
            return 1 if self.fail_connect else 0

        def transfer_async(self, endpoint, direction, descriptors):
            self.posts += 1
            return 1, 777

    cases = [
        (True, 0, "hixl source operation failed at stage connect"),
        (False, 1, "hixl source operation failed at stage submit"),
    ]
    observed = []
    for fail_connect, posts, expected_log in cases:
        engine = Engine(fail_connect)
        backend, registration = _hixl_write_backend(engine)
        with caplog.at_level("WARNING"):
            with pytest.raises(SourceUnavailableError):
                backend.post_write(registration, "file", 0, 4, _write_target())
        logs = [record.getMessage() for record in caplog.records]
        caplog.clear()
        assert engine.posts == posts
        assert logs == [expected_log]
        observed.append(logs[0].rsplit("stage ", 1)[-1])
    assert observed == ["connect", "submit"]


def test_hixl_stage_labels_cover_poll_and_disconnect_failure(caplog):
    class Engine:
        def __init__(self, fail_at):
            self.fail_at = fail_at

        def connect(self, endpoint):
            return 0

        def transfer_async(self, endpoint, direction, descriptors):
            return 0, 7

        def get_transfer_status(self, handle):
            if self.fail_at == "poll":
                return 1, None
            return 0, "completed"

        def disconnect(self, endpoint):
            return 1 if self.fail_at == "disconnect" else 0

    cases = [
        ("poll", "hixl source operation failed at stage poll"),
        ("disconnect", "hixl source operation failed at stage disconnect"),
    ]
    observed = []
    for fail_at, expected_log in cases:
        backend, registration = _hixl_write_backend(Engine(fail_at))
        with caplog.at_level("WARNING"):
            operation = backend.post_write(registration, "file", 0, 4, _write_target())
            with pytest.raises(SourceUnavailableError):
                if fail_at == "poll":
                    backend.poll_write(operation)
                else:
                    backend.close_write(operation)
        logs = [record.getMessage() for record in caplog.records]
        caplog.clear()
        assert logs == [expected_log]
        observed.append(logs[0].rsplit("stage ", 1)[-1])
    assert observed == ["poll", "disconnect"]


def test_hixl_successful_source_write_emits_no_stage_warning(caplog):
    class Engine:
        def __init__(self):
            self.posts = 0

        def connect(self, endpoint):
            return 0

        def transfer_async(self, endpoint, direction, descriptors):
            self.posts += 1
            return 0, 7

        def get_transfer_status(self, handle):
            return 0, "completed"

        def disconnect(self, endpoint):
            return 0

    engine = Engine()
    backend, registration = _hixl_write_backend(engine)
    with caplog.at_level("WARNING"):
        operation = backend.post_write(registration, "file", 0, 4, _write_target())
        assert backend.poll_write(operation) == "succeeded"
        backend.close_write(operation)
    assert engine.posts == 1
    assert [record.getMessage() for record in caplog.records] == []


def test_hixl_raised_native_exceptions_emit_the_same_stage_labels(caplog):
    for fail_at in _HIXL_STAGES:
        engine = _StageEngine(fail_at, "raise", RuntimeError(f"{fail_at} exploded"))
        backend, registration = _hixl_write_backend(engine)
        with caplog.at_level("WARNING"):
            with pytest.raises(RuntimeError) as excinfo:
                _drive_write(backend, registration, fail_at)
        logs = [record.getMessage() for record in caplog.records]
        caplog.clear()
        assert excinfo.value is engine.error
        assert logs == [f"hixl source operation failed at stage {fail_at}"]
        assert engine.posts == (0 if fail_at == "connect" else 1)


def test_hixl_raised_exception_text_never_reaches_the_stage_logs(caplog):
    secret = "hixl-secret-payload-8f3a"
    for fail_at in _HIXL_STAGES:
        engine = _StageEngine(
            fail_at, "raise", RuntimeError(f"{fail_at} leaked {secret}")
        )
        backend, registration = _hixl_write_backend(engine)
        with caplog.at_level("WARNING"):
            with pytest.raises(RuntimeError, match=secret):
                _drive_write(backend, registration, fail_at)
        formatted = caplog.text
        messages = [record.getMessage() for record in caplog.records]
        caplog.clear()
        assert messages == [f"hixl source operation failed at stage {fail_at}"]
        assert secret not in formatted
        assert not any(secret in message for message in messages)
        assert backend._uncertain_operations == []
        if fail_at == "connect":
            assert backend._peers == []
        else:
            assert backend._peers == ["127.0.0.1:1234"]


def test_hixl_interrupts_propagate_unchanged_without_stage_logs(caplog):
    for fail_at in _HIXL_STAGES:
        for interrupt in (KeyboardInterrupt, SystemExit):
            engine = _StageEngine(fail_at, "raise", interrupt())
            backend, registration = _hixl_write_backend(engine)
            with caplog.at_level("WARNING"):
                with pytest.raises(interrupt) as excinfo:
                    _drive_write(backend, registration, fail_at)
            logs = [record.getMessage() for record in caplog.records]
            caplog.clear()
            assert excinfo.value is engine.error
            assert logs == []


def test_hixl_target_registration_failure_retains_handle_and_owner(monkeypatch):
    from gpustack_model_transfer import client_native
    from gpustack_model_transfer.backends.hixl import runtime

    class Owner:
        def ensure_current(self):
            pass

        def allocate(self, name, size):
            return SimpleNamespace(data_ptr=lambda: 2 << 20, registration_bytes=size)

    class Engine:
        def initialize(self, endpoint, options):
            return 0

        def register_mem(self, desc, kind):
            return 1, 777

    hixl = SimpleNamespace(
        Hixl=Engine,
        SUCCESS=0,
        MemDesc=lambda a, n: (a, n),
        MemType=SimpleNamespace(MEM_HOST="host"),
    )
    monkeypatch.setattr(client_native, "BorrowedAclOwner", Owner)
    monkeypatch.setattr(
        runtime, "load_runtime", lambda: SimpleNamespace(_require=lambda: hixl)
    )
    target = client_native.TargetNative("hixl", "127.0.0.1", 8)
    with pytest.raises(Exception):
        target.start()
    assert target.registration.handles == {"window": 777}
    assert (
        target.owner is not None
        and target.agent is not None
        and target.block is not None
    )


@pytest.mark.parametrize(
    "target", [{}, {"metadata": "!!!", "address": 1000, "length": 8}]
)
def test_nixl_invalid_metadata_is_refused_before_file_acquisition(target):
    from gpustack_model_transfer.source.backend import (
        NixlSourceBackend,
        SourcePrePostError,
    )

    backend = NixlSourceBackend()
    with pytest.raises(SourcePrePostError):
        backend.post_write(None, "file", 0, 4, target)
    assert not backend.has_active_peers()
