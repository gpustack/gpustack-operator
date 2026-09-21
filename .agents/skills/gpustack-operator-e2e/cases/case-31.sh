#!/usr/bin/env bash
#
# CASE 31 — A same-profile replacement scheduled inside the reclaim window
#   (MUTATING, self-recovering; AUTO-SKIPS without a partition-capable node AND a node-SSH address)
#
#   MIG_NODE_SSH=<user@host> case-31.sh <NS>
#
# Goal:        MEASURE, rather than assume away, the gap between the accounting freeing a partition and
#              the hardware actually releasing it. Node-level occupancy is rebuilt from the Pod
#              annotations, so a deleted Pod's slot reappears in the per-profile key and in the healthy
#              token count the moment the Pod is gone — while the reclaimer only destroys the instance
#              after several consecutive absent sightings on its resync cadence. A replacement
#              scheduled inside that window meets an instance that still exists, and what it then gets
#              is MEASURED here rather than assumed: the allocation may be refused, or it may be
#              granted that very instance which the reclaimer then destroys underneath the starting
#              container. Both cost a retry, they are told apart by WHERE the attempt died, and only
#              the first raises an UnexpectedAdmissionError -- the second leaves that count at zero.
#              So this case records the shape of every failed attempt, not just how many there were.
#              The containment is that the request is retried and the window closes on its own; the
#              failure mode worth catching is a request that NEVER converges. So this case deletes a
#              partition Pod, asks for the same profile again immediately, retries the way a controller
#              would, and records how long convergence took, how many attempts it cost, and where each
#              failed attempt died.
# Environment: A reachable cluster whose active context is the GPU cluster, an nvidia node with at
#              least one card that can be put into a hardware partitioning mode, AND SSH to that node
#              (sudo nvidia-smi) supplied via MIG_NODE_SSH=<user@host>. It EXITS 2 (input required)
#              when MIG_NODE_SSH is unset, and AUTO-SKIPS (exit 0) when the card reports no
#              partitioning mode, the mode switch does not converge, or no 4-memory-slice profile is
#              offered.
# Inputs:      All real, nothing mocked — the reclaim cadence and the allocation outcome are the
#              measurement. One partition Pod of the DISCOVERED 4-memory-slice profile is created,
#              deleted, and then re-requested in a retry loop bounded by MIG_RECLAIM_BOUND seconds
#              (default 300), all through the pool's entrance LocalQueue. A bare Pod is not
#              re-created by anything, so the loop plays the part the controller plays in production.
# Expected:    - the first Pod runs and is deleted;
#              - the replacement eventually runs within the bound — that is the containment claim, and
#                the only hard assertion here;
#              - the attempt count, the elapsed seconds and any terminal allocation failures along the
#                way are RECORDED as the measurement of the window, not asserted against a threshold.
# Cleanup:     Trap deletes every test Pod, waits for the instances to reclaim, and restores the
#              partitioning mode of the card this case toggled (a card found already partitioned is
#              left as found). Idempotent; runs on pass AND fail.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: MIG_NODE_SSH=<user@host> case-31.sh <NS>}"
CASE_ID=31
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_partition-lib.sh"

PODPFX=gpustack-e2e-reclaimwin
BOUND="${MIG_RECLAIM_BOUND:-300}"

part_require_node_ssh "case-31.sh"
part_require_mig_capable
part_discover

restore() {
  echo
  echo "[case-31] cleanup: deleting test Pods and restoring card(s) '${TOGGLED:-<none>}'"
  delete_test_pods
  part_restore_toggled
}
trap restore EXIT

part_ensure_partitioned_card

read -r SMALL SMALL_CNT MID MID_CNT FULL FULL_CNT NPART NTOT <<<"$(card_profiles)"
MIDKEY="$(profile_key "$MID")"
[ -n "$MIDKEY" ] || part_skip "The node advertises no 4-memory-slice profile key — nothing to replace."
echo "[case-31] profile under test: ${MID} (${MIDKEY}); partitioned cards=${NPART:-0}"
wait_card_idle || echo "[case-31] warning: the card already holds live instances"

# ---------------------------------------------------------------------------------------------------
# Occupy, then release, then immediately ask for the same shape again.
# ---------------------------------------------------------------------------------------------------
echo
echo "[case-31] === one ${MID} partition, deleted, then re-requested at once ==="
A="${PODPFX}-first"
mkpod "$A" "$(partition_reslines "$MIDKEY")"
if ! wait_running "$A"; then
  record FAIL "the first partition runs" "${A} did not reach Running (held: $(held_reason "$A")) — cannot set up the reclaim window"
  part_results "A same-profile replacement scheduled inside the reclaim window"
  exit 1
fi
GI_BEFORE="$(node_gi_count)"
record PASS "the first partition runs" "${A} Running on card(s) $(pod_cards "$A"); ${GI_BEFORE} live hardware instance(s)"

delpod "$A"
T0="$(date +%s)"
ATTEMPTS=0
UAE=0
# Where the failed attempts died, tallied separately from UAE. A grant that is destroyed under the
# starting container raises no admission event, so UAE stays 0 through exactly the failure this window
# is most likely to produce; without this tally the run reads as clean.
SHAPE_ADMISSION=0
SHAPE_START=0
SHAPE_OTHER=0
OK=0
WINNER=""
while [ "$(( $(date +%s) - T0 ))" -lt "$BOUND" ]; do
  ATTEMPTS=$((ATTEMPTS + 1))
  R="${PODPFX}-repl-${ATTEMPTS}"
  mkpod "$R" "$(partition_reslines "$MIDKEY")"
  for _ in $(seq 1 20); do
    ph="$(phase "$R")"
    case "$ph" in
      Running) OK=1; WINNER="$R"; break ;;
      Failed) break ;;
    esac
    sleep 3
  done
  n="$(pod_unexpected_admission "$R")"
  [ -n "$n" ] && UAE=$((UAE + n))
  [ "$OK" = 1 ] && break
  # Read the shape ONCE: the Pod is live and a second read can answer differently.
  shape="$(pod_failure_shape "$R")"
  case "$shape" in
    admission) SHAPE_ADMISSION=$((SHAPE_ADMISSION + 1)) ;;
    start)     SHAPE_START=$((SHAPE_START + 1)) ;;
    *)         SHAPE_OTHER=$((SHAPE_OTHER + 1)) ;;
  esac
  echo "[case-31]   attempt ${ATTEMPTS} did not run (phase=$(phase "$R"), where=${shape}, held: $(held_reason "$R")) — retrying"
  delpod "$R"
  sleep 5
done
T1="$(date +%s)"
ELAPSED=$((T1 - T0))

if [ "$OK" = 1 ]; then
  record PASS "the replacement converges without intervention" "${WINNER} Running ${ELAPSED}s after the predecessor was deleted, on attempt ${ATTEMPTS} of at most ${BOUND}s"
else
  record FAIL "the replacement converges without intervention" "no same-profile replacement reached Running within ${BOUND}s over ${ATTEMPTS} attempt(s) — the window is supposed to close on its own"
fi
record PASS "OBSERVED: cost of the reclaim window" "attempts=${ATTEMPTS}, elapsed=${ELAPSED}s, refused before actuating=${SHAPE_ADMISSION} (UnexpectedAdmissionError events=${UAE}), granted then destroyed under the container=${SHAPE_START}, other=${SHAPE_OTHER} (a first attempt that runs immediately means the window was already closed on this cadence; a non-zero 'granted then destroyed' means the window handed out a partition it went on to reclaim, which no admission counter records)"

part_results "A same-profile replacement scheduled inside the reclaim window"

echo
echo "---- fold back into the spec: reclaim-window replacement ----"
echo "profile          : ${MID}"
echo "attempts         : ${ATTEMPTS}"
echo "elapsed          : ${ELAPSED}s (bound ${BOUND}s)"
echo "refused          : ${SHAPE_ADMISSION} attempt(s) never actuated (${UAE} UnexpectedAdmissionError event(s))"
echo "granted then lost: ${SHAPE_START} attempt(s) were given a partition no container could then start on"
echo "converged        : $([ "$OK" = 1 ] && echo yes || echo NO)"
echo "-------------------------------------------------------------"

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). The accounting frees a partition on Pod deletion while the reclaimer destroys"
  echo "the instance a few resync passes later; a replacement inside that window is retried, and the window must"
  echo "close on its own. Read the shape line above before diagnosing: an attempt refused before actuating and an"
  echo "attempt granted a partition that was then destroyed under it are different faults in different code."
  echo "Diagnose:"
  echo "  ${MIG_NODE_SSH} sudo nvidia-smi mig -lgi"
  echo "  kubectl -n ${NS} logs ds/${DM_DS} --tail=300 | grep -i reclaim"
  echo "  kubectl get devices ${GPU_NODE} -o json | jq '.status.groups[].accelerators[] | {id, allocatedProfiles, remainingProfiles}'"
  exit 1
fi
echo "CASE 31 PASS"
