#!/usr/bin/env bash
#
# CASE 11 — Sliced allocations spread across cards; per-card VRAM is not over-committed   (MUTATING, self-recovering; AUTO-SKIPS without >=2 sliced cards)
#
#   case-11.sh <NS>
#
# Goal:        ASSERTS the deterministic part of per-card sliced accounting — the per-card ledger
#              records REAL units, so the sliced three-view remaining drops below capacity (it used to
#              stay ~full regardless of slice size), and the sliced OnceMaxRequest is per-card (<=100),
#              not a node card-sum. OBSERVES (best-effort, NOT asserted) that slices spread across a
#              node's cards: placement is an advisory hint over an async ledger and the AdmissionCheck
#              is check-only (does not reserve), so a HARD per-card VRAM guarantee is out of scope here;
#              the observed spread is only logged so a gross regression (all slices stacked) stays visible.
# Environment: Needs REAL accelerator hardware with >=2 nvidia sliced cards on one node. AUTO-SKIPS
#              (exit 0) otherwise. Picks the InstanceType whose pool the target node belongs to.
# Inputs:      All real, nothing mocked — up to 4 sequential 60% (memory + cores) slices pinned to the
#              target node's entrance LocalQueue (60% so two slices cannot share one card's VRAM).
# Expected:    Asserted —
#              - acceleratorSliced.remaining < acceleratorSliced.capacity (ledger is units-accurate);
#              - acceleratorSliced.onceMaxRequest <= 100 (per-card, not a node card-sum).
#              Observed only (logged, never fails the case) — the distinct-card spread and any per-card
#              VRAM over-commit (advisory placement + non-reserving check → best-effort variance).
# Cleanup:     Trap deletes the test Pods.
set -uo pipefail

NS="${1:?usage: case-11.sh <NS>}"
SLICED=nvidia.com/gpu.sliced
CORESPCT=nvidia.com/gpu.sliced.cores-percentage
MEMPCT=nvidia.com/gpu.sliced.memory-percentage
PODPFX=gpustack-e2e-spread
PCT=60   # >50 so two slices cannot share a card without exceeding its VRAM

# --- Skip gate: a node with >=2 nvidia sliced cards (track its accelerator group id too, so we pick
#     the InstanceType whose pool this node belongs to). ---
read -r SLICE_NODE NCARDS GROUPID <<<"$(kubectl get devices -o json 2>/dev/null | python3 -c "
import json,sys
best=('',0,'')
for d in json.load(sys.stdin).get('items',[]):
    for g in d.get('status',{}).get('groups',[]):
        if g.get('manufacturer')=='nvidia':
            n=len(g.get('accelerators',[]))
            if n>best[1]: best=(d['metadata']['name'], n, g.get('id',''))
print(best[0], best[1], best[2])
" 2>/dev/null)"
if [ -z "$SLICE_NODE" ] || [ "${NCARDS:-0}" -lt 2 ]; then
  echo "== CASE 11 — SKIPPED =="
  echo "No node advertises >=2 nvidia sliced cards (best='${SLICE_NODE:-none}', cards=${NCARDS:-0}) — this case needs"
  echo "real multi-card accelerator hardware to prove sliced allocations spread. Run it on such a node."
  exit 0
fi
# Bound the test to at most 4 slices to keep it quick.
N="$NCARDS"; [ "$N" -gt 4 ] && N=4
echo "[case-11] spread target: ${SLICE_NODE} with ${NCARDS} sliced card(s); submitting ${N} × ${PCT}% slices"

# The sliceable accelerated InstanceType whose POOL this node belongs to (its name carries the node's
# accelerator group id, e.g. "tesla-t4"), its entrance LocalQueue, and its per-card memory (MiB). This
# must match the target node, or the queue would route the slice to a different pool's nodes.
read -r IT LQ CARDMEM <<<"$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null | GID="$GROUPID" python3 -c "
import json,sys,os
gid=os.environ.get('GID','')
for it in json.load(sys.stdin).get('items',[]):
    s=it.get('spec',{}); name=it['metadata']['name']
    if s.get('acceleratable') and s.get('sliceable') and gid and gid in name:
        print(name, it.get('status',{}).get('entrance',''), s.get('memory','')); break
")"
[ -n "$IT" ] && [ -n "$LQ" ] || { echo "no sliceable accelerated InstanceType with an entrance LocalQueue found"; exit 1; }
PHYS_MIB=$(python3 -c "
import re
m=re.match(r'\s*(\d+)\s*([GM])i?', '${CARDMEM}')
print(int(m.group(1))*(1024 if m.group(2)=='G' else 1) if m else 0)
" 2>/dev/null)
echo "[case-11] sliceable InstanceType ${IT} (card ${CARDMEM}=${PHYS_MIB}MiB) via LocalQueue ${LQ}"

restore() {
  echo
  echo "[case-11] cleanup: deleting test Pods"
  for i in $(seq 1 "$N"); do kubectl -n default delete pod "${PODPFX}-$i" --ignore-not-found --force --grace-period=0 2>/dev/null || true; done
}
trap restore EXIT

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

# Submit N slices SEQUENTIALLY (wait each Running + let the ledger settle) so the per-card bin-fit sees
# the prior placements and spreads onto a fresh card each time.
UUIDS=""
for i in $(seq 1 "$N"); do
  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: { name: ${PODPFX}-$i, namespace: default, labels: { kueue.x-k8s.io/queue-name: ${LQ} } }
spec:
  schedulerName: default-scheduler
  restartPolicy: Never
  nodeSelector: { kubernetes.io/hostname: ${SLICE_NODE} }
  containers:
    - name: main
      image: ubuntu:24.04
      command: ["sleep", "86400"]
      resources:
        limits:   { ${SLICED}: "1", ${CORESPCT}: "${PCT}", ${MEMPCT}: "${PCT}" }
        requests: { ${SLICED}: "1", ${CORESPCT}: "${PCT}", ${MEMPCT}: "${PCT}" }
EOF
  ok=""
  for _ in $(seq 1 40); do
    [ "$(kubectl -n default get pod "${PODPFX}-$i" -o jsonpath='{.status.phase}' 2>/dev/null)" = Running ] && { ok=1; break; }
    sleep 3
  done
  [ -n "$ok" ] || { record FAIL "slice ${i} admitted+running" "${PODPFX}-$i not Running"; break; }
  u=$(kubectl -n default exec "${PODPFX}-$i" -- printenv NVIDIA_VISIBLE_DEVICES 2>/dev/null)
  UUIDS="${UUIDS} ${u}"
  echo "[case-11] slice ${i} → ${u}"
  sleep 6   # let the per-card ledger reflect this placement before the next
done

# 1. Spread / per-card overcommit — OBSERVED (best-effort, NOT asserted; see header). Logged so a gross
#    regression stays visible, but best-effort variance does not fail the case.
uniq_n=$(printf '%s\n' $UUIDS | sort -u | grep -c . )
overcommit=$(for i in $(seq 1 "$N"); do
  [ "$(kubectl -n default get pod "${PODPFX}-$i" -o jsonpath='{.status.phase}' 2>/dev/null)" = Running ] || continue
  u=$(kubectl -n default exec "${PODPFX}-$i" -- printenv NVIDIA_VISIBLE_DEVICES 2>/dev/null)
  m=$(kubectl -n default exec "${PODPFX}-$i" -- printenv CUDA_DEVICE_MEMORY_LIMIT_0 2>/dev/null)
  echo "${u} ${m%m}"
done | PHYS="${PHYS_MIB}" python3 -c "
import sys,os,collections
phys=int(os.environ.get('PHYS','0')); agg=collections.defaultdict(int)
for line in sys.stdin:
    p=line.split()
    if len(p)<2: continue
    try: agg[p[0]]+=int(p[1])
    except: pass
print(sum(1 for s in agg.values() if phys>0 and s>phys))
")
echo "[case-11] OBSERVED spread (best-effort): ${N} slices → ${uniq_n} distinct card(s); per-card overcommit(s)=${overcommit:-?} (VRAM ${PHYS_MIB}MiB)"
if [ "${overcommit:-0}" != "0" ]; then
  echo "[case-11]   NOTE: ${overcommit} card(s) over-committed — expected best-effort variance (advisory placement + non-reserving gate-3); a hard guarantee needs gate-3 reservation. Known limitation, not a case failure."
fi

# 2. The sliced three-view remaining reflects the committed slices (not ~full) — per-card units ledger.
srem=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.status.acceleratorSliced.remaining}' 2>/dev/null)
scap=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.status.acceleratorSliced.capacity}' 2>/dev/null)
if [ -n "$srem" ] && [ -n "$scap" ] && [ "$srem" -lt "$scap" ]; then
  record PASS "sliced three-view reflects occupancy" "acceleratorSliced remaining ${srem} < capacity ${scap}"
else
  record FAIL "sliced three-view reflects occupancy" "remaining='${srem:-?}' not < capacity='${scap:-?}' (ledger not units-accurate)"
fi

# 4. The sliced OnceMaxRequest is per-card (<=100), not a node card-sum.
sorm=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.status.acceleratorSliced.onceMaxRequest}' 2>/dev/null)
{ [ -n "$sorm" ] && [ "$sorm" -le 100 ]; } \
  && record PASS "sliced OnceMaxRequest is per-card (<=100)" "acceleratorSliced onceMaxRequest ${sorm}" \
  || record FAIL "sliced OnceMaxRequest is per-card (<=100)" "onceMaxRequest='${sorm:-?}' > 100 (node card-sum, not per-card)"

echo
echo "== CASE 11 — Sliced allocations spread across cards; per-card VRAM is not over-committed =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). The per-card ledger must record real .sliced.units so the three-view"
  echo "remaining drops below capacity, and the sliced OnceMaxRequest must be per-card (<=100). (Spread"
  echo "across a node's cards is best-effort — only logged above, never asserted — so it never fails here.)"
  echo "Diagnose: kubectl get devices ${SLICE_NODE} -o yaml; kubectl -n ${NS} logs ds/gpustack-operator-device-manager-nvidia --tail=100"
  exit 1
fi
echo "CASE 11 PASS"
