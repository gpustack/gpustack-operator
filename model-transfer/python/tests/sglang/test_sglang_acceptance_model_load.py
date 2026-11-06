"""The acceptance runner wired for --backend local --engine sglang."""

import json
import os
import tempfile
import unittest
from contextlib import redirect_stdout
from datetime import datetime, timezone
from io import StringIO
from pathlib import Path

import sglangutil as fixtureutil

from gpustack_model_transfer.acceptance.__main__ import main

MODEL_TRANSFER_ROOT = Path(__file__).resolve().parents[2]
FIXED_NOW = datetime(2030, 1, 1, tzinfo=timezone.utc)

MODEL_LOAD_CAPABILITIES = [
    "extension-route",
    "metadata-probe",
    "multi-window-assembly",
    "owned-budget-refusal",
    "tied-tensors-and-dtype",
    "finalization-once",
    "borrowed-visibility-fence",
    "pre-kv-profile-release",
    "nixl-kv-connector-coexistence",
    "forward-equivalence",
    "tp-sharding-load-group",
]


def model_load_config():
    return {
        "schema_version": 1,
        "backend": "local",
        "suite": "model-load",
        "artifact": {
            "manifest_path": str(fixtureutil.manifest_path()),
            "manifest_sha256": fixtureutil.manifest_digest(),
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


def write_config(config):
    tmp = tempfile.TemporaryDirectory()
    path = os.path.join(tmp.name, "acceptance.json")
    with open(path, "w") as fh:
        json.dump(config, fh)
    return tmp, path


def load_context(config, engine="sglang"):
    from gpustack_model_transfer.acceptance.config import load_and_validate

    tmp, path = write_config(config)
    try:
        return load_and_validate(path, "local", "model-load", engine, now=FIXED_NOW)
    finally:
        tmp.cleanup()


def reference_factory():
    from gpustack_model_transfer.adapters.sglang.runtime import ReferenceRuntime

    def factory():
        return ReferenceRuntime(fixtureutil.manifest_dir())

    return factory


def run_main(config, engine, expect=None, backend="local"):
    config["backend"] = backend
    tmp, path = write_config(config)
    try:
        argv = ["--config", path, "--backend", backend, "--suite", "model-load"]
        if engine is not None:
            argv += ["--engine", engine]
        out = StringIO()
        with redirect_stdout(out):
            code = main(argv, now=FIXED_NOW)
    finally:
        tmp.cleanup()
    if expect is not None:
        assert code == expect, (code, out.getvalue())
    return code, out.getvalue()


class TestModelLoadSuiteAgainstTheReferenceRuntime(unittest.TestCase):
    def test_every_capability_passes_for_sglang(self):
        from gpustack_model_transfer.acceptance import model_load_suite

        context = load_context(model_load_config())
        results = model_load_suite.run(
            context, runtime=reference_factory(), engine="sglang"
        )
        statuses = {r.capability: r.status for r in results}
        for capability in MODEL_LOAD_CAPABILITIES:
            self.assertEqual(
                statuses.get(capability), "PASS", (capability, statuses.get(capability))
            )
        self.assertEqual(len(results), len(MODEL_LOAD_CAPABILITIES))

    def test_vllm_still_passes_its_own_capability_set(self):
        from gpustack_model_transfer.acceptance import model_load_suite

        context = load_context(model_load_config(), engine="vllm")
        results = model_load_suite.run(context, runtime=vllm_factory(), engine="vllm")
        for result in results:
            self.assertEqual(result.status, "PASS", result)

    def test_a_failing_check_is_no_go_not_a_skip(self):
        from gpustack_model_transfer.acceptance import model_load_suite

        context = load_context(model_load_config())

        def broken_factory():
            from gpustack_model_transfer.adapters.sglang.runtime import ReferenceRuntime

            return ReferenceRuntime(fixtureutil.manifest_dir(), corrupt_norm=True)

        results = model_load_suite.run(context, runtime=broken_factory, engine="sglang")
        statuses = {r.status for r in results}
        self.assertIn("NO-GO", statuses)


class TestFailClosedOnThisCpuHost(unittest.TestCase):
    def test_missing_prerequisites_report_unrun_and_no_go(self):
        _code, out = run_main(model_load_config(), "sglang", expect=1)
        report = json.loads(out)
        self.assertEqual(report["overall"], "NO-GO")
        self.assertEqual(report["summary"]["unrun"], len(MODEL_LOAD_CAPABILITIES))
        self.assertEqual(report["summary"]["pass"], 0)
        self.assertEqual(report["engine"], "sglang")
        for result in report["results"]:
            self.assertEqual(result["status"], "UNRUN", result)

    def test_a_remote_backend_without_its_runtime_is_unrun_not_local(self):
        # --backend nixl must not silently degrade to the local reader:
        # the nixl runtime is missing here, so every capability UNRUNs
        # naming the backend prerequisites.
        _code, out = run_main(model_load_config(), "sglang", expect=1, backend="nixl")
        report = json.loads(out)
        self.assertEqual(report["summary"]["unrun"], len(MODEL_LOAD_CAPABILITIES))
        self.assertEqual(report["summary"]["pass"], 0)
        for result in report["results"]:
            self.assertIn("missing backend prerequisites (nixl)", result["detail"])

    def test_an_unqualified_backend_engine_pair_is_unrun(self):
        # SGLang is a CUDA engine: hixl does not carry it.
        _code, out = run_main(model_load_config(), "sglang", expect=1, backend="hixl")
        report = json.loads(out)
        for result in report["results"]:
            self.assertIn("not qualified for backend", result["detail"])

    def test_modelexpress_fails_closed_on_this_host(self):
        # ModelExpress is implemented for model-load now: its run on this
        # CPU-only host fails closed with every capability UNRUN.
        _code, out = run_main(model_load_config(), "modelexpress", expect=1)
        report = json.loads(out)
        self.assertEqual(report["overall"], "NO-GO")
        self.assertGreater(report["summary"]["unrun"], 0)


def vllm_factory():
    from gpustack_model_transfer.adapters.vllm.runtime import (
        ReferenceRuntime as VllmRuntime,
    )

    def factory():
        return VllmRuntime(fixtureutil.manifest_dir())

    return factory


if __name__ == "__main__":
    unittest.main()
