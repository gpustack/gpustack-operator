"""Progress on the owning thread does not pay the idle poll delay."""

import threading
from collections import deque
from types import SimpleNamespace

import pytest

from gpustack_model_transfer.source.native_worker import NativeSourceWorker
from gpustack_model_transfer.source.transfer_service import OperationRegistry


@pytest.mark.parametrize(
    ("progress", "pending", "expected_steps", "expected_wait"),
    [
        ([True, True, False], False, 3, 0),
        ([False], True, 1, 0.002),
        ([False], False, 1, 0.2),
        ([True] * 64, True, 64, 0),
        ([True] * 64, False, 64, 0),
    ],
)
def test_native_worker_steps_until_blocked(
    progress, pending, expected_steps, expected_wait
):
    steps = []
    waits = []
    transitions = iter(progress)

    def step(worker):
        steps.append(worker)
        return next(transitions)

    worker = SimpleNamespace(
        state="serving",
        operations=SimpleNamespace(step=step, has_pending=lambda _: pending),
        _drain_requested=threading.Event(),
        _stop_event=SimpleNamespace(wait=waits.append),
        _maintain_blocks=lambda: None,
        poll_interval=0.2,
    )
    assert NativeSourceWorker.once(worker) == "serving"
    assert len(steps) == expected_steps
    assert waits == [expected_wait]


def test_registry_reports_progress_and_pending_native_completion():
    class Worker:
        pass

    worker = Worker()
    verdicts = iter(["pending", "succeeded"])
    closed = []
    worker.backend = SimpleNamespace(
        post_write=lambda *args: "native-operation",
        poll_write=lambda _: next(verdicts),
        close_write=closed.append,
    )
    worker.registration = object()
    worker._lock = threading.Lock()
    worker.native_operations = 1
    registry = OperationRegistry(None)
    job = {
        "request_id": "request",
        "state": "dispatching",
        "body": {"file": "weights", "fileOffset": 0, "length": 4, "target": {}},
        "owner": "window-owner",
        "worker": worker,
        "native": None,
        "verdict": "",
        "ack": False,
        "byte_size": 1,
    }
    registry.jobs["request"] = job
    registry.queues[worker] = deque(["request"])
    registry.windows["window-owner"] = "request"
    registry.job_bytes = 1
    for acknowledge, progress, state, pending in (
        (False, True, "submitted", True),
        (False, False, "submitted", True),
        (False, True, "terminal", False),
        (False, False, "terminal", False),
        (True, True, "acknowledged", False),
    ):
        job["ack"] = acknowledge
        assert registry.step(worker) is progress
        assert job["state"] == state
        assert registry.has_pending(worker) is pending
    assert not registry.step(worker)
    assert closed == ["native-operation"]
    assert worker.native_operations == 0
    assert registry.job_bytes == 0
    assert not registry.windows


@pytest.mark.parametrize(
    "state,released,job_state,session_state,expected",
    [
        ("serving", False, "acknowledged", "closed", False),
        ("quarantine", False, "unknown", "unknown", False),
        ("drained", False, "acknowledged", "closed", False),
        ("drained", True, "unknown", "closed", False),
        ("drained", True, "acknowledged", "open", False),
        ("drained", True, "acknowledged", "closed", True),
    ],
)
def test_retirement_keeps_unknown_ownership_and_evicts_finished_manifest(
    state, released, job_state, session_state, expected
):
    class Worker:
        pass

    worker = Worker()
    worker.state = state
    worker._native_attempted = True
    worker._release_done = released
    worker.control = SimpleNamespace(generation="generation")
    manifest = object()
    worker.assignment = SimpleNamespace(manifest=manifest)
    registry = OperationRegistry(None)
    key = ("pod", "incarnation", "generation", "lease", "session")
    other = ("pod", "incarnation", "other-generation", "lease", "session")
    registry.jobs["job"] = {"common": key, "state": job_state}
    registry.jobs["other"] = {"common": other, "state": "unknown"}
    registry.sessions[key] = {"state": session_state}
    registry.queues[worker] = deque()
    registry._manifest_tables[id(manifest)] = (manifest,)
    assert registry.retire(worker) is expected
    assert ("job" not in registry.jobs) is expected
    assert (key not in registry.sessions) is expected
    assert (id(manifest) not in registry._manifest_tables) is expected
    assert (worker not in registry.queues) is expected
    assert registry.jobs["other"]["state"] == "unknown"
