#!/usr/bin/env bash
#
# CASE 2 — CPU Instance lifecycle, drain, managed toggle, portless Service omission, InstanceTypeFlavor
#          catalog & queue ownership, CPU awareness, node pinning, additional volumes, and host-access gates
#          (MUTATING, self-recovering; 2-node pinning choice & SSH sub-checks auto-skip when absent)
#
#   case-2.sh <NS>
#
# Prerequisites: nodes=1; gpu=none; cluster=any
# Goal:        Verify the full general (CPU-only) Instance, catalog, queue-ownership, node-pinning,
#              additional-volumes, and host-boundary gate contracts across three phases:
#                1. Portless Instance lifecycle, drain & managed toggle: Kueue admits a portless
#                   CPU Instance on the cpu-only queue, status is written with zero Service created,
#                   draining gpustack.ai/managed=false removes the CPU ResourceFlavor, flips the
#                   running Instance to spec.stop=true (stopped, not recreated), keeps the
#                   InstanceType alive with its ClusterQueue emptied and held, and restoring
#                   gpustack.ai/managed=true refills and reactivates the pool;
#                2. InstanceTypeFlavor catalog, queue ownership & CPU awareness: aggregated
#                   InstanceTypeFlavor projects the fleet's pools, out-of-band ClusterQueue deletion
#                   self-heals with a new uid, deleting an admin InstanceType enforces delete-then-wait
#                   finalizer ordering, the CPU ResourceFlavor stays finest-grain with JSON cpuDetail,
#                   and instance-type-aware-cpu-manufacturer toggles collapse <-> split;
#                3. Node pinning, additionalVolumes & host-access gates: spec.nodeName pins via
#                   kubernetes.io/hostname nodeSelector without bypassing Kueue (2-node choice leg
#                   auto-skips on 1-node clusters), persistent / configMap+subPath / secret / readOnly
#                   hostPath mounts render on main only (visible over SSH when ssh client tools exist),
#                   instance-privileged-allowed and instance-host-path-volume-allowed gate new escapes
#                   on create and patch while allowing stop/edit/restart of already-held mounts, and
#                   unknown spec.nodeName is rejected.
# Environment: Any cluster with a materialized general (CPU) pool (run case-1 first) and a default
#              StorageClass for the persistent volume leg. No GPU is required. With 1 schedulable
#              managed node, the "the pin chose the node" sub-check records SKIP cleanly; without
#              ssh/ssh-keygen on the runner, the two SSH sub-checks record SKIP cleanly.
# Inputs:      Real Kubernetes and GPUStack resources in namespace default plus temporary patches to
#              <node>-gpustack-worker NodeFeatures and gpustack-settings Secret keys.
# Expected:    Every applicable check records PASS; 2-node pin choice and SSH sub-checks record SKIP
#              when the cluster or runner lacks them.
# Cleanup:     EXIT trap closes any SSH port-forward, restores gpustack.ai/managed=true on all worker
#              NodeFeatures, deletes all test Instances, InstancePersistentVolume, ConfigMap, Secrets,
#              and throwaway/aware-window InstanceTypes + ClusterQueues, restores all saved Settings,
#              and waits for the general InstanceType to return to Active with non-zero CPU capacity.
set -uo pipefail

# shellcheck disable=SC2034 # Read by _rows-lib.sh.
CASE_ID=2
CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=/dev/null
. "${CASE_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_ssh-lib.sh"

NS="${1:?usage: case-2.sh <NS>}"
# shellcheck disable=SC2034 # Read by _rows-lib.sh setting_* helpers.
SYSTEM_NS="$NS"

AWARE_KEY=instance-type-aware-cpu-manufacturer
PRIV_KEY=instance-privileged-allowed
HOST_KEY=instance-host-path-volume-allowed
DERIVED_LABEL=schedule.gpustack.ai/derived-from-node

INST_PORTLESS=e2e-c2-portless
PROBE_IT=e2e-c2-teardown
PROBE_CQ=""
INST_PINNED=e2e-c2-pinned
INST_GATED=e2e-c2-gated
INST_PLAIN=e2e-c2-plain
CM_VOL=e2e-c2-cm
SEC_VOL=e2e-c2-secret
IPV_VOL=e2e-c2-data
SSH_SECRET=e2e-c2-ssh-key
HOST_DIR=/tmp/gpustack-e2e-c2
LOCAL_PORT=22002
KEYDIR="$(mktemp -d)"

generic_row() {
  kubectl get instancetypeflavors.worker.gpustack.ai -o json 2>/dev/null | python3 -c "
import json, sys
try:
    items = json.load(sys.stdin).get('items', [])
except Exception:
    items = []
for it in items:
    s = it.get('spec', {})
    if not s.get('acceleratable') and s.get('generalGroup') == 'generic':
        print(it['metadata']['name'])
        break
" 2>/dev/null
}

split_row() {
  kubectl get instancetypeflavors.worker.gpustack.ai -o json 2>/dev/null | python3 -c "
import json, sys
try:
    items = json.load(sys.stdin).get('items', [])
except Exception:
    items = []
for it in items:
    s = it.get('spec', {})
    gg = s.get('generalGroup', '')
    if not s.get('acceleratable') and gg and gg != 'generic':
        print(it['metadata']['name'] + '|' + gg)
        break
" 2>/dev/null
}

derived_its() {
  kubectl get instancetypes.worker.gpustack.ai -l "${DERIVED_LABEL}=true" \
    -o jsonpath='{.items[*].metadata.name}' 2>/dev/null
}

wait_generic() {
  for _ in $(seq 1 24); do
    [ -n "$(generic_row)" ] && return 0
    sleep 3
  done
  return 1
}

it_cq() {
  cq_of_instancetype "$NS" "$1"
}

IT="$(kubectl get instancetypes.worker.gpustack.ai \
  -o jsonpath='{.items[?(@.spec.acceleratable==false)].metadata.name}' 2>/dev/null \
  | tr ' ' '\n' | grep -m1 'gpustack-' || true)"
[ -n "$IT" ] || { echo "no general InstanceType found — run case-1 first to materialize the chain"; exit 1; }

RF="$(kubectl get resourceflavors.kueue.x-k8s.io -o name 2>/dev/null \
  | grep -E '/gpustack--.*-[0-9]+c(-fnv64-[0-9a-f]{16})?$' | head -1 || true)"
WORKER_NFS="$(kubectl -n "$NS" get nodefeatures -o name 2>/dev/null | grep -E -- '-gpustack-worker$' || true)"
unit_ram="$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.spec.unitResources.ram}' 2>/dev/null)"

settings_save "$AWARE_KEY" "$PRIV_KEY" "$HOST_KEY"
orig_aware="$(setting_get "$AWARE_KEY")"
before_its=" $(derived_its) "

purge_extra_derived_its() {
  local it
  for it in $(derived_its); do
    case "$before_its" in
      *" $it "*) : ;;
      *)
        echo "[case-2] removing derived InstanceType created during aware window: ${it}"
        kubectl patch instancetype "$it" --type=merge -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
        kubectl delete instancetype "$it" --wait=false >/dev/null 2>&1 || true
        kubectl delete clusterqueue "$it" --wait=false >/dev/null 2>&1 || true
        ;;
    esac
  done
}

# shellcheck disable=SC2317 # Invoked via EXIT trap.
cleanup() {
  local nf phase="" cpucap=""
  echo
  echo "[case-2] cleanup: closing forwards, restoring managed=true and settings, deleting test fixtures"
  ssh_cleanup
  while IFS= read -r nf; do
    [ -n "$nf" ] || continue
    kubectl -n "$NS" patch "$nf" --type=merge \
      -p '{"spec":{"labels":{"gpustack.ai/managed":"true"}}}' >/dev/null 2>&1 || true
  done <<<"$WORKER_NFS"

  kubectl -n default delete instance "$INST_PORTLESS" "$INST_PINNED" "$INST_GATED" "$INST_PLAIN" \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n default delete instancepersistentvolume "$IPV_VOL" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n default delete configmap "$CM_VOL" --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n default delete secret "$SEC_VOL" "$SSH_SECRET" --ignore-not-found >/dev/null 2>&1 || true

  settings_restore
  [ "$orig_aware" = "true" ] || wait_generic || true
  purge_extra_derived_its

  if kubectl get instancetype "$PROBE_IT" >/dev/null 2>&1; then
    kubectl patch instancetype "$PROBE_IT" --type=merge -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
  fi
  kubectl delete instancetype "$PROBE_IT" --wait=false >/dev/null 2>&1 || true
  kubectl delete clusterqueue "$PROBE_IT" --wait=false >/dev/null 2>&1 || true
  if [ -n "$PROBE_CQ" ]; then
    kubectl delete clusterqueue "$PROBE_CQ" --wait=false >/dev/null 2>&1 || true
  fi

  for _ in $(seq 1 50); do
    phase="$(kubectl get instancetype "$IT" -o jsonpath='{.status.phase}' 2>/dev/null)"
    cpucap="$(kubectl get instancetype "$IT" -o jsonpath='{.status.cpu.capacity}' 2>/dev/null)"
    [ "$phase" = "Active" ] && [ -n "$cpucap" ] && [ "$cpucap" != "0" ] && break
    sleep 3
  done
  rm -rf "$KEYDIR" 2>/dev/null || true
  echo "[case-2] ${IT}: phase=${phase:-<none>} cpu.capacity=${cpucap:-<none>}"
}
trap cleanup EXIT

# ===================================================================================================
# Phase 1 — Portless CPU Instance lifecycle, Service omission, pool drain, and managed toggle
# ===================================================================================================
echo
echo "[case-2] === Phase 1: portless CPU Instance lifecycle, drain, and managed toggle ==="
NO_UNIT=""
[ -n "$unit_ram" ] || NO_UNIT="no unit spec on ${IT} — the Instance webhook needs unitRAM to size the Pod"
NO_NODES=""
[ -n "$WORKER_NFS" ] || NO_NODES="no <node>-gpustack-worker NodeFeatures found — the pool cannot be toggled"
NO_RF=""
[ -n "$RF" ] || NO_RF="no CPU ResourceFlavor found — run case-1 first to materialize the chain"

if [ -z "$NO_UNIT" ]; then
  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata: { name: ${INST_PORTLESS}, namespace: default }
spec:
  type: ${IT}
  image: alpine
  command: ["sleep", "86400"]
  volume: { ephemeral: { capacity: 1Gi } }
EOF

  admitted=""
  for _ in $(seq 1 20); do
    a="$(kubectl -n default get workloads.kueue.x-k8s.io \
      -o jsonpath='{range .items[*]}{.metadata.ownerReferences[0].name}{"|"}{.status.conditions[?(@.type=="Admitted")].status}{"\n"}{end}' 2>/dev/null \
      | grep -m1 -x "${INST_PORTLESS}|True" || true)"
    [ -n "$a" ] && { admitted=1; break; }
    sleep 3
  done
  if [ -n "$admitted" ]; then
    record PASS "portless workload admitted" "Workload owned by Pod ${INST_PORTLESS} is Admitted=True"
  else
    record FAIL "portless workload admitted" "Workload owned by Pod ${INST_PORTLESS} never reached Admitted=True"
  fi

  ready=""
  phase0=""
  stop0=""
  for _ in $(seq 1 40); do
    IFS='|' read -r phase0 stop0 <<<"$(kubectl -n default get instance "$INST_PORTLESS" \
      -o jsonpath='{.status.phase}|{.spec.stop}' 2>/dev/null)"
    [ "$phase0" = "Ready" ] && [ "$stop0" != "true" ] && { ready=1; break; }
    sleep 3
  done
  if [ -n "$ready" ]; then
    record PASS "instance running pre-drain" "phase=Ready spec.stop=${stop0:-<unset>}"
  else
    record FAIL "instance running pre-drain" "phase=${phase0:-<none>} spec.stop=${stop0:-<unset>}"
  fi

  case "$phase0" in
    Ready|Starting) record PASS "portless instance status written" "phase=${phase0}" ;;
    *)              record FAIL "portless instance status written" "phase='${phase0:-<EMPTY>}'" ;;
  esac

  if kubectl -n default get svc "$INST_PORTLESS" >/dev/null 2>&1; then
    record FAIL "no Service for portless Instance" "svc/${INST_PORTLESS} exists"
  else
    record PASS "no Service for portless Instance" "no svc/${INST_PORTLESS} created"
  fi

  IT_CQ_P1="$(it_cq "$IT")"
  for _ in $(seq 1 20); do
    rw="$(kubectl get clusterqueue "$IT_CQ_P1" -o jsonpath='{.status.reservingWorkloads}' 2>/dev/null)"
    [ "${rw:-0}" -ge 1 ] && break
    sleep 2
  done
else
  record FAIL "portless workload admitted" "$NO_UNIT"
  record FAIL "instance running pre-drain" "$NO_UNIT"
  record FAIL "portless instance status written" "$NO_UNIT"
  record FAIL "no Service for portless Instance" "$NO_UNIT"
fi

if [ -z "$NO_NODES" ]; then
  echo "[case-2] draining pool via gpustack.ai/managed=false on worker NodeFeatures"
  while IFS= read -r nf; do
    [ -n "$nf" ] || continue
    kubectl -n "$NS" patch "$nf" --type=merge \
      -p '{"spec":{"labels":{"gpustack.ai/managed":"false"}}}' >/dev/null
  done <<<"$WORKER_NFS"

  rf_gone=""
  for _ in $(seq 1 60); do
    n_rf="$(kubectl get resourceflavors.kueue.x-k8s.io -o name 2>/dev/null \
      | grep -cE '/gpustack--.*-[0-9]+c(-fnv64-[0-9a-f]{16})?$' || true)"
    [ "${n_rf:-0}" -eq 0 ] && { rf_gone=1; break; }
    sleep 3
  done
  if [ -n "$rf_gone" ]; then
    record PASS "drain removes pool ResourceFlavor" "no CPU (...-Nc) flavor left"
  else
    record FAIL "drain removes pool ResourceFlavor" "a CPU (...-Nc) flavor persists after managed=false"
  fi

  if [ -z "$NO_UNIT" ]; then
    stopped=""
    s_val=""
    for _ in $(seq 1 40); do
      s_val="$(kubectl -n default get instance "$INST_PORTLESS" -o jsonpath='{.spec.stop}' 2>/dev/null)"
      [ "$s_val" = "true" ] && { stopped=1; break; }
      sleep 3
    done
    if [ -n "$stopped" ]; then
      record PASS "instance STOPPED on drain (not recreated)" "spec.stop=true"
    else
      record FAIL "instance STOPPED on drain (not recreated)" "spec.stop=${s_val:-<unset>}"
    fi
  else
    record FAIL "instance STOPPED on drain (not recreated)" "$NO_UNIT"
  fi

  if [ -z "$NO_RF" ]; then
    gone=""
    for _ in $(seq 1 90); do
      kubectl get "$RF" >/dev/null 2>&1 || { gone=1; break; }
      sleep 3
    done
    if [ -n "$gone" ]; then
      record PASS "flavor deleted on de-manage" "${RF#*/} gone (no drain tombstone)"
    else
      record FAIL "flavor deleted on de-manage" "${RF#*/} still present"
    fi
  else
    record FAIL "flavor deleted on de-manage" "$NO_RF"
  fi

  IT_CQ_DRAIN="$(it_cq "$IT")"
  survived=""
  exists=""
  cqexists=""
  rg=""
  stop=""
  for _ in $(seq 1 60); do
    exists="$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o name 2>/dev/null)"
    cqexists="$(kubectl get clusterqueue "$IT_CQ_DRAIN" -o name 2>/dev/null)"
    read -r rg stop < <(kubectl get clusterqueue "$IT_CQ_DRAIN" -o json 2>/dev/null | python3 -c "
import json, sys
try:
    spec = json.load(sys.stdin).get('spec', {})
    print(len(spec.get('resourceGroups') or []), spec.get('stopPolicy') or 'None')
except Exception:
    print(-1, '?')
" 2>/dev/null)
    if [ -n "$exists" ] && [ -n "$cqexists" ] && [ "$rg" = "0" ] && [ "$stop" != "None" ]; then
      survived=1
      break
    fi
    sleep 3
  done
  if [ -n "$survived" ]; then
    record PASS "InstanceType survives flavor loss; queue emptied and held" \
      "${IT} kept, resourceGroups=0, stopPolicy=${stop}"
  else
    record FAIL "InstanceType survives flavor loss; queue emptied and held" \
      "exists='${exists:-gone}' cqexists='${cqexists:-gone}' resourceGroups='${rg}' stopPolicy='${stop}'"
  fi

  echo "[case-2] restoring gpustack.ai/managed=true on worker NodeFeatures"
  while IFS= read -r nf; do
    [ -n "$nf" ] || continue
    kubectl -n "$NS" patch "$nf" --type=merge \
      -p '{"spec":{"labels":{"gpustack.ai/managed":"true"}}}' >/dev/null 2>&1
  done <<<"$WORKER_NFS"

  reactivated=""
  p_after=""
  rg_after=""
  cpu_after=""
  for _ in $(seq 1 90); do
    p_after="$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.status.phase}' 2>/dev/null)"
    cpu_after="$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.status.cpu.capacity}' 2>/dev/null)"
    rg_after="$(kubectl get clusterqueue "$IT_CQ_DRAIN" -o json 2>/dev/null | python3 -c "
import json, sys
try:
    print(len(json.load(sys.stdin).get('spec', {}).get('resourceGroups') or []))
except Exception:
    print(0)
" 2>/dev/null)"
    if [ "$p_after" = "Active" ] && [ "${rg_after:-0}" -gt 0 ] && [ -n "$cpu_after" ] && [ "$cpu_after" != "0" ]; then
      reactivated=1
      break
    fi
    sleep 3
  done
  if [ -n "$reactivated" ]; then
    record PASS "pool reactivates when nodes return" "${IT} Active, resourceGroups=${rg_after}, cpu.capacity=${cpu_after}"
  else
    record FAIL "pool reactivates when nodes return" "phase='${p_after:-?}' resourceGroups='${rg_after:-0}' cpu.capacity='${cpu_after:-0}'"
  fi
else
  record FAIL "drain removes pool ResourceFlavor" "$NO_NODES"
  record FAIL "instance STOPPED on drain (not recreated)" "$NO_NODES"
  record FAIL "flavor deleted on de-manage" "$NO_NODES"
  record FAIL "InstanceType survives flavor loss; queue emptied and held" "$NO_NODES"
  record FAIL "pool reactivates when nodes return" "$NO_NODES"
fi

kubectl -n default delete instance "$INST_PORTLESS" --ignore-not-found >/dev/null 2>&1 || true

# ===================================================================================================
# Phase 2 — InstanceTypeFlavor catalog, queue ownership, and CPU-manufacturer awareness
# ===================================================================================================
echo
echo "[case-2] === Phase 2: InstanceTypeFlavor catalog, queue ownership, and CPU awareness ==="
IT_CQ="$(it_cq "$IT")"
CPURF="$(kubectl get resourceflavors.kueue.x-k8s.io -o name 2>/dev/null \
  | grep -E '/gpustack--.*-[0-9]+c(-fnv64-[0-9a-f]{16})?$' | head -1 || true)"

n_flavors="$(kubectl get instancetypeflavors.worker.gpustack.ai --no-headers 2>/dev/null | grep -c . || true)"
if [ "${n_flavors:-0}" -ge 1 ]; then
  record PASS "InstanceTypeFlavor lists fleet pools" "${n_flavors} row(s)"
else
  record FAIL "InstanceTypeFlavor lists fleet pools" "no rows in instancetypeflavors"
fi

generic_gg="$(kubectl get instancetypeflavors.worker.gpustack.ai \
  -o jsonpath='{.items[?(@.spec.acceleratable==false)].spec.generalGroup}' 2>/dev/null | tr ' ' '\n' | grep -m1 . || true)"
if [ -n "$generic_gg" ]; then
  record PASS "generic pool surfaces as acceleratable=false" "generalGroup=${generic_gg}"
else
  record FAIL "generic pool surfaces as acceleratable=false" "no acceleratable=false InstanceTypeFlavor row"
fi

uid_before="$(kubectl get clusterqueue "$IT_CQ" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
kubectl delete clusterqueue "$IT_CQ" --wait=false >/dev/null 2>&1 || true
recreated=""
uid_now=""
for _ in $(seq 1 50); do
  uid_now="$(kubectl get clusterqueue "$IT_CQ" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
  [ -n "$uid_now" ] && [ "$uid_now" != "$uid_before" ] && { recreated=1; break; }
  sleep 3
done
it_alive=""
for _ in $(seq 1 10); do
  [ -n "$(kubectl get instancetype "$IT" -o name 2>/dev/null)" ] && { it_alive=1; break; }
  sleep 2
done
if [ -n "$recreated" ] && [ -n "$it_alive" ]; then
  record PASS "accidental CQ delete self-heals" "${IT_CQ} recreated (uid ${uid_now}), InstanceType survived"
else
  record FAIL "accidental CQ delete self-heals" "recreated='${recreated:-no}' it_alive='${it_alive:-no}'"
fi

for _ in $(seq 1 40); do
  [ "$(kubectl get instancetype "$IT" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Active" ] && break
  sleep 3
done

cat <<EOF | kubectl apply -f - >/dev/null 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: InstanceType
metadata:
  name: ${PROBE_IT}
spec:
  generalGroup: e2ec2td
  acceleratable: true
  acceleratorGroup: nvidia-e2ec2td
  os: linux
  arch: amd64
  unitResources:
    cpu: "1"
    ram: "2Gi"
  localStorage: "100Gi"
EOF

cq_made=""
for _ in $(seq 1 30); do
  PROBE_CQ="$(it_cq "$PROBE_IT")"
  if [ -n "$PROBE_CQ" ] && kubectl get clusterqueue "$PROBE_CQ" >/dev/null 2>&1; then
    cq_made=1
    break
  fi
  sleep 2
done
if [ -n "$cq_made" ]; then
  record PASS "InstanceType creates backing ClusterQueue" "${PROBE_CQ} present"
else
  record FAIL "InstanceType creates backing ClusterQueue" "${PROBE_IT} backing queue never appeared"
fi

kubectl delete instancetype "$PROBE_IT" --wait=false >/dev/null 2>&1 || true
torn=""
early=""
it_left=""
cq_left=""
for _ in $(seq 1 50); do
  it_left="$(kubectl get instancetype "$PROBE_IT" -o name 2>/dev/null)"
  cq_left="$(kubectl get clusterqueue "$PROBE_CQ" -o name 2>/dev/null)"
  [ -z "$it_left" ] && [ -n "$cq_left" ] && early=1
  [ -z "$it_left" ] && [ -z "$cq_left" ] && { torn=1; break; }
  sleep 3
done
if [ -n "$early" ]; then
  record FAIL "delete-then-wait teardown" "${PROBE_IT} disappeared while ${PROBE_CQ} still existed"
elif [ -n "$torn" ]; then
  record PASS "delete-then-wait teardown" "${PROBE_IT} and ${PROBE_CQ} both removed in order"
else
  record FAIL "delete-then-wait teardown" "InstanceType='${it_left:-gone}' ClusterQueue='${cq_left:-gone}'"
fi

rfname="${CPURF#*/}"
if printf '%s' "$rfname" | grep -qE '^gpustack--.+-[0-9]+c(-fnv64-[0-9a-f]{16})?$'; then
  record PASS "CPU ResourceFlavor is double-dash finest grain" "$rfname"
else
  record FAIL "CPU ResourceFlavor is double-dash finest grain" "got '${rfname:-<none>}'"
fi

cpudetail="$(kubectl get "$CPURF" -o json 2>/dev/null | python3 -c "
import json, sys
try:
    a = json.load(sys.stdin).get('metadata', {}).get('annotations', {}) or {}
except Exception:
    a = {}
v = a.get('note.gpustack.ai/cpuDetail', '')
if not v:
    print('MISSING')
    raise SystemExit
try:
    d = json.loads(v)
except Exception:
    print('BADJSON')
    raise SystemExit
print('OK|manufacturer=%s product=%s' % (d.get('manufacturer', '') or '<empty>', d.get('product', '') or '<empty>'))
" 2>/dev/null)"
case "$cpudetail" in
  OK*) record PASS "CPU ResourceFlavor carries cpuDetail note" "${cpudetail#OK|}" ;;
  *)   record FAIL "CPU ResourceFlavor carries cpuDetail note" "${cpudetail:-<none>}" ;;
esac

setting_set "$AWARE_KEY" false
gname=""
wait_generic && gname="$(generic_row)"
if [ "$gname" = "gpustack--generic" ]; then
  record PASS "awareness off collapses to generic row" "${gname} (generalGroup=generic)"
else
  record FAIL "awareness off collapses to generic row" "got '${gname:-<none>}'"
fi

setting_set "$AWARE_KEY" true
split=""
for _ in $(seq 1 24); do
  split="$(split_row)"
  [ -n "$split" ] && [ -z "$(generic_row)" ] && break
  split=""
  sleep 3
done
if [ -n "$split" ]; then
  record PASS "awareness on splits CPU pool by manufacturer" "${split%%|*} (generalGroup=${split#*|})"
else
  record FAIL "awareness on splits CPU pool by manufacturer" "no split CPU row or generic row lingered"
fi

# Restore CPU awareness before Phase 3 so Phase 3 schedules cleanly against ${IT}.
if [ -z "$orig_aware" ]; then
  setting_unset "$AWARE_KEY"
else
  setting_set "$AWARE_KEY" "$orig_aware"
fi
[ "$orig_aware" = "true" ] || wait_generic || true
purge_extra_derived_its
for _ in $(seq 1 40); do
  [ "$(kubectl get instancetype "$IT" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Active" ] && break
  sleep 3
done

# ===================================================================================================
# Phase 3 — Node pinning, additional volumes, SSH mount visibility, and host-access gates
# ===================================================================================================
echo
echo "[case-2] === Phase 3: node pinning, additionalVolumes, and host-access gates ==="

NODES_LIST=""
for _n in $(kubectl get nodes -l gpustack.ai/managed=true \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null); do
  [ "$(kubectl get node "$_n" -o jsonpath='{.spec.unschedulable}' 2>/dev/null)" = "true" ] && continue
  NODES_LIST="${NODES_LIST}${_n}"$'\n'
done
NODE_COUNT="$(printf '%s' "$NODES_LIST" | grep -c . || true)"
PIN_NODE="$(printf '%s' "$NODES_LIST" | grep . | tail -1 || true)"
PIN_HOSTNAME=""
[ -n "$PIN_NODE" ] && PIN_HOSTNAME="$(kubectl get node "$PIN_NODE" -o jsonpath='{.metadata.labels.kubernetes\.io/hostname}' 2>/dev/null)"

kubectl -n default delete configmap "$CM_VOL" --ignore-not-found >/dev/null 2>&1
kubectl -n default create configmap "$CM_VOL" --from-literal=alpha=ALPHA_VALUE --from-literal=beta=BETA_VALUE >/dev/null
kubectl -n default delete secret "$SEC_VOL" --ignore-not-found >/dev/null 2>&1
kubectl -n default create secret generic "$SEC_VOL" --from-literal=token=SECRET_VALUE >/dev/null
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1
kind: InstancePersistentVolume
metadata: { name: ${IPV_VOL}, namespace: default }
spec:
  capacity: 1Gi
  accessMode: ReadWriteOnce
EOF

HAS_SSH=1
for _tool in ssh ssh-keygen; do
  command -v "$_tool" >/dev/null 2>&1 || HAS_SSH=""
done
if [ -n "$HAS_SSH" ]; then
  ssh_setup_key_secret default "$SSH_SECRET" "${KEYDIR}/id"
fi

setting_set "$HOST_KEY" true
ssh_line=""
[ -n "$HAS_SSH" ] && ssh_line="  sshPublicKey: { name: ${SSH_SECRET} }"

created=""
if [ -n "$PIN_NODE" ] && [ -n "$PIN_HOSTNAME" ]; then
  for _ in $(seq 1 20); do
    if cat <<EOF | kubectl apply -f - >/dev/null 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: Instance
metadata: { name: ${INST_PINNED}, namespace: default }
spec:
  type: ${IT}
  nodeName: ${PIN_NODE}
  image: ubuntu:24.04
  command: ["sleep", "infinity"]
  resources: { cpu: "1", ram: "2Gi", localStorage: "10Gi" }
${ssh_line}
  volume: { ephemeral: { capacity: 10Gi } }
  volumeMount: /workspace
  additionalVolumes:
    - mountPath: /mnt/data
      persistent: { name: ${IPV_VOL} }
    - mountPath: /mnt/cm/alpha
      subPath: alpha
      configMap: { name: ${CM_VOL} }
    - mountPath: /mnt/secret
      secret: { name: ${SEC_VOL} }
    - mountPath: /mnt/host
      readOnly: true
      hostPath: { path: ${HOST_DIR}, type: DirectoryOrCreate }
EOF
    then
      created=1
      break
    fi
    sleep 3
  done
fi

if [ -z "$created" ]; then
  record FAIL "pinned Instance with four additional volumes created" "create rejected on ${IT} (node=${PIN_NODE:-<none>})"
else
  record PASS "pinned Instance with four additional volumes created" "${INST_PINNED} accepted on ${IT}"

  POD="$INST_PINNED"
  sel_pairs=""
  for _ in $(seq 1 30); do
    sel_pairs="$(kubectl -n default get pod "$POD" -o json 2>/dev/null \
      | jq -r '(.spec.nodeSelector // {}) | to_entries[] | "\(.key)=\(.value)"' 2>/dev/null | grep . || true)"
    [ -n "$sel_pairs" ] && break
    sleep 3
  done
  flavor_pairs="$(kubectl get resourceflavor -o json 2>/dev/null \
    | jq -r '.items[] | (.spec.nodeLabels // {}) | to_entries[] | "\(.key)=\(.value)"' 2>/dev/null | grep . || true)"
  host_pairs="$(printf '%s\n' "$sel_pairs" | grep '^kubernetes\.io/hostname=' || true)"
  unexplained=""
  while IFS= read -r p; do
    [ -z "$p" ] && continue
    case "$p" in kubernetes.io/hostname=*) continue ;; esac
    printf '%s\n' "$flavor_pairs" | grep -qxF "$p" || unexplained="${unexplained}${p} "
  done <<<"$sel_pairs"

  if [ "$host_pairs" = "kubernetes.io/hostname=${PIN_HOSTNAME}" ] && [ -z "$unexplained" ]; then
    record PASS "pin survives admission as the one operator-set selector" \
      "kubernetes.io/hostname=${PIN_HOSTNAME}; all other entries are flavor nodeLabels"
  else
    record FAIL "pin survives admission as the one operator-set selector" \
      "hostname='${host_pairs:-<none>}' unexplained='${unexplained:-none}'"
  fi

  ran_on=""
  for _ in $(seq 1 40); do
    ran_on="$(kubectl -n default get pod "$POD" -o jsonpath='{.spec.nodeName}' 2>/dev/null)"
    [ -n "$ran_on" ] && break
    sleep 5
  done
  if [ "$ran_on" = "$PIN_NODE" ]; then
    record PASS "Pod lands on the pinned node" "spec.nodeName=${ran_on}"
  else
    record FAIL "Pod lands on the pinned node" "expected ${PIN_NODE}, got '${ran_on:-<unscheduled>}'"
  fi

  if [ "${NODE_COUNT:-0}" -ge 2 ]; then
    if [ "$ran_on" = "$PIN_NODE" ]; then
      record PASS "the pin chose the node" "${NODE_COUNT} candidates, landed on ${PIN_NODE}"
    else
      record FAIL "the pin chose the node" "${NODE_COUNT} candidates, landed on '${ran_on}'"
    fi
  else
    record SKIP "the pin chose the node" "only ${NODE_COUNT} schedulable managed node on cluster"
  fi

  admitted_pin=""
  for _ in $(seq 1 30); do
    a_pin="$(kubectl -n default get workloads.kueue.x-k8s.io \
      -o jsonpath='{range .items[*]}{.metadata.ownerReferences[0].name}{"|"}{.status.conditions[?(@.type=="Admitted")].status}{"\n"}{end}' 2>/dev/null \
      | grep -m1 "^${POD}|True$" || true)"
    [ -n "$a_pin" ] && { admitted_pin=1; break; }
    sleep 3
  done
  if [ -n "$admitted_pin" ]; then
    record PASS "pinned workload admitted by Kueue" "Admitted=True"
  else
    record FAIL "pinned workload admitted by Kueue" "no Admitted Workload for ${POD}"
  fi

  running_pin=""
  want_ready="true true "
  [ -z "$HAS_SSH" ] && want_ready="true "
  phase_pin=""
  readies_pin=""
  for _ in $(seq 1 60); do
    phase_pin="$(kubectl -n default get pod "$POD" -o jsonpath='{.status.phase}' 2>/dev/null)"
    readies_pin="$(kubectl -n default get pod "$POD" -o jsonpath='{range .status.containerStatuses[*]}{.ready} {end}' 2>/dev/null)"
    if [ "$phase_pin" = "Running" ] && [ "$readies_pin" = "$want_ready" ]; then
      running_pin=1
      break
    fi
    sleep 5
  done

  if [ -z "$running_pin" ]; then
    record FAIL "pinned Instance reaches Running" "phase='${phase_pin:-<none>}' readies='${readies_pin:-<none>}'"
  else
    record PASS "pinned Instance reaches Running" "${POD} Running on ${ran_on}"

    mounts="$(kubectl -n default get pod "$POD" \
      -o jsonpath='{range .spec.containers[?(@.name=="main")].volumeMounts[*]}{.name}:{.mountPath}:{.readOnly}:{.subPath}{"\n"}{end}' 2>/dev/null)"
    expect_mounts="additional-0:/mnt/data::
additional-1:/mnt/cm/alpha::alpha
additional-2:/mnt/secret::
additional-3:/mnt/host:true:"
    got_mounts="$(printf '%s\n' "$mounts" | grep '^additional-' || true)"
    if [ "$got_mounts" = "$expect_mounts" ]; then
      record PASS "four additional mounts on main" "additional-0..3 at paths with readOnly/subPath"
    else
      record FAIL "four additional mounts on main" "unexpected mounts: $(printf '%s' "$got_mounts" | tr '\n' ' ')"
    fi

    ws="$(printf '%s\n' "$mounts" | grep -m1 '^workspace:' || true)"
    if [ "$ws" = "workspace:/workspace::" ]; then
      record PASS "workspace mount unchanged" "$ws"
    else
      record FAIL "workspace mount unchanged" "got '${ws:-<missing>}'"
    fi

    vols="$(kubectl -n default get pod "$POD" \
      -o go-template='{{range .spec.volumes}}{{.name}}={{if .persistentVolumeClaim}}pvc:{{.persistentVolumeClaim.claimName}}{{else if .configMap}}cm:{{.configMap.name}}{{else if .secret}}secret:{{.secret.secretName}}{{else if .hostPath}}host:{{.hostPath.path}}{{else}}other{{end}} {{end}}' 2>/dev/null)"
    for want in "additional-0=pvc:${IPV_VOL}" "additional-1=cm:${CM_VOL}" "additional-2=secret:${SEC_VOL}" "additional-3=host:${HOST_DIR}"; do
      case "$vols" in
        *"$want"*) record PASS "volume source rendered" "$want" ;;
        *)         record FAIL "volume source rendered" "missing ${want} in ${vols}" ;;
      esac
    done

    if [ -n "$HAS_SSH" ]; then
      sshd_extra="$(kubectl -n default get pod "$POD" \
        -o jsonpath='{range .spec.containers[?(@.name=="sshd")].volumeMounts[*]}{.name}{"\n"}{end}' 2>/dev/null \
        | grep -c '^additional-' || true)"
      if [ "${sshd_extra:-0}" -eq 0 ]; then
        record PASS "sidecar spec untouched" "sshd carries 0 additional-* mounts"
      else
        record FAIL "sidecar spec untouched" "sshd carries ${sshd_extra} additional-* mount(s)"
      fi
    else
      record SKIP "sidecar spec untouched" "no ssh client tools on runner"
    fi

    exec_main() { kubectl -n default exec "$POD" -c main -- sh -c "$1" 2>/dev/null; }

    w="$(exec_main 'touch /mnt/data/probe && echo OK' || true)"
    if [ "$w" = "OK" ]; then
      record PASS "persistent mount is writable" "/mnt/data accepts a write"
    else
      record FAIL "persistent mount is writable" "write to /mnt/data failed ('${w:-<no output>}')"
    fi

    sub="$(exec_main 'cat /mnt/cm/alpha' || true)"
    if [ "$sub" = "ALPHA_VALUE" ]; then
      record PASS "subPath exposes only the named key" "/mnt/cm/alpha == ALPHA_VALUE"
    else
      record FAIL "subPath exposes only the named key" "/mnt/cm/alpha == '${sub:-<empty>}'"
    fi

    beta="$(exec_main 'ls /mnt/cm 2>/dev/null | grep -c "^beta$"' || true)"
    if [ "${beta:-0}" -eq 0 ]; then
      record PASS "subPath hides the other keys" "beta not present beside mount"
    else
      record FAIL "subPath hides the other keys" "beta is visible"
    fi

    tok="$(exec_main 'cat /mnt/secret/token' || true)"
    if [ "$tok" = "SECRET_VALUE" ]; then
      record PASS "secret mount readable" "/mnt/secret/token == SECRET_VALUE"
    else
      record FAIL "secret mount readable" "/mnt/secret/token == '${tok:-<empty>}'"
    fi

    ro="$(exec_main 'touch /mnt/host/probe >/dev/null 2>&1; echo $?' || true)"
    if [ -n "$ro" ] && [ "$ro" != "0" ]; then
      record PASS "readOnly mount refuses a write" "touch /mnt/host/probe exited ${ro}"
    else
      record FAIL "readOnly mount refuses a write" "touch /mnt/host/probe exited '${ro:-<no output>}'"
    fi

    if [ -n "$HAS_SSH" ]; then
      ssh_open_port_forward default "pod/${POD}" 22 "$LOCAL_PORT"
      over_ssh="$(ssh_exec "${KEYDIR}/id" "$LOCAL_PORT" 'cat /mnt/cm/alpha' 2>/dev/null | tr -d '\r\n' || true)"
      ssh_cleanup
      if [ "$over_ssh" = "ALPHA_VALUE" ]; then
        record PASS "additional mount visible over SSH" "ssh 'cat /mnt/cm/alpha' == ALPHA_VALUE"
      else
        record FAIL "additional mount visible over SSH" "got '${over_ssh:-<empty>}'"
      fi
    else
      record SKIP "additional mount visible over SSH" "no ssh client tools on runner"
    fi
  fi
fi

gate_create() {
  local name="$1" priv="$2" host="$3" out host_block=""
  if [ "$host" = "true" ]; then
    host_block="$(printf '  additionalVolumes:\n    - mountPath: /mnt/host\n      hostPath: { path: %s, type: DirectoryOrCreate }' "$HOST_DIR")"
  fi
  out="$(cat <<EOF | kubectl apply -f - 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: Instance
metadata: { name: ${name}, namespace: default }
spec:
  type: ${IT}
  image: ubuntu:24.04
  command: ["sleep", "infinity"]
  privileged: ${priv}
  resources: { cpu: "1", ram: "2Gi", localStorage: "10Gi" }
  volume: { ephemeral: { capacity: 1Gi } }
${host_block}
EOF
)"
  case "$out" in
    *created*|*configured*|*unchanged*) echo accepted ;;
    *spec.privileged*|*spec.additionalVolumes*hostPath*|*"$PRIV_KEY"*|*"$HOST_KEY"*) echo "gated: $out" ;;
    *) echo "other: $out" ;;
  esac
}

await_verdict() {
  local want="$1" priv="$2" host="$3" v=""
  for _ in $(seq 1 20); do
    kubectl -n default delete instance "$INST_GATED" --ignore-not-found >/dev/null 2>&1
    v="$(gate_create "$INST_GATED" "$priv" "$host")"
    case "$v" in
      accepted) [ "$want" = "accepted" ] && break ;;
      gated:*)  [ "$want" = "gated" ] && break ;;
    esac
    sleep 3
  done
  kubectl -n default delete instance "$INST_GATED" --ignore-not-found >/dev/null 2>&1
  printf '%s' "$v"
}

setting_set "$PRIV_KEY" false
setting_set "$HOST_KEY" false
v_priv="$(await_verdict gated true false)"
case "$v_priv" in
  gated:*) record PASS "privileged create rejected while gated off" "${PRIV_KEY}=false rejects spec.privileged" ;;
  *)       record FAIL "privileged create rejected while gated off" "verdict '${v_priv}'" ;;
esac

v_host="$(await_verdict gated false true)"
case "$v_host" in
  gated:*) record PASS "hostPath create rejected while gated off" "${HOST_KEY}=false rejects hostPath" ;;
  *)       record FAIL "hostPath create rejected while gated off" "verdict '${v_host}'" ;;
esac

setting_set "$HOST_KEY" true
v_host_on="$(await_verdict accepted false true)"
if [ "$v_host_on" = "accepted" ]; then
  record PASS "hostPath create admitted once allowed" "${HOST_KEY}=true admits hostPath"
else
  record FAIL "hostPath create admitted once allowed" "verdict '${v_host_on}'"
fi

v_priv_still="$(await_verdict gated true false)"
case "$v_priv_still" in
  gated:*) record PASS "each gate covers only its own field" "${HOST_KEY}=true does not allow privileged" ;;
  *)       record FAIL "each gate covers only its own field" "verdict '${v_priv_still}'" ;;
esac

setting_set "$HOST_KEY" false
settings_settle

gate_named() {
  case "$1" in
    *"$PRIV_KEY"*|*"$HOST_KEY"*|*privileged*not*allowed*|*host*path*not*allowed*) return 0 ;;
  esac
  return 1
}

judge_update() {
  if gate_named "$2"; then
    record FAIL "$1" "rejected by gate on already-held escape: $2"
  else
    record FAIL "$1" "rejected, but not by a gate: $2"
  fi
}

if [ -n "$created" ]; then
  out_stop="$(kubectl -n default patch instance "$INST_PINNED" --type=merge -p '{"spec":{"stop":true}}' 2>&1)"
  case "$out_stop" in
    *patched*) record PASS "existing Instance stops with the gate off" "spec.stop=true accepted" ;;
    *)         judge_update "existing Instance stops with the gate off" "$out_stop" ;;
  esac

  phase_stopped=""
  for _ in $(seq 1 40); do
    phase_stopped="$(kubectl -n default get instance "$INST_PINNED" -o jsonpath='{.status.phase}' 2>/dev/null)"
    [ "$phase_stopped" = "Stopped" ] && break
    sleep 5
  done
  if [ "$phase_stopped" = "Stopped" ]; then
    record PASS "stopping settles to Stopped" "phase=Stopped"
  else
    record FAIL "stopping settles to Stopped" "phase='${phase_stopped:-<none>}'"
  fi

  out_edit="$(kubectl -n default patch instance "$INST_PINNED" --type=merge -p '{"spec":{"volumeMount":"/workspace2"}}' 2>&1)"
  case "$out_edit" in
    *patched*) record PASS "stopped Instance still editable with the gate off" "spec.volumeMount edited while Stopped" ;;
    *)         judge_update "stopped Instance still editable with the gate off" "$out_edit" ;;
  esac

  out_widen="$(kubectl -n default patch instance "$INST_PINNED" --type=merge -p "{\"spec\":{\"additionalVolumes\":[
    {\"mountPath\":\"/mnt/host\",\"readOnly\":true,\"hostPath\":{\"path\":\"${HOST_DIR}\",\"type\":\"DirectoryOrCreate\"}},
    {\"mountPath\":\"/mnt/host-rw\",\"hostPath\":{\"path\":\"${HOST_DIR}\",\"type\":\"DirectoryOrCreate\"}}]}}" 2>&1)"
  case "$out_widen" in
    *patched*|*configured*|*unchanged*)
      record FAIL "widening a held host path faces the gate" "ACCEPTED writable mount with gate off"
      kubectl -n default patch instance "$INST_PINNED" --type=merge -p "{\"spec\":{\"additionalVolumes\":[
        {\"mountPath\":\"/mnt/host\",\"readOnly\":true,\"hostPath\":{\"path\":\"${HOST_DIR}\",\"type\":\"DirectoryOrCreate\"}}]}}" >/dev/null 2>&1 || true
      ;;
    *"$HOST_KEY"*)
      record PASS "widening a held host path faces the gate" "writable mount rejected by ${HOST_KEY}"
      ;;
    *)
      record FAIL "widening a held host path faces the gate" "rejected, but not by ${HOST_KEY}: ${out_widen}"
      ;;
  esac

  out_restart="$(kubectl -n default patch instance "$INST_PINNED" --type=merge -p '{"spec":{"stop":false}}' 2>&1)"
  case "$out_restart" in
    *patched*) record PASS "existing Instance restarts with the gate off" "spec.stop=false accepted" ;;
    *)         judge_update "existing Instance restarts with the gate off" "$out_restart" ;;
  esac
fi

cat <<EOF | kubectl apply -f - >/dev/null 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: Instance
metadata: { name: ${INST_PLAIN}, namespace: default }
spec:
  type: ${IT}
  stop: true
  image: ubuntu:24.04
  command: ["sleep", "infinity"]
  resources: { cpu: "1", ram: "2Gi", localStorage: "10Gi" }
  volume: { ephemeral: { capacity: 1Gi } }
EOF

if ! kubectl -n default get instance "$INST_PLAIN" >/dev/null 2>&1; then
  record SKIP "escalation by patch is refused" "plain stopped Instance could not be created"
else
  judge_escalation() {
    case "$3" in
      *patched*|*configured*|*unchanged*)
        record FAIL "$1" "ACCEPTED escape via patch with gate off" ;;
      *"$2"*)
        record PASS "$1" "rejected on $2" ;;
      *)
        record FAIL "$1" "rejected, but not on $2: $3" ;;
    esac
  }

  out_ep="$(kubectl -n default patch instance "$INST_PLAIN" --type=merge -p '{"spec":{"privileged":true}}' 2>&1)"
  judge_escalation "privileged cannot be acquired by patch" "spec.privileged" "$out_ep"

  out_eh="$(kubectl -n default patch instance "$INST_PLAIN" --type=merge \
    -p '{"spec":{"additionalVolumes":[{"mountPath":"/mnt/escalate","hostPath":{"path":"/"}}]}}' 2>&1)"
  judge_escalation "a hostPath cannot be acquired by patch" "spec.additionalVolumes" "$out_eh"

  out_eb="$(kubectl -n default patch instance "$INST_PLAIN" --type=merge \
    -p '{"spec":{"stop":false,"privileged":true,"additionalVolumes":[{"mountPath":"/mnt/escalate","hostPath":{"path":"/"}}]}}' 2>&1)"
  judge_escalation "both cannot be acquired in the patch that restarts it" "spec.privileged" "$out_eb"
fi

out_badnode="$(cat <<EOF | kubectl apply -f - 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: Instance
metadata: { name: ${INST_GATED}, namespace: default }
spec:
  type: ${IT}
  nodeName: gpustack-e2e-no-such-node
  image: ubuntu:24.04
  command: ["sleep", "infinity"]
  resources: { cpu: "1", ram: "2Gi", localStorage: "10Gi" }
  volume: { ephemeral: { capacity: 1Gi } }
EOF
)"
case "$out_badnode" in
  *created*|*configured*|*unchanged*)
    kubectl -n default delete instance "$INST_GATED" --ignore-not-found >/dev/null 2>&1
    record FAIL "unknown pinned node rejected" "ACCEPTED nonexistent spec.nodeName"
    ;;
  *spec.nodeName*)
    record PASS "unknown pinned node rejected" "create rejected on spec.nodeName"
    ;;
  *)
    record FAIL "unknown pinned node rejected" "rejected, but not on spec.nodeName: ${out_badnode}"
    ;;
esac

echo
echo "== CASE 2 — CPU Instance lifecycle, drain, catalog, awareness, node pinning & volumes =="
print_rows
if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s)."
  exit 1
fi
echo "CASE 2 PASS"
