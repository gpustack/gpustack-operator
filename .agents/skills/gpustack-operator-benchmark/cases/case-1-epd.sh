#!/usr/bin/env bash
#
# CASE 1 — Encode, Prefill and Decode multimodal disaggregation benchmark   (MUTATING, self-recovering)
#
#   case-1-epd.sh [OUT_DIR]
#
# Goal:        Compare 3 serving topologies on a 3-GPU budget for a Vision-Language Model
#              (Qwen/Qwen2.5-VL-7B-Instruct):
#              Arm 1 (epd-arm1-aggregated: 3 x Server),
#              Arm 2 (epd-arm2-pd: 2 x Prefill + 1 x Decode with Mooncake KV transfer),
#              Arm 3 (epd-arm3-epd: 1 x Encode + 1 x Prefill + 1 x Decode with NIXL EC + Mooncake KV).
# Environment: A cluster with at least 3 free GPUs on InstanceType bench-gpu, NodeModelStore
#              caching qwen2-5-vl-7b-instruct, and an in-cluster default/bench-driver Pod.
# Inputs:      manifests/epd-arm{1-aggregated,2-pd,3-epd}.yaml; scripts/bench_vlm.py (in-cluster).
# Expected:    All 3 arms complete Suite 1 (1k/2k/4k/8k vision tokens at c=1,4,8), Suite 2 (mixed
#              steady 1k + burst 8k), and Suite 3 (shared-image repeated 4k at c=4,8) with 0 errors
#              and stage-execution evidence in OUT_DIR.
# Cleanup:     Deletes epd-arm1-aggregated, epd-arm2-pd, and epd-arm3-epd on completion or exit.
set -uo pipefail

CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SKILL_DIR="$(cd "${CASE_DIR}/.." && pwd)"
REPO="$(cd "${SKILL_DIR}/../../.." && pwd)"
export PATH="${REPO}/.agents/skills/_e2e-lib/scripts/kubectl-shim:${PATH}"

NS="${BENCH_NS:-gpustack-bench}"
OUT="${1:-${REPO}/.claude/reports/$(date +%F)-benchmark/epd}"
LOCAL_PORT="${LOCAL_PORT:-18001}"
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

run_epd_arm() {
  local manifest="$1" deploy="$2" arm="$3"
  local arm_out="${OUT}/${arm}"
  mkdir -p "$arm_out"

  kubectl -n "$NS" delete modeldeployment epd-arm1-aggregated epd-arm2-pd epd-arm3-epd \
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
  kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${deploy}" -o json > "$arm_out/pods.json"

  local endpoint svc
  endpoint="$(kubectl -n "$NS" get modeldeployment "$deploy" -o jsonpath='{.status.endpoint}')"
  svc="$(echo "$endpoint" | sed -E 's|^[a-z]+://([^.:/]+).*|\1|')"
  kubectl -n "$NS" port-forward "svc/${svc}" "${LOCAL_PORT}:8000" > "$arm_out/port-forward.log" 2>&1 &
  PF_PID=$!
  for _ in $(seq 1 60); do
    curl -sf -o /dev/null "http://127.0.0.1:${LOCAL_PORT}/v1/models" && break
    sleep 2
  done

  local shape_spec tier img_sz txt_tok conc_spec conc reqs
  for shape_spec in "1k:896:256" "2k:1260:256" "4k:1260:2071" "8k:1792:4392"; do
    IFS=':' read -r tier img_sz txt_tok <<< "$shape_spec"
    for conc_spec in "1:8" "4:12" "8:24"; do
      IFS=':' read -r conc reqs <<< "$conc_spec"
      log "Suite 1 ${arm} ${tier} c=${conc} reqs=${reqs}"
      python3 "${SKILL_DIR}/scripts/bench_vlm.py" run \
        --endpoint "http://127.0.0.1:${LOCAL_PORT}" \
        --arm "$arm" \
        --suite suite1 \
        --label "${tier}-c${conc}" \
        --image-size "$img_sz" \
        --text-tokens "$txt_tok" \
        --output-tokens 128 \
        --concurrency "$conc" \
        --requests "$reqs" \
        --warmup 2 \
        --out "${arm_out}/suite1-${tier}-c${conc}.json"
    done
  done

  log "Suite 2 ${arm} burst"
  python3 "${SKILL_DIR}/scripts/bench_vlm.py" burst \
    --endpoint "http://127.0.0.1:${LOCAL_PORT}" \
    --arm "$arm" \
    --steady-requests 24 \
    --steady-concurrency 4 \
    --steady-image-size 896 \
    --steady-text-tokens 256 \
    --steady-output-tokens 128 \
    --burst-requests 8 \
    --burst-concurrency 2 \
    --burst-image-size 1792 \
    --burst-text-tokens 4392 \
    --burst-output-tokens 64 \
    --burst-delay 2.0 \
    --out "${arm_out}/suite2-mixed.json"

  for conc_spec in "4:12" "8:24"; do
    IFS=':' read -r conc reqs <<< "$conc_spec"
    log "Suite 3 ${arm} repeated_4k c=${conc} reqs=${reqs}"
    python3 "${SKILL_DIR}/scripts/bench_vlm.py" run \
      --endpoint "http://127.0.0.1:${LOCAL_PORT}" \
      --arm "$arm" \
      --suite suite3 \
      --label "repeated_4k_c${conc}" \
      --image-size 1792 \
      --text-tokens 400 \
      --output-tokens 64 \
      --concurrency "$conc" \
      --requests "$reqs" \
      --warmup 2 \
      --shared-image \
      --out "${arm_out}/suite3-repeated-c${conc}.json"
  done

  bash "${SKILL_DIR}/scripts/stage_counts.sh" "$deploy" "$arm_out" > "$arm_out/stage-counts-stdout.txt" 2>&1
  kill "$PF_PID" 2>/dev/null || true
  PF_PID=""
  kubectl -n "$NS" delete modeldeployment "$deploy" --wait=true --timeout=180s
}

run_epd_arm "${SKILL_DIR}/manifests/epd-arm1-aggregated.yaml" epd-arm1-aggregated aggregated
run_epd_arm "${SKILL_DIR}/manifests/epd-arm2-pd.yaml" epd-arm2-pd pd
run_epd_arm "${SKILL_DIR}/manifests/epd-arm3-epd.yaml" epd-arm3-epd epd
kubectl get devices -o json > "${OUT}/released-devices.json"
log "CASE 1 (Encode, Prefill and Decode) benchmark complete"
