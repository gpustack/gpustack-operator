"""The safety probes: verdicts that can fail, evidence that is kept."""

import threading
import unittest

from gpustack_model_transfer.backends.nixl.safety import (
    NO_GO,
    PASS,
    UNRUN,
    audit_credential_disclosure,
    probe_raw_access,
    probe_source_window_write,
    probe_stale_access_after_revoke,
)
from gpustack_model_transfer.backends.nixl.testing import (
    RAW_BACKEND_ENFORCED,
    RAW_DENIES_AND_MUTATES,
    RAW_LANDS_READ,
    RAW_MUTATES,
    RAW_NONE,
    RAW_WRAPPER_ENFORCED,
    AttemptResult,
    FakeNixlAPI,
)


def disposable_buffer(size: int = 64) -> bytearray:
    return bytearray(b"s" * size)


def registered(api):
    agent = api.create_agent("safety-probe", {"UCX_NET_DEVICES": "mlx5_0:1"})
    buffer = disposable_buffer()
    registration = api.register_memory(agent, {"source": buffer})
    return agent, buffer, registration


class LateSourceWrite(FakeNixlAPI):
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


class LateMutation(FakeNixlAPI):
    """A boundary that refuses the attempt and mutates the window later.

    The refusal is immediate; the mutation lands on a worker thread after
    the probe has taken its snapshot, so the probe sees the refusal and
    the bytes only afterwards.

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
        buffer = next(iter(registration.buffers.values()))
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


class TestRawWriteIsolation(unittest.TestCase):
    def test_a_backend_refusal_without_a_control_is_unrun(self):
        # A refusal is not proof. Nothing ran a positive control of the
        # raw WRITE path in this session, and no qualified submission and
        # terminal fence was observed, so the refusal does not establish
        # that the out-of-bounds write was refused.
        api = FakeNixlAPI(raw_mode=RAW_BACKEND_ENFORCED)
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
        # The earlier NIXL raw-key probe changed the source bytes: this
        # probe must call that NO-GO, never average it away.
        api = FakeNixlAPI(raw_mode=RAW_MUTATES)
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
        self.assertIn("source-bytes-changed", verdict.evidence)

    def test_a_wrapper_only_refusal_is_no_go(self):
        api = FakeNixlAPI(raw_mode=RAW_WRAPPER_ENFORCED)
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
        self.assertIn("wrapper", verdict.detail)

    def test_an_unobservable_boundary_is_unrun(self):
        api = FakeNixlAPI(raw_mode=RAW_NONE)
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


class TestRawReadAndAtomicIsolation(unittest.TestCase):
    def test_exfiltration_is_no_go_even_without_mutation(self):
        api = FakeNixlAPI(raw_mode=RAW_LANDS_READ)
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

    def test_atomic_on_the_source_is_probed_like_any_access(self):
        api = FakeNixlAPI(raw_mode=RAW_MUTATES)
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

    def test_backend_denial_is_unrun_for_read_and_atomic(self):
        # The raw READ path and the atomic path each need their own
        # positive control; a refusal in either proves nothing without it.
        for escape, operation in (
            ("raw_read_out_of_bounds", "raw-read"),
            ("raw_atomic_on_source", "atomic"),
        ):
            with self.subTest(escape=escape):
                api = FakeNixlAPI(raw_mode=RAW_BACKEND_ENFORCED)
                agent, buffer, registration = registered(api)
                verdict = probe_raw_access(
                    api,
                    agent,
                    registration,
                    buffer,
                    lambda a, ag, reg, name=escape: getattr(a, name)(ag, reg),
                    operation=operation,
                )
                self.assertEqual(verdict.status, UNRUN)
                self.assertIn("no-positive-control", verdict.evidence)
                self.assertIn("no-terminal-fence", verdict.evidence)


class TestStaleAccessBounds(unittest.TestCase):
    def test_a_backend_refusal_at_the_revoked_registration_is_unrun(self):
        # The revoked registration was refused and the bounded window saw
        # no change, but no same-session positive control of the raw WRITE
        # path ran and no qualified fence showed the revocation and the
        # replay are terminal, so the refusal does not prove revocation.
        api = FakeNixlAPI(raw_mode=RAW_BACKEND_ENFORCED)
        agent, buffer, registration = registered(api)
        verdict = probe_stale_access_after_revoke(
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
        self.assertEqual(api.revocations, 1)

    def test_an_unchanged_snapshot_proves_nothing(self):
        # A bounded window that saw no change is UNRUN: it proves neither
        # revocation nor the absence of a late write.
        api = FakeNixlAPI(raw_mode=RAW_NONE)
        agent, buffer, registration = registered(api)
        verdict = probe_stale_access_after_revoke(
            api,
            agent,
            registration,
            buffer,
            lambda a, ag, reg: a.raw_write_out_of_bounds(ag, reg),
            operation="raw-write",
        )
        self.assertEqual(verdict.status, UNRUN)
        self.assertIn("snapshot-unchanged", verdict.evidence)

    def test_a_late_write_is_no_go(self):
        api = FakeNixlAPI(raw_mode=RAW_MUTATES)
        agent, buffer, registration = registered(api)
        verdict = probe_stale_access_after_revoke(
            api,
            agent,
            registration,
            buffer,
            lambda a, ag, reg: a.raw_write_out_of_bounds(ag, reg),
            operation="raw-write",
        )
        self.assertEqual(verdict.status, NO_GO)


class TestLateMutationAfterARefusal(unittest.TestCase):
    """A refusal is not proof, and a late mutation shows it is not.

    The refusal is observed first and the mutation lands afterwards, so a
    verdict taken at refusal time cannot be a pass.
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

    def test_a_late_mutation_after_the_revocation_is_not_a_pass(self):
        api = LateMutation(raw_mode=RAW_BACKEND_ENFORCED)
        self.addCleanup(api.release_and_join)
        agent, buffer, registration = registered(api)
        verdict = probe_stale_access_after_revoke(
            api,
            agent,
            registration,
            buffer,
            lambda a, ag, reg: a.raw_write_out_of_bounds(ag, reg),
            operation="raw-write",
        )
        self.assertNotEqual(verdict.status, PASS)

        before = bytes(buffer)
        api.release_and_join()
        self.assertNotEqual(bytes(buffer), before, "the late write landed")


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
                api = FakeNixlAPI(raw_mode=RAW_BACKEND_ENFORCED)
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

    def test_the_stale_access_names_the_control_it_is_missing(self):
        api = FakeNixlAPI(raw_mode=RAW_BACKEND_ENFORCED)
        agent, buffer, registration = registered(api)
        verdict = probe_stale_access_after_revoke(
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
        api = FakeNixlAPI(raw_mode=RAW_BACKEND_ENFORCED)
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


class TestCredentialDisclosure(unittest.TestCase):
    def test_an_empty_audit_is_not_a_pass(self):
        # An audit over no records proves nothing about disclosure: there
        # is no record to have checked.
        verdict = audit_credential_disclosure([])
        self.assertEqual(verdict.status, UNRUN)
        self.assertIn("no-records-returned", verdict.evidence)

    def test_a_clean_setup_passes_with_internal_keys_noted(self):
        api = FakeNixlAPI(disclosure_mode="clean")
        _agent, _buffer, registration = registered(api)
        verdict = audit_credential_disclosure(api.describe_connection(registration))
        self.assertEqual(verdict.status, PASS)
        self.assertTrue(
            any("internal" in item for item in verdict.evidence),
            "internal keys are noted, not hidden",
        )

    def test_a_raw_key_on_the_wire_is_no_go(self):
        api = FakeNixlAPI(disclosure_mode="disclose")
        _agent, _buffer, registration = registered(api)
        verdict = audit_credential_disclosure(api.describe_connection(registration))
        self.assertEqual(verdict.status, NO_GO)
        self.assertIn("rkey", verdict.detail)


class TestDeniedButLandedAccess(unittest.TestCase):
    """A refusal is evidence of safety only while the source still holds.

    The boundary said no and the bytes moved anyway. That is a landed
    mutation wearing a refusal's name, and it is NO-GO.
    """

    def test_a_denial_that_changed_the_bytes_is_no_go(self):
        api = FakeNixlAPI(raw_mode=RAW_DENIES_AND_MUTATES)
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
        api = FakeNixlAPI(raw_mode=RAW_DENIES_AND_MUTATES)
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

    def test_a_denial_that_changed_the_revoked_window_is_no_go(self):
        api = FakeNixlAPI(raw_mode=RAW_DENIES_AND_MUTATES)
        agent, buffer, registration = registered(api)
        verdict = probe_stale_access_after_revoke(
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

    def test_a_denial_that_changed_the_bytes_it_never_touched_is_no_go(self):
        # A backend that refuses the revoked registration and still
        # writes the source: the refusal is not enforcement.
        api = FakeNixlAPI()
        agent, buffer, registration = registered(api)

        def deny_and_mutate(api_, agent_, registration_):
            buffer[0] ^= 0xFF
            return AttemptResult(
                denied=True,
                enforcement="backend",
                detail="the fake backend refused the raw access",
            )

        verdict = probe_stale_access_after_revoke(
            api, agent, registration, buffer, deny_and_mutate, operation="raw-write"
        )
        self.assertEqual(verdict.status, NO_GO)
        self.assertIn("denied-but-landed", verdict.evidence)


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
        api = FakeNixlAPI(source_window_mode=RAW_BACKEND_ENFORCED)
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
        api = FakeNixlAPI(source_window_mode=RAW_WRAPPER_ENFORCED)
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
        api = FakeNixlAPI(source_window_mode=RAW_MUTATES)
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
        api = FakeNixlAPI(source_window_mode=RAW_NONE)
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
        api = FakeNixlAPI(source_window_mode=RAW_DENIES_AND_MUTATES)
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
