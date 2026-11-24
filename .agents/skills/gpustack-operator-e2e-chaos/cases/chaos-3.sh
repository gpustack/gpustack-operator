#!/usr/bin/env bash
#
# CHAOS 3 — Checkerboard fragmentation, fit-label gating & cross-mode contention:
#           60%-sliced occupancy across mock nodes + concurrent EX/SH/SL(60%&30%)/PT race
#           (MUTATING, self-recovering; AUTO-SKIPS when Mock Device Manager is not active)
#
#   chaos-3.sh [NS]
#
# Prerequisites: nodes=2; gpu=nvidia; cluster=any; inputs=CHAOS_CONCURRENCY,CHAOS_TIMEOUT,E2E_MOCK_IMAGE
# Goal:        Verify scheduler fit-label gating, AdmissionCheck over-admit holding, and cross-mode
#              card isolation under multi-node checkerboard fragmentation:
#                1. Multi-node checkerboard setup: occupy every logically sliceable H100 card across
#                   mock nodes with a 60% slice (960000/1600000 units used, 640000 free per card) so
#                   total pool credits appear abundant (1.6 cards free per 4-card node) while zero
#                   card is clean/whole and zero card has >= 50% free, and verify
#                   sliced-max-free-units.fit.gpustack.ai/* converges to 640000 across nodes;
#                2. Concurrent cross-mode race: submit simultaneous exclusive, shared, 60% sliced,
#                   30% sliced, and (when virtual MIG card exists) partitioned requests;
#                3. Eligible 30% sliced (<= 40% free) and partitioned requests route and reach
#                   Running without livelock, while unplaceable exclusive, shared, and 60% sliced
#                   requests are held cleanly (AdmissionCheck Retry or fit-affinity gate, never
#                   admitted-then-stuck-Pending and zero UnexpectedAdmissionError);
#                4. Zero cross-mode card collision (EX/SH/SL never share a card and no card exceeds
#                   1600000 units);
#                5. Releasing checkerboard blockers promptly admits the held exclusive, shared, and
#                   60% sliced workloads to Running.
# Environment: Any Kind/k3s cluster running GPUStack Operator with Mock Device Manager enabled
#              (deviceManager.mock.enabled=true) and serving nvidia. AUTO-SKIPS cleanly with SKIP
#              rows and exit 0 when Mock Device Manager is not active or nvidia is excluded.
# Inputs:      MOCKED multi-node H100 (+ virtual MIG) topology via ConfigMap hot-reload; REAL
#              workload-fit-affinity setting, Kueue Workloads, and CPU mock Pods in an isolated
#              chaos-c3-<ts> namespace.
# Expected:    All five fragmentation, fit-label, cross-mode isolation, and release checks PASS.
# Cleanup:     EXIT trap deletes all workloads and the isolated chaos-c3-<ts> namespace, restores
#              gpustack-settings, and restores the original mock ConfigMap via mock_dm_restore_cm.
set -uo pipefail

CHAOS_ID=3
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
CHAOS_CONCURRENCY="${CHAOS_CONCURRENCY:-8}"
CHAOS_TIMEOUT="${CHAOS_TIMEOUT:-150}"
AC="gpustack-node-devices"
ANNO="device.gpustack.ai/accelerator.allocated"
FIT_SETTING="workload-fit-affinity"
TEST_NS="chaos-c${CHAOS_ID}-$(date +%s)-$$"

ORIG_CM_FILE="$(mktemp)"
CM_SNAPSHOT_OK=0
NS_CREATED=0

is_pos_int() {
  [[ "${1:-}" =~ ^[1-9][0-9]*$ ]]
}
is_pos_int "$CHAOS_CONCURRENCY" || CHAOS_CONCURRENCY=8
is_pos_int "$CHAOS_TIMEOUT" || CHAOS_TIMEOUT=150

settings_save "$FIT_SETTING"

# shellcheck disable=SC2317 # Invoked via EXIT trap.
cleanup() {
  echo
  echo "[chaos-3] cleanup: deleting ${TEST_NS}, restoring settings and original mock ConfigMap"
  if [[ "$NS_CREATED" -eq 1 ]]; then
    kubectl -n "$TEST_NS" delete pods --all --ignore-not-found --force --grace-period=0 >/dev/null 2>&1 || true
    kubectl -n "$TEST_NS" delete workloads.kueue.x-k8s.io --all --ignore-not-found --wait=false >/dev/null 2>&1 || true
    kubectl delete namespace "$TEST_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  fi
  if [[ "$CM_SNAPSHOT_OK" -eq 1 ]]; then
    mock_dm_restore_cm "$NS" "$MOCK_DM_CM" "$ORIG_CM_FILE" || true
  fi
  settings_restore || true
  rm -f "$ORIG_CM_FILE"
}
trap cleanup EXIT

if ! mock_dm_detect_env "$NS" || ! mock_dm_manufacturer_allowed nvidia; then
  skip_msg="mock device-manager DaemonSet (gpustack.ai/manufacturer=mock serving nvidia) is not active on this cluster"
  record SKIP "checkerboard 60%-sliced fragmentation & fit-label convergence" "$skip_msg"
  record SKIP "eligible 30%-sliced & partitioned Pods schedule on fragmented pool" "$skip_msg"
  record SKIP "unplaceable EX/SH/60%-SL requests held without stuck-Pending or UAE" "$skip_msg"
  record SKIP "zero cross-mode card collision across EX/SH/SL/PT" "$skip_msg"
  record SKIP "blocker release promptly admits held EX/SH/60%-SL workloads" "$skip_msg"
  echo
  echo "== CHAOS 3 — SKIPPED (${skip_msg}) =="
  print_rows
  exit 0
fi

if mock_dm_snapshot_cm "$NS" "$MOCK_DM_CM" "$ORIG_CM_FILE"; then
  CM_SNAPSHOT_OK=1
else
  echo "[chaos-3] FATAL: failed to snapshot ConfigMap ${NS}/${MOCK_DM_CM}" >&2
  exit 1
fi

if [[ "$(setting_get "$FIT_SETTING")" == "false" ]]; then
  setting_set "$FIT_SETTING" true
  settings_settle
fi

NODE_COUNT="${#MOCK_DM_NODES[@]}"
# Cap checkerboard nodes to at most 8 (32 cards) so local/remote runs finish deterministically within CHAOS_TIMEOUT.
ACTIVE_COUNT="$NODE_COUNT"
[[ "$ACTIVE_COUNT" -gt 8 ]] && ACTIVE_COUNT=8

echo "[chaos-3] totalMockNodes=${NODE_COUNT} checkerboardNodes=${ACTIVE_COUNT} ns=${TEST_NS}"
kubectl create namespace "$TEST_NS" >/dev/null
NS_CREATED=1

# ===================================================================================================
# Step 1 — Configure multi-node H100 topology (with 1 virtual MIG slot on last node) & plant 60% blockers
# ===================================================================================================
build_c3_spec() {
  printf 'default:\n  groups: []\nnodes:\n'
  local idx node
  for ((idx = 0; idx < ACTIVE_COUNT; idx++)); do
    node="${MOCK_DM_NODES[$idx]}"
    printf '  "%s":\n' "$node"
    if [[ "$idx" -eq $((ACTIVE_COUNT - 1)) ]]; then
      # 3 logical H100 cards + 1 virtual MIG H100 card (distinct id so _DeviceKey triggers immediate re-detect).
      cat <<'EOF'
    preset: nvidia-h100
    count: 4
    cardOverrides:
      3:
        id: GPU-c3-mig-912151ec-e9f1-fc26
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
      printf '    preset: nvidia-h100\n    count: 4\n'
    fi
  done
}

mock_dm_apply_spec "$NS" "$MOCK_DM_CM" "$MOCK_DM_KEY" "$(build_c3_spec)"

for ((idx = 0; idx < ACTIVE_COUNT; idx++)); do
  node="${MOCK_DM_NODES[$idx]}"
  want_logical=4
  [[ "$idx" -eq $((ACTIVE_COUNT - 1)) ]] && want_logical=3
  mock_dm_wait_devices "$NS" "$node" \
    "([.spec.groups[] | select(.manufacturer == \"nvidia\") | .accelerators[] | select((.status.logicalSliced.count // 0) > 0)] | length == ${want_logical})" "$CHAOS_TIMEOUT"
  mock_dm_wait_node_allocatable "$node" ".[\"nvidia.com/gpu\"] == \"${want_logical}\"" "$CHAOS_TIMEOUT" || true
done

NODE0="${MOCK_DM_NODES[0]}"
NV_GID="$(kubectl get devices.worker.gpustack.ai "$NODE0" -o json 2>/dev/null \
  | jq -r '[.spec.groups[] | select(.manufacturer == "nvidia") | .id][0] // ""')"
AKEY_NV="nvidia-${NV_GID}"
FIT_KEY="sliced-max-free-units.fit.gpustack.ai/${AKEY_NV}"

IT_NV=""
LQ_NV=""
for _ in $(seq 1 30); do
  read -r IT_NV LQ_NV <<<"$(discover_node_pool "$NODE0" "$NV_GID" --sliceable --require-entrance name entrance 2>/dev/null || true)"
  if [[ -n "$IT_NV" && -n "$LQ_NV" ]] \
    && kubectl -n "$TEST_NS" get localqueue.kueue.x-k8s.io "$LQ_NV" >/dev/null 2>&1; then
    break
  fi
  sleep 3
done

TOTAL_LOGICAL=$(( (ACTIVE_COUNT * 4) - 1 ))
mock_dm_wait_it ".items[] | select(.metadata.name == \"${IT_NV}\" and ((.status.accelerator.remaining // \"0\") | tostring | tonumber) == ${TOTAL_LOGICAL})" 60 || true

submit_c3_pod() {
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
      metadata: {
        name: $name,
        namespace: $ns,
        labels: {"kueue.x-k8s.io/queue-name": $lq}
      },
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

wait_c3_running() {
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

# Plant 60% sliced blockers on every logical card of every active node so each card has 640000 free units (40%).
BLOCKER_PODS=()
for ((idx = 0; idx < ACTIVE_COUNT; idx++)); do
  node="${MOCK_DM_NODES[$idx]}"
  cards_here=4
  [[ "$idx" -eq $((ACTIVE_COUNT - 1)) ]] && cards_here=3
  for ((c = 1; c <= cards_here; c++)); do
    bp="chaos-c3-blk-n${idx}-c${c}"
    BLOCKER_PODS+=("$bp")
    submit_c3_pod "$bp" "$node" "$LQ_NV" '{"nvidia.com/gpu.sliced":"1","nvidia.com/gpu.sliced.memory-percentage":"60"}'
    # Brief stagger per card on the same node so the allocator spreads 60% claims across distinct cards cleanly.
    wait_c3_running "$bp" 60 || true
    sleep 2
  done
done

fit_all_ok=1
for ((idx = 0; idx < ACTIVE_COUNT; idx++)); do
  node="${MOCK_DM_NODES[$idx]}"
  got_fit=""
  for _ in $(seq 1 35); do
    got_fit="$(kubectl get node "$node" -o json 2>/dev/null \
      | jq -r --arg k "$FIT_KEY" '.metadata.labels[$k] // ""' 2>/dev/null)"
    [[ "$got_fit" == "640000" ]] && break
    sleep 3
  done
  if [[ "$got_fit" != "640000" ]]; then
    fit_all_ok=0
    break
  fi
done

if [[ "$fit_all_ok" -eq 1 ]]; then
  record PASS "checkerboard 60%-sliced fragmentation & fit-label convergence" \
    "${#BLOCKER_PODS[@]} cards at 60%; ${FIT_KEY}=640000 across all ${ACTIVE_COUNT} node(s)"
else
  record FAIL "checkerboard 60%-sliced fragmentation & fit-label convergence" \
    "not all active nodes converged ${FIT_KEY}=640000"
fi

# ===================================================================================================
# Step 2 — Concurrent cross-mode race: EX + SH + 60% SL (unplaceable) vs 30% SL + PT (eligible)
# ===================================================================================================
P_RACE_EX="chaos-c3-race-excl"
P_RACE_SH="chaos-c3-race-shared"
P_RACE_SL60="chaos-c3-race-sl60"
P_RACE_SL30_A="chaos-c3-race-sl30-a"
P_RACE_SL30_B="chaos-c3-race-sl30-b"
P_RACE_PT="chaos-c3-race-part"

submit_c3_pod "$P_RACE_EX" "" "$LQ_NV" '{"nvidia.com/gpu":"1"}' &
submit_c3_pod "$P_RACE_SH" "" "$LQ_NV" '{"nvidia.com/gpu.shared":"1"}' &
submit_c3_pod "$P_RACE_SL60" "" "$LQ_NV" '{"nvidia.com/gpu.sliced":"1","nvidia.com/gpu.sliced.memory-percentage":"60"}' &
submit_c3_pod "$P_RACE_SL30_A" "" "$LQ_NV" '{"nvidia.com/gpu.sliced":"1","nvidia.com/gpu.sliced.memory-percentage":"30"}' &
submit_c3_pod "$P_RACE_SL30_B" "" "$LQ_NV" '{"nvidia.com/gpu.sliced":"1","nvidia.com/gpu.sliced.memory-percentage":"30"}' &
submit_c3_pod "$P_RACE_PT" "" "$LQ_NV" '{"nvidia.com/gpu.partitioned":"1","nvidia.com/gpu.partitioned.mig-2g.20gb":"1"}' &
wait

sl30a_ok=0; wait_c3_running "$P_RACE_SL30_A" 60 && sl30a_ok=1
sl30b_ok=0; wait_c3_running "$P_RACE_SL30_B" 60 && sl30b_ok=1
pt_ok=0; wait_c3_running "$P_RACE_PT" 60 && pt_ok=1

if [[ "$sl30a_ok" -eq 1 && "$sl30b_ok" -eq 1 && "$pt_ok" -eq 1 ]]; then
  record PASS "eligible 30%-sliced & partitioned Pods schedule on fragmented pool" \
    "2 x 30% sliced + 1 x mig-2g.20gb Running concurrently alongside checkerboard"
else
  record FAIL "eligible 30%-sliced & partitioned Pods schedule on fragmented pool" \
    "sl30a=${sl30a_ok} sl30b=${sl30b_ok} part=${pt_ok}"
fi

# Verify EX, SH, and 60%-SL are held cleanly (not Running, never bound to a node stuck Pending, 0 UAE).
sleep 15
held_check="$(kubectl -n "$TEST_NS" get workloads.kueue.x-k8s.io -o json 2>/dev/null \
  | PODS_JSON="$(kubectl -n "$TEST_NS" get pods -o json 2>/dev/null)" AC="$AC" python3 -c '
import json, os, sys

wls = json.load(sys.stdin).get("items", [])
pods = {p["metadata"]["name"]: p for p in json.loads(os.environ["PODS_JSON"]).get("items", [])}
ac_name = os.environ["AC"]
targets = ["chaos-c3-race-excl", "chaos-c3-race-shared", "chaos-c3-race-sl60"]

bad = []
summary = []
for t in targets:
    p = pods.get(t)
    if not p:
        bad.append(f"{t}:missing")
        continue
    ph = (p.get("status") or {}).get("phase", "")
    node = (p.get("spec") or {}).get("nodeName") or ""
    if ph == "Running" or node != "":
        bad.append(f"{t}:phase={ph},node={node}")
        continue
    wl = next((w for w in wls if any(o.get("name") == t for o in w.get("metadata", {}).get("ownerReferences", []))), None)
    if not wl:
        bad.append(f"{t}:no-workload")
        continue
    st = wl.get("status") or {}
    adm = next((c.get("status", "False") for c in st.get("conditions", []) if c.get("type") == "Admitted"), "False")
    qr = next((c.get("status", "False") for c in st.get("conditions", []) if c.get("type") == "QuotaReserved"), "False")
    chk = next((c.get("state", "-") for c in st.get("admissionChecks", []) if c.get("name") == ac_name), "-")
    if adm == "True":
        bad.append(f"{t}:unexpectedly-Admitted")
    elif chk != "Retry" and qr != "False":
        bad.append(f"{t}:chk={chk},qr={qr}")
    else:
        summary.append(f"{t}(ac={chk},qr={qr})")

if bad:
    print("FAIL|" + "; ".join(bad))
else:
    print("PASS|" + ", ".join(summary))
' 2>/dev/null)"

uae_c3="$(kubectl -n "$TEST_NS" get events \
  -o jsonpath='{range .items[*]}{.reason}{"\n"}{end}' 2>/dev/null \
  | grep -c 'UnexpectedAdmissionError' || true)"

if [[ "${held_check%%|*}" == "PASS" && "${uae_c3:-0}" -eq 0 ]]; then
  record PASS "unplaceable EX/SH/60%-SL requests held without stuck-Pending or UAE" \
    "${held_check#*|} (0 UAE)"
else
  record FAIL "unplaceable EX/SH/60%-SL requests held without stuck-Pending or UAE" \
    "${held_check#*|}; uae=${uae_c3}"
fi

# Check zero cross-mode card collision across all Pods and Devices.status.
if ! collision_err="$(ANNO="$ANNO" PODS_JSON="$(kubectl -n "$TEST_NS" get pods -o json 2>/dev/null)" \
  DEVS_JSON="$(kubectl get devices.worker.gpustack.ai -o json 2>/dev/null)" python3 -c '
import json, os

pods = json.loads(os.environ["PODS_JSON"]).get("items", [])
devs = json.loads(os.environ["DEVS_JSON"]).get("items", [])
anno_key = os.environ["ANNO"]
errs = []

card_modes = {}
for p in pods:
    if p.get("metadata", {}).get("deletionTimestamp"):
        continue
    if (p.get("status") or {}).get("phase") != "Running":
        continue
    node = (p.get("spec") or {}).get("nodeName") or ""
    raw = ((p.get("metadata") or {}).get("annotations") or {}).get(anno_key, "")
    if not node or not raw:
        continue
    parsed = json.loads(raw)
    for cbody in parsed.values():
        for g in ((cbody.get("devices") or {}).get("groups") or []):
            gid = g.get("id", "")
            for a in g.get("accelerators") or []:
                m = a.get("mode")
                if not m:
                    continue
                aid = a.get("id", "")
                k = f"{node}/{gid}/{aid}"
                s = card_modes.setdefault(k, set())
                s.add(m)
                if len(s) > 1:
                    errs.append(f"{k}:modes={sorted(str(x) for x in s)}")

for d in devs:
    n = d.get("metadata", {}).get("name", "")
    for g in (d.get("status") or {}).get("groups") or []:
        for a in g.get("accelerators") or []:
            aid = a.get("id", "")
            rem = a.get("remaining") or 0
            if (a.get("mode") or 0) in (1, 2, 3, "Exclusive", "Shared", "Sliced") and rem < 0:
                errs.append(f"{n}/{aid}:rem={rem}<0")

print("; ".join(errs[:3]))
' 2>&1)"; then
  collision_err="python3 cross-mode collision check failed: ${collision_err:0:180}"
fi

if [[ -z "$collision_err" ]]; then
  record PASS "zero cross-mode card collision across EX/SH/SL/PT" \
    "0 mixed-mode cards and 0 over-committed cards (>1600000 units)"
else
  record FAIL "zero cross-mode card collision across EX/SH/SL/PT" "$collision_err"
fi

# ===================================================================================================
# Step 3 — Release 3 checkerboard blockers (+ 30% co-residents) on NODE0 -> held EX, SH, and 60% SL reach Running
# ===================================================================================================
for c in 1 2 3; do
  bp="chaos-c3-blk-n0-c${c}"
  kubectl -n "$TEST_NS" delete pod "$bp" --ignore-not-found --force --grace-period=0 >/dev/null 2>&1 || true
done
for p30 in "$P_RACE_SL30_A" "$P_RACE_SL30_B"; do
  kubectl -n "$TEST_NS" delete pod "$p30" --ignore-not-found --force --grace-period=0 >/dev/null 2>&1 || true
done

rel_ex=0; wait_c3_running "$P_RACE_EX" 90 && rel_ex=1
rel_sh=0; wait_c3_running "$P_RACE_SH" 90 && rel_sh=1
rel_sl60=0; wait_c3_running "$P_RACE_SL60" 90 && rel_sl60=1

# Re-check cross-mode collision after EX, SH, and 60% SL land on the 3 freed cards!
if ! post_collision="$(ANNO="$ANNO" PODS_JSON="$(kubectl -n "$TEST_NS" get pods -o json 2>/dev/null)" python3 -c '
import json, os
pods = json.loads(os.environ["PODS_JSON"]).get("items", [])
anno_key = os.environ["ANNO"]
card_modes = {}
errs = []
for p in pods:
    if p.get("metadata", {}).get("deletionTimestamp") or (p.get("status") or {}).get("phase") != "Running":
        continue
    node = (p.get("spec") or {}).get("nodeName") or ""
    raw = ((p.get("metadata") or {}).get("annotations") or {}).get(anno_key, "")
    if not node or not raw:
        continue
    for cbody in json.loads(raw).values():
        for g in ((cbody.get("devices") or {}).get("groups") or []):
            gid = g.get("id", "")
            for a in g.get("accelerators") or []:
                m = a.get("mode")
                if m:
                    aid = a.get("id", "")
                    k = f"{node}/{gid}/{aid}"
                    s = card_modes.setdefault(k, set())
                    s.add(m)
                    if len(s) > 1:
                        errs.append(f"{k}:{sorted(str(x) for x in s)}")
print("; ".join(errs[:3]))
' 2>&1)"; then
  post_collision="python3 post-release collision check failed: ${post_collision:0:180}"
fi

if [[ "$rel_ex" -eq 1 && "$rel_sh" -eq 1 && "$rel_sl60" -eq 1 && -z "$post_collision" ]]; then
  record PASS "blocker release promptly admits held EX/SH/60%-SL workloads" \
    "EX, SH, and 60% SL all reached Running on disjoint freed cards"
else
  record FAIL "blocker release promptly admits held EX/SH/60%-SL workloads" \
    "ex=${rel_ex} sh=${rel_sh} sl60=${rel_sl60} postCollision='${post_collision}'"
fi

echo
echo "== CHAOS 3 — Checkerboard fragmentation, fit-label gating & cross-mode contention =="
print_rows
if [[ "$FAILS" -ne 0 ]]; then
  echo "[chaos-3] ${FAILS} check(s) FAILED"
  exit 1
fi
echo "CHAOS 3 PASS"
