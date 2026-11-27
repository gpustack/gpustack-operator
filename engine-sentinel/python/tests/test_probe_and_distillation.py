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
        "c10d::allreduce_",
    ],
)
def test_gemm_and_collective_operators_strictly_excluded(op_name: str) -> None:
    assert is_excluded_operator(op_name) is True
    assert classify_op_family(op_name) is None


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
    assert normalize_engine_version("v0.8.5+cu128") == "0.8.5"
    assert normalize_engine_version("0.4.6.post1+rocm6.3") == "0.4.6.post1"
    assert normalize_engine_version("0.9.0.dev123+gabcdef1") == "0.9.0.dev123"
    assert normalize_engine_version("0.9.0-gabcdef1-dirty") == "0.9.0"
    assert normalize_engine_version("0.34") == "0.34.0"

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
    assert key == f"vllm/0.8.5/nvidia/sm_90a/rmsnorm/{expected_sig}"
    parsed = parse_local_kernel_key(key)
    assert parsed == {
        "engine": "vllm",
        "engine_version": "0.8.5",
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
    assert normalize_engine_version("") == "0.0.0"

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
