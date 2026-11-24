#!/usr/bin/env bash
#
# CASE 22 — Download progress reporting (30s/5% thresholds, progress subresource RBAC),
#           ModelPrefetch, expected-digest anchor, and node-to-node peer sync (zero hub bytes,
#           NetworkPolicy isolation)
#           (MUTATING, self-recovering; peer-sync legs AUTO-SKIP on one worker or E2E_OFFLINE=1)
#
#   case-22.sh <NS>
#
# Prerequisites: nodes=2; gpu=none; features=multi-host; cluster=any; inputs=E2E_OFFLINE,MH_PYTHON
# Goal:        Prove four node-delivery contracts against one controlled test hub:
#                (1) Expected-digest anchor: matching anchor resolves with digestSource=Hub and a
#                    commit; foreign anchor is refused with DigestMismatch naming both digests;
#                    unreachable hub hands identity to the anchor (digestSource=Expected, no
#                    revision, flat hub log); Engine delivery holds anchored artifacts with
#                    AnchorNeedsNodeDelivery; Expected identity mounts from peer with zero hub
#                    requests.
#                (2) ModelStore / ModelStoreBinding / ModelPrefetch: node spec takes store name and
#                    watermarks; prefetch reaches Available with honest counts and node-pinned,
#                    queue-label-free warm-up Pod; over-budget projection is refused at admission;
#                    pinning requires allowPinned on the grant; deleting prefetch removes its Pod
#                    and pin while leaving the tree to grace.
#                (3) Download progress & v1 read-only views: slow download (128Ki bandwidth) writes
#                    at least two intermediate downloadedBytes, at most 20 progress-only writes at
#                    least 30s apart, and nothing at rest; status.nodes moves in 5% steps and ends
#                    with ready and no percent; v1 progress subresource answers live and
#                    non-decreasing to its own namespace only, naming no node; no tenant leaks into
#                    NodeModelStore; v1 views are read-only and print expected columns.
#                (4) Dedicated node-to-node peer sync & NetworkPolicy: seed pulls from hub matching
#                    manifest total; cold node pulls from peer (source=Peer, peer delta == total,
#                    hub delta == 0); tenant Pod connection to peer port (32446) is dropped.
# Environment: A cluster installed from this chart with modelManager.enabled (and modelManager.port
#              non-zero + NetworkPolicy enabled for peer sync), stock python and busybox images
#              pullable, and two schedulable workers (peer legs auto-skip with one worker). NO GPU.
# Inputs:      MOCKED: one test hub (_model-hub.py) in <NS> serving five repositories. Real: the
#              plugin, kubelet, worker, aggregated API server, NetworkPolicy and RBAC (two
#              impersonated ServiceAccounts: reader and plain). Settings are saved and restored.
# Expected:    - anchor: twin resolves & seeds W1; matching anchor Hub; mismatch DigestMismatch;
#                unreachable hub -> Expected with flat log; Engine holds AnchorNeedsNodeDelivery;
#                W2 mounts Expected from peer with flat hub log.
#              - prefetch: node spec takes store/watermark; Available 1/1; over-budget refused;
#                pinning refused without allowPinned then pinned with it; delete unpins & keeps tree.
#              - progress: 5% steps; ready with no percent; 2 live non-decreasing progress reads;
#                RBAC allows in-ns reader, forbids cross-ns and plain reader, leaks no node; no
#                tenant in NodeModelStore; v1 views read-only & print columns; watch stats meet
#                30s/20-write/quiet bounds.
#              - peer sync: seed hub bytes == manifest total; cold source=Peer, peer delta == total,
#                hub delta == 0; tenant Pod blocked from peer port 32446.
# Cleanup:     Trap kills watch process, deletes all Pods, artifacts, prefetches, stores, bindings,
#              deployments, RBAC, hub, tenant pod and second namespace, removes warm-pool node
#              label, restores PSA labels on <NS>, restores all Settings, and removes scratch dir.
set -uo pipefail

CASES_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "${CASES_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASES_DIR}/_model-hub-lib.sh"

NS="${1:?usage: case-22.sh <NS>}"
export CASE_ID=22
if [ "$NS" = "$SYSTEM_NS" ]; then
  echo "[case-22] refusing the system namespace ${SYSTEM_NS}; usage: case-22.sh <NS>" >&2
  exit 2
fi
NSB="${NS}-b"
P=c22
HUB="${P}-hub"
LABEL="e2e.gpustack.ai/case=22"
POOL_LABEL="e2e.gpustack.ai/warm-pool"
COMMIT_BOUND=120
ANCHOR_BOUND=240
POD_BOUND=300
PEER_POD_BOUND=600
PEER_PORT=32446
PEER_METRIC='gpustack_model_manager_download_bytes_total{source="peer"}'
HUB_METRIC='gpustack_model_manager_download_bytes_total{source="hub"}'
WRONG="sha256:$(printf 'f%.0s' $(seq 64))"
SCRATCH="$(mktemp -d)"
WATCH_PID=""
W1=""
W2=""
NODE_WAS_LABELED=""
ORIG_PSS_ENFORCE=""
ORIG_PSS_VERSION=""
READER="system:serviceaccount:${NS}:${P}-reader"
PLAIN="system:serviceaccount:${NS}:${P}-plain"

settings_save model-artifact-huggingface-endpoint model-artifact-delivery-mode \
  model-store-download-bandwidth model-store-peer-sync

restore_label() {
  if [ -n "$4" ]; then
    kubectl label "$1" "$2" "$3=$4" --overwrite >/dev/null 2>&1
  else
    kubectl label "$1" "$2" "$3-" >/dev/null 2>&1
  fi
}

ns_label() {
  kubectl get namespace "$1" -o json 2>/dev/null \
    | python3 -c 'import json,sys; print(json.load(sys.stdin).get("metadata",{}).get("labels",{}).get(sys.argv[1],""))' "$2"
}

cleanup() {
  local ns
  echo
  echo "[case-22] cleanup"
  [ -n "$WATCH_PID" ] && kill "$WATCH_PID" >/dev/null 2>&1
  for ns in "$NS" "$NSB"; do
    kubectl -n "$ns" delete pods -l e2e.gpustack.ai/consumer=true --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1
    kubectl -n "$ns" delete modelartifacts.worker.gpustack.ai -l "$LABEL" --ignore-not-found >/dev/null 2>&1
  done
  kubectl -n "$NS" delete pods "${P}-seed" "${P}-peer" "${P}-tenant" "${P}-mount-${W1}" "${P}-mount-${W2}" \
    --ignore-not-found --wait=false --force --grace-period=0 >/dev/null 2>&1
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "${P}-md-match" "${P}-md-expected" --ignore-not-found >/dev/null 2>&1
  kubectl -n "$NS" delete modelprefetches.worker.gpustack.ai -l "$LABEL" --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl -n "$NS" delete modelprefetches.worker.gpustack.ai "${P}-warm" "${P}-pin" "${P}-big" --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl -n "$NS" delete modelstorebindings.worker.gpustack.ai -l "$LABEL" --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl delete modelstores.worker.gpustack.ai -l "$LABEL" --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl -n "$NS" delete pods -l worker.gpustack.ai/model-prefetch --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl -n "$NS" delete role,rolebinding,serviceaccount -l "$LABEL" --ignore-not-found >/dev/null 2>&1
  kubectl -n "$NS" delete deploy,svc,configmap -l e2e.gpustack.ai/model-hub=true --ignore-not-found >/dev/null 2>&1
  kubectl -n "$NS" delete deploy,svc "${HUB}" --ignore-not-found >/dev/null 2>&1
  kubectl -n "$NS" delete configmap "${HUB}-script" --ignore-not-found >/dev/null 2>&1
  kubectl delete namespace "$NSB" --ignore-not-found --wait=false >/dev/null 2>&1
  if [ "$NODE_WAS_LABELED" = 1 ]; then
    kubectl label node "$W1" "$POOL_LABEL-" >/dev/null 2>&1
  fi
  restore_label namespace "$NS" pod-security.kubernetes.io/enforce "$ORIG_PSS_ENFORCE"
  restore_label namespace "$NS" pod-security.kubernetes.io/enforce-version "$ORIG_PSS_VERSION"
  settings_restore
  rm -rf "$SCRATCH"
}
trap cleanup EXIT

get_uid() { kubectl -n "$1" get modelartifacts.worker.gpustack.ai "$2" -o jsonpath='{.metadata.uid}'; }

nodes_of() {
  kubectl -n "$1" get modelartifacts.worker.gpustack.ai "$2" \
    -o jsonpath='{.status.nodes.ready} {.status.nodes.downloading} {.status.nodes.failed} {.status.nodes.downloadingPercent}'
}

field() { python3 -c "import json,sys; print(json.loads(sys.argv[1]).get(sys.argv[2], ''))" "$1" "$2" 2>/dev/null; }

has_columns() {
  local header="$1" col rest
  shift
  rest=" $(echo "$header" | tr -s ' ') "
  for col in "$@"; do
    case "$rest" in *" $col "*) rest="${rest#* "$col" }"; rest=" $rest" ;; *) return 1 ;; esac
  done
}

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

wait_node_ready() {
  local digest="$1" node="$2" i=0 state=""
  while [ "$i" -lt 120 ]; do
    state="$(store_state "$node" "$digest")"
    state="${state%%|*}"
    [ "$state" = "Ready" ] && return 0
    i=$((i + 1)); sleep 5
  done
  return 1
}

read -r -a WORKERS <<<"$(model_workers)"
[ "${#WORKERS[@]}" -ge 1 ] || { echo "[case-22] needs a worker, found none; NOTHING WAS VERIFIED"; exit 2; }
W1="${WORKERS[0]}"
[ "${#WORKERS[@]}" -ge 2 ] && W2="${WORKERS[1]}"
if ! kubectl get csidriver model.csi.gpustack.ai >/dev/null 2>&1; then
  echo "[case-22] the model-manager plugin is not installed; NOTHING WAS VERIFIED"
  exit 2
fi

for ns in "$NS" "$NSB"; do
  kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
done
ORIG_PSS_ENFORCE="$(ns_label "$NS" pod-security.kubernetes.io/enforce)"
ORIG_PSS_VERSION="$(ns_label "$NS" pod-security.kubernetes.io/enforce-version)"
kubectl label namespace "$NS" --overwrite pod-security.kubernetes.io/enforce=restricted \
  pod-security.kubernetes.io/enforce-version=latest >/dev/null
if kubectl label node "$W1" "$POOL_LABEL=true" >/dev/null 2>&1; then
  NODE_WAS_LABELED=1
else
  echo "[case-22] could not label ${W1} as the warm pool; NOTHING WAS VERIFIED"
  exit 2
fi

# RBAC subjects for the progress subresource authorization checks.
kubectl apply -f - >/dev/null <<YAML
apiVersion: v1
kind: ServiceAccount
metadata: {name: ${P}-reader, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "22"}}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: ${P}-plain, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "22"}}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: ${P}-reader, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "22"}}
rules:
  - {apiGroups: [worker.gpustack.ai], resources: [modelartifacts, modelartifacts/progress], verbs: [get]}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: ${P}-plain, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "22"}}
rules:
  - {apiGroups: [worker.gpustack.ai], resources: [modelartifacts], verbs: [get]}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: ${P}-reader, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "22"}}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: ${P}-reader}
subjects: [{kind: ServiceAccount, name: ${P}-reader, namespace: ${NS}}]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: ${P}-plain, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "22"}}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: ${P}-plain}
subjects: [{kind: ServiceAccount, name: ${P}-plain, namespace: ${NS}}]
YAML

REPOS="$(python3 -c "
import json
mib = 1 << 20
print(json.dumps({
  'e2e/anchored': {'files': {'config.json': {'size': 300}, 'tokenizer.json': {'size': 3000}}},
  'e2e/small': {'files': {'config.json': {'size': 300}, 'tokenizer.json': {'size': 3000},
                          'model.safetensors': {'size': 5 * mib, 'lfs': True}}},
  'e2e/big': {'files': {'config.json': {'size': 300}, 'model.safetensors': {'size': 50 * mib, 'lfs': True}}},
  'e2e/progress-model': {'files': {'config.json': {'size': 300},
                                   'model.safetensors': {'size': 24 * mib, 'lfs': True}}},
  'peer-e2e/repo': {'files': {'config.json': {'size': 300}, 'tokenizer.json': {'size': 3000},
                              'model.safetensors': {'size': 5 * mib, 'lfs': True}}},
}))")"
HUB_URL="$(mh_deploy "$NS" "$HUB" "$REPOS")"
setting_set model-artifact-huggingface-endpoint "$HUB_URL" >/dev/null
setting_set model-artifact-delivery-mode Node >/dev/null
settings_settle
echo "[case-22] hub at ${HUB_URL}; worker ${W1}"

# =============================================================================================
# Phase 1a: Expected-digest anchor
# =============================================================================================
echo "== Phase 1a: expected-digest anchor =="
artifact "$NS" "${P}-twin" e2e/anchored "" 22
DIGEST="$(wait_resolved "$NS" "${P}-twin" "$COMMIT_BOUND")"
ANCHOR_OK=1
if [ -n "$DIGEST" ]; then
  record PASS "twin resolved" "digest ${DIGEST:0:19}..."
else
  record FAIL "twin resolved" "no digest within ${COMMIT_BOUND}s: $(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-twin" -o jsonpath='{.status.conditions[?(@.type=="Resolved")].message}' 2>/dev/null)"
  ANCHOR_OK=0
fi

if [ "$ANCHOR_OK" = 1 ]; then
  TWIN_UID="$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-twin" -o jsonpath='{.metadata.uid}')"
  consumer "$NS" "${P}-seed" "${WORKERS[0]}" "${P}-twin" "$TWIN_UID" "$DIGEST"
  if pod_ready "$NS" "${P}-seed" "$POD_BOUND"; then
    record PASS "seed node" "the first worker holds the published tree"
  else
    record FAIL "seed node" "not Running within ${POD_BOUND}s: $(pod_mount_events "$NS" "${P}-seed" | head -1)"
  fi

  artifact "$NS" "${P}-match" e2e/anchored "" 22 "
  expectedDigest: $DIGEST"
  MATCH_DIGEST="$(wait_resolved "$NS" "${P}-match" "$COMMIT_BOUND")"
  MATCH_SRC="$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-match" -o jsonpath='{.status.resolved.digestSource}' 2>/dev/null)"
  MATCH_REV="$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-match" -o jsonpath='{.status.resolved.revision}' 2>/dev/null)"
  if [ "$MATCH_DIGEST" = "$DIGEST" ] && [ "$MATCH_SRC" = Hub ] && [ -n "$MATCH_REV" ]; then
    record PASS "match resolves" "digestSource Hub at commit ${MATCH_REV:0:12}..."
  else
    record FAIL "match resolves" "digest '${MATCH_DIGEST:0:19}...' source '$MATCH_SRC' revision '$MATCH_REV'"
  fi

  artifact "$NS" "${P}-mismatch" e2e/anchored "" 22 "
  expectedDigest: $WRONG"
  R=""
  for _ in $(seq 1 45); do
    R="$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-mismatch" \
      -o jsonpath='{.status.conditions[?(@.type=="Resolved")].reason}' 2>/dev/null)"
    [ "$R" = DigestMismatch ] && break
    sleep 2
  done
  MISMATCH_MSG="$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-mismatch" -o jsonpath='{.status.conditions[?(@.type=="Resolved")].message}' 2>/dev/null)"
  if [ "$R" = DigestMismatch ] && printf '%s' "$MISMATCH_MSG" | grep -qF "$WRONG" && printf '%s' "$MISMATCH_MSG" | grep -qF "$DIGEST"; then
    record PASS "mismatch refused" "DigestMismatch names the anchor and the hub's digest"
  else
    record FAIL "mismatch refused" "reason '$R', message '${MISMATCH_MSG:0:160}'"
  fi

  LOG_BEFORE="$(mh_log "$NS" "$HUB" | wc -l | tr -d ' ')"
  setting_set model-artifact-huggingface-endpoint "http://${P}-absent.${NS}.svc:8080" >/dev/null
  settings_settle
  artifact "$NS" "${P}-expected" e2e/anchored "" 22 "
  expectedDigest: $DIGEST"
  EXPECTED_DIGEST="$(wait_resolved "$NS" "${P}-expected" "$ANCHOR_BOUND")"
  EXPECTED_SRC="$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-expected" -o jsonpath='{.status.resolved.digestSource}' 2>/dev/null)"
  EXPECTED_REV="$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-expected" -o jsonpath='{.status.resolved.revision}' 2>/dev/null)"
  LOG_DURING="$(mh_log "$NS" "$HUB" | wc -l | tr -d ' ')"
  if [ "$EXPECTED_DIGEST" = "$DIGEST" ] && [ "$EXPECTED_SRC" = Expected ] && [ -z "$EXPECTED_REV" ] && [ "$LOG_DURING" = "$LOG_BEFORE" ]; then
    record PASS "anchored identity" "digestSource Expected, no revision, hub log flat (${LOG_BEFORE} -> ${LOG_DURING})"
  else
    record FAIL "anchored identity" "digest '${EXPECTED_DIGEST:0:19}...' source '$EXPECTED_SRC' revision '$EXPECTED_REV' log ${LOG_BEFORE} -> ${LOG_DURING}"
  fi
  setting_set model-artifact-huggingface-endpoint "$HUB_URL" >/dev/null
  settings_settle

  IT="$(kubectl get acceleratortypes.worker.gpustack.ai -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
  AT_LINE=""
  [ -n "$IT" ] && AT_LINE="      acceleratorType: ${IT}"
  setting_set model-artifact-delivery-mode Engine >/dev/null
  settings_settle
  for pair in "${P}-match:${P}-md-match:cannot anchor-verify" "${P}-expected:${P}-md-expected:no resolved commit"; do
    ART="${pair%%:*}"; REST="${pair#*:}"; MD="${REST%%:*}"; WANT="${REST#*:}"
    kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata: {name: ${MD}, namespace: ${NS}}
spec:
  engine: {name: vLLM, image: "${MH_PYTHON}"}
  model: {name: qwen, artifactRef: {name: ${ART}}}
  roles:
    - name: server
${AT_LINE:+$AT_LINE
}      image: ${MH_PYTHON}
      replicas: 1
YAML
    REASON=""
    for _ in $(seq 1 30); do
      REASON="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "$MD" \
        -o jsonpath='{.status.conditions[?(@.type=="WeightsReady")].reason}' 2>/dev/null)"
      [ "$REASON" = AnchorNeedsNodeDelivery ] && break
      sleep 2
    done
    MSG="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "$MD" -o jsonpath='{.status.conditions[?(@.type=="WeightsReady")].message}' 2>/dev/null)"
    PODS="$(kubectl -n "$NS" get pods -l "worker.gpustack.ai/model-deployment=$MD" --no-headers 2>/dev/null | wc -l | tr -d ' ')"
    if [ "$REASON" = AnchorNeedsNodeDelivery ] && printf '%s' "$MSG" | grep -qF "$WANT" && [ "$PODS" = 0 ]; then
      record PASS "engine block" "$MD held with AnchorNeedsNodeDelivery ('$WANT'), no Pod"
    else
      record FAIL "engine block" "$MD reason '$REASON', pods $PODS, message '${MSG:0:120}'"
    fi
    kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "$MD" --ignore-not-found >/dev/null 2>&1
  done
  setting_set model-artifact-delivery-mode Node >/dev/null
  settings_settle

  if [ "${#WORKERS[@]}" -lt 2 ]; then
    record SKIP "peer mount" "needs two schedulable workers, found ${#WORKERS[@]}"
  elif [ "${E2E_OFFLINE:-0}" = 1 ]; then
    record SKIP "peer mount" "E2E_OFFLINE=1"
  else
    EXPECTED_UID="$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-expected" -o jsonpath='{.metadata.uid}')"
    kubectl -n "$SYSTEM_NS" delete pod -l app.kubernetes.io/component=model-manager \
      --field-selector "spec.nodeName=${W2}" --wait=true --timeout=120s >/dev/null 2>&1
    for _ in $(seq 1 30); do
      PP="$(plugin_pod "$W2")"
      [ -n "$PP" ] && [ "$(kubectl -n "$SYSTEM_NS" get pod "$PP" -o jsonpath='{.status.phase}' 2>/dev/null)" = Running ] && break
      sleep 3
    done
    LOG_BEFORE="$(mh_log "$NS" "$HUB" | wc -l | tr -d ' ')"
    PEER_BEFORE="$(plugin_metric "$W2" "$PEER_METRIC")"
    HUB_BEFORE="$(plugin_metric "$W2" "$HUB_METRIC")"
    consumer "$NS" "${P}-peer" "$W2" "${P}-expected" "$EXPECTED_UID" "$DIGEST"
    if pod_ready "$NS" "${P}-peer" "$POD_BOUND"; then
      LOG_AFTER="$(mh_log "$NS" "$HUB" | wc -l | tr -d ' ')"
      PEER_AFTER="$(plugin_metric "$W2" "$PEER_METRIC")"
      HUB_AFTER="$(plugin_metric "$W2" "$HUB_METRIC")"
      if [ "$LOG_AFTER" != "$LOG_BEFORE" ]; then
        record FAIL "peer mount" "the hub was asked (${LOG_BEFORE} -> ${LOG_AFTER}) though the identity is Expected"
      elif [ "$PEER_AFTER" -gt "$PEER_BEFORE" ] && [ "$HUB_AFTER" -le "$HUB_BEFORE" ]; then
        record PASS "peer mount" "the second node took ${PEER_AFTER} peer bytes and ${HUB_AFTER} hub bytes"
      elif [ "$PEER_AFTER" -le "$PEER_BEFORE" ] && [ "$HUB_AFTER" -le "$HUB_BEFORE" ]; then
        record PASS "peer mount" "the mount is a hit on pre-warmed content (bytes flat); the hub log stayed flat"
      else
        record FAIL "peer mount" "the second node pulled from the hub (${HUB_BEFORE} -> ${HUB_AFTER})"
      fi
    else
      record FAIL "peer mount" "not Running within ${POD_BOUND}s: $(pod_mount_events "$NS" "${P}-peer" | head -1)"
    fi
  fi
else
  record SKIP "seed node" "the twin did not resolve"
  record SKIP "match resolves" "the twin did not resolve"
  record SKIP "mismatch refused" "the twin did not resolve"
  record SKIP "anchored identity" "the twin did not resolve"
  record SKIP "engine block" "the twin did not resolve"
  record SKIP "peer mount" "the twin did not resolve"
fi

# =============================================================================================
# Phase 1b: ModelStore, ModelStoreBinding & ModelPrefetch
# =============================================================================================
echo "== Phase 1b: ModelStore, ModelStoreBinding and ModelPrefetch =="
kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelStore
metadata: {name: ${P}-store, labels: {e2e.gpustack.ai/case: "22"}}
spec:
  nodeSelector: {matchLabels: {"${POOL_LABEL}": "true"}}
  watermarks: {highPercent: 95, lowPercent: 85}
YAML
kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelStoreBinding
metadata: {name: ${P}-binding, labels: {e2e.gpustack.ai/case: "22"}}
spec:
  storeRefs: [{name: ${P}-store}]
  quota: {bytes: 20Mi}
YAML
artifact "$NS" "${P}-small" e2e/small "" 22
artifact "$NS" "${P}-big" e2e/big "" 22
DIGEST_SMALL="$(wait_resolved "$NS" "${P}-small" 120)"
DIGEST_BIG="$(wait_resolved "$NS" "${P}-big" 120)"
STORE_OK=1
if [ -z "$DIGEST_SMALL" ] || [ -z "$DIGEST_BIG" ]; then
  echo "[case-22] the prefetch artifacts did not resolve"
  STORE_OK=0
fi

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

if [ "$STORE_OK" = 1 ]; then
  kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelPrefetch
metadata: {name: ${P}-warm, labels: {e2e.gpustack.ai/case: "22"}}
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
        *"${DIGEST_SMALL}"*) PINNED=1; break ;;
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

  HEX="${DIGEST_SMALL#sha256:}"
  POD_GONE="" UNPINNED="" TREE="$(docker exec "$W1" sh -c "test -d /var/lib/gpustack/models/published/$HEX/tree && echo present" 2>/dev/null)"
  kubectl -n "$NS" delete modelprefetches.worker.gpustack.ai "${P}-pin" --wait=false >/dev/null 2>&1
  for _ in $(seq 1 40); do
    [ -z "$(kubectl -n "$NS" get pod "${P}-pin-warmup-${W1}" -o name 2>/dev/null)" ] && POD_GONE=1
    case "$(kubectl get nodemodelstores.worker.gpustack.ai "$W1" \
        -o jsonpath='{.spec.pinned}' 2>/dev/null)" in
      *"${DIGEST_SMALL}"*) : ;;
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
else
  record SKIP "the prefetch is Available, the warm-up pod is node-pinned and label-free, the digest is Ready" "the prefetch artifacts did not resolve"
  record SKIP "a projection past the budget is refused at admission" "the prefetch artifacts did not resolve"
  record SKIP "pinning without the grant's permission is refused" "the prefetch artifacts did not resolve"
  record SKIP "under the grant, the digest is pinned on the node" "the prefetch artifacts did not resolve"
  record SKIP "deleting the prefetch removes its pod and its pin, and leaves the tree to the grace" "the prefetch artifacts did not resolve"
fi

# Clean up ModelStore layer before slow download.
kubectl -n "$NS" delete modelprefetches.worker.gpustack.ai -l "$LABEL" --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1
kubectl -n "$NS" delete modelprefetches.worker.gpustack.ai "${P}-pin" "${P}-big" --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1
kubectl -n "$NS" delete modelstorebindings.worker.gpustack.ai -l "$LABEL" --ignore-not-found >/dev/null 2>&1
kubectl delete modelstores.worker.gpustack.ai -l "$LABEL" --ignore-not-found >/dev/null 2>&1
for _ in $(seq 1 20); do
  [ -z "$(kubectl get nodemodelstores.worker.gpustack.ai "$W1" -o jsonpath='{.spec.store}' 2>/dev/null)" ] && break
  sleep 3
done

# =============================================================================================
# Phase 1c: Slow download progress thresholds, RBAC, v1 views & quiet-at-rest watch
# =============================================================================================
echo "== Phase 1c: download progress thresholds, RBAC & v1 views =="
setting_set model-store-download-bandwidth 128Ki >/dev/null
settings_settle
artifact "$NS" "${P}-art" e2e/progress-model "" 22
artifact "$NSB" "${P}-other" e2e/progress-model "" 22
DIGEST="$(wait_resolved "$NS" "${P}-art" 120)"
[ -n "$(wait_resolved "$NSB" "${P}-other" 120)" ] || record FAIL "the second namespace's artifact resolves" "${NSB}/${P}-other"
DOWNLOAD_OK=1
if [ -z "$DIGEST" ]; then
  record FAIL "the artifact resolves against the test hub" "${P}-art"
  DOWNLOAD_OK=0
fi
SIZE="$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-art" -o jsonpath='{.status.resolved.sizeBytes}')"

if [ "$DOWNLOAD_OK" = 1 ]; then
  WATCH_OUT="${SCRATCH}/nms-watch.jsonl"
  WATCH_PID="$(nms_watch "$W1" "$WATCH_OUT")"
  sleep 3
  consumer "$NS" "${P}-pod" "$W1" "${P}-art" "$(get_uid "$NS" "${P}-art")" "$DIGEST"

  percents="" reads=()
  for _ in $(seq 1 90); do
    sleep 5
    [ "$(kubectl -n "$NS" get pod "${P}-pod" -o jsonpath='{.status.phase}' 2>/dev/null)" = Running ] && break
    read -r _ dl _ pct <<<"$(nodes_of "$NS" "${P}-art")"
    [ "${dl:-0}" = 1 ] && [ -n "${pct:-}" ] && percents="${percents} ${pct}"
    if [ "${#reads[@]}" -lt 2 ] && [ "${dl:-0}" = 1 ]; then
      ans="$(progress_as "$NS" "${P}-art" "$READER")" && reads+=("$ans")
      sleep 5
    fi
  done
  pod_ready "$NS" "${P}-pod" 120 || record FAIL "the consumer runs after its download" "$(pod_mount_events "$NS" "${P}-pod" | tail -1)"
  RAN_AT=$(date +%s)

  bad="$(for p in $percents; do [ $((p % 5)) -eq 0 ] || echo "$p"; done)"
  if [ -n "$percents" ] && [ -z "$bad" ]; then
    record PASS "the artifact's downloading percent moved in steps of 5 while one node downloaded" "${P}-art:${percents}"
  else
    record FAIL "the artifact's percent is a multiple of 5 while downloading" "${P}-art: seen '${percents}', off-step '${bad}'"
  fi
  final=""
  for _ in $(seq 1 20); do
    final="$(nodes_of "$NS" "${P}-art")"
    read -r rd dl _ pct <<<"$final"
    [ "${rd:-0}" -ge 1 ] && [ "${dl:-1}" = 0 ] && [ -z "${pct:-}" ] && break
    sleep 3
  done
  read -r rd dl _ pct <<<"$final"
  if [ "${rd:-0}" -ge 1 ] && [ "${dl:-1}" = 0 ] && [ -z "${pct:-}" ]; then
    record PASS "after publication the artifact counts the node ready and no percent" "${P}-art: ${final}"
  else
    record FAIL "after publication the node is counted ready (got '${final}')" "${P}-art"
  fi

  if [ "${#reads[@]}" -eq 2 ]; then
    l1="$(field "${reads[0]}" live)" l2="$(field "${reads[1]}" live)"
    b1="$(field "${reads[0]}" downloadingBytes)" b2="$(field "${reads[1]}" downloadingBytes)"
    if [ "$l1" = 1 ] && [ "$l2" = 1 ] && [ "${b2:-0}" -ge "${b1:-1}" ] && [ "${b1:-0}" -gt 0 ]; then
      record PASS "two progress reads during the download are live and never go back" "live ${l1},${l2}; bytes ${b1} -> ${b2}"
    else
      record FAIL "progress reads are live and non-decreasing" "live ${l1:-?},${l2:-?}; bytes ${b1:-?} -> ${b2:-?}"
    fi
  else
    record FAIL "two progress reads were taken during the download" "${#reads[@]} taken: ${reads[*]:-none}"
  fi
else
  record SKIP "the consumer runs after its download" "the artifact did not resolve"
  record SKIP "the artifact's downloading percent moved in steps of 5 while one node downloaded" "the artifact did not resolve"
  record SKIP "after publication the artifact counts the node ready and no percent" "the artifact did not resolve"
  record SKIP "two progress reads during the download are live and never go back" "the artifact did not resolve"
fi

# Progress authorization.
ans="$(progress_as "$NS" "${P}-art" "$READER")"; rc=$?
leak=""
for n in $(kubectl get nodes -o jsonpath='{.items[*].metadata.name}'); do
  case "$ans" in *"$n"*) leak="${leak} $n" ;; esac
done
if [ "$rc" -eq 0 ] && [ "$(field "$ans" ready)" -ge 1 ] 2>/dev/null; then
  record PASS "a subject with get modelartifacts/progress reads its namespace's progress" "${READER}: ready $(field "$ans" ready)"
else
  record FAIL "the reader reads progress (rc ${rc})" "$(echo "$ans" | head -c 300)"
fi
if [ "$rc" -eq 0 ] && [ -z "$leak" ]; then
  record PASS "the tenant's progress names no node of the cluster" "${P}-art"
else
  record FAIL "the tenant's progress names no node" "found:${leak:- <no answer>}"
fi
ans="$(progress_as "$NSB" "${P}-other" "$READER")"; rc=$?
if [ "$rc" -ne 0 ] && echo "$ans" | grep -qi forbidden; then
  record PASS "the same subject is refused another namespace's progress" "${NSB}/${P}-other"
else
  record FAIL "another namespace's progress is refused (rc ${rc})" "$(echo "$ans" | head -c 300)"
fi
ans="$(progress_as "$NS" "${P}-art" "$PLAIN")"; rc=$?
if [ "$rc" -ne 0 ] && echo "$ans" | grep -qi forbidden; then
  record PASS "get modelartifacts alone does not grant progress" "$PLAIN"
else
  record FAIL "a subject without the subresource rule is refused (rc ${rc})" "$(echo "$ans" | head -c 300)"
fi

# No tenant in NodeModelStore.
dump="$(kubectl get nodemodelstores.worker.gpustack.ai -o json)"
found=""
for s in "$NS" "$NSB" "${P}-art" "${P}-other" "e2e/progress-model" "${P}-pod"; do
  case "$dump" in *"\"$s"*|*"$s\""*|*"/$s"*) found="${found} $s" ;; esac
done
if [ -n "$dump" ] && [ -z "$found" ] && echo "$dump" | grep -q "$DIGEST"; then
  record PASS "no namespace, artifact, repository or Pod of this case is in any NodeModelStore" "the digest is"
else
  record FAIL "the NodeModelStores name no tenant" "found:${found:- <no dump or no digest>}"
fi

# v1 read-only views & columns.
before="$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-art" -o jsonpath='{.status.resolved.manifestDigest}')"
kubectl -n "$NS" patch modelartifacts.v1.worker.gpustack.ai "${P}-art" --type=merge \
  -p '{"status":{"resolved":{"manifestDigest":"sha256:0000000000000000000000000000000000000000000000000000000000000000"}}}' >/dev/null 2>&1
after="$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-art" -o jsonpath='{.status.resolved.manifestDigest}')"
if [ -n "$before" ] && [ "$before" = "$after" ]; then
  record PASS "a status change sent through the v1 ModelArtifact leaves the stored status" "${P}-art"
else
  record FAIL "the v1 view does not write status (before ${before}, after ${after})" "${P}-art"
fi
out="$(kubectl get --raw "/apis/worker.gpustack.ai/v1/namespaces/${NS}/modelartifacts/${P}-art/status" 2>&1)"
if echo "$out" | grep -Eqi 'not ?found'; then
  record PASS "the v1 ModelArtifact has no status subresource" "${P}-art"
else
  record FAIL "the v1 ModelArtifact serves no status" "$(echo "$out" | head -c 200)"
fi
out="$(kubectl patch nodemodelstores.v1.worker.gpustack.ai "$W1" --type=merge -p '{"metadata":{"labels":{"e2e.gpustack.ai/c22":"x"}}}' 2>&1)"; rc=$?
if [ "$rc" -ne 0 ] && [ -z "$(kubectl get nodemodelstores.worker.gpustack.ai "$W1" -o jsonpath='{.metadata.labels.e2e\.gpustack\.ai/c22}')" ]; then
  record PASS "the v1 NodeModelStore refuses an update" "$(echo "$out" | head -c 120)"
else
  record FAIL "the v1 NodeModelStore is read-only (rc ${rc})" "$(echo "$out" | head -c 200)"
fi
cols="$(kubectl -n "$NS" get modelartifacts.v1.worker.gpustack.ai 2>&1 | head -1)"
if has_columns "$cols" NAME SOURCE REVISION SIZE READY DOWNLOADING RESOLVED; then
  record PASS "the v1 ModelArtifact prints its columns" "$(echo "$cols" | tr -s ' ')"
else
  record FAIL "the v1 ModelArtifact prints Name, Source, Revision, Size, Ready, Downloading, Resolved" "$cols"
fi
cols="$(kubectl get nodemodelstores.v1.worker.gpustack.ai 2>&1 | head -1)"
if has_columns "$cols" NAME READY USED MODELS DOWNLOADING; then
  record PASS "the v1 NodeModelStore prints its columns" "$(echo "$cols" | tr -s ' ')"
else
  record FAIL "the v1 NodeModelStore prints Name, Ready, Used, Models, Downloading" "$cols"
fi

# Watch write stats & quiet node.
if [ "$DOWNLOAD_OK" = 1 ]; then
  QUIET=330
  left=$(( RAN_AT + QUIET - $(date +%s) ))
  [ "$left" -gt 0 ] && sleep "$left"
  stats="$(nms_write_stats "$WATCH_OUT" "$DIGEST")"
  kill "$WATCH_PID" >/dev/null 2>&1
  WATCH_PID=""
  progress="${stats#progress=}" progress="${progress%% *}"
  mingap="${stats#*mingap=}" mingap="${mingap%% *}"
  inter="${stats#*intermediate=}" inter="${inter%% *}"
  last="${stats##*last=}"
  if [ "${inter:-0}" -ge 2 ]; then
    record PASS "the node's writes carried at least two intermediate progress values" "${W1}: ${stats}"
  else
    record FAIL "at least two intermediate progress values were written" "${W1}: ${stats} (size ${SIZE})"
  fi
  if [ "${progress:-99}" -le 20 ] && { [ "${mingap:--1}" -lt 0 ] || [ "${mingap}" -ge 29 ]; }; then
    record PASS "progress-only writes: at most 20, none closer than 30 s" "${W1}: ${stats}"
  else
    record FAIL "progress writes keep their thresholds" "${W1}: ${stats}"
  fi
  if [ "${last:-0}" -gt 0 ] && [ "$last" -le $((RAN_AT + 30)) ]; then
    record PASS "the node wrote nothing for a report interval after the Pod ran" "${W1}: last write $((last - RAN_AT))s after, quiet ${QUIET}s"
  else
    record FAIL "a quiet node writes nothing" "${W1}: last write $((last - RAN_AT))s after the Pod ran"
  fi
else
  record SKIP "the node's writes carried at least two intermediate progress values" "the artifact did not resolve"
  record SKIP "progress-only writes: at most 20, none closer than 30 s" "the artifact did not resolve"
  record SKIP "the node wrote nothing for a report interval after the Pod ran" "the artifact did not resolve"
fi

# =============================================================================================
# Phase 2: Dedicated node-to-node peer sync (zero hub bytes) & NetworkPolicy isolation
# =============================================================================================
echo "== Phase 2: node-to-node peer sync and tenant NetworkPolicy isolation =="
if [ "${#WORKERS[@]}" -lt 2 ]; then
  record SKIP "peer sync and tenant NetworkPolicy rows" "needs two workers, found ${#WORKERS[@]}"
elif [ "${E2E_OFFLINE:-0}" = 1 ]; then
  record SKIP "peer sync and tenant NetworkPolicy rows" "E2E_OFFLINE=1"
else
  setting_unset model-store-download-bandwidth
  setting_set model-store-peer-sync true
  settings_settle

  artifact "$NS" "${P}-weights" peer-e2e/repo "" 22
  PEER_DIGEST="$(wait_resolved "$NS" "${P}-weights" "$COMMIT_BOUND")"
  if [ -z "$PEER_DIGEST" ]; then
    record FAIL "peer-sync artifact resolved" "${P}-weights"
  else
    kubectl -n "$NS" delete pod "${P}-mount-${W1}" --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1
    SEED_PLUGIN="$(plugin_pod "$W1")"
    HEX="${PEER_DIGEST#sha256:}"
    if ! kubectl -n "$SYSTEM_NS" exec "$SEED_PLUGIN" -c main -- rm -rf "/var/lib/gpustack/models/published/$HEX"; then
      record FAIL "seed published tree cleared" "could not clear /var/lib/gpustack/models/published/${HEX} on ${W1}"
    else
      kubectl -n "$SYSTEM_NS" delete pod "$SEED_PLUGIN" >/dev/null 2>&1
      for _ in $(seq 1 30); do
        SEED_PLUGIN="$(plugin_pod "$W1")"
        kubectl -n "$SYSTEM_NS" get pod "$SEED_PLUGIN" -o jsonpath='{.status.containerStatuses[0].ready}' 2>/dev/null | grep -q true && break
        sleep 5
      done
      SEED_UID="$(get_uid "$NS" "${P}-weights")"
      consumer "$NS" "${P}-mount-${W1}" "$W1" "${P}-weights" "$SEED_UID" "$PEER_DIGEST"
      if wait_node_ready "$PEER_DIGEST" "$W1"; then
        record PASS "seed" "the seed node pulled from the hub and is Ready"
      else
        record FAIL "seed" "the seed node never reached Ready"
      fi
      SEED_HUB="$(plugin_metric "$W1" "$HUB_METRIC")"
      TOTAL_BYTES="$(kubectl get nodemodelstores.v1alpha1.worker.gpustack.ai "$W1" -o jsonpath="{.status.models[?(@.digest=='$PEER_DIGEST')].sizeBytes}" 2>/dev/null)"
      if [ -n "$SEED_HUB" ] && awk -v a="$SEED_HUB" -v b="${TOTAL_BYTES:-0}" 'BEGIN{exit !(a+0 >= b*0.999 && a+0 <= b*1.001)}'; then
        record PASS "seed-hub-bytes" "the seed pulled ${SEED_HUB} bytes from the hub, matching manifest total (${TOTAL_BYTES})"
      else
        record FAIL "seed-hub-bytes" "seed hub counter (${SEED_HUB}) != manifest total (${TOTAL_BYTES:-0})"
      fi

      COUNTERS="$(mh_peer_sync_check "$NS" "${P}-mount-${W2}" "$W2" "${P}-weights" "$SEED_UID" "$PEER_DIGEST" "$PEER_POD_BOUND")"
      read -r PEER_BASE HUB_BASE COLD_PEER COLD_HUB <<< "$COUNTERS"
      PEER_BASE="${PEER_BASE:-0}"
      HUB_BASE="${HUB_BASE:-0}"
      COLD_PEER="${COLD_PEER:-0}"
      COLD_HUB="${COLD_HUB:-0}"
      COLD_POD="${P}-mount-${W2}"
      if wait_node_ready "$PEER_DIGEST" "$W2"; then
        record PASS "pull" "the cold node reached Ready"
      else
        record FAIL "pull" "the cold node never reached Ready"
      fi
      COLD_STATE="$(kubectl get nodemodelstores.v1alpha1.worker.gpustack.ai "$W2" -o jsonpath="{.status.models[?(@.digest=='$PEER_DIGEST')].source}" 2>/dev/null)"
      if [ "$COLD_STATE" = "Peer" ]; then
        record PASS "source" "the cold node's entry says Peer"
      else
        record FAIL "source" "the cold node's entry says ${COLD_STATE:-<none>}, want Peer"
      fi
      PEER_DELTA="$(awk -v a="$COLD_PEER" -v b="$PEER_BASE" 'BEGIN{print a-b}')"
      HUB_DELTA="$(awk -v a="$COLD_HUB" -v b="$HUB_BASE" 'BEGIN{print a-b}')"
      if [ -n "$COLD_PEER" ] && awk -v a="$PEER_DELTA" -v b="${TOTAL_BYTES:-0}" 'BEGIN{exit !(a+0 >= b*0.999 && a+0 <= b*1.001)}'; then
        record PASS "peer-bytes" "cold pulled ${PEER_DELTA} bytes from peers, matching manifest total (${TOTAL_BYTES})"
      else
        record FAIL "peer-bytes" "peer delta (${PEER_DELTA}) != manifest total (${TOTAL_BYTES:-0})"
      fi
      if awk -v a="$HUB_DELTA" 'BEGIN{exit !(a+0 == 0)}'; then
        record PASS "hub-bytes" "the cold node's hub counter delta stayed at zero (${HUB_DELTA})"
      else
        record FAIL "hub-bytes" "the cold node pulled ${HUB_DELTA} bytes from the hub"
      fi

      HOTIP="$(kubectl -n "$NS" get pod "$COLD_POD" -o jsonpath='{.status.podIP}')"
      kubectl -n "$NS" run "${P}-tenant" --image=busybox:1.36 --restart=Never \
        --overrides='{"spec":{"securityContext":{"runAsNonRoot":true,"runAsUser":65534,"seccompProfile":{"type":"RuntimeDefault"}},"containers":[{"name":"tenant","image":"busybox:1.36","command":["sh","-c","sleep 60"],"securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]}}}]}}' \
        >/dev/null 2>&1
      for _ in $(seq 1 20); do
        [ "$(kubectl -n "$NS" get pod "${P}-tenant" -o jsonpath='{.status.phase}' 2>/dev/null)" = Running ] && break
        sleep 2
      done
      if kubectl -n "$NS" exec "${P}-tenant" -- sh -c "wget -q -T 5 -O- https://${HOTIP}:${PEER_PORT}/peer/v1/trees/$(printf 'a%.0s' $(seq 1 64))" >/dev/null 2>&1; then
        record FAIL "tenant" "a tenant pod reached the peer port"
      else
        record PASS "tenant" "the tenant pod's connection to peer port ${PEER_PORT} was dropped"
      fi
      kubectl -n "$NS" delete pod "${P}-tenant" --force --grace-period=0 --ignore-not-found >/dev/null 2>&1
    fi
  fi
fi

results
