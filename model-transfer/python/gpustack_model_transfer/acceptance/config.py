"""Strict acceptance-configuration validation.

The configuration file is the contract between the operator and the runner:
every field is known, typed, fresh, and consistent, or the run refuses to
start. Nothing defaults and nothing is guessed. Freshness is judged against
an injectable clock (now), defaulting to the current UTC time, so tests and
replayed runs never depend on the real date.
"""

import hashlib
import json
import math
from datetime import datetime, timezone
from pathlib import Path

from gpustack_model_transfer.core.budgets import ReadBudgets
from gpustack_model_transfer.core.manifest import load_manifest


class RefusedError(Exception):
    """The run refuses to start; the reason explains why."""

    def __init__(self, reason: str):
        super().__init__(reason)
        self.reason = reason


_TOP_KEYS = {
    "schema_version",
    "backend",
    "suite",
    "artifact",
    "readiness",
    "limits",
    "deadlines",
}
_ARTIFACT_KEYS = {"manifest_path", "manifest_sha256"}
_READINESS_KEYS = {"source_generation", "expires"}
_LIMIT_KEYS = {"window_bytes", "max_inflight", "owned_output_bytes"}
_DEADLINE_KEYS = {"operation_seconds"}


def _reject_duplicate_keys(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise RefusedError(f"duplicate configuration key: {key!r}")
        result[key] = value
    return result


def _exact_keys(obj, keys, where: str) -> None:
    if not isinstance(obj, dict):
        raise RefusedError(f"{where} must be a JSON object")
    if set(obj) != set(keys):
        missing = sorted(set(keys) - set(obj))
        unknown = sorted(set(obj) - set(keys))
        raise RefusedError(
            f"{where} keys mismatch: missing={missing} unknown={unknown}"
        )


def _require_int(obj, key, where: str, minimum=None) -> int:
    value = obj[key]
    if isinstance(value, bool) or not isinstance(value, int):
        raise RefusedError(f"{where}.{key} must be an integer")
    if minimum is not None and value < minimum:
        raise RefusedError(f"{where}.{key} must be >= {minimum}, got {value}")
    return value


def load_and_validate(
    path: str, backend: str, suite: str, engine, now: datetime | None = None
) -> dict:
    """Load the config file and check it against the requested run.

    now is the instant readiness freshness is judged against; None means
    the current UTC time. Returns a validated context with the parsed
    config, the resolved manifest path, and the loaded manifest. Raises
    RefusedError on any problem.
    """
    if now is None:
        now = datetime.now(timezone.utc)
    try:
        raw = Path(path).read_bytes()
    except OSError as exc:
        raise RefusedError(f"cannot read configuration {path!r}: {exc}") from None
    try:
        config = json.loads(raw, object_pairs_hook=_reject_duplicate_keys)
    except json.JSONDecodeError as exc:
        raise RefusedError(f"configuration is not valid JSON: {exc}") from None
    if not isinstance(config, dict):
        raise RefusedError("configuration must be a JSON object")
    _exact_keys(config, _TOP_KEYS, "configuration")
    if config["schema_version"] != 1:
        raise RefusedError(
            f"unsupported configuration schema_version "
            f"{config['schema_version']!r}; expected 1"
        )
    if config["backend"] != backend:
        raise RefusedError(
            f"configuration backend {config['backend']!r} does not match the "
            f"requested backend {backend!r}"
        )
    if config["suite"] != suite:
        raise RefusedError(
            f"configuration suite {config['suite']!r} does not match the "
            f"requested suite {suite!r}"
        )

    artifact = config["artifact"]
    _exact_keys(artifact, _ARTIFACT_KEYS, "artifact")
    manifest_path = artifact["manifest_path"]
    if not isinstance(manifest_path, str) or not manifest_path:
        raise RefusedError("artifact.manifest_path must be a non-empty string")
    if not Path(manifest_path).is_absolute():
        manifest_path = str(Path(path).resolve().parent / manifest_path)
    expected_digest = artifact["manifest_sha256"]
    if (
        not isinstance(expected_digest, str)
        or len(expected_digest) != 64
        or not all(ch in "0123456789abcdef" for ch in expected_digest)
    ):
        raise RefusedError(
            "artifact.manifest_sha256 must be 64 lowercase hex characters"
        )
    try:
        manifest_bytes = Path(manifest_path).read_bytes()
    except OSError as exc:
        raise RefusedError(f"cannot read manifest {manifest_path!r}: {exc}") from None
    actual_digest = hashlib.sha256(manifest_bytes).hexdigest()
    if actual_digest != expected_digest:
        raise RefusedError(
            f"manifest digest mismatch: configuration pins {expected_digest} "
            f"but the file digests to {actual_digest}"
        )

    readiness = config["readiness"]
    _exact_keys(readiness, _READINESS_KEYS, "readiness")
    generation = _require_int(readiness, "source_generation", "readiness", 1)
    expires = readiness["expires"]
    if not isinstance(expires, str):
        raise RefusedError("readiness.expires must be an ISO-8601 string")
    try:
        expires_at = datetime.fromisoformat(expires)
    except ValueError:
        raise RefusedError(f"readiness.expires is not ISO-8601: {expires!r}") from None
    if expires_at.tzinfo is None:
        raise RefusedError("readiness.expires must carry a timezone")
    if expires_at <= now:
        raise RefusedError(
            f"stale readiness: expired at {expires} (now {now.isoformat()})"
        )

    limits = config["limits"]
    _exact_keys(limits, _LIMIT_KEYS, "limits")
    try:
        ReadBudgets(
            window_bytes=_require_int(limits, "window_bytes", "limits", 1),
            max_inflight=_require_int(limits, "max_inflight", "limits", 1),
            owned_output_bytes=_require_int(limits, "owned_output_bytes", "limits", 1),
        ).validate()
    except Exception as exc:
        raise RefusedError(f"limits are invalid: {exc}") from None

    deadlines = config["deadlines"]
    _exact_keys(deadlines, _DEADLINE_KEYS, "deadlines")
    operation_seconds = deadlines["operation_seconds"]
    if (
        isinstance(operation_seconds, bool)
        or not isinstance(operation_seconds, (int, float))
        or not math.isfinite(operation_seconds)
        or operation_seconds <= 0
    ):
        raise RefusedError("deadlines.operation_seconds must be positive")

    try:
        manifest = load_manifest(manifest_path, expected_sha256=expected_digest)
    except Exception as exc:
        raise RefusedError(f"manifest failed verification: {exc}") from None
    if manifest.source_generation != generation:
        raise RefusedError(
            f"stale generation: manifest declares "
            f"{manifest.source_generation}, configuration readiness declares "
            f"{generation}"
        )

    return {
        "config": config,
        "manifest_path": manifest_path,
        "manifest": manifest,
        "operation_seconds": operation_seconds,
    }


__all__ = ["RefusedError", "load_and_validate"]
