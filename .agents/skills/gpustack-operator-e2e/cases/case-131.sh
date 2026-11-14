#!/usr/bin/env bash
# CASE 131 — ModelDeploymentScaler P/D and EPD capacityPrior control-plane contracts (MUTATING, self-recovering)
#   case-131.sh <NS>
# Prerequisites: nodes=1; gpu=ascend|nvidia|amd; cluster=any; inputs=E2E_MD_IMAGE
# Goal:        Verify webhook defaulting, SLO default suppression, ConfigMap priors.json fallback,
#              fail-closed CREATE refusals, Priority 1 disaggregated whole-unit resolution,
#              Priority 2 aggregated fallback (including embedded priors on H100), EPD
#              encode coupling with pdScale = gcd(u_P, u_D), and UPDATE warning + controller
#              freeze semantics for disaggregated capacityPrior scalers.
# Environment: Cluster carrying ModelDeploymentScaler and ModelDeployment CRDs plus a usable
#              AcceleratorType reporting an admitted platform (nvidia, amd, or ascend). Target
#              Deployments use placeholder vLLM image references (E2E_MD_IMAGE, default
#              vllm/pause:3.10) that are never pulled or served.
# Inputs:      <NS>            Namespace for test ConfigMaps, ModelDeployments, and Scalers.
#              E2E_MD_IMAGE    Placeholder vLLM container image (default: vllm/pause:3.10).
# Expected:    All webhook defaulting, refusal, Priority 1, Priority 2, EPD pdScale, priors.json
#              fallback, and UPDATE broken-ConfigMap rows PASS (embedded H100 Priority 2 row
#              PASSes on H100 nodes or SKIPs cleanly on other hardware).
# Cleanup:     EXIT trap deletes only the Scalers, ModelDeployments, and ConfigMaps created by this run.
set -uo pipefail

NS="${1:?usage: case-131.sh <namespace>}"
export CASE_ID=131
CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "${CASE_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_md-lib.sh"

SCALER_CRD="modeldeploymentscalers.worker.gpustack.ai"
MD_CRD="modeldeployments.worker.gpustack.ai"
SCALER_RES="modeldeploymentscalers.v1alpha1.worker.gpustack.ai"
MD_RES="modeldeployments.v1alpha1.worker.gpustack.ai"
PAUSE_IMAGE="${E2E_MD_IMAGE:-vllm/pause:3.10}"

if ! kubectl get crd "$SCALER_CRD" >/dev/null 2>&1 || ! kubectl get crd "$MD_CRD" >/dev/null 2>&1; then
  record SKIP "case-131 prerequisites" "missing ${SCALER_CRD} or ${MD_CRD}"
  print_rows
  exit 0
fi

AT="$(md_usable_accelerator_type "$NS" || true)"
if [ -z "$AT" ]; then
  record SKIP "case-131 prerequisites" "no usable AcceleratorType available in cluster"
  print_rows
  exit 0
fi

HW_PRODUCT="$(kubectl get acceleratortype "$AT" -o jsonpath='{.status.detail.product}' 2>/dev/null || true)"
HW_MFG="$(kubectl get acceleratortype "$AT" -o jsonpath='{.status.detail.manufacturer}' 2>/dev/null || true)"
if [ -z "$HW_PRODUCT" ]; then
  record SKIP "case-131 prerequisites" "AcceleratorType ${AT} reports empty status.detail.product"
  print_rows
  exit 0
fi
HW_MFG_LC="$(printf '%s' "$HW_MFG" | tr '[:upper:]' '[:lower:]')"
HW_PRODUCT_LC="$(printf '%s' "$HW_PRODUCT" | tr '[:upper:]' '[:lower:]')"
case "$HW_MFG_LC" in
  nvidia|amd|ascend) ;;
  *)
    record SKIP "case-131 prerequisites" "AcceleratorType ${AT} manufacturer ${HW_MFG:-unknown} not in nvidia/amd/ascend"
    print_rows
    exit 0
    ;;
esac

RUN_PREFIX="c131-$$"
CM_DISAGG="${RUN_PREFIX}-cm-disagg"
CM_JSON="${RUN_PREFIX}-cm-json"
CM_BOTH="${RUN_PREFIX}-cm-both-strict"
CM_AGG="${RUN_PREFIX}-cm-agg"
MD_PD="${RUN_PREFIX}-pd"
MD_PD_KV="${RUN_PREFIX}-pd-kv"
MD_H100="${RUN_PREFIX}-h100-70b"
MD_EPD="${RUN_PREFIX}-epd"
SC_P1="${RUN_PREFIX}-sc-p1"
SC_JSON="${RUN_PREFIX}-sc-json"
SC_P2_CM="${RUN_PREFIX}-sc-p2-cm"
SC_P2_EMB="${RUN_PREFIX}-sc-p2-embedded"
SC_EPD="${RUN_PREFIX}-sc-epd"

CREATED_SCALERS=()
CREATED_MDS=()
CREATED_CMS=()
TMP_DIR="$(mktemp -d)"
CLEANED=false
# shellcheck disable=SC2317 # Invoked by EXIT/INT/TERM trap.
cleanup() {
  if [ "$CLEANED" = true ]; then
    return 0
  fi
  CLEANED=true
  local name
  for name in ${CREATED_SCALERS[@]+"${CREATED_SCALERS[@]}"}; do
    kubectl -n "$NS" delete "$SCALER_RES" "$name" --ignore-not-found --wait=true --timeout=60s >/dev/null 2>&1 || true
  done
  for name in ${CREATED_MDS[@]+"${CREATED_MDS[@]}"}; do
    kubectl -n "$NS" delete "$MD_RES" "$name" --ignore-not-found --wait=true --timeout=60s >/dev/null 2>&1 || true
  done
  for name in ${CREATED_CMS[@]+"${CREATED_CMS[@]}"}; do
    kubectl -n "$NS" delete configmap "$name" --ignore-not-found >/dev/null 2>&1 || true
  done
  rm -rf "$TMP_DIR"
  return 0
}
trap cleanup EXIT INT TERM

ensure_absent() {
  local res="$1" name="$2"
  if kubectl -n "$NS" get "$res" "$name" >/dev/null 2>&1; then
    record FAIL "resource collision guard" "${NS}/${res}/${name} already exists before test mutation"
    results || true
    exit 1
  fi
}

for cm in "$CM_DISAGG" "$CM_JSON" "$CM_BOTH" "$CM_AGG"; do
  ensure_absent configmap "$cm"
done
for md in "$MD_PD" "$MD_PD_KV" "$MD_H100" "$MD_EPD"; do
  ensure_absent "$MD_RES" "$md"
done
for sc in "$SC_P1" "$SC_JSON" "$SC_P2_CM" "$SC_P2_EMB" "$SC_EPD"; do
  ensure_absent "$SCALER_RES" "$sc"
done

echo "=== CASE 131: P/D and EPD capacityPrior control-plane tests (ns=${NS}, at=${AT}, product=${HW_PRODUCT}) ==="

write_pd_md() {
  local name="$1" prefill_rep="$2" decode_rep="$3" kv_pool="${4:-}"
  local out="${TMP_DIR}/${name}.yaml"
  cat > "$out" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${name}
  namespace: ${NS}
  annotations:
    worker.gpustack.ai/precision: "none"
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: vLLM
    image: ${PAUSE_IMAGE}
  router:
    name: llm-d-router
  roles:
    - name: prefill
      kind: Prefill
      replicas: ${prefill_rep}
      acceleratorType: ${AT}
      resources:
        accelerator: "1"
    - name: decode
      kind: Decode
      replicas: ${decode_rep}
      acceleratorType: ${AT}
      resources:
        accelerator: "1"
YAML
  if [ -n "$kv_pool" ]; then
    cat >> "$out" <<YAML
  kvCache:
    poolRef:
      name: ${kv_pool}
YAML
  fi
  printf '%s\n' "$out"
}

write_epd_md() {
  local name="$1"
  local out="${TMP_DIR}/${name}.yaml"
  cat > "$out" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${name}
  namespace: ${NS}
  annotations:
    worker.gpustack.ai/precision: "none"
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: vLLM
    image: ${PAUSE_IMAGE}
  router:
    name: llm-d-router
  roles:
    - name: encode
      kind: Encode
      replicas: 1
      acceleratorType: ${AT}
      resources:
        accelerator: "1"
    - name: prefill
      kind: Prefill
      replicas: 2
      acceleratorType: ${AT}
      resources:
        accelerator: "1"
    - name: decode
      kind: Decode
      replicas: 4
      acceleratorType: ${AT}
      resources:
        accelerator: "1"
YAML
  printf '%s\n' "$out"
}

write_h100_70b_md() {
  local name="$1"
  local out="${TMP_DIR}/${name}.yaml"
  cat > "$out" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${name}
  namespace: ${NS}
  annotations:
    worker.gpustack.ai/precision: "fp8"
spec:
  model:
    name: meta-llama/Llama-3.3-70B-Instruct
  engine:
    name: vLLM
    image: ${PAUSE_IMAGE}
  router:
    name: llm-d-router
  roles:
    - name: prefill
      kind: Prefill
      replicas: 1
      acceleratorType: ${AT}
      resources:
        accelerator: "2"
      extraArgs:
        - --tensor-parallel-size
        - "2"
    - name: decode
      kind: Decode
      replicas: 1
      acceleratorType: ${AT}
      resources:
        accelerator: "2"
      extraArgs:
        - --tensor-parallel-size
        - "2"
YAML
  printf '%s\n' "$out"
}

expect_dryrun_refusal() {
  local label="$1" file="$2" needle="$3" out rc=0
  out="$(kubectl create --dry-run=server -f "$file" 2>&1)" || rc=$?
  if [ "$rc" -eq 0 ]; then
    record FAIL "$label" "expected CREATE refusal matching '${needle}', but admission succeeded"
    return 1
  fi
  if printf '%s' "$out" | grep -Fq -- "$needle"; then
    record PASS "$label" "refused as expected (${needle})"
  else
    record FAIL "$label" "refusal did not contain '${needle}': ${out}"
  fi
}

wait_scaler_cond() {
  local name="$1" cond="$2" want="$3" deadline=$((SECONDS + 60)) got=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    got="$(kubectl -n "$NS" get "$SCALER_RES" "$name" -o json 2>/dev/null | \
      jq -r --arg c "$cond" '[(.status.conditions // [])[] | select(.type == $c) | .status] | .[0] // ""' 2>/dev/null || true)"
    if [ "$got" = "$want" ]; then
      return 0
    fi
    sleep 2
  done
  return 1
}

qty_eq() {
  local raw="$1" want="$2"
  python3 - "$raw" "$want" <<'PY'
import sys
def parse_qty(s):
    s = s.strip()
    mult = {"k": 1e3, "K": 1e3, "M": 1e6, "G": 1e9, "m": 1e-3}
    if s and s[-1] in mult:
        return float(s[:-1]) * mult[s[-1]]
    return float(s)
got = parse_qty(sys.argv[1])
want = float(sys.argv[2])
sys.exit(0 if abs(got - want) < 1e-6 else 1)
PY
}

# --- Build catalog ConfigMaps ---

CREATED_CMS+=("$CM_DISAGG" "$CM_JSON" "$CM_BOTH" "$CM_AGG")
kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${CM_DISAGG}
data:
  priors.yaml: |
    apiVersion: worker.gpustack.ai/v1alpha1
    kind: ModelDeploymentScalerCapacityPriorCatalog
    profiles:
      - mode: disaggregated
        model: Qwen/Qwen3-8B
        quantization: none
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
            totalTokensPerSecond: 500
            inputTokensPerSecond: 400
            outputTokensPerSecond: 100
            ttftMilliseconds:
              p99: 300
            itlMilliseconds:
              p99: 20
          - concurrency: 2
            totalTokensPerSecond: 1000
            inputTokensPerSecond: 800
            outputTokensPerSecond: 200
            ttftMilliseconds:
              p99: 400
            itlMilliseconds:
              p99: 30
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${CM_JSON}
data:
  priors.json: |
    {
      "apiVersion": "worker.gpustack.ai/v1alpha1",
      "kind": "ModelDeploymentScalerCapacityPriorCatalog",
      "profiles": [
        {
          "mode": "disaggregated",
          "model": "Qwen/Qwen3-8B",
          "quantization": "none",
          "engine": "vllm",
          "prefill": {"replicas": 1, "hardware": "${HW_PRODUCT}", "topology": {"gpusPerReplica": 1, "tensorParallel": 1, "expertParallel": 1}},
          "decode": {"replicas": 2, "hardware": "${HW_PRODUCT}", "topology": {"gpusPerReplica": 1, "tensorParallel": 1, "expertParallel": 1}},
          "points": [
            {
              "concurrency": 1,
              "totalTokensPerSecond": 500,
              "inputTokensPerSecond": 400,
              "outputTokensPerSecond": 100,
              "ttftMilliseconds": {"p99": 300},
              "itlMilliseconds": {"p99": 20}
            },
            {
              "concurrency": 2,
              "totalTokensPerSecond": 1000,
              "inputTokensPerSecond": 800,
              "outputTokensPerSecond": 200,
              "ttftMilliseconds": {"p99": 400},
              "itlMilliseconds": {"p99": 30}
            }
          ]
        }
      ]
    }
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${CM_BOTH}
data:
  priors.yaml: |
    apiVersion: worker.gpustack.ai/v1alpha1
    kind: ModelDeploymentScalerCapacityPriorCatalog
    profiles:
      - mode: disaggregated
        model: Qwen/Qwen3-8B
        quantization: none
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
            totalTokensPerSecond: 500
            inputTokensPerSecond: 400
            outputTokensPerSecond: 100
            ttftMilliseconds:
              p90: 1500
              p99: 1500
            itlMilliseconds:
              p90: 80
              p99: 80
          - concurrency: 2
            totalTokensPerSecond: 1000
            inputTokensPerSecond: 800
            outputTokensPerSecond: 200
            ttftMilliseconds:
              p90: 2000
              p99: 2000
            itlMilliseconds:
              p90: 100
              p99: 100
      - mode: aggregated
        model: Qwen/Qwen3-8B
        quantization: none
        engine: vllm
        hardware: ${HW_PRODUCT}
        topology:
          gpusPerReplica: 1
          tensorParallel: 1
          expertParallel: 1
        points:
          - concurrency: 1
            totalTokensPerSecond: 2500
            inputTokensPerSecond: 2000
            outputTokensPerSecond: 500
            ttftMilliseconds:
              p99: 80
            itlMilliseconds:
              p99: 8
          - concurrency: 2
            totalTokensPerSecond: 5000
            inputTokensPerSecond: 4000
            outputTokensPerSecond: 1000
            ttftMilliseconds:
              p99: 100
            itlMilliseconds:
              p99: 10
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${CM_AGG}
data:
  priors.yaml: |
    apiVersion: worker.gpustack.ai/v1alpha1
    kind: ModelDeploymentScalerCapacityPriorCatalog
    profiles:
      - mode: aggregated
        model: Qwen/Qwen3-8B
        quantization: none
        engine: vllm
        hardware: ${HW_PRODUCT}
        topology:
          gpusPerReplica: 1
          tensorParallel: 1
          expertParallel: 1
        points:
          - concurrency: 1
            totalTokensPerSecond: 1000
            inputTokensPerSecond: 800
            outputTokensPerSecond: 200
            ttftMilliseconds:
              p99: 80
            itlMilliseconds:
              p99: 8
          - concurrency: 2
            totalTokensPerSecond: 2000
            inputTokensPerSecond: 1600
            outputTokensPerSecond: 400
            ttftMilliseconds:
              p99: 100
            itlMilliseconds:
              p99: 10
YAML

# Create primary P/D target deployment.
CREATED_MDS+=("$MD_PD")
kubectl apply -f "$(write_pd_md "$MD_PD" 1 2)" >/dev/null

# --- (a) Webhook defaulting, SLO default suppression, priors.json fallback ---

cat > "${TMP_DIR}/sc-default.yaml" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${SC_P1}
  namespace: ${NS}
spec:
  targetRef:
    name: ${MD_PD}
  mode: Active
  roles:
    - name: prefill
      minReplicas: 1
      maxReplicas: 3
    - name: decode
      minReplicas: 2
      maxReplicas: 6
  ratios:
    - numeratorRole: decode
      denominatorRole: prefill
      target: "2"
      min: "2"
      max: "2"
  algorithm:
    type: TokenThroughput
    tokenThroughput:
      ttftTargetMilliseconds: "500"
      itlTargetMilliseconds: "50"
    capacityPrior:
      catalogRef:
        name: ${CM_DISAGG}
YAML

CREATED_SCALERS+=("$SC_P1")
if kubectl apply -f "${TMP_DIR}/sc-default.yaml" >/dev/null 2>&1; then
  sc_json="$(kubectl -n "$NS" get "$SCALER_RES" "$SC_P1" -o json)"
  def_headroom="$(printf '%s' "$sc_json" | jq -r '.spec.algorithm.capacityPrior.headroom // ""')"
  def_key="$(printf '%s' "$sc_json" | jq -r '.spec.algorithm.capacityPrior.catalogRef.key // ""')"
  if [ -n "$def_headroom" ] && qty_eq "$def_headroom" 0.75 && [ "$def_key" = "priors.yaml" ]; then
    record PASS "webhook defaulting (headroom & catalogRef.key)" "headroom=${def_headroom}, catalogRef.key=priors.yaml"
  else
    record FAIL "webhook defaulting (headroom & catalogRef.key)" "got headroom=${def_headroom}, key=${def_key}"
  fi
else
  record FAIL "webhook defaulting (headroom & catalogRef.key)" "failed to apply ${SC_P1}"
fi

# Test capacityPrior: {} defaults headroom=0.75 even with catalogRef omitted (via server dry-run).
cat > "${TMP_DIR}/sc-empty-prior.yaml" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${RUN_PREFIX}-sc-empty-prior
  namespace: ${NS}
spec:
  targetRef:
    name: ${MD_PD}
  mode: Active
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
  algorithm:
    type: TokenThroughput
    tokenThroughput:
      ttftTargetMilliseconds: "500"
      itlTargetMilliseconds: "50"
      prefillTargetTokensPerSecond: "100"
      decodeTargetTokensPerSecond: "200"
    capacityPrior: {}
YAML
empty_prior_out="$(kubectl create --dry-run=server -f "${TMP_DIR}/sc-empty-prior.yaml" -o json 2>&1 || true)"
empty_prior_hr="$(printf '%s' "$empty_prior_out" | jq -r '.spec.algorithm.capacityPrior.headroom // ""' 2>/dev/null || true)"
if [ -n "$empty_prior_hr" ] && qty_eq "$empty_prior_hr" 0.75; then
  record PASS "webhook defaulting (capacityPrior: {} -> headroom=0.75)" "headroom=${empty_prior_hr}"
else
  record FAIL "webhook defaulting (capacityPrior: {} -> headroom=0.75)" "got headroom=${empty_prior_hr} out=${empty_prior_out}"
fi

# --- (c) Priority 1 disaggregated whole-unit resolution ---

if wait_scaler_cond "$SC_P1" CapacityPriorsReady True; then
  sc_json="$(kubectl -n "$NS" get "$SCALER_RES" "$SC_P1" -o json)"
  p1_msg="$(printf '%s' "$sc_json" | jq -r '[(.status.conditions // [])[] | select(.type=="CapacityPriorsReady") | .message] | .[0] // ""')"
  fp_p="$(printf '%s' "$sc_json" | jq -r '[(.status.resolvedCapacityPriors // [])[] | select(.name=="prefill") | .compatibilityFingerprint] | .[0] // ""')"
  fp_d="$(printf '%s' "$sc_json" | jq -r '[(.status.resolvedCapacityPriors // [])[] | select(.name=="decode") | .compatibilityFingerprint] | .[0] // ""')"
  fld_p="$(printf '%s' "$sc_json" | jq -r '[(.status.resolvedCapacityPriors // [])[] | select(.name=="prefill") | .fields[]? | "\(.field)=\(.source)=\(.effectiveTokensPerSecond)"] | .[0] // ""')"
  fld_d="$(printf '%s' "$sc_json" | jq -r '[(.status.resolvedCapacityPriors // [])[] | select(.name=="decode") | .fields[]? | "\(.field)=\(.source)=\(.effectiveTokensPerSecond)"] | .[0] // ""')"
  if printf '%s' "$p1_msg" | grep -Fq "all required capacities are resolved via catalog disaggregated whole-unit priors" && \
     [ -n "$fp_p" ] && [ "$fp_p" = "$fp_d" ] && \
     printf '%s' "$fld_p" | grep -Fq "tokenThroughput.unitTargetTokensPerSecond=Catalog=" && \
     printf '%s' "$fld_d" | grep -Fq "tokenThroughput.unitTargetTokensPerSecond=Catalog="; then
    record PASS "Priority 1 disaggregated whole-unit resolution" "fp=${fp_p:0:12}..., msg ok"
  else
    record FAIL "Priority 1 disaggregated whole-unit resolution" "msg='${p1_msg}', fp_p='${fp_p}', fp_d='${fp_d}', fld_p='${fld_p}', fld_d='${fld_d}'"
  fi
else
  record FAIL "Priority 1 disaggregated whole-unit resolution" "CapacityPriorsReady did not become True"
fi

# --- (b) Fail-closed CREATE refusals & SLO default suppression ---

cat > "${TMP_DIR}/refuse-prop.yaml" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${RUN_PREFIX}-refuse-prop
  namespace: ${NS}
spec:
  targetRef:
    name: ${MD_PD}
  mode: Active
  roles:
    - name: prefill
      minReplicas: 1
      maxReplicas: 2
    - name: decode
      minReplicas: 2
      maxReplicas: 4
  algorithm:
    type: Proportional
    proportional:
      metric: RunningRequests
      roleTargets:
        - name: prefill
          target: "4"
        - name: decode
          target: "4"
    capacityPrior: {}
YAML
expect_dryrun_refusal "CREATE refusal: Proportional + capacityPrior" "${TMP_DIR}/refuse-prop.yaml" \
  "this block is for the TokenThroughput and Predictive algorithms, and this scaler runs Proportional"

cat > "${TMP_DIR}/refuse-nottft.yaml" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${RUN_PREFIX}-refuse-nottft
  namespace: ${NS}
spec:
  targetRef:
    name: ${MD_PD}
  mode: Active
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
  algorithm:
    type: TokenThroughput
    tokenThroughput:
      itlTargetMilliseconds: "50"
    capacityPrior:
      catalogRef:
        name: ${CM_DISAGG}
YAML
expect_dryrun_refusal "CREATE refusal + suppressed SLO default: missing ttftTargetMilliseconds" "${TMP_DIR}/refuse-nottft.yaml" \
  "requires an explicit ttftTargetMilliseconds"

cat > "${TMP_DIR}/refuse-noitl.yaml" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${RUN_PREFIX}-refuse-noitl
  namespace: ${NS}
spec:
  targetRef:
    name: ${MD_PD}
  mode: Active
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
  algorithm:
    type: TokenThroughput
    tokenThroughput:
      ttftTargetMilliseconds: "500"
    capacityPrior:
      catalogRef:
        name: ${CM_DISAGG}
YAML
expect_dryrun_refusal "CREATE refusal + suppressed SLO default: missing itlTargetMilliseconds" "${TMP_DIR}/refuse-noitl.yaml" \
  "requires an explicit itlTargetMilliseconds"

cat > "${TMP_DIR}/refuse-noratios.yaml" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${RUN_PREFIX}-refuse-noratios
  namespace: ${NS}
spec:
  targetRef:
    name: ${MD_PD}
  mode: Active
  roles:
    - name: prefill
      minReplicas: 1
      maxReplicas: 2
    - name: decode
      minReplicas: 2
      maxReplicas: 4
  algorithm:
    type: TokenThroughput
    tokenThroughput:
      ttftTargetMilliseconds: "500"
      itlTargetMilliseconds: "50"
    capacityPrior:
      catalogRef:
        name: ${CM_DISAGG}
YAML
expect_dryrun_refusal "CREATE refusal: missing ratios on P/D" "${TMP_DIR}/refuse-noratios.yaml" \
  "fixed ratios covering all target roles"

cat > "${TMP_DIR}/refuse-nonfixed.yaml" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${RUN_PREFIX}-refuse-nonfixed
  namespace: ${NS}
spec:
  targetRef:
    name: ${MD_PD}
  mode: Active
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
      min: "1"
      max: "2"
  algorithm:
    type: TokenThroughput
    tokenThroughput:
      ttftTargetMilliseconds: "500"
      itlTargetMilliseconds: "50"
    capacityPrior:
      catalogRef:
        name: ${CM_DISAGG}
YAML
expect_dryrun_refusal "CREATE refusal: non-fixed ratio min != max" "${TMP_DIR}/refuse-nonfixed.yaml" \
  "requires fixed ratios (min == max)"

cat > "${TMP_DIR}/refuse-partial.yaml" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${RUN_PREFIX}-refuse-partial
  namespace: ${NS}
spec:
  targetRef:
    name: ${MD_PD}
  mode: Active
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
  algorithm:
    type: TokenThroughput
    tokenThroughput:
      ttftTargetMilliseconds: "500"
      itlTargetMilliseconds: "50"
      prefillTargetTokensPerSecond: "500"
    capacityPrior:
      catalogRef:
        name: ${CM_DISAGG}
YAML
expect_dryrun_refusal "CREATE refusal: partial explicit capacity on P/D" "${TMP_DIR}/refuse-partial.yaml" \
  "disaggregated whole-unit prior resolution does not permit partial explicit capacity field"

# KVCache poolRef set when catalog lacks servingConfig.offload: external.
CREATED_MDS+=("$MD_PD_KV")
kubectl apply -f "$(write_pd_md "$MD_PD_KV" 1 2 "${RUN_PREFIX}-missing-pool")" >/dev/null
cat > "${TMP_DIR}/refuse-kv.yaml" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${RUN_PREFIX}-refuse-kv
  namespace: ${NS}
spec:
  targetRef:
    name: ${MD_PD_KV}
  mode: Active
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
  algorithm:
    type: TokenThroughput
    tokenThroughput:
      ttftTargetMilliseconds: "500"
      itlTargetMilliseconds: "50"
    capacityPrior:
      catalogRef:
        name: ${CM_DISAGG}
YAML
expect_dryrun_refusal "CREATE refusal: kvCache offload mismatch" "${TMP_DIR}/refuse-kv.yaml" \
  "servingConfig.offload"
kubectl -n "$NS" delete "$MD_RES" "$MD_PD_KV" --ignore-not-found --wait=true >/dev/null

# Priority 1 SLO violation must NOT fall back to Priority 2 aggregated profile.
cat > "${TMP_DIR}/refuse-noslo-fallback.yaml" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${RUN_PREFIX}-refuse-noslo-fallback
  namespace: ${NS}
spec:
  targetRef:
    name: ${MD_PD}
  mode: Active
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
  algorithm:
    type: TokenThroughput
    tokenThroughput:
      ttftTargetMilliseconds: "500"
      itlTargetMilliseconds: "50"
    capacityPrior:
      catalogRef:
        name: ${CM_BOTH}
YAML
expect_dryrun_refusal "CREATE refusal: Priority 1 SLO violation does not fall back to Priority 2" \
  "${TMP_DIR}/refuse-noslo-fallback.yaml" \
  "no measured load point that satisfies the latency targets"

kubectl -n "$NS" delete "$SCALER_RES" "$SC_P1" --ignore-not-found --wait=true >/dev/null

# Test priors.json ConfigMap fallback once MD_PD is unbound.
cat > "${TMP_DIR}/sc-json.yaml" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${SC_JSON}
  namespace: ${NS}
spec:
  targetRef:
    name: ${MD_PD}
  mode: Shadow
  roles:
    - name: prefill
      minReplicas: 1
      maxReplicas: 3
    - name: decode
      minReplicas: 2
      maxReplicas: 6
  ratios:
    - numeratorRole: decode
      denominatorRole: prefill
      target: "2"
      min: "2"
      max: "2"
  algorithm:
    type: TokenThroughput
    tokenThroughput:
      ttftTargetMilliseconds: "500"
      itlTargetMilliseconds: "50"
    capacityPrior:
      catalogRef:
        name: ${CM_JSON}
YAML
CREATED_SCALERS+=("$SC_JSON")
kubectl apply -f "${TMP_DIR}/sc-json.yaml" >/dev/null
if wait_scaler_cond "$SC_JSON" CapacityPriorsReady True; then
  record PASS "ConfigMap priors.json fallback" "resolved with only priors.json present"
else
  msg="$(kubectl -n "$NS" get "$SCALER_RES" "$SC_JSON" -o json | jq -r '[(.status.conditions // [])[] | select(.type=="CapacityPriorsReady") | .message] | .[0] // ""')"
  record FAIL "ConfigMap priors.json fallback" "CapacityPriorsReady!=True (${msg})"
fi
kubectl -n "$NS" delete "$SCALER_RES" "$SC_JSON" --ignore-not-found --wait=true >/dev/null

# --- (d) Priority 2 aggregated fallback ---

cat > "${TMP_DIR}/sc-p2-cm.yaml" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${SC_P2_CM}
  namespace: ${NS}
spec:
  targetRef:
    name: ${MD_PD}
  mode: Shadow
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
  algorithm:
    type: TokenThroughput
    tokenThroughput:
      ttftTargetMilliseconds: "500"
      itlTargetMilliseconds: "50"
    capacityPrior:
      catalogRef:
        name: ${CM_AGG}
YAML
CREATED_SCALERS+=("$SC_P2_CM")
kubectl apply -f "${TMP_DIR}/sc-p2-cm.yaml" >/dev/null
if wait_scaler_cond "$SC_P2_CM" CapacityPriorsReady True; then
  msg="$(kubectl -n "$NS" get "$SCALER_RES" "$SC_P2_CM" -o json | jq -r '[(.status.conditions // [])[] | select(.type=="CapacityPriorsReady") | .message] | .[0] // ""')"
  if printf '%s' "$msg" | grep -Fq "GPU-time-balanced"; then
    record PASS "Priority 2 aggregated fallback (ConfigMap)" "GPU-time-balanced fallback resolved"
  else
    record FAIL "Priority 2 aggregated fallback (ConfigMap)" "unexpected message: ${msg}"
  fi
else
  record FAIL "Priority 2 aggregated fallback (ConfigMap)" "CapacityPriorsReady!=True"
fi
kubectl -n "$NS" delete "$SCALER_RES" "$SC_P2_CM" --ignore-not-found --wait=true >/dev/null

# Embedded priors.yaml Priority 2 fallback on nvidia-h100.
if printf '%s' "$HW_PRODUCT_LC" | grep -Fq "h100"; then
  CREATED_MDS+=("$MD_H100")
  kubectl apply -f "$(write_h100_70b_md "$MD_H100")" >/dev/null
  cat > "${TMP_DIR}/sc-p2-embedded.yaml" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${SC_P2_EMB}
  namespace: ${NS}
spec:
  targetRef:
    name: ${MD_H100}
  mode: Shadow
  roles:
    - name: prefill
      minReplicas: 1
      maxReplicas: 2
    - name: decode
      minReplicas: 1
      maxReplicas: 2
  ratios:
    - numeratorRole: decode
      denominatorRole: prefill
      target: "1"
      min: "1"
      max: "1"
  algorithm:
    type: TokenThroughput
    tokenThroughput:
      ttftTargetMilliseconds: "5000"
      itlTargetMilliseconds: "30"
    capacityPrior: {}
YAML
  CREATED_SCALERS+=("$SC_P2_EMB")
  if kubectl apply -f "${TMP_DIR}/sc-p2-embedded.yaml" >/dev/null 2>&1 && wait_scaler_cond "$SC_P2_EMB" CapacityPriorsReady True; then
    msg="$(kubectl -n "$NS" get "$SCALER_RES" "$SC_P2_EMB" -o json | jq -r '[(.status.conditions // [])[] | select(.type=="CapacityPriorsReady") | .message] | .[0] // ""')"
    if printf '%s' "$msg" | grep -Fq "GPU-time-balanced"; then
      record PASS "Priority 2 aggregated fallback (embedded H100 priors)" "resolved meta-llama/Llama-3.3-70B-Instruct via GPU-time-balanced fallback"
    else
      record FAIL "Priority 2 aggregated fallback (embedded H100 priors)" "unexpected message: ${msg}"
    fi
  else
    msg="$(kubectl -n "$NS" get "$SCALER_RES" "$SC_P2_EMB" -o json 2>/dev/null | jq -r '[(.status.conditions // [])[] | select(.type=="CapacityPriorsReady") | .message] | .[0] // ""' || true)"
    record FAIL "Priority 2 aggregated fallback (embedded H100 priors)" "failed to resolve (${msg})"
  fi
  kubectl -n "$NS" delete "$SCALER_RES" "$SC_P2_EMB" --ignore-not-found --wait=true >/dev/null
  kubectl -n "$NS" delete "$MD_RES" "$MD_H100" --ignore-not-found --wait=true >/dev/null
else
  record SKIP "Priority 2 aggregated fallback (embedded H100 priors)" "cluster hardware ${HW_PRODUCT} is not H100"
fi

# --- (e) EPD (1E:2P:4D) fixed-ratio coupling & pdScale = gcd(2,4) = 2 ---

CREATED_MDS+=("$MD_EPD")
kubectl apply -f "$(write_epd_md "$MD_EPD")" >/dev/null
cat > "${TMP_DIR}/sc-epd.yaml" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${SC_EPD}
  namespace: ${NS}
spec:
  targetRef:
    name: ${MD_EPD}
  mode: Shadow
  roles:
    - name: encode
      minReplicas: 1
      maxReplicas: 2
    - name: prefill
      minReplicas: 2
      maxReplicas: 4
    - name: decode
      minReplicas: 4
      maxReplicas: 8
  ratios:
    - numeratorRole: encode
      denominatorRole: prefill
      target: "0.5"
      min: "0.5"
      max: "0.5"
    - numeratorRole: decode
      denominatorRole: prefill
      target: "2"
      min: "2"
      max: "2"
  algorithm:
    type: TokenThroughput
    tokenThroughput:
      ttftTargetMilliseconds: "500"
      itlTargetMilliseconds: "50"
    capacityPrior:
      catalogRef:
        name: ${CM_DISAGG}
YAML
CREATED_SCALERS+=("$SC_EPD")
if kubectl apply -f "${TMP_DIR}/sc-epd.yaml" >/dev/null 2>&1 && wait_scaler_cond "$SC_EPD" CapacityPriorsReady True; then
  epd_json="$(kubectl -n "$NS" get "$SCALER_RES" "$SC_EPD" -o json)"
  seed_p="$(printf '%s' "$epd_json" | jq -r '[(.status.resolvedCapacityPriors // [])[] | select(.name=="prefill") | .seedTokensPerSecond] | .[0] // ""')"
  eff_p="$(printf '%s' "$epd_json" | jq -r '[(.status.resolvedCapacityPriors // [])[] | select(.name=="prefill") | .fields[]? | .effectiveTokensPerSecond] | .[0] // ""')"
  mrep_p="$(printf '%s' "$epd_json" | jq -r '[(.status.resolvedCapacityPriors // [])[] | select(.name=="prefill") | .measuredReplicas] | .[0] // 0')"
  mrep_d="$(printf '%s' "$epd_json" | jq -r '[(.status.resolvedCapacityPriors // [])[] | select(.name=="decode") | .measuredReplicas] | .[0] // 0')"
  has_enc="$(printf '%s' "$epd_json" | jq -r '[(.status.resolvedCapacityPriors // [])[] | select(.name=="encode")] | length')"
  if qty_eq "$seed_p" 2000 && qty_eq "$eff_p" 1500 && [ "$mrep_p" = "1" ] && [ "$mrep_d" = "2" ] && [ "$has_enc" = "0" ]; then
    record PASS "EPD 1E:2P:4D coupling & pdScale=gcd(2,4)=2" "seed=${seed_p} eff=${eff_p} measured=(${mrep_p},${mrep_d})"
  else
    record FAIL "EPD 1E:2P:4D coupling & pdScale=gcd(2,4)=2" "seed=${seed_p} eff=${eff_p} measured=(${mrep_p},${mrep_d}) has_enc=${has_enc}"
  fi
else
  msg="$(kubectl -n "$NS" get "$SCALER_RES" "$SC_EPD" -o json 2>/dev/null | jq -r '[(.status.conditions // [])[] | select(.type=="CapacityPriorsReady") | .message] | .[0] // ""' || true)"
  record FAIL "EPD 1E:2P:4D coupling & pdScale=gcd(2,4)=2" "CapacityPriorsReady!=True (${msg})"
fi
kubectl -n "$NS" delete "$SCALER_RES" "$SC_EPD" --ignore-not-found --wait=true >/dev/null
kubectl -n "$NS" delete "$MD_RES" "$MD_EPD" --ignore-not-found --wait=true >/dev/null

# --- (f) UPDATE broken-ConfigMap warning + controller freeze ---

kubectl apply -f "${TMP_DIR}/sc-default.yaml" >/dev/null
wait_scaler_cond "$SC_P1" CapacityPriorsReady True || true
kubectl -n "$NS" patch configmap "$CM_DISAGG" --type=merge -p '{"data":{"priors.yaml":"{"}}' >/dev/null
cat > "${TMP_DIR}/sc-update-broken.yaml" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${SC_P1}
  namespace: ${NS}
spec:
  targetRef:
    name: ${MD_PD}
  mode: Active
  roles:
    - name: prefill
      minReplicas: 1
      maxReplicas: 3
    - name: decode
      minReplicas: 2
      maxReplicas: 6
  ratios:
    - numeratorRole: decode
      denominatorRole: prefill
      target: "2"
      min: "2"
      max: "2"
  algorithm:
    type: TokenThroughput
    tokenThroughput:
      ttftTargetMilliseconds: "500"
      itlTargetMilliseconds: "50"
    capacityPrior:
      headroom: "0.8"
      catalogRef:
        name: ${CM_DISAGG}
YAML

upd_out="$(kubectl apply -f "${TMP_DIR}/sc-update-broken.yaml" 2>&1)" || upd_rc=$?
upd_rc="${upd_rc:-0}"
if [ "$upd_rc" -eq 0 ] && printf '%s' "$upd_out" | grep -Fq "admitting update so the scaler can be repaired or placed in Shadow mode"; then
  record PASS "UPDATE broken-ConfigMap admission warning" "admitted with repair/Shadow warning"
else
  record FAIL "UPDATE broken-ConfigMap admission warning" "rc=${upd_rc}, out=${upd_out}"
fi

if wait_scaler_cond "$SC_P1" CapacityPriorsReady False && \
   wait_scaler_cond "$SC_P1" Active False && \
   wait_scaler_cond "$SC_P1" Blocked True; then
  record PASS "UPDATE broken-ConfigMap controller freeze" "CapacityPriorsReady=False Active=False Blocked=True"
else
  conds="$(kubectl -n "$NS" get "$SCALER_RES" "$SC_P1" -o json | jq -c '[(.status.conditions // [])[] | {type,status,reason}]')"
  record FAIL "UPDATE broken-ConfigMap controller freeze" "conditions=${conds}"
fi

results
