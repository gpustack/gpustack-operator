#!/usr/bin/env bash
#
# CHAOS 2 — Dynamic card degradation, loss & recovery chaos under active scheduling load:
#           single-card unhealthy, single-card unqueryable, totalFailure / all-unqueryable,
#           transient initError, and full recovery
#           (MUTATING, self-recovering; AUTO-SKIPS when Mock Device Manager is not active)
#
#   chaos-2.sh [NS]
#
# Prerequisites: nodes=1; gpu=any; cluster=any; inputs=CHAOS_CONCURRENCY,CHAOS_TIMEOUT,E2E_MOCK_IMAGE
# Goal:        Verify dynamic hardware fault injection and recovery across CPU-mock worker nodes
#              under active scheduling load via ConfigMap hot-reloads:
#                1. Single-card unhealthy=true under load: marks target card unhealthy, keeps
#                   healthy sibling workloads Running, and steers newly scheduled Pods away from
#                   the unhealthy card;
#                2. Single-card queryState=unqueryable (lost card): retains the lost card in place
#                   with status.unhealthy=true while preserving sibling card index values unchanged;
#                3. Total query failure (all-cards queryState=unqueryable and fault.totalFailure):
#                   Detector preserves lastDetected so card inventory is never wiped from Devices;
#                4. Transient fault.initError (failCount=2): Devices inventory survives transient
#                   driver initialization errors and resumes healthy monitoring automatically;
#                5. Full recovery to queryState=ok / unhealthy=false: clears status.unhealthy
#                   across all mock nodes and restores baseline allocatable and pool capacity.
# Environment: Any Kind/k3s cluster running GPUStack Operator with Mock Device Manager enabled
#              (deviceManager.mock.enabled=true). AUTO-SKIPS cleanly with SKIP rows and exit 0
#              when Mock Device Manager is not active.
# Inputs:      MOCKED per-node fault states via hot-reloaded Mock Device Manager ConfigMap; REAL
#              CPU mock Pods scheduled continuously in an isolated chaos-c2-<ts> namespace.
# Expected:    All five fault/retention/avoidance/recovery assertions record PASS with zero
#              UnexpectedAdmissionError events.
# Cleanup:     EXIT trap deletes all test Pods/Workloads and the isolated chaos-c2-<ts> namespace,
#              and restores the original mock ConfigMap via mock_dm_restore_cm.
set -uo pipefail

CHAOS_ID=2
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
CHAOS_CONCURRENCY="${CHAOS_CONCURRENCY:-4}"
CHAOS_TIMEOUT="${CHAOS_TIMEOUT:-120}"
TEST_NS="chaos-c${CHAOS_ID}-$(date +%s)-$$"

ORIG_CM_FILE="$(mktemp)"
CM_SNAPSHOT_OK=0
NS_CREATED=0

is_pos_int() {
  [[ "${1:-}" =~ ^[1-9][0-9]*$ ]]
}
is_pos_int "$CHAOS_CONCURRENCY" || CHAOS_CONCURRENCY=4
is_pos_int "$CHAOS_TIMEOUT" || CHAOS_TIMEOUT=120

# shellcheck disable=SC2317 # Invoked via EXIT trap.
cleanup() {
  echo
  echo "[chaos-2] cleanup: deleting ${TEST_NS} and restoring original mock ConfigMap"
  if [[ "$NS_CREATED" -eq 1 ]]; then
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
  record SKIP "multi-node baseline & active background load" "$skip_msg"
  record SKIP "single-card unhealthy=true isolation & healthy sibling scheduling" "$skip_msg"
  record SKIP "single-card unqueryable in-place unhealthy retention & sibling index preservation" "$skip_msg"
  record SKIP "totalFailure & all-unqueryable lastDetected retention" "$skip_msg"
  record SKIP "transient fault.initError survival & self-recovery" "$skip_msg"
  record SKIP "full recovery restores healthy cards and pool capacity" "$skip_msg"
  echo
  echo "== CHAOS 2 — SKIPPED (${skip_msg}) =="
  print_rows
  exit 0
fi

PRESET="nvidia-h100"
MFR="nvidia"
EXCL_RES="nvidia.com/gpu"
if ! mock_dm_manufacturer_allowed nvidia; then
  if mock_dm_manufacturer_allowed amd; then
    PRESET="amd-mi308x"; MFR="amd"; EXCL_RES="amd.com/gpu"
  elif mock_dm_manufacturer_allowed ascend; then
    PRESET="ascend-910b2"; MFR="ascend"; EXCL_RES="huawei.com/npu"
  else
    skip_msg="active mock DaemonSet restricts manufacturers (${MOCK_DM_MANUFACTURERS}) outside nvidia/amd/ascend"
    record SKIP "dynamic fault chaos suite" "$skip_msg"
    print_rows
    exit 0
  fi
fi

if mock_dm_snapshot_cm "$NS" "$MOCK_DM_CM" "$ORIG_CM_FILE"; then
  CM_SNAPSHOT_OK=1
else
  echo "[chaos-2] FATAL: failed to snapshot ConfigMap ${NS}/${MOCK_DM_CM}" >&2
  exit 1
fi

NODE_A="${MOCK_DM_NODES[0]}"
NODE_B=""
[[ "${#MOCK_DM_NODES[@]}" -ge 2 ]] && NODE_B="${MOCK_DM_NODES[1]}"
echo "[chaos-2] nodeA=${NODE_A} nodeB=${NODE_B:-<none>} preset=${PRESET} (${MFR}) ns=${TEST_NS}"

kubectl create namespace "$TEST_NS" >/dev/null
NS_CREATED=1

render_chaos2_spec() {
  local node_a_body="$1"
  local node_b_body="${2:-}"
  printf 'default:\n  preset: %s\n  count: 4\nnodes:\n  "%s":\n%s\n' "$PRESET" "$NODE_A" "$node_a_body"
  if [[ -n "$NODE_B" && -n "$node_b_body" ]]; then
    printf '  "%s":\n%s\n' "$NODE_B" "$node_b_body"
  fi
}

submit_load_pod() {
  local name="$1" node="$2" lq="$3" res_json="$4"
  jq -cn \
    --arg name "$name" \
    --arg ns "$TEST_NS" \
    --arg node "$node" \
    --arg lq "$lq" \
    --arg img "$MOCK_IMAGE" \
    --argjson res "$res_json" '
    {
      apiVersion: "v1",
      kind: "Pod",
      metadata: (
        {name: $name, namespace: $ns}
        + (if $lq != "" then {labels: {"kueue.x-k8s.io/queue-name": $lq}} else {} end)
      ),
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

wait_pod_run_ns() {
  local p="$1" timeout_s="${2:-60}" elapsed=0
  while [[ "$elapsed" -lt "$timeout_s" ]]; do
    if [[ "$(kubectl -n "$TEST_NS" get pod "$p" -o jsonpath='{.status.phase}' 2>/dev/null)" == "Running" ]]; then
      return 0
    fi
    sleep 3
    elapsed=$((elapsed + 3))
  done
  return 1
}

read_pod_mock_dev() {
  local p="$1" out=""
  for _ in $(seq 1 10); do
    out="$(kubectl -n "$TEST_NS" exec "$p" -c main -- sh -c "printf '%s' \"\${GPUSTACK_MOCK_ALLOCATED_DEVICES:-}\"" 2>/dev/null || true)"
    [[ -n "$out" ]] && { printf '%s' "$out"; return 0; }
    sleep 2
  done
  printf '%s' "$out"
}

# ===================================================================================================
# Step 1 — Establish healthy 4-card baseline across mock nodes + start active tenant workload
# ===================================================================================================
BASE_BODY="    preset: ${PRESET}
    count: 4"
mock_dm_apply_spec "$NS" "$MOCK_DM_CM" "$MOCK_DM_KEY" "$(render_chaos2_spec "$BASE_BODY" "$BASE_BODY")"

base_jq="(.metadata.annotations[\"device.gpustack.ai/mock\"] == \"true\")
  and (([.spec.groups[] | select(.manufacturer == \"${MFR}\") | .accelerators[]] | length) == 4)
  and (all(.spec.groups[] | select(.manufacturer == \"${MFR}\") | .accelerators[]; (.status.unhealthy // false) == false))"

base_ok=1
mock_dm_wait_devices "$NS" "$NODE_A" "$base_jq" "$CHAOS_TIMEOUT" || base_ok=0
mock_dm_wait_node_allocatable "$NODE_A" ".[\"${EXCL_RES}\"] == \"4\"" "$CHAOS_TIMEOUT" || base_ok=0
if [[ -n "$NODE_B" ]]; then
  mock_dm_wait_devices "$NS" "$NODE_B" "$base_jq" "$CHAOS_TIMEOUT" || base_ok=0
  mock_dm_wait_node_allocatable "$NODE_B" ".[\"${EXCL_RES}\"] == \"4\"" "$CHAOS_TIMEOUT" || base_ok=0
fi

DEVS_A_JSON="$(kubectl get devices.worker.gpustack.ai "$NODE_A" -o json 2>/dev/null)"
CARD0_ID="$(printf '%s' "$DEVS_A_JSON" | jq -r --arg m "$MFR" '[.spec.groups[] | select(.manufacturer == $m) | .accelerators[] | select(.index == 0) | .id][0] // ""')"
CARD1_ID="$(printf '%s' "$DEVS_A_JSON" | jq -r --arg m "$MFR" '[.spec.groups[] | select(.manufacturer == $m) | .accelerators[] | select(.index == 1) | .id][0] // ""')"
CARD2_ID="$(printf '%s' "$DEVS_A_JSON" | jq -r --arg m "$MFR" '[.spec.groups[] | select(.manufacturer == $m) | .accelerators[] | select(.index == 2) | .id][0] // ""')"
CARD3_ID="$(printf '%s' "$DEVS_A_JSON" | jq -r --arg m "$MFR" '[.spec.groups[] | select(.manufacturer == $m) | .accelerators[] | select(.index == 3) | .id][0] // ""')"
GID_A="$(printf '%s' "$DEVS_A_JSON" | jq -r --arg m "$MFR" '[.spec.groups[] | select(.manufacturer == $m) | .id][0] // ""')"

IT_POOL=""
LQ_POOL=""
for _ in $(seq 1 25); do
  read -r IT_POOL LQ_POOL <<<"$(discover_node_pool "$NODE_A" "$GID_A" --require-entrance --fallback-any name entrance 2>/dev/null || true)"
  if [[ -n "$LQ_POOL" ]] && kubectl -n "$TEST_NS" get localqueue.kueue.x-k8s.io "$LQ_POOL" >/dev/null 2>&1; then
    break
  fi
  sleep 3
done

# Launch steady background shared load across nodes to keep active scheduling pressure throughout fault transitions.
ANCHOR_PODS=()
for ((k = 1; k <= CHAOS_CONCURRENCY; k++)); do
  ap="chaos-c2-anchor-sh-${k}"
  ANCHOR_PODS+=("$ap")
  submit_load_pod "$ap" "" "$LQ_POOL" "{\"${EXCL_RES}.shared\":\"1\"}"
done
anchor_run=0
for ap in "${ANCHOR_PODS[@]}"; do
  wait_pod_run_ns "$ap" 60 && anchor_run=$((anchor_run + 1))
done

if [[ "$base_ok" -eq 1 && "$anchor_run" -eq "${#ANCHOR_PODS[@]}" ]]; then
  record PASS "multi-node baseline & active background load" \
    "4 healthy ${MFR} cards/node; ${anchor_run}/${#ANCHOR_PODS[@]} shared load Pods Running"
else
  record FAIL "multi-node baseline & active background load" \
    "base_ok=${base_ok} anchorRunning=${anchor_run}/${#ANCHOR_PODS[@]}"
fi

# Delete shared anchors on NODE_A before exclusive avoidance check so NODE_A cards are free for whole-card claims,
# while keeping active shared load on NODE_B when multi-node is available.
kubectl -n "$TEST_NS" delete pods --all --ignore-not-found --force --grace-period=0 >/dev/null 2>&1 || true
kubectl -n "$TEST_NS" delete workloads.kueue.x-k8s.io --all --ignore-not-found --wait=false >/dev/null 2>&1 || true
sleep 5
if [[ -n "$NODE_B" ]]; then
  for k in 1 2; do
    submit_load_pod "chaos-c2-nodeb-load-${k}" "$NODE_B" "$LQ_POOL" "{\"${EXCL_RES}.shared\":\"1\"}"
  done
fi

# ===================================================================================================
# Step 2 — Single-card unhealthy: true on index 1 -> scheduler avoids card 1, siblings schedule
# ===================================================================================================
STEP2_A="    preset: ${PRESET}
    count: 4
    cardOverrides:
      1:
        unhealthy: true"
mock_dm_apply_spec "$NS" "$MOCK_DM_CM" "$MOCK_DM_KEY" "$(render_chaos2_spec "$STEP2_A" "$BASE_BODY")"

step2_jq="([.spec.groups[] | select(.manufacturer == \"${MFR}\") | .accelerators[] | select(.index == 1 and .status.unhealthy == true)] | length == 1)
  and ([.spec.groups[] | select(.manufacturer == \"${MFR}\") | .accelerators[] | select(.index != 1 and (.status.unhealthy // false) == false)] | length == 3)"

avoid_ok=1
if mock_dm_wait_devices "$NS" "$NODE_A" "$step2_jq" "$CHAOS_TIMEOUT" \
  && mock_dm_wait_node_allocatable "$NODE_A" ".[\"${EXCL_RES}\"] == \"3\"" "$CHAOS_TIMEOUT"; then
  # Concurrently submit 3 exclusive Pods pinned to NODE_A; all 3 must land on {0, 2, 3}, never CARD1_ID.
  for idx in 1 2 3; do
    submit_load_pod "chaos-c2-ex-${idx}" "$NODE_A" "$LQ_POOL" "{\"${EXCL_RES}\":\"1\"}" &
  done
  wait
  used_cards=""
  for idx in 1 2 3; do
    if ! wait_pod_run_ns "chaos-c2-ex-${idx}" 60; then
      avoid_ok=0
      break
    fi
    cdev="$(read_pod_mock_dev "chaos-c2-ex-${idx}")"
    if [[ -z "$cdev" || "$cdev" == "$CARD1_ID" ]]; then
      avoid_ok=0
      break
    fi
    used_cards="${used_cards}${cdev} "
  done
else
  avoid_ok=0
fi

if [[ "$avoid_ok" -eq 1 ]]; then
  record PASS "single-card unhealthy=true isolation & healthy sibling scheduling" \
    "card 1 (${CARD1_ID}) unhealthy; 3 concurrent Pods scheduled on [${used_cards% }]"
else
  record FAIL "single-card unhealthy=true isolation & healthy sibling scheduling" \
    "${NODE_A} failed unhealthy=true isolation or sibling placement"
fi

# Keep chaos-c2-ex-1 Running on NODE_A during remaining fault injections so faults happen under live node load!
for idx in 2 3; do
  kubectl -n "$TEST_NS" delete pod "chaos-c2-ex-${idx}" --ignore-not-found --force --grace-period=0 >/dev/null 2>&1 || true
done

# ===================================================================================================
# Step 3 — Single-card queryState: unqueryable on index 2 -> retained in place, sibling indexes unchanged
# ===================================================================================================
STEP3_A="    preset: ${PRESET}
    count: 4
    cardOverrides:
      2:
        queryState: unqueryable"
mock_dm_apply_spec "$NS" "$MOCK_DM_CM" "$MOCK_DM_KEY" "$(render_chaos2_spec "$STEP3_A" "$BASE_BODY")"

step3_jq="(([.spec.groups[] | select(.manufacturer == \"${MFR}\") | .accelerators[]] | length) == 4)
  and ([.spec.groups[] | select(.manufacturer == \"${MFR}\") | .accelerators[] | {index, id, u: (.status.unhealthy // false)}]
       == [
         {index: 0, id: \"${CARD0_ID}\", u: false},
         {index: 1, id: \"${CARD1_ID}\", u: false},
         {index: 2, id: \"${CARD2_ID}\", u: true},
         {index: 3, id: \"${CARD3_ID}\", u: false}
       ])"

if mock_dm_wait_devices "$NS" "$NODE_A" "$step3_jq" "$CHAOS_TIMEOUT"; then
  record PASS "single-card unqueryable in-place unhealthy retention & sibling index preservation" \
    "index 2 (${CARD2_ID}) retained with status.unhealthy=true; siblings kept indexes 0, 1, 3"
else
  cur3="$(kubectl get devices.worker.gpustack.ai "$NODE_A" -o json 2>/dev/null \
    | jq -c --arg m "$MFR" '[.spec.groups[] | select(.manufacturer == $m) | .accelerators[] | {index, id, u: .status.unhealthy}]' 2>/dev/null)"
  record FAIL "single-card unqueryable in-place unhealthy retention & sibling index preservation" \
    "got ${cur3:-<unreadable>}"
fi

# ===================================================================================================
# Step 4 — Total failure (fault.totalFailure=true & all-cards unqueryable) preserves lastDetected
# ===================================================================================================
STEP4_TOTAL="    preset: ${PRESET}
    count: 4
    fault:
      totalFailure: true"
STEP4_ALL_UNQ="    preset: ${PRESET}
    count: 4
    cardOverrides:
      0: { queryState: unqueryable }
      1: { queryState: unqueryable }
      2: { queryState: unqueryable }
      3: { queryState: unqueryable }"

if [[ -n "$NODE_B" ]]; then
  mock_dm_apply_spec "$NS" "$MOCK_DM_CM" "$MOCK_DM_KEY" "$(render_chaos2_spec "$STEP4_TOTAL" "$STEP4_ALL_UNQ")"
  sleep 25
  cnt_a="$(kubectl get devices.worker.gpustack.ai "$NODE_A" -o json 2>/dev/null \
    | jq -r --arg m "$MFR" '[.spec.groups[] | select(.manufacturer == $m) | .accelerators[]] | length' 2>/dev/null || echo 0)"
  cnt_b="$(kubectl get devices.worker.gpustack.ai "$NODE_B" -o json 2>/dev/null \
    | jq -r --arg m "$MFR" '[.spec.groups[] | select(.manufacturer == $m) | .accelerators[]] | length' 2>/dev/null || echo 0)"
  if [[ "${cnt_a:-0}" -eq 4 && "${cnt_b:-0}" -eq 4 ]]; then
    record PASS "totalFailure & all-unqueryable lastDetected retention" \
      "${NODE_A} (totalFailure) and ${NODE_B} (all-unqueryable) both retained 4 cards"
  else
    record FAIL "totalFailure & all-unqueryable lastDetected retention" \
      "cntA=${cnt_a} cntB=${cnt_b} (expected 4 and 4)"
  fi
else
  mock_dm_apply_spec "$NS" "$MOCK_DM_CM" "$MOCK_DM_KEY" "$(render_chaos2_spec "$STEP4_ALL_UNQ")"
  sleep 20
  mock_dm_apply_spec "$NS" "$MOCK_DM_CM" "$MOCK_DM_KEY" "$(render_chaos2_spec "$STEP4_TOTAL")"
  sleep 20
  cnt_a="$(kubectl get devices.worker.gpustack.ai "$NODE_A" -o json 2>/dev/null \
    | jq -r --arg m "$MFR" '[.spec.groups[] | select(.manufacturer == $m) | .accelerators[]] | length' 2>/dev/null || echo 0)"
  if [[ "${cnt_a:-0}" -eq 4 ]]; then
    record PASS "totalFailure & all-unqueryable lastDetected retention" \
      "${NODE_A} retained all 4 cards across all-unqueryable and totalFailure"
  else
    record FAIL "totalFailure & all-unqueryable lastDetected retention" \
      "expected 4 retained cards on ${NODE_A}, got '${cnt_a:-0}'"
  fi
fi

# ===================================================================================================
# Step 5 — Transient fault.initError (failCount=2) + card health recovery transition
# ===================================================================================================
# Note: in Step 3 card 2 on NODE_A was retained as unhealthy=true. Applying transient initError
# (failCount=2) with all cards healthy means the first 2 monitor ticks fail with initError (preserving
# Devices) and tick 3+ succeeds, flipping card 2 back to healthy=false automatically!
STEP5_INIT="    preset: ${PRESET}
    count: 4
    fault:
      initError:
        enabled: true
        failCount: 2
        message: \"chaos-2 transient driver init failure\""
mock_dm_apply_spec "$NS" "$MOCK_DM_CM" "$MOCK_DM_KEY" "$(render_chaos2_spec "$STEP5_INIT" "$BASE_BODY")"

if mock_dm_wait_devices "$NS" "$NODE_A" "$base_jq" "$CHAOS_TIMEOUT"; then
  record PASS "transient fault.initError survival & self-recovery" \
    "${NODE_A} survived failCount=2 initError without losing cards and converged to 4 healthy cards"
else
  record FAIL "transient fault.initError survival & self-recovery" \
    "${NODE_A} did not self-recover after transient failCount=2 initError"
fi

# ===================================================================================================
# Step 6 — Full clean recovery across all nodes and active load verification
# ===================================================================================================
mock_dm_apply_spec "$NS" "$MOCK_DM_CM" "$MOCK_DM_KEY" "$(render_chaos2_spec "$BASE_BODY" "$BASE_BODY")"

rec_ok=1
mock_dm_wait_devices "$NS" "$NODE_A" "$base_jq" "$CHAOS_TIMEOUT" || rec_ok=0
mock_dm_wait_node_allocatable "$NODE_A" ".[\"${EXCL_RES}\"] == \"4\"" "$CHAOS_TIMEOUT" || rec_ok=0
if [[ -n "$NODE_B" ]]; then
  mock_dm_wait_devices "$NS" "$NODE_B" "$base_jq" "$CHAOS_TIMEOUT" || rec_ok=0
  mock_dm_wait_node_allocatable "$NODE_B" ".[\"${EXCL_RES}\"] == \"4\"" "$CHAOS_TIMEOUT" || rec_ok=0
fi
if [[ -n "$IT_POOL" ]]; then
  mock_dm_wait_it ".items[] | select(.metadata.name == \"${IT_POOL}\" and ((.status.accelerator.capacity // \"0\") | tostring | tonumber) >= 4)" 60 || rec_ok=0
fi

ex1_alive="$(kubectl -n "$TEST_NS" get pod chaos-c2-ex-1 -o jsonpath='{.status.phase}' 2>/dev/null || true)"
uae_c2="$(kubectl -n "$TEST_NS" get events \
  -o jsonpath='{range .items[*]}{.reason}{"\n"}{end}' 2>/dev/null \
  | grep -c 'UnexpectedAdmissionError' || true)"

if [[ "$rec_ok" -eq 1 && "$ex1_alive" == "Running" && "${uae_c2:-0}" -eq 0 ]]; then
  record PASS "full recovery restores healthy cards and pool capacity" \
    "all mock nodes back to 4 healthy cards, in-flight Pod stayed Running, 0 UAE"
else
  record FAIL "full recovery restores healthy cards and pool capacity" \
    "rec_ok=${rec_ok} inFlightPod='${ex1_alive}' uae=${uae_c2}"
fi

echo
echo "== CHAOS 2 — Dynamic card degradation, loss & recovery chaos =="
print_rows
if [[ "$FAILS" -ne 0 ]]; then
  echo "[chaos-2] ${FAILS} check(s) FAILED"
  exit 1
fi
echo "CHAOS 2 PASS"
