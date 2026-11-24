#!/usr/bin/env bash
#
# CHAOS 4 — Control-loop churn & out-of-band disruption: mid-flight device-manager & worker Pod
#           restarts, out-of-band Devices/NodeFeature/LocalQueue/Service deletions, managed node
#           toggle, and hot card shrink + expand
#           (MUTATING, self-recovering; AUTO-SKIPS when Mock Device Manager is not active)
#
#   chaos-4.sh [NS]
#
# Prerequisites: nodes=1; gpu=any; cluster=any; inputs=CHAOS_TIMEOUT,E2E_MOCK_IMAGE
# Goal:        Verify reconcile-loop convergence and resource self-healing under mid-flight
#              control-plane and node-agent disruptions while mock workloads (including port-bearing
#              Instance and virtual MIG partitions when nvidia is served) are active:
#                1. Out-of-band deletion of watched worker objects: deleting an Instance Service,
#                   an entrance LocalQueue in the test namespace, and <node>-gpustack-worker
#                   NodeFeature triggers immediate watch-driven self-healing with fresh UIDs;
#                2. Out-of-band deletion of Devices/<node> + <node>-gpustack-device-manager
#                   NodeFeature paired with mid-flight mock device-manager Pod deletion: DaemonSet
#                   replaces the Pod, Detector recreates Devices (with device.gpustack.ai/mock=true)
#                   and NodeFeature, and NodeDevicesReconciler restores Devices.status;
#                3. Mid-flight worker controller Pod deletion: Deployment replaces the worker Pod,
#                   reconcilers re-elect leader, and pool InstanceTypes stay Active;
#                4. Node gpustack.ai/managed=false -> true toggle on a mock worker node: drains/stops
#                   managed capacity on false and restores Active schedulable capacity on true;
#                5. Hot card shrink + expand via ConfigMap hot-reload (4 -> 2 -> 4) plus clean
#                   workload teardown: zero stuck finalizers and zero leaked virtual partitions in
#                   Devices.status.
# Environment: Any Kind/k3s cluster running GPUStack Operator with Mock Device Manager enabled
#              (deviceManager.mock.enabled=true). AUTO-SKIPS cleanly with SKIP rows and exit 0
#              when Mock Device Manager is not active.
# Inputs:      MOCKED hardware via ConfigMap hot-reload; REAL Pod kills, out-of-band CR/Service
#              deletions, worker NodeFeature managed toggles, and workloads in an isolated
#              chaos-c4-<ts> namespace.
# Expected:    All six self-healing, restart convergence, managed-toggle, shrink/expand, and clean
#              teardown checks record PASS.
# Cleanup:     EXIT trap restores gpustack.ai/managed=true on all touched worker NodeFeatures,
#              deletes workloads and the isolated chaos-c4-<ts> namespace, and restores the original
#              mock ConfigMap via mock_dm_restore_cm.
set -uo pipefail

CHAOS_ID=4
CHAOS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
E2E_CASES_DIR="$(cd "${CHAOS_DIR}/../../gpustack-operator-e2e/cases" && pwd)"

# shellcheck source=/dev/null
. "${E2E_CASES_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${E2E_CASES_DIR}/_pool-lib.sh"
# shellcheck source=/dev/null
. "${E2E_CASES_DIR}/_mock-dm-lib.sh"

NS="${1:-gpustack-system}"
# shellcheck disable=SC2034 # Read by _rows-lib.sh setting_* helpers.
SYSTEM_NS="$NS"

MOCK_IMAGE="${E2E_MOCK_IMAGE:-alpine}"
CHAOS_TIMEOUT="${CHAOS_TIMEOUT:-150}"
TEST_NS="chaos-c${CHAOS_ID}-$(date +%s)-$$"
INST_SVC_NAME="chaos-c4-inst"

ORIG_CM_FILE="$(mktemp)"
CM_SNAPSHOT_OK=0
NS_CREATED=0
TOUCHED_WORKER_NFS=()

is_pos_int() {
  [[ "${1:-}" =~ ^[1-9][0-9]*$ ]]
}
is_pos_int "$CHAOS_TIMEOUT" || CHAOS_TIMEOUT=150

# shellcheck disable=SC2317 # Invoked via EXIT trap.
cleanup() {
  echo
  echo "[chaos-4] cleanup: restoring managed=true, draining ${TEST_NS}, and restoring mock ConfigMap"
  local nf
  for nf in ${TOUCHED_WORKER_NFS[@]+"${TOUCHED_WORKER_NFS[@]}"}; do
    kubectl -n "$NS" patch nodefeature "$nf" --type=merge \
      -p '{"spec":{"labels":{"gpustack.ai/managed":"true"}}}' >/dev/null 2>&1 || true
  done
  if [[ "$NS_CREATED" -eq 1 ]]; then
    kubectl -n "$TEST_NS" delete instance "$INST_SVC_NAME" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    kubectl -n "$TEST_NS" delete pods --all --ignore-not-found --force --grace-period=0 >/dev/null 2>&1 || true
    kubectl -n "$TEST_NS" delete workloads.kueue.x-k8s.io --all --ignore-not-found --wait=false >/dev/null 2>&1 || true
    kubectl delete namespace "$TEST_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  fi
  if [[ "$CM_SNAPSHOT_OK" -eq 1 ]]; then
    mock_dm_restore_cm "$NS" "$MOCK_DM_CM" "$ORIG_CM_FILE" || true
  fi
  rm -f "$ORIG_CM_FILE"
}
trap cleanup EXIT

if ! mock_dm_detect_env "$NS"; then
  skip_msg="mock device-manager DaemonSet (gpustack.ai/manufacturer=mock) is not active on this cluster"
  record SKIP "out-of-band Service, LocalQueue & worker NodeFeature watch self-heal" "$skip_msg"
  record SKIP "Devices & device-manager NodeFeature self-heal across mock DM Pod kill" "$skip_msg"
  record SKIP "worker controller Pod mid-flight deletion & convergence" "$skip_msg"
  record SKIP "gpustack.ai/managed=false -> true node drain and re-admission" "$skip_msg"
  record SKIP "hot card shrink (4 -> 2) and expand (2 -> 4) via ConfigMap hot-reload" "$skip_msg"
  record SKIP "clean teardown with zero stuck finalizers and zero leaked virtual partitions" "$skip_msg"
  echo
  echo "== CHAOS 4 — SKIPPED (${skip_msg}) =="
  print_rows
  exit 0
fi

PRESET="nvidia-h100"
MFR="nvidia"
EXCL_RES="nvidia.com/gpu"
HAS_NV_MIG=1
if ! mock_dm_manufacturer_allowed nvidia; then
  HAS_NV_MIG=0
  if mock_dm_manufacturer_allowed amd; then
    PRESET="amd-mi308x"; MFR="amd"; EXCL_RES="amd.com/gpu"
  elif mock_dm_manufacturer_allowed ascend; then
    PRESET="ascend-910b2"; MFR="ascend"; EXCL_RES="huawei.com/npu"
  else
    skip_msg="active mock DaemonSet restricts manufacturers (${MOCK_DM_MANUFACTURERS}) outside nvidia/amd/ascend"
    record SKIP "control-loop chaos suite" "$skip_msg"
    print_rows
    exit 0
  fi
fi

if mock_dm_snapshot_cm "$NS" "$MOCK_DM_CM" "$ORIG_CM_FILE"; then
  CM_SNAPSHOT_OK=1
else
  echo "[chaos-4] FATAL: failed to snapshot ConfigMap ${NS}/${MOCK_DM_CM}" >&2
  exit 1
fi

NODE_A="${MOCK_DM_NODES[0]}"
NODE_B="$NODE_A"
[[ "${#MOCK_DM_NODES[@]}" -ge 2 ]] && NODE_B="${MOCK_DM_NODES[1]}"
echo "[chaos-4] nodeA=${NODE_A} nodeB=${NODE_B} preset=${PRESET} (${MFR}) ns=${TEST_NS}"

kubectl create namespace "$TEST_NS" >/dev/null
NS_CREATED=1

# ===================================================================================================
# Setup — Configure NODE_A (mixed logical + virtual MIG when nvidia) and NODE_B (4 cards) + workloads
# ===================================================================================================
render_c4_spec() {
  local b_count="$1"
  printf 'default:\n  preset: %s\n  count: 4\nnodes:\n' "$PRESET"
  if [[ "$NODE_B" != "$NODE_A" ]]; then
    if [[ "$HAS_NV_MIG" -eq 1 ]]; then
      cat <<EOF
  "${NODE_A}":
    preset: nvidia-h100
    count: 4
    cardOverrides:
      3:
        id: GPU-c4-mig-912151ec-e9f1-fc26
        physicalSliced:
          count: 7
          profiles:
            - name: 1g.10gb
              memoryMib: 9856
              computeSlices: 1
              memorySlices: 1
              count: 7
              placements:
                - {start: 0, length: 1}
                - {start: 1, length: 1}
                - {start: 2, length: 1}
                - {start: 3, length: 1}
                - {start: 4, length: 1}
                - {start: 5, length: 1}
                - {start: 6, length: 1}
            - name: 3g.40gb
              memoryMib: 40192
              computeSlices: 3
              memorySlices: 4
              count: 2
              placements:
                - {start: 0, length: 4}
                - {start: 4, length: 4}
  "${NODE_B}":
    preset: ${PRESET}
    count: ${b_count}
EOF
    else
      cat <<EOF
  "${NODE_A}":
    preset: ${PRESET}
    count: 4
  "${NODE_B}":
    preset: ${PRESET}
    count: ${b_count}
EOF
    fi
  else
    # Single-node cluster: NODE_A == NODE_B; keep virtual MIG on card 0 when b_count >= 2.
    if [[ "$HAS_NV_MIG" -eq 1 && "$b_count" -ge 2 ]]; then
      cat <<EOF
  "${NODE_A}":
    preset: nvidia-h100
    count: ${b_count}
    cardOverrides:
      0:
        id: GPU-c4-mig-912151ec-e9f1-fc26
        physicalSliced:
          count: 7
          profiles:
            - name: 1g.10gb
              memoryMib: 9856
              computeSlices: 1
              memorySlices: 1
              count: 7
              placements:
                - {start: 0, length: 1}
                - {start: 1, length: 1}
                - {start: 2, length: 1}
                - {start: 3, length: 1}
                - {start: 4, length: 1}
                - {start: 5, length: 1}
                - {start: 6, length: 1}
            - name: 3g.40gb
              memoryMib: 40192
              computeSlices: 3
              memorySlices: 4
              count: 2
              placements:
                - {start: 0, length: 4}
                - {start: 4, length: 4}
EOF
    else
      cat <<EOF
  "${NODE_A}":
    preset: ${PRESET}
    count: ${b_count}
EOF
    fi
  fi
}

mock_dm_apply_spec "$NS" "$MOCK_DM_CM" "$MOCK_DM_KEY" "$(render_c4_spec 4)"

mock_dm_wait_devices "$NS" "$NODE_A" \
  "(.metadata.annotations[\"device.gpustack.ai/mock\"] == \"true\") and (([.spec.groups[] | select(.manufacturer == \"${MFR}\") | .accelerators[]] | length) == 4)" "$CHAOS_TIMEOUT"
if [[ "$NODE_B" != "$NODE_A" ]]; then
  mock_dm_wait_devices "$NS" "$NODE_B" \
    "(.metadata.annotations[\"device.gpustack.ai/mock\"] == \"true\") and (([.spec.groups[] | select(.manufacturer == \"${MFR}\") | .accelerators[]] | length) == 4)" "$CHAOS_TIMEOUT"
fi

GID_A="$(kubectl get devices.worker.gpustack.ai "$NODE_A" -o json 2>/dev/null \
  | jq -r --arg m "$MFR" '[.spec.groups[] | select(.manufacturer == $m) | .id][0] // ""')"

IT_A=""
LQ_A=""
for _ in $(seq 1 30); do
  read -r IT_A LQ_A <<<"$(discover_node_pool "$NODE_A" "$GID_A" --require-entrance --fallback-any name entrance 2>/dev/null || true)"
  if [[ -n "$IT_A" && -n "$LQ_A" ]] \
    && kubectl -n "$TEST_NS" get localqueue.kueue.x-k8s.io "$LQ_A" >/dev/null 2>&1; then
    break
  fi
  sleep 3
done

# Launch port-bearing Instance (so InstanceReconciler owns a Service) + active sliced/partitioned Pods on NODE_A.
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata:
  name: ${INST_SVC_NAME}
  namespace: ${TEST_NS}
spec:
  type: ${IT_A}
  nodeName: ${NODE_A}
  image: ${MOCK_IMAGE}
  command: ["sleep", "86400"]
  ports:
    - name: http
      port: 8080
      protocol: TCP
  resources:
    accelerator: "1"
    localStorage: 1Gi
  volume:
    ephemeral:
      capacity: 1Gi
EOF

PART_POD_1="chaos-c4-mig-3g"
PART_POD_2="chaos-c4-mig-1g"
if [[ "$HAS_NV_MIG" -eq 1 ]]; then
  for pair in "${PART_POD_1}:mig-3g.40gb" "${PART_POD_2}:mig-1g.10gb"; do
    pname="${pair%%:*}"
    pprof="${pair#*:}"
    jq -cn \
      --arg name "$pname" \
      --arg ns "$TEST_NS" \
      --arg node "$NODE_A" \
      --arg lq "$LQ_A" \
      --arg img "$MOCK_IMAGE" \
      --arg pk "nvidia.com/gpu.partitioned.${pprof}" '
      {
        apiVersion: "v1",
        kind: "Pod",
        metadata: {name: $name, namespace: $ns, labels: {"kueue.x-k8s.io/queue-name": $lq}},
        spec: {
          schedulerName: "default-scheduler",
          restartPolicy: "Never",
          nodeSelector: {"kubernetes.io/hostname": $node},
          containers: [{
            name: "main",
            image: $img,
            command: ["sleep", "86400"],
            resources: {
              limits: {"nvidia.com/gpu.partitioned": "1", ($pk): "1"},
              requests: {"nvidia.com/gpu.partitioned": "1", ($pk): "1"}
            }
          }]
        }
      }' | kubectl apply -f - >/dev/null
  done
fi

wait_obj_uid_changed() {
  local ns_flag="$1" kind="$2" name="$3" old_uid="$4" timeout_s="${5:-90}" elapsed=0 new_uid=""
  while [[ "$elapsed" -lt "$timeout_s" ]]; do
    if [[ -n "$ns_flag" ]]; then
      new_uid="$(kubectl -n "$ns_flag" get "$kind" "$name" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
    else
      new_uid="$(kubectl get "$kind" "$name" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
    fi
    if [[ -n "$new_uid" && "$new_uid" != "$old_uid" ]]; then
      printf '%s' "$new_uid"
      return 0
    fi
    sleep 3
    elapsed=$((elapsed + 3))
  done
  return 1
}

# Wait for Instance Pod + Service (and MIG Pods when enabled) to come up before disruption.
for _ in $(seq 1 40); do
  iph="$(kubectl -n "$TEST_NS" get instance "$INST_SVC_NAME" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  svc_u="$(kubectl -n "$TEST_NS" get svc "$INST_SVC_NAME" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
  [[ "$iph" == "Ready" || "$iph" == "Running" ]] && [[ -n "$svc_u" ]] && break
  sleep 3
done
if [[ "$HAS_NV_MIG" -eq 1 ]]; then
  for pname in "$PART_POD_1" "$PART_POD_2"; do
    for _ in $(seq 1 30); do
      [[ "$(kubectl -n "$TEST_NS" get pod "$pname" -o jsonpath='{.status.phase}' 2>/dev/null)" == "Running" ]] && break
      sleep 3
    done
  done
fi

# ===================================================================================================
# Step 1 — Out-of-band deletion of watched worker objects: Service, LocalQueue, worker NodeFeature
# ===================================================================================================
WORKER_NF_A="${NODE_A}-gpustack-worker"
old_svc_uid="$(kubectl -n "$TEST_NS" get svc "$INST_SVC_NAME" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
old_lq_uid="$(kubectl -n "$TEST_NS" get localqueue.kueue.x-k8s.io "$LQ_A" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
old_wnf_uid="$(kubectl -n "$NS" get nodefeature "$WORKER_NF_A" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"

kubectl -n "$TEST_NS" delete svc "$INST_SVC_NAME" --ignore-not-found >/dev/null 2>&1 || true
kubectl -n "$TEST_NS" delete localqueue.kueue.x-k8s.io "$LQ_A" --ignore-not-found >/dev/null 2>&1 || true
kubectl -n "$NS" delete nodefeature "$WORKER_NF_A" --ignore-not-found >/dev/null 2>&1 || true

new_svc_uid="$(wait_obj_uid_changed "$TEST_NS" svc "$INST_SVC_NAME" "$old_svc_uid" 90 || true)"
new_lq_uid="$(wait_obj_uid_changed "$TEST_NS" localqueue.kueue.x-k8s.io "$LQ_A" "$old_lq_uid" 90 || true)"
new_wnf_uid="$(wait_obj_uid_changed "$NS" nodefeature "$WORKER_NF_A" "$old_wnf_uid" 90 || true)"

if [[ -n "$new_svc_uid" && -n "$new_lq_uid" && -n "$new_wnf_uid" ]]; then
  record PASS "out-of-band Service, LocalQueue & worker NodeFeature watch self-heal" \
    "recreated svc/${INST_SVC_NAME}, lq/${LQ_A}, and nf/${WORKER_NF_A} with new UIDs"
else
  record FAIL "out-of-band Service, LocalQueue & worker NodeFeature watch self-heal" \
    "svc='${new_svc_uid:-<missing>}' lq='${new_lq_uid:-<missing>}' wnf='${new_wnf_uid:-<missing>}'"
fi

# ===================================================================================================
# Step 2 — Out-of-band Devices & device-manager NodeFeature delete + mock device-manager Pod kill
# ===================================================================================================
DM_NF_A="${NODE_A}-gpustack-device-manager"
old_dev_uid="$(kubectl get devices.worker.gpustack.ai "$NODE_A" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
old_dmnf_uid="$(kubectl -n "$NS" get nodefeature "$DM_NF_A" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
old_dm_pod="$(kubectl -n "$NS" get pods \
  -l app.kubernetes.io/component=device-manager,gpustack.ai/manufacturer=mock \
  --field-selector "spec.nodeName=${NODE_A}" \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"

kubectl delete devices.worker.gpustack.ai "$NODE_A" --ignore-not-found >/dev/null 2>&1 || true
kubectl -n "$NS" delete nodefeature "$DM_NF_A" --ignore-not-found >/dev/null 2>&1 || true
if [[ -n "$old_dm_pod" ]]; then
  kubectl -n "$NS" delete pod "$old_dm_pod" --ignore-not-found --grace-period=0 --force >/dev/null 2>&1 || true
fi

new_dev_uid="$(wait_obj_uid_changed "" devices.worker.gpustack.ai "$NODE_A" "$old_dev_uid" "$CHAOS_TIMEOUT" || true)"
new_dmnf_uid="$(wait_obj_uid_changed "$NS" nodefeature "$DM_NF_A" "$old_dmnf_uid" "$CHAOS_TIMEOUT" || true)"

dev_mock_repopulated=0
if mock_dm_wait_devices "$NS" "$NODE_A" \
  '(.metadata.annotations["device.gpustack.ai/mock"] == "true")
   and ((.status.groups // []) | length > 0)
   and any(.status.groups[].accelerators[]; (.allocated // 0) > 0 or ((.allocatedProfiles // []) | length > 0))' 90; then
  dev_mock_repopulated=1
fi

if [[ -n "$new_dev_uid" && -n "$new_dmnf_uid" && "$dev_mock_repopulated" -eq 1 ]]; then
  record PASS "Devices & device-manager NodeFeature self-heal across mock DM Pod kill" \
    "Devices/${NODE_A} (mock=true + live status) & nf/${DM_NF_A} recreated after killing ${old_dm_pod:-dm-pod}"
else
  record FAIL "Devices & device-manager NodeFeature self-heal across mock DM Pod kill" \
    "devUid='${new_dev_uid:-<missing>}' dmNfUid='${new_dmnf_uid:-<missing>}' statusRepopulated=${dev_mock_repopulated}"
fi

# ===================================================================================================
# Step 3 — Mid-flight worker controller Pod deletion & control-plane convergence
# ===================================================================================================
WORKER_PODS="$(kubectl -n "$NS" get pods -l app.kubernetes.io/component=worker \
  -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)"
if [[ -z "$WORKER_PODS" ]]; then
  WORKER_PODS="$(kubectl -n "$NS" get pods -o jsonpath='{.items[*].metadata.name}' 2>/dev/null \
    | tr ' ' '\n' | grep -E 'gpustack.*worker-' | grep -v 'gateway' | paste -sd' ' - || true)"
fi

worker_rec_ok=0
if [[ -n "$WORKER_PODS" ]]; then
  for wp in $WORKER_PODS; do
    kubectl -n "$NS" delete pod "$wp" --ignore-not-found >/dev/null 2>&1 || true
  done
  if kubectl -n "$NS" rollout status deploy/gpustack-operator-worker --timeout=150s >/dev/null 2>&1 \
    && mock_dm_wait_it ".items[] | select(.metadata.name == \"${IT_A}\" and .status.phase == \"Active\")" 60; then
    worker_rec_ok=1
  fi
fi

if [[ "$worker_rec_ok" -eq 1 ]]; then
  record PASS "worker controller Pod mid-flight deletion & convergence" \
    "worker Deployment rolled back Ready and ${IT_A} stayed Active"
else
  record FAIL "worker controller Pod mid-flight deletion & convergence" \
    "workerPods='${WORKER_PODS:-<none>}' recovered=${worker_rec_ok}"
fi

# ===================================================================================================
# Step 4 — Toggle gpustack.ai/managed=false -> true on NODE_B
# ===================================================================================================
WORKER_NF_B="${NODE_B}-gpustack-worker"
TOUCHED_WORKER_NFS+=("$WORKER_NF_B")

kubectl -n "$NS" patch nodefeature "$WORKER_NF_B" --type=merge \
  -p '{"spec":{"labels":{"gpustack.ai/managed":"false"}}}' >/dev/null

# Wait for Devices/<NODE_B> label gpustack.ai/managed to leave "true" (or Node label to flip to "false").
managed_off=0
for _ in $(seq 1 45); do
  nd_m="$(kubectl get node "$NODE_B" -o jsonpath='{.metadata.labels.gpustack\.ai/managed}' 2>/dev/null || true)"
  dv_m="$(kubectl get devices.worker.gpustack.ai "$NODE_B" -o jsonpath='{.metadata.labels.gpustack\.ai/managed}' 2>/dev/null || true)"
  if [[ "$nd_m" == "false" && "$dv_m" != "true" ]]; then
    managed_off=1
    break
  fi
  sleep 3
done

kubectl -n "$NS" patch nodefeature "$WORKER_NF_B" --type=merge \
  -p '{"spec":{"labels":{"gpustack.ai/managed":"true"}}}' >/dev/null

managed_on=0
for _ in $(seq 1 45); do
  nd_m="$(kubectl get node "$NODE_B" -o jsonpath='{.metadata.labels.gpustack\.ai/managed}' 2>/dev/null || true)"
  dv_m="$(kubectl get devices.worker.gpustack.ai "$NODE_B" -o jsonpath='{.metadata.labels.gpustack\.ai/managed}' 2>/dev/null || true)"
  if [[ "$nd_m" == "true" && "$dv_m" == "true" ]] \
    && mock_dm_wait_it ".items[] | select(.metadata.name == \"${IT_A}\" and .status.phase == \"Active\" and ((.status.accelerator.capacity // 0) > 0 or (.status.acceleratorPartitioned.capacity // 0) > 0))" 6; then
    managed_on=1
    break
  fi
  sleep 3
done

if [[ "$managed_off" -eq 1 && "$managed_on" -eq 1 ]]; then
  record PASS "gpustack.ai/managed=false -> true node drain and re-admission" \
    "${NODE_B} transitioned managed=false -> managed=true and restored Active pool capacity"
else
  record FAIL "gpustack.ai/managed=false -> true node drain and re-admission" \
    "node=${NODE_B} managedOff=${managed_off} managedOn=${managed_on}"
fi

# ===================================================================================================
# Step 5 — Hot card shrink (4 -> 2) and expand (2 -> 4) on NODE_B via ConfigMap hot-reload
# ===================================================================================================
mock_dm_apply_spec "$NS" "$MOCK_DM_CM" "$MOCK_DM_KEY" "$(render_c4_spec 2)"
shrink_ok=0
want_shrink_excl=2
[[ "$NODE_B" == "$NODE_A" && "$HAS_NV_MIG" -eq 1 ]] && want_shrink_excl=1
if mock_dm_wait_devices "$NS" "$NODE_B" \
  "(([.spec.groups[] | select(.manufacturer == \"${MFR}\") | .accelerators[]] | length) == 2)" "$CHAOS_TIMEOUT" \
  && mock_dm_wait_node_allocatable "$NODE_B" ".[\"${EXCL_RES}\"] == \"${want_shrink_excl}\"" "$CHAOS_TIMEOUT"; then
  shrink_ok=1
fi

mock_dm_apply_spec "$NS" "$MOCK_DM_CM" "$MOCK_DM_KEY" "$(render_c4_spec 4)"
expand_ok=0
want_expand_excl=4
[[ "$NODE_B" == "$NODE_A" && "$HAS_NV_MIG" -eq 1 ]] && want_expand_excl=3
if mock_dm_wait_devices "$NS" "$NODE_B" \
  "(([.spec.groups[] | select(.manufacturer == \"${MFR}\") | .accelerators[]] | length) == 4)" "$CHAOS_TIMEOUT" \
  && mock_dm_wait_node_allocatable "$NODE_B" ".[\"${EXCL_RES}\"] == \"${want_expand_excl}\"" "$CHAOS_TIMEOUT"; then
  expand_ok=1
fi

if [[ "$shrink_ok" -eq 1 && "$expand_ok" -eq 1 ]]; then
  record PASS "hot card shrink (4 -> 2) and expand (2 -> 4) via ConfigMap hot-reload" \
    "${NODE_B} Devices converged 4 -> 2 -> 4 without restarting device-manager"
else
  record FAIL "hot card shrink (4 -> 2) and expand (2 -> 4) via ConfigMap hot-reload" \
    "node=${NODE_B} shrink=${shrink_ok} expand=${expand_ok}"
fi

# ===================================================================================================
# Step 6 — Tear down all active workloads and verify zero stuck finalizers & zero leaked partitions
# ===================================================================================================
kubectl -n "$TEST_NS" delete instance "$INST_SVC_NAME" --ignore-not-found --timeout=90s >/dev/null 2>&1 || true
kubectl -n "$TEST_NS" delete pods --all --ignore-not-found --timeout=60s >/dev/null 2>&1 || true
kubectl -n "$TEST_NS" delete workloads.kueue.x-k8s.io --all --ignore-not-found --timeout=60s >/dev/null 2>&1 || true

clean_teardown=0
for _ in $(seq 1 35); do
  rem_inst="$(kubectl -n "$TEST_NS" get instances.worker.gpustack.ai --no-headers 2>/dev/null | wc -l | tr -d ' ')"
  rem_pods="$(kubectl -n "$TEST_NS" get pods --no-headers 2>/dev/null | wc -l | tr -d ' ')"
  rem_wls="$(kubectl -n "$TEST_NS" get workloads.kueue.x-k8s.io --no-headers 2>/dev/null | wc -l | tr -d ' ')"
  leaked_parts="$(kubectl get devices.worker.gpustack.ai "$NODE_A" -o json 2>/dev/null | jq -r '
    [.status.groups[]? | .accelerators[]?
     | select((.allocated // 0) > 0 or any((.allocatedProfiles // [])[]; (.count // 0) > 0))]
    | length
  ' 2>/dev/null || echo 1)"
  if [[ "${rem_inst:-1}" -eq 0 && "${rem_pods:-1}" -eq 0 && "${rem_wls:-1}" -eq 0 && "${leaked_parts:-1}" -eq 0 ]]; then
    clean_teardown=1
    break
  fi
  sleep 3
done

if [[ "$clean_teardown" -eq 1 ]]; then
  record PASS "clean teardown with zero stuck finalizers and zero leaked virtual partitions" \
    "0 leftover Instances/Pods/Workloads in ${TEST_NS}; 0 allocated partitions on ${NODE_A}"
else
  record FAIL "clean teardown with zero stuck finalizers and zero leaked virtual partitions" \
    "inst=${rem_inst:-?} pods=${rem_pods:-?} wls=${rem_wls:-?} leakedAlloc=${leaked_parts:-?}"
fi

echo
echo "== CHAOS 4 — Control-loop churn & out-of-band disruption =="
print_rows
if [[ "$FAILS" -ne 0 ]]; then
  echo "[chaos-4] ${FAILS} check(s) FAILED"
  exit 1
fi
echo "CHAOS 4 PASS"
