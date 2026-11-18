#!/usr/bin/env bash
# .auto-sync/checks/selftest_charts.sh - offline self-tests for charts-sync.
#
# Usage:
#   bash .auto-sync/checks/selftest_charts.sh [repo-root]
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
DEFAULT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd -P)"
ROOT_DIR="${1:-${DEFAULT_ROOT}}"
ROOT_DIR="$(cd "${ROOT_DIR}" && pwd -P)"

FAILS=0
PASSES=0
SCRATCH=""

cleanup() {
  if [[ -n "${SCRATCH}" && -d "${SCRATCH}" ]]; then
    rm -rf "${SCRATCH}"
  fi
}
trap cleanup EXIT

pass() {
  printf 'PASS  [charts] %s\n' "$1"
  PASSES=$((PASSES + 1))
}

fail() {
  printf 'FAIL  [charts] %s\n' "$1" >&2
  FAILS=$((FAILS + 1))
}

SCRATCH="$(cd "$(mktemp -d)" && pwd -P)"

TASKPACK_FILE="${ROOT_DIR}/.auto-sync/charts-taskpack.yaml"
INSTRUCTIONS_FILE="${ROOT_DIR}/.auto-sync/instructions/charts.md"
PREFLIGHT_SCRIPT="${ROOT_DIR}/.auto-sync/scripts/preflight-charts.sh"
VALIDATE_SCRIPT="${ROOT_DIR}/.auto-sync/checks/validate_charts.sh"
PROPOSAL_FILE="${ROOT_DIR}/.auto-sync/proposals/charts.json"

for req in "${TASKPACK_FILE}" "${INSTRUCTIONS_FILE}" "${PREFLIGHT_SCRIPT}" "${VALIDATE_SCRIPT}" "${PROPOSAL_FILE}"; do
  if [[ ! -f "${req}" ]]; then
    fail "missing required charts-sync file: ${req#"${ROOT_DIR}/"}"
  fi
done

if (( FAILS > 0 )); then
  printf 'CHARTS SELFTEST FAILED (RED): %d missing deliverable(s)\n' "${FAILS}" >&2
  exit 1
fi

# 1. Extraction check against the real deploy/gpustack-operator/chart/values.yaml.
REAL_VALUES="${ROOT_DIR}/deploy/gpustack-operator/chart/values.yaml"
REAL_TOPO_CHART="${ROOT_DIR}/deploy/gpustack-operator/chart/charts/topograph/Chart.yaml"

extracted_out=""
if extracted_out="$("${VALIDATE_SCRIPT}" --extract-images "${REAL_VALUES}" "${REAL_TOPO_CHART}")" && \
   python3 - "${extracted_out}" <<'PY'
import sys

lines = [ln.strip() for ln in sys.argv[1].splitlines() if ln.strip()]
got = set(lines)
required_repos = {
    "docker.io/gpustack/mirrored-kueue",
    "docker.io/gpustack/mirrored-node-feature-discovery",
    "docker.io/gpustack/mirrored-csi-nfs-driver",
    "docker.io/gpustack/mirrored-csi-provisioner",
    "docker.io/gpustack/mirrored-csi-resizer",
    "docker.io/gpustack/mirrored-csi-snapshotter",
    "docker.io/gpustack/mirrored-csi-livenessprobe",
    "docker.io/gpustack/mirrored-csi-node-driver-registrar",
    "docker.io/gpustack/mirrored-csi-s3-driver",
    "docker.io/gpustack/mirrored-nvidia-topograph",
}
got_repos = {ref.rsplit(":", 1)[0] for ref in got if ":" in ref.split("/")[-1]}
missing = sorted(required_repos - got_repos)
extra = sorted(got_repos - required_repos)
assert not missing and not extra, f"repo mismatch: missing={missing} extra={extra} got={sorted(got)}"
assert len(got) >= len(required_repos), f"expected >= {len(required_repos)} refs, got {sorted(got)}"
PY
then
  pass "validate_charts.sh extracts all mirrored subchart images from real values.yaml"
else
  fail "validate_charts.sh failed to extract expected mirrored subchart images from real values.yaml"
fi

# 2. Build offline fixture tarballs (reverse-patched from real subcharts) and a scratch repo.
FIXTURE_ARCHIVES="${SCRATCH}/fixture-archives"
mkdir -p "${FIXTURE_ARCHIVES}"

CHART_META_JSON="${SCRATCH}/chart-meta.json"
python3 - "${ROOT_DIR}" "${FIXTURE_ARCHIVES}" "${CHART_META_JSON}" <<'PY'
import glob
import json
import os
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import urllib.parse

root_dir, out_dir, meta_out = sys.argv[1:4]
deps_path = os.path.join(root_dir, "hack/deps.sh")
with open(deps_path, "r", encoding="utf-8") as f:
    deps_txt = f.read()
m = re.search(r"function chart_staging\(\)\s*\{.*?cat <<EOF\n(.*?)\nEOF", deps_txt, re.DOTALL)
assert m, "missing chart_staging() heredoc in hack/deps.sh"

charts = []
for raw in m.group(1).splitlines():
    raw = raw.strip()
    if not raw:
        continue
    url, ver, dest = raw.split()
    name = os.path.basename(dest)
    arc_name = os.path.basename(urllib.parse.urlsplit(url).path)
    charts.append((name, ver, arc_name, url))

kueue_cur = next(ver for n, ver, _, _ in charts if n == "kueue")
km = re.fullmatch(r"(\d+)\.(\d+)\.(\d+)", kueue_cur)
assert km, f"unexpected kueue version format: {kueue_cur}"
kueue_next = f"{km.group(1)}.{km.group(2)}.{int(km.group(3)) + 1}"

k8s_mod_m = re.search(r"https://github\.com/kubernetes/api\s+(v0\.\d+\.\d+)\s+", deps_txt)
if k8s_mod_m is None:
    raise SystemExit("FAIL: could not extract kubernetes/api version from hack/deps.sh")
k8s_mod_ver = k8s_mod_m.group(1)

with tempfile.TemporaryDirectory() as tmp:
    for name, ver, archive_name, _ in charts:
        src = os.path.join(root_dir, "deploy/gpustack-operator/chart/charts", name)
        unpatched = os.path.join(tmp, name)
        shutil.copytree(src, unpatched)
        patches = sorted(glob.glob(os.path.join(root_dir, "hack/deploy/gpustack-operator/chart/charts", name, "*.patch")))
        for p in reversed(patches):
            with open(p, "rb") as pf:
                res = subprocess.run(
                    ["patch", "-R", "-p1", "--batch", "--silent", "-F0", "--no-backup-if-mismatch", "--directory", unpatched],
                    input=pf.read(),
                    capture_output=True,
                )
            if res.returncode != 0:
                raise SystemExit(f"reverse patch failed for {name}/{os.path.basename(p)}: {res.stderr.decode()}")
        ver_file = os.path.join(unpatched, "_VERSION_")
        if os.path.exists(ver_file):
            os.unlink(ver_file)
        tgz_path = os.path.join(out_dir, archive_name)
        with tarfile.open(tgz_path, "w:gz") as tf:
            tf.add(unpatched, arcname=name)
        alt_path = os.path.join(out_dir, f"{name}-{ver}.tgz")
        if os.path.abspath(tgz_path) != os.path.abspath(alt_path):
            shutil.copyfile(tgz_path, alt_path)

        if name == "kueue":
            bumped = os.path.join(tmp, "kueue-bumped")
            shutil.copytree(unpatched, bumped)
            chart_yaml = os.path.join(bumped, "Chart.yaml")
            with open(chart_yaml, "r", encoding="utf-8") as f:
                cy = f.read()
            cy_next = cy.replace(f"version: {kueue_cur}", f"version: {kueue_next}")
            cy_next = cy_next.replace(f"appVersion: v{kueue_cur}", f"appVersion: v{kueue_next}")
            assert cy_next != cy, f"failed to bump kueue Chart.yaml from {kueue_cur} to {kueue_next}"
            with open(chart_yaml, "w", encoding="utf-8") as f:
                f.write(cy_next)
            values_yaml = os.path.join(bumped, "values.yaml")
            with open(values_yaml, "r", encoding="utf-8") as f:
                vy = f.read()
            vy_next = re.sub(rf"tag:\s*v{re.escape(kueue_cur)}\b", f"tag: v{kueue_next}", vy, count=1)
            assert vy_next != vy, f"failed to bump kueue values.yaml tag from v{kueue_cur}"
            with open(values_yaml, "w", encoding="utf-8") as f:
                f.write(vy_next)
            bumped_tgz = os.path.join(out_dir, f"kueue-{kueue_next}.tgz")
            with tarfile.open(bumped_tgz, "w:gz") as tf:
                tf.add(bumped, arcname="kueue")

with open(meta_out, "w", encoding="utf-8") as f:
    json.dump({
        "charts": {n: {"version": v, "archive": a, "url": u} for n, v, a, u in charts},
        "kueue_cur": kueue_cur,
        "kueue_next": kueue_next,
        "k8s_mod_ver": k8s_mod_ver,
    }, f, indent=2)
PY

KUEUE_CUR="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["kueue_cur"])' "${CHART_META_JSON}")"
KUEUE_NEXT="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["kueue_next"])' "${CHART_META_JSON}")"
K8S_MOD_VER="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["k8s_mod_ver"])' "${CHART_META_JSON}")"

PREFLIGHT_REPO="${SCRATCH}/preflight-repo"
mkdir -p \
  "${PREFLIGHT_REPO}/.auto-sync/scripts" \
  "${PREFLIGHT_REPO}/.auto-sync/checks" \
  "${PREFLIGHT_REPO}/.auto-sync/proposals" \
  "${PREFLIGHT_REPO}/.auto-sync/instructions" \
  "${PREFLIGHT_REPO}/hack" \
  "${PREFLIGHT_REPO}/deploy/gpustack-operator"

cp "${ROOT_DIR}/.auto-sync/scripts/lib-preflight.sh" "${PREFLIGHT_REPO}/.auto-sync/scripts/lib-preflight.sh"
cp "${PREFLIGHT_SCRIPT}" "${PREFLIGHT_REPO}/.auto-sync/scripts/preflight-charts.sh"
chmod +x "${PREFLIGHT_REPO}/.auto-sync/scripts/preflight-charts.sh"
cp "${ROOT_DIR}/.auto-sync/checks/lib.sh" "${PREFLIGHT_REPO}/.auto-sync/checks/lib.sh"
cp "${VALIDATE_SCRIPT}" "${PREFLIGHT_REPO}/.auto-sync/checks/validate_charts.sh"
chmod +x "${PREFLIGHT_REPO}/.auto-sync/checks/validate_charts.sh"
cp "${TASKPACK_FILE}" "${PREFLIGHT_REPO}/.auto-sync/charts-taskpack.yaml"
cp "${INSTRUCTIONS_FILE}" "${PREFLIGHT_REPO}/.auto-sync/instructions/charts.md"
cp "${PROPOSAL_FILE}" "${PREFLIGHT_REPO}/.auto-sync/proposals/charts.json"
cp "${ROOT_DIR}/hack/deps.sh" "${PREFLIGHT_REPO}/hack/deps.sh"
cp -R "${ROOT_DIR}/hack/lib" "${PREFLIGHT_REPO}/hack/lib"
cp -R "${ROOT_DIR}/hack/deploy" "${PREFLIGHT_REPO}/hack/deploy"
cp -R "${ROOT_DIR}/deploy/gpustack-operator/chart" "${PREFLIGHT_REPO}/deploy/gpustack-operator/chart"

git -C "${PREFLIGHT_REPO}" init -q -b main
git -C "${PREFLIGHT_REPO}" config user.name "charts-selftest"
git -C "${PREFLIGHT_REPO}" config user.email "charts-selftest@gpustack.local"
git -C "${PREFLIGHT_REPO}" config commit.gpgsign false
git -C "${PREFLIGHT_REPO}" add -A
git -C "${PREFLIGHT_REPO}" commit -q -m "baseline"
BASE_SHA="$(git -C "${PREFLIGHT_REPO}" rev-parse HEAD)"

# 3. Offline preflight fixture tests (mirrored_image_ready: false vs true).
FIXTURE_DIR="${SCRATCH}/fixture-charts"
mkdir -p "${FIXTURE_DIR}/archives"
cp "${FIXTURE_ARCHIVES}/"*.tgz "${FIXTURE_DIR}/archives/"

MOCK_NOT_READY="${SCRATCH}/mock-not-ready.json"
MOCK_READY="${SCRATCH}/mock-ready.json"
python3 - "${CHART_META_JSON}" "${FIXTURE_DIR}/upstream.json" "${extracted_out}" "${MOCK_NOT_READY}" "${MOCK_READY}" <<'PY'
import json
import sys

meta_path, upstream_out, extracted_raw, not_ready_path, ready_path = sys.argv[1:6]
with open(meta_path, "r", encoding="utf-8") as f:
    meta = json.load(f)

charts = {}
for cname, cinfo in meta["charts"].items():
    if cname == "kueue":
        ver = meta["kueue_next"]
        charts[cname] = {
            "version": ver,
            "url": f"https://github.com/kubernetes-sigs/kueue/releases/download/v{ver}/kueue-{ver}.tgz",
            "archive": f"kueue-{ver}.tgz",
        }
    else:
        charts[cname] = {
            "version": cinfo["version"],
            "url": cinfo["url"],
            "archive": f"{cname}-{cinfo['version']}.tgz",
        }

with open(upstream_out, "w", encoding="utf-8") as f:
    json.dump({"charts": charts}, f, indent=2)

base_images = [ln.strip() for ln in extracted_raw.splitlines() if ln.strip()]
kueue_next = meta["kueue_next"]


def build_doc(kueue_ready: bool):
    images = {}
    for idx, ref in enumerate(base_images, start=1):
        h1 = f"{idx:02x}" * 32
        h2 = f"{idx + 64:02x}" * 32
        images[ref] = {
            "published": True,
            "amd64Digest": f"sha256:{h1}",
            "arm64Digest": f"sha256:{h2}",
        }
    target_kueue = f"docker.io/gpustack/mirrored-kueue:v{kueue_next}"
    if kueue_ready:
        images[target_kueue] = {
            "published": True,
            "amd64Digest": "sha256:" + ("aa" * 32),
            "arm64Digest": "sha256:" + ("bb" * 32),
        }
    else:
        images[target_kueue] = {
            "published": False,
            "amd64Digest": "",
            "arm64Digest": "",
        }
    return {"images": images}


with open(not_ready_path, "w", encoding="utf-8") as f:
    json.dump(build_doc(False), f, indent=2)
with open(ready_path, "w", encoding="utf-8") as f:
    json.dump(build_doc(True), f, indent=2)
PY

STAGE_UNREADY="${SCRATCH}/stage-unready/charts"
GHA_UNREADY="${SCRATCH}/gha-unready.txt"
SUMMARY_UNREADY="${SCRATCH}/summary-unready.md"
: > "${GHA_UNREADY}"
: > "${SUMMARY_UNREADY}"
if (
  cd "${PREFLIGHT_REPO}" && \
  AUTOSYNC_CHARTS_FIXTURE_DIR="${FIXTURE_DIR}" \
  AUTOSYNC_MOCK_MANIFESTS_JSON="${MOCK_NOT_READY}" \
  AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
  GITHUB_OUTPUT="${GHA_UNREADY}" \
  GITHUB_STEP_SUMMARY="${SUMMARY_UNREADY}" \
  bash "${PREFLIGHT_REPO}/.auto-sync/scripts/preflight-charts.sh" "${STAGE_UNREADY}"
) >/dev/null && \
python3 - "${STAGE_UNREADY}" "${GHA_UNREADY}" "${SUMMARY_UNREADY}" "${BASE_SHA}" "${KUEUE_CUR}" "${KUEUE_NEXT}" <<'PY'
import json
import os
import re
import sys

stage_dir, gha_path, summary_path, base_sha, kueue_cur, kueue_next = sys.argv[1:7]
with open(os.path.join(stage_dir, "charts-preflight-report.json"), "r", encoding="utf-8") as f:
    report = json.load(f)
assert report["source_revision"] == base_sha, report
assert report["changed"] is True, report
assert report["ready_updates_count"] == 0, report
kueue = report["charts"]["kueue"]
assert kueue["changed"] is True, kueue
assert kueue["candidate_version"] == kueue_next, kueue
assert kueue["mirrored_image_ready"] is False, kueue
assert re.fullmatch(r"[0-9a-f]{64}", kueue.get("archiveSha256", "")), kueue
sync_cmd = kueue.get("sync_image_command") or report.get("sync_image_command", "")
assert "gh workflow run sync-image.yml -f images=" in sync_cmd, report
assert f"registry.k8s.io/kueue/kueue:v{kueue_next}" in sync_cmd, sync_cmd

with open(os.path.join(stage_dir, "upstream-records.json"), "r", encoding="utf-8") as f:
    recs = json.load(f)
by_id = {r["upstreamId"]: r for r in recs}
assert by_id["kueue-chart"]["status"] == "up_to_date", by_id
assert by_id["kueue-chart"]["version"] == kueue_cur, by_id
assert f"awaits mirrored images; run: {sync_cmd}" in by_id["kueue-chart"]["reason"], by_id
assert by_id["nfd-chart"]["status"] == "up_to_date", by_id

with open(gha_path, "r", encoding="utf-8") as f:
    gha_txt = f.read()
assert f"\n{base_sha}\n" in gha_txt, gha_txt

with open(summary_path, "r", encoding="utf-8") as f:
    summary_txt = f.read()
assert "Pending Mirrored Images" in summary_txt, summary_txt
assert f"`kueue` | `{kueue_cur}` | `{kueue_next}`" in summary_txt, summary_txt
assert sync_cmd in summary_txt, summary_txt
PY
then
  if [[ -z "$(git -C "${PREFLIGHT_REPO}" status --porcelain)" ]] && [[ "$(git -C "${PREFLIGHT_REPO}" rev-parse HEAD)" == "${BASE_SHA}" ]]; then
    pass "preflight-charts.sh holds unmirrored kueue at pinned up_to_date, records sync_image_command, and writes step summary"
  else
    fail "preflight-charts.sh unready case dirtied git tree"
  fi
else
  fail "preflight-charts.sh unready fixture test"
fi

STAGE_READY="${SCRATCH}/stage-ready/charts"
GHA_READY="${SCRATCH}/gha-ready.txt"
: > "${GHA_READY}"
SRC_REV=""
if (
  cd "${PREFLIGHT_REPO}" && \
  AUTOSYNC_CHARTS_FIXTURE_DIR="${FIXTURE_DIR}" \
  AUTOSYNC_MOCK_MANIFESTS_JSON="${MOCK_READY}" \
  AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
  GITHUB_OUTPUT="${GHA_READY}" \
  bash "${PREFLIGHT_REPO}/.auto-sync/scripts/preflight-charts.sh" "${STAGE_READY}"
) >/dev/null && \
SRC_REV="$(python3 - "${STAGE_READY}" "${BASE_SHA}" "${KUEUE_NEXT}" <<'PY'
import hashlib
import json
import os
import sys

stage_dir, base_sha, kueue_next = sys.argv[1:4]
with open(os.path.join(stage_dir, "charts-preflight-report.json"), "r", encoding="utf-8") as f:
    report = json.load(f)
src_rev = report["source_revision"]
assert src_rev and src_rev != base_sha, report
assert report["ready_updates_count"] == 1, report
kueue = report["charts"]["kueue"]
assert kueue["changed"] is True, kueue
assert kueue["mirrored_image_ready"] is True, kueue
assert kueue["patch_clean"] is True, kueue
with open(os.path.join(stage_dir, "upstream-records.json"), "r", encoding="utf-8") as f:
    recs = json.load(f)
by_id = {r["upstreamId"]: r for r in recs}
assert by_id["kueue-chart"]["status"] == "available", by_id
assert by_id["kueue-chart"]["version"] == kueue_next, by_id
tgz_path = os.path.join(stage_dir, "deps-cache", "charts", f"kueue-{kueue_next}.tgz")
assert os.path.isfile(tgz_path)
with open(tgz_path, "rb") as f:
    expected_sha = hashlib.sha256(f.read()).hexdigest()
assert kueue.get("archiveSha256") == expected_sha, kueue
with open(os.path.join(stage_dir, "charts-discovery.json"), "r", encoding="utf-8") as f:
    disc = json.load(f)
assert disc["charts"]["kueue"]["archiveSha256"] == expected_sha, disc
s3_reqs = disc["charts"]["csi-driver-s3"]["required_images"]
assert len(s3_reqs) == 4, s3_reqs
for req in s3_reqs:
    assert req["upstream"] and "gpustack/mirrored-" not in req["upstream"], req
    assert req["sync_spec"], req
print(src_rev)
PY
)" && \
[[ -n "${SRC_REV}" && "${SRC_REV}" != "${BASE_SHA}" ]] && \
[[ "$(git -C "${PREFLIGHT_REPO}" show "${SRC_REV}:deploy/gpustack-operator/chart/charts/kueue/_VERSION_")" == "${KUEUE_NEXT}" ]] && \
grep -q "kueue-${KUEUE_NEXT}.tgz ${KUEUE_NEXT}" <<<"$(git -C "${PREFLIGHT_REPO}" show "${SRC_REV}:hack/deps.sh")" && \
[[ -z "$(git -C "${PREFLIGHT_REPO}" status --porcelain)" ]] && \
[[ "$(git -C "${PREFLIGHT_REPO}" rev-parse HEAD)" == "${BASE_SHA}" ]]; then
  pass "preflight-charts.sh stages mirrored-ready kueue bump onto clean detached source_revision"
else
  fail "preflight-charts.sh ready fixture test"
fi

# 3b. Uncommitted local edit in repo_root survives preflight-charts.sh untouched.
STAGE_DIRTY="${SCRATCH}/stage-dirty/charts"
printf '\n# local uncommitted edit\n' >> "${PREFLIGHT_REPO}/hack/deps.sh"
if (
  cd "${PREFLIGHT_REPO}" && \
  AUTOSYNC_CHARTS_FIXTURE_DIR="${FIXTURE_DIR}" \
  AUTOSYNC_MOCK_MANIFESTS_JSON="${MOCK_READY}" \
  AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
  bash "${PREFLIGHT_REPO}/.auto-sync/scripts/preflight-charts.sh" "${STAGE_DIRTY}"
) >/dev/null && \
grep -q '# local uncommitted edit' "${PREFLIGHT_REPO}/hack/deps.sh"; then
  pass "preflight-charts.sh preserves uncommitted local edits in repo_root"
else
  fail "preflight-charts.sh preserves uncommitted local edits in repo_root"
fi
git -C "${PREFLIGHT_REPO}" checkout -- hack/deps.sh

# 3c. Upstream archive SHA-256 mismatch fails closed in preflight-charts.sh.
FIXTURE_BAD_SHA="${SCRATCH}/fixture-bad-sha"
cp -R "${FIXTURE_DIR}" "${FIXTURE_BAD_SHA}"
python3 - "${FIXTURE_BAD_SHA}/upstream.json" <<'PY'
import json
import sys
p = sys.argv[1]
with open(p, "r", encoding="utf-8") as f:
    doc = json.load(f)
doc["charts"]["kueue"]["sha256"] = "0" * 64
with open(p, "w", encoding="utf-8") as f:
    json.dump(doc, f, indent=2)
PY
if ! (
  cd "${PREFLIGHT_REPO}" && \
  AUTOSYNC_CHARTS_FIXTURE_DIR="${FIXTURE_BAD_SHA}" \
  AUTOSYNC_MOCK_MANIFESTS_JSON="${MOCK_READY}" \
  AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
  bash "${PREFLIGHT_REPO}/.auto-sync/scripts/preflight-charts.sh" "${SCRATCH}/stage-bad-sha/charts"
) >/dev/null 2>&1; then
  pass "preflight-charts.sh fails closed on archive SHA-256 mismatch"
else
  fail "preflight-charts.sh fails closed on archive SHA-256 mismatch"
fi

# 4. Gitless env -i validate_charts.sh tests (positive + negative cases).
if [[ -z "${SRC_REV}" || "${SRC_REV}" == "${BASE_SHA}" ]]; then
  fail "skipping validate_charts.sh tests because SRC_REV is empty or unchanged"
else
CAND_BASE="${SCRATCH}/cand-base"
mkdir -p "${CAND_BASE}"
git -C "${PREFLIGHT_REPO}" archive "${SRC_REV}" | tar -x -C "${CAND_BASE}"

# Align values.yaml and proposals/charts.json with the bumped kueue in the candidate tree.
python3 - "${CAND_BASE}/deploy/gpustack-operator/chart/values.yaml" "${CAND_BASE}/.auto-sync/proposals/charts.json" "${KUEUE_CUR}" "${KUEUE_NEXT}" <<'PY'
import json
import re
import sys

vy_path, prop_path, kueue_cur, kueue_next = sys.argv[1:5]
with open(vy_path, "r", encoding="utf-8") as f:
    vy = f.read()
vy_new = vy.replace(f"chart {kueue_cur}", f"chart {kueue_next}")
vy_new = re.sub(
    rf"(repository:\s*docker\.io/gpustack/mirrored-kueue\n(?:[^\n]*\n){{0,4}}?\s*tag:\s*)v{re.escape(kueue_cur)}\b",
    rf"\g<1>v{kueue_next}",
    vy_new,
    count=1,
)
assert vy_new != vy, "failed to update kueue tag in values.yaml"
with open(vy_path, "w", encoding="utf-8") as f:
    f.write(vy_new)

with open(prop_path, "r", encoding="utf-8") as f:
    prop = json.load(f)
prop["versions"]["kueue-chart"] = kueue_next
for sub in prop.get("subscriptions", []):
    if isinstance(sub, dict) and sub.get("id") == "kueue-chart":
        sub["version"] = kueue_next
with open(prop_path, "w", encoding="utf-8") as f:
    json.dump(prop, f, indent=2)
    f.write("\n")
PY

HOST_GOMODCACHE="$(go env GOMODCACHE)"
HOST_GOCACHE="$(go env GOCACHE)"

run_validator_env_i() {
  local cand_dir="$1"
  local stage_dir="$2"
  local home_dir="${SCRATCH}/env-home"
  mkdir -p "${home_dir}"
  env -i \
    PATH=/usr/local/bin:/usr/bin:/bin \
    HOME="${home_dir}" \
    TMPDIR="${SCRATCH}" \
    GOMODCACHE="${HOST_GOMODCACHE}" \
    GOCACHE="${HOST_GOCACHE}" \
    AUTOSYNC_STAGE_DIR="${stage_dir}" \
    LOOPWRIGHT_CANDIDATE_DIR="${cand_dir}" \
    AUTOSYNC_FAST_SELFTEST=true \
    bash "${cand_dir}/.auto-sync/checks/validate_charts.sh"
}

# Positive case: valid chart_staging() bump + cached .tgz + clean patch + all mirrored images published.
CAND_POS="${SCRATCH}/cand-pos"
cp -R "${CAND_BASE}" "${CAND_POS}"
if run_validator_env_i "${CAND_POS}" "${STAGE_READY}" >/dev/null 2>&1 && \
   [[ ! -e "${CAND_POS}/.git" ]] && [[ ! -e "${CAND_POS}/.sbin" ]]; then
  pass "validate_charts.sh passes positive gitless env -i run and cleans up .git/.sbin"
else
  fail "validate_charts.sh passes positive gitless env -i run and cleans up .git/.sbin"
fi

# Negative case 1: modifying mod_staging() in hack/deps.sh fails closed.
CAND_BAD_MOD="${SCRATCH}/cand-bad-mod"
cp -R "${CAND_BASE}" "${CAND_BAD_MOD}"
python3 - "${CAND_BASE}/hack/deps.sh" "${CAND_BAD_MOD}/hack/deps.sh" "${K8S_MOD_VER}" <<'PY'
import sys
src, dst, ver = sys.argv[1:4]
with open(src, "r", encoding="utf-8") as f:
    txt = f.read()
out = txt.replace(f"kubernetes/api {ver}", "kubernetes/api v0.99.99", 1)
assert out != txt, f"failed to mutate kubernetes/api {ver} in hack/deps.sh"
with open(dst, "w", encoding="utf-8") as f:
    f.write(out)
PY
bad_err=""
if ! bad_err="$(run_validator_env_i "${CAND_BAD_MOD}" "${STAGE_READY}" 2>&1)" && \
   grep -qi "deps\.sh" <<<"${bad_err}"; then
  pass "validate_charts.sh fails closed when mod_staging() in hack/deps.sh is modified"
else
  fail "validate_charts.sh fails closed when mod_staging() in hack/deps.sh is modified (${bad_err})"
fi

# Negative case 2: stray .rej, .orig, or .tgz under deploy/gpustack-operator/chart/charts/ fails closed.
for artifact_ext in rej orig tgz; do
  CAND_BAD_ART="${SCRATCH}/cand-bad-${artifact_ext}"
  cp -R "${CAND_BASE}" "${CAND_BAD_ART}"
  : > "${CAND_BAD_ART}/deploy/gpustack-operator/chart/charts/kueue/stray.${artifact_ext}"
  bad_err=""
  if ! bad_err="$(run_validator_env_i "${CAND_BAD_ART}" "${STAGE_READY}" 2>&1)" && \
     grep -q "${artifact_ext}" <<<"${bad_err}"; then
    pass "validate_charts.sh fails closed on stray .${artifact_ext} file under charts/"
  else
    fail "validate_charts.sh fails closed on stray .${artifact_ext} file under charts/ (${bad_err})"
  fi
done

# Negative case 3: unmirrored gpustack/mirrored-kueue:v0.99.0 tag in values.yaml fails closed.
CAND_BAD_IMG="${SCRATCH}/cand-bad-img"
cp -R "${CAND_BASE}" "${CAND_BAD_IMG}"
python3 - "${CAND_BASE}/deploy/gpustack-operator/chart/values.yaml" "${CAND_BAD_IMG}/deploy/gpustack-operator/chart/values.yaml" "${KUEUE_NEXT}" <<'PY'
import sys
src, dst, ver = sys.argv[1:4]
with open(src, "r", encoding="utf-8") as f:
    txt = f.read()
out = txt.replace(f"tag: v{ver}", "tag: v0.99.0", 1)
assert out != txt, f"failed to mutate tag: v{ver} in values.yaml"
with open(dst, "w", encoding="utf-8") as f:
    f.write(out)
PY
bad_err=""
if ! bad_err="$(run_validator_env_i "${CAND_BAD_IMG}" "${STAGE_READY}" 2>&1)" && \
   grep -q "mirrored-kueue:v0.99.0" <<<"${bad_err}"; then
  pass "validate_charts.sh fails closed on unmirrored gpustack/mirrored-kueue:v0.99.0 tag"
else
  fail "validate_charts.sh fails closed on unmirrored gpustack/mirrored-kueue:v0.99.0 tag (${bad_err})"
fi

# Negative case 4: mutating modelTransfer.images.nvidia in values.yaml fails closed.
CAND_BAD_MT="${SCRATCH}/cand-bad-mt"
cp -R "${CAND_BASE}" "${CAND_BAD_MT}"
python3 - "${CAND_BASE}/deploy/gpustack-operator/chart/values.yaml" "${CAND_BAD_MT}/deploy/gpustack-operator/chart/values.yaml" <<'PY'
import re
import sys
src, dst = sys.argv[1:3]
with open(src, "r", encoding="utf-8") as f:
    txt = f.read()
out = re.sub(r"(nvidia:\s*gpustack/model-transfer:)[^\s]+", r"\g<1>v9.9.9-cuda12.9", txt, count=1)
assert out != txt, "failed to mutate modelTransfer.images.nvidia in values.yaml"
with open(dst, "w", encoding="utf-8") as f:
    f.write(out)
PY
bad_err=""
if ! bad_err="$(run_validator_env_i "${CAND_BAD_MT}" "${STAGE_READY}" 2>&1)" && \
   grep -q "modelTransfer" <<<"${bad_err}"; then
  pass "validate_charts.sh fails closed when modelTransfer.images.nvidia is mutated in values.yaml"
else
  fail "validate_charts.sh fails closed when modelTransfer.images.nvidia is mutated in values.yaml (${bad_err})"
fi

# Negative case 5: registrar tag mismatch across modelManager.registrar.image and CSI drivers fails closed.
CAND_BAD_REG="${SCRATCH}/cand-bad-reg"
cp -R "${CAND_BASE}" "${CAND_BAD_REG}"
python3 - "${CAND_BAD_REG}/deploy/gpustack-operator/chart/values.yaml" <<'PY'
import re
import sys

p = sys.argv[1]
with open(p, "r", encoding="utf-8") as f:
    txt = f.read()
out = re.sub(
    r"(mirrored-csi-node-driver-registrar\s*\n\s*tag:\s*)v[0-9A-Za-z._-]+",
    r"\g<1>v2.99.0",
    txt,
    count=1,
)
assert out != txt, "failed to mutate mirrored-csi-node-driver-registrar tag"
with open(p, "w", encoding="utf-8") as f:
    f.write(out)
PY
bad_err=""
if ! bad_err="$(run_validator_env_i "${CAND_BAD_REG}" "${STAGE_READY}" 2>&1)" && \
   grep -q "registrar tag parity mismatch" <<<"${bad_err}"; then
  pass "validate_charts.sh fails closed on modelManager.registrar.image tag parity mismatch"
else
  fail "validate_charts.sh fails closed on modelManager.registrar.image tag parity mismatch (${bad_err})"
fi

# Negative case 6: missing cached .tgz when chart changed fails closed (I2).
STAGE_NO_CACHE="${SCRATCH}/stage-no-cache"
cp -R "${STAGE_READY}" "${STAGE_NO_CACHE}"
rm -f "${STAGE_NO_CACHE}/deps-cache/charts/kueue-${KUEUE_NEXT}.tgz"
bad_err=""
if ! bad_err="$(run_validator_env_i "${CAND_POS}" "${STAGE_NO_CACHE}" 2>&1)" && \
   grep -q "cached archive is missing" <<<"${bad_err}"; then
  pass "validate_charts.sh fails closed when changed chart lacks cached .tgz"
else
  fail "validate_charts.sh fails closed when changed chart lacks cached .tgz (${bad_err})"
fi

# Negative case 7: corrupted cached .tgz fails archiveSha256 check (I10).
STAGE_BAD_TGZ="${SCRATCH}/stage-bad-tgz"
cp -R "${STAGE_READY}" "${STAGE_BAD_TGZ}"
printf 'corrupt' >> "${STAGE_BAD_TGZ}/deps-cache/charts/kueue-${KUEUE_NEXT}.tgz"
bad_err=""
if ! bad_err="$(run_validator_env_i "${CAND_POS}" "${STAGE_BAD_TGZ}" 2>&1)" && \
   grep -q "SHA-256 mismatch" <<<"${bad_err}"; then
  pass "validate_charts.sh fails closed on cached .tgz archiveSha256 mismatch"
else
  fail "validate_charts.sh fails closed on cached .tgz archiveSha256 mismatch (${bad_err})"
fi

# Negative case 8: unset AUTOSYNC_STAGE_DIR fails closed unless AUTOSYNC_SKIP_IMAGE_CHECK=true (I3).
bad_err=""
if ! bad_err="$(env -i PATH=/usr/local/bin:/usr/bin:/bin HOME="${SCRATCH}/env-home" TMPDIR="${SCRATCH}" \
  LOOPWRIGHT_CANDIDATE_DIR="${CAND_POS}" AUTOSYNC_FAST_SELFTEST=true \
  bash "${CAND_POS}/.auto-sync/checks/validate_charts.sh" 2>&1)" && \
   grep -q "AUTOSYNC_STAGE_DIR" <<<"${bad_err}"; then
  pass "validate_charts.sh fails closed when AUTOSYNC_STAGE_DIR is unset"
else
  fail "validate_charts.sh fails closed when AUTOSYNC_STAGE_DIR is unset (${bad_err})"
fi

# Negative case 9: stale charts-discovery.json with mismatched archiveSha256ByVersion fails closed (R2-I3).
STAGE_STALE_META="${SCRATCH}/stage-stale-meta"
cp -R "${STAGE_READY}" "${STAGE_STALE_META}"
python3 - "${STAGE_STALE_META}/charts-discovery.json" "${STAGE_STALE_META}/charts-preflight-report.json" <<'PY'
import json
import sys

for p in sys.argv[1:]:
    with open(p, "r", encoding="utf-8") as f:
        doc = json.load(f)
    kueue_info = doc["charts"]["kueue"]
    kueue_info["candidate_version"] = "0.0.1"
    kueue_info["archiveSha256ByVersion"] = {"0.0.1": "0" * 64}
    with open(p, "w", encoding="utf-8") as f:
        json.dump(doc, f, indent=2)
PY
bad_err=""
if ! bad_err="$(run_validator_env_i "${CAND_POS}" "${STAGE_STALE_META}" 2>&1)" && \
   grep -q "metadata version mismatch" <<<"${bad_err}"; then
  pass "validate_charts.sh fails closed on stale charts-discovery.json version mismatch"
else
  fail "validate_charts.sh fails closed on stale charts-discovery.json version mismatch (${bad_err})"
fi

# 3d. csi-driver-s3 sidecar fallback emits canonical upstream + non-empty sync_spec when cand_ref is dropped (R2-I5).
FIXTURE_S3_DROPPED="${SCRATCH}/fixture-s3-dropped"
cp -R "${FIXTURE_DIR}" "${FIXTURE_S3_DROPPED}"
python3 - "${FIXTURE_S3_DROPPED}/archives" "${FIXTURE_S3_DROPPED}/upstream.json" <<'PY'
import json
import os
import sys
import tarfile
import tempfile

archives_dir, upstream_path = sys.argv[1:3]
with open(upstream_path, "r", encoding="utf-8") as f:
    up = json.load(f)
s3_arc = os.path.join(archives_dir, up["charts"]["csi-driver-s3"]["archive"])
with tempfile.TemporaryDirectory() as tmp:
    with tarfile.open(s3_arc, "r:gz") as tf:
        tf.extractall(tmp)
    vy_path = os.path.join(tmp, "csi-driver-s3", "values.yaml")
    with open(vy_path, "r", encoding="utf-8") as f:
        lines = f.readlines()
    filtered = [
        ln for ln in lines
        if not any(k in ln for k in ("csiProvisioner:", "livenessProbe:", "nodeDriverRegistrar:"))
    ]
    with open(vy_path, "w", encoding="utf-8") as f:
        f.writelines(filtered)
    with tarfile.open(s3_arc, "w:gz") as tf:
        tf.add(os.path.join(tmp, "csi-driver-s3"), arcname="csi-driver-s3")
PY
STAGE_S3_DROPPED="${SCRATCH}/stage-s3-dropped/charts"
if (
  cd "${PREFLIGHT_REPO}" && \
  AUTOSYNC_CHARTS_FIXTURE_DIR="${FIXTURE_S3_DROPPED}" \
  AUTOSYNC_MOCK_MANIFESTS_JSON="${MOCK_READY}" \
  AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
  AUTOSYNC_SKIP_SOURCE_REVISION=true \
  bash "${PREFLIGHT_REPO}/.auto-sync/scripts/preflight-charts.sh" "${STAGE_S3_DROPPED}"
) >/dev/null && \
python3 - "${STAGE_S3_DROPPED}/charts-discovery.json" <<'PY'
import json
import sys

with open(sys.argv[1], "r", encoding="utf-8") as f:
    disc = json.load(f)
reqs = disc["charts"]["csi-driver-s3"]["required_images"]
assert len(reqs) == 4, reqs
for r in reqs:
    assert r["upstream"] and "gpustack/mirrored-" not in r["upstream"], r
    assert r["sync_spec"], r
assert any(
    r["upstream"].startswith("registry.k8s.io/sig-storage/csi-provisioner:")
    for r in reqs
), reqs
assert any(
    r["upstream"].startswith("registry.k8s.io/sig-storage/livenessprobe:")
    for r in reqs
), reqs
assert any(
    r["upstream"].startswith("registry.k8s.io/sig-storage/csi-node-driver-registrar:")
    for r in reqs
), reqs
PY
then
  pass "preflight-charts.sh emits canonical upstream and sync_spec when csi-driver-s3 drops sidecar refs"
else
  fail "preflight-charts.sh emits canonical upstream and sync_spec when csi-driver-s3 drops sidecar refs"
fi

# 3e. Malformed image ref/tag in upstream chart values.yaml is rejected cleanly without shell/Markdown injection.
FIXTURE_MALFORMED_IMG="${SCRATCH}/fixture-malformed-img"
cp -R "${FIXTURE_DIR}" "${FIXTURE_MALFORMED_IMG}"
python3 - "${FIXTURE_MALFORMED_IMG}/archives" "${FIXTURE_MALFORMED_IMG}/upstream.json" <<'PY'
import json
import os
import sys
import tarfile
import tempfile

archives_dir, upstream_path = sys.argv[1:3]
with open(upstream_path, "r", encoding="utf-8") as f:
    up = json.load(f)
kueue_arc = os.path.join(archives_dir, up["charts"]["kueue"]["archive"])
with tempfile.TemporaryDirectory() as tmp:
    with tarfile.open(kueue_arc, "r:gz") as tf:
        tf.extractall(tmp)
    vy_path = os.path.join(tmp, "kueue", "values.yaml")
    with open(vy_path, "r", encoding="utf-8") as f:
        vy = f.read()
    lines = vy.splitlines()
    replaced = False
    for idx, ln in enumerate(lines):
        if "tag:" in ln and not replaced:
            indent = ln[: len(ln) - len(ln.lstrip())]
            lines[idx] = f'{indent}tag: "v1.0.0$(evil)|`<b>"'
            replaced = True
            break
    assert replaced, "failed to inject malformed tag in kueue values.yaml"
    with open(vy_path, "w", encoding="utf-8") as f:
        f.write("\n".join(lines) + "\n")
    with tarfile.open(kueue_arc, "w:gz") as tf:
        tf.add(os.path.join(tmp, "kueue"), arcname="kueue")
PY
STAGE_MALFORMED="${SCRATCH}/stage-malformed/charts"
SUMMARY_MALFORMED="${SCRATCH}/summary-malformed.md"
: > "${SUMMARY_MALFORMED}"
if (
  cd "${PREFLIGHT_REPO}" && \
  AUTOSYNC_CHARTS_FIXTURE_DIR="${FIXTURE_MALFORMED_IMG}" \
  AUTOSYNC_MOCK_MANIFESTS_JSON="${MOCK_NOT_READY}" \
  AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
  AUTOSYNC_SKIP_SOURCE_REVISION=true \
  GITHUB_STEP_SUMMARY="${SUMMARY_MALFORMED}" \
  bash "${PREFLIGHT_REPO}/.auto-sync/scripts/preflight-charts.sh" "${STAGE_MALFORMED}"
) >/dev/null 2>&1 && \
python3 - "${STAGE_MALFORMED}" "${SUMMARY_MALFORMED}" "${KUEUE_CUR}" <<'PY'
import json
import os
import sys

stage_dir, summary_path, kueue_cur = sys.argv[1:4]
with open(os.path.join(stage_dir, "charts-preflight-report.json"), "r", encoding="utf-8") as f:
    report = json.load(f)
kueue = report["charts"]["kueue"]
assert kueue["mirrored_image_ready"] is False, kueue
assert kueue["missing_sync_specs"] == [], kueue
assert kueue["sync_image_command"] == "", kueue
assert report["sync_image_command"] == "", report
assert "$(evil)" not in report["sync_image_command"]

with open(os.path.join(stage_dir, "upstream-records.json"), "r", encoding="utf-8") as f:
    recs = json.load(f)
by_id = {r["upstreamId"]: r for r in recs}
assert by_id["kueue-chart"]["status"] == "up_to_date", by_id
assert by_id["kueue-chart"]["version"] == kueue_cur, by_id

with open(summary_path, "r", encoding="utf-8") as f:
    summary_txt = f.read()
assert "$(evil)" not in summary_txt, summary_txt
assert "<b>" not in summary_txt, summary_txt
PY
then
  pass "preflight-charts.sh rejects malformed candidate image refs without shell or markdown injection"
else
  fail "preflight-charts.sh rejects malformed candidate image refs without shell or markdown injection"
fi
fi

echo
if (( FAILS > 0 )); then
  printf 'CHARTS SELFTEST FAILED: %d failure(s), %d passed\n' "${FAILS}" "${PASSES}" >&2
  exit 1
fi
printf 'CHARTS SELFTEST PASSED: %d check(s) passed\n' "${PASSES}"
