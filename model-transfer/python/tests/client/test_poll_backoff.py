"""Deterministic status pressure, progress latency and deadline contracts."""

import ssl
from types import SimpleNamespace

import pytest

from gpustack_model_transfer import client, client_transport
from gpustack_model_transfer.client import EngineSession
from gpustack_model_transfer.client_transport import WireUnknown


class Clock:
    def __init__(self):
        self.now = 0.0
        self.sleeps = []

    def monotonic(self):
        return self.now

    def sleep(self, delay):
        self.sleeps.append(delay)
        self.now += delay


class Transport:
    def __init__(self, clock, transitions, latency=0):
        self.clock = clock
        self.transitions = transitions
        self.latency = latency
        self.calls = []

    def request(self, method, path, payload=None, *, deadline=None):
        self.calls.append((self.clock.now, method, path, deadline))
        remaining = 5.0 if deadline is None else deadline - self.clock.now
        self.clock.now += min(self.latency, remaining)
        if self.latency >= remaining:
            raise WireUnknown("observation timed out")
        state = "submitted"
        for at, value in self.transitions:
            if self.clock.now >= at:
                state = value
        if isinstance(state, BaseException):
            raise state
        return {"requestID": "request", "sessionID": "session", "state": state}


@pytest.fixture
def clock(monkeypatch):
    clock = Clock()
    monkeypatch.setattr(client.time, "monotonic", clock.monotonic)
    monkeypatch.setattr(client.time, "sleep", clock.sleep)
    return clock


def session(clock, transitions=(), *, timeout=65, latency=0):
    transport = Transport(clock, transitions, latency)
    result = object.__new__(EngineSession)
    result.session_id = "session"
    result.reader = SimpleNamespace(
        timeout=timeout,
        transport=transport,
        target_instance_id="instance",
        pod_uid="pod",
        pod_name="engine",
        pod_namespace="tenant",
    )
    return result, transport


@pytest.mark.parametrize("state", ["dispatching", "submitted"])
def test_long_wait_reduces_status_pressure_without_reposting(clock, state):
    waiting, transport = session(clock, [(0, state), (60, "terminal")])
    result = waiting._wait("request", {"terminal"})
    assert result["state"] == "terminal"
    assert 60 <= clock.now <= 60.1 + 1e-9
    assert len(transport.calls) <= 620
    assert {method for _, method, _, _ in transport.calls} == {"GET"}
    assert len({path for _, _, path, _ in transport.calls}) == 1


def test_status_transport_cannot_overrun_absolute_wait_budget(clock):
    waiting, transport = session(clock, timeout=1, latency=5)
    with pytest.raises(WireUnknown, match="deadline exceeded"):
        waiting._wait("request", {"terminal"})
    assert clock.now <= 1 + 1e-9
    assert len(transport.calls) == 1
    assert transport.calls[0][3] == 1


def test_genuine_progress_resets_delay_without_resetting_deadline(clock):
    waiting, transport = session(
        clock,
        [(0, "prepared"), (0.4, "dispatching"), (0.8, "submitted"), (1.2, "terminal")],
        timeout=2,
    )
    waiting._wait("request", {"terminal"})
    assert max(clock.sleeps) <= 0.1
    assert len([delay for delay in clock.sleeps if delay == 0.005]) >= 3
    assert {deadline for _, _, _, deadline in transport.calls} == {2}
    assert clock.now <= 1.3 + 1e-9


@pytest.mark.parametrize("initial", ["unknown", "foreign", "terminal"])
def test_terminal_unknown_and_identity_mismatch_are_immediate(clock, initial):
    waiting, transport = session(clock)
    answer = {"requestID": "request", "sessionID": "session", "state": initial}
    if initial == "foreign":
        answer.update(requestID="foreign", state="submitted")
    if initial == "terminal":
        assert waiting._wait("request", {"terminal"}, answer) is answer
    else:
        with pytest.raises(WireUnknown):
            waiting._wait("request", {"terminal"}, answer)
    assert not clock.sleeps
    assert not transport.calls


def test_interrupted_observation_is_not_reposted_or_acknowledged(clock):
    waiting, transport = session(clock, [(0, KeyboardInterrupt())])
    with pytest.raises(KeyboardInterrupt):
        waiting._wait("request", {"terminal"})
    assert [method for _, method, _, _ in transport.calls] == ["GET"]


def test_submit_and_status_share_one_absolute_budget(clock):
    waiting, transport = session(clock, timeout=1, latency=0.6)
    with pytest.raises(WireUnknown, match="deadline exceeded"):
        waiting._transfer_span({"requestID": "request"})
    assert clock.now <= 1 + 1e-9
    assert [method for _, method, _, _ in transport.calls] == ["POST", "GET"]
    assert {deadline for _, _, _, deadline in transport.calls} == {1}


def test_acknowledgement_transport_keeps_its_absolute_cleanup_budget(clock):
    waiting, transport = session(clock, timeout=1, latency=5)
    with pytest.raises(WireUnknown, match="acknowledgement deadline exceeded"):
        waiting._ack(
            {"requestID": "request", "registrationID": "reg", "windowID": "window"}
        )
    assert clock.now == 1
    assert {deadline for _, _, _, deadline in transport.calls} == {1}


def test_stale_states_and_observation_loss_do_not_reset_backoff(clock):
    waiting, transport = session(
        clock,
        [
            (0, "submitted"),
            (0.02, "dispatching"),
            (0.05, "submitted"),
            (0.4, WireUnknown("observation lost")),
            (0.8, "submitted"),
        ],
        timeout=2,
    )
    with pytest.raises(WireUnknown, match="deadline exceeded"):
        waiting._wait("request", {"terminal"})
    assert clock.now <= 2 + 1e-9
    assert len(transport.calls) <= 30
    assert len([delay for delay in clock.sleeps if delay == 0.005]) == 2


def test_connect_and_tls_handshake_share_the_remaining_budget(clock, monkeypatch):
    class Socket:
        timeout = None
        closed = False

        def settimeout(self, timeout):
            self.timeout = timeout

        def close(self):
            self.closed = True

    raw = Socket()
    connect_timeouts = []

    def connect(_address, timeout):
        connect_timeouts.append(timeout)
        clock.now += 0.6
        return raw

    def handshake(sock, server_hostname):
        assert server_hostname == "broker.test"
        assert sock.timeout == pytest.approx(0.4)
        clock.now += sock.timeout
        raise TimeoutError("handshake deadline")

    connection = client_transport._PinnedHTTPS(
        "127.0.0.1", 9251, "broker.test", ssl.create_default_context(), 5
    )
    connection._context = SimpleNamespace(wrap_socket=handshake)
    connection.set_deadline(1)
    monkeypatch.setattr(client_transport.socket, "create_connection", connect)
    with pytest.raises(client_transport.WirePreAdmission):
        connection.connect()
    assert connect_timeouts == [1]
    assert clock.now == 1
    assert raw.closed


def test_partial_sends_do_not_restart_the_request_budget(clock):
    class Socket:
        timeout = None

        def settimeout(self, timeout):
            self.timeout = timeout

        def send(self, _data):
            clock.now += min(0.1, self.timeout)
            if self.timeout <= 0.1:
                raise TimeoutError("send deadline")
            return 1

    sock = client_transport._DeadlineSocket(Socket(), 5, 0.3)
    with pytest.raises(TimeoutError):
        sock.sendall(b"many partial writes")
    assert clock.now == pytest.approx(0.3)


def test_request_without_deadline_preserves_socket_sendall_timeout():
    calls = []

    class Socket:
        def sendall(self, data):
            calls.append(data)

    sock = client_transport._DeadlineSocket(Socket(), 5, None)
    sock.sendall(b"existing transport budget")
    assert calls == [b"existing transport budget"]
