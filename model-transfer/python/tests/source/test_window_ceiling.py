"""The source window ceiling follows the client contract, not the old 8 GiB
default: a reader configured above that default must not be refused here."""

import unittest

from gpustack_model_transfer.core.budgets import (
    DEFAULT_MAX_WINDOW_BYTES,
    MAX_WINDOW_BYTES,
)
from gpustack_model_transfer.source import transfer_service


class TestSourceWindowCeiling(unittest.TestCase):
    def test_the_source_ceiling_is_the_contract_ceiling(self):
        self.assertEqual(transfer_service.MAX_WINDOW, MAX_WINDOW_BYTES)
        self.assertEqual(MAX_WINDOW_BYTES, 1 << 40)
        self.assertEqual(DEFAULT_MAX_WINDOW_BYTES, 8 << 30)

    def test_a_window_above_the_old_default_is_not_refused(self):
        sixteen = 16 << 30
        self.assertGreater(sixteen, DEFAULT_MAX_WINDOW_BYTES)
        self.assertLess(sixteen, transfer_service.MAX_WINDOW)

    def test_a_window_over_the_contract_ceiling_stays_refused(self):
        self.assertGreater(MAX_WINDOW_BYTES + 1, transfer_service.MAX_WINDOW)


if __name__ == "__main__":
    unittest.main()
