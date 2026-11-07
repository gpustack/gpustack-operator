"""Fail-closed acceptance runner: config validation and PASS/NO-GO output."""

import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from contextlib import redirect_stdout
from datetime import datetime, timezone
from io import StringIO
from pathlib import Path

import fixtureutil

from gpustack_model_transfer.acceptance.__main__ import main

FIXTURE_DIR = fixtureutil.fixture_dir()
DIGEST = fixtureutil.manifest_digest()
MODEL_TRANSFER_ROOT = Path(__file__).resolve().parents[2]
# Fixed clock so expiry comparisons never depend on the real date.
FIXED_NOW = datetime(2030, 1, 1, tzinfo=timezone.utc)

EXPECTED_CAPABILITIES = [
    "version-identity",
    "identity-binding",
    "descriptor-validation",
    "bounds-overflow",
    "owned-outputs",
    "operation-completion",
    "cancel-release",
    "capabilities",
    "fixture-verification",
]


def base_config():
    return {
        "schema_version": 1,
        "backend": "local",
        "suite": "contract",
        "artifact": {
            "manifest_path": str(FIXTURE_DIR / "manifest.json"),
            "manifest_sha256": DIGEST,
        },
        "readiness": {
            "source_generation": 1,
            "expires": "2031-01-01T00:00:00+00:00",
        },
        "limits": {
            "window_bytes": 32,
            "max_inflight": 2,
            "owned_output_bytes": 4096,
        },
        "deadlines": {"operation_seconds": 30},
    }


def run(config, backend="local", suite="contract", engine=None, expect=None):
    with tempfile.TemporaryDirectory() as tmp:
        path = os.path.join(tmp, "acceptance.json")
        with open(path, "w") as fh:
            json.dump(config, fh)
        argv = ["--config", path, "--backend", backend, "--suite", suite]
        if engine:
            argv += ["--engine", engine]
        out = StringIO()
        with redirect_stdout(out):
            code = main(argv, now=FIXED_NOW)
    if expect is not None:
        assert code == expect, (code, out.getvalue())
    return code, out.getvalue()


class TestAcceptanceRefusals(unittest.TestCase):
    def test_nonfinite_deadlines_are_refused(self):
        for value in (float("nan"), float("inf"), float("-inf")):
            with self.subTest(value=value):
                config = base_config()
                config["deadlines"]["operation_seconds"] = value
                run(config, expect=2)

    def test_valid_local_contract_run_passes(self):
        _code, out = run(base_config(), expect=0)
        report = json.loads(out)
        self.assertEqual(report["overall"], "PASS")
        self.assertEqual(report["backend"], "local")
        self.assertEqual(report["suite"], "contract")
        names = [r["capability"] for r in report["results"]]
        for capability in EXPECTED_CAPABILITIES:
            self.assertIn(capability, names)
        for result in report["results"]:
            self.assertEqual(result["status"], "PASS", result)

    def test_missing_config_argument_refused(self):
        with redirect_stdout(StringIO()):
            code = main(["--backend", "local", "--suite", "contract"])
        self.assertEqual(code, 2)

    def test_unknown_backend_refused(self):
        _code, out = run(base_config(), backend="quantum", expect=2)
        self.assertIn("backend", out.lower())

    def test_unimplemented_backend_refused_with_reason(self):
        # nixl is a known, planned backend; the runner refuses it explicitly
        # instead of reporting a skip.
        cfg = base_config()
        cfg["backend"] = "nixl"
        _code, out = run(cfg, backend="nixl", expect=3)
        self.assertIn("not implemented", out)

    def test_unknown_suite_refused(self):
        _code, out = run(base_config(), suite="moonshot", expect=2)
        self.assertIn("suite", out.lower())

    def test_engine_not_applicable_to_contract_suite(self):
        _code, out = run(base_config(), engine="vllm", expect=2)
        self.assertIn("engine", out.lower())

    def test_missing_section_refused(self):
        for section in ["artifact", "readiness", "limits", "deadlines"]:
            with self.subTest(section=section):
                cfg = base_config()
                del cfg[section]
                run(cfg, expect=2)

    def test_unknown_field_refused(self):
        cfg = base_config()
        cfg["turbo"] = True
        run(cfg, expect=2)
        cfg = base_config()
        cfg["limits"]["extra"] = 1
        run(cfg, expect=2)

    def test_wrong_schema_version_refused(self):
        cfg = base_config()
        cfg["schema_version"] = 2
        run(cfg, expect=2)

    def test_stale_readiness_refused(self):
        cfg = base_config()
        cfg["readiness"]["expires"] = "2020-01-01T00:00:00+00:00"
        _code, out = run(cfg, expect=2)
        self.assertIn("stale", out)

    def test_generation_mismatch_refused(self):
        cfg = base_config()
        cfg["readiness"]["source_generation"] = 5
        _code, out = run(cfg, expect=2)
        self.assertIn("generation", out.lower())

    def test_manifest_digest_mismatch_refused(self):
        cfg = base_config()
        cfg["artifact"]["manifest_sha256"] = "0" * 64
        _code, out = run(cfg, expect=2)
        self.assertIn("manifest", out.lower())

    def test_naive_timestamp_without_timezone_refused(self):
        cfg = base_config()
        cfg["readiness"]["expires"] = "2031-01-01T00:00:00"
        run(cfg, expect=2)

    def test_readiness_freshness_uses_the_injected_clock(self):
        # Expires 2029 is in the future for the real clock but stale for a
        # fixed now in 2030, and fresh again for a fixed now in 2028.
        cfg = base_config()
        cfg["readiness"]["expires"] = "2029-06-01T00:00:00+00:00"
        code, out = run(cfg, expect=2)
        self.assertIn("stale", out)
        out = StringIO()
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "acceptance.json")
            with open(path, "w") as fh:
                json.dump(cfg, fh)
            with redirect_stdout(out):
                code = main(
                    ["--config", path, "--backend", "local", "--suite", "contract"],
                    now=datetime(2028, 1, 1, tzinfo=timezone.utc),
                )
        self.assertEqual(code, 0, out.getvalue())

    def test_cli_now_flag_refuses_invalid_values(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "acceptance.json")
            with open(path, "w") as fh:
                json.dump(base_config(), fh)
            out = StringIO()
            with redirect_stdout(out):
                code = main(
                    [
                        "--config",
                        path,
                        "--backend",
                        "local",
                        "--suite",
                        "contract",
                        "--now",
                        "not-a-time",
                    ]
                )
            self.assertEqual(code, 2)
            out = StringIO()
            with redirect_stdout(out):
                code = main(
                    [
                        "--config",
                        path,
                        "--backend",
                        "local",
                        "--suite",
                        "contract",
                        "--now",
                        "2030-01-01T00:00:00",
                    ]
                )
            self.assertEqual(code, 2)


class TestAcceptanceExecution(unittest.TestCase):
    def test_truncated_fixture_reports_no_go(self):
        # A truncated published file must fail the extent check.
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            shutil.copytree(FIXTURE_DIR, root, dirs_exist_ok=True)
            blob = bytearray((root / "checkpoint.safetensors").read_bytes())
            del blob[-1]
            (root / "checkpoint.safetensors").write_bytes(bytes(blob))
            cfg = base_config()
            cfg["artifact"]["manifest_path"] = str(root / "manifest.json")
            # manifest.json bytes are unchanged, so its digest still matches.
            _code, out = run(cfg, expect=1)
            report = json.loads(out)
            self.assertEqual(report["overall"], "NO-GO")
            statuses = {r["capability"]: r["status"] for r in report["results"]}
            self.assertEqual(statuses["fixture-verification"], "NO-GO")
            self.assertTrue(any(r["status"] == "NO-GO" for r in report["results"]))

    def test_module_entry_runs_as_python_m(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "acceptance.json")
            with open(path, "w") as fh:
                json.dump(base_config(), fh)
            env = dict(os.environ, PYTHONPATH=str(MODEL_TRANSFER_ROOT))
            proc = subprocess.run(
                [
                    sys.executable,
                    "-m",
                    "gpustack_model_transfer.acceptance",
                    "--config",
                    path,
                    "--backend",
                    "local",
                    "--suite",
                    "contract",
                    "--now",
                    "2030-01-01T00:00:00+00:00",
                ],
                capture_output=True,
                text=True,
                env=env,
                timeout=120,
            )
            self.assertEqual(proc.returncode, 0, proc.stderr)
            report = json.loads(proc.stdout)
            self.assertEqual(report["overall"], "PASS")


if __name__ == "__main__":
    unittest.main()
