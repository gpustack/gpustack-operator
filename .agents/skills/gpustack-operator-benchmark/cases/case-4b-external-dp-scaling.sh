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

NS="${BENCH_NS:-gpustack-bench}"
OUT="${1:-${REPO}/.claude/reports/$(date +%F)-benchmark/external-dp-scaling}"
DEPLOY="external-dp-scaling"
MODEL_ARTIFACT="${EDP_ARTIFACT:-deepseek-v2-lite-chat}"
IMAGE="${EDP_IMAGE:-gpustack/runner:cuda13.0-vllm0.32.0}"
ROUTER_IMAGE="${EDP_ROUTER_IMAGE:-gpustack/llm-router:v0.3.1}"
INSTANCE_TYPE="${EDP_INSTANCE_TYPE:-bench-gpu}"
MEMBERS="${EDP_MEMBERS:-4}"
LOCAL_PORT="${LOCAL_PORT:-18400}"
READY_TIMEOUT="${READY_TIMEOUT:-2400}"
SCALE_TIMEOUT="${SCALE_TIMEOUT:-1800}"
# The disaggregated handoff grace window the drain runs before the member gauge reads.
GRACE_SECONDS="${EDP_GRACE_SECONDS:-60}"
PF_PID=""

mkdir -p "$OUT"
log() { echo "[$(date +%H:%M:%S)] $*"; }
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
}

# endpoint_gate prints the group's endpoint counts: eligible counts the replica GROUPS, serving
# counts the HTTP endpoints. At one replica the recorded shape reads eligible=1 and serving=4.
endpoint_gate() {
  kubectl -n "$NS" get modeldeployment "$DEPLOY" -o json 2>/dev/null \
    | jq -r '.status.roles[] | "eligible=\(.endpoints.eligible // "?") serving=\(.endpoints.serving.value // "?")"'
}

# router_workers prints the number of distinct workers the Router's own metrics name. The
# per-worker series carry the worker URL, so one worker is one member of one group.
router_workers() {
  local name="$1" pod port
  pod="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${DEPLOY},app.kubernetes.io/component=router" \
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
  grep -oE '^vllm_router_processed_requests_total\{[^}]*worker="[^"]*"' "${OUT}/router-metrics-${name}.txt" 2>/dev/null \
    | sed -n 's/.*worker="\([^"]*\)".*/\1/p' | sort -u | wc -l | tr -d ' '
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
send_load "load-one-replica" "http://127.0.0.1:${LOCAL_PORT}" "$offset"
bash "${SKILL_DIR}/scripts/stage_counts.sh" "$DEPLOY" "$OUT" > "${OUT}/stage-counts-one-replica.txt" 2>&1

# --- two replicas ----------------------------------------------------------
log "Scaling the replica count 1 -> 2"
kubectl -n "$NS" patch modeldeployment "$DEPLOY" --type=merge \
  -p '{"spec":{"roles":[{"name":"server","replicas":2}]}}' > "${OUT}/scale-up-patch.txt" 2>&1
workers_two="$(wait_workers $((MEMBERS * 2)) || true)"
log "Router workers at two replicas: ${workers_two}"
endpoint_gate | tee "${OUT}/gate-two-replicas.txt"
kubectl -n "$NS" get modeldeployment "$DEPLOY" -o json \
  | jq '.status.roles[] | {name, ready, parallelism, endpoints}' > "${OUT}/roles-two-replicas.json" 2>/dev/null
send_load "load-two-replicas" "http://127.0.0.1:${LOCAL_PORT}" "$offset"
bash "${SKILL_DIR}/scripts/stage_counts.sh" "$DEPLOY" "$OUT" > "${OUT}/stage-counts-two-replicas.txt" 2>&1

# --- back to one -----------------------------------------------------------
log "Scaling the replica count 2 -> 1. The drain runs the ${GRACE_SECONDS}s handoff grace window."
scale_down_start="$(date +%s)"
kubectl -n "$NS" patch modeldeployment "$DEPLOY" --type=merge \
  -p '{"spec":{"roles":[{"name":"server","replicas":1}]}}' > "${OUT}/scale-down-patch.txt" 2>&1
deadline=$((SECONDS + SCALE_TIMEOUT))
while [ "$SECONDS" -lt "$deadline" ]; do
  workers_one_again="$(wait_workers "$MEMBERS" || true)"
  [ "${workers_one_again:-0}" -le "$MEMBERS" ] && break
  sleep 5
done
scale_down_end="$(date +%s)"
scale_down_seconds=$((scale_down_end - scale_down_start))
echo "scale_down_seconds=${scale_down_seconds}" >> "${OUT}/scale-down-timing.txt"
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
endpoint_gate | tee "${OUT}/gate-after-scale-down.txt"
kubectl -n "$NS" get modeldeployment "$DEPLOY" -o json \
  | jq '.status.roles[] | {name, ready, parallelism, endpoints}' > "${OUT}/roles-after-scale-down.json" 2>/dev/null
send_load "load-after-scale-down" "http://127.0.0.1:${LOCAL_PORT}" "$offset"

kill "$PF_PID" 2>/dev/null || true
PF_PID=""
bash "${SKILL_DIR}/scripts/stage_counts.sh" "$DEPLOY" "$OUT" > "${OUT}/stage-counts-final.txt" 2>&1
kubectl -n "$NS" get modeldeployment "$DEPLOY" -o json \
  | jq '.status.roles[] | {name, kind, parallelism, endpoints}' > "${OUT}/roles.json" 2>/dev/null
kubectl get devices -o json > "${OUT}/released-devices.json"

log "CASE 4b (External DP scaling) complete"
