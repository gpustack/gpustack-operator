#!/usr/bin/env bash
#
# CASE 7 — Portless Instance reaches Ready and creates no Service   (MUTATING, self-recovering)
#
#   case-7.sh <NS>
#
# Goal:        A portless Instance skips Service creation and still gets its status written. Guards
#              the regression where a portless Instance made the controller create a NodePort Service
#              with no ports — rejected by the API ("spec.ports: Required value") — which failed every
#              reconcile before the status block, so the Pod ran but the Instance status stayed empty.
# Environment: Any cluster with a materialized general pool; needs a real cluster (the API-server
#              Service validation is what rejects a portless Service — the fake client cannot). No GPU.
# Inputs:      All real, nothing mocked — sets the general InstanceType unit spec; a portless Instance
#              gpustack-e2e-portless (no spec.ports, alpine sleep + ephemeral volume) on the general pool.
# Expected:    - the Instance status is written (phase reaches Ready, or at least a non-empty,
#                non-stuck phase);
#              - no Service named after the Instance is created;
#              - no "spec.ports: Required value" reconcile error appears for it in the worker log.
# Cleanup:     Trap deletes the test Instance.
set -uo pipefail

NS="${1:?usage: case-7.sh <NS>}"
INST=gpustack-e2e-portless
IT=$(kubectl get instancetypes.worker.gpustack.ai \
  -o jsonpath='{.items[?(@.spec.acceleratable==false)].metadata.name}' 2>/dev/null | tr ' ' '\n' | grep -m1 'gpustack-')
[ -n "$IT" ] || { echo "no general InstanceType found — run case-1 first to materialize the chain"; exit 1; }

restore() {
  echo
  echo "[case-7] cleanup: deleting test Instance"
  kubectl -n default delete instance "$INST" --ignore-not-found 2>/dev/null || true
}
trap restore EXIT

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

# The general InstanceType carries no unit spec by default; the Instance webhook needs one to size
# the Pod. Set it and confirm it stuck (the validating webhook may be briefly unready after deploy).
unit_ram=""
for _ in $(seq 1 15); do
  kubectl patch instancetypes.worker.gpustack.ai "$IT" --type=merge \
    -p '{"spec":{"unitResources":{"cpu":"1","ram":"2Gi"},"localStorage":"10Gi"}}' >/dev/null 2>&1
  unit_ram=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.spec.unitResources.ram}' 2>/dev/null)
  [ -n "$unit_ram" ] && break
  sleep 3
done
[ -n "$unit_ram" ] || { echo "no unit spec on ${IT} (validating webhook not ready?)"; exit 1; }

# 1. Create a PORTLESS Instance (no spec.ports) on the general pool.
echo "[case-7] creating portless Instance ${INST} of type ${IT}"
cat <<EOF | kubectl apply -f -
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata: { name: ${INST}, namespace: default }
spec:
  type: ${IT}
  image: alpine
  command: ["sleep", "86400"]
  volume: { ephemeral: { capacity: 1Gi } }
EOF

# 2. THE assertion: the Instance status is written (phase non-empty). Before the fix the reconcile
#    errored on Service creation before the status block, leaving .status empty while the Pod ran.
phase=""
for _ in $(seq 1 40); do
  phase=$(kubectl -n default get instance "$INST" -o jsonpath='{.status.phase}' 2>/dev/null)
  [ "$phase" = "Ready" ] && break
  sleep 3
done
case "$phase" in
  Ready)                    record PASS "instance status written" "phase=Ready (portless Instance progresses)" ;;
  Starting|Running|Pending) record PASS "instance status written" "phase=${phase} (non-empty; not stuck)" ;;
  *)                        record FAIL "instance status written" "phase='${phase:-<EMPTY>}' — status never written (portless Service error blocked it)" ;;
esac

# 3. No Service must be created for a portless Instance.
if kubectl -n default get svc "$INST" >/dev/null 2>&1; then
  record FAIL "no Service for portless Instance" "svc/${INST} exists — a portless workload needs none"
else
  record PASS "no Service for portless Instance" "no svc/${INST} (Service creation skipped)"
fi

# 4. Ground truth: no 'spec.ports: Required value' reconcile error for this Instance.
errs=$(kubectl -n "$NS" logs deploy/gpustack-operator-worker --since=5m 2>/dev/null | grep "$INST" | grep -c "spec.ports: Required value")
[ "${errs:-0}" -eq 0 ] && record PASS "no portless-Service reconcile error" "0 'spec.ports: Required value' for ${INST}" \
  || record FAIL "no portless-Service reconcile error" "${errs} 'spec.ports: Required value' error(s) — controller tried to create a portless Service"

echo
echo "== CASE 7 — Portless Instance reaches Ready and creates no Service =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). A portless Instance must skip the Service and still write its status."
  echo "Diagnose: kubectl -n ${NS} logs deploy/gpustack-operator-worker --tail=200 | grep -i 'create service'"
  exit 1
fi
echo "CASE 7 PASS"
