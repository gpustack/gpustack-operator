"""Shared helpers for the ModelExpress adapter tests."""

import hashlib
import json
import os
import tempfile
from datetime import datetime, timezone
from pathlib import Path

FIXTURE_ROOT = (
    Path(__file__).resolve().parents[3] / "contracts" / "fixtures" / "tiny_dense"
)


def fixture_dir() -> Path:
    if not FIXTURE_ROOT.is_dir():
        raise AssertionError(f"missing fixture directory: {FIXTURE_ROOT}")
    return FIXTURE_ROOT


def manifest_path() -> Path:
    return fixture_dir() / "manifest.json"


def manifest_digest() -> str:
    return hashlib.sha256(manifest_path().read_bytes()).hexdigest()


def default_limits(**overrides):
    limits = {"window_bytes": 32, "max_inflight": 2, "owned_output_bytes": 4096}
    limits.update(overrides)
    return limits


def model_load_config():
    return {
        "schema_version": 1,
        "backend": "nixl",
        "suite": "model-load",
        "artifact": {
            "manifest_path": str(manifest_path()),
            "manifest_sha256": manifest_digest(),
        },
        "readiness": {
            "source_generation": 1,
            "expires": "2031-01-01T00:00:00+00:00",
        },
        "limits": default_limits(),
        "deadlines": {"operation_seconds": 30},
    }


def load_context(engine="modelexpress"):
    from gpustack_model_transfer.acceptance.config import load_and_validate

    with tempfile.TemporaryDirectory() as tmp:
        path = os.path.join(tmp, "acceptance.json")
        with open(path, "w") as fh:
            json.dump(model_load_config(), fh)
        return load_and_validate(
            path,
            "nixl",
            "model-load",
            engine,
            now=datetime(2030, 1, 1, tzinfo=timezone.utc),
        )
