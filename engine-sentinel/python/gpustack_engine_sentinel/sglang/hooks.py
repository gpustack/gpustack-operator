"""Spawn-safe SGLang Scheduler internal-state handlers and HTTP frontend sentinel."""

from __future__ import annotations

import asyncio
import inspect
import logging
import math
import time
import uuid
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from gpustack_engine_sentinel.kernel_loader import (
    aggregate_kernel_reports,
    get_process_kernel_report,
)
from gpustack_engine_sentinel.latch import BootLatch
from gpustack_engine_sentinel.probe import (
    directory_size_bytes,
    distill_probe_directory,
    purge_probe_dir,
    select_rank0_trace_file,
)
from gpustack_engine_sentinel.protocol import (
    ENGINE_SGLANG,
    FIELD_ACCEPT_THRESHOLD_ACC,
    FIELD_ACCEPT_THRESHOLD_SINGLE,
    FIELD_DRAFT_BUDGET_FRACTION,
    FIELD_MAX_ACTIVE_REQUESTS,
    FIELD_MAX_BATCHED_TOKENS,
    FIELD_PP_MAX_MICRO_BATCH_SIZE,
    FIELD_PREFILL_DECODE_INTERVAL,
    FIELD_PREFILL_MAX_REQUESTS,
    FIELD_SCHEDULE_CONSERVATIVENESS,
    PROBE_MAX_SHM_BYTES,
    PROBE_WALL_CLOCK_CAP_SECONDS,
    SGLANG_ALL_FIELDS,
    InvalidRequestError,
    RankReceipt,
    SentinelError,
    UnsupportedEngineVersionError,
    UnsupportedFieldsError,
    parse_decimal_float,
    parse_strict_int,
)
from gpustack_engine_sentinel.server import (
    SentinelControlPlane,
    ensure_outermost_sentinel_middleware,
)

LOGGER = logging.getLogger(__name__)
APPLY_BUNDLE_KEY = "gpustack_engine_sentinel_apply"
ROLLBACK_BUNDLE_KEY = "gpustack_engine_sentinel_rollback"
STATE_NAMESPACE_KEY = "gpustack_engine_sentinel"
_UNSET = object()


@dataclass
class _SGLangRankState:
    boot_id: str = field(default_factory=lambda: str(uuid.uuid4()))
    membership_epoch: int = 1
    last_world_sig: tuple[Any, ...] | None = None
    applied_digest: str = ""
    boot_defaults: dict[str, Any] = field(default_factory=dict)
    last_error_code: str = ""
    last_error_message: str = ""
    last_unsupported_fields: list[str] = field(default_factory=list)
    recorded_dspark_budget: float | object | None = _UNSET


def _sglang_get_context() -> Any:
    try:
        from sglang.srt.runtime_context import get_context

        return get_context()
    except Exception:
        return None


def _sglang_get_schedule() -> Any:
    try:
        from sglang.srt.runtime_context import get_schedule

        return get_schedule()
    except Exception:
        return None


def _sglang_get_parallel() -> Any:
    try:
        from sglang.srt.runtime_context import get_parallel

        return get_parallel()
    except Exception:
        return None


def _read_dspark_force_budget_frac(
    scheduler: Any, state: _SGLangRankState
) -> float | None:
    draft_worker = getattr(scheduler, "draft_worker", None)
    if draft_worker is not None:
        planner = getattr(draft_worker, "_verify_planner", None)
        budget_planner = getattr(planner, "_budget_planner", None)
        if budget_planner is not None and hasattr(budget_planner, "forced_budget_frac"):
            raw = budget_planner.forced_budget_frac
            return None if raw is None else float(raw)
        if hasattr(draft_worker, "_forced_budget_frac"):
            raw = draft_worker._forced_budget_frac
            return None if raw is None else float(raw)
    if state.recorded_dspark_budget is not _UNSET:
        return state.recorded_dspark_budget  # type: ignore[return-value]
    return None


def _positive_int(val: Any) -> int | None:
    if isinstance(val, bool) or not isinstance(val, int) or val <= 0:
        return None
    return val


def _probe_sglang_capabilities(scheduler: Any) -> tuple[list[str], list[str]]:
    supported: list[str] = []
    unsupported: list[str] = []
    sched_ctx = _sglang_get_schedule()
    par_ctx = _sglang_get_parallel()

    if (
        scheduler is not None
        and _positive_int(getattr(scheduler, "max_running_requests", None)) is not None
        and callable(getattr(scheduler, "get_num_allocatable_reqs", None))
    ):
        supported.append(FIELD_MAX_ACTIVE_REQUESTS)
    else:
        unsupported.append(FIELD_MAX_ACTIVE_REQUESTS)

    chunk_size = getattr(scheduler, "chunked_prefill_size", None)
    if isinstance(chunk_size, int) and chunk_size > 0:
        supported.append(FIELD_MAX_BATCHED_TOKENS)
    else:
        unsupported.append(FIELD_MAX_BATCHED_TOKENS)

    if sched_ctx is not None and hasattr(sched_ctx, "prefill_max_requests"):
        supported.append(FIELD_PREFILL_MAX_REQUESTS)
    else:
        unsupported.append(FIELD_PREFILL_MAX_REQUESTS)

    if (
        scheduler is not None
        and hasattr(scheduler, "new_token_ratio_tracker")
        and sched_ctx is not None
        and hasattr(sched_ctx, "schedule_conservativeness")
    ):
        supported.append(FIELD_SCHEDULE_CONSERVATIVENESS)
    else:
        unsupported.append(FIELD_SCHEDULE_CONSERVATIVENESS)

    if (
        scheduler is not None
        and hasattr(scheduler, "prefill_decode_interval")
        and hasattr(scheduler, "_prefill_decode_interval_remaining")
    ):
        supported.append(FIELD_PREFILL_DECODE_INTERVAL)
    else:
        unsupported.append(FIELD_PREFILL_DECODE_INTERVAL)

    pp_size = int(getattr(par_ctx, "pp_size", 1) or 1)
    if (
        pp_size > 1
        and par_ctx is not None
        and hasattr(par_ctx, "pp_max_micro_batch_size")
    ):
        supported.append(FIELD_PP_MAX_MICRO_BATCH_SIZE)
    else:
        unsupported.append(FIELD_PP_MAX_MICRO_BATCH_SIZE)

    spec_algo = getattr(scheduler, "spec_algorithm", None)
    is_spec = (
        spec_algo is not None
        and callable(getattr(spec_algo, "is_none", None))
        and not spec_algo.is_none()
    )
    if is_spec:
        supported.append(FIELD_ACCEPT_THRESHOLD_SINGLE)
        supported.append(FIELD_ACCEPT_THRESHOLD_ACC)
    else:
        unsupported.append(FIELD_ACCEPT_THRESHOLD_SINGLE)
        unsupported.append(FIELD_ACCEPT_THRESHOLD_ACC)

    draft_worker = getattr(scheduler, "draft_worker", None)
    is_dflash_family = bool(
        spec_algo is not None
        and callable(getattr(spec_algo, "is_dflash_family", None))
        and spec_algo.is_dflash_family()
    )
    if is_dflash_family and hasattr(draft_worker, "set_dspark_forced_budget_frac"):
        supported.append(FIELD_DRAFT_BUDGET_FRACTION)
    else:
        unsupported.append(FIELD_DRAFT_BUDGET_FRACTION)

    for f in SGLANG_ALL_FIELDS:
        if f not in supported and f not in unsupported:
            unsupported.append(f)
    return sorted(supported), sorted(unsupported)


def _ensure_sglang_rank_state(scheduler: Any) -> _SGLangRankState:
    state: _SGLangRankState | None = getattr(
        scheduler, "_gpustack_sentinel_state", None
    )
    if state is None:
        state = _SGLangRankState()
        scheduler._gpustack_sentinel_state = state

    par_ctx = _sglang_get_parallel()
    dp_size = int(getattr(par_ctx, "dp_size", 1) or 1)
    tp_size = int(getattr(par_ctx, "tp_size", 1) or 1)
    pp_size = int(getattr(par_ctx, "pp_size", 1) or 1)
    ep_size = int(getattr(par_ctx, "ep_size", 1) or 1)
    sig = (dp_size, tp_size, pp_size, ep_size)
    if state.last_world_sig is None:
        state.last_world_sig = sig
    elif state.last_world_sig != sig:
        state.membership_epoch += 1
        state.last_world_sig = sig

    if not state.boot_defaults and scheduler is not None:
        sched_ctx = _sglang_get_schedule()
        ctx_dict = {}
        ctx = _sglang_get_context()
        if ctx is not None and callable(
            getattr(ctx, "resolved_server_args_dict", None)
        ):
            try:
                ctx_dict = dict(ctx.resolved_server_args_dict())
            except Exception:
                ctx_dict = {}

        defaults: dict[str, Any] = {
            "max_running_requests": _positive_int(
                getattr(scheduler, "max_running_requests", None)
            ),
            "chunked_prefill_size": getattr(scheduler, "chunked_prefill_size", None),
            "prefill_max_requests": getattr(sched_ctx, "prefill_max_requests", None),
            "schedule_conservativeness": float(
                getattr(sched_ctx, "schedule_conservativeness", 1.0)
            ),
            "prefill_decode_interval": int(
                getattr(scheduler, "prefill_decode_interval", 0) or 0
            ),
            "pp_max_micro_batch_size": getattr(
                par_ctx, "pp_max_micro_batch_size", None
            ),
            "speculative_accept_threshold_single": ctx_dict.get(
                "speculative_accept_threshold_single"
            ),
            "speculative_accept_threshold_acc": ctx_dict.get(
                "speculative_accept_threshold_acc"
            ),
            "dspark_force_budget_frac": _read_dspark_force_budget_frac(
                scheduler, state
            ),
        }
        tracker = getattr(scheduler, "new_token_ratio_tracker", None)
        if tracker is not None:
            defaults["tracker_init_at_1"] = float(getattr(tracker, "init", 0.7)) / max(
                defaults["schedule_conservativeness"], 1e-6
            )
            defaults["tracker_min_factor"] = float(getattr(tracker, "min", 0.28)) / max(
                float(getattr(tracker, "init", 0.7)), 1e-6
            )
            defaults["tracker_decay_steps"] = (
                float(getattr(tracker, "init", 0.7))
                - float(getattr(tracker, "min", 0.28))
            ) / max(float(getattr(tracker, "decay", 0.001)), 1e-9)
        state.boot_defaults = defaults

    return state


def _rebuild_new_token_ratio_tracker(
    scheduler: Any, conservativeness: float, defaults: dict[str, Any]
) -> None:
    tracker = getattr(scheduler, "new_token_ratio_tracker", None)
    if tracker is None:
        return
    tracker_cls = type(tracker)
    from_config = getattr(tracker_cls, "from_config", None)
    if callable(from_config):
        try:
            scheduler.new_token_ratio_tracker = from_config()
            return
        except Exception:
            pass
    base_init = float(defaults.get("tracker_init_at_1", 0.7))
    min_factor = float(defaults.get("tracker_min_factor", 0.4))
    steps = max(float(defaults.get("tracker_decay_steps", 600.0)), 1.0)
    new_init = min(base_init * conservativeness, 1.0)
    new_min = min(new_init * min_factor, 1.0)
    new_decay = (new_init - new_min) / steps
    scheduler.new_token_ratio_tracker = tracker_cls(
        init=new_init, min=new_min, decay=new_decay, current=new_init
    )


def _capture_sglang_snapshot(scheduler: Any, state: _SGLangRankState) -> dict[str, Any]:
    sched_ctx = _sglang_get_schedule()
    par_ctx = _sglang_get_parallel()
    ctx = _sglang_get_context()
    ctx_dict = {}
    if ctx is not None and callable(getattr(ctx, "resolved_server_args_dict", None)):
        try:
            ctx_dict = dict(ctx.resolved_server_args_dict())
        except Exception:
            ctx_dict = {}

    return {
        "live_max_active": getattr(
            scheduler, "_gpustack_live_max_active_requests", None
        ),
        "batch_is_full": getattr(
            getattr(scheduler, "running_batch", None), "batch_is_full", False
        ),
        "chunked_prefill_size": getattr(scheduler, "chunked_prefill_size", None),
        "prefill_max_requests": getattr(sched_ctx, "prefill_max_requests", None),
        "schedule_conservativeness": float(
            getattr(sched_ctx, "schedule_conservativeness", 1.0)
        ),
        "tracker_current": getattr(
            getattr(scheduler, "new_token_ratio_tracker", None), "current", None
        ),
        "prefill_decode_interval": int(
            getattr(scheduler, "prefill_decode_interval", 0) or 0
        ),
        "prefill_decode_remaining": int(
            getattr(scheduler, "_prefill_decode_interval_remaining", 0) or 0
        ),
        "has_pp_mb": par_ctx is not None
        and hasattr(par_ctx, "pp_max_micro_batch_size"),
        "pp_max_micro_batch_size": getattr(par_ctx, "pp_max_micro_batch_size", None),
        "has_acc_single": "speculative_accept_threshold_single" in ctx_dict,
        "speculative_accept_threshold_single": ctx_dict.get(
            "speculative_accept_threshold_single"
        ),
        "has_acc_acc": "speculative_accept_threshold_acc" in ctx_dict,
        "speculative_accept_threshold_acc": ctx_dict.get(
            "speculative_accept_threshold_acc"
        ),
        "dspark_force_budget_frac": _read_dspark_force_budget_frac(scheduler, state),
        "applied_digest": state.applied_digest,
    }


def _resolve_rank_rollback_snapshot(
    state: _SGLangRankState, payload: dict[str, Any], rejection_cls: type[Exception]
) -> dict[str, Any]:
    if "by_boot_id" in payload:
        by_boot_id = payload.get("by_boot_id")
        if not isinstance(by_boot_id, dict):
            raise rejection_cls("rollback by_boot_id expects dict")
        rank_snap = by_boot_id.get(state.boot_id)
        if not isinstance(rank_snap, dict):
            raise rejection_cls(
                f"missing per-rank rollback snapshot for boot_id={state.boot_id!r}"
            )
        return rank_snap
    return payload


def _restore_sglang_snapshot(
    controller: Any,
    scheduler: Any,
    state: _SGLangRankState,
    snap: dict[str, Any],
    rejection_cls: type[Exception] = ValueError,
) -> None:
    snap = _resolve_rank_rollback_snapshot(state, snap, rejection_cls)
    scheduler._gpustack_live_max_active_requests = snap.get("live_max_active")
    rb = getattr(scheduler, "running_batch", None)
    if rb is not None and hasattr(rb, "batch_is_full"):
        rb.batch_is_full = bool(snap.get("batch_is_full", False))

    if hasattr(scheduler, "chunked_prefill_size"):
        scheduler.chunked_prefill_size = snap.get("chunked_prefill_size")
        sizer = getattr(scheduler, "dynamic_chunk_sizer", None)
        if sizer is not None and hasattr(sizer, "chunked_prefill_size"):
            sizer.chunked_prefill_size = snap.get("chunked_prefill_size")

    ctx_overrides: dict[str, Any] = {
        "prefill_max_requests": snap.get("prefill_max_requests"),
        "schedule_conservativeness": snap.get("schedule_conservativeness", 1.0),
        "prefill_decode_interval": snap.get("prefill_decode_interval", 0),
    }
    if snap.get("chunked_prefill_size") is not None:
        ctx_overrides["chunked_prefill_size"] = snap["chunked_prefill_size"]
    if snap.get("has_pp_mb"):
        ctx_overrides["pp_max_micro_batch_size"] = snap.get("pp_max_micro_batch_size")
    if snap.get("has_acc_single"):
        ctx_overrides["speculative_accept_threshold_single"] = snap.get(
            "speculative_accept_threshold_single"
        )
    if snap.get("has_acc_acc"):
        ctx_overrides["speculative_accept_threshold_acc"] = snap.get(
            "speculative_accept_threshold_acc"
        )

    ctx = _sglang_get_context()
    if ctx is not None and callable(getattr(ctx, "override", None)):
        ctx.override(source="gpustack_engine_sentinel_rollback", **ctx_overrides)

    delayer = getattr(scheduler, "prefill_delayer", None)
    if delayer is not None and hasattr(delayer, "_prefill_max_requests"):
        delayer._prefill_max_requests = snap.get("prefill_max_requests")

    _rebuild_new_token_ratio_tracker(
        scheduler,
        float(snap.get("schedule_conservativeness", 1.0)),
        state.boot_defaults,
    )
    tracker = getattr(scheduler, "new_token_ratio_tracker", None)
    if (
        tracker is not None
        and snap.get("tracker_current") is not None
        and hasattr(tracker, "current")
    ):
        tracker.current = snap["tracker_current"]

    if hasattr(scheduler, "prefill_decode_interval"):
        scheduler.prefill_decode_interval = int(snap.get("prefill_decode_interval", 0))
    if hasattr(scheduler, "_prefill_decode_interval_remaining"):
        scheduler._prefill_decode_interval_remaining = int(
            snap.get("prefill_decode_remaining", 0)
        )

    apply_dspark = getattr(controller, "_apply_dspark_budget_frac", None)
    draft_worker = getattr(scheduler, "draft_worker", None)
    if (
        callable(apply_dspark)
        and draft_worker is not None
        and hasattr(draft_worker, "set_dspark_forced_budget_frac")
    ):
        apply_dspark("dspark_force_budget_frac", snap.get("dspark_force_budget_frac"))
        state.recorded_dspark_budget = snap.get("dspark_force_budget_frac")

    state.applied_digest = str(snap.get("applied_digest", ""))


def _chunked_prefill_ceiling(scheduler: Any, defaults: dict[str, Any]) -> int:
    boot_chunk = int(
        defaults.get("chunked_prefill_size")
        or getattr(scheduler, "chunked_prefill_size", 1)
        or 1
    )
    limits = [boot_chunk]
    max_prefill = getattr(scheduler, "max_prefill_tokens", None)
    if isinstance(max_prefill, int) and max_prefill > 0:
        limits.append(min(boot_chunk, max_prefill))
    runner = getattr(getattr(scheduler, "tp_worker", None), "model_runner", None)
    graph_max_tokens = getattr(runner, "extend_cuda_graph_max_tokens", None)
    if isinstance(graph_max_tokens, int) and graph_max_tokens > 0:
        limits.append(graph_max_tokens)
    return max(min(limits), 1)


def _read_sglang_boot_defaults(
    scheduler: Any, defaults: dict[str, Any], supported: list[str]
) -> dict[str, Any]:
    supported_set = set(supported)
    out: dict[str, Any] = {}
    if FIELD_MAX_ACTIVE_REQUESTS in supported_set:
        max_reqs = _positive_int(defaults.get("max_running_requests")) or _positive_int(
            getattr(scheduler, "max_running_requests", None)
        )
        if max_reqs is not None:
            out[FIELD_MAX_ACTIVE_REQUESTS] = max_reqs
    if FIELD_MAX_BATCHED_TOKENS in supported_set:
        out[FIELD_MAX_BATCHED_TOKENS] = _chunked_prefill_ceiling(scheduler, defaults)
    if FIELD_PREFILL_MAX_REQUESTS in supported_set:
        out[FIELD_PREFILL_MAX_REQUESTS] = defaults.get("prefill_max_requests")
    if FIELD_SCHEDULE_CONSERVATIVENESS in supported_set:
        out[FIELD_SCHEDULE_CONSERVATIVENESS] = float(
            defaults.get("schedule_conservativeness", 1.0)
        )
    if FIELD_PREFILL_DECODE_INTERVAL in supported_set:
        out[FIELD_PREFILL_DECODE_INTERVAL] = int(
            defaults.get("prefill_decode_interval", 0)
        )
    if FIELD_PP_MAX_MICRO_BATCH_SIZE in supported_set:
        out[FIELD_PP_MAX_MICRO_BATCH_SIZE] = defaults.get("pp_max_micro_batch_size")
    if FIELD_ACCEPT_THRESHOLD_SINGLE in supported_set:
        out[FIELD_ACCEPT_THRESHOLD_SINGLE] = defaults.get(
            "speculative_accept_threshold_single"
        )
    if FIELD_ACCEPT_THRESHOLD_ACC in supported_set:
        out[FIELD_ACCEPT_THRESHOLD_ACC] = defaults.get(
            "speculative_accept_threshold_acc"
        )
    if FIELD_DRAFT_BUDGET_FRACTION in supported_set:
        out[FIELD_DRAFT_BUDGET_FRACTION] = defaults.get("dspark_force_budget_frac")
    return out


def _read_sglang_values(
    scheduler: Any, state: _SGLangRankState, supported: list[str]
) -> dict[str, Any]:
    supported_set = set(supported)
    sched_ctx = _sglang_get_schedule()
    par_ctx = _sglang_get_parallel()
    ctx = _sglang_get_context()
    ctx_dict: dict[str, Any] = {}
    if ctx is not None and callable(getattr(ctx, "resolved_server_args_dict", None)):
        try:
            ctx_dict = dict(ctx.resolved_server_args_dict())
        except Exception:
            ctx_dict = {}

    values: dict[str, Any] = {}
    if FIELD_MAX_ACTIVE_REQUESTS in supported_set:
        live_cap = getattr(scheduler, "_gpustack_live_max_active_requests", None)
        if live_cap is not None:
            values[FIELD_MAX_ACTIVE_REQUESTS] = int(live_cap)
        else:
            max_reqs = _positive_int(getattr(scheduler, "max_running_requests", None))
            if max_reqs is not None:
                values[FIELD_MAX_ACTIVE_REQUESTS] = max_reqs
    if FIELD_MAX_BATCHED_TOKENS in supported_set:
        values[FIELD_MAX_BATCHED_TOKENS] = getattr(
            scheduler, "chunked_prefill_size", None
        )
    if FIELD_PREFILL_MAX_REQUESTS in supported_set:
        values[FIELD_PREFILL_MAX_REQUESTS] = getattr(
            sched_ctx, "prefill_max_requests", None
        )
    if FIELD_SCHEDULE_CONSERVATIVENESS in supported_set:
        values[FIELD_SCHEDULE_CONSERVATIVENESS] = float(
            getattr(sched_ctx, "schedule_conservativeness", 1.0)
        )
    if FIELD_PREFILL_DECODE_INTERVAL in supported_set:
        values[FIELD_PREFILL_DECODE_INTERVAL] = int(
            getattr(scheduler, "prefill_decode_interval", 0) or 0
        )
    if FIELD_PP_MAX_MICRO_BATCH_SIZE in supported_set:
        values[FIELD_PP_MAX_MICRO_BATCH_SIZE] = getattr(
            par_ctx, "pp_max_micro_batch_size", None
        )
    if FIELD_ACCEPT_THRESHOLD_SINGLE in supported_set:
        values[FIELD_ACCEPT_THRESHOLD_SINGLE] = ctx_dict.get(
            "speculative_accept_threshold_single"
        )
    if FIELD_ACCEPT_THRESHOLD_ACC in supported_set:
        values[FIELD_ACCEPT_THRESHOLD_ACC] = ctx_dict.get(
            "speculative_accept_threshold_acc"
        )
    if FIELD_DRAFT_BUDGET_FRACTION in supported_set:
        values[FIELD_DRAFT_BUDGET_FRACTION] = _read_dspark_force_budget_frac(
            scheduler, state
        )
    return values


def _sglang_rank_id(scheduler: Any) -> int:
    par_ctx = _sglang_get_parallel()
    for candidate in (
        getattr(scheduler, "dp_rank", None),
        getattr(par_ctx, "dp_rank", None),
        getattr(par_ctx, "attn_dp_rank", None),
        getattr(par_ctx, "tp_rank", None),
    ):
        if isinstance(candidate, int):
            return candidate
    return 0


@dataclass(frozen=True)
class _ParsedSGLangBundle:
    active: int | None
    batched: int | None
    prefill_max: int | None
    conservativeness: float | None
    interval: int | None
    pp_mb: int | None
    acc_single: float | None
    acc_total: float | None
    has_draft_budget: bool
    draft_budget: float | None
    resets: set[str]
    target_digest: str


def _parse_sglang_bundle(
    controller: Any,
    scheduler: Any,
    defaults: dict[str, Any],
    parameters: dict[str, Any],
    resets: list[str],
    target_digest: str,
) -> _ParsedSGLangBundle:
    boot_max_reqs = _positive_int(
        defaults.get("max_running_requests")
    ) or _positive_int(getattr(scheduler, "max_running_requests", None))
    parsed_active: int | None = None
    if FIELD_MAX_ACTIVE_REQUESTS in parameters:
        if boot_max_reqs is None:
            raise InvalidRequestError(
                f"{FIELD_MAX_ACTIVE_REQUESTS} boot ceiling is unavailable"
            )
        parsed_active = parse_strict_int(
            parameters[FIELD_MAX_ACTIVE_REQUESTS],
            FIELD_MAX_ACTIVE_REQUESTS,
            minimum=1,
            maximum=boot_max_reqs,
        )

    parsed_batched: int | None = None
    if FIELD_MAX_BATCHED_TOKENS in parameters:
        ceiling = _chunked_prefill_ceiling(scheduler, defaults)
        parsed_batched = parse_strict_int(
            parameters[FIELD_MAX_BATCHED_TOKENS],
            FIELD_MAX_BATCHED_TOKENS,
            minimum=1,
            maximum=ceiling,
        )
        page_size = int(getattr(scheduler, "page_size", 1) or 1)
        if page_size > 1 and parsed_batched % page_size != 0:
            raise InvalidRequestError(
                f"{FIELD_MAX_BATCHED_TOKENS} ({parsed_batched}) must be a multiple of page_size ({page_size})"
            )
        trunc_align = getattr(scheduler, "truncation_align_size", None)
        if isinstance(trunc_align, int) and trunc_align > 0:
            required_align = math.lcm(page_size, trunc_align)
            if parsed_batched < required_align or parsed_batched % required_align != 0:
                raise InvalidRequestError(
                    f"{FIELD_MAX_BATCHED_TOKENS} ({parsed_batched}) must align to {required_align}"
                )

    eff_active = parsed_active if parsed_active is not None else boot_max_reqs
    parsed_prefill_max = (
        parse_strict_int(
            parameters[FIELD_PREFILL_MAX_REQUESTS],
            FIELD_PREFILL_MAX_REQUESTS,
            minimum=1,
            maximum=eff_active,
        )
        if FIELD_PREFILL_MAX_REQUESTS in parameters
        else None
    )

    parsed_conservativeness = (
        parse_decimal_float(
            parameters[FIELD_SCHEDULE_CONSERVATIVENESS],
            FIELD_SCHEDULE_CONSERVATIVENESS,
            minimum=0.1,
            maximum=2.0,
        )
        if FIELD_SCHEDULE_CONSERVATIVENESS in parameters
        else None
    )

    parsed_interval = (
        parse_strict_int(
            parameters[FIELD_PREFILL_DECODE_INTERVAL],
            FIELD_PREFILL_DECODE_INTERVAL,
            minimum=0,
        )
        if FIELD_PREFILL_DECODE_INTERVAL in parameters
        else None
    )

    parsed_pp_mb: int | None = None
    if FIELD_PP_MAX_MICRO_BATCH_SIZE in parameters:
        par_ctx = _sglang_get_parallel()
        pp_size = max(int(getattr(par_ctx, "pp_size", 1) or 1), 1)
        parsed_pp_mb = parse_strict_int(
            parameters[FIELD_PP_MAX_MICRO_BATCH_SIZE],
            FIELD_PP_MAX_MICRO_BATCH_SIZE,
            minimum=1,
            maximum=max(boot_max_reqs // pp_size, 1),
        )
        native_pp_val = getattr(controller, "_validate_pp_micro_batch_size", None)
        if callable(native_pp_val):
            native_pp_val("pp_max_micro_batch_size", parsed_pp_mb)

    parsed_acc_single: float | None = None
    if FIELD_ACCEPT_THRESHOLD_SINGLE in parameters:
        parsed_acc_single = parse_decimal_float(
            parameters[FIELD_ACCEPT_THRESHOLD_SINGLE],
            FIELD_ACCEPT_THRESHOLD_SINGLE,
            minimum=0.0,
            maximum=1.0,
        )
        native_val = getattr(controller, "_validate_accept_threshold", None)
        if callable(native_val):
            native_val("speculative_accept_threshold_single", parsed_acc_single)

    parsed_acc_total: float | None = None
    if FIELD_ACCEPT_THRESHOLD_ACC in parameters:
        parsed_acc_total = parse_decimal_float(
            parameters[FIELD_ACCEPT_THRESHOLD_ACC],
            FIELD_ACCEPT_THRESHOLD_ACC,
            minimum=0.0,
            maximum=1.0,
        )
        native_val = getattr(controller, "_validate_accept_threshold", None)
        if callable(native_val):
            native_val("speculative_accept_threshold_acc", parsed_acc_total)

    has_draft_budget = FIELD_DRAFT_BUDGET_FRACTION in parameters
    parsed_draft_budget: float | None = None
    if has_draft_budget:
        raw_budget = parameters[FIELD_DRAFT_BUDGET_FRACTION]
        if raw_budget is not None:
            parsed_draft_budget = parse_decimal_float(
                raw_budget,
                FIELD_DRAFT_BUDGET_FRACTION,
                minimum=0.0,
                maximum=1.0,
                exclusive_min=True,
            )
        native_dspark_val = getattr(controller, "_validate_dspark_budget_frac", None)
        if callable(native_dspark_val):
            native_dspark_val("dspark_force_budget_frac", parsed_draft_budget)

    return _ParsedSGLangBundle(
        active=parsed_active,
        batched=parsed_batched,
        prefill_max=parsed_prefill_max,
        conservativeness=parsed_conservativeness,
        interval=parsed_interval,
        pp_mb=parsed_pp_mb,
        acc_single=parsed_acc_single,
        acc_total=parsed_acc_total,
        has_draft_budget=has_draft_budget,
        draft_budget=parsed_draft_budget,
        resets=set(resets),
        target_digest=target_digest,
    )


def _apply_sglang_resets(
    controller: Any,
    scheduler: Any,
    state: _SGLangRankState,
    supported_set: set[str],
    reset_set: set[str],
    overrides: dict[str, Any],
) -> float | None:
    defaults = state.boot_defaults
    need_tracker_rebuild_to: float | None = None

    if FIELD_MAX_ACTIVE_REQUESTS in reset_set:
        scheduler._gpustack_live_max_active_requests = None
        rb = getattr(scheduler, "running_batch", None)
        if rb is not None and hasattr(rb, "batch_is_full"):
            rb.batch_is_full = False

    if (
        FIELD_MAX_BATCHED_TOKENS in reset_set
        and FIELD_MAX_BATCHED_TOKENS in supported_set
    ):
        boot_chunk = defaults.get("chunked_prefill_size")
        scheduler.chunked_prefill_size = boot_chunk
        sizer = getattr(scheduler, "dynamic_chunk_sizer", None)
        if sizer is not None and hasattr(sizer, "chunked_prefill_size"):
            sizer.chunked_prefill_size = boot_chunk
        if boot_chunk is not None:
            overrides["chunked_prefill_size"] = boot_chunk

    if (
        FIELD_PREFILL_MAX_REQUESTS in reset_set
        and FIELD_PREFILL_MAX_REQUESTS in supported_set
    ):
        boot_pmr = defaults.get("prefill_max_requests")
        overrides["prefill_max_requests"] = boot_pmr
        delayer = getattr(scheduler, "prefill_delayer", None)
        if delayer is not None and hasattr(delayer, "_prefill_max_requests"):
            delayer._prefill_max_requests = boot_pmr

    if (
        FIELD_SCHEDULE_CONSERVATIVENESS in reset_set
        and FIELD_SCHEDULE_CONSERVATIVENESS in supported_set
    ):
        boot_sc = float(defaults.get("schedule_conservativeness", 1.0))
        overrides["schedule_conservativeness"] = boot_sc
        need_tracker_rebuild_to = boot_sc

    if (
        FIELD_PREFILL_DECODE_INTERVAL in reset_set
        and FIELD_PREFILL_DECODE_INTERVAL in supported_set
    ):
        boot_pdi = int(defaults.get("prefill_decode_interval", 0))
        scheduler.prefill_decode_interval = boot_pdi
        overrides["prefill_decode_interval"] = boot_pdi
        rem = int(getattr(scheduler, "_prefill_decode_interval_remaining", 0) or 0)
        scheduler._prefill_decode_interval_remaining = (
            0 if boot_pdi == 0 else min(max(rem, 0), boot_pdi)
        )

    for canonical, arg_key in (
        (FIELD_PP_MAX_MICRO_BATCH_SIZE, "pp_max_micro_batch_size"),
        (FIELD_ACCEPT_THRESHOLD_SINGLE, "speculative_accept_threshold_single"),
        (FIELD_ACCEPT_THRESHOLD_ACC, "speculative_accept_threshold_acc"),
    ):
        if (
            canonical in reset_set
            and canonical in supported_set
            and arg_key in defaults
        ):
            overrides[arg_key] = defaults[arg_key]

    if (
        FIELD_DRAFT_BUDGET_FRACTION in reset_set
        and FIELD_DRAFT_BUDGET_FRACTION in supported_set
    ):
        boot_budget = defaults.get("dspark_force_budget_frac")
        apply_fn = getattr(controller, "_apply_dspark_budget_frac", None)
        if callable(apply_fn):
            apply_fn("dspark_force_budget_frac", boot_budget)
        state.recorded_dspark_budget = boot_budget

    return need_tracker_rebuild_to


def _apply_sglang_updates(
    controller: Any,
    scheduler: Any,
    state: _SGLangRankState,
    parsed: _ParsedSGLangBundle,
    overrides: dict[str, Any],
    need_tracker_rebuild_to: float | None,
) -> float | None:
    if parsed.active is not None:
        prev_cap = getattr(scheduler, "_gpustack_live_max_active_requests", None)
        scheduler._gpustack_live_max_active_requests = parsed.active
        if prev_cap is not None and parsed.active > prev_cap:
            rb = getattr(scheduler, "running_batch", None)
            if rb is not None and hasattr(rb, "batch_is_full"):
                rb.batch_is_full = False

    if parsed.batched is not None:
        scheduler.chunked_prefill_size = parsed.batched
        sizer = getattr(scheduler, "dynamic_chunk_sizer", None)
        if sizer is not None and hasattr(sizer, "chunked_prefill_size"):
            sizer.chunked_prefill_size = parsed.batched
        overrides["chunked_prefill_size"] = parsed.batched

    if parsed.prefill_max is not None:
        overrides["prefill_max_requests"] = parsed.prefill_max
        delayer = getattr(scheduler, "prefill_delayer", None)
        if delayer is not None and hasattr(delayer, "_prefill_max_requests"):
            delayer._prefill_max_requests = parsed.prefill_max

    if parsed.conservativeness is not None:
        overrides["schedule_conservativeness"] = parsed.conservativeness
        need_tracker_rebuild_to = parsed.conservativeness

    if parsed.interval is not None:
        scheduler.prefill_decode_interval = parsed.interval
        overrides["prefill_decode_interval"] = parsed.interval
        rem = int(getattr(scheduler, "_prefill_decode_interval_remaining", 0) or 0)
        scheduler._prefill_decode_interval_remaining = (
            0 if parsed.interval == 0 else min(max(rem, 0), parsed.interval)
        )

    if parsed.pp_mb is not None:
        overrides["pp_max_micro_batch_size"] = parsed.pp_mb

    apply_acc = getattr(controller, "_apply_accept_threshold", None)
    for val, arg_key in (
        (parsed.acc_single, "speculative_accept_threshold_single"),
        (parsed.acc_total, "speculative_accept_threshold_acc"),
    ):
        if val is not None:
            if callable(apply_acc):
                overrides.update(apply_acc(arg_key, val) or {})
            else:
                overrides[arg_key] = val

    if parsed.has_draft_budget:
        apply_fn = getattr(controller, "_apply_dspark_budget_frac", None)
        if callable(apply_fn):
            apply_fn("dspark_force_budget_frac", parsed.draft_budget)
        state.recorded_dspark_budget = parsed.draft_budget

    return need_tracker_rebuild_to


def _validate_and_apply_sglang_bundle(
    controller: Any,
    bundle: Any,
    *,
    dry_run: bool,
    rejection_cls: type[Exception],
) -> dict[str, Any]:
    scheduler = controller.scheduler
    state = _ensure_sglang_rank_state(scheduler)
    state.last_error_code = ""
    state.last_error_message = ""
    state.last_unsupported_fields = []

    if not isinstance(bundle, dict):
        state.last_error_code = "invalid_argument"
        state.last_error_message = "Sentinel bundle must be a dict"
        raise rejection_cls(state.last_error_message)

    supported, _ = _probe_sglang_capabilities(scheduler)
    supported_set = set(supported)
    parameters: dict[str, Any] = dict(bundle.get("parameters") or {})
    resets: list[str] = list(bundle.get("reset_fields") or [])
    target_digest = str(bundle.get("applied_config_digest", ""))

    missing_caps = [f for f in parameters if f not in supported_set]
    if missing_caps:
        state.last_error_code = "unsupported_fields"
        state.last_unsupported_fields = sorted(set(missing_caps))
        state.last_error_message = (
            f"Unsupported fields: {', '.join(state.last_unsupported_fields)}"
        )
        raise rejection_cls(state.last_error_message)

    try:
        parsed = _parse_sglang_bundle(
            controller,
            scheduler,
            state.boot_defaults,
            parameters,
            resets,
            target_digest,
        )
    except InvalidRequestError as exc:
        state.last_error_code = "invalid_argument"
        state.last_error_message = exc.message
        raise rejection_cls(exc.message) from exc
    except Exception as exc:
        state.last_error_code = "invalid_argument"
        state.last_error_message = str(exc)
        raise rejection_cls(str(exc)) from exc

    if dry_run:
        return {}

    overrides: dict[str, Any] = {}
    need_tracker_rebuild_to = _apply_sglang_resets(
        controller, scheduler, state, supported_set, parsed.resets, overrides
    )
    need_tracker_rebuild_to = _apply_sglang_updates(
        controller, scheduler, state, parsed, overrides, need_tracker_rebuild_to
    )

    # Publish context overrides before rebuilding NewTokenRatioTracker so from_config() reads the updated value.
    if overrides:
        ctx = _sglang_get_context()
        if ctx is not None and callable(getattr(ctx, "override", None)):
            ctx.override(source="gpustack_engine_sentinel", **overrides)

    if need_tracker_rebuild_to is not None:
        _rebuild_new_token_ratio_tracker(
            scheduler, need_tracker_rebuild_to, state.boot_defaults
        )

    state.applied_digest = target_digest
    return {}


def patch_sglang_scheduler_module(mod: Any) -> None:
    """Wrap Scheduler.get_num_allocatable_reqs with the separate live admission gate."""
    scheduler_cls = getattr(mod, "Scheduler", None)
    if scheduler_cls is None:
        return
    orig_fn = getattr(scheduler_cls, "get_num_allocatable_reqs", None)
    if not callable(orig_fn) or getattr(orig_fn, "_gpustack_sentinel_wrapped", False):
        return

    def _wrapped_get_num_allocatable_reqs(
        self: Any,
        running_bs: int,
        beam_width: int | None = None,
        *args: Any,
        **kwargs: Any,
    ) -> int:
        base = int(orig_fn(self, running_bs, beam_width, *args, **kwargs))
        live_cap = getattr(self, "_gpustack_live_max_active_requests", None)
        if live_cap is None:
            return base
        live_budget = max(int(live_cap) - int(running_bs), 0)
        if beam_width is not None and beam_width > 0:
            live_budget = live_budget // int(beam_width)
        return min(base, live_budget)

    _wrapped_get_num_allocatable_reqs._gpustack_sentinel_wrapped = True  # type: ignore[attr-defined]
    scheduler_cls.get_num_allocatable_reqs = _wrapped_get_num_allocatable_reqs


def patch_sglang_internal_state_module(mod: Any) -> None:
    """Extend SchedulerInternalStateController with live readback and atomic sentinel handlers."""
    ctrl_cls = getattr(mod, "SchedulerInternalStateController", None)
    handler_cls = getattr(mod, "_UpdateHandler", None)
    rejection_cls = getattr(mod, "UpdateRejected", ValueError)
    if ctrl_cls is None or handler_cls is None:
        return
    if getattr(ctrl_cls, "_gpustack_sentinel_patched", False):
        return

    orig_get = ctrl_cls.get_internal_state
    orig_handlers = ctrl_cls._update_handlers
    orig_apply_dspark = getattr(ctrl_cls, "_apply_dspark_budget_frac", None)

    if callable(orig_apply_dspark):

        def _wrapped_apply_dspark(self: Any, key: str, value: Any) -> Any:
            res = orig_apply_dspark(self, key, value)
            st = _ensure_sglang_rank_state(self.scheduler)
            st.recorded_dspark_budget = None if value is None else float(value)
            return res

        ctrl_cls._apply_dspark_budget_frac = _wrapped_apply_dspark

    def _wrapped_get_internal_state(self: Any, recv_req: Any) -> Any:
        output = orig_get(self, recv_req)
        scheduler = self.scheduler
        st = _ensure_sglang_rank_state(scheduler)
        supported, unsupported = _probe_sglang_capabilities(scheduler)
        paused = bool(getattr(getattr(scheduler, "pause_state", None), "paused", False))

        internal_dict = getattr(output, "internal_state", None)
        if isinstance(internal_dict, dict):
            sentinel_state: dict[str, Any] = {
                "rank_id": _sglang_rank_id(scheduler),
                "boot_id": st.boot_id,
                "membership_epoch": st.membership_epoch,
                "applied_digest": st.applied_digest,
                "supported_fields": supported,
                "unsupported_fields": unsupported,
                "paused": paused,
                "boot_defaults": _read_sglang_boot_defaults(
                    scheduler, st.boot_defaults, supported
                ),
                "values": _read_sglang_values(scheduler, st, supported),
                "snapshot": _capture_sglang_snapshot(scheduler, st),
                "last_error_code": st.last_error_code,
                "last_error_message": st.last_error_message,
                "last_unsupported_fields": list(st.last_unsupported_fields),
            }
            kernel_report = get_process_kernel_report(engine="SGLang")
            if kernel_report:
                sentinel_state.update(kernel_report)
            internal_dict[STATE_NAMESPACE_KEY] = sentinel_state
        return output

    def _wrapped_update_handlers(self: Any) -> dict[str, Any]:
        handlers = dict(orig_handlers(self))
        handlers[APPLY_BUNDLE_KEY] = handler_cls(
            lambda _k, val: _validate_and_apply_sglang_bundle(
                self, val, dry_run=True, rejection_cls=rejection_cls
            ),
            lambda _k, val: _validate_and_apply_sglang_bundle(
                self, val, dry_run=False, rejection_cls=rejection_cls
            ),
        )
        handlers[ROLLBACK_BUNDLE_KEY] = handler_cls(
            lambda _k, val: (
                _resolve_rank_rollback_snapshot(
                    _ensure_sglang_rank_state(self.scheduler), val, rejection_cls
                )
                if isinstance(val, dict)
                else (_ for _ in ()).throw(rejection_cls("rollback expects dict"))
            ),
            lambda _k, val: _restore_sglang_snapshot(
                self,
                self.scheduler,
                _ensure_sglang_rank_state(self.scheduler),
                _resolve_rank_rollback_snapshot(
                    _ensure_sglang_rank_state(self.scheduler), val, rejection_cls
                ),
                rejection_cls=rejection_cls,
            ),
        )
        return handlers

    ctrl_cls.get_internal_state = _wrapped_get_internal_state
    ctrl_cls._update_handlers = _wrapped_update_handlers
    ctrl_cls._gpustack_sentinel_patched = True


class SGLangSentinelBackend:
    """Drives SGLang TokenizerManager in-process internal-state fan-out and subprocess liveness."""

    engine = ENGINE_SGLANG

    def __init__(self, http_server_mod: Any) -> None:
        self._http_server = http_server_mod
        self._membership_epoch = 1
        self._last_rank_count: int | None = None
        self._last_kernel_summary: dict[str, Any] = {}
        self._active_probe: dict[str, Any] | None = None

    @staticmethod
    def _extract_rank_kernel_fields(rc: dict[str, Any]) -> dict[str, Any]:
        out: dict[str, Any] = {}
        if "loaded_kernel_keys" in rc and isinstance(rc["loaded_kernel_keys"], list):
            out["loaded_kernel_keys"] = list(rc["loaded_kernel_keys"])
        if rc.get("dispatch_manifest_digest"):
            out["dispatch_manifest_digest"] = str(rc["dispatch_manifest_digest"])
        if rc.get("kernel_load_error"):
            out["kernel_load_error"] = str(rc["kernel_load_error"])
        return out

    async def get_kernel_summary(self) -> dict[str, Any]:
        local = get_process_kernel_report(engine="SGLang")
        tm = self._get_tokenizer_manager()
        get_state_fn = (
            getattr(tm, "get_internal_state", None) if tm is not None else None
        )
        if callable(get_state_fn):
            try:
                raw_res = get_state_fn()
                if inspect.isawaitable(raw_res):
                    raw_states = await asyncio.wait_for(raw_res, timeout=2.0)
                else:
                    raw_states = raw_res
                rank_reports = [
                    self._extract_rank_kernel_fields(st[STATE_NAMESPACE_KEY])
                    for st in (raw_states or ())
                    if isinstance(st, dict)
                    and isinstance(st.get(STATE_NAMESPACE_KEY), dict)
                ]
                rank_reports = [r for r in rank_reports if r]
                if rank_reports:
                    rank_summary = aggregate_kernel_reports(rank_reports)
                    if rank_summary:
                        self._last_kernel_summary = dict(rank_summary)
                    candidates = [*rank_reports, local] if local else list(rank_reports)
                    merged = aggregate_kernel_reports(candidates)
                    if merged:
                        self._last_kernel_summary = dict(merged)
                        return merged
            except Exception:
                LOGGER.debug(
                    "SGLang get_internal_state in get_kernel_summary failed",
                    exc_info=True,
                )

        if self._last_kernel_summary and local:
            merged = aggregate_kernel_reports([self._last_kernel_summary, local])
            self._last_kernel_summary = dict(merged)
            return merged
        if self._last_kernel_summary:
            return dict(self._last_kernel_summary)
        if local:
            self._last_kernel_summary = dict(local)
        return local

    async def _stop_sglang_profiler_best_effort(self, tm: Any) -> None:
        stop_fn = getattr(tm, "stop_profile", None)
        if callable(stop_fn):
            try:
                res = stop_fn()
                if inspect.isawaitable(res):
                    await res
            except Exception:
                LOGGER.debug(
                    "SGLang stop_profile best-effort call failed", exc_info=True
                )

    async def start_probe(self, spec: dict[str, Any]) -> dict[str, Any]:
        tm = self._get_tokenizer_manager()
        if tm is None:
            raise UnsupportedEngineVersionError("TokenizerManager is not initialized")
        start_profile_fn = getattr(tm, "start_profile", None)
        if not callable(start_profile_fn):
            raise UnsupportedEngineVersionError(
                "SGLang TokenizerManager does not expose start_profile"
            )

        output_dir = Path(str(spec["output_dir"]))
        output_dir.mkdir(parents=True, exist_ok=True)
        steps = int(spec["steps"])
        probe_id = str(spec["probe_id"])
        desired_kwargs: dict[str, Any] = {
            "output_dir": str(output_dir),
            "num_steps": steps,
            "activities": ["CPU", "GPU"],
            "record_shapes": True,
        }

        use_profile_req = False
        call_kwargs: dict[str, Any] = desired_kwargs
        try:
            sig = inspect.signature(start_profile_fn)
            params = sig.parameters
            has_var_kw = any(
                p.kind == inspect.Parameter.VAR_KEYWORD for p in params.values()
            )
            if "req" in params and not has_var_kw and "output_dir" not in params:
                use_profile_req = True
            elif not has_var_kw:
                call_kwargs = {k: v for k, v in desired_kwargs.items() if k in params}
        except (TypeError, ValueError):
            call_kwargs = desired_kwargs

        started = time.monotonic()
        try:
            if use_profile_req:
                req_obj: Any
                try:
                    from sglang.srt.managers.io_struct import ProfileReq

                    req_obj = ProfileReq(
                        output_dir=str(output_dir),
                        num_steps=steps,
                        activities=["CPU", "GPU"],
                        record_shapes=True,
                        with_stack=False,
                        profile_id=probe_id,
                    )
                except Exception:
                    from types import SimpleNamespace

                    req_obj = SimpleNamespace(
                        output_dir=str(output_dir),
                        num_steps=steps,
                        activities=["CPU", "GPU"],
                        record_shapes=True,
                        with_stack=False,
                        profile_id=probe_id,
                    )
                res = start_profile_fn(req=req_obj)
            else:
                res = start_profile_fn(**call_kwargs)
            if inspect.isawaitable(res):
                await res
        except Exception as exc:
            purge_probe_dir(output_dir)
            raise SentinelError(
                500, "probe_start_failed", f"SGLang start_profile failed: {exc}"
            ) from exc

        self._active_probe = {
            "probe_id": str(spec["probe_id"]),
            "output_dir": output_dir,
            "steps": steps,
            "max_shm_bytes": int(spec.get("max_shm_bytes") or PROBE_MAX_SHM_BYTES),
            "wall_clock_cap_seconds": float(
                spec.get("wall_clock_cap_seconds") or PROBE_WALL_CLOCK_CAP_SECONDS
            ),
            "engine_version": str(spec.get("engine_version") or "0.0.0"),
            "manufacturer": str(spec.get("manufacturer") or "nvidia"),
            "architecture": str(spec.get("architecture") or "unknown"),
            "started_monotonic": started,
        }
        return {
            "ok": True,
            "status": "running",
            "probe_id": str(spec["probe_id"]),
        }

    async def collect_probe(self, spec: dict[str, Any]) -> dict[str, Any]:
        probe_id = str(spec.get("probe_id") or "")
        active = self._active_probe
        if active is None or (probe_id and active["probe_id"] != probe_id):
            return {"ok": True, "status": "idle", "probe_id": probe_id}

        tm = self._get_tokenizer_manager()
        output_dir: Path = active["output_dir"]
        max_shm_bytes: int = active["max_shm_bytes"]
        wall_cap: float = active["wall_clock_cap_seconds"]
        elapsed = time.monotonic() - float(active["started_monotonic"])

        if elapsed > wall_cap:
            try:
                if tm is not None:
                    await self._stop_sglang_profiler_best_effort(tm)
            finally:
                purge_probe_dir(output_dir)
                self._active_probe = None
            return {
                "ok": True,
                "status": "failed",
                "probe_id": probe_id,
                "error_code": "probe_timeout",
                "message": f"SGLang probe exceeded {wall_cap}s wall-clock cap",
            }

        staged_bytes = directory_size_bytes(output_dir)
        if staged_bytes > max_shm_bytes:
            try:
                if tm is not None:
                    await self._stop_sglang_profiler_best_effort(tm)
            finally:
                purge_probe_dir(output_dir)
                self._active_probe = None
            return {
                "ok": True,
                "status": "failed",
                "probe_id": probe_id,
                "error_code": "probe_shm_budget_exceeded",
                "message": (
                    f"SGLang probe staging exceeded {max_shm_bytes} byte ceiling ({staged_bytes} bytes)"
                ),
            }

        # Wait while TokenizerManager reports active profiling unless rank-0 trace is ready.
        is_profiling = (
            bool(getattr(tm, "is_profiling", False)) if tm is not None else False
        )
        trace_file = select_rank0_trace_file(output_dir)
        if is_profiling or trace_file is None:
            return {"ok": True, "status": "running", "probe_id": probe_id}

        try:
            payload = distill_probe_directory(
                output_dir,
                engine="SGLang",
                engine_version=active["engine_version"],
                manufacturer=active["manufacturer"],
                architecture=active["architecture"],
                steps_captured=active["steps"],
                wall_clock_seconds=elapsed,
            )
            return {
                "ok": True,
                "status": "completed",
                "probe_id": probe_id,
                "payload": payload,
            }
        finally:
            # Always delete ALL TP ranks' trace files immediately after distilling TP rank 0.
            purge_probe_dir(output_dir)
            self._active_probe = None

    async def abort_probe(self, spec: dict[str, Any]) -> dict[str, Any]:
        probe_id = str(spec.get("probe_id") or "")
        tm = self._get_tokenizer_manager()
        output_dir_str = str(spec.get("output_dir") or "")
        active = self._active_probe
        output_dir = (
            active["output_dir"]
            if active is not None
            else (Path(output_dir_str) if output_dir_str else None)
        )
        try:
            if tm is not None:
                await self._stop_sglang_profiler_best_effort(tm)
        finally:
            if output_dir is not None:
                purge_probe_dir(output_dir)
            if active is not None and (not probe_id or active["probe_id"] == probe_id):
                self._active_probe = None
        return {"ok": True, "status": "aborted", "probe_id": probe_id}

    def _get_tokenizer_manager(self) -> Any:
        get_gs = getattr(self._http_server, "get_global_state", None)
        gs = (
            get_gs()
            if callable(get_gs)
            else getattr(self._http_server, "_global_state", None)
        )
        if gs is None:
            return None
        return getattr(gs, "tokenizer_manager", None)

    def _make_set_req(self, server_args: dict[str, Any]) -> Any:
        from sglang.srt.managers.io_struct import SetInternalStateReq

        return SetInternalStateReq(server_args=server_args)

    async def check_live(self) -> tuple[bool, bool, str | None]:
        tm = self._get_tokenizer_manager()
        if tm is None:
            return True, False, None
        if bool(getattr(tm, "gracefully_exit", False)):
            return False, False, "shutting_down"

        # Inspect subprocess watchdog processes without triggering token generation.
        watchdog = getattr(tm, "_subprocess_watchdog", None)
        procs = getattr(watchdog, "_processes", None)
        if isinstance(procs, list):
            for idx, proc in enumerate(procs):
                is_alive_fn = getattr(proc, "is_alive", None)
                exitcode = getattr(proc, "exitcode", None)
                if (
                    callable(is_alive_fn)
                    and not is_alive_fn()
                    and exitcode not in (None, 0)
                ):
                    return (
                        False,
                        False,
                        f"scheduler_subprocess_{idx}_crashed_exit_{exitcode}",
                    )

        server_status = getattr(tm, "server_status", None)
        status_name = str(getattr(server_status, "name", server_status or ""))
        if status_name == "UnHealthy":
            return False, False, "server_unhealthy"

        # Read local TokenizerManager pause state directly without fanning out ZMQ RPCs on every probe.
        paused = bool(getattr(tm, "is_pause", False))
        return True, paused, None

    async def check_ready(self) -> tuple[bool, str | None]:
        tm = self._get_tokenizer_manager()
        if tm is None:
            return False, "engine_starting"
        server_status = getattr(tm, "server_status", None)
        status_name = str(getattr(server_status, "name", server_status or ""))
        if status_name == "Starting":
            return False, "engine_starting"
        is_ready_fn = getattr(tm, "is_ready", None)
        if callable(is_ready_fn):
            ready_val = is_ready_fn()
            if inspect.isawaitable(ready_val):
                ready_val = await ready_val
            if not bool(ready_val):
                if bool(getattr(tm, "is_pause", False)):
                    return False, "role_paused"
                return False, "engine_starting"
        if bool(getattr(tm, "is_pause", False)):
            return False, "role_paused"
        return True, None

    def _aggregate_internal_states(
        self, raw_states: list[dict[str, Any]]
    ) -> dict[str, Any]:
        if not raw_states:
            return {
                "membership_epoch": self._membership_epoch,
                "supported_fields": [],
                "unsupported_fields": list(SGLANG_ALL_FIELDS),
                "boot_defaults": {},
                "ranks": [],
                "values": {},
            }

        if self._last_rank_count is None:
            self._last_rank_count = len(raw_states)
        elif self._last_rank_count != len(raw_states):
            self._membership_epoch += 1
            self._last_rank_count = len(raw_states)

        supported_sets: list[set[str]] = []
        ranks: list[dict[str, Any]] = []
        rank_epochs: list[int] = []
        seen_rank_ids: set[int] = set()
        boot_defaults: dict[str, Any] = {}

        for idx, st in enumerate(raw_states):
            rc = st.get(STATE_NAMESPACE_KEY) if isinstance(st, dict) else None
            if not isinstance(rc, dict):
                raise SentinelError(
                    503,
                    "incomplete_rank_membership",
                    f"SGLang scheduler rank {idx} is missing sentinel state",
                )
            bid = str(rc.get("boot_id", ""))
            if not bid:
                raise SentinelError(
                    503,
                    "incomplete_rank_membership",
                    f"SGLang scheduler rank {idx} reported empty boot_id",
                )
            rid = int(rc.get("rank_id", idx))
            if rid in seen_rank_ids:
                raise SentinelError(
                    503,
                    "incomplete_rank_membership",
                    f"Duplicate SGLang scheduler rank_id {rid} in internal state",
                )
            seen_rank_ids.add(rid)
            supported_sets.append(set(rc.get("supported_fields", ())))
            epoch = int(rc.get("membership_epoch", 1))
            rank_epochs.append(epoch)
            if not boot_defaults and isinstance(rc.get("boot_defaults"), dict):
                boot_defaults = dict(rc["boot_defaults"])
            k_keys = (
                list(rc["loaded_kernel_keys"])
                if isinstance(rc.get("loaded_kernel_keys"), list)
                else None
            )
            k_digest = (
                str(rc["dispatch_manifest_digest"])
                if rc.get("dispatch_manifest_digest")
                else None
            )
            k_error = (
                str(rc["kernel_load_error"]) if rc.get("kernel_load_error") else None
            )
            receipt = RankReceipt(
                rank_id=rid,
                boot_id=bid,
                membership_epoch=epoch,
                applied_digest=str(rc.get("applied_digest", "")),
                values=dict(rc.get("values", {})),
                loaded_kernel_keys=k_keys,
                dispatch_manifest_digest=k_digest,
                kernel_load_error=k_error,
            )
            ranks.append(receipt.to_dict())

        common_supported = (
            sorted(set.intersection(*supported_sets)) if supported_sets else []
        )
        all_unsupported = sorted(set(SGLANG_ALL_FIELDS) - set(common_supported))
        ranks.sort(key=lambda r: r["rank_id"])
        top_values = dict(ranks[0]["values"]) if ranks else {}
        out: dict[str, Any] = {
            "membership_epoch": max([self._membership_epoch, *rank_epochs]),
            "supported_fields": common_supported,
            "unsupported_fields": all_unsupported,
            "boot_defaults": boot_defaults,
            "ranks": ranks,
            "values": top_values,
        }
        ksummary = aggregate_kernel_reports(ranks)
        if ksummary:
            self._last_kernel_summary = dict(ksummary)
            out.update(ksummary)
        return out

    async def get_runtime_config(self) -> dict[str, Any]:
        tm = self._get_tokenizer_manager()
        if tm is None:
            raise SentinelError(
                503, "engine_not_ready", "TokenizerManager is not initialized"
            )
        raw_states = await tm.get_internal_state()
        return self._aggregate_internal_states(list(raw_states or ()))

    async def apply_runtime_config(
        self, request_data: dict[str, Any]
    ) -> dict[str, Any]:
        tm = self._get_tokenizer_manager()
        if tm is None:
            raise SentinelError(
                503, "engine_not_ready", "TokenizerManager is not initialized"
            )

        before_states = list((await tm.get_internal_state()) or ())
        if not before_states:
            raise SentinelError(
                503,
                "incomplete_rank_membership",
                "SGLang TokenizerManager reported no scheduler ranks",
            )
        before_rcs: list[dict[str, Any]] = []
        before_snapshots_by_boot: dict[str, dict[str, Any]] = {}
        before_digests_by_boot: dict[str, str] = {}
        before_rank_boots: set[tuple[int, str]] = set()
        for idx, st in enumerate(before_states):
            rc = st.get(STATE_NAMESPACE_KEY) if isinstance(st, dict) else None
            if not isinstance(rc, dict) or not rc.get("boot_id"):
                raise SentinelError(
                    503,
                    "incomplete_rank_membership",
                    f"SGLang scheduler rank {idx} is missing sentinel state before apply",
                )
            bid = str(rc["boot_id"])
            rid = int(rc.get("rank_id", idx))
            before_rcs.append(rc)
            before_snapshots_by_boot[bid] = dict(rc.get("snapshot", {}))
            before_digests_by_boot[bid] = str(rc.get("applied_digest", ""))
            before_rank_boots.add((rid, bid))

        async def _rollback_all() -> None:
            try:
                rb_results = await tm.set_internal_state(
                    self._make_set_req(
                        {ROLLBACK_BUNDLE_KEY: {"by_boot_id": before_snapshots_by_boot}}
                    )
                )
                rb_ok = (
                    bool(rb_results)
                    and len(rb_results) == len(before_rcs)
                    and all(res is True for res in rb_results)
                )
                if rb_ok:
                    rb_states = list((await tm.get_internal_state()) or ())
                    rb_rcs = [
                        st.get(STATE_NAMESPACE_KEY, {}) if isinstance(st, dict) else {}
                        for st in rb_states
                    ]
                    rb_rank_boots = {
                        (int(rc.get("rank_id", -1)), str(rc.get("boot_id", "")))
                        for rc in rb_rcs
                    }
                    rb_ok = (
                        len(rb_rcs) == len(before_rcs)
                        and rb_rank_boots == before_rank_boots
                        and all(
                            str(rc.get("boot_id", "")) in before_digests_by_boot
                            and str(rc.get("applied_digest", ""))
                            == before_digests_by_boot.get(str(rc.get("boot_id", "")))
                            for rc in rb_rcs
                        )
                    )
            except Exception:
                rb_ok = False
            if not rb_ok:
                raise SentinelError(
                    500,
                    "rollback_incomplete",
                    "SGLang per-rank rollback did not restore all scheduler ranks",
                )

        target_digest = str(request_data.get("applied_config_digest", ""))
        bundle = {
            "applied_config_digest": target_digest,
            "parameters": request_data.get("parameters", {}),
            "reset_fields": request_data.get("reset_fields", []),
        }
        try:
            bool_results = await tm.set_internal_state(
                self._make_set_req({APPLY_BUNDLE_KEY: bundle})
            )
            after_states = list((await tm.get_internal_state()) or ())
        except Exception as exc:
            await _rollback_all()
            raise SentinelError(
                500,
                "rank_apply_failed",
                f"SGLang apply or readback failed: {exc}",
            ) from exc

        # Every single rank boolean must be strictly True and count must match membership.
        all_updated = (
            bool(bool_results)
            and len(bool_results) == len(before_rcs)
            and all(res is True for res in bool_results)
        )
        after_rcs = [
            st.get(STATE_NAMESPACE_KEY, {}) if isinstance(st, dict) else {}
            for st in after_states
        ]
        after_rank_boots = {
            (int(rc.get("rank_id", -1)), str(rc.get("boot_id", ""))) for rc in after_rcs
        }
        digests_match = (
            len(after_rcs) == len(before_rcs)
            and after_rank_boots == before_rank_boots
            and all(
                rc.get("applied_digest") == target_digest and bool(rc.get("boot_id"))
                for rc in after_rcs
            )
        )

        if not all_updated or not digests_match:
            clean_validation_refusal = (
                bool(bool_results)
                and len(bool_results) == len(before_rcs)
                and all(res is False for res in bool_results)
                and len(after_rcs) == len(before_rcs)
                and after_rank_boots == before_rank_boots
                and all(
                    str(rc.get("boot_id", "")) in before_digests_by_boot
                    and str(rc.get("applied_digest", ""))
                    == before_digests_by_boot.get(str(rc.get("boot_id", "")))
                    and rc.get("last_error_code")
                    in ("unsupported_fields", "invalid_argument")
                    for rc in after_rcs
                )
            )
            if not clean_validation_refusal:
                await _rollback_all()

            unsupported_union: set[str] = set()
            error_messages: list[str] = []
            for rc in after_rcs:
                code = rc.get("last_error_code")
                if code == "unsupported_fields":
                    unsupported_union.update(rc.get("last_unsupported_fields", ()))
                elif rc.get("last_error_message"):
                    error_messages.append(str(rc["last_error_message"]))

            if unsupported_union:
                supported_sets = [
                    set(rc.get("supported_fields", ()))
                    for rc in after_rcs
                    if "supported_fields" in rc
                ]
                common_sup = (
                    sorted(set.intersection(*supported_sets)) if supported_sets else []
                )
                raise UnsupportedFieldsError(
                    sorted(unsupported_union),
                    supported_fields=common_sup,
                )
            if error_messages:
                raise InvalidRequestError("; ".join(error_messages))
            raise SentinelError(
                500,
                "rank_apply_failed",
                f"SGLang set_internal_state did not converge across all ranks: {bool_results!r}",
            )

        return self._aggregate_internal_states(after_states)


def patch_sglang_http_server_module(mod: Any) -> None:
    """Attach SentinelControlPlane and outermost pause middleware onto SGLang's FastAPI app."""
    app = getattr(mod, "app", None)
    if app is None or getattr(app, "_gpustack_sentinel_installed", False):
        return

    backend = SGLangSentinelBackend(mod)
    plane = SentinelControlPlane(backend=backend, latch=BootLatch())
    plane.install_routes(app)
    ensure_outermost_sentinel_middleware(app, control_plane=plane)
    app._gpustack_sentinel_installed = True
    app._gpustack_sentinel_plane = plane
