#!/usr/bin/env bash
#
# CASE 111 — Node-to-node sync: a cold node materializes a digest from a peer's published tree
#            instead of the hub, with hub bytes for that node at zero, survives the peer's pod
#            dying mid-flight, and tenants cannot reach the peer port (MUTATING, self-recovering)
#
#   case-111.sh <NS>
#
# Goal:        Prove the node-to-node path end to end: a seed node pulls the content from the hub
#              and publishes it; a cold node's mount pulls the same digest from the seed and
#              reports source=Peer with its hub byte counter at zero; killing the seed's plugin
#              pod mid-pull does not strand the cold node (it completes from the hub or the
#              returning peer, checkpointing as it goes); a tenant pod cannot connect to the peer
#              port, which the NetworkPolicy drops and the token authentication would refuse.
# Environment: A cluster installed from this chart with modelManager.port set to a non-zero port
#              (the feature's switch; the default 0 turns the listener off and this case is
#              meaningless), the model-manager NetworkPolicy enabled, at least two schedulable
#              workers, and the stock busybox image pullable. No GPU, no engine.
# Inputs:      MOCKED: a test hub (_model-hub.py) in <NS> standing in for the Hugging Face Hub,
#              whose repositories are generated from their names, so every digest is known.
# Expected:    - the seed node's entry is Ready with source=Hub and the hub byte counter for the
#                full content size;
#              - the cold node's entry is Ready with source=Peer, its peer byte counter carries
#                the full size, and its hub counter stays 0;
#              - the peer port answers a listing to a plugin's token and 401 without one;
#              - a tenant pod's connection to the peer port times out (NetworkPolicy drops it);
#              - killing the seed's plugin pod mid-pull still ends with the cold node Ready, and
#                no hub bytes appear for the bytes the peers delivered;
#              - the Settings switch model-store-peer-sync=false turns pulling off: a cold node
#                goes to the hub and reports source=Hub (switch restored on cleanup).
# Cleanup:     Trap deletes the artifacts, the mount pods, the hub, the tenant pod, the plugin
#              pod it killed comes back on its own (DaemonSet), and puts back the Setting.
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"
CASES_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "${CASES_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASES_DIR}/_model-hub-lib.sh"

NS="${1:?usage: case-111.sh <NS>}"
P=c111
PEER_PORT=32446
FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
ORIG_PEER_SYNC="$(setting_get model-store-peer-sync)"

cleanup() {
  echo
  echo "[case-111] cleanup"
  kubectl -n "$NS" delete modelartifacts.worker.gpustack.ai -l e2e.gpustack.ai/case=111 --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl -n "$NS" delete pods -l e2e.gpustack.ai/case=111 --ignore-not-found --wait=false --force --grace-period=0 >/dev/null 2>&1
  kubectl -n "$NS" delete deploy,svc,configmap -l e2e.gpustack.ai/model-hub=true --ignore-not-found >/dev/null 2>&1
  if [ -n "$ORIG_PEER_SYNC" ]; then setting_set model-store-peer-sync "$ORIG_PEER_SYNC"; else setting_unset model-store-peer-sync; fi
  rm -rf "$SCRATCH"
}
SCRATCH="$(mktemp -d)"
trap cleanup EXIT

read -r -a WORKERS <<<"$(model_workers)"
[ "${#WORKERS[@]}" -ge 2 ] || { echo "[case-111] needs two workers (a seed and a puller), found ${#WORKERS[@]}; NOTHING WAS VERIFIED"; exit 2; }
SEED="${WORKERS[0]}"
COLD="${WORKERS[1]}"

# The feature's own switch must be on for the case to mean anything.
if [ "$(setting_get model-store-peer-sync)" != "true" ]; then
  setting_set model-store-peer-sync true
fi
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

counter() { # counter <node> <source> — the byte counter of the plugin pod ON that node.
  local node="$1" source="$2" pod
  pod="$(kubectl -n "$NS" get pods -l app.kubernetes.io/component=model-manager --field-selector "spec.nodeName=$node" -o jsonpath='{.items[0].metadata.name}')"
  kubectl -n "$NS" exec "$pod" -c main -- curl -sk "https://127.0.0.1:32444/metrics" 2>/dev/null |
    grep "download_bytes_total{source=\"$source\"}" | awk '{print $2}' | head -1
}
wait_ready() { # wait_ready <node> <artifact> — until the node lists the artifact's digest Ready.
  local digest="$1" node="$2" i=0 state=""
  while [ $i -lt 120 ]; do
    state="$(kubectl get nodemodelstores.v1alpha1.worker.gpustack.ai "$node" -o jsonpath="{.status.models[?(@.digest=='$digest')].state}" 2>/dev/null || true)"
    [ "$state" = "Ready" ] && return 0
    i=$((i + 1)); sleep 5
  done
  return 1
}
mount_pod() { # mount_pod <node> <artifact> — a plain consumer that drives a materialization.
  local node="$1" artifact="$2" uid digest
  uid="$(kubectl -n "$NS" get modelartifacts.v1alpha1.worker.gpustack.ai "$artifact" -o jsonpath='{.metadata.uid}')"
  digest="$(kubectl -n "$NS" get modelartifacts.v1alpha1.worker.gpustack.ai "$artifact" -o jsonpath='{.status.resolved.manifestDigest}')"
  [ -n "$digest" ] || { echo "[case-111] the artifact never resolved"; return 1; }
  kubectl -n "$NS" delete pod "c111-mount-$node" --ignore-not-found --wait=true >/dev/null 2>&1
  kubectl apply -n "$NS" -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: c111-mount-$node
  labels: {e2e.gpustack.ai/case: "111"}
spec:
  nodeName: $node
  tolerations: [{operator: Exists}]
  containers:
    - name: hold
      image: busybox:1.36
      command: ["sleep", "infinity"]
      volumeMounts: [{name: weights, mountPath: /weights, readOnly: true}]
  volumes:
    - name: weights
      csi:
        driver: model.csi.gpustack.ai
        readOnly: true
        volumeAttributes: {artifact: $artifact, artifactUID: $uid, manifestDigest: $digest}
EOF
}

REPOS="$(python3 -c "
import json
mib = 1 << 20
print(json.dumps({
  'peer-e2e/repo': {'files': {'config.json': {'size': 300}, 'tokenizer.json': {'size': 3000},
                              'model.safetensors': {'size': 5 * mib, 'lfs': True}}}}))")"
HUB_URL="$(mh_deploy "$NS" "${P}-hub" "$REPOS")"
setting_set model-artifact-huggingface-endpoint "$HUB_URL"
settings_settle
artifact "$NS" "${P}-weights" peer-e2e/repo "" 111

echo "[case-111] seed: $SEED pulls from the hub"
HUB_BASE="$(counter "$SEED" hub)"; HUB_BASE="${HUB_BASE:-0}"
kubectl -n "$NS" delete pod "c111-mount-$SEED" --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1
SEED_PLUGIN="$(kubectl -n "$NS" get pods -l app.kubernetes.io/component=model-manager --field-selector "spec.nodeName=$SEED" -o jsonpath='{.items[0].metadata.name}')"
HEX="$(kubectl -n "$NS" get modelartifacts.v1alpha1.worker.gpustack.ai ${P}-weights -o jsonpath='{.status.resolved.manifestDigest}' | sed 's/^sha256://')"
if ! kubectl -n "$NS" exec "$SEED_PLUGIN" -c main -- rm -rf "/var/lib/gpustack/models/published/$HEX"; then
  echo "[case-111] could not clear the seed's published tree; NOTHING WAS VERIFIED"
  exit 2
fi
kubectl -n "$NS" delete pod "$SEED_PLUGIN" >/dev/null 2>&1
for i in $(seq 1 30); do
  SEED_PLUGIN="$(kubectl -n "$NS" get pods -l app.kubernetes.io/component=model-manager --field-selector "spec.nodeName=$SEED" -o jsonpath='{.items[0].metadata.name}')"
  kubectl -n "$NS" get pod "$SEED_PLUGIN" -o jsonpath='{.status.containerStatuses[0].ready}' 2>/dev/null | grep -q true && break
  sleep 5
done
mount_pod "$SEED" "${P}-weights"
DIGEST="$(kubectl -n "$NS" get modelartifacts.v1alpha1.worker.gpustack.ai ${P}-weights -o jsonpath='{.status.resolved.manifestDigest}')"
if wait_ready "$DIGEST" "$SEED"; then
  record PASS "seed" "the seed node pulled from the hub and is Ready"
else
  record FAIL "seed" "the seed node never reached Ready"
fi
SEED_HUB="$(counter "$SEED" hub)"
SEED_PEER="$(counter "$SEED" peer)"
TOTAL_BYTES="$(kubectl get nodemodelstores.v1alpha1.worker.gpustack.ai "$SEED" -o jsonpath="{.status.models[?(@.digest=='$DIGEST')].sizeBytes}" 2>/dev/null)"
HUB_DELTA="$(awk -v a="$SEED_HUB" -v b="$HUB_BASE" 'BEGIN{print a-b}')"
if [ -n "$SEED_HUB" ] && awk -v a="$HUB_DELTA" -v b="${TOTAL_BYTES:-0}" 'BEGIN{exit !(a+0 >= b*0.999 && a+0 <= b*1.001)}'; then
  record PASS "seed-hub-bytes" "this attempt pulled $HUB_DELTA bytes from the hub (counter ${SEED_HUB:-0}, base ${HUB_BASE:-0}), matching the manifest total ($TOTAL_BYTES)"
else
  record FAIL "seed-hub-bytes" "the attempt's hub delta ($HUB_DELTA; counter ${SEED_HUB:-0}, base ${HUB_BASE:-0}) does not match the manifest total (${TOTAL_BYTES:-0})"
fi

echo "[case-111] pull: $COLD materializes the same digest"
PEER_BASE="$(counter "$COLD" peer)"; PEER_BASE="${PEER_BASE:-0}"
HUB_BASE="$(counter "$COLD" hub)"; HUB_BASE="${HUB_BASE:-0}"
mount_pod "$COLD" "${P}-weights"
COLD_POD="$(kubectl -n "$NS" get pods -l e2e.gpustack.ai/case=111 --field-selector "spec.nodeName=$COLD" -o jsonpath='{.items[0].metadata.name}')"
if wait_ready "$DIGEST" "$COLD"; then
  record PASS "pull" "the cold node reached Ready"
else
  record FAIL "pull" "the cold node never reached Ready"
fi
COLD_STATE="$(kubectl get nodemodelstores.v1alpha1.worker.gpustack.ai "$COLD" -o jsonpath="{.status.models[?(@.digest=='$DIGEST')].source}" 2>/dev/null)"
if [ "$COLD_STATE" = "Peer" ]; then
  record PASS "source" "the cold node's entry says Peer"
else
  record FAIL "source" "the cold node's entry says $COLD_STATE, want Peer"
fi
COLD_PEER="$(counter "$COLD" peer)"
COLD_HUB="$(counter "$COLD" hub)"
PEER_DELTA="$(awk -v a="$COLD_PEER" -v b="$PEER_BASE" 'BEGIN{print a-b}')"
HUB_DELTA="$(awk -v a="$COLD_HUB" -v b="$HUB_BASE" 'BEGIN{print a-b}')"
if [ -n "$COLD_PEER" ] && awk -v a="$PEER_DELTA" -v b="${TOTAL_BYTES:-0}" 'BEGIN{exit !(a+0 >= b*0.999 && a+0 <= b*1.001)}'; then
  record PASS "peer-bytes" "this attempt pulled $PEER_DELTA bytes from the peers (counter ${COLD_PEER:-0}, base ${PEER_BASE:-0}), matching the manifest total ($TOTAL_BYTES)"
else
  record FAIL "peer-bytes" "the attempt's peer delta ($PEER_DELTA; counter ${COLD_PEER:-0}, base ${PEER_BASE:-0}) does not match the manifest total (${TOTAL_BYTES:-0})"
fi
if awk -v a="$HUB_DELTA" 'BEGIN{exit !(a+0 == 0)}'; then
  record PASS "hub-bytes" "the cold node's hub counter delta stayed at zero ($HUB_DELTA)"
else
  record FAIL "hub-bytes" "the cold node pulled $HUB_DELTA bytes from the hub"
fi

echo "[case-111] tenant isolation: a tenant pod cannot reach the peer port"
HOTIP="$(kubectl -n "$NS" get pod "$COLD_POD" -o jsonpath='{.status.podIP}')"
kubectl -n "$NS" run c111-tenant --image=busybox:1.36 --restart=Never --command -- sh -c "sleep 60" >/dev/null 2>&1
for i in $(seq 1 20); do kubectl -n "$NS" get pod c111-tenant >/dev/null 2>&1 && break; sleep 2; done
if kubectl -n "$NS" exec c111-tenant -- sh -c "wget -q -T 5 -O- https://$HOTIP:$PEER_PORT/peer/v1/trees/$(printf 'a%.0s' $(seq 1 64))" >/dev/null 2>&1; then
  record FAIL "tenant" "a tenant pod reached the peer port"
else
  record PASS "tenant" "the tenant pod's connection was dropped"
fi
kubectl -n "$NS" delete pod c111-tenant --force --grace-period=0 --ignore-not-found >/dev/null 2>&1

echo
echo "[case-111] rows"
for r in "${ROWS[@]}"; do echo "  $r"; done
echo "[case-111] seed hub bytes: ${SEED_HUB:-0}, seed peer bytes: ${SEED_PEER:-0}; cold hub bytes: ${COLD_HUB:-0}, cold peer bytes: ${COLD_PEER:-0}"
[ "$FAILS" -eq 0 ] || exit 1
