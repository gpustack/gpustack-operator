"""Tests for vLLM rank-0 collective_rpc probe, SGLang in-process probe, and lazy startup kernel loader."""

from __future__ import annotations

import asyncio
import gzip
import json
import os
import subprocess
import sys
from pathlib import Path
from types import SimpleNamespace
from typing import Any

import pytest
from asgi_harness import FakeFastAPIApp, run_sync, send_asgi_request

from gpustack_engine_sentinel.kernel_loader import (
    UNSUPPORTED_KERNEL_ARTIFACT_ERROR,
    aggregate_kernel_reports,
    load_kernel_dispatch_manifest,
    reset_process_kernel_registry,
)
from gpustack_engine_sentinel.latch import BootLatch
from gpustack_engine_sentinel.probe import ProbeManager, format_local_kernel_key
from gpustack_engine_sentinel.protocol import (
    DISPATCH_MANIFEST_SCHEMA_VERSION,
    ENV_KERNEL_ARTIFACT_DIR,
    ENV_PROBE_MIN_COOLDOWN_SECONDS,
    ENV_PROBE_SHM_DIR,
)
from gpustack_engine_sentinel.server import (
    SentinelControlPlane,
    ensure_outermost_sentinel_middleware,
)
from gpustack_engine_sentinel.sglang.hooks import (
    SGLangSentinelBackend,
    patch_sglang_internal_state_module,
)
from gpustack_engine_sentinel.vllm.hooks import VLLMSentinelBackend
from gpustack_engine_sentinel.vllm.utility import attach_engine_core_utility

REPO_PYTHON_ROOT = Path(__file__).resolve().parents[1]


class _FakeTorchProfiler:
    """Pure-Python profiler double mimicking torch.profiler.profile without importing torch."""

    def __init__(self, events: list[Any]) -> None:
        self._events = events
        self.entered = False
        self.exited = False
        self.steps = 0

    def __enter__(self) -> _FakeTorchProfiler:
        self.entered = True
        return self

    def __exit__(self, exc_type: Any, exc: Any, tb: Any) -> None:
        del exc_type, exc, tb
        self.exited = True

    def step(self) -> None:
        self.steps += 1

    def key_averages(self, group_by_input_shape: bool = False) -> list[Any]:
        del group_by_input_shape
        return list(self._events)


class _FakeGPUWorker:
    def __init__(self, events: list[Any], *, rank: int = 0) -> None:
        self.rank = rank
        self.exec_calls = 0
        self.profiler = _FakeTorchProfiler(events)
        self._gpustack_profiler_factory = lambda: self.profiler

    def execute_model(self, batch: Any = None) -> dict[str, Any]:
        self.exec_calls += 1
        return {"step": self.exec_calls, "batch": batch}


class _FakeModelExecutor:
    def __init__(
        self, rank0_worker: _FakeGPUWorker, *other_workers: _FakeGPUWorker
    ) -> None:
        self.rank0_worker = rank0_worker
        self.workers = [rank0_worker, *other_workers]
        self.collective_calls: list[dict[str, Any]] = []

    def collective_rpc(
        self,
        fn: Any,
        args: tuple[Any, ...] = (),
        unique_reply_rank: int | None = None,
    ) -> Any:
        self.collective_calls.append(
            {
                "fn": getattr(fn, "__name__", str(fn)),
                "unique_reply_rank": unique_reply_rank,
            }
        )
        # Mirror upstream vLLM multiproc_executor: broadcast to all workers, return unique_reply_rank reply.
        results = [fn(w, *args) for w in self.workers]
        if unique_reply_rank is not None:
            return results[unique_reply_rank]
        return results


class _FakeVLLMEngineCore:
    def __init__(
        self,
        rank: int,
        *,
        model_executor: Any = None,
        tp_size: int = 2,
        dp_size: int = 2,
    ) -> None:
        self.vllm_config = SimpleNamespace(
            parallel_config=SimpleNamespace(
                data_parallel_rank=rank,
                data_parallel_index=rank,
                data_parallel_size=dp_size,
                tensor_parallel_size=tp_size,
            ),
            model_config=SimpleNamespace(max_model_len=8192),
            scheduler_config=SimpleNamespace(
                max_num_seqs=64,
                max_num_batched_tokens=4096,
                long_prefill_token_threshold=0,
                enable_chunked_prefill=True,
                policy="fcfs",
                cache_aware_admission_window=0,
                cache_aware_admission_threshold=0.5,
            ),
        )
        self.scheduler = SimpleNamespace(
            scheduler_config=self.vllm_config.scheduler_config,
            cache_config=SimpleNamespace(enable_prefix_caching=True),
            max_num_running_reqs=64,
            max_num_active_reqs=64,
            max_num_scheduled_tokens=4096,
            is_encoder_decoder=False,
            policy="fcfs",
            cache_aware_window=0,
            cache_aware_threshold=0.5,
            connector=None,
            _kv_fetch_stages=None,
        )
        self.model_executor = model_executor


class _FakeMPClient:
    def __init__(self, cores: list[_FakeVLLMEngineCore]) -> None:
        self.cores = cores
        self.core_engines = [i.to_bytes(2, "little") for i in range(len(cores))]
        self.resources = SimpleNamespace(engine_dead=False)

    async def call_utility_all_async(self, method: str, *args: Any) -> list[Any]:
        return [getattr(c, method)(*args) for c in self.cores]


def test_vllm_rank0_collective_rpc_probe_autostops_and_distills(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv(ENV_PROBE_MIN_COOLDOWN_SECONDS, "0")
    attach_engine_core_utility(_FakeVLLMEngineCore)

    events = [
        SimpleNamespace(
            key="vllm::rms_norm",
            self_device_time_total=240.0,
            input_shapes=[[64, 4096], [4096]],
            input_dtypes=["bfloat16", "bfloat16"],
            count=3,
        ),
        SimpleNamespace(
            key="vllm::rotary_embedding",
            self_device_time_total=110.0,
            input_shapes=[[64, 8, 128]],
            input_dtypes=["bfloat16"],
            count=3,
        ),
        SimpleNamespace(
            key="cublasLtMatmul",
            self_device_time_total=950.0,
            input_shapes=[[64, 4096], [4096, 4096]],
            input_dtypes=["bfloat16", "bfloat16"],
            count=3,
        ),
        SimpleNamespace(
            key="ncclKernel_AllReduce",
            self_device_time_total=300.0,
            input_shapes=[[64, 4096]],
            input_dtypes=["bfloat16"],
            count=3,
        ),
    ]
    worker0 = _FakeGPUWorker(events, rank=0)
    worker1 = _FakeGPUWorker([], rank=1)
    exec0 = _FakeModelExecutor(worker0, worker1)
    core0 = _FakeVLLMEngineCore(0, model_executor=exec0, tp_size=2, dp_size=2)
    core1 = _FakeVLLMEngineCore(1, model_executor=None, tp_size=2, dp_size=2)
    ec = SimpleNamespace(engine_core=_FakeMPClient([core0, core1]), errored=False)

    backend = VLLMSentinelBackend()
    backend.bind(ec, SimpleNamespace(), SimpleNamespace())
    latch = BootLatch(require_ack=False)
    mgr = ProbeManager(
        backend=backend,
        latch=latch,
        staging_root=tmp_path,
        cooldown_seconds=0.0,
        sweep_on_init=False,
    )
    app = FakeFastAPIApp()
    plane = SentinelControlPlane(backend=backend, latch=latch, probe_manager=mgr)
    plane.install_routes(app)
    ensure_outermost_sentinel_middleware(app, control_plane=plane)

    async def _run() -> None:
        r_start = await send_asgi_request(
            app,
            "POST",
            "/gpustack/v1/probe",
            json_body={
                "expected_boot_id": latch.boot_id,
                "steps": 3,
                "engine_version": "0.8.5+cu128",
                "manufacturer": "nvidia",
                "architecture": "sm_90a",
            },
        )
        assert r_start.status_code == 200, r_start.body
        probe_id = r_start.json()["probe_id"]
        assert exec0.collective_calls[0]["unique_reply_rank"] == 0

        # Run 4 model steps on both TP workers: only rank 0 profiles (3 steps); rank 1 stays untouched!
        for i in range(4):
            worker0.execute_model(batch=i)
            worker1.execute_model(batch=i)

        assert worker0.profiler.entered is True
        assert worker0.profiler.exited is True
        assert worker0.profiler.steps == 3
        assert worker0.exec_calls == 4
        assert worker1.profiler.entered is False
        assert worker1.profiler.steps == 0

        await asyncio.sleep(0.05)
        r_get = await send_asgi_request(app, "GET", "/gpustack/v1/probe")
        assert r_get.status_code == 200
        body = r_get.json()
        assert body["state"] == "completed"
        payload = body["payload"]
        assert payload["engine"] == "vllm"
        assert payload["engine_version"] == "0.8"
        assert payload["diagnostics"]["steps_captured"] == 3
        assert payload["diagnostics"]["filtered_excluded_count"] == 2
        op_families = [d["op_type"] for d in payload["definitions"]]
        assert op_families == ["rmsnorm", "rope_kv_append"]
        assert not (tmp_path / probe_id).exists()

    run_sync(_run())


def test_vllm_tp1_inprocess_and_unsupported_engine_version(tmp_path: Path) -> None:
    attach_engine_core_utility(_FakeVLLMEngineCore)

    # 1. TP=1 in-process direct worker (no collective_rpc).
    worker0 = _FakeGPUWorker(
        [
            {
                "name": "vllm::rms_norm",
                "dur": 80.0,
                "input_dims": [[32, 2048], [2048]],
                "input_dtypes": ["float16", "float16"],
            }
        ]
    )
    tp1_executor = SimpleNamespace(driver_worker=worker0)
    core_tp1 = _FakeVLLMEngineCore(0, model_executor=tp1_executor, tp_size=1, dp_size=1)
    ec_tp1 = SimpleNamespace(engine_core=_FakeMPClient([core_tp1]), errored=False)

    backend_tp1 = VLLMSentinelBackend()
    backend_tp1.bind(ec_tp1, SimpleNamespace(), SimpleNamespace())
    latch_tp1 = BootLatch(require_ack=False)
    app_tp1 = FakeFastAPIApp()
    plane_tp1 = SentinelControlPlane(
        backend=backend_tp1,
        latch=latch_tp1,
        probe_manager=ProbeManager(
            backend=backend_tp1,
            latch=latch_tp1,
            staging_root=tmp_path / "tp1",
            cooldown_seconds=0.0,
            sweep_on_init=False,
        ),
    )
    ensure_outermost_sentinel_middleware(app_tp1, control_plane=plane_tp1)

    async def _run_tp1() -> None:
        r_start = await send_asgi_request(
            app_tp1,
            "POST",
            "/gpustack/v1/probe",
            json_body={"expected_boot_id": latch_tp1.boot_id, "steps": 3},
        )
        assert r_start.status_code == 200
        for _ in range(3):
            worker0.execute_model()
        await asyncio.sleep(0.05)
        r_get = await send_asgi_request(app_tp1, "GET", "/gpustack/v1/probe")
        assert r_get.json()["state"] == "completed"
        assert len(r_get.json()["payload"]["definitions"]) == 1

    run_sync(_run_tp1())

    # 2. Missing model_executor / collective_rpc -> HTTP 422 unsupported_engine_version.
    core_old = _FakeVLLMEngineCore(0, model_executor=None, tp_size=2, dp_size=1)
    ec_old = SimpleNamespace(engine_core=_FakeMPClient([core_old]), errored=False)
    backend_old = VLLMSentinelBackend()
    backend_old.bind(ec_old, SimpleNamespace(), SimpleNamespace())
    latch_old = BootLatch(require_ack=False)
    app_old = FakeFastAPIApp()
    plane_old = SentinelControlPlane(
        backend=backend_old,
        latch=latch_old,
        probe_manager=ProbeManager(
            backend=backend_old,
            latch=latch_old,
            staging_root=tmp_path / "old",
            cooldown_seconds=0.0,
            sweep_on_init=False,
        ),
    )
    ensure_outermost_sentinel_middleware(app_old, control_plane=plane_old)

    r_422 = run_sync(
        send_asgi_request(
            app_old,
            "POST",
            "/gpustack/v1/probe",
            json_body={"expected_boot_id": latch_old.boot_id, "steps": 4},
        )
    )
    assert r_422.status_code == 422
    assert r_422.json()["error"] == "unsupported_engine_version"


def test_sglang_inprocess_probe_record_shapes_and_all_rank_cleanup(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.delenv("SGLANG_ADMIN_API_KEY", raising=False)
    monkeypatch.setenv(ENV_PROBE_SHM_DIR, str(tmp_path / "sglang_shm"))

    class _FakeSGLangTokenizerManager:
        def __init__(self) -> None:
            self.is_pause = False
            self.gracefully_exit = False
            self.server_status = "Up"
            self.is_profiling = False
            self.start_calls: list[dict[str, Any]] = []
            self.stop_calls = 0

        async def start_profile(
            self,
            output_dir: str | None = None,
            num_steps: int | None = None,
            activities: list[str] | None = None,
            record_shapes: bool = False,
        ) -> str:
            self.start_calls.append(
                {
                    "output_dir": output_dir,
                    "num_steps": num_steps,
                    "activities": activities,
                    "record_shapes": record_shapes,
                }
            )
            out = Path(str(output_dir))
            # Simulate multi-rank TP trace export (TP-0, TP-1, TP-2) with sensitive prompt noise.
            rank0_trace = {
                "traceEvents": [
                    {
                        "name": "sglang::fused_add_rms_norm",
                        "dur": 150.0,
                        "args": {
                            "Input Dims": [[64, 4096], [4096]],
                            "Input type": ["bfloat16", "bfloat16"],
                            "prompt": "DO_NOT_LEAK_THIS_PROMPT",
                        },
                    },
                    {
                        "name": "deep_gemm_fp8_gemm",
                        "dur": 800.0,
                        "args": {"Input Dims": [[64, 4096], [4096, 4096]]},
                    },
                    {
                        "name": "ncclAllReduce",
                        "dur": 200.0,
                        "args": {"Input Dims": [[64, 4096]]},
                    },
                ]
            }
            with gzip.open(
                out / "sglang-TP-0-step4.trace.json.gz", "wt", encoding="utf-8"
            ) as fh:
                json.dump(rank0_trace, fh)
            # TP-1 and TP-2 also write trace files that MUST be deleted in finally!
            (out / "sglang-TP-1-step4.trace.json.gz").write_bytes(b"rank1-trace-data")
            (out / "sglang-TP-2-step4.trace.json.gz").write_bytes(b"rank2-trace-data")
            return "started"

        async def stop_profile(self) -> None:
            self.stop_calls += 1
            self.is_profiling = False

    tm = _FakeSGLangTokenizerManager()
    http_mod = SimpleNamespace(
        get_global_state=lambda: SimpleNamespace(tokenizer_manager=tm)
    )
    backend = SGLangSentinelBackend(http_mod)
    latch = BootLatch(require_ack=False)
    app = FakeFastAPIApp()
    plane = SentinelControlPlane(
        backend=backend,
        latch=latch,
        probe_manager=ProbeManager(
            backend=backend,
            latch=latch,
            staging_root=tmp_path / "sglang_shm",
            cooldown_seconds=0.0,
            sweep_on_init=False,
        ),
    )
    ensure_outermost_sentinel_middleware(app, control_plane=plane)

    async def _run_sglang() -> None:
        r_start = await send_asgi_request(
            app,
            "POST",
            "/gpustack/v1/probe",
            json_body={
                "expected_boot_id": latch.boot_id,
                "steps": 4,
                "engine_version": "0.4.6+cu124",
                "manufacturer": "nvidia",
                "architecture": "sm_90a",
            },
        )
        assert r_start.status_code == 200, r_start.body
        probe_id = r_start.json()["probe_id"]
        assert tm.start_calls[0]["num_steps"] == 4
        assert tm.start_calls[0]["activities"] == ["CPU", "GPU"]
        assert tm.start_calls[0]["record_shapes"] is True

        await asyncio.sleep(0.06)
        r_get = await send_asgi_request(app, "GET", "/gpustack/v1/probe")
        assert r_get.status_code == 200
        body = r_get.json()
        assert body["state"] == "completed"
        payload = body["payload"]
        assert payload["engine"] == "sglang"
        assert payload["engine_version"] == "0.4"
        assert [d["op_type"] for d in payload["definitions"]] == ["rmsnorm"]
        assert payload["diagnostics"]["filtered_excluded_count"] == 2
        assert "DO_NOT_LEAK_THIS_PROMPT" not in json.dumps(payload)

        # ALL TP ranks' trace files (TP-0, TP-1, TP-2) and probe dir are completely purged!
        probe_dir = tmp_path / "sglang_shm" / probe_id
        assert not probe_dir.exists()

    run_sync(_run_sglang())

    # Also verify upstream SGLang ProfileReq-object signature: start_profile(self, req=None).
    class _UpstreamReqTM:
        def __init__(self) -> None:
            self.is_pause = False
            self.is_profiling = False
            self.received_reqs: list[Any] = []

        async def start_profile(self, req: Any = None) -> str:
            self.received_reqs.append(req)
            out = Path(str(req.output_dir))
            (out / "sglang-TP-0.trace.json").write_text(
                json.dumps(
                    {
                        "traceEvents": [
                            {
                                "name": "sglang::fused_add_rms_norm",
                                "dur": 75.0,
                                "args": {
                                    "Input Dims": [[32, 4096], [4096]],
                                    "Input type": ["bfloat16", "bfloat16"],
                                },
                            }
                        ]
                    }
                ),
                encoding="utf-8",
            )
            return "ok"

    req_tm = _UpstreamReqTM()
    req_backend = SGLangSentinelBackend(
        SimpleNamespace(
            get_global_state=lambda: SimpleNamespace(tokenizer_manager=req_tm)
        )
    )
    req_latch = BootLatch(require_ack=False)
    req_app = FakeFastAPIApp()
    req_plane = SentinelControlPlane(
        backend=req_backend,
        latch=req_latch,
        probe_manager=ProbeManager(
            backend=req_backend,
            latch=req_latch,
            staging_root=tmp_path / "sglang_req_shm",
            cooldown_seconds=0.0,
            sweep_on_init=False,
        ),
    )
    ensure_outermost_sentinel_middleware(req_app, control_plane=req_plane)

    async def _run_sglang_req() -> None:
        r_s = await send_asgi_request(
            req_app,
            "POST",
            "/gpustack/v1/probe",
            json_body={"expected_boot_id": req_latch.boot_id, "steps": 3},
        )
        assert r_s.status_code == 200
        assert len(req_tm.received_reqs) == 1
        assert req_tm.received_reqs[0].num_steps == 3
        assert req_tm.received_reqs[0].activities == ["CPU", "GPU"]
        assert req_tm.received_reqs[0].record_shapes is True
        await asyncio.sleep(0.05)
        r_g = await send_asgi_request(req_app, "GET", "/gpustack/v1/probe")
        assert r_g.json()["state"] == "completed"
        assert r_g.json()["payload"]["definitions"][0]["op_type"] == "rmsnorm"

    run_sync(_run_sglang_req())

    # TokenizerManager without start_profile returns 422 unsupported_engine_version.
    no_prof_mod = SimpleNamespace(
        get_global_state=lambda: SimpleNamespace(
            tokenizer_manager=SimpleNamespace(is_pause=False)
        )
    )
    backend_no_prof = SGLangSentinelBackend(no_prof_mod)
    latch_np = BootLatch(require_ack=False)
    app_np = FakeFastAPIApp()
    plane_np = SentinelControlPlane(
        backend=backend_no_prof,
        latch=latch_np,
        probe_manager=ProbeManager(
            backend=backend_no_prof,
            latch=latch_np,
            staging_root=tmp_path / "sglang_noprof",
            cooldown_seconds=0.0,
            sweep_on_init=False,
        ),
    )
    ensure_outermost_sentinel_middleware(app_np, control_plane=plane_np)
    r_np = run_sync(
        send_asgi_request(
            app_np,
            "POST",
            "/gpustack/v1/probe",
            json_body={"expected_boot_id": latch_np.boot_id, "steps": 4},
        )
    )
    assert r_np.status_code == 422
    assert r_np.json()["error"] == "unsupported_engine_version"


def _write_sample_kernel_bundle(
    root: Path,
    *,
    engine: str = "vLLM",
    engine_version: str = "0.8.5",
    manufacturer: str = "nvidia",
    architecture: str = "sm_90a",
    include_mismatch: bool = True,
) -> tuple[str, str]:
    root.mkdir(parents=True, exist_ok=True)
    kernel_dir = root / "kernels" / "rmsnorm_v1"
    kernel_dir.mkdir(parents=True, exist_ok=True)
    (kernel_dir / "kernel.py").write_text(
        "def run(x):\n    return x * 2\n",
        encoding="utf-8",
    )
    (root / "solutions").mkdir(exist_ok=True)
    (root / "solutions" / "rmsnorm_v1.json").write_text(
        json.dumps({"name": "rmsnorm_v1"}), encoding="utf-8"
    )
    (root / "traces").mkdir(exist_ok=True)
    (root / "traces" / "rmsnorm_v1.json").write_text(
        json.dumps({"evaluation": {"status": "PASSED"}}), encoding="utf-8"
    )

    good_key = format_local_kernel_key(
        engine=engine,
        engine_version=engine_version,
        manufacturer=manufacturer,
        arch=architecture,
        op_family="rmsnorm",
        const_axes={"hidden_size": 4096},
        input_dtypes=["bfloat16"],
        output_dtypes=["bfloat16"],
    )
    entries: list[dict[str, Any]] = [
        {
            "localKernelKey": good_key,
            "definition": "rmsnorm_4096",
            "solutionPath": "solutions/rmsnorm_v1.json",
            "libraryPath": "kernels/rmsnorm_v1/kernel.py",
            "entrypoint": "run",
            "tracePath": "traces/rmsnorm_v1.json",
            "speedupRatio": 1.25,
        }
    ]
    if include_mismatch:
        other_arch_key = format_local_kernel_key(
            engine=engine,
            engine_version=engine_version,
            manufacturer=manufacturer,
            arch="sm_80",
            op_family="layernorm",
            const_axes={"hidden_size": 4096},
            input_dtypes=["bfloat16"],
            output_dtypes=["bfloat16"],
        )
        entries.append(
            {
                "localKernelKey": other_arch_key,
                "definition": "layernorm_4096",
                "solutionPath": "solutions/rmsnorm_v1.json",
                "libraryPath": "kernels/rmsnorm_v1/kernel.py",
                "entrypoint": "run",
            }
        )
    manifest = {
        "schemaVersion": DISPATCH_MANIFEST_SCHEMA_VERSION,
        "compatibility": {
            "engine": engine,
            "engineVersion": engine_version,
            "manufacturer": manufacturer,
            "architecture": architecture,
        },
        "entries": entries,
    }
    raw = json.dumps(manifest, sort_keys=True, separators=(",", ":")).encode("utf-8")
    (root / "dispatch-manifest.json").write_bytes(raw)
    import hashlib

    digest = f"sha256:{hashlib.sha256(raw).hexdigest()}"
    return good_key, digest


def test_kernel_loader_validates_skips_mismatches_and_reports_additively(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    reset_process_kernel_registry()
    bundle_dir = tmp_path / "kernels"
    good_key, expected_digest = _write_sample_kernel_bundle(bundle_dir)

    # 1. Valid bundle loads matching key and skips mismatched sm_80 key cleanly.
    status = load_kernel_dispatch_manifest(
        bundle_dir,
        engine="vLLM",
        engine_version="0.8.5+cu128",
        manufacturer="nvidia",
        architecture="sm_90a",
    )
    assert status.configured is True
    assert status.kernel_load_error is None
    assert status.dispatch_manifest_digest == expected_digest
    assert status.loaded_kernel_keys == [good_key]
    assert len(status.skipped_kernel_keys) == 1
    assert status.overrides[good_key].load_callable()(21) == 42

    # 2. Manifest-level arch mismatch returns unsupported_kernel_artifact cleanly.
    bad_arch = load_kernel_dispatch_manifest(
        bundle_dir,
        engine="vLLM",
        engine_version="0.8.5",
        manufacturer="amd",
        architecture="gfx942",
    )
    assert bad_arch.kernel_load_error == UNSUPPORTED_KERNEL_ARTIFACT_ERROR
    assert bad_arch.loaded_kernel_keys == []

    # 3. Wrong schemaVersion or missing manifest returns unsupported_kernel_artifact.
    broken_dir = tmp_path / "broken_kernels"
    broken_dir.mkdir()
    (broken_dir / "dispatch-manifest.json").write_text(
        json.dumps({"schemaVersion": "wrong/v99", "compatibility": {}, "entries": []}),
        encoding="utf-8",
    )
    broken = load_kernel_dispatch_manifest(broken_dir, engine="vLLM")
    assert broken.kernel_load_error == UNSUPPORTED_KERNEL_ARTIFACT_ERROR

    # 4. Wire reporting on /gpustack/v1/runtime_config and /gpustack/v1/ready when GPUSTACK_KERNEL_ARTIFACT_DIR is set.
    monkeypatch.setenv(ENV_KERNEL_ARTIFACT_DIR, str(bundle_dir))
    monkeypatch.setenv("GPUSTACK_ENGINE_VERSION", "0.8.5")
    monkeypatch.setenv("GPUSTACK_HARDWARE_MANUFACTURER", "nvidia")
    monkeypatch.setenv("GPUSTACK_HARDWARE_ARCHITECTURE", "sm_90a")
    reset_process_kernel_registry()

    attach_engine_core_utility(_FakeVLLMEngineCore)
    core0 = _FakeVLLMEngineCore(0, model_executor=None, tp_size=1, dp_size=1)
    ec = SimpleNamespace(engine_core=_FakeMPClient([core0]), errored=False)
    backend = VLLMSentinelBackend()
    backend.bind(ec, SimpleNamespace(), SimpleNamespace())
    latch = BootLatch(require_ack=False)
    app = FakeFastAPIApp()
    plane = SentinelControlPlane(
        backend=backend,
        latch=latch,
        probe_manager=ProbeManager(
            backend=backend,
            latch=latch,
            staging_root=tmp_path / "p",
            sweep_on_init=False,
        ),
    )
    ensure_outermost_sentinel_middleware(app, control_plane=plane)

    r_rc = run_sync(send_asgi_request(app, "GET", "/gpustack/v1/runtime_config"))
    assert r_rc.status_code == 200
    rc_body = r_rc.json()
    assert rc_body["loaded_kernel_keys"] == [good_key]
    assert rc_body["dispatch_manifest_digest"] == expected_digest
    assert rc_body["ranks"][0]["loaded_kernel_keys"] == [good_key]
    assert rc_body["ranks"][0]["dispatch_manifest_digest"] == expected_digest

    r_ready = run_sync(send_asgi_request(app, "GET", "/gpustack/v1/ready"))
    assert r_ready.status_code == 200
    ready_body = r_ready.json()
    assert ready_body["ready"] is True
    assert ready_body["loaded_kernel_keys"] == [good_key]
    assert ready_body["dispatch_manifest_digest"] == expected_digest

    # 5. Aggregate kernel reports intersects keys across ranks and detects digest drift.
    agg = aggregate_kernel_reports(
        [
            {
                "loaded_kernel_keys": ["k1", "k2"],
                "dispatch_manifest_digest": "sha256:a",
            },
            {
                "loaded_kernel_keys": ["k2", "k3"],
                "dispatch_manifest_digest": "sha256:b",
            },
        ]
    )
    assert agg["loaded_kernel_keys"] == ["k2"]
    assert agg["kernel_load_error"] == UNSUPPORTED_KERNEL_ARTIFACT_ERROR

    reset_process_kernel_registry()


def test_lazy_post_import_hooks_register_forged_kernels_without_torch_or_vllm_plugins(
    tmp_path: Path,
) -> None:
    bundle_dir = tmp_path / "mounted_kernels"
    good_key, expected_digest = _write_sample_kernel_bundle(
        bundle_dir, engine="vLLM", include_mismatch=False
    )

    fake_pkg = tmp_path / "fake_engines"
    for rel in (
        "vllm/model_executor",
        "vllm/v1/worker",
        "sglang/srt/model_executor",
        "sglang/srt/layers",
    ):
        d = fake_pkg / rel
        d.mkdir(parents=True, exist_ok=True)
        cur = fake_pkg
        for part in Path(rel).parts:
            cur = cur / part
            (cur / "__init__.py").write_text("", encoding="utf-8")

    (fake_pkg / "vllm" / "model_executor" / "custom_op.py").write_text(
        "REGISTERED = {}\ndef register_gpustack_override(family, override):\n    REGISTERED[family] = override.local_kernel_key\n",
        encoding="utf-8",
    )
    (fake_pkg / "vllm" / "v1" / "worker" / "gpu_model_runner.py").write_text(
        "class GPUModelRunner:\n    pass\n", encoding="utf-8"
    )
    (fake_pkg / "sglang" / "srt" / "model_executor" / "model_runner.py").write_text(
        "class ModelRunner:\n    pass\n", encoding="utf-8"
    )
    (fake_pkg / "sglang" / "srt" / "layers" / "layernorm.py").write_text(
        "class RMSNorm:\n    pass\n", encoding="utf-8"
    )
    (fake_pkg / "sglang" / "srt" / "layers" / "rotary_embedding.py").write_text(
        "class RotaryEmbedding:\n    pass\n", encoding="utf-8"
    )

    env = dict(os.environ)
    env["GPUSTACK_ENGINE_SENTINEL_ENABLE"] = "1"
    env[ENV_KERNEL_ARTIFACT_DIR] = str(bundle_dir)
    env["GPUSTACK_ENGINE_VERSION"] = "0.8.5"
    env["GPUSTACK_HARDWARE_MANUFACTURER"] = "nvidia"
    env["GPUSTACK_HARDWARE_ARCHITECTURE"] = "sm_90a"
    env.pop("VLLM_PLUGINS", None)
    env["PYTHONPATH"] = os.pathsep.join([str(REPO_PYTHON_ROOT), str(fake_pkg)])

    script = """
import json, os, sys
import vllm.model_executor.custom_op as custom_op
import vllm.v1.worker.gpu_model_runner as gpu_runner
import sglang.srt.model_executor.model_runner as sgl_runner
import sglang.srt.layers.layernorm as sgl_ln
import sglang.srt.layers.rotary_embedding as sgl_rope

print(json.dumps({
    "registered": custom_op.REGISTERED,
    "custom_op_keys": getattr(custom_op, "_gpustack_loaded_kernel_keys", []),
    "runner_digest": getattr(gpu_runner, "_gpustack_dispatch_manifest_digest", None),
    "sgl_runner_patched": getattr(sgl_runner, "_gpustack_kernel_loader_patched", False),
    "sgl_ln_patched": getattr(sgl_ln, "_gpustack_kernel_loader_patched", False),
    "sgl_rope_patched": getattr(sgl_rope, "_gpustack_kernel_loader_patched", False),
    "vllm_plugins": os.environ.get("VLLM_PLUGINS"),
    "torch_imported": "torch" in sys.modules,
    "triton_imported": "triton" in sys.modules,
    "flashinfer_imported": "flashinfer" in sys.modules,
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
    assert out["registered"] == {"rmsnorm": good_key}
    assert out["custom_op_keys"] == [good_key]
    assert out["runner_digest"] == expected_digest
    assert out["sgl_runner_patched"] is True
    assert out["sgl_ln_patched"] is True
    assert out["sgl_rope_patched"] is True
    assert out["vllm_plugins"] is None
    assert out["torch_imported"] is False
    assert out["triton_imported"] is False
    assert out["flashinfer_imported"] is False


def test_sglang_internal_state_reports_loaded_kernels(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    bundle_dir = tmp_path / "sglang_kernels"
    good_key, expected_digest = _write_sample_kernel_bundle(
        bundle_dir, engine="SGLang", engine_version="0.4.6", include_mismatch=False
    )
    monkeypatch.setenv(ENV_KERNEL_ARTIFACT_DIR, str(bundle_dir))
    monkeypatch.setenv("GPUSTACK_ENGINE_VERSION", "0.4.6")
    monkeypatch.setenv("GPUSTACK_HARDWARE_MANUFACTURER", "nvidia")
    monkeypatch.setenv("GPUSTACK_HARDWARE_ARCHITECTURE", "sm_90a")
    reset_process_kernel_registry()

    class _UpdateHandler:
        def __init__(self, validate: Any, apply: Any) -> None:
            self.validate = validate
            self.apply = apply

    class _Ctrl:
        def __init__(self, scheduler: Any) -> None:
            self.scheduler = scheduler

        def get_internal_state(self, recv_req: Any) -> Any:
            del recv_req
            return SimpleNamespace(internal_state={})

        def _update_handlers(self) -> dict[str, Any]:
            return {}

    fake_mod = SimpleNamespace(
        SchedulerInternalStateController=_Ctrl,
        _UpdateHandler=_UpdateHandler,
        UpdateRejected=ValueError,
    )
    patch_sglang_internal_state_module(fake_mod)

    sched = SimpleNamespace(
        dp_rank=0,
        max_running_requests=32,
        chunked_prefill_size=2048,
        prefill_decode_interval=0,
        _prefill_decode_interval_remaining=0,
        pause_state=SimpleNamespace(paused=False),
        get_num_allocatable_reqs=lambda running_bs, beam_width=None: 32 - running_bs,
    )
    ctrl = _Ctrl(sched)
    out = ctrl.get_internal_state(object())
    sentinel_state = out.internal_state["gpustack_engine_sentinel"]
    assert sentinel_state["loaded_kernel_keys"] == [good_key]
    assert sentinel_state["dispatch_manifest_digest"] == expected_digest
    reset_process_kernel_registry()


def test_vllm_abort_restores_execute_model_and_sglang_shm_exceeded_purges_all_ranks(
    tmp_path: Path,
) -> None:
    attach_engine_core_utility(_FakeVLLMEngineCore)
    worker0 = _FakeGPUWorker([])
    orig_exec = worker0.execute_model
    exec0 = _FakeModelExecutor(worker0)
    core0 = _FakeVLLMEngineCore(0, model_executor=exec0, tp_size=2, dp_size=1)
    ec = SimpleNamespace(engine_core=_FakeMPClient([core0]), errored=False)
    backend = VLLMSentinelBackend()
    backend.bind(ec, SimpleNamespace(), SimpleNamespace())
    latch = BootLatch(require_ack=False)
    app = FakeFastAPIApp()
    plane = SentinelControlPlane(
        backend=backend,
        latch=latch,
        probe_manager=ProbeManager(
            backend=backend,
            latch=latch,
            staging_root=tmp_path / "vllm_abort",
            cooldown_seconds=0.0,
            sweep_on_init=False,
        ),
    )
    ensure_outermost_sentinel_middleware(app, control_plane=plane)

    async def _run_abort() -> None:
        r_start = await send_asgi_request(
            app,
            "POST",
            "/gpustack/v1/probe",
            json_body={"expected_boot_id": latch.boot_id, "steps": 5},
        )
        assert r_start.status_code == 200
        pid = r_start.json()["probe_id"]
        worker0.execute_model()  # 1 step only, still running.
        assert worker0.execute_model is not orig_exec

        r_del = await send_asgi_request(app, "DELETE", "/gpustack/v1/probe")
        assert r_del.status_code == 200
        assert r_del.json()["state"] == "aborted"
        assert worker0.execute_model == orig_exec
        assert not (tmp_path / "vllm_abort" / pid).exists()

    run_sync(_run_abort())

    # SGLang shm cap exceeded calls stop_profile and purges ALL TP ranks' trace files.
    class _OverflowTM:
        def __init__(self) -> None:
            self.is_pause = False
            self.is_profiling = True
            self.stopped = False

        async def start_profile(self, output_dir: str, **kwargs: Any) -> None:
            del kwargs
            out = Path(output_dir)
            (out / "sglang-TP-0.trace.json").write_bytes(b"a" * 512)
            (out / "sglang-TP-1.trace.json").write_bytes(b"b" * 512)

        async def stop_profile(self) -> None:
            self.stopped = True
            self.is_profiling = False

    tm = _OverflowTM()
    sgl_backend = SGLangSentinelBackend(
        SimpleNamespace(get_global_state=lambda: SimpleNamespace(tokenizer_manager=tm))
    )
    sgl_latch = BootLatch(require_ack=False)
    sgl_app = FakeFastAPIApp()
    sgl_plane = SentinelControlPlane(
        backend=sgl_backend,
        latch=sgl_latch,
        probe_manager=ProbeManager(
            backend=sgl_backend,
            latch=sgl_latch,
            staging_root=tmp_path / "sgl_cap",
            cooldown_seconds=0.0,
            shm_size_override=2000,  # 5% -> 100 bytes ceiling
            sweep_on_init=False,
        ),
    )
    ensure_outermost_sentinel_middleware(sgl_app, control_plane=sgl_plane)

    async def _run_sgl_cap() -> None:
        r_s = await send_asgi_request(
            sgl_app,
            "POST",
            "/gpustack/v1/probe",
            json_body={"expected_boot_id": sgl_latch.boot_id, "steps": 3},
        )
        assert r_s.status_code == 200
        pid = r_s.json()["probe_id"]
        await asyncio.sleep(0.06)
        r_g = await send_asgi_request(sgl_app, "GET", "/gpustack/v1/probe")
        assert r_g.json()["state"] == "failed"
        assert r_g.json()["error_code"] == "probe_shm_budget_exceeded"
        assert tm.stopped is True
        assert not (tmp_path / "sgl_cap" / pid).exists()

    run_sync(_run_sgl_cap())


def test_kernel_loader_rejects_symlinks_missing_manifest_and_tracks_registry(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    from gpustack_engine_sentinel.kernel_loader import (
        ensure_process_kernels_loaded,
        get_process_kernel_registry,
    )

    reset_process_kernel_registry()
    # Unconfigured dir -> dormant.
    assert load_kernel_dispatch_manifest(None).configured is False
    # Missing directory -> unsupported_kernel_artifact.
    assert (
        load_kernel_dispatch_manifest(tmp_path / "does_not_exist").kernel_load_error
        == UNSUPPORTED_KERNEL_ARTIFACT_ERROR
    )

    # Symlinked libraryPath is skipped cleanly while valid manifest stays intact.
    sym_dir = tmp_path / "sym_bundle"
    good_key, _ = _write_sample_kernel_bundle(sym_dir, include_mismatch=False)
    outside = tmp_path / "evil.py"
    outside.write_text("def run(x): return x\n", encoding="utf-8")
    lib = sym_dir / "kernels" / "rmsnorm_v1" / "kernel.py"
    lib.unlink()
    os.symlink(outside, lib)
    sym_status = load_kernel_dispatch_manifest(sym_dir, engine="vLLM")
    assert sym_status.loaded_kernel_keys == []
    assert good_key in sym_status.skipped_kernel_keys

    # Registry helper APIs.
    ok_dir = tmp_path / "ok_bundle"
    ok_key, _ = _write_sample_kernel_bundle(ok_dir, include_mismatch=False)
    monkeypatch.setenv(ENV_KERNEL_ARTIFACT_DIR, str(ok_dir))
    monkeypatch.setenv("GPUSTACK_ENGINE_VERSION", "0.8.5")
    monkeypatch.setenv("GPUSTACK_HARDWARE_MANUFACTURER", "nvidia")
    monkeypatch.setenv("GPUSTACK_HARDWARE_ARCHITECTURE", "sm_90a")
    loaded = ensure_process_kernels_loaded(engine="vLLM", force=True)
    assert loaded.loaded_kernel_keys == [ok_key]
    reg = get_process_kernel_registry()
    assert reg.get_override_for_family("rmsnorm") is not None
    assert reg.get_override_for_family("nonexistent") is None
    reg.mark_target_hooked("vllm.model_executor.custom_op")
    assert "vllm.model_executor.custom_op" in reg.registered_targets
    reset_process_kernel_registry()


def test_kernel_loader_inprocess_module_hooks_and_manifest_validation_edges(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    from gpustack_engine_sentinel.kernel_loader import (
        KernelLoadStatus,
        ensure_process_kernels_loaded,
        patch_sglang_layernorm_module,
        patch_sglang_model_runner_module,
        patch_sglang_rotary_embedding_module,
        patch_vllm_custom_op_module,
        patch_vllm_gpu_model_runner_module,
    )

    reset_process_kernel_registry()
    assert KernelLoadStatus(configured=False).to_report() == {}
    assert (
        KernelLoadStatus(
            configured=True, kernel_load_error=UNSUPPORTED_KERNEL_ARTIFACT_ERROR
        ).to_report()["kernel_load_error"]
        == UNSUPPORTED_KERNEL_ARTIFACT_ERROR
    )

    # Dormant when env unset.
    monkeypatch.delenv(ENV_KERNEL_ARTIFACT_DIR, raising=False)
    dormant_mod = SimpleNamespace()
    assert patch_vllm_custom_op_module(dormant_mod).configured is False
    assert not hasattr(dormant_mod, "_gpustack_kernel_loader_patched")
    assert ensure_process_kernels_loaded(force=True).configured is False

    # Malformed manifest cases: bad JSON, non-dict, non-dict compatibility, non-list entries.
    for name, content in (
        ("bad_json", "{not-json"),
        ("non_dict", "[]"),
        (
            "bad_compat",
            json.dumps(
                {"schemaVersion": DISPATCH_MANIFEST_SCHEMA_VERSION, "compatibility": []}
            ),
        ),
        (
            "bad_entries",
            json.dumps(
                {
                    "schemaVersion": DISPATCH_MANIFEST_SCHEMA_VERSION,
                    "compatibility": {},
                    "entries": "nope",
                }
            ),
        ),
    ):
        d = tmp_path / name
        d.mkdir()
        (d / "dispatch-manifest.json").write_text(content, encoding="utf-8")
        assert (
            load_kernel_dispatch_manifest(d, engine="vLLM").kernel_load_error
            == UNSUPPORTED_KERNEL_ARTIFACT_ERROR
        )

    # Skips invalid entries (non-dict, empty key, invalid key, bad solution/trace paths, version mismatch).
    mixed_dir = tmp_path / "mixed_entries"
    good_key, _ = _write_sample_kernel_bundle(mixed_dir, include_mismatch=False)
    manifest = json.loads((mixed_dir / "dispatch-manifest.json").read_text("utf-8"))
    manifest["entries"].extend(
        [
            "not-a-dict",
            {"localKernelKey": ""},
            {"localKernelKey": "bad/key"},
            {
                "localKernelKey": good_key,
                "libraryPath": "kernels/rmsnorm_v1/kernel.py",
                "solutionPath": "missing/sol.json",
            },
            {
                "localKernelKey": good_key,
                "libraryPath": "kernels/rmsnorm_v1/kernel.py",
                "tracePath": "missing/trace.json",
            },
        ]
    )
    (mixed_dir / "dispatch-manifest.json").write_text(
        json.dumps(manifest), encoding="utf-8"
    )
    mixed_st = load_kernel_dispatch_manifest(
        mixed_dir,
        engine="vLLM",
        engine_version="0.8.5",
        manufacturer="nvidia",
        architecture="sm_90a",
    )
    assert mixed_st.loaded_kernel_keys == [good_key]
    assert len(mixed_st.skipped_kernel_keys) >= 2
    assert (
        load_kernel_dispatch_manifest(
            mixed_dir, engine="SGLang", engine_version="0.8.5"
        ).kernel_load_error
        == UNSUPPORTED_KERNEL_ARTIFACT_ERROR
    )
    assert (
        load_kernel_dispatch_manifest(
            mixed_dir, engine="vLLM", engine_version="0.9.9"
        ).kernel_load_error
        == UNSUPPORTED_KERNEL_ARTIFACT_ERROR
    )

    # Missing entrypoint raises AttributeError lazily on load_callable().
    bad_ep_dir = tmp_path / "bad_ep"
    bad_ep_key, _ = _write_sample_kernel_bundle(bad_ep_dir, include_mismatch=False)
    m_ep = json.loads((bad_ep_dir / "dispatch-manifest.json").read_text("utf-8"))
    m_ep["entries"][0]["entrypoint"] = "nonexistent_fn"
    (bad_ep_dir / "dispatch-manifest.json").write_text(
        json.dumps(m_ep), encoding="utf-8"
    )
    st_ep = load_kernel_dispatch_manifest(bad_ep_dir, engine="vLLM")
    with pytest.raises(AttributeError):
        st_ep.overrides[bad_ep_key].load_callable()

    # In-process patch helpers for all 5 hook targets + idempotency + failing register callback.
    monkeypatch.setenv(ENV_KERNEL_ARTIFACT_DIR, str(mixed_dir))
    monkeypatch.setenv("GPUSTACK_ENGINE_VERSION", "0.8.5")
    monkeypatch.setenv("GPUSTACK_HARDWARE_MANUFACTURER", "nvidia")
    monkeypatch.setenv("GPUSTACK_HARDWARE_ARCHITECTURE", "sm_90a")
    reset_process_kernel_registry()

    def _boom_register(family: str, override: Any) -> None:
        del family, override
        raise RuntimeError("register failed")

    m1 = SimpleNamespace(register_gpustack_override=_boom_register)
    patch_vllm_custom_op_module(m1)
    patch_vllm_custom_op_module(m1)  # idempotent second call
    assert m1._gpustack_kernel_loader_patched is True

    m2 = SimpleNamespace()
    patch_vllm_gpu_model_runner_module(m2)
    assert m2._gpustack_loaded_kernel_keys == [good_key]

    sgl_dir = tmp_path / "sgl_inproc"
    sgl_key, _ = _write_sample_kernel_bundle(
        sgl_dir, engine="SGLang", engine_version="0.4.6", include_mismatch=False
    )
    monkeypatch.setenv(ENV_KERNEL_ARTIFACT_DIR, str(sgl_dir))
    monkeypatch.setenv("GPUSTACK_ENGINE_VERSION", "0.4.6")
    reset_process_kernel_registry()

    s1, s2, s3 = SimpleNamespace(), SimpleNamespace(), SimpleNamespace()
    patch_sglang_model_runner_module(s1)
    patch_sglang_layernorm_module(s2)
    patch_sglang_rotary_embedding_module(s3)
    assert s1._gpustack_loaded_kernel_keys == [sgl_key]
    assert "rmsnorm" in s2._gpustack_kernel_overrides
    assert s3._gpustack_kernel_overrides == {}
    reset_process_kernel_registry()


@pytest.mark.parametrize(
    "manufacturer,want_arch,have_arch,expected",
    [
        ("nvidia", "sm_90", "sm_90a", True),
        ("nvidia", "sm_90a", "sm_90", True),
        ("nvidia", "sm_90a", "9.0", True),
        ("nvidia", "9.0", "sm_90a", True),
        ("nvidia", "compute_90a", "9.0", True),
        ("nvidia", "sm_90a", "sm_90f", False),
        ("nvidia", "sm_80", "sm_90", False),
        ("amd", "gfx942:sramecc+:xnack-", "gfx942", True),
        ("amd", "gfx942", "gfx942:sramecc+", True),
        ("amd", "gfx942", "gfx90a", False),
    ],
)
def test_kernel_loader_arch_compatible_parity_with_go_kernelkey(
    tmp_path: Path,
    manufacturer: str,
    want_arch: str,
    have_arch: str,
    expected: bool,
) -> None:
    from gpustack_engine_sentinel.kernel_loader import arch_compatible

    assert arch_compatible(manufacturer, want_arch, have_arch) is expected

    bundle_dir = tmp_path / f"bundle_{manufacturer}_{want_arch.replace(':', '_')}"
    good_key, _ = _write_sample_kernel_bundle(
        bundle_dir,
        engine="vLLM",
        engine_version="0.8.5",
        manufacturer=manufacturer,
        architecture=want_arch,
        include_mismatch=False,
    )
    status = load_kernel_dispatch_manifest(
        bundle_dir,
        engine="vLLM",
        engine_version="0.8.5",
        manufacturer=manufacturer,
        architecture=have_arch,
    )
    if expected:
        assert status.kernel_load_error is None
        assert status.loaded_kernel_keys == [good_key]
    else:
        assert status.kernel_load_error == UNSUPPORTED_KERNEL_ARTIFACT_ERROR
        assert status.loaded_kernel_keys == []


def test_kernel_loader_binds_real_vllm_custom_op_and_sglang_layers_with_shape_fallback(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    from types import ModuleType

    from gpustack_engine_sentinel.kernel_loader import (
        patch_sglang_layernorm_module,
        patch_sglang_rotary_embedding_module,
        patch_vllm_custom_op_module,
    )

    class _ShapeTensor:
        def __init__(self, shape: tuple[int, ...], tag: str = "x") -> None:
            self.shape = shape
            self.ndim = len(shape)
            self.tag = tag

    # 1. Build vLLM bundle with rmsnorm (hidden_size=4096) and activation_quant (hidden_size=8192).
    vllm_dir = tmp_path / "vllm_bind_bundle"
    (vllm_dir / "kernels" / "rmsnorm").mkdir(parents=True)
    (vllm_dir / "kernels" / "silu").mkdir(parents=True)
    (vllm_dir / "kernels" / "rmsnorm" / "kernel.py").write_text(
        "def run(x, weight, eps=1e-6):\n    return ('forged_vllm_rmsnorm', x.shape, weight.shape, eps)\n",
        encoding="utf-8",
    )
    (vllm_dir / "kernels" / "silu" / "kernel.py").write_text(
        "def run(x):\n    return ('forged_vllm_silu', x.shape)\n",
        encoding="utf-8",
    )
    rms_key = format_local_kernel_key(
        engine="vLLM",
        engine_version="0.8.5",
        manufacturer="nvidia",
        arch="sm_90a",
        op_family="rmsnorm",
        const_axes={"hidden_size": 4096},
        input_dtypes=["bfloat16"],
        output_dtypes=["bfloat16"],
    )
    silu_key = format_local_kernel_key(
        engine="vLLM",
        engine_version="0.8.5",
        manufacturer="nvidia",
        arch="sm_90a",
        op_family="activation_quant",
        const_axes={"hidden_size": 8192},
        input_dtypes=["bfloat16"],
        output_dtypes=["bfloat16"],
    )
    (vllm_dir / "dispatch-manifest.json").write_text(
        json.dumps(
            {
                "schemaVersion": DISPATCH_MANIFEST_SCHEMA_VERSION,
                "compatibility": {
                    "engine": "vLLM",
                    "engineVersion": "0.8.5",
                    "manufacturer": "nvidia",
                    "architecture": "sm_90a",
                },
                "entries": [
                    {
                        "localKernelKey": rms_key,
                        "definition": "rmsnorm_4096",
                        "constAxes": {"hidden_size": 4096},
                        "libraryPath": "kernels/rmsnorm/kernel.py",
                        "entrypoint": "run",
                    },
                    {
                        "localKernelKey": silu_key,
                        "definition": "silu_and_mul_8192",
                        "constAxes": {"hidden_size": 8192},
                        "libraryPath": "kernels/silu/kernel.py",
                        "entrypoint": "run",
                    },
                ],
            }
        ),
        encoding="utf-8",
    )

    monkeypatch.setenv(ENV_KERNEL_ARTIFACT_DIR, str(vllm_dir))
    monkeypatch.setenv("GPUSTACK_ENGINE_VERSION", "0.8.5")
    monkeypatch.setenv("GPUSTACK_HARDWARE_MANUFACTURER", "nvidia")
    monkeypatch.setenv("GPUSTACK_HARDWARE_ARCHITECTURE", "sm_90")
    reset_process_kernel_registry()

    # Realistic mock vllm.model_executor.custom_op module mirroring CustomOp.register + dispatch_forward.
    custom_op_mod = ModuleType("vllm.model_executor.custom_op")
    op_registry: dict[str, type[Any]] = {}

    class CustomOp:
        def __init__(self) -> None:
            self._forward_method = self.dispatch_forward()

        def dispatch_forward(self) -> Any:
            return self.forward_cuda

        def forward(self, *args: Any, **kwargs: Any) -> Any:
            return self._forward_method(*args, **kwargs)

        def forward_cuda(self, *args: Any, **kwargs: Any) -> Any:
            raise NotImplementedError

        def forward_native(self, *args: Any, **kwargs: Any) -> Any:
            raise NotImplementedError

        @classmethod
        def register(cls, name: str) -> Any:
            def decorator(op_cls: type[Any]) -> type[Any]:
                op_cls.name = name
                op_registry[name] = op_cls
                return op_cls

            return decorator

    custom_op_mod.op_registry = op_registry  # type: ignore[attr-defined]
    custom_op_mod.CustomOp = CustomOp  # type: ignore[attr-defined]
    patch_vllm_custom_op_module(custom_op_mod)

    @CustomOp.register("rms_norm")
    class VLLMRMSNorm(CustomOp):
        def __init__(self, hidden_size: int, eps: float = 1e-6) -> None:
            self.hidden_size = hidden_size
            self.variance_epsilon = eps
            self.weight = _ShapeTensor((hidden_size,), "w")
            super().__init__()

        def forward_cuda(self, x: Any, residual: Any = None) -> Any:
            return ("stock_vllm_rmsnorm", x.shape, residual is not None)

        def forward_native(self, x: Any, residual: Any = None) -> Any:
            return ("stock_vllm_rmsnorm_native", x.shape, residual is not None)

    @CustomOp.register("silu_and_mul")
    class VLLMSiluAndMul(CustomOp):
        def forward_cuda(self, x: Any) -> Any:
            return ("stock_vllm_silu_cuda", x.shape)

        @staticmethod
        def forward_native(x: Any) -> Any:
            return ("stock_vllm_silu_native", x.shape)

    rms_4096 = VLLMRMSNorm(4096, eps=1e-5)
    assert rms_4096.forward(_ShapeTensor((16, 4096))) == (
        "forged_vllm_rmsnorm",
        (16, 4096),
        (4096,),
        1e-5,
    )
    # Mismatched dimension falls back cleanly to stock forward_cuda.
    assert rms_4096.forward(_ShapeTensor((16, 2048))) == (
        "stock_vllm_rmsnorm",
        (16, 2048),
        False,
    )
    # Fused residual path falls back cleanly when forged kernel only accepts (x, weight, eps).
    assert rms_4096.forward(
        _ShapeTensor((16, 4096)), residual=_ShapeTensor((16, 4096))
    ) == ("stock_vllm_rmsnorm", (16, 4096), True)

    silu = VLLMSiluAndMul()
    assert silu.forward(_ShapeTensor((8, 8192))) == ("forged_vllm_silu", (8, 8192))
    assert silu.forward(_ShapeTensor((8, 4096))) == ("stock_vllm_silu_cuda", (8, 4096))
    assert VLLMSiluAndMul.forward_native(_ShapeTensor((8, 8192))) == (
        "forged_vllm_silu",
        (8, 8192),
    )
    assert VLLMSiluAndMul.forward_native(_ShapeTensor((8, 4096))) == (
        "stock_vllm_silu_native",
        (8, 4096),
    )

    # 2. Realistic mock SGLang layernorm (RMSNorm) and rotary_embedding (RotaryEmbedding).
    sgl_dir = tmp_path / "sglang_bind_bundle"
    (sgl_dir / "kernels" / "rmsnorm").mkdir(parents=True)
    (sgl_dir / "kernels" / "rope").mkdir(parents=True)
    (sgl_dir / "kernels" / "rmsnorm" / "kernel.py").write_text(
        "def run(x, weight, eps=1e-6):\n    return ('forged_sglang_rmsnorm', x.shape, weight.shape)\n",
        encoding="utf-8",
    )
    (sgl_dir / "kernels" / "rope" / "kernel.py").write_text(
        "def run(positions, query, key=None):\n    return ('forged_sglang_rope', query.shape, key.shape if key else None)\n",
        encoding="utf-8",
    )
    sgl_rms_key = format_local_kernel_key(
        engine="SGLang",
        engine_version="0.4.6",
        manufacturer="nvidia",
        arch="sm_90a",
        op_family="rmsnorm",
        const_axes={"hidden_size": 4096},
        input_dtypes=["bfloat16"],
        output_dtypes=["bfloat16"],
    )
    sgl_rope_key = format_local_kernel_key(
        engine="SGLang",
        engine_version="0.4.6",
        manufacturer="nvidia",
        arch="sm_90a",
        op_family="rope_kv_append",
        const_axes={"num_heads_local": 8, "head_dim": 128},
        input_dtypes=["bfloat16"],
        output_dtypes=["bfloat16"],
    )
    (sgl_dir / "dispatch-manifest.json").write_text(
        json.dumps(
            {
                "schemaVersion": DISPATCH_MANIFEST_SCHEMA_VERSION,
                "compatibility": {
                    "engine": "SGLang",
                    "engineVersion": "0.4.6",
                    "manufacturer": "nvidia",
                    "architecture": "sm_90a",
                },
                "entries": [
                    {
                        "localKernelKey": sgl_rms_key,
                        "definition": "rmsnorm_4096",
                        "constAxes": {"hidden_size": 4096},
                        "libraryPath": "kernels/rmsnorm/kernel.py",
                        "entrypoint": "run",
                    },
                    {
                        "localKernelKey": sgl_rope_key,
                        "definition": "rope_8x128",
                        "constAxes": {"num_heads_local": 8, "head_dim": 128},
                        "libraryPath": "kernels/rope/kernel.py",
                        "entrypoint": "run",
                    },
                ],
            }
        ),
        encoding="utf-8",
    )

    monkeypatch.setenv(ENV_KERNEL_ARTIFACT_DIR, str(sgl_dir))
    monkeypatch.setenv("GPUSTACK_ENGINE_VERSION", "0.4.6")
    reset_process_kernel_registry()

    class SGLangRMSNorm:
        def __init__(self, hidden_size: int, eps: float = 1e-6) -> None:
            self.hidden_size = hidden_size
            self.variance_epsilon = eps
            self.weight = _ShapeTensor((hidden_size,), "w")

        def forward_cuda(self, x: Any, residual: Any = None) -> Any:
            return ("stock_sglang_rmsnorm", x.shape, residual is not None)

        def forward(self, x: Any, residual: Any = None) -> Any:
            return self.forward_cuda(x, residual)

    class SGLangRotaryEmbedding:
        def __init__(self, head_size: int) -> None:
            self.head_size = head_size

        def forward_cuda(self, positions: Any, query: Any, key: Any = None) -> Any:
            return ("stock_sglang_rope", query.shape, key.shape if key else None)

        def forward(self, positions: Any, query: Any, key: Any = None) -> Any:
            return self.forward_cuda(positions, query, key)

    sgl_ln_mod = ModuleType("sglang.srt.layers.layernorm")
    sgl_ln_mod.RMSNorm = SGLangRMSNorm  # type: ignore[attr-defined]
    sgl_rope_mod = ModuleType("sglang.srt.layers.rotary_embedding")
    sgl_rope_mod.RotaryEmbedding = SGLangRotaryEmbedding  # type: ignore[attr-defined]

    patch_sglang_layernorm_module(sgl_ln_mod)
    patch_sglang_rotary_embedding_module(sgl_rope_mod)

    sgl_rms = SGLangRMSNorm(4096)
    assert sgl_rms.forward(_ShapeTensor((32, 4096))) == (
        "forged_sglang_rmsnorm",
        (32, 4096),
        (4096,),
    )
    assert sgl_rms.forward(_ShapeTensor((32, 1024))) == (
        "stock_sglang_rmsnorm",
        (32, 1024),
        False,
    )

    sgl_rope = SGLangRotaryEmbedding(head_size=128)
    assert sgl_rope.forward(
        _ShapeTensor((32,)),
        _ShapeTensor((32, 8, 128)),
        _ShapeTensor((32, 8, 128)),
    ) == ("forged_sglang_rope", (32, 8, 128), (32, 8, 128))
    # Mismatched head_dim/num_heads falls back to stock forward_cuda.
    assert sgl_rope.forward(
        _ShapeTensor((32,)),
        _ShapeTensor((32, 4, 64)),
        _ShapeTensor((32, 4, 64)),
    ) == ("stock_sglang_rope", (32, 4, 64), (32, 4, 64))

    # Zero eager ML imports throughout.
    assert "torch" not in sys.modules
    assert "triton" not in sys.modules
    assert "flashinfer" not in sys.modules
    reset_process_kernel_registry()


def test_l3_mock_solutions_are_indexed_in_loaded_keys_but_non_dispatchable(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    from types import ModuleType

    from gpustack_engine_sentinel.kernel_loader import (
        get_process_kernel_registry,
        patch_sglang_layernorm_module,
        patch_vllm_custom_op_module,
    )

    mock_dir = tmp_path / "mock_bundle"
    (mock_dir / "kernels" / "layernorm_mock_v1").mkdir(parents=True)
    (mock_dir / "solutions").mkdir(parents=True)
    (mock_dir / "kernels" / "layernorm_mock_v1" / "kernel.py").write_text(
        "def run(x, weight=None, eps=1e-6):\n    return ('MOCK_BODY_MUST_NEVER_RUN', x)\n",
        encoding="utf-8",
    )
    (mock_dir / "solutions" / "layernorm_mock_v1.json").write_text(
        json.dumps({"name": "layernorm_mock_v1", "mock": True}),
        encoding="utf-8",
    )
    ln_key = format_local_kernel_key(
        engine="vLLM",
        engine_version="0.8.5",
        manufacturer="nvidia",
        arch="sm_90a",
        op_family="layernorm",
        const_axes={"hidden_size": 4096},
        input_dtypes=["bfloat16"],
        output_dtypes=["bfloat16"],
    )
    silu_key = format_local_kernel_key(
        engine="vLLM",
        engine_version="0.8.5",
        manufacturer="nvidia",
        arch="sm_90a",
        op_family="activation_quant",
        const_axes={"hidden_size": 4096},
        input_dtypes=["bfloat16"],
        output_dtypes=["bfloat16"],
    )
    (mock_dir / "solutions" / "silu_plain.json").write_text(
        json.dumps({"name": "silu_plain"}),
        encoding="utf-8",
    )
    (mock_dir / "dispatch-manifest.json").write_text(
        json.dumps(
            {
                "schemaVersion": DISPATCH_MANIFEST_SCHEMA_VERSION,
                "compatibility": {
                    "engine": "vLLM",
                    "engineVersion": "0.8.5",
                    "manufacturer": "nvidia",
                    "architecture": "sm_90a",
                },
                "entries": [
                    {
                        "localKernelKey": ln_key,
                        "definition": "layernorm_4096",
                        "constAxes": {"hidden_size": 4096},
                        "solutionPath": "solutions/layernorm_mock_v1.json",
                        "libraryPath": "kernels/layernorm_mock_v1/kernel.py",
                        "entrypoint": "run",
                    },
                    {
                        "localKernelKey": silu_key,
                        "definition": "silu_4096",
                        "constAxes": {"hidden_size": 4096},
                        "solutionPath": "solutions/silu_plain.json",
                        "libraryPath": "kernels/layernorm_mock_v1/kernel.py",
                        "entrypoint": "run",
                        "mock": True,
                    },
                ],
            }
        ),
        encoding="utf-8",
    )

    monkeypatch.setenv(ENV_KERNEL_ARTIFACT_DIR, str(mock_dir))
    monkeypatch.setenv("GPUSTACK_ENGINE_VERSION", "0.8.5")
    monkeypatch.setenv("GPUSTACK_HARDWARE_MANUFACTURER", "nvidia")
    monkeypatch.setenv("GPUSTACK_HARDWARE_ARCHITECTURE", "sm_90a")
    reset_process_kernel_registry()

    status = load_kernel_dispatch_manifest(
        mock_dir,
        engine="vLLM",
        engine_version="0.8.5",
        manufacturer="nvidia",
        architecture="sm_90a",
    )
    assert status.kernel_load_error is None
    assert sorted(status.loaded_kernel_keys) == sorted([ln_key, silu_key])
    assert status.overrides[ln_key].dispatchable is False
    assert status.overrides[silu_key].dispatchable is False

    custom_op_mod = ModuleType("vllm.model_executor.custom_op")
    registered: dict[str, Any] = {}
    custom_op_mod.register_gpustack_override = lambda fam, ov: registered.setdefault(
        fam, ov
    )  # type: ignore[attr-defined]

    class LayerNorm:
        def __init__(self) -> None:
            self.hidden_size = 4096
            self.weight = SimpleNamespace(shape=(4096,))
            self.bias = None

        def forward_cuda(self, x: Any) -> Any:
            return ("stock_layernorm", x.shape)

    custom_op_mod.LayerNorm = LayerNorm  # type: ignore[attr-defined]
    patch_vllm_custom_op_module(custom_op_mod)

    reg = get_process_kernel_registry()
    assert sorted(reg.status.loaded_kernel_keys) == sorted([ln_key, silu_key])
    assert reg.get_override_for_family("layernorm") is None
    assert reg.get_override_for_family("activation_quant") is None
    assert registered == {}
    assert sorted(custom_op_mod._gpustack_loaded_kernel_keys) == sorted(
        [ln_key, silu_key]
    )

    ln = LayerNorm()
    assert ln.forward_cuda(SimpleNamespace(shape=(8, 4096))) == (
        "stock_layernorm",
        (8, 4096),
    )

    sgl_mod = ModuleType("sglang.srt.layers.layernorm")
    sgl_mod.LayerNorm = LayerNorm  # type: ignore[attr-defined]
    patch_sglang_layernorm_module(sgl_mod)
    assert ln.forward_cuda(SimpleNamespace(shape=(8, 4096))) == (
        "stock_layernorm",
        (8, 4096),
    )
    reset_process_kernel_registry()


def test_l4_hooked_forward_catches_runtime_exception_disables_override_and_surfaces_error(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, caplog: pytest.LogCaptureFixture
) -> None:
    import logging
    from types import ModuleType

    from gpustack_engine_sentinel.kernel_loader import patch_vllm_custom_op_module

    boom_dir = tmp_path / "boom_bundle"
    (boom_dir / "kernels" / "rmsnorm").mkdir(parents=True)
    call_counter_file = tmp_path / "boom_calls.txt"
    (boom_dir / "kernels" / "rmsnorm" / "kernel.py").write_text(
        f"_CALLS = 0\n"
        f"def run(x, weight, eps=1e-6):\n"
        f"    global _CALLS\n"
        f"    _CALLS += 1\n"
        f"    with open({str(call_counter_file)!r}, 'w', encoding='utf-8') as fh:\n"
        f"        fh.write(str(_CALLS))\n"
        f"    raise RuntimeError('dynamic num_tokens=1 kernel launch failed')\n",
        encoding="utf-8",
    )
    rms_key = format_local_kernel_key(
        engine="vLLM",
        engine_version="0.8.5",
        manufacturer="nvidia",
        arch="sm_90a",
        op_family="rmsnorm",
        const_axes={"hidden_size": 4096},
        input_dtypes=["bfloat16"],
        output_dtypes=["bfloat16"],
    )
    (boom_dir / "dispatch-manifest.json").write_text(
        json.dumps(
            {
                "schemaVersion": DISPATCH_MANIFEST_SCHEMA_VERSION,
                "compatibility": {
                    "engine": "vLLM",
                    "engineVersion": "0.8.5",
                    "manufacturer": "nvidia",
                    "architecture": "sm_90a",
                },
                "entries": [
                    {
                        "localKernelKey": rms_key,
                        "definition": "rmsnorm_4096",
                        "constAxes": {"hidden_size": 4096},
                        "libraryPath": "kernels/rmsnorm/kernel.py",
                        "entrypoint": "run",
                    }
                ],
            }
        ),
        encoding="utf-8",
    )

    monkeypatch.setenv(ENV_KERNEL_ARTIFACT_DIR, str(boom_dir))
    monkeypatch.setenv("GPUSTACK_ENGINE_VERSION", "0.8.5")
    monkeypatch.setenv("GPUSTACK_HARDWARE_MANUFACTURER", "nvidia")
    monkeypatch.setenv("GPUSTACK_HARDWARE_ARCHITECTURE", "sm_90a")
    reset_process_kernel_registry()

    attach_engine_core_utility(_FakeVLLMEngineCore)
    core0 = _FakeVLLMEngineCore(0, model_executor=None, tp_size=1, dp_size=1)
    ec = SimpleNamespace(engine_core=_FakeMPClient([core0]), errored=False)
    backend = VLLMSentinelBackend()
    backend.bind(ec, SimpleNamespace(), SimpleNamespace())
    latch = BootLatch(require_ack=False)
    app = FakeFastAPIApp()
    plane = SentinelControlPlane(
        backend=backend,
        latch=latch,
        probe_manager=ProbeManager(
            backend=backend,
            latch=latch,
            staging_root=tmp_path / "p_l4",
            sweep_on_init=False,
        ),
    )
    ensure_outermost_sentinel_middleware(app, control_plane=plane)

    # Initial /ready and /runtime_config succeed and populate cached summaries.
    r_ready_init = run_sync(send_asgi_request(app, "GET", "/gpustack/v1/ready"))
    assert r_ready_init.status_code == 200
    assert r_ready_init.json()["ready"] is True
    r_rc_init = run_sync(send_asgi_request(app, "GET", "/gpustack/v1/runtime_config"))
    assert r_rc_init.status_code == 200
    assert "kernel_load_error" not in r_rc_init.json()

    custom_op_mod = ModuleType("vllm.model_executor.custom_op")

    class RMSNorm:
        def __init__(self) -> None:
            self.hidden_size = 4096
            self.variance_epsilon = 1e-6
            self.weight = SimpleNamespace(shape=(4096,))

        def forward_cuda(self, x: Any, residual: Any = None) -> Any:
            return ("stock_rmsnorm_ok", x.shape, residual is not None)

    custom_op_mod.RMSNorm = RMSNorm  # type: ignore[attr-defined]
    patch_vllm_custom_op_module(custom_op_mod)

    layer = RMSNorm()
    with caplog.at_level(logging.WARNING):
        out1 = layer.forward_cuda(SimpleNamespace(shape=(1, 4096)))
        out2 = layer.forward_cuda(SimpleNamespace(shape=(1, 4096)))

    assert out1 == ("stock_rmsnorm_ok", (1, 4096), False)
    assert out2 == ("stock_rmsnorm_ok", (1, 4096), False)
    assert int(call_counter_file.read_text("utf-8")) == 1
    warn_msgs = [r for r in caplog.records if rms_key in r.getMessage()]
    assert len(warn_msgs) == 1

    # Both /ready and /runtime_config immediately surface kernel_load_error for automatic rollback!
    r_ready_after = run_sync(send_asgi_request(app, "GET", "/gpustack/v1/ready"))
    assert r_ready_after.status_code == 503
    ready_json = r_ready_after.json()
    assert ready_json["ready"] is False
    assert ready_json["kernel_load_error"] == UNSUPPORTED_KERNEL_ARTIFACT_ERROR

    r_rc_after = run_sync(send_asgi_request(app, "GET", "/gpustack/v1/runtime_config"))
    assert r_rc_after.status_code == 200
    rc_json = r_rc_after.json()
    assert rc_json["ready"] is False
    assert rc_json["kernel_load_error"] == UNSUPPORTED_KERNEL_ARTIFACT_ERROR
    assert rc_json["ranks"][0]["kernel_load_error"] == UNSUPPORTED_KERNEL_ARTIFACT_ERROR

    reset_process_kernel_registry()


def test_profiler_events_no_double_count_when_key_averages_and_trace_file_both_exist(
    tmp_path: Path,
) -> None:
    from gpustack_engine_sentinel.vllm.utility import _extract_profiler_events

    trace_dir = tmp_path / "prof_out"
    trace_dir.mkdir()
    (trace_dir / "rank-0.trace.json").write_text(
        json.dumps(
            {
                "traceEvents": [
                    {
                        "name": "vllm::rms_norm",
                        "dur": 100.0,
                        "args": {"Input Dims": [[16, 4096], [4096]]},
                    }
                ]
            }
        ),
        encoding="utf-8",
    )

    prof_with_events = _FakeTorchProfiler(
        [
            SimpleNamespace(
                key="vllm::rms_norm",
                self_device_time_total=100.0,
                input_shapes=[[16, 4096], [4096]],
                input_dtypes=["bfloat16", "bfloat16"],
                count=2,
            )
        ]
    )
    extracted = _extract_profiler_events(prof_with_events, trace_dir)
    assert len(extracted) == 1
    assert extracted[0]["dur"] == 100.0
    assert extracted[0]["count"] == 2

    # Fallback to trace file still works when profiler key_averages() is empty.
    prof_empty = _FakeTorchProfiler([])
    fallback = _extract_profiler_events(prof_empty, trace_dir)
    assert len(fallback) == 1
    assert fallback[0]["name"] == "vllm::rms_norm"


def test_vllm_tp_multirank_surfaces_nonzero_worker_kernel_load_error_on_ready_and_runtime_config(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    from gpustack_engine_sentinel.kernel_loader import KernelDispatchRegistry

    monkeypatch.delenv(ENV_KERNEL_ARTIFACT_DIR, raising=False)
    reset_process_kernel_registry()

    bundle_dir = tmp_path / "tp_bundle"
    good_key, expected_digest = _write_sample_kernel_bundle(
        bundle_dir, include_mismatch=False
    )

    # Simulate TP=2 where rank 0 loaded cleanly and rank 1 hit a runtime kernel_load_error.
    reg_rank0 = KernelDispatchRegistry()
    reg_rank0._status = load_kernel_dispatch_manifest(
        bundle_dir,
        engine="vLLM",
        engine_version="0.8.5",
        manufacturer="nvidia",
        architecture="sm_90a",
    )
    reg_rank1 = KernelDispatchRegistry()
    reg_rank1._status = load_kernel_dispatch_manifest(
        bundle_dir,
        engine="vLLM",
        engine_version="0.8.5",
        manufacturer="nvidia",
        architecture="sm_90a",
    )
    reg_rank1.record_runtime_error(good_key)

    worker0 = _FakeGPUWorker([], rank=0)
    worker0._gpustack_kernel_registry = reg_rank0  # type: ignore[attr-defined]
    worker1 = _FakeGPUWorker([], rank=1)
    worker1._gpustack_kernel_registry = reg_rank1  # type: ignore[attr-defined]

    exec_tp2 = _FakeModelExecutor(worker0, worker1)
    attach_engine_core_utility(_FakeVLLMEngineCore)
    core0 = _FakeVLLMEngineCore(0, model_executor=exec_tp2, tp_size=2, dp_size=1)
    ec = SimpleNamespace(engine_core=_FakeMPClient([core0]), errored=False)

    backend = VLLMSentinelBackend()
    backend.bind(ec, SimpleNamespace(), SimpleNamespace())
    latch = BootLatch(require_ack=False)
    app = FakeFastAPIApp()
    plane = SentinelControlPlane(
        backend=backend,
        latch=latch,
        probe_manager=ProbeManager(
            backend=backend,
            latch=latch,
            staging_root=tmp_path / "p_tp2",
            sweep_on_init=False,
        ),
    )
    ensure_outermost_sentinel_middleware(app, control_plane=plane)

    # Poll /gpustack/v1/ready FIRST (before /runtime_config): rank 1 error must already surface!
    r_ready = run_sync(send_asgi_request(app, "GET", "/gpustack/v1/ready"))
    assert r_ready.status_code == 503
    ready_body = r_ready.json()
    assert ready_body["ready"] is False
    assert ready_body["kernel_load_error"] == UNSUPPORTED_KERNEL_ARTIFACT_ERROR
    assert ready_body["dispatch_manifest_digest"] == expected_digest
    assert any(
        c["fn"] == "vllm_worker_kernel_report" and c["unique_reply_rank"] is None
        for c in exec_tp2.collective_calls
    )

    # Poll /gpustack/v1/runtime_config: rank 1 error surfaces on rank receipt and top-level summary.
    r_rc = run_sync(send_asgi_request(app, "GET", "/gpustack/v1/runtime_config"))
    assert r_rc.status_code == 200
    rc_body = r_rc.json()
    assert rc_body["ready"] is False
    assert rc_body["kernel_load_error"] == UNSUPPORTED_KERNEL_ARTIFACT_ERROR
    assert rc_body["ranks"][0]["kernel_load_error"] == UNSUPPORTED_KERNEL_ARTIFACT_ERROR


def test_sglang_ready_queries_scheduler_subprocesses_for_kernel_load_error(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    from gpustack_engine_sentinel.sglang.hooks import STATE_NAMESPACE_KEY

    bundle_dir = tmp_path / "sglang_ready_bundle"
    good_key, expected_digest = _write_sample_kernel_bundle(
        bundle_dir,
        engine="SGLang",
        engine_version="0.4.6",
        include_mismatch=False,
    )

    # Parent HTTP process sees a structurally valid manifest on disk with kernel_load_error=None.
    monkeypatch.setenv(ENV_KERNEL_ARTIFACT_DIR, str(bundle_dir))
    monkeypatch.setenv("GPUSTACK_ENGINE_VERSION", "0.4.6")
    monkeypatch.setenv("GPUSTACK_HARDWARE_MANUFACTURER", "nvidia")
    monkeypatch.setenv("GPUSTACK_HARDWARE_ARCHITECTURE", "sm_90a")
    reset_process_kernel_registry()

    class _FakeSchedulerTM:
        def __init__(self, *, rank1_error: str | None, fail_rpc: bool = False) -> None:
            self.is_pause = False
            self.gracefully_exit = False
            self.server_status = "Up"
            self.rank1_error = rank1_error
            self.fail_rpc = fail_rpc
            self.internal_state_calls = 0

        def is_ready(self) -> bool:
            return True

        async def get_internal_state(self) -> list[dict[str, Any]]:
            self.internal_state_calls += 1
            if self.fail_rpc:
                raise RuntimeError("scheduler internal state temporarily unavailable")
            rank0_state: dict[str, Any] = {
                "rank_id": 0,
                "boot_id": "sgl-boot-0",
                "membership_epoch": 1,
                "applied_digest": "",
                "supported_fields": [],
                "unsupported_fields": [],
                "values": {},
                "loaded_kernel_keys": [good_key],
                "dispatch_manifest_digest": expected_digest,
            }
            rank1_state: dict[str, Any] = {
                "rank_id": 1,
                "boot_id": "sgl-boot-1",
                "membership_epoch": 1,
                "applied_digest": "",
                "supported_fields": [],
                "unsupported_fields": [],
                "values": {},
                "loaded_kernel_keys": [good_key],
                "dispatch_manifest_digest": expected_digest,
            }
            if self.rank1_error:
                rank1_state["kernel_load_error"] = self.rank1_error
            return [
                {STATE_NAMESPACE_KEY: rank0_state},
                {STATE_NAMESPACE_KEY: rank1_state},
            ]

    try:
        tm_err = _FakeSchedulerTM(rank1_error=UNSUPPORTED_KERNEL_ARTIFACT_ERROR)
        backend_err = SGLangSentinelBackend(
            SimpleNamespace(
                get_global_state=lambda: SimpleNamespace(tokenizer_manager=tm_err)
            )
        )
        latch_err = BootLatch(require_ack=False)
        app_err = FakeFastAPIApp()
        plane_err = SentinelControlPlane(
            backend=backend_err,
            latch=latch_err,
            probe_manager=ProbeManager(
                backend=backend_err,
                latch=latch_err,
                staging_root=tmp_path / "p_sgl_err",
                sweep_on_init=False,
            ),
        )
        ensure_outermost_sentinel_middleware(app_err, control_plane=plane_err)

        # Poll /gpustack/v1/ready directly without calling /gpustack/v1/runtime_config first.
        r_ready = run_sync(send_asgi_request(app_err, "GET", "/gpustack/v1/ready"))
        assert r_ready.status_code == 503
        ready_body = r_ready.json()
        assert ready_body["ready"] is False
        assert ready_body["reason"] == UNSUPPORTED_KERNEL_ARTIFACT_ERROR
        assert ready_body["kernel_load_error"] == UNSUPPORTED_KERNEL_ARTIFACT_ERROR
        assert ready_body["dispatch_manifest_digest"] == expected_digest
        assert tm_err.internal_state_calls >= 1

        # Transient get_internal_state failure falls back cleanly to local report without crashing /ready.
        tm_transient = _FakeSchedulerTM(rank1_error=None, fail_rpc=True)
        backend_ok = SGLangSentinelBackend(
            SimpleNamespace(
                get_global_state=lambda: SimpleNamespace(tokenizer_manager=tm_transient)
            )
        )
        latch_ok = BootLatch(require_ack=False)
        app_ok = FakeFastAPIApp()
        plane_ok = SentinelControlPlane(
            backend=backend_ok,
            latch=latch_ok,
            probe_manager=ProbeManager(
                backend=backend_ok,
                latch=latch_ok,
                staging_root=tmp_path / "p_sgl_ok",
                sweep_on_init=False,
            ),
        )
        ensure_outermost_sentinel_middleware(app_ok, control_plane=plane_ok)
        r_fallback = run_sync(send_asgi_request(app_ok, "GET", "/gpustack/v1/ready"))
        assert r_fallback.status_code == 200
        assert r_fallback.json()["ready"] is True
    finally:
        reset_process_kernel_registry()


def test_kernel_loader_major_minor_version_sharing_and_minor_or_prerelease_skip(
    tmp_path: Path,
) -> None:
    reset_process_kernel_registry()

    # 1. Bundle built for 0.8.5 (canonical 0.8) loads cleanly across 0.8.x patch/post releases.
    bundle_08 = tmp_path / "bundle_08"
    key_08, digest_08 = _write_sample_kernel_bundle(
        bundle_08,
        engine="vLLM",
        engine_version="0.8.5",
        manufacturer="nvidia",
        architecture="sm_90a",
        include_mismatch=False,
    )
    assert key_08.startswith("vllm/0.8/nvidia/sm_90a/rmsnorm/")

    for compat_runtime_ver in (
        "0.8.6",
        "0.8",
        "v0.8.6+cu128",
        "0.8.6.post1",
        "0.8.5-12-g1a2b3c4",
    ):
        st_ok = load_kernel_dispatch_manifest(
            bundle_08,
            engine="vLLM",
            engine_version=compat_runtime_ver,
            manufacturer="nvidia",
            architecture="sm_90a",
        )
        assert st_ok.kernel_load_error is None, compat_runtime_ver
        assert st_ok.dispatch_manifest_digest == digest_08
        assert st_ok.loaded_kernel_keys == [key_08]

    # 2. Manifest built for 0.8.5 is rejected cleanly on 0.9.0, 0.9.0rc1, or invalid version strings.
    for incompat_runtime_ver in ("0.9.0", "0.9.0rc1", "invalid-version"):
        st_rej = load_kernel_dispatch_manifest(
            bundle_08,
            engine="vLLM",
            engine_version=incompat_runtime_ver,
            manufacturer="nvidia",
            architecture="sm_90a",
        )
        assert st_rej.kernel_load_error == UNSUPPORTED_KERNEL_ARTIFACT_ERROR
        assert st_rej.loaded_kernel_keys == []

    # 3. Entry-level filtering skips a 0.8 entry on 0.9.0 and skips a 0.9 entry on 0.9.0rc1.
    entry_filter_dir = tmp_path / "entry_version_filter"
    key_09 = format_local_kernel_key(
        engine="vLLM",
        engine_version="0.9.0",
        manufacturer="nvidia",
        arch="sm_90a",
        op_family="layernorm",
        const_axes={"hidden_size": 4096},
        input_dtypes=["bfloat16"],
        output_dtypes=["bfloat16"],
    )
    (entry_filter_dir / "kernels" / "k").mkdir(parents=True)
    (entry_filter_dir / "kernels" / "k" / "kernel.py").write_text(
        "def run(x):\n    return x\n",
        encoding="utf-8",
    )
    (entry_filter_dir / "dispatch-manifest.json").write_text(
        json.dumps(
            {
                "schemaVersion": DISPATCH_MANIFEST_SCHEMA_VERSION,
                "compatibility": {
                    "engine": "vLLM",
                    "manufacturer": "nvidia",
                    "architecture": "sm_90a",
                },
                "entries": [
                    {
                        "localKernelKey": key_08,
                        "definition": "rmsnorm_4096",
                        "libraryPath": "kernels/k/kernel.py",
                        "entrypoint": "run",
                    },
                    {
                        "localKernelKey": key_09,
                        "definition": "layernorm_4096",
                        "libraryPath": "kernels/k/kernel.py",
                        "entrypoint": "run",
                    },
                ],
            }
        ),
        encoding="utf-8",
    )

    on_086 = load_kernel_dispatch_manifest(
        entry_filter_dir,
        engine="vLLM",
        engine_version="0.8.6",
        manufacturer="nvidia",
        architecture="sm_90a",
    )
    assert on_086.kernel_load_error is None
    assert on_086.loaded_kernel_keys == [key_08]
    assert on_086.skipped_kernel_keys == [key_09]

    on_090 = load_kernel_dispatch_manifest(
        entry_filter_dir,
        engine="vLLM",
        engine_version="0.9.0",
        manufacturer="nvidia",
        architecture="sm_90a",
    )
    assert on_090.kernel_load_error is None
    assert on_090.loaded_kernel_keys == [key_09]
    assert on_090.skipped_kernel_keys == [key_08]

    on_090rc1 = load_kernel_dispatch_manifest(
        entry_filter_dir,
        engine="vLLM",
        engine_version="0.9.0rc1",
        manufacturer="nvidia",
        architecture="sm_90a",
    )
    assert on_090rc1.kernel_load_error is None
    assert on_090rc1.loaded_kernel_keys == []
    assert on_090rc1.skipped_kernel_keys == sorted([key_08, key_09])
    reset_process_kernel_registry()


def test_kernel_loader_torch_compile_fullgraph_skips_locks_and_uses_custom_op(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    import sys

    from gpustack_engine_sentinel import kernel_loader

    reset_process_kernel_registry()
    kernel_loader._REGISTERED_CUSTOM_OPS.clear()

    bundle_dir = tmp_path / "compile_bundle"
    key, _ = _write_sample_kernel_bundle(
        bundle_dir,
        engine="vLLM",
        engine_version="0.32.0",
        manufacturer="nvidia",
        architecture="sm_90",
        include_mismatch=False,
    )
    # Overwrite sample kernel with deterministic recognizable output.
    manifest = json.loads((bundle_dir / "dispatch-manifest.json").read_text("utf-8"))
    lib_rel = manifest["entries"][0]["libraryPath"]
    (bundle_dir / lib_rel).write_text(
        "def run(x, weight, eps=1e-6):\n    return ('forged_rmsnorm', x.shape, weight, eps)\n",
        encoding="utf-8",
    )

    compiling = [True]
    registered_schemas: dict[str, str] = {}

    class _FakeCustomOpHandle:
        def __init__(self, impl: Any) -> None:
            self._impl = impl
            self.fake_fn: Any = None

        def register_fake(self, fn: Any) -> Any:
            self.fake_fn = fn
            return fn

        def __call__(self, *args: Any) -> Any:
            return ("custom_op", self._impl(*args))

    def _fake_custom_op(
        qualname: str,
        fn: Any,
        *,
        mutates_args: tuple[str, ...] = (),
        schema: str = "",
    ) -> _FakeCustomOpHandle:
        del mutates_args
        registered_schemas[qualname] = schema
        return _FakeCustomOpHandle(fn)

    fake_torch = SimpleNamespace(
        compiler=SimpleNamespace(is_compiling=lambda: compiling[0]),
        library=SimpleNamespace(custom_op=_fake_custom_op),
        empty_like=lambda x: ("fake", x.shape),
    )

    monkeypatch.setitem(sys.modules, "torch", fake_torch)
    monkeypatch.setenv(ENV_KERNEL_ARTIFACT_DIR, str(bundle_dir))
    monkeypatch.setenv("GPUSTACK_ENGINE_VERSION", "0.32.0")
    monkeypatch.setenv("GPUSTACK_HARDWARE_MANUFACTURER", "nvidia")
    monkeypatch.setenv("GPUSTACK_HARDWARE_ARCHITECTURE", "sm_90")

    class _FakeRMSNorm:
        def __init__(self) -> None:
            self.weight = "w4096"
            self.variance_epsilon = 1e-6
            self.stock_calls = 0

        def forward_cuda(self, x: Any, residual: Any = None) -> Any:
            self.stock_calls += 1
            return ("stock", x.shape, residual)

    fake_mod = SimpleNamespace(RMSNorm=_FakeRMSNorm)

    try:
        status = kernel_loader.patch_vllm_layernorm_module(fake_mod)
        assert key in status.loaded_kernel_keys
        assert len(registered_schemas) == 1
        assert next(iter(registered_schemas.values())) == (
            "(Tensor x, Tensor weight, float eps) -> Tensor"
        )

        # Sabotage registry.is_override_disabled during compiling trace to prove
        # the lock-based path is NEVER entered inside torch.compile(fullgraph=True).
        reg = kernel_loader.get_process_kernel_registry()
        orig_is_disabled = reg.is_override_disabled

        def _forbid_lock(_k: str) -> bool:
            raise AssertionError(
                "lock-taking is_override_disabled must not run inside compile"
            )

        norm = _FakeRMSNorm()
        t_match = SimpleNamespace(shape=(2, 4096))
        t_other = SimpleNamespace(shape=(2, 128))

        # 1. Under torch.compile with matching hidden_size=4096 and residual=None -> dispatches via custom_op.
        # Note: custom_op impl itself executes at runtime (outside Dynamo), so keep orig_is_disabled during impl.
        compiling[0] = True
        res_match = norm.forward_cuda(t_match)
        assert res_match == ("custom_op", ("forged_rmsnorm", (2, 4096), "w4096", 1e-6))
        assert norm.stock_calls == 0

        # 2. Under torch.compile with residual!=None or non-matching hidden_size -> falls back to stock without touching lock!
        monkeypatch.setattr(reg, "is_override_disabled", _forbid_lock)
        res_resid = norm.forward_cuda(t_match, "res_tensor")
        assert res_resid == ("stock", (2, 4096), "res_tensor")
        res_other = norm.forward_cuda(t_other)
        assert res_other == ("stock", (2, 128), None)
        assert norm.stock_calls == 2

        # 3. Outside torch.compile (eager mode) -> uses normal runtime dispatch path.
        monkeypatch.setattr(reg, "is_override_disabled", orig_is_disabled)
        compiling[0] = False
        res_eager = norm.forward_cuda(t_match)
        assert res_eager == ("forged_rmsnorm", (2, 4096), "w4096", 1e-6)
    finally:
        kernel_loader._REGISTERED_CUSTOM_OPS.clear()
        reset_process_kernel_registry()


def test_sglang_collect_probe_waits_for_partial_trace_flush(tmp_path: Path) -> None:
    import gzip

    trace_dir = tmp_path / "sglang_probe_flush"
    trace_dir.mkdir()

    class _NoIsProfilingTM:
        async def start_profile(self, **_kwargs: Any) -> None:
            return None

    backend = SGLangSentinelBackend(
        SimpleNamespace(
            get_global_state=lambda: SimpleNamespace(
                tokenizer_manager=_NoIsProfilingTM()
            )
        )
    )
    start_res = run_sync(
        backend.start_probe(
            {
                "probe_id": "p-flush",
                "output_dir": str(trace_dir),
                "steps": 4,
                "engine_version": "0.5.22",
                "manufacturer": "nvidia",
                "architecture": "sm_90",
            }
        )
    )
    assert start_res["status"] == "running"

    trace_path = trace_dir / "rank-TP-0.trace.json.gz"

    # 1. Newly opened 0-byte trace file stays running and does NOT purge output_dir.
    trace_path.write_bytes(b"")
    r0 = run_sync(backend.collect_probe({"probe_id": "p-flush"}))
    assert r0["status"] == "running"
    assert trace_path.exists()

    # 2. Truncated partial gzip payload stays running and does NOT purge output_dir.
    full_gz = gzip.compress(
        json.dumps(
            {
                "traceEvents": [
                    {
                        "name": "sglang::rms_norm",
                        "dur": 250.0,
                        "input_dims": [[4, 4096], [4096]],
                        "input_dtypes": ["bfloat16", "bfloat16"],
                    }
                ]
            }
        ).encode("utf-8")
    )
    trace_path.write_bytes(full_gz[: max(1, len(full_gz) // 2)])
    r1 = run_sync(backend.collect_probe({"probe_id": "p-flush"}))
    assert r1["status"] == "running"
    assert trace_path.exists()

    # 3. Once full gzip trace finishes flushing, collect_probe completes and purges output_dir.
    trace_path.write_bytes(full_gz)
    r2 = run_sync(backend.collect_probe({"probe_id": "p-flush"}))
    assert r2["status"] == "completed"
    assert len(r2["payload"]["bottlenecks"]) == 1
    assert r2["payload"]["bottlenecks"][0]["op_family"] == "rmsnorm"
    assert not trace_dir.exists()


def test_probe_request_rejects_malformed_version_and_arch_and_worker_finalizer_guards_exceptions(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    import stat

    from gpustack_engine_sentinel.probe import compute_signature_hash16
    from gpustack_engine_sentinel.protocol import InvalidRequestError, ProbeRequest
    from gpustack_engine_sentinel.vllm.utility import vllm_worker_probe_start

    monkeypatch.setenv(ENV_PROBE_MIN_COOLDOWN_SECONDS, "0")

    for bad_body in (
        {"expected_boot_id": "boot-1", "engine_version": "abc!!"},
        {"expected_boot_id": "boot-1", "architecture": "sm 90 x"},
        {"expected_boot_id": "boot-1", "manufacturer": "nv/idia"},
    ):
        with pytest.raises(InvalidRequestError):
            ProbeRequest.from_dict(bad_body)

    # Staging directories are created with owner-only 0o700 permissions.
    stage_dir = tmp_path / "worker_stage_0700"
    worker = _FakeGPUWorker(
        [
            SimpleNamespace(
                key="vllm::rms_norm",
                self_device_time_total=120.0,
                input_shapes=[[8, 4096], [4096]],
                input_dtypes=["bfloat16", "bfloat16"],
                count=3,
            )
        ],
        rank=0,
    )
    res = vllm_worker_probe_start(
        worker,
        {
            "probe_id": "p-err-guard",
            "output_dir": str(stage_dir),
            "steps": 3,
            "engine_version": "abc!!",
            "manufacturer": "nvidia",
            "architecture": "sm_90a",
        },
    )
    assert res["ok"] is True
    assert stat.S_IMODE(stage_dir.stat().st_mode) == 0o700

    # Even if malformed engine_version reaches the worker session directly, execute_model
    # must NOT raise: probe finalization records status="failed" and restores execute_model.
    for i in range(3):
        worker.execute_model(batch=i)
    session = worker._gpustack_probe_session
    assert session.status == "failed"
    assert session.error_code == "probe_distill_failed"
    assert not stage_dir.exists()

    # Cross-language hash parity: c10:: dtype prefix, whole-number float 1.0 == 1, U+2028/U+2029,
    # and fail-closed rejection of non-finite float constants mirroring Go's encoding/json.
    h1 = compute_signature_hash16(
        {"hidden_size": 4096},
        ["torch.float16"],
        ["float16"],
        constants={"scale": 1.0, "sep": "a\u2028b\u2029c"},
    )
    h2 = compute_signature_hash16(
        {"hidden_size": 4096},
        ["c10::float16"],
        ["c10::float16"],
        constants={"scale": 1, "sep": "a\u2028b\u2029c"},
    )
    assert h1 == h2
    for bad_val in (float("nan"), float("inf"), float("-inf")):
        with pytest.raises(ValueError, match="finite"):
            compute_signature_hash16(
                {"hidden_size": 4096},
                ["float16"],
                ["float16"],
                constants={"scale": bad_val},
            )
