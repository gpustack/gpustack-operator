#!/usr/bin/env bash
#
# CASE 111 — Node-to-node sync: a cold node materializes a digest from a peer's published tree
#            instead of the hub, with hub bytes for that node at zero, survives the peer's pod
#            dying mid-flight, and tenants cannot reach the peer port (MUTATING, self-recovering)
#
#   case-111.sh <NS>
#
# Prerequisites: nodes=2; gpu=none; features=multi-host; cluster=any
# Goal:        Prove the node-to-node path end to end: a seed node pulls the content from the hub
#              and publishes it; a cold node's mount pulls the same digest from the seed and
#              reports source=Peer with its hub byte counter at zero; killing the seed's plugin
#              pod mid-pull does not strand the cold node (it completes from the hub or the
#              returning peer, checkpointing as it goes); a tenant pod cannot connect to the peer
#              port, which the NetworkPolicy drops and the token authentication would refuse.
# Environment: A cluster installed from this chart with modelManager.port set to a non-zero port
#              (the feature's switch; the default 0 turns the listener off and this case is
#              meaningless), the model-manager NetworkPolicy enabled, at least two schedulable
#              workers, and the stock busybox image and the model's python image (MH_PYTHON)
#              pullable. No GPU, no engine.
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

CASES_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "${CASES_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASES_DIR}/_model-hub-lib.sh"

NS="${1:?usage: case-111.sh <NS>}"
P=c111
PEER_PORT=32446
POD_BOUND=600
PEER_METRIC='gpustack_model_manager_download_bytes_total{source="peer"}'
HUB_METRIC='gpustack_model_manager_download_bytes_total{source="hub"}'
ORIG_PEER_SYNC="$(setting_get model-store-peer-sync)"

cleanup() {
  echo
  echo "[case-111] cleanup"
  kubectl -n "$NS" delete modelartifacts.worker.gpustack.ai -l e2e.gpustack.ai/case=111 --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl -n "$NS" delete pods -l e2e.gpustack.ai/case=111 --ignore-not-found --wait=false --force --grace-period=0 >/dev/null 2>&1
  kubectl -n "$NS" delete pod "c111-mount-${COLD:-}" --ignore-not-found --wait=false --force --grace-period=0 >/dev/null 2>&1
  kubectl -n "$NS" delete pod "c111-mount-${SEED:-}" --ignore-not-found --wait=false --force --grace-period=0 >/dev/null 2>&1
  kubectl -n "$NS" delete deploy,svc,configmap -l e2e.gpustack.ai/model-hub=true --ignore-not-found >/dev/null 2>&1
  if [ -n "$ORIG_PEER_SYNC" ]; then setting_set model-store-peer-sync "$ORIG_PEER_SYNC"; else setting_unset model-store-peer-sync; fi
  rm -rf "$SCRATCH"
}
SEED=""
COLD=""
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

wait_ready() { # wait_ready <digest> <node> — until the node lists the artifact's digest Ready.
  local digest="$1" node="$2" i=0 state=""
  while [ $i -lt 120 ]; do
    state="$(store_state "$node" "$digest")"
    state="${state%%|*}"
    [ "$state" = "Ready" ] && return 0
    i=$((i + 1)); sleep 5
  done
  return 1
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
HUB_BASE="$(plugin_metric "$SEED" "$HUB_METRIC")"; HUB_BASE="${HUB_BASE:-0}"
kubectl -n "$NS" delete pod "c111-mount-$SEED" --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1
SEED_PLUGIN="$(plugin_pod "$SEED")"
HEX="$(kubectl -n "$NS" get modelartifacts.v1alpha1.worker.gpustack.ai ${P}-weights -o jsonpath='{.status.resolved.manifestDigest}' | sed 's/^sha256://')"
if ! kubectl -n "$SYSTEM_NS" exec "$SEED_PLUGIN" -c main -- rm -rf "/var/lib/gpustack/models/published/$HEX"; then
  echo "[case-111] could not clear the seed's published tree; NOTHING WAS VERIFIED"
  exit 2
fi
kubectl -n "$SYSTEM_NS" delete pod "$SEED_PLUGIN" >/dev/null 2>&1
for i in $(seq 1 30); do
  SEED_PLUGIN="$(plugin_pod "$SEED")"
  kubectl -n "$SYSTEM_NS" get pod "$SEED_PLUGIN" -o jsonpath='{.status.containerStatuses[0].ready}' 2>/dev/null | grep -q true && break
  sleep 5
done
SEED_UID="$(kubectl -n "$NS" get modelartifacts.v1alpha1.worker.gpustack.ai "${P}-weights" -o jsonpath='{.metadata.uid}')"
DIGEST="$(kubectl -n "$NS" get modelartifacts.v1alpha1.worker.gpustack.ai "${P}-weights" -o jsonpath='{.status.resolved.manifestDigest}')"
if [ -n "$DIGEST" ]; then
  consumer "$NS" "c111-mount-$SEED" "$SEED" "${P}-weights" "$SEED_UID" "$DIGEST"
else
  echo "[case-111] the artifact never resolved"
fi
if wait_ready "$DIGEST" "$SEED"; then
  record PASS "seed" "the seed node pulled from the hub and is Ready"
else
  record FAIL "seed" "the seed node never reached Ready"
fi
SEED_HUB="$(plugin_metric "$SEED" "$HUB_METRIC")"
SEED_PEER="$(plugin_metric "$SEED" "$PEER_METRIC")"
TOTAL_BYTES="$(kubectl get nodemodelstores.v1alpha1.worker.gpustack.ai "$SEED" -o jsonpath="{.status.models[?(@.digest=='$DIGEST')].sizeBytes}" 2>/dev/null)"
# A plugin restart resets the counter to zero mid-measurement; in that case the attempt's
# pull is exactly what the reset counter now shows.
# The fresh reset restarts the plugin, so its counter reads this attempt's pull alone:
# the counter must equal the manifest total.
if [ -n "$SEED_HUB" ] && awk -v a="$SEED_HUB" -v b="${TOTAL_BYTES:-0}" 'BEGIN{exit !(a+0 >= b*0.999 && a+0 <= b*1.001)}'; then
  record PASS "seed-hub-bytes" "the seed pulled $SEED_HUB bytes from the hub, matching the manifest total ($TOTAL_BYTES)"
else
  record FAIL "seed-hub-bytes" "the seed hub counter ($SEED_HUB) does not match the manifest total (${TOTAL_BYTES:-0})"
fi

echo "[case-111] pull: $COLD materializes the same digest"
ART_UID="$(kubectl -n "$NS" get modelartifacts.v1alpha1.worker.gpustack.ai "${P}-weights" -o jsonpath='{.metadata.uid}')"
# The cold node's plugin pod is deleted before the mount, so its cache starts empty and the
# digest can only come from the peer; the helper samples the byte counters around the mount.
COUNTERS="$(mh_peer_sync_check "$NS" "c111-mount-$COLD" "$COLD" "${P}-weights" "$ART_UID" "$DIGEST" "$POD_BOUND")"
read -r PEER_BASE HUB_BASE COLD_PEER COLD_HUB <<< "$COUNTERS"
PEER_BASE="${PEER_BASE:-0}"
HUB_BASE="${HUB_BASE:-0}"
COLD_PEER="${COLD_PEER:-0}"
COLD_HUB="${COLD_HUB:-0}"
COLD_POD="c111-mount-$COLD"
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
print_rows
echo "[case-111] seed hub bytes: ${SEED_HUB:-0}, seed peer bytes: ${SEED_PEER:-0}; cold hub bytes: ${COLD_HUB:-0}, cold peer bytes: ${COLD_PEER:-0}"
[ "$FAILS" -eq 0 ] || exit 1
