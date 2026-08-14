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
#              current CPU/memory/storage utilization as Total/Used pairs, the used side read in
#              real time from the node kubelet through the API-server node proxy — no Prometheus
#              or metrics-server involved on this path.
# Environment: Any cluster with a materialized scheduling chain (run case-1 first). No GPU and no
#              device manager required: this path reads the kubelet, and only the accelerator
#              entries — which a CPU Instance has none of — come from a device manager.
# Inputs:      All real, nothing mocked — sets the general InstanceType unit spec; a CPU Instance
#              gpustack-e2e-metrics (alpine sleep + ephemeral volume) on the general pool.
# Expected:    - GET .../instances/<name>/metrics returns one sample carrying timestamp and all
#              three pairs: cpuTotalMilliCores/cpuUsedMilliCores, memoryTotalMiB/memoryUsedMiB,
#              storageTotalMiB/storageUsedMiB;
#              - each total equals the sum of the backing pod's container limits, since a total is
#              the Instance's declaration rather than a measurement;
#              - the used figures belong to the backing pod (cpu moves after a load burst);
#              - once stopped, and so holding no Pod at all, the same call still answers: the declared
#              totals with zero measurements rather than an error, since nothing has run;
#              - a caller without the instances/metrics grant is denied
#              (kubectl auth can-i ... --as an unprivileged identity says no).
# Cleanup:     Trap deletes the test Instance.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

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

# 2. Current sample: structure and instance-scoped fields. The totals are declarations and are
#    there from the first answer; the used figures are measurements the kubelet publishes field by
#    field, so retry until the whole set is there rather than until CPU alone shows up.
checks=(
  ".sample.timestamp"
  ".sample.cpuTotalMilliCores"
  ".sample.cpuUsedMilliCores"
  ".sample.memoryTotalMiB"
  ".sample.memoryUsedMiB"
  ".sample.storageTotalMiB"
  ".sample.storageUsedMiB"
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
    record PASS "current sample served" \
      "timestamp plus all three Total/Used pairs present: $(echo "$latest" | jq -c '.sample | {cpuTotalMilliCores, cpuUsedMilliCores, memoryTotalMiB, memoryUsedMiB, storageTotalMiB, storageUsedMiB}' 2>/dev/null)"
  else
    record FAIL "current sample served" "missing fields:${missing}"
  fi
fi

# 3. A total is the Instance's declaration, not a measurement: it must equal the sum of the
#    backing pod's container limits. Init containers are excluded — they are gone by the time
#    anything is measured, and the kubelet does not evict on their limits.
totals_verdict=$(kubectl -n default get pod "$INST" -o json 2>/dev/null | python3 -c '
import json, sys

SCALES = {"Ki": 1024, "Mi": 1024 ** 2, "Gi": 1024 ** 3, "Ti": 1024 ** 4,
          "k": 1000, "M": 1000 ** 2, "G": 1000 ** 3, "T": 1000 ** 4}

def ceil(a, b):
    return -(-a // b)

def bytes_of(q):
    for suffix, scale in SCALES.items():
        if q.endswith(suffix):
            return ceil(int(float(q[: -len(suffix)]) * scale), 1)
    return int(float(q))

def milli_of(q):
    # The API sums ScaledValue(Milli) per container, which rounds a sub-milli quantity up.
    return ceil(int(float(q[:-1]) * 1000), 1000) if q.endswith("m") else int(float(q) * 1000)

sample = json.loads(sys.argv[1])["sample"]
pod = json.load(sys.stdin)

# Sum bytes across the containers and convert ONCE, exactly as the API does: converting per
# container and adding would round up twice on limits that are not MiB-aligned.
cpu = memory = storage = 0
for c in pod["spec"]["containers"]:
    limits = (c.get("resources") or {}).get("limits") or {}
    if "cpu" in limits:
        cpu += milli_of(limits["cpu"])
    if "memory" in limits:
        memory += bytes_of(limits["memory"])
    if "ephemeral-storage" in limits:
        storage += bytes_of(limits["ephemeral-storage"])

want = {"cpuTotalMilliCores": cpu,
        "memoryTotalMiB": ceil(memory, 1024 ** 2),
        "storageTotalMiB": ceil(storage, 1024 ** 2)}
bad = [f"{k}: sample={sample.get(k)} declared={v}" for k, v in want.items() if sample.get(k) != v]
print("FAIL " + "; ".join(bad) if bad else "PASS " + ", ".join(f"{k}={v}" for k, v in want.items()))
' "$latest" 2>/dev/null)
case "$totals_verdict" in
  PASS*) record PASS "totals equal the declared limits" "${totals_verdict#PASS }" ;;
  FAIL*) record FAIL "totals equal the declared limits" "${totals_verdict#FAIL }" ;;
  *)     record FAIL "totals equal the declared limits" "could not compare (pod ${INST} unreadable or python3 missing)" ;;
esac

# 4. The used figures track the backing pod: burn CPU inside the instance and require the
#    reading to rise (two kubelet sample windows apart).
before=$(echo "$latest" | jq -r '.sample.cpuUsedMilliCores // 0' 2>/dev/null)
# Collect the PIDs: job control is off in a non-interactive shell, so neither %1..%8 nor
# `jobs -p` reliably names the busy loops — leaving them alive would peg the node.
# shellcheck disable=SC2016  # the expansions belong to the instance's shell, not this one
kubectl -n default exec "$INST" -c main -- sh -c \
  'pids=""; for i in 1 2 3 4 5 6 7 8; do (while :; do :; done) & pids="$pids $!"; done; sleep 25; kill $pids 2>/dev/null' \
  >/dev/null 2>&1
after=""
for _ in $(seq 1 10); do
  after=$(kubectl get --raw "$RAW" 2>/dev/null | jq -r '.sample.cpuUsedMilliCores // 0' 2>/dev/null)
  [ -n "$after" ] && [ "$after" -gt "$before" ] && break
  sleep 5
done
if [ -n "$after" ] && [ "$after" -gt "$before" ]; then
  record PASS "used figures track the backing pod" "cpuUsedMilliCores ${before} -> ${after} under load"
else
  record FAIL "used figures track the backing pod" "cpuUsedMilliCores did not rise under load (before=${before} after=${after})"
fi

# 5. An Instance that has started nothing is answered, not refused. Stopping it takes its Pod away,
# which is the same state as never-scheduled and as a previous incarnation's Pod: nothing has run, so
# nothing has been used. The surface must serve its DECLARED totals with zero measurements rather than
# an error — a console reading it should need no branch for an Instance that is merely stopped.
echo "[case-37] stopping ${INST} to read the no-Pod answer"
# The field is spec.stop. A merge patch of a field the CRD does not have is accepted, pruned and
# only WARNED about, so a wrong name here does not fail — the Instance simply keeps running, its Pod
# never goes away, and this step then reports the surface refusing a stopped Instance that was never
# stopped. So the patch is verified by reading the field back rather than by its exit code.
kubectl -n default patch instance "$INST" --type=merge -p '{"spec":{"stop":true}}' >/dev/null 2>&1
stopped_json=""
stop_err=""
if [ "$(kubectl -n default get instance "$INST" -o jsonpath='{.spec.stop}' 2>/dev/null)" != "true" ]; then
  stop_err="spec.stop did not take — the Instance was never stopped, so this step proves nothing"
else
  for _ in $(seq 1 20); do
    if ! kubectl -n default get pod "$INST" >/dev/null 2>&1; then
      # Keep stderr: an empty body from a failed request is not the surface refusing to answer, and
      # reporting it as one blames the operator for the transport.
      stopped_json=$(kubectl get --raw "$RAW" 2>/dev/null) && [ -n "$stopped_json" ] && break
      stopped_json=""
    fi
    sleep 3
  done
  [ -z "$stopped_json" ] && stop_err="GET ${RAW} returned nothing once the Pod was gone — a stopped Instance must still be served"
fi
if [ -n "$stop_err" ]; then
  record FAIL "a stopped instance is answered, not refused" "$stop_err"
else
  gate=$(echo "$stopped_json" | python3 -c '
import json, sys

s = json.load(sys.stdin).get("sample", {})
bad = []
for total in ("cpuTotalMilliCores", "memoryTotalMiB", "storageTotalMiB"):
    if not s.get(total):
        bad.append("%s is %r, but a total is a declaration and survives having nothing to measure"
                   % (total, s.get(total)))
for used in ("cpuUsedMilliCores", "memoryUsedMiB", "storageUsedMiB"):
    # PRESENT and zero, not omitted: nothing has run, so the usage is known to be none — which is a
    # measurement this surface can state. An omitted field would say "nobody could measure it", and
    # accepting that here would let the regression this case exists to catch pass.
    if s.get(used) != 0:
        bad.append("%s is %r, but nothing has run so it must be a present zero" % (used, s.get(used)))
print(("FAIL " + "; ".join(bad)) if bad else "PASS")
' 2>/dev/null)
  case "$gate" in
    PASS) record PASS "a stopped instance is answered, not refused" \
      "declared totals served with zero measurements, no 503" ;;
    FAIL*) record FAIL "a stopped instance is answered, not refused" "${gate#FAIL }" ;;
    *) record FAIL "a stopped instance is answered, not refused" "could not parse the served sample" ;;
  esac
fi

# 6. Authorization: an unprivileged identity must not hold get on instances/metrics.
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
