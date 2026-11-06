"""Shared helpers for the HIXL backend tests."""

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


def load_manifest():
    from gpustack_model_transfer.core.local_reader import LocalFileReader

    reader = LocalFileReader(str(manifest_path()))
    try:
        return reader.manifest
    finally:
        reader.close()


def source_files() -> dict:
    """The fixture's real checkpoint bytes keyed as the reader names them."""
    blob = (fixture_dir() / "checkpoint.safetensors").read_bytes()
    return {"checkpoint.safetensors": blob}


# Deterministic remote base addresses for the published regions, one per
# manifest file, so tests and suites agree on where the source's
# registered bytes live.
REGION_BASE = 0x500000000000


def region_base(index: int) -> int:
    return REGION_BASE * (index + 1)


def peer(manifest=None) -> dict:
    """The peer descriptor: an endpoint plus one published remote region
    per checkpoint file, address-based, exactly as the reader submits."""
    manifest = manifest or load_manifest()
    regions = {}
    for index, descriptor in enumerate(manifest.files):
        regions[descriptor.path] = {
            "addr": region_base(index),
            "len": descriptor.size,
        }
    return {"endpoint": "hixl://source:9700", "regions": regions}


def publish_regions(api, manifest=None):
    """Register the fixture's real bytes as the source's remote regions."""
    manifest = manifest or load_manifest()
    for index, descriptor in enumerate(manifest.files):
        blob = (fixture_dir() / descriptor.path).read_bytes()
        api.publish_remote_region(region_base(index), blob)
    return api


def default_limits(**overrides):
    limits = {"window_bytes": 32, "max_inflight": 2, "owned_output_bytes": 4096}
    limits.update(overrides)
    return limits


def default_context(backend="hixl", suite="backend", **limits):
    """A validated acceptance context for one hixl suite invocation."""
    from gpustack_model_transfer.acceptance.config import load_and_validate

    config = {
        "schema_version": 1,
        "backend": backend,
        "suite": suite,
        "artifact": {
            "manifest_path": str(manifest_path()),
            "manifest_sha256": manifest_digest(),
        },
        "readiness": {
            "source_generation": 1,
            "expires": "2031-01-01T00:00:00+00:00",
        },
        "limits": default_limits(**limits),
        "deadlines": {"operation_seconds": 30},
    }
    with tempfile.TemporaryDirectory() as tmp:
        path = os.path.join(tmp, "acceptance.json")
        with open(path, "w") as fh:
            json.dump(config, fh)
        return load_and_validate(
            path, backend, suite, None, now=datetime(2030, 1, 1, tzinfo=timezone.utc)
        )
