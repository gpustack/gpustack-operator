#!/usr/bin/env bash
#
# CASE 101 — The node plugin delivers a Hugging Face artifact: one verified download per node, zero
#            bytes for the next Pod, never a corrupted or partial tree, and a mount only for the
#            Pod's own namespace (MUTATING, self-recovering; AUTO-SKIPS the real-Hub row with
#            E2E_C101_OFFLINE=1)
#
#   case-101.sh <NS>
#
# Goal:        Prove node delivery end to end with bare Pods, which is what a tenant can write by
#              hand: a cold mount lasting longer than one kubelet mount call downloads each file once
#              for two Pods and serves the manifest's bytes; a third Pod on the node downloads
#              nothing; a corrupting source is never published; a plugin killed mid-download never
#              exposes a partial tree; a namespace enforcing restricted mounts; forged volume
#              attributes are refused by the rule they break; the real Hub serves through its
#              redirects; and no token reaches any output.
# Environment: A cluster installed from this chart with modelManager.enabled (the default), two or
#              more schedulable workers, and the stock python image pullable. NO GPU, no engine, no
#              InstanceType: the consumers are bare Pods. The real-Hub row needs the public Hugging
#              Face Hub from the nodes and is SKIPPED with E2E_C101_OFFLINE=1.
# Inputs:      MOCKED: a test hub (_model-hub.py) in <NS> standing in for the Hugging Face Hub,
#              whose repositories are generated from their names, so every digest is known; a random
#              token for its private repository, created here and never printed. Real: the plugin,
#              kubelet, admission, and hf-internal-testing/tiny-random-bert on the real Hub,
#              filtered to its JSON, text and safetensors files (about 0.5 MB of its 27 MB).
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
#              - real Hub: a Pod runs on the real repository and its model.safetensors hashes as this
#                machine's own download of it at the resolved commit;
#              - the token is in no log, event, status or metric, after a planted copy is found.
# Cleanup:     Trap deletes the Pods, artifacts, Secrets, the second namespace and the hub, and puts
#              back the Settings it changed.
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"
CASES_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "${CASES_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASES_DIR}/_model-hub-lib.sh"

NS="${1:?usage: case-101.sh <NS>}"
NSB="${NS}-b"
P=c101
FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
SCRATCH="$(mktemp -d)"
TOKEN="hf_$(python3 -c 'import secrets; print(secrets.token_hex(16))')"
ORIG_ENDPOINT="$(setting_get model-artifact-huggingface-endpoint)"
ORIG_DELIVERY="$(setting_get model-artifact-delivery-mode)"

cleanup() {
  echo
  echo "[case-101] cleanup"
  for ns in "$NS" "$NSB"; do
    kubectl -n "$ns" delete pods -l e2e.gpustack.ai/consumer=true --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1
    kubectl -n "$ns" delete modelartifacts.worker.gpustack.ai -l e2e.gpustack.ai/case=101 --ignore-not-found >/dev/null 2>&1
  done
  kubectl -n "$NS" delete secret "${P}-token" --ignore-not-found >/dev/null 2>&1
  kubectl -n "$NS" delete deploy,svc,configmap -l e2e.gpustack.ai/model-hub=true --ignore-not-found >/dev/null 2>&1
  kubectl -n "$NS" delete deploy,svc "${P}-hub" --ignore-not-found >/dev/null 2>&1
  kubectl -n "$NS" delete configmap "${P}-hub-script" --ignore-not-found >/dev/null 2>&1
  kubectl delete namespace "$NSB" --ignore-not-found --wait=false >/dev/null 2>&1
  if [ -n "$ORIG_ENDPOINT" ]; then setting_set model-artifact-huggingface-endpoint "$ORIG_ENDPOINT"; else setting_unset model-artifact-huggingface-endpoint; fi
  if [ -n "$ORIG_DELIVERY" ]; then setting_set model-artifact-delivery-mode "$ORIG_DELIVERY"; else setting_unset model-artifact-delivery-mode; fi
  rm -rf "$SCRATCH"
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

for ns in "$NS" "$NSB"; do
  kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
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
    kubectl get --raw "/api/v1/namespaces/${SYSTEM_NS}/pods/https:$(plugin_pod "$w"):32444/proxy/metrics" 2>/dev/null
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

# ---------------------------------------------------------------- the real Hub
if [ "${E2E_C101_OFFLINE:-0}" = 1 ]; then
  record SKIP "the real Hub (E2E_C101_OFFLINE=1): NOTHING WAS VERIFIED on it" "hf-internal-testing/tiny-random-bert"
else
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
