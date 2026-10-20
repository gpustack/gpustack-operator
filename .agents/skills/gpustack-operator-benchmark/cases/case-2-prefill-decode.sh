#!/usr/bin/env bash
#
# CASE 2 — Prefill and Decode disaggregation benchmark   (MUTATING, self-recovering)
#
#   case-2-prefill-decode.sh [OUT_DIR]
#
# Goal:        Compare 2 serving topologies on a 6-GPU budget (TP=2 per engine) for a dense 32B LLM
#              (Qwen/Qwen3-32B BF16):
#              Arm 1 (pd-arm1-aggregated: 3 x Server(TP=2)),
#              Arm 2 (pd-arm2-pd: 1 x Prefill(TP=2) + 2 x Decode(TP=2) with Mooncake KV transfer).
# Environment: A cluster with at least 6 free GPUs on InstanceType bench-gpu, NodeModelStore
#              caching qwen3-32b, and an in-cluster default/bench-driver Pod.
# Inputs:      manifests/pd-arm{1-aggregated,2-pd}.yaml; scripts/bench_client.py (in-cluster).
# Expected:    Both arms complete Suite 1 (512/256, 4096/128, 256/1024, 8192/128 at c=1,4,8,16)
#              and Suite 2 (steady 512/256 + burst 4096-8192/64) with 0 errors and stage evidence.
# Cleanup:     Deletes pd-arm1-aggregated and pd-arm2-pd on completion or exit.
set -uo pipefail

CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SKILL_DIR="$(cd "${CASE_DIR}/.." && pwd)"
REPO="$(cd "${SKILL_DIR}/../../.." && pwd)"
export PATH="${REPO}/.agents/skills/_e2e-lib/scripts/kubectl-shim:${PATH}"

NS="${BENCH_NS:-gpustack-bench}"
OUT="${1:-${REPO}/.claude/reports/$(date +%F)-benchmark/prefill-decode}"
LOCAL_PORT="${LOCAL_PORT:-18002}"
PF_PID=""

mkdir -p "$OUT"
log() { echo "[$(date +%H:%M:%S)] $*"; }
cleanup() {
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
  return 0
}
trap cleanup EXIT

SUITES=(
  "s512x256-c1:512:256:1:32"
  "s512x256-c4:512:256:4:32"
  "s512x256-c8:512:256:8:32"
  "s512x256-c16:512:256:16:64"
  "s4096x128-c1:4096:128:1:32"
  "s4096x128-c4:4096:128:4:32"
  "s4096x128-c8:4096:128:8:32"
  "s4096x128-c16:4096:128:16:64"
  "s256x1024-c1:256:1024:1:32"
  "s256x1024-c4:256:1024:4:32"
  "s256x1024-c8:256:1024:8:32"
  "s256x1024-c16:256:1024:16:64"
  "s8192x128-c1:8192:128:1:32"
  "s8192x128-c4:8192:128:4:32"
  "s8192x128-c8:8192:128:8:32"
  "s8192x128-c16:8192:128:16:64"
)

run_pd_arm() {
  local manifest="$1" deploy="$2" arm="$3"
  local arm_out="${OUT}/${arm}"
  mkdir -p "$arm_out"

  kubectl -n "$NS" delete modeldeployment pd-arm1-aggregated pd-arm2-pd \
    --ignore-not-found --wait=true --timeout=180s >/dev/null 2>&1 || true
  log "Applying $manifest ($deploy)"
  kubectl apply -f "$manifest" 2>&1 | tee "$arm_out/apply.log"

  local deadline=$((SECONDS + ${READY_TIMEOUT:-1800}))
  local phase=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    phase="$(kubectl -n "$NS" get modeldeployment "$deploy" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    [ "$phase" = "Ready" ] && break
    sleep 5
  done
  phase="$(kubectl -n "$NS" get modeldeployment "$deploy" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  if [ "$phase" != "Ready" ]; then
    log "TIMEOUT: $deploy phase=${phase:-unknown}"
    kubectl -n "$NS" describe modeldeployment "$deploy" > "$arm_out/describe.txt" 2>&1
    return 1
  fi
  log "$deploy is Ready"

  kubectl -n "$NS" get modeldeployment "$deploy" -o yaml > "$arm_out/modeldeployment.yaml"
  kubectl -n "$NS" get pods -o wide > "$arm_out/pods.txt"
  kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${deploy}" -o json > "$arm_out/pods-final.json"

  local endpoint svc offset
  endpoint="$(kubectl -n "$NS" get modeldeployment "$deploy" -o jsonpath='{.status.endpoint}')"
  svc="$(echo "$endpoint" | sed -E 's|^[a-z]+://([^.:/]+).*|\1|')"
  kubectl -n "$NS" port-forward "svc/${svc}" "${LOCAL_PORT}:8000" > "$arm_out/port-forward.log" 2>&1 &
  PF_PID=$!
  for _ in $(seq 1 60); do
    curl -sf -o /dev/null "http://127.0.0.1:${LOCAL_PORT}/v1/models" && break
    sleep 2
  done

  offset="$(python3 "${SKILL_DIR}/scripts/bench_client.py" calibrate --endpoint "http://127.0.0.1:${LOCAL_PORT}" \
    --model "/models" --input-tokens 512 2>/dev/null \
    | python3 -c 'import json,sys; print(json.load(sys.stdin).get("token_offset") or 0)')"
  echo "$offset" > "$arm_out/token-offset.txt"

  local spec label in_tok out_tok conc reqs
  for spec in "${SUITES[@]}"; do
    IFS=':' read -r label in_tok out_tok conc reqs <<< "$spec"
    log "Suite 1 ${arm} ${label}"
    python3 "${SKILL_DIR}/scripts/bench_client.py" run \
      --endpoint "http://127.0.0.1:${LOCAL_PORT}" \
      --model "/models" \
      --label "$label" \
      --arm "$arm" \
      --suite suite1 \
      --shape "${in_tok}/${out_tok}" \
      --input-tokens "$in_tok" \
      --output-tokens "$out_tok" \
      --concurrency "$conc" \
      --requests "$reqs" \
      --token-offset "$offset" \
      --out "${arm_out}/${arm}-${label}.json"
  done

  log "Suite 2 ${arm} mixed steady + burst"
  python3 "${SKILL_DIR}/scripts/bench_client.py" mixed \
    --endpoint "http://127.0.0.1:${LOCAL_PORT}" \
    --model "/models" \
    --label mixed \
    --arm "$arm" \
    --suite suite2 \
    --steady-requests 64 \
    --steady-input 512 \
    --steady-output 256 \
    --steady-concurrency 4 \
    --burst-requests 40 \
    --burst-input 4096 \
    --burst-input-max 8192 \
    --burst-output 64 \
    --burst-concurrency 4 \
    --burst-interval 2.0 \
    --token-offset "$offset" \
    --out "${arm_out}/${arm}-mixed.json"

  bash "${SKILL_DIR}/scripts/stage_counts.sh" "$deploy" "$arm_out" > "$arm_out/stage-counts-final.txt" 2>&1
  kill "$PF_PID" 2>/dev/null || true
  PF_PID=""
  kubectl -n "$NS" delete modeldeployment "$deploy" --wait=true --timeout=180s
}

run_pd_arm "${SKILL_DIR}/manifests/pd-arm1-aggregated.yaml" pd-arm1-aggregated aggregated
run_pd_arm "${SKILL_DIR}/manifests/pd-arm2-pd.yaml" pd-arm2-pd pd
kubectl get devices -o json > "${OUT}/released-devices.json"
log "CASE 2 (Prefill and Decode) benchmark complete"
