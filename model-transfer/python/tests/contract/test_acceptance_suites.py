"""The cluster harness suites: fail-closed without a harness, honest with one."""

import json
import os
import sys
import tempfile
import unittest
from datetime import datetime, timezone
from pathlib import Path

import fixtureutil
import pytest

from gpustack_model_transfer.acceptance import (
    ep_coexistence_suite,
    lifecycle_suite,
    managed_startup_suite,
)
from gpustack_model_transfer.acceptance.__main__ import main

FIXTURE_DIR = fixtureutil.fixture_dir()
DIGEST = fixtureutil.manifest_digest()
FIXED_NOW = datetime(2030, 1, 1, tzinfo=timezone.utc)


def make_context(backend, suite, engine=None):
    from gpustack_model_transfer.acceptance.config import load_and_validate

    config = {
        "schema_version": 1,
        "backend": backend,
        "suite": suite,
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
    with tempfile.TemporaryDirectory() as tmp:
        path = os.path.join(tmp, "acceptance.json")
        with open(path, "w") as fh:
            json.dump(config, fh)
        return load_and_validate(path, backend, suite, engine, now=FIXED_NOW)


def run_main(backend, suite, engine=None):
    from contextlib import redirect_stdout
    from io import StringIO

    config = {
        "schema_version": 1,
        "backend": backend,
        "suite": suite,
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
    return code, json.loads(out.getvalue())


class HarnessEnvTestCase(unittest.TestCase):
    """Isolate the harness discovery variables every test in this file."""

    VARIABLES = (
        managed_startup_suite.HARNESS_VARIABLE,
        lifecycle_suite.HARNESS_VARIABLE,
        ep_coexistence_suite.HARNESS_VARIABLE,
    )

    def setUp(self):
        self._saved = {name: os.environ.get(name) for name in self.VARIABLES}
        for name in self.VARIABLES:
            os.environ.pop(name, None)
        self.addCleanup(self._restore)

    def _restore(self):
        for name, value in self._saved.items():
            if value is None:
                os.environ.pop(name, None)
            else:
                os.environ[name] = value


# ---------------------------------------------------------------------------
# managed-startup


class ReferenceRole:
    """A managed role that honors the whole honest-startup contract."""

    def __init__(self, policy="require", qualified=True):
        self.policy = policy
        self.qualified = qualified
        self.phase = "serving"
        self.reason = ""

    def serving_status(self):
        if self.qualified:
            return {"phase": self.phase, "reason": self.reason}
        if self.policy == "require":
            return {
                "phase": "failed",
                "reason": "Require remote: the remote path is not qualified",
            }
        return {"phase": self.phase, "reason": self.reason}

    def download_events(self):
        return [
            {"kind": "hub-download", "blocked": True},
            {"kind": "full-checkpoint", "blocked": True},
            {"kind": "remote-window-read", "blocked": False},
        ]

    def inventory_writes(self):
        return []

    def forward(self, inputs):
        return [f"token-{index}" for index, _ in enumerate(inputs)]

    def native_forward(self, inputs):
        return [f"token-{index}" for index, _ in enumerate(inputs)]

    def phase_timings(self):
        return {
            "cold_fill_seconds": 12.5,
            "warm_reuse_seconds": 0.5,
            "load_seconds": 40.0,
            "serving_seconds": 3.0,
        }

    def fallback_record(self):
        if self.qualified or self.policy == "require":
            return {
                "occurred": False,
                "reason": "",
                "replacement_pod": False,
                "attempts": 0,
                "route": "",
            }
        return {
            "occurred": True,
            "reason": "the remote path was not qualified",
            "replacement_pod": True,
            "attempts": 1,
            "route": "verified-node-delivery",
        }

    def connector_status(self):
        return {"ec": "continuous", "kv": "continuous"}

    def failed_rank_record(self):
        # A failed rank never leaves its worker/load group standing.
        return {
            "recreated": not self.qualified,
            "reason": "" if self.qualified else "the load group was recreated",
        }

    def replica_statuses(self):
        if not self.qualified:
            if self.policy == "require":
                return [{"replica": "a", "phase": "failed", "delivery": "remote"}]
            return [
                {
                    "replica": "a",
                    "phase": "serving",
                    "delivery": "verified-node-delivery",
                },
                {"replica": "b", "phase": "serving", "delivery": "remote"},
            ]
        return [
            {"replica": "a", "phase": "serving", "delivery": "remote"},
            {"replica": "b", "phase": "serving", "delivery": "remote"},
        ]


class ReferenceStartupHarness:
    def metadata_target(self):
        return {
            "manifest_digest": DIGEST,
            "metadata_only": True,
            "local_checkpoint_bytes": 0,
        }

    def start_role(self, target, policy, qualified=True):
        return ReferenceRole(policy, qualified)

    def stale_identity(self, target):
        return {
            "refused": True,
            "reason": "the requested source generation is no longer live",
        }

    def lose_source(self, role):
        # Serving replicas keep serving; only new loads would wait.
        return role

    def cann_gate(self):
        return {
            "safe_binding": True,
            "source_grant": True,
            "device_allocation": True,
            "network_allocation": True,
            "reclamation_rule": True,
            "cann_enabled": True,
            "cuda_support_blocked": False,
        }


class FaultyStartupHarness(ReferenceStartupHarness):
    """One injected fault; every other hook stays honest."""

    def __init__(self, fault):
        self.fault = fault

    def metadata_target(self):
        target = super().metadata_target()
        if self.fault == "metadata-only-target":
            target["local_checkpoint_bytes"] = 1 << 20
        if self.fault == "target-names-another-artifact":
            target["manifest_digest"] = "b" * 64
        return target

    def start_role(self, target, policy, qualified=True):
        role = ReferenceRole(policy, qualified)
        if self.fault == "require-refusal-honesty" and not qualified:
            role.qualified = True  # the unqualified startup served anyway
        if self.fault == "failed-rank-not-recreated" and not qualified:
            role.failed_rank_record = lambda: {
                "recreated": False,
                "reason": "the tainted group was reused",
            }
        return role

    def stale_identity(self, target):
        outcome = super().stale_identity(target)
        if self.fault == "stale-identity-refusal":
            outcome["refused"] = False
        return outcome

    def lose_source(self, role):
        if self.fault == "source-loss-serving-continuity":
            role.phase = "degraded"
        return role


class FaultyRole(ReferenceRole):
    pass


def _faulty_role_for(fault):
    role = ReferenceRole()
    if fault == "no-hidden-checkpoint-download":
        role.download_events = lambda: [{"kind": "hub-download", "blocked": False}]
    elif fault == "forward-equivalence":
        role.native_forward = lambda inputs: ["different"]
    elif fault == "prefer-bounded-fallback":
        role.qualified = False
        role.policy = "prefer"
        original = role.fallback_record

        def unbounded():
            record = original()
            record["attempts"] = 9
            return record

        role.fallback_record = unbounded
    elif fault == "flattened-replica-status":
        role.qualified = False
        role.policy = "prefer"
        role.replica_statuses = lambda: [
            {"replica": "all", "phase": "serving", "delivery": "mixed"}
        ]
    elif fault == "phase-timing-publication":
        role.phase_timings = lambda: {
            "cold_fill_seconds": 12.5,
            "warm_reuse_seconds": 0.5,
            "serving_seconds": 3.0,
        }
    elif fault == "connector-continuity":
        role.connector_status = lambda: {"ec": "continuous", "kv": "broken"}
    return role


class FaultyRoleStartupHarness(ReferenceStartupHarness):
    """Roles whose own observables lie, for the role-level faults."""

    def __init__(self, fault):
        self.fault = fault

    def start_role(self, target, policy, qualified=True):
        return _faulty_role_for(self.fault)


class FaultyCannHarness(ReferenceStartupHarness):
    def cann_gate(self):
        gate = super().cann_gate()
        gate["source_grant"] = False
        gate["cann_enabled"] = True  # enabled over a missing grant
        return gate


class TestManagedStartupSuite(HarnessEnvTestCase):
    ALL = managed_startup_suite.CAPABILITIES + (managed_startup_suite.CANN_CAPABILITIES)

    def test_the_reference_harness_passes_every_capability(self):
        context = make_context("nixl", "managed-startup", "vllm")
        results = managed_startup_suite.run(
            context, runtime=ReferenceStartupHarness, engine="vllm"
        )
        statuses = {r.capability: r.status for r in results}
        for capability in managed_startup_suite.capabilities_for("vllm"):
            self.assertEqual(statuses.get(capability), "PASS", (capability, statuses))
        self.assertNotIn("source-npu-grant-and-recovery-gate", statuses, statuses)

    def test_the_reference_harness_passes_the_cann_engine_too(self):
        context = make_context("hixl", "managed-startup", "vllm-ascend")
        results = managed_startup_suite.run(
            context, runtime=ReferenceStartupHarness, engine="vllm-ascend"
        )
        statuses = {r.capability: r.status for r in results}
        for capability in managed_startup_suite.capabilities_for("vllm-ascend"):
            self.assertEqual(statuses.get(capability), "PASS", (capability, statuses))

    def test_every_capability_fault_reports_no_go(self):
        # fault name -> the capability it must surface as NO-GO
        expectations = (
            ("metadata-only-target", "metadata-only-target", "harness"),
            ("target-names-another-artifact", "metadata-only-target", "harness"),
            ("require-refusal-honesty", "require-refusal-honesty", "harness"),
            ("failed-rank-not-recreated", "require-refusal-honesty", "harness"),
            ("stale-identity-refusal", "stale-identity-refusal", "harness"),
            (
                "source-loss-serving-continuity",
                "source-loss-serving-continuity",
                "harness",
            ),
            ("no-hidden-checkpoint-download", "no-hidden-checkpoint-download", "role"),
            ("forward-equivalence", "forward-equivalence", "role"),
            ("prefer-bounded-fallback", "prefer-bounded-fallback", "role"),
            ("flattened-replica-status", "prefer-bounded-fallback", "role"),
            ("phase-timing-publication", "phase-timing-publication", "role"),
            ("connector-continuity", "connector-continuity", "role"),
        )
        context = make_context("nixl", "managed-startup", "vllm")
        for fault, capability, level in expectations:
            with self.subTest(fault=fault):
                if level == "harness":
                    harness = FaultyStartupHarness(fault)
                else:
                    harness = FaultyRoleStartupHarness(fault)
                results = managed_startup_suite.run(
                    context, runtime=lambda h=harness: h, engine="vllm"
                )
                statuses = {r.capability: r.status for r in results}
                self.assertEqual(statuses.get(capability), "NO-GO", statuses)
                self.assertIn("PASS", statuses.values(), statuses)

    def test_a_cann_gate_enabled_over_a_missing_grant_is_no_go(self):
        context = make_context("hixl", "managed-startup", "vllm-ascend")
        results = managed_startup_suite.run(
            context, runtime=FaultyCannHarness, engine="vllm-ascend"
        )
        statuses = {r.capability: r.status for r in results}
        self.assertEqual(
            statuses.get("source-npu-grant-and-recovery-gate"), "NO-GO", statuses
        )

    def test_without_a_harness_every_capability_is_unrun(self):
        context = make_context("nixl", "managed-startup", "vllm")
        results = managed_startup_suite.run(context, engine="vllm")
        self.assertEqual(
            [r.capability for r in results],
            list(managed_startup_suite.capabilities_for("vllm")),
        )
        for result in results:
            self.assertEqual(result.status, "UNRUN", result.capability)
            self.assertIn(managed_startup_suite.HARNESS_VARIABLE, result.detail)

    def test_an_importable_harness_variable_is_discovered(self):
        source = (
            "class Harness:\n"
            "    def metadata_target(self):\n"
            f"        return {{'manifest_digest': {DIGEST!r},"
            " 'metadata_only': True, 'local_checkpoint_bytes': 0}\n"
        )
        with tempfile.TemporaryDirectory() as tmp:
            module = Path(tmp) / "ref_startup_harness.py"
            module.write_text(
                source + "    def start_role(self, target, policy, qualified=True):\n"
                "        from test_acceptance_suites import ReferenceRole\n"
                "        return ReferenceRole(policy, qualified)\n"
                "    def stale_identity(self, target):\n"
                "        return {'refused': True, 'reason': 'stale'}\n"
                "    def lose_source(self, role):\n"
                "        return role\n"
                "    def cann_gate(self):\n"
                "        return {'safe_binding': True, 'source_grant': True,"
                " 'device_allocation': True, 'network_allocation': True,"
                " 'reclamation_rule': True, 'cann_enabled': True,"
                " 'cuda_support_blocked': False}\n"
                "\n\n"
                "def build_harness(context):\n"
                "    return Harness()\n"
            )
            sys.path.insert(0, tmp)
            os.environ[managed_startup_suite.HARNESS_VARIABLE] = (
                "ref_startup_harness:build_harness"
            )
            try:
                context = make_context("nixl", "managed-startup", "vllm")
                results = managed_startup_suite.run(context, engine="vllm")
            finally:
                sys.path.remove(tmp)
                sys.modules.pop("ref_startup_harness", None)
        statuses = {r.capability: r.status for r in results}
        for capability in managed_startup_suite.capabilities_for("vllm"):
            self.assertEqual(statuses.get(capability), "PASS", statuses)

    def test_a_broken_harness_variable_is_unrun_with_the_reason(self):
        os.environ[managed_startup_suite.HARNESS_VARIABLE] = "no_such_module_xyz:f"
        context = make_context("nixl", "managed-startup", "vllm")
        results = managed_startup_suite.run(context, engine="vllm")
        for result in results:
            self.assertEqual(result.status, "UNRUN", result.capability)
            self.assertIn("could not be built", result.detail)


# ---------------------------------------------------------------------------
# lifecycle


class ReferenceReader:
    def reads(self):
        return [b"authorized-window-bytes"]

    def budget(self):
        return {
            "max_inflight": 2,
            "observed_inflight": 2,
            "window_bytes": 32,
            "observed_window_bytes": 32,
        }


class ReferenceLifecycleHarness:
    def open_readers(self, count):
        return [ReferenceReader() for _ in range(count)]

    def admission_trace(self, namespaces):
        return [
            {"namespace": ns, "admitted": True, "wait_cycles": 1} for ns in namespaces
        ]

    def evict_generation(self, generation):
        return {"surviving_reads_byte_exact": True, "stale_exposure": False}

    def restart_daemon(self):
        return {
            "live_reservations_rebuilt": True,
            "drain_evidence": "qualified-fence",
            "released_only_qualified": True,
        }

    def reconnect_cycles(self, count):
        return {
            "cycles": count,
            "within_budget": True,
            "bytes_transferred": 1024,
            "budget_bytes": 4096,
        }

    def submit_nonterminal_fault(self):
        return {
            "quarantined": True,
            "retained_bytes": 2048,
            "fence": "qualified",
            "released_after_fence": True,
        }

    def recycle_address_probe(self):
        return {
            "distinct_generations": True,
            "cross_generation_exposure": False,
        }


class FaultyLifecycleHarness(ReferenceLifecycleHarness):
    def __init__(self, fault):
        self.fault = fault

    def open_readers(self, count):
        readers = super().open_readers(count)
        if self.fault == "concurrent-authorized-readers":
            for reader in readers:
                reader.budget = lambda: {
                    "max_inflight": 2,
                    "observed_inflight": 4,
                    "window_bytes": 32,
                    "observed_window_bytes": 32,
                }
        if self.fault == "budget-above-run-limits":
            for reader in readers:
                reader.budget = lambda: {
                    "max_inflight": 2,
                    "observed_inflight": 2,
                    "window_bytes": 4096,
                    "observed_window_bytes": 4096,
                }
        return readers

    def admission_trace(self, namespaces):
        trace = super().admission_trace(namespaces)
        if self.fault == "namespace-admission-fairness":
            trace[0]["admitted"] = False
            trace[0]["wait_cycles"] = 99
        return trace

    def evict_generation(self, generation):
        outcome = super().evict_generation(generation)
        if self.fault == "generation-eviction-isolation":
            outcome["stale_exposure"] = True
        return outcome

    def restart_daemon(self):
        outcome = super().restart_daemon()
        if self.fault == "daemon-restart-reconstruction":
            # An empty process list is not DMA-drain evidence.
            outcome["drain_evidence"] = "process-scan"
        return outcome

    def reconnect_cycles(self, count):
        outcome = super().reconnect_cycles(count)
        if self.fault == "repeated-reconnect-budgets":
            outcome["within_budget"] = False
            outcome["bytes_transferred"] = 8192
        return outcome

    def submit_nonterminal_fault(self):
        outcome = super().submit_nonterminal_fault()
        if self.fault == "submitted-failure-quarantine-fence":
            outcome["quarantined"] = False
        return outcome

    def recycle_address_probe(self):
        outcome = super().recycle_address_probe()
        if self.fault == "no-recycled-address-exposure":
            outcome["cross_generation_exposure"] = True
        return outcome


class TestLifecycleSuite(HarnessEnvTestCase):
    def test_the_reference_harness_passes_every_capability(self):
        context = make_context("nixl", "lifecycle")
        results = lifecycle_suite.run(context, runtime=ReferenceLifecycleHarness)
        statuses = {r.capability: r.status for r in results}
        self.assertEqual(sorted(statuses), sorted(lifecycle_suite.CAPABILITIES))
        for capability, status in statuses.items():
            self.assertEqual(status, "PASS", capability)

    def test_every_capability_fault_reports_no_go(self):
        context = make_context("nixl", "lifecycle")
        for fault in lifecycle_suite.CAPABILITIES:
            with self.subTest(fault=fault):
                results = lifecycle_suite.run(
                    context, runtime=lambda f=fault: FaultyLifecycleHarness(f)
                )
                statuses = {r.capability: r.status for r in results}
                self.assertEqual(statuses.get(fault), "NO-GO", statuses)
                self.assertIn("PASS", statuses.values(), statuses)

    def test_a_negotiated_budget_above_the_runs_limits_is_no_go(self):
        # The run's validated limits bound what a reader may negotiate,
        # not just what it observes against its own numbers.
        context = make_context("nixl", "lifecycle")
        results = lifecycle_suite.run(
            context,
            runtime=lambda: FaultyLifecycleHarness("budget-above-run-limits"),
        )
        statuses = {r.capability: r.status for r in results}
        self.assertEqual(
            statuses.get("concurrent-authorized-readers"), "NO-GO", statuses
        )
        detail = {
            r.capability: r.detail
            for r in results
            if r.capability == "concurrent-authorized-readers"
        }
        self.assertIn("4096 > 32", detail["concurrent-authorized-readers"])

    def test_without_a_harness_every_capability_is_unrun(self):
        context = make_context("nixl", "lifecycle")
        results = lifecycle_suite.run(context)
        self.assertEqual(
            [r.capability for r in results], list(lifecycle_suite.CAPABILITIES)
        )
        for result in results:
            self.assertEqual(result.status, "UNRUN", result.capability)
            self.assertIn(lifecycle_suite.HARNESS_VARIABLE, result.detail)


# ---------------------------------------------------------------------------
# ep-coexistence


class ReferenceEPHarness:
    def ep_tuple(self):
        return {"engine": "vllm", "ep_backend": "nixl_ep", "world_size": 2}

    def coexistence_run(self):
        return {"loader_agents": 2, "ep_agents": 4, "isolated_contexts": True}

    def budget_report(self):
        return {"nic_budget": 8, "nic_used": 4, "mr_budget": 64, "mr_used": 32}

    def teardown_order(self):
        return [
            "ep-context-init",
            "loader-agents-init",
            "loader-agents-teardown",
            "ep-context-teardown",
        ]

    def serving_parity(self):
        return {"with_model_transfer": [1, 2, 3], "native": [1, 2, 3]}

    def joiner_join(self):
        return {"joined_online": True, "cold_loads": 0, "migration": "native"}

    def cann_ep_claimed(self):
        return False


class FaultyEPHarness(ReferenceEPHarness):
    def __init__(self, fault):
        self.fault = fault

    def ep_tuple(self):
        tup = super().ep_tuple()
        if self.fault == "nixl-ep-agent-coexistence":
            tup["world_size"] = 1
        return tup

    def budget_report(self):
        report = super().budget_report()
        if self.fault == "nic-and-mr-budgets":
            report["nic_used"] = 9
        return report

    def teardown_order(self):
        events = super().teardown_order()
        if self.fault == "init-teardown-ordering":
            # The EP context tears down before the loader's agents.
            return [
                "ep-context-init",
                "loader-agents-init",
                "ep-context-teardown",
                "loader-agents-teardown",
            ]
        return events

    def serving_parity(self):
        parity = super().serving_parity()
        if self.fault == "serving-equivalence":
            parity["native"] = [3, 2, 1]
        return parity

    def joiner_join(self):
        joiner = super().joiner_join()
        if self.fault == "online-joiner-no-cold-reload":
            joiner["cold_loads"] = 1
        return joiner

    def cann_ep_claimed(self):
        if self.fault == "no-cann-ep-claim":
            return True
        return super().cann_ep_claimed()


class TestEPCoexistenceSuite(HarnessEnvTestCase):
    def test_the_reference_harness_passes_every_capability(self):
        context = make_context("nixl", "ep-coexistence")
        results = ep_coexistence_suite.run(context, runtime=ReferenceEPHarness)
        statuses = {r.capability: r.status for r in results}
        self.assertEqual(sorted(statuses), sorted(ep_coexistence_suite.CAPABILITIES))
        for capability, status in statuses.items():
            self.assertEqual(status, "PASS", capability)

    def test_every_capability_fault_reports_no_go(self):
        context = make_context("nixl", "ep-coexistence")
        for fault in ep_coexistence_suite.CAPABILITIES:
            with self.subTest(fault=fault):
                results = ep_coexistence_suite.run(
                    context, runtime=lambda f=fault: FaultyEPHarness(f)
                )
                statuses = {r.capability: r.status for r in results}
                self.assertEqual(statuses.get(fault), "NO-GO", statuses)
                self.assertIn("PASS", statuses.values(), statuses)

    def test_a_cann_backend_refuses_the_coexistence_check(self):
        # No CANN EP claim follows from HIXL: even a harness that answers
        # everything else honestly fails on a non-nixl backend.
        context = make_context("hixl", "ep-coexistence")
        results = ep_coexistence_suite.run(context, runtime=ReferenceEPHarness)
        statuses = {r.capability: r.status for r in results}
        self.assertEqual(statuses.get("nixl-ep-agent-coexistence"), "NO-GO", statuses)

    def test_without_a_harness_every_capability_is_unrun(self):
        context = make_context("nixl", "ep-coexistence")
        results = ep_coexistence_suite.run(context)
        self.assertEqual(
            [r.capability for r in results], list(ep_coexistence_suite.CAPABILITIES)
        )
        for result in results:
            self.assertEqual(result.status, "UNRUN", result.capability)
            self.assertIn(ep_coexistence_suite.HARNESS_VARIABLE, result.detail)


# ---------------------------------------------------------------------------
# runner wiring


class TestRunnerWiring(HarnessEnvTestCase):
    def test_every_wired_suite_reports_unrun_without_a_cluster(self):
        for backend, suite, engine in (
            ("nixl", "managed-startup", "vllm"),
            ("hixl", "managed-startup", "vllm-ascend"),
            ("nixl", "lifecycle", None),
            ("nixl", "ep-coexistence", None),
        ):
            with self.subTest(backend=backend, suite=suite, engine=engine):
                code, report = run_main(backend, suite, engine)
                self.assertEqual(code, 1, report)
                self.assertEqual(report["overall"], "NO-GO")
                self.assertEqual(report["summary"]["unrun"], len(report["results"]))
                self.assertEqual(report["summary"]["pass"], 0)
                self.assertEqual(report["summary"]["no_go"], 0)

    def test_the_cann_engine_carries_the_npu_gate_capability(self):
        _code, report = run_main("hixl", "managed-startup", "vllm-ascend")
        names = [r["capability"] for r in report["results"]]
        self.assertIn("source-npu-grant-and-recovery-gate", names)

        _code, report = run_main("nixl", "managed-startup", "vllm")
        names = [r["capability"] for r in report["results"]]
        self.assertNotIn("source-npu-grant-and-recovery-gate", names)

    def test_unwired_combinations_still_refuse_as_not_implemented(self):
        # contract stays local; ep-coexistence never claims CANN.
        for backend, suite, engine in (
            ("nixl", "contract", None),
            ("hixl", "ep-coexistence", None),
            ("local", "lifecycle", None),
            ("hixl", "lifecycle", None),
        ):
            with self.subTest(backend=backend, suite=suite):
                code, report = run_main(backend, suite, engine)
                self.assertEqual(code, 3, report)
                self.assertTrue(report["refused"], report)
                self.assertIn("not implemented", report["reason"])

    def test_an_engine_not_qualified_for_managed_startup_refused(self):
        code, report = run_main("nixl", "managed-startup", "sglang")
        self.assertEqual(code, 2, report)
        code, report = run_main("nixl", "managed-startup", None)
        self.assertEqual(code, 2, report)

    def test_an_engine_is_not_applicable_to_engineless_suites(self):
        code, report = run_main("nixl", "lifecycle", "vllm")
        self.assertEqual(code, 2, report)
        self.assertIn("not applicable", json.dumps(report).lower())


if __name__ == "__main__":
    unittest.main()


def test_missing_cann_gate_is_unrun():
    class MissingGate(ReferenceStartupHarness):
        cann_gate = None

    results = managed_startup_suite.run(
        make_context("hixl", "managed-startup", "vllm-ascend"),
        runtime=MissingGate,
        engine="vllm-ascend",
    )
    gate = next(
        r for r in results if r.capability == "source-npu-grant-and-recovery-gate"
    )
    assert gate.status == "UNRUN"


@pytest.mark.parametrize(
    "suite_name",
    [
        "hixl_backend_suite",
        "hixl_safety_suite",
        "nixl_backend_suite",
        "nixl_safety_suite",
    ],
)
@pytest.mark.parametrize("present", [False, True])
def test_fault_check_restores_missing_or_none_mode(suite_name, present):
    import importlib
    from types import SimpleNamespace

    suite = importlib.import_module("gpustack_model_transfer.acceptance." + suite_name)
    api = SimpleNamespace(submit_mode=None) if present else SimpleNamespace()

    def check(context, runtime):
        runtime.submit_mode = "injected-fault"
        raise RuntimeError("injected failure")

    result = suite._run_check("test-capability", check, {}, api)
    assert result.status == "NO-GO"
    assert hasattr(api, "submit_mode") is present
    if present:
        assert api.submit_mode is None
