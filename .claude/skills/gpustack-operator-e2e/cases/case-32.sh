#!/usr/bin/env bash
#
# CASE 32 — An instance carved outside GPUStack: placement sees it, the node keys never do
#   (MUTATING, self-recovering; AUTO-SKIPS without a partition-capable node AND a node-SSH address)
#
#   MIG_NODE_SSH=<user@host> case-32.sh <NS>
#
# Goal:        RECORD what a managed node does when an administrator carves a hardware partition by
#              hand. Every node-level number — the per-profile capacity, the partition token health and
#              the admission check — is derived from the annotations the device plugin writes, and a
#              hand-carved instance produces no annotation, so it is invisible to all of them: the node
#              keeps advertising room it does not have, and unlike a transient over-advertisement that
#              never converges while any GPUStack workload holds the card. Once the card is idle the
#              other half of the story shows: an instance no allocation accounts for is an orphan, and
#              the reclaimer destroys it even though it never created it. Placement is the one layer
#              that reads the live hardware, so it will not double-book the card — and that is the one
#              thing this case asserts, because it is the difference between an accounting error and a
#              corrupted card. Everything else is measured and printed, which is what makes
#              "unsupported on a managed node" a documented consequence rather than a guess.
# Environment: A reachable cluster whose active context is the GPU cluster, an nvidia node with at
#              least one card that can be put into a hardware partitioning mode, AND SSH to that node
#              (sudo nvidia-smi, including the MIG instance management subcommands) supplied via
#              MIG_NODE_SSH=<user@host>. It EXITS 2 (input required) when MIG_NODE_SSH is unset, and
#              AUTO-SKIPS (exit 0) when the card reports no partitioning mode, the mode switch does not
#              converge, the profile pair is not offered, or the hand-carve command does not produce an
#              instance.
# Inputs:      Mostly real, one deliberate intrusion: the case creates ONE partition directly on the
#              card with the vendor tool over SSH — outside GPUStack entirely, with no Pod and no
#              annotation — and then reads the node's own keys and submits a whole-card-profile Pod
#              through the pool's entrance LocalQueue. The profile and its vendor profile id are
#              DISCOVERED from the card, never composed.
# Expected:    - the hand-carved instance exists on the card;
#              - the node's per-profile keys are RECORDED before and after it, showing whether they
#                move at all (the residual says they do not);
#              - a whole-card-profile Pod is NOT admitted onto the half-occupied card — placement reads
#                the live hardware, so it refuses rather than double-booking;
#              - whether GPUStack's own reclaimer removes the foreign instance is recorded either way.
# Cleanup:     Trap deletes every test Pod, destroys the compute and GPU instances it created on the
#              card with the vendor tool (unconditionally, so a failed run leaves no foreign partition
#              behind), and restores the partitioning mode of the card this case toggled (a card found
#              already partitioned is left as found). Idempotent; runs on pass AND fail.
set -uo pipefail

NS="${1:?usage: MIG_NODE_SSH=<user@host> case-32.sh <NS>}"
CASE_ID=32
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_partition-lib.sh"

PODPFX=gpustack-e2e-outofband
OBSERVE_WINDOW="${MIG_OOB_WINDOW:-120}"
OOB_CREATED=0

part_require_node_ssh "case-32.sh"
part_require_mig_capable
part_discover

restore() {
  echo
  echo "[case-32] cleanup: deleting test Pods, destroying any hand-carved instance, restoring card(s) '${TOGGLED:-<none>}'"
  delete_test_pods
  if [ "$OOB_CREATED" = 1 ]; then
    node_ssh sudo nvidia-smi mig -i "$GPU_INDEX" -dci >/dev/null 2>&1 || true
    node_ssh sudo nvidia-smi mig -i "$GPU_INDEX" -dgi >/dev/null 2>&1 || true
    echo "[case-32]   hand-carved instance destroyed; card ${GPU_INDEX} now reports $(node_gi_count) live instance(s)"
  fi
  part_restore_toggled
}
trap restore EXIT

part_ensure_partitioned_card

read -r SMALL SMALL_CNT MID MID_CNT FULL FULL_CNT NPART NTOT <<<"$(card_profiles)"
MIDKEY="$(profile_key "$MID")"
FULLKEY="$(profile_key "$FULL")"
if [ -z "$MIDKEY" ] || [ -z "$FULLKEY" ] || [ "${NPART:-0}" != 1 ]; then
  part_skip \
    "This case needs exactly ONE partitioned card offering both a 4-memory-slice and a whole-card" \
    "profile (partitioned cards=${NPART:-0}, MID key='${MIDKEY:-<none>}', FULL key='${FULLKEY:-<none>}')." \
    "With more partitioned cards the node-level over-advertisement is not isolatable."
fi
wait_card_idle || echo "[case-32] warning: the card already holds live instances"

BEFORE_FULL="$(node_key "$FULLKEY")"
BEFORE_MID="$(node_key "$MIDKEY")"
BEFORE_TOK="$(node_key "$PARTITIONED")"
echo "[case-32] before the intrusion: ${FULLKEY}=${BEFORE_FULL:-<absent>} ${MIDKEY}=${BEFORE_MID:-<absent>} ${PARTITIONED}=${BEFORE_TOK:-<absent>}"

# ---------------------------------------------------------------------------------------------------
# The intrusion: carve one instance with the vendor tool, outside GPUStack.
# ---------------------------------------------------------------------------------------------------
echo
echo "[case-32] === hand-carving one ${MID} instance with the vendor tool ==="
GIP_ID="$(node_ssh sudo nvidia-smi mig -i "$GPU_INDEX" -lgip 2>/dev/null | PROF="$MID" python3 -c "
import os,sys
prof=os.environ['PROF']
for line in sys.stdin:
    t=line.replace('|',' ').split()
    if prof in t:
        i=t.index(prof)
        if i+1 < len(t) and t[i+1].isdigit():
            print(t[i+1]); break
" 2>/dev/null)"
if [ -z "$GIP_ID" ]; then
  part_skip \
    "Could not read the vendor profile id for '${MID}' from 'nvidia-smi mig -i ${GPU_INDEX} -lgip'." \
    "Without it the case cannot carve an instance out of band."
fi
echo "[case-32]   vendor profile id for ${MID}: ${GIP_ID}"
node_ssh sudo nvidia-smi mig -i "$GPU_INDEX" -cgi "$GIP_ID" -C >/dev/null 2>&1 || true
OOB_CREATED=1
gi="$(node_gi_count)"
if [ "${gi:-0}" -lt 1 ]; then
  part_skip \
    "The hand-carve produced no instance (card reports ${gi:-0}). The card may be busy or the driver may" \
    "refuse the shape; nothing to observe."
fi
record PASS "a foreign instance exists on a managed card" "${gi} live instance(s) on card ${GPU_INDEX} after 'nvidia-smi mig -cgi ${GIP_ID} -C', with no Pod and no allocation record anywhere in the cluster"

# ---------------------------------------------------------------------------------------------------
# What the node-level numbers do about it: by design, nothing.
# ---------------------------------------------------------------------------------------------------
echo "[case-32] watching the node keys for ${OBSERVE_WINDOW}s"
moved=0
for _ in $(seq 1 $((OBSERVE_WINDOW / 5))); do
  af="$(node_key "$FULLKEY")"
  [ "${af:-x}" != "${BEFORE_FULL:-x}" ] && { moved=1; break; }
  sleep 5
done
AFTER_FULL="$(node_key "$FULLKEY")"
AFTER_MID="$(node_key "$MIDKEY")"
AFTER_TOK="$(node_key "$PARTITIONED")"
if [ "$moved" = 0 ]; then
  record PASS "OBSERVED: the node keys ignore the foreign instance" "${FULLKEY} stayed ${AFTER_FULL:-<absent>}, ${MIDKEY} stayed ${AFTER_MID:-<absent>}, ${PARTITIONED} stayed ${AFTER_TOK:-<absent>} over ${OBSERVE_WINDOW}s — the node advertises room the card no longer has, and this never converges"
else
  record PASS "OBSERVED: the node keys ignore the foreign instance" "UNEXPECTED but recorded: ${FULLKEY} moved ${BEFORE_FULL:-?} → ${AFTER_FULL:-?} (${MIDKEY}: ${BEFORE_MID:-?} → ${AFTER_MID:-?}) — something on the node-level path is seeing hardware the annotations do not describe"
fi

# Does GPUStack's own reclaimer take the foreign instance away? An instance no allocation accounts for
# is an orphan, and the reclaimer destroys an orphan once its card carries no live claim — so on an
# otherwise-idle card the expected reading is GONE, and a surviving instance means the card was not idle.
gi_now="$(node_gi_count)"
[ "${gi_now:-0}" -ge 1 ] \
  && record PASS "OBSERVED: what the reclaimer does with the foreign instance" "${gi_now} live instance(s) still present ${OBSERVE_WINDOW}s after the intrusion — the card was not fully drained, so its orphans are held" \
  || record PASS "OBSERVED: what the reclaimer does with the foreign instance" "the instance is GONE after ${OBSERVE_WINDOW}s — the reclaimer destroyed a partition it did not create, which is orphan GC working as designed on an idle card"

# ---------------------------------------------------------------------------------------------------
# The one hard assertion: placement reads the live hardware, so it must not double-book the card.
# ---------------------------------------------------------------------------------------------------
echo
echo "[case-32] === a whole-card request against the half-occupied card ==="
if [ "${gi_now:-0}" -lt 1 ]; then
  record SKIP "placement refuses to double-book the occupied card" "the foreign instance no longer exists, so there is nothing for placement to avoid"
else
  B="${PODPFX}-full"
  mkpod "$B" "$(partition_reslines "$FULLKEY")"
  ran=0
  for _ in $(seq 1 20); do running "$B" && { ran=1; break; }; sleep 3; done
  if [ "$ran" = 1 ]; then
    record FAIL "placement refuses to double-book the occupied card" "${B} reached Running although half the card is taken by a foreign instance — placement must read the live hardware, not only the annotation ledger"
  else
    record PASS "placement refuses to double-book the occupied card" "${B} did not run [$(held_reason "$B")] — the node key said there was room, placement knew better"
  fi
  delpod "$B"
fi

part_results "An instance carved outside GPUStack: placement sees it, the node keys never do"

echo
echo "---- fold back into the spec: out-of-band instance ----"
echo "profile carved by hand : ${MID} (vendor profile id ${GIP_ID})"
echo "per-profile keys       : ${FULLKEY} ${BEFORE_FULL:-?} → ${AFTER_FULL:-?}; ${MIDKEY} ${BEFORE_MID:-?} → ${AFTER_MID:-?}"
echo "partition token pool   : ${PARTITIONED} ${BEFORE_TOK:-?} → ${AFTER_TOK:-?}"
echo "observation window     : ${OBSERVE_WINDOW}s"
echo "instance still present : $([ "${gi_now:-0}" -ge 1 ] && echo yes || echo 'no — reclaimed by GPUStack')"
echo "-------------------------------------------------------"

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). Hand-carving a partition on a managed node is unsupported and the node-level"
  echo "keys cannot see it — but placement reads the live hardware and must never double-book. Diagnose:"
  echo "  ${MIG_NODE_SSH} sudo nvidia-smi mig -lgi"
  echo "  kubectl get devices ${GPU_NODE} -o json | jq '.status.groups[].accelerators[] | {id, allocatedProfiles, remainingProfiles}'"
  echo "  kubectl -n ${NS} logs ds/${DM_DS} --tail=300"
  exit 1
fi
echo "CASE 32 PASS"
