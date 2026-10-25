#!/usr/bin/env bash
#
# CASE 23 — NVIDIA MIG dynamic-allocation lifecycle
#   (MUTATING, self-recovering; AUTO-SKIPS without a MIG-capable NVIDIA card AND a node-SSH address)
#
#   MIG_NODE_SSH=<user@host> case-23.sh <NS>
#
# Goal:        On a MIG-capable NVIDIA card the operator observes MIG geometry and dynamically
#              allocates the hardware GPU/compute instances that back scheduled workloads. This case
#              proves, end to end on real hardware, that:
#                - a card whose MIG mode is OFF serves logical slices — two Pods at 20% and 40%
#                  coexist and are runtime-capped: each Pod's CUDA_DEVICE_SM_LIMIT equals its
#                  cores-percentage (the vendor libvgpu path);
#                - after the ADMIN enables MIG (nvidia-smi, over SSH) and the Device Manager re-detects,
#                  the card carves into its canonical profile set (names/counts match the ledger) and
#                  MOVES FAMILY: the node advertises one nvidia.com/gpu.partitioned.<kind>-<profile> key
#                  per profile plus nvidia.com/gpu.partitioned.units, while the whole logical family
#                  (nvidia.com/gpu.sliced + its .units/.cores-percentage/.memory-percentage/.memory-mib
#                  counting keys) DISAPPEARS — a partitioned card serves no logical slice, so it leaves
#                  the logical population entirely rather than keeping a units key;
#                - the InstanceType numeric four-view (EX/SH/SL/PT) + the per-profile ledger reflect the
#                  geometry: PT carries the partition instances and SL collapses to zero once every card
#                  of the pool is partitioned;
#                - profiles are placement-mutually-exclusive (a size-4 3g instance blocks a size-8 7g
#                  instance on the same card; two size-4 fill it, a third is held);
#                - deleting a MIG Pod frees the instance so a fresh request of the same profile admits
#                  again (reuse), and an idle card's instance is reclaimed within the debounce;
#                - after small instances are freed a whole-card profile fits again;
#                - disabling MIG and re-detecting returns the card to its logical-slice capability, and
#                  every .partitioned key disappears in the same move.
# Environment: A reachable cluster whose active context is the GPU cluster, a node with a real NVIDIA
#              card, AND SSH to that node (sudo nvidia-smi) supplied via MIG_NODE_SSH=<user@host>.
#              This case toggles node hardware state, so it needs the node address; it does NOT guess
#              it. It EXITS 2 (input required) when MIG_NODE_SSH is unset — provide the address and
#              re-run. It AUTO-SKIPS (exit 0) when the node's card is not MIG-capable, or the cluster
#              advertises no nvidia accelerator. A Devices ledger that cannot be read is a setup FAIL
#              (exit 1), never a skip.
#              Prerequisites the ADMIN owns (the case attempts the mode switch but cannot force them):
#              the card must be idle, driver-handle daemons (DCGM/nvsm/exporters) stopped, nvidia_drm
#              unloaded, CAP_SYS_ADMIN available. Targets Hopper+ (no GPU reset needed); on Ampere a
#              mode switch that pends a reset is reported as a FAIL with guidance, not forced.
# Inputs:      All real, nothing mocked — the MIG geometry, the libvgpu cap, and the GI/CI create/destroy
#              are the verification. The case toggles the node's MIG mode over SSH and restarts the
#              Device Manager to re-detect (a DaemonSet restart is sufficient — an existing group's
#              capability is rewritten in place); test Pods go through the accelerated pool's entrance
#              LocalQueue. Profiles (SMALL size-1, MID size-4 "3g", FULL size-8 "7g") are DISCOVERED from
#              the card's own ledger, so the case runs on A100 (…5gb/…20gb/…40gb) and H100
#              (…10gb/…40gb/…80gb) alike. Placement-exclusion + small-then-large sub-checks run only on a
#              single-MIG-card node (recorded SKIPPED otherwise, as their per-card math needs one card).
# Expected:    See Goal — each bullet is one PASS row in the results table.
# Cleanup:     Trap deletes every test Pod and restores the node's MIG mode to the state the case found
#              it in (re-enable if it started enabled; disable if it started disabled), refreshing the
#              Device Manager so the ledger realigns. Idempotent; runs on pass AND fail.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

NS="${1:?usage: MIG_NODE_SSH=<user@host> case-23.sh <NS>}"

PODPFX=gpustack-e2e-mig

# shellcheck disable=SC2034 # Read by the sourced library, which shellcheck does not follow.
CASE_ID=23
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_pool-lib.sh"
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_partition-lib.sh"

# ---------------------------------------------------------------------------------------------------
# The hardware gate and the cluster correlation. The library owns both: the node address is never
# guessed, the card's partitioning mode decides whether the case can run at all, and the k8s node
# behind that address is correlated with the pool that backs it.
# ---------------------------------------------------------------------------------------------------
part_require_node_ssh "case-23.sh"
part_require_mig_capable
part_discover

# ---------------------------------------------------------------------------------------------------
# Cleanup. The trap restores what the case changed: the test Pods go, and the card goes back into the
# mode the case found it in.
# ---------------------------------------------------------------------------------------------------
restore() {
  echo
  echo "[case-23] cleanup: deleting test Pods and restoring MIG mode to '${INITIAL_MODE}'"
  delete_test_pods
  local now; now="$(mig_mode "$GPU_INDEX")"
  if [ "$now" != "$INITIAL_MODE" ]; then
    # A disable needs an idle card; wait for any just-deleted workload's instance to reclaim first.
    [ "$INITIAL_MODE" = Enabled ] || wait_card_idle || true
    if [ "$INITIAL_MODE" = Enabled ]; then
      set_mig_mode "$GPU_INDEX" 1 || set_mig_mode "$GPU_INDEX" 0
    else
      set_mig_mode "$GPU_INDEX" 0
    fi
    refresh_dm
  fi
  node_ssh_close
}
trap restore EXIT

# ===================================================================================================
# Phase L — Logical slicing, only when the card started with MIG OFF.
# ===================================================================================================
if [ "$INITIAL_MODE" = Disabled ]; then
  echo
  echo "[case-23] === Phase L: logical slicing (MIG off) — 20% and 40% coexist ==="
  P20="${PODPFX}-logical20"; P40="${PODPFX}-logical40"
  mkpod "$P20" "          ${SLICED}: \"1\"
          ${SLICED}.memory-percentage: \"20\"
          ${SLICED}.cores-percentage: \"20\""
  mkpod "$P40" "          ${SLICED}: \"1\"
          ${SLICED}.memory-percentage: \"40\"
          ${SLICED}.cores-percentage: \"40\""
  l20=1; wait_running "$P20" || l20=0
  l40=1; wait_running "$P40" || l40=0
  if [ "$l20" = 1 ] && [ "$l40" = 1 ]; then
    sm20="$(exec_out "$P20" printenv CUDA_DEVICE_SM_LIMIT)"
    sm40="$(exec_out "$P40" printenv CUDA_DEVICE_SM_LIMIT)"
    # The product sets CUDA_DEVICE_SM_LIMIT to the container's cores-percentage.
    if [ "$sm20" = 20 ] && [ "$sm40" = 40 ]; then
      record PASS "logical 20% + 40% coexist and are capped" "${P20}(SM=${sm20}) + ${P40}(SM=${sm40}) both Running on the logical-sliced card"
    else
      record FAIL "logical 20% + 40% coexist and are capped" "${P20} SM limit='${sm20:-?}' (want 20), ${P40} SM limit='${sm40:-?}' (want 40) — the cores-percentage did not reach the runtime cap"
    fi
  else
    record FAIL "logical 20% + 40% coexist and are capped" "20% running=${l20} 40% running=${l40} — logical slicing broken on the MIG-off card"
  fi
  delpod "$P20"; delpod "$P40"
else
  record SKIP "logical-slice path (card started MIG-on)" "card ${GPU_INDEX} was already MIG-enabled; logical slicing is verified in reverse by Phase D (disable → logical returns)"
fi

# ===================================================================================================
# Phase E — Enable MIG (if not already) and re-detect.
# ===================================================================================================
echo
echo "[case-23] === Phase E: enable MIG + Device Manager re-detect ==="
CUR="$(node_ssh sudo nvidia-smi -i "$GPU_INDEX" --query-gpu=mig.mode.current --format=csv,noheader 2>/dev/null | tr -d '[:space:]')"
if [ "$CUR" != Enabled ]; then
  if ! set_mig_mode "$GPU_INDEX" 1; then
    record FAIL "enable MIG mode" "nvidia-smi -mig 1 did not converge to Enabled (a pending GPU reset on Ampere, a busy card, or a loaded nvidia_drm blocks it) — see Prerequisites"
    echo; echo "== CASE 23 — cannot proceed without MIG enabled =="; exit 1
  fi
fi
refresh_dm
MIGKEY_FULL=""
# Wait for the card to move family: the node advertises at least one per-profile partition key.
for _ in $(seq 1 30); do
  keys="$(partition_profile_keys)"
  [ -n "$keys" ] && break
  sleep 4
done
read -r SMALL SMALL_CNT MID MID_CNT FULL FULL_CNT NCARD NTOT <<<"$(card_profiles)"
echo "[case-23] discovered profiles: SMALL=${SMALL}(${SMALL_CNT}/card) MID=${MID}(${MID_CNT}/card) FULL=${FULL}(${FULL_CNT}/card); partitioned cards N=${NCARD:-0} of ${NTOT:-0}"
echo "[case-23] advertised per-profile partition keys: ${keys:-<none>}"

# ===================================================================================================
# Phase P — Profiles carve correctly; node advertises per-profile keys; logical keys drop to 0.
# ===================================================================================================
echo
echo "[case-23] === Phase P: profile carve + capability keys ==="
if [ -n "$MID" ] && [ -n "$FULL" ] && [ "${MID_CNT:-0}" = 2 ] && [ "${FULL_CNT:-0}" = 1 ]; then
  record PASS "canonical profiles carved (names/counts)" "MID ${MID}=2/card, FULL ${FULL}=1/card, SMALL ${SMALL}=${SMALL_CNT}/card — matches the fixed placement layout"
else
  record FAIL "canonical profiles carved (names/counts)" "MID=${MID}(${MID_CNT}) FULL=${FULL}(${FULL_CNT}) SMALL=${SMALL}(${SMALL_CNT}) — expected a size-4 (2/card) and a size-8 (1/card) profile"
fi
MIGKEY_MID="$(profile_key "$MID")"
MIGKEY_FULL="$(profile_key "$FULL")"
mid_key_cap="$(node_key "$MIGKEY_MID")"
full_key_cap="$(node_key "$MIGKEY_FULL")"
if [ -n "$MIGKEY_MID" ] && [ -n "$MIGKEY_FULL" ] && [ -n "$mid_key_cap" ] && [ -n "$full_key_cap" ]; then
  record PASS "node advertises per-profile partition keys" "${MIGKEY_MID}=${mid_key_cap}, ${MIGKEY_FULL}=${full_key_cap}"
else
  record FAIL "node advertises per-profile partition keys" "MID key='${MIGKEY_MID:-<absent>}'=${mid_key_cap:-<absent>}, FULL key='${MIGKEY_FULL:-<absent>}'=${full_key_cap:-<absent>} — a partitioned card must advertise one key per profile it can host"
fi
# The partition family's counting key comes with them: a partitioned card is worth a whole card's units.
punits="$(node_key "${PARTITIONED}.units")"
[ -n "$punits" ] \
  && record PASS "node advertises the partition units key" "${PARTITIONED}.units=${punits}" \
  || record FAIL "node advertises the partition units key" "${PARTITIONED}.units absent — the partition family's credit input is missing"
# THE FAMILY SWAP. A partitioned card serves no logical slice, so it leaves the logical population
# entirely: the reconciler-owned logical counting keys are REMOVED (not retained, not zeroed) and the
# device-plugin's logical token pool drains to nothing. This can only be asserted when EVERY card of
# the group is partitioned — one unpartitioned sibling legitimately keeps the logical family alive.
logicalpct="$(node_key "${SLICED}.memory-percentage")"
units="$(node_key "${SLICED}.units")"
if [ "${NCARD:-0}" != "${NTOT:-0}" ] || [ "${NTOT:-0}" = 0 ]; then
  record SKIP "logical family leaves the MIG card" "only ${NCARD:-0} of ${NTOT:-0} card(s) partitioned — the unpartitioned sibling(s) keep the logical keys alive, so their absence is not assertable here"
elif [ -z "$logicalpct" ] && [ -z "$units" ] && node_key_gone "${SLICED}"; then
  record PASS "logical family leaves the MIG card" "${SLICED}.memory-percentage and ${SLICED}.units removed, ${SLICED} pool drained — the card now serves only ${PARTITIONED}"
else
  record FAIL "logical family leaves the MIG card" "${SLICED}.memory-percentage='${logicalpct:-<absent>}', ${SLICED}.units='${units:-<absent>}' (both want absent), ${SLICED}='$(node_key "${SLICED}")' (want absent or 0)"
fi

# ===================================================================================================
# Phase I — InstanceType numeric values reflect the MIG geometry (aggregate profiles + PT/SL views).
# ===================================================================================================
echo
echo "[case-23] === Phase I: InstanceType numeric values ==="
# PT (acceleratorPartitioned) is the partitioned cards' view and must be non-zero. SL
# (acceleratorSliced) is the unpartitioned cards' view, and must collapse to zero — but only when every
# card of the pool is partitioned; with an unpartitioned sibling the SL check is not applicable.
#
# The POOL decides both of those, not the node. A pool spanning several GPU nodes keeps its logical
# view alive on the sibling nodes' whole cards while one node's card is partitioned, and its aggregate
# profile counts sum over every partitioned card it has. NCARD/NTOT are the toggled node's own group —
# right for the node-key assertions above, wrong here — so count the pool's cards from every Devices
# object reporting its group.
read -r POOL_PART POOL_TOT <<<"$(kubectl get devices.v1alpha1.worker.gpustack.ai -o json 2>/dev/null | GRP="${GROUPID}" python3 -c "
import json, os, sys
grp = os.environ['GRP']
tot = part = 0
for d in json.load(sys.stdin).get('items', []):
    for g in d.get('spec', {}).get('groups', []) or []:
        if g.get('id') != grp:
            continue
        for a in g.get('accelerators', []) or []:
            tot += 1
            if ((a.get('status') or {}).get('physicalSliced') or {}).get('count', 0) > 0:
                part += 1
print(part, tot)
" 2>/dev/null)"
echo "[case-23]   pool-wide partitioned cards: ${POOL_PART:-0} of ${POOL_TOT:-0} (node-local was ${NCARD:-0} of ${NTOT:-0})"
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
  record PASS "InstanceType four-view reflects MIG geometry" "aggregate profiles ${MID}=$((2*${POOL_PART:-1})), ${FULL}=$((1*${POOL_PART:-1})), PT capacity > 0 over ${POOL_PART:-0}/${POOL_TOT:-0} partitioned card(s) [${it_ok#OK }]"
else
  record FAIL "InstanceType four-view reflects MIG geometry" "status detail mismatch [${it_ok:-<empty>}] — PT must carry the partition instances and SL must read 0 on an all-partitioned pool"
fi

# ===================================================================================================
# Phase X — Placement mutual exclusion (single-MIG-card only; per-card math needs one card).
# ===================================================================================================
echo
echo "[case-23] === Phase X: profile mutual exclusion ==="
if [ "${NCARD:-0}" = 1 ] && [ -n "$MIGKEY_MID" ] && [ -n "$MIGKEY_FULL" ]; then
  A="${PODPFX}-mid-a"; B="${PODPFX}-mid-b"; C="${PODPFX}-mid-c"; FU="${PODPFX}-full"
  # A size-4 MID admits and gets exactly one MIG device.
  mkpod "$A" "          ${PARTITIONED}: \"1\"
          ${MIGKEY_MID}: \"1\""
  if wait_running "$A"; then
    nd="$(pod_mig_devices "$A")"; gic="$(node_gi_count)"
    [ "$nd" = 1 ] && record PASS "MID ${MID} admits with one MIG device" "${A} Running, nvidia-smi -L=1 MIG device, node GI count=${gic}" \
      || record FAIL "MID ${MID} admits with one MIG device" "${A} Running but nvidia-smi -L MIG devices=${nd} (want 1)"
  else
    record FAIL "MID ${MID} admits with one MIG device" "${A} not Running — MIG allocate failed (see device-manager logs)"
  fi
  # With a size-4 placed, a size-8 FULL cannot fit the same (only) card → held.
  mkpod "$FU" "          ${PARTITIONED}: \"1\"
          ${MIGKEY_FULL}: \"1\""
  hr="$(assert_held "$FU")"
  [ -n "$hr" ] && record PASS "FULL ${FULL} held while MID occupies the card" "${FU} held [${hr}] — a size-8 profile cannot co-exist with a size-4 (mutual exclusion)" \
    || record FAIL "FULL ${FULL} held while MID occupies the card" "${FU} not held with no concrete reason — placement exclusion may be violated"
  delpod "$FU"
  # A second MID fills the card (2 per card); a third is held.
  mkpod "$B" "          ${PARTITIONED}: \"1\"
          ${MIGKEY_MID}: \"1\""
  if wait_running "$B"; then
    record PASS "two MID ${MID} fill one card" "${A} + ${B} both Running (2× size-4 = full card), node GI count=$(node_gi_count)"
  else
    record FAIL "two MID ${MID} fill one card" "${B} not Running — the card should host two size-4 instances"
  fi
  mkpod "$C" "          ${PARTITIONED}: \"1\"
          ${MIGKEY_MID}: \"1\""
  hr="$(assert_held "$C")"
  [ -n "$hr" ] && record PASS "third MID ${MID} held (card full)" "${C} held [${hr}] — no third size-4 slot" \
    || record FAIL "third MID ${MID} held (card full)" "${C} not held — the card was over-partitioned"
  delpod "$C"; delpod "$B"; delpod "$A"
else
  record SKIP "profile mutual exclusion (needs a single partitioned card)" "partitioned cards N=${NCARD:-0}, MID key='${MIGKEY_MID:-<none>}', FULL key='${MIGKEY_FULL:-<none>}'; per-card placement exclusion is asserted only on a one-card node"
fi

# ===================================================================================================
# Phase R — Reuse: delete a MIG Pod, immediately re-request the same profile → it admits again.
# ===================================================================================================
echo
echo "[case-23] === Phase R: reuse a freed instance ==="
if [ -n "$MIGKEY_MID" ]; then
  R1="${PODPFX}-reuse-1"; R2="${PODPFX}-reuse-2"
  # Phase X force-deleted its Pods but the instances behind them are destroyed on the reclaim
  # debounce, not on Pod deletion. Requesting R1 before that lands asks for a slot on a card that is
  # still full: the AdmissionCheck refuses it and the retry rides Kueue's backoff, which can outlast
  # this phase's own wait. Wait for the card to go idle first — the same precondition Phases S and D
  # already take.
  wait_card_idle || echo "[case-23]   warning: card still reports live instances entering the reuse phase"
  mkpod "$R1" "          ${PARTITIONED}: \"1\"
          ${MIGKEY_MID}: \"1\""
  if wait_running "$R1"; then
    uuid1="$(exec_out "$R1" bash -c 'echo $NVIDIA_VISIBLE_DEVICES')"
    delpod "$R1"
    mkpod "$R2" "          ${PARTITIONED}: \"1\"
          ${MIGKEY_MID}: \"1\""
    if wait_running "$R2"; then
      uuid2="$(exec_out "$R2" bash -c 'echo $NVIDIA_VISIBLE_DEVICES')"
      record PASS "freed instance reused by a fresh request" "${R2} Running on the freed slot (MIG dev before=${uuid1:-?} after=${uuid2:-?})"
    else
      record FAIL "freed instance reused by a fresh request" "${R2} not Running after ${R1} freed the slot"
    fi
    delpod "$R2"
  else
    record FAIL "freed instance reused by a fresh request" "${R1} not Running — could not set up the reuse precondition"
    delpod "$R1"
  fi
else
  record SKIP "reuse a freed instance" "no size-4 MID profile key advertised"
fi

# ===================================================================================================
# Phase G — Reclaim: delete a MIG Pod, stay idle → the operator destroys the instance within the debounce.
# ===================================================================================================
echo
echo "[case-23] === Phase G: reclaim an idle instance ==="
if [ -n "$MIGKEY_MID" ]; then
  G1="${PODPFX}-reclaim"
  mkpod "$G1" "          ${PARTITIONED}: \"1\"
          ${MIGKEY_MID}: \"1\""
  if wait_running "$G1"; then
    before="$(node_gi_count)" || before="unknown"
    delpod "$G1"
    reclaimed=0; waited=0; gic=""
    for _ in $(seq 1 40); do   # up to ~120s for the reclaim debounce
      if gic="$(node_gi_count)"; then
        [ "$gic" = 0 ] && { reclaimed=1; break; }
      else
        gic="unknown (node probe did not answer)"
      fi
      sleep 3; waited=$((waited + 3))
    done
    [ "$reclaimed" = 1 ] && record PASS "idle instance reclaimed after debounce" "node GI count ${before}→0 within ~${waited}s of the Pod exiting (no re-request)" \
      || record FAIL "idle instance reclaimed after debounce" "node GI count still ${gic} ~${waited}s after the Pod exited — the instance was not reclaimed or could not be read"
  else
    record FAIL "idle instance reclaimed after debounce" "${G1} not Running — could not set up the reclaim precondition"
    delpod "$G1"
  fi
else
  record SKIP "idle instance reclaimed after debounce" "no size-4 MID profile key advertised"
fi

# ===================================================================================================
# Phase S — Small-then-large: fill the card with SMALL instances, free them, then a FULL fits again.
# ===================================================================================================
echo
echo "[case-23] === Phase S: small profiles then a whole-card profile ==="
MIGKEY_SMALL="$(profile_key "$SMALL")"
if [ "${NCARD:-0}" = 1 ] && [ -n "$MIGKEY_SMALL" ] && [ -n "$MIGKEY_FULL" ] && [ "${SMALL_CNT:-0}" -ge 1 ]; then
  smalls=(); ok_small=1
  for i in $(seq 1 "$SMALL_CNT"); do
    s="${PODPFX}-small-${i}"; smalls+=("$s")
    mkpod "$s" "          ${PARTITIONED}: \"1\"
          ${MIGKEY_SMALL}: \"1\""
    wait_running "$s" || ok_small=0
  done
  [ "$ok_small" = 1 ] && record PASS "SMALL ${SMALL} fills the card" "${SMALL_CNT}× ${SMALL} all Running, node GI count=$(node_gi_count)" \
    || record FAIL "SMALL ${SMALL} fills the card" "not all ${SMALL_CNT}× ${SMALL} reached Running"
  for s in "${smalls[@]}"; do delpod "$s"; done
  # Wait for the small instances to reclaim so the whole card is free again.
  for _ in $(seq 1 40); do [ "$(node_gi_count)" = 0 ] && break; sleep 3; done
  BIG="${PODPFX}-big"
  mkpod "$BIG" "          ${PARTITIONED}: \"1\"
          ${MIGKEY_FULL}: \"1\""
  if wait_running "$BIG"; then
    nd="$(pod_mig_devices "$BIG")"
    record PASS "whole-card FULL ${FULL} fits after small freed" "${BIG} Running, nvidia-smi -L=${nd} MIG device — the card recomposed to one 7g instance"
  else
    record FAIL "whole-card FULL ${FULL} fits after small freed" "${BIG} not Running — the card did not free back to a whole-card profile"
  fi
  delpod "$BIG"
else
  record SKIP "small-then-large recompose (needs a single partitioned card)" "partitioned cards N=${NCARD:-0} / profile keys SMALL='${MIGKEY_SMALL:-<none>}' FULL='${MIGKEY_FULL:-<none>}'"
fi

# ===================================================================================================
# Phase D — Disable MIG and re-detect → the card returns to its logical-slice capability.
# ===================================================================================================
echo
echo "[case-23] === Phase D: disable MIG → logical-slice capability returns ==="
# A mode switch needs an idle card — wait for the whole-card FULL from Phase S (just deleted) to
# reclaim before disabling, else nvidia-smi -mig 0 cannot converge (the disable prerequisite).
wait_card_idle || echo "[case-23]   warning: card still reports live instances before disable"
if set_mig_mode "$GPU_INDEX" 0; then
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
    record PASS "logical family returns after disable" "${SLICED}.memory-percentage=${logicalpct} back; every per-profile partition key and ${PARTITIONED}.units gone — the card moved back to the logical population"
  else
    record FAIL "logical family returns after disable" "${SLICED}.memory-percentage='${logicalpct:-<absent>}', per-profile partition keys left=${partleft}, ${PARTITIONED}.units='${punits_left:-<absent>}' — the card did not return to logical slicing"
  fi
else
  record FAIL "disable MIG mode" "nvidia-smi -mig 0 did not converge to Disabled"
fi

# ===================================================================================================
# Results.
# ===================================================================================================
echo
echo "== CASE 23 — NVIDIA MIG dynamic-allocation lifecycle =="
print_rows

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). The operator observes MIG geometry and dynamically creates/destroys the"
  echo "GPU/compute instances backing scheduled Pods. Diagnose:"
  echo "  kubectl get devices ${GPU_NODE} -o yaml   # per-card physicalSliced profiles + RemainingProfiles"
  echo "  kubectl get node ${GPU_NODE} -o json | jq '.status.allocatable | with_entries(select(.key|test(\"sliced|partitioned\")))'"
  echo "  kubectl -n ${NS} logs ds/${DM_DS} --tail=200"
  echo "  ${MIG_NODE_SSH} sudo nvidia-smi mig -lgi   # the live GPU instances on the card"
  exit 1
fi
echo "CASE 23 PASS"
