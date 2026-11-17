#!/usr/bin/env bash
# .auto-sync/checks/selftest_kv_transfer.sh - self-tests for kv-transfer-sync domain.
set -euo pipefail

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
cp "${VALIDATE_SCRIPT}" "${FAKE_REPO}/.auto-sync/checks/validate_kv_transfer.sh"
if [[ -f "${ROOT_DIR}/.auto-sync/proposals/kv-transfer.json" ]]; then
  cp "${ROOT_DIR}/.auto-sync/proposals/kv-transfer.json" "${FAKE_REPO}/.auto-sync/proposals/kv-transfer.json"
fi

git -C "${FAKE_REPO}" add -A
git -C "${FAKE_REPO}" commit -q -m "baseline"

# Build offline fixtures for preflight-kv-transfer.sh.
FIXTURE_DIR="${SCRATCH}/fixtures"
mkdir -p "${FIXTURE_DIR}/pypi" "${FIXTURE_DIR}/repos"

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
: > "${GHA_OUT}"
(
  cd "${FAKE_REPO}"
  AUTOSYNC_KV_FIXTURE_DIR="${FIXTURE_DIR}" \
  AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
  GITHUB_OUTPUT="${GHA_OUT}" \
    bash "${FAKE_REPO}/.auto-sync/scripts/preflight-kv-transfer.sh" "${STAGE_DIR}"
)

if python3 - "${STAGE_DIR}" <<'PY'
import json
import os
import sys

stage = sys.argv[1]
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
PY
then
  pass "preflight Stage A emits recipe_sync and selects NIXL 1.5.3 with 3-package wheel parity"
else
  fail "preflight Stage A emits recipe_sync and selects NIXL 1.5.3 with 3-package wheel parity"
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

(
  cd "${FAKE_REPO}"
  AUTOSYNC_KV_FIXTURE_DIR="${FIXTURE_DIR}" \
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
  pass "preflight emits up_to_date (no_change) while Stage A target image is unpublished"
else
  fail "preflight emits up_to_date (no_change) while Stage A target image is unpublished"
fi

# 3. Preflight Stage B (tag_promotion): 0.3.14-cpu now published multi-arch, value.go still on 0.3.13.post1-cpu.
python3 - "${FIXTURE_DIR}/image-manifests.json" <<'PY'
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
  AUTOSYNC_KV_FIXTURE_DIR="${FIXTURE_DIR}" \
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
assert recs["mooncake"]["status"] == "available" and recs["mooncake"]["version"] == "0.3.14+stage-b", recs["mooncake"]
with open(os.path.join(stage, "kv-transfer-preflight-report.json"), "r", encoding="utf-8") as f:
    rep = json.load(f)
assert rep["stage"] == "tag_promotion", rep
assert rep["image_published"] is True, rep
PY
then
  pass "preflight Stage B emits synthetic 0.3.14+stage-b when multi-arch manifest is published"
else
  fail "preflight Stage B emits synthetic 0.3.14+stage-b when multi-arch manifest is published"
fi

# 4. Preflight fully aligned (no_change) after value.go + settings.md updated to 0.3.14-cpu.
sed -i.bak 's|gpustack/mirrored-mooncake:0.3.13.post1-cpu|gpustack/mirrored-mooncake:0.3.14-cpu|' "${FAKE_REPO}/pkg/worker/settings/value.go"
sed -i.bak 's|gpustack/mirrored-mooncake:0.3.13.post1-cpu|gpustack/mirrored-mooncake:0.3.14-cpu|' "${FAKE_REPO}/docs/reference/settings.md"
rm -f "${FAKE_REPO}/pkg/worker/settings/value.go.bak" "${FAKE_REPO}/docs/reference/settings.md.bak"
git -C "${FAKE_REPO}" add -A
git -C "${FAKE_REPO}" commit -q -m "stage b landed"

(
  cd "${FAKE_REPO}"
  AUTOSYNC_KV_FIXTURE_DIR="${FIXTURE_DIR}" \
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
      "published": false,
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
  "enforce_runner_hixl_parity": true
}
JSON

CAND_DIR="${SCRATCH}/candidate"
make_gitless_cand() {
  rm -rf "${CAND_DIR}"
  mkdir -p "${CAND_DIR}/.auto-sync/checks" \
    "${CAND_DIR}/pkg/worker/settings" \
    "${CAND_DIR}/docs/reference" \
    "${CAND_DIR}/deploy/gpustack-operator/chart" \
    "${CAND_DIR}/pack/mirrored-mooncake" \
    "${CAND_DIR}/pack/model-transfer/patches/hixl" \
    "${CAND_DIR}/pack/mirrored-modelexpress-server"
  cp "${ROOT_DIR}/.auto-sync/checks/lib.sh" "${CAND_DIR}/.auto-sync/checks/lib.sh"
  cp "${VALIDATE_SCRIPT}" "${CAND_DIR}/.auto-sync/checks/validate_kv_transfer.sh"
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

# Update the 4th stage guard too -> Stage A PASSES (value.go untouched, even for unpublished tag).
python3 - "${CAND_DIR}/pack/mirrored-mooncake/Dockerfile" <<'PY'
import sys
p = sys.argv[1]
with open(p, "r", encoding="utf-8") as f:
    txt = f.read()
txt = txt.replace("0.3.10*|0.3.11*|0.3.12*|0.3.13*)", "0.3.10*|0.3.11*|0.3.12*|0.3.13*|0.3.14*)")
with open(p, "w", encoding="utf-8") as f:
    f.write(txt)
PY
assert_ok "validate_kv_transfer Stage A passes when all 4 Mooncake stage guards include 0.3.14* and value.go untouched" run_validate

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
sed -i.bak 's|gpustack/mirrored-mooncake:0.3.13.post1-cpu|gpustack/mirrored-mooncake:0.3.14-cpu|' \
  "${CAND_DIR}/pkg/worker/settings/value.go" "${CAND_DIR}/docs/reference/settings.md"
rm -f "${CAND_DIR}/pkg/worker/settings/value.go.bak" "${CAND_DIR}/docs/reference/settings.md.bak"
assert_ok "validate_kv_transfer Stage B passes when value.go + settings.md use published multi-arch tag" run_validate

# 5f. Mutating ModelDeploymentRouterImage in value.go -> FAIL.
make_gitless_cand
sed -i.bak 's|gpustack/llm-router:v0.4.0|gpustack/llm-router:v0.5.0|' "${CAND_DIR}/pkg/worker/settings/value.go"
rm -f "${CAND_DIR}/pkg/worker/settings/value.go.bak"
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
sed -i.bak "s|60dbe9d1d7d9a7eef04bc3dbf84edbad2188230a|${HIXL_FIXTURE_SHA}|" "${CAND_DIR}/pack/model-transfer/Dockerfile"
rm -f "${CAND_DIR}/pack/model-transfer/Dockerfile.bak"
assert_ok "validate_kv_transfer passes HIXL patch --dry-run against staged hixl repo" run_validate

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

echo
if (( FAILS > 0 )); then
  echo "KV-TRANSFER SELFTEST FAILED: ${FAILS} failure(s), ${PASSES} passed" >&2
  exit 1
fi
echo "KV-TRANSFER SELFTEST PASSED: ${PASSES} check(s) passed"
