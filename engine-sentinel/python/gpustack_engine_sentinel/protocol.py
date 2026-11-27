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
PROBE_PATH = "/gpustack/v1/probe"

ENV_ENABLE = "GPUSTACK_ENGINE_SENTINEL_ENABLE"
ENV_REQUIRE_ACK = "GPUSTACK_ENGINE_SENTINEL_REQUIRE_ACK"
ENV_PROBE_MIN_COOLDOWN_SECONDS = "GPUSTACK_PROBE_MIN_COOLDOWN_SECONDS"
ENV_PROBE_AUTH_TOKEN = "GPUSTACK_PROBE_AUTH_TOKEN"
ENV_PROBE_SHM_DIR = "GPUSTACK_PROBE_SHM_DIR"
ENV_PROBE_SHM_SIZE_BYTES = "GPUSTACK_PROBE_SHM_SIZE_BYTES"
ENV_KERNEL_ARTIFACT_DIR = "GPUSTACK_KERNEL_ARTIFACT_DIR"

DEFAULT_KERNEL_ARTIFACT_DIR = "/var/lib/gpustack/kernels"
DEFAULT_PROBE_SHM_DIR = "/dev/shm/gpustack-probe"
DEFAULT_SHM_SIZE_BYTES = 16 * 1024 * 1024 * 1024
DEFAULT_PROBE_MIN_COOLDOWN_SECONDS = 60.0
DEFAULT_PROBE_STEPS = 4
MIN_PROBE_STEPS = 3
MAX_PROBE_STEPS = 5
PROBE_WALL_CLOCK_CAP_SECONDS = 15.0
PROBE_MAX_PAYLOAD_BYTES = 512 * 1024
PROBE_MAX_SHM_BYTES = 64 * 1024 * 1024
PROBE_SHM_FRACTION = 0.05
PROBE_STALE_AGE_SECONDS = 60.0
DISPATCH_MANIFEST_SCHEMA_VERSION = "gpustack-forge-dispatch/v1"

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


class UnauthorizedError(SentinelError):
    """Missing or invalid probe authentication token (HTTP 401)."""

    def __init__(
        self, message: str = "Missing or invalid probe authorization token"
    ) -> None:
        super().__init__(401, "unauthorized", message)


class RolePausedError(SentinelError):
    """Probe refused because the role is paused (HTTP 409)."""

    def __init__(self, *, boot_id: str) -> None:
        super().__init__(
            409,
            "role_paused",
            "Cannot start probe while role is paused",
            extra={"boot_id": boot_id},
        )


class ProbeInProgressError(SentinelError):
    """A probe is already running on this engine boot (HTTP 409)."""

    def __init__(self, *, boot_id: str, probe_id: str) -> None:
        super().__init__(
            409,
            "probe_in_progress",
            f"Probe {probe_id} is already in progress",
            extra={"boot_id": boot_id, "probe_id": probe_id},
        )


class ProbeCooldownError(SentinelError):
    """Per-boot probe cooldown floor has not elapsed (HTTP 429)."""

    def __init__(
        self,
        *,
        boot_id: str,
        retry_after_seconds: float,
        cooldown_seconds: float,
    ) -> None:
        retry_after = max(0.0, round(retry_after_seconds, 3))
        super().__init__(
            429,
            "probe_cooldown",
            f"Probe cooldown active; retry after {retry_after}s",
            extra={
                "boot_id": boot_id,
                "retry_after_seconds": retry_after,
                "cooldown_seconds": cooldown_seconds,
            },
        )
        self.retry_after_seconds = retry_after


class UnsupportedEngineVersionError(SentinelError):
    """Running engine lacks the required probe worker/scheduler capability (HTTP 422)."""

    def __init__(self, message: str, *, extra: dict[str, Any] | None = None) -> None:
        super().__init__(422, "unsupported_engine_version", message, extra=extra)


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
    loaded_kernel_keys: list[str] | None = None
    dispatch_manifest_digest: str | None = None
    kernel_load_error: str | None = None

    def to_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {
            "ready": self.ready,
            "boot_id": self.boot_id,
            "observed_generation": self.observed_generation,
            "applied_config_digest": self.applied_config_digest,
        }
        if self.reason is not None:
            out["reason"] = self.reason
        if self.loaded_kernel_keys is not None:
            out["loaded_kernel_keys"] = list(self.loaded_kernel_keys)
        if self.dispatch_manifest_digest:
            out["dispatch_manifest_digest"] = self.dispatch_manifest_digest
        if self.kernel_load_error:
            out["kernel_load_error"] = self.kernel_load_error
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
    loaded_kernel_keys: list[str] | None = None
    dispatch_manifest_digest: str | None = None
    kernel_load_error: str | None = None

    def to_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {
            "rank_id": self.rank_id,
            "boot_id": self.boot_id,
            "membership_epoch": self.membership_epoch,
            "applied_digest": self.applied_digest,
            "values": self.values,
        }
        if self.loaded_kernel_keys is not None:
            out["loaded_kernel_keys"] = list(self.loaded_kernel_keys)
        if self.dispatch_manifest_digest:
            out["dispatch_manifest_digest"] = self.dispatch_manifest_digest
        if self.kernel_load_error:
            out["kernel_load_error"] = self.kernel_load_error
        return out


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
    loaded_kernel_keys: list[str] | None = None
    dispatch_manifest_digest: str | None = None
    kernel_load_error: str | None = None

    def to_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {
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
        if self.loaded_kernel_keys is not None:
            out["loaded_kernel_keys"] = list(self.loaded_kernel_keys)
        if self.dispatch_manifest_digest:
            out["dispatch_manifest_digest"] = self.dispatch_manifest_digest
        if self.kernel_load_error:
            out["kernel_load_error"] = self.kernel_load_error
        return out


@dataclass(frozen=True)
class ProbeRequest:
    expected_boot_id: str
    steps: int = DEFAULT_PROBE_STEPS
    engine_version: str | None = None
    manufacturer: str | None = None
    architecture: str | None = None
    metadata: dict[str, Any] = field(default_factory=dict)

    @classmethod
    def from_dict(cls, data: Any) -> ProbeRequest:
        if not isinstance(data, dict):
            raise InvalidRequestError("Request body must be a JSON object")
        expected_boot_id = data.get("expected_boot_id", "")
        if not isinstance(expected_boot_id, str) or not expected_boot_id.strip():
            raise InvalidRequestError("expected_boot_id must be a non-empty string")

        raw_steps = data.get("steps", DEFAULT_PROBE_STEPS)
        if raw_steps is None:
            steps = DEFAULT_PROBE_STEPS
        else:
            steps = parse_strict_int(
                raw_steps,
                "steps",
                minimum=MIN_PROBE_STEPS,
                maximum=MAX_PROBE_STEPS,
            )

        def _opt_str(key: str) -> str | None:
            raw = data.get(key)
            if raw is None:
                return None
            if not isinstance(raw, str):
                raise InvalidRequestError(f"{key} must be a string when provided")
            trimmed = raw.strip()
            return trimmed or None

        raw_meta = data.get("metadata")
        if raw_meta is None:
            metadata: dict[str, Any] = {}
        elif isinstance(raw_meta, dict):
            # Preserve only non-sensitive scalar hardware/engine descriptors.
            allowed_meta_keys = {
                "engine_version",
                "manufacturer",
                "architecture",
                "arch",
                "tp_size",
                "ep_size",
                "dp_size",
                "role",
                "dtype",
            }
            metadata = {
                str(k): v
                for k, v in raw_meta.items()
                if str(k) in allowed_meta_keys
                and isinstance(v, (str, int, float, bool))
            }
        else:
            raise InvalidRequestError("metadata must be an object when provided")

        engine_version = _opt_str("engine_version") or (
            str(metadata["engine_version"]).strip()
            if isinstance(metadata.get("engine_version"), str)
            and str(metadata["engine_version"]).strip()
            else None
        )
        manufacturer = _opt_str("manufacturer") or (
            str(metadata["manufacturer"]).strip()
            if isinstance(metadata.get("manufacturer"), str)
            and str(metadata["manufacturer"]).strip()
            else None
        )
        architecture = (
            _opt_str("architecture")
            or _opt_str("arch")
            or (
                str(metadata["architecture"]).strip()
                if isinstance(metadata.get("architecture"), str)
                and str(metadata["architecture"]).strip()
                else None
            )
            or (
                str(metadata["arch"]).strip()
                if isinstance(metadata.get("arch"), str)
                and str(metadata["arch"]).strip()
                else None
            )
        )

        return cls(
            expected_boot_id=expected_boot_id.strip(),
            steps=steps,
            engine_version=engine_version,
            manufacturer=manufacturer,
            architecture=architecture,
            metadata=metadata,
        )
