"""Independent operator verifier for ForgeSandbox kernel outputs.

Runs as `python -m gpustack_engine_sentinel.forge_verifier` with pure-stdlib
imports at module load time; torch/triton/flashinfer are imported lazily only
inside the default live-GPU executor.
"""

from __future__ import annotations

import argparse
import contextlib
import hashlib
import importlib.util
import inspect
import json
import math
import os
import secrets
import select
import signal
import stat
import statistics
import subprocess
import sys
import tempfile
import time
import types
from collections.abc import Callable, Sequence
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from gpustack_engine_sentinel.kernel_loader import arch_compatible
from gpustack_engine_sentinel.probe import (
    _OP_REFERENCES,
    ALLOWED_OP_FAMILIES,
    canonical_engine_name,
    is_excluded_operator,
    normalize_engine_version,
    parse_local_kernel_key,
)
from gpustack_engine_sentinel.protocol import DISPATCH_MANIFEST_SCHEMA_VERSION

VERIFICATION_REPORT_SCHEMA_VERSION = "gpustack-forge-verification/v1"
DEFAULT_INPUT_DIR = "/var/run/gpustack/forge/input"
DEFAULT_OUTPUT_DIR = "/var/run/gpustack/forge/output"
DEFAULT_REPORT_PATH = "/var/run/gpustack/forge/verified/verification-report.json"
DEFAULT_MIN_SPEEDUP_RATIO = 1.05
MIN_ALLOWED_SPEEDUP_RATIO = 1.01
DEFAULT_ATOL = 1e-2
DEFAULT_RTOL = 1e-2
DEFAULT_WARMUP_ITERS = 3
DEFAULT_BENCH_ITERS = 10
DEFAULT_SUBPROCESS_TIMEOUT_SECONDS = 300
ENV_VERIFY_WARMUP_ITERS = "GPUSTACK_FORGE_VERIFY_WARMUP_ITERS"
ENV_VERIFY_BENCH_ITERS = "GPUSTACK_FORGE_VERIFY_BENCH_ITERS"
_MAX_SAFE_JSON_TENSOR_BYTES = 256 * 1024 * 1024

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


class _PipeLineReader:
    """Unbuffered fd line reader compatible with select() deadlines."""

    def __init__(self, fd: int) -> None:
        self.fd = fd
        self._buf = bytearray()

    def read_line(self, deadline: float | None = None) -> str:
        while True:
            nl = self._buf.find(b"\n")
            if nl >= 0:
                raw = bytes(self._buf[:nl])
                del self._buf[: nl + 1]
                return raw.decode("utf-8")
            if deadline is not None:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise TimeoutError("Subprocess pipe read timed out")
                ready, _, _ = select.select([self.fd], [], [], remaining)
                if not ready:
                    raise TimeoutError("Subprocess pipe read timed out")
            chunk = os.read(self.fd, 65536)
            if not chunk:
                if not self._buf:
                    return ""
                raw = bytes(self._buf)
                self._buf.clear()
                return raw.decode("utf-8")
            self._buf.extend(chunk)


def _write_json_line(
    fd: int,
    payload: dict[str, Any],
    *,
    write_fn: Callable[[int, bytes], int] = os.write,
) -> None:
    encoded = (json.dumps(payload, separators=(",", ":")) + "\n").encode("utf-8")
    view = memoryview(encoded)
    written = 0
    while written < len(encoded):
        n = write_fn(fd, view[written:])
        if n <= 0:
            raise OSError("Short write on subprocess pipe")
        written += n


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
        if not item.is_file():
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
        if not item.is_file():
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


def _load_forge_request(input_dir: Path) -> dict[str, Any] | None:
    req_path = input_dir / "forge-request.json"
    if not req_path.is_file():
        return None
    return _load_json_file(req_path)


def _parse_threshold_float(
    val: Any,
    name: str,
    *,
    minimum: float = 0.0,
) -> float:
    if isinstance(val, bool):
        raise ValueError(f"{name} must be a finite number, not bool")
    try:
        parsed = float(val)
    except (TypeError, ValueError) as exc:
        raise ValueError(f"{name} must be a finite number, got {val!r}") from exc
    if not math.isfinite(parsed) or parsed < minimum:
        raise ValueError(f"{name} must be finite and >= {minimum}, got {val!r}")
    return parsed


def _safe_fallback_threshold(val: Any, default: float) -> float:
    if val is None or isinstance(val, bool):
        return default
    try:
        parsed = float(val)
    except (TypeError, ValueError):
        return default
    if not math.isfinite(parsed) or parsed < 0.0:
        return default
    return parsed


def _resolve_thresholds(
    req: dict[str, Any] | None,
    *,
    min_speedup_ratio: float | None,
    atol: float | None,
    rtol: float | None,
) -> VerificationThresholds:
    req_speedup = DEFAULT_MIN_SPEEDUP_RATIO
    req_atol = DEFAULT_ATOL
    req_rtol = DEFAULT_RTOL
    if isinstance(req, dict):
        if req.get("minSpeedupRatio") is not None:
            min_req_speedup = (
                MIN_ALLOWED_SPEEDUP_RATIO if min_speedup_ratio is None else 0.0
            )
            req_speedup = _parse_threshold_float(
                req["minSpeedupRatio"],
                "minSpeedupRatio",
                minimum=min_req_speedup,
            )
        if req.get("atol") is not None:
            req_atol = _parse_threshold_float(req["atol"], "atol", minimum=0.0)
        if req.get("rtol") is not None:
            req_rtol = _parse_threshold_float(req["rtol"], "rtol", minimum=0.0)
    eff_speedup = (
        _parse_threshold_float(min_speedup_ratio, "min_speedup_ratio", minimum=0.0)
        if min_speedup_ratio is not None
        else req_speedup
    )
    eff_atol = (
        _parse_threshold_float(atol, "atol", minimum=0.0)
        if atol is not None
        else req_atol
    )
    eff_rtol = (
        _parse_threshold_float(rtol, "rtol", minimum=0.0)
        if rtol is not None
        else req_rtol
    )
    return VerificationThresholds(
        min_speedup_ratio=eff_speedup,
        atol=eff_atol,
        rtol=eff_rtol,
    )


def _resolve_iter_counts(
    warmup_iters: int | None = None,
    bench_iters: int | None = None,
) -> tuple[int, int]:
    if warmup_iters is None:
        raw_w = os.environ.get(ENV_VERIFY_WARMUP_ITERS, "").strip()
        eff_warmup = int(raw_w) if raw_w else DEFAULT_WARMUP_ITERS
    else:
        eff_warmup = int(warmup_iters)

    if bench_iters is None:
        raw_b = os.environ.get(ENV_VERIFY_BENCH_ITERS, "").strip()
        eff_bench = int(raw_b) if raw_b else DEFAULT_BENCH_ITERS
    else:
        eff_bench = int(bench_iters)

    if eff_warmup < 0:
        raise ValueError(f"warmup_iters must be >= 0, got {eff_warmup}")
    if eff_bench < 1:
        raise ValueError(f"bench_iters must be >= 1, got {eff_bench}")
    return eff_warmup, eff_bench


_INTEGER_DTYPE_NAMES: frozenset[str] = frozenset(
    {"int64", "int32", "int16", "int8", "uint8", "long", "int"}
)
_LOWP_FLOAT_DTYPE_NAMES: frozenset[str] = frozenset({"float16", "bfloat16", "half"})


def _canonical_dtype_name(raw_dtype: Any) -> str:
    if raw_dtype is None:
        return ""
    text = str(raw_dtype).strip().lower().removeprefix("torch.")
    if text == "long":
        return "int64"
    if text == "int":
        return "int32"
    if text == "half":
        return "float16"
    return text


def _is_real_torch_module(torch_mod: Any) -> bool:
    tensor_cls = getattr(torch_mod, "Tensor", None)
    return (
        isinstance(tensor_cls, type)
        and callable(getattr(torch_mod, "save", None))
        and callable(getattr(torch_mod, "load", None))
    )


def _normalize_tuple_tree(obj: Any) -> Any:
    if isinstance(obj, tuple):
        return tuple(_normalize_tuple_tree(x) for x in obj)
    if isinstance(obj, list):
        return [_normalize_tuple_tree(x) for x in obj]
    return obj


def _clone_tensors(torch_mod: Any, items: list[Any]) -> list[Any]:
    out: list[Any] = []
    for item in items:
        clone_fn = getattr(item, "clone", None)
        if callable(clone_fn):
            out.append(clone_fn())
        elif hasattr(item, "_vals") and hasattr(item, "shape"):
            dtype = (
                _canonical_dtype_name(getattr(item, "dtype", "float32")) or "float32"
            )
            vals = (
                [int(v) for v in item._vals]
                if dtype in _INTEGER_DTYPE_NAMES
                else [float(v) for v in item._vals]
            )
            try:
                out.append(type(item)(vals, item.shape, dtype=dtype))
            except TypeError:
                out.append(type(item)(vals, item.shape))
        else:
            out.append(item)
    return out


def _clone_output_tree(torch_mod: Any, obj: Any) -> Any:
    if isinstance(obj, tuple):
        return tuple(_clone_output_tree(torch_mod, x) for x in obj)
    if isinstance(obj, list):
        return [_clone_output_tree(torch_mod, x) for x in obj]
    return _clone_tensors(torch_mod, [obj])[0]


def _upcast_float_inputs_to_fp32(torch_mod: Any, items: list[Any]) -> list[Any]:
    fp32_dtype = getattr(torch_mod, "float32", "float32")
    out: list[Any] = []
    for item in _clone_tensors(torch_mod, items):
        dt = _canonical_dtype_name(getattr(item, "dtype", None))
        if dt in _LOWP_FLOAT_DTYPE_NAMES:
            float_fn = getattr(item, "float", None)
            to_fn = getattr(item, "to", None)
            if callable(float_fn):
                out.append(float_fn())
            elif callable(to_fn):
                out.append(to_fn(fp32_dtype))
            else:
                out.append(item)
        else:
            out.append(item)
    return out


def _detach_to_cpu(obj: Any) -> Any:
    if isinstance(obj, tuple):
        return tuple(_detach_to_cpu(x) for x in obj)
    if isinstance(obj, list):
        return [_detach_to_cpu(x) for x in obj]
    detach_fn = getattr(obj, "detach", None)
    if callable(detach_fn):
        obj = detach_fn()
    cpu_fn = getattr(obj, "cpu", None)
    if callable(cpu_fn):
        obj = cpu_fn()
    return obj


def _move_to_device(obj: Any, device: str) -> Any:
    if device != "cuda":
        return obj
    if isinstance(obj, tuple):
        return tuple(_move_to_device(x, device) for x in obj)
    if isinstance(obj, list):
        return [_move_to_device(x, device) for x in obj]
    to_fn = getattr(obj, "to", None)
    if callable(to_fn):
        return to_fn(device)
    return obj


def _encode_fake_tensor_tree(obj: Any) -> Any:
    if isinstance(obj, tuple):
        return {
            "__gpustack_seq__": "tuple",
            "items": [_encode_fake_tensor_tree(x) for x in obj],
        }
    if isinstance(obj, list):
        return {
            "__gpustack_seq__": "list",
            "items": [_encode_fake_tensor_tree(x) for x in obj],
        }
    if hasattr(obj, "_vals") and hasattr(obj, "shape"):
        dtype = _canonical_dtype_name(getattr(obj, "dtype", "float32")) or "float32"
        vals = (
            [int(v) for v in obj._vals]
            if dtype in _INTEGER_DTYPE_NAMES
            else [float(v) for v in obj._vals]
        )
        return {
            "__gpustack_tensor__": True,
            "vals": vals,
            "shape": [int(d) for d in obj.shape],
            "dtype": dtype,
        }
    return {"__gpustack_non_tensor__": type(obj).__name__}


def _decode_fake_tensor_tree(torch_mod: Any, data: Any) -> Any:
    if isinstance(data, dict):
        if data.get("__gpustack_seq__") == "tuple":
            return tuple(
                _decode_fake_tensor_tree(torch_mod, x) for x in data.get("items", ())
            )
        if data.get("__gpustack_seq__") == "list":
            return [
                _decode_fake_tensor_tree(torch_mod, x) for x in data.get("items", ())
            ]
        if data.get("__gpustack_tensor__") is True:
            dtype = str(data.get("dtype") or "float32")
            raw_vals = data.get("vals", ())
            vals = (
                [int(v) for v in raw_vals]
                if _canonical_dtype_name(dtype) in _INTEGER_DTYPE_NAMES
                else [float(v) for v in raw_vals]
            )
            shape = tuple(int(d) for d in data.get("shape", ()))
            fake_cls = getattr(torch_mod, "_FakeTensor", None)
            if callable(fake_cls):
                try:
                    return fake_cls(vals, shape, dtype=dtype)
                except TypeError:
                    inst = fake_cls(vals, shape)
                    inst.dtype = dtype
                    return inst
            t = torch_mod.tensor(vals, dtype=dtype)
            t.shape = shape
            if not hasattr(t, "dtype"):
                t.dtype = dtype
            return t
        if "__gpustack_non_tensor__" in data:
            return None
    return None


def _save_tensor_payload(torch_mod: Any, payload: Any, dest: Path) -> None:
    normalized = _normalize_tuple_tree(payload)
    if _is_real_torch_module(torch_mod):
        torch_mod.save(_detach_to_cpu(normalized), str(dest))
        return
    encoded = _encode_fake_tensor_tree(normalized)
    dest.write_text(json.dumps(encoded, separators=(",", ":")), encoding="utf-8")


def _load_tensor_payload(
    torch_mod: Any,
    src: Path,
    *,
    expected_dir: Path | None = None,
    device: str = "cpu",
) -> Any:
    try:
        st = os.lstat(src)
    except OSError as exc:
        raise RuntimeError(
            f"Missing candidate output tensor file {src.name}: {exc}"
        ) from exc
    if stat.S_ISLNK(st.st_mode) or not stat.S_ISREG(st.st_mode):
        raise RuntimeError(f"Candidate output path {src.name} is not a regular file")
    if expected_dir is not None:
        resolved = src.resolve(strict=True)
        exp_root = expected_dir.resolve(strict=True)
        if exp_root not in resolved.parents:
            raise RuntimeError(
                f"Candidate output path {src.name} escapes temp directory"
            )

    if _is_real_torch_module(torch_mod):
        loaded = torch_mod.load(str(src), map_location="cpu", weights_only=True)
        return _move_to_device(loaded, device)

    if st.st_size > _MAX_SAFE_JSON_TENSOR_BYTES:
        raise RuntimeError("Serialized tensor payload exceeds size cap")
    raw = json.loads(src.read_text(encoding="utf-8"))
    return _decode_fake_tensor_tree(torch_mod, raw)


def _extract_dtype_tree(obj: Any) -> Any:
    if isinstance(obj, (tuple, list)):
        return [_extract_dtype_tree(x) for x in obj]
    return _canonical_dtype_name(getattr(obj, "dtype", None))


def _validate_single_tensor_meta(
    cand_out: Any,
    ref_out: Any,
    *,
    expected_dtype: str | None = None,
) -> tuple[tuple[int, ...], str]:
    if (
        cand_out is None
        or ref_out is None
        or getattr(cand_out, "shape", None) is None
        or getattr(ref_out, "shape", None) is None
        or not callable(getattr(cand_out, "numel", None))
        or not callable(getattr(ref_out, "numel", None))
    ):
        raise RuntimeError("Candidate or reference output is not a valid tensor")

    cand_shape = tuple(int(d) for d in cand_out.shape)
    ref_shape = tuple(int(d) for d in ref_out.shape)
    if cand_shape != ref_shape:
        raise RuntimeError(
            f"Output shape mismatch: candidate {cand_shape} != reference {ref_shape}"
        )

    cand_dtype = _canonical_dtype_name(getattr(cand_out, "dtype", None))
    want_dtype = expected_dtype or _canonical_dtype_name(
        getattr(ref_out, "dtype", None)
    )
    if not cand_dtype or not want_dtype or cand_dtype != want_dtype:
        raise RuntimeError(
            f"Output dtype mismatch: candidate {cand_dtype!r} != reference {want_dtype!r}"
        )

    ref_numel = int(ref_out.numel())
    cand_numel = int(cand_out.numel())
    if ref_numel <= 0 or cand_numel <= 0 or cand_numel != ref_numel or 0 in ref_shape:
        raise RuntimeError(
            f"Output tensor must be non-empty: shape={ref_shape}, numel={ref_numel}"
        )
    return cand_shape, cand_dtype


def _tensor_flat_floats(tensor: Any) -> list[float]:
    if hasattr(tensor, "_vals"):
        return [float(v) for v in tensor._vals]
    return [float(v) for v in tensor.detach().cpu().float().reshape(-1).tolist()]


def _tensor_flat_ints(tensor: Any) -> list[int]:
    if hasattr(tensor, "_vals"):
        return [int(v) for v in tensor._vals]
    return [int(v) for v in tensor.detach().cpu().reshape(-1).tolist()]


def _compare_topk_indices(
    cand_idx: Any,
    ref_idx: Any,
    *,
    atol: float,
    rtol: float,
    expected_dtype: str | None = None,
    inputs: Sequence[Any] | None = None,
) -> bool:
    shape, _ = _validate_single_tensor_meta(
        cand_idx, ref_idx, expected_dtype=expected_dtype
    )
    k = shape[-1]
    cand_flat = _tensor_flat_ints(cand_idx)
    ref_flat = _tensor_flat_ints(ref_idx)
    num_rows = len(cand_flat) // k

    dim_size: int | None = None
    score_flat: list[float] | None = None
    if inputs:
        in0 = inputs[0]
        if getattr(in0, "shape", None) is not None:
            in_shape = tuple(int(d) for d in in0.shape)
            if len(in_shape) >= 1 and in_shape[:-1] == shape[:-1]:
                dim_size = in_shape[-1]
                score_flat = _tensor_flat_floats(in0)

    for r in range(num_rows):
        c_row = cand_flat[r * k : (r + 1) * k]
        r_row = ref_flat[r * k : (r + 1) * k]
        for idx in c_row:
            if idx < 0 or (dim_size is not None and idx >= dim_size):
                return False
        if len(set(c_row)) != k:
            return False
        if score_flat is not None and dim_size is not None:
            s_row = score_flat[r * dim_size : (r + 1) * dim_size]
            if (
                len(s_row) != dim_size
                or not s_row
                or not all(math.isfinite(v) for v in s_row)
            ):
                return False
            m = max(s_row)
            # Compare shift-invariant max-normalized weights so tolerance does not shrink with dim_size.
            q_row = [math.exp(v - m) for v in s_row]
            for c_i, r_i in zip(c_row, r_row, strict=True):
                if r_i < 0 or r_i >= dim_size:
                    return False
                if abs(q_row[c_i] - q_row[r_i]) > atol + rtol * abs(q_row[r_i]):
                    return False
        elif set(c_row) != set(r_row):
            return False
    return True


def _compare_outputs(
    cand_out: Any,
    ref_out: Any,
    *,
    atol: float = DEFAULT_ATOL,
    rtol: float = DEFAULT_RTOL,
    expected_dtypes: Any = None,
    inputs: Sequence[Any] | None = None,
) -> tuple[bool, float, float]:
    """Validate shape/dtype/non-empty and compute (allclose_ok, max_abs_error, max_relative_error)."""
    if isinstance(ref_out, (tuple, list)) or isinstance(cand_out, (tuple, list)):
        if not (
            isinstance(ref_out, (tuple, list))
            and isinstance(cand_out, (tuple, list))
            and len(cand_out) == len(ref_out)
            and len(ref_out) > 0
        ):
            raise RuntimeError("Candidate and reference output structure mismatch")

        exp_list = (
            expected_dtypes
            if isinstance(expected_dtypes, list)
            and len(expected_dtypes) == len(ref_out)
            else [None] * len(ref_out)
        )

        if len(ref_out) == 2:
            dt0 = exp_list[0] or _canonical_dtype_name(
                getattr(ref_out[0], "dtype", None)
            )
            dt1 = exp_list[1] or _canonical_dtype_name(
                getattr(ref_out[1], "dtype", None)
            )
            shp0 = (
                tuple(int(d) for d in ref_out[0].shape)
                if getattr(ref_out[0], "shape", None) is not None
                else None
            )
            shp1 = (
                tuple(int(d) for d in ref_out[1].shape)
                if getattr(ref_out[1], "shape", None) is not None
                else None
            )
            if (
                dt0
                and dt0 not in _INTEGER_DTYPE_NAMES
                and dt1 in _INTEGER_DTYPE_NAMES
                and shp0 is not None
                and shp0 == shp1
            ):
                val_ok, val_abs, val_rel = _compare_outputs(
                    cand_out[0],
                    ref_out[0],
                    atol=atol,
                    rtol=rtol,
                    expected_dtypes=exp_list[0],
                )
                idx_ok = _compare_topk_indices(
                    cand_out[1],
                    ref_out[1],
                    atol=atol,
                    rtol=rtol,
                    expected_dtype=exp_list[1],
                    inputs=inputs,
                )
                return val_ok and idx_ok, val_abs, val_rel

        all_ok = True
        max_abs = 0.0
        max_rel = 0.0
        for cand_item, ref_item, exp_dt in zip(
            cand_out, ref_out, exp_list, strict=True
        ):
            item_ok, item_abs, item_rel = _compare_outputs(
                cand_item,
                ref_item,
                atol=atol,
                rtol=rtol,
                expected_dtypes=exp_dt,
                inputs=inputs,
            )
            all_ok = all_ok and item_ok
            max_abs = max(max_abs, item_abs)
            max_rel = max(max_rel, item_rel)
        return all_ok, max_abs, max_rel

    exp_dt_str = expected_dtypes if isinstance(expected_dtypes, str) else None
    _, cand_dtype = _validate_single_tensor_meta(
        cand_out, ref_out, expected_dtype=exp_dt_str
    )

    if cand_dtype in _INTEGER_DTYPE_NAMES:
        cand_ints = _tensor_flat_ints(cand_out)
        ref_ints = _tensor_flat_ints(ref_out)
        if cand_ints == ref_ints:
            return True, 0.0, 0.0
        max_diff = float(
            max(abs(cv - rv) for cv, rv in zip(cand_ints, ref_ints, strict=True))
        )
        return False, max_diff, max_diff

    if hasattr(cand_out, "_vals") and hasattr(ref_out, "_vals"):
        cand_vals = [float(v) for v in cand_out._vals]
        ref_vals = [float(v) for v in ref_out._vals]
        if len(cand_vals) != len(ref_vals) or not ref_vals:
            raise RuntimeError("Empty or mismatched tensor element buffer")
        all_ok = True
        max_abs = 0.0
        max_rel = 0.0
        for cv, rv in zip(cand_vals, ref_vals, strict=True):
            if not (math.isfinite(cv) and math.isfinite(rv)):
                return False, float("inf"), float("inf")
            diff = abs(cv - rv)
            abs_ref = abs(rv)
            if diff > max_abs:
                max_abs = diff
            if diff > atol + rtol * abs_ref:
                all_ok = False
            excess = diff - atol
            if excess > 0.0:
                rel = (excess / abs_ref) if abs_ref > 0.0 else float("inf")
                if rel > max_rel:
                    max_rel = rel
        return all_ok, max_abs, max_rel

    import torch

    cand_f = cand_out.detach().float()
    ref_f = ref_out.detach().float()
    if not bool(torch.isfinite(cand_f).all().item()) or not bool(
        torch.isfinite(ref_f).all().item()
    ):
        return False, float("inf"), float("inf")
    diff = (cand_f - ref_f).abs()
    abs_ref = ref_f.abs()
    cur_abs = float(diff.max().item())
    all_ok = bool((diff <= (atol + rtol * abs_ref)).all().item())
    excess = torch.clamp(diff - atol, min=0.0)
    over_atol = excess > 0.0
    if bool(over_atol.any().item()):
        nonzero_over = over_atol & (abs_ref > 0.0)
        zero_over = over_atol & (abs_ref == 0.0)
        if bool(zero_over.any().item()):
            cur_rel = float("inf")
        elif bool(nonzero_over.any().item()):
            cur_rel = float((excess[nonzero_over] / abs_ref[nonzero_over]).max().item())
        else:
            cur_rel = 0.0
    else:
        cur_rel = 0.0
    return all_ok, cur_abs, cur_rel


def _is_writable_dir(path_str: str | None) -> bool:
    if not path_str:
        return False
    p = Path(path_str)
    if str(p) == "/":
        return False
    try:
        return p.is_dir() and os.access(p, os.W_OK | os.X_OK)
    except OSError:
        return False


def _build_isolated_child_env(cache_root: Path | None = None) -> dict[str, str]:
    """Build sanitized subprocess environment excluding report/verified paths."""
    child_env: dict[str, str] = {}
    for key, val in os.environ.items():
        upper = key.upper()
        if "REPORT" in upper or "VERIFIED" in upper:
            continue
        child_env[key] = val
    child_env["GPUSTACK_ENGINE_SENTINEL_ENABLE"] = "0"
    pkg_root = str(Path(__file__).resolve().parents[1])
    existing_pp = child_env.get("PYTHONPATH", "")
    parts = [p for p in existing_pp.split(os.pathsep) if p] if existing_pp else []
    if pkg_root not in parts:
        parts.append(pkg_root)
    child_env["PYTHONPATH"] = os.pathsep.join(parts)

    # Non-root containers (e.g., UID 65532) default to HOME=/, causing Triton/Inductor
    # JIT compilation to fail with PermissionError on /.triton. Ensure writable cache dirs.
    base_cache = cache_root
    if base_cache is None and not _is_writable_dir(child_env.get("HOME")):
        base_cache = Path(tempfile.gettempdir())
    if base_cache is not None:
        child_env["HOME"] = str(base_cache)
        child_env.setdefault("TMPDIR", str(base_cache))
        child_env["TRITON_CACHE_DIR"] = str(base_cache / "triton")
        child_env["TORCHINDUCTOR_CACHE_DIR"] = str(base_cache / "inductor")
        child_env["XDG_CACHE_HOME"] = str(base_cache / "xdg-cache")
    return child_env


def _ordered_input_names(
    inputs_def: dict[str, Any],
    ref_fn: Callable[..., Any] | None = None,
) -> list[str]:
    """Order definition input keys by canonical reference parameter order.

    Go's encoding/json sorts map[string]TensorSpec keys alphabetically, which would
    otherwise scramble positional arguments like (x, weight) into (weight, x).
    """
    if ref_fn is not None:
        try:
            sig = inspect.signature(ref_fn)
            sig_names = [
                p.name for p in sig.parameters.values() if p.name in inputs_def
            ]
            if sig_names:
                remaining = [k for k in inputs_def if k not in sig_names]
                return sig_names + remaining
        except (TypeError, ValueError):
            pass
    return list(inputs_def.keys())


def _generate_synthetic_inputs(
    torch_mod: Any,
    definition: dict[str, Any],
    workloads: list[dict[str, Any]],
    device: str,
    *,
    ref_fn: Callable[..., Any] | None = None,
) -> list[list[Any]]:
    dtype_map = {
        "float16": getattr(torch_mod, "float16", "float16"),
        "bfloat16": getattr(torch_mod, "bfloat16", "bfloat16"),
        "float32": getattr(torch_mod, "float32", "float32"),
        "float64": getattr(torch_mod, "float64", "float64"),
        "int64": getattr(torch_mod, "int64", getattr(torch_mod, "long", "int64")),
        "long": getattr(torch_mod, "int64", getattr(torch_mod, "long", "int64")),
        "int32": getattr(torch_mod, "int32", "int32"),
    }
    axes_def = definition.get("axes") or {}
    inputs_def = definition.get("inputs") or {}
    ordered_names = _ordered_input_names(inputs_def, ref_fn)
    active_workloads = workloads or [{"axes": {}}]

    all_inputs: list[list[Any]] = []
    for wl in active_workloads:
        axis_vals: dict[str, int] = {}
        for ax_name, ax_spec in axes_def.items():
            if isinstance(ax_spec, dict) and ax_spec.get("type") == "const":
                axis_vals[ax_name] = int(ax_spec["value"])
        for ax_name, val in (wl.get("axes") or {}).items():
            axis_vals[ax_name] = int(val)

        tensors: list[Any] = []
        for in_name in ordered_names:
            in_spec = inputs_def[in_name]
            shape = [
                int(axis_vals.get(d, 1)) if isinstance(d, str) else int(d)
                for d in in_spec.get("shape", ())
            ]
            raw_dt = str(in_spec.get("dtype", "float32")).lower()
            dtype = dtype_map.get(raw_dt, dtype_map["float32"])
            if raw_dt in ("int64", "long", "int32") and callable(
                getattr(torch_mod, "zeros", None)
            ):
                tensors.append(torch_mod.zeros(shape, device=device, dtype=dtype))
            else:
                tensors.append(torch_mod.randn(shape, device=device, dtype=dtype))
        all_inputs.append(tensors)
    return all_inputs


def _make_call_timer(
    torch_mod: Any, device: str
) -> Callable[[Callable[..., Any], list[Any]], tuple[float, Any]]:
    """Capture timing primitives before any untrusted code executes."""
    perf_counter = time.perf_counter
    cuda_mod = getattr(torch_mod, "cuda", None)
    cuda_sync = getattr(cuda_mod, "synchronize", None) if device == "cuda" else None
    event_cls = getattr(cuda_mod, "Event", None) if device == "cuda" else None

    if device == "cuda" and callable(event_cls) and callable(cuda_sync):
        start_ev = event_cls(enable_timing=True)
        end_ev = event_cls(enable_timing=True)
        rec_start = start_ev.record
        rec_end = end_ev.record
        sync_end = end_ev.synchronize
        elapsed_fn = start_ev.elapsed_time

        def _time_cuda(fn: Callable[..., Any], args: list[Any]) -> tuple[float, Any]:
            cuda_sync()
            rec_start()
            out = fn(*args)
            rec_end()
            sync_end()
            return float(elapsed_fn(end_ev)), out

        return _time_cuda

    def _time_cpu(fn: Callable[..., Any], args: list[Any]) -> tuple[float, Any]:
        if callable(cuda_sync):
            cuda_sync()
        t0 = perf_counter()
        out = fn(*args)
        if callable(cuda_sync):
            cuda_sync()
        return float((perf_counter() - t0) * 1000.0), out

    return _time_cpu


def _run_isolated_candidate_subprocess(
    *,
    lib_path: Path,
    entrypoint: str,
    num_workloads: int,
    child_cwd: Path,
    child_cache_dir: Path | None = None,
    warmup_iters: int,
    bench_iters: int,
    prepare_warmup_step: Callable[[int, int], tuple[Path, Callable[[], None]]],
    prepare_bench_attempt: Callable[
        [int, int, int], tuple[list[Path], Path, Callable[[int], float]]
    ],
    prepare_post_bench_verify: Callable[[int], tuple[Path, Path]],
    cuda_sync: Callable[[], Any] | None = None,
    timeout_seconds: float = DEFAULT_SUBPROCESS_TIMEOUT_SECONDS,
) -> tuple[list[list[float]], list[list[float]]]:
    """Run candidate callable in an isolated session with stdout=/dev/null and SIGKILL cleanup."""
    child_env = _build_isolated_child_env(cache_root=child_cache_dir)
    perf_counter = time.perf_counter
    deadline = time.monotonic() + float(timeout_seconds)

    cmd_r, cmd_w = os.pipe()
    ack_r, ack_w = os.pipe()
    cmd_r_open = True
    cmd_w_open = True
    ack_r_open = True
    ack_w_open = True
    proc: subprocess.Popen[bytes] | None = None
    stderr_bytes = b""

    try:
        proc = subprocess.Popen(
            [
                sys.executable,
                "-B",
                "-m",
                "gpustack_engine_sentinel.forge_verifier",
                "--_eval-candidate-fd",
                str(cmd_r),
                str(ack_w),
            ],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.PIPE,
            cwd=str(child_cwd),
            env=child_env,
            start_new_session=True,
            pass_fds=(cmd_r, ack_w),
        )
        os.close(cmd_r)
        cmd_r_open = False
        os.close(ack_w)
        ack_w_open = False

        reader = _PipeLineReader(ack_r)

        def _rpc(msg: dict[str, Any]) -> dict[str, Any]:
            req_id = secrets.token_hex(16)
            wire_msg = dict(msg)
            wire_msg["req_id"] = req_id
            _write_json_line(cmd_w, wire_msg)
            line = reader.read_line(deadline)
            if not line:
                raise RuntimeError(
                    "RUNTIME_ERROR: candidate subprocess exited prematurely without completing evaluation"
                )
            try:
                resp = json.loads(line)
            except json.JSONDecodeError as exc:
                raise RuntimeError(
                    f"RUNTIME_ERROR: candidate subprocess returned malformed ack: {exc}"
                ) from exc
            if (
                not isinstance(resp, dict)
                or resp.get("ok") is not True
                or resp.get("req_id") != req_id
            ):
                raise RuntimeError(
                    f"RUNTIME_ERROR: candidate subprocess ack mismatch or failure: {resp!r}"
                )
            return resp

        init_resp = _rpc(
            {
                "library_path": str(lib_path),
                "entrypoint": entrypoint,
            }
        )
        if init_resp.get("event") != "loaded":
            raise RuntimeError(
                "RUNTIME_ERROR: candidate subprocess failed initialization handshake"
            )

        ref_samples_by_wl: list[list[float]] = []
        cand_samples_by_wl: list[list[float]] = []

        for wl_idx in range(num_workloads):
            for w_iter in range(warmup_iters):
                warm_in_path, run_ref_warm = prepare_warmup_step(wl_idx, w_iter)
                if w_iter % 2 == 0:
                    run_ref_warm()
                    _rpc(
                        {
                            "op": "warmup",
                            "workload": wl_idx,
                            "input_path": str(warm_in_path),
                        }
                    )
                else:
                    _rpc(
                        {
                            "op": "warmup",
                            "workload": wl_idx,
                            "input_path": str(warm_in_path),
                        }
                    )
                    run_ref_warm()

            wl_ref: list[float] = []
            wl_cand: list[float] = []
            bench_out_path: Path | None = None
            for attempt in range(2):
                capture_iter = secrets.randbelow(bench_iters)
                bench_in_paths, cur_bench_out_path, run_ref_bench_iter = (
                    prepare_bench_attempt(wl_idx, attempt, capture_iter)
                )
                if attempt == 0:
                    wl_ref = [
                        float(run_ref_bench_iter(b_iter))
                        for b_iter in range(bench_iters)
                    ]

                t_ping0 = perf_counter()
                _rpc({"op": "ping"})
                ping_ms = (perf_counter() - t_ping0) * 1000.0

                _rpc(
                    {
                        "op": "prepare_bench",
                        "workload": wl_idx,
                        "input_paths": [str(p) for p in bench_in_paths],
                    }
                )
                if callable(cuda_sync):
                    cuda_sync()
                t_bench0 = perf_counter()
                bench_resp = _rpc(
                    {
                        "op": "bench",
                        "workload": wl_idx,
                        "iters": bench_iters,
                        "capture_iter": capture_iter,
                    }
                )
                if callable(cuda_sync):
                    cuda_sync()
                parent_wall_ms = (perf_counter() - t_bench0) * 1000.0

                raw_samples = bench_resp.get("samples")
                if not isinstance(raw_samples, list) or len(raw_samples) != bench_iters:
                    raise RuntimeError(
                        "RUNTIME_ERROR: candidate subprocess returned invalid benchmark sample list"
                    )
                parsed_samples: list[float] = []
                for s in raw_samples:
                    if (
                        isinstance(s, bool)
                        or not isinstance(s, (int, float))
                        or not math.isfinite(float(s))
                        or float(s) <= 0.0
                    ):
                        raise RuntimeError(
                            f"RUNTIME_ERROR: candidate subprocess returned invalid timing sample: {s!r}"
                        )
                    parsed_samples.append(float(s))

                child_sum_ms = sum(parsed_samples)
                if child_sum_ms > parent_wall_ms * 2.5 + 25.0:
                    raise RuntimeError(
                        "RUNTIME_ERROR: candidate reported timing exceeds parent wall-clock"
                    )
                ipc_allowance_ms = max(5.0, ping_ms * 4.0)
                min_plausible_ms = max(0.0, parent_wall_ms - ipc_allowance_ms)
                if min_plausible_ms > 0.0 and child_sum_ms < min_plausible_ms * 0.5:
                    if attempt == 0:
                        continue
                    raise RuntimeError(
                        "RUNTIME_ERROR: candidate reported timing inconsistent with parent wall-clock"
                    )
                wl_cand = parsed_samples
                bench_out_path = cur_bench_out_path
                break

            if bench_out_path is None:
                raise RuntimeError("Missing captured benchmark output path")
            _rpc(
                {
                    "op": "save_captured",
                    "workload": wl_idx,
                    "output_path": str(bench_out_path),
                }
            )

            # Deliver fresh unseen random inputs only after timed benchmarking completes.
            unseen_in_path, unseen_out_path = prepare_post_bench_verify(wl_idx)
            _rpc(
                {
                    "op": "verify_unseen",
                    "workload": wl_idx,
                    "input_path": str(unseen_in_path),
                    "output_path": str(unseen_out_path),
                }
            )

            ref_samples_by_wl.append(wl_ref)
            cand_samples_by_wl.append(wl_cand)

        quit_req_id = secrets.token_hex(16)
        _write_json_line(cmd_w, {"op": "quit", "req_id": quit_req_id})
        os.close(cmd_w)
        cmd_w_open = False

        remaining = max(0.1, deadline - time.monotonic())
        proc.wait(timeout=remaining)
        if proc.stderr is not None:
            stderr_bytes = proc.stderr.read()
        if proc.returncode != 0:
            detail = stderr_bytes.decode("utf-8", errors="replace").strip()
            raise RuntimeError(
                f"Candidate evaluation subprocess failed: {detail or f'exit code {proc.returncode}'}"
            )
        return ref_samples_by_wl, cand_samples_by_wl
    except Exception as exc:
        if cmd_w_open:
            with contextlib.suppress(OSError):
                os.close(cmd_w)
            cmd_w_open = False
        if proc is not None and proc.stderr is not None and not stderr_bytes:
            with contextlib.suppress(Exception):
                if proc.poll() is None:
                    with contextlib.suppress(subprocess.TimeoutExpired):
                        proc.wait(timeout=2.0)
                if proc.poll() is not None:
                    stderr_bytes = proc.stderr.read()
        detail = stderr_bytes.decode("utf-8", errors="replace").strip()
        if detail and detail not in str(exc):
            raise RuntimeError(f"{exc} ({detail})") from exc
        raise
    finally:
        for is_open, fd in (
            (cmd_r_open, cmd_r),
            (cmd_w_open, cmd_w),
            (ack_r_open, ack_r),
            (ack_w_open, ack_w),
        ):
            if is_open:
                with contextlib.suppress(OSError):
                    os.close(fd)
        if proc is not None:
            with contextlib.suppress(ProcessLookupError, PermissionError, OSError):
                os.killpg(proc.pid, signal.SIGKILL)
            with contextlib.suppress(Exception):
                proc.wait(timeout=5)
            if proc.stderr is not None:
                with contextlib.suppress(OSError):
                    proc.stderr.close()


def _resolve_trusted_reference_callable(
    torch_mod: Any,
    op_family: str,
    definition: dict[str, Any],
) -> Callable[..., Any]:
    ref_code = _OP_REFERENCES.get(op_family)
    if not ref_code:
        raise RuntimeError(
            f"Unsupported op_family {op_family!r} in built-in reference table"
        )
    ref_ns: dict[str, Any] = {"torch": torch_mod}
    exec(ref_code, ref_ns)
    raw_fn = ref_ns.get("run")
    if not callable(raw_fn):
        raise RuntimeError(
            f"Built-in reference for {op_family!r} does not define callable run(...)"
        )

    axes = definition.get("axes")
    top_k_spec = axes.get("top_k") if isinstance(axes, dict) else None
    if (
        isinstance(top_k_spec, dict)
        and top_k_spec.get("type") == "const"
        and top_k_spec.get("value") is not None
        and "top_k" in inspect.signature(raw_fn).parameters
    ):
        top_k_val = int(top_k_spec["value"])

        def _bound_ref(*args: Any) -> Any:
            return raw_fn(*args, top_k=top_k_val)

        _bound_ref.__wrapped__ = raw_fn  # type: ignore[attr-defined]
        return _bound_ref
    return raw_fn


def _evaluate_reference_fp32(
    torch_mod: Any,
    ref_fn: Callable[..., Any],
    inputs: list[Any],
    cuda_sync: Callable[[], Any] | None,
) -> tuple[Any, Any]:
    orig_out = ref_fn(*_clone_tensors(torch_mod, inputs))
    if callable(cuda_sync):
        cuda_sync()
    expected_dtypes = _extract_dtype_tree(orig_out)

    try:
        fp32_inputs = _upcast_float_inputs_to_fp32(torch_mod, inputs)
        fp32_out = ref_fn(*fp32_inputs)
        if callable(cuda_sync):
            cuda_sync()
        return fp32_out, expected_dtypes
    except (TypeError, RuntimeError):
        return orig_out, expected_dtypes


def _evaluate_entry_with_isolated_candidate(
    *,
    lib_path: Path,
    entrypoint: str,
    op_family: str,
    definition: dict[str, Any],
    workloads: list[dict[str, Any]],
    thresholds: VerificationThresholds,
    warmup_iters: int,
    bench_iters: int,
) -> dict[str, Any]:
    """Generate inputs & reference outputs in trusted process, run child, and verify outputs."""
    import torch

    ref_fn = _resolve_trusted_reference_callable(torch, op_family, definition)

    device = "cuda" if bool(torch.cuda.is_available()) else "cpu"
    cuda_sync = getattr(torch.cuda, "synchronize", None) if device == "cuda" else None
    time_call = _make_call_timer(torch, device)

    effective_workloads = list(workloads) if workloads else [{"axes": {}}]

    with tempfile.TemporaryDirectory(prefix="gpustack-verify-") as tmp_dir_str:
        tmp_root = Path(tmp_dir_str)
        io_dir = tmp_root / "io"
        child_cwd = tmp_root / "cwd"
        child_cache = tmp_root / "cache"
        io_dir.mkdir(mode=0o700)
        child_cwd.mkdir(mode=0o700)
        child_cache.mkdir(mode=0o700)

        captured_bench_checks: dict[int, tuple[Path, list[Any]]] = {}
        unseen_verify_checks: dict[int, tuple[Path, list[Any]]] = {}

        def _prepare_warmup_step(
            wl_idx: int, w_iter: int
        ) -> tuple[Path, Callable[[], None]]:
            warm_in = _generate_synthetic_inputs(
                torch, definition, [effective_workloads[wl_idx]], device, ref_fn=ref_fn
            )[0]
            warm_path = io_dir / f"workload_{wl_idx}_warm_{w_iter}_in.bin"
            _save_tensor_payload(torch, warm_in, warm_path)

            def _run_ref_warm() -> None:
                ref_fn(*_clone_tensors(torch, warm_in))
                if callable(cuda_sync):
                    cuda_sync()

            return warm_path, _run_ref_warm

        def _prepare_bench_attempt(
            wl_idx: int, attempt: int, capture_iter: int
        ) -> tuple[list[Path], Path, Callable[[int], float]]:
            wl_spec = [effective_workloads[wl_idx]]
            bench_inputs: list[list[Any]] = []
            bench_paths: list[Path] = []
            for b_iter in range(bench_iters):
                b_in = _generate_synthetic_inputs(
                    torch, definition, wl_spec, device, ref_fn=ref_fn
                )[0]
                b_path = io_dir / f"workload_{wl_idx}_bench_{attempt}_{b_iter}_in.bin"
                _save_tensor_payload(torch, b_in, b_path)
                bench_inputs.append(b_in)
                bench_paths.append(b_path)

            out_path = io_dir / f"workload_{wl_idx}_bench_{attempt}_out.bin"
            captured_bench_checks[wl_idx] = (out_path, bench_inputs[capture_iter])

            def _run_ref_bench_iter(b_iter: int) -> float:
                cloned = _clone_tensors(torch, bench_inputs[b_iter])
                ms, _ = time_call(ref_fn, cloned)
                return ms

            return bench_paths, out_path, _run_ref_bench_iter

        def _prepare_post_bench_verify(wl_idx: int) -> tuple[Path, Path]:
            unseen_in = _generate_synthetic_inputs(
                torch, definition, [effective_workloads[wl_idx]], device, ref_fn=ref_fn
            )[0]
            in_path = io_dir / f"workload_{wl_idx}_unseen_in.bin"
            out_path = io_dir / f"workload_{wl_idx}_unseen_out.bin"
            _save_tensor_payload(torch, unseen_in, in_path)
            unseen_verify_checks[wl_idx] = (out_path, unseen_in)
            return in_path, out_path

        ref_samples_by_wl, cand_samples_by_wl = _run_isolated_candidate_subprocess(
            lib_path=lib_path,
            entrypoint=entrypoint,
            num_workloads=len(effective_workloads),
            child_cwd=child_cwd,
            child_cache_dir=child_cache,
            warmup_iters=warmup_iters,
            bench_iters=bench_iters,
            prepare_warmup_step=_prepare_warmup_step,
            prepare_bench_attempt=_prepare_bench_attempt,
            prepare_post_bench_verify=_prepare_post_bench_verify,
            cuda_sync=cuda_sync,
        )

        all_ok = True
        max_abs = 0.0
        max_rel = 0.0
        for wl_idx in range(len(effective_workloads)):
            for out_path, wl_in in (
                captured_bench_checks[wl_idx],
                unseen_verify_checks[wl_idx],
            ):
                ref_out, exp_dt = _evaluate_reference_fp32(
                    torch, ref_fn, wl_in, cuda_sync
                )
                cand_out = _load_tensor_payload(
                    torch, out_path, expected_dir=io_dir, device="cpu"
                )
                ref_cpu = _detach_to_cpu(ref_out)
                in_cpu = [_detach_to_cpu(t) for t in wl_in]
                wl_ok, wl_abs, wl_rel = _compare_outputs(
                    cand_out,
                    ref_cpu,
                    atol=thresholds.atol,
                    rtol=thresholds.rtol,
                    expected_dtypes=exp_dt,
                    inputs=in_cpu,
                )
                all_ok = all_ok and wl_ok
                max_abs = max(max_abs, wl_abs)
                max_rel = max(max_rel, wl_rel)

    if not (math.isfinite(max_abs) and math.isfinite(max_rel)):
        raise RuntimeError("Candidate output contains non-finite values (NaN or Inf)")

    ref_medians = [float(statistics.median(s)) for s in ref_samples_by_wl]
    cand_medians = [float(statistics.median(s)) for s in cand_samples_by_wl]
    n_wl = max(1, len(cand_medians))
    avg_ref_ms = sum(ref_medians) / n_wl
    avg_cand_ms = sum(cand_medians) / n_wl
    speedup = (avg_ref_ms / avg_cand_ms) if avg_cand_ms > 0.0 else 0.0

    return {
        "status": "PASSED",
        "correctness": {
            "allclose_passed": all_ok,
            "max_abs_error": max_abs,
            "max_relative_error": max_rel,
        },
        "performance": {
            "speedup_factor": speedup,
            "latency_ms": avg_cand_ms,
            "reference_latency_ms": avg_ref_ms,
        },
    }


def _run_trusted_driver_subprocess(payload: dict[str, Any]) -> dict[str, Any]:
    """Run trusted reference/orchestrator in a clean subprocess when torch lives in PYTHONPATH."""
    child_env = _build_isolated_child_env()
    cmd_r, cmd_w = os.pipe()
    res_r, res_w = os.pipe()
    cmd_r_open = True
    cmd_w_open = True
    res_r_open = True
    res_w_open = True
    proc: subprocess.Popen[bytes] | None = None
    stderr_bytes = b""

    try:
        proc = subprocess.Popen(
            [
                sys.executable,
                "-B",
                "-m",
                "gpustack_engine_sentinel.forge_verifier",
                "--_eval-trusted-fd",
                str(cmd_r),
                str(res_w),
            ],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.PIPE,
            env=child_env,
            pass_fds=(cmd_r, res_w),
        )
        os.close(cmd_r)
        cmd_r_open = False
        os.close(res_w)
        res_w_open = False

        _write_json_line(cmd_w, payload)
        os.close(cmd_w)
        cmd_w_open = False

        deadline = time.monotonic() + float(DEFAULT_SUBPROCESS_TIMEOUT_SECONDS)
        reader = _PipeLineReader(res_r)
        line = reader.read_line(deadline)
        proc.wait(timeout=max(0.1, deadline - time.monotonic()))
        if proc.stderr is not None:
            stderr_bytes = proc.stderr.read()
        if proc.returncode != 0 or not line:
            detail = stderr_bytes.decode("utf-8", errors="replace").strip()
            raise RuntimeError(
                detail or f"Trusted verifier worker exited with code {proc.returncode}"
            )
        parsed = json.loads(line)
        if not isinstance(parsed, dict):
            raise RuntimeError("Trusted verifier worker returned non-object evaluation")
        return parsed
    finally:
        for is_open, fd in (
            (cmd_r_open, cmd_r),
            (cmd_w_open, cmd_w),
            (res_r_open, res_r),
            (res_w_open, res_w),
        ):
            if is_open:
                with contextlib.suppress(OSError):
                    os.close(fd)
        if proc is not None:
            if proc.poll() is None:
                with contextlib.suppress(OSError):
                    proc.kill()
            with contextlib.suppress(Exception):
                proc.wait(timeout=5)
            if proc.stderr is not None:
                with contextlib.suppress(OSError):
                    proc.stderr.close()


def _default_live_gpu_executor(
    entry: dict[str, Any],
    definition: dict[str, Any],
    workloads: list[dict[str, Any]],
    solution: dict[str, Any],
    output_dir: Path,
    *,
    thresholds: VerificationThresholds | None = None,
    warmup_iters: int | None = None,
    bench_iters: int | None = None,
) -> dict[str, Any]:
    """Execute untrusted candidate inside an isolated child subprocess and verify in parent."""
    del solution
    parsed_key = parse_local_kernel_key(str(entry.get("localKernelKey") or ""))
    op_family = parsed_key["op_family"]
    if op_family not in _OP_REFERENCES:
        raise ValueError(
            f"Unsupported opFamily {op_family!r} in trusted reference registry"
        )
    lib_path = _verify_regular_file(output_dir, str(entry["libraryPath"]))
    entrypoint = str(entry.get("entrypoint") or "run").strip() or "run"
    eff_thresholds = thresholds or VerificationThresholds()
    eff_warmup, eff_bench = _resolve_iter_counts(warmup_iters, bench_iters)

    if importlib.util.find_spec("torch") is not None:
        return _evaluate_entry_with_isolated_candidate(
            lib_path=lib_path,
            entrypoint=entrypoint,
            op_family=op_family,
            definition=definition,
            workloads=workloads,
            thresholds=eff_thresholds,
            warmup_iters=eff_warmup,
            bench_iters=eff_bench,
        )

    return _run_trusted_driver_subprocess(
        {
            "library_path": str(lib_path),
            "entrypoint": entrypoint,
            "op_family": op_family,
            "definition": definition,
            "workloads": workloads,
            "thresholds": {
                "min_speedup_ratio": eff_thresholds.min_speedup_ratio,
                "atol": eff_thresholds.atol,
                "rtol": eff_thresholds.rtol,
            },
            "warmup_iters": eff_warmup,
            "bench_iters": eff_bench,
        }
    )


def _sanitize_json_value(obj: Any) -> Any:
    """Replace non-finite floats with None so report serialization stays RFC 8259 JSON."""
    if isinstance(obj, float):
        return obj if math.isfinite(obj) else None
    if isinstance(obj, dict):
        return {str(k): _sanitize_json_value(v) for k, v in obj.items()}
    if isinstance(obj, (list, tuple)):
        return [_sanitize_json_value(x) for x in obj]
    return obj


def _parse_eval_numeric_field(
    container: dict[str, Any],
    field_name: str,
) -> tuple[bool, float]:
    if field_name not in container:
        return False, 0.0
    raw = container[field_name]
    if isinstance(raw, bool) or not isinstance(raw, (int, float)):
        return False, 0.0
    return True, float(raw)


def _evaluate_candidate_verdict(
    evaluation: dict[str, Any],
    thresholds: VerificationThresholds,
) -> tuple[bool, str]:
    """Assert status == PASSED, numerical error bounds, and finite performance metrics."""
    if not isinstance(evaluation, dict):
        return False, "evaluation payload must be an object"
    status = str(evaluation.get("status") or "")
    if status != "PASSED":
        return False, f"evaluation status is {status!r}, expected 'PASSED'"

    correctness = evaluation.get("correctness")
    if not isinstance(correctness, dict):
        return False, "missing evaluation.correctness"
    ok_abs, max_abs = _parse_eval_numeric_field(correctness, "max_abs_error")
    ok_rel, max_rel = _parse_eval_numeric_field(correctness, "max_relative_error")
    if not ok_abs or not ok_rel:
        return False, "invalid max_abs_error or max_relative_error"

    if not math.isfinite(max_abs) or max_abs < 0.0:
        return False, f"non-finite or negative max_abs_error: {max_abs}"
    if not math.isfinite(max_rel) or max_rel < 0.0:
        return False, f"non-finite or negative max_relative_error: {max_rel}"

    if "allclose_passed" in correctness and correctness["allclose_passed"] is not None:
        allclose_passed = correctness["allclose_passed"]
        if not isinstance(allclose_passed, bool):
            return False, "invalid allclose_passed"
        if not allclose_passed:
            return (
                False,
                f"element-wise allclose check failed (max_abs_error={max_abs}, max_relative_error={max_rel})",
            )
    else:
        if correctness.get("allclose") is False:
            return False, "element-wise allclose check failed"
        if max_abs > thresholds.atol and max_rel > thresholds.rtol:
            return (
                False,
                f"max_abs_error {max_abs} exceeds atol {thresholds.atol} and max_relative_error {max_rel} exceeds rtol {thresholds.rtol}",
            )

    performance = evaluation.get("performance")
    if not isinstance(performance, dict):
        return False, "missing evaluation.performance"
    ok_speedup, speedup = _parse_eval_numeric_field(performance, "speedup_factor")
    if not ok_speedup:
        return False, "invalid speedup_factor"

    if not math.isfinite(speedup) or speedup < thresholds.min_speedup_ratio:
        return (
            False,
            f"speedup_factor {speedup} below minSpeedupRatio {thresholds.min_speedup_ratio}",
        )

    for latency_field in ("latency_ms", "reference_latency_ms"):
        if latency_field in performance:
            ok_lat, lat_val = _parse_eval_numeric_field(performance, latency_field)
            if not ok_lat:
                return False, f"invalid {latency_field}"
            if not math.isfinite(lat_val) or lat_val < 0.0:
                return False, f"non-finite or negative {latency_field}: {lat_val}"

    return True, "ok"


def _unlink_report_if_exists(report_path: Path | str | None) -> None:
    if report_path is None:
        return
    target = Path(report_path)
    if target.is_symlink() or target.exists():
        with contextlib.suppress(OSError):
            target.unlink()


def _write_report_atomic(report_path: Path, report: dict[str, Any]) -> None:
    report_path.parent.mkdir(parents=True, exist_ok=True)
    _unlink_report_if_exists(report_path)
    tmp_path = report_path.parent / f".{report_path.name}.tmp.{os.getpid()}"
    tmp_path.write_text(
        json.dumps(report, indent=2, sort_keys=True, allow_nan=False) + "\n",
        encoding="utf-8",
    )
    os.replace(tmp_path, report_path)


def _validate_compatibility_match(
    *,
    manifest_compat: dict[str, Any],
    request_compat: dict[str, Any],
) -> None:
    req_engine = str(request_compat.get("engine") or "").strip()
    if req_engine:
        man_engine = str(manifest_compat.get("engine") or "").strip()
        if not man_engine or (
            canonical_engine_name(man_engine).lower()
            != canonical_engine_name(req_engine).lower()
        ):
            raise ValueError(
                f"manifest compatibility.engine {man_engine!r} does not match request {req_engine!r}"
            )

    raw_man_ver = manifest_compat.get("engineVersion")
    man_ver = ""
    if raw_man_ver is not None:
        if not isinstance(raw_man_ver, str):
            raise ValueError(
                f"manifest compatibility.engineVersion must be a string, got {raw_man_ver!r}"
            )
        man_ver = raw_man_ver.strip()
        if man_ver:
            normalize_engine_version(man_ver)

    raw_req_ver = request_compat.get("engineVersion")
    if raw_req_ver is not None:
        if not isinstance(raw_req_ver, str):
            raise ValueError(
                f"request compatibility.engineVersion must be a string, got {raw_req_ver!r}"
            )
        req_ver = raw_req_ver.strip()
        if req_ver and (
            not man_ver
            or normalize_engine_version(man_ver) != normalize_engine_version(req_ver)
        ):
            raise ValueError(
                f"manifest compatibility.engineVersion {man_ver!r} does not match request {req_ver!r}"
            )

    req_mfr = str(request_compat.get("manufacturer") or "").strip().lower()
    if req_mfr:
        man_mfr = str(manifest_compat.get("manufacturer") or "").strip().lower()
        if man_mfr != req_mfr:
            raise ValueError(
                f"manifest compatibility.manufacturer {man_mfr!r} does not match request {req_mfr!r}"
            )

    req_arch = str(request_compat.get("architecture") or "").strip()
    if req_arch:
        man_arch = str(manifest_compat.get("architecture") or "").strip()
        eff_mfr = (
            req_mfr or str(manifest_compat.get("manufacturer") or "").strip().lower()
        )
        if not man_arch or not arch_compatible(eff_mfr, man_arch, req_arch):
            raise ValueError(
                f"manifest compatibility.architecture {man_arch!r} is incompatible with request {req_arch!r}"
            )


def _validate_key_against_compatibility(
    parsed_key: dict[str, Any],
    compat: dict[str, Any],
) -> None:
    man_engine = str(compat.get("engine") or "").strip()
    if man_engine and (
        canonical_engine_name(man_engine).lower()
        != canonical_engine_name(str(parsed_key["engine"])).lower()
    ):
        raise ValueError(
            f"Entry key engine {parsed_key['engine']!r} does not match manifest compatibility {man_engine!r}"
        )

    raw_man_ver = compat.get("engineVersion")
    if raw_man_ver is not None:
        if not isinstance(raw_man_ver, str):
            raise ValueError(
                f"manifest compatibility.engineVersion must be a string, got {raw_man_ver!r}"
            )
        man_ver = raw_man_ver.strip()
        if man_ver and (
            normalize_engine_version(man_ver)
            != normalize_engine_version(str(parsed_key["engine_version"]))
        ):
            raise ValueError(
                f"Entry key engineVersion {parsed_key['engine_version']!r} does not match manifest compatibility {man_ver!r}"
            )

    man_mfr = str(compat.get("manufacturer") or "").strip().lower()
    if man_mfr and man_mfr != str(parsed_key["manufacturer"]).strip().lower():
        raise ValueError(
            f"Entry key manufacturer {parsed_key['manufacturer']!r} does not match manifest compatibility {man_mfr!r}"
        )

    man_arch = str(compat.get("architecture") or "").strip()
    if man_arch:
        eff_mfr = man_mfr or str(parsed_key["manufacturer"]).strip().lower()
        if not arch_compatible(eff_mfr, man_arch, str(parsed_key["architecture"])):
            raise ValueError(
                f"Entry key architecture {parsed_key['architecture']!r} is incompatible with manifest compatibility {man_arch!r}"
            )


def verify_forge_output(
    *,
    input_dir: Path | str = DEFAULT_INPUT_DIR,
    output_dir: Path | str = DEFAULT_OUTPUT_DIR,
    report_path: Path | str | None = DEFAULT_REPORT_PATH,
    min_speedup_ratio: float | None = None,
    atol: float | None = None,
    rtol: float | None = None,
    warmup_iters: int | None = None,
    bench_iters: int | None = None,
    executor: CandidateExecutor | None = None,
) -> VerificationResult:
    """Validate dispatch-manifest.json, execute candidates against reference/workloads, and write report."""
    _unlink_report_if_exists(report_path)
    in_root = Path(input_dir)
    out_root = Path(output_dir)

    def _finalize(result: VerificationResult) -> VerificationResult:
        if report_path is not None:
            _write_report_atomic(Path(report_path), result.to_dict())
        return result

    try:
        forge_request = _load_forge_request(in_root)
        thresholds = _resolve_thresholds(
            forge_request,
            min_speedup_ratio=min_speedup_ratio,
            atol=atol,
            rtol=rtol,
        )
    except Exception as exc:
        return _finalize(
            VerificationResult(
                overall_status="FAILED",
                dispatch_manifest_digest="",
                passed_count=0,
                failed_count=0,
                thresholds=VerificationThresholds(
                    min_speedup_ratio=_safe_fallback_threshold(
                        min_speedup_ratio, DEFAULT_MIN_SPEEDUP_RATIO
                    ),
                    atol=_safe_fallback_threshold(atol, DEFAULT_ATOL),
                    rtol=_safe_fallback_threshold(rtol, DEFAULT_RTOL),
                ),
                error=f"Invalid forge-request.json: {exc}",
            )
        )

    try:
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

        manifest_compat = manifest.get("compatibility")
        if not isinstance(manifest_compat, dict):
            return _finalize(
                VerificationResult(
                    overall_status="FAILED",
                    dispatch_manifest_digest=manifest_digest,
                    passed_count=0,
                    failed_count=0,
                    thresholds=thresholds,
                    error="dispatch-manifest.json compatibility must be an object",
                )
            )

        requested_keys: set[str] = set()
        if forge_request is not None:
            try:
                _validate_compatibility_match(
                    manifest_compat=manifest_compat,
                    request_compat=forge_request,
                )
                raw_target_keys = forge_request.get("targetKernelKeys")
                if raw_target_keys is not None:
                    if not isinstance(raw_target_keys, list):
                        raise ValueError(
                            "forge-request.json targetKernelKeys must be a list"
                        )
                    requested_keys = {
                        str(k).strip()
                        for k in raw_target_keys
                        if isinstance(k, str) and str(k).strip()
                    }
            except Exception as exc:
                return _finalize(
                    VerificationResult(
                        overall_status="FAILED",
                        dispatch_manifest_digest=manifest_digest,
                        passed_count=0,
                        failed_count=0,
                        thresholds=thresholds,
                        error=f"Forge request binding validation failed: {exc}",
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
            manifest.get("checksums")
            if isinstance(manifest.get("checksums"), dict)
            else {}
        )
        definitions = _load_definitions(in_root)
        workloads_by_def = _load_workloads(in_root)

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
                if (
                    parsed_key["op_family"] not in ALLOWED_OP_FAMILIES
                    or parsed_key["op_family"] not in _OP_REFERENCES
                    or is_excluded_operator(parsed_key["op_family"])
                ):
                    raise ValueError(f"Excluded opFamily: {parsed_key['op_family']}")

                _validate_key_against_compatibility(parsed_key, manifest_compat)

                if requested_keys and raw_key not in requested_keys:
                    raise ValueError(
                        f"Entry localKernelKey {raw_key!r} is not in forge-request.json targetKernelKeys"
                    )

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

                definition_obj = (
                    definitions.get(def_name) or definitions.get(raw_key) or {}
                )
                if definitions and not definition_obj:
                    raise ValueError(
                        f"Definition {def_name or raw_key!r} not found in forge input definitions"
                    )
                if isinstance(definition_obj.get("local_kernel_key"), str):
                    def_key = definition_obj["local_kernel_key"].strip()
                    if def_key and def_key != raw_key:
                        raise ValueError(
                            f"Definition local_kernel_key {def_key!r} does not match entry localKernelKey {raw_key!r}"
                        )
                if isinstance(definition_obj.get("op_type"), str):
                    def_op = definition_obj["op_type"].strip()
                    if def_op and def_op != parsed_key["op_family"]:
                        raise ValueError(
                            f"Definition op_type {def_op!r} does not match entry op_family {parsed_key['op_family']!r}"
                        )

                held_out = (
                    workloads_by_def.get(def_name)
                    or workloads_by_def.get(raw_key)
                    or []
                )
                if executor is not None:
                    evaluation = executor(
                        entry, definition_obj, held_out, solution_obj, out_root
                    )
                else:
                    evaluation = _default_live_gpu_executor(
                        entry,
                        definition_obj,
                        held_out,
                        solution_obj,
                        out_root,
                        thresholds=thresholds,
                        warmup_iters=warmup_iters,
                        bench_iters=bench_iters,
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
                        "evaluation": _sanitize_json_value(evaluation),
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

        overall = "PASSED" if passed_count > 0 and failed_count == 0 else "FAILED"
        if overall == "PASSED":
            error_msg = None
        elif passed_count == 0:
            error_msg = "Zero candidate kernels passed verification"
        else:
            error_msg = (
                f"{failed_count} of {passed_count + failed_count} "
                "candidate kernel(s) failed verification"
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
    except BaseException:
        _unlink_report_if_exists(report_path)
        raise


def _load_candidate_entrypoint(
    library_path: Path,
    entrypoint: str,
) -> Callable[..., Any]:
    """Load candidate module before any workload tensors exist on stack."""
    mod_name = f"_gpustack_verify_{hashlib.sha256(str(library_path).encode('utf-8')).hexdigest()[:16]}"
    source_bytes = library_path.read_bytes()
    code_obj = compile(source_bytes, str(library_path), "exec")
    mod = types.ModuleType(mod_name)
    mod.__file__ = str(library_path)
    sys.modules[mod_name] = mod
    del source_bytes, mod_name
    with contextlib.redirect_stdout(sys.stderr):
        exec(code_obj, mod.__dict__)
    cand_fn = getattr(mod, entrypoint, None)
    if not callable(cand_fn):
        raise RuntimeError(f"Candidate entrypoint {entrypoint!r} is not callable")
    return cand_fn


def _execute_timed_batches(
    timer: Callable[[Callable[..., Any], list[Any]], tuple[float, Any]],
    cand_fn: Callable[..., Any],
    batches: list[list[Any]],
    capture_iter: int,
    torch_mod: Any,
) -> tuple[list[float], Any]:
    samples: list[float] = []
    captured_out: Any = None
    for b_iter, batch_args in enumerate(batches):
        ms, out = timer(cand_fn, batch_args)
        if out is None:
            raise RuntimeError(
                "RUNTIME_ERROR: candidate returned None during benchmark"
            )
        samples.append(ms)
        if b_iter == capture_iter:
            captured_out = _clone_output_tree(torch_mod, out)
    return samples, captured_out


def _prepare_child_rpc_session(
    raw_fds: tuple[int, int],
) -> tuple[Path, str, Callable[[Callable[..., Any]], None], Callable[[], None]]:
    """Capture trusted timing and pipe state in a closure isolated from module import."""
    in_fd, out_fd = raw_fds
    os_write = os.write
    reader = _PipeLineReader(in_fd)

    def _close_pipes() -> None:
        with contextlib.suppress(OSError):
            os.close(in_fd)
        with contextlib.suppress(OSError):
            os.close(out_fd)

    try:
        import torch

        device = "cuda" if bool(torch.cuda.is_available()) else "cpu"
        timer = _make_call_timer(torch, device)
        cuda_sync = (
            getattr(torch.cuda, "synchronize", None) if device == "cuda" else None
        )

        init_line = reader.read_line()
        if not init_line:
            raise RuntimeError("Missing candidate child initialization payload")
        init_spec = json.loads(init_line)
        init_req_id = str(init_spec["req_id"])
        lib_path = Path(str(init_spec["library_path"]))
        entrypoint = str(init_spec.get("entrypoint") or "run").strip() or "run"
    except BaseException:
        _close_pipes()
        raise

    def _run_loop(cand_fn: Callable[..., Any]) -> None:
        with contextlib.redirect_stdout(sys.stderr):
            _write_json_line(
                out_fd,
                {"ok": True, "req_id": init_req_id, "event": "loaded"},
                write_fn=os_write,
            )
            precloned_by_wl: dict[int, list[list[Any]]] = {}
            stashed_bench: dict[int, Any] = {}

            while True:
                cmd_line = reader.read_line()
                if not cmd_line:
                    break
                cmd = json.loads(cmd_line)
                req_id = str(cmd.get("req_id") or "")
                op = cmd.get("op")
                if op == "quit":
                    break
                if op == "ping":
                    _write_json_line(
                        out_fd, {"ok": True, "req_id": req_id}, write_fn=os_write
                    )
                    continue

                wl_idx = int(cmd["workload"])
                if op == "warmup":
                    in_path = Path(str(cmd["input_path"]))
                    loaded = _load_tensor_payload(torch, in_path, device=device)
                    cand_args = _clone_tensors(torch, loaded)
                    del loaded
                    cand_fn(*cand_args)
                    if callable(cuda_sync):
                        cuda_sync()
                    _write_json_line(
                        out_fd, {"ok": True, "req_id": req_id}, write_fn=os_write
                    )
                elif op == "prepare_bench":
                    raw_paths = cmd.get("input_paths")
                    if not isinstance(raw_paths, list) or not raw_paths:
                        raise RuntimeError("Missing prepare_bench input_paths")
                    batches = [
                        _clone_tensors(
                            torch,
                            _load_tensor_payload(torch, Path(str(p)), device=device),
                        )
                        for p in raw_paths
                    ]
                    precloned_by_wl[wl_idx] = batches
                    if callable(cuda_sync):
                        cuda_sync()
                    _write_json_line(
                        out_fd, {"ok": True, "req_id": req_id}, write_fn=os_write
                    )
                elif op == "bench":
                    iters = int(cmd["iters"])
                    capture_iter = int(cmd.get("capture_iter", -1))
                    batches = precloned_by_wl.pop(wl_idx, None)
                    if batches is None or len(batches) != iters:
                        raise RuntimeError("Missing prepared benchmark inputs")
                    stashed_bench.pop(wl_idx, None)
                    samples, captured_out = _execute_timed_batches(
                        timer, cand_fn, batches, capture_iter, torch
                    )
                    del batches
                    if captured_out is not None:
                        stashed_bench[wl_idx] = captured_out
                    _write_json_line(
                        out_fd,
                        {"ok": True, "req_id": req_id, "samples": samples},
                        write_fn=os_write,
                    )
                elif op == "save_captured":
                    out_path = Path(str(cmd["output_path"]))
                    if wl_idx not in stashed_bench:
                        raise RuntimeError("Missing captured benchmark output")
                    _save_tensor_payload(torch, stashed_bench.pop(wl_idx), out_path)
                    _write_json_line(
                        out_fd, {"ok": True, "req_id": req_id}, write_fn=os_write
                    )
                elif op == "verify_unseen":
                    in_path = Path(str(cmd["input_path"]))
                    out_path = Path(str(cmd["output_path"]))
                    loaded = _load_tensor_payload(torch, in_path, device=device)
                    cand_args = _clone_tensors(torch, loaded)
                    del loaded
                    out = cand_fn(*cand_args)
                    if out is None:
                        raise RuntimeError(
                            "RUNTIME_ERROR: candidate returned None during verification"
                        )
                    if callable(cuda_sync):
                        cuda_sync()
                    _save_tensor_payload(
                        torch, _clone_output_tree(torch, out), out_path
                    )
                    _write_json_line(
                        out_fd, {"ok": True, "req_id": req_id}, write_fn=os_write
                    )
                else:
                    raise RuntimeError(f"Unknown child op: {op!r}")

    return lib_path, entrypoint, _run_loop, _close_pipes


def _run_candidate_child_fd(cmd_fd: int, ack_fd: int) -> int:
    """Isolated candidate child worker executing ONLY cand_fn on per-command inputs."""
    sys.argv[1:] = []
    raw_fds = (cmd_fd, ack_fd)
    del cmd_fd, ack_fd
    close_pipes: Callable[[], None] | None = None
    try:
        lib_path, entrypoint, run_loop, close_pipes = _prepare_child_rpc_session(
            raw_fds
        )
        del raw_fds
        cand_fn = _load_candidate_entrypoint(lib_path, entrypoint)
        del lib_path, entrypoint
        run_loop(cand_fn)
        return 0
    except Exception as exc:
        sys.stderr.write(f"{exc}\n")
        sys.stderr.flush()
        return 1
    finally:
        if close_pipes is not None:
            close_pipes()


def _run_trusted_driver_fd(cmd_fd: int, res_fd: int) -> int:
    """Trusted reference/orchestrator worker used when torch is injected via PYTHONPATH."""
    reader = _PipeLineReader(cmd_fd)
    try:
        raw = reader.read_line()
        if not raw:
            raise RuntimeError("Missing trusted driver payload")
        spec = json.loads(raw)
        th_raw = spec.get("thresholds") or {}
        thresholds = VerificationThresholds(
            min_speedup_ratio=float(
                th_raw.get("min_speedup_ratio", DEFAULT_MIN_SPEEDUP_RATIO)
            ),
            atol=float(th_raw.get("atol", DEFAULT_ATOL)),
            rtol=float(th_raw.get("rtol", DEFAULT_RTOL)),
        )
        evaluation = _evaluate_entry_with_isolated_candidate(
            lib_path=Path(str(spec["library_path"])),
            entrypoint=str(spec.get("entrypoint") or "run"),
            op_family=str(spec["op_family"]),
            definition=spec.get("definition") or {},
            workloads=spec.get("workloads") or [],
            thresholds=thresholds,
            warmup_iters=int(spec.get("warmup_iters", DEFAULT_WARMUP_ITERS)),
            bench_iters=int(spec.get("bench_iters", DEFAULT_BENCH_ITERS)),
        )
        _write_json_line(res_fd, evaluation)
        return 0
    except Exception as exc:
        sys.stderr.write(f"{exc}\n")
        sys.stderr.flush()
        return 1
    finally:
        with contextlib.suppress(OSError):
            os.close(cmd_fd)
        with contextlib.suppress(OSError):
            os.close(res_fd)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Verify ForgeSandbox candidate kernels against reference definitions and held-out workloads."
    )
    parser.add_argument(
        "--_eval-candidate-fd",
        nargs=2,
        type=int,
        default=None,
        help=argparse.SUPPRESS,
    )
    parser.add_argument(
        "--_eval-trusted-fd",
        nargs=2,
        type=int,
        default=None,
        help=argparse.SUPPRESS,
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
    parser.add_argument(
        "--warmup-iters",
        type=int,
        default=None,
        help="Number of untimed warm-up iterations per workload before timing.",
    )
    parser.add_argument(
        "--bench-iters",
        type=int,
        default=None,
        help="Number of interleaved timed iterations per workload.",
    )
    args = parser.parse_args(argv)
    if args._eval_candidate_fd is not None:
        return _run_candidate_child_fd(
            args._eval_candidate_fd[0], args._eval_candidate_fd[1]
        )
    if args._eval_trusted_fd is not None:
        return _run_trusted_driver_fd(
            args._eval_trusted_fd[0], args._eval_trusted_fd[1]
        )
    result = verify_forge_output(
        input_dir=args.input_dir,
        output_dir=args.output_dir,
        report_path=args.report_path,
        min_speedup_ratio=args.min_speedup_ratio,
        atol=args.atol,
        rtol=args.rtol,
        warmup_iters=args.warmup_iters,
        bench_iters=args.bench_iters,
    )
    return 0 if result.overall_status == "PASSED" else 1


if __name__ == "__main__":
    sys.exit(main())
