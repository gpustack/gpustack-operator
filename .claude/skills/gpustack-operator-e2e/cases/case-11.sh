#!/usr/bin/env bash
#
# CASE 11 — Per-card logical-slice accounting: slices pack, and no card is over-committed   (MUTATING, self-recovering; AUTO-SKIPS without >=2 logically sliceable cards)
#
#   case-11.sh <NS>
#
# Goal:        ASSERTS the four properties per-card logical-slice accounting owes. The per-card ledger
#              records REAL units, so the SL view's remaining drops below capacity (it used to stay
#              ~full regardless of slice size) and the SL OnceMaxRequest stays per-card (<=100), not a
#              node card-sum. Placement follows from that ledger: claims that cannot share a card each
#              take a card of their own, a claim that fits beside an existing slice JOINS it rather
#              than opening a fresh card, and in neither case may a card's per-card VRAM limits sum
#              above its physical VRAM.
#
#              Packing is the point, not spreading. Spreading small slices over every card strands a
#              node with plenty of free memory unable to host one large claim. An earlier revision of
#              this case had it backwards — it was titled "slices spread across cards", fixed the slice
#              size above 50% so two could never share a card, and logged a per-card over-commit as
#              expected best-effort variance. That excused a real 120%-on-one-card over-commit on
#              2x RTX-4090 as a known limitation, when the cause was a malformed
#              GetPreferredAllocation hint kubelet discarded on every allocation.
# Environment: Needs REAL accelerator hardware with >=2 LOGICALLY SLICEABLE nvidia cards on one node —
#              a card in a hardware partitioning mode does not count, it serves no logical slice.
#              AUTO-SKIPS (exit 0) otherwise. Picks the InstanceType whose pool the target node belongs
#              to, and needs that pool to report its per-card memory (an empty Status.Detail.Memory is
#              the not-yet-ready state and would make the over-commit assertion vacuous, so the case
#              fails rather than passing on it).
#
#              The placement assertions presume the node's kubelet runs topologyManagerPolicy=none,
#              which is the suite's baseline. GetPreferredAllocation is advisory by API contract, and
#              under a restrictive policy kubelet allocates the NUMA-aligned set BEFORE it consults the
#              plugin, which can bypass the hint entirely. CASE 34 covers single-numa-node on its own.
# Inputs:      All real, nothing mocked. Two sequential passes pinned to the target node's entrance
#              LocalQueue, the node freed in between.
#                pass 1 — up to 4 x 60% claims (>50, so no two can share one card's VRAM).
#                pass 2 — 100% (fills the lowest card), then 50% (must go elsewhere), then the 100%
#                claim is DELETED and 30% is submitted. The only partly-used card is now at a higher
#                index than a free one, which is what tells packing apart from first-fit-by-index:
#                packing joins the 50% card, index order would take the freed low card.
# Expected:    - the 60% claims land on as many distinct cards as there are claims;
#              - the 50% claim does not land on the card the 100% claim filled;
#              - the 30% claim lands on the SAME card as the 50% claim;
#              - no card's per-card VRAM limits sum above its physical VRAM, in either pass;
#              - acceleratorSliced.remaining < acceleratorSliced.capacity (ledger is units-accurate);
#              - acceleratorSliced.onceMaxRequest <= 100 (per-card, not a node card-sum).
# Cleanup:     Trap deletes both passes' test Pods.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-11.sh <NS>}"
SLICED=nvidia.com/gpu.sliced
CORESPCT=nvidia.com/gpu.sliced.cores-percentage
MEMPCT=nvidia.com/gpu.sliced.memory-percentage
SPREADPFX=gpustack-e2e-spread
PACKPFX=gpustack-e2e-pack
SPREADPCT=60   # >50 so no two of these can share one card

# --- Skip gate: a node with >=2 LOGICALLY SLICEABLE nvidia cards (track its accelerator group id too,
#     so we pick the InstanceType whose pool this node belongs to). The per-card slicing CAPABILITY is
#     in Devices.spec (the runtime ledger in .status carries no capability), and only a card reporting a
#     logical slice count belongs to the logical population — a partitioned card is excluded. ---
read -r SLICE_NODE NCARDS GROUPID <<<"$(kubectl get devices -o json 2>/dev/null | python3 -c "
import json,sys
best=('',0,'')
for d in json.load(sys.stdin).get('items',[]):
    for g in d.get('spec',{}).get('groups',[]):
        if g.get('manufacturer')=='nvidia':
            n=sum(1 for a in g.get('accelerators',[])
                  if (a.get('status',{}).get('logicalSliced',{}).get('count',0) or 0)>0)
            if n>best[1]: best=(d['metadata']['name'], n, g.get('id',''))
print(best[0], best[1], best[2])
" 2>/dev/null)"
if [ -z "$SLICE_NODE" ] || [ "${NCARDS:-0}" -lt 2 ]; then
  echo "== CASE 11 — SKIPPED =="
  echo "No node reports >=2 logically sliceable nvidia cards (best='${SLICE_NODE:-none}', cards=${NCARDS:-0}) — this case"
  echo "needs real multi-card accelerator hardware, with the cards NOT in a hardware partitioning mode, to tell"
  echo "packing apart from spreading. Run it on such a node."
  exit 0
fi
# Bound the spread pass to at most 4 claims to keep it quick.
N="$NCARDS"; [ "$N" -gt 4 ] && N=4
echo "[case-11] target: ${SLICE_NODE} with ${NCARDS} logically sliceable card(s); ${N} x ${SPREADPCT}%, then 100%/50%/30%"

# The sliceable accelerated InstanceType whose POOL this node belongs to (its name carries the node's
# accelerator group id, e.g. "tesla-t4"), its entrance LocalQueue, and its per-card memory (MiB). This
# must match the target node, or the queue would route the slice to a different pool's nodes.
read -r IT LQ CARDMEM MANUF <<<"$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null | GID="$GROUPID" python3 -c "
import json,sys,os
gid=os.environ.get('GID','')
for it in json.load(sys.stdin).get('items',[]):
    s=it.get('spec',{}); name=it['metadata']['name']; st=it.get('status',{}); d=st.get('detail',{}); sd=d.get('slicedDetail',{})
    # LOGICALLY sliceable only: a hardware-partitioned card serves no logical slice.
    sliceable=(sd.get('logical',{}).get('count',0) or 0)>0
    if s.get('acceleratable') and sliceable and gid and gid in name:
        print(name, st.get('entrance',''), d.get('memory',''), d.get('manufacturer','')); break
")"
[ -n "$IT" ] && [ -n "$LQ" ] || { echo "no sliceable accelerated InstanceType with an entrance LocalQueue found"; exit 1; }
PHYS_MIB=$(python3 -c "
import re
m=re.match(r'\s*(\d+)\s*([GM])i?', '${CARDMEM}')
print(int(m.group(1))*(1024 if m.group(2)=='G' else 1) if m else 0)
" 2>/dev/null)
# The over-commit checks below are ASSERTIONS, and they compare against PHYS_MIB. A zero — an empty
# Status.Detail.Memory (the documented not-yet-ready state) or a form the regex above does not read —
# would make every one of them pass vacuously, which is precisely the failure this case exists to
# catch. Refuse to run instead.
if [ "${PHYS_MIB:-0}" -le 0 ]; then
  echo "== CASE 11 — FAILED (setup) =="
  echo "Pool ${IT} reports per-card memory '${CARDMEM:-<empty>}', which reads as ${PHYS_MIB:-0}MiB. The over-commit"
  echo "assertions would pass vacuously against a zero, so this case will not run. An empty value is the"
  echo "InstanceType's not-yet-ready state — let the pool settle and re-run."
  exit 1
fi
echo "[case-11] sliceable InstanceType ${IT} (card ${CARDMEM}=${PHYS_MIB}MiB) via LocalQueue ${LQ}"

# The logical-slicing runtime (HAMi libvgpu.so, LD_PRELOAD-injected at Allocate) needs the vendor
# runtimeClass to mount its driver-lib dependencies — a bare image without it exits 127. Derive it
# from the pool manufacturer (identity map: nvidia->nvidia, mthreads->mthreads); guard on existence.
RUNTIMECLASS=""
if [ -n "$MANUF" ] && kubectl get runtimeclass.node.k8s.io "$MANUF" >/dev/null 2>&1; then RUNTIMECLASS="$MANUF"; fi
RTC_LINE=""; [ -n "$RUNTIMECLASS" ] && RTC_LINE="runtimeClassName: ${RUNTIMECLASS}"
echo "[case-11] slice pods runtimeClass: ${RUNTIMECLASS:-<none>}"

delete_pods() { # delete_pods <prefix> <index>...
  local prefix="$1"; shift
  local i
  for i in "$@"; do
    kubectl -n default delete pod "${prefix}-$i" --ignore-not-found --force --grace-period=0 >/dev/null 2>&1 || true
  done
}

restore() {
  echo
  echo "[case-11] cleanup: deleting test Pods"
  delete_pods "$SPREADPFX" 1 2 3 4
  delete_pods "$PACKPFX" 1 2 3
}
trap restore EXIT

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
# verdict <cond-exit-code> <check> <object-on-pass> <object-on-fail>
verdict() { [ "$1" -eq 0 ] && record PASS "$2" "$3" || record FAIL "$2" "$4"; }

# SLICE_ROWS carries one "<card-uuid> <per-card MiB>" line per Pod placed, appended in submission
# order: the card the runtime actually confined the container to, and the VRAM ceiling it got there.
# The caller resets it at a pass boundary.
SLICE_ROWS=()

submit_slices() { # submit_slices <prefix> <first-index> <pct>...
  local prefix="$1" idx="$2"; shift 2
  local pct ok u m
  for pct in "$@"; do
    cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: { name: ${prefix}-${idx}, namespace: default, labels: { kueue.x-k8s.io/queue-name: ${LQ} } }
spec:
  schedulerName: default-scheduler
  ${RTC_LINE}
  restartPolicy: Never
  nodeSelector: { kubernetes.io/hostname: ${SLICE_NODE} }
  containers:
    - name: main
      image: ubuntu:24.04
      command: ["sleep", "86400"]
      resources:
        limits:   { ${SLICED}: "1", ${CORESPCT}: "${pct}", ${MEMPCT}: "${pct}" }
        requests: { ${SLICED}: "1", ${CORESPCT}: "${pct}", ${MEMPCT}: "${pct}" }
EOF
    ok=""
    for _ in $(seq 1 40); do
      [ "$(kubectl -n default get pod "${prefix}-${idx}" -o jsonpath='{.status.phase}' 2>/dev/null)" = Running ] && { ok=1; break; }
      sleep 3
    done
    [ -n "$ok" ] || { echo "[case-11] ${prefix}-${idx} (${pct}%) never reached Running"; return 1; }
    # Retry the read: a Pod reports Running slightly before its container is attachable, so an exec
    # issued the instant the poll above succeeds can come back empty — or fail outright with
    # `container not found` — while the placement itself is perfectly fine. Without this the case
    # aborts the whole pass on a timing artifact.
    u=""; m=""
    for _ in $(seq 1 8); do
      u=$(kubectl -n default exec "${prefix}-${idx}" -- printenv NVIDIA_VISIBLE_DEVICES 2>/dev/null)
      m=$(kubectl -n default exec "${prefix}-${idx}" -- printenv CUDA_DEVICE_MEMORY_LIMIT_0 2>/dev/null)
      [ -n "$u" ] && [ -n "$m" ] && break
      sleep 3
    done
    # Both are asserted on below, so an unreadable one must abort the pass rather than silently
    # skew the distinct-card count or hide an over-commit.
    [ -n "$u" ] && [ -n "$m" ] || {
      echo "[case-11] ${prefix}-${idx} (${pct}%): card='${u:-<empty>}' limit='${m:-<empty>}' — cannot read the placement back"
      return 1
    }
    SLICE_ROWS+=("${u} ${m%m}")
    echo "[case-11] ${prefix}-${idx} (${pct}%) → ${u}"
    idx=$((idx + 1))
    sleep 6   # let the per-card ledger reflect this placement before the next claim is submitted
  done
  return 0
}

# card_of <n> — the card the n-th (1-based) row in SLICE_ROWS names.
card_of() { awk '{print $1}' <<<"${SLICE_ROWS[$(($1 - 1))]}"; }

distinct_cards() {
  [ "${#SLICE_ROWS[@]}" -eq 0 ] && { echo 0; return; }
  printf '%s\n' "${SLICE_ROWS[@]}" | awk '{print $1}' | sort -u | grep -c .
}

# How many cards the placed slices over-committed: their per-card VRAM ceilings summing above the
# card's physical VRAM. This is the property the per-card ledger exists to guarantee.
overcommitted_cards() {
  [ "${#SLICE_ROWS[@]}" -eq 0 ] && { echo 0; return; }
  printf '%s\n' "${SLICE_ROWS[@]}" | PHYS="${PHYS_MIB}" python3 -c "
import sys,os,collections
phys=int(os.environ.get('PHYS','0')); agg=collections.defaultdict(int)
for line in sys.stdin:
    p=line.split()
    if len(p)<2: continue
    try: agg[p[0]]+=int(p[1])
    except: pass
print(sum(1 for s in agg.values() if phys>0 and s>phys))
"
}

# settle_ledger — wait for the SL view to report the pool fully free again, so the next pass starts
# from cards the previous one is no longer holding.
settle_ledger() {
  local srem scap
  for _ in $(seq 1 20); do
    srem=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.status.acceleratorSliced.remaining}' 2>/dev/null)
    scap=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.status.acceleratorSliced.capacity}' 2>/dev/null)
    [ -n "$srem" ] && [ "$srem" = "$scap" ] && return 0
    sleep 3
  done
  return 1
}

# --- Pass 1: claims that cannot share a card each take one of their own. ---
echo "[case-11] pass 1 — ${N} x ${SPREADPCT}% (no two fit on one card)"
SPREADPCTS=()
for _ in $(seq 1 "$N"); do SPREADPCTS+=("$SPREADPCT"); done
SLICE_ROWS=()
if submit_slices "$SPREADPFX" 1 "${SPREADPCTS[@]}"; then
  uniq_n=$(distinct_cards)
  over_n=$(overcommitted_cards)
  [ "${uniq_n:-0}" -eq "$N" ]; verdict $? "claims too large to share take distinct cards" \
    "${N} x ${SPREADPCT}% → ${uniq_n} distinct card(s)" \
    "${N} x ${SPREADPCT}% → only ${uniq_n:-?} distinct card(s) — two slices >50% cannot share a card's VRAM"
  [ "${over_n:-1}" -eq 0 ]; verdict $? "no card over-committed (spread pass)" \
    "0 of ${uniq_n} card(s) above ${PHYS_MIB}MiB" \
    "${over_n} card(s) above ${PHYS_MIB}MiB — the per-card guard did not hold"
else
  record FAIL "claims too large to share take distinct cards" "a ${SPREADPCT}% slice could not be placed and read back (reason logged above)"
  record FAIL "no card over-committed (spread pass)" "not evaluated — the spread pass did not complete"
fi

echo "[case-11] freeing the node before the packing pass"
delete_pods "$SPREADPFX" 1 2 3 4
settle_ledger || echo "[case-11] WARNING: the SL view did not return to fully free; the packing pass may misread"

# --- Pass 2: a claim that fits beside an existing slice must JOIN it, not open a fresh card.
#     The 100% claim fills the lowest card so the 50% claim is pushed onto a higher one; deleting the
#     100% claim then leaves the only partly-used card ABOVE a free one. That is what separates
#     packing from first-fit-by-index — index order would take the freed low card. ---
echo "[case-11] pass 2 — 100% then 50%, then free the 100% and submit 30%"
SLICE_ROWS=()
if submit_slices "$PACKPFX" 1 100 50; then
  block_card=$(card_of 1)
  host_card=$(card_of 2)
  [ "$host_card" != "$block_card" ]; verdict $? "a claim does not land on a card already filled" \
    "50% → ${host_card}, not the 100%-filled ${block_card}" \
    "50% → ${host_card} — the same card the 100% claim filled, so the card is over-committed"

  # Drop the 100% claim and its row: that container is gone, so its ceiling no longer applies, and
  # its card is free again — the state the join claim must decline to use.
  echo "[case-11] freeing the 100% claim, leaving ${host_card} half-used above a free card"
  delete_pods "$PACKPFX" 1
  SLICE_ROWS=("${SLICE_ROWS[1]}")
  sleep 12   # let the ledger release the filled card before the join claim is placed

  if submit_slices "$PACKPFX" 3 30; then
    join_card=$(card_of 2)
    [ "$join_card" = "$host_card" ]; verdict $? "a claim that fits joins the card already in use" \
      "30% → ${join_card}, joining the 50% claim and leaving the freed card whole" \
      "30% → ${join_card} but the 50% claim holds ${host_card} — it fits there, so opening another card burns one"
    over_p=$(overcommitted_cards)
    [ "${over_p:-1}" -eq 0 ]; verdict $? "no card over-committed (packing pass)" \
      "0 card(s) above ${PHYS_MIB}MiB (50% + 30% = 80%)" \
      "${over_p} card(s) above ${PHYS_MIB}MiB — packing must respect the per-card budget"
  else
    record FAIL "a claim that fits joins the card already in use" "the 30% join claim could not be placed and read back (reason logged above)"
    record FAIL "no card over-committed (packing pass)" "not evaluated — the join claim did not complete"
  fi

  # The SL view's remaining reflects the committed slices (not ~full) — per-card units ledger.
  srem=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.status.acceleratorSliced.remaining}' 2>/dev/null)
  scap=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.status.acceleratorSliced.capacity}' 2>/dev/null)
  { [ -n "$srem" ] && [ -n "$scap" ] && [ "$srem" -lt "$scap" ]; }; verdict $? "SL view reflects occupancy" \
    "acceleratorSliced remaining ${srem} < capacity ${scap}" \
    "remaining='${srem:-?}' not < capacity='${scap:-?}' (ledger not units-accurate)"
else
  record FAIL "a claim does not land on a card already filled" "the 100%/50% claims could not be placed and read back (reason logged above)"
  record FAIL "a claim that fits joins the card already in use" "not evaluated — the packing pass did not start"
  record FAIL "no card over-committed (packing pass)" "not evaluated — the packing pass did not start"
  record FAIL "SL view reflects occupancy" "not evaluated — the packing pass did not start"
fi

# The SL OnceMaxRequest is per-card (<=100), not a node card-sum.
sorm=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.status.acceleratorSliced.onceMaxRequest}' 2>/dev/null)
{ [ -n "$sorm" ] && [ "$sorm" -le 100 ]; }; verdict $? "SL OnceMaxRequest is per-card (<=100)" \
  "acceleratorSliced onceMaxRequest ${sorm}" \
  "onceMaxRequest='${sorm:-?}' > 100 (node card-sum, not per-card)"

echo
echo "== CASE 11 — Per-card logical-slice accounting: slices pack, and no card is over-committed =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). Placement is decided by the device plugin's GetPreferredAllocation hint;"
  echo "a hint kubelet cannot read is silently discarded and every card choice becomes arbitrary, which"
  echo "shows up here as a claim that should have joined an in-use card opening a fresh one, or a card"
  echo "over-committed. Raise the device-manager's verbosity BEFORE re-running to see the decision (see"
  echo "the shared troubleshooting reference, 'Component log verbosity')."
  echo "Diagnose: kubectl get devices ${SLICE_NODE} -o yaml; kubectl -n ${NS} logs ds/gpustack-operator-device-manager-nvidia --tail=100"
  exit 1
fi
echo "CASE 11 PASS"
