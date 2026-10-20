#!/usr/bin/env python3
"""Multimodal VLM benchmark client for Encode, Prefill and Decode (EPD) on SXM5.

Implements the three suites in docs/modules/model-deployment/encode-prefill-decode-benchmark.md:
  - Suite 1 (run): k-context multimodal scaling (1k, 2k, 4k, 8k) at concurrencies 1, 4, 8
  - Suite 2 (burst): mixed concurrent_decode_1k (560x560, 12 reqs, c=4) + heavy_vision_4k (1792x1792 unique PNGs, 12 reqs, c=4)
  - Suite 3 (repeated): repeated-image encoder-cache reuse (shared 1792x1792 PNG, different question per request, c=4 and c=8)
"""

import argparse
import asyncio
import base64
import json
import os
import random
import statistics
import string
import struct
import sys
import time
import zlib

import aiohttp

from bench_client import (
    ALTERNATE_MODEL_NAMES,
    WORDS,
    _delegate_if_local,
    _read_stream,
    is_wrong_model_error,
    resolve_model,
    summarise,
)


def make_png_data_uri(width: int, height: int, seed: str) -> str:
    """Create a deterministic RGB PNG of size width x height seeded by `seed` using stdlib zlib."""
    rng = random.Random(seed)
    # Build 16 distinct scanline patterns and tile them so generation is fast (~5ms for 1792x1792)
    # while every unique seed produces a distinct image hash.
    palette_rows = []
    for _ in range(16):
        r0 = rng.randint(0, 255)
        g0 = rng.randint(0, 255)
        b0 = rng.randint(0, 255)
        step = rng.randint(1, 7)
        row = bytearray(1 + width * 3)
        row[0] = 0  # filter type 0 (None)
        for x in range(width):
            idx = 1 + x * 3
            row[idx] = (r0 + x * step) & 0xFF
            row[idx + 1] = (g0 + x * (step + 1)) & 0xFF
            row[idx + 2] = (b0 + x * (step + 2)) & 0xFF
        palette_rows.append(bytes(row))

    raw = b"".join(palette_rows[(y + rng.randint(0, 15)) % 16] for y in range(height))

    def chunk(tag: bytes, data: bytes) -> bytes:
        return (
            struct.pack("!I", len(data))
            + tag
            + data
            + struct.pack("!I", zlib.crc32(tag + data) & 0xFFFFFFFF)
        )

    ihdr = struct.pack("!IIBBBBB", width, height, 8, 2, 0, 0, 0)
    compressed = zlib.compress(raw, level=1)
    png = b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", ihdr) + chunk(b"IDAT", compressed) + chunk(b"IEND", b"")
    return "data:image/png;base64," + base64.b64encode(png).decode("ascii")


def build_text(text_tokens: int, unique_tag: str, token_offset: int = 14) -> str:
    """Build a text prompt of about text_tokens tokens after subtracting token_offset.

    token_offset (default 14 for Qwen2.5-VL) accounts for the marker, instruction
    line, and chat-template framing around the filler words; measure it on another
    engine or model with the `calibrate` subcommand and pass `--token-offset`.
    """
    rng = random.Random(unique_tag)
    marker = "".join(rng.choice(string.ascii_lowercase) for _ in range(12))
    head = f"Ref {marker}: describe the visual details and summarize these notes.\n"
    words = max(16, text_tokens - token_offset)
    body = [rng.choice(WORDS) for _ in range(words)]
    return head + " ".join(body)


async def one_vlm_request(session, endpoint, model, image_uri, text_prompt, max_tokens, timeout_s):
    payload = {
        "model": model,
        "messages": [
            {
                "role": "user",
                "content": [
                    {"type": "image_url", "image_url": {"url": image_uri}},
                    {"type": "text", "text": text_prompt},
                ],
            }
        ],
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
                if is_wrong_model_error(resp.status, body):
                    for alt in ALTERNATE_MODEL_NAMES:
                        if alt == model:
                            continue
                        payload["model"] = alt
                        async with session.post(
                            f"{endpoint}/v1/chat/completions",
                            json=payload,
                            timeout=aiohttp.ClientTimeout(total=timeout_s),
                        ) as retry:
                            if retry.status != 200:
                                await retry.read()
                                continue
                            res = await _read_stream(retry, started)
                            if res.get("error"):
                                continue
                            res["served_model"] = alt
                            return res
                return {"error": f"http {resp.status}", "body": body[:400], "e2e_ms": (time.perf_counter() - started) * 1000}
            return await _read_stream(resp, started)
    except asyncio.TimeoutError:
        return {"error": "timeout", "e2e_ms": (time.perf_counter() - started) * 1000}
    except Exception as exc:
        return {"error": f"{type(exc).__name__}: {exc}", "e2e_ms": (time.perf_counter() - started) * 1000}


def finish_cell(meta, outcomes, wall):
    errors = [o for o in outcomes if o.get("error")]
    good = [o for o in outcomes if not o.get("error")]
    itl_all = [g for o in good for g in o.get("itl_ms", [])]
    output_tokens = sum(o.get("output_tokens") or 0 for o in good)
    return {
        "cell": meta,
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


async def cmd_run(args):
    """Suite 1 or Suite 3 cell."""
    connector = aiohttp.TCPConnector(limit=args.concurrency + 8, force_close=False)
    async with aiohttp.ClientSession(connector=connector, timeout=aiohttp.ClientTimeout(total=None)) as session:
        model = await resolve_model(session, args.endpoint, args.model)
        shared_uri = make_png_data_uri(args.image_size, args.image_size, f"shared-{args.label}") if args.shared_image else None
        # Pre-generate unique images before starting the timer so PNG synthesis is not in wall clock
        images = [
            shared_uri if shared_uri else make_png_data_uri(args.image_size, args.image_size, f"{args.arm}-{args.label}-req-{i}")
            for i in range(args.requests)
        ]
        prompts = [build_text(args.text_tokens, f"{args.arm}-{args.label}-req-{i}", args.token_offset) for i in range(args.requests)]

        if args.warmup > 0:
            warm_img = shared_uri if shared_uri else make_png_data_uri(args.image_size, args.image_size, f"warm-{args.label}")
            warm_tasks = [
                one_vlm_request(
                    session, args.endpoint, model, warm_img,
                    build_text(args.text_tokens, f"warm-{i}", args.token_offset), args.output_tokens, args.timeout
                )
                for i in range(args.warmup)
            ]
            await asyncio.gather(*warm_tasks, return_exceptions=True)

        semaphore = asyncio.Semaphore(args.concurrency)
        started = time.perf_counter()

        async def worker(idx):
            async with semaphore:
                return await one_vlm_request(
                    session, args.endpoint, model, images[idx], prompts[idx], args.output_tokens, args.timeout
                )

        outcomes = await asyncio.gather(*[worker(i) for i in range(args.requests)])
        wall = time.perf_counter() - started

    meta = {
        "label": args.label,
        "arm": args.arm,
        "suite": args.suite,
        "image_size": args.image_size,
        "shared_image": args.shared_image,
        "text_tokens": args.text_tokens,
        "output_tokens": args.output_tokens,
        "concurrency": args.concurrency,
        "requests": args.requests,
        "model": next((o.get("served_model") for o in outcomes if o.get("served_model")), model),
    }
    return finish_cell(meta, list(outcomes), wall)


async def cmd_burst(args):
    """Suite 2: concurrent_decode_1k + heavy_vision_4k."""
    connector = aiohttp.TCPConnector(limit=args.small_concurrency + args.heavy_concurrency + 8, force_close=False)
    async with aiohttp.ClientSession(connector=connector, timeout=aiohttp.ClientTimeout(total=None)) as session:
        model = await resolve_model(session, args.endpoint, args.model)
        small_imgs = [make_png_data_uri(560, 560, f"{args.arm}-small-{i}") for i in range(args.small_requests)]
        small_prompts = [build_text(624, f"{args.arm}-small-{i}", args.token_offset) for i in range(args.small_requests)]
        heavy_imgs = [make_png_data_uri(1792, 1792, f"{args.arm}-heavy-{i}") for i in range(args.heavy_requests)]
        heavy_prompts = [build_text(400, f"{args.arm}-heavy-{i}", args.token_offset) for i in range(args.heavy_requests)]

        sem_small = asyncio.Semaphore(args.small_concurrency)
        sem_heavy = asyncio.Semaphore(args.heavy_concurrency)
        started = time.perf_counter()

        async def run_small(idx):
            async with sem_small:
                return await one_vlm_request(session, args.endpoint, model, small_imgs[idx], small_prompts[idx], 256, args.timeout)

        async def run_heavy(idx):
            async with sem_heavy:
                return await one_vlm_request(session, args.endpoint, model, heavy_imgs[idx], heavy_prompts[idx], 64, args.timeout)

        small_task = asyncio.gather(*[run_small(i) for i in range(args.small_requests)])
        heavy_task = asyncio.gather(*[run_heavy(i) for i in range(args.heavy_requests)])
        small_outcomes, heavy_outcomes = await asyncio.gather(small_task, heavy_task)
        wall = time.perf_counter() - started

    return {
        "arm": args.arm,
        "suite": "suite2",
        "wall_seconds": round(wall, 3),
        "concurrent_decode_1k": finish_cell({"stream": "concurrent_decode_1k", "arm": args.arm}, list(small_outcomes), wall),
        "heavy_vision_4k": finish_cell({"stream": "heavy_vision_4k", "arm": args.arm}, list(heavy_outcomes), wall),
    }


async def cmd_calibrate(args):
    """Measure the text-template token offset (excluding image tokens) on the target endpoint."""
    words = args.text_tokens
    prompt = build_text(words, "calib", 0)
    async with aiohttp.ClientSession() as session:
        model = await resolve_model(session, args.endpoint, args.model)
        payload = {
            "model": model,
            "messages": [{"role": "user", "content": prompt}],
            "max_tokens": 1,
            "temperature": 0.0,
            "stream": False,
        }
        async with session.post(
            f"{args.endpoint}/v1/chat/completions",
            json=payload,
            timeout=aiohttp.ClientTimeout(total=300),
        ) as resp:
            if resp.status != 200:
                body = await resp.text()
                return {"error": f"http {resp.status}", "body": body[:400]}
            data = await resp.json()
    observed = (data.get("usage") or {}).get("prompt_tokens")
    return {
        "endpoint": args.endpoint,
        "model": model,
        "filler_words": words,
        "observed_prompt_tokens": observed,
        "token_offset": (observed - words) if observed is not None else None,
    }


def main():
    delegated = _delegate_if_local("bench_vlm.py")
    if delegated is not None:
        return delegated
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)

    p_cal = sub.add_parser("calibrate")
    p_cal.add_argument("--endpoint", required=True)
    p_cal.add_argument("--model", default="/models")
    p_cal.add_argument("--text-tokens", type=int, default=128)
    p_cal.add_argument("--out", default="-")
    p_cal.set_defaults(func=cmd_calibrate)

    p_run = sub.add_parser("run")
    p_run.add_argument("--endpoint", required=True)
    p_run.add_argument("--model", default="/models")
    p_run.add_argument("--arm", required=True)
    p_run.add_argument("--suite", default="suite1")
    p_run.add_argument("--label", required=True)
    p_run.add_argument("--image-size", type=int, required=True)
    p_run.add_argument("--text-tokens", type=int, required=True)
    p_run.add_argument("--token-offset", type=int, default=14)
    p_run.add_argument("--output-tokens", type=int, default=128)
    p_run.add_argument("--concurrency", type=int, required=True)
    p_run.add_argument("--requests", type=int, required=True)
    p_run.add_argument("--warmup", type=int, default=2)
    p_run.add_argument("--shared-image", action="store_true")
    p_run.add_argument("--timeout", type=float, default=600.0)
    p_run.add_argument("--out", default="-")
    p_run.set_defaults(func=cmd_run)

    p_burst = sub.add_parser("burst")
    p_burst.add_argument("--endpoint", required=True)
    p_burst.add_argument("--model", default="/models")
    p_burst.add_argument("--arm", required=True)
    p_burst.add_argument("--token-offset", type=int, default=14)
    p_burst.add_argument("--small-requests", type=int, default=12)
    p_burst.add_argument("--small-concurrency", type=int, default=4)
    p_burst.add_argument("--heavy-requests", type=int, default=12)
    p_burst.add_argument("--heavy-concurrency", type=int, default=4)
    p_burst.add_argument("--timeout", type=float, default=600.0)
    p_burst.add_argument("--out", default="-")
    p_burst.set_defaults(func=cmd_burst)

    args = parser.parse_args()
    result = asyncio.run(args.func(args))
    text = json.dumps(result, indent=2)
    if args.out == "-":
        print(text)
    else:
        os.makedirs(os.path.dirname(os.path.abspath(args.out)), exist_ok=True)
        with open(args.out, "w", encoding="utf-8") as f:
            f.write(text + "\n")
        print(f"wrote {args.out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
