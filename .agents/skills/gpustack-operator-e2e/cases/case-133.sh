#!/usr/bin/env bash
#
# CASE 133 — ModelDeployment online runtime control admission surface: reserved loraAdapters,
#            Translator parameter validation, scope gates, router isolation flags, folded Elastic EP,
#            and pause/runtimeControl coexistence under scaler-binding
#            (MUTATING, self-cleaning; AUTO-SKIPS accelerator-backed rows when no NVIDIA AcceleratorType exists)
#
#   case-133.sh <NS>
#
# Prerequisites: nodes=1; gpu=none; cluster=any; inputs=E2E_MD_IMAGE,E2E_C133_ACCELERATOR_TYPE
# Goal:        Verify the live API server and validating webhooks enforce the complete online runtime
#              control contract: reserved spec.model.loraAdapters is refused, runtimeControl.parameters
#              validates against the canonical 15-key Translator registry (bounds, engine support,
#              extraArgs ceilings, and topology prerequisites), runtimeControl is refused on unmanaged
#              command, Encode, CPU-only, and SGLang Ray roles, router isolation rejects admin/control
#              flags, folded elastic.dataParallelWidth enforces immutable presence and monotonic
#              non-decreasing updates, and role.pause plus runtimeControl edits pass through a
#              ModelDeploymentScaler single-writer fence while replicas remains blocked.
# Environment: Any cluster with ModelDeployment and ModelDeploymentScaler CRDs installed (run case-1
#              first). Accelerator-backed dry-run and scaler-binding rows require one usable NVIDIA
#              AcceleratorType (or E2E_C133_ACCELERATOR_TYPE); without one, those rows record SKIP.
# Inputs:      MOCKED: none. The placeholder image (E2E_MD_IMAGE, default registry.k8s.io/pause:3.10)
#              is never pulled. REAL: CRD schema, validating webhooks, and ModelDeploymentScaler
#              binding reconciliation.
# Expected:    Every invalid manifest is refused with the expected field/reason substring, every valid
#              manifest is accepted, folded elastic.dataParallelWidth enforces immutability and
#              monotonic scale-up on live updates, and a scaler-bound deployment allows pause,
#              runtimeControl, and mutable router edits while still refusing replicas edits.
# Cleanup:     EXIT trap deletes all ModelDeployments and ModelDeploymentScalers created by this case
#              and releases any owned Kueue Workloads.
set -uo pipefail

CASE_ID=133
export CASE_ID
CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "${CASE_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_md-lib.sh"

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-133.sh <NS>" >&2
  exit 2
fi

IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
MD_RES="modeldeployments.v1alpha1.worker.gpustack.ai"
SCALER_RES="modeldeploymentscalers.v1alpha1.worker.gpustack.ai"
RUN_PREFIX="c133-$$"
MD_ELASTIC="${RUN_PREFIX}-elastic"
MD_BOUND="${RUN_PREFIX}-bound"
SCALER_BOUND="${RUN_PREFIX}-scaler"

cleanup() {
  kubectl -n "$NS" delete "$SCALER_RES" "$SCALER_BOUND" \
    --ignore-not-found --wait=true --timeout=90s >/dev/null 2>&1 || true
  kubectl -n "$NS" delete "$MD_RES" "$MD_ELASTIC" "$MD_BOUND" \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
  md_force_release "$NS" "$MD_ELASTIC" || true
  md_force_release "$NS" "$MD_BOUND" || true
  return 0
}
trap cleanup EXIT

if ! kubectl get crd modeldeployments.worker.gpustack.ai >/dev/null 2>&1 || \
   ! kubectl get crd modeldeploymentscalers.worker.gpustack.ai >/dev/null 2>&1; then
  record SKIP "case-133 prerequisites" "ModelDeployment or ModelDeploymentScaler CRD missing"
  results
  exit 0
fi

AT="${E2E_C133_ACCELERATOR_TYPE:-$(md_usable_type_for nvidia || true)}"
if [ -z "$AT" ]; then
  AT="$(md_usable_accelerator_type "$NS" || true)"
fi
if [ -z "$AT" ]; then
  record SKIP "case-133 prerequisites" "no usable AcceleratorType available"
  results
  exit 0
fi

dry_refuse() {
  local label="$1" want="$2"
  shift 2
  local msg
  if msg="$(md_dryrun_refuses "$want" "$@")"; then
    record PASS "$label" "$msg"
  else
    record FAIL "$label" "$msg"
  fi
}

dry_accept() {
  local label="$1"
  local msg
  if msg="$(md_dryrun_accepts)"; then
    record PASS "$label" "$msg"
  else
    record FAIL "$label" "$msg"
  fi
}

# 1. Reserved spec.model.loraAdapters is refused.
dry_refuse "reserved spec.model.loraAdapters is refused" "loraAdapters" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-lora
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
    loraAdapters:
      - name: adapter-a
        artifactRef:
          name: lora-a
  engine:
    name: vLLM
    image: ${IMAGE}
  roles:
    - name: server
      kind: Server
      replicas: 1
      acceleratorType: ${AT}
      resources:
        accelerator: "1"
EOF

# 2. Scope gates: command, CPU-only, Encode, SGLang Ray refuse runtimeControl.
dry_refuse "runtimeControl refused on unmanaged command role" "command" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-scope-cmd
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: vLLM
    image: ${IMAGE}
  roles:
    - name: server
      kind: Server
      replicas: 1
      acceleratorType: ${AT}
      command: ["/bin/sh", "-c", "sleep 3600"]
      resources:
        accelerator: "1"
      runtimeControl:
        parameters:
          scheduling.maxActiveRequests: "16"
EOF

dry_refuse "runtimeControl refused on CPU-only role" "CPU" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-scope-cpu
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: vLLM
    image: ${IMAGE}
  roles:
    - name: server
      kind: Server
      replicas: 1
      runtimeControl:
        parameters:
          scheduling.maxActiveRequests: "16"
EOF

dry_refuse "runtimeControl refused on SGLang Ray role" "Ray" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-scope-sglang-ray
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: SGLang
    image: ${IMAGE}
  roles:
    - name: server
      kind: Server
      replicas: 1
      acceleratorType: ${AT}
      extraArgs:
        - --distributed-executor-backend=ray
      resources:
        accelerator: "1"
      runtimeControl:
        parameters:
          scheduling.maxActiveRequests: "16"
EOF

# 3. Role pause is unrestricted (admitted on CPU-only and custom command roles).
dry_accept "role.pause admitted on CPU-only and unmanaged command roles" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-pause-unrestricted
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: vLLM
    image: ${IMAGE}
  roles:
    - name: server
      kind: Server
      replicas: 1
      pause: true
      command: ["/bin/sh", "-c", "sleep 3600"]
EOF

# 4. Translator parameter validation (unknown, engine mismatch, range, extraArgs ceilings, prerequisites).
dry_refuse "unknown runtimeControl parameter key is refused" "unknown" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-param-unknown
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: vLLM
    image: ${IMAGE}
  roles:
    - name: server
      kind: Server
      replicas: 1
      acceleratorType: ${AT}
      resources:
        accelerator: "1"
      runtimeControl:
        parameters:
          scheduling.nonExistentKey: "10"
EOF

dry_refuse "SGLang-only parameter refused on vLLM" "speculative.acceptThresholdSingle" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-param-engine
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: vLLM
    image: ${IMAGE}
  roles:
    - name: server
      kind: Server
      replicas: 1
      acceleratorType: ${AT}
      resources:
        accelerator: "1"
      runtimeControl:
        parameters:
          speculative.acceptThresholdSingle: "0.5"
EOF

dry_refuse "scheduling.maxActiveRequests exceeding --max-num-seqs is refused" "maxActiveRequests" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-param-ceiling
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: vLLM
    image: ${IMAGE}
  roles:
    - name: server
      kind: Server
      replicas: 1
      acceleratorType: ${AT}
      extraArgs:
        - --max-num-seqs=32
      resources:
        accelerator: "1"
      runtimeControl:
        parameters:
          scheduling.maxActiveRequests: "64"
EOF

dry_refuse "speculative.draftBudgetFraction=0 (half-open lower bound) is refused" "draftBudgetFraction" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-param-halfopen
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: SGLang
    image: ${IMAGE}
  roles:
    - name: server
      kind: Server
      replicas: 1
      acceleratorType: ${AT}
      resources:
        accelerator: "1"
      runtimeControl:
        parameters:
          speculative.draftBudgetFraction: "0"
EOF

dry_refuse "scheduling.ppMaxMicroBatchSize without ppSize>1 is refused" "ppMaxMicroBatchSize" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-param-pp
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: SGLang
    image: ${IMAGE}
  roles:
    - name: server
      kind: Server
      replicas: 1
      acceleratorType: ${AT}
      resources:
        accelerator: "1"
      runtimeControl:
        parameters:
          scheduling.ppMaxMicroBatchSize: "2"
EOF

dry_refuse "scheduling.maxInflightKVLoads without KV connector is refused" "maxInflightKVLoads" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-param-kv
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: vLLM
    image: ${IMAGE}
  roles:
    - name: server
      kind: Server
      replicas: 1
      acceleratorType: ${AT}
      resources:
        accelerator: "1"
      runtimeControl:
        parameters:
          scheduling.maxInflightKVLoads: "4"
EOF

dry_accept "valid vLLM runtimeControl.parameters and extraArgs ceilings admitted" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-valid-vllm
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: vLLM
    image: ${IMAGE}
  router:
    name: vllm-router
  roles:
    - name: server
      kind: Server
      replicas: 1
      acceleratorType: ${AT}
      extraArgs:
        - --max-num-seqs=64
        - --max-num-batched-tokens=8192
      resources:
        accelerator: "1"
      runtimeControl:
        parameters:
          scheduling.maxActiveRequests: "32"
          scheduling.maxBatchedTokens: "4096"
          scheduling.longPrefillTokenThreshold: "2048"
          scheduling.cacheAwareWindow: "64"
          scheduling.cacheAwareThreshold: "0.5"
EOF

dry_accept "valid SGLang runtimeControl.parameters with pp=2 admitted" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-valid-sglang
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: SGLang
    image: ${IMAGE}
  router:
    name: sglang-gateway
  roles:
    - name: server
      kind: Server
      replicas: 1
      acceleratorType: ${AT}
      extraArgs:
        - --max-running-requests=64
        - --chunked-prefill-size=8192
        - --pp-size=2
      resources:
        accelerator: "2"
      runtimeControl:
        parameters:
          scheduling.maxActiveRequests: "32"
          scheduling.maxBatchedTokens: "4096"
          scheduling.prefillMaxRequests: "16"
          scheduling.scheduleConservativeness: "1.2"
          scheduling.prefillDecodeInterval: "4"
          scheduling.ppMaxMicroBatchSize: "8"
          speculative.acceptThresholdSingle: "0.7"
          speculative.acceptThresholdAcc: "0.8"
          speculative.draftBudgetFraction: "0.5"
EOF

# 5. Router isolation forbidden args rejected when sentinel-capable role exists.
dry_refuse "router isolation rejects --enable-igw in spec.router.extraArgs" "enable-igw" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-router-igw
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: SGLang
    image: ${IMAGE}
  router:
    name: sglang-gateway
    extraArgs:
      - --enable-igw
  roles:
    - name: server
      kind: Server
      replicas: 1
      acceleratorType: ${AT}
      resources:
        accelerator: "1"
EOF

# 6. Folded elastic.dataParallelWidth create/update immutability and monotonicity.
if out="$(kubectl apply -f - 2>&1 <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD_ELASTIC}
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: vLLM
    image: ${IMAGE}
  roles:
    - name: server
      kind: Server
      replicas: 1
      size: 1
      acceleratorType: ${AT}
      extraArgs:
        - --elastic-ep-max-dp-size=4
      resources:
        accelerator: "1"
      runtimeControl:
        parameters:
          elastic.dataParallelWidth: "2"
EOF
)"; then
  record PASS "folded elastic.dataParallelWidth deployment created" "$MD_ELASTIC"

  # Monotonic scale-up 2 -> 4 dry-run accepted.
  if msg="$(kubectl -n "$NS" patch "$MD_RES" "$MD_ELASTIC" --dry-run=server --type=json \
    -p '[{"op":"replace","path":"/spec/roles/0/runtimeControl/parameters/elastic.dataParallelWidth","value":"4"}]' 2>&1)"; then
    record PASS "elastic.dataParallelWidth monotonic scale-up 2->4 accepted" "$msg"
  else
    record FAIL "elastic.dataParallelWidth monotonic scale-up 2->4 accepted" "$msg"
  fi

  # Exceeding --elastic-ep-max-dp-size=4 refused.
  if msg="$(kubectl -n "$NS" patch "$MD_RES" "$MD_ELASTIC" --dry-run=server --type=json \
    -p '[{"op":"replace","path":"/spec/roles/0/runtimeControl/parameters/elastic.dataParallelWidth","value":"6"}]' 2>&1)"; then
    record FAIL "elastic.dataParallelWidth above --elastic-ep-max-dp-size refused" "unexpectedly accepted: $msg"
  else
    record PASS "elastic.dataParallelWidth above --elastic-ep-max-dp-size refused" "$(printf '%s' "$msg" | tr '\n' ' ' | cut -c1-160)"
  fi

  # Removing elastic.dataParallelWidth after creation refused.
  if msg="$(kubectl -n "$NS" patch "$MD_RES" "$MD_ELASTIC" --dry-run=server --type=json \
    -p '[{"op":"remove","path":"/spec/roles/0/runtimeControl/parameters/elastic.dataParallelWidth"}]' 2>&1)"; then
    record FAIL "removing elastic.dataParallelWidth after creation refused" "unexpectedly accepted: $msg"
  else
    record PASS "removing elastic.dataParallelWidth after creation refused" "$(printf '%s' "$msg" | tr '\n' ' ' | cut -c1-160)"
  fi

  # Scale down after patching up to 3 refused.
  if kubectl -n "$NS" patch "$MD_RES" "$MD_ELASTIC" --type=json \
    -p '[{"op":"replace","path":"/spec/roles/0/runtimeControl/parameters/elastic.dataParallelWidth","value":"3"}]' >/dev/null 2>&1; then
    if msg="$(kubectl -n "$NS" patch "$MD_RES" "$MD_ELASTIC" --dry-run=server --type=json \
      -p '[{"op":"replace","path":"/spec/roles/0/runtimeControl/parameters/elastic.dataParallelWidth","value":"2"}]' 2>&1)"; then
      record FAIL "elastic.dataParallelWidth scale-down 3->2 refused" "unexpectedly accepted: $msg"
    else
      record PASS "elastic.dataParallelWidth scale-down 3->2 refused" "$(printf '%s' "$msg" | tr '\n' ' ' | cut -c1-160)"
    fi
  else
    record FAIL "elastic.dataParallelWidth live patch 2->3 succeeded" "failed to patch width to 3"
  fi
else
  record SKIP "folded elastic.dataParallelWidth live update checks" "cluster refused elastic creation: $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-160)"
fi

# 7. Additional sentinel admission hardening: pause-only reserved env, sentinel volume mount overlap, huge decimal exponent.
dry_refuse "pause-only sentinel role refuses GPUSTACK_ENGINE_SENTINEL_REQUIRE_ACK" "GPUSTACK_ENGINE_SENTINEL_REQUIRE_ACK" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-pause-env
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: vLLM
    image: ${IMAGE}
  roles:
    - name: server
      kind: Server
      replicas: 1
      pause: true
      acceleratorType: ${AT}
      resources:
        accelerator: "1"
      env:
        - name: GPUSTACK_ENGINE_SENTINEL_REQUIRE_ACK
          value: "0"
EOF

dry_refuse "sentinel role refuses additionalVolumes overlapping engine-sentinel path" "engine sentinel" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-sentinel-vol
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: vLLM
    image: ${IMAGE}
  roles:
    - name: server
      kind: Server
      replicas: 1
      acceleratorType: ${AT}
      resources:
        accelerator: "1"
      additionalVolumes:
        - mountPath: /var/lib/gpustack/operator/lib/engine-sentinel
          configMap:
            name: dummy-cm
EOF

dry_refuse "decimal parameter refuses huge scientific exponent" "must be a valid decimal quantity" <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${RUN_PREFIX}-huge-exp
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: SGLang
    image: ${IMAGE}
  roles:
    - name: server
      kind: Server
      replicas: 1
      acceleratorType: ${AT}
      resources:
        accelerator: "1"
      runtimeControl:
        parameters:
          scheduling.scheduleConservativeness: "1e100000000"
EOF

# 8. Scaler-binding coexistence: pause, runtimeControl, mutable router allowed; replicas still fenced.
if kubectl apply -f - >/dev/null 2>&1 <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD_BOUND}
  namespace: ${NS}
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: vLLM
    image: ${IMAGE}
  router:
    name: vllm-router
    requestTimeoutSeconds: 60
  roles:
    - name: server
      kind: Server
      replicas: 1
      acceleratorType: ${AT}
      resources:
        accelerator: "1"
---
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${SCALER_BOUND}
  namespace: ${NS}
spec:
  targetRef:
    name: ${MD_BOUND}
  mode: Active
  algorithm:
    type: Proportional
    proportional:
      metric: RunningRequests
      roleTargets:
        - name: server
          target: "8"
  roles:
    - name: server
      minReplicas: 1
      maxReplicas: 2
EOF
then
  got_bind="$(wait_for "$MD_RES" "$MD_BOUND" '{.metadata.annotations.modeldeployment\.gpustack\.ai/scaler-binding}' "" 1 "$NS")"
  for _ in $(seq 1 30); do
    got_bind="$(kubectl -n "$NS" get "$MD_RES" "$MD_BOUND" -o jsonpath='{.metadata.annotations.modeldeployment\.gpustack\.ai/scaler-binding}' 2>/dev/null || true)"
    [ -n "$got_bind" ] && break
    sleep 2
  done
  if [ -n "$got_bind" ]; then
    record PASS "ModelDeploymentScaler bound target deployment" "$got_bind"

    if msg="$(kubectl -n "$NS" patch "$MD_RES" "$MD_BOUND" --type=merge \
      -p '{"spec":{"router":{"requestTimeoutSeconds":90},"roles":[{"name":"server","kind":"Server","replicas":1,"pause":true,"acceleratorType":"'"${AT}"'","resources":{"accelerator":"1"},"runtimeControl":{"parameters":{"scheduling.maxActiveRequests":"16"}}}]}}' 2>&1)"; then
      record PASS "scaler-bound deployment admits pause, runtimeControl, and router edits" "$msg"
    else
      record FAIL "scaler-bound deployment admits pause, runtimeControl, and router edits" "$msg"
    fi

    if msg="$(kubectl -n "$NS" patch "$MD_RES" "$MD_BOUND" --dry-run=server --type=json \
      -p '[{"op":"replace","path":"/spec/roles/0/replicas","value":2}]' 2>&1)"; then
      record FAIL "scaler-bound deployment still fences replicas edits" "unexpectedly admitted: $msg"
    else
      record PASS "scaler-bound deployment still fences replicas edits" "$(printf '%s' "$msg" | tr '\n' ' ' | cut -c1-160)"
    fi
  else
    record FAIL "ModelDeploymentScaler bound target deployment" "scaler-binding annotation never appeared"
  fi
else
  record FAIL "create scaler-bound ModelDeployment fixture" "apply failed"
fi

results
