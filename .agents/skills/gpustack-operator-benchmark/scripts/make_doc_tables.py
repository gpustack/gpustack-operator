#!/usr/bin/env python3
"""Render benchmark cell JSON as the long Markdown tables the benchmark pages carry.

    make_doc_tables.py <directory> [--arms arm1,arm2] [--arm-labels arm1=Aggregated,arm2=Prefill-Decode]
                        [--suite suite1] [--title "Suite 1: ..."]

One row per cell. Every number comes from the cell JSON; nothing is recomputed
from a model.
"""

import argparse
import json
import os
import sys

ARM_LABELS = {
    "arm1": "Aggregated",
    "arm2": "Prefill-Decode",
    "state1": "Before scale-up",
    "state3": "After scale-up",
    "wide-tp": "Wide TP",
    "external-dp": "External DP",
}


def load_cells(directory, suite=None):
    cells = []
    for name in sorted(os.listdir(directory)):
        if not name.endswith(".json"):
            continue
        path = os.path.join(directory, name)
        try:
            with open(path, encoding="utf-8") as handle:
                data = json.load(handle)
        except (OSError, json.JSONDecodeError) as err:
            print(f"warning: skipping unreadable cell file {path}: {err}", file=sys.stderr)
            continue
        if "cell" not in data:
            continue
        if suite and (data["cell"].get("suite") or "") != suite:
            continue
        cells.append(data)
    return cells


def stat(data, metric, name):
    return (data.get(metric) or {}).get(name)


def fmt(value, digits=1):
    if value is None:
        return "-"
    return f"{value:.{digits}f}"


def observed_prompt(data):
    seen = data.get("prompt_tokens_observed") or []
    if not seen:
        return "-"
    low, high = min(seen), max(seen)
    if low == high:
        return str(low)
    return f"{low} to {high}"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directory")
    parser.add_argument("--arms", default="")
    parser.add_argument("--arm-labels", default="")
    parser.add_argument("--suite", default="")
    parser.add_argument("--shapes", default="")
    parser.add_argument("--title", default="")
    parser.add_argument("--shape-header", default="Workload")
    args = parser.parse_args()

    cells = load_cells(args.directory, args.suite)
    if not cells:
        print(f"no cells under {args.directory}", file=sys.stderr)
        return 1

    labels = {}
    for pair in filter(None, args.arm_labels.split(",")):
        key, _, value = pair.partition("=")
        labels[key] = value

    order = [a for a in args.arms.split(",") if a]
    arms = [a for a in order if any(c["cell"].get("arm") == a for c in cells)]
    arms += [a for a in dict.fromkeys(c["cell"].get("arm") for c in cells) if a not in arms]

    shapes = [s for s in args.shapes.split(",") if s]
    if not shapes:
        shapes = [s for s in dict.fromkeys(c["cell"].get("shape") for c in cells) if s]

    def shape_key(shape):
        try:
            ins, outs = shape.split("/")
            return (int(ins), int(outs))
        except (ValueError, AttributeError):
            return (0, 0)

    shapes.sort(key=shape_key)

    if args.title:
        print(f"### {args.title}")
        print()

    header = [
        args.shape_header,
        "Prompt tokens",
        "Concurrency",
        "Arm",
        "Requests",
        "TTFT p50 (ms)",
        "TTFT p95 (ms)",
        "TPOT p50 (ms)",
        "ITL p99 (ms)",
        "Output (tok/s)",
        "Throughput (req/s)",
    ]
    print("| " + " | ".join(header) + " |")
    print("|" + "|".join(["---"] * len(header)) + "|")

    for shape in shapes:
        for conc in sorted({c["cell"].get("concurrency") for c in cells
                            if c["cell"].get("shape") == shape
                            and c["cell"].get("concurrency") is not None}):
            for arm in arms:
                data = next((c for c in cells
                             if c["cell"].get("shape") == shape
                             and c["cell"].get("concurrency") == conc
                             and c["cell"].get("arm") == arm), None)
                if not data:
                    continue
                cell = data["cell"]
                requests = cell.get("requests") or 0
                completed = data.get("completed")
                errors = data.get("errors") or 0
                req_text = f"{completed}/{requests}" if completed is not None else f"-/{requests}"
                if errors:
                    req_text += f" ({errors} failed)"
                print("| " + " | ".join([
                    f"`{shape}`",
                    observed_prompt(data),
                    str(conc),
                    labels.get(arm, ARM_LABELS.get(arm, arm)),
                    req_text,
                    fmt(stat(data, "ttft_ms", "p50")),
                    fmt(stat(data, "ttft_ms", "p95")),
                    fmt(stat(data, "tpot_ms", "p50"), 2),
                    fmt(stat(data, "itl_ms", "p99")),
                    fmt(data.get("output_tok_per_s"), 2),
                    fmt(data.get("req_per_s"), 3),
                ]) + " |")
    print()
    return 0


if __name__ == "__main__":
    sys.exit(main())
