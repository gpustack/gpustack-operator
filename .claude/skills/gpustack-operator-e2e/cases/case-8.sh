#!/usr/bin/env bash
#
# CASE 8 — Real accelerator slicing runtime isolation   (MUTATING, self-recovering; AUTO-SKIPS without real sliced hardware)
#
#   case-8.sh <NS>
#
# Goal:        A sliced workload is capped at runtime to its share of a real card — by memory
#              percentage, by memory MiB, and by compute (SM) percentage — via the vendor's slicing
#              runtime. End to end this drives: the Pod webhook folds .sliced.memory-percentage /
#              .sliced.memory-mib into .sliced.units; Kueue admits and the node-devices AdmissionCheck
#              reads the real Devices ledger; the device plugin allocates a real card and injects the
#              runtime cap (the memory-limit and SM-limit environment variables).
# Environment: Needs REAL accelerator hardware advertising a *.sliced resource (the runtime cap
#              cannot be mocked). AUTO-SKIPS (exit 0, prints why) when none is advertised.
#              Defaults to NVIDIA (HAMi libvgpu). Another vendor overrides the resource base key, the
#              in-container manufacturer tool and how its output is read, the cap variable names, and
#              the claim carrier image — see the vendor-dimension assignments below.
# Inputs:      All real, nothing mocked —
#              - POD_PCT: .sliced=1 + memory-%=50 + cores-%=50 on the sliceable pool's entrance LocalQueue;
#              - POD_MIB: .sliced=1 + memory-mib=4096 on the same queue.
# Expected:    - both Pods reach Running (webhook fold → Kueue → AdmissionCheck → schedule → device plugin);
#              - POD_PCT: the SM-limit variable reads 50 and the tool's total memory < physical card;
#              - POD_MIB: the injected memory cap / the tool's total == 4096 MiB.
# Cleanup:     Trap deletes both test Pods.
set -uo pipefail

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
POD_PCT=gpustack-e2e-slice-pct
POD_MIB=gpustack-e2e-slice-mib
MIB_REQ=4096   # fits a 16Gi (T4) or 24Gi (A10G) card

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

# A LOGICALLY sliceable accelerated InstanceType of THIS vendor, its entrance LocalQueue, and its
# per-card memory.
#
# Filtered by manufacturer, not merely by "sliceable": on a multi-vendor cluster the first sliceable
# pool need not be the vendor whose resource this run requests, and every figure taken from it — the
# entrance queue, the accelerator memory, the runtimeClass — would then describe different hardware
# from the `${BASE}.sliced` tokens being claimed. The manufacturer is a parameter rather than a value
# read back out of whichever pool matched, because reading it back is what allowed the mismatch.
read -r IT LQ CARDMEM <<<"$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null | MANUF="$MANUF" python3 -c "
import json,os,sys
manuf=os.environ['MANUF']
for it in json.load(sys.stdin).get('items',[]):
    s=it.get('spec',{}); st=it.get('status',{}); d=st.get('detail',{}); sd=d.get('slicedDetail',{})
    # LOGICALLY sliceable only: a hardware-partitioned card serves no logical slice.
    sliceable=(sd.get('logical',{}).get('count',0) or 0)>0
    if s.get('acceleratable') and sliceable and d.get('manufacturer','')==manuf:
        print(it['metadata']['name'], st.get('entrance',''), d.get('memory','')); break
")"
[ -n "$IT" ] && [ -n "$LQ" ] || { echo "no logically sliceable ${MANUF} InstanceType with an entrance LocalQueue found"; exit 1; }
# Physical card memory in MiB (from the InstanceType card memory, e.g. "24Gi") for the below-physical
# assertion; 0 if it cannot be parsed.
PHYS_MIB=$(python3 -c "
import re
m = re.match(r'\s*(\d+)\s*([GM])i?', '${CARDMEM}')
print(int(m.group(1)) * (1024 if m.group(2) == 'G' else 1) if m else 0)
" 2>/dev/null)
echo "[case-8] sliceable InstanceType ${IT} (card memory ${CARDMEM} = ${PHYS_MIB}MiB) via LocalQueue ${LQ}"

# The logical-slicing runtime (HAMi libvgpu.so, LD_PRELOAD-injected at Allocate) needs the vendor
# runtimeClass to mount the driver libs it depends on — a bare image without it exits 127. The
# operator's Instance controller injects this automatically; a raw Pod must set it too. Derive it
# from the pool manufacturer (identity map: nvidia->nvidia, mthreads->mthreads).
RUNTIMECLASS=""
if [ -n "$MANUF" ] && kubectl get runtimeclass.node.k8s.io "$MANUF" >/dev/null 2>&1; then RUNTIMECLASS="$MANUF"; fi
RTC_LINE=""; [ -n "$RUNTIMECLASS" ] && RTC_LINE="runtimeClassName: ${RUNTIMECLASS}"
echo "[case-8] slice pods runtimeClass: ${RUNTIMECLASS:-<none>}"

restore() {
  echo
  echo "[case-8] cleanup: deleting test Pods"
  kubectl -n default delete pod "$POD_PCT" "$POD_MIB" --ignore-not-found --force --grace-period=0 2>/dev/null || true
}
trap restore EXIT

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

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

# 1. Percentage slice: 50% memory + 50% compute. Expect the HAMI cap SM_LIMIT=50 and a memory limit
#    below the physical card.
echo "[case-8] creating ${POD_PCT}: ${SLICED}=1 memory-%=50 cores-%=50 on ${LQ}"
cat <<EOF | kubectl apply -f - >/dev/null
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
if wait_running "$POD_PCT"; then
  record PASS "percentage slice admitted+running" "${POD_PCT} Running (webhook fold → Kueue → AdmissionCheck → schedule)"
  sm=$(exec_out "$POD_PCT" printenv "$SM_ENV")
  mem=$(exec_out "$POD_PCT" printenv "$MEM_ENV")
  [ "$sm" = "50" ] && record PASS "compute (SM) capped to 50%" "${SM_ENV}=${sm}" \
    || record FAIL "compute (SM) capped to 50%" "${SM_ENV}='${sm:-<unset>}', want 50"
  smi_total=$(exec_out "$POD_PCT" $SMI | grep -oE "$SMI_MEM_RE" | head -1 | grep -oE '[0-9]+')
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
cat <<EOF | kubectl apply -f - >/dev/null
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
if wait_running "$POD_MIB"; then
  mem=$(exec_out "$POD_MIB" printenv "$MEM_ENV")
  smi_total=$(exec_out "$POD_MIB" $SMI | grep -oE "$SMI_MEM_RE" | head -1 | grep -oE '[0-9]+')
  { [ "$mem" = "${MIB_REQ}${MEM_ENV_UNIT}" ] || [ "$smi_total" = "$MIB_REQ" ]; } \
    && record PASS "memory-mib slice capped to ${MIB_REQ}MiB" "${MEM_ENV}=${mem}, ${SMI} total=${smi_total}MiB" \
    || record FAIL "memory-mib slice capped to ${MIB_REQ}MiB" "${MEM_ENV}='${mem:-?}', ${SMI} total='${smi_total:-?}', want ${MIB_REQ}"
else
  record FAIL "memory-mib slice admitted+running" "${POD_MIB} not Running"
fi

echo
echo "== CASE 8 — Real accelerator slicing runtime isolation =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). The Pod webhook folds the request, Kueue+AdmissionCheck admit it, and"
  echo "the device plugin injects the runtime cap. Diagnose: kubectl -n default describe pod ${POD_PCT};"
  echo "kubectl -n ${NS} logs ds/gpustack-operator-device-manager-${MANUF:-nvidia} --tail=100"
  exit 1
fi
echo "CASE 8 PASS"
