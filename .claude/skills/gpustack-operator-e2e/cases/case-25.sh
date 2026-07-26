#!/usr/bin/env bash
#
# CASE 25 — Per-profile capacity is derived from the live ledger, not from a static ceiling
#   (MUTATING, self-recovering; AUTO-SKIPS without a partition-capable node AND a node-SSH address)
#
#   MIG_NODE_SSH=<user@host> case-25.sh <NS>
#
# Goal:        A node's per-profile partition key reports what its own cards can still host, because
#              the profiles compete for the same physical slices. Carving one 4-memory-slice instance
#              consumes half a card, so the whole-card profile stops fitting on that card and its key
#              falls — on a node whose only partitioned card holds one such instance, to zero — while
#              the same-size profile's key is unchanged, because the key publishes
#              allocated + remaining and the scheduler subtracts the running Pod's own request from it.
#              A static ceiling would keep advertising the whole-card profile and attract a Pod that
#              cannot be placed. The case also MEASURES what this costs: the ledger-derived key
#              re-patches node capacity on every partition allocate and release, so the Node-object
#              write volume is counted over an idle window and over a carve/free window and PRINTED for
#              the record. Finally it records — as an observation, not an assertion — the residual the
#              key shape does not close: the per-profile keys are independently subtracted scalars, so
#              two Pods naming mutually exclusive profiles can both be admitted to a node that can host
#              only one of them, and the loser is failed closed at Allocate and retried.
# Environment: A reachable cluster whose active context is the GPU cluster, an nvidia node with at
#              least one card that can be put into a hardware partitioning mode, AND SSH to that node
#              (sudo nvidia-smi) supplied via MIG_NODE_SSH=<user@host>. It EXITS 2 (input required)
#              when MIG_NODE_SSH is unset, and AUTO-SKIPS (exit 0) when the card reports no
#              partitioning mode or the mode switch does not converge. The strict "reads zero" and the
#              residual observation need exactly ONE partitioned card; with more, the arithmetic is
#              asserted relative to the partitioned card count and the held-request sub-check records
#              SKIPPED.
# Inputs:      All real, nothing mocked — the node's own keys and the live geometry are the
#              verification. The case partitions card MIG_GPU_INDEX if no card is partitioned yet,
#              carves one instance of the discovered 4-memory-slice profile through the pool's entrance
#              LocalQueue, and reads the node's allocatable keys around it. Profiles are DISCOVERED
#              from the cards' own capability, never composed.
# Expected:    - on an idle partitioned card set, the whole-card profile key equals the partitioned
#                card count and the 4-slice profile key equals twice it;
#              - after ONE 4-slice instance is carved, the whole-card profile key drops by exactly one
#                card's worth (zero when there is a single partitioned card), while the 4-slice key is
#                unchanged at allocated + remaining;
#              - with a single partitioned card so occupied, a whole-card-profile Pod is HELD rather
#                than admitted onto a node that cannot place it;
#              - the Node-object write counts over both windows are recorded and printed;
#              - the mutually-exclusive-profile residual is recorded with its measured outcome.
# Cleanup:     Trap deletes every test Pod, waits for the live instances to reclaim, stops the write
#              probe, and restores the partitioning mode of the card this case toggled (a card it found
#              already partitioned is left as found). Idempotent; runs on pass AND fail.
set -uo pipefail

NS="${1:?usage: MIG_NODE_SSH=<user@host> case-25.sh <NS>}"
CASE_ID=25
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_partition-lib.sh"

PODPFX=gpustack-e2e-ledgercap
IDLE_WINDOW="${MIG_WRITE_IDLE_WINDOW:-60}"

part_require_node_ssh "case-25.sh"
part_require_mig_capable
part_discover

TOGGLED=""
restore() {
  echo
  echo "[case-25] cleanup: deleting test Pods, stopping the write probe, restoring card(s) '${TOGGLED:-<none>}'"
  stop_node_write_probe
  [ -n "$NODE_WRITE_LOG" ] && rm -f "$NODE_WRITE_LOG"
  delete_test_pods
  part_restore_toggled
}
trap restore EXIT

# ---------------------------------------------------------------------------------------------------
# Make sure the node has a partitioned card, then discover the geometry from the cards themselves.
# ---------------------------------------------------------------------------------------------------
part_ensure_partitioned_card

read -r SMALL SMALL_CNT MID MID_CNT FULL FULL_CNT NPART NTOT <<<"$(card_profiles)"
MIDKEY="$(profile_key "$MID")"
FULLKEY="$(profile_key "$FULL")"
echo "[case-25] discovered profiles: SMALL=${SMALL}(${SMALL_CNT}/card) MID=${MID}(${MID_CNT}/card) FULL=${FULL}(${FULL_CNT}/card); partitioned cards=${NPART:-0} of ${NTOT:-0}"
if [ -z "$MIDKEY" ] || [ -z "$FULLKEY" ] || [ "${NPART:-0}" -lt 1 ]; then
  part_skip \
    "The node advertises no usable per-profile pair (MID key='${MIDKEY:-<none>}', FULL key='${FULLKEY:-<none>}'," \
    "partitioned cards=${NPART:-0}). This case needs a partitioned card offering both a 4-memory-slice" \
    "and a whole-card profile."
fi

# The card must start empty, or the baseline arithmetic below is not the empty-card one.
wait_card_idle || echo "[case-25] warning: the card already holds live instances — the baseline may not be the empty-card one"

# ---------------------------------------------------------------------------------------------------
# Baseline: an idle partitioned card set advertises its full geometry.
# ---------------------------------------------------------------------------------------------------
echo
echo "[case-25] === baseline: idle partitioned card(s) ==="
base_mid="$(node_key "$MIDKEY")"
base_full="$(node_key "$FULLKEY")"
want_mid=$((MID_CNT * NPART))
want_full=$((FULL_CNT * NPART))
if [ "${base_mid:-x}" = "$want_mid" ] && [ "${base_full:-x}" = "$want_full" ]; then
  record PASS "idle per-profile keys equal the cards' geometry" "${MIDKEY}=${base_mid} (want ${want_mid}), ${FULLKEY}=${base_full} (want ${want_full}) over ${NPART} partitioned card(s)"
else
  record FAIL "idle per-profile keys equal the cards' geometry" "${MIDKEY}='${base_mid:-<absent>}' (want ${want_mid}), ${FULLKEY}='${base_full:-<absent>}' (want ${want_full}) — an empty card must advertise its full per-profile ceiling"
fi

# ---------------------------------------------------------------------------------------------------
# Write volume, part 1 — the idle baseline the carve window is compared against.
# ---------------------------------------------------------------------------------------------------
echo "[case-25] measuring Node-object writes over ${IDLE_WINDOW}s of partition inactivity"
start_node_write_probe
sleep "$IDLE_WINDOW"
stop_node_write_probe
IDLE_WRITES="$(node_write_count)"
rm -f "$NODE_WRITE_LOG"
echo "[case-25]   idle window: ${IDLE_WRITES} Node write(s) in ${IDLE_WINDOW}s"

# ---------------------------------------------------------------------------------------------------
# Carve one mid-size instance and watch the keys move: the whole-card profile loses that card, the
# same-size profile does not, because the key is allocated + remaining.
# ---------------------------------------------------------------------------------------------------
echo
echo "[case-25] === carving one ${MID} instance ==="
start_node_write_probe
CARVE_T0=$(date +%s)
A="${PODPFX}-mid"
mkpod "$A" "$(partition_reslines "$MIDKEY")"
if ! wait_running "$A"; then
  record FAIL "a mid-size partition admits" "${A} did not reach Running (held: $(held_reason "$A")) — cannot set up the occupancy the rest of the case reads"
  stop_node_write_probe
  part_results "Per-profile capacity is derived from the live ledger, not from a static ceiling"
  exit 1
fi
record PASS "a mid-size partition admits" "${A} Running on card(s) $(wait_pod_cards "$A"), profile recorded '$(pod_profiles "$A")'"

exp_full=$((want_full - FULL_CNT))
cur_full=""; cur_mid=""
for _ in $(seq 1 30); do
  cur_full="$(node_key "$FULLKEY")"
  cur_mid="$(node_key "$MIDKEY")"
  [ "${cur_full:-x}" = "$exp_full" ] && break
  sleep 3
done
if [ "${cur_full:-x}" = "$exp_full" ]; then
  record PASS "the whole-card profile key falls to what the cards can still host" "${FULLKEY}: ${base_full} → ${cur_full} (want ${exp_full}$([ "$exp_full" = 0 ] && echo ', the single partitioned card is half occupied')) after one ${MID} was carved"
else
  record FAIL "the whole-card profile key falls to what the cards can still host" "${FULLKEY}='${cur_full:-<absent>}', want ${exp_full} — the key is still reporting a static ceiling the geometry cannot serve"
fi
if [ "${cur_mid:-x}" = "$want_mid" ]; then
  record PASS "the same-size profile key still reads allocated + remaining" "${MIDKEY}=${cur_mid} unchanged (1 allocated + $((MID_CNT - 1)) remaining on the carved card) — publishing bare remaining would subtract the running instance twice"
else
  record FAIL "the same-size profile key still reads allocated + remaining" "${MIDKEY}='${cur_mid:-<absent>}', want ${want_mid} — the allocated term is missing, so the scheduler will double-subtract the live instance"
fi

# With one partitioned card, a whole-card-profile Pod must not be admitted onto this node at all.
if [ "${NPART:-0}" = 1 ]; then
  B="${PODPFX}-full"
  mkpod "$B" "$(partition_reslines "$FULLKEY")"
  hr="$(assert_held "$B")"
  if [ -n "$hr" ]; then
    record PASS "a whole-card-profile Pod is not placed on the occupied node" "${B} held [${hr}] — the zero-valued key kept the scheduler off this node"
  else
    record FAIL "a whole-card-profile Pod is not placed on the occupied node" "${B} shows no concrete held signal (phase=$(phase "$B")) — a whole-card profile cannot fit a card that already holds a 4-slice instance"
  fi
  delpod "$B"
else
  record SKIP "a whole-card-profile Pod is not placed on the occupied node" "${NPART} partitioned cards — a sibling card can legitimately host the whole-card profile, so the node-level refusal is not assertable here"
fi

# ---------------------------------------------------------------------------------------------------
# Free the instance again and close the write window.
# ---------------------------------------------------------------------------------------------------
delpod "$A"
for _ in $(seq 1 30); do
  [ "$(node_key "$FULLKEY")" = "$want_full" ] && break
  sleep 3
done
CARVE_T1=$(date +%s)
stop_node_write_probe
CARVE_WRITES="$(node_write_count)"
rm -f "$NODE_WRITE_LOG"
CARVE_SECS=$((CARVE_T1 - CARVE_T0))
record PASS "OBSERVED: node-status write volume of the ledger-derived key" "idle ${IDLE_WRITES} write(s)/${IDLE_WINDOW}s; one carve+free cycle ${CARVE_WRITES} write(s)/${CARVE_SECS}s (sampled at 1 Hz on the Node object's resourceVersion)"

# ---------------------------------------------------------------------------------------------------
# The residual this key shape does not close, DEMONSTRATED rather than asserted: the per-profile keys
# are independently subtracted scalars, so two Pods naming mutually exclusive profiles can both be
# admitted to a node that can host only one. The containment is that Allocate fails closed and the
# admission check retries; the case records what actually happened.
# ---------------------------------------------------------------------------------------------------
echo
echo "[case-25] === observation: mutually exclusive profiles on one card ==="
wait_card_idle || true
if [ "${NPART:-0}" = 1 ]; then
  X="${PODPFX}-x-full"; Y="${PODPFX}-x-mid"
  mkpod "$X" "$(partition_reslines "$FULLKEY")"
  mkpod "$Y" "$(partition_reslines "$MIDKEY")"
  xr=1; wait_running "$X" || xr=0
  yr=1; wait_running "$Y" || yr=0
  xu="$(pod_unexpected_admission "$X")"; yu="$(pod_unexpected_admission "$Y")"
  both=$((xr + yr))
  if [ "$both" -le 1 ]; then
    if [ "$xr" = 0 ]; then loser="$X"; loser_prof="$FULL"; else loser="$Y"; loser_prof="$MID"; fi
    record PASS "OBSERVED: mutually exclusive profiles do not both run" "only $both of 2 ran on the single card; ${loser} (${loser_prof}) did not [$(held_reason "$loser")] and stayed held rather than corrupting the card; UnexpectedAdmissionError X=${xu:-0} Y=${yu:-0}"
  else
    record FAIL "OBSERVED: mutually exclusive profiles do not both run" "BOTH ${X} (${FULL}) and ${Y} (${MID}) reached Running on ONE card — the geometry cannot host both, so a placement was double-booked"
  fi
  delpods "$X" "$Y"
else
  record SKIP "OBSERVED: mutually exclusive profiles do not both run" "${NPART} partitioned cards — the two profiles can legitimately land on different cards, so the node-level window is not observable here"
fi

part_results "Per-profile capacity is derived from the live ledger, not from a static ceiling"

echo
echo "---- fold back into the spec: node-status write volume ----"
echo "idle window       : ${IDLE_WRITES} Node write(s) over ${IDLE_WINDOW}s with no partition activity"
echo "carve+free window : ${CARVE_WRITES} Node write(s) over ${CARVE_SECS}s covering one allocate and one release"
echo "method            : 1 Hz poll of node/${GPU_NODE} .metadata.resourceVersion, counting transitions"
echo "-----------------------------------------------------------"

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). Each per-profile key must publish Σ (allocated + remaining) over the node's"
  echo "partitioned cards, so the scheduler's free view is the room the geometry actually has. Diagnose:"
  echo "  kubectl get devices ${GPU_NODE} -o json | jq '.status.groups[].accelerators[] | {id, allocatedProfiles, remainingProfiles}'"
  echo "  kubectl get node ${GPU_NODE} -o json | jq '.status.allocatable | with_entries(select(.key|test(\"partitioned\")))'"
  echo "  kubectl -n ${NS} logs deploy/gpustack-operator-worker --tail=200 | grep -i 'node capacity'"
  exit 1
fi
echo "CASE 25 PASS"
