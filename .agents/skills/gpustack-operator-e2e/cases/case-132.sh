#!/usr/bin/env bash
# CASE 132 — Live P/D capacityPrior whole-unit scaling under TokenThroughput (MUTATING, self-recovering)
#   case-132.sh <NS>
# Prerequisites: nodes=1; gpu=nvidia; cluster=real; inputs=E2E_C132_ACCELERATOR_TYPE,E2E_C132_ARTIFACT,E2E_C132_ENGINE_IMAGE,E2E_C132_MODEL,E2E_C132_PROBE_IMAGE,E2E_C132_READY_BOUND,E2E_C132_RETIRE_BOUND,E2E_C132_SCALE_BOUND
# Goal:        Verify an Active TokenThroughput ModelDeploymentScaler configured with a calibrated
#              disaggregated (1P, 2D) capacityPrior catalog and fixed 1:2 ratio resolves
#              CapacityPriorsReady=True, atomically scales a live routed vLLM P/D ModelDeployment
#              from 1P:2D to 2P:4D under token load, and atomically retires back to 1P:2D when
#              load drops, with zero failed probe requests and zero container restarts.
# Environment: Real GPU cluster with at least 6 allocatable GPUs on one usable AcceleratorType,
#              pre-warmed ModelArtifact (E2E_C132_ARTIFACT, default qwen3-8b) with resolved
#              weights for E2E_C132_MODEL (default Qwen/Qwen3-8B), pullable vLLM runner image
#              (E2E_C132_ENGINE_IMAGE, default gpustack/runner:cuda13.0-vllm0.32.0), and pullable
#              probe image (E2E_C132_PROBE_IMAGE, default curlimages/curl:8.10.1). Auto-skips if
#              fewer than 6 GPUs or no pre-warmed ModelArtifact is available.
# Inputs:      <NS>                       Namespace for workload, ConfigMap, Scaler, and probe Pod.
#              E2E_C132_ACCELERATOR_TYPE  Override AcceleratorType name (optional).
#              E2E_C132_ARTIFACT          Pre-warmed ModelArtifact name in <NS> (default: qwen3-8b).
#              E2E_C132_ENGINE_IMAGE      Engine container image (default: gpustack/runner:cuda13.0-vllm0.32.0).
#              E2E_C132_MODEL             Model repository name (default: Qwen/Qwen3-8B).
#              E2E_C132_PROBE_IMAGE       Probe Pod image (default: curlimages/curl:8.10.1).
#              E2E_C132_READY_BOUND       Seconds to wait for initial P/D Ready (default: 600).
#              E2E_C132_SCALE_BOUND       Seconds to wait for 1P:2D -> 2P:4D scale-up (default: 480).
#              E2E_C132_RETIRE_BOUND      Seconds to wait for 2P:4D -> 1P:2D scale-down (default: 480).
# Expected:    Scaler resolves CapacityPriorsReady=True, Bound=True, Active=True; scales
#              atomically from 1P:2D to 2P:4D under heavy concurrent load; retires back to
#              1P:2D when heavy load stops; continuous router probe sees 0 failed requests and
#              serving Pods record 0 container restarts.
# Cleanup:     EXIT trap deletes only the probe Pod, Scaler, ModelDeployment, and ConfigMap created by this run.
set -uo pipefail

NS="${1:?usage: case-132.sh <namespace>}"
export CASE_ID=132
CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "${CASE_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_md-lib.sh"

SCALER_CRD="modeldeploymentscalers.worker.gpustack.ai"
MD_CRD="modeldeployments.worker.gpustack.ai"
SCALER_RES="modeldeploymentscalers.v1alpha1.worker.gpustack.ai"
MD_RES="modeldeployments.v1alpha1.worker.gpustack.ai"

ARTIFACT="${E2E_C132_ARTIFACT:-qwen3-8b}"
MODEL="${E2E_C132_MODEL:-Qwen/Qwen3-8B}"
ENGINE_IMAGE="${E2E_C132_ENGINE_IMAGE:-gpustack/runner:cuda13.0-vllm0.32.0}"
PROBE_IMAGE="${E2E_C132_PROBE_IMAGE:-curlimages/curl:8.10.1}"
READY_BOUND="${E2E_C132_READY_BOUND:-600}"
SCALE_BOUND="${E2E_C132_SCALE_BOUND:-480}"
RETIRE_BOUND="${E2E_C132_RETIRE_BOUND:-480}"

if ! kubectl get crd "$SCALER_CRD" >/dev/null 2>&1 || ! kubectl get crd "$MD_CRD" >/dev/null 2>&1; then
  record SKIP "case-132 prerequisites" "missing ${SCALER_CRD} or ${MD_CRD}"
  print_rows
  exit 0
fi

AT="${E2E_C132_ACCELERATOR_TYPE:-$(md_usable_accelerator_type "$NS" || true)}"
if [ -z "$AT" ]; then
  record SKIP "case-132 prerequisites" "no usable AcceleratorType available in cluster"
  print_rows
  exit 0
fi

HW_PRODUCT="$(kubectl get acceleratortype "$AT" -o jsonpath='{.status.detail.product}' 2>/dev/null || true)"
HW_MFG="$(kubectl get acceleratortype "$AT" -o jsonpath='{.status.detail.manufacturer}' 2>/dev/null || true)"
HW_RES="$(kubectl get acceleratortype "$AT" -o jsonpath='{.status.detail.resourceName}' 2>/dev/null || true)"
if [ -z "$HW_PRODUCT" ]; then
  record SKIP "case-132 prerequisites" "AcceleratorType ${AT} has empty status.detail.product"
  print_rows
  exit 0
fi
HW_MFG_LC="$(printf '%s' "$HW_MFG" | tr '[:upper:]' '[:lower:]')"
if [ -z "${E2E_C132_ENGINE_IMAGE:-}" ] && [ "$HW_MFG_LC" != "nvidia" ]; then
  record SKIP "case-132 prerequisites" "default CUDA engine image requires nvidia AcceleratorType, got ${HW_MFG:-unknown}"
  print_rows
  exit 0
fi
[ -n "$HW_RES" ] || HW_RES="nvidia.com/gpu"

# Check cluster GPU capacity (>= 6 required for 2P:4D at tp=1).
GPU_CAP="$(kubectl get nodes -o json 2>/dev/null | python3 -c '
import json, sys
res = sys.argv[1]
doc = json.load(sys.stdin)
total = 0
for n in doc.get("items", []):
    conds = {c["type"]: c["status"] for c in n.get("status", {}).get("conditions", [])}
    if conds.get("Ready") != "True":
        continue
    val = n.get("status", {}).get("allocatable", {}).get(res, "0")
    try:
        total += int(float(val))
    except ValueError:
        pass
print(total)
' "$HW_RES" 2>/dev/null || echo 0)"
if [ "${GPU_CAP:-0}" -lt 6 ]; then
  record SKIP "case-132 prerequisites" "need >= 6 allocatable ${HW_RES} GPUs for 1P:2D -> 2P:4D, found ${GPU_CAP:-0}"
  print_rows
  exit 0
fi

# Verify pre-warmed ModelArtifact with resolved revision exists in NS.
if ! kubectl -n "$NS" get modelartifact "$ARTIFACT" >/dev/null 2>&1; then
  record SKIP "case-132 prerequisites" "ModelArtifact ${NS}/${ARTIFACT} not found (pre-warm weights before running)"
  print_rows
  exit 0
fi
ART_REV="$(kubectl -n "$NS" get modelartifact "$ARTIFACT" -o jsonpath='{.status.resolved.revision}' 2>/dev/null || true)"
if [ -z "$ART_REV" ]; then
  record SKIP "case-132 prerequisites" "ModelArtifact ${NS}/${ARTIFACT} has empty status.resolved.revision (weights not warmed)"
  print_rows
  exit 0
fi

RUN_PREFIX="c132-$$"
DEPLOY="${RUN_PREFIX}-pd"
SCALER="${RUN_PREFIX}-scaler"
CM_NAME="${RUN_PREFIX}-priors"
PROBE_POD="${RUN_PREFIX}-probe"

CREATED_POD=""
CREATED_SCALER=""
CREATED_MD=""
CREATED_CM=""
TMP_DIR="$(mktemp -d)"
CLEANED=false
# shellcheck disable=SC2317 # Invoked by EXIT/INT/TERM trap.
cleanup() {
  if [ "$CLEANED" = true ]; then
    return 0
  fi
  CLEANED=true
  if [ -n "$CREATED_POD" ]; then
    kubectl -n "$NS" delete pod "$CREATED_POD" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  fi
  if [ -n "$CREATED_SCALER" ]; then
    kubectl -n "$NS" delete "$SCALER_RES" "$CREATED_SCALER" --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1 || true
  fi
  if [ -n "$CREATED_MD" ]; then
    kubectl -n "$NS" delete "$MD_RES" "$CREATED_MD" --ignore-not-found --wait=true --timeout=180s >/dev/null 2>&1 || true
  fi
  if [ -n "$CREATED_CM" ]; then
    kubectl -n "$NS" delete configmap "$CREATED_CM" --ignore-not-found >/dev/null 2>&1 || true
  fi
  rm -rf "$TMP_DIR"
  return 0
}
trap cleanup EXIT INT TERM

for pair in "configmap:${CM_NAME}" "${MD_RES}:${DEPLOY}" "${SCALER_RES}:${SCALER}" "pod:${PROBE_POD}"; do
  res="${pair%%:*}"
  name="${pair#*:}"
  if kubectl -n "$NS" get "$res" "$name" >/dev/null 2>&1; then
    record FAIL "resource collision guard" "${NS}/${res}/${name} already exists before test mutation"
    results || true
    exit 1
  fi
done

RESTART_LEDGER="${TMP_DIR}/restarts.json"
printf '{}\n' > "$RESTART_LEDGER"
RESTART_READ_ERR=""

sample_pod_restarts() {
  local pods_json updated
  if ! pods_json="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${DEPLOY}" -o json 2>/dev/null)"; then
    RESTART_READ_ERR="failed to list pods for ${DEPLOY}"
    return 1
  fi
  if ! updated="$(printf '%s' "$pods_json" | jq -ce --slurpfile prev "$RESTART_LEDGER" '
    reduce (.items // [])[] as $p (
      ($prev[0] // {});
      ($p.metadata.uid // "") as $uid |
      if $uid == "" then .
      else
        (([($p.status.containerStatuses // [])[]?.restartCount // 0,
           ($p.status.initContainerStatuses // [])[]?.restartCount // 0] | add) // 0) as $cnt |
        .[$uid] = ([.[$uid] // 0, $cnt] | max)
      end
    )
  ' 2>/dev/null)"; then
    RESTART_READ_ERR="failed to aggregate container restart counts"
    return 1
  fi
  printf '%s\n' "$updated" > "$RESTART_LEDGER"
  return 0
}

echo "=== CASE 132: Live P/D capacityPrior scaling (ns=${NS}, at=${AT}, product=${HW_PRODUCT}, gpus=${GPU_CAP}, artifact=${ARTIFACT}@${ART_REV:0:8}) ==="

# Calibrated (1P, 2D) disaggregated catalog:
# Concurrency 2 gives totalTokensPerSecond=40; with headroom=0.75, unitTargetTokensPerSecond=30.
CREATED_CM="$CM_NAME"
kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${CM_NAME}
data:
  priors.yaml: |
    apiVersion: worker.gpustack.ai/v1alpha1
    kind: ModelDeploymentScalerCapacityPriorCatalog
    profiles:
      - mode: disaggregated
        model: ${MODEL}
        quantization: bf16
        engine: vllm
        prefill:
          replicas: 1
          hardware: ${HW_PRODUCT}
          topology:
            gpusPerReplica: 1
            tensorParallel: 1
            expertParallel: 1
        decode:
          replicas: 2
          hardware: ${HW_PRODUCT}
          topology:
            gpusPerReplica: 1
            tensorParallel: 1
            expertParallel: 1
        points:
          - concurrency: 1
            totalTokensPerSecond: 20
            inputTokensPerSecond: 15
            outputTokensPerSecond: 5
            ttftMilliseconds:
              p99: 400
            itlMilliseconds:
              p99: 30
          - concurrency: 2
            totalTokensPerSecond: 40
            inputTokensPerSecond: 30
            outputTokensPerSecond: 10
            ttftMilliseconds:
              p99: 450
            itlMilliseconds:
              p99: 35
YAML

CREATED_MD="$DEPLOY"
kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${DEPLOY}
  namespace: ${NS}
  annotations:
    worker.gpustack.ai/precision: "bf16"
spec:
  model:
    name: ${MODEL}
    artifactRef:
      name: ${ARTIFACT}
  engine:
    name: vLLM
    image: ${ENGINE_IMAGE}
  router:
    name: llm-d-router
  roles:
    - name: prefill
      kind: Prefill
      replicas: 1
      acceleratorType: ${AT}
      image: ${ENGINE_IMAGE}
      imagePullPolicy: IfNotPresent
      resources:
        accelerator: "1"
      env:
        - name: VLLM_HTTP_TIMEOUT_KEEP_ALIVE
          value: "3600"
      extraArgs:
        - --tensor-parallel-size
        - "1"
        - --max-model-len
        - "16384"
        - --max-num-seqs
        - "64"
        - --gpu-memory-utilization
        - "0.90"
        - --enable-scale-out
    - name: decode
      kind: Decode
      replicas: 2
      acceleratorType: ${AT}
      image: ${ENGINE_IMAGE}
      imagePullPolicy: IfNotPresent
      resources:
        accelerator: "1"
      env:
        - name: VLLM_HTTP_TIMEOUT_KEEP_ALIVE
          value: "3600"
      extraArgs:
        - --tensor-parallel-size
        - "1"
        - --max-model-len
        - "16384"
        - --max-num-seqs
        - "64"
        - --gpu-memory-utilization
        - "0.90"
        - --enable-scale-out
YAML

CREATED_SCALER="$SCALER"
kubectl -n "$NS" apply -f - >/dev/null <<YAML
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
    intervalSeconds: 5
    pendingTimeoutSeconds: 180
  roles:
    - name: prefill
      minReplicas: 1
      maxReplicas: 2
    - name: decode
      minReplicas: 2
      maxReplicas: 4
  ratios:
    - numeratorRole: decode
      denominatorRole: prefill
      target: "2"
      min: "2"
      max: "2"
  behavior:
    scaleDown:
      stabilizationWindowSeconds: 45
      decodePostScaleUpGraceSeconds: 90
  algorithm:
    type: TokenThroughput
    tokenThroughput:
      ttftTargetMilliseconds: "500"
      itlTargetMilliseconds: "50"
      stableWindowSeconds: 30
      panicWindowSeconds: 10
    capacityPrior:
      headroom: "0.75"
      catalogRef:
        name: ${CM_NAME}
YAML

role_spec_replicas() {
  local role="$1"
  kubectl -n "$NS" get "$MD_RES" "$DEPLOY" -o json 2>/dev/null | \
    jq -r --arg r "$role" '[(.spec.roles // [])[] | select(.name==$r) | .replicas] | .[0] // 0' 2>/dev/null || echo 0
}

role_ready_replicas() {
  local role="$1"
  kubectl -n "$NS" get "$MD_RES" "$DEPLOY" -o json 2>/dev/null | \
    jq -r --arg r "$role" '[(.status.roles // [])[] | select(.name==$r) | .ready] | .[0] // 0' 2>/dev/null || echo 0
}

wait_probe_worker() {
  local pid_file="$1" bound="$2"
  kubectl -n "$NS" exec "$PROBE_POD" -- sh -c "
    pid=\$(cat '${pid_file}' 2>/dev/null || true)
    [ -n \"\$pid\" ] || exit 1
    elapsed=0
    while kill -0 \"\$pid\" 2>/dev/null; do
      if [ \"\$elapsed\" -ge ${bound} ]; then
        exit 1
      fi
      sleep 1
      elapsed=\$((elapsed + 1))
    done
    exit 0
  " >/dev/null 2>&1
}

# Wait for CapacityPriorsReady=True, Bound=True, Active=True.
deadline=$((SECONDS + 90))
priors_ok=false
while [ "$SECONDS" -lt "$deadline" ]; do
  sc_json="$(kubectl -n "$NS" get "$SCALER_RES" "$SCALER" -o json 2>/dev/null || true)"
  c_priors="$(printf '%s' "$sc_json" | jq -r '[(.status.conditions // [])[] | select(.type=="CapacityPriorsReady") | .status] | .[0] // ""' 2>/dev/null || true)"
  c_bound="$(printf '%s' "$sc_json" | jq -r '[(.status.conditions // [])[] | select(.type=="Bound") | .status] | .[0] // ""' 2>/dev/null || true)"
  c_active="$(printf '%s' "$sc_json" | jq -r '[(.status.conditions // [])[] | select(.type=="Active") | .status] | .[0] // ""' 2>/dev/null || true)"
  if [ "$c_priors" = "True" ] && [ "$c_bound" = "True" ] && [ "$c_active" = "True" ]; then
    priors_ok=true
    break
  fi
  sleep 2
done
if [ "$priors_ok" = true ]; then
  record PASS "scaler CapacityPriorsReady & Bound & Active" "resolved disaggregated 1P:2D catalog"
else
  conds="$(kubectl -n "$NS" get "$SCALER_RES" "$SCALER" -o json 2>/dev/null | jq -c '[(.status.conditions // [])[] | {type,status,reason,message}]' || true)"
  record FAIL "scaler CapacityPriorsReady & Bound & Active" "conditions=${conds}"
  results || true
  exit 1
fi

# Wait for initial 1P:2D ModelDeployment Ready.
deadline=$((SECONDS + READY_BOUND))
md_ready=false
while [ "$SECONDS" -lt "$deadline" ]; do
  phase="$(kubectl -n "$NS" get "$MD_RES" "$DEPLOY" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  pr="$(role_ready_replicas prefill)"
  dr="$(role_ready_replicas decode)"
  if [ "$phase" = "Ready" ] && [ "${pr:-0}" -ge 1 ] && [ "${dr:-0}" -ge 2 ]; then
    md_ready=true
    break
  fi
  sleep 5
done
if [ "$md_ready" = true ]; then
  record PASS "initial 1P:2D deployment ready" "phase=Ready prefill=1 decode=2"
else
  record FAIL "initial 1P:2D deployment ready" "timeout (${READY_BOUND}s), phase=${phase:-unknown} prefill=${pr:-0} decode=${dr:-0}"
  results || true
  exit 1
fi
sample_pod_restarts || true

# Create in-cluster curl probe Pod.
CREATED_POD="$PROBE_POD"
kubectl -n "$NS" run "$PROBE_POD" --image="$PROBE_IMAGE" --restart=Never --command -- sleep infinity >/dev/null
if ! kubectl -n "$NS" wait "pod/${PROBE_POD}" --for=condition=Ready --timeout=180s >/dev/null 2>&1; then
  record FAIL "probe Pod ready" "Pod ${NS}/${PROBE_POD} failed to become Ready"
  results || true
  exit 1
fi

ROUTER_URL="http://${DEPLOY}-router.${NS}.svc.cluster.local:8000"
MODELS_JSON=""
deadline=$((SECONDS + 120))
while [ "$SECONDS" -lt "$deadline" ]; do
  MODELS_JSON="$(kubectl -n "$NS" exec "$PROBE_POD" -- curl -sf --max-time 10 "${ROUTER_URL}/v1/models" 2>/dev/null || true)"
  if printf '%s' "$MODELS_JSON" | grep -Fq '"data"'; then
    break
  fi
  sleep 3
done
SERVED_MODEL="$(printf '%s' "$MODELS_JSON" | jq -r '.data[0].id // ""' 2>/dev/null || true)"
[ -n "$SERVED_MODEL" ] || SERVED_MODEL="$MODEL"
if [ -n "$MODELS_JSON" ]; then
  record PASS "router /v1/models reachable" "model=${SERVED_MODEL}"
else
  record FAIL "router /v1/models reachable" "could not query ${ROUTER_URL}/v1/models"
  results || true
  exit 1
fi

# Start continuous canary loop inside probe Pod (1 request every 2s, counts ok vs fail).
# Encode payloads in base64 and pass them via argv instead of kubectl exec -i stdin.
CANARY_B64="$(jq -nc --arg m "$SERVED_MODEL" '{model:$m, messages:[{role:"user",content:"Reply with one word: ok."}], max_tokens:4, temperature:0}' | base64 | tr -d '\n\r')"
kubectl -n "$NS" exec "$PROBE_POD" -- sh -c "
printf '%s' '${CANARY_B64}' | base64 -d > /tmp/canary.json
test -s /tmp/canary.json
cat > /tmp/canary.sh <<'EOF'
#!/bin/sh
: > /tmp/canary_ok_lines
: > /tmp/canary_fail_codes
: > /tmp/canary_fail_details
rm -f /tmp/stop_canary
while [ ! -f /tmp/stop_canary ]; do
  rc=0
  code=\$(curl -sS -o /tmp/canary.body -w '%{http_code}' --max-time 60 -H 'Content-Type: application/json' -d @/tmp/canary.json '${ROUTER_URL}/v1/chat/completions' 2>/tmp/canary.err) || rc=\$?
  if [ \"\$rc\" -eq 0 ] && [ \"\$code\" = \"200\" ] && grep -Fq '\"choices\"' /tmp/canary.body 2>/dev/null; then
    echo 1 >> /tmp/canary_ok_lines
  else
    echo \"rc=\${rc}:\${code:-000}\" >> /tmp/canary_fail_codes
    printf '%s rc=%s code=%s err=%s body=%s\n' \"\$(date +%H:%M:%S)\" \"\$rc\" \"\${code:-000}\" \"\$(tr '\n\r' '  ' < /tmp/canary.err 2>/dev/null | head -c 80)\" \"\$(head -c 160 /tmp/canary.body 2>/dev/null | tr '\n\r' '  ')\" >> /tmp/canary_fail_details
  fi
  sleep 2
done
EOF
chmod +x /tmp/canary.sh
nohup /tmp/canary.sh >/tmp/canary.log 2>&1 </dev/null &
echo \$! > /tmp/canary.pid
"

# Baseline stay at 1P:2D for 15s.
sleep 15
sample_pod_restarts || true
bp="$(role_spec_replicas prefill)"
bd="$(role_spec_replicas decode)"
if [ "$bp" = "1" ] && [ "$bd" = "2" ]; then
  record PASS "baseline stays at 1P:2D" "prefill=${bp} decode=${bd}"
else
  record FAIL "baseline stays at 1P:2D" "expected 1:2, got prefill=${bp} decode=${bd}"
fi

# Start heavy burst load (16 concurrent workers sending ~150-token prompt + max_tokens=128).
LONG_TEXT="Explain the mathematical foundations of queuing theory, queueing stability, M/M/c queues, prefill and decode disaggregation in large language model serving, KV cache transfer bottlenecks, token throughput planning, latency Service Level Objectives for time to first token and inter-token latency, and whole-unit capacity scaling in detail."
BURST_B64="$(jq -nc --arg m "$SERVED_MODEL" --arg c "$LONG_TEXT" '{model:$m, messages:[{role:"user",content:$c}], max_tokens:128, temperature:0}' | base64 | tr -d '\n\r')"
kubectl -n "$NS" exec "$PROBE_POD" -- sh -c "
printf '%s' '${BURST_B64}' | base64 -d > /tmp/burst.json
test -s /tmp/burst.json
cat > /tmp/burst.sh <<'EOF'
#!/bin/sh
: > /tmp/burst_ok_lines
: > /tmp/burst_fail_codes
: > /tmp/burst_fail_details
rm -f /tmp/stop_burst
for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16; do
  (
    while [ ! -f /tmp/stop_burst ]; do
      rc=0
      code=\$(curl -sS -o \"/tmp/burst.\${i}.body\" -w '%{http_code}' --max-time 120 -H 'Content-Type: application/json' -d @/tmp/burst.json '${ROUTER_URL}/v1/chat/completions' 2>\"/tmp/burst.\${i}.err\") || rc=\$?
      if [ \"\$rc\" -eq 0 ] && [ \"\$code\" = \"200\" ] && grep -Fq '\"choices\"' \"/tmp/burst.\${i}.body\" 2>/dev/null; then
        echo 1 >> /tmp/burst_ok_lines
      else
        echo \"rc=\${rc}:\${code:-000}\" >> /tmp/burst_fail_codes
        printf '%s worker=%s rc=%s code=%s err=%s body=%s\n' \"\$(date +%H:%M:%S)\" \"\$i\" \"\$rc\" \"\${code:-000}\" \"\$(tr '\n\r' '  ' < \"/tmp/burst.\${i}.err\" 2>/dev/null | head -c 80)\" \"\$(head -c 160 \"/tmp/burst.\${i}.body\" 2>/dev/null | tr '\n\r' '  ')\" >> /tmp/burst_fail_details
      fi
    done
  ) &
done
wait
EOF
chmod +x /tmp/burst.sh
nohup /tmp/burst.sh >/tmp/burst.log 2>&1 </dev/null &
echo \$! > /tmp/burst.pid
"

# Wait for atomic whole-unit scale-up 1P:2D -> 2P:4D (read both roles from one snapshot).
deadline=$((SECONDS + SCALE_BOUND))
scaled_up=false
ratio_broken=false
while [ "$SECONDS" -lt "$deadline" ]; do
  sample_pod_restarts || true
  md_json="$(kubectl -n "$NS" get "$MD_RES" "$DEPLOY" -o json 2>/dev/null || true)"
  sp="$(printf '%s' "$md_json" | jq -r '[(.spec.roles // [])[] | select(.name=="prefill") | .replicas] | .[0] // 0' 2>/dev/null || echo 0)"
  sd="$(printf '%s' "$md_json" | jq -r '[(.spec.roles // [])[] | select(.name=="decode") | .replicas] | .[0] // 0' 2>/dev/null || echo 0)"
  rp="$(printf '%s' "$md_json" | jq -r '[(.status.roles // [])[] | select(.name=="prefill") | .ready] | .[0] // 0' 2>/dev/null || echo 0)"
  rd="$(printf '%s' "$md_json" | jq -r '[(.status.roles // [])[] | select(.name=="decode") | .ready] | .[0] // 0' 2>/dev/null || echo 0)"
  if [ $((sp * 2)) -ne "$sd" ]; then
    ratio_broken=true
    record FAIL "atomic 1:2 ratio invariant during scale-up" "observed prefill=${sp} decode=${sd}"
    break
  fi
  if [ "$sp" = "2" ] && [ "$sd" = "4" ] && [ "$rp" = "2" ] && [ "$rd" = "4" ]; then
    scaled_up=true
    break
  fi
  sleep 5
done

# Stop heavy burst now (canary keeps running).
kubectl -n "$NS" exec "$PROBE_POD" -- touch /tmp/stop_burst 2>/dev/null || true

if [ "$ratio_broken" = false ]; then
  if [ "$scaled_up" = true ]; then
    record PASS "atomic whole-unit scale-up 1P:2D -> 2P:4D" "spec=2:4 ready=2:4"
  else
    record FAIL "atomic whole-unit scale-up 1P:2D -> 2P:4D" "timeout (${SCALE_BOUND}s), spec=${sp:-0}:${sd:-0} ready=${rp:-0}:${rd:-0}"
  fi
fi

# Wait for atomic whole-unit scale-down + retirement 2P:4D -> 1P:2D.
deadline=$((SECONDS + RETIRE_BOUND))
scaled_down=false
saw_retirement=false
while [ "$SECONDS" -lt "$deadline" ]; do
  sample_pod_restarts || true
  md_json="$(kubectl -n "$NS" get "$MD_RES" "$DEPLOY" -o json 2>/dev/null || true)"
  sp="$(printf '%s' "$md_json" | jq -r '[(.spec.roles // [])[] | select(.name=="prefill") | .replicas] | .[0] // 0' 2>/dev/null || echo 0)"
  sd="$(printf '%s' "$md_json" | jq -r '[(.spec.roles // [])[] | select(.name=="decode") | .replicas] | .[0] // 0' 2>/dev/null || echo 0)"
  rp="$(printf '%s' "$md_json" | jq -r '[(.status.roles // [])[] | select(.name=="prefill") | .ready] | .[0] // 0' 2>/dev/null || echo 0)"
  rd="$(printf '%s' "$md_json" | jq -r '[(.status.roles // [])[] | select(.name=="decode") | .ready] | .[0] // 0' 2>/dev/null || echo 0)"
  ret_state="$(printf '%s' "$md_json" | jq -r '.status.retirement.state // ""' 2>/dev/null || true)"
  [ -z "$ret_state" ] || saw_retirement=true
  if [ $((sp * 2)) -ne "$sd" ]; then
    ratio_broken=true
    record FAIL "atomic 1:2 ratio invariant during scale-down" "observed prefill=${sp} decode=${sd}"
    break
  fi
  if [ "$sp" = "1" ] && [ "$sd" = "2" ] && [ "$rp" = "1" ] && [ "$rd" = "2" ]; then
    # Also verify no surplus serving pods remain before declaring scale-down complete.
    pod_cnt="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${DEPLOY},app.kubernetes.io/component in (prefill,decode)" --no-headers 2>/dev/null | wc -l | tr -d ' ')"
    if [ "${pod_cnt:-0}" -le 3 ]; then
      scaled_down=true
      break
    fi
  fi
  sleep 5
done

if [ "$ratio_broken" = false ]; then
  if [ "$scaled_down" = true ]; then
    record PASS "atomic whole-unit scale-down 2P:4D -> 1P:2D" "spec=1:2 ready=1:2 retirement_observed=${saw_retirement}"
  else
    ret_reason="$(printf '%s' "$md_json" | jq -r '.status.retirement.reason // ""' 2>/dev/null || true)"
    record FAIL "atomic whole-unit scale-down 2P:4D -> 1P:2D" "timeout (${RETIRE_BOUND}s), spec=${sp:-0}:${sd:-0} ready=${rp:-0}:${rd:-0} ret_state=${ret_state:-none} ret_reason=${ret_reason}"
  fi
fi

# Stop canary, wait for both probe workers to drain in-flight requests, and verify zero failed requests.
kubectl -n "$NS" exec "$PROBE_POD" -- touch /tmp/stop_canary 2>/dev/null || true
burst_drained=true
canary_drained=true
wait_probe_worker /tmp/burst.pid 130 || burst_drained=false
wait_probe_worker /tmp/canary.pid 70 || canary_drained=false

probe_counts="$(kubectl -n "$NS" exec "$PROBE_POD" -- sh -c '
  awk "END{print NR}" /tmp/canary_ok_lines &&
  awk "END{print NR}" /tmp/canary_fail_codes &&
  awk "END{print NR}" /tmp/burst_ok_lines &&
  awk "END{print NR}" /tmp/burst_fail_codes
' 2>/dev/null | tr '\n' ' ' | tr -s ' ' | sed 's/^ //;s/ $//' || true)"
read -r CANARY_OK CANARY_FAIL BURST_OK BURST_FAIL <<< "${probe_counts:-}"

if [ "$burst_drained" = true ] && [ "$canary_drained" = true ] && \
   [ -n "${CANARY_OK:-}" ] && [ "${CANARY_OK:-0}" -gt 0 ] && [ "${CANARY_FAIL:-1}" -eq 0 ] && \
   [ -n "${BURST_OK:-}" ] && [ "${BURST_OK:-0}" -gt 0 ] && [ "${BURST_FAIL:-1}" -eq 0 ]; then
  record PASS "continuous router probe zero failed requests" "canary_ok=${CANARY_OK}, canary_fail=0, burst_ok=${BURST_OK}, burst_fail=0"
else
  canary_codes="$(kubectl -n "$NS" exec "$PROBE_POD" -- sh -c 'sort /tmp/canary_fail_codes 2>/dev/null | uniq -c | tr "\n" ";" ' 2>/dev/null || true)"
  burst_codes="$(kubectl -n "$NS" exec "$PROBE_POD" -- sh -c 'sort /tmp/burst_fail_codes 2>/dev/null | uniq -c | tr "\n" ";" ' 2>/dev/null || true)"
  canary_sample="$(kubectl -n "$NS" exec "$PROBE_POD" -- sh -c 'head -n 3 /tmp/canary_fail_details 2>/dev/null | tr "\n" "|"' 2>/dev/null || true)"
  burst_sample="$(kubectl -n "$NS" exec "$PROBE_POD" -- sh -c 'head -n 3 /tmp/burst_fail_details 2>/dev/null | tr "\n" "|"' 2>/dev/null || true)"
  echo "[case-132-diag] canary failures:" >&2
  kubectl -n "$NS" exec "$PROBE_POD" -- sh -c 'head -n 20 /tmp/canary_fail_details 2>/dev/null' >&2 || true
  echo "[case-132-diag] burst failures (first 20):" >&2
  kubectl -n "$NS" exec "$PROBE_POD" -- sh -c 'head -n 20 /tmp/burst_fail_details 2>/dev/null' >&2 || true
  echo "[case-132-diag] burst failure timestamps (first/last 5):" >&2
  kubectl -n "$NS" exec "$PROBE_POD" -- sh -c 'head -n 5 /tmp/burst_fail_details; echo "---"; tail -n 5 /tmp/burst_fail_details' >&2 || true
  echo "[case-132-diag] router epp logs (tail 30):" >&2
  kubectl -n "$NS" logs "deploy/${DEPLOY}-router" -c epp --tail=30 >&2 2>/dev/null || true
  record FAIL "continuous router probe zero failed requests" "drained=${canary_drained}/${burst_drained}, canary_ok=${CANARY_OK:-?}, canary_fail=${CANARY_FAIL:-?} (${canary_codes} ${canary_sample}), burst_ok=${BURST_OK:-?}, burst_fail=${BURST_FAIL:-?} (${burst_codes} ${burst_sample})"
fi

# Verify 0 container and native-sidecar restarts across all deployment Pods (including retired Pods).
sample_pod_restarts || true
PODS_SEEN="$(jq 'length' "$RESTART_LEDGER" 2>/dev/null || echo 0)"
RESTARTS="$(jq '[.[]] | add // 0' "$RESTART_LEDGER" 2>/dev/null || echo -1)"
if [ -z "$RESTART_READ_ERR" ] && [ "${PODS_SEEN:-0}" -ge 4 ] && [ "${RESTARTS:--1}" -eq 0 ]; then
  record PASS "zero container restarts across serving and router Pods" "restarts=0 pods_observed=${PODS_SEEN}"
else
  record FAIL "zero container restarts across serving and router Pods" "restarts=${RESTARTS} pods_observed=${PODS_SEEN} read_err=${RESTART_READ_ERR:-none}"
fi

results
