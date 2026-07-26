#!/usr/bin/env bash
#
# CASE 27 — A partitioned card is never judged feasible for an exclusive or shared claim
#   (MUTATING, self-recovering; AUTO-SKIPS without a partition-capable node AND a node-SSH address)
#
#   MIG_NODE_SSH=<user@host> case-27.sh <NS>
#
# Goal:        A card put into a hardware partitioning mode leaves the whole-card and shared
#              populations entirely: it advertises no whole-card token and no ownership share, and the
#              pool's EX and SH views stop counting it. The failure this prevents is quiet and
#              permanent — a card whose scalar "remaining" still looks like a free whole card would
#              make an exclusive Pod pass the feasibility check, be admitted, and then sit Pending
#              forever, because CUDA cannot use a card in a partitioning mode. So the contract is that
#              an exclusive claim with no unpartitioned card left is HELD (queued or refused), never
#              admitted onto a partitioned one.
# Environment: A reachable cluster whose active context is the GPU cluster, an nvidia node with at
#              least TWO cards where at least one can be put into a hardware partitioning mode, AND SSH
#              to that node (sudo nvidia-smi) supplied via MIG_NODE_SSH=<user@host>. It EXITS 2 (input
#              required) when MIG_NODE_SSH is unset, and AUTO-SKIPS (exit 0) when the card reports no
#              partitioning mode, when the node has fewer than two cards, or when the mode switch does
#              not converge. The exclusive-fill sub-check additionally SKIPS when the node has more
#              whole cards than MIG_MAX_FILL (default 8) — filling them is the only way to reach the
#              state it asserts.
# Inputs:      All real, nothing mocked — the node's own keys, the pool's views and the placement are
#              the verification. The case reads the whole-card and shared keys with every card
#              unpartitioned, puts card MIG_GPU_INDEX into the partitioning mode, and reads them again;
#              the delta is the measurement. It then fills every remaining whole card with an exclusive
#              Pod through the pool's entrance LocalQueue and submits one more.
# Expected:    - partitioning one card removes exactly one card's worth of whole-card tokens and one
#                card's worth of ownership shares from the node;
#              - the pool's EX and SH views drop by the same one card, and its PT view becomes non-zero;
#              - with every remaining whole card held exclusively, a further exclusive claim is HELD
#                with a concrete signal — never Running, and never admitted into an endless Pending.
# Cleanup:     Trap deletes every test Pod, waits for the cards to free, and restores the partitioning
#              mode of the card this case toggled (a card found already partitioned is left as found).
#              Idempotent; runs on pass AND fail.
set -uo pipefail

NS="${1:?usage: MIG_NODE_SSH=<user@host> case-27.sh <NS>}"
CASE_ID=27
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_partition-lib.sh"

PODPFX=gpustack-e2e-exzero
MAX_FILL="${MIG_MAX_FILL:-8}"

part_require_node_ssh "case-27.sh"
part_require_mig_capable
part_discover

restore() {
  echo
  echo "[case-27] cleanup: deleting test Pods and restoring card(s) '${TOGGLED:-<none>}'"
  delete_test_pods
  part_restore_toggled
}
trap restore EXIT

NTOT_CARDS="$(card_indexes | wc -w | tr -d '[:space:]')"
if [ "${NTOT_CARDS:-0}" -lt 2 ]; then
  part_skip \
    "Node ${GPU_NODE} reports ${NTOT_CARDS:-0} nvidia card(s). This case partitions one card and then" \
    "exercises the whole-card population that is left, so it needs at least two."
fi
if [ -n "$(partitioned_cards)" ]; then
  part_skip \
    "Node ${GPU_NODE} already has a partitioned card, so the before/after delta this case measures is" \
    "not available. Disable the partitioning mode on every card (or run this case first) and re-run."
fi

# ---------------------------------------------------------------------------------------------------
# Before: every card is whole, so every card contributes to the whole-card and shared families.
# ---------------------------------------------------------------------------------------------------
echo
echo "[case-27] === before: every card unpartitioned ==="
BEF_EXCL="$(node_key "$EXCLUSIVE")"
BEF_SHARED="$(node_key "$SHARED")"
BEF_IT="$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.status.accelerator.capacity} {.status.acceleratorShared.capacity} {.status.acceleratorPartitioned.capacity}' 2>/dev/null)"
read -r BEF_EX_CAP BEF_SH_CAP BEF_PT_CAP <<<"$BEF_IT"
echo "[case-27]   node ${EXCLUSIVE}=${BEF_EXCL:-<absent>} ${SHARED}=${BEF_SHARED:-<absent>}; pool EX/SH/PT capacity=${BEF_EX_CAP:-?}/${BEF_SH_CAP:-?}/${BEF_PT_CAP:-?}"
if [ -z "${BEF_EXCL:-}" ] || [ "${BEF_EXCL:-0}" -lt 2 ] || [ -z "${BEF_SHARED:-}" ]; then
  part_skip \
    "The node does not advertise at least two whole cards (${EXCLUSIVE}='${BEF_EXCL:-<absent>}') plus their" \
    "shared companion (${SHARED}='${BEF_SHARED:-<absent>}'). This case needs both families live before the flip."
fi
SHARES_PER_CARD=$((BEF_SHARED / BEF_EXCL))

# ---------------------------------------------------------------------------------------------------
# Flip one card into the partitioning mode.
# ---------------------------------------------------------------------------------------------------
echo
echo "[case-27] === partitioning card ${GPU_INDEX} ==="
part_ensure_partitioned_card

WANT_EXCL=$((BEF_EXCL - 1))
WANT_SHARED=$((BEF_SHARED - SHARES_PER_CARD))
aft_excl=""; aft_shared=""
for _ in $(seq 1 30); do
  aft_excl="$(node_key "$EXCLUSIVE")"
  aft_shared="$(node_key "$SHARED")"
  [ "${aft_excl:-x}" = "$WANT_EXCL" ] && [ "${aft_shared:-x}" = "$WANT_SHARED" ] && break
  sleep 4
done
if [ "${aft_excl:-x}" = "$WANT_EXCL" ] && [ "${aft_shared:-x}" = "$WANT_SHARED" ]; then
  record PASS "a partitioned card leaves the whole-card and shared populations" "${EXCLUSIVE}: ${BEF_EXCL} → ${aft_excl}, ${SHARED}: ${BEF_SHARED} → ${aft_shared} (one card = ${SHARES_PER_CARD} shares)"
else
  record FAIL "a partitioned card leaves the whole-card and shared populations" "${EXCLUSIVE}='${aft_excl:-<absent>}' (want ${WANT_EXCL}), ${SHARED}='${aft_shared:-<absent>}' (want ${WANT_SHARED}) — a card in a partitioning mode must advertise neither family"
fi

# The pool's own views must agree — they are the data source the Instance webhook rejects against.
aft_ex_cap=""; aft_sh_cap=""; aft_pt_cap=""
for _ in $(seq 1 20); do
  read -r aft_ex_cap aft_sh_cap aft_pt_cap <<<"$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.status.accelerator.capacity} {.status.acceleratorShared.capacity} {.status.acceleratorPartitioned.capacity}' 2>/dev/null)"
  [ "${aft_ex_cap:-x}" = "$WANT_EXCL" ] && [ "${aft_pt_cap:-0}" != 0 ] && break
  sleep 4
done
if [ "${aft_ex_cap:-x}" = "$WANT_EXCL" ] && [ "${aft_sh_cap:-x}" = "$WANT_SHARED" ] && [ -n "${aft_pt_cap:-}" ] && [ "${aft_pt_cap}" != 0 ]; then
  record PASS "the pool's EX/SH views exclude the partitioned card" "EX ${BEF_EX_CAP:-?} → ${aft_ex_cap}, SH ${BEF_SH_CAP:-?} → ${aft_sh_cap}, PT ${BEF_PT_CAP:-?} → ${aft_pt_cap}"
else
  record FAIL "the pool's EX/SH views exclude the partitioned card" "EX='${aft_ex_cap:-?}' (want ${WANT_EXCL}), SH='${aft_sh_cap:-?}' (want ${WANT_SHARED}), PT='${aft_pt_cap:-?}' (want > 0) — an EX/SH view that still counts the partitioned card admits a Pod that can never run"
fi

# ---------------------------------------------------------------------------------------------------
# Fill every remaining whole card exclusively, then prove the partitioned card attracts nothing.
# ---------------------------------------------------------------------------------------------------
echo
echo "[case-27] === with every whole card held, an exclusive claim must be held ==="
if [ "${WANT_EXCL}" -gt "$MAX_FILL" ]; then
  record SKIP "an exclusive claim is held rather than placed on a partitioned card" "${WANT_EXCL} whole cards left, above MIG_MAX_FILL=${MAX_FILL} — filling them all is the only way to reach the state; raise MIG_MAX_FILL to run it"
else
  PARTSET="$(pad_set $(partitioned_cards))"
  fillok=1
  for i in $(seq 1 "$WANT_EXCL"); do
    p="${PODPFX}-excl-${i}"
    mkpod "$p" "          ${EXCLUSIVE}: \"1\""
    if ! wait_running "$p"; then
      record FAIL "an exclusive claim is held rather than placed on a partitioned card" "${p} did not reach Running (held: $(held_reason "$p")) — cannot fill the whole cards"
      fillok=0
      break
    fi
    c="$(wait_pod_cards "$p")"
    for cc in $c; do
      if in_set "$cc" "$PARTSET"; then
        record FAIL "an exclusive claim is held rather than placed on a partitioned card" "${p} was attributed card ${cc}, which is PARTITIONED — an exclusive claim must never be given a partitioned card"
        fillok=0
      fi
    done
    [ "$fillok" = 1 ] || break
  done
  if [ "$fillok" = 1 ]; then
    record PASS "every whole card is held exclusively" "${WANT_EXCL} exclusive Pod(s) Running, none on the partitioned card"
    OVER="${PODPFX}-excl-over"
    mkpod "$OVER" "          ${EXCLUSIVE}: \"1\""
    hr="$(assert_held "$OVER")"
    if [ -n "$hr" ]; then
      record PASS "an exclusive claim is held rather than placed on a partitioned card" "${OVER} held [${hr}] with a partitioned card present and idle — the partitioned card was not judged feasible"
    else
      record FAIL "an exclusive claim is held rather than placed on a partitioned card" "${OVER} shows no concrete held signal (phase=$(phase "$OVER"), cards='$(pod_cards "$OVER")') — an idle partitioned card must not make an exclusive request look feasible"
    fi
  fi
fi

part_results "A partitioned card is never judged feasible for an exclusive or shared claim"

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). A card in a hardware partitioning mode advertises no whole-card token and no"
  echo "ownership share, and the pool's EX/SH views must not count it — otherwise an exclusive tenant is"
  echo "admitted onto a card CUDA cannot use and stays Pending forever. Diagnose:"
  echo "  kubectl get node ${GPU_NODE} -o json | jq '.status.allocatable | with_entries(select(.key|test(\"gpu\")))'"
  echo "  kubectl get instancetypes.worker.gpustack.ai ${IT} -o json | jq '.status | {accelerator, acceleratorShared, acceleratorSliced, acceleratorPartitioned}'"
  echo "  kubectl get devices ${GPU_NODE} -o yaml"
  exit 1
fi
echo "CASE 27 PASS"
