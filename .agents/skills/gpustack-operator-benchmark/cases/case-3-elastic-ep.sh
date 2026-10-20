#!/usr/bin/env bash
#
# CASE 3 — Elastic EP online horizontal scale-up benchmark   (MUTATING, self-recovering)
#
#   case-3-elastic-ep.sh [OUT_DIR]
#
# Goal:        Verify and measure online Elastic Expert Parallelism scaling on an MoE model
#              (deepseek-ai/DeepSeek-V2-Lite-Chat BF16):
#              State 1 (elasticEp.width=2, 4 GPUs, c=1..64 sweep),
#              State 2 (live patch elasticEp.width: 2 -> 4 under continuous c=8 load),
#              State 3 (elasticEp.width=4, 8 GPUs, c=1..64 sweep).
# Environment: A cluster with 8 free GPUs on InstanceType bench-gpu, NodeModelStore caching
#              deepseek-v2-lite-chat, and an in-cluster default/bench-driver Pod.
# Inputs:      manifests/elastic-ep.yaml; scripts/bench_client.py (in-cluster).
# Expected:    State 1 and State 3 complete all 6 concurrency cells (c=1..64) with 0 errors,
#              State 2 preserves master Pod UID across width 2 -> 4 resize, and all 8 GPUs are
#              released after deletion.
# Cleanup:     Deletes b2-elastic-ep on completion or exit.
set -uo pipefail

CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SKILL_DIR="$(cd "${CASE_DIR}/.." && pwd)"
REPO="$(cd "${SKILL_DIR}/../../.." && pwd)"
export PATH="${REPO}/.agents/skills/_e2e-lib/scripts/kubectl-shim:${PATH}"

NS="${BENCH_NS:-gpustack-bench}"
DEPLOY=b2-elastic-ep
OUT="${1:-${REPO}/.claude/reports/$(date +%F)-benchmark/elastic-ep}"
LOCAL_PORT="${LOCAL_PORT:-18010}"
PF_PID=""
WATCH_PID=""

mkdir -p "$OUT"
log() { echo "[$(date +%H:%M:%S)] $*"; }
cleanup() {
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
  if [ -n "$WATCH_PID" ]; then
    kill "$WATCH_PID" 2>/dev/null || true
  fi
  return 0
}
trap cleanup EXIT

CONCURRENCIES="1 4 8 16 32 64"

wait_ready() {
  local deadline=$((SECONDS + ${READY_TIMEOUT:-2400}))
  local phase=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    phase="$(kubectl -n "$NS" get modeldeployment "$DEPLOY" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    [ "$phase" = "Ready" ] && { log "$DEPLOY is Ready"; return 0; }
    sleep 5
  done
  kubectl -n "$NS" describe modeldeployment "$DEPLOY" > "$OUT/describe.txt" 2>&1
  return 1
}

start_forward() {
  local pod
  pod="$(kubectl -n "$NS" get pods -o json 2>/dev/null | jq -r \
    '[.items[] | select(.metadata.labels["modeldeployment.gpustack.ai/role-kind"]=="server")
      | select(.metadata.labels["modeldeployment.gpustack.ai/member-index"]=="0")
      | .metadata.name] | .[0] // empty')"
  if [ -z "$pod" ]; then
    pod="$(kubectl -n "$NS" get pods -o name 2>/dev/null | cut -d/ -f2 | grep "^${DEPLOY}-" | head -1)"
  fi
  kubectl -n "$NS" port-forward "pod/${pod}" "${LOCAL_PORT}:8000" > "$OUT/port-forward.log" 2>&1 &
  PF_PID=$!
  for _ in $(seq 1 60); do
    curl -sf -o /dev/null "http://127.0.0.1:${LOCAL_PORT}/v1/models" && break
    sleep 2
  done
}

sweep() {
  local arm="$1"
  local offset conc requests
  offset="$(cat "$OUT/token-offset.txt" 2>/dev/null || echo 0)"
  for conc in $CONCURRENCIES; do
    requests=$((conc * 4))
    [ "$requests" -lt 32 ] && requests=32
    [ "$requests" -gt 128 ] && requests=128
    log "cell ${arm}/s1024x256-c${conc} (reqs=${requests})"
    python3 "${SKILL_DIR}/scripts/bench_client.py" run \
      --endpoint "http://127.0.0.1:${LOCAL_PORT}" \
      --model "/models" \
      --label "s1024x256-c${conc}" \
      --arm "$arm" \
      --suite suite1 \
      --shape "1024/256" \
      --input-tokens 1024 \
      --output-tokens 256 \
      --concurrency "$conc" \
      --requests "$requests" \
      --token-offset "$offset" \
      --out "$OUT/${arm}-s1024x256-c${conc}.json"
  done
}

kubectl -n "$NS" delete modeldeployment "$DEPLOY" --ignore-not-found --wait=true --timeout=180s >/dev/null 2>&1 || true
kubectl apply -f "${SKILL_DIR}/manifests/elastic-ep.yaml" 2>&1 | tee "$OUT/apply.log"
wait_ready || exit 1
start_forward || exit 1

OFFSET="$(python3 "${SKILL_DIR}/scripts/bench_client.py" calibrate --endpoint "http://127.0.0.1:${LOCAL_PORT}" \
  --model "/models" --input-tokens 1024 2>/dev/null \
  | python3 -c 'import json,sys; print(json.load(sys.stdin).get("token_offset") or 0)')"
echo "$OFFSET" > "$OUT/token-offset.txt"

kubectl -n "$NS" get modeldeployment "$DEPLOY" -o yaml > "$OUT/modeldeployment-state1.yaml"
kubectl -n "$NS" get modeldeployment "$DEPLOY" -o json > "$OUT/baseline-cr.json"
kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${DEPLOY}" -o json > "$OUT/pods-state1.json"
sweep state1

log "Starting steady load for scale-up window (width 2 -> 4)"
python3 "${SKILL_DIR}/scripts/bench_client.py" load \
  --endpoint "http://127.0.0.1:${LOCAL_PORT}" \
  --model "/models" \
  --label scaleup-load \
  --arm state2 \
  --suite suite2 \
  --shape "1024/256" \
  --input-tokens 1024 \
  --output-tokens 256 \
  --concurrency 8 \
  --duration 260 \
  --token-offset "$OFFSET" \
  --requests-out "$OUT/scaleup-requests.jsonl" > "$OUT/scaleup-load.json" 2>&1 &
LOAD_PID=$!

sleep 35
PATCH_T0="$(date +%s)"
kubectl -n "$NS" patch modeldeployment "$DEPLOY" --type=json \
  -p '[{"op":"replace","path":"/spec/roles/0/elasticEp/width","value":4}]' 2>&1 | tee "$OUT/patch.log"

(
  for _ in $(seq 1 240); do
    printf '%s %s %s\n' "$(date +%s)" \
      "$(kubectl -n "$NS" get modeldeployment "$DEPLOY" -o jsonpath='{.status.phase}' 2>/dev/null)" \
      "$(kubectl -n "$NS" get modeldeployment "$DEPLOY" -o jsonpath='{.status.roleSummary}' 2>/dev/null)"
    sleep 5
  done
) > "$OUT/resize-watch.txt" 2>&1 &
WATCH_PID=$!

wait_ready || true
PATCH_T1="$(date +%s)"
echo $((PATCH_T1 - PATCH_T0)) > "$OUT/resize-seconds.txt"
kill "$WATCH_PID" 2>/dev/null || true
WATCH_PID=""
wait "$LOAD_PID" 2>/dev/null || true

kubectl -n "$NS" get modeldeployment "$DEPLOY" -o yaml > "$OUT/modeldeployment-state3.yaml"
kubectl -n "$NS" get modeldeployment "$DEPLOY" -o json > "$OUT/scaled-cr.json"
kubectl -n "$NS" get pods -o wide > "$OUT/pods-state3.txt"
kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${DEPLOY}" -o json > "$OUT/pods-state3.json"

bash "${SKILL_DIR}/scripts/stage_counts.sh" "$DEPLOY" "$OUT" > "$OUT/stage-capture-state3.txt" 2>&1
sweep state3
bash "${SKILL_DIR}/scripts/stage_counts.sh" "$DEPLOY" "$OUT" > "$OUT/stage-capture-final.txt" 2>&1
kill "$PF_PID" 2>/dev/null || true
PF_PID=""
kubectl -n "$NS" delete modeldeployment "$DEPLOY" --wait=true --timeout=180s
kubectl get devices -o json > "$OUT/released-devices.json"
log "CASE 3 (Elastic EP) benchmark complete"
