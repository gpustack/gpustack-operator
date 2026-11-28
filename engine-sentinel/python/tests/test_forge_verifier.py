"""Tests for gpustack_engine_sentinel.forge_verifier live-GPU verification contract."""

from __future__ import annotations

import hashlib
import json
import os
import subprocess
import sys
from pathlib import Path
from typing import Any

import pytest

from gpustack_engine_sentinel.forge_verifier import (
    VERIFICATION_REPORT_SCHEMA_VERSION,
    verify_forge_output,
)
from gpustack_engine_sentinel.forge_verifier import (
    main as verifier_main,
)
from gpustack_engine_sentinel.probe import format_local_kernel_key
from gpustack_engine_sentinel.protocol import DISPATCH_MANIFEST_SCHEMA_VERSION

REPO_PYTHON_ROOT = Path(__file__).resolve().parents[1]


def _sha256(path: Path) -> str:
    return f"sha256:{hashlib.sha256(path.read_bytes()).hexdigest()}"


def _build_forge_dirs(
    base: Path,
    *,
    entries: list[dict[str, Any]] | None = None,
    schema_version: str = DISPATCH_MANIFEST_SCHEMA_VERSION,
    min_speedup_ratio: float = 1.05,
    atol: float = 1e-2,
    rtol: float = 1e-2,
    tamper_checksum: bool = False,
) -> tuple[Path, Path, Path, str]:
    in_dir = base / "input"
    out_dir = base / "output"
    report_path = base / "verified" / "verification-report.json"

    (in_dir / "definitions").mkdir(parents=True, exist_ok=True)
    (in_dir / "workloads").mkdir(parents=True, exist_ok=True)
    (out_dir / "solutions").mkdir(parents=True, exist_ok=True)
    (out_dir / "kernels" / "rmsnorm_v1").mkdir(parents=True, exist_ok=True)
    (out_dir / "traces").mkdir(parents=True, exist_ok=True)

    key = format_local_kernel_key(
        engine="vLLM",
        engine_version="0.8.5",
        manufacturer="nvidia",
        arch="sm_90a",
        op_family="rmsnorm",
        const_axes={"hidden_size": 4096},
        input_dtypes=["bfloat16", "bfloat16"],
        output_dtypes=["bfloat16"],
    )

    target_keys = [key]
    if entries is not None:
        extra_keys = [
            str(e["localKernelKey"])
            for e in entries
            if isinstance(e, dict) and e.get("localKernelKey")
        ]
        if extra_keys:
            target_keys = list(dict.fromkeys(extra_keys))

    (in_dir / "forge-request.json").write_text(
        json.dumps(
            {
                "engine": "vLLM",
                "engineVersion": "0.8.5",
                "manufacturer": "nvidia",
                "architecture": "sm_90a",
                "minSpeedupRatio": min_speedup_ratio,
                "atol": atol,
                "rtol": rtol,
                "targetKernelKeys": target_keys,
            }
        ),
        encoding="utf-8",
    )
    (in_dir / "definitions" / "rmsnorm_4096.json").write_text(
        json.dumps(
            {
                "name": "rmsnorm_4096",
                "op_type": "rmsnorm",
                "local_kernel_key": key,
                "axes": {
                    "hidden_size": {"type": "const", "value": 4096},
                    "num_tokens": {"type": "var"},
                },
                "inputs": {
                    "x": {"shape": ["num_tokens", "hidden_size"], "dtype": "bfloat16"},
                    "weight": {"shape": ["hidden_size"], "dtype": "bfloat16"},
                },
                "outputs": {
                    "out": {"shape": ["num_tokens", "hidden_size"], "dtype": "bfloat16"}
                },
                "reference": (
                    "def run(x, weight, eps=1e-6):\n"
                    "    return x * torch.rsqrt(x.pow(2).mean(-1, keepdim=True) + eps) * weight"
                ),
            }
        ),
        encoding="utf-8",
    )
    (in_dir / "workloads" / "rmsnorm_4096.jsonl").write_text(
        json.dumps(
            {
                "uuid": "wl-1",
                "definition": "rmsnorm_4096",
                "axes": {"num_tokens": 2},
            }
        )
        + "\n",
        encoding="utf-8",
    )

    lib_file = out_dir / "kernels" / "rmsnorm_v1" / "kernel.py"
    lib_file.write_text(
        "import torch\n"
        "def run(x, weight, eps=1e-6):\n"
        "    return x * torch.rsqrt(x.pow(2).mean(-1, keepdim=True) + eps) * weight\n",
        encoding="utf-8",
    )
    sol_file = out_dir / "solutions" / "rmsnorm_v1.json"
    sol_file.write_text(json.dumps({"name": "rmsnorm_v1"}), encoding="utf-8")
    trace_file = out_dir / "traces" / "rmsnorm_v1.json"
    trace_file.write_text(
        json.dumps(
            {
                "definition": "rmsnorm_4096",
                "solution": "rmsnorm_v1",
                "evaluation": {
                    "status": "PASSED",
                    "correctness": {"max_abs_error": 0.0, "max_relative_error": 0.0},
                    "performance": {
                        "speedup_factor": 1.25,
                        "latency_ms": 0.4,
                        "reference_latency_ms": 0.5,
                    },
                },
            }
        ),
        encoding="utf-8",
    )

    manifest_entries = (
        entries
        if entries is not None
        else [
            {
                "localKernelKey": key,
                "definition": "rmsnorm_4096",
                "solutionPath": "solutions/rmsnorm_v1.json",
                "libraryPath": "kernels/rmsnorm_v1/kernel.py",
                "entrypoint": "run",
                "tracePath": "traces/rmsnorm_v1.json",
                "librarySha256": (
                    "sha256:" + ("0" * 64) if tamper_checksum else _sha256(lib_file)
                ),
                "solutionSha256": _sha256(sol_file),
                "traceSha256": _sha256(trace_file),
            }
        ]
    )

    manifest = {
        "schemaVersion": schema_version,
        "compatibility": {
            "engine": "vLLM",
            "engineVersion": "0.8.5",
            "manufacturer": "nvidia",
            "architecture": "sm_90a",
        },
        "entries": manifest_entries,
    }
    (out_dir / "dispatch-manifest.json").write_text(
        json.dumps(manifest, indent=2), encoding="utf-8"
    )
    return in_dir, out_dir, report_path, key


def test_forge_verifier_imports_without_heavy_ml_deps() -> None:
    assert "torch" not in sys.modules
    assert "triton" not in sys.modules
    assert "flashinfer" not in sys.modules


def test_forge_verifier_passes_valid_candidate_and_writes_report(
    tmp_path: Path,
) -> None:
    in_dir, out_dir, report_path, key = _build_forge_dirs(tmp_path)

    seen_workloads: list[int] = []

    def _fake_exec(
        entry: dict[str, Any],
        definition: dict[str, Any],
        workloads: list[dict[str, Any]],
        solution: dict[str, Any],
        root: Path,
    ) -> dict[str, Any]:
        del entry, solution, root
        assert definition["name"] == "rmsnorm_4096"
        seen_workloads.append(len(workloads))
        return {
            "status": "PASSED",
            "correctness": {"max_abs_error": 1e-4, "max_relative_error": 5e-4},
            "performance": {
                "speedup_factor": 1.30,
                "latency_ms": 0.35,
                "reference_latency_ms": 0.455,
            },
        }

    result = verify_forge_output(
        input_dir=in_dir,
        output_dir=out_dir,
        report_path=report_path,
        executor=_fake_exec,
    )
    assert result.overall_status == "PASSED"
    assert result.passed_count == 1
    assert result.failed_count == 0
    assert seen_workloads == [1]

    report = json.loads(report_path.read_text(encoding="utf-8"))
    assert report["schemaVersion"] == VERIFICATION_REPORT_SCHEMA_VERSION
    assert report["overallStatus"] == "PASSED"
    assert report["dispatchManifestDigest"].startswith("sha256:")
    assert report["entries"][0]["localKernelKey"] == key
    assert report["entries"][0]["passed"] is True


@pytest.mark.parametrize(
    "evaluation,expected_reason_substr",
    [
        (
            {
                "status": "INCORRECT_NUMERICAL",
                "correctness": {"max_abs_error": 0.0, "max_relative_error": 0.0},
                "performance": {"speedup_factor": 1.5},
            },
            "expected 'PASSED'",
        ),
        (
            {
                "status": "PASSED",
                "correctness": {
                    "allclose_passed": False,
                    "max_abs_error": 0.05,
                    "max_relative_error": 0.02,
                },
                "performance": {"speedup_factor": 1.5},
            },
            "allclose",
        ),
        (
            {
                "status": "PASSED",
                "correctness": {"max_abs_error": 0.05, "max_relative_error": 0.05},
                "performance": {"speedup_factor": 1.5},
            },
            "exceeds rtol",
        ),
        (
            {
                "status": "PASSED",
                "correctness": {"max_abs_error": 0.0001, "max_relative_error": 0.0001},
                "performance": {"speedup_factor": 1.01},
            },
            "below minSpeedupRatio",
        ),
    ],
)
def test_forge_verifier_rejects_failing_evaluation_thresholds(
    tmp_path: Path, evaluation: dict[str, Any], expected_reason_substr: str
) -> None:
    in_dir, out_dir, report_path, _ = _build_forge_dirs(tmp_path)
    result = verify_forge_output(
        input_dir=in_dir,
        output_dir=out_dir,
        report_path=report_path,
        executor=lambda *_args: evaluation,
    )
    assert result.overall_status == "FAILED"
    assert result.passed_count == 0
    assert result.failed_count == 1
    assert expected_reason_substr in result.entries[0]["reason"]


def test_forge_verifier_rejects_checksum_symlink_hardlink_escape_and_bad_schema(
    tmp_path: Path,
) -> None:
    # 1. Tampered SHA-256 checksum.
    in1, out1, rep1, _ = _build_forge_dirs(tmp_path / "tamper", tamper_checksum=True)
    res1 = verify_forge_output(
        input_dir=in1,
        output_dir=out1,
        report_path=rep1,
        executor=lambda *_args: {
            "status": "PASSED",
            "correctness": {"max_abs_error": 0.0, "max_relative_error": 0.0},
            "performance": {"speedup_factor": 1.5},
        },
    )
    assert res1.overall_status == "FAILED"
    assert "Checksum mismatch" in res1.entries[0]["reason"]

    # 2. Symlink in output tree.
    in2, out2, rep2, _ = _build_forge_dirs(tmp_path / "symlink")
    lib2 = out2 / "kernels" / "rmsnorm_v1" / "kernel.py"
    target = tmp_path / "outside.py"
    target.write_text("def run(x): return x\n", encoding="utf-8")
    lib2.unlink()
    os.symlink(target, lib2)
    res2 = verify_forge_output(input_dir=in2, output_dir=out2, report_path=rep2)
    assert res2.overall_status == "FAILED"
    assert "Symlinks are forbidden" in res2.entries[0]["reason"]

    # 3. Hard link (st_nlink > 1) in output tree.
    in3, out3, rep3, _ = _build_forge_dirs(tmp_path / "hardlink")
    lib3 = out3 / "kernels" / "rmsnorm_v1" / "kernel.py"
    os.link(lib3, tmp_path / "hardlink_alias.py")
    res3 = verify_forge_output(input_dir=in3, output_dir=out3, report_path=rep3)
    assert res3.overall_status == "FAILED"
    assert "Hard-linked files are forbidden" in res3.entries[0]["reason"]

    # 4. Path escape (..).
    escape_key = format_local_kernel_key(
        engine="vLLM",
        engine_version="0.8.5",
        manufacturer="nvidia",
        arch="sm_90a",
        op_family="rmsnorm",
        const_axes={"hidden_size": 4096},
        input_dtypes=["bfloat16", "bfloat16"],
        output_dtypes=["bfloat16"],
    )
    in4, out4, rep4, _ = _build_forge_dirs(
        tmp_path / "escape",
        entries=[
            {
                "localKernelKey": escape_key,
                "definition": "rmsnorm_4096",
                "solutionPath": "../outside.json",
                "libraryPath": "kernels/rmsnorm_v1/kernel.py",
            }
        ],
    )
    res4 = verify_forge_output(input_dir=in4, output_dir=out4, report_path=rep4)
    assert res4.overall_status == "FAILED"
    assert "escapes output root" in res4.entries[0]["reason"]

    # 5. Wrong schemaVersion -> CLI exits non-zero (1).
    in5, out5, rep5, _ = _build_forge_dirs(
        tmp_path / "bad_schema", schema_version="invalid/v0"
    )
    exit_code = verifier_main(
        [
            "--input-dir",
            str(in5),
            "--output-dir",
            str(out5),
            "--report-path",
            str(rep5),
        ]
    )
    assert exit_code == 1
    assert json.loads(rep5.read_text(encoding="utf-8"))["overallStatus"] == "FAILED"


def test_forge_verifier_cli_module_invocation_exits_nonzero_on_empty_entries(
    tmp_path: Path,
) -> None:
    in_dir, out_dir, rep_path, _ = _build_forge_dirs(tmp_path / "empty", entries=[])
    env = dict(os.environ)
    env["PYTHONPATH"] = str(REPO_PYTHON_ROOT)
    proc = subprocess.run(
        [
            sys.executable,
            "-m",
            "gpustack_engine_sentinel.forge_verifier",
            "--input-dir",
            str(in_dir),
            "--output-dir",
            str(out_dir),
            "--report-path",
            str(rep_path),
        ],
        env=env,
        capture_output=True,
        text=True,
        check=False,
    )
    assert proc.returncode == 1
    report = json.loads(rep_path.read_text(encoding="utf-8"))
    assert report["overallStatus"] == "FAILED"
    assert "no candidate entries" in report["error"]


def _write_fake_torch_module(dir_path: Path) -> Path:
    """Write a pure-Python fake torch module into dir_path for isolated subprocess tests."""
    dir_path.mkdir(parents=True, exist_ok=True)
    (dir_path / "torch.py").write_text(
        """
from __future__ import annotations
from collections import namedtuple
import math
import os
import random

_TopkOut = namedtuple("topk", ["values", "indices"])

def _bcast_pair(a_vals, a_shape, b_vals, b_shape):
    if a_shape == b_shape:
        return a_vals, b_vals, a_shape
    if len(a_shape) == 2 and len(b_shape) == 2:
        r_a, c_a = a_shape
        r_b, c_b = b_shape
        r = max(r_a, r_b)
        c = max(c_a, c_b)
        out_a = []
        out_b = []
        for i in range(r):
            ia = 0 if r_a == 1 else i
            ib = 0 if r_b == 1 else i
            for j in range(c):
                ja = 0 if c_a == 1 else j
                jb = 0 if c_b == 1 else j
                out_a.append(a_vals[ia * c_a + ja])
                out_b.append(b_vals[ib * c_b + jb])
        return out_a, out_b, (r, c)
    if len(a_shape) == 2 and len(b_shape) == 1 and a_shape[1] == b_shape[0]:
        r, c = a_shape
        out_b = b_vals * r
        return a_vals, out_b, a_shape
    if len(b_shape) == 2 and len(a_shape) == 1 and b_shape[1] == a_shape[0]:
        r, c = b_shape
        out_a = a_vals * r
        return out_a, b_vals, b_shape
    raise ValueError(f"Unsupported broadcast shapes {a_shape} vs {b_shape}")

class _FakeTensor:
    def __init__(self, vals, shape=None, dtype="float32"):
        self.dtype = str(dtype or "float32")
        if self.dtype in ("int64", "int32", "long"):
            self._vals = [int(v) for v in vals]
        else:
            self._vals = [float(v) for v in vals]
        self.shape = tuple(int(d) for d in shape) if shape is not None else (len(self._vals),)
        self.ndim = len(self.shape)
        self.device = "cpu"

    def clone(self):
        return _FakeTensor(list(self._vals), self.shape, dtype=self.dtype)

    def detach(self):
        return self

    def cpu(self):
        return self

    def to(self, target):
        if target in ("float16", "bfloat16", "float32", "float64", "int64", "int32", "bool"):
            return _FakeTensor(list(self._vals), self.shape, dtype=target)
        return self

    def float(self):
        return _FakeTensor([float(v) for v in self._vals], self.shape, dtype="float32")

    def pow(self, exp):
        return _FakeTensor([float(v) ** exp for v in self._vals], self.shape, dtype=self.dtype)

    def mean(self, dim=-1, keepdim=False):
        assert dim in (-1, len(self.shape) - 1)
        if len(self.shape) == 1:
            m = sum(self._vals) / max(1, len(self._vals))
            return _FakeTensor([m], (1,) if keepdim else (), dtype=self.dtype)
        rows, cols = self.shape
        out = [sum(self._vals[r * cols : (r + 1) * cols]) / cols for r in range(rows)]
        return _FakeTensor(out, (rows, 1) if keepdim else (rows,), dtype=self.dtype)

    def sum(self, dim=-1, keepdim=False):
        assert dim in (-1, len(self.shape) - 1)
        if len(self.shape) == 1:
            s = sum(self._vals)
            return _FakeTensor([s], (1,) if keepdim else (), dtype=self.dtype)
        rows, cols = self.shape
        out = [sum(self._vals[r * cols : (r + 1) * cols]) for r in range(rows)]
        return _FakeTensor(out, (rows, 1) if keepdim else (rows,), dtype=self.dtype)

    def __sub__(self, other):
        if isinstance(other, _FakeTensor):
            av, bv, shp = _bcast_pair(self._vals, self.shape, other._vals, other.shape)
            return _FakeTensor([a - b for a, b in zip(av, bv, strict=True)], shp, dtype=self.dtype)
        return _FakeTensor([a - float(other) for a in self._vals], self.shape, dtype=self.dtype)

    def __add__(self, other):
        if isinstance(other, _FakeTensor):
            av, bv, shp = _bcast_pair(self._vals, self.shape, other._vals, other.shape)
            return _FakeTensor([a + b for a, b in zip(av, bv, strict=True)], shp, dtype=self.dtype)
        return _FakeTensor([a + float(other) for a in self._vals], self.shape, dtype=self.dtype)

    def __mul__(self, other):
        if isinstance(other, _FakeTensor):
            av, bv, shp = _bcast_pair(self._vals, self.shape, other._vals, other.shape)
            return _FakeTensor([a * b for a, b in zip(av, bv, strict=True)], shp, dtype=self.dtype)
        return _FakeTensor([a * float(other) for a in self._vals], self.shape, dtype=self.dtype)

    def __truediv__(self, other):
        if isinstance(other, _FakeTensor):
            av, bv, shp = _bcast_pair(self._vals, self.shape, other._vals, other.shape)
            return _FakeTensor([a / b for a, b in zip(av, bv, strict=True)], shp, dtype=self.dtype)
        return _FakeTensor([a / float(other) for a in self._vals], self.shape, dtype=self.dtype)

    def abs(self):
        return _FakeTensor([abs(v) for v in self._vals], self.shape, dtype=self.dtype)

    def clamp_min(self, floor):
        return _FakeTensor([max(v, floor) for v in self._vals], self.shape, dtype=self.dtype)

    def gather(self, dim, index):
        assert dim in (-1, len(self.shape) - 1)
        if len(self.shape) == 1:
            return _FakeTensor([self._vals[int(i)] for i in index._vals], index.shape, dtype=self.dtype)
        rows, cols = self.shape
        _, k = index.shape
        out = []
        for r in range(rows):
            base = r * cols
            for j in range(k):
                out.append(self._vals[base + int(index._vals[r * k + j])])
        return _FakeTensor(out, index.shape, dtype=self.dtype)

    def numel(self):
        return len(self._vals)

    def reshape(self, *shape):
        if len(shape) == 1 and isinstance(shape[0], (tuple, list)):
            dims = list(shape[0])
        else:
            dims = list(shape)
        if len(dims) == 1 and dims[0] == -1:
            return _FakeTensor(list(self._vals), (len(self._vals),), dtype=self.dtype)
        known = 1
        neg_idx = -1
        for i, d in enumerate(dims):
            if int(d) == -1:
                neg_idx = i
            else:
                known *= int(d)
        if neg_idx >= 0:
            dims[neg_idx] = len(self._vals) // max(1, known)
        return _FakeTensor(list(self._vals), tuple(int(d) for d in dims), dtype=self.dtype)

    def __getitem__(self, idx):
        if isinstance(idx, int) and len(self.shape) == 1:
            return _FakeTensor([self._vals[idx]], (1,), dtype=self.dtype)
        raise TypeError("Unsupported _FakeTensor index")

    def max(self):
        return _FakeTensor([max(self._vals)], (1,), dtype=self.dtype)

    def item(self):
        return float(self._vals[0])

float16 = "float16"
bfloat16 = "bfloat16"
float32 = "float32"
float64 = "float64"
long = "int64"
int64 = "int64"
int32 = "int32"
bool = "bool"

class cuda:
    @staticmethod
    def is_available():
        return False

def rsqrt(t):
    return _FakeTensor([1.0 / math.sqrt(v) for v in t._vals], t.shape, dtype=t.dtype)

def softmax(t, dim=-1):
    assert dim in (-1, len(t.shape) - 1)
    rows = t.shape[0] if len(t.shape) == 2 else 1
    cols = t.shape[-1]
    out = []
    for r in range(rows):
        row = t._vals[r * cols : (r + 1) * cols]
        m = max(row)
        exps = [math.exp(v - m) for v in row]
        s = sum(exps) or 1.0
        out.extend([e / s for e in exps])
    return _FakeTensor(out, t.shape, dtype=t.dtype)

def topk(t, k, dim=-1):
    assert dim in (-1, len(t.shape) - 1)
    rows = t.shape[0] if len(t.shape) == 2 else 1
    cols = t.shape[-1]
    vals_out = []
    idx_out = []
    for r in range(rows):
        row = t._vals[r * cols : (r + 1) * cols]
        ranked = sorted(enumerate(row), key=lambda p: p[1], reverse=True)[:k]
        vals_out.extend([v for _, v in ranked])
        idx_out.extend([i for i, _ in ranked])
    out_shape = (rows, k) if len(t.shape) == 2 else (k,)
    return _TopkOut(
        _FakeTensor(vals_out, out_shape, dtype=t.dtype),
        _FakeTensor(idx_out, out_shape, dtype="int64"),
    )

def randn(shape, device=None, dtype=None):
    del device
    dims = tuple(int(d) for d in shape) if isinstance(shape, (list, tuple)) else (int(shape),)
    if dims and 0 in dims:
        return _FakeTensor([], dims, dtype=dtype or float32)
    fixed = os.environ.get("_FAKE_TORCH_FIXED_VAL")
    count = math.prod(dims) if dims else 1
    if fixed is not None:
        vals = [float(fixed)] * count
    else:
        vals = [round(random.uniform(0.5, 2.5), 6) for _ in range(count)]
    return _FakeTensor(vals, dims, dtype=dtype or float32)

def zeros(shape, device=None, dtype=None):
    del device
    dims = tuple(int(d) for d in shape) if isinstance(shape, (list, tuple)) else (int(shape),)
    count = math.prod(dims) if dims else 1
    return _FakeTensor([0] * count, dims, dtype=dtype or float32)

def empty(*args, device=None, dtype=None):
    del device
    if len(args) == 1 and isinstance(args[0], (list, tuple)):
        dims = tuple(int(d) for d in args[0])
    else:
        dims = tuple(int(d) for d in args)
    if dims and 0 in dims:
        return _FakeTensor([], dims, dtype=dtype or float32)
    count = math.prod(dims) if dims else 1
    return _FakeTensor([0.0] * count, dims, dtype=dtype or float32)

def tensor(vals, device=None, dtype=None):
    del device
    if vals and isinstance(vals[0], list):
        rows = len(vals)
        cols = len(vals[0])
        flat = [item for row in vals for item in row]
        inferred = dtype or (int64 if all(isinstance(v, int) for v in flat) else float32)
        return _FakeTensor(flat, (rows, cols), dtype=inferred)
    inferred = dtype
    if inferred is None:
        inferred = int64 if vals and all(isinstance(v, int) for v in vals) else float32
    return _FakeTensor(vals, dtype=inferred)
""",
        encoding="utf-8",
    )
    return dir_path


def test_forge_verifier_default_executor_with_fake_torch_and_mixed_entries(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    fake_torch_dir = _write_fake_torch_module(tmp_path / "fake_torch_pkg")
    existing_pp = os.environ.get("PYTHONPATH", "")
    monkeypatch.setenv(
        "PYTHONPATH",
        os.pathsep.join(
            p for p in [str(fake_torch_dir), str(REPO_PYTHON_ROOT), existing_pp] if p
        ),
    )

    in_dir, out_dir, rep_path, _ = _build_forge_dirs(
        tmp_path / "live_exec", min_speedup_ratio=0.0
    )
    res = verify_forge_output(
        input_dir=in_dir,
        output_dir=out_dir,
        report_path=rep_path,
        min_speedup_ratio=0.0,
    )
    assert res.overall_status == "PASSED"
    assert res.passed_count == 1
    assert res.entries[0]["evaluation"]["correctness"]["allclose_passed"] is True
    assert res.entries[0]["evaluation"]["correctness"][
        "max_abs_error"
    ] == pytest.approx(0.0, abs=1e-6)
    assert "torch" not in sys.modules


def test_forge_verifier_default_executor_isolates_subprocess_and_blocks_os_exit_forgery(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    fake_torch_dir = _write_fake_torch_module(tmp_path / "fake_torch_iso")
    existing_pp = os.environ.get("PYTHONPATH", "")
    monkeypatch.setenv(
        "PYTHONPATH",
        os.pathsep.join(
            p for p in [str(fake_torch_dir), str(REPO_PYTHON_ROOT), existing_pp] if p
        ),
    )

    # 1. Candidate runs in a separate PID, sys.argv[1:] is scrubbed, and report_path is absent from env.
    in_dir, out_dir, rep_path, _ = _build_forge_dirs(
        tmp_path / "iso_pid", min_speedup_ratio=0.0
    )
    probe_file = tmp_path / "child_meta.json"
    lib_file = out_dir / "kernels" / "rmsnorm_v1" / "kernel.py"
    lib_file.write_text(
        f"import json, os, sys, torch\n"
        f"_IMPORT_ARGV = list(sys.argv)\n"
        f"def run(x, weight, eps=1e-6):\n"
        f"    with open({str(probe_file)!r}, 'w', encoding='utf-8') as fh:\n"
        f"        json.dump({{'pid': os.getpid(), 'argv': _IMPORT_ARGV, 'run_argv': list(sys.argv), 'env': dict(os.environ)}}, fh)\n"
        f"    return x * torch.rsqrt(x.pow(2).mean(-1, keepdim=True) + eps) * weight\n",
        encoding="utf-8",
    )
    manifest = json.loads((out_dir / "dispatch-manifest.json").read_text("utf-8"))
    manifest["entries"][0]["librarySha256"] = _sha256(lib_file)
    (out_dir / "dispatch-manifest.json").write_text(
        json.dumps(manifest), encoding="utf-8"
    )

    res = verify_forge_output(
        input_dir=in_dir,
        output_dir=out_dir,
        report_path=rep_path,
        min_speedup_ratio=0.0,
    )
    assert res.overall_status == "PASSED"
    child_meta = json.loads(probe_file.read_text(encoding="utf-8"))
    assert child_meta["pid"] != os.getpid()
    assert child_meta["argv"][1:] == []
    assert child_meta["run_argv"][1:] == []
    assert str(rep_path) not in json.dumps(child_meta["argv"])
    assert str(rep_path.parent) not in json.dumps(child_meta["argv"])
    assert str(rep_path) not in json.dumps(child_meta["env"])

    # 2. Malicious candidate calling os._exit(0) cannot forge verification-report.json.
    in_evil, out_evil, rep_evil, _ = _build_forge_dirs(
        tmp_path / "iso_exit", min_speedup_ratio=0.0
    )
    evil_lib = out_evil / "kernels" / "rmsnorm_v1" / "kernel.py"
    evil_lib.write_text(
        "import os\ndef run(x, weight, eps=1e-6):\n    os._exit(0)\n",
        encoding="utf-8",
    )
    manifest_evil = json.loads((out_evil / "dispatch-manifest.json").read_text("utf-8"))
    manifest_evil["entries"][0]["librarySha256"] = _sha256(evil_lib)
    (out_evil / "dispatch-manifest.json").write_text(
        json.dumps(manifest_evil), encoding="utf-8"
    )

    res_evil = verify_forge_output(
        input_dir=in_evil,
        output_dir=out_evil,
        report_path=rep_evil,
        min_speedup_ratio=0.0,
    )
    assert res_evil.overall_status == "FAILED"
    assert res_evil.passed_count == 0
    assert res_evil.failed_count == 1
    written = json.loads(rep_evil.read_text(encoding="utf-8"))
    assert written["overallStatus"] == "FAILED"


def test_forge_verifier_default_executor_supports_tuple_outputs(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    fake_torch_dir = _write_fake_torch_module(tmp_path / "fake_torch_tuple")
    existing_pp = os.environ.get("PYTHONPATH", "")
    monkeypatch.setenv(
        "PYTHONPATH",
        os.pathsep.join(
            p for p in [str(fake_torch_dir), str(REPO_PYTHON_ROOT), existing_pp] if p
        ),
    )

    base = tmp_path / "tuple_pass"
    in_dir = base / "input"
    out_dir = base / "output"
    rep_path = base / "verified" / "verification-report.json"
    (in_dir / "definitions").mkdir(parents=True)
    (in_dir / "workloads").mkdir(parents=True)
    (out_dir / "solutions").mkdir(parents=True)
    (out_dir / "kernels" / "moe_v1").mkdir(parents=True)

    moe_key = format_local_kernel_key(
        engine="vLLM",
        engine_version="0.8.5",
        manufacturer="nvidia",
        arch="sm_90a",
        op_family="moe_router_topk",
        const_axes={"num_experts_local": 8, "top_k": 8},
        input_dtypes=["bfloat16"],
        output_dtypes=["bfloat16", "int64"],
    )
    (in_dir / "forge-request.json").write_text(
        json.dumps(
            {
                "engine": "vLLM",
                "engineVersion": "0.8.5",
                "manufacturer": "nvidia",
                "architecture": "sm_90a",
                "minSpeedupRatio": 0.0,
                "atol": 1e-2,
                "rtol": 1e-2,
                "targetKernelKeys": [moe_key],
            }
        ),
        encoding="utf-8",
    )
    (in_dir / "definitions" / "moe_router_topk_8.json").write_text(
        json.dumps(
            {
                "name": "moe_router_topk_8",
                "op_type": "moe_router_topk",
                "local_kernel_key": moe_key,
                "axes": {
                    "num_experts_local": {"type": "const", "value": 8},
                    "top_k": {"type": "const", "value": 8},
                    "num_tokens": {"type": "var"},
                },
                "inputs": {
                    "logits": {
                        "shape": ["num_tokens", "num_experts_local"],
                        "dtype": "bfloat16",
                    }
                },
                "outputs": {
                    "values": {"shape": ["num_tokens", "top_k"], "dtype": "bfloat16"},
                    "indices": {"shape": ["num_tokens", "top_k"], "dtype": "int64"},
                },
            }
        ),
        encoding="utf-8",
    )
    (in_dir / "workloads" / "moe_router_topk_8.jsonl").write_text(
        json.dumps(
            {
                "uuid": "wl-moe",
                "definition": "moe_router_topk_8",
                "axes": {"num_tokens": 3},
            }
        )
        + "\n",
        encoding="utf-8",
    )
    lib_file = out_dir / "kernels" / "moe_v1" / "kernel.py"
    lib_file.write_text(
        "import torch\n"
        "def run(logits):\n"
        "    probs = torch.softmax(logits, dim=-1)\n"
        "    return torch.topk(probs, k=min(8, logits.shape[-1]), dim=-1)\n",
        encoding="utf-8",
    )
    sol_file = out_dir / "solutions" / "moe_v1.json"
    sol_file.write_text(json.dumps({"name": "moe_v1"}), encoding="utf-8")
    manifest = {
        "schemaVersion": DISPATCH_MANIFEST_SCHEMA_VERSION,
        "compatibility": {
            "engine": "vLLM",
            "engineVersion": "0.8.5",
            "manufacturer": "nvidia",
            "architecture": "sm_90a",
        },
        "entries": [
            {
                "localKernelKey": moe_key,
                "definition": "moe_router_topk_8",
                "solutionPath": "solutions/moe_v1.json",
                "libraryPath": "kernels/moe_v1/kernel.py",
                "entrypoint": "run",
                "librarySha256": _sha256(lib_file),
                "solutionSha256": _sha256(sol_file),
            }
        ],
    }
    (out_dir / "dispatch-manifest.json").write_text(
        json.dumps(manifest), encoding="utf-8"
    )

    res_ok = verify_forge_output(
        input_dir=in_dir,
        output_dir=out_dir,
        report_path=rep_path,
        min_speedup_ratio=0.0,
    )
    assert res_ok.overall_status == "PASSED"
    assert res_ok.entries[0]["evaluation"]["correctness"]["allclose_passed"] is True

    # Diverging duplicate/wrong indices in tuple must fail numerical verification.
    lib_file.write_text(
        "import torch\n"
        "def run(logits):\n"
        "    probs = torch.softmax(logits, dim=-1)\n"
        "    vals, idx = torch.topk(probs, k=min(8, logits.shape[-1]), dim=-1)\n"
        "    bad_idx = torch.zeros(idx.shape, dtype=torch.int64)\n"
        "    return (vals, bad_idx)\n",
        encoding="utf-8",
    )
    manifest["entries"][0]["librarySha256"] = _sha256(lib_file)
    (out_dir / "dispatch-manifest.json").write_text(
        json.dumps(manifest), encoding="utf-8"
    )
    res_bad = verify_forge_output(
        input_dir=in_dir,
        output_dir=out_dir,
        report_path=rep_path,
        min_speedup_ratio=0.0,
    )
    assert res_bad.overall_status == "FAILED"


def test_forge_verifier_mixed_pass_and_fail_entries_fails_overall_and_cli_exits_nonzero(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    fake_torch_dir = _write_fake_torch_module(tmp_path / "fake_torch_mixed")
    existing_pp = os.environ.get("PYTHONPATH", "")
    monkeypatch.setenv(
        "PYTHONPATH",
        os.pathsep.join(
            p for p in [str(fake_torch_dir), str(REPO_PYTHON_ROOT), existing_pp] if p
        ),
    )

    in_dir, out_dir, rep_path, key1 = _build_forge_dirs(
        tmp_path / "mixed_multi", min_speedup_ratio=0.0
    )
    key2 = format_local_kernel_key(
        engine="vLLM",
        engine_version="0.8.5",
        manufacturer="nvidia",
        arch="sm_90a",
        op_family="rmsnorm",
        const_axes={"hidden_size": 2048},
        input_dtypes=["bfloat16", "bfloat16"],
        output_dtypes=["bfloat16"],
    )
    bad_dir = out_dir / "kernels" / "rmsnorm_v2"
    bad_dir.mkdir(parents=True, exist_ok=True)
    bad_lib = bad_dir / "kernel.py"
    bad_lib.write_text(
        "def run(x, weight, eps=1e-6):\n    return x + 10.0\n", encoding="utf-8"
    )
    sol_file = out_dir / "solutions" / "rmsnorm_v1.json"

    req_obj = json.loads((in_dir / "forge-request.json").read_text("utf-8"))
    req_obj["targetKernelKeys"] = [key1, key2]
    (in_dir / "forge-request.json").write_text(json.dumps(req_obj), encoding="utf-8")
    (in_dir / "definitions" / "rmsnorm_2048.json").write_text(
        json.dumps(
            {
                "name": "rmsnorm_2048",
                "op_type": "rmsnorm",
                "local_kernel_key": key2,
                "axes": {
                    "hidden_size": {"type": "const", "value": 2048},
                    "num_tokens": {"type": "var"},
                },
                "inputs": {
                    "x": {"shape": ["num_tokens", "hidden_size"], "dtype": "bfloat16"},
                    "weight": {"shape": ["hidden_size"], "dtype": "bfloat16"},
                },
            }
        ),
        encoding="utf-8",
    )

    manifest = json.loads((out_dir / "dispatch-manifest.json").read_text("utf-8"))
    manifest["entries"].append(
        {
            "localKernelKey": key2,
            "definition": "rmsnorm_2048",
            "solutionPath": "solutions/rmsnorm_v1.json",
            "libraryPath": "kernels/rmsnorm_v2/kernel.py",
            "entrypoint": "run",
            "librarySha256": _sha256(bad_lib),
            "solutionSha256": _sha256(sol_file),
        }
    )
    (out_dir / "dispatch-manifest.json").write_text(
        json.dumps(manifest), encoding="utf-8"
    )

    exit_code = verifier_main(
        [
            "--input-dir",
            str(in_dir),
            "--output-dir",
            str(out_dir),
            "--report-path",
            str(rep_path),
            "--min-speedup-ratio",
            "0.0",
        ]
    )
    assert exit_code == 1
    report = json.loads(rep_path.read_text(encoding="utf-8"))
    assert report["overallStatus"] == "FAILED"
    assert report["passedCount"] == 1
    assert report["failedCount"] == 1
    assert report["entries"][0]["localKernelKey"] == key1
    assert report["entries"][0]["passed"] is True
    assert report["entries"][1]["localKernelKey"] == key2
    assert report["entries"][1]["passed"] is False


def test_forge_verifier_validation_edges_and_missing_manifest(tmp_path: Path) -> None:
    from gpustack_engine_sentinel.forge_verifier import (
        VerificationThresholds,
        _evaluate_candidate_verdict,
    )

    # Missing dispatch-manifest.json -> overall FAILED.
    res_missing = verify_forge_output(
        input_dir=tmp_path / "no_in",
        output_dir=tmp_path / "no_out",
        report_path=tmp_path / "missing_rep.json",
    )
    assert res_missing.overall_status == "FAILED"
    assert "Invalid dispatch-manifest.json" in (res_missing.error or "")

    # Direct threshold verdict edge cases.
    th = VerificationThresholds(min_speedup_ratio=1.05, atol=1e-2, rtol=1e-2)
    for bad_eval, substr in (
        ("not-a-dict", "must be an object"),
        ({"status": "PASSED"}, "missing evaluation.correctness"),
        (
            {"status": "PASSED", "correctness": {"max_abs_error": "x"}},
            "invalid max_abs_error",
        ),
        (
            {
                "status": "PASSED",
                "correctness": {"max_abs_error": -1.0, "max_relative_error": 0.0},
            },
            "non-finite or negative max_abs_error",
        ),
        (
            {
                "status": "PASSED",
                "correctness": {
                    "max_abs_error": 0.0,
                    "max_relative_error": float("nan"),
                },
            },
            "non-finite or negative max_relative_error",
        ),
        (
            {
                "status": "PASSED",
                "correctness": {"max_abs_error": 0.0, "max_relative_error": 0.0},
            },
            "missing evaluation.performance",
        ),
        (
            {
                "status": "PASSED",
                "correctness": {"max_abs_error": 0.0, "max_relative_error": 0.0},
                "performance": {"speedup_factor": "nope"},
            },
            "invalid speedup_factor",
        ),
    ):
        ok, msg = _evaluate_candidate_verdict(bad_eval, th)  # type: ignore[arg-type]
        assert ok is False
        assert substr in msg

    # Bare-hex checksum (without sha256: prefix) and non-dict entry in mixed manifest.
    in_dir, out_dir, rep_path, key = _build_forge_dirs(tmp_path / "bare_hex")
    manifest = json.loads((out_dir / "dispatch-manifest.json").read_text("utf-8"))
    manifest["entries"][0]["librarySha256"] = manifest["entries"][0][
        "librarySha256"
    ].removeprefix("sha256:")
    manifest["entries"].append("not-an-entry")
    (out_dir / "dispatch-manifest.json").write_text(
        json.dumps(manifest), encoding="utf-8"
    )
    res_mixed = verify_forge_output(
        input_dir=in_dir,
        output_dir=out_dir,
        report_path=rep_path,
        executor=lambda *_args: {
            "status": "PASSED",
            "correctness": {"max_abs_error": 0.0, "max_relative_error": 0.0},
            "performance": {"speedup_factor": 1.2},
        },
    )
    assert res_mixed.overall_status == "FAILED"
    assert res_mixed.passed_count == 1
    assert res_mixed.failed_count == 1
    assert res_mixed.entries[0]["localKernelKey"] == key


def test_s1_stdout_fd1_and_direct_report_write_and_lingering_child_cannot_forge_pass(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    import time

    fake_torch_dir = _write_fake_torch_module(tmp_path / "fake_torch_s1")
    existing_pp = os.environ.get("PYTHONPATH", "")
    monkeypatch.setenv(
        "PYTHONPATH",
        os.pathsep.join(
            p for p in [str(fake_torch_dir), str(REPO_PYTHON_ROOT), existing_pp] if p
        ),
    )
    monkeypatch.setenv("GPUSTACK_FORGE_VERIFY_WARMUP_ITERS", "1")
    monkeypatch.setenv("GPUSTACK_FORGE_VERIFY_BENCH_ITERS", "2")

    # 1. Forged stdout exploit: os.write(1, b'{"status":"PASSED",...}\n'); os._exit(0) at import time.
    in_fd1, out_fd1, rep_fd1, _ = _build_forge_dirs(tmp_path / "s1_fd1")
    evil_fd1 = out_fd1 / "kernels" / "rmsnorm_v1" / "kernel.py"
    evil_fd1.write_text(
        "import os\n"
        'os.write(1, b\'{"status":"PASSED","correctness":{"max_abs_error":0,"max_relative_error":0},"performance":{"speedup_factor":50}}\\n\')\n'
        "os._exit(0)\n",
        encoding="utf-8",
    )
    m_fd1 = json.loads((out_fd1 / "dispatch-manifest.json").read_text("utf-8"))
    m_fd1["entries"][0]["librarySha256"] = _sha256(evil_fd1)
    (out_fd1 / "dispatch-manifest.json").write_text(json.dumps(m_fd1), encoding="utf-8")

    res_fd1 = verify_forge_output(
        input_dir=in_fd1, output_dir=out_fd1, report_path=rep_fd1
    )
    assert res_fd1.overall_status == "FAILED"
    assert res_fd1.passed_count == 0
    assert json.loads(rep_fd1.read_text("utf-8"))["overallStatus"] == "FAILED"

    # 2. Direct write to report_path + pre-existing PASSED file + forked lingering child trying delayed overwrite.
    in_rw, out_rw, rep_rw, key_rw = _build_forge_dirs(tmp_path / "s1_direct_report")
    rep_rw.parent.mkdir(parents=True, exist_ok=True)
    forged_report = json.dumps(
        {
            "schemaVersion": VERIFICATION_REPORT_SCHEMA_VERSION,
            "overallStatus": "PASSED",
            "dispatchManifestDigest": _sha256(out_rw / "dispatch-manifest.json"),
            "passedCount": 1,
            "failedCount": 0,
            "thresholds": {"minSpeedupRatio": 1.05, "atol": 0.01, "rtol": 0.01},
            "entries": [
                {"index": 0, "localKernelKey": key_rw, "passed": True, "reason": "ok"}
            ],
        }
    )
    rep_rw.write_text(forged_report, encoding="utf-8")
    grandchild_pid_file = tmp_path / "grandchild.pid"
    evil_rw = out_rw / "kernels" / "rmsnorm_v1" / "kernel.py"
    evil_rw.write_text(
        f"import os, time\n"
        f"from pathlib import Path\n"
        f"def run(x, weight, eps=1e-6):\n"
        f"    Path({str(rep_rw)!r}).parent.mkdir(parents=True, exist_ok=True)\n"
        f"    Path({str(rep_rw)!r}).write_text({forged_report!r}, encoding='utf-8')\n"
        f"    pid = os.fork()\n"
        f"    if pid == 0:\n"
        f"        time.sleep(0.25)\n"
        f"        Path({str(rep_rw)!r}).write_text({forged_report!r}, encoding='utf-8')\n"
        f"        os._exit(0)\n"
        f"    Path({str(grandchild_pid_file)!r}).write_text(str(pid), encoding='utf-8')\n"
        f"    return x + 100.0\n",
        encoding="utf-8",
    )
    m_rw = json.loads((out_rw / "dispatch-manifest.json").read_text("utf-8"))
    m_rw["entries"][0]["librarySha256"] = _sha256(evil_rw)
    (out_rw / "dispatch-manifest.json").write_text(json.dumps(m_rw), encoding="utf-8")

    res_rw = verify_forge_output(
        input_dir=in_rw,
        output_dir=out_rw,
        report_path=rep_rw,
        min_speedup_ratio=0.0,
    )
    assert res_rw.overall_status == "FAILED"
    if grandchild_pid_file.exists():
        gpid = int(grandchild_pid_file.read_text("utf-8").strip())
        with pytest.raises(ProcessLookupError):
            os.kill(gpid, 0)
    time.sleep(0.35)
    assert json.loads(rep_rw.read_text("utf-8"))["overallStatus"] == "FAILED"

    # 3. Monkeypatching time.perf_counter inside candidate cannot fake speedup.
    in_mp, out_mp, rep_mp, _ = _build_forge_dirs(
        tmp_path / "s1_monkeypatch_clock", min_speedup_ratio=1000.0
    )
    evil_mp = out_mp / "kernels" / "rmsnorm_v1" / "kernel.py"
    evil_mp.write_text(
        "import time, torch\n"
        "time.perf_counter = lambda: 0.0\n"
        "def run(x, weight, eps=1e-6):\n"
        "    return x * torch.rsqrt(x.pow(2).mean(-1, keepdim=True) + eps) * weight\n",
        encoding="utf-8",
    )
    m_mp = json.loads((out_mp / "dispatch-manifest.json").read_text("utf-8"))
    m_mp["entries"][0]["librarySha256"] = _sha256(evil_mp)
    (out_mp / "dispatch-manifest.json").write_text(json.dumps(m_mp), encoding="utf-8")

    res_mp = verify_forge_output(
        input_dir=in_mp,
        output_dir=out_mp,
        report_path=rep_mp,
        min_speedup_ratio=1000.0,
    )
    assert res_mp.overall_status == "FAILED"


def test_s2_binds_entries_to_forge_request_keys_definition_and_compatibility(
    tmp_path: Path,
) -> None:
    def pass_exec(*_args: Any) -> dict[str, Any]:
        return {
            "status": "PASSED",
            "correctness": {"max_abs_error": 0.0, "max_relative_error": 0.0},
            "performance": {
                "speedup_factor": 1.5,
                "latency_ms": 0.2,
                "reference_latency_ms": 0.3,
            },
        }

    # 1. ArchCompatible sm_90 vs sm_90a succeeds, while sm_80 vs sm_90a or engine mismatch fails.
    in_compat, out_compat, rep_compat, _ = _build_forge_dirs(tmp_path / "s2_compat_ok")
    m_ok = json.loads((out_compat / "dispatch-manifest.json").read_text("utf-8"))
    m_ok["compatibility"]["architecture"] = "sm_90"
    (out_compat / "dispatch-manifest.json").write_text(
        json.dumps(m_ok), encoding="utf-8"
    )
    assert (
        verify_forge_output(
            input_dir=in_compat,
            output_dir=out_compat,
            report_path=rep_compat,
            executor=pass_exec,
        ).overall_status
        == "PASSED"
    )

    m_bad_arch = json.loads((out_compat / "dispatch-manifest.json").read_text("utf-8"))
    m_bad_arch["compatibility"]["architecture"] = "sm_80"
    (out_compat / "dispatch-manifest.json").write_text(
        json.dumps(m_bad_arch), encoding="utf-8"
    )
    res_bad_arch = verify_forge_output(
        input_dir=in_compat,
        output_dir=out_compat,
        report_path=rep_compat,
        executor=pass_exec,
    )
    assert res_bad_arch.overall_status == "FAILED"

    # 2. Entry localKernelKey not in forge-request.json targetKernelKeys -> FAILED.
    in_unreq, out_unreq, rep_unreq, _ = _build_forge_dirs(tmp_path / "s2_unrequested")
    unreq_key = format_local_kernel_key(
        engine="vLLM",
        engine_version="0.8.5",
        manufacturer="nvidia",
        arch="sm_90a",
        op_family="rmsnorm",
        const_axes={"hidden_size": 8192},
        input_dtypes=["bfloat16", "bfloat16"],
        output_dtypes=["bfloat16"],
    )
    m_unreq = json.loads((out_unreq / "dispatch-manifest.json").read_text("utf-8"))
    m_unreq["entries"][0]["localKernelKey"] = unreq_key
    (out_unreq / "dispatch-manifest.json").write_text(
        json.dumps(m_unreq), encoding="utf-8"
    )
    res_unreq = verify_forge_output(
        input_dir=in_unreq,
        output_dir=out_unreq,
        report_path=rep_unreq,
        executor=pass_exec,
    )
    assert res_unreq.overall_status == "FAILED"
    assert "targetKernelKeys" in res_unreq.entries[0]["reason"]

    # 3. Cross-op substitution: layernorm key (in targetKernelKeys) pointing to rmsnorm_4096 definition -> FAILED.
    in_cross, out_cross, rep_cross, rms_key = _build_forge_dirs(
        tmp_path / "s2_cross_op"
    )
    ln_key = format_local_kernel_key(
        engine="vLLM",
        engine_version="0.8.5",
        manufacturer="nvidia",
        arch="sm_90a",
        op_family="layernorm",
        const_axes={"hidden_size": 4096},
        input_dtypes=["bfloat16", "bfloat16"],
        output_dtypes=["bfloat16"],
    )
    req_cross = json.loads((in_cross / "forge-request.json").read_text("utf-8"))
    req_cross["targetKernelKeys"] = [rms_key, ln_key]
    (in_cross / "forge-request.json").write_text(
        json.dumps(req_cross), encoding="utf-8"
    )

    m_cross = json.loads((out_cross / "dispatch-manifest.json").read_text("utf-8"))
    m_cross["entries"][0]["localKernelKey"] = ln_key
    m_cross["entries"][0]["definition"] = "rmsnorm_4096"
    (out_cross / "dispatch-manifest.json").write_text(
        json.dumps(m_cross), encoding="utf-8"
    )
    res_cross = verify_forge_output(
        input_dir=in_cross,
        output_dir=out_cross,
        report_path=rep_cross,
        executor=pass_exec,
    )
    assert res_cross.overall_status == "FAILED"


def test_n1_warmup_and_interleaved_median_timing(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    fake_torch_dir = _write_fake_torch_module(tmp_path / "fake_torch_n1")
    # Make torch.rsqrt (used by the trusted built-in rmsnorm reference) take ~6ms per call.
    with (fake_torch_dir / "torch.py").open("a", encoding="utf-8") as fh:
        fh.write(
            "\nimport time as _rsqrt_t\n"
            "_orig_rsqrt = rsqrt\n"
            "def rsqrt(t):\n"
            "    _rsqrt_t.sleep(0.006)\n"
            "    return _orig_rsqrt(t)\n"
        )
    existing_pp = os.environ.get("PYTHONPATH", "")
    monkeypatch.setenv(
        "PYTHONPATH",
        os.pathsep.join(
            p for p in [str(fake_torch_dir), str(REPO_PYTHON_ROOT), existing_pp] if p
        ),
    )

    in_dir, out_dir, rep_path, _ = _build_forge_dirs(
        tmp_path / "n1_bench", min_speedup_ratio=1.2
    )
    # Candidate has a huge one-time cold JIT penalty on call #1 (30ms), then computes rmsnorm directly in ~0.5ms.
    cand_calls_file = tmp_path / "cand_calls.txt"
    lib_file = out_dir / "kernels" / "rmsnorm_v1" / "kernel.py"
    lib_file.write_text(
        f"import math, time, torch\n"
        f"_CALLS = 0\n"
        f"def run(x, weight, eps=1e-6):\n"
        f"    global _CALLS\n"
        f"    _CALLS += 1\n"
        f"    if _CALLS == 1:\n"
        f"        time.sleep(0.030)\n"
        f"    else:\n"
        f"        time.sleep(0.0005)\n"
        f"    with open({str(cand_calls_file)!r}, 'w', encoding='utf-8') as fh:\n"
        f"        fh.write(str(_CALLS))\n"
        f"    var = x.pow(2).mean(-1, keepdim=True) + eps\n"
        f"    inv = torch._FakeTensor([1.0 / math.sqrt(v) for v in var._vals], var.shape, dtype=var.dtype)\n"
        f"    return x * inv * weight\n",
        encoding="utf-8",
    )
    manifest = json.loads((out_dir / "dispatch-manifest.json").read_text("utf-8"))
    manifest["entries"][0]["librarySha256"] = _sha256(lib_file)
    (out_dir / "dispatch-manifest.json").write_text(
        json.dumps(manifest), encoding="utf-8"
    )

    monkeypatch.setenv("GPUSTACK_FORGE_VERIFY_WARMUP_ITERS", "2")
    monkeypatch.setenv("GPUSTACK_FORGE_VERIFY_BENCH_ITERS", "4")
    res = verify_forge_output(
        input_dir=in_dir,
        output_dir=out_dir,
        report_path=rep_path,
        min_speedup_ratio=1.2,
    )
    assert res.overall_status == "PASSED"
    assert res.entries[0]["evaluation"]["performance"]["speedup_factor"] >= 1.2
    assert int(cand_calls_file.read_text("utf-8")) >= 6

    # Also verify CUDA Event(enable_timing=True) path when cuda.is_available() is True.
    cuda_torch_dir = _write_fake_torch_module(tmp_path / "fake_torch_cuda")
    with (cuda_torch_dir / "torch.py").open("a", encoding="utf-8") as fh:
        fh.write(
            "\nimport time as _t\n"
            "_orig_rsqrt2 = rsqrt\n"
            "def rsqrt(t):\n"
            "    _t.sleep(0.006)\n"
            "    return _orig_rsqrt2(t)\n"
            "class _CudaEvent:\n"
            "    def __init__(self, enable_timing=False):\n"
            "        self.enable_timing = enable_timing\n"
            "        self.ts = 0.0\n"
            "    def record(self):\n"
            "        self.ts = _t.perf_counter()\n"
            "    def synchronize(self):\n"
            "        pass\n"
            "    def elapsed_time(self, end_ev):\n"
            "        return (end_ev.ts - self.ts) * 1000.0\n"
            "cuda.is_available = staticmethod(lambda: True)\n"
            "cuda.synchronize = staticmethod(lambda: None)\n"
            "cuda.Event = _CudaEvent\n"
        )
    monkeypatch.setenv(
        "PYTHONPATH",
        os.pathsep.join(
            p for p in [str(cuda_torch_dir), str(REPO_PYTHON_ROOT), existing_pp] if p
        ),
    )
    res_cuda = verify_forge_output(
        input_dir=in_dir,
        output_dir=out_dir,
        report_path=rep_path,
        min_speedup_ratio=1.2,
    )
    assert res_cuda.overall_status == "PASSED"
    assert res_cuda.entries[0]["evaluation"]["performance"]["speedup_factor"] >= 1.2


def test_n2_allclose_near_atol_bf16_ulp_and_fp32_reference_evaluation(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    fake_torch_dir = _write_fake_torch_module(tmp_path / "fake_torch_n2")
    existing_pp = os.environ.get("PYTHONPATH", "")
    monkeypatch.setenv(
        "PYTHONPATH",
        os.pathsep.join(
            p for p in [str(fake_torch_dir), str(REPO_PYTHON_ROOT), existing_pp] if p
        ),
    )
    monkeypatch.setenv("GPUSTACK_FORGE_VERIFY_WARMUP_ITERS", "1")
    monkeypatch.setenv("GPUSTACK_FORGE_VERIFY_BENCH_ITERS", "2")

    def _run_elementwise_case(case_name: str, fixed_in_val: float, delta: float) -> Any:
        monkeypatch.setenv("_FAKE_TORCH_FIXED_VAL", str(fixed_in_val))
        base = tmp_path / case_name
        in_dir = base / "input"
        out_dir = base / "output"
        rep_path = base / "verified" / "verification-report.json"
        (in_dir / "definitions").mkdir(parents=True)
        (in_dir / "workloads").mkdir(parents=True)
        (out_dir / "solutions").mkdir(parents=True)
        (out_dir / "kernels" / "ew_v1").mkdir(parents=True)

        ew_key = format_local_kernel_key(
            engine="vLLM",
            engine_version="0.8.5",
            manufacturer="nvidia",
            arch="sm_90a",
            op_family="custom_elementwise_fusion",
            const_axes={"hidden_size": 16},
            input_dtypes=["bfloat16", "bfloat16"],
            output_dtypes=["bfloat16"],
        )
        (in_dir / "forge-request.json").write_text(
            json.dumps(
                {
                    "engine": "vLLM",
                    "engineVersion": "0.8.5",
                    "manufacturer": "nvidia",
                    "architecture": "sm_90a",
                    "minSpeedupRatio": 0.0,
                    "atol": 0.01,
                    "rtol": 0.01,
                    "targetKernelKeys": [ew_key],
                }
            ),
            encoding="utf-8",
        )
        (in_dir / "definitions" / "ew_16.json").write_text(
            json.dumps(
                {
                    "name": "ew_16",
                    "op_type": "custom_elementwise_fusion",
                    "local_kernel_key": ew_key,
                    "axes": {
                        "hidden_size": {"type": "const", "value": 16},
                        "num_tokens": {"type": "var"},
                    },
                    "inputs": {
                        "x": {
                            "shape": ["num_tokens", "hidden_size"],
                            "dtype": "bfloat16",
                        },
                        "y": {
                            "shape": ["num_tokens", "hidden_size"],
                            "dtype": "bfloat16",
                        },
                    },
                }
            ),
            encoding="utf-8",
        )
        (in_dir / "workloads" / "ew_16.jsonl").write_text(
            json.dumps(
                {"uuid": "wl-ew", "definition": "ew_16", "axes": {"num_tokens": 2}}
            )
            + "\n",
            encoding="utf-8",
        )
        lib_file = out_dir / "kernels" / "ew_v1" / "kernel.py"
        lib_file.write_text(
            f"def run(x, y):\n    return (x + y) + {delta!r}\n",
            encoding="utf-8",
        )
        sol_file = out_dir / "solutions" / "ew_v1.json"
        sol_file.write_text(json.dumps({"name": "ew_v1"}), encoding="utf-8")
        (out_dir / "dispatch-manifest.json").write_text(
            json.dumps(
                {
                    "schemaVersion": DISPATCH_MANIFEST_SCHEMA_VERSION,
                    "compatibility": {
                        "engine": "vLLM",
                        "engineVersion": "0.8.5",
                        "manufacturer": "nvidia",
                        "architecture": "sm_90a",
                    },
                    "entries": [
                        {
                            "localKernelKey": ew_key,
                            "definition": "ew_16",
                            "solutionPath": "solutions/ew_v1.json",
                            "libraryPath": "kernels/ew_v1/kernel.py",
                            "entrypoint": "run",
                            "librarySha256": _sha256(lib_file),
                            "solutionSha256": _sha256(sol_file),
                        }
                    ],
                }
            ),
            encoding="utf-8",
        )
        return verify_forge_output(
            input_dir=in_dir,
            output_dir=out_dir,
            report_path=rep_path,
            min_speedup_ratio=0.0,
        )

    # 1. bfloat16 near |ref| = 0.0101 (0.00505 + 0.00505), diff = 0.0002 -> PASSED.
    res_near_atol = _run_elementwise_case("near_atol", 0.00505, 0.0002)
    assert res_near_atol.overall_status == "PASSED"
    c1 = res_near_atol.entries[0]["evaluation"]["correctness"]
    assert c1["allclose_passed"] is True
    assert c1["max_abs_error"] == pytest.approx(0.0002, abs=1e-6)
    assert c1["max_relative_error"] == pytest.approx(0.0, abs=1e-7)

    # 2. bfloat16 1-ULP at |ref| = 2.0 (1.0 + 1.0), diff = 0.015625 <= 0.03 -> PASSED!
    res_ulp = _run_elementwise_case("bf16_ulp_at_2", 1.0, 0.015625)
    assert res_ulp.overall_status == "PASSED"
    c2 = res_ulp.entries[0]["evaluation"]["correctness"]
    assert c2["allclose_passed"] is True
    assert c2["max_abs_error"] == pytest.approx(0.015625, abs=1e-6)
    assert c2["max_relative_error"] == pytest.approx(0.0028125, abs=1e-6)

    # 3. Genuine error exceeding atol + rtol * |ref| (|ref| = 2.0, diff = 0.05 > 0.03) -> FAILED!
    res_bad = _run_elementwise_case("exceeds_allclose", 1.0, 0.05)
    assert res_bad.overall_status == "FAILED"
    c3 = res_bad.entries[0]["evaluation"]["correctness"]
    assert c3["allclose_passed"] is False
    assert c3["max_abs_error"] == pytest.approx(0.05, abs=1e-6)
    assert c3["max_relative_error"] == pytest.approx(0.02, abs=1e-6)
    monkeypatch.delenv("_FAKE_TORCH_FIXED_VAL", raising=False)

    # 4. Reference evaluation upcasts bfloat16 inputs to float32 and falls back only on dtype error.
    from gpustack_engine_sentinel.forge_verifier import _evaluate_reference_fp32

    ns_ft: dict[str, Any] = {}
    exec((fake_torch_dir / "torch.py").read_text("utf-8"), ns_ft)
    FT = ns_ft["_FakeTensor"]
    seen_dtypes: list[str] = []

    def _recording_ref(t: Any) -> Any:
        seen_dtypes.append(t.dtype)
        return t

    bf16_in = [FT([1.5, 2.5], (2,), dtype="bfloat16")]
    fp32_out, exp_dt = _evaluate_reference_fp32(ns_ft, _recording_ref, bf16_in, None)
    assert exp_dt == "bfloat16"
    assert fp32_out.dtype == "float32"
    assert seen_dtypes == ["bfloat16", "float32"]

    def _bf16_only_ref(t: Any) -> Any:
        if t.dtype != "bfloat16":
            raise TypeError("bf16 required")
        return t

    fb_out, fb_dt = _evaluate_reference_fp32(ns_ft, _bf16_only_ref, bf16_in, None)
    assert fb_dt == "bfloat16"
    assert fb_out.dtype == "bfloat16"


def test_n3_rejects_empty_tensors_shape_broadcast_and_dtype_mismatch(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    fake_torch_dir = _write_fake_torch_module(tmp_path / "fake_torch_n3")
    existing_pp = os.environ.get("PYTHONPATH", "")
    monkeypatch.setenv(
        "PYTHONPATH",
        os.pathsep.join(
            p for p in [str(fake_torch_dir), str(REPO_PYTHON_ROOT), existing_pp] if p
        ),
    )
    monkeypatch.setenv("GPUSTACK_FORGE_VERIFY_WARMUP_ITERS", "1")
    monkeypatch.setenv("GPUSTACK_FORGE_VERIFY_BENCH_ITERS", "2")

    for case_name, cand_body in (
        (
            "empty_cand",
            "import torch\ndef run(x, weight, eps=1e-6):\n    return torch.empty(0, dtype=torch.bfloat16)\n",
        ),
        (
            "broadcast_shape",
            "import torch\ndef run(x, weight, eps=1e-6):\n    return torch._FakeTensor(list(x._vals[:4096]), (1, 4096), dtype=torch.bfloat16)\n",
        ),
        (
            "wrong_dtype",
            "import torch\ndef run(x, weight, eps=1e-6):\n    return (x * torch.rsqrt(x.pow(2).mean(-1, keepdim=True) + eps) * weight).float()\n",
        ),
    ):
        in_dir, out_dir, rep_path, _ = _build_forge_dirs(
            tmp_path / f"n3_{case_name}", min_speedup_ratio=0.0
        )
        lib_file = out_dir / "kernels" / "rmsnorm_v1" / "kernel.py"
        lib_file.write_text(cand_body, encoding="utf-8")
        manifest = json.loads((out_dir / "dispatch-manifest.json").read_text("utf-8"))
        manifest["entries"][0]["librarySha256"] = _sha256(lib_file)
        (out_dir / "dispatch-manifest.json").write_text(
            json.dumps(manifest), encoding="utf-8"
        )
        res = verify_forge_output(
            input_dir=in_dir,
            output_dir=out_dir,
            report_path=rep_path,
            min_speedup_ratio=0.0,
        )
        assert res.overall_status == "FAILED", case_name


def test_security_never_execs_untrusted_definition_reference_in_parent(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    fake_torch_dir = _write_fake_torch_module(tmp_path / "fake_torch_untrusted_ref")
    existing_pp = os.environ.get("PYTHONPATH", "")
    monkeypatch.setenv(
        "PYTHONPATH",
        os.pathsep.join(
            p for p in [str(fake_torch_dir), str(REPO_PYTHON_ROOT), existing_pp] if p
        ),
    )
    monkeypatch.setenv("GPUSTACK_FORGE_VERIFY_WARMUP_ITERS", "1")
    monkeypatch.setenv("GPUSTACK_FORGE_VERIFY_BENCH_ITERS", "2")

    in_dir, out_dir, rep_path, _ = _build_forge_dirs(
        tmp_path / "untrusted_ref", min_speedup_ratio=0.0
    )
    marker = tmp_path / "pwned_parent.txt"
    def_path = in_dir / "definitions" / "rmsnorm_4096.json"
    def_obj = json.loads(def_path.read_text("utf-8"))
    def_obj["reference"] = (
        f"import pathlib\n"
        f"pathlib.Path({str(marker)!r}).write_text('pwned', encoding='utf-8')\n"
        f"pathlib.Path({str(rep_path)!r}).parent.mkdir(parents=True, exist_ok=True)\n"
        f"pathlib.Path({str(rep_path)!r}).write_text('{{\"overallStatus\":\"PASSED\"}}', encoding='utf-8')\n"
        f"def run(x, weight, eps=1e-6):\n"
        f"    return x * 0.0\n"
    )
    def_path.write_text(json.dumps(def_obj), encoding="utf-8")

    # 1. Legitimate rmsnorm candidate passes and marker is NEVER created.
    res = verify_forge_output(
        input_dir=in_dir,
        output_dir=out_dir,
        report_path=rep_path,
        min_speedup_ratio=0.0,
    )
    assert not marker.exists()
    assert res.overall_status == "PASSED"

    # 2. Candidate returning x * 0.0 (matching malicious reference) FAILS against built-in rmsnorm reference!
    lib_file = out_dir / "kernels" / "rmsnorm_v1" / "kernel.py"
    lib_file.write_text(
        "def run(x, weight, eps=1e-6):\n    return x * 0.0\n",
        encoding="utf-8",
    )
    manifest = json.loads((out_dir / "dispatch-manifest.json").read_text("utf-8"))
    manifest["entries"][0]["librarySha256"] = _sha256(lib_file)
    (out_dir / "dispatch-manifest.json").write_text(
        json.dumps(manifest), encoding="utf-8"
    )
    res_zero = verify_forge_output(
        input_dir=in_dir,
        output_dir=out_dir,
        report_path=rep_path,
        min_speedup_ratio=0.0,
    )
    assert not marker.exists()
    assert res_zero.overall_status == "FAILED"


def test_security_blocks_stateful_call_count_cached_output_ack_spoof_and_cuda_stream_evasion(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    fake_torch_dir = _write_fake_torch_module(tmp_path / "fake_torch_spoof")
    existing_pp = os.environ.get("PYTHONPATH", "")
    monkeypatch.setenv(
        "PYTHONPATH",
        os.pathsep.join(
            p for p in [str(fake_torch_dir), str(REPO_PYTHON_ROOT), existing_pp] if p
        ),
    )
    monkeypatch.setenv("GPUSTACK_FORGE_VERIFY_WARMUP_ITERS", "1")
    monkeypatch.setenv("GPUSTACK_FORGE_VERIFY_BENCH_ITERS", "4")

    # 1. Stateful candidate computing real rmsnorm on call #1 and returning None on calls #2+ -> FAILED!
    in1, out1, rep1, _ = _build_forge_dirs(
        tmp_path / "stateful_none", min_speedup_ratio=0.0
    )
    lib1 = out1 / "kernels" / "rmsnorm_v1" / "kernel.py"
    lib1.write_text(
        "import torch\n"
        "_N = 0\n"
        "def run(x, weight, eps=1e-6):\n"
        "    global _N\n"
        "    _N += 1\n"
        "    if _N == 1:\n"
        "        return x * torch.rsqrt(x.pow(2).mean(-1, keepdim=True) + eps) * weight\n"
        "    return None\n",
        encoding="utf-8",
    )
    m1 = json.loads((out1 / "dispatch-manifest.json").read_text("utf-8"))
    m1["entries"][0]["librarySha256"] = _sha256(lib1)
    (out1 / "dispatch-manifest.json").write_text(json.dumps(m1), encoding="utf-8")
    assert (
        verify_forge_output(
            input_dir=in1, output_dir=out1, report_path=rep1, min_speedup_ratio=0.0
        ).overall_status
        == "FAILED"
    )

    # 2. Stateful candidate caching call #1's output tensor and returning it on calls #2+ -> FAILED!
    in2, out2, rep2, _ = _build_forge_dirs(
        tmp_path / "stateful_cache", min_speedup_ratio=0.0
    )
    lib2 = out2 / "kernels" / "rmsnorm_v1" / "kernel.py"
    lib2.write_text(
        "import torch\n"
        "_CACHE = None\n"
        "def run(x, weight, eps=1e-6):\n"
        "    global _CACHE\n"
        "    if _CACHE is None:\n"
        "        _CACHE = x * torch.rsqrt(x.pow(2).mean(-1, keepdim=True) + eps) * weight\n"
        "    return _CACHE\n",
        encoding="utf-8",
    )
    m2 = json.loads((out2 / "dispatch-manifest.json").read_text("utf-8"))
    m2["entries"][0]["librarySha256"] = _sha256(lib2)
    (out2 / "dispatch-manifest.json").write_text(json.dumps(m2), encoding="utf-8")
    assert (
        verify_forge_output(
            input_dir=in2, output_dir=out2, report_path=rep2, min_speedup_ratio=0.0
        ).overall_status
        == "FAILED"
    )

    # 3. Direct ack_fd pipe injection without parent's random req_id nonce -> FAILED!
    in3, out3, rep3, _ = _build_forge_dirs(
        tmp_path / "ack_spoof", min_speedup_ratio=0.0
    )
    lib3 = out3 / "kernels" / "rmsnorm_v1" / "kernel.py"
    lib3.write_text(
        "import os, torch\n"
        "for _fd in range(3, 16):\n"
        "    try:\n"
        '        os.write(_fd, b\'{"ok":true,"event":"loaded"}\\n{"ok":true,"ms":0.0001}\\n\')\n'
        "    except OSError:\n"
        "        pass\n"
        "def run(x, weight, eps=1e-6):\n"
        "    return x * torch.rsqrt(x.pow(2).mean(-1, keepdim=True) + eps) * weight\n",
        encoding="utf-8",
    )
    m3 = json.loads((out3 / "dispatch-manifest.json").read_text("utf-8"))
    m3["entries"][0]["librarySha256"] = _sha256(lib3)
    (out3 / "dispatch-manifest.json").write_text(json.dumps(m3), encoding="utf-8")
    assert (
        verify_forge_output(
            input_dir=in3, output_dir=out3, report_path=rep3, min_speedup_ratio=0.0
        ).overall_status
        == "FAILED"
    )

    # 4. Non-default CUDA stream evasion: candidate queues 20ms work onto a secondary stream
    # that end_ev.synchronize() ignores, while device-wide cuda.synchronize() flushes all streams.
    stream_torch_dir = _write_fake_torch_module(tmp_path / "fake_torch_stream")
    with (stream_torch_dir / "torch.py").open("a", encoding="utf-8") as fh:
        fh.write(
            "\nimport time as _st\n"
            "_PENDING_SIDE_STREAM_SEC = [0.0]\n"
            "_orig_rsqrt_s = rsqrt\n"
            "def rsqrt(t):\n"
            "    _st.sleep(0.004)\n"
            "    return _orig_rsqrt_s(t)\n"
            "class _SideStream:\n"
            "    def launch_slow(self, sec):\n"
            "        _PENDING_SIDE_STREAM_SEC[0] += sec\n"
            "class _StreamCudaEvent:\n"
            "    def __init__(self, enable_timing=False):\n"
            "        self.enable_timing = enable_timing\n"
            "        self.ts = 0.0\n"
            "    def record(self):\n"
            "        self.ts = _st.perf_counter()\n"
            "    def synchronize(self):\n"
            "        # Default-stream event sync intentionally does NOT drain _PENDING_SIDE_STREAM_SEC!\n"
            "        pass\n"
            "    def elapsed_time(self, end_ev):\n"
            "        return (end_ev.ts - self.ts) * 1000.0\n"
            "def _device_wide_sync():\n"
            "    if _PENDING_SIDE_STREAM_SEC[0] > 0:\n"
            "        _st.sleep(_PENDING_SIDE_STREAM_SEC[0])\n"
            "        _PENDING_SIDE_STREAM_SEC[0] = 0.0\n"
            "cuda.is_available = staticmethod(lambda: True)\n"
            "cuda.synchronize = staticmethod(_device_wide_sync)\n"
            "cuda.Event = _StreamCudaEvent\n"
            "cuda.Stream = _SideStream\n"
        )
    monkeypatch.setenv(
        "PYTHONPATH",
        os.pathsep.join(
            p for p in [str(stream_torch_dir), str(REPO_PYTHON_ROOT), existing_pp] if p
        ),
    )
    in4, out4, rep4, _ = _build_forge_dirs(
        tmp_path / "side_stream", min_speedup_ratio=1.05
    )
    lib4 = out4 / "kernels" / "rmsnorm_v1" / "kernel.py"
    lib4.write_text(
        "import math, torch\n"
        "def run(x, weight, eps=1e-6):\n"
        "    s = torch.cuda.Stream()\n"
        "    s.launch_slow(0.020)\n"
        "    var = x.pow(2).mean(-1, keepdim=True) + eps\n"
        "    inv = torch._FakeTensor([1.0 / math.sqrt(v) for v in var._vals], var.shape, dtype=var.dtype)\n"
        "    return x * inv * weight\n",
        encoding="utf-8",
    )
    m4 = json.loads((out4 / "dispatch-manifest.json").read_text("utf-8"))
    m4["entries"][0]["librarySha256"] = _sha256(lib4)
    (out4 / "dispatch-manifest.json").write_text(json.dumps(m4), encoding="utf-8")
    res_stream = verify_forge_output(
        input_dir=in4,
        output_dir=out4,
        report_path=rep4,
        min_speedup_ratio=1.05,
    )
    assert res_stream.overall_status == "FAILED"


def test_namedtuple_topk_serialization_and_int64_index_tie_equivalence(
    tmp_path: Path,
) -> None:
    from collections import namedtuple
    from types import SimpleNamespace

    from gpustack_engine_sentinel.forge_verifier import (
        _compare_outputs,
        _load_tensor_payload,
        _save_tensor_payload,
    )

    # 1. Namedtuple output is normalized to plain tuple before torch.save so weights_only=True succeeds.
    TopkReturn = namedtuple("topk", ["values", "indices"])
    saved_objs: list[Any] = []

    class _RealTensorStub:
        def __init__(self, vals: list[Any]) -> None:
            self.vals = vals

        def detach(self) -> _RealTensorStub:
            return self

        def cpu(self) -> _RealTensorStub:
            return self

    def _strict_save(obj: Any, path: str) -> None:
        saved_objs.append(obj)
        assert type(obj) is tuple, f"Expected plain tuple, got {type(obj)}"
        Path(path).write_text("ok", encoding="utf-8")

    def _strict_load(
        path: str, map_location: str = "cpu", weights_only: bool = False
    ) -> Any:
        del path, map_location
        assert weights_only is True
        return saved_objs[-1]

    mock_real_torch = SimpleNamespace(
        Tensor=_RealTensorStub, save=_strict_save, load=_strict_load
    )
    dest = tmp_path / "topk_out.bin"
    nt_payload = TopkReturn(_RealTensorStub([0.5, 0.5]), _RealTensorStub([1, 0]))
    _save_tensor_payload(mock_real_torch, nt_payload, dest)
    loaded = _load_tensor_payload(mock_real_torch, dest, expected_dir=tmp_path)
    assert type(loaded) is tuple

    # 2. Integer top-k index equivalence on tied scores, bounds, uniqueness, and standalone exact equality.
    fake_torch_dir = _write_fake_torch_module(tmp_path / "ft_idx")
    spec_path = fake_torch_dir / "torch.py"
    ns: dict[str, Any] = {}
    exec(spec_path.read_text("utf-8"), ns)
    FT = ns["_FakeTensor"]

    logits = FT([2.0, 1.0, 1.002, -3.0], (1, 4), dtype="bfloat16")
    ref_vals = FT([0.55, 0.20], (1, 2), dtype="bfloat16")
    cand_vals = FT([0.55, 0.20], (1, 2), dtype="bfloat16")

    # Tied indices (expert 2 vs expert 1 where |1.002 - 1.0| <= atol=0.01) -> PASS!
    ok_tie, abs_tie, _ = _compare_outputs(
        (cand_vals, FT([0, 2], (1, 2), dtype="int64")),
        (ref_vals, FT([0, 1], (1, 2), dtype="int64")),
        atol=0.01,
        rtol=0.01,
        inputs=[logits],
    )
    assert ok_tie is True
    assert abs_tie == pytest.approx(0.0)

    # Duplicate index [0, 0] -> FAIL!
    ok_dup, _, _ = _compare_outputs(
        (cand_vals, FT([0, 0], (1, 2), dtype="int64")),
        (ref_vals, FT([0, 1], (1, 2), dtype="int64")),
        atol=0.01,
        rtol=0.01,
        inputs=[logits],
    )
    assert ok_dup is False

    # Out-of-bounds index [0, 4] when dim_size == 4 -> FAIL!
    ok_oob, _, _ = _compare_outputs(
        (cand_vals, FT([0, 4], (1, 2), dtype="int64")),
        (ref_vals, FT([0, 1], (1, 2), dtype="int64")),
        atol=0.01,
        rtol=0.01,
        inputs=[logits],
    )
    assert ok_oob is False

    # Non-tied wrong index [0, 3] (score -3.0 vs 1.0) -> FAIL!
    ok_wrong, _, _ = _compare_outputs(
        (cand_vals, FT([0, 3], (1, 2), dtype="int64")),
        (ref_vals, FT([0, 1], (1, 2), dtype="int64")),
        atol=0.01,
        rtol=0.01,
        inputs=[logits],
    )
    assert ok_wrong is False

    # Standalone integer tensor falls back to exact equality -> FAIL when indices differ!
    ok_standalone, _, _ = _compare_outputs(
        FT([0, 2, 1], (3,), dtype="int64"),
        FT([0, 1, 2], (3,), dtype="int64"),
        atol=0.01,
        rtol=0.01,
    )
    assert ok_standalone is False


def test_security_defeats_two_input_parity_memoization_and_frame_locals_exposure(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    fake_torch_dir = _write_fake_torch_module(tmp_path / "fake_torch_memo")
    with (fake_torch_dir / "torch.py").open("a", encoding="utf-8") as fh:
        fh.write(
            "\nimport time as _memo_t\n"
            "_orig_rsqrt_memo = rsqrt\n"
            "def rsqrt(t):\n"
            "    _memo_t.sleep(0.008)\n"
            "    return _orig_rsqrt_memo(t)\n"
        )
    existing_pp = os.environ.get("PYTHONPATH", "")
    monkeypatch.setenv(
        "PYTHONPATH",
        os.pathsep.join(
            p for p in [str(fake_torch_dir), str(REPO_PYTHON_ROOT), existing_pp] if p
        ),
    )

    def _build_compact_rmsnorm(
        case_dir: Path, *, min_speedup: float
    ) -> tuple[Path, Path, Path]:
        in_dir, out_dir, rep_path, _ = _build_forge_dirs(
            case_dir, min_speedup_ratio=max(min_speedup, 1.05)
        )
        small_key = format_local_kernel_key(
            engine="vLLM",
            engine_version="0.8.5",
            manufacturer="nvidia",
            arch="sm_90a",
            op_family="rmsnorm",
            const_axes={"hidden_size": 64},
            input_dtypes=["bfloat16", "bfloat16"],
            output_dtypes=["bfloat16"],
        )
        req_obj = json.loads((in_dir / "forge-request.json").read_text("utf-8"))
        req_obj["targetKernelKeys"] = [small_key]
        (in_dir / "forge-request.json").write_text(
            json.dumps(req_obj), encoding="utf-8"
        )
        (in_dir / "definitions" / "rmsnorm_4096.json").unlink()
        (in_dir / "workloads" / "rmsnorm_4096.jsonl").unlink()
        (in_dir / "definitions" / "rmsnorm_64.json").write_text(
            json.dumps(
                {
                    "name": "rmsnorm_64",
                    "op_type": "rmsnorm",
                    "local_kernel_key": small_key,
                    "axes": {
                        "hidden_size": {"type": "const", "value": 64},
                        "num_tokens": {"type": "var"},
                    },
                    "inputs": {
                        "x": {
                            "shape": ["num_tokens", "hidden_size"],
                            "dtype": "bfloat16",
                        },
                        "weight": {"shape": ["hidden_size"], "dtype": "bfloat16"},
                    },
                    "outputs": {
                        "out": {
                            "shape": ["num_tokens", "hidden_size"],
                            "dtype": "bfloat16",
                        }
                    },
                }
            ),
            encoding="utf-8",
        )
        (in_dir / "workloads" / "rmsnorm_64.jsonl").write_text(
            json.dumps(
                {"uuid": "wl-64", "definition": "rmsnorm_64", "axes": {"num_tokens": 2}}
            )
            + "\n",
            encoding="utf-8",
        )
        manifest = json.loads((out_dir / "dispatch-manifest.json").read_text("utf-8"))
        manifest["entries"][0]["localKernelKey"] = small_key
        manifest["entries"][0]["definition"] = "rmsnorm_64"
        (out_dir / "dispatch-manifest.json").write_text(
            json.dumps(manifest), encoding="utf-8"
        )
        return in_dir, out_dir, rep_path

    # 1. Candidate memoizing (x.shape, float(x.reshape(-1)[0].item())) -> slow_ref cannot achieve fake 3x speedup.
    in_m1, out_m1, rep_m1 = _build_compact_rmsnorm(
        tmp_path / "memo_speedup", min_speedup=3.0
    )
    lib_m1 = out_m1 / "kernels" / "rmsnorm_v1" / "kernel.py"
    lib_m1.write_text(
        "import torch\n"
        "_MEMO = {}\n"
        "def run(x, weight, eps=1e-6):\n"
        "    key = (x.shape, float(x.reshape(-1)[0].item()))\n"
        "    if key not in _MEMO:\n"
        "        _MEMO[key] = x * torch.rsqrt(x.pow(2).mean(-1, keepdim=True) + eps) * weight\n"
        "    return _MEMO[key]\n",
        encoding="utf-8",
    )
    m1_doc = json.loads((out_m1 / "dispatch-manifest.json").read_text("utf-8"))
    m1_doc["entries"][0]["librarySha256"] = _sha256(lib_m1)
    (out_m1 / "dispatch-manifest.json").write_text(json.dumps(m1_doc), encoding="utf-8")
    res_m1 = verify_forge_output(
        input_dir=in_m1,
        output_dir=out_m1,
        report_path=rep_m1,
        min_speedup_ratio=3.0,
        warmup_iters=2,
        bench_iters=6,
    )
    assert res_m1.overall_status == "FAILED"
    assert res_m1.entries[0]["passed"] is False

    # 2. Candidate computing real output during warmup/bench (first 8 calls) and returning stale output on unseen post-bench check fails correctness.
    in_m2, out_m2, rep_m2 = _build_compact_rmsnorm(
        tmp_path / "post_bench_unseen", min_speedup=1.05
    )
    lib_m2 = out_m2 / "kernels" / "rmsnorm_v1" / "kernel.py"
    lib_m2.write_text(
        "import math, torch\n"
        "_CALLS = 0\n"
        "_LAST = None\n"
        "def run(x, weight, eps=1e-6):\n"
        "    global _CALLS, _LAST\n"
        "    _CALLS += 1\n"
        "    if _CALLS <= 6:\n"
        "        var = x.pow(2).mean(-1, keepdim=True) + eps\n"
        "        inv = torch._FakeTensor([1.0 / math.sqrt(v) for v in var._vals], var.shape, dtype=var.dtype)\n"
        "        _LAST = x * inv * weight\n"
        "        return _LAST\n"
        "    return _LAST\n",
        encoding="utf-8",
    )
    m2_doc = json.loads((out_m2 / "dispatch-manifest.json").read_text("utf-8"))
    m2_doc["entries"][0]["librarySha256"] = _sha256(lib_m2)
    (out_m2 / "dispatch-manifest.json").write_text(json.dumps(m2_doc), encoding="utf-8")
    res_m2 = verify_forge_output(
        input_dir=in_m2,
        output_dir=out_m2,
        report_path=rep_m2,
        min_speedup_ratio=0.0,
        warmup_iters=2,
        bench_iters=4,
    )
    assert res_m2.overall_status == "FAILED"
    assert res_m2.entries[0]["evaluation"]["correctness"]["allclose_passed"] is False

    # 3. Candidate inspecting sys._getframe(1).f_locals (and caller frame) at import time sees no harness locals.
    in_f, out_f, rep_f = _build_compact_rmsnorm(
        tmp_path / "frame_probe", min_speedup=1.05
    )
    frame_dump = tmp_path / "frame_locals.json"
    lib_f = out_f / "kernels" / "rmsnorm_v1" / "kernel.py"
    lib_f.write_text(
        f"import json, math, sys, torch\n"
        f"_F1 = sorted(sys._getframe(1).f_locals.keys())\n"
        f"_F2 = sorted(sys._getframe(2).f_locals.keys()) if sys._getframe(1).f_back is not None else []\n"
        f"with open({str(frame_dump)!r}, 'w', encoding='utf-8') as _fh:\n"
        f"    json.dump({{'f1': _F1, 'f2': _F2}}, _fh)\n"
        f"def run(x, weight, eps=1e-6):\n"
        f"    var = x.pow(2).mean(-1, keepdim=True) + eps\n"
        f"    inv = torch._FakeTensor([1.0 / math.sqrt(v) for v in var._vals], var.shape, dtype=var.dtype)\n"
        f"    return x * inv * weight\n",
        encoding="utf-8",
    )
    mf_doc = json.loads((out_f / "dispatch-manifest.json").read_text("utf-8"))
    mf_doc["entries"][0]["librarySha256"] = _sha256(lib_f)
    (out_f / "dispatch-manifest.json").write_text(json.dumps(mf_doc), encoding="utf-8")
    res_f = verify_forge_output(
        input_dir=in_f,
        output_dir=out_f,
        report_path=rep_f,
        min_speedup_ratio=0.0,
        warmup_iters=1,
        bench_iters=2,
    )
    assert res_f.overall_status == "PASSED"
    leaked = json.loads(frame_dump.read_text("utf-8"))
    forbidden = {"ack_fd", "cmd_fd", "loaded_a", "loaded_b", "time_call"}
    assert forbidden.isdisjoint(set(leaked["f1"])), leaked["f1"]
    assert forbidden.isdisjoint(set(leaked["f2"])), leaked["f2"]


def test_topk_shift_invariant_softmax_rejects_swapped_high_logits_and_accepts_true_tie(
    tmp_path: Path,
) -> None:
    from gpustack_engine_sentinel.forge_verifier import _compare_outputs

    fake_torch_dir = _write_fake_torch_module(tmp_path / "ft_shift_topk")
    ns: dict[str, Any] = {}
    exec((fake_torch_dir / "torch.py").read_text("utf-8"), ns)
    FT = ns["_FakeTensor"]
    softmax_fn = ns["softmax"]
    topk_fn = ns["topk"]

    # High unnormalized logits [[80.0, 79.3, 0.0, 0.0]]: probs are ~[0.6682, 0.3318, 0.0, 0.0].
    high_logits = FT([80.0, 79.3, 0.0, 0.0], (1, 4), dtype="float32")
    ref_probs, ref_idx = topk_fn(softmax_fn(high_logits, dim=-1), k=2, dim=-1)
    swapped_idx = FT([1, 0], (1, 2), dtype="int64")

    ok_swapped, _, _ = _compare_outputs(
        (ref_probs.clone(), swapped_idx),
        (ref_probs, ref_idx),
        atol=0.01,
        rtol=0.01,
        inputs=[high_logits],
    )
    assert ok_swapped is False

    # Genuine tie [[80.0, 80.0, 0.0, 0.0]]: probs are [0.5, 0.5, 0.0, 0.0], swapped [1, 0] is accepted.
    tied_logits = FT([80.0, 80.0, 0.0, 0.0], (1, 4), dtype="float32")
    tie_probs, tie_idx = topk_fn(softmax_fn(tied_logits, dim=-1), k=2, dim=-1)
    ok_true_tie, _, _ = _compare_outputs(
        (tie_probs.clone(), swapped_idx),
        (tie_probs, tie_idx),
        atol=0.01,
        rtol=0.01,
        inputs=[tied_logits],
    )
    assert ok_true_tie is True


def test_strict_threshold_validation_rejects_bool_nan_inf_negative_and_sub_101_request(
    tmp_path: Path,
) -> None:
    from gpustack_engine_sentinel.forge_verifier import _write_report_atomic

    def _pass_exec(*_args: Any) -> dict[str, Any]:
        return {
            "status": "PASSED",
            "correctness": {
                "allclose_passed": True,
                "max_abs_error": 0.0,
                "max_relative_error": 0.0,
            },
            "performance": {
                "speedup_factor": 1.25,
                "latency_ms": 0.4,
                "reference_latency_ms": 0.5,
            },
        }

    def _assert_strict_json(path: Path) -> dict[str, Any]:
        raw_text = path.read_text(encoding="utf-8")
        return json.loads(
            raw_text,
            parse_constant=lambda c: (_ for _ in ()).throw(
                ValueError(f"Non-RFC-8259 JSON constant {c!r}")
            ),
        )

    # 1. Bool values in forge-request.json ("atol": true, "rtol": false, "minSpeedupRatio": false).
    for idx, patch_fields in enumerate(
        (
            {"atol": True},
            {"rtol": False},
            {"minSpeedupRatio": False},
            {"minSpeedupRatio": True},
        )
    ):
        in_d, out_d, rep_p, _ = _build_forge_dirs(tmp_path / f"bool_req_{idx}")
        req_obj = json.loads((in_d / "forge-request.json").read_text("utf-8"))
        req_obj.update(patch_fields)
        (in_d / "forge-request.json").write_text(json.dumps(req_obj), encoding="utf-8")
        res = verify_forge_output(
            input_dir=in_d, output_dir=out_d, report_path=rep_p, executor=_pass_exec
        )
        assert res.overall_status == "FAILED", patch_fields
        assert "Invalid forge-request.json" in (res.error or "")
        assert _assert_strict_json(rep_p)["overallStatus"] == "FAILED"

    # 2. NaN / Infinity / negative thresholds via overrides and forge-request.json.
    in_base, out_base, _, _ = _build_forge_dirs(tmp_path / "ovr_invalid")
    for idx, kwargs in enumerate(
        (
            {"atol": True},
            {"min_speedup_ratio": False},
            {"atol": float("nan")},
            {"rtol": float("inf")},
            {"min_speedup_ratio": float("-inf")},
            {"atol": -0.01},
            {"rtol": -1e-4},
            {"min_speedup_ratio": -0.5},
        )
    ):
        rep_p = tmp_path / f"ovr_rep_{idx}.json"
        res = verify_forge_output(
            input_dir=in_base,
            output_dir=out_base,
            report_path=rep_p,
            executor=_pass_exec,
            **kwargs,  # type: ignore[arg-type]
        )
        assert res.overall_status == "FAILED", kwargs
        parsed = _assert_strict_json(rep_p)
        assert parsed["overallStatus"] == "FAILED"

    for idx, raw_bad in enumerate(
        (
            '{"minSpeedupRatio": "NaN"}',
            '{"atol": "Infinity"}',
            '{"rtol": -0.01}',
            '{"minSpeedupRatio": -1.0}',
        )
    ):
        in_d, out_d, rep_p, _ = _build_forge_dirs(tmp_path / f"req_bad_{idx}")
        base_req = json.loads((in_d / "forge-request.json").read_text("utf-8"))
        base_req.update(json.loads(raw_bad))
        (in_d / "forge-request.json").write_text(json.dumps(base_req), encoding="utf-8")
        res = verify_forge_output(
            input_dir=in_d, output_dir=out_d, report_path=rep_p, executor=_pass_exec
        )
        assert res.overall_status == "FAILED", raw_bad
        assert _assert_strict_json(rep_p)["overallStatus"] == "FAILED"

    # 3. minSpeedupRatio < 1.01 in forge-request.json without override is rejected; >= 1.01 passes.
    for sub_ratio in (0.0, 1.0, 1.009):
        in_sub, out_sub, rep_sub, _ = _build_forge_dirs(
            tmp_path / f"sub_101_{sub_ratio}", min_speedup_ratio=sub_ratio
        )
        res_sub = verify_forge_output(
            input_dir=in_sub,
            output_dir=out_sub,
            report_path=rep_sub,
            executor=_pass_exec,
        )
        assert res_sub.overall_status == "FAILED", sub_ratio
        assert "1.01" in (res_sub.error or "")

    in_101, out_101, rep_101, _ = _build_forge_dirs(
        tmp_path / "at_101", min_speedup_ratio=1.01
    )
    res_101 = verify_forge_output(
        input_dir=in_101,
        output_dir=out_101,
        report_path=rep_101,
        executor=_pass_exec,
    )
    assert res_101.overall_status == "PASSED"

    # 4. _write_report_atomic strictly rejects NaN / Infinity via allow_nan=False.
    with pytest.raises(ValueError):
        _write_report_atomic(
            tmp_path / "nan_report.json",
            {"overallStatus": "FAILED", "bad": float("nan")},
        )
    with pytest.raises(ValueError):
        _write_report_atomic(
            tmp_path / "inf_report.json",
            {"overallStatus": "FAILED", "bad": float("inf")},
        )


def test_topk_large_vocab_max_normalized_weights_rejects_wrong_indices(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    import random

    from gpustack_engine_sentinel.forge_verifier import _compare_outputs

    fake_torch_dir = _write_fake_torch_module(tmp_path / "ft_large_vocab")
    with (fake_torch_dir / "torch.py").open("a", encoding="utf-8") as fh:
        fh.write(
            "\n_GAUSS_RNG = random.Random(12345)\n"
            "def randn(shape, device=None, dtype=None):\n"
            "    del device\n"
            "    dims = tuple(int(d) for d in shape) if isinstance(shape, (list, tuple)) else (int(shape),)\n"
            "    count = math.prod(dims) if dims else 1\n"
            "    return _FakeTensor([_GAUSS_RNG.gauss(0.0, 1.0) for _ in range(count)], dims, dtype=dtype or float32)\n"
        )

    ns: dict[str, Any] = {}
    exec((fake_torch_dir / "torch.py").read_text("utf-8"), ns)
    FT = ns["_FakeTensor"]
    softmax_fn = ns["softmax"]
    topk_fn = ns["topk"]

    # 1. Realistic 32768-wide N(0, 1) row: wrong indices [0..7] with matching values must fail.
    rng = random.Random(42)
    vocab_vals = [rng.gauss(0.0, 1.0) for _ in range(32768)]
    vocab_logits = FT(vocab_vals, (1, 32768), dtype="float32")
    ref_probs, ref_idx = topk_fn(softmax_fn(vocab_logits, dim=-1), k=8, dim=-1)
    wrong_idx = FT(list(range(8)), (1, 8), dtype="int64")

    ok_wrong, _, _ = _compare_outputs(
        (ref_probs.clone(), wrong_idx),
        (ref_probs, ref_idx),
        atol=0.01,
        rtol=0.01,
        inputs=[vocab_logits],
    )
    assert ok_wrong is False

    # Shift invariance (logits + 80.0): matching indices pass and wrong indices still fail.
    shifted_logits = vocab_logits + 80.0
    shifted_probs, shifted_idx = topk_fn(
        softmax_fn(shifted_logits, dim=-1), k=8, dim=-1
    )
    ok_shifted_exact, _, _ = _compare_outputs(
        (shifted_probs.clone(), shifted_idx.clone()),
        (shifted_probs, shifted_idx),
        atol=0.01,
        rtol=0.01,
        inputs=[shifted_logits],
    )
    assert ok_shifted_exact is True

    ok_shifted_wrong, _, _ = _compare_outputs(
        (shifted_probs.clone(), wrong_idx),
        (shifted_probs, shifted_idx),
        atol=0.01,
        rtol=0.01,
        inputs=[shifted_logits],
    )
    assert ok_shifted_wrong is False

    # Genuine tie [[80.0, 80.0, 0.0, 0.0]] accepts swapped [1, 0], while [[80.0, 79.3, 0.0, 0.0]] rejects it.
    swapped_2 = FT([1, 0], (1, 2), dtype="int64")
    tied_logits = FT([80.0, 80.0, 0.0, 0.0], (1, 4), dtype="float32")
    tied_probs, tied_idx = topk_fn(softmax_fn(tied_logits, dim=-1), k=2, dim=-1)
    assert (
        _compare_outputs(
            (tied_probs.clone(), swapped_2),
            (tied_probs, tied_idx),
            atol=0.01,
            rtol=0.01,
            inputs=[tied_logits],
        )[0]
        is True
    )

    distinct_logits = FT([80.0, 79.3, 0.0, 0.0], (1, 4), dtype="float32")
    distinct_probs, distinct_idx = topk_fn(
        softmax_fn(distinct_logits, dim=-1), k=2, dim=-1
    )
    assert (
        _compare_outputs(
            (distinct_probs.clone(), swapped_2),
            (distinct_probs, distinct_idx),
            atol=0.01,
            rtol=0.01,
            inputs=[distinct_logits],
        )[0]
        is False
    )

    # 2. End-to-end live-verifier check at dim_size = 4096 with N(0, 1) logits returning wrong [0..7] indices.
    existing_pp = os.environ.get("PYTHONPATH", "")
    monkeypatch.setenv(
        "PYTHONPATH",
        os.pathsep.join(
            p for p in [str(fake_torch_dir), str(REPO_PYTHON_ROOT), existing_pp] if p
        ),
    )
    monkeypatch.setenv("GPUSTACK_FORGE_VERIFY_WARMUP_ITERS", "1")
    monkeypatch.setenv("GPUSTACK_FORGE_VERIFY_BENCH_ITERS", "2")

    base = tmp_path / "e2e_wide_topk"
    in_dir = base / "input"
    out_dir = base / "output"
    rep_path = base / "verified" / "verification-report.json"
    (in_dir / "definitions").mkdir(parents=True)
    (in_dir / "workloads").mkdir(parents=True)
    (out_dir / "solutions").mkdir(parents=True)
    (out_dir / "kernels" / "moe_wide").mkdir(parents=True)

    wide_key = format_local_kernel_key(
        engine="vLLM",
        engine_version="0.8.5",
        manufacturer="nvidia",
        arch="sm_90a",
        op_family="moe_router_topk",
        const_axes={"num_experts_local": 4096, "top_k": 8},
        input_dtypes=["bfloat16"],
        output_dtypes=["bfloat16", "int64"],
    )
    (in_dir / "forge-request.json").write_text(
        json.dumps(
            {
                "engine": "vLLM",
                "engineVersion": "0.8.5",
                "manufacturer": "nvidia",
                "architecture": "sm_90a",
                "minSpeedupRatio": 0.0,
                "atol": 0.01,
                "rtol": 0.01,
                "targetKernelKeys": [wide_key],
            }
        ),
        encoding="utf-8",
    )
    (in_dir / "definitions" / "moe_4096.json").write_text(
        json.dumps(
            {
                "name": "moe_4096",
                "op_type": "moe_router_topk",
                "local_kernel_key": wide_key,
                "axes": {
                    "num_experts_local": {"type": "const", "value": 4096},
                    "top_k": {"type": "const", "value": 8},
                    "num_tokens": {"type": "var"},
                },
                "inputs": {
                    "logits": {
                        "shape": ["num_tokens", "num_experts_local"],
                        "dtype": "bfloat16",
                    }
                },
                "outputs": {
                    "values": {"shape": ["num_tokens", "top_k"], "dtype": "bfloat16"},
                    "indices": {"shape": ["num_tokens", "top_k"], "dtype": "int64"},
                },
            }
        ),
        encoding="utf-8",
    )
    (in_dir / "workloads" / "moe_4096.jsonl").write_text(
        json.dumps(
            {"uuid": "wl-moe-4096", "definition": "moe_4096", "axes": {"num_tokens": 2}}
        )
        + "\n",
        encoding="utf-8",
    )
    lib_file = out_dir / "kernels" / "moe_wide" / "kernel.py"
    lib_file.write_text(
        "import torch\n"
        "def run(logits):\n"
        "    probs = torch.softmax(logits, dim=-1)\n"
        "    vals, _ = torch.topk(probs, k=8, dim=-1)\n"
        "    rows = vals.shape[0]\n"
        "    bad_idx = torch.tensor([list(range(8)) for _ in range(rows)], dtype=torch.int64)\n"
        "    return (vals, bad_idx)\n",
        encoding="utf-8",
    )
    sol_file = out_dir / "solutions" / "moe_wide.json"
    sol_file.write_text(json.dumps({"name": "moe_wide"}), encoding="utf-8")
    (out_dir / "dispatch-manifest.json").write_text(
        json.dumps(
            {
                "schemaVersion": DISPATCH_MANIFEST_SCHEMA_VERSION,
                "compatibility": {
                    "engine": "vLLM",
                    "engineVersion": "0.8.5",
                    "manufacturer": "nvidia",
                    "architecture": "sm_90a",
                },
                "entries": [
                    {
                        "localKernelKey": wide_key,
                        "definition": "moe_4096",
                        "solutionPath": "solutions/moe_wide.json",
                        "libraryPath": "kernels/moe_wide/kernel.py",
                        "entrypoint": "run",
                        "librarySha256": _sha256(lib_file),
                        "solutionSha256": _sha256(sol_file),
                    }
                ],
            }
        ),
        encoding="utf-8",
    )

    res = verify_forge_output(
        input_dir=in_dir,
        output_dir=out_dir,
        report_path=rep_path,
        min_speedup_ratio=0.0,
    )
    assert res.overall_status == "FAILED"
    assert res.entries[0]["evaluation"]["correctness"]["allclose_passed"] is False


def test_nonfinite_and_bool_evaluation_sanitizes_report_and_aligns_omitted_allclose(
    tmp_path: Path,
) -> None:
    def _assert_rfc8259_report(path: Path) -> dict[str, Any]:
        assert path.is_file(), f"Expected report file at {path}"
        return json.loads(
            path.read_text(encoding="utf-8"),
            parse_constant=lambda c: (_ for _ in ()).throw(
                ValueError(f"Non-RFC-8259 JSON constant {c!r}")
            ),
        )

    # 1. Non-finite floats and boolean numeric fields fail cleanly and emit valid RFC 8259 FAILED reports.
    invalid_evaluations: list[tuple[str, dict[str, Any]]] = [
        (
            "nan_max_abs",
            {
                "status": "PASSED",
                "correctness": {
                    "allclose_passed": True,
                    "max_abs_error": float("nan"),
                    "max_relative_error": 0.0,
                },
                "performance": {
                    "speedup_factor": 1.5,
                    "latency_ms": 0.2,
                    "reference_latency_ms": 0.3,
                },
            },
        ),
        (
            "neginf_max_rel",
            {
                "status": "PASSED",
                "correctness": {
                    "allclose_passed": True,
                    "max_abs_error": 0.0,
                    "max_relative_error": float("-inf"),
                },
                "performance": {
                    "speedup_factor": 1.5,
                    "latency_ms": 0.2,
                    "reference_latency_ms": 0.3,
                },
            },
        ),
        (
            "inf_speedup",
            {
                "status": "PASSED",
                "correctness": {
                    "allclose_passed": True,
                    "max_abs_error": 0.0,
                    "max_relative_error": 0.0,
                },
                "performance": {
                    "speedup_factor": float("inf"),
                    "latency_ms": 0.2,
                    "reference_latency_ms": 0.3,
                },
            },
        ),
        (
            "nan_latency_ms",
            {
                "status": "PASSED",
                "correctness": {
                    "allclose_passed": True,
                    "max_abs_error": 0.0,
                    "max_relative_error": 0.0,
                },
                "performance": {
                    "speedup_factor": 1.5,
                    "latency_ms": float("nan"),
                    "reference_latency_ms": 0.3,
                },
            },
        ),
        (
            "neg_latency_ms",
            {
                "status": "PASSED",
                "correctness": {
                    "allclose_passed": True,
                    "max_abs_error": 0.0,
                    "max_relative_error": 0.0,
                },
                "performance": {
                    "speedup_factor": 1.5,
                    "latency_ms": -0.01,
                    "reference_latency_ms": 0.3,
                },
            },
        ),
        (
            "inf_ref_latency_ms",
            {
                "status": "PASSED",
                "correctness": {
                    "allclose_passed": True,
                    "max_abs_error": 0.0,
                    "max_relative_error": 0.0,
                },
                "performance": {
                    "speedup_factor": 1.5,
                    "latency_ms": 0.2,
                    "reference_latency_ms": float("inf"),
                },
            },
        ),
        (
            "neg_ref_latency_ms",
            {
                "status": "PASSED",
                "correctness": {
                    "allclose_passed": True,
                    "max_abs_error": 0.0,
                    "max_relative_error": 0.0,
                },
                "performance": {
                    "speedup_factor": 1.5,
                    "latency_ms": 0.2,
                    "reference_latency_ms": -0.5,
                },
            },
        ),
        (
            "bool_max_abs_true",
            {
                "status": "PASSED",
                "correctness": {
                    "allclose_passed": True,
                    "max_abs_error": True,
                    "max_relative_error": 0.0,
                },
                "performance": {"speedup_factor": 1.5},
            },
        ),
        (
            "bool_max_rel_false",
            {
                "status": "PASSED",
                "correctness": {
                    "allclose_passed": True,
                    "max_abs_error": 0.0,
                    "max_relative_error": False,
                },
                "performance": {"speedup_factor": 1.5},
            },
        ),
        (
            "bool_speedup_true",
            {
                "status": "PASSED",
                "correctness": {
                    "allclose_passed": True,
                    "max_abs_error": 0.0,
                    "max_relative_error": 0.0,
                },
                "performance": {"speedup_factor": True},
            },
        ),
        (
            "bool_latency_false",
            {
                "status": "PASSED",
                "correctness": {
                    "allclose_passed": True,
                    "max_abs_error": 0.0,
                    "max_relative_error": 0.0,
                },
                "performance": {
                    "speedup_factor": 1.5,
                    "latency_ms": False,
                    "reference_latency_ms": 0.3,
                },
            },
        ),
        (
            "bool_ref_latency_true",
            {
                "status": "PASSED",
                "correctness": {
                    "allclose_passed": True,
                    "max_abs_error": 0.0,
                    "max_relative_error": 0.0,
                },
                "performance": {
                    "speedup_factor": 1.5,
                    "latency_ms": 0.2,
                    "reference_latency_ms": True,
                },
            },
        ),
    ]

    in_dir, out_dir, _, _ = _build_forge_dirs(tmp_path / "sanitized_eval")
    for case_name, bad_eval in invalid_evaluations:
        rep_path = tmp_path / f"report_{case_name}.json"
        res = verify_forge_output(
            input_dir=in_dir,
            output_dir=out_dir,
            report_path=rep_path,
            min_speedup_ratio=0.5,
            executor=lambda *_args, _ev=bad_eval: _ev,
        )
        assert res.overall_status == "FAILED", case_name
        assert res.passed_count == 0, case_name
        assert res.failed_count == 1, case_name
        parsed = _assert_rfc8259_report(rep_path)
        assert parsed["overallStatus"] == "FAILED", case_name
        assert parsed["entries"][0]["passed"] is False, case_name

    # Verify non-finite values in entry evaluation were replaced with null in serialized JSON.
    nan_rep = _assert_rfc8259_report(tmp_path / "report_nan_max_abs.json")
    assert nan_rep["entries"][0]["evaluation"]["correctness"]["max_abs_error"] is None
    inf_rep = _assert_rfc8259_report(tmp_path / "report_inf_speedup.json")
    assert inf_rep["entries"][0]["evaluation"]["performance"]["speedup_factor"] is None

    # 2. Omitted (or None) allclose_passed matches Go ValidateEvaluation: fails iff max_abs > atol and max_rel > rtol.
    omitted_cases: list[tuple[str, dict[str, Any], str]] = [
        (
            "abs_within_atol_rel_above_rtol",
            {"max_abs_error": 0.005, "max_relative_error": 0.50},
            "PASSED",
        ),
        (
            "abs_above_atol_rel_within_rtol",
            {"max_abs_error": 0.05, "max_relative_error": 0.005},
            "PASSED",
        ),
        (
            "explicit_none_allclose_abs_within_atol",
            {
                "allclose_passed": None,
                "max_abs_error": 0.005,
                "max_relative_error": 0.50,
            },
            "PASSED",
        ),
        (
            "both_exceeding_atol_and_rtol",
            {"max_abs_error": 0.05, "max_relative_error": 0.05},
            "FAILED",
        ),
    ]
    for case_name, correctness_dict, want_status in omitted_cases:
        rep_path = tmp_path / f"omitted_{case_name}.json"
        eval_payload = {
            "status": "PASSED",
            "correctness": correctness_dict,
            "performance": {
                "speedup_factor": 1.5,
                "latency_ms": 0.2,
                "reference_latency_ms": 0.3,
            },
        }
        res = verify_forge_output(
            input_dir=in_dir,
            output_dir=out_dir,
            report_path=rep_path,
            executor=lambda *_args, _ev=eval_payload: _ev,
        )
        assert res.overall_status == want_status, case_name
        parsed = _assert_rfc8259_report(rep_path)
        assert parsed["overallStatus"] == want_status, case_name
