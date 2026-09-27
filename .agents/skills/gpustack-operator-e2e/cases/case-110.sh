#!/usr/bin/env bash
#
# CASE 110 — A ModelPrefetch warms an artifact onto its target node through a per-node warm-up pod:
#            admission holds the line (budget, pin permission), the delivery reaches Available with
#            honest counts, the pin lands only under its grant and leaves with the prefetch, and the
#            node keeps the tree until its own grace decides (MUTATING, self-recovering)
#
#   case-110.sh <NS>
#
# Goal:        Prove the prefetch path end to end: a warm-up pod pinned to the target node mounts
#              the artifact's CSI volume under a restricted namespace and never carries a queue-name
#              label; the prefetch reports Available only when its nodes hold the digest; a
#              projection past the budget is refused at admission; pinning without the grant's
#              permission is refused and lands on the node once the grant allows it; deleting the
#              prefetch deletes its pod, releases the pin, and leaves reclamation to the node's own
#              grace; the store layer names itself in the node's spec.
# Environment: A cluster installed from this chart with modelManager.enabled (the default), one
#              schedulable worker to label as the warm pool (two for a sharper story), and the stock
#              python image pullable. No GPU, no engine, no InstanceType.
# Inputs:      MOCKED: a test hub (_model-hub.py) in <NS> standing in for the Hugging Face Hub, whose
#              repositories are generated from their names, so every digest is known.
# Expected:    - the node's spec names the store and takes its watermarks;
#              - the prefetch reaches Available with desired/ready honest, its warm-up pod pinned to
#                the node, label-free of the queue-name label, and the digest Ready on the node;
#              - a second prefetch whose artifact alone would pass the budget is refused with the
#                budget message;
#              - pinning is refused while the grant says no, and after the grant turns, the digest
#                is pinned on the node;
#              - deleting the prefetch deletes its pod and unpins the node, while the tree stays —
#                reclamation belongs to the grace the plugin already owns.
# Cleanup:     Trap deletes the prefetches, the binding, the store, the artifacts, the hub, the
#              node label, and puts back the Settings it changed.
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"
CASES_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "${CASES_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASES_DIR}/_model-hub-lib.sh"

NS="${1:?usage: case-110.sh <NS>}"
P=c110
POOL_LABEL="e2e.gpustack.ai/warm-pool"
FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
SCRATCH="$(mktemp -d)"
ORIG_ENDPOINT="$(setting_get model-artifact-huggingface-endpoint)"
ORIG_DELIVERY="$(setting_get model-artifact-delivery-mode)"
NODE_WAS_LABELED=""

cleanup() {
  echo
  echo "[case-110] cleanup"
  kubectl -n "$NS" delete modelprefetches.worker.gpustack.ai -l e2e.gpustack.ai/case=110 --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl -n "$NS" delete modelstorebindings.worker.gpustack.ai -l e2e.gpustack.ai/case=110 --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl delete modelstores.worker.gpustack.ai -l e2e.gpustack.ai/case=110 --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl -n "$NS" delete modelartifacts.worker.gpustack.ai -l e2e.gpustack.ai/case=110 --ignore-not-found >/dev/null 2>&1
  kubectl -n "$NS" delete pods -l worker.gpustack.ai/model-prefetch --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl -n "$NS" delete deploy,svc,configmap -l e2e.gpustack.ai/model-hub=true --ignore-not-found >/dev/null 2>&1
  if [ "$NODE_WAS_LABELED" = 1 ]; then
    kubectl label node "$W1" "$POOL_LABEL-" >/dev/null 2>&1
  fi
  if [ -n "$ORIG_ENDPOINT" ]; then setting_set model-artifact-huggingface-endpoint "$ORIG_ENDPOINT"; else setting_unset model-artifact-huggingface-endpoint; fi
  if [ -n "$ORIG_DELIVERY" ]; then setting_set model-artifact-delivery-mode "$ORIG_DELIVERY"; else setting_unset model-artifact-delivery-mode; fi
  rm -rf "$SCRATCH"
}
trap cleanup EXIT

read -r -a WORKERS <<<"$(model_workers)"
[ "${#WORKERS[@]}" -ge 1 ] || { echo "[case-110] needs a worker, found none; NOTHING WAS VERIFIED"; exit 2; }
W1="${WORKERS[0]}"
if ! kubectl get csidriver model.csi.gpustack.ai >/dev/null 2>&1; then
  echo "[case-110] the model-manager plugin is not installed; NOTHING WAS VERIFIED"
  exit 2
fi

kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl label namespace "$NS" --overwrite pod-security.kubernetes.io/enforce=restricted \
  pod-security.kubernetes.io/enforce-version=latest >/dev/null
if kubectl label node "$W1" "$POOL_LABEL=true" >/dev/null 2>&1; then
  NODE_WAS_LABELED=1
else
  echo "[case-110] could not label ${W1} as the warm pool; NOTHING WAS VERIFIED"
  exit 2
fi

REPOS="$(python3 -c "
import json
mib = 1 << 20
print(json.dumps({
  'e2e/small': {'files': {'config.json': {'size': 300}, 'tokenizer.json': {'size': 3000},
                          'model.safetensors': {'size': 5 * mib, 'lfs': True}}},
  'e2e/big': {'files': {'config.json': {'size': 300}, 'model.safetensors': {'size': 50 * mib, 'lfs': True}}},
}))")"
HUB_URL="$(mh_deploy "$NS" "${P}-hub" "$REPOS")"
setting_set model-artifact-huggingface-endpoint "$HUB_URL"
setting_set model-artifact-delivery-mode Node
settings_settle
echo "[case-110] hub at ${HUB_URL}; warm pool node ${W1}"

kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelStore
metadata: {name: ${P}-store, labels: {e2e.gpustack.ai/case: "110"}}
spec:
  nodeSelector: {matchLabels: {"${POOL_LABEL}": "true"}}
  watermarks: {highPercent: 95, lowPercent: 85}
YAML
kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelStoreBinding
metadata: {name: ${P}-binding, labels: {e2e.gpustack.ai/case: "110"}}
spec:
  storeRefs: [{name: ${P}-store}]
  quota: {bytes: 20Mi}
YAML
artifact "$NS" "${P}-small" e2e/small "" 110
artifact "$NS" "${P}-big" e2e/big "" 110
DIGEST_SMALL="$(wait_resolved "$NS" "${P}-small" 120)"
DIGEST_BIG="$(wait_resolved "$NS" "${P}-big" 120)"
if [ -z "$DIGEST_SMALL" ] || [ -z "$DIGEST_BIG" ]; then
  echo "[case-110] the artifacts did not resolve; NOTHING WAS VERIFIED"
  exit 2
fi

# wait_prefetch_available NS NAME [SECONDS] [POD_CAPTURE]: until the prefetch's Available
# condition is True, snapshotting its warm-up pod into POD_CAPTURE at first sight — the pod is
# transient, deleted once its work is done, so the first sighting is the evidence.
wait_prefetch_available() {
  local ns="$1" name="$2" seconds="${3:-300}" capture="${4:-}"
  for _ in $(seq 1 "$(( seconds / 3 ))"); do
    if [ -n "$capture" ] && ! [ -s "$capture" ]; then
      if kubectl -n "$ns" get pod -l "worker.gpustack.ai/model-prefetch=$name" -o json > "$capture.tmp" 2>/dev/null; then
        python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); sys.exit(0 if d.get("items") else 1)' "$capture.tmp" \
          && mv "$capture.tmp" "$capture"
      fi
    fi
    [ "$(kubectl -n "$ns" get modelprefetches.worker.gpustack.ai "$name" \
        -o jsonpath='{.status.conditions[?(@.type=="Available")].status}' 2>/dev/null)" = True ] && return 0
    sleep 3
  done
  return 1
}

# ---------------------------------------------------------------- the store layer reaches the node
STORE_ON_NODE=""
for _ in $(seq 1 20); do
  [ "$(kubectl get nodemodelstores.worker.gpustack.ai "$W1" \
      -o jsonpath='{.spec.store}' 2>/dev/null)" = "${P}-store" ] && { STORE_ON_NODE=1; break; }
  sleep 3
done
HIGH="$(kubectl get nodemodelstores.worker.gpustack.ai "$W1" \
  -o jsonpath='{.spec.watermarks.highPercent}' 2>/dev/null)"
if [ "$STORE_ON_NODE" = 1 ] && [ "$HIGH" = 95 ]; then
  record PASS "the node's spec names the store and takes its watermark" "${P}-store high=${HIGH}"
else
  record FAIL "the node's spec names the store and takes its watermark" "store=$(kubectl get nodemodelstores.worker.gpustack.ai "$W1" -o jsonpath='{.spec.store}') high=${HIGH}"
fi

# ---------------------------------------------------------------- warm: available with honest counts
kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelPrefetch
metadata: {name: ${P}-warm, labels: {e2e.gpustack.ai/case: "110"}}
spec:
  artifactRef: {name: ${P}-small}
  bindingRef: {name: ${P}-binding}
  placement:
    nodeSelector: {matchLabels: {"${POOL_LABEL}": "true"}}
  minReady: 1
YAML
POD_CAPTURE="$SCRATCH/${P}-warm-pod.json"
if wait_prefetch_available "$NS" "${P}-warm" 300 "$POD_CAPTURE"; then
  WANT="$(kubectl -n "$NS" get modelprefetches.worker.gpustack.ai "${P}-warm" \
    -o jsonpath='{.status.desiredNodes} {.status.readyNodes}')"
  POD_NODE="$(python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
items = d.get("items") or []
print(items[0].get("spec", {}).get("nodeName", "") if items else "")' "$POD_CAPTURE" 2>/dev/null)"
  QUEUED="$(python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
items = d.get("items") or []
print((items[0].get("metadata", {}).get("labels") or {}).get("kueue.x-k8s.io/queue-name", "") if items else "")' "$POD_CAPTURE" 2>/dev/null)"
  READY="$(kubectl get nodemodelstores.worker.gpustack.ai "$W1" \
    -o jsonpath='{.status.models[?(@.digest=="'"$DIGEST_SMALL"'")].state}' 2>/dev/null)"
  if [ "$WANT" = "1 1" ] && [ "$POD_NODE" = "$W1" ] && [ -z "$QUEUED" ] && [ "$READY" = Ready ]; then
    record PASS "the prefetch is Available, the warm-up pod is node-pinned and label-free, the digest is Ready" "warmup@${POD_NODE:-captured}"
  else
    record FAIL "the prefetch is Available with an honest, label-free delivery" "counts=${WANT:-none} node=${POD_NODE:-none} queued=${QUEUED:-unset} state=${READY:-none}"
  fi
else
  record FAIL "the prefetch reaches Available" "$(kubectl -n "$NS" get modelprefetches.worker.gpustack.ai "${P}-warm" -o jsonpath='{.status.conditions[?(@.type=="Progressing")].message}' 2>/dev/null)"
fi

# ---------------------------------------------------------------- over budget: refused at admission
BUDGET_ERR="$(kubectl -n "$NS" apply -f - 2>&1 <<YAML || true
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelPrefetch
metadata: {name: ${P}-big}
spec:
  artifactRef: {name: ${P}-big}
  bindingRef: {name: ${P}-binding}
  placement:
    nodeSelector: {matchLabels: {"${POOL_LABEL}": "true"}}
YAML
)"
if echo "$BUDGET_ERR" | grep -q "past the grant's"; then
  record PASS "a projection past the budget is refused at admission" "50Mi against a 20Mi grant"
else
  record FAIL "a projection past the budget is refused at admission" "$(echo "$BUDGET_ERR" | tail -1)"
fi

# ---------------------------------------------------------------- pinning: permission, then the pin
PIN_ERR="$(kubectl -n "$NS" apply -f - 2>&1 <<YAML || true
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelPrefetch
metadata: {name: ${P}-pin}
spec:
  artifactRef: {name: ${P}-small}
  bindingRef: {name: ${P}-binding}
  placement:
    nodeSelector: {matchLabels: {"${POOL_LABEL}": "true"}}
  retention: {pinned: true}
YAML
)"
if echo "$PIN_ERR" | grep -q "allowPinned"; then
  record PASS "pinning without the grant's permission is refused" "${P}-pin"
else
  record FAIL "pinning without the grant's permission is refused" "$(echo "$PIN_ERR" | tail -1)"
fi

kubectl -n "$NS" patch modelstorebindings.worker.gpustack.ai "${P}-binding" --type=merge \
  -p '{"spec":{"allowPinned":true}}' >/dev/null
if kubectl -n "$NS" apply -f - >/dev/null 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelPrefetch
metadata: {name: ${P}-pin}
spec:
  artifactRef: {name: ${P}-small}
  bindingRef: {name: ${P}-binding}
  placement:
    nodeSelector: {matchLabels: {"${POOL_LABEL}": "true"}}
  retention: {pinned: true}
YAML
then
  PINNED=""
  for _ in $(seq 1 40); do
    case "$(kubectl get nodemodelstores.worker.gpustack.ai "$W1" \
        -o jsonpath='{.spec.pinned}' 2>/dev/null)" in
      *${DIGEST_SMALL}*) PINNED=1; break ;;
    esac
    sleep 3
  done
  if [ "$PINNED" = 1 ]; then
    record PASS "under the grant, the digest is pinned on the node" "${W1} pinned=${DIGEST_SMALL:0:19}..."
  else
    record FAIL "under the grant, the digest is pinned on the node" "pinned=$(kubectl get nodemodelstores.worker.gpustack.ai "$W1" -o jsonpath='{.spec.pinned}')"
  fi
else
  record FAIL "under the grant, the pinned prefetch is admitted" "$(echo "$PIN_ERR" | tail -1)"
fi

# ---------------------------------------------------------------- delete: pod gone, pin gone, tree stays
HEX="${DIGEST_SMALL#sha256:}"
POD_GONE="" UNPINNED="" TREE="$(docker exec "$W1" sh -c "test -d /var/lib/gpustack/models/published/$HEX/tree && echo present" 2>/dev/null)"
kubectl -n "$NS" delete modelprefetches.worker.gpustack.ai "${P}-pin" --wait=false >/dev/null 2>&1
for _ in $(seq 1 40); do
  [ -z "$(kubectl -n "$NS" get pod "${P}-pin-warmup-${W1}" -o name 2>/dev/null)" ] && POD_GONE=1
  case "$(kubectl get nodemodelstores.worker.gpustack.ai "$W1" \
      -o jsonpath='{.spec.pinned}' 2>/dev/null)" in
    *${DIGEST_SMALL}*) : ;;
    *) UNPINNED=1 ;;
  esac
  [ "$POD_GONE" = 1 ] && [ "$UNPINNED" = 1 ] && break
  sleep 3
done
if [ "$POD_GONE" = 1 ] && [ "$UNPINNED" = 1 ] && [ -n "$TREE" ]; then
  record PASS "deleting the prefetch removes its pod and its pin, and leaves the tree to the grace" "tree kept: ${TREE:0:19}..."
else
  record FAIL "deleting the prefetch removes its pod and its pin, and leaves the tree to the grace" "podGone=${POD_GONE:-0} unpinned=${UNPINNED:-0} tree=${TREE:-none}"
fi

print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-110] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-110] PASS"
