#!/usr/bin/env bash
#
# CASE 105 — An Instance's persistent claims place it where the volume can attach: a bound claim
#            runs on its PV's node every time, waits without quota while that node is full, and a
#            claim nothing binds creates no Pod   (MUTATING, self-recovering; AUTO-SKIPS without two workers)
#
#   case-105.sh <NS>
#
# Goal:        Prove on a real API server, scheduler, kubelet and Kueue that an Instance's
#              spec.volume.persistent and spec.additionalVolumes[].persistent follow the claim
#              placement rules: a bound claim's PV node affinity reaches the Pod and TAS places it on
#              the PV's node on every start; while that node is full the Workload waits without
#              reserving quota and admits itself once room appears; a claim provisioned on the first
#              Pod's node gets no affinity and binds there; an unbound claim on an immediate class
#              creates no Pod and says why; the escape Setting instance-persistent-volume-placement
#              restores the render that knew only the claim names.
# Environment: A cluster with at least two schedulable workers (else SKIP: on one node TAS has no
#              wrong node to pick, so NOTHING WOULD BE VERIFIED), a materialized scheduling chain (run
#              case-1 first), an InstanceType whose pool covers the workers and, in <NS>, the pool's
#              entrance LocalQueue. The WaitForFirstConsumer row needs a default StorageClass that
#              binds on the first Pod's node with a provisioner (kind's local-path), else that row
#              SKIPs. NO GPU: every Pod runs the pause image.
# Inputs:      All real. Two hostPath PVs pinned to the last worker by node affinity and bound to
#              their claims through spec.volumeName; a claim on an immediate class whose provisioner
#              does not exist, so it stays Pending; a placeholder Pod assigned to the PV's node by
#              spec.nodeName, outside any queue, that takes the node's free CPU. Override the type
#              with E2E_MD_INSTANCE_TYPE and the image with E2E_MD_IMAGE.
# Expected:    - a workspace claim: over three starts, every Pod carries the PV's affinity and runs on
#                the PV's node;
#              - an additional-volume claim: the same;
#              - the PV's node full: the Pod is not scheduled, its Workload has QuotaReserved not True
#                and no admission; once the placeholder goes, the Pod runs on the PV's node;
#              - a WaitForFirstConsumer claim: the Pod carries no affinity, the claim binds and the Pod
#                runs;
#              - an unbound claim on an immediate class: no Pod, phase Starting, and the phase message
#                names the claim as Pending;
#              - the Setting off: that Instance's Pod is created, and a bound claim's Pod carries no
#                affinity.
# Cleanup:     Trap deletes every Instance and Pod carrying the case label, then the claims, the PVs
#              and the StorageClass, and puts the Setting back to the value it found.
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

NS="${1:?usage: case-105.sh <NS>}"
IT="${E2E_MD_INSTANCE_TYPE:-}"
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
SYSTEM_NS="${E2E_SYSTEM_NS:-gpustack-system}"
P=c105
LABEL="e2e.gpustack.ai/case=105"
SETTING=instance-persistent-volume-placement

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

# The Setting is read through the worker's thirty-second cache, so every change waits it out.
ORIG_SETTING="$(kubectl -n "$SYSTEM_NS" get secret gpustack-settings -o jsonpath="{.data.${SETTING}}" 2>/dev/null)"
set_placement() { # true|false
  kubectl -n "$SYSTEM_NS" patch secret gpustack-settings --type=merge \
    -p "{\"data\":{\"${SETTING}\":\"$(printf '%s' "$1" | base64)\"}}" >/dev/null
  sleep 35
}
restore_setting() {
  local v=null
  [ -n "$ORIG_SETTING" ] && v="\"${ORIG_SETTING}\""
  kubectl -n "$SYSTEM_NS" patch secret gpustack-settings --type=merge -p "{\"data\":{\"${SETTING}\":${v}}}" >/dev/null 2>&1
}

cleanup() {
  echo
  echo "[case-105] cleanup"
  kubectl -n "$NS" delete instances.worker.gpustack.ai,pods -l "$LABEL" --ignore-not-found --wait=false >/dev/null 2>&1
  for _ in $(seq 1 24); do
    [ -z "$(kubectl -n "$NS" get pods -o name 2>/dev/null | command grep "/${P}-")" ] && break
    sleep 5
  done
  kubectl -n "$NS" delete pvc -l "$LABEL" --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl delete pv "${P}-pv-ws" "${P}-pv-add" --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl delete storageclass "${P}-static" "${P}-immediate" --ignore-not-found >/dev/null 2>&1
  restore_setting
}
trap cleanup EXIT

if [ -z "$IT" ]; then
  IT="$(kubectl get instancetypes.worker.gpustack.ai -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
fi
[ -n "$IT" ] || { echo "[case-105] no InstanceType; run case-1 first" >&2; exit 2; }
WORKERS="$(kubectl get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath='{range .items[*]}{.metadata.labels.kubernetes\.io/hostname}{"\n"}{end}' | sort)"
PV_NODE="$(printf '%s\n' "$WORKERS" | sed -n '$p')"
if [ "$(printf '%s\n' "$WORKERS" | command grep -c .)" -lt 2 ]; then
  record SKIP "the placement rows" "fewer than two workers: TAS has no wrong node to pick; NOTHING WAS VERIFIED"
  print_rows
  exit 0
fi
PV_NODE_NAME="$(kubectl get nodes -l "kubernetes.io/hostname=${PV_NODE}" -o jsonpath='{.items[0].metadata.name}')"

inst() { # name volume-yaml [additional-volumes-yaml]
  kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: Instance
metadata: {name: $1, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "105"}}
spec:
  type: "${IT}"
  image: "${IMAGE}"
  resources: {cpu: "1", ram: 2Gi, localStorage: 1Gi}
  volume: $2${3:+
  additionalVolumes: $3}
YAML
}

pvc() { # name class [volumeName]; an empty class takes the cluster's default
  local vn="" sc=""
  [ -n "${3:-}" ] && vn="
  volumeName: $3"
  [ -n "$2" ] && sc="
  storageClassName: $2"
  kubectl apply -f - >/dev/null <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: $1, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "105"}}
spec:${sc}
  accessModes: [ReadWriteOnce]
  resources: {requests: {storage: 1Gi}}${vn}
YAML
}

pv() { # name
  kubectl apply -f - >/dev/null <<YAML
apiVersion: v1
kind: PersistentVolume
metadata: {name: $1, labels: {e2e.gpustack.ai/case: "105"}}
spec:
  capacity: {storage: 1Gi}
  accessModes: [ReadWriteOnce]
  storageClassName: ${P}-static
  hostPath: {path: /tmp/$1, type: DirectoryOrCreate}
  nodeAffinity:
    required:
      nodeSelectorTerms:
        - matchExpressions: [{key: kubernetes.io/hostname, operator: In, values: ["${PV_NODE}"]}]
YAML
}

pod_field() { kubectl -n "$NS" get pod "$1" -o jsonpath="$2" 2>/dev/null; }
affinity_of() { pod_field "$1" '{.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[?(@.key=="kubernetes.io/hostname")].values[0]}'; }

wait_running() { # pod bound -> node, or ""
  local node=""
  for _ in $(seq 1 $(( $2 / 3 ))); do
    if [ "$(pod_field "$1" '{.status.phase}')" = Running ]; then
      node="$(pod_field "$1" '{.spec.nodeName}')"
      break
    fi
    sleep 3
  done
  printf '%s' "$node"
}

wait_gone() { # pod
  for _ in $(seq 1 40); do
    kubectl -n "$NS" get pod "$1" >/dev/null 2>&1 || return 0
    sleep 3
  done
  return 1
}

workload_of() { # pod -> quotaReserved|admitted
  kubectl -n "$NS" get workloads.kueue.x-k8s.io -o json 2>/dev/null | python3 -c '
import json,sys
pod=sys.argv[1]
for w in json.load(sys.stdin)["items"]:
    if any(o.get("kind")=="Pod" and o.get("name")==pod for o in w["metadata"].get("ownerReferences",[])):
        c={x["type"]:x["status"] for x in w.get("status",{}).get("conditions",[])}
        print("%s|%s" % (c.get("QuotaReserved","<unset>"), "admission" if w.get("status",{}).get("admission") else "none"))
        break
' "$1"
}

placed() { # check pod
  local node aff
  node="$(wait_running "$2" 120)"
  aff="$(affinity_of "$2")"
  if [ "$node" = "$PV_NODE_NAME" ] && [ "$aff" = "$PV_NODE" ]; then
    record PASS "$1" "node=${node} affinity=${aff}"
  else
    record FAIL "$1" "node=${node:-<not running>} affinity=${aff:-<none>} pv=${PV_NODE}"
  fi
}

echo "== fixtures: two PVs pinned to ${PV_NODE}, bound claims, an immediate class =="
kubectl apply -f - >/dev/null <<YAML
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata: {name: ${P}-static}
provisioner: kubernetes.io/no-provisioner
volumeBindingMode: WaitForFirstConsumer
---
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata: {name: ${P}-immediate}
provisioner: e2e.gpustack.ai/provisions-nothing
volumeBindingMode: Immediate
YAML
pv "${P}-pv-ws"
pv "${P}-pv-add"
pvc "${P}-ws" "${P}-static" "${P}-pv-ws"
pvc "${P}-add" "${P}-static" "${P}-pv-add"
for c in "${P}-ws" "${P}-add"; do
  for _ in $(seq 1 20); do
    [ "$(kubectl -n "$NS" get pvc "$c" -o jsonpath='{.status.phase}' 2>/dev/null)" = Bound ] && break
    sleep 3
  done
done

echo "== 1. a workspace claim runs on the PV's node on every start =="
inst "${P}-ws" "{persistent: {name: ${P}-ws}}"
for round in 1 2 3; do
  placed "start ${round}: the workspace claim's Pod carries the PV's affinity and runs there" "${P}-ws"
  [ "$round" = 3 ] && break
  kubectl -n "$NS" patch instances.worker.gpustack.ai "${P}-ws" --type=merge -p '{"spec":{"stop":true}}' >/dev/null
  wait_gone "${P}-ws" || record FAIL "start ${round}: the Pod goes on stop" "still present after 120s"
  # Admission starts only a Stopped Instance, which the phase says a pass after the Pod is gone.
  for _ in $(seq 1 20); do
    [ "$(kubectl -n "$NS" get instances.worker.gpustack.ai "${P}-ws" -o jsonpath='{.status.phase}')" = Stopped ] && break
    sleep 3
  done
  kubectl -n "$NS" patch instances.worker.gpustack.ai "${P}-ws" --type=merge -p '{"spec":{"stop":false}}' >/dev/null
done
REQ="$(pod_field "${P}-ws" '{.spec.containers[0].resources.requests.cpu}')"

echo "== 2. an additional-volume claim runs on the PV's node =="
inst "${P}-add" "{ephemeral: {capacity: 1Gi}}" "[{mountPath: /mnt/data, persistent: {name: ${P}-add}}]"
placed "the additional claim's Pod carries the PV's affinity and runs there" "${P}-add"

echo "== 3. with the PV's node full, the Workload waits without quota =="
kubectl -n "$NS" delete instances.worker.gpustack.ai "${P}-ws" "${P}-add" --wait=false >/dev/null 2>&1
wait_gone "${P}-ws"
wait_gone "${P}-add"
FREE="$(kubectl get pods -A --field-selector "spec.nodeName=${PV_NODE_NAME}" -o json | python3 -c '
import json,sys
def milli(q):
    q=str(q)
    return int(q[:-1]) if q.endswith("m") else int(float(q)*1000)
alloc=milli(sys.argv[1]); used=0
for p in json.load(sys.stdin)["items"]:
    if p["status"].get("phase") in ("Succeeded","Failed"): continue
    for c in p["spec"]["containers"]:
        used+=milli(c.get("resources",{}).get("requests",{}).get("cpu","0"))
print(alloc-used)
' "$(kubectl get node "$PV_NODE_NAME" -o jsonpath='{.status.allocatable.cpu}')")"
REQ_M="$(printf '%s' "${REQ:-0}" | python3 -c 'import sys; q=sys.stdin.read().strip() or "0"; print(int(q[:-1]) if q.endswith("m") else int(float(q)*1000))')"
FILL=$(( FREE - REQ_M / 2 ))
if [ "$REQ_M" -le 0 ] || [ "$FILL" -le 0 ]; then
  record FAIL "the PV's node can be filled" "free=${FREE}m instance request=${REQ:-<none>}"
else
  kubectl apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata: {name: ${P}-fill, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "105"}}
spec:
  nodeName: ${PV_NODE_NAME}
  containers: [{name: pause, image: "${IMAGE}", resources: {requests: {cpu: "${FILL}m"}}}]
YAML
  wait_running "${P}-fill" 60 >/dev/null
  inst "${P}-full" "{persistent: {name: ${P}-ws}}"
  sleep 45
  node="$(pod_field "${P}-full" '{.spec.nodeName}')"
  wl="$(workload_of "${P}-full")"
  if [ -n "$(pod_field "${P}-full" '{.metadata.uid}')" ] && [ -z "$node" ] && [ "${wl%%|*}" != True ] && [ "${wl#*|}" = none ]; then
    record PASS "the full node's Workload waits without reserving quota" "fill=${FILL}m workload=${wl}"
  else
    record FAIL "the full node's Workload waits without reserving quota" "node=${node:-<none>} workload=${wl:-<none>}"
  fi
  kubectl -n "$NS" delete pod "${P}-fill" --wait=false >/dev/null 2>&1
  placed "once room appears the Pod runs on the PV's node" "${P}-full"
fi

echo "== 4. a WaitForFirstConsumer claim binds where its first Pod lands =="
default_class="$(kubectl get storageclass -o jsonpath='{range .items[?(@.metadata.annotations.storageclass\.kubernetes\.io/is-default-class=="true")]}{.metadata.name}|{.volumeBindingMode}|{.provisioner}{"\n"}{end}' | head -1)"
case "$default_class" in
  *'|WaitForFirstConsumer|kubernetes.io/no-provisioner'|'') record SKIP "a WaitForFirstConsumer claim binds on the first Pod's node" "no default class provisioning on the first Pod's node: '${default_class}'" ;;
  *'|WaitForFirstConsumer|'*)
    pvc "${P}-wffc" ""
    inst "${P}-wffc" "{persistent: {name: ${P}-wffc}}"
    node="$(wait_running "${P}-wffc" 180)"
    aff="$(affinity_of "${P}-wffc")"
    phase="$(kubectl -n "$NS" get pvc "${P}-wffc" -o jsonpath='{.status.phase}' 2>/dev/null)"
    if [ -n "$node" ] && [ -z "$aff" ] && [ "$phase" = Bound ]; then
      record PASS "a WaitForFirstConsumer claim binds on the first Pod's node" "node=${node} claim=${phase}"
    else
      record FAIL "a WaitForFirstConsumer claim binds on the first Pod's node" "node=${node:-<not running>} affinity=${aff:-<none>} claim=${phase:-<none>}"
    fi ;;
  *) record SKIP "a WaitForFirstConsumer claim binds on the first Pod's node" "the default class binds immediately: '${default_class}'" ;;
esac

echo "== 5. an unbound claim on an immediate class creates no Pod =="
pvc "${P}-imm" "${P}-immediate"
inst "${P}-imm" "{persistent: {name: ${P}-imm}}"
sleep 40
phase="$(kubectl -n "$NS" get instances.worker.gpustack.ai "${P}-imm" -o jsonpath='{.status.phase}')"
msg="$(kubectl -n "$NS" get instances.worker.gpustack.ai "${P}-imm" -o jsonpath='{.status.phaseMessage}')"
if ! kubectl -n "$NS" get pod "${P}-imm" >/dev/null 2>&1 && [ "$phase" = Starting ] &&
  printf '%s' "$msg" | command grep -qF "PersistentVolumeClaim \"${P}-imm\" is Pending"; then
  record PASS "an unbound immediate claim creates no Pod and says why" "${phase}: ${msg}"
else
  record FAIL "an unbound immediate claim creates no Pod and says why" "pod=$(pod_field "${P}-imm" '{.metadata.name}') phase=${phase:-<none>} message=${msg:-<none>}"
fi

echo "== 6. the escape Setting restores the render from claim names alone =="
set_placement false
pod=""
for _ in $(seq 1 20); do
  pod="$(pod_field "${P}-imm" '{.metadata.name}')"
  [ -n "$pod" ] && break
  sleep 3
done
if [ -n "$pod" ] && [ -z "$(affinity_of "${P}-imm")" ]; then
  record PASS "with the Setting off the waiting Instance's Pod is created" "pod=${pod}"
else
  record FAIL "with the Setting off the waiting Instance's Pod is created" "pod=${pod:-<none>} within 60s"
fi
inst "${P}-off" "{ephemeral: {capacity: 1Gi}}" "[{mountPath: /mnt/data, persistent: {name: ${P}-add}}]"
pod=""
for _ in $(seq 1 20); do
  pod="$(pod_field "${P}-off" '{.metadata.name}')"
  [ -n "$pod" ] && break
  sleep 3
done
aff="$(affinity_of "${P}-off")"
if [ -n "$pod" ] && [ -z "$aff" ]; then
  record PASS "with the Setting off a bound claim's Pod carries no affinity" "pod=${pod}"
else
  record FAIL "with the Setting off a bound claim's Pod carries no affinity" "pod=${pod:-<none>} affinity=${aff:-<none>}"
fi
restore_setting

print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-105] ${FAILS} check(s) FAILED"; exit 1; }
