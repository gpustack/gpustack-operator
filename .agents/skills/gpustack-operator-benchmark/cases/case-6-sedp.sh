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
#              The shape this run was on is written to OUT_DIR/cluster-shape.txt: the context and
#              the cluster, the namespace, the nodes, the InstanceType the case asked for, the
#              accelerators on each node, the InstanceTypes the cluster carries, and the operator
#              image with its digest.
#              The cluster of record for this run: gpustack-sedp-0l578ap2 (ns gpustack-bench),
#              platform gpu-h100-sxm 8xH100 non-preemptible with a cpu-e2 system node, 8
#              accelerators on the one GPU node, InstanceType
#              gpustack--nvidia-h100-80gb-hbm3-linux-amd64, operator
#              <dev>/gpustack-operator:<tag> (a development image; tests pull gpustack/gpustack-operator unless a dev image is named).
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
# shellcheck source=/dev/null
. "${SKILL_DIR}/../_e2e-lib/scripts/serving-lib.sh"

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
ROUTER_NAME="${SEDP_ROUTER_NAME:-sglang-gateway}"
INSTANCE_TYPE="${SEDP_INSTANCE_TYPE:-gpustack--nvidia-h100-80gb-hbm3-linux-amd64}"
LOCAL_PORT_BASE="${LOCAL_PORT_BASE:-18100}"
READY_TIMEOUT="${READY_TIMEOUT:-1800}"
SCALE_TIMEOUT="${SCALE_TIMEOUT:-900}"
# The budget the gateway's worker set is given to reach the members it should hold. The
# registration of a member is the gateway's own work, so the case waits for it, but the wait is
# bounded so that a member the gateway never registers does not spend the whole timeout.
WORKER_SETTLE_SECONDS="${SEDP_WORKER_SETTLE_SECONDS:-120}"
# The budget the fault row is given for the withdrawal to become visible. The operator finds the
# stopped member on its next observation, its group-forward probe is bounded at 15s, and the
# gateway polls on its own cadence, so the row waits for both instead of reading a fixed moment.
FAULT_SETTLE_SECONDS="${SEDP_FAULT_SETTLE_SECONDS:-300}"
# The disaggregated handoff grace window the drain runs before the member gauge reads. The
# scale-down of an External-classified group cannot finish faster than this.
GRACE_SECONDS="${SEDP_GRACE_SECONDS:-60}"
PF_PIDS=""

mkdir -p "$OUT"
log() { echo "[$(date +%H:%M:%S)] $*"; }

# THE SHAPE OF THE CLUSTER BELONGS IN THE EVIDENCE, so the numbers this run produced can be
# read against the machine that produced them.
capture_cluster_shape "${OUT}/cluster-shape.txt"

# record VERDICT LABEL DETAIL writes one row of the results table. The verdict belongs in the
# evidence, so a reader of the run does not have to re-derive it from the raw numbers.
record() {
  printf '%s | %s | %s\n' "$1" "$2" "$3" >> "${OUT}/verdicts.txt"
  case "$1" in
    FAIL) echo "FAIL | ${2} | ${3}" ;;
    BLOCKED) echo "BLOCKED | ${2} | ${3}" ;;
    WARN) echo "WARN | ${2} | ${3}" ;;
    *) echo "PASS | ${2} | ${3}" ;;
  esac
}

# count_verdict VERDICT prints how many rows of the results tables carry that verdict.
#
# THE TWO TABLES ARE COUNTED TOGETHER. The content rows and the role rows live in two files, and a
# summary that counts one of them reports a cell that passed while a check in the other failed.
# The count of a grep that matched nothing is a zero, so the exit status is ignored and the
# number the grep printed is the answer.
count_verdict() {
  local verdict="$1" total=0 file count
  for file in "${OUT}/verdicts.txt" "${OUT}/content-verdicts.txt"; do
    [ -e "$file" ] || continue
    count="$(grep -c "^${verdict}" "$file" 2>/dev/null || true)"
    [ -n "$count" ] || count=0
    total=$(( total + count ))
  done
  printf '%s' "$total"
}

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

# member_pods prints every member Pod name of the deployment, one per line.
member_pods() {
  kubectl -n "$NS" get pods \
    -l "app.kubernetes.io/instance=${DEPLOY},app.kubernetes.io/component=server" \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null
}

# subordinate_member_pod prints the name of a member that is not the head of its group.
#
# THE REPLICA IS NAMED BY THE POD ORDINAL AND THE MEMBER BY ITS INDEX. The operator stamps two
# labels on a role's Pods: modeldeployment.gpustack.ai/member-index says which member of its
# replica the Pod is, and modeldeployment.gpustack.ai/pod-ordinal says which replica of the role
# it belongs to (pkg/worker/controllers/worker/model_deployment_pod_group.go).
#
# THE SUBORDINATE IS FOUND BY ITS RANK, NOT BY ITS GROUP. A scale-down retires a group, and the
# ordinal that group carried belongs to the group that went, so a selector that names the ordinal
# asks for a member of a group that is already gone.
#
# THE GROUP IS THE GROUP THAT IS STILL ELIGIBLE. A retired group keeps its Pods for a while after
# the scale-down, and a selector that takes the first Pod of any group can hand back the retired
# group's member. A fault on the retired group's member measures a group that was already
# withdrawn and says nothing about the group the row is about, which is the group the operator
# still publishes as eligible.
subordinate_member_pod() {
  local eligible name index
  eligible="$(kubectl -n "$NS" get modeldeployment "$DEPLOY" -o json 2>/dev/null \
    | jq -r '.status.roles[].groups[]? | select(.state == "Eligible") | .members[]?.name' 2>/dev/null)"
  [ -n "${eligible}" ] || return 0
  for name in ${eligible}; do
    index="$(kubectl -n "$NS" get pod "$name" \
      -o jsonpath='{.metadata.labels.modeldeployment\.gpustack\.ai/member-index}' 2>/dev/null || echo "")"
    if [ -n "${index}" ] && [ "${index}" != "0" ]; then
      echo "${name}"
      return 0
    fi
  done
  # THE GROUP HAS ONE MEMBER, or none of them names its rank. The row still has a target to fault,
  # and the evidence says which one it was.
  echo "${eligible}" | head -1
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

# THE ROUTER POD IS FOUND BY THE LABELS THE OPERATOR RENDERS ON IT. The router is its own
# Deployment, and its Pod template carries the deployment's instance label and the router's own
# name label (pkg/worker/controllers/worker/model_deployment_router.go, the labels the router
# objects are rendered with). It carries no component label, so a selector that asks for one
# matches nothing, and the worker set below then reads as empty however many workers are there.
router_pod() {
  kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${DEPLOY},modeldeployment.gpustack.ai/router=${ROUTER_NAME}" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

# router_metrics_port reads the Prometheus port the operator rendered for the gateway, rather
# than assuming one.
router_metrics_port() {
  local pod args
  pod="$(router_pod)"
  [ -n "$pod" ] || return 1
  args="$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.spec.containers[0].args}' 2>/dev/null)"
  printf '%s' "$args" | sed -n 's/.*--prometheus-port=\([0-9][0-9]*\).*/\1/p' | head -1
}

# scrape_router_metrics writes the gateway's own metric page. The per-worker series carry the
# worker URL, so they are the instrument for per-member state; the selection series do not carry
# a worker label and must never be cited for distribution.
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

# healthy_workers NAME prints the number of healthy workers the gateway's scrape recorded.
#
# THE COUNT IS OF HEALTHY WORKERS, NOT OF METRIC LINES. The gateway keeps the series of a worker
# it withdrew and marks that series with the value -1, so counting the lines counted a worker
# that had already left.
healthy_workers() {
  local name="$1"
  awk '/^smg_worker_health\{/ && $NF == 1 { n++ } END { print n + 0 }' \
    "${OUT}/${name}.txt" 2>/dev/null || echo 0
}

# wait_worker_count EXPECTED BUDGET polls the gateway's worker set until it holds EXPECTED
# workers or the budget is spent, and prints the count it saw. The worker identity is the worker
# URL, so one worker is one member.
#
# THE BUDGET IS EXPLICIT, because a wait with no end spends the whole timeout against a count the
# deployment may never reach, and the case then measures that timeout rather than the scale.
wait_worker_count() {
  local expected="$1" budget="$2" count=0
  local deadline=$((SECONDS + budget))
  while [ "$SECONDS" -lt "$deadline" ]; do
    scrape_router_metrics "workers-${expected}" >/dev/null 2>&1 || true
    count="$(healthy_workers "workers-${expected}")"
    [ "${count:-0}" -ge "$expected" ] && break
    sleep 5
  done
  printf '%s' "$count"
}

# scale_replicas COUNT LABEL patches the Server role's replica count and verifies that it took.
#
# THE PATCH NAMES THE ONE FIELD IT CHANGES. A merge patch on spec.roles replaces the whole role
# list, so the role that comes back carries only the two fields the patch carried and the API
# server rejects the deployment for the role it no longer describes: the instance type, the
# image, the size, the resources and the extra arguments are all gone. A JSON patch changes the
# one number and leaves the rest of the role alone. The path names the first role because the
# manifest declares exactly one role, the server role.
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
#
# THE CHAT DOOR IS THE ONE THAT HONOURS THE JSON MODE. The completions door answers in prose and
# repeats the object it was asked for, so the response_format field it was given changes nothing
# and the answer never parses as an object. The chat door with the same field answers with the
# object itself, which is the form this check reads. The thinking switch is pinned, because a
# model that reasons out loud answers with its reasoning.
check_json() {
  local port="$1" out="$2"
  curl --fail-with-body --max-time 120 "http://127.0.0.1:${port}/v1/chat/completions" \
    -H 'Content-Type: application/json' \
    -d '{"model":"/models","messages":[{"role":"user","content":"Return a JSON object with the key name and the value gpu."}],"max_tokens":64,"temperature":0,"chat_template_kwargs":{"enable_thinking":false},"response_format":{"type":"json_object"}}' \
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
                    replica_ordinal: .metadata.labels["modeldeployment.gpustack.ai/pod-ordinal"],
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
  # THE CJK TEST IS READ BY THE PARSER THAT READ THE ANSWER. A host whose grep has no -P option
  # fails every match with a usage error, so a Chinese answer became a failed check.
  jq -e '.choices[0].text | test("[\u4e00-\u9fff]")' "$f" >/dev/null 2>&1 \
    && echo "PASS $(basename "$f")" >> "${OUT}/content-verdicts.txt" \
    || echo "FAIL $(basename "$f")" >> "${OUT}/content-verdicts.txt"
done
for f in "${OUT}"/member-*-json.json; do
  [ -e "$f" ] || continue
  # THE ANSWER MUST PARSE TO AN OBJECT. Parsing it as any JSON value is not enough: a bare string
  # is a valid JSON document, so "jq -e ." would call a plain English answer a pass. An empty
  # object carries no answer either, so the object must hold at least one key.
  jq -e '.choices[0].message.content | fromjson | select(type == "object") | keys | length > 0' \
    "$f" >/dev/null 2>&1 \
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
if ! scale_replicas 2 "scale-up-patch"; then
  cat "${OUT}/content-verdicts.txt" 2>/dev/null
  exit 1
fi
# THE ROLE'S OWN CONVERGENCE IS WHAT THE SCALE-UP WAITS FOR, and the gateway's worker set is then
# read as what it is. Waiting for the worker count instead spins the whole timeout against a count
# the deployment may never reach, and the wait then measures the timeout rather than the scale.
MEMBERS_PER_GROUP="$(kubectl -n "$NS" get modeldeployment "$DEPLOY" \
  -o jsonpath='{.spec.roles[0].size}' 2>/dev/null || echo 2)"
EXPECTED_WORKERS_UP=$((MEMBERS_PER_GROUP * 2))
deadline=$((SECONDS + SCALE_TIMEOUT))
while [ "$SECONDS" -lt "$deadline" ]; do
  ready="$(kubectl -n "$NS" get modeldeployment "$DEPLOY" \
    -o jsonpath='{.status.roles[0].ready}' 2>/dev/null || true)"
  [ "${ready:-0}" = "2" ] && break
  sleep 5
done
workers_after_up="$(wait_worker_count "${EXPECTED_WORKERS_UP}" "${WORKER_SETTLE_SECONDS}")"
log "The gateway's worker set after the scale-up: ${workers_after_up} of ${EXPECTED_WORKERS_UP} expected"
# THE WORKER SET IS A VERDICT ROW, not only a log line. The gateway's registry is the end-to-end
# evidence of which members the Router balances across, and a cell that cannot fail on its own
# central assertion cannot report one.
if [ "${workers_after_up:-0}" -ge "${EXPECTED_WORKERS_UP}" ]; then
  record PASS "the gateway's worker set covers every member of every group" \
    "the gateway holds ${workers_after_up} healthy worker(s) for the ${EXPECTED_WORKERS_UP} members of 2 groups"
else
  record FAIL "the gateway's worker set covers every member of every group" \
    "the gateway holds ${workers_after_up} healthy worker(s) where the 2 groups have ${EXPECTED_WORKERS_UP} members"
fi
group_status "after-scale-up" | tee "${OUT}/after-scale-up-roles.txt"

# --- scale 2 -> 1 ---------------------------------------------------------
log "Scaling the group count 2 -> 1. The drain runs the ${GRACE_SECONDS}s handoff grace window."
if ! scale_replicas 1 "scale-down-patch"; then
  cat "${OUT}/content-verdicts.txt" 2>/dev/null
  exit 1
fi
scale_down_start="$(date +%s)"
deadline=$((SECONDS + SCALE_TIMEOUT))
while [ "$SECONDS" -lt "$deadline" ]; do
  replicas="$(kubectl -n "$NS" get modeldeployment "$DEPLOY" -o jsonpath='{.spec.roles[0].replicas}' 2>/dev/null || true)"
  [ "$replicas" = "1" ] && break
  sleep 5
done
# THE WITHDRAWN GROUP'S WORKERS MUST LEAVE THE GATEWAY'S WORKER SET, and the loop waits for that
# to happen. A loop that stops as soon as the count is at or below the expected size stops
# immediately when the count is already below it, so it measures nothing and reports a grace window
# it never waited for.
#
# THE SURVIVOR COMES BACK AFTER THE DRAIN, and the row waits for that before it reads the set. The
# gateway withdraws the retired group's worker and, while it re-registers the survivor, holds the
# survivor as withdrawn too. A read taken in that window counts zero healthy workers and reports a
# survivor that was withdrawn, when the survivor was only between workers.
workers_before_down="${workers_after_up:-0}"
while [ "$SECONDS" -lt "$deadline" ]; do
  scrape_router_metrics "workers-after-scale-down" >/dev/null 2>&1 || true
  workers_after_down="$(healthy_workers "workers-after-scale-down")"
  [ "${workers_after_down:-0}" -lt "${workers_before_down:-0}" ] && break
  sleep 5
done
# THE SETTLE IS A SEPARATE WAIT, with its own budget, because it answers a different question:
# the drain is over when the count falls, and the survivor is back when the count is a worker the
# gateway is routing to again.
settle_deadline=$((SECONDS + WORKER_SETTLE_SECONDS))
while [ "$SECONDS" -lt "$settle_deadline" ]; do
  scrape_router_metrics "workers-after-scale-down" >/dev/null 2>&1 || true
  workers_after_down="$(healthy_workers "workers-after-scale-down")"
  [ "${workers_after_down:-0}" -ge 1 ] && break
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
  record PASS "the scale-down ran the handoff grace window" \
    "the withdrawn group's workers left the gateway's worker set after ${scale_down_seconds}s, at least the ${GRACE_SECONDS}s window"
else
  record FAIL "the scale-down ran the handoff grace window" \
    "the withdrawn group's workers left the gateway's worker set after ${scale_down_seconds}s, shorter than the ${GRACE_SECONDS}s window"
fi
# THE SURVIVOR IS STILL ROUTED TO, AND THE RETIRED GROUP'S WORKERS ARE GONE. These are two
# different facts and the row checks both: a count of zero says the scale-down withdrew more than
# it should have, and a count that did not fall says the retired group is still being served.
#
# THE COUNT IS NOT COMPARED WITH THE SURVIVING GROUP'S MEMBER COUNT. The gateway registers one
# worker per group, the leader, and never a subordinate; that is ISSUE-5, and the row that owns
# it is "the gateway's worker set covers every member of every group". A row that demanded a
# member count here would be asking for the behaviour that row already reports as missing, and it
# could never say anything else.
#
# THE SET THE GATEWAY CAN HOLD IS STATED IN THE ROW. Under ISSUE-5 one member per group is
# registered, so after the scale-down the holdable set is the surviving group's one worker; the
# row carries that number instead of leaving the reader to derive it from another row's failure.
if [ "${workers_after_down:-0}" -ge 1 ] && [ "${workers_after_down:-0}" -le "${workers_before_down:-0}" ]; then
  record PASS "the scale-down withdrew the whole group" \
    "under ISSUE-5 the gateway registers one member per group, so the set it can hold after the scale-down is 1 worker: the retired group's worker left the set (${workers_before_down} -> ${workers_after_down} healthy worker(s)) and the surviving group is still routed to"
else
  record FAIL "the scale-down withdrew the whole group" \
    "the gateway holds ${workers_after_down} healthy worker(s) after the scale-down, where the holdable set under ISSUE-5 is 1 worker (one registered member per group): the retired group's worker had to leave and the surviving group had to stay"
fi
group_status "after-scale-down" | tee "${OUT}/after-scale-down-roles.txt"

# --- the fault injection row ----------------------------------------------
# A non-head member going NotReady must withdraw the WHOLE group, and the survivor must be
# de-selected rather than left serving a collective whose peer is gone.
#
# THE ROLE MUST HOLD ONE SERVING GROUP BEFORE THE FAULT IS INJECTED. The row asks whether the
# group is withdrawn as a unit, and a role that still serves a second group answers that question
# with a number that belongs to the other group. The withdrawal above is waited for first.
#
# THE GROUP IS COUNTED BY ITS OWN PUBLICATION, not by the role's ready count. The ready count is a
# count of whole-ready INSTANCES, and a retired group keeps its Pods for a while after the
# scale-down, so the ready count stays at two while one group is eligible and one is being
# withdrawn. Waiting for a ready count of one therefore spends the whole budget against a number
# the role never reaches, and the fault is then injected into a state nobody checked.
deadline=$((SECONDS + SCALE_TIMEOUT))
fault_premise_held=0
while [ "$SECONDS" -lt "$deadline" ]; do
  eligible_groups="$(kubectl -n "$NS" get modeldeployment "$DEPLOY" -o json 2>/dev/null \
    | jq '[.status.roles[].groups[]? | select(.state == "Eligible")] | length' 2>/dev/null || echo 0)"
  [ "${eligible_groups:-0}" = "1" ] && { fault_premise_held=1; break; }
  sleep 5
done
# THE PREMISE WAIT FAILS LOUDLY. The wait reads the group's own publication, and a publication
# that never shows exactly one eligible group means the fault would be injected into a state
# nobody checked: the rows below would measure whatever that state is and call it the withdrawal
# contract. The case says so in its results and skips the injection, instead of spending the
# fault on an unverifiable premise.
if [ "${fault_premise_held}" != "1" ]; then
  record BLOCKED "the role held one serving group before the fault" \
    "the group publication never showed exactly one eligible group within ${SCALE_TIMEOUT}s, so the fault was not injected and no withdrawal was measured"
  record BLOCKED "the group was withdrawn as a unit when a member was stopped" \
    "the fault premise never held, so there is no fault to measure the withdrawal against"
  record BLOCKED "the survivor was de-selected after the fault" \
    "the fault premise never held, so there is no fault to measure the de-selection against"
fi
if [ "${fault_premise_held}" = "1" ]; then
log "Injecting a non-head member fault"
FAULT_POD="$(subordinate_member_pod)"
if [ -n "$FAULT_POD" ]; then
  kubectl -n "$NS" label pod "$FAULT_POD" "e2e.gpustack.ai/fault-injected=true" --overwrite \
    > "${OUT}/fault-label.txt" 2>&1
  # The label alone does not change readiness; the readiness gate is what the probe reads. The
  # case records what it did and what the group did, so the evidence names its own method.
  kubectl -n "$NS" exec "$FAULT_POD" -c main -- sh -c 'kill -STOP 1' \
    > "${OUT}/fault-inject.txt" 2>&1 || true
  # THE WITHDRAWAL IS WAITED FOR, not read at a fixed moment after the fault. The operator finds
  # the stopped member on its next observation and its group-forward probe is bounded at 15s, so
  # the group leaves the eligible set after the fault plus the observation cycle plus the probe
  # bound. A read at a fixed moment sits on that boundary and reports a group that has not been
  # seen yet.
  fault_deadline=$((SECONDS + FAULT_SETTLE_SECONDS))
  while [ "$SECONDS" -lt "$fault_deadline" ]; do
    group_status "after-fault" >/dev/null 2>&1 || true
    FAULT_ELIGIBLE="$(jq -r '[.endpoints.eligible // 0] | add // 0' \
      "${OUT}/after-fault-roles.json" 2>/dev/null || echo 0)"
    [ "${FAULT_ELIGIBLE:-0}" -eq 0 ] && break
    sleep 5
  done
  group_status "after-fault" | tee "${OUT}/after-fault-roles.txt"
  # THE GATEWAY'S WORKER SET IS THE GATEWAY'S OWN DISCOVERY, and the gateway polls on its own
  # cadence, so the case waits for the set to drain rather than reading it the moment the operator
  # has withdrawn the group.
  while [ "$SECONDS" -lt "$fault_deadline" ]; do
    scrape_router_metrics "workers-after-fault" >/dev/null 2>&1 || true
    FAULT_HEALTHY="$(healthy_workers "workers-after-fault")"
    [ "${FAULT_HEALTHY:-0}" -eq 0 ] && break
    sleep 5
  done
  scrape_router_metrics "workers-after-fault" || true
  grep '^smg_worker_health{' "${OUT}/workers-after-fault.txt" > "${OUT}/worker-health-after-fault.txt" 2>/dev/null || true
  # THE PREMISE IS READ BACK BEFORE THE ROWS ARE SCORED. The row measures what happens when a
  # member goes NotReady, so the member's own Ready condition is the fact that says whether that
  # happened. A stopped process whose Pod never lost its Ready condition gave the operator no
  # member-loss signal at all, and the rows below would report the absence of the event as a
  # failure of the contract.
  FAULT_READY="$(kubectl -n "$NS" get pod "$FAULT_POD" \
    -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "")"
  echo "fault_pod=${FAULT_POD}" > "${OUT}/fault-premise.txt"
  echo "ready_condition=${FAULT_READY:-unknown}" >> "${OUT}/fault-premise.txt"
  kubectl -n "$NS" exec "$FAULT_POD" -c main -- sh -c 'kill -CONT 1' \
    > "${OUT}/fault-restore.txt" 2>&1 || true

  # THE FAULT ROW GETS A VERDICT, not only files. The two failures this row exists to catch are
  # a group that kept serving after a member was stopped and a router that kept the survivor
  # routed, so the evidence has to say which of them happened.
  #
  # THE PREMISE COMES FIRST. A row that cannot establish the event it measures says so, rather
  # than reporting a failure of a contract the event never reached.
  if [ "${FAULT_READY}" = "True" ]; then
    record BLOCKED "the group was withdrawn as a unit when a member was stopped" \
      "the faulted member ${FAULT_POD} still reports Ready=True, so no member went NotReady and the group was never asked to withdraw"
    record BLOCKED "the survivor was de-selected after the fault" \
      "the faulted member ${FAULT_POD} still reports Ready=True, so no member went NotReady and the survivor was never asked to be de-selected"
    FAULT_ELIGIBLE="$(jq -r '[.endpoints.eligible // 0] | add // 0' "${OUT}/after-fault-roles.json" 2>/dev/null || echo 0)"
    FAULT_SERVING="$(jq -r '[.endpoints.serving.value // 0] | add // 0' "${OUT}/after-fault-roles.json" 2>/dev/null || echo 0)"
    FAULT_STATE="$(jq -r '[.endpoints.serving.state // "unknown"] | join(",")' "${OUT}/after-fault-roles.json" 2>/dev/null || echo unknown)"
  else
    # THE GROUP IS WITHDRAWN AS A UNIT. A collective whose peer is gone cannot serve, so the
    # role must publish no serving endpoint and no eligible endpoint once the member is stopped.
    FAULT_ELIGIBLE="$(jq -r '[.endpoints.eligible // 0] | add // 0' "${OUT}/after-fault-roles.json" 2>/dev/null || echo 0)"
    FAULT_SERVING="$(jq -r '[.endpoints.serving.value // 0] | add // 0' "${OUT}/after-fault-roles.json" 2>/dev/null || echo 0)"
    FAULT_STATE="$(jq -r '[.endpoints.serving.state // "unknown"] | join(",")' "${OUT}/after-fault-roles.json" 2>/dev/null || echo unknown)"
    if [ "${FAULT_SERVING:-0}" -eq 0 ] && [ "${FAULT_ELIGIBLE:-0}" -eq 0 ]; then
      record PASS "the group was withdrawn as a unit when a member was stopped" \
        "the role publishes ${FAULT_SERVING} serving endpoint(s) and ${FAULT_ELIGIBLE} eligible endpoint(s) after the fault (${FAULT_STATE})"
    else
      record FAIL "the group was withdrawn as a unit when a member was stopped" \
        "the role still publishes ${FAULT_SERVING} serving endpoint(s) and ${FAULT_ELIGIBLE} eligible endpoint(s) after the fault (${FAULT_STATE})"
    fi

    # THE SURVIVOR IS DE-SELECTED. The gateway keeps the series of a worker it withdrew and marks
    # it with the value -1, so the count of healthy workers is the count of workers it still
    # routes to.
    FAULT_HEALTHY="$(healthy_workers "workers-after-fault")"
    if [ "${FAULT_HEALTHY:-0}" -eq 0 ]; then
      record PASS "the survivor was de-selected after the fault" \
        "the gateway holds ${FAULT_HEALTHY} healthy worker(s) after the fault"
    else
      record FAIL "the survivor was de-selected after the fault" \
        "the gateway still holds ${FAULT_HEALTHY} healthy worker(s) after the fault"
    fi
  fi
else
  echo "no non-head member found" > "${OUT}/fault-inject.txt"
  record BLOCKED "the fault injection row ran" "no non-head member of the group was found, so the fault could not be injected"
fi
# THE PREMISE GUARD ENDS HERE: the injection above ran only because the publication held one
# eligible group, and a premise that never held skips the fault entirely.
fi

# --- the counter, collected once -----------------------------------------
bash "${SKILL_DIR}/scripts/stage_counts.sh" "$DEPLOY" "$OUT" > "${OUT}/stage-counts.txt" 2>&1
kubectl get devices -o json > "${OUT}/released-devices.json" 2>/dev/null || true

cat "${OUT}/verdicts.txt" 2>/dev/null
cat "${OUT}/content-verdicts.txt" 2>/dev/null
FAILS="$(count_verdict FAIL)"
BLOCKED_ROWS="$(count_verdict BLOCKED)"
WARNS="$(count_verdict WARN)"
PASSES="$(count_verdict PASS)"
log "CASE 6 (SGLang External DP) complete: ${PASSES:-0} passed row(s), ${FAILS:-0} failed row(s), ${BLOCKED_ROWS:-0} blocked row(s), ${WARNS:-0} warning(s)"

# THE SCRIPT ENDS HERE, and the last statement is the exit code. An explicit trailing "exit 0"
# makes the analyzer read the EXIT trap's handler as a function nothing calls.
[ "${FAILS:-0}" -eq 0 ] && [ "${BLOCKED_ROWS:-0}" -eq 0 ] || exit 1
