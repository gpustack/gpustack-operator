#!/usr/bin/env bash
#
# CASE 16 — InstanceTypeFlavor catalog + declarative queue ownership   (MUTATING, self-recovering)
#
#   case-16.sh <NS>
#
# Goal:        On the general pool, prove the queue-ownership split (InstanceTypeReconciler owns the
#              CQ's existence + schedule labels + teardown; NodeQueueReconciler owns its quota /
#              StopPolicy; NodeFlavorReconciler authors the derived type) —
#              A: the aggregated, list-only InstanceTypeFlavor resource projects the fleet's pools, the
#                 generic (CPU-only) pool appearing with acceleratable=false;
#              B: deleting a backing ClusterQueue while its InstanceType still lives self-heals — the
#                 reconciler recreates it (a NEW uid) and the chain never stays down;
#              C: deleting an admin InstanceType holds its finalizer until the operator has deleted the
#                 backing ClusterQueue and Kueue has actually removed it (delete-then-wait), then releases.
# Environment: Any cluster with a materialized general pool. No GPU. Uses a throwaway admin type for the
#              teardown probe so the derived chain is untouched.
# Inputs:      All real, nothing mocked —
#              - reads the InstanceTypeFlavor catalog;
#              - deletes the derived general pool's backing ClusterQueue, then lets it self-heal;
#              - creates + deletes a throwaway admin InstanceType e2e-case16-teardown (generalGroup e2e16td).
# Expected:    - A — >=1 InstanceTypeFlavor row, and a generic (acceleratable=false) one;
#              - B — the ClusterQueue is recreated with a NEW uid while the InstanceType survives;
#              - C — the throwaway's backing ClusterQueue is created, then both it and the InstanceType
#                are removed on delete.
# Cleanup:     Trap force-strips the throwaway's finalizer if stuck, deletes the throwaway type + its
#              ClusterQueue, and waits for the derived general InstanceType to return to Active.
set -uo pipefail

NS="${1:?usage: case-16.sh <NS>}"

# The derived general InstanceType == its backing ClusterQueue name (one isolated CQ per pool).
IT=$(kubectl get instancetypes.worker.gpustack.ai \
  -o jsonpath='{.items[?(@.spec.acceleratable==false)].metadata.name}' 2>/dev/null | tr ' ' '\n' | grep -m1 'gpustack-')
[ -n "$IT" ] || { echo "no general InstanceType found — run case-1 first to materialize the chain"; exit 1; }
echo "general InstanceType / ClusterQueue: ${IT}"

PROBE=e2e-case16-teardown   # throwaway admin InstanceType for the teardown probe

cleanup() {
  echo
  echo "[case-16] cleanup: removing the throwaway type and waiting for the derived chain to recover"
  # Strip our finalizer only if it is stuck Terminating, then delete both objects best-effort.
  kubectl get instancetype "$PROBE" >/dev/null 2>&1 && \
    kubectl patch instancetype "$PROBE" --type=merge -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
  kubectl delete instancetype "$PROBE" --wait=false >/dev/null 2>&1 || true
  kubectl delete clusterqueue "$PROBE" --wait=false >/dev/null 2>&1 || true
  for _ in $(seq 1 50); do
    [ "$(kubectl get instancetype "$IT" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Active" ] && break
    sleep 3
  done
}
trap cleanup EXIT

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

# --- A. The aggregated InstanceTypeFlavor catalog lists the pools; the generic one is acceleratable=false. ---
n_flavors=$(kubectl get instancetypeflavors.worker.gpustack.ai --no-headers 2>/dev/null | grep -c . || true)
[ "${n_flavors:-0}" -ge 1 ] \
  && record PASS "InstanceTypeFlavor lists the fleet's pools" "${n_flavors} row(s)" \
  || record FAIL "InstanceTypeFlavor lists the fleet's pools" "no rows — the aggregated catalog must project the ResourceFlavors"
generic=$(kubectl get instancetypeflavors.worker.gpustack.ai \
  -o jsonpath='{.items[?(@.spec.acceleratable==false)].spec.generalGroup}' 2>/dev/null | tr ' ' '\n' | grep -m1 .)
[ -n "$generic" ] \
  && record PASS "generic pool surfaces as acceleratable=false" "generalGroup=${generic}" \
  || record FAIL "generic pool surfaces as acceleratable=false" "no acceleratable=false InstanceTypeFlavor row"

# --- B. Accidental delete of the backing ClusterQueue self-heals (recreated with a new uid). ---
uid_before=$(kubectl get clusterqueue "$IT" -o jsonpath='{.metadata.uid}' 2>/dev/null)
echo "[case-16] deleting ClusterQueue ${IT} (uid ${uid_before}) while its InstanceType still lives"
kubectl delete clusterqueue "$IT" --wait=false >/dev/null 2>&1 || true
recreated=""
for _ in $(seq 1 50); do
  uid_now=$(kubectl get clusterqueue "$IT" -o jsonpath='{.metadata.uid}' 2>/dev/null)
  [ -n "$uid_now" ] && [ "$uid_now" != "$uid_before" ] && { recreated=1; break; }
  sleep 3
done
# The InstanceType must stay alive throughout (recreation already proves the reconciler saw a live
# type); poll rather than single-shot to ride out a transient empty read from the API.
it_alive=""
for _ in $(seq 1 10); do
  [ -n "$(kubectl get instancetype "$IT" -o name 2>/dev/null)" ] && { it_alive=1; break; }
  sleep 2
done
{ [ -n "$recreated" ] && [ -n "$it_alive" ]; } \
  && record PASS "accidental CQ delete self-heals" "${IT} recreated (uid ${uid_now}), InstanceType survived" \
  || record FAIL "accidental CQ delete self-heals" "recreated='${recreated:-no}' it_alive='${it_alive:-no}' — a live InstanceType must recreate its deleted queue"

# Wait for the recreated queue to refill + report Active before moving on.
for _ in $(seq 1 40); do
  [ "$(kubectl get instancetype "$IT" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Active" ] && break
  sleep 3
done

# --- C. Deleting an InstanceType tears down its ClusterQueue, then releases (delete-then-wait). ---
cat <<EOF | kubectl apply -f - >/dev/null 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: InstanceType
metadata:
  name: ${PROBE}
spec:
  generalGroup: e2e16td
  acceleratable: false
  os: linux
  arch: amd64
  unitResources:
    cpu: "1"
    ram: "2Gi"
  localStorage: "100Gi"
EOF
# The reconciler ensures the name-identical backing ClusterQueue exists.
cq_made=""
for _ in $(seq 1 30); do
  kubectl get clusterqueue "$PROBE" >/dev/null 2>&1 && { cq_made=1; break; }
  sleep 2
done
[ -n "$cq_made" ] \
  && record PASS "InstanceType creates its backing ClusterQueue" "${PROBE} queue present" \
  || record FAIL "InstanceType creates its backing ClusterQueue" "${PROBE} queue never appeared"

echo "[case-16] deleting InstanceType ${PROBE}; the finalizer must hold until its queue is gone"
kubectl delete instancetype "$PROBE" --wait=false >/dev/null 2>&1 || true
torn=""
for _ in $(seq 1 50); do
  it_gone=$(kubectl get instancetype "$PROBE" -o name 2>/dev/null)
  cq_gone=$(kubectl get clusterqueue "$PROBE" -o name 2>/dev/null)
  [ -z "$it_gone" ] && [ -z "$cq_gone" ] && { torn=1; break; }
  sleep 3
done
[ -n "$torn" ] \
  && record PASS "delete-then-wait teardown" "${PROBE}: InstanceType + backing ClusterQueue both removed" \
  || record FAIL "delete-then-wait teardown" "it_gone='${it_gone:-present}' cq_gone='${cq_gone:-present}' — teardown must delete the queue then release the finalizer"

echo
echo "== CASE 16 — InstanceTypeFlavor catalog + declarative queue ownership =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). The InstanceTypeReconciler owns the backing queue's existence"
  echo "(recreate on accidental delete; delete-then-wait teardown); the InstanceTypeFlavor catalog"
  echo "projects the pools. See instancetype.go / nodequeue.go / extensionapis."
  exit 1
fi
echo "CASE 16 PASS"
