"""Tests for /gpustack/v1/probe abuse controls, /dev/shm bounds, GEMM/collective filtering, and distillation."""

from __future__ import annotations

import asyncio
import hashlib
import json
import os
import sys
import time
from pathlib import Path
from typing import Any

import pytest
from asgi_harness import FakeFastAPIApp, run_sync, send_asgi_request

from gpustack_engine_sentinel.latch import BootLatch
from gpustack_engine_sentinel.probe import (
    ALLOWED_OP_FAMILIES,
    ProbeManager,
    classify_op_family,
    compute_shm_budget_bytes,
    compute_signature_hash16,
    distill_trace_events,
    format_local_kernel_key,
    is_excluded_operator,
    normalize_engine_version,
    parse_local_kernel_key,
    purge_probe_dir,
    sweep_stale_probe_dirs,
)
from gpustack_engine_sentinel.protocol import (
    ENV_PROBE_AUTH_TOKEN,
    ENV_PROBE_MIN_COOLDOWN_SECONDS,
    ENV_PROBE_SHM_SIZE_BYTES,
    PROBE_MAX_SHM_BYTES,
)
from gpustack_engine_sentinel.server import (
    SentinelControlPlane,
    ensure_outermost_sentinel_middleware,
)


def test_probe_module_imports_zero_heavy_ml_deps() -> None:
    assert "torch" not in sys.modules
    assert "triton" not in sys.modules
    assert "flashinfer" not in sys.modules
    assert "fastapi" not in sys.modules
    assert "starlette" not in sys.modules


@pytest.mark.parametrize(
    "op_name",
    [
        "gemm",
        "matmul",
        "bmm",
        "addmm",
        "cublasLtMatmul",
        "cublasSgemm",
        "hipblaslt_matmul",
        "rocblas_gemm_ex",
        "aten::mm",
        "aten::addmm",
        "aten::bmm",
        "aten::matmul",
        "aten::linear",
        "fp8_blockwise_gemm",
        "mxfp8_gemm",
        "nvfp4_gemm",
        "cutlass_block_scaled_gemm",
        "deep_gemm_fp8_nt",
        "ncclKernel_AllReduce_RING_LL",
        "rccl_all_gather",
        "hccl_reduce_scatter",
        "all_reduce",
        "all_gather",
        "reduce_scatter",
        "all_to_all",
        "fused_all_reduce_rmsnorm",
        "vllm::fused_all_reduce_rms_norm",
        "mscclpp_rms_norm",
        "msccl_rmsnorm",
        "mscclpp_fused_add_rmsnorm",
        "rocshmem_fused_add",
        "rocshmem_all_to_all",
        "c10d::allreduce_",
    ],
)
def test_gemm_and_collective_operators_strictly_excluded(op_name: str) -> None:
    assert is_excluded_operator(op_name) is True
    assert classify_op_family(op_name) is None


def test_msccl_and_rocshmem_collectives_excluded_from_distillation() -> None:
    payload = distill_trace_events(
        [
            {
                "name": "mscclpp_rms_norm",
                "dur": 200.0,
                "input_dims": [[32, 4096], [4096]],
                "input_dtypes": ["bfloat16", "bfloat16"],
            },
            {
                "name": "rocshmem_fused_add",
                "dur": 150.0,
                "input_dims": [[32, 4096], [32, 4096]],
                "input_dtypes": ["bfloat16", "bfloat16"],
            },
        ],
        engine="sglang",
        engine_version="0.4.6",
        manufacturer="amd",
        architecture="gfx942",
        steps_captured=3,
    )
    assert payload["definitions"] == []
    assert payload["workloads"] == []
    assert payload["diagnostics"]["filtered_excluded_count"] == 2


@pytest.mark.parametrize(
    "op_name,expected_family",
    [
        ("vllm::rms_norm", "rmsnorm"),
        ("sglang::fused_add_rms_norm", "rmsnorm"),
        ("aten::layer_norm", "layernorm"),
        ("vllm::rotary_embedding", "rope_kv_append"),
        ("vllm::reshape_and_cache_flash", "rope_kv_append"),
        ("vllm::silu_and_mul", "activation_quant"),
        ("vllm::dynamic_scaled_fp8_quant", "activation_quant"),
        ("vllm::topk_softmax", "moe_router_topk"),
        ("vllm::moe_align_block_size", "moe_permute_unpermute"),
        ("vllm::paged_attention_v2", "paged_attention"),
        ("flashinfer::batch_prefill_with_ragged_kv", "ragged_prefill_attention"),
        ("flash_mla_decode", "mla_local"),
        ("sglang::verify_tree_greedy", "speculative_verify"),
        ("flashinfer::top_k_top_p_sampling_from_probs", "sampling_logits"),
        ("custom_elementwise_fusion", "custom_elementwise_fusion"),
    ],
)
def test_allowed_op_families_classified_correctly(
    op_name: str, expected_family: str
) -> None:
    assert expected_family in ALLOWED_OP_FAMILIES
    assert is_excluded_operator(op_name) is False
    assert classify_op_family(op_name) == expected_family


def test_local_kernel_key_canonical_hash_const_axes_only_and_version_norm() -> None:
    expected_versions = (
        ("0.8.5", "0.8"),
        ("0.8.6", "0.8"),
        ("0.8", "0.8"),
        ("v0.8.5+cu128", "0.8"),
        ("V0.8.5+cu128", "0.8"),
        ("0.8.5-12-g1a2b3c4", "0.8"),
        ("0.8.6.post1", "0.8"),
        ("0.4.6.post2", "0.4"),
        ("0.4.6.post1+rocm6.3", "0.4"),
        ("0.9.0", "0.9"),
        ("0.9.0-gabcdef1-dirty", "0.9"),
        ("0.9.0rc1", "0.9rc1"),
        ("0.9.0-rc.1", "0.9-rc.1"),
        ("0.9.0.dev0", "0.9.dev0"),
        ("0.9.0.dev123+gabcdef1", "0.9.dev123"),
        ("0.9.0a1", "0.9a1"),
        ("0.9.0b2", "0.9b2"),
        ("0.34", "0.34"),
        ("0.10.1.1+cu126", "0.10"),
    )
    for raw_ver, want_ver in expected_versions:
        assert normalize_engine_version(raw_ver) == want_ver

    for bad_ver in ("", "   ", "latest", "1", "0.8.5/evil", "0.34.", "0.8.5 extra"):
        with pytest.raises(ValueError):
            normalize_engine_version(bad_ver)

    const_axes = {"hidden_size": 4096}
    in_dtypes = ["torch.bfloat16", "bfloat16"]
    out_dtypes = ["bfloat16"]
    constants = {"eps": 1e-6}

    expected_canonical = json.dumps(
        {
            "constAxes": {"hidden_size": 4096},
            "constants": {"eps": 1e-6},
            "inputDtypes": ["bfloat16", "bfloat16"],
            "outputDtypes": ["bfloat16"],
        },
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
    ).encode("utf-8")
    expected_sig = hashlib.sha256(expected_canonical).hexdigest()[:16]
    assert (
        compute_signature_hash16(const_axes, in_dtypes, out_dtypes, constants)
        == expected_sig
    )
    # Dynamic var axes accidentally passed in const_axes are stripped so hash stays identical.
    assert (
        compute_signature_hash16(
            {"hidden_size": 4096, "batch_size": 128, "num_tokens": 512},
            in_dtypes,
            out_dtypes,
            constants,
        )
        == expected_sig
    )

    key = format_local_kernel_key(
        engine="vLLM",
        engine_version="0.8.5+cu128",
        manufacturer="NVIDIA",
        arch="SM_90A",
        op_family="rmsnorm",
        const_axes=const_axes,
        input_dtypes=in_dtypes,
        output_dtypes=out_dtypes,
        constants=constants,
    )
    assert key == f"vllm/0.8/nvidia/sm_90a/rmsnorm/{expected_sig}"
    for same_line_ver in (
        "0.8.5",
        "0.8.6",
        "0.8",
        "v0.8.5+cu128",
        "0.8.5-12-g1a2b3c4",
        "0.8.6.post1",
    ):
        assert (
            format_local_kernel_key(
                engine="vLLM",
                engine_version=same_line_ver,
                manufacturer="NVIDIA",
                arch="SM_90A",
                op_family="rmsnorm",
                const_axes=const_axes,
                input_dtypes=in_dtypes,
                output_dtypes=out_dtypes,
                constants=constants,
            )
            == key
        )

    key_09 = format_local_kernel_key(
        engine="vLLM",
        engine_version="0.9.0",
        manufacturer="NVIDIA",
        arch="SM_90A",
        op_family="rmsnorm",
        const_axes=const_axes,
        input_dtypes=in_dtypes,
        output_dtypes=out_dtypes,
        constants=constants,
    )
    key_09_rc1 = format_local_kernel_key(
        engine="vLLM",
        engine_version="0.9.0rc1",
        manufacturer="NVIDIA",
        arch="SM_90A",
        op_family="rmsnorm",
        const_axes=const_axes,
        input_dtypes=in_dtypes,
        output_dtypes=out_dtypes,
        constants=constants,
    )
    assert key_09 == f"vllm/0.9/nvidia/sm_90a/rmsnorm/{expected_sig}"
    assert key_09_rc1 == f"vllm/0.9rc1/nvidia/sm_90a/rmsnorm/{expected_sig}"
    assert key_09 != key
    assert key_09_rc1 != key_09

    parsed = parse_local_kernel_key(key)
    assert parsed == {
        "engine": "vllm",
        "engine_version": "0.8",
        "manufacturer": "nvidia",
        "architecture": "sm_90a",
        "op_family": "rmsnorm",
        "signature_hash16": expected_sig,
    }

    with pytest.raises(ValueError):
        format_local_kernel_key(
            engine="vLLM",
            engine_version="0.8.5",
            manufacturer="nvidia",
            arch="sm_90a",
            op_family="gemm",
            const_axes=const_axes,
            input_dtypes=in_dtypes,
            output_dtypes=out_dtypes,
        )
    with pytest.raises(ValueError):
        parse_local_kernel_key("vLLM/0.8.5/nvidia/sm_90a/rmsnorm/nothex")


def test_distillation_merges_var_axes_filters_gemm_and_leaks_zero_prompts() -> None:
    secret_prompt = "TOP_SECRET_USER_PROMPT_XYZ_999"
    events = [
        {
            "name": "vllm::rms_norm",
            "dur": 120.0,
            "input_dims": [[32, 4096], [4096]],
            "input_dtypes": ["bfloat16", "bfloat16"],
            "args": {
                "prompt": secret_prompt,
                "input_ids": [101, 202, 303, 404],
                "raw_tensor": [0.123456, -0.987654],
            },
        },
        {
            "name": "vllm::rms_norm",
            "dur": 180.0,
            "input_dims": [[128, 4096], [4096]],
            "input_dtypes": ["bfloat16", "bfloat16"],
            "args": {"text": secret_prompt, "tokens": [999, 888]},
        },
        {
            "name": "cublasLtMatmul",
            "dur": 900.0,
            "input_dims": [[128, 4096], [4096, 4096]],
            "input_dtypes": ["bfloat16", "bfloat16"],
        },
        {
            "name": "fp8_blockwise_gemm",
            "dur": 500.0,
            "input_dims": [[128, 4096], [4096, 4096]],
        },
        {
            "name": "fused_all_reduce_rmsnorm",
            "dur": 400.0,
            "input_dims": [[128, 4096]],
        },
        {
            "name": "cudaGraphLaunch_decode_step",
            "cat": "cuda_graph",
            "dur": 55.0,
        },
    ]

    payload = distill_trace_events(
        events,
        engine="vllm",
        engine_version="0.8.5+cu128",
        manufacturer="nvidia",
        architecture="sm_90a",
        steps_captured=4,
    )

    # Different batch/num_tokens (32 vs 128) collapse into ONE Definition + TWO Workloads!
    assert len(payload["definitions"]) == 1
    assert len(payload["workloads"]) == 2
    assert payload["definitions"][0]["op_type"] == "rmsnorm"
    assert payload["definitions"][0]["axes"] == {
        "hidden_size": {"type": "const", "value": 4096},
        "num_tokens": {"type": "var"},
    }
    observed_tokens = sorted(w["axes"]["num_tokens"] for w in payload["workloads"])
    assert observed_tokens == [32, 128]
    assert payload["diagnostics"]["filtered_excluded_count"] == 3
    assert payload["diagnostics"]["steps_captured"] == 4
    assert len(payload["diagnostics"]["cuda_graph_hotspots"]) == 1

    serialized = json.dumps(payload)
    assert secret_prompt not in serialized
    assert "input_ids" not in serialized
    assert "raw_tensor" not in serialized
    assert "101, 202" not in serialized
    assert "0.123456" not in serialized


def test_distillation_enforces_512kib_payload_cap() -> None:
    events = [
        {
            "name": "vllm::rms_norm",
            "dur": float(i + 1),
            "input_dims": [[i + 1, 4096], [4096]],
            "input_dtypes": ["bfloat16", "bfloat16"],
        }
        for i in range(400)
    ]
    payload = distill_trace_events(
        events,
        engine="vllm",
        engine_version="0.8.5",
        manufacturer="nvidia",
        architecture="sm_90a",
        steps_captured=4,
        max_payload_bytes=4096,
    )
    assert payload["diagnostics"]["truncated"] is True
    assert len(json.dumps(payload, separators=(",", ":")).encode("utf-8")) <= 4096


def test_shm_budget_and_stale_dir_sweep(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    # Default 16Gi -> min(64 MiB, 5% of 16Gi) = 64 MiB.
    monkeypatch.delenv(ENV_PROBE_SHM_SIZE_BYTES, raising=False)
    assert compute_shm_budget_bytes(tmp_path) == PROBE_MAX_SHM_BYTES

    # Smaller shmSize (256 MiB) -> 5% = 13,421,772 bytes.
    small_shm = 256 * 1024 * 1024
    monkeypatch.setenv(ENV_PROBE_SHM_SIZE_BYTES, str(small_shm))
    assert compute_shm_budget_bytes(tmp_path) == int(small_shm * 0.05)

    # Stale directory sweep removes >60s dirs while keeping fresh dirs.
    old_dir = tmp_path / "stale-probe-1"
    fresh_dir = tmp_path / "fresh-probe-2"
    old_dir.mkdir()
    fresh_dir.mkdir()
    (old_dir / "rank0.trace.json").write_text("{}", encoding="utf-8")
    (fresh_dir / "rank0.trace.json").write_text("{}", encoding="utf-8")

    now = time.time()
    os.utime(old_dir, (now - 120.0, now - 120.0))
    os.utime(fresh_dir, (now - 5.0, now - 5.0))

    removed = sweep_stale_probe_dirs(tmp_path, max_age_seconds=60.0, now=now)
    assert removed == ["stale-probe-1"]
    assert not old_dir.exists()
    assert fresh_dir.exists()
    assert purge_probe_dir(fresh_dir) == 1
    assert not fresh_dir.exists()


class _MockProbeBackend:
    def __init__(self, *, complete_immediately: bool = False) -> None:
        self.engine = "vllm"
        self.alive = True
        self.paused = False
        self.complete_immediately = complete_immediately
        self.started_specs: list[dict[str, Any]] = []
        self.aborted_ids: list[str] = []
        self.next_collect_response: dict[str, Any] = {"status": "running"}

    async def check_live(self) -> tuple[bool, bool, str | None]:
        return self.alive, self.paused, None

    async def get_runtime_config(self) -> dict[str, Any]:
        return {
            "membership_epoch": 1,
            "supported_fields": [],
            "unsupported_fields": [],
            "boot_defaults": {},
            "ranks": [],
            "values": {},
        }

    async def apply_runtime_config(
        self, request_data: dict[str, Any]
    ) -> dict[str, Any]:
        del request_data
        return await self.get_runtime_config()

    async def start_probe(self, spec: dict[str, Any]) -> dict[str, Any]:
        self.started_specs.append(spec)
        out_dir = Path(spec["output_dir"])
        (out_dir / "raw-rank0.trace.json").write_text("{}", encoding="utf-8")
        if self.complete_immediately:
            payload = distill_trace_events(
                [
                    {
                        "name": "vllm::rms_norm",
                        "dur": 42.0,
                        "input_dims": [[16, 4096], [4096]],
                        "input_dtypes": ["bfloat16", "bfloat16"],
                    }
                ],
                engine="vllm",
                engine_version=spec["engine_version"],
                manufacturer=spec["manufacturer"],
                architecture=spec["architecture"],
                steps_captured=spec["steps"],
            )
            return {"status": "completed", "payload": payload}
        return {"status": "running"}

    async def collect_probe(self, spec: dict[str, Any]) -> dict[str, Any]:
        del spec
        return dict(self.next_collect_response)

    async def abort_probe(self, spec: dict[str, Any]) -> dict[str, Any]:
        self.aborted_ids.append(spec["probe_id"])
        return {"status": "aborted"}


def test_probe_http_guards_cooldown_auth_abort_timeout_and_shm_cap(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.delenv(ENV_PROBE_AUTH_TOKEN, raising=False)
    monkeypatch.delenv(ENV_PROBE_MIN_COOLDOWN_SECONDS, raising=False)

    # Pre-seed a stale dir to verify SentinelControlPlane sweeps on startup.
    stale_dir = tmp_path / "orphan-old"
    stale_dir.mkdir()
    old_ts = time.time() - 120.0
    os.utime(stale_dir, (old_ts, old_ts))

    fake_now = [1000.0]
    backend = _MockProbeBackend(complete_immediately=True)
    latch = BootLatch(require_ack=False)
    mgr = ProbeManager(
        backend=backend,
        latch=latch,
        staging_root=tmp_path,
        cooldown_seconds=60.0,
        wall_clock_cap_seconds=15.0,
        shm_size_override=2000,  # 5% -> 100 byte staging ceiling for budget test later.
        clock=lambda: fake_now[0],
        sweep_on_init=True,
    )
    app = FakeFastAPIApp()
    plane = SentinelControlPlane(backend=backend, latch=latch, probe_manager=mgr)
    plane.install_routes(app)
    ensure_outermost_sentinel_middleware(app, control_plane=plane)

    assert not stale_dir.exists()
    assert mgr.swept_stale_dirs == ["orphan-old"]

    # 1. Initial GET returns idle.
    r0 = run_sync(send_asgi_request(app, "GET", "/gpustack/v1/probe"))
    assert r0.status_code == 200
    assert r0.json()["state"] == "idle"

    # 2. Method validation (PUT -> 405).
    assert (
        run_sync(send_asgi_request(app, "PUT", "/gpustack/v1/probe")).status_code == 405
    )

    # 3. Steps validation strictly [3, 5].
    for bad_steps in (2, 6, True, "abc"):
        r_bad = run_sync(
            send_asgi_request(
                app,
                "POST",
                "/gpustack/v1/probe",
                json_body={"expected_boot_id": latch.boot_id, "steps": bad_steps},
            )
        )
        assert r_bad.status_code == 400

    # 4. Boot ID mismatch -> 409 boot_id_mismatch.
    r_boot = run_sync(
        send_asgi_request(
            app,
            "POST",
            "/gpustack/v1/probe",
            json_body={"expected_boot_id": "wrong-boot", "steps": 4},
        )
    )
    assert r_boot.status_code == 409
    assert r_boot.json()["error"] == "boot_id_mismatch"

    # 5. Refusal while paused -> 409 role_paused.
    latch.set_paused(True)
    r_paused = run_sync(
        send_asgi_request(
            app,
            "POST",
            "/gpustack/v1/probe",
            json_body={"expected_boot_id": latch.boot_id, "steps": 4},
        )
    )
    assert r_paused.status_code == 409
    assert r_paused.json()["error"] == "role_paused"
    latch.set_paused(False)

    # 6. Successful probe with default steps=4 purges staging dir immediately.
    r_ok = run_sync(
        send_asgi_request(
            app,
            "POST",
            "/gpustack/v1/probe",
            json_body={
                "expected_boot_id": latch.boot_id,
                "engine_version": "0.8.5+cu128",
                "manufacturer": "nvidia",
                "architecture": "sm_90a",
            },
        )
    )
    assert r_ok.status_code == 200
    body = r_ok.json()
    assert body["state"] == "completed"
    assert body["steps"] == 4
    assert len(body["payload"]["definitions"]) == 1
    assert not (tmp_path / body["probe_id"]).exists()

    # 7. Immediate second POST within 60s -> 429 probe_cooldown with Retry-After header.
    fake_now[0] += 10.0
    r_cd = run_sync(
        send_asgi_request(
            app,
            "POST",
            "/gpustack/v1/probe",
            json_body={"expected_boot_id": latch.boot_id, "steps": 3},
        )
    )
    assert r_cd.status_code == 429
    assert r_cd.json()["error"] == "probe_cooldown"
    assert "retry-after" in r_cd.headers

    # 8. Optional auth token gate via GPUSTACK_PROBE_AUTH_TOKEN.
    monkeypatch.setenv(ENV_PROBE_AUTH_TOKEN, "s3cr3t-token")
    assert (
        run_sync(send_asgi_request(app, "GET", "/gpustack/v1/probe")).status_code == 401
    )
    assert (
        run_sync(
            send_asgi_request(
                app,
                "GET",
                "/gpustack/v1/probe",
                headers={"Authorization": "Bearer wrong"},
            )
        ).status_code
        == 401
    )
    assert (
        run_sync(
            send_asgi_request(
                app,
                "GET",
                "/gpustack/v1/probe",
                headers={"Authorization": "Bearer s3cr3t-token"},
            )
        ).status_code
        == 200
    )
    assert (
        run_sync(
            send_asgi_request(
                app,
                "GET",
                "/gpustack/v1/probe",
                headers={"X-GPUStack-Probe-Token": "s3cr3t-token"},
            )
        ).status_code
        == 200
    )
    monkeypatch.delenv(ENV_PROBE_AUTH_TOKEN, raising=False)

    # 9. Concurrent probe -> 409 probe_in_progress, and DELETE aborts + purges staging dir.
    async def _run_async_probe_guards() -> None:
        backend.complete_immediately = False
        fake_now[0] += 120.0
        r_start = await send_asgi_request(
            app,
            "POST",
            "/gpustack/v1/probe",
            json_body={"expected_boot_id": latch.boot_id, "steps": 5},
        )
        assert r_start.status_code == 200
        pid = r_start.json()["probe_id"]
        assert r_start.json()["state"] == "running"

        # Second concurrent POST returns 409 probe_in_progress.
        r_dup = await send_asgi_request(
            app,
            "POST",
            "/gpustack/v1/probe",
            json_body={"expected_boot_id": latch.boot_id, "steps": 3},
        )
        assert r_dup.status_code == 409
        assert r_dup.json()["error"] == "probe_in_progress"

        # DELETE aborts active probe and deletes staging directory.
        r_del = await send_asgi_request(app, "DELETE", "/gpustack/v1/probe")
        assert r_del.status_code == 200
        assert r_del.json()["state"] == "aborted"
        assert pid in backend.aborted_ids
        assert not (tmp_path / pid).exists()

        # 10. Staging byte ceiling violation aborts and purges staging dir.
        fake_now[0] += 120.0
        r_cap = await send_asgi_request(
            app,
            "POST",
            "/gpustack/v1/probe",
            json_body={"expected_boot_id": latch.boot_id, "steps": 3},
        )
        assert r_cap.status_code == 200
        pid_cap = r_cap.json()["probe_id"]
        # Write > 100 bytes (5% of 2000) into staging dir.
        (tmp_path / pid_cap / "huge.trace.json").write_bytes(b"x" * 256)
        await asyncio.sleep(0.06)
        r_after_cap = await send_asgi_request(app, "GET", "/gpustack/v1/probe")
        assert r_after_cap.json()["state"] == "failed"
        assert r_after_cap.json()["error_code"] == "probe_shm_budget_exceeded"
        assert not (tmp_path / pid_cap).exists()

        # 11. Wall-clock ceiling (> 15s) aborts and purges staging dir.
        fake_now[0] += 120.0
        r_to = await send_asgi_request(
            app,
            "POST",
            "/gpustack/v1/probe",
            json_body={"expected_boot_id": latch.boot_id, "steps": 3},
        )
        assert r_to.status_code == 200
        pid_to = r_to.json()["probe_id"]
        fake_now[0] += 16.0
        await asyncio.sleep(0.06)
        r_after_to = await send_asgi_request(app, "GET", "/gpustack/v1/probe")
        assert r_after_to.json()["state"] == "failed"
        assert r_after_to.json()["error_code"] == "probe_timeout"
        assert not (tmp_path / pid_to).exists()

    run_sync(_run_async_probe_guards())


def test_distillation_covers_all_op_families_and_plain_json_trace(
    tmp_path: Path,
) -> None:
    from gpustack_engine_sentinel.probe import distill_probe_directory

    trace_events = [
        {
            "name": "vllm::rotary_embedding",
            "dur": 50.0,
            "input_dims": [[32, 8, 128]],
            "input_dtypes": ["fp16"],
        },
        {
            "name": "vllm::rotary_embedding",
            "dur": 25.0,
            "input_dims": [[32, 128]],
            "input_dtypes": ["fp16"],
        },
        {
            "name": "vllm::topk_softmax",
            "dur": 40.0,
            "input_dims": [[32, 8]],
            "input_dtypes": ["float32"],
            "constants": {"top_k": 2, "prompt": "ignored"},
        },
        {
            "name": "vllm::moe_align_block_size",
            "dur": 35.0,
            "input_dims": [[32, 4096]],
            "input_dtypes": ["bfloat16"],
        },
        {
            "name": "flashinfer::top_k_top_p_sampling_from_probs",
            "dur": 30.0,
            "input_dims": [[8, 32000]],
            "input_dtypes": ["float32"],
        },
        {
            "name": "vllm::paged_attention_v2",
            "dur": 90.0,
            "input_dims": [],
        },
        {
            "name": "unknown_unclassified_kernel",
            "dur": 10.0,
            "input_dims": [[4, 4]],
        },
    ]
    probe_dir = tmp_path / "plain_trace"
    probe_dir.mkdir()
    (probe_dir / "rank-0.trace.json").write_text(
        json.dumps(trace_events), encoding="utf-8"
    )
    payload = distill_probe_directory(
        probe_dir,
        engine="vllm",
        engine_version="0.8.5",
        manufacturer="nvidia",
        architecture="sm_90a",
        steps_captured=3,
    )
    families = {d["op_type"] for d in payload["definitions"]}
    assert {
        "rope_kv_append",
        "moe_router_topk",
        "moe_permute_unpermute",
        "sampling_logits",
    }.issubset(families)
    assert payload["diagnostics"]["unclassified_count"] == 1
    assert any(
        h["kernel_name"] == "vllm::paged_attention_v2"
        for h in payload["diagnostics"]["cuda_graph_hotspots"]
    )


def test_probe_edge_cases_error_paths_and_definition_trimming(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    from gpustack_engine_sentinel.probe import (
        canonical_engine_name,
        load_chrome_trace_events,
        select_rank0_trace_file,
        validate_probe_auth,
    )

    assert is_excluded_operator("") is True
    assert is_excluded_operator(123) is True  # type: ignore[arg-type]
    assert classify_op_family("") is None
    assert canonical_engine_name("custom") == "custom"
    with pytest.raises(ValueError):
        normalize_engine_version("")
    with pytest.raises(ValueError):
        normalize_engine_version(None)

    with pytest.raises(ValueError):
        parse_local_kernel_key("")
    with pytest.raises(ValueError):
        parse_local_kernel_key("too/few/parts")
    with pytest.raises(ValueError):
        parse_local_kernel_key("vLLM/0.8.5/nvidia/sm_90a/gemm/0123456789abcdef")

    # Dict header validation path.
    validate_probe_auth({"Authorization": "Bearer tok1"}, expected_token="tok1")
    with pytest.raises(Exception):
        validate_probe_auth({"Authorization": "Bearer wrong"}, expected_token="tok1")

    # purge_probe_dir with nested subdirs and symlinks + sweep stale file.
    nested = tmp_path / "nested_purge"
    (nested / "sub").mkdir(parents=True)
    (nested / "sub" / "f.json").write_text("{}", encoding="utf-8")
    os.symlink(tmp_path, nested / "link_dir")
    assert purge_probe_dir(nested) >= 2
    assert not nested.exists()

    orphan_file = tmp_path / "orphan.tmp"
    orphan_file.write_text("x", encoding="utf-8")
    old_t = time.time() - 120.0
    os.utime(orphan_file, (old_t, old_t))
    assert "orphan.tmp" in sweep_stale_probe_dirs(tmp_path, max_age_seconds=60.0)
    assert select_rank0_trace_file(tmp_path / "missing_dir") is None

    bad_trace = tmp_path / "scalar.json"
    bad_trace.write_text("123", encoding="utf-8")
    assert load_chrome_trace_events(bad_trace) == []

    # Definition trimming when budget is tiny across multiple definitions.
    multi_def_events = [
        {
            "name": "vllm::rms_norm",
            "dur": 100.0,
            "input_dims": [[32, 4096 + i * 128]],
            "input_dtypes": "bfloat16",
            "output_dtypes": "bfloat16",
            "count": "bad-count",
        }
        for i in range(12)
    ] + [
        {"name": "vllm::rms_norm", "dur": -5.0, "input_dims": [4096]},
        {"name": "", "dur": "bad"},
        "not-a-dict",  # type: ignore[list-item]
    ]
    trimmed = distill_trace_events(
        multi_def_events,
        engine="vllm",
        engine_version="0.8.5",
        manufacturer="nvidia",
        architecture="sm_90a",
        steps_captured=3,
        max_payload_bytes=900,
    )
    assert trimmed["diagnostics"]["truncated"] is True
    assert len(trimmed["definitions"]) < 12

    # ProbeManager env cooldown, missing start_probe (422), failing start/collect, aborted/failed results.
    monkeypatch.setenv(ENV_PROBE_MIN_COOLDOWN_SECONDS, "45")
    latch = BootLatch(require_ack=False)

    class _NoProbeBackend:
        engine = "generic"

        async def check_live(self) -> tuple[bool, bool, str | None]:
            return True, False, None

    mgr_no = ProbeManager(
        backend=_NoProbeBackend(),
        latch=latch,
        staging_root=tmp_path / "no_probe",
        sweep_on_init=False,
    )
    assert mgr_no.staging_root == tmp_path / "no_probe"
    with pytest.raises(Exception) as exc_info:
        run_sync(mgr_no.start_probe({"expected_boot_id": latch.boot_id, "steps": 4}))
    assert getattr(exc_info.value, "error_code", "") == "unsupported_engine_version"

    class _ErrorBackend:
        engine = "vllm"

        def __init__(self) -> None:
            self.mode = "start_fail"

        async def check_live(self) -> tuple[bool, bool, str | None]:
            return True, False, None

        async def start_probe(self, spec: dict[str, Any]) -> dict[str, Any]:
            del spec
            if self.mode == "start_fail":
                raise RuntimeError("boom start")
            if self.mode == "immediate_failed":
                return {"status": "failed", "error_code": "err_f", "message": "failed"}
            if self.mode == "immediate_aborted":
                return {
                    "status": "aborted",
                    "error_code": "err_a",
                    "message": "aborted",
                }
            return {"status": "running"}

        async def collect_probe(self, spec: dict[str, Any]) -> dict[str, Any]:
            del spec
            raise RuntimeError("collect exploded")

    eb = _ErrorBackend()
    mgr_err = ProbeManager(
        backend=eb,
        latch=latch,
        staging_root=tmp_path / "err_probe",
        cooldown_seconds=0.0,
        sweep_on_init=False,
    )
    with pytest.raises(Exception) as exc_start:
        run_sync(mgr_err.start_probe({"expected_boot_id": latch.boot_id, "steps": 4}))
    assert getattr(exc_start.value, "status_code", 0) == 500

    eb.mode = "immediate_failed"
    _, res_f = run_sync(
        mgr_err.start_probe({"expected_boot_id": latch.boot_id, "steps": 4})
    )
    assert res_f["state"] == "failed"

    eb.mode = "immediate_aborted"
    _, res_a = run_sync(
        mgr_err.start_probe({"expected_boot_id": latch.boot_id, "steps": 4})
    )
    assert res_a["state"] == "aborted"

    async def _run_collect_fail() -> None:
        eb.mode = "collect_fail"
        await mgr_err.start_probe({"expected_boot_id": latch.boot_id, "steps": 4})
        await asyncio.sleep(0.05)
        _, st = await mgr_err.get_probe()
        assert st["state"] == "failed"
        assert st["error_code"] == "probe_collect_failed"

    run_sync(_run_collect_fail())


def test_probe_manager_concurrent_start_probe_serialized_with_asyncio_lock(
    tmp_path: Path,
) -> None:
    class _YieldingProbeBackend(_MockProbeBackend):
        def __init__(self, *, complete_immediately: bool = False) -> None:
            super().__init__(complete_immediately=complete_immediately)
            self.start_invocations = 0

        async def start_probe(self, spec: dict[str, Any]) -> dict[str, Any]:
            self.start_invocations += 1
            await asyncio.sleep(0)
            return await super().start_probe(spec)

    async def _run_race_in_progress() -> None:
        staging_root = tmp_path / "race_running"
        backend = _YieldingProbeBackend(complete_immediately=False)
        latch = BootLatch(require_ack=False)
        mgr = ProbeManager(
            backend=backend,
            latch=latch,
            staging_root=staging_root,
            cooldown_seconds=0.0,
            sweep_on_init=False,
        )
        app = FakeFastAPIApp()
        plane = SentinelControlPlane(backend=backend, latch=latch, probe_manager=mgr)
        ensure_outermost_sentinel_middleware(app, control_plane=plane)

        body = {"expected_boot_id": latch.boot_id, "steps": 3}
        r1, r2 = await asyncio.gather(
            send_asgi_request(app, "POST", "/gpustack/v1/probe", json_body=body),
            send_asgi_request(app, "POST", "/gpustack/v1/probe", json_body=body),
        )
        statuses = sorted([r1.status_code, r2.status_code])
        assert statuses == [200, 409]
        err_resp = r1 if r1.status_code == 409 else r2
        assert err_resp.json()["error"] == "probe_in_progress"
        assert backend.start_invocations == 1
        assert len(list(staging_root.iterdir())) == 1

        r_del = await send_asgi_request(app, "DELETE", "/gpustack/v1/probe")
        assert r_del.status_code == 200
        assert list(staging_root.iterdir()) == []

    async def _run_race_cooldown() -> None:
        staging_root = tmp_path / "race_cooldown"
        backend = _YieldingProbeBackend(complete_immediately=True)
        latch = BootLatch(require_ack=False)
        mgr = ProbeManager(
            backend=backend,
            latch=latch,
            staging_root=staging_root,
            cooldown_seconds=60.0,
            sweep_on_init=False,
        )
        app = FakeFastAPIApp()
        plane = SentinelControlPlane(backend=backend, latch=latch, probe_manager=mgr)
        ensure_outermost_sentinel_middleware(app, control_plane=plane)

        body = {"expected_boot_id": latch.boot_id, "steps": 3}
        r1, r2 = await asyncio.gather(
            send_asgi_request(app, "POST", "/gpustack/v1/probe", json_body=body),
            send_asgi_request(app, "POST", "/gpustack/v1/probe", json_body=body),
        )
        statuses = sorted([r1.status_code, r2.status_code])
        assert statuses == [200, 429]
        err_resp = r1 if r1.status_code == 429 else r2
        assert err_resp.json()["error"] == "probe_cooldown"
        assert backend.start_invocations == 1
        assert list(staging_root.iterdir()) == []

    run_sync(_run_race_in_progress())
    run_sync(_run_race_cooldown())


class _MiniTensor:
    """Pure-Python tensor double covering PyTorch ops used in _OP_REFERENCES."""

    def __init__(self, data: Any, dtype: str = "float32") -> None:
        self._data = data
        self.dtype = dtype
        self.device = "cpu"

    @property
    def shape(self) -> tuple[int, ...]:
        dims: list[int] = []
        cur = self._data
        while isinstance(cur, list):
            dims.append(len(cur))
            cur = cur[0] if cur else None
        return tuple(dims)

    @property
    def ndim(self) -> int:
        return len(self.shape)

    def _map(self, fn: Any, data: Any | None = None) -> Any:
        cur = self._data if data is None else data
        if isinstance(cur, list):
            return [self._map(fn, item) for item in cur]
        return fn(cur)

    def _binop(self, other: Any, op: Any) -> _MiniTensor:
        def _apply(a: Any, b: Any) -> Any:
            if isinstance(a, list) and isinstance(b, list):
                if len(a) == len(b):
                    return [_apply(x, y) for x, y in zip(a, b, strict=True)]
                if len(a) == 1:
                    return [_apply(a[0], y) for y in b]
                if len(b) == 1:
                    return [_apply(x, b[0]) for x in a]
                raise ValueError(f"Broadcast mismatch {len(a)} vs {len(b)}")
            if isinstance(a, list):
                return [_apply(x, b) for x in a]
            if isinstance(b, list):
                return [_apply(a, y) for y in b]
            return op(a, b)

        b_data = other._data if isinstance(other, _MiniTensor) else other
        a_data = self._data
        a_ndim = self.ndim
        b_ndim = other.ndim if isinstance(other, _MiniTensor) else 0
        while a_ndim < b_ndim:
            a_data = [a_data]
            a_ndim += 1
        while b_ndim < a_ndim:
            b_data = [b_data]
            b_ndim += 1
        return _MiniTensor(_apply(a_data, b_data), self.dtype)

    def __add__(self, other: Any) -> _MiniTensor:
        return self._binop(other, lambda a, b: a + b)

    def __sub__(self, other: Any) -> _MiniTensor:
        return self._binop(other, lambda a, b: a - b)

    def __mul__(self, other: Any) -> _MiniTensor:
        return self._binop(other, lambda a, b: a * b)

    def __truediv__(self, other: Any) -> _MiniTensor:
        return self._binop(other, lambda a, b: a / b)

    def __neg__(self) -> _MiniTensor:
        return _MiniTensor(self._map(lambda v: -v), self.dtype)

    def __le__(self, other: Any) -> _MiniTensor:
        return self._binop(other, lambda a, b: bool(a <= b))

    def __getitem__(self, key: Any) -> _MiniTensor:
        if not isinstance(key, tuple):
            key = (key,)
        if key and key[0] is Ellipsis:
            rest = key[1:]
            key = (slice(None),) * (self.ndim - len(rest)) + rest

        def _slice(data: Any, idx_tuple: tuple[Any, ...]) -> Any:
            if not idx_tuple:
                return data
            head, *tail = idx_tuple
            if isinstance(head, slice):
                return [_slice(item, tuple(tail)) for item in data[head]]
            return _slice(data[head], tuple(tail))

        return _MiniTensor(_slice(self._data, key), self.dtype)

    def pow(self, exp: float) -> _MiniTensor:
        return _MiniTensor(self._map(lambda v: v**exp), self.dtype)

    def mean(self, dim: int = -1, keepdim: bool = False) -> _MiniTensor:
        assert dim == -1

        def _reduce(data: Any) -> Any:
            if data and not isinstance(data[0], list):
                val = sum(data) / max(len(data), 1)
                return [val] if keepdim else val
            return [_reduce(sub) for sub in data]

        return _MiniTensor(_reduce(self._data), self.dtype)

    def sum(self, dim: int = -1, keepdim: bool = False) -> _MiniTensor:
        assert dim == -1

        def _reduce(data: Any) -> Any:
            if data and not isinstance(data[0], list):
                val = sum(data)
                return [val] if keepdim else val
            return [_reduce(sub) for sub in data]

        return _MiniTensor(_reduce(self._data), self.dtype)

    def clamp_min(self, floor: float) -> _MiniTensor:
        return _MiniTensor(self._map(lambda v: max(float(v), floor)), self.dtype)

    def to(self, dtype: str) -> _MiniTensor:
        if dtype in ("float32", "float16", "bfloat16", "float64"):
            return _MiniTensor(self._map(float), dtype)
        return _MiniTensor(self._data, dtype)

    def unsqueeze(self, dim: int) -> _MiniTensor:
        real_dim = dim if dim >= 0 else self.ndim + 1 + dim

        def _ins(data: Any, cur_dim: int) -> Any:
            if cur_dim == real_dim:
                return [data]
            return [_ins(sub, cur_dim + 1) for sub in data]

        return _MiniTensor(_ins(self._data, 0), self.dtype)

    def transpose(self, dim0: int, dim1: int) -> _MiniTensor:
        d0 = dim0 if dim0 >= 0 else self.ndim + dim0
        d1 = dim1 if dim1 >= 0 else self.ndim + dim1
        if d0 == d1:
            return _MiniTensor(self._data, self.dtype)
        if self.ndim == 2:
            rows = len(self._data)
            cols = len(self._data[0])
            return _MiniTensor(
                [[self._data[r][c] for r in range(rows)] for c in range(cols)],
                self.dtype,
            )
        if self.ndim == 3 and {d0, d1} == {0, 1}:
            d_0, d_1 = len(self._data), len(self._data[0])
            return _MiniTensor(
                [[self._data[i][j] for i in range(d_0)] for j in range(d_1)],
                self.dtype,
            )
        if self.ndim == 3 and {d0, d1} == {1, 2}:
            return _MiniTensor(
                [
                    [[mat[r][c] for r in range(len(mat))] for c in range(len(mat[0]))]
                    for mat in self._data
                ],
                self.dtype,
            )
        raise NotImplementedError(f"transpose({dim0}, {dim1}) for {self.shape}")

    def masked_fill(self, mask: _MiniTensor, value: float) -> _MiniTensor:
        return self._binop(mask, lambda a, m: value if m else a)

    def index_select(self, dim: int, index: _MiniTensor) -> _MiniTensor:
        assert dim == 0
        return _MiniTensor([self._data[int(i)] for i in index._data], self.dtype)

    def flatten_vals(self) -> list[float]:
        out: list[float] = []

        def _walk(cur: Any) -> None:
            if isinstance(cur, list):
                for item in cur:
                    _walk(item)
            else:
                out.append(float(cur))

        _walk(self._data)
        return out


def _make_mini_filled(shape: list[int], start: float = 0.1) -> _MiniTensor:
    counter = [0]

    def _build(dims: list[int]) -> Any:
        if not dims:
            counter[0] += 1
            return start + (counter[0] % 7) * 0.13
        return [_build(dims[1:]) for _ in range(dims[0])]

    return _MiniTensor(_build(shape))


def _mini_softmax_rows(data: Any) -> Any:
    import math

    if data and not isinstance(data[0], list):
        m = max(data)
        exps = [math.exp(v - m) if math.isfinite(v) else 0.0 for v in data]
        s = sum(exps) or 1.0
        return [e / s for e in exps]
    return [_mini_softmax_rows(sub) for sub in data]


def _mini_matmul(a: _MiniTensor, b: _MiniTensor) -> _MiniTensor:
    def _mm2d(m1: list[list[float]], m2: list[list[float]]) -> list[list[float]]:
        k_len = len(m2)
        n_len = len(m2[0])
        return [
            [sum(row[k] * m2[k][j] for k in range(k_len)) for j in range(n_len)]
            for row in m1
        ]

    if a.ndim == 2 and b.ndim == 2:
        return _MiniTensor(_mm2d(a._data, b._data))
    if a.ndim == 3 and b.ndim == 3:
        return _MiniTensor(
            [_mm2d(am, bm) for am, bm in zip(a._data, b._data, strict=True)]
        )
    raise NotImplementedError(f"matmul {a.shape} @ {b.shape}")


def _mini_cat(tensors: tuple[_MiniTensor, ...], dim: int = -1) -> _MiniTensor:
    assert dim == -1

    def _cat_last(items: list[Any]) -> Any:
        if items[0] and not isinstance(items[0][0], list):
            out: list[Any] = []
            for it in items:
                out.extend(it)
            return out
        return [_cat_last(list(group)) for group in zip(*items, strict=True)]

    return _MiniTensor(_cat_last([t._data for t in tensors]))


def _mini_topk(
    t: _MiniTensor, k: int, dim: int = -1
) -> tuple[_MiniTensor, _MiniTensor]:
    assert dim == -1

    def _tk(data: Any) -> tuple[Any, Any]:
        if data and not isinstance(data[0], list):
            ranked = sorted(enumerate(data), key=lambda p: p[1], reverse=True)[:k]
            return [v for _, v in ranked], [i for i, _ in ranked]
        sub = [_tk(item) for item in data]
        return [s[0] for s in sub], [s[1] for s in sub]

    vals, idxs = _tk(t._data)
    return _MiniTensor(vals), _MiniTensor(idxs, dtype="int64")


def _mini_cumsum(t: _MiniTensor, dim: int = -1) -> _MiniTensor:
    assert dim == -1

    def _cs(data: Any) -> Any:
        if data and not isinstance(data[0], list):
            acc = 0.0
            out = []
            for v in data:
                acc += v
                out.append(acc)
            return out
        return [_cs(item) for item in data]

    return _MiniTensor(_cs(t._data))


def _mini_clamp(t: _MiniTensor, **kwargs: Any) -> _MiniTensor:
    lo = kwargs.get("min")
    hi = kwargs.get("max")

    def _clip(v: float) -> float:
        res = lo if lo is not None and v < lo else v
        return hi if hi is not None and res > hi else res

    return _MiniTensor(t._map(_clip), t.dtype)


def _mini_layer_norm(
    x: _MiniTensor,
    normalized_shape: tuple[int, ...],
    weight: _MiniTensor | None = None,
    bias: _MiniTensor | None = None,
    eps: float = 1e-5,
) -> _MiniTensor:
    import math

    del normalized_shape
    mean = x.mean(-1, keepdim=True)
    diff = x - mean
    var = diff.pow(2).mean(-1, keepdim=True)
    normed = diff * _MiniTensor(var._map(lambda v: 1.0 / math.sqrt(v + eps)))
    if weight is not None:
        normed = normed * weight
    if bias is not None:
        normed = normed + bias
    return normed


def _build_mini_torch() -> Any:
    import math
    from types import SimpleNamespace

    return SimpleNamespace(
        long="int64",
        bool="bool",
        rsqrt=lambda t: _MiniTensor(t._map(lambda v: 1.0 / math.sqrt(v))),
        softmax=lambda t, dim=-1: _MiniTensor(_mini_softmax_rows(t._data)),
        topk=_mini_topk,
        cumsum=_mini_cumsum,
        cat=_mini_cat,
        matmul=_mini_matmul,
        clamp=_mini_clamp,
        ones=lambda shape, device=None, dtype="float32": _MiniTensor(
            (
                [
                    [True if dtype == "bool" else 1.0 for _ in range(shape[1])]
                    for _ in range(shape[0])
                ]
                if len(shape) == 2
                else [1.0 for _ in range(shape[0])]
            ),
            dtype=dtype,
        ),
        triu=lambda t, diagonal=0: _MiniTensor(
            [
                [
                    (t._data[r][c] if c - r >= diagonal else False)
                    for c in range(len(t._data[0]))
                ]
                for r in range(len(t._data))
            ],
            dtype=t.dtype,
        ),
        arange=lambda start, end=None, step=1, device=None, dtype="int64": _MiniTensor(
            list(range(start, end, step)) if end is not None else list(range(start)),
            dtype=dtype,
        ),
        nn=SimpleNamespace(
            functional=SimpleNamespace(
                layer_norm=_mini_layer_norm,
                silu=lambda t: _MiniTensor(t._map(lambda v: v / (1.0 + math.exp(-v)))),
            )
        ),
    )


def test_all_12_op_references_parse_and_execute_against_extracted_shapes() -> None:
    import ast
    import inspect
    import math

    from gpustack_engine_sentinel.probe import (
        _DYNAMIC_VAR_AXES,
        _OP_REFERENCES,
        _extract_shapes_and_axes,
    )

    mini_torch = _build_mini_torch()
    assert set(_OP_REFERENCES.keys()) == ALLOWED_OP_FAMILIES
    sample_dims: dict[str, list[list[int]]] = {
        "rmsnorm": [[3, 8]],
        "layernorm": [[3, 8]],
        "rope_kv_append": [[3, 2, 8]],
        "activation_quant": [[3, 8]],
        "moe_router_topk": [[3, 12]],
        "moe_permute_unpermute": [[3, 8]],
        "paged_attention": [[3, 2, 8]],
        "ragged_prefill_attention": [[3, 2, 8]],
        "mla_local": [[3, 2, 8]],
        "speculative_verify": [[3, 6]],
        "sampling_logits": [[3, 64]],
        "custom_elementwise_fusion": [[3, 8]],
    }

    for family in sorted(ALLOWED_OP_FAMILIES):
        ref_code = _OP_REFERENCES[family]
        ast.parse(ref_code)
        ns: dict[str, Any] = {"torch": mini_torch}
        exec(ref_code, ns)
        run_fn = ns["run"]
        assert callable(run_fn), f"{family} reference missing run()"

        const_axes, var_axes, in_specs, out_specs = _extract_shapes_and_axes(
            family, sample_dims[family]
        )
        sig = inspect.signature(run_fn)
        required_params = [
            p
            for p in sig.parameters.values()
            if p.default is inspect.Parameter.empty
            and p.kind
            in (
                inspect.Parameter.POSITIONAL_ONLY,
                inspect.Parameter.POSITIONAL_OR_KEYWORD,
            )
        ]
        all_pos_params = [
            p
            for p in sig.parameters.values()
            if p.kind
            in (
                inspect.Parameter.POSITIONAL_ONLY,
                inspect.Parameter.POSITIONAL_OR_KEYWORD,
            )
        ]
        assert len(required_params) <= len(in_specs) <= len(all_pos_params), (
            f"{family}: in_specs={list(in_specs)} does not match run{sig}"
        )

        axis_vals = {**const_axes, **var_axes}
        tensors = [
            _make_mini_filled([int(axis_vals[d]) for d in dims_ref])
            for dims_ref in in_specs.values()
        ]
        out = run_fn(*tensors)
        outputs = out if isinstance(out, tuple) else (out,)
        assert len(outputs) == len(out_specs), (
            f"{family}: got {len(outputs)} outputs, expected {len(out_specs)}"
        )
        for tensor_out, dims_ref in zip(outputs, out_specs.values(), strict=True):
            expected_shape = tuple(int(axis_vals[d]) for d in dims_ref)
            assert tensor_out.shape == expected_shape, (
                f"{family}: output shape {tensor_out.shape} != {expected_shape}"
            )
            vals = tensor_out.flatten_vals()
            assert vals and all(math.isfinite(v) for v in vals)

    expected_go_dynamic_axes = {
        "batch",
        "batch_size",
        "bs",
        "context_len",
        "kv_len",
        "max_num_tokens",
        "max_seq_len",
        "num_decode_tokens",
        "num_prefill_tokens",
        "num_requests",
        "num_seqs",
        "num_tokens",
        "q_len",
        "seq_len",
        "seq_length",
        "sequence_length",
        "token_count",
        "total_tokens",
    }
    assert set(_DYNAMIC_VAR_AXES) == expected_go_dynamic_axes
    assert "prompt_len" not in _DYNAMIC_VAR_AXES


def test_distillation_schema_alignment_gpu_share_denominator_and_arch_norm() -> None:
    events = [
        {
            "name": "vllm::silu_and_mul",
            "dur": 20.0,
            "input_dims": [[4, 16]],
            "input_dtypes": ["bfloat16"],
        },
        {
            "name": "vllm::topk_softmax",
            "dur": 30.0,
            "input_dims": [[4, 16]],
            "input_dtypes": ["bfloat16"],
        },
        {
            "name": "flashinfer::top_k_top_p_sampling_from_probs",
            "dur": 50.0,
            "input_dims": [[2, 128]],
            "input_dtypes": ["bfloat16"],
        },
        {
            "name": "cublasLtMatmul",
            "dur": 800.0,
            "input_dims": [[4, 4096], [4096, 4096]],
        },
        {
            "name": "ncclAllReduce",
            "dur": 50.0,
            "input_dims": [[4, 4096]],
        },
        {
            "name": "custom_unclassified_kernel",
            "dur": 50.0,
            "input_dims": [[4, 16]],
        },
    ]
    payload = distill_trace_events(
        events,
        engine="vLLM",
        engine_version="0.8.5",
        manufacturer="NVIDIA",
        architecture="9.0a",
        steps_captured=4,
    )
    assert payload["architecture"] == "sm_90a"
    defs_by_op = {d["op_type"]: d for d in payload["definitions"]}

    act_def = defs_by_op["activation_quant"]
    assert act_def["axes"]["half_hidden_size"] == {"type": "const", "value": 8}
    assert act_def["outputs"] == {
        "out": {"shape": ["num_tokens", "half_hidden_size"], "dtype": "bfloat16"}
    }

    moe_def = defs_by_op["moe_router_topk"]
    assert moe_def["axes"]["top_k"] == {"type": "const", "value": 8}
    assert moe_def["outputs"] == {
        "values": {"shape": ["num_tokens", "top_k"], "dtype": "bfloat16"},
        "indices": {"shape": ["num_tokens", "top_k"], "dtype": "int64"},
    }

    samp_def = defs_by_op["sampling_logits"]
    assert samp_def["axes"]["top_k"] == {"type": "const", "value": 50}
    assert samp_def["outputs"] == {
        "probs": {"shape": ["batch_size", "top_k"], "dtype": "bfloat16"},
        "indices": {"shape": ["batch_size", "top_k"], "dtype": "int64"},
    }

    # Total GPU duration across all normalized events is 20+30+50+800+50+50 = 1000 us.
    shares = {
        b["op_family"]: b["gpu_time_share_percent"] for b in payload["bottlenecks"]
    }
    assert shares["sampling_logits"] == pytest.approx(5.0)
    assert shares["moe_router_topk"] == pytest.approx(3.0)
    assert shares["activation_quant"] == pytest.approx(2.0)

    # format_local_kernel_key normalizes arch across vendors consistently with Go.
    for mfr, raw_arch, want_arch in (
        ("nvidia", "9.0a", "sm_90a"),
        ("nvidia", "compute_90a", "sm_90a"),
        ("amd", "gfx942:sramecc+:xnack-", "gfx942"),
        ("ascend", "ascend-910b", "910b"),
    ):
        k = format_local_kernel_key(
            engine="vllm",
            engine_version="0.8.5",
            manufacturer=mfr,
            arch=raw_arch,
            op_family="rmsnorm",
            const_axes={"hidden_size": 4096},
            input_dtypes=["bfloat16"],
            output_dtypes=["bfloat16"],
        )
        assert parse_local_kernel_key(k)["architecture"] == want_arch


def test_probe_budget_eviction_order_hotspot_cap_and_envelope_ceiling(
    tmp_path: Path,
) -> None:
    from gpustack_engine_sentinel.protocol import PROBE_MAX_PAYLOAD_BYTES

    long_kernel = "void cutlass::Kernel<" + ("TemplateArg," * 40) + ">()"
    assert len(long_kernel) > 256

    # Build 24 distinct definitions, each with 4 workloads, plus CUDA graph hotspots.
    events: list[dict[str, Any]] = []
    for def_idx in range(24):
        hidden = 1024 + def_idx * 64
        base_dur = float((24 - def_idx) * 100)
        for wl_idx in range(4):
            events.append(
                {
                    "name": "vllm::rms_norm",
                    "dur": base_dur + float(wl_idx + 1),
                    "input_dims": [[(wl_idx + 1) * 8, hidden], [hidden]],
                    "input_dtypes": ["bfloat16", "bfloat16"],
                }
            )
    for hs_idx in range(10):
        events.append(
            {
                "name": f"{long_kernel}_{hs_idx}",
                "cat": "cuda_graph",
                "dur": float(50 - hs_idx),
            }
        )

    full = distill_trace_events(
        events,
        engine="vllm",
        engine_version="0.8.5",
        manufacturer="nvidia",
        architecture="sm_90a",
        steps_captured=4,
        max_payload_bytes=PROBE_MAX_PAYLOAD_BYTES,
    )
    assert all(
        len(h["kernel_name"]) <= 256 for h in full["diagnostics"]["cuda_graph_hotspots"]
    )

    # Budget tight enough to drop tail definitions > 16 and trim excess workloads,
    # while keeping >= 1 workload for every retained definition!
    tight_budget = 14000
    capped = distill_trace_events(
        events,
        engine="vllm",
        engine_version="0.8.5",
        manufacturer="nvidia",
        architecture="sm_90a",
        steps_captured=4,
        max_payload_bytes=tight_budget,
    )
    assert capped["diagnostics"]["truncated"] is True
    assert (
        len(json.dumps(capped, separators=(",", ":")).encode("utf-8")) <= tight_budget
    )
    assert 1 <= len(capped["definitions"]) <= 16
    wl_defs = {w["definition"] for w in capped["workloads"]}
    for d in capped["definitions"]:
        assert d["name"] in wl_defs

    # Even tighter budget where only 1 definition fits: hotspots trim to [] BEFORE the last workload is lost!
    very_tight = 1600
    min_capped = distill_trace_events(
        events,
        engine="vllm",
        engine_version="0.8.5",
        manufacturer="nvidia",
        architecture="sm_90a",
        steps_captured=4,
        max_payload_bytes=very_tight,
    )
    assert len(min_capped["definitions"]) == 1
    assert len(min_capped["workloads"]) == 1
    assert min_capped["diagnostics"]["cuda_graph_hotspots"] == []
    assert (
        len(json.dumps(min_capped, separators=(",", ":")).encode("utf-8")) <= very_tight
    )

    # Full ProbeManager GET /gpustack/v1/probe envelope omits duplicate top-level diagnostics
    # when payload is present and stays strictly <= PROBE_MAX_PAYLOAD_BYTES (524,288 bytes).
    huge_events: list[dict[str, Any]] = []
    for def_idx in range(16):
        hidden = 2048 + def_idx * 128
        for wl_idx in range(250):
            huge_events.append(
                {
                    "name": "vllm::rms_norm",
                    "dur": float(1000 - def_idx * 10 + (wl_idx % 5)),
                    "input_dims": [[wl_idx + 1, hidden], [hidden]],
                    "input_dtypes": ["bfloat16", "bfloat16"],
                }
            )
    for hs_idx in range(32):
        huge_events.append(
            {
                "name": f"{hs_idx:02d}_{long_kernel}",
                "cat": "cuda_graph",
                "dur": float(100 - hs_idx),
            }
        )
    near_cap_payload = distill_trace_events(
        huge_events,
        engine="vllm",
        engine_version="0.8.5",
        manufacturer="nvidia",
        architecture="sm_90a",
        steps_captured=4,
        max_payload_bytes=PROBE_MAX_PAYLOAD_BYTES,
    )

    class _NearCapBackend(_MockProbeBackend):
        async def start_probe(self, spec: dict[str, Any]) -> dict[str, Any]:
            del spec
            return {"status": "completed", "payload": near_cap_payload}

    latch = BootLatch(require_ack=False)
    mgr = ProbeManager(
        backend=_NearCapBackend(complete_immediately=True),
        latch=latch,
        staging_root=tmp_path / "env_cap",
        cooldown_seconds=0.0,
        sweep_on_init=False,
    )
    _, idle_env = run_sync(mgr.get_probe())
    assert "diagnostics" in idle_env
    _, completed_env = run_sync(
        mgr.start_probe({"expected_boot_id": latch.boot_id, "steps": 4})
    )
    assert completed_env["state"] == "completed"
    assert "payload" in completed_env
    assert "diagnostics" not in completed_env
    assert "diagnostics" in completed_env["payload"]
    serialized_env = json.dumps(completed_env, separators=(",", ":")).encode("utf-8")
    assert len(serialized_env) <= PROBE_MAX_PAYLOAD_BYTES


def test_cuda_graph_kernels_distill_with_model_dims_fallback() -> None:
    """CUDA-graph-captured GPU kernels lacking aten input_dims distill via engine model_dims."""
    events = [
        {
            "name": "cudaGraphLaunch",
            "dur": 1887.8,
            "input_dims": [],
            "count": 4,
        },
        {
            "name": "triton_red_fused_fused_add_rms_norm_2",
            "dur": 314.7,
            "input_dims": [],
            "count": 144,
        },
        {
            "name": "triton_poi_fused_mul_silu_slice_1",
            "dur": 202.2,
            "input_dims": [],
            "count": 144,
        },
        {
            "name": "void vllm::reshape_and_cache_flash_kernel<__nv_bfloat16>()",
            "dur": 320.4,
            "input_dims": [],
            "count": 144,
        },
    ]
    # Without model_dims, empty-shape CUDA-graph kernels remain hotspots only.
    no_dims = distill_trace_events(
        events,
        engine="vLLM",
        engine_version="0.32.0",
        manufacturer="nvidia",
        architecture="sm_90",
        steps_captured=4,
    )
    assert no_dims["bottlenecks"] == []
    assert len(no_dims["diagnostics"]["cuda_graph_hotspots"]) == 4

    # With engine model_dims, all three non-GEMM kernel families distill while preserving hotspots.
    with_dims = distill_trace_events(
        events,
        engine="vLLM",
        engine_version="0.32.0",
        manufacturer="nvidia",
        architecture="sm_90",
        steps_captured=4,
        model_dims={
            "num_tokens": 8,
            "hidden_size": 4096,
            "intermediate_size_local": 12288,
            "num_heads_local": 32,
            "head_dim": 128,
            "dtype": "torch.bfloat16",
        },
    )
    assert len(with_dims["diagnostics"]["cuda_graph_hotspots"]) == 4
    by_op = {d["op_type"]: d for d in with_dims["definitions"]}
    assert set(by_op.keys()) == {"rmsnorm", "activation_quant", "rope_kv_append"}
    assert by_op["rmsnorm"]["axes"]["hidden_size"] == {"type": "const", "value": 4096}
    assert by_op["activation_quant"]["axes"]["hidden_size"] == {
        "type": "const",
        "value": 24576,
    }
    assert by_op["activation_quant"]["axes"]["half_hidden_size"] == {
        "type": "const",
        "value": 12288,
    }
    assert by_op["rope_kv_append"]["axes"]["num_heads_local"] == {
        "type": "const",
        "value": 32,
    }
    assert by_op["rope_kv_append"]["axes"]["head_dim"] == {
        "type": "const",
        "value": 128,
    }
    assert all(w["axes"]["num_tokens"] == 8 for w in with_dims["workloads"])
    assert [b["op_family"] for b in with_dims["bottlenecks"]] == [
        "rope_kv_append",
        "rmsnorm",
        "activation_quant",
    ]
