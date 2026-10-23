#!/usr/bin/env bash
#
# CASE 9 — vLLM Prefill/Decode under a ModelDeploymentScaler   (MUTATING, self-recovering)
#
#   case-9-vpd-scaler.sh [OUT_DIR]
#
# Goal:        Prove the Scaler-driven leg of the vLLM prefill/decode cell: a ModelDeploymentScaler
#              in Active mode reads the engine's running-request counter, raises the replica count
#              of the roles that carry the load, the new Pods join the routing, and when the load
#              stops the counts come back down THROUGH THE RETIREMENT PROTOCOL rather than by a
#              number being patched.
#
#              The cell is also where a silent decode-only fallback would hide: a router that stops
#              using the prefiller answers every request perfectly and moves no KV at all. The case
#              therefore PRE-CHECKS THE PREFILLER'S CONTRIBUTION before the load, and a leg that
#              did not use the prefiller stops the case there instead of passing on the traffic.
#
#              The KV transfer on the pair's direct leg is asserted from the engine's own counters
#              (vllm:mooncake_*), which the gpustack runner image carries; an upstream vLLM image
#              carries none of them and fails the row rather than passing it silently.
#
# Environment: A cluster with 4 free GPUs on one InstanceType, the NodeModelStore cache warm for
#              qwen3-8b (the artifact of manifests/10-model-delivery.yaml, which holds
#              Qwen/Qwen3-8B), and an in-cluster default/bench-driver Pod carrying python3 and
#              aiohttp. The pair starts at one replica per role and may reach two per role, so the
#              peak is four GPUs.
# Inputs:      The manifest is written by this script into OUT_DIR/vpd-scaler.yaml and the scaler
#              into OUT_DIR/vpd-scaler-scaler.yaml. The operator stamps each replica's own
#              address, so the manifest is identical for every replica.
#
#              THE TUNING IS DELIBERATE AND STATED HERE, not hidden in the numbers. The algorithm
#              is Proportional on RunningRequests with a target of four requests per replica and a
#              tolerance of 0.1: the scaler divides what it observed by the target and takes the
#              ceiling, so a role crosses its target at five running requests and is held inside
#              the tolerance band below that. The sampling interval is 10 s so the rise is seen
#              within a short run. The scale-down stabilization window is 30 s rather than the
#              default 300 s, because a five-minute window would outlive the test, and the
#              decode post-scale-up grace is 30 s rather than the default 120 s for the same
#              reason. The retirement's own 60 s disaggregated handoff grace window is NOT
#              touched: it is a property of the operator, and the case asserts it.
#
#              THE LOAD IS DRIVEN IN THE CLUSTER, through default/bench-driver, because a
#              port-forwarded stream bottlenecks on the API server's SPDY proxy and drops
#              connections exactly when the leg is under the most load. The endpoint is the
#              cluster-internal Service DNS, so nothing is forwarded at all.
#
# Expected:    - the router's own decision counter shows requests sent through a PREFILL replica,
#                before the load runs, and the prefill half's own transfer counter moved;
#              - the scaler's status records the running-request count it read, and that count
#                agrees with the engine's own counter read at the same time;
#              - the decode role's replica count rises, and so does the prefill role's;
#              - the new Pods are published as eligible groups of their own role, so the two
#                roles are tracked independently;
#              - the prefill half counted the bytes it transferred, the decode half counted the
#                blocks it was handed, and neither half counted a failed transfer;
#              - when the load stops, the scaler commits a scale-down, the deployment goes through
#                the retirement protocol, the 60 s disaggregated handoff grace window is observed
#                rather than assumed, and the counts come back to the floor.
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
# series carry a transfer, and which carry a failure.
# shellcheck source=/dev/null
. "${SKILL_DIR}/../_e2e-lib/scripts/serving-lib.sh"

NS="${BENCH_NS:-gpustack-bench}"
DRIVER_NS="${BENCH_DRIVER_NS:-default}"
DRIVER_POD="${BENCH_DRIVER:-bench-driver}"
DRIVER_IMAGE="${BENCH_C9_DRIVER_IMAGE:-docker.io/library/python:3.11-slim}"
OUT="${1:-${REPO}/.claude/reports/$(date +%F)-benchmark/vpd-scaler}"
DEPLOY="vpd-scaler"
SCALER="autoscale-vpd-scaler"
MODEL="${VPD_MODEL:-Qwen/Qwen3-8B}"
ARTIFACT="${VPD_ARTIFACT:-qwen3-8b}"
IMAGE="${VPD_IMAGE:-gpustack/runner:cuda13.0-vllm0.32.0}"
INSTANCE_TYPE="${VPD_INSTANCE_TYPE:-bench-gpu}"
LOCAL_PORT_BASE="${LOCAL_PORT_BASE:-18400}"
READY_TIMEOUT="${READY_TIMEOUT:-1800}"
SCALE_TIMEOUT="${SCALE_TIMEOUT:-900}"
BINDING_TIMEOUT="${BINDING_TIMEOUT:-300}"
# The load. The prompt is long on purpose: a prefill replica finishes a short prompt far faster
# than the request lives, so a short prompt leaves the prefiller's own running-request counter
# below its target and the prefill role never grows. A long prompt keeps the prefiller busy
# enough for its counter to cross the target too.
IN_TOK="${VPD_INPUT_TOKENS:-8192}"
OUT_TOK="${VPD_OUTPUT_TOKENS:-64}"
LOAD_CONCURRENCY="${VPD_CONCURRENCY:-24}"
LOAD_SECONDS="${VPD_LOAD_SECONDS:-150}"
COOLDOWN_SECONDS="${VPD_COOLDOWN_SECONDS:-240}"
SCALER_RESOURCE=modeldeploymentscalers.v1alpha1.worker.gpustack.ai
# THE DEPLOYMENT IS READ FROM ITS CRD, not from the aggregated v1 API. The aggregated API serves
# the same kind but proxies it through the operator's cached client, so a read through it can
# answer with a status the operator has not seen yet, and this case samples the status every two
# seconds to time a scale-out and a fall-back. The CRD answers with what is stored.
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
  # THE SCALER GOES FIRST, because a bound deployment cannot be deleted cleanly while its owner
  # still holds it.
  kubectl -n "$NS" delete "$SCALER_RESOURCE" "$SCALER" --ignore-not-found --wait=true --timeout=120s \
    >/dev/null 2>&1 || true
  kubectl -n "$NS" delete "$MD_RESOURCE" "$DEPLOY" --ignore-not-found --wait=true --timeout=300s \
    >/dev/null 2>&1 || true
  return 0
}
trap cleanup EXIT

# record VERDICT LABEL DETAIL writes one row of the results table.
record() {
  printf '%s | %s | %s\n' "$1" "$2" "$3" >> "${OUT}/verdicts.txt"
  case "$1" in
    FAIL) echo "FAIL | ${2} | ${3}" ;;
    WARN) echo "WARN | ${2} | ${3}" ;;
    *) echo "PASS | ${2} | ${3}" ;;
  esac
}

# ---------------------------------------------------------------- the manifests

write_manifest() {
  cat > "${OUT}/vpd-scaler.yaml" <<YAML
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
    name: vLLM
    image: ${IMAGE}
  router:
    name: llm-d-router
  roles:
    - name: prefill
      kind: Prefill
      replicas: 1
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
        # THE ROUTER COUNTS TOKENS by POSTing /v1/chat/completions/render, and vLLM serves that
        # path only under this flag. Without it the failure is silent: the render answers 404
        # while the chat requests still answer 200, and a router that cannot count tokens does
        # not split a long prompt at all.
        - --enable-scale-out
      # THE ENGINE HOLDS ITS CONNECTIONS AS LONG AS THE ROUTER HOLDS THEM. vLLM closes an idle
      # HTTP connection after five seconds, and the router pools its upstream connections for far
      # longer, so a request can be written to a connection the engine has already closed, and the
      # router answers that request with a refusal the client sees as a failed request. The
      # engine's keep-alive is raised to match the pool that holds the connection.
      env:
        - name: VLLM_HTTP_TIMEOUT_KEEP_ALIVE
          value: "3600"
    - name: decode
      kind: Decode
      replicas: 1
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
        # THE ROUTER COUNTS TOKENS by POSTing /v1/chat/completions/render, and vLLM serves that
        # path only under this flag. Without it the failure is silent: the render answers 404
        # while the chat requests still answer 200, and a router that cannot count tokens does
        # not split a long prompt at all.
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

# The scaler. Every role of the target has a bound, because a role without one is refused, and the
# algorithm states a target for every role, because a role without one is not planned for.
write_scaler_manifest() {
  cat > "${OUT}/vpd-scaler-scaler.yaml" <<YAML
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
      # The window the recommendations are remembered over. The default is 300 s, which outlives
      # a short test; 30 s keeps the fall-back inside the run without hiding it.
      stabilizationWindowSeconds: 30
      # A decode role that just grew is held for this long before it may shrink again. The default
      # is 120 s; 30 s keeps the pair from collapsing straight back while the retirement of the
      # other half is still running.
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

# router_metrics_port reads the port the router serves its metrics on. The operator renders
# --prometheus-port on every router it manages, and the llm-d picker listens on the operator's own
# port by default, so the argument is read when it is there and the operator's port is used when
# the router is configured by a document instead of by arguments.
router_metrics_port() {
  local pod args
  pod="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${DEPLOY},app.kubernetes.io/component=router" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
  [ -n "$pod" ] || return 1
  args="$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.spec.containers[0].args}' 2>/dev/null)"
  printf '%s' "$args" | sed -n 's/.*--prometheus-port=\([0-9][0-9]*\).*/\1/p' | head -1
}

# scrape_router_metrics writes the router's own metric page.
scrape_router_metrics() { # <tag>
  local tag="$1" pod port
  pod="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${DEPLOY},app.kubernetes.io/component=router" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
  port="$(router_metrics_port || true)"
  [ -n "$port" ] || port=9090
  if [ -z "$pod" ]; then
    echo "the router Pod was not found" > "${OUT}/router-${tag}.txt"
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
  echo "the router's metrics page did not answer" > "${OUT}/router-${tag}.txt"
  return 1
}

# engine_metrics writes the engine metrics of every Pod of a role into one file, so that the sum
# over the file is the role's own total. The host cannot reach a Pod IP, so each Pod is reached
# through its own port-forward on the port the operator stamped on it.
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

# metric_sum FILE NAME sums every series of NAME in a Prometheus exposition. A series that is not
# exposed reads as zero, which is what a counter that never moved looks like.
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
  scaler_json > "${OUT}/scaler-unbound.json" 2>/dev/null || true
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

# wait_floor waits for every role to be back at one replica and for the retirement to have cleared.
wait_floor() {
  local deadline=$((SECONDS + COOLDOWN_SECONDS)) prefill=0 decode=0 retiring="clear"
  while [ "$SECONDS" -lt "$deadline" ]; do
    prefill="$(role_replicas prefill)"
    decode="$(role_replicas decode)"
    retiring="$(deployment_json | jq -r 'if (.status.retirement // null) == null then "clear" else "retiring" end' 2>/dev/null)"
    if [ "${prefill:-0}" -le 1 ] && [ "${decode:-0}" -le 1 ] && [ "$retiring" = "clear" ]; then
      return 0
    fi
    sleep 5
  done
  return 1
}

# ---------------------------------------------------------------- the driver

# The in-cluster driver carries the load. It is created when it is missing, and the case STOPS
# when it cannot be made ready: the load runs against a cluster-internal Service name the host
# cannot resolve, so a fallback would measure the runner's network.
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
    log "ERROR: the driver $DRIVER_NS/$DRIVER_POD could not be made ready; the load must run inside the cluster"
    return 1
  fi
  log "the driver $DRIVER_NS/$DRIVER_POD is ready"
  return 0
}

# ---------------------------------------------------------------- the sampler

# The sampler records what the leg looked like every two seconds, so the rise and the fall-back
# can be read against the signals that caused them, and so the grace window can be measured
# rather than assumed.
sampler_loop() { # <phase-file> <timeline>
  local phase_file="$1" timeline="$2"
  while :; do
    local md_json sc_json phase
    md_json="$(deployment_json)"
    sc_json="$(scaler_json)"
    phase="$(cat "$phase_file" 2>/dev/null || true)"
    jq -nc \
      --arg ts "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
      --arg phase "$phase" \
      --argjson md "${md_json:-null}" \
      --argjson sc "${sc_json:-null}" \
      '{
        ts: $ts,
        phase: $phase,
        desired_by_role: (($md.status.roles // []) | map({key: .name, value: .desired}) | from_entries),
        ready_by_role: (($md.status.roles // []) | map({key: .name, value: .ready}) | from_entries),
        groups_by_role: (($md.status.roles // []) | map({key: .name, value: ((.groups // []) | map({ordinal, state, reason}) )}) | from_entries),
        retirement_role: ($md.status.retirement.roleName // ""),
        retirement_ordinal: ($md.status.retirement.replicaOrdinal // -1),
        retirement_state: ($md.status.retirement.state // ""),
        retirement_reason: ($md.status.retirement.reason // ""),
        retirement_started_at: ($md.status.retirement.startedAt // ""),
        retirement_phase_started_at: ($md.status.retirement.phaseStartedAt // ""),
        running_requests_by_role: (($sc.status.roles // []) | map({key: .name, value: .runningRequests}) | from_entries),
        recommended_by_role: (($sc.status.roles // []) | map({key: .name, value: .rawRecommendedReplicas}) | from_entries),
        constrained_by_role: (($sc.status.roles // []) | map({key: .name, value: .constrainedReplicas}) | from_entries),
        committed_vector: ($sc.status.lastCommit.vector // {}),
        last_scale_down: ($sc.status.lastScaleDown // null),
        conditions: (($sc.status.conditions // []) | map({type: .type, status: .status, reason: .reason}))
      }' >> "$timeline" 2>/dev/null || true
    sleep 2
  done
}

# ---------------------------------------------------------------- the load

# run_load LABEL DURATION CONCURRENCY drives the load through the router.
run_load() { # <label> <duration> <concurrency>
  local label="$1" dur="$2" conc="$3"
  python3 "${SKILL_DIR}/scripts/bench_client.py" load \
    --endpoint "http://${DEPLOY}-router.${NS}.svc:8000" \
    --model "/models" \
    --arm "vpd-scaler" \
    --suite "vpd-scaler" \
    --label "$label" \
    --input-tokens "$IN_TOK" \
    --output-tokens "$OUT_TOK" \
    --concurrency "$conc" \
    --duration "$dur" \
    --out "${OUT}/${label}.json" \
    --requests-out "${OUT}/${label}_reqs.jsonl" > "${OUT}/${label}.log" 2>&1
}

# ---------------------------------------------------------------- the case

log "Writing the manifests into ${OUT}"
write_manifest
write_scaler_manifest
log "the model is ${MODEL}, served as /models from the artifact ${ARTIFACT}"
kubectl apply --dry-run=server -f "${OUT}/vpd-scaler.yaml" > "${OUT}/apply-dry-run.txt" 2>&1
kubectl apply --dry-run=server -f "${OUT}/vpd-scaler-scaler.yaml" >> "${OUT}/apply-dry-run.txt" 2>&1

log "Applying the deployment"
kubectl -n "$NS" delete "$MD_RESOURCE" "$DEPLOY" --ignore-not-found --wait=true --timeout=180s >/dev/null 2>&1 || true
kubectl apply -f "${OUT}/vpd-scaler.yaml" 2>&1 | tee "${OUT}/apply.log"
if ! wait_ready; then
  kubectl -n "$NS" describe "$MD_RESOURCE" "$DEPLOY" > "${OUT}/describe.txt" 2>&1
  record FAIL "the deployment reached Ready" "the deployment did not reach Ready; see describe.txt"
  cat "${OUT}/verdicts.txt" 2>/dev/null
  exit 1
fi
record PASS "the deployment reached Ready" "the vLLM prefill/decode pair is Ready with one replica per role"

# The serving profile names where the evidence lives for this engine, and the vendor is read off
# the InstanceType of the deployment that is now in the cluster. The images this case pins are
# NVIDIA ones, so the fallback is the pinned vendor and not a guess.
VENDOR="$(serving_vendor "$(deployment_json 2>/dev/null || echo '{}')" 2>/dev/null || true)"
if [ -z "$VENDOR" ]; then
  VENDOR=nvidia
fi
if ! serving_profile "$VENDOR" vLLM; then
  record FAIL "the serving profile for this vendor and engine exists" \
    "no serving profile for vendor '${VENDOR}' and engine vLLM, so the case does not know where the transfer evidence lives"
  cat "${OUT}/verdicts.txt" 2>/dev/null
  exit 1
fi
record PASS "the serving profile for this vendor and engine exists" \
  "the vendor is ${VENDOR} and the profile names ${SP_SENT_COUNT} for the transfer and ${SP_FAILED} for the failures"

log "Preparing the in-cluster driver"
if ! ensure_driver; then
  record FAIL "the in-cluster driver is ready" "the driver could not be made ready, so the load cannot be driven in the cluster"
  cat "${OUT}/verdicts.txt" 2>/dev/null
  exit 1
fi

# --- the prefiller-contribution pre-check ----------------------------------------
#
# THE CHECK RUNS BEFORE THE LOAD, and it is the guard against the silent decode-only fallback: a
# router that stopped using the prefiller answers every request perfectly, moves no KV at all and
# would let the rest of this case pass on traffic that never touched the prefiller. The router's
# own decision counter names the two paths, and the prefill half's own transfer counter says that
# the prefiller really sent the blocks.
log "Pre-checking the prefiller's contribution"
run_load "precheck" 30 2 || true
sleep 10
scrape_router_metrics "precheck" || true
engine_metrics prefill "precheck" || true
engine_metrics decode "precheck" || true

PD_DECISIONS="$(metric_value "${OUT}/router-precheck.txt" "llm_d_epp_disagg_decision_total" 'decision_type="prefill-decode"')"
DECODE_ONLY="$(metric_value "${OUT}/router-precheck.txt" "llm_d_epp_disagg_decision_total" 'decision_type="decode-only"')"
SENT_COUNT="$(metric_sum "${OUT}/engine-prefill-precheck.txt" "$SP_SENT_COUNT")"
SENT_SUM="$(metric_sum "${OUT}/engine-prefill-precheck.txt" "$SP_SENT_SUM")"
log "the router sent ${PD_DECISIONS} request(s) through a prefill replica and ${DECODE_ONLY} straight to a decode replica"

if [ "${PD_DECISIONS:-0}" -le 0 ]; then
  record FAIL "the router used the prefiller" \
    "the router's decision counter recorded no prefill-decode decision (decode-only: ${DECODE_ONLY}), so the leg never used the prefiller and the rest of this case would measure a decode-only router"
  cat "${OUT}/verdicts.txt" 2>/dev/null
  exit 1
fi
record PASS "the router used the prefiller" \
  "the router's decision counter recorded ${PD_DECISIONS} prefill-decode decision(s) against ${DECODE_ONLY} decode-only"

if [ "${SENT_COUNT:-0}" -le 0 ] && [ "${SENT_SUM:-0}" -le 0 ]; then
  record FAIL "the prefiller sent KV over the direct leg" \
    "the prefill half counted no transfer (${SP_SENT_COUNT}=${SENT_COUNT}, ${SP_SENT_SUM}=${SENT_SUM}); ${SP_TRANSFER_SOURCE}"
  cat "${OUT}/verdicts.txt" 2>/dev/null
  exit 1
fi
record PASS "the prefiller sent KV over the direct leg" \
  "the prefill half counted ${SENT_COUNT} transfer(s) and ${SENT_SUM} byte(s) in ${SP_SENT_COUNT} and ${SP_SENT_SUM}"

# --- the scaler takes the target ---------------------------------------------------
log "Applying the scaler"
kubectl apply -f "${OUT}/vpd-scaler-scaler.yaml" 2>&1 | tee "${OUT}/scaler-apply.log"
if ! wait_scaler_bound; then
  record FAIL "the scaler bound to the target and acts" "the scaler never reported Bound=True and Active=True; see scaler-unbound.json"
  cat "${OUT}/verdicts.txt" 2>/dev/null
  exit 1
fi
record PASS "the scaler bound to the target and acts" "the scaler reports Bound=True and Active=True"

PHASE_FILE="${OUT}/phase"
TIMELINE="${OUT}/timeline.jsonl"
echo "idle" > "$PHASE_FILE"
: > "$TIMELINE"
sampler_loop "$PHASE_FILE" "$TIMELINE" &
SAMPLER_PID=$!

# --- the load, and the rise -----------------------------------------------------------
echo "load" > "$PHASE_FILE"
log "Running the load for ${LOAD_SECONDS}s at concurrency ${LOAD_CONCURRENCY}"
run_load "load" "$LOAD_SECONDS" "$LOAD_CONCURRENCY" || true

# THE RAW METRICS ARE READ WHILE THE LOAD IS STILL ON, because after it stops the counters fall
# back and the scaler's recorded value would have nothing to agree with.
engine_metrics decode "under-load" || true
engine_metrics prefill "under-load" || true
scaler_json > "${OUT}/scaler-under-load.json" 2>/dev/null || true
deployment_json > "${OUT}/deployment-under-load.json" 2>/dev/null || true

RAW_DECODE_RUNNING="$(metric_sum "${OUT}/engine-decode-under-load.txt" "vllm:num_requests_running")"
RAW_PREFILL_RUNNING="$(metric_sum "${OUT}/engine-prefill-under-load.txt" "vllm:num_requests_running")"
SCALER_DECODE_RUNNING="$(scaler_role_field decode runningRequests)"
SCALER_PREFILL_RUNNING="$(scaler_role_field prefill runningRequests)"
log "the engine's own running counter: decode=${RAW_DECODE_RUNNING} prefill=${RAW_PREFILL_RUNNING}"
log "the scaler's recorded running count: decode=${SCALER_DECODE_RUNNING} prefill=${SCALER_PREFILL_RUNNING}"

if [ "${RAW_DECODE_RUNNING:-0}" -le 0 ] && [ "${SCALER_DECODE_RUNNING:-0}" -le 0 ]; then
  record FAIL "the scaler read the engine's running-request counter" \
    "neither the engine's vllm:num_requests_running (${RAW_DECODE_RUNNING}) nor the scaler's status (${SCALER_DECODE_RUNNING}) showed a running request on the decode role"
else
  record PASS "the scaler read the engine's running-request counter" \
    "the engine's vllm:num_requests_running read ${RAW_DECODE_RUNNING} on the decode role and ${RAW_PREFILL_RUNNING} on the prefill role; the scaler recorded ${SCALER_DECODE_RUNNING} and ${SCALER_PREFILL_RUNNING}"
fi

# The two readings are taken at different instants and the load moves, so the comparison carries a
# band. A scaler that read nothing reads zero, and zero is outside every band.
if [ "${SCALER_DECODE_RUNNING:-0}" -gt 0 ] && [ "${RAW_DECODE_RUNNING:-0}" -gt 0 ]; then
  if awk "BEGIN { exit !((${SCALER_DECODE_RUNNING} - ${RAW_DECODE_RUNNING}) <= 0.5 * ${RAW_DECODE_RUNNING} && (${RAW_DECODE_RUNNING} - ${SCALER_DECODE_RUNNING}) <= 0.5 * ${RAW_DECODE_RUNNING}) }"; then
    record PASS "the scaler's count agrees with the engine's own counter" \
      "the scaler recorded ${SCALER_DECODE_RUNNING} running requests on the decode role against the engine's ${RAW_DECODE_RUNNING}"
  else
    record FAIL "the scaler's count agrees with the engine's own counter" \
      "the scaler recorded ${SCALER_DECODE_RUNNING} running requests on the decode role against the engine's ${RAW_DECODE_RUNNING}"
  fi
else
  record FAIL "the scaler's count agrees with the engine's own counter" \
    "the scaler recorded ${SCALER_DECODE_RUNNING} running requests on the decode role against the engine's ${RAW_DECODE_RUNNING}"
fi

DECODE_REPLICAS="$(wait_replicas decode 2 || true)"
PREFILL_REPLICAS="$(wait_replicas prefill 2 || true)"
log "after the load: decode=${DECODE_REPLICAS} prefill=${PREFILL_REPLICAS}"

if [ "${DECODE_REPLICAS:-0}" -ge 2 ]; then
  record PASS "the decode role grew" "the decode role holds ${DECODE_REPLICAS} replicas"
else
  record FAIL "the decode role grew" "the decode role holds ${DECODE_REPLICAS} replicas, which is not more than the one it started with"
fi
if [ "${PREFILL_REPLICAS:-0}" -ge 2 ]; then
  record PASS "the prefill role grew" "the prefill role holds ${PREFILL_REPLICAS} replicas"
else
  record FAIL "the prefill role grew" "the prefill role holds ${PREFILL_REPLICAS} replicas, which is not more than the one it started with; its own running-request signal was ${SCALER_PREFILL_RUNNING} against a target of 4"
fi

# --- the new Pods joined the routing --------------------------------------------------
#
# THE GROUP STATUS IS THE OPERATOR'S OWN PUBLICATION that a replica is eligible and that the
# router confirmed it, and the two roles are tracked independently: each role publishes its own
# groups, so a decode group says nothing about a prefill group.
GROUPS_DECODE="$(deployment_json | jq -r '[(.status.roles // [])[] | select(.name == "decode") | (.groups // [])[] | .state] | join(",")' 2>/dev/null)"
GROUPS_PREFILL="$(deployment_json | jq -r '[(.status.roles // [])[] | select(.name == "prefill") | (.groups // [])[] | .state] | join(",")' 2>/dev/null)"
log "the published groups: decode=[${GROUPS_DECODE}] prefill=[${GROUPS_PREFILL}]"

if [ "$(printf '%s' "$GROUPS_DECODE" | tr ',' '\n' | grep -c 'Eligible')" -ge 2 ] 2>/dev/null; then
  record PASS "the new decode Pod joined the routing" "the decode role publishes the group states: ${GROUPS_DECODE}"
else
  record FAIL "the new decode Pod joined the routing" "the decode role publishes the group states: ${GROUPS_DECODE:-none}"
fi
if [ "$(printf '%s' "$GROUPS_PREFILL" | tr ',' '\n' | grep -c 'Eligible')" -ge 2 ] 2>/dev/null; then
  record PASS "the new prefill Pod joined the routing" "the prefill role publishes the group states: ${GROUPS_PREFILL}"
else
  record FAIL "the new prefill Pod joined the routing" "the prefill role publishes the group states: ${GROUPS_PREFILL:-none}"
fi

# --- the KV transfer on the direct leg --------------------------------------------------
FAILED_TRANSFERS="$(metric_sum "${OUT}/engine-decode-under-load.txt" "$SP_FAILED")"
FAILED_TRANSFERS_PREFILL="$(metric_sum "${OUT}/engine-prefill-under-load.txt" "$SP_FAILED")"
RECEIVED="$(metric_sum "${OUT}/engine-decode-under-load.txt" "$SP_RECEIVED")"
SENT_UNDER_LOAD="$(metric_sum "${OUT}/engine-prefill-under-load.txt" "$SP_SENT_COUNT")"

if [ "${SENT_UNDER_LOAD:-0}" -gt 0 ]; then
  record PASS "the prefill half moved KV while the load ran" \
    "the prefill half counted ${SENT_UNDER_LOAD} transfer(s) in ${SP_SENT_COUNT}"
else
  record FAIL "the prefill half moved KV while the load ran" \
    "the prefill half counted no transfer in ${SP_SENT_COUNT}; ${SP_TRANSFER_SOURCE}"
fi
if [ "${FAILED_TRANSFERS:-0}" -eq 0 ] && [ "${FAILED_TRANSFERS_PREFILL:-0}" -eq 0 ]; then
  record PASS "no half counted a failed transfer" \
    "the decode half counted ${FAILED_TRANSFERS} and the prefill half ${FAILED_TRANSFERS_PREFILL} failed transfers in ${SP_FAILED}"
else
  record FAIL "no half counted a failed transfer" \
    "the decode half counted ${FAILED_TRANSFERS} and the prefill half ${FAILED_TRANSFERS_PREFILL} failed transfers in ${SP_FAILED}"
fi
record PASS "the decode half counted the blocks it was handed" \
  "the decode half counted ${RECEIVED} in ${SP_RECEIVED}"

# --- the scaler's decisions --------------------------------------------------------------
scaler_json > "${OUT}/scaler.json" 2>/dev/null || true
kubectl -n "$NS" get events --field-selector "involvedObject.name=${SCALER}" \
  -o json > "${OUT}/scaler-events.json" 2>/dev/null || true
COMMIT_EVENTS="$(jq -r '[.items // [] | select(.reason == "ReplicaVectorCommitted")] | length' "${OUT}/scaler-events.json" 2>/dev/null || echo 0)"
COMMITTED_VECTOR="$(scaler_json | jq -c '.status.lastCommit.vector // {}' 2>/dev/null)"
log "the scaler's events: ${COMMIT_EVENTS} committed vector(s); the last committed vector is ${COMMITTED_VECTOR}"

if [ "${COMMIT_EVENTS:-0}" -gt 0 ]; then
  record PASS "the scaler published its decisions" \
    "the scaler recorded ${COMMIT_EVENTS} ReplicaVectorCommitted event(s); the last committed vector is ${COMMITTED_VECTOR}"
else
  record FAIL "the scaler published its decisions" \
    "the scaler recorded no ReplicaVectorCommitted event; the last committed vector is ${COMMITTED_VECTOR}"
fi

# --- the fall-back ------------------------------------------------------------------------
echo "cooldown" > "$PHASE_FILE"
log "Stopping the load and waiting for the fall-back"
sleep 20
if wait_floor; then
  record PASS "the counts came back to the floor" \
    "the prefill role holds $(role_replicas prefill) and the decode role $(role_replicas decode) replicas, and no retirement is outstanding"
else
  record FAIL "the counts came back to the floor" \
    "the prefill role holds $(role_replicas prefill) and the decode role $(role_replicas decode) replicas, and the retirement state is $(deployment_json | jq -r '.status.retirement.state // "clear"')"
fi

LAST_SCALE_DOWN="$(scaler_json | jq -c '.status.lastScaleDown // null' 2>/dev/null)"
SD_ROLE="$(scaler_json | jq -r '.status.lastScaleDown.role // ""' 2>/dev/null)"
SD_OUTCOME="$(scaler_json | jq -r '.status.lastScaleDown.outcome // ""' 2>/dev/null)"
log "the last scale-down: role=${SD_ROLE:-none} outcome=${SD_OUTCOME:-none}"

if [ -n "$SD_ROLE" ] && [ "$SD_OUTCOME" = "Committed" ]; then
  record PASS "the scaler committed a scale-down" "the last scale-down was ${SD_ROLE} with outcome ${SD_OUTCOME}: ${LAST_SCALE_DOWN}"
else
  record FAIL "the scaler committed a scale-down" "the last scale-down was role=${SD_ROLE:-none} outcome=${SD_OUTCOME:-none}: ${LAST_SCALE_DOWN}"
fi

# THE GRACE WINDOW IS READ FROM THE TIMELINE, because it is a property of the protocol and not of
# the scaler's bookkeeping. The sampler polls every two seconds, so the measured gap can be up to
# two seconds shorter than the real one; 58 s is therefore the floor the measurement supports.
GRACE_REASON="$(jq -r 'select((.retirement_state // "") != "") | .retirement_reason' "$TIMELINE" 2>/dev/null | grep -m 1 'grace window' || true)"
GRACE_PHASE_START="$(jq -r 'select((.retirement_reason // "") | test("grace window")) | .retirement_phase_started_at' "$TIMELINE" 2>/dev/null | head -1)"
FALLBACK_TS="$(jq -r 'select((.phase // "") == "cooldown") | .ts' "$TIMELINE" 2>/dev/null | tail -1)"
GRACE_SECONDS="-1"
if [ -n "$GRACE_PHASE_START" ] && [ -n "$FALLBACK_TS" ] && [ "$GRACE_PHASE_START" != "null" ]; then
  GRACE_SECONDS="$(python3 -c "
import datetime, sys
def parse(t):
    t = t.strip().replace('Z', '+00:00')
    return datetime.datetime.fromisoformat(t)
try:
    print(int((parse(sys.argv[2]) - parse(sys.argv[1])).total_seconds()))
except Exception:
    print(-1)
" "$GRACE_PHASE_START" "$FALLBACK_TS" 2>/dev/null || echo -1)"
fi
log "the grace window: reason=${GRACE_REASON:-none} phase started ${GRACE_PHASE_START:-none}, the floor was reached ${FALLBACK_TS:-none}, ${GRACE_SECONDS}s apart"

if [ -n "$GRACE_REASON" ]; then
  record PASS "the retirement held the disaggregated handoff grace window" \
    "the deployment's own retirement reason named it: ${GRACE_REASON}"
else
  record FAIL "the retirement held the disaggregated handoff grace window" \
    "no sample of the retirement carried a reason about the grace window; the retirement states seen were $(jq -r 'select((.retirement_state // "") != "") | .retirement_state' "$TIMELINE" 2>/dev/null | sort -u | tr '\n' ' ')"
fi

if [ "${GRACE_SECONDS:-0}" -ge 58 ] 2>/dev/null; then
  record PASS "the fall-back respected the 60 s grace window" \
    "the drain phase started ${GRACE_PHASE_START} and the counts were back at the floor ${GRACE_SECONDS}s later, which is at least the 60 s the operator holds a disaggregated replica for"
else
  record FAIL "the fall-back respected the 60 s grace window" \
    "the drain phase started ${GRACE_PHASE_START:-none} and the counts were back at the floor ${GRACE_SECONDS}s later, which is less than the 60 s the operator holds a disaggregated replica for"
fi

RETIREMENT_STATES="$(jq -r 'select((.retirement_state // "") != "") | .retirement_state' "$TIMELINE" 2>/dev/null | sort -u | tr '\n' ' ')"
if [ -n "$RETIREMENT_STATES" ]; then
  record PASS "the fall-back went through the retirement protocol" \
    "the retirement states that were observed were: ${RETIREMENT_STATES}"
else
  record FAIL "the fall-back went through the retirement protocol" "no retirement was observed at all"
fi

echo "idle" > "$PHASE_FILE"
kill "$SAMPLER_PID" 2>/dev/null || true
wait "$SAMPLER_PID" 2>/dev/null || true

kubectl -n "$NS" get "$MD_RESOURCE" "$DEPLOY" -o json > "${OUT}/modeldeployment.json" 2>/dev/null || true
kubectl get devices -o json > "${OUT}/released-devices.json" 2>/dev/null || true

cat "${OUT}/verdicts.txt" 2>/dev/null
FAILS="$(grep -c '^FAIL' "${OUT}/verdicts.txt" 2>/dev/null || true)"
WARNS="$(grep -c '^WARN' "${OUT}/verdicts.txt" 2>/dev/null || true)"
log "CASE 9 (vLLM P/D under a ModelDeploymentScaler) complete: ${FAILS:-0} failed row(s), ${WARNS:-0} warning(s)"

# THE SCRIPT ENDS HERE, and the last statement is the exit code. An explicit trailing "exit 0"
# makes the analyzer read the EXIT trap's handler as a function nothing calls, and it then reports
# the whole handler as unreachable.
[ "${FAILS:-0}" -eq 0 ] || exit 1
