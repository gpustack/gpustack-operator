#!/usr/bin/env bash
#
# CASE 14 — Multiple slices coexist on one physical card within budget (Story 6)
#   (MUTATING, self-recovering)
#
#   case-14.sh <NS>
#
# Regression for specs/2026-07-04-ssh-instance-accelerator-slicing Story 6: two sliced Instances whose
# combined per-card memory percentage is <= 100 both admit and run on the same physical card, while a
# third slice that would push the card over 100% is held (SchedulingGated / not Running) rather than
# over-admitted. This exercises the sliced credits quota + the node-devices AdmissionCheck end to end
# on REAL accelerator hardware; it AUTO-SKIPS when no `*.sliced` resource is advertised.
#
# Self-recovering: deletes the three test Instances on exit.
set -uo pipefail

NS="${1:?usage: case-14.sh <NS>}"
INST_A=gpustack-e2e-coexist-a
INST_B=gpustack-e2e-coexist-b
INST_C=gpustack-e2e-coexist-c

# --- Skip gate: real sliced accelerator required. ---
sliced_node=$(kubectl get nodes -o json 2>/dev/null | python3 -c "
import json,sys
for n in json.load(sys.stdin).get('items',[]):
    a=n.get('status',{}).get('allocatable',{})
    for k,v in a.items():
        if k.endswith('/gpu.sliced') and int(v)>0:
            print(n['metadata']['name']); sys.exit(0)
" 2>/dev/null)
if [ -z "$sliced_node" ]; then
  echo "== CASE 14 — SKIPPED =="
  echo "No node advertises a *.sliced accelerator resource — this case needs real accelerator hardware."
  exit 0
fi
echo "[case-14] real sliced accelerator found on ${sliced_node}"

IT=$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null | python3 -c "
import json,sys
for it in json.load(sys.stdin).get('items',[]):
    s=it.get('spec',{})
    if s.get('acceleratable') and s.get('sliceable'):
        print(it['metadata']['name']); break
")
[ -n "$IT" ] || { echo "no sliceable accelerated InstanceType found"; exit 1; }
echo "[case-14] sliceable InstanceType ${IT}"

restore() {
  echo
  echo "[case-14] cleanup: deleting test Instances"
  kubectl -n default delete instance "$INST_A" "$INST_B" "$INST_C" --ignore-not-found --wait=false 2>/dev/null || true
}
trap restore EXIT

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

mkslice() { # mkslice <name> <mem-pct>
  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: Instance
metadata: { name: $1, namespace: default }
spec:
  type: ${IT}
  image: ubuntu:24.04
  command: ["sleep", "infinity"]
  resources:
    cpu: "1"
    ram: "4Gi"
    localStorage: "10Gi"
    accelerator: "1"
    acceleratorSlicedMemoryPercentage: $2
    acceleratorSlicedCoresPercentage: 100
  volume: { ephemeral: { capacity: 5Gi } }
  volumeMount: /workspace
EOF
}

running() { [ "$(kubectl -n default get pod "$1" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Running" ]; }
wait_running() { for _ in $(seq 1 40); do running "$1" && return 0; sleep 3; done; return 1; }

# 1. Two 40% slices (combined 80% <= 100%): both must run on the same card.
echo "[case-14] creating two 40% slices (${INST_A}, ${INST_B})"
mkslice "$INST_A" 40
mkslice "$INST_B" 40
a_ok=1; wait_running "$INST_A" || a_ok=0
b_ok=1; wait_running "$INST_B" || b_ok=0
if [ "$a_ok" = 1 ] && [ "$b_ok" = 1 ]; then
  na=$(kubectl -n default get pod "$INST_A" -o jsonpath='{.spec.nodeName}' 2>/dev/null)
  nb=$(kubectl -n default get pod "$INST_B" -o jsonpath='{.spec.nodeName}' 2>/dev/null)
  record PASS "two 40% slices coexist" "${INST_A}@${na} + ${INST_B}@${nb} both Running (combined 80% <= 100%)"
else
  record FAIL "two 40% slices coexist" "A running=${a_ok} B running=${b_ok} — both should admit within one card"
fi

# 2. A third slice that would exceed the card (80% + 40% > 100%) must be held, not over-admitted.
echo "[case-14] creating an over-budget third 40% slice (${INST_C})"
mkslice "$INST_C" 40
held=1
for _ in $(seq 1 8); do
  if running "$INST_C"; then held=0; break; fi
  sleep 3
done
if [ "$held" = 1 ]; then
  ph=$(kubectl -n default get pod "$INST_C" -o jsonpath='{.status.phase}' 2>/dev/null)
  record PASS "over-budget slice is held (not over-admitted)" "${INST_C} not Running (phase='${ph:-<no pod>}'; 120% > one card)"
else
  record FAIL "over-budget slice is held (not over-admitted)" "${INST_C} Running — three 40% slices over-admitted one card"
fi

echo
echo "== CASE 14 — Multiple slices coexist on one physical card within budget (Story 6) =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'
if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). Diagnose: kubectl -n default get instances,pods;"
  echo "kubectl -n default get workloads -o wide"
  exit 1
fi
echo "CASE 14 PASS"
