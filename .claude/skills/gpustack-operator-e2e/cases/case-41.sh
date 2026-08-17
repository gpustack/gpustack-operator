#!/usr/bin/env bash
#
# CASE 41 — The slice pass reads only the carved cards: a whole card on the same node is never queried
#   (MUTATING, self-recovering; AUTO-SKIPS without >=2 logically sliceable cards on one node)
#
#   case-41.sh <NS>
#
# Goal:        ASSERTS that the per-process pass behind /monitor/snapshot is scoped to the accelerators
#              something was actually carved on, not swept across the node. One Instance takes a WHOLE
#              card and another takes a logical SLICE of a different card of the SAME node; the slice
#              section must then carry a diagnostics entry for the carved card and NONE for the whole
#              one. That entry list is the producer's own statement of what it covered — "an accelerator
#              absent from here was not covered at all" — so its shape is the observable that tells a
#              scoped pass from a node-wide one.
#
#              Why it matters: the pass reads /proc to attribute per-process device memory to an
#              Instance's Pod and container. A pass that queried every card of the node would read
#              processes on cards nobody sliced — a whole-card tenant's, or a host process's — and
#              every one of those rows is somebody else's. They cannot become a slice figure, so at
#              best they inflate rowsAmbiguous/rowsNonInstance on a device that owes no slice figure at
#              all, and at worst they land on a slice that does. Not querying the card is what makes
#              that structurally impossible rather than merely filtered.
# Environment: A reachable cluster with a materialized scheduling chain (run case-1 first) and REAL
#              accelerator hardware: one node with >=2 LOGICALLY SLICEABLE cards of one manufacturer,
#              so a whole card and a carved card can coexist there. A card in a hardware partitioning
#              mode does not count — it serves no logical slice.
#
#              The manufacturer must also serve the per-process pass. The slice section is built only
#              for a manufacturer whose detector implements AcceleratorProcessDetector — the collector
#              skips every other one — so a manufacturer without it advertises logical slices while
#              covering no device at all, and this case would wait out its timeout on a cluster that is
#              behaving correctly. PROCESS_MANUFACTURERS mirrors which detectors ship a process.go;
#              a manufacturer outside it AUTO-SKIPS (exit 0), as does hardware that is missing.
# Inputs:      All real, nothing mocked — two Instances on the node's own pool: gpustack-e2e-scope-whole
#              (accelerator: 1, no slice percentages, so a whole card) and gpustack-e2e-scope-sliced
#              (accelerator: 1 at 50% memory / 100% cores, so a logical slice). Both carry
#              spec.nodeName, so they hold cards of the node whose device manager is scraped — a pool
#              spanning nodes would otherwise place them anywhere in it while the snapshot is read from
#              one node, and every check would fail on a placement that is perfectly correct.
#
#              The card each Instance was granted is read from the Instance metrics subresource
#              (sample.accelerators[].id) rather than from the vendor visible-devices env, because that
#              env is not one identity across vendors: an AMD whole-card grant carries only a bare
#              serial in AMD_VISIBLE_DEVICES while its sliced grant also carries "GPU-<serial>" in
#              ROCR_VISIBLE_DEVICES, and Cambricon's carries driver indexes. The subresource speaks the
#              same device ids the snapshot keys on, which is what these checks compare.
#
#              Optionally E2E_WHOLE_LOAD_IMAGE (+ E2E_WHOLE_LOAD_COMMAND) replaces the whole-card
#              Instance's image with one that really allocates device memory, which is what turns the
#              last check from structural into measured; that check SKIPS without it.
# Expected:    - the two Instances hold DIFFERENT cards of the scraped node (setup precondition, asserted);
#              - slices.devices carries the carved card;
#              - slices.devices does NOT carry the whole card;
#              - slices.devices carries exactly the carved cards and nothing else;
#              - no slices.usages record names the whole card;
#              - with a load image: the whole card is STILL absent from slices.devices while its
#                processes are running, and the carved card's rowsNonInstance stays 0 — the load on the
#                unsliced card reached neither list.
# Cleanup:     Trap deletes both test Instances; idempotent, runs on pass AND fail, safe to re-run.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-41.sh <NS>}"
WHOLE=gpustack-e2e-scope-whole
SLICED=gpustack-e2e-scope-sliced
SLICEPCT=50
DMPORT=32443

# PROCESS_MANUFACTURERS are the manufacturers whose detector implements AcceleratorProcessDetector —
# one per pkg/devicemanager/detector/<manufacturer>/process.go. The monitor's slice pass builds a
# section only for those and skips every other manufacturer, so one outside this set advertises logical
# slices while covering no device: there would be nothing for this case to read, and a timeout would be
# reported as a defect. Keep it in step with those files; a manufacturer missing from it only skips.
PROCESS_MANUFACTURERS="nvidia amd ascend cambricon iluvatar metax hygon thead"

# --- Skip gate: a node with >=2 LOGICALLY SLICEABLE cards of one ELIGIBLE manufacturer, plus that
#     group's id so the pool's InstanceType can be picked by identity. The per-card slicing CAPABILITY
#     lives in Devices.spec; only a card reporting a logical slice count belongs to the logical
#     population, which excludes a hardware-partitioned one. ---
read -r NODE NCARDS GROUPID MANUF <<<"$(kubectl get devices -o json 2>/dev/null | MFRS="$PROCESS_MANUFACTURERS" python3 -c "
import json,sys,os
eligible=set(os.environ.get('MFRS','').split())
best=('',0,'','')
for d in json.load(sys.stdin).get('items',[]):
    for g in d.get('spec',{}).get('groups',[]):
        if g.get('manufacturer') not in eligible: continue
        n=sum(1 for a in g.get('accelerators',[])
              if (a.get('status',{}).get('logicalSliced',{}).get('count',0) or 0)>0)
        if n>best[1]: best=(d['metadata']['name'], n, g.get('id',''), g.get('manufacturer',''))
print(best[0], best[1], best[2], best[3])
" 2>/dev/null)"
if [ -z "$NODE" ] || [ "${NCARDS:-0}" -lt 2 ]; then
  echo "== CASE 41 — SKIPPED =="
  echo "No node reports >=2 logically sliceable cards of one manufacturer that also serves the per-process"
  echo "pass (best='${NODE:-none}', cards=${NCARDS:-0}; eligible: ${PROCESS_MANUFACTURERS})."
  echo "A whole card and a carved card must coexist on ONE node for the slice section to have something"
  echo "to leave out, and the manufacturer must implement AcceleratorProcessDetector or the section"
  echo "covers none of its cards. Run on a node with two sliceable cards of an eligible manufacturer."
  exit 0
fi
echo "[case-41] target: ${NODE} with ${NCARDS} logically sliceable ${MANUF} card(s)"

# The sliceable accelerated InstanceType whose POOL this node belongs to, matched on the pool's identity
# tuple — spec.acceleratorGroup ("<manufacturer>-<group id>") plus spec.os/spec.arch, which is what the
# controller keys a pool by. See the index's note on pool lookup for what a looser match selects. Both
# Instances go to this one type: the whole card and the slice have to be drawn from the same pool for
# "different cards of one node" to be the thing measured.
read -r IT CARDMEM <<<"$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null | NODE_GID="$GROUPID" NODE_JSON="$(kubectl get node "$NODE" -o json 2>/dev/null)" python3 -c "
import json,sys,os
gid=os.environ.get('NODE_GID','')
nl=(json.loads(os.environ.get('NODE_JSON') or '{}').get('metadata',{}) or {}).get('labels',{}) or {}
# A pool BACKS this node when every discriminator it carries is a label the node carries too. Those are
# the schedule labels the webhook stamps from PoolScheduleLabels, so this is the pool's whole identity —
# os/arch, the acceleratable boolean, the accelerator group and, under CPU-aware grouping, the general
# group. schedule.gpustack.ai/* is the pool's own bookkeeping and never a node label.
def backs(it):
    d={k:v for k,v in (it['metadata'].get('labels') or {}).items() if not k.startswith('schedule.gpustack.ai/')}
    return bool(d) and all(nl.get(k)==v for k,v in d.items())
def in_group(s):
    return bool(gid) and (s.get('acceleratorGroup') or '').endswith('-'+gid)
for it in json.load(sys.stdin).get('items',[]):
    s=it.get('spec',{}); d=it.get('status',{}).get('detail',{})
    sliceable=(d.get('slicedDetail',{}).get('logical',{}).get('count',0) or 0)>0
    if s.get('acceleratable') and sliceable and in_group(s) and backs(it):
        print(it['metadata']['name'], d.get('memory','')); break
")"
[ -n "$IT" ] || { echo "no sliceable accelerated InstanceType backs ${NODE} in group '${GROUPID}'"; exit 1; }
echo "[case-41] pool InstanceType ${IT} (card ${CARDMEM:-<empty>})"

# The device manager of this node and this manufacturer serves the snapshot. Several manufacturers can
# run a device manager on one node, and only this one's covers these cards.
DM=$(kubectl -n "$NS" get pod -l app.kubernetes.io/component=device-manager \
  --field-selector "spec.nodeName=${NODE}" -o json 2>/dev/null \
  | MF="$MANUF" python3 -c "
import json,sys,os
mf=os.environ.get('MF','')
for p in json.load(sys.stdin).get('items',[]):
    if mf and mf in p['metadata']['name'] and p.get('status',{}).get('phase')=='Running':
        print(p['metadata']['name']); break
" 2>/dev/null)
if [ -z "$DM" ]; then
  echo "== CASE 41 — SKIPPED =="
  echo "No Running ${MANUF} device manager pod on ${NODE} — nothing serves /monitor/snapshot there."
  exit 0
fi
echo "[case-41] device manager ${DM}"

restore() {
  echo
  echo "[case-41] cleanup: deleting test Instances"
  kubectl -n default delete instance "$WHOLE" "$SLICED" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap restore EXIT

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
# verdict <cond-exit-code> <check> <object-on-pass> <object-on-fail>
verdict() { [ "$1" -eq 0 ] && record PASS "$2" "$3" || record FAIL "$2" "$4"; }

LOAD_IMAGE="${E2E_WHOLE_LOAD_IMAGE:-}"
LOAD_CMD="${E2E_WHOLE_LOAD_COMMAND:-}"

mkinstance() { # mkinstance <name> <image> <command-json> [slice-pct]
  local pct="${4:-}" sliced=""
  if [ -n "$pct" ]; then
    sliced=$(printf '\n    acceleratorSlicedMemoryPercentage: %s\n    acceleratorSlicedCoresPercentage: 100' "$pct")
  fi
  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: Instance
metadata: { name: $1, namespace: default }
spec:
  type: ${IT}
  nodeName: ${NODE}
  image: $2
  command: $3
  resources:
    cpu: "1"
    ram: "4Gi"
    localStorage: "10Gi"
    accelerator: "1"${sliced}
  volume: { ephemeral: { capacity: 5Gi } }
  volumeMount: /workspace
EOF
}

wait_running() { for _ in $(seq 1 60); do
  [ "$(kubectl -n default get pod "$1" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Running" ] && return 0
  sleep 3
done; return 1; }

# granted_card <instance> — the device id of the accelerator this Instance was granted, retried.
#
# Read from the Instance metrics subresource, which names the accelerator with the same device id the
# monitor snapshot keys its slice records on. That shared identity is what these checks compare, and the
# vendor visible-devices env is not it: each vendor spells its own, and on AMD a whole-card grant carries
# a bare serial while a sliced grant of the same card also carries "GPU-<serial>", so comparing one
# against the other would fail on a correct placement.
#
# The subresource answers before its first sample carries an accelerator, so an empty read is a timing
# artifact rather than an absent grant; reading it as "no card" would report a defect on a placement that
# is perfectly correct. This insists on a value.
granted_card() {
  local out
  for _ in $(seq 1 20); do
    out=$(kubectl get --raw "/apis/worker.gpustack.ai/v1/namespaces/default/instances/$1/metrics" 2>/dev/null \
      | python3 -c "
import json,sys
try: s=json.load(sys.stdin)
except Exception: sys.exit(0)
for a in (s.get('sample') or {}).get('accelerators') or []:
    if a.get('id'): print(a['id']); break
" 2>/dev/null)
    [ -n "$out" ] && { printf '%s' "$out"; return 0; }
    sleep 3
  done
  return 1
}

# snapshot_devices — the deviceID of every entry in the snapshot's slice diagnostics list.
snapshot_devices() {
  kubectl get --raw "/api/v1/namespaces/${NS}/pods/https:${DM}:${DMPORT}/proxy/monitor/snapshot" 2>/dev/null \
    | python3 -c "
import json,sys
try: s=json.load(sys.stdin)
except Exception: sys.exit(0)
for d in (s.get('slices') or {}).get('devices') or []:
    print(d.get('deviceID',''))
" 2>/dev/null
}

# snapshot_usage_devices — the deviceID of every measured per-container usage record.
snapshot_usage_devices() {
  kubectl get --raw "/api/v1/namespaces/${NS}/pods/https:${DM}:${DMPORT}/proxy/monitor/snapshot" 2>/dev/null \
    | python3 -c "
import json,sys
try: s=json.load(sys.stdin)
except Exception: sys.exit(0)
for u in (s.get('slices') or {}).get('usages') or []:
    print(u.get('deviceID',''))
" 2>/dev/null
}

# snapshot_non_instance <deviceID> — that device's rowsNonInstance, or empty when it is not covered.
snapshot_non_instance() {
  kubectl get --raw "/api/v1/namespaces/${NS}/pods/https:${DM}:${DMPORT}/proxy/monitor/snapshot" 2>/dev/null \
    | DEV="$1" python3 -c "
import json,sys,os
dev=os.environ.get('DEV','')
try: s=json.load(sys.stdin)
except Exception: sys.exit(0)
for d in (s.get('slices') or {}).get('devices') or []:
    if d.get('deviceID')==dev: print(d.get('rowsNonInstance',0)); break
" 2>/dev/null
}

echo "[case-41] creating one whole-card and one ${SLICEPCT}% sliced Instance on ${IT}"
mkinstance "$WHOLE"  "${LOAD_IMAGE:-ubuntu:24.04}" "${LOAD_CMD:-[\"sleep\", \"infinity\"]}"
mkinstance "$SLICED" "ubuntu:24.04" '["sleep", "infinity"]' "$SLICEPCT"

if ! wait_running "$WHOLE" || ! wait_running "$SLICED"; then
  echo "== CASE 41 — SKIPPED =="
  echo "The two Instances did not both reach Running on ${NODE}: whole=$(kubectl -n default get pod "$WHOLE" -o jsonpath='{.status.phase}' 2>/dev/null), sliced=$(kubectl -n default get pod "$SLICED" -o jsonpath='{.status.phase}' 2>/dev/null)."
  echo "A whole card and a carved card must both be held for the slice section to have something to"
  echo "leave out, so there is nothing to measure here. Free the node's cards and re-run."
  exit 0
fi

WHOLE_NODE=$(kubectl -n default get pod "$WHOLE" -o jsonpath='{.spec.nodeName}' 2>/dev/null)
SLICED_NODE=$(kubectl -n default get pod "$SLICED" -o jsonpath='{.spec.nodeName}' 2>/dev/null)
if [ "$WHOLE_NODE" != "$NODE" ] || [ "$SLICED_NODE" != "$NODE" ]; then
  echo "== CASE 41 — FAILED (setup) =="
  echo "Both Instances carry spec.nodeName=${NODE}, but they run on '${WHOLE_NODE:-<none>}' and"
  echo "'${SLICED_NODE:-<none>}'. The snapshot below is read from ${NODE}'s device manager, so the"
  echo "comparison would be made against a node that holds neither card. Node pinning did not take."
  exit 1
fi

WHOLE_CARD=$(granted_card "$WHOLE")  || { echo "== CASE 41 — FAILED (setup) =="; echo "the whole-card Instance's metrics subresource reported no accelerator"; exit 1; }
SLICED_CARD=$(granted_card "$SLICED") || { echo "== CASE 41 — FAILED (setup) =="; echo "the sliced Instance's metrics subresource reported no accelerator"; exit 1; }
echo "[case-41] whole card  -> ${WHOLE_CARD}"
echo "[case-41] sliced card -> ${SLICED_CARD}"

# Setup precondition, asserted rather than assumed: the whole card and the carved card must be
# different, or "the whole one is absent from the slice list" would be asking the carved card to be
# absent from its own list. Cross-mode separation itself is CASE 22's contract.
[ "$WHOLE_CARD" != "$SLICED_CARD" ]
verdict $? "the two Instances hold different cards of one node" \
  "${WHOLE_NODE}: whole=${WHOLE_CARD} sliced=${SLICED_CARD}" \
  "both landed on ${WHOLE_CARD} — cross-mode separation broke (CASE 22), nothing to measure here"
if [ "$WHOLE_CARD" = "$SLICED_CARD" ]; then
  echo
  echo "== CASE 41 — Slice pass reads only the carved cards =="
  { echo "STATUS|CHECK|OBJECT"; printf '%s\n' "${ROWS[@]}"; } | column -t -s '|'
  echo "CASE 41 FAILED (setup)"
  exit 1
fi

# One monitor tick has to have run since the slice was placed, or the section reflects the node as it
# was before and every check below reads a stale list.
echo "[case-41] waiting for a monitor tick that covers the carved card"
for _ in $(seq 1 40); do
  snapshot_devices | grep -qx "$SLICED_CARD" && break
  sleep 3
done

DEVS=$(snapshot_devices)
USES=$(snapshot_usage_devices)
NDEVS=$(printf '%s' "$DEVS" | grep -c . || true)
echo "[case-41] slices.devices carries ${NDEVS} entry/entries: $(printf '%s' "$DEVS" | tr '\n' ' ')"

printf '%s\n' "$DEVS" | grep -qx "$SLICED_CARD"
verdict $? "the carved card is covered by the slice pass" \
  "slices.devices carries ${SLICED_CARD}" \
  "slices.devices has no entry for the carved card ${SLICED_CARD} — the pass did not cover it at all"

! printf '%s\n' "$DEVS" | grep -qx "$WHOLE_CARD"
verdict $? "the whole card is never queried" \
  "slices.devices has no entry for ${WHOLE_CARD}" \
  "slices.devices carries the whole card ${WHOLE_CARD} — the pass swept the node instead of the carved cards"

[ "${NDEVS:-0}" -eq 1 ]
verdict $? "the slice pass covered exactly the carved cards" \
  "1 entry for 1 carved card" \
  "${NDEVS} entries for 1 carved card: $(printf '%s' "$DEVS" | tr '\n' ' ')"

! printf '%s\n' "$USES" | grep -qx "$WHOLE_CARD"
verdict $? "no usage record names the whole card" \
  "slices.usages carries no record on ${WHOLE_CARD}" \
  "slices.usages carries a record on the whole card ${WHOLE_CARD} — a whole-card tenant's processes became slice usage"

# The measured half of the contract. Without a load image the whole-card container holds the card but
# runs no device process, so "its processes reached neither list" has no processes to speak of and the
# check would pass on nothing.
if [ -n "$LOAD_IMAGE" ]; then
  NONINST=$(snapshot_non_instance "$SLICED_CARD")
  ! printf '%s\n' "$(snapshot_devices)" | grep -qx "$WHOLE_CARD"
  verdict $? "a real load on the whole card reaches neither list" \
    "${WHOLE_CARD} still absent from slices.devices while ${LOAD_IMAGE} runs on it" \
    "${WHOLE_CARD} appeared in slices.devices once a process held it — the pass follows processes, not carvings"
  [ "${NONINST:-0}" = "0" ]
  verdict $? "the carved card attributed no foreign row" \
    "rowsNonInstance=0 on ${SLICED_CARD}" \
    "rowsNonInstance=${NONINST:-?} on ${SLICED_CARD} — a row from outside its Instances was read against it"
else
  record SKIP "a real load on the whole card reaches neither list" \
    "set E2E_WHOLE_LOAD_IMAGE (a workload that allocates device memory) to measure this rather than assert its shape"
fi

echo
echo "== CASE 41 — The slice pass reads only the carved cards: a whole card on the same node is never queried =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). slices.devices is the producer's own statement of which accelerators the"
  echo "per-process pass covered, so a whole card appearing there means the pass was scoped to the node"
  echo "rather than to what was carved on it. Read the section directly and compare against the"
  echo "allocations the node actually holds:"
  echo "Diagnose: kubectl get --raw \"/api/v1/namespaces/${NS}/pods/https:${DM}:${DMPORT}/proxy/monitor/snapshot\" | python3 -m json.tool"
  echo "          kubectl get devices ${NODE} -o yaml"
  echo "          kubectl -n ${NS} logs ${DM} --tail=200"
  exit 1
fi
echo "CASE 41 PASS"
