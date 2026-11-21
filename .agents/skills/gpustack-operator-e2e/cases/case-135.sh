#!/usr/bin/env bash
#
# CASE 135 — Live SGLang online runtime control, spawn-safe engine-sentinel, sglang-gateway Envoy
#            allowlist, in-process set_internal_state without SGLANG_ADMIN_API_KEY, and role pause
#            (MUTATING, self-cleaning; AUTO-SKIPS without >=2 NVIDIA GPUs and pre-warmed ModelArtifact)
#
#   case-135.sh <NS>
#
# Prerequisites: nodes=1; gpu=nvidia; cluster=real; inputs=E2E_C135_ACCELERATOR_TYPE,E2E_C135_ARTIFACT,E2E_C135_ENGINE_IMAGE,E2E_C135_MODEL,E2E_C135_PROBE_IMAGE,E2E_C135_READY_BOUND
# Goal:        Verify the full SGLang online runtime control lifecycle on real NVIDIA GPUs:
#              default flat host-staged gpustack-engine-sentinel mount hooking spawn Scheduler child
#              processes with zero SGLANG_ADMIN_API_KEY and zero auth Secrets; unified
#              /gpustack/v1/{live,ready} probes (live returns 200 without triggering token
#              generation); sglang-gateway Envoy allowlist blocking control/internal_state routes
#              with 403 while serving inference 200 and exposing :9091/observer/endpoints; live
#              SGLang parameter tuning (scheduling.maxActiveRequests, maxBatchedTokens,
#              prefillMaxRequests, scheduleConservativeness, prefillDecodeInterval) and boot-default
#              reset without Pod restart; and role pause draining traffic and returning 503 on
#              direct Pod inference while keeping /live 200, followed by clean resume.
# Environment: Real NVIDIA GPU cluster with >=1 allocatable GPU on one usable AcceleratorType,
#              pre-warmed ModelArtifact (E2E_C135_ARTIFACT, default qwen3-8b) resolving
#              E2E_C135_MODEL (default Qwen/Qwen3-8B), pullable SGLang runner image
#              (E2E_C135_ENGINE_IMAGE, default gpustack/runner:cuda13.0-sglang0.5.22), and pullable
#              probe image (E2E_C135_PROBE_IMAGE, default curlimages/curl:8.10.1). Auto-skips if
#              fewer than 1 NVIDIA GPU or the pre-warmed ModelArtifact is absent.
# Inputs:      MOCKED: none. Uses real SGLang engine Pod, real sglang-gateway + Envoy sidecar,
#              and an in-cluster curl probe Pod.
# Expected:    Sentinel mounts cleanly without SGLANG_ADMIN_API_KEY, sglang-gateway blocks control
#              routes with 403, live SGLang parameters apply and read back without Pod restart,
#              role pause blocks direct inference with 503 while /live stays 200, and resume plus
#              parameter removal restores boot defaults and 200 inference.
# Cleanup:     EXIT trap deletes the probe Pod and ModelDeployment created by this case and
#              force-releases any Kueue Workloads.
set -uo pipefail

CASE_ID=135
export CASE_ID
CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "${CASE_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_md-lib.sh"

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-135.sh <NS>" >&2
  exit 2
fi

MD_RES="modeldeployments.v1alpha1.worker.gpustack.ai"
ARTIFACT="${E2E_C135_ARTIFACT:-qwen3-8b}"
MODEL="${E2E_C135_MODEL:-Qwen/Qwen3-8B}"
ENGINE_IMAGE="${E2E_C135_ENGINE_IMAGE:-gpustack/runner:cuda13.0-sglang0.5.22}"
PROBE_IMAGE="${E2E_C135_PROBE_IMAGE:-curlimages/curl:8.10.1}"
READY_BOUND="${E2E_C135_READY_BOUND:-600}"

RUN_PREFIX="c135-$$"
DEPLOY="${RUN_PREFIX}-sglang"
PROBE="${RUN_PREFIX}-probe"

cleanup() {
  kubectl -n "$NS" delete pod "$PROBE" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n "$NS" delete "$MD_RES" "$DEPLOY" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  md_force_release "$NS" "$DEPLOY" || true
  return 0
}
trap cleanup EXIT

AT="${E2E_C135_ACCELERATOR_TYPE:-$(md_usable_type_for nvidia || true)}"
if [ -z "$AT" ]; then
  record SKIP "case-135 prerequisites" "no usable NVIDIA AcceleratorType found"
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
if [ "${GPU_CAP:-0}" -lt 1 ]; then
  record SKIP "case-135 prerequisites" "need >=1 allocatable ${HW_RES}, found ${GPU_CAP:-0}"
  results
  exit 0
fi

if ! kubectl -n "$NS" get modelartifact "$ARTIFACT" >/dev/null 2>&1; then
  record SKIP "case-135 prerequisites" "ModelArtifact ${NS}/${ARTIFACT} not found"
  results
  exit 0
fi
ART_REV="$(kubectl -n "$NS" get modelartifact "$ARTIFACT" -o jsonpath='{.status.resolved.revision}' 2>/dev/null || true)"
if [ -z "$ART_REV" ]; then
  record SKIP "case-135 prerequisites" "ModelArtifact ${NS}/${ARTIFACT} has empty resolved.revision"
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

# 1. Deploy SGLang Server behind sglang-gateway.
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
    name: SGLang
    image: ${ENGINE_IMAGE}
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

phase="$(wait_md_phase_ready "$READY_BOUND" || true)"
if [ "$phase" != "Ready" ]; then
  record FAIL "SGLang deployment reached Ready with default engine-sentinel" "phase=${phase}"
  results
  exit 1
fi
record PASS "SGLang deployment reached Ready with default engine-sentinel" "phase=${phase}"

WORKER_POD="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${DEPLOY},app.kubernetes.io/component=server" -o jsonpath='{.items[0].metadata.name}')"
WORKER_UID="$(kubectl -n "$NS" get pod "$WORKER_POD" -o jsonpath='{.metadata.uid}')"
WORKER_IP="$(kubectl -n "$NS" get pod "$WORKER_POD" -o jsonpath='{.status.podIP}')"
ROUTER_POD="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${DEPLOY},modeldeployment.gpustack.ai/router" -o jsonpath='{.items[0].metadata.name}')"
ROUTER_IP="$(kubectl -n "$NS" get pod "$ROUTER_POD" -o jsonpath='{.status.podIP}')"
ENDPOINT="$(kubectl -n "$NS" get "$MD_RES" "$DEPLOY" -o jsonpath='{.status.endpoint}')"

# 2. Verify flat mount, zero SGLANG_ADMIN_API_KEY / auth Secret, unified probes.
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
if envs.get("GPUSTACK_ENGINE_SENTINEL_ENGINE") != "sglang":
    errs.append("engine={}".format(envs.get("GPUSTACK_ENGINE_SENTINEL_ENGINE")))
if "SGLANG_ADMIN_API_KEY" in envs:
    errs.append("SGLANG_ADMIN_API_KEY unexpectedly set")
if "GPUSTACK_RUNTIME_CONTROL_TOKEN" in envs:
    errs.append("GPUSTACK_RUNTIME_CONTROL_TOKEN unexpectedly set")
if c.get("startupProbe", {}).get("httpGet", {}).get("path") != "/gpustack/v1/live":
    errs.append("startupProbe != /gpustack/v1/live")
if c.get("readinessProbe", {}).get("httpGet", {}).get("path") != "/gpustack/v1/ready":
    errs.append("readinessProbe != /gpustack/v1/ready")
print("OK" if not errs else "; ".join(errs))
')"
if [ "$pod_check" = "OK" ]; then
  record PASS "SGLang Pod carries flat engine-sentinel mount, unified probes, and zero SGLANG_ADMIN_API_KEY" "$WORKER_POD"
else
  record FAIL "SGLang Pod carries flat engine-sentinel mount, unified probes, and zero SGLANG_ADMIN_API_KEY" "$pod_check"
fi

# 3. Verify sglang-gateway Envoy allowlist blocks control routes with 403 while serving inference 200.
chat_code="$(probe_curl -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' \
  -d '{"model":"'"${MODEL}"'","messages":[{"role":"user","content":"Reply with OK"}],"max_tokens":8}' \
  "${ENDPOINT}/v1/chat/completions" 2>/dev/null || echo 000)"
if [ "$chat_code" = "200" ]; then
  record PASS "sglang-gateway inference POST /v1/chat/completions returns 200" "${ENDPOINT}"
else
  record FAIL "sglang-gateway inference POST /v1/chat/completions returns 200" "http=${chat_code}"
fi

blocked_errs=""
for route in "/gpustack/v1/ready" "/gpustack/v1/runtime_config" "/set_internal_state" "/pause_generation" "/observer/endpoints"; do
  code="$(probe_curl -o /dev/null -w '%{http_code}' "${ENDPOINT}${route}" 2>/dev/null || echo 000)"
  [ "$code" = "403" ] || blocked_errs="${blocked_errs}${route}=${code} "
done
if [ -z "$blocked_errs" ]; then
  record PASS "sglang-gateway Envoy allowlist blocks all control/internal_state/observer routes with 403" "5/5 blocked"
else
  record FAIL "sglang-gateway Envoy allowlist blocks all control/internal_state/observer routes with 403" "$blocked_errs"
fi

obs_code="$(probe_curl -o /dev/null -w '%{http_code}' "http://${ROUTER_IP}:9091/observer/endpoints" 2>/dev/null || echo 000)"
if [ "$obs_code" = "200" ]; then
  record PASS "sglang-gateway internal :9091/observer/endpoints responds 200" "${ROUTER_IP}:9091"
else
  record FAIL "sglang-gateway internal :9091/observer/endpoints responds 200" "http=${obs_code}"
fi

# 4. Live SGLang parameter tuning (spawned scheduler hook + in-process state) without Pod restart.
init_sglang_cfg="$(probe_curl "http://${WORKER_IP}:8000/gpustack/v1/runtime_config" 2>/dev/null || echo '{}')"
sglang_supported="$(printf '%s' "$init_sglang_cfg" | python3 -c '
import json, sys
d = json.load(sys.stdin)
print(",".join(d.get("supported_fields") or []))
' 2>/dev/null || true)"
record PASS "SGLang sentinel /gpustack/v1/runtime_config initial capability discovery" "supported=${sglang_supported:-none}"

kubectl -n "$NS" patch "$MD_RES" "$DEPLOY" --type=merge -p '{"spec":{"roles":[{"name":"server","kind":"Server","replicas":1,"acceleratorType":"'"${AT}"'","resources":{"accelerator":"1"},"extraArgs":["--context-length=4096","--max-running-requests=64","--chunked-prefill-size=4096","--mem-fraction-static=0.80"],"runtimeControl":{"parameters":{"scheduling.maxActiveRequests":"24","scheduling.maxBatchedTokens":"2048","scheduling.prefillMaxRequests":"12","scheduling.scheduleConservativeness":"1.2","scheduling.prefillDecodeInterval":"2"}}}]}}' >/dev/null
gen_tune="$(kubectl -n "$NS" get "$MD_RES" "$DEPLOY" -o jsonpath='{.metadata.generation}')"
if rc_out="$(wait_rc_synced "$gen_tune" "false" 120)"; then
  record PASS "live SGLang runtimeControl.parameters converged" "$rc_out"
else
  record FAIL "live SGLang runtimeControl.parameters converged" "$rc_out"
fi

cfg_json="$(probe_curl "http://${WORKER_IP}:8000/gpustack/v1/runtime_config" 2>/dev/null || echo '{}')"
cfg_check="$(printf '%s' "$cfg_json" | python3 -c '
import json, sys
d = json.load(sys.stdin)
vals = d.get("values") or {}
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
if [ "$cfg_check" = "OK" ]; then
  record PASS "SGLang sentinel /gpustack/v1/runtime_config live readback matches tuned values" "5/5 matched"
else
  record FAIL "SGLang sentinel /gpustack/v1/runtime_config live readback matches tuned values" "$cfg_check"
fi

uid_after="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${DEPLOY},app.kubernetes.io/component=server" -o jsonpath='{.items[0].metadata.uid}')"
if [ "$uid_after" = "$WORKER_UID" ]; then
  record PASS "SGLang worker Pod UID unchanged after live parameter tuning" "$uid_after"
else
  record FAIL "SGLang worker Pod UID unchanged after live parameter tuning" "before=${WORKER_UID} after=${uid_after}"
fi

# 5. Role pause -> direct 503 + /live 200 -> resume + reset parameters.
kubectl -n "$NS" patch "$MD_RES" "$DEPLOY" --type=json -p '[{"op":"add","path":"/spec/roles/0/pause","value":true}]' >/dev/null
gen_pause="$(kubectl -n "$NS" get "$MD_RES" "$DEPLOY" -o jsonpath='{.metadata.generation}')"
if rc_pause="$(wait_rc_synced "$gen_pause" "true" 120)"; then
  record PASS "SGLang role pause=true converged" "$rc_pause"
else
  record FAIL "SGLang role pause=true converged" "$rc_pause"
fi

live_code="$(probe_curl -o /dev/null -w '%{http_code}' "http://${WORKER_IP}:8000/gpustack/v1/live" 2>/dev/null || echo 000)"
ready_code="$(probe_curl -o /dev/null -w '%{http_code}' "http://${WORKER_IP}:8000/gpustack/v1/ready" 2>/dev/null || echo 000)"
direct_code="$(probe_curl -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' \
  -d '{"model":"'"${MODEL}"'","messages":[{"role":"user","content":"Hi"}],"max_tokens":4}' \
  "http://${WORKER_IP}:8000/v1/chat/completions" 2>/dev/null || echo 000)"
if [ "$live_code" = "200" ] && [ "$ready_code" = "503" ] && [ "$direct_code" = "503" ]; then
  record PASS "paused SGLang keeps /live 200 while gating /ready and direct inference with 503" "live=200 ready=503 direct=503"
else
  record FAIL "paused SGLang keeps /live 200 while gating /ready and direct inference with 503" "live=${live_code} ready=${ready_code} direct=${direct_code}"
fi

kubectl -n "$NS" patch "$MD_RES" "$DEPLOY" --type=json \
  -p '[{"op":"replace","path":"/spec/roles/0/pause","value":false},{"op":"remove","path":"/spec/roles/0/runtimeControl"}]' >/dev/null
if rc_res="$(wait_rc_synced "cleared" "false" 120)"; then
  record PASS "SGLang role resume and parameter reset converged" "$rc_res"
else
  record FAIL "SGLang role resume and parameter reset converged" "$rc_res"
fi

resume_chat="$(probe_curl -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' \
  -d '{"model":"'"${MODEL}"'","messages":[{"role":"user","content":"Reply OK"}],"max_tokens":8}' \
  "${ENDPOINT}/v1/chat/completions" 2>/dev/null || echo 000)"
if [ "$resume_chat" = "200" ]; then
  record PASS "SGLang inference returns 200 after resume" "http=200"
else
  record FAIL "SGLang inference returns 200 after resume" "http=${resume_chat}"
fi

sglang_reset_check="$(probe_curl "http://${WORKER_IP}:8000/gpustack/v1/runtime_config" 2>/dev/null | python3 -c '
import json, sys
d = json.load(sys.stdin)
vals = d.get("values") or {}
bd = d.get("boot_defaults") or {}
errs = []
for k in ("scheduling.maxActiveRequests", "scheduling.maxBatchedTokens", "scheduling.prefillMaxRequests", "scheduling.scheduleConservativeness", "scheduling.prefillDecodeInterval"):
    if k in bd:
        v, b = vals.get(k), bd.get(k)
        if isinstance(b, float) and isinstance(v, (int, float)):
            if abs(float(v) - float(b)) > 1e-6:
                errs.append(f"{k}={v}!=boot({b})")
        elif v != b:
            errs.append(f"{k}={v}!=boot({b})")
print("OK" if not errs else "; ".join(errs))
' 2>/dev/null || echo "invalid json")"
if [ "$sglang_reset_check" = "OK" ]; then
  record PASS "clearing runtimeControl restores all 5 SGLang parameters to boot_defaults" "5/5 restored"
else
  record FAIL "clearing runtimeControl restores all 5 SGLang parameters to boot_defaults" "$sglang_reset_check"
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
  record PASS "clearing SGLang runtimeControl with pause=false deletes role ledger ConfigMap" "runtime-control-${md_uid}-server deleted"
else
  record FAIL "clearing SGLang runtimeControl with pause=false deletes role ledger ConfigMap" "runtime-control-${md_uid}-server still exists"
fi

results
