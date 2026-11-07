"""Budgets: tensor and window budgets are separate limits."""

import unittest
from unittest.mock import patch

from gpustack_model_transfer.core.budgets import (
    DEFAULT_MAX_WINDOW_BYTES,
    DEFAULT_MIN_SOURCE_WINDOW_BYTES,
    DEFAULT_TRANSFER_WORKERS,
    MAX_INFLIGHT_OPERATIONS,
    MAX_WINDOW_BYTES,
    MIN_SOURCE_WINDOW_BYTES,
    MIN_WINDOW_BYTES,
    TP_LOAD_MODE_ALL,
    TP_LOAD_MODE_BROADCAST,
    ReadBudgets,
    WindowSlots,
    detect_cpu_threads,
    effective_window_bytes,
    max_window_bytes,
    min_source_window_bytes,
    tp_load_mode,
    transfer_workers,
)
from gpustack_model_transfer.core.errors import BudgetError


class TestReadBudgets(unittest.TestCase):
    def test_limits_match_contract_ceiling(self):
        self.assertEqual(MAX_WINDOW_BYTES, 1 << 40)
        self.assertEqual(DEFAULT_MAX_WINDOW_BYTES, 8 << 30)
        self.assertEqual(MIN_WINDOW_BYTES, 1 << 30)
        self.assertEqual(MAX_INFLIGHT_OPERATIONS, 256)

    def test_max_window_bytes_env_and_default(self):
        self.assertEqual(max_window_bytes({}), 8 << 30)
        self.assertEqual(
            max_window_bytes(
                {"GPUSTACK_MODEL_TRANSFER_MAX_WINDOW_BYTES": str(4 << 30)}
            ),
            4 << 30,
        )
        with self.assertRaises(Exception):
            max_window_bytes(
                {"GPUSTACK_MODEL_TRANSFER_MAX_WINDOW_BYTES": str(MAX_WINDOW_BYTES + 1)}
            )

    def test_under_floor_window_silently_resolves_up(self):
        for raw in ("0", "-16", str(1 << 20)):
            self.assertEqual(
                max_window_bytes({"GPUSTACK_MODEL_TRANSFER_MAX_WINDOW_BYTES": raw}),
                MIN_WINDOW_BYTES,
                f"{raw} silently resolves up to the 1 GiB floor",
            )

    def test_larger_configured_windows_are_legal(self):
        for window in (8 << 30, 16 << 30, 64 << 30, 128 << 30):
            self.assertEqual(
                max_window_bytes(
                    {"GPUSTACK_MODEL_TRANSFER_MAX_WINDOW_BYTES": str(window)}
                ),
                window,
            )
            ReadBudgets(
                window_bytes=window, max_inflight=2, owned_output_bytes=window
            ).validate()

    def test_valid_budgets(self):
        ReadBudgets(
            window_bytes=65536, max_inflight=2, owned_output_bytes=1024 * 1024
        ).validate()

    def test_window_ceiling_enforced(self):
        with self.assertRaises(Exception):
            ReadBudgets(
                window_bytes=MAX_WINDOW_BYTES + 1,
                max_inflight=1,
                owned_output_bytes=1024,
            ).validate()

    def test_window_must_be_positive(self):
        with self.assertRaises(Exception):
            ReadBudgets(
                window_bytes=0, max_inflight=1, owned_output_bytes=1024
            ).validate()

    def test_inflight_ceiling_enforced(self):
        with self.assertRaises(Exception):
            ReadBudgets(
                window_bytes=1024,
                max_inflight=MAX_INFLIGHT_OPERATIONS + 1,
                owned_output_bytes=1024,
            ).validate()

    def test_transfer_workers_env_and_cpu_fallback(self):
        self.assertEqual(transfer_workers({"GPUSTACK_MODEL_TRANSFER_WORKERS": "8"}), 8)
        with self.assertRaises(Exception):
            transfer_workers({"GPUSTACK_MODEL_TRANSFER_WORKERS": "0"})
        with (
            patch(
                "gpustack_model_transfer.core.budgets.os.sched_getaffinity",
                side_effect=OSError("unavailable"),
                create=True,
            ),
            patch(
                "gpustack_model_transfer.core.budgets.os.cpu_count", return_value=None
            ),
        ):
            self.assertEqual(detect_cpu_threads(), DEFAULT_TRANSFER_WORKERS)
            self.assertEqual(transfer_workers({}), 4)

    def test_owned_output_budget_may_exceed_window(self):
        # The owned-output budget and the window budget are independent: a
        # tensor larger than one window is fine when it fits the output budget.
        ReadBudgets(
            window_bytes=1024, max_inflight=2, owned_output_bytes=1024 * 1024
        ).validate()

    def test_owned_output_budget_must_be_positive(self):
        with self.assertRaises(Exception):
            ReadBudgets(
                window_bytes=1024, max_inflight=1, owned_output_bytes=0
            ).validate()


class TestWindowSlots(unittest.TestCase):
    def test_acquire_and_free(self):
        slots = WindowSlots(count=2)
        a = slots.acquire()
        b = slots.acquire()
        slots.bind(a, "op-1")
        slots.bind(b, "op-2")
        slots.mark_terminal("op-1")
        freed = slots.free(a, "op-1")
        self.assertTrue(freed)

    def test_exhausted_slots_raise_capacity_error(self):
        slots = WindowSlots(count=1)
        slots.acquire()
        with self.assertRaises(Exception):
            slots.acquire()

    def test_slot_not_reusable_until_operation_terminal(self):
        slots = WindowSlots(count=1)
        slot = slots.acquire()
        slots.bind(slot, "op-1")
        with self.assertRaises(Exception):
            slots.free(slot, "op-1")

    def test_terminal_operation_frees_its_slot(self):
        slots = WindowSlots(count=1)
        slot = slots.acquire()
        slots.bind(slot, "op-1")
        slots.mark_terminal("op-1")
        self.assertTrue(slots.free(slot, "op-1"))
        slots.acquire()  # reusable now


if __name__ == "__main__":
    unittest.main()


class TestMinSourceWindowBytes(unittest.TestCase):
    def test_floor_matches_go_contract(self):
        """The floor mirrors protocol.DefaultWindowBytes on the Go side: 1 GiB."""
        self.assertEqual(MIN_SOURCE_WINDOW_BYTES, 1 << 30)
        self.assertEqual(DEFAULT_MIN_SOURCE_WINDOW_BYTES, 1 << 30)

    def test_default_when_env_unset(self):
        self.assertEqual(min_source_window_bytes({}), 1 << 30)

    def test_floor_is_fixed_even_when_env_is_set(self):
        self.assertEqual(
            min_source_window_bytes(
                {"GPUSTACK_MODEL_TRANSFER_MIN_SOURCE_WINDOW_BYTES": str(2 << 30)}
            ),
            1 << 30,
        )

    def test_invalid_and_nonpositive_fall_back_to_default(self):
        self.assertEqual(
            min_source_window_bytes(
                {"GPUSTACK_MODEL_TRANSFER_MIN_SOURCE_WINDOW_BYTES": "nonsense"}
            ),
            1 << 30,
        )
        self.assertEqual(
            min_source_window_bytes(
                {"GPUSTACK_MODEL_TRANSFER_MIN_SOURCE_WINDOW_BYTES": "0"}
            ),
            1 << 30,
        )
        self.assertEqual(
            min_source_window_bytes(
                {"GPUSTACK_MODEL_TRANSFER_MIN_SOURCE_WINDOW_BYTES": "-4"}
            ),
            1 << 30,
        )


class TestTPLoadMode(unittest.TestCase):
    def test_default_is_broadcast_when_env_unset(self):
        self.assertEqual(tp_load_mode({}), TP_LOAD_MODE_BROADCAST)

    def test_empty_value_is_broadcast(self):
        for raw in ("", "   ", None):
            env = {} if raw is None else {"GPUSTACK_MODEL_TRANSFER_TP_MODE": raw}
            self.assertEqual(tp_load_mode(env), TP_LOAD_MODE_BROADCAST)

    def test_all_aliases_normalize_to_all(self):
        for raw in ("all", "all_tp", "all-tp", "all_tps", "ALL", "All_TP", " all "):
            mode = tp_load_mode({"GPUSTACK_MODEL_TRANSFER_TP_MODE": raw})
            self.assertEqual(mode, TP_LOAD_MODE_ALL, f"{raw!r} means all")

    def test_broadcast_aliases_normalize_to_broadcast(self):
        for raw in ("broadcast", "tp0", "TP0", "Broadcast"):
            mode = tp_load_mode({"GPUSTACK_MODEL_TRANSFER_TP_MODE": raw})
            self.assertEqual(mode, TP_LOAD_MODE_BROADCAST, f"{raw!r} means broadcast")

    def test_unknown_values_raise(self):
        for raw in ("every", "tp1", "all-for-one"):
            with self.assertRaises(BudgetError):
                tp_load_mode({"GPUSTACK_MODEL_TRANSFER_TP_MODE": raw})


class TestEffectiveWindowBytes(unittest.TestCase):
    def test_broadcast_mode_keeps_the_full_ceiling_per_rank(self):
        self.assertEqual(
            effective_window_bytes({}, tp_size=8), DEFAULT_MAX_WINDOW_BYTES
        )
        self.assertEqual(
            effective_window_bytes(
                {"GPUSTACK_MODEL_TRANSFER_TP_MODE": "broadcast"}, tp_size=8
            ),
            DEFAULT_MAX_WINDOW_BYTES,
        )

    def test_all_mode_with_a_lone_rank_keeps_the_full_ceiling(self):
        self.assertEqual(
            effective_window_bytes(
                {"GPUSTACK_MODEL_TRANSFER_TP_MODE": "all"}, tp_size=1
            ),
            DEFAULT_MAX_WINDOW_BYTES,
        )

    def test_all_mode_splits_the_ceiling_across_the_ranks(self):
        self.assertEqual(
            effective_window_bytes(
                {"GPUSTACK_MODEL_TRANSFER_TP_MODE": "all"}, tp_size=8
            ),
            1 << 30,
            "8 GiB / 8 ranks = 1 GiB",
        )
        self.assertEqual(
            effective_window_bytes(
                {
                    "GPUSTACK_MODEL_TRANSFER_TP_MODE": "all",
                    "GPUSTACK_MODEL_TRANSFER_MAX_WINDOW_BYTES": str(8 << 30),
                },
                tp_size=4,
            ),
            2 << 30,
            "8 GiB / 4 ranks = 2 GiB",
        )

    def test_all_mode_split_is_floored_at_the_1_gib_minimum(self):
        self.assertEqual(
            effective_window_bytes(
                {"GPUSTACK_MODEL_TRANSFER_TP_MODE": "all"}, tp_size=64
            ),
            MIN_WINDOW_BYTES,
            "8 GiB / 64 ranks is under the floor, so the floor holds",
        )

    def test_explicit_tp_mode_argument_wins_over_env(self):
        self.assertEqual(
            effective_window_bytes(
                {"GPUSTACK_MODEL_TRANSFER_TP_MODE": "broadcast"},
                tp_size=8,
                tp_mode="all",
            ),
            1 << 30,
        )
        self.assertEqual(
            effective_window_bytes(
                {"GPUSTACK_MODEL_TRANSFER_TP_MODE": "all"},
                tp_size=8,
                tp_mode="broadcast",
            ),
            DEFAULT_MAX_WINDOW_BYTES,
        )
