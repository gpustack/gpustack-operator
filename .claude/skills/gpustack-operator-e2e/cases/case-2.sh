#!/usr/bin/env bash
#
# CASE 2 — Running Instance admits, then drain stops it (not recreate)   (MUTATING, self-recovering)
#
#   case-2.sh <NS>
#
# Goal:        A running general Instance's Workload admits on the CPU-only queue despite requesting
#              resources the queue does not cover, and when its pool is drained the Instance is
#              STOPPED (spec.stop=true) — not recreated with a stuck Pending Pod.
# Environment: A real cluster with a materialized general pool (the fake client cannot reproduce
#              Kueue admission + eviction). No GPU. Targets the general (CPU) pool fed by every
#              managed node, so it behaves the same on a 1-node or an N-node cluster.
# Inputs:      All real, nothing mocked —
#              - sets the general InstanceType unit spec (cpu=1, ram=2Gi, localStorage=10Gi);
#              - a running Instance gpustack-e2e-instance (alpine sleep + ephemeral volume) in ns default;
#              - drains the pool by toggling gpustack.ai/managed=false on every <node>-gpustack-worker
#                NodeFeature (a general pool's capacity is Node CPU count, not a bumpable label).
# Expected:    - the Workload reaches Admitted=True on the cpu-only queue (quotaCheckStrategy checks
#                only the covered cpu dimension and ignores the uncovered memory/ephemeral-storage);
#              - the Instance is running (spec.stop unset) before the drain;
#              - the drain deletes the pool's general ResourceFlavor;
#              - the Instance flips to spec.stop=true (STOPPED, not recreated);
#              - the worker log shows the stop-on-inactive/gone-type branch ran.
# Cleanup:     Trap restores gpustack.ai/managed=true on all nodes, deletes the test Instance, clears
#              the InstanceType's drain-latched Spec.Inactive (draining a pool that holds an admitted
#              workload latches it Inactive BY DESIGN — managed=true alone will NOT reactivate it,
#              unlike an idle-drained pool in case-3), and waits for the InstanceType to return to
#              Active WITH a non-zero CPU capacity so a following case finds a healthy chain rather
#              than a pool whose flavor is still being rebuilt.
set -uo pipefail

NS="${1:?usage: case-2.sh <NS>}"
# Target the general (CPU) pool: it is non-accelerated (no GPU needed) and every managed node feeds
# it, so this behaves the same on a 1-node local cluster and an N-node real one.
IT=$(kubectl get instancetypes.worker.gpustack.ai \
  -o jsonpath='{.items[?(@.spec.acceleratable==false)].metadata.name}' 2>/dev/null | tr ' ' '\n' | grep -m1 'gpustack-')
[ -n "$IT" ] || { echo "no general InstanceType found — run case-1 first to materialize the chain"; exit 1; }
# Every <node>-gpustack-worker NodeFeature: draining ALL of them is what removes a pool that spans
# more than one node (de-managing a single node leaves the others' flavors behind).
WORKER_NFS=$(kubectl -n "$NS" get nodefeatures -o name 2>/dev/null | grep -E -- '-gpustack-worker$')
[ -n "$WORKER_NFS" ] || { echo "no <node>-gpustack-worker NodeFeatures found"; exit 1; }
echo "general InstanceType: ${IT}; draining $(echo "$WORKER_NFS" | grep -c .) node(s)"

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
  echo "[case-2] restoring gpustack.ai/managed=true on all nodes, deleting test Instance, clearing drain-latched Inactive, waiting for rebuild"
  echo "$WORKER_NFS" | xargs -r -I{} kubectl -n "$NS" patch {} --type=merge \
    -p '{"spec":{"labels":{"gpustack.ai/managed":"true"}}}' 2>/dev/null || true
  kubectl -n default delete instance gpustack-e2e-instance --ignore-not-found 2>/dev/null || true
  # Draining a pool that still holds an admitted workload latches the InstanceType Inactive BY
  # DESIGN: NodeQueueReconciler drives HoldAndDrain to evict the workload and syncInactive mirrors
  # that into Spec.Inactive=true, sticky until an admin clears it — so managed=true alone does NOT
  # reactivate this pool (an idle-drained pool would, per case-3). Clearing it races the drain:
  # while the queue is still HoldAndDrain the mirror RE-LATCHES inactive=true, so a single clear
  # sticks only once the drain has settled (StopPolicy leaves HoldAndDrain, flavors rebuilt).
  # Re-patch inactive=false each iteration until the type reaches Active.
  #
  # Active alone is too weak a readiness signal: the pool reports Active as soon as its
  # ClusterQueue is admitting again, while its ResourceFlavor is still being rebuilt and its
  # CPU capacity is still zero. A case starting against that pool sees a healthy phase and no
  # room, and fails for a reason that has nothing to do with what it tests. Wait for both.
  for _ in $(seq 1 40); do
    phase=$(kubectl get instancetype "$IT" -o jsonpath='{.status.phase}' 2>/dev/null)
    cpucap=$(kubectl get instancetype "$IT" -o jsonpath='{.status.cpu.capacity}' 2>/dev/null)
    [ "$phase" = "Active" ] && [ -n "$cpucap" ] && [ "$cpucap" != "0" ] && break
    kubectl patch instancetypes.worker.gpustack.ai "$IT" --type=merge \
      -p '{"spec":{"inactive":false}}' >/dev/null 2>&1 || true
    sleep 3
  done
  echo "[case-2] ${IT}: phase=${phase:-<none>} cpu.capacity=${cpucap:-<none>}"
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

# The drain only STOPS the Instance if the ClusterQueue still counts the reservation when the
# NodeQueueReconciler reacts. A Workload reports Admitted=True a few seconds BEFORE the CQ's
# reservingWorkloads counter reflects it; draining inside that lag makes NodeQueue observe an
# unreserved queue and empty it via the idle path (StopPolicy stays None, no HoldAndDrain) — the
# same path case-3 exercises — so the graceful drain-evict never runs and the Instance is not
# stopped. Gate the drain on the counted reservation so it deterministically hits the reserved
# state (HoldAndDrain), independent of that admission→accounting lag.
for _ in $(seq 1 20); do
  rw=$(kubectl get clusterqueue "$IT" -o jsonpath='{.status.reservingWorkloads}' 2>/dev/null)
  [ "${rw:-0}" -ge 1 ] && break
  sleep 2
done

# 3. Drain: exclude the node from management so the general flavor is deleted and the derived
#    InstanceType (the Instance's type) tears down. Toggle via the NodeFeature (NFD reverts a
#    direct node label).
echo "[case-2] draining: gpustack.ai/managed=false on all worker NodeFeatures"
echo "$WORKER_NFS" | xargs -r -I{} kubectl -n "$NS" patch {} --type=merge \
  -p '{"spec":{"labels":{"gpustack.ai/managed":"false"}}}'

# 4. The guaranteed drain effect: the pool's general ResourceFlavor is deleted (node-index driven,
#    independent of the running Instance/Workload). This confirms the managed-toggle propagated;
#    the fuller teardown (InstanceType survival + queue emptying) is CASE 3.
rf_gone=""
for _ in $(seq 1 40); do
  n=$(kubectl get resourceflavors.kueue.x-k8s.io -o name 2>/dev/null | grep -cE '/gpustack--.*-[0-9]+c$')
  [ "${n:-0}" -eq 0 ] && { rf_gone=1; break; }
  sleep 3
done
[ -n "$rf_gone" ] && record PASS "drain removes the pool ResourceFlavor" "no CPU (…-Nc) flavor left" \
  || record FAIL "drain removes the pool ResourceFlavor" "a CPU (…-Nc) flavor persists — managed-toggle did not propagate"

# 5. THE assertion: the Instance whose backing queue is now draining (HoldAndDrain) gets STOPPED,
#    not recreated. On a drain the queue evicts the Pod; the reconciler must set spec.stop (which
#    deletes the Pod) instead of recreating a Pod the drained queue can never admit. A ClusterQueue
#    watch re-enqueues the Instance so the stop is prompt even when no Pod event fires (poll — async).
stopped=""
for _ in $(seq 1 40); do
  s=$(kubectl -n default get instance gpustack-e2e-instance -o jsonpath='{.spec.stop}' 2>/dev/null)
  [ "$s" = "true" ] && { stopped=1; break; }
  sleep 3
done
[ -n "$stopped" ] && record PASS "instance STOPPED (not recreated)" "spec.stop=true" \
  || record FAIL "instance STOPPED (not recreated)" "spec.stop still ${s:-<unset>} — a running Instance must stop when its type drains"

# Ground truth in the logs — proves the stop-on-drain/gone branch ran (grep -c, not -q: under
# pipefail, -q closes the pipe early and kubectl logs gets SIGPIPE → non-zero pipeline).
logged=""
for _ in 1 2 3 4 5 6 7 8 9 10; do
  cnt=$(kubectl -n "$NS" logs deploy/gpustack-operator-worker --since=10m 2>/dev/null | grep -c "stop instance as its instance type is gone, deleting, or draining")
  [ "${cnt:-0}" -gt 0 ] && { logged=1; break; }
  sleep 3
done
[ -n "$logged" ] && record PASS "stop-on-drain/gone branch ran" "log: stop instance as its instance type is gone, deleting, or draining" \
  || record FAIL "stop-on-drain/gone branch ran" "log line absent — the stop branch may not have run"

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
