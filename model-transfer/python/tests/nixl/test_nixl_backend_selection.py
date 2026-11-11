"""The NIXL transport plugin selection, end to end.

One agent setting travels from one environment variable and one sysfs
directory to both ends of the transfer: the resolver in envs, the source
holder's agent, and the target's agent. The tests pin the whole path with
LITERAL plugin names -- "UCX" and "LIBFABRIC" -- because the plugin names
are the contract with the NIXL installation, and an assertion built from
the constant under test would pass whatever that constant held.
"""

import base64
import sys
import threading
from types import SimpleNamespace

import pytest

from gpustack_model_transfer import envs
from gpustack_model_transfer.client_native import TargetNative
from gpustack_model_transfer.core.errors import SourceUnavailableError
from gpustack_model_transfer.source.backend import NixlSourceBackend


@pytest.fixture(autouse=True)
def clean_plugin_env(monkeypatch):
    """Every test decides its own plugin inputs, so the process environment
    never leaks one in: an unset variable is the auto-detect rows' subject,
    and a leaked value from the developer's shell would turn those rows into
    tests of whatever their machine exports."""
    monkeypatch.delenv("GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND", raising=False)
    monkeypatch.delenv("UCX_NET_DEVICES", raising=False)
    monkeypatch.delenv("GPUSTACK_RDMA_DEVICES", raising=False)


def sysfs(tmp_path, devices):
    """Build a fake /sys/class/infiniband tree: each entry is a device
    directory name, and a trailing dict holds the files written beneath it."""
    root = tmp_path / "infiniband"
    for name, files in devices:
        dev = root / name
        dev.mkdir(parents=True)
        for file_name, content in (files or {}).items():
            target = dev
            for part in file_name.split("/"):
                target = target / part
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(content)
    return str(root)


class TestResolveNixlBackend:
    def test_explicit_ucx_and_libfabric_case_and_whitespace(self):
        assert (
            envs.resolve_nixl_backend({"GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND": "UCX"})
            == "UCX"
        )
        assert (
            envs.resolve_nixl_backend(
                {"GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND": " libfabric "}
            )
            == "LIBFABRIC"
        )
        assert (
            envs.resolve_nixl_backend(
                {"GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND": "LibFabric"}
            )
            == "LIBFABRIC"
        )
        assert (
            envs.resolve_nixl_backend(
                {"GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND": "  ucx "}
            )
            == "UCX"
        )

    def test_empty_value_auto_detects(self, tmp_path):
        root = sysfs(tmp_path, [("rdmap0", {"device/vendor": "0x1d0f"})])
        assert (
            envs.resolve_nixl_backend(
                {"GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND": ""}, root
            )
            == "LIBFABRIC"
        )

    def test_invalid_value_raises_source_unavailable(self):
        with pytest.raises(SourceUnavailableError) as raised:
            envs.resolve_nixl_backend({"GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND": "tcp"})
        assert "unsupported GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND 'tcp'" in str(
            raised.value
        )
        assert "supported: UCX, LIBFABRIC" in str(raised.value)

    def test_efa_vendor_device_alone_selects_libfabric(self, tmp_path):
        root = sysfs(tmp_path, [("infiniband0", {"device/vendor": "0x1d0f\n"})])
        assert envs.resolve_nixl_backend(None, root) == "LIBFABRIC"

    def test_efa_device_names_select_libfabric_without_vendor(self, tmp_path):
        for name in ("rdmap0", "efa_0"):
            root = sysfs(tmp_path, [(name, None)])
            assert envs.resolve_nixl_backend(None, root) == "LIBFABRIC", name

    def test_efa_beside_a_conventional_device_keeps_ucx(self, tmp_path):
        root = sysfs(
            tmp_path,
            [
                ("rdmap0", None),
                ("mlx5_0", {"device/vendor": "0x15b3"}),
            ],
        )
        assert envs.resolve_nixl_backend(None, root) == "UCX"

    def test_a_conventional_device_alone_keeps_ucx(self, tmp_path):
        root = sysfs(tmp_path, [("mlx5_0", {"device/vendor": "0x15b3"})])
        assert envs.resolve_nixl_backend(None, root) == "UCX"

    def test_a_missing_sysfs_root_keeps_ucx(self, tmp_path):
        assert envs.resolve_nixl_backend(None, str(tmp_path / "absent")) == "UCX"

    def test_an_empty_sysfs_root_keeps_ucx(self, tmp_path):
        root = sysfs(tmp_path, [])
        assert envs.resolve_nixl_backend(None, root) == "UCX"

    def test_an_unreadable_vendor_entry_is_not_efa(self, tmp_path):
        root = sysfs(tmp_path, [("mlx5_0", None)])
        assert envs.resolve_nixl_backend(None, root) == "UCX"

    def test_envs_delegation_resolves_the_plugin(self):
        view = envs.get_envs({"GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND": "libfabric"})
        assert view.GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND == "LIBFABRIC"
        assert envs.ENVS.GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND_RAW is None
        # The delegated auto-detect row reads the real sysfs, so it pins only
        # the plugin FALLBACK the module documents: a host with no answerable
        # fabric question (no /sys/class/infiniband) keeps UCX.
        assert envs.resolve_nixl_backend(None, "/nonexistent-infiniband") == "UCX"


class RecordingAPI:
    """A NIXL wrapper that records the settings each agent was created
    with, and nothing else."""

    def __init__(self):
        self.agent_settings = []

    def load(self):
        return self

    def create_agent(self, name, settings):
        self.agent_settings.append(settings)
        return SimpleNamespace(name=name, settings=settings)

    def register_memory(self, agent, blocks):
        return SimpleNamespace(handles={})

    def agent_metadata(self, agent):
        return b"metadata"


@pytest.fixture()
def fake_nixl_runtime(monkeypatch):
    api = RecordingAPI()
    monkeypatch.setattr(
        "gpustack_model_transfer.backends.nixl.runtime.load_runtime",
        lambda: api,
    )
    return api


@pytest.fixture()
def fake_torch(monkeypatch):
    """A torch whose pinned allocations are bytes on the heap: enough for
    the target's window to allocate, align and report a pointer."""
    import ctypes

    class FakeTensor:
        def __init__(self, size):
            self._buffer = ctypes.create_string_buffer(size)
            self._size = size

        def data_ptr(self):
            return ctypes.addressof(self._buffer)

        def is_pinned(self):
            return True

    def empty(size, dtype=None, pin_memory=False):
        assert dtype == "uint8"
        assert pin_memory is True
        return FakeTensor(size)

    torch = SimpleNamespace(uint8="uint8", empty=empty)
    monkeypatch.setitem(sys.modules, "torch", torch)
    return torch


class TestNixlSourceBackendStart:
    def test_defaults_to_ucx(self, fake_nixl_runtime, monkeypatch):
        monkeypatch.setenv("GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND", "UCX")
        backend = NixlSourceBackend(fake_nixl_runtime)
        agent = backend.start()
        assert fake_nixl_runtime.agent_settings == [
            {"enable_listen_thread": False, "backends": ["UCX"]}
        ]
        assert agent.settings == {"enable_listen_thread": False, "backends": ["UCX"]}

    def test_libfabric_follows_the_environment(self, fake_nixl_runtime, monkeypatch):
        monkeypatch.setenv("GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND", "LIBFABRIC")
        backend = NixlSourceBackend(fake_nixl_runtime)
        backend.start()
        assert fake_nixl_runtime.agent_settings == [
            {"enable_listen_thread": False, "backends": ["LIBFABRIC"]}
        ]

    def test_an_invalid_setting_refuses_the_start(self, fake_nixl_runtime, monkeypatch):
        monkeypatch.setenv("GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND", "tcp")
        backend = NixlSourceBackend(fake_nixl_runtime)
        with pytest.raises(SourceUnavailableError):
            backend.start()
        assert fake_nixl_runtime.agent_settings == []


class TestTargetNativeNixlStart:
    def start_nixl_target(self, fake_nixl_runtime, fake_torch, monkeypatch, env):
        for key, value in env.items():
            monkeypatch.setenv(key, value)
        target = TargetNative("nixl", "127.0.0.1", 8)
        target.start()
        return target

    def test_libfabric_skips_the_ucx_device_synthesis(
        self, fake_nixl_runtime, fake_torch, monkeypatch
    ):
        target = self.start_nixl_target(
            fake_nixl_runtime,
            fake_torch,
            monkeypatch,
            {
                "GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND": "LIBFABRIC",
                "GPUSTACK_RDMA_DEVICES": "rdmap0",
            },
        )
        assert fake_nixl_runtime.agent_settings == [
            {"enable_listen_thread": False, "backends": ["LIBFABRIC"]}
        ]
        import os

        assert not os.environ.get("UCX_NET_DEVICES", "").strip(), (
            "a LIBFABRIC agent carries no synthesized UCX device list"
        )
        assert base64.b64decode(target.metadata) == b"metadata"

    def test_ucx_synthesizes_the_device_list(
        self, fake_nixl_runtime, fake_torch, monkeypatch
    ):
        import os

        self.start_nixl_target(
            fake_nixl_runtime,
            fake_torch,
            monkeypatch,
            {
                "GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND": "UCX",
                "GPUSTACK_RDMA_DEVICES": "mlx5_0",
            },
        )
        assert fake_nixl_runtime.agent_settings == [
            {"enable_listen_thread": False, "backends": ["UCX"]}
        ]
        assert os.environ.get("UCX_NET_DEVICES") == "mlx5_0:1"

    def test_ucx_keeps_a_server_declared_device_list(
        self, fake_nixl_runtime, fake_torch, monkeypatch
    ):
        import os

        self.start_nixl_target(
            fake_nixl_runtime,
            fake_torch,
            monkeypatch,
            {
                "GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND": "UCX",
                "GPUSTACK_RDMA_DEVICES": "mlx5_0",
                "UCX_NET_DEVICES": "mlx5_1:1",
            },
        )
        assert os.environ.get("UCX_NET_DEVICES") == "mlx5_1:1", (
            "a UCX_NET_DEVICES the server already carries is never overwritten"
        )

    def test_start_is_thread_bound_and_starts_one_agent(
        self, fake_nixl_runtime, fake_torch, monkeypatch
    ):
        monkeypatch.setenv("GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND", "UCX")
        target = TargetNative("nixl", "127.0.0.1", 8)
        target.start()
        assert target.thread_id == threading.get_ident()
        assert target.nixl_backend == "UCX"
        assert len(fake_nixl_runtime.agent_settings) == 1

    def test_source_and_target_log_resolved_nixl_backend(
        self, fake_nixl_runtime, fake_torch, monkeypatch, caplog
    ):
        import logging

        monkeypatch.setenv("GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND", "LIBFABRIC")
        with caplog.at_level(logging.INFO):
            source = NixlSourceBackend(api=fake_nixl_runtime)
            source.start()
            target = self.start_nixl_target(
                fake_nixl_runtime,
                fake_torch,
                monkeypatch,
                {"GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND": "LIBFABRIC"},
            )
        assert source.nixl_backend == "LIBFABRIC"
        assert target.nixl_backend == "LIBFABRIC"
        assert (
            "Starting NIXL source agent with transport plugin LIBFABRIC" in caplog.text
        )
        assert (
            "Starting NIXL target agent with transport plugin LIBFABRIC" in caplog.text
        )

    def test_connect_peer_failure_logs_transport_plugin_and_pinning_hint(
        self, fake_nixl_runtime, monkeypatch, caplog
    ):
        import logging

        def fail_connect(agent, metadata):
            raise RuntimeError("peer unreachable")

        fake_nixl_runtime.connect_peer = fail_connect
        monkeypatch.setenv("GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND", "LIBFABRIC")
        source = NixlSourceBackend(api=fake_nixl_runtime)
        source.start()
        registration = source.register(
            {"weights.bin": SimpleNamespace(data_ptr=lambda: 1024, byte_size=16)}
        )
        with (
            caplog.at_level(logging.WARNING),
            pytest.raises(RuntimeError, match="peer unreachable"),
        ):
            source.post_write(
                registration,
                "weights.bin",
                0,
                8,
                {
                    "backend": "nixl",
                    "address": 4096,
                    "length": 16,
                    "metadata": base64.b64encode(b"peer").decode("ascii"),
                    "endpoint": "",
                },
            )
        assert (
            "NIXL source connect_peer failed (source transport plugin LIBFABRIC; "
            "verify target uses the same GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND)"
            in caplog.text
        )
