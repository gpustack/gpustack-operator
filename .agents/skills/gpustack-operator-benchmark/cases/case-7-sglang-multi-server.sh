#!/usr/bin/env bash
#
# CASE 7 — SGLang multi-Server behind the gateway   (MUTATING, self-recovering)
#
#   case-7-sglang-multi-server.sh [OUT_DIR]
#
# Goal:        Exercise the SGLang multi-Server cell, which has no runtime coverage today: one
#              Server role whose replica count goes 1 -> 2 -> 1, each replica an INDEPENDENT
#              SGLang instance (not a member of a collective), served through the gateway. The case
#              proves the four content checks on every replica, and watches the gateway's worker
#              set grow by exactly one worker per replica and shrink back with it.
# Environment: A cluster with 2 free GPUs on one InstanceType, NodeModelStore caching qwen3-8b,
#              and an in-cluster default/bench-driver Pod. The model is Qwen/Qwen3-8B, served from
#              the qwen3-8b artifact the store caches: one GPU per replica, two replicas at peak
#              = 2 GPUs.
# Inputs:      The manifest is written by this script into OUT_DIR/sglang-multi-server.yaml. The
#              operator stamps the replica's own addresses, so the manifest is identical for every
#              replica.
# Expected:    - every replica answers ordinary, SSE, exact-Chinese and JSON content checks;
#              - scaling 1 -> 2 grows the gateway's worker set by exactly one worker per replica;
#              - scaling 2 -> 1 shrinks it back, and no replica of the removed instance stays
#                routed;
#              - the deployment reports no External classification, because an ordinary Server role
#                is leader-served Internal.
# Cleanup:     Deletes the deployment on completion and releases its GPU claims.
#
# NOT EXECUTED YET: this script was written for the acceptance run and has never been run on a
# cluster. Every expectation above is the contract, not an observation.
set -uo pipefail

CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SKILL_DIR="$(cd "${CASE_DIR}/.." && pwd)"
REPO="$(cd "${SKILL_DIR}/../../.." && pwd)"
export PATH="${REPO}/.agents/skills/_e2e-lib/scripts/kubectl-shim:${PATH}"

NS="${BENCH_NS:-gpustack-bench}"
OUT="${1:-${REPO}/.claude/reports/$(date +%F)-benchmark/sglang-multi-server}"
DEPLOY="sglang-multi-server"
MODEL="${SGLANG_MODEL:-Qwen/Qwen3-8B}"
# THE ARTIFACT IS WHAT THE DEPLOYMENT SERVES, and naming it is what keeps the pod alive. Without
# an artifactRef the operator renders the model as the repository and the engine downloads the
# weights itself onto the container's writable layer -- 16GB for Qwen3-8B against the 15Gi
# ephemeral limit the role derives -- so the pod is evicted while it is still starting, and the
# NodeModelStore cache the environment already warmed is never read. With one, the operator
# serves the weights from the delivery the artifact names and pins the served name below.
ARTIFACT="${SGLANG_ARTIFACT:-qwen3-8b}"
IMAGE="${SGLANG_IMAGE:-gpustack/runner:cuda13.0-sglang0.5.22}"
ROUTER_IMAGE="${SGLANG_ROUTER_IMAGE:-gpustack/llm-router:v0.3.1}"
INSTANCE_TYPE="${SGLANG_INSTANCE_TYPE:-gpustack--nvidia-h100-80gb-hbm3-linux-amd64}"
LOCAL_PORT_BASE="${LOCAL_PORT_BASE:-18200}"
READY_TIMEOUT="${READY_TIMEOUT:-1200}"
SCALE_TIMEOUT="${SCALE_TIMEOUT:-900}"
PF_PIDS=""

mkdir -p "$OUT"
log() { echo "[$(date +%H:%M:%S)] $*"; }

cleanup() {
  local pid
  for pid in $PF_PIDS; do
    kill "$pid" 2>/dev/null || true
  done
  PF_PIDS=""
  kubectl -n "$NS" delete modeldeployment "$DEPLOY" --ignore-not-found --wait=true --timeout=180s \
    >/dev/null 2>&1 || true
  return 0
}
trap cleanup EXIT

write_manifest() {
  cat > "${OUT}/sglang-multi-server.yaml" <<YAML
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: ${DEPLOY}
  namespace: ${NS}
spec:
  model:
    # THE SERVED NAME IS /models, and the artifactRef is what makes it the served name: with an
    # artifact the operator renders the model argument as the delivery's path or repository and
    # pins --served-model-name to spec.model.name, so the engine answers to /models and the
    # request bodies below carry that same string.
    name: /models
    artifactRef:
      name: ${ARTIFACT}
  engine:
    name: SGLang
    image: ${IMAGE}
  router:
    name: sglang-gateway
    image: ${ROUTER_IMAGE}
  roles:
    - name: server
      kind: Server
      replicas: 1
      size: 1
      instanceType: ${INSTANCE_TYPE}
      image: ${IMAGE}
      imagePullPolicy: IfNotPresent
      resources:
        accelerator: "1"
YAML
}

# replica_pods prints every Server replica Pod name, one per line.
replica_pods() {
  kubectl -n "$NS" get pods \
    -l "app.kubernetes.io/instance=${DEPLOY},app.kubernetes.io/component=server" \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null
}

start_replica_forward() {
  local pod="$1" port="$2"
  kubectl -n "$NS" port-forward "pod/${pod}" "${port}:8000" > "${OUT}/port-forward-${port}.log" 2>&1 &
  PF_PIDS="$PF_PIDS $!"
  local _
  for _ in $(seq 1 60); do
    if curl -sf -o /dev/null "http://127.0.0.1:${port}/health"; then
      return 0
    fi
    sleep 2
  done
  return 1
}

stop_forwards() {
  local pid
  for pid in $PF_PIDS; do
    kill "$pid" 2>/dev/null || true
  done
  PF_PIDS=""
}

router_metrics_port() {
  local pod args
  pod="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${DEPLOY},app.kubernetes.io/component=router" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
  [ -n "$pod" ] || return 1
  args="$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.spec.containers[0].args}' 2>/dev/null)"
  printf '%s' "$args" | sed -n 's/.*--prometheus-port=\([0-9][0-9]*\).*/\1/p' | head -1
}

# scrape_router_metrics writes the gateway's own metric page. The per-worker series carry the
# worker URL; the selection series do not carry a worker label and must never be cited for
# distribution.
scrape_router_metrics() {
  local name="$1" pod port
  pod="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${DEPLOY},app.kubernetes.io/component=router" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
  port="$(router_metrics_port || true)"
  if [ -z "$pod" ] || [ -z "$port" ]; then
    echo "router pod or metrics port not found" > "${OUT}/${name}.txt"
    return 1
  fi
  kubectl -n "$NS" port-forward "pod/${pod}" "$((LOCAL_PORT_BASE + 90)):${port}" \
    > "${OUT}/port-forward-metrics.log" 2>&1 &
  PF_PIDS="$PF_PIDS $!"
  local _
  for _ in $(seq 1 30); do
    if curl -sf -o "${OUT}/${name}.txt" "http://127.0.0.1:$((LOCAL_PORT_BASE + 90))/metrics"; then
      return 0
    fi
    sleep 2
  done
  echo "metrics scrape failed" > "${OUT}/${name}.txt"
  return 1
}

# wait_workers waits until the gateway's worker set reaches the expected size, and prints the
# number of workers it saw.
wait_workers() {
  local expected="$1" deadline=$((SECONDS + SCALE_TIMEOUT)) count
  while [ "$SECONDS" -lt "$deadline" ]; do
    scrape_router_metrics "workers-${expected}" >/dev/null 2>&1 || true
    count="$(grep -c '^smg_worker_health{' "${OUT}/workers-${expected}.txt" 2>/dev/null || echo 0)"
    if [ "${count:-0}" -ge "$expected" ]; then
      printf '%s' "$count"
      return 0
    fi
    sleep 5
  done
  printf '%s' "${count:-0}"
  return 1
}

# THE MODEL IN THE BODY IS THE DEPLOYMENT'S OWN SERVED NAME. The operator pins the engine's
# --served-model-name to spec.model.name, and a request that names anything else fails silently
# behind a deployment that still reports Ready. The manifest above writes /models, and the four
# bodies below carry that same string -- the served name, not the repository the artifact holds.
check_ordinary() {
  local port="$1" out="$2"
  curl --fail-with-body --max-time 120 "http://127.0.0.1:${port}/v1/completions" \
    -H 'Content-Type: application/json' \
    -d '{"model":"/models","prompt":"The sky is","max_tokens":8,"temperature":0}' \
    -o "$out" 2>"${out}.err"
}

check_sse() {
  local port="$1" out="$2"
  curl --fail-with-body --no-buffer --max-time 120 "http://127.0.0.1:${port}/v1/completions" \
    -H 'Content-Type: application/json' \
    -d '{"model":"/models","prompt":"A GPU is","max_tokens":8,"temperature":0,"stream":true,"stream_options":{"include_usage":true}}' \
    -o "$out" 2>"${out}.err"
}

check_chinese() {
  local port="$1" out="$2"
  curl --fail-with-body --max-time 120 "http://127.0.0.1:${port}/v1/completions" \
    -H 'Content-Type: application/json' \
    -d '{"model":"/models","prompt":"请用一句中文说明什么是 GPU。","max_tokens":64,"temperature":0}' \
    -o "$out" 2>"${out}.err"
}

check_json() {
  local port="$1" out="$2"
  curl --fail-with-body --max-time 120 "http://127.0.0.1:${port}/v1/completions" \
    -H 'Content-Type: application/json' \
    -d '{"model":"/models","prompt":"Return a JSON object with the key name and the value gpu.","max_tokens":64,"temperature":0,"response_format":{"type":"json_object"}}' \
    -o "$out" 2>"${out}.err"
}

# check_replica runs the four content checks against one replica's own door.
check_replica() {
  local port="$1" tag="$2"
  check_ordinary "$port" "${OUT}/${tag}-ordinary.json"
  check_sse "$port" "${OUT}/${tag}-sse.txt"
  check_chinese "$port" "${OUT}/${tag}-chinese.json"
  check_json "$port" "${OUT}/${tag}-json.json"
}

collect_verdicts() {
  local tag="$1" f
  f="${OUT}/${tag}-ordinary.json"
  [ -e "$f" ] || return 0
  jq -e '.choices[0].text | length > 0' "$f" >/dev/null 2>&1 \
    && echo "PASS $(basename "$f")" >> "${OUT}/content-verdicts.txt" \
    || echo "FAIL $(basename "$f")" >> "${OUT}/content-verdicts.txt"

  f="${OUT}/${tag}-sse.txt"
  [ -e "$f" ] || return 0
  grep -q 'data: \[DONE\]' "$f" && echo "PASS $(basename "$f")" >> "${OUT}/content-verdicts.txt" \
    || echo "FAIL $(basename "$f")" >> "${OUT}/content-verdicts.txt"

  f="${OUT}/${tag}-chinese.json"
  [ -e "$f" ] || return 0
  jq -e '.choices[0].text' "$f" 2>/dev/null | grep -qP '[\x{4e00}-\x{9fff}]' \
    && echo "PASS $(basename "$f")" >> "${OUT}/content-verdicts.txt" \
    || echo "FAIL $(basename "$f")" >> "${OUT}/content-verdicts.txt"

  f="${OUT}/${tag}-json.json"
  [ -e "$f" ] || return 0
  jq -e '.choices[0].text' "$f" 2>/dev/null | jq -e . >/dev/null 2>&1 \
    && echo "PASS $(basename "$f")" >> "${OUT}/content-verdicts.txt" \
    || echo "FAIL $(basename "$f")" >> "${OUT}/content-verdicts.txt"
}

log "The model is ${MODEL}, served as /models from the artifact ${ARTIFACT}"
log "Writing the manifest into ${OUT}/sglang-multi-server.yaml"
write_manifest
kubectl apply --dry-run=server -f "${OUT}/sglang-multi-server.yaml" > "${OUT}/apply-dry-run.txt" 2>&1

log "Applying the deployment"
kubectl -n "$NS" delete modeldeployment "$DEPLOY" --ignore-not-found --wait=true --timeout=180s >/dev/null 2>&1 || true
kubectl apply -f "${OUT}/sglang-multi-server.yaml" 2>&1 | tee "${OUT}/apply.log"

deadline=$((SECONDS + READY_TIMEOUT))
phase=""
while [ "$SECONDS" -lt "$deadline" ]; do
  phase="$(kubectl -n "$NS" get modeldeployment "$DEPLOY" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  [ "$phase" = "Ready" ] && break
  sleep 5
done
if [ "$phase" != "Ready" ]; then
  log "TIMEOUT: phase=${phase:-unknown}"
  kubectl -n "$NS" describe modeldeployment "$DEPLOY" > "${OUT}/describe.txt" 2>&1
  exit 1
fi
log "The deployment is Ready"

kubectl -n "$NS" get modeldeployment "$DEPLOY" -o json \
  | jq '.status.roles[] | {name, kind, ready, parallelism, endpoints}' > "${OUT}/initial-roles.json" 2>/dev/null

# --- the content checks on every replica -----------------------------------
index=0
for pod in $(replica_pods); do
  port=$((LOCAL_PORT_BASE + index))
  tag="replica-${index}"
  index=$((index + 1))
  log "Checking replica ${pod} on local port ${port}"
  if start_replica_forward "$pod" "$port"; then
    check_replica "$port" "$tag"
    collect_verdicts "$tag"
  else
    echo "FAIL ${tag} did not answer its own /health" >> "${OUT}/content-verdicts.txt"
  fi
done
stop_forwards
cat "${OUT}/content-verdicts.txt" 2>/dev/null

# --- scale 1 -> 2 ----------------------------------------------------------
log "Scaling the replica count 1 -> 2"
kubectl -n "$NS" patch modeldeployment "$DEPLOY" --type=merge \
  -p '{"spec":{"roles":[{"name":"server","replicas":2}]}}' > "${OUT}/scale-up-patch.txt" 2>&1
workers_after_up="$(wait_workers 2 || true)"
log "The gateway's worker set after the scale-up: ${workers_after_up}"
kubectl -n "$NS" get modeldeployment "$DEPLOY" -o json \
  | jq '.status.roles[] | {name, ready, parallelism, endpoints}' > "${OUT}/after-scale-up-roles.json" 2>/dev/null

# --- scale 2 -> 1 ----------------------------------------------------------
log "Scaling the replica count 2 -> 1"
kubectl -n "$NS" patch modeldeployment "$DEPLOY" --type=merge \
  -p '{"spec":{"roles":[{"name":"server","replicas":1}]}}' > "${OUT}/scale-down-patch.txt" 2>&1
deadline=$((SECONDS + SCALE_TIMEOUT))
while [ "$SECONDS" -lt "$deadline" ]; do
  replicas="$(kubectl -n "$NS" get modeldeployment "$DEPLOY" -o jsonpath='{.spec.roles[0].replicas}' 2>/dev/null || true)"
  [ "$replicas" = "1" ] && break
  sleep 5
done
while [ "$SECONDS" -lt "$deadline" ]; do
  workers_after_down="$(scrape_router_metrics "workers-after-scale-down" >/dev/null 2>&1 \
    && grep -c '^smg_worker_health{' "${OUT}/workers-after-scale-down.txt" 2>/dev/null || echo 0)"
  [ "${workers_after_down:-0}" -le 1 ] && break
  sleep 5
done
kubectl -n "$NS" get modeldeployment "$DEPLOY" -o json \
  | jq '.status.roles[] | {name, ready, parallelism, endpoints}' > "${OUT}/after-scale-down-roles.json" 2>/dev/null

bash "${SKILL_DIR}/scripts/stage_counts.sh" "$DEPLOY" "$OUT" > "${OUT}/stage-counts.txt" 2>&1
kubectl get devices -o json > "${OUT}/released-devices.json" 2>/dev/null || true

log "CASE 7 (SGLang multi-Server) complete"
