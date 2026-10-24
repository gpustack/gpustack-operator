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
#              The shape this run was on is written to OUT_DIR/cluster-shape.txt: the context and
#              the cluster, the namespace, the nodes, the InstanceType the case asked for, the
#              accelerators on each node, the InstanceTypes the cluster carries, and the operator
#              image with its digest.
#              The cluster of record for this run: gpustack-sedp-0l578ap2 (ns gpustack-bench),
#              platform gpu-h100-sxm 8xH100 non-preemptible with a cpu-e2 system node, 8
#              accelerators on the one GPU node, InstanceType
#              gpustack--nvidia-h100-80gb-hbm3-linux-amd64, operator
#              thxcode/gpustack-operator:dev-6ddcf389.
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
# THE ROUTER'S NAME IS WRITTEN ONCE, because the manifest names the router and the Pod lookup
# below selects on the label the operator renders from that same name.
ROUTER_NAME="vllm-router"
ROUTER_IMAGE="${VS_ROUTER_IMAGE:-gpustack/llm-router:v0.3.1}"
INSTANCE_TYPE="${VS_INSTANCE_TYPE:-gpustack--nvidia-h100-80gb-hbm3-linux-amd64}"
LOCAL_PORT_BASE="${LOCAL_PORT_BASE:-18300}"
READY_TIMEOUT="${READY_TIMEOUT:-1800}"
# THE SCALE WAIT NEEDS A BOUND. Without one the wait for the Router's worker set has no deadline
# to reach, the wait returns nothing, and the cell sends its traffic into a Router that has not
# finished growing.
SCALE_TIMEOUT="${SCALE_TIMEOUT:-300}"
# The spread probe sends this many cheap requests so the pinned policy has enough traffic to name
# both replicas. Four requests could land on one replica and say nothing about the other.
SPREAD_REQUESTS="${SPREAD_REQUESTS:-8}"
PF_PIDS=""

mkdir -p "$OUT"
log() { echo "[$(date +%H:%M:%S)] $*"; }

# THE SHAPE OF THE CLUSTER BELONGS IN THE EVIDENCE, so the numbers this run produced can be
# read against the machine that produced them.
capture_cluster_shape "${OUT}/cluster-shape.txt"

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
    name: ${ROUTER_NAME}
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
#
# THE ROUTER POD IS FOUND BY THE LABELS THE OPERATOR RENDERS ON IT. The router is its own
# Deployment, and its Pod template carries the deployment's instance label and the router's own
# name label (pkg/worker/controllers/worker/model_deployment_router.go, the labels the router
# objects are rendered with). It carries no component label, so a selector that asks for one
# matches nothing.
router_pod() {
  kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${DEPLOY},modeldeployment.gpustack.ai/router=${ROUTER_NAME}" \
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

# scale_replicas COUNT LABEL patches the server role's replica count and verifies that it took.
#
# THE PATCH NAMES THE ONE FIELD IT CHANGES. A merge patch on spec.roles replaces the whole role
# list, so the role comes back carrying only the two fields the patch carried and the API server
# rejects the deployment for the role it no longer describes. A JSON patch changes the one number
# and leaves the rest of the role alone.
#
# THE PATCH IS VERIFIED BEFORE ANYBODY LOOKS AT THE SCALE. A rejected patch leaves the deployment
# at the size it already had, so a case that waits for the new size and then reads the worker set
# watches an unscaled deployment and reports it as a scale. The exit status and the replica count
# are therefore both read, and a patch that did not take ends the case.
scale_replicas() {
  local count="$1" label="$2" replicas
  if ! kubectl -n "$NS" patch modeldeployment "$DEPLOY" --type=json \
    -p "[{\"op\":\"add\",\"path\":\"/spec/roles/0/replicas\",\"value\":${count}}]" > "${OUT}/${label}.txt" 2>&1; then
    record FAIL "the ${label} was accepted" \
      "the patch was rejected: $(tr '\n' ' ' < "${OUT}/${label}.txt" 2>/dev/null)"
    return 1
  fi
  replicas="$(kubectl -n "$NS" get modeldeployment "$DEPLOY" -o jsonpath='{.spec.roles[0].replicas}' 2>/dev/null || true)"
  if [ "${replicas:-0}" != "${count}" ]; then
    record FAIL "the ${label} was accepted" \
      "the patch did not take effect: roles[0].replicas is ${replicas:-unknown}, not ${count}"
    return 1
  fi
  record PASS "the ${label} was accepted" "roles[0].replicas is ${count}"
  return 0
}

# router_active_workers FILE prints the number of workers the Router's own gauge holds.
#
# THE GAUGE IS THE WORKER SET. The vLLM router exposes vllm_router_active_workers from the moment
# it starts, while the per-worker series it also exposes only appear once a worker has served a
# request. The gauge is therefore the series that answers "how many workers does the Router
# hold", and it is the series that shrinks when a worker is withdrawn.
router_active_workers() {
  local file="$1"
  awk '/^vllm_router_active_workers / { print $2; found = 1 } END { if (!found) exit 1 }' \
    "$file" 2>/dev/null || echo 0
}

# router_healthy_workers FILE prints the number of workers the Router names whose circuit
# breaker is closed. A worker is named by the per-worker request counter, and its health is
# stated by the per-worker circuit-breaker series: the state is 0 for a closed breaker and
# non-zero for an open one. A worker that has never served a request is not named, so it is
# not counted as healthy either.
router_healthy_workers() {
  local file="$1"
  awk '
    /^vllm_router_processed_requests_total\{/ {
      match($0, /worker="[^"]*"/)
      if (RSTART > 0) { w = substr($0, RSTART + 8, RLENGTH - 9); seen[w] = 1 }
    }
    /^vllm_router_cb_state\{/ {
      match($0, /worker="[^"]*"/)
      if (RSTART > 0) { w = substr($0, RSTART + 8, RLENGTH - 9); state[w] = $NF + 0 }
    }
    END { n = 0; for (w in seen) if (state[w] == 0) n++; print n + 0 }' "$file" 2>/dev/null || echo 0
}

# router_workers_that_grew BEFORE AFTER prints the workers whose request count grew between two
# scrapes, and by how much.
#
# THE GROWTH IS THE PROOF THAT A WORKER SERVED. The per-worker request counters are cumulative
# and the Router keeps the series of a worker it withdrew, so the number of workers NAMED says
# nothing about who is serving now. A worker whose count grew inside the window is a worker
# that served inside the window, and that is what the scaling leg has to show.
router_workers_that_grew() {
  local before="$1" after="$2"
  awk '
    FNR == NR {
      if ($0 ~ /^vllm_router_processed_requests_total\{/) {
        match($0, /worker="[^"]*"/)
        if (RSTART > 0) { w = substr($0, RSTART + 8, RLENGTH - 9); before[w] = $NF + 0 }
      }
      next
    }
    /^vllm_router_processed_requests_total\{/ {
      match($0, /worker="[^"]*"/)
      if (RSTART > 0) {
        w = substr($0, RSTART + 8, RLENGTH - 9)
        now = $NF + 0
        was = (w in before) ? before[w] : 0
        if (now > was) grew[w] = now - was
      }
    }
    END { for (w in grew) print w, grew[w] }' "$before" "$after" 2>/dev/null | sort
}

# wait_active_workers DIRECTION COUNT waits for the Router's worker set to reach COUNT.
#
# DIRECTION is "atleast" or "atmost". The set is read from the Router's own gauge, because the
# per-worker request counters are cumulative and keep naming a worker the Router has withdrawn.
wait_active_workers() {
  local direction="$1" want="$2" tag="$3" deadline=$((SECONDS + SCALE_TIMEOUT)) got=0
  while [ "$SECONDS" -lt "$deadline" ]; do
    scrape_router_metrics "router-workers-${tag}" >/dev/null 2>&1 || true
    got="$(router_active_workers "${OUT}/router-workers-${tag}.txt")"
    if [ "$direction" = "atleast" ] && [ "${got:-0}" -ge "$want" ]; then
      printf '%s' "$got"
      return 0
    fi
    if [ "$direction" = "atmost" ] && [ "${got:-0}" -le "$want" ]; then
      printf '%s' "$got"
      return 0
    fi
    sleep 5
  done
  printf '%s' "${got:-0}"
  return 1
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
#
# THE FRAMES ARE NOT WRAPPED IN AN ARRAY. The brackets collected the whole list into ONE value,
# and every reader of that value then indexed an array with a field name, which jq refuses. Each
# reader therefore answered empty while the stream itself was whole, and the case reported a
# broken stream that the router had served correctly. One frame per line is what the readers
# below slurp back, so each of them gets one frame per element.
#
# THE FRAME IS A CHAT COMPLETION CHUNK. The router answers a chat completion, so the answer
# arrives as choices[0].delta.content, and the last frame of the stream carries no choice at
# all, only the usage. The readers below therefore read every field as absent when it is
# absent, instead of failing on the frame that has none.
sse_chunks() {
  jq -R -s -c '
    split("\n")
      | map(select(startswith("data: ")))
      | map(sub("^data: "; ""))
      | map(select(. != "[DONE]"))
      | map(fromjson? // empty)
      | .[]' "$1" 2>/dev/null
}

# sse_content_chunks FILE counts the SSE chunks that carry a content delta.
sse_content_chunks() {
  sse_chunks "$1" | jq -s '[.[] | select((.choices[0].delta.content? // "") != "")] | length' 2>/dev/null || echo 0
}

# sse_text FILE prints the content the SSE deltas carry, reassembled.
sse_text() {
  sse_chunks "$1" | jq -rs '[.[] | (.choices[0].delta.content? // "")] | join("")' 2>/dev/null || true
}

# sse_finish_reason FILE prints the finish reason the stream ended with.
sse_finish_reason() {
  sse_chunks "$1" | jq -rs '[.[] | (.choices[0].finish_reason? // empty)] | map(select(. != "")) | last // ""' 2>/dev/null || true
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

# THE TOKEN BUDGET CARRIES HEADROOM, because the answer must arrive whatever the thinking switch
# did. The switch is pinned on every request, but a model that reasons out loud puts that
# reasoning in its own field and answers after it, so a budget that fits the answer alone cuts the
# answer off before it is written. The switch is recorded as its own row below and must not fail
# the case, so the budget cannot let it fail the case either.
CONTENT_MAX_TOKENS=256

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
request_body "$ARITHMETIC_PROMPT" "$CONTENT_MAX_TOKENS" > "${OUT}/body-arithmetic.json"
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

request_body "$ARITHMETIC_PROMPT" "$CONTENT_MAX_TOKENS" '{"stream":true,"stream_options":{"include_usage":true}}' > "${OUT}/body-sse.json"
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

request_body "$CHINESE_PROMPT" "$CONTENT_MAX_TOKENS" > "${OUT}/body-chinese.json"
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

request_body "$JSON_PROMPT" "$CONTENT_MAX_TOKENS" '{"response_format":{"type":"json_object"}}' > "${OUT}/body-json.json"
chat_request "${LOCAL_PORT_BASE}" "${OUT}/body-json.json" "${OUT}/router-json.json"
answer="$(answer_of "${OUT}/router-json.json")"
# THE ANSWER TEXT IS READ OUT OF THE ENVELOPE AND PARSED AFTERWARDS. The router answers a chat
# completion, so the JSON the model produced is a document inside choices[0].message.content and
# not the response itself. Handing that text to jq as its input makes jq read the text as the
# document, and fromjson then refuses to parse a document that is already an object. So the
# field goes through jq's filter first and the answer is parsed second, the way case-6 reads
# the field of a completion.
if jq -e '.choices[0].message.content | fromjson | type == "object" and ((.name // "") | ascii_downcase) == "gpu"' \
  "${OUT}/router-json.json" >/dev/null 2>&1; then
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
    "the counter ${ROUTER_SERIES} moved from $(counter_before_display "$ROUTER_SERIES" "$ROUTER_BEFORE" "${OUT}/router-metrics-before.txt") to ${ROUTER_AFTER} over ${SENT} requests"
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

# --- the manual scaling leg --------------------------------------------------------------
#
# THE LEG IS OPTIONAL, and the gate is off by default. The content classes above are this
# case's standing contract; the manual 1 -> 2 -> 1 cycle is a cell of its own, and it runs the
# same case with VS_MANUAL_SCALE=1.
#
# THE CYCLE STARTS BY GOING DOWN TO ONE. The deployment is rendered with two replicas, so the
# cycle's first move is the scale back to one, and the two moves after it are the scale up to
# two and the scale back to one again.
if [ "${VS_MANUAL_SCALE:-0}" = "1" ]; then
  log "The manual scaling leg: one -> two -> one"

  if ! scale_replicas 1 "scale-down-to-one-patch"; then
    stop_forwards
    cat "${OUT}/content-verdicts.txt" 2>/dev/null
    exit 1
  fi
  ACTIVE_ONE="$(wait_active_workers atmost 1 "down-one" || true)"
  log "The Router's worker set after the scale down to one: ${ACTIVE_ONE}"
  if [ "${ACTIVE_ONE:-0}" -le 1 ]; then
    record PASS "the Router's worker set came down to one" \
      "the Router's gauge holds ${ACTIVE_ONE} worker(s)"
  else
    record FAIL "the Router's worker set came down to one" \
      "the Router's gauge still holds ${ACTIVE_ONE:-0} worker(s)"
  fi

  if ! scale_replicas 2 "scale-up-to-two-patch"; then
    stop_forwards
    cat "${OUT}/content-verdicts.txt" 2>/dev/null
    exit 1
  fi
  ACTIVE_TWO="$(wait_active_workers atleast 2 "up-two" || true)"
  log "The Router's worker set after the scale up to two: ${ACTIVE_TWO}"
  if [ "${ACTIVE_TWO:-0}" -ge 2 ]; then
    record PASS "the Router's worker set grew to two" \
      "the Router's gauge holds ${ACTIVE_TWO} worker(s)"
  else
    record FAIL "the Router's worker set grew to two" \
      "the Router's gauge holds ${ACTIVE_TWO:-0} worker(s)"
  fi

  # THE REQUEST IS SERVED ACROSS BOTH WORKERS. The counters are read before the traffic and
  # again after it, and the workers whose count GREW inside that window are the workers that
  # served inside it. The Router keeps the series of a worker it withdrew, so the number of
  # workers named says nothing about who served.
  scrape_router_metrics "router-metrics-before-scale-traffic" || true
  SCALE_REQUESTS="${VS_SCALE_REQUESTS:-16}"
  log "Sending ${SCALE_REQUESTS} requests through the Router at two replicas"
  index=1
  while [ "$index" -le "$SCALE_REQUESTS" ]; do
    body="$(jq -nc --arg model "/models" --arg i "$index" \
      '{model: $model, messages: [{role: "user", content: ("Reply with the word blue only. Scale check " + $i + ".")}], max_tokens: 8, temperature: 0}')"
    curl -sf --max-time 120 "http://127.0.0.1:${LOCAL_PORT_BASE}/v1/chat/completions" \
      -H 'Content-Type: application/json' --data-binary "$body" \
      -o "${OUT}/scale-spread-${index}.json" 2>"${OUT}/scale-spread-${index}.err" || true
    index=$((index + 1))
  done
  scrape_router_metrics "router-metrics-after-scale-traffic" || true
  GREW="$(router_workers_that_grew "${OUT}/router-metrics-before-scale-traffic.txt" \
    "${OUT}/router-metrics-after-scale-traffic.txt")"
  GREW_COUNT="$(printf '%s' "$GREW" | grep -c . || true)"
  log "The workers that served inside the window: $(printf '%s' "$GREW" | tr '\n' ' ')"
  if [ "${GREW_COUNT:-0}" -ge 2 ]; then
    record PASS "a request was served across both workers" \
      "the workers whose request count grew are: $(printf '%s' "$GREW" | tr '\n' ' ')"
  else
    record FAIL "a request was served across both workers" \
      "the workers whose request count grew are: $(printf '%s' "$GREW" | tr '\n' ' ')"
  fi

  HEALTHY_TWO="$(router_healthy_workers "${OUT}/router-metrics-after-scale-traffic.txt")"
  if [ "${HEALTHY_TWO:-0}" -ge 2 ]; then
    record PASS "both workers were healthy at two replicas" \
      "${HEALTHY_TWO} worker(s) are named with a closed circuit breaker"
  else
    record FAIL "both workers were healthy at two replicas" \
      "${HEALTHY_TWO:-0} worker(s) are named with a closed circuit breaker"
  fi

  if ! scale_replicas 1 "scale-down-to-one-patch"; then
    stop_forwards
    cat "${OUT}/content-verdicts.txt" 2>/dev/null
    exit 1
  fi
  ACTIVE_ONE_AGAIN="$(wait_active_workers atmost 1 "down-one-again" || true)"
  log "The Router's worker set after the scale back to one: ${ACTIVE_ONE_AGAIN}"
  if [ "${ACTIVE_ONE_AGAIN:-0}" -le 1 ]; then
    record PASS "the Router's worker set came back to one" \
      "the Router's gauge holds ${ACTIVE_ONE_AGAIN} worker(s)"
  else
    record FAIL "the Router's worker set came back to one" \
      "the Router's gauge still holds ${ACTIVE_ONE_AGAIN:-0} worker(s)"
  fi

  # THE ROUTER STILL SERVES AFTER THE SCALE DOWN. The answer has to come back, and the counter
  # the Router keeps for its own requests has to move.
  ROUTER_BEFORE_LAST="$(serving_sum "$(cat "${OUT}/router-metrics-after-scale-traffic.txt" 2>/dev/null || true)" "$ROUTER_SERIES" || echo 0)"
  body="$(jq -nc --arg model "/models" \
    '{model: $model, messages: [{role: "user", content: "Reply with the word blue only. After the scale."}], max_tokens: 8, temperature: 0}')"
  curl -sf --max-time 120 "http://127.0.0.1:${LOCAL_PORT_BASE}/v1/chat/completions" \
    -H 'Content-Type: application/json' --data-binary "$body" \
    -o "${OUT}/after-scale-request.json" 2>"${OUT}/after-scale-request.err" || true
  scrape_router_metrics "router-metrics-after-final-request" || true
  ROUTER_AFTER_LAST="$(serving_sum "$(cat "${OUT}/router-metrics-after-final-request.txt" 2>/dev/null || true)" "$ROUTER_SERIES" || echo 0)"
  FINAL_ANSWER="$(answer_of "${OUT}/after-scale-request.json" 2>/dev/null || true)"
  if [ -n "${FINAL_ANSWER}" ] && [ "${ROUTER_AFTER_LAST:-0}" -gt "${ROUTER_BEFORE_LAST:-0}" ]; then
    record PASS "the Router still served after the scale back to one" \
      "the counter ${ROUTER_SERIES} moved from $(counter_before_display "$ROUTER_SERIES" "$ROUTER_BEFORE_LAST" "${OUT}/router-metrics-after-scale-traffic.txt") to ${ROUTER_AFTER_LAST} and the answer was: ${FINAL_ANSWER}"
  else
    record FAIL "the Router still served after the scale back to one" \
      "the counter ${ROUTER_SERIES} moved from ${ROUTER_BEFORE_LAST} to ${ROUTER_AFTER_LAST} and the answer was: ${FINAL_ANSWER:-none}"
  fi

  kubectl -n "$NS" get modeldeployment "$DEPLOY" -o json \
    | jq '.status.roles[] | {name, ready, parallelism, endpoints}' > "${OUT}/roles-after-scaling.json" 2>/dev/null || true
fi

stop_forwards
cat "${OUT}/content-verdicts.txt" 2>/dev/null

FAILS="$(grep -c '^FAIL' "${OUT}/content-verdicts.txt" 2>/dev/null || true)"
WARNS="$(grep -c '^WARN' "${OUT}/content-verdicts.txt" 2>/dev/null || true)"
log "CASE 8 (vLLM multi-Server content) complete: ${FAILS:-0} failed row(s), ${WARNS:-0} warning(s)"
# THE SCRIPT ENDS HERE, and the last statement is the exit code. An explicit trailing "exit 0"
# makes the analyzer read the EXIT trap's handler as a function nothing calls, and it then reports
# the whole handler as unreachable.
[ "${FAILS:-0}" -eq 0 ] || exit 1
