#!/usr/bin/env bash
# .auto-sync/scripts/preflight-router.sh - Taskpack 2.0 preflight for router-sync.
#
# Discovers highest stable upstream tags for llm-d/llm-d-router (v*),
# vllm-project/router (v*), and sgl-project/sglang (strictly gateway-v*),
# stages clones into $AUTOSYNC_STAGE_DIR, checks commit 516f1cb ancestry for
# automatic retirement of 0001-epd-routing.patch, probes multi-arch image
# manifests via crane, and emits Taskpack 2.0 preflight outputs and staged resources.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
# shellcheck source=./lib-preflight.sh
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/lib-preflight.sh"
# shellcheck source=../checks/lib.sh
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/../checks/lib.sh"

REPO_ROOT="$(autosync::preflight::repo_root)"
STAGE_DIR="$(autosync::preflight::init_stage "router" "${1:-${AUTOSYNC_STAGE_DIR:-}}")"
BASE_SHA="$(git -C "${REPO_ROOT}" rev-parse HEAD)"

autosync::preflight::snapshot_baselines "${STAGE_DIR}" \
  "pack/llm-router" \
  "pkg/worker/settings/value.go" \
  "docs/reference/settings.md"

if [[ -f "${REPO_ROOT}/.auto-sync/setting-image-holds.json" ]]; then
  autosync::preflight::snapshot_baselines "${STAGE_DIR}" ".auto-sync/setting-image-holds.json"
fi

autosync::preflight::warm_caches "${STAGE_DIR}" --sbin

PROBE_LIST_FILE="$(mktemp "${TMPDIR:-/tmp}/autosync-router-probes.XXXXXXXX")"
DISCOVERY_JSON="${STAGE_DIR}/router-discovery.json"
ROUTER_STAGE_TMP=""
PIN_WORK=""
cleanup_router_preflight() {
  rm -f "${PROBE_LIST_FILE}" "${DISCOVERY_JSON}"
  if [[ -n "${ROUTER_STAGE_TMP:-}" && -d "${ROUTER_STAGE_TMP}" ]]; then
    rm -rf "${ROUTER_STAGE_TMP}"
  fi
  if [[ -n "${PIN_WORK:-}" && -d "${PIN_WORK}" ]]; then
    rm -rf "${PIN_WORK}"
  fi
}
trap cleanup_router_preflight EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

python3 -B - "${REPO_ROOT}" "${STAGE_DIR}" "${AUTOSYNC_ROUTER_FIXTURE_DIR:-}" "${DISCOVERY_JSON}" "${PROBE_LIST_FILE}" "${BASE_SHA}" <<'PY'
import json
import os
import re
import shutil
import subprocess
import sys

repo_root, stage_dir, fixture_dir, discovery_path, probe_list_path, base_sha = sys.argv[1:7]
sys.path.insert(0, os.path.join(repo_root, ".auto-sync", "checks"))
from dockerfile_arg import read_dockerfile_arg
from setting_image_holds import (
    domain_holds,
    load_setting_image_holds,
    resolve_holds_file_for_preflight,
    verify_router_holds,
)


def read_text(path: str) -> str:
    with open(path, "r", encoding="utf-8") as f:
        return f.read()


def extract_value_go_image(text: str, var_name: str) -> str:
    m = re.search(
        rf'{re.escape(var_name)}\s*=\s*settings\.NewEditable\([^)]*?setting\.InitializeFromEnv\("([^"]+)"\)',
        text,
        re.DOTALL,
    )
    if not m:
        raise SystemExit(f"FAIL: missing {var_name} default in value.go")
    return m.group(1).strip()


def resolve_repo_url(env_var: str, fixture_name: str, default_url: str) -> tuple[str, bool]:
    override = os.environ.get(env_var, "").strip()
    if override:
        return override, os.path.isdir(override)
    if fixture_dir:
        local_repo = os.path.join(fixture_dir, "repos", fixture_name)
        if os.path.isdir(local_repo):
            return local_repo, True
        raise SystemExit(f"FAIL: AUTOSYNC_ROUTER_FIXTURE_DIR is set but missing fixture repo: {local_repo}")
    return default_url, False


def ls_remote_stable_tags(repo_url: str, ref_glob: str, tag_re: re.Pattern[str], strip_prefix: str) -> tuple[str, list[str]]:
    out = subprocess.check_output(
        ["git", "-c", "protocol.file.allow=always", "ls-remote", "--tags", "--refs", repo_url, ref_glob],
        text=True,
    )
    tags = []
    for raw in out.splitlines():
        raw = raw.strip()
        if not raw:
            continue
        parts = raw.split("\t", 1)
        if len(parts) != 2:
            continue
        ref = parts[1].strip()
        if not ref.startswith("refs/tags/"):
            continue
        tag = ref[len("refs/tags/"):]
        if tag_re.fullmatch(tag):
            tags.append(tag)

    if not tags:
        raise SystemExit(f"FAIL: no stable tags matching {tag_re.pattern} found in {repo_url} ({ref_glob})")

    def ver_key(t: str) -> tuple[int, ...]:
        num_part = t[len(strip_prefix):]
        return tuple(int(seg) for seg in num_part.split("."))

    tags = sorted(set(tags), key=ver_key, reverse=True)
    return tags[0], tags[:5]


def ensure_tag_fetched(dst: str, repo_url: str, tag: str, filter_blobless: bool = False) -> None:
    rc = subprocess.run(
        ["git", "-C", dst, "rev-parse", "--verify", "-q", f"refs/tags/{tag}^{{commit}}"],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    ).returncode
    if rc == 0:
        return
    cmd = ["git", "-C", dst, "-c", "protocol.file.allow=always", "fetch", "--depth", "1"]
    if filter_blobless:
        cmd.append("--filter=blob:none")
    cmd.extend([repo_url, f"refs/tags/{tag}:refs/tags/{tag}"])
    subprocess.run(cmd, check=True)


def sync_full_history_repo(repo_url: str, dst: str, pinned_tag: str, target_tag: str) -> None:
    is_local_repo = os.path.isdir(repo_url) or repo_url.startswith("file://")
    filter_args = [] if is_local_repo else ["--filter=blob:none"]
    if os.path.isdir(os.path.join(dst, ".git")):
        subprocess.run(["git", "-C", dst, "remote", "set-url", "origin", repo_url], check=True)
        subprocess.run(
            ["git", "-C", dst, "-c", "protocol.file.allow=always", "fetch", *filter_args, "--tags", "--force", "--prune", "origin"],
            check=True,
        )
    else:
        if os.path.exists(dst):
            shutil.rmtree(dst)
        subprocess.run(
            ["git", "-c", "protocol.file.allow=always", "clone", *filter_args, "--no-single-branch", repo_url, dst],
            check=True,
        )
    ensure_tag_fetched(dst, repo_url, target_tag, filter_blobless=not is_local_repo)
    if pinned_tag and pinned_tag != target_tag:
        try:
            ensure_tag_fetched(dst, repo_url, pinned_tag, filter_blobless=not is_local_repo)
        except subprocess.CalledProcessError:
            pass
    subprocess.run(["git", "-C", dst, "checkout", "-f", "-q", f"refs/tags/{target_tag}"], check=True)


def sync_tagged_repo(repo_url: str, dst: str, pinned_tag: str, target_tag: str, sparse_subpath: str = "", is_local: bool = False) -> None:
    if is_local:
        sync_full_history_repo(repo_url, dst, pinned_tag, target_tag)
        return

    use_sparse = bool(sparse_subpath)
    if not os.path.isdir(os.path.join(dst, ".git")):
        if os.path.exists(dst):
            shutil.rmtree(dst)
        cmd = ["git", "-c", "protocol.file.allow=always", "clone", "--depth", "1", "--branch", target_tag]
        if use_sparse:
            cmd.extend(["--filter=blob:none", "--sparse"])
        cmd.extend([repo_url, dst])
        subprocess.run(cmd, check=True)
        if use_sparse:
            subprocess.run(["git", "-C", dst, "sparse-checkout", "set", sparse_subpath], check=True)
    else:
        subprocess.run(["git", "-C", dst, "remote", "set-url", "origin", repo_url], check=True)
        if use_sparse:
            subprocess.run(["git", "-C", dst, "sparse-checkout", "set", sparse_subpath], check=True)
        ensure_tag_fetched(dst, repo_url, target_tag, filter_blobless=use_sparse)

    if pinned_tag and pinned_tag != target_tag:
        try:
            ensure_tag_fetched(dst, repo_url, pinned_tag, filter_blobless=use_sparse)
            if use_sparse:
                subprocess.run(["git", "-C", dst, "checkout", "-f", "-q", f"refs/tags/{pinned_tag}"], check=True)
        except subprocess.CalledProcessError:
            pass

    subprocess.run(["git", "-C", dst, "checkout", "-f", "-q", f"refs/tags/{target_tag}"], check=True)
    if use_sparse:
        subprocess.run(
            ["git", "-C", dst, "archive", target_tag, "--", sparse_subpath],
            stdout=subprocess.DEVNULL,
            check=True,
        )


router_df = read_text(os.path.join(repo_root, "pack/llm-router/Dockerfile"))
value_go = read_text(os.path.join(repo_root, "pkg/worker/settings/value.go"))

recipe = {
    "llm-d-router": read_dockerfile_arg(router_df, "LLMD_ROUTER_VERSION", "pack/llm-router/Dockerfile"),
    "vllm-router": read_dockerfile_arg(router_df, "VLLM_ROUTER_VERSION", "pack/llm-router/Dockerfile"),
    "sglang-gateway": read_dockerfile_arg(router_df, "SGLANG_GATEWAY_VERSION", "pack/llm-router/Dockerfile"),
}
defaults = {
    "router_image": extract_value_go_image(value_go, "ModelDeploymentRouterImage"),
    "proxy_image": extract_value_go_image(value_go, "ModelDeploymentRouterProxyImage"),
    "sidecar_image": extract_value_go_image(value_go, "ModelDeploymentRoutingSidecarImage"),
}

holds_path = resolve_holds_file_for_preflight(repo_root, fixture_dir)
holds_label = holds_path or ".auto-sync/setting-image-holds.json"
router_holds = domain_holds(load_setting_image_holds(holds_path), "router")
verify_router_holds(
    router_holds,
    defaults["router_image"],
    defaults["proxy_image"],
    defaults["sidecar_image"],
    holds_label,
)

upstream_override = {}
if fixture_dir and os.path.isfile(os.path.join(fixture_dir, "upstream.json")):
    with open(os.path.join(fixture_dir, "upstream.json"), "r", encoding="utf-8") as f:
        upstream_override = json.load(f)

llmd_url, llmd_local = resolve_repo_url(
    "AUTOSYNC_ROUTER_LLMD_REPO_URL",
    "llm-d-router",
    "https://github.com/llm-d/llm-d-router.git",
)
vllm_url, vllm_local = resolve_repo_url(
    "AUTOSYNC_ROUTER_VLLM_REPO_URL",
    "vllm-router",
    "https://github.com/vllm-project/router.git",
)
sglang_url, sglang_local = resolve_repo_url(
    "AUTOSYNC_ROUTER_SGLANG_REPO_URL",
    "sglang",
    "https://github.com/sgl-project/sglang.git",
)

llmd_tag_re = re.compile(r"^v\d+(?:\.\d+)+$")
vllm_tag_re = re.compile(r"^v\d+(?:\.\d+)+$")
sglang_tag_re = re.compile(r"^gateway-v\d+(?:\.\d+)+$")

llmd_stage = os.path.join(stage_dir, "llm-d-router")
vllm_stage = os.path.join(stage_dir, "vllm-router")
sglang_stage = os.path.join(stage_dir, "sglang")

if "llm-d-router" in upstream_override:
    up_llmd = str(upstream_override["llm-d-router"]).strip()
    up_llmd_recent = [up_llmd]
elif llmd_url:
    up_llmd, up_llmd_recent = ls_remote_stable_tags(llmd_url, "refs/tags/v*", llmd_tag_re, "v")
else:
    up_llmd, up_llmd_recent = recipe["llm-d-router"], [recipe["llm-d-router"]]

if "vllm-router" in upstream_override:
    up_vllm = str(upstream_override["vllm-router"]).strip()
    up_vllm_recent = [up_vllm]
elif vllm_url:
    up_vllm, up_vllm_recent = ls_remote_stable_tags(vllm_url, "refs/tags/v*", vllm_tag_re, "v")
else:
    up_vllm, up_vllm_recent = recipe["vllm-router"], [recipe["vllm-router"]]

if "sglang-gateway" in upstream_override:
    up_sglang = str(upstream_override["sglang-gateway"]).strip()
    up_sglang_recent = [up_sglang]
elif sglang_url:
    up_sglang, up_sglang_recent = ls_remote_stable_tags(
        sglang_url, "refs/tags/gateway-v*", sglang_tag_re, "gateway-v"
    )
else:
    up_sglang, up_sglang_recent = recipe["sglang-gateway"], [recipe["sglang-gateway"]]

if llmd_url:
    sync_tagged_repo(llmd_url, llmd_stage, recipe["llm-d-router"], up_llmd, is_local=llmd_local)
if vllm_url:
    sync_full_history_repo(vllm_url, vllm_stage, recipe["vllm-router"], up_vllm)
if sglang_url:
    sync_tagged_repo(
        sglang_url,
        sglang_stage,
        recipe["sglang-gateway"],
        up_sglang,
        sparse_subpath="sgl-model-gateway",
        is_local=sglang_local,
    )

# Check 516f1cb ancestry in vllm-router for automatic retirement of 0001-epd-routing.patch.
epd_ref = os.environ.get("AUTOSYNC_ROUTER_EPD_COMMIT", "516f1cb").strip() or "516f1cb"
epd_commit = ""
retire_0001 = False
if os.path.isdir(os.path.join(vllm_stage, ".git")):
    rev_res = subprocess.run(
        ["git", "-C", vllm_stage, "rev-parse", "--verify", "-q", f"{epd_ref}^{{commit}}"],
        capture_output=True,
        text=True,
    )
    if rev_res.returncode == 0:
        epd_commit = rev_res.stdout.strip()
        anc_res = subprocess.run(
            ["git", "-C", vllm_stage, "merge-base", "--is-ancestor", epd_commit, f"refs/tags/{up_vllm}"],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        retire_0001 = anc_res.returncode == 0

# Check if vllm-router target tag implements UID-keyed TrackedPod withdrawal (0004 retirement).
retire_0004 = False


def read_git_tag_file(repo_dir: str, tag: str, rel_path: str) -> str:
    if not os.path.isdir(os.path.join(repo_dir, ".git")):
        return ""
    res = subprocess.run(
        ["git", "-C", repo_dir, "show", f"refs/tags/{tag}:{rel_path}"],
        capture_output=True,
        text=True,
    )
    return res.stdout if res.returncode == 0 else ""


sd_txt = read_git_tag_file(vllm_stage, up_vllm, "src/service_discovery.rs")
if sd_txt and re.search(r"\bstruct\s+TrackedPod\b", sd_txt) and re.search(r"\buid\s*:\s*Option<String>", sd_txt):
    retire_0004 = True

# Parse upstream Go / Rust toolchain directives at target tags.
llmd_gomod = read_git_tag_file(llmd_stage, up_llmd, "go.mod")
llmd_go_directive = ""
recommended_go_version_arg = ""
m_go = re.search(r"^\s*go\s+((\d+\.\d+)(?:\.\d+)?)\b", llmd_gomod, re.MULTILINE)
if m_go:
    llmd_go_directive = m_go.group(1)
    recommended_go_version_arg = f"{m_go.group(2)}-bookworm"


def parse_cargo_meta(cargo_txt: str) -> dict[str, str]:
    if not cargo_txt:
        return {}
    out: dict[str, str] = {}
    in_pkg = False
    for raw_ln in cargo_txt.splitlines():
        st = raw_ln.strip()
        if not st or st.startswith("#"):
            continue
        if st.startswith("["):
            in_pkg = st == "[package]"
            continue
        if in_pkg:
            for key in ("edition", "rust-version"):
                m_k = re.match(rf'^{re.escape(key)}\s*=\s*["\']([^"\']+)["\']', st)
                if m_k:
                    out[key.replace("-", "_")] = m_k.group(1).strip()
    return out


vllm_cargo_meta = parse_cargo_meta(read_git_tag_file(vllm_stage, up_vllm, "Cargo.toml"))
sglang_cargo_meta = parse_cargo_meta(read_git_tag_file(sglang_stage, up_sglang, "sgl-model-gateway/Cargo.toml"))

def parse_generic_target_tags(raw_val: str) -> dict[str, str]:
    parsed = {}
    for item in (seg.strip() for seg in raw_val.split(",") if seg.strip()):
        if ":" in item:
            repo_part, tag_part = item.rsplit(":", 1)
            repo_name = repo_part.strip().split("/")[-1]
            tag_part = tag_part.strip()
            if not tag_part:
                continue
            if repo_name in ("llm-router", "router"):
                parsed["router"] = tag_part
            elif repo_name in ("mirrored-envoy", "envoy", "proxy"):
                parsed["proxy"] = tag_part
            elif repo_name in ("mirrored-llm-d-router-disagg-sidecar", "sidecar"):
                parsed["sidecar"] = tag_part
        else:
            parsed["router"] = item
    return parsed


def list_repo_tags(repo: str) -> list[str]:
    repo_tags = upstream_override.get("repo_tags")
    if isinstance(repo_tags, dict):
        for key in (repo, f"docker.io/{repo}"):
            if isinstance(repo_tags.get(key), list):
                return [str(t).strip() for t in repo_tags[key] if str(t).strip()]
    if fixture_dir:
        return []
    crane_bin = os.environ.get("AUTOSYNC_CRANE_BIN", "crane").strip() or "crane"
    try:
        res = subprocess.run(
            [crane_bin, "ls", f"docker.io/{repo}"],
            capture_output=True,
            text=True,
            timeout=30,
        )
        if res.returncode != 0:
            return []
        return [ln.strip() for ln in res.stdout.splitlines() if ln.strip()]
    except (OSError, subprocess.SubprocessError):
        return []


def discover_newer_tag(repo: str, current_image: str) -> str:
    last_seg = current_image.split("/")[-1]
    if ":" not in last_seg:
        return ""
    current_tag = current_image.rsplit(":", 1)[1].strip()
    m_cur = re.fullmatch(r"(.*?)v(\d+)\.(\d+)\.(\d+)", current_tag)
    if not m_cur:
        return ""
    prefix = m_cur.group(1)
    cur_tuple = (int(m_cur.group(2)), int(m_cur.group(3)), int(m_cur.group(4)))
    tag_pat = re.compile(rf"^{re.escape(prefix)}v(\d+)\.(\d+)\.(\d+)$")
    best_tuple = cur_tuple
    best_tag = ""
    for tag in list_repo_tags(repo):
        m = tag_pat.fullmatch(tag)
        if not m:
            continue
        cand_tuple = (int(m.group(1)), int(m.group(2)), int(m.group(3)))
        if cand_tuple > best_tuple:
            best_tuple = cand_tuple
            best_tag = tag
    return best_tag


generic_targets = parse_generic_target_tags(os.environ.get("AUTOSYNC_TARGET_TAG", ""))

env_router_tag = os.environ.get("AUTOSYNC_ROUTER_TARGET_TAG", "").strip()
if "router" in router_holds:
    if env_router_tag or generic_targets.get("router"):
        req_router = env_router_tag or generic_targets.get("router", "")
        raise SystemExit(
            f"FAIL: target tag targets router ({req_router!r}), "
            f"which is held at {router_holds['router']['tag']!r} by {holds_label}; update {holds_label} first"
        )
    target_router_image = defaults["router_image"]
else:
    target_router_tag = env_router_tag
    if not target_router_tag:
        target_router_tag = generic_targets.get("router", "")
    if not target_router_tag and isinstance(upstream_override.get("router_target_tag"), str):
        target_router_tag = upstream_override["router_target_tag"].strip()
    if not target_router_tag:
        target_router_tag = discover_newer_tag("gpustack/llm-router", defaults["router_image"])
    target_router_image = (
        f"gpustack/llm-router:{target_router_tag}"
        if target_router_tag
        else defaults["router_image"]
    )

env_proxy_tag = os.environ.get("AUTOSYNC_ROUTER_PROXY_TARGET_TAG", "").strip()
if "proxy" in router_holds:
    if env_proxy_tag or generic_targets.get("proxy"):
        req_proxy = env_proxy_tag or generic_targets.get("proxy", "")
        raise SystemExit(
            f"FAIL: target tag targets proxy ({req_proxy!r}), "
            f"which is held at {router_holds['proxy']['tag']!r} by {holds_label}; update {holds_label} first"
        )
    target_proxy_image = defaults["proxy_image"]
else:
    target_proxy_tag = env_proxy_tag
    if not target_proxy_tag:
        target_proxy_tag = generic_targets.get("proxy", "")
    if not target_proxy_tag and isinstance(upstream_override.get("proxy_target_tag"), str):
        target_proxy_tag = upstream_override["proxy_target_tag"].strip()
    if not target_proxy_tag:
        target_proxy_tag = discover_newer_tag("gpustack/mirrored-envoy", defaults["proxy_image"])
    target_proxy_image = (
        f"gpustack/mirrored-envoy:{target_proxy_tag}"
        if target_proxy_tag
        else defaults["proxy_image"]
    )

env_sidecar_tag = os.environ.get("AUTOSYNC_ROUTER_SIDECAR_TARGET_TAG", "").strip()
if "sidecar" in router_holds:
    if env_sidecar_tag or generic_targets.get("sidecar"):
        req_sidecar = env_sidecar_tag or generic_targets.get("sidecar", "")
        raise SystemExit(
            f"FAIL: target tag targets sidecar ({req_sidecar!r}), "
            f"which is held at {router_holds['sidecar']['tag']!r} by {holds_label}; update {holds_label} first"
        )
    target_sidecar_image = defaults["sidecar_image"]
else:
    target_sidecar_tag = env_sidecar_tag
    if not target_sidecar_tag:
        target_sidecar_tag = generic_targets.get("sidecar", "")
    if not target_sidecar_tag and isinstance(upstream_override.get("sidecar_target_tag"), str):
        target_sidecar_tag = upstream_override["sidecar_target_tag"].strip()
    if not target_sidecar_tag:
        target_sidecar_tag = recipe["llm-d-router"]
    target_sidecar_image = f"gpustack/mirrored-llm-d-router-disagg-sidecar:{target_sidecar_tag}"

probe_refs = []
for ref in (
    defaults["router_image"],
    target_router_image,
    defaults["proxy_image"],
    target_proxy_image,
    defaults["sidecar_image"],
    target_sidecar_image,
    f"gpustack/mirrored-llm-d-router-disagg-sidecar:{up_llmd}",
):
    if ref and ref not in probe_refs:
        probe_refs.append(ref)

extra_targets = upstream_override.get("target_images")
if isinstance(extra_targets, list):
    for ref in extra_targets:
        if isinstance(ref, str) and ref and ref not in probe_refs:
            probe_refs.append(ref)

with open(probe_list_path, "w", encoding="utf-8") as f:
    for ref in probe_refs:
        f.write(ref + "\n")

discovery = {
    "base_sha": base_sha,
    "recipe": recipe,
    "defaults": defaults,
    "target_tags": {
        "llm-d-router": up_llmd,
        "vllm-router": up_vllm,
        "sglang-gateway": up_sglang,
    },
    "recent_tags": {
        "llm-d-router": up_llmd_recent,
        "vllm-router": up_vllm_recent,
        "sglang-gateway": up_sglang_recent,
    },
    "held_setting_images": router_holds,
    "targets": {
        "router_image": target_router_image,
        "proxy_image": target_proxy_image,
        "sidecar_image": target_sidecar_image,
    },
    "epd_commit": epd_commit or epd_ref,
    "retire_0001_epd_routing": retire_0001,
    "retire_0004_discovery_readiness": retire_0004,
    "toolchain": {
        "llm_d_go_directive": llmd_go_directive,
        "recommended_go_version_arg": recommended_go_version_arg,
        "vllm_router_cargo": vllm_cargo_meta,
        "sglang_gateway_cargo": sglang_cargo_meta,
    },
}

with open(discovery_path, "w", encoding="utf-8") as f:
    json.dump(discovery, f, indent=2)
    f.write("\n")
PY

PROBE_REFS=()
while IFS= read -r ref; do
  [[ -n "${ref}" ]] && PROBE_REFS+=("${ref}")
done < "${PROBE_LIST_FILE}"

MANIFESTS_JSON="${STAGE_DIR}/image-manifests.json"
if [[ -n "${AUTOSYNC_ROUTER_FIXTURE_DIR:-}" && -f "${AUTOSYNC_ROUTER_FIXTURE_DIR}/image-manifests.json" ]]; then
  AUTOSYNC_MOCK_MANIFESTS_JSON="${AUTOSYNC_ROUTER_FIXTURE_DIR}/image-manifests.json" \
    autosync::preflight::record_image_probes "${MANIFESTS_JSON}" "${PROBE_REFS[@]}"
else
  autosync::preflight::record_image_probes "${MANIFESTS_JSON}" "${PROBE_REFS[@]}"
fi

UPSTREAM_RECORDS_JSON="${STAGE_DIR}/upstream-records.json"
PREFLIGHT_REPORT_JSON="${STAGE_DIR}/router-preflight-report.json"

python3 - "${DISCOVERY_JSON}" "${MANIFESTS_JSON}" "${UPSTREAM_RECORDS_JSON}" "${PREFLIGHT_REPORT_JSON}" <<'PY'
import json
import sys

disc_path, man_path, recs_path, rep_path = sys.argv[1:5]
with open(disc_path, "r", encoding="utf-8") as f:
    disc = json.load(f)
with open(man_path, "r", encoding="utf-8") as f:
    man = json.load(f)

images = man.get("images", {})


def is_published(ref: str) -> bool:
    entry = images.get(ref)
    if not isinstance(entry, dict):
        entry = images.get(f"docker.io/{ref}")
    return isinstance(entry, dict) and entry.get("published") is True


recipe = disc["recipe"]
defaults = disc["defaults"]
target_tags = disc["target_tags"]
recent_tags = disc["recent_tags"]
targets = disc["targets"]

stage_a_drifts = {
    uid: target_tags[uid] != recipe[uid]
    for uid in ("llm-d-router", "vllm-router", "sglang-gateway")
}
has_stage_a = any(stage_a_drifts.values())

router_stage_b = (
    targets["router_image"] != defaults["router_image"]
    and is_published(targets["router_image"])
)
proxy_stage_b = (
    targets["proxy_image"] != defaults["proxy_image"]
    and is_published(targets["proxy_image"])
)
sidecar_stage_b = (
    targets["sidecar_image"] != defaults["sidecar_image"]
    and is_published(targets["sidecar_image"])
)
has_stage_b = router_stage_b or proxy_stage_b or sidecar_stage_b

if has_stage_b:
    stage = "tag_promotion"
    image_published = True
    records = []
    for uid in ("llm-d-router", "vllm-router", "sglang-gateway"):
        promote = router_stage_b or (uid == "llm-d-router" and (sidecar_stage_b or proxy_stage_b))
        records.append({
            "id": uid,
            "status": "available" if promote else "up_to_date",
            "version": f"{recipe[uid]}+stage-b" if promote else recipe[uid],
            **({"currentVersion": recipe[uid]} if promote else {}),
            "recentVersions": recent_tags[uid],
        })
elif has_stage_a:
    stage = "recipe_sync"
    image_published = False
    records = [
        {
            "id": uid,
            "status": "available" if stage_a_drifts[uid] else "up_to_date",
            "version": target_tags[uid] if stage_a_drifts[uid] else recipe[uid],
            **({"currentVersion": recipe[uid]} if stage_a_drifts[uid] else {}),
            "recentVersions": recent_tags[uid],
        }
        for uid in ("llm-d-router", "vllm-router", "sglang-gateway")
    ]
else:
    stage = "no_change"
    image_published = (
        targets["router_image"] == defaults["router_image"]
        and targets["proxy_image"] == defaults["proxy_image"]
        and targets["sidecar_image"] == defaults["sidecar_image"]
        and is_published(defaults["router_image"])
        and is_published(defaults["proxy_image"])
        and is_published(defaults["sidecar_image"])
    )
    records = [
        {
            "id": uid,
            "status": "up_to_date",
            "version": recipe[uid],
            "recentVersions": recent_tags[uid],
        }
        for uid in ("llm-d-router", "vllm-router", "sglang-gateway")
    ]

with open(recs_path, "w", encoding="utf-8") as f:
    json.dump(records, f, indent=2)
    f.write("\n")

report = {
    "schemaVersion": 1,
    "base_sha": disc.get("base_sha", ""),
    "stage": stage,
    "image_published": image_published,
    "retire_0001_epd_routing": disc["retire_0001_epd_routing"],
    "retire_0004_discovery_readiness": disc.get("retire_0004_discovery_readiness", False),
    "epd_commit": disc["epd_commit"],
    "toolchain": disc.get("toolchain") or {},
    "has_stage_a": has_stage_a,
    "stage_a_drifts": stage_a_drifts,
    "stage_b_flags": {
        "router": router_stage_b,
        "proxy": proxy_stage_b,
        "sidecar": sidecar_stage_b,
    },
    "held_setting_images": disc.get("held_setting_images") or {},
    "recipe": recipe,
    "target_tags": target_tags,
    "recent_tags": recent_tags,
    "defaults": defaults,
    "targets": targets,
    "rebase": {},
}

with open(rep_path, "w", encoding="utf-8") as f:
    json.dump(report, f, indent=2)
    f.write("\n")
PY

autosync::preflight::apply_force_to_upstream_records "${UPSTREAM_RECORDS_JSON}"

if autosync::preflight::has_available_upstreams "${UPSTREAM_RECORDS_JSON}" || \
   [[ "${AUTOSYNC_FORCE:-false}" == "true" || \
      "${COMMENT_COMMAND:-}" == "revise" || \
      "${COMMENT_COMMAND:-}" == "retry" || \
      "${AUTOSYNC_WARM_TOOLS:-false}" == "true" ]]; then
  if [[ -n "${AUTOSYNC_ROUTER_FIXTURE_DIR:-}" && "${AUTOSYNC_WARM_TOOLS:-false}" != "true" ]]; then
    autosync::preflight::warm_caches "${STAGE_DIR}" --sbin --go
  else
    autosync::preflight::warm_caches "${STAGE_DIR}" --sbin --go --tools shellcheck,hugo
  fi
fi

STAGED_TREE=""
COMMIT_PATHS=()
if autosync::preflight::has_available_upstreams "${UPSTREAM_RECORDS_JSON}"; then
  ROUTER_STAGE_TMP="$(mktemp -d "${TMPDIR:-/tmp}/autosync-router-stage.XXXXXXXX")"
  STAGED_TREE="${ROUTER_STAGE_TMP}/staged-tree"
  BUNDLE_WORK="${ROUTER_STAGE_TMP}/bundle-work"
  mkdir -p "${STAGED_TREE}" "${BUNDLE_WORK}"

  COMMIT_PATHS=(".auto-sync/.stage/router")

  ROUTER_STAGE_NAME="$(python3 - "${PREFLIGHT_REPORT_JSON}" <<'PY'
import json
import sys
with open(sys.argv[1], "r", encoding="utf-8") as f:
    print(json.load(f).get("stage", ""))
PY
)"

  if [[ "${ROUTER_STAGE_NAME}" == "recipe_sync" ]]; then
    STAGED_PATCH_DIRS_FILE="${ROUTER_STAGE_TMP}/staged-patch-dirs.txt"
    python3 - "${REPO_ROOT}" "${STAGE_DIR}" "${PREFLIGHT_REPORT_JSON}" "${STAGED_TREE}" "${BUNDLE_WORK}" "${ROUTER_STAGE_TMP}" <<'PY' > "${STAGED_PATCH_DIRS_FILE}"
import json
import os
import re
import shutil
import subprocess
import sys

repo_root, stage_dir, rep_path, staged_tree, bundle_work, tmp_root = sys.argv[1:7]

with open(rep_path, "r", encoding="utf-8") as f:
    rep = json.load(f)

recipe = rep["recipe"]
target_tags = rep["target_tags"]
retire_0001 = bool(rep.get("retire_0001_epd_routing"))
retire_0004 = bool(rep.get("retire_0004_discovery_readiness"))

ROUTER_SPECS = [
    ("llm-d-router", os.path.join(stage_dir, "llm-d-router"), "llm-d", ""),
    ("vllm-router", os.path.join(stage_dir, "vllm-router"), "vllm-router", ""),
    ("sglang-gateway", os.path.join(stage_dir, "sglang"), "sglang-gateway", "sgl-model-gateway"),
]


def archive_tag(stage_repo: str, tag: str, dst_dir: str, sparse_subpath: str) -> None:
    if os.path.exists(dst_dir):
        shutil.rmtree(dst_dir)
    os.makedirs(dst_dir, exist_ok=True)
    promisor = (
        subprocess.run(
            ["git", "-C", stage_repo, "config", "--bool", "remote.origin.promisor"],
            capture_output=True,
            text=True,
        ).stdout.strip()
        == "true"
    )
    cmd = ["git", "-C", stage_repo, "archive", f"refs/tags/{tag}"]
    if promisor and sparse_subpath:
        cmd.extend(["--", sparse_subpath])
    arc = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if arc.returncode != 0:
        raise RuntimeError(f"git archive {tag} failed: {arc.stderr.decode('utf-8', 'replace')}")
    tar_res = subprocess.run(["tar", "-x", "-C", dst_dir], input=arc.stdout, stderr=subprocess.PIPE)
    if tar_res.returncode != 0:
        raise RuntimeError(f"tar extract {tag} failed: {tar_res.stderr.decode('utf-8', 'replace')}")


def has_tag(stage_repo: str, tag: str) -> bool:
    if not os.path.isdir(os.path.join(stage_repo, ".git")):
        return False
    return (
        subprocess.run(
            ["git", "-C", stage_repo, "rev-parse", "--verify", "-q", f"refs/tags/{tag}^{{commit}}"],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        ).returncode
        == 0
    )


def split_patch_header_and_files(patch_text: str) -> tuple[str, list[str]]:
    lines = patch_text.splitlines(keepends=True)
    header_end = 0
    for idx, ln in enumerate(lines):
        if ln.startswith("diff --git "):
            header_end = idx
            break
    else:
        header_end = len(lines)
    header = "".join(lines[:header_end])
    touched = []
    for ln in lines[header_end:]:
        m = re.match(r"^diff --git a/(.+?) b/(.+?)$", ln.rstrip("\r\n"))
        if m:
            for p in (m.group(1), m.group(2)):
                if p != "/dev/null" and p not in touched:
                    touched.append(p)
    return header, touched


def copy_rel_file(src_root: str, dst_root: str, rel: str) -> bool:
    src = os.path.join(src_root, *rel.split("/"))
    if not os.path.isfile(src):
        return False
    dst = os.path.join(dst_root, *rel.split("/"))
    os.makedirs(os.path.dirname(dst), exist_ok=True)
    shutil.copyfile(src, dst)
    return True


def write_rebased_patch_file(r_repo: str, p: dict, new_sha: str, out_dir: str, old_tag: str, new_tag: str, env: dict) -> str:
    diff_bytes = subprocess.check_output(
        ["git", "-C", r_repo, "diff", "--full-index", f"{new_sha}^", new_sha],
        env=env,
    )
    hdr = p["header"]
    if old_tag in hdr:
        hdr = re.sub(
            rf"(AGAINST:.*?){re.escape(old_tag)}",
            rf"\g<1>{new_tag}",
            hdr,
            count=1,
            flags=re.DOTALL,
        )
    out_pf = os.path.join(out_dir, p["name"])
    with open(out_pf, "wb") as f:
        f.write(hdr.encode("utf-8"))
        f.write(diff_bytes)
    return out_pf


def collect_target_extras(
    uid: str, kept: list[dict], conflicted_files: list[str], r_repo: str, env: dict
) -> list[str]:
    all_touched = []
    for p in kept:
        for rel in p["touched"]:
            if rel not in all_touched:
                all_touched.append(rel)
    for cf in conflicted_files:
        if cf not in all_touched:
            all_touched.append(cf)
    porcelain_res = subprocess.run(
        ["git", "-C", r_repo, "status", "--porcelain"],
        capture_output=True,
        text=True,
        env=env,
    )
    for ln in porcelain_res.stdout.splitlines():
        if len(ln) > 3:
            rp = ln[3:].split(" -> ")[-1].strip()
            if rp and rp not in all_touched:
                all_touched.append(rp)

    target_extras = list(all_touched)
    if uid == "llm-d-router":
        for extra in ("go.mod", "pkg/epp/config/loader/defaults.go"):
            if extra not in target_extras:
                target_extras.append(extra)
    elif uid == "vllm-router":
        if "Cargo.toml" not in target_extras:
            target_extras.append("Cargo.toml")
    elif uid == "sglang-gateway":
        if "sgl-model-gateway/Cargo.toml" not in target_extras:
            target_extras.append("sgl-model-gateway/Cargo.toml")
    return target_extras


git_env = {
    **os.environ,
    "LC_ALL": "C",
    "GIT_AUTHOR_NAME": "autosync-preflight",
    "GIT_AUTHOR_EMAIL": "autosync@gpustack.local",
    "GIT_COMMITTER_NAME": "autosync-preflight",
    "GIT_COMMITTER_EMAIL": "autosync@gpustack.local",
}

rebase_results = {}
staged_patch_dirs = []

for uid, stage_repo, subdir, sparse_subpath in ROUTER_SPECS:
    old_tag = recipe[uid]
    new_tag = target_tags[uid]
    if old_tag == new_tag:
        continue
    if not (has_tag(stage_repo, old_tag) and has_tag(stage_repo, new_tag)):
        rebase_results[uid] = {
            "auto_rebased": False,
            "reason": "missing recipe or target tag in staged clone",
            "retired_patches": [],
            "conflicted_files": [],
        }
        continue

    patch_dir = os.path.join(repo_root, "pack/llm-router/patches", subdir)
    patch_names = sorted(p for p in os.listdir(patch_dir) if p.endswith(".patch")) if os.path.isdir(patch_dir) else []

    retired_names = []
    if uid == "vllm-router":
        if retire_0001 and "0001-epd-routing.patch" in patch_names:
            retired_names.append("0001-epd-routing.patch")
        if retire_0004 and "0004-discovery-readiness-and-deselect-removal.patch" in patch_names:
            retired_names.append("0004-discovery-readiness-and-deselect-removal.patch")
    retired_set = set(retired_names)

    r_work = os.path.join(tmp_root, f"rebase-{subdir}")
    r_repo = os.path.join(r_work, "repo")
    r_target = os.path.join(r_work, "target")
    os.makedirs(r_work, exist_ok=True)

    try:
        archive_tag(stage_repo, old_tag, r_repo, sparse_subpath)
        archive_tag(stage_repo, new_tag, r_target, sparse_subpath)
    except RuntimeError as exc:
        rebase_results[uid] = {
            "auto_rebased": False,
            "reason": str(exc),
            "retired_patches": retired_names,
            "conflicted_files": [],
        }
        continue

    subprocess.run(["git", "-C", r_repo, "init", "-q", "-b", "base"], check=True, env=git_env)
    subprocess.run(["git", "-C", r_repo, "config", "commit.gpgsign", "false"], check=True, env=git_env)
    subprocess.run(["git", "-C", r_repo, "add", "-A"], check=True, env=git_env)
    subprocess.run(["git", "-C", r_repo, "commit", "-q", "-m", f"base {old_tag}"], check=True, env=git_env)

    applied = []
    base_apply_err = ""
    for pname in patch_names:
        pf = os.path.join(patch_dir, pname)
        with open(pf, "r", encoding="utf-8") as f:
            ptxt = f.read()
        header_txt, touched = split_patch_header_and_files(ptxt)
        ap = subprocess.run(["git", "-C", r_repo, "apply", "--index", pf], capture_output=True, text=True, env=git_env)
        if ap.returncode != 0:
            base_apply_err = f"baseline patch {pname} failed to apply onto {old_tag}: {ap.stderr.strip()}"
            break
        subprocess.run(["git", "-C", r_repo, "commit", "-q", "-m", pname], check=True, env=git_env)
        sha = subprocess.check_output(["git", "-C", r_repo, "rev-parse", "HEAD"], text=True, env=git_env).strip()
        applied.append({
            "name": pname,
            "sha": sha,
            "header": header_txt,
            "touched": touched,
        })

    if base_apply_err:
        rebase_results[uid] = {
            "auto_rebased": False,
            "reason": base_apply_err,
            "retired_patches": retired_names,
            "conflicted_files": [],
        }
        continue

    # Switch to orphan target branch holding exact new_tag tree.
    subprocess.run(["git", "-C", r_repo, "checkout", "--orphan", "target"], check=True, env=git_env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    subprocess.run(["git", "-C", r_repo, "rm", "-rfq", "--", "."], check=True, env=git_env)
    for item in os.listdir(r_repo):
        if item == ".git":
            continue
        p = os.path.join(r_repo, item)
        if os.path.isdir(p) and not os.path.islink(p):
            shutil.rmtree(p)
        else:
            os.unlink(p)
    for item in os.listdir(r_target):
        src_p = os.path.join(r_target, item)
        dst_p = os.path.join(r_repo, item)
        if os.path.isdir(src_p) and not os.path.islink(src_p):
            shutil.copytree(src_p, dst_p)
        else:
            shutil.copy2(src_p, dst_p)
    subprocess.run(["git", "-C", r_repo, "add", "-A"], check=True, env=git_env)
    subprocess.run(["git", "-C", r_repo, "commit", "-q", "-m", f"target {new_tag}"], check=True, env=git_env)

    kept = [p for p in applied if p["name"] not in retired_set]
    rebased_commits = []
    conflict_patch = ""
    conflicted_files: list[str] = []
    conflict_detail = ""

    for p in kept:
        cp = subprocess.run(["git", "-C", r_repo, "cherry-pick", p["sha"]], capture_output=True, text=True, env=git_env)
        if cp.returncode == 0:
            new_sha = subprocess.check_output(["git", "-C", r_repo, "rev-parse", "HEAD"], text=True, env=git_env).strip()
            rebased_commits.append((p, new_sha))
        else:
            conflict_patch = p["name"]
            u_res = subprocess.run(
                ["git", "-C", r_repo, "diff", "--name-only", "--diff-filter=U"],
                capture_output=True,
                text=True,
                env=git_env,
            )
            conflicted_files = [ln.strip() for ln in u_res.stdout.splitlines() if ln.strip()]
            st_short = subprocess.run(
                ["git", "-C", r_repo, "status", "--short"],
                capture_output=True,
                text=True,
                env=git_env,
            ).stdout
            conflict_detail = (
                f"patch: {conflict_patch}\n"
                f"target: {new_tag}\n"
                f"conflicted_files:\n"
                + "".join(f"  - {cf}\n" for cf in conflicted_files)
                + f"\ngit status --short:\n{st_short}\n"
                + f"cherry-pick stderr:\n{cp.stderr.strip()}\n"
            )
            break

    if not conflict_patch:
        out_patch_dir = os.path.join(staged_tree, "pack/llm-router/patches", subdir)
        os.makedirs(out_patch_dir, exist_ok=True)
        exported_paths = [
            write_rebased_patch_file(r_repo, p, new_sha, out_patch_dir, old_tag, new_tag, git_env)
            for p, new_sha in rebased_commits
        ]

        # Verify git apply --check in lexical order on fresh target tree.
        chk_tree = os.path.join(r_work, "verify")
        shutil.copytree(r_target, chk_tree)
        subprocess.run(["git", "-C", chk_tree, "init", "-q"], check=True, env=git_env)
        verify_ok = True
        failed_patch_name = ""
        verify_err = ""
        for out_pf in sorted(exported_paths):
            chk = subprocess.run(["git", "-C", chk_tree, "apply", "--check", out_pf], capture_output=True, text=True, env=git_env)
            if chk.returncode != 0:
                verify_ok = False
                failed_patch_name = os.path.basename(out_pf)
                verify_err = chk.stderr.strip()
                break
            subprocess.run(["git", "-C", chk_tree, "apply", out_pf], check=True, env=git_env)

        if verify_ok:
            rebase_results[uid] = {
                "auto_rebased": True,
                "retired_patches": retired_names,
                "kept_patches": [p["name"] for p, _ in rebased_commits],
                "conflicted_files": [],
            }
            staged_patch_dirs.append(f"pack/llm-router/patches/{subdir}")
            continue

        shutil.rmtree(out_patch_dir, ignore_errors=True)
        tgt_out = os.path.join(bundle_work, "target-files", subdir)
        os.makedirs(tgt_out, exist_ok=True)
        for rel in collect_target_extras(uid, kept, [], r_repo, git_env):
            copy_rel_file(r_target, tgt_out, rel)
        rebase_results[uid] = {
            "auto_rebased": False,
            "verify_failed": True,
            "reason": f"rebased patch {failed_patch_name} failed git apply --check on {new_tag}: {verify_err}",
            "conflict_patch": "",
            "conflicted_patch": "",
            "rebased_before_conflict": [],
            "retired_patches": retired_names,
            "conflicted_files": [],
        }
        continue

    # Conflict: stage target-files, cherry-pick-working, conflicts, and any already-rebased patches.
    rebased_before_conflict = [p["name"] for p, _ in rebased_commits]
    if rebased_commits:
        pre_conf_patch_dir = os.path.join(bundle_work, "rebased-patches", subdir)
        os.makedirs(pre_conf_patch_dir, exist_ok=True)
        for p, new_sha in rebased_commits:
            write_rebased_patch_file(r_repo, p, new_sha, pre_conf_patch_dir, old_tag, new_tag, git_env)

    r_pre_conflict = os.path.join(r_work, "pre-conflict")
    os.makedirs(r_pre_conflict, exist_ok=True)
    arch = subprocess.Popen(
        ["git", "-C", r_repo, "archive", "HEAD"],
        stdout=subprocess.PIPE,
        env=git_env,
    )
    assert arch.stdout is not None
    subprocess.run(["tar", "-x", "-C", r_pre_conflict], stdin=arch.stdout, check=True)
    arch.stdout.close()
    if arch.wait() != 0:
        raise RuntimeError(f"git archive HEAD failed for {subdir}")
    base_src_tree = r_pre_conflict

    target_extras = collect_target_extras(uid, kept, conflicted_files, r_repo, git_env)

    tgt_out = os.path.join(bundle_work, "target-files", subdir)
    cp_out = os.path.join(bundle_work, "cherry-pick-working", subdir)
    conf_out = os.path.join(bundle_work, "conflicts", subdir)
    os.makedirs(tgt_out, exist_ok=True)
    os.makedirs(cp_out, exist_ok=True)
    os.makedirs(conf_out, exist_ok=True)

    for rel in target_extras:
        copy_rel_file(base_src_tree, tgt_out, rel)
        copy_rel_file(r_repo, cp_out, rel)

    with open(os.path.join(conf_out, f"{conflict_patch}.txt"), "w", encoding="utf-8") as f:
        f.write(conflict_detail)

    rebase_results[uid] = {
        "auto_rebased": False,
        "reason": f"cherry-pick conflict in {conflict_patch} onto {new_tag}",
        "conflict_patch": conflict_patch,
        "conflicted_patch": conflict_patch,
        "rebased_before_conflict": rebased_before_conflict,
        "retired_patches": retired_names,
        "conflicted_files": conflicted_files,
    }

rep["rebase"] = rebase_results
with open(rep_path, "w", encoding="utf-8") as f:
    json.dump(rep, f, indent=2)
    f.write("\n")

for pdir in staged_patch_dirs:
    print(pdir)
PY
    while IFS= read -r rel_p; do
      [[ -n "${rel_p}" ]] && COMMIT_PATHS+=("${rel_p}")
    done < "${STAGED_PATCH_DIRS_FILE}"
  elif [[ "${ROUTER_STAGE_NAME}" == "tag_promotion" ]]; then
    mkdir -p \
      "${STAGED_TREE}/pkg/worker/settings" \
      "${STAGED_TREE}/pkg/worker/controllers/worker" \
      "${STAGED_TREE}/docs/reference"
    cp -f "${REPO_ROOT}/pkg/worker/settings/value.go" "${STAGED_TREE}/pkg/worker/settings/value.go"
    if [[ -f "${REPO_ROOT}/pkg/worker/settings/value_test.go" ]]; then
      cp -f "${REPO_ROOT}/pkg/worker/settings/value_test.go" "${STAGED_TREE}/pkg/worker/settings/value_test.go"
    fi
    if [[ -f "${REPO_ROOT}/pkg/worker/controllers/worker/model_deployment_router_test.go" ]]; then
      cp -f "${REPO_ROOT}/pkg/worker/controllers/worker/model_deployment_router_test.go" \
        "${STAGED_TREE}/pkg/worker/controllers/worker/model_deployment_router_test.go"
    fi
    if [[ -f "${REPO_ROOT}/pkg/worker/controllers/worker/model_deployment_render_test.go" ]]; then
      cp -f "${REPO_ROOT}/pkg/worker/controllers/worker/model_deployment_render_test.go" \
        "${STAGED_TREE}/pkg/worker/controllers/worker/model_deployment_render_test.go"
    fi
    cp -f "${REPO_ROOT}/docs/reference/settings.md" "${STAGED_TREE}/docs/reference/settings.md"
    python3 - "${PREFLIGHT_REPORT_JSON}" \
      "${STAGED_TREE}/pkg/worker/settings/value.go" \
      "${STAGED_TREE}/docs/reference/settings.md" \
      "${STAGED_TREE}/pkg/worker/settings/value_test.go" \
      "${STAGED_TREE}/pkg/worker/controllers/worker/model_deployment_router_test.go" \
      "${STAGED_TREE}/pkg/worker/controllers/worker/model_deployment_render_test.go" <<'PY'
import json
import os
import re
import sys

rep_p, vg_p, sm_p, vt_p, rt_p, rn_p = sys.argv[1:7]
with open(rep_p, "r", encoding="utf-8") as f:
    rep = json.load(f)

sb = rep.get("stage_b_flags") or {}
defaults = rep.get("defaults") or {}
targets = rep.get("targets") or {}


def update_value_go(text: str, var_name: str, new_img: str) -> str:
    pat = re.compile(
        rf'({re.escape(var_name)}\s*=\s*settings\.NewEditable\([^)]*?setting\.InitializeFromEnv\(")([^"]+)("\))',
        re.DOTALL,
    )
    out, n = pat.subn(rf'\g<1>{new_img}\g<3>', text, count=1)
    if n != 1:
        raise SystemExit(f"FAIL: could not update {var_name} in value.go")
    return out


def update_settings_md(text: str, key: str, new_img: str) -> str:
    pat = re.compile(
        rf'^(\| `{re.escape(key)}`<br/>.*?<br/>Default: `)([^`]+)(`)',
        re.MULTILINE,
    )
    out, n = pat.subn(rf'\g<1>{new_img}\g<3>', text, count=1)
    if n != 1:
        raise SystemExit(f"FAIL: could not update {key} in settings.md")
    return out


def replace_exact_image_literals(path: str, pairs: list[tuple[str, str]]) -> None:
    if not os.path.isfile(path) or not pairs:
        return
    with open(path, "r", encoding="utf-8") as f:
        txt = f.read()
    changed = False
    for old_img, new_img in pairs:
        if old_img and new_img and old_img != new_img and f'"{old_img}"' in txt:
            txt = txt.replace(f'"{old_img}"', f'"{new_img}"')
            changed = True
    if changed:
        with open(path, "w", encoding="utf-8") as f:
            f.write(txt)


with open(vg_p, "r", encoding="utf-8") as f:
    vg_txt = f.read()
with open(sm_p, "r", encoding="utf-8") as f:
    sm_txt = f.read()

router_pairs = []
sidecar_pairs = []
if sb.get("router"):
    vg_txt = update_value_go(vg_txt, "ModelDeploymentRouterImage", targets["router_image"])
    sm_txt = update_settings_md(sm_txt, "model-deployment-router-image", targets["router_image"])
    router_pairs.append((defaults.get("router_image", ""), targets["router_image"]))
if sb.get("proxy"):
    vg_txt = update_value_go(vg_txt, "ModelDeploymentRouterProxyImage", targets["proxy_image"])
    sm_txt = update_settings_md(sm_txt, "model-deployment-router-proxy-image", targets["proxy_image"])
    router_pairs.append((defaults.get("proxy_image", ""), targets["proxy_image"]))
if sb.get("sidecar"):
    vg_txt = update_value_go(vg_txt, "ModelDeploymentRoutingSidecarImage", targets["sidecar_image"])
    sm_txt = update_settings_md(sm_txt, "model-deployment-routing-sidecar-image", targets["sidecar_image"])
    sidecar_pairs.append((defaults.get("sidecar_image", ""), targets["sidecar_image"]))

with open(vg_p, "w", encoding="utf-8") as f:
    f.write(vg_txt)
with open(sm_p, "w", encoding="utf-8") as f:
    f.write(sm_txt)

replace_exact_image_literals(vt_p, router_pairs + sidecar_pairs)
replace_exact_image_literals(rt_p, router_pairs)
replace_exact_image_literals(rn_p, sidecar_pairs)
PY

    ROUTER_PIN_REBASE="$(python3 - "${PREFLIGHT_REPORT_JSON}" <<'PY'
import json
import sys
with open(sys.argv[1], "r", encoding="utf-8") as f:
    sb = (json.load(f).get("stage_b_flags") or {})
print("true" if (sb.get("router") or sb.get("proxy")) else "false")
PY
)"
    if [[ "${ROUTER_PIN_REBASE}" == "true" && "${AUTOSYNC_SKIP_GO_TEST:-false}" != "true" ]] && \
       [[ -f "${REPO_ROOT}/go.mod" && -f "${STAGED_TREE}/pkg/worker/controllers/worker/model_deployment_router_test.go" ]] && \
       command -v go >/dev/null 2>&1; then
      PIN_WORK="$(mktemp -d "${TMPDIR:-/tmp}/autosync-router-pin.XXXXXXXX")"
      for entry in "${REPO_ROOT}"/*; do
        base_entry="$(basename "${entry}")"
        if [[ "${base_entry}" != "pkg" ]]; then
          ln -sfn "${entry}" "${PIN_WORK}/${base_entry}"
        fi
      done
      mkdir -p "${PIN_WORK}/pkg"
      for pkg_entry in "${REPO_ROOT}/pkg"/*; do
        pkg_base="$(basename "${pkg_entry}")"
        if [[ "${pkg_base}" != "worker" ]]; then
          ln -sfn "${pkg_entry}" "${PIN_WORK}/pkg/${pkg_base}"
        fi
      done
      mkdir -p "${PIN_WORK}/pkg/worker"
      for wk_entry in "${REPO_ROOT}/pkg/worker"/*; do
        wk_base="$(basename "${wk_entry}")"
        if [[ "${wk_base}" != "settings" && "${wk_base}" != "controllers" ]]; then
          ln -sfn "${wk_entry}" "${PIN_WORK}/pkg/worker/${wk_base}"
        fi
      done
      cp -R "${REPO_ROOT}/pkg/worker/settings" "${PIN_WORK}/pkg/worker/settings"
      cp -R "${REPO_ROOT}/pkg/worker/controllers" "${PIN_WORK}/pkg/worker/controllers"
      cp -f "${STAGED_TREE}/pkg/worker/settings/value.go" "${PIN_WORK}/pkg/worker/settings/value.go"
      cp -f "${STAGED_TREE}/pkg/worker/controllers/worker/model_deployment_router_test.go" \
        "${PIN_WORK}/pkg/worker/controllers/worker/model_deployment_router_test.go"
      python3 - "${PIN_WORK}/pkg/worker/controllers/worker/model_deployment_router_test.go" <<'PY'
import re
import sys

p = sys.argv[1]
with open(p, "r", encoding="utf-8") as f:
    txt = f.read()
txt_empty, n = re.subn(
    r'(pinned\s*:=\s*map\[string\]string\{).*?(\n\t\})',
    r'\1\2',
    txt,
    count=1,
    flags=re.DOTALL,
)
if n != 1:
    raise SystemExit("FAIL: could not empty pinned digest map for rebase")
with open(p, "w", encoding="utf-8") as f:
    f.write(txt_empty)
PY
      PIN_OUT_FILE="${PIN_WORK}/pin.out"
      pin_rc=0
      (
        cd "${PIN_WORK}"
        autosync::go_test ./pkg/worker/controllers/worker \
          -run '^TestRenderModelDeploymentRouterObjects_SerializedOutputIsPinnedToThePreSplitRender$' \
          > "${PIN_OUT_FILE}" 2>&1
      ) || pin_rc=$?
      python3 - "${STAGED_TREE}/pkg/worker/controllers/worker/model_deployment_router_test.go" "${PIN_OUT_FILE}" "${pin_rc}" <<'PY'
import re
import sys

test_p, out_p, rc_s = sys.argv[1:4]
with open(out_p, "r", encoding="utf-8", errors="replace") as f:
    out_txt = f.read()
found = {}
for m in re.finditer(r'no pinned digest for "([^"]+)"; the digest this build produces is ([0-9a-f]{64}) -- pin it in the table', out_txt):
    found[m.group(1)] = m.group(2)
if not found:
    print(f"FAIL: router pin rebase produced no digests (go test exit {rc_s}); output:", file=sys.stderr)
    print(out_txt[:2000], file=sys.stderr)
    raise SystemExit(1)
with open(test_p, "r", encoding="utf-8") as f:
    txt = f.read()
for case_name, digest in found.items():
    pat = re.compile(rf'("{re.escape(case_name)}"\s*:\s*")[0-9a-f]{{64}}(")')
    txt, n = pat.subn(rf'\g<1>{digest}\g<2>', txt, count=1)
    if n != 1:
        raise SystemExit(f"FAIL: could not update pinned digest for {case_name!r} in {test_p}")
with open(test_p, "w", encoding="utf-8") as f:
    f.write(txt)
PY
      rm -rf "${PIN_WORK}" 2>/dev/null || true
    fi

    COMMIT_PATHS+=(
      "pkg/worker/settings/value.go"
      "docs/reference/settings.md"
    )
    for extra_test_rel in \
      "pkg/worker/settings/value_test.go" \
      "pkg/worker/controllers/worker/model_deployment_router_test.go" \
      "pkg/worker/controllers/worker/model_deployment_render_test.go"; do
      if [[ -f "${STAGED_TREE}/${extra_test_rel}" ]]; then
        COMMIT_PATHS+=("${extra_test_rel}")
      fi
    done
  fi

  BUNDLE_SPECS=(
    "${PREFLIGHT_REPORT_JSON}"
    "${UPSTREAM_RECORDS_JSON}"
    "${MANIFESTS_JSON}"
  )
  for sub_dir_name in target-files cherry-pick-working conflicts rebased-patches; do
    if [[ -d "${BUNDLE_WORK}/${sub_dir_name}" ]]; then
      BUNDLE_SPECS+=("${BUNDLE_WORK}/${sub_dir_name}")
    fi
  done
  autosync::preflight::populate_stage_bundle "${STAGED_TREE}" "router" "${BUNDLE_SPECS[@]}"
fi

EMIT_ARGS=(
  --manifests "${MANIFESTS_JSON}"
)
if [[ -n "${STAGED_TREE}" && -d "${STAGED_TREE}" ]]; then
  if [[ -d "${STAGED_TREE}/.auto-sync/.stage/router" ]]; then
    EMIT_ARGS+=(--bundle-dir "${STAGED_TREE}/.auto-sync/.stage/router")
  fi
  CAND_PATHS_CSV=""
  for cp_rel in "${COMMIT_PATHS[@]+"${COMMIT_PATHS[@]}"}"; do
    [[ "${cp_rel}" == ".auto-sync/.stage"* ]] && continue
    CAND_PATHS_CSV="${CAND_PATHS_CSV:+${CAND_PATHS_CSV},}${cp_rel}"
  done
  if [[ -n "${CAND_PATHS_CSV}" ]]; then
    EMIT_ARGS+=(
      --candidate-tree "${STAGED_TREE}"
      --candidate-paths "${CAND_PATHS_CSV}"
    )
  fi
fi

autosync::preflight::emit_taskpack_v2 \
  "router" "${REPO_ROOT}" "${BASE_SHA}" "${STAGE_DIR}" \
  "${UPSTREAM_RECORDS_JSON}" "${PREFLIGHT_REPORT_JSON}" \
  "${EMIT_ARGS[@]}"
