#!/usr/bin/env bash
# .auto-sync/checks/validate_charts.sh - command.v1 verification gate for charts-sync.
#
# Enforces chart_staging()-only diff guard on hack/deps.sh, proposal and version parity,
# zero .rej/.orig/.tgz artifacts under deploy/gpustack-operator/chart/charts/, cached
# re-staging reproducibility, mirrored image publication verification against
# image-manifests.json, make generate chart, make lint chart, go test ./pkg/worker/kuberess/...,
# and clean git status after regeneration.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
# shellcheck source=./lib.sh
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/lib.sh"

# extract_subchart_mirrored_images <values_yaml> <topograph_chart_yaml>
# Extracts all gpustack/mirrored-* subchart image references across kueue,
# node-feature-discovery, csi-driver-nfs, csi-driver-s3, and topograph in values.yaml,
# asserting every subchart block contributes and printing deduplicated references.
extract_subchart_mirrored_images() {
  local values_yaml="${1:?values.yaml path required}"
  local topo_chart_yaml="${2:?topograph Chart.yaml path required}"

  python3 - "${values_yaml}" "${topo_chart_yaml}" <<'PY'
import json
import os
import subprocess
import sys

try:
    import yaml as yaml_mod
except ImportError:
    yaml_mod = None

values_path, topo_chart_path = sys.argv[1:3]


def load_yaml(path: str):
    if not os.path.isfile(path):
        raise SystemExit(f"FAIL: missing YAML file: {path}")
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


doc = load_yaml(values_path) or {}
topo_chart = load_yaml(topo_chart_path) or {}
topo_app_ver = str(topo_chart.get("appVersion") or "").strip()

per_chart_counts = {
    "kueue": 0,
    "node-feature-discovery": 0,
    "csi-driver-nfs": 0,
    "csi-driver-s3": 0,
    "topograph": 0,
    "modelManager": 0,
}
unique_refs = []


def record(chart_key: str, repo: str, tag: str, base_repo: str = ""):
    repo = (repo or "").strip()
    tag = (tag or "").strip()
    if base_repo and repo.startswith("/"):
        repo = base_repo.rstrip("/") + repo
    if not repo:
        raise SystemExit(f"FAIL: empty image repository in {chart_key} block")
    if "gpustack/mirrored-" not in repo:
        raise SystemExit(f"FAIL: non-mirrored subchart repository in {chart_key}: {repo}")
    if ":" in repo.split("/")[-1]:
        full = repo
    else:
        if not tag:
            raise SystemExit(f"FAIL: empty image tag for {chart_key} ({repo})")
        full = f"{repo}:{tag}"
    per_chart_counts[chart_key] += 1
    if full not in unique_refs:
        unique_refs.append(full)


kueue_img = (((doc.get("kueue") or {}).get("controllerManager") or {}).get("manager") or {}).get("image") or {}
record("kueue", str(kueue_img.get("repository") or ""), str(kueue_img.get("tag") or ""))

nfd_img = (doc.get("node-feature-discovery") or {}).get("image") or {}
record("node-feature-discovery", str(nfd_img.get("repository") or ""), str(nfd_img.get("tag") or ""))

nfs_img = (doc.get("csi-driver-nfs") or {}).get("image") or {}
nfs_base = str(nfs_img.get("baseRepo") or "")
for key in ("nfs", "csiProvisioner", "csiResizer", "csiSnapshotter", "livenessProbe", "nodeDriverRegistrar"):
    item = nfs_img.get(key) or {}
    record("csi-driver-nfs", str(item.get("repository") or ""), str(item.get("tag") or ""), nfs_base)
nfs_reg_tag = str((nfs_img.get("nodeDriverRegistrar") or {}).get("tag") or "").strip()

s3_imgs = (doc.get("csi-driver-s3") or {}).get("images") or {}
s3_reg_tag = ""
for key in ("s3", "csiProvisioner", "livenessProbe", "nodeDriverRegistrar"):
    val = str(s3_imgs.get(key) or "").strip()
    if not val or ":" not in val.split("/")[-1]:
        raise SystemExit(f"FAIL: invalid csi-driver-s3.images.{key} reference: {val!r}")
    if "gpustack/mirrored-" not in val:
        raise SystemExit(f"FAIL: non-mirrored csi-driver-s3.images.{key}: {val}")
    if key == "nodeDriverRegistrar":
        s3_reg_tag = val.rsplit(":", 1)[1].strip()
    per_chart_counts["csi-driver-s3"] += 1
    if val not in unique_refs:
        unique_refs.append(val)

topo_img = (doc.get("topograph") or {}).get("image") or {}
topo_tag = str(topo_img.get("tag") or "").strip() or topo_app_ver
record("topograph", str(topo_img.get("repository") or ""), topo_tag)

mm_reg_img = ((doc.get("modelManager") or {}).get("registrar") or {}).get("image") or {}
mm_reg_repo = str(mm_reg_img.get("repository") or "").strip()
mm_reg_tag = str(mm_reg_img.get("tag") or "").strip()
record("modelManager", mm_reg_repo, mm_reg_tag)
if not mm_reg_tag and ":" in mm_reg_repo.split("/")[-1]:
    mm_reg_tag = mm_reg_repo.rsplit(":", 1)[1].strip()

if not (mm_reg_tag and nfs_reg_tag and s3_reg_tag) or not (mm_reg_tag == nfs_reg_tag == s3_reg_tag):
    raise SystemExit(
        f"FAIL: registrar tag parity mismatch: modelManager.registrar.image.tag={mm_reg_tag!r}, "
        f"csi-driver-nfs.image.nodeDriverRegistrar.tag={nfs_reg_tag!r}, "
        f"csi-driver-s3.images.nodeDriverRegistrar={s3_reg_tag!r}"
    )

total_entries = sum(per_chart_counts.values())
if total_entries < 14:
    raise SystemExit(
        f"FAIL: expected at least 14 mirrored image entries (13 subchart + modelManager.registrar.image), got {total_entries} ({per_chart_counts})"
    )
for chart_key, cnt in per_chart_counts.items():
    if cnt < 1:
        raise SystemExit(f"FAIL: extracted 0 mirrored image references for {chart_key}")

for ref in unique_refs:
    print(ref)
PY
}

if [[ "${1:-}" == "--extract-images" ]]; then
  if (( $# < 3 )); then
    printf 'Usage: %s --extract-images <values.yaml> <topograph-Chart.yaml>\n' "$0" >&2
    exit 2
  fi
  extract_subchart_mirrored_images "$2" "$3"
  exit 0
fi

autosync::bootstrap_env "${AUTOSYNC_STAGE_DIR:-}" "${LOOPWRIGHT_CANDIDATE_DIR:-$(pwd -P)}"
cd "${AUTOSYNC_CANDIDATE_ROOT}"

_CLEANUP_PATHS=()
cleanup_on_exit() {
  local p
  for p in "${_CLEANUP_PATHS[@]+"${_CLEANUP_PATHS[@]}"}"; do
    [[ -n "${p}" && -e "${p}" ]] && rm -rf "${p}"
  done
  autosync::cleanup_ephemeral_git "${AUTOSYNC_CANDIDATE_ROOT}"
}
trap cleanup_on_exit EXIT

autosync::ensure_ephemeral_git "${AUTOSYNC_CANDIDATE_ROOT}"

CAND_DEPS="${AUTOSYNC_CANDIDATE_ROOT}/hack/deps.sh"
CAND_VALUES="${AUTOSYNC_CANDIDATE_ROOT}/deploy/gpustack-operator/chart/values.yaml"
CAND_CHART_YAML="${AUTOSYNC_CANDIDATE_ROOT}/deploy/gpustack-operator/chart/Chart.yaml"
CAND_CHARTS_DIR="${AUTOSYNC_CANDIDATE_ROOT}/deploy/gpustack-operator/chart/charts"
CAND_PROPOSAL="${AUTOSYNC_CANDIDATE_ROOT}/.auto-sync/proposals/charts.json"

# 1. Shared-file diff guards on hack/deps.sh (chart_staging() only) and values.yaml.
if [[ -n "${AUTOSYNC_STAGE_DIR:-}" ]]; then
  BASE_DEPS="${AUTOSYNC_STAGE_DIR}/baseline/hack/deps.sh"
  if [[ ! -f "${BASE_DEPS}" ]]; then
    printf 'FAIL: AUTOSYNC_STAGE_DIR is set (%s) but baseline hack/deps.sh is missing: %s\n' \
      "${AUTOSYNC_STAGE_DIR}" "${BASE_DEPS}" >&2
    exit 1
  fi
  assert_deps_sh_only_section "${BASE_DEPS}" "${CAND_DEPS}" "charts"

  BASE_VALUES="${AUTOSYNC_STAGE_DIR}/baseline/deploy/gpustack-operator/chart/values.yaml"
  if [[ ! -f "${BASE_VALUES}" ]]; then
    printf 'FAIL: AUTOSYNC_STAGE_DIR is set (%s) but baseline values.yaml is missing: %s\n' \
      "${AUTOSYNC_STAGE_DIR}" "${BASE_VALUES}" >&2
    exit 1
  fi
  python3 - "${BASE_VALUES}" "${CAND_VALUES}" <<'PY'
import re
import sys

try:
    import yaml as yaml_mod
except ImportError:
    yaml_mod = None

base_path, cand_path = sys.argv[1:3]
ALLOWED_TOP = {
    "kueue",
    "node-feature-discovery",
    "csi-driver-nfs",
    "csi-driver-s3",
    "topograph",
    "modelManager",
}


def slice_blocks(text: str, indent: int) -> dict[str, list[str]]:
    blocks: dict[str, list[str]] = {}
    cur_key = "__preamble__"
    pending: list[str] = []
    key_re = re.compile(rf"^{' ' * indent}([A-Za-z0-9_-]+):")
    comment_re = re.compile(rf"^{' ' * indent}#")
    for line in text.splitlines(keepends=True):
        if line.strip() == "" or comment_re.match(line):
            pending.append(line)
            continue
        m = key_re.match(line)
        if m:
            key = m.group(1)
            blocks[key] = pending + [line]
            pending = []
            cur_key = key
            continue
        blocks.setdefault(cur_key, []).extend(pending)
        pending = []
        blocks[cur_key].append(line)
    if pending:
        blocks.setdefault(cur_key, []).extend(pending)
    return blocks


with open(base_path, "r", encoding="utf-8") as f:
    base_txt = f.read()
with open(cand_path, "r", encoding="utf-8") as f:
    cand_txt = f.read()

if yaml_mod is not None:
    base_doc = yaml_mod.safe_load(base_txt) or {}
    cand_doc = yaml_mod.safe_load(cand_txt) or {}
    for key in sorted(set(base_doc.keys()) | set(cand_doc.keys())):
        if key not in ALLOWED_TOP and base_doc.get(key) != cand_doc.get(key):
            raise SystemExit(
                f"FAIL: deploy/gpustack-operator/chart/values.yaml modified outside allowed top-level keys: {key}"
            )
    base_mm = dict(base_doc.get("modelManager") or {})
    cand_mm = dict(cand_doc.get("modelManager") or {})
    for subkey in sorted(set(base_mm.keys()) | set(cand_mm.keys())):
        if subkey != "registrar" and base_mm.get(subkey) != cand_mm.get(subkey):
            raise SystemExit(
                f"FAIL: values.yaml modified under modelManager.{subkey} (only registrar.image allowed)"
            )
    base_reg = dict(base_mm.get("registrar") or {})
    cand_reg = dict(cand_mm.get("registrar") or {})
    for subkey in sorted(set(base_reg.keys()) | set(cand_reg.keys())):
        if subkey != "image" and base_reg.get(subkey) != cand_reg.get(subkey):
            raise SystemExit(
                f"FAIL: values.yaml modified under modelManager.registrar.{subkey} (only image allowed)"
            )
    base_img = dict(base_reg.get("image") or {})
    cand_img = dict(cand_reg.get("image") or {})
    for subkey in sorted(set(base_img.keys()) | set(cand_img.keys())):
        if subkey not in ("repository", "tag") and base_img.get(subkey) != cand_img.get(subkey):
            raise SystemExit(
                f"FAIL: values.yaml modified under modelManager.registrar.image.{subkey} (only repository/tag allowed)"
            )

base_blocks = slice_blocks(base_txt, 0)
cand_blocks = slice_blocks(cand_txt, 0)
for key in sorted(set(base_blocks.keys()) | set(cand_blocks.keys())):
    if key not in ALLOWED_TOP and base_blocks.get(key) != cand_blocks.get(key):
        raise SystemExit(
            f"FAIL: deploy/gpustack-operator/chart/values.yaml modified outside allowed top-level keys: {key}"
        )

if base_blocks.get("modelManager") != cand_blocks.get("modelManager"):
    base_mm_sub = slice_blocks("".join(base_blocks.get("modelManager", [])[1:]), 2)
    cand_mm_sub = slice_blocks("".join(cand_blocks.get("modelManager", [])[1:]), 2)
    for subkey in sorted(set(base_mm_sub.keys()) | set(cand_mm_sub.keys())):
        if subkey != "registrar" and base_mm_sub.get(subkey) != cand_mm_sub.get(subkey):
            raise SystemExit(
                f"FAIL: values.yaml modified under modelManager.{subkey} (only registrar.image allowed)"
            )
    base_reg_sub = slice_blocks("".join(base_mm_sub.get("registrar", [])[1:]), 4)
    cand_reg_sub = slice_blocks("".join(cand_mm_sub.get("registrar", [])[1:]), 4)
    for subkey in sorted(set(base_reg_sub.keys()) | set(cand_reg_sub.keys())):
        if subkey != "image" and base_reg_sub.get(subkey) != cand_reg_sub.get(subkey):
            raise SystemExit(
                f"FAIL: values.yaml modified under modelManager.registrar.{subkey} (only image allowed)"
            )
    base_img_sub = slice_blocks("".join(base_reg_sub.get("image", [])[1:]), 6)
    cand_img_sub = slice_blocks("".join(cand_reg_sub.get("image", [])[1:]), 6)
    for subkey in sorted(set(base_img_sub.keys()) | set(cand_img_sub.keys())):
        if subkey not in ("repository", "tag") and base_img_sub.get(subkey) != cand_img_sub.get(subkey):
            raise SystemExit(
                f"FAIL: values.yaml modified under modelManager.registrar.image.{subkey} (only repository/tag allowed)"
            )
PY
fi

# 2. Verify parity across hack/deps.sh, Chart.yaml, _VERSION_ stamps, and proposals/charts.json.
python3 - "${CAND_DEPS}" "${CAND_CHART_YAML}" "${CAND_CHARTS_DIR}" "${CAND_PROPOSAL}" <<'PY'
import json
import os
import re
import subprocess
import sys

try:
    import yaml as yaml_mod
except ImportError:
    yaml_mod = None

deps_path, chart_yaml_path, charts_dir, prop_path = sys.argv[1:5]
expected_charts = ("kueue", "node-feature-discovery", "csi-driver-nfs", "csi-driver-s3", "topograph")

with open(deps_path, "r", encoding="utf-8") as f:
    deps_txt = f.read()
m = re.search(r"function chart_staging\(\)\s*\{.*?cat <<EOF\n(.*?)\nEOF", deps_txt, re.DOTALL)
if not m:
    raise SystemExit("FAIL: missing chart_staging() heredoc in hack/deps.sh")

deps_versions = {}
for line in m.group(1).splitlines():
    line = line.strip()
    if not line:
        continue
    parts = line.split()
    if len(parts) != 3:
        raise SystemExit(f"FAIL: malformed chart_staging() line: {line}")
    _, ver, dest = parts
    deps_versions[os.path.basename(dest)] = ver

for c in expected_charts:
    if c not in deps_versions:
        raise SystemExit(f"FAIL: missing chart {c} in hack/deps.sh chart_staging()")
    vf = os.path.join(charts_dir, c, "_VERSION_")
    if not os.path.isfile(vf):
        raise SystemExit(f"FAIL: missing _VERSION_ file for vendored chart {c}: {vf}")
    with open(vf, "r", encoding="utf-8") as f:
        vval = f.read().strip()
    if vval != deps_versions[c]:
        raise SystemExit(f"FAIL: {c}/_VERSION_ ({vval}) != hack/deps.sh ({deps_versions[c]})")

with open(chart_yaml_path, "r", encoding="utf-8") as f:
    cy_raw = f.read()
if yaml_mod is not None:
    cy = yaml_mod.safe_load(cy_raw) or {}
else:
    res = subprocess.run(
        ["ruby", "-ryaml", "-rjson", "-e", "puts JSON.generate(YAML.safe_load(STDIN.read))"],
        input=cy_raw,
        text=True,
        capture_output=True,
        check=True,
    )
    cy = json.loads(res.stdout)

cy_deps = {str(d.get("name")): str(d.get("version")) for d in (cy.get("dependencies") or []) if isinstance(d, dict)}
for c in expected_charts:
    if cy_deps.get(c) != deps_versions[c]:
        raise SystemExit(f"FAIL: Chart.yaml dependency {c} ({cy_deps.get(c)!r}) != hack/deps.sh ({deps_versions[c]})")

if not os.path.isfile(prop_path):
    raise SystemExit(f"FAIL: missing proposal artifact: {prop_path}")
with open(prop_path, "r", encoding="utf-8") as f:
    prop = json.load(f)
if not isinstance(prop, dict) or prop.get("schema_version") != 1:
    raise SystemExit("FAIL: proposals/charts.json must have schema_version == 1")
if not isinstance(prop.get("summary"), str) or not prop["summary"].strip():
    raise SystemExit("FAIL: proposals/charts.json summary must be a non-empty string")
if not isinstance(prop.get("subscriptions"), list):
    raise SystemExit("FAIL: proposals/charts.json subscriptions must be a JSON array")
p_vers = prop.get("versions")
if not isinstance(p_vers, dict):
    raise SystemExit("FAIL: proposals/charts.json versions must be a JSON object")
chart_to_upstream = {
    "kueue": "kueue-chart",
    "node-feature-discovery": "nfd-chart",
    "csi-driver-nfs": "csi-driver-nfs-chart",
    "csi-driver-s3": "csi-driver-s3-chart",
    "topograph": "topograph-chart",
}
for c in expected_charts:
    up_id = chart_to_upstream[c]
    if p_vers.get(up_id) != deps_versions[c]:
        raise SystemExit(f"FAIL: proposals/charts.json versions.{up_id} ({p_vers.get(up_id)!r}) != hack/deps.sh ({deps_versions[c]})")
PY

# 3. Clean subchart staging & patch reproducibility check.
stray_artifacts="$(find "${CAND_CHARTS_DIR}" -type f \( -name '*.rej' -o -name '*.orig' -o -name '*.tgz' \) | sort)"
if [[ -n "${stray_artifacts}" ]]; then
  printf 'FAIL: unexpected .rej/.orig/.tgz file(s) under deploy/gpustack-operator/chart/charts/:\n%s\n' \
    "${stray_artifacts}" >&2
  exit 1
fi

RESTAGE_LIST_FILE="$(mktemp "${TMPDIR:-/tmp}/autosync-charts-restage.XXXXXXXX")"
_CLEANUP_PATHS+=("${RESTAGE_LIST_FILE}")
python3 - "${AUTOSYNC_STAGE_DIR:-}" "${CAND_DEPS}" "${GPUSTACK_DEPS_CACHE_DIR:-}" "${AUTOSYNC_VERIFY_RESTAGE:-false}" "${AUTOSYNC_SKIP_RESTAGE_CHECK:-false}" "${RESTAGE_LIST_FILE}" <<'PY'
import hashlib
import json
import os
import re
import sys
import urllib.parse

stage_dir, cand_deps, cache_dir, verify_all, skip_restage, out_path = sys.argv[1:7]


def parse_entries(path: str) -> dict[str, tuple[str, str]]:
    with open(path, "r", encoding="utf-8") as f:
        txt = f.read()
    m = re.search(r"function chart_staging\(\)\s*\{.*?cat <<EOF\n(.*?)\nEOF", txt, re.DOTALL)
    out = {}
    if not m:
        return out
    for line in m.group(1).splitlines():
        line = line.strip()
        if not line:
            continue
        url, ver, dest = line.split()
        out[os.path.basename(dest)] = (url, ver)
    return out


cand_entries = parse_entries(cand_deps)
base_entries = {}
if stage_dir and os.path.isfile(os.path.join(stage_dir, "baseline", "hack", "deps.sh")):
    base_entries = parse_entries(os.path.join(stage_dir, "baseline", "hack", "deps.sh"))

expected_hashes_by_ver: dict[str, dict[str, str]] = {}
if stage_dir:
    for meta_name in ("charts-discovery.json", "charts-preflight-report.json"):
        meta_path = os.path.join(stage_dir, meta_name)
        if os.path.isfile(meta_path):
            with open(meta_path, "r", encoding="utf-8") as f:
                meta_doc = json.load(f) or {}
            for cname, cinfo in (meta_doc.get("charts") or {}).items():
                if not isinstance(cinfo, dict):
                    continue
                by_ver: dict[str, str] = {}
                raw_map = cinfo.get("archiveSha256ByVersion")
                if isinstance(raw_map, dict):
                    for k_ver, k_sha in raw_map.items():
                        k_ver_s = str(k_ver or "").strip()
                        k_sha_s = str(k_sha or "").strip().lower()
                        if k_ver_s and k_sha_s:
                            by_ver[k_ver_s] = k_sha_s
                if not by_ver:
                    sha = str(cinfo.get("archiveSha256") or "").strip().lower()
                    cand_v = str(cinfo.get("candidate_version") or "").strip()
                    if sha and cand_v:
                        by_ver[cand_v] = sha
                if by_ver:
                    expected_hashes_by_ver[cname] = by_ver
            break

charts_cache = os.path.join(cache_dir, "charts") if cache_dir else ""
to_restage = []

for name, (url, ver) in cand_entries.items():
    changed = (bool(base_entries) or bool(stage_dir)) and (
        (name not in base_entries) or (base_entries[name][1] != ver) or (base_entries[name][0] != url)
    )
    arc_name = os.path.basename(urllib.parse.urlsplit(url).path)
    cached_tgz_path = ""
    if charts_cache and os.path.isdir(charts_cache):
        for cand_tgz in (f"{name}-{ver}.tgz", arc_name):
            fp = os.path.join(charts_cache, cand_tgz)
            if os.path.isfile(fp):
                cached_tgz_path = fp
                break
    has_cached = bool(cached_tgz_path)
    if has_cached and name in expected_hashes_by_ver:
        ver_hashes = expected_hashes_by_ver[name]
        if ver not in ver_hashes:
            raise SystemExit(
                f"FAIL: staged charts metadata version mismatch for {name}: "
                f"candidate={ver} not in {sorted(ver_hashes)}"
            )
        exp_sha = ver_hashes[ver]
        if not re.fullmatch(r"[0-9a-f]{64}", exp_sha):
            raise SystemExit(f"FAIL: invalid archiveSha256 in staged charts metadata for {name}: {exp_sha!r}")
        with open(cached_tgz_path, "rb") as f:
            actual_sha = hashlib.sha256(f.read()).hexdigest()
        if actual_sha != exp_sha:
            raise SystemExit(
                f"FAIL: cached archive SHA-256 mismatch for {name} {ver} ({cached_tgz_path}): "
                f"expected {exp_sha}, got {actual_sha}"
            )
    if verify_all == "true":
        if changed or not base_entries:
            to_restage.append(name)
    elif changed:
        if has_cached:
            to_restage.append(name)
        elif skip_restage != "true":
            raise SystemExit(
                f"FAIL: chart {name} changed ({ver}) but cached archive is missing in {charts_cache or '<unset>'}; "
                "cannot verify clean offline re-staging"
            )

with open(out_path, "w", encoding="utf-8") as f:
    for name in to_restage:
        f.write(name + "\n")
PY

RESTAGE_CHARTS=()
while IFS= read -r rc_name || [[ -n "${rc_name}" ]]; do
  [[ -n "${rc_name}" ]] && RESTAGE_CHARTS+=("${rc_name}")
done < "${RESTAGE_LIST_FILE}"
rm -f "${RESTAGE_LIST_FILE}"

if (( ${#RESTAGE_CHARTS[@]} > 0 )); then
  TMP_RESTAGE="$(mktemp -d "${TMPDIR:-/tmp}/autosync-charts-restage.XXXXXXXX")"
  _CLEANUP_PATHS+=("${TMP_RESTAGE}")
  mkdir -p "${TMP_RESTAGE}/deploy/gpustack-operator/chart"
  cp -R "${AUTOSYNC_CANDIDATE_ROOT}/hack" "${TMP_RESTAGE}/hack"
  cp -R "${CAND_CHARTS_DIR}" "${TMP_RESTAGE}/deploy/gpustack-operator/chart/charts"

  for rc_name in "${RESTAGE_CHARTS[@]}"; do
    rm -f "${TMP_RESTAGE}/deploy/gpustack-operator/chart/charts/${rc_name}/_VERSION_"
  done

  GPUSTACK_DEPS_STAGE_ONLY=charts \
  GPUSTACK_DEPS_OFFLINE=true \
  GPUSTACK_DEPS_CACHE_DIR="${GPUSTACK_DEPS_CACHE_DIR:-}" \
    bash "${TMP_RESTAGE}/hack/deps.sh"

  restage_stray="$(find "${TMP_RESTAGE}/deploy/gpustack-operator/chart/charts" -type f \( -name '*.rej' -o -name '*.orig' -o -name '*.tgz' \) | sort)"
  if [[ -n "${restage_stray}" ]]; then
    printf 'FAIL: re-staging charts produced .rej/.orig/.tgz artifacts:\n%s\n' "${restage_stray}" >&2
    exit 1
  fi

  diff_rc=0
  diff -rq -- "${TMP_RESTAGE}/deploy/gpustack-operator/chart/charts" "${CAND_CHARTS_DIR}" || diff_rc=$?
  if (( diff_rc != 0 )); then
    printf 'FAIL: deploy/gpustack-operator/chart/charts/ differs from clean offline hack/deps.sh re-staging\n' >&2
    exit 1
  fi
fi

# 4. Subchart gpustack/mirrored-* image extraction and manifest verification.
extracted_images_raw="$(extract_subchart_mirrored_images "${CAND_VALUES}" "${CAND_CHARTS_DIR}/topograph/Chart.yaml")"
SUBCHART_IMAGES=()
while IFS= read -r img_ref || [[ -n "${img_ref}" ]]; do
  [[ -n "${img_ref}" ]] && SUBCHART_IMAGES+=("${img_ref}")
done <<<"${extracted_images_raw}"

if [[ -z "${AUTOSYNC_STAGE_DIR:-}" ]]; then
  if [[ "${AUTOSYNC_SKIP_IMAGE_CHECK:-false}" != "true" ]]; then
    printf 'FAIL: AUTOSYNC_STAGE_DIR must be set for image publication verification (set AUTOSYNC_SKIP_IMAGE_CHECK=true only for local manual runs)\n' >&2
    exit 1
  fi
else
  MANIFESTS_JSON="${AUTOSYNC_STAGE_DIR}/image-manifests.json"
  if [[ ! -f "${MANIFESTS_JSON}" ]]; then
    printf 'FAIL: AUTOSYNC_STAGE_DIR is set (%s) but image-manifests.json is missing\n' \
      "${AUTOSYNC_STAGE_DIR}" >&2
    exit 1
  fi
  for img_ref in "${SUBCHART_IMAGES[@]+"${SUBCHART_IMAGES[@]}"}"; do
    autosync::assert_image_published "${MANIFESTS_JSON}" "${img_ref}"
  done
fi

if [[ "${AUTOSYNC_FAST_SELFTEST:-false}" == "true" ]]; then
  exit 0
fi

# 5. Chart regeneration, chart lint, kuberess tests, and clean working tree check.
make -C "${AUTOSYNC_CANDIDATE_ROOT}" generate chart
make -C "${AUTOSYNC_CANDIDATE_ROOT}" lint chart
(
  cd "${AUTOSYNC_CANDIDATE_ROOT}"
  go test ./pkg/worker/kuberess/...
)

if [[ -f "${AUTOSYNC_CANDIDATE_ROOT}/hack/check/symbols.sh" ]]; then
  bash "${AUTOSYNC_CANDIDATE_ROOT}/hack/check/symbols.sh" "${AUTOSYNC_CANDIDATE_ROOT}" >/dev/null
fi

drift="$(git -C "${AUTOSYNC_CANDIDATE_ROOT}" status --porcelain)"
if [[ -n "${drift}" ]]; then
  printf 'FAIL: working tree has unstaged drift after make generate chart:\n%s\n' "${drift}" >&2
  exit 1
fi
