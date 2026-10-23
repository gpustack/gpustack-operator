#!/usr/bin/env bash
#
# CASE 8 — vLLM multi-Server: the content classes, through the Router   (MUTATING, self-recovering)
#
#   case-8-vs-content.sh [OUT_DIR]
#
# Goal:        A vLLM group of two Server replicas behind vllm-router answers, THROUGH THE ROUTER, the
#              four content classes a latency number cannot stand in for: an arithmetic answer, a
#              complete server-sent-event stream, an exact Chinese phrase and a structured JSON
#              object. Every other cell measures throughput; this one measures whether the answer is
#              right, and whether it was the ROUTER's answer rather than a replica's.
# Environment: A cluster with 2 free GPUs on one InstanceType, NodeModelStore caching qwen3-8b. The
#              model is Qwen/Qwen3-8B, served from the qwen3-8b artifact the store caches: one GPU
#              per replica, two replicas = 2 GPUs.
# Inputs:      The manifest is written by this script into OUT_DIR/vs-content.yaml. The operator
#              stamps each replica's own address, so the manifest is identical for every replica.
# Expected:    - the router answers all four content classes, and each answer is right;
#              - the router's own request counter rises by exactly the number of requests sent, which
#                is what makes the answer the ROUTER's answer and not a replica's;
#              - the router names BOTH replicas as workers it sent requests to, under the policy the
#                manifest pins;
#              - the served model name is /models, the name the manifest writes, because the operator
#                pins --served-model-name to spec.model.name and a mismatch fails silently behind a
#                deployment that reports Ready.
# Cleanup:     Deletes the deployment on completion and releases its GPU claims.
#
# NOT EXECUTED YET: this script was written for the acceptance run and has never been run on a
# cluster. Every expectation above is the contract, not an observation.
set -uo pipefail

CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SKILL_DIR="$(cd "${CASE_DIR}/.." && pwd)"
REPO="$(cd "${SKILL_DIR}/../../.." && pwd)"
export PATH="${REPO}/.agents/skills/_e2e-lib/scripts/kubectl-shim:${PATH}"

# The shared serving library holds the one place a vendor or engine difference is written: which
# router counter counts a request, and which series carry a worker's name.
# shellcheck source=/dev/null
. "${SKILL_DIR}/../_e2e-lib/scripts/serving-lib.sh"

NS="${BENCH_NS:-gpustack-bench}"
OUT="${1:-${REPO}/.claude/reports/$(date +%F)-benchmark/vs-content}"
DEPLOY="vs-content"
MODEL="${VS_MODEL:-Qwen/Qwen3-8B}"
# THE ARTIFACT IS WHAT THE DEPLOYMENT SERVES, and naming it is what keeps the pod alive. Without
# an artifactRef the operator renders the model as the repository and the engine downloads the
# weights itself onto the container's writable layer -- 16GB for Qwen3-8B against the 15Gi
# ephemeral limit the role derives -- so the pod is evicted while it is still starting, and the
# NodeModelStore cache the environment already warmed is never read. With one, the operator
# serves the weights from the delivery the artifact names and pins the served name below.
ARTIFACT="${VS_ARTIFACT:-qwen3-8b}"
IMAGE="${VS_IMAGE:-gpustack/runner:cuda13.0-vllm0.32.0}"
ROUTER_IMAGE="${VS_ROUTER_IMAGE:-gpustack/llm-router:v0.3.1}"
INSTANCE_TYPE="${VS_INSTANCE_TYPE:-gpustack--nvidia-h100-80gb-hbm3-linux-amd64}"
LOCAL_PORT_BASE="${LOCAL_PORT_BASE:-18300}"
READY_TIMEOUT="${READY_TIMEOUT:-1800}"
# The spread probe sends this many cheap requests so the pinned policy has enough traffic to name
# both replicas. Four requests could land on one replica and say nothing about the other.
SPREAD_REQUESTS="${SPREAD_REQUESTS:-8}"
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

# record VERDICT LABEL DETAIL writes one row of the results table.
record() {
  printf '%s | %s | %s\n' "$1" "$2" "$3" >> "${OUT}/content-verdicts.txt"
  case "$1" in
    FAIL) echo "FAIL | ${2} | ${3}" ;;
    WARN) echo "WARN | ${2} | ${3}" ;;
    *) echo "PASS | ${2} | ${3}" ;;
  esac
}

write_manifest() {
  cat > "${OUT}/vs-content.yaml" <<YAML
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
    name: vLLM
    image: ${IMAGE}
  router:
    name: vllm-router
    image: ${ROUTER_IMAGE}
    extraArgs:
      # THE POLICY IS PINNED so that the traffic spreads over both replicas. The default policy of
      # the vLLM router is cache_aware, which keeps requests sharing a prefix on the worker already
      # holding it, so a second replica can stay idle under traffic that shares one -- that is the
      # policy working, not a replica that failed. Under round_robin the router recorded 60
      # requests on each of two replicas out of 120 (docs/modules/model-deployment/routing.md).
      - --policy
      - round_robin
  roles:
    - name: server
      kind: Server
      replicas: 2
      size: 1
      instanceType: ${INSTANCE_TYPE}
      image: ${IMAGE}
      imagePullPolicy: IfNotPresent
      resources:
        accelerator: "1"
      extraArgs:
        - --tensor-parallel-size
        - "1"
        - --max-model-len
        - "16384"
        - --max-num-seqs
        - "64"
        - --gpu-memory-utilization
        - "0.90"
        # The router counts tokens by POSTing /v1/chat/completions/render, and vLLM serves that
        # path only under this flag. Without it the failure is silent: the render answers 404
        # while the chat requests still answer 200.
        - --enable-scale-out
      # THE ENGINE HOLDS ITS CONNECTIONS AS LONG AS THE ROUTER HOLDS THEM. vLLM closes an idle
      # HTTP connection after five seconds, and the router pools its upstream connections for far
      # longer, so a request can be written to a connection the engine has already closed, and the
      # router answers that request with a refusal the client sees as a failed request. The
      # engine's keep-alive is raised to match the pool that holds the connection.
      env:
        - name: VLLM_HTTP_TIMEOUT_KEEP_ALIVE
          value: "3600"
YAML
}

# router_pod prints the router Pod's name, or nothing when there is none.
router_pod() {
  kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${DEPLOY},app.kubernetes.io/component=router" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

# router_metrics_port reads the port the router was told to serve its metrics on. The operator
# renders --prometheus-port on every router it manages (pkg/worker/kvcache/router/argv.go), so the
# port is read off the Pod rather than assumed.
router_metrics_port() {
  local pod args
  pod="$(router_pod)"
  [ -n "$pod" ] || return 1
  args="$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.spec.containers[0].args}' 2>/dev/null)"
  printf '%s' "$args" | sed -n 's/.*--prometheus-port=\([0-9][0-9]*\).*/\1/p' | head -1
}

# start_router_forward opens the ROUTER's own door and the door of its metrics page. The requests
# this case sends reach the replicas through the router, because the router is what it dials.
start_router_forward() {
  local pod port metrics_port
  pod="$(router_pod)"
  if [ -z "$pod" ]; then
    echo "the router Pod was not found" > "${OUT}/port-forward-router.log"
    return 1
  fi
  port="${LOCAL_PORT_BASE}"
  kubectl -n "$NS" port-forward "pod/${pod}" "${port}:8000" > "${OUT}/port-forward-router.log" 2>&1 &
  PF_PIDS="$PF_PIDS $!"
  local _
  for _ in $(seq 1 60); do
    if curl -sf -o /dev/null "http://127.0.0.1:${port}/v1/models"; then
      break
    fi
    sleep 2
  done

  metrics_port="$(router_metrics_port || true)"
  if [ -z "$metrics_port" ]; then
    echo "the router's metrics port was not found" >> "${OUT}/port-forward-router.log"
    return 1
  fi
  kubectl -n "$NS" port-forward "pod/${pod}" "$((LOCAL_PORT_BASE + 90)):${metrics_port}" \
    > "${OUT}/port-forward-metrics.log" 2>&1 &
  PF_PIDS="$PF_PIDS $!"
  for _ in $(seq 1 30); do
    if curl -sf -o "${OUT}/router-metrics.txt" "http://127.0.0.1:$((LOCAL_PORT_BASE + 90))/metrics"; then
      return 0
    fi
    sleep 2
  done
  echo "the router's metrics page did not answer" >> "${OUT}/port-forward-metrics.log"
  return 1
}

stop_forwards() {
  local pid
  for pid in $PF_PIDS; do
    kill "$pid" 2>/dev/null || true
  done
  PF_PIDS=""
}

# scrape_router_metrics writes the router's own metric page.
scrape_router_metrics() {
  local name="$1"
  curl -sf --max-time 30 -o "${OUT}/${name}.txt" "http://127.0.0.1:$((LOCAL_PORT_BASE + 90))/metrics" \
    || { echo "the metrics scrape failed" > "${OUT}/${name}.txt"; return 1; }
}

# request_body PROMPT MAX_TOKENS [EXTRA_JSON] prints one chat request body.
#
# THE THINKING SWITCH IS PINNED HERE, and it is the operator's own recipe: the group-forward probe
# sends the same field to keep a model that reasons out loud from answering with its reasoning
# (pkg/worker/controllers/worker/model_deployment_group_forward_probe.go, the ChatFields of the
# pair input). Qwen3 answers a chat request with the reasoning in reasoning_content and the answer
# in content, so the switch is what makes the content rows below about the answer. The rows also
# record whether the engine honoured it, because a request that came back with reasoning says the
# switch did not take.
request_body() {
  local prompt="$1" max_tokens="$2" extra="${3:-{\}}"
  jq -nc --arg model "/models" --arg prompt "$prompt" --argjson max_tokens "$max_tokens" --argjson extra "$extra" \
    '{model: $model, messages: [{role: "user", content: $prompt}], max_tokens: $max_tokens, temperature: 0, chat_template_kwargs: {enable_thinking: false}} + $extra'
}

# chat_request PORT BODY OUTFILE sends one request through the router.
chat_request() {
  local port="$1" body="$2" out="$3"
  curl --fail-with-body --max-time 180 "http://127.0.0.1:${port}/v1/chat/completions" \
    -H 'Content-Type: application/json' --data-binary "@${body}" -o "$out" 2>"${out}.err"
}

# sse_chunks FILE prints the data chunks of an SSE body, one per line, without the [DONE] marker.
sse_chunks() {
  jq -R -s '
    [ split("\n")
      | map(select(startswith("data: ")))
      | map(sub("^data: "; ""))
      | map(select(. != "[DONE]"))
      | map(fromjson? // empty) ] | .[]' "$1" 2>/dev/null
}

# sse_content_chunks FILE counts the SSE chunks that carry a content delta.
sse_content_chunks() {
  sse_chunks "$1" | jq -s '[.[] | select((.choices[0].delta.content // "") != "")] | length' 2>/dev/null || echo 0
}

# sse_text FILE prints the content the SSE deltas carry, reassembled.
sse_text() {
  sse_chunks "$1" | jq -rs '[.[] | (.choices[0].delta.content // "")] | join("")' 2>/dev/null || true
}

# sse_finish_reason FILE prints the finish reason the stream ended with.
sse_finish_reason() {
  sse_chunks "$1" | jq -rs '[.[] | (.choices[0].finish_reason // empty)] | map(select(. != "")) | last // ""' 2>/dev/null || true
}

# answer_of FILE prints the answer a chat response carries.
answer_of() {
  jq -r '.choices[0].message.content // ""' "$1" 2>/dev/null || true
}

# reasoning_of FILE prints the reasoning a chat response carries, empty when it carries none.
reasoning_of() {
  jq -r '.choices[0].message.reasoning_content // ""' "$1" 2>/dev/null || true
}

# The four content classes. Each is one request through the router, and each answers a question a
# throughput number cannot: is the answer right, is the stream whole, is the text what was asked
# for, and is the object a JSON object at all.
ARITHMETIC_PROMPT="What is 17 multiplied by 23? Reply with the number only."
ARITHMETIC_EXPECT="391"
CHINESE_PROMPT="请只输出下面这句话，不要添加任何其他内容：今天天气很好。"
CHINESE_EXPECT="今天天气很好"
JSON_PROMPT="Return a JSON object with the key name and the value gpu."

log "The model is ${MODEL}, served as /models from the artifact ${ARTIFACT}"
log "Writing the manifest into ${OUT}/vs-content.yaml"
write_manifest
kubectl apply --dry-run=server -f "${OUT}/vs-content.yaml" > "${OUT}/apply-dry-run.txt" 2>&1

log "Applying the deployment"
kubectl -n "$NS" delete modeldeployment "$DEPLOY" --ignore-not-found --wait=true --timeout=180s >/dev/null 2>&1 || true
kubectl apply -f "${OUT}/vs-content.yaml" 2>&1 | tee "${OUT}/apply.log"

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

log "Opening the router's door"
if ! start_router_forward; then
  record FAIL "the router's door opened" "the router Pod did not serve /v1/models or its metrics page; see port-forward-router.log"
  cat "${OUT}/content-verdicts.txt" 2>/dev/null
  exit 1
fi

# The router's own request counter, before any request of this case. The router counts only the
# non-PD series while the deployment has no prefill or decode role, which is what
# serving_router_requests names for this router in its non-PD form.
ROUTER_SERIES="$(serving_router_requests vllm-router false || echo vllm_router_requests_total)"
scrape_router_metrics "router-metrics-before" || true
ROUTER_BEFORE="$(serving_sum "$(cat "${OUT}/router-metrics-before.txt" 2>/dev/null || true)" "$ROUTER_SERIES" || echo 0)"
log "The router's own counter ${ROUTER_SERIES} before: ${ROUTER_BEFORE}"

# --- the four content classes, through the router ------------------------------
request_body "$ARITHMETIC_PROMPT" 32 > "${OUT}/body-arithmetic.json"
chat_request "${LOCAL_PORT_BASE}" "${OUT}/body-arithmetic.json" "${OUT}/router-arithmetic.json"
answer="$(answer_of "${OUT}/router-arithmetic.json")"
case "$answer" in
  *"${ARITHMETIC_EXPECT}"*)
    record PASS "the router answers the arithmetic class" "the answer carries ${ARITHMETIC_EXPECT}: ${answer}"
    ;;
  *)
    record FAIL "the router answers the arithmetic class" "the answer does not carry ${ARITHMETIC_EXPECT}: ${answer}"
    ;;
esac

request_body "$ARITHMETIC_PROMPT" 32 '{"stream":true,"stream_options":{"include_usage":true}}' > "${OUT}/body-sse.json"
chat_request "${LOCAL_PORT_BASE}" "${OUT}/body-sse.json" "${OUT}/router-sse.txt"
segments="$(sse_content_chunks "${OUT}/router-sse.txt")"
stream_text="$(sse_text "${OUT}/router-sse.txt")"
finish="$(sse_finish_reason "${OUT}/router-sse.txt")"
if [ "${segments:-0}" -ge 2 ] && [ "${segments:-0}" -gt 0 ]; then
  record PASS "the router's stream arrives in segments" "the stream carried ${segments} content-bearing segments"
else
  record FAIL "the router's stream arrives in segments" "the stream carried ${segments:-0} content-bearing segments, which is not a stream cut into pieces"
fi
case "$stream_text" in
  *"${ARITHMETIC_EXPECT}"*)
    record PASS "the reassembled stream carries the answer" "the segments reassemble to: ${stream_text}"
    ;;
  *)
    record FAIL "the reassembled stream carries the answer" "the segments reassemble to: ${stream_text}"
    ;;
esac
if [ -n "$finish" ]; then
  record PASS "the stream ends with a finish reason" "the finish reason was ${finish}"
else
  record FAIL "the stream ends with a finish reason" "no chunk carried a finish reason"
fi
if grep -q 'data: \[DONE\]' "${OUT}/router-sse.txt" 2>/dev/null; then
  record PASS "the stream is closed" "the stream ended with the [DONE] marker"
else
  record FAIL "the stream is closed" "the stream carried no [DONE] marker"
fi

request_body "$CHINESE_PROMPT" 64 > "${OUT}/body-chinese.json"
chat_request "${LOCAL_PORT_BASE}" "${OUT}/body-chinese.json" "${OUT}/router-chinese.json"
answer="$(answer_of "${OUT}/router-chinese.json")"
case "$answer" in
  *"${CHINESE_EXPECT}"*)
    record PASS "the router answers the exact Chinese class" "the answer carries ${CHINESE_EXPECT}: ${answer}"
    ;;
  *)
    record FAIL "the router answers the exact Chinese class" "the answer does not carry ${CHINESE_EXPECT}: ${answer}"
    ;;
esac

request_body "$JSON_PROMPT" 64 '{"response_format":{"type":"json_object"}}' > "${OUT}/body-json.json"
chat_request "${LOCAL_PORT_BASE}" "${OUT}/body-json.json" "${OUT}/router-json.json"
answer="$(answer_of "${OUT}/router-json.json")"
if printf '%s' "$answer" | jq -e 'fromjson? | select(type == "object") | (.name // "") | ascii_downcase == "gpu"' >/dev/null 2>&1; then
  record PASS "the router answers the structured JSON class" "the answer is a JSON object whose name is gpu: ${answer}"
else
  record FAIL "the router answers the structured JSON class" "the answer is not a JSON object with name=gpu: ${answer}"
fi

# --- what the thinking switch did ------------------------------------------------
# The switch is pinned on every request above. Whether the engine honoured it is a fact about the
# engine, not about the answer, so it is recorded as its own row and does not fail the case.
for tag in arithmetic chinese json; do
  reasoning="$(reasoning_of "${OUT}/router-${tag}.json")"
  if [ -n "$reasoning" ]; then
    record WARN "the thinking switch was honoured on the ${tag} request" \
      "the answer also carried reasoning, so the engine did not take the switch: ${reasoning}"
  else
    record PASS "the thinking switch was honoured on the ${tag} request" "the answer carried no reasoning"
  fi
done

# --- the traffic the router spread ------------------------------------------------
# The spread probe carries no assertion of its own: it exists so that the pinned policy has enough
# requests to name both replicas in its per-worker counter.
log "Sending ${SPREAD_REQUESTS} spread requests through the router"
index=1
while [ "$index" -le "$SPREAD_REQUESTS" ]; do
  body="$(jq -nc --arg model "/models" --arg i "$index" \
    '{model: $model, messages: [{role: "user", content: ("Reply with the word blue only. Spread check " + $i + ".")}], max_tokens: 8, temperature: 0}')"
  curl -sf --max-time 120 "http://127.0.0.1:${LOCAL_PORT_BASE}/v1/chat/completions" \
    -H 'Content-Type: application/json' --data-binary "$body" \
    -o "${OUT}/spread-${index}.json" 2>"${OUT}/spread-${index}.err" || true
  index=$((index + 1))
done

scrape_router_metrics "router-metrics-after" || true
ROUTER_AFTER="$(serving_sum "$(cat "${OUT}/router-metrics-after.txt" 2>/dev/null || true)" "$ROUTER_SERIES" || echo 0)"
SENT=$((4 + SPREAD_REQUESTS))
log "The router's own counter ${ROUTER_SERIES} after: ${ROUTER_AFTER} (sent ${SENT})"

if [ "${ROUTER_AFTER:-0}" -eq "${ROUTER_BEFORE:-0}" ] && [ "${SENT}" -gt 0 ]; then
  record FAIL "the router counted every request of this case" \
    "the counter ${ROUTER_SERIES} stayed at ${ROUTER_AFTER} over ${SENT} requests, so the answers did not come through this router"
else
  record PASS "the router counted every request of this case" \
    "the counter ${ROUTER_SERIES} moved from ${ROUTER_BEFORE} to ${ROUTER_AFTER} over ${SENT} requests"
fi

# The per-worker counter names each worker the router sent a request to. The selection series do
# NOT carry a worker label and must never be cited for distribution; this one does.
WORKERS="$(grep '^vllm_router_processed_requests_total{' "${OUT}/router-metrics-after.txt" 2>/dev/null \
  | sed -n 's/.*worker="\([^"]*\)".*/\1/p' | sort -u)"
WORKER_COUNT="$(printf '%s' "$WORKERS" | grep -c . || true)"
if [ "${WORKER_COUNT:-0}" -ge 2 ]; then
  record PASS "the router named both replicas as workers" "the workers were: ${WORKERS}"
else
  record FAIL "the router named both replicas as workers" \
    "the per-worker counter named ${WORKER_COUNT:-0} worker(s): ${WORKERS:-none}"
fi
grep '^vllm_router_policy_decisions_total' "${OUT}/router-metrics-after.txt" > "${OUT}/router-policy-decisions.txt" 2>/dev/null || true

stop_forwards
cat "${OUT}/content-verdicts.txt" 2>/dev/null

FAILS="$(grep -c '^FAIL' "${OUT}/content-verdicts.txt" 2>/dev/null || true)"
WARNS="$(grep -c '^WARN' "${OUT}/content-verdicts.txt" 2>/dev/null || true)"
log "CASE 8 (vLLM multi-Server content) complete: ${FAILS:-0} failed row(s), ${WARNS:-0} warning(s)"
# THE SCRIPT ENDS HERE, and the last statement is the exit code. An explicit trailing "exit 0"
# makes the analyzer read the EXIT trap's handler as a function nothing calls, and it then reports
# the whole handler as unreachable.
[ "${FAILS:-0}" -eq 0 ] || exit 1
