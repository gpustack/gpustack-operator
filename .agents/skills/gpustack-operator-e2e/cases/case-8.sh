#!/usr/bin/env bash
#
# CASE 8 — Real accelerator slicing: runtime isolation, slice webhook and sibling-pool re-enqueue   (MUTATING, self-recovering; AUTO-SKIPS without real sliced hardware)
#
#   case-8.sh <NS>
#
# Prerequisites: nodes=1; gpu=any; cluster=real; inputs=E2E_SLICE_BASE,E2E_SLICE_IMAGE,E2E_SLICE_MANUF,E2E_SLICE_MEM_ENV,E2E_SLICE_MEM_ENV_UNIT,E2E_SLICE_SMI,E2E_SLICE_SMI_MEM_RE,E2E_SLICE_SM_ENV
# Goal:        On one real logically sliceable accelerator pool, three contracts hold at once —
#              1. a sliced workload is capped at runtime to its share of a real card — by memory
#                 percentage, by memory MiB and by compute (SM) percentage — through the vendor's
#                 slicing runtime: the Pod webhook folds .sliced.memory-percentage /
#                 .sliced.memory-mib into .sliced.units, Kueue admits and the node-devices
#                 AdmissionCheck reads the real Devices ledger, the device plugin allocates a real
#                 card and injects the runtime cap (the memory-limit and SM-limit variables);
#              2. the Instance admission webhooks size a slice of a sliceable InstanceType: the
#                 compute percentage sizes CPU, the memory percentage sizes RAM, and the
#                 accelerator count is pinned to 1 (a request for more than one card is rejected);
#              3. every InstanceType that shares the pool's os / arch / acceleratorGroup /
#                 acceleratable keeps one identical four-view status when the Devices ledger moves,
#                 because the enqueue on a Devices change lists all of them.
# Environment: Needs REAL accelerator hardware advertising a *.sliced resource whose cards are
#              LOGICALLY sliceable (a hardware-partitioned card serves no logical slice), and a
#              logically sliceable accelerated InstanceType with an entrance LocalQueue.
#              AUTO-SKIPS (exit 0, prints why) when no node advertises a *.sliced resource — the
#              runtime cap cannot be mocked. Defaults to NVIDIA (HAMi libvgpu). Another vendor
#              overrides the resource base key, the in-container manufacturer tool and how its
#              output is read, the cap variable names and the claim carrier image — see the
#              vendor-dimension assignments below.
# Inputs:      All real, nothing mocked —
#              - POD_PCT: .sliced=1 + memory-%=50 + cores-%=50, and POD_MIB: .sliced=1 +
#                memory-mib=4096, on the sliceable pool's entrance LocalQueue;
#              - POD_LOAD: one 50% slice on the entrance queue — the workload that moves the Devices
#                ledger while the sibling InstanceTypes watch it;
#              - two throwaway admin InstanceTypes e2e-case8-sib-a / e2e-case8-sib-b declaring the
#                pool's acceleratorGroup/os/arch (plus a unit spec);
#              - three probe Instances (mem%=25 cores%=25; mem%=50 cores unset; mem%=25 cores%=25
#                with accelerator=2).
# Expected:    - both slice Pods reach Running (webhook fold → Kueue → AdmissionCheck → schedule → device plugin);
#              - POD_PCT: the SM-limit variable reads 50 and the tool's total memory < physical card;
#              - POD_MIB: the injected memory cap / the tool's total == 4096 MiB;
#              - the probe Instances persist accelerator=1 with CPU/RAM sized by the slice
#                percentages, and the two-card request is rejected;
#              - the sibling InstanceTypes report the same four-view status before and after the
#                load, and the status moves with the ledger.
# Cleanup:     Trap deletes both slice Pods, the load Pod, the three probe Instances and the two
#              throwaway InstanceTypes (with their ClusterQueues), then waits for the pool's
#              four-view status to return to its pre-run value. Each section also releases what it
#              created before the next section starts, so one section's workload never starves the
#              next. Idempotent, runs on pass AND fail, safe to re-run.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

NS="${1:?usage: case-8.sh <NS>}"
# --- Vendor dimensions. Every one defaults to NVIDIA, so an NVIDIA run needs nothing set. ---
# The manufacturer. It selects the pool and the runtimeClass, so it has to agree with the resource
# family below — they are the same vendor named two ways, and a run that mixes them would request one
# vendor's tokens against another vendor's queue.
MANUF="${E2E_SLICE_MANUF:-nvidia}"
# The resource family.
BASE="${E2E_SLICE_BASE:-nvidia.com/gpu}"
SLICED=${BASE}.sliced
CORESPCT=${BASE}.sliced.cores-percentage
MEMPCT=${BASE}.sliced.memory-percentage
MEMMIB=${BASE}.sliced.memory-mib
# The in-container manufacturer tool, and the regex that lifts the card's TOTAL memory in MiB out of
# its output (first match; the digits are taken from it).
SMI="${E2E_SLICE_SMI:-nvidia-smi}"
SMI_MEM_RE="${E2E_SLICE_SMI_MEM_RE:-/[[:space:]]*[0-9]+MiB}"
# The variables the slicing runtime injects to enforce the cap, and the unit the memory one carries.
SM_ENV="${E2E_SLICE_SM_ENV:-CUDA_DEVICE_SM_LIMIT}"
MEM_ENV="${E2E_SLICE_MEM_ENV:-CUDA_DEVICE_MEMORY_LIMIT_0}"
MEM_ENV_UNIT="${E2E_SLICE_MEM_ENV_UNIT:-m}"
# The claim carrier. A bare base image is enough where the runtimeClass mounts the driver libs; a
# vendor whose slicing runtime needs its userspace libs in the image overrides it.
IMAGE="${E2E_SLICE_IMAGE:-ubuntu:24.04}"
# --- The objects this case creates. Every one is deleted by the trap. ---
POD_PCT=gpustack-e2e-slice-pct
POD_MIB=gpustack-e2e-slice-mib
POD_LOAD=gpustack-e2e-case8-load
SIB_A=e2e-case8-sib-a
SIB_B=e2e-case8-sib-b
INST_OK=gpustack-e2e-slice-ok
INST_MEMONLY=gpustack-e2e-slice-memonly
INST_BAD=gpustack-e2e-slice-badacc
MIB_REQ=4096   # fits a 16Gi (T4) or 24Gi (A10G) card
# The pool every section works on, filled by the discovery below.
IT=""; LQ=""; CARDMEM=""; UNIT_CPU=""; UNIT_RAM=""
AKEY=""; OS=""; ARCH=""; POOL_MANUF=""; SLICED_CAP=""; BASE_SIG=""

# sig <instancetype> — the pool's four-view remaining signature (EX|SH|SL|PT), or empty.
sig() {
  kubectl get instancetype "$1" -o jsonpath='{.status.accelerator.remaining}|{.status.acceleratorShared.remaining}|{.status.acceleratorSliced.remaining}|{.status.acceleratorPartitioned.remaining}' 2>/dev/null
}

# wait_settled <baseline> — wait for the pool's four-view status to return to the baseline.
#
# The pool's room is shared: a section's workload holds a slice until it is released, so the next
# section must start on a pool that gave it back. A pool that never settles is reported, because
# the next section's workload then competes for room the first one still holds.
wait_settled() {
  local base="$1" now=""
  for _ in $(seq 1 30); do
    now=$(sig "$IT")
    if [ "$now" = "$base" ]; then return 0; fi
    sleep 3
  done
  echo "[case-8] warning: the pool did not return to ${base} within 90s (now ${now:-?})"
  return 0
}

# wait_running <pod> — poll until the Pod is Running (admitted by Kueue + scheduled), else empty.
wait_running() {
  for _ in $(seq 1 40); do
    [ "$(kubectl -n default get pod "$1" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Running" ] && return 0
    sleep 3
  done
  return 1
}

# exec_out <pod> <cmd>... — run a command in the Pod and echo its output, retried.
#
# A Pod reports Running slightly before its container is attachable, so an exec issued the instant
# wait_running returns can come back empty — or fail outright with `container not found` — while the
# slice is applied perfectly correctly. Every read below feeds a PASS/FAIL verdict, so without this
# the case records a product defect for what is only a timing artifact.
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

# cleanup_types — drop the throwaway sibling InstanceTypes and the ClusterQueues the operator
# derives from them. Best effort: a type that is already gone is not an error.
cleanup_types() {
  for t in "$SIB_A" "$SIB_B"; do
    kubectl patch instancetype "$t" --type=merge -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
    kubectl delete instancetype "$t" --wait=false >/dev/null 2>&1 || true
    kubectl delete clusterqueue "$t" --wait=false >/dev/null 2>&1 || true
  done
}

restore() {
  # Nothing was created before the pool was found, so there is nothing to restore.
  [ -n "${IT:-}" ] || return 0
  echo
  echo "[case-8] cleanup: deleting the test Pods, Instances and InstanceTypes"
  kubectl -n default delete pod "$POD_PCT" "$POD_MIB" "$POD_LOAD" --ignore-not-found --force --grace-period=0 2>/dev/null || true
  kubectl -n default delete instance "$INST_OK" "$INST_MEMONLY" "$INST_BAD" --ignore-not-found 2>/dev/null || true
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

# --- The pool every section works on: one logically sliceable accelerated InstanceType, its
#     entrance LocalQueue, its per-card memory, its per-card unit spec and the identity its
#     siblings declare.
#
# The requested manufacturer is preferred, not required: on a multi-vendor cluster the first
# sliceable pool need not be the vendor whose resource this run requests, and every figure taken
# from it — the entrance queue, the accelerator memory, the runtimeClass — would then describe
#     different hardware from the `${BASE}.sliced` tokens being claimed. The manufacturer is a
#     parameter rather than a value read back out of whichever pool matched, because reading it back
#     is what allowed the mismatch. A pool of another manufacturer stays in the list as the
#     fallback: the sections that name no vendor can still use it.
IFS='|' read -r IT LQ CARDMEM UNIT_CPU UNIT_RAM AKEY OS ARCH POOL_MANUF SLICED_CAP <<<"$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null | MANUF="$MANUF" python3 -c "
import json,os,sys
manuf=os.environ['MANUF']
cands=[]
for it in json.load(sys.stdin).get('items',[]):
    s=it.get('spec',{}); st=it.get('status',{}); d=st.get('detail',{}); sd=d.get('slicedDetail',{}); u=s.get('unitResources',{})
    # LOGICALLY sliceable only: a hardware-partitioned card serves no logical slice.
    sliceable=(sd.get('logical',{}).get('count',0) or 0)>0
    if not (s.get('acceleratable') and sliceable): continue
    if it['metadata']['name'].startswith('e2e-case8-sib'): continue  # this run's own throwaways
    if not st.get('entrance'): continue
    cands.append([it['metadata']['name'], st.get('entrance',''), d.get('memory',''),
                  u.get('cpu',''), u.get('ram',''), s.get('acceleratorGroup',''),
                  s.get('os',''), s.get('arch',''), d.get('manufacturer',''),
                  st.get('acceleratorSliced',{}).get('capacity','')])
# The vendor this run asks for first; another vendor's pool is the fallback.
cands.sort(key=lambda c: 0 if c[8]==manuf else 1)
if cands: print('|'.join(cands[0]))
")"
if [ -z "${IT:-}" ] || [ -z "${LQ:-}" ]; then
  echo "no logically sliceable ${MANUF} InstanceType with an entrance LocalQueue found"
  exit 1
fi
echo "[case-8] sliceable InstanceType ${IT} (card memory ${CARDMEM}, unit cpu=${UNIT_CPU} ram=${UNIT_RAM}) via LocalQueue ${LQ}"

# Physical card memory in MiB (from the InstanceType card memory, e.g. "24Gi") for the below-physical
# assertion; 0 if it cannot be parsed.
PHYS_MIB=$(python3 -c "
import re
m = re.match(r'\s*(\d+)\s*([GM])i?', '${CARDMEM}')
print(int(m.group(1)) * (1024 if m.group(2) == 'G' else 1) if m else 0)
" 2>/dev/null)
echo "[case-8] card memory ${CARDMEM} = ${PHYS_MIB}MiB"

# The logical-slicing runtime (HAMi libvgpu.so, LD_PRELOAD-injected at Allocate) needs the vendor
# runtimeClass to mount the driver libs it depends on — a bare image without it exits 127. The
# operator's Instance controller injects this automatically; a raw Pod must set it too. Derive it
# from the pool's manufacturer (identity map: nvidia->nvidia, mthreads->mthreads), falling back to
# the requested one when the pool does not name a manufacturer.
RUNTIME_MANUF="${POOL_MANUF:-$MANUF}"
RUNTIMECLASS=""
if [ -n "$RUNTIME_MANUF" ] && kubectl get runtimeclass.node.k8s.io "$RUNTIME_MANUF" >/dev/null 2>&1; then RUNTIMECLASS="$RUNTIME_MANUF"; fi
RTC_LINE=""; [ -n "$RUNTIMECLASS" ] && RTC_LINE="runtimeClassName: ${RUNTIMECLASS}"
echo "[case-8] slice pods runtimeClass: ${RUNTIMECLASS:-<none>}"

# The pool's four-view status before anything runs. Every section returns it to this value before
# the next one starts.
BASE_SIG=$(sig "$IT")
if [ -z "$BASE_SIG" ] || [ "$BASE_SIG" = "|||" ]; then
  echo "[case-8] the pool ${IT} has no four-view status yet"
  exit 1
fi
echo "[case-8] baseline four-view (EX|SH|SL|PT) on ${IT} = ${BASE_SIG}"

# --- Section 1: the runtime isolation of a real slice (percentage, then MiB). ---
#
# The resource keys, the manufacturer tool and the cap variables are vendor-specific, so this
# section runs only on a pool of the vendor this run asks for. On a pool of another vendor the
# other two sections still run, and this one says why it stepped aside.
echo "[case-8] section 1: the real slice runtime isolation (percentage, then MiB)"

# The two manifests, built before the vendor gate: building them touches nothing.
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
  echo "[case-8] section 1 skipped: the pool ${IT} reports manufacturer '${POOL_MANUF:-<none>}' and this run asks for ${MANUF} — the resource keys, the tool and the cap variables are vendor-specific (set E2E_SLICE_MANUF and E2E_SLICE_BASE to run it)"
else
  # 1. Percentage slice: 50% memory + 50% compute. Expect the HAMI cap SM_LIMIT=50 and a memory limit
  #    below the physical card.
  echo "[case-8] creating ${POD_PCT}: ${SLICED}=1 memory-%=50 cores-%=50 on ${LQ}"
  printf '%s\n' "$POD_PCT_MANIFEST" | kubectl apply -f - >/dev/null
  if wait_running "$POD_PCT"; then
    record PASS "percentage slice admitted+running" "${POD_PCT} Running (webhook fold → Kueue → AdmissionCheck → schedule)"
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

  # 2. MiB slice: exactly ${MIB_REQ} MiB. Expect the cap to be that value.
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

  # Release this section's slices before the next section starts: the pool's room is shared, and
  # the next section's workload must fit beside what is still running.
  echo "[case-8] releasing the slice pods before the next section"
  if ! kubectl -n default delete pod "$POD_PCT" "$POD_MIB" --ignore-not-found --force --grace-period=0 >/dev/null 2>&1; then
    echo "[case-8] warning: could not delete the slice pods; the next section may not fit beside them"
  fi
  wait_settled "$BASE_SIG"
fi

# --- Section 2: the sibling InstanceTypes of one pool stay status-consistent. ---
#
# Several InstanceTypes that declare the SAME os / arch / acceleratorGroup / acceleratable share
# ONE ResourceFlavor + Devices pool, so a Devices-ledger change must re-enqueue EVERY one of them
# and leave their four-view status identical. Deploying a sliced workload moves the ledger and is
# the trigger.
echo "[case-8] section 2: the sibling InstanceTypes of one pool stay status-consistent"

# The sliced resource + its memory/cores percentage keys, read off a pool node's allocatable (the
# device-manager advertises "<vendor>/<class>.sliced[.memory-percentage|.cores-percentage]").
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
  echo "[case-8] section 2 skipped: the pool ${IT} declares no acceleratorGroup, so it has no sibling identity to share"
elif [ -z "${SLICED_CAP:-}" ] || [ "${SLICED_CAP}" = "0" ]; then
  echo "[case-8] section 2 skipped: the pool ${IT} reports no sliced capacity, so a logical slice cannot move its ledger"
elif [ -z "${POOL_SLICED:-}" ]; then
  echo "[case-8] section 2 skipped: no node carrying ${AKEY} advertises a *.sliced resource"
else
  echo "[case-8] pool: derived=${IT} acceleratorGroup=${AKEY} os/arch=${OS}/${ARCH} entrance=${LQ} sliced=${POOL_SLICED}"

  # The baseline this section measures against: the pool's four-view status before its load.
  BASE2=$(sig "$IT")
  if [ -z "$BASE2" ] || [ "$BASE2" = "|||" ]; then BASE2="$BASE_SIG"; fi
  echo "[case-8] baseline four-view (EX|SH|SL|PT) on ${IT} = ${BASE2}"

  # 1. Declare two sibling admin InstanceTypes on the SAME pool identity.
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

  # 2. Deploy a 50% logical-slice workload on the pool's entrance LocalQueue — this moves the Devices ledger.
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

  # 3. The ledger change must re-enqueue EVERY sibling: all stay identical AND move from the baseline.
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

  # Release the load and the throwaway types, then let the freed slice settle back so the next
  # section sees the pool whole again.
  echo "[case-8] releasing the load pod and the sibling InstanceTypes"
  if ! kubectl -n default delete pod "$POD_LOAD" --ignore-not-found --force --grace-period=0 >/dev/null 2>&1; then
    echo "[case-8] warning: could not delete the load pod; the next section may not fit beside it"
  fi
  cleanup_types
  wait_settled "$BASE2"
fi

# --- Section 3: the Instance admission webhooks on a sliceable type. ---
#
# Q1: Default scales the per-card unit CPU/RAM to the slice percentages (compute % sizes CPU,
#     memory % sizes RAM), floors fractions, never below 1, and pins the accelerator to 1;
# Q2: a lone memory percentage is mirrored to the compute percentage, so CPU is sized too;
# Q3: Validate rejects a sliceable request whose accelerator count is not 1.
echo "[case-8] section 3: the Instance admission webhooks on a sliceable type"

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
  echo "[case-8] section 3 skipped: ${UNIT_WHY}"
else
  # The expected slice sizing, derived from the pool's per-card unit spec (immutable — read, don't
  # pin). CPU floors to an integer core, never below 1; RAM scales linearly.
  RAM_N="${UNIT_RAM%Gi}"
  cpu_pct() { local v=$(( UNIT_CPU * $1 / 100 )); [ "$v" -lt 1 ] && v=1; echo "$v"; }
  ram_pct() { echo "$(( RAM_N * $1 / 100 ))Gi"; }
  EXP_CPU_OK=$(cpu_pct 25);  EXP_RAM_OK=$(ram_pct 25)
  EXP_CPU_MEM=$(cpu_pct 50); EXP_RAM_MEM=$(ram_pct 50)
  echo "[case-8] expected: 25% slice → cpu=${EXP_CPU_OK} ram=${EXP_RAM_OK}; 50% slice → cpu=${EXP_CPU_MEM} ram=${EXP_RAM_MEM}"

  # mk_slice NAME MEM_PCT CORES_PCT [ACCELERATOR] — apply a sliced Instance (accelerator omitted
  # when the 4th arg is empty, so Default fills it). Prints kubectl's combined output; exits
  # non-zero when admission rejects. A 0 cores-percentage means "unset" (exercises the
  # mirror-from-memory path).
  mk_slice() {
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

  # Q1 — a 25% slice: accelerator pinned to 1, CPU = unitCPU × 25%, RAM = unitRAM × 25%.
  echo "[case-8] Q1 apply: $(mk_slice "$INST_OK" 25 25)"
  acc=""; for _ in $(seq 1 10); do acc=$(get_res "$INST_OK" accelerator); [ -n "$acc" ] && break; sleep 1; done
  cpu=$(get_res "$INST_OK" cpu); ram=$(get_res "$INST_OK" ram)
  if [ "$acc" = "1" ] && [ "$cpu" = "$EXP_CPU_OK" ] && [ "$ram" = "$EXP_RAM_OK" ]; then
    record PASS "Default scales slice to unit CPU/RAM, accelerator=1" "acc=${acc} cpu=${cpu} ram=${ram} (unit ${UNIT_CPU}/${UNIT_RAM} × 25%)"
  else
    record FAIL "Default scales slice to unit CPU/RAM, accelerator=1" "acc='${acc:-?}' cpu='${cpu:-?}' ram='${ram:-?}', want 1/${EXP_CPU_OK}/${EXP_RAM_OK}"
  fi

  # Q2 — a lone memory percentage (50%) is mirrored to compute, so CPU is sized too.
  echo "[case-8] Q2 apply: $(mk_slice "$INST_MEMONLY" 50 0)"
  cores2=""; for _ in $(seq 1 10); do cores2=$(get_res "$INST_MEMONLY" acceleratorSlicedCoresPercentage); [ -n "$cores2" ] && break; sleep 1; done
  cpu2=$(get_res "$INST_MEMONLY" cpu); ram2=$(get_res "$INST_MEMONLY" ram)
  if [ "$cores2" = "50" ] && [ "$cpu2" = "$EXP_CPU_MEM" ] && [ "$ram2" = "$EXP_RAM_MEM" ]; then
    record PASS "lone memory % mirrored to compute, sizes CPU" "cores%=${cores2} cpu=${cpu2} ram=${ram2}"
  else
    record FAIL "lone memory % mirrored to compute, sizes CPU" "cores%='${cores2:-?}' cpu='${cpu2:-?}' ram='${ram2:-?}', want 50/${EXP_CPU_MEM}/${EXP_RAM_MEM}"
  fi

  # Q3 — Validate rejects a sliceable request whose accelerator count is not 1.
  # Match the webhook's own message: any admission denial says "denied the request", so a looser
  # pattern would pass on a rejection for an unrelated reason.
  Q3_MSG='accelerator request must be exactly 1 for a sliced request'
  err=$(mk_slice "$INST_BAD" 25 25 2); rc=$?
  if [ "$rc" -ne 0 ] && echo "$err" | grep -qF "$Q3_MSG"; then
    record PASS "Validate rejects accelerator != 1 on sliceable" "rejected: $(echo "$err" | grep -oF "$Q3_MSG" | head -1)"
  elif [ "$rc" -ne 0 ]; then
    record FAIL "Validate rejects accelerator != 1 on sliceable" "rejected, but not with '${Q3_MSG}': $(echo "$err" | head -1)"
  else
    kubectl -n default delete instance "$INST_BAD" --ignore-not-found >/dev/null 2>&1 || true
    record FAIL "Validate rejects accelerator != 1 on sliceable" "accepted (rc=${rc}) — sliceable accelerator must be 1"
  fi
fi

echo
echo "== CASE 8 — Real accelerator slicing: runtime isolation, slice webhook and sibling-pool re-enqueue =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). The device plugin must cap a real slice at runtime, the Instance"
  echo "webhooks must size a slice of one card's unit and pin the accelerator to 1, and a Devices"
  echo "change must refresh every InstanceType on the pool. Diagnose: kubectl -n default describe"
  echo "pod ${POD_PCT}; kubectl -n ${NS} logs ds/gpustack-operator-device-manager-${RUNTIME_MANUF} --tail=100;"
  echo "kubectl -n ${NS} logs deploy/gpustack-operator-worker --tail=200"
  exit 1
fi
echo "CASE 8 PASS"
