#!/usr/bin/env bash
#
# CASE 4b — External DP: the manual replicas 1 -> 2 -> 1 scaling leg   (MUTATING, self-recovering)
#
#   case-4b-external-dp-scaling.sh [OUT_DIR]
#
# Goal:        Add the leg CASE 4 does not have. CASE 4 compares two topologies and never scales
#              an External DP deployment, and a scale of an External DP group is a different
#              contract from a scale of an ordinary multi-Server role: the group's members share a
#              collective, its withdrawal is group-atomic, and its drain runs the disaggregated
#              handoff grace window. This case applies the SAME shape CASE 4's second arm used
#              and drives the replica count 1 -> 2 -> 1, checking at each step that the Router's
#              worker set grows and shrinks with the groups, that the endpoint gate counts the
#              groups, and that the scale-down takes at least the grace window.
# Environment: A cluster with free GPUs on InstanceType bench-gpu, NodeModelStore caching
#              deepseek-v2-lite-chat, and an in-cluster default/bench-driver Pod.
#              GPU BUDGET: the recorded shape is size=4 members at 2 GPUs each, so one replica is
#              8 GPUs and TWO replicas are 16. A cluster that cannot carry 16 free GPUs must run
#              this case on a smaller size, which is a different shape and not this case's claim.
#              The shape this run was on is written to OUT_DIR/cluster-shape.txt: the context and
#              the cluster, the namespace, the nodes, the InstanceType the case asked for, the
#              accelerators on each node, the InstanceTypes the cluster carries, and the operator
#              image with its digest.
#              The cluster of record for this run: gpustack-sedp-0l578ap2 (ns gpustack-bench),
#              platform gpu-h100-sxm 8xH100 non-preemptible with a cpu-e2 system node, 8
#              accelerators on the one GPU node, InstanceType
#              gpustack--nvidia-h100-80gb-hbm3-linux-amd64, operator
#              <dev>/gpustack-operator:<tag> (a development image; tests pull gpustack/gpustack-operator unless a dev image is named).
# Inputs:      manifests/external-dp-arm2-external-dp.yaml, copied into OUT_DIR and rewritten
#              there so the RECORDED manifest is not modified in place. The copy carries the
#              pinned runner image (DECISIONS.md D6) and this case's own deployment name; the
#              shape itself — size=4, TP=2, DP=4, EP, vllm-router round_robin, VLLM_PORT=19500 —
#              is the recorded one, unchanged.
# Expected:    - at one replica the group reports four qualified HTTP endpoints behind the Router;
#              - at two replicas the Router's worker set grows by exactly the new group's four
#                members, and the endpoint count doubles;
#              - scaling back to one withdraws the whole second group atomically, and the scale-down
#                takes at least 60s because the drain runs the disaggregated handoff grace window
#                before the member gauge reads;
#              - the traffic sent at each step is served with zero errors.
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
OUT="${1:-${REPO}/.claude/reports/$(date +%F)-benchmark/external-dp-scaling}"
DEPLOY="external-dp-scaling"
MODEL_ARTIFACT="${EDP_ARTIFACT:-deepseek-v2-lite-chat}"
IMAGE="${EDP_IMAGE:-gpustack/runner:cuda13.0-vllm0.32.0}"
ROUTER_IMAGE="${EDP_ROUTER_IMAGE:-gpustack/llm-router:v0.3.1}"
ROUTER_NAME="${EDP_ROUTER_NAME:-vllm-router}"
INSTANCE_TYPE="${EDP_INSTANCE_TYPE:-bench-gpu}"
MEMBERS="${EDP_MEMBERS:-4}"
# The shape this run uses. "recorded" is the shape the manifest was recorded with: size=4
# members at 2 GPUs each, so two replicas are 16 GPUs. "width2" is the plan's shape for a
# cluster that cannot carry that: size=2 members at 1 GPU each, so two replicas are 4 GPUs.
SHAPE="${EDP_SHAPE:-recorded}"
WIDTH2_MEMBERS="${EDP_MEMBERS:-2}"
WIDTH2_ACCELERATOR="${EDP_ACCELERATOR:-1}"
WIDTH2_TP="${EDP_TP:-1}"
WIDTH2_DP="${EDP_DP:-2}"
if [ "$SHAPE" = "width2" ]; then
  MEMBERS="${EDP_MEMBERS:-2}"
fi
LOCAL_PORT="${LOCAL_PORT:-18400}"
READY_TIMEOUT="${READY_TIMEOUT:-2400}"
SCALE_TIMEOUT="${SCALE_TIMEOUT:-1800}"
# The disaggregated handoff grace window the drain runs before the member gauge reads.
GRACE_SECONDS="${EDP_GRACE_SECONDS:-60}"
PF_PID=""

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
cleanup() {
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
  kubectl -n "$NS" delete modeldeployment "$DEPLOY" --ignore-not-found --wait=true --timeout=180s \
    >/dev/null 2>&1 || true
  return 0
}
trap cleanup EXIT

# The recorded manifest, copied and rewritten. The copy is what this case applies, so the
# recorded file keeps the shape and the image it was recorded with.
#
# THE SHAPE IS A PARAMETER, BECAUSE THE RECORDED SHAPE DOES NOT FIT EVERYWHERE. The recorded
# shape is size=4 members at 2 GPUs each, so one replica is 8 GPUs and TWO replicas — which is
# what this case drives — are 16. A cluster that cannot carry 16 free GPUs runs the plan's
# width-2 shape instead: size=2 members at 1 GPU each, so one replica is 2 GPUs and two are 4.
# That is a different shape and not this case's recorded claim, so the shape that ran is written
# into the evidence and the run says which one it was.
write_manifest() {
  local src="${SKILL_DIR}/manifests/external-dp-arm2-external-dp.yaml"
  if [ ! -f "$src" ]; then
    log "MISSING: the recorded manifest ${src} is not present"
    return 1
  fi
  sed -e "s|^  name: external-dp-arm2-external-dp$|  name: ${DEPLOY}|" \
      -e "s|^  namespace: gpustack-bench$|  namespace: ${NS}|" \
      -e "s|gpustack/runner:cuda13.0-vllm0.30.0|${IMAGE}|g" \
      -e "s|gpustack/llm-router:v0.3.1|${ROUTER_IMAGE}|g" \
      -e "s|^      instanceType: bench-gpu$|      instanceType: ${INSTANCE_TYPE}|" \
      -e "s|^      name: deepseek-v2-lite-chat$|      name: ${MODEL_ARTIFACT}|" \
      "$src" > "${OUT}/external-dp-scaling.yaml"
  if [ "$SHAPE" = "width2" ]; then
    sed -e "s|^      size: 4$|      size: ${WIDTH2_MEMBERS}|" \
        -e "s|^        accelerator: \"2\"$|        accelerator: \"${WIDTH2_ACCELERATOR}\"|" \
        -e "s|^        - \"2\"$|        - \"${WIDTH2_TP}\"|" \
        -e "s|^        - \"4\"$|        - \"${WIDTH2_DP}\"|" \
        "${OUT}/external-dp-scaling.yaml" > "${OUT}/external-dp-scaling.yaml.tmp" \
      && mv "${OUT}/external-dp-scaling.yaml.tmp" "${OUT}/external-dp-scaling.yaml"
  fi
  {
    echo "shape=${SHAPE}"
    echo "members_per_group=${MEMBERS}"
    echo "accelerator_per_member=${WIDTH2_ACCELERATOR}"
    echo "tensor_parallel_size=${WIDTH2_TP}"
    echo "data_parallel_size=${WIDTH2_DP}"
    echo "gpus_per_replica=$((MEMBERS * WIDTH2_ACCELERATOR))"
    echo "gpus_at_two_replicas=$((MEMBERS * 2 * WIDTH2_ACCELERATOR))"
  } > "${OUT}/shape.txt"
  log "The shape that will run: $(tr '\n' ' ' < "${OUT}/shape.txt")"
}

# endpoint_gate prints the group's endpoint counts: eligible counts the replica GROUPS, serving
# counts the HTTP endpoints. At one replica the recorded shape reads eligible=1 and serving=4.
endpoint_gate() {
  kubectl -n "$NS" get modeldeployment "$DEPLOY" -o json 2>/dev/null \
    | jq -r '.status.roles[] | "eligible=\(.endpoints.eligible // "?") serving=\(.endpoints.serving.value // "?")"'
}

# router_workers prints the number of workers the Router's own metrics name.
#
# THE WORKER SET IS READ FROM THE GAUGE, NOT FROM THE REQUEST COUNTER. The Router exposes
# vllm_router_active_workers from the moment it starts, while the per-worker series this case
# used to count (vllm_router_processed_requests_total{worker=...}) only appears after a request
# has been served by that worker. Counting those meant the first check, which runs BEFORE any
# traffic, saw zero workers and spun out the whole timeout against a Router that was already
# holding the whole group. The gauge is the series that answers "how many workers does the
# Router hold", and the per-worker URLs are collected separately as the distribution evidence
# after each load.
router_workers() {
  local name="$1" pod port
  pod="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${DEPLOY},modeldeployment.gpustack.ai/router=${ROUTER_NAME}" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
  [ -n "$pod" ] || { echo "0"; return 1; }
  port="$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.spec.containers[0].args}' 2>/dev/null \
    | sed -n 's/.*--prometheus-port=\([0-9][0-9]*\).*/\1/p' | head -1)"
  [ -n "$port" ] || { echo "0"; return 1; }
  kubectl -n "$NS" port-forward "pod/${pod}" "$((LOCAL_PORT + 90)):${port}" \
    > "${OUT}/port-forward-metrics-${name}.log" 2>&1 &
  PF_PID=$!
  local _
  for _ in $(seq 1 30); do
    if curl -sf -o "${OUT}/router-metrics-${name}.txt" "http://127.0.0.1:$((LOCAL_PORT + 90))/metrics"; then
      break
    fi
    sleep 2
  done
  kill "$PF_PID" 2>/dev/null || true
  PF_PID=""
  awk '/^vllm_router_active_workers / { print $2; found = 1 } END { if (!found) exit 1 }' \
    "${OUT}/router-metrics-${name}.txt" 2>/dev/null || echo 0
}

# router_worker_urls prints the distinct worker URLs the Router's per-worker series name. The
# series exist only for workers that have served a request, so this is the evidence of the
# distribution AFTER a load, not a way to count the worker set.
router_worker_urls() {
  local name="$1"
  grep -oE '^vllm_router_processed_requests_total\{[^}]*worker="[^"]*"' "${OUT}/router-metrics-${name}.txt" 2>/dev/null \
    | sed -n 's/.*worker="\([^"]*\)".*/\1/p' | sort -u | tr '\n' ' '
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
    record FAIL "the ${label} took" "the patch was rejected: $(cat "${OUT}/${label}.txt" 2>/dev/null)"
    return 1
  fi
  replicas="$(kubectl -n "$NS" get modeldeployment "$DEPLOY" -o jsonpath='{.spec.roles[0].replicas}' 2>/dev/null || true)"
  if [ "${replicas:-0}" != "${count}" ]; then
    record FAIL "the ${label} took" \
      "the patch did not take effect: roles[0].replicas is ${replicas:-unknown}, not ${count}"
    return 1
  fi
  record PASS "the ${label} took" "roles[0].replicas is ${count}"
  return 0
}

# wait_workers waits until the Router names at least the expected number of workers.
wait_workers() {
  local expected="$1" deadline=$((SECONDS + SCALE_TIMEOUT)) count
  while [ "$SECONDS" -lt "$deadline" ]; do
    count="$(router_workers "wait-${expected}")"
    if [ "${count:-0}" -ge "$expected" ]; then
      printf '%s' "$count"
      return 0
    fi
    sleep 5
  done
  printf '%s' "${count:-0}"
  return 1
}

wait_ready() {
  local deadline=$((SECONDS + READY_TIMEOUT)) phase
  while [ "$SECONDS" -lt "$deadline" ]; do
    phase="$(kubectl -n "$NS" get modeldeployment "$DEPLOY" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    [ "$phase" = "Ready" ] && return 0
    sleep 5
  done
  return 1
}

# send_load drives one cell of traffic through the Router and records the result.
send_load() {
  local label="$1" endpoint="$2" offset="$3"
  python3 "${SKILL_DIR}/scripts/bench_client.py" run \
    --endpoint "$endpoint" \
    --model "/models" \
    --label "$label" \
    --arm "external-dp-scaling" \
    --suite "scaling" \
    --shape "1024/256" \
    --input-tokens 1024 \
    --output-tokens 256 \
    --concurrency 8 \
    --requests 32 \
    --token-offset "$offset" \
    --out "${OUT}/${label}.json"
}

# record_load LABEL records the outcome of one load cell. The traffic being served without
# errors is part of this cell's contract, so the error count belongs in the evidence as a
# verdict row and not only in the raw JSON.
record_load() {
  local label="$1" requests errors
  requests="$(jq -r '.requests // "?"' "${OUT}/${label}.json" 2>/dev/null || echo '?')"
  errors="$(jq -r '.errors // "?"' "${OUT}/${label}.json" 2>/dev/null || echo '?')"
  if [ "${errors}" = "0" ]; then
    record PASS "the ${label} traffic was served without errors" "${requests} request(s), 0 error(s)"
  else
    record FAIL "the ${label} traffic was served without errors" "${requests} request(s), ${errors} error(s)"
  fi
}

log "Writing the manifest into ${OUT}/external-dp-scaling.yaml"
if ! write_manifest; then
  exit 1
fi
kubectl apply --dry-run=server -f "${OUT}/external-dp-scaling.yaml" > "${OUT}/apply-dry-run.txt" 2>&1

log "Applying the deployment"
kubectl -n "$NS" delete modeldeployment "$DEPLOY" --ignore-not-found --wait=true --timeout=180s >/dev/null 2>&1 || true
kubectl apply -f "${OUT}/external-dp-scaling.yaml" 2>&1 | tee "${OUT}/apply.log"

if ! wait_ready; then
  log "TIMEOUT: the deployment did not reach Ready"
  kubectl -n "$NS" describe modeldeployment "$DEPLOY" > "${OUT}/describe.txt" 2>&1
  kubectl -n "$NS" get pods -o wide > "${OUT}/pods.txt" 2>&1
  exit 1
fi
log "The deployment is Ready"

kubectl -n "$NS" get modeldeployment "$DEPLOY" -o yaml > "${OUT}/modeldeployment.yaml"
kubectl -n "$NS" get pods -o wide > "${OUT}/pods.txt"
kubectl -n "$NS" get svc -o wide > "${OUT}/services.txt"
endpoint="$(kubectl -n "$NS" get modeldeployment "$DEPLOY" -o jsonpath='{.status.endpoint}')"
echo "$endpoint" > "${OUT}/endpoint.txt"
svc="$(echo "$endpoint" | sed -E 's|^[a-z]+://([^.:/]+).*|\1|')"
kubectl -n "$NS" port-forward "svc/${svc}" "${LOCAL_PORT}:8000" > "${OUT}/port-forward.log" 2>&1 &
PF_PID=$!
for _ in $(seq 1 90); do
  curl -sf -o /dev/null "http://127.0.0.1:${LOCAL_PORT}/v1/models" && break
  sleep 2
done

offset="$(python3 "${SKILL_DIR}/scripts/bench_client.py" calibrate --endpoint "http://127.0.0.1:${LOCAL_PORT}" \
  --model "/models" --input-tokens 1024 2>/dev/null \
  | python3 -c 'import json,sys; print(json.load(sys.stdin).get("token_offset") or 0)')"
echo "$offset" > "${OUT}/token-offset.txt"

# --- one replica -----------------------------------------------------------
endpoint_gate | tee "${OUT}/gate-one-replica.txt"
workers_one="$(wait_workers "$MEMBERS" || true)"
log "Router workers at one replica: ${workers_one}"
if [ "${workers_one:-0}" -ge "$MEMBERS" ]; then
  record PASS "the Router holds one whole group" \
    "the Router holds ${workers_one} worker(s) at one replica, and the group is ${MEMBERS} member(s)"
else
  record FAIL "the Router holds one whole group" \
    "the Router holds ${workers_one:-0} worker(s) at one replica, and the group is ${MEMBERS} member(s)"
fi
send_load "load-one-replica" "http://127.0.0.1:${LOCAL_PORT}" "$offset"
record_load "load-one-replica"
router_workers "load-one-replica" >/dev/null 2>&1 || true
router_worker_urls "load-one-replica" > "${OUT}/worker-urls-one-replica.txt" 2>/dev/null || true
bash "${SKILL_DIR}/scripts/stage_counts.sh" "$DEPLOY" "$OUT" > "${OUT}/stage-counts-one-replica.txt" 2>&1

# --- two replicas ----------------------------------------------------------
log "Scaling the replica count 1 -> 2"
if ! scale_replicas 2 "scale-up-patch"; then
  cat "${OUT}/verdicts.txt" 2>/dev/null
  exit 1
fi
workers_two="$(wait_workers $((MEMBERS * 2)) || true)"
log "Router workers at two replicas: ${workers_two}"
if [ "${workers_two:-0}" -ge $((MEMBERS * 2)) ]; then
  record PASS "the Router's worker set grew with the second group" \
    "the Router holds ${workers_two} worker(s) at two replicas, and two groups are $((MEMBERS * 2)) member(s)"
else
  record FAIL "the Router's worker set grew with the second group" \
    "the Router holds ${workers_two:-0} worker(s) at two replicas, and two groups are $((MEMBERS * 2)) member(s)"
fi
endpoint_gate | tee "${OUT}/gate-two-replicas.txt"
kubectl -n "$NS" get modeldeployment "$DEPLOY" -o json \
  | jq '.status.roles[] | {name, ready, parallelism, endpoints}' > "${OUT}/roles-two-replicas.json" 2>/dev/null
send_load "load-two-replicas" "http://127.0.0.1:${LOCAL_PORT}" "$offset"
record_load "load-two-replicas"
router_workers "load-two-replicas" >/dev/null 2>&1 || true
router_worker_urls "load-two-replicas" > "${OUT}/worker-urls-two-replicas.txt" 2>/dev/null || true
bash "${SKILL_DIR}/scripts/stage_counts.sh" "$DEPLOY" "$OUT" > "${OUT}/stage-counts-two-replicas.txt" 2>&1

# --- back to one -----------------------------------------------------------
log "Scaling the replica count 2 -> 1. The drain runs the ${GRACE_SECONDS}s handoff grace window."
# THE RETIRED MEMBERS ARE NAMED BEFORE THE PATCH, out of the deployment's own group publication:
# the drain is measured on the members the scale-down retires, and a name read after the patch
# could already be a name the API server has stopped answering for.
PRE_PATCH_MEMBERS="$(kubectl -n "$NS" get modeldeployment "$DEPLOY" -o json 2>/dev/null \
  | jq -r '[.status.roles[].groups[]?.members[]?.name] | unique | .[]' 2>/dev/null)"
scale_down_start="$(date +%s)"
if ! scale_replicas 1 "scale-down-patch"; then
  cat "${OUT}/verdicts.txt" 2>/dev/null
  exit 1
fi
deadline=$((SECONDS + SCALE_TIMEOUT))
while [ "$SECONDS" -lt "$deadline" ]; do
  workers_one_again="$(wait_workers "$MEMBERS" || true)"
  [ "${workers_one_again:-0}" -le "$MEMBERS" ] && break
  sleep 5
done
withdrawal_end="$(date +%s)"
withdrawal_seconds=$((withdrawal_end - scale_down_start))
echo "endpoint_withdrawal_seconds=${withdrawal_seconds}" >> "${OUT}/scale-down-timing.txt"
# THE ENDPOINT WITHDRAWAL IS ITS OWN ROW. The Router's worker set falls as soon as the retired
# members stop being routed, and that moment is not the drain: D11 measured this leg at 21s and
# ruled the row that timed it against the grace window a harness mis-measurement, because the
# window governs the DRAIN that follows the withdrawal.
if [ "${workers_one_again:-0}" -le "$MEMBERS" ]; then
  record PASS "the second group's endpoints were withdrawn" \
    "the Router holds ${workers_one_again} worker(s) after ${withdrawal_seconds}s, and one group is ${MEMBERS} member(s)"
else
  record FAIL "the second group's endpoints were withdrawn" \
    "the Router still holds ${workers_one_again:-0} worker(s) at the end of the budget, where one group is ${MEMBERS} member(s)"
fi

# THE DRAIN IS MEASURED TO ITS COMPLETION. The retired members keep their Pods until their
# in-flight work is handed off, and the grace window is the floor the operator gives that handoff:
# the Pods must not be deleted before the window has run. The row waits for every member name
# beyond one group's worth to stop existing, and compares the deletion moment with the window.
drain_deadline=$((SECONDS + SCALE_TIMEOUT))
drain_end=""
while [ "$SECONDS" -lt "$drain_deadline" ]; do
  existing=0
  for name in ${PRE_PATCH_MEMBERS}; do
    kubectl -n "$NS" get pod "$name" -o name >/dev/null 2>&1 && existing=$((existing + 1))
  done
  [ "${existing}" -le "$MEMBERS" ] && { drain_end="$(date +%s)"; break; }
  sleep 5
done
if [ -z "${PRE_PATCH_MEMBERS}" ]; then
  record BLOCKED "the drain ran the handoff grace window before the retired members were deleted" \
    "the deployment's group publication named no member pods, so the retired members could not be watched to their deletion"
elif [ -n "${drain_end}" ]; then
  drain_seconds=$((drain_end - scale_down_start))
  echo "drain_seconds=${drain_seconds}" >> "${OUT}/scale-down-timing.txt"
  if [ "${drain_seconds}" -ge "${GRACE_SECONDS}" ]; then
    record PASS "the drain ran the handoff grace window before the retired members were deleted" \
      "the retired members' pods were deleted ${drain_seconds}s after the patch, at or past the ${GRACE_SECONDS}s window, so the drain was not cut short"
  else
    record FAIL "the drain ran the handoff grace window before the retired members were deleted" \
      "the retired members' pods were deleted after only ${drain_seconds}s, shorter than the ${GRACE_SECONDS}s window, so the drain was cut short"
  fi
else
  record FAIL "the drain ran the handoff grace window before the retired members were deleted" \
    "the retired members' pods were still present at the end of the ${SCALE_TIMEOUT}s budget"
fi
endpoint_gate | tee "${OUT}/gate-after-scale-down.txt"
kubectl -n "$NS" get modeldeployment "$DEPLOY" -o json \
  | jq '.status.roles[] | {name, ready, parallelism, endpoints}' > "${OUT}/roles-after-scale-down.json" 2>/dev/null
send_load "load-after-scale-down" "http://127.0.0.1:${LOCAL_PORT}" "$offset"
record_load "load-after-scale-down"
router_workers "load-after-scale-down" >/dev/null 2>&1 || true
router_worker_urls "load-after-scale-down" > "${OUT}/worker-urls-after-scale-down.txt" 2>/dev/null || true

kill "$PF_PID" 2>/dev/null || true
PF_PID=""
bash "${SKILL_DIR}/scripts/stage_counts.sh" "$DEPLOY" "$OUT" > "${OUT}/stage-counts-final.txt" 2>&1
kubectl -n "$NS" get modeldeployment "$DEPLOY" -o json \
  | jq '.status.roles[] | {name, kind, parallelism, endpoints}' > "${OUT}/roles.json" 2>/dev/null
kubectl get devices -o json > "${OUT}/released-devices.json"

cat "${OUT}/verdicts.txt" 2>/dev/null
FAILS="$(grep -c '^FAIL' "${OUT}/verdicts.txt" 2>/dev/null || true)"
WARNS="$(grep -c '^WARN' "${OUT}/verdicts.txt" 2>/dev/null || true)"
log "CASE 4b (External DP scaling) complete: ${FAILS:-0} failed row(s), ${WARNS:-0} warning(s)"

# THE SCRIPT ENDS HERE, and the last statement is the exit code. An explicit trailing "exit 0"
# makes the analyzer read the EXIT trap's handler as a function nothing calls.
[ "${FAILS:-0}" -eq 0 ] || exit 1
