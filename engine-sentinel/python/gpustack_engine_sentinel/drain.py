"""PreStop graceful drain CLI matching model_deployment_drain.go 2-consecutive-idle semantics."""

from __future__ import annotations

import argparse
import socket
import ssl
import sys
import time
import urllib.request

INFLIGHT_METRICS_BY_ENGINE: dict[str, tuple[str, ...]] = {
    "vllm": (
        "vllm:num_requests_running",
        "vllm:num_requests_waiting",
    ),
    "sglang": (
        "sglang:num_running_reqs",
        "sglang:num_queue_reqs",
        "sglang:num_prefill_bootstrap_queue_reqs",
        "sglang:num_prefill_inflight_queue_reqs",
        "sglang:num_decode_prealloc_queue_reqs",
        "sglang:num_decode_transfer_queue_reqs",
    ),
}


def resolve_engine_metrics(engine: str) -> tuple[str, ...]:
    key = engine.strip().lower()
    metrics = INFLIGHT_METRICS_BY_ENGINE.get(key)
    if metrics is None:
        raise ValueError(f"Unsupported engine {engine!r}; expected vLLM or SGLang")
    return metrics


def parse_busy_count(body: str | None, metric_names: tuple[str, ...]) -> float:
    if body is None:
        return 1.0
    names = set(metric_names)
    seen: set[str] = set()
    busy = 0.0
    for line in body.splitlines():
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        name = stripped.split("{", 1)[0].split(" ", 1)[0]
        if name not in names:
            continue
        if "{" in stripped:
            if "}" not in stripped:
                return 1.0
            sample = stripped.rsplit("}", 1)[1]
        else:
            sample = stripped[len(name) :]
        try:
            busy += float(sample.split()[0])
            seen.add(name)
        except (IndexError, ValueError):
            return 1.0
    required_base = set(metric_names[:2])
    if not required_base.issubset(seen):
        return 1.0
    return busy


def run_drain(
    *,
    engine: str,
    port: int,
    scheme: str = "http",
    settle: float = 5.0,
    deadline: float = 25.0,
    host: str = "127.0.0.1",
    poll_interval: float = 1.0,
    request_timeout: float = 1.0,
) -> int:
    start = time.monotonic()
    if settle > 0:
        time.sleep(settle)

    try:
        metric_names = resolve_engine_metrics(engine)
    except ValueError:
        return 0

    norm_scheme = scheme.strip().lower() or "http"
    url = f"{norm_scheme}://{host}:{port}/metrics"

    context = ssl.create_default_context()
    context.check_hostname = False
    context.verify_mode = ssl.CERT_NONE

    idle = 0
    while time.monotonic() - start < deadline:
        try:
            with urllib.request.urlopen(
                url, timeout=request_timeout, context=context
            ) as resp:
                body: str | None = resp.read().decode("utf-8", errors="replace")
        except Exception as err:
            reason = getattr(err, "reason", err)
            if not isinstance(err, (TimeoutError, socket.timeout)) and not isinstance(
                reason, (TimeoutError, socket.timeout)
            ):
                return 0
            body = None

        busy = parse_busy_count(body, metric_names)
        idle = idle + 1 if busy == 0 else 0
        if idle == 2:
            return 0
        if poll_interval > 0:
            time.sleep(poll_interval)
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="python3 -m gpustack_engine_sentinel.drain",
        description="Wait for an inference engine replica to drain in-flight requests before SIGTERM.",
    )
    parser.add_argument(
        "--engine",
        required=True,
        help="Target inference engine name (vLLM or SGLang).",
    )
    parser.add_argument(
        "--port",
        required=True,
        type=int,
        help="Local metrics port on 127.0.0.1.",
    )
    parser.add_argument(
        "--scheme",
        default="http",
        help="Metrics URL scheme (http or https).",
    )
    parser.add_argument(
        "--settle",
        type=float,
        default=5.0,
        help="Seconds to keep serving normally before polling in-flight gauges.",
    )
    parser.add_argument(
        "--deadline",
        type=float,
        default=25.0,
        help="Total seconds from hook start before returning to allow SIGTERM.",
    )
    parser.add_argument(
        "--host",
        default="127.0.0.1",
        help="Loopback host to scrape (default 127.0.0.1).",
    )
    parser.add_argument(
        "--poll-interval",
        type=float,
        default=1.0,
        help="Seconds between /metrics polls (default 1.0).",
    )
    parser.add_argument(
        "--request-timeout",
        type=float,
        default=1.0,
        help="Per-poll HTTP read timeout in seconds (default 1.0).",
    )
    return parser


def main(argv: list[str] | None = None) -> int:
    try:
        args = build_parser().parse_args(argv)
        return run_drain(
            engine=args.engine,
            port=args.port,
            scheme=args.scheme,
            settle=args.settle,
            deadline=args.deadline,
            host=args.host,
            poll_interval=args.poll_interval,
            request_timeout=args.request_timeout,
        )
    except BaseException:
        # PreStop always exits 0 so Kubelet never records FailedPreStopHook events.
        return 0


if __name__ == "__main__":
    sys.exit(main())
