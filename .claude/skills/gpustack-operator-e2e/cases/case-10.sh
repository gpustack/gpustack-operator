#!/usr/bin/env bash
#
# CASE 10 — Starting a resized-while-stopped Instance re-validates its resources   (MUTATING, self-recovering)
#
#   case-10.sh <NS>
#
# Guards the regression where a stopped Instance's resources are mutable (the running-instance
# immutability guard is skipped while stopped), but the start path only re-checked the upper caps
# (capResourcesToInstanceType) — not the full create-time contract. So a request that ValidateCreate
# rejects (CPU over the non-accel cap, a negative quantity, an out-of-range slice %) could be slipped
# in while stopped and then started. The fix runs the SAME validation on start as on create.
#
# Environment-agnostic: uses the general (CPU) pool, so it runs on any cluster (no GPU needed). The
# fake client cannot reproduce the stop→edit→start sequence across a real API server; needs a real
# cluster. Self-recovering: deletes the test Instance and restores the unit spec on exit.
set -uo pipefail

NS="${1:?usage: case-10.sh <NS>}"
INST=gpustack-e2e-resize-stopped
IT=$(kubectl get instancetypes.worker.gpustack.ai \
  -o jsonpath='{.items[?(@.spec.acceleratable==false)].metadata.name}' 2>/dev/null | tr ' ' '\n' | grep -m1 'gpustack-')
[ -n "$IT" ] || { echo "no general InstanceType found — run case-1 first to materialize the chain"; exit 1; }

restore() {
  echo
  echo "[case-10] cleanup: deleting test Instance"
  kubectl -n default delete instance "$INST" --ignore-not-found --force --grace-period=0 2>/dev/null || true
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

# The non-accelerated CPU cap this case exercises, plus a value comfortably above it.
CAP=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.status.cpu.onceMaxRequest}' 2>/dev/null)
[ -n "$CAP" ] || CAP=1
OVER=$((CAP + 1000))
echo "[case-10] InstanceType ${IT}, Status.CPU.OnceMaxRequest=${CAP}, over-cap request=${OVER}"

# 0. Control: create-time rejects an over-cap CPU request outright (the contract start must match).
if kubectl apply -f - >/dev/null 2>&1 <<EOF
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata: { name: ${INST}-ctl, namespace: default }
spec:
  type: ${IT}
  image: alpine
  command: ["sleep", "86400"]
  resources: { cpu: "${OVER}" }
  volume: { ephemeral: { capacity: 1Gi } }
EOF
then
  record FAIL "create rejects over-cap CPU" "create with cpu=${OVER} was admitted (expected reject)"
  kubectl -n default delete instance "${INST}-ctl" --ignore-not-found --force --grace-period=0 >/dev/null 2>&1 || true
else
  record PASS "create rejects over-cap CPU" "cpu=${OVER} > OnceMaxRequest ${CAP} rejected at create"
fi

# 1. Create a valid Instance (cpu=1) and let it reach Ready, then stop it.
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata: { name: ${INST}, namespace: default }
spec:
  type: ${IT}
  image: alpine
  command: ["sleep", "86400"]
  resources: { cpu: "1" }
  volume: { ephemeral: { capacity: 1Gi } }
EOF
phase=""
for _ in $(seq 1 40); do
  phase=$(kubectl -n default get instance "$INST" -o jsonpath='{.status.phase}' 2>/dev/null)
  [ "$phase" = Ready ] && break
  sleep 3
done
[ "$phase" = Ready ] || { echo "[case-10] ${INST} did not reach Ready (phase=${phase:-<empty>})"; exit 1; }
kubectl -n default patch instance "$INST" --type=merge -p '{"spec":{"stop":true}}' >/dev/null
for _ in $(seq 1 20); do
  [ "$(kubectl -n default get instance "$INST" -o jsonpath='{.status.phase}' 2>/dev/null)" = Stopped ] && break
  sleep 3
done

# 2. While stopped, resources are mutable — inject an over-cap CPU (create would reject this).
kubectl -n default patch instance "$INST" --type=merge -p "{\"spec\":{\"resources\":{\"cpu\":\"${OVER}\"}}}" >/dev/null 2>&1
got=$(kubectl -n default get instance "$INST" -o jsonpath='{.spec.resources.cpu}' 2>/dev/null)
[ "$got" = "$OVER" ] && record PASS "resources mutable while stopped" "cpu edited to ${OVER} while Stopped" \
  || record FAIL "resources mutable while stopped" "cpu edit did not stick (got '${got:-<empty>}')"

# 3. THE assertion: starting the instance re-validates and rejects the over-cap request.
err=$(kubectl -n default patch instance "$INST" --type=merge -p '{"spec":{"stop":false}}' 2>&1 >/dev/null)
rc=$?
if [ "$rc" -ne 0 ] && echo "$err" | grep -qiE 'exceeds the maximum CPU'; then
  record PASS "start re-validates resized resources" "start rejected cpu=${OVER} (same as create)"
else
  # Leave it stopped again for a clean teardown if it slipped through.
  kubectl -n default patch instance "$INST" --type=merge -p '{"spec":{"stop":true}}' >/dev/null 2>&1 || true
  record FAIL "start re-validates resized resources" "start accepted (rc=${rc}) an over-cap request — create-time validation bypassed"
fi

# 4. A valid resize (cpu within cap) still starts, so the guard does not over-reject.
kubectl -n default patch instance "$INST" --type=merge -p '{"spec":{"resources":{"cpu":"1"}}}' >/dev/null 2>&1
if kubectl -n default patch instance "$INST" --type=merge -p '{"spec":{"stop":false}}' >/dev/null 2>&1; then
  record PASS "start allows a valid resize" "cpu=1 within cap starts"
else
  record FAIL "start allows a valid resize" "a valid (in-cap) resized start was rejected"
fi

echo
echo "== CASE 10 — Starting a resized-while-stopped Instance re-validates its resources =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). Start must re-run the same resource validation as create (sign,"
  echo "accelerator/CPU caps, slice-% ranges), not just the upper caps. See webhooks/worker/instance.go."
  exit 1
fi
echo "CASE 10 PASS"
