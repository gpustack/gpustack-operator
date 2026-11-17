#!/usr/bin/env bash
# .auto-sync/checks/selftest_router.sh - self-tests for router-sync domain.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
DEFAULT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd -P)"
ROOT_DIR="${1:-${DEFAULT_ROOT}}"
ROOT_DIR="$(cd "${ROOT_DIR}" && pwd -P)"

PREFLIGHT_SCRIPT="${ROOT_DIR}/.auto-sync/scripts/preflight-router.sh"
VALIDATE_SCRIPT="${ROOT_DIR}/.auto-sync/checks/validate_router.sh"

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

echo "== router-sync domain self-tests =="

for req in "${PREFLIGHT_SCRIPT}" "${VALIDATE_SCRIPT}"; do
  if [[ ! -x "${req}" ]]; then
    fail "missing or non-executable script: ${req#"${ROOT_DIR}/"}"
    echo "ROUTER SELFTEST FAILED: ${FAILS} failure(s), ${PASSES} passed" >&2
    exit 1
  fi
done

# Build an isolated fake repo checkout for preflight tests.
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
  "pack/llm-router/Dockerfile" \
  "pack/llm-router/patches/llm-d/0001-observer-endpoints-and-dispatch.patch" \
  "pack/llm-router/patches/vllm-router/0001-epd-routing.patch" \
  "pack/llm-router/patches/vllm-router/0002-mooncake-lazy-bootstrap-lookup.patch" \
  "pack/llm-router/patches/vllm-router/0003-observer-read-only-endpoint-view.patch" \
  "pack/llm-router/patches/vllm-router/0004-discovery-readiness-and-deselect-removal.patch" \
  "pack/llm-router/patches/sglang-gateway/0001-discovery-readiness-and-deselect-removal.patch" \
  "pack/llm-router/patches/sglang-gateway/0002-observer-read-only-endpoint-view.patch" \
  "pack/llm-router/patches/sglang-gateway/0003-attn-dp-aware-worker-registration.patch"; do
  copy_repo_file "${rel}"
done

mkdir -p "${FAKE_REPO}/.auto-sync/scripts" "${FAKE_REPO}/.auto-sync/checks" "${FAKE_REPO}/.auto-sync/proposals"
cp "${ROOT_DIR}/.auto-sync/scripts/lib-preflight.sh" "${FAKE_REPO}/.auto-sync/scripts/lib-preflight.sh"
cp "${PREFLIGHT_SCRIPT}" "${FAKE_REPO}/.auto-sync/scripts/preflight-router.sh"
cp "${ROOT_DIR}/.auto-sync/checks/lib.sh" "${FAKE_REPO}/.auto-sync/checks/lib.sh"
cp "${ROOT_DIR}/.auto-sync/checks/dockerfile_arg.py" "${FAKE_REPO}/.auto-sync/checks/dockerfile_arg.py"
cp "${VALIDATE_SCRIPT}" "${FAKE_REPO}/.auto-sync/checks/validate_router.sh"
if [[ -f "${ROOT_DIR}/.auto-sync/proposals/router.json" ]]; then
  cp "${ROOT_DIR}/.auto-sync/proposals/router.json" "${FAKE_REPO}/.auto-sync/proposals/router.json"
fi

git -C "${FAKE_REPO}" add -A
git -C "${FAKE_REPO}" commit -q -m "baseline"

init_mock_repo() {
  local dir="$1"
  mkdir -p "${dir}"
  git -C "${dir}" init -q -b main
  git -C "${dir}" config user.name "upstream"
  git -C "${dir}" config user.email "upstream@example.com"
  git -C "${dir}" config commit.gpgsign false
}

# Build offline mock upstream git repos for preflight tests.
FIXTURE_DIR="${SCRATCH}/fixtures"
mkdir -p "${FIXTURE_DIR}/repos"

# 1a. Mock llm-d-router repo (aligned at v0.11.0).
LLMD_SRC="${FIXTURE_DIR}/repos/llm-d-router"
init_mock_repo "${LLMD_SRC}"
mkdir -p "${LLMD_SRC}/cmd/epp"
printf 'package main\nfunc main() {}\n' > "${LLMD_SRC}/cmd/epp/main.go"
git -C "${LLMD_SRC}" add -A
git -C "${LLMD_SRC}" commit -q -m "llmd v0.11.0"
git -C "${LLMD_SRC}" tag v0.11.0

# 1b. Mock sglang repo with gateway-v0.3.1, gateway-v0.3.2, prerelease gateway-v0.3.3-rc1, and engine tag v0.4.9!
SGLANG_SRC="${FIXTURE_DIR}/repos/sglang"
init_mock_repo "${SGLANG_SRC}"
mkdir -p "${SGLANG_SRC}/sgl-model-gateway/src"
printf 'pub fn base() {}\n' > "${SGLANG_SRC}/sgl-model-gateway/src/lib.rs"
git -C "${SGLANG_SRC}" add -A
git -C "${SGLANG_SRC}" commit -q -m "gateway v0.3.1"
git -C "${SGLANG_SRC}" tag gateway-v0.3.1

printf 'pub fn v032() {}\n' >> "${SGLANG_SRC}/sgl-model-gateway/src/lib.rs"
git -C "${SGLANG_SRC}" add -A
git -C "${SGLANG_SRC}" commit -q -m "gateway v0.3.2"
git -C "${SGLANG_SRC}" tag gateway-v0.3.2

printf 'pub fn rc() {}\n' >> "${SGLANG_SRC}/sgl-model-gateway/src/lib.rs"
git -C "${SGLANG_SRC}" add -A
git -C "${SGLANG_SRC}" commit -q -m "gateway rc"
git -C "${SGLANG_SRC}" tag gateway-v0.3.3-rc1

printf 'pub fn engine() {}\n' >> "${SGLANG_SRC}/sgl-model-gateway/src/lib.rs"
git -C "${SGLANG_SRC}" add -A
git -C "${SGLANG_SRC}" commit -q -m "engine v0.4.9"
git -C "${SGLANG_SRC}" tag v0.4.9

# 1c. Mock vllm-router repo: initially at v0.1.16.1 and v0.1.16.2 (BEFORE 516f1cb).
VLLM_SRC="${FIXTURE_DIR}/repos/vllm-router"
init_mock_repo "${VLLM_SRC}"
mkdir -p "${VLLM_SRC}/src"
printf 'pub fn router_v16() {}\n' > "${VLLM_SRC}/src/lib.rs"
git -C "${VLLM_SRC}" add -A
git -C "${VLLM_SRC}" commit -q -m "v0.1.16.1"
git -C "${VLLM_SRC}" tag v0.1.16.1

printf 'pub fn patch_fix() {}\n' >> "${VLLM_SRC}/src/lib.rs"
git -C "${VLLM_SRC}" add -A
git -C "${VLLM_SRC}" commit -q -m "v0.1.16.2"
git -C "${VLLM_SRC}" tag v0.1.16.2

cat > "${FIXTURE_DIR}/image-manifests.json" <<'JSON'
{
  "images": {
    "docker.io/gpustack/llm-router:v0.4.0": {
      "published": true,
      "amd64Digest": "sha256:1111111111111111111111111111111111111111111111111111111111111111",
      "arm64Digest": "sha256:2222222222222222222222222222222222222222222222222222222222222222"
    },
    "docker.io/gpustack/llm-router:v0.5.0": {
      "published": false,
      "amd64Digest": "",
      "arm64Digest": ""
    },
    "docker.io/gpustack/mirrored-envoy:distroless-v1.33.2": {
      "published": true,
      "amd64Digest": "sha256:3333333333333333333333333333333333333333333333333333333333333333",
      "arm64Digest": "sha256:4444444444444444444444444444444444444444444444444444444444444444"
    },
    "docker.io/gpustack/mirrored-llm-d-router-disagg-sidecar:v0.11.0": {
      "published": true,
      "amd64Digest": "sha256:5555555555555555555555555555555555555555555555555555555555555555",
      "arm64Digest": "sha256:6666666666666666666666666666666666666666666666666666666666666666"
    },
    "docker.io/gpustack/mirrored-llm-d-router-disagg-sidecar:v0.12.0": {
      "published": false,
      "amd64Digest": "",
      "arm64Digest": ""
    }
  }
}
JSON

STAGE_DIR="${SCRATCH}/stage-router"
GHA_OUT="${SCRATCH}/gha-output.txt"
: > "${GHA_OUT}"

# Test 1: preflight-router.sh selects gateway-v0.3.2 over v0.4.9 / gateway-v0.3.3-rc1,
# and sets retire_0001_epd_routing=false when 516f1cb is not an ancestor of v0.1.16.2.
(
  cd "${FAKE_REPO}"
  AUTOSYNC_ROUTER_FIXTURE_DIR="${FIXTURE_DIR}" \
  AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
  GITHUB_OUTPUT="${GHA_OUT}" \
    bash "${FAKE_REPO}/.auto-sync/scripts/preflight-router.sh" "${STAGE_DIR}"
)

if python3 - "${STAGE_DIR}" "${GHA_OUT}" <<'PY'
import json
import os
import sys

stage, gha_out = sys.argv[1:3]
with open(os.path.join(stage, "upstream-records.json"), "r", encoding="utf-8") as f:
    recs = {r["upstreamId"]: r for r in json.load(f)}
assert recs["llm-d-router"]["status"] == "up_to_date" and recs["llm-d-router"]["version"] == "v0.11.0", recs["llm-d-router"]
assert recs["vllm-router"]["status"] == "available" and recs["vllm-router"]["version"] == "v0.1.16.2", recs["vllm-router"]
assert recs["sglang-gateway"]["status"] == "available" and recs["sglang-gateway"]["version"] == "gateway-v0.3.2", recs["sglang-gateway"]

with open(os.path.join(stage, "router-preflight-report.json"), "r", encoding="utf-8") as f:
    rep = json.load(f)
assert rep["stage"] == "recipe_sync", rep
assert rep["image_published"] is False, rep
assert rep["retire_0001_epd_routing"] is False, rep
assert rep["target_tags"]["sglang-gateway"] == "gateway-v0.3.2", rep

with open(gha_out, "r", encoding="utf-8") as f:
    out_txt = f.read()
for key in ("stage_dir<<", "upstream_records<<", "source_revision<<"):
    assert key in out_txt, f"missing GITHUB_OUTPUT key {key}"
PY
then
  pass "preflight selects gateway-v0.3.2 over v0.4.9 and sets retire_0001_epd_routing=false before 516f1cb"
else
  fail "preflight selects gateway-v0.3.2 over v0.4.9 and sets retire_0001_epd_routing=false before 516f1cb"
fi

# Test 2: add commit 516f1cb and tag v0.1.17 -> retire_0001_epd_routing becomes true.
printf 'pub fn epd() {}\n' >> "${VLLM_SRC}/src/lib.rs"
git -C "${VLLM_SRC}" add -A
git -C "${VLLM_SRC}" commit -q -m "epd routing 516f1cb"
git -C "${VLLM_SRC}" tag 516f1cb
printf 'pub fn v017() {}\n' >> "${VLLM_SRC}/src/lib.rs"
git -C "${VLLM_SRC}" add -A
git -C "${VLLM_SRC}" commit -q -m "v0.1.17"
git -C "${VLLM_SRC}" tag v0.1.17

(
  cd "${FAKE_REPO}"
  AUTOSYNC_ROUTER_FIXTURE_DIR="${FIXTURE_DIR}" \
  AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
    bash "${FAKE_REPO}/.auto-sync/scripts/preflight-router.sh" "${STAGE_DIR}"
)

if python3 - "${STAGE_DIR}" <<'PY'
import json
import os
import sys

stage = sys.argv[1]
with open(os.path.join(stage, "upstream-records.json"), "r", encoding="utf-8") as f:
    recs = {r["upstreamId"]: r for r in json.load(f)}
assert recs["vllm-router"]["status"] == "available" and recs["vllm-router"]["version"] == "v0.1.17", recs["vllm-router"]
with open(os.path.join(stage, "router-preflight-report.json"), "r", encoding="utf-8") as f:
    rep = json.load(f)
assert rep["retire_0001_epd_routing"] is True, rep
PY
then
  pass "preflight sets retire_0001_epd_routing=true when 516f1cb is an ancestor of v0.1.17"
else
  fail "preflight sets retire_0001_epd_routing=true when 516f1cb is an ancestor of v0.1.17"
fi

# Test 3: Stage B (+stage-b) vs no_change when recipes aligned and router/sidecar image published.
python3 - "${FAKE_REPO}/pack/llm-router/Dockerfile" <<'PY'
import sys
p = sys.argv[1]
with open(p, "r", encoding="utf-8") as f:
    txt = f.read()
txt = txt.replace("ARG VLLM_ROUTER_VERSION=v0.1.16.1", "ARG VLLM_ROUTER_VERSION=v0.1.17", 1)
txt = txt.replace("ARG SGLANG_GATEWAY_VERSION=gateway-v0.3.1", "ARG SGLANG_GATEWAY_VERSION=gateway-v0.3.2", 1)
with open(p, "w", encoding="utf-8") as f:
    f.write(txt)
PY
git -C "${FAKE_REPO}" add -A
git -C "${FAKE_REPO}" commit -q -m "stage a aligned"

# 3a. Target gpustack/llm-router:v0.5.0 is still unpublished -> no_change.
(
  cd "${FAKE_REPO}"
  AUTOSYNC_ROUTER_FIXTURE_DIR="${FIXTURE_DIR}" \
  AUTOSYNC_ROUTER_TARGET_TAG="v0.5.0" \
  AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
    bash "${FAKE_REPO}/.auto-sync/scripts/preflight-router.sh" "${STAGE_DIR}"
)

if python3 - "${STAGE_DIR}" <<'PY'
import json
import os
import sys

stage = sys.argv[1]
with open(os.path.join(stage, "upstream-records.json"), "r", encoding="utf-8") as f:
    recs = json.load(f)
assert all(r["status"] == "up_to_date" for r in recs), recs
with open(os.path.join(stage, "router-preflight-report.json"), "r", encoding="utf-8") as f:
    rep = json.load(f)
assert rep["stage"] == "no_change", rep
PY
then
  pass "preflight emits no_change while target gpustack/llm-router:v0.5.0 is unpublished"
else
  fail "preflight emits no_change while target gpustack/llm-router:v0.5.0 is unpublished"
fi

# 3b. Publish gpustack/llm-router:v0.5.0 multi-arch -> Stage B tag_promotion (+stage-b).
python3 - "${FIXTURE_DIR}/image-manifests.json" <<'PY'
import json
import sys
p = sys.argv[1]
with open(p, "r", encoding="utf-8") as f:
    doc = json.load(f)
doc["images"]["docker.io/gpustack/llm-router:v0.5.0"] = {
    "published": True,
    "amd64Digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "arm64Digest": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
}
with open(p, "w", encoding="utf-8") as f:
    json.dump(doc, f, indent=2)
PY

(
  cd "${FAKE_REPO}"
  AUTOSYNC_ROUTER_FIXTURE_DIR="${FIXTURE_DIR}" \
  AUTOSYNC_ROUTER_TARGET_TAG="v0.5.0" \
  AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
    bash "${FAKE_REPO}/.auto-sync/scripts/preflight-router.sh" "${STAGE_DIR}"
)

if python3 - "${STAGE_DIR}" <<'PY'
import json
import os
import sys

stage = sys.argv[1]
with open(os.path.join(stage, "upstream-records.json"), "r", encoding="utf-8") as f:
    recs = {r["upstreamId"]: r for r in json.load(f)}
assert any(r["status"] == "available" and r["version"].endswith("+stage-b") for r in recs.values()), recs
with open(os.path.join(stage, "router-preflight-report.json"), "r", encoding="utf-8") as f:
    rep = json.load(f)
assert rep["stage"] == "tag_promotion", rep
assert rep["image_published"] is True, rep
PY
then
  pass "preflight Stage B emits +stage-b and tag_promotion when multi-arch router image is published"
else
  fail "preflight Stage B emits +stage-b and tag_promotion when multi-arch router image is published"
fi

# 3c. Offline repo_tags auto-discovery triggers Stage B tag_promotion without AUTOSYNC_*_TARGET_TAG.
cat > "${FIXTURE_DIR}/upstream.json" <<'JSON'
{
  "repo_tags": {
    "gpustack/llm-router": ["v0.4.0", "v0.5.0-rc1", "v0.5.0"],
    "gpustack/mirrored-envoy": ["distroless-v1.33.2"]
  }
}
JSON

(
  cd "${FAKE_REPO}"
  AUTOSYNC_ROUTER_FIXTURE_DIR="${FIXTURE_DIR}" \
  AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
    bash "${FAKE_REPO}/.auto-sync/scripts/preflight-router.sh" "${STAGE_DIR}"
)

if python3 - "${STAGE_DIR}" <<'PY'
import json
import os
import sys

stage = sys.argv[1]
with open(os.path.join(stage, "upstream-records.json"), "r", encoding="utf-8") as f:
    recs = {r["upstreamId"]: r for r in json.load(f)}
assert any(r["status"] == "available" and r["version"].endswith("+stage-b") for r in recs.values()), recs
with open(os.path.join(stage, "router-preflight-report.json"), "r", encoding="utf-8") as f:
    rep = json.load(f)
assert rep["stage"] == "tag_promotion", rep
assert rep["image_published"] is True, rep
assert rep["targets"]["router_image"] == "gpustack/llm-router:v0.5.0", rep
PY
then
  pass "preflight Stage B auto-discovers gpustack/llm-router:v0.5.0 via repo_tags without AUTOSYNC_*_TARGET_TAG"
else
  fail "preflight Stage B auto-discovers gpustack/llm-router:v0.5.0 via repo_tags without AUTOSYNC_*_TARGET_TAG"
fi
rm -f "${FIXTURE_DIR}/upstream.json"

# ----------------------------------------------------------------------------
# Test 4: Gitless env -i validate_router.sh tests with mock upstream repos and patches
# ----------------------------------------------------------------------------
VAL_STAGE="${SCRATCH}/val-stage"
mkdir -p "${VAL_STAGE}/baseline/pkg/worker/settings" \
  "${VAL_STAGE}/baseline/docs/reference" \
  "${VAL_STAGE}/baseline/pack/llm-router/patches/llm-d" \
  "${VAL_STAGE}/baseline/pack/llm-router/patches/vllm-router" \
  "${VAL_STAGE}/baseline/pack/llm-router/patches/sglang-gateway" \
  "${VAL_STAGE}/go-cache" "${VAL_STAGE}/go-mod" "${VAL_STAGE}/deps-cache"

cp "${ROOT_DIR}/pkg/worker/settings/value.go" "${VAL_STAGE}/baseline/pkg/worker/settings/value.go"
cp "${ROOT_DIR}/docs/reference/settings.md" "${VAL_STAGE}/baseline/docs/reference/settings.md"

cat > "${VAL_STAGE}/baseline/pack/llm-router/Dockerfile" <<'DF'
ARG UBUNTU_VERSION=24.04
ARG GO_VERSION=1.26-bookworm
ARG RUST_VERSION=1.95-bookworm

ARG LLMD_ROUTER_VERSION=v0.11.0
ARG VLLM_ROUTER_VERSION=v0.1.16.1
ARG SGLANG_GATEWAY_VERSION=gateway-v0.3.1

FROM golang:${GO_VERSION} AS epp-builder
ARG LLMD_ROUTER_VERSION
COPY pack/llm-router/patches/llm-d/ /tmp/patches/

FROM rust:${RUST_VERSION} AS rust-builder
RUN echo rust

FROM rust-builder AS vllm-router-builder
ARG VLLM_ROUTER_VERSION
COPY pack/llm-router/patches/vllm-router/ /tmp/patches/

FROM rust-builder AS sglang-gateway-builder
ARG SGLANG_GATEWAY_VERSION
COPY pack/llm-router/patches/sglang-gateway/ /tmp/patches/

FROM ubuntu:${UBUNTU_VERSION}
COPY --from=epp-builder /out/epp /usr/local/bin/epp
DF

# Build clean mock upstream repos in VAL_STAGE matching mock patches.
# 4a. Mock llm-d-router
VAL_LLMD="${VAL_STAGE}/llm-d-router"
init_mock_repo "${VAL_LLMD}"
mkdir -p "${VAL_LLMD}/pkg"
printf 'package pkg\nfunc Route() int { return 1 }\n' > "${VAL_LLMD}/pkg/route.go"
git -C "${VAL_LLMD}" add -A
git -C "${VAL_LLMD}" commit -q -m "v0.11.0"
git -C "${VAL_LLMD}" tag v0.11.0

# Create patch for llm-d
printf 'package pkg\nfunc Route() int { return 2 }\n' > "${VAL_LLMD}/pkg/route.go"
git -C "${VAL_LLMD}" diff > "${VAL_STAGE}/baseline/pack/llm-router/patches/llm-d/0001-observer-endpoints-and-dispatch.patch"
git -C "${VAL_LLMD}" checkout -q -- pkg/route.go

# 4b. Mock vllm-router with v0.1.16.1 (pre-516f1cb) and v0.1.17 (with 516f1cb* commit!)
VAL_VLLM="${VAL_STAGE}/vllm-router"
init_mock_repo "${VAL_VLLM}"
mkdir -p "${VAL_VLLM}/src"
printf 'pub fn base() -> i32 { 1 }\n' > "${VAL_VLLM}/src/router.rs"
git -C "${VAL_VLLM}" add -A
git -C "${VAL_VLLM}" commit -q -m "v0.1.16.1"
git -C "${VAL_VLLM}" tag v0.1.16.1

# Patch 0001 creates src/epd.rs on v0.1.16.1
printf 'pub fn epd() -> bool { true }\n' > "${VAL_VLLM}/src/epd.rs"
git -C "${VAL_VLLM}" add src/epd.rs
git -C "${VAL_VLLM}" diff --cached > "${VAL_STAGE}/baseline/pack/llm-router/patches/vllm-router/0001-epd-routing.patch"
git -C "${VAL_VLLM}" reset -q HEAD src/epd.rs
rm -f "${VAL_VLLM}/src/epd.rs"

# Patch 0002 modifies src/router.rs (works on both v0.1.16.1 and v0.1.17)
printf 'pub fn base() -> i32 { 2 }\n' > "${VAL_VLLM}/src/router.rs"
git -C "${VAL_VLLM}" diff > "${VAL_STAGE}/baseline/pack/llm-router/patches/vllm-router/0002-mooncake-lazy-bootstrap-lookup.patch"
git -C "${VAL_VLLM}" checkout -q -- src/router.rs

# Patch 0003 creates src/observer.rs
printf 'pub struct VllmObserver {}\n' > "${VAL_VLLM}/src/observer.rs"
git -C "${VAL_VLLM}" add src/observer.rs
git -C "${VAL_VLLM}" diff --cached > "${VAL_STAGE}/baseline/pack/llm-router/patches/vllm-router/0003-observer-read-only-endpoint-view.patch"
git -C "${VAL_VLLM}" reset -q HEAD src/observer.rs
rm -f "${VAL_VLLM}/src/observer.rs"

# Patch 0004 creates src/discovery.rs
printf 'pub fn deselect() {}\n' > "${VAL_VLLM}/src/discovery.rs"
git -C "${VAL_VLLM}" add src/discovery.rs
git -C "${VAL_VLLM}" diff --cached > "${VAL_STAGE}/baseline/pack/llm-router/patches/vllm-router/0004-discovery-readiness-and-deselect-removal.patch"
git -C "${VAL_VLLM}" reset -q HEAD src/discovery.rs
rm -f "${VAL_VLLM}/src/discovery.rs"

# Now upstream merges 516f1cb (creating src/epd.rs) and tags v0.1.17!
printf 'pub fn epd() -> bool { true }\n' > "${VAL_VLLM}/src/epd.rs"
git -C "${VAL_VLLM}" add src/epd.rs
git -C "${VAL_VLLM}" commit -q -m "upstream PR 266 EPD routing"
git -C "${VAL_VLLM}" tag 516f1cb
printf 'pub fn v017_extra() {}\n' > "${VAL_VLLM}/src/extra.rs"
git -C "${VAL_VLLM}" add src/extra.rs
git -C "${VAL_VLLM}" commit -q -m "v0.1.17"
git -C "${VAL_VLLM}" tag v0.1.17

# 4c. Mock sglang repo with gateway-v0.3.1
VAL_SGLANG="${VAL_STAGE}/sglang"
init_mock_repo "${VAL_SGLANG}"
mkdir -p "${VAL_SGLANG}/sgl-model-gateway/src"
printf 'pub fn gw() -> i32 { 1 }\n' > "${VAL_SGLANG}/sgl-model-gateway/src/lib.rs"
git -C "${VAL_SGLANG}" add -A
git -C "${VAL_SGLANG}" commit -q -m "gateway-v0.3.1"
git -C "${VAL_SGLANG}" tag gateway-v0.3.1

printf 'pub fn gw() -> i32 { 2 }\n' > "${VAL_SGLANG}/sgl-model-gateway/src/lib.rs"
git -C "${VAL_SGLANG}" diff > "${VAL_STAGE}/baseline/pack/llm-router/patches/sglang-gateway/0001-discovery-readiness-and-deselect-removal.patch"
git -C "${VAL_SGLANG}" checkout -q -- sgl-model-gateway/src/lib.rs

cat > "${VAL_SGLANG}/sgl-model-gateway/src/observer.rs" <<'RS'
pub struct ObserverRouter {
    pub boot_generation: u64,
}
RS
git -C "${VAL_SGLANG}" add sgl-model-gateway/src/observer.rs
git -C "${VAL_SGLANG}" diff --cached > "${VAL_STAGE}/baseline/pack/llm-router/patches/sglang-gateway/0002-observer-read-only-endpoint-view.patch"
git -C "${VAL_SGLANG}" reset -q HEAD sgl-model-gateway/src/observer.rs
rm -f "${VAL_SGLANG}/sgl-model-gateway/src/observer.rs"

printf 'pub fn dp() {}\n' > "${VAL_SGLANG}/sgl-model-gateway/src/dp.rs"
git -C "${VAL_SGLANG}" add sgl-model-gateway/src/dp.rs
git -C "${VAL_SGLANG}" diff --cached > "${VAL_STAGE}/baseline/pack/llm-router/patches/sglang-gateway/0003-attn-dp-aware-worker-registration.patch"
git -C "${VAL_SGLANG}" reset -q HEAD sgl-model-gateway/src/dp.rs
rm -f "${VAL_SGLANG}/sgl-model-gateway/src/dp.rs"

cat > "${VAL_STAGE}/image-manifests.json" <<'JSON'
{
  "schemaVersion": 1,
  "images": {
    "gpustack/llm-router:v0.4.0": {
      "image": "gpustack/llm-router:v0.4.0",
      "normalized": "docker.io/gpustack/llm-router:v0.4.0",
      "published": true,
      "amd64Digest": "sha256:1111111111111111111111111111111111111111111111111111111111111111",
      "arm64Digest": "sha256:2222222222222222222222222222222222222222222222222222222222222222"
    },
    "gpustack/llm-router:v0.5.0": {
      "image": "gpustack/llm-router:v0.5.0",
      "normalized": "docker.io/gpustack/llm-router:v0.5.0",
      "published": false,
      "amd64Digest": "",
      "arm64Digest": ""
    },
    "gpustack/mirrored-envoy:distroless-v1.33.2": {
      "image": "gpustack/mirrored-envoy:distroless-v1.33.2",
      "normalized": "docker.io/gpustack/mirrored-envoy:distroless-v1.33.2",
      "published": true,
      "amd64Digest": "sha256:3333333333333333333333333333333333333333333333333333333333333333",
      "arm64Digest": "sha256:4444444444444444444444444444444444444444444444444444444444444444"
    },
    "gpustack/mirrored-llm-d-router-disagg-sidecar:v0.11.0": {
      "image": "gpustack/mirrored-llm-d-router-disagg-sidecar:v0.11.0",
      "normalized": "docker.io/gpustack/mirrored-llm-d-router-disagg-sidecar:v0.11.0",
      "published": true,
      "amd64Digest": "sha256:5555555555555555555555555555555555555555555555555555555555555555",
      "arm64Digest": "sha256:6666666666666666666666666666666666666666666666666666666666666666"
    }
  }
}
JSON

write_preflight_report() {
  local retire_bool="$1"
  local vllm_target="$2"
  cat > "${VAL_STAGE}/router-preflight-report.json" <<JSON
{
  "schemaVersion": 1,
  "stage": "recipe_sync",
  "image_published": false,
  "retire_0001_epd_routing": ${retire_bool},
  "target_tags": {
    "llm-d-router": "v0.11.0",
    "vllm-router": "${vllm_target}",
    "sglang-gateway": "gateway-v0.3.1"
  }
}
JSON
}

CAND_DIR="${SCRATCH}/candidate"
make_gitless_cand() {
  rm -rf "${CAND_DIR}"
  mkdir -p "${CAND_DIR}/.auto-sync/checks" \
    "${CAND_DIR}/.auto-sync/proposals" \
    "${CAND_DIR}/pkg/worker/settings" \
    "${CAND_DIR}/docs/reference" \
    "${CAND_DIR}/pack/llm-router/patches"
  cp "${ROOT_DIR}/.auto-sync/checks/lib.sh" "${CAND_DIR}/.auto-sync/checks/lib.sh"
  cp "${VALIDATE_SCRIPT}" "${CAND_DIR}/.auto-sync/checks/validate_router.sh"
  cp "${ROOT_DIR}/.auto-sync/proposals/router.json" "${CAND_DIR}/.auto-sync/proposals/router.json"
  cp "${VAL_STAGE}/baseline/pkg/worker/settings/value.go" "${CAND_DIR}/pkg/worker/settings/value.go"
  cp "${VAL_STAGE}/baseline/docs/reference/settings.md" "${CAND_DIR}/docs/reference/settings.md"
  cp "${VAL_STAGE}/baseline/pack/llm-router/Dockerfile" "${CAND_DIR}/pack/llm-router/Dockerfile"
  cp -R "${VAL_STAGE}/baseline/pack/llm-router/patches/." "${CAND_DIR}/pack/llm-router/patches/"
}

set_router_proposal() {
  local stage="$1"
  local vllm_v="$2"
  local llmd_v="${3:-v0.11.0}"
  python3 - "${CAND_DIR}/.auto-sync/proposals/router.json" "${stage}" "${vllm_v}" "${llmd_v}" <<'PY'
import json
import sys

p, stage, vllm_v, llmd_v = sys.argv[1:5]
with open(p, "r", encoding="utf-8") as f:
    doc = json.load(f)
doc["stage"] = stage
doc["versions"]["vllm-router"] = vllm_v
doc["versions"]["llm-d-router"] = llmd_v
with open(p, "w", encoding="utf-8") as f:
    json.dump(doc, f, indent=2)
    f.write("\n")
PY
}

BUILD_LOG="${SCRATCH}/builder-calls.log"
STUB_BUILD_RUNNER="${SCRATCH}/stub-build.sh"
cat > "${STUB_BUILD_RUNNER}" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "${ROUTER_BUILD_LOG}"
SH
chmod +x "${STUB_BUILD_RUNNER}"

run_validate_env_i() {
  : > "${BUILD_LOG}"
  env -i \
    PATH="/usr/local/bin:/usr/bin:/bin" \
    HOME="${SCRATCH}" \
    TMPDIR="${SCRATCH}" \
    AUTOSYNC_STAGE_DIR="${VAL_STAGE}" \
    LOOPWRIGHT_CANDIDATE_DIR="${CAND_DIR}" \
    AUTOSYNC_ROUTER_BUILD_RUNNER="${STUB_BUILD_RUNNER}" \
    ROUTER_BUILD_LOG="${BUILD_LOG}" \
    AUTOSYNC_ALLOW_MISSING_STAGED_REPO="${AUTOSYNC_ALLOW_MISSING_STAGED_REPO:-false}" \
    AUTOSYNC_FAST_SELFTEST=true \
    bash "${CAND_DIR}/.auto-sync/checks/validate_router.sh"
}

# 4.1 Positive case: rebase onto v0.1.17 (with 516f1cb), retire 0001-epd-routing.patch,
# remaining patches apply cleanly, ObserverRouter present, and stubbed builder invoked ONLY for vllm-router-builder.
write_preflight_report "true" "v0.1.17"
make_gitless_cand
set_router_proposal "recipe_sync" "v0.1.17"
sed -i.bak 's/^ARG VLLM_ROUTER_VERSION=v0\.1\.16\.1/ARG VLLM_ROUTER_VERSION=v0.1.17/' "${CAND_DIR}/pack/llm-router/Dockerfile"
rm -f "${CAND_DIR}/pack/llm-router/Dockerfile.bak"
rm -f "${CAND_DIR}/pack/llm-router/patches/vllm-router/0001-epd-routing.patch"

if run_validate_env_i && \
   [[ "$(wc -l < "${BUILD_LOG}" | tr -d ' ')" == "1" ]] && \
   grep -q -- '--target vllm-router-builder' "${BUILD_LOG}" && \
   ! grep -q -- '--target epp-builder' "${BUILD_LOG}" && \
   ! grep -q -- '--target sglang-gateway-builder' "${BUILD_LOG}"; then
  pass "gitless env -i validate_router passes rebase onto v0.1.17 with 0001 retired and builds ONLY vllm-router-builder"
else
  fail "gitless env -i validate_router passes rebase onto v0.1.17 with 0001 retired and builds ONLY vllm-router-builder"
fi

# 4.2 Negative case 1: dropping struct ObserverRouter from SGLang observer patch -> FAIL.
write_preflight_report "true" "v0.1.17"
make_gitless_cand
sed -i.bak 's/^ARG VLLM_ROUTER_VERSION=v0\.1\.16\.1/ARG VLLM_ROUTER_VERSION=v0.1.17/' "${CAND_DIR}/pack/llm-router/Dockerfile"
rm -f "${CAND_DIR}/pack/llm-router/Dockerfile.bak"
rm -f "${CAND_DIR}/pack/llm-router/patches/vllm-router/0001-epd-routing.patch"
sed -i.bak 's/struct ObserverRouter/struct RenamedRouter/' \
  "${CAND_DIR}/pack/llm-router/patches/sglang-gateway/0002-observer-read-only-endpoint-view.patch"
rm -f "${CAND_DIR}/pack/llm-router/patches/sglang-gateway/0002-observer-read-only-endpoint-view.patch.bak"
assert_fails "validate_router fails closed when struct ObserverRouter is dropped from sglang-gateway observer patch" run_validate_env_i

# 4.3 Negative case 2: deleting 0001-epd-routing.patch when retire_0001_epd_routing == false -> FAIL.
write_preflight_report "false" "v0.1.16.1"
make_gitless_cand
rm -f "${CAND_DIR}/pack/llm-router/patches/vllm-router/0001-epd-routing.patch"
assert_fails "validate_router fails closed when 0001-epd-routing.patch is deleted with retire_0001_epd_routing=false" run_validate_env_i

# 4.4 Negative case 2b: keeping 0001-epd-routing.patch when retire_0001_epd_routing == true and bumped to v0.1.17 -> FAIL.
write_preflight_report "true" "v0.1.17"
make_gitless_cand
sed -i.bak 's/^ARG VLLM_ROUTER_VERSION=v0\.1\.16\.1/ARG VLLM_ROUTER_VERSION=v0.1.17/' "${CAND_DIR}/pack/llm-router/Dockerfile"
rm -f "${CAND_DIR}/pack/llm-router/Dockerfile.bak"
assert_fails "validate_router fails closed when 0001-epd-routing.patch is NOT retired after rebasing onto v0.1.17" run_validate_env_i

# 4.5 Negative case 3a: promoting unpublished gpustack/llm-router:v0.5.0 in value.go -> FAIL.
write_preflight_report "false" "v0.1.16.1"
make_gitless_cand
sed -i.bak 's|gpustack/llm-router:v0.4.0|gpustack/llm-router:v0.5.0|' \
  "${CAND_DIR}/pkg/worker/settings/value.go" "${CAND_DIR}/docs/reference/settings.md"
rm -f "${CAND_DIR}/pkg/worker/settings/value.go.bak" "${CAND_DIR}/docs/reference/settings.md.bak"
assert_fails "validate_router fails closed when unpublished gpustack/llm-router:v0.5.0 is promoted in value.go" run_validate_env_i

# 4.6 Negative case 3b: mutating KVCacheBackendImage in value.go -> FAIL.
make_gitless_cand
sed -i.bak 's|gpustack/mirrored-mooncake:0.3.13.post1-cpu|gpustack/mirrored-mooncake:0.3.14-cpu|' \
  "${CAND_DIR}/pkg/worker/settings/value.go"
rm -f "${CAND_DIR}/pkg/worker/settings/value.go.bak"
assert_fails "validate_router fails closed when KVCacheBackendImage is mutated in value.go" run_validate_env_i

# 4.7 Positive Stage B case: promoting published multi-arch gpustack/llm-router:v0.5.0 -> PASS.
python3 - "${VAL_STAGE}/image-manifests.json" <<'PY'
import json
import sys
p = sys.argv[1]
with open(p, "r", encoding="utf-8") as f:
    doc = json.load(f)
doc["images"]["gpustack/llm-router:v0.5.0"] = {
    "image": "gpustack/llm-router:v0.5.0",
    "normalized": "docker.io/gpustack/llm-router:v0.5.0",
    "published": True,
    "amd64Digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "arm64Digest": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
}
with open(p, "w", encoding="utf-8") as f:
    json.dump(doc, f, indent=2)
PY
make_gitless_cand
set_router_proposal "tag_promotion" "v0.1.16.1" "v0.11.0+stage-b"
sed -i.bak 's|gpustack/llm-router:v0.4.0|gpustack/llm-router:v0.5.0|' \
  "${CAND_DIR}/pkg/worker/settings/value.go" "${CAND_DIR}/docs/reference/settings.md"
rm -f "${CAND_DIR}/pkg/worker/settings/value.go.bak" "${CAND_DIR}/docs/reference/settings.md.bak"
if run_validate_env_i && [[ ! -s "${BUILD_LOG}" ]]; then
  pass "validate_router Stage B passes for published multi-arch gpustack/llm-router:v0.5.0 without rebuilding router stages"
else
  fail "validate_router Stage B passes for published multi-arch gpustack/llm-router:v0.5.0 without rebuilding router stages"
fi

# 4.8 Negative case 4: mismatched proposals/router.json fails closed.
make_gitless_cand
set_router_proposal "recipe_sync" "v0.99.0"
assert_fails "validate_router fails closed on mismatched proposals/router.json version" run_validate_env_i

# 4.9 Negative case 5: retire_0001_epd_routing=true fails even without bumping VLLM_ROUTER_VERSION.
write_preflight_report "true" "v0.1.17"
make_gitless_cand
assert_fails "validate_router fails closed when retire_0001_epd_routing=true and 0001-epd-routing.patch remains" run_validate_env_i

# 4.10 Negative case 6: missing staged repo fails closed unless AUTOSYNC_ALLOW_MISSING_STAGED_REPO=true.
write_preflight_report "false" "v0.1.16.1"
make_gitless_cand
mv "${VAL_STAGE}/vllm-router" "${SCRATCH}/vllm-router-backup"
assert_fails "validate_router fails closed when staged vllm-router clone is missing" run_validate_env_i
if AUTOSYNC_ALLOW_MISSING_STAGED_REPO=true run_validate_env_i >/dev/null 2>&1; then
  pass "validate_router allows missing staged repo only when AUTOSYNC_ALLOW_MISSING_STAGED_REPO=true"
else
  fail "validate_router allows missing staged repo only when AUTOSYNC_ALLOW_MISSING_STAGED_REPO=true"
fi
mv "${SCRATCH}/vllm-router-backup" "${VAL_STAGE}/vllm-router"

# 4.11 Negative case 7: missing fixture repo under AUTOSYNC_ROUTER_FIXTURE_DIR fails closed.
FIX_MISSING_REPO="${SCRATCH}/fix-missing-router"
cp -R "${FIXTURE_DIR}" "${FIX_MISSING_REPO}"
rm -rf "${FIX_MISSING_REPO}/repos/sglang"
run_router_preflight_with_fixture() {
  (
    cd "$1"
    AUTOSYNC_ROUTER_FIXTURE_DIR="$2" \
    AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
      bash "$1/.auto-sync/scripts/preflight-router.sh" "$3"
  )
}
assert_fails "preflight-router.sh fails closed when AUTOSYNC_ROUTER_FIXTURE_DIR lacks sglang repo" \
  run_router_preflight_with_fixture "${FAKE_REPO}" "${FIX_MISSING_REPO}" "${SCRATCH}/stage-missing-router"

echo
if (( FAILS > 0 )); then
  echo "ROUTER SELFTEST FAILED: ${FAILS} failure(s), ${PASSES} passed" >&2
  exit 1
fi
echo "ROUTER SELFTEST PASSED: ${PASSES} check(s) passed"
