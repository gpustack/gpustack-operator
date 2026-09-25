#!/usr/bin/env bash
#
# CASE 99 — ModelArtifact serving: vLLM loads a claim artifact behind llm-d-router under
#           spec.model.name, and SGLang downloads a gated artifact pinned to its resolved commit
#           (MUTATING, self-recovering; needs real accelerators)
#
#   case-99.sh <NS>
#
# Goal:        Prove both deliveries serve on real engines: a claim is loaded from its read-only
#              local path and served under spec.model.name, so the router's token producer, the
#              served-model list and the metrics' model label agree; a gated repository is downloaded
#              with the artifact's token at the resolved commit, not at the branch head, and the
#              download stays inside its size-limited cache.
# Environment: A cluster with one accelerator InstanceType (E2E_C99_IT) that can host two
#              single-card replicas at once, a default StorageClass, a worker and nodes that reach
#              the Hub, and in <NS> the pool's entrance LocalQueue. HF_TOKEN_READONLY must hold a
#              token that reads E2E_C99_GATED (default XyX824/mam-c-gated-tiny), whose commit
#              E2E_C99_GATED_COMMIT is a copy of a small public model and whose main carries a
#              different tokenizer. E2E_PD_PROBE_IMAGE (default curlimages/curl:8.11.1) sends the
#              requests.
# Inputs:      All real. Qwen/Qwen2.5-0.5B-Instruct is copied into a fresh claim by a Job
#              (python:3.12-slim with huggingface_hub). vLLM 0.29.0 and SGLang 0.5.18 runner images
#              are synthesized from the InstanceType.
# Expected:    - the claim deployment goes Ready; /v1/models lists exactly spec.model.name; every
#                model_name label is spec.model.name; a chat request through llm-d-router answers
#                200 naming spec.model.name; the router logs no tokenization or per-request failure;
#              - the gated deployment goes Ready; its cache holds only the pinned commit's snapshot;
#                tokenizer_config.json in it hashes to the pinned commit's file, not main's; a chat
#                request answers 200; the cache's usage stays under its sizeLimit.
# Cleanup:     Trap deletes the deployments, the artifacts, the claim, the Job, the Secret and the
#              probe Pod.
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

NS="${1:?usage: case-99.sh <NS>}"
IT="${E2E_C99_IT:?set E2E_C99_IT to an accelerator InstanceType}"
GATED="${E2E_C99_GATED:-XyX824/mam-c-gated-tiny}"
GATED_COMMIT="${E2E_C99_GATED_COMMIT:-849d87054adea51b3cc2e81b95ed17b1289e9a40}"
PROBE_IMAGE="${E2E_PD_PROBE_IMAGE:-curlimages/curl:8.11.1}"
READY_BOUND="${E2E_C99_READY_BOUND:-1200}"
P=c99
LABEL="e2e.gpustack.ai/case=99"
HUB=https://huggingface.co

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

[ -n "${HF_TOKEN_READONLY:-}" ] || { echo "[case-99] HF_TOKEN_READONLY is not set" >&2; exit 2; }

cleanup() {
  echo
  echo "[case-99] cleanup"
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai -l "$LABEL" --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl -n "$NS" delete pod "${P}-probe" --ignore-not-found --wait=false >/dev/null 2>&1
  for _ in $(seq 1 36); do
    [ -z "$(kubectl -n "$NS" get pods -l 'app.kubernetes.io/name=model-deployment' -o name 2>/dev/null | command grep "${P}-")" ] && break
    sleep 5
  done
  kubectl -n "$NS" delete job,modelartifacts.worker.gpustack.ai,pvc,secret -l "$LABEL" --ignore-not-found --wait=false >/dev/null 2>&1
}
trap cleanup EXIT

wait_ready() { # md -> phase
  local phase=""
  for _ in $(seq 1 $(( READY_BOUND / 10 ))); do
    phase="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "$1" -o jsonpath='{.status.phase}' 2>/dev/null)"
    [ "$phase" = Ready ] && break
    sleep 10
  done
  printf '%s' "$phase"
}

leader() { kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=$1,app.kubernetes.io/component=server" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null; }
in_pod() { local pod="$1"; shift; kubectl -n "$NS" exec "$pod" -c main -- "$@" 2>/dev/null; }
probe() { kubectl -n "$NS" exec "${P}-probe" -- curl -sS -m 120 "$@" 2>/dev/null; }

echo "== fixtures: a probe, a claim holding Qwen2.5-0.5B-Instruct, the token =="
kubectl -n "$NS" run "${P}-probe" --image="$PROBE_IMAGE" --labels="$LABEL" --restart=Never --command -- sleep 7200 >/dev/null
kubectl -n "$NS" create secret generic "${P}-token" --from-file=token=/dev/stdin --dry-run=client -o yaml <<<"$HF_TOKEN_READONLY" \
  | kubectl label --local -f - "$LABEL" -o yaml | kubectl apply -f - >/dev/null
kubectl apply -f - >/dev/null <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: ${P}-models, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "99"}}
spec: {accessModes: [ReadWriteOnce], resources: {requests: {storage: 10Gi}}}
---
apiVersion: batch/v1
kind: Job
metadata: {name: ${P}-copy, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "99"}}
spec:
  backoffLimit: 3
  template:
    metadata: {labels: {e2e.gpustack.ai/case: "99"}}
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
metadata: {name: ${P}-claim, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "99"}}
spec: {source: {persistentVolumeClaim: {claimName: ${P}-models, path: qwen}}}
---
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata: {name: ${P}-gated, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "99"}}
spec: {source: {huggingFace: {repository: "${GATED}", revision: "${GATED_COMMIT}", secretRef: {name: ${P}-token}}}}
YAML
if ! kubectl -n "$NS" wait --for=condition=complete "job/${P}-copy" --timeout=900s >/dev/null 2>&1; then
  record FAIL "the claim is populated" "job/${P}-copy did not complete within 900s"
  print_rows
  exit 1
fi
record PASS "the claim is populated" "Qwen/Qwen2.5-0.5B-Instruct copied to ${P}-models/qwen"

echo "== 1. vLLM on the claim, behind llm-d-router =="
kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata: {name: ${P}-vllm, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "99"}}
spec:
  engine: {name: vllm, version: "0.29.0"}
  model: {name: e2e/c99-claim, artifactRef: {name: ${P}-claim}}
  router: {name: llm-d-router}
  roles:
    - {name: server, instanceType: "${IT}", replicas: 1, resources: {accelerator: 1}, extraArgs: ["--max-model-len", "4096", "--gpu-memory-utilization", "0.4"]}
YAML
phase="$(wait_ready "${P}-vllm")"
pod="$(leader "${P}-vllm")"
if [ "$phase" != Ready ]; then
  record FAIL "the claim deployment goes Ready" "phase=${phase:-<none>} after ${READY_BOUND}s"
else
  record PASS "the claim deployment goes Ready" "pod=${pod}"
  direct="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "${P}-vllm" -o jsonpath='{.status.router.roles[0].endpoint}')"
  models="$(probe "${direct}/v1/models" | python3 -c 'import json,sys;print(",".join(m["id"] for m in json.load(sys.stdin)["data"]))' 2>/dev/null)"
  labels="$(probe "${direct}/metrics" | command grep -o 'model_name="[^"]*"' | sort -u | tr '\n' ' ')"
  if [ "$models" = e2e/c99-claim ] && [ "$labels" = 'model_name="e2e/c99-claim" ' ]; then
    record PASS "the engine serves and reports spec.model.name" "models=${models} labels=${labels}"
  else
    record FAIL "the engine serves and reports spec.model.name" "models=${models:-<none>} labels=${labels:-<none>}"
  fi
  endpoint="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "${P}-vllm" -o jsonpath='{.status.endpoint}')"
  answer="$(probe -H 'Content-Type: application/json' "${endpoint}/v1/chat/completions" \
    -d '{"model":"e2e/c99-claim","max_tokens":8,"messages":[{"role":"user","content":"Say hello."}]}' \
    | python3 -c 'import json,sys;print(json.load(sys.stdin).get("model",""))' 2>/dev/null)"
  router_logs="$(for d in $(kubectl -n "$NS" get deploy -o name 2>/dev/null | command grep "/${P}-vllm"); do
      kubectl -n "$NS" logs "$d" --all-containers --tail=-1 2>/dev/null; done)"
  router_errors="$(printf '%s\n' "$router_logs" | command grep -cE 'tokenization failed|failed to prepare per request data')"
  # An empty log would make zero errors a reading of nothing, so the router's logs must exist.
  if [ "$answer" = e2e/c99-claim ] && [ -n "$router_logs" ] && [ "${router_errors:-0}" = 0 ]; then
    record PASS "a routed chat request answers under spec.model.name and the token producer never fails" "endpoint=${endpoint}"
  else
    record FAIL "a routed chat request answers under spec.model.name and the token producer never fails" "answer=${answer:-<none>} router errors=${router_errors}"
  fi
fi

echo "== 2. SGLang downloads the gated commit with the token =="
kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata: {name: ${P}-sglang, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "99"}}
spec:
  engine: {name: sglang, version: "0.5.18"}
  model: {name: e2e/c99-gated, artifactRef: {name: ${P}-gated}}
  roles:
    - {name: server, instanceType: "${IT}", replicas: 1, resources: {accelerator: 1}, extraArgs: ["--mem-fraction-static", "0.4"]}
YAML
phase="$(wait_ready "${P}-sglang")"
pod="$(leader "${P}-sglang")"
if [ "$phase" != Ready ]; then
  record FAIL "the gated deployment goes Ready" "phase=${phase:-<none>} after ${READY_BOUND}s"
else
  record PASS "the gated deployment goes Ready" "pod=${pod}"
  repo_dir="/var/lib/gpustack/model-cache/hub/models--${GATED//\//--}"
  snapshots="$(in_pod "$pod" ls "${repo_dir}/snapshots" | tr '\n' ' ')"
  got="$(in_pod "$pod" sha256sum "${repo_dir}/snapshots/${GATED_COMMIT}/tokenizer_config.json" | cut -d' ' -f1)"
  # The header is read from stdin so the token never appears in the process list.
  pinned="$(printf 'Authorization: Bearer %s' "$HF_TOKEN_READONLY" | curl -fsSL -H @- "${HUB}/${GATED}/resolve/${GATED_COMMIT}/tokenizer_config.json" | shasum -a 256 | cut -d' ' -f1)"
  head="$(printf 'Authorization: Bearer %s' "$HF_TOKEN_READONLY" | curl -fsSL -H @- "${HUB}/${GATED}/resolve/main/tokenizer_config.json" | shasum -a 256 | cut -d' ' -f1)"
  if [ "$snapshots" = "${GATED_COMMIT} " ] && [ -n "$got" ] && [ "$got" = "$pinned" ] && [ "$got" != "$head" ]; then
    record PASS "only the pinned commit is downloaded, tokenizer included" "snapshot=${GATED_COMMIT} tokenizer_config=${got:0:12} (main ${head:0:12})"
  else
    record FAIL "only the pinned commit is downloaded, tokenizer included" "snapshots=${snapshots:-<none>} got=${got:-<none>} pinned=${pinned:-<none>} main=${head:-<none>}"
  fi
  endpoint="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "${P}-sglang" -o jsonpath='{.status.endpoint}')"
  answer="$(probe -H 'Content-Type: application/json' "${endpoint}/v1/chat/completions" \
    -d '{"model":"e2e/c99-gated","max_tokens":8,"messages":[{"role":"user","content":"Say hello."}]}' \
    | python3 -c 'import json,sys;print(json.load(sys.stdin)["choices"][0]["message"]["content"] != "")' 2>/dev/null)"
  [ "$answer" = True ] && record PASS "a chat request answers" "e2e/c99-gated" \
    || record FAIL "a chat request answers" "answer=${answer:-<none>}"
  used="$(in_pod "$pod" du -sb /var/lib/gpustack/model-cache | cut -f1)"
  limit="$(kubectl -n "$NS" get pod "$pod" -o jsonpath="{.spec.volumes[?(@.name=='gpustack-model-cache')].emptyDir.sizeLimit}")"
  record "$([ -n "$used" ] && [ "$used" -lt "$limit" ] && echo PASS || echo FAIL)" \
    "the download stays inside its cache" "used=${used:-<none>} bytes of sizeLimit=${limit:-<none>}"
fi

print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-99] ${FAILS} check(s) FAILED"; exit 1; }
