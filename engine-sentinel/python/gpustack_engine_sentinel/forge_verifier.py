"""Independent operator verifier for ForgeSandbox kernel outputs.

Runs as `python -m gpustack_engine_sentinel.forge_verifier` with pure-stdlib
imports at module load time; torch/triton/flashinfer are imported lazily only
inside the default live-GPU executor.
"""

from __future__ import annotations

import argparse
import hashlib
import importlib.util
import json
import math
import os
import stat
import sys
import time
from collections.abc import Callable
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from gpustack_engine_sentinel.probe import (
    ALLOWED_OP_FAMILIES,
    is_excluded_operator,
    parse_local_kernel_key,
)
from gpustack_engine_sentinel.protocol import DISPATCH_MANIFEST_SCHEMA_VERSION

VERIFICATION_REPORT_SCHEMA_VERSION = "gpustack-forge-verification/v1"
DEFAULT_INPUT_DIR = "/var/run/gpustack/forge/input"
DEFAULT_OUTPUT_DIR = "/var/run/gpustack/forge/output"
DEFAULT_REPORT_PATH = "/var/run/gpustack/forge/verified/verification-report.json"
DEFAULT_MIN_SPEEDUP_RATIO = 1.05
DEFAULT_ATOL = 1e-2
DEFAULT_RTOL = 1e-2

CandidateExecutor = Callable[
    [dict[str, Any], dict[str, Any], list[dict[str, Any]], dict[str, Any], Path],
    dict[str, Any],
]


@dataclass(frozen=True)
class VerificationThresholds:
    min_speedup_ratio: float = DEFAULT_MIN_SPEEDUP_RATIO
    atol: float = DEFAULT_ATOL
    rtol: float = DEFAULT_RTOL


@dataclass
class VerificationResult:
    overall_status: str
    dispatch_manifest_digest: str
    passed_count: int
    failed_count: int
    thresholds: VerificationThresholds
    entries: list[dict[str, Any]] = field(default_factory=list)
    error: str | None = None

    def to_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {
            "schemaVersion": VERIFICATION_REPORT_SCHEMA_VERSION,
            "overallStatus": self.overall_status,
            "dispatchManifestDigest": self.dispatch_manifest_digest,
            "passedCount": self.passed_count,
            "failedCount": self.failed_count,
            "thresholds": {
                "minSpeedupRatio": self.thresholds.min_speedup_ratio,
                "atol": self.thresholds.atol,
                "rtol": self.thresholds.rtol,
            },
            "entries": self.entries,
        }
        if self.error:
            out["error"] = self.error
        return out


def _sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as fh:
        while True:
            chunk = fh.read(65536)
            if not chunk:
                break
            h.update(chunk)
    return f"sha256:{h.hexdigest()}"


def _verify_regular_file(root: Path, rel_path: str) -> Path:
    """Validate that rel_path points to a single-link regular file strictly inside root."""
    if not isinstance(rel_path, str) or not rel_path.strip():
        raise ValueError("File path must be a non-empty relative string")
    raw = Path(rel_path.strip())
    if raw.is_absolute() or ".." in raw.parts:
        raise ValueError(f"Path escapes output root: {rel_path!r}")

    root_resolved = root.resolve()
    cur = root_resolved
    for part in raw.parts:
        cur = cur / part
        try:
            st = os.lstat(cur)
        except OSError as exc:
            raise ValueError(f"Missing required path {rel_path!r}: {exc}") from exc
        if stat.S_ISLNK(st.st_mode):
            raise ValueError(f"Symlinks are forbidden in forge output: {rel_path!r}")

    resolved = (root_resolved / raw).resolve(strict=True)
    if root_resolved != resolved and root_resolved not in resolved.parents:
        raise ValueError(f"Resolved path escapes output root: {rel_path!r}")
    st = os.lstat(resolved)
    if not stat.S_ISREG(st.st_mode):
        raise ValueError(f"Path is not a regular file: {rel_path!r}")
    if st.st_nlink != 1:
        raise ValueError(
            f"Hard-linked files are forbidden in forge output: {rel_path!r}"
        )
    return resolved


def _verify_checksum(path: Path, expected: str | None) -> None:
    if not expected:
        return
    norm = expected.strip().lower()
    if not norm.startswith("sha256:"):
        norm = f"sha256:{norm}"
    actual = _sha256_file(path)
    if actual != norm:
        raise ValueError(
            f"Checksum mismatch for {path.name}: expected {norm}, got {actual}"
        )


def _load_json_file(path: Path) -> dict[str, Any]:
    data = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(data, dict):
        raise ValueError(f"Expected JSON object in {path.name}")
    return data


def _load_definitions(input_dir: Path) -> dict[str, dict[str, Any]]:
    defs: dict[str, dict[str, Any]] = {}
    defs_dir = input_dir / "definitions"
    if not defs_dir.is_dir():
        return defs
    for item in sorted(defs_dir.glob("*.json")):
        if not item.is_file() or item.is_symlink():
            continue
        obj = _load_json_file(item)
        name = str(obj.get("name") or item.stem)
        defs[name] = obj
        if isinstance(obj.get("local_kernel_key"), str):
            defs[obj["local_kernel_key"]] = obj
    return defs


def _load_workloads(input_dir: Path) -> dict[str, list[dict[str, Any]]]:
    by_def: dict[str, list[dict[str, Any]]] = {}
    wl_dir = input_dir / "workloads"
    if not wl_dir.is_dir():
        return by_def
    for item in sorted(wl_dir.glob("*.jsonl")):
        if not item.is_file() or item.is_symlink():
            continue
        stem = item.stem
        for line in item.read_text(encoding="utf-8").splitlines():
            stripped = line.strip()
            if not stripped:
                continue
            obj = json.loads(stripped)
            if not isinstance(obj, dict):
                continue
            def_name = str(obj.get("definition") or stem)
            by_def.setdefault(def_name, []).append(obj)
            if isinstance(obj.get("local_kernel_key"), str):
                by_def.setdefault(obj["local_kernel_key"], []).append(obj)
    return by_def


def _load_thresholds_from_request(
    input_dir: Path,
    *,
    min_speedup_ratio: float | None,
    atol: float | None,
    rtol: float | None,
) -> VerificationThresholds:
    req_speedup = DEFAULT_MIN_SPEEDUP_RATIO
    req_atol = DEFAULT_ATOL
    req_rtol = DEFAULT_RTOL
    req_path = input_dir / "forge-request.json"
    if req_path.is_file() and not req_path.is_symlink():
        try:
            req = _load_json_file(req_path)
            if req.get("minSpeedupRatio") is not None:
                req_speedup = float(req["minSpeedupRatio"])
            if req.get("atol") is not None:
                req_atol = float(req["atol"])
            if req.get("rtol") is not None:
                req_rtol = float(req["rtol"])
        except Exception:
            pass
    return VerificationThresholds(
        min_speedup_ratio=(
            float(min_speedup_ratio) if min_speedup_ratio is not None else req_speedup
        ),
        atol=float(atol) if atol is not None else req_atol,
        rtol=float(rtol) if rtol is not None else req_rtol,
    )


def _default_live_gpu_executor(
    entry: dict[str, Any],
    definition: dict[str, Any],
    workloads: list[dict[str, Any]],
    solution: dict[str, Any],
    output_dir: Path,
) -> dict[str, Any]:
    """Lazily import torch and benchmark candidate solution against reference on GPU."""
    del solution
    import torch  # Lazy import inside live-GPU verifier execution path only.

    lib_path = _verify_regular_file(output_dir, str(entry["libraryPath"]))
    entrypoint = str(entry.get("entrypoint") or "run").strip() or "run"
    mod_name = f"_gpustack_verify_{hashlib.sha256(str(lib_path).encode('utf-8')).hexdigest()[:16]}"
    spec = importlib.util.spec_from_file_location(mod_name, str(lib_path))
    if spec is None or spec.loader is None:
        raise RuntimeError(f"Unable to load candidate module {lib_path}")
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    cand_fn = getattr(mod, entrypoint, None)
    if not callable(cand_fn):
        raise RuntimeError(f"Candidate entrypoint {entrypoint!r} is not callable")

    ref_code = str(definition.get("reference") or "")
    ref_ns: dict[str, Any] = {"torch": torch}
    exec(ref_code, ref_ns)
    ref_fn = ref_ns.get("run")
    if not callable(ref_fn):
        raise RuntimeError("Definition reference does not define callable run(...)")

    device = "cuda" if torch.cuda.is_available() else "cpu"
    dtype_map = {
        "float16": torch.float16,
        "bfloat16": torch.bfloat16,
        "float32": torch.float32,
        "float64": torch.float64,
    }

    axes_def = definition.get("axes") or {}
    inputs_def = definition.get("inputs") or {}
    active_workloads = workloads or [{"axes": {}}]

    max_abs = 0.0
    max_rel = 0.0
    cand_total_ms = 0.0
    ref_total_ms = 0.0

    for wl in active_workloads:
        axis_vals: dict[str, int] = {}
        for ax_name, ax_spec in axes_def.items():
            if isinstance(ax_spec, dict) and ax_spec.get("type") == "const":
                axis_vals[ax_name] = int(ax_spec["value"])
        for ax_name, val in (wl.get("axes") or {}).items():
            axis_vals[ax_name] = int(val)

        tensors: list[Any] = []
        for in_spec in inputs_def.values():
            shape = [
                int(axis_vals.get(d, 1)) if isinstance(d, str) else int(d)
                for d in in_spec.get("shape", ())
            ]
            dtype = dtype_map.get(
                str(in_spec.get("dtype", "float32")).lower(), torch.float32
            )
            tensors.append(torch.randn(shape, device=device, dtype=dtype))

        ref_start = time.perf_counter()
        ref_out = ref_fn(*tensors)
        if device == "cuda":
            torch.cuda.synchronize()
        ref_total_ms += (time.perf_counter() - ref_start) * 1000.0

        cand_start = time.perf_counter()
        cand_out = cand_fn(*tensors)
        if device == "cuda":
            torch.cuda.synchronize()
        cand_total_ms += (time.perf_counter() - cand_start) * 1000.0

        diff = (cand_out.float() - ref_out.float()).abs()
        cur_abs = float(diff.max().item()) if diff.numel() else 0.0
        denom = ref_out.float().abs().clamp_min(1e-8)
        cur_rel = float((diff / denom).max().item()) if diff.numel() else 0.0
        max_abs = max(max_abs, cur_abs)
        max_rel = max(max_rel, cur_rel)

    n = max(1, len(active_workloads))
    avg_cand_ms = cand_total_ms / n
    avg_ref_ms = ref_total_ms / n
    speedup = (avg_ref_ms / avg_cand_ms) if avg_cand_ms > 0.0 else 0.0

    return {
        "status": "PASSED",
        "correctness": {
            "max_abs_error": max_abs,
            "max_relative_error": max_rel,
        },
        "performance": {
            "speedup_factor": speedup,
            "latency_ms": avg_cand_ms,
            "reference_latency_ms": avg_ref_ms,
        },
    }


def _evaluate_candidate_verdict(
    evaluation: dict[str, Any],
    thresholds: VerificationThresholds,
) -> tuple[bool, str]:
    """Assert status == PASSED, numerical error <= atol/rtol, and speedup >= minSpeedupRatio."""
    if not isinstance(evaluation, dict):
        return False, "evaluation payload must be an object"
    status = str(evaluation.get("status") or "")
    if status != "PASSED":
        return False, f"evaluation status is {status!r}, expected 'PASSED'"

    correctness = evaluation.get("correctness")
    if not isinstance(correctness, dict):
        return False, "missing evaluation.correctness"
    try:
        max_abs = float(correctness["max_abs_error"])
        max_rel = float(correctness["max_relative_error"])
    except (KeyError, TypeError, ValueError):
        return False, "invalid max_abs_error or max_relative_error"

    if not math.isfinite(max_abs) or max_abs < 0.0:
        return False, f"non-finite or negative max_abs_error: {max_abs}"
    if not math.isfinite(max_rel) or max_rel < 0.0:
        return False, f"non-finite or negative max_relative_error: {max_rel}"
    if max_abs > thresholds.atol:
        return False, f"max_abs_error {max_abs} exceeds atol {thresholds.atol}"
    if max_rel > thresholds.rtol:
        return False, f"max_relative_error {max_rel} exceeds rtol {thresholds.rtol}"

    performance = evaluation.get("performance")
    if not isinstance(performance, dict):
        return False, "missing evaluation.performance"
    try:
        speedup = float(performance["speedup_factor"])
    except (KeyError, TypeError, ValueError):
        return False, "invalid speedup_factor"

    if not math.isfinite(speedup) or speedup < thresholds.min_speedup_ratio:
        return (
            False,
            f"speedup_factor {speedup} below minSpeedupRatio {thresholds.min_speedup_ratio}",
        )

    return True, "ok"


def _write_report_atomic(report_path: Path, report: dict[str, Any]) -> None:
    report_path.parent.mkdir(parents=True, exist_ok=True)
    tmp_path = report_path.parent / f".{report_path.name}.tmp.{os.getpid()}"
    tmp_path.write_text(
        json.dumps(report, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    os.replace(tmp_path, report_path)


def verify_forge_output(
    *,
    input_dir: Path | str = DEFAULT_INPUT_DIR,
    output_dir: Path | str = DEFAULT_OUTPUT_DIR,
    report_path: Path | str | None = DEFAULT_REPORT_PATH,
    min_speedup_ratio: float | None = None,
    atol: float | None = None,
    rtol: float | None = None,
    executor: CandidateExecutor | None = None,
) -> VerificationResult:
    """Validate dispatch-manifest.json, execute candidates against reference/workloads, and write report."""
    in_root = Path(input_dir)
    out_root = Path(output_dir)
    thresholds = _load_thresholds_from_request(
        in_root,
        min_speedup_ratio=min_speedup_ratio,
        atol=atol,
        rtol=rtol,
    )

    def _finalize(result: VerificationResult) -> VerificationResult:
        if report_path is not None:
            _write_report_atomic(Path(report_path), result.to_dict())
        return result

    try:
        manifest_path = _verify_regular_file(out_root, "dispatch-manifest.json")
        manifest_digest = _sha256_file(manifest_path)
        manifest = _load_json_file(manifest_path)
    except Exception as exc:
        return _finalize(
            VerificationResult(
                overall_status="FAILED",
                dispatch_manifest_digest="",
                passed_count=0,
                failed_count=0,
                thresholds=thresholds,
                error=f"Invalid dispatch-manifest.json: {exc}",
            )
        )

    if manifest.get("schemaVersion") != DISPATCH_MANIFEST_SCHEMA_VERSION:
        return _finalize(
            VerificationResult(
                overall_status="FAILED",
                dispatch_manifest_digest=manifest_digest,
                passed_count=0,
                failed_count=0,
                thresholds=thresholds,
                error=f"Unsupported dispatch-manifest schemaVersion: {manifest.get('schemaVersion')!r}",
            )
        )

    raw_entries = manifest.get("entries")
    if not isinstance(raw_entries, list) or not raw_entries:
        return _finalize(
            VerificationResult(
                overall_status="FAILED",
                dispatch_manifest_digest=manifest_digest,
                passed_count=0,
                failed_count=0,
                thresholds=thresholds,
                error="dispatch-manifest.json contains no candidate entries",
            )
        )

    manifest_checksums = (
        manifest.get("checksums") if isinstance(manifest.get("checksums"), dict) else {}
    )
    definitions = _load_definitions(in_root)
    workloads_by_def = _load_workloads(in_root)
    run_executor = executor or _default_live_gpu_executor

    entry_reports: list[dict[str, Any]] = []
    passed_count = 0
    failed_count = 0

    for idx, entry in enumerate(raw_entries):
        if not isinstance(entry, dict):
            failed_count += 1
            entry_reports.append(
                {
                    "index": idx,
                    "localKernelKey": "",
                    "passed": False,
                    "reason": "manifest entry must be an object",
                }
            )
            continue

        raw_key = str(entry.get("localKernelKey") or "").strip()
        def_name = str(entry.get("definition") or "").strip()
        try:
            parsed_key = parse_local_kernel_key(raw_key)
            if parsed_key[
                "op_family"
            ] not in ALLOWED_OP_FAMILIES or is_excluded_operator(
                parsed_key["op_family"]
            ):
                raise ValueError(f"Excluded opFamily: {parsed_key['op_family']}")

            lib_rel = str(entry.get("libraryPath") or "")
            lib_file = _verify_regular_file(out_root, lib_rel)
            _verify_checksum(
                lib_file,
                entry.get("librarySha256") or manifest_checksums.get(lib_rel),
            )

            sol_rel = str(entry.get("solutionPath") or "")
            sol_file = _verify_regular_file(out_root, sol_rel)
            _verify_checksum(
                sol_file,
                entry.get("solutionSha256") or manifest_checksums.get(sol_rel),
            )
            solution_obj = _load_json_file(sol_file)

            trace_rel = str(entry.get("tracePath") or "")
            if trace_rel:
                trace_file = _verify_regular_file(out_root, trace_rel)
                _verify_checksum(
                    trace_file,
                    entry.get("traceSha256") or manifest_checksums.get(trace_rel),
                )
                _load_json_file(trace_file)

            definition_obj = definitions.get(def_name) or definitions.get(raw_key) or {}
            held_out = (
                workloads_by_def.get(def_name) or workloads_by_def.get(raw_key) or []
            )
            evaluation = run_executor(
                entry, definition_obj, held_out, solution_obj, out_root
            )
            ok, reason = _evaluate_candidate_verdict(evaluation, thresholds)
            if ok:
                passed_count += 1
            else:
                failed_count += 1
            entry_reports.append(
                {
                    "index": idx,
                    "localKernelKey": raw_key,
                    "definition": def_name or parsed_key["op_family"],
                    "passed": ok,
                    "reason": reason,
                    "evaluation": evaluation,
                }
            )
        except Exception as exc:
            failed_count += 1
            entry_reports.append(
                {
                    "index": idx,
                    "localKernelKey": raw_key,
                    "definition": def_name,
                    "passed": False,
                    "reason": str(exc),
                }
            )

    overall = "PASSED" if passed_count > 0 else "FAILED"
    error_msg = (
        None if passed_count > 0 else "Zero candidate kernels passed verification"
    )
    return _finalize(
        VerificationResult(
            overall_status=overall,
            dispatch_manifest_digest=manifest_digest,
            passed_count=passed_count,
            failed_count=failed_count,
            thresholds=thresholds,
            entries=entry_reports,
            error=error_msg,
        )
    )


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Verify ForgeSandbox candidate kernels against reference definitions and held-out workloads."
    )
    parser.add_argument(
        "--input-dir",
        default=DEFAULT_INPUT_DIR,
        help="Path to ForgeSandbox input directory containing forge-request.json, definitions/, workloads/.",
    )
    parser.add_argument(
        "--output-dir",
        default=DEFAULT_OUTPUT_DIR,
        help="Path to ForgeSandbox output directory containing dispatch-manifest.json and solutions/.",
    )
    parser.add_argument(
        "--report-path",
        default=DEFAULT_REPORT_PATH,
        help="Destination path for verification-report.json.",
    )
    parser.add_argument(
        "--min-speedup-ratio",
        type=float,
        default=None,
        help="Minimum required speedup_factor override.",
    )
    parser.add_argument(
        "--atol",
        type=float,
        default=None,
        help="Maximum allowed absolute numerical error override.",
    )
    parser.add_argument(
        "--rtol",
        type=float,
        default=None,
        help="Maximum allowed relative numerical error override.",
    )
    args = parser.parse_args(argv)
    result = verify_forge_output(
        input_dir=args.input_dir,
        output_dir=args.output_dir,
        report_path=args.report_path,
        min_speedup_ratio=args.min_speedup_ratio,
        atol=args.atol,
        rtol=args.rtol,
    )
    return 0 if result.overall_status == "PASSED" else 1


if __name__ == "__main__":
    sys.exit(main())
