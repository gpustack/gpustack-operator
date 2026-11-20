"""Tests for zero-intrusion vLLM post-import hooks, multi-rank EngineCore utility fan-out, and vllm-omni degradation."""

from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path
from types import SimpleNamespace
from typing import Any

import pytest
from asgi_harness import FakeFastAPIApp, run_sync, send_asgi_request

REPO_PYTHON_ROOT = Path(__file__).resolve().parents[1]


class FakeVLLMScheduler:
    def __init__(
        self,
        *,
        max_num_seqs: int = 64,
        max_num_batched_tokens: int = 4096,
        running_count: int = 0,
        enable_chunked_prefill: bool = True,
        policy: str = "fcfs",
        enable_prefix_caching: bool = True,
        has_kv_connector: bool = True,
        enable_kv_load_controls: bool = True,
    ) -> None:
        self.scheduler_config = SimpleNamespace(
            max_num_seqs=max_num_seqs,
            max_num_active_seqs=None,
            max_num_batched_tokens=max_num_batched_tokens,
            max_num_scheduled_tokens=None,
            long_prefill_token_threshold=0,
            enable_chunked_prefill=enable_chunked_prefill,
            policy=policy,
            cache_aware_admission_window=0,
            cache_aware_admission_threshold=0.5,
            max_num_inflight_kv_loads=32 if enable_kv_load_controls else None,
            max_async_kv_loads_per_step=8 if enable_kv_load_controls else None,
        )
        self.cache_config = SimpleNamespace(enable_prefix_caching=enable_prefix_caching)
        self.max_num_running_reqs = max_num_seqs
        self.max_num_active_reqs = max_num_seqs
        self.max_num_scheduled_tokens = max_num_batched_tokens
        self.max_model_len = 8192
        self.is_encoder_decoder = False
        self.policy = policy
        self.cache_aware_window = 0
        self.cache_aware_threshold = 0.5
        self.connector = object() if has_kv_connector else None
        self._kv_fetch_stages = (
            {} if (has_kv_connector and enable_kv_load_controls) else None
        )
        self.running = [f"req-{i}" for i in range(running_count)]

    def assert_step_invariants(self) -> None:
        # Upstream vLLM scheduler.py:1682 invariant that runs every step!
        assert len(self.running) <= self.max_num_running_reqs


class FakeEngineCore:
    def __init__(self, rank: int, dp_size: int, scheduler: FakeVLLMScheduler) -> None:
        self.vllm_config = SimpleNamespace(
            parallel_config=SimpleNamespace(
                data_parallel_rank=rank,
                data_parallel_index=rank,
                data_parallel_size=dp_size,
            ),
            model_config=SimpleNamespace(max_model_len=scheduler.max_model_len),
            scheduler_config=scheduler.scheduler_config,
        )
        self.scheduler = scheduler


class FakeDPLBAsyncMPClient:
    """Simulates vLLM DPLBAsyncMPClient managing multiple EngineCore ranks."""

    def __init__(self, cores: list[FakeEngineCore]) -> None:
        self.cores = cores
        self.core_engines = [i.to_bytes(2, "little") for i in range(len(cores))]
        self.engine_ranks_managed = list(range(len(cores)))
        self.resources = SimpleNamespace(engine_dead=False)

    async def call_utility_all_async(self, method: str, *args: Any) -> list[Any]:
        results = []
        for core in self.cores:
            fn = getattr(core, method)
            results.append(fn(*args))
        return results


class FakeAsyncLLM:
    def __init__(self, mp_client: Any) -> None:
        self.engine_core = mp_client
        self.errored = False
        self._paused = False

    async def check_health(self) -> None:
        if self.errored:

            class EngineDeadError(Exception):
                pass

            raise EngineDeadError("engine errored")

    async def is_paused(self) -> bool:
        return self._paused


def _build_hooked_vllm_app(engine_client: Any) -> FakeFastAPIApp:
    from gpustack_engine_sentinel.vllm.hooks import patch_vllm_api_server_module

    async def ok_chat(scope, receive, send):
        body = json.dumps({"choices": []}).encode("utf-8")
        await send(
            {
                "type": "http.response.start",
                "status": 200,
                "headers": [(b"content-type", b"application/json")],
            }
        )
        await send({"type": "http.response.body", "body": body})

    def raw_build_app(args: Any, supported_tasks: Any = None) -> FakeFastAPIApp:
        app = FakeFastAPIApp()
        app.state.args = args
        app.add_api_route("/v1/chat/completions", ok_chat, methods=["POST"])
        app.add_api_route("/health", ok_chat, methods=["GET"])
        app.add_api_route("/metrics", ok_chat, methods=["GET"])
        return app

    async def raw_init_app_state(
        ec: Any, state: Any, args: Any, supported_tasks: Any = None
    ) -> None:
        state.engine_client = ec
        state.args = args

    fake_mod = SimpleNamespace(
        build_app=raw_build_app, init_app_state=raw_init_app_state
    )
    patch_vllm_api_server_module(fake_mod)

    args = SimpleNamespace()
    app = fake_mod.build_app(args, ("generate",))
    run_sync(fake_mod.init_app_state(engine_client, app.state, args, ("generate",)))
    return app


def test_vllm_multirank_shrink_active_reqs_cached_attrs_boot_defaults_reset_and_epoch():
    from gpustack_engine_sentinel.vllm.utility import attach_engine_core_utility

    attach_engine_core_utility(FakeEngineCore)
    sched0 = FakeVLLMScheduler(
        max_num_seqs=64, max_num_batched_tokens=4096, running_count=16
    )
    sched1 = FakeVLLMScheduler(
        max_num_seqs=64, max_num_batched_tokens=4096, running_count=12
    )
    core0 = FakeEngineCore(0, 2, sched0)
    core1 = FakeEngineCore(1, 2, sched1)
    mp_client = FakeDPLBAsyncMPClient([core0, core1])
    engine_client = FakeAsyncLLM(mp_client)
    app = _build_hooked_vllm_app(engine_client)

    # 1. Inspect initial state via GET /gpustack/v1/runtime_config (zero auth headers!).
    r_get = run_sync(send_asgi_request(app, "GET", "/gpustack/v1/runtime_config"))
    assert r_get.status_code == 200
    body0 = r_get.json()
    assert body0["engine"] == "vllm"
    assert len(body0["ranks"]) == 2
    frontend_boot_id = body0["boot_id"]
    assert set(body0["supported_fields"]) == {
        "scheduling.maxActiveRequests",
        "scheduling.maxBatchedTokens",
        "scheduling.longPrefillTokenThreshold",
        "scheduling.cacheAwareWindow",
        "scheduling.cacheAwareThreshold",
        "scheduling.maxInflightKVLoads",
        "scheduling.maxAsyncKVLoadsPerStep",
    }
    assert body0["boot_defaults"] == {
        "scheduling.maxActiveRequests": 64,
        "scheduling.maxBatchedTokens": 4096,
        "scheduling.longPrefillTokenThreshold": 0,
        "scheduling.cacheAwareWindow": 0,
        "scheduling.cacheAwareThreshold": 0.5,
        "scheduling.maxInflightKVLoads": 32,
        "scheduling.maxAsyncKVLoadsPerStep": 8,
    }

    # 2. Wrong expected_boot_id is rejected with 409 Conflict.
    r_bad_boot = run_sync(
        send_asgi_request(
            app,
            "POST",
            "/gpustack/v1/runtime_config",
            json_body={
                "operation_id": "op-bad",
                "expected_boot_id": "wrong-boot-id",
                "generation": 2,
                "applied_config_digest": "sha256:bad",
                "parameters": {"scheduling.maxActiveRequests": 4},
            },
        )
    )
    assert r_bad_boot.status_code == 409

    # 3. Exceeding boot ceiling (65 > 64) is rejected with 400 and rolls back cleanly!
    r_oob = run_sync(
        send_asgi_request(
            app,
            "POST",
            "/gpustack/v1/runtime_config",
            json_body={
                "operation_id": "op-oob",
                "expected_boot_id": frontend_boot_id,
                "generation": 2,
                "applied_config_digest": "sha256:oob",
                "parameters": {
                    "scheduling.cacheAwareWindow": 10,
                    "scheduling.maxActiveRequests": 65,
                },
            },
        )
    )
    assert r_oob.status_code == 400
    assert sched0.cache_aware_window == 0
    assert sched1.cache_aware_window == 0

    # 4. Shrink maxActiveRequests to 4 while 16 and 12 requests are actively running!
    r_apply = run_sync(
        send_asgi_request(
            app,
            "POST",
            "/gpustack/v1/runtime_config",
            json_body={
                "operation_id": "op-1",
                "expected_boot_id": frontend_boot_id,
                "generation": 3,
                "applied_config_digest": "sha256:gen3",
                "parameters": {
                    "scheduling.maxActiveRequests": 4,
                    "scheduling.maxBatchedTokens": 1024,
                    "scheduling.longPrefillTokenThreshold": 512,
                    "scheduling.cacheAwareWindow": 8,
                    "scheduling.cacheAwareThreshold": "0.65",
                    "scheduling.maxInflightKVLoads": 12,
                    "scheduling.maxAsyncKVLoadsPerStep": 3,
                },
            },
        )
    )
    assert r_apply.status_code == 200, r_apply.body
    applied = r_apply.json()
    assert applied["applied_config_digest"] == "sha256:gen3"
    assert [rk["rank_id"] for rk in applied["ranks"]] == [0, 1]
    assert all(rk["applied_digest"] == "sha256:gen3" for rk in applied["ranks"])
    assert applied["values"]["scheduling.maxActiveRequests"] == 4
    assert applied["values"]["scheduling.cacheAwareWindow"] == 8

    for sched in (sched0, sched1):
        # CRITICAL INVARIANT: max_num_running_reqs NEVER mutated!
        assert sched.max_num_running_reqs == 64
        sched.assert_step_invariants()
        assert sched.max_num_active_reqs == 4
        assert sched.max_num_scheduled_tokens == 1024
        assert sched.scheduler_config.long_prefill_token_threshold == 512
        assert sched.scheduler_config.cache_aware_admission_window == 8
        assert sched.cache_aware_window == 8
        assert sched.scheduler_config.cache_aware_admission_threshold == pytest.approx(
            0.65
        )
        assert sched.cache_aware_threshold == pytest.approx(0.65)
        assert sched.scheduler_config.max_num_inflight_kv_loads == 12
        assert sched.scheduler_config.max_async_kv_loads_per_step == 3

    # 5. Reset all fields back to exact boot defaults!
    r_reset = run_sync(
        send_asgi_request(
            app,
            "POST",
            "/gpustack/v1/runtime_config",
            json_body={
                "operation_id": "op-2",
                "expected_boot_id": frontend_boot_id,
                "generation": 4,
                "applied_config_digest": "sha256:gen4",
                "reset_fields": [
                    "scheduling.maxActiveRequests",
                    "scheduling.maxBatchedTokens",
                    "scheduling.longPrefillTokenThreshold",
                    "scheduling.cacheAwareWindow",
                    "scheduling.cacheAwareThreshold",
                    "scheduling.maxInflightKVLoads",
                    "scheduling.maxAsyncKVLoadsPerStep",
                ],
            },
        )
    )
    assert r_reset.status_code == 200, r_reset.body
    for sched in (sched0, sched1):
        assert sched.max_num_running_reqs == 64
        assert sched.max_num_active_reqs == 64
        assert sched.max_num_scheduled_tokens == 4096
        assert sched.scheduler_config.long_prefill_token_threshold == 0
        assert sched.cache_aware_window == 0
        assert sched.scheduler_config.cache_aware_admission_window == 0
        assert sched.cache_aware_threshold == pytest.approx(0.5)
        assert sched.scheduler_config.max_num_inflight_kv_loads == 32
        assert sched.scheduler_config.max_async_kv_loads_per_step == 8

    # 6. Simulate Elastic EP scale-up (DP 2 -> 3): membership_epoch increments!
    sched2 = FakeVLLMScheduler(max_num_seqs=64, max_num_batched_tokens=4096)
    core2 = FakeEngineCore(2, 3, sched2)
    for c in (core0, core1, core2):
        c.vllm_config.parallel_config.data_parallel_size = 3
    mp_client.cores.append(core2)
    mp_client.core_engines.append((2).to_bytes(2, "little"))

    r_scaled = run_sync(send_asgi_request(app, "GET", "/gpustack/v1/runtime_config"))
    assert r_scaled.status_code == 200
    scaled_body = r_scaled.json()
    assert scaled_body["membership_epoch"] == 2
    assert len(scaled_body["ranks"]) == 3


def test_vllm_attribute_drift_rollback_and_vllm_omni_degradation():
    from gpustack_engine_sentinel.vllm.utility import attach_engine_core_utility

    attach_engine_core_utility(FakeEngineCore)
    sched0 = FakeVLLMScheduler(has_kv_connector=False, enable_kv_load_controls=False)
    sched1 = FakeVLLMScheduler(has_kv_connector=False, enable_kv_load_controls=False)
    delattr(sched1, "cache_aware_window")

    mp_client = FakeDPLBAsyncMPClient(
        [FakeEngineCore(0, 2, sched0), FakeEngineCore(1, 2, sched1)]
    )
    app = _build_hooked_vllm_app(FakeAsyncLLM(mp_client))

    r_get = run_sync(send_asgi_request(app, "GET", "/gpustack/v1/runtime_config"))
    info = r_get.json()
    assert "scheduling.cacheAwareWindow" in info["unsupported_fields"]
    assert "scheduling.maxActiveRequests" in info["supported_fields"]

    # Mutating supported + unsupported field fails atomically (422) without mutating rank 0!
    r_post = run_sync(
        send_asgi_request(
            app,
            "POST",
            "/gpustack/v1/runtime_config",
            json_body={
                "operation_id": "op-drift",
                "expected_boot_id": info["boot_id"],
                "generation": 2,
                "applied_config_digest": "sha256:drift",
                "parameters": {
                    "scheduling.maxActiveRequests": 8,
                    "scheduling.cacheAwareWindow": 4,
                },
            },
        )
    )
    assert r_post.status_code == 422
    assert "scheduling.cacheAwareWindow" in r_post.json()["unsupported_fields"]
    assert sched0.max_num_active_reqs == 64
    assert sched1.max_num_active_reqs == 64

    # Simulate vllm-omni multi-stage AsyncOmni worker (no EngineCore call_utility_all_async):
    omni_client = SimpleNamespace(
        stage_configs=[{"stage_type": "diffusion"}], errored=False
    )
    omni_app = _build_hooked_vllm_app(omni_client)
    r_omni = run_sync(send_asgi_request(omni_app, "GET", "/gpustack/v1/runtime_config"))
    assert r_omni.status_code == 200
    omni_info = r_omni.json()
    assert omni_info["supported_fields"] == []
    assert "scheduling.maxActiveRequests" in omni_info["unsupported_fields"]

    r_omni_post = run_sync(
        send_asgi_request(
            omni_app,
            "POST",
            "/gpustack/v1/runtime_config",
            json_body={
                "operation_id": "op-omni",
                "expected_boot_id": omni_info["boot_id"],
                "generation": 1,
                "applied_config_digest": "sha256:omni",
                "parameters": {"scheduling.maxActiveRequests": 8},
            },
        )
    )
    assert r_omni_post.status_code == 422


def test_vllm_fresh_interpreter_sitecustomize_hooks_launchers_and_engine_core(
    tmp_path: Path,
):
    # Verify in a clean subprocess that sitecustomize.py hooks BOTH modern vllm.entrypoints.launchers.app
    # AND vllm.entrypoints.openai.api_server AND vllm.v1.engine.core without touching VLLM_PLUGINS!
    fake_vllm = tmp_path / "fake_vllm"
    (fake_vllm / "vllm" / "entrypoints" / "launchers" / "api_server").mkdir(
        parents=True
    )
    (fake_vllm / "vllm" / "entrypoints" / "openai").mkdir(parents=True)
    (fake_vllm / "vllm" / "v1" / "engine").mkdir(parents=True)
    for d in (
        fake_vllm / "vllm",
        fake_vllm / "vllm" / "entrypoints",
        fake_vllm / "vllm" / "entrypoints" / "launchers",
        fake_vllm / "vllm" / "entrypoints" / "launchers" / "api_server",
        fake_vllm / "vllm" / "entrypoints" / "openai",
        fake_vllm / "vllm" / "v1",
        fake_vllm / "vllm" / "v1" / "engine",
    ):
        (d / "__init__.py").write_text("", encoding="utf-8")

    (fake_vllm / "vllm" / "entrypoints" / "launchers" / "app.py").write_text(
        """
from asgi_harness import FakeFastAPIApp
def build_app(args, supported_tasks=None, model_config=None):
    app = FakeFastAPIApp()
    app.state.args = args
    return app
""",
        encoding="utf-8",
    )
    (
        fake_vllm / "vllm" / "entrypoints" / "launchers" / "api_server" / "app_state.py"
    ).write_text(
        """
async def init_app_state(engine_client, state, args, supported_tasks=None):
    state.engine_client = engine_client
    state.args = args
""",
        encoding="utf-8",
    )
    (fake_vllm / "vllm" / "entrypoints" / "openai" / "api_server.py").write_text(
        """
from vllm.entrypoints.launchers.app import build_app
from vllm.entrypoints.launchers.api_server.app_state import init_app_state
""",
        encoding="utf-8",
    )
    (fake_vllm / "vllm" / "v1" / "engine" / "core.py").write_text(
        "class EngineCore:\n    pass\n",
        encoding="utf-8",
    )

    env = dict(os.environ)
    env["GPUSTACK_ENGINE_SENTINEL_ENABLE"] = "1"
    env.pop("VLLM_PLUGINS", None)
    env["PYTHONPATH"] = os.pathsep.join(
        [
            str(REPO_PYTHON_ROOT),
            str(fake_vllm),
            str(Path(__file__).resolve().parent),
        ]
    )

    script = """
import json, os
from types import SimpleNamespace
from asgi_harness import run_sync, send_asgi_request
from vllm.entrypoints.openai.api_server import build_app, init_app_state
from vllm.v1.engine.core import EngineCore

app = build_app(SimpleNamespace())
ec = SimpleNamespace(errored=False, engine_core=SimpleNamespace(resources=SimpleNamespace(engine_dead=False)))
run_sync(init_app_state(ec, app.state, SimpleNamespace()))

r_ready = run_sync(send_asgi_request(app, "GET", "/gpustack/v1/ready"))
r_live = run_sync(send_asgi_request(app, "GET", "/gpustack/v1/live"))
print(json.dumps({
    "ready_status": r_ready.status_code,
    "live_status": r_live.status_code,
    "has_rpc": hasattr(EngineCore, "gpustack_engine_sentinel_rpc"),
    "vllm_plugins": os.environ.get("VLLM_PLUGINS"),
}))
"""
    res = subprocess.run(
        [sys.executable, "-c", script],
        env=env,
        capture_output=True,
        text=True,
        check=True,
    )
    out = json.loads(res.stdout.strip())
    assert out["ready_status"] == 503
    assert out["live_status"] == 200
    assert out["has_rpc"] is True
    assert out["vllm_plugins"] is None
