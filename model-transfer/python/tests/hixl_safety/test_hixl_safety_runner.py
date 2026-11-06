"""Fault injection at observable boundaries, and the runner's wiring."""

import json
import os
import tempfile
import unittest
from contextlib import redirect_stdout
from datetime import datetime, timezone
from io import StringIO

import hixlutil

from gpustack_model_transfer.acceptance.__main__ import main
from gpustack_model_transfer.backends.hixl import HixlReader
from gpustack_model_transfer.backends.hixl.safety import (
    NO_GO,
    PASS,
    check_pre_submission_timeout,
    check_submitted_nonterminal_quarantine,
)
from gpustack_model_transfer.backends.hixl.testing import (
    SUBMIT_NONTERMINAL,
    SUBMIT_TIMEOUT_PRE,
    FakeHixlAPI,
)
from gpustack_model_transfer.core.budgets import ReadBudgets

FIXED_NOW = datetime(2030, 1, 1, tzinfo=timezone.utc)


def open_reader(api, poll_deadline_seconds=0.0):
    manifest = hixlutil.load_manifest()
    limits = hixlutil.default_limits()
    hixlutil.publish_regions(api, manifest)
    return HixlReader(
        manifest,
        api,
        hixlutil.peer(manifest),
        ReadBudgets(
            window_bytes=limits["window_bytes"],
            max_inflight=limits["max_inflight"],
            owned_output_bytes=limits["owned_output_bytes"],
        ),
        poll_deadline_seconds=poll_deadline_seconds,
    )


def first_span():
    manifest = hixlutil.load_manifest()
    tensor = manifest.tensors[0]
    return tensor.file, (tensor.data_begin, min(tensor.data_end, tensor.data_begin + 8))


class TestPreSubmissionTimeout(unittest.TestCase):
    def test_a_timeout_before_submission_fails_cleanly(self):
        api = FakeHixlAPI(submit_mode=SUBMIT_TIMEOUT_PRE)
        reader = open_reader(api)
        file_path, span = first_span()
        verdict = check_pre_submission_timeout(reader, file_path, span)
        self.assertEqual(verdict.status, PASS)
        self.assertIn("slot-released", verdict.evidence)
        self.assertIn("bytes-deactivated", verdict.evidence)

    def test_a_read_that_should_have_refused_is_no_go(self):
        api = FakeHixlAPI()
        reader = open_reader(api)
        file_path, span = first_span()
        verdict = check_pre_submission_timeout(reader, file_path, span)
        self.assertEqual(verdict.status, NO_GO)


class TestSubmittedNonterminalQuarantine(unittest.TestCase):
    def test_a_nonterminal_operation_is_retained_until_a_fence(self):
        api = FakeHixlAPI(submit_mode=SUBMIT_NONTERMINAL)
        reader = open_reader(api)
        file_path, span = first_span()
        verdict = check_submitted_nonterminal_quarantine(reader, file_path, span)
        self.assertEqual(verdict.status, PASS)
        for evidence in ("bytes-held", "close-refused", "fence-released"):
            self.assertIn(evidence, verdict.evidence)

    def test_a_completing_operation_cannot_be_quarantined(self):
        api = FakeHixlAPI()
        reader = open_reader(api)
        file_path, span = first_span()
        verdict = check_submitted_nonterminal_quarantine(reader, file_path, span)
        self.assertEqual(verdict.status, NO_GO)


class TestAcceptanceRunnerWiring(unittest.TestCase):
    def suite_config(self, suite):
        return {
            "schema_version": 1,
            "backend": "hixl",
            "suite": suite,
            "artifact": {
                "manifest_path": str(hixlutil.manifest_path()),
                "manifest_sha256": hixlutil.manifest_digest(),
            },
            "readiness": {
                "source_generation": 1,
                "expires": "2031-01-01T00:00:00+00:00",
            },
            "limits": hixlutil.default_limits(),
            "deadlines": {"operation_seconds": 30},
        }

    def run_main(self, suite, expect=None):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "acceptance.json")
            with open(path, "w") as fh:
                json.dump(self.suite_config(suite), fh)
            argv = ["--config", path, "--backend", "hixl", "--suite", suite]
            out = StringIO()
            with redirect_stdout(out):
                code = main(argv, now=FIXED_NOW)
        if expect is not None:
            self.assertEqual(code, expect, out.getvalue())
        return code, out.getvalue()

    def test_backend_suite_fails_closed_without_a_runtime(self):
        _code, out = self.run_main("backend", expect=1)
        report = json.loads(out)
        self.assertEqual(report["overall"], "NO-GO")
        self.assertGreater(report["summary"]["unrun"], 0)
        self.assertEqual(report["summary"]["pass"], 0)
        self.assertEqual(report["backend"], "hixl")

    def test_safety_suite_fails_closed_without_a_runtime(self):
        _code, out = self.run_main("safety", expect=1)
        report = json.loads(out)
        self.assertEqual(report["overall"], "NO-GO")
        self.assertGreater(report["summary"]["unrun"], 0)
        self.assertEqual(report["summary"]["pass"], 0)


class TestSuitesAgainstTheInjectedRuntime(unittest.TestCase):
    def test_every_backend_capability_passes_against_the_fake(self):
        from gpustack_model_transfer.acceptance import hixl_backend_suite

        context = hixlutil.default_context()
        results = hixl_backend_suite.run(context, runtime=FakeHixlAPI)
        statuses = {r.capability: r.status for r in results}
        for capability in hixl_backend_suite.CAPABILITIES:
            self.assertEqual(
                statuses.get(capability), "PASS", (capability, statuses.get(capability))
            )

    def test_a_faulty_runtime_reports_no_go_not_a_skip(self):
        from gpustack_model_transfer.acceptance import hixl_backend_suite

        context = hixlutil.default_context()
        results = hixl_backend_suite.run(
            context, runtime=lambda: FakeHixlAPI(submit_mode=SUBMIT_NONTERMINAL)
        )
        self.assertIn("NO-GO", {r.status for r in results})

    def test_a_bare_refusal_leaves_the_observation_dependent_boundaries_unrun(self):
        # A backend refusal is not proof. Every boundary that rests on
        # such a refusal — the raw accesses, the source window write and
        # the stale replay — is UNRUN, and recovery rests on them. The
        # boundaries that observe real state transitions still pass.
        from gpustack_model_transfer.acceptance import hixl_safety_suite

        context = hixlutil.default_context(backend="hixl", suite="safety")
        results = hixl_safety_suite.run(
            context, runtime=lambda: FakeHixlAPI(raw_mode="backend")
        )
        statuses = {r.capability: r.status for r in results}
        for capability in (
            "raw-write-isolation",
            "raw-read-isolation",
            "atomic-isolation",
            "source-window-write-isolation",
            "stale-replay-after-repair",
            "recovery-capability",
        ):
            self.assertEqual(statuses.get(capability), "UNRUN", capability)
        for capability in (
            "credential-disclosure",
            "pre-submission-timeout",
            "submitted-nonterminal-quarantine",
        ):
            self.assertEqual(statuses.get(capability), "PASS", capability)

    def test_recovery_cannot_pass_while_the_in_window_capability_is_unrun(self):
        # The reclaim rule rests on the source window's immutability. That
        # boundary is unproven, so recovery is unproven with it, and the
        # blocker is named.
        from gpustack_model_transfer.acceptance import hixl_safety_suite

        context = hixlutil.default_context(backend="hixl", suite="safety")
        results = hixl_safety_suite.run(
            context, runtime=lambda: FakeHixlAPI(raw_mode="backend")
        )
        by_capability = {r.capability: r for r in results}
        self.assertEqual(by_capability["source-window-write-isolation"].status, "UNRUN")
        self.assertNotEqual(by_capability["recovery-capability"].status, "PASS")
        self.assertIn(
            "source-window-write-isolation", by_capability["recovery-capability"].detail
        )

    def test_a_mutating_boundary_reports_no_go(self):
        from gpustack_model_transfer.acceptance import hixl_safety_suite

        context = hixlutil.default_context(backend="hixl", suite="safety")
        results = hixl_safety_suite.run(
            context, runtime=lambda: FakeHixlAPI(raw_mode="mutate")
        )
        raw = [r for r in results if r.capability.endswith("isolation")]
        self.assertTrue(raw)
        self.assertIn("NO-GO", {r.status for r in raw})
        by_capability = {r.capability: r for r in results}
        self.assertEqual(by_capability["recovery-capability"].status, "NO-GO")

    def test_a_source_window_write_that_lands_is_never_a_passing_run(self):
        # The boundary refuses an out-of-registration address, and a peer
        # can still write inside the registered source window. The
        # out-of-registration refusal must not stand in for source
        # immutability, and it does not stand in for itself either.
        from gpustack_model_transfer.acceptance import hixl_safety_suite

        context = hixlutil.default_context(backend="hixl", suite="safety")
        results = hixl_safety_suite.run(
            context,
            runtime=lambda: FakeHixlAPI(
                raw_mode="backend", source_window_mode="mutate"
            ),
        )
        by_capability = {r.capability: r for r in results}
        self.assertEqual(by_capability["raw-write-isolation"].status, "UNRUN")
        self.assertEqual(by_capability["source-window-write-isolation"].status, "NO-GO")
        self.assertEqual(by_capability["recovery-capability"].status, "NO-GO")

    def test_a_runtime_without_the_in_window_hook_stays_unrun(self):
        # A missing hook is an unexposed boundary, not a pass.
        from gpustack_model_transfer.acceptance import hixl_safety_suite

        class NoInWindowHook:
            """A runtime with the out-of-bounds hooks but no in-window one."""

            def __init__(self, inner):
                self._inner = inner

            def __getattr__(self, name):
                if name == "raw_write_in_source_window":
                    raise AttributeError(name)
                return getattr(self._inner, name)

            def __setattr__(self, name, value):
                if name == "_inner":
                    super().__setattr__(name, value)
                else:
                    setattr(self._inner, name, value)

        context = hixlutil.default_context(backend="hixl", suite="safety")
        results = hixl_safety_suite.run(
            context,
            runtime=lambda: NoInWindowHook(FakeHixlAPI(raw_mode="backend")),
        )
        by_capability = {r.capability: r for r in results}
        self.assertEqual(by_capability["source-window-write-isolation"].status, "UNRUN")
        self.assertEqual(by_capability["recovery-capability"].status, "UNRUN")

    def test_an_unrun_boundary_keeps_recovery_disabled_not_crashed(self):
        from gpustack_model_transfer.acceptance import hixl_safety_suite
        from gpustack_model_transfer.backends.hixl.testing import FakeHixlAPI

        class PartlyUnobservable(FakeHixlAPI):
            # The fault and isolation hooks answer, but the connection
            # records do not: the reclaim prerequisites pass while the
            # disclosure audit stays UNRUN.
            def describe_connection(self, connection):
                raise NotImplementedError("no connection records here")

        context = hixlutil.default_context(backend="hixl", suite="safety")
        results = hixl_safety_suite.run(
            context, runtime=lambda: PartlyUnobservable(raw_mode="backend")
        )
        by_capability = {r.capability: r for r in results}
        self.assertEqual(by_capability["credential-disclosure"].status, "UNRUN")
        self.assertEqual(by_capability["recovery-capability"].status, "UNRUN")
        self.assertIn(
            "credential-disclosure", by_capability["recovery-capability"].detail
        )


class TestSuitesAgainstARestrictedRuntime(unittest.TestCase):
    """A runtime without fault hooks answers UNRUN, never a crash."""

    class RestrictedAPI:
        version = "0.0.1"

        def create_agent(self, name, settings):
            return name

        def register_memory(self, agent, buffers):
            return buffers

        def connect(self, agent, peer):
            return peer

        def disconnect(self, agent, connection):
            pass

        def deregister(self, agent, registration):
            pass

        def submit_write(self, agent, registration, connection, window, source, length):
            return "op-1"

        def submit_read(self, agent, registration, connection, window, source, length):
            return "op-2"

        def poll(self, agent, operation_id):
            from gpustack_model_transfer.backends.hixl.runtime import TransferOutcome

            return TransferOutcome(state="succeeded")

        def device_memory_bytes(self, agent):
            raise NotImplementedError("no device memory on this runtime")

        def describe_connection(self, connection):
            raise NotImplementedError("no connection records on this runtime")

    def test_backend_suite_reports_unrun_not_crash(self):
        from gpustack_model_transfer.acceptance import hixl_backend_suite

        context = hixlutil.default_context()
        results = hixl_backend_suite.run(context, runtime=self.RestrictedAPI)
        by_capability = {r.capability: r for r in results}
        self.assertEqual(by_capability["memory-accounting"].status, "UNRUN")
        self.assertIn(
            "device-wide NPU memory", by_capability["memory-accounting"].detail
        )
        for result in results:
            self.assertNotIn("AttributeError", result.detail)

    def test_safety_suite_reports_unrun_not_crash(self):
        from gpustack_model_transfer.acceptance import hixl_safety_suite

        context = hixlutil.default_context(backend="hixl", suite="safety")
        results = hixl_safety_suite.run(context, runtime=self.RestrictedAPI)
        statuses = {r.capability: r.status for r in results}
        for capability in hixl_safety_suite.CAPABILITIES:
            self.assertEqual(statuses.get(capability), "UNRUN", capability)
        for result in results:
            self.assertNotIn("AttributeError", result.detail)


if __name__ == "__main__":
    unittest.main()
