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

if extracted_out="$("${VALIDATE_SCRIPT}" --extract-images "${REAL_VALUES}" "${REAL_TOPO_CHART}")" && \
   python3 - "${extracted_out}" <<'PY'
import sys

lines = [ln.strip() for ln in sys.argv[1].splitlines() if ln.strip()]
expected = {
    "docker.io/gpustack/mirrored-kueue:v0.18.11",
    "docker.io/gpustack/mirrored-node-feature-discovery:v0.19.0-gpustack1",
    "docker.io/gpustack/mirrored-csi-nfs-driver:v4.13.4",
    "docker.io/gpustack/mirrored-csi-provisioner:v6.3.0",
    "docker.io/gpustack/mirrored-csi-resizer:v2.2.0",
    "docker.io/gpustack/mirrored-csi-snapshotter:v8.6.0",
    "docker.io/gpustack/mirrored-csi-livenessprobe:v2.19.0",
    "docker.io/gpustack/mirrored-csi-node-driver-registrar:v2.17.0",
    "docker.io/gpustack/mirrored-csi-s3-driver:v0.43.7",
    "docker.io/gpustack/mirrored-nvidia-topograph:v1.0.0",
}
got = set(lines)
missing = sorted(expected - got)
extra = sorted(got - expected)
assert not missing and not extra, f"missing={missing} extra={extra} got={sorted(got)}"
PY
then
  pass "validate_charts.sh extracts all mirrored subchart images from real values.yaml"
else
  fail "validate_charts.sh failed to extract expected mirrored subchart images from real values.yaml"
fi

# 2. Build offline fixture tarballs (reverse-patched from real subcharts) and a scratch repo.
FIXTURE_ARCHIVES="${SCRATCH}/fixture-archives"
mkdir -p "${FIXTURE_ARCHIVES}"

python3 - "${ROOT_DIR}" "${FIXTURE_ARCHIVES}" <<'PY'
import glob
import os
import re
import shutil
import subprocess
import tarfile
import tempfile
import sys

root_dir, out_dir = sys.argv[1:3]
charts = [
    ("kueue", "0.18.11", "kueue-0.18.11.tgz"),
    ("node-feature-discovery", "0.19.0", "node-feature-discovery-chart-0.19.0.tgz"),
    ("csi-driver-nfs", "4.13.4", "csi-driver-nfs-4.13.4.tgz"),
    ("csi-driver-s3", "0.43.7", "csi-s3-0.43.7.tgz"),
    ("topograph", "1.0.0", "topograph-1.0.0.tgz"),
]

with tempfile.TemporaryDirectory() as tmp:
    for name, ver, archive_name in charts:
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
            cy = cy.replace("version: 0.18.11", "version: 0.18.12")
            cy = cy.replace("appVersion: v0.18.11", "appVersion: v0.18.12")
            with open(chart_yaml, "w", encoding="utf-8") as f:
                f.write(cy)
            values_yaml = os.path.join(bumped, "values.yaml")
            with open(values_yaml, "r", encoding="utf-8") as f:
                vy = f.read()
            vy = re.sub(r"tag:\s*v0\.18\.11\b", "tag: v0.18.12", vy, count=1)
            with open(values_yaml, "w", encoding="utf-8") as f:
                f.write(vy)
            bumped_tgz = os.path.join(out_dir, "kueue-0.18.12.tgz")
            with tarfile.open(bumped_tgz, "w:gz") as tf:
                tf.add(bumped, arcname="kueue")
PY

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

cat > "${FIXTURE_DIR}/upstream.json" <<'JSON'
{
  "charts": {
    "kueue": {
      "version": "0.18.12",
      "url": "https://github.com/kubernetes-sigs/kueue/releases/download/v0.18.12/kueue-0.18.12.tgz",
      "archive": "kueue-0.18.12.tgz"
    },
    "node-feature-discovery": {
      "version": "0.19.0",
      "url": "https://github.com/kubernetes-sigs/node-feature-discovery/releases/download/v0.19.0/node-feature-discovery-chart-0.19.0.tgz",
      "archive": "node-feature-discovery-0.19.0.tgz"
    },
    "csi-driver-nfs": {
      "version": "4.13.4",
      "url": "https://raw.githubusercontent.com/kubernetes-csi/csi-driver-nfs/refs/heads/master/charts/v4.13.4/csi-driver-nfs-4.13.4.tgz",
      "archive": "csi-driver-nfs-4.13.4.tgz"
    },
    "csi-driver-s3": {
      "version": "0.43.7",
      "url": "https://thxcode.github.io/k8s-csi-s3/charts/csi-s3-0.43.7.tgz",
      "archive": "csi-driver-s3-0.43.7.tgz"
    },
    "topograph": {
      "version": "1.0.0",
      "url": "https://dsx-ai-factory.github.io/topograph/topograph-1.0.0.tgz",
      "archive": "topograph-1.0.0.tgz"
    }
  }
}
JSON

MOCK_NOT_READY="${SCRATCH}/mock-not-ready.json"
MOCK_READY="${SCRATCH}/mock-ready.json"
python3 - "${MOCK_NOT_READY}" "${MOCK_READY}" <<'PY'
import json
import sys

not_ready_path, ready_path = sys.argv[1:3]
base_images = [
    "docker.io/gpustack/mirrored-kueue:v0.18.11",
    "docker.io/gpustack/mirrored-node-feature-discovery:v0.19.0-gpustack1",
    "docker.io/gpustack/mirrored-csi-nfs-driver:v4.13.4",
    "docker.io/gpustack/mirrored-csi-provisioner:v6.3.0",
    "docker.io/gpustack/mirrored-csi-resizer:v2.2.0",
    "docker.io/gpustack/mirrored-csi-snapshotter:v8.6.0",
    "docker.io/gpustack/mirrored-csi-livenessprobe:v2.19.0",
    "docker.io/gpustack/mirrored-csi-node-driver-registrar:v2.17.0",
    "docker.io/gpustack/mirrored-csi-s3-driver:v0.43.7",
    "docker.io/gpustack/mirrored-nvidia-topograph:v1.0.0",
]

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
    if kueue_ready:
        images["docker.io/gpustack/mirrored-kueue:v0.18.12"] = {
            "published": True,
            "amd64Digest": "sha256:" + ("aa" * 32),
            "arm64Digest": "sha256:" + ("bb" * 32),
        }
    else:
        images["docker.io/gpustack/mirrored-kueue:v0.18.12"] = {
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
: > "${GHA_UNREADY}"
if (
  cd "${PREFLIGHT_REPO}" && \
  AUTOSYNC_CHARTS_FIXTURE_DIR="${FIXTURE_DIR}" \
  AUTOSYNC_MOCK_MANIFESTS_JSON="${MOCK_NOT_READY}" \
  AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
  GITHUB_OUTPUT="${GHA_UNREADY}" \
  bash "${PREFLIGHT_REPO}/.auto-sync/scripts/preflight-charts.sh" "${STAGE_UNREADY}"
) >/dev/null && \
python3 - "${STAGE_UNREADY}" "${GHA_UNREADY}" "${BASE_SHA}" <<'PY'
import json
import os
import sys

stage_dir, gha_path, base_sha = sys.argv[1:4]
with open(os.path.join(stage_dir, "charts-preflight-report.json"), "r", encoding="utf-8") as f:
    report = json.load(f)
assert report["source_revision"] == base_sha, report
kueue = report["charts"]["kueue"]
assert kueue["changed"] is True, kueue
assert kueue["candidate_version"] == "0.18.12", kueue
assert kueue["mirrored_image_ready"] is False, kueue
sync_cmd = kueue.get("sync_image_command") or report.get("sync_image_command", "")
assert "gh workflow run sync-image.yml -f images=" in sync_cmd, report
assert "registry.k8s.io/kueue/kueue:v0.18.12" in sync_cmd, sync_cmd

with open(os.path.join(stage_dir, "upstream-records.json"), "r", encoding="utf-8") as f:
    recs = json.load(f)
by_id = {r["upstreamId"]: r for r in recs}
assert by_id["kueue-chart"]["status"] == "available", by_id
assert by_id["nfd-chart"]["status"] == "up_to_date", by_id

with open(gha_path, "r", encoding="utf-8") as f:
    gha_txt = f.read()
assert f"\n{base_sha}\n" in gha_txt, gha_txt
PY
then
  if [[ -z "$(git -C "${PREFLIGHT_REPO}" status --porcelain)" ]] && [[ "$(git -C "${PREFLIGHT_REPO}" rev-parse HEAD)" == "${BASE_SHA}" ]]; then
    pass "preflight-charts.sh marks unmirrored kueue image mirrored_image_ready=false and emits sync-image.yml command"
  else
    fail "preflight-charts.sh unready case dirtied git tree"
  fi
else
  fail "preflight-charts.sh unready fixture test"
fi

STAGE_READY="${SCRATCH}/stage-ready/charts"
GHA_READY="${SCRATCH}/gha-ready.txt"
: > "${GHA_READY}"
if (
  cd "${PREFLIGHT_REPO}" && \
  AUTOSYNC_CHARTS_FIXTURE_DIR="${FIXTURE_DIR}" \
  AUTOSYNC_MOCK_MANIFESTS_JSON="${MOCK_READY}" \
  AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
  GITHUB_OUTPUT="${GHA_READY}" \
  bash "${PREFLIGHT_REPO}/.auto-sync/scripts/preflight-charts.sh" "${STAGE_READY}"
) >/dev/null && \
SRC_REV="$(python3 - "${STAGE_READY}" "${BASE_SHA}" <<'PY'
import json
import os
import sys

stage_dir, base_sha = sys.argv[1:3]
with open(os.path.join(stage_dir, "charts-preflight-report.json"), "r", encoding="utf-8") as f:
    report = json.load(f)
src_rev = report["source_revision"]
assert src_rev and src_rev != base_sha, report
kueue = report["charts"]["kueue"]
assert kueue["changed"] is True, kueue
assert kueue["mirrored_image_ready"] is True, kueue
assert kueue["patch_clean"] is True, kueue
assert os.path.isfile(os.path.join(stage_dir, "deps-cache", "charts", "kueue-0.18.12.tgz"))
print(src_rev)
PY
)" && \
[[ -n "${SRC_REV}" && "${SRC_REV}" != "${BASE_SHA}" ]] && \
[[ "$(git -C "${PREFLIGHT_REPO}" show "${SRC_REV}:deploy/gpustack-operator/chart/charts/kueue/_VERSION_")" == "0.18.12" ]] && \
grep -q 'kueue-0.18.12.tgz 0.18.12' <<<"$(git -C "${PREFLIGHT_REPO}" show "${SRC_REV}:hack/deps.sh")" && \
[[ -z "$(git -C "${PREFLIGHT_REPO}" status --porcelain)" ]] && \
[[ "$(git -C "${PREFLIGHT_REPO}" rev-parse HEAD)" == "${BASE_SHA}" ]]; then
  pass "preflight-charts.sh stages mirrored-ready kueue bump onto clean detached source_revision"
else
  fail "preflight-charts.sh ready fixture test"
fi

# 4. Gitless env -i validate_charts.sh tests (positive + 3 negative cases).
CAND_BASE="${SCRATCH}/cand-base"
mkdir -p "${CAND_BASE}"
git -C "${PREFLIGHT_REPO}" archive "${SRC_REV}" | tar -x -C "${CAND_BASE}"

# Align values.yaml and proposals/charts.json with the bumped kueue 0.18.12 in the candidate tree.
python3 - "${CAND_BASE}/deploy/gpustack-operator/chart/values.yaml" "${CAND_BASE}/.auto-sync/proposals/charts.json" <<'PY'
import json
import re
import sys

vy_path, prop_path = sys.argv[1:3]
with open(vy_path, "r", encoding="utf-8") as f:
    vy = f.read()
vy = vy.replace("chart 0.18.11", "chart 0.18.12")
vy = re.sub(r"(repository:\s*docker\.io/gpustack/mirrored-kueue\n(?:[^\n]*\n){0,4}?\s*tag:\s*)v0\.18\.11\b", r"\g<1>v0.18.12", vy, count=1)
with open(vy_path, "w", encoding="utf-8") as f:
    f.write(vy)

with open(prop_path, "r", encoding="utf-8") as f:
    prop = json.load(f)
prop["versions"]["kueue"] = "0.18.12"
for sub in prop.get("subscriptions", []):
    if isinstance(sub, dict) and sub.get("id") == "kueue-chart":
        sub["version"] = "0.18.12"
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
sed 's|kubernetes/api v0.36.5|kubernetes/api v0.36.6|' "${CAND_BASE}/hack/deps.sh" > "${CAND_BAD_MOD}/hack/deps.sh"
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
sed 's|tag: v0.18.12|tag: v0.99.0|' "${CAND_BASE}/deploy/gpustack-operator/chart/values.yaml" > "${CAND_BAD_IMG}/deploy/gpustack-operator/chart/values.yaml"
bad_err=""
if ! bad_err="$(run_validator_env_i "${CAND_BAD_IMG}" "${STAGE_READY}" 2>&1)" && \
   grep -q "mirrored-kueue:v0.99.0" <<<"${bad_err}"; then
  pass "validate_charts.sh fails closed on unmirrored gpustack/mirrored-kueue:v0.99.0 tag"
else
  fail "validate_charts.sh fails closed on unmirrored gpustack/mirrored-kueue:v0.99.0 tag (${bad_err})"
fi

echo
if (( FAILS > 0 )); then
  printf 'CHARTS SELFTEST FAILED: %d failure(s), %d passed\n' "${FAILS}" "${PASSES}" >&2
  exit 1
fi
printf 'CHARTS SELFTEST PASSED: %d check(s) passed\n' "${PASSES}"
