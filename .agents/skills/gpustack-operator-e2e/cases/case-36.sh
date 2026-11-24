#!/usr/bin/env bash
#
# CASE 36 — Live online runtimeControl on vLLM & SGLang: gpustack-engine-sentinel, probes & ACK
#           latch, Envoy allowlist/observer, router hash decoupling, live parameter tuning &
#           boot-default reset, role pause/RolePaused/resume, PreStop drain, and extended P/D,
#           PP=2, and speculative decoding parameters
#           (MUTATING, self-cleaning; AUTO-SKIPS without NVIDIA GPUs and pre-warmed ModelArtifact)
#
#   case-36.sh <NS>
#
# Prerequisites: nodes=1; gpu=nvidia; cluster=real; inputs=E2E_C36_ACCELERATOR_TYPE,E2E_C36_ARTIFACT,E2E_C36_MODEL,E2E_C36_PROBE_IMAGE,E2E_C36_READY_BOUND,E2E_C36_SGLANG_IMAGE,E2E_C36_VLLM_IMAGE
# Goal:        Verify the complete online runtimeControl lifecycle on real NVIDIA GPUs across
#              both vLLM and SGLang in five sequential legs (recycling GPUs between legs):
#              - Leg 1 (vLLM core): default flat host-staged gpustack-engine-sentinel mount
#                without VLLM_PLUGINS/dev-mode/auth Secrets; unified /gpustack/v1/{live,ready}
#                probes and PreStop drain CLI; vllm-router Envoy allowlist (403 on control/admin/
#                observer routes on :8000, 200 on inference and :9091/observer/endpoints); router
#                hash decoupling (requestTimeoutSeconds edit rolls router without restarting
#                worker); live 3-key tuning, single-key partial reset, version-gated
#                cacheAwareWindow/Threshold, Active ModelDeploymentScaler RolePaused guard during
#                role pause (withdrawn endpoint-eligible, /live 200, /ready & direct inference
#                503), resume + full boot-default reset + ledger ConfigMap deletion, and clean
#                PreStop drain with zero FailedPreStopHook events.
#              - Leg 2 (SGLang core): spawn-safe engine-sentinel with zero SGLANG_ADMIN_API_KEY
#                or auth Secrets; sglang-gateway Envoy allowlist & :9091 observer; live 5-key
#                tuning (maxActiveRequests, maxBatchedTokens, prefillMaxRequests,
#                scheduleConservativeness, prefillDecodeInterval) without Pod restart; role pause
#                (/live 200, /ready & direct inference 503); resume + full boot-default reset +
#                ledger ConfigMap deletion.
#              - Leg 3 (vLLM 1P+1D behind llm-d-router; requires >= 2 GPUs): decode KV-load knobs
#                (maxInflightKVLoads, maxAsyncKVLoadsPerStep — live or UnsupportedEngineVersion)
#                and independent per-role pause on decode while prefill stays /ready=200.
#              - Leg 4 (SGLang --pp-size=2; requires >= 2 GPUs on one node): multi-GPU PP leader
#                sentinel and scheduling.ppMaxMicroBatchSize live tuning & reset.
#              - Leg 5 (SGLang --speculative-algorithm=NGRAM): speculative.acceptThresholdSingle,
#                speculative.acceptThresholdAcc, and speculative.draftBudgetFraction (live or
#                UnsupportedEngineVersion) plus clean reset without Pod restart.
# Environment: Real NVIDIA GPU cluster with >= 1 allocatable GPU (>= 2 for Leg 3, >= 2 on a
#              single node for Leg 4), pre-warmed ModelArtifact (E2E_C36_ARTIFACT, default
#              qwen3-8b) resolving E2E_C36_MODEL (default Qwen/Qwen3-8B), pullable vLLM &
#              SGLang runner images, and pullable probe image. Auto-skips if NVIDIA GPUs or
#              pre-warmed ModelArtifact are absent.
# Inputs:      MOCKED: none. Runs real vLLM/SGLang Pods, vllm-router, sglang-gateway,
#              llm-d-router, ModelDeploymentScaler, and one shared in-cluster curl probe Pod.
# Expected:    All sentinel injection, Envoy allowlist, router decoupling, live tuning/reset,
#              pause/RolePaused/resume, PreStop drain, and extended parameter assertions pass
#              with zero unexpected worker restarts.
# Cleanup:     EXIT trap deletes the probe Pod, ModelDeploymentScaler, and all ModelDeployments
#              created by this run and force-releases any Kueue Workloads.
set -uo pipefail

CASE_ID=36
export CASE_ID
CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "${CASE_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_md-lib.sh"

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-36.sh <NS>" >&2
  exit 2
fi

MD_RES="modeldeployments.v1alpha1.worker.gpustack.ai"
SCALER_RES="modeldeploymentscalers.v1alpha1.worker.gpustack.ai"
ARTIFACT="${E2E_C36_ARTIFACT:-qwen3-8b}"
MODEL="${E2E_C36_MODEL:-Qwen/Qwen3-8B}"
VLLM_IMAGE="${E2E_C36_VLLM_IMAGE:-gpustack/runner:cuda13.0-vllm0.32.0}"
SGLANG_IMAGE="${E2E_C36_SGLANG_IMAGE:-gpustack/runner:cuda13.0-sglang0.5.22}"
PROBE_IMAGE="${E2E_C36_PROBE_IMAGE:-curlimages/curl:8.10.1}"
READY_BOUND="${E2E_C36_READY_BOUND:-720}"

RUN_PREFIX="e2e-c36-$$"
MD_VLLM="${RUN_PREFIX}-vllm"
SCALER_VLLM="${RUN_PREFIX}-scaler"
MD_SGLANG="${RUN_PREFIX}-sglang"
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

# shellcheck disable=SC2317 # Invoked by EXIT trap.
cleanup() {
  kubectl -n "$NS" delete pod "$PROBE" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n "$NS" delete "$SCALER_RES" "$SCALER_VLLM" --ignore-not-found --wait=true --timeout=90s >/dev/null 2>&1 || true
  delete_and_wait_md "$MD_VLLM"
  delete_and_wait_md "$MD_SGLANG"
  delete_and_wait_md "$MD_PD"
  delete_and_wait_md "$MD_PP"
  delete_and_wait_md "$MD_SPEC"
  return 0
}
trap cleanup EXIT

AT="${E2E_C36_ACCELERATOR_TYPE:-$(md_usable_type_for nvidia || true)}"
if [ -z "$AT" ]; then
  record SKIP "case-36 prerequisites" "no usable NVIDIA AcceleratorType found"
  results
  exit 0
fi

HW_RES="$(kubectl get acceleratortype "$AT" -o jsonpath='{.status.detail.resourceName}' 2>/dev/null || true)"
[ -n "$HW_RES" ] || HW_RES="nvidia.com/gpu"
GPU_STATS="$(kubectl get nodes -o json 2>/dev/null | python3 -c '
import json, sys
res = sys.argv[1]
doc = json.load(sys.stdin)
total, best = 0, 0
for n in doc.get("items", []):
    conds = {c["type"]: c["status"] for c in n.get("status", {}).get("conditions", [])}
    if conds.get("Ready") != "True":
        continue
    val = n.get("status", {}).get("allocatable", {}).get(res, "0")
    try:
        v = int(float(val))
        total += v
        best = max(best, v)
    except ValueError:
        pass
print("{} {}".format(total, best))
' "$HW_RES" 2>/dev/null || echo "0 0")"
GPU_CAP="${GPU_STATS%% *}"
MAX_GPUS_PER_NODE="${GPU_STATS##* }"

if [ "${GPU_CAP:-0}" -lt 1 ]; then
  record SKIP "case-36 prerequisites" "need >= 1 allocatable ${HW_RES}, found ${GPU_CAP:-0}"
  results
  exit 0
fi

if ! kubectl -n "$NS" get modelartifact "$ARTIFACT" >/dev/null 2>&1; then
  record SKIP "case-36 prerequisites" "ModelArtifact ${NS}/${ARTIFACT} not found"
  results
  exit 0
fi
ART_REV="$(kubectl -n "$NS" get modelartifact "$ARTIFACT" -o jsonpath='{.status.resolved.revision}' 2>/dev/null || true)"
if [ -z "$ART_REV" ]; then
  record SKIP "case-36 prerequisites" "ModelArtifact ${NS}/${ARTIFACT} has empty resolved.revision"
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

wait_rc_synced() {
  local name="$1" want_gen="$2" want_paused="$3" bound="${4:-120}" out=""
  for _ in $(seq 1 $((bound / 3))); do
    out="$(kubectl -n "$NS" get "$MD_RES" "$name" -o json 2>/dev/null | python3 -c '
import json, sys
doc = json.load(sys.stdin)
cond = ""
for c in doc.get("status", {}).get("conditions", []):
    if c.get("type") == "RuntimeControlSynced":
        cond = "{}:{}".format(c.get("status"), c.get("reason"))
rc = {}
for r in doc.get("status", {}).get("roles", []):
    if r.get("name") == "server":
        rc = r.get("runtimeControl") or {}
print("{}|{}|{}|{}|{}".format(cond, rc.get("observedGeneration", 0), str(rc.get("paused", False)).lower(), rc.get("synced", False), rc.get("appliedConfigDigest", "")))
' 2>/dev/null || true)"
    case "$out" in
      "True:"*"|${want_gen}|${want_paused}|True|"*)
        echo "$out"
        return 0
        ;;
      "True:NotConfigured|0|false|False|"*)
        if [ "$want_gen" = "cleared" ] && [ "$want_paused" = "false" ]; then
          echo "$out"
          return 0
        fi
        ;;
    esac
    sleep 3
  done
  echo "$out"
  return 1
}

wait_role_rc() {
  local name="$1" role_name="$2" want_gen="$3" want_synced="$4" want_paused="$5" timeout="${6:-120}" out=""
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
  local name="$1" role_name="$2" timeout="${3:-120}" out=""
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
  kubectl -n "$NS" get pod "$1" -o json 2>/dev/null | python3 -c '
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

# Start shared in-cluster curl probe Pod.
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
      command: ["sh", "-c", "sleep 7200"]
EOF
kubectl -n "$NS" wait --for=condition=Ready "pod/${PROBE}" --timeout=120s >/dev/null 2>&1 || true

# ==============================================================================
# LEG 1: vLLM core online runtimeControl + Envoy allowlist + pause + PreStop
# ==============================================================================

kubectl apply -f - >/dev/null <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD_VLLM}
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
    name: vllm-router
    requestTimeoutSeconds: 60
  roles:
    - name: server
      kind: Server
      replicas: 1
      acceleratorType: ${AT}
      resources:
        accelerator: "1"
      extraArgs:
        - --max-model-len=4096
        - --max-num-seqs=64
        - --max-num-batched-tokens=8192
        - --gpu-memory-utilization=0.80
EOF

if phase="$(wait_md_ready "$MD_VLLM")"; then
  record PASS "Leg 1: vLLM deployment reached Ready with default engine-sentinel" "phase=${phase}"

  WORKER_POD="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD_VLLM},app.kubernetes.io/component=server" -o jsonpath='{.items[0].metadata.name}')"
  WORKER_UID="$(kubectl -n "$NS" get pod "$WORKER_POD" -o jsonpath='{.metadata.uid}')"
  WORKER_IP="$(kubectl -n "$NS" get pod "$WORKER_POD" -o jsonpath='{.status.podIP}')"
  ROUTER_POD="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD_VLLM},modeldeployment.gpustack.ai/router" -o jsonpath='{.items[0].metadata.name}')"
  ROUTER_IP="$(kubectl -n "$NS" get pod "$ROUTER_POD" -o jsonpath='{.status.podIP}')"
  ENDPOINT="$(kubectl -n "$NS" get "$MD_RES" "$MD_VLLM" -o jsonpath='{.status.endpoint}')"

  pod_check="$(kubectl -n "$NS" get pod "$WORKER_POD" -o json | python3 -c '
import json, sys
p = json.load(sys.stdin)
c = p["spec"]["containers"][0]
envs = {e["name"]: e.get("value", "") for e in c.get("env", [])}
mounts = {m["name"]: m["mountPath"] for m in c.get("volumeMounts", [])}
errs = []
if mounts.get("gpustack-engine-sentinel") != "/var/lib/gpustack/operator/lib/engine-sentinel":
    errs.append("mount={}".format(mounts.get("gpustack-engine-sentinel")))
if envs.get("GPUSTACK_ENGINE_SENTINEL_ENABLE") != "1":
    errs.append("enable={}".format(envs.get("GPUSTACK_ENGINE_SENTINEL_ENABLE")))
if envs.get("GPUSTACK_ENGINE_SENTINEL_ENGINE") != "vllm":
    errs.append("engine={}".format(envs.get("GPUSTACK_ENGINE_SENTINEL_ENGINE")))
for forbidden in ("VLLM_PLUGINS", "VLLM_SERVER_DEV_MODE", "GPUSTACK_RUNTIME_CONTROL_TOKEN"):
    if forbidden in envs:
        errs.append("{} unexpectedly set".format(forbidden))
if c.get("startupProbe", {}).get("httpGet", {}).get("path") != "/gpustack/v1/live":
    errs.append("startupProbe={}".format(c.get("startupProbe")))
if c.get("livenessProbe", {}).get("httpGet", {}).get("path") != "/gpustack/v1/live":
    errs.append("livenessProbe={}".format(c.get("livenessProbe")))
if c.get("readinessProbe", {}).get("httpGet", {}).get("path") != "/gpustack/v1/ready":
    errs.append("readinessProbe={}".format(c.get("readinessProbe")))
prestop = c.get("lifecycle", {}).get("preStop", {}).get("exec", {}).get("command", [])
if "gpustack_engine_sentinel.drain" not in " ".join(prestop):
    errs.append("preStop={}".format(prestop))
print("OK" if not errs else "; ".join(errs))
')"
  if [ "$pod_check" = "OK" ]; then
    record PASS "Leg 1: worker Pod carries flat engine-sentinel mount, unified probes, PreStop CLI, zero auth/dev-mode" "$WORKER_POD"
  else
    record FAIL "Leg 1: worker Pod carries flat engine-sentinel mount, unified probes, PreStop CLI, zero auth/dev-mode" "$pod_check"
  fi

  auth_secrets="$(kubectl -n "$NS" get secrets -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep '^runtime-control-auth-' || true)"
  if [ -z "$auth_secrets" ]; then
    record PASS "Leg 1: zero operator-managed runtime-control-auth Secrets created" "none"
  else
    record FAIL "Leg 1: zero operator-managed runtime-control-auth Secrets created" "$auth_secrets"
  fi

  chat_code="$(probe_curl -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' \
    -d '{"model":"'"${MODEL}"'","messages":[{"role":"user","content":"Reply with OK"}],"max_tokens":8}' \
    "${ENDPOINT}/v1/chat/completions" 2>/dev/null || echo 000)"
  if [ "$chat_code" = "200" ]; then
    record PASS "Leg 1: vllm-router inference POST /v1/chat/completions returns 200" "${ENDPOINT}"
  else
    record FAIL "Leg 1: vllm-router inference POST /v1/chat/completions returns 200" "http=${chat_code}"
  fi

  blocked_errs=""
  for route in "/gpustack/v1/ready" "/gpustack/v1/runtime_config" "/gpustack/v1/pause" "/scale_elastic_ep" "/observer/endpoints"; do
    code="$(probe_curl -o /dev/null -w '%{http_code}' "${ENDPOINT}${route}" 2>/dev/null || echo 000)"
    [ "$code" = "403" ] || blocked_errs="${blocked_errs}${route}=${code} "
  done
  if [ -z "$blocked_errs" ]; then
    record PASS "Leg 1: vllm-router Envoy allowlist blocks control/admin/observer routes with 403" "5/5 blocked"
  else
    record FAIL "Leg 1: vllm-router Envoy allowlist blocks control/admin/observer routes with 403" "$blocked_errs"
  fi

  obs_code="$(probe_curl -o /dev/null -w '%{http_code}' "http://${ROUTER_IP}:9091/observer/endpoints" 2>/dev/null || echo 000)"
  if [ "$obs_code" = "200" ]; then
    record PASS "Leg 1: Pod-internal :9091/observer/endpoints listener responds 200" "${ROUTER_IP}:9091"
  else
    record FAIL "Leg 1: Pod-internal :9091/observer/endpoints listener responds 200" "http=${obs_code}"
  fi

  # Router hash decoupling
  kubectl -n "$NS" patch "$MD_RES" "$MD_VLLM" --type=merge -p '{"spec":{"router":{"requestTimeoutSeconds":90}}}' >/dev/null
  sleep 15
  kubectl -n "$NS" rollout status "deployment/${MD_VLLM}-router" --timeout=180s >/dev/null 2>&1 || true
  after_router_uid="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD_VLLM},app.kubernetes.io/component=server" -o jsonpath='{.items[0].metadata.uid}')"
  after_router_restarts="$(kubectl -n "$NS" get pod "$WORKER_POD" -o jsonpath='{.status.containerStatuses[0].restartCount}')"
  if [ "$after_router_uid" = "$WORKER_UID" ] && [ "${after_router_restarts:-0}" = "0" ]; then
    record PASS "Leg 1: router edit rolled router Deployment without restarting worker Pod" "uid=${WORKER_UID} restarts=0"
  else
    record FAIL "Leg 1: router edit rolled router Deployment without restarting worker Pod" "before=${WORKER_UID} after=${after_router_uid} restarts=${after_router_restarts}"
  fi

  # Live vLLM parameter tuning, partial reset, and version-gated keys
  init_cfg_json="$(probe_curl "http://${WORKER_IP}:8000/gpustack/v1/runtime_config" 2>/dev/null || echo '{}')"
  vllm_supported="$(printf '%s' "$init_cfg_json" | python3 -c 'import json,sys; print(",".join(json.load(sys.stdin).get("supported_fields") or []))' 2>/dev/null || true)"
  record PASS "Leg 1: vLLM sentinel /gpustack/v1/runtime_config capability discovery" "supported=${vllm_supported:-none}"

  kubectl -n "$NS" patch "$MD_RES" "$MD_VLLM" --type=merge -p '{"spec":{"roles":[{"name":"server","kind":"Server","replicas":1,"acceleratorType":"'"${AT}"'","resources":{"accelerator":"1"},"extraArgs":["--max-model-len=4096","--max-num-seqs=64","--max-num-batched-tokens=8192","--gpu-memory-utilization=0.80"],"runtimeControl":{"parameters":{"scheduling.maxActiveRequests":"24","scheduling.maxBatchedTokens":"4096","scheduling.longPrefillTokenThreshold":"1024"}}}]}}' >/dev/null
  gen_tune="$(kubectl -n "$NS" get "$MD_RES" "$MD_VLLM" -o jsonpath='{.metadata.generation}')"
  if rc_out="$(wait_rc_synced "$MD_VLLM" "$gen_tune" "false" 120)"; then
    record PASS "Leg 1: live vLLM runtimeControl.parameters converged (3 scheduler knobs)" "$rc_out"
  else
    record FAIL "Leg 1: live vLLM runtimeControl.parameters converged (3 scheduler knobs)" "$rc_out"
  fi

  cfg_check="$(probe_curl "http://${WORKER_IP}:8000/gpustack/v1/runtime_config" 2>/dev/null | python3 -c '
import json, sys
vals = (json.load(sys.stdin).get("values") or {})
errs = []
if vals.get("scheduling.maxActiveRequests") != 24:
    errs.append("maxActiveRequests={}".format(vals.get("scheduling.maxActiveRequests")))
if vals.get("scheduling.maxBatchedTokens") != 4096:
    errs.append("maxBatchedTokens={}".format(vals.get("scheduling.maxBatchedTokens")))
if vals.get("scheduling.longPrefillTokenThreshold") != 1024:
    errs.append("longPrefill={}".format(vals.get("scheduling.longPrefillTokenThreshold")))
print("OK" if not errs else "; ".join(errs))
' 2>/dev/null || echo "invalid json")"
  if [ "$cfg_check" = "OK" ]; then
    record PASS "Leg 1: vLLM runtime_config live readback matches tuned values" "maxActiveRequests=24 maxBatchedTokens=4096 longPrefill=1024"
  else
    record FAIL "Leg 1: vLLM runtime_config live readback matches tuned values" "$cfg_check"
  fi

  # Single-key removal reset
  kubectl -n "$NS" patch "$MD_RES" "$MD_VLLM" --type=json \
    -p '[{"op":"remove","path":"/spec/roles/0/runtimeControl/parameters/scheduling.longPrefillTokenThreshold"}]' >/dev/null
  gen_partial="$(kubectl -n "$NS" get "$MD_RES" "$MD_VLLM" -o jsonpath='{.metadata.generation}')"
  if rc_part="$(wait_rc_synced "$MD_VLLM" "$gen_partial" "false" 120)"; then
    record PASS "Leg 1: partial parameter removal converged without Pod restart" "$rc_part"
  else
    record FAIL "Leg 1: partial parameter removal converged without Pod restart" "$rc_part"
  fi

  partial_check="$(probe_curl "http://${WORKER_IP}:8000/gpustack/v1/runtime_config" 2>/dev/null | python3 -c '
import json, sys
d = json.load(sys.stdin)
vals, bd = d.get("values") or {}, d.get("boot_defaults") or {}
errs = []
if vals.get("scheduling.maxActiveRequests") != 24:
    errs.append("maxActiveRequests={}".format(vals.get("scheduling.maxActiveRequests")))
if vals.get("scheduling.maxBatchedTokens") != 4096:
    errs.append("maxBatchedTokens={}".format(vals.get("scheduling.maxBatchedTokens")))
if vals.get("scheduling.longPrefillTokenThreshold") != bd.get("scheduling.longPrefillTokenThreshold"):
    errs.append("longPrefill={}!=boot({})".format(vals.get("scheduling.longPrefillTokenThreshold"), bd.get("scheduling.longPrefillTokenThreshold")))
print("OK" if not errs else "; ".join(errs))
' 2>/dev/null || echo "invalid json")"
  if [ "$partial_check" = "OK" ]; then
    record PASS "Leg 1: removing single key resets only that key to boot_defaults" "longPrefill restored"
  else
    record FAIL "Leg 1: removing single key resets only that key to boot_defaults" "$partial_check"
  fi

  # Version-gated cacheAwareWindow & cacheAwareThreshold
  kubectl -n "$NS" patch "$MD_RES" "$MD_VLLM" --type=json \
    -p '[{"op":"add","path":"/spec/roles/0/runtimeControl/parameters/scheduling.cacheAwareWindow","value":"64"},{"op":"add","path":"/spec/roles/0/runtimeControl/parameters/scheduling.cacheAwareThreshold","value":"0.5"}]' >/dev/null
  gen_ca="$(kubectl -n "$NS" get "$MD_RES" "$MD_VLLM" -o jsonpath='{.metadata.generation}')"
  if printf '%s' "$vllm_supported" | grep -q 'scheduling.cacheAwareWindow' && printf '%s' "$vllm_supported" | grep -q 'scheduling.cacheAwareThreshold'; then
    if rc_ca="$(wait_rc_synced "$MD_VLLM" "$gen_ca" "false" 120)"; then
      record PASS "Leg 1: cacheAwareWindow/Threshold live tuning converged" "$rc_ca"
    else
      record FAIL "Leg 1: cacheAwareWindow/Threshold live tuning converged" "$rc_ca"
    fi
  else
    ca_reason=""
    for _ in $(seq 1 40); do
      ca_reason="$(kubectl -n "$NS" get "$MD_RES" "$MD_VLLM" -o json 2>/dev/null | python3 -c '
import json, sys
d = json.load(sys.stdin)
rc = (d.get("status", {}).get("roles") or [{}])[0].get("runtimeControl") or {}
if rc.get("observedGeneration") == int(sys.argv[1]) and rc.get("synced") is False:
    print("{}|{}".format(rc.get("reason", ""), ",".join(rc.get("unsupportedFields") or [])))
' "$gen_ca" 2>/dev/null || true)"
      [ -n "$ca_reason" ] && break
      sleep 3
    done
    if printf '%s' "$ca_reason" | grep -q '^UnsupportedEngineVersion|'; then
      record PASS "Leg 1: unsupported cacheAwareWindow/Threshold report UnsupportedEngineVersion" "$ca_reason"
    else
      record FAIL "Leg 1: unsupported cacheAwareWindow/Threshold report UnsupportedEngineVersion" "got=${ca_reason:-timeout}"
    fi
    kubectl -n "$NS" patch "$MD_RES" "$MD_VLLM" --type=json \
      -p '[{"op":"remove","path":"/spec/roles/0/runtimeControl/parameters/scheduling.cacheAwareWindow"},{"op":"remove","path":"/spec/roles/0/runtimeControl/parameters/scheduling.cacheAwareThreshold"}]' >/dev/null
    gen_ca_rec="$(kubectl -n "$NS" get "$MD_RES" "$MD_VLLM" -o jsonpath='{.metadata.generation}')"
    if rc_rec="$(wait_rc_synced "$MD_VLLM" "$gen_ca_rec" "false" 120)"; then
      record PASS "Leg 1: removing unsupported cache-aware keys recovers RuntimeControlSynced=True" "$rc_rec"
    else
      record FAIL "Leg 1: removing unsupported cache-aware keys recovers RuntimeControlSynced=True" "$rc_rec"
    fi
  fi

  uid_after_tune="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD_VLLM},app.kubernetes.io/component=server" -o jsonpath='{.items[0].metadata.uid}')"
  if [ "$uid_after_tune" = "$WORKER_UID" ]; then
    record PASS "Leg 1: worker Pod UID unchanged after live tuning and unsupported-field test" "$uid_after_tune"
  else
    record FAIL "Leg 1: worker Pod UID unchanged after live tuning and unsupported-field test" "before=${WORKER_UID} after=${uid_after_tune}"
  fi

  # Attach Active ModelDeploymentScaler + pause -> RolePaused -> resume + reset
  kubectl apply -f - >/dev/null <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${SCALER_VLLM}
  namespace: ${NS}
spec:
  targetRef:
    name: ${MD_VLLM}
  mode: Active
  algorithm:
    type: Proportional
    proportional:
      metric: RunningRequests
      roleTargets:
        - name: server
          target: "4"
  roles:
    - name: server
      minReplicas: 1
      maxReplicas: 2
EOF
  for _ in $(seq 1 30); do
    bound_ann="$(kubectl -n "$NS" get "$MD_RES" "$MD_VLLM" -o jsonpath='{.metadata.annotations.modeldeployment\.gpustack\.ai/scaler-binding}' 2>/dev/null || true)"
    [ -n "$bound_ann" ] && break
    sleep 2
  done

  kubectl -n "$NS" patch "$MD_RES" "$MD_VLLM" --type=json -p '[{"op":"add","path":"/spec/roles/0/pause","value":true}]' >/dev/null
  gen_pause="$(kubectl -n "$NS" get "$MD_RES" "$MD_VLLM" -o jsonpath='{.metadata.generation}')"
  if rc_pause="$(wait_rc_synced "$MD_VLLM" "$gen_pause" "true" 120)"; then
    record PASS "Leg 1: vLLM role pause=true converged" "$rc_pause"
  else
    record FAIL "Leg 1: vLLM role pause=true converged" "$rc_pause"
  fi

  elig_label="$(kubectl -n "$NS" get pod "$WORKER_POD" -o jsonpath='{.metadata.labels.modeldeployment\.gpustack\.ai/endpoint-eligible}' 2>/dev/null || true)"
  live_code="$(probe_curl -o /dev/null -w '%{http_code}' "http://${WORKER_IP}:8000/gpustack/v1/live" 2>/dev/null || echo 000)"
  ready_code="$(probe_curl -o /dev/null -w '%{http_code}' "http://${WORKER_IP}:8000/gpustack/v1/ready" 2>/dev/null || echo 000)"
  direct_code="$(probe_curl -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' \
    -d '{"model":"'"${MODEL}"'","messages":[{"role":"user","content":"Hi"}],"max_tokens":4}' \
    "http://${WORKER_IP}:8000/v1/chat/completions" 2>/dev/null || echo 000)"
  if [ -z "$elig_label" ] && [ "$live_code" = "200" ] && [ "$ready_code" = "503" ] && [ "$direct_code" = "503" ]; then
    record PASS "Leg 1: paused vLLM withdraws endpoint-eligible, /live=200, /ready=503, direct=503" "live=200 ready=503 direct=503"
  else
    record FAIL "Leg 1: paused vLLM withdraws endpoint-eligible, /live=200, /ready=503, direct=503" "eligible=${elig_label:-empty} live=${live_code} ready=${ready_code} direct=${direct_code}"
  fi

  scaler_blocked=""
  for _ in $(seq 1 30); do
    scaler_blocked="$(kubectl -n "$NS" get "$SCALER_RES" "$SCALER_VLLM" -o json 2>/dev/null | python3 -c '
import json, sys
for c in json.load(sys.stdin).get("status", {}).get("conditions", []):
    if c.get("type") == "Blocked":
        print("{}|{}".format(c.get("status"), c.get("reason")))
' 2>/dev/null || true)"
    [ "$scaler_blocked" = "True|RolePaused" ] && break
    sleep 3
  done
  if [ "$scaler_blocked" = "True|RolePaused" ]; then
    record PASS "Leg 1: ModelDeploymentScaler reports Blocked=True Reason=RolePaused" "$scaler_blocked"
  else
    record FAIL "Leg 1: ModelDeploymentScaler reports Blocked=True Reason=RolePaused" "got=${scaler_blocked}"
  fi

  kubectl -n "$NS" patch "$MD_RES" "$MD_VLLM" --type=json \
    -p '[{"op":"replace","path":"/spec/roles/0/pause","value":false},{"op":"remove","path":"/spec/roles/0/runtimeControl"}]' >/dev/null
  if rc_res="$(wait_rc_synced "$MD_VLLM" "cleared" "false" 120)"; then
    record PASS "Leg 1: vLLM resume and runtimeControl removal reset converged" "$rc_res"
  else
    record FAIL "Leg 1: vLLM resume and runtimeControl removal reset converged" "$rc_res"
  fi

  resume_chat="$(probe_curl -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' \
    -d '{"model":"'"${MODEL}"'","messages":[{"role":"user","content":"Reply OK"}],"max_tokens":8}' \
    "${ENDPOINT}/v1/chat/completions" 2>/dev/null || echo 000)"
  if [ "$resume_chat" = "200" ]; then
    record PASS "Leg 1: vLLM inference returns 200 after resume" "http=200"
  else
    record FAIL "Leg 1: vLLM inference returns 200 after resume" "http=${resume_chat}"
  fi

  reset_check="$(probe_curl "http://${WORKER_IP}:8000/gpustack/v1/runtime_config" 2>/dev/null | python3 -c '
import json, sys
d = json.load(sys.stdin)
vals, bd = d.get("values") or {}, d.get("boot_defaults") or {}
errs = []
for k in ("scheduling.maxActiveRequests", "scheduling.maxBatchedTokens", "scheduling.longPrefillTokenThreshold"):
    if k in bd and vals.get(k) != bd.get(k):
        errs.append("{}={}!=boot({})".format(k, vals.get(k), bd.get(k)))
print("OK" if not errs else "; ".join(errs))
' 2>/dev/null || echo "invalid json")"
  if [ "$reset_check" = "OK" ]; then
    record PASS "Leg 1: clearing runtimeControl restores tuned vLLM parameters to boot_defaults" "all restored"
  else
    record FAIL "Leg 1: clearing runtimeControl restores tuned vLLM parameters to boot_defaults" "$reset_check"
  fi

  md_uid="$(kubectl -n "$NS" get "$MD_RES" "$MD_VLLM" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
  ledger_gone="false"
  for _ in $(seq 1 15); do
    if ! kubectl -n "$NS" get configmap "runtime-control-${md_uid}-server" >/dev/null 2>&1; then
      ledger_gone="true"
      break
    fi
    sleep 2
  done
  if [ "$ledger_gone" = "true" ]; then
    record PASS "Leg 1: clearing vLLM runtimeControl deletes role ledger ConfigMap" "runtime-control-${md_uid}-server deleted"
  else
    record FAIL "Leg 1: clearing vLLM runtimeControl deletes role ledger ConfigMap" "still exists"
  fi

  # PreStop drain check
  kubectl -n "$NS" delete pod "$WORKER_POD" --wait=true --timeout=120s >/dev/null 2>&1 || true
  prestop_fail="$(kubectl -n "$NS" get events --field-selector "involvedObject.name=${WORKER_POD},reason=FailedPreStopHook" -o name 2>/dev/null || true)"
  if [ -z "$prestop_fail" ]; then
    record PASS "Leg 1: PreStop gpustack_engine_sentinel.drain exited cleanly with zero FailedPreStopHook events" "$WORKER_POD"
  else
    record FAIL "Leg 1: PreStop gpustack_engine_sentinel.drain exited cleanly with zero FailedPreStopHook events" "$prestop_fail"
  fi
else
  record FAIL "Leg 1: vLLM deployment reached Ready with default engine-sentinel" "phase=${phase}"
fi

kubectl -n "$NS" delete "$SCALER_RES" "$SCALER_VLLM" --ignore-not-found --wait=true --timeout=90s >/dev/null 2>&1 || true
delete_and_wait_md "$MD_VLLM"

# ==============================================================================
# LEG 2: SGLang core online runtimeControl + sglang-gateway Envoy allowlist
# ==============================================================================

kubectl apply -f - >/dev/null <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD_SGLANG}
  namespace: ${NS}
spec:
  model:
    name: ${MODEL}
    artifactRef:
      name: ${ARTIFACT}
  engine:
    name: SGLang
    image: ${SGLANG_IMAGE}
  router:
    name: sglang-gateway
    requestTimeoutSeconds: 60
  roles:
    - name: server
      kind: Server
      replicas: 1
      acceleratorType: ${AT}
      resources:
        accelerator: "1"
      extraArgs:
        - --context-length=4096
        - --max-running-requests=64
        - --chunked-prefill-size=4096
        - --mem-fraction-static=0.80
EOF

if sg_phase="$(wait_md_ready "$MD_SGLANG")"; then
  record PASS "Leg 2: SGLang deployment reached Ready with default engine-sentinel" "phase=${sg_phase}"

  SG_POD="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD_SGLANG},app.kubernetes.io/component=server" -o jsonpath='{.items[0].metadata.name}')"
  SG_UID="$(kubectl -n "$NS" get pod "$SG_POD" -o jsonpath='{.metadata.uid}')"
  SG_IP="$(kubectl -n "$NS" get pod "$SG_POD" -o jsonpath='{.status.podIP}')"
  SG_RTR_POD="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD_SGLANG},modeldeployment.gpustack.ai/router" -o jsonpath='{.items[0].metadata.name}')"
  SG_RTR_IP="$(kubectl -n "$NS" get pod "$SG_RTR_POD" -o jsonpath='{.status.podIP}')"
  SG_EP="$(kubectl -n "$NS" get "$MD_RES" "$MD_SGLANG" -o jsonpath='{.status.endpoint}')"

  sg_pod_check="$(kubectl -n "$NS" get pod "$SG_POD" -o json | python3 -c '
import json, sys
p = json.load(sys.stdin)
c = p["spec"]["containers"][0]
envs = {e["name"]: e.get("value", "") for e in c.get("env", [])}
mounts = {m["name"]: m["mountPath"] for m in c.get("volumeMounts", [])}
errs = []
if mounts.get("gpustack-engine-sentinel") != "/var/lib/gpustack/operator/lib/engine-sentinel":
    errs.append("mount={}".format(mounts.get("gpustack-engine-sentinel")))
if envs.get("GPUSTACK_ENGINE_SENTINEL_ENABLE") != "1":
    errs.append("enable={}".format(envs.get("GPUSTACK_ENGINE_SENTINEL_ENABLE")))
if envs.get("GPUSTACK_ENGINE_SENTINEL_ENGINE") != "sglang":
    errs.append("engine={}".format(envs.get("GPUSTACK_ENGINE_SENTINEL_ENGINE")))
for forbidden in ("SGLANG_ADMIN_API_KEY", "GPUSTACK_RUNTIME_CONTROL_TOKEN"):
    if forbidden in envs:
        errs.append("{} unexpectedly set".format(forbidden))
if c.get("startupProbe", {}).get("httpGet", {}).get("path") != "/gpustack/v1/live":
    errs.append("startupProbe != /gpustack/v1/live")
if c.get("readinessProbe", {}).get("httpGet", {}).get("path") != "/gpustack/v1/ready":
    errs.append("readinessProbe != /gpustack/v1/ready")
print("OK" if not errs else "; ".join(errs))
')"
  if [ "$sg_pod_check" = "OK" ]; then
    record PASS "Leg 2: SGLang Pod carries flat engine-sentinel mount, unified probes, zero SGLANG_ADMIN_API_KEY" "$SG_POD"
  else
    record FAIL "Leg 2: SGLang Pod carries flat engine-sentinel mount, unified probes, zero SGLANG_ADMIN_API_KEY" "$sg_pod_check"
  fi

  sg_chat="$(probe_curl -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' \
    -d '{"model":"'"${MODEL}"'","messages":[{"role":"user","content":"Reply with OK"}],"max_tokens":8}' \
    "${SG_EP}/v1/chat/completions" 2>/dev/null || echo 000)"
  if [ "$sg_chat" = "200" ]; then
    record PASS "Leg 2: sglang-gateway inference POST /v1/chat/completions returns 200" "${SG_EP}"
  else
    record FAIL "Leg 2: sglang-gateway inference POST /v1/chat/completions returns 200" "http=${sg_chat}"
  fi

  sg_blocked=""
  for route in "/gpustack/v1/ready" "/gpustack/v1/runtime_config" "/set_internal_state" "/pause_generation" "/observer/endpoints"; do
    code="$(probe_curl -o /dev/null -w '%{http_code}' "${SG_EP}${route}" 2>/dev/null || echo 000)"
    [ "$code" = "403" ] || sg_blocked="${sg_blocked}${route}=${code} "
  done
  if [ -z "$sg_blocked" ]; then
    record PASS "Leg 2: sglang-gateway Envoy allowlist blocks control/internal_state/observer routes with 403" "5/5 blocked"
  else
    record FAIL "Leg 2: sglang-gateway Envoy allowlist blocks control/internal_state/observer routes with 403" "$sg_blocked"
  fi

  sg_obs="$(probe_curl -o /dev/null -w '%{http_code}' "http://${SG_RTR_IP}:9091/observer/endpoints" 2>/dev/null || echo 000)"
  if [ "$sg_obs" = "200" ]; then
    record PASS "Leg 2: sglang-gateway internal :9091/observer/endpoints responds 200" "${SG_RTR_IP}:9091"
  else
    record FAIL "Leg 2: sglang-gateway internal :9091/observer/endpoints responds 200" "http=${sg_obs}"
  fi

  init_sg_cfg="$(probe_curl "http://${SG_IP}:8000/gpustack/v1/runtime_config" 2>/dev/null || echo '{}')"
  sg_sup="$(printf '%s' "$init_sg_cfg" | python3 -c 'import json,sys; print(",".join(json.load(sys.stdin).get("supported_fields") or []))' 2>/dev/null || true)"
  record PASS "Leg 2: SGLang sentinel capability discovery" "supported=${sg_sup:-none}"

  kubectl -n "$NS" patch "$MD_RES" "$MD_SGLANG" --type=merge -p '{"spec":{"roles":[{"name":"server","kind":"Server","replicas":1,"acceleratorType":"'"${AT}"'","resources":{"accelerator":"1"},"extraArgs":["--context-length=4096","--max-running-requests=64","--chunked-prefill-size=4096","--mem-fraction-static=0.80"],"runtimeControl":{"parameters":{"scheduling.maxActiveRequests":"24","scheduling.maxBatchedTokens":"2048","scheduling.prefillMaxRequests":"12","scheduling.scheduleConservativeness":"1.2","scheduling.prefillDecodeInterval":"2"}}}]}}' >/dev/null
  gen_sg_tune="$(kubectl -n "$NS" get "$MD_RES" "$MD_SGLANG" -o jsonpath='{.metadata.generation}')"
  if rc_sg="$(wait_rc_synced "$MD_SGLANG" "$gen_sg_tune" "false" 120)"; then
    record PASS "Leg 2: live SGLang runtimeControl.parameters converged (5 keys)" "$rc_sg"
  else
    record FAIL "Leg 2: live SGLang runtimeControl.parameters converged (5 keys)" "$rc_sg"
  fi

  sg_cfg_chk="$(probe_curl "http://${SG_IP}:8000/gpustack/v1/runtime_config" 2>/dev/null | python3 -c '
import json, sys
vals = (json.load(sys.stdin).get("values") or {})
errs = []
if vals.get("scheduling.maxActiveRequests") != 24:
    errs.append("maxActiveRequests={}".format(vals.get("scheduling.maxActiveRequests")))
if vals.get("scheduling.maxBatchedTokens") != 2048:
    errs.append("maxBatchedTokens={}".format(vals.get("scheduling.maxBatchedTokens")))
if vals.get("scheduling.prefillMaxRequests") != 12:
    errs.append("prefillMaxRequests={}".format(vals.get("scheduling.prefillMaxRequests")))
if abs(float(vals.get("scheduling.scheduleConservativeness", 0)) - 1.2) > 1e-6:
    errs.append("scheduleConservativeness={}".format(vals.get("scheduling.scheduleConservativeness")))
if vals.get("scheduling.prefillDecodeInterval") != 2:
    errs.append("prefillDecodeInterval={}".format(vals.get("scheduling.prefillDecodeInterval")))
print("OK" if not errs else "; ".join(errs))
' 2>/dev/null || echo "invalid json")"
  if [ "$sg_cfg_chk" = "OK" ]; then
    record PASS "Leg 2: SGLang runtime_config live readback matches tuned values" "5/5 matched"
  else
    record FAIL "Leg 2: SGLang runtime_config live readback matches tuned values" "$sg_cfg_chk"
  fi

  sg_uid_after="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD_SGLANG},app.kubernetes.io/component=server" -o jsonpath='{.items[0].metadata.uid}')"
  if [ "$sg_uid_after" = "$SG_UID" ]; then
    record PASS "Leg 2: SGLang worker Pod UID unchanged after live tuning" "$sg_uid_after"
  else
    record FAIL "Leg 2: SGLang worker Pod UID unchanged after live tuning" "before=${SG_UID} after=${sg_uid_after}"
  fi

  kubectl -n "$NS" patch "$MD_RES" "$MD_SGLANG" --type=json -p '[{"op":"add","path":"/spec/roles/0/pause","value":true}]' >/dev/null
  gen_sg_pause="$(kubectl -n "$NS" get "$MD_RES" "$MD_SGLANG" -o jsonpath='{.metadata.generation}')"
  if rc_sg_p="$(wait_rc_synced "$MD_SGLANG" "$gen_sg_pause" "true" 120)"; then
    record PASS "Leg 2: SGLang role pause=true converged" "$rc_sg_p"
  else
    record FAIL "Leg 2: SGLang role pause=true converged" "$rc_sg_p"
  fi

  sg_live="$(probe_curl -o /dev/null -w '%{http_code}' "http://${SG_IP}:8000/gpustack/v1/live" 2>/dev/null || echo 000)"
  sg_rdy="$(probe_curl -o /dev/null -w '%{http_code}' "http://${SG_IP}:8000/gpustack/v1/ready" 2>/dev/null || echo 000)"
  sg_dir="$(probe_curl -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' \
    -d '{"model":"'"${MODEL}"'","messages":[{"role":"user","content":"Hi"}],"max_tokens":4}' \
    "http://${SG_IP}:8000/v1/chat/completions" 2>/dev/null || echo 000)"
  if [ "$sg_live" = "200" ] && [ "$sg_rdy" = "503" ] && [ "$sg_dir" = "503" ]; then
    record PASS "Leg 2: paused SGLang keeps /live=200 and gates /ready and direct inference with 503" "live=200 ready=503 direct=503"
  else
    record FAIL "Leg 2: paused SGLang keeps /live=200 and gates /ready and direct inference with 503" "live=${sg_live} ready=${sg_rdy} direct=${sg_dir}"
  fi

  kubectl -n "$NS" patch "$MD_RES" "$MD_SGLANG" --type=json \
    -p '[{"op":"replace","path":"/spec/roles/0/pause","value":false},{"op":"remove","path":"/spec/roles/0/runtimeControl"}]' >/dev/null
  if rc_sg_res="$(wait_rc_synced "$MD_SGLANG" "cleared" "false" 120)"; then
    record PASS "Leg 2: SGLang role resume and parameter reset converged" "$rc_sg_res"
  else
    record FAIL "Leg 2: SGLang role resume and parameter reset converged" "$rc_sg_res"
  fi

  sg_res_chat="$(probe_curl -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' \
    -d '{"model":"'"${MODEL}"'","messages":[{"role":"user","content":"Reply OK"}],"max_tokens":8}' \
    "${SG_EP}/v1/chat/completions" 2>/dev/null || echo 000)"
  if [ "$sg_res_chat" = "200" ]; then
    record PASS "Leg 2: SGLang inference returns 200 after resume" "http=200"
  else
    record FAIL "Leg 2: SGLang inference returns 200 after resume" "http=${sg_res_chat}"
  fi

  sg_reset_chk="$(probe_curl "http://${SG_IP}:8000/gpustack/v1/runtime_config" 2>/dev/null | python3 -c '
import json, sys
d = json.load(sys.stdin)
vals, bd = d.get("values") or {}, d.get("boot_defaults") or {}
errs = []
for k in ("scheduling.maxActiveRequests", "scheduling.maxBatchedTokens", "scheduling.prefillMaxRequests", "scheduling.scheduleConservativeness", "scheduling.prefillDecodeInterval"):
    if k in bd:
        v, b = vals.get(k), bd.get(k)
        if isinstance(b, float) and isinstance(v, (int, float)):
            if abs(float(v) - float(b)) > 1e-6:
                errs.append("{}={}!=boot({})".format(k, v, b))
        elif v != b:
            errs.append("{}={}!=boot({})".format(k, v, b))
print("OK" if not errs else "; ".join(errs))
' 2>/dev/null || echo "invalid json")"
  if [ "$sg_reset_chk" = "OK" ]; then
    record PASS "Leg 2: clearing runtimeControl restores all 5 SGLang parameters to boot_defaults" "5/5 restored"
  else
    record FAIL "Leg 2: clearing runtimeControl restores all 5 SGLang parameters to boot_defaults" "$sg_reset_chk"
  fi

  sg_md_uid="$(kubectl -n "$NS" get "$MD_RES" "$MD_SGLANG" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
  sg_ledger_gone="false"
  for _ in $(seq 1 15); do
    if ! kubectl -n "$NS" get configmap "runtime-control-${sg_md_uid}-server" >/dev/null 2>&1; then
      sg_ledger_gone="true"
      break
    fi
    sleep 2
  done
  if [ "$sg_ledger_gone" = "true" ]; then
    record PASS "Leg 2: clearing SGLang runtimeControl deletes role ledger ConfigMap" "runtime-control-${sg_md_uid}-server deleted"
  else
    record FAIL "Leg 2: clearing SGLang runtimeControl deletes role ledger ConfigMap" "still exists"
  fi
else
  record FAIL "Leg 2: SGLang deployment reached Ready with default engine-sentinel" "phase=${sg_phase}"
fi

delete_and_wait_md "$MD_SGLANG"

# ==============================================================================
# LEG 3: vLLM 1P+1D KV-load knobs & independent per-role pause (needs >= 2 GPUs)
# ==============================================================================

if [ "${GPU_CAP:-0}" -lt 2 ]; then
  record SKIP "Leg 3: vLLM 1P+1D KV-load knobs & per-role pause" "need >= 2 allocatable ${HW_RES}, found ${GPU_CAP:-0}"
else
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
    record PASS "Leg 3: vLLM 1P+1D deployment reached Ready" "phase=${pd_phase}"
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
      record PASS "Leg 3: vLLM 1P+1D inference through llm-d-router returns 200" "${PD_EP}"
    else
      record FAIL "Leg 3: vLLM 1P+1D inference through llm-d-router returns 200" "http=${pd_chat}"
    fi

    dec_cfg="$(probe_curl "http://${DECODE_IP}:${DECODE_PORT}/gpustack/v1/runtime_config" 2>/dev/null || echo '{}')"
    dec_sup="$(printf '%s' "$dec_cfg" | python3 -c 'import json,sys; print(",".join(json.load(sys.stdin).get("supported_fields") or []))' 2>/dev/null || true)"
    record PASS "Leg 3: decode sentinel capability discovery" "port=${DECODE_PORT} supported=${dec_sup:-none}"

    kubectl -n "$NS" patch "$MD_RES" "$MD_PD" --type=json \
      -p '[{"op":"add","path":"/spec/roles/1/runtimeControl","value":{"parameters":{"scheduling.maxInflightKVLoads":"8","scheduling.maxAsyncKVLoadsPerStep":"4"}}}]' >/dev/null
    gen_kv="$(kubectl -n "$NS" get "$MD_RES" "$MD_PD" -o jsonpath='{.metadata.generation}')"
    if printf '%s' "$dec_sup" | grep -q 'scheduling.maxInflightKVLoads' && printf '%s' "$dec_sup" | grep -q 'scheduling.maxAsyncKVLoadsPerStep'; then
      if kv_out="$(wait_role_rc "$MD_PD" decode "$gen_kv" true false 120)"; then
        record PASS "Leg 3: decode KV-load parameters converged live" "$kv_out"
      else
        record FAIL "Leg 3: decode KV-load parameters converged live" "$kv_out"
      fi
    else
      if kv_out="$(wait_role_rc "$MD_PD" decode "$gen_kv" false false 120)" && printf '%s' "$kv_out" | grep -q 'reason=UnsupportedEngineVersion'; then
        record PASS "Leg 3: decode KV-load parameters report UnsupportedEngineVersion" "$kv_out"
      else
        record FAIL "Leg 3: decode KV-load parameters report UnsupportedEngineVersion" "$kv_out"
      fi
    fi

    kubectl -n "$NS" patch "$MD_RES" "$MD_PD" --type=json \
      -p '[{"op":"remove","path":"/spec/roles/1/runtimeControl"}]' >/dev/null
    if kv_clr="$(wait_role_rc_cleared "$MD_PD" decode 120)"; then
      record PASS "Leg 3: removing decode KV-load parameters recovers RuntimeControlSynced=True" "$kv_clr"
    else
      record FAIL "Leg 3: removing decode KV-load parameters recovers RuntimeControlSynced=True" "$kv_clr"
    fi

    dec_uid_after="$(kubectl -n "$NS" get pod "$DECODE_POD" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
    if [ "$dec_uid_after" = "$DECODE_UID" ]; then
      record PASS "Leg 3: decode Pod UID unchanged across KV-load test" "$DECODE_UID"
    else
      record FAIL "Leg 3: decode Pod UID unchanged across KV-load test" "before=${DECODE_UID} after=${dec_uid_after}"
    fi

    kubectl -n "$NS" patch "$MD_RES" "$MD_PD" --type=json \
      -p '[{"op":"add","path":"/spec/roles/1/pause","value":true}]' >/dev/null
    gen_dec_pause="$(kubectl -n "$NS" get "$MD_RES" "$MD_PD" -o jsonpath='{.metadata.generation}')"
    if dp_out="$(wait_role_rc "$MD_PD" decode "$gen_dec_pause" true true 120)"; then
      dec_ready="$(probe_curl -o /dev/null -w '%{http_code}' "http://${DECODE_IP}:${DECODE_PORT}/gpustack/v1/ready" 2>/dev/null || echo 000)"
      pre_ready="$(probe_curl -o /dev/null -w '%{http_code}' "http://${PREFILL_IP}:${PREFILL_PORT}/gpustack/v1/ready" 2>/dev/null || echo 000)"
      if [ "$dec_ready" = "503" ] && [ "$pre_ready" = "200" ]; then
        record PASS "Leg 3: pausing decode gates decode /ready=503 while prefill stays /ready=200" "decode=503 prefill=200"
      else
        record FAIL "Leg 3: pausing decode gates decode /ready=503 while prefill stays /ready=200" "decode=${dec_ready} prefill=${pre_ready}"
      fi
    else
      record FAIL "Leg 3: decode role pause=true converged" "$dp_out"
    fi
  else
    record FAIL "Leg 3: vLLM 1P+1D deployment reached Ready" "phase=${pd_phase}"
  fi
  delete_and_wait_md "$MD_PD"
fi

# ==============================================================================
# LEG 4: SGLang --pp-size=2 scheduling.ppMaxMicroBatchSize (needs >= 2 GPUs/node)
# ==============================================================================

if [ "${MAX_GPUS_PER_NODE:-0}" -lt 2 ]; then
  record SKIP "Leg 4: SGLang --pp-size=2 scheduling.ppMaxMicroBatchSize" "need >= 2 allocatable ${HW_RES} on a single node, max is ${MAX_GPUS_PER_NODE:-0}"
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
    record PASS "Leg 4: SGLang --pp-size=2 deployment reached Ready" "phase=${pp_phase}"
    PP_LEADER_POD="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD_PP},app.kubernetes.io/component=server,modeldeployment.gpustack.ai/member-index=0" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
    [ -n "$PP_LEADER_POD" ] || PP_LEADER_POD="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD_PP},app.kubernetes.io/component=server" -o jsonpath='{.items[0].metadata.name}')"
    PP_LEADER_IP="$(kubectl -n "$NS" get pod "$PP_LEADER_POD" -o jsonpath='{.status.podIP}')"
    PP_LEADER_UID="$(kubectl -n "$NS" get pod "$PP_LEADER_POD" -o jsonpath='{.metadata.uid}')"

    pp_init="$(probe_curl "http://${PP_LEADER_IP}:8000/gpustack/v1/runtime_config" 2>/dev/null || echo '{}')"
    pp_sup="$(printf '%s' "$pp_init" | python3 -c 'import json,sys; print(",".join(json.load(sys.stdin).get("supported_fields") or []))' 2>/dev/null || true)"
    record PASS "Leg 4: SGLang PP=2 leader sentinel capability discovery" "pod=${PP_LEADER_POD} supported=${pp_sup:-none}"

    kubectl -n "$NS" patch "$MD_RES" "$MD_PP" --type=json \
      -p '[{"op":"add","path":"/spec/roles/0/runtimeControl","value":{"parameters":{"scheduling.ppMaxMicroBatchSize":"4"}}}]' >/dev/null
    gen_pp="$(kubectl -n "$NS" get "$MD_RES" "$MD_PP" -o jsonpath='{.metadata.generation}')"
    if printf '%s' "$pp_sup" | grep -q 'scheduling.ppMaxMicroBatchSize'; then
      if pp_out="$(wait_role_rc "$MD_PP" server "$gen_pp" true false 120)"; then
        pp_val="$(probe_curl "http://${PP_LEADER_IP}:8000/gpustack/v1/runtime_config" 2>/dev/null | python3 -c 'import json,sys; print((json.load(sys.stdin).get("values") or {}).get("scheduling.ppMaxMicroBatchSize"))' 2>/dev/null || true)"
        if [ "$pp_val" = "4" ]; then
          record PASS "Leg 4: scheduling.ppMaxMicroBatchSize live tuning and readback converged" "value=4 ${pp_out}"
        else
          record FAIL "Leg 4: scheduling.ppMaxMicroBatchSize live tuning and readback converged" "got=${pp_val} ${pp_out}"
        fi
      else
        record FAIL "Leg 4: scheduling.ppMaxMicroBatchSize live tuning converged" "$pp_out"
      fi
    else
      if pp_out="$(wait_role_rc "$MD_PP" server "$gen_pp" false false 120)" && printf '%s' "$pp_out" | grep -q 'reason=UnsupportedEngineVersion'; then
        record PASS "Leg 4: scheduling.ppMaxMicroBatchSize reports UnsupportedEngineVersion" "$pp_out"
      else
        record FAIL "Leg 4: scheduling.ppMaxMicroBatchSize reports UnsupportedEngineVersion" "$pp_out"
      fi
    fi

    kubectl -n "$NS" patch "$MD_RES" "$MD_PP" --type=json \
      -p '[{"op":"remove","path":"/spec/roles/0/runtimeControl"}]' >/dev/null
    if pp_clr="$(wait_role_rc_cleared "$MD_PP" server 120)"; then
      pp_uid_after="$(kubectl -n "$NS" get pod "$PP_LEADER_POD" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
      if [ "$pp_uid_after" = "$PP_LEADER_UID" ]; then
        record PASS "Leg 4: clearing runtimeControl resets SGLang PP=2 without restarting leader Pod" "uid=${PP_LEADER_UID}"
      else
        record FAIL "Leg 4: clearing runtimeControl resets SGLang PP=2 without restarting leader Pod" "before=${PP_LEADER_UID} after=${pp_uid_after}"
      fi
    else
      record FAIL "Leg 4: clearing SGLang PP=2 runtimeControl converged" "$pp_clr"
    fi
  else
    record FAIL "Leg 4: SGLang --pp-size=2 deployment reached Ready" "phase=${pp_phase}"
  fi
  delete_and_wait_md "$MD_PP"
fi

# ==============================================================================
# LEG 5: SGLang speculative decoding (--speculative-algorithm=NGRAM)
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
  record PASS "Leg 5: SGLang NGRAM speculative deployment reached Ready" "phase=${spec_phase}"
  SPEC_POD="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD_SPEC},app.kubernetes.io/component=server" -o jsonpath='{.items[0].metadata.name}')"
  SPEC_IP="$(kubectl -n "$NS" get pod "$SPEC_POD" -o jsonpath='{.status.podIP}')"
  SPEC_UID="$(kubectl -n "$NS" get pod "$SPEC_POD" -o jsonpath='{.metadata.uid}')"

  spec_init="$(probe_curl "http://${SPEC_IP}:8000/gpustack/v1/runtime_config" 2>/dev/null || echo '{}')"
  spec_sup="$(printf '%s' "$spec_init" | python3 -c 'import json,sys; print(",".join(json.load(sys.stdin).get("supported_fields") or []))' 2>/dev/null || true)"
  record PASS "Leg 5: SGLang NGRAM sentinel capability discovery" "supported=${spec_sup:-none}"

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
    print("single={} acc={}".format(s, a))
' 2>/dev/null || echo "invalid json")"
      if [ "$sp_check" = "OK" ]; then
        record PASS "Leg 5: speculative acceptThresholdSingle/Acc live tuning and readback converged" "single=0.85 acc=0.75"
      else
        record FAIL "Leg 5: speculative acceptThresholdSingle/Acc live tuning and readback converged" "$sp_check"
      fi
    else
      record FAIL "Leg 5: speculative acceptThresholdSingle/Acc live tuning converged" "$sp_out"
    fi
  else
    if sp_out="$(wait_role_rc "$MD_SPEC" server "$gen_sp" false false 120)" && printf '%s' "$sp_out" | grep -q 'reason=UnsupportedEngineVersion'; then
      record PASS "Leg 5: speculative acceptThresholdSingle/Acc report UnsupportedEngineVersion" "$sp_out"
    else
      record FAIL "Leg 5: speculative acceptThresholdSingle/Acc report UnsupportedEngineVersion" "$sp_out"
    fi
  fi

  kubectl -n "$NS" patch "$MD_RES" "$MD_SPEC" --type=json \
    -p '[{"op":"add","path":"/spec/roles/0/runtimeControl/parameters/speculative.draftBudgetFraction","value":"0.5"}]' >/dev/null
  gen_dbf="$(kubectl -n "$NS" get "$MD_RES" "$MD_SPEC" -o jsonpath='{.metadata.generation}')"
  if printf '%s' "$spec_sup" | grep -q 'speculative.draftBudgetFraction'; then
    if dbf_out="$(wait_role_rc "$MD_SPEC" server "$gen_dbf" true false 120)"; then
      record PASS "Leg 5: speculative.draftBudgetFraction live tuning converged" "$dbf_out"
    else
      record FAIL "Leg 5: speculative.draftBudgetFraction live tuning converged" "$dbf_out"
    fi
  else
    if dbf_out="$(wait_role_rc "$MD_SPEC" server "$gen_dbf" false false 120)" && printf '%s' "$dbf_out" | grep -q 'reason=UnsupportedEngineVersion'; then
      record PASS "Leg 5: speculative.draftBudgetFraction reports UnsupportedEngineVersion on NGRAM" "$dbf_out"
    else
      record FAIL "Leg 5: speculative.draftBudgetFraction reports UnsupportedEngineVersion on NGRAM" "$dbf_out"
    fi
  fi

  kubectl -n "$NS" patch "$MD_RES" "$MD_SPEC" --type=json \
    -p '[{"op":"remove","path":"/spec/roles/0/runtimeControl"}]' >/dev/null
  if sp_clr="$(wait_role_rc_cleared "$MD_SPEC" server 120)"; then
    sp_uid_after="$(kubectl -n "$NS" get pod "$SPEC_POD" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
    if [ "$sp_uid_after" = "$SPEC_UID" ]; then
      record PASS "Leg 5: clearing speculative runtimeControl resets cleanly without restarting Pod" "uid=${SPEC_UID}"
    else
      record FAIL "Leg 5: clearing speculative runtimeControl resets cleanly without restarting Pod" "before=${SPEC_UID} after=${sp_uid_after}"
    fi
  else
    record FAIL "Leg 5: clearing speculative runtimeControl converged" "$sp_clr"
  fi
else
  record FAIL "Leg 5: SGLang NGRAM speculative deployment reached Ready" "phase=${spec_phase}"
fi

delete_and_wait_md "$MD_SPEC"
results
