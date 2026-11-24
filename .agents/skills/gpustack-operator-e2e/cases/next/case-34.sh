#!/usr/bin/env bash
#
# CASE 34 — Live multimodal EPD serving behind llm-d-router + vLLM: NIXL encoder-cache leg on
#           NVIDIA CUDA / AMD ROCm, and shared-directory RWX leg on Ascend
#           (MUTATING, self-recovering)
#           AUTO-SKIPS without a pool of 3+ free exclusive NVIDIA/AMD/Ascend cards, pullable
#           serving images, and (on Ascend) a Bound ReadWriteMany PVC
#
#   case-34.sh <NS>
#
# Prerequisites: nodes=1; gpu=nvidia|amd|ascend; cluster=real; inputs=E2E_C34_ACCELERATOR_TYPE,E2E_C34_CARDS,E2E_C34_CLAIM,E2E_C34_IMAGE,E2E_C34_IMAGE_URL,E2E_C34_INSTANCE_TYPE,E2E_C34_MAX_TOKENS,E2E_C34_MODEL,E2E_C34_MOUNT,E2E_C34_PROBE_IMAGE,E2E_C34_READY_BOUND,E2E_C34_ROUTER_IMAGE,E2E_C34_SETTLE_BOUND,E2E_C34_SIDECAR_IMAGE,E2E_C34_STAGE_BOUND,E2E_C34_TP,E2E_C34_WEIGHTS,E2E_SYSTEM_NS
# Goal:        Functional end-to-end verification of live three-role multimodal EPD serving
#              behind llm-d-router + vLLM, dispatching on the named AcceleratorType's platform:
#              - NVIDIA CUDA / AMD ROCm (NIXL leg): three roles Ready with zero restarts and
#                published endpoint; encoder renders --ec-transfer-config (ECCPUConnector,
#                ec_producer), VLLM_USE_V2_MODEL_RUNNER=1, and VLLM_EC_SIDE_CHANNEL_HOST/PORT
#                with host via downward API status.podIP; prefiller renders ECCPUConnector
#                ec_consumer without the side-channel port; decoder routing-proxy renders
#                --ec-connector=ec-nixl; one multimodal chat request returns HTTP 200 with
#                generated content and traverses all three stages verified from engine/sidecar
#                logs (encode POST /v1/chat/completions, prefill POST /v1/chat/completions,
#                decode routing-proxy processing multimodal items).
#              - Ascend (shared-directory RWX leg): caller-supplied PVC is Bound and offers
#                ReadWriteMany; three roles Ready with zero restarts and published endpoint;
#                encoder renders --ec-transfer-config (ECExampleConnector, ec_producer,
#                shared_storage_path) without NIXL vars; prefiller renders ECExampleConnector
#                ec_consumer with matching shared_storage_path; encoder mounts PVC writable,
#                prefiller mounts the same PVC at the same path, decoder mounts no PVC;
#                decoder routing-proxy renders --ec-connector=ec-example; one multimodal chat
#                request returns HTTP 200 with generated content.
# Environment: Real NVIDIA CUDA, AMD ROCm, or Ascend pool whose AcceleratorType reports at least
#              E2E_C34_CARDS * E2E_C34_TP (default 3) free exclusive cards, staged multimodal
#              (vision-language) model weights at E2E_C34_WEIGHTS, pullable vLLM/router/sidecar
#              images, and an entrance LocalQueue in <NS>. Exits 2 when E2E_C34_ACCELERATOR_TYPE /
#              E2E_C34_INSTANCE_TYPE is unset, or on Ascend when E2E_C34_CLAIM is unset.
#              AUTO-SKIPS (exit 0) on unsupported manufacturer, insufficient free cards, non-Bound
#              or non-RWX claim on Ascend, or unpullable images.
# Inputs:      All real, nothing mocked. E2E_C34_ACCELERATOR_TYPE / E2E_C34_INSTANCE_TYPE
#              (required), E2E_C34_CLAIM (required on Ascend; existing RWX PVC in <NS>),
#              E2E_C34_MOUNT (default /ec-cache), E2E_C34_IMAGE (default per manufacturer),
#              E2E_C34_ROUTER_IMAGE (default docker.io/gpustack/llm-router:v0.3.1),
#              E2E_C34_SIDECAR_IMAGE (default docker.io/gpustack/mirrored-llm-d-router-disagg-sidecar:v0.11.0),
#              E2E_C34_WEIGHTS (default /data/models/model_scope/Qwen/Qwen2.5-VL-7B-Instruct),
#              E2E_C34_MODEL (default /models/Qwen2.5-VL-7B-Instruct),
#              E2E_C34_PROBE_IMAGE (default docker.io/curlimages/curl:8.10.1),
#              E2E_C34_IMAGE_URL (default 1x1 PNG data URI), E2E_C34_CARDS (default 3),
#              E2E_C34_TP (default 1), E2E_C34_READY_BOUND (default 1800),
#              E2E_C34_MAX_TOKENS (default 64), E2E_C34_STAGE_BOUND (default 60),
#              E2E_C34_SETTLE_BOUND (default 180), E2E_SYSTEM_NS (default gpustack-system).
# Expected:    All readiness, endpoint, zero-restart, rendered connector/env/mount/sidecar,
#              multimodal HTTP 200 response, and (on NIXL) three-stage traversal checks pass.
# Cleanup:     EXIT trap restores model-deployment-router-image and
#              model-deployment-routing-sidecar-image to their exact original bytes, waits for
#              the settings cache to settle on the router Deployment, then deletes the probe Pod
#              and ModelDeployment and force-releases any Kueue Workloads. Caller's PVC is never
#              deleted.
set -uo pipefail

CASE_ID=34
export CASE_ID
CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "${CASE_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_epd-lib.sh"

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-34.sh <NS>" >&2
  exit 2
fi

MD="e2e-c34-epd"
PROBE_NAME="e2e-c34-probe"
IT="${E2E_C34_ACCELERATOR_TYPE:-${E2E_C34_INSTANCE_TYPE:-}}"
CLAIM="${E2E_C34_CLAIM:-}"
MOUNT="${E2E_C34_MOUNT:-/ec-cache}"
IMAGE="${E2E_C34_IMAGE:-}"
ROUTER_IMAGE="${E2E_C34_ROUTER_IMAGE:-docker.io/gpustack/llm-router:v0.3.1}"
SIDECAR_IMAGE="${E2E_C34_SIDECAR_IMAGE:-docker.io/gpustack/mirrored-llm-d-router-disagg-sidecar:v0.11.0}"
WEIGHTS="${E2E_C34_WEIGHTS:-/data/models/model_scope/Qwen/Qwen2.5-VL-7B-Instruct}"
MODEL="${E2E_C34_MODEL:-/models/Qwen2.5-VL-7B-Instruct}"
PROBE_IMAGE="${E2E_C34_PROBE_IMAGE:-docker.io/curlimages/curl:8.10.1}"
IMAGE_URL="${E2E_C34_IMAGE_URL:-data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==}"
CARDS="${E2E_C34_CARDS:-3}"
TP="${E2E_C34_TP:-1}"
READY_BOUND="${E2E_C34_READY_BOUND:-1800}"
MAX_TOKENS="${E2E_C34_MAX_TOKENS:-64}"
STAGE_BOUND="${E2E_C34_STAGE_BOUND:-60}"
SYSTEM_NS="${E2E_SYSTEM_NS:-gpustack-system}"
SETTLE_BOUND="${E2E_C34_SETTLE_BOUND:-180}"

if [ -n "$(epd_cluster_registry "$SYSTEM_NS")" ]; then
  ROUTER_IMAGE="$(epd_registry_relative "$ROUTER_IMAGE")"
  SIDECAR_IMAGE="$(epd_registry_relative "$SIDECAR_IMAGE")"
fi

skip() {
  echo "[case-34] SKIP: $1"
  [ "${#ROWS[@]}" -gt 0 ] && print_rows
  echo "NOTHING FURTHER WAS VERIFIED"
  exit 0
}
skip_row() {
  record SKIP "$1" "$2"
  print_rows
  echo "[case-34] skipped"
  exit 0
}

# --- input gates (exit 2) ---------------------------------------------------

command -v jq >/dev/null 2>&1 || { echo "[case-34] jq is required." >&2; exit 2; }

if [ -z "$IT" ]; then
  echo "[case-34] E2E_C34_ACCELERATOR_TYPE / E2E_C34_INSTANCE_TYPE is required (nvidia, amd, or ascend); refusing to guess a pool." >&2
  exit 2
fi

local_queues="$(kubectl -n "$NS" get localqueue --no-headers 2>/dev/null || true)"
if [ -z "$local_queues" ]; then
  echo "[case-34] namespace '${NS}' carries no LocalQueue; run where the pool's entrance LocalQueue exists." >&2
  exit 2
fi

IT_JSON="$(kubectl get acceleratortypes.worker.gpustack.ai "$IT" -o json 2>/dev/null || true)"
if [ -z "$IT_JSON" ]; then
  IT_JSON="$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o json 2>/dev/null || true)"
fi
if [ -z "$IT_JSON" ]; then
  echo "[case-34] AcceleratorType / InstanceType '${IT}' not found." >&2
  exit 2
fi

MANUFACTURER="$(printf '%s' "$IT_JSON" | jq -r '.status.detail.manufacturer // ""')"
MODE=""
case "$MANUFACTURER" in
  nvidia)
    MODE="nixl"
    [ -n "$IMAGE" ] || IMAGE="docker.io/gpustack/runner:cuda13.0-vllm0.30.0"
    ;;
  amd)
    MODE="nixl"
    [ -n "$IMAGE" ] || IMAGE="quay.io/gpustack/runner:rocm6.3-vllm0.30.0"
    ;;
  ascend)
    MODE="shared-dir"
    [ -n "$IMAGE" ] || IMAGE="quay.io/gpustack/runner:cann9.1-910b-vllm0.30.0rc1-router"
    if [ -z "$CLAIM" ]; then
      echo "[case-34] E2E_C34_CLAIM is required on Ascend (an existing ReadWriteMany PVC in ${NS})." >&2
      exit 2
    fi
    ;;
  *)
    skip "type '${IT}' reports manufacturer '${MANUFACTURER:-<none>}', expected nvidia, amd, or ascend."
    ;;
esac

IT_PHASE="$(printf '%s' "$IT_JSON" | jq -r '.status.phase // ""')"
FREE_EX="$(printf '%s' "$IT_JSON" | jq -r '.status.accelerator.remaining // "0"')"
[ "$IT_PHASE" = Active ] || skip "AcceleratorType '${IT}' is not Active (phase '${IT_PHASE}')."
NEED_CARDS=$((CARDS * TP))
{ [ "$FREE_EX" -ge "$NEED_CARDS" ] 2>/dev/null; } ||
  skip "pool needs ${NEED_CARDS} free exclusive cards for three roles at TP=${TP}, has ${FREE_EX}."

echo "[case-34] pool '${IT}' (${MANUFACTURER}, leg=${MODE}): ${FREE_EX} free exclusive cards, need ${NEED_CARDS} (TP=${TP})"

# --- Ascend RWX PVC pre-check -----------------------------------------------

if [ "$MODE" = "shared-dir" ]; then
  PVC_JSON="$(kubectl -n "$NS" get pvc "$CLAIM" -o json 2>/dev/null || true)"
  if [ -z "$PVC_JSON" ]; then
    skip "no pvc/${CLAIM} exists in namespace ${NS}, and the Ascend shared-directory leg requires an existing RWX claim."
  fi
  MODES="$(printf '%s' "$PVC_JSON" | jq -r '.spec.accessModes // [] | join(",")')"
  case "$MODES" in
    *ReadWriteMany*)
      record PASS "the claim offers ReadWriteMany" "pvc/${CLAIM} accessModes=${MODES}"
      ;;
    *)
      skip_row "the claim offers ReadWriteMany" \
        "pvc/${CLAIM} accessModes=${MODES:-<none>}: encoder and prefiller require ReadWriteMany storage"
      ;;
  esac
  PVC_PHASE="$(printf '%s' "$PVC_JSON" | jq -r '.status.phase // ""')"
  if [ "$PVC_PHASE" = Bound ]; then
    record PASS "the claim is Bound" "pvc/${CLAIM} phase=Bound"
  else
    skip_row "the claim is Bound" \
      "pvc/${CLAIM} is '${PVC_PHASE}', so no Pod can mount it and the shared-directory leg cannot run"
  fi
fi

# --- image settings pin and trap --------------------------------------------

ROUTER_SETTING=model-deployment-router-image
SIDECAR_SETTING=model-deployment-routing-sidecar-image

force_release() {
  local md="$1" row wl uids u
  uids="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${md}" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' 2>/dev/null || true)"
  [ -n "$uids" ] || return 0
  while IFS= read -r row; do
    [ -n "$row" ] || continue
    wl="${row%%=*}"
    for u in $uids; do
      case " ${row#*=} " in
        *" $u "*)
          kubectl -n "$NS" delete workloads.kueue.x-k8s.io "$wl" \
            --ignore-not-found --wait=false >/dev/null 2>&1 || true
          break
          ;;
      esac
    done
  done <<EOF
$(kubectl -n "$NS" get workloads.kueue.x-k8s.io \
  -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.ownerReferences[*].uid}{"\n"}{end}' 2>/dev/null || true)
EOF
  return 0
}

# shellcheck disable=SC2317 # Invoked by EXIT trap.
cleanup() {
  echo
  echo "[case-34] cleanup"
  epd_restore_router_sidecar_images "$SYSTEM_NS" "$ROUTER_SETTING" "$SIDECAR_SETTING"
  epd_settle_router_image "$NS" "$MD" "$SETTLE_BOUND" "$ROUTER_IMAGE" ||
    echo "[case-34] settle bound ${SETTLE_BOUND}s spent with router still on '${ROUTER_IMAGE}'; settings restore not yet read back" >&2
  kubectl -n "$NS" delete pod "$PROBE_NAME" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "$MD" \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
  sleep 5
  force_release "$MD"
}
trap cleanup EXIT

epd_pin_router_sidecar_images "$SYSTEM_NS" "$ROUTER_SETTING" "$SIDECAR_SETTING" \
  "$ROUTER_IMAGE" "$SIDECAR_IMAGE"

# --- fixture apply ----------------------------------------------------------

if [ "$MODE" = "nixl" ]; then
  APPLY_OUT="$(cat <<YAML | kubectl apply -f - 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD}
  namespace: ${NS}
spec:
  engine:
    name: vLLM
    image: ${IMAGE}
  model:
    name: ${MODEL}
  router:
    name: llm-d-router
  roles:
  - name: encode
    kind: Encode
    acceleratorType: ${IT}
    replicas: 1
    size: 1
    image: ${IMAGE}
    imagePullPolicy: IfNotPresent
    resources:
      accelerator: "${TP}"
    additionalVolumes:
    - hostPath:
        path: ${WEIGHTS}
        type: Directory
      mountPath: ${MODEL}
      readOnly: true
  - name: prefill
    kind: Prefill
    acceleratorType: ${IT}
    replicas: 1
    size: 1
    image: ${IMAGE}
    imagePullPolicy: IfNotPresent
    resources:
      accelerator: "${TP}"
    additionalVolumes:
    - hostPath:
        path: ${WEIGHTS}
        type: Directory
      mountPath: ${MODEL}
      readOnly: true
  - name: decode
    kind: Decode
    acceleratorType: ${IT}
    replicas: 1
    size: 1
    image: ${IMAGE}
    imagePullPolicy: IfNotPresent
    resources:
      accelerator: "${TP}"
    additionalVolumes:
    - hostPath:
        path: ${WEIGHTS}
        type: Directory
      mountPath: ${MODEL}
      readOnly: true
YAML
)"
else
  APPLY_OUT="$(cat <<YAML | kubectl apply -f - 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD}
  namespace: ${NS}
spec:
  engine:
    name: vLLM
    image: ${IMAGE}
  model:
    name: ${MODEL}
  router:
    name: llm-d-router
  roles:
  - name: encode
    kind: Encode
    acceleratorType: ${IT}
    replicas: 1
    size: 1
    image: ${IMAGE}
    imagePullPolicy: IfNotPresent
    resources:
      accelerator: "${TP}"
    additionalVolumes:
    - mountPath: ${MOUNT}
      persistentVolumeClaim:
        claimName: ${CLAIM}
    - hostPath:
        path: ${WEIGHTS}
        type: Directory
      mountPath: ${MODEL}
      readOnly: true
  - name: prefill
    kind: Prefill
    acceleratorType: ${IT}
    replicas: 1
    size: 1
    image: ${IMAGE}
    imagePullPolicy: IfNotPresent
    resources:
      accelerator: "${TP}"
    additionalVolumes:
    - mountPath: ${MOUNT}
      persistentVolumeClaim:
        claimName: ${CLAIM}
    - hostPath:
        path: ${WEIGHTS}
        type: Directory
      mountPath: ${MODEL}
      readOnly: true
  - name: decode
    kind: Decode
    acceleratorType: ${IT}
    replicas: 1
    size: 1
    image: ${IMAGE}
    imagePullPolicy: IfNotPresent
    resources:
      accelerator: "${TP}"
    additionalVolumes:
    - hostPath:
        path: ${WEIGHTS}
        type: Directory
      mountPath: ${MODEL}
      readOnly: true
YAML
)"
fi

if kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "$MD" >/dev/null 2>&1; then
  record PASS "apply" "three-role EPD deployment admitted on ${IT} (llm-d-router, ${MANUFACTURER}, leg=${MODE})"
else
  record FAIL "apply" "refused: ${APPLY_OUT}"
  results
  exit 1
fi

# --- wait: pods Ready, endpoint published, no crash-loop --------------------

EN_POD=""; PF_POD=""; DC_POD=""
UNPULLABLE_COMPONENT=""
for comp in encode prefill decode; do
  epd_poll_role "$NS" "$MD" "$READY_BOUND" "$comp"; rc=$?
  if [ "$rc" = 2 ]; then
    UNPULLABLE_COMPONENT="$comp"
    break
  fi
done

if [ -n "$UNPULLABLE_COMPONENT" ]; then
  record SKIP "the three role Pods reach Ready" \
    "the ${UNPULLABLE_COMPONENT} Pod waits on $(epd_pull_reason "$NS" "$MD" "$UNPULLABLE_COMPONENT" | tr -s ' '): image would not pull"
  print_rows
  exit 0
fi

for pair in "encode:${EN_POD}" "prefill:${PF_POD}" "decode:${DC_POD}"; do
  comp="${pair%%:*}"; pod="${pair#*:}"
  if [ -n "$pod" ]; then
    record PASS "${comp} pod" "${pod} Ready"
  else
    record FAIL "${comp} pod" "no Ready ${comp} pod within ${READY_BOUND}s"
  fi
done

ALL_READY=no
[ -n "$EN_POD" ] && [ -n "$PF_POD" ] && [ -n "$DC_POD" ] && ALL_READY=yes

ENDPOINT="$(epd_wait_endpoint "$NS" "$MD" "$READY_BOUND")"; ENDPOINT_RC=$?
ROUTER_POD="$(epd_router_pod "$NS" "$MD")"
CHAIN_UP=no
[ "$ALL_READY" = yes ] && [ -n "$ROUTER_POD" ] && CHAIN_UP=yes

if [ "$ENDPOINT_RC" = 0 ]; then
  record PASS "endpoint" "${ENDPOINT}"
elif [ "$ENDPOINT_RC" = 2 ]; then
  record SKIP "endpoint" \
    "the router Pod waits on $(epd_router_pull_reason "$NS" "$MD" | tr -s ' '), router image '${ROUTER_IMAGE}' would not pull"
elif [ "$CHAIN_UP" = yes ]; then
  record FAIL "endpoint" \
    "all three role Pods and router Pod (${ROUTER_POD}) are Ready but no endpoint appeared within ${READY_BOUND}s"
else
  record SKIP "endpoint" \
    "no endpoint within ${READY_BOUND}s and chain was not up (encode=${EN_POD:-none} prefill=${PF_POD:-none} decode=${DC_POD:-none} router=${ROUTER_POD:-none})"
fi

if [ "$ALL_READY" = yes ]; then
  RESTARTS="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" \
    -o jsonpath='{range .items[*]}{.status.containerStatuses[*].restartCount}{" "}{end}' 2>/dev/null || true)"
  if [ -n "$RESTARTS" ] && ! printf '%s' "$RESTARTS" | grep -qE '[^0 ]'; then
    record PASS "no crash-loop" "every container restartCount is 0 (${RESTARTS})"
  else
    record FAIL "no crash-loop" "restartCounts: ${RESTARTS:-<none>}"
  fi
fi

# --- rendered leg verification (NIXL vs shared-directory) -------------------

if [ "$MODE" = "nixl" ]; then
  if [ -n "$EN_POD" ]; then
    DOC="$(epd_ec_document "$NS" "$EN_POD")"
    if [ -n "$DOC" ]; then
      if printf '%s' "$DOC" | jq -e '.ec_connector == "ECCPUConnector" and .ec_role == "ec_producer"' >/dev/null 2>&1; then
        record PASS "encoder document" "ec_connector=ECCPUConnector ec_role=ec_producer on ${EN_POD}"
      else
        record FAIL "encoder document" "got: ${DOC}"
      fi
      SIDE_HOST_FIELD="$(epd_ec_env_field_path "$NS" "$EN_POD" VLLM_EC_SIDE_CHANNEL_HOST)"
      SIDE_PORT="$(epd_ec_env "$NS" "$EN_POD" VLLM_EC_SIDE_CHANNEL_PORT)"
      RUNNER="$(epd_ec_env "$NS" "$EN_POD" VLLM_USE_V2_MODEL_RUNNER)"
      POD_IP="$(kubectl -n "$NS" get pod "$EN_POD" -o jsonpath='{.status.podIP}' 2>/dev/null || true)"
      if [ "$SIDE_HOST_FIELD" = status.podIP ] && [ -n "$POD_IP" ] && [ -n "$SIDE_PORT" ]; then
        record PASS "encoder side channel" \
          "VLLM_EC_SIDE_CHANNEL_HOST fieldRef=${SIDE_HOST_FIELD} (podIP=${POD_IP}), PORT=${SIDE_PORT}"
      else
        record FAIL "encoder side channel" \
          "host field='${SIDE_HOST_FIELD:-<none>}' port='${SIDE_PORT}' podIP='${POD_IP:-<none>}'"
      fi
      if [ "$RUNNER" = 1 ]; then
        record PASS "encoder model runner" "VLLM_USE_V2_MODEL_RUNNER=1"
      else
        record FAIL "encoder model runner" "VLLM_USE_V2_MODEL_RUNNER='${RUNNER}'"
      fi
    else
      record FAIL "encoder document" "no --ec-transfer-config argument on ${EN_POD}"
    fi
  fi

  if [ -n "$PF_POD" ]; then
    DOC="$(epd_ec_document "$NS" "$PF_POD")"
    if [ -n "$DOC" ]; then
      if printf '%s' "$DOC" | jq -e '.ec_connector == "ECCPUConnector" and .ec_role == "ec_consumer"' >/dev/null 2>&1; then
        record PASS "prefiller document" "ec_connector=ECCPUConnector ec_role=ec_consumer on ${PF_POD}"
      else
        record FAIL "prefiller document" "got: ${DOC}"
      fi
      SIDE_PORT="$(epd_ec_env "$NS" "$PF_POD" VLLM_EC_SIDE_CHANNEL_PORT)"
      if [ -z "$SIDE_PORT" ]; then
        record PASS "prefiller binds no side channel" "VLLM_EC_SIDE_CHANNEL_PORT absent on ${PF_POD}"
      else
        record FAIL "prefiller binds no side channel" "unexpected VLLM_EC_SIDE_CHANNEL_PORT=${SIDE_PORT}"
      fi
    else
      record FAIL "prefiller document" "no --ec-transfer-config argument on ${PF_POD}"
    fi
  fi

  if [ -n "$DC_POD" ]; then
    EC_FLAG="$(kubectl -n "$NS" get pod "$DC_POD" -o json 2>/dev/null | jq -r \
      '[(.spec.containers[]?, .spec.initContainers[]?) | select(.name=="routing-proxy") | (.args // [])[]] | map(select(startswith("--ec-connector="))) | first // ""' 2>/dev/null || true)"
    if [ "$EC_FLAG" = "--ec-connector=ec-nixl" ]; then
      record PASS "decode sidecar connector" "routing-proxy on ${DC_POD} carries --ec-connector=ec-nixl"
    else
      record FAIL "decode sidecar connector" "routing-proxy carries '${EC_FLAG:-<none>}'"
    fi
  fi
else
  if [ -n "$EN_POD" ]; then
    DOC="$(epd_ec_document "$NS" "$EN_POD")"
    if [ -n "$DOC" ]; then
      if printf '%s' "$DOC" | jq -e --arg p "$MOUNT" \
        '.ec_connector == "ECExampleConnector" and .ec_role == "ec_producer" and .ec_connector_extra_config.shared_storage_path == $p' \
        >/dev/null 2>&1; then
        record PASS "encoder document" "ECExampleConnector ec_producer shared_storage_path=${MOUNT} on ${EN_POD}"
      else
        record FAIL "encoder document" "got: ${DOC}"
      fi
      SIDE_PORT="$(epd_ec_env "$NS" "$EN_POD" VLLM_EC_SIDE_CHANNEL_PORT)"
      RUNNER="$(epd_ec_env "$NS" "$EN_POD" VLLM_USE_V2_MODEL_RUNNER)"
      if [ -z "$SIDE_PORT" ] && [ -z "$RUNNER" ]; then
        record PASS "encoder carries no NIXL variable" "VLLM_EC_SIDE_CHANNEL_PORT and VLLM_USE_V2_MODEL_RUNNER absent on ${EN_POD}"
      else
        record FAIL "encoder carries no NIXL variable" "port='${SIDE_PORT}' runner='${RUNNER}'"
      fi
    else
      record FAIL "encoder document" "no --ec-transfer-config argument on ${EN_POD}"
    fi
  fi

  if [ -n "$PF_POD" ]; then
    DOC="$(epd_ec_document "$NS" "$PF_POD")"
    if [ -n "$DOC" ]; then
      if printf '%s' "$DOC" | jq -e --arg p "$MOUNT" \
        '.ec_connector == "ECExampleConnector" and .ec_role == "ec_consumer" and .ec_connector_extra_config.shared_storage_path == $p' \
        >/dev/null 2>&1; then
        record PASS "prefiller document" "ECExampleConnector ec_consumer shared_storage_path=${MOUNT} on ${PF_POD}"
      else
        record FAIL "prefiller document" "got: ${DOC}"
      fi
    else
      record FAIL "prefiller document" "no --ec-transfer-config argument on ${PF_POD}"
    fi
  fi

  if [ "$ALL_READY" = yes ]; then
    EN_CLAIM="$(epd_role_pod_claim "$NS" "$MD" encode)"
    PF_CLAIM="$(epd_role_pod_claim "$NS" "$MD" prefill)"
    DC_CLAIM="$(epd_role_pod_claim "$NS" "$MD" decode)"
    case "$EN_CLAIM" in
      "${CLAIM}|${MOUNT}|false")
        record PASS "encoder mounts the claim writable" "rendered ${EN_CLAIM}"
        ;;
      *)
        record FAIL "encoder mounts the claim writable" "rendered '${EN_CLAIM:-<none>}', expected '${CLAIM}|${MOUNT}|false'"
        ;;
    esac
    case "$PF_CLAIM" in
      "${CLAIM}|${MOUNT}|"*)
        record PASS "prefiller mounts the same claim at the same path" "rendered ${PF_CLAIM}"
        ;;
      *)
        record FAIL "prefiller mounts the same claim at the same path" "rendered '${PF_CLAIM:-<none>}', expected '${CLAIM}|${MOUNT}|*'"
        ;;
    esac
    if [ -z "$DC_CLAIM" ]; then
      record PASS "decoder mounts no claim" "shared-directory leg ends at prefiller"
    else
      record FAIL "decoder mounts no claim" "rendered ${DC_CLAIM}"
    fi
  fi

  if [ -n "$DC_POD" ]; then
    EC_FLAG="$(kubectl -n "$NS" get pod "$DC_POD" -o json 2>/dev/null | jq -r \
      '[(.spec.containers[]?, .spec.initContainers[]?) | select(.name=="routing-proxy") | (.args // [])[]] | map(select(startswith("--ec-connector="))) | first // ""' 2>/dev/null || true)"
    if [ "$EC_FLAG" = "--ec-connector=ec-example" ]; then
      record PASS "decode sidecar connector" "routing-proxy on ${DC_POD} carries --ec-connector=ec-example"
    else
      record FAIL "decode sidecar connector" "routing-proxy carries '${EC_FLAG:-<none>}'"
    fi
  fi
fi

# --- multimodal request through the router ----------------------------------

if [ "$ALL_READY" != yes ]; then
  record SKIP "a multimodal request through the router returns 200" \
    "the three role Pods were not all Ready, so no chain was available to serve a request"
  results
  exit $?
fi

kubectl -n "$NS" run "$PROBE_NAME" --image="$PROBE_IMAGE" --restart=Never \
  --labels="gpustack.ai/e2e-probe=${MD}" --command -- sleep 1800 >/dev/null 2>&1
if ! kubectl -n "$NS" wait "pod/${PROBE_NAME}" --for=condition=Ready --timeout=300s >/dev/null 2>&1; then
  record SKIP "a multimodal request through the router returns 200" \
    "probe Pod '${PROBE_NAME}' never became Ready on image '${PROBE_IMAGE}'"
  results
  exit $?
fi

# Prompt clears a 16-token KV block so the prefix-cache decider does not degrade to decode-only.
PAYLOAD="$(jq -nc --arg model "$MODEL" --arg url "$IMAGE_URL" --argjson mt "$MAX_TOKENS" '{
  model: $model,
  messages: [{role: "user", content: [
    {type: "image_url", image_url: {url: $url}},
    {type: "text", text: "Describe this image in detail. Name every colour you can see, say where each one appears in the picture, and explain how the shapes are arranged."}
  ]}],
  max_tokens: $mt
}')"

RESPONSE="$(kubectl -n "$NS" exec "$PROBE_NAME" -- curl -sS --max-time 300 \
  -w $'\nHTTP_STATUS:%{http_code}\n' -H 'Content-Type: application/json' \
  --data-binary "$PAYLOAD" "http://${MD}-router.${NS}.svc:8000/v1/chat/completions" 2>&1)" || RESPONSE=""

CONTENT=""
if [[ "$RESPONSE" == *'HTTP_STATUS:200'* ]]; then
  CONTENT="$(sed '/^HTTP_STATUS:/d' <<<"$RESPONSE" | jq -r '.choices[0].message.content // empty' 2>/dev/null || true)"
fi

if [ -n "$CONTENT" ]; then
  record PASS "a multimodal request through the router returns 200" \
    "HTTP 200 with generated content from ${MD}-router (${#CONTENT} chars)"
else
  record FAIL "a multimodal request through the router returns 200" \
    "no content returned: $(printf '%s' "$RESPONSE" | tr '\n' ' ' | cut -c1-220)"
fi

# --- three-stage engine/sidecar traversal check (NIXL leg) ------------------

if [ "$MODE" = "nixl" ]; then
  stage_pod() {
    kubectl -n "$NS" get pods \
      -l "app.kubernetes.io/instance=${MD},modeldeployment.gpustack.ai/role-kind=$1" \
      -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true
  }

  stage_logged() {
    local pod="$1" container="$2" marker="$3" deadline=$((SECONDS + STAGE_BOUND)) logs
    while [ "$SECONDS" -lt "$deadline" ]; do
      logs="$(kubectl -n "$NS" logs "$pod" -c "$container" --tail=4000 2>/dev/null || true)"
      case "$logs" in
        *"$marker"*) return 0 ;;
      esac
      sleep 3
    done
    return 1
  }

  check_stage_log() {
    local row="$1" pod="$2" container="$3" marker="$4" why="$5"
    if [ -z "$pod" ]; then
      record FAIL "$row" "no Pod with matching role-kind label found"
      return 1
    fi
    if stage_logged "$pod" "$container" "$marker"; then
      record PASS "$row" "${pod}/${container} logged '${marker}'"
      return 0
    fi
    record FAIL "$row" "${pod}/${container} never logged '${marker}' within ${STAGE_BOUND}s; ${why}"
    return 1
  }

  dump_stage_logs() {
    local pod container
    echo "[case-34] request did not traverse every stage; router and Pod logs follow" >&2
    if [ -n "$ROUTER_POD" ]; then
      echo "[case-34] ---- router ${ROUTER_POD}/epp" >&2
      kubectl -n "$NS" logs "$ROUTER_POD" -c epp --tail=200 2>&1 >&2 || true
    fi
    for pod in "$STAGE_EP" "$STAGE_PF" "$STAGE_DC"; do
      [ -n "$pod" ] || continue
      for container in main routing-proxy; do
        echo "[case-34] ---- ${pod}/${container}" >&2
        kubectl -n "$NS" logs "$pod" -c "$container" --tail=200 2>&1 >&2 || true
      done
    done
  }

  STAGE_EP="$(stage_pod encode)"
  STAGE_PF="$(stage_pod prefill)"
  STAGE_DC="$(stage_pod decode)"

  STAGES_RAN=yes
  check_stage_log "the encoder served the request" "$STAGE_EP" main 'POST /v1/chat/completions' \
    "vision tokens must be produced on the encoder stage" || STAGES_RAN=no
  check_stage_log "the prefiller served the request" "$STAGE_PF" main 'POST /v1/chat/completions' \
    "prompt must traverse the prefill stage" || STAGES_RAN=no
  check_stage_log "the decoder pulled the encodings" "$STAGE_DC" routing-proxy 'processing multimodal items' \
    "routing-proxy must pull encodings from the encoder stage" || STAGES_RAN=no

  [ "$STAGES_RAN" = yes ] || dump_stage_logs
fi

results
