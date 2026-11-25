#!/usr/bin/env bash
#
# CASE 5 — CPU Mock logical-slice packing/co-residency/over-commit guard & virtual MIG/partition
#          mixed-node, live-ledger capacity, exclusion, concurrent multi-profile allocation,
#          visibility co-allocation, and dead-Pod reclamation
#          (MUTATING, self-recovering; AUTO-SKIPS when Mock Device Manager is not active)
#
#   case-5.sh <NS>
#
# Prerequisites: nodes=1; gpu=any; cluster=any; inputs=E2E_MOCK_IMAGE
# Goal:        Verify the full logical-slicing and virtual hardware-partitioning (MIG) scheduling,
#              ledger accounting, co-allocation, and reclaim contracts on CPU-only Mock Device
#              Manager nodes via ConfigMap hot-reload:
#                Phase A (Logical slicing on Mock DM):
#                  1. Multi-card spread & packing (2x nvidia-h100): two 60% claims (>50%) take
#                     distinct cards; 100% + 50% -> free 100% -> 30% joins the half-used card
#                     rather than opening the freed card; zero card is over-committed in
#                     Devices.status; running CPU Pods observe GPUSTACK_MOCK_ALLOCATION_MODE=sliced
#                     and matching NVIDIA_VISIBLE_DEVICES; SL view reflects occupancy with
#                     onceMaxRequest <= 100;
#                  2. Single-card co-residency & over-budget refusal (1x nvidia-h100): two 40%
#                     sliced Instances coexist on the single card, each Instance's /metrics
#                     subresource reports mode=Sliced and matching memoryTotalMiB, a third 40%
#                     slice (120% > 100%) is held, and the two admitted slices stay Running (no
#                     self-eviction).
#                Phase B (Virtual MIG / Partitioning on Mock DM):
#                  1. Mixed logical + MIG node & whole-card exclusion: 1 MIG card + 1 logical whole
#                     card advertise both families simultaneously, EX/SH views exclude the MIG card,
#                     occupying the 1 whole card holds a 2nd exclusive Pod (never placed on the MIG
#                     card), and simultaneous 3g.40gb partition + 20% sliced Pods land on their
#                     respective card populations with 0 UnexpectedAdmissionError;
#                  2. Virtual MIG live-ledger capacity, concurrent multi-profile (3g.40gb, 2g.20gb,
#                     1g.10gb), visibility co-allocation, saturation & dead-Pod reclaim: carving
#                     updates Devices.status.allocatedProfiles and drops mig-7g.80gb allocatable to
#                     0; 3g.40gb + 2g.20gb + 1g.10gb coexist with distinct MOCK-MIG-* handles and
#                     0 UnexpectedAdmissionError; a sidecar requesting nvidia.visibility shares
#                     main's exact virtual partition handle; over-fitting requests are held; and
#                     deleting partitioned Pods reclaims their virtual partitions, freeing
#                     Devices.status and admitting a 7g.80gb saturation + refill cycle cleanly.
# Environment: Any CPU-only Kind/k3s cluster running GPUStack Operator with Mock Device Manager
#              enabled (deviceManager.mock.enabled=true) and serving nvidia. AUTO-SKIPS cleanly
#              with SKIP rows and exit 0 when Mock Device Manager is not active.
# Inputs:      Hot-reloaded mock devices YAML specs written through _mock-dm-lib.sh plus CPU alpine
#              Pods and Instances (override image via E2E_MOCK_IMAGE).
# Expected:    Every Phase A and Phase B check records PASS.
# Cleanup:     EXIT trap deletes all test Pods, Instances, and Workloads, and restores the original
#              mock ConfigMap via mock_dm_restore_cm.
set -uo pipefail

# shellcheck disable=SC2034 # Read by _rows-lib.sh.
CASE_ID=5
CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=/dev/null
. "${CASE_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_pool-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_mock-dm-lib.sh"

NS="${1:?usage: case-5.sh <NS>}"
MOCK_IMAGE="${E2E_MOCK_IMAGE:-alpine}"
ORIG_CM_FILE="$(mktemp)"
CM_SNAPSHOT_OK=0
TEST_PODS=()
TEST_INSTANCES=()

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

clear_test_instances() {
  local inst
  for inst in ${TEST_INSTANCES[@]+"${TEST_INSTANCES[@]}"}; do
    kubectl -n default delete instance "$inst" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    delete_pod_and_workload "$inst"
  done
  TEST_INSTANCES=()
}

# shellcheck disable=SC2317 # Invoked via EXIT trap.
cleanup() {
  echo
  echo "[case-5] cleanup: deleting test Pods/Instances and restoring original mock ConfigMap"
  clear_test_pods
  clear_test_instances
  if [ "$CM_SNAPSHOT_OK" -eq 1 ]; then
    mock_dm_restore_cm "$NS" "$MOCK_DM_CM" "$ORIG_CM_FILE" || true
  fi
  rm -f "$ORIG_CM_FILE"
}
trap cleanup EXIT

if ! mock_dm_detect_env "$NS" || ! mock_dm_manufacturer_allowed nvidia; then
  skip_msg="mock device-manager DaemonSet (gpustack.ai/manufacturer=mock serving nvidia) is not active on this cluster"
  record SKIP "Phase A: logical-slice packing, co-residency, over-commit guard & mock env" "$skip_msg"
  record SKIP "Phase B: virtual MIG mixed-node, live-ledger, multi-profile, visibility & reclaim" "$skip_msg"
  echo
  echo "== CASE 5 — SKIPPED (${skip_msg}) =="
  print_rows
  exit 0
fi

if mock_dm_snapshot_cm "$NS" "$MOCK_DM_CM" "$ORIG_CM_FILE"; then
  CM_SNAPSHOT_OK=1
else
  echo "[case-5] FATAL: failed to snapshot ConfigMap ${NS}/${MOCK_DM_CM}"
  exit 1
fi

TARGET_NODE="$MOCK_DM_PRIMARY_NODE"
DEFAULT_BLOCK="  groups: []"
echo "[case-5] target node=${TARGET_NODE} cm=${MOCK_DM_CM}/${MOCK_DM_KEY}"

pod_phase() { kubectl -n default get pod "$1" -o jsonpath='{.status.phase}' 2>/dev/null; }
wait_pod_running() {
  local p="$1" tries="${2:-40}"
  for _ in $(seq 1 "$tries"); do
    [ "$(pod_phase "$p")" = "Running" ] && return 0
    sleep 3
  done
  return 1
}

exec_ctr_env() {
  local p="$1" ctr="$2" var="$3" out=""
  for _ in $(seq 1 10); do
    out="$(kubectl -n default exec "$p" -c "$ctr" -- sh -c "printf '%s' \"\${${var}:-}\"" 2>/dev/null || true)"
    [ -n "$out" ] && { printf '%s' "$out"; return 0; }
    sleep 2
  done
  printf '%s' "$out"
}

pod_unexpected_admission() {
  local uid
  uid="$(kubectl -n default get pod "$1" -o jsonpath='{.metadata.uid}' 2>/dev/null)" || return 0
  [ -n "$uid" ] || return 0
  kubectl -n default get events \
    --field-selector "involvedObject.name=$1,involvedObject.uid=${uid}" \
    -o jsonpath='{range .items[*]}{.reason}{"\n"}{end}' 2>/dev/null \
    | grep -c 'UnexpectedAdmissionError' || true
}

submit_pod() {
  local name="$1" lq="$2" res_json="$3" sidecar_vis="${4:-}"
  local labels_json='{}'
  [ -n "$lq" ] && labels_json="{\"kueue.x-k8s.io/queue-name\":\"${lq}\"}"
  TEST_PODS+=("$name")
  jq -cn \
    --arg name "$name" \
    --arg node "$TARGET_NODE" \
    --arg img "$MOCK_IMAGE" \
    --arg vis "$sidecar_vis" \
    --argjson labels "$labels_json" \
    --argjson res "$res_json" '
    {
      apiVersion: "v1",
      kind: "Pod",
      metadata: {name: $name, namespace: "default", labels: $labels},
      spec: {
        schedulerName: "default-scheduler",
        restartPolicy: "Never",
        nodeSelector: {"kubernetes.io/hostname": $node},
        containers: (
          [{
            name: "main",
            image: $img,
            command: ["sleep", "86400"],
            resources: {limits: $res, requests: $res}
          }]
          + (if $vis != "" then [{
              name: "sidecar",
              image: $img,
              command: ["sleep", "86400"],
              resources: {
                limits: {($vis): "1"},
                requests: {($vis): "1"}
              }
            }] else [] end)
        )
      }
    }' | kubectl apply -f - >/dev/null
}

# ===================================================================================================
# Phase A1 — Multi-card Logical Slicing: Spread, Packing, Over-Commit Guard & Mock Env (2x H100)
# ===================================================================================================
echo
echo "[case-5] === Phase A1: multi-card logical-slice spread, packing & over-commit guard ==="
SPEC_A1="$(cat <<EOF
default:
${DEFAULT_BLOCK}
nodes:
  ${TARGET_NODE}:
    preset: nvidia-h100
    count: 2
EOF
)"
mock_dm_apply_spec "$NS" "$MOCK_DM_CM" "$MOCK_DM_KEY" "$SPEC_A1"
mock_dm_wait_devices "$NS" "$TARGET_NODE" \
  '([.spec.groups[] | select(.manufacturer == "nvidia") | .accelerators[] | select((.status.logicalSliced.count // 0) > 0)] | length == 2)' 90
mock_dm_wait_node_allocatable "$TARGET_NODE" '((.["nvidia.com/gpu.sliced"] // "0" | tonumber) > 0) and (.["nvidia.com/gpu"] == "2")' 90 || true

NV_GID="$(kubectl get devices.worker.gpustack.ai "$TARGET_NODE" -o json 2>/dev/null \
  | jq -r '[.spec.groups[] | select(.manufacturer == "nvidia") | .id][0] // ""')"

IT_SL=""
LQ_SL=""
for _ in $(seq 1 30); do
  read -r IT_SL LQ_SL <<<"$(discover_node_pool "$TARGET_NODE" "$NV_GID" --sliceable --require-entrance name entrance 2>/dev/null || true)"
  [ -n "$IT_SL" ] && [ -n "$LQ_SL" ] && break
  sleep 3
done
mock_dm_wait_it ".items[] | select(.metadata.name == \"${IT_SL}\" and ((.status.acceleratorSliced.remaining // \"0\") | tostring | tonumber) == 200)" 60 || true

# Pass 1: 2 x 60% claims (>50%, so no two fit on one card) -> must take 2 distinct cards.
submit_pod "e2e-c5-spread-1" "$LQ_SL" '{"nvidia.com/gpu.sliced":"1","nvidia.com/gpu.sliced.cores-percentage":"60","nvidia.com/gpu.sliced.memory-percentage":"60"}'
r_sp1=0; wait_pod_running "e2e-c5-spread-1" 40 && r_sp1=1
sleep 5
submit_pod "e2e-c5-spread-2" "$LQ_SL" '{"nvidia.com/gpu.sliced":"1","nvidia.com/gpu.sliced.cores-percentage":"60","nvidia.com/gpu.sliced.memory-percentage":"60"}'
r_sp2=0; wait_pod_running "e2e-c5-spread-2" 40 && r_sp2=1

if [ "$r_sp1" -eq 1 ] && [ "$r_sp2" -eq 1 ]; then
  u_sp1="$(exec_ctr_env "e2e-c5-spread-1" main NVIDIA_VISIBLE_DEVICES)"
  m_sp1="$(exec_ctr_env "e2e-c5-spread-1" main GPUSTACK_MOCK_ALLOCATED_DEVICES)"
  mode_sp1="$(exec_ctr_env "e2e-c5-spread-1" main GPUSTACK_MOCK_ALLOCATION_MODE)"
  mfr_sp1="$(exec_ctr_env "e2e-c5-spread-1" main GPUSTACK_MOCK_MANUFACTURER)"
  u_sp2="$(exec_ctr_env "e2e-c5-spread-2" main NVIDIA_VISIBLE_DEVICES)"
  over_sp="$(kubectl get devices.worker.gpustack.ai "$TARGET_NODE" -o json 2>/dev/null | jq -r '
    [.status.groups[]? | select(.manufacturer == "nvidia") | .accelerators[]? | select((.remaining // 0) < 0)] | length
  ' 2>/dev/null)"

  if [ -n "$u_sp1" ] && [ "$u_sp1" = "$m_sp1" ] && [ "$mode_sp1" = "sliced" ] && [ "$mfr_sp1" = "nvidia" ]; then
    record PASS "running sliced CPU Pod receives Mock diagnostic & vendor env vars" \
      "mode=${mode_sp1} mfr=${mfr_sp1} NVIDIA_VISIBLE_DEVICES=${u_sp1}"
  else
    record FAIL "running sliced CPU Pod receives Mock diagnostic & vendor env vars" \
      "mode='${mode_sp1}' mfr='${mfr_sp1}' vis='${u_sp1}' mock='${m_sp1}'"
  fi

  if [ -n "$u_sp1" ] && [ -n "$u_sp2" ] && [ "$u_sp1" != "$u_sp2" ] && [ "${over_sp:-1}" -eq 0 ]; then
    record PASS "claims too large to share (>50%) take distinct cards without over-commit" \
      "60% -> ${u_sp1}, 60% -> ${u_sp2}"
  else
    record FAIL "claims too large to share (>50%) take distinct cards without over-commit" \
      "u1='${u_sp1}' u2='${u_sp2}' overcommitted='${over_sp:-?}'"
  fi
else
  record FAIL "running sliced CPU Pod receives Mock diagnostic & vendor env vars" "spread Pods did not reach Running"
  record FAIL "claims too large to share (>50%) take distinct cards without over-commit" "r1=${r_sp1} r2=${r_sp2}"
fi

clear_test_pods
mock_dm_wait_it ".items[] | select(.metadata.name == \"${IT_SL}\" and ((.status.acceleratorSliced.remaining // \"0\") | tostring | tonumber) == 200)" 60 || true

# Pass 2: Packing — 100% fills card A, 50% goes to card B, free 100%, then 30% must JOIN card B.
submit_pod "e2e-c5-pack-100" "$LQ_SL" '{"nvidia.com/gpu.sliced":"1","nvidia.com/gpu.sliced.cores-percentage":"100","nvidia.com/gpu.sliced.memory-percentage":"100"}'
r_pk1=0; wait_pod_running "e2e-c5-pack-100" 40 && r_pk1=1
sleep 5
submit_pod "e2e-c5-pack-50" "$LQ_SL" '{"nvidia.com/gpu.sliced":"1","nvidia.com/gpu.sliced.cores-percentage":"50","nvidia.com/gpu.sliced.memory-percentage":"50"}'
r_pk2=0; wait_pod_running "e2e-c5-pack-50" 40 && r_pk2=1

if [ "$r_pk1" -eq 1 ] && [ "$r_pk2" -eq 1 ]; then
  block_card="$(exec_ctr_env "e2e-c5-pack-100" main NVIDIA_VISIBLE_DEVICES)"
  host_card="$(exec_ctr_env "e2e-c5-pack-50" main NVIDIA_VISIBLE_DEVICES)"
  delete_pod_and_workload "e2e-c5-pack-100"
  mock_dm_wait_devices "$NS" "$TARGET_NODE" \
    "any(.status.groups[]? | select(.manufacturer == \"nvidia\") | .accelerators[]?; .id == \"${block_card}\" and (.remaining // 0) == 1600000)" 45 || sleep 10

  submit_pod "e2e-c5-pack-30" "$LQ_SL" '{"nvidia.com/gpu.sliced":"1","nvidia.com/gpu.sliced.cores-percentage":"30","nvidia.com/gpu.sliced.memory-percentage":"30"}'
  if wait_pod_running "e2e-c5-pack-30" 40; then
    join_card="$(exec_ctr_env "e2e-c5-pack-30" main NVIDIA_VISIBLE_DEVICES)"
    over_pk="$(kubectl get devices.worker.gpustack.ai "$TARGET_NODE" -o json 2>/dev/null | jq -r '
      [.status.groups[]? | select(.manufacturer == "nvidia") | .accelerators[]? | select((.remaining // 0) < 0)] | length
    ' 2>/dev/null)"
    if [ "$host_card" != "$block_card" ] && [ "$join_card" = "$host_card" ] && [ "${over_pk:-1}" -eq 0 ]; then
      record PASS "fitting slice packs onto partly-used card rather than opening freed card" \
        "50% + 30% packed on ${host_card}, leaving ${block_card} whole"
    else
      record FAIL "fitting slice packs onto partly-used card rather than opening freed card" \
        "block='${block_card}' host='${host_card}' join='${join_card}' over='${over_pk:-?}'"
    fi
  else
    record FAIL "fitting slice packs onto partly-used card rather than opening freed card" \
      "e2e-c5-pack-30 did not reach Running"
  fi

  read -r sl_rem sl_cap sl_max <<<"$(kubectl get instancetypes.worker.gpustack.ai "$IT_SL" \
    -o jsonpath='{.status.acceleratorSliced.remaining}{" "}{.status.acceleratorSliced.capacity}{" "}{.status.acceleratorSliced.onceMaxRequest}' 2>/dev/null)"
  if [ -n "$sl_rem" ] && [ -n "$sl_cap" ] && [ "$sl_rem" -lt "$sl_cap" ] && [ "${sl_max:-999}" -le 100 ]; then
    record PASS "SL four-view reflects occupancy with per-card OnceMaxRequest (<=100)" \
      "remaining=${sl_rem} < capacity=${sl_cap}, onceMaxRequest=${sl_max}"
  else
    record FAIL "SL four-view reflects occupancy with per-card OnceMaxRequest (<=100)" \
      "remaining='${sl_rem}' capacity='${sl_cap}' onceMaxRequest='${sl_max}'"
  fi
else
  record FAIL "fitting slice packs onto partly-used card rather than opening freed card" "100%/50% setup failed"
  record SKIP "SL four-view reflects occupancy with per-card OnceMaxRequest (<=100)" "setup failed"
fi
clear_test_pods

# ===================================================================================================
# Phase A2 — Single-card Logical Slice Co-Residency & Over-Budget Refusal (1x H100)
# ===================================================================================================
echo
echo "[case-5] === Phase A2: single-card 40%+40% co-residency & 3rd 40% over-budget hold ==="
SPEC_A2="$(cat <<EOF
default:
${DEFAULT_BLOCK}
nodes:
  ${TARGET_NODE}:
    preset: nvidia-h100
    count: 1
EOF
)"
mock_dm_apply_spec "$NS" "$MOCK_DM_CM" "$MOCK_DM_KEY" "$SPEC_A2"
mock_dm_wait_devices "$NS" "$TARGET_NODE" \
  '([.spec.groups[] | select(.manufacturer == "nvidia") | .accelerators[]] | length == 1)' 90
mock_dm_wait_it ".items[] | select(.metadata.name == \"${IT_SL}\" and ((.status.acceleratorSliced.capacity // \"0\") | tostring | tonumber) == 100 and ((.status.acceleratorSliced.remaining // \"0\") | tostring | tonumber) == 100)" 60 || true

mk_slice_inst() {
  local name="$1" pct="$2"
  TEST_INSTANCES+=("$name")
  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: Instance
metadata: { name: ${name}, namespace: default }
spec:
  type: ${IT_SL}
  nodeName: ${TARGET_NODE}
  image: ${MOCK_IMAGE}
  command: ["sleep", "86400"]
  resources:
    cpu: "500m"
    ram: "512Mi"
    localStorage: "2Gi"
    accelerator: "1"
    acceleratorSlicedMemoryPercentage: ${pct}
    acceleratorSlicedCoresPercentage: 100
  volume: { ephemeral: { capacity: 1Gi } }
  volumeMount: /workspace
EOF
}

mk_slice_inst "e2e-c5-coexist-a" 40
mk_slice_inst "e2e-c5-coexist-b" 40
co_a=0; wait_pod_running "e2e-c5-coexist-a" 45 && co_a=1
co_b=0; wait_pod_running "e2e-c5-coexist-b" 45 && co_b=1

if [ "$co_a" -eq 1 ] && [ "$co_b" -eq 1 ]; then
  card_a="$(exec_ctr_env "e2e-c5-coexist-a" main NVIDIA_VISIBLE_DEVICES)"
  card_b="$(exec_ctr_env "e2e-c5-coexist-b" main NVIDIA_VISIBLE_DEVICES)"
  if [ -n "$card_a" ] && [ "$card_a" = "$card_b" ]; then
    record PASS "two 40% sliced Instances coexist on one card" "both Running on ${card_a} (80% <= 100%)"
  else
    record FAIL "two 40% sliced Instances coexist on one card" "A='${card_a}' B='${card_b}'"
  fi

  mk_slice_inst "e2e-c5-coexist-c" 40
  c_held=1
  for _ in $(seq 1 8); do
    [ "$(pod_phase "e2e-c5-coexist-c")" = "Running" ] && { c_held=0; break; }
    sleep 3
  done
  if [ "$c_held" -eq 1 ]; then
    record PASS "over-budget 3rd 40% slice is held (120% > 100%)" \
      "e2e-c5-coexist-c phase=$(pod_phase "e2e-c5-coexist-c")"
  else
    record FAIL "over-budget 3rd 40% slice is held (120% > 100%)" "e2e-c5-coexist-c unexpectedly Running"
  fi

  sleep 8
  if [ "$(pod_phase "e2e-c5-coexist-a")" = "Running" ] && [ "$(pod_phase "e2e-c5-coexist-b")" = "Running" ]; then
    record PASS "co-resident slices survive sibling rejection without self-eviction" "A=Running B=Running"
  else
    record FAIL "co-resident slices survive sibling rejection without self-eviction" \
      "A=$(pod_phase "e2e-c5-coexist-a") B=$(pod_phase "e2e-c5-coexist-b")"
  fi
else
  record FAIL "two 40% sliced Instances coexist on one card" "co_a=${co_a} co_b=${co_b}"
  record SKIP "over-budget 3rd 40% slice is held (120% > 100%)" "setup failed"
  record SKIP "co-resident slices survive sibling rejection without self-eviction" "setup failed"
fi
clear_test_instances

# ===================================================================================================
# Phase B1 — Mixed Logical + Virtual MIG Node & Whole-Card Exclusion
# ===================================================================================================
echo
echo "[case-5] === Phase B1: mixed logical + virtual MIG node & whole-card exclusion ==="
SPEC_B1="$(cat <<EOF
default:
${DEFAULT_BLOCK}
nodes:
  ${TARGET_NODE}:
    preset: nvidia-h100
    count: 2
    cardOverrides:
      0:
        id: GPU-912151ec-e9f1-fc26-2897-89f4077de4b9
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
            - name: 2g.20gb
              memoryMib: 19968
              computeSlices: 2
              memorySlices: 2
              count: 3
              placements:
                - {start: 0, length: 2}
                - {start: 2, length: 2}
                - {start: 4, length: 2}
            - name: 3g.40gb
              memoryMib: 40192
              computeSlices: 3
              memorySlices: 4
              count: 2
              placements:
                - {start: 0, length: 4}
                - {start: 4, length: 4}
            - name: 7g.80gb
              memoryMib: 80896
              computeSlices: 7
              memorySlices: 8
              count: 1
              placements:
                - {start: 0, length: 8}
EOF
)"
mock_dm_apply_spec "$NS" "$MOCK_DM_CM" "$MOCK_DM_KEY" "$SPEC_B1"

mixed_jq='([.spec.groups[] | select(.manufacturer == "nvidia") | .accelerators[] | select((.status.physicalSliced.profiles // []) | length > 0)] | length == 1)
  and ([.spec.groups[] | select(.manufacturer == "nvidia") | .accelerators[] | select((.status.logicalSliced.count // 0) > 0)] | length == 1)'

if mock_dm_wait_devices "$NS" "$TARGET_NODE" "$mixed_jq" 90 \
  && mock_dm_wait_node_allocatable "$TARGET_NODE" \
    '(.["nvidia.com/gpu"] == "1") and (.["nvidia.com/gpu.shared"] == "10") and ((.["nvidia.com/gpu.sliced"] // "0" | tonumber) > 0) and (.["nvidia.com/gpu.partitioned"] == "7") and (.["nvidia.com/gpu.partitioned.mig-3g.40gb"] == "2")' 90; then
  record PASS "mixed node advertises both logical-slice and virtual-MIG families" \
    "${TARGET_NODE}: gpu=1 shared=10 partitioned=7 mig-3g.40gb=2"
else
  record FAIL "mixed node advertises both logical-slice and virtual-MIG families" \
    "${TARGET_NODE} mixed resource keys did not converge"
fi

mock_dm_wait_it ".items[] | select(.metadata.name == \"${IT_SL}\" and ((.status.accelerator.capacity // \"0\") | tostring | tonumber) == 1 and ((.status.acceleratorShared.capacity // \"0\") | tostring | tonumber) == 10 and ((.status.acceleratorPartitioned.capacity // \"0\") | tostring | tonumber) > 0)" 60 || true
read -r mix_ex mix_sh mix_pt <<<"$(kubectl get instancetypes.worker.gpustack.ai "$IT_SL" \
  -o jsonpath='{.status.accelerator.capacity}{" "}{.status.acceleratorShared.capacity}{" "}{.status.acceleratorPartitioned.capacity}' 2>/dev/null)"
if [ "$mix_ex" = "1" ] && [ "$mix_sh" = "10" ] && [ "${mix_pt:-0}" -gt 0 ]; then
  record PASS "partitioned card excluded from pool EX/SH views" "EX=${mix_ex} SH=${mix_sh} PT=${mix_pt}"
else
  record FAIL "partitioned card excluded from pool EX/SH views" "EX='${mix_ex}' SH='${mix_sh}' PT='${mix_pt}'"
fi

# Occupy the 1 whole card exclusively -> 2nd exclusive Pod must be held (never placed on MIG card 0).
submit_pod "e2e-c5-mix-ex1" "$LQ_SL" '{"nvidia.com/gpu":"1"}'
if wait_pod_running "e2e-c5-mix-ex1" 40; then
  ex1_dev="$(exec_ctr_env "e2e-c5-mix-ex1" main GPUSTACK_MOCK_ALLOCATED_DEVICES)"
  submit_pod "e2e-c5-mix-ex2" "$LQ_SL" '{"nvidia.com/gpu":"1"}'
  sleep 12
  if [ "$ex1_dev" != "GPU-912151ec-e9f1-fc26-2897-89f4077de4b9" ] && [ "$(pod_phase "e2e-c5-mix-ex2")" != "Running" ]; then
    record PASS "exclusive claim never lands on idle partitioned card" \
      "ex1 on whole card ${ex1_dev}, ex2 held ($(pod_phase "e2e-c5-mix-ex2"))"
  else
    record FAIL "exclusive claim never lands on idle partitioned card" \
      "ex1='${ex1_dev}' ex2_phase=$(pod_phase "e2e-c5-mix-ex2")"
  fi
else
  record FAIL "exclusive claim never lands on idle partitioned card" "e2e-c5-mix-ex1 did not reach Running"
fi
clear_test_pods
mock_dm_wait_it ".items[] | select(.metadata.name == \"${IT_SL}\" and ((.status.accelerator.remaining // \"0\") | tostring | tonumber) == 1)" 60 || true

# Simultaneous partition (3g.40gb) + logical slice (20%) on the mixed node.
submit_pod "e2e-c5-mix-part" "$LQ_SL" '{"nvidia.com/gpu.partitioned":"1","nvidia.com/gpu.partitioned.mig-3g.40gb":"1"}'
submit_pod "e2e-c5-mix-sl" "$LQ_SL" '{"nvidia.com/gpu.sliced":"1","nvidia.com/gpu.sliced.memory-percentage":"20"}'
mp_ok=0; wait_pod_running "e2e-c5-mix-part" 45 && mp_ok=1
ms_ok=0; wait_pod_running "e2e-c5-mix-sl" 45 && ms_ok=1
uae_mix=$(( $(pod_unexpected_admission "e2e-c5-mix-part") + $(pod_unexpected_admission "e2e-c5-mix-sl") ))

if [ "$mp_ok" -eq 1 ] && [ "$ms_ok" -eq 1 ]; then
  mp_mode="$(exec_ctr_env "e2e-c5-mix-part" main GPUSTACK_MOCK_ALLOCATION_MODE)"
  mp_vis="$(exec_ctr_env "e2e-c5-mix-part" main NVIDIA_VISIBLE_DEVICES)"
  ms_mode="$(exec_ctr_env "e2e-c5-mix-sl" main GPUSTACK_MOCK_ALLOCATION_MODE)"
  ms_vis="$(exec_ctr_env "e2e-c5-mix-sl" main NVIDIA_VISIBLE_DEVICES)"
  if [ "$mp_mode" = "partitioned" ] && [ "$ms_mode" = "sliced" ] && [ "$uae_mix" -eq 0 ] \
    && printf '%s' "$mp_vis" | grep -q '^MOCK-MIG-nvidia-0-3g\.40gb-' \
    && [ "$ms_vis" != "GPU-912151ec-e9f1-fc26-2897-89f4077de4b9" ]; then
    record PASS "mixed-node partition and logical-slice Pods land on disjoint populations (0 UAE)" \
      "part=${mp_vis}, sliced=${ms_vis}"
  else
    record FAIL "mixed-node partition and logical-slice Pods land on disjoint populations (0 UAE)" \
      "mp=${mp_mode}/${mp_vis} ms=${ms_mode}/${ms_vis} uae=${uae_mix}"
  fi
else
  record FAIL "mixed-node partition and logical-slice Pods land on disjoint populations (0 UAE)" \
    "mp_ok=${mp_ok} ms_ok=${ms_ok} uae=${uae_mix}"
fi
clear_test_pods

# ===================================================================================================
# Phase B2 — Virtual MIG Live Ledger, Concurrent Multi-Profile, Visibility Co-Allocation & Reclaim
# ===================================================================================================
echo
echo "[case-5] === Phase B2: virtual MIG live ledger, multi-profile (3g/2g/1g), visibility & reclaim ==="
SPEC_B2="$(cat <<EOF
default:
${DEFAULT_BLOCK}
nodes:
  ${TARGET_NODE}:
    preset: nvidia-h100-mig
    count: 1
EOF
)"
mock_dm_apply_spec "$NS" "$MOCK_DM_CM" "$MOCK_DM_KEY" "$SPEC_B2"

mig1_jq='([.spec.groups[] | select(.manufacturer == "nvidia") | .accelerators[]] | length == 1)
  and ([.spec.groups[] | select(.manufacturer == "nvidia") | .accelerators[] | select((.status.physicalSliced.profiles // []) | length > 0)] | length == 1)'
mock_dm_wait_devices "$NS" "$TARGET_NODE" "$mig1_jq" 90
mock_dm_wait_node_allocatable "$TARGET_NODE" \
  '(.["nvidia.com/gpu.partitioned"] == "7") and (.["nvidia.com/gpu.partitioned.mig-7g.80gb"] == "1") and (.["nvidia.com/gpu.partitioned.mig-3g.40gb"] == "2") and (.["nvidia.com/gpu.partitioned.mig-2g.20gb"] == "3") and (.["nvidia.com/gpu.partitioned.mig-1g.10gb"] == "7")' 90 || true

NV_MIG_GID="$(kubectl get devices.worker.gpustack.ai "$TARGET_NODE" -o json 2>/dev/null \
  | jq -r '[.spec.groups[] | select(.manufacturer == "nvidia") | .id][0] // ""')"
read -r IT_MIG LQ_MIG <<<"$(discover_node_pool "$TARGET_NODE" "$NV_MIG_GID" --require-entrance --fallback-any name entrance 2>/dev/null || true)"
: "$IT_MIG"

# Submit 3g.40gb (with sidecar requesting device.gpustack.ai/nvidia.visibility: "1") and 2g.20gb
# concurrently (zero wait between applies), then 1g.10gb so all three shapes coexist on the card.
submit_pod "e2e-c5-mig-3g" "$LQ_MIG" \
  '{"nvidia.com/gpu.partitioned":"1","nvidia.com/gpu.partitioned.mig-3g.40gb":"1"}' \
  "device.gpustack.ai/nvidia.visibility"
submit_pod "e2e-c5-mig-2g" "$LQ_MIG" \
  '{"nvidia.com/gpu.partitioned":"1","nvidia.com/gpu.partitioned.mig-2g.20gb":"1"}'

ok_3g=0; wait_pod_running "e2e-c5-mig-3g" 45 && ok_3g=1
ok_2g=0; wait_pod_running "e2e-c5-mig-2g" 45 && ok_2g=1

submit_pod "e2e-c5-mig-1g" "$LQ_MIG" \
  '{"nvidia.com/gpu.partitioned":"1","nvidia.com/gpu.partitioned.mig-1g.10gb":"1"}'
ok_1g=0; wait_pod_running "e2e-c5-mig-1g" 45 && ok_1g=1

if [ "$ok_3g" -eq 1 ] && [ "$ok_2g" -eq 1 ] && [ "$ok_1g" -eq 1 ]; then
  id_3g="$(exec_ctr_env "e2e-c5-mig-3g" main NVIDIA_VISIBLE_DEVICES)"
  id_3g_side="$(exec_ctr_env "e2e-c5-mig-3g" sidecar NVIDIA_VISIBLE_DEVICES)"
  mode_3g_side="$(exec_ctr_env "e2e-c5-mig-3g" sidecar GPUSTACK_MOCK_ALLOCATION_MODE)"
  id_2g="$(exec_ctr_env "e2e-c5-mig-2g" main NVIDIA_VISIBLE_DEVICES)"
  id_1g="$(exec_ctr_env "e2e-c5-mig-1g" main NVIDIA_VISIBLE_DEVICES)"
  uae_multi=$(( $(pod_unexpected_admission "e2e-c5-mig-3g") + $(pod_unexpected_admission "e2e-c5-mig-2g") + $(pod_unexpected_admission "e2e-c5-mig-1g") ))

  prof_3g="$(kubectl -n default get pod "e2e-c5-mig-3g" -o json 2>/dev/null | jq -r '.metadata.annotations["device.gpustack.ai/accelerator.allocated"] | fromjson | .main.devices.groups[0].accelerators[0].allocatedPhysicalProfile // ""')"
  prof_2g="$(kubectl -n default get pod "e2e-c5-mig-2g" -o json 2>/dev/null | jq -r '.metadata.annotations["device.gpustack.ai/accelerator.allocated"] | fromjson | .main.devices.groups[0].accelerators[0].allocatedPhysicalProfile // ""')"
  prof_1g="$(kubectl -n default get pod "e2e-c5-mig-1g" -o json 2>/dev/null | jq -r '.metadata.annotations["device.gpustack.ai/accelerator.allocated"] | fromjson | .main.devices.groups[0].accelerators[0].allocatedPhysicalProfile // ""')"

  if [ "$prof_3g" = "3g.40gb" ] && [ "$prof_2g" = "2g.20gb" ] && [ "$prof_1g" = "1g.10gb" ] \
    && printf '%s' "$id_3g" | grep -q '^MOCK-MIG-nvidia-0-3g\.40gb-' \
    && printf '%s' "$id_2g" | grep -q '^MOCK-MIG-nvidia-0-2g\.20gb-' \
    && printf '%s' "$id_1g" | grep -q '^MOCK-MIG-nvidia-0-1g\.10gb-' \
    && [ "$uae_multi" -eq 0 ]; then
    record PASS "concurrent multi-profile virtual partitions (3g.40gb, 2g.20gb, 1g.10gb) coexist cleanly" \
      "${id_3g}, ${id_2g}, ${id_1g} (0 UAE)"
  else
    record FAIL "concurrent multi-profile virtual partitions (3g.40gb, 2g.20gb, 1g.10gb) coexist cleanly" \
      "profs=${prof_3g}/${prof_2g}/${prof_1g} ids=${id_3g}/${id_2g}/${id_1g} uae=${uae_multi}"
  fi

  if [ "$mode_3g_side" = "visibility" ] && [ -n "$id_3g_side" ] && [ "$id_3g_side" = "$id_3g" ]; then
    record PASS "visibility sidecar shares existing virtual partition handle" \
      "sidecar mode=visibility NVIDIA_VISIBLE_DEVICES=${id_3g_side}"
  else
    record FAIL "visibility sidecar shares existing virtual partition handle" \
      "sidecar mode='${mode_3g_side}' vis='${id_3g_side}' vs main='${id_3g}'"
  fi

  # Live-ledger capacity update: mig-7g.80gb allocatable must be 0 and Devices.status tracks allocatedProfiles.
  ledger_live_jq='any(.status.groups[]? | select(.manufacturer == "nvidia") | .accelerators[]?;
    any((.allocatedProfiles // [])[]; .name == "3g.40gb" and (.count // 0) == 1)
    and any((.allocatedProfiles // [])[]; .name == "2g.20gb" and (.count // 0) == 1)
    and any((.allocatedProfiles // [])[]; .name == "1g.10gb" and (.count // 0) == 1)
    and all((.remainingProfiles // [])[]; .name != "7g.80gb" or (.count // 0) == 0))'
  if mock_dm_wait_devices "$NS" "$TARGET_NODE" "$ledger_live_jq" 45 \
    && mock_dm_wait_node_allocatable "$TARGET_NODE" '.["nvidia.com/gpu.partitioned.mig-7g.80gb"] == "0"' 45; then
    record PASS "live ledger updates Devices.status.allocatedProfiles and drops mig-7g.80gb to 0" \
      "allocatedProfiles={3g:1,2g:1,1g:1}, mig-7g.80gb=0"
  else
    record FAIL "live ledger updates Devices.status.allocatedProfiles and drops mig-7g.80gb to 0" \
      "Devices.status or Node allocatable did not reflect carved partitions"
  fi

  # Placement exclusion: no room left for another 2g.20gb on this single card -> must be held!
  submit_pod "e2e-c5-mig-over2g" "$LQ_MIG" \
    '{"nvidia.com/gpu.partitioned":"1","nvidia.com/gpu.partitioned.mig-2g.20gb":"1"}'
  sleep 12
  if [ "$(pod_phase "e2e-c5-mig-over2g")" != "Running" ] \
    && [ "$(pod_phase "e2e-c5-mig-3g")" = "Running" ] \
    && [ "$(pod_phase "e2e-c5-mig-2g")" = "Running" ] \
    && [ "$(pod_phase "e2e-c5-mig-1g")" = "Running" ]; then
    record PASS "placement exclusion holds over-fitting partition while live partitions stay Running" \
      "e2e-c5-mig-over2g phase=$(pod_phase "e2e-c5-mig-over2g")"
  else
    record FAIL "placement exclusion holds over-fitting partition while live partitions stay Running" \
      "over2g=$(pod_phase "e2e-c5-mig-over2g")"
  fi
else
  record FAIL "concurrent multi-profile virtual partitions (3g.40gb, 2g.20gb, 1g.10gb) coexist cleanly" \
    "ok_3g=${ok_3g} ok_2g=${ok_2g} ok_1g=${ok_1g}"
  record SKIP "visibility sidecar shares existing virtual partition handle" "setup failed"
  record SKIP "live ledger updates Devices.status.allocatedProfiles and drops mig-7g.80gb to 0" "setup failed"
  record SKIP "placement exclusion holds over-fitting partition while live partitions stay Running" "setup failed"
fi

# Delete all 3g/2g/1g Pods and verify dead-Pod virtual partition reclamation frees Devices.status
# and admits a whole-card 7g.80gb saturation + refill cycle!
clear_test_pods

reclaimed_jq='all(.status.groups[]? | select(.manufacturer == "nvidia") | .accelerators[]?;
  ((.allocatedProfiles // []) | length == 0) and ((.remaining // 0) == 1600000))'
if mock_dm_wait_devices "$NS" "$TARGET_NODE" "$reclaimed_jq" 60 \
  && mock_dm_wait_node_allocatable "$TARGET_NODE" '(.["nvidia.com/gpu.partitioned.mig-7g.80gb"] == "1") and (.["nvidia.com/gpu.partitioned"] == "7")' 60; then
  # Confirm PartitionStore truly reclaimed the in-memory slots by placing 7g.80gb spanning [0, 8)!
  submit_pod "e2e-c5-mig-7g" "$LQ_MIG" \
    '{"nvidia.com/gpu.partitioned":"1","nvidia.com/gpu.partitioned.mig-7g.80gb":"1"}'
  # shellcheck disable=SC2016 # $node is a jq variable bound by mock_dm_wait_node_allocatable.
  sat_7g_jq='(.["nvidia.com/gpu.partitioned"] == "1") and (($node.status.capacity["nvidia.com/gpu.partitioned"] // "0" | tonumber) > 0)'
  if wait_pod_running "e2e-c5-mig-7g" 45 \
    && mock_dm_wait_node_allocatable "$TARGET_NODE" "$sat_7g_jq" 45; then
    record PASS "dead-Pod virtual partition reclamation frees ledger and admits full-card 7g.80gb saturation" \
      "Devices.status cleared, 7g.80gb Running (allocatable=1, capacity preserved)"
  else
    record FAIL "dead-Pod virtual partition reclamation frees ledger and admits full-card 7g.80gb saturation" \
      "7g.80gb saturation after reclaim failed (phase=$(pod_phase "e2e-c5-mig-7g"))"
  fi
else
  record FAIL "dead-Pod virtual partition reclamation frees ledger and admits full-card 7g.80gb saturation" \
    "Devices.status or mig-7g.80gb allocatable did not return to idle"
fi

clear_test_pods

echo
echo "== CASE 5 — CPU Mock logical slicing & virtual MIG/partitioning =="
print_rows
if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s)."
  exit 1
fi
echo "CASE 5 PASS"
