#!/usr/bin/env bash
#
# CASE 101 — Node delivery and the control plane that owns it: one verified download per node, each
#            status guard admitting only its own writer, a Setting that reaches every node, the
#            CSIDriver Node delivery needs, and an HTTPS proxy carrying every download
#            (MUTATING, self-recovering; AUTO-SKIPS the real-Hub row with E2E_C101_OFFLINE=1)
#
#   case-101.sh <NS>
#
# Prerequisites: nodes=2; gpu=none; features=multi-host; cluster=any; inputs=E2E_C101_OFFLINE,E2E_MODEL_MANAGER_SA,MH_PYTHON
# Goal:        Prove node delivery end to end with bare Pods, which is what a tenant can write by
#              hand, and the control-plane contract that governs it on a real API server: a cold
#              mount lasting longer than one kubelet mount call downloads each file once for two
#              Pods and serves the manifest's bytes; a third Pod on the node downloads nothing; a
#              corrupting source is never published; a plugin killed mid-download never exposes a
#              partial tree; a namespace enforcing restricted mounts; forged volume attributes are
#              refused by the rule they break; each status guard admits its writer and refuses the
#              others by the rule it names; a Setting change rewrites every node's spec and every
#              plugin applies it; without the CSIDriver Node delivery is refused and every
#              NodeModelStore is removed; a cold download through an HTTPS proxy reaches the hub
#              only from the proxy; the real Hub serves through its redirects; and no token reaches
#              any output.
# Environment: A cluster installed from this chart with modelManager.enabled (the default), two or
#              more schedulable workers, the stock python image pullable and openssl on this machine.
#              NO GPU, no engine, no InstanceType: the consumers are bare Pods. The real-Hub row
#              needs the public Hugging Face Hub from the nodes and is SKIPPED with
#              E2E_C101_OFFLINE=1.
# Inputs:      MOCKED: a test hub (_model-hub.py) in <NS> standing in for the Hugging Face Hub,
#              whose repositories are generated from their names, so every digest is known; a second
#              hub over HTTPS with a self-signed certificate made here, handed to the plugin and the
#              controller through the model-artifact-ca-bundle Setting; a forward proxy; a random
#              token for the hub's private repository, created here and never printed. Real: the
#              plugin, kubelet, admission, the webhooks, the kubelet-issued tokens, the plugin's
#              downloads, and hf-internal-testing/tiny-random-bert on the real Hub, filtered to its
#              JSON, text and safetensors files (about 0.5 MB of its 27 MB).
# Expected:    - cold: both Pods Running, each file fetched once, the cold mount outlasting 120 s,
#                every file's SHA-256 equal to the hub's;
#              - warm: a third Pod Running, no new request at the hub, the node's download counter
#                unchanged;
#              - corrupt: the node lists the digest Failed with IntegrityMismatch and the Pod never
#                runs;
#              - killed plugin: no sample shows the Pod's volumes mounted before the node lists the
#                digest Ready, and the Pod runs once it does, on the manifest's bytes;
#              - restricted: the consumers run in a namespace enforcing restricted, where a hostPath
#                Pod is refused;
#              - forged: another namespace's artifact, its own artifact with another digest, and a
#                forged pod.namespace attribute are each refused with their rule, beside a Pod of the
#                same namespace that mounts;
#              - guards: the plugin's token writes only its own node's status, a token bound to no
#                Pod and the cluster administrator are refused, and the worker alone writes a
#                ModelArtifact's status;
#              - a Setting change reaches every node's spec and every plugin applies it;
#              - without the CSIDriver Node is refused and every NodeModelStore is removed; with it
#                back Node is accepted and every NodeModelStore returns;
#              - through the HTTPS proxy every file the hub serves is fetched from the proxy's
#                address, and without it from the plugin's; equal addresses make that row undecidable;
#              - real Hub: a Pod runs on the real repository and its model.safetensors hashes as this
#                machine's own download of it at the resolved commit;
#              - the token is in no log, event, status or metric, after a planted copy is found.
# Cleanup:     Trap deletes the Pods, artifacts, Secrets, the second namespace, the hubs, the proxy
#              and the CA ConfigMap, puts back the CSIDriver and the Settings it changed, and
#              restores the namespace PSA labels it changed.
set -uo pipefail

# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"
CASES_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "${CASES_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASES_DIR}/_model-hub-lib.sh"

NS="${1:?usage: case-101.sh <NS>}"
if [ "$NS" = "$SYSTEM_NS" ]; then
  echo "[case-101] refusing system namespace ${SYSTEM_NS}; usage: case-101.sh <consumer-NS>" >&2
  exit 2
fi
NSB="${NS}-b"
P=c101
TLS_HUB="${P}-tlshub"
PSA_SNAPSHOT=0
SCRATCH="$(mktemp -d)"
TOKEN="hf_$(python3 -c 'import secrets; print(secrets.token_hex(16))')"
SA="${E2E_MODEL_MANAGER_SA:-gpustack-operator-model-manager}"
# Remember what the store carried before the case touched it, so the cleanup puts every key back.
settings_save model-artifact-huggingface-endpoint model-artifact-https-proxy \
  model-artifact-no-proxy model-artifact-ca-bundle model-store-download-concurrency \
  model-artifact-delivery-mode

cleanup() {
  local labels
  echo
  echo "[case-101] cleanup"
  for ns in "$NS" "$NSB"; do
    kubectl -n "$ns" delete pods -l e2e.gpustack.ai/consumer=true --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1
    kubectl -n "$ns" delete modelartifacts.worker.gpustack.ai -l e2e.gpustack.ai/case=101 --ignore-not-found >/dev/null 2>&1
  done
  kubectl -n "$NS" delete secret "${P}-token" --ignore-not-found >/dev/null 2>&1
  for n in "${P}-hub" "$TLS_HUB" "${P}-proxy"; do
    kubectl -n "$NS" delete deploy,svc "$n" --ignore-not-found >/dev/null 2>&1
    kubectl -n "$NS" delete configmap "${n}-script" --ignore-not-found >/dev/null 2>&1
  done
  kubectl -n "$NS" delete secret "${P}-tls" --ignore-not-found >/dev/null 2>&1
  kubectl -n "$SYSTEM_NS" delete configmap "${P}-ca" --ignore-not-found >/dev/null 2>&1
  kubectl delete namespace "$NSB" --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl get csidriver model.csi.gpustack.ai >/dev/null 2>&1 || kubectl apply -f "$SCRATCH/csidriver.yaml" >/dev/null 2>&1
  settings_restore
  rm -rf "$SCRATCH"
  if [ "$PSA_SNAPSHOT" = 1 ]; then
    labels=(pod-security.kubernetes.io/enforce- pod-security.kubernetes.io/enforce-version-)
    [ -n "$ORIG_ENFORCE" ] && labels[0]="pod-security.kubernetes.io/enforce=${ORIG_ENFORCE}"
    [ -n "$ORIG_ENFORCE_VERSION" ] && labels[1]="pod-security.kubernetes.io/enforce-version=${ORIG_ENFORCE_VERSION}"
    kubectl label namespace "$NS" --overwrite "${labels[@]}" >/dev/null || {
      echo "[case-101] failed to restore PSA labels on namespace ${NS}" >&2
      exit 1
    }
  fi
}
trap cleanup EXIT

# get_uid NS NAME
get_uid() { kubectl -n "$1" get modelartifacts.worker.gpustack.ai "$2" -o jsonpath='{.metadata.uid}'; }
# hub_gets REPO: the number of downloads the hub answered for REPO, by path, as "path count" lines.
hub_gets() {
  mh_log "$NS" "${P}-hub" | python3 -c "
import collections, json, sys
c = collections.Counter(json.loads(l)['path'] for l in sys.stdin if l.strip()
                        and json.loads(l).get('status') in (200, 206) and json.loads(l)['method'] == 'GET'
                        and json.loads(l)['path'].startswith('/$1/'))
for p, n in sorted(c.items()): print(p.rsplit('/', 1)[-1], n)"
}
# files_match NS POD REPO: whether every file the Pod lists hashes to the hub's, and the hub lists no
# file the Pod lacks.
files_match() {
  local listed want
  listed="$(kubectl -n "$1" logs "$2" 2>/dev/null | sort)"
  want="$(mh_call "$NS" "${P}-hub" GET /_manifest | python3 -c "
import json, sys
for p, f in sorted(json.load(sys.stdin)['$3']['files'].items()): print(f['sha256'], p)" | sort)"
  [ -n "$want" ] && [ "$listed" = "$want" ]
}

read -r -a WORKERS <<<"$(model_workers)"
[ "${#WORKERS[@]}" -ge 2 ] || { echo "[case-101] needs two workers, found ${#WORKERS[@]}; NOTHING WAS VERIFIED"; exit 2; }
W1="${WORKERS[0]}" W2="${WORKERS[1]}"
if ! kubectl get csidriver model.csi.gpustack.ai >/dev/null 2>&1; then
  echo "[case-101] the model-manager plugin is not installed; NOTHING WAS VERIFIED"
  exit 2
fi
# The object to put back: its name, labels, annotations and spec, without what the API server set.
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
echo "[case-101] hub at ${HUB_URL}; workers ${W1}, ${W2}"

# ---------------------------------------------------------------- cold
mh_call "$NS" "${P}-hub" POST "/_control?throttle=40960" >/dev/null
artifact "$NS" "${P}-cold" e2e/cold "" 101
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
    gets="$(hub_gets e2e/cold)"
    if echo "$gets" | awk '$2 != 1 {bad=1} END {exit bad}' && [ "$(echo "$gets" | wc -l | tr -d ' ')" = 3 ]; then
      record PASS "each of 3 files was fetched once for two Pods" "e2e/cold: $(echo "$gets" | tr '\n' ' ')"
    else
      record FAIL "each file was fetched once" "e2e/cold: $(echo "$gets" | tr '\n' ' ')"
    fi
    if files_match "$NS" "${P}-p1" e2e/cold && files_match "$NS" "${P}-p2" e2e/cold; then
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

  # ------------------------------------------------------------ warm
  mh_call "$NS" "${P}-hub" POST "/_control?throttle=0" >/dev/null
  before_gets="$(hub_gets e2e/cold)"
  before_bytes="$(plugin_metric "$W1" gpustack_model_manager_download_bytes_total)"
  consumer "$NS" "${P}-p3" "$W1" "${P}-cold" "$UID_COLD" "$DIGEST"
  if pod_ready "$NS" "${P}-p3" 120; then
    # The counter must have counted the cold download, or an unreadable one reads 0 twice and
    # passes whatever the node did.
    after_bytes="$(plugin_metric "$W1" gpustack_model_manager_download_bytes_total)"
    if [ "${before_bytes:-0}" -gt 0 ] && [ "$(hub_gets e2e/cold)" = "$before_gets" ] && [ "$after_bytes" = "$before_bytes" ]; then
      record PASS "a third Pod runs with no request and no byte downloaded" "${P}-p3 (${before_bytes} bytes before and after)"
    else
      record FAIL "a third Pod downloads nothing" "${P}-p3 (bytes ${before_bytes:-<none>} before, ${after_bytes:-<none>} after)"
    fi
  else
    record FAIL "a third Pod on the warm node runs" "${P}-p3"
  fi

  # ------------------------------------------------------------ restricted, with its control
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

  # ------------------------------------------------------------ forged attributes
  artifact "$NSB" "${P}-mine" e2e/other "" 101
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

# ---------------------------------------------------------------- corrupt
mh_call "$NS" "${P}-hub" POST "/_control?corrupt=e2e/corrupt" >/dev/null
artifact "$NS" "${P}-corrupt" e2e/corrupt "" 101
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

# ---------------------------------------------------------------- killed mid-download
mh_call "$NS" "${P}-hub" POST "/_control?throttle=40960" >/dev/null
artifact "$NS" "${P}-restart" e2e/restart "" 101
RD="$(wait_resolved "$NS" "${P}-restart" 120)"
consumer "$NS" "${P}-pk" "$W2" "${P}-restart" "$(get_uid "$NS" "${P}-restart")" "$RD"
for _ in $(seq 1 30); do [ -n "$(hub_gets e2e/restart)" ] && break; sleep 2; done
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
if pod_ready "$NS" "${P}-pk" 300 && files_match "$NS" "${P}-pk" e2e/restart; then
  record PASS "after the restart the Pod runs on the manifest's bytes" "${NS}/${P}-pk"
else
  record FAIL "after the restart the Pod runs on the manifest's bytes" "$(pod_mount_events "$NS" "${P}-pk" | tail -1)"
fi

# ---------------------------------------------------------------- the token
printf '%s' "$TOKEN" | kubectl -n "$NS" create secret generic "${P}-token" --from-file=token=/dev/stdin >/dev/null
artifact "$NS" "${P}-private" e2e/private "${P}-token" 101
PD="$(wait_resolved "$NS" "${P}-private" 120)"
consumer "$NS" "${P}-pp" "$W2" "${P}-private" "$(get_uid "$NS" "${P}-private")" "$PD" "${P}-token"
if pod_ready "$NS" "${P}-pp" 240; then
  record PASS "a private repository mounts with the Secret kubelet hands over" "${NS}/${P}-pp"
else
  record FAIL "a private repository mounts" "$(pod_mount_events "$NS" "${P}-pp" | tail -1)"
fi
scan() { # everything the token must never reach
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
  record PASS "the token is in no log, event, status or metric ($(wc -c <"$SCRATCH/scan") bytes scanned; the planted copy is found)" "all"
else
  record FAIL "the token is in no output ($(grep -c -F "$TOKEN" "$SCRATCH/scan") matches)" "all"
fi

# ---------------------------------------------------------------- the NodeModelStore status guard
SERVER="$(kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.server}')"
kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d >"$SCRATCH/ca.crt"
# status_as TOKEN NODE: an update of NODE's NodeModelStore status as the bearer of TOKEN alone, the
# one verb the plugin's role holds on status and the one it sends, printing the API server's answer
# and succeeding on a 2xx. The object is read as the administrator, with a changed usedPercent, and
# put back as TOKEN.
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

# ---------------------------------------------------------------- the ModelArtifact status guard
artifact "$NS" "${P}-guard" e2e/guard "" 101
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

# ---------------------------------------------------------------- a Setting reaches every node
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
# The restore puts back every key the case saved, the endpoint among them, so the endpoint
# carries its own value again until the case points it at the TLS hub.
settings_restore

# ---------------------------------------------------------------- Node delivery needs the CSIDriver
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

# ---------------------------------------------------------------- an HTTPS proxy, with its control
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

# remotes REPO: the distinct client addresses the hub served REPO's file downloads to.
remotes() {
  mh_log "$NS" "$TLS_HUB" | python3 -c "
import json, sys
print(' '.join(sorted({json.loads(l)['remote'] for l in sys.stdin if l.strip() and json.loads(l)['method'] == 'GET'
                        and json.loads(l)['path'].startswith('/$1/') and json.loads(l).get('status') in (200, 206)})))"
}
fetch() { # name repo pod
  artifact "$NS" "$1" "$2" "" 101
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
  else
    if [ "$direct" = "$PLUGIN_IP" ]; then
      record PASS "without the proxy the files reached the hub from the plugin (${direct}), so the address tells the paths apart" "e2e/direct"
    else
      record FAIL "without the proxy the files reached the hub from the plugin ${PLUGIN_IP} (saw ${direct})" "e2e/direct"
    fi
  fi
else
  record FAIL "a cold download without the proxy mounts" "$(pod_mount_events "$NS" "${P}-pd" | tail -1)"
fi

# ---------------------------------------------------------------- the real Hub
if [ "${E2E_C101_OFFLINE:-0}" = 1 ]; then
  record SKIP "the real Hub (E2E_C101_OFFLINE=1): NOTHING WAS VERIFIED on it" "hf-internal-testing/tiny-random-bert"
else
  # The test CA bundle and the proxy would stand between the plugin and the real Hub, so both go
  # before the endpoint moves.
  setting_unset model-artifact-ca-bundle
  setting_set model-artifact-huggingface-endpoint https://huggingface.co
  settings_settle
  REAL=hf-internal-testing/tiny-random-bert
  # The repository also holds a 26 MB TensorFlow file; the filter keeps the download to about 0.5 MB.
  artifact "$NS" "${P}-real" "$REAL" "" 101 "
  allowPatterns: [\"*.json\", \"*.txt\", \"model.safetensors\"]"
  RED="$(wait_resolved "$NS" "${P}-real" 180)"
  consumer "$NS" "${P}-pr" "$W2" "${P}-real" "$(get_uid "$NS" "${P}-real")" "$RED"
  commit="$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-real" -o jsonpath='{.status.resolved.revision}')"
  # The independent reading is this machine's own download at the commit: the file is a plain git
  # blob in this repository, so the tree carries no SHA-256 for it.
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

print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-101] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-101] PASS"
