#!/usr/bin/env bash
#
# CASE 14 — Multiple slices coexist on one physical card within budget
#   (MUTATING, self-recovering; AUTO-SKIPS without real sliced hardware)
#
#   case-14.sh <NS>
#
# Goal:        Two sliced Instances whose combined per-card memory <= 100% both admit and run on the
#              same physical card, a third slice that would push the card over 100% is held (not
#              over-admitted), and the two admitted slices stay Running after the third's rejected
#              admission attempt re-triggers their own AdmissionCheck reconcile (a self-eviction
#              regression guard: a pre-fix reconciler could count a Workload's own already-admitted
#              allocation against itself and evict a stable slice). Exercises the sliced credits quota +
#              the node-devices AdmissionCheck end to end.
# Environment: Needs REAL accelerator hardware advertising a *.sliced resource. AUTO-SKIPS (exit 0) otherwise.
# Inputs:      All real, nothing mocked — INST_A + INST_B (each a 40% memory slice, cores%=100), then
#              INST_C (a third 40% slice = 120% over one card), on the sliceable pool.
# Expected:    - both 40% slices reach Running (combined 80% <= 100%);
#              - the over-budget third is held (not Running);
#              - A and B stay Running after C's rejected admission (no self-eviction on sibling re-evaluation).
# Cleanup:     Trap deletes the three test Instances.
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

# Select on the LOGICAL slice view in .status, not on a spec flag: the pool's sliceability is an
# observed property of its cards, and a pool whose cards are all in a hardware partitioning mode
# serves no logical slice however "acceleratable" it is.
IT=$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null | python3 -c "
import json,sys
for it in json.load(sys.stdin).get('items',[]):
    if not it.get('spec',{}).get('acceleratable'):
        continue
    sl=(it.get('status',{}).get('acceleratorSliced') or {})
    if int(sl.get('capacity') or 0) > 0:
        print(it['metadata']['name']); break
")
if [ -z "$IT" ]; then
  echo "== CASE 14 — SKIPPED =="
  echo "No accelerated InstanceType reports a logical slice capacity — every accelerated pool is either"
  echo "non-sliceable or fully in a hardware partitioning mode. This case needs a logically sliceable pool."
  exit 0
fi

# The over-budget assertion below reads "a third 40% slice cannot fit", which is only true when the
# pool has exactly ONE logically sliceable card: the AdmissionCheck budget is per card, so on a
# multi-card pool the third slice legitimately lands on a free sibling card and runs. Rather than
# fill every other card to force the condition — which scales with the node and proves nothing extra —
# the case declines to run, and per-card accounting stays covered by CASE 11.
SL_CAP=$(kubectl get instancetype "$IT" -o jsonpath='{.status.acceleratorSliced.capacity}' 2>/dev/null)
if [ "${SL_CAP:-0}" -gt 100 ]; then
  echo "== CASE 14 — SKIPPED =="
  echo "Pool ${IT} reports a logical slice capacity of ${SL_CAP}% — more than one logically sliceable card."
  echo "This case's over-budget assertion needs a single-card pool (the budget is enforced per card, so a"
  echo "third slice would simply land on a free sibling card here). CASE 11 covers per-card accounting on"
  echo "multi-card hardware. Run this case on a single-accelerator node."
  exit 0
fi
echo "[case-14] logically sliceable InstanceType ${IT} (single-card pool, capacity ${SL_CAP}%)"

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

# 3. A and B must still be Running after C's (rejected) admission attempt re-triggers their own
# AdmissionCheck reconcile — this is the self-eviction regression: a pre-fix reconciler could count a
# Workload's own already-admitted allocation against itself and flip it to Retry, evicting a stable
# slice purely because a sibling Workload's admission was (re-)evaluated.
sleep 10
ra=$(kubectl -n default get pod "$INST_A" -o jsonpath='{.status.phase}' 2>/dev/null)
rb=$(kubectl -n default get pod "$INST_B" -o jsonpath='{.status.phase}' 2>/dev/null)
if [ "$ra" = "Running" ] && [ "$rb" = "Running" ]; then
  record PASS "A and B stay admitted after C's rejection" "${INST_A}=${ra} ${INST_B}=${rb} — no self-eviction on sibling re-evaluation"
else
  record FAIL "A and B stay admitted after C's rejection" "${INST_A}=${ra:-?} ${INST_B}=${rb:-?} — a sibling's admission attempt evicted an already-running slice"
fi

echo
echo "== CASE 14 — Multiple slices coexist on one physical card within budget =="
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
