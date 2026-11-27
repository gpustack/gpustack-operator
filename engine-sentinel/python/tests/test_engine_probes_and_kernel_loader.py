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
        assert payload["engine_version"] == "0.8.5"
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
        assert payload["engine_version"] == "0.4.6"
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
