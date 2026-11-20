"""Tests for gpustack_engine_sentinel.drain PreStop CLI matching model_deployment_drain.go 2-consecutive-idle semantics."""

from __future__ import annotations

import os
import shutil
import socket
import ssl
import subprocess
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import pytest

REPO_PYTHON_ROOT = Path(__file__).resolve().parents[1]


class _MetricsServer:
    def __init__(
        self, handler_fn, *, ssl_context: ssl.SSLContext | None = None
    ) -> None:
        outer = self

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self) -> None:
                outer.calls += 1
                status, body, delay = handler_fn(outer.calls, self.path)
                if delay > 0:
                    time.sleep(delay)
                self.send_response(status)
                self.send_header("Content-Type", "text/plain; version=0.0.4")
                self.end_headers()
                self.wfile.write(body.encode("utf-8"))

            def log_message(self, format: str, *args) -> None:  # noqa: A002
                del format, args

        self.calls = 0
        self._httpd = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        if ssl_context is not None:
            self._httpd.socket = ssl_context.wrap_socket(
                self._httpd.socket, server_side=True
            )
        self.port = int(self._httpd.server_address[1])
        self._thread = threading.Thread(target=self._httpd.serve_forever, daemon=True)
        self._thread.start()

    def close(self) -> None:
        self._httpd.shutdown()
        self._httpd.server_close()
        self._thread.join(timeout=2)


def _run_drain_cli(
    *,
    engine: str,
    port: int,
    scheme: str = "http",
    settle: float = 0.2,
    deadline: float = 2.0,
    extra_args: list[str] | None = None,
    sentinel_enabled: bool = True,
) -> tuple[int, float, str]:
    env = dict(os.environ)
    env["PYTHONPATH"] = str(REPO_PYTHON_ROOT)
    if sentinel_enabled:
        env["GPUSTACK_ENGINE_SENTINEL_ENABLE"] = "1"
    else:
        env.pop("GPUSTACK_ENGINE_SENTINEL_ENABLE", None)

    cmd = [
        sys.executable,
        "-m",
        "gpustack_engine_sentinel.drain",
        "--engine",
        engine,
        "--port",
        str(port),
        "--scheme",
        scheme,
        "--settle",
        str(settle),
        "--deadline",
        str(deadline),
        *(extra_args or []),
    ]
    start = time.monotonic()
    res = subprocess.run(cmd, env=env, capture_output=True, text=True, check=False)
    elapsed = time.monotonic() - start
    return res.returncode, elapsed, res.stdout + res.stderr


def test_drain_vllm_settle_and_two_consecutive_idle_reads():
    # Sequence: 1=busy, 2=idle(1), 3=busy(resets idle to 0!), 4=idle(1), 5=idle(2 -> exit 0!)
    def responder(call_idx: int, _path: str) -> tuple[int, str, float]:
        running = "1.0" if call_idx in (1, 3) else "0.0"
        body = (
            "# HELP vllm:num_requests_running Number of requests.\n"
            "# TYPE vllm:num_requests_running gauge\n"
            f'vllm:num_requests_running{{engine="0",model_name="a}} b"}} {running}\n'
            "vllm:num_requests_waiting 0\n"
            'vllm:num_requests_waiting_by_reason{engine="0",reason="cap"} 9.0\n'
        )
        return 200, body, 0.0

    srv = _MetricsServer(responder)
    try:
        rc, elapsed, out = _run_drain_cli(
            engine="vLLM",
            port=srv.port,
            settle=0.25,
            deadline=5.0,
            extra_args=["--poll-interval", "0.1"],
        )
        assert rc == 0, out
        assert srv.calls == 5
        assert 0.55 <= elapsed < 4.0
    finally:
        srv.close()


def test_drain_sglang_counts_all_six_disagg_and_scheduler_gauges():
    # Call 1 has sglang:num_decode_transfer_queue_reqs=1 (busy); calls 2 & 3 have all 6 gauges 0 -> exit!
    def responder(call_idx: int, _path: str) -> tuple[int, str, float]:
        transfer = "1" if call_idx == 1 else "0"
        body = (
            "sglang:num_running_reqs 0\n"
            "sglang:num_queue_reqs 0\n"
            "sglang:num_prefill_bootstrap_queue_reqs 0\n"
            "sglang:num_prefill_inflight_queue_reqs 0\n"
            "sglang:num_decode_prealloc_queue_reqs 0\n"
            f'sglang:num_decode_transfer_queue_reqs{{dp="0"}} {transfer}\n'
        )
        return 200, body, 0.0

    srv = _MetricsServer(responder)
    try:
        rc, elapsed, out = _run_drain_cli(
            engine="SGLang",
            port=srv.port,
            settle=0.1,
            deadline=4.0,
            extra_args=["--poll-interval", "0.1"],
        )
        assert rc == 0, out
        assert srv.calls == 3
        assert elapsed < 3.0
    finally:
        srv.close()


def test_drain_connection_refused_and_http_error_exit_zero_immediately_after_settle():
    # Reserve and immediately close an ephemeral port so connection is refused.
    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    sock.bind(("127.0.0.1", 0))
    closed_port = int(sock.getsockname()[1])
    sock.close()

    rc, elapsed, out = _run_drain_cli(
        engine="vLLM",
        port=closed_port,
        settle=0.2,
        deadline=5.0,
    )
    assert rc == 0, out
    assert 0.18 <= elapsed < 1.5

    # HTTP 500 also exits 0 immediately after settle without waiting for deadline.
    srv = _MetricsServer(lambda _n, _p: (500, "internal error\n", 0.0))
    try:
        rc500, elapsed500, out500 = _run_drain_cli(
            engine="vLLM",
            port=srv.port,
            settle=0.15,
            deadline=5.0,
        )
        assert rc500 == 0, out500
        assert srv.calls == 1
        assert 0.14 <= elapsed500 < 1.5
    finally:
        srv.close()


def test_drain_timeout_and_unparseable_sample_count_as_busy_until_deadline():
    # Timeout case: handler sleeps longer than request-timeout -> treated as busy until deadline, exits 0!
    srv_slow = _MetricsServer(
        lambda _n, _p: (200, "vllm:num_requests_running 0\n", 0.35)
    )
    try:
        rc_slow, elapsed_slow, out_slow = _run_drain_cli(
            engine="vLLM",
            port=srv_slow.port,
            settle=0.1,
            deadline=0.75,
            extra_args=["--request-timeout", "0.15", "--poll-interval", "0.1"],
        )
        assert rc_slow == 0, out_slow
        assert elapsed_slow >= 0.72
    finally:
        srv_slow.close()

    # Unparseable sample case: missing float value counts as busy until deadline, exits 0!
    srv_bad = _MetricsServer(
        lambda _n, _p: (
            200,
            'vllm:num_requests_running{engine="0"}\nvllm:num_requests_waiting 0\n',
            0.0,
        )
    )
    try:
        rc_bad, elapsed_bad, out_bad = _run_drain_cli(
            engine="vLLM",
            port=srv_bad.port,
            settle=0.1,
            deadline=0.65,
            extra_args=["--poll-interval", "0.1"],
        )
        assert rc_bad == 0, out_bad
        assert elapsed_bad >= 0.62
        assert srv_bad.calls >= 3
    finally:
        srv_bad.close()


def test_drain_self_signed_https_polls_unverified_tls(tmp_path: Path):
    openssl = shutil.which("openssl")
    if openssl is None:
        pytest.skip("openssl not on PATH")

    cert_file = tmp_path / "cert.pem"
    key_file = tmp_path / "key.pem"
    subprocess.run(
        [
            openssl,
            "req",
            "-x509",
            "-newkey",
            "rsa:2048",
            "-keyout",
            str(key_file),
            "-out",
            str(cert_file),
            "-days",
            "1",
            "-nodes",
            "-subj",
            "/CN=127.0.0.1",
        ],
        capture_output=True,
        check=True,
    )
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.load_cert_chain(certfile=str(cert_file), keyfile=str(key_file))

    def responder(call_idx: int, _path: str) -> tuple[int, str, float]:
        running = "1" if call_idx == 1 else "0"
        return (
            200,
            f"vllm:num_requests_running {running}\nvllm:num_requests_waiting 0\n",
            0.0,
        )

    srv = _MetricsServer(responder, ssl_context=ctx)
    try:
        rc, elapsed, out = _run_drain_cli(
            engine="vLLM",
            port=srv.port,
            scheme="https",
            settle=0.1,
            deadline=4.0,
            extra_args=["--poll-interval", "0.1"],
        )
        assert rc == 0, out
        assert srv.calls == 3
        assert elapsed < 3.0
    finally:
        srv.close()


def test_parse_busy_count_treats_missing_or_malformed_gauges_as_busy():
    from gpustack_engine_sentinel.drain import parse_busy_count

    metrics = ("vllm:num_requests_running", "vllm:num_requests_waiting")
    assert (
        parse_busy_count("# HELP unrelated gauge\nunrelated_metric 0\n", metrics) == 1.0
    )
    assert (
        parse_busy_count(
            'vllm:num_requests_running{model="broken 0\n',
            metrics,
        )
        == 1.0
    )
    assert (
        parse_busy_count(
            "vllm:num_requests_running 0\nvllm:num_requests_waiting 0\n",
            metrics,
        )
        == 0.0
    )
