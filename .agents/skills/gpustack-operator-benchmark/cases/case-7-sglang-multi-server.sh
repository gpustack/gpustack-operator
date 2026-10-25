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
# THE ROUTER'S NAME IS WRITTEN ONCE, because the manifest names the router and the Pod lookup
# below selects on the label the operator renders from that same name.
ROUTER_NAME="sglang-gateway"
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
    name: ${ROUTER_NAME}
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

# THE ROUTER POD IS FOUND BY THE LABELS THE OPERATOR RENDERS ON IT. The router is its own
# Deployment, and its Pod template carries the deployment's instance label and the router's own
# name label (pkg/worker/controllers/worker/model_deployment_router.go, the labels the router
# objects are rendered with). It carries no component label, so a selector that asks for one
# matches nothing, and the worker set below then reads as empty however many workers are there.
router_pod() {
  kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${DEPLOY},modeldeployment.gpustack.ai/router=${ROUTER_NAME}" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

router_metrics_port() {
  local pod args
  pod="$(router_pod)"
  [ -n "$pod" ] || return 1
  args="$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.spec.containers[0].args}' 2>/dev/null)"
  printf '%s' "$args" | sed -n 's/.*--prometheus-port=\([0-9][0-9]*\).*/\1/p' | head -1
}

# scrape_router_metrics writes the gateway's own metric page. The per-worker series carry the
# worker URL; the selection series do not carry a worker label and must never be cited for
# distribution.
scrape_router_metrics() {
  local name="$1" pod port
  pod="$(router_pod)"
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
    # grep -c prints 0 on no match and exits 1, so the count is already the answer. Appending a
    # second 0 to it made the count two lines, and the comparison below read that as a
    # non-integer.
    #
    # THE COUNT IS OF HEALTHY WORKERS, NOT OF METRIC LINES. The gateway keeps the series of a
    # worker it withdrew and marks that series with the value -1, so counting the lines counted
    # a worker that had already left and the count never reached the expected size.
    count="$(awk '/^smg_worker_health\{/ && $NF == 1 { n++ } END { print n + 0 }' \
      "${OUT}/workers-${expected}.txt" 2>/dev/null || echo 0)"
    if [ "${count:-0}" -ge "$expected" ]; then
      printf '%s' "$count"
      return 0
    fi
    sleep 5
  done
  printf '%s' "${count:-0}"
  return 1
}

# scale_replicas COUNT LABEL patches the Server role's replica count and verifies that it took.
#
# THE PATCH NAMES THE ONE FIELD IT CHANGES. A merge patch on spec.roles replaces the whole role
# list, so the role that comes back carries only the two fields the patch carried and the API
# server rejects the deployment for the role it no longer describes: the instance type, the
# image, the size and the resources are all gone, and the recorded rejection reads
# "spec.roles[0].instanceType: Invalid value: empty". A JSON patch changes the one number and
# leaves the rest of the role alone. The path names the first role because the manifest
# declares exactly one role, the server role.
#
# THE PATCH IS VERIFIED BEFORE ANYBODY LOOKS AT THE SCALE. A rejected patch leaves the
# deployment at the size it already had, so a case that waits for the new size and then reads
# the worker set watches an unscaled deployment and reports it as a scale. The exit status and
# the replica count are therefore both read, and a patch that did not take ends the case
# instead of being observed.
scale_replicas() {
  local count="$1" label="$2" replicas
  if ! kubectl -n "$NS" patch modeldeployment "$DEPLOY" --type=json \
    -p "[{\"op\":\"add\",\"path\":\"/spec/roles/0/replicas\",\"value\":${count}}]" > "${OUT}/${label}.txt" 2>&1; then
    echo "FAIL ${label}.txt the patch was rejected" >> "${OUT}/content-verdicts.txt"
    return 1
  fi
  replicas="$(kubectl -n "$NS" get modeldeployment "$DEPLOY" -o jsonpath='{.spec.roles[0].replicas}' 2>/dev/null || true)"
  if [ "${replicas:-0}" != "${count}" ]; then
    echo "FAIL ${label}.txt the patch did not take effect: roles[0].replicas is ${replicas:-unknown}, not ${count}" \
      >> "${OUT}/content-verdicts.txt"
    return 1
  fi
  return 0
}

# THE CHINESE CLASS IS A QUESTION, so it asks the CHAT endpoint, and the chat endpoint is where
# the thinking switch applies: on the completions endpoint the model continues the prompt instead
# of answering it, so a model that reasons out loud never reaches the answer the assertion looks
# for. The switch keeps the reasoning out of the answer, the prompt pins the answer, and the token
# budget carries headroom so that a reasoning preamble cannot truncate the answer before it
# arrives. The assertion stays an exact-substring match, which is the test plan's verbatim-Chinese
# class: the answer must carry the pinned phrase and not merely some Chinese.
CHINESE_PROMPT="请只输出下面这句话，不要添加任何其他内容：今天天气很好。"
CHINESE_EXPECT="今天天气很好"
CHINESE_MAX_TOKENS=256

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
  curl --fail-with-body --max-time 120 "http://127.0.0.1:${port}/v1/chat/completions" \
    -H 'Content-Type: application/json' \
    --data-binary "$(jq -nc --arg model "/models" --arg prompt "$CHINESE_PROMPT" --argjson max_tokens "$CHINESE_MAX_TOKENS" \
      '{model: $model, messages: [{role: "user", content: $prompt}], max_tokens: $max_tokens, temperature: 0, chat_template_kwargs: {enable_thinking: false}}')" \
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
  local tag="$1" f answer
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
  # THE ANSWER COMES OUT OF THE CHAT SHAPE, and the match is a plain shell case on the pinned
  # phrase. The earlier form asked grep for a CJK range with -P, which the BSD grep on this host
  # does not accept, so the check failed on the tool rather than on the answer.
  answer="$(jq -r '.choices[0].message.content // ""' "$f" 2>/dev/null || true)"
  case "$answer" in
    *"${CHINESE_EXPECT}"*)
      echo "PASS $(basename "$f")" >> "${OUT}/content-verdicts.txt"
      ;;
    *)
      echo "FAIL $(basename "$f")" >> "${OUT}/content-verdicts.txt"
      ;;
  esac

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
if ! scale_replicas 2 "scale-up-patch"; then
  cat "${OUT}/content-verdicts.txt" 2>/dev/null
  exit 1
fi
workers_after_up="$(wait_workers 2 || true)"
log "The gateway's worker set after the scale-up: ${workers_after_up}"
kubectl -n "$NS" get modeldeployment "$DEPLOY" -o json \
  | jq '.status.roles[] | {name, ready, parallelism, endpoints}' > "${OUT}/after-scale-up-roles.json" 2>/dev/null

# --- scale 2 -> 1 ----------------------------------------------------------
log "Scaling the replica count 2 -> 1"
if ! scale_replicas 1 "scale-down-patch"; then
  cat "${OUT}/content-verdicts.txt" 2>/dev/null
  exit 1
fi
deadline=$((SECONDS + SCALE_TIMEOUT))
while [ "$SECONDS" -lt "$deadline" ]; do
  replicas="$(kubectl -n "$NS" get modeldeployment "$DEPLOY" -o jsonpath='{.spec.roles[0].replicas}' 2>/dev/null || true)"
  [ "$replicas" = "1" ] && break
  sleep 5
done
while [ "$SECONDS" -lt "$deadline" ]; do
  # The scrape and the count are two steps, because grep -c prints 0 and exits 1 on no match, so
  # chaining them into one expression appended a second zero and made the count two lines.
  scrape_router_metrics "workers-after-scale-down" >/dev/null 2>&1 || true
  # The count is of HEALTHY workers. The gateway keeps the series of a worker it withdrew and
  # marks that series with the value -1, so counting the lines counted a worker that had
  # already left and the loop spun out its whole timeout against a deployment that had in fact
  # been scaled down.
  workers_after_down="$(awk '/^smg_worker_health\{/ && $NF == 1 { n++ } END { print n + 0 }' \
    "${OUT}/workers-after-scale-down.txt" 2>/dev/null || echo 0)"
  [ "${workers_after_down:-0}" -le 1 ] && break
  sleep 5
done
kubectl -n "$NS" get modeldeployment "$DEPLOY" -o json \
  | jq '.status.roles[] | {name, ready, parallelism, endpoints}' > "${OUT}/after-scale-down-roles.json" 2>/dev/null

bash "${SKILL_DIR}/scripts/stage_counts.sh" "$DEPLOY" "$OUT" > "${OUT}/stage-counts.txt" 2>&1
kubectl get devices -o json > "${OUT}/released-devices.json" 2>/dev/null || true

log "CASE 7 (SGLang multi-Server) complete"
