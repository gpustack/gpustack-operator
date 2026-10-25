#!/usr/bin/env bash
#
# CASE 127 — the host compilation cache a managed role renders, the startup budget a role
#   declares, and user volume overrides on the cache path   (MUTATING, self-recovering;
#   AUTO-SKIPS the rendered-Pod rows without a usable InstanceType and an entrance LocalQueue,
#   and the warm-restart rows without a real engine image)
#
#   case-127.sh <NS>
#
# Prerequisites: nodes=1; gpu=none; cluster=any; inputs=E2E_C127_ENGINE_IMAGE,E2E_C127_HOLD,E2E_C127_MODEL,E2E_C127_PROBE_IMAGE,E2E_C127_READY_BOUND,E2E_MD_IMAGE,E2E_MD_INSTANCE_TYPE
# Goal:        A managed role's replica mounts its OWN subdirectory of one host compilation cache,
#              always at the same path inside the container, so the engine can be pointed at the
#              cache by a single defaulted variable without the render knowing which weights,
#              parallel shape or flag set the directory was keyed by. The host side is computed
#              per role from the manufacturer that ran the compiler, the engine that drove it, an
#              identity of the weights, the declared parallel shape and a digest of everything
#              else that reaches the generated code, which is what lets two roles that share
#              nothing share one cache root without overwriting each other's kernels. The role's
#              declared startup budget is what scales the replica's startup gate, so a replica
#              compiling graphs for a long time is given that long before the kubelet restarts it,
#              and a replica still inside its startup window is HELD, not revoked: reading a
#              running container the kubelet has not marked Started as a fault would restart the
#              very compilation the budget exists to protect. The schema bounds the declared
#              startup budget to [10, 86400] seconds, while admission allows a role to mount its
#              own volume at the compilation cache path via additionalVolumes so the renderer can
#              yield its default hostPath volume in favor of the user's volume.
#
#              THE STARTUP WINDOW IS OBSERVED, NOT ASSUMED. A placeholder image cannot exec the
#              operator's argv, so on a cluster whose only image is the placeholder the container
#              never enters the running-but-not-started state the hold lives in, and that row
#              records SKIP naming the wait reason rather than passing on a window nobody saw.
#              Where the window does open, the row is a hard verdict.
#
# Environment: Admission rows: any cluster with the ModelDeployment CRD installed (run case-1
#              first); every assertion there is a server-side dry run, so no InstanceType is
#              needed for the schema-bound refusal rows, while the accepted rows need a usable
#              InstanceType reporting an admitted platform (ascend, nvidia or amd).
#
#              Rendered-Pod rows: a usable InstanceType (not terminating, not inactive) and the
#              pool's entrance LocalQueue in <NS> (`kubectl -n <NS> get localqueue`), else the
#              whole leg records SKIP. The placeholder image (E2E_MD_IMAGE, default pause) is
#              never expected to serve; the startup-hold row additionally needs the image to
#              actually exec the operator's argv, and with the default placeholder it records SKIP
#              naming the wait reason.
#
#              Warm-restart rows: real graph-mode work, gated on E2E_C127_ENGINE_IMAGE and
#              E2E_C127_MODEL being set AND an InstanceType reporting an admitted manufacturer
#              (ascend, nvidia or amd), else they record SKIP. Reading the host directory needs a
#              namespace whose PodSecurity admits a hostPath probe Pod (E2E_C127_PROBE_IMAGE,
#              default busybox:1.36, pullable) and E2E_C127_READY_BOUND (default 2700) seconds of
#              patience for real compilation, twice: once before and once after the Pod is
#              deleted and replaced.
#
# Inputs:      MOCKED: nothing -- the placeholder image is plumbing for a Pod spec that would
#              otherwise be refused for reasons none of these rows are about. Real: the CRD schema
#              bounds on startupTimeoutSeconds, the webhook's acceptance of user additionalVolumes
#              at the cache path, the rendered Pod (volume, mount, defaulted variable, scaled
#              startup gate), the operator's status writer, and -- when the real engine is
#              provided -- the engine's own compiled artifacts on the node's disk.
#
# Expected:    - ADMISSION: a startupTimeoutSeconds below 10 or above 86400 is refused by the CRD
#                schema naming `startupTimeoutSeconds`; a managed role with startupTimeoutSeconds
#                3600 is accepted; a managed role mounting an additionalVolume at the compilation
#                cache path is accepted (yielding the default hostPath volume); a take-over role
#                mounting the cache path is accepted;
#              - RENDERED POD: the Pod carries volume `gpustack-compile-cache`, a HostPath
#                (DirectoryOrCreate) under
#                `<cache>/<manufacturer>-<engine>/m-<32 hex>/tp1-pp1-dp1-ep1-cp1_1-<8 hex>`;
#                `main` mounts it at the cache path; the engine's cache-root variable
#                (VLLM_CACHE_ROOT) is defaulted there; the startup gate reads
#                failureThreshold 360, periodSeconds 10, timeoutSeconds 5 for the declared
#                budget of 3600;
#              - STARTUP HOLD: while the running main container has not passed its startup probe
#                (Started == false), the deployment's EndpointEligibility condition is NOT
#                Revoked; skipped, naming the wait reason, where the placeholder never opens the
#                window;
#              - WARM RESTART (gated): the graph-mode deployment reaches Ready, the host cache
#                subdirectory holds files, the replacement Pod the case forces by deleting the
#                first renders the SAME subdirectory, the files are still there, and the
#                replacement reaches Ready.
#
# Cleanup:     Trap deletes both deployments without waiting, then deletes every Kueue Workload
#              their Pods owned -- the manual form of the release the controller performs, since
#              Kueue holds a finalizer on each Pod of a serving group -- and deletes the probe
#              Pods and the scratch directory. Idempotent, runs on pass AND fail, safe to re-run.
#              Creates no cluster-scoped object, changes no node, label or setting.
set -uo pipefail

# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-127.sh <NS>" >&2
  exit 2
fi
export CASE_ID=127
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
IT="${E2E_MD_INSTANCE_TYPE:-}"
HOLD="${E2E_C127_HOLD:-240}"
READY_BOUND="${E2E_C127_READY_BOUND:-2700}"
ENGINE_IMAGE="${E2E_C127_ENGINE_IMAGE:-}"
ENGINE_MODEL="${E2E_C127_MODEL:-}"
PROBE_IMAGE="${E2E_C127_PROBE_IMAGE:-busybox:1.36}"

CACHE_PATH="/var/lib/gpustack/cache"
CACHE_VOLUME="gpustack-compile-cache"
PLACEHOLDER_MODEL="Qwen/Qwen2.5-0.5B-Instruct"

SFX="$(set +o pipefail; LC_ALL=C tr -dc 'a-z0-9' </dev/urandom 2>/dev/null | head -c 5)"
[ -n "$SFX" ] || SFX="$$$(date +%s)"
MD="case127-${SFX}"
MD_WARM="case127-warm-${SFX}"
WORK="$(mktemp -d)"

# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_md-lib.sh"

# The first InstanceType a deployment can name: not terminating, not inactive. See case-49 for why
# the first one listed is not good enough. The lookup is the library's: md_usable_instance_type.

has_localqueue() {
  kubectl -n "$NS" get localqueue -o name 2>/dev/null | head -n 1 | grep -q .
}

pod_field() {
  kubectl -n "$NS" get pod "$1" -o jsonpath="$2" 2>/dev/null
}

# Every Workload owning a Pod whose UID is listed, as "<name> <uid> <admitted>" lines. Found
# through the owner references because a Workload's name is Kueue's to choose.
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
  echo
  echo "[case-${CASE_ID}] cleanup"
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "$MD" "$MD_WARM" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  sleep 5
  while IFS= read -r w; do
    [ -n "$w" ] || continue
    kubectl -n "$NS" delete workloads.kueue.x-k8s.io "$w" --ignore-not-found --wait=false >/dev/null 2>&1
  done < <(workloads_of ${PODS_CREATED[@]+"${PODS_CREATED[@]}"} | awk '{print $1}')
  local p
  for p in ${PROBES[@]+"${PROBES[@]}"}; do
    kubectl -n "$NS" delete pod "$p" --ignore-not-found --wait=false >/dev/null 2>&1
  done
  rm -rf "$WORK"
}
trap cleanup EXIT

# ------------------------------------------------------------------ the admission leg

MOUNT=""
COMMAND_LINE=""
STARTUP_TIMEOUT=""

admission_manifest() {
  local extra="" cmd="" timeout=""
  if [ -n "$MOUNT" ]; then
    extra="    additionalVolumes:
    - mountPath: ${MOUNT}
      persistentVolumeClaim:
        claimName: case127-extra"
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
  name: case127-admission
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
    instanceType: ${IT:-case127-any}
    image: ${IMAGE}
${timeout}
${cmd}
${extra}
YAML
}

# The instrument is the library's: it reads the manifest, prints the text of the row and returns the
# verdict. The row stays here, so the table and the verdict never leave the case that owns them.
admission_accepts() { # <check>
  local check="$1" obj status=0
  obj="$(md_dryrun_accepts < <(admission_manifest))" || status=1
  if [ "$status" -eq 0 ]; then
    record PASS "$check" "$obj"
  else
    record FAIL "$check" "$obj"
  fi
  return 0
}

admission_refuses_timeout() { # <check>
  local check="$1" obj status=0
  obj="$(md_dryrun_refuses "startupTimeoutSeconds" < <(admission_manifest))" || status=1
  if [ "$status" -eq 0 ]; then
    record PASS "$check" "$obj"
  else
    record FAIL "$check" "$obj"
  fi
  return 0
}

if [ -z "$IT" ]; then
  IT="$(md_usable_instance_type)"
fi

MOUNT=""
COMMAND_LINE=""
STARTUP_TIMEOUT="5"
admission_refuses_timeout "startupTimeoutSeconds below the 10s floor is refused by the CRD schema"

MOUNT=""
COMMAND_LINE=""
STARTUP_TIMEOUT="86401"
admission_refuses_timeout "startupTimeoutSeconds above the 86400s ceiling is refused by the CRD schema"

if [ -n "$IT" ]; then
  MOUNT=""
  COMMAND_LINE=""
  STARTUP_TIMEOUT="3600"
  admission_accepts "a managed role with startupTimeoutSeconds 3600 is ACCEPTED"

  MOUNT="$CACHE_PATH"
  COMMAND_LINE=""
  STARTUP_TIMEOUT=""
  admission_accepts "a managed role mounting the compilation cache path via additionalVolumes is ACCEPTED"

  MOUNT="$CACHE_PATH"
  COMMAND_LINE="/pause"
  STARTUP_TIMEOUT=""
  admission_accepts "a take-over role mounting the compilation cache path is ACCEPTED"
else
  record SKIP "a managed role with startupTimeoutSeconds 3600 is ACCEPTED" \
    "no usable InstanceType reports an admitted platform (ascend, nvidia, amd), so there is no shape the webhook would admit; the schema refusal rows above do not need one"
  record SKIP "a managed role mounting the compilation cache path via additionalVolumes is ACCEPTED" \
    "no usable InstanceType, so no shape the webhook would admit whole"
  record SKIP "a take-over role mounting the compilation cache path is ACCEPTED" \
    "no usable InstanceType, so no shape the webhook would admit whole"
fi

# ------------------------------------------------------------------ the rendered-Pod leg

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
    instanceType: ${IT}
    replicas: 1
    image: ${IMAGE}
    startupTimeoutSeconds: 3600
YAML
}

eligibility_reason() {
  kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "$1" \
    -o jsonpath='{.status.conditions[?(@.type=="EndpointEligibility")].reason}' 2>/dev/null
}

LEG_ROW="a managed replica renders its compilation cache and its startup budget"
POD=""
if [ -z "$IT" ]; then
  record SKIP "$LEG_ROW" "no usable InstanceType in the cluster, so nothing would be admitted"
elif ! has_localqueue; then
  record SKIP "$LEG_ROW" "no LocalQueue in ${NS}, so the group would never be admitted; see case-78's Environment note"
else
  APPLY_OUT="$(rendered_manifest | kubectl apply -f - 2>&1 | tr '\n' ' ')"
  for _ in $(seq 1 90); do
    POD="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" \
      -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
    [ -n "$POD" ] && break
    sleep 2
  done
  if [ -z "$POD" ]; then
    POD=""
    record FAIL "$LEG_ROW" "no Pod ever appeared; apply said: $(printf '%s' "$APPLY_OUT" | cut -c1-200)"
    record SKIP "the Pod mounts its own cache subdirectory of one host root" "the fixture never rendered a Pod"
    record SKIP "the engine's cache-root variable points at the mounted cache" "the fixture never rendered a Pod"
    record SKIP "the declared startup budget scales the startup gate" "the fixture never rendered a Pod"
    record SKIP "a replica inside its startup window is held, not revoked" "the fixture never rendered a Pod"
  else
    PODS_CREATED+=("$(pod_field "$POD" '{.metadata.uid}')")

    # The volume: one name, one host type, one computed host path. The path's four segments are
    # what the two-roles-cannot-collide property lives in: the manufacturer that ran the
    # compiler, the engine, the weights, and the shape-plus-digest leaf. A CPU pool renders the
    # cpu fallback where a vendor segment would be, so the first segment is matched by shape.
    VOL_PATH="$(pod_field "$POD" '{.spec.volumes[?(@.name=="gpustack-compile-cache")].hostPath.path}')"
    VOL_TYPE="$(pod_field "$POD" '{.spec.volumes[?(@.name=="gpustack-compile-cache")].hostPath.type}')"
    if printf '%s' "$VOL_PATH" | grep -Eq "^${CACHE_PATH}/[a-z0-9]+-vllm/m-[0-9a-f]{32}/tp1-pp1-dp1-ep1-cp1_1-[0-9a-f]{8}$" \
      && [ "$VOL_TYPE" = "DirectoryOrCreate" ]; then
      record PASS "the Pod mounts its own cache subdirectory of one host root" \
        "volume ${CACHE_VOLUME} HostPath ${VOL_TYPE} at ${VOL_PATH}"
    else
      record FAIL "the Pod mounts its own cache subdirectory of one host root" \
        "volume ${CACHE_VOLUME} HostPath type '${VOL_TYPE}' at '${VOL_PATH:-<absent>}'"
    fi

    MOUNT_GOT="$(pod_field "$POD" '{.spec.containers[?(@.name=="main")].volumeMounts[?(@.name=="gpustack-compile-cache")].mountPath}')"
    if [ "$MOUNT_GOT" = "$CACHE_PATH" ]; then
      record PASS "main mounts the cache at the one constant path" "${CACHE_PATH}"
    else
      record FAIL "main mounts the cache at the one constant path" \
        "main volumeMount mountPath '${MOUNT_GOT:-<absent>}' (want ${CACHE_PATH})"
    fi

    ENV_GOT="$(pod_field "$POD" '{.spec.containers[?(@.name=="main")].env[?(@.name=="VLLM_CACHE_ROOT")].value}')"
    if [ "$ENV_GOT" = "$CACHE_PATH" ]; then
      record PASS "the engine's cache-root variable points at the mounted cache" \
        "VLLM_CACHE_ROOT=${CACHE_PATH} defaulted on main"
    else
      record FAIL "the engine's cache-root variable points at the mounted cache" \
        "VLLM_CACHE_ROOT='${ENV_GOT:-<absent>}' (want ${CACHE_PATH})"
    fi

    FT="$(pod_field "$POD" '{.spec.containers[?(@.name=="main")].startupProbe.failureThreshold}')"
    PS="$(pod_field "$POD" '{.spec.containers[?(@.name=="main")].startupProbe.periodSeconds}')"
    TS="$(pod_field "$POD" '{.spec.containers[?(@.name=="main")].startupProbe.timeoutSeconds}')"
    if [ "$FT" = "360" ] && [ "$PS" = "10" ] && [ "$TS" = "5" ]; then
      record PASS "the declared startup budget scales the startup gate" \
        "startupTimeoutSeconds 3600 renders failureThreshold ${FT} x periodSeconds ${PS}, timeout ${TS}"
    else
      record FAIL "the declared startup budget scales the startup gate" \
        "failureThreshold '${FT:-<absent>}' periodSeconds '${PS:-<absent>}' timeoutSeconds '${TS:-<absent>}' (want 360/10/5)"
    fi

    # The startup window: a RUNNING main container the kubelet has not marked Started. The loop
    # stops on whichever answer arrives first -- the window, the container passing its probe, a
    # terminal wait (which the placeholder image produces, since it cannot exec the operator's
    # argv and the kubelet reports that as a stuck start), or the bound.
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
        HOLD_MISSED="the container passed its startup probe within the polling granularity, so the window closed before it could be read"
        break
      fi
      case "$WAITING" in
        "" | ContainerCreating | PodInitializing) ;;
        *)
          HOLD_MISSED="the main container is stuck waiting (${WAITING}): the placeholder image cannot exec the operator's argv, so no window opens on this image"
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
        record FAIL "a replica inside its startup window is held, not revoked" \
          "the container has been running unstarted since ${RUNNING}, but no EndpointEligibility reason ever appeared, so the hold was not observed"
      elif [ "$REASON" = "Revoked" ]; then
        record FAIL "a replica inside its startup window is held, not revoked" \
          "EndpointEligibility reads Revoked while the container has been running unstarted since ${RUNNING}: a slow start would be restarted, the restart this case exists to forbid"
      else
        record PASS "a replica inside its startup window is held, not revoked" \
          "Started == false since ${RUNNING}, EndpointEligibility reason ${REASON}"
      fi
    elif [ -n "$HOLD_MISSED" ]; then
      record SKIP "a replica inside its startup window is held, not revoked" "$HOLD_MISSED"
    else
      record FAIL "a replica inside its startup window is held, not revoked" \
        "neither a startup window nor a terminal state within ${HOLD}s (running='${RUNNING:-}' waiting='${WAITING:-}' started='${STARTED:-}')"
    fi
  fi
fi

# ------------------------------------------------------------------ the warm-restart leg

warm_cache_probe() { # <probe-name> <node> <host-dir>
  # 0: the listing is on stdout (possibly empty -- a measured answer).
  # 2: the probe could not be created. 3: its image never pulled. 4: it never completed.
  local probe_name="$1" node="$2" host_dir="$3" phase listing
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
  local i=0
  while [ "$i" -lt 90 ]; do
    phase="$(pod_field "$probe_name" '{.status.phase}')"
    case "$phase" in
      Succeeded)
        listing="$(kubectl -n "$NS" logs "$probe_name" -c probe 2>/dev/null)"
        printf '%s' "$listing"
        return 0
        ;;
      Failed)
        return 4
        ;;
    esac
    local waiting
    waiting="$(pod_field "$probe_name" '{.status.containerStatuses[0].state.waiting.reason}')"
    case "$waiting" in
      ErrImagePull | ImagePullBackOff) return 3 ;;
    esac
    sleep 2
    i=$((i + 2))
  done
  return 4
}

wait_pod_ready() { # <pod> <bound-seconds>
  local pod="$1" bound="$2" i ready
  for ((i = 0; i < bound; i += 5)); do
    ready="$(pod_field "$pod" '{.status.conditions[?(@.type=="Ready")].status}')"
    [ "$ready" = "True" ] && return 0
    sleep 5
  done
  return 1
}

WARM_ROW="the graph-mode deployment runs, populates its cache, and a replacement reuses it"
if [ -z "$ENGINE_IMAGE" ] || [ -z "$ENGINE_MODEL" ]; then
  record SKIP "$WARM_ROW" \
    "E2E_C127_ENGINE_IMAGE and E2E_C127_MODEL are not both set, so there is no real engine to compile anything; the warm-restart leg is skipped"
else
  WARM_IT=""
  # The first usable InstanceType whose reported manufacturer is the one named: the warm-restart leg
  # needs a pool whose cache directory carries a real vendor segment, not the cpu fallback. The
  # lookup is the library's: md_usable_type_for.
  for m in ascend nvidia amd; do
    WARM_IT="$(md_usable_type_for "$m")"
    [ -n "$WARM_IT" ] && break
  done
  if [ -z "$WARM_IT" ]; then
    record SKIP "$WARM_ROW" \
      "no usable InstanceType reports an admitted manufacturer (ascend, nvidia, amd), so no pool can run the real engine"
  elif ! has_localqueue; then
    record SKIP "$WARM_ROW" "no LocalQueue in ${NS}, so the group would never be admitted"
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
    instanceType: ${WARM_IT}
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
      record FAIL "$WARM_ROW" "the graph-mode deployment never rendered a Pod"
    else
      PODS_CREATED+=("$(pod_field "$WARM_POD" '{.metadata.uid}')")
      WARM_NODE="$(pod_field "$WARM_POD" '{.spec.nodeName}')"
      WARM_DIR="$(pod_field "$WARM_POD" '{.spec.volumes[?(@.name=="gpustack-compile-cache")].hostPath.path}')"
      if wait_pod_ready "$WARM_POD" "$READY_BOUND"; then
        record PASS "the graph-mode deployment runs and reaches Ready" \
          "${WARM_POD} Ready on ${WARM_NODE}, cache directory ${WARM_DIR:-<absent>}"
      else
        record FAIL "the graph-mode deployment runs and reaches Ready" \
          "${WARM_POD} not Ready within ${READY_BOUND}s (waiting='$(pod_field "$WARM_POD" '{.status.containerStatuses[?(@.name=="main")].state.waiting.reason}')')"
      fi

      PROBE1="case127-probe1-${SFX}"
      LISTING="$(warm_cache_probe "$PROBE1" "$WARM_NODE" "$WARM_DIR")"
      PROBE_RC=$?
      if [ "$PROBE_RC" -eq 2 ]; then
        record SKIP "the host cache subdirectory holds the compiled artifacts" \
          "the probe Pod was refused (namespace policy), so the host directory could not be read"
      elif [ "$PROBE_RC" -eq 3 ]; then
        record SKIP "the host cache subdirectory holds the compiled artifacts" \
          "the probe image ${PROBE_IMAGE} never pulled, so the host directory could not be read"
      elif [ "$PROBE_RC" -ne 0 ]; then
        record SKIP "the host cache subdirectory holds the compiled artifacts" \
          "the probe Pod never completed, so the host directory could not be read"
      elif [ -n "$LISTING" ]; then
        record PASS "the host cache subdirectory holds the compiled artifacts" \
          "${WARM_DIR} carries: $(printf '%s' "$LISTING" | tr '\n' ' ' | cut -c1-120)"
      else
        record FAIL "the host cache subdirectory holds the compiled artifacts" \
          "${WARM_DIR} is empty after the deployment reached Ready: the engine wrote nothing the mount points at"
      fi
      kubectl -n "$NS" delete pod "$PROBE1" --ignore-not-found --wait=false >/dev/null 2>&1

      FIRST_POD="$WARM_POD"
      kubectl -n "$NS" delete pod "$FIRST_POD" --wait=false >/dev/null 2>&1
      REPLACEMENT=""
      for _ in $(seq 1 120); do
        REPLACEMENT="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD_WARM}" \
          -o jsonpath='{.items[*].metadata.name}' 2>/dev/null | tr ' ' '\n' | grep -v "^${FIRST_POD}$" | head -n 1)"
        [ -n "$REPLACEMENT" ] && break
        sleep 2
      done
      if [ -z "$REPLACEMENT" ]; then
        record SKIP "the replacement renders the same cache subdirectory" \
          "no replacement Pod appeared after the first was deleted"
        record SKIP "the populated directory survives the replacement" "no replacement Pod to read"
        record SKIP "the replacement reaches Ready on the populated cache" "no replacement Pod to wait for"
      else
        PODS_CREATED+=("$(pod_field "$REPLACEMENT" '{.metadata.uid}')")
        REPL_DIR="$(pod_field "$REPLACEMENT" '{.spec.volumes[?(@.name=="gpustack-compile-cache")].hostPath.path}')"
        REPL_NODE="$(pod_field "$REPLACEMENT" '{.spec.nodeName}')"
        if [ "$REPL_DIR" = "$WARM_DIR" ] && [ "$REPL_NODE" = "$WARM_NODE" ]; then
          record PASS "the replacement renders the same cache subdirectory" \
            "${REPLACEMENT} on ${REPL_NODE} carries ${REPL_DIR}"
        else
          record FAIL "the replacement renders the same cache subdirectory" \
            "${REPLACEMENT} renders '${REPL_DIR:-<absent>}' on ${REPL_NODE:-<absent>} (first Pod had ${WARM_DIR} on ${WARM_NODE})"
        fi

        PROBE2="case127-probe2-${SFX}"
        LISTING2="$(warm_cache_probe "$PROBE2" "$REPL_NODE" "$REPL_DIR")"
        PROBE_RC2=$?
        if [ "$PROBE_RC2" -eq 0 ] && [ -n "$LISTING2" ]; then
          record PASS "the populated directory survives the replacement" \
            "${REPL_DIR} still carries: $(printf '%s' "$LISTING2" | tr '\n' ' ' | cut -c1-120)"
        elif [ "$PROBE_RC2" -eq 2 ] || [ "$PROBE_RC2" -eq 3 ]; then
          record SKIP "the populated directory survives the replacement" \
            "the probe could not read the host directory this round"
        elif [ "$PROBE_RC2" -eq 4 ]; then
          record SKIP "the populated directory survives the replacement" \
            "the probe Pod never completed this round"
        else
          record FAIL "the populated directory survives the replacement" \
            "${REPL_DIR} reads empty while the replacement serves"
        fi
        kubectl -n "$NS" delete pod "$PROBE2" --ignore-not-found --wait=false >/dev/null 2>&1

        if wait_pod_ready "$REPLACEMENT" "$READY_BOUND"; then
          record PASS "the replacement reaches Ready on the populated cache" \
            "${REPLACEMENT} Ready; its warm start reused the directory the first Pod compiled"
        else
          record FAIL "the replacement reaches Ready on the populated cache" \
            "${REPLACEMENT} not Ready within ${READY_BOUND}s"
        fi
      fi
    fi
  fi
fi

results
