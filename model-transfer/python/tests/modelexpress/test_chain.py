"""Pinned constants, chain composition, and the offline guards."""

import os
import unittest

from gpustack_model_transfer.adapters.modelexpress import (
    BUILTIN_CHAIN,
    INSERT_BEFORE,
    LOAD_PLUGIN_VARIABLE,
    PINNED_COMMIT,
    apply_offline_guards,
    build_chain,
    check_composition,
    offline_guards_active,
)
from gpustack_model_transfer.adapters.modelexpress.chain import CompositionError


class TestPinnedRoute(unittest.TestCase):
    def test_the_hook_and_commit_are_pinned(self):
        self.assertEqual(LOAD_PLUGIN_VARIABLE, "MX_LOAD_PLUGIN")
        self.assertEqual(
            PINNED_COMMIT,
            "modelexpress@9410f3efbaa7aae57917d670de731a79f36684f3",
        )

    def test_the_chain_and_insertion_point_are_recorded(self):
        self.assertEqual(BUILTIN_CHAIN, ("p2p", "rdma", "server_cache"))
        self.assertEqual(INSERT_BEFORE, "rdma")


class TestChainComposition(unittest.TestCase):
    def test_the_plugin_is_inserted_before_rdma(self):
        chain = build_chain({"p2p": False, "rdma": True, "server_cache": False})
        self.assertEqual(chain, ("gpustack", "rdma"))

    def test_a_clean_chain_passes_the_composition_check(self):
        chain = check_composition({"p2p": False, "rdma": True, "server_cache": False})
        self.assertIn("gpustack", chain)
        self.assertNotIn("p2p", chain)
        self.assertNotIn("server_cache", chain)

    def test_a_reachable_earlier_p2p_strategy_is_refused(self):
        with self.assertRaises(CompositionError) as caught:
            check_composition({"p2p": True, "rdma": True, "server_cache": False})
        self.assertIn("p2p", str(caught.exception))
        self.assertIn("authorized", str(caught.exception))

    def test_an_authorized_earlier_strategy_is_permitted(self):
        chain = check_composition(
            {"p2p": True, "rdma": True, "server_cache": False},
            authorized={"p2p": "identity-audited for this composition"},
        )
        self.assertIn("p2p", chain)
        self.assertLess(chain.index("gpustack"), chain.index("rdma"))

    def test_a_reachable_later_strategy_is_refused(self):
        # server_cache after the plugin bypasses delivery constraints
        # under either policy; the composition refuses rather than audit.
        with self.assertRaises(CompositionError) as caught:
            check_composition({"p2p": False, "rdma": True, "server_cache": True})
        self.assertIn("server_cache", str(caught.exception))
        self.assertIn("identity", str(caught.exception))

    def test_a_chain_without_rdma_still_places_the_plugin(self):
        chain = check_composition({"p2p": False, "rdma": False, "server_cache": False})
        self.assertEqual(chain, ("gpustack",))


class TestOfflineGuards(unittest.TestCase):
    def test_the_guards_are_the_two_documented_variables(self):
        from gpustack_model_transfer.adapters.modelexpress import OFFLINE_GUARDS

        self.assertEqual(
            OFFLINE_GUARDS,
            ("HF_HUB_OFFLINE=1", "TRANSFORMERS_OFFLINE=1"),
        )

    def test_applying_the_guards_touches_a_copy_only(self):
        env = {"OTHER": "1"}
        guarded = apply_offline_guards(env)
        self.assertEqual(guarded["HF_HUB_OFFLINE"], "1")
        self.assertEqual(guarded["TRANSFORMERS_OFFLINE"], "1")
        self.assertNotIn("HF_HUB_OFFLINE", env)
        self.assertNotIn("HF_HUB_OFFLINE", os.environ)

    def test_the_guards_are_detectable(self):
        self.assertFalse(offline_guards_active({}))
        self.assertTrue(
            offline_guards_active({"HF_HUB_OFFLINE": "1", "TRANSFORMERS_OFFLINE": "1"})
        )


if __name__ == "__main__":
    unittest.main()
