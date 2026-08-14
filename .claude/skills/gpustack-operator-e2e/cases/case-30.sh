#!/usr/bin/env bash
#
# CASE 30 — A terminated init container still charges the card its instance occupies
#   (MUTATING, self-recovering; AUTO-SKIPS without a partition-capable node AND a node-SSH address)
#
#   MIG_NODE_SSH=<user@host> case-30.sh <NS>
#
# Goal:        A device is scoped to the Pod's life, not the container's. The kubelet keeps a finished
#              init container's devices in its record for as long as the Pod exists, and the operator's
#              reclaimer destroys a hardware partition on Pod deletion — not on container termination.
#              So a Pod whose init container took a partition and then exited, with the Pod still
#              running, is STILL occupying that hardware. The accounting has to say so: the node's
#              per-profile capacity must keep charging the instance, or the node advertises room it
#              does not have and the next tenant is admitted onto a card that cannot take it. Only
#              hardware shows this — a ledger fixture passes whether or not the entry survives the
#              container. The case therefore reads the live instance from the card, reads the node key,
#              and then admits a further Pod against the room the node claims is left, requiring it to
#              actually start.
# Environment: A reachable cluster whose active context is the GPU cluster, an nvidia node with at
#              least one card that can be put into a hardware partitioning mode, AND SSH to that node
#              (sudo nvidia-smi) supplied via MIG_NODE_SSH=<user@host>. It EXITS 2 (input required)
#              when MIG_NODE_SSH is unset, and AUTO-SKIPS (exit 0) when the card reports no
#              partitioning mode, the mode switch does not converge, or the cards offer no
#              4-memory-slice plus whole-card profile pair.
# Inputs:      All real, nothing mocked. One Pod whose INIT container requests the DISCOVERED
#              4-memory-slice partition and exits after a few seconds, and whose app container holds no
#              accelerator at all — the request rules allow accelerator claims in exactly one container
#              group, and this Pod puts them in the init group. Then a second, ordinary partition Pod
#              of the same profile, to consume the room the node still reports.
# Expected:    - the Pod reaches Running with its init container Terminated;
#              - the card still holds a live hardware instance;
#              - the whole-card profile key still reads one card's worth less, i.e. the terminated
#                container's instance is still charged;
#              - the follow-up partition Pod, admitted against the remaining reported room, reaches
#                Running without a single UnexpectedAdmissionError.
# Cleanup:     Trap deletes every test Pod, waits for the instances to reclaim, and restores the
#              partitioning mode of the card this case toggled (a card found already partitioned is
#              left as found). Idempotent; runs on pass AND fail.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: MIG_NODE_SSH=<user@host> case-30.sh <NS>}"
CASE_ID=30
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_partition-lib.sh"

PODPFX=gpustack-e2e-initterm

part_require_node_ssh "case-30.sh"
part_require_mig_capable
part_discover

restore() {
  echo
  echo "[case-30] cleanup: deleting test Pods and restoring card(s) '${TOGGLED:-<none>}'"
  delete_test_pods
  part_restore_toggled
}
trap restore EXIT

part_ensure_partitioned_card

read -r SMALL SMALL_CNT MID MID_CNT FULL FULL_CNT NPART NTOT <<<"$(card_profiles)"
MIDKEY="$(profile_key "$MID")"
FULLKEY="$(profile_key "$FULL")"
echo "[case-30] discovered profiles: MID=${MID}(${MID_CNT}/card) FULL=${FULL}(${FULL_CNT}/card); partitioned cards=${NPART:-0}"
if [ -z "$MIDKEY" ] || [ -z "$FULLKEY" ]; then
  part_skip \
    "The node advertises no 4-memory-slice key ('${MIDKEY:-<none>}') or no whole-card key" \
    "('${FULLKEY:-<none>}'). This case reads the whole-card key to see the init container's charge."
fi
wait_card_idle || echo "[case-30] warning: the card already holds live instances"
BASE_FULL="$(node_key "$FULLKEY")"
echo "[case-30] baseline ${FULLKEY}=${BASE_FULL:-<absent>}"

# ---------------------------------------------------------------------------------------------------
# A Pod whose INIT container holds the partition and then exits, while the Pod keeps running.
# ---------------------------------------------------------------------------------------------------
echo
echo "[case-30] === init container takes a ${MID} partition, then terminates ==="
P="${PODPFX}-pod"
TESTPODS+=("$P")
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: { name: ${P}, namespace: default, labels: { kueue.x-k8s.io/queue-name: ${LQ} } }
spec:
  schedulerName: default-scheduler
  ${RTC_LINE}
  restartPolicy: Never
  nodeSelector: { kubernetes.io/hostname: ${GPU_NODE} }
  initContainers:
    - name: warmup
      image: ${IMAGE}
      command: ["sh", "-c", "nvidia-smi -L || true; sleep 15"]
      resources:
        limits:
$(partition_reslines "$MIDKEY")
        requests:
$(partition_reslines "$MIDKEY")
  containers:
    - name: main
      image: ${IMAGE}
      command: ["sleep", "86400"]
      resources:
        limits:   { cpu: "200m", memory: 256Mi }
        requests: { cpu: "200m", memory: 256Mi }
EOF

if ! wait_running "$P" 60; then
  record FAIL "the Pod runs on past its init container" "${P} did not reach Running (phase=$(phase "$P"), held: $(held_reason "$P")) — an init-group accelerator claim must be admitted"
  part_results "A terminated init container still charges the card its instance occupies"
  exit 1
fi
initstate="$(kubectl -n default get pod "$P" -o jsonpath='{.status.initContainerStatuses[0].state.terminated.reason}' 2>/dev/null)"
if [ -n "$initstate" ]; then
  record PASS "the Pod runs on past its init container" "${P} Running with init container Terminated/${initstate}, card(s) $(pod_cards "$P"), profile '$(pod_profiles "$P")'"
else
  record FAIL "the Pod runs on past its init container" "${P} is Running but its init container reports no terminated state — cannot exercise the terminated-container charge"
fi

# ---------------------------------------------------------------------------------------------------
# The hardware is still occupied, and the accounting says so.
# ---------------------------------------------------------------------------------------------------
gi="$(node_gi_count)"
[ "${gi:-0}" -ge 1 ] \
  && record PASS "the hardware instance outlives the container" "${gi} live instance(s) on the card while the init container is long gone — the reclaimer destroys on Pod deletion, not container exit" \
  || record FAIL "the hardware instance outlives the container" "the card reports ${gi:-0} live instance(s) — the instance was destroyed while its Pod is still alive"

exp_full=$((BASE_FULL - FULL_CNT))
cur_full=""
for _ in $(seq 1 30); do
  cur_full="$(node_key "$FULLKEY")"
  [ "${cur_full:-x}" = "$exp_full" ] && break
  sleep 3
done
if [ "${cur_full:-x}" = "$exp_full" ]; then
  record PASS "the terminated container's instance is still charged" "${FULLKEY}: ${BASE_FULL} → ${cur_full} (want ${exp_full}) — the occupied card can no longer host a whole-card profile"
else
  record FAIL "the terminated container's instance is still charged" "${FULLKEY}='${cur_full:-<absent>}', want ${exp_full} — the ledger dropped a terminated container's entry and is advertising room the card does not have"
fi

# ---------------------------------------------------------------------------------------------------
# Admit against the room the node still claims: it must be real room.
# ---------------------------------------------------------------------------------------------------
echo
echo "[case-30] === a further request against the reported free room ==="
Q="${PODPFX}-next"
mkpod "$Q" "$(partition_reslines "$MIDKEY")"
if wait_running "$Q"; then
  uae="$(pod_unexpected_admission "$Q")"
  [ -z "$uae" ] \
    && record PASS "the reported free room is real" "${Q} (${MID}) Running beside the init container's instance, 0 UnexpectedAdmissionError" \
    || record FAIL "the reported free room is real" "${Q} Running but recorded ${uae} UnexpectedAdmissionError event(s) — the node advertised room the card could not place"
else
  record FAIL "the reported free room is real" "${Q} did not reach Running (held: $(held_reason "$Q")) — the node's remaining per-profile room must be placeable"
fi

part_results "A terminated init container still charges the card its instance occupies"

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). An allocation charges its card until its POD is gone, never until its"
  echo "container exits — a liveness filter would report a card free while its instance still occupies"
  echo "memory slices. Diagnose:"
  echo "  kubectl -n default get pod ${P} -o jsonpath='{.metadata.annotations.${ANNO}}'; echo"
  echo "  kubectl get devices ${GPU_NODE} -o json | jq '.status.groups[].accelerators[] | {id, allocatedProfiles, remainingProfiles}'"
  echo "  ${MIG_NODE_SSH} sudo nvidia-smi mig -lgi"
  exit 1
fi
echo "CASE 30 PASS"
