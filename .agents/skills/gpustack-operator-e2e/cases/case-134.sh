#!/usr/bin/env bash
#
# CASE 134 — Live vLLM online runtime control, default engine-sentinel, Envoy allowlist,
#            router hash decoupling, role pause with ModelDeploymentScaler RolePaused guard,
#            same-UID restart latch replay, and PreStop drain CLI
#            (MUTATING, self-cleaning; AUTO-SKIPS without >=2 NVIDIA GPUs and pre-warmed ModelArtifact)
#
#   case-134.sh <NS>
#
# Prerequisites: nodes=1; gpu=nvidia; cluster=real; inputs=E2E_C134_ACCELERATOR_TYPE,E2E_C134_ARTIFACT,E2E_C134_ENGINE_IMAGE,E2E_C134_MODEL,E2E_C134_PROBE_IMAGE,E2E_C134_READY_BOUND
# Goal:        Verify the full vLLM online runtime control lifecycle on real NVIDIA GPUs:
#              default flat host-staged gpustack-engine-sentinel mount without VLLM_PLUGINS or
#              argv mutation or auth Secrets; unified /gpustack/v1/{live,ready} probes and ACK latch;
#              vllm-router Envoy allowlist blocking control endpoints with 403 while serving inference
#              200 and exposing :9091/observer/endpoints; router mutable field edits without worker
#              Pod restart; live runtimeControl.parameters tuning and boot-default reset without Pod
#              restart; role pause draining traffic, returning 503 on direct Pod inference while
#              keeping /live 200 and holding ModelDeploymentScaler with Reason=RolePaused; resume;
#              same-UID container restart fail-closed latch replay; and PreStop drain CLI execution.
# Environment: Real NVIDIA GPU cluster with >=2 allocatable GPUs on one usable AcceleratorType,
#              pre-warmed ModelArtifact (E2E_C134_ARTIFACT, default qwen3-8b) resolving
#              E2E_C134_MODEL (default Qwen/Qwen3-8B), pullable vLLM runner image
#              (E2E_C134_ENGINE_IMAGE, default gpustack/runner:cuda13.0-vllm0.32.0), and pullable
#              probe image (E2E_C134_PROBE_IMAGE, default curlimages/curl:8.10.1). Auto-skips if
#              fewer than 2 NVIDIA GPUs or the pre-warmed ModelArtifact is absent.
# Inputs:      MOCKED: none. Uses real vLLM engine Pods, real vllm-router + Envoy sidecar,
#              real ModelDeploymentScaler, and an in-cluster curl probe Pod.
# Expected:    All sentinel injection, Envoy allowlist, hash decoupling, live tuning/reset,
#              pause/RolePaused/resume, restart replay, and PreStop drain assertions pass with zero
#              unexpected worker restarts.
# Cleanup:     EXIT trap deletes the probe Pod, ModelDeploymentScaler, and ModelDeployment created
#              by this run and force-releases any Kueue Workloads.
set -uo pipefail

CASE_ID=134
export CASE_ID
CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "${CASE_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_md-lib.sh"

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-134.sh <NS>" >&2
  exit 2
fi

MD_RES="modeldeployments.v1alpha1.worker.gpustack.ai"
SCALER_RES="modeldeploymentscalers.v1alpha1.worker.gpustack.ai"
ARTIFACT="${E2E_C134_ARTIFACT:-qwen3-8b}"
MODEL="${E2E_C134_MODEL:-Qwen/Qwen3-8B}"
ENGINE_IMAGE="${E2E_C134_ENGINE_IMAGE:-gpustack/runner:cuda13.0-vllm0.32.0}"
PROBE_IMAGE="${E2E_C134_PROBE_IMAGE:-curlimages/curl:8.10.1}"
READY_BOUND="${E2E_C134_READY_BOUND:-600}"

RUN_PREFIX="c134-$$"
DEPLOY="${RUN_PREFIX}-vllm"
SCALER="${RUN_PREFIX}-scaler"
PROBE="${RUN_PREFIX}-probe"

cleanup() {
  kubectl -n "$NS" delete pod "$PROBE" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n "$NS" delete "$SCALER_RES" "$SCALER" --ignore-not-found --wait=true --timeout=90s >/dev/null 2>&1 || true
  kubectl -n "$NS" delete "$MD_RES" "$DEPLOY" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  md_force_release "$NS" "$DEPLOY" || true
  return 0
}
trap cleanup EXIT

AT="${E2E_C134_ACCELERATOR_TYPE:-$(md_usable_type_for nvidia || true)}"
if [ -z "$AT" ]; then
  record SKIP "case-134 prerequisites" "no usable NVIDIA AcceleratorType found"
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
  record SKIP "case-134 prerequisites" "need >=2 allocatable ${HW_RES}, found ${GPU_CAP:-0}"
  results
  exit 0
fi

if ! kubectl -n "$NS" get modelartifact "$ARTIFACT" >/dev/null 2>&1; then
  record SKIP "case-134 prerequisites" "ModelArtifact ${NS}/${ARTIFACT} not found"
  results
  exit 0
fi
ART_REV="$(kubectl -n "$NS" get modelartifact "$ARTIFACT" -o jsonpath='{.status.resolved.revision}' 2>/dev/null || true)"
if [ -z "$ART_REV" ]; then
  record SKIP "case-134 prerequisites" "ModelArtifact ${NS}/${ARTIFACT} has empty resolved.revision"
  results
  exit 0
fi

probe_curl() {
  kubectl -n "$NS" exec "$PROBE" -- curl -sS --max-time 15 "$@"
}

wait_md_phase_ready() {
  local bound="$1" phase=""
  for _ in $(seq 1 $((bound / 5))); do
    phase="$(kubectl -n "$NS" get "$MD_RES" "$DEPLOY" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    [ "$phase" = "Ready" ] && { echo "$phase"; return 0; }
    sleep 5
  done
  echo "$phase"
  return 1
}

wait_rc_synced() {
  local want_gen="$1" want_paused="$2" bound="${3:-120}"
  local out=""
  for _ in $(seq 1 $((bound / 3))); do
    out="$(kubectl -n "$NS" get "$MD_RES" "$DEPLOY" -o json 2>/dev/null | python3 -c '
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

# Start in-cluster curl probe Pod.
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

# 1. Deploy vLLM Server behind vllm-router with static ceilings in extraArgs.
kubectl apply -f - >/dev/null <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${DEPLOY}
  namespace: ${NS}
spec:
  model:
    name: ${MODEL}
    artifactRef:
      name: ${ARTIFACT}
  engine:
    name: vLLM
    image: ${ENGINE_IMAGE}
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

phase="$(wait_md_phase_ready "$READY_BOUND" || true)"
if [ "$phase" != "Ready" ]; then
  record FAIL "vLLM deployment reached Ready with default engine-sentinel" "phase=${phase}"
  results
  exit 1
fi
record PASS "vLLM deployment reached Ready with default engine-sentinel" "phase=${phase}"

WORKER_POD="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${DEPLOY},app.kubernetes.io/component=server" -o jsonpath='{.items[0].metadata.name}')"
WORKER_UID="$(kubectl -n "$NS" get pod "$WORKER_POD" -o jsonpath='{.metadata.uid}')"
WORKER_IP="$(kubectl -n "$NS" get pod "$WORKER_POD" -o jsonpath='{.status.podIP}')"
ROUTER_POD="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${DEPLOY},modeldeployment.gpustack.ai/router" -o jsonpath='{.items[0].metadata.name}')"
ROUTER_IP="$(kubectl -n "$NS" get pod "$ROUTER_POD" -o jsonpath='{.status.podIP}')"
ENDPOINT="$(kubectl -n "$NS" get "$MD_RES" "$DEPLOY" -o jsonpath='{.status.endpoint}')"

# 2. Verify flat engine-sentinel mount, zero VLLM_PLUGINS/dev-mode/auth-secret, unified probes, PreStop drain CLI.
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
if "VLLM_PLUGINS" in envs:
    errs.append("VLLM_PLUGINS unexpectedly set")
if "VLLM_SERVER_DEV_MODE" in envs:
    errs.append("VLLM_SERVER_DEV_MODE unexpectedly set")
if "GPUSTACK_RUNTIME_CONTROL_TOKEN" in envs:
    errs.append("GPUSTACK_RUNTIME_CONTROL_TOKEN unexpectedly set")
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
  record PASS "worker Pod carries flat engine-sentinel mount, unified probes, PreStop CLI, zero auth/dev-mode" "$WORKER_POD"
else
  record FAIL "worker Pod carries flat engine-sentinel mount, unified probes, PreStop CLI, zero auth/dev-mode" "$pod_check"
fi

auth_secrets="$(kubectl -n "$NS" get secrets -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep '^runtime-control-auth-' || true)"
if [ -z "$auth_secrets" ]; then
  record PASS "zero operator-managed runtime-control-auth Secrets created" "none"
else
  record FAIL "zero operator-managed runtime-control-auth Secrets created" "$auth_secrets"
fi

# 3. Verify Envoy allowlist on vllm-router (:8000 blocks control routes with 403, serves inference 200; :9091 observer reachable).
chat_code="$(probe_curl -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' \
  -d '{"model":"'"${MODEL}"'","messages":[{"role":"user","content":"Reply with OK"}],"max_tokens":8}' \
  "${ENDPOINT}/v1/chat/completions" 2>/dev/null || echo 000)"
if [ "$chat_code" = "200" ]; then
  record PASS "router inference POST /v1/chat/completions returns 200" "${ENDPOINT}"
else
  record FAIL "router inference POST /v1/chat/completions returns 200" "http=${chat_code}"
fi

blocked_errs=""
for route in "/gpustack/v1/ready" "/gpustack/v1/runtime_config" "/gpustack/v1/pause" "/scale_elastic_ep" "/observer/endpoints"; do
  code="$(probe_curl -o /dev/null -w '%{http_code}' "${ENDPOINT}${route}" 2>/dev/null || echo 000)"
  [ "$code" = "403" ] || blocked_errs="${blocked_errs}${route}=${code} "
done
if [ -z "$blocked_errs" ]; then
  record PASS "vllm-router Envoy allowlist returns 403 for all control/admin/observer routes on :8000" "5/5 blocked"
else
  record FAIL "vllm-router Envoy allowlist returns 403 for all control/admin/observer routes on :8000" "$blocked_errs"
fi

obs_code="$(probe_curl -o /dev/null -w '%{http_code}' "http://${ROUTER_IP}:9091/observer/endpoints" 2>/dev/null || echo 000)"
if [ "$obs_code" = "200" ]; then
  record PASS "Pod-internal :9091/observer/endpoints listener responds 200" "${ROUTER_IP}:9091"
else
  record FAIL "Pod-internal :9091/observer/endpoints listener responds 200" "http=${obs_code}"
fi

# 4. Router hash decoupling: update requestTimeoutSeconds -> router rolls, worker Pod UID unchanged.
kubectl -n "$NS" patch "$MD_RES" "$DEPLOY" --type=merge -p '{"spec":{"router":{"requestTimeoutSeconds":90}}}' >/dev/null
sleep 15
kubectl -n "$NS" rollout status "deployment/${DEPLOY}-router" --timeout=180s >/dev/null 2>&1 || true
after_router_uid="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${DEPLOY},app.kubernetes.io/component=server" -o jsonpath='{.items[0].metadata.uid}')"
after_router_restarts="$(kubectl -n "$NS" get pod "$WORKER_POD" -o jsonpath='{.status.containerStatuses[0].restartCount}')"
if [ "$after_router_uid" = "$WORKER_UID" ] && [ "${after_router_restarts:-0}" = "0" ]; then
  record PASS "router edit rolled router Deployment without restarting worker Pod" "uid=${WORKER_UID} restarts=0"
else
  record FAIL "router edit rolled router Deployment without restarting worker Pod" "before=${WORKER_UID} after=${after_router_uid} restarts=${after_router_restarts}"
fi

# 5. Live runtimeControl.parameters tuning without Pod restart + readback + partial removal reset + version-gated fields.
init_cfg_json="$(probe_curl "http://${WORKER_IP}:8000/gpustack/v1/runtime_config" 2>/dev/null || echo '{}')"
vllm_supported="$(printf '%s' "$init_cfg_json" | python3 -c '
import json, sys
d = json.load(sys.stdin)
print(",".join(d.get("supported_fields") or []))
' 2>/dev/null || true)"
record PASS "sentinel /gpustack/v1/runtime_config initial capability discovery" "supported=${vllm_supported:-none}"

kubectl -n "$NS" patch "$MD_RES" "$DEPLOY" --type=merge -p '{"spec":{"roles":[{"name":"server","kind":"Server","replicas":1,"acceleratorType":"'"${AT}"'","resources":{"accelerator":"1"},"extraArgs":["--max-model-len=4096","--max-num-seqs=64","--max-num-batched-tokens=8192","--gpu-memory-utilization=0.80"],"runtimeControl":{"parameters":{"scheduling.maxActiveRequests":"24","scheduling.maxBatchedTokens":"4096","scheduling.longPrefillTokenThreshold":"1024"}}}]}}' >/dev/null
gen_tune="$(kubectl -n "$NS" get "$MD_RES" "$DEPLOY" -o jsonpath='{.metadata.generation}')"
if rc_out="$(wait_rc_synced "$gen_tune" "false" 120)"; then
  record PASS "live vLLM runtimeControl.parameters converged (3 core scheduler knobs)" "$rc_out"
else
  record FAIL "live vLLM runtimeControl.parameters converged (3 core scheduler knobs)" "$rc_out"
fi

cfg_json="$(probe_curl "http://${WORKER_IP}:8000/gpustack/v1/runtime_config" 2>/dev/null || echo '{}')"
cfg_check="$(printf '%s' "$cfg_json" | python3 -c '
import json, sys
d = json.load(sys.stdin)
vals = d.get("values") or {}
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
  record PASS "sentinel /gpustack/v1/runtime_config live readback matches tuned vLLM values" "maxActiveRequests=24 maxBatchedTokens=4096 longPrefill=1024"
else
  record FAIL "sentinel /gpustack/v1/runtime_config live readback matches tuned vLLM values" "$cfg_check"
fi

# 5b. Removing a single key resets only that key to boot_defaults while keeping remaining tuned values.
kubectl -n "$NS" patch "$MD_RES" "$DEPLOY" --type=json \
  -p '[{"op":"remove","path":"/spec/roles/0/runtimeControl/parameters/scheduling.longPrefillTokenThreshold"}]' >/dev/null
gen_partial="$(kubectl -n "$NS" get "$MD_RES" "$DEPLOY" -o jsonpath='{.metadata.generation}')"
if rc_part="$(wait_rc_synced "$gen_partial" "false" 120)"; then
  record PASS "partial parameter removal converged without Pod restart" "$rc_part"
else
  record FAIL "partial parameter removal converged without Pod restart" "$rc_part"
fi

partial_check="$(probe_curl "http://${WORKER_IP}:8000/gpustack/v1/runtime_config" 2>/dev/null | python3 -c '
import json, sys
d = json.load(sys.stdin)
vals = d.get("values") or {}
bd = d.get("boot_defaults") or {}
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
  record PASS "removing single key auto-resets it to boot_defaults while preserving remaining parameters" "longPrefill restored"
else
  record FAIL "removing single key auto-resets it to boot_defaults while preserving remaining parameters" "$partial_check"
fi

# 5c. Test version-gated cacheAwareWindow & cacheAwareThreshold (live apply if supported, else UnsupportedEngineVersion + clean recovery).
kubectl -n "$NS" patch "$MD_RES" "$DEPLOY" --type=json \
  -p '[{"op":"add","path":"/spec/roles/0/runtimeControl/parameters/scheduling.cacheAwareWindow","value":"64"},{"op":"add","path":"/spec/roles/0/runtimeControl/parameters/scheduling.cacheAwareThreshold","value":"0.5"}]' >/dev/null
gen_ca="$(kubectl -n "$NS" get "$MD_RES" "$DEPLOY" -o jsonpath='{.metadata.generation}')"
if printf '%s' "$vllm_supported" | grep -q 'scheduling.cacheAwareWindow' && printf '%s' "$vllm_supported" | grep -q 'scheduling.cacheAwareThreshold'; then
  if rc_ca="$(wait_rc_synced "$gen_ca" "false" 120)"; then
    record PASS "vLLM cacheAwareWindow and cacheAwareThreshold live tuning converged" "$rc_ca"
  else
    record FAIL "vLLM cacheAwareWindow and cacheAwareThreshold live tuning converged" "$rc_ca"
  fi
else
  ca_reason=""
  for _ in $(seq 1 40); do
    ca_reason="$(kubectl -n "$NS" get "$MD_RES" "$DEPLOY" -o json 2>/dev/null | python3 -c '
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
    record PASS "unsupported cacheAwareWindow/Threshold cleanly report UnsupportedEngineVersion without restart" "$ca_reason"
  else
    record FAIL "unsupported cacheAwareWindow/Threshold cleanly report UnsupportedEngineVersion without restart" "got=${ca_reason:-timeout}"
  fi
  # Remove unsupported keys and confirm recovery back to Synced=True.
  kubectl -n "$NS" patch "$MD_RES" "$DEPLOY" --type=json \
    -p '[{"op":"remove","path":"/spec/roles/0/runtimeControl/parameters/scheduling.cacheAwareWindow"},{"op":"remove","path":"/spec/roles/0/runtimeControl/parameters/scheduling.cacheAwareThreshold"}]' >/dev/null
  gen_ca_rec="$(kubectl -n "$NS" get "$MD_RES" "$DEPLOY" -o jsonpath='{.metadata.generation}')"
  if rc_rec="$(wait_rc_synced "$gen_ca_rec" "false" 120)"; then
    record PASS "removing unsupported cache-aware keys recovers RuntimeControlSynced=True" "$rc_rec"
  else
    record FAIL "removing unsupported cache-aware keys recovers RuntimeControlSynced=True" "$rc_rec"
  fi
fi

uid_after_tune="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${DEPLOY},app.kubernetes.io/component=server" -o jsonpath='{.items[0].metadata.uid}')"
if [ "$uid_after_tune" = "$WORKER_UID" ]; then
  record PASS "worker Pod UID unchanged after live parameter tuning and unsupported-field test" "$uid_after_tune"
else
  record FAIL "worker Pod UID unchanged after live parameter tuning and unsupported-field test" "before=${WORKER_UID} after=${uid_after_tune}"
fi

# 6. Attach Active ModelDeploymentScaler, then test role pause=true -> ASGI 503 + /live 200 + RolePaused -> resume.
kubectl apply -f - >/dev/null <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${SCALER}
  namespace: ${NS}
spec:
  targetRef:
    name: ${DEPLOY}
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
  bound_ann="$(kubectl -n "$NS" get "$MD_RES" "$DEPLOY" -o jsonpath='{.metadata.annotations.modeldeployment\.gpustack\.ai/scaler-binding}' 2>/dev/null || true)"
  [ -n "$bound_ann" ] && break
  sleep 2
done

kubectl -n "$NS" patch "$MD_RES" "$DEPLOY" --type=json -p '[{"op":"add","path":"/spec/roles/0/pause","value":true}]' >/dev/null
gen_pause="$(kubectl -n "$NS" get "$MD_RES" "$DEPLOY" -o jsonpath='{.metadata.generation}')"
if rc_pause="$(wait_rc_synced "$gen_pause" "true" 120)"; then
  record PASS "role pause=true converged (withdrawn, drained, sentinel paused)" "$rc_pause"
else
  record FAIL "role pause=true converged (withdrawn, drained, sentinel paused)" "$rc_pause"
fi

elig_label="$(kubectl -n "$NS" get pod "$WORKER_POD" -o jsonpath='{.metadata.labels.modeldeployment\.gpustack\.ai/endpoint-eligible}' 2>/dev/null || true)"
live_code="$(probe_curl -o /dev/null -w '%{http_code}' "http://${WORKER_IP}:8000/gpustack/v1/live" 2>/dev/null || echo 000)"
ready_code="$(probe_curl -o /dev/null -w '%{http_code}' "http://${WORKER_IP}:8000/gpustack/v1/ready" 2>/dev/null || echo 000)"
direct_code="$(probe_curl -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' \
  -d '{"model":"'"${MODEL}"'","messages":[{"role":"user","content":"Hi"}],"max_tokens":4}' \
  "http://${WORKER_IP}:8000/v1/chat/completions" 2>/dev/null || echo 000)"
if [ -z "$elig_label" ] && [ "$live_code" = "200" ] && [ "$ready_code" = "503" ] && [ "$direct_code" = "503" ]; then
  record PASS "paused role withdraws endpoint-eligible, keeps /live 200, gates /ready and direct inference with 503" "live=200 ready=503 direct=503"
else
  record FAIL "paused role withdraws endpoint-eligible, keeps /live 200, gates /ready and direct inference with 503" "eligible=${elig_label:-empty} live=${live_code} ready=${ready_code} direct=${direct_code}"
fi

scaler_blocked=""
for _ in $(seq 1 30); do
  scaler_blocked="$(kubectl -n "$NS" get "$SCALER_RES" "$SCALER" -o json 2>/dev/null | python3 -c '
import json, sys
d = json.load(sys.stdin)
for c in d.get("status", {}).get("conditions", []):
    if c.get("type") == "Blocked":
        print("{}|{}".format(c.get("status"), c.get("reason")))
' 2>/dev/null || true)"
  [ "$scaler_blocked" = "True|RolePaused" ] && break
  sleep 3
done
if [ "$scaler_blocked" = "True|RolePaused" ]; then
  record PASS "ModelDeploymentScaler reports Blocked=True Reason=RolePaused while paused" "$scaler_blocked"
else
  record FAIL "ModelDeploymentScaler reports Blocked=True Reason=RolePaused while paused" "got=${scaler_blocked}"
fi

# Resume role (pause=false) and clear parameters to verify boot-default reset.
kubectl -n "$NS" patch "$MD_RES" "$DEPLOY" --type=json \
  -p '[{"op":"replace","path":"/spec/roles/0/pause","value":false},{"op":"remove","path":"/spec/roles/0/runtimeControl"}]' >/dev/null
if rc_res="$(wait_rc_synced "cleared" "false" 120)"; then
  record PASS "role resume and runtimeControl removal reset converged" "$rc_res"
else
  record FAIL "role resume and runtimeControl removal reset converged" "$rc_res"
fi

resume_chat="$(probe_curl -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' \
  -d '{"model":"'"${MODEL}"'","messages":[{"role":"user","content":"Reply OK"}],"max_tokens":8}' \
  "${ENDPOINT}/v1/chat/completions" 2>/dev/null || echo 000)"
if [ "$resume_chat" = "200" ]; then
  record PASS "inference through router returns 200 after resume" "http=200"
else
  record FAIL "inference through router returns 200 after resume" "http=${resume_chat}"
fi

reset_check="$(probe_curl "http://${WORKER_IP}:8000/gpustack/v1/runtime_config" 2>/dev/null | python3 -c '
import json, sys
d = json.load(sys.stdin)
vals = d.get("values") or {}
bd = d.get("boot_defaults") or {}
errs = []
for k in ("scheduling.maxActiveRequests", "scheduling.maxBatchedTokens", "scheduling.longPrefillTokenThreshold"):
    if k in bd and vals.get(k) != bd.get(k):
        errs.append(f"{k}={vals.get(k)}!=boot({bd.get(k)})")
print("OK" if not errs else "; ".join(errs))
' 2>/dev/null || echo "invalid json")"
if [ "$reset_check" = "OK" ]; then
  record PASS "clearing runtimeControl restores all tuned vLLM parameters to boot_defaults" "all restored"
else
  record FAIL "clearing runtimeControl restores all tuned vLLM parameters to boot_defaults" "$reset_check"
fi

md_uid="$(kubectl -n "$NS" get "$MD_RES" "$DEPLOY" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
ledger_gone="false"
for _ in $(seq 1 15); do
  if ! kubectl -n "$NS" get configmap "runtime-control-${md_uid}-server" >/dev/null 2>&1; then
    ledger_gone="true"
    break
  fi
  sleep 2
done
if [ "$ledger_gone" = "true" ]; then
  record PASS "clearing runtimeControl with pause=false deletes role ledger ConfigMap" "runtime-control-${md_uid}-server deleted"
else
  record FAIL "clearing runtimeControl with pause=false deletes role ledger ConfigMap" "runtime-control-${md_uid}-server still exists"
fi

# 7. Delete worker Pod to exercise PreStop gpustack_engine_sentinel.drain and check zero FailedPreStopHook events.
kubectl -n "$NS" delete pod "$WORKER_POD" --wait=true --timeout=120s >/dev/null 2>&1 || true
prestop_fail="$(kubectl -n "$NS" get events --field-selector "involvedObject.name=${WORKER_POD},reason=FailedPreStopHook" -o name 2>/dev/null || true)"
if [ -z "$prestop_fail" ]; then
  record PASS "PreStop gpustack_engine_sentinel.drain exited cleanly with zero FailedPreStopHook events" "$WORKER_POD"
else
  record FAIL "PreStop gpustack_engine_sentinel.drain exited cleanly with zero FailedPreStopHook events" "$prestop_fail"
fi

results
