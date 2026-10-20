#!/usr/bin/env bash
#
# CASE 4 — External DP vs Wide TP MoE serving benchmark   (MUTATING, self-recovering)
#
#   case-4-external-dp.sh [OUT_DIR]
#
# Goal:        Compare 2 8-GPU serving topologies for an MoE model (deepseek-ai/DeepSeek-V2-Lite-Chat
#              BF16):
#              Arm 1 (external-dp-arm1-wide-tp: 1 x Server(TP=8, 8 GPUs)),
#              Arm 2 (external-dp-arm2-external-dp: 1 replica x size=4 members, TP=2 + DP=4 + EP,
#              fronted by vllm-router round_robin, with VLLM_PORT=19500).
# Environment: A cluster with 8 free GPUs on InstanceType bench-gpu, NodeModelStore caching
#              deepseek-v2-lite-chat, and an in-cluster default/bench-driver Pod.
# Inputs:      manifests/external-dp-arm{1-wide-tp,2-external-dp}.yaml; scripts/bench_client.py.
# Expected:    Both arms complete all 12 concurrency cells (1024/256 and 4096/128 at c=1..64) with
#              0 errors, and all 4 DP ranks in Arm 2 serve balanced traffic behind vllm-router.
# Cleanup:     Deletes external-dp-arm1-wide-tp and external-dp-arm2-external-dp on completion.
set -uo pipefail

CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SKILL_DIR="$(cd "${CASE_DIR}/.." && pwd)"
REPO="$(cd "${SKILL_DIR}/../../.." && pwd)"
export PATH="${REPO}/.agents/skills/_e2e-lib/scripts/kubectl-shim:${PATH}"

NS="${BENCH_NS:-gpustack-bench}"
OUT="${1:-${REPO}/.claude/reports/$(date +%F)-benchmark/external-dp}"
LOCAL_PORT="${LOCAL_PORT:-18020}"
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

SHAPES="1024:256 4096:128"
CONCURRENCIES="1 4 8 16 32 64"

run_ext_arm() {
  local manifest="$1" deploy="$2" arm="$3"
  local endpoint svc offset phase

  kubectl -n "$NS" delete modeldeployment external-dp-arm1-wide-tp external-dp-arm2-external-dp \
    --ignore-not-found --wait=true --timeout=180s >/dev/null 2>&1 || true
  log "Applying $manifest ($deploy)"
  kubectl apply -f "$manifest" 2>&1 | tee "$OUT/${arm}-apply.log"

  local deadline=$((SECONDS + ${READY_TIMEOUT:-2400}))
  while [ "$SECONDS" -lt "$deadline" ]; do
    phase="$(kubectl -n "$NS" get modeldeployment "$deploy" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    [ "$phase" = "Ready" ] && break
    sleep 5
  done
  phase="$(kubectl -n "$NS" get modeldeployment "$deploy" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  if [ "$phase" != "Ready" ]; then
    log "TIMEOUT: $deploy phase=${phase:-unknown}"
    kubectl -n "$NS" describe modeldeployment "$deploy" > "$OUT/${arm}-describe.txt" 2>&1
    return 1
  fi
  log "$deploy is Ready"

  kubectl -n "$NS" get modeldeployment "$deploy" -o yaml > "$OUT/${arm}-modeldeployment.yaml"
  kubectl -n "$NS" get pods -o wide > "$OUT/${arm}-pods.txt"
  kubectl -n "$NS" get svc -o wide > "$OUT/${arm}-services.txt"
  endpoint="$(kubectl -n "$NS" get modeldeployment "$deploy" -o jsonpath='{.status.endpoint}')"
  echo "$endpoint" > "$OUT/${arm}-endpoint.txt"

  svc="$(echo "$endpoint" | sed -E 's|^[a-z]+://([^.:/]+).*|\1|')"
  kubectl -n "$NS" port-forward "svc/${svc}" "${LOCAL_PORT}:8000" > "$OUT/${arm}-port-forward.log" 2>&1 &
  PF_PID=$!
  for _ in $(seq 1 90); do
    curl -sf -o /dev/null "http://127.0.0.1:${LOCAL_PORT}/v1/models" && break
    sleep 2
  done

  offset="$(python3 "${SKILL_DIR}/scripts/bench_client.py" calibrate --endpoint "http://127.0.0.1:${LOCAL_PORT}" \
    --model "/models" --input-tokens 1024 2>/dev/null \
    | python3 -c 'import json,sys; print(json.load(sys.stdin).get("token_offset") or 0)')"
  echo "$offset" > "$OUT/${arm}-token-offset.txt"

  local shape in_tok out_tok conc requests
  for shape in $SHAPES; do
    in_tok="${shape%%:*}"
    out_tok="${shape##*:}"
    for conc in $CONCURRENCIES; do
      requests=$((conc * 4))
      [ "$requests" -lt 32 ] && requests=32
      log "cell ${arm}/s${in_tok}x${out_tok}-c${conc}"
      python3 "${SKILL_DIR}/scripts/bench_client.py" run \
        --endpoint "http://127.0.0.1:${LOCAL_PORT}" \
        --model "/models" \
        --label "s${in_tok}x${out_tok}-c${conc}" \
        --arm "$arm" \
        --suite suite1 \
        --shape "${in_tok}/${out_tok}" \
        --input-tokens "$in_tok" \
        --output-tokens "$out_tok" \
        --concurrency "$conc" \
        --requests "$requests" \
        --token-offset "$offset" \
        --out "$OUT/${arm}-s${in_tok}x${out_tok}-c${conc}.json"
    done
  done

  bash "${SKILL_DIR}/scripts/stage_counts.sh" "$deploy" "$OUT" > "$OUT/${arm}-stage-counts.txt" 2>&1
  kubectl -n "$NS" get modeldeployment "$deploy" -o json \
    | jq '.status.roles[] | {name, kind, parallelism, endpoints}' > "$OUT/${arm}-roles.json" 2>/dev/null
  kill "$PF_PID" 2>/dev/null || true
  PF_PID=""
  kubectl -n "$NS" delete modeldeployment "$deploy" --wait=true --timeout=180s
}

run_ext_arm "${SKILL_DIR}/manifests/external-dp-arm1-wide-tp.yaml" external-dp-arm1-wide-tp arm1
run_ext_arm "${SKILL_DIR}/manifests/external-dp-arm2-external-dp.yaml" external-dp-arm2-external-dp arm2
kubectl get devices -o json > "${OUT}/released-devices.json"
log "CASE 4 (External DP) benchmark complete"
