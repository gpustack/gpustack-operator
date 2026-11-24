#!/usr/bin/env bash
#
# CASE 3 — CPU Mock Detector + Allocator full scheduling chain: slice-% webhook, multi-vendor discovery,
#          AdmissionCheck hold & release, four-view & watch, cross-mode exclusion (EX/SH/SL), fit-label
#          fragmentation skip, and accelerated CPU awareness
#          (MUTATING, self-recovering; Phase A runs on any CPU cluster, Phase B runs when Mock DM is active)
#
#   case-3.sh <NS>
#
# Prerequisites: nodes=1; gpu=none; cluster=any; inputs=E2E_MOCK_IMAGE
# Goal:        Verify the Pod slice webhook and accelerated scheduling/admission chain end to end:
#                Phase A (runs on every CPU cluster):
#                  1. gpustack-node-devices AdmissionCheck is Active;
#                  2. Pod mutating webhook folds .sliced.memory-percentage into .sliced.units
#                     (x 16000) and defaults .sliced.cores-percentage=100; validating webhook
#                     rejects a memoryless .sliced Pod;
#                  3. InstanceType unitResources / localStorage are immutable after CREATE and a
#                     rejected edit leaves <node>-gpustack-worker NodeFeature labels untouched.
#                Phase B (runs when Mock device-manager DaemonSet is active; auto-skips cleanly
#                with SKIP rows when mock DM is not deployed):
#                  1. Real CPU Mock Detector + Allocator chain: Devices carries
#                     device.gpustack.ai/mock="true", multi-vendor discovery (nvidia + amd when
#                     unfiltered), CQ wires gpustack-node-devices, spec.os/arch match CQ labels,
#                     CPU alpine Pods requesting .exclusive, .shared, and .sliced reach Running
#                     with zero /dev/* or ld.so.preload host mounts and receive
#                     GPUSTACK_MOCK_ALLOCATED_DEVICES, GPUSTACK_MOCK_ALLOCATION_MODE,
#                     GPUSTACK_MOCK_MANUFACTURER, and vendor *_VISIBLE_DEVICES, while InstanceType
#                     four-view and native watch reflect occupancy transitions;
#                  2. AdmissionCheck over-admit hold & release: 4 x 60% sliced Pods occupy all 4
#                     cards while leaving 1.6 cards of Kueue credits free — a 1-card exclusive Pod
#                     passes Gate 1 credits, is held in Retry by Gate 3 AdmissionCheck, and admits
#                     to Running as soon as one sliced card frees;
#                  3. Cross-mode exclusion (EX/SH/SL): exclusive->shared (queued + raw),
#                     exclusive->sliced (free-card steering + full-node hold), and
#                     sliced->exclusive (sliced-card avoidance + full-node hold) never co-locate;
#                  4. Fit-label fragmentation skip: sliced-max-free-units.fit.gpustack.ai/* tracks
#                     the live ledger, workload-fit-affinity=false livelocks on a fragmented first
#                     node while true skips directly to a roomy second node (2-node TAS leg skips
#                     cleanly on 1-node clusters while still proving single-node fit-label & hold);
#                  5. Accelerated CPU awareness: instance-type-aware-cpu-manufacturer=true splits
#                     the accelerated InstanceType by CPU, populates status.detail.cpu alongside
#                     GPU descriptors, and runs 1-card and 2-card Instances reserving distinct
#                     mock card UUIDs in Devices.status.
# Environment: Any cluster with the operator installed. Phase A needs no GPU and no Mock DM.
#              Phase B activates automatically when mock_dm_detect_env finds a running mock
#              device-manager DaemonSet (gpustack.ai/manufacturer=mock).
# Inputs:      Real Pod/Instance/InstanceType admissions plus ConfigMap hot-reloads through
#              _mock-dm-lib.sh when Mock DM is active. Workload image defaults to alpine
#              (override via E2E_MOCK_IMAGE).
# Expected:    Phase A records PASS on every cluster; Phase B records PASS when Mock DM is active
#              (or SKIP rows when Mock DM is absent).
# Cleanup:     EXIT trap deletes all test Pods, Instances, Workloads, and throwaway/aware-window
#              InstanceTypes/AcceleratorTypes, restores gpustack-settings keys (restarting worker
#              if awareness was toggled), and restores the original mock ConfigMap via
#              mock_dm_restore_cm.
set -uo pipefail

# shellcheck disable=SC2034 # Read by _rows-lib.sh.
CASE_ID=3
CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=/dev/null
. "${CASE_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_pool-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_mock-dm-lib.sh"

NS="${1:?usage: case-3.sh <NS>}"
# shellcheck disable=SC2034 # Read by _rows-lib.sh setting_* helpers.
SYSTEM_NS="$NS"

MOCK_IMAGE="${E2E_MOCK_IMAGE:-alpine}"
AC=gpustack-node-devices
ANNO=device.gpustack.ai/accelerator.allocated
AWARE_KEY=instance-type-aware-cpu-manufacturer
FIT_SETTING=workload-fit-affinity
DERIVED_LABEL=schedule.gpustack.ai/derived-from-node

POD_WH_OK=e2e-c3-webhook-pct
POD_WH_BAD=e2e-c3-webhook-nomem
IMM_IT=e2e-c3-imm-probe

ORIG_CM_FILE="$(mktemp)"
CM_SNAPSHOT_OK=0
AWARE_BOUNCED=0
TEST_PODS=()
TEST_INSTANCES=()
BEFORE_ITS_JSON=""
BEFORE_ATS_JSON=""
ORIG_AWARE=""

settings_save "$AWARE_KEY" "$FIT_SETTING"
ORIG_AWARE="$(setting_get "$AWARE_KEY")"
BEFORE_ITS_JSON="$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null || echo '{"items":[]}')"
BEFORE_ATS_JSON="$(kubectl get acceleratortypes.worker.gpustack.ai -o json 2>/dev/null || echo '{"items":[]}')"

bounce_worker() {
  kubectl -n "$NS" rollout restart deploy/gpustack-operator-worker >/dev/null 2>&1 || return 1
  kubectl -n "$NS" rollout status deploy/gpustack-operator-worker --timeout=180s >/dev/null 2>&1
}

delete_pod_and_workload() {
  local p="$1" wl
  kubectl -n default delete pod "$p" --ignore-not-found --force --grace-period=0 >/dev/null 2>&1 || true
  for wl in $(kubectl -n default get workloads.kueue.x-k8s.io -o json 2>/dev/null | P="$p" python3 -c '
import json, os, sys
for wl in json.load(sys.stdin).get("items", []):
    if any(o.get("kind") == "Pod" and o.get("name") == os.environ["P"] for o in wl["metadata"].get("ownerReferences", [])):
        print(wl["metadata"]["name"])
' 2>/dev/null); do
    kubectl -n default delete workloads.kueue.x-k8s.io "$wl" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  done
}

clear_test_pods() {
  local p
  for p in ${TEST_PODS[@]+"${TEST_PODS[@]}"}; do
    delete_pod_and_workload "$p"
  done
  TEST_PODS=()
}

# shellcheck disable=SC2317 # Invoked via EXIT trap.
cleanup() {
  local inst it at
  echo
  echo "[case-3] cleanup: deleting test Pods/Instances, restoring settings and mock ConfigMap"
  delete_pod_and_workload "$POD_WH_OK"
  delete_pod_and_workload "$POD_WH_BAD"
  clear_test_pods

  for inst in ${TEST_INSTANCES[@]+"${TEST_INSTANCES[@]}"}; do
    kubectl -n default delete instance "$inst" --ignore-not-found --timeout=60s >/dev/null 2>&1 || true
    kubectl -n default wait --for=delete "pod/${inst}" --timeout=60s >/dev/null 2>&1 || true
  done

  if kubectl get instancetype "$IMM_IT" >/dev/null 2>&1; then
    kubectl patch instancetype "$IMM_IT" --type=merge -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
    kubectl delete instancetype "$IMM_IT" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  fi
  kubectl delete clusterqueue "$IMM_IT" --ignore-not-found --wait=false >/dev/null 2>&1 || true

  if [ "$CM_SNAPSHOT_OK" -eq 1 ]; then
    mock_dm_restore_cm "$NS" "$MOCK_DM_CM" "$ORIG_CM_FILE" || true
  fi
  rm -f "$ORIG_CM_FILE"

  settings_restore
  if [ "$AWARE_BOUNCED" -eq 1 ]; then
    bounce_worker || true
    if [ -z "$ORIG_AWARE" ] && [ -n "$(setting_get "$AWARE_KEY")" ]; then
      setting_unset "$AWARE_KEY" || true
    fi
  fi

  for it in $(kubectl get instancetypes.worker.gpustack.ai -l "${DERIVED_LABEL}=true" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
    if ! printf '%s' "$BEFORE_ITS_JSON" | jq -e --arg name "$it" '.items | any(.metadata.name == $name)' >/dev/null 2>&1; then
      kubectl delete instancetype "$it" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    fi
  done
  for at in $(kubectl get acceleratortypes.worker.gpustack.ai -l "${DERIVED_LABEL}=true" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
    if ! printf '%s' "$BEFORE_ATS_JSON" | jq -e --arg name "$at" '.items | any(.metadata.name == $name)' >/dev/null 2>&1; then
      kubectl delete acceleratortype "$at" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    fi
  done
}
trap cleanup EXIT

# ===================================================================================================
# Phase A — Always runs on any CPU cluster: AdmissionCheck Active, Pod slice-% webhook, immutability
# ===================================================================================================
echo
echo "[case-3] === Phase A: AdmissionCheck Active, Pod slice-% webhook, and InstanceType immutability ==="

ac_active="$(kubectl get admissioncheck "$AC" -o jsonpath='{.status.conditions[?(@.type=="Active")].status}' 2>/dev/null)"
if [ "$ac_active" = "True" ]; then
  record PASS "AdmissionCheck Active" "${AC} Active=True"
else
  record FAIL "AdmissionCheck Active" "got Active='${ac_active:-<missing>}'"
fi

delete_pod_and_workload "$POD_WH_OK"
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: ${POD_WH_OK}
  namespace: default
  labels:
    kueue.x-k8s.io/queue-name: gpustack-e2e-c3-any
spec:
  schedulerName: default-scheduler
  containers:
    - name: main
      image: ${MOCK_IMAGE}
      command: ["sleep", "86400"]
      resources:
        limits:
          nvidia.com/gpu.sliced: "1"
          nvidia.com/gpu.sliced.memory-percentage: "20"
        requests:
          nvidia.com/gpu.sliced: "1"
          nvidia.com/gpu.sliced.memory-percentage: "20"
EOF

wh_units="$(kubectl -n default get pod "$POD_WH_OK" \
  -o jsonpath='{.spec.containers[0].resources.requests.nvidia\.com/gpu\.sliced\.units}' 2>/dev/null)"
wh_cores="$(kubectl -n default get pod "$POD_WH_OK" \
  -o jsonpath='{.spec.containers[0].resources.requests.nvidia\.com/gpu\.sliced\.cores-percentage}' 2>/dev/null)"
wh_units_n="$(python3 -c "
v = '${wh_units}'
m = {'k': 1000, 'M': 1000000, 'G': 1000000000}
print(int(float(v[:-1]) * m[v[-1]]) if v and v[-1] in m else (int(v) if v else ''))
" 2>/dev/null)"

if [ "$wh_units_n" = "320000" ]; then
  record PASS "memory-% folded to .sliced.units" "nvidia.com/gpu.sliced.units=${wh_units} (=320000)"
else
  record FAIL "memory-% folded to .sliced.units" "got '${wh_units:-<unset>}' (=${wh_units_n:-?}), want 320000"
fi
if [ "$wh_cores" = "100" ]; then
  record PASS "cores-% defaulted to 100" "nvidia.com/gpu.sliced.cores-percentage=${wh_cores}"
else
  record FAIL "cores-% defaulted to 100" "got '${wh_cores:-<unset>}', want 100"
fi
delete_pod_and_workload "$POD_WH_OK"

wh_err="$(cat <<EOF | kubectl apply -f - 2>&1 >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: ${POD_WH_BAD}
  namespace: default
  labels:
    kueue.x-k8s.io/queue-name: gpustack-e2e-c3-any
spec:
  schedulerName: default-scheduler
  containers:
    - name: main
      image: ${MOCK_IMAGE}
      command: ["sleep", "86400"]
      resources:
        limits:
          nvidia.com/gpu.sliced: "1"
        requests:
          nvidia.com/gpu.sliced: "1"
EOF
)"
wh_rc=$?
wh_reason="$(printf '%s' "$wh_err" | tr '\n' ' ' | tr -s ' ')"
if [ "$wh_rc" -ne 0 ] && printf '%s' "$wh_reason" | grep -qF "request must set nvidia.com/gpu.sliced.memory-percentage or nvidia.com/gpu.sliced.memory-mib"; then
  record PASS "memoryless .sliced Pod rejected" "validating webhook denied: $(printf '%s' "$wh_reason" | cut -c1-180)"
elif [ "$wh_rc" -ne 0 ]; then
  record FAIL "memoryless .sliced Pod rejected" "refused for unexpected reason: $(printf '%s' "$wh_reason" | cut -c1-180)"
else
  delete_pod_and_workload "$POD_WH_BAD"
  record FAIL "memoryless .sliced Pod rejected" "Pod was admitted without memory budget"
fi

# Accelerated InstanceType unit-spec immutability & worker NodeFeature isolation.
WORKER_NF_SAMPLE="$(kubectl -n "$NS" get nodefeatures -o jsonpath='{.items[*].metadata.name}' 2>/dev/null \
  | tr ' ' '\n' | grep -m1 -- '-gpustack-worker$' || true)"
nf_before=""
if [ -n "$WORKER_NF_SAMPLE" ]; then
  nf_before="$(kubectl -n "$NS" get nodefeature "$WORKER_NF_SAMPLE" -o json 2>/dev/null \
    | jq -cS '.spec.labels // {}' 2>/dev/null || true)"
fi

cat <<EOF | kubectl apply -f - >/dev/null 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: InstanceType
metadata:
  name: ${IMM_IT}
spec:
  generalGroup: e2ec3imm
  acceleratable: true
  acceleratorGroup: nvidia-e2ec3imm
  os: linux
  arch: amd64
  unitResources:
    cpu: "1"
    ram: "4Gi"
  localStorage: "32Gi"
EOF

cpu_before="$(kubectl get instancetypes.worker.gpustack.ai "$IMM_IT" -o jsonpath='{.spec.unitResources.cpu}' 2>/dev/null)"
err_edit="$(kubectl patch instancetypes.worker.gpustack.ai "$IMM_IT" --type=merge \
  -p '{"spec":{"unitResources":{"cpu":"2","ram":"8Gi"},"localStorage":"64Gi"}}' 2>&1)"
cpu_after="$(kubectl get instancetypes.worker.gpustack.ai "$IMM_IT" -o jsonpath='{.spec.unitResources.cpu}' 2>/dev/null)"
if printf '%s' "$err_edit" | grep -qi 'immutable' && [ -n "$cpu_after" ] && [ "$cpu_after" = "$cpu_before" ]; then
  record PASS "accelerated InstanceType unit-spec is immutable" "spec.unitResources.cpu stayed ${cpu_before}"
else
  record FAIL "accelerated InstanceType unit-spec is immutable" "cpu ${cpu_before}->${cpu_after}, err=${err_edit:0:90}"
fi

if [ -n "$WORKER_NF_SAMPLE" ]; then
  nf_after="$(kubectl -n "$NS" get nodefeature "$WORKER_NF_SAMPLE" -o json 2>/dev/null \
    | jq -cS '.spec.labels // {}' 2>/dev/null || true)"
  if [ "$nf_before" = "$nf_after" ]; then
    record PASS "rejected unit-spec edit leaves worker NodeFeature untouched" "${WORKER_NF_SAMPLE} labels unchanged"
  else
    record FAIL "rejected unit-spec edit leaves worker NodeFeature untouched" "${WORKER_NF_SAMPLE} labels changed"
  fi
fi

kubectl delete instancetype "$IMM_IT" --ignore-not-found --timeout=60s >/dev/null 2>&1 || true

# ===================================================================================================
# Phase B Gate — Check for active CPU Mock Device Manager
# ===================================================================================================
if ! mock_dm_detect_env "$NS" || ! mock_dm_manufacturer_allowed nvidia; then
  skip_why="mock device-manager DaemonSet (gpustack.ai/manufacturer=mock serving nvidia) is not active on this cluster"
  record SKIP "Phase B1: multi-vendor Mock Detector + Allocator full chain" "$skip_why"
  record SKIP "Phase B2: AdmissionCheck over-admit hold and release on Mock DM" "$skip_why"
  record SKIP "Phase B3: cross-mode exclusion (EX/SH/SL) on Mock DM" "$skip_why"
  record SKIP "Phase B4: fit-label fragmentation skip on Mock DM" "$skip_why"
  record SKIP "Phase B5: accelerated CPU awareness on Mock DM" "$skip_why"
  echo
  echo "== CASE 3 — Phase A verified; Phase B auto-skipped (${skip_why}) =="
  print_rows
  [ "$FAILS" -eq 0 ] || exit 1
  echo "CASE 3 PASS"
  exit 0
fi

if mock_dm_snapshot_cm "$NS" "$MOCK_DM_CM" "$ORIG_CM_FILE"; then
  CM_SNAPSHOT_OK=1
else
  echo "[case-3] FATAL: failed to snapshot ConfigMap ${NS}/${MOCK_DM_CM}"
  exit 1
fi

NODE_A="${MOCK_DM_NODES[0]}"
NODE_B=""
[ "${#MOCK_DM_NODES[@]}" -ge 2 ] && NODE_B="${MOCK_DM_NODES[1]}"
MULTI_MFR=0
mock_dm_manufacturer_allowed amd && MULTI_MFR=1
echo "[case-3] Mock DM active: ds=${MOCK_DM_DS} cm=${MOCK_DM_CM}/${MOCK_DM_KEY} nodeA=${NODE_A} nodeB=${NODE_B:-<none>} multiVendor=${MULTI_MFR}"

pod_phase() { kubectl -n default get pod "$1" -o jsonpath='{.status.phase}' 2>/dev/null; }
wait_pod_running() {
  local p="$1" tries="${2:-40}"
  for _ in $(seq 1 "$tries"); do
    [ "$(pod_phase "$p")" = "Running" ] && return 0
    sleep 3
  done
  return 1
}

pod_cards() {
  kubectl -n default get pod "$1" -o json 2>/dev/null | ANNO="$ANNO" python3 -c "
import json, os, sys
try:
    o = json.load(sys.stdin)
except Exception:
    sys.exit(0)
ann = (o.get('metadata', {}).get('annotations') or {}).get(os.environ['ANNO'], '')
if not ann:
    sys.exit(0)
try:
    st = json.loads(ann)
except Exception:
    sys.exit(0)
cards = set()
for c in st.values():
    for g in (c.get('devices') or {}).get('groups', []):
        gid = g.get('id', '')
        for a in g.get('accelerators', []):
            cards.add(('%s:%s' % (gid, a.get('id', ''))).replace(' ', '~'))
print(' '.join(sorted(cards)))
" 2>/dev/null
}

held_signal() {
  local p="$1" ph cond ev wl gates
  ph="$(pod_phase "$p")"
  [ "$ph" = "Failed" ] && { printf 'Failed/%s' "$(kubectl -n default get pod "$p" -o jsonpath='{.status.reason}' 2>/dev/null)"; return 0; }
  cond="$(kubectl -n default get pod "$p" -o jsonpath='{.status.conditions[?(@.type=="PodScheduled")].reason}' 2>/dev/null)"
  [ "$cond" = "Unschedulable" ] && { printf 'Unschedulable'; return 0; }
  ev="$(kubectl -n default get events --field-selector "involvedObject.name=${p}" \
    -o jsonpath='{range .items[*]}{.reason}{"\n"}{end}' 2>/dev/null \
    | grep -iE 'UnexpectedAdmissionError|FailedScheduling' | head -1 || true)"
  [ -n "$ev" ] && { printf '%s' "$ev"; return 0; }
  wl="$(kubectl -n default get workloads.kueue.x-k8s.io -o json 2>/dev/null | P="$p" python3 -c '
import json, os, sys
for wl in json.load(sys.stdin).get("items", []):
    if any(o.get("name") == os.environ["P"] for o in wl.get("metadata", {}).get("ownerReferences", [])):
        for c in wl.get("status", {}).get("admissionChecks", []):
            if c.get("state") in ("Retry", "Rejected"):
                print("admission-check(%s/%s)" % (c.get("name", ""), c.get("state", "")))
                break
        break
' 2>/dev/null)"
  [ -n "$wl" ] && { printf '%s' "$wl"; return 0; }
  gates="$(kubectl -n default get pod "$p" -o jsonpath='{.spec.schedulingGates[*].name}' 2>/dev/null)"
  [ -n "$gates" ] && { printf 'scheduling-gated(%s)' "$gates"; return 0; }
  printf ''
}

exec_env() {
  local p="$1" var="$2" out=""
  for _ in $(seq 1 10); do
    out="$(kubectl -n default exec "$p" -c main -- sh -c "printf '%s' \"\${${var}:-}\"" 2>/dev/null || true)"
    [ -n "$out" ] && { printf '%s' "$out"; return 0; }
    sleep 2
  done
  printf '%s' "$out"
}

mk_mock_pod() {
  local name="$1" node="$2" lq="$3" res_json="$4"
  local labels_json='{}'
  [ -n "$lq" ] && labels_json="{\"kueue.x-k8s.io/queue-name\":\"${lq}\"}"
  TEST_PODS+=("$name")
  jq -cn \
    --arg name "$name" \
    --arg node "$node" \
    --arg img "$MOCK_IMAGE" \
    --argjson labels "$labels_json" \
    --argjson res "$res_json" '
    {
      apiVersion: "v1",
      kind: "Pod",
      metadata: {name: $name, namespace: "default", labels: $labels},
      spec: (
        {
          schedulerName: "default-scheduler",
          restartPolicy: "Never",
          containers: [{
            name: "main",
            image: $img,
            command: ["sleep", "86400"],
            resources: {limits: $res, requests: $res}
          }]
        }
        + (if $node != "" then {nodeSelector: {"kubernetes.io/hostname": $node}} else {} end)
      )
    }' | kubectl apply -f - >/dev/null
}

# ===================================================================================================
# Phase B1 — Multi-vendor Mock Detector + Allocator env + InstanceType four-view & watch
# ===================================================================================================
echo
echo "[case-3] === Phase B1: multi-vendor Mock Detector + Allocator env + four-view & watch ==="

if [ "$MULTI_MFR" -eq 1 ]; then
  SPEC_B1="$(cat <<EOF
default:
  preset: amd-mi300x
  count: 1
nodes:
  ${NODE_A}:
    presets:
      - preset: nvidia-h100
        count: 2
      - preset: amd-mi300x
        count: 1
EOF
)"
else
  SPEC_B1="$(cat <<EOF
default:
  preset: nvidia-h100
  count: 1
nodes:
  ${NODE_A}:
    preset: nvidia-h100
    count: 2
EOF
)"
fi

mock_dm_apply_spec "$NS" "$MOCK_DM_CM" "$MOCK_DM_KEY" "$SPEC_B1"

if [ "$MULTI_MFR" -eq 1 ]; then
  want_b1_jq='(.metadata.annotations["device.gpustack.ai/mock"] == "true")
    and ([.spec.groups[] | select(.manufacturer == "nvidia") | .accelerators[]] | length == 2)
    and ([.spec.groups[] | select(.manufacturer == "amd") | .accelerators[]] | length == 1)'
else
  want_b1_jq='(.metadata.annotations["device.gpustack.ai/mock"] == "true")
    and ([.spec.groups[] | select(.manufacturer == "nvidia") | .accelerators[]] | length == 2)'
fi

if mock_dm_wait_devices "$NS" "$NODE_A" "$want_b1_jq" 90; then
  record PASS "Mock Detector stamps device.gpustack.ai/mock=true and multi-vendor groups" \
    "${NODE_A}: nvidia=2$([ "$MULTI_MFR" -eq 1 ] && printf ' amd=1' || printf '')"
else
  record FAIL "Mock Detector stamps device.gpustack.ai/mock=true and multi-vendor groups" \
    "${NODE_A} Devices did not converge within 90s"
fi

mock_dm_wait_node_allocatable "$NODE_A" \
  '(.["nvidia.com/gpu"] == "2") and (.["nvidia.com/gpu.shared"] == "20") and ((.["nvidia.com/gpu.sliced"] // "0" | tonumber) > 0)' 90 || true

NV_GID="$(kubectl get devices.worker.gpustack.ai "$NODE_A" -o json 2>/dev/null \
  | jq -r '[.spec.groups[] | select(.manufacturer == "nvidia") | .id][0] // ""')"

IT_NV=""
LQ_NV=""
for _ in $(seq 1 30); do
  read -r IT_NV LQ_NV <<<"$(discover_node_pool "$NODE_A" "$NV_GID" --sliceable --require-entrance name entrance 2>/dev/null || true)"
  [ -n "$IT_NV" ] && [ -n "$LQ_NV" ] && break
  sleep 3
done

if [ -n "$IT_NV" ] && [ -n "$LQ_NV" ]; then
  record PASS "derived Mock InstanceType Active with entrance queue" "${IT_NV} (entrance=${LQ_NV})"
else
  record FAIL "derived Mock InstanceType Active with entrance queue" "no pool for ${NODE_A} group=${NV_GID:-<none>}"
fi

if [ -n "$IT_NV" ]; then
  IT_NV_CQ="$(cq_of_instancetype "$NS" "$IT_NV")"
  ac_wired="$(kubectl get clusterqueue "$IT_NV_CQ" \
    -o jsonpath='{.spec.admissionChecksStrategy.admissionChecks[*].name}' 2>/dev/null \
    | tr ' ' '\n' | grep -x "$AC" || true)"
  if [ -n "$ac_wired" ]; then
    record PASS "backing ClusterQueue wires AdmissionCheck" "${IT_NV_CQ} -> ${AC}"
  else
    record FAIL "backing ClusterQueue wires AdmissionCheck" "${IT_NV_CQ} missing ${AC}"
  fi

  os_arch_ok="$(kubectl get instancetypes.worker.gpustack.ai "$IT_NV" -o json 2>/dev/null | jq -r '
    (.spec.os // "") as $sos | (.spec.arch // "") as $sarch |
    (.metadata.labels["kubernetes.io/os"] // "") as $los | (.metadata.labels["kubernetes.io/arch"] // "") as $larch |
    if ($sos != "" and $sarch != "" and $sos == $los and $sarch == $larch) then "PASS|\($sos)/\($sarch)"
    else "FAIL|spec=\($sos)/\($sarch) labels=\($los)/\($larch)" end
  ')"
  if [ "${os_arch_ok%%|*}" = "PASS" ]; then
    record PASS "accelerated InstanceType materializes spec.os/arch" "${IT_NV}: ${os_arch_ok#*|}"
  else
    record FAIL "accelerated InstanceType materializes spec.os/arch" "${IT_NV}: ${os_arch_ok#*|}"
  fi

  # Start a native watch before the exclusive Pod runs and releases so we verify watch transitions too.
  watch_file="$(mktemp)"
  base_ex="$(kubectl get instancetypes.worker.gpustack.ai "$IT_NV" -o jsonpath='{.status.accelerator.remaining}' 2>/dev/null)"
  ( timeout 75 kubectl get instancetypes.v1alpha1.worker.gpustack.ai "$IT_NV" -w \
      -o "jsonpath={.status.accelerator.remaining}{'\n'}" >"$watch_file" 2>/dev/null ) &
  wpid=$!
  sleep 2

  # 1) Exclusive CPU Pod
  P_EX=e2e-c3-mock-excl
  mk_mock_pod "$P_EX" "$NODE_A" "$LQ_NV" '{"nvidia.com/gpu":"1"}'
  if wait_pod_running "$P_EX" 40; then
    m_mode="$(exec_env "$P_EX" GPUSTACK_MOCK_ALLOCATION_MODE)"
    m_mfr="$(exec_env "$P_EX" GPUSTACK_MOCK_MANUFACTURER)"
    m_devs="$(exec_env "$P_EX" GPUSTACK_MOCK_ALLOCATED_DEVICES)"
    v_devs="$(exec_env "$P_EX" NVIDIA_VISIBLE_DEVICES)"
    no_preload="$(kubectl -n default exec "$P_EX" -c main -- sh -c '[ ! -s /etc/ld.so.preload ] && ls /dev/nvidia* >/dev/null 2>&1; echo $?' 2>/dev/null || true)"
    if [ "$m_mode" = "exclusive" ] && [ "$m_mfr" = "nvidia" ] && [ -n "$m_devs" ] && [ "$m_devs" = "$v_devs" ] && [ "$no_preload" != "0" ]; then
      record PASS "Mock Allocator .exclusive CPU Pod runs with diagnostic env & zero host devices" \
        "mode=${m_mode} NVIDIA_VISIBLE_DEVICES=${v_devs}"
    else
      record FAIL "Mock Allocator .exclusive CPU Pod runs with diagnostic env & zero host devices" \
        "mode='${m_mode}' mfr='${m_mfr}' mock='${m_devs}' vis='${v_devs}' devCheck='${no_preload}'"
    fi
  else
    record FAIL "Mock Allocator .exclusive CPU Pod runs with diagnostic env & zero host devices" \
      "${P_EX} phase=$(pod_phase "$P_EX")"
  fi

  # Wait for EX remaining to drop by 1 for the watch assertion, then release P_EX.
  want_drop=$(( ${base_ex:-2} - 1 ))
  for _ in $(seq 1 20); do
    cur_ex="$(kubectl get instancetypes.worker.gpustack.ai "$IT_NV" -o jsonpath='{.status.accelerator.remaining}' 2>/dev/null)"
    [ "$cur_ex" = "$want_drop" ] && break
    sleep 3
  done
  delete_pod_and_workload "$P_EX"
  for _ in $(seq 1 20); do
    cur_ex="$(kubectl get instancetypes.worker.gpustack.ai "$IT_NV" -o jsonpath='{.status.accelerator.remaining}' 2>/dev/null)"
    [ "$cur_ex" = "$base_ex" ] && break
    sleep 3
  done
  kill "$wpid" >/dev/null 2>&1 || true
  wait "$wpid" 2>/dev/null || true
  if grep -qx "$want_drop" "$watch_file" && grep -qx "$base_ex" "$watch_file"; then
    record PASS "four-view and native watch reflect occupancy transitions" \
      "observed accelerator.remaining ${base_ex}->${want_drop}->${base_ex}"
  else
    record FAIL "four-view and native watch reflect occupancy transitions" \
      "watch log=[$(tr '\n' ' ' <"$watch_file")]"
  fi
  rm -f "$watch_file"

  # 2) Shared CPU Pod
  P_SH=e2e-c3-mock-shared
  mk_mock_pod "$P_SH" "$NODE_A" "$LQ_NV" '{"nvidia.com/gpu.shared":"1"}'
  if wait_pod_running "$P_SH" 40; then
    m_mode="$(exec_env "$P_SH" GPUSTACK_MOCK_ALLOCATION_MODE)"
    m_devs="$(exec_env "$P_SH" GPUSTACK_MOCK_ALLOCATED_DEVICES)"
    v_devs="$(exec_env "$P_SH" NVIDIA_VISIBLE_DEVICES)"
    if [ "$m_mode" = "shared" ] && [ -n "$m_devs" ] && [ "$m_devs" = "$v_devs" ]; then
      record PASS "Mock Allocator .shared CPU Pod runs with diagnostic env" \
        "mode=${m_mode} NVIDIA_VISIBLE_DEVICES=${v_devs}"
    else
      record FAIL "Mock Allocator .shared CPU Pod runs with diagnostic env" \
        "mode='${m_mode}' mock='${m_devs}' vis='${v_devs}'"
    fi
  else
    record FAIL "Mock Allocator .shared CPU Pod runs with diagnostic env" "${P_SH} phase=$(pod_phase "$P_SH")"
  fi
  delete_pod_and_workload "$P_SH"

  # 3) Sliced CPU Pod
  P_SL=e2e-c3-mock-sliced
  mk_mock_pod "$P_SL" "$NODE_A" "$LQ_NV" '{"nvidia.com/gpu.sliced":"1","nvidia.com/gpu.sliced.memory-percentage":"25"}'
  if wait_pod_running "$P_SL" 40; then
    m_mode="$(exec_env "$P_SL" GPUSTACK_MOCK_ALLOCATION_MODE)"
    m_devs="$(exec_env "$P_SL" GPUSTACK_MOCK_ALLOCATED_DEVICES)"
    v_devs="$(exec_env "$P_SL" NVIDIA_VISIBLE_DEVICES)"
    if [ "$m_mode" = "sliced" ] && [ -n "$m_devs" ] && [ "$m_devs" = "$v_devs" ]; then
      record PASS "Mock Allocator .sliced CPU Pod runs with diagnostic env" \
        "mode=${m_mode} NVIDIA_VISIBLE_DEVICES=${v_devs}"
    else
      record FAIL "Mock Allocator .sliced CPU Pod runs with diagnostic env" \
        "mode='${m_mode}' mock='${m_devs}' vis='${v_devs}'"
    fi
  else
    record FAIL "Mock Allocator .sliced CPU Pod runs with diagnostic env" "${P_SL} phase=$(pod_phase "$P_SL")"
  fi
  clear_test_pods
fi

# ===================================================================================================
# Phase B2 — AdmissionCheck over-admit hold & release on 4x Mock H100
# ===================================================================================================
echo
echo "[case-3] === Phase B2: AdmissionCheck over-admit hold & release ==="
SPEC_B2="$(cat <<EOF
default:
  preset: amd-mi300x
  count: 1
nodes:
  ${NODE_A}:
    preset: nvidia-h100
    count: 4
EOF
)"
mock_dm_apply_spec "$NS" "$MOCK_DM_CM" "$MOCK_DM_KEY" "$SPEC_B2"
mock_dm_wait_devices "$NS" "$NODE_A" '([.spec.groups[] | select(.manufacturer == "nvidia") | .accelerators[]] | length == 4)' 90
mock_dm_wait_node_allocatable "$NODE_A" '.["nvidia.com/gpu"] == "4"' 90 || true

read -r IT_NV LQ_NV <<<"$(discover_node_pool "$NODE_A" "$NV_GID" --sliceable --require-entrance name entrance 2>/dev/null || true)"
mock_dm_wait_it ".items[] | select(.metadata.name == \"${IT_NV}\" and (.status.accelerator.remaining // 0) == 4)" 60 || true

# Place 4 x 60% sliced Pods so each of the 4 cards is in Sliced mode while 1.6 cards of Kueue credits stay free.
b2_ok=1
for idx in 1 2 3 4; do
  ps="e2e-c3-hold-sl-${idx}"
  mk_mock_pod "$ps" "$NODE_A" "$LQ_NV" '{"nvidia.com/gpu.sliced":"1","nvidia.com/gpu.sliced.memory-percentage":"60"}'
  if ! wait_pod_running "$ps" 40; then
    b2_ok=0
    break
  fi
  sleep 4
done

if [ "$b2_ok" -eq 1 ]; then
  mock_dm_wait_it ".items[] | select(.metadata.name == \"${IT_NV}\" and (.status.accelerator.remaining // 0) == 0)" 60 || true
  P_OVER=e2e-c3-overadmit-excl
  mk_mock_pod "$P_OVER" "" "$LQ_NV" '{"nvidia.com/gpu":"1"}'

  held_retry=""
  ac_state=""
  wl_admitted=""
  for _ in $(seq 1 40); do
    read -r ac_state wl_admitted <<<"$(kubectl -n default get workloads.kueue.x-k8s.io -o json 2>/dev/null | P="$P_OVER" AC="$AC" python3 -c '
import json, os, sys
for wl in json.load(sys.stdin).get("items", []):
    if any(o.get("kind") == "Pod" and o.get("name") == os.environ["P"] for o in wl["metadata"].get("ownerReferences", [])):
        st = wl.get("status", {})
        chk = next((c.get("state", "-") for c in st.get("admissionChecks", []) if c.get("name") == os.environ["AC"]), "-")
        adm = next((c.get("status", "False") for c in st.get("conditions", []) if c.get("type") == "Admitted"), "False")
        print(chk, adm)
        break
' 2>/dev/null)"
    [ "$ac_state" = "Retry" ] && [ "$wl_admitted" != "True" ] && { held_retry=1; break; }
    [ "$wl_admitted" = "True" ] && break
    sleep 3
  done

  if [ -n "$held_retry" ]; then
    record PASS "exclusive over-admit held by AdmissionCheck" \
      "AdmissionCheck state=Retry, Workload Admitted=${wl_admitted} (0 clean whole cards)"
    # Free one 60% sliced Pod -> 1 card becomes clean -> P_OVER must flip to Ready + Admitted + Running!
    delete_pod_and_workload "e2e-c3-hold-sl-1"
    released_ok=""
    for _ in $(seq 1 50); do
      if [ "$(pod_phase "$P_OVER")" = "Running" ]; then
        released_ok=1
        break
      fi
      sleep 3
    done
    if [ -n "$released_ok" ]; then
      record PASS "freed card releases held Workload to Running" "${P_OVER} Running after freeing 1 sliced card"
    else
      record FAIL "freed card releases held Workload to Running" "${P_OVER} phase=$(pod_phase "$P_OVER")"
    fi
  else
    record FAIL "exclusive over-admit held by AdmissionCheck" "state='${ac_state:-<none>}' admitted='${wl_admitted:-<none>}'"
    record SKIP "freed card releases held Workload to Running" "not attempted: request was not held"
  fi
else
  record FAIL "exclusive over-admit held by AdmissionCheck" "4 x 60% sliced setup Pods did not reach Running"
  record SKIP "freed card releases held Workload to Running" "setup incomplete"
fi
clear_test_pods

# ===================================================================================================
# Phase B3 — Cross-mode exclusion (EX / SH / SL) on 2x Mock H100
# ===================================================================================================
echo
echo "[case-3] === Phase B3: cross-mode exclusion (EX/SH/SL) ==="
SPEC_B3="$(cat <<EOF
default:
  preset: amd-mi300x
  count: 1
nodes:
  ${NODE_A}:
    preset: nvidia-h100
    count: 2
EOF
)"
mock_dm_apply_spec "$NS" "$MOCK_DM_CM" "$MOCK_DM_KEY" "$SPEC_B3"
mock_dm_wait_devices "$NS" "$NODE_A" '([.spec.groups[] | select(.manufacturer == "nvidia") | .accelerators[]] | length == 2)' 90
mock_dm_wait_node_allocatable "$NODE_A" '.["nvidia.com/gpu"] == "2"' 90 || true
mock_dm_wait_it ".items[] | select(.metadata.name == \"${IT_NV}\" and (.status.accelerator.remaining // 0) == 2)" 60 || true

run_ex_shared_variant() {
  local tag="$1" lq="$2"
  local p1="e2e-c3-${tag}-ex1" p2="e2e-c3-${tag}-ex2" ps="e2e-c3-${tag}-sh"
  mk_mock_pod "$p1" "$NODE_A" "$lq" '{"nvidia.com/gpu":"1"}'
  wait_pod_running "$p1" 40 || { record FAIL "cross-mode ${tag}: exclusive->shared held" "${p1} did not run"; clear_test_pods; return; }
  mk_mock_pod "$p2" "$NODE_A" "$lq" '{"nvidia.com/gpu":"1"}'
  wait_pod_running "$p2" 40 || { record FAIL "cross-mode ${tag}: exclusive->shared held" "${p2} did not run"; clear_test_pods; return; }

  local c1 c2
  c1="$(pod_cards "$p1")"
  c2="$(pod_cards "$p2")"
  if [ -z "$c1" ] || [ -z "$c2" ] || [ "$c1" = "$c2" ]; then
    record FAIL "cross-mode ${tag}: exclusive->shared held" "exclusive cards collided (${c1} vs ${c2})"
    clear_test_pods
    return
  fi

  mk_mock_pod "$ps" "$NODE_A" "$lq" '{"nvidia.com/gpu.shared":"1"}'
  local sig="" scards=""
  for _ in $(seq 1 15); do
    scards="$(pod_cards "$ps")"
    [ -n "$scards" ] && break
    sig="$(held_signal "$ps")"
    [ -n "$sig" ] && break
    sleep 3
  done
  if [ -z "$scards" ] && [ -n "$sig" ]; then
    record PASS "cross-mode ${tag}: exclusive->shared held" "shared Pod held [${sig}], zero co-location"
  else
    record FAIL "cross-mode ${tag}: exclusive->shared held" "cards='${scards:-<none>}' signal='${sig:-<none>}'"
  fi
  clear_test_pods
  mock_dm_wait_it ".items[] | select(.metadata.name == \"${IT_NV}\" and (.status.accelerator.remaining // 0) == 2)" 60 || true
}

run_ex_shared_variant "queued" "$LQ_NV"
run_ex_shared_variant "raw" ""

# Variant C: exclusive -> sliced (1 free card steers sliced Pod cleanly; 0 free cards holds sliced Pod)
mk_mock_pod "e2e-c3-c-ex1" "$NODE_A" "" '{"nvidia.com/gpu":"1"}'
if wait_pod_running "e2e-c3-c-ex1" 40; then
  c_ex1="$(pod_cards "e2e-c3-c-ex1")"
  mk_mock_pod "e2e-c3-c-sl1" "$NODE_A" "" '{"nvidia.com/gpu.sliced":"1","nvidia.com/gpu.sliced.memory-percentage":"20"}'
  if wait_pod_running "e2e-c3-c-sl1" 40; then
    c_sl1="$(pod_cards "e2e-c3-c-sl1")"
    if [ -n "$c_sl1" ] && [ "$c_sl1" != "$c_ex1" ]; then
      record PASS "cross-mode C: sliced claim steers to free card" "excl=${c_ex1}, sliced=${c_sl1}"
    else
      record FAIL "cross-mode C: sliced claim steers to free card" "excl=${c_ex1}, sliced=${c_sl1:-<none>}"
    fi
  else
    record FAIL "cross-mode C: sliced claim steers to free card" "e2e-c3-c-sl1 phase=$(pod_phase "e2e-c3-c-sl1")"
  fi
  delete_pod_and_workload "e2e-c3-c-sl1"
  mock_dm_wait_it ".items[] | select(.metadata.name == \"${IT_NV}\" and (.status.accelerator.remaining // 0) == 1)" 45 || true
  mk_mock_pod "e2e-c3-c-ex2" "$NODE_A" "" '{"nvidia.com/gpu":"1"}'
  if wait_pod_running "e2e-c3-c-ex2" 40; then
    mk_mock_pod "e2e-c3-c-slfull" "$NODE_A" "" '{"nvidia.com/gpu.sliced":"1","nvidia.com/gpu.sliced.memory-percentage":"20"}'
    sig_c=""
    sc_c=""
    for _ in $(seq 1 15); do
      sc_c="$(pod_cards "e2e-c3-c-slfull")"
      [ -n "$sc_c" ] && break
      sig_c="$(held_signal "e2e-c3-c-slfull")"
      [ -n "$sig_c" ] && break
      sleep 3
    done
    if [ -z "$sc_c" ] && [ -n "$sig_c" ]; then
      record PASS "cross-mode C: sliced claim held when all cards exclusive" "held [${sig_c}]"
    else
      record FAIL "cross-mode C: sliced claim held when all cards exclusive" "cards='${sc_c}' sig='${sig_c}'"
    fi
  else
    record FAIL "cross-mode C: sliced claim held when all cards exclusive" "e2e-c3-c-ex2 did not run"
  fi
else
  record FAIL "cross-mode C: sliced claim steers to free card" "e2e-c3-c-ex1 did not run"
  record SKIP "cross-mode C: sliced claim held when all cards exclusive" "precondition failed"
fi
clear_test_pods
mock_dm_wait_it ".items[] | select(.metadata.name == \"${IT_NV}\" and (.status.accelerator.remaining // 0) == 2)" 60 || true

# Variant D: sliced -> exclusive (exclusive avoids sliced card; 2nd exclusive held)
mk_mock_pod "e2e-c3-d-sl1" "$NODE_A" "" '{"nvidia.com/gpu.sliced":"1","nvidia.com/gpu.sliced.memory-percentage":"20"}'
if wait_pod_running "e2e-c3-d-sl1" 40; then
  c_dsl="$(pod_cards "e2e-c3-d-sl1")"
  mk_mock_pod "e2e-c3-d-ex1" "$NODE_A" "" '{"nvidia.com/gpu":"1"}'
  if wait_pod_running "e2e-c3-d-ex1" 40; then
    c_dex="$(pod_cards "e2e-c3-d-ex1")"
    if [ -n "$c_dex" ] && [ "$c_dex" != "$c_dsl" ]; then
      record PASS "cross-mode D: exclusive claim avoids sliced card" "sliced=${c_dsl}, excl=${c_dex}"
    else
      record FAIL "cross-mode D: exclusive claim avoids sliced card" "sliced=${c_dsl}, excl=${c_dex:-<none>}"
    fi
    mk_mock_pod "e2e-c3-d-exfull" "$NODE_A" "" '{"nvidia.com/gpu":"1"}'
    sig_d=""
    sc_d=""
    for _ in $(seq 1 15); do
      sc_d="$(pod_cards "e2e-c3-d-exfull")"
      [ -n "$sc_d" ] && break
      sig_d="$(held_signal "e2e-c3-d-exfull")"
      [ -n "$sig_d" ] && break
      sleep 3
    done
    if [ -z "$sc_d" ] && [ -n "$sig_d" ]; then
      record PASS "cross-mode D: exclusive claim held when remaining card is sliced" "held [${sig_d}]"
    else
      record FAIL "cross-mode D: exclusive claim held when remaining card is sliced" "cards='${sc_d}' sig='${sig_d}'"
    fi
  else
    record FAIL "cross-mode D: exclusive claim avoids sliced card" "e2e-c3-d-ex1 did not run"
    record SKIP "cross-mode D: exclusive claim held when remaining card is sliced" "precondition failed"
  fi
else
  record FAIL "cross-mode D: exclusive claim avoids sliced card" "e2e-c3-d-sl1 did not run"
  record SKIP "cross-mode D: exclusive claim held when remaining card is sliced" "precondition failed"
fi
clear_test_pods
mock_dm_wait_it ".items[] | select(.metadata.name == \"${IT_NV}\" and (.status.accelerator.remaining // 0) == 2)" 60 || true

# ===================================================================================================
# Phase B4 — Fit-label fragmentation skip (sliced-max-free-units.fit.gpustack.ai/*)
# ===================================================================================================
echo
echo "[case-3] === Phase B4: fit-label fragmentation skip ==="
AKEY_NV="nvidia-${NV_GID}"
FIT_KEY="sliced-max-free-units.fit.gpustack.ai/${AKEY_NV}"

wait_fit_label() {
  local node="$1" want="$2" got=""
  for _ in $(seq 1 35); do
    got="$(kubectl get node "$node" -o json 2>/dev/null \
      | jq -r --arg k "$FIT_KEY" '.metadata.labels[$k] // ""' 2>/dev/null)"
    [ "$got" = "$want" ] && { printf '%s' "$got"; return 0; }
    sleep 3
  done
  printf '%s' "$got"
  return 1
}

if [ -n "$NODE_B" ]; then
  SPEC_B4="$(cat <<EOF
default:
  preset: amd-mi300x
  count: 1
nodes:
  ${NODE_A}:
    preset: nvidia-h100
    count: 2
  ${NODE_B}:
    preset: nvidia-h100
    count: 2
EOF
)"
  mock_dm_apply_spec "$NS" "$MOCK_DM_CM" "$MOCK_DM_KEY" "$SPEC_B4"
  mock_dm_wait_devices "$NS" "$NODE_A" '([.spec.groups[] | select(.manufacturer == "nvidia") | .accelerators[]] | length == 2)' 90
  mock_dm_wait_devices "$NS" "$NODE_B" '([.spec.groups[] | select(.manufacturer == "nvidia") | .accelerators[]] | length == 2)' 90
  mock_dm_wait_node_allocatable "$NODE_B" '.["nvidia.com/gpu"] == "2"' 90 || true
fi

# Fragment NODE_A with 2 x 60% slices -> each card on NODE_A has 40% = 640000 free units.
mk_mock_pod "e2e-c3-frag-a1" "$NODE_A" "$LQ_NV" '{"nvidia.com/gpu.sliced":"1","nvidia.com/gpu.sliced.memory-percentage":"60"}'
wait_pod_running "e2e-c3-frag-a1" 40 || true
sleep 4
mk_mock_pod "e2e-c3-frag-a2" "$NODE_A" "$LQ_NV" '{"nvidia.com/gpu.sliced":"1","nvidia.com/gpu.sliced.memory-percentage":"60"}'
wait_pod_running "e2e-c3-frag-a2" 40 || true

fit_a="$(wait_fit_label "$NODE_A" "640000" || true)"
if [ "$fit_a" = "640000" ]; then
  record PASS "fit label tracks fragmented Mock ledger" "${NODE_A}: ${FIT_KEY}=640000"
else
  record FAIL "fit label tracks fragmented Mock ledger" "${NODE_A}: got '${fit_a:-<none>}', want 640000"
fi

if [ -n "$NODE_B" ]; then
  fit_b="$(wait_fit_label "$NODE_B" "1600000" || true)"
  setting_set "$FIT_SETTING" false
  settings_settle

  P_FIT_OFF=e2e-c3-fit-off
  mk_mock_pod "$P_FIT_OFF" "" "$LQ_NV" '{"nvidia.com/gpu.sliced":"1","nvidia.com/gpu.sliced.memory-percentage":"50"}'
  saw_retry_a=""
  for _ in $(seq 1 30); do
    obs="$(kubectl -n default get workloads.kueue.x-k8s.io -o json 2>/dev/null | P="$P_FIT_OFF" AC="$AC" python3 -c '
import json, os, sys
for wl in json.load(sys.stdin).get("items", []):
    if any(o.get("name") == os.environ["P"] for o in wl["metadata"].get("ownerReferences", [])):
        st = wl.get("status", {})
        chk = next((c.get("state", "") for c in st.get("admissionChecks", []) if c.get("name") == os.environ["AC"]), "")
        adm = next((c.get("status", "False") for c in st.get("conditions", []) if c.get("type") == "Admitted"), "False")
        print(chk, adm)
        break
' 2>/dev/null)"
    case "$obs" in
      "Retry False") saw_retry_a=1; break ;;
    esac
    sleep 3
  done
  if [ -n "$saw_retry_a" ] && [ "$fit_b" = "1600000" ]; then
    record PASS "without fit pin Workload hits Retry on fragmented first node" "${P_FIT_OFF} answered Retry"
  else
    record FAIL "without fit pin Workload hits Retry on fragmented first node" "obs='${obs:-<none>}' fitB='${fit_b:-<none>}'"
  fi
  delete_pod_and_workload "$P_FIT_OFF"

  setting_set "$FIT_SETTING" true
  settings_settle

  P_FIT_ON=e2e-c3-fit-on
  mk_mock_pod "$P_FIT_ON" "" "$LQ_NV" '{"nvidia.com/gpu.sliced":"1","nvidia.com/gpu.sliced.memory-percentage":"50"}'
  if wait_pod_running "$P_FIT_ON" 40; then
    placed_node="$(kubectl -n default get pod "$P_FIT_ON" -o jsonpath='{.spec.nodeName}' 2>/dev/null)"
    pod_fit_leak="$(kubectl -n default get pod "$P_FIT_ON" -o json 2>/dev/null | grep -c 'fit\.gpustack\.ai' || true)"
    if [ "$placed_node" = "$NODE_B" ] && [ "${pod_fit_leak:-0}" = "0" ]; then
      record PASS "with fit pin TAS skips fragmented node to roomy node" "${P_FIT_ON} Running on ${NODE_B}, 0 fit keys on Pod"
    else
      record FAIL "with fit pin TAS skips fragmented node to roomy node" "node='${placed_node}' fitMentions='${pod_fit_leak}'"
    fi
  else
    record FAIL "with fit pin TAS skips fragmented node to roomy node" "${P_FIT_ON} phase=$(pod_phase "$P_FIT_ON")"
  fi
  delete_pod_and_workload "$P_FIT_ON"
else
  record SKIP "2-node fit-pin TAS skip comparison" "only 1 mock node (${NODE_A}) present; testing single-node no-fit affinity hold"
  setting_set "$FIT_SETTING" true
  settings_settle
  P_NOFIT=e2e-c3-fit-nofit
  mk_mock_pod "$P_NOFIT" "" "$LQ_NV" '{"nvidia.com/gpu.sliced":"1","nvidia.com/gpu.sliced.memory-percentage":"50"}'
  sleep 15
  nofit_msg="$(kubectl -n default get workloads.kueue.x-k8s.io -o json 2>/dev/null | P="$P_NOFIT" python3 -c '
import json, os, sys
for wl in json.load(sys.stdin).get("items", []):
    if any(o.get("name") == os.environ["P"] for o in wl["metadata"].get("ownerReferences", [])):
        conds = {c["type"]: c for c in wl.get("status", {}).get("conditions", [])}
        qr = conds.get("QuotaReserved", {})
        print(qr.get("status", ""), qr.get("message", ""))
        break
' 2>/dev/null)"
  if printf '%s' "$nofit_msg" | grep -qi '^False .*affinity'; then
    record PASS "with no fitting node Workload holds no quota (affinity)" "${nofit_msg:0:120}"
  else
    record FAIL "with no fitting node Workload holds no quota (affinity)" "got '${nofit_msg:0:120}'"
  fi
  delete_pod_and_workload "$P_NOFIT"
fi

clear_test_pods

# ===================================================================================================
# Phase B5 — Accelerated CPU-manufacturer awareness on Mock DM
# ===================================================================================================
echo
echo "[case-3] === Phase B5: accelerated CPU-manufacturer awareness ==="
mock_dm_apply_spec "$NS" "$MOCK_DM_CM" "$MOCK_DM_KEY" "$SPEC_B3"
mock_dm_wait_devices "$NS" "$NODE_A" '([.spec.groups[] | select(.manufacturer == "nvidia") | .accelerators[]] | length == 2)' 90

ARF="$(kubectl get resourceflavors.kueue.x-k8s.io -o json 2>/dev/null | jq -r \
  --arg akey "$AKEY_NV" '
  [.items[]
   | select(.metadata.annotations["note.gpustack.ai/acceleratable"] == "true"
       and .metadata.annotations["note.gpustack.ai/acceleratorGroup"] == $akey)
   | .metadata.name] | sort | .[0] // ""')"

if [ -z "$ARF" ]; then
  record FAIL "aware accelerated InstanceType materializes" "no ResourceFlavor found for ${AKEY_NV}"
else
  GKEY="$(kubectl get resourceflavor "$ARF" -o jsonpath='{.metadata.annotations.note\.gpustack\.ai/generalGroup}' 2>/dev/null)"
  PRODUCT="$(kubectl get resourceflavor "$ARF" -o jsonpath='{.metadata.annotations.note\.gpustack\.ai/product}' 2>/dev/null)"
  MEMORY="$(kubectl get resourceflavor "$ARF" -o jsonpath='{.metadata.annotations.note\.gpustack\.ai/memory}' 2>/dev/null)"
  CORES="$(kubectl get resourceflavor "$ARF" -o jsonpath='{.metadata.annotations.note\.gpustack\.ai/cores}' 2>/dev/null)"
  RF_OS="$(kubectl get resourceflavor "$ARF" -o jsonpath='{.metadata.labels.kubernetes\.io/os}' 2>/dev/null)"
  RF_ARCH="$(kubectl get resourceflavor "$ARF" -o jsonpath='{.metadata.labels.kubernetes\.io/arch}' 2>/dev/null)"
  AWARE_IT="gpustack--${GKEY}--${AKEY_NV}-${RF_OS}-${RF_ARCH}"

  setting_set "$AWARE_KEY" true
  AWARE_BOUNCED=1
  bounce_worker

  if mock_dm_wait_it ".items[] | select(.metadata.name == \"${AWARE_IT}\" and .status.phase == \"Active\")" 90; then
    record PASS "aware accelerated InstanceType materializes Active" "$AWARE_IT"
  else
    record FAIL "aware accelerated InstanceType materializes Active" "${AWARE_IT} did not reach Active"
  fi

  it_info="$(kubectl get instancetype "$AWARE_IT" -o json 2>/dev/null | jq -r \
    --arg ag "$AKEY_NV" --arg gg "$GKEY" --arg prod "$PRODUCT" --arg mem "$MEMORY" --arg cores "$CORES" '
    ((.spec.acceleratorGroup // "") == $ag and (.spec.generalGroup // "") == $gg) as $id_ok |
    ((.status.detail.product // "") == $prod and (.status.detail.memory // "") == $mem and ((.status.detail.cores // "" | tostring) == $cores)) as $gpu_ok |
    (((.status.detail.cpu // {}) | length) > 0) as $cpu_ok |
    "\($id_ok)|\($gpu_ok)|\($cpu_ok)"
  ' 2>/dev/null)"
  IFS='|' read -r id_ok gpu_ok cpu_ok <<<"$it_info"
  if [ "$id_ok" = "true" ]; then
    record PASS "aware type splits by CPU" "${AWARE_IT}: ${AKEY_NV} + ${GKEY}"
  else
    record FAIL "aware type splits by CPU" "${AWARE_IT} mismatch"
  fi
  if [ "$gpu_ok" = "true" ]; then
    record PASS "aware type carries GPU descriptors" "product=${PRODUCT} memory=${MEMORY} cores=${CORES}"
  else
    record FAIL "aware type carries GPU descriptors" "status.detail mismatch"
  fi
  if [ "$cpu_ok" = "true" ]; then
    record PASS "aware type folds status.detail.cpu" "status.detail.cpu present"
  else
    record FAIL "aware type folds status.detail.cpu" "status.detail.cpu empty"
  fi

  # Deploy 2-card CPU Instance on AWARE_IT -> verifies distinct mock UUIDs + Devices.status reservation.
  INST_AWARE=e2e-c3-aware-two
  TEST_INSTANCES+=("$INST_AWARE")
  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata: { name: ${INST_AWARE}, namespace: default }
spec:
  type: ${AWARE_IT}
  image: ${MOCK_IMAGE}
  command: ["sleep", "86400"]
  volume: { ephemeral: { capacity: 1Gi } }
  resources:
    accelerator: "2"
    localStorage: 1Gi
EOF

  inst_ph=""
  for _ in $(seq 1 50); do
    inst_ph="$(kubectl -n default get instance "$INST_AWARE" -o jsonpath='{.status.phase}' 2>/dev/null)"
    { [ "$inst_ph" = "Ready" ] || [ "$inst_ph" = "Running" ]; } && break
    sleep 3
  done
  if [ "$inst_ph" = "Ready" ] || [ "$inst_ph" = "Running" ]; then
    vis_csv="$(exec_env "$INST_AWARE" NVIDIA_VISIBLE_DEVICES)"
    mock_csv="$(exec_env "$INST_AWARE" GPUSTACK_MOCK_ALLOCATED_DEVICES)"
    ann_ids="$(kubectl -n default get pod "$INST_AWARE" -o json 2>/dev/null | jq -r '
      [.metadata.annotations["device.gpustack.ai/accelerator.allocated"] | fromjson
       | .main.devices.groups[].accelerators[].id] | sort | join(",")
    ' 2>/dev/null || true)"
    vis_sorted="$(printf '%s' "$vis_csv" | tr ',' '\n' | sort -u | paste -sd, -)"
    n_uniq="$(printf '%s' "$vis_csv" | tr ',' '\n' | grep -c . || true)"
    if [ "$n_uniq" -eq 2 ] && [ "$vis_csv" = "$mock_csv" ] && [ "$vis_sorted" = "$ann_ids" ]; then
      record PASS "2-card aware Instance receives 2 distinct Mock UUIDs" "UUIDs=${vis_sorted}"
    else
      record FAIL "2-card aware Instance receives 2 distinct Mock UUIDs" "vis='${vis_csv}' mock='${mock_csv}' ann='${ann_ids}'"
    fi
  else
    record FAIL "2-card aware Instance receives 2 distinct Mock UUIDs" "${INST_AWARE} phase='${inst_ph:-<none>}'"
  fi
fi

echo
echo "== CASE 3 — CPU Mock Detector + Allocator full scheduling & admission chain =="
print_rows
if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s)."
  exit 1
fi
echo "CASE 3 PASS"
