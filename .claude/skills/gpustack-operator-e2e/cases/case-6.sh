#!/usr/bin/env bash
#
# CASE 6 — Pooled three-view + watch freshness (Direction 2)   (MUTATING, self-recovering)
#
#   case-6.sh <NS>
#
# Verifies the materialized-InstanceType refactor
# (specs/2026-06-29-instancetype-unified-pool-refactor.md, T5.1/T5.4/T5.5):
#
#   1. The five-step pooling sequence on an 8× A10G (24Gi) pool drives the InstanceType
#      three-view (.status.accelerator / .acceleratorShared / .acceleratorSliced) exactly
#      through 8/80/800 → 6/60/600 → 4/58/400 → 2/38/360 → 2/38/356 → 1/28/256.
#   2. Watch freshness — a pod alloc/free moves the observed three-view within a reconcile,
#      seen live over `kubectl get instancetype -w` (the whole point of promoting InstanceType
#      to a real CRD: a native watch delivers the .status change, the old aggregated
#      projection could not).
#   3. The unit spec is frozen after create (unitResources / localStorage immutable on update): an
#      edit is rejected by the validating webhook. The unit spec lives only on the InstanceType —
#      never a ClusterQueue note or a Node — and its write path touches NO Node / NodeFeature.
#   4. Zero Cohort objects exist (Cohort was removed entirely: one isolated CQ per pool).
#
# Runs on a GPU-LESS cluster BY APPROXIMATION, in the same spirit as CASE 5. The three-view
# is computed by InstanceTypeReconciler from the per-card `Devices` CR ledger
# (status.groups[].accelerators[].{mode,remaining}), which a real per-node DeviceManager
# writes from real accelerators. A GPU-less node has none, so two inputs are mocked:
#   1. The accelerator feature labels (fake NodeFeature → NFD merge → Node.Labels), so the
#      real Worker derives the accelerated ResourceFlavor → ClusterQueue → InstanceType.
#   2. A per-card `Devices` CR ledger. It is created under a PHANTOM node name the
#      DeviceManager DaemonSet never runs on (and NodeDevicesReconciler leaves a Devices with
#      no matching Node untouched), so our mocked status.groups is stable and never fought.
#      It carries the pool's feature-key + kubernetes.io/os|arch + gpustack.ai/managed=true
#      labels so the reconciler reverse-looks-it-up (poolDevicesSelector).
# NOT mocked on purpose — that IS the verification: the flavor/CQ/InstanceType derivation
# (real NodeFlavor/InstanceType reconcilers) and the three-view bin-packing math the real
# InstanceTypeReconciler runs over our mocked ledger.
#
# Self-recovering: deletes the mocked Devices CR and the injected NodeFeature on exit (trap);
# removing the NodeFeature drains the derived flavor, so the InstanceType + CQ tear down
# themselves.
set -uo pipefail

NS="${1:?usage: case-6.sh <NS>}"
NODE=$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')
AKEY=nvidia-e2emock                                # non-colliding fake key (never a real product) so the mocked pool
                                                   # stays isolated on a real-accelerator cluster — mirrors case-4;
                                                   # manufacturer 'nvidia' still makes it acceleratable + sliceable.
COUNT=8                                             # 8× A10G-like card (the canonical Story-6 node)
MEM_MIB=24576                                       # 24Gi per card
D=1600000                                           # ResourceMaxUnits (credit base M)
ACCEL_NF="${NODE}-gpustack-e2e-accel"               # fake accelerator NodeFeature (case-4/5 style)
WORKER_NF="${NODE}-gpustack-worker"                 # the worker NodeFeature the unit-spec write must NOT touch
LABELPFX="acceleratable.feature.gpustack.ai/${AKEY}"
MOCK_DEV="${NODE}-gpustack-e2e-devices"             # phantom-node Devices CR carrying the mocked ledger
MANAGED_LABEL="gpustack.ai/managed"

restore() {
  echo
  echo "[case-6] cleanup: deleting mocked Devices, injected NodeFeature"
  kubectl delete devices.worker.gpustack.ai "$MOCK_DEV" --ignore-not-found 2>/dev/null || true
  kubectl -n "$NS" delete nodefeature "$ACCEL_NF" --ignore-not-found 2>/dev/null || true
  # Removing the accelerator drains the derived flavor → the InstanceType + CQ self-tear-down.
  sleep 5
}
trap restore EXIT

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

# set_ledger "<8 space-separated card tokens>" patches the mocked Devices ledger.
# Tokens: f=free, e=exclusive, s9=shared(9 shares free), l80/l78=sliced(80/78% free).
set_ledger() {
  local tokens="$1"
  local groups
  groups=$(D="$D" TOKENS="$tokens" python3 - <<'PY'
import json, os
D = int(os.environ["D"])
tok = {
    "f":  (0, D),                 # free whole card
    "e":  (1, 0),                 # exclusive: nothing left
    "s9": (2, 9 * (D // 10)),     # shared: 9 of 10 ownership shares free
    "l80": (3, 80 * (D // 100)),  # sliced: 80% VRAM free
    "l78": (3, 78 * (D // 100)),  # sliced: 78% VRAM free
}
accs = []
for i, t in enumerate(os.environ["TOKENS"].split()):
    mode, rem = tok[t]
    accs.append({"id": "c%d" % i, "index": i, "mode": mode, "remaining": rem})
print(json.dumps({"status": {"groups": [
    {"id": "g0", "manufacturer": "nvidia", "accelerators": accs},
]}}))
PY
)
  # Target the v1alpha1 CRD explicitly: the unversioned/v1 resource is the aggregated
  # proxy, whose /status subresource write returns ServiceUnavailable — only the real
  # v1alpha1 CRD serves the status subresource.
  kubectl patch devices.v1alpha1.worker.gpustack.ai "$MOCK_DEV" --subresource=status --type=merge -p "$groups" >/dev/null
}

# assert_view <label> <excl> <shared> <sliced> polls the InstanceType three-view until it
# matches the oracle (Remaining == OnceMaxRequest on a single node), then records PASS/FAIL.
assert_view() {
  local label="$1" wE="$2" wS="$3" wL="$4" e s l
  for _ in $(seq 1 40); do
    e=$(kubectl get instancetypes.worker.gpustack.ai "$ITNAME" -o jsonpath='{.status.accelerator.remaining}' 2>/dev/null)
    s=$(kubectl get instancetypes.worker.gpustack.ai "$ITNAME" -o jsonpath='{.status.acceleratorShared.remaining}' 2>/dev/null)
    l=$(kubectl get instancetypes.worker.gpustack.ai "$ITNAME" -o jsonpath='{.status.acceleratorSliced.remaining}' 2>/dev/null)
    [ "$e" = "$wE" ] && [ "$s" = "$wS" ] && [ "$l" = "$wL" ] && break
    sleep 3
  done
  if [ "$e" = "$wE" ] && [ "$s" = "$wS" ] && [ "$l" = "$wL" ]; then
    record PASS "$label" "three-view ${e}/${s}/${l}"
  else
    record FAIL "$label" "got ${e:-?}/${s:-?}/${l:-?}, want ${wE}/${wS}/${wL} — three-view math (T5.4b) or ledger reverse-lookup"
  fi
}

# 1. Inject a fake accelerator (count=8): NFD merges it onto the node, the Worker derives the
#    accelerated ResourceFlavor → ClusterQueue → InstanceType.
echo "[case-6] injecting fake accelerator ${AKEY} (count=${COUNT}) on node ${NODE}"
cat <<EOF | kubectl apply -f -
apiVersion: nfd.k8s-sigs.io/v1alpha1
kind: NodeFeature
metadata:
  name: ${ACCEL_NF}
  namespace: ${NS}
  labels:
    nfd.node.kubernetes.io/node-name: ${NODE}
    app.kubernetes.io/part-of: gpustack-operator-e2e
spec:
  labels:
    ${LABELPFX}: "true"
    ${LABELPFX}.count: "${COUNT}"
    ${LABELPFX}.product: "A10G"
    ${LABELPFX}.memory: "24Gi"
    ${LABELPFX}.cores: "12"
  features: {}
EOF

# 2. Wait for the derived accelerated InstanceType (its name is the pool name
#    gpustack-${AKEY}-<os>-<arch>) and capture it + its schedule labels.
ITNAME=""
for _ in $(seq 1 40); do
  ITNAME=$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null | python3 -c "
import json,sys
for it in json.load(sys.stdin).get('items',[]):
    n=it['metadata']['name']
    if n.startswith('gpustack-${AKEY}-'):
        print(n); break
" 2>/dev/null)
  [ -n "$ITNAME" ] && break
  sleep 3
done
[ -n "$ITNAME" ] || { echo "[case-6] derived accelerated InstanceType never materialized — is instance-type-derived-from-node on?"; exit 1; }
echo "[case-6] derived InstanceType: ${ITNAME}"
record PASS "derived accelerated InstanceType" "${ITNAME}"

# Read the pool's schedule labels (feature-key + os + arch) from the InstanceType, so the
# mocked Devices carries the exact labels the reconciler reverse-looks-it-up by.
read -r OS ARCH <<<"$(kubectl get instancetypes.worker.gpustack.ai "$ITNAME" -o json | python3 -c "
import json,sys
l=json.load(sys.stdin).get('metadata',{}).get('labels',{})
print(l.get('kubernetes.io/os',''), l.get('kubernetes.io/arch',''))
")"
[ -n "$OS" ] && [ -n "$ARCH" ] || { echo "[case-6] InstanceType is missing os/arch labels"; exit 1; }

# The accelerated InstanceType also materializes spec.os/spec.arch from the backing ClusterQueue's
# kubernetes.io/os|arch labels — os/arch live only as schedule labels, never in the CQ notes, so
# they must be read from the labels, not blanked. They must match the schedule labels above.
read -r SPEC_OS SPEC_ARCH <<<"$(kubectl get instancetypes.worker.gpustack.ai "$ITNAME" -o jsonpath='{.spec.os}{" "}{.spec.arch}')"
[ "$SPEC_OS" = "$OS" ] && [ "$SPEC_ARCH" = "$ARCH" ] \
  && record PASS "InstanceType materializes spec.os/arch" "spec os=${SPEC_OS} arch=${SPEC_ARCH} (from CQ labels)" \
  || record FAIL "InstanceType materializes spec.os/arch" "spec os='${SPEC_OS:-}' arch='${SPEC_ARCH:-}' vs labels ${OS}/${ARCH} — must read from the CQ kubernetes.io/os|arch labels, not the notes"

# 3. Create the phantom-node Devices CR carrying the mocked per-card ledger.
echo "[case-6] creating mocked Devices ${MOCK_DEV} (os=${OS} arch=${ARCH})"
cat <<EOF | kubectl apply -f -
apiVersion: worker.gpustack.ai/v1alpha1
kind: Devices
metadata:
  name: ${MOCK_DEV}
  labels:
    ${LABELPFX}: "true"
    kubernetes.io/os: "${OS}"
    kubernetes.io/arch: "${ARCH}"
    ${MANAGED_LABEL}: "true"
    app.kubernetes.io/part-of: gpustack-operator-e2e
spec:
  groups:
    - id: g0
      manufacturer: nvidia
      name: A10G
      memory: ${MEM_MIB}
EOF

# 4. Walk the five-step pooling sequence; the three-view must match the oracle at each step.
set_ledger "f f f f f f f f";       assert_view "init: 8 free"                         8 80 800
set_ledger "e e f f f f f f";       assert_view "step 1: +2 exclusive"                 6 60 600
set_ledger "e e s9 s9 f f f f";     assert_view "step 2: +2 shared (9 free)"           4 58 400
set_ledger "e e s9 s9 l80 l80 f f"; assert_view "step 3: +2 sliced (80% free)"         2 38 360
set_ledger "e e s9 s9 l78 l78 f f"; assert_view "step 4: sliced cards drop to 78%"     2 38 356
set_ledger "e e e s9 s9 l78 l78 f"; assert_view "step 5: +1 exclusive"                 1 28 256

# 5. Watch freshness: a native watch on the InstanceType observes the .status three-view move
#    as the ledger allocs/frees (exclusive 8 → 4 → 8).
echo "[case-6] asserting watch freshness over kubectl get -w"
set_ledger "f f f f f f f f"; assert_view "watch precondition: back to 8 free" 8 80 800 >/dev/null
watchlog=$(mktemp)
# Watch the NATIVE v1alpha1 CRD, not the unversioned name — the latter resolves to the aggregated
# worker.gpustack.ai/v1 apiservice (a proxy), whose watch re-projects/coalesces and drops intermediate
# .status transitions. Direction 2 is precisely that the real CRD delivers them via a native watch;
# mirrors set_ledger targeting devices.v1alpha1 for the same aggregated-proxy reason.
( timeout 60 kubectl get instancetypes.v1alpha1.worker.gpustack.ai "$ITNAME" -w \
    -o "jsonpath={.status.accelerator.remaining}{'\n'}" >"$watchlog" 2>/dev/null ) &
wpid=$!
sleep 2
set_ledger "e e s9 s9 f f f f"          # alloc → exclusive drops to 4
sleep 20                                # dwell long enough for a remote cluster's reconcile+watch to surface the drop
set_ledger "f f f f f f f f"            # free → exclusive recovers to 8
sleep 15
wait "$wpid" 2>/dev/null || true
if grep -qx 4 "$watchlog" && grep -qx 8 "$watchlog"; then
  record PASS "watch freshness (kubectl get -w)" "observed exclusive 8→4→8 via native watch"
else
  record FAIL "watch freshness (kubectl get -w)" "watch missed a transition: [$(tr '\n' ' ' <"$watchlog")] — native watch on .status (Direction 2)"
fi
rm -f "$watchlog"

# 6. The unit spec is FROZEN after create (declarative-management: unitResources / localStorage are
#    immutable on update), lives only on the InstanceType (never a CQ note), and its write path never
#    touches the NodeFeature. Editing the accelerated type's unit spec must be REJECTED by the
#    validating webhook and leave the stored spec unchanged.
echo "[case-6] attempting to edit InstanceType unit spec (must be rejected — immutable)"
nfBefore=$(kubectl -n "$NS" get nodefeature "$WORKER_NF" -o json 2>/dev/null | python3 -c "
import json,sys
print(json.dumps(json.load(sys.stdin).get('spec',{}).get('labels',{}),sort_keys=True))
" 2>/dev/null)
cpuBefore=$(kubectl get instancetypes.worker.gpustack.ai "$ITNAME" -o jsonpath='{.spec.unitResources.cpu}' 2>/dev/null)
errEdit=$(kubectl patch instancetypes.worker.gpustack.ai "$ITNAME" --type=merge \
  -p '{"spec":{"unitResources":{"cpu":"2","ram":"8Gi"},"localStorage":"64Gi"}}' 2>&1)
cpuAfter=$(kubectl get instancetypes.worker.gpustack.ai "$ITNAME" -o jsonpath='{.spec.unitResources.cpu}' 2>/dev/null)
{ echo "$errEdit" | grep -qiE 'immutable' && [ -n "$cpuAfter" ] && [ "$cpuAfter" = "$cpuBefore" ]; } \
  && record PASS "unit-spec edit is rejected (immutable)" "unitResources frozen; spec.unitResources.cpu stayed ${cpuBefore}" \
  || record FAIL "unit-spec edit is rejected (immutable)" "err='${errEdit:0:70}' cpu ${cpuBefore}->${cpuAfter} — the unit spec must be immutable after create"

# The unit spec must NOT flow into the ClusterQueue notes — it lives only on the InstanceType.
cqnote=$(kubectl get clusterqueue "$ITNAME" -o json 2>/dev/null | python3 -c "
import json,sys
print(json.load(sys.stdin).get('metadata',{}).get('annotations',{}).get('note.gpustack.ai/unitCPU','<absent>'))
" 2>/dev/null)
[ "$cqnote" = "<absent>" ] && record PASS "unit spec is not a ClusterQueue note" "no note.gpustack.ai/unitCPU on ${ITNAME}" \
  || record FAIL "unit spec is not a ClusterQueue note" "found note.gpustack.ai/unitCPU='${cqnote}' — unit spec leaked into CQ notes"

nfAfter=$(kubectl -n "$NS" get nodefeature "$WORKER_NF" -o json 2>/dev/null | python3 -c "
import json,sys
print(json.dumps(json.load(sys.stdin).get('spec',{}).get('labels',{}),sort_keys=True))
" 2>/dev/null)
[ "$nfBefore" = "$nfAfter" ] && record PASS "unit-spec write does not touch NodeFeature" "worker NodeFeature spec.labels unchanged" \
  || record FAIL "unit-spec write does not touch NodeFeature" "worker NodeFeature spec.labels changed — the write leaked upward"

# 7. Zero Cohort objects (Cohort removed entirely).
cohorts=$(kubectl get cohorts.kueue.x-k8s.io -A --no-headers 2>/dev/null | grep -c . || true)
[ "${cohorts:-0}" = "0" ] && record PASS "zero Cohort objects" "no cohorts.kueue.x-k8s.io" \
  || record FAIL "zero Cohort objects" "${cohorts} Cohort(s) present — CohortReconciler should be gone"

echo
echo "== CASE 6 — Pooled three-view + watch freshness (Direction 2) =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). See specs/2026-06-29-instancetype-unified-pool-refactor.md (Test Plan, e2e case-6)."
  echo "Map a FAIL to its Task: three-view→T5.4b, watch freshness→T5.5, unit-spec→T5.4b/T5.4c, Cohort→F3b."
  echo "Diagnose: kubectl -n ${NS} logs deploy/gpustack-operator-worker --tail=200"
  exit 1
fi
echo "CASE 6 PASS"
