"""Stage diagnostics contract tests.

The bounded HIXL stage vocabulary (connect, submit, poll, disconnect) must
stamp the exception the backend raises — for returned non-success statuses
and raised native exceptions alike — ride the registry's report to the
worker, and reach the quarantine wire as one of four exact fixed reason
strings, without ever carrying exception text, without changing any charge
or retention semantics, and without trusting a forged stage attribute.
"""

import threading
from collections import deque
from types import SimpleNamespace

import pytest

from gpustack_model_transfer.source.backend import (
    HixlSourceBackend,
    SourceRegistration,
)
from gpustack_model_transfer.source.native_worker import NativeSourceWorker
from gpustack_model_transfer.source.transfer_service import OperationRegistry

_HIXL_STAGES = ("connect", "submit", "poll", "disconnect")


class _Engine:
    """Failing HIXL engine: one stage fails by status or by raising."""

    def __init__(self, fail_at, mode="status", error=None):
        self.fail_at = fail_at
        self.mode = mode
        self.error = error

    def connect(self, endpoint):
        if self.fail_at == "connect":
            if self.mode == "raise":
                raise self.error
            return 1
        return 0

    def transfer_async(self, endpoint, direction, descriptors):
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


def _headroom_backend(engine):
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


def _drive(backend, registration, fail_at):
    operation = backend.post_write(
        registration,
        "file",
        0,
        4,
        {
            "endpoint": "endpoint-red",
            "address": 2000,
            "length": 128,
        },
    )
    if fail_at == "poll":
        backend.poll_write(operation)
    elif fail_at == "disconnect":
        backend.close_write(operation)


class _Hostile(Exception):
    """An exception object that refuses new attributes."""

    def __setattr__(self, name, value):
        raise RuntimeError(f"cannot set {name}")


@pytest.mark.parametrize("stage", _HIXL_STAGES)
@pytest.mark.parametrize("mode", ["status", "raise"])
def test_backend_stamps_bounded_stage(stage, mode, capsys):
    engine = _Engine(stage, mode, RuntimeError(f"{stage} exploded"))
    backend, registration = _headroom_backend(engine)
    with pytest.raises(Exception) as excinfo:
        _drive(backend, registration, stage)
    stamped = getattr(excinfo.value, "gpustack_native_stage", None)
    assert stamped == stage, (
        f"the {stage} failure must stamp the bounded stage label; got {stamped!r}"
    )


def test_backend_reraises_identical_exception_with_message_intact():
    engine = _Engine("submit", "raise", RuntimeError("submit exploded"))
    backend, registration = _headroom_backend(engine)
    with pytest.raises(RuntimeError) as excinfo:
        backend.post_write(
            registration,
            "file",
            0,
            4,
            {
                "endpoint": "endpoint-red",
                "address": 2000,
                "length": 128,
            },
        )
    assert excinfo.value is engine.error
    assert str(excinfo.value) == "submit exploded"
    assert excinfo.value.args == ("submit exploded",)


def test_hostile_exception_object_does_not_block_the_failure():
    engine = _Engine("submit", "raise", _Hostile("submit exploded"))
    backend, registration = _headroom_backend(engine)
    with pytest.raises(_Hostile) as excinfo:
        backend.post_write(
            registration,
            "file",
            0,
            4,
            {
                "endpoint": "endpoint-red",
                "address": 2000,
                "length": 128,
            },
        )
    assert str(excinfo.value) == "submit exploded"
    assert getattr(excinfo.value, "gpustack_native_stage", None) is None


def test_hostile_setattr_raising_baseexception_does_not_mask_the_original():
    class _HostileBase(BaseException):
        pass

    class _RefusingException(Exception):
        def __setattr__(self, name, value):
            raise _HostileBase("hostile attribute store")

    class _Engine:
        def connect(self, endpoint):
            raise original

        def transfer_async(self, endpoint, direction, descriptors):
            return 0, 7

        def get_transfer_status(self, handle):
            return 0, "completed"

        def disconnect(self, endpoint):
            return 0

    backend, registration = _headroom_backend(_Engine())
    original = _RefusingException("the original failure")
    with pytest.raises(_RefusingException) as excinfo:
        backend.post_write(
            registration,
            "file",
            0,
            4,
            {
                "endpoint": "endpoint-red",
                "address": 2000,
                "length": 128,
            },
        )
    assert excinfo.value is original, (
        "the hostile attribute store must never replace the original failure"
    )
    assert not isinstance(excinfo.value, _HostileBase)


def test_interrupt_from_native_call_propagates_unstamped():
    engine = _Engine("submit", "raise", KeyboardInterrupt("rank interrupted"))
    backend, registration = _headroom_backend(engine)
    with pytest.raises(KeyboardInterrupt) as excinfo:
        _drive(backend, registration, "submit")
    assert excinfo.value is engine.error
    assert getattr(excinfo.value, "gpustack_native_stage", None) is None


def test_hostile_stage_equality_cannot_block_unknown():
    class _HostileEquality:
        def __eq__(self, other):
            raise RuntimeError("hostile equality")

        def __repr__(self):
            return "<hostile-stage>"

    class _TaggedFailureBackend:
        """A backend whose single post_write raises a pre-tagged failure,
        bypassing the backend's own stamping so the extraction defense is
        exercised directly."""

        def __init__(self, failure):
            self.failure = failure

        def post_write(self, registration, name, offset, length, target):
            raise self.failure

        def poll_write(self, operation):
            raise self.failure

        def close_write(self, operation):
            raise self.failure

    failure = RuntimeError("submit exploded")
    failure.gpustack_native_stage = _HostileEquality()
    backend = _TaggedFailureBackend(failure)
    registration = SimpleNamespace(
        blocks={"file": SimpleNamespace(data_ptr=lambda: 1000, byte_size=128)}
    )
    captures = []
    worker = _Worker(backend, registration, captures)
    registry = OperationRegistry(None)
    registry.jobs["request"] = {
        "state": "dispatching",
        "body": {
            "file": "file",
            "fileOffset": 0,
            "length": 4,
            "target": {"endpoint": "endpoint-red", "address": 2000, "length": 128},
        },
        "owner": ("target", "process", "registration", "window"),
        "common": ("source", "process", "generation", "lease", "session"),
        "worker": worker,
        "native": None,
        "verdict": "",
    }
    registry.queues[worker] = deque(["request"])
    registry.step(worker)
    job = registry.jobs["request"]
    assert job["state"] == "unknown", (
        "a hostile stage object cannot prevent the operation from being marked unknown"
    )
    assert captures == [("native-operation-unknown", None)]


class _Worker:
    """Identity-hashable stand-in the registry's step loop can key on."""

    def __init__(self, backend, registration, captures):
        self.backend = backend
        self.registration = registration
        self.native_operations = 1
        self._lock = threading.Lock()
        self._captures = captures

    def report_unknown(self, reason, stage=None):
        self._captures.append((reason, stage))


def _registry_step_stage(engine, fail_at):
    backend, registration = _headroom_backend(engine)
    captures = []
    worker = _Worker(backend, registration, captures)
    registry = OperationRegistry(None)
    registry.jobs["request"] = {
        "state": "dispatching",
        "body": {
            "file": "file",
            "fileOffset": 0,
            "length": 4,
            "target": {"endpoint": "endpoint-red", "address": 2000, "length": 128},
        },
        "owner": ("target", "process", "registration", "window"),
        "common": ("source", "process", "generation", "lease", "session"),
        "worker": worker,
        "native": None,
        "verdict": "",
    }
    registry.queues[worker] = deque(["request"])
    registry.step(worker)
    job = registry.jobs["request"]
    if job["state"] == "submitted":
        # The poll stage runs on the registry's next pass, after admission.
        registry.step(worker)
        job = registry.jobs["request"]
    return job["state"], captures


@pytest.mark.parametrize("stage", ["submit", "poll"])
def test_registry_step_forwards_bounded_stage(stage):
    engine = _Engine(stage, "status")
    state, captures = _registry_step_stage(engine, stage)
    assert state == "unknown", "the operation must remain unknown"
    assert captures == [("native-operation-unknown", stage)], (
        f"the registry must forward the bounded {stage!r} stage to "
        f"report_unknown; got {captures!r}"
    )


def test_registry_step_without_stage_keeps_legacy_report():
    class _PlainFailure:
        def post_write(self, registration, name, offset, length, target):
            raise RuntimeError("plain wrapper failure")

    backend = _PlainFailure()
    registration = SimpleNamespace(
        blocks={"file": SimpleNamespace(data_ptr=lambda: 1000, byte_size=128)}
    )
    captures = []
    worker = _Worker(backend, registration, captures)
    registry = OperationRegistry(None)
    registry.jobs["request"] = {
        "state": "dispatching",
        "body": {
            "file": "file",
            "fileOffset": 0,
            "length": 4,
            "target": {"endpoint": "endpoint-red", "address": 2000, "length": 128},
        },
        "owner": ("target", "process", "registration", "window"),
        "worker": worker,
        "native": None,
        "verdict": "",
    }
    registry.queues[worker] = deque(["request"])
    registry.step(worker)
    assert captures == [("native-operation-unknown", None)], (
        "a failure without a stamped stage must forward no stage"
    )


def _receipt_worker():
    sent = []
    worker = NativeSourceWorker(
        SimpleNamespace(quarantine=sent.append),
        None,
        backend="hixl",
        allocator=None,
        worker_instance_id="receipt-test",
    )
    return worker, sent


def test_native_worker_maps_stage_to_bounded_reason():
    worker, sent = _receipt_worker()
    worker.report_unknown("native-operation-unknown", stage="submit")
    assert sent == ["hixl-submit"], (
        "the quarantine wire must carry the exact stage-coded reason"
    )


def test_native_worker_legacy_call_keeps_wire_shape():
    worker, sent = _receipt_worker()
    worker.report_unknown("native-operation-unknown")
    assert sent == ["unknown"], (
        "a call without a stage keeps the legacy quarantine reason"
    )


def test_native_worker_non_hixl_reasons_keep_current_semantics():
    worker, sent = _receipt_worker()
    worker._quarantine("worker-failure")
    worker._quarantine("stale-generation")
    assert sent == ["worker-failure", "stale-generation"]


def test_native_worker_forged_stage_is_not_trusted():
    worker, sent = _receipt_worker()
    worker.report_unknown("native-operation-unknown", stage="hixl-<script>")
    assert sent == ["unknown"], (
        "a forged stage outside the vocabulary must not reach the wire"
    )


@pytest.mark.parametrize(
    "mode,error",
    [
        ("status", None),
        ("raise", RuntimeError("connect failed")),
        ("raise", KeyboardInterrupt("interrupted")),
    ],
)
def test_hixl_connect_failure_balances_file_reference(mode, error):
    backend, registration = _headroom_backend(_Engine("connect", mode, error))
    with pytest.raises(BaseException) as failure:
        _drive(backend, registration, "connect")
    if error is not None:
        assert failure.value is error
    assert not backend._in_use_counts
    assert not backend._peers


def test_hixl_submit_failure_keeps_file_reference():
    backend, registration = _headroom_backend(
        _Engine("submit", "raise", RuntimeError("submit uncertain"))
    )
    with pytest.raises(RuntimeError, match="submit uncertain"):
        _drive(backend, registration, "submit")
    assert backend._in_use_counts == {"file": 1}
    assert backend._peers == ["endpoint-red"]
