#!/usr/bin/env bash
#
# CASE 24 — Mixed node: a partition lands on a partitioned card, a logical slice on a whole one
#   (MUTATING, self-recovering; AUTO-SKIPS without a partition-capable node of at least two cards
#    AND a node-SSH address)
#
#   MIG_NODE_SSH=<user@host> case-24.sh <NS>
#
# Goal:        On ONE node whose cards of a single model are deliberately mixed — some put into a
#              hardware partitioning mode, the rest left whole — every partition request lands on a
#              partitionable card and every logical-slice request on an unpartitioned one, with ZERO
#              UnexpectedAdmissionError. This is the regression guard for the production failure the
#              two-family split exists to remove: a single token pool spanning both card populations
#              let the kubelet hand a partition request a token belonging to a card that cannot be
#              partitioned, the instance could not be created, and the Pod died terminally.
#              The guarantee is CONSTRUCTIVE, not lucky: each family's token pool is advertised only by
#              the cards that can serve it, and the partition family's Allocate picks the card itself.
#              So the case runs several rounds and requires every one of them to place correctly.
# Environment: A reachable cluster whose active context is the GPU cluster, a node with at least TWO
#              nvidia cards of one model where at least one card can be put into a partitioning mode,
#              AND SSH to that node (sudo nvidia-smi) supplied via MIG_NODE_SSH=<user@host>. It EXITS 2
#              (input required) when MIG_NODE_SSH is unset. It AUTO-SKIPS (exit 0) when the card
#              reports no partitioning mode, when the node carries fewer than two cards, or when the
#              mixed state cannot be established (every card already partitioned, or the mode switch
#              does not converge).
# Inputs:      All real, nothing mocked — the mixed card population, the two token pools and the
#              placement are the verification. The case puts the cards named by MIG_MIXED_INDEXES
#              (default: card MIG_GPU_INDEX alone) into the partitioning mode if they are not already,
#              leaves every other card whole, and restarts the Device Manager to re-detect. Each round
#              submits ONE partition Pod (the discovered 4-memory-slice profile) and ONE logical-slice
#              Pod (20% memory / 20% cores) at the same time through the pool's entrance LocalQueue.
#              Profiles are DISCOVERED from the cards' own capability, never composed.
# Expected:    - the node advertises BOTH families at once: a partition token pool and per-profile
#                keys from the partitioned cards, a logical token pool and .sliced counting keys from
#                the whole ones;
#              - in every round the partition Pod reaches Running on a card in the PARTITIONED set;
#              - in every round the logical Pod reaches Running on a card in the UNPARTITIONED set;
#              - no test Pod in any round records a single UnexpectedAdmissionError.
# Cleanup:     Trap deletes every test Pod, waits for the live instances to reclaim, restores the
#              partitioning mode of exactly the cards this case toggled (and no others), and refreshes
#              the Device Manager so the ledger realigns. Idempotent; runs on pass AND fail.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: MIG_NODE_SSH=<user@host> case-24.sh <NS>}"
CASE_ID=24
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_partition-lib.sh"

PODPFX=gpustack-e2e-mixed
ROUNDS="${MIG_MIXED_ROUNDS:-3}"

part_require_node_ssh "case-24.sh"
part_require_mig_capable
part_discover

# --- The card population. A mixed node needs at least two cards: one to partition, one to leave whole.
ALL_INDEXES="$(card_indexes)"
NCARDS="$(echo "$ALL_INDEXES" | wc -w | tr -d '[:space:]')"
if [ "${NCARDS:-0}" -lt 2 ]; then
  part_skip \
    "Node ${GPU_NODE} reports ${NCARDS:-0} nvidia card(s); a mixed node needs at least two — one" \
    "partitioned and one left whole. Run this case on a multi-card partition-capable node."
fi
MIXED_INDEXES="${MIG_MIXED_INDEXES:-$GPU_INDEX}"
echo "[case-24] node cards: ${ALL_INDEXES} (${NCARDS}); partitioning: ${MIXED_INDEXES}"

TOGGLED=""
UAE_TOTAL=0
UAE_UNREAD=0

restore() {
  echo
  echo "[case-24] cleanup: deleting test Pods and restoring the partitioning mode of card(s) '${TOGGLED:-<none>}'"
  delete_test_pods
  if [ -n "$TOGGLED" ]; then
    wait_card_idle || true
    local i
    for i in $TOGGLED; do set_mig_mode "$i" 0 || true; done
    refresh_dm
  fi
}
trap restore EXIT

# ---------------------------------------------------------------------------------------------------
# Establish the mixed state: partition the named cards, leave every other card whole.
# ---------------------------------------------------------------------------------------------------
echo
echo "[case-24] === establishing the mixed card population ==="
for idx in $MIXED_INDEXES; do
  cur="$(mig_mode "$idx")"
  case "$cur" in
    Enabled)
      echo "[case-24]   card ${idx} already partitioned — left as found (not restored by this case)" ;;
    Disabled)
      if set_mig_mode "$idx" 1; then
        TOGGLED="${TOGGLED}${idx} "
      else
        record FAIL "mixed card population established" "card ${idx}: the partitioning mode did not converge to Enabled (a pending GPU reset, a busy card, or a loaded nvidia_drm blocks it) — drain the card and retry"
        part_results "Mixed node: a partition lands on a partitioned card, a logical slice on a whole one"
        exit 1
      fi ;;
    *)
      record FAIL "mixed card population established" "card ${idx} reports no partitioning mode ('${cur:-<none>}') — MIG_MIXED_INDEXES names a card that cannot be partitioned"
      part_results "Mixed node: a partition lands on a partitioned card, a logical slice on a whole one"
      exit 1 ;;
  esac
done
refresh_dm
KEYS="$(wait_partition_keys)"

PARTSET="$(pad_set $(partitioned_cards))"
PLAINSET="$(pad_set $(unpartitioned_cards))"
NPART="$(echo "$PARTSET" | wc -w | tr -d '[:space:]')"
NPLAIN="$(echo "$PLAINSET" | wc -w | tr -d '[:space:]')"
echo "[case-24] partitioned cards (${NPART}):${PARTSET}"
echo "[case-24] whole cards (${NPLAIN}):${PLAINSET}"
if [ "${NPART:-0}" -lt 1 ] || [ "${NPLAIN:-0}" -lt 1 ]; then
  part_skip \
    "The node is not mixed after the toggle: ${NPART:-0} partitioned card(s), ${NPLAIN:-0} whole card(s)." \
    "This case needs at least one of each on ONE node. Check the Device Manager re-detect and retry."
fi
record PASS "mixed card population established" "${NPART} partitioned card(s) + ${NPLAIN} whole card(s) on ${GPU_NODE}"

read -r SMALL SMALL_CNT MID MID_CNT FULL FULL_CNT NCARD NTOT <<<"$(card_profiles)"
echo "[case-24] discovered profiles: SMALL=${SMALL}(${SMALL_CNT}/card) MID=${MID}(${MID_CNT}/card) FULL=${FULL}(${FULL_CNT}/card)"
MIDKEY="$(profile_key "$MID")"

# ---------------------------------------------------------------------------------------------------
# Both families are advertised at once — that is what makes the node mixed to the scheduler.
# ---------------------------------------------------------------------------------------------------
ptok="$(node_key "$PARTITIONED")"
stok="$(node_key "$SLICED")"
sunits="$(node_key "${SLICED}.units")"
punits="$(node_key "${PARTITIONED}.units")"
if [ -n "$MIDKEY" ] && [ -n "${ptok:-}" ] && [ "${ptok:-0}" != 0 ] && [ -n "${stok:-}" ] && [ "${stok:-0}" != 0 ]; then
  record PASS "both families advertised on one node" "${PARTITIONED}=${ptok} (+${PARTITIONED}.units=${punits:-<absent>}, per-profile keys: ${KEYS:-<none>}), ${SLICED}=${stok} (+${SLICED}.units=${sunits:-<absent>})"
else
  record FAIL "both families advertised on one node" "${PARTITIONED}='${ptok:-<absent>}', ${SLICED}='${stok:-<absent>}', per-profile keys='${KEYS:-<none>}' — a mixed node must advertise a non-zero token pool for BOTH families"
fi
if [ -z "$MIDKEY" ]; then
  record FAIL "partition profile key discovered" "no 4-memory-slice profile key advertised (MID='${MID:-<none>}') — cannot submit a partition request"
  part_results "Mixed node: a partition lands on a partitioned card, a logical slice on a whole one"
  exit 1
fi

# ---------------------------------------------------------------------------------------------------
# The rounds. Each submits one request of each family AT THE SAME TIME and requires both to land on a
# card of the right population. Repetition is the point: a single correct placement could be luck.
# ---------------------------------------------------------------------------------------------------
for r in $(seq 1 "$ROUNDS"); do
  echo
  echo "[case-24] === round ${r}/${ROUNDS}: one partition request + one logical-slice request ==="
  PP="${PODPFX}-part-${r}"
  LP="${PODPFX}-logical-${r}"
  mkpod "$PP" "$(partition_reslines "$MIDKEY")"
  mkpod "$LP" "$(sliced_reslines 20)"

  prun=1; wait_running "$PP" || prun=0
  lrun=1; wait_running "$LP" || lrun=0
  pcards="$(wait_pod_cards "$PP")"
  lcards="$(wait_pod_cards "$LP")"

  # The partition request must be on a card that can host a partition.
  if [ "$prun" = 1 ] && [ -n "$pcards" ]; then
    bad=""
    for c in $pcards; do in_set "$c" "$PARTSET" || bad="$c"; done
    if [ -n "$bad" ]; then
      record FAIL "round ${r}: partition lands on a partitioned card" "${PP} Running but attributed card ${bad}, which is NOT in the partitioned set${PARTSET}— a partition was placed on a card that cannot host one"
    else
      pprof="$(pod_profiles "$PP")"
      record PASS "round ${r}: partition lands on a partitioned card" "${PP} Running on ${pcards} (profile recorded: ${pprof:-<none>}), inside the partitioned set"
    fi
  else
    record FAIL "round ${r}: partition lands on a partitioned card" "${PP} running=${prun}, allocated card(s)='${pcards:-<none>}', held reason='$(held_reason "$PP")' — the partition request did not run on the mixed node"
  fi

  # The logical-slice request must be on a card that is NOT partitioned.
  if [ "$lrun" = 1 ] && [ -n "$lcards" ]; then
    bad=""
    for c in $lcards; do in_set "$c" "$PLAINSET" || bad="$c"; done
    if [ -n "$bad" ]; then
      record FAIL "round ${r}: logical slice lands on a whole card" "${LP} Running but attributed card ${bad}, which is PARTITIONED — a logical slice was placed on a card that serves no logical slicing"
    else
      record PASS "round ${r}: logical slice lands on a whole card" "${LP} Running on ${lcards}, inside the whole-card set"
    fi
  else
    record FAIL "round ${r}: logical slice lands on a whole card" "${LP} running=${lrun}, allocated card(s)='${lcards:-<none>}', held reason='$(held_reason "$LP")' — the logical request did not run on the mixed node"
  fi

  # The headline number: no terminal device-plugin admission failure, in either family. A Pod whose
  # events cannot be read is counted as unread rather than as zero — the difference between "it did
  # not happen" and "we could not tell" is the whole value of this check.
  for p in "$PP" "$LP"; do
    if ! n="$(pod_unexpected_admission "$p")"; then
      UAE_UNREAD=$((UAE_UNREAD + 1))
      continue
    fi
    [ -n "$n" ] && UAE_TOTAL=$((UAE_TOTAL + n))
  done

  delpods "$PP" "$LP"
  wait_card_idle || echo "[case-24]   warning: a live instance is still present entering the next round"
done

if [ "$UAE_UNREAD" != 0 ]; then
  record FAIL "zero UnexpectedAdmissionError across all rounds" "${UAE_TOTAL} event(s) counted, but ${UAE_UNREAD} Pod('s) events could not be read at all — this check is UNVERIFIED, which is not the same as passed"
elif [ "$UAE_TOTAL" = 0 ]; then
  record PASS "zero UnexpectedAdmissionError across all rounds" "${ROUNDS} round(s) × 2 Pods, 0 UnexpectedAdmissionError events"
else
  record FAIL "zero UnexpectedAdmissionError across all rounds" "${UAE_TOTAL} UnexpectedAdmissionError event(s) over ${ROUNDS} round(s) — a request was offered a token from the wrong card population"
fi

part_results "Mixed node: a partition lands on a partitioned card, a logical slice on a whole one"

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). On a mixed node each family's token pool is advertised ONLY by the cards"
  echo "that can serve it, and the partition family's Allocate chooses the card itself — so a partition can"
  echo "never be placed on a whole card, nor a logical slice on a partitioned one. Diagnose:"
  echo "  kubectl get devices ${GPU_NODE} -o yaml   # per-card physicalSliced/logicalSliced capability"
  echo "  kubectl get node ${GPU_NODE} -o json | jq '.status.allocatable | with_entries(select(.key|test(\"sliced|partitioned\")))'"
  echo "  kubectl -n default get pod <pod> -o jsonpath='{.metadata.annotations.${ANNO}}'"
  echo "  kubectl -n ${NS} logs ds/${DM_DS} --tail=200"
  exit 1
fi
echo "CASE 24 PASS"
