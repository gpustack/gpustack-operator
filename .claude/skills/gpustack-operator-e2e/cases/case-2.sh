#!/usr/bin/env bash
#
# CASE 2 — Running Instance admits, then drain stops it (not recreate)   (MUTATING, self-recovering)
#
#   case-2.sh <NS>
#
# Verifies the Instance ↔ InstanceType ↔ pool contract on a real cluster (the fake client cannot —
# see references/drain-recycle.md):
#   1. A RUNNING general Instance's Workload is ADMITTED on the CPU-only ClusterQueue even though it
#      also requests memory/ephemeral-storage the queue does not cover — the queue's
#      quotaCheckStrategy: IgnoreUndeclared checks only the covered `cpu` dimension and ignores the
#      rest, instead of refusing to assign a flavor for the uncovered resources (F3b/F3d).
#   2. Draining the pool — toggling gpustack.ai/managed=false on the worker NodeFeature (the old
#      "bump the ram capacity label" no longer works; a general pool's capacity is Node CPU count,
#      not a bumpable label) — removes the pool's general ResourceFlavor (F3a, node-index-driven).
#   3. The RUNNING Instance whose type just drained is STOPPED (spec.stop=true), not recreated: on a
#      drain the queue evicts the Pod, and instance.go now evaluates the gone/Inactive type before
#      recreating a Pod, with an InstanceType watch re-enqueuing the Instance. This closed a
#      pre-existing gap where the stop check sat behind `pod == nil` with no InstanceType watch, so a
#      running Instance was left with a stuck Pending Pod, never stopped.
#
# Self-recovering: restores gpustack.ai/managed, deletes the test Instance, and waits for the
# chain to rebuild on exit (trap).
set -uo pipefail

NS="${1:?usage: case-2.sh <NS>}"
NODE=$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')
WORKER_NF="${NODE}-gpustack-worker"
IT=$(kubectl get instancetypes.worker.gpustack.ai -o jsonpath='{.items[?(@.status.phase=="Active")].metadata.name}' | awk '{print $1}')
[ -n "$IT" ] || { echo "no Active InstanceType found — run case-1 first to materialize the chain"; exit 1; }
before=$(kubectl get node "$NODE" -o jsonpath='{.metadata.labels.gpustack\.ai/managed}')
echo "active InstanceType: ${IT} on node ${NODE} (managed=${before:-<unset>})"

# The derived general InstanceType carries no unit spec by default; the InstanceWebhook needs one
# to size the Instance's Pod, so set it via the InstanceType API (this itself exercises the
# admin-writable spec→CQ path). Confirm it stuck before creating the Instance: right after a fresh
# deploy the InstanceType validating webhook may briefly be unready and reject the patch, and an
# empty unitRAM then fails the Instance webhook's quantity parse ("invalid RAM unit"). Once set the
# reconciler preserves it (admin values are authoritative), so retry until it is observed.
unit_ram=""
for _ in $(seq 1 15); do
  kubectl patch instancetypes.worker.gpustack.ai "$IT" --type=merge \
    -p '{"spec":{"unitResources":{"cpu":"1","ram":"2Gi"},"localStorage":"10Gi"}}' >/dev/null 2>&1
  unit_ram=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.spec.unitResources.ram}' 2>/dev/null)
  [ -n "$unit_ram" ] && break
  sleep 3
done
[ -n "$unit_ram" ] || { echo "no unit spec on ${IT} — the Instance webhook needs unitRAM to size the Pod (validating webhook not ready?)"; exit 1; }

restore() {
  echo
  echo "[case-2] restoring gpustack.ai/managed=${before:-true}, deleting test Instance, waiting for rebuild"
  kubectl -n "$NS" patch nodefeature "$WORKER_NF" --type=merge \
    -p "{\"spec\":{\"labels\":{\"gpustack.ai/managed\":\"${before:-true}\"}}}" 2>/dev/null || true
  kubectl -n default delete instance gpustack-e2e-instance --ignore-not-found 2>/dev/null || true
  for _ in $(seq 1 40); do
    [ "$(kubectl get instancetype "$IT" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Active" ] && break
    sleep 3
  done
}
trap restore EXIT

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

# 1. Create a running Instance referencing the Active InstanceType. alpine is kept alive (sleep)
#    so its Kueue Workload holds quota; the ephemeral volume lets non-type validation pass.
cat <<EOF | kubectl apply -f -
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata: { name: gpustack-e2e-instance, namespace: default }
spec:
  type: ${IT}
  image: alpine
  command: ["sleep", "86400"]
  volume: { ephemeral: { capacity: 1Gi } }
EOF

# 2. The Pod must be admitted by Kueue (holding quota is what lets the drain evict it).
admitted=""
for _ in $(seq 1 20); do
  a=$(kubectl -n default get workloads.kueue.x-k8s.io \
        -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Admitted")].status}{"\n"}{end}' 2>/dev/null | grep -m1 True)
  [ -n "$a" ] && { admitted=1; break; }
  sleep 3
done
[ -n "$admitted" ] && record PASS "workload admitted" "Kueue Admitted=True (holds quota)" \
  || record FAIL "workload admitted" "no Admitted workload — the cpu-only CQ must admit despite the Pod's memory/ephemeral-storage (quotaCheckStrategy: IgnoreUndeclared)"

stop0=$(kubectl -n default get instance gpustack-e2e-instance -o jsonpath='{.spec.stop}' 2>/dev/null)
[ "$stop0" != "true" ] && record PASS "instance running pre-drain" "spec.stop=${stop0:-<unset>}" \
  || record FAIL "instance running pre-drain" "spec.stop already true before drain"

# 3. Drain: exclude the node from management so the general flavor is deleted and the derived
#    InstanceType (the Instance's type) tears down. Toggle via the NodeFeature (NFD reverts a
#    direct node label).
echo "[case-2] draining: gpustack.ai/managed=false on ${WORKER_NF}"
kubectl -n "$NS" patch nodefeature "$WORKER_NF" --type=merge \
  -p '{"spec":{"labels":{"gpustack.ai/managed":"false"}}}'

# 4. The guaranteed drain effect: the pool's general ResourceFlavor is deleted (F3a — node-index
#    driven, independent of the running Instance/Workload). This confirms the managed-toggle
#    propagated; the fuller teardown (derived InstanceType removal) is CASE 3.
rf_gone=""
for _ in $(seq 1 40); do
  n=$(kubectl get resourceflavors.kueue.x-k8s.io -o name 2>/dev/null | grep -c "${IT}-")
  [ "${n:-0}" -eq 0 ] && { rf_gone=1; break; }
  sleep 3
done
[ -n "$rf_gone" ] && record PASS "drain removes the pool ResourceFlavor" "no ${IT}-* flavor (F3a)" \
  || record FAIL "drain removes the pool ResourceFlavor" "a ${IT}-* flavor persists — managed-toggle did not propagate"

# 5. THE assertion: the Instance whose InstanceType is now gone/Inactive gets STOPPED, not recreated.
#    On a drain the queue evicts the Pod; the reconciler must set spec.stop (which deletes the Pod)
#    instead of recreating a Pod the drained queue can never admit. An InstanceType watch re-enqueues
#    the Instance so the stop is prompt even when no Pod event fires (poll — async).
stopped=""
for _ in $(seq 1 40); do
  s=$(kubectl -n default get instance gpustack-e2e-instance -o jsonpath='{.spec.stop}' 2>/dev/null)
  [ "$s" = "true" ] && { stopped=1; break; }
  sleep 3
done
[ -n "$stopped" ] && record PASS "instance STOPPED (not recreated)" "spec.stop=true" \
  || record FAIL "instance STOPPED (not recreated)" "spec.stop still ${s:-<unset>} — a running Instance must stop when its type drains"

# Ground truth in the logs — proves the stop-on-inactive/gone branch ran (grep -c, not -q: under
# pipefail, -q closes the pipe early and kubectl logs gets SIGPIPE → non-zero pipeline).
logged=""
for _ in 1 2 3 4 5 6 7 8 9 10; do
  cnt=$(kubectl -n "$NS" logs deploy/gpustack-operator-worker --since=10m 2>/dev/null | grep -c "stop instance as inactive instance type")
  [ "${cnt:-0}" -gt 0 ] && { logged=1; break; }
  sleep 3
done
[ -n "$logged" ] && record PASS "stop-on-inactive/gone branch ran" "log: stop instance as inactive instance type" \
  || record FAIL "stop-on-inactive/gone branch ran" "log line absent — the stop branch may not have run"

echo
echo "== CASE 2 — Running Instance admits, then drain stops it (not recreate) =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). See references/drain-recycle.md (why a real cluster & the drain trigger)."
  exit 1
fi
echo "CASE 2 PASS"
