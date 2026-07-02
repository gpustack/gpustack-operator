#!/usr/bin/env bash
#
# CASE 3 — Managed-toggle scopes node onboarding (Story 5)   (MUTATING, self-recovering)
#
#   case-3.sh <NS>
#
# Story 5: an admin scopes which nodes are onboarded. Excluding a node from
# management (gpustack.ai/managed=false) must remove its contribution from the
# pool. Post-refactor there is NO drain tombstone on the ResourceFlavor anymore
# (specs/2026-06-29-instancetype-unified-pool-refactor.md F3a: "no node contributes
# → the flavor is deleted"); teardown flows through the InstanceType finalizer
# (F5d): the derived InstanceType's pool loses its ResourceFlavor → the InstanceType
# is deleted → its finalizer drives the backing ClusterQueue through HoldAndDrain
# and removes it.
#
# So this case asserts the NEW observable (not the old schedule.gpustack.ai/drain
# annotation): after the toggle the node's general ResourceFlavor is DELETED and the
# derived InstanceType tears down (backing CQ HoldAndDrain or gone).
#
# IMPORTANT: toggle via the NodeFeature, not the node directly (NFD reverts a direct
# node label). Verify against a CONTINUOUSLY RUNNING operator.
#
# Self-recovering: restores gpustack.ai/managed on exit and waits for the chain to
# rebuild so a following case still finds an Active InstanceType.
set -uo pipefail

NS="${1:?usage: case-3.sh <NS>}"
# Target the general (CPU) pool: non-accelerated (no GPU needed) and fed by every managed node, so
# the case behaves the same on a 1-node local cluster and an N-node real one.
IT=$(kubectl get instancetypes.worker.gpustack.ai \
  -o jsonpath='{.items[?(@.spec.acceleratable==false)].metadata.name}' 2>/dev/null | tr ' ' '\n' | grep -m1 'gpustack-')
# One of the pool's CPU ResourceFlavors (named "${IT}-${count}c"), kept as a full type/name path so
# `kubectl get "$RF"` works — watched so we see it deleted on de-manage.
RF=$(kubectl get resourceflavors.kueue.x-k8s.io -o name 2>/dev/null | grep -E "/${IT}-[0-9]+c$" | head -1)
[ -n "$RF" ] && [ -n "$IT" ] || { echo "no general RF/InstanceType found — run case-1 first to materialize the chain"; exit 1; }
# Every <node>-gpustack-worker NodeFeature: de-managing ALL of them removes a pool that spans more
# than one node (a single node's toggle leaves the others' flavors behind).
WORKER_NFS=$(kubectl -n "$NS" get nodefeatures -o name 2>/dev/null | grep -E -- '-gpustack-worker$')
[ -n "$WORKER_NFS" ] || { echo "no <node>-gpustack-worker NodeFeatures found"; exit 1; }
echo "general InstanceType: ${IT}  ResourceFlavor: ${RF#*/}  draining $(echo "$WORKER_NFS" | grep -c .) node(s)"

restore() {
  echo
  echo "[case-3] restoring gpustack.ai/managed=true on all nodes and waiting for the chain to rebuild"
  echo "$WORKER_NFS" | xargs -r -I{} kubectl -n "$NS" patch {} --type=merge \
    -p '{"spec":{"labels":{"gpustack.ai/managed":"true"}}}' 2>/dev/null || true
  for _ in $(seq 1 40); do
    p=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.status.phase}' 2>/dev/null)
    [ "$p" = "Active" ] && break
    sleep 3
  done
}
trap restore EXIT

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

# Toggle out of management via the NodeFeature (NFD would revert a direct node label).
echo "[case-3] toggling gpustack.ai/managed=false on all worker NodeFeatures"
echo "$WORKER_NFS" | xargs -r -I{} kubectl -n "$NS" patch {} --type=merge \
  -p '{"spec":{"labels":{"gpustack.ai/managed":"false"}}}'

# --- Assertion A: the node's general ResourceFlavor is DELETED (no tombstone). ---
gone=""
for _ in $(seq 1 30); do
  kubectl get "$RF" >/dev/null 2>&1 || { gone=1; break; }
  sleep 3
done
[ -n "$gone" ] && record PASS "flavor deleted on de-manage" "${RF#*/} gone (F3a: no drain tombstone)" \
  || record FAIL "flavor deleted on de-manage" "${RF#*/} still present — NodeFlavorReconciler did not drop the unmanaged node"

# --- Assertion B: the derived InstanceType tears down (CQ HoldAndDrain, or IT/CQ gone). ---
torn=""
for _ in $(seq 1 40); do
  sp=$(kubectl get clusterqueue "$IT" -o jsonpath='{.spec.stopPolicy}' 2>/dev/null)
  exists=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o name 2>/dev/null)
  cqexists=$(kubectl get clusterqueue "$IT" -o name 2>/dev/null)
  if [ "$sp" = "HoldAndDrain" ] || [ -z "$exists" ] || [ -z "$cqexists" ]; then torn=1; break; fi
  sleep 3
done
[ -n "$torn" ] && record PASS "derived InstanceType tears down" "backing CQ HoldAndDrain or removed (F5d finalizer)" \
  || record FAIL "derived InstanceType tears down" "InstanceType/CQ still Active — the derived pool did not tear down after its flavor vanished"

echo
echo "== CASE 3 — Managed-toggle scopes node onboarding (Story 5) =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). Confirm the operator was NOT restarted between toggle and assertion."
  echo "Post-refactor teardown is delete-based (F3a) + InstanceType-finalizer HoldAndDrain (F5d),"
  echo "not the old schedule.gpustack.ai/drain tombstone. See references/drain-recycle.md."
  exit 1
fi
echo "CASE 3 PASS"
