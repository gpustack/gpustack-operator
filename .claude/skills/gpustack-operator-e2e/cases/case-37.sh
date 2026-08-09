#!/usr/bin/env bash
#
# CASE 37 — Instance metrics subresource serves the current instance-scoped utilization   (MUTATING, self-recovering)
#
#   case-37.sh <NS>
#
# <NS> is the operator's own namespace, as everywhere in this suite — not where the test objects
# go. The Instance lives in `default`: the Instance webhook rejects a reserved namespace, so
# creating it in <NS> would be denied outright.
#
# Goal:        The aggregated API's instances/<name>/metrics subresource returns the Instance's own
#              current CPU/memory/disk utilization, read in real time from the node kubelet through
#              the API-server node proxy — no Prometheus or metrics-server involved on this path.
# Environment: Any cluster with a materialized scheduling chain (run case-1 first); the device
#              manager DaemonSet rolls on the Instance's node. No GPU required.
# Inputs:      All real, nothing mocked — sets the general InstanceType unit spec; a CPU Instance
#              gpustack-e2e-metrics (alpine sleep + ephemeral volume) on the general pool.
# Expected:    - GET .../instances/<name>/metrics returns one sample carrying timestamp,
#              cpuUsageNanoCores, memoryWorkingSetMiB, rootfsUsedMiB and
#              ephemeralStorageUsedMiB;
#              - the figures belong to the backing pod (cpu/memory move after a load burst);
#              - a caller without the instances/metrics grant is denied
#              (kubectl auth can-i ... --as an unprivileged identity says no).
# Cleanup:     Trap deletes the test Instance.
set -uo pipefail

NS="${1:?usage: case-37.sh <NS>}"
INST=gpustack-e2e-metrics
IT=$(kubectl get instancetypes.worker.gpustack.ai \
  -o jsonpath='{.items[?(@.spec.acceleratable==false)].metadata.name}' 2>/dev/null | tr ' ' '\n' | grep -m1 'gpustack-')
[ -n "$IT" ] || { echo "no general InstanceType found — run case-1 first to materialize the chain"; exit 1; }

restore() {
  echo
  echo "[case-37] cleanup: deleting test Instance"
  kubectl -n default delete instance "$INST" --ignore-not-found 2>/dev/null || true
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

# 1. Create the Instance on the general pool.
echo "[case-37] creating Instance ${INST} of type ${IT}"
cat <<EOF | kubectl apply -f -
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata: { name: ${INST}, namespace: default }
spec:
  type: ${IT}
  image: alpine
  command: ["sleep", "86400"]
  volume: { ephemeral: { capacity: 1Gi } }
EOF

phase=""
for _ in $(seq 1 40); do
  phase=$(kubectl -n default get instance "$INST" -o jsonpath='{.status.phase}' 2>/dev/null)
  [ "$phase" = "Ready" ] && break
  sleep 3
done
[ "$phase" = "Ready" ] || record FAIL "instance ready" "phase='${phase:-<EMPTY>}'"

RAW="/apis/worker.gpustack.ai/v1/namespaces/default/instances/${INST}/metrics"

# 2. Current sample: structure and instance-scoped fields. The kubelet publishes a fresh pod's
#    stats field by field — rootfs lands only once the CRI has sized every container's writable
#    layer, so retry until the whole set is there rather than until CPU alone shows up.
checks=(
  ".sample.timestamp"
  ".sample.cpuUsageNanoCores"
  ".sample.memoryWorkingSetMiB"
  ".sample.rootfsUsedMiB"
  ".sample.ephemeralStorageUsedMiB"
)
missing_fields() {
  local payload=$1 jp v out=""
  for jp in "${checks[@]}"; do
    v=$(echo "$payload" | jq -r "if ${jp} == null then \"null\" else \"set\" end" 2>/dev/null)
    [ "$v" = "set" ] || out="${out} ${jp}"
  done
  echo "$out"
}

latest=""
missing=""
for _ in $(seq 1 15); do
  latest=$(kubectl get --raw "$RAW" 2>/dev/null)
  if [ -n "$latest" ]; then
    missing=$(missing_fields "$latest")
    [ -z "$missing" ] && break
  fi
  sleep 3
done
if [ -z "$latest" ]; then
  record FAIL "current sample served" "kubectl get --raw ${RAW} returned nothing"
else
  if [ -z "$missing" ]; then
    record PASS "current sample served" "timestamp/cpu/memory/rootfs/ephemeralStorage all present"
  else
    record FAIL "current sample served" "missing fields:${missing}"
  fi
fi

# 3. The figures track the backing pod: burn CPU inside the instance and require the
#    reading to rise (two kubelet sample windows apart).
before=$(echo "$latest" | jq -r '.sample.cpuUsageNanoCores // 0' 2>/dev/null)
# Collect the PIDs: job control is off in a non-interactive shell, so neither %1..%8 nor
# `jobs -p` reliably names the busy loops — leaving them alive would peg the node.
# shellcheck disable=SC2016  # the expansions belong to the instance's shell, not this one
kubectl -n default exec "$INST" -c main -- sh -c \
  'pids=""; for i in 1 2 3 4 5 6 7 8; do (while :; do :; done) & pids="$pids $!"; done; sleep 25; kill $pids 2>/dev/null' \
  >/dev/null 2>&1
after=""
for _ in $(seq 1 10); do
  after=$(kubectl get --raw "$RAW" 2>/dev/null | jq -r '.sample.cpuUsageNanoCores // 0' 2>/dev/null)
  [ -n "$after" ] && [ "$after" -gt "$before" ] && break
  sleep 5
done
if [ -n "$after" ] && [ "$after" -gt "$before" ]; then
  record PASS "figures track the backing pod" "cpuUsageNanoCores ${before} -> ${after} under load"
else
  record FAIL "figures track the backing pod" "cpuUsageNanoCores did not rise under load (before=${before} after=${after})"
fi

# 4. Authorization: an unprivileged identity must not hold get on instances/metrics.
deny=$(kubectl auth can-i get instances.worker.gpustack.ai/metrics -n default \
  --as system:serviceaccount:default:default 2>/dev/null)
if [ "$deny" = "no" ]; then
  record PASS "unprivileged caller denied" "system:serviceaccount:default:default cannot get instances/metrics"
else
  record FAIL "unprivileged caller denied" "can-i answered '${deny:-<error>}' for the default SA"
fi

# Results.
echo
echo "STATUS | CHECK | OBJECT"
for r in "${ROWS[@]}"; do echo "$r" | tr '|' ' ' | awk '{printf "%s | %s | %s\n", $1, $2, substr($0, index($0,$3))}'; done
[ "$FAILS" -eq 0 ] || { echo "[case-37] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-37] all checks passed"
