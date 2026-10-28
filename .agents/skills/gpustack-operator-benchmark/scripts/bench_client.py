#!/usr/bin/env python3
"""Benchmark client for the GPUStack Operator ModelDeployment live benchmarks.

Measures per-request TTFT, inter-token latency, TPOT and end-to-end latency against
an OpenAI-compatible chat-completions endpoint, and writes one JSON record per cell.

Subcommands:
  calibrate  Measure prompt token overhead via a /v1/chat/completions probe.
  run        One cell: N requests at a fixed concurrency against one prompt shape.
  burst      Two concurrent streams with different prompt shapes (mixed-traffic suite).
  load       Long-running single-shape load that keeps recording until stopped, for
             the live scale-up window.

Every request carries a unique leading marker so a prefix cache cannot answer it
from a previous request's blocks.
"""

import argparse
import asyncio
import json
import os
import random
import statistics
import string
import sys
import time

import aiohttp

# Model names a deployment may serve, tried in order when a request is refused
# for naming a model the endpoint does not have.
ALTERNATE_MODEL_NAMES = ["/models", "model", "default", "qwen2-5-vl-7b-instruct"]


def is_wrong_model_error(status: int, body: str) -> bool:
    """Return True only when an HTTP response indicates an unknown/unserved model."""
    lower = body.lower()
    if "model" not in lower:
        return False
    if status == 404:
        return True
    if status == 400:
        return any(
            phrase in lower
            for phrase in ("not found", "does not exist", "unknown model", "invalid model")
        )
    return False

# Words that tokenize to a single token in the Qwen and DeepSeek BPE vocabularies.
# They are used to build a prompt of a target length without a tokenizer dependency.
WORDS = [
    "time", "work", "system", "people", "year", "day", "way", "man", "thing", "life",
    "child", "world", "school", "state", "family", "group", "country", "student", "company", "number",
    "point", "home", "room", "water", "area", "money", "story", "fact", "month", "book",
    "eye", "job", "word", "business", "issue", "side", "kind", "head", "house", "service",
    "friend", "father", "power", "hour", "game", "line", "end", "member", "law", "car",
    "city", "community", "name", "president", "team", "minute", "idea", "kid", "body", "information",
]


def percentile(values, pct):
    """Nearest-rank percentile over a sorted copy of values."""
    if not values:
        return None
    ordered = sorted(values)
    if len(ordered) == 1:
        return ordered[0]
    rank = max(1, min(len(ordered), int(round(pct / 100.0 * len(ordered) + 0.5))))
    return ordered[rank - 1]


def summarise(values):
    if not values:
        return None
    return {
        "mean": round(statistics.fmean(values), 3),
        "p50": round(percentile(values, 50), 3),
        "p95": round(percentile(values, 95), 3),
        "p99": round(percentile(values, 99), 3),
        "min": round(min(values), 3),
        "max": round(max(values), 3),
    }


def build_prompt(target_tokens, unique_tag, offset=0):
    """Build a prompt of about target_tokens tokens with a unique leading marker.

    The marker is a short random token sequence, so two requests never share a
    prefix and a prefix cache cannot answer the second one from the first one's
    blocks. The rest is filler drawn from WORDS, each of which is a single token
    in the Qwen and DeepSeek vocabularies.

    offset is the number of tokens the marker, the instruction line and the chat
    template add on top of the filler words. It is measured once per engine with
    a probe request and subtracted here, so the requested length is the length
    that reaches the engine.
    """
    rng = random.Random(unique_tag)
    marker = "".join(rng.choice(string.ascii_lowercase) for _ in range(12))
    head = f"Ref {marker}: summarize the following notes.\n"
    words = max(1, target_tokens - offset)
    body = []
    while len(body) < words:
        body.append(rng.choice(WORDS))
    return head + " ".join(body)


async def resolve_model(session, endpoint, model):
    """Return the model name the endpoint actually serves.

    The deployment publishes a served name that is not the artifact name, and a
    request naming a model the endpoint does not have is refused with a 404.
    Reading the endpoint's own model list once costs one request and saves a
    failed request per measurement.
    """
    try:
        async with session.get(f"{endpoint}/v1/models",
                               timeout=aiohttp.ClientTimeout(total=30)) as resp:
            if resp.status != 200:
                return model
            data = await resp.json()
    except Exception:  # noqa: BLE001 - fall back to the configured name
        return model
    served = [item.get("id") for item in (data.get("data") or []) if item.get("id")]
    if model in served:
        return model
    if served:
        print(f"warning: model '{model}' not served; falling back to '{served[0]}'", file=sys.stderr)
        return served[0]
    return model


async def one_request(session, endpoint, model, prompt, max_tokens, timeout_s):
    """Issue one streaming chat completion and measure it.

    Returns a dict of per-request measurements, or a dict with an "error" key.
    """
    payload = {
        "model": model,
        "messages": [{"role": "user", "content": prompt}],
        "max_tokens": max_tokens,
        "temperature": 0.0,
        "stream": True,
        "stream_options": {"include_usage": True},
    }
    started = time.perf_counter()
    try:
        async with session.post(
            f"{endpoint}/v1/chat/completions",
            json=payload,
            timeout=aiohttp.ClientTimeout(total=timeout_s),
        ) as resp:
            if resp.status != 200:
                body = await resp.text()
                # A router or engine rejects a model name it does not serve. The
                # operator publishes the served name on the deployment, and the
                # engine's own default is the mount path, so one retry with the
                # alternative name is worth a full request's worth of time.
                if is_wrong_model_error(resp.status, body):
                    for alternate in ALTERNATE_MODEL_NAMES:
                        if alternate == model:
                            continue
                        retry_payload = {**payload, "model": alternate}
                        async with session.post(
                            f"{endpoint}/v1/chat/completions",
                            json=retry_payload,
                            timeout=aiohttp.ClientTimeout(total=timeout_s),
                        ) as retry:
                            if retry.status != 200:
                                await retry.read()
                                continue
                            res = await _read_stream(retry, started)
                            if res.get("error"):
                                continue
                            res["served_model"] = alternate
                            return res
                return {"error": f"http {resp.status}", "body": body[:400], "e2e_ms": (time.perf_counter() - started) * 1000}
            return await _read_stream(resp, started)
    except asyncio.TimeoutError:
        return {"error": "timeout", "e2e_ms": (time.perf_counter() - started) * 1000}
    except Exception as exc:  # noqa: BLE001 - a transport failure is a measured outcome here
        return {"error": f"{type(exc).__name__}: {exc}", "e2e_ms": (time.perf_counter() - started) * 1000}


async def _read_stream(resp, started):
    """Read an SSE response body and turn it into the per-request measurements."""
    first_token_at = None
    token_times = []
    usage = {}
    buffer = b""
    done = False

    def consume_line(raw):
        nonlocal first_token_at, usage, done
        line = raw.strip()
        if not line or not line.startswith(b"data:"):
            return
        payload = line[5:].strip()
        if payload == b"[DONE]":
            done = True
            return
        try:
            event = json.loads(payload)
        except json.JSONDecodeError:
            return
        if event.get("usage"):
            usage = event["usage"]
        for choice in event.get("choices") or []:
            delta = choice.get("delta") or {}
            piece = delta.get("content")
            if piece:
                now = time.perf_counter()
                if first_token_at is None:
                    first_token_at = now
                token_times.append(now)

    while not done:
        chunk = await resp.content.read(65536)
        if not chunk:
            break
        buffer += chunk
        while b"\n" in buffer:
            raw, buffer = buffer.split(b"\n", 1)
            consume_line(raw)
            if done:
                break
    if not done and buffer.strip():
        consume_line(buffer)

    finished = time.perf_counter()
    e2e_ms = (finished - started) * 1000
    if first_token_at is None:
        return {"error": "no tokens", "e2e_ms": e2e_ms}
    ttft_ms = (first_token_at - started) * 1000
    gaps = [(token_times[i] - token_times[i - 1]) * 1000 for i in range(1, len(token_times))]
    output_tokens = usage.get("completion_tokens") or len(token_times)
    tpot_ms = (e2e_ms - ttft_ms) / max(1, output_tokens - 1) if output_tokens > 1 else 0.0
    result = {
        "ttft_ms": round(ttft_ms, 3),
        "itl_ms": [round(g, 3) for g in gaps],
        "tpot_ms": round(tpot_ms, 3),
        "e2e_ms": round(e2e_ms, 3),
        "prompt_tokens": usage.get("prompt_tokens"),
        "output_tokens": output_tokens,
        "started_at": started,
    }
    return result


async def run_cell(args):
    """One cell: a fixed concurrency of requests over one prompt shape."""
    connector = aiohttp.TCPConnector(limit=args.concurrency + 8, force_close=False)
    timeout = aiohttp.ClientTimeout(total=None)
    async with aiohttp.ClientSession(connector=connector, timeout=timeout) as session:
        model = await resolve_model(session, args.endpoint, args.model)
        # Warm-up requests are issued and discarded, so the measured cells do not
        # pay for the first-call CUDA graph capture and weight paging.
        if args.warmup > 0:
            warm_tasks = []
            for _ in range(args.warmup):
                warm_tasks.append(
                    one_request(session, args.endpoint, model,
                                build_prompt(args.input_tokens, "warm", args.token_offset),
                                args.output_tokens, args.timeout)
                )
            await asyncio.gather(*warm_tasks, return_exceptions=True)

        semaphore = asyncio.Semaphore(args.concurrency)
        results = []
        started = time.perf_counter()

        async def worker(index):
            async with semaphore:
                prompt = build_prompt(args.input_tokens, f"c{args.concurrency}-{index}", args.token_offset)
                return await one_request(session, args.endpoint, model, prompt,
                                         args.output_tokens, args.timeout)

        outcomes = await asyncio.gather(*[worker(i) for i in range(args.requests)])
        wall = time.perf_counter() - started

    return finish_cell(args, list(outcomes), wall)


def finish_cell(args, outcomes, wall):
    """Reduce per-request outcomes into the cell's aggregate record."""
    errors = [o for o in outcomes if o.get("error")]
    good = [o for o in outcomes if not o.get("error")]
    itl_all = [g for o in good for g in o.get("itl_ms", [])]
    output_tokens = sum(o.get("output_tokens") or 0 for o in good)
    record = {
        "cell": {
            "label": args.label,
            "arm": args.arm,
            "suite": args.suite,
            "shape": args.shape,
            "input_tokens_target": args.input_tokens,
            "output_tokens": args.output_tokens,
            "concurrency": args.concurrency,
            "requests": args.requests,
            "warmup": args.warmup,
            "endpoint": args.endpoint,
            "model": next((o.get("served_model") for o in good if o.get("served_model")), args.model),
        },
        "wall_seconds": round(wall, 3),
        "completed": len(good),
        "errors": len(errors),
        "error_samples": [
            {"error": e.get("error"), "body": e.get("body"), "e2e_ms": e.get("e2e_ms")}
            for e in errors[:5]
        ],
        "prompt_tokens_observed": sorted({o.get("prompt_tokens") for o in good if o.get("prompt_tokens")}),
        "ttft_ms": summarise([o["ttft_ms"] for o in good]),
        "itl_ms": summarise(itl_all),
        "tpot_ms": summarise([o["tpot_ms"] for o in good]),
        "e2e_ms": summarise([o["e2e_ms"] for o in good]),
        "output_tokens_total": output_tokens,
        "output_tok_per_s": round(output_tokens / wall, 3) if wall > 0 else None,
        "req_per_s": round(len(good) / wall, 3) if wall > 0 else None,
    }
    if args.record_requests:
        record["requests_detail"] = good
    return record


async def run_burst(args):
    """Mixed traffic: a steady decode stream and a long-prefill burst stream, concurrently.

    The steady workers keep decode requests in flight; the burst workers inject
    long prompts that force a prefill. Both run against the same endpoint for the
    same wall-clock window, so the decode stream's inter-token latency shows what
    the prefill bursts cost it.
    """
    connector = aiohttp.TCPConnector(limit=args.steady_concurrency + args.burst_concurrency + 16,
                                     force_close=False)
    async with aiohttp.ClientSession(connector=connector, timeout=aiohttp.ClientTimeout(total=None)) as session:
        model = await resolve_model(session, args.endpoint, args.model)
        started = time.perf_counter()

        async def steady_worker(index):
            out = []
            for i in range(max(1, args.steady_requests // args.steady_concurrency)):
                prompt = build_prompt(args.steady_input, f"steady-{index}-{i}", args.token_offset)
                res = await one_request(session, args.endpoint, model, prompt,
                                        args.steady_output, args.timeout)
                res["stream"] = "steady"
                out.append(res)
            return out

        async def burst_worker(index):
            out = []
            span = max(0, args.burst_input_max - args.burst_input)
            for i in range(max(1, args.burst_requests // args.burst_concurrency)):
                size = args.burst_input + (random.randint(0, span) if span else 0)
                prompt = build_prompt(size, f"burst-{index}-{i}", args.token_offset)
                res = await one_request(session, args.endpoint, model, prompt,
                                        args.burst_output, args.timeout)
                res["stream"] = "burst"
                out.append(res)
                if args.burst_interval:
                    await asyncio.sleep(args.burst_interval)
            return out

        steady_task = asyncio.gather(
            *[steady_worker(i) for i in range(args.steady_concurrency)]
        )
        burst_task = asyncio.gather(
            *[burst_worker(i) for i in range(args.burst_concurrency)]
        )
        steady_rows, burst_rows = await asyncio.gather(steady_task, burst_task)
        steady = [row for group in steady_rows for row in group]
        burst = [row for group in burst_rows for row in group]
        wall = time.perf_counter() - started

    record = {
        "cell": {
            "label": args.label,
            "arm": args.arm,
            "suite": args.suite,
            "shape": args.shape,
            "steady": {"requests": args.steady_requests, "input_tokens": args.steady_input,
                       "output_tokens": args.steady_output, "concurrency": args.steady_concurrency},
            "burst": {"requests": args.burst_requests, "input_tokens": args.burst_input,
                      "input_tokens_max": args.burst_input_max, "output_tokens": args.burst_output,
                      "concurrency": args.burst_concurrency, "interval_s": args.burst_interval},
            "endpoint": args.endpoint,
        },
        "wall_seconds": round(wall, 3),
    }
    for name, rows in (("steady", steady), ("burst", burst)):
        good = [r for r in rows if not r.get("error")]
        bad = [r for r in rows if r.get("error")]
        itl_all = [g for r in good for g in r.get("itl_ms", [])]
        out_tok = sum(r.get("output_tokens") or 0 for r in good)
        record[name] = {
            "completed": len(good),
            "errors": len(bad),
            "error_samples": [{"error": r.get("error"), "body": r.get("body")} for r in bad[:5]],
            "prompt_tokens_observed": sorted({r.get("prompt_tokens") for r in good if r.get("prompt_tokens")}),
            "ttft_ms": summarise([r["ttft_ms"] for r in good]),
            "itl_ms": summarise(itl_all),
            "tpot_ms": summarise([r["tpot_ms"] for r in good]),
            "e2e_ms": summarise([r["e2e_ms"] for r in good]),
            "output_tokens_total": out_tok,
            "output_tok_per_s": round(out_tok / wall, 3) if wall > 0 else None,
            "req_per_s": round(len(good) / wall, 3) if wall > 0 else None,
        }
    if args.record_requests:
        record["requests_detail"] = {"steady": steady, "burst": burst}
    return record


async def run_load(args):
    """A continuous load, recorded request by request, for the live scale-up window.

    Runs until the duration elapses. Every request is written to the per-request
    JSONL file with its wall-clock start and finish, so a scale-up transition can
    be located in the timeline afterwards.
    """
    connector = aiohttp.TCPConnector(limit=args.concurrency + 8, force_close=False)
    semaphore = asyncio.Semaphore(args.concurrency)
    stop_at = time.time() + args.duration
    out_path = args.requests_out
    started = time.perf_counter()
    dispatched = 0
    count = 0
    errors = 0

    async with aiohttp.ClientSession(connector=connector, timeout=aiohttp.ClientTimeout(total=None)) as session:
        model = await resolve_model(session, args.endpoint, args.model)
        with open(out_path, "w", encoding="utf-8") as sink:
            async def worker(index):
                nonlocal dispatched, count, errors
                while time.time() < stop_at:
                    async with semaphore:
                        dispatched += 1
                        req_id = dispatched
                        prompt = build_prompt(args.input_tokens, f"load-{index}-{req_id}", args.token_offset)
                        res = await one_request(session, args.endpoint, model, prompt,
                                                args.output_tokens, args.timeout)
                    count += 1
                    if res.get("error"):
                        errors += 1
                    res["seq"] = count
                    sink.write(json.dumps(res) + "\n")
                    sink.flush()

            await asyncio.gather(*[worker(i) for i in range(args.concurrency)])

    wall = time.perf_counter() - started
    return {
        "cell": {
            "label": args.label,
            "arm": args.arm,
            "suite": args.suite,
            "shape": args.shape,
            "duration_s": args.duration,
            "concurrency": args.concurrency,
            "input_tokens": args.input_tokens,
            "output_tokens": args.output_tokens,
            "endpoint": args.endpoint,
        },
        "wall_seconds": round(wall, 3),
        "requests": count,
        "errors": errors,
        "requests_jsonl": out_path,
    }


async def calibrate(args):
    """Measure the token offset between the requested length and the length the engine sees.

    One probe request reports the engine's own prompt_tokens for a prompt built
    from --input-tokens filler words. The difference is the fixed overhead of the
    marker, the instruction line and the chat template, and it is the number to
    pass to later cells as --token-offset.
    """
    words = args.input_tokens
    prompt = build_prompt(words, "calib", 0)
    async with aiohttp.ClientSession() as session:
        model = await resolve_model(session, args.endpoint, args.model)
        payload = {
            "model": model,
            "messages": [{"role": "user", "content": prompt}],
            "max_tokens": 1,
            "temperature": 0.0,
            "stream": False,
        }
        async with session.post(f"{args.endpoint}/v1/chat/completions", json=payload,
                                timeout=aiohttp.ClientTimeout(total=300)) as resp:
            if resp.status != 200:
                body = await resp.text()
                print(json.dumps({"error": f"http {resp.status}", "body": body[:400]}))
                return None
            data = await resp.json()
    observed = (data.get("usage") or {}).get("prompt_tokens")
    print(json.dumps({
        "endpoint": args.endpoint,
        "model": model,
        "filler_words": words,
        "observed_prompt_tokens": observed,
        "token_offset": (observed - words) if observed is not None else None,
    }))
    return None


def _delegate_if_local(script_name):
    if os.environ.get("IN_BENCH_DRIVER") == "1" or os.environ.get("BENCH_INCLUSTER") == "0":
        return None
    import re
    import subprocess
    script_dir = os.path.dirname(os.path.abspath(__file__))
    repo = os.path.abspath(os.path.join(script_dir, "../../.."))
    if not os.path.isdir(os.path.join(repo, ".agents")):
        repo = os.path.abspath(os.path.join(script_dir, "../../../.."))
    shim = os.path.join(repo, ".agents/skills/_e2e-lib/scripts/kubectl-shim")
    env = os.environ.copy()
    env["PATH"] = f"{shim}:{env.get('PATH', '')}"
    ns = env.get("BENCH_NS", "gpustack-bench")
    driver_ns = env.get("BENCH_DRIVER_NS", "default")
    driver_pod = env.get("BENCH_DRIVER", "bench-driver")

    def exec_ok(cmd_args, retries=3):
        for attempt in range(retries):
            if subprocess.call(
                cmd_args,
                env=env, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            ) == 0:
                return True
            time.sleep(0.5 * (attempt + 1))
        return False

    if not exec_ok(["kubectl", "-n", driver_ns, "exec", driver_pod, "--", "mkdir", "-p", "/tmp/bench"]):
        print(f"warning: driver pod {driver_ns}/{driver_pod} unavailable; running locally", file=sys.stderr)
        return None
    for name in sorted({"bench_client.py", script_name}):
        src = os.path.join(script_dir, name)
        if os.path.isfile(src):
            if not exec_ok(["kubectl", "-n", driver_ns, "cp", src, f"{driver_pod}:/tmp/bench/{name}"]):
                print(f"warning: failed to copy {name} to {driver_ns}/{driver_pod}; running locally", file=sys.stderr)
                return None

    argv = list(sys.argv[1:])
    local_out = None
    local_reqs = None
    pid = os.getpid()
    remote_out = f"/tmp/bench_out_{pid}.json"
    remote_reqs = f"/tmp/bench_reqs_{pid}.jsonl"

    def resolve_cluster_url(ep):
        if not (ep.startswith("http://127.0.0.1:") or ep.startswith("http://localhost:")):
            return ep
        m = re.search(r":(\d+)", ep[7:])
        port = m.group(1) if m else ""
        try:
            ps_out = subprocess.check_output(["ps", "-eo", "args"], text=True)
            for line in ps_out.splitlines():
                if "port-forward" in line and f"{port}:8000" in line:
                    m_svc = re.search(r"svc/([^\s]+)", line)
                    if m_svc:
                        return f"http://{m_svc.group(1)}.{ns}.svc:8000"
                    m_pod = re.search(r"pod/([^\s]+)", line)
                    if m_pod:
                        ip = subprocess.check_output(
                            ["kubectl", "-n", ns, "get", "pod", m_pod.group(1), "-o", "jsonpath={.status.podIP}"],
                            env=env, stdin=subprocess.DEVNULL, text=True
                        ).strip()
                        if ip:
                            return f"http://{ip}:8000"
        except Exception:
            pass
        try:
            ep_cluster = subprocess.check_output(
                ["kubectl", "-n", ns, "get", "modeldeployment", "-o", "jsonpath={.items[0].status.endpoint}"],
                env=env, stdin=subprocess.DEVNULL, text=True
            ).strip()
            if ep_cluster:
                return ep_cluster
        except Exception:
            pass
        return ep

    for i, a in enumerate(argv):
        if a == "--endpoint" and i + 1 < len(argv):
            argv[i + 1] = resolve_cluster_url(argv[i + 1])
        elif a == "--out" and i + 1 < len(argv) and argv[i + 1] != "-":
            local_out = argv[i + 1]
            argv[i + 1] = remote_out
        elif a == "--requests-out" and i + 1 < len(argv):
            local_reqs = argv[i + 1]
            argv[i + 1] = remote_reqs

    def read_remote_file(remote_path):
        reader_py = (
            "import sys, time\n"
            "with open(sys.argv[1], 'rb') as f:\n"
            "    data = f.read()\n"
            "chunk = 65536\n"
            "for off in range(0, len(data), chunk):\n"
            "    sys.stdout.buffer.write(data[off:off + chunk])\n"
            "    sys.stdout.buffer.flush()\n"
            "    if off + chunk < len(data):\n"
            "        time.sleep(0.02)\n"
            "if len(data) > chunk:\n"
            "    time.sleep(0.5)\n"
        )
        last_err = None
        for attempt in range(5):
            try:
                return subprocess.check_output(
                    ["kubectl", "-n", driver_ns, "exec", driver_pod, "--", "python3", "-c", reader_py, remote_path],
                    env=env, stdin=subprocess.DEVNULL,
                )
            except subprocess.CalledProcessError as err:
                last_err = err
                time.sleep(1.0 * (attempt + 1))
        raise last_err

    cmd = ["kubectl", "-n", driver_ns, "exec", driver_pod, "--", "env", "IN_BENCH_DRIVER=1", "python3", f"/tmp/bench/{script_name}"] + argv
    rc = subprocess.call(cmd, env=env, stdin=subprocess.DEVNULL)
    if local_out:
        if rc == 0:
            try:
                data = read_remote_file(remote_out)
                os.makedirs(os.path.dirname(os.path.abspath(local_out)), exist_ok=True)
                with open(local_out, "wb") as f:
                    f.write(data)
                print(f"wrote {local_out}")
            except subprocess.CalledProcessError as err:
                print(f"error: failed to retrieve {remote_out} from {driver_ns}/{driver_pod}: {err}", file=sys.stderr)
                rc = err.returncode or 1
        subprocess.call(["kubectl", "-n", driver_ns, "exec", driver_pod, "--", "rm", "-f", remote_out], env=env, stdin=subprocess.DEVNULL)
    if local_reqs:
        if rc == 0:
            try:
                data = read_remote_file(remote_reqs)
                os.makedirs(os.path.dirname(os.path.abspath(local_reqs)), exist_ok=True)
                with open(local_reqs, "wb") as f:
                    f.write(data)
            except subprocess.CalledProcessError as err:
                print(f"error: failed to retrieve {remote_reqs} from {driver_ns}/{driver_pod}: {err}", file=sys.stderr)
                rc = err.returncode or 1
        subprocess.call(["kubectl", "-n", driver_ns, "exec", driver_pod, "--", "rm", "-f", remote_reqs], env=env, stdin=subprocess.DEVNULL)
    return rc


def main():
    delegated = _delegate_if_local("bench_client.py")
    if delegated is not None:
        return delegated
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)

    def common(p):
        p.add_argument("--endpoint", required=True, help="base URL, e.g. http://127.0.0.1:8000")
        p.add_argument("--model", required=True)
        p.add_argument("--label", default="cell")
        p.add_argument("--arm", default="")
        p.add_argument("--suite", default="")
        p.add_argument("--shape", default="")
        p.add_argument("--out", default="-")
        p.add_argument("--timeout", type=float, default=600.0)
        p.add_argument("--record-requests", action="store_true")
        p.add_argument("--token-offset", type=int, default=0,
                       help="tokens the marker, instruction and chat template add to the "
                            "filler words; subtracted from --input-tokens")

    p_run = sub.add_parser("run")
    common(p_run)
    p_run.add_argument("--input-tokens", type=int, required=True)
    p_run.add_argument("--output-tokens", type=int, required=True)
    p_run.add_argument("--concurrency", type=int, required=True)
    p_run.add_argument("--requests", type=int, required=True)
    p_run.add_argument("--warmup", type=int, default=4)
    p_run.set_defaults(func=run_cell)

    for burst_name in ("burst", "mixed"):
        p_burst = sub.add_parser(burst_name)
        common(p_burst)
        p_burst.add_argument("--steady-requests", type=int, required=True)
        p_burst.add_argument("--steady-input", type=int, required=True)
        p_burst.add_argument("--steady-output", type=int, required=True)
        p_burst.add_argument("--steady-concurrency", type=int, default=8)
        p_burst.add_argument("--burst-requests", type=int, required=True)
        p_burst.add_argument("--burst-input", type=int, required=True)
        p_burst.add_argument("--burst-input-max", type=int, default=0)
        p_burst.add_argument("--burst-output", type=int, required=True)
        p_burst.add_argument("--burst-concurrency", type=int, default=4)
        p_burst.add_argument("--burst-interval", type=float, default=1.0)
        p_burst.set_defaults(func=run_burst)

    p_load = sub.add_parser("load")
    common(p_load)
    p_load.add_argument("--input-tokens", type=int, required=True)
    p_load.add_argument("--output-tokens", type=int, required=True)
    p_load.add_argument("--concurrency", type=int, required=True)
    p_load.add_argument("--duration", type=float, required=True)
    p_load.add_argument("--requests-out", required=True)
    p_load.set_defaults(func=run_load)

    p_cal = sub.add_parser("calibrate")
    p_cal.add_argument("--endpoint", required=True)
    p_cal.add_argument("--model", required=True)
    p_cal.add_argument("--input-tokens", type=int, default=512)
    p_cal.set_defaults(func=calibrate)

    args = parser.parse_args()
    result = asyncio.run(args.func(args))
    if isinstance(result, dict):
        text = json.dumps(result, indent=2)
        if getattr(args, "out", "-") == "-":
            print(text)
        else:
            os.makedirs(os.path.dirname(os.path.abspath(args.out)), exist_ok=True)
            with open(args.out, "w", encoding="utf-8") as sink:
                sink.write(text + "\n")
            print(f"wrote {args.out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
