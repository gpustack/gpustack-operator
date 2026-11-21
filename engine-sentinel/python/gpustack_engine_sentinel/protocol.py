"""Frozen /gpustack/v1/* HTTP wire protocol types, field constants, and validators."""

from __future__ import annotations

import math
import re
from dataclasses import dataclass, field
from typing import Any

READY_PATH = "/gpustack/v1/ready"
LIVE_PATH = "/gpustack/v1/live"
ACK_PATH = "/gpustack/v1/ack"
PAUSE_PATH = "/gpustack/v1/pause"
RUNTIME_CONFIG_PATH = "/gpustack/v1/runtime_config"

ENV_ENABLE = "GPUSTACK_ENGINE_SENTINEL_ENABLE"
ENV_REQUIRE_ACK = "GPUSTACK_ENGINE_SENTINEL_REQUIRE_ACK"

ENGINE_VLLM = "vllm"
ENGINE_SGLANG = "sglang"

FIELD_MAX_ACTIVE_REQUESTS = "scheduling.maxActiveRequests"
FIELD_MAX_BATCHED_TOKENS = "scheduling.maxBatchedTokens"
FIELD_LONG_PREFILL_TOKEN_THRESHOLD = "scheduling.longPrefillTokenThreshold"
FIELD_PREFILL_MAX_REQUESTS = "scheduling.prefillMaxRequests"
FIELD_SCHEDULE_CONSERVATIVENESS = "scheduling.scheduleConservativeness"
FIELD_PREFILL_DECODE_INTERVAL = "scheduling.prefillDecodeInterval"
FIELD_CACHE_AWARE_WINDOW = "scheduling.cacheAwareWindow"
FIELD_CACHE_AWARE_THRESHOLD = "scheduling.cacheAwareThreshold"
FIELD_PP_MAX_MICRO_BATCH_SIZE = "scheduling.ppMaxMicroBatchSize"
FIELD_MAX_INFLIGHT_KV_LOADS = "scheduling.maxInflightKVLoads"
FIELD_MAX_ASYNC_KV_LOADS_PER_STEP = "scheduling.maxAsyncKVLoadsPerStep"

FIELD_ACCEPT_THRESHOLD_SINGLE = "speculative.acceptThresholdSingle"
FIELD_ACCEPT_THRESHOLD_ACC = "speculative.acceptThresholdAcc"
FIELD_DRAFT_BUDGET_FRACTION = "speculative.draftBudgetFraction"

VLLM_ALL_FIELDS: tuple[str, ...] = (
    FIELD_MAX_ACTIVE_REQUESTS,
    FIELD_MAX_BATCHED_TOKENS,
    FIELD_LONG_PREFILL_TOKEN_THRESHOLD,
    FIELD_CACHE_AWARE_WINDOW,
    FIELD_CACHE_AWARE_THRESHOLD,
    FIELD_MAX_INFLIGHT_KV_LOADS,
    FIELD_MAX_ASYNC_KV_LOADS_PER_STEP,
)

SGLANG_ALL_FIELDS: tuple[str, ...] = (
    FIELD_MAX_ACTIVE_REQUESTS,
    FIELD_MAX_BATCHED_TOKENS,
    FIELD_PREFILL_MAX_REQUESTS,
    FIELD_SCHEDULE_CONSERVATIVENESS,
    FIELD_PREFILL_DECODE_INTERVAL,
    FIELD_PP_MAX_MICRO_BATCH_SIZE,
    FIELD_ACCEPT_THRESHOLD_SINGLE,
    FIELD_ACCEPT_THRESHOLD_ACC,
    FIELD_DRAFT_BUDGET_FRACTION,
)

ALL_CANONICAL_FIELDS: frozenset[str] = frozenset(VLLM_ALL_FIELDS) | frozenset(
    SGLANG_ALL_FIELDS
)

_LEAF_TO_CANONICAL: dict[str, str] = {
    canonical.split(".", 1)[1]: canonical for canonical in ALL_CANONICAL_FIELDS
}
_LEAF_TO_CANONICAL["dspark_force_budget_frac"] = FIELD_DRAFT_BUDGET_FRACTION

_INT_STR_RE = re.compile(r"^[+-]?\d+$")


class SentinelError(Exception):
    """Structured HTTP error raised by engine sentinel handlers."""

    def __init__(
        self,
        status_code: int,
        error_code: str,
        message: str,
        *,
        extra: dict[str, Any] | None = None,
    ) -> None:
        super().__init__(message)
        self.status_code = status_code
        self.error_code = error_code
        self.message = message
        self.extra = extra or {}

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "error": self.error_code,
            "message": self.message,
        }
        payload.update(self.extra)
        return payload


class InvalidRequestError(SentinelError):
    """Malformed JSON payload or out-of-bounds parameter value (HTTP 400)."""

    def __init__(self, message: str, *, extra: dict[str, Any] | None = None) -> None:
        super().__init__(400, "invalid_argument", message, extra=extra)


class BootIDMismatchError(SentinelError):
    """Fenced operation whose boot_id does not match this process (HTTP 409)."""

    def __init__(self, *, current_boot_id: str, expected_boot_id: str) -> None:
        super().__init__(
            409,
            "boot_id_mismatch",
            f"boot_id mismatch: current={current_boot_id}, expected={expected_boot_id}",
            extra={
                "boot_id": current_boot_id,
                "expected_boot_id": expected_boot_id,
            },
        )


class UnsupportedFieldsError(SentinelError):
    """Requested fields are unsupported on the running engine/config (HTTP 422)."""

    def __init__(
        self,
        unsupported_fields: list[str],
        *,
        supported_fields: list[str] | None = None,
        extra: dict[str, Any] | None = None,
    ) -> None:
        merged_extra: dict[str, Any] = {
            "unsupported_fields": sorted(set(unsupported_fields)),
        }
        if supported_fields is not None:
            merged_extra["supported_fields"] = sorted(set(supported_fields))
        if extra:
            merged_extra.update(extra)
        super().__init__(
            422,
            "unsupported_fields",
            f"Unsupported runtime control fields: {', '.join(sorted(set(unsupported_fields)))}",
            extra=merged_extra,
        )


def normalize_field_name(raw_name: str) -> str:
    """Resolve a canonical dotted field name or known leaf name."""
    if not isinstance(raw_name, str) or not raw_name.strip():
        raise InvalidRequestError("Field name must be a non-empty string")
    name = raw_name.strip()
    if name in ALL_CANONICAL_FIELDS:
        return name
    resolved = _LEAF_TO_CANONICAL.get(name)
    if resolved is not None:
        return resolved
    raise InvalidRequestError(f"Unknown runtime control field: {name!r}")


def parse_strict_int(
    value: Any,
    field_name: str,
    *,
    minimum: int | None = None,
    maximum: int | None = None,
) -> int:
    """Validate an integer or canonical integer string, rejecting booleans and floats."""
    if isinstance(value, bool):
        raise InvalidRequestError(f"{field_name} must be an integer, got {value!r}")
    if isinstance(value, int):
        parsed = value
    elif isinstance(value, str) and _INT_STR_RE.match(value.strip()):
        parsed = int(value.strip())
    else:
        raise InvalidRequestError(f"{field_name} must be an integer, got {value!r}")

    if minimum is not None and parsed < minimum:
        raise InvalidRequestError(f"{field_name} must be >= {minimum}, got {parsed}")
    if maximum is not None and parsed > maximum:
        raise InvalidRequestError(f"{field_name} must be <= {maximum}, got {parsed}")
    return parsed


def parse_decimal_float(
    value: Any,
    field_name: str,
    *,
    minimum: float | None = None,
    maximum: float | None = None,
    exclusive_min: bool = False,
) -> float:
    """Validate a float or decimal string, rejecting booleans and non-finite values."""
    if isinstance(value, bool):
        raise InvalidRequestError(f"{field_name} must be numeric, got {value!r}")
    if isinstance(value, int | float):
        parsed = float(value)
    elif isinstance(value, str) and value.strip():
        try:
            parsed = float(value.strip())
        except ValueError as exc:
            raise InvalidRequestError(
                f"{field_name} must be a decimal number, got {value!r}"
            ) from exc
    else:
        raise InvalidRequestError(
            f"{field_name} must be a number or decimal string, got {value!r}"
        )

    if not math.isfinite(parsed):
        raise InvalidRequestError(f"{field_name} must be finite, got {value!r}")
    if minimum is not None:
        if exclusive_min and parsed <= minimum:
            raise InvalidRequestError(f"{field_name} must be > {minimum}, got {parsed}")
        if not exclusive_min and parsed < minimum:
            raise InvalidRequestError(
                f"{field_name} must be >= {minimum}, got {parsed}"
            )
    if maximum is not None and parsed > maximum:
        raise InvalidRequestError(f"{field_name} must be <= {maximum}, got {parsed}")
    return parsed


@dataclass(frozen=True)
class ReadyResponse:
    ready: bool
    boot_id: str
    observed_generation: int | None
    applied_config_digest: str
    reason: str | None = None

    def to_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {
            "ready": self.ready,
            "boot_id": self.boot_id,
            "observed_generation": self.observed_generation,
            "applied_config_digest": self.applied_config_digest,
        }
        if self.reason is not None:
            out["reason"] = self.reason
        return out


@dataclass(frozen=True)
class LiveResponse:
    alive: bool
    boot_id: str
    paused: bool
    error: str | None = None

    def to_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {
            "alive": self.alive,
            "boot_id": self.boot_id,
            "paused": self.paused,
        }
        if self.error is not None:
            out["error"] = self.error
        return out


@dataclass(frozen=True)
class AckRequest:
    boot_id: str
    observed_generation: int
    applied_config_digest: str

    @classmethod
    def from_dict(cls, data: Any) -> AckRequest:
        if not isinstance(data, dict):
            raise InvalidRequestError("Request body must be a JSON object")
        boot_id = data.get("boot_id")
        if not isinstance(boot_id, str) or not boot_id.strip():
            raise InvalidRequestError("boot_id must be a non-empty string")
        observed_generation = parse_strict_int(
            data.get("observed_generation"), "observed_generation", minimum=0
        )
        digest = data.get("applied_config_digest", "")
        if not isinstance(digest, str):
            raise InvalidRequestError("applied_config_digest must be a string")
        return cls(
            boot_id=boot_id.strip(),
            observed_generation=observed_generation,
            applied_config_digest=digest,
        )


@dataclass(frozen=True)
class PauseRequest:
    paused: bool
    expected_boot_id: str | None = None

    @classmethod
    def from_dict(cls, data: Any) -> PauseRequest:
        if not isinstance(data, dict):
            raise InvalidRequestError("Request body must be a JSON object")
        paused = data.get("paused")
        if not isinstance(paused, bool):
            raise InvalidRequestError("paused must be a boolean")
        raw_boot = data.get("expected_boot_id")
        if raw_boot is None:
            expected_boot_id = None
        elif isinstance(raw_boot, str) and raw_boot.strip():
            expected_boot_id = raw_boot.strip()
        else:
            raise InvalidRequestError(
                "expected_boot_id must be a non-empty string when provided"
            )
        return cls(paused=paused, expected_boot_id=expected_boot_id)


@dataclass(frozen=True)
class PauseResponse:
    paused: bool
    boot_id: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "paused": self.paused,
            "boot_id": self.boot_id,
        }


@dataclass(frozen=True)
class RankReceipt:
    rank_id: int
    boot_id: str
    membership_epoch: int
    applied_digest: str
    values: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "rank_id": self.rank_id,
            "boot_id": self.boot_id,
            "membership_epoch": self.membership_epoch,
            "applied_digest": self.applied_digest,
            "values": self.values,
        }


def _collect_section_parameters(
    raw_section: Any, prefix: str, target: dict[str, Any]
) -> None:
    if raw_section is None:
        return
    if not isinstance(raw_section, dict):
        raise InvalidRequestError(f"{prefix} must be an object when provided")
    for k, v in raw_section.items():
        canonical = normalize_field_name(k if "." in str(k) else f"{prefix}.{k}")
        if not canonical.startswith(f"{prefix}."):
            raise InvalidRequestError(f"Field {k!r} does not belong to {prefix}")
        target[canonical] = v


@dataclass(frozen=True)
class RuntimeConfigRequest:
    operation_id: str
    expected_boot_id: str
    generation: int
    applied_config_digest: str
    parameters: dict[str, Any] = field(default_factory=dict)
    reset_fields: list[str] = field(default_factory=list)

    @classmethod
    def from_dict(cls, data: Any) -> RuntimeConfigRequest:
        if not isinstance(data, dict):
            raise InvalidRequestError("Request body must be a JSON object")
        operation_id = data.get("operation_id", "")
        if not isinstance(operation_id, str) or not operation_id.strip():
            raise InvalidRequestError("operation_id must be a non-empty string")
        expected_boot_id = data.get("expected_boot_id", "")
        if not isinstance(expected_boot_id, str) or not expected_boot_id.strip():
            raise InvalidRequestError("expected_boot_id must be a non-empty string")
        generation = parse_strict_int(
            data.get("generation", 0), "generation", minimum=0
        )
        digest = data.get("applied_config_digest", "")
        if not isinstance(digest, str):
            raise InvalidRequestError("applied_config_digest must be a string")

        parameters: dict[str, Any] = {}
        _collect_section_parameters(data.get("scheduling"), "scheduling", parameters)
        _collect_section_parameters(data.get("speculative"), "speculative", parameters)

        raw_params = data.get("parameters")
        if raw_params is not None:
            if not isinstance(raw_params, dict):
                raise InvalidRequestError("parameters must be an object when provided")
            for k, v in raw_params.items():
                canonical = normalize_field_name(k)
                parameters[canonical] = v

        raw_resets = data.get("reset_fields")
        if raw_resets is None:
            reset_fields: list[str] = []
        elif isinstance(raw_resets, list):
            reset_fields = []
            seen: set[str] = set()
            for item in raw_resets:
                canonical = normalize_field_name(item)
                if canonical not in seen:
                    seen.add(canonical)
                    reset_fields.append(canonical)
        else:
            raise InvalidRequestError("reset_fields must be a list of field names")

        return cls(
            operation_id=operation_id.strip(),
            expected_boot_id=expected_boot_id.strip(),
            generation=generation,
            applied_config_digest=digest,
            parameters=parameters,
            reset_fields=reset_fields,
        )


@dataclass(frozen=True)
class RuntimeConfigResponse:
    engine: str
    boot_id: str
    membership_epoch: int
    observed_generation: int | None
    applied_config_digest: str
    last_operation_id: str
    ready: bool
    paused: bool
    supported_fields: list[str]
    unsupported_fields: list[str]
    boot_defaults: dict[str, Any]
    ranks: list[dict[str, Any]]
    values: dict[str, Any]

    def to_dict(self) -> dict[str, Any]:
        return {
            "engine": self.engine,
            "boot_id": self.boot_id,
            "membership_epoch": self.membership_epoch,
            "observed_generation": self.observed_generation,
            "applied_config_digest": self.applied_config_digest,
            "last_operation_id": self.last_operation_id,
            "ready": self.ready,
            "paused": self.paused,
            "supported_fields": sorted(set(self.supported_fields)),
            "unsupported_fields": sorted(set(self.unsupported_fields)),
            "boot_defaults": dict(self.boot_defaults),
            "ranks": self.ranks,
            "values": dict(self.values),
        }
