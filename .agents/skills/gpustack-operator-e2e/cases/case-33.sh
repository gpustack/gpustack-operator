#!/usr/bin/env bash
#
# CASE 33 — Live ModelArtifact serving: PVC claim + gated commit download, node-cache 7B
#           zero-byte warm restart, and cache_prefix KV weight isolation
#   (MUTATING, self-cleaning; requires real accelerators; each phase auto-skips if its
#    specific prerequisite such as HF_TOKEN_READONLY, E2E_C33_MEMBER_NODE, or Node
#    delivery mode is absent)
#
#   case-33.sh <NS>
#
# Prerequisites: nodes=1; gpu=any; features=mooncake-store; cluster=real; inputs=E2E_C33_AT,E2E_C33_IT,E2E_C33_GATED,E2E_C33_GATED_COMMIT,E2E_C33_MEMBER_NODE,E2E_C33_NODE_MODEL,E2E_C33_VLLM_IMAGE,E2E_C33_SGLANG_IMAGE,E2E_C33_READY_BOUND,E2E_C33_NODE_READY_BOUND,E2E_C33_ENGINE_URL,E2E_PD_PROBE_IMAGE,HF_TOKEN_READONLY
# Goal:        Verify live ModelArtifact serving across all three delivery and isolation modes:
#                1. Claim + gated Hub serving: vLLM serves a PVC ModelArtifact behind llm-d-router
#                   under spec.model.name with zero router tokenization failures, and SGLang
#                   downloads a gated Hugging Face ModelArtifact pinned to its resolved commit
#                   (tokenizer hash matches pinned commit, not main) within its cache sizeLimit.
#                2. Node-cache CSI 7B serving & warm restart (when model-artifact-delivery-mode=Node):
#                   downloads and verifies a 7B artifact once (WeightsReady Materializing->Mounted,
#                   download_bytes_total == sizeBytes), serves under spec.model.name, and after Pod
#                   deletion the replacement mounts on the same node with zero bytes downloaded and
#                   a counted mount hit.
#                3. KV store cache_prefix weight isolation: two commits of SmolLM-135M-Instruct on
#                   the same KVCachePoolBinding render distinct cache_prefix values; same-artifact
#                   deployment gets external prefix cache hits (with must-miss control), while the
#                   other commit gets zero hits.
# Environment: A real accelerator cluster with a usable AcceleratorType (E2E_C33_AT / E2E_C33_IT,
#              or auto-discovered), Hub reachability, and the pool's entrance LocalQueue in <NS>.
#              Phase 1 gated sub-leg requires HF_TOKEN_READONLY; Phase 2 requires
#              model-artifact-delivery-mode=Node and modelManager.enabled; Phase 3 requires
#              E2E_C33_MEMBER_NODE (node hostname with 4Gi spare DRAM for Mooncake).
# Inputs:      REAL: Hugging Face repositories (Qwen2.5-0.5B-Instruct, gated repo, Qwen2.5-7B-Instruct,
#              SmolLM-135M-Instruct commits A & B), real vLLM/SGLang runner images, Mooncake store,
#              and curl probe Pod.
# Expected:    Every enabled phase completes its live serving, warm-restart, and cache_prefix
#              assertions cleanly; phases whose environment-specific inputs are unset record SKIP.
# Cleanup:     EXIT trap deletes all ModelDeployments, probe Pods, Jobs, ModelArtifacts, PVCs,
#              Secrets, KVCachePoolBindings, and the cluster-scoped KVCachePool/KVCacheBackend.
set -uo pipefail

CASE_ID=33
export CASE_ID
CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=/dev/null
. "${CASE_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_md-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_model-hub-lib.sh"

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-33.sh <NS>" >&2
  exit 2
fi

IT="${E2E_C33_AT:-${E2E_C33_IT:-}}"
if [ -z "$IT" ]; then
  for m in nvidia amd ascend; do
    IT="$(md_usable_type_for "$m")"
    [ -n "$IT" ] && break
  done
fi
if [ -z "$IT" ]; then
  echo "[case-33] set E2E_C33_AT or E2E_C33_IT to a real accelerator type" >&2
  exit 2
fi

GATED="${E2E_C33_GATED:-XyX824/mam-c-gated-tiny}"
GATED_COMMIT="${E2E_C33_GATED_COMMIT:-849d87054adea51b3cc2e81b95ed17b1289e9a40}"
NODE_MODEL="${E2E_C33_NODE_MODEL:-Qwen/Qwen2.5-7B-Instruct}"
MEMBER_NODE="${E2E_C33_MEMBER_NODE:-}"
PROBE_IMAGE="${E2E_PD_PROBE_IMAGE:-curlimages/curl:8.11.1}"
READY_BOUND="${E2E_C33_READY_BOUND:-1200}"
NODE_READY_BOUND="${E2E_C33_NODE_READY_BOUND:-3600}"
VLLM_IMAGE="${E2E_C33_VLLM_IMAGE:-quay.io/gpustack/runner:cuda12.9-vllm0.29.0}"
SGLANG_IMAGE="${E2E_C33_SGLANG_IMAGE:-quay.io/gpustack/runner:cuda12.9-sglang0.5.18}"
ENGINE_URL="${E2E_C33_ENGINE_URL:-http://127.0.0.1:8000}"

COMMIT_A="0a0a7c2a1b1dc8f75f1d5a6ac86d38e3e7bab014"
COMMIT_B="fcc320f490e08fdb4b99d935b2c58d40bf35b0d0"
P="e2e-c33"
LABEL="e2e.gpustack.ai/case=33"
HUB="https://huggingface.co"
BYTES_FILE="${TMPDIR:-/tmp}/${P}-bytes.$$"

cleanup() {
  echo
  echo "[case-33] cleanup"
  rm -f "$BYTES_FILE"
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai -l "$LABEL" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n "$NS" delete pod "${P}-probe" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  for _ in $(seq 1 36); do
    [ -z "$(kubectl -n "$NS" get pods -o name 2>/dev/null | command grep "${P}-" || true)" ] && break
    sleep 5
  done
  kubectl -n "$NS" delete job,modelartifacts.worker.gpustack.ai,kvcachepoolbindings.worker.gpustack.ai,pvc,secret \
    -l "$LABEL" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete kvcachepools.worker.gpustack.ai "${P}-kvcp" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete kvcachebackends.worker.gpustack.ai "${P}-kvcb" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT

wait_md_ready() {
  local md="$1" bound="$2" phase=""
  for _ in $(seq 1 $(( bound / 10 ))); do
    phase="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "$md" -o jsonpath='{.status.phase}' 2>/dev/null)"
    [ "$phase" = Ready ] && break
    sleep 10
  done
  printf '%s' "$phase"
}

leader_pod() {
  kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${1},app.kubernetes.io/component=server" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}
in_pod() { local pod="$1"; shift; kubectl -n "$NS" exec "$pod" -c main -- "$@" 2>/dev/null; }
probe_curl() { kubectl -n "$NS" exec "${P}-probe" -- curl -sS -m 120 "$@" 2>/dev/null; }

kubectl -n "$NS" run "${P}-probe" --image="$PROBE_IMAGE" --labels="$LABEL" --restart=Never --command -- sleep 14400 >/dev/null
kubectl -n "$NS" wait "pod/${P}-probe" --for=condition=Ready --timeout=120s >/dev/null || true

# =================================================================================================
# PHASE 1: Claim artifact serving (vLLM + llm-d-router) & gated commit download (SGLang)
# =================================================================================================

kubectl apply -f - >/dev/null <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: ${P}-models, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "33"}}
spec: {accessModes: [ReadWriteOnce], resources: {requests: {storage: 10Gi}}}
---
apiVersion: batch/v1
kind: Job
metadata: {name: ${P}-copy, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "33"}}
spec:
  backoffLimit: 3
  template:
    metadata: {labels: {e2e.gpustack.ai/case: "33"}}
    spec:
      restartPolicy: OnFailure
      containers:
        - name: copy
          image: python:3.12-slim
          command: [sh, -c, "pip install -q huggingface_hub && hf download Qwen/Qwen2.5-0.5B-Instruct --local-dir /models/qwen"]
          volumeMounts: [{name: models, mountPath: /models}]
      volumes: [{name: models, persistentVolumeClaim: {claimName: ${P}-models}}]
---
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata: {name: ${P}-claim, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "33"}}
spec: {source: {persistentVolumeClaim: {claimName: ${P}-models, path: qwen}}}
YAML

if kubectl -n "$NS" wait --for=condition=complete "job/${P}-copy" --timeout=900s >/dev/null 2>&1; then
  record PASS "PVC claim populated with Qwen2.5-0.5B-Instruct" "${P}-models/qwen"
  kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata: {name: ${P}-vllm, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "33"}}
spec:
  engine: {name: vLLM, image: "${VLLM_IMAGE}"}
  model: {name: e2e/c33-claim, artifactRef: {name: ${P}-claim}}
  router: {name: llm-d-router}
  roles:
    - {name: server, acceleratorType: "${IT}", replicas: 1, resources: {accelerator: 1}, extraArgs: ["--max-model-len", "4096", "--gpu-memory-utilization", "0.4"]}
YAML
  phase="$(wait_md_ready "${P}-vllm" "$READY_BOUND")"
  pod="$(leader_pod "${P}-vllm")"
  if [ "$phase" != Ready ]; then
    record FAIL "claim ModelDeployment reaches Ready" "phase=${phase:-<none>} after ${READY_BOUND}s"
  else
    record PASS "claim ModelDeployment reaches Ready" "pod=${pod}"
    direct="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "${P}-vllm" -o jsonpath='{.status.router.roles[0].endpoint}')"
    models="$(probe_curl "${direct}/v1/models" | python3 -c 'import json,sys;print(",".join(m["id"] for m in json.load(sys.stdin)["data"]))' 2>/dev/null)"
    labels="$(probe_curl "${direct}/metrics" | command grep -o 'model_name="[^"]*"' | sort -u | tr '\n' ' ')"
    if [ "$models" = e2e/c33-claim ] && [ "$labels" = 'model_name="e2e/c33-claim" ' ]; then
      record PASS "vLLM serves and labels metrics under spec.model.name" "models=${models} labels=${labels}"
    else
      record FAIL "vLLM serves and labels metrics under spec.model.name" "models=${models:-<none>} labels=${labels:-<none>}"
    fi
    endpoint="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "${P}-vllm" -o jsonpath='{.status.endpoint}')"
    answer="$(probe_curl -H 'Content-Type: application/json' "${endpoint}/v1/chat/completions" \
      -d '{"model":"e2e/c33-claim","max_tokens":8,"messages":[{"role":"user","content":"Say hello."}]}' \
      | python3 -c 'import json,sys;print(json.load(sys.stdin).get("model",""))' 2>/dev/null)"
    router_logs="$(for d in $(kubectl -n "$NS" get deploy -o name 2>/dev/null | command grep "/${P}-vllm"); do
        kubectl -n "$NS" logs "$d" --all-containers --tail=-1 2>/dev/null; done)"
    router_errors="$(printf '%s\n' "$router_logs" | command grep -cE 'tokenization failed|failed to prepare per request data' || true)"
    if [ "$answer" = e2e/c33-claim ] && [ -n "$router_logs" ] && [ "${router_errors:-0}" = 0 ]; then
      record PASS "routed claim chat succeeds with zero router tokenization errors" "endpoint=${endpoint}"
    else
      record FAIL "routed claim chat succeeds with zero router tokenization errors" "answer=${answer:-<none>} errors=${router_errors:-?}"
    fi
  fi
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "${P}-vllm" --ignore-not-found --wait=false >/dev/null 2>&1 || true
else
  record FAIL "PVC claim populated with Qwen2.5-0.5B-Instruct" "job/${P}-copy did not complete within 900s"
fi

# Gated commit download on SGLang.
if [ -z "${HF_TOKEN_READONLY:-}" ]; then
  record SKIP "SGLang gated commit ModelArtifact serving" "HF_TOKEN_READONLY is not set"
else
  printf '%s' "$HF_TOKEN_READONLY" | kubectl -n "$NS" create secret generic "${P}-token" --from-file=token=/dev/stdin --dry-run=client -o yaml \
    | kubectl label --local -f - "$LABEL" -o yaml | kubectl apply -f - >/dev/null
  kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata: {name: ${P}-gated, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "33"}}
spec: {source: {huggingFace: {repository: "${GATED}", revision: "${GATED_COMMIT}", secretRef: {name: ${P}-token}}}}
---
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata: {name: ${P}-sglang, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "33"}}
spec:
  engine: {name: SGLang, image: "${SGLANG_IMAGE}"}
  model: {name: e2e/c33-gated, artifactRef: {name: ${P}-gated}}
  roles:
    - {name: server, acceleratorType: "${IT}", replicas: 1, resources: {accelerator: 1}, extraArgs: ["--mem-fraction-static", "0.4"]}
YAML
  phase="$(wait_md_ready "${P}-sglang" "$READY_BOUND")"
  pod="$(leader_pod "${P}-sglang")"
  if [ "$phase" != Ready ]; then
    record FAIL "SGLang gated deployment reaches Ready" "phase=${phase:-<none>} after ${READY_BOUND}s"
  else
    record PASS "SGLang gated deployment reaches Ready" "pod=${pod}"
    repo_dir="/var/lib/gpustack/model-cache/hub/models--${GATED//\//--}"
    snapshots="$(in_pod "$pod" ls "${repo_dir}/snapshots" | tr '\n' ' ')"
    got="$(in_pod "$pod" sha256sum "${repo_dir}/snapshots/${GATED_COMMIT}/tokenizer_config.json" | cut -d' ' -f1)"
    pinned="$(printf 'Authorization: Bearer %s' "$HF_TOKEN_READONLY" | curl -fsSL -H @- "${HUB}/${GATED}/resolve/${GATED_COMMIT}/tokenizer_config.json" | shasum -a 256 | cut -d' ' -f1)"
    head_sha="$(printf 'Authorization: Bearer %s' "$HF_TOKEN_READONLY" | curl -fsSL -H @- "${HUB}/${GATED}/resolve/main/tokenizer_config.json" | shasum -a 256 | cut -d' ' -f1)"
    if [ "$snapshots" = "${GATED_COMMIT} " ] && [ -n "$got" ] && [ "$got" = "$pinned" ] && [ "$got" != "$head_sha" ]; then
      record PASS "only pinned commit downloaded with matching tokenizer hash" "commit=${GATED_COMMIT} sha=${got:0:12}"
    else
      record FAIL "only pinned commit downloaded with matching tokenizer hash" "snapshots='${snapshots}' got=${got:0:12} pinned=${pinned:0:12}"
    fi
    endpoint="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "${P}-sglang" -o jsonpath='{.status.endpoint}')"
    answer="$(probe_curl -H 'Content-Type: application/json' "${endpoint}/v1/chat/completions" \
      -d '{"model":"e2e/c33-gated","max_tokens":8,"messages":[{"role":"user","content":"Say hello."}]}' \
      | python3 -c 'import json,sys;print(json.load(sys.stdin)["choices"][0]["message"]["content"] != "")' 2>/dev/null)"
    if [ "$answer" = True ]; then
      record PASS "gated SGLang deployment answers chat request" "e2e/c33-gated"
    else
      record FAIL "gated SGLang deployment answers chat request" "answer=${answer:-<none>}"
    fi
    used="$(in_pod "$pod" du -sb /var/lib/gpustack/model-cache | cut -f1)"
    limit="$(kubectl -n "$NS" get pod "$pod" -o jsonpath="{.spec.volumes[?(@.name=='gpustack-model-cache')].emptyDir.sizeLimit}")"
    if [ -n "$used" ] && [ -n "$limit" ] && [ "$used" -lt "$limit" ]; then
      record PASS "gated download stays inside cache sizeLimit" "used=${used} < limit=${limit}"
    else
      record FAIL "gated download stays inside cache sizeLimit" "used=${used:-<none>} limit=${limit:-<none>}"
    fi
  fi
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "${P}-sglang" --ignore-not-found --wait=false >/dev/null 2>&1 || true
fi

# =================================================================================================
# PHASE 2: Node-cache CSI 7B serving & zero-byte warm restart
# =================================================================================================

if [ "$(setting_get model-artifact-delivery-mode)" != Node ]; then
  record SKIP "Phase 2 Node-cache 7B zero-byte warm restart" "model-artifact-delivery-mode is not Node"
else
  running_pod_node() {
    kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${P}-node" -o json 2>/dev/null | python3 -c "
import json, sys
for p in json.load(sys.stdin)['items']:
    if not p['metadata'].get('deletionTimestamp'):
        print(p['metadata']['name'], p['spec'].get('nodeName', '')); break"
  }
  mount_hits() {
    plugin_metrics "$1" | awk '$1 == "gpustack_model_manager_mounts_total{result=\"hit\"}" {printf "%d", $2}'
  }
  node_serves() {
    local ep models ans
    ep="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "${P}-node" -o jsonpath='{.status.endpoint}')"
    models="$(probe_curl "${ep}/v1/models" | python3 -c 'import json,sys;print(",".join(m["id"] for m in json.load(sys.stdin)["data"]))' 2>/dev/null)"
    ans="$(probe_curl -H 'Content-Type: application/json' "${ep}/v1/chat/completions" \
      -d '{"model":"e2e/c33-node","max_tokens":8,"messages":[{"role":"user","content":"Say hello."}]}' \
      | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d.get("model","") if d["choices"][0]["message"]["content"] else "")' 2>/dev/null)"
    printf '%s|%s' "$models" "$ans"
  }

  kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata: {name: ${P}-node-model, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "33"}}
spec: {source: {huggingFace: {repository: "${NODE_MODEL}", revision: main}}}
YAML
  DIGEST="$(wait_resolved "$NS" "${P}-node-model" 180)"
  MODEL_SIZE="$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-node-model" -o jsonpath='{.status.resolved.sizeBytes}')"
  if [ -z "$DIGEST" ]; then
    record FAIL "7B ModelArtifact resolves" "${NODE_MODEL}"
  else
    record PASS "7B ModelArtifact resolves" "${NODE_MODEL} ${DIGEST:0:19} (${MODEL_SIZE}B)"
    for n in $(kubectl get nodes -o jsonpath='{.items[*].metadata.name}'); do
      printf '%s %s\n' "$n" "$(plugin_metric "$n" gpustack_model_manager_download_bytes_total)"
    done >"$BYTES_FILE"

    kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata: {name: ${P}-node, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "33"}}
spec:
  engine: {name: vLLM, image: "${VLLM_IMAGE}"}
  model: {name: e2e/c33-node, artifactRef: {name: ${P}-node-model}}
  roles:
    - {name: server, acceleratorType: "${IT}", replicas: 1, resources: {accelerator: 1}, extraArgs: ["--max-model-len", "4096", "--gpu-memory-utilization", "0.85"]}
YAML
    pod=""
    node=""
    for _ in $(seq 1 90); do
      read -r pod node <<<"$(running_pod_node)"
      [ -n "$node" ] && break
      sleep 10
    done
    if [ -z "$node" ]; then
      record FAIL "7B Node-delivery replica placed" "no Pod scheduled within 900s"
    else
      BYTES_BEFORE="$(awk -v n="$node" '$1 == n {print $2}' "$BYTES_FILE")"
      vol="$(kubectl -n "$NS" get pod "$pod" -o jsonpath="{.spec.volumes[?(@.csi.driver=='model.csi.gpustack.ai')].csi.volumeAttributes.manifestDigest}|{.spec.volumes[?(@.name=='gpustack-model-cache')].name}")"
      delivery="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "${P}-node" -o jsonpath='{.status.model.delivery}')"
      if [ "$vol" = "${DIGEST}|" ] && [ "$delivery" = Node ]; then
        record PASS "replica carries model.csi.gpustack.ai digest volume with delivery=Node" "${pod} on ${node}"
      else
        record FAIL "replica carries model.csi.gpustack.ai digest volume with delivery=Node" "vol=${vol} delivery=${delivery}"
      fi

      seen=""
      reading=""
      for _ in $(seq 1 $(( NODE_READY_BOUND / 10 ))); do
        reading="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "${P}-node" \
          -o jsonpath="{.status.conditions[?(@.type=='WeightsReady')].status}|{.status.conditions[?(@.type=='WeightsReady')].reason}" 2>/dev/null)"
        [ -n "$reading" ] && case "${seen} " in *" ${reading#*|} "*) ;; *) seen="${seen} ${reading#*|}" ;; esac
        [ "$reading" = "True|Mounted" ] && break
        sleep 10
      done
      if [[ "${seen} " == *" Materializing "* ]] && [ "$reading" = "True|Mounted" ]; then
        record PASS "WeightsReady transitioned Materializing -> Mounted" "reasons:${seen}"
      else
        record FAIL "WeightsReady transitioned Materializing -> Mounted" "reasons:${seen} last=${reading}"
      fi

      stored="$(kubectl get nodemodelstores.worker.gpustack.ai "$node" -o jsonpath="{range .status.models[?(@.digest=='${DIGEST}')]}{.state}|{.sizeBytes}{end}")"
      BYTES_AFTER="$(plugin_metric "$node" gpustack_model_manager_download_bytes_total)"
      grown=$(( ${BYTES_AFTER:-0} - ${BYTES_BEFORE:-0} ))
      pub_log="$(kubectl -n "$SYSTEM_NS" logs "$(plugin_pod "$node")" -c main --tail=-1 2>/dev/null \
        | grep '"published"' | grep "${DIGEST#sha256:}" | tail -1 | grep -o 'seconds=[0-9.]*' || true)"
      if [ "$stored" = "Ready|${MODEL_SIZE}" ] && [ "$grown" = "${MODEL_SIZE:-x}" ] && [ -n "$pub_log" ]; then
        record PASS "node downloaded and published 7B digest once" "${grown}B, ${pub_log}"
      else
        record FAIL "node downloaded and published 7B digest once" "store=${stored:-<none>} grown=${grown} size=${MODEL_SIZE} log=${pub_log:-<none>}"
      fi

      phase="$(wait_md_ready "${P}-node" "$NODE_READY_BOUND")"
      if [ "$phase" = Ready ]; then
        IFS='|' read -r models ans <<<"$(node_serves)"
        if [ "$models" = e2e/c33-node ] && [ "$ans" = e2e/c33-node ]; then
          record PASS "vLLM serves 7B from node cache" "e2e/c33-node"
        else
          record FAIL "vLLM serves 7B from node cache" "models=${models:-<none>} ans=${ans:-<none>}"
        fi
      else
        record FAIL "7B deployment reaches Ready" "phase=${phase:-<none>}"
      fi

      # Warm restart on same node: zero bytes downloaded + counted hit.
      HITS_BEFORE="$(mount_hits "$node")"
      BYTES_BEFORE="$(plugin_metric "$node" gpustack_model_manager_download_bytes_total)"
      kubectl -n "$NS" delete pod "$pod" --wait=false >/dev/null
      new_pod=""
      new_node=""
      for _ in $(seq 1 90); do
        read -r new_pod new_node <<<"$(running_pod_node)"
        [ -n "$new_pod" ] && [ "$new_pod" != "$pod" ] && [ -n "$new_node" ] && break
        new_pod=""
        sleep 5
      done
      sleep 20
      phase="$(wait_md_ready "${P}-node" "$NODE_READY_BOUND")"
      HITS_AFTER="$(mount_hits "$node")"
      BYTES_AFTER="$(plugin_metric "$node" gpustack_model_manager_download_bytes_total)"
      if [ "$new_node" = "$node" ] && [ "$BYTES_AFTER" = "$BYTES_BEFORE" ] && [ "${HITS_AFTER:-0}" -gt "${HITS_BEFORE:-0}" ]; then
        record PASS "warm replacement mounted with zero bytes downloaded and counted hit" "node=${node} hits ${HITS_BEFORE}->${HITS_AFTER}"
      else
        record FAIL "warm replacement mounted with zero bytes downloaded and counted hit" "node=${new_node:-<none>} bytes ${BYTES_BEFORE}->${BYTES_AFTER} hits ${HITS_BEFORE}->${HITS_AFTER}"
      fi
      if [ "$phase" = Ready ]; then
        IFS='|' read -r models ans <<<"$(node_serves)"
        if [ "$models" = e2e/c33-node ] && [ "$ans" = e2e/c33-node ]; then
          record PASS "warm replacement serves chat completions" "e2e/c33-node"
        else
          record FAIL "warm replacement serves chat completions" "models=${models:-<none>} ans=${ans:-<none>}"
        fi
      else
        record FAIL "warm replacement reaches Ready" "phase=${phase:-<none>}"
      fi
    fi
    kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "${P}-node" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  fi
fi

# =================================================================================================
# PHASE 3: cache_prefix KV weight isolation across two commits on one KVCachePoolBinding
# =================================================================================================

if [ -z "$MEMBER_NODE" ]; then
  record SKIP "Phase 3 cache_prefix KV weight isolation" "E2E_C33_MEMBER_NODE is not set"
else
  SERVED_KV="e2e/c33-kv"
  PROMPTS_PY='
import json, random, sys, urllib.request
words = ("time year people way day man thing woman life child world school state family student "
         "group country problem hand part place case week company system program question work").split()
start, count, served, url = int(sys.argv[1]), int(sys.argv[2]), sys.argv[3], sys.argv[4]
for seed in range(start, start + count):
    rng = random.Random(seed * 7919 + 13)
    prompt = "%d " % seed + " ".join(rng.choice(words) for _ in range(1000))
    body = json.dumps({"model": served, "prompt": prompt, "max_tokens": 1, "temperature": 0}).encode()
    req = urllib.request.Request(url + "/v1/completions", data=body, headers={"Content-Type": "application/json"})
    urllib.request.urlopen(req, timeout=600).read()
'
  METRIC_PY='
import sys, urllib.request
total = 0.0
for line in urllib.request.urlopen("http://127.0.0.1:8000/metrics").read().decode().splitlines():
    if line.startswith(sys.argv[1]):
        total += float(line.rsplit(" ", 1)[1])
print(int(total))
'
  kv_metric() { kubectl -n "$NS" exec "$(leader_pod "$1")" -c main -- python3 -c "$METRIC_PY" "$2" 2>/dev/null; }
  kv_hits() { kv_metric "$1" vllm:external_prefix_cache_hits_total; }
  kv_prefix() {
    kubectl -n "$NS" get pod "$(leader_pod "$1")" -o jsonpath='{.spec.containers[0].command}' 2>/dev/null \
      | command grep -o 'cache_prefix[^,]*' | head -1
  }
  kv_send() {
    local check="$1" md="$2" start="$3" count="$4" before after
    before="$(kv_metric "$md" vllm:request_success_total)"
    if ! kubectl -n "$NS" exec "$(leader_pod "$md")" -c main -- python3 -c "$PROMPTS_PY" "$start" "$count" "$SERVED_KV" "$ENGINE_URL" >/dev/null 2>&1; then
      record FAIL "$check" "${md}: sending seeds ${start}+${count} failed"
      return 1
    fi
    after="$(kv_metric "$md" vllm:request_success_total)"
    if [ -z "$before" ] || [ -z "$after" ] || [ $((after - before)) -ne "$count" ]; then
      record FAIL "$check" "${md}: request_success_total ${before:-?} -> ${after:-?}, not +${count}"
      return 1
    fi
    return 0
  }
  kv_deploy() {
    kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata: {name: ${1}, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "33"}}
spec:
  engine: {name: vLLM, image: "${VLLM_IMAGE}"}
  model: {name: ${SERVED_KV}, artifactRef: {name: ${2}}}
  kvCache: {poolRef: {name: ${P}-shared}}
  roles:
    - {name: server, acceleratorType: "${IT}", replicas: 1, resources: {accelerator: 1}, extraArgs: ["--max-model-len", "2048", "--gpu-memory-utilization", "0.3"]}
YAML
  }

  kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCacheBackend
metadata: {name: ${P}-kvcb}
spec:
  type: Mooncake
  connection:
    managed:
      leader: {multiTenancy: true}
      members:
        - {nodeSelector: {kubernetes.io/hostname: "${MEMBER_NODE}"}, medium: DRAM, capacityPerMember: 4Gi}
---
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePool
metadata: {name: ${P}-kvcp}
spec: {backends: [${P}-kvcb], quota: {total: 4Gi}}
---
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePoolBinding
metadata: {name: ${P}-shared, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "33"}}
spec:
  poolRef: {name: ${P}-kvcp}
  quota: {ceiling: 4Gi}
  domain: {name: ${P}-shared, blockSize: 16, dtype: bfloat16}
---
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata: {name: ${P}-a, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "33"}}
spec: {source: {huggingFace: {repository: HuggingFaceTB/SmolLM-135M-Instruct, revision: "${COMMIT_A}"}}}
---
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata: {name: ${P}-b, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "33"}}
spec: {source: {huggingFace: {repository: HuggingFaceTB/SmolLM-135M-Instruct, revision: "${COMMIT_B}"}}}
YAML

  kv_deploy "${P}-writer" "${P}-a"
  kv_deploy "${P}-same" "${P}-a"
  w="$(wait_md_ready "${P}-writer" "$READY_BOUND")"
  s="$(wait_md_ready "${P}-same" "$READY_BOUND")"
  if [ "$w" != Ready ] || [ "$s" != Ready ]; then
    record FAIL "KV writer and same-artifact deployment reach Ready" "writer=${w:-<none>} same=${s:-<none>}"
  else
    kv_send "writer fills store with commit A" "${P}-writer" 1000 8 && record PASS "writer fills store with commit A" "8 requests finished"
    sleep 15
    p_writer="$(kv_prefix "${P}-writer")"
    p_same="$(kv_prefix "${P}-same")"
    if [ -n "$p_writer" ] && [ "$p_writer" = "$p_same" ]; then
      record PASS "same artifact renders identical cache_prefix" "$p_writer"
    else
      record FAIL "same artifact renders identical cache_prefix" "writer=${p_writer:-<none>} same=${p_same:-<none>}"
    fi
    before="$(kv_hits "${P}-same")"
    if kv_send "must-miss control on unseen prompts" "${P}-same" 9000 4; then
      control="$(kv_hits "${P}-same")"
      if [ -n "$control" ] && [ "$control" = "$before" ]; then
        record PASS "must-miss control on unseen prompts" "hits ${before} -> ${control}"
      else
        record FAIL "must-miss control on unseen prompts" "hits ${before:-?} -> ${control:-?}"
      fi
      if kv_send "must-hit replay on same artifact" "${P}-same" 1000 8; then
        after="$(kv_hits "${P}-same")"
        if [ -n "$after" ] && [ "$after" -gt "$control" ]; then
          record PASS "must-hit replay on same artifact" "hits ${control} -> ${after}"
        else
          record FAIL "must-hit replay on same artifact" "hits ${control} -> ${after:-<none>}"
        fi
      fi
    fi

    kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "${P}-same" --wait=false >/dev/null 2>&1 || true
    kv_deploy "${P}-rev" "${P}-b"
    r="$(wait_md_ready "${P}-rev" "$READY_BOUND")"
    if [ "$r" != Ready ]; then
      record FAIL "commit B deployment reaches Ready" "phase=${r:-<none>}"
    else
      p_rev="$(kv_prefix "${P}-rev")"
      before="$(kv_hits "${P}-rev")"
      if kv_send "commit B gets zero hits from commit A blocks" "${P}-rev" 1000 8; then
        after="$(kv_hits "${P}-rev")"
        if [ -n "$p_rev" ] && [ "$p_rev" != "$p_writer" ] && [ -n "$after" ] && [ "$after" = "$before" ]; then
          record PASS "commit B gets zero hits from commit A blocks" "prefix=${p_rev}, hits ${before} -> ${after}"
        else
          record FAIL "commit B gets zero hits from commit A blocks" "prefix=${p_rev:-<none>} (writer ${p_writer}), hits ${before:-?} -> ${after:-<none>}"
        fi
      fi
    fi
  fi
fi

results
