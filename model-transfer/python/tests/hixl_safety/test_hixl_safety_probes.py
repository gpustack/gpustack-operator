"""The HIXL safety probes: verdicts that can fail, evidence kept."""

import threading
import unittest

from gpustack_model_transfer.backends.hixl.safety import (
    NO_GO,
    PASS,
    UNRUN,
    SafetyReport,
    SafetyVerdict,
    audit_credential_disclosure,
    check_recovery_standing,
    probe_raw_access,
    probe_source_window_write,
    probe_stale_replay_after_repair,
)
from gpustack_model_transfer.backends.hixl.testing import (
    RAW_BACKEND_ENFORCED,
    RAW_DENIES_AND_MUTATES,
    RAW_LANDS_READ,
    RAW_MUTATES,
    RAW_NONE,
    RAW_WRAPPER_ENFORCED,
    AttemptResult,
    FakeHixlAPI,
)


def registered(api, size: int = 64):
    agent = api.create_agent("safety-probe", {})
    buffer = bytearray(b"s" * size)
    registration = api.register_memory(agent, {"source": buffer})
    return agent, buffer, registration


class LateSourceWrite(FakeHixlAPI):
    """A boundary that refuses the write and writes the window afterwards.

    The refusal is what the probe observes; the write lands after the probe
    has taken its snapshot. A probe that answered PASS on the refusal alone
    would have reported safety before the write reached the weights.
    """

    def __init__(self, **kwargs):
        super().__init__(**kwargs)
        self.pending = None

    def raw_write_in_source_window(self, agent, registration):
        result = super().raw_write_in_source_window(agent, registration)
        self.pending = registration
        return result

    def fire_pending_write(self):
        """The write that follows the refusal."""
        assert self.pending is not None, "the attempt never ran"
        buffer = next(iter(self.pending.buffers.values()))
        buffer[0] ^= 0xFF


class LateMutation(FakeHixlAPI):
    """A boundary that refuses the attempt and mutates the window later.

    The refusal is immediate; the mutation lands on a worker thread after
    the probe has taken its snapshot, so the probe sees the refusal and
    the bytes only afterwards. This is the coordinator's reproduction: a
    PASS verdict followed by an asynchronous mutation of the same buffer.

    The registration owns the buffer, the fake keeps the registration,
    and the thread holds it until it joins, so the mutation cannot be
    lost to a collected buffer.
    """

    def __init__(self, **kwargs):
        super().__init__(**kwargs)
        self._release = threading.Event()
        self._armed = threading.Event()
        self._thread = None
        self._registration = None

    def _mutate_when_released(self, registration):
        self._armed.set()
        self._release.wait(timeout=5.0)
        target = self._alias_target(registration)
        if target is None:
            return
        buffer = next(iter(target.buffers.values()))
        buffer[0] ^= 0xFF

    def _schedule(self, registration):
        self._registration = registration
        self._thread = threading.Thread(
            target=self._mutate_when_released,
            args=(registration,),
            daemon=True,
        )
        self._thread.start()
        self._armed.wait(timeout=5.0)

    def raw_write_out_of_bounds(self, agent, registration):
        result = super().raw_write_out_of_bounds(agent, registration)
        self._schedule(registration)
        return result

    def release_and_join(self):
        """Let the late mutation land, then wait for the thread."""
        self._release.set()
        if self._thread is not None:
            self._thread.join(timeout=5.0)


class TestRawAccessProbes(unittest.TestCase):
    def test_a_backend_refusal_without_a_control_is_unrun(self):
        # A refusal is not proof. Nothing ran a positive control of the
        # raw WRITE path in this session, and no qualified submission and
        # terminal fence was observed, so the refusal does not establish
        # that the out-of-bounds write was refused.
        api = FakeHixlAPI(raw_mode=RAW_BACKEND_ENFORCED)
        agent, buffer, registration = registered(api)
        verdict = probe_raw_access(
            api,
            agent,
            registration,
            buffer,
            lambda a, ag, reg: a.raw_write_out_of_bounds(ag, reg),
            operation="raw-write",
        )
        self.assertEqual(verdict.status, UNRUN)
        self.assertEqual(verdict.enforcement, "backend")
        self.assertIn("no-positive-control", verdict.evidence)
        self.assertIn("no-terminal-fence", verdict.evidence)

    def test_a_landed_write_is_no_go(self):
        api = FakeHixlAPI(raw_mode=RAW_MUTATES)
        agent, buffer, registration = registered(api)
        verdict = probe_raw_access(
            api,
            agent,
            registration,
            buffer,
            lambda a, ag, reg: a.raw_write_out_of_bounds(ag, reg),
            operation="raw-write",
        )
        self.assertEqual(verdict.status, NO_GO)

    def test_a_wrapper_only_refusal_is_no_go(self):
        api = FakeHixlAPI(raw_mode=RAW_WRAPPER_ENFORCED)
        agent, buffer, registration = registered(api)
        verdict = probe_raw_access(
            api,
            agent,
            registration,
            buffer,
            lambda a, ag, reg: a.raw_atomic_on_source(ag, reg),
            operation="atomic",
        )
        self.assertEqual(verdict.status, NO_GO)
        self.assertIn("wrapper", verdict.detail)

    def test_exfiltration_is_no_go_without_mutation(self):
        api = FakeHixlAPI(raw_mode=RAW_LANDS_READ)
        agent, buffer, registration = registered(api)
        verdict = probe_raw_access(
            api,
            agent,
            registration,
            buffer,
            lambda a, ag, reg: a.raw_read_out_of_bounds(ag, reg),
            operation="raw-read",
        )
        self.assertEqual(verdict.status, NO_GO)
        self.assertIn("out-of-bounds-read-landed", verdict.evidence)

    def test_an_unobservable_boundary_is_unrun(self):
        api = FakeHixlAPI(raw_mode=RAW_NONE)
        agent, buffer, registration = registered(api)
        verdict = probe_raw_access(
            api,
            agent,
            registration,
            buffer,
            lambda a, ag, reg: a.raw_read_out_of_bounds(ag, reg),
            operation="raw-read",
        )
        self.assertEqual(verdict.status, UNRUN)


class TestStaleReplayAfterRepair(unittest.TestCase):
    def test_a_backend_refusal_after_the_repair_is_unrun(self):
        # The stale handle was refused and both generations are unchanged,
        # but no same-session positive control of the raw WRITE path ran
        # and no qualified fence showed the deregistration and the replay
        # are terminal, so the refusal does not prove revocation.
        api = FakeHixlAPI(raw_mode=RAW_BACKEND_ENFORCED)
        agent, buffer, registration = registered(api)
        verdict = probe_stale_replay_after_repair(
            api,
            agent,
            registration,
            buffer,
            lambda a, ag, reg: a.raw_write_out_of_bounds(ag, reg),
            operation="raw-write",
        )
        self.assertEqual(verdict.status, UNRUN)
        self.assertIn("no-positive-control", verdict.evidence)
        self.assertIn("no-terminal-fence", verdict.evidence)

    def test_an_unchanged_snapshot_proves_nothing(self):
        # The repaired replay scenario the PoC timed out on: no change
        # observed, and that is UNRUN, never a pass.
        api = FakeHixlAPI(raw_mode=RAW_NONE)
        agent, buffer, registration = registered(api)
        verdict = probe_stale_replay_after_repair(
            api,
            agent,
            registration,
            buffer,
            lambda a, ag, reg: a.raw_write_out_of_bounds(ag, reg),
            operation="raw-write",
        )
        self.assertEqual(verdict.status, UNRUN)
        self.assertIn("reclamation rule is still unproven", verdict.detail)

    def test_a_late_access_is_no_go(self):
        api = FakeHixlAPI(raw_mode=RAW_MUTATES)
        agent, buffer, registration = registered(api)
        verdict = probe_stale_replay_after_repair(
            api,
            agent,
            registration,
            buffer,
            lambda a, ag, reg: a.raw_write_out_of_bounds(ag, reg),
            operation="raw-write",
        )
        self.assertEqual(verdict.status, NO_GO)

    def test_the_repair_registers_a_new_generation_not_the_same_buffer(self):
        # The repaired cleanup order: the old generation is deregistered
        # and a NEW buffer takes its place, so a stale handle that lands
        # reaches the new generation's memory, which the probe observes.
        api = FakeHixlAPI(raw_mode=RAW_MUTATES)
        agent, buffer, registration = registered(api)
        old_before = bytes(buffer)
        verdict = probe_stale_replay_after_repair(
            api,
            agent,
            registration,
            buffer,
            lambda a, ag, reg: a.raw_write_out_of_bounds(ag, reg),
            operation="raw-write",
        )
        self.assertEqual(verdict.status, NO_GO)
        self.assertIn("new-generation-bytes-changed", verdict.evidence)
        # The stale write went through the revoked handle into the new
        # registration; the old generation's own bytes never moved.
        self.assertEqual(bytes(buffer), old_before)
        self.assertTrue(registration.revoked)
        self.assertEqual(len(api.registrations), 2)

    def test_a_revoked_handle_with_no_live_target_is_denied(self):
        # No repair followed the revocation, so there is no live memory
        # for the stale handle to alias: the backend refuses the handle.
        api = FakeHixlAPI(raw_mode=RAW_NONE)
        agent, _buffer, registration = registered(api)
        api.deregister(agent, registration)
        result = api.raw_write_out_of_bounds(agent, registration)
        self.assertTrue(result.denied)
        self.assertEqual(result.enforcement, "backend")


class TestLateMutationAfterARefusal(unittest.TestCase):
    """A refusal is not proof, and a late mutation shows it is not.

    The coordinator reproduced a PASS verdict followed by an asynchronous
    mutation of the same buffer. The verdict must not be a pass, and the
    mutation must land deterministically once the probe has returned.
    """

    def test_a_late_mutation_after_a_refusal_is_not_a_pass(self):
        api = LateMutation(raw_mode=RAW_BACKEND_ENFORCED)
        self.addCleanup(api.release_and_join)
        agent, buffer, registration = registered(api)
        before = bytes(buffer)
        verdict = probe_raw_access(
            api,
            agent,
            registration,
            buffer,
            lambda a, ag, reg: a.raw_write_out_of_bounds(ag, reg),
            operation="raw-write",
        )
        self.assertNotEqual(verdict.status, PASS)

        api.release_and_join()
        self.assertNotEqual(bytes(buffer), before, "the late mutation landed")

    def test_a_late_mutation_after_the_repair_is_not_a_pass(self):
        # The stale handle reaches the repaired generation's memory. The
        # refusal is observed first and the mutation lands afterwards, so
        # a verdict taken at refusal time cannot be a pass.
        api = LateMutation(raw_mode=RAW_BACKEND_ENFORCED)
        self.addCleanup(api.release_and_join)
        agent, buffer, registration = registered(api)
        verdict = probe_stale_replay_after_repair(
            api,
            agent,
            registration,
            buffer,
            lambda a, ag, reg: a.raw_write_out_of_bounds(ag, reg),
            operation="raw-write",
        )
        self.assertNotEqual(verdict.status, PASS)

        new_generation = api.registrations[-1].buffers["source"]
        before = bytes(new_generation)
        api.release_and_join()
        self.assertNotEqual(bytes(new_generation), before, "the late write landed")


class TestOperationSpecificPositiveControl(unittest.TestCase):
    """Each raw operation needs its own positive control.

    A raw WRITE refusal needs a WRITE control, a raw READ refusal needs a
    READ control, and an atomic refusal needs an atomic control. The
    verdict has to name the control it is missing, or the next run
    cannot tell what to build.
    """

    CASES = (
        ("raw-write", "raw_write_out_of_bounds", "the raw WRITE path"),
        ("raw-read", "raw_read_out_of_bounds", "the raw READ path"),
        ("atomic", "raw_atomic_on_source", "the atomic path"),
    )

    def test_each_raw_operation_names_its_own_control(self):
        for operation, hook, control in self.CASES:
            with self.subTest(operation=operation):
                api = FakeHixlAPI(raw_mode=RAW_BACKEND_ENFORCED)
                agent, buffer, registration = registered(api)
                verdict = probe_raw_access(
                    api,
                    agent,
                    registration,
                    buffer,
                    lambda a, ag, reg, name=hook: getattr(a, name)(ag, reg),
                    operation=operation,
                )
                self.assertEqual(verdict.status, UNRUN)
                self.assertIn(f"operation={operation}", verdict.evidence)
                self.assertIn(control, verdict.detail)

    def test_the_stale_replay_names_the_control_it_is_missing(self):
        api = FakeHixlAPI(raw_mode=RAW_BACKEND_ENFORCED)
        agent, buffer, registration = registered(api)
        verdict = probe_stale_replay_after_repair(
            api,
            agent,
            registration,
            buffer,
            lambda a, ag, reg: a.raw_write_out_of_bounds(ag, reg),
            operation="raw-write",
        )
        self.assertEqual(verdict.status, UNRUN)
        self.assertIn("operation=raw-write", verdict.evidence)
        self.assertIn("the raw WRITE path", verdict.detail)

    def test_an_unknown_operation_is_refused(self):
        api = FakeHixlAPI(raw_mode=RAW_BACKEND_ENFORCED)
        agent, buffer, registration = registered(api)
        with self.assertRaises(ValueError):
            probe_raw_access(
                api,
                agent,
                registration,
                buffer,
                lambda a, ag, reg: a.raw_write_out_of_bounds(ag, reg),
                operation="raw-teapot",
            )


class TestRecoveryStandingNeedsAQualifiedFence(unittest.TestCase):
    """A recorded proof is not a qualified fence.

    The reclaim rule needs a fence that retains both endpoints until it
    resolves the retained operation. No such harness exists, so neither a
    recorded rule nor a verdict's marker can enable recovery.
    """

    def _report(self, evidence) -> SafetyReport:
        report = SafetyReport()
        report.add(
            SafetyVerdict(
                capability="submitted-nonterminal-quarantine",
                status=PASS,
                detail="the nonterminal operation was retained and released",
                evidence=evidence,
            )
        )
        report.recovery.enable("a reclaim rule was recorded")
        return report

    def test_a_marker_never_substitutes_for_a_qualified_fence(self):
        report = self._report(("state=unknown", "bytes-held", "fence-released"))
        verdict = check_recovery_standing(report)
        self.assertEqual(verdict.status, UNRUN)
        self.assertFalse(report.recovery.enabled)

    def test_a_recorded_rule_without_a_fence_harness_stays_unrun(self):
        report = self._report(("state=unknown", "bytes-held"))
        verdict = check_recovery_standing(report)
        self.assertEqual(verdict.status, UNRUN)
        self.assertFalse(report.recovery.enabled)


class TestCredentialDisclosure(unittest.TestCase):
    def test_an_empty_audit_is_not_a_pass(self):
        # An audit over no records proves nothing about disclosure: there
        # is no record to have checked.
        verdict = audit_credential_disclosure([])
        self.assertEqual(verdict.status, UNRUN)
        self.assertIn("no-records-returned", verdict.evidence)

    def test_a_clean_setup_passes_with_internal_keys_noted(self):
        api = FakeHixlAPI(disclosure_mode="clean")
        _agent, _buffer, _registration = registered(api)
        connection = api.connect(_agent, {})
        verdict = audit_credential_disclosure(api.describe_connection(connection))
        self.assertEqual(verdict.status, PASS)

    def test_a_raw_key_on_the_wire_is_no_go(self):
        api = FakeHixlAPI(disclosure_mode="disclose")
        agent, _buffer, _registration = registered(api)
        connection = api.connect(agent, {})
        verdict = audit_credential_disclosure(api.describe_connection(connection))
        self.assertEqual(verdict.status, NO_GO)
        self.assertIn("mr_key", verdict.detail)


class TestRecoveryStanding(unittest.TestCase):
    def test_recovery_stays_unrun_until_a_reclaim_rule_is_proven(self):
        from gpustack_model_transfer.backends.hixl.safety import SafetyReport

        report = SafetyReport()
        report.add(
            type(
                "V",
                (),
                {
                    "capability": "stale-replay",
                    "status": UNRUN,
                    "detail": "the bounded window proved nothing",
                    "enforcement": "",
                    "evidence": (),
                    "validate": lambda self: None,
                },
            )()
        )
        verdict = check_recovery_standing(report)
        self.assertEqual(verdict.status, UNRUN)
        self.assertIn("blocked", verdict.detail)
        self.assertFalse(report.recovery.enabled)

    def test_a_recorded_reclaim_rule_does_not_enable_recovery_yet(self):
        # The reclaim rule needs a qualified fence that retains both
        # endpoints until it resolves the retained operation. Until that
        # harness exists, a recorded rule alone leaves recovery disabled.
        from gpustack_model_transfer.backends.hixl.safety import (
            SafetyReport,
            SafetyVerdict,
        )

        report = SafetyReport()
        report.add(
            SafetyVerdict(
                capability="submitted-nonterminal-quarantine",
                status=PASS,
                detail="the fence resolved and released the retained window",
                evidence=("state=unknown", "bytes-held", "fence-released"),
            )
        )
        report.recovery.enable("qualified fence drained the replay generation")
        verdict = check_recovery_standing(report)
        self.assertEqual(verdict.status, UNRUN)
        self.assertFalse(report.recovery.enabled)

    def test_a_no_go_boundary_blocks_recovery_outright(self):
        from gpustack_model_transfer.backends.hixl.safety import (
            SafetyReport,
            SafetyVerdict,
        )

        report = SafetyReport()
        report.add(
            SafetyVerdict(
                capability="raw-write-isolation",
                status=NO_GO,
                detail="the out-of-bounds attempt landed",
            )
        )
        with self.assertRaises(ValueError):
            report.recovery.enable("a proof cannot stand on a failed boundary")
        verdict = check_recovery_standing(report)
        self.assertEqual(verdict.status, NO_GO)
        self.assertIn("raw-write-isolation", verdict.detail)
        self.assertFalse(report.recovery.enabled)

    def test_an_enabled_but_blocked_record_refuses_to_report_pass(self):
        from gpustack_model_transfer.backends.hixl.safety import (
            SafetyReport,
            SafetyVerdict,
        )

        report = SafetyReport()
        report.add(
            SafetyVerdict(
                capability="stale-replay",
                status=UNRUN,
                detail="the bounded window proved nothing",
            )
        )
        # A record enabled before the blocker was added is inconsistent;
        # the standing refuses to inherit the enable.
        report.recovery.proof = "enabled out of band"
        report.recovery.enabled = True
        verdict = check_recovery_standing(report)
        self.assertEqual(verdict.status, UNRUN)
        self.assertFalse(report.recovery.enabled)


class TestDeniedButLandedAccess(unittest.TestCase):
    """A refusal is evidence of safety only while the source still holds.

    The boundary said no and the bytes moved anyway. That is a landed
    mutation wearing a refusal's name, and it is NO-GO.
    """

    def test_a_denial_that_changed_the_bytes_is_no_go(self):
        api = FakeHixlAPI(raw_mode=RAW_DENIES_AND_MUTATES)
        agent, buffer, registration = registered(api)
        verdict = probe_raw_access(
            api,
            agent,
            registration,
            buffer,
            lambda a, ag, reg: a.raw_write_out_of_bounds(ag, reg),
            operation="raw-write",
        )
        self.assertEqual(verdict.status, NO_GO)
        self.assertIn("denied-but-landed", verdict.evidence)
        self.assertIn("source-bytes-changed", verdict.evidence)

    def test_a_denial_that_exfiltrated_is_no_go(self):
        api = FakeHixlAPI(raw_mode=RAW_DENIES_AND_MUTATES)
        agent, buffer, registration = registered(api)
        verdict = probe_raw_access(
            api,
            agent,
            registration,
            buffer,
            lambda a, ag, reg: a.raw_read_out_of_bounds(ag, reg),
            operation="raw-read",
        )
        self.assertEqual(verdict.status, NO_GO)
        self.assertIn("denied-but-landed", verdict.evidence)

    def test_a_denial_that_changed_the_new_generation_is_no_go(self):
        api = FakeHixlAPI(raw_mode=RAW_DENIES_AND_MUTATES)
        agent, buffer, registration = registered(api)
        verdict = probe_stale_replay_after_repair(
            api,
            agent,
            registration,
            buffer,
            lambda a, ag, reg: a.raw_write_out_of_bounds(ag, reg),
            operation="raw-write",
        )
        self.assertEqual(verdict.status, NO_GO)
        self.assertIn("new-generation-bytes-changed", verdict.evidence)
        self.assertIn("denied-but-landed", verdict.evidence)

    def test_a_denial_that_changed_the_old_generation_is_no_go(self):
        # The other generation: a backend that refuses the stale handle
        # and still writes the old generation's bytes. The fake cannot
        # express that, so the attempt says so itself.
        api = FakeHixlAPI()
        agent, buffer, registration = registered(api)
        old_before = bytes(buffer)

        def deny_and_mutate(api_, agent_, registration_):
            buffer[0] ^= 0xFF
            return AttemptResult(
                denied=True,
                enforcement="backend",
                detail="the fake backend refused the raw access",
            )

        verdict = probe_stale_replay_after_repair(
            api, agent, registration, buffer, deny_and_mutate, operation="raw-write"
        )
        self.assertEqual(verdict.status, NO_GO)
        self.assertIn("old-generation-bytes-changed", verdict.evidence)
        self.assertIn("denied-but-landed", verdict.evidence)
        self.assertNotEqual(bytes(buffer), old_before)


class TestSourceWindowWrite(unittest.TestCase):
    """The in-window WRITE negative: the question the spec asks.

    The out-of-bounds probe asks whether the boundary refuses an address
    outside the registration. This one asks whether a peer can change
    the source weights inside the window the source published.
    """

    def test_a_backend_refusal_without_a_control_is_unrun(self):
        # A refusal is not proof. Nothing ran a positive WRITE control in
        # this session, and no fence showed the attempt was terminal, so the
        # refusal cannot establish that the source window is immutable.
        api = FakeHixlAPI(source_window_mode=RAW_BACKEND_ENFORCED)
        agent, buffer, registration = registered(api)
        verdict = probe_source_window_write(
            api,
            agent,
            registration,
            buffer,
            lambda a, ag, reg: a.raw_write_in_source_window(ag, reg),
        )
        self.assertEqual(verdict.status, UNRUN)
        self.assertEqual(verdict.enforcement, "backend")
        self.assertIn("no-positive-control", verdict.evidence)
        self.assertIn("no-terminal-fence", verdict.evidence)

    def test_a_write_that_lands_after_the_refusal_cannot_pass(self):
        # The refusal and the write are separate events. The probe sees the
        # refusal, so a snapshot cannot see the write: the verdict must not
        # be a pass, or the report would claim safety before the write
        # reached the weights.
        api = LateSourceWrite(source_window_mode=RAW_BACKEND_ENFORCED)
        agent, buffer, registration = registered(api)
        before = bytes(buffer)
        verdict = probe_source_window_write(
            api,
            agent,
            registration,
            buffer,
            lambda a, ag, reg: a.raw_write_in_source_window(ag, reg),
        )
        self.assertNotEqual(verdict.status, PASS)

        api.fire_pending_write()
        self.assertNotEqual(bytes(buffer), before, "the late write landed")
        self.assertNotEqual(verdict.status, PASS)

    def test_a_wrapper_only_refusal_is_no_go(self):
        api = FakeHixlAPI(source_window_mode=RAW_WRAPPER_ENFORCED)
        agent, buffer, registration = registered(api)
        verdict = probe_source_window_write(
            api,
            agent,
            registration,
            buffer,
            lambda a, ag, reg: a.raw_write_in_source_window(ag, reg),
        )
        self.assertEqual(verdict.status, NO_GO)
        self.assertIn("wrapper", verdict.detail)

    def test_a_landed_source_write_is_no_go(self):
        api = FakeHixlAPI(source_window_mode=RAW_MUTATES)
        agent, buffer, registration = registered(api)
        verdict = probe_source_window_write(
            api,
            agent,
            registration,
            buffer,
            lambda a, ag, reg: a.raw_write_in_source_window(ag, reg),
        )
        self.assertEqual(verdict.status, NO_GO)
        self.assertIn("source-bytes-changed", verdict.evidence)

    def test_an_unchanged_window_is_unrun(self):
        api = FakeHixlAPI(source_window_mode=RAW_NONE)
        agent, buffer, registration = registered(api)
        verdict = probe_source_window_write(
            api,
            agent,
            registration,
            buffer,
            lambda a, ag, reg: a.raw_write_in_source_window(ag, reg),
        )
        self.assertEqual(verdict.status, UNRUN)

    def test_a_denial_that_changed_the_window_is_no_go(self):
        api = FakeHixlAPI(source_window_mode=RAW_DENIES_AND_MUTATES)
        agent, buffer, registration = registered(api)
        verdict = probe_source_window_write(
            api,
            agent,
            registration,
            buffer,
            lambda a, ag, reg: a.raw_write_in_source_window(ag, reg),
        )
        self.assertEqual(verdict.status, NO_GO)
        self.assertIn("denied-but-landed", verdict.evidence)
        self.assertIn("source-bytes-changed", verdict.evidence)


if __name__ == "__main__":
    unittest.main()
