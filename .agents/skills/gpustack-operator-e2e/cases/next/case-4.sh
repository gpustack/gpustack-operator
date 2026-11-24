#!/usr/bin/env bash
#
# CASE 4 — Dynamic Mock fault injection: single-card unhealthy, unqueryable/lost-card in-place
#          unhealthy retention & sibling Index preservation, total-failure lastDetected retention,
#          recovery, and clean shrink pruning
#          (MUTATING, self-recovering; AUTO-SKIPS when Mock Device Manager is not active)
#
#   case-4.sh <NS>
#
# Prerequisites: nodes=1; gpu=any; cluster=any; inputs=E2E_MOCK_IMAGE
# Goal:        Verify the full Detector and Allocator hardware-fault and recovery lifecycle end to
#              end on a live cluster by hot-reloading the Mock Device Manager ConfigMap (zero Pod
#              restarts):
#                1. Baseline healthy 4-card preset: all 4 cards (index 0..3) report healthy and
#                   Devices carries device.gpustack.ai/mock="true";
#                2. Single-card unhealthy=true: index 1 flips to status.unhealthy=true, healthy
#                   siblings (0, 2, 3) still allocate cleanly, and kubelet/scheduler never places a
#                   workload onto the unhealthy card;
#                3. Single-card queryState=unqueryable (lost card): index 2 is retained in place at
#                   its original index 2 with status.unhealthy=true while sibling card index values
#                   (0, 1, 3) stay preserved unchanged;
#                4. Total failure (all cards queryState=unqueryable and fault.totalFailure=true):
#                   Detector preserves lastDetected so previously discovered cards are NOT wiped out
#                   of Devices;
#                5. Recovery (queryState=ok, unhealthy=false, totalFailure=false): status.unhealthy
#                   clears across all 4 cards and full pool capacity (4) returns;
#                6. Clean shrink (count=2): removed slots (index 2, 3) are pruned cleanly from
#                   Devices with zero ghost unhealthy entries and allocatable updates to 2.
# Environment: Any CPU-only Kind/k3s cluster running the GPUStack Operator with Mock Device Manager
#              enabled (deviceManager.mock.enabled=true). AUTO-SKIPS cleanly with SKIP rows and
#              exit 0 when Mock Device Manager is not active on the cluster.
# Inputs:      Hot-reloaded mock devices YAML specs written through _mock-dm-lib.sh plus CPU alpine
#              Pods (override via E2E_MOCK_IMAGE) verifying unhealthy-card avoidance.
# Expected:    All 6 lifecycle transitions record PASS.
# Cleanup:     EXIT trap deletes all test Pods/Workloads and restores the original mock ConfigMap
#              via mock_dm_restore_cm.
set -uo pipefail

# shellcheck disable=SC2034 # Read by _rows-lib.sh.
CASE_ID=4
CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=/dev/null
. "${CASE_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_pool-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_mock-dm-lib.sh"

NS="${1:?usage: case-4.sh <NS>}"
MOCK_IMAGE="${E2E_MOCK_IMAGE:-alpine}"
ORIG_CM_FILE="$(mktemp)"
CM_SNAPSHOT_OK=0
TEST_PODS=()

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
  echo
  echo "[case-4] cleanup: deleting test Pods and restoring original mock ConfigMap"
  clear_test_pods
  if [ "$CM_SNAPSHOT_OK" -eq 1 ]; then
    mock_dm_restore_cm "$NS" "$MOCK_DM_CM" "$ORIG_CM_FILE" || true
  fi
  rm -f "$ORIG_CM_FILE"
}
trap cleanup EXIT

if ! mock_dm_detect_env "$NS"; then
  skip_msg="mock device-manager DaemonSet (gpustack.ai/manufacturer=mock) is not active on this cluster"
  record SKIP "baseline 4-card healthy mock inventory" "$skip_msg"
  record SKIP "single-card unhealthy flag & scheduler avoidance" "$skip_msg"
  record SKIP "single-card unqueryable in-place unhealthy retention & index preservation" "$skip_msg"
  record SKIP "total-failure lastDetected retention" "$skip_msg"
  record SKIP "recovery clears unhealthy and restores capacity" "$skip_msg"
  record SKIP "clean shrink prunes removed slots without ghost entries" "$skip_msg"
  echo
  echo "== CASE 4 — SKIPPED (${skip_msg}) =="
  print_rows
  exit 0
fi

# Pick a manufacturer + 4-card preset supported by the active mock DaemonSet.
PRESET="nvidia-h100"
MFR="nvidia"
EXCL_RES="nvidia.com/gpu"
FALLBACK_PRESET="amd-mi300x"
if ! mock_dm_manufacturer_allowed nvidia; then
  if mock_dm_manufacturer_allowed amd; then
    PRESET="amd-mi300x"; MFR="amd"; EXCL_RES="amd.com/gpu"; FALLBACK_PRESET="amd-rx-7800-xt"
  elif mock_dm_manufacturer_allowed ascend; then
    PRESET="ascend-910b2"; MFR="ascend"; EXCL_RES="huawei.com/npu"; FALLBACK_PRESET="ascend-310p3"
  else
    skip_msg="active mock DaemonSet restricts manufacturers (${MOCK_DM_MANUFACTURERS}) outside nvidia/amd/ascend"
    record SKIP "dynamic mock fault-injection suite" "$skip_msg"
    print_rows
    exit 0
  fi
fi
mock_dm_manufacturer_allowed amd || FALLBACK_PRESET="$PRESET"

if mock_dm_snapshot_cm "$NS" "$MOCK_DM_CM" "$ORIG_CM_FILE"; then
  CM_SNAPSHOT_OK=1
else
  echo "[case-4] FATAL: failed to snapshot ConfigMap ${NS}/${MOCK_DM_CM}"
  exit 1
fi

TARGET_NODE="$MOCK_DM_PRIMARY_NODE"
echo "[case-4] target node=${TARGET_NODE} cm=${MOCK_DM_CM}/${MOCK_DM_KEY} preset=${PRESET} (${MFR})"

render_spec() {
  local node_body="$1"
  cat <<EOF
default:
  preset: ${FALLBACK_PRESET}
  count: 1
nodes:
  ${TARGET_NODE}:
${node_body}
EOF
}

# ===================================================================================================
# Step 1 — Baseline healthy 4-card preset
# ===================================================================================================
echo
echo "[case-4] === Step 1: baseline 4x ${PRESET} healthy ==="
SPEC_STEP1="$(render_spec "    preset: ${PRESET}
    count: 4")"
mock_dm_apply_spec "$NS" "$MOCK_DM_CM" "$MOCK_DM_KEY" "$SPEC_STEP1"

step1_jq="(.metadata.annotations[\"device.gpustack.ai/mock\"] == \"true\")
  and (([.spec.groups[] | select(.manufacturer == \"${MFR}\") | .accelerators[]] | length) == 4)
  and (([.spec.groups[] | select(.manufacturer == \"${MFR}\") | .accelerators[] | .index]) == [0, 1, 2, 3])
  and (all(.spec.groups[] | select(.manufacturer == \"${MFR}\") | .accelerators[]; (.status.unhealthy // false) == false))"

if mock_dm_wait_devices "$NS" "$TARGET_NODE" "$step1_jq" 90 \
  && mock_dm_wait_node_allocatable "$TARGET_NODE" ".[\"${EXCL_RES}\"] == \"4\"" 90; then
  record PASS "baseline 4-card healthy mock inventory" \
    "${TARGET_NODE}: 4 healthy ${MFR} cards (index 0..3), device.gpustack.ai/mock=true"
else
  record FAIL "baseline 4-card healthy mock inventory" \
    "${TARGET_NODE} did not converge to 4 healthy ${MFR} cards"
fi

BASE_DEVS_JSON="$(kubectl get devices.worker.gpustack.ai "$TARGET_NODE" -o json 2>/dev/null)"
CARD0_ID="$(printf '%s' "$BASE_DEVS_JSON" | jq -r --arg m "$MFR" '[.spec.groups[] | select(.manufacturer == $m) | .accelerators[] | select(.index == 0) | .id][0] // ""')"
CARD1_ID="$(printf '%s' "$BASE_DEVS_JSON" | jq -r --arg m "$MFR" '[.spec.groups[] | select(.manufacturer == $m) | .accelerators[] | select(.index == 1) | .id][0] // ""')"
CARD2_ID="$(printf '%s' "$BASE_DEVS_JSON" | jq -r --arg m "$MFR" '[.spec.groups[] | select(.manufacturer == $m) | .accelerators[] | select(.index == 2) | .id][0] // ""')"
CARD3_ID="$(printf '%s' "$BASE_DEVS_JSON" | jq -r --arg m "$MFR" '[.spec.groups[] | select(.manufacturer == $m) | .accelerators[] | select(.index == 3) | .id][0] // ""')"
GROUP_ID="$(printf '%s' "$BASE_DEVS_JSON" | jq -r --arg m "$MFR" '[.spec.groups[] | select(.manufacturer == $m) | .id][0] // ""')"
read -r IT_POOL LQ_POOL <<<"$(discover_node_pool "$TARGET_NODE" "$GROUP_ID" --require-entrance --fallback-any name entrance 2>/dev/null || true)"

# ===================================================================================================
# Step 2 — Single-card unhealthy: true on index 1 -> marked unhealthy, scheduler avoids it
# ===================================================================================================
echo
echo "[case-4] === Step 2: single-card unhealthy=true on index 1 (${CARD1_ID}) ==="
SPEC_STEP2="$(render_spec "    preset: ${PRESET}
    count: 4
    cardOverrides:
      1:
        unhealthy: true")"
mock_dm_apply_spec "$NS" "$MOCK_DM_CM" "$MOCK_DM_KEY" "$SPEC_STEP2"

step2_jq="([.spec.groups[] | select(.manufacturer == \"${MFR}\") | .accelerators[] | select(.index == 1 and .status.unhealthy == true)] | length == 1)
  and ([.spec.groups[] | select(.manufacturer == \"${MFR}\") | .accelerators[] | select(.index != 1 and (.status.unhealthy // false) == false)] | length == 3)"

if mock_dm_wait_devices "$NS" "$TARGET_NODE" "$step2_jq" 90 \
  && mock_dm_wait_node_allocatable "$TARGET_NODE" ".[\"${EXCL_RES}\"] == \"3\"" 90; then
  record PASS "single-card unhealthy=true updates Devices and Node allocatable" \
    "index 1 (${CARD1_ID}) status.unhealthy=true; ${EXCL_RES} allocatable=3"
else
  record FAIL "single-card unhealthy=true updates Devices and Node allocatable" \
    "index 1 did not flip to unhealthy=true with allocatable=3"
fi

# Verify 3 exclusive Pods run on healthy siblings (0, 2, 3) avoiding CARD1_ID, and 4th is held.
avoid_ok=1
allocated_set=""
for idx in 1 2 3; do
  p="e2e-c4-unhealthy-ex-${idx}"
  TEST_PODS+=("$p")
  jq -cn \
    --arg name "$p" \
    --arg node "$TARGET_NODE" \
    --arg lq "$LQ_POOL" \
    --arg img "$MOCK_IMAGE" \
    --arg res "$EXCL_RES" '
    {
      apiVersion: "v1",
      kind: "Pod",
      metadata: {
        name: $name,
        namespace: "default",
        labels: (if $lq != "" then {"kueue.x-k8s.io/queue-name": $lq} else {} end)
      },
      spec: {
        schedulerName: "default-scheduler",
        restartPolicy: "Never",
        nodeSelector: {"kubernetes.io/hostname": $node},
        containers: [{
          name: "main",
          image: $img,
          command: ["sleep", "86400"],
          resources: {limits: {($res): "1"}, requests: {($res): "1"}}
        }]
      }
    }' | kubectl apply -f - >/dev/null

  p_run=""
  for _ in $(seq 1 40); do
    [ "$(kubectl -n default get pod "$p" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Running" ] && { p_run=1; break; }
    sleep 3
  done
  if [ -z "$p_run" ]; then
    avoid_ok=0
    break
  fi
  got_dev=""
  for _ in $(seq 1 10); do
    got_dev="$(kubectl -n default exec "$p" -c main -- sh -c "printf '%s' \"\${GPUSTACK_MOCK_ALLOCATED_DEVICES:-}\"" 2>/dev/null || true)"
    [ -n "$got_dev" ] && break
    sleep 2
  done
  if [ "$got_dev" = "$CARD1_ID" ] || [ -z "$got_dev" ]; then
    avoid_ok=0
    break
  fi
  allocated_set="${allocated_set}${got_dev} "
done

if [ "$avoid_ok" -eq 1 ]; then
  p4="e2e-c4-unhealthy-ex-4"
  TEST_PODS+=("$p4")
  jq -cn \
    --arg name "$p4" \
    --arg node "$TARGET_NODE" \
    --arg img "$MOCK_IMAGE" \
    --arg res "$EXCL_RES" '
    {
      apiVersion: "v1",
      kind: "Pod",
      metadata: {name: $name, namespace: "default"},
      spec: {
        schedulerName: "default-scheduler",
        restartPolicy: "Never",
        nodeSelector: {"kubernetes.io/hostname": $node},
        containers: [{
          name: "main",
          image: $img,
          command: ["sleep", "86400"],
          resources: {limits: {($res): "1"}, requests: {($res): "1"}}
        }]
      }
    }' | kubectl apply -f - >/dev/null
  sleep 12
  p4_phase="$(kubectl -n default get pod "$p4" -o jsonpath='{.status.phase}' 2>/dev/null)"
  if [ "$p4_phase" != "Running" ]; then
    record PASS "healthy siblings allocate while unhealthy card is avoided" \
      "3 Pods Running on [${allocated_set% }], 4th held (phase=${p4_phase:-Pending})"
  else
    record FAIL "healthy siblings allocate while unhealthy card is avoided" \
      "4th Pod unexpectedly reached Running despite 1 unhealthy card"
  fi
else
  record FAIL "healthy siblings allocate while unhealthy card is avoided" \
    "healthy sibling allocation failed or landed on unhealthy ${CARD1_ID}"
fi
clear_test_pods
if [ -n "$IT_POOL" ]; then
  mock_dm_wait_it ".items[] | select(.metadata.name == \"${IT_POOL}\")" 30 || true
fi

# ===================================================================================================
# Step 3 — Single-card queryState: unqueryable (lost card at index 2) -> retained in place, sibling indexes preserved
# ===================================================================================================
echo
echo "[case-4] === Step 3: single-card queryState=unqueryable on index 2 (${CARD2_ID}) ==="
SPEC_STEP3="$(render_spec "    preset: ${PRESET}
    count: 4
    cardOverrides:
      2:
        queryState: unqueryable")"
mock_dm_apply_spec "$NS" "$MOCK_DM_CM" "$MOCK_DM_KEY" "$SPEC_STEP3"

step3_jq="(([.spec.groups[] | select(.manufacturer == \"${MFR}\") | .accelerators[]] | length) == 4)
  and ([.spec.groups[] | select(.manufacturer == \"${MFR}\") | .accelerators[] | {index, id, u: (.status.unhealthy // false)}]
       == [
         {index: 0, id: \"${CARD0_ID}\", u: false},
         {index: 1, id: \"${CARD1_ID}\", u: false},
         {index: 2, id: \"${CARD2_ID}\", u: true},
         {index: 3, id: \"${CARD3_ID}\", u: false}
       ])"

if mock_dm_wait_devices "$NS" "$TARGET_NODE" "$step3_jq" 90; then
  record PASS "unqueryable card retained in place as unhealthy; sibling indexes preserved" \
    "index 2 (${CARD2_ID}) status.unhealthy=true; siblings kept exact indexes 0, 1, 3"
else
  cur_state="$(kubectl get devices.worker.gpustack.ai "$TARGET_NODE" -o json 2>/dev/null \
    | jq -c --arg m "$MFR" '[.spec.groups[] | select(.manufacturer == $m) | .accelerators[] | {index, id, u: .status.unhealthy}]' 2>/dev/null)"
  record FAIL "unqueryable card retained in place as unhealthy; sibling indexes preserved" \
    "got ${cur_state:-<unreadable>}"
fi

# ===================================================================================================
# Step 4 — All-cards unqueryable & totalFailure=true -> Detector preserves lastDetected (not wiped)
# ===================================================================================================
echo
echo "[case-4] === Step 4: all-cards unqueryable & totalFailure=true preserve lastDetected ==="
SPEC_STEP4A="$(render_spec "    preset: ${PRESET}
    count: 4
    cardOverrides:
      0: { queryState: unqueryable }
      1: { queryState: unqueryable }
      2: { queryState: unqueryable }
      3: { queryState: unqueryable }")"
mock_dm_apply_spec "$NS" "$MOCK_DM_CM" "$MOCK_DM_KEY" "$SPEC_STEP4A"
sleep 25

SPEC_STEP4B="$(render_spec "    preset: ${PRESET}
    count: 4
    fault:
      totalFailure: true")"
mock_dm_apply_spec "$NS" "$MOCK_DM_CM" "$MOCK_DM_KEY" "$SPEC_STEP4B"
sleep 25

preserved_cnt="$(kubectl get devices.worker.gpustack.ai "$TARGET_NODE" -o json 2>/dev/null \
  | jq -r --arg m "$MFR" '[.spec.groups[] | select(.manufacturer == $m) | .accelerators[]] | length' 2>/dev/null)"
if [ "${preserved_cnt:-0}" -eq 4 ]; then
  record PASS "all-cards unqueryable and totalFailure preserve lastDetected" \
    "${TARGET_NODE} Devices retained all 4 cards across total query failures"
else
  record FAIL "all-cards unqueryable and totalFailure preserve lastDetected" \
    "expected 4 retained cards, got '${preserved_cnt:-0}'"
fi

# ===================================================================================================
# Step 5 — Recovery: queryState=ok, unhealthy=false, totalFailure=false -> clears unhealthy & restores capacity
# ===================================================================================================
echo
echo "[case-4] === Step 5: recovery to 4 healthy cards ==="
mock_dm_apply_spec "$NS" "$MOCK_DM_CM" "$MOCK_DM_KEY" "$SPEC_STEP1"

if mock_dm_wait_devices "$NS" "$TARGET_NODE" "$step1_jq" 90 \
  && mock_dm_wait_node_allocatable "$TARGET_NODE" ".[\"${EXCL_RES}\"] == \"4\"" 90; then
  record PASS "recovery clears status.unhealthy and restores full capacity" \
    "${TARGET_NODE}: all 4 cards healthy again, ${EXCL_RES} allocatable=4"
else
  record FAIL "recovery clears status.unhealthy and restores full capacity" \
    "${TARGET_NODE} did not recover all 4 healthy cards within 90s"
fi

# ===================================================================================================
# Step 6 — Clean shrink (count=2): removed slots pruned cleanly with zero ghost unhealthy entries
# ===================================================================================================
echo
echo "[case-4] === Step 6: clean shrink to count=2 ==="
SPEC_STEP6="$(render_spec "    preset: ${PRESET}
    count: 2")"
mock_dm_apply_spec "$NS" "$MOCK_DM_CM" "$MOCK_DM_KEY" "$SPEC_STEP6"

step6_jq="(([.spec.groups[] | select(.manufacturer == \"${MFR}\") | .accelerators[]] | length) == 2)
  and ([.spec.groups[] | select(.manufacturer == \"${MFR}\") | .accelerators[] | {index, id, u: (.status.unhealthy // false)}]
       == [
         {index: 0, id: \"${CARD0_ID}\", u: false},
         {index: 1, id: \"${CARD1_ID}\", u: false}
       ])"

if mock_dm_wait_devices "$NS" "$TARGET_NODE" "$step6_jq" 90 \
  && mock_dm_wait_node_allocatable "$TARGET_NODE" ".[\"${EXCL_RES}\"] == \"2\"" 90; then
  record PASS "clean shrink prunes removed slots without ghost unhealthy entries" \
    "${TARGET_NODE}: shrunk cleanly to index [0, 1], ${EXCL_RES} allocatable=2"
else
  cur6="$(kubectl get devices.worker.gpustack.ai "$TARGET_NODE" -o json 2>/dev/null \
    | jq -c --arg m "$MFR" '[.spec.groups[] | select(.manufacturer == $m) | .accelerators[] | {index, id, u: .status.unhealthy}]' 2>/dev/null)"
  record FAIL "clean shrink prunes removed slots without ghost unhealthy entries" \
    "got ${cur6:-<unreadable>}"
fi

echo
echo "== CASE 4 — Dynamic Mock fault injection, unhealthy retention, recovery & shrink =="
print_rows
if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s)."
  exit 1
fi
echo "CASE 4 PASS"
