#!/usr/bin/env bash
#
# CASE 2 — Drain stops a running Instance (not recreate)   (MUTATING, self-recovering)
#
#   case-2.sh <NS>
#
# Verifies the Instance <-> InstanceType contract on a real cluster (the unit
# tests cannot — see references/drain-recycle.md). When the InstanceType a
# RUNNING Instance references goes Inactive (its backing ClusterQueue enters
# HoldAndDrain), the reconciler must STOP the Instance (spec.stop=true), not
# recreate its Pod.
#
# Self-contained: creates a test Instance and drains by bumping the general ram
# capacity label, then RESTORES the label and deletes the Instance on exit (trap)
# so the cluster is left as found. Idempotent. Requires no accelerator.
set -uo pipefail

NS="${1:?usage: case-2.sh <NS>}"
NODE=$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')
IT=$(kubectl get instancetypes.worker.gpustack.ai -o jsonpath='{.items[?(@.status.phase=="Active")].metadata.name}' | awk '{print $1}')
[ -n "$IT" ] || { echo "no Active InstanceType found — run case-1 first to materialize the chain"; exit 1; }
echo "active InstanceType: ${IT} on node ${NODE}"

gKey=$(kubectl get node "$NODE" -o json | grep -oE '"general\.feature\.gpustack\.ai/[a-z0-9-]+\.ram"' | head -1 | sed -E 's#.*/(.*)\.ram"#\1#')
[ -n "$gKey" ] || { echo "no general ram label on node ${NODE} — chain not materialized"; exit 1; }
old=$(kubectl -n "$NS" get nodefeature "${NODE}-gpustack-worker" -o jsonpath="{.spec.labels.general\.feature\.gpustack\.ai/${gKey}\.ram}")

restore() {
  echo
  echo "[case-2] restoring ram label and deleting test Instance"
  kubectl -n "$NS" patch nodefeature "${NODE}-gpustack-worker" --type=merge \
    -p "{\"spec\":{\"labels\":{\"general.feature.gpustack.ai/${gKey}.ram\":\"${old}\"}}}" 2>/dev/null || true
  kubectl -n default delete instance gpustack-e2e-instance --ignore-not-found 2>/dev/null || true
}
trap restore EXIT

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

# 1. Create a running Instance referencing the Active InstanceType. alpine is kept
#    alive (sleep) so its Kueue Workload holds quota; the ephemeral volume lets
#    non-type validation pass.
cat <<EOF | kubectl apply -f -
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata: { name: gpustack-e2e-instance, namespace: default }
spec:
  type: ${IT}
  image: alpine
  command: ["sleep", "86400"]
  volume: { ephemeral: { capacity: 1Gi } }
EOF

# 2. The Pod must be admitted by Kueue (holding quota is what lets HoldAndDrain evict it).
admitted=""
for _ in $(seq 1 20); do
  a=$(kubectl -n default get workloads.kueue.x-k8s.io \
        -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Admitted")].status}{"\n"}{end}' 2>/dev/null | grep -m1 True)
  [ -n "$a" ] && { admitted=1; break; }
  sleep 3
done
[ -n "$admitted" ] && record PASS "workload admitted" "Kueue Admitted=True (holds quota)" \
  || record FAIL "workload admitted" "no Admitted workload — keep the container alive (sleep); nothing for HoldAndDrain to evict"

stop0=$(kubectl -n default get instance gpustack-e2e-instance -o jsonpath='{.spec.stop}' 2>/dev/null)
[ "$stop0" != "true" ] && record PASS "instance running pre-drain" "spec.stop=${stop0:-<unset>}" \
  || record FAIL "instance running pre-drain" "spec.stop already true before drain"

# 3. Drain: bump the general ram label so the node matches a NEW profile and the OLD one drains.
new=32Gi; [ "$old" = "32Gi" ] && new=24Gi   # any different even Gi value forces a new profile
echo "[case-2] draining: ram ${old} -> ${new} (gKey=${gKey})"
kubectl -n "$NS" patch nodefeature "${NODE}-gpustack-worker" --type=merge \
  -p "{\"spec\":{\"labels\":{\"general.feature.gpustack.ai/${gKey}.ram\":\"${new}\"}}}"

# 4. THE assertion: the Instance whose InstanceType is now Inactive gets STOPPED (poll — async).
stopped=""
for _ in $(seq 1 30); do
  s=$(kubectl -n default get instance gpustack-e2e-instance -o jsonpath='{.spec.stop}' 2>/dev/null)
  [ "$s" = "true" ] && { stopped=1; break; }
  sleep 3
done
[ -n "$stopped" ] && record PASS "instance STOPPED (not recreated)" "spec.stop=true" \
  || record FAIL "instance STOPPED (not recreated)" "spec.stop still ${s:-<unset>} — a buggy Phase!=Inactive recreates the Pod instead"

# Ground truth in the logs — proves the phase==Inactive branch ran, not some other path.
# Poll with a time window (not --tail): the line is emitted just before spec.stop is set, so
# kubectl logs can lag the stop poll by a moment; a burst of reconcile logs also rules out --tail.
# NOTE: use grep -c, not grep -q. Under `set -o pipefail`, grep -q closes the pipe on first match
# while kubectl logs is still streaming a large log → kubectl gets SIGPIPE (exit 141) → the pipeline
# reports non-zero even though the line matched. grep -c consumes all input, so kubectl exits cleanly.
logged=""
for _ in 1 2 3 4 5 6 7 8 9 10; do
  cnt=$(kubectl -n "$NS" logs deploy/gpustack-operator-worker --since=10m 2>/dev/null | grep -c "stop instance as inactive instance type")
  if [ "${cnt:-0}" -gt 0 ]; then
    logged=1
    break
  fi
  sleep 3
done
[ -n "$logged" ] && record PASS "inactive-type branch ran" "log: stop instance as inactive instance type" \
  || record FAIL "inactive-type branch ran" "log line absent — InstanceType may have gone straight to 'gone' instead of 'Inactive'"

echo
echo "== CASE 2 — Drain stops a running Instance (not recreate) =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). See references/drain-recycle.md (unit-test blind spot & why a real cluster)."
  exit 1
fi
echo "CASE 2 PASS"
