#!/usr/bin/env bash
#
# CASE 9 — Real NVIDIA MIG hardware lifecycle: enable, carve, mixed node, live capacity ledger, concurrent multi-profile, reclaim and disable   (MUTATING, self-recovering; AUTO-SKIPS without a MIG-capable NVIDIA card AND a node-SSH address)
#
#   MIG_NODE_SSH=<user@host> case-9.sh <NS>
#
# Prerequisites: nodes=1; gpu=nvidia; features=mig,partition; cluster=real; inputs=MIG_MAX_FILL,MIG_MIXED_INDEXES,MIG_MIXED_ROUNDS,MIG_NODE_SSH,MIG_RECLAIM_BOUND,MIG_WRITE_IDLE_WINDOW
# Goal:        Proves the full NVIDIA MIG hardware lifecycle end to end on real hardware, in one
#              self-recovering sequence:
#                1. Logical slicing (when the card starts MIG-off): 20% and 40% slices coexist and
#                   carry their CUDA_DEVICE_SM_LIMIT caps;
#                2. Exclusive/shared exclusion delta (on a >=2-card node starting unpartitioned):
#                   partitioning one card removes 1 card from whole-card/shared node keys and pool
#                   EX/SH views, and filling remaining whole cards holds a further exclusive claim;
#                3. Enable, carve & family swap: canonical profiles carve (MID=2/card, FULL=1/card),
#                   per-profile keys and .partitioned.units appear, logical keys leave when all cards
#                   are partitioned, and InstanceType four-view reflects MIG geometry;
#                4. Mixed node placement (when >=1 partitioned card and >=1 whole card coexist): both
#                   families are advertised at once, every partition Pod lands in PARTSET, every
#                   logical Pod lands in PLAINSET, with 0 UnexpectedAdmissionError across rounds;
#                5. Live capacity ledger & saturation: idle per-profile keys match geometry, Node
#                   write volume is measured, carving MID drops FULL while MID stays allocated +
#                   remaining, single-card FULL Pod is held, mutually exclusive FULL+MID never both
#                   run on one card, saturating all partitioned cards sets allocatable == live count
#                   while capacity survives, extra SMALL is held, running Pods are unaffected, and
#                   freeing one restores the count and admits a refill without restart;
#                6. Per-card mutual exclusion, reuse, idle reclaim & small-then-large recompose;
#                7. Concurrent multi-profile resolution (MID + SMALL simultaneously: hardware device
#                   list + durable pod record + 0 UAE), reclaim-window immediate replacement
#                   convergence within MIG_RECLAIM_BOUND, and terminated init-container charge;
#                8. Disable MIG and re-detect: logical family returns and partition keys disappear.
# Environment: A reachable cluster whose active context is the GPU cluster, a node with a real
#              MIG-capable NVIDIA card, AND SSH to that node (sudo nvidia-smi) supplied via
#              MIG_NODE_SSH=<user@host>. EXITS 2 (input required) when MIG_NODE_SSH is unset.
#              AUTO-SKIPS (exit 0) when the card is not MIG-capable or no nvidia Devices exist.
#              Topology-dependent sub-phases (multi-card exclusion, mixed node, single-card mutual
#              exclusion) record SKIP cleanly when the node's card count does not apply.
# Inputs:      All real, nothing mocked — MIG geometry, libvgpu cap, GI/CI create/destroy, node
#              resource keys, and per-Pod allocation annotations are verified live.
# Expected:    Every applicable phase records PASS; non-applicable topology sub-checks record SKIP.
# Cleanup:     Trap stops any active node write probe, deletes every test Pod, waits for live GI
#              reclaim, restores the partitioning mode of GPU_INDEX and any toggled cards to the
#              initial state (unless MIG_KEEP_MODE=1), refreshes the Device Manager, and closes the
#              SSH control master. Idempotent; runs on pass AND fail.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

NS="${1:?usage: MIG_NODE_SSH=<user@host> case-9.sh <NS>}"
# shellcheck disable=SC2034 # Read by _partition-lib.sh.
CASE_ID=9
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_partition-lib.sh"

PODPFX=e2e-c9-mig
ROUNDS="${MIG_MIXED_ROUNDS:-3}"
IDLE_WINDOW="${MIG_WRITE_IDLE_WINDOW:-60}"
MAX_FILL="${MIG_MAX_FILL:-8}"
BOUND="${MIG_RECLAIM_BOUND:-300}"

part_require_node_ssh "case-9.sh"
part_require_mig_capable
part_discover

TOGGLED=""
restore() {
  echo
  echo "[case-9] cleanup: deleting test Pods, stopping write probe and restoring MIG mode to '${INITIAL_MODE}'"
  stop_node_write_probe
  [ -n "${NODE_WRITE_LOG:-}" ] && rm -f "$NODE_WRITE_LOG"
  delete_test_pods
  local restored=0
  if [ -n "${TOGGLED:-}" ] && [ "${MIG_KEEP_MODE:-0}" != 1 ]; then
    wait_card_idle || true
    local i
    for i in $TOGGLED; do set_mig_mode "$i" 0 || true; done
    restored=1
  fi
  local now
  now="$(mig_mode "$GPU_INDEX")"
  if [ "$now" != "$INITIAL_MODE" ] && [ "${MIG_KEEP_MODE:-0}" != 1 ]; then
    [ "$INITIAL_MODE" = Enabled ] || wait_card_idle || true
    if [ "$INITIAL_MODE" = Enabled ]; then
      set_mig_mode "$GPU_INDEX" 1 || set_mig_mode "$GPU_INDEX" 0
    else
      set_mig_mode "$GPU_INDEX" 0
    fi
    restored=1
  fi
  [ "$restored" = 1 ] && refresh_dm
  node_ssh_close
}
trap restore EXIT

# ===================================================================================================
# Phase 1 — Logical slicing when the card starts MIG OFF.
# ===================================================================================================
if [ "$INITIAL_MODE" = Disabled ]; then
  echo
  echo "[case-9] === phase 1: logical slicing (MIG off) — 20% and 40% coexist ==="
  P20="${PODPFX}-logical20"; P40="${PODPFX}-logical40"
  mkpod "$P20" "$(sliced_reslines 20)"
  mkpod "$P40" "$(sliced_reslines 40)"
  l20=1; wait_running "$P20" || l20=0
  l40=1; wait_running "$P40" || l40=0
  if [ "$l20" = 1 ] && [ "$l40" = 1 ]; then
    sm20="$(exec_out "$P20" printenv CUDA_DEVICE_SM_LIMIT)"
    sm40="$(exec_out "$P40" printenv CUDA_DEVICE_SM_LIMIT)"
    if [ "$sm20" = 20 ] && [ "$sm40" = 40 ]; then
      record PASS "logical 20% + 40% coexist and are capped" "${P20}(SM=${sm20}) + ${P40}(SM=${sm40}) both Running"
    else
      record FAIL "logical 20% + 40% coexist and are capped" "${P20} SM='${sm20:-?}' (want 20), ${P40} SM='${sm40:-?}' (want 40)"
    fi
  else
    record FAIL "logical 20% + 40% coexist and are capped" "20% running=${l20} 40% running=${l40}"
  fi
  delpods "$P20" "$P40"
else
  record SKIP "logical-slice path (card started MIG-on)" "card ${GPU_INDEX} was already MIG-enabled; logical return is verified in Phase 8"
fi

# ===================================================================================================
# Phase 2 — Exclusive/shared exclusion delta (multi-card node starting all-unpartitioned).
# ===================================================================================================
echo
echo "[case-9] === phase 2: partitioned card leaves whole-card and shared populations ==="
ALL_INDEXES="$(card_indexes)"
NTOT_CARDS="$(echo "$ALL_INDEXES" | wc -w | tr -d '[:space:]')"
if [ "${NTOT_CARDS:-0}" -lt 2 ]; then
  record SKIP "phase 2: exclusive/shared exclusion delta" "node ${GPU_NODE} reports ${NTOT_CARDS:-0} card(s); needs >=2"
elif [ -n "$(partitioned_cards)" ]; then
  record SKIP "phase 2: exclusive/shared exclusion delta" "node ${GPU_NODE} already has a partitioned card; before/after delta unavailable"
else
  BEF_EXCL="$(node_key "$EXCLUSIVE")"
  BEF_SHARED="$(node_key "$SHARED")"
  BEF_IT="$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.status.accelerator.capacity} {.status.acceleratorShared.capacity} {.status.acceleratorPartitioned.capacity}' 2>/dev/null)"
  read -r BEF_EX_CAP BEF_SH_CAP BEF_PT_CAP <<<"$BEF_IT"
  if [ -z "${BEF_EXCL:-}" ] || [ "${BEF_EXCL:-0}" -lt 2 ] || [ -z "${BEF_SHARED:-}" ]; then
    record SKIP "phase 2: exclusive/shared exclusion delta" "${EXCLUSIVE}='${BEF_EXCL:-<absent>}' ${SHARED}='${BEF_SHARED:-<absent>}'"
  else
    SHARES_PER_CARD=$((BEF_SHARED / BEF_EXCL))
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
      record PASS "a partitioned card leaves the whole-card and shared populations" "${EXCLUSIVE}: ${BEF_EXCL} -> ${aft_excl}, ${SHARED}: ${BEF_SHARED} -> ${aft_shared}"
    else
      record FAIL "a partitioned card leaves the whole-card and shared populations" "${EXCLUSIVE}='${aft_excl:-<absent>}' (want ${WANT_EXCL}), ${SHARED}='${aft_shared:-<absent>}' (want ${WANT_SHARED})"
    fi

    WANT_POOL_EX=$((${BEF_EX_CAP:-0} - 1))
    WANT_POOL_SH=$((${BEF_SH_CAP:-0} - SHARES_PER_CARD))
    aft_ex_cap=""; aft_sh_cap=""; aft_pt_cap=""
    for _ in $(seq 1 20); do
      read -r aft_ex_cap aft_sh_cap aft_pt_cap <<<"$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.status.accelerator.capacity} {.status.acceleratorShared.capacity} {.status.acceleratorPartitioned.capacity}' 2>/dev/null)"
      [ "${aft_ex_cap:-x}" = "$WANT_POOL_EX" ] && [ "${aft_pt_cap:-0}" != 0 ] && break
      sleep 4
    done
    if [ "${aft_ex_cap:-x}" = "$WANT_POOL_EX" ] && [ "${aft_sh_cap:-x}" = "$WANT_POOL_SH" ] && [ -n "${aft_pt_cap:-}" ] && [ "${aft_pt_cap}" != 0 ]; then
      record PASS "the pool's EX/SH views exclude the partitioned card" "EX ${BEF_EX_CAP:-?} -> ${aft_ex_cap}, SH ${BEF_SH_CAP:-?} -> ${aft_sh_cap}, PT ${BEF_PT_CAP:-?} -> ${aft_pt_cap}"
    else
      record FAIL "the pool's EX/SH views exclude the partitioned card" "EX='${aft_ex_cap:-?}' (want ${WANT_POOL_EX}), SH='${aft_sh_cap:-?}' (want ${WANT_POOL_SH}), PT='${aft_pt_cap:-?}'"
    fi

    if [ "${WANT_EXCL}" -gt "$MAX_FILL" ]; then
      record SKIP "an exclusive claim is held rather than placed on a partitioned card" "${WANT_EXCL} whole cards left, above MIG_MAX_FILL=${MAX_FILL}"
    else
      # shellcheck disable=SC2046 # partitioned_cards returns space-separated tokens.
      PARTSET_P2="$(pad_set $(partitioned_cards))"
      fillok=1
      P2_PODS=()
      for i in $(seq 1 "$WANT_EXCL"); do
        p="${PODPFX}-excl-${i}"
        P2_PODS+=("$p")
        mkpod "$p" "          ${EXCLUSIVE}: \"1\""
        if ! wait_running "$p"; then
          record FAIL "an exclusive claim is held rather than placed on a partitioned card" "${p} did not reach Running (held: $(held_reason "$p"))"
          fillok=0
          break
        fi
        c="$(wait_pod_cards "$p")"
        for cc in $c; do
          if in_set "$cc" "$PARTSET_P2"; then
            record FAIL "an exclusive claim is held rather than placed on a partitioned card" "${p} was attributed partitioned card ${cc}"
            fillok=0
          fi
        done
        [ "$fillok" = 1 ] || break
      done
      if [ "$fillok" = 1 ]; then
        record PASS "every whole card is held exclusively" "${WANT_EXCL} exclusive Pod(s) Running, none on the partitioned card"
        OVER="${PODPFX}-excl-over"
        P2_PODS+=("$OVER")
        mkpod "$OVER" "          ${EXCLUSIVE}: \"1\""
        hr="$(assert_held "$OVER")"
        if [ -n "$hr" ]; then
          record PASS "an exclusive claim is held rather than placed on a partitioned card" "${OVER} held [${hr}] with a partitioned card present and idle"
        else
          record FAIL "an exclusive claim is held rather than placed on a partitioned card" "${OVER} shows no concrete held signal (phase=$(phase "$OVER"))"
        fi
      fi
      delpods "${P2_PODS[@]}"
    fi
  fi
fi

# ===================================================================================================
# Phase 3 — Enable MIG (if not already), carve profiles, capability keys & InstanceType views.
# ===================================================================================================
echo
echo "[case-9] === phase 3: enable MIG, profile carve, capability keys & InstanceType views ==="
CUR="$(mig_mode "$GPU_INDEX")"
if [ "$CUR" != Enabled ]; then
  if ! set_mig_mode "$GPU_INDEX" 1; then
    record FAIL "enable MIG mode" "nvidia-smi -mig 1 did not converge to Enabled"
    part_results "Real NVIDIA MIG hardware lifecycle"
    exit 1
  fi
  refresh_dm
fi
KEYS="$(wait_partition_keys)"
read -r SMALL SMALL_CNT MID MID_CNT FULL FULL_CNT NCARD NTOT <<<"$(card_profiles)"
echo "[case-9] discovered profiles: SMALL=${SMALL}(${SMALL_CNT}/card) MID=${MID}(${MID_CNT}/card) FULL=${FULL}(${FULL_CNT}/card); partitioned cards N=${NCARD:-0} of ${NTOT:-0}"

if [ -n "$MID" ] && [ -n "$FULL" ] && [ "${MID_CNT:-0}" = 2 ] && [ "${FULL_CNT:-0}" = 1 ]; then
  record PASS "canonical profiles carved (names/counts)" "MID ${MID}=2/card, FULL ${FULL}=1/card, SMALL ${SMALL}=${SMALL_CNT}/card"
else
  record FAIL "canonical profiles carved (names/counts)" "MID=${MID}(${MID_CNT}) FULL=${FULL}(${FULL_CNT}) SMALL=${SMALL}(${SMALL_CNT})"
fi

MIGKEY_MID="$(profile_key "$MID")"
MIGKEY_FULL="$(profile_key "$FULL")"
MIGKEY_SMALL="$(profile_key "$SMALL")"
mid_key_cap="$(node_key "$MIGKEY_MID")"
full_key_cap="$(node_key "$MIGKEY_FULL")"
if [ -n "$MIGKEY_MID" ] && [ -n "$MIGKEY_FULL" ] && [ -n "$mid_key_cap" ] && [ -n "$full_key_cap" ]; then
  record PASS "node advertises per-profile partition keys" "${MIGKEY_MID}=${mid_key_cap}, ${MIGKEY_FULL}=${full_key_cap}"
else
  record FAIL "node advertises per-profile partition keys" "MID='${MIGKEY_MID:-<absent>}'=${mid_key_cap:-<absent>}, FULL='${MIGKEY_FULL:-<absent>}'=${full_key_cap:-<absent>}"
fi

punits="$(node_key "${PARTITIONED}.units")"
if [ -n "$punits" ]; then
  record PASS "node advertises the partition units key" "${PARTITIONED}.units=${punits}"
else
  record FAIL "node advertises the partition units key" "${PARTITIONED}.units absent"
fi

logicalpct="$(node_key "${SLICED}.memory-percentage")"
units="$(node_key "${SLICED}.units")"
if [ "${NCARD:-0}" != "${NTOT:-0}" ] || [ "${NTOT:-0}" = 0 ]; then
  record SKIP "logical family leaves the MIG card" "only ${NCARD:-0} of ${NTOT:-0} card(s) partitioned — unpartitioned sibling(s) keep logical keys alive"
elif [ -z "$logicalpct" ] && [ -z "$units" ] && node_key_gone "${SLICED}"; then
  record PASS "logical family leaves the MIG card" "${SLICED}.memory-percentage and ${SLICED}.units removed, ${SLICED} pool drained"
else
  record FAIL "logical family leaves the MIG card" "${SLICED}.memory-percentage='${logicalpct:-<absent>}', ${SLICED}.units='${units:-<absent>}', ${SLICED}='$(node_key "${SLICED}")'"
fi

read -r POOL_PART POOL_TOT <<<"$(kubectl get devices.v1alpha1.worker.gpustack.ai -o json 2>/dev/null | GRP="${GROUPID}" python3 -c "
import json, os, sys
grp = os.environ['GRP']
tot = part = 0
for d in json.load(sys.stdin).get('items', []):
    for g in d.get('spec', {}).get('groups', []) or []:
        if g.get('id') != grp: continue
        for a in g.get('accelerators', []) or []:
            tot += 1
            if ((a.get('status') or {}).get('physicalSliced') or {}).get('count', 0) > 0:
                part += 1
print(part, tot)
" 2>/dev/null)"
it_ok="$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o json 2>/dev/null | MID="$MID" FULL="$FULL" NCARD="${POOL_PART:-1}" ALLMIG="$([ "${POOL_PART:-0}" = "${POOL_TOT:-0}" ] && [ "${POOL_TOT:-0}" != 0 ] && echo 1 || echo 0)" python3 -c "
import json,sys,os
it=json.load(sys.stdin); st=it.get('status',{})
det=st.get('detail',{}).get('slicedDetail',{}).get('physical',{})
profs={p['name']:p.get('count',0) for p in det.get('profiles',[]) or []}
mid=os.environ['MID']; full=os.environ['FULL']; n=int(os.environ['NCARD'] or 1)
allmig=os.environ['ALLMIG']=='1'
sliced=st.get('acceleratorSliced',{}); part=st.get('acceleratorPartitioned',{})
scap=int(sliced.get('capacity',0) or 0); pcap=int(part.get('capacity',0) or 0)
ok = profs.get(mid)==2*n and profs.get(full)==1*n and pcap>0 and (scap==0 or not allmig)
print('OK' if ok else 'BAD', 'mid=%s full=%s ptCap=%s slCap=%s allPartitioned=%s' % (profs.get(mid), profs.get(full), pcap, scap, allmig))
" 2>/dev/null)"
if [[ "$it_ok" == OK* ]]; then
  record PASS "InstanceType four-view reflects MIG geometry" "${it_ok#OK } over ${POOL_PART:-0}/${POOL_TOT:-0} partitioned card(s)"
else
  record FAIL "InstanceType four-view reflects MIG geometry" "status detail mismatch [${it_ok:-<empty>}]"
fi

# ===================================================================================================
# Phase 4 — Mixed node placement (partition lands on partitioned card, logical slice on whole card).
# ===================================================================================================
echo
echo "[case-9] === phase 4: mixed node placement ==="
# shellcheck disable=SC2046 # partitioned_cards and unpartitioned_cards return space-separated tokens.
PARTSET="$(pad_set $(partitioned_cards))"
# shellcheck disable=SC2046 # unpartitioned_cards returns space-separated tokens.
PLAINSET="$(pad_set $(unpartitioned_cards))"
NPART_MIX="$(echo "$PARTSET" | wc -w | tr -d '[:space:]')"
NPLAIN_MIX="$(echo "$PLAINSET" | wc -w | tr -d '[:space:]')"

if [ "${NPART_MIX:-0}" -lt 1 ] || [ "${NPLAIN_MIX:-0}" -lt 1 ] || [ -z "$MIGKEY_MID" ]; then
  record SKIP "phase 4: mixed node placement" "node is not mixed (${NPART_MIX:-0} partitioned, ${NPLAIN_MIX:-0} whole, MID='${MIGKEY_MID:-<none>}')"
else
  ptok="$(node_key "$PARTITIONED")"
  stok="$(node_key "$SLICED")"
  if [ -n "${ptok:-}" ] && [ "${ptok:-0}" != 0 ] && [ -n "${stok:-}" ] && [ "${stok:-0}" != 0 ]; then
    record PASS "mixed node advertises both families at once" "${PARTITIONED}=${ptok}, ${SLICED}=${stok}, keys=${KEYS:-<none>}"
  else
    record FAIL "mixed node advertises both families at once" "${PARTITIONED}='${ptok:-<absent>}', ${SLICED}='${stok:-<absent>}'"
  fi

  UAE_TOTAL=0; UAE_UNREAD=0
  for r in $(seq 1 "$ROUNDS"); do
    PP="${PODPFX}-mixed-part-${r}"
    LP="${PODPFX}-mixed-logical-${r}"
    mkpod "$PP" "$(partition_reslines "$MIGKEY_MID")"
    mkpod "$LP" "$(sliced_reslines 20)"
    prun=1; wait_running "$PP" || prun=0
    lrun=1; wait_running "$LP" || lrun=0
    pcards="$(wait_pod_cards "$PP")"
    lcards="$(wait_pod_cards "$LP")"

    if [ "$prun" = 1 ] && [ -n "$pcards" ]; then
      bad=""; for c in $pcards; do in_set "$c" "$PARTSET" || bad="$c"; done
      if [ -n "$bad" ]; then
        record FAIL "mixed round ${r}: partition lands on a partitioned card" "${PP} attributed ${bad} not in PARTSET"
      else
        record PASS "mixed round ${r}: partition lands on a partitioned card" "${PP} Running on ${pcards} (profile $(pod_profiles "$PP"))"
      fi
    else
      record FAIL "mixed round ${r}: partition lands on a partitioned card" "${PP} running=${prun} cards='${pcards:-<none>}' held='$(held_reason "$PP")'"
    fi

    if [ "$lrun" = 1 ] && [ -n "$lcards" ]; then
      bad=""; for c in $lcards; do in_set "$c" "$PLAINSET" || bad="$c"; done
      if [ -n "$bad" ]; then
        record FAIL "mixed round ${r}: logical slice lands on a whole card" "${LP} attributed partitioned card ${bad}"
      else
        record PASS "mixed round ${r}: logical slice lands on a whole card" "${LP} Running on ${lcards}"
      fi
    else
      record FAIL "mixed round ${r}: logical slice lands on a whole card" "${LP} running=${lrun} cards='${lcards:-<none>}' held='$(held_reason "$LP")'"
    fi

    for p in "$PP" "$LP"; do
      if ! n="$(pod_unexpected_admission "$p")"; then
        UAE_UNREAD=$((UAE_UNREAD + 1))
      elif [ -n "$n" ]; then
        UAE_TOTAL=$((UAE_TOTAL + n))
      fi
    done
    delpods "$PP" "$LP"
    wait_card_idle || true
  done

  if [ "$UAE_UNREAD" != 0 ]; then
    record FAIL "mixed node: zero UnexpectedAdmissionError across rounds" "${UAE_TOTAL} counted, ${UAE_UNREAD} unreadable"
  elif [ "$UAE_TOTAL" = 0 ]; then
    record PASS "mixed node: zero UnexpectedAdmissionError across rounds" "${ROUNDS} round(s) x 2 Pods, 0 UnexpectedAdmissionError"
  else
    record FAIL "mixed node: zero UnexpectedAdmissionError across rounds" "${UAE_TOTAL} UnexpectedAdmissionError event(s)"
  fi
fi

# ===================================================================================================
# Phase 5 — Live capacity ledger, Node write volume & saturation.
# ===================================================================================================
echo
echo "[case-9] === phase 5: live capacity ledger and saturation ==="
CEILING="$(partition_ceiling)"
if [ -z "$MIGKEY_MID" ] || [ -z "$MIGKEY_FULL" ] || [ -z "$MIGKEY_SMALL" ] || [ "${NCARD:-0}" -lt 1 ]; then
  record SKIP "phase 5: live capacity ledger and saturation" "missing profile keys (MID='${MIGKEY_MID:-<none>}' FULL='${MIGKEY_FULL:-<none>}' SMALL='${MIGKEY_SMALL:-<none>}')"
else
  wait_card_idle || true
  base_mid="$(node_key "$MIGKEY_MID")"
  base_full="$(node_key "$MIGKEY_FULL")"
  want_mid=$((MID_CNT * NCARD))
  want_full=$((FULL_CNT * NCARD))
  if [ "${base_mid:-x}" = "$want_mid" ] && [ "${base_full:-x}" = "$want_full" ]; then
    record PASS "idle per-profile keys equal the cards' geometry" "${MIGKEY_MID}=${base_mid}, ${MIGKEY_FULL}=${base_full}"
  else
    record FAIL "idle per-profile keys equal the cards' geometry" "${MIGKEY_MID}='${base_mid:-<absent>}' (want ${want_mid}), ${MIGKEY_FULL}='${base_full:-<absent>}' (want ${want_full})"
  fi

  start_node_write_probe
  sleep "$IDLE_WINDOW"
  stop_node_write_probe
  IDLE_WRITES="$(node_write_count)"
  rm -f "$NODE_WRITE_LOG"

  start_node_write_probe
  CARVE_T0=$(date +%s)
  A="${PODPFX}-ledger-mid"
  mkpod "$A" "$(partition_reslines "$MIGKEY_MID")"
  if ! wait_running "$A"; then
    record FAIL "live ledger: mid-size partition admits" "${A} did not reach Running (held: $(held_reason "$A"))"
    stop_node_write_probe
  else
    record PASS "live ledger: mid-size partition admits" "${A} Running on $(wait_pod_cards "$A")"
    exp_full=$((want_full - FULL_CNT))
    cur_full=""; cur_mid=""
    for _ in $(seq 1 30); do
      cur_full="$(node_key "$MIGKEY_FULL")"
      cur_mid="$(node_key "$MIGKEY_MID")"
      [ "${cur_full:-x}" = "$exp_full" ] && break
      sleep 3
    done
    if [ "${cur_full:-x}" = "$exp_full" ]; then
      record PASS "whole-card profile key falls to what the cards can still host" "${MIGKEY_FULL}: ${base_full} -> ${cur_full}"
    else
      record FAIL "whole-card profile key falls to what the cards can still host" "${MIGKEY_FULL}='${cur_full:-<absent>}', want ${exp_full}"
    fi
    if [ "${cur_mid:-x}" = "$want_mid" ]; then
      record PASS "same-size profile key still reads allocated + remaining" "${MIGKEY_MID}=${cur_mid} unchanged"
    else
      record FAIL "same-size profile key still reads allocated + remaining" "${MIGKEY_MID}='${cur_mid:-<absent>}', want ${want_mid}"
    fi

    if [ "${NCARD:-0}" = 1 ]; then
      B="${PODPFX}-ledger-full"
      mkpod "$B" "$(partition_reslines "$MIGKEY_FULL")"
      hr="$(assert_held "$B")"
      if [ -n "$hr" ]; then
        record PASS "whole-card-profile Pod held on occupied single-card node" "${B} held [${hr}]"
      else
        record FAIL "whole-card-profile Pod held on occupied single-card node" "${B} not held (phase=$(phase "$B"))"
      fi
      delpod "$B"
    else
      record SKIP "whole-card-profile Pod held on occupied single-card node" "${NCARD} partitioned cards — sibling card can host FULL"
    fi

    delpod "$A"
    for _ in $(seq 1 30); do [ "$(node_key "$MIGKEY_FULL")" = "$want_full" ] && break; sleep 3; done
    CARVE_T1=$(date +%s)
    stop_node_write_probe
    CARVE_WRITES="$(node_write_count)"
    rm -f "$NODE_WRITE_LOG"
    record PASS "OBSERVED: node-status write volume" "idle ${IDLE_WRITES}/${IDLE_WINDOW}s; carve+free ${CARVE_WRITES}/$((CARVE_T1 - CARVE_T0))s"
  fi

  wait_card_idle || true
  if [ "${NCARD:-0}" = 1 ]; then
    X="${PODPFX}-x-full"; Y="${PODPFX}-x-mid"
    mkpod "$X" "$(partition_reslines "$MIGKEY_FULL")"
    mkpod "$Y" "$(partition_reslines "$MIGKEY_MID")"
    xr=1; wait_running "$X" || xr=0
    yr=1; wait_running "$Y" || yr=0
    both=$((xr + yr))
    if [ "$both" -le 1 ]; then
      record PASS "OBSERVED: mutually exclusive profiles do not both run" "only ${both} of 2 ran on single card"
    else
      record FAIL "OBSERVED: mutually exclusive profiles do not both run" "BOTH ${X} and ${Y} Running on ONE card"
    fi
    delpods "$X" "$Y"
    wait_card_idle || true
  else
    record SKIP "OBSERVED: mutually exclusive profiles do not both run" "${NCARD} partitioned cards"
  fi

  base_tok=""
  for _ in $(seq 1 20); do
    base_tok="$(node_key "$PARTITIONED")"
    [ "${base_tok:-x}" = "${CEILING}" ] && break
    sleep 3
  done
  if [ "${base_tok:-x}" = "${CEILING}" ]; then
    record PASS "idle node advertises full partition ceiling" "${PARTITIONED}=${base_tok}"
  else
    record FAIL "idle node advertises full partition ceiling" "${PARTITIONED}='${base_tok:-<absent>}', want ${CEILING}"
  fi

  FILLED=(); SAT_OK=1
  for i in $(seq 1 "$NCARD"); do
    p="${PODPFX}-sat-full-${i}"
    mkpod "$p" "$(partition_reslines "$MIGKEY_FULL")"
    if ! wait_running "$p"; then
      record FAIL "every partitioned card is saturated" "${p} did not reach Running (held: $(held_reason "$p"))"
      SAT_OK=0; break
    fi
    FILLED+=("$p")
  done
  if [ "$SAT_OK" = 1 ]; then
    record PASS "every partitioned card is saturated" "${#FILLED[@]}x ${FULL} Running, GI count=$(node_gi_count)"
    sat_tok=""
    for _ in $(seq 1 30); do
      sat_tok="$(node_key "$PARTITIONED")"
      [ "${sat_tok:-x}" = "${#FILLED[@]}" ] && break
      sleep 3
    done
    if [ "${sat_tok:-x}" = "${#FILLED[@]}" ]; then
      record PASS "saturated node advertises live instance count" "${PARTITIONED}=${sat_tok}"
    else
      record FAIL "saturated node advertises live instance count" "${PARTITIONED}='${sat_tok:-<absent>}', want ${#FILLED[@]}"
    fi
    sat_cap="$(node_key_cap "$PARTITIONED")"
    if [ -n "$sat_cap" ]; then
      record PASS "capacity survives saturation" "${PARTITIONED} capacity=${sat_cap}"
    else
      record FAIL "capacity survives saturation" "${PARTITIONED} has no capacity entry"
    fi

    EXTRA="${PODPFX}-sat-extra"
    mkpod "$EXTRA" "$(partition_reslines "$MIGKEY_SMALL")"
    hr="$(assert_held "$EXTRA")"
    if [ -n "$hr" ]; then
      record PASS "further partition request held on full node" "${EXTRA} held [${hr}]"
    else
      record FAIL "further partition request held on full node" "${EXTRA} not held (phase=$(phase "$EXTRA"))"
    fi

    stillok=1
    for p in "${FILLED[@]}"; do
      running "$p" || stillok=0
      [ "$(pod_mig_devices "$p")" = 1 ] || stillok=0
    done
    if [ "$stillok" = 1 ]; then
      record PASS "running partitions unaffected by saturation" "${#FILLED[@]}x ${FULL} still Running with 1 MIG device"
    else
      record FAIL "running partitions unaffected by saturation" "a saturating Pod stopped or lost its MIG device"
    fi

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
      record PASS "freeing an instance raises the healthy count" "${PARTITIONED}: ${sat_tok} -> ${freed_tok}"
    else
      record FAIL "freeing an instance raises the healthy count" "${PARTITIONED}='${freed_tok:-<absent>}' (was ${sat_tok:-?})"
    fi

    REFILL="${PODPFX}-sat-refill"
    mkpod "$REFILL" "$(partition_reslines "$MIGKEY_SMALL")"
    if wait_running "$REFILL"; then
      record PASS "freed room is usable again without restart" "${REFILL} (${SMALL}) Running"
    else
      record FAIL "freed room is usable again without restart" "${REFILL} did not reach Running (held: $(held_reason "$REFILL"))"
    fi
    delpods "$REFILL" "${FILLED[@]}"
  else
    [ "${#FILLED[@]}" -gt 0 ] && delpods "${FILLED[@]}"
  fi
  wait_card_idle || true
fi

# ===================================================================================================
# Phase 6 — Per-card mutual exclusion, reuse, idle reclaim & small-then-large recompose.
# ===================================================================================================
echo
echo "[case-9] === phase 6: mutual exclusion, reuse, reclaim and recompose ==="
if [ "${NCARD:-0}" = 1 ] && [ -n "$MIGKEY_MID" ] && [ -n "$MIGKEY_FULL" ]; then
  A="${PODPFX}-mid-a"; B="${PODPFX}-mid-b"; C="${PODPFX}-mid-c"; FU="${PODPFX}-full"
  mkpod "$A" "$(partition_reslines "$MIGKEY_MID")"
  if wait_running "$A"; then
    nd="$(pod_mig_devices "$A")"; gic="$(node_gi_count)"
    if [ "$nd" = 1 ]; then
      record PASS "MID ${MID} admits with one MIG device" "${A} Running, 1 MIG device, node GI=${gic}"
    else
      record FAIL "MID ${MID} admits with one MIG device" "${A} Running but MIG devices=${nd} (want 1)"
    fi
  else
    record FAIL "MID ${MID} admits with one MIG device" "${A} not Running"
  fi
  mkpod "$FU" "$(partition_reslines "$MIGKEY_FULL")"
  hr="$(assert_held "$FU")"
  if [ -n "$hr" ]; then
    record PASS "FULL ${FULL} held while MID occupies the card" "${FU} held [${hr}]"
  else
    record FAIL "FULL ${FULL} held while MID occupies the card" "${FU} not held"
  fi
  delpod "$FU"
  mkpod "$B" "$(partition_reslines "$MIGKEY_MID")"
  if wait_running "$B"; then
    record PASS "two MID ${MID} fill one card" "${A} + ${B} Running, node GI=$(node_gi_count)"
  else
    record FAIL "two MID ${MID} fill one card" "${B} not Running"
  fi
  mkpod "$C" "$(partition_reslines "$MIGKEY_MID")"
  hr="$(assert_held "$C")"
  if [ -n "$hr" ]; then
    record PASS "third MID ${MID} held (card full)" "${C} held [${hr}]"
  else
    record FAIL "third MID ${MID} held (card full)" "${C} not held"
  fi
  delpods "$C" "$B" "$A"
  wait_card_idle || true
else
  record SKIP "profile mutual exclusion (needs a single partitioned card)" "partitioned cards N=${NCARD:-0}"
fi

if [ -n "$MIGKEY_MID" ]; then
  R1="${PODPFX}-reuse-1"; R2="${PODPFX}-reuse-2"
  mkpod "$R1" "$(partition_reslines "$MIGKEY_MID")"
  if wait_running "$R1"; then
    uuid1="$(exec_out "$R1" printenv NVIDIA_VISIBLE_DEVICES)"
    delpod "$R1"
    mkpod "$R2" "$(partition_reslines "$MIGKEY_MID")"
    if wait_running "$R2"; then
      uuid2="$(exec_out "$R2" printenv NVIDIA_VISIBLE_DEVICES)"
      record PASS "freed instance reused by a fresh request" "${R2} Running (before=${uuid1:-?} after=${uuid2:-?})"
    else
      record FAIL "freed instance reused by a fresh request" "${R2} not Running after ${R1} freed the slot"
    fi
    delpod "$R2"
  else
    record FAIL "freed instance reused by a fresh request" "${R1} not Running"
    delpod "$R1"
  fi
  wait_card_idle || true

  G1="${PODPFX}-reclaim"
  mkpod "$G1" "$(partition_reslines "$MIGKEY_MID")"
  if wait_running "$G1"; then
    before="$(node_gi_count)" || before="unknown"
    delpod "$G1"
    reclaimed=0; waited=0; gic=""
    for _ in $(seq 1 40); do
      if gic="$(node_gi_count)"; then
        [ "$gic" = 0 ] && { reclaimed=1; break; }
      else
        gic="unknown"
      fi
      sleep 3; waited=$((waited + 3))
    done
    if [ "$reclaimed" = 1 ]; then
      record PASS "idle instance reclaimed after debounce" "node GI ${before} -> 0 within ~${waited}s"
    else
      record FAIL "idle instance reclaimed after debounce" "node GI still ${gic} ~${waited}s after Pod exit"
    fi
  else
    record FAIL "idle instance reclaimed after debounce" "${G1} not Running"
    delpod "$G1"
  fi
else
  record SKIP "reuse & reclaim" "no MID profile key advertised"
fi

if [ "${NCARD:-0}" = 1 ] && [ -n "$MIGKEY_SMALL" ] && [ -n "$MIGKEY_FULL" ] && [ "${SMALL_CNT:-0}" -ge 1 ]; then
  smalls=(); ok_small=1
  for i in $(seq 1 "$SMALL_CNT"); do
    s="${PODPFX}-small-${i}"; smalls+=("$s")
    mkpod "$s" "$(partition_reslines "$MIGKEY_SMALL")"
    wait_running "$s" || ok_small=0
  done
  if [ "$ok_small" = 1 ]; then
    record PASS "SMALL ${SMALL} fills the card" "${SMALL_CNT}x ${SMALL} Running, GI=$(node_gi_count)"
  else
    record FAIL "SMALL ${SMALL} fills the card" "not all ${SMALL_CNT}x ${SMALL} reached Running"
  fi
  delpods "${smalls[@]}"
  wait_card_idle || true
  BIG="${PODPFX}-big"
  mkpod "$BIG" "$(partition_reslines "$MIGKEY_FULL")"
  if wait_running "$BIG"; then
    record PASS "whole-card FULL ${FULL} fits after small freed" "${BIG} Running, MIG devices=$(pod_mig_devices "$BIG")"
  else
    record FAIL "whole-card FULL ${FULL} fits after small freed" "${BIG} not Running"
  fi
  delpod "$BIG"
  wait_card_idle || true
else
  record SKIP "small-then-large recompose (needs a single partitioned card)" "partitioned cards N=${NCARD:-0}"
fi

# ===================================================================================================
# Phase 7 — Concurrent multi-profile resolution, reclaim-window convergence & init-container charge.
# ===================================================================================================
echo
echo "[case-9] === phase 7: concurrent multi-profile, reclaim window and terminated init container ==="
if [ -z "$MIGKEY_MID" ]; then
  record SKIP "phase 7: concurrent multi-profile, reclaim window & init container" "no MID profile key advertised"
else
  # 7a: two simultaneous requests for DIFFERENT profiles (MID + SMALL).
  if [ -z "$MIGKEY_SMALL" ] || [ "$SMALL" = "$MID" ]; then
    record SKIP "phase 7a: concurrent distinct-profile requests" "SMALL='${MIGKEY_SMALL:-<none>}' MID='${MIGKEY_MID}'"
  else
    PM="${PODPFX}-conc-mid"; PS="${PODPFX}-conc-small"
    mkpod "$PM" "$(partition_reslines "$MIGKEY_MID")"
    mkpod "$PS" "$(partition_reslines "$MIGKEY_SMALL")"
    mrun=1; wait_running "$PM" || mrun=0
    srun=1; wait_running "$PS" || srun=0
    if [ "$mrun" = 1 ] && [ "$srun" = 1 ]; then
      record PASS "both distinct-profile requests run" "${PM} (${MID}) + ${PS} (${SMALL}) Running"
    else
      record FAIL "both distinct-profile requests run" "${PM}=${mrun} ${PS}=${srun}"
    fi
    if [ "$mrun" = 1 ]; then
      smi="$(kubectl -n default exec "$PM" -- nvidia-smi -L 2>/dev/null)"
      devs="$(printf '%s\n' "$smi" | grep -c 'MIG')"
      rec="$(pod_profiles "$PM")"
      if printf '%s\n' "$smi" | grep -q "$MID" && [ "${devs:-0}" = 1 ]; then
        record PASS "${PM} given requested ${MID}" "1 ${MID} instance visible"
      else
        record FAIL "${PM} given requested ${MID}" "got '$(printf '%s' "$smi" | tr '\n' ';')'"
      fi
      if [ "$rec" = "$MID" ]; then
        record PASS "${PM} durable record names ${MID}" "recorded '${rec}'"
      else
        record FAIL "${PM} durable record names ${MID}" "recorded '${rec:-<none>}'"
      fi
    fi
    if [ "$srun" = 1 ]; then
      smi="$(kubectl -n default exec "$PS" -- nvidia-smi -L 2>/dev/null)"
      devs="$(printf '%s\n' "$smi" | grep -c 'MIG')"
      rec="$(pod_profiles "$PS")"
      if printf '%s\n' "$smi" | grep -q "$SMALL" && [ "${devs:-0}" = 1 ]; then
        record PASS "${PS} given requested ${SMALL}" "1 ${SMALL} instance visible"
      else
        record FAIL "${PS} given requested ${SMALL}" "got '$(printf '%s' "$smi" | tr '\n' ';')'"
      fi
      if [ "$rec" = "$SMALL" ]; then
        record PASS "${PS} durable record names ${SMALL}" "recorded '${rec}'"
      else
        record FAIL "${PS} durable record names ${SMALL}" "recorded '${rec:-<none>}'"
      fi
    fi
    uae=0; unreadable=0
    for p in "$PM" "$PS"; do
      n="$(pod_unexpected_admission "$p")"
      case "$n" in '') ;; *[!0-9]*) unreadable=$((unreadable + 1)) ;; *) uae=$((uae + n)) ;; esac
    done
    if [ "$uae" = 0 ] && [ "$unreadable" = 0 ]; then
      record PASS "concurrent distinct-profile: zero UnexpectedAdmissionError" "0 UAE events"
    else
      record FAIL "concurrent distinct-profile: zero UnexpectedAdmissionError" "${uae} UAE, ${unreadable} unreadable"
    fi
    delpods "$PM" "$PS"
    wait_card_idle || true
  fi

  # 7b: immediate replacement inside the reclaim window.
  RW_FIRST="${PODPFX}-rw-first"
  mkpod "$RW_FIRST" "$(partition_reslines "$MIGKEY_MID")"
  if wait_running "$RW_FIRST"; then
    record PASS "reclaim window: first partition runs" "${RW_FIRST} Running on $(pod_cards "$RW_FIRST")"
    delpod "$RW_FIRST"
    T0="$(date +%s)"
    ATTEMPTS=0; UAE=0; SHAPE_ADMISSION=0; SHAPE_START=0; SHAPE_OTHER=0; OK=0; WINNER=""
    while [ "$(( $(date +%s) - T0 ))" -lt "$BOUND" ]; do
      ATTEMPTS=$((ATTEMPTS + 1))
      R="${PODPFX}-rw-repl-${ATTEMPTS}"
      mkpod "$R" "$(partition_reslines "$MIGKEY_MID")"
      for _ in $(seq 1 20); do
        ph="$(phase "$R")"
        case "$ph" in Running) OK=1; WINNER="$R"; break ;; Failed) break ;; esac
        sleep 3
      done
      n="$(pod_unexpected_admission "$R")"
      case "$n" in ''|*[!0-9]*) ;; *) UAE=$((UAE + n)) ;; esac
      [ "$OK" = 1 ] && break
      shape="$(pod_failure_shape "$R")"
      case "$shape" in
        admission) SHAPE_ADMISSION=$((SHAPE_ADMISSION + 1)) ;;
        start)     SHAPE_START=$((SHAPE_START + 1)) ;;
        *)         SHAPE_OTHER=$((SHAPE_OTHER + 1)) ;;
      esac
      delpod "$R"
      sleep 5
    done
    ELAPSED=$(( $(date +%s) - T0 ))
    if [ "$OK" = 1 ]; then
      record PASS "reclaim window: replacement converges without intervention" "${WINNER} Running after ${ELAPSED}s on attempt ${ATTEMPTS}"
    else
      record FAIL "reclaim window: replacement converges without intervention" "no replacement ran within ${BOUND}s (${ATTEMPTS} attempts)"
    fi
    record PASS "OBSERVED: reclaim window cost" "attempts=${ATTEMPTS} elapsed=${ELAPSED}s admission=${SHAPE_ADMISSION}(UAE=${UAE}) start=${SHAPE_START} other=${SHAPE_OTHER}"
    [ -n "$WINNER" ] && delpod "$WINNER"
    wait_card_idle || true
  else
    record FAIL "reclaim window: first partition runs" "${RW_FIRST} did not reach Running"
    delpod "$RW_FIRST"
  fi

  # 7c: terminated init container still charges the card.
  if [ -z "$MIGKEY_FULL" ]; then
    record SKIP "phase 7c: terminated init container charge" "no FULL profile key advertised"
  else
    wait_card_idle || true
    BASE_FULL_7C="$(node_key "$MIGKEY_FULL")"
    P_INIT="${PODPFX}-init-pod"
    TESTPODS+=("$P_INIT")
    cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: { name: ${P_INIT}, namespace: default, labels: { kueue.x-k8s.io/queue-name: ${LQ} } }
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
$(partition_reslines "$MIGKEY_MID")
        requests:
$(partition_reslines "$MIGKEY_MID")
  containers:
    - name: main
      image: ${IMAGE}
      command: ["sleep", "86400"]
      resources:
        limits:   { cpu: "200m", memory: 256Mi }
        requests: { cpu: "200m", memory: 256Mi }
EOF
    if wait_running "$P_INIT" 60; then
      initstate="$(kubectl -n default get pod "$P_INIT" -o jsonpath='{.status.initContainerStatuses[0].state.terminated.reason}' 2>/dev/null)"
      if [ -n "$initstate" ]; then
        record PASS "init-container Pod runs past its init container" "${P_INIT} Running, init Terminated/${initstate}"
      else
        record FAIL "init-container Pod runs past its init container" "init container has no terminated state"
      fi
      gi="$(node_gi_count)"
      if [ "${gi:-0}" -ge 1 ]; then
        record PASS "hardware instance outlives terminated init container" "${gi} live GI(s) on card"
      else
        record FAIL "hardware instance outlives terminated init container" "${gi:-0} live GI(s) on card"
      fi
      exp_full_7c=$((BASE_FULL_7C - FULL_CNT))
      cur_full_7c=""
      for _ in $(seq 1 30); do
        cur_full_7c="$(node_key "$MIGKEY_FULL")"
        [ "${cur_full_7c:-x}" = "$exp_full_7c" ] && break
        sleep 3
      done
      if [ "${cur_full_7c:-x}" = "$exp_full_7c" ]; then
        record PASS "terminated init container's instance is still charged" "${MIGKEY_FULL}: ${BASE_FULL_7C} -> ${cur_full_7c}"
      else
        record FAIL "terminated init container's instance is still charged" "${MIGKEY_FULL}='${cur_full_7c:-<absent>}', want ${exp_full_7c}"
      fi
      Q_NEXT="${PODPFX}-init-next"
      mkpod "$Q_NEXT" "$(partition_reslines "$MIGKEY_MID")"
      if wait_running "$Q_NEXT"; then
        uae="$(pod_unexpected_admission "$Q_NEXT")"
        if [ -z "$uae" ]; then
          record PASS "reported free room beside init container is real" "${Q_NEXT} Running, 0 UAE"
        else
          record FAIL "reported free room beside init container is real" "${Q_NEXT} recorded ${uae} UAE"
        fi
      else
        record FAIL "reported free room beside init container is real" "${Q_NEXT} did not reach Running (held: $(held_reason "$Q_NEXT"))"
      fi
      delpods "$Q_NEXT" "$P_INIT"
      wait_card_idle || true
    else
      record FAIL "init-container Pod runs past its init container" "${P_INIT} did not reach Running (held: $(held_reason "$P_INIT"))"
      delpod "$P_INIT"
    fi
  fi
fi

# ===================================================================================================
# Phase 8 — Disable MIG and re-detect -> logical-slice capability returns.
# ===================================================================================================
echo
echo "[case-9] === phase 8: disable MIG -> logical-slice capability returns ==="
wait_card_idle || true
if [ "${NCARD:-0}" -gt 1 ]; then
  record SKIP "phase 8: disable MIG -> logical family returns" "${NCARD} cards are partitioned; disabling only card ${GPU_INDEX} leaves others partitioned"
elif set_mig_mode "$GPU_INDEX" 0; then
  refresh_dm
  logicalpct=""; partleft="x"; punits_left="x"
  for _ in $(seq 1 30); do
    logicalpct="$(node_key "${SLICED}.memory-percentage")"
    partleft="$(partition_profile_keys | wc -w | tr -d '[:space:]')"
    punits_left="$(node_key "${PARTITIONED}.units")"
    [ -n "$logicalpct" ] && [ "${partleft:-1}" = 0 ] && [ -z "$punits_left" ] && break
    sleep 4
  done
  if [ -n "$logicalpct" ] && [ "${partleft:-1}" = 0 ] && [ -z "$punits_left" ]; then
    record PASS "logical family returns after disable" "${SLICED}.memory-percentage=${logicalpct} back; partition keys gone"
  else
    record FAIL "logical family returns after disable" "${SLICED}.memory-percentage='${logicalpct:-<absent>}', partition keys left=${partleft}, ${PARTITIONED}.units='${punits_left:-<absent>}'"
  fi
else
  record FAIL "disable MIG mode" "nvidia-smi -mig 0 did not converge to Disabled"
fi

part_results "Real NVIDIA MIG hardware lifecycle"

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). Diagnose:"
  echo "  kubectl get devices ${GPU_NODE} -o yaml"
  echo "  kubectl get node ${GPU_NODE} -o json | jq '.status.allocatable | with_entries(select(.key|test(\"sliced|partitioned\")))'"
  echo "  kubectl -n ${NS} logs ds/${DM_DS} --tail=200"
  echo "  ${MIG_NODE_SSH} sudo nvidia-smi mig -lgi"
  exit 1
fi
echo "CASE 9 PASS"
