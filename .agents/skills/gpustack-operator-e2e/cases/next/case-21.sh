#!/usr/bin/env bash
#
# CASE 21 — Node model-cache CSI delivery: cold mount dedup, restricted PSA namespace, forged
#           attributes, corrupt rejection, crash resume, status guards, HTTPS proxy, plugin
#           rollout, GC eviction, and restart/Node reconciliation
#           (MUTATING, self-recovering; real-Hub row AUTO-SKIPS with E2E_C21_OFFLINE=1; GC and
#           Node-deletion rows AUTO-SKIP without docker access to the kind worker)
#
#   case-21.sh <NS>
#
# Prerequisites: nodes=2; gpu=none; features=kind,multi-host; cluster=kind; inputs=E2E_C21_OFFLINE,E2E_MD_IMAGE,E2E_MD_INSTANCE_TYPE,E2E_MODEL_MANAGER_SA,MH_PYTHON
# Goal:        Prove node model-cache CSI delivery and its lifecycle end to end across two phases:
#                Phase 1 — Cold/warm mount, security boundaries, status guards, proxy & real Hub:
#                  a cold mount outlasting 120s downloads each file once for two Pods with exact
#                  SHA-256; a third Pod downloads nothing; consumers run in a restricted PSA
#                  namespace; forged attributes are refused; a corrupting source reports
#                  Failed/IntegrityMismatch; killing the plugin mid-download never exposes a partial
#                  tree and resumes cleanly; private repo mounts with token redacted from all
#                  outputs; NodeModelStore and ModelArtifact status guards admit only their writers;
#                  download-concurrency Setting reaches every node; removing CSIDriver removes every
#                  NodeModelStore and refuses Node delivery; HTTPS proxy carries every file download
#                  (with direct control); real Hub serves through redirects.
#                Phase 2 — Plugin rollout, filtered delivery, consumers, restart reconciliation & GC:
#                  DaemonSet rolling restart leaves mounted Pods reading with zero errors while a
#                  concurrent mount succeeds; filtered artifact downloads only allowed files and
#                  reports FilterNeedsNodeDelivery under Engine (waking unedited on Node); Instance
#                  and ModelDeployment mount hub artifacts (Materializing -> Mounted); CSIDriver
#                  absence reports NodeDeliveryUnavailable and wakes unedited on return; plugin
#                  restart and NodeModelStore deletion rewrite identical entries from disk; GC
#                  watermarks 50/45 and 10/5 evict oldest unreferenced digests while keeping
#                  referenced ones; deleting a Node collects its NodeModelStore and re-registration
#                  creates a new UID.
# Environment: A kind cluster installed from this chart with modelManager.enabled (the default), two
#              or more schedulable workers, a materialized scheduling chain (run case-1 first) with
#              an InstanceType, stock python image pullable, openssl on this machine, and docker
#              reaching the kind worker containers for GC/Node rows. NO GPU.
# Inputs:      MOCKED: test hubs (_model-hub.py) over HTTP and HTTPS with self-signed CA bundle,
#              forward proxy, random private token, and a 100 MiB tmpfs over W2's cache root.
#              Real: the CSI plugin, kubelet, webhooks, RBAC, collector, worker, and real Hub
#              hf-internal-testing/tiny-random-bert (skipped when E2E_C21_OFFLINE=1).
# Expected:    - Phase 1: cold dedup (>120s, 1 fetch/file, SHA-256 match, Ready); warm hit (0 new
#                requests/bytes); restricted PSA enforced; forged attributes refused; corrupt source
#                Failed/IntegrityMismatch; killed plugin never mounts early and finishes intact;
#                private token redacted; status guards exact; concurrency Setting converged;
#                CSIDriver gates Node delivery; HTTPS proxy & direct control distinct; real Hub
#                model.safetensors hash matches.
#              - Phase 2: reader Pod has 0 errors across DaemonSet roll and concurrent mount passes;
#                filtered artifact holds only allowed files and wakes from FilterNeedsNodeDelivery;
#                Instance and ModelDeployment mount hub artifacts; NodeDeliveryUnavailable wakes on
#                CSIDriver return; plugin restart & NodeModelStore delete preserve disk entries;
#                GC 50/45 and 10/5 keep referenced digest; Node delete collects NodeModelStore and
#                re-register creates a new UID.
# Cleanup:     Trap restarts kubelet if stopped, deletes all Pods, deployments, Instances,
#              artifacts, Secrets, hubs, proxy, CA ConfigMap, and created namespaces, restores the
#              CSIDriver, restores all saved Settings, unmounts tmpfs and restarts its plugin, and
#              restores PSA labels on <NS>. Idempotent, on pass AND fail.
set -uo pipefail

CASES_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "${CASES_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASES_DIR}/_model-hub-lib.sh"

NS="${1:?usage: case-21.sh <NS>}"
export CASE_ID=21
if [ "$NS" = "$SYSTEM_NS" ]; then
  echo "[case-21] refusing system namespace ${SYSTEM_NS}; usage: case-21.sh <consumer-NS>" >&2
  exit 2
fi
NSB="${NS}-b"
MDNS="${NS}-c21"
P=c21
TLS_HUB="${P}-tlshub"
IT="${E2E_MD_ACCELERATOR_TYPE:-${E2E_MD_INSTANCE_TYPE:-}}"
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
DS=gpustack-operator-model-manager
CACHE_ROOT=/var/lib/gpustack/models
PSA_SNAPSHOT=0
ORIG_ENFORCE=""
ORIG_ENFORCE_VERSION=""
TMPFS_NODE=""
DELETED_NODE=""
SCRATCH="$(mktemp -d)"
TOKEN="hf_$(python3 -c 'import secrets; print(secrets.token_hex(16))')"
SA="${E2E_MODEL_MANAGER_SA:-gpustack-operator-model-manager}"

settings_save model-artifact-huggingface-endpoint model-artifact-https-proxy \
  model-artifact-no-proxy model-artifact-ca-bundle model-store-download-concurrency \
  model-artifact-delivery-mode model-store-high-watermark model-store-low-watermark

cleanup() {
  local labels ns n
  echo
  echo "[case-21] cleanup"
  if [ -n "$DELETED_NODE" ] && ! kubectl get node "$DELETED_NODE" >/dev/null 2>&1; then
    docker exec "$DELETED_NODE" systemctl start kubelet >/dev/null 2>&1 || true
  fi
  kubectl -n "$MDNS" delete modeldeployments.worker.gpustack.ai,instances.worker.gpustack.ai --all --ignore-not-found --wait=false >/dev/null 2>&1
  for ns in "$NS" "$NSB" "$MDNS"; do
    kubectl -n "$ns" delete pods -l e2e.gpustack.ai/consumer=true --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1
    kubectl -n "$ns" delete modelartifacts.worker.gpustack.ai -l e2e.gpustack.ai/case=21 --ignore-not-found >/dev/null 2>&1
  done
  for _ in $(seq 1 24); do [ -z "$(kubectl -n "$MDNS" get pods -o name 2>/dev/null)" ] && break; sleep 5; done
  kubectl -n "$NS" delete secret "${P}-token" --ignore-not-found >/dev/null 2>&1
  for n in "${P}-hub" "${P}-hub2" "$TLS_HUB" "${P}-proxy"; do
    kubectl -n "$NS" delete deploy,svc "$n" --ignore-not-found >/dev/null 2>&1
    kubectl -n "$NS" delete configmap "${n}-script" --ignore-not-found >/dev/null 2>&1
  done
  kubectl -n "$NS" delete secret "${P}-tls" --ignore-not-found >/dev/null 2>&1
  kubectl -n "$SYSTEM_NS" delete configmap "${P}-ca" --ignore-not-found >/dev/null 2>&1
  kubectl delete namespace "$NSB" "$MDNS" --ignore-not-found --wait=false >/dev/null 2>&1
  if [ -s "$SCRATCH/csidriver.yaml" ] && ! kubectl get csidriver model.csi.gpustack.ai >/dev/null 2>&1; then
    kubectl apply -f "$SCRATCH/csidriver.yaml" >/dev/null 2>&1
  fi
  settings_restore
  if [ -n "$TMPFS_NODE" ]; then
    docker exec "$TMPFS_NODE" umount -l "$CACHE_ROOT" >/dev/null 2>&1 || true
    kubectl -n "$SYSTEM_NS" delete pod "$(plugin_pod "$TMPFS_NODE")" --wait=false >/dev/null 2>&1 || true
  fi
  rm -rf "$SCRATCH"
  if [ "$PSA_SNAPSHOT" = 1 ]; then
    labels=(pod-security.kubernetes.io/enforce- pod-security.kubernetes.io/enforce-version-)
    [ -n "$ORIG_ENFORCE" ] && labels[0]="pod-security.kubernetes.io/enforce=${ORIG_ENFORCE}"
    [ -n "$ORIG_ENFORCE_VERSION" ] && labels[1]="pod-security.kubernetes.io/enforce-version=${ORIG_ENFORCE_VERSION}"
    kubectl label namespace "$NS" --overwrite "${labels[@]}" >/dev/null || {
      echo "[case-21] failed to restore PSA labels on namespace ${NS}" >&2
      exit 1
    }
  fi
}
trap cleanup EXIT

get_uid() { kubectl -n "$1" get modelartifacts.worker.gpustack.ai "$2" -o jsonpath='{.metadata.uid}'; }

hub_gets() { # hub_gets HUB_NAME REPO
  mh_log "$NS" "$1" | python3 -c "
import collections, json, sys
c = collections.Counter(json.loads(l)['path'] for l in sys.stdin if l.strip()
                        and json.loads(l).get('status') in (200, 206) and json.loads(l)['method'] == 'GET'
                        and json.loads(l)['path'].startswith('/$2/'))
for p, n in sorted(c.items()): print(p.rsplit('/', 1)[-1], n)"
}

served() { # served HUB_NAME REPO
  mh_log "$NS" "$1" | python3 -c "
import json, sys
print('\n'.join(sorted({json.loads(l)['path'].rsplit('/', 1)[-1] for l in sys.stdin if l.strip()
                        and json.loads(l)['method'] == 'GET' and json.loads(l).get('status') in (200, 206)
                        and json.loads(l)['path'].startswith('/$2/')})))"
}

files_match() { # files_match HUB_NAME NS POD REPO
  local listed want
  listed="$(kubectl -n "$2" logs "$3" 2>/dev/null | sort)"
  want="$(mh_call "$NS" "$1" GET /_manifest | python3 -c "
import json, sys
for p, f in sorted(json.load(sys.stdin)['$4']['files'].items()): print(f['sha256'], p)" | sort)"
  [ -n "$want" ] && [ "$listed" = "$want" ]
}

read -r -a WORKERS <<<"$(model_workers)"
[ "${#WORKERS[@]}" -ge 2 ] || { echo "[case-21] needs two workers, found ${#WORKERS[@]}; NOTHING WAS VERIFIED"; exit 2; }
W1="${WORKERS[0]}" W2="${WORKERS[1]}"
if ! kubectl get csidriver model.csi.gpustack.ai >/dev/null 2>&1; then
  echo "[case-21] the model-manager plugin is not installed; NOTHING WAS VERIFIED"
  exit 2
fi
if [ -z "$IT" ]; then
  IT="$(kubectl get acceleratortypes.worker.gpustack.ai -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
fi

kubectl get csidriver model.csi.gpustack.ai -o json | python3 -c "
import json, sys
o = json.load(sys.stdin)
m = o['metadata']
print(json.dumps({'apiVersion': o['apiVersion'], 'kind': o['kind'], 'spec': o['spec'],
                  'metadata': {k: m[k] for k in ('name', 'labels', 'annotations') if k in m}}))" >"$SCRATCH/csidriver.yaml"

for ns in "$NS" "$NSB"; do
  kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f - >/dev/null || exit 1
  if [ "$ns" = "$NS" ]; then
    ORIG_ENFORCE="$(kubectl get namespace "$NS" -o jsonpath='{.metadata.labels.pod-security\.kubernetes\.io/enforce}')" || exit 1
    ORIG_ENFORCE_VERSION="$(kubectl get namespace "$NS" -o jsonpath='{.metadata.labels.pod-security\.kubernetes\.io/enforce-version}')" || exit 1
    PSA_SNAPSHOT=1
  fi
  kubectl label namespace "$ns" --overwrite pod-security.kubernetes.io/enforce=restricted \
    pod-security.kubernetes.io/enforce-version=latest >/dev/null
done

# =============================================================================================
# Phase 1: Cold/warm dedup, PSA restricted, forged attributes, corrupt, crash resume,
#          status guards, concurrency Setting, CSIDriver gate, HTTPS proxy & real Hub
# =============================================================================================
echo "== Phase 1: cold/warm CSI mount, security boundaries, guards, proxy & real Hub =="
REPOS="$(python3 -c "
import json
mib = 1 << 20
print(json.dumps({
  'e2e/cold': {'files': {'config.json': {'size': 300}, 'tokenizer.json': {'size': 3000},
                         'model.safetensors': {'size': 5 * mib, 'lfs': True}}},
  'e2e/corrupt': {'files': {'config.json': {'size': 300}, 'model.safetensors': {'size': mib, 'lfs': True}}},
  'e2e/restart': {'files': {'config.json': {'size': 300}, 'model.safetensors': {'size': 8 * mib, 'lfs': True}}},
  'e2e/other': {'files': {'config.json': {'size': 400}}},
  'e2e/guard': {'files': {'config.json': {'size': 100}}},
  'e2e/private': {'token': '${TOKEN}', 'files': {'config.json': {'size': 500}, 'w.bin': {'size': 70000, 'lfs': True}}},
}))")"
HUB_URL="$(mh_deploy "$NS" "${P}-hub" "$REPOS")"
setting_set model-artifact-huggingface-endpoint "$HUB_URL"
setting_set model-artifact-delivery-mode Node
settings_settle
echo "[case-21] hub at ${HUB_URL}; workers ${W1}, ${W2}"

# Cold mount dedup.
mh_call "$NS" "${P}-hub" POST "/_control?throttle=40960" >/dev/null
artifact "$NS" "${P}-cold" e2e/cold "" 21
DIGEST="$(wait_resolved "$NS" "${P}-cold" 120)"
UID_COLD="$(get_uid "$NS" "${P}-cold")"
if [ -z "$DIGEST" ]; then
  record FAIL "the artifact resolves against the test hub" "${P}-cold"
else
  START=$(date +%s)
  consumer "$NS" "${P}-p1" "$W1" "${P}-cold" "$UID_COLD" "$DIGEST"
  consumer "$NS" "${P}-p2" "$W1" "${P}-cold" "$UID_COLD" "$DIGEST"
  COLD_RAN=""
  if pod_ready "$NS" "${P}-p1" 900 && pod_ready "$NS" "${P}-p2" 300; then
    COLD_RAN=1
    ELAPSED=$(( $(date +%s) - START ))
    if [ "$ELAPSED" -gt 120 ]; then
      record PASS "the cold mount outlasted one mount call's 120 s (${ELAPSED}s)" "${P}-p1"
    else
      record FAIL "the cold mount outlasted 120 s (took ${ELAPSED}s; throttle too weak to prove it)" "${P}-p1"
    fi
    gets="$(hub_gets "${P}-hub" e2e/cold)"
    if echo "$gets" | awk '$2 != 1 {bad=1} END {exit bad}' && [ "$(echo "$gets" | wc -l | tr -d ' ')" = 3 ]; then
      record PASS "each of 3 files was fetched once for two Pods" "e2e/cold: $(echo "$gets" | tr '\n' ' ')"
    else
      record FAIL "each file was fetched once" "e2e/cold: $(echo "$gets" | tr '\n' ' ')"
    fi
    if files_match "${P}-hub" "$NS" "${P}-p1" e2e/cold && files_match "${P}-hub" "$NS" "${P}-p2" e2e/cold; then
      record PASS "both Pods read every file with the hub's SHA-256" "${P}-p1, ${P}-p2"
    else
      record FAIL "both Pods read the hub's bytes" "${P}-p1, ${P}-p2"
    fi
    if [ "$(store_state "$W1" "$DIGEST")" = "Ready|" ]; then
      record PASS "the node lists the digest Ready" "$W1"
    else
      record FAIL "the node lists the digest Ready (got $(store_state "$W1" "$DIGEST"))" "$W1"
    fi
  else
    record FAIL "both cold Pods run" "$(pod_mount_events "$NS" "${P}-p1" | tail -1)"
  fi

  # Warm mount.
  mh_call "$NS" "${P}-hub" POST "/_control?throttle=0" >/dev/null
  before_gets="$(hub_gets "${P}-hub" e2e/cold)"
  before_bytes="$(plugin_metric "$W1" gpustack_model_manager_download_bytes_total)"
  consumer "$NS" "${P}-p3" "$W1" "${P}-cold" "$UID_COLD" "$DIGEST"
  if pod_ready "$NS" "${P}-p3" 120; then
    after_bytes="$(plugin_metric "$W1" gpustack_model_manager_download_bytes_total)"
    if [ "${before_bytes:-0}" -gt 0 ] && [ "$(hub_gets "${P}-hub" e2e/cold)" = "$before_gets" ] && [ "$after_bytes" = "$before_bytes" ]; then
      record PASS "a third Pod runs with no request and no byte downloaded" "${P}-p3 (${before_bytes} bytes before and after)"
    else
      record FAIL "a third Pod downloads nothing" "${P}-p3 (bytes ${before_bytes:-<none>} before, ${after_bytes:-<none>} after)"
    fi
  else
    record FAIL "a third Pod on the warm node runs" "${P}-p3"
  fi

  # Restricted PSA control.
  if [ -n "$COLD_RAN" ]; then
    record PASS "the consumers run in a namespace enforcing restricted" "$NS"
  else
    record FAIL "the consumers run in a namespace enforcing restricted" "the cold Pods did not run"
  fi
  if kubectl -n "$NS" run "${P}-hostpath" --image="$MH_PYTHON" --restart=Never --dry-run=server \
       --overrides='{"spec":{"volumes":[{"name":"h","hostPath":{"path":"/tmp"}}]}}' >/dev/null 2>"$SCRATCH/psa"; then
    record FAIL "the same namespace refuses a hostPath Pod (the restricted label is in force)" "$NS"
  else
    if grep -q "violates PodSecurity" "$SCRATCH/psa"; then
      record PASS "the same namespace refuses a hostPath Pod" "$NS"
    else
      record FAIL "the hostPath Pod is refused by PodSecurity" "$(head -1 "$SCRATCH/psa")"
    fi
  fi

  # Forged attributes.
  artifact "$NSB" "${P}-mine" e2e/other "" 21
  MINE="$(wait_resolved "$NSB" "${P}-mine" 120)"
  UID_MINE="$(get_uid "$NSB" "${P}-mine")"
  consumer "$NSB" "${P}-own" "$W1" "${P}-mine" "$UID_MINE" "$MINE"
  consumer "$NSB" "${P}-f1" "$W1" "${P}-cold" "$UID_COLD" "$DIGEST"
  consumer "$NSB" "${P}-f2" "$W1" "${P}-mine" "$UID_MINE" "$DIGEST"
  consumer "$NSB" "${P}-f3" "$W1" "${P}-cold" "$UID_COLD" "$DIGEST" "" "
          csi.storage.k8s.io/pod.namespace: \"${NS}\""
  if pod_ready "$NSB" "${P}-own" 180; then
    record PASS "the namespace's own artifact mounts (the baseline)" "${NSB}/${P}-own"
  else
    record FAIL "the namespace's own artifact mounts" "${NSB}/${P}-own"
  fi
  sleep 20
  for row in "f1|another namespace's artifact|has no such ModelArtifact" \
             "f2|its own artifact with another digest|resolves to another digest" \
             "f3|a forged pod.namespace attribute|has no such ModelArtifact"; do
    IFS='|' read -r pod what rule <<<"$row"
    phase="$(kubectl -n "$NSB" get pod "${P}-${pod}" -o jsonpath='{.status.phase}')"
    if [ "$phase" != Running ] && pod_mount_events "$NSB" "${P}-${pod}" | grep -q "$rule"; then
      record PASS "${what} is refused: ${rule}" "${NSB}/${P}-${pod}"
    else
      record FAIL "${what} is refused: ${rule} (phase ${phase})" "$(pod_mount_events "$NSB" "${P}-${pod}" | tail -1)"
    fi
  done
fi

# Corrupting source.
mh_call "$NS" "${P}-hub" POST "/_control?corrupt=e2e/corrupt" >/dev/null
artifact "$NS" "${P}-corrupt" e2e/corrupt "" 21
CD="$(wait_resolved "$NS" "${P}-corrupt" 120)"
consumer "$NS" "${P}-pc" "$W2" "${P}-corrupt" "$(get_uid "$NS" "${P}-corrupt")" "$CD"
state=""
for _ in $(seq 1 60); do state="$(store_state "$W2" "$CD")"; [ "$state" = "Failed|IntegrityMismatch" ] && break; sleep 3; done
if [ "$state" = "Failed|IntegrityMismatch" ] && [ "$(kubectl -n "$NS" get pod "${P}-pc" -o jsonpath='{.status.phase}')" != Running ]; then
  record PASS "a corrupting source is never published: Failed, IntegrityMismatch, and the Pod does not run" "$W2"
else
  record FAIL "a corrupting source is refused (store: ${state})" "$W2"
fi
mh_call "$NS" "${P}-hub" POST "/_control?corrupt=" >/dev/null

# Killed mid-download.
mh_call "$NS" "${P}-hub" POST "/_control?throttle=40960" >/dev/null
artifact "$NS" "${P}-restart" e2e/restart "" 21
RD="$(wait_resolved "$NS" "${P}-restart" 120)"
consumer "$NS" "${P}-pk" "$W2" "${P}-restart" "$(get_uid "$NS" "${P}-restart")" "$RD"
for _ in $(seq 1 30); do [ -n "$(hub_gets "${P}-hub" e2e/restart)" ] && break; sleep 2; done
sleep 10
kubectl -n "$SYSTEM_NS" delete pod "$(plugin_pod "$W2")" --wait=false >/dev/null
early=0 samples=0
for i in $(seq 1 150); do
  mounted="$(kubectl -n "$NS" get pod "${P}-pk" -o jsonpath="{.status.conditions[?(@.type=='PodReadyToStartContainers')].status}")"
  state="$(store_state "$W2" "$RD")"
  samples=$((samples + 1))
  [ "$mounted" = True ] && [ "${state%%|*}" != Ready ] && early=$((early + 1))
  [ "$mounted" = True ] && break
  [ "$i" = 20 ] && mh_call "$NS" "${P}-hub" POST "/_control?throttle=0" >/dev/null
  sleep 3
done
if [ "$early" = 0 ]; then
  record PASS "no sample of ${samples} showed the Pod mounted before the digest was Ready" "${NS}/${P}-pk"
else
  record FAIL "the Pod was mounted before publication in ${early} samples" "${NS}/${P}-pk"
fi
if pod_ready "$NS" "${P}-pk" 300 && files_match "${P}-hub" "$NS" "${P}-pk" e2e/restart; then
  record PASS "after the restart the Pod runs on the manifest's bytes" "${NS}/${P}-pk"
else
  record FAIL "after the restart the Pod runs on the manifest's bytes" "$(pod_mount_events "$NS" "${P}-pk" | tail -1)"
fi

# Private repo + token redaction.
printf '%s' "$TOKEN" | kubectl -n "$NS" create secret generic "${P}-token" --from-file=token=/dev/stdin >/dev/null
artifact "$NS" "${P}-private" e2e/private "${P}-token" 21
PD="$(wait_resolved "$NS" "${P}-private" 120)"
consumer "$NS" "${P}-priv" "$W2" "${P}-private" "$(get_uid "$NS" "${P}-private")" "$PD" "${P}-token"
if pod_ready "$NS" "${P}-priv" 240; then
  record PASS "a private repository mounts with the Secret kubelet hands over" "${NS}/${P}-priv"
else
  record FAIL "a private repository mounts" "$(pod_mount_events "$NS" "${P}-priv" | tail -1)"
fi
scan() {
  for pod in $(kubectl -n "$SYSTEM_NS" get pods -l 'app.kubernetes.io/component in (model-manager,operator)' -o name); do
    kubectl -n "$SYSTEM_NS" logs "$pod" --all-containers 2>/dev/null
  done
  kubectl get events -A -o json 2>/dev/null
  kubectl get nodemodelstores.worker.gpustack.ai -o json 2>/dev/null
  kubectl get modelartifacts.worker.gpustack.ai -A -o json 2>/dev/null
  for w in "$W1" "$W2"; do
    plugin_metrics "$w"
  done
}
{ scan; printf 'planted %s\n' "$TOKEN"; } >"$SCRATCH/planted"
scan >"$SCRATCH/scan"
if [ "$(grep -c -F "$TOKEN" "$SCRATCH/planted")" -ge 1 ] && [ "$(grep -c -F "$TOKEN" "$SCRATCH/scan")" = 0 ] \
   && [ "$(wc -c <"$SCRATCH/scan")" -gt 10000 ]; then
  record PASS "the token is in no log, event, status or metric ($(wc -c <"$SCRATCH/scan") bytes scanned; planted copy found)" "all"
else
  record FAIL "the token is in no output ($(grep -c -F "$TOKEN" "$SCRATCH/scan") matches)" "all"
fi

# NodeModelStore status guard.
SERVER="$(kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.server}')"
kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d >"$SCRATCH/ca.crt"
status_as() {
  local code
  kubectl get nodemodelstores.v1alpha1.worker.gpustack.ai "$2" -o json | python3 -c "
import json, sys
o = json.load(sys.stdin)
o.setdefault('status', {}).setdefault('capacity', {})['usedPercent'] = 95
print(json.dumps(o))" >"$SCRATCH/object"
  printf 'Authorization: Bearer %s\n' "$1" >"$SCRATCH/auth"
  code="$(curl -sS -o "$SCRATCH/answer" -w '%{http_code}' --cacert "$SCRATCH/ca.crt" -H @"$SCRATCH/auth" \
    -X PUT -H 'Content-Type: application/json' --data @"$SCRATCH/object" \
    "${SERVER}/apis/worker.gpustack.ai/v1alpha1/nodemodelstores/$2/status")"
  rm -f "$SCRATCH/auth"
  cat "$SCRATCH/answer"
  case "$code" in 2*) return 0 ;; *) return 1 ;; esac
}
PLUGIN1="$(plugin_pod "$W1")"
BOUND="$(kubectl -n "$SYSTEM_NS" create token "$SA" --bound-object-kind Pod --bound-object-name "$PLUGIN1" --duration=10m)"
UNBOUND="$(kubectl -n "$SYSTEM_NS" create token "$SA" --duration=10m)"
patch='{"status":{"capacity":{"totalBytes":1,"storedBytes":1,"usedPercent":95}}}'
if status_as "$BOUND" "$W1" >"$SCRATCH/own" 2>&1; then
  record PASS "the plugin's token bound to its Pod writes its own node's status (the baseline)" "$W1"
else
  record FAIL "the plugin writes its own node's status" "$(tr -d "\n" <"$SCRATCH/own" | cut -c1-300)"
fi
for row in "$BOUND|$W2|the plugin writes only the status of the node its Pod runs on|the plugin on another node" \
           "$UNBOUND|$W1|the plugin's token must be bound to its Pod|a token bound to no Pod" \
           "|$W1|only the model-manager plugin writes a NodeModelStore's status|the cluster administrator"; do
  IFS='|' read -r token node rule who <<<"$row"
  if [ -n "$token" ]; then
    status_as "$token" "$node" >"$SCRATCH/out" 2>&1
  else
    kubectl patch nodemodelstores.v1alpha1.worker.gpustack.ai "$node" --subresource=status --type=merge -p "$patch" >"$SCRATCH/out" 2>&1
  fi
  rc=$?
  if [ "$rc" != 0 ] && grep -q -F "$rule" "$SCRATCH/out"; then
    record PASS "${who} is refused: ${rule}" "$node"
  else
    record FAIL "${who} is refused: ${rule} (rc ${rc})" "$(tr -d "\n" <"$SCRATCH/out" | cut -c1-300)"
  fi
done

# ModelArtifact status guard.
artifact "$NS" "${P}-guard" e2e/guard "" 21
if [ -n "$(wait_resolved "$NS" "${P}-guard" 120)" ]; then
  record PASS "the worker's own status writes are admitted: a new artifact resolves" "${NS}/${P}-guard"
else
  record FAIL "the worker's own status writes are admitted" "${NS}/${P}-guard"
fi
kubectl -n "$NS" patch modelartifacts.v1alpha1.worker.gpustack.ai "${P}-guard" --subresource=status --type=merge \
  -p '{"status":{"resolved":{"manifestDigest":"sha256:0000000000000000000000000000000000000000000000000000000000000000"}}}' >"$SCRATCH/ma" 2>&1
rc=$?
if [ "$rc" != 0 ] && grep -q -F "only the worker writes a ModelArtifact's status" "$SCRATCH/ma"; then
  record PASS "the administrator's ModelArtifact status write is refused: only the worker writes it" "${NS}/${P}-guard"
else
  record FAIL "the administrator's ModelArtifact status write is refused (rc ${rc})" "$(head -1 "$SCRATCH/ma")"
fi

# Concurrency Setting propagation.
cur="$(setting_get model-store-download-concurrency)"
want=5
[ "$cur" = 5 ] && want=6
setting_set model-store-download-concurrency "$want"
converged=0
for _ in $(seq 1 30); do
  converged=1
  for n in $(kubectl get nodemodelstores.worker.gpustack.ai -o name); do
    got="$(kubectl get "$n" -o jsonpath='{.spec.download.concurrency}|{.metadata.generation}|{.status.observedGeneration}')"
    IFS='|' read -r c g o <<<"$got"
    { [ "$c" = "$want" ] && [ "$g" = "$o" ]; } || converged=0
  done
  [ "$converged" = 1 ] && break
  sleep 2
done
count="$(kubectl get nodemodelstores.worker.gpustack.ai -o name | wc -l | tr -d ' ')"
if [ "$converged" = 1 ] && [ "$count" -ge 2 ]; then
  record PASS "model-store-download-concurrency=${want} reached ${count} nodes' spec and every plugin applied it" "all nodes"
else
  record FAIL "the Setting reached every node's spec and observedGeneration within 60 s" "$(kubectl get nodemodelstores.worker.gpustack.ai -o jsonpath='{range .items[*]}{.metadata.name}={.spec.download.concurrency}/{.metadata.generation}/{.status.observedGeneration} {end}')"
fi
settings_restore

# CSIDriver gate for Node delivery.
set_delivery() {
  kubectl -n "$SYSTEM_NS" patch settings.gpustack.ai model-artifact-delivery-mode --type=merge \
    -p "{\"spec\":{\"value\":\"$1\"}}" >"$SCRATCH/set" 2>&1
}
set_delivery Engine || record FAIL "Engine is accepted" "$(head -1 "$SCRATCH/set")"
kubectl delete csidriver model.csi.gpustack.ai >/dev/null
gone=0
for _ in $(seq 1 30); do [ -z "$(kubectl get nodemodelstores.worker.gpustack.ai -o name)" ] && { gone=1; break; }; sleep 2; done
if [ "$gone" = 1 ]; then
  record PASS "without the CSIDriver every NodeModelStore is removed" "all nodes"
else
  record FAIL "without the CSIDriver every NodeModelStore is removed" "$(kubectl get nodemodelstores.worker.gpustack.ai -o name | tr '\n' ' ')"
fi
if set_delivery Node; then
  record FAIL "Node is refused while the CSIDriver is absent" "model-artifact-delivery-mode"
else
  if grep -q "does not exist" "$SCRATCH/set"; then
    record PASS "Node is refused while the CSIDriver is absent" "model-artifact-delivery-mode"
  else
    record FAIL "Node is refused for the CSIDriver" "$(head -1 "$SCRATCH/set")"
  fi
fi
kubectl apply -f "$SCRATCH/csidriver.yaml" >/dev/null
back=0
for _ in $(seq 1 60); do [ "$(kubectl get nodemodelstores.worker.gpustack.ai -o name | wc -l | tr -d ' ')" -ge "$count" ] && { back=1; break; }; sleep 2; done
if [ "$back" = 1 ]; then
  record PASS "with the CSIDriver back every NodeModelStore returns" "${count} nodes"
else
  record FAIL "with the CSIDriver back every NodeModelStore returns" "$(kubectl get nodemodelstores.worker.gpustack.ai -o name | wc -l)"
fi
if set_delivery Node; then
  record PASS "with the CSIDriver back Node is accepted" "model-artifact-delivery-mode"
else
  record FAIL "with the CSIDriver back Node is accepted" "$(head -1 "$SCRATCH/set")"
fi
for _ in $(seq 1 30); do
  [ "$(kubectl get nodemodelstore "$W1" -o jsonpath="{.status.conditions[?(@.type=='Ready')].status}" 2>/dev/null)" = True ] && break
  sleep 2
done

# HTTPS proxy + CA bundle + direct control.
HOST="${TLS_HUB}.${NS}.svc"
TLS_REPOS='{"e2e/proxied":{"files":{"config.json":{"size":200},"w.bin":{"size":300000,"lfs":true}}},"e2e/direct":{"files":{"config.json":{"size":210},"w.bin":{"size":310000,"lfs":true}}}}'
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=${HOST}" -addext "subjectAltName=DNS:${HOST}" \
  -keyout "$SCRATCH/tls.key" -out "$SCRATCH/tls.crt" >/dev/null 2>&1
kubectl -n "$NS" create secret tls "${P}-tls" --cert="$SCRATCH/tls.crt" --key="$SCRATCH/tls.key" >/dev/null
kubectl -n "$SYSTEM_NS" create configmap "${P}-ca" --from-file=ca.crt="$SCRATCH/tls.crt" >/dev/null
TLS_URL="$(mh_deploy "$NS" "$TLS_HUB" "$TLS_REPOS" "${P}-tls")"
PROXY_URL="$(mh_proxy "$NS" "${P}-proxy")"
setting_set model-artifact-ca-bundle "${P}-ca"
setting_set model-artifact-no-proxy ""
setting_set model-artifact-https-proxy "$PROXY_URL"
setting_set model-artifact-huggingface-endpoint "$TLS_URL"
settings_settle

remotes() {
  mh_log "$NS" "$TLS_HUB" | python3 -c "
import json, sys
print(' '.join(sorted({json.loads(l)['remote'] for l in sys.stdin if l.strip() and json.loads(l)['method'] == 'GET'
                        and json.loads(l)['path'].startswith('/$1/') and json.loads(l).get('status') in (200, 206)})))"
}
fetch() { # name repo pod
  artifact "$NS" "$1" "$2" "" 21
  local d
  d="$(wait_resolved "$NS" "$1" 120)"
  [ -n "$d" ] || return 1
  consumer "$NS" "$3" "$W1" "$1" "$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "$1" -o jsonpath='{.metadata.uid}')" "$d"
  pod_ready "$NS" "$3" 240
}
PROXY_IP="$(mh_pod_ip "$NS" "${P}-proxy")"
PLUGIN_IP="$(kubectl -n "$SYSTEM_NS" get pod "$(plugin_pod "$W1")" -o jsonpath='{.status.podIP}')"
if fetch "${P}-proxied" e2e/proxied "${P}-pp"; then
  via="$(remotes e2e/proxied)"
  if [ "$via" = "$PROXY_IP" ]; then
    record PASS "through the HTTPS proxy every file reached the hub from the proxy (${via})" "e2e/proxied"
  else
    record FAIL "every file reached the hub from the proxy ${PROXY_IP} (saw ${via})" "e2e/proxied"
  fi
  tunnels="$(mh_log "$NS" "${P}-proxy" | grep -c -F "\"connect\": \"${HOST}:8443\"")"
  if [ "$tunnels" -ge 1 ]; then
    record PASS "the proxy tunneled to the hub (${tunnels} tunnels)" "${P}-proxy"
  else
    record FAIL "the proxy tunneled to the hub" "${P}-proxy"
  fi
else
  record FAIL "a cold download through the HTTPS proxy and the CA bundle mounts" "$(pod_mount_events "$NS" "${P}-pp" | tail -1)"
fi
setting_set model-artifact-https-proxy ""
settings_settle
if fetch "${P}-direct" e2e/direct "${P}-pd"; then
  direct="$(remotes e2e/direct)"
  if [ "$PROXY_IP" = "$PLUGIN_IP" ]; then
    record SKIP "undecidable: the proxy and the plugin share the address ${PLUGIN_IP}" "e2e/direct"
  elif [ "$direct" = "$PLUGIN_IP" ]; then
    record PASS "without the proxy the files reached the hub from the plugin (${direct})" "e2e/direct"
  else
    record FAIL "without the proxy the files reached the hub from the plugin ${PLUGIN_IP} (saw ${direct})" "e2e/direct"
  fi
else
  record FAIL "a cold download without the proxy mounts" "$(pod_mount_events "$NS" "${P}-pd" | tail -1)"
fi

# Real Hub.
if [ "${E2E_C21_OFFLINE:-0}" = 1 ]; then
  record SKIP "the real Hub (E2E_C21_OFFLINE=1): NOTHING WAS VERIFIED on it" "hf-internal-testing/tiny-random-bert"
else
  setting_unset model-artifact-ca-bundle
  setting_set model-artifact-huggingface-endpoint https://huggingface.co
  settings_settle
  REAL=hf-internal-testing/tiny-random-bert
  artifact "$NS" "${P}-real" "$REAL" "" 21 "
  allowPatterns: [\"*.json\", \"*.txt\", \"model.safetensors\"]"
  RED="$(wait_resolved "$NS" "${P}-real" 180)"
  consumer "$NS" "${P}-pr" "$W2" "${P}-real" "$(get_uid "$NS" "${P}-real")" "$RED"
  commit="$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-real" -o jsonpath='{.status.resolved.revision}')"
  want="$(curl -fsSL "https://huggingface.co/${REAL}/resolve/${commit}/model.safetensors" 2>/dev/null \
    | python3 -c "import hashlib, sys; print(hashlib.sha256(sys.stdin.buffer.read()).hexdigest())")"
  got=""
  if pod_ready "$NS" "${P}-pr" 300; then
    for _ in $(seq 1 15); do
      got="$(kubectl -n "$NS" logs "${P}-pr" | awk '$2 == "model.safetensors" {print $1}')"
      [ -n "$got" ] && break
      sleep 2
    done
  fi
  if [ -n "$got" ] && [ "$got" = "$want" ] && [ -n "$commit" ]; then
    record PASS "the real Hub serves through its redirects; model.safetensors hashes as this machine's download of it" "$REAL@${commit:0:12}"
  else
    record FAIL "the real Hub serves the weights (pod ${got:-<none>}, here ${want:-<none>})" "$(pod_mount_events "$NS" "${P}-pr" | tail -1)"
  fi
fi

# Clean up Phase 1 consumer Pods before Phase 2.
for ns in "$NS" "$NSB"; do
  kubectl -n "$ns" delete pods -l e2e.gpustack.ai/consumer=true --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1
done

# =============================================================================================
# Phase 2: Plugin rollout, filtered delivery, consumers, restart reconciliation, GC & Node delete
# =============================================================================================
echo "== Phase 2: plugin rollout, filtered delivery, consumers, GC eviction & reconciliation =="
kubectl create namespace "$MDNS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
if docker exec "$W2" true >/dev/null 2>&1; then DOCKER_W2=1; else DOCKER_W2=0; fi

mount_node() { # ARTIFACT_NS NAME NODE POD
  local d
  d="$(wait_resolved "$1" "$2" 120)"
  consumer "$1" "$4" "$3" "$2" "$(get_uid "$1" "$2")" "$d"
  printf '%s' "$d"
}
weights() {
  kubectl -n "$MDNS" get modeldeployments.worker.gpustack.ai "$1" \
    -o jsonpath="{.status.conditions[?(@.type=='WeightsReady')].status}|{.status.conditions[?(@.type=='WeightsReady')].reason}" 2>/dev/null
}
entries() {
  kubectl get nodemodelstores.worker.gpustack.ai "$1" -o json 2>/dev/null | python3 -c "
import json, sys
for m in sorted(json.load(sys.stdin).get('status', {}).get('models', []), key=lambda m: m['digest']):
    print(m['digest'], m['state'], m.get('sizeBytes', 0), m.get('referenced', False))"
}
wait_entries() {
  for _ in $(seq 1 $(( $3 / 3 ))); do
    [ "$(entries "$1")" = "$2" ] && return 0
    sleep 3
  done
  return 1
}
plugin_ready() {
  local pod
  for _ in $(seq 1 $(( $2 / 3 ))); do
    pod="$(plugin_pod "$1")"
    [ -n "$pod" ] && [ "$(kubectl -n "$SYSTEM_NS" get pod "$pod" -o jsonpath='{.status.containerStatuses[?(@.name=="main")].ready}')" = true ] && return 0
    sleep 3
  done
  return 1
}

REPOS2="$(python3 -c "
import json
mib = 1 << 20
print(json.dumps({
  'e2e/gc-a': {'files': {'config.json': {'size': 100}, 'w.bin': {'size': 20 * mib, 'lfs': True}}},
  'e2e/gc-b': {'files': {'config.json': {'size': 110}, 'w.bin': {'size': 20 * mib, 'lfs': True}}},
  'e2e/gc-c': {'files': {'config.json': {'size': 120}, 'w.bin': {'size': 20 * mib, 'lfs': True}}},
  'e2e/read': {'files': {'config.json': {'size': 200}, 'w.bin': {'size': mib, 'lfs': True}}},
  'e2e/roll': {'files': {'config.json': {'size': 210}, 'w.bin': {'size': 2 * mib, 'lfs': True}}},
  'e2e/filtered': {'files': {'config.json': {'size': 300}, 'tokenizer.json': {'size': 310},
                             'original/w.pth': {'size': mib, 'lfs': True}, 'w.bin': {'size': mib, 'lfs': True}}},
  'e2e/instance': {'files': {'config.json': {'size': 400}}},
  'e2e/md': {'files': {'config.json': {'size': 500}, 'w.bin': {'size': 4 * mib, 'lfs': True}}},
  'e2e/drv': {'files': {'config.json': {'size': 600}}},
  'e2e/lifecycle-model': {'files': {'config.json': {'size': 300},
                                    'model.safetensors': {'size': mib, 'lfs': True}}},
}))")"
HUB2_URL="$(mh_deploy "$NS" "${P}-hub2" "$REPOS2")"
setting_unset model-artifact-ca-bundle
setting_set model-artifact-https-proxy ""
setting_set model-artifact-huggingface-endpoint "$HUB2_URL"
setting_set model-artifact-delivery-mode Node
settings_settle

# Collection part 1: fill tmpfs on W2.
GC=0 D_a="" D_b="" D_c="" RELEASED=0
if [ "$DOCKER_W2" = 1 ] && docker exec "$W2" mount -t tmpfs -o size=100m tmpfs "$CACHE_ROOT" >/dev/null 2>&1; then
  TMPFS_NODE="$W2"
  kubectl -n "$SYSTEM_NS" delete pod "$(plugin_pod "$W2")" --wait=true >/dev/null
  total=""
  for _ in $(seq 1 60); do
    total="$(kubectl get nodemodelstores.worker.gpustack.ai "$W2" -o jsonpath='{.status.capacity.totalBytes}' 2>/dev/null)"
    [ -n "$total" ] && [ "$total" -le $((128 << 20)) ] && break
    sleep 2
  done
  if [ -n "$total" ] && [ "$total" -le $((128 << 20)) ]; then
    GC=1
    for x in a b c; do artifact "$NS" "${P}-gc-${x}" "e2e/gc-${x}" "" 21; done
    fill() {
      local d
      d="$(mount_node "$NS" "${P}-gc-$1" "$W2" "${P}-gc-$1")"
      printf -v "D_$1" '%s' "$d"
      pod_ready "$NS" "${P}-gc-$1" 240 || record FAIL "the collection fixture $1 mounts" "$(pod_mount_events "$NS" "${P}-gc-$1" | tail -1)"
    }
    fill a
    kubectl -n "$NS" delete pod "${P}-gc-a" --wait=true >/dev/null
    sleep 5
    fill b
    kubectl -n "$NS" delete pod "${P}-gc-b" --wait=true >/dev/null
    fill c
    RELEASED="$(date +%s)"
  else
    record FAIL "the plugin reports the tmpfs cache root" "totalBytes=${total:-<none>}"
  fi
else
  record SKIP "collection: no docker access to the kind node ${W2}; NOTHING WAS VERIFIED on collection" "$W2"
fi

# Plugin DaemonSet rolling restart.
artifact "$NS" "${P}-read" e2e/read "" 21
artifact "$NS" "${P}-roll" e2e/roll "" 21
RDIG="$(wait_resolved "$NS" "${P}-read" 120)"
kubectl apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata: {name: ${P}-reader, namespace: ${NS}, labels: {e2e.gpustack.ai/consumer: "true"}}
spec:
  nodeName: ${W1}
  terminationGracePeriodSeconds: 1
  securityContext: {runAsNonRoot: true, runAsUser: 65534, seccompProfile: {type: RuntimeDefault}}
  containers:
    - name: c
      image: ${MH_PYTHON}
      command: ["python3", "-c", "import hashlib,os,time\ndef h():\n  out={}\n  for r,_,fs in os.walk('/model'):\n    for f in fs:\n      p=os.path.join(r,f); out[p]=hashlib.sha256(open(p,'rb').read()).hexdigest()\n  return out\nfirst=h()\nwhile True:\n  try: print(int(time.time()), 'ok' if h()==first and first else 'mismatch', flush=True)\n  except Exception as e: print(int(time.time()), 'error', e, flush=True)\n  time.sleep(1)"]
      securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
      volumeMounts: [{name: model, mountPath: /model, readOnly: true}]
  volumes:
    - name: model
      csi:
        driver: model.csi.gpustack.ai
        readOnly: true
        volumeAttributes: {artifact: ${P}-read, artifactUID: "$(get_uid "$NS" "${P}-read")", manifestDigest: "${RDIG}"}
YAML
if pod_ready "$NS" "${P}-reader" 240; then
  sleep 5
  ROLL_START="$(date +%s)"
  kubectl -n "$SYSTEM_NS" rollout restart "ds/${DS}" >/dev/null
  sleep 3
  mount_node "$NS" "${P}-roll" "$W1" "${P}-roll" >/dev/null
  kubectl -n "$SYSTEM_NS" rollout status "ds/${DS}" --timeout=600s >/dev/null
  ROLL_END="$(date +%s)"
  sleep 5
  kubectl -n "$NS" logs "${P}-reader" >"$SCRATCH/reader"
  bad="$(awk '$2 != "ok"' "$SCRATCH/reader" | head -1)"
  during="$(awk -v s="$ROLL_START" -v e="$ROLL_END" '$1 >= s && $1 <= e && $2 == "ok"' "$SCRATCH/reader" | wc -l | tr -d ' ')"
  span=$((ROLL_END - ROLL_START))
  if [ -z "$bad" ] && [ "$during" -ge $((span / 2)) ] && [ "$span" -gt 0 ]; then
    record PASS "a mounted Pod read its files ${during} times with no mismatch or error through the ${span}s roll" "${NS}/${P}-reader"
  else
    record FAIL "a mounted Pod reads its files through the roll (${during} ok in ${span}s)" "${bad:-too few readings}"
  fi
  if pod_ready "$NS" "${P}-roll" 300 && files_match "${P}-hub2" "$NS" "${P}-roll" e2e/roll; then
    record PASS "a mount asked for during the roll runs on the manifest's bytes after it" "${NS}/${P}-roll"
  else
    record FAIL "a mount asked for during the roll succeeds after it" "$(pod_mount_events "$NS" "${P}-roll" | tail -1)"
  fi
else
  record FAIL "the reader Pod mounts" "$(pod_mount_events "$NS" "${P}-reader" | tail -1)"
fi
kubectl -n "$NS" delete pod "${P}-reader" "${P}-roll" --wait=false >/dev/null 2>&1

# Filtered artifact.
kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata: {name: ${P}-filtered, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "21"}}
spec:
  source: {huggingFace: {repository: e2e/filtered, revision: main}}
  allowPatterns: ["*.json", "original/"]
  ignorePatterns: ["tokenizer.json"]
YAML
mount_node "$NS" "${P}-filtered" "$W1" "${P}-filtered" >/dev/null
if pod_ready "$NS" "${P}-filtered" 240; then
  sleep 3
  listed="$(kubectl -n "$NS" logs "${P}-filtered" | awk '{print $2}' | sort | tr '\n' ' ')"
  got="$(served "${P}-hub2" e2e/filtered | sort | tr '\n' ' ')"
  if [ "$listed" = "config.json original/w.pth " ] && [ "$got" = "config.json w.pth " ]; then
    record PASS "a filtered artifact holds and downloads only its files: ${listed}" "${NS}/${P}-filtered"
  else
    record FAIL "a filtered artifact holds and downloads only its files" "listed=${listed} served=${got}"
  fi
else
  record FAIL "a filtered artifact mounts" "$(pod_mount_events "$NS" "${P}-filtered" | tail -1)"
fi
kubectl -n "$NS" delete pod "${P}-filtered" --wait=false >/dev/null 2>&1

# ModelDeployment & Instance consumers.
for _ in $(seq 1 30); do [ -n "$(kubectl -n "$MDNS" get localqueues -o name 2>/dev/null)" ] && break; sleep 2; done
md() {
  local at=""
  [ -n "$IT" ] && at="acceleratorType: \"${IT}\", "
  kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata: {name: $1, namespace: ${MDNS}}
spec:
  engine: {name: vLLM, image: "${IMAGE}"}
  model: {name: e2e/c21, artifactRef: {name: $2}}
  roles:
    - {name: server, ${at}replicas: 1, image: "${IMAGE}", command: ["/pause"]}
YAML
}
mdartifact() {
  kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata: {name: $1, namespace: ${MDNS}, labels: {e2e.gpustack.ai/case: "21"}}
spec:
  source: {huggingFace: {repository: $2, revision: main}}${3:-}
YAML
}

setting_set model-artifact-delivery-mode Engine
settings_settle
mdartifact "${P}-mdf" e2e/filtered '
  allowPatterns: ["*.json"]'
wait_resolved "$MDNS" "${P}-mdf" 120 >/dev/null
md "${P}-engine" "${P}-mdf"
reading=""
for _ in $(seq 1 30); do reading="$(weights "${P}-engine")"; [ "$reading" = "False|FilterNeedsNodeDelivery" ] && break; sleep 2; done
pods="$(kubectl -n "$MDNS" get pods -l "app.kubernetes.io/instance=${P}-engine" -o name 2>/dev/null)"
if [ "$reading" = "False|FilterNeedsNodeDelivery" ] && [ -z "$pods" ]; then
  record PASS "under Engine delivery a filtered artifact's deployment reports FilterNeedsNodeDelivery and has no Pod" "${MDNS}/${P}-engine"
else
  record FAIL "under Engine delivery a filtered artifact's deployment is held" "${reading:-<none>} pods=${pods}"
fi
setting_set model-artifact-delivery-mode Node
woke="" pods=""
for _ in $(seq 1 60); do
  reading="$(weights "${P}-engine")"
  pods="$(kubectl -n "$MDNS" get pods -l "app.kubernetes.io/instance=${P}-engine" -o name 2>/dev/null)"
  [ -n "$pods" ] && [ "${reading#*|}" != FilterNeedsNodeDelivery ] && { woke=1; break; }
  sleep 3
done
if [ -n "$woke" ]; then
  record PASS "switched back to Node, the held deployment starts without an edit" "${MDNS}/${P}-engine ${reading}"
else
  record FAIL "switched back to Node, the held deployment starts without an edit" "${reading:-<none>} pods=${pods:-<none>}"
fi
kubectl -n "$MDNS" delete modeldeployments.worker.gpustack.ai "${P}-engine" --wait=false >/dev/null
settings_settle

# Instance on hub artifact.
INST_TYPE="${E2E_MD_INSTANCE_TYPE:-$(kubectl get instancetypes.worker.gpustack.ai -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)}"
[ -n "$INST_TYPE" ] || INST_TYPE="$IT"
mdartifact "${P}-inst" e2e/instance
wait_resolved "$MDNS" "${P}-inst" 120 >/dev/null
kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: Instance
metadata: {name: ${P}-inst, namespace: ${MDNS}}
spec:
  type: "${INST_TYPE}"
  image: "${IMAGE}"
  resources: {cpu: "1", ram: 2Gi, localStorage: 1Gi}
  volume: {ephemeral: {capacity: 1Gi}}
  additionalVolumes: [{mountPath: /models/hub, model: {artifactRef: {name: ${P}-inst}}}]
YAML
phase=""
for _ in $(seq 1 100); do
  phase="$(kubectl -n "$MDNS" get pod "${P}-inst" -o jsonpath='{.status.phase}' 2>/dev/null)"
  [ "$phase" = Running ] && break
  sleep 3
done
vol="$(kubectl -n "$MDNS" get pod "${P}-inst" -o jsonpath="{.spec.volumes[?(@.csi.driver=='model.csi.gpustack.ai')].csi.volumeAttributes.artifact}|{.spec.containers[0].volumeMounts[?(@.mountPath=='/models/hub')].readOnly}" 2>/dev/null)"
if [ "$phase" = Running ] && [ "$vol" = "${P}-inst|true" ]; then
  record PASS "an Instance naming a hub artifact runs with the plugin's read-only volume" "${MDNS}/${P}-inst"
else
  record FAIL "an Instance naming a hub artifact runs with the plugin's volume" "phase=${phase:-<none>} volume=${vol}"
fi
kubectl -n "$MDNS" delete instances.worker.gpustack.ai "${P}-inst" --wait=false >/dev/null

# ModelDeployment on Node delivery (Materializing -> Mounted).
mh_call "$NS" "${P}-hub2" POST "/_control?throttle=65536" >/dev/null
mdartifact "${P}-md" e2e/md
MDIG="$(wait_resolved "$MDNS" "${P}-md" 120)"
md "${P}-node" "${P}-md"
seen="" reading=""
for _ in $(seq 1 120); do
  reading="$(weights "${P}-node")"
  [ -n "$reading" ] && case "${seen} " in *" ${reading#*|} "*) ;; *) seen="${seen} ${reading#*|}" ;; esac
  [ "$reading" = "True|Mounted" ] && break
  sleep 3
done
mh_call "$NS" "${P}-hub2" POST "/_control?throttle=0" >/dev/null
pod="$(kubectl -n "$MDNS" get pods -l "app.kubernetes.io/instance=${P}-node" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
digest="$(kubectl -n "$MDNS" get pod "$pod" -o jsonpath="{.spec.volumes[?(@.csi.driver=='model.csi.gpustack.ai')].csi.volumeAttributes.manifestDigest}" 2>/dev/null)"
delivery="$(kubectl -n "$MDNS" get modeldeployments.worker.gpustack.ai "${P}-node" -o jsonpath='{.status.model.delivery}')"
if [ -n "$MDIG" ] && [ "$digest" = "$MDIG" ]; then
  record PASS "the deployment's Pod carries the plugin's volume with the artifact's digest" "${MDNS}/${pod}"
else
  record FAIL "the deployment's Pod carries the plugin's volume with the artifact's digest" "pod=${pod:-<none>} digest=${digest:-<none>} want=${MDIG}"
fi
case "${seen} " in
  *" Materializing "*)
    if [ "$reading" = "True|Mounted" ] && [ "$delivery" = Node ]; then
      record PASS "WeightsReady went through Materializing to Mounted, delivery Node" "reasons:${seen}"
    else
      record FAIL "WeightsReady reaches Mounted with delivery Node" "last=${reading} delivery=${delivery} reasons:${seen}"
    fi ;;
  *) record FAIL "WeightsReady reports Materializing while the node downloads" "reasons:${seen}" ;;
esac

# Deployment waiting for CSIDriver.
mdartifact "${P}-drv" e2e/drv
wait_resolved "$MDNS" "${P}-drv" 120 >/dev/null
kubectl delete csidriver model.csi.gpustack.ai >/dev/null
md "${P}-drv" "${P}-drv"
reading=""
for _ in $(seq 1 30); do reading="$(weights "${P}-drv")"; [ "$reading" = "False|NodeDeliveryUnavailable" ] && break; sleep 2; done
pods="$(kubectl -n "$MDNS" get pods -l "app.kubernetes.io/instance=${P}-drv" -o name 2>/dev/null)"
if [ "$reading" = "False|NodeDeliveryUnavailable" ] && [ -z "$pods" ]; then
  record PASS "without the CSIDriver a deployment reports NodeDeliveryUnavailable and has no Pod" "${MDNS}/${P}-drv"
else
  record FAIL "without the CSIDriver a deployment is held" "${reading:-<none>} pods=${pods}"
fi
kubectl apply -f "$SCRATCH/csidriver.yaml" >/dev/null
woke=""
for _ in $(seq 1 40); do
  reading="$(weights "${P}-drv")"
  pods="$(kubectl -n "$MDNS" get pods -l "app.kubernetes.io/instance=${P}-drv" -o name 2>/dev/null)"
  [ -n "$pods" ] && [ "${reading#*|}" != NodeDeliveryUnavailable ] && { woke=1; break; }
  sleep 3
done
if [ -n "$woke" ]; then
  record PASS "with the CSIDriver back, the held deployment starts without an edit" "${MDNS}/${P}-drv ${reading}"
else
  record FAIL "with the CSIDriver back, the held deployment starts without an edit" "${reading:-<none>} pods=${pods:-<none>}"
fi
kubectl -n "$MDNS" delete modeldeployments.worker.gpustack.ai "${P}-drv" --wait=false >/dev/null

# Restart & deleted NodeModelStore reconciliation from disk.
LIFE_ART="${P}-life"
LIFE_POD="${P}-life-pod"
artifact "$NS" "$LIFE_ART" e2e/lifecycle-model "" 21
LIFE_DIGEST="$(mount_node "$NS" "$LIFE_ART" "$W1" "$LIFE_POD")"
LIFE_OK=1
if [ -z "$LIFE_DIGEST" ] || ! pod_ready "$NS" "$LIFE_POD" 300; then
  record FAIL "the consumer runs on ${W1}" "$(pod_mount_events "$NS" "$LIFE_POD" | tail -1)"
  LIFE_OK=0
fi
if [ "$LIFE_OK" = 1 ]; then
  BEFORE=""
  for _ in $(seq 1 20); do
    BEFORE="$(entries "$W1")"
    echo "$BEFORE" | grep -q "^${LIFE_DIGEST} Ready [0-9]* True$" && break
    sleep 3
  done
  kubectl -n "$SYSTEM_NS" delete pod "$(plugin_pod "$W1")" --wait=true --timeout=120s >/dev/null 2>&1
  if plugin_ready "$W1" 180 && wait_entries "$W1" "$BEFORE" 120; then
    record PASS "after the plugin restarts the node's entries are rewritten as before" "$W1"
  else
    record FAIL "the plugin rewrites the same entries after a restart" "${W1}: $(entries "$W1" | tr '\n' ';')"
  fi
  LIFE_OLD_UID="$(kubectl get nodemodelstores.worker.gpustack.ai "$W1" -o jsonpath='{.metadata.uid}')"
  kubectl delete nodemodelstores.v1alpha1.worker.gpustack.ai "$W1" --wait=true >/dev/null 2>&1
  LIFE_NEW_UID=""
  for _ in $(seq 1 40); do
    LIFE_NEW_UID="$(kubectl get nodemodelstores.worker.gpustack.ai "$W1" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
    [ -n "$LIFE_NEW_UID" ] && [ "$LIFE_NEW_UID" != "$LIFE_OLD_UID" ] && break
    sleep 3
  done
  if [ -n "$LIFE_NEW_UID" ] && [ "$LIFE_NEW_UID" != "$LIFE_OLD_UID" ] && wait_entries "$W1" "$BEFORE" 420; then
    record PASS "a deleted object comes back and the plugin rewrites the same entries from disk" "${W1}: ${LIFE_OLD_UID:0:8} -> ${LIFE_NEW_UID:0:8}"
  else
    record FAIL "the object is recreated with the node's entries" "${W1}: uid ${LIFE_NEW_UID:-<none>}, $(entries "$W1" | tr '\n' ';')"
  fi
else
  record FAIL "the plugin rewrites the same entries after a restart" "${W1}: the consumer Pod did not mount"
  record FAIL "the object is recreated with the node's entries" "${W1}: the consumer Pod did not mount"
fi

# Collection part 2: evict by watermark.
if [ "$GC" = 1 ]; then
  left=$(( RELEASED + 660 - $(date +%s) ))
  [ "$left" -gt 0 ] && { echo "[case-21] waiting ${left}s for the collection grace"; sleep "$left"; }
  for _ in $(seq 1 30); do [ -n "$(store_state "$W2" "$D_c")" ] && break; sleep 2; done
  gone() {
    for _ in $(seq 1 $(( $2 / 3 ))); do [ -z "$(store_state "$W2" "$1")" ] && return 0; sleep 3; done
    return 1
  }
  held() { [ -n "$(store_state "$W2" "$1")" ]; }
  setting_set model-store-low-watermark 45
  setting_set model-store-high-watermark 50
  if gone "$D_a" 120 && held "$D_b" && held "$D_c"; then
    record PASS "at 50/45 the collector removed only the oldest unreferenced digest" "$W2"
  else
    record FAIL "at 50/45 the collector removes only the oldest" "a=$(store_state "$W2" "$D_a") b=$(store_state "$W2" "$D_b") c=$(store_state "$W2" "$D_c")"
  fi
  setting_set model-store-low-watermark 5
  setting_set model-store-high-watermark 10
  if gone "$D_b" 120 && held "$D_c" && [ -n "$(kubectl -n "$NS" logs "${P}-gc-c" | head -1)" ]; then
    record PASS "at 10/5 the collector removed the other unreferenced digest and kept the referenced one" "$W2"
  else
    record FAIL "at 10/5 the referenced digest stays" "b=$(store_state "$W2" "$D_b") c=$(store_state "$W2" "$D_c")"
  fi
fi

if [ -n "$TMPFS_NODE" ]; then
  docker exec "$TMPFS_NODE" umount -l "$CACHE_ROOT" >/dev/null 2>&1 || true
fi

# Node deletion collects NodeModelStore.
if [ "$DOCKER_W2" = 1 ]; then
  W2_OLD_UID="$(kubectl get nodemodelstores.worker.gpustack.ai "$W2" -o jsonpath='{.metadata.uid}')"
  DELETED_NODE="$W2"
  docker exec "$W2" systemctl stop kubelet >/dev/null 2>&1
  kubectl delete node "$W2" --wait=true >/dev/null 2>&1
  STORE_GONE=""
  for _ in $(seq 1 40); do
    kubectl get nodemodelstores.worker.gpustack.ai "$W2" >/dev/null 2>&1 || { STORE_GONE=1; break; }
    sleep 3
  done
  if [ -n "$W2_OLD_UID" ] && [ -n "$STORE_GONE" ]; then
    record PASS "deleting the Node collects its NodeModelStore" "$W2"
  else
    record FAIL "the NodeModelStore goes with its Node" "${W2}: uid ${W2_OLD_UID:-<none>} still there"
  fi
  docker exec "$W2" systemctl start kubelet >/dev/null 2>&1
  W2_NEW_UID=""
  for _ in $(seq 1 80); do
    W2_NEW_UID="$(kubectl get nodemodelstores.worker.gpustack.ai "$W2" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
    [ -n "$W2_NEW_UID" ] && break
    sleep 3
  done
  if [ -n "$W2_NEW_UID" ] && [ "$W2_NEW_UID" != "$W2_OLD_UID" ]; then
    record PASS "the node registered again gets a new NodeModelStore" "${W2}: ${W2_NEW_UID:0:8}"
    DELETED_NODE=""
  else
    record FAIL "the node registered again gets its object back" "${W2}: $(kubectl get node "$W2" -o name 2>&1)"
  fi
else
  record SKIP "the NodeModelStore goes with its Node" "no docker access to the node ${W2}"
fi

results
