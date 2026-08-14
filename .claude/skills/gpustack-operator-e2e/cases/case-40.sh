#!/usr/bin/env bash
#
# CASE 40 — The device manager exports this node's Instances as Prometheus gauges, from exactly one target
#   (MUTATING, self-recovering; AUTO-SKIPS without a device manager pod on the Instance's node)
#
#   case-40.sh <NS>
#
# <NS> is the operator's own namespace, where the device manager pods live. The Instance goes to
# `default`: the Instance webhook rejects a reserved namespace.
#
# Goal:        A device manager publishes every Instance of its own node on /metrics as gauges the
#              Instance metrics subresource agrees with field for field, labelled by the Instance's
#              identity and node; and on a node running several device managers exactly ONE of them
#              publishes the pod-level families, so a query never double-counts an Instance.
# Environment: Any cluster with a materialized scheduling chain (run case-1 first) whose Instance
#              lands on a node running at least one Ready device manager pod. AUTO-SKIPS (exit 0)
#              when it does not — a node with no accelerators runs no device manager, and then this
#              surface legitimately has nothing to publish. No GPU required; the accelerator
#              families are asserted only when the scrape carries any.
# Inputs:      All real, nothing mocked — sets the general InstanceType unit spec; a CPU Instance
#              gpustack-e2e-exporter (alpine sleep) on the general pool. Reads /metrics through the
#              API server's pod proxy, so nothing has to be installed in the device manager image.
# Expected:    - every pod-level family (cpu/memory/storage, total and used) carries a series for
#                the test Instance;
#              - their label set is exactly namespace/instance_name/instance_uid/node, carrying the
#                Instance's own identity — and no bare `instance` label, which Prometheus would
#                rename to exported_instance;
#              - the gauge values equal the subresource's sample for the same Instance;
#              - exactly ONE device manager target on the node carries the pod-level families,
#                however many manufacturers the node runs;
#              - every target reports on itself: collector success/duration for source=kubelet;
#              - accelerator families, where present, carry id, manufacturer and mode beside the
#                Instance's labels.
# Cleanup:     Trap deletes the test Instance and restores the InstanceType unit spec it patched
#              when there was one to restore; idempotent, runs on pass AND fail.
set -uo pipefail

NS="${1:?usage: case-40.sh <NS>}"
INST=gpustack-e2e-exporter
IT=$(kubectl get instancetypes.worker.gpustack.ai \
  -o jsonpath='{.items[?(@.spec.acceleratable==false)].metadata.name}' 2>/dev/null | tr ' ' '\n' | grep -m1 'gpustack-')
[ -n "$IT" ] || { echo "no general InstanceType found — run case-1 first to materialize the chain"; exit 1; }

# The unit spec is shared baseline: capture it before patching so the trap can put it back.
UNIT_BEFORE=$(kubectl get instancetypes.worker.gpustack.ai "$IT" \
  -o jsonpath='{.spec.unitResources}' 2>/dev/null)
STORAGE_BEFORE=$(kubectl get instancetypes.worker.gpustack.ai "$IT" \
  -o jsonpath='{.spec.localStorage}' 2>/dev/null)

restore() {
  echo
  echo "[case-40] cleanup: deleting test Instance"
  kubectl -n default delete instance "$INST" --ignore-not-found 2>/dev/null || true
  if [ -n "$UNIT_BEFORE" ]; then
    echo "[case-40] cleanup: restoring ${IT} unit spec"
    kubectl patch instancetypes.worker.gpustack.ai "$IT" --type=merge \
      -p "{\"spec\":{\"unitResources\":${UNIT_BEFORE},\"localStorage\":\"${STORAGE_BEFORE}\"}}" >/dev/null 2>&1 || true
  else
    echo "[case-40] note: ${IT} had no unit spec before this case; the one it set is left in place"
  fi
}
trap restore EXIT

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

results() {
  echo
  echo "STATUS | CHECK | OBJECT"
  for r in "${ROWS[@]}"; do echo "$r" | tr '|' ' ' | awk '{printf "%s | %s | %s\n", $1, $2, substr($0, index($0,$3))}'; done
  [ "$FAILS" -eq 0 ] || { echo "[case-40] ${FAILS} check(s) FAILED"; exit 1; }
  echo "[case-40] all checks passed"
}

# The Instance webhook needs a unit spec to size the Pod; confirm it stuck (the validating webhook
# may be briefly unready after a deploy).
unit_ram=""
for _ in $(seq 1 15); do
  kubectl patch instancetypes.worker.gpustack.ai "$IT" --type=merge \
    -p '{"spec":{"unitResources":{"cpu":"1","ram":"2Gi"},"localStorage":"10Gi"}}' >/dev/null 2>&1
  unit_ram=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.spec.unitResources.ram}' 2>/dev/null)
  [ -n "$unit_ram" ] && break
  sleep 3
done
[ -n "$unit_ram" ] || { echo "no unit spec on ${IT} (validating webhook not ready?)"; exit 1; }

# 1. An Instance to be exported.
echo "[case-40] creating Instance ${INST} of type ${IT}"
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

INST_UID=$(kubectl -n default get instance "$INST" -o jsonpath='{.metadata.uid}' 2>/dev/null)
NODE=$(kubectl -n default get pod "$INST" -o jsonpath='{.spec.nodeName}' 2>/dev/null)
[ -n "$NODE" ] || { echo "[case-40] the Instance has no backing pod on any node — cannot locate a device manager"; results; }

# 2. The device managers of THAT node, Ready ones only: a pod that is not Ready is not scraped, and
#    is not in the running to be the node's exporter either.
mapfile -t DM_PODS < <(kubectl -n "$NS" get pods \
  -l app.kubernetes.io/component=device-manager \
  --field-selector "spec.nodeName=${NODE},status.phase=Running" \
  -o jsonpath='{range .items[*]}{.metadata.name}{" "}{range .status.conditions[?(@.type=="Ready")]}{.status}{end}{"\n"}{end}' 2>/dev/null \
  | awk '$2=="True"{print $1}')

if [ "${#DM_PODS[@]}" -eq 0 ]; then
  echo "== CASE 40 — SKIPPED =="
  echo "No Ready device manager pod runs on ${NODE}, the node the test Instance landed on. The"
  echo "exporter lives in the device manager, so on a node without one this surface has nothing to"
  echo "publish — the metrics subresource still answers for the Instance (CASE 37). Run this case on"
  echo "a cluster whose general pool includes a node carrying accelerators."
  exit 0
fi
echo "[case-40] node ${NODE} runs ${#DM_PODS[@]} Ready device manager pod(s): ${DM_PODS[*]}"

# scrape <pod> — that pod's /metrics, through the API server's pod proxy so the device manager image
# needs no client tooling. The port is the container's own, not a Service port.
scrape() {
  local pod=$1 port
  port=$(kubectl -n "$NS" get pod "$pod" \
    -o jsonpath='{.spec.containers[0].ports[?(@.name=="https")].containerPort}' 2>/dev/null)
  kubectl get --raw "/api/v1/namespaces/${NS}/pods/https:${pod}:${port:-32443}/proxy/metrics" 2>/dev/null
}

# 3. Which target carries the pod-level families for this Instance. The poller samples on the
#    monitor period, so the first scrape after the Instance goes Ready can legitimately miss it.
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

# 4. What the exporting target actually published for this Instance: every pod-level family
#    present, labelled by exactly the four identifying labels with the Instance's own values, and
#    carrying the same figures the subresource serves. One parse, three verdicts.
if [ -n "$SCRAPE" ]; then
  sample=$(kubectl get --raw "/apis/worker.gpustack.ai/v1/namespaces/default/instances/${INST}/metrics" 2>/dev/null)
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

  # 5. The reserved target label. `instance_name` exists precisely so the exposition never carries a
  #    bare `instance`, which Prometheus renames to exported_instance under honor_labels: false.
  collide=$(printf '%s' "$SCRAPE" | grep -c "^gpustack_instance_.*[{,]instance=\"")
  if [ "${collide:-1}" -eq 0 ]; then
    record PASS "no series collides with Prometheus's own instance label" "no bare instance= on any gpustack_instance_ series"
  else
    record FAIL "no series collides with Prometheus's own instance label" "${collide} series carry a bare instance= label"
  fi

  # 7. Accelerator families, when the node runs an accelerated Instance. A CPU-only node has none,
  #    which is not a failure of anything — say so rather than pass vacuously.
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

# 8. Every target reports on its own sources, whichever of them is the exporter: a device manager
#    whose kubelet read is failing must say so here rather than only in its log.
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

results
