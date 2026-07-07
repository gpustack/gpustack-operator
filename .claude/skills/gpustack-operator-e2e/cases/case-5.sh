#!/usr/bin/env bash
#
# CASE 5 — Pod webhook folds slice-by-memory-% into units   (MUTATING, self-recovering)
#
#   case-5.sh <NS>
#
# Goal:        The Pod mutating webhook (objectSelector on kueue.x-k8s.io/queue-name, failurePolicy
#              Fail) folds a per-container .sliced.memory-percentage into per-card .sliced.units
#              (× M/100 = ×16000) and defaults an absent .sliced.cores-percentage to 100; the
#              validating webhook REJECTS a .sliced request that carries no memory at all.
# Environment: Any cluster, GPU-less — the memory-% fold is a pure ×16000 computation (M=1,600,000)
#              needing no card VRAM. Only the operator-installed Pod webhooks must be up; the Pods are
#              never expected to schedule (the webhook fires at CREATE, so the result is observable on
#              the persisted Pod).
# Inputs:      Real Pods, nothing mocked (the queue-name is a made-up label the objectSelector matches
#              on; the fold needs no ClusterQueue lookup) —
#              - POD_OK: .sliced=1 + memory-percentage=20 (expects the fold);
#              - POD_BAD: .sliced=1 with no memory at all (expects rejection).
# Expected:    - POD_OK persists .sliced.units=320000 (=20 × 16000) and .sliced.cores-percentage=100;
#              - POD_BAD is denied at CREATE by the validating webhook.
# Cleanup:     Trap deletes both test Pods.
set -uo pipefail

NS="${1:?usage: case-5.sh <NS>}"
QLABEL=kueue.x-k8s.io/queue-name
SLICED=nvidia.com/gpu.sliced
UNITS=nvidia.com/gpu.sliced.units
CORESPCT=nvidia.com/gpu.sliced.cores-percentage
MEMPCT=nvidia.com/gpu.sliced.memory-percentage
POD_OK=gpustack-e2e-webhook-pct
POD_BAD=gpustack-e2e-webhook-nomem

restore() {
  echo
  echo "[case-5] cleanup: deleting test Pods"
  kubectl -n default delete pod "$POD_OK" "$POD_BAD" --ignore-not-found --force --grace-period=0 2>/dev/null || true
}
trap restore EXIT

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

# 1. A .sliced Pod requesting memory-percentage=20: the mutating webhook folds it to
#    .sliced.units = 20 × 16000 = 320000 and defaults .sliced.cores-percentage = 100.
#    (A made-up queue-name is enough — the objectSelector matches the label's presence, and
#    the memory-% fold needs no ClusterQueue lookup.)
echo "[case-5] creating memory-% sliced Pod (expect fold → units=320000, cores-%=100)"
kubectl -n default delete pod "$POD_OK" --ignore-not-found --force --grace-period=0 >/dev/null 2>&1 || true
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: ${POD_OK}
  namespace: default
  labels:
    ${QLABEL}: gpustack-e2e-any
spec:
  schedulerName: default-scheduler
  containers:
    - name: main
      image: alpine
      command: ["sleep", "86400"]
      resources:
        limits:
          ${SLICED}: "1"
          ${MEMPCT}: "20"
        requests:
          ${SLICED}: "1"
          ${MEMPCT}: "20"
EOF

units=$(kubectl -n default get pod "$POD_OK" -o jsonpath="{.spec.containers[0].resources.requests.${UNITS//./\\.}}" 2>/dev/null)
cores=$(kubectl -n default get pod "$POD_OK" -o jsonpath="{.spec.containers[0].resources.requests.${CORESPCT//./\\.}}" 2>/dev/null)
# Kubernetes canonicalizes 320000 to the decimal-SI form "320k"; compare the numeric value.
unitsN=$(python3 -c "
v='${units}'
m={'k':1000,'M':1000000,'G':1000000000}
print(int(float(v[:-1])*m[v[-1]]) if v and v[-1] in m else (int(v) if v else ''))
" 2>/dev/null)
[ "$unitsN" = "320000" ] && record PASS "memory-% folded to units" "${UNITS}=${units} (=320000 = 20 × M/100)" \
  || record FAIL "memory-% folded to units" "got ${UNITS}=${units:-<unset>} (=${unitsN:-?}), want 320000 — mutating webhook fold"
[ "$cores" = "100" ] && record PASS "cores-% defaulted to 100" "${CORESPCT}=${cores}" \
  || record FAIL "cores-% defaulted to 100" "got ${CORESPCT}=${cores:-<unset>}, want 100 — mutating webhook default"

# 2. A .sliced Pod with NO memory (neither percentage nor mib) must be REJECTED at CREATE by
#    the validating webhook (failurePolicy Fail), rather than silently given a full/min slice.
echo "[case-5] creating memoryless sliced Pod (expect webhook REJECT)"
err=$(cat <<EOF | kubectl apply -f - 2>&1 >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: ${POD_BAD}
  namespace: default
  labels:
    ${QLABEL}: gpustack-e2e-any
spec:
  schedulerName: default-scheduler
  containers:
    - name: main
      image: alpine
      command: ["sleep", "86400"]
      resources:
        limits:
          ${SLICED}: "1"
        requests:
          ${SLICED}: "1"
EOF
)
rc=$?
if [ $rc -ne 0 ] && echo "$err" | grep -qiE "denied|memory|admission|webhook"; then
  record PASS "memoryless .sliced rejected" "webhook denied: $(echo "$err" | tr '\n' ' ' | grep -oiE 'admission webhook[^;]*' | cut -c1-48)"
else
  # If it slipped through, clean it up and fail.
  kubectl -n default delete pod "$POD_BAD" --ignore-not-found --force --grace-period=0 >/dev/null 2>&1 || true
  record FAIL "memoryless .sliced rejected" "Pod was admitted (rc=${rc}) — validating webhook must reject a .sliced request with no memory"
fi

echo
echo "== CASE 5 — Pod webhook folds slice-by-memory-% into units =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). The webhook set is {Instance, Pod}; confirm the gpustack-worker"
  echo "mutating/validating Pod webhooks are installed and sort before kueue's. Diagnose:"
  echo "  kubectl get mutatingwebhookconfigurations,validatingwebhookconfigurations | grep gpustack"
  exit 1
fi
echo "CASE 5 PASS"
