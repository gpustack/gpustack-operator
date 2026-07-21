#!/usr/bin/env bash
#
# CASE 17 — InstanceType declarative admission   (MUTATING for the update probes, self-recovering)
#
#   case-17.sh <NS>
#
# Goal:        The InstanceType admission contract —
#              A: required inputs are enforced on CREATE (an accelerated InstanceType missing
#                 spec.acceleratorGroup, or any type missing its unit spec unitResources /
#                 localStorage, is rejected by the validating webhook; spec.generalGroup is NOT
#                 required — the Default webhook fills an empty one with the "generic" sentinel);
#              B: the Default (mutating) webhook stamps, from the spec identity, the
#                 feature.gpustack.ai/acceleratable boolean + kubernetes.io/os|arch schedule labels
#                 AND the queue-entrance label, so a stored type is selectable and reverse-lookup-able
#                 (the Pod webhook reads per-card VRAM off it) from day one;
#              C: the whole spec is frozen on UPDATE (changing unitResources / localStorage / generalGroup
#                 is rejected — only displayName / description / inactive stay editable).
# Environment: Any cluster with a materialized general pool (the webhook chain must be up). No GPU.
#              CPU-manufacturer awareness is assumed off (the default), so the non-accelerated stamp is
#              the acceleratable boolean + os/arch, not a general.* CPU key.
#              A/B use server-side dry-run (nothing persisted); C uses REAL patches on a throwaway type,
#              because server-side dry-run does not reliably exercise update admission for a merge patch.
# Inputs:      Nothing mocked —
#              - A/B: dry-run InstanceType manifests (accelerated, missing acceleratorGroup; non-accel,
#                missing unit spec; a valid non-accel one);
#              - C: a real throwaway InstanceType e2e-case17-upd (generalGroup e2e17upd) patched three
#                ways — unitResources.cpu, localStorage, and generalGroup.
# Expected:    - A — CREATE rejects the accelerated-missing-acceleratorGroup and the missing-unit-spec
#                manifests;
#              - B — the Default webhook stamps feature.gpustack.ai/acceleratable=false + os=linux +
#                arch=amd64 + a gpustack- queue-entrance label;
#              - C — the unitResources, localStorage AND generalGroup patches are all REJECTED (immutable)
#                and do not persist.
# Cleanup:     Trap force-strips the throwaway's finalizer if present, deletes the throwaway type + its
#              ClusterQueue.
set -uo pipefail

NS="${1:?usage: case-17.sh <NS>}"
ENTRANCE="schedule.gpustack.ai/queue-entrance"
UPD=e2e-case17-upd   # throwaway persisted InstanceType for the UPDATE (immutability) probes

# Sanity: the general chain must be materialized (the webhook is serving).
IT=$(kubectl get instancetypes.worker.gpustack.ai \
  -o jsonpath='{.items[?(@.spec.acceleratable==false)].metadata.name}' 2>/dev/null | tr ' ' '\n' | grep -m1 'gpustack-')
[ -n "$IT" ] || { echo "no general InstanceType found — run case-1 first to materialize the chain"; exit 1; }

cleanup() {
  echo
  echo "[case-17] cleanup: removing the throwaway type ${UPD}"
  kubectl get instancetype "$UPD" >/dev/null 2>&1 && \
    kubectl patch instancetype "$UPD" --type=merge -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
  kubectl delete instancetype "$UPD" --wait=false >/dev/null 2>&1 || true
  kubectl delete clusterqueue "$UPD" --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

# --- A. CREATE rejects missing required inputs (server dry-run, nothing persisted). ---
# An accelerated type with no acceleratorGroup has no pool to schedule onto — the validating webhook
# rejects it. (A non-accel type needs no group at all: Default fills generalGroup with "generic".)
errAcc=$(kubectl apply --dry-run=server -f - 2>&1 <<'EOF'
apiVersion: worker.gpustack.ai/v1alpha1
kind: InstanceType
metadata:
  name: e2e-case17-noaccgroup
spec:
  acceleratable: true
  os: linux
  arch: amd64
  unitResources:
    cpu: "1"
    ram: "2Gi"
  localStorage: "100Gi"
EOF
)
echo "$errAcc" | grep -qiE 'spec.acceleratorGroup|acceleratorGroup.*must be specified' \
  && record PASS "CREATE rejects accelerated missing acceleratorGroup" "validating webhook: acceleratorGroup required when acceleratable" \
  || record FAIL "CREATE rejects accelerated missing acceleratorGroup" "not rejected: ${errAcc:0:90}"

errUnit=$(kubectl apply --dry-run=server -f - 2>&1 <<'EOF'
apiVersion: worker.gpustack.ai/v1alpha1
kind: InstanceType
metadata:
  name: e2e-case17-nounit
spec:
  generalGroup: e2e17probe
  acceleratable: false
  os: linux
  arch: amd64
EOF
)
echo "$errUnit" | grep -qiE 'unitResources|localStorage' \
  && record PASS "CREATE rejects missing unit spec" "validating webhook: unitResources/localStorage required" \
  || record FAIL "CREATE rejects missing unit spec" "not rejected: ${errUnit:0:90}"

# --- B. Default webhook stamps the schedule + entrance labels on a valid CREATE (server dry-run). ---
stamped=$(kubectl apply --dry-run=server -o json -f - 2>/dev/null <<'EOF'
apiVersion: worker.gpustack.ai/v1alpha1
kind: InstanceType
metadata:
  name: e2e-case17-probe
spec:
  generalGroup: e2e17probe
  acceleratable: false
  os: linux
  arch: amd64
  unitResources:
    cpu: "1"
    ram: "2Gi"
  localStorage: "100Gi"
EOF
)
stamp=$(echo "$stamped" | python3 -c "
import json,sys
try: l=json.load(sys.stdin).get('metadata',{}).get('labels',{}) or {}
except Exception: l={}
accel=l.get('feature.gpustack.ai/acceleratable','')
os_=l.get('kubernetes.io/os',''); arch=l.get('kubernetes.io/arch','')
ent=l.get('${ENTRANCE}','')
ok = accel=='false' and os_=='linux' and arch=='amd64' and ent.startswith('gpustack-')
print(('PASS' if ok else 'FAIL')+'|acceleratable=%s os=%s arch=%s entrance=%s'%(accel or '<none>',os_ or '<none>',arch or '<none>',ent or '<none>'))
" 2>/dev/null)
if [ "${stamp%%|*}" = "PASS" ]; then
  record PASS "Default stamps schedule + entrance labels" "${stamp#*|}"
else
  record FAIL "Default stamps schedule + entrance labels" "${stamp#*|} — Default webhook must stamp the acceleratable boolean + os/arch + queue-entrance"
fi

# --- C. UPDATE freezes the whole spec — unitResources, localStorage AND generalGroup (REAL patches on a throwaway type). ---
kubectl apply -f - >/dev/null 2>&1 <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: InstanceType
metadata:
  name: ${UPD}
spec:
  generalGroup: e2e17upd
  acceleratable: false
  os: linux
  arch: amd64
  unitResources:
    cpu: "1"
    ram: "2Gi"
  localStorage: "100Gi"
EOF
made=""
for _ in $(seq 1 15); do kubectl get instancetype "$UPD" >/dev/null 2>&1 && { made=1; break; }; sleep 2; done
[ -n "$made" ] || record FAIL "throwaway type created for update probes" "${UPD} never appeared"

errCPU=$(kubectl patch instancetype "$UPD" --type=merge -p '{"spec":{"unitResources":{"cpu":"999"}}}' 2>&1)
cpuNow=$(kubectl get instancetype "$UPD" -o jsonpath='{.spec.unitResources.cpu}' 2>/dev/null)
{ echo "$errCPU" | grep -qiE 'unitResources.*immutable|immutable' && [ "$cpuNow" = "1" ]; } \
  && record PASS "UPDATE freezes unitResources" "cpu change rejected, stored cpu still 1" \
  || record FAIL "UPDATE freezes unitResources" "err='${errCPU:0:70}' storedCpu='${cpuNow}' — a unit change must be rejected and not persist"

errStg=$(kubectl patch instancetype "$UPD" --type=merge -p '{"spec":{"localStorage":"999Gi"}}' 2>&1)
stgNow=$(kubectl get instancetype "$UPD" -o jsonpath='{.spec.localStorage}' 2>/dev/null)
{ echo "$errStg" | grep -qiE 'localStorage.*immutable|immutable' && [ "$stgNow" = "100Gi" ]; } \
  && record PASS "UPDATE freezes localStorage" "localStorage change rejected, stored still 100Gi" \
  || record FAIL "UPDATE freezes localStorage" "err='${errStg:0:70}' stored='${stgNow}' — a localStorage change must be rejected and not persist"

errGroup=$(kubectl patch instancetype "$UPD" --type=merge -p '{"spec":{"generalGroup":"e2e17upd2"}}' 2>&1)
grpNow=$(kubectl get instancetype "$UPD" -o jsonpath='{.spec.generalGroup}' 2>/dev/null)
{ echo "$errGroup" | grep -qiE 'immutable|Forbidden' && [ "$grpNow" = "e2e17upd" ]; } \
  && record PASS "UPDATE freezes generalGroup (immutable)" "generalGroup change rejected, stored still e2e17upd" \
  || record FAIL "UPDATE freezes generalGroup (immutable)" "err='${errGroup:0:70}' storedGroup='${grpNow}' — generalGroup is part of pool identity, must be frozen"

echo
echo "== CASE 17 — InstanceType declarative admission =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). The webhook must require acceleratorGroup when acceleratable + os/arch"
  echo "+ a well-formed unit spec on CREATE, freeze unitResources/localStorage on UPDATE (real patch), and"
  echo "stamp the acceleratable boolean + os/arch + queue-entrance labels in Default. See pkg/worker/webhooks/worker/instance_type.go."
  exit 1
fi
echo "CASE 17 PASS"
