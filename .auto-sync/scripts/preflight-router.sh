#!/usr/bin/env bash
# .auto-sync/scripts/preflight-router.sh - host-side preflight for router-sync.
#
# Discovers highest stable upstream tags for llm-d/llm-d-router (v*),
# vllm-project/router (v*), and sgl-project/sglang (strictly gateway-v*),
# stages clones into $AUTOSYNC_STAGE_DIR, checks commit 516f1cb ancestry for
# automatic retirement of 0001-epd-routing.patch, probes multi-arch image
# manifests via crane, and emits upstream-records.json + router-preflight-report.json.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
# shellcheck source=./lib-preflight.sh
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/lib-preflight.sh"

REPO_ROOT="$(autosync::preflight::repo_root)"
STAGE_DIR="$(autosync::preflight::init_stage "router" "${1:-${AUTOSYNC_STAGE_DIR:-}}")"
BASE_SHA="$(git -C "${REPO_ROOT}" rev-parse HEAD)"

autosync::preflight::snapshot_baselines "${STAGE_DIR}" \
  "pack/llm-router" \
  "pkg/worker/settings/value.go" \
  "docs/reference/settings.md"

if [[ -n "${AUTOSYNC_ROUTER_FIXTURE_DIR:-}" && "${AUTOSYNC_WARM_TOOLS:-false}" != "true" ]]; then
  autosync::preflight::warm_caches "${STAGE_DIR}" --sbin --go
else
  autosync::preflight::warm_caches "${STAGE_DIR}" --sbin --go --tools shellcheck,hugo
fi

PROBE_LIST_FILE="$(mktemp "${TMPDIR:-/tmp}/autosync-router-probes.XXXXXXXX")"
DISCOVERY_JSON="${STAGE_DIR}/router-discovery.json"
trap 'rm -f "${PROBE_LIST_FILE}" "${DISCOVERY_JSON}"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

python3 - "${REPO_ROOT}" "${STAGE_DIR}" "${AUTOSYNC_ROUTER_FIXTURE_DIR:-}" "${DISCOVERY_JSON}" "${PROBE_LIST_FILE}" <<'PY'
import json
import os
import re
import shutil
import subprocess
import sys

repo_root, stage_dir, fixture_dir, discovery_path, probe_list_path = sys.argv[1:6]
sys.path.insert(0, os.path.join(repo_root, ".auto-sync", "checks"))
from dockerfile_arg import read_dockerfile_arg


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
    if os.path.isdir(os.path.join(dst, ".git")):
        subprocess.run(["git", "-C", dst, "remote", "set-url", "origin", repo_url], check=True)
        subprocess.run(
            ["git", "-C", dst, "-c", "protocol.file.allow=always", "fetch", "--tags", "--force", "--prune", "origin"],
            check=True,
        )
    else:
        if os.path.exists(dst):
            shutil.rmtree(dst)
        subprocess.run(
            ["git", "-c", "protocol.file.allow=always", "clone", "--no-single-branch", repo_url, dst],
            check=True,
        )
    ensure_tag_fetched(dst, repo_url, target_tag)
    if pinned_tag and pinned_tag != target_tag:
        try:
            ensure_tag_fetched(dst, repo_url, pinned_tag)
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

target_router_tag = os.environ.get("AUTOSYNC_ROUTER_TARGET_TAG", "").strip()
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

target_proxy_tag = os.environ.get("AUTOSYNC_ROUTER_PROXY_TARGET_TAG", "").strip()
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

target_sidecar_tag = os.environ.get("AUTOSYNC_ROUTER_SIDECAR_TARGET_TAG", "").strip()
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
    "targets": {
        "router_image": target_router_image,
        "proxy_image": target_proxy_image,
        "sidecar_image": target_sidecar_image,
    },
    "epd_commit": epd_commit or epd_ref,
    "retire_0001_epd_routing": retire_0001,
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
    not has_stage_a
    and targets["router_image"] != defaults["router_image"]
    and is_published(targets["router_image"])
)
proxy_stage_b = (
    not has_stage_a
    and targets["proxy_image"] != defaults["proxy_image"]
    and is_published(targets["proxy_image"])
)
sidecar_stage_b = (
    not has_stage_a
    and targets["sidecar_image"] != defaults["sidecar_image"]
    and is_published(targets["sidecar_image"])
)
has_stage_b = router_stage_b or proxy_stage_b or sidecar_stage_b

if has_stage_a:
    stage = "recipe_sync"
    image_published = False
    records = [
        {
            "upstreamId": uid,
            "status": "available" if stage_a_drifts[uid] else "up_to_date",
            "version": target_tags[uid] if stage_a_drifts[uid] else recipe[uid],
            "recentVersions": recent_tags[uid],
        }
        for uid in ("llm-d-router", "vllm-router", "sglang-gateway")
    ]
elif has_stage_b:
    stage = "tag_promotion"
    image_published = True
    records = []
    for uid in ("llm-d-router", "vllm-router", "sglang-gateway"):
        promote = router_stage_b or (uid == "llm-d-router" and (sidecar_stage_b or proxy_stage_b))
        records.append({
            "upstreamId": uid,
            "status": "available" if promote else "up_to_date",
            "version": f"{recipe[uid]}+stage-b" if promote else recipe[uid],
            "recentVersions": recent_tags[uid],
        })
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
            "upstreamId": uid,
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
    "stage": stage,
    "image_published": image_published,
    "retire_0001_epd_routing": disc["retire_0001_epd_routing"],
    "epd_commit": disc["epd_commit"],
    "recipe": recipe,
    "target_tags": target_tags,
    "recent_tags": recent_tags,
    "defaults": defaults,
    "targets": targets,
}

with open(rep_path, "w", encoding="utf-8") as f:
    json.dump(report, f, indent=2)
    f.write("\n")
PY

COMPACT_RECORDS="$(python3 -c 'import json,sys; print(json.dumps(json.load(open(sys.argv[1])), separators=(",",":")))' "${UPSTREAM_RECORDS_JSON}")"
autosync::preflight::write_github_output "stage_dir" "${STAGE_DIR}"
autosync::preflight::write_github_output "upstream_records" "${COMPACT_RECORDS}"
autosync::preflight::write_github_output "source_revision" "${BASE_SHA}"
