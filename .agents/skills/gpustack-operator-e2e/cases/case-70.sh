#!/usr/bin/env bash
#
# CASE 70 — A routed deployment owns one complete router, survives both router transitions, and
#           reports every cache-event publication state observable from the cluster
#           (MUTATING, self-cleaning, AUTO-SKIPS without a pullable router image)
#
#   case-70.sh <NS>
#
# Prerequisites: nodes=1; gpu=none; features=mooncake-store; cluster=any; inputs=E2E_KUBE_CONTEXT,E2E_MD_IMAGE,E2E_MD_INSTANCE_TYPE,E2E_MD_SETTLE,E2E_ROUTER_SETTLE,E2E_ROUTER_STABLE,E2E_ROUTER_UNPULLABLE_IMAGE
# Goal:        A routed deployment owns exactly one complete router object set through both router
#              transitions, its status reports every cache-event publication state observable from
#              the cluster, and a ready router becomes the published endpoint.
# Environment: A single-node cluster with no accelerator, the operator deployed, the general
#              InstanceType materialized by CASE 1, and pull access to the Mooncake fixture image.
#              No serving engine image is required: the assertions read rendered objects, Pod
#              specifications and status. A real Ready Binding keeps the shared-store composition
#              path covered alongside the direct-only path exercised by unit tests. The
#              router-endpoint half needs the upstream router image to be pullable; an image pull
#              failure is a stated SKIP, and it leaves the router-lifecycle and cache-event rows
#              untouched.
# Inputs:      All real: the operator's charts and images, the Mooncake fixture image and a Ready
#              Binding. No serving engine image — the assertions read rendered objects, Pod
#              specifications and status. The upstream router image is pulled from its registry
#              for the endpoint half.
# Expected:    Adding spec.router creates Deployment, ConfigMap, Service, ServiceAccount, Role and
#              RoleBinding with discoverability notes and owner references. Removing it prunes all
#              six; adding it again restores all six; deleting the ModelDeployment lets Kubernetes
#              garbage collection remove all six. The deployment remains Starting with no ready
#              role replicas. KVEventsPublishing reaches NotApplicable, NoRouter,
#              PublisherDisabled, RoleUnmanaged and Publishing, with status and reason asserted.
#              A ready router becomes the published endpoint, and stays ready with no container
#              restart.
# Cleanup:     The teardown releases any finalizer-held Pods it created and deletes the lifecycle
#              objects, in the fixture namespace and in the namespace the runner passed.
set -uo pipefail

# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

OPERATOR_NS="${1:?usage: case-70.sh <NS>}"
KCTX="${E2E_KUBE_CONTEXT:-}"
k() { if [ -n "$KCTX" ]; then kubectl --context "$KCTX" "$@"; else kubectl "$@"; fi; }
# The readers in _md-lib.sh talk to the same context this case's own kubectl calls do.
# shellcheck disable=SC2034 # Read by the sourced _md-lib.sh.
MD_KUBE_CONTEXT="$KCTX"

# The injected fixture library carries the results table with it: it owns record, and it sources
# _rows-lib.sh, which owns print_rows. One printer for this case.
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_kvcache-inject-lib.sh"
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_md-lib.sh"

# The shared fixture creates the Ready Binding in its own namespace. ModelDeployment poolRef is
# local by type, so the deployments must live there too; OPERATOR_NS remains the required runner
# argument and identifies where the installed operator itself lives.
if ! k get namespace "$OPERATOR_NS" >/dev/null 2>&1; then
  echo "[case-70] operator namespace $OPERATOR_NS does not exist" >&2
  exit 2
fi
NS="$TEST_NS"
CASE_ID=70
IT="${E2E_MD_INSTANCE_TYPE:-}"
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
SETTLE="${E2E_MD_SETTLE:-120}"
LIFECYCLE=case70-lifecycle
SERVER=case70-server
UNMANAGED=case70-unmanaged
ROUTED_SERVER=case70-routed-server
DISABLED=case70-publisher-disabled
ROUTER_IMAGE="${E2E_ROUTER_UNPULLABLE_IMAGE:-invalid.invalid/gpustack/router:not-present}"

# The router-endpoint half runs its own deployment, in the namespace the runner passed, with the
# router image the operator defaults to. It names a Binding that does not exist, so no cache
# connector is rendered and the deployment depends on nothing but the router.
ROUTER_READY=case70-router-ready
ROUTER_SETTLE="${E2E_ROUTER_SETTLE:-180}"
STABLE="${E2E_ROUTER_STABLE:-30}"

# This case's own poller: the shared one in _rows-lib.sh takes a kind/name/path/want and reads one
# jsonpath, while this case waits on a predicate the caller supplies, so the two cannot be called
# with the same arguments.
wait_for() {
  local deadline=$((SECONDS + $1)); shift
  while [ "$SECONDS" -lt "$deadline" ]; do
    "$@" && return 0
    sleep 3
  done
  "$@"
}

[ -n "$IT" ] || IT="$(md_usable_instance_type)"
if [ -z "$IT" ]; then
  echo "[case-70] no usable InstanceType; run case-1 first" >&2
  exit 2
fi

# md_force_release deletes the Kueue Workloads that still hold a Pod of the named deployment. The
# namespace is an argument because the merged scope owns deployments in two of them.

cleanup() {
  local pod
  for pod in $(k -n "$NS" get pods -l "app.kubernetes.io/instance=${LIFECYCLE}" \
      -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
    k -n "$NS" patch pod "$pod" --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
  done
  for pod in $(k -n "$OPERATOR_NS" get pods -l "app.kubernetes.io/instance=${ROUTER_READY}" \
      -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
    k -n "$OPERATOR_NS" patch pod "$pod" --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
  done
  k -n "$NS" delete modeldeployment "$LIFECYCLE" "$SERVER" "$UNMANAGED" "$ROUTED_SERVER" "$DISABLED" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  k -n "$OPERATOR_NS" delete modeldeployment "$ROUTER_READY" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  sleep 5
  md_force_release "$NS" "$LIFECYCLE"
  md_force_release "$NS" "$SERVER"
  md_force_release "$NS" "$UNMANAGED"
  md_force_release "$NS" "$ROUTED_SERVER"
  md_force_release "$NS" "$DISABLED"
  md_force_release "$OPERATOR_NS" "$ROUTER_READY"
}

case70_teardown() {
  cleanup
  kvi_teardown
}
trap case70_teardown EXIT

role() {
  local name="$1" kind="$2" managed="$3"
  printf '  - name: %s\n    kind: %s\n    instanceType: %s\n    replicas: 1\n' "$name" "$kind" "$IT"
  printf '    image: %s\n' "$IMAGE"
  [ "$managed" = yes ] || printf '    command: ["/pause"]\n'
}

apply_md() {
  local name="$1" router="$2" roles="$3" engine="${4:-vLLM}"
  {
    cat <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${name}
  namespace: ${NS}
spec:
  engine:
    name: ${engine}
    image: ${IMAGE}
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  kvCache:
    poolRef:
      name: ${BINDING}
YAML
    if [ "$router" = yes ]; then
      cat <<YAML
  router:
    name: llm-d-router
    image: ${ROUTER_IMAGE}
YAML
    fi
    printf '  roles:\n%s\n' "$roles"
  } | k apply -f -
}

condition_is() {
  local md="$1" status="$2" reason="$3" got
  got="$(k -n "$NS" get modeldeployment "$md" \
    -o jsonpath='{.status.conditions[?(@.type=="KVEventsPublishing")].status}{"|"}{.status.conditions[?(@.type=="KVEventsPublishing")].reason}' \
    2>/dev/null)"
  [ "$got" = "$status|$reason" ]
}

assert_condition() {
  local md="$1" status="$2" reason="$3" label="$4" got
  if wait_for "$SETTLE" condition_is "$md" "$status" "$reason"; then
    record PASS "$label" "${status}/${reason}"
  else
    got="$(k -n "$NS" get modeldeployment "$md" \
      -o jsonpath='{.status.conditions[?(@.type=="KVEventsPublishing")].status}{"/"}{.status.conditions[?(@.type=="KVEventsPublishing")].reason}' 2>/dev/null)"
    record FAIL "$label" "wanted ${status}/${reason}, got ${got:-none}"
  fi
}

router_count_is() {
  local expected="$1" total=0 kind
  for kind in deployment configmap service serviceaccount role rolebinding; do
    k -n "$NS" get "$kind" "${LIFECYCLE}-router" >/dev/null 2>&1 && total=$((total + 1))
  done
  [ "$total" = "$expected" ]
}

router_objects_valid() {
  local uid kind row
  uid="$(k -n "$NS" get modeldeployment "$LIFECYCLE" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
  [ -n "$uid" ] || return 1
  for kind in deployment configmap service serviceaccount role rolebinding; do
    row="$(k -n "$NS" get "$kind" "${LIFECYCLE}-router" \
      -o jsonpath='{.metadata.ownerReferences[0].uid}{"|"}{.metadata.labels.resource\.gpustack\.ai/type}{"|"}{.metadata.annotations.note\.gpustack\.ai/router}' 2>/dev/null)"
    [ "$row" = "$uid|modeldeployments|llm-d-router" ] || return 1
  done
}

diagnose_router() {
  echo >&2
  echo "[case-70] router diagnostics" >&2
  k -n "$OPERATOR_NS" get deployment "${ROUTER_READY}-router" -o yaml >&2 || true
  k -n "$OPERATOR_NS" get configmap "${ROUTER_READY}-router" -o yaml >&2 || true
  k -n "$OPERATOR_NS" get pods \
    -l "modeldeployment.gpustack.ai/router=llm-d-router,app.kubernetes.io/instance=${ROUTER_READY}" \
    -o yaml >&2 || true
  for container in envoy epp; do
    echo "[case-70] ${container} current log" >&2
    k -n "$OPERATOR_NS" logs deployment/"${ROUTER_READY}-router" -c "$container" >&2 || true
    echo "[case-70] ${container} previous log" >&2
    k -n "$OPERATOR_NS" logs deployment/"${ROUTER_READY}-router" -c "$container" --previous >&2 || true
  done
}

pair="$(role prefill Prefill yes)
$(role decode Decode yes)"

# Section one: the router lifecycle of a routed deployment, and the cache-event publication
# states its status reports. Every row here reads the fixture the injected library stood up, so
# the section runs only when that fixture converged; a failure to converge is one row, and the
# section below still runs.
fixture_ok=yes
if ! kvi_setup; then
  record FAIL "the cache fixture becomes Ready" \
    "backend, pool or Binding did not converge; Publishing cannot be measured"
  fixture_ok=no
fi

if [ "$fixture_ok" = yes ]; then
  # Begin unrouted: this is both one direction of the transition and NoRouter.
  apply_md "$LIFECYCLE" no "$pair" >/dev/null
  assert_condition "$LIFECYCLE" False NoRouter "an unrouted P/D pair reports no consumer"

  k -n "$NS" patch modeldeployment "$LIFECYCLE" --type=merge \
    -p '{"spec":{"router":{"name":"llm-d-router","image":"'"$ROUTER_IMAGE"'"}}}' >/dev/null
  if wait_for "$SETTLE" router_count_is 6 && router_objects_valid; then
    record PASS "adding spec.router converges all six objects" "owned and discoverable"
  else
    record FAIL "adding spec.router converges all six objects" "missing object, owner reference or resource note"
  fi

  starting_without_ready_roles() {
    local phase ready
    phase="$(k -n "$NS" get modeldeployment "$LIFECYCLE" -o jsonpath='{.status.phase}' 2>/dev/null)"
    ready="$(k -n "$NS" get modeldeployment "$LIFECYCLE" \
      -o jsonpath='{range .status.roles[*]}{.ready}{" "}{end}' 2>/dev/null)"
    [ "$phase" = Starting ] && ! echo "$ready" | grep -Eq '(^| )1( |$)'
  }
  if wait_for "$SETTLE" starting_without_ready_roles; then
    ready="$(k -n "$NS" get modeldeployment "$LIFECYCLE" \
      -o jsonpath='{range .status.roles[*]}{.ready}{" "}{end}' 2>/dev/null)"
    record PASS "the no-accelerator deployment stays Starting" "phase=Starting, ready=[${ready:-none}]"
  else
    phase="$(k -n "$NS" get modeldeployment "$LIFECYCLE" -o jsonpath='{.status.phase}' 2>/dev/null)"
    ready="$(k -n "$NS" get modeldeployment "$LIFECYCLE" \
      -o jsonpath='{range .status.roles[*]}{.ready}{" "}{end}' 2>/dev/null)"
    record FAIL "the no-accelerator deployment stays Starting" "phase=${phase:-none}, ready=[${ready:-none}]"
  fi

  assert_condition "$LIFECYCLE" True Publishing "a managed routed pair publishes cache events"

  producer_publishes_events() {
    local pod command ports
    pod="$(k -n "$NS" get pods \
      -l "app.kubernetes.io/instance=${LIFECYCLE},app.kubernetes.io/component=prefill" \
      -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
    [ -n "$pod" ] || return 1
    command="$(k -n "$NS" get pod "$pod" -o jsonpath='{.spec.containers[0].command}' 2>/dev/null)"
    ports="$(k -n "$NS" get pod "$pod" \
      -o jsonpath='{range .spec.containers[0].ports[*]}{.containerPort}{" "}{end}' 2>/dev/null)"
    [[ "$command" == *--kv-events-config* ]] && [[ " $ports " == *" 5557 "* ]] && [[ " $ports " == *" 5558 "* ]]
  }
  if wait_for "$SETTLE" producer_publishes_events; then
    record PASS "the producer Pod carries the KV event publisher" "argument and both event ports rendered"
  else
    record FAIL "the producer Pod carries the KV event publisher" "argument or event port absent"
  fi

  # PublisherDisabled is a rendered-configuration state. SGLang has router metrics but no KV event
  # publisher integration, so this fixture reaches the state without tying it to Pod liveness.
  apply_md "$DISABLED" yes "$(role server Server yes)" SGLang >/dev/null
  assert_condition "$DISABLED" False PublisherDisabled "a routed engine without event publishing reports publisher disabled"

  k -n "$NS" patch modeldeployment "$LIFECYCLE" --type=json \
    -p='[{"op":"remove","path":"/spec/router"}]' >/dev/null
  if wait_for "$SETTLE" router_count_is 0; then
    record PASS "removing spec.router prunes all six objects" "six of six absent"
  else
    record FAIL "removing spec.router prunes all six objects" "one or more router objects remain"
  fi

  k -n "$NS" patch modeldeployment "$LIFECYCLE" --type=merge \
    -p '{"spec":{"router":{"name":"llm-d-router","image":"'"$ROUTER_IMAGE"'"}}}' >/dev/null
  if wait_for "$SETTLE" router_count_is 6; then
    record PASS "re-adding spec.router restores all six objects" "six of six present"
  else
    record FAIL "re-adding spec.router restores all six objects" "one or more router objects absent"
  fi

  apply_md "$SERVER" no "$(role server Server no)" >/dev/null
  assert_condition "$SERVER" True NotApplicable "an unrouted server-only deployment is not applicable"

  apply_md "$UNMANAGED" yes "$(role prefill Prefill no)
$(role decode Decode no)" >/dev/null
  assert_condition "$UNMANAGED" Unknown RoleUnmanaged "a producer-owned command is unmanaged"

  apply_md "$ROUTED_SERVER" yes "$(role server Server yes)" >/dev/null
  assert_condition "$ROUTED_SERVER" True Publishing "a routed server does not report not applicable"

  # This delete is an assertion, not cleanup: only a live API server's garbage collector can prove it.
  k -n "$NS" delete modeldeployment "$LIFECYCLE" --wait=false >/dev/null
  gone() {
    ! k -n "$NS" get modeldeployment "$LIFECYCLE" >/dev/null 2>&1 && router_count_is 0
  }
  if wait_for "$SETTLE" gone; then
    record PASS "deleting the owner garbage-collects all six objects" "owner and six dependents absent"
  else
    record FAIL "deleting the owner garbage-collects all six objects" "owner or router dependent remains"
  fi
fi

# Section two: the router readiness and the endpoint it publishes. The upstream router image may
# be unavailable; an image pull failure is a stated SKIP, and any other failure to become Ready is
# a FAIL.
k apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${ROUTER_READY}
  namespace: ${OPERATOR_NS}
spec:
  engine:
    name: vLLM
    image: ${IMAGE}
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  kvCache:
    poolRef:
      name: case70-no-such-binding
  router:
    name: llm-d-router
  roles:
  - name: prefill
    kind: Prefill
    instanceType: ${IT}
    replicas: 1
    image: ${IMAGE}
  - name: decode
    kind: Decode
    instanceType: ${IT}
    replicas: 1
    image: ${IMAGE}
YAML

ready=no
for _ in $(seq 1 "$((ROUTER_SETTLE / 3))"); do
  if [ "$(k -n "$OPERATOR_NS" get deployment "${ROUTER_READY}-router" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)" = 1 ]; then
    ready=yes
    break
  fi
  sleep 3
done

if [ "$ready" != yes ]; then
  waiting="$(k -n "$OPERATOR_NS" get pods -l "modeldeployment.gpustack.ai/router=llm-d-router,app.kubernetes.io/instance=${ROUTER_READY}" \
    -o jsonpath='{range .items[*].status.containerStatuses[*]}{.name}{"="}{.state.waiting.reason}{" "}{end}' 2>/dev/null)"
  case "$waiting" in
    *ErrImagePull*|*ImagePullBackOff*|*InvalidImageName*)
      record SKIP "router readiness and endpoint" "upstream router image could not be pulled: ${waiting}"
      ;;
    *)
      record FAIL "router readiness and endpoint" "router did not become Ready within ${ROUTER_SETTLE}s; waiting=[${waiting:-none}]"
      k -n "$OPERATOR_NS" get pods -l "modeldeployment.gpustack.ai/router=llm-d-router,app.kubernetes.io/instance=${ROUTER_READY}" >&2 || true
      diagnose_router
      ;;
  esac
else
  want="http://${ROUTER_READY}-router.${OPERATOR_NS}.svc:8000"
  got=""
  for _ in $(seq 1 40); do
    got="$(k -n "$OPERATOR_NS" get modeldeployment "$ROUTER_READY" -o jsonpath='{.status.endpoint}' 2>/dev/null)"
    [ "$got" = "$want" ] && break
    sleep 3
  done

  # The endpoint row and the stability row describe the same live router, so the stability
  # measurement is taken once, here, and both rows report it.
  endpoint_ok=no
  stable=yes
  if [ "$got" = "$want" ]; then
    endpoint_ok=yes
    pod="$(k -n "$OPERATOR_NS" get pods \
      -l "modeldeployment.gpustack.ai/router=llm-d-router,app.kubernetes.io/instance=${ROUTER_READY}" \
      -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
    envoy_restarts="$(k -n "$OPERATOR_NS" get pod "$pod" \
      -o jsonpath='{.status.containerStatuses[?(@.name=="envoy")].restartCount}' 2>/dev/null)"
    epp_restarts="$(k -n "$OPERATOR_NS" get pod "$pod" \
      -o jsonpath='{.status.containerStatuses[?(@.name=="epp")].restartCount}' 2>/dev/null)"
    for _ in $(seq 1 "$((STABLE / 3))"); do
      sleep 3
      current_ready="$(k -n "$OPERATOR_NS" get deployment "${ROUTER_READY}-router" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)"
      current_envoy_restarts="$(k -n "$OPERATOR_NS" get pod "$pod" \
        -o jsonpath='{.status.containerStatuses[?(@.name=="envoy")].restartCount}' 2>/dev/null)"
      current_epp_restarts="$(k -n "$OPERATOR_NS" get pod "$pod" \
        -o jsonpath='{.status.containerStatuses[?(@.name=="epp")].restartCount}' 2>/dev/null)"
      if [ "$current_ready" != 1 ] || [ "$current_envoy_restarts" != "$envoy_restarts" ] || \
        [ "$current_epp_restarts" != "$epp_restarts" ]; then
        stable=no
        break
      fi
    done
  fi

  if [ "$endpoint_ok" = yes ]; then
    record PASS "a ready router becomes the published endpoint" "${got}"
  else
    record FAIL "a ready router becomes the published endpoint" "wanted ${want}, got ${got:-none}"
  fi

  if [ "$endpoint_ok" = yes ]; then
    if [ "$stable" = yes ]; then
      record PASS "router readiness remains stable" "${STABLE}s with no container restarts"
    else
      record FAIL "router readiness remains stable" "ready=${current_ready:-none}, restarts envoy=${envoy_restarts:-none}->${current_envoy_restarts:-none} epp=${epp_restarts:-none}->${current_epp_restarts:-none}"
      diagnose_router
    fi
  fi
fi

echo
echo "== case-${CASE_ID}: router lifecycle, cache-event condition states and the router endpoint =="
print_rows

[ "$FAILS" -eq 0 ] || { echo "[case-${CASE_ID}] ${FAILS} check(s) FAILED"; exit 1; }
if [ "$SKIPS" -gt 0 ]; then
  echo "[case-${CASE_ID}] ${SKIPS} check(s) SKIPPED and $(( ${#ROWS[@]} - SKIPS )) passed - the skipped ones verified NOTHING"
  exit 0
fi
echo "all case-${CASE_ID} checks PASS"
