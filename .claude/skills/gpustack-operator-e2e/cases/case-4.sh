#!/usr/bin/env bash
#
# CASE 4 — AdmissionCheck holds exclusive over-admit (Story 4)   (MUTATING, self-recovering)
#
#   case-4.sh <NS>
#
# Story 4 / success-criterion 2 (specs/2026-06-29-instancetype-unified-pool-refactor.md):
# when every card is already sliced, a request for whole exclusive cards passes the coarse
# Kueue `credits` gate (gate 1) but must be HELD by the node-devices AdmissionCheck (gate 3),
# which reads the per-card `Devices` ledger — so it never becomes an admitted-then-unschedulable
# workload. The check returns `Retry` (transient: re-checked as capacity frees), not `Rejected`.
#
# Runs on a GPU-LESS cluster BY APPROXIMATION (same mock recipe as CASE 6): a fake accelerator
# NodeFeature (count=8) drives the real derivation of the accelerated ResourceFlavor →
# ClusterQueue (which references the AdmissionCheck) → InstanceType, and a PHANTOM-node `Devices`
# CR carries a mocked per-card ledger where all 8 cards are 50%-sliced — no clean whole card.
# NOT mocked: the CQ→AdmissionCheck wiring, the Workload quota reservation, and the per-card
# feasibility the real NodeDevicesAdmissionCheckReconciler computes over the mocked ledger.
#
# Self-recovering: deletes the test Instance, the mocked Devices, and the NodeFeature on exit.
set -uo pipefail

NS="${1:?usage: case-4.sh <NS>}"
NODE=$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')
AKEY=nvidia-a10g
COUNT=8
MEM_MIB=24576
D=1600000
AC=gpustack-node-devices
ACCEL_NF="${NODE}-gpustack-e2e-accel"
MOCK_DEV="${NODE}-gpustack-e2e-devices"
LABELPFX="acceleratable.feature.gpustack.ai/${AKEY}"
MANAGED_LABEL="gpustack.ai/managed"
EXCL_RES="nvidia.com/gpu"                 # exclusive whole-card resource for nvidia
POD=gpustack-e2e-overadmit

restore() {
  echo
  echo "[case-4] cleanup: deleting Pod, mocked Devices, injected NodeFeature"
  kubectl -n default delete pod "$POD" --ignore-not-found --force --grace-period=0 2>/dev/null || true
  kubectl delete devices.worker.gpustack.ai "$MOCK_DEV" --ignore-not-found 2>/dev/null || true
  kubectl -n "$NS" delete nodefeature "$ACCEL_NF" --ignore-not-found 2>/dev/null || true
  sleep 5
}
trap restore EXIT

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

# 0. Precondition: the AdmissionCheck object exists and is Active (installKueue applies it,
#    NodeDevicesAdmissionCheckReconciler activates it).
acActive=$(kubectl get admissioncheck "$AC" -o jsonpath='{.status.conditions[?(@.type=="Active")].status}' 2>/dev/null)
[ "$acActive" = "True" ] && record PASS "AdmissionCheck Active" "${AC} controllerName=worker.gpustack.ai/node-devices" \
  || record FAIL "AdmissionCheck Active" "got Active=${acActive:-<missing>} — installKueue applies it, the AC reconciler activates it"

# 1. Inject a fake accelerator (count=8) → derived accelerated ResourceFlavor/CQ/InstanceType.
echo "[case-4] injecting fake accelerator ${AKEY} (count=${COUNT})"
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

# 2. Wait for the derived accelerated InstanceType and read its os/arch + entrance LocalQueue.
ITNAME=""
for _ in $(seq 1 40); do
  ITNAME=$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null | python3 -c "
import json,sys
for it in json.load(sys.stdin).get('items',[]):
    if it['metadata']['name'].startswith('gpustack-${AKEY}-'): print(it['metadata']['name']); break
" 2>/dev/null)
  [ -n "$ITNAME" ] && break
  sleep 3
done
[ -n "$ITNAME" ] || { echo "[case-4] derived accelerated InstanceType never materialized"; exit 1; }
read -r OS ARCH LQ <<<"$(kubectl get instancetypes.worker.gpustack.ai "$ITNAME" -o json | python3 -c "
import json,sys
o=json.load(sys.stdin); l=o.get('metadata',{}).get('labels',{})
print(l.get('kubernetes.io/os',''), l.get('kubernetes.io/arch',''), o.get('status',{}).get('entrance',''))
")"
echo "[case-4] derived InstanceType ${ITNAME} (os=${OS} arch=${ARCH} entrance=${LQ})"

# 3. The backing ClusterQueue must reference the node-devices AdmissionCheck (gate-3 wiring).
acRef=""
for _ in $(seq 1 20); do
  acRef=$(kubectl get clusterqueue "$ITNAME" -o jsonpath='{.spec.admissionChecksStrategy.admissionChecks[*].name}' 2>/dev/null | tr ' ' '\n' | grep -x "$AC")
  [ -n "$acRef" ] && break
  sleep 3
done
[ -n "$acRef" ] && record PASS "CQ references AdmissionCheck" "admissionChecksStrategy → ${AC}" \
  || record FAIL "CQ references AdmissionCheck" "CQ ${ITNAME} does not reference ${AC} — gate-3 not wired (acceleratable+derived+AC-Active)"

# 4. Mock the per-card ledger: 8 cards, each 50%-sliced → no clean whole card for exclusive.
echo "[case-4] creating mocked Devices ${MOCK_DEV}: 8× card sliced 50% (no clean card)"
accs=$(D="$D" COUNT="$COUNT" python3 - <<'PY'
import json, os
D = int(os.environ["D"]); n = int(os.environ["COUNT"])
half = 50 * (D // 100)                       # 50% VRAM free, mode=Sliced(3)
print(json.dumps([{"id": "c%d" % i, "index": i, "mode": 3, "remaining": half} for i in range(n)]))
PY
)
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
# Target the v1alpha1 CRD explicitly: the aggregated v1 proxy's /status subresource write
# returns ServiceUnavailable — only the real v1alpha1 CRD serves the status subresource.
kubectl patch devices.v1alpha1.worker.gpustack.ai "$MOCK_DEV" --subresource=status --type=merge \
  -p "{\"status\":{\"groups\":[{\"id\":\"g0\",\"manufacturer\":\"nvidia\",\"accelerators\":${accs}}]}}" >/dev/null

# 5. Submit a raw Pod requesting 5 EXCLUSIVE cards, routed to the accelerated pool's entrance
#    LocalQueue. Credits (5×M ≤ 8×M) reserve quota at gate 1; gate 3 must hold it because no
#    clean whole card exists. (A raw Pod — not an Instance — keeps this independent of the
#    Instance webhook's unit-spec requirements; Kueue's pod integration builds the Workload.)
[ -n "$LQ" ] || { echo "[case-4] InstanceType has no entrance LocalQueue yet"; exit 1; }
echo "[case-4] submitting raw Pod requesting 5 exclusive cards on queue ${LQ}"
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: ${POD}
  namespace: default
  labels:
    kueue.x-k8s.io/queue-name: ${LQ}
spec:
  schedulerName: default-scheduler
  containers:
    - name: main
      image: alpine
      command: ["sleep", "86400"]
      resources:
        limits: { ${EXCL_RES}: "5" }
        requests: { ${EXCL_RES}: "5" }
EOF

# 6. THE assertion: the Workload is held by the AdmissionCheck (state Retry) and NOT Admitted.
verdict=""
for _ in $(seq 1 40); do
  read -r state admitted <<<"$(kubectl -n default get workloads.kueue.x-k8s.io -o json 2>/dev/null | python3 -c "
import json,sys
for wl in json.load(sys.stdin).get('items',[]):
    checks=wl.get('status',{}).get('admissionChecks',[])
    st=next((c.get('state','') for c in checks if c.get('name')=='${AC}'), '')
    if not st: continue
    adm=next((c.get('status','') for c in wl.get('status',{}).get('conditions',[]) if c.get('type')=='Admitted'), 'False')
    print(st, adm); break
" 2>/dev/null)"
  [ "$state" = "Retry" ] && [ "$admitted" != "True" ] && { verdict=1; break; }
  [ "$state" = "Rejected" ] && { verdict=rejected; break; }
  sleep 3
done
if [ "$verdict" = 1 ]; then
  record PASS "exclusive over-admit held by gate-3" "AdmissionCheck state=Retry, workload NOT Admitted (per-card infeasible)"
elif [ "$verdict" = rejected ]; then
  record FAIL "exclusive over-admit held by gate-3" "state=Rejected (should be Retry — transient infeasibility, re-checked as capacity frees)"
else
  record FAIL "exclusive over-admit held by gate-3" "got state=${state:-<none>} admitted=${admitted:-?} — gate-3 admitted the over-request (ledger reverse-lookup or feasibility math)"
fi

echo
echo "== CASE 4 — AdmissionCheck holds exclusive over-admit (Story 4) =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). Map a FAIL to its Task: AC Active→T4.2, CQ ref→T4.2/F5d, Retry verdict→T4.2."
  echo "Diagnose: kubectl -n ${NS} logs deploy/gpustack-operator-worker --tail=200 | grep -i admission"
  exit 1
fi
echo "CASE 4 PASS"
