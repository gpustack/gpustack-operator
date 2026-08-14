#!/usr/bin/env bash
#
# CASE 26 — Partition token health is a node-level count: allocated + remaining
#   (MUTATING, self-recovering; AUTO-SKIPS without a partition-capable node AND a node-SSH address)
#
#   MIG_NODE_SSH=<user@host> case-26.sh <NS>
#
# Goal:        The partition family's tokens are a fungible count, not a card selection, so their
#              health answers only one question: how many more instances can this node host. The node
#              advertises allocated + remaining healthy tokens. The allocated term is load-bearing —
#              the scheduler subtracts the requests of the Pods already on the node, so publishing
#              bare remaining would lose one slot per live instance. The consequence this case proves
#              is the one an operator sees: on a node with no room left, allocatable still equals the
#              live instance count while the scheduler's FREE view is zero, so a further partition Pod
#              is held; the running partitions are untouched; and freeing one raises the count again
#              with no restart of anything.
# Environment: A reachable cluster whose active context is the GPU cluster, an nvidia node with at
#              least one card that can be put into a hardware partitioning mode, AND SSH to that node
#              (sudo nvidia-smi) supplied via MIG_NODE_SSH=<user@host>. It EXITS 2 (input required)
#              when MIG_NODE_SSH is unset, and AUTO-SKIPS (exit 0) when the card reports no
#              partitioning mode, when the mode switch does not converge, or when the cards offer no
#              whole-card profile to saturate them with.
# Inputs:      All real, nothing mocked — the node's token pool and the live geometry are the
#              verification. The case partitions card MIG_GPU_INDEX if no card is partitioned yet, then
#              saturates every partitioned card with ONE whole-card-profile instance each (the cheapest
#              way to leave a card with no room for any profile), submitted sequentially through the
#              pool's entrance LocalQueue. Profiles are DISCOVERED from the cards' own capability.
#              Survival across a kubelet restart is deliberately NOT exercised here — the interesting
#              path is the one taken when the container is STOPPED at restart time, which has its own
#              case.
# Expected:    - an idle partitioned card set advertises its full partition ceiling healthy;
#              - once every partitioned card holds a whole-card instance, allocatable equals the live
#                instance count (one per card) — not zero, and not the ceiling;
#              - a further partition Pod is HELD, because the scheduler's free view is zero;
#              - every already-running partition Pod is still Running and still holds exactly one
#                hardware instance;
#              - freeing one instance raises allocatable again with no restart, and a fresh partition
#                request admits.
# Cleanup:     Trap deletes every test Pod, waits for the live instances to reclaim, and restores the
#              partitioning mode of the card this case toggled (a card found already partitioned is
#              left as found). Idempotent; runs on pass AND fail.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: MIG_NODE_SSH=<user@host> case-26.sh <NS>}"
CASE_ID=26
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_partition-lib.sh"

PODPFX=gpustack-e2e-saturate

part_require_node_ssh "case-26.sh"
part_require_mig_capable
part_discover

restore() {
  echo
  echo "[case-26] cleanup: deleting test Pods and restoring card(s) '${TOGGLED:-<none>}'"
  delete_test_pods
  part_restore_toggled
}
trap restore EXIT

part_ensure_partitioned_card

read -r SMALL SMALL_CNT MID MID_CNT FULL FULL_CNT NPART NTOT <<<"$(card_profiles)"
FULLKEY="$(profile_key "$FULL")"
SMALLKEY="$(profile_key "$SMALL")"
CEILING="$(partition_ceiling)"
echo "[case-26] discovered profiles: SMALL=${SMALL}(${SMALL_CNT}/card) FULL=${FULL}(${FULL_CNT}/card); partitioned cards=${NPART:-0}, partition ceiling=${CEILING:-0}"
if [ -z "$FULLKEY" ] || [ -z "$SMALLKEY" ] || [ "${NPART:-0}" -lt 1 ]; then
  part_skip \
    "The node advertises no whole-card profile key ('${FULLKEY:-<none>}') or no smallest-profile key" \
    "('${SMALLKEY:-<none>}') over ${NPART:-0} partitioned card(s). This case saturates each card with one" \
    "whole-card instance, so it needs both."
fi

wait_card_idle || echo "[case-26] warning: the card already holds live instances — the ceiling baseline may be off"

# ---------------------------------------------------------------------------------------------------
# Baseline — an empty node advertises its whole partition ceiling healthy.
# ---------------------------------------------------------------------------------------------------
echo
echo "[case-26] === baseline: idle partitioned card(s) advertise the full ceiling ==="
base_tok=""
for _ in $(seq 1 20); do
  base_tok="$(node_key "$PARTITIONED")"
  [ "${base_tok:-x}" = "${CEILING}" ] && break
  sleep 3
done
if [ "${base_tok:-x}" = "${CEILING}" ]; then
  record PASS "idle node advertises the full partition ceiling" "${PARTITIONED}=${base_tok} = Σ per-card ceiling over ${NPART} partitioned card(s)"
else
  record FAIL "idle node advertises the full partition ceiling" "${PARTITIONED}='${base_tok:-<absent>}', want ${CEILING} — an empty partitioned card must advertise every slot it could host as Healthy"
fi

# ---------------------------------------------------------------------------------------------------
# Saturate: one whole-card instance per partitioned card leaves no room for any profile.
# ---------------------------------------------------------------------------------------------------
echo
echo "[case-26] === saturating ${NPART} partitioned card(s) with one ${FULL} each ==="
FILLED=()
for i in $(seq 1 "$NPART"); do
  p="${PODPFX}-full-${i}"
  mkpod "$p" "$(partition_reslines "$FULLKEY")"
  if ! wait_running "$p"; then
    record FAIL "every partitioned card is saturated" "${p} did not reach Running (held: $(held_reason "$p")) — cannot saturate the node"
    part_results "Partition token health is a node-level count: allocated + remaining"
    exit 1
  fi
  FILLED+=("$p")
done
record PASS "every partitioned card is saturated" "${#FILLED[@]}× ${FULL} Running, node reports $(node_gi_count) live hardware instance(s)"

sat_tok=""
for _ in $(seq 1 30); do
  sat_tok="$(node_key "$PARTITIONED")"
  [ "${sat_tok:-x}" = "${#FILLED[@]}" ] && break
  sleep 3
done
if [ "${sat_tok:-x}" = "${#FILLED[@]}" ]; then
  record PASS "a saturated node advertises exactly its live instance count" "${PARTITIONED}=${sat_tok} = ${#FILLED[@]} allocated + 0 remaining; the scheduler subtracts the running Pods' requests, so its free view is zero"
else
  record FAIL "a saturated node advertises exactly its live instance count" "${PARTITIONED}='${sat_tok:-<absent>}', want ${#FILLED[@]} — allocatable must keep the allocated term, or the scheduler double-subtracts the live instances (too low) / keeps attracting Pods (too high)"
fi
# Capacity is the operator-owned side and must not have collapsed with the free view.
sat_cap="$(node_key_cap "$PARTITIONED")"
[ -n "$sat_cap" ] \
  && record PASS "capacity survives saturation" "${PARTITIONED} capacity=${sat_cap} while allocatable=${sat_tok:-?} — the family is saturated, not absent" \
  || record FAIL "capacity survives saturation" "${PARTITIONED} has no capacity entry — a saturated family must keep its keys, or they would be deleted and re-added on every release"

# ---------------------------------------------------------------------------------------------------
# A further partition Pod is held, and the running ones are untouched.
# ---------------------------------------------------------------------------------------------------
EXTRA="${PODPFX}-extra"
mkpod "$EXTRA" "$(partition_reslines "$SMALLKEY")"
hr="$(assert_held "$EXTRA")"
[ -n "$hr" ] \
  && record PASS "a further partition request is held on a full node" "${EXTRA} held [${hr}] — no room for even the smallest profile" \
  || record FAIL "a further partition request is held on a full node" "${EXTRA} shows no concrete held signal (phase=$(phase "$EXTRA")) — the node has no room, so the request must not be admitted onto it"

stillok=1
for p in "${FILLED[@]}"; do
  running "$p" || stillok=0
  [ "$(pod_mig_devices "$p")" = 1 ] || stillok=0
done
[ "$stillok" = 1 ] \
  && record PASS "running partitions are unaffected by saturation" "${#FILLED[@]}× ${FULL} still Running, each still holding exactly one hardware instance" \
  || record FAIL "running partitions are unaffected by saturation" "at least one saturating Pod is no longer Running or no longer sees exactly one instance — a health recount must never disturb a live allocation"

# ---------------------------------------------------------------------------------------------------
# Freeing one instance raises the count again, with no restart of anything.
# ---------------------------------------------------------------------------------------------------
echo
echo "[case-26] === freeing one instance restores the count ==="
delpod "$EXTRA"
FREED="${FILLED[0]}"
delpod "$FREED"
freed_tok=""
for _ in $(seq 1 40); do
  freed_tok="$(node_key "$PARTITIONED")"
  [ -n "$freed_tok" ] && [ "${freed_tok}" -gt "${sat_tok:-0}" ] && break
  sleep 3
done
if [ -n "$freed_tok" ] && [ "${freed_tok}" -gt "${sat_tok:-0}" ]; then
  record PASS "freeing an instance raises the healthy count" "${PARTITIONED}: ${sat_tok} → ${freed_tok} after ${FREED} was deleted, with no plugin or kubelet restart"
else
  record FAIL "freeing an instance raises the healthy count" "${PARTITIONED} still '${freed_tok:-<absent>}' after freeing an instance (was ${sat_tok:-?}) — released room must reappear on the next ListAndWatch cycle"
fi

REFILL="${PODPFX}-refill"
mkpod "$REFILL" "$(partition_reslines "$SMALLKEY")"
if wait_running "$REFILL"; then
  record PASS "the freed room is usable again" "${REFILL} (${SMALL}) Running on the room ${FREED} released"
else
  record FAIL "the freed room is usable again" "${REFILL} did not reach Running (held: $(held_reason "$REFILL")) — a released slot must be allocatable without a restart"
fi

part_results "Partition token health is a node-level count: allocated + remaining"

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). A node advertises Σ (allocated + remaining) healthy partition tokens, never"
  echo "bare remaining and never zero while instances live. Diagnose:"
  echo "  kubectl get node ${GPU_NODE} -o json | jq '{cap:.status.capacity, alloc:.status.allocatable} | map_values(with_entries(select(.key|test(\"partitioned\"))))'"
  echo "  kubectl get devices ${GPU_NODE} -o json | jq '.status.groups[].accelerators[] | {id, allocatedProfiles, remainingProfiles}'"
  echo "  ${MIG_NODE_SSH} sudo nvidia-smi mig -lgi"
  echo "  kubectl -n ${NS} logs ds/${DM_DS} --tail=200"
  exit 1
fi
echo "CASE 26 PASS"
