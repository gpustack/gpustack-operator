#!/usr/bin/env bash
#
# CASE 114 — An expectedDigest asserts the content: a match resolves, a mismatch refuses naming both
#            digests, a twice-unreachable hub hands the identity to the anchor, and an anchored
#            artifact is delivered by a node alone (MUTATING, self-recovering; the peer leg
#            AUTO-SKIPS on one worker or with E2E_C114_OFFLINE=1)
#
#   case-114.sh <NS>
#
# Goal:        Pin the anchor end to end against a controlled hub: the digest a resolution
#              produces is exactly the assertion, a hub serving other content is refused with both
#              digests in the message, a hub that twice cannot be reached writes the anchor as the
#              identity (digestSource Expected, no revision) and is never asked again, and an
#              anchored artifact is held under Engine delivery and served from the peer's published
#              tree under Node delivery.
# Environment: A cluster installed from this chart with modelManager.enabled (the default), the
#              stock python image pullable, and two schedulable workers for the peer leg. NO GPU
#              and no internet: the hub is this case's own in-cluster test hub, so the refusal and
#              the confirmed outage are exact.
# Inputs:      MOCKED: the hub (_model-hub.py) serving one small repository, and the worker's hub
#              endpoint Setting pointed at it for the run and restored afterwards. Everything the
#              anchor reads — resolution, the staircase, the ledger — is the real code path.
# Expected:    - an unanchored twin resolves, and its digest is published on the first worker;
#              - an artifact anchored to that digest resolves with digestSource Hub and a commit;
#              - an artifact anchored to another digest stays Resolved=False, reason
#                DigestMismatch, message carrying both digests;
#              - with the hub unreachable, an artifact anchored to the first digest waits out one
#                blip, then resolves to the anchor (digestSource Expected, revision empty), while
#                the hub's request log stays flat from the flip onward;
#              - under Engine delivery both anchored artifacts are held with
#                AnchorNeedsNodeDelivery — the Expected one's message naming the missing commit —
#                and no Pod is created;
#              - a consumer on the second worker mounts the Expected identity, the hub's request
#                log still flat, its peer bytes risen (or the node pre-warmed).
# Cleanup:     Trap restores both Settings, deletes the Pods, artifacts, deployments and the hub.
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"
CASES_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "${CASES_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASES_DIR}/_model-hub-lib.sh"

NS="${1:?usage: case-114.sh <consumer-NS>}"
if [ "$NS" = "$SYSTEM_NS" ]; then
  echo "[case-114] refusing system namespace ${SYSTEM_NS}; usage: case-114.sh <consumer-NS>" >&2
  exit 2
fi
P=c114
COMMIT_BOUND=120
ANCHOR_BOUND=240
POD_BOUND=300
WRONG="sha256:$(printf 'f%.0s' $(seq 64))"
FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

WORKERS=()
for _w in $(model_workers); do WORKERS+=("$_w"); done
if [ "${#WORKERS[@]}" -lt 1 ]; then
  echo "[case-114] SKIP: needs a schedulable worker, found none" >&2
  exit 0
fi

# The two Settings this case flips, saved so the trap restores whatever the cluster carried.
EP_SAVED="$(setting_get model-artifact-huggingface-endpoint)"
DELIVERY_SAVED="$(setting_get model-artifact-delivery-mode)"

# shellcheck disable=SC2317  # the trap keeps it reachable; the checker cannot see the EXIT
cleanup() {
  echo
  echo "[case-114] cleanup"
  if [ -n "$EP_SAVED" ]; then setting_set model-artifact-huggingface-endpoint "$EP_SAVED" >/dev/null; else setting_unset model-artifact-huggingface-endpoint >/dev/null; fi
  if [ -n "$DELIVERY_SAVED" ]; then setting_set model-artifact-delivery-mode "$DELIVERY_SAVED" >/dev/null; else setting_unset model-artifact-delivery-mode >/dev/null; fi
  kubectl -n "$NS" delete pod "${P}-seed" "${P}-peer" --ignore-not-found --wait=true --timeout=180s >/dev/null 2>&1
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "${P}-md-match" "${P}-md-expected" --ignore-not-found >/dev/null 2>&1
  kubectl -n "$NS" delete modelartifacts.worker.gpustack.ai "${P}-twin" "${P}-match" "${P}-mismatch" "${P}-expected" --ignore-not-found >/dev/null 2>&1
  kubectl -n "$NS" delete deploy "${P}-hub" --ignore-not-found >/dev/null 2>&1
  kubectl -n "$NS" delete svc "${P}-hub" --ignore-not-found >/dev/null 2>&1
  kubectl -n "$NS" delete configmap "${P}-hub-script" --ignore-not-found >/dev/null 2>&1
}
trap cleanup EXIT

REPOS="$(python3 -c "
import json
print(json.dumps({
  'e2e/anchored': {'files': {'config.json': {'size': 300}, 'tokenizer.json': {'size': 3000}}},
}))")"
HUB_URL="$(mh_deploy "$NS" "${P}-hub" "$REPOS")"
setting_set model-artifact-huggingface-endpoint "$HUB_URL" >/dev/null
setting_set model-artifact-delivery-mode Node >/dev/null
settings_settle
echo "[case-114] hub at ${HUB_URL}; workers ${WORKERS[*]}"

echo "== 1. the twin resolves, its digest publishes on the first worker =="
artifact "$NS" "${P}-twin" e2e/anchored "" "$P"
DIGEST="$(wait_resolved "$NS" "${P}-twin" "$COMMIT_BOUND")"
if [ -n "$DIGEST" ]; then
  record PASS "twin resolved" "digest ${DIGEST:0:19}…"
else
  record FAIL "twin resolved" "no digest within ${COMMIT_BOUND}s: $(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-twin" -o jsonpath='{.status.conditions[?(@.type=="Resolved")].message}' 2>/dev/null)"
fi
[ -n "$DIGEST" ] || { print_rows "${ROWS[@]}"; exit 1; }

TWIN_UID=$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-twin" -o jsonpath='{.metadata.uid}')
consumer "$NS" "${P}-seed" "${WORKERS[0]}" "${P}-twin" "$TWIN_UID" "$DIGEST"
if pod_ready "$NS" "${P}-seed" "$POD_BOUND"; then
  record PASS "seed node" "the first worker holds the published tree"
else
  record FAIL "seed node" "not Running within ${POD_BOUND}s: $(pod_mount_events "$NS" "${P}-seed" | head -1)"
fi

echo "== 2. a matching anchor resolves as a hub identity =="
artifact "$NS" "${P}-match" e2e/anchored "" "$P" "
      expectedDigest: $DIGEST"
MATCH_DIGEST="$(wait_resolved "$NS" "${P}-match" "$COMMIT_BOUND")"
MATCH_SRC="$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-match" -o jsonpath='{.status.resolved.digestSource}' 2>/dev/null)"
MATCH_REV="$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-match" -o jsonpath='{.status.resolved.revision}' 2>/dev/null)"
if [ "$MATCH_DIGEST" = "$DIGEST" ] && [ "$MATCH_SRC" = Hub ] && [ -n "$MATCH_REV" ]; then
  record PASS "match resolves" "digestSource Hub at commit ${MATCH_REV:0:12}…"
else
  record FAIL "match resolves" "digest '${MATCH_DIGEST:0:19}…' source '$MATCH_SRC' revision '$MATCH_REV'"
fi

echo "== 3. a foreign anchor is refused with both digests =="
artifact "$NS" "${P}-mismatch" e2e/anchored "" "$P" "
      expectedDigest: $WRONG"
MISMATCH_MSG=""
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

echo "== 4. a twice-unreachable hub hands the identity to the anchor =="
LOG_BEFORE="$(mh_log "$NS" "${P}-hub" | wc -l | tr -d ' ')"
setting_set model-artifact-huggingface-endpoint "http://${P}-absent.${NS}.svc:8080" >/dev/null
settings_settle
artifact "$NS" "${P}-expected" e2e/anchored "" "$P" "
      expectedDigest: $DIGEST"
EXPECTED_DIGEST="$(wait_resolved "$NS" "${P}-expected" "$ANCHOR_BOUND")"
EXPECTED_SRC="$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-expected" -o jsonpath='{.status.resolved.digestSource}' 2>/dev/null)"
EXPECTED_REV="$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-expected" -o jsonpath='{.status.resolved.revision}' 2>/dev/null)"
LOG_DURING="$(mh_log "$NS" "${P}-hub" | wc -l | tr -d ' ')"
if [ "$EXPECTED_DIGEST" = "$DIGEST" ] && [ "$EXPECTED_SRC" = Expected ] && [ -z "$EXPECTED_REV" ] && [ "$LOG_DURING" = "$LOG_BEFORE" ]; then
  record PASS "anchored identity" "digestSource Expected, no revision, hub log flat (${LOG_BEFORE} → ${LOG_DURING})"
else
  record FAIL "anchored identity" "digest '${EXPECTED_DIGEST:0:19}…' source '$EXPECTED_SRC' revision '$EXPECTED_REV' log ${LOG_BEFORE} → ${LOG_DURING}"
fi
if [ -n "$EP_SAVED" ]; then setting_set model-artifact-huggingface-endpoint "$EP_SAVED" >/dev/null; else setting_unset model-artifact-huggingface-endpoint >/dev/null; fi
settings_settle

echo "== 5. Engine delivery is refused for anchored artifacts =="
IT="$(kubectl get instancetypes.worker.gpustack.ai -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
if [ -z "$IT" ]; then
  record SKIP "engine block" "no InstanceType exists to reference; the render is unit-covered"
else
  setting_set model-artifact-delivery-mode Engine >/dev/null
  settings_settle
  for pair in "${P}-match:${P}-md-match:cannot anchor-verify" "${P}-expected:${P}-md-expected:no resolved commit"; do
    ART="${pair%%:*}"; REST="${pair#*:}"; MD="${REST%%:*}"; WANT="${REST#*:}"
    kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata: {name: ${MD}, namespace: ${NS}}
spec:
  engine: {name: vLLM, version: "0.11.0"}
  model: {name: qwen, artifactRef: {name: ${ART}}}
  roles:
    - name: server
      instanceType: ${IT}
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
  if [ -n "$DELIVERY_SAVED" ]; then setting_set model-artifact-delivery-mode "$DELIVERY_SAVED" >/dev/null; else setting_unset model-artifact-delivery-mode >/dev/null; fi
  settings_settle
fi

echo "== 6. the Expected identity is served from the peer =="
if [ "${#WORKERS[@]}" -lt 2 ]; then
  record SKIP "peer mount" "needs two schedulable workers, found ${#WORKERS[@]}"
elif [ "${E2E_C114_OFFLINE:-0}" = 1 ]; then
  record SKIP "peer mount" "E2E_C114_OFFLINE=1"
else
  EXPECTED_UID=$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-expected" -o jsonpath='{.metadata.uid}')
  kubectl -n "$SYSTEM_NS" delete pod -l app.kubernetes.io/component=model-manager \
    --field-selector "spec.nodeName=${WORKERS[1]}" --wait=true --timeout=120s >/dev/null 2>&1
  for _ in $(seq 1 30); do
    PP="$(plugin_pod "${WORKERS[1]}")"
    [ -n "$PP" ] && [ "$(kubectl -n "$SYSTEM_NS" get pod "$PP" -o jsonpath='{.status.phase}' 2>/dev/null)" = Running ] && break
    sleep 3
  done
  LOG_BEFORE="$(mh_log "$NS" "${P}-hub" | wc -l | tr -d ' ')"
  PEER_BEFORE="$(plugin_metric "${WORKERS[1]}" "gpustack_model_manager_download_bytes_total{source=\"peer\"}")"
  HUB_BEFORE="$(plugin_metric "${WORKERS[1]}" "gpustack_model_manager_download_bytes_total{source=\"hub\"}")"
  consumer "$NS" "${P}-peer" "${WORKERS[1]}" "${P}-expected" "$EXPECTED_UID" "$DIGEST"
  if pod_ready "$NS" "${P}-peer" "$POD_BOUND"; then
    LOG_AFTER="$(mh_log "$NS" "${P}-hub" | wc -l | tr -d ' ')"
    PEER_AFTER="$(plugin_metric "${WORKERS[1]}" "gpustack_model_manager_download_bytes_total{source=\"peer\"}")"
    HUB_AFTER="$(plugin_metric "${WORKERS[1]}" "gpustack_model_manager_download_bytes_total{source=\"hub\"}")"
    if [ "$LOG_AFTER" != "$LOG_BEFORE" ]; then
      record FAIL "peer mount" "the hub was asked (${LOG_BEFORE} → ${LOG_AFTER}) though the identity is Expected"
    elif [ "$PEER_AFTER" -gt "$PEER_BEFORE" ] && [ "$HUB_AFTER" -le "$HUB_BEFORE" ]; then
      record PASS "peer mount" "the second node took ${PEER_AFTER} peer bytes and ${HUB_AFTER} hub bytes"
    elif [ "$PEER_AFTER" -le "$PEER_BEFORE" ] && [ "$HUB_AFTER" -le "$HUB_BEFORE" ]; then
      record PASS "peer mount" "the mount is a hit on pre-warmed content (bytes flat); the hub log stayed flat"
    else
      record FAIL "peer mount" "the second node pulled from the hub (${HUB_BEFORE} → ${HUB_AFTER})"
    fi
  else
    record FAIL "peer mount" "not Running within ${POD_BOUND}s: $(pod_mount_events "$NS" "${P}-peer" | head -1)"
  fi
fi

print_rows "${ROWS[@]}"
exit "$FAILS"
