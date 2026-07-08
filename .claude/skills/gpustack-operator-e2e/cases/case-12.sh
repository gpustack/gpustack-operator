#!/usr/bin/env bash
#
# CASE 12 — Sliceable Instance webhook: slice-% sizes CPU/RAM, accelerator pinned to 1   (MUTATING, self-recovering)
#
#   case-12.sh <NS>
#
# Goal:        On a SLICEABLE accelerated InstanceType, assert the Instance admission webhooks —
#              Q1: Default scales the per-card unit CPU/RAM to the slice percentages (compute % sizes
#                  CPU, memory % sizes RAM), floors fractions, never below 1, and pins the accelerator to 1;
#              Q2: a lone memory percentage is mirrored to the compute percentage, so CPU is sized too;
#              Q3: Validate rejects a sliceable request whose accelerator count is not 1 (the slice is
#                  expressed through the percentages, not the card count).
# Environment: Any cluster BY APPROXIMATION (same fake-accelerator mock as CASE 6). The Instances never
#              schedule (no real card) — only the admission result is asserted. No real hardware.
# Inputs:      - MOCKED: a fake accelerator NodeFeature (nvidia-e2emock, count=8) → the derived sliceable
#                InstanceType, whose unit resources are pinned (cpu=16, ram=40Gi) for deterministic math;
#              - real probes: INST_OK (mem%=25, cores%=25), INST_MEMONLY (mem%=50, cores unset),
#                INST_BAD (mem%=25, cores%=25, accelerator=2).
# Expected:    - Q1 — INST_OK persists accelerator=1, cpu=4, ram=10Gi (16 / 40Gi × 25%);
#              - Q2 — INST_MEMONLY mirrors cores%=50 and sizes cpu=8, ram=20Gi;
#              - Q3 — INST_BAD is REJECTED (a sliceable accelerator must be 1).
# Cleanup:     Trap deletes the three test Instances and the injected NodeFeature.
set -uo pipefail

NS="${1:?usage: case-12.sh <NS>}"
NODE=$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')
AKEY=nvidia-e2emock                                 # non-colliding fake key (mirrors case-4/6)
COUNT=8
ACCEL_NF="${NODE}-gpustack-e2e-accel"
LABELPFX="acceleratable.feature.gpustack.ai/${AKEY}"
UNIT_CPU=16                                          # pinned so the slice math is deterministic
UNIT_RAM=40Gi
INST_OK=gpustack-e2e-slice-ok
INST_MEMONLY=gpustack-e2e-slice-memonly
INST_BAD=gpustack-e2e-slice-badacc

restore() {
  echo
  echo "[case-12] cleanup: deleting test Instances, injected NodeFeature"
  kubectl -n default delete instance "$INST_OK" "$INST_MEMONLY" "$INST_BAD" --ignore-not-found 2>/dev/null || true
  kubectl -n "$NS" delete nodefeature "$ACCEL_NF" --ignore-not-found 2>/dev/null || true
  sleep 5
}
trap restore EXIT

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

# 1. Inject the fake accelerator so the Worker derives the sliceable accelerated InstanceType.
echo "[case-12] injecting fake accelerator ${AKEY} (count=${COUNT}) on node ${NODE}"
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: nfd.k8s-sigs.io/v1alpha1
kind: NodeFeature
metadata:
  name: ${ACCEL_NF}
  namespace: ${NS}
  labels:
    nfd.node.kubernetes.io/node-name: ${NODE}
    app.kubernetes.io/part-of: gpustack-operator-e2e
spec:
  labels:
    ${LABELPFX}: "true"
    ${LABELPFX}.count: "${COUNT}"
    ${LABELPFX}.product: "A10G"
    ${LABELPFX}.memory: "24Gi"
    ${LABELPFX}.cores: "12"
  features: {}
EOF

# 2. Wait for the derived sliceable InstanceType (name gpustack--${AKEY}-<os>-<arch> when
#    CPU-manufacturer awareness is off).
IT=""
for _ in $(seq 1 40); do
  IT=$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null | python3 -c "
import json,sys
for it in json.load(sys.stdin).get('items',[]):
    s=it.get('spec',{}); n=it['metadata']['name']
    if n.startswith('gpustack--${AKEY}-') and s.get('acceleratable') and s.get('sliceable'):
        print(n); break
" 2>/dev/null)
  [ -n "$IT" ] && break
  sleep 3
done
[ -n "$IT" ] || { echo "[case-12] derived sliceable InstanceType never materialized — is instance-type-derived-from-node on?"; exit 1; }
echo "[case-12] sliceable InstanceType: ${IT}"

# 3. Pin its unit resources so the slice math is deterministic (round-trips via the CQ notes).
#    Re-apply the patch inside the wait loop — the validating webhook can be briefly unready right
#    after a fresh deploy — and hard-fail if CPU *and* RAM never settle to the pinned values, so a
#    later slice check cannot fail for the wrong reason (mirrors case-2 / case-9).
pinned=""
for _ in $(seq 1 15); do
  kubectl patch instancetypes.worker.gpustack.ai "$IT" --type=merge \
    -p "{\"spec\":{\"unitResources\":{\"cpu\":\"${UNIT_CPU}\",\"ram\":\"${UNIT_RAM}\"}}}" >/dev/null 2>&1
  ucpu=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.spec.unitResources.cpu}' 2>/dev/null)
  uram=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.spec.unitResources.ram}' 2>/dev/null)
  [ "$ucpu" = "$UNIT_CPU" ] && [ "$uram" = "$UNIT_RAM" ] && { pinned=1; break; }
  sleep 3
done
[ -n "$pinned" ] || { echo "[case-12] could not pin unit resources on ${IT} (cpu='${ucpu:-?}' ram='${uram:-?}', want ${UNIT_CPU}/${UNIT_RAM}) — validating webhook not ready?"; exit 1; }

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

# Q1 — a 25% slice: accelerator pinned to 1, CPU = unitCPU × 25% = 4, RAM = unitRAM × 25% = 10Gi.
echo "[case-12] Q1 apply: $(mk_slice "$INST_OK" 25 25)"
acc=""; for _ in $(seq 1 10); do acc=$(get_res "$INST_OK" accelerator); [ -n "$acc" ] && break; sleep 1; done
cpu=$(get_res "$INST_OK" cpu); ram=$(get_res "$INST_OK" ram)
if [ "$acc" = "1" ] && [ "$cpu" = "4" ] && [ "$ram" = "10Gi" ]; then
  record PASS "Default scales slice to unit CPU/RAM, accelerator=1" "acc=${acc} cpu=${cpu} ram=${ram} (unit ${UNIT_CPU}/${UNIT_RAM} × 25%)"
else
  record FAIL "Default scales slice to unit CPU/RAM, accelerator=1" "acc='${acc:-?}' cpu='${cpu:-?}' ram='${ram:-?}', want 1/4/10Gi"
fi

# Q2 — a lone memory percentage (50%) is mirrored to compute, so CPU is sized too (=8, RAM=20Gi).
echo "[case-12] Q2 apply: $(mk_slice "$INST_MEMONLY" 50 0)"
cores2=""; for _ in $(seq 1 10); do cores2=$(get_res "$INST_MEMONLY" acceleratorSlicedCoresPercentage); [ -n "$cores2" ] && break; sleep 1; done
cpu2=$(get_res "$INST_MEMONLY" cpu); ram2=$(get_res "$INST_MEMONLY" ram)
if [ "$cores2" = "50" ] && [ "$cpu2" = "8" ] && [ "$ram2" = "20Gi" ]; then
  record PASS "lone memory % mirrored to compute, sizes CPU" "cores%=${cores2} cpu=${cpu2} ram=${ram2}"
else
  record FAIL "lone memory % mirrored to compute, sizes CPU" "cores%='${cores2:-?}' cpu='${cpu2:-?}' ram='${ram2:-?}', want 50/8/20Gi"
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
echo "== CASE 12 — Sliceable Instance webhook: slice-% sizes CPU/RAM, accelerator pinned to 1 =="
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
