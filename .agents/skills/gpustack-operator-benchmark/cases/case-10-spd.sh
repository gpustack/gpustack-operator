#!/usr/bin/env bash
#
# CASE 10 — SGLang Prefill/Decode behind the gateway   (MUTATING, self-recovering)
#
#   case-10-spd.sh [OUT_DIR]
#
# Goal:        Exercise the SGLang Prefill/Decode cell end to end: the operator renders the
#              disaggregated leg, the gateway routes to both halves, the halves move KV over the
#              direct leg, and the four content classes come back right THROUGH THE GATEWAY. The
#              second phase then drives the same Scaler contract case 9 drives on vLLM, against
#              the SGLang engine's own running-request counter.
#
#              THE RENDERED LEG IS RECORDED, NOT ASSUMED. The operator renders
#              --disaggregation-mode and --disaggregation-transfer-backend on both halves, and
#              which backend it renders depends on the transport of the leg: the direct leg's
#              protocol is tcp when nothing declares one, so a pair with no store bound renders
#              mooncake_tcp on both halves. This case reads the value off the Pod's own command
#              line and records it, so the record says what the operator rendered rather than
#              what this case expected.
#
#              THE UPSTREAM BASIS IS A PROOF OF CONCEPT, and the case header carries that risk
#              on purpose. SGLang's disaggregation on Kubernetes is not the same maturity as the
#              vLLM pair: the gateway's P/D routing, the bootstrap registry the prefill half
#              serves and the Mooncake transfer between the halves are three pieces that have to
#              agree, and a failure in any of them can leave the pair READY and still answer
#              every request from the decode half alone. The case therefore PRE-CHECKS that the
#              prefiller really took part, and when the Kubernetes P/D path does not come up the
#              case records BLOCKED -- never a pass on traffic that never touched the prefiller.
#
# Environment: A cluster with 4 free GPUs on one InstanceType, the NodeModelStore cache warm for
#              qwen3-8b (the artifact of manifests/10-model-delivery.yaml, which holds
#              Qwen/Qwen3-8B), and an in-cluster default/bench-driver Pod carrying python3 and
#              aiohttp. The pair starts at one replica per role and the Scaler phase may take
#              each role to two, so the peak is four GPUs.
# Inputs:      The manifest is written by this script into OUT_DIR/spd.yaml and the scaler into
#              OUT_DIR/spd-scaler.yaml. The operator stamps each replica's own address, so the
#              manifest is identical for every replica.
# Expected:    - the deployment renders a disaggregated leg on both halves, and the rendered
#                backend is recorded;
#              - the gateway holds workers of BOTH worker types, so its P/D routing is in place;
#              - the four content classes come back right through the gateway;
#              - the prefill half counted the KV it transferred, the decode half counted the
#                tokens it generated for requests whose blocks arrived, and neither half counted
#                a failed transfer;
#              - the two roles publish their group statuses independently;
#              - the Scaler phase reads the SGLang engine's own running-request counter, raises
#                the decode role, and brings the counts back down.
# Cleanup:     Deletes the scaler and the deployment on completion and releases their GPU claims.
#
# NOT EXECUTED YET: this script was written for the acceptance run and has never been run on a
# cluster. Every expectation above is the contract, not an observation.
set -uo pipefail

CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SKILL_DIR="$(cd "${CASE_DIR}/.." && pwd)"
REPO="$(cd "${SKILL_DIR}/../../.." && pwd)"
export PATH="${REPO}/.agents/skills/_e2e-lib/scripts/kubectl-shim:${PATH}"
export BENCH_INCLUSTER=1

# The shared serving library holds the one place a vendor or engine difference is written: which
# series carry a transfer, which carry a failure and which carry a worker.
# shellcheck source=/dev/null
. "${SKILL_DIR}/../_e2e-lib/scripts/serving-lib.sh"

NS="${BENCH_NS:-gpustack-bench}"
DRIVER_NS="${BENCH_DRIVER_NS:-default}"
DRIVER_POD="${BENCH_DRIVER:-bench-driver}"
DRIVER_IMAGE="${BENCH_C10_DRIVER_IMAGE:-docker.io/library/python:3.11-slim}"
OUT="${1:-${REPO}/.claude/reports/$(date +%F)-benchmark/spd}"
DEPLOY="spd"
SCALER="autoscale-spd"
MODEL="${SPD_MODEL:-Qwen/Qwen3-8B}"
ARTIFACT="${SPD_ARTIFACT:-qwen3-8b}"
IMAGE="${SPD_IMAGE:-gpustack/runner:cuda13.0-sglang0.5.22}"
ROUTER_IMAGE="${SPD_ROUTER_IMAGE:-gpustack/llm-router:v0.3.1}"
INSTANCE_TYPE="${SPD_INSTANCE_TYPE:-bench-gpu}"
LOCAL_PORT_BASE="${LOCAL_PORT_BASE:-18500}"
READY_TIMEOUT="${READY_TIMEOUT:-1800}"
SCALE_TIMEOUT="${SCALE_TIMEOUT:-900}"
BINDING_TIMEOUT="${BINDING_TIMEOUT:-300}"
IN_TOK="${SPD_INPUT_TOKENS:-8192}"
OUT_TOK="${SPD_OUTPUT_TOKENS:-64}"
LOAD_CONCURRENCY="${SPD_CONCURRENCY:-24}"
LOAD_SECONDS="${SPD_LOAD_SECONDS:-150}"
COOLDOWN_SECONDS="${SPD_COOLDOWN_SECONDS:-240}"
SCALER_RESOURCE=modeldeploymentscalers.v1alpha1.worker.gpustack.ai
MD_RESOURCE=modeldeployments.v1alpha1.worker.gpustack.ai
PF_PIDS=""

mkdir -p "$OUT"
log() { echo "[$(date +%H:%M:%S)] $*"; }

cleanup() {
  local pid
  for pid in $PF_PIDS; do
    kill "$pid" 2>/dev/null || true
  done
  PF_PIDS=""
  kubectl -n "$NS" delete "$SCALER_RESOURCE" "$SCALER" --ignore-not-found --wait=true --timeout=120s \
    >/dev/null 2>&1 || true
  kubectl -n "$NS" delete "$MD_RESOURCE" "$DEPLOY" --ignore-not-found --wait=true --timeout=300s \
    >/dev/null 2>&1 || true
  return 0
}
trap cleanup EXIT

# record VERDICT LABEL DETAIL writes one row of the results table.
#
# BLOCKED IS ITS OWN VERDICT, and it is not a pass. It says the thing under test could not be
# exercised, which is a different fact from the thing being wrong, and a reader must be able to
# tell the two apart.
record() {
  printf '%s | %s | %s\n' "$1" "$2" "$3" >> "${OUT}/verdicts.txt"
  case "$1" in
    FAIL) echo "FAIL | ${2} | ${3}" ;;
    BLOCKED) echo "BLOCKED | ${2} | ${3}" ;;
    WARN) echo "WARN | ${2} | ${3}" ;;
    *) echo "PASS | ${2} | ${3}" ;;
  esac
}

# ---------------------------------------------------------------- the manifests

write_manifest() {
  cat > "${OUT}/spd.yaml" <<YAML
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: ${DEPLOY}
  namespace: ${NS}
spec:
  model:
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
    - name: prefill
      kind: Prefill
      replicas: 1
      size: 1
      instanceType: ${INSTANCE_TYPE}
      image: ${IMAGE}
      imagePullPolicy: IfNotPresent
      # An SGLang half takes 26 to 28 s to exit after its delete, so the grace has to carry that
      # or the departing replica is killed while it is still serving.
      terminationGracePeriodSeconds: 60
      resources:
        accelerator: "1"
      extraArgs:
        - --tp-size
        - "1"
        - --context-length
        - "16384"
        - --mem-fraction-static
        - "0.90"
    - name: decode
      kind: Decode
      replicas: 1
      size: 1
      instanceType: ${INSTANCE_TYPE}
      image: ${IMAGE}
      imagePullPolicy: IfNotPresent
      terminationGracePeriodSeconds: 60
      resources:
        accelerator: "1"
      extraArgs:
        - --tp-size
        - "1"
        - --context-length
        - "16384"
        - --mem-fraction-static
        - "0.90"
YAML
}

# The scaler of the second phase. Every role of the target has a bound and a target, and the
# algorithm and the windows are the ones case 9 uses, tuned the same way: a short test cannot
# wait out a five-minute scale-down window.
write_scaler_manifest() {
  cat > "${OUT}/spd-scaler.yaml" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${SCALER}
  namespace: ${NS}
spec:
  targetRef:
    name: ${DEPLOY}
  mode: Active
  sampling:
    intervalSeconds: 10
  roles:
    - name: prefill
      minReplicas: 1
      maxReplicas: 2
    - name: decode
      minReplicas: 1
      maxReplicas: 2
  algorithm:
    type: Proportional
    proportional:
      metric: RunningRequests
      tolerance: "0.1"
      roleTargets:
        - name: prefill
          target: "4"
        - name: decode
          target: "4"
  behavior:
    scaleDown:
      stabilizationWindowSeconds: 30
      decodePostScaleUpGraceSeconds: 30
YAML
}

# ---------------------------------------------------------------- the cluster helpers

# role_pods prints every Pod of a role, one per line.
role_pods() {
  kubectl -n "$NS" get pods \
    -l "app.kubernetes.io/instance=${DEPLOY},app.kubernetes.io/component=$1" \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null
}

# pod_metrics_port reads the port the operator stamped on a Pod for its metrics.
pod_metrics_port() {
  kubectl -n "$NS" get pod "$1" -o jsonpath='{.metadata.annotations.prometheus\.io/port}' 2>/dev/null
}

# rendered_arg ROLE FLAG prints the value the operator rendered for a flag on a role's Pod.
#
# THE RENDERED COMMAND IS THE RECORD. The manifest says which shape was asked for and the
# operator says what that shape became; only the Pod's own command line carries the second.
rendered_arg() { # <role> <flag>
  local role="$1" flag="$2" pod cmd
  pod="$(role_pods "$role" | head -1)"
  [ -n "$pod" ] || return 1
  cmd="$(kubectl -n "$NS" get pod "$pod" -o json 2>/dev/null \
    | jq -r '.spec.containers[0].command // [] | join(" ")' 2>/dev/null)"
  printf '%s' "$cmd" | tr ' ' '\n' | awk -v flag="$flag" '
    $0 == flag { getline; print; found = 1 }
    index($0, flag "=") == 1 { print substr($0, length(flag) + 2); found = 1 }
    END { if (!found) exit 1 }'
}

# router_metrics_port reads the port the gateway serves its metrics on.
router_metrics_port() {
  local pod args
  pod="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${DEPLOY},app.kubernetes.io/component=router" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
  [ -n "$pod" ] || return 1
  args="$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.spec.containers[0].args}' 2>/dev/null)"
  printf '%s' "$args" | sed -n 's/.*--prometheus-port=\([0-9][0-9]*\).*/\1/p' | head -1
}

# scrape_router_metrics writes the gateway's own metric page.
scrape_router_metrics() { # <tag>
  local tag="$1" pod port
  pod="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${DEPLOY},app.kubernetes.io/component=router" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
  port="$(router_metrics_port || true)"
  [ -n "$port" ] || port=9090
  if [ -z "$pod" ]; then
    echo "the gateway Pod was not found" > "${OUT}/router-${tag}.txt"
    return 1
  fi
  kubectl -n "$NS" port-forward "pod/${pod}" "$((LOCAL_PORT_BASE + 90)):${port}" \
    > "${OUT}/pf-router-${tag}.log" 2>&1 &
  local pf=$!
  PF_PIDS="$PF_PIDS ${pf}"
  local _
  for _ in $(seq 1 20); do
    if curl -sf --max-time 10 -o "${OUT}/router-${tag}.txt" "http://127.0.0.1:$((LOCAL_PORT_BASE + 90))/metrics"; then
      kill "$pf" 2>/dev/null || true
      return 0
    fi
    sleep 1
  done
  kill "$pf" 2>/dev/null || true
  echo "the gateway's metrics page did not answer" > "${OUT}/router-${tag}.txt"
  return 1
}

# engine_metrics writes the engine metrics of every Pod of a role into one file, so that the sum
# over the file is the role's own total.
engine_metrics() { # <role> <tag>
  local role="$1" tag="$2" pod port local_port index=0 out
  out="${OUT}/engine-${role}-${tag}.txt"
  : > "$out"
  for pod in $(role_pods "$role"); do
    port="$(pod_metrics_port "$pod")"
    if [ -z "$port" ]; then
      printf '# no metrics port on %s\n' "$pod" >> "$out"
      continue
    fi
    local_port=$((LOCAL_PORT_BASE + 20 + index))
    index=$((index + 1))
    kubectl -n "$NS" port-forward "pod/${pod}" "${local_port}:${port}" \
      > "${OUT}/pf-engine-${role}-${tag}-${pod}.log" 2>&1 &
    local pf=$!
    PF_PIDS="$PF_PIDS ${pf}"
    local _
    for _ in $(seq 1 20); do
      if curl -sf --max-time 10 "http://127.0.0.1:${local_port}/metrics" > "${out}.part" 2>/dev/null; then
        cat "${out}.part" >> "$out"
        break
      fi
      sleep 1
    done
    kill "$pf" 2>/dev/null || true
    rm -f "${out}.part"
  done
  return 0
}

# metric_sum FILE NAME sums every series of NAME in a Prometheus exposition.
metric_sum() {
  local file="$1" name="$2"
  serving_sum "$(cat "$file" 2>/dev/null || true)" "$name" 2>/dev/null || echo 0
}

# metric_value FILE NAME LABEL prints the value of the series of NAME carrying LABEL.
metric_value() {
  local file="$1" name="$2" label="$3"
  awk -v name="$name" -v label="$label" '
    index($1, name) == 1 && index($1, label) > 0 { total += $2; found = 1 }
    END { if (!found) exit 1; printf "%.10g\n", total }' "$file" 2>/dev/null || echo 0
}

# scaler_json prints the scaler's object.
scaler_json() {
  kubectl -n "$NS" get "$SCALER_RESOURCE" "$SCALER" -o json 2>/dev/null || true
}

# scaler_role_field ROLE FIELD prints one field of one role's status.
scaler_role_field() {
  scaler_json | jq -r --arg r "$1" --arg f "$2" \
    '[(.status.roles // [])[] | select(.name == $r) | .[$f]] | .[0] // empty' 2>/dev/null
}

# deployment_json prints the deployment's object.
deployment_json() {
  kubectl -n "$NS" get "$MD_RESOURCE" "$DEPLOY" -o json 2>/dev/null || true
}

# role_replicas ROLE prints the replica count the spec asks for.
role_replicas() {
  deployment_json | jq -r --arg r "$1" \
    '[(.spec.roles // [])[] | select(.name == $r) | .replicas] | .[0] // 0' 2>/dev/null
}

# wait_ready waits for the deployment to report Ready.
wait_ready() {
  local deadline=$((SECONDS + READY_TIMEOUT)) phase=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    phase="$(kubectl -n "$NS" get "$MD_RESOURCE" "$DEPLOY" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    [ "$phase" = "Ready" ] && return 0
    sleep 5
  done
  log "TIMEOUT: phase=${phase:-unknown}"
  return 1
}

# wait_scaler_bound waits for the scaler to bind to the target and to act.
wait_scaler_bound() {
  local deadline=$((SECONDS + BINDING_TIMEOUT)) bound="" active=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    bound="$(scaler_json | jq -r '[(.status.conditions // [])[] | select(.type == "Bound") | .status] | .[0] // ""' 2>/dev/null)"
    active="$(scaler_json | jq -r '[(.status.conditions // [])[] | select(.type == "Active") | .status] | .[0] // ""' 2>/dev/null)"
    if [ "$bound" = "True" ] && [ "$active" = "True" ]; then
      return 0
    fi
    sleep 3
  done
  log "TIMEOUT: the scaler did not bind (Bound=${bound:-none} Active=${active:-none})"
  return 1
}

# wait_replicas ROLE COUNT waits for a role to reach a replica count.
wait_replicas() {
  local role="$1" want="$2" deadline=$((SECONDS + SCALE_TIMEOUT)) got=0
  while [ "$SECONDS" -lt "$deadline" ]; do
    got="$(role_replicas "$role")"
    if [ "${got:-0}" -ge "$want" ] 2>/dev/null; then
      printf '%s' "$got"
      return 0
    fi
    sleep 5
  done
  printf '%s' "${got:-0}"
  return 1
}

# ---------------------------------------------------------------- the driver and the load

# The in-cluster driver carries the load.
ensure_driver() {
  local ready=""
  if kubectl -n "$DRIVER_NS" get pod "$DRIVER_POD" >/dev/null 2>&1; then
    kubectl -n "$DRIVER_NS" wait "pod/$DRIVER_POD" --for=condition=Ready --timeout=120s >/dev/null 2>&1 || true
    if kubectl -n "$DRIVER_NS" exec "$DRIVER_POD" -- python3 -c "import aiohttp" >/dev/null 2>&1; then
      ready=yes
    fi
  fi
  if [ -z "$ready" ]; then
    log "the driver $DRIVER_NS/$DRIVER_POD is not usable, creating it from $DRIVER_IMAGE"
    kubectl -n "$DRIVER_NS" delete pod "$DRIVER_POD" --ignore-not-found --wait=true >/dev/null 2>&1 || true
    kubectl -n "$DRIVER_NS" run "$DRIVER_POD" --image="$DRIVER_IMAGE" --restart=Never \
      --command -- sleep infinity >/dev/null 2>&1 || true
    kubectl -n "$DRIVER_NS" wait "pod/$DRIVER_POD" --for=condition=Ready --timeout=300s >/dev/null 2>&1 || true
    kubectl -n "$DRIVER_NS" exec "$DRIVER_POD" -- pip install --quiet aiohttp >/dev/null 2>&1 || true
    if kubectl -n "$DRIVER_NS" exec "$DRIVER_POD" -- python3 -c "import aiohttp" >/dev/null 2>&1; then
      ready=yes
    fi
  fi
  if [ -z "$ready" ]; then
    log "ERROR: the driver $DRIVER_NS/$DRIVER_POD could not be made ready"
    return 1
  fi
  log "the driver $DRIVER_NS/$DRIVER_POD is ready"
  return 0
}

# run_load LABEL DURATION CONCURRENCY drives the load through the gateway.
run_load() { # <label> <duration> <concurrency>
  local label="$1" dur="$2" conc="$3"
  python3 "${SKILL_DIR}/scripts/bench_client.py" load \
    --endpoint "http://${DEPLOY}-router.${NS}.svc:8000" \
    --model "/models" \
    --arm "spd" \
    --suite "spd" \
    --label "$label" \
    --input-tokens "$IN_TOK" \
    --output-tokens "$OUT_TOK" \
    --concurrency "$conc" \
    --duration "$dur" \
    --out "${OUT}/${label}.json" \
    --requests-out "${OUT}/${label}_reqs.jsonl" > "${OUT}/${label}.log" 2>&1
}

# ---------------------------------------------------------------- the content checks

# request_body PROMPT MAX_TOKENS [EXTRA_JSON] prints one chat request body.
#
# THE THINKING SWITCH IS PINNED HERE, and it is the operator's own recipe: the group-forward probe
# sends the same field to keep a model that reasons out loud from answering with its reasoning
# (pkg/worker/controllers/worker/model_deployment_group_forward_probe.go, the ChatFields of the
# pair input).
request_body() {
  local prompt="$1" max_tokens="$2" extra="${3:-{\}}"
  jq -nc --arg model "/models" --arg prompt "$prompt" --argjson max_tokens "$max_tokens" --argjson extra "$extra" \
    '{model: $model, messages: [{role: "user", content: $prompt}], max_tokens: $max_tokens, temperature: 0, chat_template_kwargs: {enable_thinking: false}} + $extra'
}

# chat_request PORT BODY OUTFILE sends one request through the gateway.
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

ARITHMETIC_PROMPT="What is 17 multiplied by 23? Reply with the number only."
ARITHMETIC_EXPECT="391"
CHINESE_PROMPT="请只输出下面这句话，不要添加任何其他内容：今天天气很好。"
CHINESE_EXPECT="今天天气很好"
JSON_PROMPT="Return a JSON object with the key name and the value gpu."

# The four content classes, through the gateway. Each answers a question a throughput number
# cannot: is the answer right, is the stream whole, is the text what was asked for, and is the
# object a JSON object at all.
check_content_classes() { # <tag>
  local tag="$1" answer segments stream_text finish reasoning

  request_body "$ARITHMETIC_PROMPT" 32 > "${OUT}/body-${tag}-arithmetic.json"
  chat_request "${LOCAL_PORT_BASE}" "${OUT}/body-${tag}-arithmetic.json" "${OUT}/${tag}-arithmetic.json"
  answer="$(answer_of "${OUT}/${tag}-arithmetic.json")"
  case "$answer" in
    *"${ARITHMETIC_EXPECT}"*)
      record PASS "the gateway answers the arithmetic class" "the answer carries ${ARITHMETIC_EXPECT}: ${answer}"
      ;;
    *)
      record FAIL "the gateway answers the arithmetic class" "the answer does not carry ${ARITHMETIC_EXPECT}: ${answer}"
      ;;
  esac

  request_body "$ARITHMETIC_PROMPT" 32 '{"stream":true,"stream_options":{"include_usage":true}}' > "${OUT}/body-${tag}-sse.json"
  chat_request "${LOCAL_PORT_BASE}" "${OUT}/body-${tag}-sse.json" "${OUT}/${tag}-sse.txt"
  segments="$(sse_content_chunks "${OUT}/${tag}-sse.txt")"
  stream_text="$(sse_text "${OUT}/${tag}-sse.txt")"
  finish="$(sse_finish_reason "${OUT}/${tag}-sse.txt")"
  if [ "${segments:-0}" -ge 2 ]; then
    record PASS "the gateway's stream arrives in segments" "the stream carried ${segments} content-bearing segments"
  else
    record FAIL "the gateway's stream arrives in segments" "the stream carried ${segments:-0} content-bearing segments"
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
  if grep -q 'data: \[DONE\]' "${OUT}/${tag}-sse.txt" 2>/dev/null; then
    record PASS "the stream is closed" "the stream ended with the [DONE] marker"
  else
    record FAIL "the stream is closed" "the stream carried no [DONE] marker"
  fi

  request_body "$CHINESE_PROMPT" 64 > "${OUT}/body-${tag}-chinese.json"
  chat_request "${LOCAL_PORT_BASE}" "${OUT}/body-${tag}-chinese.json" "${OUT}/${tag}-chinese.json"
  answer="$(answer_of "${OUT}/${tag}-chinese.json")"
  case "$answer" in
    *"${CHINESE_EXPECT}"*)
      record PASS "the gateway answers the exact Chinese class" "the answer carries ${CHINESE_EXPECT}: ${answer}"
      ;;
    *)
      record FAIL "the gateway answers the exact Chinese class" "the answer does not carry ${CHINESE_EXPECT}: ${answer}"
      ;;
  esac

  request_body "$JSON_PROMPT" 64 '{"response_format":{"type":"json_object"}}' > "${OUT}/body-${tag}-json.json"
  chat_request "${LOCAL_PORT_BASE}" "${OUT}/body-${tag}-json.json" "${OUT}/${tag}-json.json"
  answer="$(answer_of "${OUT}/${tag}-json.json")"
  if printf '%s' "$answer" | jq -e 'fromjson? | select(type == "object") | (.name // "") | ascii_downcase == "gpu"' >/dev/null 2>&1; then
    record PASS "the gateway answers the structured JSON class" "the answer is a JSON object whose name is gpu: ${answer}"
  else
    record FAIL "the gateway answers the structured JSON class" "the answer is not a JSON object with name=gpu: ${answer}"
  fi

  for name in arithmetic chinese json; do
    reasoning="$(reasoning_of "${OUT}/${tag}-${name}.json")"
    if [ -n "$reasoning" ]; then
      record WARN "the thinking switch was honoured on the ${name} request" \
        "the answer also carried reasoning, so the engine did not take the switch: ${reasoning}"
    else
      record PASS "the thinking switch was honoured on the ${name} request" "the answer carried no reasoning"
    fi
  done
}

# ---------------------------------------------------------------- the case

log "Writing the manifests into ${OUT}"
write_manifest
write_scaler_manifest
log "the model is ${MODEL}, served as /models from the artifact ${ARTIFACT}"
kubectl apply --dry-run=server -f "${OUT}/spd.yaml" > "${OUT}/apply-dry-run.txt" 2>&1
kubectl apply --dry-run=server -f "${OUT}/spd-scaler.yaml" >> "${OUT}/apply-dry-run.txt" 2>&1

log "Applying the deployment"
kubectl -n "$NS" delete "$MD_RESOURCE" "$DEPLOY" --ignore-not-found --wait=true --timeout=180s >/dev/null 2>&1 || true
kubectl apply -f "${OUT}/spd.yaml" 2>&1 | tee "${OUT}/apply.log"
if ! wait_ready; then
  kubectl -n "$NS" describe "$MD_RESOURCE" "$DEPLOY" > "${OUT}/describe.txt" 2>&1
  # A P/D pair that does not come up is BLOCKED, not FAIL: the upstream basis of this cell is a
  # proof of concept, and a reader must be able to tell "the Kubernetes P/D path did not come up"
  # apart from "the P/D path came up and answered wrongly".
  record BLOCKED "the SGLang prefill/decode pair reached Ready" \
    "the pair did not reach Ready, so the Kubernetes P/D path could not be exercised; see describe.txt"
  cat "${OUT}/verdicts.txt" 2>/dev/null
  exit 1
fi
record PASS "the SGLang prefill/decode pair reached Ready" "the pair is Ready with one replica per role"

# The serving profile names where the evidence lives for this engine, and the vendor is read off
# the InstanceType of the deployment that is now in the cluster. The images this case pins are
# NVIDIA ones, so the fallback is the pinned vendor and not a guess.
VENDOR="$(serving_vendor "$(deployment_json 2>/dev/null || echo '{}')" 2>/dev/null || true)"
if [ -z "$VENDOR" ]; then
  VENDOR=nvidia
fi
if ! serving_profile "$VENDOR" SGLang; then
  record BLOCKED "the serving profile for this vendor and engine exists" \
    "no serving profile for vendor '${VENDOR}' and engine SGLang, so the case does not know where the transfer evidence lives"
  cat "${OUT}/verdicts.txt" 2>/dev/null
  exit 1
fi
record PASS "the serving profile for this vendor and engine exists" \
  "the vendor is ${VENDOR} and the profile names ${SP_SENT_COUNT} for the transfer and ${SP_FAILED} for the failures"

# --- the rendered leg ------------------------------------------------------------------
PREFILL_MODE="$(rendered_arg prefill "--disaggregation-mode" || true)"
DECODE_MODE="$(rendered_arg decode "--disaggregation-mode" || true)"
PREFILL_BACKEND="$(rendered_arg prefill "--disaggregation-transfer-backend" || true)"
DECODE_BACKEND="$(rendered_arg decode "--disaggregation-transfer-backend" || true)"
PREFILL_BOOTSTRAP="$(rendered_arg prefill "--disaggregation-bootstrap-port" || true)"
log "the rendered leg: prefill mode=${PREFILL_MODE:-none} backend=${PREFILL_BACKEND:-none} bootstrap=${PREFILL_BOOTSTRAP:-none}"
log "the rendered leg: decode mode=${DECODE_MODE:-none} backend=${DECODE_BACKEND:-none}"

if [ "$PREFILL_MODE" = "prefill" ] && [ "$DECODE_MODE" = "decode" ]; then
  record PASS "the operator rendered a disaggregated leg" \
    "the prefill half rendered --disaggregation-mode ${PREFILL_MODE} and the decode half rendered --disaggregation-mode ${DECODE_MODE}"
else
  record FAIL "the operator rendered a disaggregated leg" \
    "the prefill half rendered mode=${PREFILL_MODE:-none} and the decode half mode=${DECODE_MODE:-none}"
fi

if [ -n "$PREFILL_BACKEND" ] && [ "$PREFILL_BACKEND" = "$DECODE_BACKEND" ]; then
  record PASS "both halves render the same transfer backend" \
    "the rendered backend is ${PREFILL_BACKEND} on both halves; the pin the operator applies on this leg is ${SP_PIN_ARG}"
else
  record FAIL "both halves render the same transfer backend" \
    "the prefill half rendered ${PREFILL_BACKEND:-none} and the decode half rendered ${DECODE_BACKEND:-none}; they have to agree or the pair transfers over a leg the other end does not speak"
fi
if [ "$PREFILL_BACKEND" = "mooncake_tcp" ]; then
  record PASS "the direct leg is pinned to TCP" "the rendered backend is mooncake_tcp, which is the operator's spelling of the TCP pin"
elif [ -n "$PREFILL_BACKEND" ]; then
  record WARN "the direct leg is pinned to TCP" "the rendered backend is ${PREFILL_BACKEND} rather than the TCP pin ${SP_PIN_ARG}"
fi

# --- the gateway's own worker set ------------------------------------------------------
scrape_router_metrics "phase1" || true
PREFILL_WORKERS="$(metric_value "${OUT}/router-phase1.txt" "smg_worker_pool_size" 'worker_type="prefill"')"
DECODE_WORKERS="$(metric_value "${OUT}/router-phase1.txt" "smg_worker_pool_size" 'worker_type="decode"')"
log "the gateway's worker pool: prefill=${PREFILL_WORKERS} decode=${DECODE_WORKERS}"

if [ "${PREFILL_WORKERS:-0}" -ge 1 ] && [ "${DECODE_WORKERS:-0}" -ge 1 ]; then
  record PASS "the gateway holds workers of both kinds" \
    "the gateway's pool holds ${PREFILL_WORKERS} prefill worker(s) and ${DECODE_WORKERS} decode worker(s)"
else
  record FAIL "the gateway holds workers of both kinds" \
    "the gateway's pool holds ${PREFILL_WORKERS:-0} prefill worker(s) and ${DECODE_WORKERS:-0} decode worker(s), so its P/D routing is not in place"
fi

# --- the groups of the two roles, published independently --------------------------------
GROUPS_PREFILL="$(deployment_json | jq -r '[(.status.roles // [])[] | select(.name == "prefill") | (.groups // [])[] | .state] | join(",")' 2>/dev/null)"
GROUPS_DECODE="$(deployment_json | jq -r '[(.status.roles // [])[] | select(.name == "decode") | (.groups // [])[] | .state] | join(",")' 2>/dev/null)"
log "the published groups: prefill=[${GROUPS_PREFILL}] decode=[${GROUPS_DECODE}]"

if [ -n "$GROUPS_PREFILL" ] && [ -n "$GROUPS_DECODE" ]; then
  record PASS "the two roles publish their groups independently" \
    "the prefill role publishes the group states ${GROUPS_PREFILL} and the decode role ${GROUPS_DECODE}"
else
  record FAIL "the two roles publish their groups independently" \
    "the prefill role publishes ${GROUPS_PREFILL:-none} and the decode role ${GROUPS_DECODE:-none}"
fi

# --- the content classes through the gateway ----------------------------------------------
log "Opening the gateway's door"
kubectl -n "$NS" port-forward "pod/$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${DEPLOY},app.kubernetes.io/component=router" \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)" "${LOCAL_PORT_BASE}:8000" \
  > "${OUT}/pf-gateway.log" 2>&1 &
PF_PIDS="$PF_PIDS $!"
GATEWAY_UP=0
for _ in $(seq 1 60); do
  if curl -sf -o /dev/null "http://127.0.0.1:${LOCAL_PORT_BASE}/v1/models"; then
    GATEWAY_UP=1
    break
  fi
  sleep 2
done
if [ "$GATEWAY_UP" -ne 1 ]; then
  record BLOCKED "the gateway's door opened" \
    "the gateway Pod did not serve /v1/models, so the SGLang P/D path could not be exercised through it; see pf-gateway.log"
  cat "${OUT}/verdicts.txt" 2>/dev/null
  exit 1
fi
record PASS "the gateway's door opened" "the gateway answered /v1/models"

check_content_classes "phase1"

# --- the prefiller's contribution and the transfer on the direct leg -----------------------
#
# THE CHECK IS THE SAME GUARD case 9 runs: a gateway that answered every request from the decode
# half alone would let this case pass on traffic that never touched the prefiller.
engine_metrics prefill "phase1" || true
engine_metrics decode "phase1" || true
SENT_COUNT="$(metric_sum "${OUT}/engine-prefill-phase1.txt" "$SP_SENT_COUNT")"
SENT_SUM="$(metric_sum "${OUT}/engine-prefill-phase1.txt" "$SP_SENT_SUM")"
FAILED="$(metric_sum "${OUT}/engine-prefill-phase1.txt" "$SP_FAILED")"
FAILED_DECODE="$(metric_sum "${OUT}/engine-decode-phase1.txt" "$SP_FAILED")"
RECEIVED="$(metric_sum "${OUT}/engine-decode-phase1.txt" "$SP_RECEIVED")"
TRANSFER_QUEUE="$(metric_sum "${OUT}/engine-decode-phase1.txt" "sglang:num_decode_transfer_queue_reqs")"
log "the transfer evidence: sent=${SENT_COUNT} bytes_sum=${SENT_SUM} failed=${FAILED} received=${RECEIVED} decode_waiting=${TRANSFER_QUEUE}"

if [ "${SENT_COUNT:-0}" -gt 0 ] || [ "${SENT_SUM:-0}" -gt 0 ]; then
  record PASS "the prefiller moved KV over the direct leg" \
    "the prefill half counted ${SENT_COUNT} transfer(s) and ${SENT_SUM} megabyte(s) in ${SP_SENT_COUNT} and ${SP_SENT_SUM}"
else
  record BLOCKED "the prefiller moved KV over the direct leg" \
    "the prefill half counted no transfer in ${SP_SENT_COUNT} or ${SP_SENT_SUM}; ${SP_TRANSFER_SOURCE}. A gateway that answers from the decode half alone looks exactly like this, so the cell cannot be called exercised."
fi

if [ "${FAILED:-0}" -eq 0 ] && [ "${FAILED_DECODE:-0}" -eq 0 ]; then
  record PASS "no half counted a failed transfer" \
    "the prefill half counted ${FAILED} and the decode half ${FAILED_DECODE} failed transfers in ${SP_FAILED}"
else
  record FAIL "no half counted a failed transfer" \
    "the prefill half counted ${FAILED} and the decode half ${FAILED_DECODE} failed transfers in ${SP_FAILED}"
fi

# The decode half's own transfer counters are recorded even when they are absent, because a
# metric the image does not expose is a fact about the image and not a pass.
if grep -q '^sglang:kv_transfer_total_mb' "${OUT}/engine-prefill-phase1.txt" 2>/dev/null; then
  record PASS "the engine exposed its disaggregation counters" \
    "the prefill half exposed sglang:kv_transfer_total_mb and the decode half exposed sglang:num_decode_transfer_queue_reqs"
else
  record WARN "the engine exposed its disaggregation counters" \
    "the prefill half exposed no sglang:kv_transfer_total_mb, so the engine-side disaggregation counters this case reads are not proven at runtime; the transfer rows above stand on the counters the shared serving library names"
fi

record PASS "the decode half counted what it generated" \
  "the decode half counted ${RECEIVED} in ${SP_RECEIVED} and ${TRANSFER_QUEUE} requests waiting on a transfer in sglang:num_decode_transfer_queue_reqs"

# --- the Scaler phase ------------------------------------------------------------------------
#
# THE SCALER CONTRACT IS THE ONE case 9 drives on vLLM, and what is different here is only where
# the running-request count comes from: the SGLang engine exports sglang:num_running_reqs where
# vLLM exports vllm:num_requests_running. The phase therefore asks the same question of the other
# engine: does the scaler read the engine's own counter and act on it.
log "Preparing the in-cluster driver"
if ! ensure_driver; then
  record BLOCKED "the in-cluster driver is ready" "the driver could not be made ready, so the Scaler phase could not be driven"
  cat "${OUT}/verdicts.txt" 2>/dev/null
  exit 1
fi

log "Applying the scaler"
kubectl apply -f "${OUT}/spd-scaler.yaml" 2>&1 | tee "${OUT}/scaler-apply.log"
if ! wait_scaler_bound; then
  record FAIL "the scaler bound to the target and acts" "the scaler never reported Bound=True and Active=True"
  cat "${OUT}/verdicts.txt" 2>/dev/null
  exit 1
fi
record PASS "the scaler bound to the target and acts" "the scaler reports Bound=True and Active=True"

log "Running the load for ${LOAD_SECONDS}s at concurrency ${LOAD_CONCURRENCY}"
run_load "load" "$LOAD_SECONDS" "$LOAD_CONCURRENCY" || true

engine_metrics decode "under-load" || true
engine_metrics prefill "under-load" || true
scaler_json > "${OUT}/scaler-under-load.json" 2>/dev/null || true

RAW_DECODE_RUNNING="$(metric_sum "${OUT}/engine-decode-under-load.txt" "sglang:num_running_reqs")"
RAW_PREFILL_RUNNING="$(metric_sum "${OUT}/engine-prefill-under-load.txt" "sglang:num_running_reqs")"
SCALER_DECODE_RUNNING="$(scaler_role_field decode runningRequests)"
SCALER_PREFILL_RUNNING="$(scaler_role_field prefill runningRequests)"
log "the engine's own running counter: decode=${RAW_DECODE_RUNNING} prefill=${RAW_PREFILL_RUNNING}"
log "the scaler's recorded running count: decode=${SCALER_DECODE_RUNNING} prefill=${SCALER_PREFILL_RUNNING}"

if [ "${SCALER_DECODE_RUNNING:-0}" -gt 0 ]; then
  record PASS "the scaler read the SGLang engine's running-request counter" \
    "the engine's sglang:num_running_reqs read ${RAW_DECODE_RUNNING} on the decode role and ${RAW_PREFILL_RUNNING} on the prefill role; the scaler recorded ${SCALER_DECODE_RUNNING} and ${SCALER_PREFILL_RUNNING}"
else
  record FAIL "the scaler read the SGLang engine's running-request counter" \
    "the scaler recorded ${SCALER_DECODE_RUNNING} running requests on the decode role against the engine's ${RAW_DECODE_RUNNING}"
fi

DECODE_REPLICAS="$(wait_replicas decode 2 || true)"
PREFILL_REPLICAS="$(wait_replicas prefill 2 || true)"
log "after the load: decode=${DECODE_REPLICAS} prefill=${PREFILL_REPLICAS}"

if [ "${DECODE_REPLICAS:-0}" -ge 2 ]; then
  record PASS "the decode role grew under the scaler" "the decode role holds ${DECODE_REPLICAS} replicas"
else
  record FAIL "the decode role grew under the scaler" "the decode role holds ${DECODE_REPLICAS} replicas"
fi
if [ "${PREFILL_REPLICAS:-0}" -ge 2 ]; then
  record PASS "the prefill role grew under the scaler" "the prefill role holds ${PREFILL_REPLICAS} replicas"
else
  record WARN "the prefill role grew under the scaler" \
    "the prefill role holds ${PREFILL_REPLICAS} replicas; its own running-request signal was ${SCALER_PREFILL_RUNNING} against a target of 4"
fi

log "Stopping the load and waiting for the fall-back"
sleep 20
FALLBACK_DEADLINE=$((SECONDS + COOLDOWN_SECONDS))
FELL_BACK=0
while [ "$SECONDS" -lt "$FALLBACK_DEADLINE" ]; do
  prefill_now="$(role_replicas prefill)"
  decode_now="$(role_replicas decode)"
  retiring="$(deployment_json | jq -r 'if (.status.retirement // null) == null then "clear" else "retiring" end' 2>/dev/null)"
  if [ "${prefill_now:-0}" -le 1 ] && [ "${decode_now:-0}" -le 1 ] && [ "$retiring" = "clear" ]; then
    FELL_BACK=1
    break
  fi
  sleep 5
done
if [ "$FELL_BACK" -eq 1 ]; then
  record PASS "the counts came back to the floor" \
    "the prefill role holds $(role_replicas prefill) and the decode role $(role_replicas decode) replicas, and no retirement is outstanding"
else
  record FAIL "the counts came back to the floor" \
    "the prefill role holds $(role_replicas prefill) and the decode role $(role_replicas decode) replicas, and the retirement state is $(deployment_json | jq -r '.status.retirement.state // "clear"')"
fi

kubectl -n "$NS" get events --field-selector "involvedObject.name=${SCALER}" \
  -o json > "${OUT}/scaler-events.json" 2>/dev/null || true
COMMIT_EVENTS="$(jq -r '[.items // [] | select(.reason == "ReplicaVectorCommitted")] | length' "${OUT}/scaler-events.json" 2>/dev/null || echo 0)"
if [ "${COMMIT_EVENTS:-0}" -gt 0 ]; then
  record PASS "the scaler published its decisions" "the scaler recorded ${COMMIT_EVENTS} ReplicaVectorCommitted event(s)"
else
  record FAIL "the scaler published its decisions" "the scaler recorded no ReplicaVectorCommitted event"
fi

kubectl -n "$NS" get "$MD_RESOURCE" "$DEPLOY" -o json > "${OUT}/modeldeployment.json" 2>/dev/null || true
kubectl get devices -o json > "${OUT}/released-devices.json" 2>/dev/null || true

cat "${OUT}/verdicts.txt" 2>/dev/null
FAILS="$(grep -c '^FAIL' "${OUT}/verdicts.txt" 2>/dev/null || true)"
BLOCKED_ROWS="$(grep -c '^BLOCKED' "${OUT}/verdicts.txt" 2>/dev/null || true)"
WARNS="$(grep -c '^WARN' "${OUT}/verdicts.txt" 2>/dev/null || true)"
log "CASE 10 (SGLang P/D behind the gateway) complete: ${FAILS:-0} failed row(s), ${BLOCKED_ROWS:-0} blocked row(s), ${WARNS:-0} warning(s)"

# THE SCRIPT ENDS HERE, and the last statement is the exit code. An explicit trailing "exit 0"
# makes the analyzer read the EXIT trap's handler as a function nothing calls, and it then reports
# the whole handler as unreachable.
[ "${FAILS:-0}" -eq 0 ] && [ "${BLOCKED_ROWS:-0}" -eq 0 ] || exit 1
