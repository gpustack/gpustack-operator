#!/usr/bin/env bash
#
# CASE 8 — Real NVIDIA logical slicing: runtime isolation, per-card packing, co-residency, over-commit refusal and slice metrics   (MUTATING, self-recovering; AUTO-SKIPS without real sliced hardware)
#
#   case-8.sh <NS>
#
# Prerequisites: nodes=1; gpu=any; cluster=real; inputs=E2E_SLICE_BASE,E2E_SLICE_IMAGE,E2E_SLICE_LOAD_COMMAND,E2E_SLICE_LOAD_IMAGE,E2E_SLICE_MANUF,E2E_SLICE_MEM_ENV,E2E_SLICE_MEM_ENV_UNIT,E2E_SLICE_SMI,E2E_SLICE_SMI_MEM_RE,E2E_SLICE_SM_ENV
# Goal:        On real logically sliceable accelerator hardware, five contracts hold end to end —
#              1. Runtime isolation (percentage and MiB): a sliced workload is capped at runtime to
#                 its share of a real card by memory percentage, memory MiB, and compute (SM)
#                 percentage through the vendor's slicing runtime (HAMi libvgpu.so by default);
#              2. Sibling-pool consistency: every InstanceType that shares the pool's os / arch /
#                 acceleratorGroup / acceleratable keeps one identical four-view status when the
#                 Devices ledger moves;
#              3. Instance admission webhooks: Default scales unit CPU/RAM to slice percentages,
#                 mirrors a lone memory percentage to compute, and pins accelerator=1; Validate
#                 rejects a sliceable request whose accelerator count is not 1;
#              4. Per-card logical-slice packing & over-commit guard (when >=2 logically sliceable
#                 NVIDIA cards exist on one node): claims too large to share (>50%) take distinct
#                 cards, a claim that fits joins the card already in use rather than opening a fresh
#                 card, no card's per-card VRAM limits sum above its physical VRAM, and the SL view
#                 reports units-accurate occupancy and per-card OnceMaxRequest (<=100);
#              5. Single-card co-residency, over-budget refusal, self-eviction guard and per-slice
#                 metrics (when a single-card logically sliceable NVIDIA pool exists): two 40%
#                 slices coexist on one physical card, a third 40% slice is held, the two admitted
#                 slices stay Running after the third's rejection, and each Instance's /metrics
#                 subresource reports its own slice share.
# Environment: Needs REAL accelerator hardware advertising a *.sliced resource whose cards are
#              LOGICALLY sliceable (a hardware-partitioned card serves no logical slice), and a
#              logically sliceable accelerated InstanceType with an entrance LocalQueue.
#              AUTO-SKIPS (exit 0, prints why) when no node advertises a *.sliced resource — the
#              runtime cap cannot be mocked. Defaults to NVIDIA (HAMi libvgpu). Legs 4 and 5 are
#              topology-specific NVIDIA legs (>=2 cards vs single-card pool) and record SKIP cleanly
#              when the cluster shape does not match.
# Inputs:      All real, nothing mocked —
#              - Leg 1: POD_PCT (.sliced=1 + memory-%=50 + cores-%=50) and POD_MIB (.sliced=1 +
#                memory-mib=4096) on the sliceable pool's entrance LocalQueue;
#              - Leg 2: POD_LOAD (50% slice) + two throwaway admin InstanceTypes e2e-c8-sib-a /
#                e2e-c8-sib-b declaring the pool's acceleratorGroup/os/arch;
#              - Leg 3: three probe Instances (25%/25%, 50%/unset, 25%/25% with accelerator=2);
#              - Leg 4: up to 4 x 60% Pods, then 100% + 50% -> delete 100% -> 30% on a >=2-card
#                NVIDIA node;
#              - Leg 5: three 40% sliced Instances (two fitting + one over-budget) on a single-card
#                NVIDIA pool, optionally with E2E_SLICE_LOAD_IMAGE / E2E_SLICE_LOAD_COMMAND.
# Expected:    Every applicable leg's checks record PASS; topology-specific legs that cannot run on
#              this cluster shape record SKIP with the reason.
# Cleanup:     Trap deletes every test Pod, Instance, and throwaway InstanceType (with its
#              ClusterQueue), then waits for the pool's four-view status to return to baseline.
#              Each leg also releases its objects before the next starts. Idempotent, runs on pass
#              AND fail, safe to re-run.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_pool-lib.sh"

NS="${1:?usage: case-8.sh <NS>}"
# --- Vendor dimensions. Every one defaults to NVIDIA, so an NVIDIA run needs nothing set. ---
MANUF="${E2E_SLICE_MANUF:-nvidia}"
BASE="${E2E_SLICE_BASE:-nvidia.com/gpu}"
SLICED=${BASE}.sliced
CORESPCT=${BASE}.sliced.cores-percentage
MEMPCT=${BASE}.sliced.memory-percentage
MEMMIB=${BASE}.sliced.memory-mib
SMI="${E2E_SLICE_SMI:-nvidia-smi}"
SMI_MEM_RE="${E2E_SLICE_SMI_MEM_RE:-/[[:space:]]*[0-9]+MiB}"
SM_ENV="${E2E_SLICE_SM_ENV:-CUDA_DEVICE_SM_LIMIT}"
MEM_ENV="${E2E_SLICE_MEM_ENV:-CUDA_DEVICE_MEMORY_LIMIT_0}"
MEM_ENV_UNIT="${E2E_SLICE_MEM_ENV_UNIT:-m}"
IMAGE="${E2E_SLICE_IMAGE:-ubuntu:24.04}"
LOAD_IMAGE="${E2E_SLICE_LOAD_IMAGE:-}"
LOAD_COMMAND="${E2E_SLICE_LOAD_COMMAND:-[\"python\", \"-c\", \"import torch,time;h=torch.empty(1024**3//2,dtype=torch.float16,device='cuda');h.fill_(1.0);print('allocated',torch.cuda.memory_allocated()//1024**2,'MiB',flush=True);\\nwhile True: time.sleep(5)\"]}"

# --- The objects this case creates. Every one is deleted by the trap. ---
POD_PCT=e2e-c8-slice-pct
POD_MIB=e2e-c8-slice-mib
POD_LOAD=e2e-c8-load
SIB_A=e2e-c8-sib-a
SIB_B=e2e-c8-sib-b
INST_OK=e2e-c8-slice-ok
INST_MEMONLY=e2e-c8-slice-memonly
INST_BAD=e2e-c8-slice-badacc
SPREADPFX=e2e-c8-spread
PACKPFX=e2e-c8-pack
COEXIST_A=e2e-c8-coexist-a
COEXIST_B=e2e-c8-coexist-b
COEXIST_C=e2e-c8-coexist-c
MIB_REQ=4096   # fits a 16Gi (T4) or 24Gi (A10G) card
SPREADPCT=60   # >50 so no two of these can share one card

# The pool legs 1-3 work on, filled by the discovery below.
IT=""; LQ=""; CARDMEM=""; UNIT_CPU=""; UNIT_RAM=""
AKEY=""; OS=""; ARCH=""; POOL_MANUF=""; SLICED_CAP=""; BASE_SIG=""

# sig <instancetype> — the pool's four-view remaining signature (EX|SH|SL|PT), or empty.
sig() {
  kubectl get instancetype "$1" -o jsonpath='{.status.accelerator.remaining}|{.status.acceleratorShared.remaining}|{.status.acceleratorSliced.remaining}|{.status.acceleratorPartitioned.remaining}' 2>/dev/null
}

# wait_settled <baseline> [instancetype] — wait for the pool's four-view status to return to baseline.
wait_settled() {
  local base="$1" target_it="${2:-$IT}" now=""
  [ -n "$target_it" ] || return 0
  for _ in $(seq 1 30); do
    now=$(sig "$target_it")
    if [ "$now" = "$base" ]; then return 0; fi
    sleep 3
  done
  echo "[case-8] warning: pool ${target_it} did not return to ${base} within 90s (now ${now:-?})"
  return 0
}

# wait_running <pod> — poll until the Pod is Running (admitted by Kueue + scheduled), else non-zero.
wait_running() {
  for _ in $(seq 1 40); do
    [ "$(kubectl -n default get pod "$1" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Running" ] && return 0
    sleep 3
  done
  return 1
}

# exec_out <pod> <cmd>... — run a command in the Pod and echo its output, retried.
exec_out() {
  local pod="$1"; shift
  local out
  for _ in $(seq 1 8); do
    out=$(kubectl -n default exec "$pod" -- "$@" 2>/dev/null)
    [ -n "$out" ] && { printf '%s\n' "$out"; return 0; }
    sleep 3
  done
  return 1
}

# verdict <cond-exit-code> <check> <object-on-pass> <object-on-fail>
verdict() { if [ "$1" -eq 0 ]; then record PASS "$2" "$3"; else record FAIL "$2" "$4"; fi; }

delete_indexed_pods() { # delete_indexed_pods <prefix> <index>...
  local prefix="$1"; shift
  local i
  for i in "$@"; do
    kubectl -n default delete pod "${prefix}-$i" --ignore-not-found --force --grace-period=0 >/dev/null 2>&1 || true
  done
}

cleanup_types() {
  local t
  for t in "$SIB_A" "$SIB_B"; do
    kubectl patch instancetype "$t" --type=merge -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
    kubectl delete instancetype "$t" --wait=false >/dev/null 2>&1 || true
    kubectl delete clusterqueue "$t" --wait=false >/dev/null 2>&1 || true
  done
}

restore() {
  [ -n "${IT:-}" ] || return 0
  echo
  echo "[case-8] cleanup: deleting test Pods, Instances and InstanceTypes"
  kubectl -n default delete pod "$POD_PCT" "$POD_MIB" "$POD_LOAD" --ignore-not-found --force --grace-period=0 >/dev/null 2>&1 || true
  delete_indexed_pods "$SPREADPFX" 1 2 3 4
  delete_indexed_pods "$PACKPFX" 1 2 3
  kubectl -n default delete instance "$INST_OK" "$INST_MEMONLY" "$INST_BAD" \
    "$COEXIST_A" "$COEXIST_B" "$COEXIST_C" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  cleanup_types
  sleep 5
  if [ -n "$BASE_SIG" ]; then wait_settled "$BASE_SIG"; fi
}
trap restore EXIT

# --- Skip gate: real sliced accelerator required. ---
sliced_node=$(kubectl get nodes -o json 2>/dev/null | SUFFIX="/${BASE#*/}.sliced" python3 -c "
import json,os,sys
for n in json.load(sys.stdin).get('items',[]):
    a=n.get('status',{}).get('allocatable',{})
    for k,v in a.items():
        if k.endswith(os.environ['SUFFIX']) and int(v)>0:
            print(n['metadata']['name']); sys.exit(0)
" 2>/dev/null)
if [ -z "$sliced_node" ]; then
  echo "== CASE 8 — SKIPPED =="
  echo "No node advertises a *.sliced accelerator resource — this case needs real accelerator hardware"
  echo "(the HAMI runtime cap cannot be mocked). Run it on a GPU cluster to exercise real slicing isolation."
  exit 0
fi
echo "[case-8] real sliced accelerator found on ${sliced_node}"

# --- Pool discovery for legs 1-3 ---
IFS='|' read -r IT LQ CARDMEM UNIT_CPU UNIT_RAM AKEY OS ARCH POOL_MANUF SLICED_CAP <<<"$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null | MANUF="$MANUF" python3 -c "
import json,os,sys
manuf=os.environ['MANUF']
cands=[]
for it in json.load(sys.stdin).get('items',[]):
    s=it.get('spec',{}); st=it.get('status',{}); d=st.get('detail',{}); sd=d.get('slicedDetail',{}); u=s.get('unitResources',{})
    sliceable=(sd.get('logical',{}).get('count',0) or 0)>0
    if not (s.get('acceleratable') and sliceable): continue
    if it['metadata']['name'].startswith('e2e-c8-sib'): continue
    if not st.get('entrance'): continue
    cands.append([it['metadata']['name'], st.get('entrance',''), d.get('memory',''),
                  u.get('cpu',''), u.get('ram',''), s.get('acceleratorGroup',''),
                  s.get('os',''), s.get('arch',''), d.get('manufacturer',''),
                  st.get('acceleratorSliced',{}).get('capacity','')])
cands.sort(key=lambda c: 0 if c[8]==manuf else 1)
if cands: print('|'.join(cands[0]))
")"
if [ -z "${IT:-}" ] || [ -z "${LQ:-}" ]; then
  echo "no logically sliceable ${MANUF} InstanceType with an entrance LocalQueue found"
  exit 1
fi
echo "[case-8] sliceable InstanceType ${IT} (card memory ${CARDMEM}, unit cpu=${UNIT_CPU} ram=${UNIT_RAM}) via LocalQueue ${LQ}"

PHYS_MIB=$(python3 -c "
import re
m = re.match(r'\s*(\d+)\s*([GM])i?', '${CARDMEM}')
print(int(m.group(1)) * (1024 if m.group(2) == 'G' else 1) if m else 0)
" 2>/dev/null)
echo "[case-8] card memory ${CARDMEM} = ${PHYS_MIB}MiB"

RUNTIME_MANUF="${POOL_MANUF:-$MANUF}"
RUNTIMECLASS=""
if [ -n "$RUNTIME_MANUF" ] && kubectl get runtimeclass.node.k8s.io "$RUNTIME_MANUF" >/dev/null 2>&1; then RUNTIMECLASS="$RUNTIME_MANUF"; fi
RTC_LINE=""; [ -n "$RUNTIMECLASS" ] && RTC_LINE="runtimeClassName: ${RUNTIMECLASS}"
echo "[case-8] slice pods runtimeClass: ${RUNTIMECLASS:-<none>}"

BASE_SIG=$(sig "$IT")
if [ -z "$BASE_SIG" ] || [ "$BASE_SIG" = "|||" ]; then
  echo "[case-8] the pool ${IT} has no four-view status yet"
  exit 1
fi
echo "[case-8] baseline four-view (EX|SH|SL|PT) on ${IT} = ${BASE_SIG}"

# ===================================================================================================
# Leg 1 — Real slice runtime isolation (percentage, then MiB).
# ===================================================================================================
echo
echo "[case-8] === leg 1: real slice runtime isolation (percentage, then MiB) ==="

POD_PCT_MANIFEST=$(cat <<EOF
apiVersion: v1
kind: Pod
metadata: { name: ${POD_PCT}, namespace: default, labels: { kueue.x-k8s.io/queue-name: ${LQ} } }
spec:
  schedulerName: default-scheduler
  ${RTC_LINE}
  restartPolicy: Never
  containers:
    - name: main
      image: ${IMAGE}
      command: ["sleep", "86400"]
      resources:
        limits:   { ${SLICED}: "1", ${CORESPCT}: "50", ${MEMPCT}: "50" }
        requests: { ${SLICED}: "1", ${CORESPCT}: "50", ${MEMPCT}: "50" }
EOF
)
POD_MIB_MANIFEST=$(cat <<EOF
apiVersion: v1
kind: Pod
metadata: { name: ${POD_MIB}, namespace: default, labels: { kueue.x-k8s.io/queue-name: ${LQ} } }
spec:
  schedulerName: default-scheduler
  ${RTC_LINE}
  restartPolicy: Never
  containers:
    - name: main
      image: ${IMAGE}
      command: ["sleep", "86400"]
      resources:
        limits:   { ${SLICED}: "1", ${MEMMIB}: "${MIB_REQ}" }
        requests: { ${SLICED}: "1", ${MEMMIB}: "${MIB_REQ}" }
EOF
)

if [ "${POOL_MANUF:-}" != "$MANUF" ]; then
  record SKIP "leg 1: real slice runtime isolation" \
    "pool ${IT} reports manufacturer '${POOL_MANUF:-<none>}', run asks for ${MANUF} (set E2E_SLICE_MANUF and E2E_SLICE_BASE)"
else
  echo "[case-8] creating ${POD_PCT}: ${SLICED}=1 memory-%=50 cores-%=50 on ${LQ}"
  printf '%s\n' "$POD_PCT_MANIFEST" | kubectl apply -f - >/dev/null
  if wait_running "$POD_PCT"; then
    record PASS "percentage slice admitted+running" "${POD_PCT} Running (webhook fold -> Kueue -> AdmissionCheck -> schedule)"
    sm=$(exec_out "$POD_PCT" printenv "$SM_ENV")
    mem=$(exec_out "$POD_PCT" printenv "$MEM_ENV")
    if [ "$sm" = "50" ]; then
      record PASS "compute (SM) capped to 50%" "${SM_ENV}=${sm}"
    else
      record FAIL "compute (SM) capped to 50%" "${SM_ENV}='${sm:-<unset>}', want 50"
    fi
    smi_total=$(exec_out "$POD_PCT" "$SMI" | grep -oE "$SMI_MEM_RE" | head -1 | grep -oE '[0-9]+')
    if [ -n "$mem" ] && [ -n "$smi_total" ] && [ "${PHYS_MIB:-0}" -gt 0 ] && [ "$smi_total" -lt "$PHYS_MIB" ]; then
      record PASS "memory capped below physical" "${SMI} total=${smi_total}MiB < physical ${PHYS_MIB}MiB (${MEM_ENV}=${mem})"
    else
      record FAIL "memory capped below physical" "${SMI} total='${smi_total:-?}' not < physical '${PHYS_MIB:-?}'MiB (${MEM_ENV}='${mem:-<unset>}')"
    fi
  else
    record FAIL "percentage slice admitted+running" "${POD_PCT} not Running — check Kueue admission / AdmissionCheck / device plugin"
  fi

  echo "[case-8] creating ${POD_MIB}: ${SLICED}=1 memory-mib=${MIB_REQ} on ${LQ}"
  printf '%s\n' "$POD_MIB_MANIFEST" | kubectl apply -f - >/dev/null
  if wait_running "$POD_MIB"; then
    mem=$(exec_out "$POD_MIB" printenv "$MEM_ENV")
    smi_total=$(exec_out "$POD_MIB" "$SMI" | grep -oE "$SMI_MEM_RE" | head -1 | grep -oE '[0-9]+')
    if [ "$mem" = "${MIB_REQ}${MEM_ENV_UNIT}" ] || [ "$smi_total" = "$MIB_REQ" ]; then
      record PASS "memory-mib slice capped to ${MIB_REQ}MiB" "${MEM_ENV}=${mem}, ${SMI} total=${smi_total}MiB"
    else
      record FAIL "memory-mib slice capped to ${MIB_REQ}MiB" "${MEM_ENV}='${mem:-?}', ${SMI} total='${smi_total:-?}', want ${MIB_REQ}"
    fi
  else
    record FAIL "memory-mib slice admitted+running" "${POD_MIB} not Running"
  fi

  echo "[case-8] releasing leg 1 slice pods"
  kubectl -n default delete pod "$POD_PCT" "$POD_MIB" --ignore-not-found --force --grace-period=0 >/dev/null 2>&1 || true
  wait_settled "$BASE_SIG"
fi

# ===================================================================================================
# Leg 2 — Sibling InstanceTypes of one pool stay status-consistent.
# ===================================================================================================
echo
echo "[case-8] === leg 2: sibling InstanceTypes of one pool stay status-consistent ==="

POOL_SLICED=""; POOL_MEMPCT=""; POOL_CORESPCT=""
if [ -n "${AKEY:-}" ]; then
  read -r POOL_SLICED POOL_MEMPCT POOL_CORESPCT <<<"$(kubectl get nodes -o json 2>/dev/null | python3 -c "
import json,sys
akey='${AKEY}'
for n in json.load(sys.stdin).get('items',[]):
    if n['metadata'].get('labels',{}).get('acceleratable.feature.gpustack.ai/'+akey)!='true': continue
    for k in n.get('status',{}).get('allocatable',{}):
        if k.endswith('.sliced'):
            print(k, k+'.memory-percentage', k+'.cores-percentage'); raise SystemExit
")"
fi

if [ -z "${AKEY:-}" ]; then
  record SKIP "leg 2: sibling InstanceTypes stay status-consistent" "pool ${IT} declares no acceleratorGroup"
elif [ -z "${SLICED_CAP:-}" ] || [ "${SLICED_CAP}" = "0" ]; then
  record SKIP "leg 2: sibling InstanceTypes stay status-consistent" "pool ${IT} reports no sliced capacity"
elif [ -z "${POOL_SLICED:-}" ]; then
  record SKIP "leg 2: sibling InstanceTypes stay status-consistent" "no node carrying ${AKEY} advertises a *.sliced resource"
else
  BASE2=$(sig "$IT")
  if [ -z "$BASE2" ] || [ "$BASE2" = "|||" ]; then BASE2="$BASE_SIG"; fi

  for t in "$SIB_A" "$SIB_B"; do
    kubectl apply -f - >/dev/null 2>&1 <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: InstanceType
metadata:
  name: ${t}
spec:
  acceleratorGroup: ${AKEY}
  acceleratable: true
  os: ${OS}
  arch: ${ARCH}
  unitResources:
    cpu: "1"
    ram: "2Gi"
  localStorage: "100Gi"
EOF
  done
  ok=""; sa=""; sb=""; sd=""
  for _ in $(seq 1 40); do
    sa=$(sig "$SIB_A"); sb=$(sig "$SIB_B"); sd=$(sig "$IT")
    if [ -n "$sa" ] && [ "$sa" = "$sb" ] && [ "$sb" = "$sd" ]; then ok=1; break; fi
    sleep 3
  done
  if [ -n "$ok" ]; then
    record PASS "siblings share the pool status before load" "all three = ${sd} (EX|SH|SL|PT)"
  else
    record FAIL "siblings share the pool status before load" "derived=${sd:-?} a=${sa:-?} b=${sb:-?} — sibling types on one pool must show the same status"
  fi

  echo "[case-8] deploying 50% logical-slice Pod ${POD_LOAD} on ${LQ}"
  cat <<EOF | kubectl apply -f - >/dev/null 2>&1
apiVersion: v1
kind: Pod
metadata: { name: ${POD_LOAD}, namespace: default, labels: { kueue.x-k8s.io/queue-name: ${LQ} } }
spec:
  schedulerName: default-scheduler
  ${RTC_LINE}
  restartPolicy: Never
  containers:
    - name: main
      image: ${IMAGE}
      command: ["sleep", "86400"]
      resources:
        limits:   { ${POOL_SLICED}: "1", ${POOL_MEMPCT}: "50", ${POOL_CORESPCT}: "50" }
        requests: { ${POOL_SLICED}: "1", ${POOL_MEMPCT}: "50", ${POOL_CORESPCT}: "50" }
EOF
  running=""
  for _ in $(seq 1 50); do
    if [ "$(kubectl -n default get pod "$POD_LOAD" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Running" ]; then running=1; break; fi
    sleep 3
  done
  if [ -n "$running" ]; then
    record PASS "logical-slice workload admits + runs" "${POD_LOAD} Running (a slice is taken from the pool)"
  else
    record FAIL "logical-slice workload admits + runs" "${POD_LOAD} not Running — Kueue admission / node-devices AdmissionCheck / scheduling failed"
  fi

  moved=""
  for _ in $(seq 1 40); do
    sa=$(sig "$SIB_A"); sb=$(sig "$SIB_B"); sd=$(sig "$IT")
    if [ -n "$sd" ] && [ "$sd" = "$sa" ] && [ "$sa" = "$sb" ] && [ "$sd" != "$BASE2" ]; then moved=1; break; fi
    sleep 3
  done
  if [ -n "$moved" ]; then
    record PASS "ledger change re-enqueues all siblings consistently" "all three = ${sd} (moved from ${BASE2}, equal)"
  else
    record FAIL "ledger change re-enqueues all siblings consistently" "derived=${sd:-?} a=${sa:-?} b=${sb:-?} base=${BASE2} — a Devices change must refresh every type on the pool"
  fi

  echo "[case-8] releasing leg 2 load pod and sibling InstanceTypes"
  kubectl -n default delete pod "$POD_LOAD" --ignore-not-found --force --grace-period=0 >/dev/null 2>&1 || true
  cleanup_types
  wait_settled "$BASE2"
fi

# ===================================================================================================
# Leg 3 — Instance admission webhooks on a sliceable type.
# ===================================================================================================
echo
echo "[case-8] === leg 3: Instance admission webhooks on a sliceable type ==="

UNIT_OK=1
UNIT_WHY=""
case "${UNIT_CPU:-}" in
  ''|*[!0-9]*) UNIT_OK=0; UNIT_WHY="unit CPU '${UNIT_CPU:-}' is not an integer core count — cannot compute deterministic slice math";;
esac
case "${UNIT_RAM:-}" in
  *Gi) ;;
  *) UNIT_OK=0; UNIT_WHY="unit RAM '${UNIT_RAM:-}' is not in Gi — cannot compute deterministic slice math";;
esac

if [ "$UNIT_OK" -eq 0 ]; then
  record SKIP "leg 3: Instance admission webhooks on sliceable type" "${UNIT_WHY}"
else
  RAM_N="${UNIT_RAM%Gi}"
  cpu_pct() { local v=$(( UNIT_CPU * $1 / 100 )); [ "$v" -lt 1 ] && v=1; echo "$v"; }
  ram_pct() { echo "$(( RAM_N * $1 / 100 ))Gi"; }
  EXP_CPU_OK=$(cpu_pct 25);  EXP_RAM_OK=$(ram_pct 25)
  EXP_CPU_MEM=$(cpu_pct 50); EXP_RAM_MEM=$(ram_pct 50)

  mk_slice_inst() {
    {
      cat <<EOF
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata: { name: $1, namespace: default }
spec:
  type: ${IT}
  image: alpine
  command: ["sleep", "86400"]
  volume: { ephemeral: { capacity: 1Gi } }
  resources:
    acceleratorSlicedMemoryPercentage: $2
    acceleratorSlicedCoresPercentage: $3
EOF
      [ -n "${4:-}" ] && echo "    accelerator: \"$4\""
    } | kubectl apply -f - 2>&1
  }

  get_res() { kubectl -n default get instance "$1" -o jsonpath="{.spec.resources.$2}" 2>/dev/null; }

  echo "[case-8] Q1 apply: $(mk_slice_inst "$INST_OK" 25 25)"
  acc=""; for _ in $(seq 1 10); do acc=$(get_res "$INST_OK" accelerator); [ -n "$acc" ] && break; sleep 1; done
  cpu=$(get_res "$INST_OK" cpu); ram=$(get_res "$INST_OK" ram)
  if [ "$acc" = "1" ] && [ "$cpu" = "$EXP_CPU_OK" ] && [ "$ram" = "$EXP_RAM_OK" ]; then
    record PASS "Default scales slice to unit CPU/RAM, accelerator=1" "acc=${acc} cpu=${cpu} ram=${ram} (unit ${UNIT_CPU}/${UNIT_RAM} x 25%)"
  else
    record FAIL "Default scales slice to unit CPU/RAM, accelerator=1" "acc='${acc:-?}' cpu='${cpu:-?}' ram='${ram:-?}', want 1/${EXP_CPU_OK}/${EXP_RAM_OK}"
  fi

  echo "[case-8] Q2 apply: $(mk_slice_inst "$INST_MEMONLY" 50 0)"
  cores2=""; for _ in $(seq 1 10); do cores2=$(get_res "$INST_MEMONLY" acceleratorSlicedCoresPercentage); [ -n "$cores2" ] && break; sleep 1; done
  cpu2=$(get_res "$INST_MEMONLY" cpu); ram2=$(get_res "$INST_MEMONLY" ram)
  if [ "$cores2" = "50" ] && [ "$cpu2" = "$EXP_CPU_MEM" ] && [ "$ram2" = "$EXP_RAM_MEM" ]; then
    record PASS "lone memory % mirrored to compute, sizes CPU" "cores%=${cores2} cpu=${cpu2} ram=${ram2}"
  else
    record FAIL "lone memory % mirrored to compute, sizes CPU" "cores%='${cores2:-?}' cpu='${cpu2:-?}' ram='${ram2:-?}', want 50/${EXP_CPU_MEM}/${EXP_RAM_MEM}"
  fi

  Q3_MSG='accelerator request must be exactly 1 for a sliced request'
  err=$(mk_slice_inst "$INST_BAD" 25 25 2); rc=$?
  if [ "$rc" -ne 0 ] && echo "$err" | grep -qF "$Q3_MSG"; then
    record PASS "Validate rejects accelerator != 1 on sliceable" "rejected: $(echo "$err" | grep -oF "$Q3_MSG" | head -1)"
  elif [ "$rc" -ne 0 ]; then
    record FAIL "Validate rejects accelerator != 1 on sliceable" "rejected, but not with '${Q3_MSG}': $(echo "$err" | head -1)"
  else
    kubectl -n default delete instance "$INST_BAD" --ignore-not-found >/dev/null 2>&1 || true
    record FAIL "Validate rejects accelerator != 1 on sliceable" "accepted (rc=${rc}) — sliceable accelerator must be 1"
  fi

  kubectl -n default delete instance "$INST_OK" "$INST_MEMONLY" "$INST_BAD" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  wait_settled "$BASE_SIG"
fi

# ===================================================================================================
# Leg 4 — Per-card logical-slice accounting & packing (requires >=2 logically sliceable NVIDIA cards).
# ===================================================================================================
echo
echo "[case-8] === leg 4: per-card logical-slice accounting & packing (>=2 NVIDIA cards) ==="

read -r PACK_NODE PACK_NCARDS PACK_GROUPID <<<"$(kubectl get devices -o json 2>/dev/null | python3 -c "
import json,sys
best=('',0,'')
for d in json.load(sys.stdin).get('items',[]):
    for g in d.get('spec',{}).get('groups',[]):
        if g.get('manufacturer')=='nvidia':
            n=sum(1 for a in g.get('accelerators',[])
                  if (a.get('status',{}).get('logicalSliced',{}).get('count',0) or 0)>0)
            if n>best[1]: best=(d['metadata']['name'], n, g.get('id',''))
print(best[0], best[1], best[2])
" 2>/dev/null)"

if [ -z "${PACK_NODE:-}" ] || [ "${PACK_NCARDS:-0}" -lt 2 ]; then
  record SKIP "leg 4: per-card logical-slice packing & over-commit guard" \
    "no node reports >=2 logically sliceable nvidia cards (best='${PACK_NODE:-none}', cards=${PACK_NCARDS:-0})"
else
  PACK_N="$PACK_NCARDS"; [ "$PACK_N" -gt 4 ] && PACK_N=4
  read -r PACK_IT PACK_LQ PACK_CARDMEM PACK_MANUF <<<"$(discover_node_pool "$PACK_NODE" "$PACK_GROUPID" --sliceable name entrance memory manufacturer)"
  PACK_PHYS_MIB=$(python3 -c "
import re
m=re.match(r'\s*(\d+)\s*([GM])i?', '${PACK_CARDMEM:-}')
print(int(m.group(1))*(1024 if m.group(2)=='G' else 1) if m else 0)
" 2>/dev/null)
  if [ -z "${PACK_IT:-}" ] || [ -z "${PACK_LQ:-}" ] || [ "${PACK_PHYS_MIB:-0}" -le 0 ]; then
    record FAIL "leg 4: per-card logical-slice packing setup" \
      "pool='${PACK_IT:-<none>}' lq='${PACK_LQ:-<none>}' cardmem='${PACK_CARDMEM:-<empty>}' (${PACK_PHYS_MIB:-0}MiB)"
  else
    PACK_RTC=""
    if [ -n "$PACK_MANUF" ] && kubectl get runtimeclass.node.k8s.io "$PACK_MANUF" >/dev/null 2>&1; then
      PACK_RTC="runtimeClassName: ${PACK_MANUF}"
    fi
    SLICE_ROWS=()

    submit_pack_slices() { # submit_pack_slices <prefix> <first-index> <pct>...
      local prefix="$1" idx="$2"; shift 2
      local pct ok u m
      for pct in "$@"; do
        cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: { name: ${prefix}-${idx}, namespace: default, labels: { kueue.x-k8s.io/queue-name: ${PACK_LQ} } }
spec:
  schedulerName: default-scheduler
  ${PACK_RTC}
  restartPolicy: Never
  nodeSelector: { kubernetes.io/hostname: ${PACK_NODE} }
  containers:
    - name: main
      image: ${IMAGE}
      command: ["sleep", "86400"]
      resources:
        limits:   { nvidia.com/gpu.sliced: "1", nvidia.com/gpu.sliced.cores-percentage: "${pct}", nvidia.com/gpu.sliced.memory-percentage: "${pct}" }
        requests: { nvidia.com/gpu.sliced: "1", nvidia.com/gpu.sliced.cores-percentage: "${pct}", nvidia.com/gpu.sliced.memory-percentage: "${pct}" }
EOF
        ok=""
        for _ in $(seq 1 40); do
          [ "$(kubectl -n default get pod "${prefix}-${idx}" -o jsonpath='{.status.phase}' 2>/dev/null)" = Running ] && { ok=1; break; }
          sleep 3
        done
        [ -n "$ok" ] || { echo "[case-8] ${prefix}-${idx} (${pct}%) never reached Running"; return 1; }
        u=""; m=""
        for _ in $(seq 1 8); do
          u=$(kubectl -n default exec "${prefix}-${idx}" -- printenv NVIDIA_VISIBLE_DEVICES 2>/dev/null)
          m=$(kubectl -n default exec "${prefix}-${idx}" -- printenv CUDA_DEVICE_MEMORY_LIMIT_0 2>/dev/null)
          [ -n "$u" ] && [ -n "$m" ] && break
          sleep 3
        done
        if [ -z "$u" ] || [ -z "$m" ]; then echo "[case-8] ${prefix}-${idx} (${pct}%): unreadable placement"; return 1; fi
        SLICE_ROWS+=("${u} ${m%m}")
        echo "[case-8] ${prefix}-${idx} (${pct}%) -> ${u}"
        idx=$((idx + 1))
        sleep 6
      done
      return 0
    }

    card_of() { awk '{print $1}' <<<"${SLICE_ROWS[$(($1 - 1))]}"; }
    distinct_cards() {
      [ "${#SLICE_ROWS[@]}" -eq 0 ] && { echo 0; return; }
      printf '%s\n' "${SLICE_ROWS[@]}" | awk '{print $1}' | sort -u | grep -c .
    }
    overcommitted_cards() {
      [ "${#SLICE_ROWS[@]}" -eq 0 ] && { echo 0; return; }
      printf '%s\n' "${SLICE_ROWS[@]}" | PHYS="${PACK_PHYS_MIB}" python3 -c "
import sys,os,collections
phys=int(os.environ.get('PHYS','0')); agg=collections.defaultdict(int)
for line in sys.stdin:
    p=line.split()
    if len(p)<2: continue
    try: agg[p[0]]+=int(p[1])
    except: pass
print(sum(1 for s in agg.values() if phys>0 and s>phys))
"
    }
    settle_pack_ledger() {
      local srem scap
      for _ in $(seq 1 20); do
        srem=$(kubectl get instancetypes.worker.gpustack.ai "$PACK_IT" -o jsonpath='{.status.acceleratorSliced.remaining}' 2>/dev/null)
        scap=$(kubectl get instancetypes.worker.gpustack.ai "$PACK_IT" -o jsonpath='{.status.acceleratorSliced.capacity}' 2>/dev/null)
        [ -n "$srem" ] && [ "$srem" = "$scap" ] && return 0
        sleep 3
      done
      return 1
    }

    # Pass 1: claims that cannot share a card each take one of their own.
    echo "[case-8] leg 4 pass 1 — ${PACK_N} x ${SPREADPCT}% on ${PACK_NODE}"
    SPREADPCTS=()
    for _ in $(seq 1 "$PACK_N"); do SPREADPCTS+=("$SPREADPCT"); done
    SLICE_ROWS=()
    if submit_pack_slices "$SPREADPFX" 1 "${SPREADPCTS[@]}"; then
      uniq_n=$(distinct_cards)
      over_n=$(overcommitted_cards)
      rc=0; [ "${uniq_n:-0}" -eq "$PACK_N" ] || rc=1
      verdict "$rc" "claims too large to share take distinct cards" \
        "${PACK_N} x ${SPREADPCT}% -> ${uniq_n} distinct card(s)" \
        "${PACK_N} x ${SPREADPCT}% -> only ${uniq_n:-?} distinct card(s) — two slices >50% cannot share a card's VRAM"
      rc=0; [ "${over_n:-1}" -eq 0 ] || rc=1
      verdict "$rc" "no card over-committed (spread pass)" \
        "0 of ${uniq_n} card(s) above ${PACK_PHYS_MIB}MiB" \
        "${over_n} card(s) above ${PACK_PHYS_MIB}MiB — the per-card guard did not hold"
    else
      record FAIL "claims too large to share take distinct cards" "a ${SPREADPCT}% slice could not be placed and read back"
      record FAIL "no card over-committed (spread pass)" "not evaluated — the spread pass did not complete"
    fi

    delete_indexed_pods "$SPREADPFX" 1 2 3 4
    settle_pack_ledger || echo "[case-8] WARNING: SL view did not return to fully free before packing pass"

    # Pass 2: 100% then 50%, delete 100%, submit 30% -> must join 50% card.
    echo "[case-8] leg 4 pass 2 — 100% then 50%, free 100%, submit 30%"
    SLICE_ROWS=()
    if submit_pack_slices "$PACKPFX" 1 100 50; then
      block_card=$(card_of 1)
      host_card=$(card_of 2)
      rc=0; [ "$host_card" != "$block_card" ] || rc=1
      verdict "$rc" "a claim does not land on a card already filled" \
        "50% -> ${host_card}, not the 100%-filled ${block_card}" \
        "50% -> ${host_card} — the same card the 100% claim filled, so the card is over-committed"

      delete_indexed_pods "$PACKPFX" 1
      SLICE_ROWS=("${SLICE_ROWS[1]}")
      sleep 12

      if submit_pack_slices "$PACKPFX" 3 30; then
        join_card=$(card_of 2)
        rc=0; [ "$join_card" = "$host_card" ] || rc=1
        verdict "$rc" "a claim that fits joins the card already in use" \
          "30% -> ${join_card}, joining the 50% claim and leaving the freed card whole" \
          "30% -> ${join_card} but the 50% claim holds ${host_card} — opening another card burns one"
        over_p=$(overcommitted_cards)
        rc=0; [ "${over_p:-1}" -eq 0 ] || rc=1
        verdict "$rc" "no card over-committed (packing pass)" \
          "0 card(s) above ${PACK_PHYS_MIB}MiB (50% + 30% = 80%)" \
          "${over_p} card(s) above ${PACK_PHYS_MIB}MiB — packing must respect the per-card budget"
      else
        record FAIL "a claim that fits joins the card already in use" "the 30% join claim could not be placed and read back"
        record FAIL "no card over-committed (packing pass)" "not evaluated — the join claim did not complete"
      fi

      srem=$(kubectl get instancetypes.worker.gpustack.ai "$PACK_IT" -o jsonpath='{.status.acceleratorSliced.remaining}' 2>/dev/null)
      scap=$(kubectl get instancetypes.worker.gpustack.ai "$PACK_IT" -o jsonpath='{.status.acceleratorSliced.capacity}' 2>/dev/null)
      rc=0; { [ -n "$srem" ] && [ -n "$scap" ] && [ "$srem" -lt "$scap" ]; } || rc=1
      verdict "$rc" "SL view reflects occupancy" \
        "acceleratorSliced remaining ${srem} < capacity ${scap}" \
        "remaining='${srem:-?}' not < capacity='${scap:-?}' (ledger not units-accurate)"
    else
      record FAIL "a claim does not land on a card already filled" "the 100%/50% claims could not be placed and read back"
      record FAIL "a claim that fits joins the card already in use" "not evaluated — the packing pass did not start"
      record FAIL "no card over-committed (packing pass)" "not evaluated — the packing pass did not start"
      record FAIL "SL view reflects occupancy" "not evaluated — the packing pass did not start"
    fi

    sorm=$(kubectl get instancetypes.worker.gpustack.ai "$PACK_IT" -o jsonpath='{.status.acceleratorSliced.onceMaxRequest}' 2>/dev/null)
    rc=0; { [ -n "$sorm" ] && [ "$sorm" -le 100 ]; } || rc=1
    verdict "$rc" "SL OnceMaxRequest is per-card (<=100)" \
      "acceleratorSliced onceMaxRequest ${sorm}" \
      "onceMaxRequest='${sorm:-?}' > 100 (node card-sum, not per-card)"

    delete_indexed_pods "$PACKPFX" 1 2 3
    settle_pack_ledger || true
  fi
fi

# ===================================================================================================
# Leg 5 — Single-card co-residency, over-budget refusal, self-eviction guard and per-slice metrics.
# ===================================================================================================
echo
echo "[case-8] === leg 5: single-card co-residency, over-budget refusal and per-slice metrics ==="

COEXIST_IT=$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null | python3 -c "
import json,sys
for it in json.load(sys.stdin).get('items',[]):
    if not it.get('spec',{}).get('acceleratable'): continue
    if (it.get('status',{}).get('detail',{}) or {}).get('manufacturer') != 'nvidia': continue
    sl=(it.get('status',{}).get('acceleratorSliced') or {})
    cap=int(sl.get('capacity') or 0)
    if 0 < cap <= 100:
        print(it['metadata']['name']); break
")

if [ -z "${COEXIST_IT:-}" ]; then
  record SKIP "leg 5: single-card co-residency, over-budget refusal and per-slice metrics" \
    "no single-card logically sliceable NVIDIA InstanceType (acceleratorSliced.capacity in 1..100); multi-card accounting is covered by leg 4"
else
  echo "[case-8] leg 5 target: single-card logically sliceable NVIDIA pool ${COEXIST_IT}"

  mk_coexist() { # mk_coexist <name> <mem-pct> [image] [command-json]
    cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: Instance
metadata: { name: $1, namespace: default }
spec:
  type: ${COEXIST_IT}
  image: ${3:-ubuntu:24.04}
  command: ${4:-[\"sleep\", \"infinity\"]}
  resources:
    cpu: "1"
    ram: "4Gi"
    localStorage: "10Gi"
    accelerator: "1"
    acceleratorSlicedMemoryPercentage: $2
    acceleratorSlicedCoresPercentage: 100
  volume: { ephemeral: { capacity: 5Gi } }
  volumeMount: /workspace
EOF
  }

  visible_card() {
    local out
    for _ in $(seq 1 8); do
      out=$(kubectl -n default exec "$1" -c main -- printenv NVIDIA_VISIBLE_DEVICES 2>/dev/null)
      [ -n "$out" ] && { echo "$out"; return 0; }
      sleep 3
    done
    return 1
  }

  if [ -n "$LOAD_IMAGE" ]; then
    echo "[case-8] ${COEXIST_A} carries a device-memory load from ${LOAD_IMAGE}"
    mk_coexist "$COEXIST_A" 40 "$LOAD_IMAGE" "$LOAD_COMMAND"
  else
    mk_coexist "$COEXIST_A" 40
  fi
  mk_coexist "$COEXIST_B" 40
  a_ok=1; wait_running "$COEXIST_A" || a_ok=0
  b_ok=1; wait_running "$COEXIST_B" || b_ok=0
  if [ "$a_ok" = 1 ] && [ "$b_ok" = 1 ]; then
    ca=$(visible_card "$COEXIST_A")
    cb=$(visible_card "$COEXIST_B")
    if [ -n "$ca" ] && [ "$ca" = "$cb" ]; then
      record PASS "two 40% slices coexist on one card" "${COEXIST_A} + ${COEXIST_B} both Running on ${ca} (combined 80% <= 100%)"
    else
      record FAIL "two 40% slices coexist on one card" "A@'${ca:-<unreadable>}' B@'${cb:-<unreadable>}' — two slices within one card's budget must share that card"
    fi
  else
    record FAIL "two 40% slices coexist on one card" "A running=${a_ok} B running=${b_ok} — both should admit within one card"
  fi

  mk_coexist "$COEXIST_C" 40
  held=1
  for _ in $(seq 1 8); do
    if [ "$(kubectl -n default get pod "$COEXIST_C" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Running" ]; then
      held=0; break
    fi
    sleep 3
  done
  if [ "$held" = 1 ]; then
    ph=$(kubectl -n default get pod "$COEXIST_C" -o jsonpath='{.status.phase}' 2>/dev/null)
    record PASS "over-budget slice is held (not over-admitted)" "${COEXIST_C} not Running (phase='${ph:-<no pod>}'; 120% > one card)"
  else
    record FAIL "over-budget slice is held (not over-admitted)" "${COEXIST_C} Running — three 40% slices over-admitted one card"
  fi

  sleep 10
  ra=$(kubectl -n default get pod "$COEXIST_A" -o jsonpath='{.status.phase}' 2>/dev/null)
  rb=$(kubectl -n default get pod "$COEXIST_B" -o jsonpath='{.status.phase}' 2>/dev/null)
  if [ "$ra" = "Running" ] && [ "$rb" = "Running" ]; then
    record PASS "A and B stay admitted after C's rejection" "${COEXIST_A}=${ra} ${COEXIST_B}=${rb} — no self-eviction on sibling re-evaluation"
  else
    record FAIL "A and B stay admitted after C's rejection" "${COEXIST_A}=${ra:-?} ${COEXIST_B}=${rb:-?} — a sibling's admission attempt evicted an already-running slice"
  fi

  accel_json() {
    kubectl get --raw \
      "/apis/worker.gpustack.ai/v1/namespaces/default/instances/$1/metrics" 2>/dev/null |
      python3 -c '
import json, sys
try:
    s = json.load(sys.stdin).get("sample", {})
except Exception:
    sys.exit(0)
acc = (s.get("accelerators") or [None])[0]
if acc is None:
    sys.exit(0)
print(json.dumps({
    "id": acc.get("id"),
    "mode": acc.get("mode"),
    "totalMiB": acc.get("memoryTotalMiB"),
    "usedMiB": acc.get("memoryUsedMiB"),
    "utilPct": acc.get("memoryUtilizationPercent"),
    "coresPct": acc.get("coresUtilizationPercent"),
}))' 2>/dev/null
  }

  sa=""; sb=""
  for _ in $(seq 1 12); do
    sa=$(accel_json "$COEXIST_A")
    sb=$(accel_json "$COEXIST_B")
    [ -n "$sa" ] && [ -n "$sb" ] && break
    sleep 5
  done

  if [ -z "$sa" ] || [ -z "$sb" ]; then
    record FAIL "each slice reports its own share" \
      "no accelerator entry served for A ($([ -n "$sa" ] && echo present || echo absent)) or B ($([ -n "$sb" ] && echo present || echo absent))"
  else
    mverdict=$(LOADED="$LOAD_IMAGE" python3 -c '
import json, os, sys

a, b = json.loads(sys.argv[1]), json.loads(sys.argv[2])
loaded = bool(os.environ.get("LOADED"))
bad = []

for name, s in (("A", a), ("B", b)):
    if s["mode"] != "Sliced":
        bad.append("%s mode=%r" % (name, s["mode"]))
    if not s["totalMiB"]:
        bad.append("%s carries no memory grant" % name)
    if s["usedMiB"] is not None and s["totalMiB"]:
        want = int(s["usedMiB"] * 100 / s["totalMiB"])
        if s["utilPct"] != want:
            bad.append("%s utilization=%r, but %s/%s is %d" % (
                name, s["utilPct"], s["usedMiB"], s["totalMiB"], want))
    elif s["utilPct"] is not None:
        bad.append("%s states a utilization with nothing measured" % name)

if a["totalMiB"] and b["totalMiB"] and a["totalMiB"] != b["totalMiB"]:
    bad.append("A and B asked for the same share but were granted %s and %s MiB" % (
        a["totalMiB"], b["totalMiB"]))

if loaded:
    if not a["usedMiB"]:
        bad.append("A holds a workload but its usedMiB is %r" % (a["usedMiB"],))
    if a["usedMiB"] and a["usedMiB"] == b["usedMiB"]:
        bad.append("A and B report the same usedMiB (%s) though only A is loaded" % a["usedMiB"])

print(("FAIL " + "; ".join(bad)) if bad else "PASS")
' "$sa" "$sb" 2>/dev/null)
    case "$mverdict" in
      PASS) record PASS "each slice reports its own share" \
        "A=${sa} B=${sb}$([ -n "$LOAD_IMAGE" ] && echo ' (A loaded, B idle)' || echo ' (both idle: structure only)')" ;;
      FAIL*) record FAIL "each slice reports its own share" "${mverdict#FAIL }" ;;
      *) record FAIL "each slice reports its own share" "could not compare (python3 missing?)" ;;
    esac
  fi

  kubectl -n default delete instance "$COEXIST_A" "$COEXIST_B" "$COEXIST_C" --ignore-not-found --wait=false >/dev/null 2>&1 || true
fi

echo
echo "== CASE 8 — Real NVIDIA logical slicing: runtime isolation, per-card packing, co-residency, over-commit refusal and slice metrics =="
print_rows

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). Diagnose: kubectl -n default describe pod ${POD_PCT};"
  echo "kubectl -n ${NS} logs ds/gpustack-operator-device-manager-${RUNTIME_MANUF} --tail=100;"
  echo "kubectl -n ${NS} logs deploy/gpustack-operator-worker --tail=200"
  exit 1
fi
echo "CASE 8 PASS"
