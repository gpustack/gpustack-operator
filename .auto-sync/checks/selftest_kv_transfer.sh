#!/usr/bin/env bash
# .auto-sync/checks/selftest_kv_transfer.sh - self-tests for kv-transfer-sync domain.
set -euo pipefail
export PYTHONDONTWRITEBYTECODE=1

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
DEFAULT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd -P)"
ROOT_DIR="${1:-${DEFAULT_ROOT}}"
ROOT_DIR="$(cd "${ROOT_DIR}" && pwd -P)"

PREFLIGHT_SCRIPT="${ROOT_DIR}/.auto-sync/scripts/preflight-kv-transfer.sh"
VALIDATE_SCRIPT="${ROOT_DIR}/.auto-sync/checks/validate_kv_transfer.sh"

PASSES=0
FAILS=0
SCRATCH="$(cd "$(mktemp -d)" && pwd -P)"
trap 'rm -rf "${SCRATCH}"' EXIT

pass() {
  printf 'PASS  %s\n' "$1"
  PASSES=$((PASSES + 1))
}

fail() {
  printf 'FAIL  %s\n' "$1" >&2
  FAILS=$((FAILS + 1))
}

assert_ok() {
  local label="$1"
  shift
  if "$@"; then
    pass "${label}"
  else
    fail "${label}"
  fi
}

assert_fails() {
  local label="$1"
  shift
  if "$@" >/dev/null 2>&1; then
    fail "${label} (expected non-zero exit)"
  else
    pass "${label}"
  fi
}

echo "== kv-transfer-sync domain self-tests =="

for req in "${PREFLIGHT_SCRIPT}" "${VALIDATE_SCRIPT}"; do
  if [[ ! -x "${req}" ]]; then
    fail "missing or non-executable script: ${req#"${ROOT_DIR}/"}"
    echo "KV-TRANSFER SELFTEST FAILED: ${FAILS} failure(s), ${PASSES} passed" >&2
    exit 1
  fi
done

# Build an isolated fake repo checkout for preflight tests so real working tree is never touched.
FAKE_REPO="${SCRATCH}/repo"
mkdir -p "${FAKE_REPO}"
git -C "${FAKE_REPO}" init -q -b main
git -C "${FAKE_REPO}" config user.name "selftest"
git -C "${FAKE_REPO}" config user.email "selftest@gpustack.local"
git -C "${FAKE_REPO}" config commit.gpgsign false

copy_repo_file() {
  local rel="$1"
  mkdir -p "${FAKE_REPO}/$(dirname "${rel}")"
  cp "${ROOT_DIR}/${rel}" "${FAKE_REPO}/${rel}"
}

for rel in \
  "pkg/worker/settings/value.go" \
  "docs/reference/settings.md" \
  "deploy/gpustack-operator/chart/values.yaml" \
  "deploy/gpustack-operator/chart/values.schema.json" \
  "deploy/gpustack-operator/chart/README.md" \
  "pack/mirrored-mooncake/Dockerfile" \
  "pack/model-transfer/Dockerfile" \
  "pack/model-transfer/patches/hixl/001_channel_generation.patch" \
  "pack/mirrored-modelexpress-server/Dockerfile"; do
  copy_repo_file "${rel}"
done

mkdir -p "${FAKE_REPO}/.auto-sync/scripts" "${FAKE_REPO}/.auto-sync/checks" "${FAKE_REPO}/.auto-sync/proposals"
cp "${ROOT_DIR}/.auto-sync/scripts/lib-preflight.sh" "${FAKE_REPO}/.auto-sync/scripts/lib-preflight.sh"
cp "${PREFLIGHT_SCRIPT}" "${FAKE_REPO}/.auto-sync/scripts/preflight-kv-transfer.sh"
cp "${ROOT_DIR}/.auto-sync/checks/lib.sh" "${FAKE_REPO}/.auto-sync/checks/lib.sh"
cp "${ROOT_DIR}/.auto-sync/checks/dockerfile_arg.py" "${FAKE_REPO}/.auto-sync/checks/dockerfile_arg.py"
cp "${VALIDATE_SCRIPT}" "${FAKE_REPO}/.auto-sync/checks/validate_kv_transfer.sh"
if [[ -f "${ROOT_DIR}/.auto-sync/proposals/kv-transfer.json" ]]; then
  cp "${ROOT_DIR}/.auto-sync/proposals/kv-transfer.json" "${FAKE_REPO}/.auto-sync/proposals/kv-transfer.json"
fi

git -C "${FAKE_REPO}" add -A
git -C "${FAKE_REPO}" commit -q -m "baseline"

# Build offline fixtures for preflight-kv-transfer.sh (immutable template copied per test case).
FIXTURE_BASE="${SCRATCH}/fixtures-base"
mkdir -p "${FIXTURE_BASE}/pypi" "${FIXTURE_BASE}/repos"
FIXTURE_DIR="${FIXTURE_BASE}"

clone_fixture() {
  local dst="$1"
  rm -rf "${dst}"
  cp -R "${FIXTURE_BASE}" "${dst}"
}

# PyPI JSON fixtures: 1.5.3 has all three packages with x86_64+aarch64; 1.6.0 lacks nixl-cu13 aarch64.
cat > "${FIXTURE_DIR}/pypi/nixl.json" <<'JSON'
{
  "releases": {
    "1.5.2": [{"packagetype": "bdist_wheel", "filename": "nixl-1.5.2-py3-none-any.whl", "yanked": false}],
    "1.5.3": [{"packagetype": "bdist_wheel", "filename": "nixl-1.5.3-py3-none-any.whl", "yanked": false}],
    "1.6.0": [{"packagetype": "bdist_wheel", "filename": "nixl-1.6.0-py3-none-any.whl", "yanked": false}]
  }
}
JSON
cat > "${FIXTURE_DIR}/pypi/nixl-cu12.json" <<'JSON'
{
  "releases": {
    "1.5.2": [
      {"packagetype": "bdist_wheel", "filename": "nixl_cu12-1.5.2-cp312-cp312-manylinux_2_28_x86_64.whl", "yanked": false},
      {"packagetype": "bdist_wheel", "filename": "nixl_cu12-1.5.2-cp312-cp312-manylinux_2_28_aarch64.whl", "yanked": false}
    ],
    "1.5.3": [
      {"packagetype": "bdist_wheel", "filename": "nixl_cu12-1.5.3-cp312-cp312-manylinux_2_28_x86_64.whl", "yanked": false},
      {"packagetype": "bdist_wheel", "filename": "nixl_cu12-1.5.3-cp312-cp312-manylinux_2_28_aarch64.whl", "yanked": false}
    ],
    "1.6.0": [
      {"packagetype": "bdist_wheel", "filename": "nixl_cu12-1.6.0-cp312-cp312-manylinux_2_28_x86_64.whl", "yanked": false},
      {"packagetype": "bdist_wheel", "filename": "nixl_cu12-1.6.0-cp312-cp312-manylinux_2_28_aarch64.whl", "yanked": false}
    ]
  }
}
JSON
cat > "${FIXTURE_DIR}/pypi/nixl-cu13.json" <<'JSON'
{
  "releases": {
    "1.5.2": [
      {"packagetype": "bdist_wheel", "filename": "nixl_cu13-1.5.2-cp312-cp312-manylinux_2_28_x86_64.whl", "yanked": false},
      {"packagetype": "bdist_wheel", "filename": "nixl_cu13-1.5.2-cp312-cp312-manylinux_2_28_aarch64.whl", "yanked": false}
    ],
    "1.5.3": [
      {"packagetype": "bdist_wheel", "filename": "nixl_cu13-1.5.3-cp312-cp312-manylinux_2_28_x86_64.whl", "yanked": false},
      {"packagetype": "bdist_wheel", "filename": "nixl_cu13-1.5.3-cp312-cp312-manylinux_2_28_aarch64.whl", "yanked": false}
    ],
    "1.6.0": [
      {"packagetype": "bdist_wheel", "filename": "nixl_cu13-1.6.0-cp312-cp312-manylinux_2_28_x86_64.whl", "yanked": false}
    ]
  }
}
JSON

# Create mock git repos for hixl, gpustack-runner, nixl, ucx, modelexpress.
init_mock_repo() {
  local dir="$1"
  mkdir -p "${dir}"
  git -C "${dir}" init -q -b main
  git -C "${dir}" config user.name "upstream"
  git -C "${dir}" config user.email "upstream@example.com"
  git -C "${dir}" config commit.gpgsign false
}

HIXL_SRC="${FIXTURE_DIR}/repos/hixl"
init_mock_repo "${HIXL_SRC}"
mkdir -p "${HIXL_SRC}/src/hixl/cs"
cat > "${HIXL_SRC}/src/hixl/cs/hixl_cs_server.cc" <<'CPP'
// line 1
// line 2
// line 3
// line 4
// line 5
// line 6
// line 7
// line 8
// line 9
// line 10
// line 11
#include <atomic>
#include <cinttypes>
// line 14
// line 15
// line 16
// line 17
// line 18
// line 19
// line 20
// line 21
// line 22
// line 23
// line 24
// line 25
// line 26
// line 27
// line 28
// line 29
// line 30
// line 31
// line 32
// line 33
// line 34
// line 35
// line 36
// line 37
// line 38
// line 39
// line 40
std::atomic<uint64_t> g_next_server_channel_index{0UL};

CPP
git -C "${HIXL_SRC}" add -A
git -C "${HIXL_SRC}" commit -q -m "hixl base"
HIXL_FIXTURE_SHA="$(git -C "${HIXL_SRC}" rev-parse HEAD)"

# Make runner match FAKE_REPO's HIXL_COMMIT and patch initially, or update runner commit dynamically.
RUNNER_SRC="${FIXTURE_DIR}/repos/gpustack-runner"
init_mock_repo "${RUNNER_SRC}"
mkdir -p "${RUNNER_SRC}/pack/cann/patches/hixl"
cat > "${RUNNER_SRC}/pack/cann/Dockerfile.vllm" <<EOF
ARG VLLM_HIXL_COMMIT="60dbe9d1d7d9a7eef04bc3dbf84edbad2188230a"
EOF
cp "${FAKE_REPO}/pack/model-transfer/patches/hixl/001_channel_generation.patch" \
  "${RUNNER_SRC}/pack/cann/patches/hixl/001_channel_generation.patch"
git -C "${RUNNER_SRC}" add -A
git -C "${RUNNER_SRC}" commit -q -m "runner base"

cat > "${FIXTURE_DIR}/mooncake-release.json" <<'JSON'
[
  {"tag_name": "v0.3.14-rc1", "prerelease": true, "draft": false},
  {"tag_name": "v0.3.14", "prerelease": false, "draft": false},
  {"tag_name": "v0.3.13.post1", "prerelease": false, "draft": false}
]
JSON

cat > "${FIXTURE_DIR}/upstream.json" <<'JSON'
{
  "nixl_rocm": "1683cf3b7f3d11674c03c5e861cea22339876c96",
  "ucx_rocm": "2ff0b47ce7626584cb9b56c647d08f712c6c3edf",
  "hixl": "60dbe9d1d7d9a7eef04bc3dbf84edbad2188230a",
  "modelexpress": "ac6d03a87a550c8188c3ed59f187b90d4f0554f5"
}
JSON

# Manifests JSON: 0.3.13.post1-cpu published, 0.3.14-cpu UNPUBLISHED initially.
cat > "${FIXTURE_DIR}/image-manifests.json" <<'JSON'
{
  "images": {
    "docker.io/gpustack/mirrored-mooncake:0.3.13.post1-cpu": {
      "published": true,
      "amd64Digest": "sha256:1111111111111111111111111111111111111111111111111111111111111111",
      "arm64Digest": "sha256:2222222222222222222222222222222222222222222222222222222222222222"
    },
    "docker.io/gpustack/mirrored-mooncake:0.3.14-cpu": {
      "published": false,
      "amd64Digest": "",
      "arm64Digest": ""
    },
    "docker.io/gpustack/mirrored-modelexpress-server:v0.7.0": {
      "published": true,
      "amd64Digest": "sha256:3333333333333333333333333333333333333333333333333333333333333333",
      "arm64Digest": "sha256:4444444444444444444444444444444444444444444444444444444444444444"
    },
    "docker.io/gpustack/model-transfer:v0.1.0-cuda12.9": {
      "published": true,
      "amd64Digest": "sha256:5555555555555555555555555555555555555555555555555555555555555555",
      "arm64Digest": "sha256:6666666666666666666666666666666666666666666666666666666666666666"
    },
    "docker.io/gpustack/model-transfer:v0.1.0-rocm7.2": {
      "published": true,
      "amd64Digest": "sha256:7777777777777777777777777777777777777777777777777777777777777777",
      "arm64Digest": ""
    },
    "docker.io/gpustack/model-transfer:v0.1.0-cann9.2-a5": {
      "published": true,
      "amd64Digest": "sha256:8888888888888888888888888888888888888888888888888888888888888888",
      "arm64Digest": "sha256:9999999999999999999999999999999999999999999999999999999999999999"
    }
  }
}
JSON

STAGE_DIR="${SCRATCH}/stage-kv"
GHA_OUT="${SCRATCH}/gha-output.txt"

# 1. Preflight Stage A: new Mooncake 0.3.14 + NIXL 1.5.3 (ignoring incomplete 1.6.0).
FIX_1="${SCRATCH}/fix-1"
clone_fixture "${FIX_1}"
: > "${GHA_OUT}"
BASE_SHA_KV="$(git -C "${FAKE_REPO}" rev-parse HEAD)"
(
  cd "${FAKE_REPO}"
  AUTOSYNC_KV_FIXTURE_DIR="${FIX_1}" \
  AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
  GITHUB_OUTPUT="${GHA_OUT}" \
    bash "${FAKE_REPO}/.auto-sync/scripts/preflight-kv-transfer.sh" "${STAGE_DIR}"
)

if python3 - "${STAGE_DIR}" "${FAKE_REPO}" "${BASE_SHA_KV}" <<'PY'
import json
import os
import subprocess
import sys

stage, repo, base_sha = sys.argv[1:4]
with open(os.path.join(stage, "upstream-records.json"), "r", encoding="utf-8") as f:
    recs = {r["upstreamId"]: r for r in json.load(f)}
assert recs["mooncake"]["status"] == "available" and recs["mooncake"]["version"] == "0.3.14", recs["mooncake"]
assert recs["nixl-pypi"]["status"] == "available" and recs["nixl-pypi"]["version"] == "1.5.3", recs["nixl-pypi"]
assert recs["hixl"]["status"] == "up_to_date", recs["hixl"]
with open(os.path.join(stage, "kv-transfer-preflight-report.json"), "r", encoding="utf-8") as f:
    rep = json.load(f)
assert rep["stage"] == "recipe_sync", rep
assert rep["image_published"] is False, rep
assert rep["nixl_pypi"]["selected_version"] == "1.5.3", rep
src_rev = rep.get("source_revision", "")
assert src_rev and src_rev != base_sha, rep
ls = subprocess.check_output(
    ["git", "-C", repo, "ls-tree", "-r", src_rev, ".auto-sync/.stage/kv-transfer"], text=True
)
assert "100644 blob " in ls and "kv-transfer-preflight-report.json" in ls, ls
assert "100644 blob " in ls and "runner-001_channel_generation.patch" in ls, ls
assert "100644 blob " in ls and "upstream-records.json" in ls, ls
assert "120000 " not in ls, ls
assert subprocess.check_output(["git", "-C", repo, "status", "--porcelain"], text=True).strip() == ""
PY
then
  pass "preflight Stage A emits recipe_sync, selects NIXL 1.5.3, and stages .auto-sync/.stage/kv-transfer cleanly"
else
  fail "preflight Stage A emits recipe_sync, selects NIXL 1.5.3, and stages .auto-sync/.stage/kv-transfer cleanly"
fi

# 2. Preflight waiting state (no_change): recipes already updated to 0.3.14 & 1.5.3, image still unpublished.
python3 - "${FAKE_REPO}/pack/mirrored-mooncake/Dockerfile" "${FAKE_REPO}/pack/model-transfer/Dockerfile" <<'PY'
import sys
mc_path, mt_path = sys.argv[1:3]
with open(mc_path, "r", encoding="utf-8") as f:
    mc = f.read()
mc = mc.replace('ARG MOONCAKE_VERSION="0.3.13.post1"', 'ARG MOONCAKE_VERSION="0.3.14"', 1)
mc = mc.replace('0.3.10*|0.3.11*|0.3.12*|0.3.13*)', '0.3.10*|0.3.11*|0.3.12*|0.3.13*|0.3.14*)')
with open(mc_path, "w", encoding="utf-8") as f:
    f.write(mc)

with open(mt_path, "r", encoding="utf-8") as f:
    mt = f.read()
mt = mt.replace('ARG NIXL_VERSION="1.5.2"', 'ARG NIXL_VERSION="1.5.3"', 1)
with open(mt_path, "w", encoding="utf-8") as f:
    f.write(mt)
PY
git -C "${FAKE_REPO}" add -A
git -C "${FAKE_REPO}" commit -q -m "stage a landed"

FIX_2="${SCRATCH}/fix-2"
clone_fixture "${FIX_2}"
BASE_SHA_KV2="$(git -C "${FAKE_REPO}" rev-parse HEAD)"
(
  cd "${FAKE_REPO}"
  AUTOSYNC_KV_FIXTURE_DIR="${FIX_2}" \
  AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
    bash "${FAKE_REPO}/.auto-sync/scripts/preflight-kv-transfer.sh" "${STAGE_DIR}"
)

if python3 - "${STAGE_DIR}" "${BASE_SHA_KV2}" <<'PY'
import json
import os
import sys

stage, base_sha = sys.argv[1:3]
with open(os.path.join(stage, "upstream-records.json"), "r", encoding="utf-8") as f:
    recs = json.load(f)
assert all(r["status"] == "up_to_date" for r in recs), recs
with open(os.path.join(stage, "kv-transfer-preflight-report.json"), "r", encoding="utf-8") as f:
    rep = json.load(f)
assert rep["stage"] == "no_change", rep
assert rep["source_revision"] == base_sha, rep
PY
then
  pass "preflight emits up_to_date (no_change) and keeps source_revision == base_sha while Stage A image is unpublished"
else
  fail "preflight emits up_to_date (no_change) and keeps source_revision == base_sha while Stage A image is unpublished"
fi

# 2b. hicann/hixl HEAD != recipe HIXL_COMMIT while gpustack/runner matches recipe HIXL_COMMIT -> hixl stays up_to_date.
FIX_2B="${SCRATCH}/fix-2b"
clone_fixture "${FIX_2B}"
python3 - "${FIX_2B}/upstream.json" "${FIX_2B}/repos/gpustack-runner/pack/cann/Dockerfile.vllm" <<'PY'
import json
import sys
up_path, runner_df = sys.argv[1:3]
with open(up_path, "r", encoding="utf-8") as f:
    doc = json.load(f)
doc["hixl"] = "1111111111111111111111111111111111111111"
with open(up_path, "w", encoding="utf-8") as f:
    json.dump(doc, f, indent=2)
with open(runner_df, "r", encoding="utf-8") as f:
    txt = f.read()
assert "60dbe9d1d7d9a7eef04bc3dbf84edbad2188230a" in txt, txt
PY
(
  cd "${FAKE_REPO}"
  AUTOSYNC_KV_FIXTURE_DIR="${FIX_2B}" \
  AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
    bash "${FAKE_REPO}/.auto-sync/scripts/preflight-kv-transfer.sh" "${STAGE_DIR}"
)
if python3 - "${STAGE_DIR}" "${BASE_SHA_KV2}" <<'PY'
import json
import os
import sys

stage, base_sha = sys.argv[1:3]
with open(os.path.join(stage, "upstream-records.json"), "r", encoding="utf-8") as f:
    recs = {r["upstreamId"]: r for r in json.load(f)}
assert recs["hixl"]["status"] == "up_to_date", recs["hixl"]
assert recs["hixl"]["version"] == "60dbe9d1d7d9a7eef04bc3dbf84edbad2188230a", recs["hixl"]
assert "held at gpustack/runner VLLM_HIXL_COMMIT 60dbe9d1d7d9 while upstream hicann/hixl is at 111111111111" == recs["hixl"].get("reason"), recs["hixl"]
with open(os.path.join(stage, "kv-transfer-preflight-report.json"), "r", encoding="utf-8") as f:
    rep = json.load(f)
assert rep["stage"] == "no_change", rep
assert rep["source_revision"] == base_sha, rep
assert rep["enforce_runner_hixl_parity"] is True, rep
assert rep["hixl_upstream_head"] == "1111111111111111111111111111111111111111", rep
assert rep["hixl_upstream_held_by_runner"] is True, rep
assert rep["runner"]["hixl_commit"] == "60dbe9d1d7d9a7eef04bc3dbf84edbad2188230a", rep
PY
then
  pass "preflight holds hixl up_to_date when hicann/hixl advances ahead of gpustack/runner VLLM_HIXL_COMMIT"
else
  fail "preflight holds hixl up_to_date when hicann/hixl advances ahead of gpustack/runner VLLM_HIXL_COMMIT"
fi

# 3. Preflight Stage B (tag_promotion): 0.3.14-cpu now published multi-arch, value.go still on 0.3.13.post1-cpu.
FIX_3="${SCRATCH}/fix-3"
clone_fixture "${FIX_3}"
python3 - "${FIX_3}/image-manifests.json" <<'PY'
import json
import sys
p = sys.argv[1]
with open(p, "r", encoding="utf-8") as f:
    doc = json.load(f)
doc["images"]["docker.io/gpustack/mirrored-mooncake:0.3.14-cpu"] = {
    "published": True,
    "amd64Digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "arm64Digest": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
}
with open(p, "w", encoding="utf-8") as f:
    json.dump(doc, f, indent=2)
PY

(
  cd "${FAKE_REPO}"
  AUTOSYNC_KV_FIXTURE_DIR="${FIX_3}" \
  AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
    bash "${FAKE_REPO}/.auto-sync/scripts/preflight-kv-transfer.sh" "${STAGE_DIR}"
)

if python3 - "${STAGE_DIR}" "${FAKE_REPO}" "${BASE_SHA_KV2}" <<'PY'
import json
import os
import subprocess
import sys

stage, repo, base_sha = sys.argv[1:4]
with open(os.path.join(stage, "upstream-records.json"), "r", encoding="utf-8") as f:
    recs = {r["upstreamId"]: r for r in json.load(f)}
assert recs["mooncake"]["status"] == "available" and recs["mooncake"]["version"] == "0.3.14+stage-b", recs["mooncake"]
with open(os.path.join(stage, "kv-transfer-preflight-report.json"), "r", encoding="utf-8") as f:
    rep = json.load(f)
assert rep["stage"] == "tag_promotion", rep
assert rep["image_published"] is True, rep
src_rev = rep.get("source_revision", "")
assert src_rev and src_rev != base_sha, rep
ls = subprocess.check_output(
    ["git", "-C", repo, "ls-tree", "-r", src_rev, ".auto-sync/.stage/kv-transfer"], text=True
)
assert "100644 blob " in ls and "kv-transfer-preflight-report.json" in ls, ls
assert "120000 " not in ls, ls
assert subprocess.check_output(["git", "-C", repo, "status", "--porcelain"], text=True).strip() == ""
PY
then
  pass "preflight Stage B emits 0.3.14+stage-b and stages .auto-sync/.stage/kv-transfer cleanly"
else
  fail "preflight Stage B emits 0.3.14+stage-b and stages .auto-sync/.stage/kv-transfer cleanly"
fi

# 4. Preflight fully aligned (no_change) after value.go + settings.md updated to 0.3.14-cpu.
sed -i.bak 's|gpustack/mirrored-mooncake:0.3.13.post1-cpu|gpustack/mirrored-mooncake:0.3.14-cpu|' "${FAKE_REPO}/pkg/worker/settings/value.go"
sed -i.bak 's|gpustack/mirrored-mooncake:0.3.13.post1-cpu|gpustack/mirrored-mooncake:0.3.14-cpu|' "${FAKE_REPO}/docs/reference/settings.md"
rm -f "${FAKE_REPO}/pkg/worker/settings/value.go.bak" "${FAKE_REPO}/docs/reference/settings.md.bak"
git -C "${FAKE_REPO}" add -A
git -C "${FAKE_REPO}" commit -q -m "stage b landed"

FIX_4="${SCRATCH}/fix-4"
clone_fixture "${FIX_4}"
python3 - "${FIX_4}/image-manifests.json" <<'PY'
import json
import sys
p = sys.argv[1]
with open(p, "r", encoding="utf-8") as f:
    doc = json.load(f)
doc["images"]["docker.io/gpustack/mirrored-mooncake:0.3.14-cpu"] = {
    "published": True,
    "amd64Digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "arm64Digest": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
}
with open(p, "w", encoding="utf-8") as f:
    json.dump(doc, f, indent=2)
PY
(
  cd "${FAKE_REPO}"
  AUTOSYNC_KV_FIXTURE_DIR="${FIX_4}" \
  AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
    bash "${FAKE_REPO}/.auto-sync/scripts/preflight-kv-transfer.sh" "${STAGE_DIR}"
)

if python3 - "${STAGE_DIR}" <<'PY'
import json
import os
import sys

stage = sys.argv[1]
with open(os.path.join(stage, "upstream-records.json"), "r", encoding="utf-8") as f:
    recs = json.load(f)
assert all(r["status"] == "up_to_date" for r in recs), recs
with open(os.path.join(stage, "kv-transfer-preflight-report.json"), "r", encoding="utf-8") as f:
    rep = json.load(f)
assert rep["stage"] == "no_change", rep
PY
then
  pass "preflight emits up_to_date (no_change) when recipes and value.go are aligned"
else
  fail "preflight emits up_to_date (no_change) when recipes and value.go are aligned"
fi

# 4b. Preflight Stage B via repo_tags auto-discovery for model-transfer (no AUTOSYNC_*_TARGET_TAG).
# Verify: (1) v0.9.0-cuda13.0 is ignored for cuda12.9 flavor (major-line prefix),
# (2) v0.2.0-rocm7.2 with published=false is NOT promoted, and (3) once published=true it IS promoted.
FIX_4B="${SCRATCH}/fix-4b"
clone_fixture "${FIX_4B}"
python3 - "${FIX_4B}/upstream.json" "${FIX_4B}/image-manifests.json" <<'PY'
import json
import sys

up_p, man_p = sys.argv[1:3]
with open(up_p, "r", encoding="utf-8") as f:
    up = json.load(f)
up["repo_tags"] = {
    "gpustack/mirrored-modelexpress-server": ["v0.7.0"],
    "gpustack/model-transfer": [
        "v0.1.0-cuda12.9",
        "v0.2.0-cuda12.9",
        "v0.9.0-cuda13.0",
        "v0.2.0-rocm7.2",
        "v0.2.0-cann9.2-a5",
    ],
}
with open(up_p, "w", encoding="utf-8") as f:
    json.dump(up, f, indent=2)

with open(man_p, "r", encoding="utf-8") as f:
    man = json.load(f)
man["images"]["docker.io/gpustack/mirrored-mooncake:0.3.14-cpu"] = {
    "published": True,
    "amd64Digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "arm64Digest": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
}
man["images"]["docker.io/gpustack/model-transfer:v0.2.0-cuda12.9"] = {
    "published": True,
    "amd64Digest": "sha256:c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1",
    "arm64Digest": "sha256:d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1",
}
man["images"]["docker.io/gpustack/model-transfer:v0.9.0-cuda13.0"] = {
    "published": True,
    "amd64Digest": "sha256:c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9",
    "arm64Digest": "sha256:d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9",
}
man["images"]["docker.io/gpustack/model-transfer:v0.2.0-rocm7.2"] = {
    "published": False,
    "amd64Digest": "sha256:e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1",
    "arm64Digest": "",
}
man["images"]["docker.io/gpustack/model-transfer:v0.2.0-cann9.2-a5"] = {
    "published": True,
    "amd64Digest": "sha256:f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1",
    "arm64Digest": "sha256:0101010101010101010101010101010101010101010101010101010101010101",
}
with open(man_p, "w", encoding="utf-8") as f:
    json.dump(man, f, indent=2)
PY

(
  cd "${FAKE_REPO}"
  AUTOSYNC_KV_FIXTURE_DIR="${FIX_4B}" \
  AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
    bash "${FAKE_REPO}/.auto-sync/scripts/preflight-kv-transfer.sh" "${STAGE_DIR}"
)

if python3 - "${STAGE_DIR}" <<'PY'
import json
import os
import sys

stage = sys.argv[1]
with open(os.path.join(stage, "kv-transfer-preflight-report.json"), "r", encoding="utf-8") as f:
    rep = json.load(f)
mt = rep["targets"]["model_transfer_images"]
assert mt["nvidia"] == "gpustack/model-transfer:v0.2.0-cuda12.9", mt
assert mt["amd"] == "gpustack/model-transfer:v0.1.0-rocm7.2", mt
assert mt["ascend"] == "gpustack/model-transfer:v0.2.0-cann9.2-a5", mt
PY
then
  pass "preflight Stage B ignores cuda13.0 for cuda12.9 and keeps unpublished rocm7.2 unpromoted"
else
  fail "preflight Stage B ignores cuda13.0 for cuda12.9 and keeps unpublished rocm7.2 unpromoted"
fi

python3 - "${FIX_4B}/image-manifests.json" <<'PY'
import json
import sys

p = sys.argv[1]
with open(p, "r", encoding="utf-8") as f:
    man = json.load(f)
man["images"]["docker.io/gpustack/model-transfer:v0.2.0-rocm7.2"]["published"] = True
with open(p, "w", encoding="utf-8") as f:
    json.dump(man, f, indent=2)
PY

(
  cd "${FAKE_REPO}"
  AUTOSYNC_KV_FIXTURE_DIR="${FIX_4B}" \
  AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
    bash "${FAKE_REPO}/.auto-sync/scripts/preflight-kv-transfer.sh" "${STAGE_DIR}"
)

if python3 - "${STAGE_DIR}" <<'PY'
import json
import os
import sys

stage = sys.argv[1]
with open(os.path.join(stage, "upstream-records.json"), "r", encoding="utf-8") as f:
    recs = {r["upstreamId"]: r for r in json.load(f)}
assert recs["gpustack-runner"]["status"] == "available", recs["gpustack-runner"]
assert recs["gpustack-runner"]["version"] == "aligned+stage-b", recs["gpustack-runner"]
with open(os.path.join(stage, "kv-transfer-preflight-report.json"), "r", encoding="utf-8") as f:
    rep = json.load(f)
assert rep["stage"] == "tag_promotion", rep
assert rep["image_published"] is True, rep
mt = rep["targets"]["model_transfer_images"]
assert mt["nvidia"] == "gpustack/model-transfer:v0.2.0-cuda12.9", mt
assert mt["amd"] == "gpustack/model-transfer:v0.2.0-rocm7.2", mt
assert mt["ascend"] == "gpustack/model-transfer:v0.2.0-cann9.2-a5", mt
PY
then
  pass "preflight Stage B auto-discovers model-transfer tags via repo_tags without AUTOSYNC_*_TARGET_TAG"
else
  fail "preflight Stage B auto-discovers model-transfer tags via repo_tags without AUTOSYNC_*_TARGET_TAG"
fi

# 4c. Missing PyPI fixture file under AUTOSYNC_KV_FIXTURE_DIR fails closed.
FIX_MISSING_PYPI="${SCRATCH}/fix-missing-pypi"
clone_fixture "${FIX_MISSING_PYPI}"
rm -f "${FIX_MISSING_PYPI}/pypi/nixl-cu13.json"
run_kv_preflight_with_fixture() {
  (
    cd "$1"
    AUTOSYNC_KV_FIXTURE_DIR="$2" \
    AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
      bash "$1/.auto-sync/scripts/preflight-kv-transfer.sh" "$3"
  )
}
assert_fails "preflight fails closed when AUTOSYNC_KV_FIXTURE_DIR is missing pypi/nixl-cu13.json" \
  run_kv_preflight_with_fixture "${FAKE_REPO}" "${FIX_MISSING_PYPI}" "${SCRATCH}/stage-missing-pypi"

# 5. Gitless candidate validator tests (Mooncake 5-block parser, two-stage gate, shared-file guards, HIXL parity, ModelExpress build).
VAL_STAGE="${SCRATCH}/val-stage"
mkdir -p "${VAL_STAGE}/baseline/pkg/worker/settings" \
  "${VAL_STAGE}/baseline/docs/reference" \
  "${VAL_STAGE}/baseline/deploy/gpustack-operator/chart" \
  "${VAL_STAGE}/baseline/pack/mirrored-mooncake" \
  "${VAL_STAGE}/baseline/pack/model-transfer/patches/hixl" \
  "${VAL_STAGE}/baseline/pack/mirrored-modelexpress-server" \
  "${VAL_STAGE}/go-cache" "${VAL_STAGE}/go-mod" "${VAL_STAGE}/deps-cache"

cp "${ROOT_DIR}/pkg/worker/settings/value.go" "${VAL_STAGE}/baseline/pkg/worker/settings/value.go"
cp "${ROOT_DIR}/docs/reference/settings.md" "${VAL_STAGE}/baseline/docs/reference/settings.md"
cp "${ROOT_DIR}/deploy/gpustack-operator/chart/values.yaml" "${VAL_STAGE}/baseline/deploy/gpustack-operator/chart/values.yaml"
cp "${ROOT_DIR}/pack/mirrored-mooncake/Dockerfile" "${VAL_STAGE}/baseline/pack/mirrored-mooncake/Dockerfile"
cp "${ROOT_DIR}/pack/model-transfer/Dockerfile" "${VAL_STAGE}/baseline/pack/model-transfer/Dockerfile"
cp "${ROOT_DIR}/pack/model-transfer/patches/hixl/001_channel_generation.patch" \
  "${VAL_STAGE}/baseline/pack/model-transfer/patches/hixl/001_channel_generation.patch"
cp "${ROOT_DIR}/pack/mirrored-modelexpress-server/Dockerfile" \
  "${VAL_STAGE}/baseline/pack/mirrored-modelexpress-server/Dockerfile"

cp -R "${RUNNER_SRC}" "${VAL_STAGE}/gpustack-runner"
rm -rf "${VAL_STAGE}/gpustack-runner/.git"
cp -R "${HIXL_SRC}" "${VAL_STAGE}/hixl"

cat > "${VAL_STAGE}/image-manifests.json" <<'JSON'
{
  "schemaVersion": 1,
  "images": {
    "gpustack/mirrored-mooncake:0.3.13.post1-cpu": {
      "image": "gpustack/mirrored-mooncake:0.3.13.post1-cpu",
      "normalized": "docker.io/gpustack/mirrored-mooncake:0.3.13.post1-cpu",
      "published": true,
      "amd64Digest": "sha256:1111111111111111111111111111111111111111111111111111111111111111",
      "arm64Digest": "sha256:2222222222222222222222222222222222222222222222222222222222222222"
    },
    "gpustack/mirrored-mooncake:0.3.14-cpu": {
      "image": "gpustack/mirrored-mooncake:0.3.14-cpu",
      "normalized": "docker.io/gpustack/mirrored-mooncake:0.3.14-cpu",
      "published": true,
      "amd64Digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "arm64Digest": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
    },
    "gpustack/mirrored-mooncake:0.9.9-unpublished-cpu": {
      "image": "gpustack/mirrored-mooncake:0.9.9-unpublished-cpu",
      "normalized": "docker.io/gpustack/mirrored-mooncake:0.9.9-unpublished-cpu",
      "published": false,
      "amd64Digest": "",
      "arm64Digest": ""
    },
    "gpustack/mirrored-modelexpress-server:v0.7.0": {
      "image": "gpustack/mirrored-modelexpress-server:v0.7.0",
      "normalized": "docker.io/gpustack/mirrored-modelexpress-server:v0.7.0",
      "published": true,
      "amd64Digest": "sha256:3333333333333333333333333333333333333333333333333333333333333333",
      "arm64Digest": "sha256:4444444444444444444444444444444444444444444444444444444444444444"
    },
    "gpustack/model-transfer:v0.1.0-cuda12.9": {
      "image": "gpustack/model-transfer:v0.1.0-cuda12.9",
      "normalized": "docker.io/gpustack/model-transfer:v0.1.0-cuda12.9",
      "published": true,
      "amd64Digest": "sha256:5555555555555555555555555555555555555555555555555555555555555555",
      "arm64Digest": "sha256:6666666666666666666666666666666666666666666666666666666666666666"
    },
    "gpustack/model-transfer:v0.1.0-rocm7.2": {
      "image": "gpustack/model-transfer:v0.1.0-rocm7.2",
      "normalized": "docker.io/gpustack/model-transfer:v0.1.0-rocm7.2",
      "published": true,
      "amd64Digest": "sha256:7777777777777777777777777777777777777777777777777777777777777777",
      "arm64Digest": ""
    },
    "gpustack/model-transfer:v0.1.0-cann9.2-a5": {
      "image": "gpustack/model-transfer:v0.1.0-cann9.2-a5",
      "normalized": "docker.io/gpustack/model-transfer:v0.1.0-cann9.2-a5",
      "published": true,
      "amd64Digest": "sha256:8888888888888888888888888888888888888888888888888888888888888888",
      "arm64Digest": "sha256:9999999999999999999999999999999999999999999999999999999999999999"
    }
  }
}
JSON

cat > "${VAL_STAGE}/kv-transfer-preflight-report.json" <<'JSON'
{
  "schemaVersion": 1,
  "stage": "recipe_sync",
  "image_published": false,
  "enforce_runner_hixl_parity": true,
  "nixl_pypi": {
    "selected_version": "1.5.3",
    "recent_versions": ["1.5.3", "1.5.2"]
  }
}
JSON

CAND_DIR="${SCRATCH}/candidate"
make_gitless_cand() {
  rm -rf "${CAND_DIR}"
  mkdir -p "${CAND_DIR}/.auto-sync/checks" \
    "${CAND_DIR}/.auto-sync/proposals" \
    "${CAND_DIR}/pkg/worker/settings" \
    "${CAND_DIR}/docs/reference" \
    "${CAND_DIR}/deploy/gpustack-operator/chart" \
    "${CAND_DIR}/pack/mirrored-mooncake" \
    "${CAND_DIR}/pack/model-transfer/patches/hixl" \
    "${CAND_DIR}/pack/mirrored-modelexpress-server"
  cp "${ROOT_DIR}/.auto-sync/checks/lib.sh" "${CAND_DIR}/.auto-sync/checks/lib.sh"
  cp "${ROOT_DIR}/.auto-sync/checks/dockerfile_arg.py" "${CAND_DIR}/.auto-sync/checks/dockerfile_arg.py"
  cp "${VALIDATE_SCRIPT}" "${CAND_DIR}/.auto-sync/checks/validate_kv_transfer.sh"
  cp "${ROOT_DIR}/.auto-sync/proposals/kv-transfer.json" "${CAND_DIR}/.auto-sync/proposals/kv-transfer.json"
  cp "${VAL_STAGE}/baseline/pkg/worker/settings/value.go" "${CAND_DIR}/pkg/worker/settings/value.go"
  cp "${VAL_STAGE}/baseline/docs/reference/settings.md" "${CAND_DIR}/docs/reference/settings.md"
  cp "${VAL_STAGE}/baseline/deploy/gpustack-operator/chart/values.yaml" "${CAND_DIR}/deploy/gpustack-operator/chart/values.yaml"
  cp "${VAL_STAGE}/baseline/pack/mirrored-mooncake/Dockerfile" "${CAND_DIR}/pack/mirrored-mooncake/Dockerfile"
  cp "${VAL_STAGE}/baseline/pack/model-transfer/Dockerfile" "${CAND_DIR}/pack/model-transfer/Dockerfile"
  cp "${VAL_STAGE}/baseline/pack/model-transfer/patches/hixl/001_channel_generation.patch" \
    "${CAND_DIR}/pack/model-transfer/patches/hixl/001_channel_generation.patch"
  cp "${VAL_STAGE}/baseline/pack/mirrored-modelexpress-server/Dockerfile" \
    "${CAND_DIR}/pack/mirrored-modelexpress-server/Dockerfile"
  # Align candidate HIXL_COMMIT with HIXL_FIXTURE_SHA when testing patch dry-run separately;
  # baseline runner and model-transfer already share 60dbe9d1d7d9a7eef04bc3dbf84edbad2188230a.
  # Add HIXL_FIXTURE_SHA only after testing baseline.
}

set_kv_proposal() {
  local stage="$1"
  shift
  python3 - "${CAND_DIR}/.auto-sync/proposals/kv-transfer.json" "${stage}" "$@" <<'PY'
import json
import sys

p, stage = sys.argv[1:3]
with open(p, "r", encoding="utf-8") as f:
    doc = json.load(f)
doc["stage"] = stage
for kv in sys.argv[3:]:
    k, v = kv.split("=", 1)
    doc["versions"][k] = v
with open(p, "w", encoding="utf-8") as f:
    json.dump(doc, f, indent=2)
    f.write("\n")
PY
}

run_validate() {
  AUTOSYNC_STAGE_DIR="${VAL_STAGE}" \
  LOOPWRIGHT_CANDIDATE_DIR="${CAND_DIR}" \
  AUTOSYNC_FAST_SELFTEST=true \
    bash "${CAND_DIR}/.auto-sync/checks/validate_kv_transfer.sh"
}

# Remove hixl git check for baseline tests where HIXL_COMMIT is upstream real SHA not in mock hixl repo,
# then add HIXL_FIXTURE_SHA test explicitly below.
mv "${VAL_STAGE}/hixl" "${SCRATCH}/hixl-repo-backup"

# 5a. Baseline unchanged (0.3.13.post1) passes in gitless candidate dir.
make_gitless_cand
assert_ok "validate_kv_transfer passes on baseline 0.3.13.post1 in gitless candidate dir" run_validate

# 5b. Mooncake 5-block case parser: bump ARG MOONCAKE_VERSION="0.3.14" without updating all 4 stage guards -> FAIL.
make_gitless_cand
set_kv_proposal "recipe_sync" "mooncake=0.3.14"
sed -i.bak 's/^ARG MOONCAKE_VERSION="0.3.13.post1"/ARG MOONCAKE_VERSION="0.3.14"/' "${CAND_DIR}/pack/mirrored-mooncake/Dockerfile"
rm -f "${CAND_DIR}/pack/mirrored-mooncake/Dockerfile.bak"
assert_fails "validate_kv_transfer fails closed when ARG MOONCAKE_VERSION=0.3.14 lacks stage case guards" run_validate

# Update only 3 of the 4 stage guards -> MUST still fail closed!
python3 - "${CAND_DIR}/pack/mirrored-mooncake/Dockerfile" <<'PY'
import sys
p = sys.argv[1]
with open(p, "r", encoding="utf-8") as f:
    txt = f.read()
txt = txt.replace("0.3.10*|0.3.11*|0.3.12*|0.3.13*)", "0.3.10*|0.3.11*|0.3.12*|0.3.13*|0.3.14*)", 3)
with open(p, "w", encoding="utf-8") as f:
    f.write(txt)
PY
assert_fails "validate_kv_transfer fails closed when only 3 of 4 Mooncake stage guards include 0.3.14*" run_validate

# Update the 4th stage guard WITHOUT updating the 5th context_manager.cpp block -> MUST still fail closed!
python3 - "${CAND_DIR}/pack/mirrored-mooncake/Dockerfile" <<'PY'
import sys
p = sys.argv[1]
with open(p, "r", encoding="utf-8") as f:
    txt = f.read()
txt = txt.replace("0.3.10*|0.3.11*|0.3.12*|0.3.13*)", "0.3.10*|0.3.11*|0.3.12*|0.3.13*|0.3.14*)")
with open(p, "w", encoding="utf-8") as f:
    f.write(txt)
PY
assert_fails "validate_kv_transfer fails closed when 5th context_manager.cpp case block omits 0.3.14*" run_validate

# Update the 5th context_manager.cpp case block too -> Stage A PASSES (value.go untouched, even for unpublished tag).
python3 - "${CAND_DIR}/pack/mirrored-mooncake/Dockerfile" <<'PY'
import sys
p = sys.argv[1]
with open(p, "r", encoding="utf-8") as f:
    txt = f.read()
txt = txt.replace("0.3.13*)", "0.3.13*|0.3.14*)", 1)
with open(p, "w", encoding="utf-8") as f:
    f.write(txt)
PY
assert_ok "validate_kv_transfer Stage A passes when all 5 Mooncake case blocks handle 0.3.14* and value.go untouched" run_validate

# 5c. Unpublished tag written to value.go -> FAIL.
make_gitless_cand
sed -i.bak 's|gpustack/mirrored-mooncake:0.3.13.post1-cpu|gpustack/mirrored-mooncake:0.9.9-unpublished-cpu|' \
  "${CAND_DIR}/pkg/worker/settings/value.go" "${CAND_DIR}/docs/reference/settings.md"
rm -f "${CAND_DIR}/pkg/worker/settings/value.go.bak" "${CAND_DIR}/docs/reference/settings.md.bak"
assert_fails "validate_kv_transfer fails closed when unpublished tag is written to value.go" run_validate

# 5d. Unpublished tag written to values.yaml -> FAIL.
make_gitless_cand
sed -i.bak 's|nvidia: gpustack/model-transfer:v0.1.0-cuda12.9|nvidia: gpustack/model-transfer:v9.9.9-cuda12.9|' \
  "${CAND_DIR}/deploy/gpustack-operator/chart/values.yaml"
rm -f "${CAND_DIR}/deploy/gpustack-operator/chart/values.yaml.bak"
assert_fails "validate_kv_transfer fails closed when unpublished tag is written to values.yaml" run_validate

# 5e. Stage B (value.go + settings.md updated to published multi-arch 0.3.14-cpu) -> PASS.
make_gitless_cand
set_kv_proposal "tag_promotion" "mooncake=0.3.13.post1+stage-b"
sed -i.bak 's|gpustack/mirrored-mooncake:0.3.13.post1-cpu|gpustack/mirrored-mooncake:0.3.14-cpu|' \
  "${CAND_DIR}/pkg/worker/settings/value.go" "${CAND_DIR}/docs/reference/settings.md"
rm -f "${CAND_DIR}/pkg/worker/settings/value.go.bak" "${CAND_DIR}/docs/reference/settings.md.bak"
assert_ok "validate_kv_transfer Stage B passes when value.go + settings.md use published multi-arch tag" run_validate

# 5f. Mutating ModelDeploymentRouterImage in value.go -> FAIL.
make_gitless_cand
sed -i.bak -E 's|(setting\.InitializeFromEnv\("gpustack/llm-router:)[^"]+(")|\1v99.0.0\2|' \
  "${CAND_DIR}/pkg/worker/settings/value.go"
rm -f "${CAND_DIR}/pkg/worker/settings/value.go.bak"
if cmp -s "${VAL_STAGE}/baseline/pkg/worker/settings/value.go" "${CAND_DIR}/pkg/worker/settings/value.go"; then
  fail "selftest setup: ModelDeploymentRouterImage mutation in value.go was a no-op"
fi
assert_fails "validate_kv_transfer fails closed on ModelDeploymentRouterImage mutation in value.go" run_validate

# 5g. HIXL patch byte mismatch against gpustack/runner fixture -> FAIL.
make_gitless_cand
printf '// extra comment\n' >> "${CAND_DIR}/pack/model-transfer/patches/hixl/001_channel_generation.patch"
assert_fails "validate_kv_transfer fails closed on HIXL patch byte mismatch against gpustack/runner" run_validate

# 5h. HIXL commit mismatch against gpustack/runner VLLM_HIXL_COMMIT -> FAIL.
make_gitless_cand
sed -i.bak 's|^ARG HIXL_COMMIT="[^"]*"|ARG HIXL_COMMIT="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"|' \
  "${CAND_DIR}/pack/model-transfer/Dockerfile"
rm -f "${CAND_DIR}/pack/model-transfer/Dockerfile.bak"
assert_fails "validate_kv_transfer fails closed on HIXL_COMMIT mismatch against gpustack/runner" run_validate

# 5i. HIXL patch dry-run against staged hixl tree.
mv "${SCRATCH}/hixl-repo-backup" "${VAL_STAGE}/hixl"
sed -i.bak "s|60dbe9d1d7d9a7eef04bc3dbf84edbad2188230a|${HIXL_FIXTURE_SHA}|" "${VAL_STAGE}/gpustack-runner/pack/cann/Dockerfile.vllm"
rm -f "${VAL_STAGE}/gpustack-runner/pack/cann/Dockerfile.vllm.bak"
make_gitless_cand
set_kv_proposal "recipe_sync" "hixl=${HIXL_FIXTURE_SHA}"
sed -i.bak "s|60dbe9d1d7d9a7eef04bc3dbf84edbad2188230a|${HIXL_FIXTURE_SHA}|" "${CAND_DIR}/pack/model-transfer/Dockerfile"
rm -f "${CAND_DIR}/pack/model-transfer/Dockerfile.bak"
assert_ok "validate_kv_transfer passes HIXL patch --dry-run against staged hixl repo" run_validate

# 5i-2. Dirty tracked LFS file at HEAD in staged hixl repo still checks out earlier HIXL_COMMIT cleanly.
mkdir -p "${VAL_STAGE}/hixl/docs/en/figures"
printf '*.png filter=lfs diff=lfs merge=lfs -text\n' > "${VAL_STAGE}/hixl/.gitattributes"
printf 'png-placeholder\n' > "${VAL_STAGE}/hixl/docs/en/figures/benchmark_perf_a2_dual.png"
git -C "${VAL_STAGE}/hixl" add .gitattributes docs/en/figures/benchmark_perf_a2_dual.png
git -C "${VAL_STAGE}/hixl" commit -q -m "add lfs png at head"
git -C "${VAL_STAGE}/hixl" config filter.lfs.smudge false
git -C "${VAL_STAGE}/hixl" config filter.lfs.clean false
git -C "${VAL_STAGE}/hixl" config filter.lfs.required true
printf 'dirty-lfs-pointer-or-bytes\n' > "${VAL_STAGE}/hixl/docs/en/figures/benchmark_perf_a2_dual.png"
assert_ok "validate_kv_transfer checks out earlier HIXL_COMMIT cleanly despite dirty LFS-tracked file at HEAD" run_validate

# Missing HIXL_COMMIT fails with explicit commit-not-present message.
MISSING_HIXL_SHA="1234567890abcdef1234567890abcdef12345678"
sed -i.bak "s|${HIXL_FIXTURE_SHA}|${MISSING_HIXL_SHA}|" "${VAL_STAGE}/gpustack-runner/pack/cann/Dockerfile.vllm"
sed -i.bak "s|${HIXL_FIXTURE_SHA}|${MISSING_HIXL_SHA}|" "${CAND_DIR}/pack/model-transfer/Dockerfile"
rm -f "${VAL_STAGE}/gpustack-runner/pack/cann/Dockerfile.vllm.bak" "${CAND_DIR}/pack/model-transfer/Dockerfile.bak"
set_kv_proposal "recipe_sync" "hixl=${MISSING_HIXL_SHA}"
missing_hixl_err=""
if ! missing_hixl_err="$(run_validate 2>&1)" && \
   grep -q "HIXL_COMMIT ${MISSING_HIXL_SHA} not present in staged hixl tree" <<<"${missing_hixl_err}"; then
  pass "validate_kv_transfer reports missing HIXL_COMMIT via cat-file -e"
else
  fail "validate_kv_transfer reports missing HIXL_COMMIT via cat-file -e (${missing_hixl_err})"
fi
sed -i.bak "s|${MISSING_HIXL_SHA}|${HIXL_FIXTURE_SHA}|" "${VAL_STAGE}/gpustack-runner/pack/cann/Dockerfile.vllm"
sed -i.bak "s|${MISSING_HIXL_SHA}|${HIXL_FIXTURE_SHA}|" "${CAND_DIR}/pack/model-transfer/Dockerfile"
rm -f "${VAL_STAGE}/gpustack-runner/pack/cann/Dockerfile.vllm.bak" "${CAND_DIR}/pack/model-transfer/Dockerfile.bak"
set_kv_proposal "recipe_sync" "hixl=${HIXL_FIXTURE_SHA}"
git -C "${VAL_STAGE}/hixl" config --unset filter.lfs.smudge || true
git -C "${VAL_STAGE}/hixl" config --unset filter.lfs.clean || true
git -C "${VAL_STAGE}/hixl" config --unset filter.lfs.required || true

# Corrupt target file in hixl fixture -> patch --dry-run MUST fail closed.
printf 'broken context\n' > "${VAL_STAGE}/hixl/src/hixl/cs/hixl_cs_server.cc"
git -C "${VAL_STAGE}/hixl" add -A
git -C "${VAL_STAGE}/hixl" commit -q -m "break patch"
HIXL_BROKEN_SHA="$(git -C "${VAL_STAGE}/hixl" rev-parse HEAD)"
sed -i.bak "s|${HIXL_FIXTURE_SHA}|${HIXL_BROKEN_SHA}|" "${VAL_STAGE}/gpustack-runner/pack/cann/Dockerfile.vllm"
sed -i.bak "s|${HIXL_FIXTURE_SHA}|${HIXL_BROKEN_SHA}|" "${CAND_DIR}/pack/model-transfer/Dockerfile"
rm -f "${VAL_STAGE}/gpustack-runner/pack/cann/Dockerfile.vllm.bak" "${CAND_DIR}/pack/model-transfer/Dockerfile.bak"
assert_fails "validate_kv_transfer fails closed when HIXL patch --dry-run fails" run_validate

# Restore valid hixl/runner state for ModelExpress test.
rm -rf "${VAL_STAGE}/hixl"
sed -i.bak "s|${HIXL_BROKEN_SHA}|60dbe9d1d7d9a7eef04bc3dbf84edbad2188230a|" "${VAL_STAGE}/gpustack-runner/pack/cann/Dockerfile.vllm"
rm -f "${VAL_STAGE}/gpustack-runner/pack/cann/Dockerfile.vllm.bak"

# 5j. ModelExpress builder check when MODELEXPRESS_COMMIT changes.
make_gitless_cand
set_kv_proposal "recipe_sync" "modelexpress=1111111111111111111111111111111111111111"
sed -i.bak 's|^ARG MODELEXPRESS_COMMIT=.*|ARG MODELEXPRESS_COMMIT=1111111111111111111111111111111111111111|' \
  "${CAND_DIR}/pack/mirrored-modelexpress-server/Dockerfile"
rm -f "${CAND_DIR}/pack/mirrored-modelexpress-server/Dockerfile.bak"
MX_MARKER="${SCRATCH}/mx-built"
if AUTOSYNC_DOCKER_BUILD_CMD="touch '${MX_MARKER}'" \
   AUTOSYNC_STAGE_DIR="${VAL_STAGE}" \
   LOOPWRIGHT_CANDIDATE_DIR="${CAND_DIR}" \
   AUTOSYNC_FAST_SELFTEST=true \
   bash "${CAND_DIR}/.auto-sync/checks/validate_kv_transfer.sh" && [[ -f "${MX_MARKER}" ]]; then
  pass "validate_kv_transfer triggers ModelExpress builder command when MODELEXPRESS_COMMIT changes"
else
  fail "validate_kv_transfer triggers ModelExpress builder command when MODELEXPRESS_COMMIT changes"
fi

if AUTOSYNC_DOCKER_BUILD_CMD="false" \
   AUTOSYNC_STAGE_DIR="${VAL_STAGE}" \
   LOOPWRIGHT_CANDIDATE_DIR="${CAND_DIR}" \
   AUTOSYNC_FAST_SELFTEST=true \
   bash "${CAND_DIR}/.auto-sync/checks/validate_kv_transfer.sh" >/dev/null 2>&1; then
  fail "validate_kv_transfer fails closed when ModelExpress builder fails"
else
  pass "validate_kv_transfer fails closed when ModelExpress builder fails"
fi

# 5k. NIXL_VERSION PyPI wheel verification check (verified 1.5.3 passes; unverified 1.6.0 fails closed).
make_gitless_cand
set_kv_proposal "recipe_sync" "nixl-pypi=1.5.3"
sed -i.bak 's|^ARG NIXL_VERSION="1.5.2"|ARG NIXL_VERSION="1.5.3"|' "${CAND_DIR}/pack/model-transfer/Dockerfile"
rm -f "${CAND_DIR}/pack/model-transfer/Dockerfile.bak"
assert_ok "validate_kv_transfer passes wheel-verified NIXL_VERSION=1.5.3" run_validate

make_gitless_cand
set_kv_proposal "recipe_sync" "nixl-pypi=1.6.0"
sed -i.bak 's|^ARG NIXL_VERSION="1.5.2"|ARG NIXL_VERSION="1.6.0"|' "${CAND_DIR}/pack/model-transfer/Dockerfile"
rm -f "${CAND_DIR}/pack/model-transfer/Dockerfile.bak"
assert_fails "validate_kv_transfer fails closed on unverified NIXL_VERSION=1.6.0" run_validate

# 5l. NIXL_ROCM_VERSION <-> pyproject.toml [project].version alignment check.
NIXL_ROCM_MOCK="${VAL_STAGE}/nixl-rocm"
init_mock_repo "${NIXL_ROCM_MOCK}"
cat > "${NIXL_ROCM_MOCK}/pyproject.toml" <<'TOML'
[project]
name = "nixl"
version = "1.5.1"
TOML
git -C "${NIXL_ROCM_MOCK}" add pyproject.toml
git -C "${NIXL_ROCM_MOCK}" commit -q -m "nixl rocm 1.5.1"
NIXL_ROCM_MOCK_SHA="$(git -C "${NIXL_ROCM_MOCK}" rev-parse HEAD)"

make_gitless_cand
set_kv_proposal "recipe_sync" "nixl-rocm=${NIXL_ROCM_MOCK_SHA}"
sed -i.bak "s|^ARG NIXL_ROCM_REF=\"[^\"]*\"|ARG NIXL_ROCM_REF=\"${NIXL_ROCM_MOCK_SHA}\"|" "${CAND_DIR}/pack/model-transfer/Dockerfile"
sed -i.bak 's|^ARG NIXL_ROCM_VERSION="[^"]*"|ARG NIXL_ROCM_VERSION="1.5.1"|' "${CAND_DIR}/pack/model-transfer/Dockerfile"
rm -f "${CAND_DIR}/pack/model-transfer/Dockerfile.bak"
assert_ok "validate_kv_transfer passes when NIXL_ROCM_VERSION matches pyproject.toml at NIXL_ROCM_REF" run_validate

sed -i.bak 's|^ARG NIXL_ROCM_VERSION="1.5.1"|ARG NIXL_ROCM_VERSION="9.9.9"|' "${CAND_DIR}/pack/model-transfer/Dockerfile"
rm -f "${CAND_DIR}/pack/model-transfer/Dockerfile.bak"
assert_fails "validate_kv_transfer fails closed when NIXL_ROCM_VERSION disagrees with pyproject.toml" run_validate
rm -rf "${NIXL_ROCM_MOCK}"

# 5m. Mismatched proposals/kv-transfer.json fails closed.
make_gitless_cand
set_kv_proposal "recipe_sync" "mooncake=0.99.0"
assert_fails "validate_kv_transfer fails closed on mismatched proposals/kv-transfer.json" run_validate

# 5n. Invalid gpustack-runner value in proposals/kv-transfer.json fails closed.
make_gitless_cand
set_kv_proposal "aligned" "gpustack-runner=alinged"
assert_fails "validate_kv_transfer fails closed on typo gpustack-runner=alinged" run_validate

# 5o. ROCm amd64-only image marked published=false fails closed.
make_gitless_cand
python3 - "${VAL_STAGE}/image-manifests.json" <<'PY'
import json
import sys
p = sys.argv[1]
with open(p, "r", encoding="utf-8") as f:
    doc = json.load(f)
doc["images"]["gpustack/model-transfer:v0.1.0-rocm7.2"]["published"] = False
with open(p, "w", encoding="utf-8") as f:
    json.dump(doc, f, indent=2)
PY
assert_fails "validate_kv_transfer fails closed when amd64-only ROCm image has published=false" run_validate

# 5p. Malformed unmatched quote in ARG MOONCAKE_VERSION fails closed (R2-S7).
make_gitless_cand
sed -i.bak 's|^ARG MOONCAKE_VERSION="0.3.13.post1"|ARG MOONCAKE_VERSION="0.3.14|' "${CAND_DIR}/pack/mirrored-mooncake/Dockerfile"
rm -f "${CAND_DIR}/pack/mirrored-mooncake/Dockerfile.bak"
bad_quote_err=""
if ! bad_quote_err="$(run_validate 2>&1)" && \
   grep -q "malformed ARG MOONCAKE_VERSION" <<<"${bad_quote_err}"; then
  pass "validate_kv_transfer fails closed on unmatched quote in ARG MOONCAKE_VERSION"
else
  fail "validate_kv_transfer fails closed on unmatched quote in ARG MOONCAKE_VERSION (${bad_quote_err})"
fi

# 6. Unit tests for canonical .auto-sync/checks/dockerfile_arg.py parser and CLI.
if python3 - "${ROOT_DIR}/.auto-sync/checks" "${SCRATCH}" <<'PY'
import os
import subprocess
import sys

checks_dir, scratch = sys.argv[1:3]
sys.path.insert(0, checks_dir)
from dockerfile_arg import read_dockerfile_arg

valid_cases = [
    ("ARG FOO=0.3.14\n", "0.3.14"),
    ('ARG FOO="0.3.14"\n', "0.3.14"),
    ("ARG FOO='0.3.14'\n", "0.3.14"),
    ("ARG FOO=0.3.14   # trailing comment\n", "0.3.14"),
    ('ARG FOO="a\\"b" # comment\n', 'a"b'),
    ("  ARG   FOO=0.3.14\n", "0.3.14"),
]
for txt, expected in valid_cases:
    got = read_dockerfile_arg(txt, "FOO", "unit-test")
    assert got == expected, f"for {txt!r}: got {got!r}, expected {expected!r}"

invalid_cases = [
    ('ARG FOO="0.3.14\n', "malformed ARG FOO"),
    ("ARG FOO='0.3.14\n", "malformed ARG FOO"),
    ("ARG FOO=0.3.14#bad\n", "unquoted '#'"),
    ("ARG FOO=0.3.14 \\\n", "multi-line line continuation"),
    ("ARG FOO=0.3.14 \\   \n", "multi-line line continuation"),
    ("ARG FOO=0.3.14 \\ # comment\n", "multi-line line continuation"),
    ("ARG FOO=0.3.14 BAR=1\n", "malformed ARG FOO"),
    ("ARG FOO=\n", "malformed ARG FOO"),
    ('ARG FOO=""\n', "malformed ARG FOO"),
    ("ARG FOO\n", "missing ARG FOO"),
]
for txt, err_substr in invalid_cases:
    try:
        got = read_dockerfile_arg(txt, "FOO", "unit-test")
    except SystemExit as exc:
        assert err_substr in str(exc), f"for {txt!r}: unexpected error {exc!r} (expected {err_substr!r})"
    else:
        raise AssertionError(f"for {txt!r}: expected failure, got {got!r}")

cli_df = os.path.join(scratch, "cli-test.Dockerfile")
with open(cli_df, "w", encoding="utf-8") as f:
    f.write('ARG FOO="a\\"b" # comment\n')

script = os.path.join(checks_dir, "dockerfile_arg.py")
res = subprocess.run([sys.executable, script, cli_df, "FOO", "cli-df"], capture_output=True, text=True)
assert res.returncode == 0 and res.stdout.strip() == 'a"b', (res.returncode, res.stdout, res.stderr)

opt_res = subprocess.run([sys.executable, script, "--optional", cli_df, "MISSING", "cli-df"], capture_output=True, text=True)
assert opt_res.returncode == 0 and opt_res.stdout == "", (opt_res.returncode, opt_res.stdout, opt_res.stderr)

missing_res = subprocess.run([sys.executable, script, "/nonexistent/Dockerfile", "FOO"], capture_output=True, text=True)
assert missing_res.returncode != 0 and "FAIL: cannot read" in missing_res.stderr and "Traceback" not in missing_res.stderr, (
    missing_res.returncode,
    missing_res.stdout,
    missing_res.stderr,
)
PY
then
  pass "dockerfile_arg.py parses valid/quoted/escaped ARGs and fails closed on malformed inputs"
else
  fail "dockerfile_arg.py parses valid/quoted/escaped ARGs and fails closed on malformed inputs"
fi

echo
if (( FAILS > 0 )); then
  echo "KV-TRANSFER SELFTEST FAILED: ${FAILS} failure(s), ${PASSES} passed" >&2
  exit 1
fi
echo "KV-TRANSFER SELFTEST PASSED: ${PASSES} check(s) passed"
