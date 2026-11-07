"""Fault injection at observable boundaries, and the runner's wiring."""

import json
import os
import tempfile
import unittest
from contextlib import redirect_stdout
from datetime import datetime, timezone
from io import StringIO

import nixlutil
import pytest

from gpustack_model_transfer.acceptance.__main__ import main
from gpustack_model_transfer.backends.nixl import NixlReader, qualify_endpoints
from gpustack_model_transfer.backends.nixl.safety import (
    NO_GO,
    PASS,
    check_post_submission_quarantine,
    check_pre_submission_fault,
)
from gpustack_model_transfer.backends.nixl.testing import (
    SUBMIT_NONTERMINAL,
    SUBMIT_REFUSE_PRE,
    FakeNixlAPI,
)
from gpustack_model_transfer.core.budgets import ReadBudgets

FIXED_NOW = datetime(2030, 1, 1, tzinfo=timezone.utc)


def open_reader(api, poll_deadline_seconds=0.0):
    manifest = nixlutil.load_manifest()
    limits = nixlutil.default_limits()
    return NixlReader(
        manifest,
        api,
        {"source": "peer"},
        ReadBudgets(
            window_bytes=limits["window_bytes"],
            max_inflight=limits["max_inflight"],
            owned_output_bytes=limits["owned_output_bytes"],
        ),
        qualify_endpoints("tcp://10.0.0.1:9100", ["mlx5_0:1"], {}),
        poll_deadline_seconds=poll_deadline_seconds,
    )


def first_span():
    manifest = nixlutil.load_manifest()
    tensor = manifest.tensors[0]
    return tensor.file, (tensor.data_begin, min(tensor.data_end, tensor.data_begin + 8))


class TestPreSubmissionFault(unittest.TestCase):
    def test_a_refusal_before_submission_fails_cleanly(self):
        api = FakeNixlAPI(submit_mode=SUBMIT_REFUSE_PRE)
        reader = open_reader(api)
        file_path, span = first_span()
        verdict = check_pre_submission_fault(reader, file_path, span)
        self.assertEqual(verdict.status, PASS)
        self.assertIn("slot-released", verdict.evidence)

    def test_a_read_that_should_have_refused_is_no_go(self):
        # A healthy reader where the check expected a refusal: the check
        # itself must fail rather than declare a pass from nothing.
        api = FakeNixlAPI()
        reader = open_reader(api)
        file_path, span = first_span()
        verdict = check_pre_submission_fault(reader, file_path, span)
        self.assertEqual(verdict.status, NO_GO)


class TestPostSubmissionQuarantine(unittest.TestCase):
    def test_a_nonterminal_operation_is_retained_until_a_fence(self):
        api = FakeNixlAPI(submit_mode=SUBMIT_NONTERMINAL)
        reader = open_reader(api)
        file_path, span = first_span()
        verdict = check_post_submission_quarantine(reader, file_path, span)
        self.assertEqual(verdict.status, PASS)
        self.assertIn("close-refused", verdict.evidence)
        self.assertIn("fence-released", verdict.evidence)

    def test_a_completing_operation_cannot_be_quarantined(self):
        api = FakeNixlAPI()
        reader = open_reader(api)
        file_path, span = first_span()
        verdict = check_post_submission_quarantine(reader, file_path, span)
        self.assertEqual(verdict.status, NO_GO)


class TestAcceptanceRunnerWiring(unittest.TestCase):
    def backend_config(self, suite):
        return {
            "schema_version": 1,
            "backend": "nixl",
            "suite": suite,
            "artifact": {
                "manifest_path": str(nixlutil.manifest_path()),
                "manifest_sha256": nixlutil.manifest_digest(),
            },
            "readiness": {
                "source_generation": 1,
                "expires": "2031-01-01T00:00:00+00:00",
            },
            "limits": nixlutil.default_limits(),
            "deadlines": {"operation_seconds": 30},
        }

    def run_main(self, suite, engine=None, expect=None):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "acceptance.json")
            with open(path, "w") as fh:
                json.dump(self.backend_config(suite), fh)
            argv = ["--config", path, "--backend", "nixl", "--suite", suite]
            if engine is not None:
                argv += ["--engine", engine]
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
        self.assertEqual(report["summary"]["unrun"], 10)
        self.assertEqual(report["summary"]["pass"], 0)
        self.assertEqual(report["backend"], "nixl")

    def test_safety_suite_fails_closed_without_a_runtime(self):
        _code, out = self.run_main("safety", expect=1)
        report = json.loads(out)
        self.assertEqual(report["overall"], "NO-GO")
        self.assertEqual(report["summary"]["unrun"], 8)
        self.assertEqual(report["summary"]["pass"], 0)

    def test_an_engine_is_not_applicable_to_the_backend_suites(self):
        _code, out = self.run_main("backend", engine="vllm", expect=2)
        self.assertIn("engine", out.lower())


class TestScaleEvidenceGate(unittest.TestCase):
    def test_the_small_fixture_alone_leaves_the_scale_capability_unrun(self):
        from gpustack_model_transfer.acceptance import nixl_backend_suite

        context = nixlutil.default_context()
        results = nixl_backend_suite.run(context, runtime=FakeNixlAPI)
        scale = {
            r.capability: r
            for r in results
            if r.capability == "scale-8gib-qualification"
        }
        self.assertIn("scale-8gib-qualification", scale)
        self.assertEqual(scale["scale-8gib-qualification"].status, "UNRUN")
        self.assertIn(
            "no 8 GiB scale evidence", scale["scale-8gib-qualification"].detail
        )

    def test_configured_byte_counts_cannot_qualify_hardware(self):
        from gpustack_model_transfer.acceptance import nixl_backend_suite

        for byte_count in (1 << 30, 9 << 30):
            with self.subTest(byte_count=byte_count):
                context = nixlutil.default_context()
                context["config"]["readiness"]["scale_8gib_verified"] = {
                    "bytes_moved": byte_count
                }
                results = nixl_backend_suite.run(context, runtime=FakeNixlAPI)
                scale = next(
                    r for r in results if r.capability == "scale-8gib-qualification"
                )
                self.assertEqual(scale.status, "UNRUN")


class TestSuitesAgainstARestrictedRuntime(unittest.TestCase):
    """A runtime without the fault-injection hooks answers UNRUN."""

    class RestrictedAPI:
        """The seam surface with no fault hitches and no counters."""

        version = "1.5.0"

        def create_agent(self, name, settings):
            return name

        def register_memory(self, agent, buffers):
            return buffers

        def renew_registration(self, agent, registration):
            pass

        def revoke_registration(self, agent, registration):
            pass

        def submit_write(self, agent, registration, window, target, length):
            return "op-1"

        def poll(self, agent, operation_id):
            from gpustack_model_transfer.backends.nixl.runtime import TransferOutcome

            return TransferOutcome(state="succeeded")

        def port_counters(self, agent, rail):
            raise NotImplementedError("no counters on this runtime")

        def device_memory_bytes(self, agent):
            raise NotImplementedError("no device memory on this runtime")

    def test_backend_suite_reports_unrun_not_crash(self):
        from gpustack_model_transfer.acceptance import nixl_backend_suite

        context = nixlutil.default_context()
        results = nixl_backend_suite.run(context, runtime=self.RestrictedAPI)
        by_capability = {r.capability: r for r in results}
        self.assertEqual(
            by_capability["counter-attribution"].status,
            "UNRUN",
            by_capability["counter-attribution"].detail,
        )
        self.assertIn("port counters", by_capability["counter-attribution"].detail)
        self.assertEqual(
            by_capability["memory-accounting"].status,
            "UNRUN",
            by_capability["memory-accounting"].detail,
        )
        self.assertIn("device-wide memory", by_capability["memory-accounting"].detail)
        self.assertEqual(by_capability["per-operation-terminal-states"].status, "UNRUN")

    def test_safety_suite_reports_unrun_not_crash(self):
        from gpustack_model_transfer.acceptance import nixl_safety_suite

        context = nixlutil.default_context(backend="nixl", suite="safety")
        results = nixl_safety_suite.run(context, runtime=self.RestrictedAPI)
        statuses = {r.capability: r.status for r in results}
        for capability in nixl_safety_suite.CAPABILITIES:
            self.assertEqual(statuses.get(capability), "UNRUN", capability)
        for result in results:
            self.assertNotIn("AttributeError", result.detail)


class TestSuitesAgainstTheInjectedRuntime(unittest.TestCase):
    def test_every_backend_capability_passes_against_the_fake(self):
        from gpustack_model_transfer.acceptance import nixl_backend_suite

        context = nixlutil.default_context()
        results = nixl_backend_suite.run(context, runtime=FakeNixlAPI)
        statuses = {r.capability: r.status for r in results}
        for capability in nixl_backend_suite.CAPABILITIES:
            if capability == "scale-8gib-qualification":
                continue
            self.assertEqual(
                statuses.get(capability), "PASS", (capability, statuses.get(capability))
            )
        self.assertEqual(len(results), len(nixl_backend_suite.CAPABILITIES) + 0)

    def test_a_faulty_runtime_reports_no_go_not_a_skip(self):
        from gpustack_model_transfer.acceptance import nixl_backend_suite

        context = nixlutil.default_context()
        results = nixl_backend_suite.run(
            context,
            runtime=lambda: FakeNixlAPI(submit_mode=SUBMIT_NONTERMINAL),
        )
        statuses = {r.status for r in results}
        self.assertIn("NO-GO", statuses)

    def test_a_bare_refusal_leaves_the_observation_dependent_boundaries_unrun(self):
        # A backend refusal is not proof. Every boundary that rests on
        # such a refusal — the raw accesses, the source window write and
        # the stale access — is UNRUN. The boundaries that observe real
        # state transitions still pass.
        from gpustack_model_transfer.acceptance import nixl_safety_suite

        context = nixlutil.default_context(backend="nixl", suite="safety")
        results = nixl_safety_suite.run(
            context,
            runtime=lambda: FakeNixlAPI(raw_mode="backend"),
        )
        statuses = {r.capability: r.status for r in results}
        for capability in (
            "raw-write-isolation",
            "raw-read-isolation",
            "atomic-isolation",
            "source-window-write-isolation",
            "stale-access-bounds",
        ):
            self.assertEqual(statuses.get(capability), "UNRUN", capability)
        for capability in (
            "credential-disclosure",
            "pre-submission-fault-release",
            "post-submission-unknown-quarantine",
        ):
            self.assertEqual(statuses.get(capability), "PASS", capability)

    def test_a_mutating_boundary_reports_no_go(self):
        from gpustack_model_transfer.acceptance import nixl_safety_suite

        context = nixlutil.default_context(backend="nixl", suite="safety")
        results = nixl_safety_suite.run(
            context,
            runtime=lambda: FakeNixlAPI(raw_mode="mutate"),
        )
        statuses = {r.capability: r.status for r in results}
        self.assertIn("NO-GO", set(statuses.values()))
        # The verdict that matters most is the raw-access one.
        raw = [r for r in results if r.capability.endswith("isolation")]
        self.assertTrue(raw, "the isolation capabilities must be present")
        self.assertIn("NO-GO", {r.status for r in raw})

    def test_a_source_window_write_that_lands_is_never_a_passing_run(self):
        # The boundary refuses an out-of-registration address, and a peer
        # can still write inside the registered source window. The
        # out-of-registration refusal must not stand in for source
        # immutability.
        from gpustack_model_transfer.acceptance import nixl_safety_suite

        context = nixlutil.default_context(backend="nixl", suite="safety")
        results = nixl_safety_suite.run(
            context,
            runtime=lambda: FakeNixlAPI(
                raw_mode="backend", source_window_mode="mutate"
            ),
        )
        by_capability = {r.capability: r for r in results}
        self.assertEqual(by_capability["raw-write-isolation"].status, "UNRUN")
        self.assertEqual(by_capability["source-window-write-isolation"].status, "NO-GO")

    def test_a_runtime_without_the_in_window_hook_stays_unrun(self):
        # A missing hook is an unexposed boundary, not a pass.
        from gpustack_model_transfer.acceptance import nixl_safety_suite

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

        context = nixlutil.default_context(backend="nixl", suite="safety")
        results = nixl_safety_suite.run(
            context,
            runtime=lambda: NoInWindowHook(FakeNixlAPI(raw_mode="backend")),
        )
        by_capability = {r.capability: r for r in results}
        self.assertEqual(by_capability["source-window-write-isolation"].status, "UNRUN")


if __name__ == "__main__":
    unittest.main()


@pytest.mark.parametrize("held_slot,active_bytes", [(0, 0), (None, 8)])
def test_refused_operation_is_not_safe_while_resources_remain(
    monkeypatch, held_slot, active_bytes
):
    from types import SimpleNamespace

    reader = open_reader(FakeNixlAPI(submit_mode=SUBMIT_REFUSE_PRE))
    read = reader.read_bytes
    original_slots, original_ledger = reader._slots, reader.ledger

    def refuse_with_leak(*args):
        try:
            read(*args)
        finally:
            reader._slots = SimpleNamespace(slot_of=lambda _: held_slot)
            reader.ledger = SimpleNamespace(active_bytes=active_bytes)

    monkeypatch.setattr(reader, "read_bytes", refuse_with_leak)
    try:
        file_path, span = first_span()
        verdict = check_pre_submission_fault(reader, file_path, span)
        assert verdict.status == NO_GO
        assert "slot-released" not in verdict.evidence
    finally:
        reader._slots, reader.ledger = original_slots, original_ledger
        reader.close()


@pytest.mark.parametrize("phase", ["pre", "post"])
@pytest.mark.parametrize("fail", [False, True])
def test_safety_suite_closes_reader_on_every_probe_exit(monkeypatch, phase, fail):
    from gpustack_model_transfer.acceptance import nixl_safety_suite as suite
    from gpustack_model_transfer.backends.nixl import reader as reader_module
    from gpustack_model_transfer.backends.nixl import safety

    readers = []

    class Reader:
        def __init__(self, *args, **kwargs):
            self.closed = False
            readers.append(self)

        def close(self):
            self.closed = True

    def probe(*args):
        if fail:
            raise RuntimeError("probe failed")
        return "checked"

    probe_name = (
        "check_pre_submission_fault"
        if phase == "pre"
        else "check_post_submission_quarantine"
    )
    monkeypatch.setattr(reader_module, "NixlReader", Reader)
    monkeypatch.setattr(safety, probe_name, probe)
    check = (
        suite._check_pre_submission_fault_release
        if phase == "pre"
        else suite._check_post_submission_unknown_quarantine
    )
    if fail:
        with pytest.raises(RuntimeError, match="probe failed"):
            check(nixlutil.default_context(suite="safety"), FakeNixlAPI())
    else:
        assert (
            check(nixlutil.default_context(suite="safety"), FakeNixlAPI()) == "checked"
        )
    assert len(readers) == 1 and readers[0].closed
