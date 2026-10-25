#!/usr/bin/env bash
#
# CASE 16 — InstanceTypeFlavor catalog, queue ownership, and CPU-manufacturer awareness   (MUTATING, self-recovering)
#
#   case-16.sh <NS>
#
# Goal:        On the general (CPU) pool, prove the queue-ownership split (InstanceTypeReconciler owns the
#              CQ's existence + schedule labels + teardown; NodeQueueReconciler owns its quota /
#              StopPolicy; NodeFlavorReconciler authors the derived type) and the shape of the
#              aggregated catalog —
#              A: the aggregated, list-only InstanceTypeFlavor resource projects the fleet's pools, the
#                 generic (CPU-only) pool appearing with acceleratable=false;
#              B: deleting a backing ClusterQueue while its InstanceType still lives self-heals — the
#                 reconciler recreates it (a NEW uid) and the chain never stays down;
#              C: deleting an admin InstanceType holds its finalizer until the operator has deleted the
#                 backing ClusterQueue and Kueue has actually removed it (delete-then-wait), then releases;
#              D: the CPU ResourceFlavor is named gpustack--${gKey}-${os}-${arch}-${count}c (the
#                 double-dash finest grain) and always carries a cpuDetail note recording the raw CPU
#                 manufacturer/product/family;
#              E: instance-type-aware-cpu-manufacturer drives the collapse<->split of the aggregated view
#                 while the ResourceFlavor stays the finest, setting-independent grain — awareness OFF
#                 collapses every CPU into one CPU-agnostic row named gpustack--generic (acceleratable=false,
#                 generalGroup=generic); awareness ON replaces it with a real-CPU-keyed row named
#                 gpustack--${gKey} (generalGroup=${gKey}, not "generic"), and the generic row is gone.
# Environment: Any cluster with a materialized general (CPU) pool. No GPU. The ResourceFlavor and the
#              InstanceTypeFlavor catalog exist whenever a node is managed, so this runs anywhere; the
#              catalog recomputes live at list time, so the toggle needs no redeploy. A throwaway admin
#              type carries the teardown probe so the derived chain is untouched.
# Inputs:      All real, nothing mocked —
#              - reads the InstanceTypeFlavor catalog and the CPU ResourceFlavor;
#              - deletes the derived general pool's backing ClusterQueue, then lets it self-heal;
#              - creates + deletes a throwaway admin InstanceType e2e-case16-teardown (generalGroup e2e16td);
#              - patches the gpustack-settings Secret's instance-type-aware-cpu-manufacturer key true/false
#                (the editable-setting store the aggregated apiserver reads, with a <=30s value cache).
# Expected:    - A — >=1 InstanceTypeFlavor row, and a generic (acceleratable=false) one;
#              - B — the ClusterQueue is recreated with a NEW uid while the InstanceType survives;
#              - C — the throwaway's backing ClusterQueue is created, then both it and the InstanceType
#                are removed on delete, and no sample sees the InstanceType gone while its queue remains;
#              - D — a gpustack--...-Nc CPU flavor whose note.gpustack.ai/cpuDetail parses as JSON;
#              - E — awareness off shows exactly the gpustack--generic (generalGroup=generic) row;
#                awareness on shows a gpustack--${gKey} (generalGroup!=generic) row and no generic row.
# Cleanup:     Trap restores the awareness setting to its original value, waits for the catalog to
#              re-collapse so the reconciler's setting cache has flipped back, deletes the derived
#              InstanceTypes (+ their ClusterQueues) created while awareness was on, strips and deletes
#              the throwaway type and its ClusterQueue, and waits for the derived general InstanceType
#              to return to Active. Idempotent, runs on pass AND fail.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

NS="${1:?usage: case-16.sh <NS>}"
# shellcheck disable=SC2034 # Read by the sourced library, which shellcheck does not follow.
SYSTEM_NS="$NS"   # the settings Secret lives in the namespace this case was given
AWARE_KEY=instance-type-aware-cpu-manufacturer
DERIVED_LABEL=schedule.gpustack.ai/derived-from-node
PROBE=e2e-case16-teardown   # throwaway admin InstanceType for the teardown probe

# The Settings store and the helpers that read and write it.

# --- Helpers (defined before the trap so cleanup can use them). ---

# generic_row prints the name of the collapsed, CPU-agnostic catalog row (acceleratable=false,
# generalGroup=="generic"), else empty.
generic_row() {
  kubectl get instancetypeflavors.worker.gpustack.ai -o json 2>/dev/null | python3 -c "
import json,sys
try: items=json.load(sys.stdin).get('items',[])
except Exception: items=[]
for it in items:
    s=it.get('spec',{})
    if not s.get('acceleratable') and s.get('generalGroup')=='generic':
        print(it['metadata']['name']); break
" 2>/dev/null
}

# split_row prints 'name|generalGroup' of a per-CPU catalog row (acceleratable=false, generalGroup a
# real CPU key, not the "generic" sentinel), else empty.
split_row() {
  kubectl get instancetypeflavors.worker.gpustack.ai -o json 2>/dev/null | python3 -c "
import json,sys
try: items=json.load(sys.stdin).get('items',[])
except Exception: items=[]
for it in items:
    s=it.get('spec',{}); gg=s.get('generalGroup','')
    if not s.get('acceleratable') and gg and gg!='generic':
        print(it['metadata']['name']+'|'+gg); break
" 2>/dev/null
}

# derived_its prints the names of the operator-authored (derived-from-node) InstanceTypes.
derived_its() {
  kubectl get instancetypes.worker.gpustack.ai -l "${DERIVED_LABEL}=true" \
    -o jsonpath='{.items[*].metadata.name}' 2>/dev/null
}

# wait_generic waits for the collapsed generic row to appear (rides out the <=30s setting cache).
wait_generic() { for _ in $(seq 1 24); do [ -n "$(generic_row)" ] && return 0; sleep 3; done; return 1; }

# The derived general InstanceType == its backing ClusterQueue name (one isolated CQ per pool).
IT=$(kubectl get instancetypes.worker.gpustack.ai \
  -o jsonpath='{.items[?(@.spec.acceleratable==false)].metadata.name}' 2>/dev/null | tr ' ' '\n' | grep -m1 'gpustack-')
[ -n "$IT" ] || { echo "no general InstanceType found — run case-1 first to materialize the chain"; exit 1; }
echo "general InstanceType / ClusterQueue: ${IT}"

# The CPU ResourceFlavor is keyed by the node's real CPU (gpustack--${gKey}-...-${count}c, plus a
# -fnv64-<16 hex> topology profile on a TAS cluster),
# independent of any collapsed InstanceType name — the finest, setting-independent grain.
CPURF=$(kubectl get resourceflavors.kueue.x-k8s.io -o name 2>/dev/null | grep -E '/gpustack--.*-[0-9]+c(-fnv64-[0-9a-f]{16})?$' | head -1)
[ -n "$CPURF" ] || { echo "no CPU ResourceFlavor found — run case-1 first to materialize the chain"; exit 1; }

# Capture the original setting and the derived types present now, so cleanup puts the setting back
# exactly as it found it and removes only what the aware window created.
settings_save "$AWARE_KEY"
orig="$(setting_get "$AWARE_KEY")"
before_its=" $(derived_its) "

cleanup() {
  echo
  echo "[case-16] cleanup: restoring ${AWARE_KEY}=${orig:-<unset>}, removing the throwaway type and waiting for the derived chain to recover"
  settings_restore
  # Let the setting cache flip back (the catalog re-collapsing is the signal both the aggregated read
  # and the reconciler read have flipped) before removing the aware-named derived types, so the
  # reconciler will not recreate one after we delete it.
  [ "$orig" = "true" ] || wait_generic || true
  for it in $(derived_its); do
    case "$before_its" in
      *" $it "*) : ;;
      *)
        echo "[case-16] removing derived type created during the aware window: ${it}"
        kubectl patch instancetype "$it" --type=merge -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
        kubectl delete instancetype "$it" --wait=false >/dev/null 2>&1 || true
        kubectl delete clusterqueue "$it" --wait=false >/dev/null 2>&1 || true
        ;;
    esac
  done
  # Strip the throwaway's finalizer only if it is stuck Terminating, then delete both objects best-effort.
  kubectl get instancetype "$PROBE" >/dev/null 2>&1 && \
    kubectl patch instancetype "$PROBE" --type=merge -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
  kubectl delete instancetype "$PROBE" --wait=false >/dev/null 2>&1 || true
  kubectl delete clusterqueue "$PROBE" --wait=false >/dev/null 2>&1 || true
  for _ in $(seq 1 50); do
    [ "$(kubectl get instancetype "$IT" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Active" ] && break
    sleep 3
  done
}
trap cleanup EXIT

# --- A. The aggregated InstanceTypeFlavor catalog lists the pools; the generic one is acceleratable=false. ---
n_flavors=$(kubectl get instancetypeflavors.worker.gpustack.ai --no-headers 2>/dev/null | grep -c . || true)
if [ "${n_flavors:-0}" -ge 1 ]; then
  record PASS "InstanceTypeFlavor lists the fleet's pools" "${n_flavors} row(s)"
else
  record FAIL "InstanceTypeFlavor lists the fleet's pools" "no rows — the aggregated catalog must project the ResourceFlavors"
fi
generic=$(kubectl get instancetypeflavors.worker.gpustack.ai \
  -o jsonpath='{.items[?(@.spec.acceleratable==false)].spec.generalGroup}' 2>/dev/null | tr ' ' '\n' | grep -m1 .)
if [ -n "$generic" ]; then
  record PASS "generic pool surfaces as acceleratable=false" "generalGroup=${generic}"
else
  record FAIL "generic pool surfaces as acceleratable=false" "no acceleratable=false InstanceTypeFlavor row"
fi

# --- B. Accidental delete of the backing ClusterQueue self-heals (recreated with a new uid). ---
uid_before=$(kubectl get clusterqueue "$IT" -o jsonpath='{.metadata.uid}' 2>/dev/null)
echo "[case-16] deleting ClusterQueue ${IT} (uid ${uid_before}) while its InstanceType still lives"
kubectl delete clusterqueue "$IT" --wait=false >/dev/null 2>&1 || true
recreated=""
for _ in $(seq 1 50); do
  uid_now=$(kubectl get clusterqueue "$IT" -o jsonpath='{.metadata.uid}' 2>/dev/null)
  [ -n "$uid_now" ] && [ "$uid_now" != "$uid_before" ] && { recreated=1; break; }
  sleep 3
done
# The InstanceType must stay alive throughout (recreation already proves the reconciler saw a live
# type); poll rather than single-shot to ride out a transient empty read from the API.
it_alive=""
for _ in $(seq 1 10); do
  [ -n "$(kubectl get instancetype "$IT" -o name 2>/dev/null)" ] && { it_alive=1; break; }
  sleep 2
done
if [ -n "$recreated" ] && [ -n "$it_alive" ]; then
  record PASS "accidental CQ delete self-heals" "${IT} recreated (uid ${uid_now}), InstanceType survived"
else
  record FAIL "accidental CQ delete self-heals" "recreated='${recreated:-no}' it_alive='${it_alive:-no}' — a live InstanceType must recreate its deleted queue"
fi

# Wait for the recreated queue to refill + report Active before moving on.
for _ in $(seq 1 40); do
  [ "$(kubectl get instancetype "$IT" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Active" ] && break
  sleep 3
done

# --- C. Deleting an InstanceType tears down its ClusterQueue, then releases (delete-then-wait). ---
cat <<EOF | kubectl apply -f - >/dev/null 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: InstanceType
metadata:
  name: ${PROBE}
spec:
  generalGroup: e2e16td
  acceleratable: false
  os: linux
  arch: amd64
  unitResources:
    cpu: "1"
    ram: "2Gi"
  localStorage: "100Gi"
EOF
# The reconciler ensures the name-identical backing ClusterQueue exists.
cq_made=""
for _ in $(seq 1 30); do
  kubectl get clusterqueue "$PROBE" >/dev/null 2>&1 && { cq_made=1; break; }
  sleep 2
done
if [ -n "$cq_made" ]; then
  record PASS "InstanceType creates its backing ClusterQueue" "${PROBE} queue present"
else
  record FAIL "InstanceType creates its backing ClusterQueue" "${PROBE} queue never appeared"
fi

echo "[case-16] deleting InstanceType ${PROBE}; the finalizer must hold until its queue is gone"
kubectl delete instancetype "$PROBE" --wait=false >/dev/null 2>&1 || true
# Every sample also checks the ORDER: an InstanceType that is gone while its queue still exists had
# its finalizer released before the queue was removed. Sampling can miss a short window, so a PASS
# means no out-of-order sample was seen, not that the order was proven.
torn=""
early=""
for _ in $(seq 1 50); do
  it_left=$(kubectl get instancetype "$PROBE" -o name 2>/dev/null)
  cq_left=$(kubectl get clusterqueue "$PROBE" -o name 2>/dev/null)
  [ -z "$it_left" ] && [ -n "$cq_left" ] && early=1
  [ -z "$it_left" ] && [ -z "$cq_left" ] && { torn=1; break; }
  sleep 3
done
if [ -n "$early" ]; then
  record FAIL "delete-then-wait teardown" "${PROBE}: the InstanceType was gone while its ClusterQueue still existed — the finalizer was released before the queue was removed"
elif [ -n "$torn" ]; then
  record PASS "delete-then-wait teardown" "${PROBE}: InstanceType + backing ClusterQueue both removed, and no sample saw the InstanceType gone before its queue"
else
  record FAIL "delete-then-wait teardown" "InstanceType='${it_left:-gone}' ClusterQueue='${cq_left:-gone}' — teardown must delete the queue then release the finalizer"
fi

# --- D. The ResourceFlavor is the finest, double-dash grain and always records its raw CPU detail. ---
rfname=${CPURF#*/}
if echo "$rfname" | grep -qE '^gpustack--.+-[0-9]+c(-fnv64-[0-9a-f]{16})?$'; then
  record PASS "CPU ResourceFlavor is the finest, double-dash grain" "${rfname}"
else
  record FAIL "CPU ResourceFlavor is the finest, double-dash grain" "${rfname} — expected gpustack--\${gKey}-\${os}-\${arch}-\${count}c[-fnv64-<hash>]"
fi

cpudetail=$(kubectl get "$CPURF" -o json 2>/dev/null | python3 -c "
import json,sys
try: a=json.load(sys.stdin).get('metadata',{}).get('annotations',{}) or {}
except Exception: a={}
v=a.get('note.gpustack.ai/cpuDetail','')
if not v: print('MISSING'); raise SystemExit
try: d=json.loads(v)
except Exception: print('BADJSON'); raise SystemExit
print('OK|manufacturer=%s product=%s'%(d.get('manufacturer','') or '<empty>', d.get('product','') or '<empty>'))
" 2>/dev/null)
case "$cpudetail" in
  OK*) record PASS "CPU ResourceFlavor carries a cpuDetail note" "${cpudetail#OK|}" ;;
  *)   record FAIL "CPU ResourceFlavor carries a cpuDetail note" "${cpudetail:-<none>} — a CPU flavor must always record its raw CPU detail" ;;
esac

# --- E. Awareness OFF collapses the CPU pool to one CPU-agnostic generic row. ---
echo "[case-16] awareness OFF: expecting the catalog to collapse the CPU pool to gpustack--generic"
setting_set "$AWARE_KEY" false
gname=""
wait_generic && gname=$(generic_row)
if [ "$gname" = "gpustack--generic" ]; then
  record PASS "awareness off collapses to one generic row" "${gname} (acceleratable=false, generalGroup=generic)"
else
  record FAIL "awareness off collapses to one generic row" "generic row='${gname:-<none>}' — expected gpustack--generic"
fi

# --- F. Awareness ON splits the CPU pool into a real-CPU-keyed row (generic row gone). ---
echo "[case-16] awareness ON: expecting the generic row to split into a real-CPU-keyed row"
setting_set "$AWARE_KEY" true
split=""
for _ in $(seq 1 24); do
  split=$(split_row)
  [ -n "$split" ] && [ -z "$(generic_row)" ] && break
  split=""
  sleep 3
done
if [ -n "$split" ]; then
  record PASS "awareness on splits the CPU pool by manufacturer" "${split%%|*} (generalGroup=${split#*|}, generic row gone)"
else
  record FAIL "awareness on splits the CPU pool by manufacturer" "no real-CPU row (or the generic row lingered) — the catalog must recompute per the setting"
fi

echo
echo "== CASE 16 — InstanceTypeFlavor catalog, queue ownership, and CPU-manufacturer awareness =="
print_rows

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). The InstanceTypeReconciler owns the backing queue's existence"
  echo "(recreate on accidental delete; delete-then-wait teardown) and the InstanceTypeFlavor catalog"
  echo "projects the pools. The ResourceFlavor is the finest, setting-independent grain (always"
  echo "carrying cpuDetail); instance-type-aware-cpu-manufacturer collapses (off) or splits (on) only the"
  echo "aggregated InstanceTypeFlavor/InstanceType/ClusterQueue view."
  exit 1
fi
echo "CASE 16 PASS"
