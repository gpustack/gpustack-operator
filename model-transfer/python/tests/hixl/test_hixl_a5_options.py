"""Tests for Ascend A5 HIXL initialization options and physical NPU resolution."""

from __future__ import annotations

import ctypes
import json
import threading
from types import SimpleNamespace

import pytest

import gpustack_model_transfer.client_native as client_native
from gpustack_model_transfer import envs
from gpustack_model_transfer.backends.hixl import runtime as hixl_runtime
from gpustack_model_transfer.core.errors import SourceUnavailableError
from gpustack_model_transfer.source.backend import HixlSourceBackend


@pytest.fixture(autouse=True)
def clean_ascend_comm_env(monkeypatch):
    monkeypatch.delenv("ASCEND_LOCAL_COMM_RES", raising=False)
    monkeypatch.delenv("ASCEND_GLOBAL_RESOURCE_CONFIG", raising=False)


class FakeAclLib:
    """Fake ACL shared library mapping thread-local logical NPU IDs to physical IDs."""

    def __init__(
        self,
        phy_map: dict[int, int] | None = None,
        *,
        default_dev: int = 0,
        get_dev_rc: int = 0,
        get_phy_rc: int = 0,
        include_get_dev: bool = True,
        include_get_phy: bool = True,
    ):
        self.phy_map = {0: 0} if phy_map is None else dict(phy_map)
        self.default_dev = default_dev
        self.get_dev_rc = get_dev_rc
        self.get_phy_rc = get_phy_rc
        self.local = threading.local()
        self.calls: list[tuple[str, int]] = []
        if include_get_dev:
            self.aclrtGetDevice = self._get_device
        if include_get_phy:
            self.aclrtGetPhyDevIdByUserDevId = self._get_phy_id

    def set_thread_device(self, dev_id: int) -> None:
        self.local.dev_id = dev_id

    def _get_device(self, out_ptr) -> int:
        dev = getattr(self.local, "dev_id", self.default_dev)
        self.calls.append(("aclrtGetDevice", dev))
        if self.get_dev_rc == 0:
            ctypes.cast(out_ptr, ctypes.POINTER(ctypes.c_int32))[0] = dev
        return self.get_dev_rc

    def _get_phy_id(self, user_dev_id, out_ptr) -> int:
        logical = int(getattr(user_dev_id, "value", user_dev_id))
        self.calls.append(("aclrtGetPhyDevIdByUserDevId", logical))
        if self.get_phy_rc != 0:
            return self.get_phy_rc
        if logical not in self.phy_map:
            return 107001
        ctypes.cast(out_ptr, ctypes.POINTER(ctypes.c_int32))[0] = self.phy_map[logical]
        return 0


class TestEnvsRegistration:
    def test_ascend_comm_vars_registered_in_envs(self, monkeypatch):
        assert envs.ASCEND_LOCAL_COMM_RES == ""
        assert envs.ASCEND_GLOBAL_RESOURCE_CONFIG == ""
        assert "ASCEND_LOCAL_COMM_RES" in dir(envs)
        assert "ASCEND_GLOBAL_RESOURCE_CONFIG" in dir(envs)

        view = envs.get_envs(
            {
                "ASCEND_LOCAL_COMM_RES": '{"version":"1.3"}',
                "ASCEND_GLOBAL_RESOURCE_CONFIG": '{"comm_resource_config.protocol_desc":["ub_ctp:host"]}',
            }
        )
        assert view.ASCEND_LOCAL_COMM_RES == '{"version":"1.3"}'
        assert (
            view.ASCEND_GLOBAL_RESOURCE_CONFIG
            == '{"comm_resource_config.protocol_desc":["ub_ctp:host"]}'
        )

        monkeypatch.setenv("ASCEND_LOCAL_COMM_RES", '{"version":"1.3"}')
        assert envs.ASCEND_LOCAL_COMM_RES == '{"version":"1.3"}'


class TestHixlInitOptionsNormalization:
    def test_defaults_preserve_only_buffer_pool(self):
        assert hixl_runtime.build_hixl_init_options({}) == {"BufferPool": "0:0"}
        assert hixl_runtime.build_hixl_init_options({"ASCEND_LOCAL_COMM_RES": ""}) == {
            "BufferPool": "0:0"
        }
        assert hixl_runtime.build_hixl_init_options(
            {"ASCEND_GLOBAL_RESOURCE_CONFIG": ""}
        ) == {"BufferPool": "0:0"}

    def test_forwards_local_comm_res_when_non_empty(self):
        opts = hixl_runtime.build_hixl_init_options(
            {"ASCEND_LOCAL_COMM_RES": '{"version":"1.3"}'}
        )
        assert opts == {
            "BufferPool": "0:0",
            "LocalCommRes": '{"version":"1.3"}',
        }

    def test_canonical_flat_dotted_json_preserved(self):
        raw = json.dumps(
            {
                "comm_resource_config.protocol_desc": ["roce:host"],
                "local_comm_res_path": "/nonexistent/ub_endpoint_npu_0.json",
                "custom_key": {"nested": True},
            }
        )
        opts = hixl_runtime.build_hixl_init_options(
            {"ASCEND_GLOBAL_RESOURCE_CONFIG": raw}
        )
        assert opts["BufferPool"] == "0:0"
        parsed = json.loads(opts["GlobalResourceConfig"])
        assert parsed == {
            "comm_resource_config.protocol_desc": ["roce:host"],
            "local_comm_res_path": "/nonexistent/ub_endpoint_npu_0.json",
            "custom_key": {"nested": True},
        }

    def test_nested_protocol_desc_normalized_to_flat_dotted_key(self):
        raw = json.dumps(
            {
                "comm_resource_config": {"protocol_desc": ["roce:host"]},
                "local_comm_res_path": "/nonexistent/ub_endpoint_npu_2.json",
                "extra": 7,
            }
        )
        opts = hixl_runtime.build_hixl_init_options(
            {"ASCEND_GLOBAL_RESOURCE_CONFIG": raw}
        )
        parsed = json.loads(opts["GlobalResourceConfig"])
        assert parsed == {
            "comm_resource_config.protocol_desc": ["roce:host"],
            "local_comm_res_path": "/nonexistent/ub_endpoint_npu_2.json",
            "extra": 7,
        }
        assert "comm_resource_config" not in parsed

    def test_nested_comm_resource_config_keeps_other_keys_when_present(self):
        raw = json.dumps(
            {
                "comm_resource_config": {
                    "protocol_desc": ["ub_ctp:host"],
                    "bandwidth_cap": 100,
                },
                "top_flag": "keep",
            }
        )
        opts = hixl_runtime.build_hixl_init_options(
            {"ASCEND_GLOBAL_RESOURCE_CONFIG": raw}
        )
        parsed = json.loads(opts["GlobalResourceConfig"])
        assert parsed == {
            "comm_resource_config.protocol_desc": ["ub_ctp:host"],
            "comm_resource_config": {"bandwidth_cap": 100},
            "top_flag": "keep",
        }

    def test_flat_dotted_key_takes_precedence_over_nested_key(self):
        env_roce_lost = {
            "ASCEND_GLOBAL_RESOURCE_CONFIG": json.dumps(
                {
                    "comm_resource_config.protocol_desc": ["uboe:device"],
                    "comm_resource_config": {"protocol_desc": ["roce:host"]},
                    "other": "value",
                }
            )
        }
        opts = hixl_runtime.build_hixl_init_options(env_roce_lost)
        parsed = json.loads(opts["GlobalResourceConfig"])
        assert parsed == {
            "comm_resource_config.protocol_desc": ["uboe:device"],
            "other": "value",
        }
        assert hixl_runtime.uses_roce_host_protocol(env_roce_lost) is False

        env_roce_won = {
            "ASCEND_GLOBAL_RESOURCE_CONFIG": json.dumps(
                {
                    "comm_resource_config.protocol_desc": ["roce:host"],
                    "comm_resource_config": {
                        "protocol_desc": ["uboe:device"],
                        "keep_me": 1,
                    },
                }
            )
        }
        opts_won = hixl_runtime.build_hixl_init_options(env_roce_won)
        parsed_won = json.loads(opts_won["GlobalResourceConfig"])
        assert parsed_won == {
            "comm_resource_config.protocol_desc": ["roce:host"],
            "comm_resource_config": {"keep_me": 1},
        }
        assert hixl_runtime.uses_roce_host_protocol(env_roce_won) is True

    @pytest.mark.parametrize(
        "raw",
        [
            "not-valid-json{",
            "  raw-string  ",
            "[1, 2, 3]",
            '"roce:host"',
            "42",
            "true",
        ],
    )
    def test_non_object_global_config_passed_through_verbatim(self, raw):
        env = {"ASCEND_GLOBAL_RESOURCE_CONFIG": raw}
        opts = hixl_runtime.build_hixl_init_options(env)
        assert opts == {"BufferPool": "0:0", "GlobalResourceConfig": raw}
        assert hixl_runtime.uses_roce_host_protocol(env) is False


class TestTopologyProtocols:
    @pytest.mark.parametrize(
        "protocol_desc,expected_roce",
        [
            (["ub_ctp:device"], False),
            ("ub_ctp:device", False),
            (["ub_ctp:host"], False),
            ("ub_ctp:host", False),
            (["ub_rtp:device"], False),
            ("ub_rtp:device", False),
            (["uboe:device"], False),
            ("uboe:device", False),
            (["roce:device"], False),
            ("ROCE:HOST", False),
            (" roce:host ", False),
            ([], False),
            (["roce:host"], True),
            ("roce:host", True),
            (["ub_ctp:host", "roce:host"], True),
        ],
    )
    def test_a5_protocol_descriptors_and_roce_host_detection(
        self, protocol_desc, expected_roce
    ):
        flat_env = {
            "ASCEND_GLOBAL_RESOURCE_CONFIG": json.dumps(
                {"comm_resource_config.protocol_desc": protocol_desc}
            )
        }
        nested_env = {
            "ASCEND_GLOBAL_RESOURCE_CONFIG": json.dumps(
                {"comm_resource_config": {"protocol_desc": protocol_desc}}
            )
        }
        assert hixl_runtime.uses_roce_host_protocol(flat_env) is expected_roce
        assert hixl_runtime.uses_roce_host_protocol(nested_env) is expected_roce

        flat_opts = hixl_runtime.build_hixl_init_options(flat_env)
        nested_opts = hixl_runtime.build_hixl_init_options(nested_env)
        assert json.loads(flat_opts["GlobalResourceConfig"]) == {
            "comm_resource_config.protocol_desc": protocol_desc
        }
        assert json.loads(nested_opts["GlobalResourceConfig"]) == {
            "comm_resource_config.protocol_desc": protocol_desc
        }


class TestPhysicalNpuDirectoryResolution:
    def test_non_empty_local_comm_res_skips_directory_acl_lookup(self, tmp_path):
        hixlep = tmp_path / "hixlep"
        hixlep.mkdir()
        env = {
            "ASCEND_LOCAL_COMM_RES": '{"endpoint":"already-loaded"}',
            "ASCEND_GLOBAL_RESOURCE_CONFIG": json.dumps(
                {
                    "comm_resource_config.protocol_desc": ["roce:host"],
                    "local_comm_res_path": str(hixlep),
                }
            ),
        }

        def fail_resolver():
            raise AssertionError("ACL lookup must not run when LocalCommRes is set")

        opts = hixl_runtime.build_hixl_init_options(env, phy_id_resolver=fail_resolver)
        assert opts["LocalCommRes"] == '{"endpoint":"already-loaded"}'
        assert json.loads(opts["GlobalResourceConfig"]) == {
            "comm_resource_config.protocol_desc": ["roce:host"],
            "local_comm_res_path": str(hixlep),
        }

    def test_regular_file_local_comm_res_path_passed_verbatim_without_acl(
        self, tmp_path
    ):
        ep_file = tmp_path / "ub_endpoint_npu_6.json"
        ep_file.write_text('{"id":6}')
        env = {
            "ASCEND_LOCAL_COMM_RES": "",
            "ASCEND_GLOBAL_RESOURCE_CONFIG": json.dumps(
                {
                    "comm_resource_config.protocol_desc": ["roce:host"],
                    "local_comm_res_path": str(ep_file),
                }
            ),
        }

        def fail_resolver():
            raise AssertionError("ACL lookup must not run for regular file path")

        opts = hixl_runtime.build_hixl_init_options(env, phy_id_resolver=fail_resolver)
        assert "LocalCommRes" not in opts
        assert json.loads(opts["GlobalResourceConfig"])["local_comm_res_path"] == str(
            ep_file
        )

    def test_visible_zero_maps_to_physical_six(self, tmp_path):
        hixlep = tmp_path / "hixlep"
        hixlep.mkdir()
        # Even if npu_0 exists, visible 0 -> physical 6 must select npu_6.
        (hixlep / "ub_endpoint_npu_0.json").write_text('{"phy":0}')
        (hixlep / "ub_endpoint_npu_6.json").write_text('{"phy":6}')

        acl = FakeAclLib({0: 6}, default_dev=0)
        env = {
            "ASCEND_LOCAL_COMM_RES": "",
            "ASCEND_GLOBAL_RESOURCE_CONFIG": json.dumps(
                {
                    "comm_resource_config": {"protocol_desc": ["roce:host"]},
                    "local_comm_res_path": str(hixlep),
                }
            ),
        }
        opts = hixl_runtime.build_hixl_init_options(
            env,
            phy_id_resolver=lambda: hixl_runtime._resolve_acl_physical_device_id(
                loader=lambda name: acl
            ),
        )
        parsed = json.loads(opts["GlobalResourceConfig"])
        assert parsed == {
            "comm_resource_config.protocol_desc": ["roce:host"],
            "local_comm_res_path": str(hixlep / "ub_endpoint_npu_6.json"),
        }
        assert acl.calls == [
            ("aclrtGetDevice", 0),
            ("aclrtGetPhyDevIdByUserDevId", 0),
        ]

    def test_multi_tp_ranks_resolve_distinct_physical_npus_per_thread(self, tmp_path):
        hixlep = tmp_path / "hixlep"
        hixlep.mkdir()
        rank_to_phy = {0: 4, 1: 5, 2: 6, 3: 7}
        for phy in rank_to_phy.values():
            (hixlep / f"ub_endpoint_npu_{phy}.json").write_text(f'{{"phy":{phy}}}')

        acl = FakeAclLib(rank_to_phy)
        env = {
            "ASCEND_GLOBAL_RESOURCE_CONFIG": json.dumps(
                {
                    "comm_resource_config.protocol_desc": ["roce:host"],
                    "local_comm_res_path": str(hixlep),
                }
            )
        }
        barrier = threading.Barrier(len(rank_to_phy))
        results: dict[int, str] = {}
        errors: list[BaseException] = []

        def worker(rank: int):
            try:
                acl.set_thread_device(rank)
                barrier.wait(timeout=2)
                opts = hixl_runtime.build_hixl_init_options(
                    env,
                    phy_id_resolver=lambda: (
                        hixl_runtime._resolve_acl_physical_device_id(
                            loader=lambda _: acl
                        )
                    ),
                )
                results[rank] = json.loads(opts["GlobalResourceConfig"])[
                    "local_comm_res_path"
                ]
            except BaseException as exc:
                errors.append(exc)

        threads = [
            threading.Thread(target=worker, args=(rank,)) for rank in rank_to_phy
        ]
        for t in threads:
            t.start()
        for t in threads:
            t.join(timeout=3)

        assert not errors
        for rank, phy in rank_to_phy.items():
            assert results[rank] == str(hixlep / f"ub_endpoint_npu_{phy}.json")

    def test_library_probe_prefers_libacl_rt_then_falls_back_to_libascendcl(self):
        loaded: list[str] = []
        acl = FakeAclLib({0: 3})

        def loader_prefers_acl_rt(name: str):
            loaded.append(name)
            if name == "libacl_rt.so":
                return acl
            raise AssertionError(
                "libascendcl.so should not be loaded when libacl_rt.so succeeds"
            )

        assert (
            hixl_runtime._resolve_acl_physical_device_id(loader=loader_prefers_acl_rt)
            == 3
        )
        assert loaded == ["libacl_rt.so"]

        fallback_loaded: list[str] = []

        def loader_fallback(name: str):
            fallback_loaded.append(name)
            if name == "libacl_rt.so":
                raise OSError("libacl_rt.so not found")
            if name == "libascendcl.so":
                return acl
            raise OSError(name)

        assert hixl_runtime._resolve_acl_physical_device_id(loader=loader_fallback) == 3
        assert fallback_loaded == ["libacl_rt.so", "libascendcl.so"]

        partial_rt = FakeAclLib({0: 5}, include_get_phy=False)
        full_ascendcl = FakeAclLib({0: 5})
        symbol_fallback_loaded: list[str] = []

        def loader_symbol_fallback(name: str):
            symbol_fallback_loaded.append(name)
            return partial_rt if name == "libacl_rt.so" else full_ascendcl

        assert (
            hixl_runtime._resolve_acl_physical_device_id(loader=loader_symbol_fallback)
            == 5
        )
        assert symbol_fallback_loaded == ["libacl_rt.so", "libascendcl.so"]

    @pytest.mark.parametrize(
        "failure",
        [
            "missing_libs",
            "missing_get_device",
            "missing_get_phy",
            "get_device_nonzero",
            "get_phy_nonzero",
            "missing_file",
            "directory_instead_of_file",
        ],
    )
    def test_directory_resolution_fails_closed_never_guesses_npu_zero(
        self, tmp_path, failure
    ):
        hixlep = tmp_path / "hixlep"
        hixlep.mkdir()
        # Keep ub_endpoint_npu_0.json present to prove failure never falls back to 0.
        (hixlep / "ub_endpoint_npu_0.json").write_text('{"phy":0}')
        if failure == "directory_instead_of_file":
            (hixlep / "ub_endpoint_npu_6.json").mkdir()

        def make_loader():
            if failure == "missing_libs":

                def raise_oserror(name):
                    raise OSError(f"{name} missing")

                return raise_oserror
            if failure == "missing_get_device":
                lib = FakeAclLib({0: 6}, include_get_dev=False)
            elif failure == "missing_get_phy":
                lib = FakeAclLib({0: 6}, include_get_phy=False)
            elif failure == "get_device_nonzero":
                lib = FakeAclLib({0: 6}, get_dev_rc=507001)
            elif failure == "get_phy_nonzero":
                lib = FakeAclLib({0: 6}, get_phy_rc=507002)
            else:
                lib = FakeAclLib({0: 6})
            return lambda _: lib

        env = {
            "ASCEND_GLOBAL_RESOURCE_CONFIG": json.dumps(
                {
                    "comm_resource_config.protocol_desc": ["roce:host"],
                    "local_comm_res_path": str(hixlep),
                }
            )
        }
        loader = make_loader()
        with pytest.raises(SourceUnavailableError) as exc_info:
            hixl_runtime.build_hixl_init_options(
                env,
                phy_id_resolver=lambda: hixl_runtime._resolve_acl_physical_device_id(
                    loader=loader
                ),
            )
        message = str(exc_info.value)
        assert "ASCEND_LOCAL_COMM_RES" in message
        assert "local_comm_res_path" in message


class TestSourceAndTargetEndpointWiring:
    def test_hixl_source_backend_forwards_built_options(self, monkeypatch, tmp_path):
        ep_file = tmp_path / "ub_endpoint_npu_2.json"
        ep_file.write_text('{"phy":2}')
        monkeypatch.setenv("ASCEND_LOCAL_COMM_RES", '{"version":"1.3"}')
        monkeypatch.setenv(
            "ASCEND_GLOBAL_RESOURCE_CONFIG",
            json.dumps(
                {
                    "comm_resource_config": {"protocol_desc": ["ub_ctp:host"]},
                    "local_comm_res_path": str(ep_file),
                }
            ),
        )
        seen = []

        class Engine:
            def initialize(self, address, options):
                seen.append((address, dict(options)))
                return 0

        backend = HixlSourceBackend(
            engine_factory=Engine, engine_address="127.0.0.1:29500"
        )
        backend.start()
        assert seen == [
            (
                "127.0.0.1:29500",
                {
                    "BufferPool": "0:0",
                    "LocalCommRes": '{"version":"1.3"}',
                    "GlobalResourceConfig": json.dumps(
                        {
                            "comm_resource_config.protocol_desc": ["ub_ctp:host"],
                            "local_comm_res_path": str(ep_file),
                        },
                        separators=(",", ":"),
                    ),
                },
            )
        ]

    def test_target_native_forwards_built_options_and_keeps_real_api_seam(
        self, monkeypatch, tmp_path
    ):
        ep_file = tmp_path / "ub_endpoint_npu_4.json"
        ep_file.write_text('{"phy":4}')
        monkeypatch.setenv(
            "ASCEND_GLOBAL_RESOURCE_CONFIG",
            json.dumps(
                {
                    "comm_resource_config": {"protocol_desc": ["uboe:device"]},
                    "local_comm_res_path": str(ep_file),
                }
            ),
        )
        seen_init = []

        class FakeOwner:
            def ensure_current(self):
                pass

            def allocate(self, name, size):
                return SimpleNamespace(
                    data_ptr=lambda: 2 << 20, registration_bytes=size
                )

            def release(self, block):
                pass

        class Client:
            def initialize(self, endpoint, options):
                seen_init.append((endpoint, dict(options)))
                return 0

            def register_mem(self, desc, mem_type):
                return 0, 101

            def finalize(self):
                return 0

        fake_hixl = SimpleNamespace(
            Hixl=Client,
            SUCCESS=0,
            MemDesc=lambda a, n: (a, n),
            MemType=SimpleNamespace(MEM_HOST="HOST", MEM_DEVICE="DEVICE"),
            TransferOp=SimpleNamespace(READ="READ", WRITE="WRITE"),
            TransferStatus=SimpleNamespace(
                COMPLETED="COMPLETED", FAILED="FAILED", TIMEOUT="TIMEOUT"
            ),
        )
        monkeypatch.setattr(client_native, "BorrowedAclOwner", FakeOwner)
        monkeypatch.setattr(
            hixl_runtime,
            "load_runtime",
            lambda: SimpleNamespace(
                _require=lambda: fake_hixl,
                deregister=lambda agent, reg: reg.handles.clear(),
            ),
        )

        target = client_native.TargetNative("hixl", "127.0.0.1", 4 << 20)
        target.start()
        try:
            assert len(seen_init) == 1
            _, opts = seen_init[0]
            assert opts == {
                "BufferPool": "0:0",
                "GlobalResourceConfig": json.dumps(
                    {
                        "comm_resource_config.protocol_desc": ["uboe:device"],
                        "local_comm_res_path": str(ep_file),
                    },
                    separators=(",", ":"),
                ),
            }
        finally:
            target.close()

        # RealHixlAPI.create_agent keeps its explicit caller-options seam untouched.
        explicit_calls = []

        class ExplicitClient(Client):
            def initialize(self, endpoint, options):
                explicit_calls.append((endpoint, dict(options)))
                return 0

            def deregister_mem(self, handle):
                return 0

            def connect(self, endpoint):
                return 0

            def disconnect(self, endpoint):
                return 0

            def transfer_async(self, *args):
                return 0, 1

            def get_transfer_status(self, req):
                return 0, "COMPLETED"

        real_api = hixl_runtime.RealHixlAPI()
        real_api._hixl = SimpleNamespace(
            Hixl=ExplicitClient,
            SUCCESS=0,
            MemDesc=fake_hixl.MemDesc,
            MemType=fake_hixl.MemType,
            TransferOp=fake_hixl.TransferOp,
            TransferStatus=fake_hixl.TransferStatus,
        )
        real_api.create_agent("explicit", {"local_engine": "127.0.0.1:9999"})
        assert explicit_calls == [("127.0.0.1:9999", {})]


class TestSourceWorkerPreflightVsQuarantine:
    @staticmethod
    def _assignment(generation: str = "gen-a5-preflight"):
        import hashlib
        from pathlib import Path

        from gpustack_model_transfer.source.workorder import parse_assignment

        fixture = Path(__file__).resolve().parents[3] / "contracts/fixtures/tiny_dense"
        raw_manifest = (fixture / "manifest.json").read_bytes()
        manifest_obj = json.loads(raw_manifest)
        return parse_assignment(
            {
                "generation": generation,
                "workerSecret": "super-secret-worker-token",
                "digest": "sha256:" + "a" * 64,
                "namespace": "test",
                "artifactUID": manifest_obj["artifact_uid"],
                "sourceGeneration": manifest_obj["source_generation"],
                "manifestSHA256": hashlib.sha256(raw_manifest).hexdigest(),
                "manifestJSON": raw_manifest.decode(),
                "treePath": str(fixture),
                "totalBytes": sum(f["size"] for f in manifest_obj["files"]),
                "registrations": len(manifest_obj["files"]),
                "residentBudgetBytes": 4 << 20,
            },
            backend="hixl",
        )

    @staticmethod
    def _acl_runtime_lib(phy_id: int = 6):
        class _Fn:
            def __init__(self, fn):
                self.fn = fn

            def __call__(self, *args):
                return self.fn(*args)

        class FullFakeAcl(FakeAclLib):
            def __init__(self):
                super().__init__({0: phy_id}, default_dev=0)
                self.disposed = False
                self.aclInit = _Fn(lambda _cfg: 0)
                self.aclrtSetDevice = _Fn(lambda _dev: 0)
                self.aclrtCreateContext = _Fn(self._create_ctx)
                self.aclrtSetCurrentContext = _Fn(lambda _ctx: 0)
                self.aclrtDestroyContext = _Fn(lambda _ctx: 0)
                self.aclrtResetDevice = _Fn(lambda _dev: 0)
                self.aclFinalize = _Fn(self._finalize)
                self.aclrtMallocHost = _Fn(self._malloc)
                self.aclrtFreeHost = _Fn(lambda _ptr: 0)

            def _create_ctx(self, out_ptr, _dev):
                ctypes.cast(out_ptr, ctypes.POINTER(ctypes.c_void_p))[0] = (
                    ctypes.c_void_p(0xCAFE)
                )
                return 0

            def _finalize(self):
                self.disposed = True
                return 0

            def _malloc(self, out_ptr, size):
                raise AssertionError(
                    "host allocation must not run on preflight or init failure"
                )

        return FullFakeAcl()

    def test_missing_endpoint_file_stops_cleanly_without_quarantine_while_init_failure_quarantines(
        self, monkeypatch, tmp_path, caplog
    ):
        from gpustack_model_transfer.source import memory as source_memory
        from gpustack_model_transfer.source.backend import (
            release_source_retained,
            retained_source_states,
        )
        from gpustack_model_transfer.source.memory import AclHostOwner
        from gpustack_model_transfer.source.native_worker import NativeSourceWorker
        from gpustack_model_transfer.source.transfer_service import OperationRegistry

        hixlep = tmp_path / "hixlep"
        hixlep.mkdir()
        monkeypatch.setenv(
            "ASCEND_GLOBAL_RESOURCE_CONFIG",
            json.dumps(
                {
                    "comm_resource_config.protocol_desc": ["roce:host"],
                    "local_comm_res_path": str(hixlep),
                }
            ),
        )
        acl = self._acl_runtime_lib(phy_id=6)
        native_starts: list[str] = []
        quarantines: list[str] = []

        def make_control(gen: str):
            return SimpleNamespace(
                generation=gen,
                worker_id="pod-uid",
                worker_instance_id="inst-1",
                lease_id="lease-1",
                register=lambda: "lease-1",
                native_start=lambda: native_starts.append(gen),
                heartbeat=lambda **kw: {
                    "action": "none",
                    "activeSessions": 0,
                    "unknownSessions": 0,
                },
                filled=lambda count, descs: None,
                drain_ack=lambda native_released: None,
                quarantine=lambda reason: quarantines.append(reason),
                close=lambda: None,
            )

        before_retained = set(retained_source_states())
        assignment_missing = self._assignment("gen-missing-ep")
        port_released = []
        worker_missing = NativeSourceWorker(
            make_control("gen-missing-ep"),
            assignment_missing,
            backend=lambda: self._backend_with_finalizer(port_released),
            allocator=lambda: AclHostOwner(loader=lambda _: acl),
            worker_instance_id="inst-1",
            poll_interval=0.01,
        )
        with caplog.at_level("WARNING"):
            worker_missing.start()
        worker_missing.close()

        assert worker_missing.state == "stopped"
        assert worker_missing._native_attempted is False
        assert native_starts == []
        assert quarantines == []
        assert set(retained_source_states()) == before_retained
        assert source_memory._ACL_RUNTIME is None
        assert acl.disposed is True
        assert port_released == ["released"]
        assert "ub_endpoint_npu_6.json" in caplog.text
        assert "ASCEND_LOCAL_COMM_RES" in caplog.text
        assert "super-secret-worker-token" not in caplog.text

        # Unavailable ACL physical ID symbol also stops cleanly in preflight without quarantine.
        caplog.clear()
        acl_no_sym = self._acl_runtime_lib(phy_id=6)
        del acl_no_sym.aclrtGetPhyDevIdByUserDevId
        worker_no_sym = NativeSourceWorker(
            make_control("gen-no-sym"),
            self._assignment("gen-no-sym"),
            backend=lambda: self._backend_with_finalizer(port_released),
            allocator=lambda: AclHostOwner(loader=lambda _: acl_no_sym),
            worker_instance_id="inst-1",
            poll_interval=0.01,
        )
        with caplog.at_level("WARNING"):
            worker_no_sym.start()
        worker_no_sym.close()
        assert worker_no_sym.state == "stopped"
        assert worker_no_sym._native_attempted is False
        assert native_starts == []
        assert quarantines == []
        assert source_memory._ACL_RUNTIME is None
        assert acl_no_sym.disposed is True
        assert "aclrtGetPhyDevIdByUserDevId" in caplog.text
        assert "super-secret-worker-token" not in caplog.text

        supervisor_stub = SimpleNamespace(
            workers={"gen-missing-ep": worker_missing},
            worker_id="pod-uid",
            worker_instance_id="inst-1",
            _starting=0,
            _admission_closed=threading.Event(),
            _lock=threading.Lock(),
        )
        registry = OperationRegistry(supervisor_stub)
        registry.freeze()
        assert registry.runtime() == {
            "workerID": "pod-uid",
            "workerInstanceID": "inst-1",
            "generations": [],
            "nativeOperations": 0,
            "unknownOperations": 0,
            "retainedGenerations": [],
        }

        # Now provide the endpoint file so preflight passes, and fail engine.initialize():
        # native_start runs and the generation MUST still be quarantined.
        (hixlep / "ub_endpoint_npu_6.json").write_text('{"phy":6}')
        acl2 = self._acl_runtime_lib(phy_id=6)
        assignment_init_fail = self._assignment("gen-init-fail")
        failing_backend = HixlSourceBackend(
            engine_factory=lambda: SimpleNamespace(
                initialize=lambda addr, opts: 1,
            ),
            engine_address="127.0.0.1:29700",
        )
        worker_init_fail = NativeSourceWorker(
            make_control("gen-init-fail"),
            assignment_init_fail,
            backend=lambda: failing_backend,
            allocator=lambda: AclHostOwner(loader=lambda _: acl2),
            worker_instance_id="inst-1",
            poll_interval=0.01,
        )
        try:
            worker_init_fail.start()
            assert native_starts == ["gen-init-fail"]
            assert worker_init_fail._native_attempted is True
            assert worker_init_fail.state == "quarantined"
            assert quarantines == ["SourceUnavailableError"]
            supervisor_stub.workers["gen-init-fail"] = worker_init_fail
            snap = registry.runtime()
            assert snap["retainedGenerations"] == ["gen-init-fail"]
        finally:
            worker_init_fail._stop_event.set()
            if worker_init_fail._control_thread is not None:
                worker_init_fail._control_thread.join(timeout=2)
            if worker_init_fail.allocator is not None:
                worker_init_fail.allocator.dispose()
            for item in retained_source_states():
                if item not in before_retained:
                    release_source_retained(item, external_fence="test-cleanup")

    @staticmethod
    def _backend_with_finalizer(released: list[str]) -> HixlSourceBackend:
        backend = HixlSourceBackend(
            engine_factory=lambda: SimpleNamespace(
                initialize=lambda addr, opts: (_ for _ in ()).throw(
                    AssertionError("engine must not initialize when preflight fails")
                )
            ),
            engine_address="127.0.0.1:29701",
        )
        backend.on_finalized = lambda: released.append("released")
        return backend
