"""The policy: Require terminates, Prefer downgrades, mutation is honest."""

import unittest

from gpustack_model_transfer.adapters.modelexpress import (
    POLICY_PREFER,
    POLICY_REQUIRE,
    FallbackDecision,
    StrategyRecoveryError,
    WriterRefitFence,
    decide_fallback,
    route_downgrade,
)


class TestRequirePolicy(unittest.TestCase):
    def test_a_missing_plugin_terminates_before_any_strategy(self):
        decision = decide_fallback(POLICY_REQUIRE, mutated=False, plugin_present=False)
        self.assertTrue(decision.terminal)
        self.assertIn("terminates", decision.reason)

    def test_a_failed_plugin_terminates_before_mutation(self):
        decision = decide_fallback(POLICY_REQUIRE, mutated=False, plugin_present=True)
        self.assertTrue(decision.terminal)

    def test_a_failure_after_mutation_is_terminal(self):
        decision = decide_fallback(POLICY_REQUIRE, mutated=True, plugin_present=True)
        self.assertTrue(decision.terminal)
        self.assertIn("partially mutated", decision.reason)


class TestPreferPolicy(unittest.TestCase):
    def test_a_missing_plugin_permits_a_routed_downgrade(self):
        decision = decide_fallback(POLICY_PREFER, mutated=False, plugin_present=False)
        self.assertFalse(decision.terminal)
        self.assertEqual(route_downgrade(decision), "verified-node-delivery")

    def test_a_pre_mutation_failure_permits_a_routed_downgrade(self):
        decision = decide_fallback(POLICY_PREFER, mutated=False, plugin_present=True)
        self.assertFalse(decision.terminal)
        self.assertTrue(decision.routed)

    def test_a_post_mutation_failure_is_terminal_under_prefer_too(self):
        decision = decide_fallback(POLICY_PREFER, mutated=True, plugin_present=True)
        self.assertTrue(decision.terminal)
        self.assertIn("not clean fallback", decision.reason)

    def test_a_terminal_decision_has_no_downgrade_route(self):
        decision = FallbackDecision(terminal=True, reason="forbidden")
        with self.assertRaises(StrategyRecoveryError):
            route_downgrade(decision)

    def test_a_decision_without_a_route_has_no_downgrade(self):
        decision = FallbackDecision(terminal=False, reason="allowed", routed="")
        with self.assertRaises(StrategyRecoveryError):
            route_downgrade(decision)


class TestWriterRefitFence(unittest.TestCase):
    def test_checkpoint_identity_and_live_generations_stay_apart(self):
        fence = WriterRefitFence()
        # A live processed-tensor generation is never publishable,
        # whatever its writer state.
        fence.begin_write("rank-0")
        self.assertFalse(fence.publishable())
        fence.finish("rank-0")
        self.assertFalse(fence.publishable())
        self.assertTrue(fence.readable())

    def test_one_writer_at_a_time(self):
        fence = WriterRefitFence()
        fence.begin_write("rank-0")
        with self.assertRaises(StrategyRecoveryError):
            fence.begin_write("rank-1")

    def test_only_the_writer_finishes(self):
        fence = WriterRefitFence()
        fence.begin_write("rank-0")
        with self.assertRaises(StrategyRecoveryError):
            fence.finish("rank-1")

    def test_a_refit_underway_is_not_readable(self):
        fence = WriterRefitFence()
        fence.begin_refit("rank-0")
        self.assertFalse(fence.readable())
        fence.finish("rank-0")
        self.assertTrue(fence.readable())


if __name__ == "__main__":
    unittest.main()
