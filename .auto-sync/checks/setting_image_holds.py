#!/usr/bin/env python3
"""Load, validate, and enforce .auto-sync/setting-image-holds.json."""

from __future__ import annotations

import json
import os
import re
import sys

KV_TRANSFER_HOLD_KEYS = frozenset(
    {
        "mooncake",
        "modelexpress",
        "model-transfer",
        "model-transfer.nvidia",
        "model-transfer.amd",
        "model-transfer.ascend",
    }
)
ROUTER_HOLD_KEYS = frozenset({"router", "proxy", "sidecar"})
ALLOWED_HOLD_KEYS = KV_TRANSFER_HOLD_KEYS | ROUTER_HOLD_KEYS
_MT_PREFIX_RE = re.compile(r"^v\d+\.\d+\.\d+$")


def extract_ref_tag(ref: str) -> str:
    last_seg = ref.split("/")[-1]
    if ":" not in last_seg:
        return ""
    return ref.rsplit(":", 1)[1].strip()


def resolve_holds_file_for_preflight(repo_root: str, fixture_dir: str = "") -> str:
    env_path = os.environ.get("AUTOSYNC_SETTING_IMAGE_HOLDS_FILE", "").strip()
    if env_path:
        if not os.path.isfile(env_path):
            raise SystemExit(
                f"FAIL: AUTOSYNC_SETTING_IMAGE_HOLDS_FILE does not exist: {env_path}"
            )
        return env_path
    if fixture_dir:
        fix_path = os.path.join(fixture_dir, "setting-image-holds.json")
        return fix_path if os.path.isfile(fix_path) else ""
    live_path = os.path.join(repo_root, ".auto-sync", "setting-image-holds.json")
    return live_path if os.path.isfile(live_path) else ""


def resolve_holds_file_for_validate(
    baseline_dir: str, candidate_root: str, domain: str
) -> str:
    env_path = os.environ.get("AUTOSYNC_SETTING_IMAGE_HOLDS_FILE", "").strip()
    if env_path:
        if not os.path.isfile(env_path):
            raise SystemExit(
                f"FAIL: AUTOSYNC_SETTING_IMAGE_HOLDS_FILE does not exist: {env_path}"
            )
        return env_path

    if domain == "kv-transfer":
        fixture_dir = os.environ.get("AUTOSYNC_KV_FIXTURE_DIR", "").strip()
    elif domain == "router":
        fixture_dir = os.environ.get("AUTOSYNC_ROUTER_FIXTURE_DIR", "").strip()
    else:
        raise SystemExit(f"FAIL: unsupported holds domain: {domain!r}")

    if fixture_dir:
        fix_path = os.path.join(fixture_dir, "setting-image-holds.json")
        return fix_path if os.path.isfile(fix_path) else ""

    base_holds = os.path.join(baseline_dir, ".auto-sync", "setting-image-holds.json")
    base_root = os.environ.get("AUTOSYNC_BASELINE_ROOT", "").strip()
    base_root_holds = (
        os.path.join(base_root, ".auto-sync", "setting-image-holds.json")
        if base_root
        else ""
    )
    cand_holds = os.path.join(candidate_root, ".auto-sync", "setting-image-holds.json")

    trusted_base = ""
    if os.path.isfile(base_holds):
        trusted_base = base_holds
    elif base_root_holds and os.path.isfile(base_root_holds):
        trusted_base = base_root_holds

    if trusted_base:
        if not os.path.isfile(cand_holds):
            raise SystemExit(
                "FAIL: candidate removed .auto-sync/setting-image-holds.json (outside writable scope)"
            )
        with open(trusted_base, "rb") as bf, open(cand_holds, "rb") as cf:
            if bf.read() != cf.read():
                raise SystemExit(
                    "FAIL: candidate modified .auto-sync/setting-image-holds.json (outside writable scope)"
                )
        return trusted_base

    if os.path.isfile(cand_holds):
        return cand_holds
    return ""


def load_setting_image_holds(path: str) -> dict[str, dict[str, str]]:
    if not path:
        return {}
    try:
        with open(path, "r", encoding="utf-8") as f:
            doc = json.load(f)
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise SystemExit(
            f"FAIL: cannot parse setting image holds file {path}: {exc}"
        ) from exc

    if not isinstance(doc, dict):
        raise SystemExit(f"FAIL: {path} must be a JSON object")
    if set(doc.keys()) != {"schema_version", "holds"}:
        raise SystemExit(
            f"FAIL: {path} must contain exactly 'schema_version' and 'holds' (got {sorted(doc.keys())})"
        )
    schema_version = doc.get("schema_version")
    if isinstance(schema_version, bool) or schema_version != 1:
        raise SystemExit(
            f"FAIL: {path} schema_version must be 1 (got {schema_version!r})"
        )
    raw_holds = doc.get("holds")
    if not isinstance(raw_holds, dict):
        raise SystemExit(f"FAIL: {path} 'holds' must be a JSON object")

    parsed: dict[str, dict[str, str]] = {}
    for key, entry in raw_holds.items():
        if key not in ALLOWED_HOLD_KEYS:
            raise SystemExit(
                f"FAIL: unknown key {key!r} in {path}; allowed keys: {sorted(ALLOWED_HOLD_KEYS)}"
            )
        if not isinstance(entry, dict):
            raise SystemExit(f"FAIL: holds.{key} in {path} must be a JSON object")
        if set(entry.keys()) != {"tag", "reason"}:
            raise SystemExit(
                f"FAIL: holds.{key} in {path} must contain exactly 'tag' and 'reason' (got {sorted(entry.keys())})"
            )
        tag = entry.get("tag")
        reason = entry.get("reason")
        if (
            not isinstance(tag, str)
            or not tag
            or tag != tag.strip()
            or any(ch.isspace() for ch in tag)
            or "/" in tag
            or ":" in tag
        ):
            raise SystemExit(
                f"FAIL: holds.{key}.tag in {path} must be a non-empty image tag without whitespace, '/', or ':' (got {tag!r})"
            )
        if key == "model-transfer" and not _MT_PREFIX_RE.fullmatch(tag):
            raise SystemExit(
                f"FAIL: holds.model-transfer.tag in {path} must be a vX.Y.Z prefix (got {tag!r})"
            )
        if not isinstance(reason, str) or not reason.strip():
            raise SystemExit(
                f"FAIL: holds.{key}.reason in {path} must be a non-empty string"
            )
        parsed[key] = {"tag": tag, "reason": reason.strip()}

    if "model-transfer" in parsed:
        mt_prefix = parsed["model-transfer"]["tag"]
        for vendor in ("nvidia", "amd", "ascend"):
            vk = f"model-transfer.{vendor}"
            if vk in parsed and not parsed[vk]["tag"].startswith(f"{mt_prefix}-"):
                raise SystemExit(
                    f"FAIL: conflicting holds in {path}: model-transfer ({mt_prefix!r}) vs {vk} ({parsed[vk]['tag']!r})"
                )

    return parsed


def domain_holds(
    holds: dict[str, dict[str, str]], domain: str
) -> dict[str, dict[str, str]]:
    if domain == "kv-transfer":
        allowed = KV_TRANSFER_HOLD_KEYS
    elif domain == "router":
        allowed = ROUTER_HOLD_KEYS
    else:
        raise SystemExit(f"FAIL: unsupported holds domain: {domain!r}")
    return {k: dict(v) for k, v in holds.items() if k in allowed}


def verify_kv_transfer_holds(
    holds: dict[str, dict[str, str]],
    kv_img: str,
    mx_img: str,
    mt_images: dict[str, str],
    holds_label: str,
) -> None:
    if "mooncake" in holds:
        held_tag = holds["mooncake"]["tag"]
        cur_tag = extract_ref_tag(kv_img)
        if cur_tag != held_tag:
            raise SystemExit(
                f"FAIL: setting-image hold 'mooncake' expects tag {held_tag!r} per {holds_label}, "
                f"but KVCacheBackendImage is {kv_img!r} (tag {cur_tag!r}); update {holds_label} or value.go"
            )
    if "modelexpress" in holds:
        held_tag = holds["modelexpress"]["tag"]
        cur_tag = extract_ref_tag(mx_img)
        if cur_tag != held_tag:
            raise SystemExit(
                f"FAIL: setting-image hold 'modelexpress' expects tag {held_tag!r} per {holds_label}, "
                f"but ModelDeploymentModelExpressServerImage is {mx_img!r} (tag {cur_tag!r}); update {holds_label} or value.go"
            )
    if "model-transfer" in holds:
        mt_prefix = holds["model-transfer"]["tag"]
        for vendor in ("nvidia", "amd", "ascend"):
            cur_ref = mt_images.get(vendor, "")
            cur_tag = extract_ref_tag(cur_ref)
            if not cur_tag.startswith(f"{mt_prefix}-"):
                raise SystemExit(
                    f"FAIL: setting-image hold 'model-transfer' expects tag prefix {mt_prefix + '-*'} per {holds_label}, "
                    f"but modelTransfer.images.{vendor} is {cur_ref!r} (tag {cur_tag!r}); update {holds_label} or values.yaml"
                )
    for vendor in ("nvidia", "amd", "ascend"):
        vk = f"model-transfer.{vendor}"
        if vk in holds:
            held_tag = holds[vk]["tag"]
            cur_ref = mt_images.get(vendor, "")
            cur_tag = extract_ref_tag(cur_ref)
            if cur_tag != held_tag:
                raise SystemExit(
                    f"FAIL: setting-image hold '{vk}' expects tag {held_tag!r} per {holds_label}, "
                    f"but modelTransfer.images.{vendor} is {cur_ref!r} (tag {cur_tag!r}); update {holds_label} or values.yaml"
                )


def verify_router_holds(
    holds: dict[str, dict[str, str]],
    router_img: str,
    proxy_img: str,
    sidecar_img: str,
    holds_label: str,
) -> None:
    mapping = (
        ("router", "ModelDeploymentRouterImage", router_img),
        ("proxy", "ModelDeploymentRouterProxyImage", proxy_img),
        ("sidecar", "ModelDeploymentRoutingSidecarImage", sidecar_img),
    )
    for key, var_name, img in mapping:
        if key not in holds:
            continue
        held_tag = holds[key]["tag"]
        cur_tag = extract_ref_tag(img)
        if cur_tag != held_tag:
            raise SystemExit(
                f"FAIL: setting-image hold '{key}' expects tag {held_tag!r} per {holds_label}, "
                f"but {var_name} is {img!r} (tag {cur_tag!r}); update {holds_label} or value.go"
            )


if __name__ == "__main__":
    if len(sys.argv) != 2:
        raise SystemExit("usage: setting_image_holds.py <holds_json_path>")
    loaded = load_setting_image_holds(sys.argv[1])
    print(json.dumps(loaded, indent=2))
