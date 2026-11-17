#!/usr/bin/env bash
# .auto-sync/scripts/preflight-charts.sh - host-side preflight for charts-sync.
#
# Discovers upstream Helm chart versions for kueue, node-feature-discovery,
# csi-driver-nfs, csi-driver-s3, and topograph, caches .tgz archives into
# $AUTOSYNC_STAGE_DIR/deps-cache/charts/, checks patch application, probes
# docker.io/gpustack/mirrored-* multi-arch manifests, and stages ready updates
# onto a detached source_revision commit while leaving HEAD and working tree clean.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
# shellcheck source=./lib-preflight.sh
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/lib-preflight.sh"

REPO_ROOT="$(autosync::preflight::repo_root)"
STAGE_DIR="$(autosync::preflight::init_stage "charts" "${1:-${AUTOSYNC_STAGE_DIR:-}}")"
BASE_SHA="$(git -C "${REPO_ROOT}" rev-parse HEAD)"

autosync::preflight::snapshot_baselines "${STAGE_DIR}" \
  "hack/deps.sh" \
  "deploy/gpustack-operator/chart/values.yaml" \
  "deploy/gpustack-operator/chart/Chart.yaml" \
  "deploy/gpustack-operator/chart/values.schema.json" \
  "deploy/gpustack-operator/chart/README.md"

if [[ -n "${AUTOSYNC_CHARTS_FIXTURE_DIR:-}" && "${AUTOSYNC_WARM_TOOLS:-false}" != "true" ]]; then
  autosync::preflight::warm_caches "${STAGE_DIR}" --sbin --go
else
  autosync::preflight::warm_caches "${STAGE_DIR}" --sbin --go --tools shellcheck,helm,helm-docs,helm-schema
fi

if [[ -z "${AUTOSYNC_CHARTS_FIXTURE_DIR:-}" ]] && command -v docker >/dev/null 2>&1; then
  CT_VERSION="${CHART_TESTING_VERSION:-}"
  if [[ -z "${CT_VERSION}" && -f "${REPO_ROOT}/hack/lib/helm.sh" ]]; then
    CT_VERSION="$(sed -n 's/^chart_testing_version=.*:-"\([^"]*\)".*$/\1/p' "${REPO_ROOT}/hack/lib/helm.sh" | head -n 1)"
  fi
  CT_VERSION="${CT_VERSION:-v3.14.0}"
  CT_IMAGE="quay.io/helmpack/chart-testing:${CT_VERSION}"
  docker image inspect "${CT_IMAGE}" >/dev/null 2>&1 || docker pull "${CT_IMAGE}" || true
fi

mkdir -p "${STAGE_DIR}/deps-cache/charts"

DISCOVERY_JSON="${STAGE_DIR}/charts-discovery.json"
PROBE_LIST_FILE="$(mktemp "${TMPDIR:-/tmp}/autosync-charts-probes.XXXXXXXX")"
WORK_TMP="$(mktemp -d "${TMPDIR:-/tmp}/autosync-charts-work.XXXXXXXX")"

cleanup_preflight() {
  rm -f "${PROBE_LIST_FILE}"
  rm -rf "${WORK_TMP}"
}
trap cleanup_preflight EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

# Discover upstream chart versions, cache .tgz archives, check patches, and gather probe refs.
python3 - "${REPO_ROOT}" "${STAGE_DIR}" "${AUTOSYNC_CHARTS_FIXTURE_DIR:-}" "${DISCOVERY_JSON}" "${PROBE_LIST_FILE}" "${WORK_TMP}" <<'PY'
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import tarfile
import urllib.parse
import urllib.request

try:
    import yaml as yaml_mod
except ImportError:
    yaml_mod = None

repo_root, stage_dir, fixture_dir, discovery_path, probe_list_path, work_tmp = sys.argv[1:7]
deps_cache_charts = os.path.join(stage_dir, "deps-cache", "charts")
os.makedirs(deps_cache_charts, exist_ok=True)


def load_yaml(path: str):
    with open(path, "r", encoding="utf-8") as f:
        raw = f.read()
    if yaml_mod is not None:
        return yaml_mod.safe_load(raw)
    res = subprocess.run(
        ["ruby", "-ryaml", "-rjson", "-e", "puts JSON.generate(YAML.safe_load(STDIN.read))"],
        input=raw,
        text=True,
        capture_output=True,
        check=True,
    )
    return json.loads(res.stdout)


def parse_semver_key(ver: str):
    v = ver.lstrip("v")
    parts = re.findall(r"\d+|[a-zA-Z]+", v)
    out = []
    for p in parts:
        if p.isdigit():
            out.append((0, int(p), ""))
        else:
            out.append((1, 0, p.lower()))
    return out


def is_prerelease(ver: str) -> bool:
    v = ver.lstrip("v")
    if "-" in v:
        return True
    if re.search(r"(?:^|[0-9.])(a|b|rc|alpha|beta|dev)[0-9]*$", v, re.IGNORECASE):
        return True
    return False


def fetch_bytes(url: str, headers: dict[str, str] | None = None) -> bytes:
    req = urllib.request.Request(url, headers=headers or {"User-Agent": "gpustack-autosync/1.0"})
    with urllib.request.urlopen(req, timeout=60) as resp:
        return resp.read()


def fetch_json(url: str):
    headers = {
        "User-Agent": "gpustack-autosync/1.0",
        "Accept": "application/vnd.github+json",
    }
    tok = os.environ.get("GH_TOKEN") or os.environ.get("GITHUB_TOKEN") or ""
    if tok and "api.github.com" in url:
        headers["Authorization"] = f"Bearer {tok}"
    return json.loads(fetch_bytes(url, headers=headers).decode("utf-8"))


def fetch_yaml(url: str):
    raw = fetch_bytes(url).decode("utf-8")
    if yaml_mod is not None:
        return yaml_mod.safe_load(raw)
    res = subprocess.run(
        ["ruby", "-ryaml", "-rjson", "-e", "puts JSON.generate(YAML.safe_load(STDIN.read))"],
        input=raw,
        text=True,
        capture_output=True,
        check=True,
    )
    return json.loads(res.stdout)


def parse_chart_staging(deps_sh_path: str) -> dict[str, dict[str, str]]:
    with open(deps_sh_path, "r", encoding="utf-8") as f:
        text = f.read()
    m = re.search(r"function chart_staging\(\)\s*\{.*?cat <<EOF\n(.*?)\nEOF", text, re.DOTALL)
    if not m:
        raise SystemExit(f"FAIL: missing chart_staging() heredoc in {deps_sh_path}")
    pinned = {}
    for line in m.group(1).splitlines():
        line = line.strip()
        if not line:
            continue
        url, ver, dest = line.split()
        name = os.path.basename(dest)
        pinned[name] = {
            "url": url,
            "version": ver,
            "dest": dest,
            "archive_name": os.path.basename(urllib.parse.urlsplit(url).path),
        }
    return pinned


def extract_current_mirrored_images(values_path: str, topo_chart_path: str) -> list[str]:
    doc = load_yaml(values_path)
    topo_chart = load_yaml(topo_chart_path) if os.path.isfile(topo_chart_path) else {}
    topo_app_ver = str((topo_chart or {}).get("appVersion") or "").strip()

    refs = []

    def add_ref(repo: str, tag: str, base_repo: str = ""):
        repo = (repo or "").strip()
        tag = (tag or "").strip()
        if base_repo and repo.startswith("/"):
            repo = base_repo.rstrip("/") + repo
        if not repo or "gpustack/mirrored-" not in repo:
            return
        if ":" in repo.split("/")[-1]:
            full = repo
        elif tag:
            full = f"{repo}:{tag}"
        else:
            return
        if full not in refs:
            refs.append(full)

    kueue_img = (((doc.get("kueue") or {}).get("controllerManager") or {}).get("manager") or {}).get("image") or {}
    add_ref(str(kueue_img.get("repository") or ""), str(kueue_img.get("tag") or ""))

    nfd_img = (doc.get("node-feature-discovery") or {}).get("image") or {}
    add_ref(str(nfd_img.get("repository") or ""), str(nfd_img.get("tag") or ""))

    nfs_img = (doc.get("csi-driver-nfs") or {}).get("image") or {}
    nfs_base = str(nfs_img.get("baseRepo") or "")
    for key in ("nfs", "csiProvisioner", "csiResizer", "csiSnapshotter", "livenessProbe", "nodeDriverRegistrar"):
        item = nfs_img.get(key) or {}
        add_ref(str(item.get("repository") or ""), str(item.get("tag") or ""), nfs_base)

    s3_imgs = (doc.get("csi-driver-s3") or {}).get("images") or {}
    for key in ("s3", "csiProvisioner", "livenessProbe", "nodeDriverRegistrar"):
        val = str(s3_imgs.get(key) or "").strip()
        if val and "gpustack/mirrored-" in val and val not in refs:
            refs.append(val)

    topo_img = (doc.get("topograph") or {}).get("image") or {}
    topo_tag = str(topo_img.get("tag") or "").strip() or topo_app_ver
    add_ref(str(topo_img.get("repository") or ""), topo_tag)

    mm_reg = ((doc.get("modelManager") or {}).get("registrar") or {}).get("image") or {}
    add_ref(str(mm_reg.get("repository") or ""), str(mm_reg.get("tag") or ""))

    return refs


def find_fixture_archive(fix_root: str, chart_name: str, ver: str, url: str, archive_hint: str = "") -> str:
    arc_url = os.path.basename(urllib.parse.urlsplit(url).path) if url else ""
    candidates = []
    for cand_name in [archive_hint, f"{chart_name}-{ver}.tgz", arc_url]:
        if not cand_name:
            continue
        candidates.append(os.path.join(fix_root, "archives", cand_name))
        candidates.append(os.path.join(fix_root, cand_name))
    for p in candidates:
        if os.path.isfile(p):
            return p
    return ""


def normalize_sha256_digest(raw_val) -> str:
    val = str(raw_val or "").strip().lower()
    if val.startswith("sha256:"):
        val = val[len("sha256:"):]
    return val if re.fullmatch(r"[0-9a-f]{64}", val) else ""


def cache_archive(
    chart_name: str,
    ver: str,
    url: str,
    fix_root: str,
    archive_hint: str = "",
    required: bool = True,
    expected_sha256: str = "",
) -> tuple[str, str]:
    dst = os.path.join(deps_cache_charts, f"{chart_name}-{ver}.tgz")
    expected_norm = normalize_sha256_digest(expected_sha256)
    if expected_sha256 and not expected_norm:
        raise SystemExit(f"FAIL: invalid expected SHA-256 digest for {chart_name} {ver}: {expected_sha256!r}")

    if os.path.isfile(dst):
        with open(dst, "rb") as f:
            data = f.read()
    elif fix_root:
        src = find_fixture_archive(fix_root, chart_name, ver, url, archive_hint)
        if src:
            with open(src, "rb") as f:
                data = f.read()
        elif required:
            raise SystemExit(f"FAIL: missing fixture chart archive for {chart_name} {ver} in {fix_root}")
        else:
            return "", ""
    else:
        data = fetch_bytes(url)

    actual_sha256 = hashlib.sha256(data).hexdigest()
    if expected_norm and actual_sha256 != expected_norm:
        raise SystemExit(
            f"FAIL: archive SHA-256 mismatch for {chart_name} {ver}: "
            f"expected {expected_norm}, got {actual_sha256}"
        )

    if not os.path.isfile(dst):
        with open(dst, "wb") as f:
            f.write(data)
    return dst, actual_sha256


def resolve_github_release_chart(repo_slug: str, asset_prefix: str, fallback_url_tmpl: str, pinned_ver: str, pinned_url: str):
    releases = fetch_json(f"https://api.github.com/repos/{repo_slug}/releases?per_page=20")
    candidates = []
    for rel in releases:
        if not isinstance(rel, dict) or rel.get("prerelease") or rel.get("draft"):
            continue
        tag = str(rel.get("tag_name") or "").strip()
        if not tag:
            continue
        ver = tag[1:] if tag.startswith("v") else tag
        if not re.match(r"^\d+\.\d+\.\d+$", ver) or is_prerelease(ver):
            continue
        dl_url = ""
        asset_sha = ""
        expected_asset = f"{asset_prefix}-{ver}.tgz"
        for asset in rel.get("assets") or []:
            if isinstance(asset, dict) and asset.get("name") == expected_asset:
                dl_url = str(asset.get("browser_download_url") or "")
                asset_sha = normalize_sha256_digest(asset.get("digest"))
                break
        if not dl_url:
            dl_url = fallback_url_tmpl.format(tag=tag, ver=ver)
        candidates.append((parse_semver_key(ver), ver, dl_url, asset_sha))
    if not candidates:
        return pinned_ver, pinned_url, ""
    candidates.sort(key=lambda x: x[0], reverse=True)
    _, best_ver, best_url, best_sha = candidates[0]
    if parse_semver_key(best_ver) < parse_semver_key(pinned_ver):
        return pinned_ver, pinned_url, ""
    return best_ver, best_url, best_sha


def resolve_helm_index_chart(index_url: str, entry_name: str, base_url: str, pinned_ver: str, pinned_url: str):
    doc = fetch_yaml(index_url)
    entries = ((doc or {}).get("entries") or {}).get(entry_name) or []
    candidates = []
    for item in entries:
        if not isinstance(item, dict):
            continue
        ver = str(item.get("version") or "").strip().lstrip("v")
        if not re.match(r"^\d+\.\d+\.\d+$", ver) or is_prerelease(ver):
            continue
        urls = item.get("urls") or []
        if not urls:
            continue
        dl_url = urllib.parse.urljoin(base_url, str(urls[0]))
        entry_sha = normalize_sha256_digest(item.get("digest"))
        candidates.append((parse_semver_key(ver), ver, dl_url, entry_sha))
    if not candidates:
        return pinned_ver, pinned_url, ""
    candidates.sort(key=lambda x: x[0], reverse=True)
    _, best_ver, best_url, best_sha = candidates[0]
    if parse_semver_key(best_ver) < parse_semver_key(pinned_ver):
        return pinned_ver, pinned_url, ""
    return best_ver, best_url, best_sha


def unpack_tgz_strip_one(tgz_path: str, dst_dir: str):
    if os.path.exists(dst_dir):
        shutil.rmtree(dst_dir)
    os.makedirs(dst_dir, exist_ok=True)
    subprocess.run(
        ["tar", "-zxf", tgz_path, "--directory", dst_dir, "--no-same-owner", "--strip-components", "1"],
        check=True,
    )


def check_patches_clean(chart_name: str, unpacked_dir: str) -> bool:
    patch_dir = os.path.join(repo_root, "hack/deploy/gpustack-operator/chart/charts", chart_name)
    if not os.path.isdir(patch_dir):
        return True
    test_dir = os.path.join(work_tmp, f"patchtest-{chart_name}")
    if os.path.exists(test_dir):
        shutil.rmtree(test_dir)
    shutil.copytree(unpacked_dir, test_dir)
    patches = sorted(p for p in os.listdir(patch_dir) if p.endswith(".patch"))
    for pname in patches:
        pfile = os.path.join(patch_dir, pname)
        with open(pfile, "rb") as pf:
            res = subprocess.run(
                ["patch", "-p1", "-N", "--forward", "--silent", "-F0", "--no-backup-if-mismatch", "--directory", test_dir],
                input=pf.read(),
                capture_output=True,
            )
        if res.returncode != 0:
            return False
        for cur, _, files in os.walk(test_dir):
            for fn in files:
                if fn.endswith(".rej"):
                    return False
    return True


def normalize_tag_v(tag: str) -> str:
    tag = (tag or "").strip()
    if not tag:
        return ""
    return tag if tag.startswith("v") else f"v{tag}"


def split_image_repo_and_tag(ref: str) -> tuple[str, str]:
    base_ref = ref.split("@", 1)[0]
    if ":" in base_ref.split("/")[-1]:
        repo, tag = base_ref.rsplit(":", 1)
        return repo, tag
    return base_ref, ""


def sync_spec_for(upstream_img: str, mirrored_repo_short: str, mirrored_tag: str) -> str:
    # mirrored_repo_short is e.g. "kueue" or "csi-nfs-driver" (without "mirrored-")
    src_base = upstream_img.split("/")[-1]
    dst_base = f"{mirrored_repo_short}:{mirrored_tag}"
    if src_base == dst_base:
        return upstream_img
    return f"{upstream_img}>{dst_base}"


def extract_candidate_requirements(chart_name: str, unpacked_dir: str, cand_ver: str, pinned_ver: str, current_values: dict) -> list[dict]:
    c_chart = load_yaml(os.path.join(unpacked_dir, "Chart.yaml")) or {}
    c_values = load_yaml(os.path.join(unpacked_dir, "values.yaml")) or {}
    app_ver = str(c_chart.get("appVersion") or "").strip()

    reqs = []
    if chart_name == "kueue":
        img = (((c_values.get("controllerManager") or {}).get("manager") or {}).get("image") or {})
        repo = str(img.get("repository") or "registry.k8s.io/kueue/kueue").strip()
        tag = str(img.get("tag") or "").strip() or normalize_tag_v(app_ver or cand_ver)
        m_tag = normalize_tag_v(tag)
        up = f"{repo}:{tag}"
        mir = f"docker.io/gpustack/mirrored-kueue:{m_tag}"
        reqs.append({
            "upstream": up,
            "mirrored": mir,
            "sync_spec": sync_spec_for(up, "kueue", m_tag),
        })
    elif chart_name == "node-feature-discovery":
        img = c_values.get("image") or {}
        repo = str(img.get("repository") or "registry.k8s.io/nfd/node-feature-discovery").strip()
        tag = str(img.get("tag") or "").strip() or normalize_tag_v(app_ver or cand_ver)
        up = f"{repo}:{tag}"
        if cand_ver == pinned_ver:
            cur_tag = str(((current_values.get("node-feature-discovery") or {}).get("image") or {}).get("tag") or "").strip()
            m_tag = cur_tag or normalize_tag_v(tag)
        else:
            m_tag = normalize_tag_v(tag)
        mir = f"docker.io/gpustack/mirrored-node-feature-discovery:{m_tag}"
        reqs.append({
            "upstream": up,
            "mirrored": mir,
            "sync_spec": sync_spec_for(up, "node-feature-discovery", m_tag),
        })
    elif chart_name == "csi-driver-nfs":
        img = c_values.get("image") or {}
        base = str(img.get("baseRepo") or "registry.k8s.io").rstrip("/")
        mapping = [
            ("nfs", "csi-nfs-driver"),
            ("csiProvisioner", "csi-provisioner"),
            ("csiResizer", "csi-resizer"),
            ("csiSnapshotter", "csi-snapshotter"),
            ("livenessProbe", "csi-livenessprobe"),
            ("nodeDriverRegistrar", "csi-node-driver-registrar"),
        ]
        for key, mir_short in mapping:
            entry = img.get(key) or {}
            repo = str(entry.get("repository") or "").strip()
            if repo.startswith("/"):
                repo = base + repo
            tag = str(entry.get("tag") or "").strip() or normalize_tag_v(cand_ver)
            m_tag = normalize_tag_v(tag)
            up = f"{repo}:{tag}"
            mir = f"docker.io/gpustack/mirrored-{mir_short}:{m_tag}"
            reqs.append({
                "upstream": up,
                "mirrored": mir,
                "sync_spec": sync_spec_for(up, mir_short, m_tag),
            })
    elif chart_name == "csi-driver-s3":
        imgs = c_values.get("images") or {}
        s3_full = str(imgs.get("s3") or f"docker.io/thxcode/csi-s3-driver:{cand_ver}").strip()
        _, parsed_s3_tag = split_image_repo_and_tag(s3_full)
        s3_tag = parsed_s3_tag or cand_ver
        m_tag = normalize_tag_v(s3_tag)
        mir = f"docker.io/gpustack/mirrored-csi-s3-driver:{m_tag}"
        reqs.append({
            "upstream": s3_full,
            "mirrored": mir,
            "sync_spec": sync_spec_for(s3_full, "csi-s3-driver", m_tag),
        })
        # Shared sidecars follow the higher of current pinned NFS/S3 sidecars and candidate S3 sidecars.
        cur_s3 = (current_values.get("csi-driver-s3") or {}).get("images") or {}
        for key, mir_short, default_up_repo in [
            ("csiProvisioner", "csi-provisioner", "registry.k8s.io/sig-storage/csi-provisioner"),
            ("livenessProbe", "csi-livenessprobe", "registry.k8s.io/sig-storage/livenessprobe"),
            ("nodeDriverRegistrar", "csi-node-driver-registrar", "registry.k8s.io/sig-storage/csi-node-driver-registrar"),
        ]:
            cur_ref = str(cur_s3.get(key) or "").strip()
            cand_ref = str(imgs.get(key) or "").strip()
            cur_repo, cur_tag = split_image_repo_and_tag(cur_ref)
            cand_repo, cand_tag = split_image_repo_and_tag(cand_ref)
            if cand_tag and (not cur_tag or parse_semver_key(cand_tag) > parse_semver_key(cur_tag)):
                m_sidecar_tag = normalize_tag_v(cand_tag)
                reqs.append({
                    "upstream": cand_ref,
                    "mirrored": f"docker.io/gpustack/mirrored-{mir_short}:{m_sidecar_tag}",
                    "sync_spec": sync_spec_for(cand_ref, mir_short, m_sidecar_tag),
                })
            elif cur_ref:
                if cand_ref and "gpustack/mirrored-" not in cand_ref:
                    up_repo = cand_repo
                elif "gpustack/mirrored-" not in cur_ref:
                    up_repo = cur_repo
                else:
                    up_repo = default_up_repo
                up_ref = f"{up_repo}:{normalize_tag_v(cur_tag)}" if cur_tag else ""
                reqs.append({
                    "upstream": up_ref,
                    "mirrored": cur_ref,
                    "sync_spec": sync_spec_for(up_ref, mir_short, cur_tag) if up_ref else "",
                })
    elif chart_name == "topograph":
        img = c_values.get("image") or {}
        repo = str(img.get("repository") or "ghcr.io/nvidia/topograph").strip()
        tag = str(img.get("tag") or "").strip() or normalize_tag_v(app_ver or cand_ver)
        m_tag = normalize_tag_v(tag)
        up = f"{repo}:{tag}"
        mir = f"docker.io/gpustack/mirrored-nvidia-topograph:{m_tag}"
        reqs.append({
            "upstream": up,
            "mirrored": mir,
            "sync_spec": sync_spec_for(up, "nvidia-topograph", m_tag),
        })

    return reqs


pinned_map = parse_chart_staging(os.path.join(repo_root, "hack/deps.sh"))
current_values = load_yaml(os.path.join(repo_root, "deploy/gpustack-operator/chart/values.yaml")) or {}
current_mirrored = extract_current_mirrored_images(
    os.path.join(repo_root, "deploy/gpustack-operator/chart/values.yaml"),
    os.path.join(repo_root, "deploy/gpustack-operator/chart/charts/topograph/Chart.yaml"),
)

upstream_override = {}
if fixture_dir and os.path.isfile(os.path.join(fixture_dir, "upstream.json")):
    with open(os.path.join(fixture_dir, "upstream.json"), "r", encoding="utf-8") as f:
        upstream_override = (json.load(f) or {}).get("charts") or {}

chart_specs = [
    ("kueue", "kueue-chart", "github-release", "kubernetes-sigs/kueue"),
    ("node-feature-discovery", "nfd-chart", "github-release", "kubernetes-sigs/node-feature-discovery"),
    ("csi-driver-nfs", "csi-driver-nfs-chart", "git-tag", "kubernetes-csi/csi-driver-nfs"),
    ("csi-driver-s3", "csi-driver-s3-chart", "git-tag", "thxcode/k8s-csi-s3"),
    ("topograph", "topograph-chart", "git-tag", "NVIDIA/topograph"),
]

probe_refs = list(current_mirrored)
charts_meta = {}

for name, up_id, kind, source in chart_specs:
    pin = pinned_map[name]
    pinned_ver = pin["version"]
    pinned_url = pin["url"]

    # Cache the pinned archive when available so offline hack/deps.sh always has it.
    _, pinned_sha256 = cache_archive(
        name, pinned_ver, pinned_url, fixture_dir, pin["archive_name"], required=(not fixture_dir)
    )

    expected_sha256 = ""
    if name in upstream_override:
        ov = upstream_override[name]
        cand_ver = str(ov.get("version") or pinned_ver).strip().lstrip("v")
        cand_url = str(ov.get("url") or pinned_url).strip()
        archive_hint = str(ov.get("archive") or "").strip()
        expected_sha256 = str(ov.get("sha256") or ov.get("digest") or "").strip()
    elif fixture_dir:
        cand_ver = pinned_ver
        cand_url = pinned_url
        archive_hint = pin["archive_name"]
    else:
        archive_hint = ""
        if name == "kueue":
            cand_ver, cand_url, expected_sha256 = resolve_github_release_chart(
                "kubernetes-sigs/kueue",
                "kueue",
                "https://github.com/kubernetes-sigs/kueue/releases/download/{tag}/kueue-{ver}.tgz",
                pinned_ver,
                pinned_url,
            )
        elif name == "node-feature-discovery":
            cand_ver, cand_url, expected_sha256 = resolve_github_release_chart(
                "kubernetes-sigs/node-feature-discovery",
                "node-feature-discovery-chart",
                "https://github.com/kubernetes-sigs/node-feature-discovery/releases/download/{tag}/node-feature-discovery-chart-{ver}.tgz",
                pinned_ver,
                pinned_url,
            )
        elif name == "csi-driver-nfs":
            cand_ver, cand_url, expected_sha256 = resolve_helm_index_chart(
                "https://raw.githubusercontent.com/kubernetes-csi/csi-driver-nfs/master/charts/index.yaml",
                "csi-driver-nfs",
                "https://raw.githubusercontent.com/kubernetes-csi/csi-driver-nfs/master/charts/",
                pinned_ver,
                pinned_url,
            )
        elif name == "csi-driver-s3":
            cand_ver, cand_url, expected_sha256 = resolve_helm_index_chart(
                "https://thxcode.github.io/k8s-csi-s3/charts/index.yaml",
                "csi-s3",
                "https://thxcode.github.io/k8s-csi-s3/charts/",
                pinned_ver,
                pinned_url,
            )
        elif name == "topograph":
            cand_ver, cand_url, expected_sha256 = resolve_helm_index_chart(
                "https://dsx-ai-factory.github.io/topograph/index.yaml",
                "topograph",
                "https://dsx-ai-factory.github.io/topograph/",
                pinned_ver,
                pinned_url,
            )

    cand_tgz, cand_sha256 = cache_archive(
        name,
        cand_ver,
        cand_url,
        fixture_dir,
        archive_hint,
        required=True,
        expected_sha256=expected_sha256,
    )
    unpacked_dir = os.path.join(work_tmp, f"unpacked-{name}")
    unpack_tgz_strip_one(cand_tgz, unpacked_dir)

    patch_clean = check_patches_clean(name, unpacked_dir)
    reqs = extract_candidate_requirements(name, unpacked_dir, cand_ver, pinned_ver, current_values)
    for r in reqs:
        if r["mirrored"] not in probe_refs:
            probe_refs.append(r["mirrored"])

    archive_sha256_by_version = {}
    if pinned_sha256:
        archive_sha256_by_version[pinned_ver] = pinned_sha256
    if cand_sha256:
        archive_sha256_by_version[cand_ver] = cand_sha256

    charts_meta[name] = {
        "upstream_id": up_id,
        "kind": kind,
        "source": source,
        "pinned_version": pinned_ver,
        "pinned_url": pinned_url,
        "candidate_version": cand_ver,
        "candidate_url": cand_url,
        "changed": cand_ver != pinned_ver,
        "cache_archive": os.path.relpath(cand_tgz, stage_dir),
        "archiveSha256": cand_sha256,
        "archiveSha256ByVersion": archive_sha256_by_version,
        "patch_clean": patch_clean,
        "required_images": reqs,
    }

with open(probe_list_path, "w", encoding="utf-8") as f:
    for ref in probe_refs:
        f.write(ref + "\n")

with open(discovery_path, "w", encoding="utf-8") as f:
    json.dump({"charts": charts_meta, "current_mirrored_images": current_mirrored}, f, indent=2)
    f.write("\n")
PY

PROBE_ARGS=()
while IFS= read -r ref || [[ -n "${ref}" ]]; do
  [[ -n "${ref}" ]] && PROBE_ARGS+=("${ref}")
done < "${PROBE_LIST_FILE}"

MANIFESTS_JSON="${STAGE_DIR}/image-manifests.json"
autosync::preflight::record_image_probes "${MANIFESTS_JSON}" "${PROBE_ARGS[@]}"

READY_LIST_FILE="${WORK_TMP}/ready-charts.txt"
python3 - "${DISCOVERY_JSON}" "${MANIFESTS_JSON}" "${READY_LIST_FILE}" <<'PY'
import json
import re
import sys

disc_path, manifests_path, ready_out = sys.argv[1:4]
DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")

with open(disc_path, "r", encoding="utf-8") as f:
    disc = json.load(f)
with open(manifests_path, "r", encoding="utf-8") as f:
    manifests = (json.load(f) or {}).get("images") or {}


def is_published(ref: str) -> bool:
    cands = [ref]
    if ref.startswith("docker.io/"):
        cands.append(ref[len("docker.io/"):])
    else:
        cands.append("docker.io/" + ref)
    entry = None
    for k in cands:
        if k in manifests:
            entry = manifests[k]
            break
    if not isinstance(entry, dict):
        return False
    if entry.get("published") is not True:
        return False
    amd = str(entry.get("amd64Digest") or "")
    arm = str(entry.get("arm64Digest") or "")
    return bool(DIGEST_RE.fullmatch(amd) and DIGEST_RE.fullmatch(arm) and amd != arm)


ready_names = []
for name, info in disc["charts"].items():
    missing_specs = []
    missing_images = []
    for req in info["required_images"]:
        mir = req["mirrored"]
        if not is_published(mir):
            missing_images.append(mir)
            spec = req["sync_spec"]
            if spec and spec not in missing_specs:
                missing_specs.append(spec)
    info["mirrored_image_ready"] = len(missing_images) == 0
    info["missing_images"] = missing_images
    info["missing_sync_specs"] = missing_specs
    info["sync_image_command"] = (
        f'gh workflow run sync-image.yml -f images="{" ".join(missing_specs)}"'
        if missing_specs
        else ""
    )
    if info["changed"] and info["mirrored_image_ready"] and info["patch_clean"]:
        ready_names.append(name)

with open(disc_path, "w", encoding="utf-8") as f:
    json.dump(disc, f, indent=2)
    f.write("\n")

with open(ready_out, "w", encoding="utf-8") as f:
    for name in ready_names:
        f.write(name + "\n")
PY

SOURCE_REVISION="${BASE_SHA}"
STAGED_CHARTS=()
while IFS= read -r cname || [[ -n "${cname}" ]]; do
  [[ -n "${cname}" ]] && STAGED_CHARTS+=("${cname}")
done < "${READY_LIST_FILE}"

if (( ${#STAGED_CHARTS[@]} > 0 )) && [[ "${AUTOSYNC_SKIP_SOURCE_REVISION:-false}" != "true" ]]; then
  STAGE_WORK="${WORK_TMP}/staged-repo"
  mkdir -p "${STAGE_WORK}/deploy/gpustack-operator"
  cp -R "${REPO_ROOT}/hack" "${STAGE_WORK}/hack"
  cp -R "${REPO_ROOT}/deploy/gpustack-operator/chart" "${STAGE_WORK}/deploy/gpustack-operator/chart"
  if [[ -f "${REPO_ROOT}/Makefile" ]]; then
    cp -f "${REPO_ROOT}/Makefile" "${STAGE_WORK}/Makefile"
  fi

  python3 - "${DISCOVERY_JSON}" "${READY_LIST_FILE}" \
    "${STAGE_WORK}/hack/deps.sh" \
    "${STAGE_WORK}/deploy/gpustack-operator/chart/Chart.yaml" <<'PY'
import json
import re
import sys

disc_path, ready_path, deps_path, chart_yaml_path = sys.argv[1:5]
with open(disc_path, "r", encoding="utf-8") as f:
    disc = json.load(f)
with open(ready_path, "r", encoding="utf-8") as f:
    ready = {ln.strip() for ln in f if ln.strip()}

with open(deps_path, "r", encoding="utf-8") as f:
    deps_txt = f.read()

m = re.search(r"(function chart_staging\(\)\s*\{.*?cat <<EOF\n)(.*?)(\nEOF)", deps_txt, re.DOTALL)
if not m:
    raise SystemExit("FAIL: missing chart_staging() heredoc")

new_lines = []
for raw in m.group(2).splitlines():
    parts = raw.split()
    if len(parts) == 3:
        url, ver, dest = parts
        name = dest.rsplit("/", 1)[-1]
        if name in ready:
            info = disc["charts"][name]
            url = info["candidate_url"]
            ver = info["candidate_version"]
            new_lines.append(f"{url} {ver} {dest}")
            continue
    new_lines.append(raw)

deps_out = deps_txt[: m.start(2)] + "\n".join(new_lines) + deps_txt[m.end(2) :]
with open(deps_path, "w", encoding="utf-8") as f:
    f.write(deps_out)

with open(chart_yaml_path, "r", encoding="utf-8") as f:
    cy_txt = f.read()

for name in sorted(ready):
    ver = disc["charts"][name]["candidate_version"]
    cy_txt = re.sub(
        rf"(^\s*-\s*name:\s*{re.escape(name)}\n\s*version:\s*)([^\n]+)",
        rf"\g<1>{ver}",
        cy_txt,
        count=1,
        flags=re.MULTILINE,
    )

with open(chart_yaml_path, "w", encoding="utf-8") as f:
    f.write(cy_txt)
PY

  (
    cd "${STAGE_WORK}"
    GPUSTACK_DEPS_STAGE_ONLY=charts \
    GPUSTACK_DEPS_OFFLINE=true \
    GPUSTACK_DEPS_CACHE_DIR="${STAGE_DIR}/deps-cache" \
      bash "${STAGE_WORK}/hack/deps.sh"
  )

  if [[ "${AUTOSYNC_SKIP_GENERATE_CHART:-false}" != "true" ]] && \
     command -v helm-docs >/dev/null 2>&1 && \
     command -v helm-schema >/dev/null 2>&1; then
    (
      cd "${STAGE_WORK}"
      PATH="${STAGE_DIR}/.sbin:${PATH}" bash "${STAGE_WORK}/hack/generate.sh" chart
    )
  fi

  SOURCE_REVISION="$(
    autosync::preflight::commit_staged_tree_revision \
      "${REPO_ROOT}" "${BASE_SHA}" \
      "chore(chart): stage vendored helm subcharts" \
      "${STAGE_WORK}" \
      "hack/deps.sh" \
      "deploy/gpustack-operator/chart"
  )"
fi

REPORT_JSON="${STAGE_DIR}/charts-preflight-report.json"
UPSTREAM_RECORDS_JSON="${STAGE_DIR}/upstream-records.json"

python3 - "${DISCOVERY_JSON}" "${REPORT_JSON}" "${UPSTREAM_RECORDS_JSON}" \
  "${BASE_SHA}" "${SOURCE_REVISION}" "${MANIFESTS_JSON}" <<'PY'
import json
import os
import sys

disc_path, report_path, records_path, base_sha, src_rev, manifests_path = sys.argv[1:7]

with open(disc_path, "r", encoding="utf-8") as f:
    disc = json.load(f)

charts = disc["charts"]
all_missing_specs = []
any_changed = False
ready_count = 0

for name, info in charts.items():
    if info["changed"]:
        any_changed = True
        if info["mirrored_image_ready"] and info["patch_clean"]:
            ready_count += 1
    for spec in info.get("missing_sync_specs") or []:
        if spec not in all_missing_specs:
            all_missing_specs.append(spec)

global_sync_cmd = (
    f'gh workflow run sync-image.yml -f images="{" ".join(all_missing_specs)}"'
    if all_missing_specs
    else ""
)

report = {
    "schema_version": 1,
    "base_sha": base_sha,
    "source_revision": src_rev,
    "changed": any_changed,
    "ready_updates_count": ready_count,
    "image_manifests_path": os.path.basename(manifests_path),
    "sync_image_command": global_sync_cmd,
    "charts": charts,
}

with open(report_path, "w", encoding="utf-8") as f:
    json.dump(report, f, indent=2)
    f.write("\n")

upstream_records = []
for name in ("kueue", "node-feature-discovery", "csi-driver-nfs", "csi-driver-s3", "topograph"):
    info = charts[name]
    if info["changed"]:
        status = "available"
        if info["mirrored_image_ready"]:
            reason = f"{name} {info['candidate_version']} available with mirrored images ready"
        else:
            reason = (
                f"{name} {info['candidate_version']} available but mirrored images are not ready; "
                f"run: {info['sync_image_command']}"
            )
    else:
        status = "up_to_date"
        reason = f"{name} matches pinned version {info['pinned_version']}"
    upstream_records.append({
        "upstreamId": info["upstream_id"],
        "status": status,
        "version": info["candidate_version"],
        "reason": reason,
    })

with open(records_path, "w", encoding="utf-8") as f:
    json.dump(upstream_records, f, indent=2)
    f.write("\n")
PY

COMPACT_RECORDS="$(python3 -c 'import json,sys; print(json.dumps(json.load(open(sys.argv[1])), separators=(",", ":")))' "${UPSTREAM_RECORDS_JSON}")"
autosync::preflight::write_github_output "stage_dir" "${STAGE_DIR}"
autosync::preflight::write_github_output "upstream_records" "${COMPACT_RECORDS}"
autosync::preflight::write_github_output "source_revision" "${SOURCE_REVISION}"
