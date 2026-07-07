#!/usr/bin/env bash
#
# CASE 9 — Instance lifecycle survives an InstanceType unit-spec change   (MUTATING, self-recovering)
#
#   case-9.sh <NS>
#
# Goal:        Two admin scenarios on the general pool —
#              Q1: a RUNNING Instance whose InstanceType unit spec was shrunk can still be STOPPED and
#                  DELETED (stop only guards the start/stop race and delete is unvalidated, so neither
#                  re-checks the now-oversized resources against the shrunk unit spec).
#              Q2: (re)starting a STOPPED Instance after the shrink follows the
#                  instance-general-resources-overcommit setting — ON (default) re-derives CPU/RAM
#                  from the current unit spec and resizes-and-starts; OFF keeps the retained resources
#                  and the start-time cap rejects them. (Whether resize-on-start is the desired
#                  semantics is a separate product question; this case pins the behavior as it stands.)
# Environment: Any cluster with a materialized general pool. No GPU. Reads the overcommit setting from
#              the settings Secret and asserts the matching branch.
# Inputs:      All real, nothing mocked — the general InstanceType unit spec set to ram=2Gi then shrunk
#              to 1Gi; INST_A (running) and INST_B (running → stopped), both alpine on the general pool.
# Expected:    - Q1a — INST_A stops after the shrink; Q1b — INST_A deletes after the shrink;
#              - Q2 (overcommit ON) — INST_B starts and its spec.resources.ram re-derives to 1Gi;
#                Q2 (overcommit OFF) — the start is rejected for exceeding the shrunk per-unit cap.
# Cleanup:     Trap deletes both test Instances and restores the unit spec to ram=2Gi.
set -uo pipefail

NS="${1:?usage: case-9.sh <NS>}"
IT=$(kubectl get instancetypes.worker.gpustack.ai \
  -o jsonpath='{.items[?(@.spec.acceleratable==false)].metadata.name}' 2>/dev/null | tr ' ' '\n' | grep -m1 'gpustack-')
[ -n "$IT" ] || { echo "no general InstanceType found — run case-1 first to materialize the chain"; exit 1; }
INST_A=gpustack-e2e-lifecycle-stop    # Q1: running → stop/delete after shrink
INST_B=gpustack-e2e-lifecycle-start   # Q2: stopped → (re)start after shrink

# The Q2 outcome depends on the overcommit setting; read it (seeded in the settings Secret).
OVERCOMMIT=$(kubectl -n "$NS" get secret gpustack-settings -o jsonpath='{.data.instance-general-resources-overcommit}' 2>/dev/null | base64 -d 2>/dev/null)
[ -n "$OVERCOMMIT" ] || OVERCOMMIT=true

set_unit() { # ram
  kubectl patch instancetypes.worker.gpustack.ai "$IT" --type=merge \
    -p "{\"spec\":{\"unitResources\":{\"cpu\":\"1\",\"ram\":\"$1\"},\"localStorage\":\"10Gi\"}}" >/dev/null 2>&1
}

restore() {
  echo
  echo "[case-9] cleanup: deleting test Instances, restoring unit spec"
  kubectl -n default delete instance "$INST_A" "$INST_B" --ignore-not-found 2>/dev/null || true
  set_unit "2Gi"
}
trap restore EXIT

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

wait_phase() { # name phase
  for _ in $(seq 1 40); do
    [ "$(kubectl -n default get instance "$1" -o jsonpath='{.status.phase}' 2>/dev/null)" = "$2" ] && return 0
    sleep 3
  done
  return 1
}

mk_instance() { # name
  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata: { name: $1, namespace: default }
spec:
  type: ${IT}
  image: alpine
  command: ["sleep", "86400"]
  volume: { ephemeral: { capacity: 1Gi } }
EOF
}

# Baseline unit spec RAM=2Gi; confirm it stuck (the validating webhook may be briefly unready after
# a fresh deploy). Instances created now are sized at unitRAM=2Gi.
unit_ram=""
for _ in $(seq 1 15); do
  set_unit "2Gi"
  unit_ram=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.spec.unitResources.ram}' 2>/dev/null)
  [ "$unit_ram" = "2Gi" ] && break
  sleep 3
done
[ "$unit_ram" = "2Gi" ] || { echo "could not set unit spec on ${IT} (validating webhook not ready?)"; exit 1; }

# A running (for Q1) and B running→stopped (for Q2), both sized at unitRAM=2Gi.
mk_instance "$INST_A"
mk_instance "$INST_B"
wait_phase "$INST_A" Ready || { echo "instance ${INST_A} did not reach Ready"; exit 1; }
wait_phase "$INST_B" Ready || { echo "instance ${INST_B} did not reach Ready"; exit 1; }
kubectl -n default patch instance "$INST_B" --type=merge -p '{"spec":{"stop":true}}' >/dev/null
wait_phase "$INST_B" Stopped || { echo "instance ${INST_B} did not reach Stopped"; exit 1; }
ramB=$(kubectl -n default get instance "$INST_B" -o jsonpath='{.spec.resources.ram}' 2>/dev/null)
echo "[case-9] ${INST_A} Ready, ${INST_B} Stopped (ram=${ramB}); shrinking ${IT} unitRAM 2Gi → 1Gi"

# Shrink the unit spec below the instances' RAM.
set_unit "1Gi"
for _ in $(seq 1 10); do
  [ "$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.spec.unitResources.ram}')" = "1Gi" ] && break
  sleep 2
done

# Q1a — a running Instance can still be STOPPED after the unit-spec shrink.
if kubectl -n default patch instance "$INST_A" --type=merge -p '{"spec":{"stop":true}}' >/dev/null 2>&1 && wait_phase "$INST_A" Stopped; then
  record PASS "Q1 running instance stops after shrink" "${INST_A} → Stopped (stop does not re-check unit spec)"
else
  record FAIL "Q1 running instance stops after shrink" "${INST_A} could not stop — stop must not re-validate resources"
fi

# Q1b — and can be DELETED.
if kubectl -n default delete instance "$INST_A" --timeout=90s >/dev/null 2>&1; then
  record PASS "Q1 instance deletes after shrink" "${INST_A} deleted (delete is unvalidated)"
else
  record FAIL "Q1 instance deletes after shrink" "${INST_A} delete failed/timed out"
fi

# Q2 — start the stopped Instance after the shrink; the outcome depends on overcommit.
echo "[case-9] Q2: starting ${INST_B} after shrink (instance-general-resources-overcommit=${OVERCOMMIT})"
err=$(kubectl -n default patch instance "$INST_B" --type=merge -p '{"spec":{"stop":false}}' 2>&1 >/dev/null)
rc=$?
if [ "$OVERCOMMIT" = "true" ]; then
  # Overcommit re-derives CPU/RAM from the current unit spec on start → resized down and starts.
  newram=""
  wait_phase "$INST_B" Ready && newram=$(kubectl -n default get instance "$INST_B" -o jsonpath='{.spec.resources.ram}' 2>/dev/null)
  if [ "$rc" -eq 0 ] && [ "$newram" = "1Gi" ]; then
    record PASS "Q2 start re-derives to new unit spec (overcommit)" "${INST_B} started; ram ${ramB} → ${newram}"
  else
    record FAIL "Q2 start re-derives to new unit spec (overcommit)" "rc=${rc}, ram='${newram:-?}' (want started with ram=1Gi)"
  fi
else
  # No overcommit: the retained oversized resources are rejected by the start-time cap.
  if [ "$rc" -ne 0 ] && echo "$err" | grep -qiE 'maximum RAM|exceeds|denied|admission|invalid'; then
    record PASS "Q2 start rejected after shrink (no overcommit)" "start rejected: $(echo "$err" | grep -oiE 'exceeds[^\"]*' | head -1 | cut -c1-40)"
  else
    kubectl -n default patch instance "$INST_B" --type=merge -p '{"spec":{"stop":true}}' >/dev/null 2>&1 || true
    record FAIL "Q2 start rejected after shrink (no overcommit)" "start accepted (rc=${rc}) — expected rejection (ram ${ramB} > shrunk cap)"
  fi
fi

echo
echo "== CASE 9 — Instance lifecycle survives an InstanceType unit-spec change =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). Stop/Delete must never re-validate against the unit spec; Start's"
  echo "outcome follows instance-general-resources-overcommit (resize-and-start vs cap-and-reject)."
  exit 1
fi
echo "CASE 9 PASS"
