#!/usr/bin/env bash
#
# Run the hardware-partition cases serially in their required order. MUTATING — the skill confirms
# before running this.
#
#   # Real hardware (NVIDIA MIG or Hygon DCU Multi-Instance):
#   MIG_NODE_SSH=<user@host> run-partition-block.sh <RAW_DIR> [NS] [CASES...]
#
#   # CPU-only Mock Device Manager (deviceManager.mock.enabled=true, no SSH needed):
#   run-partition-block.sh <RAW_DIR> [NS] [CASES...]
#
# Default case sequence:
#   - With MIG_NODE_SSH set (real hardware):  9 7 13
#       * CASE 9  — real NVIDIA MIG hardware lifecycle (starts unpartitioned in Phase 2 to measure
#                   whole-card exclusion delta, exercises carve/mixed/live-ledger/concurrent/reclaim,
#                   and disables MIG in Phase 8; auto-skips on non-NVIDIA hosts).
#       * CASE 7  — unified SSH Instance suite (Leg 4 verifies partition-backed SSH sidecar
#                   confinement; on an NVIDIA MIG host where CASE 9 ended Disabled, this runner
#                   temporarily enables MIG on MIG_GPU_INDEX before CASE 7 and restores at the end).
#       * CASE 13 — real Hygon DCU Multi-Instance partitions (auto-skips on non-Hygon hosts).
#   - Without MIG_NODE_SSH when CPU Mock Device Manager is active:  5 7
#       * CASE 5  — CPU Mock logical-slice packing/co-residency & virtual MIG lifecycle.
#       * CASE 7  — unified SSH Instance suite (hot-reloading Mock DM to nvidia-h100-mig for Leg 4).
#
# Every case writes to <RAW_DIR>/case-<N>.txt and its exit code is echoed, so the block's outcome
# is readable from the files alone after a compaction.
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

RAW="${1:?usage: run-partition-block.sh <RAW_DIR> [NS] [CASES...]}"
NS="${2:-gpustack-system}"
shift 2 2>/dev/null || shift $#

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "${HERE}/_mock-dm-lib.sh"

mkdir -p "$RAW"

# CPU-only Mock Device Manager mode when MIG_NODE_SSH is not supplied.
if [ -z "${MIG_NODE_SSH:-}" ]; then
  if mock_dm_detect_env "$NS"; then
    CASES="${*:-5 7}"
    echo "== partition block (CPU Mock Device Manager mode): cases ${CASES} =="
    echo "   namespace ${NS}, mock ds ${MOCK_DM_DS}, raw output under ${RAW}"
    FAILED=""
    for n in $CASES; do
      if [ ! -f "${HERE}/case-${n}.sh" ]; then
        echo "!! no case-${n}.sh — skipping"
        continue
      fi
      out="${RAW}/case-${n}.txt"
      echo
      echo "---------- CASE ${n} start $(date +%H:%M:%S) ----------"
      bash "${HERE}/case-${n}.sh" "$NS" >"$out" 2>&1
      rc=$?
      echo "CASE ${n} exit=${rc} $(date +%H:%M:%S) -> ${out}"
      [ "$rc" -ne 0 ] && FAILED="${FAILED}${n} "
      tail -25 "$out"
    done
    echo
    if [ -n "$FAILED" ]; then
      echo "PARTITION_BLOCK_DONE — non-zero exits: ${FAILED}"
      exit 1
    fi
    echo "PARTITION_BLOCK_DONE — all cases exited 0"
    exit 0
  fi

  echo "!! MIG_NODE_SSH=<user@host> is required on real hardware (and no active Mock Device Manager was detected in ${NS})." >&2
  echo "   Ask the user for MIG_NODE_SSH (never hardcode it), or enable deviceManager.mock.enabled=true for CPU Mock MIG." >&2
  exit 2
fi

CASES="${*:-9 7 13}"
GPU_INDEX="${MIG_GPU_INDEX:-0}"

# block_ssh <cmd...> — the same ssh options the cases use, including MIG_NODE_SSH_OPTS.
block_ssh() {
  # shellcheck disable=SC2086
  ssh -o StrictHostKeyChecking=no -o BatchMode=yes -o ConnectTimeout=15 ${MIG_NODE_SSH_OPTS:-} "$MIG_NODE_SSH" "$@"
}

if ! block_ssh true >/dev/null 2>&1; then
  echo "!! could not reach ${MIG_NODE_SSH} over SSH; check MIG_NODE_SSH / MIG_NODE_SSH_OPTS." >&2
  exit 1
fi

# mig_modes — "<index>, <mode>" per NVIDIA card when nvidia-smi is present on the node, empty on
# non-NVIDIA hosts (such as Hygon DCU Multi-Instance nodes, whose mode is never toggled here).
mig_modes() { block_ssh sudo nvidia-smi --query-gpu=index,mig.mode.current --format=csv,noheader 2>/dev/null || true; }

cards_to_disable() {
  local start="$1" end="$2" line idx mode was=" "
  while IFS= read -r line; do
    idx="$(printf '%s' "${line%%,*}" | tr -d '[:space:]')"
    mode="$(printf '%s' "${line#*,}" | tr -d '[:space:]')"
    [ -n "$idx" ] && [ "$mode" = Disabled ] && was="${was}${idx} "
  done <<<"$start"
  while IFS= read -r line; do
    idx="$(printf '%s' "${line%%,*}" | tr -d '[:space:]')"
    mode="$(printf '%s' "${line#*,}" | tr -d '[:space:]')"
    case "$was" in *" ${idx} "*) [ "$mode" = Enabled ] && echo "$idx" ;; esac
  done <<<"$end"
  return 0
}

any_mig_enabled() {
  local line mode
  while IFS= read -r line; do
    mode="$(printf '%s' "${line#*,}" | tr -d '[:space:]')"
    [ "$mode" = Enabled ] && return 0
  done <<<"$1"
  return 1
}

START_MODES="$(mig_modes)"
if [ -n "$START_MODES" ]; then
  echo "   NVIDIA MIG modes at block start:"
  printf '%s\n' "$START_MODES" | sed 's/^/     /'
else
  echo "   non-NVIDIA or non-MIG host (${MIG_NODE_SSH}); no nvidia-smi MIG mode to snapshot"
fi

restore_nvidia_modes() {
  [ -n "$START_MODES" ] || return 0
  echo "---------- restoring NVIDIA partitioning mode to block-start state ----------"
  local end_modes c restored=0
  end_modes="$(mig_modes)"
  if [ -z "$end_modes" ]; then
    echo "!! could not read NVIDIA MIG modes over SSH; check cards manually"
    return 0
  fi
  for c in $(cards_to_disable "$START_MODES" "$end_modes"); do
    echo "   card ${c}: Disabled at block start, Enabled now — disabling"
    block_ssh sudo nvidia-smi -i "$c" -mig 0 >/dev/null 2>&1 || true
    restored=1
  done
  if [ "$restored" -eq 1 ]; then
    kubectl -n "$NS" rollout restart ds/gpustack-operator-device-manager-nvidia >/dev/null 2>&1 || true
    kubectl -n "$NS" rollout status ds/gpustack-operator-device-manager-nvidia --timeout=300s >/dev/null 2>&1 || true
  fi
  mig_modes | sed 's/^/  /'
}
trap restore_nvidia_modes EXIT

echo "== partition block: cases ${CASES} =="
echo "   namespace ${NS}, raw output under ${RAW}"
FAILED=""
for n in $CASES; do
  if [ ! -f "${HERE}/case-${n}.sh" ]; then
    echo "!! no case-${n}.sh — skipping"
    continue
  fi
  # CASE 7 Leg 4 exercises partition-backed SSH sidecar confinement when a partitioned card is
  # already active. On an NVIDIA MIG host where no card is currently Enabled (for instance right
  # after CASE 9 Phase 8 disabled MIG), enable MIG on GPU_INDEX so Leg 4 runs against real MIG.
  if [ "$n" = "7" ] && [ -n "$START_MODES" ] && ! any_mig_enabled "$(mig_modes)"; then
    echo "   enabling MIG on card ${GPU_INDEX} before CASE 7 so Leg 4 sees a live real MIG partition"
    if block_ssh sudo nvidia-smi -i "$GPU_INDEX" -mig 1 >/dev/null 2>&1; then
      kubectl -n "$NS" rollout restart ds/gpustack-operator-device-manager-nvidia >/dev/null 2>&1 || true
      kubectl -n "$NS" rollout status ds/gpustack-operator-device-manager-nvidia --timeout=300s >/dev/null 2>&1 || true
      sleep 10
    fi
  fi
  out="${RAW}/case-${n}.txt"
  echo
  echo "---------- CASE ${n} start $(date +%H:%M:%S) ----------"
  MIG_NODE_SSH="$MIG_NODE_SSH" MIG_KEEP_MODE=0 bash "${HERE}/case-${n}.sh" "$NS" >"$out" 2>&1
  rc=$?
  echo "CASE ${n} exit=${rc} $(date +%H:%M:%S) -> ${out}"
  [ "$rc" -ne 0 ] && FAILED="${FAILED}${n} "
  tail -25 "$out"
done

echo
if [ -n "$FAILED" ]; then
  echo "PARTITION_BLOCK_DONE — non-zero exits: ${FAILED}"
  echo "(exit 2 means the case required input it was not given; exit 0 can still be an auto-skip)"
  exit 1
fi
echo "PARTITION_BLOCK_DONE — all cases exited 0"
