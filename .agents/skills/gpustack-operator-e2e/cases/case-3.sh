#!/usr/bin/env bash
#
# CASE 3 — Managed-toggle scopes node onboarding   (MUTATING, self-recovering)
#
#   case-3.sh <NS>
#
# Goal:        De-managing every node feeding a pool DELETES its ResourceFlavor but keeps the
#              InstanceType alive with its backing ClusterQueue's resource groups emptied
#              (drain-when-no-flavors), and restoring the nodes refills the queue and reactivates
#              the type. Under the queue-ownership split, NodeFlavorReconciler drops the flavor and
#              NodeQueueReconciler empties the quota — the InstanceType is never deleted for lack of
#              flavors (an idle pool has no reservations, so quota is emptied directly, StopPolicy
#              stays None, and identity is kept).
# Environment: A real cluster with a materialized general pool and a CONTINUOUSLY RUNNING operator
#              (must NOT be restarted between toggle and assertion). No GPU. Targets the general
#              (CPU) pool fed by every managed node — same on a 1-node or an N-node cluster.
# Inputs:      All real, nothing mocked — toggles gpustack.ai/managed=false, then back to true, on
#              every <node>-gpustack-worker NodeFeature (toggle via the NodeFeature, not the node
#              directly: NFD reverts a direct node label).
# Expected:    - A — the pool's general ResourceFlavor is DELETED (no drain tombstone);
#              - B — the InstanceType SURVIVES and its ClusterQueue's spec.resourceGroups is emptied
#                (length 0);
#              - C — restoring the nodes refills resourceGroups and the InstanceType returns to Active.
# Cleanup:     Trap restores gpustack.ai/managed=true on all nodes and waits for the InstanceType to
#              return to Active so a following case still finds a healthy chain.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-3.sh <NS>}"
# Target the general (CPU) pool: non-accelerated (no GPU needed) and fed by every managed node, so
# the case behaves the same on a 1-node local cluster and an N-node real one.
IT=$(kubectl get instancetypes.worker.gpustack.ai \
  -o jsonpath='{.items[?(@.spec.acceleratable==false)].metadata.name}' 2>/dev/null | tr ' ' '\n' | grep -m1 'gpustack-')
# One of the pool's CPU ResourceFlavors (named gpustack--${gKey}-${os}-${arch}-${count}c — keyed by
# the node's real CPU, so its name is independent of the collapsed InstanceType name), kept as a
# full type/name path so `kubectl get "$RF"` works — watched so we see it deleted on de-manage.
RF=$(kubectl get resourceflavors.kueue.x-k8s.io -o name 2>/dev/null | grep -E '/gpustack--.*-[0-9]+c$' | head -1)
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
  for _ in $(seq 1 90); do
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
# NFD can take a few minutes to propagate a NodeFeature label change to the Node object on a busy
# cluster, and the flavor deletion only follows once the Node is seen unmanaged — wait generously.
gone=""
for _ in $(seq 1 90); do
  kubectl get "$RF" >/dev/null 2>&1 || { gone=1; break; }
  sleep 3
done
[ -n "$gone" ] && record PASS "flavor deleted on de-manage" "${RF#*/} gone (no drain tombstone)" \
  || record FAIL "flavor deleted on de-manage" "${RF#*/} still present — NodeFlavorReconciler did not drop the unmanaged node"

# --- Assertion B: the InstanceType SURVIVES (not deleted); its backing queue's quota is emptied. ---
survived=""
for _ in $(seq 1 60); do
  exists=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o name 2>/dev/null)
  cqexists=$(kubectl get clusterqueue "$IT" -o name 2>/dev/null)
  rg=$(kubectl get clusterqueue "$IT" -o json 2>/dev/null | python3 -c "import json,sys
try: print(len(json.load(sys.stdin).get('spec',{}).get('resourceGroups') or []))
except Exception: print(-1)" 2>/dev/null)
  if [ -n "$exists" ] && [ -n "$cqexists" ] && [ "$rg" = "0" ]; then survived=1; break; fi
  sleep 3
done
[ -n "$survived" ] && record PASS "InstanceType survives flavor loss; queue emptied" "${IT} kept, resourceGroups=0 (drain-when-no-flavors, idle)" \
  || record FAIL "InstanceType survives flavor loss; queue emptied" "exists='${exists:-gone}' cqexists='${cqexists:-gone}' resourceGroups='${rg}' — the type must survive with its queue emptied, not tear down"

# --- Assertion C: restoring the nodes refills the queue and the InstanceType reactivates. ---
echo "[case-3] restoring gpustack.ai/managed=true; expecting the pool to refill and reactivate"
echo "$WORKER_NFS" | xargs -r -I{} kubectl -n "$NS" patch {} --type=merge \
  -p '{"spec":{"labels":{"gpustack.ai/managed":"true"}}}' >/dev/null 2>&1
reactivated=""
for _ in $(seq 1 90); do
  p=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.status.phase}' 2>/dev/null)
  rg=$(kubectl get clusterqueue "$IT" -o json 2>/dev/null | python3 -c "import json,sys
try: print(len(json.load(sys.stdin).get('spec',{}).get('resourceGroups') or []))
except Exception: print(0)" 2>/dev/null)
  if [ "$p" = "Active" ] && [ "${rg:-0}" -gt 0 ]; then reactivated=1; break; fi
  sleep 3
done
[ -n "$reactivated" ] && record PASS "pool reactivates when nodes return" "${IT} Active, resourceGroups refilled" \
  || record FAIL "pool reactivates when nodes return" "phase='${p:-?}' resourceGroups='${rg:-0}' — the pool must refill and reactivate"

echo
echo "== CASE 3 — Managed-toggle scopes node onboarding =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). Confirm the operator was NOT restarted between toggle and assertion."
  echo "Under the queue-ownership split the flavor is deleted but the InstanceType survives — the"
  echo "NodeQueueReconciler empties/drains the queue (never deletes the type). See references/drain-recycle.md."
  exit 1
fi
echo "CASE 3 PASS"
