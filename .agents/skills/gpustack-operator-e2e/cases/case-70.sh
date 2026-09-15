#!/usr/bin/env bash
#
# CASE 70 — A routed deployment owns one complete router, survives both router transitions, and
#           reports every cache-event publication state observable from the cluster
#           (MUTATING, self-cleaning)
#
#   case-70.sh <NS>
#
# Environment: A single-node cluster with no accelerator, the operator deployed, the general
#              InstanceType materialized by CASE 1, and pull access to the Mooncake fixture image.
#              No serving engine image is required: the assertions read rendered objects, Pod
#              specifications and status. A real Ready Binding keeps the shared-store composition
#              path covered alongside the direct-only path exercised by unit tests.
#
# Expected:    Adding spec.router creates Deployment, ConfigMap, Service, ServiceAccount, Role and
#              RoleBinding with discoverability notes and owner references. Removing it prunes all
#              six; adding it again restores all six; deleting the ModelDeployment lets Kubernetes
#              garbage collection remove all six. The deployment remains Starting with no ready
#              role replicas. KVEventsPublishing reaches NotApplicable, NoRouter,
#              PublisherDisabled, RoleUnmanaged and Publishing, with status and reason asserted.
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

OPERATOR_NS="${1:?usage: case-70.sh <NS>}"
KCTX="${E2E_KUBE_CONTEXT:-}"
k() { if [ -n "$KCTX" ]; then kubectl --context "$KCTX" "$@"; else kubectl "$@"; fi; }

# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_kvcache-inject-lib.sh"

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

wait_for() {
  local deadline=$((SECONDS + $1)); shift
  while [ "$SECONDS" -lt "$deadline" ]; do
    "$@" && return 0
    sleep 3
  done
  "$@"
}

usable_instance_type() {
  k get instancetypes.worker.gpustack.ai \
    -o jsonpath='{range .items[*]}{.metadata.name}|{.metadata.deletionTimestamp}|{.spec.inactive}{"\n"}{end}' \
    2>/dev/null | while IFS='|' read -r name deleting inactive; do
      [ -n "$name" ] || continue
      [ -z "$deleting" ] || continue
      [ "$inactive" = true ] && continue
      echo "$name"
      break
    done
}

[ -n "$IT" ] || IT="$(usable_instance_type)"
if [ -z "$IT" ]; then
  echo "[case-70] no usable InstanceType; run case-1 first" >&2
  exit 2
fi

force_release() {
  local md="$1" uids row wl owners uid
  uids="$(k -n "$NS" get pods -l "app.kubernetes.io/instance=${md}" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' 2>/dev/null)"
  [ -n "$uids" ] || return 0
  while IFS='|' read -r wl owners; do
    [ -n "$wl" ] || continue
    for uid in $uids; do
      case ",$owners," in *",$uid,"*)
        k -n "$NS" delete workload "$wl" --ignore-not-found --wait=false >/dev/null 2>&1
        break
        ;;
      esac
    done
  done <<EOF
$(k -n "$NS" get workloads.kueue.x-k8s.io \
  -o jsonpath='{range .items[*]}{.metadata.name}{"|"}{range .metadata.ownerReferences[*]}{.uid}{","}{end}{"\n"}{end}' 2>/dev/null)
EOF
}

cleanup() {
  local pod
  for pod in $(k -n "$NS" get pods -l "app.kubernetes.io/instance=${LIFECYCLE}" \
      -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
    k -n "$NS" patch pod "$pod" --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
  done
  k -n "$NS" delete modeldeployment "$LIFECYCLE" "$SERVER" "$UNMANAGED" "$ROUTED_SERVER" "$DISABLED" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  sleep 5
  force_release "$LIFECYCLE"
  force_release "$SERVER"
  force_release "$UNMANAGED"
  force_release "$ROUTED_SERVER"
  force_release "$DISABLED"
}

case70_teardown() {
  cleanup
  kvi_teardown
}
trap case70_teardown EXIT

if ! kvi_setup; then
  record FAIL "the cache fixture becomes Ready" \
    "backend, pool or Binding did not converge; Publishing cannot be measured"
  kvi_results "$CASE_ID"
  exit 1
fi

role() {
  local name="$1" kind="$2" managed="$3"
  printf '  - name: %s\n    kind: %s\n    instanceType: %s\n    replicas: 1\n' "$name" "$kind" "$IT"
  printf '    template:\n      image: %s\n' "$IMAGE"
  [ "$managed" = yes ] || printf '      command: ["/pause"]\n'
}

apply_md() {
  local name="$1" router="$2" roles="$3" engine="${4:-vllm}"
  {
    cat <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${name}
  namespace: ${NS}
spec:
  engine: ${engine}
  engineVersion: "0.11.0"
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  kvCache:
    poolRef:
      name: ${BINDING}
YAML
    if [ "$router" = yes ]; then
      cat <<YAML
  router:
    name: llm-d
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
    [ "$row" = "$uid|modeldeployments|llm-d" ] || return 1
  done
}

pair="$(role prefill prefill yes)
$(role decode decode yes)"

# Begin unrouted: this is both one direction of the transition and NoRouter.
apply_md "$LIFECYCLE" no "$pair" >/dev/null
assert_condition "$LIFECYCLE" False NoRouter "an unrouted P/D pair reports no consumer"

k -n "$NS" patch modeldeployment "$LIFECYCLE" --type=merge \
  -p '{"spec":{"router":{"name":"llm-d","image":"'"$ROUTER_IMAGE"'"}}}' >/dev/null
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
apply_md "$DISABLED" yes "$(role server server yes)" sglang >/dev/null
assert_condition "$DISABLED" False PublisherDisabled "a routed engine without event publishing reports publisher disabled"

k -n "$NS" patch modeldeployment "$LIFECYCLE" --type=json \
  -p='[{"op":"remove","path":"/spec/router"}]' >/dev/null
if wait_for "$SETTLE" router_count_is 0; then
  record PASS "removing spec.router prunes all six objects" "six of six absent"
else
  record FAIL "removing spec.router prunes all six objects" "one or more router objects remain"
fi

k -n "$NS" patch modeldeployment "$LIFECYCLE" --type=merge \
  -p '{"spec":{"router":{"name":"llm-d","image":"'"$ROUTER_IMAGE"'"}}}' >/dev/null
if wait_for "$SETTLE" router_count_is 6; then
  record PASS "re-adding spec.router restores all six objects" "six of six present"
else
  record FAIL "re-adding spec.router restores all six objects" "one or more router objects absent"
fi

apply_md "$SERVER" no "$(role server server no)" >/dev/null
assert_condition "$SERVER" True NotApplicable "an unrouted server-only deployment is not applicable"

apply_md "$UNMANAGED" yes "$(role prefill prefill no)
$(role decode decode no)" >/dev/null
assert_condition "$UNMANAGED" Unknown RoleUnmanaged "a producer-owned command is unmanaged"

apply_md "$ROUTED_SERVER" yes "$(role server server yes)" >/dev/null
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

echo
echo "== case-70: router lifecycle and cache-event condition states =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

[ "$FAILS" -eq 0 ] || { echo "[case-70] ${FAILS} check(s) FAILED"; exit 1; }
echo "all case-70 checks PASS"
