#!/usr/bin/env bash
# .auto-sync/checks/selftest_k8s_deps.sh - offline self-tests for k8s-deps-sync.
#
# Usage:
#   bash .auto-sync/checks/selftest_k8s_deps.sh [repo-root]
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
  printf 'PASS  [k8s-deps] %s\n' "$1"
  PASSES=$((PASSES + 1))
}

fail() {
  printf 'FAIL  [k8s-deps] %s\n' "$1" >&2
  FAILS=$((FAILS + 1))
}

SCRATCH="$(cd "$(mktemp -d)" && pwd -P)"

TASKPACK_FILE="${ROOT_DIR}/.auto-sync/k8s-deps-taskpack.yaml"
INSTRUCTIONS_FILE="${ROOT_DIR}/.auto-sync/instructions/k8s-deps.md"
PROPOSAL_FILE="${ROOT_DIR}/.auto-sync/proposals/k8s-deps.json"
PREFLIGHT_SCRIPT="${ROOT_DIR}/.auto-sync/scripts/preflight-k8s-deps.sh"
VALIDATE_SCRIPT="${ROOT_DIR}/.auto-sync/checks/validate_k8s_deps.sh"

for req in "${TASKPACK_FILE}" "${INSTRUCTIONS_FILE}" "${PROPOSAL_FILE}" "${PREFLIGHT_SCRIPT}" "${VALIDATE_SCRIPT}"; do
  if [[ ! -f "${req}" ]]; then
    fail "missing required k8s-deps-sync deliverable: ${req#"${ROOT_DIR}/"}"
  fi
done

if (( FAILS > 0 )); then
  printf 'K8S-DEPS SELFTEST FAILED (RED): %d missing deliverable(s)\n' "${FAILS}" >&2
  exit 1
fi

HOST_GOMODCACHE="$(go env GOMODCACHE)"
HOST_GOCACHE="$(go env GOCACHE)"

# Helper to copy the minimal repo subset needed by preflight and validation.
copy_k8s_deps_tree() {
  local src_root="$1"
  local dst_root="$2"
  local rel_paths=(
    "go.mod"
    "go.sum"
    "pack/gpustack-operator/Dockerfile"
    "hack/deps.sh"
    "hack/lib"
    "hack/staging"
    "hack/check/upgrade-compat.sh"
    "hack/check/toolpins-selftest.sh"
    "pkg/worker/settings/value.go"
    "docs/reference/settings.md"
    "api"
    "staging"
    ".auto-sync/scripts/lib-preflight.sh"
    ".auto-sync/checks/lib.sh"
    ".auto-sync/proposals/k8s-deps.json"
  )
  local rel
  for rel in "${rel_paths[@]}"; do
    if [[ -e "${src_root}/${rel}" ]]; then
      mkdir -p "${dst_root}/$(dirname "${rel}")"
      cp -R "${src_root}/${rel}" "${dst_root}/$(dirname "${rel}")/"
    fi
  done
  cp "${PREFLIGHT_SCRIPT}" "${dst_root}/.auto-sync/scripts/preflight-k8s-deps.sh"
  cp "${VALIDATE_SCRIPT}" "${dst_root}/.auto-sync/checks/validate_k8s_deps.sh"
  chmod +x "${dst_root}/.auto-sync/scripts/preflight-k8s-deps.sh" "${dst_root}/.auto-sync/checks/validate_k8s_deps.sh"
}

# 1. Offline preflight-k8s-deps.sh fixture discovery test.
PREFLIGHT_REPO="${SCRATCH}/preflight-repo"
mkdir -p "${PREFLIGHT_REPO}/.auto-sync/scripts" "${PREFLIGHT_REPO}/.auto-sync/checks" "${PREFLIGHT_REPO}/.auto-sync/proposals"
copy_k8s_deps_tree "${ROOT_DIR}" "${PREFLIGHT_REPO}"

git -C "${PREFLIGHT_REPO}" init -q -b main
git -C "${PREFLIGHT_REPO}" config user.name "k8s-deps-selftest"
git -C "${PREFLIGHT_REPO}" config user.email "k8s-deps-selftest@gpustack.local"
git -C "${PREFLIGHT_REPO}" config commit.gpgsign false
git -C "${PREFLIGHT_REPO}" add -A
git -C "${PREFLIGHT_REPO}" commit -q -m "baseline"
BASE_SHA="$(git -C "${PREFLIGHT_REPO}" rev-parse HEAD)"

FIXTURE_DISCOVERY="${SCRATCH}/fixture-discovery"
mkdir -p "${FIXTURE_DISCOVERY}"

cat > "${FIXTURE_DISCOVERY}/kubernetes-releases.json" <<'JSON'
[
  {"tag_name": "v1.37.0", "prerelease": false, "draft": false},
  {"tag_name": "v1.36.7-rc.1", "prerelease": true, "draft": false},
  {"tag_name": "v1.36.6", "prerelease": false, "draft": false},
  {"tag_name": "v1.36.5", "prerelease": false, "draft": false}
]
JSON

cat > "${FIXTURE_DISCOVERY}/helm-releases.json" <<'JSON'
[
  {"tag_name": "v4.0.0", "prerelease": false, "draft": false},
  {"tag_name": "v3.22.0-rc.1", "prerelease": true, "draft": false},
  {"tag_name": "v3.21.5", "prerelease": false, "draft": false},
  {"tag_name": "v3.21.4", "prerelease": false, "draft": false}
]
JSON

cat > "${FIXTURE_DISCOVERY}/kustomize-tags.txt" <<'TXT'
0000000000000000000000000000000000000001	refs/tags/kustomize/v5.8.4
0000000000000000000000000000000000000002	refs/tags/kustomize/v5.8.5-rc.1
0000000000000000000000000000000000000003	refs/tags/kustomize/v5.8.3
TXT

cat > "${FIXTURE_DISCOVERY}/skopeo-releases.json" <<'JSON'
[
  {"tag_name": "v1.24.1", "prerelease": false, "draft": false}
]
JSON

cat > "${FIXTURE_DISCOVERY}/vals-releases.json" <<'JSON'
[
  {"tag_name": "v0.50.1", "prerelease": false, "draft": false},
  {"tag_name": "v0.50.0", "prerelease": false, "draft": false}
]
JSON

cat > "${FIXTURE_DISCOVERY}/ytt-releases.json" <<'JSON'
[
  {"tag_name": "v0.55.4", "prerelease": false, "draft": false}
]
JSON

cat > "${FIXTURE_DISCOVERY}/controller-runtime-releases.json" <<'JSON'
[
  {"tag_name": "v0.25.0", "prerelease": false, "draft": false},
  {"tag_name": "v0.24.2", "prerelease": false, "draft": false},
  {"tag_name": "v0.24.1", "prerelease": false, "draft": false}
]
JSON

cat > "${FIXTURE_DISCOVERY}/kueue-releases.json" <<'JSON'
[
  {"tag_name": "v0.19.0", "prerelease": false, "draft": false},
  {"tag_name": "v0.18.11", "prerelease": false, "draft": false}
]
JSON

STAGE_DISC="${SCRATCH}/stage-disc/k8s-deps"
GHA_DISC="${SCRATCH}/gha-disc.txt"
: > "${GHA_DISC}"

if (
  cd "${PREFLIGHT_REPO}" && \
  AUTOSYNC_K8S_DEPS_FIXTURE_DIR="${FIXTURE_DISCOVERY}" \
  AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
  AUTOSYNC_SKIP_SOURCE_REVISION=true \
  GOMODCACHE="${HOST_GOMODCACHE}" \
  GOCACHE="${HOST_GOCACHE}" \
  GITHUB_OUTPUT="${GHA_DISC}" \
  bash "${PREFLIGHT_REPO}/.auto-sync/scripts/preflight-k8s-deps.sh" "${STAGE_DISC}" >/dev/null
) && \
python3 - "${STAGE_DISC}" "${GHA_DISC}" "${BASE_SHA}" <<'PY'
import json
import os
import sys

stage_dir, gha_path, base_sha = sys.argv[1:4]

with open(os.path.join(stage_dir, "upstream-records.json"), "r", encoding="utf-8") as f:
    recs = json.load(f)
assert isinstance(recs, list) and len(recs) == 8, recs
by_id = {r["upstreamId"]: r for r in recs}

assert by_id["kustomize-cli"]["version"] == "5.8.4", by_id["kustomize-cli"]
assert by_id["kustomize-cli"]["status"] == "available", by_id["kustomize-cli"]

assert by_id["helm-cli"]["version"] == "v3.21.5", by_id["helm-cli"]
assert by_id["helm-cli"]["status"] == "available", by_id["helm-cli"]

assert by_id["kubernetes"]["version"] == "v1.36.6", by_id["kubernetes"]
assert by_id["kubernetes"]["status"] == "available", by_id["kubernetes"]

assert by_id["vals-cli"]["version"] == "0.50.1", by_id["vals-cli"]
assert by_id["vals-cli"]["status"] == "available", by_id["vals-cli"]

assert by_id["skopeo-cli"]["version"] == "1.24.1", by_id["skopeo-cli"]
assert by_id["skopeo-cli"]["status"] == "up_to_date", by_id["skopeo-cli"]

assert by_id["ytt-cli"]["version"] == "0.55.4", by_id["ytt-cli"]
assert by_id["ytt-cli"]["status"] == "up_to_date", by_id["ytt-cli"]

assert by_id["controller-runtime"]["version"] == "v0.24.2", by_id["controller-runtime"]
assert by_id["controller-runtime"]["status"] == "available", by_id["controller-runtime"]

assert by_id["kueue-sdk"]["version"] == "v0.18.11", by_id["kueue-sdk"]
assert by_id["kueue-sdk"]["status"] == "up_to_date", by_id["kueue-sdk"]

with open(os.path.join(stage_dir, "k8s-deps-preflight-report.json"), "r", encoding="utf-8") as f:
    report = json.load(f)
assert report["k8s_minor"] == "0.36", report
assert report["helm_major"] == "3", report
assert report["base_sha"] == base_sha, report
assert report["source_revision"] == base_sha, report

for rel in (
    "pack/gpustack-operator/Dockerfile",
    "hack/deps.sh",
    "hack/lib/helm.sh",
    "go.mod",
    "api",
):
    assert os.path.exists(os.path.join(stage_dir, "baseline", rel)), f"missing baseline {rel}"

with open(gha_path, "r", encoding="utf-8") as f:
    gha_txt = f.read()
assert f"\n{stage_dir}\n" in gha_txt, gha_txt
assert f"\n{base_sha}\n" in gha_txt, gha_txt
assert "upstream_records<<" in gha_txt, gha_txt
PY
then
  if [[ -z "$(git -C "${PREFLIGHT_REPO}" status --porcelain)" ]] && \
     [[ "$(git -C "${PREFLIGHT_REPO}" rev-parse HEAD)" == "${BASE_SHA}" ]]; then
    pass "preflight-k8s-deps.sh filters cross-minor k8s/helm v4, normalizes kustomize/v5.8.4, and snapshots baselines"
  else
    fail "preflight-k8s-deps.sh discovery left dirty git tree"
  fi
else
  fail "preflight-k8s-deps.sh offline discovery fixture test"
fi

# 2. Offline preflight-k8s-deps.sh detached source_revision test via cached mod_staging tarballs.
FIXTURE_STAGE="${SCRATCH}/fixture-stage"
cp -R "${FIXTURE_DISCOVERY}" "${FIXTURE_STAGE}"
mkdir -p "${FIXTURE_STAGE}/deps-cache/staging/k8s.io"

for mod_name in code-generator api apimachinery kube-aggregator apiextensions-apiserver; do
  tar -zcf "${FIXTURE_STAGE}/deps-cache/staging/k8s.io/${mod_name}-v0.36.6.tar.gz" \
    --exclude='_VERSION_' \
    -C "${ROOT_DIR}/staging/k8s.io/${mod_name}" .
done
# Seed unpatched code-generator source so hack/staging/k8s.io/code-generator/001_enhanced.patch applies cleanly.
rm -f "${FIXTURE_STAGE}/deps-cache/staging/k8s.io/code-generator-v0.36.6.tar.gz"
CG_TMP="${SCRATCH}/cg-unpatched"
mkdir -p "${CG_TMP}"
cp -R "${ROOT_DIR}/staging/k8s.io/code-generator/." "${CG_TMP}/"
rm -f "${CG_TMP}/_VERSION_"
(cd "${CG_TMP}" && patch -p1 -R --silent < "${ROOT_DIR}/hack/staging/k8s.io/code-generator/001_enhanced.patch")
tar -zcf "${FIXTURE_STAGE}/deps-cache/staging/k8s.io/code-generator-v0.36.6.tar.gz" -C "${CG_TMP}" .

STAGE_SRC_REV="${SCRATCH}/stage-srcrev/k8s-deps"
GHA_SRC_REV="${SCRATCH}/gha-srcrev.txt"
: > "${GHA_SRC_REV}"

if (
  cd "${PREFLIGHT_REPO}" && \
  AUTOSYNC_K8S_DEPS_FIXTURE_DIR="${FIXTURE_STAGE}" \
  AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
  AUTOSYNC_SKIP_GENERATE=true \
  GOMODCACHE="${HOST_GOMODCACHE}" \
  GOCACHE="${HOST_GOCACHE}" \
  GITHUB_OUTPUT="${GHA_SRC_REV}" \
  bash "${PREFLIGHT_REPO}/.auto-sync/scripts/preflight-k8s-deps.sh" "${STAGE_SRC_REV}" >/dev/null
) && \
SRC_REV="$(python3 - "${STAGE_SRC_REV}" "${BASE_SHA}" <<'PY'
import json
import os
import sys

stage_dir, base_sha = sys.argv[1:3]
with open(os.path.join(stage_dir, "k8s-deps-preflight-report.json"), "r", encoding="utf-8") as f:
    report = json.load(f)
assert report["source_revision"] != base_sha, report
assert report["patch_clean"] is True, report
assert report["staging_updated"] is True, report
print(report["source_revision"])
PY
)" && \
[[ -n "${SRC_REV}" && "${SRC_REV}" != "${BASE_SHA}" ]] && \
grep -q 'kubernetes/api v0.36.6' <<<"$(git -C "${PREFLIGHT_REPO}" show "${SRC_REV}:hack/deps.sh")" && \
[[ "$(git -C "${PREFLIGHT_REPO}" show "${SRC_REV}:staging/k8s.io/api/_VERSION_")" == "v0.36.6" ]] && \
[[ -z "$(git -C "${PREFLIGHT_REPO}" status --porcelain)" ]] && \
[[ "$(git -C "${PREFLIGHT_REPO}" rev-parse HEAD)" == "${BASE_SHA}" ]] && \
[[ "$(git -C "${PREFLIGHT_REPO}" rev-parse refs/heads/main)" == "${BASE_SHA}" ]]; then
  pass "preflight-k8s-deps.sh stages mod_staging onto detached source_revision while preserving main"
else
  fail "preflight-k8s-deps.sh detached source_revision staging"
fi

# 3. Gitless env -i validate_k8s_deps.sh tests (AUTOSYNC_FAST_SELFTEST=true).
CAND_BASE="${SCRATCH}/cand-base"
mkdir -p "${CAND_BASE}/.auto-sync/scripts" "${CAND_BASE}/.auto-sync/checks" "${CAND_BASE}/.auto-sync/proposals"
copy_k8s_deps_tree "${ROOT_DIR}" "${CAND_BASE}"

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
    bash "${cand_dir}/.auto-sync/checks/validate_k8s_deps.sh"
}

# 3a. Positive case: bumping VALS_VERSION="0.50.1" in pack/gpustack-operator/Dockerfile.
CAND_POS="${SCRATCH}/cand-pos"
cp -R "${CAND_BASE}" "${CAND_POS}"
sed 's/^ARG VALS_VERSION="0\.50\.0"$/ARG VALS_VERSION="0.50.1"/' \
  "${CAND_BASE}/pack/gpustack-operator/Dockerfile" > "${CAND_POS}/pack/gpustack-operator/Dockerfile"

if run_validator_env_i "${CAND_POS}" "${STAGE_DISC}" >/dev/null 2>&1 && \
   [[ ! -e "${CAND_POS}/.git" ]] && [[ ! -e "${CAND_POS}/.sbin" ]]; then
  pass "validate_k8s_deps.sh positive VALS_VERSION=0.50.1 bump passes guards, upgrade-compat, and toolpins-selftest"
else
  fail "validate_k8s_deps.sh positive VALS_VERSION=0.50.1 bump"
fi

# 3b. Negative case 1: mutating LIB_HAMI_CORE_COMMIT in pack/gpustack-operator/Dockerfile.
CAND_NEG1="${SCRATCH}/cand-neg1"
cp -R "${CAND_BASE}" "${CAND_NEG1}"
sed 's/^ARG LIB_HAMI_CORE_COMMIT="[^"]*"$/ARG LIB_HAMI_CORE_COMMIT="1111111111111111111111111111111111111111"/' \
  "${CAND_BASE}/pack/gpustack-operator/Dockerfile" > "${CAND_NEG1}/pack/gpustack-operator/Dockerfile"

neg1_err=""
if ! neg1_err="$(run_validator_env_i "${CAND_NEG1}" "${STAGE_DISC}" 2>&1)" && \
   [[ ! -e "${CAND_NEG1}/.git" ]]; then
  pass "validate_k8s_deps.sh fails closed on LIB_HAMI_CORE_COMMIT mutation in Dockerfile"
else
  fail "validate_k8s_deps.sh fails closed on LIB_HAMI_CORE_COMMIT mutation (${neg1_err})"
fi

# 3c. Negative case 2: mutating chart_staging() in hack/deps.sh.
CAND_NEG2="${SCRATCH}/cand-neg2"
cp -R "${CAND_BASE}" "${CAND_NEG2}"
sed 's|kueue-0\.18\.11\.tgz 0\.18\.11|kueue-0.18.12.tgz 0.18.12|' \
  "${CAND_BASE}/hack/deps.sh" > "${CAND_NEG2}/hack/deps.sh"

neg2_err=""
if ! neg2_err="$(run_validator_env_i "${CAND_NEG2}" "${STAGE_DISC}" 2>&1)" && \
   [[ ! -e "${CAND_NEG2}/.git" ]]; then
  pass "validate_k8s_deps.sh fails closed on chart_staging() mutation in hack/deps.sh"
else
  fail "validate_k8s_deps.sh fails closed on chart_staging() mutation (${neg2_err})"
fi

# 3d. Negative case 3: modifying a hand-written .go file under api/v1/.
CAND_NEG3="${SCRATCH}/cand-neg3"
cp -R "${CAND_BASE}" "${CAND_NEG3}"
hand_api_file="$(find "${CAND_NEG3}/api/v1" -maxdepth 1 -type f -name '*.go' ! -name 'zz_generated*' ! -name 'generated.*' | sort | head -n 1)"
if [[ -z "${hand_api_file}" ]]; then
  fail "unable to locate hand-written api/v1/*.go file for negative test"
else
  printf '\n// illegal hand-written api edit\n' >> "${hand_api_file}"
  neg3_err=""
  if ! neg3_err="$(run_validator_env_i "${CAND_NEG3}" "${STAGE_DISC}" 2>&1)" && \
     [[ ! -e "${CAND_NEG3}/.git" ]]; then
    pass "validate_k8s_deps.sh fails closed on hand-written api/v1/*.go edit"
  else
    fail "validate_k8s_deps.sh fails closed on hand-written api/v1/*.go edit (${neg3_err})"
  fi
fi

# 3e. Negative case 4: bumping HELM_VERSION="3.21.5" in Dockerfile without updating hack/lib/helm.sh.
CAND_NEG4="${SCRATCH}/cand-neg4"
cp -R "${CAND_BASE}" "${CAND_NEG4}"
sed 's/^ARG HELM_VERSION="3\.21\.4"$/ARG HELM_VERSION="3.21.5"/' \
  "${CAND_BASE}/pack/gpustack-operator/Dockerfile" > "${CAND_NEG4}/pack/gpustack-operator/Dockerfile"

neg4_err=""
if ! neg4_err="$(run_validator_env_i "${CAND_NEG4}" "${STAGE_DISC}" 2>&1)" && \
   [[ ! -e "${CAND_NEG4}/.git" ]]; then
  pass "validate_k8s_deps.sh fails closed on unaligned HELM_VERSION bump in Dockerfile"
else
  fail "validate_k8s_deps.sh fails closed on unaligned HELM_VERSION bump (${neg4_err})"
fi

# 3f. Negative case 5: cross-minor K8s jump (k8s.io/api v0.37.0 in go.mod) fails closed.
CAND_NEG5="${SCRATCH}/cand-neg5"
cp -R "${CAND_BASE}" "${CAND_NEG5}"
sed 's|k8s\.io/api v0\.36\.5|k8s.io/api v0.37.0|' \
  "${CAND_BASE}/go.mod" > "${CAND_NEG5}/go.mod"

neg5_err=""
if ! neg5_err="$(run_validator_env_i "${CAND_NEG5}" "${STAGE_DISC}" 2>&1)" && \
   [[ ! -e "${CAND_NEG5}/.git" ]]; then
  pass "validate_k8s_deps.sh fails closed on cross-minor k8s.io/api v0.37.0 jump"
else
  fail "validate_k8s_deps.sh fails closed on cross-minor k8s.io/api jump (${neg5_err})"
fi

echo
if (( FAILS > 0 )); then
  printf 'K8S-DEPS SELFTEST FAILED: %d failure(s), %d passed\n' "${FAILS}" "${PASSES}" >&2
  exit 1
fi
printf 'K8S-DEPS SELFTEST PASSED: %d check(s) passed\n' "${PASSES}"
