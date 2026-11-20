"""EngineCore multi-rank utility RPC for vLLM online scheduler controls."""

from __future__ import annotations

import uuid
from dataclasses import dataclass, field
from typing import Any

from gpustack_engine_sentinel.protocol import (
    FIELD_CACHE_AWARE_THRESHOLD,
    FIELD_CACHE_AWARE_WINDOW,
    FIELD_LONG_PREFILL_TOKEN_THRESHOLD,
    FIELD_MAX_ACTIVE_REQUESTS,
    FIELD_MAX_ASYNC_KV_LOADS_PER_STEP,
    FIELD_MAX_BATCHED_TOKENS,
    FIELD_MAX_INFLIGHT_KV_LOADS,
    VLLM_ALL_FIELDS,
    InvalidRequestError,
    parse_decimal_float,
    parse_strict_int,
)

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


def execute_engine_core_rpc(
    engine_core: Any, payload: dict[str, Any]
) -> dict[str, Any]:
    """Execute inspect, apply, or rollback inside a vLLM EngineCore process."""
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
        return {
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
        return {
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
