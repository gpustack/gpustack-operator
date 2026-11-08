"""Pinned constants, chain composition, and the delivery-policy filter."""

import unittest

from gpustack_model_transfer.adapters.modelexpress import (
    BUILTIN_CHAIN,
    DELIVERY_POLICY_VARIABLE,
    INSERT_BEFORE,
    LOAD_PLUGIN_VARIABLE,
    PINNED_COMMIT,
    PLUGIN_NAME,
    CompositionError,
    build_chain,
    check_composition,
    filter_chain_strategies,
)


class FakeStrategy:
    def __init__(self, name):
        self.name = name


class TestPinnedRoute(unittest.TestCase):
    def test_the_hook_and_commit_are_pinned(self):
        self.assertEqual(LOAD_PLUGIN_VARIABLE, "MX_LOAD_PLUGIN")
        self.assertEqual(
            PINNED_COMMIT,
            "modelexpress@9410f3efbaa7aae57917d670de731a79f36684f3",
        )

    def test_the_formats_and_insertion_point_are_recorded(self):
        from gpustack_model_transfer.adapters.modelexpress import (
            LOAD_FORMAT,
            LOAD_FORMAT_ALIASES,
        )

        self.assertEqual(LOAD_FORMAT, "modelexpress")
        self.assertEqual(LOAD_FORMAT_ALIASES, ("modelexpress", "mx"))
        self.assertEqual(PLUGIN_NAME, "gpustack")
        self.assertEqual(INSERT_BEFORE, "server-cache")

    def test_the_insertion_point_names_the_upstream_strategy_exactly(self):
        # The pinned upstream names its cache strategy "server-cache";
        # the launch env must match it exactly or the chain refuses the
        # plugin hook's insertion point.
        self.assertIn(INSERT_BEFORE, BUILTIN_CHAIN)
        self.assertEqual(BUILTIN_CHAIN.index(INSERT_BEFORE), 1)

    def test_the_builtin_chain_is_the_reviewed_upstream_order(self):
        self.assertEqual(
            BUILTIN_CHAIN,
            (
                "rdma",
                "server-cache",
                "instant_tensor",
                "model_streamer",
                "gds",
                "default",
            ),
        )

    def test_the_delivery_policy_variable_is_declared(self):
        self.assertEqual(
            DELIVERY_POLICY_VARIABLE, "GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY"
        )


class TestChainComposition(unittest.TestCase):
    def test_the_plugin_is_inserted_before_the_local_cache(self):
        chain = build_chain({"rdma": True, "server_cache": True, "default": True})
        self.assertEqual(chain, ("rdma", "gpustack", "server-cache", "default"))

    def test_the_upstream_cache_spelling_composes_the_same_chain(self):
        chain = build_chain({"rdma": True, "server-cache": True, "default": True})
        self.assertEqual(chain, ("rdma", "gpustack", "server-cache", "default"))

    def test_the_plugin_precedes_every_fallback_and_follows_rdma(self):
        chain = check_composition(build_chain({"rdma": True, "server_cache": True}))
        self.assertLess(chain.index("rdma"), chain.index(PLUGIN_NAME))
        self.assertLess(chain.index(PLUGIN_NAME), chain.index("server-cache"))

    def test_a_chain_without_rdma_still_places_the_plugin(self):
        chain = check_composition(build_chain({"server_cache": True, "default": True}))
        self.assertEqual(chain, ("gpustack", "server-cache", "default"))

    def test_a_fallback_ahead_of_the_plugin_is_refused(self):
        with self.assertRaises(CompositionError):
            check_composition(("rdma", "server-cache", "gpustack", "default"))

    def test_rdma_behind_the_plugin_is_refused(self):
        with self.assertRaises(CompositionError):
            check_composition(("gpustack", "rdma", "server-cache"))

    def test_a_chain_without_the_plugin_is_refused(self):
        with self.assertRaises(CompositionError):
            check_composition(("rdma", "server-cache", "default"))


class TestDeliveryPolicyFilter(unittest.TestCase):
    def setUp(self):
        import os

        self._saved = dict(os.environ)
        self.addCleanup(lambda: os.environ.update({**self._saved}))
        for name in ("MX_LOAD_PLUGIN", DELIVERY_POLICY_VARIABLE):
            os.environ.pop(name, None)

    def test_prefer_keeps_the_full_chain(self):
        import os

        os.environ["MX_LOAD_PLUGIN"] = "gpustack"
        strategies = [
            FakeStrategy("rdma"),
            FakeStrategy("server-cache"),
            FakeStrategy("default"),
        ]
        filtered = filter_chain_strategies(strategies)
        self.assertEqual(
            [s.name for s in filtered],
            ["rdma", "gpustack", "server-cache", "default"],
        )

    def test_require_strips_the_local_strategies(self):
        import os

        os.environ["MX_LOAD_PLUGIN"] = "gpustack"
        os.environ[DELIVERY_POLICY_VARIABLE] = "Require"
        strategies = [
            FakeStrategy("rdma"),
            FakeStrategy("server-cache"),
            FakeStrategy("default"),
        ]
        filtered = filter_chain_strategies(strategies)
        self.assertEqual([s.name for s in filtered], ["rdma", "gpustack"])

    def test_an_unrequested_process_keeps_its_own_chain(self):
        strategies = [
            FakeStrategy("rdma"),
            FakeStrategy("server-cache"),
            FakeStrategy("default"),
        ]
        self.assertEqual(filter_chain_strategies(strategies), strategies)

    def test_an_unrequested_process_keeps_its_own_chain_under_require(self):
        """Stripping fallbacks without placing the plugin strands the load."""
        import os

        os.environ[DELIVERY_POLICY_VARIABLE] = "Require"
        os.environ.pop("GPUSTACK_MODEL_TRANSFER_LOAD_FORMAT", None)
        strategies = [
            FakeStrategy("rdma"),
            FakeStrategy("server-cache"),
            FakeStrategy("default"),
        ]
        self.assertEqual(filter_chain_strategies(strategies), strategies)

    def test_rdma_behind_a_fallback_is_normalized_to_the_front(self):
        """Peer D2D must be tried first, whatever order the build composed."""
        import os

        os.environ["MX_LOAD_PLUGIN"] = "gpustack"
        strategies = [FakeStrategy("instant_tensor"), FakeStrategy("rdma")]
        placed = filter_chain_strategies(strategies)
        self.assertEqual(
            [strategy.name for strategy in placed],
            ["rdma", PLUGIN_NAME, "instant_tensor"],
        )

    def test_require_strips_to_rdma_first_after_normalization(self):
        """The Require remainder must not put the plugin ahead of Peer D2D."""
        import os

        os.environ["MX_LOAD_PLUGIN"] = "gpustack"
        os.environ[DELIVERY_POLICY_VARIABLE] = "Require"
        strategies = [FakeStrategy("instant_tensor"), FakeStrategy("rdma")]
        placed = filter_chain_strategies(strategies)
        self.assertEqual([strategy.name for strategy in placed], ["rdma", PLUGIN_NAME])

    def test_an_already_ordered_rdma_is_not_moved(self):
        import os

        os.environ["MX_LOAD_PLUGIN"] = "gpustack"
        strategies = [FakeStrategy("rdma"), FakeStrategy("server-cache")]
        placed = filter_chain_strategies(strategies)
        self.assertEqual(
            [strategy.name for strategy in placed],
            ["rdma", PLUGIN_NAME, "server-cache"],
        )

    def test_the_filter_refuses_a_chain_that_violates_the_invariant(self):
        """A composed chain that breaks placement fails loudly, not silently."""
        import os
        from unittest.mock import patch

        os.environ["MX_LOAD_PLUGIN"] = "gpustack"
        with patch(
            "gpustack_model_transfer.adapters.modelexpress.chain.place_gpustack_strategy",
            return_value=[
                FakeStrategy("default"),
                FakeStrategy(PLUGIN_NAME),
                FakeStrategy("rdma"),
            ],
        ):
            with self.assertRaises(CompositionError):
                filter_chain_strategies([FakeStrategy("rdma")])

    def test_apply_offline_guards_returns_a_new_mapping(self):
        from gpustack_model_transfer.adapters.modelexpress import (
            apply_offline_guards,
        )

        source = {}
        guarded = apply_offline_guards(source)
        self.assertEqual(guarded["HF_HUB_OFFLINE"], "1")
        self.assertEqual(guarded["TRANSFORMERS_OFFLINE"], "1")
        self.assertEqual(source, {})

    def test_offline_guards_active_reads_the_environment(self):
        from gpustack_model_transfer.adapters.modelexpress import (
            offline_guards_active,
        )

        self.assertFalse(offline_guards_active(env={}))
        self.assertTrue(
            offline_guards_active(
                env={"HF_HUB_OFFLINE": "1", "TRANSFORMERS_OFFLINE": "1"}
            )
        )


if __name__ == "__main__":
    unittest.main()
