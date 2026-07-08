#!/usr/bin/env bash
#
# CASE 20 — Sibling InstanceTypes on one pool stay status-consistent   (MUTATING, self-recovering; AUTO-SKIPS without a real sliceable accelerator)
#
#   case-20.sh <NS>
#
# Goal:        Several InstanceTypes that declare the SAME os / arch / acceleratorGroup / acceleratable
#              share ONE ResourceFlavor + Devices pool, so a Devices-ledger change must re-enqueue EVERY
#              one of them and leave their three-view status identical — the InstanceTypeReconciler's
#              enqueueInstanceTypeWhenDevicesChanged lists all types carrying the pool's feature key, not
#              just the one whose name matches. Deploying a sliced workload moves the ledger and is the
#              trigger.
# Environment: Needs a REAL sliceable accelerator pool (a Devices ledger a slice actually moves; the same
#              path CASE 8 uses). AUTO-SKIPS (exit 0, prints why) on a GPU-less / non-sliceable cluster.
# Inputs:      All real, nothing mocked —
#              - two throwaway admin InstanceTypes e2e-case20-a / e2e-case20-b declaring the derived
#                accelerated pool's acceleratorGroup/os/arch (+ a unit spec);
#              - one Pod e2e-case20-load requesting a 50% slice on the pool's entrance LocalQueue.
# Expected:    - all siblings (the derived one + a + b) report the SAME three-view status before the load;
#              - the sliced Pod admits + runs (a slice is taken from the pool);
#              - after the load every sibling's three-view is still identical AND has moved from the
#                pre-load value — proof the ledger change re-enqueued them all.
# Cleanup:     Trap deletes the load Pod and both throwaway types (+ their ClusterQueues), then waits for
#              the derived type's status to return to its pre-load value.
set -uo pipefail

NS="${1:?usage: case-20.sh <NS>}"
A=e2e-case20-a
B=e2e-case20-b
POD=gpustack-e2e-case20-load

# sig <instancetype> — the pool's three-view remaining signature (exclusive|shared|sliced), or empty.
sig() {
  kubectl get instancetype "$1" -o jsonpath='{.status.accelerator.remaining}|{.status.acceleratorShared.remaining}|{.status.acceleratorSliced.remaining}' 2>/dev/null
}

# --- Skip gate: a real SLICEABLE accelerated pool (derived InstanceType, sliced capacity > 0). ---
read -r DERIVED AKEY OS ARCH LQ <<<"$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null | python3 -c "
import json,sys
for it in json.load(sys.stdin).get('items',[]):
    s=it.get('spec',{}); st=it.get('status',{}); n=it['metadata']['name']
    sliced=st.get('acceleratorSliced',{}).get('capacity','0')
    if n.startswith('e2e-case20'): continue  # skip this case's own throwaways (e.g. a prior aborted run)
    if s.get('acceleratable') and s.get('acceleratorGroup') and s.get('sliceable') and sliced not in ('','0') and st.get('entrance'):
        print(n, s['acceleratorGroup'], s.get('os',''), s.get('arch',''), st['entrance']); break
")"
if [ -z "${DERIVED:-}" ] || [ -z "${LQ:-}" ]; then
  echo "== CASE 20 — SKIPPED =="
  echo "No real sliceable accelerated pool (an acceleratable+sliceable InstanceType with a non-zero sliced"
  echo "capacity and an entrance LocalQueue) — this case needs real accelerator hardware. Run it on a GPU cluster."
  exit 0
fi
# The sliced resource + its memory/cores percentage keys, read off a pool node's allocatable (the
# device-manager advertises "<vendor>/<class>.sliced[.memory-percentage|.cores-percentage]").
read -r SLICED MEMPCT CORESPCT <<<"$(kubectl get nodes -o json 2>/dev/null | python3 -c "
import json,sys
akey='${AKEY}'
for n in json.load(sys.stdin).get('items',[]):
    if n['metadata'].get('labels',{}).get('acceleratable.feature.gpustack.ai/'+akey)!='true': continue
    for k in n.get('status',{}).get('allocatable',{}):
        if k.endswith('.sliced'):
            print(k, k+'.memory-percentage', k+'.cores-percentage'); raise SystemExit
")"
[ -n "${SLICED:-}" ] || { echo "[case-20] no *.sliced resource advertised on a node carrying ${AKEY}"; exit 1; }
echo "[case-20] pool: derived=${DERIVED} acceleratorGroup=${AKEY} os/arch=${OS}/${ARCH} entrance=${LQ} sliced=${SLICED}"

cleanup() {
  echo
  echo "[case-20] cleanup: deleting load Pod + throwaway types"
  kubectl -n default delete pod "$POD" --ignore-not-found --force --grace-period=0 >/dev/null 2>&1 || true
  for t in "$A" "$B"; do
    kubectl patch instancetype "$t" --type=merge -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
    kubectl delete instancetype "$t" --wait=false >/dev/null 2>&1 || true
    kubectl delete clusterqueue "$t" --wait=false >/dev/null 2>&1 || true
  done
  # Let the freed slice settle back so a following case sees the pool whole again.
  for _ in $(seq 1 30); do [ "$(sig "$DERIVED")" = "$BASE" ] && break; sleep 3; done
}
trap cleanup EXIT

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

BASE=$(sig "$DERIVED")
[ -n "$BASE" ] && [ "$BASE" != "||" ] || { echo "[case-20] derived pool has no three-view status yet"; exit 1; }
echo "[case-20] baseline three-view (exclusive|shared|sliced) on ${DERIVED} = ${BASE}"

# 1. Declare two sibling admin InstanceTypes on the SAME pool identity.
for t in "$A" "$B"; do
  kubectl apply -f - >/dev/null 2>&1 <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: InstanceType
metadata:
  name: ${t}
spec:
  acceleratorGroup: ${AKEY}
  acceleratable: true
  os: ${OS}
  arch: ${ARCH}
  unitResources:
    cpu: "1"
    ram: "2Gi"
  localStorage: "100Gi"
EOF
done
ok=""
for _ in $(seq 1 40); do
  sa=$(sig "$A"); sb=$(sig "$B"); sd=$(sig "$DERIVED")
  [ -n "$sa" ] && [ "$sa" = "$sb" ] && [ "$sb" = "$sd" ] && { ok=1; break; }
  sleep 3
done
[ -n "$ok" ] \
  && record PASS "siblings share the pool status before load" "all three = ${sd} (exclusive|shared|sliced)" \
  || record FAIL "siblings share the pool status before load" "derived=${sd:-?} a=${sa:-?} b=${sb:-?} — sibling types on one pool must show the same status"

# 2. Deploy a 50% sliced workload on the pool's entrance LocalQueue — this moves the Devices ledger.
echo "[case-20] deploying 50% sliced Pod ${POD} on ${LQ}"
cat <<EOF | kubectl apply -f - >/dev/null 2>&1
apiVersion: v1
kind: Pod
metadata: { name: ${POD}, namespace: default, labels: { kueue.x-k8s.io/queue-name: ${LQ} } }
spec:
  schedulerName: default-scheduler
  restartPolicy: Never
  containers:
    - name: main
      image: ubuntu:24.04
      command: ["sleep", "86400"]
      resources:
        limits:   { ${SLICED}: "1", ${MEMPCT}: "50", ${CORESPCT}: "50" }
        requests: { ${SLICED}: "1", ${MEMPCT}: "50", ${CORESPCT}: "50" }
EOF
running=""
for _ in $(seq 1 50); do
  [ "$(kubectl -n default get pod "$POD" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Running" ] && { running=1; break; }
  sleep 3
done
[ -n "$running" ] \
  && record PASS "sliced workload admits + runs" "${POD} Running (a slice is taken from the pool)" \
  || record FAIL "sliced workload admits + runs" "${POD} not Running — Kueue admission / node-devices AdmissionCheck / scheduling failed"

# 3. The ledger change must re-enqueue EVERY sibling: all stay identical AND move from the baseline.
moved=""
for _ in $(seq 1 40); do
  sa=$(sig "$A"); sb=$(sig "$B"); sd=$(sig "$DERIVED")
  if [ -n "$sd" ] && [ "$sd" = "$sa" ] && [ "$sa" = "$sb" ] && [ "$sd" != "$BASE" ]; then moved=1; break; fi
  sleep 3
done
[ -n "$moved" ] \
  && record PASS "ledger change re-enqueues all siblings consistently" "all three = ${sd} (moved from ${BASE}, equal)" \
  || record FAIL "ledger change re-enqueues all siblings consistently" "derived=${sd:-?} a=${sa:-?} b=${sb:-?} base=${BASE} — a Devices change must refresh every type on the pool"

echo
echo "== CASE 20 — Sibling InstanceTypes on one pool stay status-consistent =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). Types sharing os/arch/acceleratorGroup/acceleratable share one pool;"
  echo "enqueueInstanceTypeWhenDevicesChanged must enqueue ALL of them on a Devices change so their"
  echo "three-view status stays identical. See pkg/worker/controllers/worker/instancetype.go."
  exit 1
fi
echo "CASE 20 PASS"
