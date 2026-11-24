#!/usr/bin/env bash
#
# CASE 6 — Instance metrics subresource, Prometheus /metrics exporter (single target + accelerated
#          labels), and carved-card-only slice process pass
#          (MUTATING, self-recovering; exporter & carved-card legs auto-skip when absent)
#
#   case-6.sh <NS>
#
# Prerequisites: nodes=1; gpu=none; cluster=any; inputs=E2E_MOCK_IMAGE,E2E_WHOLE_LOAD_COMMAND,E2E_WHOLE_LOAD_IMAGE
# Goal:        Verify both Instance telemetry surfaces and the scoped per-process slice pass across
#              CPU, Mock Device Manager, and Real Device Manager clusters:
#                Phase 1 (Instance metrics subresource & Prometheus exporter):
#                  1. GET /apis/worker.gpustack.ai/v1/namespaces/default/instances/<name>/metrics
#                     serves timestamp and all three Total/Used pairs (CPU, memory, storage);
#                  2. Totals equal the sum of the backing Pod's non-init container limits;
#                  3. Used CPU tracks a real CPU load burst inside the container;
#                  4. On nodes running Ready device-manager Pod(s), exactly ONE target publishes
#                     pod-level gpustack_instance_* families with the four identifying labels
#                     (namespace, instance_name, instance_uid, node), matching subresource totals,
#                     zero bare instance= label collision, and collector self-metrics;
#                  5. Stopping the Instance (no backing Pod) still answers GET .../metrics with
#                     declared totals and present zero measurements (no 503);
#                  6. Unprivileged callers (system:serviceaccount:default:default) are denied get
#                     on instances/metrics.
#                Phase 2 (Carved-card-only slice process pass & accelerated exporter labels):
#                  When a node offers >=2 logically sliceable cards of a process-capable
#                  manufacturer (Mock or Real DM), deploying one whole-card Instance and one 50%
#                  sliced Instance on that node proves:
#                  1. Prometheus gpustack_instance_accelerator_* series carry id, index,
#                     instance_name, instance_uid, manufacturer, mode, namespace, and node;
#                  2. /monitor/snapshot slices.devices covers the carved card and NEVER queries the
#                     whole card (1 entry for 1 carved card, zero slices.usages on the whole card);
#                  3. Optional E2E_WHOLE_LOAD_IMAGE verifies live processes on the whole card
#                     reach neither slices.devices nor rowsNonInstance.
# Environment: Any cluster with a materialized general pool (run case-1 first). Phase 1's
#              subresource checks run on every CPU cluster; exporter checks auto-skip when no
#              device-manager Pod runs on the node; Phase 2 auto-skips when no node offers >=2
#              logically sliceable cards with a Running device-manager Pod (real or mock).
# Inputs:      One CPU Instance (e2e-c6-metrics) in Phase 1, plus one whole-card Instance
#              (e2e-c6-scope-whole) and one 50% sliced Instance (e2e-c6-scope-sliced) in Phase 2.
# Expected:    Every applicable check records PASS; absent hardware/exporter legs record SKIP.
# Cleanup:     EXIT trap deletes all test Instances. Idempotent; runs on pass AND fail.
set -uo pipefail

# shellcheck disable=SC2034 # Read by _rows-lib.sh.
CASE_ID=6
CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=/dev/null
. "${CASE_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_pool-lib.sh"

NS="${1:?usage: case-6.sh <NS>}"
WORK_IMAGE="${E2E_MOCK_IMAGE:-alpine}"
LOAD_IMAGE="${E2E_WHOLE_LOAD_IMAGE:-}"
LOAD_CMD="${E2E_WHOLE_LOAD_COMMAND:-}"

INST_CPU=e2e-c6-metrics
INST_WHOLE=e2e-c6-scope-whole
INST_SLICED=e2e-c6-scope-sliced
SLICEPCT=50

# Manufacturers whose real or mock detector implements AcceleratorProcessDetector.
PROCESS_MANUFACTURERS="nvidia amd ascend cambricon iluvatar metax mthreads hygon thead"

IT_GEN="$(kubectl get instancetypes.worker.gpustack.ai \
  -o jsonpath='{.items[?(@.spec.acceleratable==false)].metadata.name}' 2>/dev/null \
  | tr ' ' '\n' | grep -m1 'gpustack-' || true)"
[ -n "$IT_GEN" ] || { echo "no general InstanceType found — run case-1 first to materialize the chain"; exit 1; }

unit_ram="$(kubectl get instancetypes.worker.gpustack.ai "$IT_GEN" -o jsonpath='{.spec.unitResources.ram}' 2>/dev/null)"
[ -n "$unit_ram" ] || { echo "no unit spec on ${IT_GEN}"; exit 1; }

# shellcheck disable=SC2317 # Invoked via EXIT trap.
cleanup() {
  echo
  echo "[case-6] cleanup: deleting test Instances"
  kubectl -n default delete instance "$INST_CPU" "$INST_WHOLE" "$INST_SLICED" \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT

scrape_dm_pod() {
  local pod="$1" endpoint="${2:-metrics}" port
  port="$(kubectl -n "$NS" get pod "$pod" \
    -o jsonpath='{.spec.containers[0].ports[?(@.name=="https")].containerPort}' 2>/dev/null)"
  kubectl get --raw "/api/v1/namespaces/${NS}/pods/https:${pod}:${port:-32443}/proxy/${endpoint}" 2>/dev/null
}

ready_dm_pods_on_node() {
  local node="$1"
  kubectl -n "$NS" get pods \
    -l app.kubernetes.io/component=device-manager \
    --field-selector "spec.nodeName=${node},status.phase=Running" \
    -o jsonpath='{range .items[*]}{.metadata.name}{" "}{range .status.conditions[?(@.type=="Ready")]}{.status}{end}{"\n"}{end}' 2>/dev/null \
    | awk '$2=="True"{print $1}'
}

# ===================================================================================================
# Phase 1 — Instance metrics subresource & Prometheus /metrics exporter
# ===================================================================================================
echo
echo "[case-6] === Phase 1: Instance metrics subresource & Prometheus exporter ==="
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata: { name: ${INST_CPU}, namespace: default }
spec:
  type: ${IT_GEN}
  image: alpine
  command: ["sleep", "86400"]
  volume: { ephemeral: { capacity: 1Gi } }
EOF

phase_cpu=""
for _ in $(seq 1 40); do
  phase_cpu="$(kubectl -n default get instance "$INST_CPU" -o jsonpath='{.status.phase}' 2>/dev/null)"
  [ "$phase_cpu" = "Ready" ] && break
  sleep 3
done
[ "$phase_cpu" = "Ready" ] || record FAIL "CPU Instance reaches Ready" "phase='${phase_cpu:-<EMPTY>}'"

RAW_CPU="/apis/worker.gpustack.ai/v1/namespaces/default/instances/${INST_CPU}/metrics"
sample_checks=(
  ".sample.timestamp"
  ".sample.cpuTotalMilliCores"
  ".sample.cpuUsedMilliCores"
  ".sample.memoryTotalMiB"
  ".sample.memoryUsedMiB"
  ".sample.storageTotalMiB"
  ".sample.storageUsedMiB"
)

missing_fields() {
  local payload="$1" jp v out=""
  for jp in "${sample_checks[@]}"; do
    v="$(printf '%s' "$payload" | jq -r "if ${jp} == null then \"null\" else \"set\" end" 2>/dev/null)"
    [ "$v" = "set" ] || out="${out} ${jp}"
  done
  printf '%s' "$out"
}

latest=""
missing=""
for _ in $(seq 1 15); do
  latest="$(kubectl get --raw "$RAW_CPU" 2>/dev/null || true)"
  if [ -n "$latest" ]; then
    missing="$(missing_fields "$latest")"
    [ -z "$missing" ] && break
  fi
  sleep 3
done
if [ -z "$latest" ]; then
  record FAIL "current metrics sample served" "GET ${RAW_CPU} returned nothing"
elif [ -z "$missing" ]; then
  record PASS "current metrics sample served" \
    "$(printf '%s' "$latest" | jq -c '.sample | {cpuTotalMilliCores, cpuUsedMilliCores, memoryTotalMiB, memoryUsedMiB, storageTotalMiB, storageUsedMiB}' 2>/dev/null)"
else
  record FAIL "current metrics sample served" "missing fields:${missing}"
fi

totals_verdict="$(kubectl -n default get pod "$INST_CPU" -o json 2>/dev/null | python3 -c '
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
    return ceil(int(float(q[:-1]) * 1000), 1000) if q.endswith("m") else int(float(q) * 1000)

sample = json.loads(sys.argv[1])["sample"]
pod = json.load(sys.stdin)

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
' "$latest" 2>/dev/null || true)"
case "$totals_verdict" in
  PASS*) record PASS "totals equal declared Pod container limits" "${totals_verdict#PASS }" ;;
  FAIL*) record FAIL "totals equal declared Pod container limits" "${totals_verdict#FAIL }" ;;
  *)     record FAIL "totals equal declared Pod container limits" "could not compare Pod limits" ;;
esac

cpu_before="$(printf '%s' "$latest" | jq -r '.sample.cpuUsedMilliCores // 0' 2>/dev/null)"
# shellcheck disable=SC2016 # Expansions belong to the container's sh.
kubectl -n default exec "$INST_CPU" -c main -- sh -c \
  'pids=""; for i in 1 2 3 4 5 6 7 8; do (while :; do :; done) & pids="$pids $!"; done; sleep 25; kill $pids 2>/dev/null' \
  >/dev/null 2>&1 || true
cpu_after=""
for _ in $(seq 1 10); do
  cpu_after="$(kubectl get --raw "$RAW_CPU" 2>/dev/null | jq -r '.sample.cpuUsedMilliCores // 0' 2>/dev/null || true)"
  [ -n "$cpu_after" ] && [ "$cpu_after" -gt "$cpu_before" ] && break
  sleep 5
done
if [ -n "$cpu_after" ] && [ "$cpu_after" -gt "$cpu_before" ]; then
  record PASS "used CPU tracks backing Pod load" "cpuUsedMilliCores ${cpu_before} -> ${cpu_after}"
else
  record FAIL "used CPU tracks backing Pod load" "before=${cpu_before} after=${cpu_after:-<none>}"
fi

INST_UID="$(kubectl -n default get instance "$INST_CPU" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
CPU_NODE="$(kubectl -n default get pod "$INST_CPU" -o jsonpath='{.spec.nodeName}' 2>/dev/null)"

DM_PODS=()
if [ -n "$CPU_NODE" ]; then
  while IFS= read -r dmp; do
    [ -n "$dmp" ] && DM_PODS+=("$dmp")
  done < <(ready_dm_pods_on_node "$CPU_NODE")
fi

ACC_LABEL_CHECKED=0
ACC_WANT="id,index,instance_name,instance_uid,manufacturer,mode,namespace,node"

if [ -z "$CPU_NODE" ] || [ "${#DM_PODS[@]}" -eq 0 ]; then
  record SKIP "exporter pod-level Prometheus series" "no Ready device-manager Pod runs on ${CPU_NODE:-<none>}"
else
  POD_FAMILIES="gpustack_instance_cpu_total_millicores gpustack_instance_cpu_used_millicores gpustack_instance_memory_total_mib gpustack_instance_memory_used_mib gpustack_instance_storage_total_mib gpustack_instance_storage_used_mib"
  EXPORTING=()
  SCRAPE=""
  for _ in $(seq 1 12); do
    EXPORTING=()
    for dmp in "${DM_PODS[@]}"; do
      body="$(scrape_dm_pod "$dmp" metrics || true)"
      [ -n "$body" ] || continue
      if printf '%s' "$body" | grep -q "^gpustack_instance_cpu_total_millicores{.*instance_uid=\"${INST_UID}\""; then
        EXPORTING+=("$dmp")
        SCRAPE="$body"
      fi
    done
    [ "${#EXPORTING[@]}" -ge 1 ] && break
    sleep 5
  done

  if [ "${#EXPORTING[@]}" -eq 1 ]; then
    record PASS "exactly one target carries pod-level families" "${EXPORTING[0]} of ${#DM_PODS[@]} DM pod(s) on ${CPU_NODE}"
  elif [ "${#EXPORTING[@]}" -eq 0 ]; then
    record FAIL "exactly one target carries pod-level families" "no DM pod on ${CPU_NODE} published instance_uid=${INST_UID}"
  else
    record FAIL "exactly one target carries pod-level families" "${#EXPORTING[@]} pods published it (${EXPORTING[*]})"
  fi

  if [ -n "$SCRAPE" ]; then
    cur_sample="$(kubectl get --raw "$RAW_CPU" 2>/dev/null || echo 'null')"
    while IFS='|' read -r chk st det; do
      [ -n "$chk" ] && record "$st" "$chk" "$det"
    done < <(printf '%s' "$SCRAPE" | python3 -c '
import json, re, sys

families = sys.argv[1].split()
identity = {"namespace": sys.argv[2], "instance_name": sys.argv[3],
            "instance_uid": sys.argv[4], "node": sys.argv[5]}
sample = (json.loads(sys.argv[6]) or {}).get("sample") or {}
totals = {"gpustack_instance_cpu_total_millicores": "cpuTotalMilliCores",
          "gpustack_instance_memory_total_mib": "memoryTotalMiB",
          "gpustack_instance_storage_total_mib": "storageTotalMiB"}

line_re = re.compile(r"^([a-z_]+)\{([^}]*)\}\s+([0-9.e+-]+)$")
label_re = re.compile(r"([a-z_]+)=\"([^\"]*)\"")

seen, wrong_labels, disagreed, compared = {}, [], [], []
for raw in sys.stdin:
    m = line_re.match(raw.strip())
    if not m:
        continue
    name, labels, value = m.group(1), dict(label_re.findall(m.group(2))), m.group(3)
    if name not in families or labels.get("instance_uid") != identity["instance_uid"]:
        continue
    seen[name] = value
    if set(labels) != set(identity):
        wrong_labels.append(name + ": " + ",".join(sorted(labels)))
    else:
        mm = [k for k, v in labels.items() if identity[k] != v]
        if mm:
            wrong_labels.append(name + ": " + ",".join(sorted(mm)))
    if name in totals:
        field = totals[name]
        compared.append(field + "=" + str(sample.get(field)))
        if float(value) != float(sample.get(field, -1)):
            disagreed.append(name + "=" + value + " vs " + str(sample.get(field)))

def emit(c, ok, d):
    print(c + "|" + ("PASS" if ok else "FAIL") + "|" + d)

missing = [f for f in families if f not in seen]
emit("every pod-level family carries the Instance", not missing,
     ", ".join(sorted(k + "=" + v for k, v in seen.items())) if not missing else "missing: " + ", ".join(missing))
emit("pod-level labels are the exact four identifying ones", not wrong_labels,
     ",".join(sorted(identity)) if not wrong_labels else "; ".join(wrong_labels))
emit("exporter totals agree with metrics subresource", bool(compared) and not disagreed,
     ", ".join(sorted(compared)) if compared and not disagreed else ("; ".join(disagreed) or "none"))
' "$POD_FAMILIES" default "$INST_CPU" "$INST_UID" "$CPU_NODE" "$cur_sample" 2>/dev/null)

    collide="$(printf '%s' "$SCRAPE" | grep -c '^gpustack_instance_.*[{,]instance="' || true)"
    if [ "${collide:-1}" -eq 0 ]; then
      record PASS "no series collides with Prometheus bare instance label" "0 bare instance= labels"
    else
      record FAIL "no series collides with Prometheus bare instance label" "${collide} series carry bare instance="
    fi

    acc_line="$(printf '%s' "$SCRAPE" | grep -m1 '^gpustack_instance_accelerator_memory_total_mib{' || true)"
    if [ -n "$acc_line" ]; then
      ACC_LABEL_CHECKED=1
      acc_labels="$(printf '%s' "$acc_line" | sed -e 's/^[^{]*{//' -e 's/}.*$//' \
        | tr ',' '\n' | cut -d= -f1 | sort | paste -sd, -)"
      if [ "$acc_labels" = "$ACC_WANT" ]; then
        record PASS "accelerator families carry id, index, manufacturer and mode" "$acc_labels"
      else
        record FAIL "accelerator families carry id, index, manufacturer and mode" "got '${acc_labels}', want '${ACC_WANT}'"
      fi
    fi
  fi

  for dmp in "${DM_PODS[@]}"; do
    b_self="$(scrape_dm_pod "$dmp" metrics || true)"
    ok_k="$(printf '%s' "$b_self" | grep -m1 '^gpustack_instance_metrics_collector_success{source="kubelet"}' | awk '{print $2}')"
    dur_k="$(printf '%s' "$b_self" | grep -m1 '^gpustack_instance_metrics_collector_duration_seconds{source="kubelet"}' | awk '{print $2}')"
    ok_s="$(printf '%s' "$b_self" | grep -m1 '^gpustack_instance_metrics_collector_success{source="snapshot"}' | awk '{print $2}')"
    if [ "$ok_k" = "1" ] && [ -n "$dur_k" ] && [ -n "$ok_s" ]; then
      record PASS "${dmp} reports collector source health" "kubelet=1 (${dur_k}s), snapshot=${ok_s}"
    else
      record FAIL "${dmp} reports collector source health" "kubelet='${ok_k:-?}' dur='${dur_k:-?}' snap='${ok_s:-?}'"
    fi
  done
fi

# Stopped Instance still answers GET .../metrics with declared totals and present 0 used figures.
kubectl -n default patch instance "$INST_CPU" --type=merge -p '{"spec":{"stop":true}}' >/dev/null 2>&1
stopped_json=""
stop_err=""
if [ "$(kubectl -n default get instance "$INST_CPU" -o jsonpath='{.spec.stop}' 2>/dev/null)" != "true" ]; then
  stop_err="spec.stop did not take"
else
  for _ in $(seq 1 25); do
    if ! kubectl -n default get pod "$INST_CPU" >/dev/null 2>&1; then
      stopped_json="$(kubectl get --raw "$RAW_CPU" 2>/dev/null || true)"
      [ -n "$stopped_json" ] && break
    fi
    sleep 3
  done
  [ -z "$stopped_json" ] && stop_err="GET ${RAW_CPU} returned nothing once Pod was gone"
fi
if [ -n "$stop_err" ]; then
  record FAIL "stopped Instance is answered with zero usage" "$stop_err"
else
  stop_gate="$(printf '%s' "$stopped_json" | python3 -c '
import json, sys
s = json.load(sys.stdin).get("sample", {})
bad = []
for total in ("cpuTotalMilliCores", "memoryTotalMiB", "storageTotalMiB"):
    if not s.get(total):
        bad.append(f"{total}={s.get(total)!r}")
for used in ("cpuUsedMilliCores", "memoryUsedMiB", "storageUsedMiB"):
    if s.get(used) != 0:
        bad.append(f"{used}={s.get(used)!r} (want 0)")
print("FAIL " + "; ".join(bad) if bad else "PASS")
' 2>/dev/null || true)"
  case "$stop_gate" in
    PASS)  record PASS "stopped Instance is answered with zero usage" "declared totals + 0 used, no 503" ;;
    FAIL*) record FAIL "stopped Instance is answered with zero usage" "${stop_gate#FAIL }" ;;
    *)     record FAIL "stopped Instance is answered with zero usage" "could not parse stopped sample" ;;
  esac
fi
kubectl -n default delete instance "$INST_CPU" --ignore-not-found --wait=false >/dev/null 2>&1 || true

deny_sa="$(kubectl auth can-i get instances.worker.gpustack.ai/metrics -n default \
  --as system:serviceaccount:default:default 2>/dev/null || true)"
if [ "$deny_sa" = "no" ]; then
  record PASS "unprivileged caller denied instances/metrics" "system:serviceaccount:default:default denied"
else
  record FAIL "unprivileged caller denied instances/metrics" "can-i answered '${deny_sa:-<error>}'"
fi

# ===================================================================================================
# Phase 2 — Carved-card-only slice process pass (/monitor/snapshot) & accelerated exporter labels
# ===================================================================================================
echo
echo "[case-6] === Phase 2: carved-card-only slice process pass (/monitor/snapshot) ==="

read -r SL_NODE SL_NCARDS SL_GID SL_MANUF <<<"$(kubectl get devices.worker.gpustack.ai -o json 2>/dev/null \
  | MFRS="$PROCESS_MANUFACTURERS" python3 -c "
import json, os, sys
eligible = set(os.environ.get('MFRS', '').split())
best = ('', 0, '', '')
for d in json.load(sys.stdin).get('items', []):
    for g in d.get('spec', {}).get('groups', []):
        if g.get('manufacturer') not in eligible:
            continue
        n = sum(1 for a in g.get('accelerators', [])
                if (a.get('status', {}).get('logicalSliced', {}).get('count', 0) or 0) > 0
                and not (a.get('status', {}).get('unhealthy', False)))
        if n > best[1]:
            best = (d['metadata']['name'], n, g.get('id', ''), g.get('manufacturer', ''))
print(best[0], best[1], best[2], best[3])
" 2>/dev/null)"

DM_SL=""
if [ -n "$SL_NODE" ] && [ "${SL_NCARDS:-0}" -ge 2 ]; then
  DM_SL="$(kubectl -n "$NS" get pods -l app.kubernetes.io/component=device-manager \
    --field-selector "spec.nodeName=${SL_NODE},status.phase=Running" -o json 2>/dev/null \
    | jq -r --arg mf "$SL_MANUF" '
      [.items[]
       | select(any((.status.conditions // [])[]; .type == "Ready" and .status == "True"))
       | select((.metadata.labels["gpustack.ai/manufacturer"] // "") as $l | ($l == $mf or $l == "mock"))
       | .metadata.name][0] // ""
    ' 2>/dev/null)"
fi

if [ -z "$SL_NODE" ] || [ "${SL_NCARDS:-0}" -lt 2 ] || [ -z "$DM_SL" ]; then
  [ "$ACC_LABEL_CHECKED" -eq 0 ] && record SKIP "accelerator families carry id, index, manufacturer and mode" \
    "no multi-card accelerated node available to run accelerated Instances"
  record SKIP "carved-card-only slice process pass (/monitor/snapshot)" \
    "needs >=2 logically sliceable cards + Running device-manager Pod on one node (best='${SL_NODE:-none}', cards=${SL_NCARDS:-0}, dm='${DM_SL:-none}')"
else
  IT_ACC="$(discover_node_pool "$SL_NODE" "$SL_GID" --sliceable name 2>/dev/null || true)"
  if [ -z "$IT_ACC" ]; then
    record FAIL "carved-card-only slice process pass (/monitor/snapshot)" \
      "no sliceable InstanceType backs ${SL_NODE} group=${SL_GID}"
  else
    mk_scope_instance() {
      local name="$1" img="$2" cmd_json="$3" pct="${4:-}" sliced_yaml=""
      if [ -n "$pct" ]; then
        sliced_yaml="$(printf '\n    acceleratorSlicedMemoryPercentage: %s\n    acceleratorSlicedCoresPercentage: 100' "$pct")"
      fi
      cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: Instance
metadata: { name: ${name}, namespace: default }
spec:
  type: ${IT_ACC}
  nodeName: ${SL_NODE}
  image: ${img}
  command: ${cmd_json}
  resources:
    cpu: "500m"
    ram: "512Mi"
    localStorage: "2Gi"
    accelerator: "1"${sliced_yaml}
  volume: { ephemeral: { capacity: 1Gi } }
  volumeMount: /workspace
EOF
    }

    mk_scope_instance "$INST_WHOLE" "${LOAD_IMAGE:-$WORK_IMAGE}" "${LOAD_CMD:-[\"sleep\", \"86400\"]}"
    mk_scope_instance "$INST_SLICED" "$WORK_IMAGE" '["sleep", "86400"]' "$SLICEPCT"

    wait_inst_pod() {
      for _ in $(seq 1 50); do
        [ "$(kubectl -n default get pod "$1" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Running" ] && return 0
        sleep 3
      done
      return 1
    }

    if ! wait_inst_pod "$INST_WHOLE" || ! wait_inst_pod "$INST_SLICED"; then
      record FAIL "whole and sliced Instances reach Running on target node" \
        "whole=$(kubectl -n default get pod "$INST_WHOLE" -o jsonpath='{.status.phase}' 2>/dev/null) sliced=$(kubectl -n default get pod "$INST_SLICED" -o jsonpath='{.status.phase}' 2>/dev/null)"
    else
      granted_card() {
        local inst="$1" cid=""
        for _ in $(seq 1 20); do
          cid="$(kubectl get --raw "/apis/worker.gpustack.ai/v1/namespaces/default/instances/${inst}/metrics" 2>/dev/null \
            | jq -r '(.sample.accelerators // [])[0].id // ""' 2>/dev/null || true)"
          [ -n "$cid" ] && { printf '%s' "$cid"; return 0; }
          sleep 3
        done
        return 1
      }

      WHOLE_CARD="$(granted_card "$INST_WHOLE" || true)"
      SLICED_CARD="$(granted_card "$INST_SLICED" || true)"

      if [ -n "$WHOLE_CARD" ] && [ -n "$SLICED_CARD" ] && [ "$WHOLE_CARD" != "$SLICED_CARD" ]; then
        record PASS "whole and sliced Instances hold distinct cards on one node" \
          "${SL_NODE}: whole=${WHOLE_CARD} sliced=${SLICED_CARD}"
      else
        record FAIL "whole and sliced Instances hold distinct cards on one node" \
          "whole='${WHOLE_CARD:-<none>}' sliced='${SLICED_CARD:-<none>}'"
      fi

      # Verify accelerated exporter label set if not already checked in Phase 1.
      if [ "$ACC_LABEL_CHECKED" -eq 0 ]; then
        acc_line=""
        for _ in $(seq 1 12); do
          for dmp in $(ready_dm_pods_on_node "$SL_NODE"); do
            b_acc="$(scrape_dm_pod "$dmp" metrics || true)"
            acc_line="$(printf '%s' "$b_acc" | grep -m1 '^gpustack_instance_accelerator_memory_total_mib{' || true)"
            [ -n "$acc_line" ] && break 2
          done
          sleep 4
        done
        if [ -n "$acc_line" ]; then
          acc_labels="$(printf '%s' "$acc_line" | sed -e 's/^[^{]*{//' -e 's/}.*$//' \
            | tr ',' '\n' | cut -d= -f1 | sort | paste -sd, -)"
          if [ "$acc_labels" = "$ACC_WANT" ]; then
            record PASS "accelerator families carry id, index, manufacturer and mode" "$acc_labels"
          else
            record FAIL "accelerator families carry id, index, manufacturer and mode" "got '${acc_labels}', want '${ACC_WANT}'"
          fi
        else
          record FAIL "accelerator families carry id, index, manufacturer and mode" \
            "no gpustack_instance_accelerator_memory_total_mib series published on ${SL_NODE}"
        fi
      fi

      snap_json=""
      for _ in $(seq 1 30); do
        snap_json="$(scrape_dm_pod "$DM_SL" "monitor/snapshot" || true)"
        if [ -n "$snap_json" ] && printf '%s' "$snap_json" | jq -e --arg sc "$SLICED_CARD" \
          'any((.slices.devices // [])[]; .deviceID == $sc)' >/dev/null 2>&1; then
          break
        fi
        sleep 3
      done

      DEVS_COV="$(printf '%s' "$snap_json" | jq -r '(.slices.devices // [])[] | .deviceID // ""' 2>/dev/null || true)"
      USES_COV="$(printf '%s' "$snap_json" | jq -r '(.slices.usages // [])[] | .deviceID // ""' 2>/dev/null || true)"
      N_COV="$(printf '%s' "$DEVS_COV" | grep -c . || true)"

      if printf '%s\n' "$DEVS_COV" | grep -qxF "$SLICED_CARD"; then
        record PASS "carved card is covered by slice process pass" "slices.devices carries ${SLICED_CARD}"
      else
        record FAIL "carved card is covered by slice process pass" "slices.devices missing ${SLICED_CARD}"
      fi

      if ! printf '%s\n' "$DEVS_COV" | grep -qxF "$WHOLE_CARD"; then
        record PASS "whole card on same node is never queried by slice pass" "slices.devices omits ${WHOLE_CARD}"
      else
        record FAIL "whole card on same node is never queried by slice pass" "slices.devices queried whole card ${WHOLE_CARD}"
      fi

      if [ "${N_COV:-0}" -eq 1 ]; then
        record PASS "slice pass covered exactly the carved cards" "1 entry for 1 carved card"
      else
        record FAIL "slice pass covered exactly the carved cards" "${N_COV} entries: $(printf '%s' "$DEVS_COV" | tr '\n' ' ')"
      fi

      if ! printf '%s\n' "$USES_COV" | grep -qxF "$WHOLE_CARD"; then
        record PASS "no slices.usages record names the whole card" "0 usage records on ${WHOLE_CARD}"
      else
        record FAIL "no slices.usages record names the whole card" "slices.usages named whole card ${WHOLE_CARD}"
      fi

      if [ -n "$LOAD_IMAGE" ]; then
        non_inst="$(printf '%s' "$snap_json" | jq -r --arg sc "$SLICED_CARD" \
          '[(.slices.devices // [])[] | select(.deviceID == $sc) | .rowsNonInstance // 0][0] // 0' 2>/dev/null)"
        if ! printf '%s\n' "$DEVS_COV" | grep -qxF "$WHOLE_CARD" && [ "${non_inst:-0}" = "0" ]; then
          record PASS "real load on whole card reaches neither slices.devices nor rowsNonInstance" \
            "rowsNonInstance=0 on ${SLICED_CARD}"
        else
          record FAIL "real load on whole card reaches neither slices.devices nor rowsNonInstance" \
            "rowsNonInstance='${non_inst}'"
        fi
      else
        record SKIP "real load on whole card reaches neither slices.devices nor rowsNonInstance" \
          "set E2E_WHOLE_LOAD_IMAGE to measure live device processes on the whole card"
      fi
    fi
  fi
fi

echo
echo "== CASE 6 — Instance metrics subresource, Prometheus exporter & carved-card slice pass =="
print_rows
if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s)."
  exit 1
fi
echo "CASE 6 PASS"
