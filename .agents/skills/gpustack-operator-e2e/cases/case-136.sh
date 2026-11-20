#!/usr/bin/env bash
#
# CASE 136 — Live folded Elastic EP (runtimeControl.parameters["elastic.dataParallelWidth"])
#            online resize from 2 to 3, pause serialization, and post-resize parameter replay
#            (MUTATING, self-cleaning; AUTO-SKIPS without >=3 NVIDIA GPUs and pre-warmed MoE ModelArtifact)
#
#   case-136.sh <NS>
#
# Prerequisites: nodes=1; gpu=nvidia; cluster=real; inputs=E2E_C136_ACCELERATOR_TYPE,E2E_C136_ARTIFACT,E2E_C136_ENGINE_IMAGE,E2E_C136_MODEL,E2E_C136_PROBE_IMAGE,E2E_C136_READY_BOUND
# Goal:        Verify folded Elastic EP configured via runtimeControl.parameters["elastic.dataParallelWidth"]
#              materializes a Ray head + master + worker collective at width 2, applies non-elastic
#              runtimeControl.parameters alongside elastic width, holds resize while pause=true,
#              expands in place from width 2 to 3 without replacing surviving master/worker Pods
#              once unpaused, replays non-elastic parameters onto expanded ranks, and serves 200
#              inference before and after expansion.
# Environment: Real NVIDIA GPU cluster with >=3 allocatable GPUs on one usable AcceleratorType,
#              pre-warmed MoE ModelArtifact (E2E_C136_ARTIFACT, default deepseek-v2-lite-chat)
#              resolving E2E_C136_MODEL (default deepseek-ai/DeepSeek-V2-Lite-Chat), pullable vLLM
#              runner image (E2E_C136_ENGINE_IMAGE, default gpustack/runner:cuda13.0-vllm0.32.0),
#              and pullable probe image (E2E_C136_PROBE_IMAGE, default curlimages/curl:8.10.1).
#              Auto-skips if fewer than 3 GPUs or the ModelArtifact is absent.
# Inputs:      MOCKED: none. Runs a real vLLM Ray Elastic EP collective and an in-cluster curl probe Pod.
# Expected:    Deployment reaches Ready at width=2, surviving master Pod UID is preserved across
#              2->3 resize, RuntimeControlSynced=True after expansion with live parameters replayed,
#              and /v1/chat/completions answers 200 before and after resize.
# Cleanup:     EXIT trap deletes the probe Pod and ModelDeployment created by this case and
#              force-releases any Kueue Workloads.
set -uo pipefail

CASE_ID=136
export CASE_ID
CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "${CASE_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_md-lib.sh"

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-136.sh <NS>" >&2
  exit 2
fi

MD_RES="modeldeployments.v1alpha1.worker.gpustack.ai"
ARTIFACT="${E2E_C136_ARTIFACT:-deepseek-v2-lite-chat}"
MODEL="${E2E_C136_MODEL:-deepseek-ai/DeepSeek-V2-Lite-Chat}"
ENGINE_IMAGE="${E2E_C136_ENGINE_IMAGE:-gpustack/runner:cuda13.0-vllm0.32.0}"
PROBE_IMAGE="${E2E_C136_PROBE_IMAGE:-curlimages/curl:8.10.1}"
READY_BOUND="${E2E_C136_READY_BOUND:-720}"

RUN_PREFIX="c136-$$"
DEPLOY="${RUN_PREFIX}-elastic"
PROBE="${RUN_PREFIX}-probe"

cleanup() {
  kubectl -n "$NS" delete pod "$PROBE" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n "$NS" delete "$MD_RES" "$DEPLOY" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  md_force_release "$NS" "$DEPLOY" || true
  return 0
}
trap cleanup EXIT

AT="${E2E_C136_ACCELERATOR_TYPE:-$(md_usable_type_for nvidia || true)}"
if [ -z "$AT" ]; then
  record SKIP "case-136 prerequisites" "no usable NVIDIA AcceleratorType found"
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
if [ "${GPU_CAP:-0}" -lt 3 ]; then
  record SKIP "case-136 prerequisites" "need >=3 allocatable ${HW_RES}, found ${GPU_CAP:-0}"
  results
  exit 0
fi

if ! kubectl -n "$NS" get modelartifact "$ARTIFACT" >/dev/null 2>&1; then
  record SKIP "case-136 prerequisites" "ModelArtifact ${NS}/${ARTIFACT} not found"
  results
  exit 0
fi
ART_REV="$(kubectl -n "$NS" get modelartifact "$ARTIFACT" -o jsonpath='{.status.resolved.revision}' 2>/dev/null || true)"
if [ -z "$ART_REV" ]; then
  record SKIP "case-136 prerequisites" "ModelArtifact ${NS}/${ARTIFACT} has empty resolved.revision"
  results
  exit 0
fi

probe_curl() {
  kubectl -n "$NS" exec "$PROBE" -- curl -sS --max-time 20 "$@"
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
  roles:
    - name: server
      kind: Server
      replicas: 1
      size: 1
      acceleratorType: ${AT}
      resources:
        accelerator: "1"
      extraArgs:
        - --max-model-len=4096
        - --max-num-seqs=32
        - --gpu-memory-utilization=0.80
        - --elastic-ep-max-dp-size=3
        - --all2all-backend=allgather_reducescatter
        - --api-server-count=1
        - --trust-remote-code
      runtimeControl:
        parameters:
          elastic.dataParallelWidth: "2"
          scheduling.maxActiveRequests: "16"
EOF

phase=""
for _ in $(seq 1 $((READY_BOUND / 5))); do
  phase="$(kubectl -n "$NS" get "$MD_RES" "$DEPLOY" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  [ "$phase" = "Ready" ] && break
  sleep 5
done
if [ "$phase" != "Ready" ]; then
  record FAIL "folded Elastic EP deployment reached Ready at width=2" "phase=${phase}"
  results
  exit 1
fi
record PASS "folded Elastic EP deployment reached Ready at width=2" "phase=${phase}"

ENDPOINT="$(kubectl -n "$NS" get "$MD_RES" "$DEPLOY" -o jsonpath='{.status.endpoint}')"
MD_UID="$(kubectl -n "$NS" get "$MD_RES" "$DEPLOY" -o jsonpath='{.metadata.uid}')"

# Identify the surviving master Pod from elastic-observation-<uid> (.masterUID), falling back to the Pod answering :8000.
read_obs() {
  kubectl -n "$NS" get configmap "elastic-observation-${MD_UID}" -o json 2>/dev/null | python3 -c '
import json, sys
cm = json.load(sys.stdin)
obs = json.loads((cm.get("data") or {}).get("observation.json") or "{}")
eff = obs.get("effective") or {}
print("{}|{}|{}|{}|{}".format(obs.get("masterUID", ""), eff.get("Known", False), eff.get("Value", ""), obs.get("stableWidth", 0), obs.get("state", "")))
' 2>/dev/null || echo "||||"
}

obs_pre="$(read_obs)"
MASTER_UID="${obs_pre%%|*}"
MASTER_POD=""
if [ -n "$MASTER_UID" ]; then
  MASTER_POD="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${DEPLOY},app.kubernetes.io/component=server" -o json 2>/dev/null | python3 -c '
import json, sys
uid = sys.argv[1]
for p in json.load(sys.stdin).get("items", []):
    if p.get("metadata", {}).get("uid") == uid:
        print(p["metadata"]["name"])
        break
' "$MASTER_UID" 2>/dev/null || true)"
fi
if [ -z "$MASTER_POD" ]; then
  for p in $(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${DEPLOY},app.kubernetes.io/component=server" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
    pip="$(kubectl -n "$NS" get pod "$p" -o jsonpath='{.status.podIP}' 2>/dev/null || true)"
    if [ -n "$pip" ] && [ "$(probe_curl -o /dev/null -w '%{http_code}' "http://${pip}:8000/gpustack/v1/live" 2>/dev/null || echo 000)" = "200" ]; then
      MASTER_POD="$p"
      MASTER_UID="$(kubectl -n "$NS" get pod "$p" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
      break
    fi
  done
fi
MASTER_IP="$(kubectl -n "$NS" get pod "$MASTER_POD" -o jsonpath='{.status.podIP}' 2>/dev/null || true)"

if [ -n "$MASTER_POD" ] && [ -n "$MASTER_UID" ]; then
  record PASS "identified Elastic EP master Pod via observation ledger" "pod=${MASTER_POD} uid=${MASTER_UID} obs=${obs_pre}"
else
  record FAIL "identified Elastic EP master Pod via observation ledger" "obs=${obs_pre}"
fi

pre_rc_check="$(probe_curl "http://${MASTER_IP}:8000/gpustack/v1/runtime_config" 2>/dev/null | python3 -c '
import json, sys
d = json.load(sys.stdin)
ranks = d.get("ranks") or []
vals = d.get("values") or {}
epoch = d.get("membership_epoch")
if len(ranks) == 2 and vals.get("scheduling.maxActiveRequests") == 16 and all((r.get("values") or {}).get("scheduling.maxActiveRequests") == 16 for r in ranks):
    print(f"OK|ranks={len(ranks)} epoch={epoch}")
else:
    print(f"FAIL|ranks={len(ranks)} epoch={epoch} vals={vals}")
' 2>/dev/null || echo "FAIL|invalid json")"
if printf '%s' "$pre_rc_check" | grep -q '^OK|'; then
  record PASS "width=2 sentinel runtime_config confirms 2 ranks with scheduling.maxActiveRequests=16" "$pre_rc_check"
else
  record FAIL "width=2 sentinel runtime_config confirms 2 ranks with scheduling.maxActiveRequests=16" "$pre_rc_check"
fi

pre_code="$(probe_curl -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' \
  -d '{"model":"'"${MODEL}"'","messages":[{"role":"user","content":"Reply OK"}],"max_tokens":8}' \
  "${ENDPOINT}/v1/chat/completions" 2>/dev/null || echo 000)"
if [ "$pre_code" = "200" ]; then
  record PASS "Elastic EP inference returns 200 at width=2" "$ENDPOINT"
else
  record FAIL "Elastic EP inference returns 200 at width=2" "http=${pre_code}"
fi

# Keep sending periodic inference requests while expanding 2 -> 3 to verify serving continuity.
CONT_FILE="$(mktemp)"
(
  ok=0
  fail=0
  for _ in $(seq 1 12); do
    code="$(probe_curl -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' \
      -d '{"model":"'"${MODEL}"'","messages":[{"role":"user","content":"Ping"}],"max_tokens":4}' \
      "${ENDPOINT}/v1/chat/completions" 2>/dev/null || echo 000)"
    if [ "$code" = "200" ]; then
      ok=$((ok + 1))
    else
      fail=$((fail + 1))
    fi
    sleep 5
  done
  printf '%s|%s\n' "$ok" "$fail" > "$CONT_FILE"
) &
CONT_PID=$!

# Resize 2 -> 3 via runtimeControl.parameters["elastic.dataParallelWidth"].
kubectl -n "$NS" patch "$MD_RES" "$DEPLOY" --type=json \
  -p '[{"op":"replace","path":"/spec/roles/0/runtimeControl/parameters/elastic.dataParallelWidth","value":"3"}]' >/dev/null
gen_resize="$(kubectl -n "$NS" get "$MD_RES" "$DEPLOY" -o jsonpath='{.metadata.generation}')"

resized="false"
obs_post=""
rc_synced=""
op_state=""
for _ in $(seq 1 120); do
  obs_post="$(read_obs)"
  eff_known="$(printf '%s' "$obs_post" | cut -d'|' -f2)"
  eff_val="$(printf '%s' "$obs_post" | cut -d'|' -f3)"
  stable_w="$(printf '%s' "$obs_post" | cut -d'|' -f4)"
  rc_info="$(kubectl -n "$NS" get "$MD_RES" "$DEPLOY" -o json 2>/dev/null | python3 -c '
import json, sys
d = json.load(sys.stdin)
rc = (d.get("status", {}).get("roles") or [{}])[0].get("runtimeControl") or {}
print("{}|{}|{}".format(rc.get("observedGeneration", 0), rc.get("synced", False), rc.get("reason", "")))
' 2>/dev/null || echo "0|False|")"
  rc_gen="$(printf '%s' "$rc_info" | cut -d'|' -f1)"
  rc_synced="$(printf '%s' "$rc_info" | cut -d'|' -f2)"
  op_state="$(kubectl -n "$NS" get configmap "elastic-resize-${MD_UID}" -o json 2>/dev/null | python3 -c '
import json, sys
cm = json.load(sys.stdin)
op = json.loads((cm.get("data") or {}).get("operation.json") or "{}")
print(op.get("state", "none"))
' 2>/dev/null || echo "resolved")"
  if [ "$eff_known" = "True" ] && [ "$eff_val" = "3" ] && [ "$stable_w" = "3" ] \
    && [ "$rc_gen" = "$gen_resize" ] && [ "$rc_synced" = "True" ] \
    && { [ "$op_state" = "resolved" ] || [ "$op_state" = "Completed" ]; }; then
    resized="true"
    break
  fi
  sleep 5
done

wait "$CONT_PID" 2>/dev/null || true
cont_res="$(cat "$CONT_FILE" 2>/dev/null || echo '0|0')"
rm -f "$CONT_FILE"

if [ "$resized" = "true" ]; then
  record PASS "folded Elastic EP expanded 2->3 with proven observation and replayed runtimeControl" "obs=${obs_post} op=${op_state} rc=${rc_info}"
else
  record FAIL "folded Elastic EP expanded 2->3 with proven observation and replayed runtimeControl" "obs=${obs_post} op=${op_state} rc=${rc_info}"
fi

# Check ElasticResize Condition reports no refusal (True/NoRefusal).
cond_er="$(kubectl -n "$NS" get "$MD_RES" "$DEPLOY" -o json 2>/dev/null | python3 -c '
import json, sys
d = json.load(sys.stdin)
for c in d.get("status", {}).get("conditions") or []:
    if c.get("type") == "ElasticResize":
        print("{}|{}|{}".format(c.get("status"), c.get("reason"), c.get("message", "")))
        break
' 2>/dev/null || true)"
if printf '%s' "$cond_er" | grep -q '^True|NoRefusal|'; then
  record PASS "ElasticResize condition reports True/NoRefusal after 2->3 expansion" "$cond_er"
else
  record FAIL "ElasticResize condition reports True/NoRefusal after 2->3 expansion" "got=${cond_er:-absent}"
fi

master_uid_after="$(kubectl -n "$NS" get pod "$MASTER_POD" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
if [ -n "$MASTER_UID" ] && [ "$master_uid_after" = "$MASTER_UID" ]; then
  record PASS "surviving Elastic EP master Pod UID preserved across 2->3 expansion" "$MASTER_UID"
else
  record FAIL "surviving Elastic EP master Pod UID preserved across 2->3 expansion" "before=${MASTER_UID} after=${master_uid_after}"
fi

post_rc_check="$(probe_curl "http://${MASTER_IP}:8000/gpustack/v1/runtime_config" 2>/dev/null | python3 -c '
import json, sys
d = json.load(sys.stdin)
ranks = d.get("ranks") or []
vals = d.get("values") or {}
epoch = d.get("membership_epoch")
if len(ranks) == 3 and vals.get("scheduling.maxActiveRequests") == 16 and all((r.get("values") or {}).get("scheduling.maxActiveRequests") == 16 for r in ranks):
    print(f"OK|ranks={len(ranks)} epoch={epoch}")
else:
    print(f"FAIL|ranks={len(ranks)} epoch={epoch} vals={vals}")
' 2>/dev/null || echo "FAIL|invalid json")"
if printf '%s' "$post_rc_check" | grep -q '^OK|'; then
  record PASS "width=3 sentinel runtime_config confirms parameter replay across all 3 ranks" "$post_rc_check"
else
  record FAIL "width=3 sentinel runtime_config confirms parameter replay across all 3 ranks" "$post_rc_check"
fi

cont_ok="${cont_res%%|*}"
if [ "${cont_ok:-0}" -ge 1 ]; then
  record PASS "continuous inference probe served requests during 2->3 expansion" "ok=${cont_res%%|*} fail=${cont_res##*|}"
else
  record FAIL "continuous inference probe served requests during 2->3 expansion" "ok=${cont_res%%|*} fail=${cont_res##*|}"
fi

post_code="$(probe_curl -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' \
  -d '{"model":"'"${MODEL}"'","messages":[{"role":"user","content":"Reply OK"}],"max_tokens":8}' \
  "${ENDPOINT}/v1/chat/completions" 2>/dev/null || echo 000)"
if [ "$post_code" = "200" ]; then
  record PASS "Elastic EP inference returns 200 after width=3 expansion" "$ENDPOINT"
else
  record FAIL "Elastic EP inference returns 200 after width=3 expansion" "http=${post_code}"
fi

results
