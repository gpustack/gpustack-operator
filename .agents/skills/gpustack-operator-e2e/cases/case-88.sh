#!/usr/bin/env bash
#
# CASE 88 — A topology-aware ModelDeployment passes device admission on a real accelerator
#           (MUTATING, self-recovering; AUTO-SKIPS without a free accelerator)
#
#   case-88.sh <NS>
#
# Goal:        Prove the operator's TAS and per-card admission paths agree on the same accelerator
#              node. A node-label TopologySource gives one free accelerator node a region/zone
#              profile; the existing accelerated ClusterQueue migrates to that profile without a
#              UID change; a zone-required ModelDeployment then reserves the accelerated flavor,
#              passes gpustack-node-devices, receives a Kueue topology assignment and binds there.
# Environment: The operator and bundled Kueue with TAS enabled, one Ready schedulable Node with a
#              free accelerator in its Devices ledger, and an Active accelerated InstanceType.
#              The namespace must contain the InstanceType's entrance LocalQueue. AUTO-SKIPS when
#              no free accelerator exists; exits 2 when the scheduling chain is incomplete.
# Inputs:      Real Node labels, Devices ledger, generated Topology/ResourceFlavor/ClusterQueue,
#              Kueue Workload and the rendered Pod. The pause image does not need an accelerator
#              userspace; placement is proven from admission, the Pod request and its bound Node.
# Expected:    Node and Devices carry the same topology profile; the accelerated ResourceFlavor
#              references that profile's Topology; ClusterQueue name and UID stay fixed; the
#              Workload requests zone placement, receives a topology assignment, both admission
#              checks become Ready, and its Pod requesting one accelerator binds to that Node.
# Cleanup:     Deletes the Workload before the ModelDeployment, deletes the source, and removes the
#              case selector label. It does not delete operator-owned queues, flavors or Topologies.
set -uo pipefail

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-88.sh <NS>" >&2
  exit 2
fi

PREFIX="case88-$$"
SOURCE="${PREFIX}-native"
MD="${PREFIX}-gpu-zone"
SELECTOR_KEY="e2e.gpustack.ai/case88"
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
NODE=""
WL=""

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

cleanup() {
  if [ -n "$WL" ]; then
    kubectl -n "$NS" delete workload.kueue.x-k8s.io "$WL" \
      --ignore-not-found --wait=false >/dev/null 2>&1
  fi
  kubectl -n "$NS" delete modeldeployment.worker.gpustack.ai "$MD" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl delete topologysource.worker.gpustack.ai "$SOURCE" \
    --ignore-not-found --wait=true --timeout=90s >/dev/null 2>&1
  if [ -n "$NODE" ]; then
    kubectl label node "$NODE" "${SELECTOR_KEY}-" >/dev/null 2>&1
  fi
}
trap cleanup EXIT

# A free whole card is mode 0 with the full scalar budget. Select from the authoritative ledger,
# then prove below that the matching Node is Ready and schedulable.
NODE="$(kubectl get devices.worker.gpustack.ai -o json 2>/dev/null | jq -r '
  [.items[]
   | select(any(.status.groups[]?.accelerators[]?;
       ((.mode // 0) == 0 and (.remaining // 0) >= 1600000)))
   | .metadata.name] | sort | .[0] // ""')"
if [ -z "$NODE" ]; then
  echo "== CASE 88 — SKIPPED =="
  echo "No Devices ledger reports a free whole accelerator card."
  exit 0
fi

ready="$(kubectl get node "$NODE" -o jsonpath='{range .status.conditions[?(@.type=="Ready")]}{.status}{end}' 2>/dev/null)"
unschedulable="$(kubectl get node "$NODE" -o jsonpath='{.spec.unschedulable}' 2>/dev/null)"
if [ "$ready" != True ] || [ "$unschedulable" = true ]; then
  echo "== CASE 88 — SKIPPED =="
  echo "The free accelerator Node is not Ready and schedulable (Ready=${ready:-missing}, unschedulable=${unschedulable:-false})."
  exit 0
fi

ACCELERATOR_KEY="$(kubectl get devices.worker.gpustack.ai "$NODE" -o json 2>/dev/null | jq -r '
  [.metadata.labels | to_entries[]
   | select(.key | startswith("acceleratable.feature.gpustack.ai/"))
   | select(.key | endswith(".count") or endswith(".capacity") | not)
   | select(.value == "true")
   | .key | sub("^acceleratable.feature.gpustack.ai/"; "")] | sort | .[0] // ""')"
IT="$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null | jq -r --arg key "$ACCELERATOR_KEY" '
  [.items[]
   | select(.spec.acceleratorGroup == $key)
   | select((.spec.inactive // false) == false and .status.phase == "Active")
   | .metadata.name] | sort | .[0] // ""')"
if [ -z "$ACCELERATOR_KEY" ] || [ -z "$IT" ]; then
  echo "The free accelerator Node has no matching Active InstanceType; key=${ACCELERATOR_KEY:-missing}." >&2
  exit 2
fi

ENTRANCE="$(kubectl get instancetype.worker.gpustack.ai "$IT" -o jsonpath='{.status.entrance}' 2>/dev/null)"
CQ="$IT"
CQ_UID="$(kubectl get clusterqueue.kueue.x-k8s.io "$CQ" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
LQ_CQ="$(kubectl -n "$NS" get localqueue.kueue.x-k8s.io "$ENTRANCE" -o jsonpath='{.spec.clusterQueue}' 2>/dev/null)"
if [ -z "$ENTRANCE" ] || [ -z "$CQ_UID" ] || [ "$LQ_CQ" != "$CQ" ]; then
  echo "Accelerated scheduling chain is incomplete: IT=${IT}, entrance=${ENTRANCE:-missing}, queue=${LQ_CQ:-missing}." >&2
  exit 2
fi

kubectl label node "$NODE" "${SELECTOR_KEY}=${PREFIX}" --overwrite >/dev/null
cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: TopologySource
metadata:
  name: ${SOURCE}
spec:
  nodeSelector:
    matchLabels:
      ${SELECTOR_KEY}: ${PREFIX}
  levels:
    - topology.kubernetes.io/region
    - topology.kubernetes.io/zone
  nodeLabels: {}
YAML

PROFILE=""
TOPOLOGY=""
for _ in $(seq 1 90); do
  source_ready="$(kubectl get topologysource "$SOURCE" \
    -o jsonpath='{range .status.conditions[?(@.type=="Ready")]}{.status}{end}' 2>/dev/null)"
  PROFILE="$(kubectl get node "$NODE" -o jsonpath='{.metadata.labels.topology\.gpustack\.ai/profile}' 2>/dev/null)"
  device_profile="$(kubectl get devices.worker.gpustack.ai "$NODE" \
    -o jsonpath='{.metadata.labels.topology\.gpustack\.ai/profile}' 2>/dev/null)"
  TOPOLOGY="gpustack-${PROFILE}"
  levels="$(kubectl get topology.kueue.x-k8s.io "$TOPOLOGY" \
    -o jsonpath='{range .spec.levels[*]}{.nodeLabel}{" "}{end}' 2>/dev/null)"
  [ "$source_ready" = True ] && [ -n "$PROFILE" ] && [ "$device_profile" = "$PROFILE" ] \
    && [ "$levels" = 'topology.kubernetes.io/region topology.kubernetes.io/zone kubernetes.io/hostname ' ] \
    && break
  sleep 2
done
if [ "$source_ready" = True ] && [ -n "$PROFILE" ] && [ "$device_profile" = "$PROFILE" ]; then
  record PASS "Node and Devices converge on one topology profile" \
    "${NODE}: ${PROFILE}; source Ready and ledger selector updated"
else
  record FAIL "Node and Devices converge on one topology profile" \
    "source=${source_ready:-missing}, node=${PROFILE:-missing}, devices=${device_profile:-missing}"
fi

RF=""
for _ in $(seq 1 90); do
  active="$(kubectl get clusterqueue.kueue.x-k8s.io "$CQ" \
    -o jsonpath='{range .status.conditions[?(@.type=="Active")]}{.status}{end}' 2>/dev/null)"
  topology_ready="$(kubectl get clusterqueue.kueue.x-k8s.io "$CQ" \
    -o jsonpath='{range .status.conditions[?(@.type=="TopologyReady")]}{.status}{end}' 2>/dev/null)"
  RF="$(kubectl get clusterqueue.kueue.x-k8s.io "$CQ" -o json 2>/dev/null | jq -r '.spec.resourceGroups[].flavors[].name' \
    | while read -r f; do
        [ "$(kubectl get resourceflavor.kueue.x-k8s.io "$f" -o jsonpath='{.spec.topologyName}' 2>/dev/null)" = "$TOPOLOGY" ] && echo "$f"
      done | head -1)"
  uid_now="$(kubectl get clusterqueue.kueue.x-k8s.io "$CQ" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
  [ "$active" = True ] && [ "$topology_ready" = True ] && [ -n "$RF" ] && [ "$uid_now" = "$CQ_UID" ] && break
  sleep 2
done
if [ "$active" = True ] && [ "$topology_ready" = True ] && [ -n "$RF" ] && [ "$uid_now" = "$CQ_UID" ]; then
  record PASS "accelerated queue migrates in place to the discovered topology" \
    "${CQ} kept uid=${CQ_UID}; flavor=${RF}; topology=${TOPOLOGY}"
else
  record FAIL "accelerated queue migrates in place to the discovered topology" \
    "active=${active:-missing}, topologyReady=${topology_ready:-missing}, flavor=${RF:-missing}, uid=${uid_now:-missing}"
fi

cat <<YAML | kubectl -n "$NS" apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD}
spec:
  engine:
    name: vllm
    version: "0.11.0"
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  kvCache:
    poolRef:
      name: ${PREFIX}-no-such-binding
  roles:
    - name: server
      instanceType: ${IT}
      replicas: 1
      size: 1
      image: ${IMAGE}
      command: ["/pause"]
      topology:
        requiredLevel: topology.kubernetes.io/zone
YAML

POD=""
BOUND_NODE=""
for _ in $(seq 1 90); do
  POD="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
  WL="$(kubectl -n "$NS" get pod "$POD" \
    -o jsonpath='{.metadata.annotations.kueue\.x-k8s\.io/workload}' 2>/dev/null)"
  admitted="$(kubectl -n "$NS" get workload.kueue.x-k8s.io "$WL" \
    -o jsonpath='{range .status.conditions[?(@.type=="Admitted")]}{.status}{end}' 2>/dev/null)"
  device_check="$(kubectl -n "$NS" get workload.kueue.x-k8s.io "$WL" \
    -o jsonpath='{range .status.admissionChecks[?(@.name=="gpustack-node-devices")]}{.state}{end}' 2>/dev/null)"
  BOUND_NODE="$(kubectl -n "$NS" get pod "$POD" -o jsonpath='{.spec.nodeName}' 2>/dev/null)"
  [ "$admitted" = True ] && [ "$device_check" = Ready ] && [ -n "$BOUND_NODE" ] && break
  sleep 2
done

wl_json="$(kubectl -n "$NS" get workload.kueue.x-k8s.io "$WL" -o json 2>/dev/null)"
required="$(printf '%s' "$wl_json" | jq -r '.spec.podSets[0].topologyRequest.required // ""' 2>/dev/null)"
assignment_count="$(printf '%s' "$wl_json" | jq '[.status.admission.podSetAssignments[]?.topologyAssignment.slices[]?] | length' 2>/dev/null)"
if [ "$required" = topology.kubernetes.io/zone ] && [ "${assignment_count:-0}" -gt 0 ]; then
  record PASS "Kueue admits the GPU Workload through TAS" \
    "required=${required}; topologyAssignment slices=${assignment_count}; admitted=${admitted:-missing}"
else
  record FAIL "Kueue admits the GPU Workload through TAS" \
    "required=${required:-missing}, assignments=${assignment_count:-0}, admitted=${admitted:-missing}"
fi

if [ "$device_check" = Ready ]; then
  record PASS "per-card device admission accepts the topology-scoped ledger" \
    "gpustack-node-devices=Ready for flavor ${RF}"
else
  check_message="$(printf '%s' "$wl_json" | jq -r '.status.admissionChecks[]? | select(.name=="gpustack-node-devices") | .message' 2>/dev/null)"
  record FAIL "per-card device admission accepts the topology-scoped ledger" \
    "state=${device_check:-missing}: ${check_message:-no message}"
fi

gpu_request="$(kubectl -n "$NS" get pod "$POD" -o jsonpath='{.spec.containers[0].resources.requests.nvidia\.com/gpu}' 2>/dev/null)"
bound_profile="$(kubectl get node "$BOUND_NODE" -o jsonpath='{.metadata.labels.topology\.gpustack\.ai/profile}' 2>/dev/null)"
if [ "$BOUND_NODE" = "$NODE" ] && [ "$bound_profile" = "$PROFILE" ] && [ "$gpu_request" = 1 ]; then
  record PASS "the admitted Pod requests and binds the real accelerator" \
    "pod=${POD}; node=${BOUND_NODE}; profile=${bound_profile}; nvidia.com/gpu=${gpu_request}"
else
  record FAIL "the admitted Pod requests and binds the real accelerator" \
    "pod=${POD:-missing}, node=${BOUND_NODE:-missing}, expected=${NODE}, profile=${bound_profile:-missing}, gpu=${gpu_request:-missing}"
fi

echo
echo "== CASE 88 — A topology-aware ModelDeployment passes device admission on a real accelerator =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). Inspect the Workload admission checks, assigned flavor, Devices labels and bound Node."
  exit 1
fi
echo "CASE 88 PASS"
