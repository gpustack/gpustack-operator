#!/usr/bin/env bash
#
# CASE 109 — The NodeModelStore's lifecycle: it goes with its Node, and the plugin rewrites it from
#            the node's disk after a restart and after the object is deleted (MUTATING,
#            self-recovering; the Node row AUTO-SKIPS without docker access to the kind nodes)
#
#   case-109.sh <NS>
#
# Goal:        Prove the object is reconstructible and owned: a plugin restart and a deletion of the
#              object each end with the entries the node held before, rewritten by the plugin from
#              disk rather than from the previous status; deleting the Node object collects its
#              NodeModelStore through the owner reference, and the node registered again gets a new
#              one.
# Environment: A kind cluster installed from this chart with modelManager.enabled (the default), two
#              workers, the stock python image pullable, and docker reaching the node containers for
#              the Node row (kubelet is stopped there while its Node is deleted, then started to
#              register the node again).
# Inputs:      MOCKED: a test hub (_model-hub.py) in <NS>. Real: the plugin, kubelet, the worker and
#              the garbage collector.
# Expected:    - after the plugin Pod on a node is deleted and replaced, the node's entries (digest,
#                state, size, referenced) equal the ones before;
#              - after the node's NodeModelStore is deleted, the worker creates it again and the
#                plugin rewrites the same entries;
#              - after a worker's Node object is deleted, its NodeModelStore is gone; after kubelet
#                registers the node again, a new one exists with a new UID.
# Cleanup:     Trap deletes the Pods, the artifact and the hub, puts back the Settings it changed,
#              and starts kubelet on a node whose Node object it deleted if it is still missing.
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"
CASES_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "${CASES_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASES_DIR}/_model-hub-lib.sh"

NS="${1:?usage: case-109.sh <NS>}"
P=c109
LABEL="e2e.gpustack.ai/case=109"
FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
ORIG_ENDPOINT="$(setting_get model-artifact-huggingface-endpoint)"
ORIG_DELIVERY="$(setting_get model-artifact-delivery-mode)"
DELETED_NODE=""

cleanup() {
  echo
  echo "[case-109] cleanup"
  if [ -n "$DELETED_NODE" ] && ! kubectl get node "$DELETED_NODE" >/dev/null 2>&1; then
    docker exec "$DELETED_NODE" systemctl start kubelet >/dev/null 2>&1
  fi
  kubectl -n "$NS" delete pods -l e2e.gpustack.ai/consumer=true --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1
  kubectl -n "$NS" delete modelartifacts.worker.gpustack.ai -l "$LABEL" --ignore-not-found >/dev/null 2>&1
  kubectl -n "$NS" delete deploy,svc,configmap -l e2e.gpustack.ai/model-hub=true --ignore-not-found >/dev/null 2>&1
  kubectl -n "$NS" delete configmap "${P}-hub-script" --ignore-not-found >/dev/null 2>&1
  if [ -n "$ORIG_ENDPOINT" ]; then setting_set model-artifact-huggingface-endpoint "$ORIG_ENDPOINT"; else setting_unset model-artifact-huggingface-endpoint; fi
  if [ -n "$ORIG_DELIVERY" ]; then setting_set model-artifact-delivery-mode "$ORIG_DELIVERY"; else setting_unset model-artifact-delivery-mode; fi
}
trap cleanup EXIT

# entries NODE: the node's entries as sorted "digest state size referenced" lines.
entries() {
  kubectl get nodemodelstores.worker.gpustack.ai "$1" -o json 2>/dev/null | python3 -c "
import json, sys
for m in sorted(json.load(sys.stdin).get('status', {}).get('models', []), key=lambda m: m['digest']):
    print(m['digest'], m['state'], m.get('sizeBytes', 0), m.get('referenced', False))"
}
# wait_entries NODE WANT BOUND: waits until the node's entries equal WANT; 0 once they do.
wait_entries() {
  for _ in $(seq 1 $(( $3 / 3 ))); do
    [ "$(entries "$1")" = "$2" ] && return 0
    sleep 3
  done
  return 1
}
plugin_ready() { # plugin_ready NODE BOUND
  local pod
  for _ in $(seq 1 $(( $2 / 3 ))); do
    pod="$(plugin_pod "$1")"
    [ -n "$pod" ] && [ "$(kubectl -n "$SYSTEM_NS" get pod "$pod" -o jsonpath='{.status.containerStatuses[?(@.name=="main")].ready}')" = true ] && return 0
    sleep 3
  done
  return 1
}

read -r -a WORKERS <<<"$(model_workers)"
[ "${#WORKERS[@]}" -ge 2 ] || { echo "[case-109] needs two workers, found ${#WORKERS[@]}; NOTHING WAS VERIFIED"; exit 2; }
W1="${WORKERS[0]}" W2="${WORKERS[1]}"
if ! kubectl get csidriver model.csi.gpustack.ai >/dev/null 2>&1; then
  echo "[case-109] the model-manager plugin is not installed; NOTHING WAS VERIFIED"
  exit 2
fi
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

REPOS='{"e2e/lifecycle-model": {"files": {"config.json": {"size": 300}, "model.safetensors": {"size": 1048576, "lfs": true}}}}'
HUB_URL="$(mh_deploy "$NS" "${P}-hub" "$REPOS")"
setting_set model-artifact-huggingface-endpoint "$HUB_URL"
setting_set model-artifact-delivery-mode Node
settings_settle

artifact "$NS" "${P}-art" e2e/lifecycle-model "" 109
DIGEST="$(wait_resolved "$NS" "${P}-art" 120)"
UID_ART="$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-art" -o jsonpath='{.metadata.uid}')"
consumer "$NS" "${P}-pod" "$W1" "${P}-art" "$UID_ART" "$DIGEST"
if [ -z "$DIGEST" ] || ! pod_ready "$NS" "${P}-pod" 300; then
  record FAIL "the consumer runs on ${W1}" "$(pod_mount_events "$NS" "${P}-pod" | tail -1)"
  print_rows
  exit 1
fi
BEFORE=""
for _ in $(seq 1 20); do
  BEFORE="$(entries "$W1")"
  echo "$BEFORE" | grep -q "^${DIGEST} Ready [0-9]* True$" && break
  sleep 3
done
echo "[case-109] ${W1} holds:"
echo "$BEFORE"

# ---------------------------------------------------------------- plugin restart
kubectl -n "$SYSTEM_NS" delete pod "$(plugin_pod "$W1")" --wait=true --timeout=120s >/dev/null 2>&1
if plugin_ready "$W1" 180 && wait_entries "$W1" "$BEFORE" 120; then
  record PASS "after the plugin restarts the node's entries are rewritten as before" "$W1"
else
  record FAIL "the plugin rewrites the same entries after a restart" "${W1}: $(entries "$W1" | tr '\n' ';')"
fi

# ---------------------------------------------------------------- object deleted
OLD_UID="$(kubectl get nodemodelstores.worker.gpustack.ai "$W1" -o jsonpath='{.metadata.uid}')"
kubectl delete nodemodelstores.v1alpha1.worker.gpustack.ai "$W1" --wait=true >/dev/null 2>&1
NEW_UID=""
for _ in $(seq 1 40); do
  NEW_UID="$(kubectl get nodemodelstores.worker.gpustack.ai "$W1" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
  [ -n "$NEW_UID" ] && [ "$NEW_UID" != "$OLD_UID" ] && break
  sleep 3
done
if [ -n "$NEW_UID" ] && [ "$NEW_UID" != "$OLD_UID" ] && wait_entries "$W1" "$BEFORE" 420; then
  record PASS "a deleted object comes back and the plugin rewrites the same entries from disk" "${W1}: ${OLD_UID:0:8} -> ${NEW_UID:0:8}"
else
  record FAIL "the object is recreated with the node's entries" "${W1}: uid ${NEW_UID:-<none>}, $(entries "$W1" | tr '\n' ';')"
fi

# ---------------------------------------------------------------- the Node goes, its object with it
if ! docker exec "$W2" true >/dev/null 2>&1; then
  record SKIP "the NodeModelStore goes with its Node" "no docker access to the node ${W2}"
else
  OLD_UID="$(kubectl get nodemodelstores.worker.gpustack.ai "$W2" -o jsonpath='{.metadata.uid}')"
  DELETED_NODE="$W2"
  # kubelet stops first: a running kubelet registers its Node again within seconds, and the worker
  # then adopts the object for the new Node before the garbage collector reaches it.
  docker exec "$W2" systemctl stop kubelet >/dev/null 2>&1
  kubectl delete node "$W2" --wait=true >/dev/null 2>&1
  gone=""
  for _ in $(seq 1 40); do
    kubectl get nodemodelstores.worker.gpustack.ai "$W2" >/dev/null 2>&1 || { gone=1; break; }
    sleep 3
  done
  if [ -n "$OLD_UID" ] && [ -n "$gone" ]; then
    record PASS "deleting the Node collects its NodeModelStore" "$W2"
  else
    record FAIL "the NodeModelStore goes with its Node" "${W2}: uid ${OLD_UID:-<none>} still there"
  fi
  docker exec "$W2" systemctl start kubelet >/dev/null 2>&1
  NEW_UID=""
  for _ in $(seq 1 80); do
    NEW_UID="$(kubectl get nodemodelstores.worker.gpustack.ai "$W2" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
    [ -n "$NEW_UID" ] && break
    sleep 3
  done
  if [ -n "$NEW_UID" ] && [ "$NEW_UID" != "$OLD_UID" ]; then
    record PASS "the node registered again gets a new NodeModelStore" "${W2}: ${NEW_UID:0:8}"
    DELETED_NODE=""
  else
    record FAIL "the node registered again gets its object back" "${W2}: $(kubectl get node "$W2" -o name 2>&1)"
  fi
fi

print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-109] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-109] PASS"
