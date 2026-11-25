#!/usr/bin/env python3
"""Compute and validate Base Image workflow build recommendations for .auto-sync/.

Maps modified pack/<dir>/ files (Dockerfile, patches, build context) to the exact
inputs of .github/workflows/base-image.yml so Stage A recipe syncs record actionable
`base_image_builds` entries in preflight reports, summaries, and proposal JSON files.
"""

from __future__ import annotations

import json
import os
import re
import shlex
import sys
from typing import Any

CHECKS_DIR = os.path.dirname(os.path.abspath(__file__))
if CHECKS_DIR not in sys.path:
    sys.path.insert(0, CHECKS_DIR)

from dockerfile_arg import has_dockerfile_arg, read_dockerfile_arg  # noqa: E402

WORKFLOW_NAME = "Base Image"

# Mapping from pack/<dir> name -> .github/workflows/base-image.yml repository metadata.
PACK_BASE_IMAGE_MAP: dict[str, dict[str, Any]] = {
    "llm-router": {
        "workflow_repository": "llm-router",
        "repository": "gpustack/llm-router",
        "domain": "router",
        "valid_targets": ("",),
    },
    "mirrored-mooncake": {
        "workflow_repository": "mirrored-mooncake",
        "repository": "gpustack/mirrored-mooncake",
        "domain": "kv-transfer",
        "valid_targets": ("", "cuda", "cann", "rocm"),
    },
    "model-transfer": {
        "workflow_repository": "model-transfer",
        "repository": "gpustack/model-transfer",
        "domain": "kv-transfer",
        "valid_targets": ("cuda", "rocm", "cann"),
    },
    "mirrored-modelexpress-server": {
        "workflow_repository": "mirrored-modelexpress-server",
        "repository": "gpustack/mirrored-modelexpress-server",
        "domain": "kv-transfer",
        "valid_targets": ("",),
    },
}

REPO_TO_WORKFLOW_REPO: dict[str, str] = {
    meta["repository"]: meta["workflow_repository"]
    for meta in PACK_BASE_IMAGE_MAP.values()
}

WORKFLOW_REPO_TO_VALID_TARGETS: dict[str, tuple[str, ...]] = {
    meta["workflow_repository"]: meta["valid_targets"]
    for meta in PACK_BASE_IMAGE_MAP.values()
}


def _read_text(path: str) -> str:
    if not os.path.isfile(path):
        return ""
    with open(path, "r", encoding="utf-8") as f:
        return f.read()


def _extract_value_go_image(root: str, var_name: str) -> str:
    txt = _read_text(os.path.join(root, "pkg", "worker", "settings", "value.go"))
    if not txt:
        return ""
    m = re.search(
        rf'{re.escape(var_name)}\s*=\s*settings\.NewEditable\([^)]*?setting\.InitializeFromEnv\("([^"]+)"\)',
        txt,
        re.DOTALL,
    )
    return m.group(1).strip() if m else ""


def _extract_values_yaml_mt_images(root: str) -> dict[str, str]:
    txt = _read_text(
        os.path.join(root, "deploy", "gpustack-operator", "chart", "values.yaml")
    )
    if not txt:
        return {}
    out: dict[str, str] = {}
    in_mt = False
    in_images = False
    for raw in txt.splitlines():
        if raw.strip() == "" or raw.lstrip().startswith("#"):
            continue
        if not raw.startswith(" "):
            in_mt = raw == "modelTransfer:" or raw.startswith("modelTransfer: ")
            in_images = False
        elif in_mt and re.match(r"^  [A-Za-z0-9_-]+:", raw):
            in_images = raw == "  images:" or raw.startswith("  images: ")
        elif in_mt and in_images:
            m = re.match(
                r'^    (nvidia|amd|ascend):\s*(?:"([^"]+)"|\'([^\']+)\'|([^\s#]+))',
                raw,
            )
            if m:
                out[m.group(1)] = (m.group(2) or m.group(3) or m.group(4)).strip()
    return out


def _bump_patch_semver(tag: str, fallback: str) -> str:
    m = re.fullmatch(r"(.*?)v(\d+)\.(\d+)\.(\d+)(.*)", tag.strip())
    if not m:
        return fallback
    prefix, major, minor, patch, suffix = m.groups()
    return f"{prefix}v{int(major)}.{int(minor)}.{int(patch) + 1}{suffix}"


def _image_tag(image_ref: str) -> str:
    last_seg = image_ref.split("/")[-1]
    if ":" not in last_seg:
        return ""
    return image_ref.rsplit(":", 1)[1].strip()


def target_slug(target: str) -> str:
    """Mirror .github/workflows/base-image.yml Validate Inputs slug calculation."""
    slug = re.sub(r"[^a-z0-9.-]", "-", target.lower())
    slug = re.sub(r"-+", "-", slug).strip(".-")
    return slug or "variant"


def format_workflow_command(workflow_repo: str, tag: str, target: str = "") -> str:
    parts = [
        "gh",
        "workflow",
        "run",
        f'"{WORKFLOW_NAME}"',
        "-f",
        f"repository={workflow_repo}",
        "-f",
        f"tag={tag}",
    ]
    if target:
        parts.extend(["-f", f"target={target}"])
    return " ".join(parts)


def _read_arg_safe(df_path: str, arg_name: str, default: str = "") -> str:
    txt = _read_text(df_path)
    if not txt or not has_dockerfile_arg(txt, arg_name):
        return default
    try:
        return read_dockerfile_arg(txt, arg_name, df_path)
    except SystemExit:
        return default


def _parse_all_dockerfile_args(txt: str) -> dict[str, str]:
    args: dict[str, str] = {}
    pat = re.compile(r"^[ \t]*ARG[ \t]+([A-Z0-9_]+)=(.*)$")
    for line in txt.splitlines():
        m = pat.match(line)
        if not m:
            continue
        name = m.group(1)
        if name in args:
            continue
        try:
            args[name] = read_dockerfile_arg(line, name)
        except SystemExit:
            pass
    return args


def _recommended_tag_for(
    pack_name: str,
    target: str,
    root: str,
    override_versions: dict[str, str] | None = None,
) -> str:
    overrides = override_versions or {}
    df_path = os.path.join(root, "pack", pack_name, "Dockerfile")

    if pack_name == "llm-router":
        cur_img = _extract_value_go_image(root, "ModelDeploymentRouterImage")
        cur_tag = _image_tag(cur_img)
        return _bump_patch_semver(cur_tag, "v0.1.0")

    if pack_name == "mirrored-mooncake":
        ver = (
            overrides.get("mooncake")
            or _read_arg_safe(df_path, "MOONCAKE_VERSION", "")
            or "0.0.0"
        )
        ver = ver.lstrip("v")
        if not target:
            return f"{ver}-cpu"
        return f"{ver}-{target_slug(target)}"

    if pack_name == "mirrored-modelexpress-server":
        ver = (
            overrides.get("modelexpress")
            or _read_arg_safe(df_path, "MODELEXPRESS_VERSION", "")
            or "v0.1.0"
        )
        return ver

    if pack_name == "model-transfer":
        mt_imgs = _extract_values_yaml_mt_images(root)
        vendor_key = {"cuda": "nvidia", "rocm": "amd", "cann": "ascend"}.get(target, "")
        cur_img = mt_imgs.get(vendor_key, "")
        cur_tag = _image_tag(cur_img)
        if cur_tag:
            bumped = _bump_patch_semver(cur_tag, "")
            if bumped and target_slug(target) in bumped:
                return bumped
        return f"v0.1.0-{target_slug(target)}"

    return "dev"


def _make_entry(
    pack_name: str,
    target: str,
    root: str,
    reason: str,
    override_versions: dict[str, str] | None = None,
) -> dict[str, str]:
    meta = PACK_BASE_IMAGE_MAP[pack_name]
    wf_repo = meta["workflow_repository"]
    tag = _recommended_tag_for(pack_name, target, root, override_versions)
    return {
        "repository": meta["repository"],
        "target": target,
        "recommended_tag": tag,
        "reason": reason,
        "command": format_workflow_command(wf_repo, tag, target),
    }


def _collect_pack_files(root: str, pack_name: str) -> dict[str, bytes]:
    pack_dir = os.path.join(root, "pack", pack_name)
    out: dict[str, bytes] = {}
    if not os.path.isdir(pack_dir):
        return out
    for cur, dirs, files in os.walk(pack_dir, followlinks=False):
        dirs[:] = sorted(d for d in dirs if d != ".git")
        for fn in sorted(files):
            fp = os.path.join(cur, fn)
            if os.path.islink(fp) or not os.path.isfile(fp):
                continue
            rel = os.path.relpath(fp, root).replace(os.sep, "/")
            with open(fp, "rb") as f:
                out[rel] = f.read()
    return out


def _changed_args_summary(base_df: str, cand_df: str) -> list[str]:
    base_args = _parse_all_dockerfile_args(base_df)
    cand_args = _parse_all_dockerfile_args(cand_df)
    changed: list[str] = []
    for k in sorted(set(base_args) | set(cand_args)):
        if base_args.get(k) != cand_args.get(k):
            if k in cand_args:
                changed.append(f"{k}={cand_args[k]}")
            else:
                changed.append(f"{k} removed")
    return changed


def _infer_model_transfer_targets(
    changed_files: list[str],
    base_df: str,
    cand_df: str,
) -> list[str]:
    targets: set[str] = set()
    df_rel = "pack/model-transfer/Dockerfile"
    for rel in changed_files:
        if rel.startswith("pack/model-transfer/patches/hixl/"):
            targets.add("cann")
        elif rel != df_rel:
            return ["cuda", "rocm", "cann"]

    if df_rel in changed_files:
        # Compare non-ARG lines to see if shared Dockerfile stages/instructions changed.
        def strip_vendor_args(txt: str) -> str:
            out_lines = []
            for ln in txt.splitlines(keepends=True):
                if re.match(
                    r"^[ \t]*ARG[ \t]+(?:CUDA_RUNTIME_IMAGE|NIXL_VERSION|NIXL_TORCH_VERSION|"
                    r"ROCM_RUNTIME_IMAGE|NIXL_ROCM_VERSION|NIXL_ROCM_REF|NIXL_ROCM_UCX_REF|NIXL_ROCM_TORCH_VERSION|"
                    r"CANN_RUNTIME_IMAGE|HIXL_COMMIT|HIXL_CMAKE_COMMIT)=",
                    ln,
                ):
                    continue
                out_lines.append(ln)
            return "".join(out_lines)

        if strip_vendor_args(base_df) != strip_vendor_args(cand_df):
            return ["cuda", "rocm", "cann"]

        base_args = _parse_all_dockerfile_args(base_df)
        cand_args = _parse_all_dockerfile_args(cand_df)
        changed_keys = {
            k for k in (set(base_args) | set(cand_args)) if base_args.get(k) != cand_args.get(k)
        }
        for k in changed_keys:
            if k in ("CUDA_RUNTIME_IMAGE", "NIXL_VERSION", "NIXL_TORCH_VERSION"):
                targets.add("cuda")
            elif k in (
                "ROCM_RUNTIME_IMAGE",
                "NIXL_ROCM_VERSION",
                "NIXL_ROCM_REF",
                "NIXL_ROCM_UCX_REF",
                "NIXL_ROCM_TORCH_VERSION",
            ):
                targets.add("rocm")
            elif k in ("CANN_RUNTIME_IMAGE", "HIXL_COMMIT", "HIXL_CMAKE_COMMIT"):
                targets.add("cann")
            else:
                return ["cuda", "rocm", "cann"]

    ordered = [t for t in ("cuda", "rocm", "cann") if t in targets]
    return ordered or ["cuda", "rocm", "cann"]


def recommend_from_diff(
    baseline_dir: str,
    candidate_dir: str,
    domain: str | None = None,
) -> list[dict[str, str]]:
    """Compute required Base Image builds for modified pack/<dir>/ trees."""
    builds: list[dict[str, str]] = []
    for pack_name, meta in sorted(PACK_BASE_IMAGE_MAP.items()):
        if domain and meta["domain"] != domain:
            continue
        base_files = _collect_pack_files(baseline_dir, pack_name)
        cand_files = _collect_pack_files(candidate_dir, pack_name)
        changed_files = sorted(
            rel
            for rel in (set(base_files) | set(cand_files))
            if base_files.get(rel) != cand_files.get(rel)
        )
        if not changed_files:
            continue

        df_rel = f"pack/{pack_name}/Dockerfile"
        base_df = base_files.get(df_rel, b"").decode("utf-8", errors="replace")
        cand_df = cand_files.get(df_rel, b"").decode("utf-8", errors="replace")
        arg_changes = _changed_args_summary(base_df, cand_df) if df_rel in changed_files else []

        reason_parts: list[str] = []
        if arg_changes:
            reason_parts.append(f"{df_rel} ({', '.join(arg_changes)})")
        elif df_rel in changed_files:
            reason_parts.append(df_rel)
        other_files = [rel for rel in changed_files if rel != df_rel]
        if other_files:
            shown = other_files[:4]
            extra = f" (+{len(other_files) - 4} more)" if len(other_files) > 4 else ""
            reason_parts.append(", ".join(shown) + extra)
        reason = "Modified " + "; ".join(reason_parts)

        if pack_name == "model-transfer":
            targets = _infer_model_transfer_targets(changed_files, base_df, cand_df)
        else:
            targets = [""]

        for target in targets:
            builds.append(_make_entry(pack_name, target, candidate_dir, reason))

    builds.sort(key=lambda item: (item["repository"], item["target"]))
    return builds


def recommend_for_preflight(
    repo_root: str,
    domain: str,
    stage: str,
    drifts: dict[str, bool],
    target_versions: dict[str, str] | None = None,
) -> list[dict[str, str]]:
    """Compute Stage A Base Image build recommendations during preflight."""
    if stage != "recipe_sync":
        return []
    versions = target_versions or {}
    builds: list[dict[str, str]] = []

    if domain == "router":
        drifted = [
            uid
            for uid in ("llm-d-router", "vllm-router", "sgl-router")
            if drifts.get(uid)
        ]
        if drifted:
            detail = ", ".join(
                f"{uid}={versions[uid]}" if versions.get(uid) else uid
                for uid in drifted
            )
            builds.append(
                _make_entry(
                    "llm-router",
                    "",
                    repo_root,
                    f"Stage A recipe sync for pack/llm-router/Dockerfile ({detail})",
                    versions,
                )
            )
    elif domain == "kv-transfer":
        if drifts.get("mooncake"):
            mc_ver = versions.get("mooncake", "")
            detail = f"MOONCAKE_VERSION={mc_ver}" if mc_ver else "mooncake"
            builds.append(
                _make_entry(
                    "mirrored-mooncake",
                    "",
                    repo_root,
                    f"Stage A recipe sync for pack/mirrored-mooncake/Dockerfile ({detail})",
                    versions,
                )
            )
        mt_targets: list[tuple[str, list[str]]] = []
        cuda_reasons = [
            f"NIXL_VERSION={versions['nixl-pypi']}" if versions.get("nixl-pypi") else "nixl-pypi"
        ] if drifts.get("nixl-pypi") else []
        if cuda_reasons:
            mt_targets.append(("cuda", cuda_reasons))

        rocm_reasons: list[str] = []
        if drifts.get("nixl-rocm") or drifts.get("nixl-pypi"):
            rocm_reasons.append(
                f"NIXL_ROCM_REF={versions['nixl-rocm']}" if versions.get("nixl-rocm") else "nixl-rocm"
            )
        if drifts.get("ucx-rocm"):
            rocm_reasons.append(
                f"NIXL_ROCM_UCX_REF={versions['ucx-rocm']}" if versions.get("ucx-rocm") else "ucx-rocm"
            )
        if rocm_reasons:
            mt_targets.append(("rocm", rocm_reasons))

        cann_reasons: list[str] = []
        if drifts.get("hixl"):
            cann_reasons.append(
                f"HIXL_COMMIT={versions['hixl']}" if versions.get("hixl") else "hixl"
            )
        if drifts.get("gpustack-runner"):
            cann_reasons.append("gpustack-runner HIXL patch parity")
        if cann_reasons:
            mt_targets.append(("cann", cann_reasons))

        for target, reasons in mt_targets:
            builds.append(
                _make_entry(
                    "model-transfer",
                    target,
                    repo_root,
                    f"Stage A recipe sync for pack/model-transfer/Dockerfile target {target} ({', '.join(reasons)})",
                    versions,
                )
            )

        if drifts.get("modelexpress"):
            mx_ver = versions.get("modelexpress", "")
            detail = f"MODELEXPRESS_VERSION={mx_ver}" if mx_ver else "modelexpress"
            builds.append(
                _make_entry(
                    "mirrored-modelexpress-server",
                    "",
                    repo_root,
                    f"Stage A recipe sync for pack/mirrored-modelexpress-server/Dockerfile ({detail})",
                    versions,
                )
            )

    builds.sort(key=lambda item: (item["repository"], item["target"]))
    return builds


def _validate_command(cmd: str, wf_repo: str, tag: str, target: str) -> str | None:
    try:
        tokens = shlex.split(cmd)
    except ValueError as exc:
        return f"command is not valid shell syntax: {exc}"
    if len(tokens) < 4 or tokens[:4] != ["gh", "workflow", "run", WORKFLOW_NAME]:
        return f'command must invoke `gh workflow run "{WORKFLOW_NAME}" ...`, got {cmd!r}'
    inputs: dict[str, str] = {}
    i = 4
    while i < len(tokens):
        tok = tokens[i]
        if tok in ("-f", "--raw-field", "-F", "--field"):
            if i + 1 >= len(tokens) or "=" not in tokens[i + 1]:
                return f"malformed {tok} input in command: {cmd!r}"
            k, v = tokens[i + 1].split("=", 1)
            inputs[k] = v
            i += 2
            continue
        i += 1
    if inputs.get("repository") != wf_repo:
        return f"command repository input ({inputs.get('repository')!r}) != expected {wf_repo!r}"
    if inputs.get("tag") != tag:
        return f"command tag input ({inputs.get('tag')!r}) != recommended_tag {tag!r}"
    if target:
        if inputs.get("target") != target:
            return f"command target input ({inputs.get('target')!r}) != expected {target!r}"
    else:
        if inputs.get("target", "") != "":
            return f"command must not set non-empty target input for default-stage build (got {inputs.get('target')!r})"
    return None


def validate_proposal_builds(
    baseline_dir: str,
    candidate_dir: str,
    proposal_path: str,
    domain: str,
) -> list[dict[str, str]]:
    """Validate proposal base_image_builds against modified pack/* files."""
    expected = recommend_from_diff(baseline_dir, candidate_dir, domain=domain)
    if not os.path.isfile(proposal_path):
        raise SystemExit(f"FAIL: missing proposal artifact: {proposal_path}")
    with open(proposal_path, "r", encoding="utf-8") as f:
        prop = json.load(f)
    if not isinstance(prop, dict):
        raise SystemExit(f"FAIL: {proposal_path} must be a JSON object")

    raw_builds = prop.get("base_image_builds")
    if raw_builds is None:
        if expected:
            missing_desc = ", ".join(
                f"{b['repository']}(target={b['target'] or 'default'})" for b in expected
            )
            raise SystemExit(
                f"FAIL: {proposal_path} missing base_image_builds for modified pack/* files; "
                f"required: {missing_desc}"
            )
        return expected

    if not isinstance(raw_builds, list):
        raise SystemExit(f"FAIL: {proposal_path} base_image_builds must be a JSON array")

    if not expected:
        if raw_builds:
            raise SystemExit(
                f"FAIL: {proposal_path} base_image_builds must be empty when no mapped pack/* files changed"
            )
        return expected

    seen_keys: set[tuple[str, str]] = set()
    for idx, item in enumerate(raw_builds):
        if not isinstance(item, dict):
            raise SystemExit(
                f"FAIL: {proposal_path} base_image_builds[{idx}] must be a JSON object"
            )
        repo = item.get("repository")
        target = item.get("target")
        tag = item.get("recommended_tag")
        reason = item.get("reason")
        cmd = item.get("command")

        if not isinstance(repo, str) or repo not in REPO_TO_WORKFLOW_REPO:
            raise SystemExit(
                f"FAIL: {proposal_path} base_image_builds[{idx}].repository ({repo!r}) "
                f"must be one of {sorted(REPO_TO_WORKFLOW_REPO)}"
            )
        wf_repo = REPO_TO_WORKFLOW_REPO[repo]
        valid_targets = WORKFLOW_REPO_TO_VALID_TARGETS[wf_repo]
        if not isinstance(target, str) or target not in valid_targets:
            raise SystemExit(
                f"FAIL: {proposal_path} base_image_builds[{idx}].target ({target!r}) "
                f"for {repo} must be one of {list(valid_targets)}"
            )
        if not isinstance(tag, str) or not tag.strip():
            raise SystemExit(
                f"FAIL: {proposal_path} base_image_builds[{idx}].recommended_tag must be a non-empty string"
            )
        if target:
            slug = target_slug(target)
            if slug not in tag:
                raise SystemExit(
                    f"FAIL: {proposal_path} base_image_builds[{idx}].recommended_tag ({tag!r}) "
                    f"must contain target slug {slug!r} per .github/workflows/base-image.yml"
                )
        if not isinstance(reason, str) or not reason.strip():
            raise SystemExit(
                f"FAIL: {proposal_path} base_image_builds[{idx}].reason must be a non-empty string"
            )
        if not isinstance(cmd, str) or not cmd.strip():
            raise SystemExit(
                f"FAIL: {proposal_path} base_image_builds[{idx}].command must be a non-empty string"
            )
        cmd_err = _validate_command(cmd, wf_repo, tag.strip(), target)
        if cmd_err:
            raise SystemExit(f"FAIL: {proposal_path} base_image_builds[{idx}]: {cmd_err}")

        key = (repo, target)
        if key in seen_keys:
            raise SystemExit(
                f"FAIL: {proposal_path} base_image_builds has duplicate entry for {repo} target={target!r}"
            )
        seen_keys.add(key)

    expected_keys = {(b["repository"], b["target"]) for b in expected}
    missing = sorted(expected_keys - seen_keys)
    if missing:
        missing_str = ", ".join(f"{r}(target={t or 'default'})" for r, t in missing)
        raise SystemExit(
            f"FAIL: {proposal_path} base_image_builds missing required build recommendation(s): {missing_str}"
        )
    extra = sorted(seen_keys - expected_keys)
    if extra:
        extra_str = ", ".join(f"{r}(target={t or 'default'})" for r, t in extra)
        raise SystemExit(
            f"FAIL: {proposal_path} base_image_builds has unexpected build recommendation(s): {extra_str}"
        )
    return expected


def main(argv: list[str]) -> int:
    args = argv[1:]
    if not args:
        raise SystemExit(
            "usage: base_image_builds.py diff <baseline_dir> <candidate_dir> [--domain <domain>]\n"
            "       base_image_builds.py validate <baseline_dir> <candidate_dir> <proposal_json> --domain <domain>"
        )
    mode = args[0]
    rest = args[1:]
    domain: str | None = None
    positional: list[str] = []
    i = 0
    while i < len(rest):
        if rest[i] == "--domain":
            if i + 1 >= len(rest):
                raise SystemExit("FAIL: --domain requires a value")
            domain = rest[i + 1].strip()
            i += 2
            continue
        positional.append(rest[i])
        i += 1

    if mode == "diff":
        if len(positional) != 2:
            raise SystemExit("FAIL: diff requires <baseline_dir> <candidate_dir>")
        builds = recommend_from_diff(positional[0], positional[1], domain=domain)
        print(json.dumps(builds, indent=2))
        return 0

    if mode == "validate":
        if len(positional) != 3 or not domain:
            raise SystemExit(
                "FAIL: validate requires <baseline_dir> <candidate_dir> <proposal_json> --domain <domain>"
            )
        validate_proposal_builds(positional[0], positional[1], positional[2], domain)
        return 0

    raise SystemExit(f"FAIL: unknown mode: {mode!r}")


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
