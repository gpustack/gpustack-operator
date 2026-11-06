"""Shared helpers for the model-transfer adapter and contract tests: fixtures and derived trees."""

import hashlib
import json
import shutil
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


def manifest_dir() -> Path:
    return fixture_dir()


def manifest_bytes() -> bytes:
    return manifest_path().read_bytes()


def manifest_digest() -> str:
    return hashlib.sha256(manifest_bytes()).hexdigest()


def manifest_dict() -> dict:
    return json.loads(manifest_bytes())


def blob_bytes() -> bytes:
    return (fixture_dir() / "checkpoint.safetensors").read_bytes()


def tensor_payload(name: str) -> bytes:
    m = manifest_dict()
    tensors = {t["name"]: t for t in m["tensors"]}
    blob = blob_bytes()
    t = tensors[name]
    return blob[t["data_begin"] : t["data_end"]]


def tensor_entry(name: str) -> dict:
    for t in manifest_dict()["tensors"]:
        if t["name"] == name:
            return t
    raise AssertionError(f"unknown fixture tensor: {name!r}")


def parameter_names() -> list:
    return sorted(
        t["name"]
        for t in manifest_dict()["tensors"]
        if not t["name"].startswith("metadata.")
    )


def write_tied_fixture_without_lm_head(tmp: str) -> Path:
    """Write a checkpoint tree whose manifest omits lm_head.weight."""
    root = Path(tmp)
    shutil.copy(
        fixture_dir() / "checkpoint.safetensors", root / "checkpoint.safetensors"
    )
    manifest = manifest_dict()
    manifest["tensors"] = [
        t for t in manifest["tensors"] if t["name"] != "lm_head.weight"
    ]
    (root / "manifest.json").write_bytes(json.dumps(manifest).encode())

    return root
