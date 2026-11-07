"""Capability records: direction candidate, rejected options, rail pairs."""

import unittest

from gpustack_model_transfer.backends.nixl import (
    DIRECTION_WRITE,
    QualifiedRailPair,
    RailQualifier,
    UnqualifiedEndpointError,
    nixl_backend_capabilities,
)
from gpustack_model_transfer.core.capabilities import (
    InvalidCapabilitiesError,
    RejectedOption,
)


class TestNixlCapabilities(unittest.TestCase):
    def test_requested_applied_effective_are_distinct(self):
        from gpustack_model_transfer.backends.nixl import load_runtime
        from gpustack_model_transfer.backends.nixl.testing import FakeNixlAPI

        api = load_runtime(api=FakeNixlAPI())
        record = nixl_backend_capabilities(
            requested=("ucx", "tcp"),
            applied="ucx",
            effective="ucx",
            effective_observed=True,
            rejected=(RejectedOption("tcp", "no verbs devices on the granted set"),),
            qualified_rail_pairs=_pair(),
        )
        record.validate()
        self.assertEqual(record.base.requested, ("ucx", "tcp"))
        self.assertEqual(record.base.applied, "ucx")
        self.assertEqual(record.base.effective, "ucx")
        self.assertTrue(record.base.effective_observed)
        self.assertEqual(
            record.base.rejected[0].reason, "no verbs devices on the granted set"
        )
        _ = api

    def test_an_unobserved_effective_backend_stays_unknown(self):
        record = nixl_backend_capabilities(
            requested=("ucx",),
            applied="ucx",
            effective="unknown",
            effective_observed=False,
            qualified_rail_pairs=_pair(),
        )
        record.validate()
        with self.assertRaises(InvalidCapabilitiesError):
            nixl_backend_capabilities(
                requested=("ucx",),
                applied="ucx",
                effective="ucx",
                effective_observed=False,
                qualified_rail_pairs=_pair(),
            )

    def test_the_direction_is_a_candidate_never_an_answer(self):
        record = nixl_backend_capabilities(
            requested=("ucx",),
            applied="ucx",
            effective="ucx",
            effective_observed=True,
            direction=DIRECTION_WRITE,
            qualified_rail_pairs=_pair(),
        )
        self.assertEqual(record.direction, "write")
        with self.assertRaises(UnqualifiedEndpointError):
            nixl_backend_capabilities(
                requested=("ucx",),
                applied="ucx",
                effective="ucx",
                effective_observed=True,
                direction="bidirectional",
                qualified_rail_pairs=_pair(),
            )

    def test_a_rejected_option_may_not_be_the_applied_one(self):
        with self.assertRaises(InvalidCapabilitiesError):
            nixl_backend_capabilities(
                requested=("ucx", "tcp"),
                applied="tcp",
                effective="tcp",
                effective_observed=True,
                rejected=(RejectedOption("tcp", "no verbs devices"),),
                qualified_rail_pairs=_pair(),
            )


class TestRailQualification(unittest.TestCase):
    def test_a_pair_is_advertised_only_from_a_passing_probe(self):
        qualifier = RailQualifier()
        passed = qualifier.probe_pair(
            "mlx5_0:1", "mlx5_1:1", lambda s, t: (True, ("xmit=1",))
        )
        failed = qualifier.probe_pair("mlx5_0:1", "mlx5_2:1", lambda s, t: (False, ()))
        self.assertTrue(passed.probe_passed)
        self.assertFalse(failed.probe_passed)
        self.assertEqual(qualifier.qualified_pairs(), (passed,))

    def test_a_probe_error_fails_the_pair(self):
        qualifier = RailQualifier()

        def probe(source, target):
            raise RuntimeError("rail unreachable")

        pair = qualifier.probe_pair("mlx5_0:1", "mlx5_9:1", probe)
        self.assertFalse(pair.probe_passed)
        self.assertIn("probe error", pair.counters[0])

    def test_a_record_without_any_qualified_pair_is_refused(self):
        # Either refusal is correct: the payload path is empty (core) and
        # no pair was qualified (the route's own rule).
        with self.assertRaises((UnqualifiedEndpointError, InvalidCapabilitiesError)):
            nixl_backend_capabilities(
                requested=("ucx",),
                applied="ucx",
                effective="ucx",
                effective_observed=True,
            )

    def test_an_unqualified_pair_cannot_enter_capabilities(self):
        with self.assertRaises(UnqualifiedEndpointError):
            nixl_backend_capabilities(
                requested=("ucx",),
                applied="ucx",
                effective="ucx",
                effective_observed=True,
                qualified_rail_pairs=(
                    QualifiedRailPair(
                        source_rail="mlx5_0:1",
                        target_rail="mlx5_9:1",
                        probe_passed=False,
                    ),
                ),
            )

    def test_counters_ride_the_pair_as_attribution_only(self):
        qualifier = RailQualifier()
        pair = qualifier.probe_pair(
            "mlx5_0:1", "mlx5_1:1", lambda s, t: (True, ("port_xmit_data=4096",))
        )
        self.assertEqual(pair.counters, ("port_xmit_data=4096",))
        # The counters describe the probe's own traffic; a second pair
        # starts from zero evidence whatever the first showed.
        other = qualifier.probe_pair("mlx5_1:1", "mlx5_0:1", lambda s, t: (False, ()))
        self.assertFalse(other.probe_passed)
        self.assertEqual(other.counters, ())


if __name__ == "__main__":
    unittest.main()


def _pair():
    return (
        QualifiedRailPair(
            source_rail="mlx5_0:1", target_rail="mlx5_1:1", probe_passed=True
        ),
    )
