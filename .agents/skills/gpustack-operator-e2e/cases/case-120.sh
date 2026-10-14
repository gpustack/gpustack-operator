#!/usr/bin/env bash
#
# CASE 120 — The admitted ROCm row runs: vLLM on AMD behind llm-d-router with the NIXL encoder-cache
#           leg, and a multimodal request goes through the router (MUTATING, self-recovering)
#           AUTO-SKIPS without a real AMD pool with three free cards and the serving images
#
#   case-120.sh <NS>
#
# Goal:        This is the functional reading of the admitted matrix row
#              llm-d-router + vLLM + AMD + ECCPUConnector. Case 116 pins the ADMISSION of the
#              shape, case 119 pins the shared-directory render of the OTHER row; nothing yet
#              proves this one runs. The encoder's output reaches the prefiller over NIXL here —
#              the sidecar on the decode Pod pulls it — so the rendered mount rows of case 119
#              say nothing about this leg, and the only reading that says anything is a real
#              engine moving real encodings: three roles Ready on real AMD cards, the rendered
#              leg naming the network connector on both halves that carry it, and one multimodal
#              chat request answered through the router with HTTP 200.
#
#              THE THREE ROWS ARE INDEPENDENT, and each can fail while the others pass. Readiness
#              is a scheduling and scheduling chain fact. The documents are a render fact: the
#              encoder carries --ec-transfer-config naming ECCPUConnector as ec_producer with the
#              NIXL side channel bound to the Pod's own address, the prefiller carries the same
#              argument as ec_consumer, and the decode Pod's sidecar carries --ec-connector=ec-nixl.
#              The request is a runtime fact, and it is the only row that can say the three roles
#              actually spoke to each other: a 200 with generated content means the router picked
#              all three stages and the answer came back, which no amount of Ready conditions or
#              correct argv proves on its own.
#
# Environment: A real AMD/ROCm accelerator pool whose InstanceType reports at least THREE free
#              exclusive cards (one per role — this row runs each role at TP 1 by default), the
#              vllm-ascend-free ROCm vLLM engine image pullable, and the llm-d-router image
#              pullable. Model weights staged at a hostPath on the accelerator nodes. A multimodal
#              (vision-language) model is REQUIRED here, not a text-only one: a text request never
#              engages the encoder stage, so a text-only model would leave the leg untested while
#              every row passed. The namespace must carry the pool's entrance LocalQueue.
#              Exits 2 (input required) when E2E_C120_INSTANCE_TYPE is unset — the case refuses
#              to guess a pool. AUTO-SKIPS (exit 0) when no InstanceType reports manufacturer
#              amd, when the named one holds fewer than E2E_C120_CARDS (default 3) free exclusive
#              cards, or when any of the three images would not pull (naming the image); each skip
#              prints what it found.
#
#              THE ENDPOINT IS POLLED, NOT READ ONCE, and a bound spent is only a verdict when the
#              WHOLE chain came up: the controller writes .status.endpoint after it observes the
#              router Deployment's own ready replica, so a single read races the writer. The router
#              Pod is watched during that wait, because it is the one workload whose image failure
#              explains a missing endpoint — and an unpullable router image is a SKIP naming the
#              image, never a verdict. An endpoint inside E2E_C120_READY_BOUND seconds is a PASS; a
#              bound spent with the three role Pods AND the router Pod Ready is a FAIL, because a
#              chain that came up and published no front door is a defect; a bound spent with any of
#              those Pods missing is a SKIP naming which ones never appeared, because there the
#              missing endpoint only follows the Pods.
#
# Inputs:      All real, nothing mocked. One ModelDeployment with an encode, a prefill and a
#              decode role on the AMD type; the load is one multimodal chat request carrying an
#              image, driven from a throwaway probe Pod through the router's Service.
#              E2E_C120_INSTANCE_TYPE (required),
#              E2E_C120_IMAGE (engine, default quay.io/gpustack/runner:rocm6.3-vllm0.30.0),
#              E2E_C120_VERSION (engine version, default 0.30.0 — the floor ECCPUConnector first
#              appears in, so a default run clears it),
#              E2E_C120_ROUTER_IMAGE (default docker.io/gpustack/llm-router:v0.3.1),
#              E2E_C120_SIDECAR_IMAGE (default docker.io/gpustack/mirrored-llm-d-router-disagg-sidecar:v0.11.0),
#              E2E_C120_WEIGHTS (default /data/models/model_scope/Qwen/Qwen2.5-VL-7B-Instruct),
#              E2E_C120_MODEL (default /models/Qwen2.5-VL-7B-Instruct),
#              E2E_C120_PROBE_IMAGE (default docker.io/library/busybox:1.37),
#              E2E_C120_IMAGE_URL (default a 1x1 PNG data URI, so the request needs no network),
#              E2E_C120_CARDS (default 3), E2E_C120_TP (default 1),
#              E2E_C120_READY_BOUND (default 1800), E2E_C120_MAX_TOKENS (default 64),
#              E2E_C120_SETTLE_BOUND (default 180 — how long cleanup waits for the restored image
#              to be read back through the settings cache).
#
#              THE ROUTER AND SIDECAR IMAGES COME FROM THE SETTINGS SECRET, which the case
#              overrides when the caller names different ones and restores in the trap: both
#              settings carry the images the operator renders, and case-70's convention is to
#              pin them per case rather than leave the row dependent on whatever the cluster
#              already carries. The values are restored to their exact original state BEFORE the
#              deployment is deleted, and the restore is then waited for on the router Deployment's
#              own image, because the worker reads settings through a cache: returning while the
#              router still runs this case's image would hand the next case a router it did not
#              choose.
#
# Expected:    - the three role Pods reach Ready with zero restarts (no crash-loop), and the
#                deployment publishes an endpoint;
#              - the encoder Pod's main container carries --ec-transfer-config naming
#                ECCPUConnector as ec_producer, plus VLLM_USE_V2_MODEL_RUNNER=1 and the
#                VLLM_EC_SIDE_CHANNEL_HOST/PORT pair, with the host rendered through the
#                downward API (valueFrom.fieldRef.fieldPath = status.podIP) so the producer's
#                side channel binds its own Pod address rather than localhost;
#              - the prefiller Pod carries the same argument as ec_consumer, and does NOT carry
#                the side channel pair;
#              - the decode Pod runs the routing sidecar with --ec-connector=ec-nixl;
#              - one multimodal chat request through the router Service returns HTTP 200 with
#                generated content.
#
# Cleanup:     The trap restores the two image settings to their exact original values, waits for
#              the restore to be read back through the settings cache (bounded), and only then
#              deletes the ModelDeployment and the probe Pod and releases the Workload holding its
#              replicas by hand if the group wedges. Creates no cluster-scoped object and changes no
#              baseline; runs on pass AND fail, safe to re-run.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-120.sh <NS>" >&2
  exit 2
fi

MD=case120-epd
IT="${E2E_C120_INSTANCE_TYPE:-}"
IMAGE="${E2E_C120_IMAGE:-quay.io/gpustack/runner:rocm6.3-vllm0.30.0}"
VERSION="${E2E_C120_VERSION:-0.30.0}"
ROUTER_IMAGE="${E2E_C120_ROUTER_IMAGE:-docker.io/gpustack/llm-router:v0.3.1}"
SIDECAR_IMAGE="${E2E_C120_SIDECAR_IMAGE:-docker.io/gpustack/mirrored-llm-d-router-disagg-sidecar:v0.11.0}"
WEIGHTS="${E2E_C120_WEIGHTS:-/data/models/model_scope/Qwen/Qwen2.5-VL-7B-Instruct}"
MODEL="${E2E_C120_MODEL:-/models/Qwen2.5-VL-7B-Instruct}"
PROBE_IMAGE="${E2E_C120_PROBE_IMAGE:-docker.io/library/busybox:1.37}"
IMAGE_URL="${E2E_C120_IMAGE_URL:-data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==}"
CARDS="${E2E_C120_CARDS:-3}"
TP="${E2E_C120_TP:-1}"
READY_BOUND="${E2E_C120_READY_BOUND:-1800}"
MAX_TOKENS="${E2E_C120_MAX_TOKENS:-64}"
SYSTEM_NS="${E2E_SYSTEM_NS:-gpustack-system}"
SETTLE_BOUND="${E2E_C120_SETTLE_BOUND:-180}"
# 30 minutes is not slack: this row loads a vision-language model on three roles, and the encoder
# compiles a model it then never answers with. A smaller bound would report a slow cluster as a
# broken leg.
PROBE_NAME=case120-probe

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

skip() { echo "[case-120] SKIP: $1"; echo "NOTHING WAS VERIFIED"; exit 0; }

# --- input gates (exit 2) ---------------------------------------------------

command -v jq >/dev/null 2>&1 || { echo "[case-120] jq is required." >&2; exit 2; }

if [ -z "$IT" ]; then
  echo "[case-120] E2E_C120_INSTANCE_TYPE is required (an accelerated InstanceType reporting" >&2
  echo "          manufacturer amd); refusing to guess a pool." >&2
  exit 2
fi

kubectl -n "$NS" get localqueue --no-headers 2>/dev/null | grep -q . || {
  echo "[case-120] namespace '${NS}' carries no LocalQueue; run where the pool's entrance" >&2
  echo "          LocalQueue exists, or the group is created and never admitted." >&2
  exit 2
}

# The manufacturer check is the platform the MATRIX admits, read from the object rather than
# assumed from the variable name: an InstanceType whose manufacturer is not amd is refused at
# admission by the very row this case exercises, and every row below would then read a refusal
# as a failure of the operator.
IT_JSON="$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o json 2>/dev/null)"
if [ -z "$IT_JSON" ]; then
  echo "[case-120] InstanceType '${IT}' not found." >&2
  exit 2
fi
MANUFACTURER="$(printf '%s' "$IT_JSON" | jq -r '.status.detail.manufacturer // ""')"
[ "$MANUFACTURER" = amd ] ||
  skip "InstanceType '${IT}' reports manufacturer '${MANUFACTURER:-<none>}', and the admitted row this case runs is AMD."

IT_PHASE="$(printf '%s' "$IT_JSON" | jq -r '.status.phase // ""')"
FREE_EX="$(printf '%s' "$IT_JSON" | jq -r '.status.accelerator.remaining // "0"')"
[ "$IT_PHASE" = Active ] || skip "InstanceType '${IT}' is not Active (phase '${IT_PHASE}')."
NEED_CARDS=$((CARDS * TP))
{ [ "$FREE_EX" -ge "$NEED_CARDS" ] 2>/dev/null; } ||
  skip "pool needs ${NEED_CARDS} free exclusive cards for three roles at TP=${TP}, has ${FREE_EX}."

echo "[case-120] pool '${IT}': ${FREE_EX} free exclusive cards, need ${NEED_CARDS} (TP=${TP})"

# --- the image settings, overridden and restored -----------------------------

# Both settings are read before anything is applied and written back in the trap, which is armed
# before the first patch. The restore is a PATCH of the exact prior bytes rather than a delete of
# the key, because a key the cluster shipped at its default is indistinguishable from one a user
# set, and deleting the former would leave this run's baseline changed for every case after it.
ROUTER_SETTING=model-deployment-router-image
SIDECAR_SETTING=model-deployment-routing-sidecar-image
ORIG_ROUTER="$(kubectl -n "$SYSTEM_NS" get secret gpustack-settings \
  -o jsonpath="{.data.${ROUTER_SETTING}}" 2>/dev/null)"
ORIG_SIDECAR="$(kubectl -n "$SYSTEM_NS" get secret gpustack-settings \
  -o jsonpath="{.data.${SIDECAR_SETTING}}" 2>/dev/null)"

set_image_setting() { # key value
  kubectl -n "$SYSTEM_NS" patch secret gpustack-settings --type=merge \
    -p "{\"data\":{\"$1\":\"$(printf '%s' "$2" | base64)\"}}" >/dev/null 2>&1
}

restore_image_setting() { # key original
  local v=null
  [ -n "$2" ] && v="\"$2\""
  kubectl -n "$SYSTEM_NS" patch secret gpustack-settings --type=merge \
    -p "{\"data\":{\"$1\":${v}}}" >/dev/null 2>&1
}

# Restoring the bytes is not the end of the restore: the worker reads settings through a cache and
# re-renders the router Deployment by the value, so a case that returned while the router still ran
# this case's image would hand the next one a router it did not choose. Wait for the restore to be
# READ BACK, which is what the Deployment's own image shows.
router_image() {
  kubectl -n "$NS" get deploy "${MD}-router" \
    -o jsonpath='{.spec.template.spec.containers[*].image}' 2>/dev/null
}

settle_back() {
  local deadline=$((SECONDS + SETTLE_BOUND)) image
  while [ "$SECONDS" -lt "$deadline" ]; do
    image="$(router_image)"
    # No router was ever rendered — an apply that was refused, or a run that never reached it — so
    # nothing carries this case's image and there is nothing to wait for.
    [ -n "$image" ] || return 0
    # The restored value is not always comparable: a key the cluster shipped absent is restored by
    # deleting it, and what the Deployment then carries is the operator's own default, which this
    # case never knew. "No longer this case's image" is the condition both spellings satisfy.
    case "$image" in
      *"$ROUTER_IMAGE"*) ;;
      *) return 0 ;;
    esac
    sleep 5
  done
  # The bound is spent and the router still runs this case's image: the restore was written but the
  # worker has not read it back. Cleanup cannot record a row from here, and returning silently would
  # hand the next case a router it did not choose without saying so, so say it on stderr.
  echo "[case-120] settle bound ${SETTLE_BOUND}s spent with the router still on '${ROUTER_IMAGE}'; the settings restore has not been read back" >&2
  return 0
}

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

# THE SETTINGS GO BACK BEFORE THE DEPLOYMENT IS DELETED, which is the order that lets settle_back
# prove the restore: while the ModelDeployment stands, its router Deployment stands with it, and
# the image on that Deployment is the one thing in the cluster that shows what the worker has read
# since the patch. Deleting the deployment first would remove the only witness.
cleanup() {
  echo
  echo "[case-120] cleanup"
  restore_image_setting "$ROUTER_SETTING" "$ORIG_ROUTER"
  restore_image_setting "$SIDECAR_SETTING" "$ORIG_SIDECAR"
  settle_back
  # By name only. kubectl refuses a name combined with a selector ("name cannot be provided when a
  # selector is specified") and stderr is silenced below, so a stale probe would survive every run
  # and take the name the next run's kubectl run needs. The e2e-probe label is still on the Pod; it
  # is what the header's re-run contract is about, not a second way to select it here.
  kubectl -n "$NS" delete pod "$PROBE_NAME" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "$MD" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  sleep 5
  force_release "$MD"
}

# The trap is armed BEFORE the first mutation, so an interrupt between the two patches and here
# cannot leave the cluster running a router image this run chose.
trap cleanup EXIT

set_image_setting "$ROUTER_SETTING" "$ROUTER_IMAGE"
set_image_setting "$SIDECAR_SETTING" "$SIDECAR_IMAGE"

# --- fixture ----------------------------------------------------------------

# The decode role carries the sidecar that pulls encodings over NIXL, so the deployment needs the
# router: an encoder role without one is refused at admission. No claim is mounted anywhere: this
# row's connector moves embeddings over the network, and a claim here would be the OTHER row's
# subject (case 119).
APPLY_OUT="$(cat <<YAML | kubectl apply -f - 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD}
  namespace: ${NS}
spec:
  engine:
    name: vLLM
    version: "${VERSION}"
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
  record PASS "apply" "the three-role encoder deployment was admitted on ${IT} (llm-d-router, vLLM ${VERSION}, amd)"
else
  record FAIL "apply" "refused: ${APPLY_OUT}"
  print_rows
  exit 1
fi

# --- wait: pods Ready, no crash-loop, endpoint published --------------------

# A pull failure on any of the three images is a SKIP naming the image (case-81's rule): a
# container nobody could start is not a verdict about the leg. The bound is generous because a
# vision-language model is loaded three times over.
pull_reason() { # <component>
  kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD},app.kubernetes.io/component=$1" \
    -o jsonpath='{range .items[*]}{.status.containerStatuses[*].state.waiting.reason}{" "}{end}' 2>/dev/null
}

wait_pods() { # <component> -> pod name on stdout once Ready, rc!=0 on timeout
  local comp="$1" deadline=$((SECONDS + READY_BOUND)) pods ready p
  while [ "$SECONDS" -lt "$deadline" ]; do
    pods="$(kubectl -n "$NS" get pods \
      -l "app.kubernetes.io/instance=${MD},app.kubernetes.io/component=${comp}" \
      -o json 2>/dev/null)"
    ready="$(printf '%s' "$pods" | jq -r \
      '[.items[] | select([.status.conditions // [] | .[] | select(.type=="Ready" and .status=="True")] | length > 0)] | length')"
    if [ "${ready:-0}" -ge 1 ]; then
      printf '%s' "$pods" | jq -r '.items[0].metadata.name'
      return 0
    fi
    for p in ImagePullBackOff ErrImagePull; do
      case "$(pull_reason "$comp")" in
        *"$p"*) return 2 ;;
      esac
    done
    sleep 15
  done
  return 1
}

# Each role is polled separately and the exit code is read IMMEDIATELY, because a pull failure on
# the first role means the other two polls would sit out the whole bound before anything reported
# it. 2 is "an image would not pull", which is an environment answer; 1 is a timeout.
poll_role() { # <component> -> sets POD_<component> and returns the wait_pods code
  local comp="$1" pod rc
  pod="$(wait_pods "$comp")"; rc=$?
  case "$comp" in
    encode) EN_POD="$pod" ;;
    prefill) PF_POD="$pod" ;;
    decode) DC_POD="$pod" ;;
  esac
  return "$rc"
}

EN_POD=""; PF_POD=""; DC_POD=""
UNPULLABLE_COMPONENT=""
for comp in encode prefill decode; do
  poll_role "$comp"; rc=$?
  if [ "$rc" = 2 ]; then
    UNPULLABLE_COMPONENT="$comp"
    break
  fi
done

if [ -n "$UNPULLABLE_COMPONENT" ]; then
  record SKIP "the three role Pods reach Ready" \
    "the ${UNPULLABLE_COMPONENT} Pod waits on $(pull_reason "$UNPULLABLE_COMPONENT" | tr -s ' '): no container started, so the leg was never exercised. Render the images and re-run"
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
# The router's Pods are selected by the router Deployment's OWN selector rather than by a label
# spelled here. The router carries the same instance label the roles carry, plus a router key of its
# own, so an instance-only selector would return the role Pods too and learn nothing about the
# router; the Deployment's selector is the one that cannot drift from what was rendered.
router_pods() {
  local sel
  # shellcheck disable=SC2016  # the dollar names are kubectl jsonpath map variables, and must not expand
  sel="$(kubectl -n "$NS" get deploy "${MD}-router" \
    -o jsonpath='{range $k,$v := .spec.selector.matchLabels}{$k}={$v},{end}' 2>/dev/null | sed 's/,$//')"
  [ -n "$sel" ] || return 0
  kubectl -n "$NS" get pods -l "$sel" -o json 2>/dev/null
}

# What any router container is WAITING on, joined; empty when none is waiting on an image. Read for
# the same two reasons wait_pods reads them on the roles.
router_pull_reason() {
  router_pods | jq -r \
    '[.items[]? | .status.containerStatuses[]? | .state.waiting?.reason // empty] | join(" ")' 2>/dev/null
}

# The Ready router Pod's name, or empty when the router is not up. Read once when the verdict is
# formed, because by then the bound is spent and the router's state is settled.
router_pod() {
  router_pods | jq -r \
    '[.items[]? | select([.status.conditions // [] | .[] | select(.type=="Ready" and .status=="True")] | length > 0) | .metadata.name] | if length > 0 then .[0] else "" end' 2>/dev/null
}

# The endpoint is written once the ROUTER's OWN Deployment reports a ready replica — it is the
# router's status the operator copies, not a fact about the role Pods — so a single read here
# races that writer by design, and the wait is bounded because the router is a fourth workload no
# role poll above covers. Prints the endpoint and returns 0 on arrival, 2 when the router image
# would not pull (the same code wait_pods returns for a role image), 1 once the bound is spent.
wait_endpoint() {
  local deadline=$((SECONDS + READY_BOUND)) ep p
  while [ "$SECONDS" -lt "$deadline" ]; do
    ep="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "$MD" \
      -o jsonpath='{.status.endpoint}' 2>/dev/null)"
    if [ -n "$ep" ]; then
      printf '%s' "$ep"
      return 0
    fi
    for p in ImagePullBackOff ErrImagePull; do
      case "$(router_pull_reason)" in
        *"$p"*) return 2 ;;
      esac
    done
    sleep 5
  done
  return 1
}

ALL_READY=no
[ -n "$EN_POD" ] && [ -n "$PF_POD" ] && [ -n "$DC_POD" ] && ALL_READY=yes

ENDPOINT="$(wait_endpoint)"; ENDPOINT_RC=$?
ROUTER_POD="$(router_pod)"
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
    "the router Pod waits on $(router_pull_reason | tr -s ' '), so router image '${ROUTER_IMAGE}' would not pull and no front door could be published. Render the image and re-run"
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

# --- the rendered leg, both halves that carry it ----------------------------

# The document is the argument AFTER --ec-transfer-config, which is how the operator renders it:
# the flag has no environment equivalent, so it is always two argv entries. Reading the pair off
# the live argv rather than pattern-matching the JSON keeps the row a question about the flag.
ec_document() { # <pod>
  kubectl -n "$NS" get pod "$1" -o json 2>/dev/null | jq -r \
    --arg f "--ec-transfer-config" \
    '[(.spec.containers[] | select(.name=="main") | (.command // []) + (.args // []))][0] // []
     | (index($f) // -1) as $i | if $i < 0 then "" else .[$i + 1] end' 2>/dev/null
}

ec_env() { # <pod> <name>
  kubectl -n "$NS" get pod "$1" -o json 2>/dev/null | jq -r \
    --arg n "$2" '[(.spec.containers[] | select(.name=="main") | .env // [])[] | select(.name==$n)][0].value // ""' 2>/dev/null
}

# The downward-API half of the same read: the field selector the kubelet resolves, not the value it
# resolved. Rendered by valueFrom.fieldRef.fieldPath, so the variable has no .value to read.
ec_env_field_path() { # <pod> <name>
  kubectl -n "$NS" get pod "$1" -o json 2>/dev/null | jq -r \
    --arg n "$2" '[(.spec.containers[] | select(.name=="main") | .env // [])[] | select(.name==$n)][0].valueFrom.fieldRef.fieldPath // ""' 2>/dev/null
}

if [ -n "$EN_POD" ]; then
  DOC="$(ec_document "$EN_POD")"
  if [ -n "$DOC" ]; then
    if printf '%s' "$DOC" | jq -e '.ec_connector == "ECCPUConnector" and .ec_role == "ec_producer"' \
      >/dev/null 2>&1; then
      record PASS "encoder document" "ec_connector=ECCPUConnector ec_role=ec_producer on ${EN_POD}"
    else
      record FAIL "encoder document" "got: ${DOC}"
    fi
    # THE PRODUCER IS THE ONLY ROLE THAT BINDS THE NIXL SIDE CHANNEL, and a bind address of
    # localhost is worthless: the consumer is a different Pod. The host half is rendered through
    # the downward API, so it carries NO .value at all — what the render must show is the field
    # selector, because that is what makes the kubelet substitute the Pod's own address at
    # container start. Reading .value here would read a key the renderer never writes, and the row
    # would fail on every correct cluster.
    #
    # The port half is the opposite: a plain string the renderer owns outright, so .value is the
    # right key and the only one there is.
    SIDE_HOST_FIELD="$(ec_env_field_path "$EN_POD" VLLM_EC_SIDE_CHANNEL_HOST)"
    SIDE_PORT="$(ec_env "$EN_POD" VLLM_EC_SIDE_CHANNEL_PORT)"
    RUNNER="$(ec_env "$EN_POD" VLLM_USE_V2_MODEL_RUNNER)"
    POD_IP="$(kubectl -n "$NS" get pod "$EN_POD" -o jsonpath='{.status.podIP}' 2>/dev/null)"
    if [ "$SIDE_HOST_FIELD" = status.podIP ] && [ -n "$POD_IP" ] && [ -n "$SIDE_PORT" ]; then
      record PASS "encoder side channel" \
        "VLLM_EC_SIDE_CHANNEL_HOST is bound to ${SIDE_HOST_FIELD}, which the kubelet resolves to this Pod's own ${POD_IP}, and PORT=${SIDE_PORT}"
    else
      record FAIL "encoder side channel" \
        "host field='${SIDE_HOST_FIELD:-<none>}' port='${SIDE_PORT}' podIP='${POD_IP:-<none>}': a producer that binds anything but its own address is unreachable by the consumer, and a host rendered as a literal would be whatever that literal says"
    fi
    if [ "$RUNNER" = 1 ]; then
      record PASS "encoder model runner" "VLLM_USE_V2_MODEL_RUNNER=1, the connector's precondition"
    else
      record FAIL "encoder model runner" \
        "VLLM_USE_V2_MODEL_RUNNER='${RUNNER}': ECCPUConnector raises ValueError at construction without it"
    fi
  else
    record FAIL "encoder document" "no --ec-transfer-config argument on ${EN_POD}"
  fi
fi

if [ -n "$PF_POD" ]; then
  DOC="$(ec_document "$PF_POD")"
  if [ -n "$DOC" ]; then
    if printf '%s' "$DOC" | jq -e '.ec_connector == "ECCPUConnector" and .ec_role == "ec_consumer"' \
      >/dev/null 2>&1; then
      record PASS "prefiller document" "ec_connector=ECCPUConnector ec_role=ec_consumer on ${PF_POD}"
    else
      record FAIL "prefiller document" "got: ${DOC}"
    fi
    # A consumer that rendered the side channel would advertise an address it never binds.
    SIDE_PORT="$(ec_env "$PF_POD" VLLM_EC_SIDE_CHANNEL_PORT)"
    if [ -z "$SIDE_PORT" ]; then
      record PASS "prefiller binds no side channel" \
        "VLLM_EC_SIDE_CHANNEL_PORT is absent on ${PF_POD}, which is right: a consumer does not bind"
    else
      record FAIL "prefiller binds no side channel" "the consumer carries VLLM_EC_SIDE_CHANNEL_PORT=${SIDE_PORT}"
    fi
  else
    record FAIL "prefiller document" "no --ec-transfer-config argument on ${PF_POD}"
  fi
fi

# The sidecar flag lives on the decode Pod's own container and nowhere else: the engine-side name
# (ECCPUConnector) and the sidecar-side name (ec-nixl) are different vocabularies on purpose, and
# this row is where a cross-wiring between them would show.
if [ -n "$DC_POD" ]; then
  EC_FLAG="$(kubectl -n "$NS" get pod "$DC_POD" -o json 2>/dev/null | jq -r \
    '[(.spec.containers[] | select(.name=="routing-proxy") | .args // [])[]] | map(select(startswith("--ec-connector="))) | first // ""' 2>/dev/null)"
  if [ "$EC_FLAG" = "--ec-connector=ec-nixl" ]; then
    record PASS "decode sidecar connector" "routing-proxy on ${DC_POD} carries --ec-connector=ec-nixl"
  else
    record FAIL "decode sidecar connector" \
      "routing-proxy carries '${EC_FLAG:-<no --ec-connector flag>}', and the sidecar refuses to start on any other spelling"
  fi
fi

# --- the request: a multimodal turn through the router ----------------------

# The only row that can say the three roles spoke to each other. The request carries an image:
# a text-only turn never engages the encoder stage, so on a text-only model this row would pass
# with the leg entirely dormant.
if [ -z "$EN_POD" ] || [ -z "$PF_POD" ] || [ -z "$DC_POD" ]; then
  record SKIP "a multimodal request through the router returns 200" \
    "the three role Pods were not all Ready, so there is no chain to send a request through"
  print_rows
  [ "$FAILS" -eq 0 ] || { echo "[case-120] ${FAILS} check(s) FAILED"; exit 1; }
  echo "[case-120] all checks passed"
  exit 0
fi

kubectl -n "$NS" run "$PROBE_NAME" --image="$PROBE_IMAGE" --restart=Never \
  --labels="gpustack.ai/e2e-probe=${MD}" --command -- sleep 1800 >/dev/null 2>&1
if ! kubectl -n "$NS" wait "pod/$PROBE_NAME" --for=condition=Ready --timeout=300s >/dev/null 2>&1; then
  record SKIP "a multimodal request through the router returns 200" \
    "the probe Pod '${PROBE_NAME}' never became Ready on image '${PROBE_IMAGE}', so no request was sent"
  print_rows
  [ "$FAILS" -eq 0 ] || { echo "[case-120] ${FAILS} check(s) FAILED"; exit 1; }
  echo "[case-120] all checks passed"
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
  --data-binary "$PAYLOAD" "http://${MD}-router.${NS}.svc:8081/v1/chat/completions" 2>&1)" || RESPONSE=""

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
[ "$FAILS" -eq 0 ] || { echo "[case-120] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-120] all checks passed"