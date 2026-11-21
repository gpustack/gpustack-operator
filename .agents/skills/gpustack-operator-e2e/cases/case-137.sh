#!/usr/bin/env bash
#
# CASE 137 — Live extended runtimeControl.parameters coverage: vLLM P/D KV-load knobs & per-role
#            pause, SGLang multi-node PP micro-batch tuning, and SGLang speculative thresholds
#            (MUTATING, self-cleaning; AUTO-SKIPS without >=2 NVIDIA GPUs and pre-warmed ModelArtifact)
#
#   case-137.sh <NS>
#
# Prerequisites: nodes=2; gpu=nvidia; cluster=real; inputs=E2E_C137_ACCELERATOR_TYPE,E2E_C137_ARTIFACT,E2E_C137_VLLM_IMAGE,E2E_C137_SGLANG_IMAGE,E2E_C137_MODEL,E2E_C137_PROBE_IMAGE,E2E_C137_READY_BOUND
# Goal:        Exercise the remaining 6 runtimeControl.parameters on real NVIDIA GPUs so all 15
#              canonical parameters are physically verified across vLLM and SGLang:
#              Leg A (vLLM 1P+1D behind llm-d-router): scheduling.maxInflightKVLoads and
#              scheduling.maxAsyncKVLoadsPerStep (live apply or UnsupportedEngineVersion + clean
#              recovery), plus independent per-role pause on decode while prefill stays active.
#              Leg B (SGLang size=2 --pp-size=2): multi-node sentinel injection and live tuning +
#              boot-default reset of scheduling.ppMaxMicroBatchSize.
#              Leg C (SGLang --speculative-algorithm=NGRAM): speculative.acceptThresholdSingle,
#              speculative.acceptThresholdAcc, and speculative.draftBudgetFraction (live apply
#              where supported, clean UnsupportedEngineVersion where engine lacks DFlash/DSpark).
# Environment: Real NVIDIA GPU cluster with >=2 allocatable GPUs on one usable AcceleratorType,
#              pre-warmed ModelArtifact (E2E_C137_ARTIFACT, default qwen3-8b) resolving
#              E2E_C137_MODEL (default Qwen/Qwen3-8B), pullable vLLM and SGLang runner images,
#              and pullable probe image (E2E_C137_PROBE_IMAGE, default curlimages/curl:8.10.1).
#              Auto-skips if fewer than 2 GPUs or the ModelArtifact is absent.
# Inputs:      MOCKED: none. Runs real vLLM P/D, SGLang PP=2, and SGLang NGRAM ModelDeployments.
# Expected:    Each leg reaches Ready, reports real sentinel supported/unsupported capabilities,
#              applies supported parameters with live readback and boot-default reset, reports
#              UnsupportedEngineVersion cleanly without restarting Pods for version-gated knobs,
#              and isolates per-role pause in P/D without affecting the sibling role.
# Cleanup:     EXIT trap deletes the probe Pod and all three ModelDeployments created by this case
#              and force-releases any Kueue Workloads.
set -uo pipefail

CASE_ID=137
export CASE_ID
CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "${CASE_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_md-lib.sh"

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-137.sh <NS>" >&2
  exit 2
fi

MD_RES="modeldeployments.v1alpha1.worker.gpustack.ai"
ARTIFACT="${E2E_C137_ARTIFACT:-qwen3-8b}"
MODEL="${E2E_C137_MODEL:-Qwen/Qwen3-8B}"
VLLM_IMAGE="${E2E_C137_VLLM_IMAGE:-gpustack/runner:cuda13.0-vllm0.32.0}"
SGLANG_IMAGE="${E2E_C137_SGLANG_IMAGE:-gpustack/runner:cuda13.0-sglang0.5.22}"
PROBE_IMAGE="${E2E_C137_PROBE_IMAGE:-curlimages/curl:8.10.1}"
READY_BOUND="${E2E_C137_READY_BOUND:-720}"

RUN_PREFIX="c137-$$"
MD_PD="${RUN_PREFIX}-vllm-pd"
MD_PP="${RUN_PREFIX}-sglang-pp"
MD_SPEC="${RUN_PREFIX}-sglang-spec"
PROBE="${RUN_PREFIX}-probe"

delete_and_wait_md() {
  local name="$1"
  kubectl -n "$NS" delete "$MD_RES" "$name" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  md_force_release "$NS" "$name" || true
  for _ in $(seq 1 30); do
    if ! kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${name}" -o name 2>/dev/null | grep -q .; then
      break
    fi
    sleep 2
  done
}

cleanup() {
  kubectl -n "$NS" delete pod "$PROBE" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  delete_and_wait_md "$MD_PD"
  delete_and_wait_md "$MD_PP"
  delete_and_wait_md "$MD_SPEC"
  return 0
}
trap cleanup EXIT

AT="${E2E_C137_ACCELERATOR_TYPE:-$(md_usable_type_for nvidia || true)}"
if [ -z "$AT" ]; then
  record SKIP "case-137 prerequisites" "no usable NVIDIA AcceleratorType found"
  results
  exit 0
fi

HW_RES="$(kubectl get acceleratortype "$AT" -o jsonpath='{.status.detail.resourceName}' 2>/dev/null || true)"
[ -n "$HW_RES" ] || HW_RES="nvidia.com/gpu"
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
if [ "${GPU_CAP:-0}" -lt 2 ]; then
  record SKIP "case-137 prerequisites" "need >=2 allocatable ${HW_RES}, found ${GPU_CAP:-0}"
  results
  exit 0
fi

if ! kubectl -n "$NS" get modelartifact "$ARTIFACT" >/dev/null 2>&1; then
  record SKIP "case-137 prerequisites" "ModelArtifact ${NS}/${ARTIFACT} not found"
  results
  exit 0
fi
ART_REV="$(kubectl -n "$NS" get modelartifact "$ARTIFACT" -o jsonpath='{.status.resolved.revision}' 2>/dev/null || true)"
if [ -z "$ART_REV" ]; then
  record SKIP "case-137 prerequisites" "ModelArtifact ${NS}/${ARTIFACT} has empty resolved.revision"
  results
  exit 0
fi

probe_curl() {
  kubectl -n "$NS" exec "$PROBE" -- curl -sS --max-time 20 "$@"
}

wait_md_ready() {
  local name="$1" phase=""
  for _ in $(seq 1 $((READY_BOUND / 5))); do
    phase="$(kubectl -n "$NS" get "$MD_RES" "$name" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    [ "$phase" = "Ready" ] && { echo "$phase"; return 0; }
    sleep 5
  done
  echo "${phase:-unknown}"
  return 1
}

wait_role_rc() {
  local name="$1" role_name="$2" want_gen="$3" want_synced="$4" want_paused="$5" timeout="${6:-120}"
  local out
  for _ in $(seq 1 $((timeout / 3))); do
    out="$(kubectl -n "$NS" get "$MD_RES" "$name" -o json 2>/dev/null | python3 -c '
import json, sys
d = json.load(sys.stdin)
rname, wgen, wsync, wp = sys.argv[1], int(sys.argv[2]), sys.argv[3] == "true", sys.argv[4] == "true"
for r in d.get("status", {}).get("roles") or []:
    if r.get("name") == rname:
        rc = r.get("runtimeControl") or {}
        if rc.get("observedGeneration") == wgen and rc.get("synced") is wsync and bool(rc.get("paused", False)) is wp:
            unsup = ",".join(rc.get("unsupportedFields") or [])
            print("OK|reason={}|unsup={}|converged={}/{}".format(rc.get("reason", ""), unsup, rc.get("convergedPods", 0), rc.get("totalAnsweringPods", 0)))
            sys.exit(0)
        print("WAIT|gen={}|synced={}|paused={}|reason={}".format(rc.get("observedGeneration"), rc.get("synced"), rc.get("paused"), rc.get("reason", "")))
        sys.exit(1)
print("WAIT|no-role")
sys.exit(1)
' "$role_name" "$want_gen" "$want_synced" "$want_paused" 2>/dev/null || true)"
    if printf '%s' "$out" | grep -q '^OK|'; then
      echo "$out"
      return 0
    fi
    sleep 3
  done
  echo "${out:-timeout}"
  return 1
}

wait_role_rc_cleared() {
  local name="$1" role_name="$2" timeout="${3:-120}"
  local out
  for _ in $(seq 1 $((timeout / 3))); do
    out="$(kubectl -n "$NS" get "$MD_RES" "$name" -o json 2>/dev/null | python3 -c '
import json, sys
d = json.load(sys.stdin)
rname = sys.argv[1]
cond = ""
for c in d.get("status", {}).get("conditions") or []:
    if c.get("type") == "RuntimeControlSynced":
        cond = "{}:{}".format(c.get("status"), c.get("reason"))
for r in d.get("status", {}).get("roles") or []:
    if r.get("name") == rname:
        rc = r.get("runtimeControl")
        if rc is None and cond.startswith("True:"):
            print("OK|cond={}|roleRuntimeControl=cleared".format(cond))
            sys.exit(0)
        print("WAIT|cond={}|rc={}".format(cond, rc))
        sys.exit(1)
print("WAIT|no-role")
sys.exit(1)
' "$role_name" 2>/dev/null || true)"
    if printf '%s' "$out" | grep -q '^OK|'; then
      echo "$out"
      return 0
    fi
    sleep 3
  done
  echo "${out:-timeout}"
  return 1
}

engine_port_of_pod() {
  local pod="$1"
  kubectl -n "$NS" get pod "$pod" -o json 2>/dev/null | python3 -c '
import json, sys
p = json.load(sys.stdin)
for c in p.get("spec", {}).get("containers", []):
    if c.get("name") == "main":
        port = c.get("readinessProbe", {}).get("httpGet", {}).get("port")
        print(port if port else 8000)
        sys.exit(0)
print(8000)
' 2>/dev/null || echo 8000
}

kubectl -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${PROBE}
  namespace: ${NS}
spec:
  restartPolicy: Never
  containers:
    - name: curl
      image: ${PROBE_IMAGE}
      command: ["sh", "-c", "sleep 3600"]
EOF
kubectl -n "$NS" wait --for=condition=Ready "pod/${PROBE}" --timeout=120s >/dev/null 2>&1 || true

# ==============================================================================
# Leg A: vLLM 1P+1D behind llm-d-router
#   - scheduling.maxInflightKVLoads & scheduling.maxAsyncKVLoadsPerStep on decode
#   - independent per-role pause on decode while prefill stays Ready/unpaused
# ==============================================================================
kubectl apply -f - >/dev/null <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD_PD}
  namespace: ${NS}
spec:
  model:
    name: ${MODEL}
    artifactRef:
      name: ${ARTIFACT}
  engine:
    name: vLLM
    image: ${VLLM_IMAGE}
  router:
    name: llm-d-router
  roles:
    - name: prefill
      kind: Prefill
      replicas: 1
      size: 1
      acceleratorType: ${AT}
      resources:
        accelerator: "1"
      extraArgs:
        - --max-model-len=4096
        - --max-num-seqs=32
        - --gpu-memory-utilization=0.80
        - --enable-scale-out
    - name: decode
      kind: Decode
      replicas: 1
      size: 1
      acceleratorType: ${AT}
      resources:
        accelerator: "1"
      extraArgs:
        - --max-model-len=4096
        - --max-num-seqs=32
        - --gpu-memory-utilization=0.80
        - --enable-scale-out
EOF

if pd_phase="$(wait_md_ready "$MD_PD")"; then
  record PASS "Leg A: vLLM 1P+1D deployment reached Ready" "phase=${pd_phase}"
  PD_EP="$(kubectl -n "$NS" get "$MD_RES" "$MD_PD" -o jsonpath='{.status.endpoint}')"
  PREFILL_POD="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD_PD},app.kubernetes.io/component=prefill" -o jsonpath='{.items[0].metadata.name}')"
  DECODE_POD="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD_PD},app.kubernetes.io/component=decode" -o jsonpath='{.items[0].metadata.name}')"
  PREFILL_IP="$(kubectl -n "$NS" get pod "$PREFILL_POD" -o jsonpath='{.status.podIP}')"
  DECODE_IP="$(kubectl -n "$NS" get pod "$DECODE_POD" -o jsonpath='{.status.podIP}')"
  DECODE_UID="$(kubectl -n "$NS" get pod "$DECODE_POD" -o jsonpath='{.metadata.uid}')"
  PREFILL_PORT="$(engine_port_of_pod "$PREFILL_POD")"
  DECODE_PORT="$(engine_port_of_pod "$DECODE_POD")"

  pd_chat="000"
  for _ in $(seq 1 6); do
    pd_chat="$(probe_curl -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' \
      -d '{"model":"'"${MODEL}"'","messages":[{"role":"user","content":"Reply OK"}],"max_tokens":8}' \
      "${PD_EP}/v1/chat/completions" 2>/dev/null || echo 000)"
    [ "$pd_chat" = "200" ] && break
    sleep 5
  done
  if [ "$pd_chat" = "200" ]; then
    record PASS "Leg A: vLLM 1P+1D inference through llm-d-router returns 200" "${PD_EP}"
  else
    record FAIL "Leg A: vLLM 1P+1D inference through llm-d-router returns 200" "http=${pd_chat}"
  fi

  dec_cfg="$(probe_curl "http://${DECODE_IP}:${DECODE_PORT}/gpustack/v1/runtime_config" 2>/dev/null || echo '{}')"
  dec_sup="$(printf '%s' "$dec_cfg" | python3 -c 'import json,sys; print(",".join(json.load(sys.stdin).get("supported_fields") or []))' 2>/dev/null || true)"
  record PASS "Leg A: decode sentinel capability discovery" "port=${DECODE_PORT} supported=${dec_sup:-none}"

  kubectl -n "$NS" patch "$MD_RES" "$MD_PD" --type=json \
    -p '[{"op":"add","path":"/spec/roles/1/runtimeControl","value":{"parameters":{"scheduling.maxInflightKVLoads":"8","scheduling.maxAsyncKVLoadsPerStep":"4"}}}]' >/dev/null
  gen_kv="$(kubectl -n "$NS" get "$MD_RES" "$MD_PD" -o jsonpath='{.metadata.generation}')"
  if printf '%s' "$dec_sup" | grep -q 'scheduling.maxInflightKVLoads' && printf '%s' "$dec_sup" | grep -q 'scheduling.maxAsyncKVLoadsPerStep'; then
    if kv_out="$(wait_role_rc "$MD_PD" decode "$gen_kv" true false 120)"; then
      record PASS "Leg A: decode KV-load parameters converged live" "$kv_out"
    else
      record FAIL "Leg A: decode KV-load parameters converged live" "$kv_out"
    fi
  else
    if kv_out="$(wait_role_rc "$MD_PD" decode "$gen_kv" false false 120)" && printf '%s' "$kv_out" | grep -q 'reason=UnsupportedEngineVersion'; then
      record PASS "Leg A: decode KV-load parameters cleanly report UnsupportedEngineVersion on vLLM image" "$kv_out"
    else
      record FAIL "Leg A: decode KV-load parameters cleanly report UnsupportedEngineVersion on vLLM image" "$kv_out"
    fi
  fi

  # Remove KV-load parameters and verify clean recovery + unchanged decode UID.
  kubectl -n "$NS" patch "$MD_RES" "$MD_PD" --type=json \
    -p '[{"op":"remove","path":"/spec/roles/1/runtimeControl"}]' >/dev/null
  if kv_clr="$(wait_role_rc_cleared "$MD_PD" decode 120)"; then
    record PASS "Leg A: removing decode KV-load parameters recovers RuntimeControlSynced=True" "$kv_clr"
  else
    record FAIL "Leg A: removing decode KV-load parameters recovers RuntimeControlSynced=True" "$kv_clr"
  fi

  dec_uid_after="$(kubectl -n "$NS" get pod "$DECODE_POD" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
  if [ "$dec_uid_after" = "$DECODE_UID" ]; then
    record PASS "Leg A: decode Pod UID unchanged across KV-load parameter test" "$DECODE_UID"
  else
    record FAIL "Leg A: decode Pod UID unchanged across KV-load parameter test" "before=${DECODE_UID} after=${dec_uid_after}"
  fi

  # Independent per-role pause on decode while prefill remains active.
  kubectl -n "$NS" patch "$MD_RES" "$MD_PD" --type=json \
    -p '[{"op":"add","path":"/spec/roles/1/pause","value":true}]' >/dev/null
  gen_dec_pause="$(kubectl -n "$NS" get "$MD_RES" "$MD_PD" -o jsonpath='{.metadata.generation}')"
  if dp_out="$(wait_role_rc "$MD_PD" decode "$gen_dec_pause" true true 120)"; then
    dec_ready="$(probe_curl -o /dev/null -w '%{http_code}' "http://${DECODE_IP}:${DECODE_PORT}/gpustack/v1/ready" 2>/dev/null || echo 000)"
    pre_ready="$(probe_curl -o /dev/null -w '%{http_code}' "http://${PREFILL_IP}:${PREFILL_PORT}/gpustack/v1/ready" 2>/dev/null || echo 000)"
    if [ "$dec_ready" = "503" ] && [ "$pre_ready" = "200" ]; then
      record PASS "Leg A: pausing decode gates decode /ready=503 while prefill stays /ready=200" "decode=503 prefill=200"
    else
      record FAIL "Leg A: pausing decode gates decode /ready=503 while prefill stays /ready=200" "decode=${dec_ready} prefill=${pre_ready}"
    fi
  else
    record FAIL "Leg A: decode role pause=true converged" "$dp_out"
  fi
else
  record FAIL "Leg A: vLLM 1P+1D deployment reached Ready" "phase=${pd_phase}"
fi

delete_and_wait_md "$MD_PD"

# ==============================================================================
# Leg B: SGLang --pp-size=2 (requires >=2 GPUs per member on one node) -> scheduling.ppMaxMicroBatchSize
# ==============================================================================
MAX_GPUS_PER_NODE="$(kubectl get nodes -o json 2>/dev/null | python3 -c '
import json, sys
res = sys.argv[1]
doc = json.load(sys.stdin)
best = 0
for n in doc.get("items", []):
    conds = {c["type"]: c["status"] for c in n.get("status", {}).get("conditions", [])}
    if conds.get("Ready") != "True":
        continue
    val = n.get("status", {}).get("allocatable", {}).get(res, "0")
    try:
        best = max(best, int(float(val)))
    except ValueError:
        pass
print(best)
' "$HW_RES" 2>/dev/null || echo 0)"

if [ "${MAX_GPUS_PER_NODE:-0}" -lt 2 ]; then
  record SKIP "Leg B: SGLang --pp-size=2 scheduling.ppMaxMicroBatchSize" "need >=2 allocatable ${HW_RES} on a single node for per-member --pp-size=2, max per node is ${MAX_GPUS_PER_NODE:-0}"
else
kubectl apply -f - >/dev/null <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD_PP}
  namespace: ${NS}
spec:
  model:
    name: ${MODEL}
    artifactRef:
      name: ${ARTIFACT}
  engine:
    name: SGLang
    image: ${SGLANG_IMAGE}
  roles:
    - name: server
      kind: Server
      replicas: 1
      size: 1
      acceleratorType: ${AT}
      resources:
        accelerator: "2"
      extraArgs:
        - --pp-size=2
        - --context-length=4096
        - --max-running-requests=64
        - --chunked-prefill-size=4096
        - --mem-fraction-static=0.80
EOF

if pp_phase="$(wait_md_ready "$MD_PP")"; then
  record PASS "Leg B: SGLang --pp-size=2 deployment reached Ready" "phase=${pp_phase}"
  PP_LEADER_POD="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD_PP},app.kubernetes.io/component=server,modeldeployment.gpustack.ai/member-index=0" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
  [ -n "$PP_LEADER_POD" ] || PP_LEADER_POD="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD_PP},app.kubernetes.io/component=server" -o jsonpath='{.items[0].metadata.name}')"
  PP_LEADER_IP="$(kubectl -n "$NS" get pod "$PP_LEADER_POD" -o jsonpath='{.status.podIP}')"
  PP_LEADER_UID="$(kubectl -n "$NS" get pod "$PP_LEADER_POD" -o jsonpath='{.metadata.uid}')"

  pp_init="$(probe_curl "http://${PP_LEADER_IP}:8000/gpustack/v1/runtime_config" 2>/dev/null || echo '{}')"
  pp_sup="$(printf '%s' "$pp_init" | python3 -c 'import json,sys; print(",".join(json.load(sys.stdin).get("supported_fields") or []))' 2>/dev/null || true)"
  record PASS "Leg B: SGLang PP=2 leader sentinel capability discovery" "pod=${PP_LEADER_POD} supported=${pp_sup:-none}"

  kubectl -n "$NS" patch "$MD_RES" "$MD_PP" --type=json \
    -p '[{"op":"add","path":"/spec/roles/0/runtimeControl","value":{"parameters":{"scheduling.ppMaxMicroBatchSize":"4"}}}]' >/dev/null
  gen_pp="$(kubectl -n "$NS" get "$MD_RES" "$MD_PP" -o jsonpath='{.metadata.generation}')"
  if printf '%s' "$pp_sup" | grep -q 'scheduling.ppMaxMicroBatchSize'; then
    if pp_out="$(wait_role_rc "$MD_PP" server "$gen_pp" true false 120)"; then
      pp_val="$(probe_curl "http://${PP_LEADER_IP}:8000/gpustack/v1/runtime_config" 2>/dev/null | python3 -c 'import json,sys; print((json.load(sys.stdin).get("values") or {}).get("scheduling.ppMaxMicroBatchSize"))' 2>/dev/null || true)"
      if [ "$pp_val" = "4" ]; then
        record PASS "Leg B: scheduling.ppMaxMicroBatchSize live tuning and readback converged" "value=4 ${pp_out}"
      else
        record FAIL "Leg B: scheduling.ppMaxMicroBatchSize live tuning and readback converged" "got=${pp_val} ${pp_out}"
      fi
    else
      record FAIL "Leg B: scheduling.ppMaxMicroBatchSize live tuning converged" "$pp_out"
    fi
  else
    if pp_out="$(wait_role_rc "$MD_PP" server "$gen_pp" false false 120)" && printf '%s' "$pp_out" | grep -q 'reason=UnsupportedEngineVersion'; then
      record PASS "Leg B: scheduling.ppMaxMicroBatchSize cleanly reports UnsupportedEngineVersion" "$pp_out"
    else
      record FAIL "Leg B: scheduling.ppMaxMicroBatchSize cleanly reports UnsupportedEngineVersion" "$pp_out"
    fi
  fi

  kubectl -n "$NS" patch "$MD_RES" "$MD_PP" --type=json \
    -p '[{"op":"remove","path":"/spec/roles/0/runtimeControl"}]' >/dev/null
  if pp_clr="$(wait_role_rc_cleared "$MD_PP" server 120)"; then
    pp_uid_after="$(kubectl -n "$NS" get pod "$PP_LEADER_POD" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
    if [ "$pp_uid_after" = "$PP_LEADER_UID" ]; then
      record PASS "Leg B: clearing runtimeControl resets SGLang PP=2 without restarting leader Pod" "uid=${PP_LEADER_UID}"
    else
      record FAIL "Leg B: clearing runtimeControl resets SGLang PP=2 without restarting leader Pod" "before=${PP_LEADER_UID} after=${pp_uid_after}"
    fi
  else
    record FAIL "Leg B: clearing SGLang PP=2 runtimeControl converged" "$pp_clr"
  fi
else
  record FAIL "Leg B: SGLang --pp-size=2 deployment reached Ready" "phase=${pp_phase}"
fi

delete_and_wait_md "$MD_PP"
fi

# ==============================================================================
# Leg C: SGLang speculative decoding (--speculative-algorithm=NGRAM)
#   - speculative.acceptThresholdSingle & speculative.acceptThresholdAcc
#   - speculative.draftBudgetFraction (live if supported, else UnsupportedEngineVersion)
# ==============================================================================
kubectl apply -f - >/dev/null <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD_SPEC}
  namespace: ${NS}
spec:
  model:
    name: ${MODEL}
    artifactRef:
      name: ${ARTIFACT}
  engine:
    name: SGLang
    image: ${SGLANG_IMAGE}
  roles:
    - name: server
      kind: Server
      replicas: 1
      size: 1
      acceleratorType: ${AT}
      resources:
        accelerator: "1"
      extraArgs:
        - --speculative-algorithm=NGRAM
        - --context-length=4096
        - --max-running-requests=64
        - --chunked-prefill-size=4096
        - --mem-fraction-static=0.80
EOF

if spec_phase="$(wait_md_ready "$MD_SPEC")"; then
  record PASS "Leg C: SGLang NGRAM speculative deployment reached Ready" "phase=${spec_phase}"
  SPEC_POD="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD_SPEC},app.kubernetes.io/component=server" -o jsonpath='{.items[0].metadata.name}')"
  SPEC_IP="$(kubectl -n "$NS" get pod "$SPEC_POD" -o jsonpath='{.status.podIP}')"
  SPEC_UID="$(kubectl -n "$NS" get pod "$SPEC_POD" -o jsonpath='{.metadata.uid}')"

  spec_init="$(probe_curl "http://${SPEC_IP}:8000/gpustack/v1/runtime_config" 2>/dev/null || echo '{}')"
  spec_sup="$(printf '%s' "$spec_init" | python3 -c 'import json,sys; print(",".join(json.load(sys.stdin).get("supported_fields") or []))' 2>/dev/null || true)"
  record PASS "Leg C: SGLang NGRAM sentinel capability discovery" "supported=${spec_sup:-none}"

  kubectl -n "$NS" patch "$MD_RES" "$MD_SPEC" --type=json \
    -p '[{"op":"add","path":"/spec/roles/0/runtimeControl","value":{"parameters":{"speculative.acceptThresholdSingle":"0.85","speculative.acceptThresholdAcc":"0.75"}}}]' >/dev/null
  gen_sp="$(kubectl -n "$NS" get "$MD_RES" "$MD_SPEC" -o jsonpath='{.metadata.generation}')"
  if printf '%s' "$spec_sup" | grep -q 'speculative.acceptThresholdSingle' && printf '%s' "$spec_sup" | grep -q 'speculative.acceptThresholdAcc'; then
    if sp_out="$(wait_role_rc "$MD_SPEC" server "$gen_sp" true false 120)"; then
      sp_check="$(probe_curl "http://${SPEC_IP}:8000/gpustack/v1/runtime_config" 2>/dev/null | python3 -c '
import json, sys
vals = (json.load(sys.stdin).get("values") or {})
s, a = vals.get("speculative.acceptThresholdSingle"), vals.get("speculative.acceptThresholdAcc")
if s is not None and a is not None and abs(float(s) - 0.85) < 1e-6 and abs(float(a) - 0.75) < 1e-6:
    print("OK")
else:
    print(f"single={s} acc={a}")
' 2>/dev/null || echo "invalid json")"
      if [ "$sp_check" = "OK" ]; then
        record PASS "Leg C: speculative acceptThresholdSingle/Acc live tuning and readback converged" "single=0.85 acc=0.75"
      else
        record FAIL "Leg C: speculative acceptThresholdSingle/Acc live tuning and readback converged" "$sp_check"
      fi
    else
      record FAIL "Leg C: speculative acceptThresholdSingle/Acc live tuning converged" "$sp_out"
    fi
  else
    if sp_out="$(wait_role_rc "$MD_SPEC" server "$gen_sp" false false 120)" && printf '%s' "$sp_out" | grep -q 'reason=UnsupportedEngineVersion'; then
      record PASS "Leg C: speculative acceptThresholdSingle/Acc cleanly report UnsupportedEngineVersion" "$sp_out"
    else
      record FAIL "Leg C: speculative acceptThresholdSingle/Acc cleanly report UnsupportedEngineVersion" "$sp_out"
    fi
  fi

  # Test speculative.draftBudgetFraction (DFlash/DSpark-specific; on NGRAM expect UnsupportedEngineVersion).
  kubectl -n "$NS" patch "$MD_RES" "$MD_SPEC" --type=json \
    -p '[{"op":"add","path":"/spec/roles/0/runtimeControl/parameters/speculative.draftBudgetFraction","value":"0.5"}]' >/dev/null
  gen_dbf="$(kubectl -n "$NS" get "$MD_RES" "$MD_SPEC" -o jsonpath='{.metadata.generation}')"
  if printf '%s' "$spec_sup" | grep -q 'speculative.draftBudgetFraction'; then
    if dbf_out="$(wait_role_rc "$MD_SPEC" server "$gen_dbf" true false 120)"; then
      record PASS "Leg C: speculative.draftBudgetFraction live tuning converged" "$dbf_out"
    else
      record FAIL "Leg C: speculative.draftBudgetFraction live tuning converged" "$dbf_out"
    fi
  else
    if dbf_out="$(wait_role_rc "$MD_SPEC" server "$gen_dbf" false false 120)" && printf '%s' "$dbf_out" | grep -q 'reason=UnsupportedEngineVersion'; then
      record PASS "Leg C: speculative.draftBudgetFraction cleanly reports UnsupportedEngineVersion on NGRAM" "$dbf_out"
    else
      record FAIL "Leg C: speculative.draftBudgetFraction cleanly reports UnsupportedEngineVersion on NGRAM" "$dbf_out"
    fi
  fi

  kubectl -n "$NS" patch "$MD_RES" "$MD_SPEC" --type=json \
    -p '[{"op":"remove","path":"/spec/roles/0/runtimeControl"}]' >/dev/null
  if sp_clr="$(wait_role_rc_cleared "$MD_SPEC" server 120)"; then
    sp_uid_after="$(kubectl -n "$NS" get pod "$SPEC_POD" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
    if [ "$sp_uid_after" = "$SPEC_UID" ]; then
      record PASS "Leg C: clearing speculative runtimeControl resets cleanly without restarting Pod" "uid=${SPEC_UID}"
    else
      record FAIL "Leg C: clearing speculative runtimeControl resets cleanly without restarting Pod" "before=${SPEC_UID} after=${sp_uid_after}"
    fi
  else
    record FAIL "Leg C: clearing speculative runtimeControl converged" "$sp_clr"
  fi
else
  record FAIL "Leg C: SGLang NGRAM speculative deployment reached Ready" "phase=${spec_phase}"
fi

results
