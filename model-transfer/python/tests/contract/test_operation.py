"""Per-operation states: completion, UNKNOWN retention, idempotent cancel."""

import unittest

from gpustack_model_transfer.core.operation import (
    Operation,
    OpState,
)


def pending_op() -> Operation:
    return Operation(op_id="op-1", kind="read_tensor", tensor="w", offset=0, length=48)


class TestOperationStates(unittest.TestCase):
    def test_new_operation_is_pending(self):
        op = pending_op()
        self.assertEqual(op.state, OpState.PENDING)
        self.assertFalse(op.state.terminal)

    def test_pending_completes_to_succeeded(self):
        op = pending_op()
        op.mark_succeeded()
        self.assertEqual(op.state, OpState.SUCCEEDED)
        self.assertTrue(op.state.terminal)
        self.assertFalse(op.retained)

    def test_pending_fails_with_typed_reason(self):
        op = pending_op()
        op.mark_failed("source-unavailable", "file gone")
        self.assertEqual(op.state, OpState.FAILED)
        self.assertEqual(op.error_code, "source-unavailable")

    def test_double_terminal_mark_of_same_state_is_noop(self):
        op = pending_op()
        op.mark_succeeded()
        op.mark_succeeded()
        self.assertEqual(op.state, OpState.SUCCEEDED)

    def test_conflicting_terminal_mark_rejected(self):
        op = pending_op()
        op.mark_succeeded()
        with self.assertRaises(Exception):
            op.mark_failed("x", "late failure")

    def test_cancel_from_pending(self):
        op = pending_op()
        op.mark_cancelled()
        self.assertEqual(op.state, OpState.CANCELLED)
        self.assertTrue(op.state.terminal)

    def test_cancel_is_idempotent(self):
        op = pending_op()
        op.mark_cancelled()
        op.mark_cancelled()
        self.assertEqual(op.state, OpState.CANCELLED)

    def test_cancel_after_completion_is_noop(self):
        op = pending_op()
        op.mark_succeeded()
        op.mark_cancelled()
        self.assertEqual(op.state, OpState.SUCCEEDED)

    def test_unknown_from_pending_retains_resources(self):
        op = pending_op()
        op.mark_unknown("backend state not terminal at deadline")
        self.assertEqual(op.state, OpState.UNKNOWN)
        self.assertTrue(op.retained)

    def test_unknown_is_not_terminal_and_not_releasable(self):
        op = pending_op()
        op.mark_unknown("deadline")
        self.assertFalse(op.state.terminal)
        self.assertFalse(op.can_release())

    def test_unknown_resolves_to_succeeded(self):
        op = pending_op()
        op.mark_unknown("deadline")
        op.resolve_unknown(OpState.SUCCEEDED)
        self.assertEqual(op.state, OpState.SUCCEEDED)
        self.assertFalse(op.retained)
        self.assertTrue(op.can_release())

    def test_unknown_resolves_to_failed(self):
        op = pending_op()
        op.mark_unknown("deadline")
        op.resolve_unknown(
            OpState.FAILED,
            error_code="source-unavailable",
            error_message="confirmed lost",
        )
        self.assertEqual(op.state, OpState.FAILED)
        self.assertEqual(op.error_code, "source-unavailable")
        self.assertTrue(op.can_release())

    def test_resolve_unknown_rejects_non_terminal_target(self):
        op = pending_op()
        op.mark_unknown("deadline")
        with self.assertRaises(Exception):
            op.resolve_unknown(OpState.PENDING)

    def test_resolve_on_pending_rejected(self):
        op = pending_op()
        with self.assertRaises(Exception):
            op.resolve_unknown(OpState.SUCCEEDED)

    def test_cancel_on_unknown_is_noop_and_retains(self):
        # An unknown-completion operation cannot be clean-cancelled; its
        # resources stay retained until a qualified fence resolves it.
        op = pending_op()
        op.mark_unknown("deadline")
        op.mark_cancelled()
        self.assertEqual(op.state, OpState.UNKNOWN)
        self.assertTrue(op.retained)

    def test_terminal_operations_are_releasable(self):
        for finalizer in [
            lambda o: o.mark_succeeded(),
            lambda o: o.mark_failed("x", "y"),
            lambda o: o.mark_cancelled(),
        ]:
            with self.subTest(finalizer=finalizer):
                op = pending_op()
                finalizer(op)
                self.assertTrue(op.can_release())


if __name__ == "__main__":
    unittest.main()
