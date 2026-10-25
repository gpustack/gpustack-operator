#!/usr/bin/env bash
#
# CASE 37 — One Instance's utilization: served by the metrics subresource and published by the node's device manager
#   (MUTATING, self-recovering; AUTO-SKIPS the exporter half without a device manager pod on the Instance's node)
#
#   case-37.sh <NS>
#
# <NS> is the operator's own namespace, as everywhere in this suite — not where the test objects
# go. The Instance lives in `default`: the Instance webhook rejects a reserved namespace, so
# creating it in <NS> would be denied outright.
#
# Goal:        One Instance's own utilization is observable through both surfaces the operator
#              offers: the aggregated API's instances/<name>/metrics subresource serves it straight
#              from the node kubelet — no Prometheus or metrics-server on that path — and the
#              device manager running on that node publishes the same Instance as Prometheus
#              gauges labelled by the Instance's identity.
# Environment: Any cluster with a materialized scheduling chain (run case-1 first). The subresource
#              half needs no GPU and no device manager: it reads the kubelet, and only the
#              accelerator entries — which a CPU Instance has none of — come from a device manager.
#              The exporter half AUTO-SKIPS when the node the Instance landed on runs no Ready
#              device manager pod: a node with no accelerators runs no device manager, and then
#              that surface has nothing to publish.
# Inputs:      All real, nothing mocked — reads the general InstanceType unit spec; one CPU
#              Instance gpustack-e2e-metrics (alpine sleep + ephemeral volume) on the general pool.
#              Reads /metrics through the API server's pod proxy, so nothing has to be installed in
#              the device manager image.
# Expected:    - GET .../instances/<name>/metrics returns one sample carrying timestamp and all
#              three pairs: cpuTotalMilliCores/cpuUsedMilliCores, memoryTotalMiB/memoryUsedMiB,
#              storageTotalMiB/storageUsedMiB;
#              - each total equals the sum of the backing pod's container limits, since a total is
#              the Instance's declaration rather than a measurement;
#              - the used figures belong to the backing pod (cpu moves after a load burst);
#              - once stopped, and so holding no Pod at all, the same call still answers: the declared
#              totals with zero measurements rather than an error, since nothing has run;
#              - a caller without the instances/metrics grant is denied
#              (kubectl auth can-i ... --as an unprivileged identity says no);
#              - exactly ONE device manager target on the node carries the pod-level families, so a
#              query never double-counts an Instance;
#              - every pod-level family carries a series for the Instance, labelled by exactly
#              namespace/instance_name/instance_uid/node, and its totals agree with the
#              subresource's sample;
#              - no series collides with Prometheus's own instance label, which Prometheus would
#              rename to exported_instance;
#              - accelerator families, where present, carry id, manufacturer and mode beside the
#              Instance's labels;
#              - every device manager target reports on its own sources.
# Cleanup:     Trap deletes the test Instance; idempotent, runs on pass AND fail. The InstanceType
#              is only read.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

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

# The derived general InstanceType is created with its unit spec, and that spec is immutable
# afterwards, so there is nothing to set: read it as a precondition. The Instance webhook sizes the
# Pod from it.
unit_ram=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.spec.unitResources.ram}' 2>/dev/null)
[ -n "$unit_ram" ] || { echo "no unit spec on ${IT} — the Instance webhook needs unitRAM to size the Pod"; exit 1; }

# 1. Create the Instance on the general pool. Both halves of this case read the SAME Instance: the
#    subresource through the kubelet, the device manager through its own exporter.
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

# 5. The exporter half. The device manager on the Instance's node publishes the Instance as
#    Prometheus gauges; the subresource half above read the very same Instance, so it must still
#    be Ready here.
phase=$(kubectl -n default get instance "$INST" -o jsonpath='{.status.phase}' 2>/dev/null)
[ "$phase" = "Ready" ] || record FAIL "instance ready" "phase='${phase:-<EMPTY>}'"

INST_UID=$(kubectl -n default get instance "$INST" -o jsonpath='{.metadata.uid}' 2>/dev/null)
NODE=$(kubectl -n default get pod "$INST" -o jsonpath='{.spec.nodeName}' 2>/dev/null)

# The device managers of THAT node, Ready ones only: a pod that is not Ready is not scraped, and
# is not in the running to be the node's exporter either. A read loop rather than mapfile, which
# bash 3.2 does not have.
DM_PODS=()
if [ -n "$NODE" ]; then
  while IFS= read -r pod; do
    [ -n "$pod" ] && DM_PODS+=("$pod")
  done < <(kubectl -n "$NS" get pods \
    -l app.kubernetes.io/component=device-manager \
    --field-selector "spec.nodeName=${NODE},status.phase=Running" \
    -o jsonpath='{range .items[*]}{.metadata.name}{" "}{range .status.conditions[?(@.type=="Ready")]}{.status}{end}{"\n"}{end}' 2>/dev/null \
    | awk '$2=="True"{print $1}')
fi

if [ -z "$NODE" ] || [ "${#DM_PODS[@]}" -eq 0 ]; then
  echo "== CASE 37 — EXPORTER HALF SKIPPED =="
  echo "No Ready device manager pod runs on ${NODE:-<no node>}, the node the test Instance landed on."
  echo "The exporter lives in the device manager, so on a node without one this surface has nothing to"
  echo "publish. The metrics subresource half still answers for the Instance. Run this case on a"
  echo "cluster whose general pool includes a node carrying accelerators."
else
  echo "[case-37] node ${NODE} runs ${#DM_PODS[@]} Ready device manager pod(s): ${DM_PODS[*]}"

  # scrape <pod> — that pod's /metrics, through the API server's pod proxy so the device manager image
  # needs no client tooling. The port is the container's own, not a Service port.
  scrape() {
    local pod=$1 port
    port=$(kubectl -n "$NS" get pod "$pod" \
      -o jsonpath='{.spec.containers[0].ports[?(@.name=="https")].containerPort}' 2>/dev/null)
    kubectl get --raw "/api/v1/namespaces/${NS}/pods/https:${pod}:${port:-32443}/proxy/metrics" 2>/dev/null
  }

  # Which target carries the pod-level families for this Instance. The poller samples on the
  # monitor period, so the first scrape after the Instance goes Ready can legitimately miss it.
  POD_FAMILIES="gpustack_instance_cpu_total_millicores gpustack_instance_cpu_used_millicores \
gpustack_instance_memory_total_mib gpustack_instance_memory_used_mib \
gpustack_instance_storage_total_mib gpustack_instance_storage_used_mib"

  EXPORTING=()
  SCRAPE=""
  for _ in $(seq 1 12); do
    EXPORTING=()
    for pod in "${DM_PODS[@]}"; do
      body=$(scrape "$pod")
      [ -n "$body" ] || continue
      if printf '%s' "$body" | grep -q "^gpustack_instance_cpu_total_millicores{.*instance_uid=\"${INST_UID}\""; then
        EXPORTING+=("$pod")
        SCRAPE=$body
      fi
    done
    [ "${#EXPORTING[@]}" -ge 1 ] && break
    sleep 5
  done

  if [ "${#EXPORTING[@]}" -eq 1 ]; then
    record PASS "exactly one target carries the pod-level families" \
      "${EXPORTING[0]} of ${#DM_PODS[@]} device manager pod(s) on ${NODE}"
  elif [ "${#EXPORTING[@]}" -eq 0 ]; then
    record FAIL "exactly one target carries the pod-level families" \
      "no device manager pod on ${NODE} published instance_uid=${INST_UID} within 60s"
  else
    record FAIL "exactly one target carries the pod-level families" \
      "${#EXPORTING[@]} pods published it (${EXPORTING[*]}) — the node's Instances are double-counted"
  fi

  # What the exporting target actually published for this Instance: every pod-level family
  # present, labelled by exactly the four identifying labels with the Instance's own values, and
  # carrying the same figures the subresource serves. One parse, three verdicts.
  if [ -n "$SCRAPE" ]; then
    sample=$(kubectl get --raw "$RAW" 2>/dev/null)
    while IFS='|' read -r check state detail; do
      [ -n "$check" ] && record "$state" "$check" "$detail"
    done <<VERDICTS
$(printf '%s' "$SCRAPE" | python3 -c '
import json, re, sys

families = sys.argv[1].split()
identity = {"namespace": sys.argv[2], "instance_name": sys.argv[3],
            "instance_uid": sys.argv[4], "node": sys.argv[5]}
sample = (json.loads(sys.argv[6]) or {}).get("sample") or {}

# The total families are declarations both surfaces derive from the same Pod, so they are the ones
# that must match to the digit. A used figure is measured per read and legitimately moves between
# the poll behind this scrape and the subresource call above.
totals = {"gpustack_instance_cpu_total_millicores": "cpuTotalMilliCores",
          "gpustack_instance_memory_total_mib": "memoryTotalMiB",
          "gpustack_instance_storage_total_mib": "storageTotalMiB"}

line = re.compile(r"^([a-z_]+)\{([^}]*)\}\s+([0-9.e+-]+)$")
label = re.compile(r"([a-z_]+)=\"([^\"]*)\"")

seen, wrong_labels, disagreed, compared = {}, [], [], []
for raw in sys.stdin:
    m = line.match(raw.strip())
    if not m:
        continue
    name, labels, value = m.group(1), dict(label.findall(m.group(2))), m.group(3)
    if name not in families or labels.get("instance_uid") != identity["instance_uid"]:
        continue
    seen[name] = value

    if set(labels) != set(identity):
        wrong_labels.append(name + ": " + ",".join(sorted(labels)))
    else:
        mismatched = [k for k, v in labels.items() if identity[k] != v]
        if mismatched:
            wrong_labels.append(name + ": " + ",".join(sorted(mismatched)) + " not this Instance")

    if name in totals:
        field = totals[name]
        compared.append(field + "=" + str(sample.get(field)))
        if float(value) != float(sample.get(field, -1)):
            disagreed.append(name + "=" + value + " but sample." + field + "=" + str(sample.get(field)))

def verdict(check, ok, detail):
    print(check + "|" + ("PASS" if ok else "FAIL") + "|" + detail)

missing = [f for f in families if f not in seen]
verdict("every pod-level family carries the Instance", not missing,
        ", ".join(sorted(k + "=" + v for k, v in seen.items())) if not missing
        else "no series for: " + ", ".join(missing))
verdict("their labels are exactly the four identifying ones", not wrong_labels,
        ",".join(sorted(identity)) if not wrong_labels else "; ".join(wrong_labels))
verdict("the totals agree with the subresource", bool(compared) and not disagreed,
        ", ".join(sorted(compared)) if compared and not disagreed
        else ("; ".join(disagreed) or "no total family matched the Instance"))
' "$POD_FAMILIES" default "$INST" "$INST_UID" "$NODE" "${sample:-null}" 2>/dev/null)
VERDICTS

    # The reserved target label. `instance_name` exists precisely so the exposition never carries a
    # bare `instance`, which Prometheus renames to exported_instance under honor_labels: false.
    collide=$(printf '%s' "$SCRAPE" | grep -c "^gpustack_instance_.*[{,]instance=\"")
    if [ "${collide:-1}" -eq 0 ]; then
      record PASS "no series collides with Prometheus's own instance label" "no bare instance= on any gpustack_instance_ series"
    else
      record FAIL "no series collides with Prometheus's own instance label" "${collide} series carry a bare instance= label"
    fi

    # Accelerator families, when the node runs an accelerated Instance. A CPU-only node has none,
    # which is not a failure of anything — say so rather than pass vacuously.
    acc=$(printf '%s' "$SCRAPE" | grep -m1 "^gpustack_instance_accelerator_memory_total_mib{")
    # mode is part of the set: every accelerator family reports the Instance's own grant and usage
    # whatever the allocation did, and mode is what a query groups or filters by rather than what it
    # picks a metric name with.
    ACC_WANT="id,index,instance_name,instance_uid,manufacturer,mode,namespace,node"
    if [ -z "$acc" ]; then
      record SKIP "accelerator families carry id, index, manufacturer and mode" \
        "no accelerator series on ${NODE}: no Instance here holds an accelerator, so there is none to label"
    else
      acc_labels=$(printf '%s' "$acc" | sed -e 's/^[^{]*{//' -e 's/}.*$//' \
        | tr ',' '\n' | cut -d= -f1 | sort | tr '\n' ',')
      if [ "${acc_labels%,}" = "$ACC_WANT" ]; then
        record PASS "accelerator families carry id, index, manufacturer and mode" "${acc_labels%,}"
      else
        record FAIL "accelerator families carry id, index, manufacturer and mode" \
          "label set is ${acc_labels%,}, expected ${ACC_WANT}"
      fi
    fi
  fi

  # Every target reports on its own sources, whichever of them is the exporter: a device manager
  # whose kubelet read is failing must say so here rather than only in its log.
  for pod in "${DM_PODS[@]}"; do
    body=$(scrape "$pod")
    ok=$(printf '%s' "$body" | grep -m1 '^gpustack_instance_metrics_collector_success{source="kubelet"}' | awk '{print $2}')
    dur=$(printf '%s' "$body" | grep -m1 '^gpustack_instance_metrics_collector_duration_seconds{source="kubelet"}' | awk '{print $2}')
    snap=$(printf '%s' "$body" | grep -m1 '^gpustack_instance_metrics_collector_success{source="snapshot"}' | awk '{print $2}')
    if [ "$ok" = "1" ] && [ -n "$dur" ] && [ -n "$snap" ]; then
      record PASS "${pod} reports on its own sources" "kubelet success=1 duration=${dur}s, snapshot success=${snap}"
    else
      record FAIL "${pod} reports on its own sources" \
        "kubelet success='${ok:-<absent>}' duration='${dur:-<absent>}' snapshot success='${snap:-<absent>}'"
    fi
  done
fi

# 6. An Instance that has started nothing is answered, not refused. Stopping it takes its Pod away,
#    which is the same state as never-scheduled and as a previous incarnation's Pod: nothing has run, so
#    nothing has been used. The surface must serve its DECLARED totals with zero measurements rather than
#    an error — a console reading it should need no branch for an Instance that is merely stopped.
#    The stop comes last: it takes the backing Pod away, and the exporter half above reads that Pod.
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

# 7. Authorization: an unprivileged identity must not hold get on instances/metrics.
deny=$(kubectl auth can-i get instances.worker.gpustack.ai/metrics -n default \
  --as system:serviceaccount:default:default 2>/dev/null)
if [ "$deny" = "no" ]; then
  record PASS "unprivileged caller denied" "system:serviceaccount:default:default cannot get instances/metrics"
else
  record FAIL "unprivileged caller denied" "can-i answered '${deny:-<error>}' for the default SA"
fi

# Results.
print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-37] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-37] all checks passed"
