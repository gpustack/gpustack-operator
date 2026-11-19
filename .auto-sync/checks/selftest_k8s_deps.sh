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
    chmod -R u+w "${SCRATCH}" 2>/dev/null || true
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

FAST_CP_MODE="plain"
detect_fast_cp_mode() {
  local probe_src="${SCRATCH}/.cp-probe-src"
  local probe_dst="${SCRATCH}/.cp-probe-dst"
  mkdir -p "${probe_src}"
  : > "${probe_src}/f"
  if cp -Rc "${probe_src}" "${probe_dst}" 2>/dev/null; then
    FAST_CP_MODE="apfs"
  elif cp -R --reflink=auto "${probe_src}" "${probe_dst}" 2>/dev/null; then
    FAST_CP_MODE="reflink"
  fi
  rm -rf "${probe_src}" "${probe_dst}"
}
detect_fast_cp_mode

fast_cp_dir() {
  case "${FAST_CP_MODE}" in
    apfs) cp -Rc "$1" "$2" ;;
    reflink) cp -R --reflink=auto "$1" "$2" ;;
    *) cp -R "$1" "$2" ;;
  esac
}

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
      fast_cp_dir "${src_root}/${rel}" "${dst_root}/$(dirname "${rel}")/"
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
git -C "${PREFLIGHT_REPO}" config gc.auto 0
git -C "${PREFLIGHT_REPO}" add -A
git -C "${PREFLIGHT_REPO}" -c gc.auto=0 commit -q -m "baseline"
BASE_SHA="$(git -C "${PREFLIGHT_REPO}" rev-parse HEAD)"

FIXTURE_META="${SCRATCH}/fixture-meta.json"
python3 - "${ROOT_DIR}" "${FIXTURE_META}" <<'PY'
import json
import os
import re
import sys

root_dir, meta_out = sys.argv[1:3]


def read(rel: str) -> str:
    with open(os.path.join(root_dir, rel), "r", encoding="utf-8") as f:
        return f.read()


df_txt = read("pack/gpustack-operator/Dockerfile")
gomod_txt = read("go.mod")
deps_txt = read("hack/deps.sh")


def get_arg(name: str) -> str:
    m = re.search(rf'^ARG {re.escape(name)}="([^"]+)"$', df_txt, re.MULTILINE)
    assert m, f"missing ARG {name}"
    return m.group(1).strip()


def get_mod(mod: str) -> str:
    m = re.search(rf'^\s*{re.escape(mod)}\s+(v\d+\.\d+\.\d+)\b', gomod_txt, re.MULTILINE)
    assert m, f"missing {mod} in go.mod"
    return m.group(1).strip()


def bump_patch(ver: str) -> str:
    prefix = "v" if ver.startswith("v") else ""
    parts = ver.lstrip("v").split(".")
    assert len(parts) == 3, f"unexpected semver {ver}"
    return f"{prefix}{parts[0]}.{parts[1]}.{int(parts[2]) + 1}"


def bump_minor(ver: str) -> str:
    prefix = "v" if ver.startswith("v") else ""
    parts = ver.lstrip("v").split(".")
    assert len(parts) == 3, f"unexpected semver {ver}"
    return f"{prefix}{parts[0]}.{int(parts[1]) + 1}.0"


k8s_mod_cur = get_mod("k8s.io/api")
k8s_mod_next = bump_patch(k8s_mod_cur)
k8s_mod_cross_minor = bump_minor(k8s_mod_cur)
kubectl_cur = get_arg("KUBECTL_VERSION")
k8s_rel_cur = f"v{kubectl_cur}"
k8s_rel_next = bump_patch(k8s_rel_cur)
k8s_rel_cross_minor = bump_minor(k8s_rel_cur)

helm_cur = get_arg("HELM_VERSION")
helm_next = bump_patch(helm_cur)
vals_cur = get_arg("VALS_VERSION")
vals_next = bump_patch(vals_cur)
kust_cur = get_arg("KUSTOMIZE_VERSION")
kust_next = bump_patch(kust_cur)
skopeo_cur = get_arg("SKOPEO_VERSION")
ytt_cur = get_arg("YTT_VERSION")

cr_cur = get_mod("sigs.k8s.io/controller-runtime")
cr_next = bump_patch(cr_cur)
cr_cross_minor = bump_minor(cr_cur)
kueue_cur = get_mod("sigs.k8s.io/kueue")
kueue_cross_minor = bump_minor(kueue_cur)

m_chart = re.search(r"function chart_staging\(\)\s*\{.*?cat <<EOF\n(.*?)\nEOF", deps_txt, re.DOTALL)
assert m_chart, "missing chart_staging() in hack/deps.sh"
first_chart_line = next(ln.strip() for ln in m_chart.group(1).splitlines() if ln.strip())
chart_url, chart_ver, chart_dest = first_chart_line.split()
chart_name = os.path.basename(chart_dest)
chart_ver_next = bump_patch(chart_ver)

with open(meta_out, "w", encoding="utf-8") as f:
    json.dump({
        "k8s_minor": ".".join(k8s_mod_cur.lstrip("v").split(".")[:2]),
        "k8s_mod_cur": k8s_mod_cur,
        "k8s_mod_next": k8s_mod_next,
        "k8s_mod_cross_minor": k8s_mod_cross_minor,
        "k8s_rel_cur": k8s_rel_cur,
        "k8s_rel_next": k8s_rel_next,
        "k8s_rel_cross_minor": k8s_rel_cross_minor,
        "helm_cur": helm_cur,
        "helm_next": helm_next,
        "vals_cur": vals_cur,
        "vals_next": vals_next,
        "kust_cur": kust_cur,
        "kust_next": kust_next,
        "skopeo_cur": skopeo_cur,
        "ytt_cur": ytt_cur,
        "cr_cur": cr_cur,
        "cr_next": cr_next,
        "cr_cross_minor": cr_cross_minor,
        "kueue_cur": kueue_cur,
        "kueue_cross_minor": kueue_cross_minor,
        "first_chart_name": chart_name,
        "first_chart_ver": chart_ver,
        "first_chart_ver_next": chart_ver_next,
    }, f, indent=2)
PY

read_meta() {
  python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))[sys.argv[2]])' "${FIXTURE_META}" "$1"
}

K8S_MOD_CUR="$(read_meta k8s_mod_cur)"
K8S_MOD_NEXT="$(read_meta k8s_mod_next)"
K8S_MOD_CROSS_MINOR="$(read_meta k8s_mod_cross_minor)"
K8S_REL_CUR="$(read_meta k8s_rel_cur)"
K8S_REL_NEXT="$(read_meta k8s_rel_next)"
K8S_REL_CROSS_MINOR="$(read_meta k8s_rel_cross_minor)"
HELM_CUR="$(read_meta helm_cur)"
HELM_NEXT="$(read_meta helm_next)"
VALS_CUR="$(read_meta vals_cur)"
VALS_NEXT="$(read_meta vals_next)"
KUST_CUR="$(read_meta kust_cur)"
KUST_NEXT="$(read_meta kust_next)"
SKOPEO_CUR="$(read_meta skopeo_cur)"
YTT_CUR="$(read_meta ytt_cur)"
CR_CUR="$(read_meta cr_cur)"
CR_NEXT="$(read_meta cr_next)"
CR_CROSS_MINOR="$(read_meta cr_cross_minor)"
KUEUE_CUR="$(read_meta kueue_cur)"
KUEUE_CROSS_MINOR="$(read_meta kueue_cross_minor)"
FIRST_CHART_NAME="$(read_meta first_chart_name)"
FIRST_CHART_VER="$(read_meta first_chart_ver)"
FIRST_CHART_VER_NEXT="$(read_meta first_chart_ver_next)"

FIXTURE_DISCOVERY="${SCRATCH}/fixture-discovery"
mkdir -p "${FIXTURE_DISCOVERY}"

cat > "${FIXTURE_DISCOVERY}/kubernetes-releases.json" <<JSON
[
  {"tag_name": "${K8S_REL_CROSS_MINOR}", "prerelease": false, "draft": false},
  {"tag_name": "${K8S_REL_NEXT}-rc.1", "prerelease": true, "draft": false},
  {"tag_name": "${K8S_REL_NEXT}", "prerelease": false, "draft": false},
  {"tag_name": "${K8S_REL_CUR}", "prerelease": false, "draft": false}
]
JSON

cat > "${FIXTURE_DISCOVERY}/helm-releases.json" <<JSON
[
  {"tag_name": "v4.0.0", "prerelease": false, "draft": false},
  {"tag_name": "v${HELM_NEXT}-rc.1", "prerelease": true, "draft": false},
  {"tag_name": "v${HELM_NEXT}", "prerelease": false, "draft": false},
  {"tag_name": "v${HELM_CUR}", "prerelease": false, "draft": false}
]
JSON

cat > "${FIXTURE_DISCOVERY}/kustomize-tags.txt" <<TXT
0000000000000000000000000000000000000001	refs/tags/kustomize/v${KUST_NEXT}
0000000000000000000000000000000000000002	refs/tags/kustomize/v${KUST_NEXT}-rc.1
0000000000000000000000000000000000000003	refs/tags/kustomize/v${KUST_CUR}
TXT

cat > "${FIXTURE_DISCOVERY}/skopeo-releases.json" <<JSON
[
  {"tag_name": "v${SKOPEO_CUR}", "prerelease": false, "draft": false}
]
JSON

cat > "${FIXTURE_DISCOVERY}/vals-releases.json" <<JSON
[
  {"tag_name": "v${VALS_NEXT}", "prerelease": false, "draft": false},
  {"tag_name": "v${VALS_CUR}", "prerelease": false, "draft": false}
]
JSON

cat > "${FIXTURE_DISCOVERY}/ytt-releases.json" <<JSON
[
  {"tag_name": "v${YTT_CUR}", "prerelease": false, "draft": false}
]
JSON

cat > "${FIXTURE_DISCOVERY}/controller-runtime-releases.json" <<JSON
[
  {"tag_name": "${CR_CROSS_MINOR}", "prerelease": false, "draft": false},
  {"tag_name": "${CR_NEXT}", "prerelease": false, "draft": false},
  {"tag_name": "${CR_CUR}", "prerelease": false, "draft": false}
]
JSON

cat > "${FIXTURE_DISCOVERY}/kueue-releases.json" <<JSON
[
  {"tag_name": "${KUEUE_CROSS_MINOR}", "prerelease": false, "draft": false},
  {"tag_name": "${KUEUE_CUR}", "prerelease": false, "draft": false}
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
python3 - "${STAGE_DISC}" "${GHA_DISC}" "${BASE_SHA}" "${FIXTURE_META}" <<'PY'
import json
import os
import sys

stage_dir, gha_path, base_sha, meta_path = sys.argv[1:5]
with open(meta_path, "r", encoding="utf-8") as f:
    meta = json.load(f)

with open(os.path.join(stage_dir, "upstream-records.json"), "r", encoding="utf-8") as f:
    recs = json.load(f)
assert isinstance(recs, list) and len(recs) == 8, recs
by_id = {r["upstreamId"]: r for r in recs}

assert by_id["kustomize-cli"]["version"] == meta["kust_next"], by_id["kustomize-cli"]
assert by_id["kustomize-cli"]["status"] == "available", by_id["kustomize-cli"]

assert by_id["helm-cli"]["version"] == f"v{meta['helm_next']}", by_id["helm-cli"]
assert by_id["helm-cli"]["status"] == "available", by_id["helm-cli"]

assert by_id["kubernetes"]["version"] == meta["k8s_rel_next"], by_id["kubernetes"]
assert by_id["kubernetes"]["status"] == "available", by_id["kubernetes"]

assert by_id["vals-cli"]["version"] == meta["vals_next"], by_id["vals-cli"]
assert by_id["vals-cli"]["status"] == "available", by_id["vals-cli"]

assert by_id["skopeo-cli"]["version"] == meta["skopeo_cur"], by_id["skopeo-cli"]
assert by_id["skopeo-cli"]["status"] == "up_to_date", by_id["skopeo-cli"]

assert by_id["ytt-cli"]["version"] == meta["ytt_cur"], by_id["ytt-cli"]
assert by_id["ytt-cli"]["status"] == "up_to_date", by_id["ytt-cli"]

assert by_id["controller-runtime"]["version"] == meta["cr_next"], by_id["controller-runtime"]
assert by_id["controller-runtime"]["status"] == "available", by_id["controller-runtime"]

assert by_id["kueue-sdk"]["version"] == meta["kueue_cur"], by_id["kueue-sdk"]
assert by_id["kueue-sdk"]["status"] == "up_to_date", by_id["kueue-sdk"]

with open(os.path.join(stage_dir, "k8s-deps-preflight-report.json"), "r", encoding="utf-8") as f:
    report = json.load(f)
assert report["k8s_minor"] == meta["k8s_minor"], report
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
    pass "preflight-k8s-deps.sh filters cross-minor k8s/helm v4, normalizes kustomize, and snapshots baselines"
  else
    fail "preflight-k8s-deps.sh discovery left dirty git tree"
  fi
else
  fail "preflight-k8s-deps.sh offline discovery fixture test"
fi

# Prepare shared immutable fixtures for concurrent subtests.
FIXTURE_STAGE="${SCRATCH}/fixture-stage"
fast_cp_dir "${FIXTURE_DISCOVERY}" "${FIXTURE_STAGE}"
mkdir -p "${FIXTURE_STAGE}/deps-cache/staging/k8s.io"

tar_pids=()
for mod_name in api apimachinery kube-aggregator apiextensions-apiserver; do
  (
    GZIP=-1 tar -zcf "${FIXTURE_STAGE}/deps-cache/staging/k8s.io/${mod_name}-${K8S_MOD_NEXT}.tar.gz" \
      --exclude='_VERSION_' \
      -C "${ROOT_DIR}/staging/k8s.io/${mod_name}" .
  ) &
  tar_pids+=("$!")
done
# Seed unpatched code-generator source once so hack/staging/k8s.io/code-generator/001_enhanced.patch applies cleanly.
(
  cg_tmp="${SCRATCH}/cg-unpatched"
  mkdir -p "${cg_tmp}"
  fast_cp_dir "${ROOT_DIR}/staging/k8s.io/code-generator/." "${cg_tmp}/"
  rm -f "${cg_tmp}/_VERSION_"
  (cd "${cg_tmp}" && patch -p1 -R --silent < "${ROOT_DIR}/hack/staging/k8s.io/code-generator/001_enhanced.patch")
  GZIP=-1 tar -zcf "${FIXTURE_STAGE}/deps-cache/staging/k8s.io/code-generator-${K8S_MOD_NEXT}.tar.gz" -C "${cg_tmp}" .
) &
tar_pids+=("$!")

for tpid in "${tar_pids[@]}"; do
  wait "${tpid}"
done

CAND_BASE="${SCRATCH}/cand-base"
mkdir -p "${CAND_BASE}/.auto-sync/scripts" "${CAND_BASE}/.auto-sync/checks" "${CAND_BASE}/.auto-sync/proposals"
copy_k8s_deps_tree "${ROOT_DIR}" "${CAND_BASE}"

FIXTURE_NOCHANGE="${SCRATCH}/fixture-nochange"
mkdir -p "${FIXTURE_NOCHANGE}"
cat > "${FIXTURE_NOCHANGE}/kubernetes-releases.json" <<JSON
[{"tag_name": "${K8S_REL_CUR}", "prerelease": false, "draft": false}]
JSON
cat > "${FIXTURE_NOCHANGE}/helm-releases.json" <<JSON
[{"tag_name": "v${HELM_CUR}", "prerelease": false, "draft": false}]
JSON
cat > "${FIXTURE_NOCHANGE}/kustomize-tags.txt" <<TXT
0000000000000000000000000000000000000003	refs/tags/kustomize/v${KUST_CUR}
TXT
cat > "${FIXTURE_NOCHANGE}/skopeo-releases.json" <<JSON
[{"tag_name": "v${SKOPEO_CUR}", "prerelease": false, "draft": false}]
JSON
cat > "${FIXTURE_NOCHANGE}/vals-releases.json" <<JSON
[{"tag_name": "v${VALS_CUR}", "prerelease": false, "draft": false}]
JSON
cat > "${FIXTURE_NOCHANGE}/ytt-releases.json" <<JSON
[{"tag_name": "v${YTT_CUR}", "prerelease": false, "draft": false}]
JSON
cat > "${FIXTURE_NOCHANGE}/controller-runtime-releases.json" <<JSON
[{"tag_name": "${CR_CUR}", "prerelease": false, "draft": false}]
JSON
cat > "${FIXTURE_NOCHANGE}/kueue-releases.json" <<JSON
[{"tag_name": "${KUEUE_CUR}", "prerelease": false, "draft": false}]
JSON

REAL_GO_BIN="$(command -v go)"

run_validator_env_i_isolated() {
  local cand_dir="$1"
  local stage_dir="$2"
  local case_root="$3"
  local home_dir="${case_root}/env-home"
  local tmp_dir="${case_root}/tmp"
  mkdir -p "${home_dir}" "${tmp_dir}"
  env -i \
    PATH=/usr/local/bin:/usr/bin:/bin \
    HOME="${home_dir}" \
    TMPDIR="${tmp_dir}" \
    GOMODCACHE="${HOST_GOMODCACHE}" \
    GOCACHE="${HOST_GOCACHE}" \
    AUTOSYNC_STAGE_DIR="${stage_dir}" \
    LOOPWRIGHT_CANDIDATE_DIR="${cand_dir}" \
    AUTOSYNC_FAST_SELFTEST=true \
    bash "${cand_dir}/.auto-sync/checks/validate_k8s_deps.sh"
}

RESULTS_DIR="${SCRATCH}/results"
mkdir -p "${RESULTS_DIR}"
CURRENT_CASE_RES=""
CASE_NAMES=()
CASE_PIDS=()
CASE_RCS=()
ACTIVE_PIDS=()
ACTIVE_INDICES=()
MAX_JOBS=4

record_pass() {
  printf 'PASS\t%s\n' "$1" >> "${CURRENT_CASE_RES}"
}

record_fail() {
  local msg="${1//$'\n'/ }"
  printf 'FAIL\t%s\n' "${msg}" >> "${CURRENT_CASE_RES}"
}

spawn_case() {
  local cid="$1"
  shift
  if (( ${#ACTIVE_PIDS[@]} >= MAX_JOBS )); then
    local reaped_idx="${ACTIVE_INDICES[0]}"
    local reaped_pid="${ACTIVE_PIDS[0]}"
    local reaped_rc=0
    wait "${reaped_pid}" || reaped_rc=$?
    CASE_RCS[reaped_idx]="${reaped_rc}"
    ACTIVE_PIDS=("${ACTIVE_PIDS[@]:1}")
    ACTIVE_INDICES=("${ACTIVE_INDICES[@]:1}")
  fi
  local next_idx="${#CASE_NAMES[@]}"
  CASE_NAMES+=("${cid}")
  (
    CURRENT_CASE_RES="${RESULTS_DIR}/${cid}.res"
    "$@"
  ) &
  local pid="$!"
  CASE_PIDS+=("${pid}")
  CASE_RCS+=("")
  ACTIVE_PIDS+=("${pid}")
  ACTIVE_INDICES+=("${next_idx}")
}

# 1b + 1c. Helm lockstep filter and HTTP retry / atomic cache tests.
case_1bc() {
  local helm_lockstep_repo="${SCRATCH}/helm-lockstep-repo"
  fast_cp_dir "${PREFLIGHT_REPO}" "${helm_lockstep_repo}"
  python3 - "${helm_lockstep_repo}" <<'PY'
import os
import re
import sys

repo = sys.argv[1]
df_p = os.path.join(repo, "pack/gpustack-operator/Dockerfile")
helm_sh_p = os.path.join(repo, "hack/lib/helm.sh")
gomod_p = os.path.join(repo, "go.mod")

with open(df_p, "r", encoding="utf-8") as f:
    df_txt = f.read()
df_txt = re.sub(r'^ARG HELM_VERSION="[^"]+"$', 'ARG HELM_VERSION="3.21.4"', df_txt, count=1, flags=re.MULTILINE)
with open(df_p, "w", encoding="utf-8") as f:
    f.write(df_txt)

with open(helm_sh_p, "r", encoding="utf-8") as f:
    h_txt = f.read()
h_txt = re.sub(r'default is v3\.\d+\.\d+\.', 'default is v3.21.4.', h_txt, count=1)
h_txt = re.sub(r'^helm_version=\$\{HELM_VERSION:-"v3\.\d+\.\d+"\}$', 'helm_version=${HELM_VERSION:-"v3.21.4"}', h_txt, count=1, flags=re.MULTILINE)
with open(helm_sh_p, "w", encoding="utf-8") as f:
    f.write(h_txt)

with open(gomod_p, "r", encoding="utf-8") as f:
    gm_txt = f.read()
gm_txt = re.sub(r'^(\s*helm\.sh/helm/v3\s+)v3\.\d+\.\d+', r'\g<1>v3.21.4', gm_txt, count=1, flags=re.MULTILINE)
with open(gomod_p, "w", encoding="utf-8") as f:
    f.write(gm_txt)
PY
  git -C "${helm_lockstep_repo}" add -A
  git -C "${helm_lockstep_repo}" -c gc.auto=0 commit -q --allow-empty -m "pin helm 3.21.4 for lockstep test"

  local fixture_helm_lockstep="${SCRATCH}/fixture-helm-lockstep"
  fast_cp_dir "${FIXTURE_DISCOVERY}" "${fixture_helm_lockstep}"
  cat > "${fixture_helm_lockstep}/helm-releases.json" <<'JSON'
[
  {"tag_name": "v4.0.0", "prerelease": false, "draft": false},
  {"tag_name": "v3.22.2", "prerelease": false, "draft": false},
  {"tag_name": "v3.22.1", "prerelease": false, "draft": false},
  {"tag_name": "v3.21.5", "prerelease": false, "draft": false},
  {"tag_name": "v3.21.4", "prerelease": false, "draft": false}
]
JSON
  cat > "${fixture_helm_lockstep}/helm-v3.22.2-go.mod" <<'GOMOD'
module helm.sh/helm/v3

go 1.24.0

require k8s.io/api v1.0.0
GOMOD
  cat > "${fixture_helm_lockstep}/helm-v3.22.1-go.mod" <<'GOMOD'
module helm.sh/helm/v3

go 1.24.0

require (
	k8s.io/api v0.35.0
	k8s.io/client-go v0.37.1 // max(api, client-go) must select 0.37
)
GOMOD
  cat > "${fixture_helm_lockstep}/helm-v3.21.5-go.mod" <<'GOMOD'
module helm.sh/helm/v3

go 1.24.0

exclude (
	k8s.io/api v0.37.9 // ignored outside require
)

replace k8s.io/client-go => k8s.io/client-go v0.37.9 // ignored outside require

require k8s.io/api v0.36.6 // indirect
require (
	k8s.io/client-go v0.36.6
)
GOMOD

  local stage_helm_lockstep="${SCRATCH}/stage-helm-lockstep/k8s-deps"
  local helm_lockstep_err="${SCRATCH}/helm-lockstep.stderr"
  if (
    cd "${helm_lockstep_repo}" && \
    AUTOSYNC_K8S_DEPS_FIXTURE_DIR="${fixture_helm_lockstep}" \
    AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
    AUTOSYNC_SKIP_SOURCE_REVISION=true \
    GOMODCACHE="${HOST_GOMODCACHE}" \
    GOCACHE="${HOST_GOCACHE}" \
    bash "${helm_lockstep_repo}/.auto-sync/scripts/preflight-k8s-deps.sh" "${stage_helm_lockstep}" >/dev/null 2>"${helm_lockstep_err}"
  ) && \
  python3 - "${stage_helm_lockstep}" "${helm_lockstep_err}" <<'PY'
import json
import os
import re
import sys

stage_dir, err_path = sys.argv[1:3]
with open(os.path.join(stage_dir, "upstream-records.json"), "r", encoding="utf-8") as f:
    recs = json.load(f)
by_id = {r["upstreamId"]: r for r in recs}
assert by_id["helm-cli"]["status"] == "available", by_id["helm-cli"]
assert by_id["helm-cli"]["version"] == "v3.21.5", by_id["helm-cli"]
assert "v3.22.1" not in by_id["helm-cli"]["recentVersions"], by_id["helm-cli"]

with open(os.path.join(stage_dir, "k8s-deps-preflight-report.json"), "r", encoding="utf-8") as f:
    rep = json.load(f)
skipped = {item["tag"]: item for item in rep.get("skipped_helm_candidates", [])}
assert skipped.get("v3.22.2", {}).get("reason") == "incompatible_k8s_minor", skipped
assert skipped.get("v3.22.1", {}).get("reason") == "incompatible_k8s_minor", skipped

with open(err_path, "r", encoding="utf-8") as f:
    err_txt = f.read()
assert "skipping Helm v3.22.2" in err_txt, err_txt
assert "skipping Helm v3.22.1" in err_txt, err_txt
assert not re.search(r"\.go:", err_txt), err_txt
PY
  then
    record_pass "preflight-k8s-deps.sh skips Helm v3.22.1 (k8s.io v0.37.1) and selects compatible Helm v3.21.5"
  else
    record_fail "preflight-k8s-deps.sh Helm k8s.io minor lockstep filter"
  fi

  local stage_helm_http="${SCRATCH}/stage-helm-http/k8s-deps"
  mkdir -p "${stage_helm_http}/discovery"
  printf '<html>stale poisoned cache</html>\n' > "${stage_helm_http}/discovery/helm-v3.21.5-go.mod"
  local helm_http_err="${SCRATCH}/helm-http.stderr"
  if python3 - "${helm_lockstep_repo}" "${fixture_helm_lockstep}" "${stage_helm_http}" "${helm_http_err}" "${HOST_GOMODCACHE}" "${HOST_GOCACHE}" <<'PY'
import http.server
import json
import os
import subprocess
import sys
import threading

repo_dir, fixture_dir, stage_dir, err_path, gomodcache, gocache = sys.argv[1:7]
counts = {}
lock = threading.Lock()


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        with lock:
            counts[self.path] = counts.get(self.path, 0) + 1
            n = counts[self.path]
        if self.path == "/v3.22.2/go.mod":
            self.send_response(503)
            self.end_headers()
            self.wfile.write(b"transient outage")
        elif self.path == "/v3.22.1/go.mod":
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"<html><body>502 Bad Gateway</body></html>")
        elif self.path == "/v3.21.5/go.mod":
            if n < 3:
                self.send_response(503)
                self.end_headers()
                self.wfile.write(b"retry me")
            else:
                self.send_response(200)
                self.end_headers()
                self.wfile.write(b"module helm.sh/helm/v3\n\nrequire k8s.io/client-go v0.36.6\n")
        else:
            self.send_response(404)
            self.end_headers()

    def log_message(self, fmt, *args):
        pass


srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
port = srv.server_address[1]
t = threading.Thread(target=srv.serve_forever, daemon=True)
t.start()
try:
    env = dict(os.environ)
    env["AUTOSYNC_K8S_DEPS_FIXTURE_DIR"] = fixture_dir
    env["AUTOSYNC_HELM_GOMOD_BASE_URL"] = f"http://127.0.0.1:{port}"
    env["AUTOSYNC_SKIP_GO_MOD_DOWNLOAD"] = "true"
    env["AUTOSYNC_SKIP_SOURCE_REVISION"] = "true"
    env["GOMODCACHE"] = gomodcache
    env["GOCACHE"] = gocache
    with open(err_path, "w", encoding="utf-8") as ef:
        res = subprocess.run(
            ["bash", os.path.join(repo_dir, ".auto-sync/scripts/preflight-k8s-deps.sh"), stage_dir],
            cwd=repo_dir,
            env=env,
            stdout=subprocess.DEVNULL,
            stderr=ef,
        )
    assert res.returncode == 0, f"preflight exited {res.returncode}"
finally:
    srv.shutdown()
    srv.server_close()

assert counts.get("/v3.22.2/go.mod") == 3, counts
assert counts.get("/v3.21.5/go.mod") == 3, counts
assert not os.path.exists(os.path.join(stage_dir, "discovery", "helm-v3.22.1-go.mod"))
assert not os.path.exists(os.path.join(stage_dir, "discovery", "helm-v3.22.2-go.mod"))
with open(os.path.join(stage_dir, "discovery", "helm-v3.21.5-go.mod"), "r", encoding="utf-8") as f:
    assert "k8s.io/client-go v0.36.6" in f.read()

with open(os.path.join(stage_dir, "k8s-deps-preflight-report.json"), "r", encoding="utf-8") as f:
    rep = json.load(f)
assert rep["target"]["helm_tag"] == "v3.21.5", rep["target"]
skipped = {item["tag"]: item for item in rep.get("skipped_helm_candidates", [])}
assert skipped.get("v3.22.2", {}).get("reason") == "fetch_error", skipped
assert skipped.get("v3.22.1", {}).get("reason") == "fetch_error", skipped

with open(err_path, "r", encoding="utf-8") as f:
    err_txt = f.read()
assert "failed to fetch Helm v3.22.2 go.mod after retries" in err_txt, err_txt
assert "failed to fetch Helm v3.22.1 go.mod after retries" in err_txt, err_txt
PY
  then
    record_pass "preflight-k8s-deps.sh retries transient Helm go.mod fetches, avoids cache poisoning, and records fetch_error"
  else
    record_fail "preflight-k8s-deps.sh Helm go.mod retry and atomic cache"
  fi
}

# 2. Offline preflight-k8s-deps.sh detached source_revision test via cached mod_staging tarballs.
case_2() {
  local repo_2="${SCRATCH}/repo-2"
  fast_cp_dir "${PREFLIGHT_REPO}" "${repo_2}"
  local stage_src_rev="${SCRATCH}/stage-srcrev/k8s-deps"
  local gha_src_rev="${SCRATCH}/gha-srcrev.txt"
  local tmp_2="${SCRATCH}/tmp-2"
  mkdir -p "${tmp_2}"
  : > "${gha_src_rev}"

  local src_rev=""
  if (
    cd "${repo_2}" && \
    TMPDIR="${tmp_2}" \
    AUTOSYNC_K8S_DEPS_FIXTURE_DIR="${FIXTURE_STAGE}" \
    AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
    AUTOSYNC_SKIP_GENERATE=true \
    GOMODCACHE="${HOST_GOMODCACHE}" \
    GOCACHE="${HOST_GOCACHE}" \
    GITHUB_OUTPUT="${gha_src_rev}" \
    bash "${repo_2}/.auto-sync/scripts/preflight-k8s-deps.sh" "${stage_src_rev}" >/dev/null
  ) && \
  src_rev="$(python3 - "${stage_src_rev}" "${BASE_SHA}" <<'PY'
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
  [[ -n "${src_rev}" && "${src_rev}" != "${BASE_SHA}" ]] && \
  grep -q "kubernetes/api ${K8S_MOD_NEXT}" <<<"$(git -C "${repo_2}" show "${src_rev}:hack/deps.sh")" && \
  [[ "$(git -C "${repo_2}" show "${src_rev}:staging/k8s.io/api/_VERSION_")" == "${K8S_MOD_NEXT}" ]] && \
  grep -q '100644 blob .*\.auto-sync/\.stage/k8s-deps/k8s-deps-preflight-report\.json$' <<<"$(git -C "${repo_2}" ls-tree -r "${src_rev}" .auto-sync/.stage/k8s-deps)" && \
  grep -q '100644 blob .*\.auto-sync/\.stage/k8s-deps/upstream-records\.json$' <<<"$(git -C "${repo_2}" ls-tree -r "${src_rev}" .auto-sync/.stage/k8s-deps)" && \
  ! grep -q '^120000 ' <<<"$(git -C "${repo_2}" ls-tree -r "${src_rev}" .auto-sync/.stage/k8s-deps)" && \
  [[ -z "$(git -C "${repo_2}" status --porcelain)" ]] && \
  [[ "$(git -C "${repo_2}" rev-parse HEAD)" == "${BASE_SHA}" ]] && \
  [[ "$(git -C "${repo_2}" rev-parse refs/heads/main)" == "${BASE_SHA}" ]]; then
    record_pass "preflight-k8s-deps.sh stages mod_staging and .auto-sync/.stage/k8s-deps onto detached source_revision while preserving main"
  else
    record_fail "preflight-k8s-deps.sh detached source_revision staging"
  fi
}

# 2b. Uncommitted local edit in repo_root survives preflight-k8s-deps.sh untouched.
case_2b() {
  local repo_2b="${SCRATCH}/repo-2b"
  fast_cp_dir "${PREFLIGHT_REPO}" "${repo_2b}"
  printf '// local uncommitted edit\n' >> "${repo_2b}/go.mod"
  local stage_dirty="${SCRATCH}/stage-dirty/k8s-deps"
  local tmp_2b="${SCRATCH}/tmp-2b"
  mkdir -p "${tmp_2b}"
  if (
    cd "${repo_2b}" && \
    TMPDIR="${tmp_2b}" \
    AUTOSYNC_K8S_DEPS_FIXTURE_DIR="${FIXTURE_STAGE}" \
    AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
    AUTOSYNC_SKIP_GENERATE=true \
    GOMODCACHE="${HOST_GOMODCACHE}" \
    GOCACHE="${HOST_GOCACHE}" \
    bash "${repo_2b}/.auto-sync/scripts/preflight-k8s-deps.sh" "${stage_dirty}" >/dev/null
  ) && \
  grep -q '// local uncommitted edit' "${repo_2b}/go.mod"; then
    record_pass "preflight-k8s-deps.sh preserves uncommitted local edits in repo_root"
  else
    record_fail "preflight-k8s-deps.sh preserves uncommitted local edits in repo_root"
  fi
}

# 2c. Partial upstream tag release / go mod tidy failure degrades gracefully without crashing.
case_2c() {
  local repo_2c="${SCRATCH}/repo-2c"
  fast_cp_dir "${PREFLIGHT_REPO}" "${repo_2c}"
  local wrap_go_bin="${SCRATCH}/wrap-go-bin"
  local tmp_2c="${SCRATCH}/tmp-2c"
  mkdir -p "${wrap_go_bin}" "${tmp_2c}"
  cat > "${wrap_go_bin}/go" <<'SH'
#!/usr/bin/env bash
if [[ "${1:-}" == "mod" && "${2:-}" == "tidy" ]]; then
  echo "api/v1/zz_generated.apiservice.go:17:2: simulated go mod tidy failure: unknown revision" >&2
  echo "open api/v1/doc.go: simulated error" >&2
  echo "::error::simulated workflow command" >&2
  echo "::warning file=x.go,line=1::simulated warning" >&2
  echo "::add-mask::secret" >&2
  echo "##[error]simulated azure matcher" >&2
  echo "##[warning]simulated azure warning" >&2
  echo "##[group]simulated azure group" >&2
  exit 1
fi
exec "${REAL_GO_BIN}" "$@"
SH
  chmod +x "${wrap_go_bin}/go"
  local stage_fail_tidy="${SCRATCH}/stage-fail-tidy/k8s-deps"
  local fail_tidy_err="${SCRATCH}/fail-tidy.stderr"
  mkdir -p "${SCRATCH}/stage-fail-tidy/go-mod"
  : > "${SCRATCH}/stage-fail-tidy/go-mod/.warmed"
  if (
    cd "${repo_2c}" && \
    TMPDIR="${tmp_2c}" \
    REAL_GO_BIN="${REAL_GO_BIN}" \
    PATH="${wrap_go_bin}:${PATH}" \
    AUTOSYNC_K8S_DEPS_FIXTURE_DIR="${FIXTURE_STAGE}" \
    AUTOSYNC_SKIP_GENERATE=true \
    GOMODCACHE="${HOST_GOMODCACHE}" \
    GOCACHE="${HOST_GOCACHE}" \
    bash "${repo_2c}/.auto-sync/scripts/preflight-k8s-deps.sh" "${stage_fail_tidy}" >/dev/null 2>"${fail_tidy_err}"
  ) && \
  python3 - "${stage_fail_tidy}" "${fail_tidy_err}" "${BASE_SHA}" "${FIXTURE_META}" <<'PY'
import json
import os
import re
import sys

stage_dir, err_path, base_sha, meta_path = sys.argv[1:5]
with open(meta_path, "r", encoding="utf-8") as f:
    meta = json.load(f)
with open(os.path.join(stage_dir, "k8s-deps-preflight-report.json"), "r", encoding="utf-8") as f:
    rep = json.load(f)
assert rep["patch_clean"] is False, rep
assert rep["staging_updated"] is False, rep
assert rep["source_revision"] != base_sha, rep
assert "go mod tidy/download failed" in rep.get("staging_error", ""), rep
for k in (
    "kubernetes",
    "k8s_modules",
    "kubectl_cli",
    "controller_runtime",
    "kueue_sdk",
    "helm_cli",
    "helm_tag",
    "helm_module",
):
    assert rep["target"][k] == rep["current"][k], (k, rep["target"], rep["current"])
assert rep["target"]["vals_cli"] == meta["vals_next"], rep["target"]
assert rep.get("failed_target", {}).get("kubernetes") == meta["k8s_rel_next"], rep
held_back = rep.get("held_back_upstreams") or {}
assert held_back.get("kubernetes", {}).get("candidate") == meta["k8s_rel_next"], held_back
assert held_back["kubernetes"].get("reason"), held_back

with open(os.path.join(stage_dir, "upstream-records.json"), "r", encoding="utf-8") as f:
    recs = json.load(f)
by_id = {r["upstreamId"]: r for r in recs}
for uid, cur_v in (
    ("kubernetes", meta["k8s_rel_cur"]),
    ("controller-runtime", meta["cr_cur"]),
    ("kueue-sdk", meta["kueue_cur"]),
    ("helm-cli", f"v{meta['helm_cur']}"),
):
    assert by_id[uid]["status"] == "up_to_date", by_id[uid]
    assert by_id[uid]["version"] == cur_v, by_id[uid]
assert by_id["vals-cli"]["status"] == "available", by_id["vals-cli"]
assert by_id["kustomize-cli"]["status"] == "available", by_id["kustomize-cli"]

with open(err_path, "r", encoding="utf-8") as f:
    err_txt = f.read()
assert not re.search(r"\.go:", err_txt), err_txt
for marker in ("::error", "::warning", "::add-mask", "##[error]", "##[warning]", "##[group]"):
    assert marker not in err_txt.lower(), (marker, err_txt)
assert ".go@17:2" in err_txt, err_txt
assert "open api/v1/doc.go@ simulated error" in err_txt, err_txt
PY
  then
    record_pass "preflight-k8s-deps.sh sanitizes .go:<line> errors, holds Go modules up_to_date on tidy failure"
  else
    record_fail "preflight-k8s-deps.sh degrades gracefully when go mod tidy fails"
  fi
}

# 2d. When staging fails with both k8s and helm bumped, retry without helm bump resets work_repo completely and succeeds.
case_2d() {
  local repo_2d="${SCRATCH}/repo-2d"
  fast_cp_dir "${PREFLIGHT_REPO}" "${repo_2d}"
  local wrap_go_helm_fail="${SCRATCH}/wrap-go-helm-fail"
  local tmp_2d="${SCRATCH}/tmp-2d"
  mkdir -p "${wrap_go_helm_fail}" "${tmp_2d}"
  cat > "${wrap_go_helm_fail}/go" <<'SH'
#!/usr/bin/env bash
if [[ "${1:-}" == "mod" && ( "${2:-}" == "tidy" || "${2:-}" == "download" ) ]]; then
  if [[ -f "go.mod" ]] && grep -q "helm.sh/helm/v3 v${FAIL_HELM_NEXT}" go.mod; then
    mkdir -p staging/k8s.io/api
    printf 'stale mutated state from attempt 1\n' > staging/k8s.io/api/dirty_from_attempt1.txt
    echo "simulated helm bump failure for v${FAIL_HELM_NEXT}" >&2
    exit 1
  fi
  exit 0
fi
exec "${REAL_GO_BIN}" "$@"
SH
  chmod +x "${wrap_go_helm_fail}/go"
  local stage_helm_fb="${SCRATCH}/stage-helm-fb/k8s-deps"
  mkdir -p "${SCRATCH}/stage-helm-fb/go-mod"
  : > "${SCRATCH}/stage-helm-fb/go-mod/.warmed"
  local fb_rev=""
  if (
    cd "${repo_2d}" && \
    TMPDIR="${tmp_2d}" \
    REAL_GO_BIN="${REAL_GO_BIN}" \
    FAIL_HELM_NEXT="${HELM_NEXT}" \
    PATH="${wrap_go_helm_fail}:${PATH}" \
    AUTOSYNC_K8S_DEPS_FIXTURE_DIR="${FIXTURE_STAGE}" \
    AUTOSYNC_SKIP_GENERATE=true \
    GOMODCACHE="${HOST_GOMODCACHE}" \
    GOCACHE="${HOST_GOCACHE}" \
    bash "${repo_2d}/.auto-sync/scripts/preflight-k8s-deps.sh" "${stage_helm_fb}" >/dev/null 2>&1
  ) && \
  fb_rev="$(python3 - "${stage_helm_fb}" "${BASE_SHA}" "${FIXTURE_META}" <<'PY'
import json
import os
import sys

stage_dir, base_sha, meta_path = sys.argv[1:4]
with open(meta_path, "r", encoding="utf-8") as f:
    meta = json.load(f)
with open(os.path.join(stage_dir, "k8s-deps-preflight-report.json"), "r", encoding="utf-8") as f:
    rep = json.load(f)
assert rep["patch_clean"] is True, rep
assert rep["staging_updated"] is True, rep
assert rep["source_revision"] != base_sha, rep
assert rep["target"]["helm_tag"] == f"v{meta['helm_cur']}", rep["target"]
held_back = rep.get("held_back_upstreams") or {}
assert held_back.get("helm-cli", {}).get("candidate") == f"v{meta['helm_next']}", held_back

with open(os.path.join(stage_dir, "upstream-records.json"), "r", encoding="utf-8") as f:
    recs = json.load(f)
by_id = {r["upstreamId"]: r for r in recs}
assert by_id["kubernetes"]["status"] == "available", by_id["kubernetes"]
assert by_id["kubernetes"]["version"] == meta["k8s_rel_next"], by_id["kubernetes"]
assert by_id["helm-cli"]["status"] == "up_to_date", by_id["helm-cli"]
assert by_id["helm-cli"]["version"] == f"v{meta['helm_cur']}", by_id["helm-cli"]
print(rep["source_revision"])
PY
  )" && \
  grep -q "k8s.io/api ${K8S_MOD_NEXT}" <<<"$(git -C "${repo_2d}" show "${fb_rev}:go.mod")" && \
  grep -q "helm.sh/helm/v3 v${HELM_CUR}" <<<"$(git -C "${repo_2d}" show "${fb_rev}:go.mod")" && \
  ! git -C "${repo_2d}" cat-file -e "${fb_rev}:staging/k8s.io/api/dirty_from_attempt1.txt" 2>/dev/null; then
    record_pass "preflight-k8s-deps.sh retries without helm bump and stages k8s update cleanly"
  else
    record_fail "preflight-k8s-deps.sh helm fallback retry"
  fi
}

# 2e. Gitless work_repo copy passes hack/check/agents-shell.sh after ensure_ephemeral_git and cleans up .git.
case_2e() {
  local work_gitless="${SCRATCH}/work-repo-gitless/gpustack.ai/gpustack"
  mkdir -p "${SCRATCH}/work-repo-gitless/gpustack.ai"
  fast_cp_dir "${CAND_BASE}" "${work_gitless}"
  local sc_for_work="${SHELLCHECK_BIN:-}"
  if [[ -z "${sc_for_work}" && -x "${ROOT_DIR}/.sbin/shellcheck" ]]; then
    sc_for_work="${ROOT_DIR}/.sbin/shellcheck"
  fi
  if [[ -z "${sc_for_work}" ]]; then
    sc_for_work="$(type -P shellcheck 2>/dev/null || true)"
  fi
  if [[ -z "${sc_for_work}" ]]; then
    sc_for_work="${SCRATCH}/stub-shellcheck.sh"
    printf '#!/usr/bin/env bash\nexit 0\n' > "${sc_for_work}"
    chmod +x "${sc_for_work}"
  fi
  local pre_rc=0
  SHELLCHECK_BIN="${sc_for_work}" bash "${ROOT_DIR}/hack/check/agents-shell.sh" "${work_gitless}" >/dev/null 2>&1 || pre_rc=$?
  local ensure_rc=0
  (
    # shellcheck source=lib.sh
    # shellcheck disable=SC1091
    source "${ROOT_DIR}/.auto-sync/checks/lib.sh"
    autosync::ensure_ephemeral_git "${work_gitless}"
  ) || ensure_rc=$?
  local post_rc=0
  SHELLCHECK_BIN="${sc_for_work}" bash "${ROOT_DIR}/hack/check/agents-shell.sh" "${work_gitless}" >/dev/null 2>&1 || post_rc=$?
  local cleanup_rc=0
  (
    # shellcheck source=lib.sh
    # shellcheck disable=SC1091
    source "${ROOT_DIR}/.auto-sync/checks/lib.sh"
    autosync::cleanup_ephemeral_git "${work_gitless}"
  ) || cleanup_rc=$?
  # shellcheck disable=SC2016
  if (( pre_rc == 2 && ensure_rc == 0 && post_rc == 0 && cleanup_rc == 0 )) && [[ ! -e "${work_gitless}/.git" ]] && \
     grep -qF 'autosync::ensure_ephemeral_git "${work_repo}"' "${PREFLIGHT_SCRIPT}" && \
     grep -qF 'autosync::cleanup_ephemeral_git "${work_repo}"' "${PREFLIGHT_SCRIPT}"; then
    record_pass "ensure_ephemeral_git on gitless work_repo copy lets agents-shell.sh succeed (rc=0) and cleans up .git"
  else
    record_fail "ensure_ephemeral_git on gitless work_repo copy (pre_rc=${pre_rc}, ensure_rc=${ensure_rc}, post_rc=${post_rc}, cleanup_rc=${cleanup_rc})"
  fi
}

# 3a. Positive case: bumping VALS_VERSION in pack/gpustack-operator/Dockerfile.
case_3a() {
  local cand_pos="${SCRATCH}/cand-pos"
  fast_cp_dir "${CAND_BASE}" "${cand_pos}"
  python3 - "${CAND_BASE}/pack/gpustack-operator/Dockerfile" "${cand_pos}/pack/gpustack-operator/Dockerfile" "${VALS_CUR}" "${VALS_NEXT}" <<'PY'
import re
import sys
src, dst, v_cur, v_next = sys.argv[1:5]
with open(src, "r", encoding="utf-8") as f:
    txt = f.read()
out = re.sub(rf'^ARG VALS_VERSION="{re.escape(v_cur)}"$', f'ARG VALS_VERSION="{v_next}"', txt, count=1, flags=re.MULTILINE)
assert out != txt, "failed to bump VALS_VERSION"
with open(dst, "w", encoding="utf-8") as f:
    f.write(out)
PY
  python3 - "${cand_pos}/.auto-sync/proposals/k8s-deps.json" "${VALS_NEXT}" <<'PY'
import json
import sys

p, v_next = sys.argv[1:3]
with open(p, "r", encoding="utf-8") as f:
    doc = json.load(f)
doc["versions"]["vals-cli"] = v_next
with open(p, "w", encoding="utf-8") as f:
    json.dump(doc, f, indent=2)
    f.write("\n")
PY
  if run_validator_env_i_isolated "${cand_pos}" "${STAGE_DISC}" "${SCRATCH}/case-3a" >/dev/null 2>&1 && \
     [[ ! -e "${cand_pos}/.git" ]] && [[ ! -e "${cand_pos}/.sbin" ]]; then
    record_pass "validate_k8s_deps.sh positive VALS_VERSION bump passes guards, upgrade-compat, and toolpins-selftest"
  else
    record_fail "validate_k8s_deps.sh positive VALS_VERSION bump"
  fi
}

# 3b. Negative case 1: mutating LIB_HAMI_CORE_COMMIT in pack/gpustack-operator/Dockerfile.
case_3b() {
  local cand_neg1="${SCRATCH}/cand-neg1"
  fast_cp_dir "${CAND_BASE}" "${cand_neg1}"
  sed 's/^ARG LIB_HAMI_CORE_COMMIT="[^"]*"$/ARG LIB_HAMI_CORE_COMMIT="1111111111111111111111111111111111111111"/' \
    "${CAND_BASE}/pack/gpustack-operator/Dockerfile" > "${cand_neg1}/pack/gpustack-operator/Dockerfile"
  if cmp -s "${CAND_BASE}/pack/gpustack-operator/Dockerfile" "${cand_neg1}/pack/gpustack-operator/Dockerfile"; then
    record_fail "negative test 3b failed to mutate LIB_HAMI_CORE_COMMIT"
    return
  fi
  local neg1_err=""
  if ! neg1_err="$(run_validator_env_i_isolated "${cand_neg1}" "${STAGE_DISC}" "${SCRATCH}/case-3b" 2>&1)" && \
     [[ ! -e "${cand_neg1}/.git" ]]; then
    record_pass "validate_k8s_deps.sh fails closed on LIB_HAMI_CORE_COMMIT mutation in Dockerfile"
  else
    record_fail "validate_k8s_deps.sh fails closed on LIB_HAMI_CORE_COMMIT mutation (${neg1_err})"
  fi
}

# 3c. Negative case 2: mutating chart_staging() in hack/deps.sh.
case_3c() {
  local cand_neg2="${SCRATCH}/cand-neg2"
  fast_cp_dir "${CAND_BASE}" "${cand_neg2}"
  python3 - "${CAND_BASE}/hack/deps.sh" "${cand_neg2}/hack/deps.sh" "${FIRST_CHART_NAME}" "${FIRST_CHART_VER}" "${FIRST_CHART_VER_NEXT}" <<'PY'
import sys
src, dst, cname, cver, cnext = sys.argv[1:6]
with open(src, "r", encoding="utf-8") as f:
    txt = f.read()
old_tok = f" {cver} ${{charts_dir}}/{cname}"
new_tok = f" {cnext} ${{charts_dir}}/{cname}"
out = txt.replace(old_tok, new_tok, 1)
assert out != txt, f"failed to mutate {old_tok} in hack/deps.sh"
with open(dst, "w", encoding="utf-8") as f:
    f.write(out)
PY
  local neg2_err=""
  if ! neg2_err="$(run_validator_env_i_isolated "${cand_neg2}" "${STAGE_DISC}" "${SCRATCH}/case-3c" 2>&1)" && \
     [[ ! -e "${cand_neg2}/.git" ]]; then
    record_pass "validate_k8s_deps.sh fails closed on chart_staging() mutation in hack/deps.sh"
  else
    record_fail "validate_k8s_deps.sh fails closed on chart_staging() mutation (${neg2_err})"
  fi
}

# 3d. Negative case 3: modifying a hand-written .go file under api/v1/.
case_3d() {
  local cand_neg3="${SCRATCH}/cand-neg3"
  fast_cp_dir "${CAND_BASE}" "${cand_neg3}"
  local hand_api_file
  hand_api_file="$(find "${cand_neg3}/api/v1" -maxdepth 1 -type f -name '*.go' ! -name 'zz_generated*' ! -name 'generated.*' | sort | head -n 1)"
  if [[ -z "${hand_api_file}" ]]; then
    record_fail "unable to locate hand-written api/v1/*.go file for negative test"
    return
  fi
  printf '\n// illegal hand-written api edit\n' >> "${hand_api_file}"
  local neg3_err=""
  if ! neg3_err="$(run_validator_env_i_isolated "${cand_neg3}" "${STAGE_DISC}" "${SCRATCH}/case-3d" 2>&1)" && \
     [[ ! -e "${cand_neg3}/.git" ]]; then
    record_pass "validate_k8s_deps.sh fails closed on hand-written api/v1/*.go edit"
  else
    record_fail "validate_k8s_deps.sh fails closed on hand-written api/v1/*.go edit (${neg3_err})"
  fi
}

# 3e. Negative case 4: bumping HELM_VERSION in Dockerfile without updating hack/lib/helm.sh.
case_3e() {
  local cand_neg4="${SCRATCH}/cand-neg4"
  fast_cp_dir "${CAND_BASE}" "${cand_neg4}"
  python3 - "${CAND_BASE}/pack/gpustack-operator/Dockerfile" "${cand_neg4}/pack/gpustack-operator/Dockerfile" "${HELM_CUR}" "${HELM_NEXT}" <<'PY'
import re
import sys
src, dst, h_cur, h_next = sys.argv[1:5]
with open(src, "r", encoding="utf-8") as f:
    txt = f.read()
out = re.sub(rf'^ARG HELM_VERSION="{re.escape(h_cur)}"$', f'ARG HELM_VERSION="{h_next}"', txt, count=1, flags=re.MULTILINE)
assert out != txt, f"failed to mutate HELM_VERSION from {h_cur}"
with open(dst, "w", encoding="utf-8") as f:
    f.write(out)
PY
  local neg4_err=""
  if ! neg4_err="$(run_validator_env_i_isolated "${cand_neg4}" "${STAGE_DISC}" "${SCRATCH}/case-3e" 2>&1)" && \
     [[ ! -e "${cand_neg4}/.git" ]]; then
    record_pass "validate_k8s_deps.sh fails closed on unaligned HELM_VERSION bump in Dockerfile"
  else
    record_fail "validate_k8s_deps.sh fails closed on unaligned HELM_VERSION bump (${neg4_err})"
  fi
}

# 3f. Negative case 5: cross-minor K8s jump (k8s.io/api in go.mod) fails closed.
case_3f() {
  local cand_neg5="${SCRATCH}/cand-neg5"
  fast_cp_dir "${CAND_BASE}" "${cand_neg5}"
  python3 - "${CAND_BASE}/go.mod" "${cand_neg5}/go.mod" "${K8S_MOD_CUR}" "${K8S_MOD_CROSS_MINOR}" <<'PY'
import sys
src, dst, k_cur, k_cross = sys.argv[1:5]
with open(src, "r", encoding="utf-8") as f:
    txt = f.read()
out = txt.replace(f"k8s.io/api {k_cur}", f"k8s.io/api {k_cross}", 1)
assert out != txt, f"failed to mutate k8s.io/api {k_cur}"
with open(dst, "w", encoding="utf-8") as f:
    f.write(out)
PY
  local neg5_err=""
  if ! neg5_err="$(run_validator_env_i_isolated "${cand_neg5}" "${STAGE_DISC}" "${SCRATCH}/case-3f" 2>&1)" && \
     [[ ! -e "${cand_neg5}/.git" ]]; then
    record_pass "validate_k8s_deps.sh fails closed on cross-minor k8s.io/api jump"
  else
    record_fail "validate_k8s_deps.sh fails closed on cross-minor k8s.io/api jump (${neg5_err})"
  fi
}

# 3g. Negative case 6: mismatched proposals/k8s-deps.json fails closed.
case_3g() {
  local cand_neg6="${SCRATCH}/cand-neg6"
  fast_cp_dir "${CAND_BASE}" "${cand_neg6}"
  python3 - "${CAND_BASE}/.auto-sync/proposals/k8s-deps.json" "${cand_neg6}/.auto-sync/proposals/k8s-deps.json" <<'PY'
import json
import sys
src, dst = sys.argv[1:3]
with open(src, "r", encoding="utf-8") as f:
    doc = json.load(f)
old_v = doc["versions"]["helm-cli"]
new_v = old_v.lstrip("v") if old_v.startswith("v") else f"v{old_v}"
assert new_v != old_v
doc["versions"]["helm-cli"] = new_v
with open(dst, "w", encoding="utf-8") as f:
    json.dump(doc, f, indent=2)
    f.write("\n")
PY
  local neg6_err=""
  if ! neg6_err="$(run_validator_env_i_isolated "${cand_neg6}" "${STAGE_DISC}" "${SCRATCH}/case-3g" 2>&1)" && \
     [[ ! -e "${cand_neg6}/.git" ]]; then
    record_pass "validate_k8s_deps.sh fails closed on mismatched proposals/k8s-deps.json"
  else
    record_fail "validate_k8s_deps.sh fails closed on mismatched proposals/k8s-deps.json (${neg6_err})"
  fi
}

# 3h. Negative cases 7 & 8: mismatched k8s-modules / extra key in proposals/k8s-deps.json fail closed.
case_3h1() {
  local cand_neg7="${SCRATCH}/cand-neg7"
  fast_cp_dir "${CAND_BASE}" "${cand_neg7}"
  python3 - "${CAND_BASE}/.auto-sync/proposals/k8s-deps.json" "${cand_neg7}/.auto-sync/proposals/k8s-deps.json" <<'PY'
import json
import sys
src, dst = sys.argv[1:3]
with open(src, "r", encoding="utf-8") as f:
    doc = json.load(f)
doc["versions"]["k8s-modules"] = "v0.0.0"
with open(dst, "w", encoding="utf-8") as f:
    json.dump(doc, f, indent=2)
    f.write("\n")
PY
  local neg7_err=""
  if ! neg7_err="$(run_validator_env_i_isolated "${cand_neg7}" "${STAGE_DISC}" "${SCRATCH}/case-3h1" 2>&1)" && \
     grep -q "k8s-modules" <<<"${neg7_err}"; then
    record_pass "validate_k8s_deps.sh fails closed on mismatched proposals/k8s-deps.json k8s-modules"
  else
    record_fail "validate_k8s_deps.sh fails closed on mismatched proposals/k8s-deps.json k8s-modules (${neg7_err})"
  fi
}

case_3h2() {
  local cand_neg8="${SCRATCH}/cand-neg8"
  fast_cp_dir "${CAND_BASE}" "${cand_neg8}"
  python3 - "${CAND_BASE}/.auto-sync/proposals/k8s-deps.json" "${cand_neg8}/.auto-sync/proposals/k8s-deps.json" <<'PY'
import json
import sys
src, dst = sys.argv[1:3]
with open(src, "r", encoding="utf-8") as f:
    doc = json.load(f)
doc["versions"]["extra-key"] = "1.0.0"
with open(dst, "w", encoding="utf-8") as f:
    json.dump(doc, f, indent=2)
    f.write("\n")
PY
  local neg8_err=""
  if ! neg8_err="$(run_validator_env_i_isolated "${cand_neg8}" "${STAGE_DISC}" "${SCRATCH}/case-3h2" 2>&1)" && \
     grep -q "extra-key" <<<"${neg8_err}"; then
    record_pass "validate_k8s_deps.sh fails closed on extra unexpected key in proposals/k8s-deps.json"
  else
    record_fail "validate_k8s_deps.sh fails closed on extra unexpected key in proposals/k8s-deps.json (${neg8_err})"
  fi
}

# 3i. no_change fixture keeps source_revision == base_sha.
case_3i() {
  local repo_3i="${SCRATCH}/repo-3i"
  fast_cp_dir "${PREFLIGHT_REPO}" "${repo_3i}"
  local stage_nochange="${SCRATCH}/stage-nochange/k8s-deps"
  if (
    cd "${repo_3i}" && \
    AUTOSYNC_K8S_DEPS_FIXTURE_DIR="${FIXTURE_NOCHANGE}" \
    AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
    GOMODCACHE="${HOST_GOMODCACHE}" \
    GOCACHE="${HOST_GOCACHE}" \
    bash "${repo_3i}/.auto-sync/scripts/preflight-k8s-deps.sh" "${stage_nochange}" >/dev/null
  ) && \
  python3 - "${stage_nochange}" "${BASE_SHA}" <<'PY'
import json
import os
import sys

stage_dir, base_sha = sys.argv[1:3]
with open(os.path.join(stage_dir, "upstream-records.json"), "r", encoding="utf-8") as f:
    recs = json.load(f)
assert all(r["status"] == "up_to_date" for r in recs), recs
with open(os.path.join(stage_dir, "k8s-deps-preflight-report.json"), "r", encoding="utf-8") as f:
    rep = json.load(f)
assert rep["source_revision"] == base_sha, rep
PY
  then
    record_pass "preflight-k8s-deps.sh keeps source_revision == base_sha when all upstreams are up_to_date"
  else
    record_fail "preflight-k8s-deps.sh keeps source_revision == base_sha when all upstreams are up_to_date"
  fi
}

# 3j. Regression coverage for offline uv-cache + uv-python + module-path copy failure safety + gc.auto=0.
case_3j() {
  local reg_stage="${SCRATCH}/reg-stage/k8s-deps"
  local reg_cand="${SCRATCH}/reg-cand"
  local reg_fakebin="${SCRATCH}/reg-fakebin"
  local reg_empty_home="${SCRATCH}/reg-empty-home"
  mkdir -p "${reg_stage}/.sbin" "${reg_stage}/uv-cache" "${reg_stage}/uv-python/cpython-3.11" "${reg_cand}/go.mod.dir" "${reg_fakebin}" "${reg_empty_home}"
  printf 'cached-wheel\n' > "${reg_stage}/uv-cache/wheel.whl"
  printf 'python-3.11\n' > "${reg_stage}/uv-python/cpython-3.11/python"
  local reg_tmp="${SCRATCH}/reg-tmp"
  mkdir -p "${reg_tmp}"
  if [[ "$(id -u)" -ne 0 ]]; then
    chmod -R a-w "${reg_stage}/uv-cache" "${reg_stage}/uv-python"
    # shellcheck disable=SC2016
    if env -i PATH=/usr/local/bin:/usr/bin:/bin HOME="${reg_empty_home}" TMPDIR="${reg_tmp}" bash -c '
      source "$1/.auto-sync/checks/lib.sh"
      autosync::bootstrap_env "$2" "$3"
      [[ -n "${UV_CACHE_DIR:-}" && "${UV_CACHE_DIR}" != "$2/uv-cache" && -w "${UV_CACHE_DIR}" ]]
      [[ -f "${UV_CACHE_DIR}/wheel.whl" && -w "${UV_CACHE_DIR}/wheel.whl" && "${UV_OFFLINE:-}" == "1" ]]
      [[ -n "${UV_PYTHON_INSTALL_DIR:-}" && "${UV_PYTHON_INSTALL_DIR}" != "$2/uv-python" && -w "${UV_PYTHON_INSTALL_DIR}" ]]
      [[ -f "${UV_PYTHON_INSTALL_DIR}/cpython-3.11/python" && "${UV_PYTHON_PREFERENCE:-}" == "only-managed" ]]
    ' _ "${ROOT_DIR}" "${reg_stage}" "${reg_cand}"; then
      record_pass "bootstrap_env copies read-only uv-cache and uv-python writable under empty HOME"
    else
      record_fail "bootstrap_env copies read-only uv-cache and uv-python writable under empty HOME"
    fi
    chmod -R u+w "${reg_stage}/uv-cache" "${reg_stage}/uv-python" 2>/dev/null || true
  else
    # root ignores permission bits; read-only fallback cannot be exercised
    chmod -R u+w "${reg_stage}/uv-cache" "${reg_stage}/uv-python" 2>/dev/null || true
    record_pass "bootstrap_env read-only uv-cache and uv-python fallback skipped as root"
  fi

  printf 'keep-me\n' > "${reg_cand}/sentinel.txt"
  local reg_mod_rc=0
  local reg_mod_err=""
  # shellcheck disable=SC2016
  reg_mod_err="$(TMPDIR="${reg_tmp}" bash -c '
    source "$1"
    autosync::ensure_ephemeral_git "$2"
    [[ "$(git -C "$2" config --get gc.auto || true)" == "0" ]]
    autosync::run_in_module_path_copy "$2" bash -c "rm -f sentinel.txt; exit 9"
  ' _ "${ROOT_DIR}/.auto-sync/checks/lib.sh" "${reg_cand}" 2>&1)" || reg_mod_rc=$?
  if (( reg_mod_rc == 9 )) && [[ -f "${reg_cand}/sentinel.txt" ]] && ! grep -q "unbound variable" <<<"${reg_mod_err}"; then
    record_pass "run_in_module_path_copy preserves target files on command failure and sets gc.auto=0"
  else
    record_fail "run_in_module_path_copy preserves target files on command failure (rc=${reg_mod_rc}, err=${reg_mod_err})"
  fi

  local uv_fail_stage="${SCRATCH}/uv-fail-stage"
  local uv_fail_repo="${SCRATCH}/uv-fail-repo"
  mkdir -p "${uv_fail_stage}" "${uv_fail_repo}/model-transfer/python"
  cat > "${reg_fakebin}/uv" <<'SH'
#!/usr/bin/env bash
if [[ -n "${UV_CACHE_DIR:-}" ]]; then
  mkdir -p "${UV_CACHE_DIR}"
  : > "${UV_CACHE_DIR}/CACHEDIR.TAG"
fi
if [[ -n "${UV_PYTHON_INSTALL_DIR:-}" ]]; then
  mkdir -p "${UV_PYTHON_INSTALL_DIR}"
  : > "${UV_PYTHON_INSTALL_DIR}/partial-python"
fi
exit 1
SH
  chmod +x "${reg_fakebin}/uv"
  # shellcheck disable=SC2016
  if bash -c '
    export PATH="$4:${PATH}"
    cd "$1"
    git init -q
    source "$2/.auto-sync/scripts/lib-preflight.sh"
    export GOPROXY="https://proxy.golang.org,direct"
    autosync::preflight::warm_caches "$3" --go-all >/dev/null 2>&1
    [[ -z "$(ls -A "$3/uv-cache/" 2>/dev/null)" ]]
    [[ -z "$(ls -A "$3/uv-python/" 2>/dev/null)" ]]
  ' _ "${uv_fail_repo}" "${ROOT_DIR}" "${uv_fail_stage}" "${reg_fakebin}"; then
    record_pass "warm_caches leaves uv-cache and uv-python empty when uv fails"
  else
    record_fail "warm_caches leaves uv-cache and uv-python empty when uv fails"
  fi
}

# 3k. AUTOSYNC_FORCE=true promotes the first up_to_date record to available on no_change fixtures without altering versions.
case_3k() {
  local repo_3k="${SCRATCH}/repo-3k"
  fast_cp_dir "${PREFLIGHT_REPO}" "${repo_3k}"
  local stage_force="${SCRATCH}/stage-force/k8s-deps"
  if (
    cd "${repo_3k}" && \
    AUTOSYNC_FORCE=true \
    AUTOSYNC_K8S_DEPS_FIXTURE_DIR="${FIXTURE_NOCHANGE}" \
    AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
    GOMODCACHE="${HOST_GOMODCACHE}" \
    GOCACHE="${HOST_GOCACHE}" \
    bash "${repo_3k}/.auto-sync/scripts/preflight-k8s-deps.sh" "${stage_force}" >/dev/null
  ) && \
  python3 - "${stage_force}" "${BASE_SHA}" <<'PY'
import json
import os
import sys

stage_dir, base_sha = sys.argv[1:3]
with open(os.path.join(stage_dir, "upstream-records.json"), "r", encoding="utf-8") as f:
    recs = json.load(f)
avail = [r for r in recs if r.get("status") == "available"]
assert len(avail) == 1, recs
assert "(forced via AUTOSYNC_FORCE=true)" in avail[0].get("reason", ""), avail[0]
with open(os.path.join(stage_dir, "k8s-deps-preflight-report.json"), "r", encoding="utf-8") as f:
    rep = json.load(f)
assert rep["source_revision"] != base_sha, rep
PY
  then
    record_pass "preflight-k8s-deps.sh honors AUTOSYNC_FORCE=true by promoting an up_to_date record and staging source_revision"
  else
    record_fail "preflight-k8s-deps.sh AUTOSYNC_FORCE=true promotion"
  fi
}

# 3l. warm_caches --go-all keeps repo_root git status --porcelain empty even if go mod download all mutates go.sum.
case_3l() {
  local repo_3l="${SCRATCH}/repo-3l"
  fast_cp_dir "${PREFLIGHT_REPO}" "${repo_3l}"
  local wrap_go_3l="${SCRATCH}/wrap-go-3l"
  local stage_3l="${SCRATCH}/stage-3l/k8s-deps"
  mkdir -p "${wrap_go_3l}" "${stage_3l}"
  cat > "${wrap_go_3l}/go" <<'SH'
#!/usr/bin/env bash
if [[ "${1:-}" == "env" && "${2:-}" == "GOMODCACHE" ]]; then
  echo "${TMPDIR:-/tmp}/nonexistent-gomodcache"
  exit 0
fi
if [[ "${1:-}" == "mod" && "${2:-}" == "download" ]]; then
  [[ -f "go.mod" && -f "go.sum" && -d "staging" ]] || exit 1
  if [[ "$(pwd -P)" == "${FORBIDDEN_REPO_ROOT}" ]]; then
    echo "FAIL: go mod download ran directly inside repo_root" >&2
    exit 1
  fi
  printf 'example.com/mutated v1.0.0 h1:deadbeef=\n' >> go.sum
  mkdir -p "${GOMODCACHE}"
  : > "${GOMODCACHE}/.warmed"
  exit 0
fi
exec "${REAL_GO_BIN}" "$@"
SH
  chmod +x "${wrap_go_3l}/go"
  local baseline_sum="${SCRATCH}/repo-3l-baseline.sum"
  cp -f "${repo_3l}/go.sum" "${baseline_sum}"
  # shellcheck disable=SC2016
  if (
    cd "${repo_3l}" && \
    FORBIDDEN_REPO_ROOT="${repo_3l}" \
    REAL_GO_BIN="${REAL_GO_BIN}" \
    PATH="${wrap_go_3l}:${PATH}" \
    GOPROXY="https://proxy.golang.org,direct" \
    AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=false \
    bash -c '
      set -euo pipefail
      source "$1/.auto-sync/scripts/lib-preflight.sh"
      autosync::preflight::warm_caches "$2" --go-all
    ' _ "${ROOT_DIR}" "${stage_3l}" >/dev/null 2>&1
  ) && \
  [[ -z "$(git -C "${repo_3l}" status --porcelain)" ]] && \
  cmp -s "${baseline_sum}" "${repo_3l}/go.sum" && \
  [[ -f "${SCRATCH}/stage-3l/go-mod/.warmed" ]]; then
    record_pass "warm_caches --go-all isolates go mod download all and leaves git status --porcelain empty"
  else
    record_fail "warm_caches --go-all dirtied repo_root or failed isolation"
  fi
}

# 3m. preflight-k8s-deps.sh isolates go mod download all from work_repo/go.sum and validate_k8s_deps.sh step 5 fails closed on untidy go.sum.
case_3m() {
  local repo_3m="${SCRATCH}/repo-3m"
  fast_cp_dir "${PREFLIGHT_REPO}" "${repo_3m}"
  local wrap_go_3m="${SCRATCH}/wrap-go-3m"
  local stage_3m="${SCRATCH}/stage-3m/k8s-deps"
  local tmp_3m="${SCRATCH}/tmp-3m"
  mkdir -p "${wrap_go_3m}" "${stage_3m}/go-mod" "${stage_3m}/go-cache" "${tmp_3m}"
  : > "${stage_3m}/go-mod/.warmed"

  cat > "${wrap_go_3m}/go" <<'SH'
#!/usr/bin/env bash
if [[ "${1:-}" == "mod" && "${2:-}" == "tidy" ]]; then
  if [[ -f go.sum ]]; then
    grep -v '^example\.com/bloated ' go.sum > go.sum.tmp || true
    mv -f go.sum.tmp go.sum
  fi
  exit 0
fi
if [[ "${1:-}" == "mod" && "${2:-}" == "download" ]]; then
  if [[ "${3:-}" == "all" ]]; then
    [[ -f "go.mod" && -f "go.sum" && -d "staging" ]] || exit 1
    if [[ -d ".git" ]]; then
      echo "FAIL: go mod download all ran inside work_repo instead of isolated temp dir" >&2
      exit 1
    fi
    printf 'example.com/bloated v1.0.0 h1:deadbeef=\n' >> go.sum
    mkdir -p "${GOMODCACHE}"
    : > "${GOMODCACHE}/.all-warmed"
  fi
  exit 0
fi
if [[ "${WRAP_INTERCEPT_LIST_ALL:-0}" == "1" && "${1:-}" == "list" && "${2:-}" == "-m" && "${3:-}" == "-json" && "${4:-}" == "all" ]]; then
  [[ -f "${GOMODCACHE}/.all-warmed" ]] || exit 1
  exit 0
fi
exec "${REAL_GO_BIN}" "$@"
SH
  chmod +x "${wrap_go_3m}/go"

  local preflight_log_3m="${tmp_3m}/preflight.log"
  local src_rev_3m=""
  if (
    cd "${repo_3m}" && \
    TMPDIR="${tmp_3m}" \
    REAL_GO_BIN="${REAL_GO_BIN}" \
    WRAP_INTERCEPT_LIST_ALL=1 \
    PATH="${wrap_go_3m}:${PATH}" \
    AUTOSYNC_K8S_DEPS_FIXTURE_DIR="${FIXTURE_STAGE}" \
    AUTOSYNC_SKIP_GENERATE=true \
    AUTOSYNC_WARM_GO_LIST_ALL=true \
    GOCACHE="${stage_3m}/go-cache" \
    bash "${repo_3m}/.auto-sync/scripts/preflight-k8s-deps.sh" "${stage_3m}" >"${preflight_log_3m}" 2>&1
  ) && \
  src_rev_3m="$(python3 - "${stage_3m}" "${BASE_SHA}" <<'PY'
import json
import os
import sys

stage_dir, base_sha = sys.argv[1:3]
with open(os.path.join(stage_dir, "k8s-deps-preflight-report.json"), "r", encoding="utf-8") as f:
    rep = json.load(f)
assert rep["patch_clean"] is True, rep
assert rep["staging_updated"] is True, rep
assert rep["source_revision"] != base_sha, rep
print(rep["source_revision"])
PY
  )" && \
  [[ -f "${stage_3m}/go-mod/.all-warmed" ]] && \
  ! git -C "${repo_3m}" show "${src_rev_3m}:go.sum" | grep -q '^example\.com/bloated '; then
    record_pass "preflight-k8s-deps.sh isolates go mod download all and keeps staged go.sum tidy"
  else
    local preflight_err_3m=""
    if [[ -f "${preflight_log_3m}" ]]; then
      preflight_err_3m="$(cat "${preflight_log_3m}")"
    fi
    record_fail "preflight-k8s-deps.sh isolates go mod download all and keeps staged go.sum tidy (${preflight_err_3m})"
    return
  fi

  local cand_3m="${SCRATCH}/cand-3m"
  fast_cp_dir "${CAND_BASE}" "${cand_3m}"
  cat > "${cand_3m}/Makefile" <<'MK'
.PHONY: generate lint test
generate:
	@true
lint:
	@true
test:
	@true
MK

  local stage_val_3m="${SCRATCH}/stage-val-3m"
  fast_cp_dir "${STAGE_DISC}" "${stage_val_3m}"
  rm -f "${stage_val_3m}/toolchain.env"
  local make_bin_dir git_bin_dir py_bin_dir
  make_bin_dir="$(dirname "$(command -v make)")"
  git_bin_dir="$(dirname "$(command -v git)")"
  py_bin_dir="$(dirname "$(command -v python3)")"
  local val_path="${wrap_go_3m}:${make_bin_dir}:${git_bin_dir}:${py_bin_dir}:/usr/local/bin:/usr/bin:/bin"

  run_val_3m() {
    local case_root="$1"
    mkdir -p "${case_root}/home" "${case_root}/tmp"
    env -i \
      PATH="${val_path}" \
      HOME="${case_root}/home" \
      TMPDIR="${case_root}/tmp" \
      REAL_GO_BIN="${REAL_GO_BIN}" \
      GOMODCACHE="${HOST_GOMODCACHE}" \
      GOCACHE="${HOST_GOCACHE}" \
      AUTOSYNC_STAGE_DIR="${stage_val_3m}" \
      LOOPWRIGHT_CANDIDATE_DIR="${cand_3m}" \
      AUTOSYNC_FAST_SELFTEST=false \
      bash "${cand_3m}/.auto-sync/checks/validate_k8s_deps.sh"
  }

  if run_val_3m "${SCRATCH}/val-3m-ok" >/dev/null 2>&1; then
    record_pass "validate_k8s_deps.sh step 5 passes when go.sum is tidy"
  else
    record_fail "validate_k8s_deps.sh step 5 passes when go.sum is tidy"
  fi

  printf 'example.com/bloated v1.0.0 h1:deadbeef=\n' >> "${cand_3m}/go.sum"
  local bloat_err=""
  if ! bloat_err="$(run_val_3m "${SCRATCH}/val-3m-bloat" 2>&1)" && \
     grep -q "uncommitted drift" <<<"${bloat_err}" && \
     grep -q "go.sum" <<<"${bloat_err}"; then
    record_pass "validate_k8s_deps.sh step 5 fails closed on bloated go.sum via go mod tidy"
  else
    record_fail "validate_k8s_deps.sh step 5 fails closed on bloated go.sum (${bloat_err})"
  fi
}

spawn_case "1bc" case_1bc
spawn_case "2" case_2
spawn_case "2b" case_2b
spawn_case "2c" case_2c
spawn_case "2d" case_2d
spawn_case "2e" case_2e
spawn_case "3a" case_3a
spawn_case "3b" case_3b
spawn_case "3c" case_3c
spawn_case "3d" case_3d
spawn_case "3e" case_3e
spawn_case "3f" case_3f
spawn_case "3g" case_3g
spawn_case "3h1" case_3h1
spawn_case "3h2" case_3h2
spawn_case "3i" case_3i
spawn_case "3j" case_3j
spawn_case "3k" case_3k
spawn_case "3l" case_3l
spawn_case "3m" case_3m

for i in "${!CASE_NAMES[@]}"; do
  cid="${CASE_NAMES[$i]}"
  cpid="${CASE_PIDS[$i]}"
  sub_rc="${CASE_RCS[$i]}"
  if [[ -z "${sub_rc}" ]]; then
    sub_rc=0
    wait "${cpid}" || sub_rc=$?
  fi
  res_file="${RESULTS_DIR}/${cid}.res"
  if [[ -f "${res_file}" ]]; then
    while IFS=$'\t' read -r kind msg || [[ -n "${kind}" ]]; do
      [[ -n "${kind}" ]] || continue
      if [[ "${kind}" == "PASS" ]]; then
        pass "${msg}"
      else
        fail "${msg}"
      fi
    done < "${res_file}"
  fi
  if (( sub_rc != 0 )); then
    fail "subtest ${cid} exited with unexpected status ${sub_rc}"
  fi
done

echo
if (( FAILS > 0 )); then
  printf 'K8S-DEPS SELFTEST FAILED: %d failure(s), %d passed\n' "${FAILS}" "${PASSES}" >&2
  exit 1
fi
printf 'K8S-DEPS SELFTEST PASSED: %d check(s) passed\n' "${PASSES}"
