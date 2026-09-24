#!/usr/bin/env bash
#
# CASE 4 — AdmissionCheck holds exclusive over-admit   (MUTATING, self-recovering)
#
#   case-4.sh <NS>
#
# Goal:        When every card is already sliced, a request for whole exclusive cards passes the
#              coarse Kueue credits gate but is HELD by the node-devices AdmissionCheck reading the
#              per-card Devices ledger — so it never becomes an admitted-then-unschedulable Workload.
#              The check returns Retry (transient, re-checked as capacity frees), not Rejected.
# Environment: Any cluster BY APPROXIMATION — the fake product key nvidia-e2emock never collides with
#              a real GPU's derived pool/Devices, so it is safe on a real-accelerator cluster too.
#              No real hardware needed.
# Inputs:      - MOCKED: a fake accelerator NodeFeature (nvidia-e2emock, count=8, 24Gi/card) that
#                drives real derivation of the accelerated ResourceFlavor → ClusterQueue → InstanceType;
#                a phantom-node Devices ledger where all 8 cards are 50%-sliced (no clean whole card);
#              - MOCKED: the Devices ledger is labelled with the accelerated flavor's own node
#                selector (minus its node-batch pin), which is how the AdmissionCheck finds it;
#              - MOCKED: the Node's nvidia.com/gpu capacity (8), advertised through the status
#                subresource only when the Node reports none. Kueue's topology-aware scheduling fits a
#                Pod against the Node's own allocatable, so on a node with no such resource the Workload
#                never reserves quota and the AdmissionCheck is never consulted;
#              - real: a raw Pod requesting 5 exclusive nvidia.com/gpu cards on the pool's entrance
#                LocalQueue (a raw Pod, not an Instance, keeps this independent of the Instance webhook);
#              - NOT mocked (the verification): the CQ→AdmissionCheck wiring, the Workload quota
#                reservation, and the per-card feasibility the reconciler computes over the ledger.
# Expected:    - the gpustack-node-devices AdmissionCheck is Active;
#              - the derived accelerated InstanceType materializes;
#              - its backing ClusterQueue references the AdmissionCheck and carries the mocked flavor;
#              - the Workload's AdmissionCheck state is Retry and it is NOT Admitted (held, not rejected).
# Cleanup:     Trap deletes the test Pod, the mocked Devices, and the injected NodeFeature, and removes
#              the nvidia.com/gpu capacity from the Node when this case advertised it.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-4.sh <NS>}"
NODE=$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')
AKEY=nvidia-e2emock   # 'nvidia' is a known manufacturer (gates derivation); 'e2emock' never collides with a real product
# The mocked Devices group id, derived from AKEY so the two can never drift apart. A pool reads only
# the Devices groups whose "<manufacturer>-<id>" equals its own spec.acceleratorGroup (== AKEY), so a
# group id picked independently of AKEY makes every accelerator view read zero.
AGID="${AKEY#*-}"
COUNT=8
MEM_MIB=24576
D=1600000
AC=gpustack-node-devices
ACCEL_NF="${NODE}-gpustack-e2e-accel"
MOCK_DEV="${NODE}-gpustack-e2e-devices"
LABELPFX="acceleratable.feature.gpustack.ai/${AKEY}"
EXCL_RES="nvidia.com/gpu"                 # exclusive whole-card resource for nvidia
POD=gpustack-e2e-overadmit
# Set to 1 BEFORE the Node is patched, so an interruption between the two still removes it; removing
# a key that was never added fails harmlessly, while leaving an added one advertises cards that do not
# exist to every later case.
ADVERTISED=0

restore() {
  echo
  echo "[case-4] cleanup: deleting Pod, mocked Devices, injected NodeFeature"
  kubectl -n default delete pod "$POD" --ignore-not-found --force --grace-period=0 2>/dev/null || true
  kubectl delete devices.worker.gpustack.ai "$MOCK_DEV" --ignore-not-found 2>/dev/null || true
  kubectl -n "$NS" delete nodefeature "$ACCEL_NF" --ignore-not-found 2>/dev/null || true
  if [ "$ADVERTISED" -eq 1 ]; then
    echo "[case-4] cleanup: removing the advertised ${EXCL_RES} capacity from node ${NODE}"
    kubectl patch node "$NODE" --subresource=status --type=json \
      -p '[{"op":"remove","path":"/status/capacity/nvidia.com~1gpu"},{"op":"remove","path":"/status/allocatable/nvidia.com~1gpu"}]' \
      >/dev/null 2>&1 || true
  fi
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
    ${LABELPFX}.product: "E2E-Mock"
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
    if it['metadata']['name'].startswith('gpustack--${AKEY}-'): print(it['metadata']['name']); break
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

# The queue must also CARRY the mocked flavor, and not be draining, before anything is submitted. A
# queue left over from an earlier mock of the same key already references the check while it is
# still draining its old flavor or has emptied its resource groups, and a Workload submitted into
# that window is judged by a queue with no quota at all — which is a different question from the one
# this case asks.
cqFlavors=""
cqStop=""
for _ in $(seq 1 40); do
  IFS='|' read -r cqStop cqFlavors <<<"$(kubectl get clusterqueue "$ITNAME" \
    -o jsonpath='{.spec.stopPolicy}|{.spec.resourceGroups[*].flavors[*].name}' 2>/dev/null)"
  case "$cqStop" in "" | None) [ -n "$cqFlavors" ] && break ;; esac
  sleep 3
done
case "$cqStop" in "" | None) ;; *) cqFlavors="" ;; esac
[ -n "$cqFlavors" ] || { echo "[case-4] CQ ${ITNAME} never carried a flavor for the mocked accelerator outside a drain (stopPolicy=${cqStop:-<none>}), so no quota exists to pass gate 1"; exit 1; }

# Kueue's topology-aware scheduling fits the Pod against the Node's own allocatable of every resource
# it requests, so on a node reporting no nvidia.com/gpu the Workload is excluded before quota is
# reserved and the AdmissionCheck is never consulted. Advertise the mocked count only when the Node
# reports none: a node that already reports the resource is one a real device plugin owns, and
# overwriting its value would misstate real hardware.
gpuAlloc=$(kubectl get node "$NODE" -o jsonpath='{.status.allocatable.nvidia\.com/gpu}' 2>/dev/null)
if [ -z "$gpuAlloc" ]; then
  echo "[case-4] advertising ${EXCL_RES}=${COUNT} on node ${NODE} (it reports none)"
  ADVERTISED=1
  kubectl patch node "$NODE" --subresource=status --type=json \
    -p "[{\"op\":\"add\",\"path\":\"/status/capacity/nvidia.com~1gpu\",\"value\":\"${COUNT}\"},{\"op\":\"add\",\"path\":\"/status/allocatable/nvidia.com~1gpu\",\"value\":\"${COUNT}\"}]" \
    >/dev/null || { echo "[case-4] could not advertise ${EXCL_RES} on node ${NODE}"; exit 1; }
elif [ "$gpuAlloc" -lt 5 ] 2>/dev/null; then
  echo
  echo "== CASE 4 — SKIPPED: NOTHING WAS VERIFIED =="
  echo "Node ${NODE} reports ${EXCL_RES}=${gpuAlloc} from a real device plugin, fewer than the 5 this case"
  echo "requests, so topology-aware scheduling excludes the Pod before gate 3 is reached. Its value is"
  echo "not overwritten."
  exit 0
fi

# 4. Mock the per-card ledger: 8 cards, each 50%-sliced → no clean whole card for exclusive.
echo "[case-4] creating mocked Devices ${MOCK_DEV}: 8× card sliced 50% (no clean card)"
accs=$(D="$D" COUNT="$COUNT" python3 - <<'PY'
import json, os
D = int(os.environ["D"]); n = int(os.environ["COUNT"])
half = 50 * (D // 100)                       # 50% VRAM free, mode=Sliced(3)
print(json.dumps([{"id": "c%d" % i, "index": i, "mode": 3, "remaining": half} for i in range(n)]))
PY
)
# The ledger is labelled with the accelerated flavor's own node selector, minus its ".count"
# node-batch pin, because that selector is how the AdmissionCheck finds a node's Devices. A
# hand-written label set drifts from it — the flavor also pins the CPU group and, under topology-aware
# scheduling, the topology profile — and a ledger the check cannot find holds every Workload in Retry
# whatever its cards say, which is this row passing with the per-card math never consulted.
DEV_LABELS=$(kubectl get resourceflavors.kueue.x-k8s.io $cqFlavors -o json 2>/dev/null | LABELPFX="$LABELPFX" python3 -c '
import json, os, sys
d = json.load(sys.stdin)
for rf in d.get("items", [d]):
    labels = rf.get("spec", {}).get("nodeLabels", {})
    if labels.get(os.environ["LABELPFX"]) != "true":
        continue
    for k, v in sorted(labels.items()):
        if not k.endswith(".count"):
            print("    %s: \"%s\"" % (k, v))
    break
')
[ -n "$DEV_LABELS" ] || { echo "[case-4] no flavor of CQ ${ITNAME} pins ${LABELPFX}, so there is no selector to label the ledger with"; exit 1; }
cat <<EOF | kubectl apply -f -
apiVersion: worker.gpustack.ai/v1alpha1
kind: Devices
metadata:
  name: ${MOCK_DEV}
  labels:
${DEV_LABELS}
    app.kubernetes.io/part-of: gpustack-operator-e2e
spec:
  groups:
    - id: ${AGID}
      manufacturer: nvidia
      name: E2E-Mock
      memory: ${MEM_MIB}
EOF
# Target the v1alpha1 CRD explicitly: the aggregated v1 proxy's /status subresource write
# returns ServiceUnavailable — only the real v1alpha1 CRD serves the status subresource.
kubectl patch devices.v1alpha1.worker.gpustack.ai "$MOCK_DEV" --subresource=status --type=merge \
  -p "{\"status\":{\"groups\":[{\"id\":\"${AGID}\",\"manufacturer\":\"nvidia\",\"accelerators\":${accs}}]}}" >/dev/null

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
#    The Workload is the one owned by this case's Pod, found whether or not it carries the check: a
#    Workload admitted through a queue with no check is the failure that must be reported as such,
#    and skipping it would report "no Workload" instead.
verdict=""
for _ in $(seq 1 40); do
  read -r state admitted reserved reservedMsg <<<"$(kubectl -n default get workloads.kueue.x-k8s.io -o json 2>/dev/null | python3 -c "
import json,sys
for wl in json.load(sys.stdin).get('items',[]):
    if not any(o.get('kind')=='Pod' and o.get('name')=='${POD}' for o in wl['metadata'].get('ownerReferences',[])): continue
    st=wl.get('status',{})
    check=next((c.get('state','') for c in st.get('admissionChecks',[]) if c.get('name')=='${AC}'), '-')
    conds={c.get('type'):c for c in st.get('conditions',[])}
    qr=conds.get('QuotaReserved',{})
    print(check, conds.get('Admitted',{}).get('status','False'), qr.get('status','False'),
          ' '.join((qr.get('message') or '-').split())[:200]); break
" 2>/dev/null)"
  [ "$state" = "Retry" ] && [ "$admitted" != "True" ] && { verdict=1; break; }
  [ "$state" = "Rejected" ] && { verdict=rejected; break; }
  # Admission ends the question, and the state is read at that moment: afterwards the admitted Pod
  # runs into a node with no such device and fails, and what the Workload says then is no longer the
  # verdict gate 3 gave.
  [ "$admitted" = "True" ] && break
  sleep 3
done
if [ "$verdict" = 1 ]; then
  record PASS "exclusive over-admit held by gate-3" "AdmissionCheck state=Retry, workload NOT Admitted (per-card infeasible)"
elif [ "$verdict" = rejected ]; then
  record FAIL "exclusive over-admit held by gate-3" "state=Rejected (should be Retry — transient infeasibility, re-checked as capacity frees)"
elif [ -z "$state" ]; then
  record FAIL "exclusive over-admit held by gate-3" "no Workload owned by Pod ${POD} appeared, so gate-3 was never asked"
elif [ "$admitted" = "True" ] && [ "$state" = "-" ]; then
  record FAIL "exclusive over-admit held by gate-3" "admitted with no ${AC} check on the Workload at all — the queue it was admitted through carries none"
elif [ "$admitted" = "True" ]; then
  record FAIL "exclusive over-admit held by gate-3" "admitted with check state=${state} — gate-3 admitted the over-request (ledger reverse-lookup or feasibility math)"
elif [ "$reserved" != "True" ]; then
  record FAIL "exclusive over-admit held by gate-3" "the Workload never reserved quota, so gate-3 was never consulted (check state=${state}): QuotaReserved=${reserved} ${reservedMsg}"
else
  record FAIL "exclusive over-admit held by gate-3" "quota reserved but the check never reported a verdict (state=${state}, admitted=${admitted})"
fi

echo
echo "== CASE 4 — AdmissionCheck holds exclusive over-admit =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). Confirm the AdmissionCheck is Active, the backing CQ references it,"
  echo "and the over-request Workload's check state is Retry (held), not Rejected and not Admitted."
  echo "Diagnose: kubectl -n ${NS} logs deploy/gpustack-operator-worker --tail=200 | grep -i admission"
  exit 1
fi
echo "CASE 4 PASS"
