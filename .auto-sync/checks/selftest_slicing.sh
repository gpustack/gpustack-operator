#!/usr/bin/env bash
# .auto-sync/checks/selftest_slicing.sh - offline self-tests for slicing-sync.
#
# Usage:
#   bash .auto-sync/checks/selftest_slicing.sh [repo-root]
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
  printf 'PASS  %s\n' "$1"
  PASSES=$((PASSES + 1))
}

fail() {
  printf 'FAIL  %s\n' "$1" >&2
  FAILS=$((FAILS + 1))
}

SCRATCH="$(cd "$(mktemp -d)" && pwd -P)"

PREFLIGHT_SCRIPT="${ROOT_DIR}/.auto-sync/scripts/preflight-slicing.sh"
VALIDATE_SCRIPT="${ROOT_DIR}/.auto-sync/checks/validate_slicing.sh"
PROPOSAL_FILE="${ROOT_DIR}/.auto-sync/proposals/slicing.json"
TASKPACK_FILE="${ROOT_DIR}/.auto-sync/slicing-taskpack.yaml"
INSTRUCTIONS_FILE="${ROOT_DIR}/.auto-sync/instructions/slicing.md"

for req in "${PREFLIGHT_SCRIPT}" "${VALIDATE_SCRIPT}" "${PROPOSAL_FILE}" "${TASKPACK_FILE}" "${INSTRUCTIONS_FILE}"; do
  if [[ ! -f "${req}" ]]; then
    fail "missing required slicing-sync file: ${req#"${ROOT_DIR}/"}"
  fi
done

if (( FAILS > 0 )); then
  printf 'SLICING SELFTEST FAILED (RED): %d missing deliverable(s)\n' "${FAILS}" >&2
  exit 1
fi

# 1. Build local mock upstream repositories (HAMi-core and ubs-virt with CRLF CMakeLists.txt).
HAMI_MOCK="${SCRATCH}/mock-hami-core"
UBS_MOCK="${SCRATCH}/mock-ubs-virt"
mkdir -p "${HAMI_MOCK}/src" "${HAMI_MOCK}/include" "${UBS_MOCK}/ubs-virt-enpu/vcann-rt/src"

git -C "${HAMI_MOCK}" init -q -b main
git -C "${HAMI_MOCK}" config user.name "mock"
git -C "${HAMI_MOCK}" config user.email "mock@example.com"
printf 'int hami_v1(void) { return 1; }\n' > "${HAMI_MOCK}/src/hook.c"
printf '#define HAMI_VER 1\n' > "${HAMI_MOCK}/include/hook.h"
printf 'cmake_minimum_required(VERSION 3.16)\n' > "${HAMI_MOCK}/CMakeLists.txt"
printf '#!/usr/bin/env bash\nexit 0\n' > "${HAMI_MOCK}/build.sh"
printf 'HAMi-core v1\n' > "${HAMI_MOCK}/README.md"
git -C "${HAMI_MOCK}" add -A
git -C "${HAMI_MOCK}" commit -q -m "hami v1"
HAMI_PINNED="$(git -C "${HAMI_MOCK}" rev-parse HEAD)"

# Commit 2 touches only an unwatched file (README.md).
printf 'HAMi-core v1 docs update\n' > "${HAMI_MOCK}/README.md"
git -C "${HAMI_MOCK}" add README.md
git -C "${HAMI_MOCK}" commit -q -m "hami docs only"
HAMI_UNWATCHED_HEAD="$(git -C "${HAMI_MOCK}" rev-parse HEAD)"

git -C "${UBS_MOCK}" init -q -b main
git -C "${UBS_MOCK}" config user.name "mock"
git -C "${UBS_MOCK}" config user.email "mock@example.com"
git -C "${UBS_MOCK}" config core.autocrlf false
python3 - "${UBS_MOCK}/ubs-virt-enpu/vcann-rt/CMakeLists.txt" <<'PY'
import sys

lines = [f"# line {i}\r\n" for i in range(1, 49)]
lines.extend([
    "set(LINK_LIBS dcmi)\r\n",
    "\r\n",
    'message(STATUS "CMakeList build start")\r\n',
    "add_library(vruntime SHARED src/ascend/hook.c ${SRC_LIST})\r\n",
    "add_custom_command(\r\n",
    "    TARGET vruntime POST_BUILD\r\n",
    "    COMMAND chmod 400 $<TARGET_FILE:vruntime>\r\n",
])
with open(sys.argv[1], "wb") as f:
    f.write("".join(lines).encode("utf-8"))
PY
printf 'int rt_v1(void) { return 1; }\n' > "${UBS_MOCK}/ubs-virt-enpu/vcann-rt/src/rt.c"
git -C "${UBS_MOCK}" add -A
git -C "${UBS_MOCK}" commit -q -m "ubs v1"
UBS_PINNED="$(git -C "${UBS_MOCK}" rev-parse HEAD)"

# Commit 2 touches watched ubs-virt-enpu/vcann-rt/src/rt.c while keeping CRLF CMakeLists.txt clean.
printf 'int rt_v2(void) { return 2; }\n' > "${UBS_MOCK}/ubs-virt-enpu/vcann-rt/src/rt.c"
git -C "${UBS_MOCK}" add -A
git -C "${UBS_MOCK}" commit -q -m "ubs v2 watched change"
UBS_WATCHED_HEAD="$(git -C "${UBS_MOCK}" rev-parse HEAD)"

# 2. Build a synthetic repository mirroring the slicing domain files.
SYNTH_REPO="${SCRATCH}/synth-repo"
mkdir -p \
  "${SYNTH_REPO}/.auto-sync/scripts" \
  "${SYNTH_REPO}/.auto-sync/checks" \
  "${SYNTH_REPO}/.auto-sync/proposals" \
  "${SYNTH_REPO}/.auto-sync/instructions" \
  "${SYNTH_REPO}/pack/gpustack-operator/external/nvidia" \
  "${SYNTH_REPO}/pack/gpustack-operator/external/ascend/vcann-rt" \
  "${SYNTH_REPO}/hack/check"

cp "${ROOT_DIR}/.auto-sync/scripts/lib-preflight.sh" "${SYNTH_REPO}/.auto-sync/scripts/lib-preflight.sh"
cp "${PREFLIGHT_SCRIPT}" "${SYNTH_REPO}/.auto-sync/scripts/preflight-slicing.sh"
cp "${ROOT_DIR}/.auto-sync/checks/lib.sh" "${SYNTH_REPO}/.auto-sync/checks/lib.sh"
cp "${VALIDATE_SCRIPT}" "${SYNTH_REPO}/.auto-sync/checks/validate_slicing.sh"
cp "${PROPOSAL_FILE}" "${SYNTH_REPO}/.auto-sync/proposals/slicing.json"
cp "${INSTRUCTIONS_FILE}" "${SYNTH_REPO}/.auto-sync/instructions/slicing.md"
cp "${TASKPACK_FILE}" "${SYNTH_REPO}/.auto-sync/slicing-taskpack.yaml"
cp -R "${ROOT_DIR}/pack/gpustack-operator/external/nvidia/." "${SYNTH_REPO}/pack/gpustack-operator/external/nvidia/"
cp -R "${ROOT_DIR}/pack/gpustack-operator/external/ascend/." "${SYNTH_REPO}/pack/gpustack-operator/external/ascend/"
cp "${ROOT_DIR}/hack/check/symbols.sh" "${SYNTH_REPO}/hack/check/symbols.sh"

python3 - "${ROOT_DIR}/pack/gpustack-operator/Dockerfile" "${SYNTH_REPO}/pack/gpustack-operator/Dockerfile" "${HAMI_PINNED}" "${UBS_PINNED}" <<'PY'
import re
import sys

src, dst, hami_sha, ubs_sha = sys.argv[1:5]
with open(src, "r", encoding="utf-8") as f:
    txt = f.read()
txt = re.sub(r'^ARG LIB_HAMI_CORE_COMMIT="[^"]*"', f'ARG LIB_HAMI_CORE_COMMIT="{hami_sha}"', txt, flags=re.M)
txt = re.sub(r'^ARG LIB_UBS_VIRT_ENPU_COMMIT="[^"]*"', f'ARG LIB_UBS_VIRT_ENPU_COMMIT="{ubs_sha}"', txt, flags=re.M)
with open(dst, "w", encoding="utf-8") as f:
    f.write(txt)
PY

python3 - "${SYNTH_REPO}/.auto-sync/proposals/slicing.json" "${HAMI_PINNED}" "${UBS_PINNED}" <<'PY'
import json
import sys

p, hami_sha, ubs_sha = sys.argv[1:4]
with open(p, "r", encoding="utf-8") as f:
    doc = json.load(f)
doc["versions"]["hami-core"] = hami_sha
doc["versions"]["ubs-virt"] = ubs_sha
with open(p, "w", encoding="utf-8") as f:
    json.dump(doc, f, indent=2)
    f.write("\n")
PY

git -C "${SYNTH_REPO}" init -q -b main
git -C "${SYNTH_REPO}" config user.name "test"
git -C "${SYNTH_REPO}" config user.email "test@example.com"
git -C "${SYNTH_REPO}" config core.autocrlf false
git -C "${SYNTH_REPO}" add -A
git -C "${SYNTH_REPO}" commit -q -m "baseline"
SYNTH_BASE_SHA="$(git -C "${SYNTH_REPO}" rev-parse HEAD)"

# 3. Test preflight-slicing.sh: unwatched HAMi-core change vs watched ubs-virt change with CRLF patch clean.
STAGE_1="${SCRATCH}/stage-1/slicing"
GHA_1="${SCRATCH}/gha-1.txt"
PREFLIGHT_LOG_1="${SCRATCH}/preflight-1.log"
: > "${GHA_1}"
if (
  cd "${SYNTH_REPO}" && \
  AUTOSYNC_SLICING_HAMI_REPO_URL="${HAMI_MOCK}" \
  AUTOSYNC_SLICING_UBS_REPO_URL="${UBS_MOCK}" \
  GITHUB_OUTPUT="${GHA_1}" \
  bash "${SYNTH_REPO}/.auto-sync/scripts/preflight-slicing.sh" "${STAGE_1}"
) >"${PREFLIGHT_LOG_1}" 2>&1 && \
python3 - "${STAGE_1}" "${GHA_1}" "${HAMI_PINNED}" "${HAMI_UNWATCHED_HEAD}" "${UBS_PINNED}" "${UBS_WATCHED_HEAD}" "${SYNTH_BASE_SHA}" <<'PY'
import json
import os
import sys

stage_dir, gha_path, hami_pin, hami_head, ubs_pin, ubs_head, base_sha = sys.argv[1:8]

with open(os.path.join(stage_dir, "upstream-records.json"), "r", encoding="utf-8") as f:
    records = json.load(f)
by_id = {r["upstreamId"]: r for r in records}
assert by_id["hami-core"]["status"] == "up_to_date", by_id["hami-core"]
assert by_id["hami-core"]["version"] == hami_pin, by_id["hami-core"]
assert by_id["ubs-virt"]["status"] == "available", by_id["ubs-virt"]
assert by_id["ubs-virt"]["version"] == ubs_head, by_id["ubs-virt"]

with open(os.path.join(stage_dir, "slicing-preflight-report.json"), "r", encoding="utf-8") as f:
    report = json.load(f)
assert report["hami_core"]["head_changed"] is True, report
assert report["hami_core"]["watched_changed"] is False, report
assert report["hami_core"]["pinned_commit_reachable"] is True, report
assert report["ubs_virt"]["watched_changed"] is True, report
assert report["ubs_virt"]["pinned_commit_reachable"] is True, report
assert report["patch_clean"] is True, report

assert os.path.isfile(os.path.join(stage_dir, "baseline", "pack", "gpustack-operator", "Dockerfile"))
assert os.path.isdir(os.path.join(stage_dir, "hami-core", ".git"))
assert os.path.isdir(os.path.join(stage_dir, "ubs-virt", ".git"))

with open(gha_path, "r", encoding="utf-8") as f:
    gha_txt = f.read()
assert f"\n{stage_dir}\n" in gha_txt
assert f"\n{base_sha}\n" in gha_txt
assert "upstream_records<<" in gha_txt
PY
then
  pass "preflight-slicing.sh detects unwatched vs watched changes and CRLF dry-run patch clean"
else
  tail -n 50 "${PREFLIGHT_LOG_1}" >&2 || true
  fail "preflight-slicing.sh detects unwatched vs watched changes and CRLF dry-run patch clean"
fi

# 4. Advance HAMi-core watched src/ and break ubs-virt CMakeLists.txt CRLF context.
printf 'int hami_v2(void) { return 2; }\n' > "${HAMI_MOCK}/src/hook.c"
git -C "${HAMI_MOCK}" add src/hook.c
git -C "${HAMI_MOCK}" commit -q -m "hami v2 watched change"
HAMI_WATCHED_HEAD="$(git -C "${HAMI_MOCK}" rev-parse HEAD)"

python3 - "${UBS_MOCK}/ubs-virt-enpu/vcann-rt/CMakeLists.txt" <<'PY'
import sys

with open(sys.argv[1], "rb") as f:
    data = f.read()
# Convert CRLF to LF so the CRLF patch hunk intentionally fails to apply.
with open(sys.argv[1], "wb") as f:
    f.write(data.replace(b"\r\n", b"\n"))
PY
git -C "${UBS_MOCK}" add -A
git -C "${UBS_MOCK}" commit -q -m "ubs v3 lf breaks patch"
UBS_BROKEN_HEAD="$(git -C "${UBS_MOCK}" rev-parse HEAD)"

STAGE_2="${SCRATCH}/stage-2/slicing"
PREFLIGHT_LOG_2="${SCRATCH}/preflight-2.log"
if (
  cd "${SYNTH_REPO}" && \
  AUTOSYNC_SLICING_HAMI_REPO_URL="${HAMI_MOCK}" \
  AUTOSYNC_SLICING_UBS_REPO_URL="${UBS_MOCK}" \
  bash "${SYNTH_REPO}/.auto-sync/scripts/preflight-slicing.sh" "${STAGE_2}"
) >"${PREFLIGHT_LOG_2}" 2>&1 && \
python3 - "${STAGE_2}" "${HAMI_WATCHED_HEAD}" <<'PY'
import json
import os
import sys

stage_dir, hami_head = sys.argv[1:3]
with open(os.path.join(stage_dir, "upstream-records.json"), "r", encoding="utf-8") as f:
    records = json.load(f)
by_id = {r["upstreamId"]: r for r in records}
assert by_id["hami-core"]["status"] == "available", by_id["hami-core"]
assert by_id["hami-core"]["version"] == hami_head, by_id["hami-core"]

with open(os.path.join(stage_dir, "slicing-preflight-report.json"), "r", encoding="utf-8") as f:
    report = json.load(f)
assert report["hami_core"]["watched_changed"] is True, report
assert report["patch_clean"] is False, report
PY
then
  pass "preflight-slicing.sh records watched HAMi-core change and patch_clean=false on LF mismatch"
else
  tail -n 50 "${PREFLIGHT_LOG_2}" >&2 || true
  fail "preflight-slicing.sh records watched HAMi-core change and patch_clean=false on LF mismatch"
fi

# 4b. Unreachable pinned commit in upstream history emits available at head_sha instead of aborting.
SYNTH_UNREACH="${SCRATCH}/synth-unreach"
cp -R "${SYNTH_REPO}" "${SYNTH_UNREACH}"
UNREACH_SHA="deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
sed -i.bak "s/^ARG LIB_HAMI_CORE_COMMIT=\"[^\"]*\"/ARG LIB_HAMI_CORE_COMMIT=\"${UNREACH_SHA}\"/" \
  "${SYNTH_UNREACH}/pack/gpustack-operator/Dockerfile"
rm -f "${SYNTH_UNREACH}/pack/gpustack-operator/Dockerfile.bak"
STAGE_UNREACH="${SCRATCH}/stage-unreach/slicing"
PREFLIGHT_LOG_UNREACH="${SCRATCH}/preflight-unreach.log"
if (
  cd "${SYNTH_UNREACH}" && \
  AUTOSYNC_SLICING_HAMI_REPO_URL="${HAMI_MOCK}" \
  AUTOSYNC_SLICING_UBS_REPO_URL="${UBS_MOCK}" \
  bash "${SYNTH_UNREACH}/.auto-sync/scripts/preflight-slicing.sh" "${STAGE_UNREACH}"
) >"${PREFLIGHT_LOG_UNREACH}" 2>&1 && \
python3 - "${STAGE_UNREACH}" "${UNREACH_SHA}" "${HAMI_WATCHED_HEAD}" <<'PY'
import json
import os
import sys

stage_dir, unreach_sha, hami_head = sys.argv[1:4]
with open(os.path.join(stage_dir, "slicing-preflight-report.json"), "r", encoding="utf-8") as f:
    report = json.load(f)
assert report["hami_core"]["pinned_commit_reachable"] is False, report

with open(os.path.join(stage_dir, "upstream-records.json"), "r", encoding="utf-8") as f:
    by_id = {r["upstreamId"]: r for r in json.load(f)}
hami_rec = by_id["hami-core"]
assert hami_rec["status"] == "available", hami_rec
assert hami_rec["version"] == hami_head, hami_rec
assert f"pinned commit {unreach_sha} is unreachable" in hami_rec["reason"], hami_rec
PY
then
  pass "preflight-slicing.sh records pinned_commit_reachable=false and available status when pinned commit is unreachable"
else
  tail -n 50 "${PREFLIGHT_LOG_UNREACH}" >&2 || true
  fail "preflight-slicing.sh unreachable pinned commit handling"
fi

# 5. Gitless env -i validate_slicing.sh tests with stubbed AUTOSYNC_XBUILD_RUNNER.
STUB_RUNNER="${SCRATCH}/stub-xbuild-runner.sh"
cat > "${STUB_RUNNER}" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
script_path="${1:?script path required}"
target="${2:?target required}"
if [[ "${XB_MODE:-}" != "local" ]]; then
  echo "FAIL: expected XB_MODE=local, got '${XB_MODE:-}'" >&2
  exit 1
fi
case "${XB_STAGE:-}" in
  /opt/vgpu|/opt/enpu|/opt/enpu/*|"")
    echo "FAIL: XB_STAGE must be a non-root temp path, got '${XB_STAGE:-}'" >&2
    exit 1
    ;;
esac
if [[ ! -d "${XB_STAGE}" ]]; then
  echo "FAIL: XB_STAGE directory does not exist: ${XB_STAGE}" >&2
  exit 1
fi
printf '%s %s %s\n' "${script_path}" "${target}" "${XB_STAGE}" >> "${AUTOSYNC_TEST_LOG}"
SH
chmod +x "${STUB_RUNNER}"

make_gitless_candidate() {
  local dst="$1"
  rm -rf "${dst}"
  mkdir -p "${dst}"
  tar --exclude='.git' -cf - -C "${SYNTH_REPO}" . | tar -xf - -C "${dst}"
}

set_commits_in_candidate() {
  local cand_dir="$1"
  local hami_sha="$2"
  local ubs_sha="$3"
  python3 - "${cand_dir}/pack/gpustack-operator/Dockerfile" "${cand_dir}/.auto-sync/proposals/slicing.json" "${hami_sha}" "${ubs_sha}" <<'PY'
import json
import re
import sys

df_path, prop_path, hami_sha, ubs_sha = sys.argv[1:5]
with open(df_path, "r", encoding="utf-8") as f:
    txt = f.read()
txt = re.sub(r'^ARG LIB_HAMI_CORE_COMMIT="[^"]*"', f'ARG LIB_HAMI_CORE_COMMIT="{hami_sha}"', txt, flags=re.M)
txt = re.sub(r'^ARG LIB_UBS_VIRT_ENPU_COMMIT="[^"]*"', f'ARG LIB_UBS_VIRT_ENPU_COMMIT="{ubs_sha}"', txt, flags=re.M)
with open(df_path, "w", encoding="utf-8") as f:
    f.write(txt)

with open(prop_path, "r", encoding="utf-8") as f:
    doc = json.load(f)
doc["versions"]["hami-core"] = hami_sha
doc["versions"]["ubs-virt"] = ubs_sha
with open(prop_path, "w", encoding="utf-8") as f:
    json.dump(doc, f, indent=2)
    f.write("\n")
PY
}

# 5a. Positive NVIDIA-only bump.
CAND_NV="${SCRATCH}/cand-nv"
LOG_NV="${SCRATCH}/log-nv.txt"
VAL_LOG_NV="${SCRATCH}/val-nv.log"
: > "${LOG_NV}"
make_gitless_candidate "${CAND_NV}"
set_commits_in_candidate "${CAND_NV}" "${HAMI_WATCHED_HEAD}" "${UBS_PINNED}"

if env -i PATH=/usr/local/bin:/usr/bin:/bin \
  TMPDIR="${SCRATCH}" \
  AUTOSYNC_STAGE_DIR="${STAGE_1}" \
  LOOPWRIGHT_CANDIDATE_DIR="${CAND_NV}" \
  AUTOSYNC_XBUILD_RUNNER="${STUB_RUNNER}" \
  AUTOSYNC_TEST_LOG="${LOG_NV}" \
  bash "${CAND_NV}/.auto-sync/checks/validate_slicing.sh" >"${VAL_LOG_NV}" 2>&1 && \
  [[ ! -e "${CAND_NV}/.git" ]] && \
  python3 - "${LOG_NV}" "${SCRATCH}" <<'PY'
import sys

log_path, scratch = sys.argv[1:3]
with open(log_path, "r", encoding="utf-8") as f:
    lines = [ln.strip() for ln in f if ln.strip()]

expected = [
    (".agents/skills/gpustack-operator-xbuild-and-verify/scripts/build.sh", "xbuild-nvidia-cuda-12"),
    (".agents/skills/gpustack-operator-xbuild-and-verify/cases/nvidia-case-1.sh", "xbuild-nvidia-cuda-12"),
    (".agents/skills/gpustack-operator-xbuild-and-verify/cases/nvidia-case-4.sh", "xbuild-nvidia-cuda-12"),
    (".agents/skills/gpustack-operator-xbuild-and-verify/scripts/build.sh", "xbuild-nvidia-cuda-13"),
    (".agents/skills/gpustack-operator-xbuild-and-verify/cases/nvidia-case-1.sh", "xbuild-nvidia-cuda-13"),
    (".agents/skills/gpustack-operator-xbuild-and-verify/cases/nvidia-case-4.sh", "xbuild-nvidia-cuda-13"),
]
assert len(lines) == len(expected), f"expected 6 NVIDIA invocations, got {lines}"
for line, (exp_script, exp_target) in zip(lines, expected):
    parts = line.split()
    assert len(parts) == 3, line
    assert parts[0] == exp_script and parts[1] == exp_target, (parts, exp_script, exp_target)
    assert parts[2].startswith(f"{scratch}/xb-stage-nvidia-"), parts[2]
PY
then
  pass "validate_slicing.sh gitless env -i NVIDIA-only bump runs only NVIDIA stages with non-root XB_STAGE"
else
  tail -n 50 "${VAL_LOG_NV}" >&2 || true
  fail "validate_slicing.sh gitless env -i NVIDIA-only bump runs only NVIDIA stages with non-root XB_STAGE"
fi

# 5b. Positive Ascend-only bump with CRLF vcann-rt patch.
CAND_ASC="${SCRATCH}/cand-asc"
LOG_ASC="${SCRATCH}/log-asc.txt"
VAL_LOG_ASC="${SCRATCH}/val-asc.log"
: > "${LOG_ASC}"
make_gitless_candidate "${CAND_ASC}"
set_commits_in_candidate "${CAND_ASC}" "${HAMI_PINNED}" "${UBS_WATCHED_HEAD}"

if env -i PATH=/usr/local/bin:/usr/bin:/bin \
  TMPDIR="${SCRATCH}" \
  AUTOSYNC_STAGE_DIR="${STAGE_1}" \
  LOOPWRIGHT_CANDIDATE_DIR="${CAND_ASC}" \
  AUTOSYNC_XBUILD_RUNNER="${STUB_RUNNER}" \
  AUTOSYNC_TEST_LOG="${LOG_ASC}" \
  bash "${CAND_ASC}/.auto-sync/checks/validate_slicing.sh" >"${VAL_LOG_ASC}" 2>&1 && \
  [[ ! -e "${CAND_ASC}/.git" ]] && \
  python3 - "${LOG_ASC}" "${SCRATCH}" <<'PY'
import sys

log_path, scratch = sys.argv[1:3]
with open(log_path, "r", encoding="utf-8") as f:
    lines = [ln.strip() for ln in f if ln.strip()]

expected = [
    (".agents/skills/gpustack-operator-xbuild-and-verify/scripts/build.sh", "xbuild-ascend-cann-8-910b"),
    (".agents/skills/gpustack-operator-xbuild-and-verify/cases/ascend-case-1.sh", "xbuild-ascend-cann-8-910b"),
    (".agents/skills/gpustack-operator-xbuild-and-verify/scripts/build.sh", "xbuild-ascend-cann-9-910b"),
    (".agents/skills/gpustack-operator-xbuild-and-verify/cases/ascend-case-1.sh", "xbuild-ascend-cann-9-910b"),
]
assert len(lines) == len(expected), f"expected 4 Ascend invocations, got {lines}"
for line, (exp_script, exp_target) in zip(lines, expected):
    parts = line.split()
    assert len(parts) == 3, line
    assert parts[0] == exp_script and parts[1] == exp_target, (parts, exp_script, exp_target)
    assert parts[2].startswith(f"{scratch}/xb-stage-ascend-"), parts[2]
PY
then
  pass "validate_slicing.sh gitless env -i Ascend-only bump verifies CRLF patch and runs only Ascend stages"
else
  tail -n 50 "${VAL_LOG_ASC}" >&2 || true
  fail "validate_slicing.sh gitless env -i Ascend-only bump verifies CRLF patch and runs only Ascend stages"
fi

# 5c. Negative 1: mutating SKOPEO_VERSION or GO_VERSION in Dockerfile fails closed.
CAND_BAD_DF="${SCRATCH}/cand-bad-df"
VAL_LOG_BAD_DF="${SCRATCH}/val-bad-df.log"
make_gitless_candidate "${CAND_BAD_DF}"
sed 's/^ARG SKOPEO_VERSION="[^"]*"/ARG SKOPEO_VERSION="9.99.0"/' "${SYNTH_REPO}/pack/gpustack-operator/Dockerfile" > "${CAND_BAD_DF}/pack/gpustack-operator/Dockerfile"

if ! env -i PATH=/usr/local/bin:/usr/bin:/bin \
  TMPDIR="${SCRATCH}" \
  AUTOSYNC_STAGE_DIR="${STAGE_1}" \
  LOOPWRIGHT_CANDIDATE_DIR="${CAND_BAD_DF}" \
  AUTOSYNC_XBUILD_RUNNER="${STUB_RUNNER}" \
  AUTOSYNC_TEST_LOG="${SCRATCH}/log-bad-df.txt" \
  bash "${CAND_BAD_DF}/.auto-sync/checks/validate_slicing.sh" >"${VAL_LOG_BAD_DF}" 2>&1 && \
  [[ ! -e "${CAND_BAD_DF}/.git" ]]; then
  pass "validate_slicing.sh fails closed on non-slicing Dockerfile ARG mutation (SKOPEO_VERSION)"
else
  tail -n 50 "${VAL_LOG_BAD_DF}" >&2 || true
  fail "validate_slicing.sh fails closed on non-slicing Dockerfile ARG mutation (SKOPEO_VERSION)"
fi

# 5d. Negative 2: broken vcann-rt patch hunk against target commit fails closed.
CAND_BAD_PATCH="${SCRATCH}/cand-bad-patch"
LOG_BAD_PATCH="${SCRATCH}/log-bad-patch.txt"
VAL_LOG_BAD_PATCH="${SCRATCH}/val-bad-patch.log"
: > "${LOG_BAD_PATCH}"
make_gitless_candidate "${CAND_BAD_PATCH}"
set_commits_in_candidate "${CAND_BAD_PATCH}" "${HAMI_PINNED}" "${UBS_BROKEN_HEAD}"

if ! env -i PATH=/usr/local/bin:/usr/bin:/bin \
  TMPDIR="${SCRATCH}" \
  AUTOSYNC_STAGE_DIR="${STAGE_2}" \
  LOOPWRIGHT_CANDIDATE_DIR="${CAND_BAD_PATCH}" \
  AUTOSYNC_XBUILD_RUNNER="${STUB_RUNNER}" \
  AUTOSYNC_TEST_LOG="${LOG_BAD_PATCH}" \
  bash "${CAND_BAD_PATCH}/.auto-sync/checks/validate_slicing.sh" >"${VAL_LOG_BAD_PATCH}" 2>&1 && \
  [[ ! -e "${CAND_BAD_PATCH}/.git" ]] && \
  [[ ! -s "${LOG_BAD_PATCH}" ]]; then
  pass "validate_slicing.sh fails closed on broken vcann-rt patch hunk before xbuild"
else
  tail -n 50 "${VAL_LOG_BAD_PATCH}" >&2 || true
  fail "validate_slicing.sh fails closed on broken vcann-rt patch hunk before xbuild"
fi

# 5e. Negative 3: AUTOSYNC_STAGE_DIR set without baseline Dockerfile fails closed.
MISSING_BASE_STAGE="${SCRATCH}/missing-base-stage"
VAL_LOG_MISSING_BASE="${SCRATCH}/val-missing-base.log"
mkdir -p "${MISSING_BASE_STAGE}"
if ! env -i PATH=/usr/local/bin:/usr/bin:/bin \
  TMPDIR="${SCRATCH}" \
  AUTOSYNC_STAGE_DIR="${MISSING_BASE_STAGE}" \
  LOOPWRIGHT_CANDIDATE_DIR="${CAND_NV}" \
  AUTOSYNC_XBUILD_RUNNER="${STUB_RUNNER}" \
  AUTOSYNC_TEST_LOG="${SCRATCH}/log-missing-base.txt" \
  bash "${CAND_NV}/.auto-sync/checks/validate_slicing.sh" >"${VAL_LOG_MISSING_BASE}" 2>&1; then
  pass "validate_slicing.sh fails closed when AUTOSYNC_STAGE_DIR lacks baseline Dockerfile"
else
  tail -n 50 "${VAL_LOG_MISSING_BASE}" >&2 || true
  fail "validate_slicing.sh fails closed when AUTOSYNC_STAGE_DIR lacks baseline Dockerfile"
fi

# 5f. Negative 4: AUTOSYNC_STAGE_DIR set without ubs-virt/.git fails closed.
MISSING_UBS_STAGE="${SCRATCH}/missing-ubs-stage"
VAL_LOG_MISSING_UBS="${SCRATCH}/val-missing-ubs.log"
mkdir -p "${MISSING_UBS_STAGE}"
cp -R "${STAGE_1}/baseline" "${MISSING_UBS_STAGE}/baseline"
if ! env -i PATH=/usr/local/bin:/usr/bin:/bin \
  TMPDIR="${SCRATCH}" \
  AUTOSYNC_STAGE_DIR="${MISSING_UBS_STAGE}" \
  LOOPWRIGHT_CANDIDATE_DIR="${CAND_NV}" \
  AUTOSYNC_XBUILD_RUNNER="${STUB_RUNNER}" \
  AUTOSYNC_TEST_LOG="${SCRATCH}/log-missing-ubs.txt" \
  bash "${CAND_NV}/.auto-sync/checks/validate_slicing.sh" >"${VAL_LOG_MISSING_UBS}" 2>&1 && \
  grep -q "ubs-virt" "${VAL_LOG_MISSING_UBS}"; then
  pass "validate_slicing.sh fails closed when AUTOSYNC_STAGE_DIR lacks ubs-virt/.git"
else
  tail -n 50 "${VAL_LOG_MISSING_UBS}" >&2 || true
  fail "validate_slicing.sh fails closed when AUTOSYNC_STAGE_DIR lacks ubs-virt/.git"
fi

# 5g. Negative 5: unset AUTOSYNC_STAGE_DIR fails closed unless AUTOSYNC_ALLOW_MISSING_STAGE_DIR=true.
VAL_LOG_UNSET_STAGE="${SCRATCH}/val-unset-stage.log"
if ! env -i PATH=/usr/local/bin:/usr/bin:/bin \
  TMPDIR="${SCRATCH}" \
  LOOPWRIGHT_CANDIDATE_DIR="${CAND_NV}" \
  AUTOSYNC_XBUILD_RUNNER="${STUB_RUNNER}" \
  AUTOSYNC_TEST_LOG="${SCRATCH}/log-unset-stage.txt" \
  bash "${CAND_NV}/.auto-sync/checks/validate_slicing.sh" >"${VAL_LOG_UNSET_STAGE}" 2>&1; then
  pass "validate_slicing.sh fails closed when AUTOSYNC_STAGE_DIR is unset"
else
  tail -n 50 "${VAL_LOG_UNSET_STAGE}" >&2 || true
  fail "validate_slicing.sh fails closed when AUTOSYNC_STAGE_DIR is unset"
fi

if (( FAILS > 0 )); then
  printf 'SLICING SELFTEST FAILED: %d failure(s), %d passed\n' "${FAILS}" "${PASSES}" >&2
  exit 1
fi
printf 'SLICING SELFTEST PASSED: %d check(s) passed\n' "${PASSES}"
