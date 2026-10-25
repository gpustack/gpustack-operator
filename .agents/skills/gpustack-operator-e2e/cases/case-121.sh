#!/usr/bin/env bash
#
# CASE 121 — The admitted Ascend row runs: vLLM behind llm-d-router with the shared-directory
#           encoder-cache leg, and a multimodal request goes through the router
#           (MUTATING, self-recovering)
#           AUTO-SKIPS without a real Ascend pool with three free cards and a ReadWriteMany claim
#
#   case-121.sh <NS>
#
# Goal:        This is the functional reading of the admitted matrix row
#              llm-d-router + vLLM + Ascend + ECExampleConnector, and the only case that measures
#              the row the whole design calls mounted-directory. Case 119 proved the two roles
#              NAME one claim and that a pair of witness Pods can exchange bytes through it; it
#              said nothing about whether the operator's own roles do. Here the claim is a real
#              ReadWriteMany volume shared by the encoder's Pod and the prefiller's Pod, and a real
#              engine writes the embeddings it produces into it and the prefiller reads them out.
#              The engine's view is asserted, not inferred: the encoder carries --ec-transfer-config
#              naming ECExampleConnector as ec_producer with the shared path in the document, the
#              prefiller carries the same argument as ec_consumer with the same path, and the decode
#              Pod's sidecar carries --ec-connector=ec-example.
#
#              THE CLAIM MUST BE READWRITEMANY, and the case says so rather than accepting a
#              ReadWriteOnce one. This row's two ends are two Pods, which the scheduler may place
#              on two nodes; a ReadWriteOnce claim binds to one node's disk and the prefiller on
#              another would mount a directory holding no encodings, which is a storage answer and
#              not a verdict about the leg. E2E_C121_CLAIM therefore has to name a claim whose
#              accessModes include ReadWriteMany, and the case reads that from the object.
#
#              A claim that fails either test is a SKIP, not a failure: it says the CLUSTER's storage
#              is not set up for this row and nothing about the operator was exercised. Both
#              answers are rows in the table rather than lines of prose, so the pass already recorded
#              beside them survives the skip.
#
# Environment: A real Ascend accelerator pool whose InstanceType reports at least THREE free
#              exclusive cards (one per role at TP 1 by default), model weights staged at a hostPath
#              on the accelerator nodes, the vllm-ascend engine image pullable, and the llm-d-router
#              image pullable. A multimodal (vision-language) model is REQUIRED, not a text-only
#              one: a text request never engages the encoder stage, so a text-only model would
#              leave the leg untested while every row passed. The namespace must carry the pool's
#              entrance LocalQueue and must permit the case's temporary probe Pod.
#              Exits 2 (input required) when E2E_C121_INSTANCE_TYPE or E2E_C121_CLAIM is unset.
#              AUTO-SKIPS (exit 0) when no InstanceType reports manufacturer ascend, when the named
#              one holds fewer than E2E_C121_CARDS (default 3) free exclusive cards, when the named
#              claim is not Bound or does not offer ReadWriteMany (each a row in the table, not a
#              line of prose), or when any of the three images would not pull (naming the image).
#
#              THE ENDPOINT IS POLLED, NOT READ ONCE, and a bound spent is only a verdict when the
#              WHOLE chain came up: the controller writes .status.endpoint once the router's own
#              Deployment reports a ready replica, which is a fourth workload no role poll below
#              covers, so a single read would race the writer. The router Pod is watched during that
#              wait, and an unpullable router image is a SKIP naming the image, never a verdict. An
#              endpoint that appears inside E2E_C121_READY_BOUND seconds is a PASS; a bound spent
#              with the three role Pods AND the router Pod Ready is a FAIL, because a chain that came
#              up and published no front door is a defect; a bound spent with any of those Pods
#              missing is a SKIP naming which ones never appeared.
#
# Inputs:      All real, nothing mocked. One ModelDeployment with an encode role and a prefill role
#              mounting E2E_C121_CLAIM at one path (the decode role mounts nothing — this leg ends
#              at the prefiller); the load is one multimodal chat request carrying an image, driven
#              from a throwaway probe Pod through the router's Service.
#              E2E_C121_INSTANCE_TYPE (required), E2E_C121_CLAIM (required, an existing
#              ReadWriteMany claim in <NS>), E2E_C121_MOUNT (default /ec-cache),
#              E2E_C121_IMAGE (engine, default
#              quay.io/gpustack/runner:cann9.1-910b-vllm0.30.0rc1-router; the tag may not be published yet, so pass an image you built),
#              E2E_C121_ROUTER_IMAGE (default docker.io/gpustack/llm-router:v0.3.1),
#              E2E_C121_SIDECAR_IMAGE (default docker.io/gpustack/mirrored-llm-d-router-disagg-sidecar:v0.11.0),
#              E2E_C121_WEIGHTS (default /data/models/model_scope/Qwen/Qwen2.5-VL-7B-Instruct),
#              E2E_C121_MODEL (default /models/Qwen2.5-VL-7B-Instruct),
#              E2E_C121_PROBE_IMAGE (default docker.io/library/busybox:1.37),
#              E2E_C121_IMAGE_URL (default a 1x1 PNG data URI, so the request needs no network),
#              E2E_C121_CARDS (default 3), E2E_C121_TP (default 1),
#              E2E_C121_READY_BOUND (default 1800), E2E_C121_MAX_TOKENS (default 64),
#              E2E_C121_SETTLE_BOUND (default 180 — how long cleanup waits for the restored image
#              to be read back through the settings cache).
#
#              THE ROUTER AND SIDECAR IMAGES COME FROM THE SETTINGS SECRET, which the case
#              overrides when the caller names different ones and restores in the trap: both
#              settings carry the images the operator renders, and a case that depends on whatever
#              the cluster already carries is not a reading of this operator. The values are
#              restored to their exact original state BEFORE the deployment is deleted, and the
#              restore is then waited for on the router Deployment's own image, because the worker
#              reads settings through a cache.
#
# Expected:    - the three role Pods reach Ready with zero restarts (no crash-loop), and the
#                deployment publishes an endpoint;
#              - the claim the caller named is Bound and offers ReadWriteMany, which is what makes
#                the two Pods one filesystem rather than two mounts of one name;
#              - the encoder Pod's main container carries --ec-transfer-config naming
#                ECExampleConnector as ec_producer with ec_connector_extra_config.shared_storage_path
#                equal to the mount path, and carries NO NIXL side channel and no
#                VLLM_USE_V2_MODEL_RUNNER (neither belongs to this connector);
#              - the prefiller Pod carries the same document as ec_consumer with the same path;
#              - the decoder Pod runs the routing sidecar with --ec-connector=ec-example, and no
#                role Pod mounts a claim it was not given (the decoder mounts none);
#              - one multimodal chat request through the router Service returns HTTP 200 with
#                generated content.
#
# Cleanup:     The trap restores the two image settings to their exact original values, waits for
#              the restore to be read back through the settings cache (bounded), and only then
#              deletes the ModelDeployment and the probe Pod and releases the Workload holding its
#              replicas by hand if the group wedges. The claim is the CALLER's and is never
#              deleted. Creates no cluster-scoped object and changes no baseline; runs on pass AND
#              fail, safe to re-run.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-121.sh <NS>" >&2
  exit 2
fi

# The image override and the live serving group are read through the library: the registry the
# operator prefixes onto every image, the pin and the restore of the router and the sidecar, the
# role Pods, the router and the connector document.
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_epd-lib.sh"

MD=case121-epd
IT="${E2E_C121_INSTANCE_TYPE:-}"
CLAIM="${E2E_C121_CLAIM:-}"
MOUNT="${E2E_C121_MOUNT:-/ec-cache}"
IMAGE="${E2E_C121_IMAGE:-quay.io/gpustack/runner:cann9.1-910b-vllm0.30.0rc1-router}"
ROUTER_IMAGE="${E2E_C121_ROUTER_IMAGE:-docker.io/gpustack/llm-router:v0.3.1}"
SIDECAR_IMAGE="${E2E_C121_SIDECAR_IMAGE:-docker.io/gpustack/mirrored-llm-d-router-disagg-sidecar:v0.11.0}"
WEIGHTS="${E2E_C121_WEIGHTS:-/data/models/model_scope/Qwen/Qwen2.5-VL-7B-Instruct}"
MODEL="${E2E_C121_MODEL:-/models/Qwen2.5-VL-7B-Instruct}"
# Same defect as case-120: busybox ships no curl, and `|| RESPONSE=""` swallowed the exec's 127.
PROBE_IMAGE="${E2E_C121_PROBE_IMAGE:-docker.io/curlimages/curl:8.10.1}"
IMAGE_URL="${E2E_C121_IMAGE_URL:-data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==}"
CARDS="${E2E_C121_CARDS:-3}"
TP="${E2E_C121_TP:-1}"
READY_BOUND="${E2E_C121_READY_BOUND:-1800}"
MAX_TOKENS="${E2E_C121_MAX_TOKENS:-64}"
SYSTEM_NS="${E2E_SYSTEM_NS:-gpustack-system}"
SETTLE_BOUND="${E2E_C121_SETTLE_BOUND:-180}"
# The operator prefixes `container-registry` onto every image it chooses, and the two settings below
# take a reference of the form `namespace/name:tag`. A value that already names a host would render as
# `<registry>/<host>/...` on a cluster that sets a registry, so drop the host there.
if [ -n "$(epd_cluster_registry "$SYSTEM_NS")" ]; then
  ROUTER_IMAGE="$(epd_registry_relative "$ROUTER_IMAGE")"
  SIDECAR_IMAGE="$(epd_registry_relative "$SIDECAR_IMAGE")"
fi
PROBE_NAME=case121-probe

# A skip before anything was recorded is prose, and a skip after it is a row: the answer this run
# reached about its own input belongs in the table beside the passes it already recorded, so the
# rows are printed before the case leaves either way. `${#ROWS[@]}` is the same emptiness test
# the shared printer and the other libraries in this suite use.
skip() {
  echo "[case-121] SKIP: $1"
  [ "${#ROWS[@]}" -gt 0 ] && print_rows
  echo "NOTHING FURTHER WAS VERIFIED"
  exit 0
}
skip_row() { record SKIP "$1" "$2"; print_rows; echo "[case-121] skipped"; exit 0; }

# --- input gates (exit 2) ---------------------------------------------------

command -v jq >/dev/null 2>&1 || { echo "[case-121] jq is required." >&2; exit 2; }

if [ -z "$IT" ]; then
  echo "[case-121] E2E_C121_INSTANCE_TYPE is required (an accelerated InstanceType reporting" >&2
  echo "          manufacturer ascend); refusing to guess a pool." >&2
  exit 2
fi
if [ -z "$CLAIM" ]; then
  echo "[case-121] E2E_C121_CLAIM is required: this row carries the encoder's output through a" >&2
  echo "          shared directory, so it needs a ReadWriteMany claim the operator did not" >&2
  echo "          create. Stage one (a CSI driver with RWX, or an NFS export) and pass its name." >&2
  exit 2
fi

# Capture first, then test. `kubectl ... | grep -q .` is a RACE under `set -o pipefail`:
# grep -q exits on its first match, kubectl takes SIGPIPE and exits 141, and pipefail
# then reports the whole pipeline as failed even though the match succeeded. Measured on
# this cluster: 3 passes out of 10 runs with 5 LocalQueues present (PIPESTATUS 141 0).
local_queues="$(kubectl -n "$NS" get localqueue --no-headers 2>/dev/null)"
[ -n "$local_queues" ] || {
  echo "[case-121] namespace '${NS}' carries no LocalQueue; run where the pool's entrance" >&2
  echo "          LocalQueue exists, or the group is created and never admitted." >&2
  exit 2
}

IT_JSON="$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o json 2>/dev/null)"
if [ -z "$IT_JSON" ]; then
  echo "[case-121] InstanceType '${IT}' not found." >&2
  exit 2
fi
MANUFACTURER="$(printf '%s' "$IT_JSON" | jq -r '.status.detail.manufacturer // ""')"
[ "$MANUFACTURER" = ascend ] ||
  skip "InstanceType '${IT}' reports manufacturer '${MANUFACTURER:-<none>}', and the admitted row this case runs is Ascend."

IT_PHASE="$(printf '%s' "$IT_JSON" | jq -r '.status.phase // ""')"
FREE_EX="$(printf '%s' "$IT_JSON" | jq -r '.status.accelerator.remaining // "0"')"
[ "$IT_PHASE" = Active ] || skip "InstanceType '${IT}' is not Active (phase '${IT_PHASE}')."
NEED_CARDS=$((CARDS * TP))
{ [ "$FREE_EX" -ge "$NEED_CARDS" ] 2>/dev/null; } ||
  skip "pool needs ${NEED_CARDS} free exclusive cards for three roles at TP=${TP}, has ${FREE_EX}."

echo "[case-121] pool '${IT}': ${FREE_EX} free exclusive cards, need ${NEED_CARDS} (TP=${TP})"

# --- the claim, read before anything is applied ------------------------------

# Read here rather than after the apply, and both answers are SKIPs rather than failures: a claim
# that cannot serve two Pods is a storage answer about the CLUSTER, and nothing about the leg has
# been exercised at this point. The access modes and the phase are the admission ticket for this row.
PVC_JSON="$(kubectl -n "$NS" get pvc "$CLAIM" -o json 2>/dev/null)"
if [ -z "$PVC_JSON" ]; then
  skip "no pvc/${CLAIM} exists in namespace ${NS}, and this row needs a claim the operator did not create."
fi
MODES="$(printf '%s' "$PVC_JSON" | jq -r '.spec.accessModes // [] | join(",")')"
case "$MODES" in
  *ReadWriteMany*)
    record PASS "the claim offers ReadWriteMany" "pvc/${CLAIM} accessModes=${MODES}"
    ;;
  *)
    skip_row "the claim offers ReadWriteMany" \
      "pvc/${CLAIM} accessModes=${MODES:-<none>}: the encoder and the prefiller are two Pods, and a claim one node can bind leaves the other reading an empty directory. That is a storage answer about this cluster, not a verdict about the leg, so nothing below ran"
    ;;
esac
PVC_PHASE="$(printf '%s' "$PVC_JSON" | jq -r '.status.phase // ""')"
[ "$PVC_PHASE" = Bound ] ||
  skip_row "the claim is Bound" \
    "pvc/${CLAIM} is '${PVC_PHASE}', so no Pod can mount it and the leg cannot run. That is a storage answer about this cluster, not a verdict about the operator, so nothing below ran"

# --- the image settings, overridden and restored -----------------------------

ROUTER_SETTING=model-deployment-router-image
SIDECAR_SETTING=model-deployment-routing-sidecar-image
# The pin, the restore and the wait for the router to be re-rendered are the library's:
# epd_pin_router_sidecar_images, epd_restore_router_sidecar_images and epd_settle_router_image.

force_release() {
  local md="$1" row wl uids u
  uids="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${md}" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' 2>/dev/null)"
  [ -n "$uids" ] || return 0
  while IFS= read -r row; do
    [ -n "$row" ] || continue
    wl="${row%%=*}"
    for u in $uids; do
      case " ${row#*=} " in
        *" $u "*)
          kubectl -n "$NS" delete workloads.kueue.x-k8s.io "$wl" \
            --ignore-not-found --wait=false >/dev/null 2>&1
          break
          ;;
      esac
    done
  done <<EOF
$(kubectl -n "$NS" get workloads.kueue.x-k8s.io \
  -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.ownerReferences[*].uid}{"\n"}{end}' 2>/dev/null)
EOF
  return 0
}
# THE SETTINGS GO BACK BEFORE THE DEPLOYMENT IS DELETED, which is the order that lets the settle
# prove the restore: while the ModelDeployment stands, its router Deployment stands with it, and
# the image on that Deployment is the one thing in the cluster that shows what the worker has read
# since the patch. Deleting the deployment first would remove the only witness.
cleanup() {
  echo
  echo "[case-121] cleanup"
  epd_restore_router_sidecar_images "$SYSTEM_NS" "$ROUTER_SETTING" "$SIDECAR_SETTING"
  # The bound is spent and the router still runs this case's image: the restore was written but the
  # worker has not read it back. Cleanup cannot record a row from here, and returning silently would
  # hand the next case a router it did not choose without saying so, so say it on stderr.
  epd_settle_router_image "$NS" "$MD" "$SETTLE_BOUND" "$ROUTER_IMAGE" ||
    echo "[case-121] settle bound ${SETTLE_BOUND}s spent with the router still on '${ROUTER_IMAGE}'; the settings restore has not been read back" >&2
  kubectl -n "$NS" delete pod "$PROBE_NAME" --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "$MD" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  sleep 5
  force_release "$MD"
}

# The trap is armed BEFORE the first mutation, so an interrupt between the two patches and here
# cannot leave the cluster running a router image this run chose.
trap cleanup EXIT

epd_pin_router_sidecar_images "$SYSTEM_NS" "$ROUTER_SETTING" "$SIDECAR_SETTING" \
  "$ROUTER_IMAGE" "$SIDECAR_IMAGE"

# --- fixture ----------------------------------------------------------------

# The encoder and the prefiller mount ONE claim at ONE path; the decoder mounts nothing, because
# this leg ends at the prefiller. The three mounts must agree, which admission refuses when they
# do not — so writing them identically here is not a formality, it is the object the refusal
# would otherwise reject.
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
    instanceType: ${IT}
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
    instanceType: ${IT}
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
    instanceType: ${IT}
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

if kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "$MD" >/dev/null 2>&1; then
  record PASS "apply" \
    "the three-role encoder deployment was admitted on ${IT}, encode and prefill mounting ${CLAIM} at ${MOUNT}"
else
  record FAIL "apply" "refused: ${APPLY_OUT}"
  print_rows
  exit 1
fi

# --- wait: pods Ready, no crash-loop, endpoint published --------------------

# A pull failure on any of the three images is a SKIP naming the image (case-81's rule): a
# container nobody could start is not a verdict about the leg. The reading of a role's Pods is the
# library's: epd_pull_reason, epd_wait_pods and epd_poll_role.

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
    "the ${UNPULLABLE_COMPONENT} Pod waits on $(epd_pull_reason "$NS" "$MD" "$UNPULLABLE_COMPONENT" | tr -s ' '): no container started, so the leg was never exercised. Render the images and re-run"
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

# THE ROUTER POD IS WATCHED TOO, because the endpoint is written from the router Deployment's own
# status: a router image that would not pull is the one explanation for a missing endpoint that no
# role poll can see. It is the environment answer the role images already get, and reading it as an
# operator defect would cost the whole bound and then be wrong.
#
# The reading of the router is the library's: epd_router_pods, epd_router_pull_reason,
# epd_router_pod and epd_wait_endpoint.

ALL_READY=no
[ -n "$EN_POD" ] && [ -n "$PF_POD" ] && [ -n "$DC_POD" ] && ALL_READY=yes

ENDPOINT="$(epd_wait_endpoint "$NS" "$MD" "$READY_BOUND")"; ENDPOINT_RC=$?
ROUTER_POD="$(epd_router_pod "$NS" "$MD")"
# The FAIL verdict needs the WHOLE chain up, the router included. A router that never came up
# publishes no front door either, and calling that a defect would blame the operator for an image
# it was told to run.
CHAIN_UP=no
[ "$ALL_READY" = yes ] && [ -n "$ROUTER_POD" ] && CHAIN_UP=yes

if [ "$ENDPOINT_RC" = 0 ]; then
  record PASS "endpoint" "${ENDPOINT}"
elif [ "$ENDPOINT_RC" = 2 ]; then
  # An image that would not pull is not a verdict about the leg: no container started, so there was
  # never a front door to publish. Same shape as the role-image SKIP above, naming the image.
  record SKIP "endpoint" \
    "the router Pod waits on $(epd_router_pull_reason "$NS" "$MD" | tr -s ' '), so router image '${ROUTER_IMAGE}' would not pull and no front door could be published. Render the image and re-run"
elif [ "$CHAIN_UP" = yes ]; then
  # Every Pod came up, the router's included, and the controller still publishes no front door.
  # Nothing else explains that, so it is a defect in what the operator publishes rather than an
  # environment answer.
  record FAIL "endpoint" \
    "all three role Pods and the router Pod (${ROUTER_POD}) are Ready and no endpoint appeared within ${READY_BOUND}s, so a chain that came up publishes nothing a request could be sent to"
else
  # A missing Pod is the reason: a missing endpoint that follows a missing Pod says nothing about the
  # operator, and the Pod rows above already carry the verdict.
  record SKIP "endpoint" \
    "no endpoint within ${READY_BOUND}s, and the chain was not up (encode=${EN_POD:-none} prefill=${PF_POD:-none} decode=${DC_POD:-none} router=${ROUTER_POD:-none}), so the missing endpoint follows the missing Pods"
fi

if [ "$ALL_READY" = yes ]; then
  RESTARTS="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" \
    -o jsonpath='{range .items[*]}{.status.containerStatuses[*].restartCount}{" "}{end}' 2>/dev/null)"
  if [ -n "$RESTARTS" ] && ! printf '%s' "$RESTARTS" | grep -qE '[^0 ]'; then
    record PASS "no crash-loop" "every container restartCount is 0 (${RESTARTS})"
  else
    record FAIL "no crash-loop" "restartCounts: ${RESTARTS:-<none>}"
  fi
fi

# --- the rendered leg, and the mounts that carry it -------------------------

# The document the engine was handed, the variable it was given and the claim a role mounts are
# read through the library: epd_ec_document, epd_ec_env and epd_role_pod_claim.

if [ -n "$EN_POD" ]; then
  DOC="$(epd_ec_document "$NS" "$EN_POD")"
  if [ -n "$DOC" ]; then
    if printf '%s' "$DOC" | jq -e \
      --arg p "$MOUNT" \
      '.ec_connector == "ECExampleConnector" and .ec_role == "ec_producer"
       and .ec_connector_extra_config.shared_storage_path == $p' >/dev/null 2>&1; then
      record PASS "encoder document" \
        "ECExampleConnector ec_producer, shared_storage_path=${MOUNT}, on ${EN_POD}"
    else
      record FAIL "encoder document" "got: ${DOC}"
    fi
    # NEITHER NIXL VARIABLE BELONGS ON THIS ROW. The side channel is the other row's transport,
    # and VLLM_USE_V2_MODEL_RUNNER is the other connector's precondition; both are asserted
    # absent so a render that reached for the wrong connector is caught here rather than as a
    # transfer failure the log never names.
    SIDE_PORT="$(epd_ec_env "$NS" "$EN_POD" VLLM_EC_SIDE_CHANNEL_PORT)"
    RUNNER="$(epd_ec_env "$NS" "$EN_POD" VLLM_USE_V2_MODEL_RUNNER)"
    if [ -z "$SIDE_PORT" ] && [ -z "$RUNNER" ]; then
      record PASS "encoder carries no NIXL variable" \
        "neither VLLM_EC_SIDE_CHANNEL_PORT nor VLLM_USE_V2_MODEL_RUNNER is set on ${EN_POD}, which is right for the shared-directory connector"
    else
      record FAIL "encoder carries no NIXL variable" \
        "port='${SIDE_PORT}' runner='${RUNNER}': this row's connector is the shared directory, not the NIXL side channel"
    fi
  else
    record FAIL "encoder document" "no --ec-transfer-config argument on ${EN_POD}"
  fi
fi

if [ -n "$PF_POD" ]; then
  DOC="$(epd_ec_document "$NS" "$PF_POD")"
  if [ -n "$DOC" ]; then
    if printf '%s' "$DOC" | jq -e \
      --arg p "$MOUNT" \
      '.ec_connector == "ECExampleConnector" and .ec_role == "ec_consumer"
       and .ec_connector_extra_config.shared_storage_path == $p' >/dev/null 2>&1; then
      record PASS "prefiller document" \
        "ECExampleConnector ec_consumer, shared_storage_path=${MOUNT}, on ${PF_POD}"
    else
      record FAIL "prefiller document" "got: ${DOC}"
    fi
  else
    record FAIL "prefiller document" "no --ec-transfer-config argument on ${PF_POD}"
  fi
fi

if [ -n "$EN_POD" ] && [ -n "$PF_POD" ] && [ -n "$DC_POD" ]; then
  EN_CLAIM="$(epd_role_pod_claim "$NS" "$MD" encode)"
  PF_CLAIM="$(epd_role_pod_claim "$NS" "$MD" prefill)"
  DC_CLAIM="$(epd_role_pod_claim "$NS" "$MD" decode)"

  case "$EN_CLAIM" in
    "${CLAIM}|${MOUNT}|false")
      record PASS "encoder mounts the claim writable" "rendered ${EN_CLAIM}"
      ;;
    *)
      record FAIL "encoder mounts the claim writable" \
        "rendered '${EN_CLAIM:-no claim mount}', expected '${CLAIM}|${MOUNT}|false': the encoder writes the embeddings the connector carries"
      ;;
  esac

  case "$PF_CLAIM" in
    "${CLAIM}|${MOUNT}|"*)
      # Read-only is the PREFILLER's own choice — it only reads — so this row pins the claim and
      # the path and leaves the flag open, exactly as case 117 does.
      record PASS "prefiller mounts the same claim at the same path" \
        "rendered ${PF_CLAIM}, matching the encoder's claim and path"
      ;;
    *)
      record FAIL "prefiller mounts the same claim at the same path" \
        "rendered '${PF_CLAIM:-no claim mount}', expected '${CLAIM}|${MOUNT}|<any readOnly>'"
      ;;
  esac

  if [ -z "$DC_CLAIM" ]; then
    record PASS "decoder mounts no claim" "the shared-directory leg ends at the prefiller"
  else
    record FAIL "decoder mounts no claim" "rendered ${DC_CLAIM}"
  fi
fi

# The sidecar flag is the sidecar's OWN vocabulary (ec-example), not the engine's name
# (ECExampleConnector). Asserting the flag pins the second spelling, which is the one the sidecar
# refuses to start without.
if [ -n "$DC_POD" ]; then
  EC_FLAG="$(kubectl -n "$NS" get pod "$DC_POD" -o json 2>/dev/null | jq -r \
    '[(.spec.containers[]?, .spec.initContainers[]?) | select(.name=="routing-proxy") | (.args // [])[]] | map(select(startswith("--ec-connector="))) | first // ""' 2>/dev/null)"
  if [ "$EC_FLAG" = "--ec-connector=ec-example" ]; then
    record PASS "decode sidecar connector" "routing-proxy on ${DC_POD} carries --ec-connector=ec-example"
  else
    record FAIL "decode sidecar connector" \
      "routing-proxy carries '${EC_FLAG:-<no --ec-connector flag>}', and the sidecar refuses to start on any other spelling"
  fi
fi

# --- the request: a multimodal turn through the router ----------------------

# The only row that can say the three roles spoke to each other over the shared directory. The
# request carries an image: a text-only turn never engages the encoder stage.
if [ -z "$EN_POD" ] || [ -z "$PF_POD" ] || [ -z "$DC_POD" ]; then
  record SKIP "a multimodal request through the router returns 200" \
    "the three role Pods were not all Ready, so there is no chain to send a request through"
  print_rows
  [ "$FAILS" -eq 0 ] || { echo "[case-121] ${FAILS} check(s) FAILED"; exit 1; }
  echo "[case-121] all checks passed"
  exit 0
fi

kubectl -n "$NS" run "$PROBE_NAME" --image="$PROBE_IMAGE" --restart=Never \
  --labels="gpustack.ai/e2e-probe=${MD}" --command -- sleep 1800 >/dev/null 2>&1
if ! kubectl -n "$NS" wait "pod/$PROBE_NAME" --for=condition=Ready --timeout=300s >/dev/null 2>&1; then
  record SKIP "a multimodal request through the router returns 200" \
    "the probe Pod '${PROBE_NAME}' never became Ready on image '${PROBE_IMAGE}', so no request was sent"
  print_rows
  [ "$FAILS" -eq 0 ] || { echo "[case-121] ${FAILS} check(s) FAILED"; exit 1; }
  echo "[case-121] all checks passed"
  exit 0
fi

PAYLOAD="$(jq -nc --arg model "$MODEL" --arg url "$IMAGE_URL" --argjson mt "$MAX_TOKENS" '{
  model: $model,
  messages: [{role: "user", content: [
    {type: "image_url", image_url: {url: $url}},
    {type: "text", text: "Name one colour you can see in this image."}
  ]}],
  max_tokens: $mt
}')"

RESPONSE="$(kubectl -n "$NS" exec "$PROBE_NAME" -- curl -sS --max-time 300 \
  -w $'\nHTTP_STATUS:%{http_code}\n' -H 'Content-Type: application/json' \
  --data-binary "$PAYLOAD" "http://${MD}-router.${NS}.svc:8000/v1/chat/completions" 2>&1)" || RESPONSE=""

CONTENT=""
if [[ "$RESPONSE" == *'HTTP_STATUS:200'* ]]; then
  CONTENT="$(sed '/^HTTP_STATUS:/d' <<<"$RESPONSE" | jq -r '.choices[0].message.content // empty' 2>/dev/null)"
fi

if [ -n "$CONTENT" ]; then
  record PASS "a multimodal request through the router returns 200" \
    "HTTP 200 with generated content from ${MD}-router (${#CONTENT} chars)"
else
  record FAIL "a multimodal request through the router returns 200" \
    "the turn carried an image and returned no content: $(printf '%s' "$RESPONSE" | tr '\n' ' ' | cut -c1-220)"
fi

# Results.
print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-121] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-121] all checks passed"
