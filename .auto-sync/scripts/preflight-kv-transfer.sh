#!/usr/bin/env bash
# .auto-sync/scripts/preflight-kv-transfer.sh - host-side preflight for kv-transfer-sync.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
# shellcheck source=./lib-preflight.sh
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/lib-preflight.sh"

REPO_ROOT="$(autosync::preflight::repo_root)"
STAGE_DIR="$(autosync::preflight::init_stage "kv-transfer" "${1:-${AUTOSYNC_STAGE_DIR:-}}")"
BASE_SHA="$(git -C "${REPO_ROOT}" rev-parse HEAD)"

autosync::preflight::snapshot_baselines "${STAGE_DIR}" \
  "pkg/worker/settings/value.go" \
  "docs/reference/settings.md" \
  "deploy/gpustack-operator/chart/values.yaml" \
  "deploy/gpustack-operator/chart/values.schema.json" \
  "deploy/gpustack-operator/chart/README.md" \
  "pack/mirrored-mooncake/Dockerfile" \
  "pack/model-transfer/Dockerfile" \
  "pack/model-transfer/patches/hixl/001_channel_generation.patch" \
  "pack/mirrored-modelexpress-server/Dockerfile"

if [[ -n "${AUTOSYNC_KV_FIXTURE_DIR:-}" && "${AUTOSYNC_WARM_TOOLS:-false}" != "true" ]]; then
  autosync::preflight::warm_caches "${STAGE_DIR}" --sbin --go
else
  autosync::preflight::warm_caches "${STAGE_DIR}" --sbin --go --tools shellcheck,helm,helm-docs,helm-schema,hugo
fi

# Extract current repository pins and stage upstream data in Python.
PROBE_LIST_FILE="$(mktemp "${TMPDIR:-/tmp}/autosync-kv-probes.XXXXXXXX")"
trap 'rm -f "${PROBE_LIST_FILE}"' EXIT

DISCOVERY_JSON="${STAGE_DIR}/kv-transfer-discovery.json"

python3 - "${REPO_ROOT}" "${STAGE_DIR}" "${AUTOSYNC_KV_FIXTURE_DIR:-}" "${DISCOVERY_JSON}" "${PROBE_LIST_FILE}" <<'PY'
import json
import os
import re
import shutil
import subprocess
import sys
import urllib.request

repo_root, stage_dir, fixture_dir, discovery_path, probe_list_path = sys.argv[1:6]


def read_text(path: str) -> str:
    with open(path, "r", encoding="utf-8") as f:
        return f.read()


def extract_arg(text: str, name: str) -> str:
    m = re.search(rf'^ARG {re.escape(name)}=(?:"([^"]+)"|([^\s#]+))', text, re.MULTILINE)
    if not m:
        raise SystemExit(f"FAIL: missing ARG {name}")
    return (m.group(1) or m.group(2)).strip()


def extract_value_go_image(text: str, var_name: str) -> str:
    m = re.search(
        rf'{re.escape(var_name)}\s*=\s*settings\.NewEditable\([^)]*?setting\.InitializeFromEnv\("([^"]+)"\)',
        text,
        re.DOTALL,
    )
    if not m:
        raise SystemExit(f"FAIL: missing {var_name} default in value.go")
    return m.group(1).strip()


def extract_values_yaml_mt_images(text: str) -> dict[str, str]:
    out = {}
    in_mt = False
    in_images = False
    for raw in text.splitlines():
        if raw.strip() == "" or raw.lstrip().startswith("#"):
            continue
        if not raw.startswith(" "):
            in_mt = raw == "modelTransfer:" or raw.startswith("modelTransfer: ")
            in_images = False
        elif in_mt and re.match(r"^  [A-Za-z0-9_-]+:", raw):
            in_images = raw == "  images:" or raw.startswith("  images: ")
        elif in_mt and in_images:
            m = re.match(r'^    (nvidia|amd|ascend):\s*(?:"([^"]+)"|\'([^\']+)\'|([^\s#]+))', raw)
            if m:
                out[m.group(1)] = (m.group(2) or m.group(3) or m.group(4)).strip()
    return out


def fetch_json(url: str, headers: dict[str, str] | None = None):
    req = urllib.request.Request(url, headers=headers or {"User-Agent": "gpustack-autosync/1.0"})
    with urllib.request.urlopen(req, timeout=30) as resp:
        return json.loads(resp.read().decode("utf-8"))


def parse_version_key(ver: str):
    parts = re.findall(r"\d+|[a-zA-Z]+", ver.lstrip("v"))
    key = []
    for p in parts:
        if p.isdigit():
            key.append((0, int(p), ""))
        else:
            key.append((1, 0, p.lower()))
    return key


def is_prerelease_version(ver: str) -> bool:
    v = ver.lstrip("v")
    if "-" in v:
        return True
    if re.search(r"(?:^|[0-9.])(a|b|rc|alpha|beta|dev)[0-9]*$", v, re.IGNORECASE):
        return True
    return False


def has_manylinux_arch(files: list[dict], arch: str) -> bool:
    pat = re.compile(rf"(?:manylinux|linux)[^/]*{re.escape(arch)}\.whl$")
    for f in files:
        if f.get("yanked"):
            continue
        if f.get("packagetype") != "bdist_wheel":
            continue
        fn = str(f.get("filename") or "")
        if pat.search(fn):
            return True
    return False


def has_meta_wheel(files: list[dict]) -> bool:
    for f in files:
        if f.get("yanked"):
            continue
        if f.get("packagetype") != "bdist_wheel":
            continue
        fn = str(f.get("filename") or "")
        if fn.endswith("-none-any.whl"):
            return True
    return has_manylinux_arch(files, "x86_64") and has_manylinux_arch(files, "aarch64")


def resolve_nixl_pypi(fixture_root: str) -> tuple[str, list[str]]:
    pkgs = {}
    for pkg in ("nixl", "nixl-cu12", "nixl-cu13"):
        if fixture_root:
            fp = os.path.join(fixture_root, "pypi", f"{pkg}.json")
            if os.path.isfile(fp):
                with open(fp, "r", encoding="utf-8") as f:
                    pkgs[pkg] = json.load(f)
                continue
            return "", []
        pkgs[pkg] = fetch_json(f"https://pypi.org/pypi/{pkg}/json")

    common = set(pkgs["nixl"].get("releases", {}).keys())
    common &= set(pkgs["nixl-cu12"].get("releases", {}).keys())
    common &= set(pkgs["nixl-cu13"].get("releases", {}).keys())

    candidates = []
    for ver in common:
        if is_prerelease_version(ver):
            continue
        f_meta = pkgs["nixl"]["releases"].get(ver, [])
        f_cu12 = pkgs["nixl-cu12"]["releases"].get(ver, [])
        f_cu13 = pkgs["nixl-cu13"]["releases"].get(ver, [])
        if (
            has_meta_wheel(f_meta)
            and has_manylinux_arch(f_cu12, "x86_64")
            and has_manylinux_arch(f_cu12, "aarch64")
            and has_manylinux_arch(f_cu13, "x86_64")
            and has_manylinux_arch(f_cu13, "aarch64")
        ):
            candidates.append(ver)

    if not candidates:
        raise SystemExit("FAIL: no NIXL version with 3-package x86_64+aarch64 wheel parity found")
    candidates.sort(key=parse_version_key, reverse=True)
    return candidates[0], candidates[:5]


def resolve_mooncake_release(fixture_root: str) -> tuple[str, list[str]]:
    releases = None
    if fixture_root:
        fp = os.path.join(fixture_root, "mooncake-release.json")
        if os.path.isfile(fp):
            with open(fp, "r", encoding="utf-8") as f:
                raw = json.load(f)
            releases = raw if isinstance(raw, list) else [raw]
    else:
        headers = {
            "User-Agent": "gpustack-autosync/1.0",
            "Accept": "application/vnd.github+json",
        }
        tok = os.environ.get("GH_TOKEN") or os.environ.get("GITHUB_TOKEN") or ""
        if tok:
            headers["Authorization"] = f"Bearer {tok}"
        releases = fetch_json(
            "https://api.github.com/repos/kvcache-ai/Mooncake/releases?per_page=20",
            headers=headers,
        )

    if not releases:
        return "", []

    stable = []
    for rel in releases:
        if not isinstance(rel, dict):
            continue
        if rel.get("prerelease") or rel.get("draft"):
            continue
        tag = str(rel.get("tag_name") or "").strip()
        if not tag:
            continue
        ver = tag[1:] if tag.startswith("v") else tag
        if is_prerelease_version(ver):
            continue
        stable.append(ver)

    if not stable:
        return "", []
    stable.sort(key=parse_version_key, reverse=True)
    return stable[0], stable[:5]


def sync_git_repo(url: str, dst: str, pin_commit: str = "") -> str:
    if os.path.isdir(os.path.join(dst, ".git")):
        subprocess.run(["git", "-C", dst, "fetch", "--depth", "1", "origin"], check=True)
        subprocess.run(["git", "-C", dst, "checkout", "--detach", "FETCH_HEAD"], check=True)
    else:
        if os.path.exists(dst):
            shutil.rmtree(dst)
        subprocess.run(["git", "clone", "--depth", "1", url, dst], check=True)
    head_sha = subprocess.check_output(["git", "-C", dst, "rev-parse", "HEAD"], text=True).strip()
    if pin_commit and re.fullmatch(r"[0-9a-f]{40}", pin_commit):
        rc = subprocess.run(
            ["git", "-C", dst, "cat-file", "-e", f"{pin_commit}^{{commit}}"],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        ).returncode
        if rc != 0:
            subprocess.run(["git", "-C", dst, "fetch", "--depth", "1", "origin", pin_commit], check=False)
    return head_sha


def stage_fixture_repo(src: str, dst: str) -> str:
    if os.path.exists(dst):
        shutil.rmtree(dst)
    shutil.copytree(src, dst)
    if os.path.isdir(os.path.join(dst, ".git")):
        return subprocess.check_output(["git", "-C", dst, "rev-parse", "HEAD"], text=True).strip()
    return "fixture"


mooncake_df = read_text(os.path.join(repo_root, "pack/mirrored-mooncake/Dockerfile"))
mt_df = read_text(os.path.join(repo_root, "pack/model-transfer/Dockerfile"))
mx_df = read_text(os.path.join(repo_root, "pack/mirrored-modelexpress-server/Dockerfile"))
value_go = read_text(os.path.join(repo_root, "pkg/worker/settings/value.go"))
values_yaml = read_text(os.path.join(repo_root, "deploy/gpustack-operator/chart/values.yaml"))
local_hixl_patch_path = os.path.join(repo_root, "pack/model-transfer/patches/hixl/001_channel_generation.patch")
with open(local_hixl_patch_path, "rb") as f:
    local_hixl_patch_bytes = f.read()

recipe = {
    "mooncake_version": extract_arg(mooncake_df, "MOONCAKE_VERSION"),
    "nixl_version": extract_arg(mt_df, "NIXL_VERSION"),
    "nixl_rocm_ref": extract_arg(mt_df, "NIXL_ROCM_REF"),
    "ucx_rocm_ref": extract_arg(mt_df, "NIXL_ROCM_UCX_REF"),
    "hixl_commit": extract_arg(mt_df, "HIXL_COMMIT"),
    "modelexpress_commit": extract_arg(mx_df, "MODELEXPRESS_COMMIT"),
}
defaults = {
    "kv_cache_backend_image": extract_value_go_image(value_go, "KVCacheBackendImage"),
    "modelexpress_server_image": extract_value_go_image(value_go, "ModelDeploymentModelExpressServerImage"),
    "model_transfer_images": extract_values_yaml_mt_images(values_yaml),
}

upstream_override = {}
if fixture_dir and os.path.isfile(os.path.join(fixture_dir, "upstream.json")):
    with open(os.path.join(fixture_dir, "upstream.json"), "r", encoding="utf-8") as f:
        upstream_override = json.load(f)

if "mooncake" in upstream_override:
    up_mooncake = str(upstream_override["mooncake"]).lstrip("v")
    up_mooncake_recent = [up_mooncake]
else:
    up_mooncake, up_mooncake_recent = resolve_mooncake_release(fixture_dir)
    if not up_mooncake:
        up_mooncake = recipe["mooncake_version"]
        up_mooncake_recent = [up_mooncake]

if "nixl_pypi" in upstream_override:
    up_nixl_pypi = str(upstream_override["nixl_pypi"])
    up_nixl_recent = [up_nixl_pypi]
else:
    up_nixl_pypi, up_nixl_recent = resolve_nixl_pypi(fixture_dir)
    if not up_nixl_pypi:
        up_nixl_pypi = recipe["nixl_version"]
        up_nixl_recent = [up_nixl_pypi]

# Stage gpustack/runner, hixl, nixl, ucx, modelexpress.
runner_stage = os.path.join(stage_dir, "gpustack-runner")
hixl_stage = os.path.join(stage_dir, "hixl")
nixl_stage = os.path.join(stage_dir, "nixl")
ucx_stage = os.path.join(stage_dir, "ucx")
mx_stage = os.path.join(stage_dir, "modelexpress")

if fixture_dir:
    runner_fix = os.path.join(fixture_dir, "repos/gpustack-runner")
    if os.path.isdir(runner_fix):
        runner_head = stage_fixture_repo(runner_fix, runner_stage)
    else:
        runner_head = "aligned"

    hixl_fix = os.path.join(fixture_dir, "repos/hixl")
    up_hixl = stage_fixture_repo(hixl_fix, hixl_stage) if os.path.isdir(hixl_fix) else recipe["hixl_commit"]

    nixl_fix = os.path.join(fixture_dir, "repos/nixl")
    up_nixl_rocm = stage_fixture_repo(nixl_fix, nixl_stage) if os.path.isdir(nixl_fix) else recipe["nixl_rocm_ref"]

    ucx_fix = os.path.join(fixture_dir, "repos/ucx")
    up_ucx_rocm = stage_fixture_repo(ucx_fix, ucx_stage) if os.path.isdir(ucx_fix) else recipe["ucx_rocm_ref"]

    mx_fix = os.path.join(fixture_dir, "repos/modelexpress")
    up_mx = stage_fixture_repo(mx_fix, mx_stage) if os.path.isdir(mx_fix) else recipe["modelexpress_commit"]
else:
    runner_head = sync_git_repo("https://github.com/gpustack/runner.git", runner_stage)
    up_hixl = sync_git_repo("https://github.com/hicann/hixl.git", hixl_stage, recipe["hixl_commit"])
    up_nixl_rocm = sync_git_repo("https://github.com/ai-dynamo/nixl.git", nixl_stage, recipe["nixl_rocm_ref"])
    up_ucx_rocm = sync_git_repo("https://github.com/openucx/ucx.git", ucx_stage, recipe["ucx_rocm_ref"])
    up_mx = sync_git_repo("https://github.com/ai-dynamo/modelexpress.git", mx_stage, recipe["modelexpress_commit"])

if "hixl" in upstream_override:
    up_hixl = str(upstream_override["hixl"])
if "nixl_rocm" in upstream_override:
    up_nixl_rocm = str(upstream_override["nixl_rocm"])
if "ucx_rocm" in upstream_override:
    up_ucx_rocm = str(upstream_override["ucx_rocm"])
if "modelexpress" in upstream_override:
    up_mx = str(upstream_override["modelexpress"])
if "gpustack_runner" in upstream_override:
    runner_head = str(upstream_override["gpustack_runner"])

# Inspect gpustack/runner HIXL commit and patch parity.
runner_hixl_commit = ""
runner_patch_match = True
runner_cann_df = os.path.join(runner_stage, "pack/cann/Dockerfile.vllm")
if os.path.isfile(runner_cann_df):
    m = re.search(r'^ARG VLLM_HIXL_COMMIT=(?:"([^"]+)"|([^\s#]+))', read_text(runner_cann_df), re.MULTILINE)
    if m:
        runner_hixl_commit = (m.group(1) or m.group(2)).strip()
runner_patch_path = os.path.join(runner_stage, "pack/cann/patches/hixl/001_channel_generation.patch")
if os.path.isfile(runner_patch_path):
    with open(runner_patch_path, "rb") as f:
        runner_patch_match = f.read() == local_hixl_patch_bytes

# Compute candidate target image tags for Stage B probing.
target_mooncake_image = f"gpustack/mirrored-mooncake:{recipe['mooncake_version']}-cpu"
target_mx_tag = os.environ.get("AUTOSYNC_MODELEXPRESS_TARGET_TAG", "").strip()
if not target_mx_tag and isinstance(upstream_override.get("modelexpress_target_tag"), str):
    target_mx_tag = upstream_override["modelexpress_target_tag"].strip()
target_mx_image = (
    f"gpustack/mirrored-modelexpress-server:{target_mx_tag}"
    if target_mx_tag
    else defaults["modelexpress_server_image"]
)

probe_refs = []
for ref in (
    defaults["kv_cache_backend_image"],
    target_mooncake_image,
    f"gpustack/mirrored-mooncake:{up_mooncake}-cpu",
    defaults["modelexpress_server_image"],
    target_mx_image,
    defaults["model_transfer_images"].get("nvidia", ""),
    defaults["model_transfer_images"].get("amd", ""),
    defaults["model_transfer_images"].get("ascend", ""),
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
    "upstream": {
        "mooncake": up_mooncake,
        "mooncake_recent": up_mooncake_recent,
        "nixl_pypi": up_nixl_pypi,
        "nixl_pypi_recent": up_nixl_recent,
        "nixl_rocm": up_nixl_rocm,
        "ucx_rocm": up_ucx_rocm,
        "hixl": up_hixl,
        "modelexpress": up_mx,
        "gpustack_runner": runner_head,
    },
    "runner": {
        "hixl_commit": runner_hixl_commit,
        "patch_match": runner_patch_match,
    },
    "targets": {
        "mooncake_image": target_mooncake_image,
        "modelexpress_image": target_mx_image,
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
if [[ -n "${AUTOSYNC_KV_FIXTURE_DIR:-}" && -f "${AUTOSYNC_KV_FIXTURE_DIR}/image-manifests.json" ]]; then
  AUTOSYNC_MOCK_MANIFESTS_JSON="${AUTOSYNC_KV_FIXTURE_DIR}/image-manifests.json" \
    autosync::preflight::record_image_probes "${MANIFESTS_JSON}" "${PROBE_REFS[@]}"
else
  autosync::preflight::record_image_probes "${MANIFESTS_JSON}" "${PROBE_REFS[@]}"
fi

UPSTREAM_RECORDS_JSON="${STAGE_DIR}/upstream-records.json"
PREFLIGHT_REPORT_JSON="${STAGE_DIR}/kv-transfer-preflight-report.json"

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
up = disc["upstream"]
runner = disc["runner"]
targets = disc["targets"]

runner_hixl_drift = bool(runner["hixl_commit"] and runner["hixl_commit"] != recipe["hixl_commit"])
runner_patch_drift = not bool(runner["patch_match"])

stage_a_drifts = {
    "mooncake": up["mooncake"] != recipe["mooncake_version"],
    "nixl-pypi": up["nixl_pypi"] != recipe["nixl_version"],
    "nixl-rocm": up["nixl_rocm"] != recipe["nixl_rocm_ref"],
    "ucx-rocm": up["ucx_rocm"] != recipe["ucx_rocm_ref"],
    "hixl": (up["hixl"] != recipe["hixl_commit"]) or runner_hixl_drift,
    "modelexpress": up["modelexpress"] != recipe["modelexpress_commit"],
    "gpustack-runner": runner_hixl_drift or runner_patch_drift,
}

has_stage_a = any(stage_a_drifts.values())

mooncake_stage_b = (
    not has_stage_a
    and targets["mooncake_image"] != defaults["kv_cache_backend_image"]
    and is_published(targets["mooncake_image"])
)
mx_stage_b = (
    not has_stage_a
    and targets["modelexpress_image"] != defaults["modelexpress_server_image"]
    and is_published(targets["modelexpress_image"])
)
has_stage_b = mooncake_stage_b or mx_stage_b

records = []
if has_stage_a:
    stage = "recipe_sync"
    image_published = False
    target_hixl = runner["hixl_commit"] if runner_hixl_drift else up["hixl"]
    records = [
        {
            "upstreamId": "mooncake",
            "status": "available" if stage_a_drifts["mooncake"] else "up_to_date",
            "version": up["mooncake"] if stage_a_drifts["mooncake"] else recipe["mooncake_version"],
            "recentVersions": up["mooncake_recent"],
        },
        {
            "upstreamId": "nixl-pypi",
            "status": "available" if stage_a_drifts["nixl-pypi"] else "up_to_date",
            "version": up["nixl_pypi"] if stage_a_drifts["nixl-pypi"] else recipe["nixl_version"],
            "recentVersions": up["nixl_pypi_recent"],
        },
        {
            "upstreamId": "nixl-rocm",
            "status": "available" if stage_a_drifts["nixl-rocm"] else "up_to_date",
            "version": up["nixl_rocm"] if stage_a_drifts["nixl-rocm"] else recipe["nixl_rocm_ref"],
        },
        {
            "upstreamId": "ucx-rocm",
            "status": "available" if stage_a_drifts["ucx-rocm"] else "up_to_date",
            "version": up["ucx_rocm"] if stage_a_drifts["ucx-rocm"] else recipe["ucx_rocm_ref"],
        },
        {
            "upstreamId": "hixl",
            "status": "available" if stage_a_drifts["hixl"] else "up_to_date",
            "version": target_hixl if stage_a_drifts["hixl"] else recipe["hixl_commit"],
        },
        {
            "upstreamId": "modelexpress",
            "status": "available" if stage_a_drifts["modelexpress"] else "up_to_date",
            "version": up["modelexpress"] if stage_a_drifts["modelexpress"] else recipe["modelexpress_commit"],
        },
        {
            "upstreamId": "gpustack-runner",
            "status": "available" if stage_a_drifts["gpustack-runner"] else "up_to_date",
            "version": up["gpustack_runner"] if stage_a_drifts["gpustack-runner"] else "aligned",
        },
    ]
elif has_stage_b:
    stage = "tag_promotion"
    image_published = True
    records = [
        {
            "upstreamId": "mooncake",
            "status": "available" if mooncake_stage_b else "up_to_date",
            "version": f"{recipe['mooncake_version']}+stage-b" if mooncake_stage_b else recipe["mooncake_version"],
            "recentVersions": up["mooncake_recent"],
        },
        {
            "upstreamId": "nixl-pypi",
            "status": "up_to_date",
            "version": recipe["nixl_version"],
            "recentVersions": up["nixl_pypi_recent"],
        },
        {
            "upstreamId": "nixl-rocm",
            "status": "up_to_date",
            "version": recipe["nixl_rocm_ref"],
        },
        {
            "upstreamId": "ucx-rocm",
            "status": "up_to_date",
            "version": recipe["ucx_rocm_ref"],
        },
        {
            "upstreamId": "hixl",
            "status": "up_to_date",
            "version": recipe["hixl_commit"],
        },
        {
            "upstreamId": "modelexpress",
            "status": "available" if mx_stage_b else "up_to_date",
            "version": f"{recipe['modelexpress_commit']}+stage-b" if mx_stage_b else recipe["modelexpress_commit"],
        },
        {
            "upstreamId": "gpustack-runner",
            "status": "up_to_date",
            "version": "aligned",
        },
    ]
else:
    stage = "no_change"
    image_published = (
        targets["mooncake_image"] == defaults["kv_cache_backend_image"]
        and is_published(targets["mooncake_image"])
    )
    records = [
        {
            "upstreamId": "mooncake",
            "status": "up_to_date",
            "version": recipe["mooncake_version"],
            "recentVersions": up["mooncake_recent"],
        },
        {
            "upstreamId": "nixl-pypi",
            "status": "up_to_date",
            "version": recipe["nixl_version"],
            "recentVersions": up["nixl_pypi_recent"],
        },
        {
            "upstreamId": "nixl-rocm",
            "status": "up_to_date",
            "version": recipe["nixl_rocm_ref"],
        },
        {
            "upstreamId": "ucx-rocm",
            "status": "up_to_date",
            "version": recipe["ucx_rocm_ref"],
        },
        {
            "upstreamId": "hixl",
            "status": "up_to_date",
            "version": recipe["hixl_commit"],
        },
        {
            "upstreamId": "modelexpress",
            "status": "up_to_date",
            "version": recipe["modelexpress_commit"],
        },
        {
            "upstreamId": "gpustack-runner",
            "status": "up_to_date",
            "version": "aligned",
        },
    ]

with open(recs_path, "w", encoding="utf-8") as f:
    json.dump(records, f, indent=2)
    f.write("\n")

report = {
    "schemaVersion": 1,
    "stage": stage,
    "image_published": image_published,
    "enforce_runner_hixl_parity": True,
    "recipe": recipe,
    "defaults": defaults,
    "targets": targets,
    "nixl_pypi": {
        "selected_version": up["nixl_pypi"],
        "recent_versions": up["nixl_pypi_recent"],
    },
    "runner": runner,
}

with open(rep_path, "w", encoding="utf-8") as f:
    json.dump(report, f, indent=2)
    f.write("\n")
PY

rm -f "${DISCOVERY_JSON}"

COMPACT_RECORDS="$(python3 -c 'import json,sys; print(json.dumps(json.load(open(sys.argv[1])), separators=(",",":")))' "${UPSTREAM_RECORDS_JSON}")"
autosync::preflight::write_github_output "stage_dir" "${STAGE_DIR}"
autosync::preflight::write_github_output "upstream_records" "${COMPACT_RECORDS}"
autosync::preflight::write_github_output "source_revision" "${BASE_SHA}"
