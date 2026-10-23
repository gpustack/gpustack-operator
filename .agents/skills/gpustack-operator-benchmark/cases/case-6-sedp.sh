#!/usr/bin/env bash
#
# CASE 6 — SGLang External DP: one group of two members behind the gateway   (MUTATING, self-recovering)
#
#   case-6-sedp.sh [OUT_DIR]
#
# Goal:        Exercise the SGLang External DP shape end to end: one instance is one SGLang
#              distributed launch spanning `size: 2` member Pods, one attention-DP group per member,
#              SGLANG_RUST_SERVER=1 so every member answers on its own listener, and the gateway
#              balances across the members. The case proves the four content checks on EVERY
#              member, scales the group count 1 -> 2 -> 1 and watches the gateway's worker set and
#              the group's endpoint count at each step, injects a non-head member fault and checks
#              that the WHOLE group is withdrawn, and collects the exclusion evidence that tells
#              SGLang External DP apart from internal data parallelism.
# Environment: A cluster with 4 free GPUs on one InstanceType, NodeModelStore caching
#              qwen3-8b, and an in-cluster default/bench-driver Pod. The model is Qwen/Qwen3-8B,
#              served from the qwen3-8b artifact the store caches: one GPU per member, two members
#              per group, two groups at peak = 4 GPUs.
# Inputs:      The manifest is written by this script into OUT_DIR/sedp.yaml, from the shape
#              documented in docs/modules/model-deployment/external-dp.md#sglang-external-dp. The
#              operator stamps GPUSTACK_MEMBER_INDEX and GPUSTACK_REPLICA_LEADER_ADDRESS, so the
#              manifest is identical for every member.
# Expected:    - the role derives parallelism.loadBalance External and every member answers;
#              - the group-forward probe (a real generation per member) succeeds on every member;
#              - ordinary, SSE, exact-Chinese and JSON content checks pass on every member;
#              - scaling 1 -> 2 grows the gateway worker set by exactly the new group's members;
#              - scaling 2 -> 1 withdraws the whole group atomically and takes at least 60s, because
#                the disaggregated handoff grace window runs before the member gauge reads;
#              - a non-head member going NotReady withdraws the whole group, and the survivor is
#                de-selected rather than left serving;
#              - the exclusion evidence records distinct member indexes, one generation covering
#                the member set, and per-door execution.
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
OUT="${1:-${REPO}/.claude/reports/$(date +%F)-benchmark/sedp}"
DEPLOY="sedp"
MODEL="${SEDP_MODEL:-Qwen/Qwen3-8B}"
# THE ARTIFACT IS WHAT THE DEPLOYMENT SERVES, and naming it is what keeps the pod alive. Without
# an artifactRef the operator renders the model as the repository and the engine downloads the
# weights itself onto the container's writable layer -- 16GB for Qwen3-8B against the 15Gi
# ephemeral limit the role derives -- so the pod is evicted while it is still starting, and the
# NodeModelStore cache the environment already warmed is never read. With one, the operator
# serves the weights from the delivery the artifact names and pins the served name below.
ARTIFACT="${SEDP_ARTIFACT:-qwen3-8b}"
IMAGE="${SEDP_IMAGE:-gpustack/runner:cuda13.0-sglang0.5.22}"
ROUTER_IMAGE="${SEDP_ROUTER_IMAGE:-gpustack/llm-router:v0.3.1}"
INSTANCE_TYPE="${SEDP_INSTANCE_TYPE:-gpustack--nvidia-h100-80gb-hbm3-linux-amd64}"
LOCAL_PORT_BASE="${LOCAL_PORT_BASE:-18100}"
READY_TIMEOUT="${READY_TIMEOUT:-1800}"
SCALE_TIMEOUT="${SCALE_TIMEOUT:-900}"
# The disaggregated handoff grace window the drain runs before the member gauge reads. The
# scale-down of an External-classified group cannot finish faster than this.
GRACE_SECONDS="${SEDP_GRACE_SECONDS:-60}"
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

# ---------------------------------------------------------------------------
# The manifest. One Server role, two members, one attention-DP group each.
# ---------------------------------------------------------------------------
write_manifest() {
  cat > "${OUT}/sedp.yaml" <<YAML
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
      size: 2
      instanceType: ${INSTANCE_TYPE}
      image: ${IMAGE}
      imagePullPolicy: IfNotPresent
      resources:
        accelerator: "1"
      extraArgs:
        - --tp-size
        - "2"
        - --attn-dp-size
        - "2"
        - --nnodes
        - "2"
        - --node-rank
        - \$(GPUSTACK_MEMBER_INDEX)
        - --dist-init-addr
        - \$(GPUSTACK_REPLICA_LEADER_ADDRESS):29500
      env:
        - name: SGLANG_RUST_SERVER
          value: "1"
YAML
}

# member_pod prints the Pod name of one member of one replica group, by its member index.
member_pod() {
  local rank="$1" replica="${2:-0}"
  kubectl -n "$NS" get pods \
    -l "app.kubernetes.io/instance=${DEPLOY},app.kubernetes.io/component=server,modeldeployment.gpustack.ai/member-index=${rank},modeldeployment.gpustack.ai/replica-index=${replica}" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

# member_pods prints every member Pod name of the deployment, one per line.
member_pods() {
  kubectl -n "$NS" get pods \
    -l "app.kubernetes.io/instance=${DEPLOY},app.kubernetes.io/component=server" \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null
}

# start_member_forward opens a port-forward to one member's own listener and waits for it to
# answer. The member's listener is its own door: the Rust server's listener IS that scheduler's
# ingress, so a request that arrives here crossed the collective through this member.
start_member_forward() {
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

# router_metrics_port reads the Prometheus port the operator rendered for the gateway, rather
# than assuming one.
router_metrics_port() {
  local pod args
  pod="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${DEPLOY},app.kubernetes.io/component=router" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
  [ -n "$pod" ] || return 1
  args="$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.spec.containers[0].args}' 2>/dev/null)"
  printf '%s' "$args" | sed -n 's/.*--prometheus-port=\([0-9][0-9]*\).*/\1/p' | head -1
}

# scrape_router_metrics writes the gateway's own metric page. The per-worker series carry the
# worker URL, so they are the instrument for per-member state; the selection series do not carry
# a worker label and must never be cited for distribution.
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

# group_status writes the deployment's roles, parallelism and endpoints.
group_status() {
  local name="$1"
  kubectl -n "$NS" get modeldeployment "$DEPLOY" -o json > "${OUT}/${name}-modeldeployment.json" 2>/dev/null
  jq '.status.roles[] | {name, kind, ready, parallelism, endpoints}' "${OUT}/${name}-modeldeployment.json" \
    > "${OUT}/${name}-roles.json" 2>/dev/null
  cat "${OUT}/${name}-roles.json"
}

# wait_ready waits for the deployment's phase to become Ready.
wait_ready() {
  local deadline=$((SECONDS + READY_TIMEOUT)) phase
  while [ "$SECONDS" -lt "$deadline" ]; do
    phase="$(kubectl -n "$NS" get modeldeployment "$DEPLOY" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    [ "$phase" = "Ready" ] && return 0
    sleep 5
  done
  return 1
}

# wait_workers waits until the gateway's worker set reaches the expected size, and prints the
# number of workers it saw. The worker identity is the worker URL, so one worker is one member.
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

# ---------------------------------------------------------------------------
# The content checks. Every one of them runs against a MEMBER's own door, not the gateway's.
# ---------------------------------------------------------------------------
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

# check_chinese asks for one Chinese sentence and requires CJK in the answer. The check is the
# CONTENT, not the status: a 200 with an English answer is a failed check.
check_chinese() {
  local port="$1" out="$2"
  curl --fail-with-body --max-time 120 "http://127.0.0.1:${port}/v1/completions" \
    -H 'Content-Type: application/json' \
    -d '{"model":"/models","prompt":"请用一句中文说明什么是 GPU。","max_tokens":64,"temperature":0}' \
    -o "$out" 2>"${out}.err"
}

# check_json asks for a JSON object and requires the answer to parse as one.
check_json() {
  local port="$1" out="$2"
  curl --fail-with-body --max-time 120 "http://127.0.0.1:${port}/v1/completions" \
    -H 'Content-Type: application/json' \
    -d '{"model":"/models","prompt":"Return a JSON object with the key name and the value gpu.","max_tokens":64,"temperature":0,"response_format":{"type":"json_object"}}' \
    -o "$out" 2>"${out}.err"
}

# ---------------------------------------------------------------------------
# The run.
# ---------------------------------------------------------------------------
log "The model is ${MODEL}, served as /models from the artifact ${ARTIFACT}"
log "Writing the manifest into ${OUT}/sedp.yaml"
write_manifest
kubectl apply --dry-run=server -f "${OUT}/sedp.yaml" > "${OUT}/apply-dry-run.txt" 2>&1

log "Applying the deployment"
kubectl -n "$NS" delete modeldeployment "$DEPLOY" --ignore-not-found --wait=true --timeout=180s >/dev/null 2>&1 || true
kubectl apply -f "${OUT}/sedp.yaml" 2>&1 | tee "${OUT}/apply.log"

if ! wait_ready; then
  log "TIMEOUT: the deployment did not reach Ready"
  kubectl -n "$NS" describe modeldeployment "$DEPLOY" > "${OUT}/describe.txt" 2>&1
  kubectl -n "$NS" get pods -o wide > "${OUT}/pods.txt" 2>&1
  exit 1
fi
log "The deployment is Ready"

# --- the classification and the shape -------------------------------------
group_status "initial" | tee "${OUT}/initial-roles.txt"
kubectl -n "$NS" get modeldeployment "$DEPLOY" -o json \
  | jq '{phase: .status.phase, roles: [.spec.roles[] | {name, kind, replicas, size}]}' \
  > "${OUT}/initial-shape.json" 2>/dev/null

# --- the member set, and the exclusion evidence ---------------------------
kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${DEPLOY}" -o wide > "${OUT}/pods-initial.txt" 2>&1
kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${DEPLOY}" -o json \
  | jq '[.items[] | {pod: .metadata.name, member_index: .metadata.labels["modeldeployment.gpustack.ai/member-index"],
                    replica_index: .metadata.labels["modeldeployment.gpustack.ai/replica-index"],
                    uid: .metadata.uid, pod_ip: .status.podIP, phase: .status.phase}]' \
  > "${OUT}/exclusion-members.json" 2>/dev/null

# The generation is the group verdict's identity. One group carries one generation covering the
# whole member set, so the sorted member-UID set is recorded with it.
kubectl -n "$NS" get modeldeployment "$DEPLOY" -o json \
  | jq '[.status.roles[].endpoints | .. | objects | select(has("generation")) | .generation] | unique' \
  > "${OUT}/exclusion-generation.json" 2>/dev/null
jq -r '[.[] | .uid] | sort | join(",")' "${OUT}/exclusion-members.json" \
  > "${OUT}/exclusion-member-uid-set.txt" 2>/dev/null

# --- per-member probes ---------------------------------------------------
MEMBERS="$(member_pods)"
log "Members: ${MEMBERS}"
index=0
for pod in $MEMBERS; do
  port=$((LOCAL_PORT_BASE + index))
  index=$((index + 1))
  log "Probing member ${pod} on local port ${port}"
  if ! start_member_forward "$pod" "$port"; then
    log "FAIL: member ${pod} did not answer its own /health"
    echo "${pod}" >> "${OUT}/failed-members.txt"
    continue
  fi
  check_ordinary "$port" "${OUT}/member-${index}-ordinary.json"
  check_sse "$port" "${OUT}/member-${index}-sse.txt"
  check_chinese "$port" "${OUT}/member-${index}-chinese.json"
  check_json "$port" "${OUT}/member-${index}-json.json"
done
stop_forwards

# The content verdicts, collected from what was written.
for f in "${OUT}"/member-*-ordinary.json; do
  [ -e "$f" ] || continue
  jq -e '.choices[0].text | length > 0' "$f" >/dev/null 2>&1 \
    && echo "PASS $(basename "$f")" >> "${OUT}/content-verdicts.txt" \
    || echo "FAIL $(basename "$f")" >> "${OUT}/content-verdicts.txt"
done
for f in "${OUT}"/member-*-sse.txt; do
  [ -e "$f" ] || continue
  grep -q 'data: \[DONE\]' "$f" && echo "PASS $(basename "$f")" >> "${OUT}/content-verdicts.txt" \
    || echo "FAIL $(basename "$f")" >> "${OUT}/content-verdicts.txt"
done
for f in "${OUT}"/member-*-chinese.json; do
  [ -e "$f" ] || continue
  jq -e '.choices[0].text' "$f" 2>/dev/null | grep -qP '[\x{4e00}-\x{9fff}]' \
    && echo "PASS $(basename "$f")" >> "${OUT}/content-verdicts.txt" \
    || echo "FAIL $(basename "$f")" >> "${OUT}/content-verdicts.txt"
done
for f in "${OUT}"/member-*-json.json; do
  [ -e "$f" ] || continue
  # THE ANSWER MUST PARSE TO AN OBJECT. Parsing it as any JSON value is not enough: a bare string
  # is a valid JSON document, so "jq -e ." would call a plain English answer a pass.
  jq -e '.choices[0].text | fromjson | type == "object"' "$f" >/dev/null 2>&1 \
    && echo "PASS $(basename "$f")" >> "${OUT}/content-verdicts.txt" \
    || echo "FAIL $(basename "$f")" >> "${OUT}/content-verdicts.txt"
done
cat "${OUT}/content-verdicts.txt" 2>/dev/null

# --- the gateway's worker set at one group --------------------------------
scrape_router_metrics "workers-one-group" || true
grep '^smg_worker_health{' "${OUT}/workers-one-group.txt" > "${OUT}/worker-health-one-group.txt" 2>/dev/null || true
grep '^smg_worker_requests_active{' "${OUT}/workers-one-group.txt" > "${OUT}/worker-active-one-group.txt" 2>/dev/null || true

# --- scale 1 -> 2 ---------------------------------------------------------
log "Scaling the group count 1 -> 2"
start_epoch="$(date +%s)"
kubectl -n "$NS" patch modeldeployment "$DEPLOY" --type=merge \
  -p '{"spec":{"roles":[{"name":"server","replicas":2}]}}' > "${OUT}/scale-up-patch.txt" 2>&1
workers_after_up="$(wait_workers 4 || true)"
log "The gateway's worker set after the scale-up: ${workers_after_up}"
group_status "after-scale-up" | tee "${OUT}/after-scale-up-roles.txt"

# --- scale 2 -> 1 ---------------------------------------------------------
log "Scaling the group count 2 -> 1. The drain runs the ${GRACE_SECONDS}s handoff grace window."
kubectl -n "$NS" patch modeldeployment "$DEPLOY" --type=merge \
  -p '{"spec":{"roles":[{"name":"server","replicas":1}]}}' > "${OUT}/scale-down-patch.txt" 2>&1
scale_down_start="$(date +%s)"
deadline=$((SECONDS + SCALE_TIMEOUT))
while [ "$SECONDS" -lt "$deadline" ]; do
  replicas="$(kubectl -n "$NS" get modeldeployment "$DEPLOY" -o jsonpath='{.spec.roles[0].replicas}' 2>/dev/null || true)"
  [ "$replicas" = "1" ] && break
  sleep 5
done
# The group is withdrawn as a unit: no surviving member stays routed.
while [ "$SECONDS" -lt "$deadline" ]; do
  workers_after_down="$(scrape_router_metrics "workers-after-scale-down" >/dev/null 2>&1 \
    && grep -c '^smg_worker_health{' "${OUT}/workers-after-scale-down.txt" 2>/dev/null || echo 0)"
  [ "${workers_after_down:-0}" -le 2 ] && break
  sleep 5
done
scale_down_end="$(date +%s)"
scale_down_seconds=$((scale_down_end - scale_down_start))
echo "scale_down_seconds=${scale_down_seconds}" >> "${OUT}/scale-down-timing.txt"
echo "scale_up_seconds=$((scale_down_end - start_epoch))" >> "${OUT}/scale-down-timing.txt"
log "Scale-down took ${scale_down_seconds}s; the expectation is at least ${GRACE_SECONDS}s"
# THE VERDICT BELONGS IN THE EVIDENCE, not only the number: the acceptance record has to carry
# whether the contract held, so a reader of the evidence does not have to do the comparison.
if [ "${scale_down_seconds}" -ge "${GRACE_SECONDS}" ]; then
  echo "PASS scale_down_seconds=${scale_down_seconds} is at least the ${GRACE_SECONDS}s handoff grace window" \
    >> "${OUT}/scale-down-timing.txt"
else
  echo "FAIL scale_down_seconds=${scale_down_seconds} is shorter than the ${GRACE_SECONDS}s handoff grace window" \
    >> "${OUT}/scale-down-timing.txt"
fi
group_status "after-scale-down" | tee "${OUT}/after-scale-down-roles.txt"

# --- the fault injection row ----------------------------------------------
# A non-head member going NotReady must withdraw the WHOLE group, and the survivor must be
# de-selected rather than left serving a collective whose peer is gone.
log "Injecting a non-head member fault"
FAULT_POD="$(member_pod 1 0)"
if [ -n "$FAULT_POD" ]; then
  kubectl -n "$NS" label pod "$FAULT_POD" "e2e.gpustack.ai/fault-injected=true" --overwrite \
    > "${OUT}/fault-label.txt" 2>&1
  # The label alone does not change readiness; the readiness gate is what the probe reads. The
  # case records what it did and what the group did, so the evidence names its own method.
  kubectl -n "$NS" exec "$FAULT_POD" -c main -- sh -c 'kill -STOP 1' \
    > "${OUT}/fault-inject.txt" 2>&1 || true
  sleep 30
  group_status "after-fault" | tee "${OUT}/after-fault-roles.txt"
  scrape_router_metrics "workers-after-fault" || true
  grep '^smg_worker_health{' "${OUT}/workers-after-fault.txt" > "${OUT}/worker-health-after-fault.txt" 2>/dev/null || true
  kubectl -n "$NS" exec "$FAULT_POD" -c main -- sh -c 'kill -CONT 1' \
    > "${OUT}/fault-restore.txt" 2>&1 || true
else
  echo "no non-head member found" > "${OUT}/fault-inject.txt"
fi

# --- the counter, collected once -----------------------------------------
bash "${SKILL_DIR}/scripts/stage_counts.sh" "$DEPLOY" "$OUT" > "${OUT}/stage-counts.txt" 2>&1
kubectl get devices -o json > "${OUT}/released-devices.json" 2>/dev/null || true

log "CASE 6 (SGLang External DP) complete"
