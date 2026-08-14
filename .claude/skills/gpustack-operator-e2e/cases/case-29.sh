#!/usr/bin/env bash
#
# CASE 29 — Two concurrent requests for different profiles each get their own instance
#   (MUTATING, self-recovering; AUTO-SKIPS without a partition-capable node AND a node-SSH address)
#
#   MIG_NODE_SSH=<user@host> case-29.sh <NS>
#
# Goal:        The device-plugin allocation RPC carries a resource name and a device count and nothing
#              else — no profile, no Pod identity — so when two Pods are admitted together, both
#              carrying the same bare partition key and differing only in the profile they name, the
#              plugin must still actuate each one's OWN profile. Getting this wrong is silent rather
#              than loud: the wrong hardware shape is carved and both Pods start, so nothing fails
#              until a tenant notices the instance is not the size they asked for. This case submits
#              the two requests simultaneously and checks the shape each Pod actually received, from
#              two independent sources: the hardware itself, through the device list inside the
#              container, and the durable per-Pod allocation record the plugin wrote. It also checks
#              that neither Pod's record was overwritten with the other's — the record is keyed by
#              container, and a second allocation must accumulate rather than erase.
# Environment: A reachable cluster whose active context is the GPU cluster, an nvidia node with at
#              least one card that can be put into a hardware partitioning mode, AND SSH to that node
#              (sudo nvidia-smi) supplied via MIG_NODE_SSH=<user@host>. It EXITS 2 (input required)
#              when MIG_NODE_SSH is unset, and AUTO-SKIPS (exit 0) when the card reports no
#              partitioning mode, the mode switch does not converge, or the cards offer fewer than two
#              distinct profiles that can coexist.
# Inputs:      All real, nothing mocked — the actuated hardware shapes are the verification. Two Pods
#              are created back to back with no wait between them, one naming the DISCOVERED
#              4-memory-slice profile and one the smallest profile, both through the pool's entrance
#              LocalQueue. The two shapes are chosen so they fit side by side on one card, which is
#              what makes the resolution — rather than the geometry — the thing under test.
# Expected:    - both Pods reach Running;
#              - the device list inside each Pod names the profile that Pod requested, and exactly one
#                instance;
#              - each Pod's durable allocation record names its own profile and not the other's;
#              - neither Pod records an UnexpectedAdmissionError.
# Cleanup:     Trap deletes every test Pod, waits for the instances to reclaim, and restores the
#              partitioning mode of the card this case toggled (a card found already partitioned is
#              left as found). Idempotent; runs on pass AND fail.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: MIG_NODE_SSH=<user@host> case-29.sh <NS>}"
CASE_ID=29
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_partition-lib.sh"

PODPFX=gpustack-e2e-concprof

part_require_node_ssh "case-29.sh"
part_require_mig_capable
part_discover

restore() {
  echo
  echo "[case-29] cleanup: deleting test Pods and restoring card(s) '${TOGGLED:-<none>}'"
  delete_test_pods
  part_restore_toggled
}
trap restore EXIT

part_ensure_partitioned_card

read -r SMALL SMALL_CNT MID MID_CNT FULL FULL_CNT NPART NTOT <<<"$(card_profiles)"
MIDKEY="$(profile_key "$MID")"
SMALLKEY="$(profile_key "$SMALL")"
echo "[case-29] discovered profiles: SMALL=${SMALL} MID=${MID}; partitioned cards=${NPART:-0}"
if [ -z "$MIDKEY" ] || [ -z "$SMALLKEY" ] || [ "$SMALL" = "$MID" ]; then
  part_skip \
    "The node does not advertise two distinct coexisting profile keys (SMALL='${SMALLKEY:-<none>}'," \
    "MID='${MIDKEY:-<none>}'). This case needs two different shapes that fit one card side by side."
fi
wait_card_idle || echo "[case-29] warning: the card already holds live instances"

# ---------------------------------------------------------------------------------------------------
# Submit both at once — no wait between them, so the two allocations are in flight together.
# ---------------------------------------------------------------------------------------------------
echo
echo "[case-29] === two simultaneous requests: ${MID} and ${SMALL} ==="
PM="${PODPFX}-mid"
PS="${PODPFX}-small"
mkpod "$PM" "$(partition_reslines "$MIDKEY")"
mkpod "$PS" "$(partition_reslines "$SMALLKEY")"

mrun=1; wait_running "$PM" || mrun=0
srun=1; wait_running "$PS" || srun=0
if [ "$mrun" = 1 ] && [ "$srun" = 1 ]; then
  record PASS "both distinct-profile requests run" "${PM} (${MID}) and ${PS} (${SMALL}) both Running; node reports $(node_gi_count) live instance(s)"
else
  record FAIL "both distinct-profile requests run" "${PM} running=${mrun} (held: $(held_reason "$PM")), ${PS} running=${srun} (held: $(held_reason "$PS")) — both shapes fit one card, so both must be placed"
fi

# ---------------------------------------------------------------------------------------------------
# What each Pod actually got, from the hardware and from the durable record.
# ---------------------------------------------------------------------------------------------------
check_one() {
  local pod="$1" want="$2" other="$3" smi devs rec
  smi="$(kubectl -n default exec "$pod" -- nvidia-smi -L 2>/dev/null)"
  devs="$(printf '%s\n' "$smi" | grep -c 'MIG')"
  rec="$(pod_profiles "$pod")"
  if printf '%s\n' "$smi" | grep -q "$want" && [ "${devs:-0}" = 1 ]; then
    record PASS "${pod} was given the ${want} it asked for" "device list names ${want}, exactly ${devs} instance visible"
  else
    record FAIL "${pod} was given the ${want} it asked for" "device list = '$(printf '%s' "$smi" | tr '\n' ';')' (want one ${want} instance) — a request was resolved to the wrong container's profile"
  fi
  if [ "$rec" = "$want" ]; then
    record PASS "${pod}'s durable record names its own profile" "recorded profile '${rec}', with no trace of ${other}"
  else
    record FAIL "${pod}'s durable record names its own profile" "recorded profile '${rec:-<none>}', want exactly '${want}' — the per-container allocation record was cross-patched or lost"
  fi
}
[ "$mrun" = 1 ] && check_one "$PM" "$MID" "$SMALL"
[ "$srun" = 1 ] && check_one "$PS" "$SMALL" "$MID"

uae=0
for p in "$PM" "$PS"; do
  n="$(pod_unexpected_admission "$p")"
  [ -n "$n" ] && uae=$((uae + n))
done
[ "$uae" = 0 ] \
  && record PASS "no terminal admission failure" "0 UnexpectedAdmissionError events across both Pods" \
  || record FAIL "no terminal admission failure" "${uae} UnexpectedAdmissionError event(s) — a profile the card could not host was attempted"

part_results "Two concurrent requests for different profiles each get their own instance"

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). Two Pods that differ only in the profile they name must each have their own"
  echo "shape actuated: the plugin narrows the candidate containers by what the offered cards can serve, and"
  echo "records each container's allocation under its own key. Diagnose:"
  echo "  kubectl -n default get pod ${PM} -o jsonpath='{.metadata.annotations.${ANNO}}'; echo"
  echo "  kubectl -n default get pod ${PS} -o jsonpath='{.metadata.annotations.${ANNO}}'; echo"
  echo "  ${MIG_NODE_SSH} sudo nvidia-smi mig -lgi"
  echo "  kubectl -n ${NS} logs ds/${DM_DS} --tail=200"
  exit 1
fi
echo "CASE 29 PASS"
