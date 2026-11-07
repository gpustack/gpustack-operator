"""Session ordering, transport negotiation, and the recovery standing."""

import unittest

from gpustack_model_transfer.backends.hixl import (
    FabricMemRelaxation,
    HixlSession,
    RecoveryCapability,
    SessionOrderError,
    TransportOptionRejectedError,
    negotiate_transport,
)


class FakeRegistration:
    pass


class FakeConnection:
    pass


class TestSessionOrder(unittest.TestCase):
    def open_session(self, relaxation=None):
        return HixlSession(agent=object(), relaxation=relaxation)

    def test_the_documented_order_proceeds(self):
        session = self.open_session()
        registration = FakeRegistration()
        connection = FakeConnection()
        session.register(registration)
        session.connect(connection)
        session.disconnect(connection)
        session.deregister(registration)
        self.assertEqual(session.phase, "deregistered")

    def test_connect_before_register_is_refused(self):
        session = self.open_session()
        with self.assertRaises(SessionOrderError):
            session.connect(FakeConnection())

    def test_deregister_before_disconnect_is_refused(self):
        session = self.open_session()
        registration = FakeRegistration()
        connection = FakeConnection()
        session.register(registration)
        session.connect(connection)
        with self.assertRaises(SessionOrderError) as caught:
            session.deregister(registration)
        self.assertIn("disconnect before deregister", str(caught.exception))

    def test_disconnecting_the_last_connection_enables_deregistration(self):
        session = self.open_session()
        registration = FakeRegistration()
        first, second = FakeConnection(), FakeConnection()
        session.register(registration)
        session.connect(first)
        session.connect(second)
        session.disconnect(first)
        with self.assertRaises(SessionOrderError):
            session.deregister(registration)
        session.disconnect(second)
        session.deregister(registration)

    def test_a_foreign_registration_or_connection_is_refused(self):
        session = self.open_session()
        with self.assertRaises(SessionOrderError):
            session.deregister(FakeRegistration())
        session.register(FakeRegistration())
        session.connect(FakeConnection())
        with self.assertRaises(SessionOrderError):
            session.disconnect(FakeConnection())

    def test_the_relaxed_mode_needs_its_evidence(self):
        with self.assertRaises(SessionOrderError):
            FabricMemRelaxation(evidence="   ")
        with self.assertRaises(SessionOrderError):
            FabricMemRelaxation(evidence="")
        FabricMemRelaxation(evidence="fence record 2026-10-30")

    def test_the_relaxed_mode_permits_deregister_while_connected(self):
        session = self.open_session(
            FabricMemRelaxation(evidence="qualified fence record")
        )
        registration = FakeRegistration()
        session.register(registration)
        connection = FakeConnection()
        session.connect(connection)
        # Permitted only because the relaxation carries its proof.
        session.deregister(registration)
        # The connection may still close afterwards: the relaxed order
        # must not strand an unclosable connection behind the phase.
        session.disconnect(connection)
        self.assertEqual(session.phase, "deregistered")

    def test_a_relaxed_deregister_keeps_the_session_connected(self):
        session = self.open_session(
            FabricMemRelaxation(evidence="qualified fence record")
        )
        registration = FakeRegistration()
        connection = FakeConnection()
        session.register(registration)
        session.connect(connection)
        session.deregister(registration)
        self.assertEqual(session.phase, "connected")

    def test_a_torn_down_session_may_register_again(self):
        session = self.open_session()
        registration = FakeRegistration()
        session.register(registration)
        connection = FakeConnection()
        session.connect(connection)
        session.disconnect(connection)
        session.deregister(registration)
        self.assertEqual(session.phase, "deregistered")
        # Renewal: a fully torn-down session starts a new cycle.
        renewal = FakeRegistration()
        session.register(renewal)
        self.assertEqual(session.phase, "registered")
        again = FakeConnection()
        session.connect(again)
        session.disconnect(again)
        session.deregister(renewal)
        self.assertEqual(session.phase, "deregistered")

    def test_connect_without_any_registration_is_refused(self):
        session = self.open_session()
        with self.assertRaises(SessionOrderError):
            session.connect(FakeConnection())


class TestTransportNegotiation(unittest.TestCase):
    def test_an_unanswered_option_is_an_explicit_rejection(self):
        negotiation = negotiate_transport(("rdma", "tcp"), {"tcp": True})
        negotiation.validate()
        self.assertEqual(negotiation.applied_transport_options, ("tcp",))
        rejected = negotiation.rejected[0]
        self.assertEqual(rejected.option, "rdma")
        self.assertIn("did not answer", rejected.reason)

    def test_a_fully_rejected_negotiation_stays_functional_only(self):
        negotiation = negotiate_transport(
            ("rdma",),
            {"rdma": "the reviewed Python API refuses transport options"},
        )
        negotiation.validate()
        self.assertTrue(negotiation.functional_only)
        self.assertEqual(negotiation.effective_backend, "unknown")
        self.assertFalse(negotiation.effective_observed)

    def test_an_observed_effective_backend_ends_functional_only(self):
        from gpustack_model_transfer.backends.hixl import TransportNegotiation

        negotiation = TransportNegotiation(
            requested_transport_options=("rdma",),
            applied_transport_options=("rdma",),
            effective_backend="rdma",
            effective_observed=True,
        )
        negotiation.validate()
        self.assertFalse(negotiation.functional_only)

    def test_an_observed_but_rejected_negotiation_stays_functional_only(self):
        from gpustack_model_transfer.backends.hixl import TransportNegotiation

        negotiation = TransportNegotiation(
            requested_transport_options=("rdma", "tcp"),
            applied_transport_options=("tcp",),
            effective_backend="tcp",
            effective_observed=True,
            rejected=(
                TransportOptionRejectedError
                and type("R", (), {"option": "rdma", "reason": "refused"})(),
            ),
        )
        negotiation.validate()
        # The requested RDMA path was refused: observed or not, the run
        # does not get to claim it.
        self.assertTrue(negotiation.functional_only)

    def test_an_unobserved_named_backend_is_refused(self):
        from gpustack_model_transfer.backends.hixl import TransportNegotiation

        with self.assertRaises(TransportOptionRejectedError):
            TransportNegotiation(
                requested_transport_options=("rdma",),
                applied_transport_options=("rdma",),
                effective_backend="rdma",
                effective_observed=False,
            ).validate()

    def test_an_applied_option_must_have_been_requested(self):
        from gpustack_model_transfer.backends.hixl import TransportNegotiation

        with self.assertRaises(TransportOptionRejectedError):
            TransportNegotiation(
                requested_transport_options=("rdma",),
                applied_transport_options=("rdma", "sm"),
                effective_backend="unknown",
                effective_observed=False,
            ).validate()


class TestRecoveryCapability(unittest.TestCase):
    def test_recovery_starts_disabled_and_blocks_promotion(self):
        recovery = RecoveryCapability()
        self.assertFalse(recovery.enabled)
        self.assertIn("no safe reclaim rule", recovery.status)

    def test_a_blocker_is_named_and_kept(self):
        recovery = RecoveryCapability()
        recovery.record_blocker("stale-replay: snapshot proves nothing")
        recovery.record_blocker("stale-replay: snapshot proves nothing")
        self.assertEqual(len(recovery.blockers), 1)
        self.assertIn("stale-replay", recovery.status)

    def test_enabling_needs_the_proof(self):
        recovery = RecoveryCapability()
        with self.assertRaises(ValueError):
            recovery.enable("  ")
        recovery.enable("qualified fence drained the replay generation")
        self.assertTrue(recovery.enabled)
        self.assertIn("qualified fence", recovery.status)

    def test_enabling_over_blockers_is_refused(self):
        recovery = RecoveryCapability()
        recovery.record_blocker("stale-replay: unobservable in this run")
        with self.assertRaises(ValueError) as caught:
            recovery.enable("a proof cannot stand on an unproven boundary")
        self.assertIn("stale-replay", str(caught.exception))
        self.assertFalse(recovery.enabled)


class TestSourceContextDefault(unittest.TestCase):
    def test_a_source_npu_context_is_required_by_default(self):
        from gpustack_model_transfer.backends.hixl import (
            HixlCapabilities,
            negotiate_transport,
        )

        capabilities = HixlCapabilities(
            negotiation=negotiate_transport(("rdma",), {"rdma": True})
        )
        capabilities.validate()
        self.assertTrue(
            capabilities.requires_source_npu_context,
            "the measured source initialized an NPU context; an "
            "accelerator-free source stays unqualified",
        )

    def test_dropping_the_source_npu_context_is_a_typed_refusal(self):
        from gpustack_model_transfer.backends.hixl import (
            HixlCapabilities,
            SourceNpuContextRequiredError,
            negotiate_transport,
        )

        with self.assertRaises(SourceNpuContextRequiredError):
            HixlCapabilities(
                negotiation=negotiate_transport(("rdma",), {"rdma": True}),
                requires_source_npu_context=False,
            )

    def test_window_and_inflight_ceilings_hold(self):
        from gpustack_model_transfer.backends.hixl import (
            HixlCapabilities,
            negotiate_transport,
        )

        base = negotiate_transport(("rdma",), {"rdma": True})
        with self.assertRaises(ValueError):
            HixlCapabilities(negotiation=base, window_bytes=(1 << 30) + 1)
        with self.assertRaises(ValueError):
            HixlCapabilities(negotiation=base, max_inflight=3)


if __name__ == "__main__":
    unittest.main()
