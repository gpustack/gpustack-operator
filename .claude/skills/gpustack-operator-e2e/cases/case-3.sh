#!/usr/bin/env bash
#
# CASE 3 — Managed-toggle is an independent drain trigger   (MUTATING, self-recovering)
#
#   case-3.sh <NS>
#
# Excluding a node from management (gpustack.ai/managed=false) must drain its
# single-node ResourceFlavors with the SAME chain as CASE 2 (flavor
# schedule.gpustack.ai/drain=true -> ClusterQueue HoldAndDrain) — but via a
# DIFFERENT trigger on a different code path. A managed toggle changes no feature
# label, so it drains only if the ResourceFlavor/Cohort Node-watch UpdateFunc
# predicates include systemname.ManagedLabelKey. See references/drain-recycle.md.
#
# IMPORTANT: verify against a CONTINUOUSLY RUNNING operator — a restart's For-watch
# resync drains the orphan regardless of the predicate and masks the bug. Toggle
# via the NodeFeature, not the node (NFD reverts a direct node label).
#
# Self-recovering: restores the managed label on exit.
#
# NOTE: toggling a node hosting a running Instance Stops that Instance. On a shared
# cluster pick a node whose Instances you can disrupt (or one with none).
set -uo pipefail

NS="${1:?usage: case-3.sh <NS>}"
NODE=$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')
before=$(kubectl get node "$NODE" -o jsonpath='{.metadata.labels.gpustack\.ai/managed}')
echo "node ${NODE}, current gpustack.ai/managed=${before:-<unset>}"

restore() {
  echo
  echo "[case-3] restoring gpustack.ai/managed=${before:-true}"
  kubectl -n "$NS" patch nodefeature "${NODE}-gpustack-worker" --type=merge \
    -p "{\"spec\":{\"labels\":{\"gpustack.ai/managed\":\"${before:-true}\"}}}" 2>/dev/null || true
}
trap restore EXIT

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

# Toggle out of management via the NodeFeature (NFD would revert a direct node label).
echo "[case-3] toggling gpustack.ai/managed=false"
kubectl -n "$NS" patch nodefeature "${NODE}-gpustack-worker" --type=merge \
  -p '{"spec":{"labels":{"gpustack.ai/managed":"false"}}}'

# Poll the drain chain: flavor annotated draining, ClusterQueue HoldAndDrain.
drained=""
for _ in $(seq 1 30); do
  d=$(kubectl get resourceflavors.kueue.x-k8s.io \
        -o jsonpath='{range .items[*]}{.metadata.annotations.schedule\.gpustack\.ai/drain}{"\n"}{end}' 2>/dev/null | grep -m1 true)
  [ -n "$d" ] && { drained=1; break; }
  sleep 3
done
[ -n "$drained" ] && record PASS "flavor draining on toggle" "schedule.gpustack.ai/drain=true" \
  || record FAIL "flavor draining on toggle" "no flavor drained — Node-watch predicate likely missing systemname.ManagedLabelKey"

held=""
for _ in $(seq 1 20); do
  h=$(kubectl get clusterqueues.kueue.x-k8s.io \
        -o jsonpath='{range .items[*]}{.spec.stopPolicy}{"\n"}{end}' 2>/dev/null | grep -m1 HoldAndDrain)
  [ -n "$h" ] && { held=1; break; }
  sleep 3
done
[ -n "$held" ] && record PASS "ClusterQueue HoldAndDrain" "stopPolicy=HoldAndDrain" \
  || record FAIL "ClusterQueue HoldAndDrain" "CQ never entered HoldAndDrain (may show a misleading 0/-1 quota instead)"

echo
echo "== CASE 3 — Managed-toggle is an independent drain trigger =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). Confirm the operator was NOT restarted between toggle and assertion"
  echo "(a restart's resync masks a missing predicate). See references/drain-recycle.md."
  exit 1
fi
echo "CASE 3 PASS"
