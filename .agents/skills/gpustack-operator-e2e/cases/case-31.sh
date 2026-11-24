#!/usr/bin/env bash
#
# CASE 31 — ModelDeploymentScaler admission, defaulting, single-writer fence,
#           binding/suspend/term lifecycle, capacityPrior defaulting, priors.json fallback,
#           Priority 1/2 whole-unit resolution, EPD coupling, and ConfigMap freeze
#   (MUTATING, self-recovering; target-dependent and capacityPrior rows AUTO-SKIP when no
#    usable admitted-platform AcceleratorType exists)
#
#   case-31.sh <NS>
#
# Prerequisites: nodes=1; gpu=none; cluster=any; inputs=E2E_C31_BOUND,E2E_MD_IMAGE,E2E_MD_INSTANCE_TYPE,E2E_MD_ACCELERATOR_TYPE
# Goal:        Verify the full ModelDeploymentScaler control-plane contract:
#                1. Admission defaults (16 fields on stored scaler) and schema/webhook refusals
#                   (absent targetRef.name, maxReplicas ceiling, scaleUp window, sampling interval,
#                   Proportional without metric, zero role target, infeasible integer ratios).
#                2. Target-bound refusals (unknown role, custom command role, elastic-EP role) and
#                   lifecycle (Shadow observes without binding, targetRef immutable, Active binds
#                   with term, single-writer fence blocks replicas and scaler annotations while
#                   allowing labels, second scaler gets Bound=False/AlreadyBound, suspend releases
#                   binding & unblocks replicas, resume advances term monotonically, finalizer
#                   present and deletion releases binding).
#                3. Disaggregated P/D & EPD capacityPrior contracts: headroom/catalogRef.key
#                   defaulting, SLO default suppression & fail-closed CREATE refusals, Priority 1
#                   disaggregated whole-unit resolution, priors.json fallback, Priority 2
#                   aggregated GPU-time-balanced fallback (ConfigMap + embedded H100), EPD
#                   1E:2P:4D coupling with pdScale=gcd(2,4)=2, and UPDATE broken-ConfigMap warning
#                   + controller freeze (CapacityPriorsReady=False, Active=False, Blocked=True).
# Environment: Any cluster with ModelDeployment and ModelDeploymentScaler CRDs installed (run
#              case-1 first) and jq on PATH. Target-bound and capacityPrior rows require a usable
#              AcceleratorType reporting ascend, nvidia, or amd (mock or real) with a non-empty
#              status.detail.product; otherwise those rows record SKIP explicitly.
# Inputs:      REAL: CRD schema, defaulting/validating webhooks, ModelDeploymentScaler controller,
#              and synthetic ConfigMap capacity prior catalogs. Placeholder pause images are never
#              pulled or served.
# Expected:    All admission defaults, refusals, binding/fence/term lifecycle, and capacityPrior
#              resolution/freeze rows PASS (embedded H100 row PASSes on H100 or SKIPs elsewhere).
# Cleanup:     EXIT trap deletes all ModelDeploymentScalers, ModelDeployments, and ConfigMaps
#              created by this case, waiting for scaler finalizers to clear cleanly.
set -uo pipefail

CASE_ID=31
export CASE_ID
CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=/dev/null
. "${CASE_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_md-lib.sh"

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-31.sh <NS>" >&2
  exit 2
fi

command -v jq >/dev/null 2>&1 || { echo "[case-31] jq is required." >&2; exit 2; }

SCALER_CRD="modeldeploymentscalers.worker.gpustack.ai"
MD_CRD="modeldeployments.worker.gpustack.ai"
SCALER_RES="modeldeploymentscalers.v1alpha1.worker.gpustack.ai"
MD_RES="modeldeployments.v1alpha1.worker.gpustack.ai"
BINDING_KEY="modeldeployment.gpustack.ai/scaler-binding"
FINALIZER_KEY="modeldeployment.gpustack.ai/scaler-binding"
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
BOUND_WAIT="${E2E_C31_BOUND:-120}"

if ! kubectl get crd "$SCALER_CRD" >/dev/null 2>&1 || ! kubectl get crd "$MD_CRD" >/dev/null 2>&1; then
  record SKIP "case-31 prerequisites" "missing ${SCALER_CRD} or ${MD_CRD}"
  results
  exit 0
fi

RUN_PREFIX="e2e-c31-$$"
DEFAULTS_SCALER="${RUN_PREFIX}-defaults"
ABSENT_TARGET="${RUN_PREFIX}-absent-target"
TARGET="${RUN_PREFIX}-target"
CMD_TARGET="${RUN_PREFIX}-target-command"
ELASTIC_TARGET="${RUN_PREFIX}-target-elastic"
S1="${RUN_PREFIX}-scaler1"
S2="${RUN_PREFIX}-scaler2"

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

TMP_DIR="$(mktemp -d)"
CLEANED=false
cleanup() {
  if [ "$CLEANED" = true ]; then
    return 0
  fi
  CLEANED=true
  kubectl -n "$NS" delete "$SCALER_RES" \
    "$DEFAULTS_SCALER" "$S1" "$S2" "$SC_P1" "$SC_JSON" "$SC_P2_CM" "$SC_P2_EMB" "$SC_EPD" \
    --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1 || true
  kubectl -n "$NS" delete "$MD_RES" \
    "$TARGET" "$CMD_TARGET" "$ELASTIC_TARGET" "$MD_PD" "$MD_PD_KV" "$MD_H100" "$MD_EPD" \
    --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1 || true
  kubectl -n "$NS" delete configmap "$CM_DISAGG" "$CM_JSON" "$CM_BOTH" "$CM_AGG" \
    --ignore-not-found >/dev/null 2>&1 || true
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT INT TERM

# Helpers.
scaler_condition() {
  kubectl -n "$NS" get "$SCALER_RES" "$1" -o json 2>/dev/null \
    | jq -r --arg t "$2" \
      '([(.status.conditions // [])[] | select(.type == $t) | (.status + "|" + (.reason // ""))] | .[0]) // ""'
}

wait_condition() {
  local scaler="$1" ctype="$2" want="$3" reason="$4" bound="$5" deadline
  deadline=$((SECONDS + bound))
  while [ "$SECONDS" -lt "$deadline" ]; do
    [ "$(scaler_condition "$scaler" "$ctype")" = "${want}|${reason}" ] && return 0
    sleep 3
  done
  return 1
}

wait_scaler_cond_status() {
  local name="$1" cond="$2" want="$3" deadline=$((SECONDS + 60)) got=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    got="$(kubectl -n "$NS" get "$SCALER_RES" "$name" -o json 2>/dev/null \
      | jq -r --arg c "$cond" '[(.status.conditions // [])[] | select(.type == $c) | .status] | .[0] // ""' 2>/dev/null || true)"
    [ "$got" = "$want" ] && return 0
    sleep 2
  done
  return 1
}

target_binding() {
  kubectl -n "$NS" get "$MD_RES" "$1" -o json 2>/dev/null \
    | jq -r --arg k "$BINDING_KEY" '.metadata.annotations[$k] // ""'
}

target_binding_term() {
  local raw
  raw="$(target_binding "$1")"
  [ -n "$raw" ] || { echo ""; return 0; }
  printf '%s' "$raw" | jq -r '.term // ""'
}

role_replicas() {
  kubectl -n "$NS" get "$MD_RES" "$1" -o json 2>/dev/null \
    | jq -r --arg r "$2" '[(.spec.roles // [])[] | select(.name == $r) | .replicas] | .[0] // ""'
}

stored() {
  kubectl -n "$NS" get "$SCALER_RES" "$1" -o json 2>/dev/null | jq -r "$2"
}

expect_eq() {
  if [ "$2" = "$3" ]; then
    record PASS "$1" "reading=${3}"
  else
    record FAIL "$1" "expected ${3}, got ${2:-<empty>}"
  fi
}

refuses() {
  local check="$1" phrase="$2" obj status=0
  obj="$(md_dryrun_refuses "$phrase")" || status=1
  if [ "$status" -eq 0 ]; then
    record PASS "$check" "$obj"
  else
    record FAIL "$check" "$obj"
  fi
}

accepts() {
  local check="$1" obj status=0
  obj="$(md_dryrun_accepts)" || status=1
  if [ "$status" -eq 0 ]; then
    record PASS "$check" "$obj"
  else
    record FAIL "$check" "$obj"
  fi
}

expect_file_refusal() {
  local label="$1" file="$2" needle="$3" out rc=0
  out="$(kubectl create --dry-run=server -f "$file" 2>&1)" || rc=$?
  if [ "$rc" -eq 0 ]; then
    record FAIL "$label" "expected CREATE refusal matching '${needle}', but succeeded"
  elif printf '%s' "$out" | grep -Fq -- "$needle"; then
    record PASS "$label" "refused (${needle})"
  else
    record FAIL "$label" "refusal missing '${needle}': ${out:0:180}"
  fi
}

qty_eq() {
  python3 - "$1" "$2" <<'PY'
import sys
def parse_qty(s):
    s = s.strip()
    mult = {"k": 1e3, "K": 1e3, "M": 1e6, "G": 1e9, "m": 1e-3}
    return float(s[:-1]) * mult[s[-1]] if (s and s[-1] in mult) else float(s)
sys.exit(0 if abs(parse_qty(sys.argv[1]) - float(sys.argv[2])) < 1e-6 else 1)
PY
}

# Discover admitted-platform AcceleratorType for target-dependent and capacityPrior phases.
IT="${E2E_MD_ACCELERATOR_TYPE:-${E2E_MD_INSTANCE_TYPE:-}}"
if [ -z "$IT" ]; then
  for m in ascend nvidia amd; do
    found="$(md_usable_type_for "$m")"
    if [ -n "$found" ]; then
      IT="$found"
      break
    fi
  done
fi

# =================================================================================================
# PHASE 1: Stored defaults and target-independent admission refusals
# =================================================================================================

kubectl apply -f - >/dev/null 2>&1 <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${DEFAULTS_SCALER}
  namespace: ${NS}
spec:
  targetRef:
    name: ${ABSENT_TARGET}
  roles:
    - name: prefill
      maxReplicas: 4
    - name: decode
      maxReplicas: 4
  algorithm:
    type: TokenThroughput
EOF

if kubectl -n "$NS" get "$SCALER_RES" "$DEFAULTS_SCALER" >/dev/null 2>&1; then
  expect_eq "mode defaults to Shadow" "$(stored "$DEFAULTS_SCALER" '.spec.mode')" "Shadow"
  expect_eq "sampling interval defaults to 15s" "$(stored "$DEFAULTS_SCALER" '.spec.sampling.intervalSeconds')" "15"
  expect_eq "pending timeout defaults to 300s" "$(stored "$DEFAULTS_SCALER" '.spec.sampling.pendingTimeoutSeconds')" "300"
  expect_eq "role minReplicas defaults to 1" "$(stored "$DEFAULTS_SCALER" '.spec.roles[0].minReplicas')" "1"
  expect_eq "scaleUp maxStepReplicas defaults to 4" "$(stored "$DEFAULTS_SCALER" '.spec.behavior.scaleUp.maxStepReplicas')" "4"
  expect_eq "scaleUp maxStepPercent defaults to 100" "$(stored "$DEFAULTS_SCALER" '.spec.behavior.scaleUp.maxStepPercent')" "100"
  expect_eq "scaleUp stabilizationWindowSeconds defaults to 0" "$(stored "$DEFAULTS_SCALER" '.spec.behavior.scaleUp.stabilizationWindowSeconds // 0')" "0"
  expect_eq "scaleDown stabilizationWindowSeconds defaults to 300" "$(stored "$DEFAULTS_SCALER" '.spec.behavior.scaleDown.stabilizationWindowSeconds')" "300"
  expect_eq "cooldownAfterAbortSeconds defaults to 300" "$(stored "$DEFAULTS_SCALER" '.spec.behavior.scaleDown.cooldownAfterAbortSeconds')" "300"
  expect_eq "decodePostScaleUpGraceSeconds defaults to 120" "$(stored "$DEFAULTS_SCALER" '.spec.behavior.scaleDown.decodePostScaleUpGraceSeconds')" "120"
  expect_eq "rebalance policy defaults to Opposing" "$(stored "$DEFAULTS_SCALER" '.spec.rebalance.policy')" "Opposing"
  expect_eq "tokenThroughput stableWindowSeconds defaults to 180" "$(stored "$DEFAULTS_SCALER" '.spec.algorithm.tokenThroughput.stableWindowSeconds')" "180"
  expect_eq "tokenThroughput panicWindowSeconds defaults to 60" "$(stored "$DEFAULTS_SCALER" '.spec.algorithm.tokenThroughput.panicWindowSeconds')" "60"
  expect_eq "tokenThroughput panicThreshold defaults to 2" "$(stored "$DEFAULTS_SCALER" '.spec.algorithm.tokenThroughput.panicThreshold')" "2"
  expect_eq "tokenThroughput ttftTargetMilliseconds defaults to 500" "$(stored "$DEFAULTS_SCALER" '.spec.algorithm.tokenThroughput.ttftTargetMilliseconds')" "500"
  expect_eq "tokenThroughput itlTargetMilliseconds defaults to 50" "$(stored "$DEFAULTS_SCALER" '.spec.algorithm.tokenThroughput.itlTargetMilliseconds')" "50"
else
  record FAIL "stored scaler defaults" "failed to create ${DEFAULTS_SCALER}"
fi

accepts "scaler naming an absent target is ACCEPTED" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${DEFAULTS_SCALER}-baseline
  namespace: ${NS}
spec:
  targetRef:
    name: ${ABSENT_TARGET}
  roles:
    - name: prefill
      maxReplicas: 4
  algorithm:
    type: TokenThroughput
EOF

refuses "scaler naming no targetRef.name is refused" "spec.targetRef.name" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${DEFAULTS_SCALER}-notarget
  namespace: ${NS}
spec:
  targetRef: {}
  roles:
    - name: prefill
      maxReplicas: 4
  algorithm:
    type: TokenThroughput
EOF

refuses "maxReplicas ceiling above schema is refused" "spec.roles[0].maxReplicas" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${DEFAULTS_SCALER}-ceiling
  namespace: ${NS}
spec:
  targetRef:
    name: ${ABSENT_TARGET}
  roles:
    - name: prefill
      maxReplicas: 2000
  algorithm:
    type: TokenThroughput
EOF

refuses "scaleUp stabilizationWindowSeconds above schema is refused" "spec.behavior.scaleUp.stabilizationWindowSeconds" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${DEFAULTS_SCALER}-window
  namespace: ${NS}
spec:
  targetRef:
    name: ${ABSENT_TARGET}
  roles:
    - name: prefill
      maxReplicas: 4
  algorithm:
    type: TokenThroughput
  behavior:
    scaleUp:
      stabilizationWindowSeconds: 4000
EOF

refuses "sampling intervalSeconds below schema is refused" "spec.sampling.intervalSeconds" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${DEFAULTS_SCALER}-interval
  namespace: ${NS}
spec:
  targetRef:
    name: ${ABSENT_TARGET}
  roles:
    - name: prefill
      maxReplicas: 4
  algorithm:
    type: TokenThroughput
  sampling:
    intervalSeconds: 1
EOF

refuses "Proportional algorithm naming no metric is refused" "spec.algorithm.proportional.metric" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${DEFAULTS_SCALER}-metric
  namespace: ${NS}
spec:
  targetRef:
    name: ${ABSENT_TARGET}
  roles:
    - name: prefill
      maxReplicas: 4
  algorithm:
    type: Proportional
    proportional:
      roleTargets:
        - name: prefill
          target: "4"
EOF

refuses "role target of zero is refused" "the target of a role is the signal one replica of it is expected to carry" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${DEFAULTS_SCALER}-zerotarget
  namespace: ${NS}
spec:
  targetRef:
    name: ${ABSENT_TARGET}
  roles:
    - name: prefill
      maxReplicas: 4
  algorithm:
    type: Proportional
    proportional:
      metric: RunningRequests
      roleTargets:
        - name: prefill
          target: "0"
EOF

refuses "infeasible integer bounds and ratios are refused" \
  "no replica vector inside these bounds satisfies every ratio at the same time" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${DEFAULTS_SCALER}-infeasible
  namespace: ${NS}
spec:
  targetRef:
    name: ${ABSENT_TARGET}
  roles:
    - name: prefill
      minReplicas: 1
      maxReplicas: 2
    - name: decode
      minReplicas: 1
      maxReplicas: 2
  ratios:
    - numeratorRole: prefill
      denominatorRole: decode
      target: "3"
      min: "3"
      max: "3"
  algorithm:
    type: TokenThroughput
EOF

kubectl -n "$NS" delete "$SCALER_RES" "$DEFAULTS_SCALER" --ignore-not-found --wait=true --timeout=60s >/dev/null 2>&1 || true

# =================================================================================================
# PHASE 2: Target-bound refusals, single-writer fence, and binding/suspend/term lifecycle
# =================================================================================================

TARGET_REASON=""
[ -n "$IT" ] || TARGET_REASON="no usable AcceleratorType reports ascend, nvidia, or amd"

if [ -z "$TARGET_REASON" ]; then
  out="$(kubectl apply -f - 2>&1 <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${TARGET}
  namespace: ${NS}
spec:
  engine:
    name: vLLM
    image: ${IMAGE}
  model:
    name: /models
  roles:
    - name: prefill
      kind: Prefill
      replicas: 2
      size: 1
      acceleratorType: ${IT}
      resources:
        accelerator: "1"
    - name: decode
      kind: Decode
      replicas: 2
      size: 1
      acceleratorType: ${IT}
      resources:
        accelerator: "1"
EOF
)"
  case "$out" in *created* | *configured*) ;; *) TARGET_REASON="target refused: ${out:0:180}" ;; esac
fi

if [ -n "$TARGET_REASON" ]; then
  record SKIP "Phase 2 target-bound scaler refusals and lifecycle" "$TARGET_REASON"
else
  refuses "scaler naming unknown target role is refused" \
    "the target does not have a role with this name, so there is nothing to scale" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${RUN_PREFIX}-unknownrole
  namespace: ${NS}
spec:
  targetRef:
    name: ${TARGET}
  roles:
    - name: prefill
      maxReplicas: 4
    - name: encoder
      maxReplicas: 2
  algorithm:
    type: TokenThroughput
EOF

  if kubectl apply -f - >/dev/null 2>&1 <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${CMD_TARGET}
  namespace: ${NS}
spec:
  engine:
    name: vLLM
    image: ${IMAGE}
  model:
    name: /models
  roles:
    - name: server
      kind: Server
      replicas: 1
      size: 1
      acceleratorType: ${IT}
      command: ["/bin/sh", "-c", "sleep 3600"]
      resources:
        accelerator: "1"
EOF
  then
    refuses "scaler targeting unmanaged command role is refused" \
      "this role runs a command of its own, and a scaler cannot tell what that command serves" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${RUN_PREFIX}-command
  namespace: ${NS}
spec:
  targetRef:
    name: ${CMD_TARGET}
  roles:
    - name: server
      maxReplicas: 2
  algorithm:
    type: TokenThroughput
EOF
  else
    record SKIP "scaler targeting unmanaged command role is refused" "could not create command target"
  fi

  if kubectl apply -f - >/dev/null 2>&1 <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${ELASTIC_TARGET}
  namespace: ${NS}
spec:
  engine:
    name: vLLM
    image: ${IMAGE}
  model:
    name: /models
  roles:
    - name: server
      kind: Server
      replicas: 1
      size: 1
      acceleratorType: ${IT}
      runtimeControl:
        parameters:
          elastic.dataParallelWidth: "2"
      resources:
        accelerator: "1"
EOF
  then
    refuses "scaler targeting elastic-EP role is refused" "this role runs the elastic-EP profile" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${RUN_PREFIX}-elastic
  namespace: ${NS}
spec:
  targetRef:
    name: ${ELASTIC_TARGET}
  roles:
    - name: server
      maxReplicas: 2
  algorithm:
    type: TokenThroughput
EOF
  else
    record SKIP "scaler targeting elastic-EP role is refused" "elastic-EP target refused on this cluster"
  fi

  # Shadow -> Active -> single-writer fence -> second scaler AlreadyBound -> suspend -> resume term -> delete.
  kubectl apply -f - >/dev/null 2>&1 <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${S1}
  namespace: ${NS}
spec:
  targetRef:
    name: ${TARGET}
  mode: Shadow
  roles:
    - name: prefill
      minReplicas: 2
      maxReplicas: 4
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
EOF

  if wait_condition "$S1" Bound True Bound "$BOUND_WAIT"; then
    record PASS "Shadow scaler reports Bound=True" "prefill replicas=$(role_replicas "$TARGET" prefill)"
  else
    record FAIL "Shadow scaler reports Bound=True" "got $(scaler_condition "$S1" Bound)"
  fi
  expect_eq "Shadow scaler leaves target replicas untouched" "$(role_replicas "$TARGET" prefill)" "2"
  expect_eq "Shadow scaler does not take scaler-binding annotation" "$(target_binding "$TARGET")" ""

  out="$(kubectl -n "$NS" patch "$SCALER_RES" "$S1" --type=merge \
    -p '{"spec":{"targetRef":{"name":"e2e-c31-somewhere-else"}}}' 2>&1 | tr '\n' ' ')"
  case "$out" in
    *"the target of a scaler is fixed once it is set"*) record PASS "scaler targetRef is immutable" "${out:0:140}" ;;
    *) record FAIL "scaler targetRef is immutable" "got: ${out:0:180}" ;;
  esac

  kubectl -n "$NS" patch "$SCALER_RES" "$S1" --type=merge -p '{"spec":{"mode":"Active"}}' >/dev/null 2>&1
  term_one=""
  deadline=$((SECONDS + BOUND_WAIT))
  while [ "$SECONDS" -lt "$deadline" ]; do
    term_one="$(target_binding_term "$TARGET")"
    [ -n "$term_one" ] && break
    sleep 3
  done
  if [ -n "$term_one" ]; then
    record PASS "Active scaler takes scaler-binding annotation" "term=${term_one}"
  else
    record FAIL "Active scaler takes scaler-binding annotation" "annotation absent"
  fi
  expect_eq "Active scaler reports Bound=True/Bound" "$(scaler_condition "$S1" Bound)" "True|Bound"
  expect_eq "Active scaler reports Active=True/Active" "$(scaler_condition "$S1" Active)" "True|Active"

  out="$(kubectl -n "$NS" patch "$MD_RES" "$TARGET" --type=json \
    -p '[{"op":"replace","path":"/spec/roles/0/replicas","value":3}]' 2>&1 | tr '\n' ' ')"
  case "$out" in
    *"the replica count of a bound deployment belongs to the scaler that owns it"*)
      record PASS "single-writer fence blocks user replicas patch" "refused"
      ;;
    *) record FAIL "single-writer fence blocks user replicas patch" "got: ${out:0:180}" ;;
  esac

  out="$(kubectl -n "$NS" patch "$MD_RES" "$TARGET" --type=merge \
    -p '{"metadata":{"annotations":{"modeldeployment.gpustack.ai/scaler-binding":"{\"scalerName\":\"e2e-c31-impostor\",\"scalerUID\":\"00000000-0000-0000-0000-000000000000\",\"term\":99}"}}}' 2>&1 | tr '\n' ' ')"
  case "$out" in
    *"the binding and decision annotations of a bound deployment belong to the scaler that owns it"*)
      record PASS "single-writer fence blocks user scaler-binding annotation patch" "refused"
      ;;
    *) record FAIL "single-writer fence blocks user scaler-binding annotation patch" "got: ${out:0:180}" ;;
  esac

  out="$(kubectl -n "$NS" patch "$MD_RES" "$TARGET" --type=merge \
    -p '{"metadata":{"labels":{"e2e-c31.gpustack.ai/user-edit":"yes"}}}' 2>&1 | tr '\n' ' ')"
  case "$out" in
    *patched* | *configured*) record PASS "user may still edit labels on a bound deployment" "${out:0:120}" ;;
    *) record FAIL "user may still edit labels on a bound deployment" "${out:0:180}" ;;
  esac

  kubectl apply -f - >/dev/null 2>&1 <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${S2}
  namespace: ${NS}
spec:
  targetRef:
    name: ${TARGET}
  mode: Active
  roles:
    - name: prefill
      minReplicas: 2
      maxReplicas: 4
  algorithm:
    type: Proportional
    proportional:
      metric: RunningRequests
      roleTargets:
        - name: prefill
          target: "4"
EOF
  if wait_condition "$S2" Bound False AlreadyBound "$BOUND_WAIT"; then
    record PASS "second scaler on bound target gets Bound=False/AlreadyBound" "AlreadyBound"
  else
    record FAIL "second scaler on bound target gets Bound=False/AlreadyBound" "got $(scaler_condition "$S2" Bound)"
  fi
  kubectl -n "$NS" delete "$SCALER_RES" "$S2" --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1 || true

  kubectl -n "$NS" patch "$SCALER_RES" "$S1" --type=merge -p '{"spec":{"suspend":true}}' >/dev/null 2>&1
  deadline=$((SECONDS + BOUND_WAIT))
  while [ "$SECONDS" -lt "$deadline" ]; do
    [ -z "$(target_binding "$TARGET")" ] && break
    sleep 3
  done
  expect_eq "suspended scaler releases scaler-binding" "$(target_binding "$TARGET")" ""
  expect_eq "suspended scaler reports Active=False/Suspended" "$(scaler_condition "$S1" Active)" "False|Suspended"

  out="$(kubectl -n "$NS" patch "$MD_RES" "$TARGET" --type=json \
    -p '[{"op":"replace","path":"/spec/roles/0/replicas","value":3}]' 2>&1 | tr '\n' ' ')"
  case "$out" in
    *patched* | *configured*) record PASS "replicas writable while scaler is suspended" "${out:0:120}" ;;
    *) record FAIL "replicas writable while scaler is suspended" "${out:0:180}" ;;
  esac

  kubectl -n "$NS" patch "$SCALER_RES" "$S1" --type=merge -p '{"spec":{"suspend":false}}' >/dev/null 2>&1
  term_two=""
  deadline=$((SECONDS + BOUND_WAIT))
  while [ "$SECONDS" -lt "$deadline" ]; do
    term_two="$(target_binding_term "$TARGET")"
    if [ -n "$term_two" ] && [ -n "$term_one" ] && [ "$term_two" -gt "$term_one" ]; then
      break
    fi
    sleep 3
  done
  if [ -n "$term_two" ] && [ -n "$term_one" ] && [ "$term_two" -gt "$term_one" ]; then
    record PASS "resumed scaler advances binding term monotonically" "${term_one} -> ${term_two}"
  else
    record FAIL "resumed scaler advances binding term monotonically" "${term_one:-?} -> ${term_two:-?}"
  fi

  finalizers="$(kubectl -n "$NS" get "$SCALER_RES" "$S1" -o json 2>/dev/null | jq -r '(.metadata.finalizers // []) | join(",")')"
  case "$finalizers" in
    *"$FINALIZER_KEY"*) record PASS "scaler carries scaler-binding finalizer" "${finalizers}" ;;
    *) record FAIL "scaler carries scaler-binding finalizer" "got '${finalizers:-<none>}'" ;;
  esac

  kubectl -n "$NS" delete "$SCALER_RES" "$S1" --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1 || true
  if kubectl -n "$NS" get "$SCALER_RES" "$S1" >/dev/null 2>&1; then
    record FAIL "deleting scaler releases finalizer and binding" "${S1} still exists"
  else
    record PASS "deleting scaler releases finalizer and binding" "${S1} deleted cleanly"
  fi
  expect_eq "deleted scaler leaves target unbound" "$(target_binding "$TARGET")" ""

  kubectl -n "$NS" delete "$MD_RES" "$TARGET" "$CMD_TARGET" "$ELASTIC_TARGET" \
    --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1 || true
fi

# =================================================================================================
# PHASE 3: Disaggregated P/D & EPD capacityPrior control-plane contracts
# =================================================================================================

HW_PRODUCT=""
HW_MFG=""
if [ -n "$IT" ]; then
  HW_PRODUCT="$(kubectl get acceleratortype "$IT" -o jsonpath='{.status.detail.product}' 2>/dev/null || true)"
  HW_MFG="$(kubectl get acceleratortype "$IT" -o jsonpath='{.status.detail.manufacturer}' 2>/dev/null || true)"
fi
HW_MFG_LC="$(printf '%s' "$HW_MFG" | tr '[:upper:]' '[:lower:]')"
HW_PRODUCT_LC="$(printf '%s' "$HW_PRODUCT" | tr '[:upper:]' '[:lower:]')"

if [ -z "$IT" ] || [ -z "$HW_PRODUCT" ] || [[ ! "$HW_MFG_LC" =~ ^(nvidia|amd|ascend)$ ]]; then
  record SKIP "Phase 3 capacityPrior control-plane contracts" \
    "needs usable nvidia/amd/ascend AcceleratorType with status.detail.product (got at=${IT:-none}, mfg=${HW_MFG:-none}, product=${HW_PRODUCT:-none})"
else
  write_pd_md() {
    local name="$1" prefill_rep="$2" decode_rep="$3" kv_pool="${4:-}" out="${TMP_DIR}/${1}.yaml"
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
    image: ${IMAGE}
  router:
    name: llm-d-router
  roles:
    - name: prefill
      kind: Prefill
      replicas: ${prefill_rep}
      acceleratorType: ${IT}
      resources:
        accelerator: "1"
    - name: decode
      kind: Decode
      replicas: ${decode_rep}
      acceleratorType: ${IT}
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
          topology: {gpusPerReplica: 1, tensorParallel: 1, expertParallel: 1}
        decode:
          replicas: 2
          hardware: ${HW_PRODUCT}
          topology: {gpusPerReplica: 1, tensorParallel: 1, expertParallel: 1}
        points:
          - concurrency: 1
            totalTokensPerSecond: 500
            inputTokensPerSecond: 400
            outputTokensPerSecond: 100
            ttftMilliseconds: {p99: 300}
            itlMilliseconds: {p99: 20}
          - concurrency: 2
            totalTokensPerSecond: 1000
            inputTokensPerSecond: 800
            outputTokensPerSecond: 200
            ttftMilliseconds: {p99: 400}
            itlMilliseconds: {p99: 30}
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
      "profiles": [{
        "mode": "disaggregated", "model": "Qwen/Qwen3-8B", "quantization": "none", "engine": "vllm",
        "prefill": {"replicas": 1, "hardware": "${HW_PRODUCT}", "topology": {"gpusPerReplica": 1, "tensorParallel": 1, "expertParallel": 1}},
        "decode": {"replicas": 2, "hardware": "${HW_PRODUCT}", "topology": {"gpusPerReplica": 1, "tensorParallel": 1, "expertParallel": 1}},
        "points": [
          {"concurrency": 1, "totalTokensPerSecond": 500, "inputTokensPerSecond": 400, "outputTokensPerSecond": 100, "ttftMilliseconds": {"p99": 300}, "itlMilliseconds": {"p99": 20}},
          {"concurrency": 2, "totalTokensPerSecond": 1000, "inputTokensPerSecond": 800, "outputTokensPerSecond": 200, "ttftMilliseconds": {"p99": 400}, "itlMilliseconds": {"p99": 30}}
        ]
      }]
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
        prefill: {replicas: 1, hardware: ${HW_PRODUCT}, topology: {gpusPerReplica: 1, tensorParallel: 1, expertParallel: 1}}
        decode: {replicas: 2, hardware: ${HW_PRODUCT}, topology: {gpusPerReplica: 1, tensorParallel: 1, expertParallel: 1}}
        points:
          - concurrency: 1
            totalTokensPerSecond: 500
            inputTokensPerSecond: 400
            outputTokensPerSecond: 100
            ttftMilliseconds: {p90: 1500, p99: 1500}
            itlMilliseconds: {p90: 80, p99: 80}
          - concurrency: 2
            totalTokensPerSecond: 1000
            inputTokensPerSecond: 800
            outputTokensPerSecond: 200
            ttftMilliseconds: {p90: 2000, p99: 2000}
            itlMilliseconds: {p90: 100, p99: 100}
      - mode: aggregated
        model: Qwen/Qwen3-8B
        quantization: none
        engine: vllm
        hardware: ${HW_PRODUCT}
        topology: {gpusPerReplica: 1, tensorParallel: 1, expertParallel: 1}
        points:
          - concurrency: 1
            totalTokensPerSecond: 2500
            inputTokensPerSecond: 2000
            outputTokensPerSecond: 500
            ttftMilliseconds: {p99: 80}
            itlMilliseconds: {p99: 8}
          - concurrency: 2
            totalTokensPerSecond: 5000
            inputTokensPerSecond: 4000
            outputTokensPerSecond: 1000
            ttftMilliseconds: {p99: 100}
            itlMilliseconds: {p99: 10}
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
        topology: {gpusPerReplica: 1, tensorParallel: 1, expertParallel: 1}
        points:
          - concurrency: 1
            totalTokensPerSecond: 1000
            inputTokensPerSecond: 800
            outputTokensPerSecond: 200
            ttftMilliseconds: {p99: 80}
            itlMilliseconds: {p99: 8}
          - concurrency: 2
            totalTokensPerSecond: 2000
            inputTokensPerSecond: 1600
            outputTokensPerSecond: 400
            ttftMilliseconds: {p99: 100}
            itlMilliseconds: {p99: 10}
YAML

  kubectl apply -f "$(write_pd_md "$MD_PD" 1 2)" >/dev/null

  cat > "${TMP_DIR}/sc-default.yaml" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${SC_P1}
  namespace: ${NS}
spec:
  targetRef: {name: ${MD_PD}}
  mode: Active
  roles:
    - {name: prefill, minReplicas: 1, maxReplicas: 3}
    - {name: decode, minReplicas: 2, maxReplicas: 6}
  ratios:
    - {numeratorRole: decode, denominatorRole: prefill, target: "2", min: "2", max: "2"}
  algorithm:
    type: TokenThroughput
    tokenThroughput:
      ttftTargetMilliseconds: "500"
      itlTargetMilliseconds: "50"
    capacityPrior:
      catalogRef: {name: ${CM_DISAGG}}
YAML

  if kubectl apply -f "${TMP_DIR}/sc-default.yaml" >/dev/null 2>&1; then
    sc_json="$(kubectl -n "$NS" get "$SCALER_RES" "$SC_P1" -o json)"
    def_hr="$(printf '%s' "$sc_json" | jq -r '.spec.algorithm.capacityPrior.headroom // ""')"
    def_key="$(printf '%s' "$sc_json" | jq -r '.spec.algorithm.capacityPrior.catalogRef.key // ""')"
    if [ -n "$def_hr" ] && qty_eq "$def_hr" 0.75 && [ "$def_key" = "priors.yaml" ]; then
      record PASS "capacityPrior defaulting (headroom=0.75, key=priors.yaml)" "headroom=${def_hr}, key=${def_key}"
    else
      record FAIL "capacityPrior defaulting (headroom=0.75, key=priors.yaml)" "headroom=${def_hr}, key=${def_key}"
    fi
  else
    record FAIL "capacityPrior defaulting (headroom=0.75, key=priors.yaml)" "apply ${SC_P1} failed"
  fi

  cat > "${TMP_DIR}/sc-empty-prior.yaml" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${RUN_PREFIX}-sc-empty-prior
  namespace: ${NS}
spec:
  targetRef: {name: ${MD_PD}}
  mode: Active
  roles:
    - {name: prefill, minReplicas: 1, maxReplicas: 2}
    - {name: decode, minReplicas: 2, maxReplicas: 4}
  ratios:
    - {numeratorRole: decode, denominatorRole: prefill, target: "2", min: "2", max: "2"}
  algorithm:
    type: TokenThroughput
    tokenThroughput:
      ttftTargetMilliseconds: "500"
      itlTargetMilliseconds: "50"
      prefillTargetTokensPerSecond: "100"
      decodeTargetTokensPerSecond: "200"
    capacityPrior: {}
YAML
  empty_hr="$(kubectl create --dry-run=server -f "${TMP_DIR}/sc-empty-prior.yaml" -o json 2>/dev/null | jq -r '.spec.algorithm.capacityPrior.headroom // ""' || true)"
  if [ -n "$empty_hr" ] && qty_eq "$empty_hr" 0.75; then
    record PASS "capacityPrior: {} defaults headroom=0.75" "headroom=${empty_hr}"
  else
    record FAIL "capacityPrior: {} defaults headroom=0.75" "got '${empty_hr}'"
  fi

  if wait_scaler_cond_status "$SC_P1" CapacityPriorsReady True; then
    sc_json="$(kubectl -n "$NS" get "$SCALER_RES" "$SC_P1" -o json)"
    p1_msg="$(printf '%s' "$sc_json" | jq -r '[(.status.conditions // [])[] | select(.type=="CapacityPriorsReady") | .message] | .[0] // ""')"
    fp_p="$(printf '%s' "$sc_json" | jq -r '[(.status.resolvedCapacityPriors // [])[] | select(.name=="prefill") | .compatibilityFingerprint] | .[0] // ""')"
    fp_d="$(printf '%s' "$sc_json" | jq -r '[(.status.resolvedCapacityPriors // [])[] | select(.name=="decode") | .compatibilityFingerprint] | .[0] // ""')"
    fld_p="$(printf '%s' "$sc_json" | jq -r '[(.status.resolvedCapacityPriors // [])[] | select(.name=="prefill") | .fields[]? | "\(.field)=\(.source)=\(.effectiveTokensPerSecond)"] | .[0] // ""')"
    fld_d="$(printf '%s' "$sc_json" | jq -r '[(.status.resolvedCapacityPriors // [])[] | select(.name=="decode") | .fields[]? | "\(.field)=\(.source)=\(.effectiveTokensPerSecond)"] | .[0] // ""')"
    if printf '%s' "$p1_msg" | grep -Fq "all required capacities are resolved via catalog disaggregated whole-unit priors" \
      && [ -n "$fp_p" ] && [ "$fp_p" = "$fp_d" ] \
      && printf '%s' "$fld_p" | grep -Fq "tokenThroughput.unitTargetTokensPerSecond=Catalog=" \
      && printf '%s' "$fld_d" | grep -Fq "tokenThroughput.unitTargetTokensPerSecond=Catalog="; then
      record PASS "Priority 1 disaggregated whole-unit resolution" "fp=${fp_p:0:12}..."
    else
      record FAIL "Priority 1 disaggregated whole-unit resolution" "msg='${p1_msg}', fp=${fp_p}/${fp_d}"
    fi
  else
    record FAIL "Priority 1 disaggregated whole-unit resolution" "CapacityPriorsReady!=True"
  fi

  # Fail-closed CREATE refusals & SLO default suppression.
  cat > "${TMP_DIR}/refuse-prop.yaml" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata: {name: ${RUN_PREFIX}-refuse-prop, namespace: ${NS}}
spec:
  targetRef: {name: ${MD_PD}}
  mode: Active
  roles:
    - {name: prefill, minReplicas: 1, maxReplicas: 2}
    - {name: decode, minReplicas: 2, maxReplicas: 4}
  algorithm:
    type: Proportional
    proportional:
      metric: RunningRequests
      roleTargets: [{name: prefill, target: "4"}, {name: decode, target: "4"}]
    capacityPrior: {}
YAML
  expect_file_refusal "CREATE refusal: Proportional + capacityPrior" "${TMP_DIR}/refuse-prop.yaml" \
    "this block is for the TokenThroughput and Predictive algorithms, and this scaler runs Proportional"

  cat > "${TMP_DIR}/refuse-nottft.yaml" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata: {name: ${RUN_PREFIX}-refuse-nottft, namespace: ${NS}}
spec:
  targetRef: {name: ${MD_PD}}
  mode: Active
  roles: [{name: prefill, minReplicas: 1, maxReplicas: 2}, {name: decode, minReplicas: 2, maxReplicas: 4}]
  ratios: [{numeratorRole: decode, denominatorRole: prefill, target: "2", min: "2", max: "2"}]
  algorithm:
    type: TokenThroughput
    tokenThroughput: {itlTargetMilliseconds: "50"}
    capacityPrior: {catalogRef: {name: ${CM_DISAGG}}}
YAML
  expect_file_refusal "CREATE refusal + SLO suppression: missing ttftTargetMilliseconds" "${TMP_DIR}/refuse-nottft.yaml" \
    "requires an explicit ttftTargetMilliseconds"

  cat > "${TMP_DIR}/refuse-noitl.yaml" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata: {name: ${RUN_PREFIX}-refuse-noitl, namespace: ${NS}}
spec:
  targetRef: {name: ${MD_PD}}
  mode: Active
  roles: [{name: prefill, minReplicas: 1, maxReplicas: 2}, {name: decode, minReplicas: 2, maxReplicas: 4}]
  ratios: [{numeratorRole: decode, denominatorRole: prefill, target: "2", min: "2", max: "2"}]
  algorithm:
    type: TokenThroughput
    tokenThroughput: {ttftTargetMilliseconds: "500"}
    capacityPrior: {catalogRef: {name: ${CM_DISAGG}}}
YAML
  expect_file_refusal "CREATE refusal + SLO suppression: missing itlTargetMilliseconds" "${TMP_DIR}/refuse-noitl.yaml" \
    "requires an explicit itlTargetMilliseconds"

  cat > "${TMP_DIR}/refuse-noratios.yaml" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata: {name: ${RUN_PREFIX}-refuse-noratios, namespace: ${NS}}
spec:
  targetRef: {name: ${MD_PD}}
  mode: Active
  roles: [{name: prefill, minReplicas: 1, maxReplicas: 2}, {name: decode, minReplicas: 2, maxReplicas: 4}]
  algorithm:
    type: TokenThroughput
    tokenThroughput: {ttftTargetMilliseconds: "500", itlTargetMilliseconds: "50"}
    capacityPrior: {catalogRef: {name: ${CM_DISAGG}}}
YAML
  expect_file_refusal "CREATE refusal: missing ratios on P/D" "${TMP_DIR}/refuse-noratios.yaml" \
    "fixed ratios covering all target roles"

  cat > "${TMP_DIR}/refuse-nonfixed.yaml" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata: {name: ${RUN_PREFIX}-refuse-nonfixed, namespace: ${NS}}
spec:
  targetRef: {name: ${MD_PD}}
  mode: Active
  roles: [{name: prefill, minReplicas: 1, maxReplicas: 2}, {name: decode, minReplicas: 2, maxReplicas: 4}]
  ratios: [{numeratorRole: decode, denominatorRole: prefill, target: "2", min: "1", max: "2"}]
  algorithm:
    type: TokenThroughput
    tokenThroughput: {ttftTargetMilliseconds: "500", itlTargetMilliseconds: "50"}
    capacityPrior: {catalogRef: {name: ${CM_DISAGG}}}
YAML
  expect_file_refusal "CREATE refusal: non-fixed ratio min != max" "${TMP_DIR}/refuse-nonfixed.yaml" \
    "requires fixed ratios (min == max)"

  cat > "${TMP_DIR}/refuse-partial.yaml" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata: {name: ${RUN_PREFIX}-refuse-partial, namespace: ${NS}}
spec:
  targetRef: {name: ${MD_PD}}
  mode: Active
  roles: [{name: prefill, minReplicas: 1, maxReplicas: 2}, {name: decode, minReplicas: 2, maxReplicas: 4}]
  ratios: [{numeratorRole: decode, denominatorRole: prefill, target: "2", min: "2", max: "2"}]
  algorithm:
    type: TokenThroughput
    tokenThroughput: {ttftTargetMilliseconds: "500", itlTargetMilliseconds: "50", prefillTargetTokensPerSecond: "500"}
    capacityPrior: {catalogRef: {name: ${CM_DISAGG}}}
YAML
  expect_file_refusal "CREATE refusal: partial explicit capacity on P/D" "${TMP_DIR}/refuse-partial.yaml" \
    "disaggregated whole-unit prior resolution does not permit partial explicit capacity field"

  kubectl apply -f "$(write_pd_md "$MD_PD_KV" 1 2 "${RUN_PREFIX}-missing-pool")" >/dev/null
  cat > "${TMP_DIR}/refuse-kv.yaml" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata: {name: ${RUN_PREFIX}-refuse-kv, namespace: ${NS}}
spec:
  targetRef: {name: ${MD_PD_KV}}
  mode: Active
  roles: [{name: prefill, minReplicas: 1, maxReplicas: 2}, {name: decode, minReplicas: 2, maxReplicas: 4}]
  ratios: [{numeratorRole: decode, denominatorRole: prefill, target: "2", min: "2", max: "2"}]
  algorithm:
    type: TokenThroughput
    tokenThroughput: {ttftTargetMilliseconds: "500", itlTargetMilliseconds: "50"}
    capacityPrior: {catalogRef: {name: ${CM_DISAGG}}}
YAML
  expect_file_refusal "CREATE refusal: kvCache offload mismatch" "${TMP_DIR}/refuse-kv.yaml" "servingConfig.offload"
  kubectl -n "$NS" delete "$MD_RES" "$MD_PD_KV" --ignore-not-found --wait=true >/dev/null 2>&1 || true

  cat > "${TMP_DIR}/refuse-noslo.yaml" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata: {name: ${RUN_PREFIX}-refuse-noslo, namespace: ${NS}}
spec:
  targetRef: {name: ${MD_PD}}
  mode: Active
  roles: [{name: prefill, minReplicas: 1, maxReplicas: 2}, {name: decode, minReplicas: 2, maxReplicas: 4}]
  ratios: [{numeratorRole: decode, denominatorRole: prefill, target: "2", min: "2", max: "2"}]
  algorithm:
    type: TokenThroughput
    tokenThroughput: {ttftTargetMilliseconds: "500", itlTargetMilliseconds: "50"}
    capacityPrior: {catalogRef: {name: ${CM_BOTH}}}
YAML
  expect_file_refusal "CREATE refusal: Priority 1 SLO violation does not fall back to Priority 2" \
    "${TMP_DIR}/refuse-noslo.yaml" "no measured load point that satisfies the latency targets"

  kubectl -n "$NS" delete "$SCALER_RES" "$SC_P1" --ignore-not-found --wait=true >/dev/null 2>&1 || true

  # priors.json fallback.
  kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata: {name: ${SC_JSON}, namespace: ${NS}}
spec:
  targetRef: {name: ${MD_PD}}
  mode: Shadow
  roles: [{name: prefill, minReplicas: 1, maxReplicas: 3}, {name: decode, minReplicas: 2, maxReplicas: 6}]
  ratios: [{numeratorRole: decode, denominatorRole: prefill, target: "2", min: "2", max: "2"}]
  algorithm:
    type: TokenThroughput
    tokenThroughput: {ttftTargetMilliseconds: "500", itlTargetMilliseconds: "50"}
    capacityPrior: {catalogRef: {name: ${CM_JSON}}}
YAML
  if wait_scaler_cond_status "$SC_JSON" CapacityPriorsReady True; then
    record PASS "ConfigMap priors.json fallback" "resolved via priors.json"
  else
    record FAIL "ConfigMap priors.json fallback" "CapacityPriorsReady!=True"
  fi
  kubectl -n "$NS" delete "$SCALER_RES" "$SC_JSON" --ignore-not-found --wait=true >/dev/null 2>&1 || true

  # Priority 2 aggregated fallback (ConfigMap).
  kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata: {name: ${SC_P2_CM}, namespace: ${NS}}
spec:
  targetRef: {name: ${MD_PD}}
  mode: Shadow
  roles: [{name: prefill, minReplicas: 1, maxReplicas: 2}, {name: decode, minReplicas: 2, maxReplicas: 4}]
  ratios: [{numeratorRole: decode, denominatorRole: prefill, target: "2", min: "2", max: "2"}]
  algorithm:
    type: TokenThroughput
    tokenThroughput: {ttftTargetMilliseconds: "500", itlTargetMilliseconds: "50"}
    capacityPrior: {catalogRef: {name: ${CM_AGG}}}
YAML
  if wait_scaler_cond_status "$SC_P2_CM" CapacityPriorsReady True; then
    msg="$(kubectl -n "$NS" get "$SCALER_RES" "$SC_P2_CM" -o json | jq -r '[(.status.conditions // [])[] | select(.type=="CapacityPriorsReady") | .message] | .[0] // ""')"
    if printf '%s' "$msg" | grep -Fq "GPU-time-balanced"; then
      record PASS "Priority 2 aggregated fallback (ConfigMap)" "GPU-time-balanced resolved"
    else
      record FAIL "Priority 2 aggregated fallback (ConfigMap)" "unexpected msg: ${msg}"
    fi
  else
    record FAIL "Priority 2 aggregated fallback (ConfigMap)" "CapacityPriorsReady!=True"
  fi
  kubectl -n "$NS" delete "$SCALER_RES" "$SC_P2_CM" --ignore-not-found --wait=true >/dev/null 2>&1 || true

  # Embedded H100 Priority 2 fallback.
  if printf '%s' "$HW_PRODUCT_LC" | grep -Fq "h100"; then
    kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD_H100}
  namespace: ${NS}
  annotations: {worker.gpustack.ai/precision: "fp8"}
spec:
  model: {name: meta-llama/Llama-3.3-70B-Instruct}
  engine: {name: vLLM, image: ${IMAGE}}
  router: {name: llm-d-router}
  roles:
    - {name: prefill, kind: Prefill, replicas: 1, acceleratorType: ${IT}, resources: {accelerator: "2"}, extraArgs: ["--tensor-parallel-size", "2"]}
    - {name: decode, kind: Decode, replicas: 1, acceleratorType: ${IT}, resources: {accelerator: "2"}, extraArgs: ["--tensor-parallel-size", "2"]}
---
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata: {name: ${SC_P2_EMB}, namespace: ${NS}}
spec:
  targetRef: {name: ${MD_H100}}
  mode: Shadow
  roles: [{name: prefill, minReplicas: 1, maxReplicas: 2}, {name: decode, minReplicas: 1, maxReplicas: 2}]
  ratios: [{numeratorRole: decode, denominatorRole: prefill, target: "1", min: "1", max: "1"}]
  algorithm:
    type: TokenThroughput
    tokenThroughput: {ttftTargetMilliseconds: "5000", itlTargetMilliseconds: "30"}
    capacityPrior: {}
YAML
    if wait_scaler_cond_status "$SC_P2_EMB" CapacityPriorsReady True; then
      msg="$(kubectl -n "$NS" get "$SCALER_RES" "$SC_P2_EMB" -o json | jq -r '[(.status.conditions // [])[] | select(.type=="CapacityPriorsReady") | .message] | .[0] // ""')"
      if printf '%s' "$msg" | grep -Fq "GPU-time-balanced"; then
        record PASS "Priority 2 aggregated fallback (embedded H100 priors)" "resolved via GPU-time-balanced"
      else
        record FAIL "Priority 2 aggregated fallback (embedded H100 priors)" "unexpected msg: ${msg}"
      fi
    else
      record FAIL "Priority 2 aggregated fallback (embedded H100 priors)" "CapacityPriorsReady!=True"
    fi
    kubectl -n "$NS" delete "$SCALER_RES" "$SC_P2_EMB" --ignore-not-found --wait=true >/dev/null 2>&1 || true
    kubectl -n "$NS" delete "$MD_RES" "$MD_H100" --ignore-not-found --wait=true >/dev/null 2>&1 || true
  else
    record SKIP "Priority 2 aggregated fallback (embedded H100 priors)" "hardware ${HW_PRODUCT} is not H100"
  fi

  # EPD (1E:2P:4D) coupling & pdScale = gcd(2,4) = 2.
  kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD_EPD}
  namespace: ${NS}
  annotations: {worker.gpustack.ai/precision: "none"}
spec:
  model: {name: Qwen/Qwen3-8B}
  engine: {name: vLLM, image: ${IMAGE}}
  router: {name: llm-d-router}
  roles:
    - {name: encode, kind: Encode, replicas: 1, acceleratorType: ${IT}, resources: {accelerator: "1"}}
    - {name: prefill, kind: Prefill, replicas: 2, acceleratorType: ${IT}, resources: {accelerator: "1"}}
    - {name: decode, kind: Decode, replicas: 4, acceleratorType: ${IT}, resources: {accelerator: "1"}}
---
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata: {name: ${SC_EPD}, namespace: ${NS}}
spec:
  targetRef: {name: ${MD_EPD}}
  mode: Shadow
  roles:
    - {name: encode, minReplicas: 1, maxReplicas: 2}
    - {name: prefill, minReplicas: 2, maxReplicas: 4}
    - {name: decode, minReplicas: 4, maxReplicas: 8}
  ratios:
    - {numeratorRole: encode, denominatorRole: prefill, target: "0.5", min: "0.5", max: "0.5"}
    - {numeratorRole: decode, denominatorRole: prefill, target: "2", min: "2", max: "2"}
  algorithm:
    type: TokenThroughput
    tokenThroughput: {ttftTargetMilliseconds: "500", itlTargetMilliseconds: "50"}
    capacityPrior: {catalogRef: {name: ${CM_DISAGG}}}
YAML
  if wait_scaler_cond_status "$SC_EPD" CapacityPriorsReady True; then
    epd_json="$(kubectl -n "$NS" get "$SCALER_RES" "$SC_EPD" -o json)"
    seed_p="$(printf '%s' "$epd_json" | jq -r '[(.status.resolvedCapacityPriors // [])[] | select(.name=="prefill") | .seedTokensPerSecond] | .[0] // ""')"
    eff_p="$(printf '%s' "$epd_json" | jq -r '[(.status.resolvedCapacityPriors // [])[] | select(.name=="prefill") | .fields[]? | .effectiveTokensPerSecond] | .[0] // ""')"
    mrep_p="$(printf '%s' "$epd_json" | jq -r '[(.status.resolvedCapacityPriors // [])[] | select(.name=="prefill") | .measuredReplicas] | .[0] // 0')"
    mrep_d="$(printf '%s' "$epd_json" | jq -r '[(.status.resolvedCapacityPriors // [])[] | select(.name=="decode") | .measuredReplicas] | .[0] // 0')"
    has_enc="$(printf '%s' "$epd_json" | jq -r '[(.status.resolvedCapacityPriors // [])[] | select(.name=="encode")] | length')"
    if qty_eq "$seed_p" 2000 && qty_eq "$eff_p" 1500 && [ "$mrep_p" = "1" ] && [ "$mrep_d" = "2" ] && [ "$has_enc" = "0" ]; then
      record PASS "EPD 1E:2P:4D coupling & pdScale=gcd(2,4)=2" "seed=${seed_p} eff=${eff_p} measured=(${mrep_p},${mrep_d})"
    else
      record FAIL "EPD 1E:2P:4D coupling & pdScale=gcd(2,4)=2" "seed=${seed_p} eff=${eff_p} measured=(${mrep_p},${mrep_d}) enc=${has_enc}"
    fi
  else
    record FAIL "EPD 1E:2P:4D coupling & pdScale=gcd(2,4)=2" "CapacityPriorsReady!=True"
  fi
  kubectl -n "$NS" delete "$SCALER_RES" "$SC_EPD" --ignore-not-found --wait=true >/dev/null 2>&1 || true
  kubectl -n "$NS" delete "$MD_RES" "$MD_EPD" --ignore-not-found --wait=true >/dev/null 2>&1 || true

  # UPDATE broken-ConfigMap warning + controller freeze.
  kubectl apply -f "${TMP_DIR}/sc-default.yaml" >/dev/null
  wait_scaler_cond_status "$SC_P1" CapacityPriorsReady True || true
  kubectl -n "$NS" patch configmap "$CM_DISAGG" --type=merge -p '{"data":{"priors.yaml":"{"}}' >/dev/null
  cat > "${TMP_DIR}/sc-update-broken.yaml" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata: {name: ${SC_P1}, namespace: ${NS}}
spec:
  targetRef: {name: ${MD_PD}}
  mode: Active
  roles: [{name: prefill, minReplicas: 1, maxReplicas: 3}, {name: decode, minReplicas: 2, maxReplicas: 6}]
  ratios: [{numeratorRole: decode, denominatorRole: prefill, target: "2", min: "2", max: "2"}]
  algorithm:
    type: TokenThroughput
    tokenThroughput: {ttftTargetMilliseconds: "500", itlTargetMilliseconds: "50"}
    capacityPrior: {headroom: "0.8", catalogRef: {name: ${CM_DISAGG}}}
YAML
  upd_rc=0
  upd_out="$(kubectl apply -f "${TMP_DIR}/sc-update-broken.yaml" 2>&1)" || upd_rc=$?
  if [ "$upd_rc" -eq 0 ] && printf '%s' "$upd_out" | grep -Fq "admitting update so the scaler can be repaired or placed in Shadow mode"; then
    record PASS "UPDATE broken-ConfigMap admission warning" "admitted with repair/Shadow warning"
  else
    record FAIL "UPDATE broken-ConfigMap admission warning" "rc=${upd_rc}, out=${upd_out:0:180}"
  fi
  if wait_scaler_cond_status "$SC_P1" CapacityPriorsReady False \
    && wait_scaler_cond_status "$SC_P1" Active False \
    && wait_scaler_cond_status "$SC_P1" Blocked True; then
    record PASS "UPDATE broken-ConfigMap controller freeze" "CapacityPriorsReady=False Active=False Blocked=True"
  else
    record FAIL "UPDATE broken-ConfigMap controller freeze" "did not reach freeze state"
  fi
fi

results
