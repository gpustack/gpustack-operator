"""The loading group, the publication gate, and the runtime standing."""

import json
import subprocess
import sys
import unittest
from pathlib import Path

import mxutil

from gpustack_model_transfer.acceptance.__main__ import main
from gpustack_model_transfer.adapters.modelexpress import (
    STRATEGY_NAME,
    GroupDisagreementError,
    LoadGroupCoordinator,
    PluginResult,
    PublicationRefusedError,
    RankProposal,
    WriterRefitFence,
    checkpoint_identity,
    publication_allowed,
    refuse_publication,
)
from gpustack_model_transfer.adapters.modelexpress.runtime import (
    probe_entry,
)


class TestLoadGroupCoordinator(unittest.TestCase):
    def test_agreeing_ranks_proceed(self):
        coordinator = LoadGroupCoordinator(expected_size=2)
        coordinator.propose(RankProposal(0, True, STRATEGY_NAME))
        coordinator.propose(RankProposal(1, True, STRATEGY_NAME))
        agreed = coordinator.agree()
        self.assertEqual(agreed.chosen_strategy, STRATEGY_NAME)
        self.assertTrue(agreed.backend_available)

    def test_a_missing_rank_is_a_named_disagreement(self):
        coordinator = LoadGroupCoordinator(expected_size=2)
        coordinator.propose(RankProposal(0, True, STRATEGY_NAME))
        with self.assertRaises(GroupDisagreementError) as caught:
            coordinator.agree()
        self.assertIn("missing ranks [1]", str(caught.exception))

    def test_backend_disagreement_is_refused(self):
        coordinator = LoadGroupCoordinator(expected_size=2)
        coordinator.propose(RankProposal(0, True, STRATEGY_NAME))
        coordinator.propose(RankProposal(1, False, STRATEGY_NAME))
        with self.assertRaises(GroupDisagreementError) as caught:
            coordinator.agree()
        self.assertIn("backend availability", str(caught.exception))

    def test_strategy_disagreement_is_refused(self):
        coordinator = LoadGroupCoordinator(expected_size=2)
        coordinator.propose(RankProposal(0, True, STRATEGY_NAME))
        coordinator.propose(RankProposal(1, True, "native"))
        with self.assertRaises(GroupDisagreementError) as caught:
            coordinator.agree()
        self.assertIn("chosen strategy", str(caught.exception))

    def test_a_rank_cannot_propose_twice(self):
        coordinator = LoadGroupCoordinator(expected_size=1)
        coordinator.propose(RankProposal(0, True, STRATEGY_NAME))
        with self.assertRaises(GroupDisagreementError):
            coordinator.propose(RankProposal(0, True, STRATEGY_NAME))

    def test_a_group_of_zero_ranks_is_refused_at_construction(self):
        with self.assertRaises(ValueError) as caught:
            LoadGroupCoordinator(expected_size=0)
        self.assertIn("at least one rank", str(caught.exception))

    def test_a_rank_outside_the_group_is_refused_at_proposal(self):
        coordinator = LoadGroupCoordinator(expected_size=2)
        with self.assertRaises(GroupDisagreementError) as caught:
            coordinator.propose(RankProposal(2, True, STRATEGY_NAME))
        self.assertIn("outside the loading group", str(caught.exception))
        self.assertIn("0..1", str(caught.exception))

    def test_a_holed_rank_set_is_a_named_disagreement(self):
        # Two proposals for a two-rank group, but rank 1 is missing and
        # rank 2 never belonged: the count alone would wave this through.
        coordinator = LoadGroupCoordinator(expected_size=2)
        coordinator.proposals.extend(
            [RankProposal(0, True, STRATEGY_NAME), RankProposal(2, True, STRATEGY_NAME)]
        )
        with self.assertRaises(GroupDisagreementError) as caught:
            coordinator.agree()
        self.assertIn("missing ranks [1]", str(caught.exception))
        self.assertIn("unexpected ranks [2]", str(caught.exception))


class TestPublicationGate(unittest.TestCase):
    def test_an_unpublished_result_is_never_a_stage_two_source(self):
        result = PluginResult(model=object())
        fence = WriterRefitFence()
        self.assertFalse(publication_allowed(result, fence))
        with self.assertRaises(PublicationRefusedError):
            refuse_publication(result, fence)

    def test_even_a_finished_fence_does_not_publish_a_live_model(self):
        fence = WriterRefitFence()
        fence.begin_write("rank-0")
        fence.finish("rank-0")
        self.assertFalse(publication_allowed(PluginResult(model=object()), fence))

    def test_publication_identity_is_the_verified_checkpoint_digest(self):
        identity = checkpoint_identity("sha256:" + "a" * 64)
        self.assertEqual(identity["kind"], "verified-checkpoint")
        self.assertEqual(identity["manifest_digest"], "sha256:" + "a" * 64)


class TestRuntimeStanding(unittest.TestCase):
    def test_the_probe_reports_a_host_without_modelexpress(self):
        readiness = probe_entry(env={}, cuda_available=False)
        self.assertFalse(readiness.ready)
        self.assertIn("modelexpress", readiness.missing)
        self.assertFalse(readiness.plugin_hook_exposed)

    def test_an_unset_plugin_hook_keeps_the_entry_not_ready(self):
        # Everything but the hook may be present: without the pinned
        # insertion variable the entry has no supported route, and
        # readiness must say so rather than pass.
        from unittest.mock import patch

        with patch(
            "gpustack_model_transfer.adapters.modelexpress.runtime._importable",
            return_value=True,
        ):
            readiness = probe_entry(env={"OTHER": "1"}, cuda_available=True)
        self.assertFalse(readiness.ready)
        self.assertIn("MX_LOAD_PLUGIN", readiness.missing)
        self.assertEqual(readiness.missing, ("MX_LOAD_PLUGIN",))

    def test_the_hook_variable_marks_the_plugin_hook_exposed(self):
        readiness = probe_entry(
            env={"MX_LOAD_PLUGIN": "gpustack"}, cuda_available=False
        )
        self.assertTrue(readiness.plugin_hook_exposed)
        self.assertNotIn("MX_LOAD_PLUGIN", readiness.missing)

    def test_importing_the_adapter_needs_neither_modelexpress_norch(self):
        code = (
            "import sys, json;"
            "sys.path.insert(0, sys.argv[1]);"
            "import gpustack_model_transfer.adapters.modelexpress as m;"
            "json.dump(sorted(x for x in sys.modules"
            "    if x.split('.')[0] in ('modelexpress', 'torch')), sys.stdout)"
        )
        done = subprocess.run(
            [sys.executable, "-c", code, str(Path(__file__).resolve().parents[2])],
            capture_output=True,
            text=True,
            timeout=60,
            check=False,
        )
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertEqual(done.stdout.strip(), "[]")


def _ready_nixl():
    from gpustack_model_transfer.backends.nixl.runtime import RuntimeReadiness

    return RuntimeReadiness(
        nixl_available=True, version="1.5.0", device_module_available=True, missing=()
    )


class TestAcceptanceWiring(unittest.TestCase):
    def run_main(self):
        import os
        import tempfile
        from contextlib import redirect_stdout
        from datetime import datetime, timezone
        from io import StringIO

        config = mxutil.model_load_config()
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "acceptance.json")
            with open(path, "w") as fh:
                json.dump(config, fh)
            out = StringIO()
            with redirect_stdout(out):
                code = main(
                    [
                        "--config",
                        path,
                        "--backend",
                        "nixl",
                        "--suite",
                        "model-load",
                        "--engine",
                        "modelexpress",
                    ],
                    now=datetime(2030, 1, 1, tzinfo=timezone.utc),
                )
            captured = out.getvalue()
        return code, captured

    def test_the_engine_is_wired_and_fails_closed_without_the_entry(self):
        code, out = self.run_main()
        self.assertEqual(code, 1, out)
        report = json.loads(out)
        self.assertEqual(report["overall"], "NO-GO")
        self.assertEqual(report["engine"], "modelexpress")
        self.assertGreater(report["summary"]["unrun"], 0)
        self.assertEqual(report["summary"]["pass"], 0)

    def test_a_ready_host_with_an_unready_engine_reports_its_reason(self):
        from unittest.mock import patch

        from gpustack_model_transfer.acceptance import modelexpress_suite
        from gpustack_model_transfer.adapters.modelexpress.runtime import EntryReadiness

        ready = EntryReadiness(
            modelexpress_available=True,
            plugin_hook_exposed=True,
            torch_available=True,
            cuda_available=True,
            missing=(),
        )
        with (
            patch(
                "gpustack_model_transfer.adapters.modelexpress.runtime.probe_entry",
                return_value=ready,
            ),
            patch(
                "gpustack_model_transfer.backends.nixl.runtime.probe_runtime",
                return_value=_ready_nixl(),
            ),
            patch(
                "gpustack_model_transfer.adapters.vllm.torch_runtime.probe_host_driver",
                return_value=(None, "missing prerequisites: vllm"),
            ),
        ):
            results = modelexpress_suite.run(mxutil.load_context())
        for result in results:
            self.assertEqual(result.status, "UNRUN", result.capability)
            self.assertIn("missing prerequisites: vllm", result.detail)

    def test_an_injected_runtime_passes_the_suite_and_leaves_no_fixtures(self):
        """Every capability passes and the checks restore what they replaced."""
        import os
        from types import SimpleNamespace

        from gpustack_model_transfer.acceptance import modelexpress_suite

        native_name = "gpustack_model_transfer.adapters.vllm.native"
        native_before = sys.modules.get(native_name)
        os.environ.pop("GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY", None)

        class Driver:
            # The suite reads the finalization record the model carries.
            def model_finalization_count(self, model):
                return getattr(model, "finalization_count", 0)

        def runtime():
            return SimpleNamespace(_close_reader=lambda: None), Driver()

        results = modelexpress_suite.run({}, runtime=runtime)
        self.assertEqual(len(results), len(modelexpress_suite.CAPABILITIES))
        for result in results:
            self.assertEqual(result.status, "PASS", result.detail)
        # The stubbed native module is gone and the delivery policy env
        # the fixtures set is restored.
        self.assertIs(sys.modules.get(native_name), native_before)
        self.assertNotIn("GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY", os.environ)


if __name__ == "__main__":
    unittest.main()
