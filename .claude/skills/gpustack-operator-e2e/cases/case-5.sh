#!/usr/bin/env bash
#
# CASE 5 — Sliced accelerator: partitions=8 → 1/8 admits, 0.125 credit   (MUTATING, self-recovering)
#
#   case-5.sh <NS>
#
# Implements the Final Checkpoint of specs/accelerator-resource-modes-refactor.md:
# label the ${node}-gpustack-worker NodeFeature partitions=8 → sliced InstanceType
# Capacity=32 → a 1/8 request admits and consumes 0.125 of a card. On the integer
# credit base B=D=12800 (specs/unified-credit-base-scoring.md) that 0.125 card is
# scored as 1600 credits, so Kueue's ResourceValue int64 ceil no longer rounds it
# up to 1 — Assertion G checks the true 1600, not a ceiled 1.
#
# Runs on a GPU-LESS cluster BY APPROXIMATION. The whole sliced chain depends on
# two DeviceManager-reported inputs that a GPU-less cluster never produces, so both
# are mocked here:
#   1. Accelerator feature labels (DeviceManager detector → <node>-gpustack-device-manager
#      NodeFeature → NFD merges onto Node.Labels): acceleratable / .count / .product /
#      .memory / .cores. .count is the gate for flavor derivation.
#   2. The bare device-plugin resource nvidia.com/gpu.sliced (DeviceManager allocator →
#      Node.status.capacity): the scheduler's NodeResourcesFit needs it on the node to
#      place a Pod that requests .sliced=C.
# NOT mocked on purpose: .sliced.units — it is auto-patched by the worker control-plane
# NodeCapacityReconciler (T13) from .count + .sliced.partitions + managed, so leaving it
# unmocked is itself the T13 verification. See references/drain-recycle.md (CASE 5).
#
# Mocks count=4 (the spec canonical node-5 A10G×4 case) so Capacity = 4×8 = 32.
#
# Self-recovering: deletes the injected NodeFeature and test Instance, strips the admin
# slicing label, and patch-removes the mocked capacity on exit (trap).
set -uo pipefail

NS="${1:?usage: case-5.sh <NS>}"
NODE=$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')
AKEY=nvidia-a10g                       # manufacturer 'nvidia' is a known acceleratable manufacturer
COUNT=4                                # card count (spec canonical node-5 A10G×4 → Capacity=4×8=32)
PARTITIONS=8
ACCEL_NF="${NODE}-gpustack-e2e-accel"  # case-4-style fake accelerator NodeFeature
WORKER_NF="${NODE}-gpustack-worker"    # admin slicing label target (T16 merge-preservation)
LABELPFX="acceleratable.feature.gpustack.ai/${AKEY}"
PARTITIONS_LABEL="${LABELPFX}.sliced.partitions"
SLICED_RES="nvidia.com/gpu.sliced"
SLICED_UNITS_RES="nvidia.com/gpu.sliced.units"
CREDITS_RES="credits.gpustack.ai/nvidia"
INSTANCE=gpustack-e2e-sliced-inst

restore() {
  echo
  echo "[case-5] cleanup: deleting Instance, stripping labels, removing mocked capacity"
  kubectl -n default delete instance "$INSTANCE" --ignore-not-found 2>/dev/null || true
  # Remove the admin slicing label (merge means deleting just this key).
  kubectl -n "$NS" patch nodefeature "$WORKER_NF" --type=merge \
    -p "{\"spec\":{\"labels\":{\"${PARTITIONS_LABEL}\":null}}}" 2>/dev/null || true
  # Delete the fake accelerator NodeFeature (drains the accelerated profile).
  kubectl -n "$NS" delete nodefeature "$ACCEL_NF" --ignore-not-found 2>/dev/null || true
  # Manually remove the bare device-plugin token — T13 only manages .sliced.units.
  kubectl patch node "$NODE" --subresource=status --type=merge \
    -p "{\"status\":{\"capacity\":{\"${SLICED_RES}\":null}}}" 2>/dev/null || true
  # Let NodeCapacityReconciler auto-reclaim .sliced.units after the label drops.
  sleep 5
}
trap restore EXIT

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

# 1. Inject a fake accelerator (count=4): NFD merges these onto the node, the Worker
#    derives the -Nd accelerated profile, and .sliced.partitions (step 2) turns it into -Ns.
echo "[case-5] injecting fake accelerator ${AKEY} (count=${COUNT}) on node ${NODE}"
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

# 2. Wait for the exclusive accelerated chain (-Nd, active) as a precondition.
excCQ=""
for _ in $(seq 1 40); do
  excCQ=$(kubectl get clusterqueues.kueue.x-k8s.io -o json 2>/dev/null | python3 -c "
import json,sys
for it in json.load(sys.stdin)['items']:
    n=it['metadata']['name']
    if '${AKEY}-' in n and n.endswith('d') and not n.endswith('${PARTITIONS}s') and it['spec'].get('stopPolicy','None')!='HoldAndDrain':
        print(n); break
" 2>/dev/null)
  [ -n "$excCQ" ] && break
  sleep 3
done
[ -n "$excCQ" ] || { echo "[case-5] exclusive accelerated chain never materialized — see references/drain-recycle.md"; exit 1; }
echo "[case-5] exclusive chain active: ${excCQ}"

# 3. Inject the admin slicing label into the worker NodeFeature (T16 must preserve it).
echo "[case-5] labeling worker NodeFeature ${PARTITIONS_LABEL}=${PARTITIONS}"
kubectl -n "$NS" patch nodefeature "$WORKER_NF" --type=merge \
  -p "{\"spec\":{\"labels\":{\"${PARTITIONS_LABEL}\":\"${PARTITIONS}\"}}}"

# 4. Mock the bare device-plugin token so a Pod requesting .sliced=C can be placed.
echo "[case-5] mocking device-plugin ${SLICED_RES}=1 on node status.capacity"
kubectl patch node "$NODE" --subresource=status --type=merge \
  -p "{\"status\":{\"capacity\":{\"${SLICED_RES}\":\"1\"}}}"

# --- Assertion A: admin slicing label survives reconcile on the worker NodeFeature (T16). ---
# python3 (not jsonpath) reads the label — its key carries '/' and '.' which jsonpath
# quoting mishandles; case-4 uses the same python3 approach for the same reason.
nfLab=""
for _ in $(seq 1 20); do
  nfLab=$(kubectl -n "$NS" get nodefeature "$WORKER_NF" -o json 2>/dev/null | python3 -c "
import json,sys
print(json.load(sys.stdin).get('spec',{}).get('labels',{}).get('${PARTITIONS_LABEL}',''))
" 2>/dev/null)
  [ "$nfLab" = "$PARTITIONS" ] && break
  nfLab=""; sleep 2
done
[ "$nfLab" = "$PARTITIONS" ] && record PASS "admin slicing label on worker NF" "preserved =${PARTITIONS} (T16 merge)" \
  || record FAIL "admin slicing label on worker NF" "wiped/reverted (${nfLab:-<unset>}) — NodeFeatureReconciler overwrote it (T16 regression)"

# --- Assertion B: label propagated to Node.Labels (NFD merge; the source downstream reads). ---
nodeLab=""
for _ in $(seq 1 30); do
  nodeLab=$(kubectl get node "$NODE" -o json 2>/dev/null | python3 -c "
import json,sys
print(json.load(sys.stdin).get('metadata',{}).get('labels',{}).get('${PARTITIONS_LABEL}',''))
" 2>/dev/null)
  [ "$nodeLab" = "$PARTITIONS" ] && break
  nodeLab=""; sleep 3
done
[ "$nodeLab" = "$PARTITIONS" ] && record PASS "slicing label on Node.Labels" "propagated =${PARTITIONS}" \
  || record FAIL "slicing label on Node.Labels" "absent (${nodeLab:-<unset>}) — NFD did not merge it"

# --- Assertion C: NodeCapacityReconciler (T13) auto-patches .sliced.units = count×D. ---
expUnits=$((COUNT * 12800))
unitsCap=""
for _ in $(seq 1 40); do
  unitsCap=$(kubectl get node "$NODE" -o json 2>/dev/null | python3 -c "
import json,sys
print(json.load(sys.stdin).get('status',{}).get('capacity',{}).get('${SLICED_UNITS_RES}',''))
" 2>/dev/null)
  [ "$unitsCap" = "$expUnits" ] && break
  unitsCap=""; sleep 3
done
[ "$unitsCap" = "$expUnits" ] && record PASS "auto .sliced.units capacity" "${SLICED_UNITS_RES}=${expUnits} (count×D, T13)" \
  || record FAIL "auto .sliced.units capacity" "got ${unitsCap:-<unset>}, want ${expUnits} — NodeCapacityReconciler did not patch (check managed=true + .count on Node)"

# 5. Wait for the sliced ClusterQueue (-Ns, active) and capture its name (= InstanceType name).
slicedCQ=""
for _ in $(seq 1 40); do
  slicedCQ=$(kubectl get clusterqueues.kueue.x-k8s.io -o json 2>/dev/null | python3 -c "
import json,sys
for it in json.load(sys.stdin)['items']:
    n=it['metadata']['name']
    if '${AKEY}-' in n and n.endswith('${PARTITIONS}s') and it['spec'].get('stopPolicy','None')!='HoldAndDrain':
        print(n); break
" 2>/dev/null)
  [ -n "$slicedCQ" ] && break
  sleep 3
done
[ -n "$slicedCQ" ] && record PASS "sliced ClusterQueue (-${PARTITIONS}s, active)" "$(echo "$slicedCQ" | cut -c1-52)" \
  || { record FAIL "sliced ClusterQueue (-${PARTITIONS}s, active)" "never materialized — slicing label may not have reached the Worker"; slicedCQ=""; }

# --- Assertion D: sliced InstanceType Accelerator.Capacity = count × partitions = 32. ---
expCap=$((COUNT * PARTITIONS))
if [ -n "$slicedCQ" ]; then
  cap=""
  for _ in $(seq 1 30); do
    cap=$(kubectl get instancetypes.worker.gpustack.ai "$slicedCQ" -o jsonpath='{.status.accelerator.capacity}' 2>/dev/null)
    [ "$cap" = "$expCap" ] && break
    cap=""; sleep 3
  done
  [ "$cap" = "$expCap" ] && record PASS "sliced InstanceType Capacity" "accelerator.capacity=${cap} (count×partitions=${COUNT}×${PARTITIONS})" \
    || record FAIL "sliced InstanceType Capacity" "got ${cap:-<unset>}, want ${expCap} — card-count/partitions derivation (Task 11)"
fi

# 6. Submit a 1/8 request: accelerator=1 (C, one card's slice), acceleratorUnits=1 (U).
echo "[case-5] submitting 1/8 sliced Instance (C=1, U=1) on ${slicedCQ}"
cat <<EOF | kubectl apply -f -
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata: { name: ${INSTANCE}, namespace: default }
spec:
  type: ${slicedCQ}
  image: alpine
  command: ["sleep", "86400"]
  resources:
    cpu: "1"
    ram: "1Gi"
    # localStorage deliberately omitted: the default path clamps to the
    # InstanceType's OnceMaxRequest (pkg/worker/webhooks/worker/instance.go), so
    # it can never exceed the limit. On a small node (docker-desktop 2c/6g) the
    # sliced InstanceType's storage OnceMaxRequest is < 1Gi, and an explicit
    # "1Gi" is rejected by the webhook — which masked the real behavior first run.
    accelerator: "1"          # C = 1 card's slice
    acceleratorUnits: 1       # U = 1 → 1/8 slice
  volume: { ephemeral: { capacity: 1Gi } }
EOF

# --- Assertion F: the request was admitted (sliced CQ has ≥1 admitted workload).
#     Read from ClusterQueue.status (cluster-level, no Workload-name coupling). ---
admitted=""
for _ in $(seq 1 30); do
  aw=$(kubectl get clusterqueue "$slicedCQ" -o jsonpath='{.status.admittedWorkloads}' 2>/dev/null)
  [ "${aw:-0}" -ge 1 ] 2>/dev/null && { admitted=1; break; }
  sleep 3
done
[ -n "$admitted" ] && record PASS "workload admitted" "sliced CQ admittedWorkloads>=1 (1600 credits = 0.125 card borrowed from exclusive)" \
  || record FAIL "workload admitted" "admittedWorkloads=${aw:-0} — credit borrow failed or Pod unschedulable (check .sliced mock + .sliced.units)"

# --- Assertion G: consumed exactly 1600 credits (= 0.125 card on the integer base
#     B=D=12800), and it is borrowed (Story 1 topology). Integer-valued credits are
#     the whole point of specs/unified-credit-base-scoring.md: Kueue's ResourceValue
#     ceils non-CPU usage to int64, so the pre-fix 0.125 was rounded up to 1 — 1600
#     passes through untouched. ClusterQueue.status.flavorsUsage[].resources[credits]
#     .total/borrowed; enableClusterQueueResources gates only metrics, not status. ---
expCredits=$((PARTITIONS > 0 ? 12800 / PARTITIONS : 0))   # B/partitions = 12800/8 = 1600
credits=""
for _ in $(seq 1 30); do
  credits=$(kubectl get clusterqueue "$slicedCQ" -o json 2>/dev/null | python3 -c "
import json,sys
from decimal import Decimal
cq=json.load(sys.stdin)
def norm(v):
    if not v: return ''
    if v.endswith('m'): return str(Decimal(v[:-1])/1000)
    return str(Decimal(v))
for fu in cq.get('status',{}).get('flavorsUsage',[]):
    for r in fu.get('resources',[]):
        if r.get('name')=='${CREDITS_RES}':
            print(norm(r.get('total','')), norm(r.get('borrowed','')))
" 2>/dev/null)
  total=$(echo "$credits" | awk '{print $1}')
  borrowed=$(echo "$credits" | awk '{print $2}')
  [ "$total" = "$expCredits" ] && [ "$borrowed" = "$expCredits" ] && break
  sleep 3
done
if [ "$total" = "$expCredits" ] && [ "$borrowed" = "$expCredits" ]; then
  record PASS "consumes ${expCredits} credits (= 0.125 card, borrowed)" "flavorsUsage[${CREDITS_RES}] total=${total} borrowed=${borrowed} (Story 1 borrow, integer base)"
else
  record FAIL "consumes ${expCredits} credits (= 0.125 card, borrowed)" "got total=${total:-<missing>} borrowed=${borrowed:-<missing>}, want ${expCredits} — base scaling (unified-credit-base-scoring) or transform/webhook off"
fi

# --- Assertion E (record-only): UnitResource folded per slice (round-down). ---
if [ -n "$slicedCQ" ]; then
  ucpu=$(kubectl get instancetypes.worker.gpustack.ai "$slicedCQ" -o jsonpath='{.spec.unitResources.cpu}' 2>/dev/null)
  uram=$(kubectl get instancetypes.worker.gpustack.ai "$slicedCQ" -o jsonpath='{.spec.unitResources.ram}' 2>/dev/null)
  record PASS "UnitResource per-slice (record)" "cpu=${ucpu:-?} ram=${uram:-?} (folded by ${PARTITIONS})"
fi

echo
echo "== CASE 5 — Sliced accelerator: partitions=8 → 1/8 admits, 0.125 credit =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). See specs/accelerator-resource-modes-refactor.md (Final Checkpoint)"
  echo "and references/drain-recycle.md (CASE 5). Map a FAIL to its Task: A→T16, B→NFD, C→T13,"
  echo "D→T11, F/G→T6/T7(borrow)+T9/T10(transform). Diagnose:"
  echo "  kubectl -n ${NS} logs deploy/gpustack-operator-worker --tail=200"
  exit 1
fi
echo "CASE 5 PASS"
