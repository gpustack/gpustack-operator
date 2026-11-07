"""The acceptance suite wired for --backend local --engine vllm-ascend."""

import json
import os
import tempfile
import unittest
from contextlib import redirect_stdout
from datetime import datetime, timezone
from io import StringIO

import ascendutil as fixtureutil

from gpustack_model_transfer.acceptance.__main__ import main

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


def load_context(config, engine="vllm-ascend"):
    from gpustack_model_transfer.acceptance.config import load_and_validate

    tmp, path = write_config(config)
    try:
        return load_and_validate(path, "local", "model-load", engine, now=FIXED_NOW)
    finally:
        tmp.cleanup()


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
    def test_every_capability_passes_for_vllm_ascend(self):
        from gpustack_model_transfer.acceptance import model_load_suite
        from gpustack_model_transfer.adapters.vllm_ascend.runtime import (
            ReferenceRuntime,
        )

        context = load_context(model_load_config())
        results = model_load_suite.run(
            context,
            runtime=lambda: ReferenceRuntime(fixtureutil.manifest_dir()),
            engine="vllm-ascend",
        )
        self.assertTrue(
            all(r.detail.startswith("reference fixture only:") for r in results)
        )
        statuses = {r.capability: r.status for r in results}
        for capability in MODEL_LOAD_CAPABILITIES:
            self.assertEqual(
                statuses.get(capability), "PASS", (capability, statuses.get(capability))
            )
        self.assertEqual(len(results), len(MODEL_LOAD_CAPABILITIES))

    def test_the_borrowed_stream_is_the_npu_stream(self):
        # The suite must fence on the engine's own stream, not a CUDA one
        # that happens to be another engine's default.
        from gpustack_model_transfer.adapters.vllm_ascend import (
            DEFAULT_VISIBILITY_STREAM,
        )
        from gpustack_model_transfer.adapters.vllm_ascend.constants import (
            CONNECTOR_ENV_KEYS,
        )

        self.assertEqual(DEFAULT_VISIBILITY_STREAM, "npu:0")
        self.assertIn("ASCEND_RT_VISIBLE_DEVICES", CONNECTOR_ENV_KEYS)
        self.assertIn("HIXL_BACKEND", CONNECTOR_ENV_KEYS)

    def test_a_failing_check_is_no_go_not_a_skip(self):
        from gpustack_model_transfer.acceptance import model_load_suite
        from gpustack_model_transfer.adapters.vllm_ascend.runtime import (
            ReferenceRuntime,
        )

        context = load_context(model_load_config())
        results = model_load_suite.run(
            context,
            runtime=lambda: ReferenceRuntime(
                fixtureutil.manifest_dir(), corrupt_norm=True
            ),
            engine="vllm-ascend",
        )
        self.assertIn("NO-GO", {r.status for r in results})


class TestFailClosedOnThisCpuHost(unittest.TestCase):
    def test_missing_prerequisites_report_unrun_and_no_go(self):
        # No torch, no torch_npu, no vllm_ascend on this checkout: nothing
        # may pass and nothing may be skipped-as-pass.
        _code, out = run_main(model_load_config(), "vllm-ascend", expect=1)
        report = json.loads(out)
        self.assertEqual(report["overall"], "NO-GO")
        self.assertEqual(report["summary"]["unrun"], len(MODEL_LOAD_CAPABILITIES))
        self.assertEqual(report["summary"]["pass"], 0)
        self.assertEqual(report["engine"], "vllm-ascend")

    def test_a_remote_backend_without_its_runtime_is_unrun_not_local(self):
        # --backend hixl must not silently degrade to the local reader:
        # on this host the HIXL stack is missing, so every capability
        # UNRUNs naming the backend prerequisites.
        _code, out = run_main(
            model_load_config(), "vllm-ascend", expect=1, backend="hixl"
        )
        report = json.loads(out)
        self.assertEqual(report["summary"]["pass"], 0)
        for result in report["results"]:
            self.assertIn("missing backend prerequisites (hixl)", result["detail"])

    def test_an_unqualified_backend_engine_pair_is_unrun(self):
        # vLLM-Ascend is the CANN engine: nixl does not carry it.
        _code, out = run_main(
            model_load_config(), "vllm-ascend", expect=1, backend="nixl"
        )
        report = json.loads(out)
        for result in report["results"]:
            self.assertIn("not qualified for backend", result["detail"])
            self.assertIn("hixl/vllm-ascend", result["detail"])

    def test_modelexpress_fails_closed_on_this_host(self):
        # ModelExpress is implemented for model-load now: its run on this
        # CPU-only host fails closed with every capability UNRUN.
        _code, out = run_main(model_load_config(), "modelexpress", expect=1)
        report = json.loads(out)
        self.assertEqual(report["overall"], "NO-GO")
        self.assertGreater(report["summary"]["unrun"], 0)


if __name__ == "__main__":
    unittest.main()
