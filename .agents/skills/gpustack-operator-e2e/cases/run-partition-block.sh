#!/usr/bin/env bash
#
# Run the hardware-partition cases in their required order. MUTATING (every case
# toggles node hardware) — the skill confirms before running this.
#
#   MIG_NODE_SSH=<user@host> run-partition-block.sh <RAW_DIR> [NS] [CASES...]
#
# Why a runner and not a list in prose: the family has a real ordering constraint
# and a run is long enough to outlive the lead's context, so a step remembered
# only in conversation is a step that gets dropped after a compaction.
#
#   CASE 27 FIRST. It measures the before/after delta of the whole-card and
#   shared keys across the first partitioning, so it skips outright if any card
#   is already partitioned.
#   CASE 24 SECOND. It needs to establish a mixed population from an all-whole
#   node.
#   THE REST in any order. Each partitions a card only if none is partitioned
#   yet and restores exactly the card it toggled.
#
# The cases all toggle the SAME node hardware, so they can never overlap; this is
# serial by construction. A failure is recorded and the block CONTINUES, because
# each case is self-recovering and the next one's precondition is the restored
# baseline, not its predecessor's verdict.
#
# Every case writes to <RAW_DIR>/5<N>-case<N>.txt and its exit code is echoed, so
# the block's outcome is readable from the files alone after a compaction.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

RAW="${1:?usage: run-partition-block.sh <RAW_DIR> [NS] [CASES...]}"
NS="${2:-gpustack-system}"
shift 2 2>/dev/null || shift $#
CASES="${*:-27 24 25 26 28 29 30 31 32 34}"
: "${MIG_NODE_SSH:?MIG_NODE_SSH=<user@host> required — ask the user, never hardcode it}"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
mkdir -p "$RAW"

# block_ssh <cmd...> — the same ssh options the cases use, including MIG_NODE_SSH_OPTS (identity file,
# port, jump host). Unquoted on purpose so the options split into words, as in the lib.
block_ssh() {
  # shellcheck disable=SC2086
  ssh -o StrictHostKeyChecking=no -o BatchMode=yes -o ConnectTimeout=15 ${MIG_NODE_SSH_OPTS:-} "$MIG_NODE_SSH" "$@"
}

# mig_modes — "<index>, <mode>" per card, as nvidia-smi reports it.
mig_modes() { block_ssh sudo nvidia-smi --query-gpu=index,mig.mode.current --format=csv,noheader 2>/dev/null; }

# cards_to_disable <start-modes> <end-modes> — the indexes that were Disabled when the block started
# and are Enabled now. Only those were switched by the block; a card already Enabled at the start is
# the lead's and is left as found.
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

# Record every card's mode before any case runs. Without it the final restore cannot tell a card the
# block switched from one the lead enabled on purpose, so the block refuses to start.
START_MODES="$(mig_modes)"
if [ -z "$START_MODES" ]; then
  echo "!! could not read the cards' MIG modes over SSH before the block; refusing to run, because the"
  echo "   final restore could not tell which cards the block switched. Check MIG_NODE_SSH / MIG_NODE_SSH_OPTS."
  exit 1
fi
echo "   MIG modes at block start:"
printf '%s\n' "$START_MODES" | sed 's/^/     /'

echo "== partition block: cases ${CASES} =="
echo "   namespace ${NS}, raw output under ${RAW}"
FAILED=""
for n in $CASES; do
  [ -f "${HERE}/case-${n}.sh" ] || { echo "!! no case-${n}.sh — skipping"; continue; }
  out="${RAW}/5${n}-case${n}.txt"
  echo
  echo "---------- CASE ${n} start $(date +%H:%M:%S) ----------"
  # Hold the partitioning mode across the block: each case restores what it toggled, which in a block
  # only means the next case pays to switch it back on. CASE 27 must see an unpartitioned node, so it
  # runs before the lease is taken; the block restores once at the end.
  keep=1; [ "$n" = 27 ] && keep=0
  MIG_NODE_SSH="$MIG_NODE_SSH" MIG_KEEP_MODE="$keep" bash "${HERE}/case-${n}.sh" "$NS" >"$out" 2>&1
  rc=$?
  echo "CASE ${n} exit=${rc} $(date +%H:%M:%S) -> ${out}"
  [ "$rc" -ne 0 ] && FAILED="${FAILED}${n} "
  tail -25 "$out"
done

echo "---------- restoring the partitioning mode the block held ----------"
END_MODES="$(mig_modes)"
[ -n "$END_MODES" ] || echo "!! could not read the cards' MIG modes over SSH; no card restored — check them by hand"
for c in $(cards_to_disable "$START_MODES" "$END_MODES"); do
  echo "   card ${c}: Disabled at block start, Enabled now — disabling"
  block_ssh sudo nvidia-smi -i "$c" -mig 0 >/dev/null 2>&1 || true
done
kubectl -n "$NS" rollout restart ds/gpustack-operator-device-manager-nvidia >/dev/null 2>&1
kubectl -n "$NS" rollout status ds/gpustack-operator-device-manager-nvidia --timeout=300s >/dev/null 2>&1
block_ssh sudo nvidia-smi --query-gpu=index,mig.mode.current --format=csv,noheader 2>&1 | sed 's/^/  /'

echo
if [ -n "$FAILED" ]; then
  echo "PARTITION_BLOCK_DONE — non-zero exits: ${FAILED}"
  echo "(exit 2 means the case required input it was not given; exit 0 can still be an auto-skip)"
  exit 1
fi
echo "PARTITION_BLOCK_DONE — all cases exited 0"
