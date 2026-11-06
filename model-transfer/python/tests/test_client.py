"""Deterministic host-window pipeline and device ownership tests, without hardware."""

import sys
import threading
import time
from collections import defaultdict
from concurrent.futures import ThreadPoolExecutor
from types import SimpleNamespace

import pytest

from gpustack_model_transfer import client
from gpustack_model_transfer.client import EngineSession
from gpustack_model_transfer.client_native import TargetNative
from gpustack_model_transfer.client_transport import (
    WirePreAdmission,
    WireRefused,
    WireUnknown,
)
from gpustack_model_transfer.core.budgets import ReadBudgets
from gpustack_model_transfer.core.errors import SourceUnavailableError


class FakeNative:
    supports_window_offset = True
    requires_synchronization = True

    def __init__(self, harness, size):
        self.harness = harness
        self.size = size
        self.buffer = bytearray(size)
        self.registration_id = f"registration-{len(harness.windows)}"
        self.window_id = f"window-{len(harness.windows)}"
        self.pending = []
        self.closed = False
        harness.windows.append(self)

    def record(self, kind, detail=None):
        assert threading.get_ident() == self.harness.owner_thread
        self.harness.events.append((kind, self.window_id, detail))

    def start(self):
        self.record("start")
        if len(self.harness.windows) == self.harness.fail_start:
            raise RuntimeError("registration failed")

    def descriptor(self):
        self.record("descriptor")
        return {"length": self.size, "address": id(self.buffer)}

    def copy_to(self, target, offset, length, window_offset=0):
        self.record("copy", target.name)
        assert not self.closed
        if self.harness.on_copy is not None:
            self.harness.on_copy(self, target)
        time.sleep(self.harness.copy_delay)
        data = bytes(self.buffer[window_offset : window_offset + length])
        target.data[offset : offset + length] = data
        # Model a device that may keep reading these host bytes after copy_to.
        self.pending.append((target.device.index, window_offset, data))

    def synchronize(self, target):
        self.record("synchronize", target.device.index)
        remaining = []
        for device, offset, data in self.pending:
            assert self.buffer[offset : offset + len(data)] == data
            if device != target.device.index:
                remaining.append((device, offset, data))
        self.pending = remaining

    def close(self):
        self.record("close")
        assert not self.pending
        assert not self.harness.active_posts
        self.closed = True


class FakeFactory:
    def __init__(self, harness, supported):
        self.harness = harness
        self.supports_window_offset = supported

    def __call__(self, backend, pod_ip, size):
        native = FakeNative(self.harness, size)
        native.supports_window_offset = self.supports_window_offset
        return native


class PipelineHarness:
    def __init__(
        self,
        *,
        sizes=(8, 8, 8, 8),
        window=16,
        inflight=2,
        supported=True,
        gap=0,
        files=None,
        backend="nixl",
    ):
        self.owner_thread = threading.get_ident()
        self.events = []
        self.windows = []
        self.posts = []
        self.acks = []
        self.close_phases = []
        self.results = {}
        self.outcomes = {}
        self.gates = {}
        self.started = defaultdict(threading.Event)
        self.completed = defaultdict(threading.Event)
        self.active_posts = 0
        self.on_copy = None
        self.fail_start = None
        self.post_delay = 0
        self.copy_delay = 0
        self.request_threads = []
        self.tensors = []
        self.payloads = {}
        for i, size in enumerate(sizes):
            name = f"tensor-{i}"
            file = files[i] if files else "weights.safetensors"
            begin = len(self.payloads.get(file, bytes(16))) + gap
            end = begin + size
            self.payloads[file] = bytes(j % 251 for j in range(end))
            self.tensors.append(
                SimpleNamespace(
                    name=name,
                    file=file,
                    data_begin=begin,
                    data_end=end,
                    byte_size=size,
                    dtype="U8",
                    shape=(size,),
                )
            )
        self.by_name = {tensor.name: tensor for tensor in self.tensors}
        self.reader = SimpleNamespace(
            native_factory=FakeFactory(self, supported),
            backend=backend,
            pod_ip="127.0.0.1",
            pod_namespace="tenant",
            pod_name="target",
            pod_uid="pod",
            target_instance_id="instance",
            budgets=ReadBudgets(window, inflight, 1024),
            workers=inflight,
            timeout=0.1,
            transport=self,
            target_validator=lambda d, t: True,
            _close_phase=self.close_phase,
        )
        self.session = EngineSession(
            self.reader,
            "session",
            SimpleNamespace(tensors=self.tensors),
            ("generation", "source-pod", "source-instance"),
        )

    def close_phase(self, session_id, request_id, *, finalize):
        assert not self.active_posts
        self.close_phases.append(finalize)

    def target(self, tensor, device=0):
        return SimpleNamespace(
            name=tensor.name,
            data=bytearray(tensor.byte_size),
            device=SimpleNamespace(
                type="npu" if self.reader.backend == "hixl" else "cuda", index=device
            ),
        )

    def fill(self, index, *, device=0):
        tensor = self.tensors[index]
        target = self.target(tensor, device)
        self.session.fill(tensor, target)
        assert (
            target.data
            == self.payloads[tensor.file][tensor.data_begin : tensor.data_end]
        )
        return target

    def request(self, method, path, body=None, *, deadline=None):
        self.request_threads.append(threading.get_ident())
        if path == "/v1/transfers":
            self.posts.append(body)
            number = len(self.posts)
            self.active_posts += 1
            self.started[number].set()
            try:
                if number in self.gates:
                    assert self.gates[number].wait(3), "test did not release transfer"
                time.sleep(self.post_delay)
                return self.transfer(number, body)
            finally:
                self.active_posts -= 1
                self.completed[number].set()
        request_id = path.split("/")[-2 if path.endswith("/ack") else -1].split("?")[0]
        operation = next(item for item in self.posts if item["requestID"] == request_id)
        number = self.posts.index(operation) + 1
        if self.outcomes.get(number) == "plain-refused":
            raise WireRefused("status unavailable")
        if path.endswith("/ack"):
            assert body["registrationID"] == operation["registrationID"]
            assert body["windowID"] == operation["windowID"]
            self.acks.append(request_id)
            if self.outcomes.get(number) == "ack-unknown":
                self.results[request_id]["state"] = "unknown"
                raise WireUnknown("ack outcome unknown")
            self.results[request_id]["state"] = "acknowledged"
        return self.results[request_id]

    def transfer(self, number, operation):
        outcome = self.outcomes.get(number, "succeeded")
        if outcome == "pre-admission":
            raise WirePreAdmission("no bytes sent")
        if outcome == "plain-refused":
            raise WireRefused("plain conflict after uncertain admission")
        native = next(w for w in self.windows if w.window_id == operation["windowID"])
        assert not native.closed and not native.pending, "overwrote a live H2D source"
        length = operation["length"]
        assert 0 < length <= native.size
        tensor = self.by_name[operation["tensor"]]
        begin = tensor.data_begin + operation["tensorOffset"]
        end = begin + length
        # Mirror the broker's rule: a span crossing tensors must start at
        # offset zero and end on a contiguous tensor boundary in the same file.
        if end > tensor.data_end:
            assert operation["tensorOffset"] == 0
            cursor = tensor.data_end
            for item in self.tensors[self.tensors.index(tensor) + 1 :]:
                assert item.file == tensor.file and item.data_begin == cursor
                cursor = item.data_end
                if cursor >= end:
                    break
            assert cursor == end
        if outcome not in ("not-posted", "failed", "refused"):
            native.buffer[:length] = self.payloads[tensor.file][begin:end]
        self.events.append(("post", native.window_id, number))
        result = {
            "requestID": operation["requestID"],
            "sessionID": operation["sessionID"],
            "state": "unknown" if outcome == "unknown" else "terminal",
            "verdict": "not-posted" if outcome == "refused" else outcome,
        }
        if outcome in ("lost-response", "ack-unknown", "unclassified"):
            result["verdict"] = "succeeded"
        self.results[operation["requestID"]] = result
        if outcome == "refused":
            raise WireRefused("refused before posting")
        if outcome == "lost-response":
            raise WireUnknown("post response lost")
        if outcome == "unclassified":
            raise RuntimeError("unexpected post failure")
        return result


@pytest.fixture
def make_pipeline():
    harnesses = []

    def make(**kwargs):
        harness = PipelineHarness(**kwargs)
        harnesses.append(harness)
        return harness

    yield make
    for harness in harnesses:
        for gate in harness.gates.values():
            gate.set()
        harness.session.abort()
        # These retained owners contain only bytearrays, never native memory.
        client._release_retained(harness.session)


@pytest.mark.parametrize(
    "options",
    [
        {},
        {"sizes": (3, 5, 11, 13)},
        {"sizes": (31,)},
        {"sizes": (0, 8, 0, 8)},
        {"sizes": (0, 0)},
        {"window": 9},
        {"window": 1},
        {"inflight": 1},
        {"supported": False},
        {"gap": 3},
        {"files": ("a", "a", "b", "b")},
        {"backend": "hixl"},
    ],
)
def test_window_pipeline_preserves_bytes_and_total_budget(make_pipeline, options):
    harness = make_pipeline(**options)
    for index in range(len(harness.tensors)):
        harness.fill(index)
    harness.session.close()
    assert sum(w.size for w in harness.windows) <= harness.session.window_bytes
    assert all(w.closed for w in harness.windows)
    assert len({post["requestID"] for post in harness.posts}) == len(harness.posts)
    assert len(harness.acks) == len(harness.posts)
    expected_windows = (
        0
        if harness.session.window_bytes == 0
        else (
            2
            if harness.reader.native_factory.supports_window_offset
            and harness.reader.budgets.max_inflight >= 2
            and harness.session.window_bytes >= 2
            else 1
        )
    )
    assert len(harness.windows) == expected_windows


@pytest.mark.parametrize("backend", ["nixl", "hixl"])
def test_prefetch_overlaps_copy_and_uses_only_cpu_on_worker(make_pipeline, backend):
    harness = make_pipeline(backend=backend)
    overlap = []

    def copy_waits_for_prefetch(native, target):
        if target.name == "tensor-0":
            assert harness.started[2].wait(1)
            assert harness.active_posts == 1
            overlap.append(True)
            harness.gates[2].set()

    harness.gates[2] = threading.Event()
    harness.on_copy = copy_waits_for_prefetch
    pairs = [(tensor, harness.target(tensor)) for tensor in harness.tensors]
    harness.session.fill_many(pairs)
    assert overlap == [True]
    assert all(thread != harness.owner_thread for thread in harness.request_threads)
    for tensor, target in pairs:
        assert (
            target.data
            == harness.payloads[tensor.file][tensor.data_begin : tensor.data_end]
        )


@pytest.mark.parametrize("backend", ["nixl", "hixl"])
def test_window_reuse_fences_every_device_after_cached_copies(make_pipeline, backend):
    harness = make_pipeline(sizes=(4,) * 8, backend=backend)
    harness.fill(0, device=0)
    harness.fill(1, device=1)
    assert not [event for event in harness.events if event[0] == "synchronize"]
    for index in range(2, 8):
        harness.fill(index)
    harness.session.close()
    first = harness.windows[0].window_id
    events = [event for event in harness.events if event[1] == first]
    second_copy = events.index(("copy", first, "tensor-1"))
    first_reuse = events.index(("post", first, 3))
    for device in (0, 1):
        assert second_copy < events.index(("synchronize", first, device)) < first_reuse
    assert [post["windowID"] for post in harness.posts] == [
        harness.windows[i % 2].window_id for i in range(4)
    ]


@pytest.mark.parametrize("backend", ["nixl", "hixl"])
def test_pending_source_close_keeps_target_windows_until_retry(make_pipeline, backend):
    harness = make_pipeline(backend=backend)
    for index in range(len(harness.tensors)):
        harness.fill(index)
    close_phase = harness.reader._close_phase
    attempts = []

    def pending_once(session_id, request_id, *, finalize):
        attempts.append((request_id, finalize))
        if len(attempts) == 1:
            raise WireUnknown("source close remains pending")
        close_phase(session_id, request_id, finalize=finalize)

    harness.reader._close_phase = pending_once
    with pytest.raises(SourceUnavailableError, match="session closing outcome unknown"):
        harness.session.close()
    assert not harness.session.closed
    assert all(not window.closed for window in harness.windows)
    assert not any(event[0] == "close" for event in harness.events)
    harness.session.close()
    assert harness.session.closed
    assert all(window.closed for window in harness.windows)
    assert sum(event[0] == "close" for event in harness.events) == len(harness.windows)
    assert len({request_id for request_id, _ in attempts}) == 1
    assert [finalize for _, finalize in attempts] == [False, False, True]


@pytest.mark.parametrize(
    "outcome", ["not-posted", "failed", "pre-admission", "refused"]
)
def test_clean_speculative_failure_is_deferred_until_demand(make_pipeline, outcome):
    harness = make_pipeline()
    harness.outcomes[2] = outcome
    harness.fill(0)
    with pytest.raises(SourceUnavailableError, match="verification failed"):
        harness.fill(1)
    assert not harness.session.unknown and harness.session.inflight is None
    assert len(harness.posts) == 2
    harness.session.abort()
    assert all(window.closed for window in harness.windows)


def test_undemanded_clean_failure_does_not_fail_close(make_pipeline):
    harness = make_pipeline()
    harness.outcomes[2] = "failed"
    harness.fill(0)
    harness.session.close()
    assert harness.session.closed and not harness.session.unknown


@pytest.mark.parametrize("number", [1, 2])
@pytest.mark.parametrize(
    "outcome", ["unknown", "unclassified", "ack-unknown", "plain-refused"]
)
def test_unknown_foreground_or_prefetch_retains_both_windows(
    make_pipeline, number, outcome
):
    harness = make_pipeline()
    harness.outcomes[number] = outcome
    if number == 2:
        harness.gates[2] = threading.Event()
        harness.fill(0)
        harness.gates[2].set()
    with pytest.raises(SourceUnavailableError):
        harness.fill(number - 1)
    assert harness.session.unknown and harness.session.inflight is not None
    assert any(item is harness.session for item in client._RETAINED)
    assert not harness.active_posts and harness.session._prefetch_executor is None
    with pytest.raises(SourceUnavailableError, match="retains unknown"):
        harness.session.close()
    harness.session.abort()
    assert not harness.close_phases
    assert len(harness.windows) == 2 and not any(w.closed for w in harness.windows)


@pytest.mark.parametrize("action", ["close", "abort"])
@pytest.mark.parametrize(
    "outcome", ["succeeded", "not-posted", "unknown", "unclassified"]
)
def test_teardown_joins_active_prefetch_before_release(make_pipeline, action, outcome):
    harness = make_pipeline()
    harness.outcomes[2] = outcome
    gate = harness.gates[2] = threading.Event()
    harness.on_copy = lambda native, target: harness.started[2].wait(1)
    harness.fill(0)
    checked = []

    def release_after_closing_starts():
        assert harness.session._closing.wait(1)
        checked.append(
            not any(w.closed for w in harness.windows)
            and not harness.close_phases
            and harness.active_posts == 1
        )
        gate.set()

    helper = threading.Thread(target=release_after_closing_starts)
    helper.start()
    try:
        if action == "close" and outcome in ("unknown", "unclassified"):
            with pytest.raises(SourceUnavailableError, match="retains unknown"):
                harness.session.close()
        else:
            getattr(harness.session, action)()
    finally:
        gate.set()
        helper.join(timeout=2)
    assert checked == [True] and not helper.is_alive()
    assert harness.session._prefetch_executor is None and not harness.active_posts
    assert all(w.closed for w in harness.windows) == (
        outcome in ("succeeded", "not-posted")
    )


def test_foreground_copy_failure_joins_prefetch_and_retains_every_owner(make_pipeline):
    harness = make_pipeline()
    gate = harness.gates[2] = threading.Event()
    failed = threading.Event()

    def fail_copy(native, target):
        assert harness.started[2].wait(1)
        failed.set()
        raise RuntimeError("copy failed after enqueue")

    def release_after_copy_failure():
        assert failed.wait(1)
        assert harness.active_posts == 1
        gate.set()

    harness.on_copy = fail_copy
    helper = threading.Thread(target=release_after_copy_failure)
    helper.start()
    try:
        with pytest.raises(SourceUnavailableError):
            harness.fill(0)
    finally:
        gate.set()
        helper.join(timeout=2)
    assert harness.session.unknown and not harness.active_posts
    assert harness.session._prefetch_executor is None
    assert not any(w.closed for w in harness.windows)


def test_validation_failure_joins_prefetch_without_poisoning_session(make_pipeline):
    harness = make_pipeline()
    gate = harness.gates[2] = threading.Event()
    harness.fill(0)
    validated = threading.Event()

    def reject(descriptor, target):
        validated.set()
        return False

    def release_after_validation():
        assert validated.wait(1)
        gate.set()

    harness.reader.target_validator = reject
    helper = threading.Thread(target=release_after_validation)
    helper.start()
    try:
        with pytest.raises(SourceUnavailableError, match="descriptor invalid"):
            harness.fill(1)
    finally:
        gate.set()
        helper.join(timeout=2)
    assert not harness.session.unknown and not harness.active_posts
    assert harness.session._prefetch_executor is None
    harness.session.close()


def test_fill_many_iterator_failure_joins_active_prefetch(make_pipeline):
    harness = make_pipeline()
    gate = harness.gates[2] = threading.Event()
    failed = threading.Event()

    def items():
        tensor = harness.tensors[0]
        yield tensor, harness.target(tensor)
        assert harness.started[2].wait(1)
        failed.set()
        raise RuntimeError("input iterator failed")

    def release_after_iterator_failure():
        assert failed.wait(1)
        gate.set()

    helper = threading.Thread(target=release_after_iterator_failure)
    helper.start()
    try:
        with pytest.raises(RuntimeError, match="input iterator failed"):
            harness.session.fill_many(items())
    finally:
        gate.set()
        helper.join(timeout=2)
    assert not harness.active_posts and harness.session._prefetch_executor is None
    assert not harness.session.unknown
    harness.session.close()


@pytest.mark.parametrize("order", [(0, 0, 3, 1, 2), (3, 2, 1, 0)])
def test_out_of_order_reads_join_or_reuse_prefetched_spans(make_pipeline, order):
    harness = make_pipeline()
    for index in order:
        harness.fill(index)
    harness.session.close()
    assert not harness.session.unknown


def test_journal_limit_refuses_demand_without_speculative_extra_operation(
    make_pipeline,
):
    harness = make_pipeline()
    harness.session.operation_count = (1 << 20) - 1
    harness.fill(0)
    with pytest.raises(SourceUnavailableError, match="verification failed"):
        harness.fill(1)
    assert len(harness.posts) == 1
    assert not harness.session.unknown
    harness.session.close()


def test_second_window_initialization_failure_retains_both_allocations(make_pipeline):
    harness = make_pipeline()
    harness.fail_start = 2
    with pytest.raises(SourceUnavailableError):
        harness.fill(0)
    assert harness.session.unknown
    assert len(harness.session._native_owners) == 2
    assert not harness.posts and not any(w.closed for w in harness.windows)


def test_submit_failure_after_enqueue_keeps_operation_tracked(make_pipeline):
    harness = make_pipeline()

    class UncertainExecutor:
        def __init__(self):
            self.executor = ThreadPoolExecutor(max_workers=1)

        def submit(self, function, operation):
            self.executor.submit(function, operation)
            raise RuntimeError("submit failed after enqueue")

        def shutdown(self, *, wait):
            self.executor.shutdown(wait=wait)

    harness.session._prefetch_executor = UncertainExecutor()
    with pytest.raises(SourceUnavailableError):
        harness.fill(0)
    assert harness.session.unknown and harness.session.inflight is not None
    assert len(harness.posts) == 1 and not harness.active_posts
    assert harness.session.inflight["requestID"] == harness.posts[0]["requestID"]
    assert not any(w.closed for w in harness.windows)


def test_interrupted_close_still_joins_active_prefetch(make_pipeline):
    harness = make_pipeline()
    gate = harness.gates[2] = threading.Event()
    harness.fill(0)
    assert harness.started[2].wait(1)
    interrupted = threading.Event()

    class InterruptedExecutor:
        def __init__(self, executor):
            self.executor = executor

        def shutdown(self, *, wait):
            if not interrupted.is_set():
                interrupted.set()
                raise KeyboardInterrupt
            self.executor.shutdown(wait=wait)

    harness.session._prefetch_executor = InterruptedExecutor(
        harness.session._prefetch_executor
    )

    def release_after_interrupt():
        assert interrupted.wait(1)
        assert not any(w.closed for w in harness.windows)
        gate.set()

    helper = threading.Thread(target=release_after_interrupt)
    helper.start()
    try:
        with pytest.raises(KeyboardInterrupt):
            harness.session.close()
    finally:
        gate.set()
        helper.join(timeout=2)
    assert harness.session.unknown and not harness.active_posts
    assert harness.session._prefetch_executor is None
    assert not any(w.closed for w in harness.windows)


def test_lost_prefetch_response_is_polled_without_repost(make_pipeline):
    harness = make_pipeline()
    harness.outcomes[2] = "lost-response"
    for index in range(4):
        harness.fill(index)
    harness.session.close()
    assert len(harness.posts) == 4
    assert len({p["requestID"] for p in harness.posts}) == 4


def test_pipeline_reduces_wall_time_by_overlapping_transfer_and_copy(make_pipeline):
    elapsed = []
    for inflight, window in ((1, 8), (2, 16)):
        harness = make_pipeline(sizes=(8,) * 8, window=window, inflight=inflight)
        harness.post_delay = harness.copy_delay = 0.03
        started = time.monotonic()
        for index in range(8):
            harness.fill(index)
        harness.session.close()
        elapsed.append(time.monotonic() - started)
        assert len(harness.posts) == 8
    assert elapsed[1] < elapsed[0] * 0.85, elapsed


class TorchTensor:
    """Small tensor view that records async-copy arguments without importing Torch."""

    def __init__(self, data, events, device="cpu", index=None):
        self.data = memoryview(data)
        self.events = events
        self.device = SimpleNamespace(type=device, index=index)

    def is_contiguous(self):
        return True

    def view(self, dtype):
        assert dtype == "uint8"
        return self

    def reshape(self, size):
        assert size == -1
        return self

    def numel(self):
        return len(self.data)

    def __getitem__(self, key):
        return TorchTensor(
            self.data[key], self.events, self.device.type, self.device.index
        )

    def copy_(self, source, *, non_blocking):
        self.events.append(("copy", non_blocking, source))
        self.data[:] = source.data


def native_copy_fixture(monkeypatch, *, host_tensor=True):
    events = []
    payload = bytearray(range(16))

    def frombuffer(data, *, dtype):
        events.append(("frombuffer",))
        assert dtype == "uint8"
        return TorchTensor(data, events)

    def synchronize(device):
        events.append(("synchronize", device.type, device.index))

    torch = SimpleNamespace(
        uint8="uint8",
        frombuffer=frombuffer,
        cuda=SimpleNamespace(synchronize=synchronize),
        npu=SimpleNamespace(synchronize=synchronize),
    )
    monkeypatch.setitem(sys.modules, "torch", torch)
    native = TargetNative("nixl", "127.0.0.1", len(payload))
    native.thread_id = threading.get_ident()
    native.block = SimpleNamespace(
        view=lambda offset, length: memoryview(payload)[offset : offset + length],
        tensor=TorchTensor(payload, events) if host_tensor else None,
    )
    native.owner = SimpleNamespace(release=lambda block: events.append(("release",)))
    return native, events, payload, torch


@pytest.mark.parametrize("device", ["cuda", "hip"])
@pytest.mark.parametrize("host_tensor", [True, False])
def test_native_cuda_copies_defer_sync_and_preserve_zero_copy_source(
    monkeypatch, device, host_tensor
):
    native, events, payload, _ = native_copy_fixture(
        monkeypatch, host_tensor=host_tensor
    )
    target = TorchTensor(bytearray(8), events, device, 0)
    native.copy_to(target, 0, 4, 2)
    native.copy_to(target, 4, 4, 6)
    assert target.data == payload[2:10]
    assert not any(event[0] == "synchronize" for event in events)
    copies = [event for event in events if event[0] == "copy"]
    assert len(copies) == 2 and all(event[1] for event in copies)
    assert all(event[2].data.obj is payload for event in copies)
    assert len([event for event in events if event[0] == "frombuffer"]) == (
        0 if host_tensor else 2
    )
    native.synchronize(target)
    native.close()
    assert [event[0] for event in events][-2:] == ["synchronize", "release"]


@pytest.mark.parametrize("device", ["npu", "privateuseone", "cpu"])
def test_borrowed_acl_and_cpu_copy_keep_existing_completion_contract(
    monkeypatch, device
):
    native, events, payload, _ = native_copy_fixture(monkeypatch, host_tensor=False)
    target = TorchTensor(bytearray(8), events, device, 0)
    native.copy_to(target, 0, 4, 2)
    native.copy_to(target, 4, 4, 6)
    assert target.data == payload[2:10]
    assert len([event for event in events if event[0] == "synchronize"]) == (
        0 if device == "cpu" else 2
    )
    assert not native._pending_devices
    native.close()


def test_native_close_fences_all_destination_devices_before_free(monkeypatch):
    native, events, _, _ = native_copy_fixture(monkeypatch)
    for device in (0, 1):
        target = TorchTensor(bytearray(4), events, "cuda", device)
        native.copy_to(target, 0, 4)
    native.close()
    assert events[-3:] == [
        ("synchronize", "cuda", 0),
        ("synchronize", "cuda", 1),
        ("release",),
    ]
    native.close()
    assert events[-3:] == [
        ("synchronize", "cuda", 0),
        ("synchronize", "cuda", 1),
        ("release",),
    ]


def test_native_sync_failure_keeps_host_memory_live(monkeypatch):
    native, events, _, torch = native_copy_fixture(monkeypatch)
    target = TorchTensor(bytearray(4), events, "cuda", 0)
    native.copy_to(target, 0, 4)

    def fail(device):
        raise RuntimeError("device completion unknown")

    torch.cuda.synchronize = fail
    with pytest.raises(RuntimeError):
        native.close()
    assert not native.closed and native._pending_devices
    assert not any(event[0] == "release" for event in events)


def test_native_rejects_device_work_on_prefetch_thread(monkeypatch):
    native, events, _, _ = native_copy_fixture(monkeypatch)
    target = TorchTensor(bytearray(4), events, "cuda", 0)
    errors = []

    def wrong_thread():
        for operation in (
            lambda: native.copy_to(target, 0, 4),
            lambda: native.synchronize(target),
            native.descriptor,
            native.close,
        ):
            with pytest.raises(SourceUnavailableError) as error:
                operation()
            errors.append(str(error.value))

    helper = threading.Thread(target=wrong_thread)
    helper.start()
    helper.join(timeout=2)
    assert len(errors) == 4 and not events and not helper.is_alive()
