#!/usr/bin/env bash
#
# CASE 12 — Logically sliceable Instance webhook: slice-% sizes CPU/RAM, accelerator pinned to 1   (MUTATING, self-recovering; AUTO-SKIPS without a real logically sliceable accelerated pool)
#
#   case-12.sh <NS>
#
# Goal:        On a SLICEABLE accelerated InstanceType, assert the Instance admission webhooks —
#              Q1: Default scales the per-card unit CPU/RAM to the slice percentages (compute % sizes
#                  CPU, memory % sizes RAM), floors fractions, never below 1, and pins the accelerator to 1;
#              Q2: a lone memory percentage is mirrored to the compute percentage, so CPU is sized too;
#              Q3: Validate rejects a sliceable request whose accelerator count is not 1 (the slice is
#                  expressed through the percentages, not the card count).
# Environment: Needs a REAL LOGICALLY sliceable accelerated pool (an InstanceType whose observed
#              Status.Detail reports a logical-slice count). A partition-only pool does NOT qualify:
#              a slice percentage against a pool that offers no logical slicing is now rejected at
#              admission, where it used to be served silently as a whole card. AUTO-SKIPS (exit 0)
#              when none is present. Only the admission result is asserted (probes are deleted at once).
# Inputs:      - the real sliceable accelerated InstanceType, READ-ONLY (its immutable per-card unit spec
#                drives the expected slice math — the unit is not pinned/mutated);
#              - real probes: INST_OK (mem%=25, cores%=25), INST_MEMONLY (mem%=50, cores unset),
#                INST_BAD (mem%=25, cores%=25, accelerator=2).
# Expected:    - Q1 — INST_OK persists accelerator=1, cpu=unitCPU×25% (floor, min 1), ram=unitRAM×25%;
#              - Q2 — INST_MEMONLY mirrors cores%=50 and sizes cpu=unitCPU×50%, ram=unitRAM×50%;
#              - Q3 — INST_BAD is REJECTED (a sliceable accelerator must be 1).
# Cleanup:     Trap deletes the three test Instances (the InstanceType is never mutated).
set -uo pipefail

NS="${1:?usage: case-12.sh <NS>}"
INST_OK=gpustack-e2e-slice-ok
INST_MEMONLY=gpustack-e2e-slice-memonly
INST_BAD=gpustack-e2e-slice-badacc
IT=""; UNIT_CPU=""; UNIT_RAM=""                      # the real sliceable IT (read-only) + its per-card unit spec

restore() {
  echo
  echo "[case-12] cleanup: deleting test Instances"
  kubectl -n default delete instance "$INST_OK" "$INST_MEMONLY" "$INST_BAD" --ignore-not-found 2>/dev/null || true
  sleep 5
}
trap restore EXIT

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

# 1. Find a real LOGICALLY sliceable accelerated InstanceType (observed Status.Detail carries a
#    logical-slice count) and read its per-card unit spec. AUTO-SKIP when none is present.
read -r IT UNIT_CPU UNIT_RAM <<<"$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null | python3 -c "
import json,sys
for it in json.load(sys.stdin).get('items',[]):
    s=it.get('spec',{}); d=it.get('status',{}).get('detail',{}); sd=d.get('slicedDetail',{}); u=s.get('unitResources',{})
    # LOGICALLY sliceable only: a hardware-partitioned card serves no logical slice.
    sliceable=(sd.get('logical',{}).get('count',0) or 0)>0
    if s.get('acceleratable') and sliceable:
        print(it['metadata']['name'], u.get('cpu',''), u.get('ram','')); break
")"
if [ -z "$IT" ]; then
  echo "== CASE 12 — SKIPPED =="
  echo "No real logically sliceable accelerated InstanceType (observed Status.Detail reports a logical-slice"
  echo "count) — this case needs real accelerator hardware whose cards are logically sliceable, i.e. NOT in a"
  echo "hardware partitioning mode. Run it on such a cluster."
  exit 0
fi
echo "[case-12] logically sliceable InstanceType: ${IT} (per-card unit cpu=${UNIT_CPU} ram=${UNIT_RAM})"

# 2. Derive the expected slice sizing from the pool's per-card unit spec (immutable — read, don't pin).
#    CPU floors to an integer core, never below 1; RAM scales linearly. Requires an integer unit CPU
#    and a Gi unit RAM (what a real accelerated pool derives) — SKIP on an unexpected unit shape so a
#    slice check never fails for the wrong reason.
case "$UNIT_CPU" in ''|*[!0-9]*) echo "== CASE 12 — SKIPPED =="; echo "unit CPU '${UNIT_CPU}' is not an integer core count — cannot compute deterministic slice math"; exit 0;; esac
case "$UNIT_RAM" in *Gi) ;; *) echo "== CASE 12 — SKIPPED =="; echo "unit RAM '${UNIT_RAM}' is not in Gi — cannot compute deterministic slice math"; exit 0;; esac
RAM_N="${UNIT_RAM%Gi}"
cpu_pct() { local v=$(( UNIT_CPU * $1 / 100 )); [ "$v" -lt 1 ] && v=1; echo "$v"; }
ram_pct() { echo "$(( RAM_N * $1 / 100 ))Gi"; }
EXP_CPU_OK=$(cpu_pct 25);  EXP_RAM_OK=$(ram_pct 25)
EXP_CPU_MEM=$(cpu_pct 50); EXP_RAM_MEM=$(ram_pct 50)
echo "[case-12] expected: 25% slice → cpu=${EXP_CPU_OK} ram=${EXP_RAM_OK}; 50% slice → cpu=${EXP_CPU_MEM} ram=${EXP_RAM_MEM}"

# mk_slice NAME MEM_PCT CORES_PCT [ACCELERATOR] — apply a sliced Instance (accelerator omitted
# when the 4th arg is empty, so Default fills it). Prints kubectl's combined output; exits non-zero
# when admission rejects. A 0 cores-percentage means "unset" (exercises the mirror-from-memory path).
mk_slice() {
  {
    cat <<EOF
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata: { name: $1, namespace: default }
spec:
  type: ${IT}
  image: alpine
  command: ["sleep", "86400"]
  volume: { ephemeral: { capacity: 1Gi } }
  resources:
    acceleratorSlicedMemoryPercentage: $2
    acceleratorSlicedCoresPercentage: $3
EOF
    [ -n "${4:-}" ] && echo "    accelerator: \"$4\""
  } | kubectl apply -f - 2>&1
}

get_res() { kubectl -n default get instance "$1" -o jsonpath="{.spec.resources.$2}" 2>/dev/null; }

# Q1 — a 25% slice: accelerator pinned to 1, CPU = unitCPU × 25%, RAM = unitRAM × 25%.
echo "[case-12] Q1 apply: $(mk_slice "$INST_OK" 25 25)"
acc=""; for _ in $(seq 1 10); do acc=$(get_res "$INST_OK" accelerator); [ -n "$acc" ] && break; sleep 1; done
cpu=$(get_res "$INST_OK" cpu); ram=$(get_res "$INST_OK" ram)
if [ "$acc" = "1" ] && [ "$cpu" = "$EXP_CPU_OK" ] && [ "$ram" = "$EXP_RAM_OK" ]; then
  record PASS "Default scales slice to unit CPU/RAM, accelerator=1" "acc=${acc} cpu=${cpu} ram=${ram} (unit ${UNIT_CPU}/${UNIT_RAM} × 25%)"
else
  record FAIL "Default scales slice to unit CPU/RAM, accelerator=1" "acc='${acc:-?}' cpu='${cpu:-?}' ram='${ram:-?}', want 1/${EXP_CPU_OK}/${EXP_RAM_OK}"
fi

# Q2 — a lone memory percentage (50%) is mirrored to compute, so CPU is sized too.
echo "[case-12] Q2 apply: $(mk_slice "$INST_MEMONLY" 50 0)"
cores2=""; for _ in $(seq 1 10); do cores2=$(get_res "$INST_MEMONLY" acceleratorSlicedCoresPercentage); [ -n "$cores2" ] && break; sleep 1; done
cpu2=$(get_res "$INST_MEMONLY" cpu); ram2=$(get_res "$INST_MEMONLY" ram)
if [ "$cores2" = "50" ] && [ "$cpu2" = "$EXP_CPU_MEM" ] && [ "$ram2" = "$EXP_RAM_MEM" ]; then
  record PASS "lone memory % mirrored to compute, sizes CPU" "cores%=${cores2} cpu=${cpu2} ram=${ram2}"
else
  record FAIL "lone memory % mirrored to compute, sizes CPU" "cores%='${cores2:-?}' cpu='${cpu2:-?}' ram='${ram2:-?}', want 50/${EXP_CPU_MEM}/${EXP_RAM_MEM}"
fi

# Q3 — Validate rejects a sliceable request whose accelerator count is not 1.
err=$(mk_slice "$INST_BAD" 25 25 2); rc=$?
if [ "$rc" -ne 0 ] && echo "$err" | grep -qiE 'must be 1|denied|admission|invalid'; then
  record PASS "Validate rejects accelerator != 1 on sliceable" "rejected: $(echo "$err" | grep -oiE 'accelerator request must be 1' | head -1)"
else
  kubectl -n default delete instance "$INST_BAD" --ignore-not-found >/dev/null 2>&1 || true
  record FAIL "Validate rejects accelerator != 1 on sliceable" "accepted (rc=${rc}) — sliceable accelerator must be 1"
fi

echo
echo "== CASE 12 — Logically sliceable Instance webhook: slice-% sizes CPU/RAM, accelerator pinned to 1 =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). On a sliceable InstanceType the Default webhook must size CPU/RAM by the"
  echo "slice percentages of ONE card's unit (compute%→CPU, memory%→RAM, floor, min 1) and Validate must"
  echo "pin the accelerator count to 1. Diagnose: kubectl -n ${NS} logs deploy/gpustack-operator-worker --tail=200"
  exit 1
fi
echo "CASE 12 PASS"
