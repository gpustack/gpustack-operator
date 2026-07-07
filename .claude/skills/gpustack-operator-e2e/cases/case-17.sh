#!/usr/bin/env bash
#
# CASE 17 — InstanceType declarative admission   (MUTATING for the update probes, self-recovering)
#
#   case-17.sh <NS>
#
# The InstanceType admission contract introduced by the declarative-management change
# (require + freeze the admin inputs; the Default webhook stamps the schedule discriminators):
#   A — required inputs are enforced on CREATE: an InstanceType missing spec.group, or missing
#       its unit spec (unitResources / localStorage), is rejected by the validating webhook.
#   B — the Default (mutating) webhook stamps, from the spec identity, the feature-key +
#       kubernetes.io/os|arch schedule labels AND the queue-entrance label, so a stored type is
#       selectable and reverse-lookup-able (the Pod webhook reads per-card VRAM off it) from day one.
#   C — the unit spec is frozen on UPDATE: changing spec.unitResources / spec.localStorage is
#       rejected, while spec.group (a schedule discriminator) stays mutable and persists.
#
# A/B use server-side dry-run (nothing persisted). C uses REAL patches against a throwaway type:
# server-side dry-run does NOT reliably exercise update admission for a merge patch (it does for
# create), so the freeze must be probed with a real update — the throwaway is cleaned up on exit.
# Env-agnostic (CPU pool); run CASE 1 first so the webhook chain is up.
set -uo pipefail

NS="${1:?usage: case-17.sh <NS>}"
GEN_PREFIX="general.feature.gpustack.ai/"
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
errGroup=$(kubectl apply --dry-run=server -f - 2>&1 <<'EOF'
apiVersion: worker.gpustack.ai/v1alpha1
kind: InstanceType
metadata:
  name: e2e-case17-nogroup
spec:
  acceleratable: false
  os: linux
  arch: amd64
  unitResources:
    cpu: "1"
    ram: "2Gi"
  localStorage: "100Gi"
EOF
)
echo "$errGroup" | grep -qiE 'spec.group|group.*must be specified' \
  && record PASS "CREATE rejects missing group" "validating webhook: group required" \
  || record FAIL "CREATE rejects missing group" "not rejected: ${errGroup:0:90}"

errUnit=$(kubectl apply --dry-run=server -f - 2>&1 <<'EOF'
apiVersion: worker.gpustack.ai/v1alpha1
kind: InstanceType
metadata:
  name: e2e-case17-nounit
spec:
  group: e2e17probe
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
  group: e2e17probe
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
fkey=l.get('${GEN_PREFIX}e2e17probe','')
os_=l.get('kubernetes.io/os',''); arch=l.get('kubernetes.io/arch','')
ent=l.get('${ENTRANCE}','')
ok = fkey=='true' and os_=='linux' and arch=='amd64' and ent.startswith('gpustack-')
print(('PASS' if ok else 'FAIL')+'|fkey=%s os=%s arch=%s entrance=%s'%(fkey or '<none>',os_ or '<none>',arch or '<none>',ent or '<none>'))
" 2>/dev/null)
if [ "${stamp%%|*}" = "PASS" ]; then
  record PASS "Default stamps schedule + entrance labels" "${stamp#*|}"
else
  record FAIL "Default stamps schedule + entrance labels" "${stamp#*|} — Default webhook must stamp feature-key + os/arch + queue-entrance"
fi

# --- C. UPDATE freezes the unit spec; group stays mutable (REAL patches on a throwaway type). ---
kubectl apply -f - >/dev/null 2>&1 <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: InstanceType
metadata:
  name: ${UPD}
spec:
  group: e2e17upd
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

okGroup=$(kubectl patch instancetype "$UPD" --type=merge -p '{"spec":{"group":"e2e17upd2"}}' 2>&1)
grpNow=$(kubectl get instancetype "$UPD" -o jsonpath='{.spec.group}' 2>/dev/null)
[ "$grpNow" = "e2e17upd2" ] \
  && record PASS "UPDATE allows group change (mutable)" "group changed + persisted (e2e17upd -> e2e17upd2)" \
  || record FAIL "UPDATE allows group change (mutable)" "err='${okGroup:0:70}' storedGroup='${grpNow}' — group is a discriminator, must be mutable"

echo
echo "== CASE 17 — InstanceType declarative admission =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). The webhook must require group/os/arch + a well-formed unit spec on"
  echo "CREATE, freeze unitResources/localStorage on UPDATE (real patch), and stamp the feature-key +"
  echo "os/arch + queue-entrance labels in Default. See pkg/worker/webhooks/worker/instancetype.go."
  exit 1
fi
echo "CASE 17 PASS"
