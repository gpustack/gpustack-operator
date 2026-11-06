"""Shared helpers for the SGLang adapter tests: fixture location and reads."""

import hashlib
import json
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


def element_size(dtype: str) -> int:
    return {"F32": 4, "F16": 2, "BF16": 2, "I64": 8, "I32": 4, "I8": 1}[dtype]


def tensor_values(name: str) -> list:
    import struct

    entry = tensor_entry(name)
    payload = tensor_payload(name)
    if entry["dtype"] == "F32":
        return list(struct.unpack(f"<{len(payload) // 4}f", payload))
    if entry["dtype"] == "F16":
        return list(struct.unpack(f"<{len(payload) // 2}e", payload))
    if entry["dtype"] == "I64":
        return list(struct.unpack(f"<{len(payload) // 8}q", payload))
    raise AssertionError(f"unexpected dtype {entry['dtype']}")


def tied_manifest_dir(tmp_dir, *, drop_lm_head: bool = False):
    """A fixture copy whose manifest may omit lm_head.weight (tied models)."""
    import shutil

    root = Path(tmp_dir)
    shutil.copytree(fixture_dir(), root, dirs_exist_ok=True)
    data = manifest_dict()
    if drop_lm_head:
        data["tensors"] = [t for t in data["tensors"] if t["name"] != "lm_head.weight"]
    raw = json.dumps(data, sort_keys=True, separators=(",", ":")).encode() + b"\n"
    (root / "manifest.json").write_bytes(raw)
    return root
