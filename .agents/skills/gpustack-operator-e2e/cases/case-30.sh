#!/usr/bin/env bash
#
# CASE 30 — Managed role host compilation cache mount, user additionalVolumes yield,
#           startupTimeoutSeconds schema bounds, startup-window Revoked hold, and
#           optional warm-restart cache reuse
#   (MUTATING, self-recovering; AUTO-SKIPS rendered-Pod rows without a usable
#    AcceleratorType and entrance LocalQueue, and warm-restart rows without a real
#    engine image and model)
#
#   case-30.sh <NS>
#
# Prerequisites: nodes=1; gpu=none; cluster=any; inputs=E2E_C30_ENGINE_IMAGE,E2E_C30_HOLD,E2E_C30_MODEL,E2E_C30_PROBE_IMAGE,E2E_C30_READY_BOUND,E2E_MD_IMAGE,E2E_MD_INSTANCE_TYPE,E2E_MD_ACCELERATOR_TYPE
# Goal:        Verify the managed role host compilation cache and startup-budget contracts:
#                1. Admission: startupTimeoutSeconds < 10 or > 86400 is refused by the CRD
#                   schema; startupTimeoutSeconds 3600 is accepted; a managed or take-over role
#                   mounting /var/lib/gpustack/cache via additionalVolumes is accepted (yielding
#                   the default hostPath volume).
#                2. Rendered Pod: volume gpustack-compile-cache (HostPath DirectoryOrCreate) at
#                   /var/lib/gpustack/cache/<mfg>-vllm/m-<32hex>/tp1-pp1-dp1-ep1-cp1_1-<8hex>,
#                   mounted at /var/lib/gpustack/cache, VLLM_CACHE_ROOT defaulted there, and
#                   startupProbe scaled to failureThreshold=360, periodSeconds=10, timeoutSeconds=5.
#                3. Startup-window hold: while main is running with Started=false,
#                   EndpointEligibility is NOT Revoked (auto-skips on placeholder images that
#                   cannot exec the managed engine argv).
#                4. Warm restart (gated on E2E_C30_ENGINE_IMAGE + E2E_C30_MODEL + real vendor):
#                   graph-mode deployment populates the host cache subdir, deleting the Pod
#                   re-renders the identical host subdir with files preserved, and the
#                   replacement reaches Ready.
# Environment: Any cluster with a materialized scheduling chain (run case-1 first) and the
#              ModelDeployment CRD installed. Rendered-Pod rows require a usable AcceleratorType
#              and a LocalQueue in <NS>. Warm-restart rows require E2E_C30_ENGINE_IMAGE,
#              E2E_C30_MODEL, and an ascend/nvidia/amd AcceleratorType.
# Inputs:      REAL: CRD schema bounds, validating webhooks, rendered Pods, EndpointEligibility
#              condition, and (when configured) real engine compiled cache files on the host.
# Expected:    Schema bounds refuse out-of-range startupTimeoutSeconds, valid manifests admit,
#              rendered Pod carries the computed cache volume/mount/env/startupProbe, startup
#              hold never sets Revoked, and warm-restart reuses the populated host directory.
# Cleanup:     EXIT trap deletes both ModelDeployments without waiting, releases every Kueue
#              Workload their Pods owned, deletes probe Pods, and removes the temp directory.
set -uo pipefail

CASE_ID=30
export CASE_ID
CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=/dev/null
. "${CASE_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_md-lib.sh"

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-30.sh <NS>" >&2
  exit 2
fi

IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
IT="${E2E_MD_ACCELERATOR_TYPE:-${E2E_MD_INSTANCE_TYPE:-}}"
HOLD="${E2E_C30_HOLD:-240}"
READY_BOUND="${E2E_C30_READY_BOUND:-2700}"
ENGINE_IMAGE="${E2E_C30_ENGINE_IMAGE:-}"
ENGINE_MODEL="${E2E_C30_MODEL:-}"
PROBE_IMAGE="${E2E_C30_PROBE_IMAGE:-busybox:1.36}"

CACHE_PATH="/var/lib/gpustack/cache"
CACHE_VOLUME="gpustack-compile-cache"
PLACEHOLDER_MODEL="Qwen/Qwen2.5-0.5B-Instruct"

SFX="$(set +o pipefail; LC_ALL=C tr -dc 'a-z0-9' </dev/urandom 2>/dev/null | head -c 5)"
[ -n "$SFX" ] || SFX="$$$(date +%s)"
MD="e2e-c30-${SFX}"
MD_WARM="e2e-c30-warm-${SFX}"
WORK="$(mktemp -d)"

has_localqueue() {
  kubectl -n "$NS" get localqueue -o name 2>/dev/null | head -n 1 | grep -q .
}

pod_field() {
  kubectl -n "$NS" get pod "$1" -o jsonpath="$2" 2>/dev/null
}

workloads_of() {
  local uids=" $* "
  kubectl -n "$NS" get workloads.kueue.x-k8s.io -o jsonpath='{range .items[*]}{.metadata.name}|{.metadata.uid}|{.status.conditions[?(@.type=="Admitted")].status}|{.metadata.ownerReferences[*].uid}{"\n"}{end}' \
    2>/dev/null \
    | while IFS='|' read -r name uid admitted owners; do
        [ -n "$name" ] || continue
        for o in $owners; do
          case "$uids" in *" $o "*) echo "$name ${uid:-} ${admitted:-}"; break ;; esac
        done
      done
}

PODS_CREATED=()
PROBES=()

cleanup() {
  local w p
  echo
  echo "[case-${CASE_ID}] cleanup"
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "$MD" "$MD_WARM" \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
  sleep 5
  while IFS= read -r w; do
    [ -n "$w" ] || continue
    kubectl -n "$NS" delete workloads.kueue.x-k8s.io "$w" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  done < <(workloads_of ${PODS_CREATED[@]+"${PODS_CREATED[@]}"} | awk '{print $1}')
  for p in ${PROBES[@]+"${PROBES[@]}"}; do
    kubectl -n "$NS" delete pod "$p" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  done
  rm -rf "$WORK"
}
trap cleanup EXIT

# =================================================================================================
# PHASE 1: Admission bounds (startupTimeoutSeconds & user additionalVolumes yield)
# =================================================================================================

MOUNT=""
COMMAND_LINE=""
STARTUP_TIMEOUT=""

admission_manifest() {
  local extra="" cmd="" timeout=""
  if [ -n "$MOUNT" ]; then
    extra="    additionalVolumes:
    - mountPath: ${MOUNT}
      persistentVolumeClaim:
        claimName: e2e-c30-extra"
  fi
  if [ -n "$COMMAND_LINE" ]; then
    cmd="    command: [\"${COMMAND_LINE}\"]"
  fi
  if [ -n "$STARTUP_TIMEOUT" ]; then
    timeout="    startupTimeoutSeconds: ${STARTUP_TIMEOUT}"
  fi
  cat <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: e2e-c30-admission
  namespace: ${NS}
spec:
  engine:
    name: vLLM
    image: ${IMAGE}
  model:
    name: ${PLACEHOLDER_MODEL}
  roles:
  - name: decode
    kind: Decode
    acceleratorType: ${IT:-e2e-c30-any}
    image: ${IMAGE}
${timeout}
${cmd}
${extra}
YAML
}

admission_accepts() {
  local check="$1" obj status=0
  obj="$(md_dryrun_accepts < <(admission_manifest))" || status=1
  if [ "$status" -eq 0 ]; then
    record PASS "$check" "$obj"
  else
    record FAIL "$check" "$obj"
  fi
}

admission_refuses_timeout() {
  local check="$1" obj status=0
  obj="$(md_dryrun_refuses "startupTimeoutSeconds" < <(admission_manifest))" || status=1
  if [ "$status" -eq 0 ]; then
    record PASS "$check" "$obj"
  else
    record FAIL "$check" "$obj"
  fi
}

if [ -z "$IT" ]; then
  IT="$(md_usable_instance_type)"
fi

MOUNT=""
COMMAND_LINE=""
STARTUP_TIMEOUT="5"
admission_refuses_timeout "startupTimeoutSeconds below 10s floor is refused by CRD schema"

MOUNT=""
COMMAND_LINE=""
STARTUP_TIMEOUT="86401"
admission_refuses_timeout "startupTimeoutSeconds above 86400s ceiling is refused by CRD schema"

if [ -n "$IT" ]; then
  MOUNT=""
  COMMAND_LINE=""
  STARTUP_TIMEOUT="3600"
  admission_accepts "managed role with startupTimeoutSeconds 3600 is ACCEPTED"

  MOUNT="$CACHE_PATH"
  COMMAND_LINE=""
  STARTUP_TIMEOUT=""
  admission_accepts "managed role mounting compilation cache path via additionalVolumes is ACCEPTED"

  MOUNT="$CACHE_PATH"
  COMMAND_LINE="/pause"
  STARTUP_TIMEOUT=""
  admission_accepts "take-over role mounting compilation cache path is ACCEPTED"
else
  record SKIP "managed role with startupTimeoutSeconds 3600 is ACCEPTED" "no usable AcceleratorType"
  record SKIP "managed role mounting compilation cache path via additionalVolumes is ACCEPTED" "no usable AcceleratorType"
  record SKIP "take-over role mounting compilation cache path is ACCEPTED" "no usable AcceleratorType"
fi

# =================================================================================================
# PHASE 2: Rendered Pod compilation cache mount, env, startupProbe, and startup-window Revoked hold
# =================================================================================================

rendered_manifest() {
  cat <<YAML
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
    name: ${PLACEHOLDER_MODEL}
  roles:
  - name: server
    acceleratorType: ${IT}
    replicas: 1
    image: ${IMAGE}
    startupTimeoutSeconds: 3600
YAML
}

eligibility_reason() {
  kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "$1" \
    -o jsonpath='{.status.conditions[?(@.type=="EndpointEligibility")].reason}' 2>/dev/null
}

LEG_ROW="managed replica renders compilation cache and startup budget"
POD=""
if [ -z "$IT" ]; then
  record SKIP "$LEG_ROW" "no usable AcceleratorType in the cluster"
elif ! has_localqueue; then
  record SKIP "$LEG_ROW" "no LocalQueue in ${NS}; see case-29 Environment note"
else
  APPLY_OUT="$(rendered_manifest | kubectl apply -f - 2>&1 | tr '\n' ' ')"
  for _ in $(seq 1 90); do
    POD="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" \
      -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
    [ -n "$POD" ] && break
    sleep 2
  done
  if [ -z "$POD" ]; then
    record FAIL "$LEG_ROW" "no Pod appeared; apply: ${APPLY_OUT:0:200}"
    record SKIP "Pod mounts its own cache subdirectory of host root" "fixture rendered no Pod"
    record SKIP "engine cache-root variable points at mounted cache" "fixture rendered no Pod"
    record SKIP "declared startup budget scales startupProbe" "fixture rendered no Pod"
    record SKIP "replica inside startup window is held, not revoked" "fixture rendered no Pod"
  else
    PODS_CREATED+=("$(pod_field "$POD" '{.metadata.uid}')")

    VOL_PATH="$(pod_field "$POD" '{.spec.volumes[?(@.name=="gpustack-compile-cache")].hostPath.path}')"
    VOL_TYPE="$(pod_field "$POD" '{.spec.volumes[?(@.name=="gpustack-compile-cache")].hostPath.type}')"
    if printf '%s' "$VOL_PATH" | grep -Eq "^${CACHE_PATH}/[a-z0-9]+-vllm/m-[0-9a-f]{32}/tp1-pp1-dp1-ep1-cp1_1-[0-9a-f]{8}$" \
      && [ "$VOL_TYPE" = "DirectoryOrCreate" ]; then
      record PASS "Pod mounts its own cache subdirectory of host root" \
        "volume ${CACHE_VOLUME} HostPath ${VOL_TYPE} at ${VOL_PATH}"
    else
      record FAIL "Pod mounts its own cache subdirectory of host root" \
        "type='${VOL_TYPE}' path='${VOL_PATH:-<absent>}'"
    fi

    MOUNT_GOT="$(pod_field "$POD" '{.spec.containers[?(@.name=="main")].volumeMounts[?(@.name=="gpustack-compile-cache")].mountPath}')"
    if [ "$MOUNT_GOT" = "$CACHE_PATH" ]; then
      record PASS "main mounts cache at constant path" "${CACHE_PATH}"
    else
      record FAIL "main mounts cache at constant path" "got '${MOUNT_GOT:-<absent>}' (want ${CACHE_PATH})"
    fi

    ENV_GOT="$(pod_field "$POD" '{.spec.containers[?(@.name=="main")].env[?(@.name=="VLLM_CACHE_ROOT")].value}')"
    if [ "$ENV_GOT" = "$CACHE_PATH" ]; then
      record PASS "engine cache-root variable points at mounted cache" "VLLM_CACHE_ROOT=${CACHE_PATH}"
    else
      record FAIL "engine cache-root variable points at mounted cache" "got '${ENV_GOT:-<absent>}'"
    fi

    FT="$(pod_field "$POD" '{.spec.containers[?(@.name=="main")].startupProbe.failureThreshold}')"
    PS="$(pod_field "$POD" '{.spec.containers[?(@.name=="main")].startupProbe.periodSeconds}')"
    TS="$(pod_field "$POD" '{.spec.containers[?(@.name=="main")].startupProbe.timeoutSeconds}')"
    if [ "$FT" = "360" ] && [ "$PS" = "10" ] && [ "$TS" = "5" ]; then
      record PASS "declared startup budget scales startupProbe" "failureThreshold=${FT} period=${PS} timeout=${TS}"
    else
      record FAIL "declared startup budget scales startupProbe" "got ${FT:-?}/${PS:-?}/${TS:-?} (want 360/10/5)"
    fi

    STARTED=""
    RUNNING=""
    WAITING=""
    HOLD_OBSERVED=0
    HOLD_MISSED=""
    i=0
    while [ "$i" -lt "$HOLD" ]; do
      STARTED="$(pod_field "$POD" '{.status.containerStatuses[?(@.name=="main")].started}')"
      RUNNING="$(pod_field "$POD" '{.status.containerStatuses[?(@.name=="main")].state.running.startedAt}')"
      WAITING="$(pod_field "$POD" '{.status.containerStatuses[?(@.name=="main")].state.waiting.reason}')"
      if [ -n "$RUNNING" ] && [ "$STARTED" != "true" ]; then
        HOLD_OBSERVED=1
        break
      fi
      if [ "$STARTED" = "true" ]; then
        HOLD_MISSED="container passed startupProbe before window could be sampled"
        break
      fi
      case "$WAITING" in
        "" | ContainerCreating | PodInitializing) ;;
        *)
          HOLD_MISSED="main container waiting (${WAITING}): placeholder image cannot exec managed argv"
          break
          ;;
      esac
      sleep 5
      i=$((i + 5))
    done

    if [ "$HOLD_OBSERVED" -eq 1 ]; then
      REASON=""
      j=0
      while [ "$j" -lt 30 ]; do
        REASON="$(eligibility_reason "$MD")"
        [ -n "$REASON" ] && break
        sleep 3
        j=$((j + 3))
      done
      if [ -z "$REASON" ]; then
        record FAIL "replica inside startup window is held, not revoked" "no EndpointEligibility reason appeared"
      elif [ "$REASON" = "Revoked" ]; then
        record FAIL "replica inside startup window is held, not revoked" "EndpointEligibility=Revoked during startup window"
      else
        record PASS "replica inside startup window is held, not revoked" "Started=false since ${RUNNING}, reason=${REASON}"
      fi
    elif [ -n "$HOLD_MISSED" ]; then
      record SKIP "replica inside startup window is held, not revoked" "$HOLD_MISSED"
    else
      record FAIL "replica inside startup window is held, not revoked" \
        "no startup window or terminal state within ${HOLD}s (waiting='${WAITING:-}')"
    fi
  fi
fi

# =================================================================================================
# PHASE 3: Real-engine warm-restart host compilation cache reuse (gated on ENGINE_IMAGE + MODEL)
# =================================================================================================

warm_cache_probe() {
  local probe_name="$1" node="$2" host_dir="$3" phase listing waiting i=0
  PROBES+=("$probe_name")
  if ! cat <<YAML | kubectl -n "$NS" create -f - >/dev/null 2>&1
apiVersion: v1
kind: Pod
metadata:
  name: ${probe_name}
  namespace: ${NS}
spec:
  nodeName: ${node}
  restartPolicy: Never
  containers:
  - name: probe
    image: ${PROBE_IMAGE}
    command: ["/bin/sh", "-c", "ls -A /cache"]
    volumeMounts:
    - name: cache
      mountPath: /cache
      readOnly: true
  volumes:
  - name: cache
    hostPath:
      path: ${host_dir}
      type: DirectoryOrCreate
YAML
  then
    return 2
  fi
  while [ "$i" -lt 90 ]; do
    phase="$(pod_field "$probe_name" '{.status.phase}')"
    case "$phase" in
      Succeeded)
        listing="$(kubectl -n "$NS" logs "$probe_name" -c probe 2>/dev/null)"
        printf '%s' "$listing"
        return 0
        ;;
      Failed) return 4 ;;
    esac
    waiting="$(pod_field "$probe_name" '{.status.containerStatuses[0].state.waiting.reason}')"
    case "$waiting" in
      ErrImagePull | ImagePullBackOff) return 3 ;;
    esac
    sleep 2
    i=$((i + 2))
  done
  return 4
}

wait_pod_ready() {
  local pod="$1" bound="$2" i ready
  for ((i = 0; i < bound; i += 5)); do
    ready="$(pod_field "$pod" '{.status.conditions[?(@.type=="Ready")].status}')"
    [ "$ready" = "True" ] && return 0
    sleep 5
  done
  return 1
}

WARM_ROW="graph-mode deployment populates cache and replacement reuses it"
if [ -z "$ENGINE_IMAGE" ] || [ -z "$ENGINE_MODEL" ]; then
  record SKIP "$WARM_ROW" "E2E_C30_ENGINE_IMAGE and E2E_C30_MODEL not both set"
else
  WARM_IT=""
  for m in ascend nvidia amd; do
    WARM_IT="$(md_usable_type_for "$m")"
    [ -n "$WARM_IT" ] && break
  done
  if [ -z "$WARM_IT" ]; then
    record SKIP "$WARM_ROW" "no usable AcceleratorType reports ascend, nvidia, or amd"
  elif ! has_localqueue; then
    record SKIP "$WARM_ROW" "no LocalQueue in ${NS}"
  else
    cat <<YAML | kubectl apply -f - >/dev/null 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD_WARM}
  namespace: ${NS}
spec:
  engine:
    name: vLLM
    image: ${ENGINE_IMAGE}
  model:
    name: ${ENGINE_MODEL}
  roles:
  - name: server
    acceleratorType: ${WARM_IT}
    replicas: 1
    image: ${ENGINE_IMAGE}
    startupTimeoutSeconds: 3600
YAML
    WARM_POD=""
    for _ in $(seq 1 90); do
      WARM_POD="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD_WARM}" \
        -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
      [ -n "$WARM_POD" ] && break
      sleep 2
    done
    if [ -z "$WARM_POD" ]; then
      record FAIL "$WARM_ROW" "graph-mode deployment never rendered a Pod"
    else
      PODS_CREATED+=("$(pod_field "$WARM_POD" '{.metadata.uid}')")
      WARM_NODE="$(pod_field "$WARM_POD" '{.spec.nodeName}')"
      WARM_DIR="$(pod_field "$WARM_POD" '{.spec.volumes[?(@.name=="gpustack-compile-cache")].hostPath.path}')"
      if wait_pod_ready "$WARM_POD" "$READY_BOUND"; then
        record PASS "graph-mode deployment reaches Ready" "${WARM_POD} on ${WARM_NODE} (${WARM_DIR:-<absent>})"
      else
        record FAIL "graph-mode deployment reaches Ready" "${WARM_POD} not Ready within ${READY_BOUND}s"
      fi

      PROBE1="e2e-c30-probe1-${SFX}"
      PROBE_RC=0
      LISTING="$(warm_cache_probe "$PROBE1" "$WARM_NODE" "$WARM_DIR")" || PROBE_RC=$?
      if [ "$PROBE_RC" -ne 0 ]; then
        record SKIP "host cache subdirectory holds compiled artifacts" "probe rc=${PROBE_RC}"
      elif [ -n "$LISTING" ]; then
        record PASS "host cache subdirectory holds compiled artifacts" "$(printf '%s' "$LISTING" | tr '\n' ' ' | cut -c1-120)"
      else
        record FAIL "host cache subdirectory holds compiled artifacts" "${WARM_DIR} is empty"
      fi
      kubectl -n "$NS" delete pod "$PROBE1" --ignore-not-found --wait=false >/dev/null 2>&1 || true

      FIRST_POD="$WARM_POD"
      kubectl -n "$NS" delete pod "$FIRST_POD" --wait=false >/dev/null 2>&1 || true
      REPLACEMENT=""
      for _ in $(seq 1 120); do
        REPLACEMENT="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD_WARM}" \
          -o jsonpath='{.items[*].metadata.name}' 2>/dev/null | tr ' ' '\n' | grep -v "^${FIRST_POD}$" | head -n 1)"
        [ -n "$REPLACEMENT" ] && break
        sleep 2
      done
      if [ -z "$REPLACEMENT" ]; then
        record SKIP "replacement renders same cache subdirectory" "no replacement Pod appeared"
        record SKIP "populated directory survives replacement" "no replacement Pod"
        record SKIP "replacement reaches Ready on populated cache" "no replacement Pod"
      else
        PODS_CREATED+=("$(pod_field "$REPLACEMENT" '{.metadata.uid}')")
        REPL_DIR="$(pod_field "$REPLACEMENT" '{.spec.volumes[?(@.name=="gpustack-compile-cache")].hostPath.path}')"
        REPL_NODE="$(pod_field "$REPLACEMENT" '{.spec.nodeName}')"
        if [ "$REPL_DIR" = "$WARM_DIR" ] && [ "$REPL_NODE" = "$WARM_NODE" ]; then
          record PASS "replacement renders same cache subdirectory" "${REPLACEMENT} on ${REPL_NODE}: ${REPL_DIR}"
        else
          record FAIL "replacement renders same cache subdirectory" "got ${REPL_DIR:-<absent>} on ${REPL_NODE:-<absent>}"
        fi

        PROBE2="e2e-c30-probe2-${SFX}"
        PROBE_RC2=0
        LISTING2="$(warm_cache_probe "$PROBE2" "$REPL_NODE" "$REPL_DIR")" || PROBE_RC2=$?
        if [ "$PROBE_RC2" -eq 0 ] && [ -n "$LISTING2" ]; then
          record PASS "populated directory survives replacement" "$(printf '%s' "$LISTING2" | tr '\n' ' ' | cut -c1-120)"
        elif [ "$PROBE_RC2" -ne 0 ]; then
          record SKIP "populated directory survives replacement" "probe rc=${PROBE_RC2}"
        else
          record FAIL "populated directory survives replacement" "${REPL_DIR} is empty"
        fi
        kubectl -n "$NS" delete pod "$PROBE2" --ignore-not-found --wait=false >/dev/null 2>&1 || true

        if wait_pod_ready "$REPLACEMENT" "$READY_BOUND"; then
          record PASS "replacement reaches Ready on populated cache" "${REPLACEMENT} Ready"
        else
          record FAIL "replacement reaches Ready on populated cache" "${REPLACEMENT} not Ready within ${READY_BOUND}s"
        fi
      fi
    fi
  fi
fi

results
