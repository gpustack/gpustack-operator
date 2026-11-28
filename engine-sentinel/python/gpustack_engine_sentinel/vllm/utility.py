"""EngineCore multi-rank utility RPC for vLLM online scheduler controls."""

from __future__ import annotations

import logging
import time
import uuid
from dataclasses import dataclass, field
from functools import wraps
from pathlib import Path
from typing import Any

from gpustack_engine_sentinel.kernel_loader import (
    aggregate_kernel_reports,
    get_process_kernel_report,
)
from gpustack_engine_sentinel.probe import (
    directory_size_bytes,
    distill_trace_events,
    load_chrome_trace_events,
    purge_probe_dir,
    select_rank0_trace_file,
)
from gpustack_engine_sentinel.protocol import (
    FIELD_CACHE_AWARE_THRESHOLD,
    FIELD_CACHE_AWARE_WINDOW,
    FIELD_LONG_PREFILL_TOKEN_THRESHOLD,
    FIELD_MAX_ACTIVE_REQUESTS,
    FIELD_MAX_ASYNC_KV_LOADS_PER_STEP,
    FIELD_MAX_BATCHED_TOKENS,
    FIELD_MAX_INFLIGHT_KV_LOADS,
    MAX_PROBE_STEPS,
    MIN_PROBE_STEPS,
    PROBE_MAX_SHM_BYTES,
    PROBE_WALL_CLOCK_CAP_SECONDS,
    VLLM_ALL_FIELDS,
    InvalidRequestError,
    parse_decimal_float,
    parse_strict_int,
)

LOGGER = logging.getLogger(__name__)
UTILITY_METHOD_NAME = "gpustack_engine_sentinel_rpc"
_SENTINEL = object()


@dataclass
class _VLLMRankState:
    boot_id: str = field(default_factory=lambda: str(uuid.uuid4()))
    membership_epoch: int = 1
    last_dp_size: int | None = None
    applied_digest: str = ""
    boot_defaults: dict[str, Any] = field(default_factory=dict)


def _is_fcfs_policy(scheduler: Any, scheduler_config: Any) -> bool:
    sched_policy = getattr(scheduler, "policy", None)
    if sched_policy is not None:
        val = getattr(sched_policy, "value", sched_policy)
        return str(val).lower().endswith("fcfs")
    cfg_policy = getattr(scheduler_config, "policy", "fcfs")
    return str(cfg_policy).lower() == "fcfs"


def _probe_vllm_capabilities(
    scheduler: Any, scheduler_config: Any
) -> tuple[list[str], list[str]]:
    supported: list[str] = []
    unsupported: list[str] = []

    if (
        scheduler is not None
        and hasattr(scheduler, "max_num_active_reqs")
        and hasattr(scheduler, "max_num_running_reqs")
    ):
        supported.append(FIELD_MAX_ACTIVE_REQUESTS)
    else:
        unsupported.append(FIELD_MAX_ACTIVE_REQUESTS)

    chunked_enabled = (
        getattr(scheduler_config, "enable_chunked_prefill", True) is not False
    )
    if (
        scheduler is not None
        and hasattr(scheduler, "max_num_scheduled_tokens")
        and chunked_enabled
    ):
        supported.append(FIELD_MAX_BATCHED_TOKENS)
    else:
        unsupported.append(FIELD_MAX_BATCHED_TOKENS)

    if (
        scheduler_config is not None
        and hasattr(scheduler_config, "long_prefill_token_threshold")
        and not bool(getattr(scheduler, "is_encoder_decoder", False))
    ):
        supported.append(FIELD_LONG_PREFILL_TOKEN_THRESHOLD)
    else:
        unsupported.append(FIELD_LONG_PREFILL_TOKEN_THRESHOLD)

    prefix_caching = bool(
        getattr(getattr(scheduler, "cache_config", None), "enable_prefix_caching", True)
    )
    fcfs = _is_fcfs_policy(scheduler, scheduler_config)

    if (
        scheduler is not None
        and scheduler_config is not None
        and hasattr(scheduler, "cache_aware_window")
        and hasattr(scheduler_config, "cache_aware_admission_window")
        and prefix_caching
        and fcfs
    ):
        supported.append(FIELD_CACHE_AWARE_WINDOW)
    else:
        unsupported.append(FIELD_CACHE_AWARE_WINDOW)

    if (
        scheduler is not None
        and scheduler_config is not None
        and hasattr(scheduler, "cache_aware_threshold")
        and hasattr(scheduler_config, "cache_aware_admission_threshold")
        and prefix_caching
        and fcfs
    ):
        supported.append(FIELD_CACHE_AWARE_THRESHOLD)
    else:
        unsupported.append(FIELD_CACHE_AWARE_THRESHOLD)

    has_connector = getattr(scheduler, "connector", None) is not None
    has_fetch_stages = getattr(scheduler, "_kv_fetch_stages", None) is not None
    if (
        scheduler_config is not None
        and hasattr(scheduler_config, "max_num_inflight_kv_loads")
        and has_connector
        and has_fetch_stages
    ):
        supported.append(FIELD_MAX_INFLIGHT_KV_LOADS)
    else:
        unsupported.append(FIELD_MAX_INFLIGHT_KV_LOADS)

    if (
        scheduler_config is not None
        and hasattr(scheduler_config, "max_async_kv_loads_per_step")
        and has_connector
    ):
        supported.append(FIELD_MAX_ASYNC_KV_LOADS_PER_STEP)
    else:
        unsupported.append(FIELD_MAX_ASYNC_KV_LOADS_PER_STEP)

    for f in VLLM_ALL_FIELDS:
        if f not in supported and f not in unsupported:
            unsupported.append(f)
    return sorted(supported), sorted(unsupported)


def _ensure_rank_state(engine_core: Any) -> _VLLMRankState:
    state: _VLLMRankState | None = getattr(
        engine_core, "_gpustack_sentinel_state", None
    )
    if state is None:
        state = _VLLMRankState()
        engine_core._gpustack_sentinel_state = state

    vllm_cfg = getattr(engine_core, "vllm_config", None)
    parallel_cfg = getattr(vllm_cfg, "parallel_config", None)
    dp_size = int(getattr(parallel_cfg, "data_parallel_size", 1) or 1)
    if state.last_dp_size is None:
        state.last_dp_size = dp_size
    elif state.last_dp_size != dp_size:
        state.membership_epoch += 1
        state.last_dp_size = dp_size

    scheduler = getattr(engine_core, "scheduler", None)
    sched_cfg = getattr(scheduler, "scheduler_config", None) or getattr(
        vllm_cfg, "scheduler_config", None
    )
    if not state.boot_defaults and scheduler is not None:
        defaults: dict[str, Any] = {}
        for attr in (
            "max_num_running_reqs",
            "max_num_active_reqs",
            "max_num_scheduled_tokens",
            "cache_aware_window",
            "cache_aware_threshold",
        ):
            if hasattr(scheduler, attr):
                defaults[f"sched.{attr}"] = getattr(scheduler, attr)
        if sched_cfg is not None:
            for attr in (
                "max_num_active_seqs",
                "max_num_scheduled_tokens",
                "long_prefill_token_threshold",
                "cache_aware_admission_window",
                "cache_aware_admission_threshold",
                "max_num_inflight_kv_loads",
                "max_async_kv_loads_per_step",
            ):
                if hasattr(sched_cfg, attr):
                    defaults[f"cfg.{attr}"] = getattr(sched_cfg, attr)
        state.boot_defaults = defaults

    return state


def _capture_snapshot(scheduler: Any, sched_cfg: Any) -> dict[str, Any]:
    snap: dict[str, Any] = {}
    if scheduler is not None:
        for attr in (
            "max_num_active_reqs",
            "max_num_scheduled_tokens",
            "cache_aware_window",
            "cache_aware_threshold",
        ):
            if hasattr(scheduler, attr):
                snap[f"sched.{attr}"] = getattr(scheduler, attr)
    if sched_cfg is not None:
        for attr in (
            "max_num_active_seqs",
            "max_num_scheduled_tokens",
            "long_prefill_token_threshold",
            "cache_aware_admission_window",
            "cache_aware_admission_threshold",
            "max_num_inflight_kv_loads",
            "max_async_kv_loads_per_step",
        ):
            if hasattr(sched_cfg, attr):
                snap[f"cfg.{attr}"] = getattr(sched_cfg, attr)
    return snap


def _restore_snapshot(scheduler: Any, sched_cfg: Any, snap: dict[str, Any]) -> None:
    for key, val in snap.items():
        prefix, attr = key.split(".", 1)
        if prefix == "sched" and scheduler is not None and hasattr(scheduler, attr):
            setattr(scheduler, attr, val)
        elif prefix == "cfg" and sched_cfg is not None and hasattr(sched_cfg, attr):
            setattr(sched_cfg, attr, val)


def _read_canonical_boot_defaults(
    defaults: dict[str, Any], supported: list[str]
) -> dict[str, Any]:
    out: dict[str, Any] = {}
    supported_set = set(supported)
    if FIELD_MAX_ACTIVE_REQUESTS in supported_set and (
        "sched.max_num_running_reqs" in defaults
    ):
        out[FIELD_MAX_ACTIVE_REQUESTS] = int(defaults["sched.max_num_running_reqs"])
    if FIELD_MAX_BATCHED_TOKENS in supported_set and (
        "sched.max_num_scheduled_tokens" in defaults
    ):
        out[FIELD_MAX_BATCHED_TOKENS] = int(defaults["sched.max_num_scheduled_tokens"])
    if FIELD_LONG_PREFILL_TOKEN_THRESHOLD in supported_set and (
        "cfg.long_prefill_token_threshold" in defaults
    ):
        out[FIELD_LONG_PREFILL_TOKEN_THRESHOLD] = defaults[
            "cfg.long_prefill_token_threshold"
        ]
    if FIELD_CACHE_AWARE_WINDOW in supported_set and (
        "sched.cache_aware_window" in defaults
    ):
        out[FIELD_CACHE_AWARE_WINDOW] = defaults["sched.cache_aware_window"]
    if FIELD_CACHE_AWARE_THRESHOLD in supported_set and (
        "sched.cache_aware_threshold" in defaults
    ):
        out[FIELD_CACHE_AWARE_THRESHOLD] = defaults["sched.cache_aware_threshold"]
    if FIELD_MAX_INFLIGHT_KV_LOADS in supported_set and (
        "cfg.max_num_inflight_kv_loads" in defaults
    ):
        out[FIELD_MAX_INFLIGHT_KV_LOADS] = defaults["cfg.max_num_inflight_kv_loads"]
    if FIELD_MAX_ASYNC_KV_LOADS_PER_STEP in supported_set and (
        "cfg.max_async_kv_loads_per_step" in defaults
    ):
        out[FIELD_MAX_ASYNC_KV_LOADS_PER_STEP] = defaults[
            "cfg.max_async_kv_loads_per_step"
        ]
    return out


def _read_current_values(
    scheduler: Any, sched_cfg: Any, supported: list[str]
) -> dict[str, Any]:
    values: dict[str, Any] = {}
    supported_set = set(supported)
    if FIELD_MAX_ACTIVE_REQUESTS in supported_set:
        values[FIELD_MAX_ACTIVE_REQUESTS] = getattr(
            scheduler, "max_num_active_reqs", None
        )
    if FIELD_MAX_BATCHED_TOKENS in supported_set:
        values[FIELD_MAX_BATCHED_TOKENS] = getattr(
            scheduler, "max_num_scheduled_tokens", None
        )
    if FIELD_LONG_PREFILL_TOKEN_THRESHOLD in supported_set:
        values[FIELD_LONG_PREFILL_TOKEN_THRESHOLD] = getattr(
            sched_cfg, "long_prefill_token_threshold", None
        )
    if FIELD_CACHE_AWARE_WINDOW in supported_set:
        values[FIELD_CACHE_AWARE_WINDOW] = getattr(
            scheduler, "cache_aware_window", None
        )
    if FIELD_CACHE_AWARE_THRESHOLD in supported_set:
        values[FIELD_CACHE_AWARE_THRESHOLD] = getattr(
            scheduler, "cache_aware_threshold", None
        )
    if FIELD_MAX_INFLIGHT_KV_LOADS in supported_set:
        values[FIELD_MAX_INFLIGHT_KV_LOADS] = getattr(
            sched_cfg, "max_num_inflight_kv_loads", None
        )
    if FIELD_MAX_ASYNC_KV_LOADS_PER_STEP in supported_set:
        values[FIELD_MAX_ASYNC_KV_LOADS_PER_STEP] = getattr(
            sched_cfg, "max_async_kv_loads_per_step", None
        )
    return values


def _rank_id_of(engine_core: Any) -> int:
    vllm_cfg = getattr(engine_core, "vllm_config", None)
    parallel_cfg = getattr(vllm_cfg, "parallel_config", None)
    rank = getattr(parallel_cfg, "data_parallel_rank", None)
    if rank is None:
        rank = getattr(parallel_cfg, "data_parallel_index", 0)
    return int(rank or 0)


def _reset_one_field(
    canonical: str,
    scheduler: Any,
    sched_cfg: Any,
    defaults: dict[str, Any],
) -> None:
    if canonical == FIELD_MAX_ACTIVE_REQUESTS:
        if "sched.max_num_active_reqs" in defaults:
            scheduler.max_num_active_reqs = defaults["sched.max_num_active_reqs"]
        if sched_cfg is not None and "cfg.max_num_active_seqs" in defaults:
            sched_cfg.max_num_active_seqs = defaults["cfg.max_num_active_seqs"]
    elif canonical == FIELD_MAX_BATCHED_TOKENS:
        if "sched.max_num_scheduled_tokens" in defaults:
            scheduler.max_num_scheduled_tokens = defaults[
                "sched.max_num_scheduled_tokens"
            ]
        if sched_cfg is not None and "cfg.max_num_scheduled_tokens" in defaults:
            sched_cfg.max_num_scheduled_tokens = defaults[
                "cfg.max_num_scheduled_tokens"
            ]
    elif canonical == FIELD_LONG_PREFILL_TOKEN_THRESHOLD:
        if sched_cfg is not None and "cfg.long_prefill_token_threshold" in defaults:
            sched_cfg.long_prefill_token_threshold = defaults[
                "cfg.long_prefill_token_threshold"
            ]
    elif canonical == FIELD_CACHE_AWARE_WINDOW:
        if "sched.cache_aware_window" in defaults:
            scheduler.cache_aware_window = defaults["sched.cache_aware_window"]
        if sched_cfg is not None and "cfg.cache_aware_admission_window" in defaults:
            sched_cfg.cache_aware_admission_window = defaults[
                "cfg.cache_aware_admission_window"
            ]
    elif canonical == FIELD_CACHE_AWARE_THRESHOLD:
        if "sched.cache_aware_threshold" in defaults:
            scheduler.cache_aware_threshold = defaults["sched.cache_aware_threshold"]
        if sched_cfg is not None and "cfg.cache_aware_admission_threshold" in defaults:
            sched_cfg.cache_aware_admission_threshold = defaults[
                "cfg.cache_aware_admission_threshold"
            ]
    elif canonical == FIELD_MAX_INFLIGHT_KV_LOADS:
        if sched_cfg is not None and "cfg.max_num_inflight_kv_loads" in defaults:
            sched_cfg.max_num_inflight_kv_loads = defaults[
                "cfg.max_num_inflight_kv_loads"
            ]
    elif (
        canonical == FIELD_MAX_ASYNC_KV_LOADS_PER_STEP
        and sched_cfg is not None
        and "cfg.max_async_kv_loads_per_step" in defaults
    ):
        sched_cfg.max_async_kv_loads_per_step = defaults[
            "cfg.max_async_kv_loads_per_step"
        ]


@dataclass
class _WorkerProbeSession:
    probe_id: str
    boot_id: str
    target_steps: int
    output_dir: Path
    max_shm_bytes: int
    wall_clock_cap_seconds: float
    engine_version: str
    manufacturer: str
    architecture: str
    started_monotonic: float
    orig_execute_model: Any
    steps_completed: int = 0
    observed_tokens: int = 0
    status: str = "running"
    profiler: Any = None
    profiler_entered: bool = False
    payload: dict[str, Any] | None = None
    error_code: str | None = None
    error_message: str | None = None


def _create_default_torch_profiler() -> Any:
    """Lazily import torch.profiler and configure CPU + accelerator activities."""
    import torch

    activities = [torch.profiler.ProfilerActivity.CPU]
    cuda_mod = getattr(torch, "cuda", None)
    if (
        cuda_mod is not None
        and callable(getattr(cuda_mod, "is_available", None))
        and cuda_mod.is_available()
    ):
        activities.append(torch.profiler.ProfilerActivity.CUDA)
    xpu_mod = getattr(torch, "xpu", None)
    if (
        hasattr(torch.profiler.ProfilerActivity, "XPU")
        and xpu_mod is not None
        and callable(getattr(xpu_mod, "is_available", None))
        and xpu_mod.is_available()
    ):
        activities.append(torch.profiler.ProfilerActivity.XPU)
    npu_mod = getattr(torch, "npu", None)
    if (
        hasattr(torch.profiler.ProfilerActivity, "PrivateUse1")
        and npu_mod is not None
        and callable(getattr(npu_mod, "is_available", None))
        and npu_mod.is_available()
    ):
        activities.append(torch.profiler.ProfilerActivity.PrivateUse1)
    return torch.profiler.profile(
        activities=activities,
        record_shapes=True,
    )


def _extract_profiler_events(prof: Any, output_dir: Path) -> list[dict[str, Any]]:
    """Extract normalized privacy-safe events from a completed profiler instance."""
    events: list[dict[str, Any]] = []
    if prof is not None:
        key_averages_fn = getattr(prof, "key_averages", None)
        raw_items: Any = None
        if callable(key_averages_fn):
            try:
                raw_items = key_averages_fn(group_by_input_shape=True)
            except TypeError:
                raw_items = key_averages_fn()
        elif callable(getattr(prof, "events", None)):
            raw_items = prof.events()

        if raw_items is not None:
            for item in raw_items:
                if isinstance(item, dict):
                    events.append(item)
                    continue
                name = getattr(item, "key", None) or getattr(item, "name", None)
                if not isinstance(name, str) or not name.strip():
                    continue
                dur = (
                    getattr(item, "self_device_time_total", None)
                    or getattr(item, "self_cuda_time_total", None)
                    or getattr(item, "device_time_total", None)
                    or getattr(item, "cuda_time_total", None)
                    or getattr(item, "self_cpu_time_total", None)
                    or 0.0
                )
                shapes = getattr(item, "input_shapes", None) or []
                dtypes = getattr(item, "input_dtypes", None) or []
                count = getattr(item, "count", 1) or 1
                events.append(
                    {
                        "name": name,
                        "dur": float(dur),
                        "input_dims": list(shapes)
                        if isinstance(shapes, (list, tuple))
                        else [],
                        "input_dtypes": list(dtypes)
                        if isinstance(dtypes, (list, tuple))
                        else [],
                        "count": int(count),
                    }
                )

    if not events:
        trace_file = select_rank0_trace_file(output_dir)
        if trace_file is not None:
            events.extend(load_chrome_trace_events(trace_file))
    return events


def _restore_worker_execute_model(worker: Any, session: _WorkerProbeSession) -> None:
    if getattr(worker, "execute_model", None) is not session.orig_execute_model:
        worker.execute_model = session.orig_execute_model


def _extract_worker_model_dims(worker: Any, observed_tokens: int) -> dict[str, Any]:
    """Extract static per-GPU model dimensions from a vLLM worker for CUDA-graph fallback."""
    vllm_cfg = getattr(worker, "vllm_config", None)
    model_cfg = getattr(worker, "model_config", None) or getattr(
        vllm_cfg, "model_config", None
    )
    parallel_cfg = getattr(worker, "parallel_config", None) or getattr(
        vllm_cfg, "parallel_config", None
    )
    if model_cfg is None:
        return {}

    dims: dict[str, Any] = {"num_tokens": max(1, int(observed_tokens or 1))}
    tp_size = 1
    if parallel_cfg is not None:
        raw_tp = getattr(parallel_cfg, "tensor_parallel_size", None)
        if isinstance(raw_tp, int) and not isinstance(raw_tp, bool) and raw_tp > 0:
            tp_size = raw_tp

    def _call_or_attr(obj: Any, method_name: str, attr_name: str, *args: Any) -> int:
        fn = getattr(obj, method_name, None)
        if callable(fn):
            try:
                val = fn(*args)
                if isinstance(val, int) and not isinstance(val, bool) and val > 0:
                    return val
            except Exception:
                pass
        val = getattr(obj, attr_name, None)
        if isinstance(val, int) and not isinstance(val, bool) and val > 0:
            return val
        return 0

    hidden = _call_or_attr(model_cfg, "get_hidden_size", "hidden_size")
    if hidden > 0:
        dims["hidden_size"] = hidden

    head_dim = _call_or_attr(model_cfg, "get_head_size", "head_size")
    if head_dim > 0:
        dims["head_dim"] = head_dim

    if parallel_cfg is not None:
        num_heads = _call_or_attr(
            model_cfg, "get_num_attention_heads", "num_attention_heads", parallel_cfg
        )
        if num_heads > 0:
            dims["num_heads_local"] = num_heads

    vocab = _call_or_attr(model_cfg, "get_vocab_size", "vocab_size")
    if vocab > 0:
        dims["vocab_size_local"] = max(1, vocab // tp_size)

    hf_cfg = getattr(model_cfg, "hf_config", None)
    if hf_cfg is not None:
        inter = getattr(hf_cfg, "intermediate_size", None)
        if isinstance(inter, int) and not isinstance(inter, bool) and inter > 0:
            dims["intermediate_size_local"] = max(1, inter // tp_size)
        for exp_attr in ("num_local_experts", "num_experts", "n_routed_experts"):
            n_exp = getattr(hf_cfg, exp_attr, None)
            if isinstance(n_exp, int) and not isinstance(n_exp, bool) and n_exp > 0:
                dims["num_experts_local"] = n_exp
                break

    dtype = getattr(model_cfg, "dtype", None)
    if dtype is not None:
        dims["dtype"] = str(dtype)
    return dims


def _extract_scheduled_tokens(args: tuple[Any, ...], kwargs: dict[str, Any]) -> int:
    sched_out = args[0] if args else kwargs.get("scheduler_output")
    if sched_out is None:
        return 0
    total = getattr(sched_out, "total_num_scheduled_tokens", None)
    if isinstance(total, int) and not isinstance(total, bool) and total > 0:
        return total
    per_req = getattr(sched_out, "num_scheduled_tokens", None)
    if isinstance(per_req, dict):
        s = sum(
            v
            for v in per_req.values()
            if isinstance(v, int) and not isinstance(v, bool)
        )
        if s > 0:
            return s
    return 0


def _finalize_worker_probe(
    worker: Any,
    session: _WorkerProbeSession,
    *,
    aborted: bool = False,
    error_code: str | None = None,
    error_message: str | None = None,
) -> None:
    _restore_worker_execute_model(worker, session)
    raw_bytes = directory_size_bytes(session.output_dir)
    try:
        if session.profiler is not None and session.profiler_entered:
            try:
                session.profiler.__exit__(None, None, None)
            finally:
                session.profiler_entered = False

        if aborted:
            session.status = "aborted"
            session.error_code = error_code or "probe_aborted"
            session.error_message = error_message or "Probe aborted"
            return

        if error_code is not None:
            session.status = "failed"
            session.error_code = error_code
            session.error_message = error_message or error_code
            return

        events = _extract_profiler_events(session.profiler, session.output_dir)
        wall = max(0.0, time.monotonic() - session.started_monotonic)
        model_dims = _extract_worker_model_dims(worker, session.observed_tokens)
        session.payload = distill_trace_events(
            events,
            engine="vLLM",
            engine_version=session.engine_version,
            manufacturer=session.manufacturer,
            architecture=session.architecture,
            steps_captured=session.steps_completed,
            raw_bytes_staged=raw_bytes,
            wall_clock_seconds=wall,
            model_dims=model_dims or None,
        )
        session.status = "completed"
    finally:
        purge_probe_dir(session.output_dir)


def _worker_tp_rank(worker: Any) -> int:
    for attr in ("rank", "local_rank"):
        val = getattr(worker, attr, None)
        if isinstance(val, int) and not isinstance(val, bool):
            return val
    return 0


def vllm_worker_probe_start(worker: Any, spec: dict[str, Any]) -> dict[str, Any]:
    """Rank-0 GPU worker callback: arm step-counted torch.profiler around execute_model."""
    # collective_rpc broadcasts to all TP workers; only rank 0 profiles.
    if _worker_tp_rank(worker) != 0:
        return {"ok": True, "skipped": True}

    orig_exec = getattr(worker, "execute_model", None)
    if not callable(orig_exec):
        return {
            "ok": False,
            "error_code": "unsupported_engine_version",
            "message": "vLLM rank-0 worker does not expose callable execute_model",
        }

    existing: _WorkerProbeSession | None = getattr(
        worker, "_gpustack_probe_session", None
    )
    if existing is not None and existing.status == "running":
        return {
            "ok": False,
            "error_code": "probe_in_progress",
            "message": f"Probe {existing.probe_id} is already running on rank-0 worker",
        }

    target_steps = max(
        MIN_PROBE_STEPS,
        min(MAX_PROBE_STEPS, int(spec.get("steps", 4))),
    )
    output_dir = Path(str(spec.get("output_dir") or ""))
    output_dir.mkdir(parents=True, exist_ok=True)
    session = _WorkerProbeSession(
        probe_id=str(spec["probe_id"]),
        boot_id=str(spec.get("boot_id", "")),
        target_steps=target_steps,
        output_dir=output_dir,
        max_shm_bytes=int(spec.get("max_shm_bytes") or PROBE_MAX_SHM_BYTES),
        wall_clock_cap_seconds=float(
            spec.get("wall_clock_cap_seconds") or PROBE_WALL_CLOCK_CAP_SECONDS
        ),
        engine_version=str(spec.get("engine_version") or "0.0.0"),
        manufacturer=str(spec.get("manufacturer") or "nvidia"),
        architecture=str(spec.get("architecture") or "unknown"),
        started_monotonic=time.monotonic(),
        orig_execute_model=orig_exec,
    )

    profiler_factory = getattr(worker, "_gpustack_profiler_factory", None)
    if not callable(profiler_factory):
        profiler_factory = _create_default_torch_profiler

    @wraps(orig_exec)
    def _step_counted_execute_model(*args: Any, **kwargs: Any) -> Any:
        if session.status != "running":
            return orig_exec(*args, **kwargs)

        elapsed = time.monotonic() - session.started_monotonic
        if elapsed > session.wall_clock_cap_seconds:
            _finalize_worker_probe(
                worker,
                session,
                error_code="probe_timeout",
                error_message=f"Probe exceeded {session.wall_clock_cap_seconds}s wall-clock cap",
            )
            return orig_exec(*args, **kwargs)

        if not session.profiler_entered:
            try:
                session.profiler = profiler_factory()
                session.profiler.__enter__()
                session.profiler_entered = True
            except Exception as exc:
                _finalize_worker_probe(
                    worker,
                    session,
                    error_code="unsupported_engine_version",
                    error_message=f"Failed to start torch profiler: {exc}",
                )
                return orig_exec(*args, **kwargs)

        try:
            sched_toks = _extract_scheduled_tokens(args, kwargs)
            if sched_toks > session.observed_tokens:
                session.observed_tokens = sched_toks
            result = orig_exec(*args, **kwargs)
        finally:
            if session.status == "running":
                session.steps_completed += 1
                step_fn = getattr(session.profiler, "step", None)
                if callable(step_fn):
                    step_fn()

                staged_bytes = directory_size_bytes(session.output_dir)
                if staged_bytes > session.max_shm_bytes:
                    _finalize_worker_probe(
                        worker,
                        session,
                        error_code="probe_shm_budget_exceeded",
                        error_message=(
                            f"Probe staging exceeded {session.max_shm_bytes} byte ceiling"
                        ),
                    )
                elif session.steps_completed >= session.target_steps:
                    _finalize_worker_probe(worker, session)
        return result

    worker._gpustack_probe_session = session
    worker.execute_model = _step_counted_execute_model
    return {
        "ok": True,
        "status": "running",
        "probe_id": session.probe_id,
        "steps": session.target_steps,
    }


def vllm_worker_probe_collect(worker: Any, probe_id: str) -> dict[str, Any]:
    """Rank-0 GPU worker callback: read current step-counted probe state and distilled payload."""
    if _worker_tp_rank(worker) != 0:
        return {"ok": True, "skipped": True}
    session: _WorkerProbeSession | None = getattr(
        worker, "_gpustack_probe_session", None
    )
    if session is None or (probe_id and session.probe_id != probe_id):
        return {
            "ok": True,
            "status": "idle",
            "probe_id": probe_id,
        }
    if session.status == "running":
        elapsed = time.monotonic() - session.started_monotonic
        if elapsed > session.wall_clock_cap_seconds:
            _finalize_worker_probe(
                worker,
                session,
                error_code="probe_timeout",
                error_message=f"Probe exceeded {session.wall_clock_cap_seconds}s wall-clock cap",
            )
        elif directory_size_bytes(session.output_dir) > session.max_shm_bytes:
            _finalize_worker_probe(
                worker,
                session,
                error_code="probe_shm_budget_exceeded",
                error_message=f"Probe staging exceeded {session.max_shm_bytes} byte ceiling",
            )
    out: dict[str, Any] = {
        "ok": True,
        "status": session.status,
        "probe_id": session.probe_id,
        "steps_completed": session.steps_completed,
    }
    if session.payload is not None:
        out["payload"] = session.payload
    if session.error_code is not None:
        out["error_code"] = session.error_code
    if session.error_message is not None:
        out["message"] = session.error_message
    return out


def vllm_worker_probe_abort(worker: Any, probe_id: str) -> dict[str, Any]:
    """Rank-0 GPU worker callback: abort active probe, restore execute_model, and purge staging."""
    if _worker_tp_rank(worker) != 0:
        return {"ok": True, "skipped": True}
    session: _WorkerProbeSession | None = getattr(
        worker, "_gpustack_probe_session", None
    )
    if session is not None and (not probe_id or session.probe_id == probe_id):
        if session.status == "running":
            _finalize_worker_probe(worker, session, aborted=True)
        else:
            _restore_worker_execute_model(worker, session)
            purge_probe_dir(session.output_dir)
    return {
        "ok": True,
        "status": "aborted",
        "probe_id": probe_id,
    }


def vllm_worker_kernel_report(worker: Any) -> dict[str, Any]:
    """GPU worker callback: report loaded forged kernel metadata if configured."""
    worker_report_fn = getattr(worker, "_gpustack_kernel_report", None)
    if callable(worker_report_fn):
        res = worker_report_fn()
        if isinstance(res, dict):
            return res
    worker_reg = getattr(worker, "_gpustack_kernel_registry", None)
    if worker_reg is not None and hasattr(worker_reg, "status"):
        st = worker_reg.status
        return st.to_report() if st.configured else {}
    return get_process_kernel_report(engine="vLLM")


def _resolve_tp_size(engine_core: Any) -> int:
    vllm_cfg = getattr(engine_core, "vllm_config", None)
    parallel_cfg = getattr(vllm_cfg, "parallel_config", None)
    return max(1, int(getattr(parallel_cfg, "tensor_parallel_size", 1) or 1))


def _dispatch_rank0_worker(engine_core: Any, fn: Any, *args: Any) -> dict[str, Any]:
    """Dispatch a callable to rank-0 GPU worker via collective_rpc(..., unique_reply_rank=0) or TP=1 in-process."""
    model_executor = getattr(engine_core, "model_executor", None)
    collective_rpc = getattr(model_executor, "collective_rpc", None)
    if callable(collective_rpc):
        try:
            res = collective_rpc(fn, args=args, unique_reply_rank=0)
        except TypeError:
            try:
                res = collective_rpc(fn, args=args)
            except Exception as exc:
                return {
                    "ok": False,
                    "error_code": "unsupported_engine_version",
                    "message": f"vLLM collective_rpc invocation incompatible: {exc}",
                }
        except Exception as exc:
            return {
                "ok": False,
                "error_code": "probe_worker_error",
                "message": str(exc),
            }
        if isinstance(res, list):
            if not res:
                return {
                    "ok": False,
                    "error_code": "unsupported_engine_version",
                    "message": "vLLM collective_rpc returned empty worker list",
                }
            res = res[0]
        if isinstance(res, dict):
            return res
        return {"ok": True, "result": res}

    # TP=1 in-process fallback when model_executor exposes local worker directly.
    if _resolve_tp_size(engine_core) == 1:
        for container in (model_executor, engine_core):
            if container is None:
                continue
            for attr in ("driver_worker", "worker"):
                worker = getattr(container, attr, None)
                if worker is not None and callable(
                    getattr(worker, "execute_model", None)
                ):
                    res = fn(worker, *args)
                    return res if isinstance(res, dict) else {"ok": True, "result": res}

    return {
        "ok": False,
        "error_code": "unsupported_engine_version",
        "message": "vLLM EngineCore lacks model_executor.collective_rpc and local rank-0 worker",
    }


def _has_kernel_report_fields(report: Any) -> bool:
    return isinstance(report, dict) and any(
        k in report
        for k in (
            "loaded_kernel_keys",
            "dispatch_manifest_digest",
            "kernel_load_error",
        )
    )


def _collect_rank_kernel_report(engine_core: Any) -> dict[str, Any]:
    local_report = get_process_kernel_report(engine="vLLM")
    reports: list[dict[str, Any]] = []
    if _has_kernel_report_fields(local_report):
        reports.append(local_report)

    model_executor = getattr(engine_core, "model_executor", None)
    if model_executor is not None:
        collective_rpc = getattr(model_executor, "collective_rpc", None)
        if callable(collective_rpc):
            raw_res: Any = None
            try:
                raw_res = collective_rpc(vllm_worker_kernel_report, args=())
            except TypeError:
                try:
                    raw_res = collective_rpc(vllm_worker_kernel_report)
                except Exception:
                    raw_res = None
            except Exception:
                raw_res = None

            if isinstance(raw_res, (list, tuple)):
                for item in raw_res:
                    if _has_kernel_report_fields(item):
                        reports.append(item)
            elif _has_kernel_report_fields(raw_res):
                reports.append(raw_res)
        elif _resolve_tp_size(engine_core) == 1:
            for container in (model_executor, engine_core):
                if container is None:
                    continue
                found_local = False
                for attr in ("driver_worker", "worker"):
                    worker = getattr(container, attr, None)
                    if worker is not None and callable(
                        getattr(worker, "execute_model", None)
                    ):
                        w_rep = vllm_worker_kernel_report(worker)
                        if _has_kernel_report_fields(w_rep):
                            reports.append(w_rep)
                        found_local = True
                        break
                if found_local:
                    break

    if reports:
        return aggregate_kernel_reports(reports)
    return local_report


def execute_engine_core_rpc(
    engine_core: Any, payload: dict[str, Any]
) -> dict[str, Any]:
    """Execute inspect, apply, rollback, or probe RPCs inside a vLLM EngineCore process."""
    state = _ensure_rank_state(engine_core)
    scheduler = getattr(engine_core, "scheduler", None)
    vllm_cfg = getattr(engine_core, "vllm_config", None)
    sched_cfg = getattr(scheduler, "scheduler_config", None) or getattr(
        vllm_cfg, "scheduler_config", None
    )
    supported, unsupported = _probe_vllm_capabilities(scheduler, sched_cfg)
    rank_id = _rank_id_of(engine_core)
    op = payload.get("op", "inspect")

    if op == "inspect":
        resp: dict[str, Any] = {
            "ok": True,
            "rank_id": rank_id,
            "boot_id": state.boot_id,
            "membership_epoch": state.membership_epoch,
            "applied_digest": state.applied_digest,
            "supported_fields": supported,
            "unsupported_fields": unsupported,
            "boot_defaults": _read_canonical_boot_defaults(
                state.boot_defaults, supported
            ),
            "values": _read_current_values(scheduler, sched_cfg, supported),
        }
        kernel_report = _collect_rank_kernel_report(engine_core)
        if kernel_report:
            resp.update(kernel_report)
        return resp

    if op in ("probe_start", "probe_collect", "probe_abort"):
        target_rank = int(payload.get("target_rank", 0))
        if rank_id != target_rank:
            return {
                "ok": True,
                "skipped": True,
                "rank_id": rank_id,
                "boot_id": state.boot_id,
            }
        probe_id = str(payload.get("probe_id") or "")
        if op == "probe_start":
            worker_res = _dispatch_rank0_worker(
                engine_core, vllm_worker_probe_start, dict(payload)
            )
        elif op == "probe_collect":
            worker_res = _dispatch_rank0_worker(
                engine_core, vllm_worker_probe_collect, probe_id
            )
        else:
            worker_res = _dispatch_rank0_worker(
                engine_core, vllm_worker_probe_abort, probe_id
            )
            out_dir = payload.get("output_dir")
            if isinstance(out_dir, str) and out_dir.strip():
                purge_probe_dir(Path(out_dir))
        merged = dict(worker_res)
        merged["rank_id"] = rank_id
        merged["boot_id"] = state.boot_id
        return merged

    if op == "rollback":
        by_boot_id = payload.get("by_boot_id")
        if isinstance(by_boot_id, dict):
            entry = by_boot_id.get(state.boot_id)
            if not isinstance(entry, dict):
                return {
                    "ok": True,
                    "skipped": True,
                    "rank_id": rank_id,
                    "boot_id": state.boot_id,
                    "membership_epoch": state.membership_epoch,
                    "applied_digest": state.applied_digest,
                }
            snap = entry.get("snapshot") or {}
            prev_digest = str(entry.get("previous_digest", ""))
        else:
            snap = payload.get("snapshot") or {}
            prev_digest = str(payload.get("previous_digest", ""))
        _restore_snapshot(scheduler, sched_cfg, snap)
        state.applied_digest = prev_digest
        return {
            "ok": True,
            "rank_id": rank_id,
            "boot_id": state.boot_id,
            "membership_epoch": state.membership_epoch,
            "applied_digest": state.applied_digest,
        }

    if op != "apply":
        return {
            "ok": False,
            "error_code": "invalid_argument",
            "message": f"Unsupported EngineCore RPC op: {op!r}",
            "rank_id": rank_id,
        }

    parameters: dict[str, Any] = dict(payload.get("parameters") or {})
    reset_fields: list[str] = list(payload.get("reset_fields") or [])
    target_digest = str(payload.get("applied_config_digest", ""))

    supported_set = set(supported)
    missing_caps = [f for f in parameters if f not in supported_set]
    if missing_caps:
        return {
            "ok": False,
            "error_code": "unsupported_fields",
            "unsupported_fields": sorted(set(missing_caps)),
            "supported_fields": supported,
            "rank_id": rank_id,
            "boot_id": state.boot_id,
            "membership_epoch": state.membership_epoch,
            "applied_digest": state.applied_digest,
            "boot_defaults": _read_canonical_boot_defaults(
                state.boot_defaults, supported
            ),
            "values": _read_current_values(scheduler, sched_cfg, supported),
        }

    before_snap = _capture_snapshot(scheduler, sched_cfg)
    before_digest = state.applied_digest
    defaults = state.boot_defaults

    try:
        # Apply resets first so simultaneous field overrides win cleanly.
        for canonical in reset_fields:
            if canonical in supported_set:
                _reset_one_field(canonical, scheduler, sched_cfg, defaults)

        if FIELD_MAX_ACTIVE_REQUESTS in parameters:
            ceiling = int(
                defaults.get(
                    "sched.max_num_running_reqs",
                    getattr(scheduler, "max_num_running_reqs", 1),
                )
            )
            val = parse_strict_int(
                parameters[FIELD_MAX_ACTIVE_REQUESTS],
                FIELD_MAX_ACTIVE_REQUESTS,
                minimum=1,
                maximum=ceiling,
            )
            # Update max_num_active_reqs only; never mutate max_num_running_reqs.
            scheduler.max_num_active_reqs = val
            if sched_cfg is not None and hasattr(sched_cfg, "max_num_active_seqs"):
                sched_cfg.max_num_active_seqs = val

        if FIELD_MAX_BATCHED_TOKENS in parameters:
            token_ceiling = int(
                defaults.get(
                    "sched.max_num_scheduled_tokens",
                    getattr(scheduler, "max_num_scheduled_tokens", 1),
                )
            )
            val = parse_strict_int(
                parameters[FIELD_MAX_BATCHED_TOKENS],
                FIELD_MAX_BATCHED_TOKENS,
                minimum=1,
                maximum=token_ceiling,
            )
            scheduler.max_num_scheduled_tokens = val
            if sched_cfg is not None and hasattr(sched_cfg, "max_num_scheduled_tokens"):
                sched_cfg.max_num_scheduled_tokens = val

        if FIELD_LONG_PREFILL_TOKEN_THRESHOLD in parameters:
            eff_batched = int(getattr(scheduler, "max_num_scheduled_tokens", 1))
            max_model_len = getattr(scheduler, "max_model_len", _SENTINEL)
            if max_model_len is _SENTINEL:
                max_model_len = getattr(
                    getattr(vllm_cfg, "model_config", None), "max_model_len", None
                )
            upper = (
                min(eff_batched, int(max_model_len))
                if isinstance(max_model_len, int) and max_model_len > 0
                else eff_batched
            )
            val = parse_strict_int(
                parameters[FIELD_LONG_PREFILL_TOKEN_THRESHOLD],
                FIELD_LONG_PREFILL_TOKEN_THRESHOLD,
                minimum=0,
                maximum=upper,
            )
            sched_cfg.long_prefill_token_threshold = val

        if FIELD_CACHE_AWARE_WINDOW in parameters:
            val = parse_strict_int(
                parameters[FIELD_CACHE_AWARE_WINDOW],
                FIELD_CACHE_AWARE_WINDOW,
                minimum=0,
            )
            sched_cfg.cache_aware_admission_window = val
            scheduler.cache_aware_window = val

        if FIELD_CACHE_AWARE_THRESHOLD in parameters:
            fval = parse_decimal_float(
                parameters[FIELD_CACHE_AWARE_THRESHOLD],
                FIELD_CACHE_AWARE_THRESHOLD,
                minimum=0.0,
                maximum=1.0,
            )
            sched_cfg.cache_aware_admission_threshold = fval
            scheduler.cache_aware_threshold = fval

        if FIELD_MAX_INFLIGHT_KV_LOADS in parameters:
            val = parse_strict_int(
                parameters[FIELD_MAX_INFLIGHT_KV_LOADS],
                FIELD_MAX_INFLIGHT_KV_LOADS,
                minimum=1,
            )
            sched_cfg.max_num_inflight_kv_loads = val

        if FIELD_MAX_ASYNC_KV_LOADS_PER_STEP in parameters:
            val = parse_strict_int(
                parameters[FIELD_MAX_ASYNC_KV_LOADS_PER_STEP],
                FIELD_MAX_ASYNC_KV_LOADS_PER_STEP,
                minimum=1,
            )
            sched_cfg.max_async_kv_loads_per_step = val

        state.applied_digest = target_digest
        apply_resp: dict[str, Any] = {
            "ok": True,
            "rank_id": rank_id,
            "boot_id": state.boot_id,
            "membership_epoch": state.membership_epoch,
            "applied_digest": state.applied_digest,
            "supported_fields": supported,
            "unsupported_fields": unsupported,
            "boot_defaults": _read_canonical_boot_defaults(defaults, supported),
            "values": _read_current_values(scheduler, sched_cfg, supported),
            "previous_snapshot": before_snap,
            "previous_digest": before_digest,
        }
        kernel_report = _collect_rank_kernel_report(engine_core)
        if kernel_report:
            apply_resp.update(kernel_report)
        return apply_resp
    except InvalidRequestError as exc:
        _restore_snapshot(scheduler, sched_cfg, before_snap)
        state.applied_digest = before_digest
        return {
            "ok": False,
            "error_code": "invalid_argument",
            "message": exc.message,
            "rank_id": rank_id,
            "boot_id": state.boot_id,
            "membership_epoch": state.membership_epoch,
            "applied_digest": state.applied_digest,
        }
    except Exception as exc:
        _restore_snapshot(scheduler, sched_cfg, before_snap)
        state.applied_digest = before_digest
        return {
            "ok": False,
            "error_code": "internal_error",
            "message": str(exc),
            "rank_id": rank_id,
            "boot_id": state.boot_id,
            "membership_epoch": state.membership_epoch,
            "applied_digest": state.applied_digest,
        }


def attach_engine_core_utility(engine_core_cls: Any) -> None:
    """Attach gpustack_engine_sentinel_rpc onto vLLM EngineCore idempotently."""
    if hasattr(engine_core_cls, UTILITY_METHOD_NAME):
        return

    def _rpc_method(self: Any, payload: dict[str, Any]) -> dict[str, Any]:
        return execute_engine_core_rpc(self, payload)

    setattr(engine_core_cls, UTILITY_METHOD_NAME, _rpc_method)
