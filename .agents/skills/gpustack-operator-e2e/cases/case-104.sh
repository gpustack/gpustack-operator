#!/usr/bin/env bash
#
# CASE 104 — Serving from the node cache: vLLM serves a 7B model its node downloaded once, and the
#            replica that replaces it starts from the published tree with zero bytes downloaded
#            (MUTATING, self-recovering; needs a real accelerator)
#
#   case-104.sh <NS>
#
# Goal:        Prove Node delivery at the size it exists for: a ModelDeployment on a Hugging Face
#              artifact of about 15 GB renders the plugin's volume, its node downloads and verifies
#              the files once and publishes them, vLLM loads the read-only tree and serves under
#              spec.model.name, and a replacement replica on the same node mounts the published tree
#              without a single byte downloaded. The download's wall time and the publication time
#              are recorded, because nothing else measures them at this size.
# Environment: A cluster installed from this chart with modelManager.enabled and
#              model-artifact-delivery-mode Node, an accelerator InstanceType (E2E_C104_IT) whose
#              node has room for the model twice over in the cache filesystem, nodes that reach the
#              Hub, and in <NS> the pool's entrance LocalQueue. One accelerator is enough: the
#              second replica is the replacement of the first, on the same node.
#              E2E_PD_PROBE_IMAGE (default curlimages/curl:8.11.1) sends the requests.
# Inputs:      All real. Public E2E_C104_MODEL (default Qwen/Qwen2.5-7B-Instruct) at main; vLLM
#              0.29.0, its runner image synthesized from the InstanceType. E2E_C104_READY_BOUND
#              (default 3600 s) bounds the first start, download included.
# Expected:    - the deployment reports status.model.delivery Node and its Pod carries the plugin's
#                volume with the artifact's digest, and no engine download cache;
#              - WeightsReady goes through Materializing to Mounted, and the node lists the digest
#                Ready with its size;
#              - the deployment goes Ready; /v1/models lists exactly spec.model.name; a chat request
#                answers 200 under it;
#              - the node's download counter grew by exactly the artifact's size (every byte came
#                once) and the plugin logged the publication with its duration;
#              - after the replica's Pod is deleted, its replacement lands on the same node, the
#                download counter does not move, a mount hit is counted, and the replacement goes
#                Ready and answers.
# Cleanup:     Trap deletes the deployment, the artifact and the probe Pod. The published tree stays
#              in the node's cache; the collector removes it once it is unreferenced and needed.
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"
CASES_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "${CASES_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASES_DIR}/_model-hub-lib.sh"

NS="${1:?usage: case-104.sh <NS>}"
IT="${E2E_C104_IT:?set E2E_C104_IT to an accelerator InstanceType}"
MODEL="${E2E_C104_MODEL:-Qwen/Qwen2.5-7B-Instruct}"
PROBE_IMAGE="${E2E_PD_PROBE_IMAGE:-curlimages/curl:8.11.1}"
READY_BOUND="${E2E_C104_READY_BOUND:-3600}"
P=c104
SERVED=e2e/c104
LABEL="e2e.gpustack.ai/case=104"

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

cleanup() {
  echo
  echo "[case-104] cleanup"
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "${P}-md" --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl -n "$NS" delete pod "${P}-probe" --ignore-not-found --wait=false >/dev/null 2>&1
  for _ in $(seq 1 36); do
    [ -z "$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${P}-md" -o name 2>/dev/null)" ] && break
    sleep 5
  done
  kubectl -n "$NS" delete modelartifacts.worker.gpustack.ai -l "$LABEL" --ignore-not-found --wait=false >/dev/null 2>&1
}
trap cleanup EXIT

[ "$(setting_get model-artifact-delivery-mode)" = Node ] \
  || { echo "[case-104] model-artifact-delivery-mode is not Node; NOTHING WAS VERIFIED"; exit 2; }

pods_json() { kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${P}-md" -o json 2>/dev/null; }
running_pod() { # the Pod not being deleted, "name node"
  pods_json | python3 -c "
import json, sys
for p in json.load(sys.stdin)['items']:
    if not p['metadata'].get('deletionTimestamp'):
        print(p['metadata']['name'], p['spec'].get('nodeName', '')); break"
}
wait_phase() { # want bound -> phase
  local phase=""
  for _ in $(seq 1 $(( $2 / 10 ))); do
    phase="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "${P}-md" -o jsonpath='{.status.phase}' 2>/dev/null)"
    [ "$phase" = "$1" ] && break
    sleep 10
  done
  printf '%s' "$phase"
}
# hits NODE: the plugin's mount hits on NODE, the one labeled series of mounts_total.
hits() {
  kubectl get --raw "/api/v1/namespaces/${SYSTEM_NS}/pods/https:$(plugin_pod "$1"):32444/proxy/metrics" 2>/dev/null \
    | awk '$1 == "gpustack_model_manager_mounts_total{result=\"hit\"}" {printf "%d", $2}'
}
probe() { kubectl -n "$NS" exec "${P}-probe" -- curl -sS -m 120 "$@" 2>/dev/null; }
serves() { # -> "models|answer"
  local endpoint models answer
  endpoint="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "${P}-md" -o jsonpath='{.status.endpoint}')"
  models="$(probe "${endpoint}/v1/models" | python3 -c 'import json,sys;print(",".join(m["id"] for m in json.load(sys.stdin)["data"]))' 2>/dev/null)"
  answer="$(probe -H 'Content-Type: application/json' "${endpoint}/v1/chat/completions" \
    -d "{\"model\":\"${SERVED}\",\"max_tokens\":8,\"messages\":[{\"role\":\"user\",\"content\":\"Say hello.\"}]}" \
    | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d.get("model","") if d["choices"][0]["message"]["content"] else "")' 2>/dev/null)"
  printf '%s|%s' "$models" "$answer"
}

echo "== fixtures: a probe and the artifact =="
kubectl -n "$NS" run "${P}-probe" --image="$PROBE_IMAGE" --labels="$LABEL" --restart=Never --command -- sleep 14400 >/dev/null
kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata: {name: ${P}-model, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "104"}}
spec: {source: {huggingFace: {repository: "${MODEL}", revision: main}}}
YAML
DIGEST="$(wait_resolved "$NS" "${P}-model" 180)"
SIZE="$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-model" -o jsonpath='{.status.resolved.sizeBytes}')"
if [ -z "$DIGEST" ]; then
  record FAIL "the artifact resolves" "${MODEL}"
  print_rows
  exit 1
fi
record PASS "the artifact resolves" "${MODEL} ${DIGEST:0:19} ${SIZE} bytes"

echo "== 1. the first replica downloads, publishes and serves =="
# Every node's download counter before the deployment exists: the plugin starts downloading as soon
# as the Pod is placed, before this case can see where, so a reading taken after placement misses
# the first bytes.
for n in $(kubectl get nodes -o jsonpath='{.items[*].metadata.name}'); do
  printf '%s %s\n' "$n" "$(plugin_metric "$n" gpustack_model_manager_download_bytes_total)"
done >"${TMPDIR:-/tmp}/${P}-bytes.$$"
kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata: {name: ${P}-md, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "104"}}
spec:
  engine: {name: vllm, version: "0.29.0"}
  model: {name: ${SERVED}, artifactRef: {name: ${P}-model}}
  roles:
    - {name: server, instanceType: "${IT}", replicas: 1, resources: {accelerator: 1}, extraArgs: ["--max-model-len", "4096", "--gpu-memory-utilization", "0.85"]}
YAML
START="$(date +%s)"
pod="" node=""
for _ in $(seq 1 90); do
  read -r pod node <<<"$(running_pod)"
  [ -n "$node" ] && break
  sleep 10
done
if [ -z "$node" ]; then
  record FAIL "the replica is placed" "no Pod with a node within 900s"
  print_rows
  exit 1
fi
BYTES_BEFORE="$(awk -v n="$node" '$1 == n {print $2}' "${TMPDIR:-/tmp}/${P}-bytes.$$")"
rm -f "${TMPDIR:-/tmp}/${P}-bytes.$$"
vol="$(kubectl -n "$NS" get pod "$pod" -o jsonpath="{.spec.volumes[?(@.csi.driver=='model.csi.gpustack.ai')].csi.volumeAttributes.manifestDigest}|{.spec.volumes[?(@.name=='gpustack-model-cache')].name}")"
delivery="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "${P}-md" -o jsonpath='{.status.model.delivery}')"
if [ "$vol" = "${DIGEST}|" ] && [ "$delivery" = Node ]; then
  record PASS "the replica carries the plugin's volume with the digest and no engine cache; delivery Node" "${pod} on ${node}"
else
  record FAIL "the replica carries the plugin's volume with the digest" "volume=${vol} delivery=${delivery}"
fi

seen="" reading="" published_at=""
for _ in $(seq 1 $(( READY_BOUND / 10 ))); do
  reading="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "${P}-md" \
    -o jsonpath="{.status.conditions[?(@.type=='WeightsReady')].status}|{.status.conditions[?(@.type=='WeightsReady')].reason}" 2>/dev/null)"
  # The reasons seen, each once, matched as whole words: WeightsNotMounted contains Mounted.
  [ -n "$reading" ] && case "${seen} " in *" ${reading#*|} "*) ;; *) seen="${seen} ${reading#*|}" ;; esac
  [ -z "$published_at" ] && [ "$(store_state "$node" "$DIGEST")" = "Ready|" ] && published_at="$(date +%s)"
  [ "$reading" = "True|Mounted" ] && break
  sleep 10
done
MOUNTED_AT="$(date +%s)"
case "${seen} " in
  *" Materializing "*)
    if [ "$reading" = "True|Mounted" ]; then
      record PASS "WeightsReady went through Materializing to Mounted" "reasons:${seen}"
    else
      record FAIL "WeightsReady reaches Mounted" "last=${reading} reasons:${seen}"
    fi ;;
  *) record FAIL "WeightsReady reports Materializing while the node downloads" "reasons:${seen} last=${reading}" ;;
esac

stored="$(kubectl get nodemodelstores.worker.gpustack.ai "$node" -o jsonpath="{range .status.models[?(@.digest=='${DIGEST}')]}{.state}|{.sizeBytes}{end}")"
BYTES_AFTER="$(plugin_metric "$node" gpustack_model_manager_download_bytes_total)"
grown=$(( ${BYTES_AFTER:-0} - ${BYTES_BEFORE:-0} ))
pub_log="$(kubectl -n "$SYSTEM_NS" logs "$(plugin_pod "$node")" -c main --tail=-1 2>/dev/null \
  | grep '"published"' | grep "${DIGEST#sha256:}" | tail -1 | grep -o 'seconds=[0-9.]*')"
if [ "$stored" = "Ready|${SIZE}" ] && [ "$grown" = "${SIZE:-x}" ] && [ -n "$pub_log" ]; then
  record PASS "the node published the digest once: ${grown} bytes downloaded for ${SIZE}, ${pub_log} from start to publication" "$node"
else
  record FAIL "the node published the digest" "store=${stored:-<none>} downloaded=${grown} size=${SIZE} log=${pub_log:-<none>}"
fi
record INFO "wall time: ${P}-md created to the digest Ready $(( ${published_at:-$MOUNTED_AT} - START ))s, to WeightsReady Mounted $(( MOUNTED_AT - START ))s" "$node"

phase="$(wait_phase Ready "$READY_BOUND")"
READY_AT="$(date +%s)"
if [ "$phase" = Ready ]; then
  IFS='|' read -r models answer <<<"$(serves)"
  if [ "$models" = "$SERVED" ] && [ "$answer" = "$SERVED" ]; then
    record PASS "vLLM serves the node's tree under spec.model.name and answers a chat request" "ready $(( READY_AT - START ))s after creation"
  else
    record FAIL "vLLM serves under spec.model.name" "models=${models:-<none>} answer=${answer:-<none>}"
  fi
else
  record FAIL "the deployment goes Ready" "phase=${phase:-<none>}"
fi

echo "== 2. the replacement starts from the published tree =="
HITS_BEFORE="$(hits "$node")"
BYTES_BEFORE="$(plugin_metric "$node" gpustack_model_manager_download_bytes_total)"
kubectl -n "$NS" delete pod "$pod" --wait=false >/dev/null
R_START="$(date +%s)"
new="" new_node=""
for _ in $(seq 1 90); do
  read -r new new_node <<<"$(running_pod)"
  [ -n "$new" ] && [ "$new" != "$pod" ] && [ -n "$new_node" ] && break
  new=""
  sleep 5
done
sleep 20
phase="$(wait_phase Ready "$READY_BOUND")"
R_READY="$(date +%s)"
HITS_AFTER="$(hits "$node")"
BYTES_AFTER="$(plugin_metric "$node" gpustack_model_manager_download_bytes_total)"
if [ "$new_node" = "$node" ] && [ "$BYTES_AFTER" = "$BYTES_BEFORE" ] && [ "${HITS_AFTER:-0}" -gt "${HITS_BEFORE:-0}" ]; then
  record PASS "the replacement ${new} mounted on ${node} with zero bytes downloaded and a counted hit" "bytes ${BYTES_BEFORE} before and after"
else
  record FAIL "the replacement mounts from the published tree" "node=${new_node:-<none>} bytes ${BYTES_BEFORE}->${BYTES_AFTER} hits ${HITS_BEFORE}->${HITS_AFTER}"
fi
if [ "$phase" = Ready ]; then
  IFS='|' read -r models answer <<<"$(serves)"
  if [ "$models" = "$SERVED" ] && [ "$answer" = "$SERVED" ]; then
    record PASS "the replacement serves and answers" "ready $(( R_READY - R_START ))s after the delete"
  else
    record FAIL "the replacement serves" "models=${models:-<none>} answer=${answer:-<none>}"
  fi
else
  record FAIL "the replacement goes Ready" "phase=${phase:-<none>}"
fi

print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-104] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-104] PASS"
