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
        input_dtypes=["bfloat16"],
        output_dtypes=["bfloat16"],
    )

    (in_dir / "forge-request.json").write_text(
        json.dumps(
            {
                "minSpeedupRatio": min_speedup_ratio,
                "atol": atol,
                "rtol": rtol,
                "targetKeys": [key],
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
                    "x": {"shape": ["num_tokens", "hidden_size"], "dtype": "bfloat16"}
                },
                "reference": "def run(x): return x",
            }
        ),
        encoding="utf-8",
    )
    (in_dir / "workloads" / "rmsnorm_4096.jsonl").write_text(
        json.dumps(
            {
                "uuid": "wl-1",
                "definition": "rmsnorm_4096",
                "axes": {"num_tokens": 64},
            }
        )
        + "\n",
        encoding="utf-8",
    )

    lib_file = out_dir / "kernels" / "rmsnorm_v1" / "kernel.py"
    lib_file.write_text("def run(x):\n    return x\n", encoding="utf-8")
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
                "correctness": {"max_abs_error": 0.05, "max_relative_error": 0.001},
                "performance": {"speedup_factor": 1.5},
            },
            "exceeds atol",
        ),
        (
            {
                "status": "PASSED",
                "correctness": {"max_abs_error": 0.001, "max_relative_error": 0.05},
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
        input_dtypes=["bfloat16"],
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


def test_forge_verifier_default_executor_with_fake_torch_and_mixed_entries(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    from types import ModuleType

    # Minimal pure-Python fake torch module to exercise _default_live_gpu_executor without ML deps.
    class _FakeTensor:
        def __init__(self, vals: list[float]) -> None:
            self._vals = vals

        def float(self) -> _FakeTensor:
            return self

        def __sub__(self, other: _FakeTensor) -> _FakeTensor:
            return _FakeTensor(
                [a - b for a, b in zip(self._vals, other._vals, strict=True)]
            )

        def __truediv__(self, other: _FakeTensor) -> _FakeTensor:
            return _FakeTensor(
                [a / b for a, b in zip(self._vals, other._vals, strict=True)]
            )

        def abs(self) -> _FakeTensor:
            return _FakeTensor([abs(v) for v in self._vals])

        def clamp_min(self, floor: float) -> _FakeTensor:
            return _FakeTensor([max(v, floor) for v in self._vals])

        def numel(self) -> int:
            return len(self._vals)

        def max(self) -> _FakeTensor:
            return _FakeTensor([max(self._vals)])

        def item(self) -> float:
            return float(self._vals[0])

    fake_torch = ModuleType("torch")
    fake_torch.float16 = "float16"  # type: ignore[attr-defined]
    fake_torch.bfloat16 = "bfloat16"  # type: ignore[attr-defined]
    fake_torch.float32 = "float32"  # type: ignore[attr-defined]
    fake_torch.float64 = "float64"  # type: ignore[attr-defined]
    fake_torch.cuda = type(  # type: ignore[attr-defined]
        "_Cuda", (), {"is_available": staticmethod(lambda: False)}
    )()
    fake_torch.randn = lambda shape, device=None, dtype=None: _FakeTensor(
        [1.0, 2.0, 3.0]
    )  # type: ignore[attr-defined]
    monkeypatch.setitem(sys.modules, "torch", fake_torch)

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
    assert res.entries[0]["evaluation"]["correctness"]["max_abs_error"] == 0.0


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
    assert res_mixed.overall_status == "PASSED"
    assert res_mixed.passed_count == 1
    assert res_mixed.failed_count == 1
    assert res_mixed.entries[0]["localKernelKey"] == key
