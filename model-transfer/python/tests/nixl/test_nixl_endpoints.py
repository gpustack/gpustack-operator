"""Endpoint qualification, per-agent isolation, and the memlock gate."""

import os
import unittest

from gpustack_model_transfer.backends.nixl import (
    MemlockInsufficientError,
    UnqualifiedEndpointError,
    check_memlock,
    qualify_endpoints,
    require_memlock,
)


class TestQualifyEndpoints(unittest.TestCase):
    def test_inherited_wildcards_are_refused(self):
        for value in ("any", "mlx5_*", "mlx5_0:1,all", "mlx5_0:1,"):
            with self.subTest(value=value):
                with self.assertRaises(UnqualifiedEndpointError):
                    qualify_endpoints(
                        "tcp://10.0.0.1:9100", ["mlx5_0:1"], {"UCX_NET_DEVICES": value}
                    )

    def test_an_explicit_selection_is_qualified(self):
        selection = qualify_endpoints(
            "tcp://10.0.0.1:9100", ["mlx5_0:1", "mlx5_1:1"], {}
        )
        self.assertEqual(selection.control, "tcp://10.0.0.1:9100")
        self.assertEqual(selection.payload_rails, ("mlx5_0:1", "mlx5_1:1"))
        self.assertEqual(selection.direction, "write")

    def test_ucx_net_devices_all_is_refused(self):
        with self.assertRaises(UnqualifiedEndpointError):
            qualify_endpoints(
                "tcp://10.0.0.1:9100", ["mlx5_0:1"], {"UCX_NET_DEVICES": "all"}
            )

    def test_wildcard_and_unnamed_rails_are_refused(self):
        for rails in (
            ("all",),
            ("any",),
            ("all:1",),
            ("ANY:2",),
            ("mlx5_*",),
            ("",),
            (),
        ):
            with self.subTest(rails=rails):
                with self.assertRaises(UnqualifiedEndpointError):
                    qualify_endpoints("tcp://10.0.0.1:9100", rails, {})

    def test_a_selection_without_a_control_path_is_refused(self):
        with self.assertRaises(UnqualifiedEndpointError):
            qualify_endpoints("", ["mlx5_0:1"], {})

    def test_an_unknown_direction_is_refused(self):
        with self.assertRaises(UnqualifiedEndpointError):
            qualify_endpoints(
                "tcp://10.0.0.1:9100",
                ["mlx5_0:1"],
                {},
                direction="both",
            )

    def test_agent_settings_never_touch_the_process_environment(self):
        selection = qualify_endpoints(
            "tcp://10.0.0.1:9100", ["mlx5_0:1"], dict(os.environ)
        )
        settings = selection.agent_settings()
        self.assertIn("UCX_NET_DEVICES", settings)
        self.assertEqual(settings["UCX_NET_DEVICES"], "mlx5_0:1")
        # A NEW dictionary for one agent: the process keeps its own.
        self.assertIsNot(settings, os.environ)
        # The strongest check: nothing leaked into the real environment.
        snapshot = dict(os.environ)
        _ = selection.agent_settings()
        self.assertEqual(os.environ, snapshot)

    def test_a_live_engine_context_blocks_process_wide_changes(self):
        def live_contexts():
            return 1

        # UCX_TLS set for a live context: the selection must refuse rather
        # than disturb it.
        with self.assertRaises(UnqualifiedEndpointError):
            qualify_endpoints(
                "tcp://10.0.0.1:9100",
                ["mlx5_0:1"],
                {"UCX_TLS": "rc"},
                live_contexts=live_contexts,
            )
        # No live contexts: the same environment is fine, per agent.
        selection = qualify_endpoints(
            "tcp://10.0.0.1:9100",
            ["mlx5_0:1"],
            {"UCX_TLS": "rc"},
            live_contexts=lambda: 0,
        )
        self.assertEqual(selection.agent_settings()["UCX_NET_DEVICES"], "mlx5_0:1")


class TestMemlock(unittest.TestCase):
    def test_a_sufficient_lock_passes(self):
        check = check_memlock(4096, getrlimit=lambda _which: (8192, 8192))
        self.assertTrue(check.sufficient)
        self.assertFalse(check.unlimited)

    def test_an_unlimited_lock_passes(self):
        check = check_memlock(1 << 40, getrlimit=lambda _which: (-1, -1))
        self.assertTrue(check.sufficient)
        self.assertTrue(check.unlimited)

    def test_an_insufficient_lock_is_a_typed_refusal(self):
        with self.assertRaises(MemlockInsufficientError):
            require_memlock(1 << 30, getrlimit=lambda _which: (4096, 4096))
        check = check_memlock(1 << 30, getrlimit=lambda _which: (4096, 8192))
        self.assertFalse(check.sufficient)
        self.assertEqual(check.soft_limit, 4096)
        self.assertEqual(check.hard_limit, 8192)


if __name__ == "__main__":
    unittest.main()
