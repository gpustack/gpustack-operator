#!/usr/bin/env bash
#
# preflight.sh — detect the target environment (local or ssh).
#
#   XB_MODE=local                      bash .../preflight.sh
#   XB_MODE=ssh XB_HOST=root@host       bash .../preflight.sh
#
# Prints a STATUS | CHECK | DETAIL table. STATUS is PASS / WARN / FAIL.
#   - docker + buildx are required to BUILD (both CASE-1 builds). buildx missing => FAIL with
#     an install hint (it is a single plugin binary).
#   - npu-smi / ascend docker runtime / /dev/davinci* are required only for the
#     HARDWARE cases (2, 3); absent => WARN (build-only still works).
# Exits non-zero if any required (build) check FAILs.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
. "${HERE}/lib.sh"

echo "# preflight on $(xtarget_desc)"

out="$(xsh <<'PAYLOAD'
set -u
row() { printf '%s | %s | %s\n' "$1" "$2" "$3"; }
fails=0

# OS
if [ -r /etc/os-release ]; then
  os="$(. /etc/os-release; echo "${NAME} ${VERSION}")"
  row PASS "os-release" "${os} ($(uname -m), $(uname -r))"
else
  row WARN "os-release" "not found ($(uname -m))"
fi

# docker (required)
if command -v docker >/dev/null 2>&1; then
  row PASS "docker" "$(docker version --format '{{.Server.Version}}' 2>/dev/null || echo 'daemon?')"
else
  row FAIL "docker" "not found"; fails=$((fails+1))
fi

# buildx (required to build the multi-stage Dockerfile: heredoc + RUN --mount)
if docker buildx version >/dev/null 2>&1; then
  row PASS "buildx" "$(docker buildx version 2>/dev/null | head -1)"
else
  row FAIL "buildx" "missing — install: curl -sSL -o ~/.docker/cli-plugins/docker-buildx https://github.com/docker/buildx/releases/download/v0.19.3/buildx-v0.19.3.linux-\$(uname -m | sed s/x86_64/amd64/;s/aarch64/arm64/) && chmod +x ~/.docker/cli-plugins/docker-buildx"
  fails=$((fails+1))
fi

# --- hardware checks (WARN only; needed for ASCEND-CASE 2/3/4) ---
if command -v npu-smi >/dev/null 2>&1; then
  n="$(npu-smi info 2>/dev/null | grep -cE '910|310|Ascend' || true)"
  row PASS "npu-smi" "present (chips matched: ${n})"
else
  row WARN "npu-smi" "absent — ASCEND-CASE 2/3/4 (hardware) unavailable"
fi

if docker info 2>/dev/null | grep -qiE 'Runtimes:.*ascend|Default Runtime: ascend'; then
  row PASS "ascend-runtime" "$(docker info 2>/dev/null | grep -i 'Default Runtime' | tr -s ' ')"
else
  row WARN "ascend-runtime" "ascend docker runtime not detected — ASCEND-CASE 2/3/4 need it"
fi

if ls /dev/davinci0 >/dev/null 2>&1; then
  row PASS "davinci-devices" "$(ls /dev/davinci[0-9]* 2>/dev/null | tr '\n' ' ')"
else
  row WARN "davinci-devices" "/dev/davinci* absent — ASCEND-CASE 2/3/4 need a real NPU"
fi

# --- NVIDIA hardware checks (WARN only; needed for NVIDIA-CASE 2/3) ---
if command -v nvidia-smi >/dev/null 2>&1; then
  g="$(nvidia-smi -L 2>/dev/null | grep -c '^GPU ' || true)"
  row PASS "nvidia-smi" "present (GPUs: ${g}; NVIDIA-CASE 3 needs >= 2)"
else
  row WARN "nvidia-smi" "absent — NVIDIA-CASE 2/3 (hardware) unavailable"
fi

if docker info 2>/dev/null | grep -qiE 'Runtimes:.*nvidia|Default Runtime: nvidia'; then
  row PASS "nvidia-runtime" "$(docker info 2>/dev/null | grep -i 'Default Runtime' | tr -s ' ')"
else
  row WARN "nvidia-runtime" "nvidia docker runtime not detected — NVIDIA-CASE 2/3 need it (or run with --gpus all)"
fi

if ls /dev/nvidia0 >/dev/null 2>&1; then
  row PASS "nvidia-devices" "$(ls /dev/nvidia[0-9]* 2>/dev/null | tr '\n' ' ')"
else
  row WARN "nvidia-devices" "/dev/nvidia* absent — NVIDIA-CASE 2/3 need a real GPU"
fi

echo "FAILS=${fails}"
PAYLOAD
)"
echo "${out}"

# Mirror the payload's FAILS into our exit code so callers (and CI) can detect a
# failed required check from the return status, not just by reading the table.
echo "${out}" | grep -q 'FAILS=0' && exit 0 || exit 1
