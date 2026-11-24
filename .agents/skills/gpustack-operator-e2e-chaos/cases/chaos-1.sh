#!/usr/bin/env bash
#
# CHAOS 1 — Scale heterogeneous scheduling & Kueue admission storm: concurrent mixed EX/SH/SL/PT
#           CPU mock bursts plus multi-role P/D and 3-role EPD ModelDeployments across mock nodes
#           (MUTATING, self-recovering; AUTO-SKIPS when Mock Device Manager is not active)
#
#   chaos-1.sh [NS]
#
# Prerequisites: nodes=2; gpu=any; cluster=any; inputs=CHAOS_CONCURRENCY,CHAOS_BURST_SIZE,CHAOS_TIMEOUT,E2E_MOCK_IMAGE
# Goal:        Drive a high-concurrency heterogeneous scheduling and Kueue admission storm across
#              all active CPU-mock worker nodes (scaling naturally from 2-4 local Kind nodes to
#              8-32+ remote Kind/k3s nodes) and verify:
#                1. Concurrent mixed exclusive, shared, sliced (25% & 50%), partitioned (virtual
#                   MIG), multi-role Prefill/Decode, and 3-role Encode/Prefill/Decode (EPD)
#                   ModelDeployment workloads compose Workloads/PodSets and admit cleanly without
#                   admission livelock;
#                2. Zero double-booking on Devices.status (no card simultaneously claimed in
#                   conflicting modes, no two exclusive Pods sharing a card, and no card exceeding
#                   its 1600000 credit budget);
#                3. Zero negative Remaining credits across Devices.status and all four InstanceType
#                   status views (EX/SH/SL/PT);
#                4. Zero UnexpectedAdmissionError events across the storm namespace;
#                5. Truthful four-view InstanceType accounting before, during, and after teardown,
#                   with full capacity restoration once the storm drains.
# Environment: Any Kind/k3s cluster running GPUStack Operator with Mock Device Manager enabled
#              (deviceManager.mock.enabled=true). AUTO-SKIPS cleanly with SKIP rows and exit 0
#              when Mock Device Manager is not active.
# Inputs:      MOCKED hardware inventory via hot-reloaded Mock Device Manager ConfigMap; REAL
#              Kueue Workloads, Pods, and multi-role P/D + EPD ModelDeployments in an isolated
#              chaos-c1-<ts> namespace. Scales with CHAOS_CONCURRENCY, CHAOS_BURST_SIZE,
#              CHAOS_TIMEOUT, and E2E_MOCK_IMAGE.
# Expected:    All storm invariants (admission progress, zero double-booking, zero negative
#              Remaining, zero UnexpectedAdmissionError, and baseline capacity recovery) PASS.
# Cleanup:     EXIT trap deletes all workloads and the isolated chaos-c1-<ts> namespace, releases
#              Kueue Workloads, and restores the original mock ConfigMap via mock_dm_restore_cm.
set -uo pipefail

CHAOS_ID=1
CHAOS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
E2E_CASES_DIR="$(cd "${CHAOS_DIR}/../../gpustack-operator-e2e/cases" && pwd)"

# shellcheck source=/dev/null
. "${E2E_CASES_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${E2E_CASES_DIR}/_pool-lib.sh"
# shellcheck source=/dev/null
. "${E2E_CASES_DIR}/_mock-dm-lib.sh"
# shellcheck source=/dev/null
. "${E2E_CASES_DIR}/_md-lib.sh"

NS="${1:-gpustack-system}"
# shellcheck disable=SC2034 # Read by _rows-lib.sh setting_* helpers.
SYSTEM_NS="$NS"

MOCK_IMAGE="${E2E_MOCK_IMAGE:-alpine}"
CHAOS_TIMEOUT="${CHAOS_TIMEOUT:-180}"
ANNO="device.gpustack.ai/accelerator.allocated"
TEST_NS="chaos-c${CHAOS_ID}-$(date +%s)-$$"
MD_NAME="chaos-c1-pd"
MD_EPD_NAME="chaos-c1-epd"

ORIG_CM_FILE="$(mktemp)"
CM_SNAPSHOT_OK=0
NS_CREATED=0

is_pos_int() {
  [[ "${1:-}" =~ ^[1-9][0-9]*$ ]]
}

if ! is_pos_int "$CHAOS_TIMEOUT"; then
  CHAOS_TIMEOUT=180
fi

# shellcheck disable=SC2317 # Invoked via EXIT trap.
cleanup() {
  echo
  echo "[chaos-1] cleanup: draining ${TEST_NS} and restoring original mock ConfigMap"
  if [[ "$NS_CREATED" -eq 1 ]]; then
    kubectl -n "$TEST_NS" delete modeldeployments.worker.gpustack.ai "$MD_NAME" "$MD_EPD_NAME" \
      --ignore-not-found --wait=false >/dev/null 2>&1 || true
    md_force_release "$TEST_NS" "$MD_NAME" >/dev/null 2>&1 || true
    md_force_release "$TEST_NS" "$MD_EPD_NAME" >/dev/null 2>&1 || true
    kubectl -n "$TEST_NS" delete pods --all --ignore-not-found --force --grace-period=0 >/dev/null 2>&1 || true
    kubectl -n "$TEST_NS" delete workloads.kueue.x-k8s.io --all --ignore-not-found --wait=false >/dev/null 2>&1 || true
    kubectl delete namespace "$TEST_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    local i
    for ((i = 0; i < 20; i++)); do
      if ! kubectl get namespace "$TEST_NS" >/dev/null 2>&1; then
        break
      fi
      sleep 2
    done
  fi
  if [[ "$CM_SNAPSHOT_OK" -eq 1 ]]; then
    mock_dm_restore_cm "$NS" "$MOCK_DM_CM" "$ORIG_CM_FILE" || true
  fi
  rm -f "$ORIG_CM_FILE"
}
trap cleanup EXIT

if ! mock_dm_detect_env "$NS"; then
  skip_msg="mock device-manager DaemonSet (gpustack.ai/manufacturer=mock) is not active on this cluster"
  record SKIP "heterogeneous multi-node mock storm topology" "$skip_msg"
  record SKIP "concurrent mixed EX/SH/SL/PT & P/D burst admission" "$skip_msg"
  record SKIP "three-role EPD ModelDeployment Workload/PodSet admission" "$skip_msg"
  record SKIP "zero double-booking and credit budget bounds on Devices.status" "$skip_msg"
  record SKIP "zero negative Remaining credits across Devices and InstanceTypes" "$skip_msg"
  record SKIP "zero UnexpectedAdmissionError events during storm" "$skip_msg"
  record SKIP "four-view InstanceType accounting and post-teardown capacity recovery" "$skip_msg"
  echo
  echo "== CHAOS 1 — SKIPPED (${skip_msg}) =="
  print_rows
  exit 0
fi

HAS_NV=0; mock_dm_manufacturer_allowed nvidia && HAS_NV=1
HAS_AMD=0; mock_dm_manufacturer_allowed amd && HAS_AMD=1
HAS_ASCEND=0; mock_dm_manufacturer_allowed ascend && HAS_ASCEND=1

if [[ "$HAS_NV" -eq 0 && "$HAS_AMD" -eq 0 && "$HAS_ASCEND" -eq 0 ]]; then
  skip_msg="active mock DaemonSet restricts manufacturers (${MOCK_DM_MANUFACTURERS}) outside nvidia/amd/ascend"
  record SKIP "heterogeneous multi-node mock storm topology" "$skip_msg"
  print_rows
  exit 0
fi

if mock_dm_snapshot_cm "$NS" "$MOCK_DM_CM" "$ORIG_CM_FILE"; then
  CM_SNAPSHOT_OK=1
else
  echo "[chaos-1] FATAL: failed to snapshot ConfigMap ${NS}/${MOCK_DM_CM}" >&2
  exit 1
fi

NODE_COUNT="${#MOCK_DM_NODES[@]}"
DEFAULT_CONCURRENCY=$(( NODE_COUNT * 3 ))
[[ "$DEFAULT_CONCURRENCY" -lt 6 ]] && DEFAULT_CONCURRENCY=6
[[ "$DEFAULT_CONCURRENCY" -gt 24 ]] && DEFAULT_CONCURRENCY=24
CHAOS_CONCURRENCY="${CHAOS_CONCURRENCY:-$DEFAULT_CONCURRENCY}"
is_pos_int "$CHAOS_CONCURRENCY" || CHAOS_CONCURRENCY="$DEFAULT_CONCURRENCY"

DEFAULT_BURST=$(( NODE_COUNT * 4 ))
[[ "$DEFAULT_BURST" -lt 8 ]] && DEFAULT_BURST=8
[[ "$DEFAULT_BURST" -gt 128 ]] && DEFAULT_BURST=128
CHAOS_BURST_SIZE="${CHAOS_BURST_SIZE:-$DEFAULT_BURST}"
is_pos_int "$CHAOS_BURST_SIZE" || CHAOS_BURST_SIZE="$DEFAULT_BURST"

echo "[chaos-1] nodes=${NODE_COUNT} (${MOCK_DM_NODE_LIST}) concurrency=${CHAOS_CONCURRENCY} burst=${CHAOS_BURST_SIZE} ns=${TEST_NS}"

kubectl create namespace "$TEST_NS" >/dev/null
NS_CREATED=1

# ===================================================================================================
# Step 1 — Configure heterogeneous multi-node mock hardware across all discovered worker nodes
# ===================================================================================================
PRIMARY_PRESET="nvidia-h100"
PRIMARY_MFR="nvidia"
PRIMARY_RES="nvidia.com/gpu"
if [[ "$HAS_NV" -eq 0 ]]; then
  if [[ "$HAS_AMD" -eq 1 ]]; then
    PRIMARY_PRESET="amd-mi308x"; PRIMARY_MFR="amd"; PRIMARY_RES="amd.com/gpu"
  else
    PRIMARY_PRESET="ascend-910b2"; PRIMARY_MFR="ascend"; PRIMARY_RES="huawei.com/npu"
  fi
fi

build_storm_spec() {
  printf 'default:\n  preset: %s\n  count: 4\nnodes:\n' "$PRIMARY_PRESET"
  local idx node
  for ((idx = 0; idx < NODE_COUNT; idx++)); do
    node="${MOCK_DM_NODES[$idx]}"
    printf '  "%s":\n' "$node"
    if [[ "$NODE_COUNT" -eq 1 && "$HAS_NV" -eq 1 ]]; then
      # On a 1-node cluster, include 11 logical H100 cards + 1 virtual MIG H100 card (distinct id for _DeviceKey)
      # so P/D (2), EPD (3), and mixed EX/SH/SL/PT burst Pods fit concurrently without oversubscription.
      cat <<'EOF'
    preset: nvidia-h100
    count: 12
    cardOverrides:
      11:
        id: GPU-c1-mig-912151ec-e9f1-fc26
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
EOF
    else
      case $((idx % 4)) in
        0)
          printf '    preset: %s\n    count: 8\n' "$PRIMARY_PRESET"
          ;;
        1)
          if [[ "$HAS_NV" -eq 1 ]]; then
            printf '    preset: nvidia-h100-mig\n    count: 2\n'
          elif [[ "$HAS_AMD" -eq 1 ]]; then
            printf '    preset: amd-mi308x\n    count: 4\n'
          else
            printf '    preset: %s\n    count: 4\n' "$PRIMARY_PRESET"
          fi
          ;;
        2)
          if [[ "$HAS_AMD" -eq 1 ]]; then
            printf '    preset: amd-mi308x\n    count: 4\n'
          elif [[ "$HAS_NV" -eq 1 ]]; then
            printf '    preset: nvidia-h100\n    count: 4\n'
          else
            printf '    preset: %s\n    count: 4\n' "$PRIMARY_PRESET"
          fi
          ;;
        3)
          if [[ "$HAS_ASCEND" -eq 1 ]]; then
            printf '    preset: ascend-910b2\n    count: 4\n'
          elif [[ "$HAS_NV" -eq 1 ]]; then
            printf '    preset: nvidia-h100\n    count: 4\n'
          else
            printf '    preset: %s\n    count: 4\n' "$PRIMARY_PRESET"
          fi
          ;;
      esac
    fi
  done
}

STORM_SPEC="$(build_storm_spec)"
mock_dm_apply_spec "$NS" "$MOCK_DM_CM" "$MOCK_DM_KEY" "$STORM_SPEC"

topo_ok=1
for ((idx = 0; idx < NODE_COUNT; idx++)); do
  node="${MOCK_DM_NODES[$idx]}"
  if ! mock_dm_wait_devices "$NS" "$node" \
    '(.metadata.annotations["device.gpustack.ai/mock"] == "true") and (([.spec.groups[].accelerators[]] | length) >= 2)' 90; then
    topo_ok=0
    break
  fi
done

NODE0="${MOCK_DM_NODES[0]}"
mock_dm_wait_node_allocatable "$NODE0" "((.[\"${PRIMARY_RES}\"] // \"0\" | tonumber) >= 2)" 90 || true

PRIMARY_GID="$(kubectl get devices.worker.gpustack.ai "$NODE0" -o json 2>/dev/null \
  | jq -r --arg m "$PRIMARY_MFR" '[.spec.groups[] | select(.manufacturer == $m) | .id][0] // ""')"

IT_PRIMARY=""
LQ_PRIMARY=""
for _ in $(seq 1 30); do
  read -r IT_PRIMARY LQ_PRIMARY <<<"$(discover_node_pool "$NODE0" "$PRIMARY_GID" --require-entrance --fallback-any name entrance 2>/dev/null || true)"
  if [[ -n "$IT_PRIMARY" && -n "$LQ_PRIMARY" ]] \
    && kubectl -n "$TEST_NS" get localqueue.kueue.x-k8s.io "$LQ_PRIMARY" >/dev/null 2>&1; then
    break
  fi
  sleep 3
done

# Discover optional MIG pool (either on node 1 or mixed on node 0).
MIG_NODE=""
IT_MIG=""
LQ_MIG=""
if [[ "$HAS_NV" -eq 1 ]]; then
  for cand_node in "${MOCK_DM_NODES[@]}"; do
    has_mig="$(kubectl get devices.worker.gpustack.ai "$cand_node" -o json 2>/dev/null \
      | jq -r '[.spec.groups[] | select(.manufacturer == "nvidia") | .accelerators[] | select((.status.physicalSliced.profiles // []) | length > 0)] | length' 2>/dev/null || echo 0)"
    if [[ "${has_mig:-0}" -gt 0 ]]; then
      MIG_NODE="$cand_node"
      mig_gid="$(kubectl get devices.worker.gpustack.ai "$cand_node" -o json 2>/dev/null \
        | jq -r '[.spec.groups[] | select(.manufacturer == "nvidia") | .id][0] // ""')"
      read -r IT_MIG LQ_MIG <<<"$(discover_node_pool "$cand_node" "$mig_gid" --require-entrance --fallback-any name entrance 2>/dev/null || true)"
      break
    fi
  done
fi

# Discover optional secondary vendor pool (AMD or Ascend) for cross-pool diversity.
IT_SEC=""
LQ_SEC=""
SEC_RES=""
for ((idx = 0; idx < NODE_COUNT; idx++)); do
  cand="${MOCK_DM_NODES[$idx]}"
  sec_info="$(kubectl get devices.worker.gpustack.ai "$cand" -o json 2>/dev/null \
    | jq -r --arg pm "$PRIMARY_MFR" '[.spec.groups[] | select(.manufacturer != $pm) | "\(.manufacturer)|\(.id)"][0] // ""')"
  if [[ -n "$sec_info" ]]; then
    sec_mfr="${sec_info%%|*}"
    sec_gid="${sec_info#*|}"
    case "$sec_mfr" in
      amd) SEC_RES="amd.com/gpu" ;;
      ascend) SEC_RES="huawei.com/npu" ;;
      *) SEC_RES="" ;;
    esac
    if [[ -n "$SEC_RES" ]]; then
      read -r IT_SEC LQ_SEC <<<"$(discover_node_pool "$cand" "$sec_gid" --require-entrance name entrance 2>/dev/null || true)"
      [[ -n "$LQ_SEC" ]] && break
    fi
  fi
done
: "$IT_MIG" "$IT_SEC"

if [[ "$topo_ok" -eq 1 && -n "$IT_PRIMARY" && -n "$LQ_PRIMARY" ]]; then
  record PASS "heterogeneous multi-node mock storm topology" \
    "${NODE_COUNT} mock node(s) converged; primary=${IT_PRIMARY} migNode=${MIG_NODE:-<none>} secQueue=${LQ_SEC:-<none>}"
else
  record FAIL "heterogeneous multi-node mock storm topology" \
    "topo_ok=${topo_ok} primary='${IT_PRIMARY:-<missing>}' lq='${LQ_PRIMARY:-<missing>}'"
  print_rows
  exit 1
fi

# Wait for baseline InstanceType capacities to settle and snapshot baseline four-view values.
sleep 5
JQ_QNUM='def qnum:
  if . == null then 0
  elif type == "number" then .
  elif test("^-?[0-9]+(\\.[0-9]+)?k$") then (sub("k$";"") | tonumber * 1000)
  elif test("^-?[0-9]+(\\.[0-9]+)?M$") then (sub("M$";"") | tonumber * 1000000)
  elif test("^-?[0-9]+(\\.[0-9]+)?m$") then (sub("m$";"") | tonumber / 1000)
  else (tonumber? // 0) end;'

BASE_ITS_SNAP="$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null | jq -c "${JQ_QNUM}"'
  [.items[] | select(.spec.acceleratable == true and (.status.phase // "") == "Active") | {
    name: .metadata.name,
    exCap: (.status.accelerator.capacity | qnum),
    exRem: (.status.accelerator.remaining | qnum),
    shCap: (.status.acceleratorShared.capacity | qnum),
    shRem: (.status.acceleratorShared.remaining | qnum),
    slCap: (.status.acceleratorSliced.capacity | qnum),
    slRem: (.status.acceleratorSliced.remaining | qnum),
    ptCap: (.status.acceleratorPartitioned.capacity | qnum),
    ptRem: (.status.acceleratorPartitioned.remaining | qnum)
  }]
')"

# ===================================================================================================
# Step 2 — Submit concurrent mixed EX / SH / SL / PT Pod burst + multi-role P/D ModelDeployment
# ===================================================================================================
submit_storm_pod() {
  local name="$1" lq="$2" res_json="$3"
  jq -cn \
    --arg name "$name" \
    --arg ns "$TEST_NS" \
    --arg lq "$lq" \
    --arg img "$MOCK_IMAGE" \
    --argjson res "$res_json" '
    {
      apiVersion: "v1",
      kind: "Pod",
      metadata: {
        name: $name,
        namespace: $ns,
        labels: {
          "app.kubernetes.io/part-of": "chaos-1-storm",
          "kueue.x-k8s.io/queue-name": $lq
        }
      },
      spec: {
        schedulerName: "default-scheduler",
        restartPolicy: "Never",
        containers: [{
          name: "main",
          image: $img,
          command: ["sleep", "86400"],
          resources: {limits: $res, requests: $res}
        }]
      }
    }' | kubectl apply -f - >/dev/null
}

# Submit multi-role P/D and 3-role EPD ModelDeployments when a usable AcceleratorType exists for PRIMARY_MFR.
AT_PD="$(md_usable_type_for "$PRIMARY_MFR")"
PD_SUBMITTED=0
EPD_SUBMITTED=0
EPD_APPLY_ERR=""
EPD_ENGINE="vLLM"
EPD_ROUTER="vllm-router"
if [[ "$PRIMARY_MFR" == "ascend" ]]; then
  # Ascend vLLM EPD requires a shared RWX PVC; SGLang + llm-d-router runs the network encoder-urls leg without PVC.
  EPD_ENGINE="SGLang"
  EPD_ROUTER="llm-d-router"
fi

if [[ -n "$AT_PD" ]]; then
  if cat <<EOF | kubectl apply -f - >/dev/null 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD_NAME}
  namespace: ${TEST_NS}
spec:
  engine:
    name: vLLM
    image: ${MOCK_IMAGE}
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  roles:
    - name: prefill
      kind: Prefill
      acceleratorType: ${AT_PD}
      replicas: 1
      image: ${MOCK_IMAGE}
      command: ["sleep", "86400"]
    - name: decode
      kind: Decode
      acceleratorType: ${AT_PD}
      replicas: 1
      image: ${MOCK_IMAGE}
      command: ["sleep", "86400"]
EOF
  then
    PD_SUBMITTED=1
  fi

  if epd_out="$(cat <<EOF | kubectl apply -f - 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD_EPD_NAME}
  namespace: ${TEST_NS}
spec:
  engine:
    name: ${EPD_ENGINE}
    image: ${MOCK_IMAGE}
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  router:
    name: ${EPD_ROUTER}
    image: ${MOCK_IMAGE}
  roles:
    - name: encode
      kind: Encode
      acceleratorType: ${AT_PD}
      replicas: 1
      image: ${MOCK_IMAGE}
      command: ["sleep", "86400"]
    - name: prefill
      kind: Prefill
      acceleratorType: ${AT_PD}
      replicas: 1
      image: ${MOCK_IMAGE}
      command: ["sleep", "86400"]
    - name: decode
      kind: Decode
      acceleratorType: ${AT_PD}
      replicas: 1
      image: ${MOCK_IMAGE}
      command: ["sleep", "86400"]
EOF
  )"; then
    EPD_SUBMITTED=1
  else
    EPD_APPLY_ERR="$(printf '%s' "$epd_out" | tr '\n' ' ' | tr -s ' ')"
  fi
fi

# Fan out CHAOS_BURST_SIZE mixed-mode Pods with bounded concurrency CHAOS_CONCURRENCY.
inflight=0
for ((b = 1; b <= CHAOS_BURST_SIZE; b++)); do
  pname="chaos-c1-pod-${b}"
  target_lq="$LQ_PRIMARY"
  res_payload=""

  case $((b % 5)) in
    0)
      if [[ -n "$LQ_SEC" && -n "$SEC_RES" ]]; then
        target_lq="$LQ_SEC"
        res_payload="{\"${SEC_RES}\":\"1\"}"
      else
        res_payload="{\"${PRIMARY_RES}\":\"1\"}"
      fi
      ;;
    1)
      res_payload="{\"${PRIMARY_RES}.shared\":\"1\"}"
      ;;
    2)
      if [[ "$PRIMARY_MFR" == "nvidia" || "$PRIMARY_MFR" == "amd" || "$PRIMARY_MFR" == "ascend" ]]; then
        res_payload="{\"${PRIMARY_RES}.sliced\":\"1\",\"${PRIMARY_RES}.sliced.memory-percentage\":\"25\"}"
      else
        res_payload="{\"${PRIMARY_RES}.shared\":\"1\"}"
      fi
      ;;
    3)
      if [[ "$PRIMARY_MFR" == "nvidia" || "$PRIMARY_MFR" == "amd" || "$PRIMARY_MFR" == "ascend" ]]; then
        res_payload="{\"${PRIMARY_RES}.sliced\":\"1\",\"${PRIMARY_RES}.sliced.memory-percentage\":\"50\"}"
      else
        res_payload="{\"${PRIMARY_RES}\":\"1\"}"
      fi
      ;;
    4)
      if [[ -n "$LQ_MIG" ]]; then
        target_lq="$LQ_MIG"
        if [[ $((b % 2)) -eq 0 ]]; then
          res_payload='{"nvidia.com/gpu.partitioned":"1","nvidia.com/gpu.partitioned.mig-1g.10gb":"1"}'
        else
          res_payload='{"nvidia.com/gpu.partitioned":"1","nvidia.com/gpu.partitioned.mig-2g.20gb":"1"}'
        fi
      else
        res_payload="{\"${PRIMARY_RES}.shared\":\"1\"}"
      fi
      ;;
  esac

  submit_storm_pod "$pname" "$target_lq" "$res_payload" &
  inflight=$((inflight + 1))
  if [[ "$inflight" -ge "$CHAOS_CONCURRENCY" ]]; then
    wait
    inflight=0
  fi
done
wait

# ===================================================================================================
# Step 3 — Sample active scheduling storm and verify invariants across Devices, Pods, InstanceTypes
# ===================================================================================================
WANT_MIN_RUNNING=$(( CHAOS_BURST_SIZE / 2 ))
[[ "$WANT_MIN_RUNNING" -lt 4 ]] && WANT_MIN_RUNNING=4
[[ "$WANT_MIN_RUNNING" -gt 12 ]] && WANT_MIN_RUNNING=12

peak_running=0
pd_running=0
epd_admitted_ok=0
epd_pod_roles=""
epd_wl_podsets=""
epd_admitted_roles=""
double_book_err=""
neg_rem_err=""
saw_four_view_drop=0

deadline=$((SECONDS + CHAOS_TIMEOUT))
while [[ "$SECONDS" -lt "$deadline" ]]; do
  pods_json="$(kubectl -n "$TEST_NS" get pods -o json 2>/dev/null || echo '{"items":[]}')"
  wls_json="$(kubectl -n "$TEST_NS" get workloads.kueue.x-k8s.io -o json 2>/dev/null || echo '{"items":[]}')"
  devs_json="$(kubectl get devices.worker.gpustack.ai -o json 2>/dev/null || echo '{"items":[]}')"
  its_json="$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null || echo '{"items":[]}')"

  cur_running="$(printf '%s' "$pods_json" | jq -r --arg epd "$MD_EPD_NAME" \
    '[.items[] | select((.metadata.labels["app.kubernetes.io/instance"] // "") != $epd and .status.phase == "Running")] | length' 2>/dev/null || echo 0)"
  [[ "${cur_running:-0}" -gt "$peak_running" ]] && peak_running="$cur_running"

  if [[ "$PD_SUBMITTED" -eq 1 ]]; then
    cur_pd="$(printf '%s' "$pods_json" | jq -r --arg md "$MD_NAME" \
      '[.items[] | select(.metadata.labels["app.kubernetes.io/instance"] == $md and .status.phase == "Running")] | length' 2>/dev/null || echo 0)"
    [[ "${cur_pd:-0}" -gt "$pd_running" ]] && pd_running="$cur_pd"
  fi

  if [[ "$EPD_SUBMITTED" -eq 1 && "$epd_admitted_ok" -eq 0 ]]; then
    epd_state="$(jq -rn --arg md "$MD_EPD_NAME" --argjson pods "$pods_json" --argjson wls "$wls_json" '
      [($pods.items // [])[]
        | select(.metadata.labels["app.kubernetes.io/instance"] == $md
            and ((.metadata.labels["app.kubernetes.io/component"] // "") | IN("encode", "prefill", "decode")))] as $epods
      | ($epods | map(.metadata.labels["app.kubernetes.io/component"]) | unique | sort | join(",")) as $roles
      | ($epods | map(.metadata.uid)) as $uids
      | [($wls.items // [])[]
          | select(any((.metadata.ownerReferences // [])[]; .uid as $u | $uids | index($u)))] as $ewls
      | ($ewls | map((.spec.podSets // [])[] | "\(.name)=\(.count)") | sort | join(",")) as $podsets
      | ($ewls | map(select(any((.status.conditions // [])[]; .type == "Admitted" and .status == "True"))
          | (.status.admission.podSetAssignments // [])[] | .name) | sort | join(",")) as $admitted
      | "\($roles)|\($podsets)|\($admitted)"
    ' 2>/dev/null || true)"
    if [[ -n "$epd_state" ]]; then
      IFS='|' read -r epd_pod_roles epd_wl_podsets epd_admitted_roles <<<"$epd_state"
      if [[ "$epd_wl_podsets" == "decode=1,encode=1,prefill=1" && "$epd_admitted_roles" == "decode,encode,prefill" ]]; then
        epd_admitted_ok=1
      fi
    fi
  fi

  # Check Devices.status + Pod allocation annotations for double-booking or over-budget cards.
  if ! cur_db="$(ANNO="$ANNO" PODS_JSON="$pods_json" DEVS_JSON="$devs_json" python3 -c '
import json, os

pods = json.loads(os.environ["PODS_JSON"]).get("items", [])
devs = json.loads(os.environ["DEVS_JSON"]).get("items", [])
anno_key = os.environ["ANNO"]

errs = []
for d in devs:
    node = d.get("metadata", {}).get("name", "")
    spec_part_cards = set()
    for g in d.get("spec", {}).get("groups", []) or []:
        gid = g.get("id", "")
        for a in g.get("accelerators", []) or []:
            ps = (a.get("status") or {}).get("physicalSliced") or {}
            if (ps.get("profiles") or []) or (ps.get("count") or 0) > 0:
                spec_part_cards.add((gid, a.get("id", "")))
    for g in d.get("status", {}).get("groups", []) or []:
        gid = g.get("id", "")
        for a in g.get("accelerators", []) or []:
            cid = a.get("id", "")
            mode = (a.get("mode") or "")
            alloc = a.get("allocated", 0) or 0
            if mode in ("Exclusive", "Shared", "Sliced") and alloc > 1600000:
                errs.append(f"{node}/{cid}: allocated={alloc}>1600000 in {mode}")
            if (gid, cid) in spec_part_cards and mode in ("Exclusive", "Shared", "Sliced"):
                errs.append(f"{node}/{cid}: partitioned card claimed in {mode}")

# Cross-check live Running/Pending allocated annotations across Pods.
card_modes = {}
excl_owners = {}
for p in pods:
    if p.get("metadata", {}).get("deletionTimestamp"):
        continue
    if (p.get("status") or {}).get("phase") in ("Succeeded", "Failed"):
        continue
    node = (p.get("spec") or {}).get("nodeName") or ""
    raw = ((p.get("metadata") or {}).get("annotations") or {}).get(anno_key, "")
    if not node or not raw:
        continue
    try:
        parsed = json.loads(raw)
    except Exception:
        continue
    pname = p["metadata"]["name"]
    for ctr, cbody in parsed.items():
        for g in ((cbody.get("devices") or {}).get("groups") or []):
            gid = g.get("id", "")
            for a in g.get("accelerators") or []:
                aid = a.get("id", "")
                key = f"{node}/{gid}/{aid}"
                m = a.get("mode") or ""
                if not m:
                    continue
                prev = card_modes.setdefault(key, set())
                prev.add(m)
                if len(prev) > 1:
                    errs.append(f"{key}: conflicting Pod modes {sorted(prev)}")
                if m == "Exclusive":
                    owners = excl_owners.setdefault(key, set())
                    owners.add(pname)
                    if len(owners) > 1:
                        errs.append(f"{key}: multiple Exclusive Pods {sorted(owners)}")

print("; ".join(errs[:3]))
' 2>&1)"; then
    double_book_err="python3 double-booking check failed: ${cur_db:0:180}"
  elif [[ -n "$cur_db" ]]; then
    double_book_err="$cur_db"
  fi

  # Check for negative Remaining across Devices.status and InstanceType four-view statuses.
  if ! cur_neg="$(jq -rn --argjson devs "$devs_json" --argjson its "$its_json" "${JQ_QNUM}"'
    (
      [($devs.items // [])[] | .metadata.name as $n
       | (.status.groups // [])[] | (.accelerators // [])[]
       | select((.remaining // 0) < 0 or any((.remainingProfiles // [])[]; (.count // 0) < 0))
       | "\($n)/\(.id):rem=\(.remaining // 0)"]
      +
      [($its.items // [])[] | .metadata.name as $it
       | [
           ["EX", (.status.accelerator.remaining | qnum), (.status.accelerator.capacity | qnum)],
           ["SH", (.status.acceleratorShared.remaining | qnum), (.status.acceleratorShared.capacity | qnum)],
           ["SL", (.status.acceleratorSliced.remaining | qnum), (.status.acceleratorSliced.capacity | qnum)],
           ["PT", (.status.acceleratorPartitioned.remaining | qnum), (.status.acceleratorPartitioned.capacity | qnum)]
         ][]
       | select(.[1] < 0 or .[1] > .[2])
       | "\($it)/\(.[0]):rem=\(.[1])/cap=\(.[2])"]
    ) | .[0:3] | join("; ")
  ' 2>&1)"; then
    neg_rem_err="jq negative-remaining check failed: ${cur_neg:0:180}"
  elif [[ -n "$cur_neg" ]]; then
    neg_rem_err="$cur_neg"
  fi

  # Observe whether at least one accelerated InstanceType decremented remaining while workloads run.
  drop_hit="$(jq -rn --argjson its "$its_json" "${JQ_QNUM}"'
    any(($its.items // [])[];
      .spec.acceleratable == true and (
        ((.status.accelerator.remaining | qnum) < (.status.accelerator.capacity | qnum))
        or ((.status.acceleratorShared.remaining | qnum) < (.status.acceleratorShared.capacity | qnum))
        or ((.status.acceleratorSliced.remaining | qnum) < (.status.acceleratorSliced.capacity | qnum))
        or ((.status.acceleratorPartitioned.remaining | qnum) < (.status.acceleratorPartitioned.capacity | qnum))
      )
    )
  ' 2>/dev/null || echo false)"
  [[ "$drop_hit" == "true" ]] && saw_four_view_drop=1

  if [[ "$peak_running" -ge "$WANT_MIN_RUNNING" && "$saw_four_view_drop" -eq 1 ]]; then
    if [[ "$PD_SUBMITTED" -eq 0 || "$pd_running" -ge 2 ]] \
      && [[ "$EPD_SUBMITTED" -eq 0 || "$epd_admitted_ok" -eq 1 ]]; then
      break
    fi
  fi
  sleep 4
done

# Let active storm run a brief steady window to catch delayed races, then inspect events.
sleep 6
uae_count="$(kubectl -n "$TEST_NS" get events \
  -o jsonpath='{range .items[*]}{.reason}{"\n"}{end}' 2>/dev/null \
  | grep -c 'UnexpectedAdmissionError' || true)"

pd_want=0
[[ "$PD_SUBMITTED" -eq 1 ]] && pd_want=2
if [[ "$peak_running" -ge "$WANT_MIN_RUNNING" ]] && [[ "$PD_SUBMITTED" -eq 0 || "$pd_running" -ge 2 ]]; then
  record PASS "concurrent mixed EX/SH/SL/PT & P/D burst admission" \
    "peakRunning=${peak_running}/${CHAOS_BURST_SIZE} pdRunning=${pd_running}/${pd_want}"
else
  record FAIL "concurrent mixed EX/SH/SL/PT & P/D burst admission" \
    "peakRunning=${peak_running} (want >=${WANT_MIN_RUNNING}) pdRunning=${pd_running} (submitted=${PD_SUBMITTED})"
fi

if [[ -z "$AT_PD" ]]; then
  record SKIP "three-role EPD ModelDeployment Workload/PodSet admission" \
    "no usable AcceleratorType for ${PRIMARY_MFR}"
elif [[ "$EPD_SUBMITTED" -eq 0 ]]; then
  record FAIL "three-role EPD ModelDeployment Workload/PodSet admission" \
    "apply failed: ${EPD_APPLY_ERR:0:180}"
elif [[ "$epd_admitted_ok" -eq 1 ]]; then
  record PASS "three-role EPD ModelDeployment Workload/PodSet admission" \
    "${MD_EPD_NAME} (${EPD_ENGINE}+${EPD_ROUTER}): podSets=[${epd_wl_podsets}] admitted=[${epd_admitted_roles}]"
else
  record FAIL "three-role EPD ModelDeployment Workload/PodSet admission" \
    "${MD_EPD_NAME}: roles=[${epd_pod_roles:-<none>}] podSets=[${epd_wl_podsets:-<none>}] admitted=[${epd_admitted_roles:-<none>}]"
fi

if [[ -z "$double_book_err" ]]; then
  record PASS "zero double-booking and credit budget bounds on Devices.status" \
    "0 conflicting modes, 0 shared Exclusive cards, 0 cards > 1600000 units"
else
  record FAIL "zero double-booking and credit budget bounds on Devices.status" "$double_book_err"
fi

if [[ -z "$neg_rem_err" ]]; then
  record PASS "zero negative Remaining credits across Devices and InstanceTypes" \
    "all Devices.status and InstanceType EX/SH/SL/PT views remained in [0, capacity]"
else
  record FAIL "zero negative Remaining credits across Devices and InstanceTypes" "$neg_rem_err"
fi

if [[ "${uae_count:-0}" -eq 0 ]]; then
  record PASS "zero UnexpectedAdmissionError events during storm" "0 UnexpectedAdmissionError in ${TEST_NS}"
else
  record FAIL "zero UnexpectedAdmissionError events during storm" "${uae_count} UnexpectedAdmissionError event(s)"
fi

# ===================================================================================================
# Step 4 — Drain all storm workloads and verify full four-view capacity restoration to baseline
# ===================================================================================================
kubectl -n "$TEST_NS" delete modeldeployments.worker.gpustack.ai "$MD_NAME" "$MD_EPD_NAME" \
  --ignore-not-found --wait=false >/dev/null 2>&1 || true
md_force_release "$TEST_NS" "$MD_NAME" >/dev/null 2>&1 || true
md_force_release "$TEST_NS" "$MD_EPD_NAME" >/dev/null 2>&1 || true
kubectl -n "$TEST_NS" delete pods --all --ignore-not-found --force --grace-period=0 >/dev/null 2>&1 || true
kubectl -n "$TEST_NS" delete workloads.kueue.x-k8s.io --all --ignore-not-found --wait=false >/dev/null 2>&1 || true

recovered=0
for _ in $(seq 1 35); do
  cur_its="$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null || echo '{"items":[]}')"
  all_full="$(jq -rn --argjson base "$BASE_ITS_SNAP" --argjson cur "$cur_its" "${JQ_QNUM}"'
    all($base[]; . as $b |
      any(($cur.items // [])[];
        .metadata.name == $b.name
        and (.status.accelerator.remaining | qnum) == (.status.accelerator.capacity | qnum)
        and (.status.acceleratorShared.remaining | qnum) == (.status.acceleratorShared.capacity | qnum)
        and (.status.acceleratorSliced.remaining | qnum) == (.status.acceleratorSliced.capacity | qnum)
        and (.status.acceleratorPartitioned.remaining | qnum) == (.status.acceleratorPartitioned.capacity | qnum)
      )
    )
  ' 2>/dev/null || echo false)"
  if [[ "$all_full" == "true" ]]; then
    recovered=1
    break
  fi
  sleep 3
done

if [[ "$saw_four_view_drop" -eq 1 && "$recovered" -eq 1 ]]; then
  record PASS "four-view InstanceType accounting and post-teardown capacity recovery" \
    "four-view remaining tracked storm occupancy and returned to 100% capacity after drain"
else
  record FAIL "four-view InstanceType accounting and post-teardown capacity recovery" \
    "sawOccupancyDrop=${saw_four_view_drop} recoveredToBaseline=${recovered}"
fi

echo
echo "== CHAOS 1 — Scale heterogeneous scheduling & Kueue admission storm =="
print_rows
if [[ "$FAILS" -ne 0 ]]; then
  echo "[chaos-1] ${FAILS} check(s) FAILED"
  exit 1
fi
echo "CHAOS 1 PASS"
