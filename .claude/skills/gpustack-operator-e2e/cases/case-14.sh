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
# Environment: Needs REAL NVIDIA accelerator hardware advertising nvidia.com/gpu.sliced. AUTO-SKIPS
#              (exit 0) otherwise. NVIDIA-only because the same-card assertion reads the visible-devices
#              env the runtime injected, and each vendor injects its own variable name — deriving that
#              here would duplicate a mapping the allocators already own, and drift from it.
# Inputs:      All real, nothing mocked — INST_A + INST_B (each a 40% memory slice, cores%=100), then
#              INST_C (a third 40% slice = 120% over one card), on the sliceable pool. Optionally
#              E2E_SLICE_LOAD_IMAGE (+ E2E_SLICE_LOAD_COMMAND) gives A a workload that allocates device
#              memory inside its slice; B stays idle either way.
# Expected:    - both 40% slices reach Running on the SAME physical card (combined 80% <= 100%),
#                compared on the card the runtime confined each to, not on the node;
#              - the over-budget third is held (not Running);
#              - A and B stay Running after C's rejected admission (no self-eviction on sibling re-evaluation);
#              - each Instance's metrics entry is ITS OWN: mode Sliced, the memory quota it was
#                granted (the same for both, since both asked for the same share), and a stated
#                utilization that agrees with the pair beside it. With a load image, A's used figure
#                is non-zero and differs from idle B's — two slices of one card reporting one number
#                between them is what a card-wide figure looks like, and this is what tells them apart.
# Cleanup:     Trap deletes the three test Instances.
set -uo pipefail

NS="${1:?usage: case-14.sh <NS>}"
INST_A=gpustack-e2e-coexist-a
INST_B=gpustack-e2e-coexist-b
INST_C=gpustack-e2e-coexist-c

# A workload that allocates device memory inside its slice, so the per-slice figures step 4 reads are
# measurements. Optional: unset, the slices stay idle and step 4 drops to its structural assertions.
# Any image that can allocate on the card will do; the default was run against an H100 and a 4090.
# dcgmproftester is NOT a substitute — it cannot initialize DCGM inside a MIG instance, and on a
# logical slice it measures the whole card rather than the share.
LOAD_IMAGE="${E2E_SLICE_LOAD_IMAGE:-}"
LOAD_COMMAND="${E2E_SLICE_LOAD_COMMAND:-[\"python\", \"-c\", \"import torch,time;h=torch.empty(1024**3//2,dtype=torch.float16,device='cuda');h.fill_(1.0);print('allocated',torch.cuda.memory_allocated()//1024**2,'MiB',flush=True);\\nwhile True: time.sleep(5)\"]}"

# --- Skip gate: real sliced accelerator required. ---
sliced_node=$(kubectl get nodes -o json 2>/dev/null | python3 -c "
import json,sys
for n in json.load(sys.stdin).get('items',[]):
    a=n.get('status',{}).get('allocatable',{})
    for k,v in a.items():
        if k == 'nvidia.com/gpu.sliced' and int(v)>0:
            print(n['metadata']['name']); sys.exit(0)
" 2>/dev/null)
if [ -z "$sliced_node" ]; then
  echo "== CASE 14 — SKIPPED =="
  echo "No node advertises nvidia.com/gpu.sliced — this case needs real NVIDIA accelerator hardware (see the"
  echo "Environment note above for why it is NVIDIA-only)."
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
    # NVIDIA only, to match the visible-devices variable the same-card assertion reads.
    if (it.get('status',{}).get('detail',{}) or {}).get('manufacturer') != 'nvidia':
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

mkslice() { # mkslice <name> <mem-pct> [image] [command-json]
  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: Instance
metadata: { name: $1, namespace: default }
spec:
  type: ${IT}
  image: ${3:-ubuntu:24.04}
  command: ${4:-[\"sleep\", \"infinity\"]}
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

# visible_card <pod> — the card the runtime confined the container to, retried.
#
# A Pod reports Running slightly before its container is attachable, so an exec issued the instant
# wait_running returns can come back empty (or fail with `container not found`) on a placement that is
# perfectly correct. Here that matters twice over: the check below reads an empty value as "the two
# slices are on different cards" and records a FAIL, turning a timing artifact into a reported defect.
visible_card() {
  local out
  for _ in $(seq 1 8); do
    out=$(kubectl -n default exec "$1" -c main -- printenv NVIDIA_VISIBLE_DEVICES 2>/dev/null)
    [ -n "$out" ] && { echo "$out"; return 0; }
    sleep 3
  done
  return 1
}

# 1. Two 40% slices (combined 80% <= 100%): both must run on the same card.
#
# A carries a workload that actually allocates device memory when one is supplied, so step 4 can read a
# MEASUREMENT rather than a plumbed zero; B stays idle on purpose, because "each slice reads its own
# figure" is only proven by two slices of one card reading DIFFERENT figures. Unset, both stay idle and
# step 4 asserts the structural half alone.
echo "[case-14] creating two 40% slices (${INST_A}, ${INST_B})"
if [ -n "$LOAD_IMAGE" ]; then
  echo "[case-14] ${INST_A} carries a device-memory load from ${LOAD_IMAGE}"
  mkslice "$INST_A" 40 "$LOAD_IMAGE" "$LOAD_COMMAND"
else
  mkslice "$INST_A" 40
fi
mkslice "$INST_B" 40
a_ok=1; wait_running "$INST_A" || a_ok=0
b_ok=1; wait_running "$INST_B" || b_ok=0
if [ "$a_ok" = 1 ] && [ "$b_ok" = 1 ]; then
  # Compare the CARD, not the node. This case's claim is that two in-budget slices coexist on one
  # PHYSICAL card, and a node name cannot tell that apart from two slices on two cards of one node —
  # it was recorded without being compared at all, which made the check vacuous.
  ca=$(visible_card "$INST_A")
  cb=$(visible_card "$INST_B")
  if [ -n "$ca" ] && [ "$ca" = "$cb" ]; then
    record PASS "two 40% slices coexist on one card" "${INST_A} + ${INST_B} both Running on ${ca} (combined 80% <= 100%)"
  else
    record FAIL "two 40% slices coexist on one card" "A@'${ca:-<unreadable>}' B@'${cb:-<unreadable>}' — two slices within one card's budget must share that card"
  fi
else
  record FAIL "two 40% slices coexist on one card" "A running=${a_ok} B running=${b_ok} — both should admit within one card"
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

# 4. Each slice's metrics are ITS OWN, not the card's. Two slices of one card is the only fixture that
# can tell those apart: a surface reporting the card would give both the same number, and one reporting
# a plumbed zero would give both zero even while a workload holds memory.
accel_json() { # accel_json <instance> — the first accelerator entry, or empty
  kubectl get --raw \
    "/apis/worker.gpustack.ai/v1/namespaces/default/instances/$1/metrics" 2>/dev/null |
    python3 -c '
import json, sys
try:
    s = json.load(sys.stdin).get("sample", {})
except Exception:
    sys.exit(0)
acc = (s.get("accelerators") or [None])[0]
if acc is None:
    sys.exit(0)
print(json.dumps({
    "id": acc.get("id"),
    "mode": acc.get("mode"),
    "totalMiB": acc.get("memoryTotalMiB"),
    "usedMiB": acc.get("memoryUsedMiB"),          # absent stays absent, never 0
    "utilPct": acc.get("memoryUtilizationPercent"),
    "coresPct": acc.get("coresUtilizationPercent"),
}))' 2>/dev/null
}

sa=""; sb=""
for _ in $(seq 1 12); do
  sa=$(accel_json "$INST_A")
  sb=$(accel_json "$INST_B")
  [ -n "$sa" ] && [ -n "$sb" ] && break
  sleep 5   # the producer publishes on its monitor period, so the first read can precede the section
done

if [ -z "$sa" ] || [ -z "$sb" ]; then
  record FAIL "each slice reports its own share" \
    "no accelerator entry served for A ($([ -n "$sa" ] && echo present || echo absent)) or B ($([ -n "$sb" ] && echo present || echo absent)) — a running sliced Instance must carry one"
else
  echo "[case-14] ${INST_A} accelerator: ${sa}"
  echo "[case-14] ${INST_B} accelerator: ${sb}"
  verdict=$(LOADED="$LOAD_IMAGE" python3 -c '
import json, os, sys

a, b = json.loads(sys.argv[1]), json.loads(sys.argv[2])
loaded = bool(os.environ.get("LOADED"))
bad = []

for name, s in (("A", a), ("B", b)):
    if s["mode"] != "Sliced":
        bad.append("%s mode=%r" % (name, s["mode"]))
    if not s["totalMiB"]:
        bad.append("%s carries no memory grant" % name)
    # The percentage is stated rather than left to the caller, so it must agree with the pair it
    # comes from; a disagreement means two sources where the API promises one.
    if s["usedMiB"] is not None and s["totalMiB"]:
        want = int(s["usedMiB"] * 100 / s["totalMiB"])
        if s["utilPct"] != want:
            bad.append("%s utilization=%r, but %s/%s is %d" % (
                name, s["utilPct"], s["usedMiB"], s["totalMiB"], want))
    elif s["utilPct"] is not None:
        bad.append("%s states a utilization with nothing measured" % name)

# Both asked for the same 40%%, so both were granted the same quota. A grant that came from the CARD
# rather than from the allocation would show up here as the card capacity in both entries — which is
# also what the loaded comparison below rules out from the other side.
if a["totalMiB"] and b["totalMiB"] and a["totalMiB"] != b["totalMiB"]:
    bad.append("A and B asked for the same share but were granted %s and %s MiB" % (
        a["totalMiB"], b["totalMiB"]))

# Two slices reading ONE figure between them is what a card-wide number looks like, and it is the
# confusion this surface exists to remove. It is compared BETWEEN the two Instances, which is the
# comparison that stays honest whichever of them holds the card memory.
if loaded:
    if not a["usedMiB"]:
        bad.append("A holds a workload but its usedMiB is %r" % (a["usedMiB"],))
    if a["usedMiB"] and a["usedMiB"] == b["usedMiB"]:
        bad.append("A and B report the same usedMiB (%s) though only A is loaded" % a["usedMiB"])

print(("FAIL " + "; ".join(bad)) if bad else "PASS")
' "$sa" "$sb" 2>/dev/null)
  case "$verdict" in
    PASS) record PASS "each slice reports its own share" \
      "A=${sa} B=${sb}$([ -n "$LOAD_IMAGE" ] && echo ' (A loaded, B idle)' || echo ' (both idle: structure only)')" ;;
    FAIL*) record FAIL "each slice reports its own share" "${verdict#FAIL }" ;;
    *) record FAIL "each slice reports its own share" "could not compare (python3 missing?)" ;;
  esac
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
