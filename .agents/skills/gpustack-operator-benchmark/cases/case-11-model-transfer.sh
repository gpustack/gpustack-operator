#!/usr/bin/env bash
#
# CASE 11 -- Model-Transfer Remote Weight Loading: Require vs Disabled   (MUTATING, self-recovering)
#
#   case-11-model-transfer.sh [OUT_DIR]
#
# Goal:        Compare cross-node ModelDeployment weight loading under Require with local CSI
#              disk loading under Disabled after measured Node-to-Node sync.
#              Ascend/HIXL covers Qwen/Qwen3-8B (TP=2) and Qwen/Qwen3.6-35B-A3B (TP=8 + EP).
#              NVIDIA/NIXL covers Qwen/Qwen3-8B (TP=2) and
#              Qwen/Qwen3-Next-80B-A3B-Instruct-FP8 (TP=4, TP=4 + EP, TP=2 + PP=2,
#              DP=2 + TP=4 + EP). MT_MODELS may also select the 8x H100 large models from
#              vllm-project/recipes: Qwen/Qwen3-235B-A22B-Instruct-2507-FP8,
#              Qwen/Qwen3-235B-A22B-Instruct-2507, Qwen/Qwen3-VL-235B-A22B-Instruct and
#              NVIDIA:gpustack/GLM-5.3-W4A8, each run in tp8 or tp8_ep. All remote arms run before the
#              target materializes weights.
#              Both loading arms use eager/disabled-graph mode and one content-checked chat
#              completion. On NVIDIA vLLM, --kernel-config '{"enable_flashinfer_autotune":false}'
#              is injected by default (override with MT_ENABLE_FLASHINFER_AUTOTUNE=1) to prevent
#              FlashInfer autotune host-cache deadlock (TP0 cuda:0 cache hit vs TP1..N miss).
#              SGLang supplies functional coverage; published performance tables
#              use vLLM/vLLM-Ascend. Sync and model loading have separate timers; Pod readiness
#              time is diagnostic only. Same-node consumers and seed/scale-out runs are excluded.
# Environment: An isolated two-node Ascend or NVIDIA accelerator pool, with verified model
#              artifacts cached only on the source node and its model-transfer source Serving.
#              The target must initially lack the selected digests and be schedulable. The
#              model-manager peer service and metrics endpoint must be available. Run without
#              competing materializations so peer counters and publication timing are attributable.
#              Confirm the pinned engine image supports the selected model, parallelism and
#              eager flags before execution. OUT_DIR/cluster-shape.txt records the cluster shape.
# Inputs:      Explicit MT_SOURCE_NODE, MT_TARGET_NODE (distinct), MT_INSTANCE_TYPE and MT_IMAGE;
#              MT_ENGINE selects vLLM (default) or SGLang. MT_MODELS may select cases within the
#              vendor's model set, MT_ALLOW_CUSTOM_MODEL=1 admits any model, and MT_INTERFACE
#              overrides the per-case RDMA interface count. Use a fresh OUT_DIR. The script writes
#              each deployment manifest to OUT_DIR/<case_id>/model-transfer-bench.yaml and each
#              sync consumer manifest to OUT_DIR/peer-sync/<artifact>/consumer.yaml.
# Overrides:   Optional model-transfer parameter matrix inputs, all defaulting to unset.
#              MT_TP_MODE injects GPUSTACK_MODEL_TRANSFER_TP_MODE and MT_MAX_WINDOW_BYTES injects
#              GPUSTACK_MODEL_TRANSFER_MAX_WINDOW_BYTES into the server role env of every arm.
#              MT_SOURCE_MAX_RAM_BYTES patches the source node's model-manager DaemonSet with
#              GPUSTACK_MODEL_TRANSFER_SOURCE_MAX_MEMORY_BYTES and waits for the source
#              generations to serve again. MT_SOURCE_IDLE_EVICT_SECONDS grants the idle window the
#              source is given to release its resident blocks after an arm, samples
#              NodeModelStore.status.modelTransfer.residentBytes before the arm, after Ready and
#              after the window, and records source-memory.json in the arm directory.
# Expected:    - Require consumers run on the target node, with the source on the other node;
#              - requested RDMA interfaces and their environment variables are visible;
#              - Node-to-Node sync increases peer bytes without Hub fallback and records one
#                materialization-to-publication interval, including verification; an already
#                cached target is not a new sync sample;
#              - Disabled consumers load the synchronized checkpoint from local CSI storage;
#              - each loading arm reaches Ready and returns a complete chat answer containing
#                1591 for 37 * 43, with finish_reason=stop;
#              - OUT_DIR/comparison.tsv records model-loading time, broker operation count,
#                RDMA and chat checks, and diagnostic readiness time; per-arm logs and
#                timer-scope.txt retain the loading evidence;
#              - OUT_DIR/peer-sync/<artifact>/ retains artifact/store snapshots, before/after
#                metrics and result.json. Peer sync does not prove a cold OS page cache;
#                its state remains unknown unless independently established.
# Cleanup:     The exit trap restores the original model-artifact-remote-delivery-policy,
#              restores the model-manager DaemonSet env when MT_SOURCE_MAX_RAM_BYTES patched it,
#              uncordons the source only if this run cordoned it, and attempts to delete the
#              active benchmark deployment and sync consumer. Completed consumers are deleted
#              between arms. Evidence and synchronized model-cache contents remain on disk.
#
# The revised cross-node/eager/peer-sync cases have not been executed on hardware. Historical
# measurements do not establish acceptance of this revision.
set -uo pipefail

CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SKILL_DIR="$(cd "${CASE_DIR}/.." && pwd)"
REPO="$(cd "${SKILL_DIR}/../../.." && pwd)"
export PATH="${REPO}/.agents/skills/_e2e-lib/scripts/kubectl-shim:${PATH}"

# shellcheck source=/dev/null
. "${SKILL_DIR}/../_e2e-lib/scripts/serving-lib.sh"

NS="${BENCH_NS:-gpustack-bench}"
OUT="${1:-${REPO}/.claude/reports/$(date +%F)-benchmark/model-transfer}"
ENGINE="${MT_ENGINE:-vLLM}"
INSTANCE_TYPE="${MT_ACCELERATOR_TYPE:-${MT_INSTANCE_TYPE:?Set MT_ACCELERATOR_TYPE or MT_INSTANCE_TYPE for the selected Ascend or NVIDIA pool}}"
case "${INSTANCE_TYPE}" in
  *nvidia*) VENDOR=NVIDIA ;;
  *ascend*) VENDOR=Ascend ;;
  *) echo "Only Ascend/HIXL and NVIDIA/NIXL are in this benchmark" >&2; exit 1 ;;
esac
case "${ENGINE}" in
  vLLM|SGLang) ;;
  *) echo "MT_ENGINE must be vLLM or SGLang" >&2; exit 1 ;;
esac
IMAGE="${MT_IMAGE:?Set MT_IMAGE to the pinned engine image for this environment}"
REPLICAS="${MT_REPLICAS:-1}"
SCALE_OUT="${MT_SCALE_OUT:-0}"
SOURCE_NODE="${MT_SOURCE_NODE:-}"
TARGET_NODE="${MT_TARGET_NODE:-}"
DEFAULT_INTERFACE="${MT_INTERFACE:-}"
ALLOW_CUSTOM_MODEL="${MT_ALLOW_CUSTOM_MODEL:-0}"
TP_MODE_OVERRIDE="${MT_TP_MODE:-}"
MAX_WINDOW_BYTES_OVERRIDE="${MT_MAX_WINDOW_BYTES:-}"
SOURCE_MAX_RAM_BYTES="${MT_SOURCE_MAX_RAM_BYTES:-}"
SOURCE_IDLE_EVICT_SECONDS="${MT_SOURCE_IDLE_EVICT_SECONDS:-}"
MODEL_MANAGER_NS="${MT_MODEL_MANAGER_NS:-gpustack-system}"
MODEL_MANAGER_DS=""
MM_ORIG_ENV=""
MM_MAIN_INDEX=""
SOURCE_RESTORE_TIMEOUT="${SOURCE_RESTORE_TIMEOUT:-900}"
POLICIES="${MT_POLICIES:-Require Disabled}"
READY_TIMEOUT="${READY_TIMEOUT:-1800}"
CHAT_CHECKS=1
if [ -z "${SOURCE_NODE}" ] || [ -z "${TARGET_NODE}" ] || [ "${SOURCE_NODE}" = "${TARGET_NODE}" ]; then
  echo "Set distinct MT_SOURCE_NODE and MT_TARGET_NODE for cross-node cases" >&2
  exit 1
fi
if [ "${SCALE_OUT}" != 0 ] || [ "${REPLICAS}" != 1 ]; then
  echo "Case 11 uses one target replica; same-node seed/scale-out cases are excluded" >&2
  exit 1
fi
if [ "${POLICIES}" != "Require Disabled" ]; then
  echo "Run Require then Disabled to keep peer sync after all remote measurements" >&2
  exit 1
fi
case "${ALLOW_CUSTOM_MODEL}" in
  0|1) ;;
  *) echo "MT_ALLOW_CUSTOM_MODEL must be 0 or 1" >&2; exit 1 ;;
esac
case "${TP_MODE_OVERRIDE}" in
  ""|broadcast|tp0|all|all_tp|all-tp|all_tps) ;;
  *) echo "MT_TP_MODE must be broadcast, tp0, all, all_tp, all-tp or all_tps" >&2; exit 1 ;;
esac
case "${MAX_WINDOW_BYTES_OVERRIDE}" in
  "") ;;
  0|*[!0-9]*) echo "MT_MAX_WINDOW_BYTES must be a positive integer of bytes" >&2; exit 1 ;;
esac
case "${SOURCE_MAX_RAM_BYTES}" in
  "") ;;
  0|*[!0-9]*) echo "MT_SOURCE_MAX_RAM_BYTES must be a positive integer of bytes" >&2; exit 1 ;;
esac
case "${SOURCE_IDLE_EVICT_SECONDS}" in
  "") ;;
  0|*[!0-9]*) echo "MT_SOURCE_IDLE_EVICT_SECONDS must be a positive integer of seconds" >&2; exit 1 ;;
esac
if [ -n "${MT_MODELS:-}" ]; then
  MODELS_SPEC="${MT_MODELS}"
elif [[ "${INSTANCE_TYPE}" == *nvidia* ]]; then
  MODELS_SPEC="b1-8b-tp2:Qwen/Qwen3-8B:qwen3-8b:2:1:tp2 b3-80b-fp8-tp4:Qwen/Qwen3-Next-80B-A3B-Instruct-FP8:qwen3-next-80b-a3b-fp8:4:4:tp4 b3-80b-fp8-tp4-ep:Qwen/Qwen3-Next-80B-A3B-Instruct-FP8:qwen3-next-80b-a3b-fp8:4:4:tp4_ep b3-80b-fp8-tp2-pp2:Qwen/Qwen3-Next-80B-A3B-Instruct-FP8:qwen3-next-80b-a3b-fp8:4:4:tp2_pp2 b3-80b-fp8-dp2-tp4-ep:Qwen/Qwen3-Next-80B-A3B-Instruct-FP8:qwen3-next-80b-a3b-fp8:8:8:dp2_tp4_ep"
else
  MODELS_SPEC="a1-8b-tp2:Qwen/Qwen3-8B:qwen3-8b:2:1:tp2 a2-35b-tp8-ep:Qwen/Qwen3.6-35B-A3B:qwen3-6-35b-a3b:8:8:tp8_ep"
fi
ORIG_POLICY=""
ACTIVE_DEPLOY=""
ACTIVE_SYNC_POD=""
SOURCE_CORDONED=0

mkdir -p "${OUT}"
[ ! -e "${OUT}/comparison.tsv" ] || { echo "Use a fresh OUT_DIR" >&2; exit 1; }
log() { echo "[$(date +%H:%M:%S)] $*"; }

capture_cluster_shape "${OUT}/cluster-shape.txt"

ORIG_POLICY="$(kubectl -n gpustack-system get setting model-artifact-remote-delivery-policy \
  -o jsonpath='{.status.value}' 2>/dev/null || true)"
[ -n "${ORIG_POLICY}" ] || { log "ERROR: cannot save the original policy"; exit 1; }

cleanup() {
  if [ -n "${MODEL_MANAGER_DS}" ] && [ -n "${MM_ORIG_ENV}" ] && [ -n "${MM_MAIN_INDEX}" ]; then
    kubectl -n "${MODEL_MANAGER_NS}" patch daemonset "${MODEL_MANAGER_DS}" --type=json \
      -p "[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/${MM_MAIN_INDEX}/env\",\"value\":${MM_ORIG_ENV}}]" \
      >/dev/null 2>&1 || true
  fi
  if [ "${SOURCE_CORDONED}" = 1 ]; then
    kubectl uncordon "${SOURCE_NODE}" >/dev/null 2>&1 || true
  fi
  if [ -n "${ACTIVE_SYNC_POD}" ]; then
    kubectl -n "${NS}" delete pod "${ACTIVE_SYNC_POD}" --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1 || true
  fi
  if [ -n "${ORIG_POLICY}" ]; then
    kubectl -n gpustack-system patch setting model-artifact-remote-delivery-policy \
      --type=merge -p "{\"spec\":{\"value\":\"${ORIG_POLICY}\"}}" >/dev/null 2>&1 || true
  fi
  if [ -n "${ACTIVE_DEPLOY}" ]; then
    kubectl -n "${NS}" delete modeldeployment "${ACTIVE_DEPLOY}" --ignore-not-found --wait=true --timeout=180s \
      >/dev/null 2>&1 || true
  fi
  return 0
}
trap cleanup EXIT

render_extra_args() {
  local mode="$1"
  local accel="$2"
  local model="$3"
  if [ "${ENGINE}" = "SGLang" ]; then
    if [ "${VENDOR}" = NVIDIA ]; then
      printf '        - --cuda-graph-backend-decode=disabled\n        - --cuda-graph-backend-prefill=disabled\n'
    else
      printf '        - --disable-cuda-graph\n'
    fi
    case "${mode}" in
      tp2)
        printf '        - --tp\n        - "2"\n'
        ;;
      tp8)
        printf '        - --tp\n        - "8"\n        - --context-length\n        - "8192"\n        - --mem-fraction-static\n        - "0.80"\n'
        ;;
      tp8_ep)
        printf '        - --tp\n        - "8"\n        - --ep\n        - "8"\n'
        ;;
      tp4)
        printf '        - --tp\n        - "4"\n        - --context-length\n        - "8192"\n        - --mem-fraction-static\n        - "0.80"\n'
        ;;
      tp4_ep|tp8_ep2)
        printf '        - --tp\n        - "%s"\n        - --ep\n        - "2"\n        - --context-length\n        - "8192"\n        - --mem-fraction-static\n        - "0.80"\n' "${accel}"
        ;;
      tp2_pp2)
        printf '        - --tp\n        - "2"\n        - --pp-size\n        - "2"\n        - --context-length\n        - "8192"\n        - --mem-fraction-static\n        - "0.80"\n'
        ;;
      dp2_tp4_ep|dp2_tp4)
        printf '        - --dp-size\n        - "2"\n        - --tp\n        - "4"\n        - --context-length\n        - "8192"\n        - --mem-fraction-static\n        - "0.80"\n'
        ;;
      *)
        printf '        - --tp\n        - "%s"\n' "${accel}"
        ;;
    esac
  else
    printf '        - --enforce-eager\n'
    if [ "${VENDOR}" = NVIDIA ] && [ "${MT_ENABLE_FLASHINFER_AUTOTUNE:-0}" != "1" ]; then
      printf '        - --kernel-config\n        - '\''{"enable_flashinfer_autotune":false}'\''\n'
    fi
    case "${mode}" in
      tp2)
        printf '        - --tensor-parallel-size\n        - "2"\n'
        ;;
      tp8)
        printf '        - --tensor-parallel-size\n        - "8"\n        - --max-model-len\n        - "8192"\n'
        ;;
      tp8_ep)
        printf '        - --tensor-parallel-size\n        - "8"\n        - --enable-expert-parallel\n        - --max-model-len\n        - "8192"\n'
        if [[ "${model}" == *"Qwen3.6-35B"* ]]; then
          printf '        - --language-model-only\n'
        fi
        ;;
      tp4)
        printf '        - --tensor-parallel-size\n        - "4"\n        - --enable-prefix-caching\n        - --max-model-len\n        - "8192"\n'
        ;;
      tp4_ep)
        printf '        - --tensor-parallel-size\n        - "4"\n        - --enable-expert-parallel\n        - --max-model-len\n        - "8192"\n'
        ;;
      tp2_pp2)
        printf '        - --tensor-parallel-size\n        - "2"\n        - --pipeline-parallel-size\n        - "2"\n        - --max-model-len\n        - "8192"\n'
        ;;
      dp2_tp4_ep)
        printf '        - --data-parallel-size\n        - "2"\n        - --tensor-parallel-size\n        - "4"\n        - --enable-expert-parallel\n        - --max-model-len\n        - "8192"\n'
        ;;
      *)
        printf '        - --tensor-parallel-size\n        - "%s"\n' "${accel}"
        ;;
    esac
  fi
}

role_env_lines() {
  local lines=""
  if [ -n "${TP_MODE_OVERRIDE}" ]; then
    lines="${lines}        - name: GPUSTACK_MODEL_TRANSFER_TP_MODE
          value: \"${TP_MODE_OVERRIDE}\"
"
  fi
  if [ -n "${MAX_WINDOW_BYTES_OVERRIDE}" ]; then
    lines="${lines}        - name: GPUSTACK_MODEL_TRANSFER_MAX_WINDOW_BYTES
          value: \"${MAX_WINDOW_BYTES_OVERRIDE}\"
"
  fi
  printf '%s' "${lines}"
}

write_manifest() {
  local manifest_path="$1"
  local deploy="$2"
  local model="$3"
  local artifact="$4"
  local accel="$5"
  local iface="$6"
  local mode="$7"
  local init_replicas="$8"
  local iface_line=""
  if [ -n "${iface}" ] && [ "${iface}" != "0" ]; then
    iface_line="        interface: \"${iface}\""
  fi
  local extra_args
  extra_args="$(render_extra_args "${mode}" "${accel}" "${model}")"
  local env_lines env_block=""
  env_lines="$(role_env_lines)"
  if [ -n "${env_lines}" ]; then
    env_block="
      env:
${env_lines}"
  fi
  cat > "${manifest_path}" <<YAML
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: ${deploy}
  namespace: ${NS}
spec:
  model:
    name: ${model}
    artifactRef:
      name: ${artifact}
  engine:
    name: ${ENGINE}
    image: ${IMAGE}
  roles:
    - name: server
      kind: Server
      replicas: ${init_replicas}
      size: 1
      startupTimeoutSeconds: 1800
      acceleratorType: ${INSTANCE_TYPE}
      image: ${IMAGE}
      imagePullPolicy: IfNotPresent
      resources:
        accelerator: "${accel}"
${iface_line}${env_block}
      extraArgs:
${extra_args}
YAML
}

set_policy() {
  local policy="$1"
  log "Setting model-artifact-remote-delivery-policy=${policy}"
  kubectl -n gpustack-system patch setting model-artifact-remote-delivery-policy \
    --type=merge -p "{\"spec\":{\"value\":\"${policy}\"}}" >/dev/null
  local _
  for _ in $(seq 1 30); do
    local current
    current="$(kubectl -n gpustack-system get setting model-artifact-remote-delivery-policy \
      -o jsonpath='{.status.value}' 2>/dev/null || true)"
    if [ "${current}" = "${policy}" ]; then
      return 0
    fi
    sleep 1
  done
  log "ERROR: timed out waiting for policy ${policy}"
  return 1
}

model_manager_main_env() {
  kubectl -n "${MODEL_MANAGER_NS}" get daemonset "${MODEL_MANAGER_DS}" -o json | python3 -c '
import json, sys
ds = json.load(sys.stdin)
mains = [c for c in ds["spec"]["template"]["spec"]["containers"] if c["name"] == "main"]
assert len(mains) == 1, "model-manager DaemonSet does not carry exactly one main container"
print(json.dumps(mains[0].get("env", [])))
'
}

model_manager_main_index() {
  kubectl -n "${MODEL_MANAGER_NS}" get daemonset "${MODEL_MANAGER_DS}" -o json | python3 -c '
import json, sys
ds = json.load(sys.stdin)
containers = ds["spec"]["template"]["spec"]["containers"]
print([i for i, c in enumerate(containers) if c["name"] == "main"][0])
'
}

wait_model_manager_ready() {
  local waited=0 mm_pod
  while [ "${waited}" -lt 600 ]; do
    mm_pod="$(kubectl -n "${MODEL_MANAGER_NS}" get pods \
      -l app.kubernetes.io/component=model-manager \
      --field-selector "spec.nodeName=${SOURCE_NODE}" \
      -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | head -n 1)"
    if [ -n "${mm_pod}" ] && kubectl -n "${MODEL_MANAGER_NS}" wait --for=condition=ready \
      "pod/${mm_pod}" --timeout=10s >/dev/null 2>&1; then
      log "model-manager ${mm_pod} on ${SOURCE_NODE} is Ready"
      return 0
    fi
    sleep 5
    waited=$((waited + 5))
  done
  log "ERROR: model-manager on ${SOURCE_NODE} did not become Ready within 600s"
  return 1
}

wait_source_serving() {
  local artifact="$1" waited=0 tmp="${OUT}/source-restart"
  mkdir -p "${tmp}"
  while [ "${waited}" -lt "${SOURCE_RESTORE_TIMEOUT}" ]; do
    if check_source "${artifact}" "${tmp}" >/dev/null 2>&1; then
      log "source generation for ${artifact} serves again after the model-manager restart"
      return 0
    fi
    sleep 5
    waited=$((waited + 5))
  done
  log "ERROR: source generation for ${artifact} did not serve again within ${SOURCE_RESTORE_TIMEOUT}s"
  return 1
}

patch_source_model_manager() {
  local ds_names patch_doc
  ds_names="$(kubectl -n "${MODEL_MANAGER_NS}" get daemonset \
    -l app.kubernetes.io/component=model-manager -o jsonpath='{.items[*].metadata.name}')"
  # The names are whitespace separated on purpose: one DaemonSet is expected.
  # shellcheck disable=SC2086
  set -- ${ds_names}
  if [ "$#" -ne 1 ]; then
    log "ERROR: expected one model-manager DaemonSet, found: ${ds_names:-none}"
    return 1
  fi
  MODEL_MANAGER_DS="$1"
  MM_ORIG_ENV="$(model_manager_main_env)" || return 1
  MM_MAIN_INDEX="$(model_manager_main_index)" || return 1
  patch_doc="$(python3 -c '
import json, sys
env, index, value = json.loads(sys.argv[1]), sys.argv[2], sys.argv[3]
name = "GPUSTACK_MODEL_TRANSFER_SOURCE_MAX_MEMORY_BYTES"
existing = [i for i, e in enumerate(env) if e.get("name") == name]
if existing:
    op = {"op": "replace", "path": f"/spec/template/spec/containers/{index}/env/{existing[0]}"}
else:
    op = {"op": "add", "path": f"/spec/template/spec/containers/{index}/env/-"}
print(json.dumps([{**op, "value": {"name": name, "value": value}}]))
' "${MM_ORIG_ENV}" "${MM_MAIN_INDEX}" "${SOURCE_MAX_RAM_BYTES}")" || return 1
  log "Patching model-manager ${MODEL_MANAGER_DS} with GPUSTACK_MODEL_TRANSFER_SOURCE_MAX_MEMORY_BYTES=${SOURCE_MAX_RAM_BYTES}"
  kubectl -n "${MODEL_MANAGER_NS}" patch daemonset "${MODEL_MANAGER_DS}" --type=json \
    -p "${patch_doc}" >/dev/null || return 1
  wait_model_manager_ready || return 1
  local entry artifact
  for entry in ${MODELS_SPEC}; do
    artifact="$(printf '%s' "${entry}" | cut -d: -f3)"
    wait_source_serving "${artifact}" || return 1
  done
}

source_memory_snapshot() {
  local phase="$1" digest="$2" out_file="$3"
  kubectl get nodemodelstore "${SOURCE_NODE}" -o json > "${out_file}.store.json" 2>/dev/null || return 1
  python3 - "${out_file}.store.json" "${SOURCE_NODE}" "${digest}" "${phase}" > "${out_file}" <<'PY'
import json, pathlib, sys
store_path, node, digest, phase = pathlib.Path(sys.argv[1]), *sys.argv[2:]
store = json.loads(store_path.read_text())
generations = [
    {"state": g.get("state"), "resident_bytes": g.get("residentBytes", 0),
     "active_sessions": g.get("activeSessions", 0)}
    for g in (store.get("status", {}).get("modelTransfer") or {}).get("generations", [])
    if g.get("digest") == digest
]
snapshot = {"phase": phase, "node": node, "digest": digest, "generations": generations,
            "resident_bytes": sum(g["resident_bytes"] for g in generations)}
print(json.dumps(snapshot))
PY
}

observe_idle_eviction() {
  local arm_dir="$1" digest="$2" waited=0 released=0 snapshot
  while :; do
    if snapshot="$(source_memory_snapshot after-idle "${digest}" \
      "${arm_dir}/source-memory-after-idle.json")"; then
      if [ "$(printf '%s' "${snapshot}" | python3 -c 'import json, sys; print(json.load(sys.stdin)["resident_bytes"])')" = "0" ]; then
        released=1
        break
      fi
    fi
    [ "${waited}" -ge "${SOURCE_IDLE_EVICT_SECONDS}" ] && break
    sleep 10
    waited=$((waited + 10))
  done
  python3 - "${arm_dir}" "${SOURCE_NODE}" "${digest}" "${SOURCE_IDLE_EVICT_SECONDS}" "${released}" <<'PY'
import json, pathlib, sys
arm, node, digest, window, released = pathlib.Path(sys.argv[1]), *sys.argv[2:]
def phase(name):
    path = arm / name
    if not path.exists():
        return None
    try:
        return json.loads(path.read_text())
    except ValueError:
        return None
result = {
    "source_node": node,
    "digest": digest,
    "expected_idle_evict_seconds": int(window),
    "before": phase("source-memory-before.json"),
    "during": phase("source-memory-during.json"),
    "after_idle": phase("source-memory-after-idle.json"),
    "released": released == "1",
    "timer": "resident bytes sampled before the arm, after Ready and after the idle window",
}
(arm / "source-memory.json").write_text(json.dumps(result, indent=2) + "\n")
PY
  if [ "${released}" != 1 ]; then
    log "WARN: source resident bytes did not release within ${SOURCE_IDLE_EVICT_SECONDS}s; recorded in ${arm_dir}/source-memory.json"
  else
    log "Source released its resident bytes within ${SOURCE_IDLE_EVICT_SECONDS}s"
  fi
  return 0
}

wait_replicas_ready() {
  local deploy="$1"
  local target_replicas="$2"
  local out_file="$3"
  local start_ts
  start_ts="$(date +%s)"
  local elapsed=0
  while [ "${elapsed}" -lt "${READY_TIMEOUT}" ]; do
    local ready_count
    ready_count="$(kubectl -n "${NS}" get pods \
      -l "app.kubernetes.io/instance=${deploy},app.kubernetes.io/component=server" \
      -o jsonpath='{range .items[*]}{.status.containerStatuses[0].ready}{"\n"}{end}' 2>/dev/null \
      | grep -c '^true$' || true)"
    if [ "${ready_count}" -eq "${target_replicas}" ]; then
      local end_ts
      end_ts="$(date +%s)"
      echo "$((end_ts - start_ts))" > "${out_file}"
      log "All ${target_replicas} replicas of ${deploy} Ready in $((end_ts - start_ts))s"
      return 0
    fi
    sleep 2
    elapsed="$(($(date +%s) - start_ts))"
  done
  log "ERROR: timed out after ${READY_TIMEOUT}s waiting for ${target_replicas} replicas of ${deploy}"
  return 1
}

verify_rdma_mount() {
  local pod="$1"
  local iface="$2"
  local arm_dir="$3"
  local allowed_hcas="${MT_ALLOWED_RDMA_HCAS:-mlx5_0,mlx5_1,mlx5_2,mlx5_3,mlx5_4,mlx5_5,mlx5_6,mlx5_7}"
  if [ -z "${iface}" ] || [ "${iface}" = "0" ]; then
    echo "n/a" > "${arm_dir}/rdma-check.txt"
    return 0
  fi
  # shellcheck disable=SC2016 # container variables expand inside the remote container shell
  if kubectl -n "${NS}" exec "${pod}" -- python3 -c '
import glob, os, sys
allowed = set(filter(None, sys.argv[1].split(",")))
uverbs = sorted(glob.glob("/dev/infiniband/uverbs*"))
rdma_devs = [d.strip() for d in os.environ.get("GPUSTACK_RDMA_DEVICES", "").split(",") if d.strip()]
nccl_hcas = [d.strip().split(":")[0] for d in os.environ.get("NCCL_IB_HCA", "").lstrip("^=").split(",") if d.strip()]
ucx_net = os.environ.get("UCX_NET_DEVICES", "") or ",".join(f"{d}:1" for d in rdma_devs)
print("UVERBS=" + ",".join(uverbs))
print("GPUSTACK_RDMA_DEVICES=" + ",".join(rdma_devs))
print("NCCL_IB_HCA=" + os.environ.get("NCCL_IB_HCA", ""))
print("UCX_NET_DEVICES=" + ucx_net)
assert uverbs, "missing /dev/infiniband/uverbs*"
assert rdma_devs, "empty GPUSTACK_RDMA_DEVICES"
assert nccl_hcas, "empty NCCL_IB_HCA"
if allowed:
    assert set(rdma_devs).issubset(allowed), f"disallowed HCA in GPUSTACK_RDMA_DEVICES: {rdma_devs} vs {allowed}"
    assert set(nccl_hcas).issubset(allowed), f"disallowed HCA in NCCL_IB_HCA: {nccl_hcas} vs {allowed}"
' "${allowed_hcas}" > "${arm_dir}/rdma-mount-${pod}.txt" 2>&1; then
    echo "PASS" > "${arm_dir}/rdma-check.txt"
    log "Verified RDMA mount and env vars inside ${pod}"
    return 0
  else
    echo "FAIL" > "${arm_dir}/rdma-check.txt"
    log "WARN: RDMA mount verification failed inside ${pod}"
    return 1
  fi
}

verify_chat_completions() {
  local deploy="$1"
  local model="$2"
  local arm_dir="$3"
  local target_pod="$4"
  local pod="${target_pod}"
  if [ -z "${pod}" ]; then
    pod="$(kubectl -n "${NS}" get pods \
      -l "app.kubernetes.io/instance=${deploy},app.kubernetes.io/component=server" \
      -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
  fi
  if [ -z "${pod}" ]; then
    echo "0/${CHAT_CHECKS}" > "${arm_dir}/chat-checks.txt"
    return 1
  fi
  local ok=0
  local idx
  for idx in $(seq 1 "${CHAT_CHECKS}"); do
    local prompt expected
    case "${idx}" in
      1)
        prompt="What is 37 * 43? Reply with the number only."
        expected="1591"
        ;;
      2)
        prompt="What is the capital of France? Reply in one word."
        expected="Paris"
        ;;
      *)
        prompt="Write a 4-line Python function rev(xs) that returns xs reversed."
        expected="def rev"
        ;;
    esac
    local out_file="${arm_dir}/chat-check-${idx}.json"
    if kubectl -n "${NS}" exec "${pod}" -- python3 -c "
import json, urllib.request
expected = '''${expected}'''
req = urllib.request.Request(
    'http://127.0.0.1:8000/v1/chat/completions',
    data=json.dumps({
        'model': '${model}',
        'messages': [{'role': 'user', 'content': '''${prompt}'''}],
        'max_tokens': 64,
        'temperature': 0.0,
        'chat_template_kwargs': {'enable_thinking': False},
    }).encode('utf-8'),
    headers={'Content-Type': 'application/json'},
)
with urllib.request.urlopen(req, timeout=60) as resp:
    body = resp.read().decode('utf-8')
    data = json.loads(body)
    assert data.get('choices'), 'missing choices'
    content = data['choices'][0].get('message', {}).get('content', '') or ''
    assert data['choices'][0].get('finish_reason') == 'stop', 'incomplete response'
    assert expected.lower() in content.lower(), f'expected {expected!r} in {content!r}'
    print(body)
" > "${out_file}" 2>&1; then
      ok="$((ok + 1))"
    fi
  done
  echo "${ok}/${CHAT_CHECKS}" > "${arm_dir}/chat-checks.txt"
  log "Verified ${ok}/${CHAT_CHECKS} /v1/chat/completions checks on ${pod}"
  [ "${ok}" -eq "${CHAT_CHECKS}" ]
}

collect_arm_evidence() {
  local deploy="$1"
  local model="$2"
  local iface="$3"
  local arm_dir="$4"
  local pods
  pods="$(kubectl -n "${NS}" get pods \
    -l "app.kubernetes.io/instance=${deploy},app.kubernetes.io/component=server" \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)"
  : > "${arm_dir}/load-times.tsv"
  local pod target_pod=""
  for pod in ${pods}; do
    local node
    node="$(kubectl -n "${NS}" get pod "${pod}" -o jsonpath='{.spec.nodeName}' 2>/dev/null || true)"
    if [ "${node}" != "${TARGET_NODE}" ]; then
      log "ERROR: ${pod} is on ${node}, expected ${TARGET_NODE}"
      return 1
    fi
    target_pod="${pod}"
    local restarts
    restarts="$(kubectl -n "${NS}" get pod "${pod}" \
      -o jsonpath='{.status.containerStatuses[0].restartCount}' 2>/dev/null || echo 0)"
    kubectl -n "${NS}" logs "${pod}" > "${arm_dir}/${pod}.log" 2>&1 || true
    local load_sec
    local timer_pattern
    if [ "${ENGINE}" = SGLang ]; then
      timer_pattern='Loading weights took [0-9.]+ seconds'
    elif [ "${VENDOR}" = Ascend ]; then
      timer_pattern='Model runner load_model total time: [0-9.]+ seconds'
    else
      timer_pattern='Model loading took [0-9.]+ GiB memory and [0-9.]+ seconds'
    fi
    load_sec="$(grep -Eo "${timer_pattern}" "${arm_dir}/${pod}.log" \
      | sed -E 's/.* ([0-9.]+) seconds/\1/' | sort -nr | head -n 1 || true)"
    printf '%s\n' "${timer_pattern}" > "${arm_dir}/timer-scope.txt"
    local broker_ops
    broker_ops="$(grep -Eo 'operations=[0-9]+|transfer_ops=[0-9]+' \
      "${arm_dir}/${pod}.log" | tail -n 1 | grep -Eo '[0-9]+' | tail -n 1 || true)"
    if [ -z "${broker_ops}" ]; then
      broker_ops="n/a"
    fi
    printf '%s\t%s\t%s\t%s\t%s\n' "${pod}" "${node}" "${restarts}" "${load_sec:-n/a}" "${broker_ops}" >> "${arm_dir}/load-times.tsv"
  done

  if [ -n "${target_pod}" ]; then
    verify_rdma_mount "${target_pod}" "${iface}" "${arm_dir}" || true
  fi
  verify_chat_completions "${deploy}" "${model}" "${arm_dir}" "${target_pod}" || true
}

ensure_source_resident() {
  local artifact="$1"
  local digest resident
  digest="$(kubectl -n "${NS}" get modelartifact "${artifact}" \
    -o jsonpath='{.status.resolved.manifestDigest}' 2>/dev/null || true)"
  [ -n "${digest}" ] || return 1
  resident="$(kubectl get nodemodelstore "${SOURCE_NODE}" -o json 2>/dev/null | python3 -c '
import json, sys
digest = sys.argv[1]
store = json.load(sys.stdin)
gens = (store.get("status", {}).get("modelTransfer") or {}).get("generations", [])
print(sum(g.get("residentBytes", 0) for g in gens if g.get("digest") == digest and g.get("state") == "Serving"))
' "${digest}" 2>/dev/null || echo 0)"
  if [ "${resident:-0}" -gt 0 ]; then
    return 0
  fi
  if kubectl -n "${NS}" get modelprefetch "${artifact}" >/dev/null 2>&1; then
    log "Source generation for ${artifact} was idle-evicted (residentBytes=0); re-warming ModelPrefetch"
    kubectl -n "${NS}" patch modelprefetch "${artifact}" --type=merge -p '{"spec":{"source":null}}' >/dev/null 2>&1 || true
    sleep 5
    kubectl -n "${NS}" patch modelprefetch "${artifact}" --type=merge -p '{"spec":{"source":{}}}' >/dev/null 2>&1 || true
    local waited=0
    while [ "${waited}" -lt 300 ]; do
      resident="$(kubectl get nodemodelstore "${SOURCE_NODE}" -o json 2>/dev/null | python3 -c '
import json, sys
digest = sys.argv[1]
store = json.load(sys.stdin)
gens = (store.get("status", {}).get("modelTransfer") or {}).get("generations", [])
print(sum(g.get("residentBytes", 0) for g in gens if g.get("digest") == digest and g.get("state") == "Serving"))
' "${digest}" 2>/dev/null || echo 0)"
      if [ "${resident:-0}" -gt 0 ]; then
        log "Re-warmed source generation for ${artifact} (residentBytes=${resident})"
        return 0
      fi
      sleep 5
      waited=$((waited + 5))
    done
  fi
  return 0
}

check_source() {
  local artifact="$1" evidence="$2"
  ensure_source_resident "${artifact}" || true
  kubectl -n "${NS}" get modelartifact "${artifact}" -o json > "${evidence}/artifact.json" || return 1
  kubectl get nodemodelstore -o json > "${evidence}/stores.json" || return 1
  python3 - "${evidence}" "${SOURCE_NODE}" "${TARGET_NODE}" <<'PY'
import json, pathlib, sys
root, source, target = pathlib.Path(sys.argv[1]), *sys.argv[2:]
artifact = json.loads((root / 'artifact.json').read_text())
digest = artifact['status']['resolved']['manifestDigest']
assert digest, 'artifact lacks a resolved manifest digest'
stores = json.loads((root / 'stores.json').read_text())['items']
ready, serving = [], []
for store in stores:
    node, status = store['metadata']['name'], store.get('status', {})
    models = [m for m in status.get('models', []) if m['digest'] == digest]
    assert node != target or not models, 'target already has model state; use a fresh target cache'
    if any(m['state'] == 'Ready' for m in models):
        ready.append(node)
    if any(g['digest'] == digest and g['state'] == 'Serving'
           for g in (status.get('modelTransfer') or {}).get('generations', [])):
        serving.append(node)
assert ready == [source], f'expected sole disk source {source}, got {ready}'
assert serving == [source], f'expected sole remote source {source}, got {serving}'
PY
}

prepare_peer_sync() {
  local artifact="$1" sync_dir="${OUT}/peer-sync/$1" plugin plugin_uid uid digest
  [ ! -f "${sync_dir}/result.json" ] || return 0
  mkdir -p "${sync_dir}"
  check_source "${artifact}" "${sync_dir}" || return 1
  plugin="$(kubectl -n gpustack-system get pods -l app.kubernetes.io/component=model-manager \
    --field-selector "spec.nodeName=${TARGET_NODE}" -o jsonpath='{.items[0].metadata.name}')" || return 1
  plugin_uid="$(kubectl -n gpustack-system get pod "${plugin}" -o jsonpath='{.metadata.uid}')" || return 1
  kubectl -n gpustack-system exec "${plugin}" -c main -- curl -fsk -m 20 \
    https://127.0.0.1:32444/metrics > "${sync_dir}/before.metrics" || return 1
  uid="$(kubectl -n "${NS}" get modelartifact "${artifact}" -o jsonpath='{.metadata.uid}')" || return 1
  digest="$(kubectl -n "${NS}" get modelartifact "${artifact}" -o jsonpath='{.status.resolved.manifestDigest}')" || return 1
  ACTIVE_SYNC_POD="mt-sync-${RANDOM}-${RANDOM}"
  cat > "${sync_dir}/consumer.yaml" <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: ${ACTIVE_SYNC_POD}
  namespace: ${NS}
spec:
  nodeName: ${TARGET_NODE}
  terminationGracePeriodSeconds: 1
  securityContext: {runAsNonRoot: true, runAsUser: 65534, seccompProfile: {type: RuntimeDefault}}
  containers:
    - name: mount
      image: python:3.12-alpine
      command: ["python3", "-c", "import time; time.sleep(86400)"]
      securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
      volumeMounts: [{name: model, mountPath: /model, readOnly: true}]
  volumes:
    - name: model
      csi:
        driver: model.csi.gpustack.ai
        readOnly: true
        volumeAttributes:
          artifact: ${artifact}
          artifactUID: ${uid}
          manifestDigest: ${digest}
YAML
  kubectl apply -f "${sync_dir}/consumer.yaml" >/dev/null || return 1
  kubectl -n "${NS}" wait --for=condition=Ready "pod/${ACTIVE_SYNC_POD}" \
    --timeout="${READY_TIMEOUT}s" > "${sync_dir}/wait.txt" 2>&1 || return 1
  kubectl -n gpustack-system exec "${plugin}" -c main -- curl -fsk -m 20 \
    https://127.0.0.1:32444/metrics > "${sync_dir}/after.metrics" || return 1
  [ "${plugin_uid}" = "$(kubectl -n gpustack-system get pod "${plugin}" -o jsonpath='{.metadata.uid}')" ] || return 1
  kubectl get nodemodelstore "${TARGET_NODE}" -o json > "${sync_dir}/target.json" || return 1
  python3 - "${sync_dir}" "${digest}" <<'PY'
import json, pathlib, sys
root, digest = pathlib.Path(sys.argv[1]), sys.argv[2]
def metrics(name):
    lines = (root / name).read_text().splitlines()
    assert any(s.startswith('gpustack_model_manager_') for s in lines), 'missing metrics'
    return {p[0]: float(p[1]) for s in lines if s and not s.startswith('#') if len(p := s.split()) >= 2}
before, after = metrics('before.metrics'), metrics('after.metrics')
def delta(key):
    assert key in after, f'missing metric {key}'
    value = after[key] - before.get(key, 0)
    assert value >= 0, f'counter reset: {key}'
    return value
peer = delta('gpustack_model_manager_download_bytes_total{source="peer"}')
hub_key = 'gpustack_model_manager_download_bytes_total{source="hub"}'
hub = after.get(hub_key, 0) - before.get(hub_key, 0)
assert peer > 0 and hub == 0, f'expected peer-only materialization, peer={peer}, hub={hub}'
assert delta('gpustack_model_manager_publish_duration_seconds_count') == 1, 'concurrent publications invalidate timer'
seconds = delta('gpustack_model_manager_publish_duration_seconds_sum')
assert seconds > 0, 'missing materialization duration'
result = {'digest': digest, 'peer_bytes': peer, 'hub_bytes': hub,
          'peer_materialization_seconds': seconds,
          'timer': 'materialization start to publication, including verification',
          'page_cache_state': 'unknown; sync does not establish cold page cache'}
(root / 'result.json').write_text(json.dumps(result, indent=2) + '\n')
PY
  local result=$?
  kubectl -n "${NS}" delete pod "${ACTIVE_SYNC_POD}" --wait=true --timeout=120s >/dev/null || return 1
  ACTIVE_SYNC_POD=""
  return "${result}"
}

run_arm() {
  local case_id="$1"
  local model="$2"
  local artifact="$3"
  local accel="$4"
  local iface="$5"
  local mode="$6"
  local policy="$7"
  local arm_name="$8"
  local deploy="${case_id}-mt"
  local arm_dir="${OUT}/${case_id}/${arm_name}"
  local digest=""
  mkdir -p "${arm_dir}"
  ACTIVE_DEPLOY="${deploy}"
  if [ -n "${SOURCE_IDLE_EVICT_SECONDS}" ]; then
    digest="$(kubectl -n "${NS}" get modelartifact "${artifact}" \
      -o jsonpath='{.status.resolved.manifestDigest}' 2>/dev/null || true)"
    if [ -n "${digest}" ]; then
      source_memory_snapshot before "${digest}" "${arm_dir}/source-memory-before.json" || true
    fi
  fi
  set_policy "${policy}" || return 1
  kubectl -n "${NS}" delete modeldeployment "${deploy}" --ignore-not-found --wait=true --timeout=180s \
    >/dev/null 2>&1 || true
  if [ "${policy}" = Disabled ]; then
    prepare_peer_sync "${artifact}" || return 1
  else
    check_source "${artifact}" "${arm_dir}" || return 1
  fi
  if [ "$(kubectl get node "${TARGET_NODE}" -o jsonpath='{.spec.unschedulable}')" = true ]; then
    log "ERROR: target is cordoned"
    return 1
  fi
  if [ "$(kubectl get node "${SOURCE_NODE}" -o jsonpath='{.spec.unschedulable}')" != true ]; then
    kubectl cordon "${SOURCE_NODE}" >/dev/null || return 1
    SOURCE_CORDONED=1
  fi
  write_manifest "${OUT}/${case_id}/model-transfer-bench.yaml" "${deploy}" "${model}" "${artifact}" "${accel}" "${iface}" "${mode}" 1
  kubectl -n "${NS}" apply -f "${OUT}/${case_id}/model-transfer-bench.yaml" >/dev/null || return 1
  wait_replicas_ready "${deploy}" 1 "${arm_dir}/ready-wall-seconds.txt" || return 1
  if [ -n "${SOURCE_IDLE_EVICT_SECONDS}" ] && [ -n "${digest}" ]; then
    source_memory_snapshot during "${digest}" "${arm_dir}/source-memory-during.json" || true
  fi
  collect_arm_evidence "${deploy}" "${model}" "${iface}" "${arm_dir}" || return 1
  if [ "${SOURCE_CORDONED}" = 1 ]; then
    kubectl uncordon "${SOURCE_NODE}" >/dev/null || return 1
    SOURCE_CORDONED=0
  fi
  kubectl -n "${NS}" delete modeldeployment "${deploy}" --ignore-not-found --wait=true --timeout=180s \
    >/dev/null 2>&1 || true
  if [ -n "${SOURCE_IDLE_EVICT_SECONDS}" ] && [ -n "${digest}" ]; then
    observe_idle_eviction "${arm_dir}" "${digest}"
  fi
  ACTIVE_DEPLOY=""
}

{
  printf 'case_id\tmodel\tengine\tmode\tinterface\tarm\tpod\tnode\trestarts\trdma_check\tbroker_ops\tweight_load_s\tready_wall_s\tchat_checks\n'
} > "${OUT}/comparison.tsv"

FAILED_ARMS=0

if [ -n "${SOURCE_MAX_RAM_BYTES}" ]; then
  patch_source_model_manager || exit 1
fi

for policy in ${POLICIES}; do
  for entry in ${MODELS_SPEC}; do
    IFS=':' read -r f1 f2 f3 f4 f5 f6 <<< "${entry}"
    if [ -n "${f6:-}" ]; then
      case_id="${f1}"
      model="${f2}"
      artifact="${f3}"
      accel="${f4}"
      iface="${DEFAULT_INTERFACE:-${f5}}"
      mode="${f6}"
    else
      model="${f1}"
      artifact="${f2}"
      accel="${f3}"
      enable_ep="${f4}"
      case_id="${artifact}"
      iface="${DEFAULT_INTERFACE:-0}"
      if [ "${enable_ep}" = "1" ]; then
        mode="tp${accel}_ep"
      else
        mode="tp${accel}"
      fi
    fi
    case "${VENDOR}:${model}" in
      NVIDIA:Qwen/Qwen3-8B|NVIDIA:Qwen/Qwen3-Next-80B-A3B-Instruct-FP8|Ascend:Qwen/Qwen3-8B|Ascend:Qwen/Qwen3.6-35B-A3B) ;;
      NVIDIA:Qwen/Qwen3-235B-A22B-Instruct-2507-FP8|NVIDIA:Qwen/Qwen3-235B-A22B-Instruct-2507|NVIDIA:Qwen/Qwen3-VL-235B-A22B-Instruct|NVIDIA:gpustack/GLM-5.3-W4A8)
        if [ "${accel}" != "8" ]; then
          log "ERROR: ${model} is an 8x H100 recipes model and runs in tp8 or tp8_ep, not ${mode}"
          exit 1
        fi
        ;;
      *)
        if [ "${ALLOW_CUSTOM_MODEL}" != "1" ]; then
          log "ERROR: ${model} is outside the ${VENDOR} matrix"
          exit 1
        fi
        log "Admitting custom model ${model} under MT_ALLOW_CUSTOM_MODEL=1"
        ;;
    esac
    log "Starting Case 11 (${ENGINE}): case=${case_id} model=${model} artifact=${artifact} accel=${accel} interface=${iface} mode=${mode}"
    arm="$(printf '%s' "${policy}" | tr '[:upper:]' '[:lower:]')"
    if ! run_arm "${case_id}" "${model}" "${artifact}" "${accel}" "${iface}" "${mode}" "${policy}" "${arm}"; then
      FAILED_ARMS="$((FAILED_ARMS + 1))"
      # Stop with the active identities intact so the exit trap can remove them.
      log "ERROR: arm failed; evidence retained in ${OUT}/${case_id}/${arm}"
      exit 1
    fi
    arm_dir="${OUT}/${case_id}/${arm}"
    wall="$(cat "${arm_dir}/ready-wall-seconds.txt" 2>/dev/null || echo "n/a")"
    chat_ok="$(cat "${arm_dir}/chat-checks.txt" 2>/dev/null || echo "n/a")"
    rdma_ok="$(cat "${arm_dir}/rdma-check.txt" 2>/dev/null || echo "n/a")"
    if [ "${rdma_ok}" = "FAIL" ] || [ "${chat_ok}" != "${CHAT_CHECKS}/${CHAT_CHECKS}" ]; then
      FAILED_ARMS="$((FAILED_ARMS + 1))"
    fi
    if [ -f "${arm_dir}/load-times.tsv" ]; then
      while IFS=$'\t' read -r pod node restarts load_s broker_ops; do
        printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
          "${case_id}" "${model}" "${ENGINE}" "${mode}" "${iface}" "${arm}" "${pod}" "${node}" "${restarts}" "${rdma_ok}" "${broker_ops}" "${load_s}" "${wall}" "${chat_ok}" \
          >> "${OUT}/comparison.tsv"
      done < "${arm_dir}/load-times.tsv"
    fi
  done
done

log "Comparison written to ${OUT}/comparison.tsv"
cat "${OUT}/comparison.tsv"
if [ "${FAILED_ARMS}" -gt 0 ]; then
  log "ERROR: ${FAILED_ARMS} arm(s) failed verification"
  exit 1
fi
