"""The acceptance runner wired for --backend local --engine vllm --suite model-load."""

import json
import os
import tempfile
import unittest
from contextlib import redirect_stdout
from datetime import datetime, timezone
from io import StringIO
from pathlib import Path

import fixtureutil

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


def load_context(config):
    from gpustack_model_transfer.acceptance.config import load_and_validate

    tmp, path = write_config(config)
    try:
        return load_and_validate(path, "local", "model-load", "vllm", now=FIXED_NOW)
    finally:
        tmp.cleanup()


def reference_factory(**kwargs):
    from gpustack_model_transfer.adapters.vllm.runtime import ReferenceRuntime

    def factory():
        return ReferenceRuntime(fixtureutil.manifest_dir(), **kwargs)

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
    def test_every_capability_passes(self):
        from gpustack_model_transfer.acceptance import model_load_suite

        context = load_context(model_load_config())
        results = model_load_suite.run(context, runtime=reference_factory())
        self.assertTrue(
            all(r.detail.startswith("reference fixture only:") for r in results)
        )
        statuses = {r.capability: r.status for r in results}
        for capability in MODEL_LOAD_CAPABILITIES:
            self.assertEqual(
                statuses.get(capability), "PASS", (capability, statuses.get(capability))
            )
        self.assertEqual(len(results), len(MODEL_LOAD_CAPABILITIES))

    def test_a_failing_check_is_no_go_not_a_skip(self):
        from gpustack_model_transfer.acceptance import model_load_suite

        config = model_load_config()
        config["limits"]["window_bytes"] = 32
        context = load_context(config)

        def broken_factory():
            from gpustack_model_transfer.adapters.vllm.runtime import ReferenceRuntime

            return ReferenceRuntime(fixtureutil.manifest_dir(), corrupt_norm=True)

        results = model_load_suite.run(context, runtime=broken_factory)
        statuses = {r.capability: r.status for r in results}
        self.assertIn(
            "NO-GO", statuses.values(), "a broken load must be visible as NO-GO"
        )


class TestFailClosedOnThisCpuHost(unittest.TestCase):
    def test_missing_prerequisites_report_unrun_and_no_go(self):
        # No torch, no vllm, no CUDA on this checkout: nothing may pass and
        # nothing may be skipped-as-pass. Every capability is UNRUN and the
        # run is NO-GO with exit 1.
        _code, out = run_main(model_load_config(), "vllm", expect=1)
        report = json.loads(out)
        self.assertEqual(report["overall"], "NO-GO")
        self.assertEqual(report["summary"]["unrun"], len(MODEL_LOAD_CAPABILITIES))
        self.assertEqual(report["summary"]["pass"], 0)
        self.assertEqual(report["engine"], "vllm")
        for result in report["results"]:
            self.assertEqual(result["status"], "UNRUN", result)

    def test_suite_context_loads_for_vllm(self):
        context = load_context(model_load_config())
        self.assertEqual(context["manifest"].contract_version, 2)
        self.assertEqual(context["config"]["suite"], "model-load")


class TestOtherEnginesStayNotImplemented(unittest.TestCase):
    def test_vllm_ascend_is_implemented_and_fails_closed_here(self):
        # vllm-ascend is implemented for model-load now: on this CPU-only
        # host its run fails closed with every capability UNRUN and exit 1.
        _code, out = run_main(model_load_config(), "vllm-ascend", expect=1)
        report = json.loads(out)
        self.assertEqual(report["overall"], "NO-GO")
        self.assertEqual(report["summary"]["unrun"], len(MODEL_LOAD_CAPABILITIES))
        self.assertEqual(report["engine"], "vllm-ascend")

    def test_sglang_is_implemented_and_fails_closed_here(self):
        # sglang is implemented for model-load now: on this CPU-only host
        # its run still fails closed — every capability UNRUN, exit 1 —
        # never a skipped pass. The sglang route carries one capability
        # beyond this suite's vLLM set: the load group.
        _code, out = run_main(model_load_config(), "sglang", expect=1)
        report = json.loads(out)
        self.assertEqual(report["overall"], "NO-GO")
        self.assertEqual(report["summary"]["unrun"], len(MODEL_LOAD_CAPABILITIES) + 1)
        self.assertEqual(report["engine"], "sglang")

    def test_modelexpress_is_implemented_and_fails_closed_here(self):
        # The optional ModelExpress path is wired now; on this CPU-only
        # host its run still fails closed with every capability UNRUN.
        _code, out = run_main(model_load_config(), "modelexpress", expect=1)
        report = json.loads(out)
        self.assertEqual(report["overall"], "NO-GO")
        self.assertGreater(report["summary"]["unrun"], 0)

    def test_a_remote_backend_without_its_runtime_is_unrun_not_local(self):
        # --backend nixl must not silently degrade to the local reader:
        # on this host the nixl runtime is missing, so every capability
        # UNRUNs naming the backend prerequisites.
        _code, out = run_main(model_load_config(), "vllm", expect=1, backend="nixl")
        report = json.loads(out)
        self.assertEqual(report["overall"], "NO-GO")
        self.assertEqual(report["summary"]["unrun"], len(MODEL_LOAD_CAPABILITIES))
        self.assertEqual(report["summary"]["pass"], 0)
        for result in report["results"]:
            self.assertIn("missing backend prerequisites (nixl)", result["detail"])

    def test_an_unqualified_backend_engine_pair_is_unrun(self):
        # vLLM is a CUDA engine: hixl does not carry it, and the run says
        # so instead of reading local files under the hixl name.
        _code, out = run_main(model_load_config(), "vllm", expect=1, backend="hixl")
        report = json.loads(out)
        for result in report["results"]:
            self.assertIn("not qualified for backend", result["detail"])
            self.assertIn("hixl/vllm-ascend", result["detail"])

    def test_an_injected_runtime_keeps_its_behavior_under_any_backend(self):
        # The gate is a real-host probe only: an injected runtime factory
        # runs the checks unchanged, whatever the requested backend.
        from gpustack_model_transfer.acceptance import model_load_suite
        from gpustack_model_transfer.acceptance.config import load_and_validate

        config = model_load_config()
        config["backend"] = "nixl"
        tmp, path = write_config(config)
        try:
            context = load_and_validate(
                path, "nixl", "model-load", "vllm", now=FIXED_NOW
            )
        finally:
            tmp.cleanup()
        results = model_load_suite.run(
            context, runtime=reference_factory(), engine="vllm", backend="nixl"
        )
        self.assertTrue(results)
        self.assertEqual({r.status for r in results}, {"PASS"})

    def test_engine_is_required_for_model_load(self):
        _code, out = run_main(model_load_config(), None, expect=2)
        self.assertIn("engine", out.lower())

    def test_config_suite_must_match(self):
        config = model_load_config()
        config["suite"] = "contract"
        _code, out = run_main(config, "vllm", expect=2)
        self.assertIn("suite", out.lower())


if __name__ == "__main__":
    unittest.main()
