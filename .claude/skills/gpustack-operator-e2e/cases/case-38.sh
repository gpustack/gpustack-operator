#!/usr/bin/env bash
#
# CASE 38 — AMD accelerator claims over both carriers: exclusive whole cards, logical slices, and the Instance metrics array   (MUTATING, self-recovering; AUTO-SKIPS without real AMD hardware)
#
#   case-38.sh <NS>
#
# Goal:        ASSERTS the AMD claim matrix end to end, and asserts every row TWICE — once from what
#              the operator recorded (the allocation annotation, the per-card ledger, the Instance
#              metrics subresource) and once from INSIDE the container with AMD's own tooling. The two
#              readings are kept separate on purpose: a ledger that says the right thing about a
#              container that cannot touch an accelerator is the failure mode this case exists to
#              catch, and a single merged verdict would hide it behind whichever half passed.
#
#              The matrix, each row over the carrier(s) that can express it:
#                exclusive whole accelerator          — Instance and Pod, 1 and 2 accelerators
#                sliced + memory percentage           — Instance and Pod, 1 accelerator
#                sliced + memory MiB                  — Pod, 1 accelerator
#                sliced + cores percentage            — Pod, 1 accelerator
#                sliced, TWO slices on ONE accelerator — Pod, unequal shares
#
#              Four properties beyond the rows themselves:
#                - a two-accelerator exclusive claim lands on two DISTINCT accelerators, evidenced
#                  both from the allocation annotation and from inside the container;
#                - a sliced claim's recorded compute placement is a CU run whose length is the
#                  requested share of the accelerator's compute units;
#                - two slices sharing ONE accelerator get DISJOINT compute windows, each container
#                  carrying its own window and not its neighbour's, with the accelerator's budget
#                  charged for both and never exceeded. Every other sliced row is one claim on one
#                  accelerator, where the first window always starts at 0, so this is the only shape
#                  that exercises seating a window beside the ones already held — and an overlap
#                  there hands two containers the same compute units while every ledger reading
#                  still looks correct;
#                - every exclusive Instance's metrics subresource carries one accelerator entry per
#                  allocated accelerator, identified by accelerator identity and never by ordinal,
#                  with the physical memory capacity and in-range utilizations, and its temperature
#                  and power cross-checked against the same figures read inside the container.
#
#              Expectations are COMPUTED from the live ledger and the pool's own descriptors
#              immediately before each claim rather than hardcoded, so the case is valid on an
#              accelerator host that already carries unrelated work. Where a precondition is absent
#              — no second accelerator, no default StorageClass, a compute share the accelerator
#              cannot express, per-slice metrics not offered — the row records SKIP with the value
#              seen, never a vacuous PASS.
# Environment: Needs REAL AMD hardware: one node with at least one healthy, logically sliceable AMD
#              accelerator advertising both the whole-accelerator and the .sliced resource families.
#              AUTO-SKIPS (exit 0, prints why) when no such node exists. The two-accelerator rows
#              additionally need a second healthy accelerator in the same accelerator group and
#              record SKIP without one. A Devices query that errors FAILS SETUP rather than skipping:
#              "no AMD hardware" and "the query did not answer" must not be indistinguishable.
#
#              The claim carrier must be a ROCm-family image, because every in-container reading is
#              taken with AMD's own tools — amd-smi for which accelerators are visible and their
#              physical capacity, the HIP surface for the memory a slice is capped to, and the
#              bundled CU-mask reader for the compute window. A bare base image runs an AMD slice
#              perfectly well (the slicing library resolves the ROCm runtime lazily) but ships none
#              of those tools, so the readings would all be unavailable rather than wrong. Override
#              with E2E_AMD_IMAGE=<ref>; it is a large image, so pre-load it on the node or the first
#              claim spends its whole wait pulling.
#
#              The Instance carrier needs a default StorageClass for its workspace volume. Without
#              one, every Instance row falls back to the Pod carrier and says so.
#
#              Presumes the node's kubelet runs topologyManagerPolicy=none, the suite's baseline.
# Inputs:      All real, nothing mocked. Up to eleven claims, submitted in phases so that the
#              whole-accelerator phases have the population to themselves:
#                phase 1  one exclusive Instance and one exclusive Pod, one accelerator each;
#                phase 2  one exclusive Instance for two accelerators (only when the pool's unit
#                         resources for two accelerators fit the node — the sizing is computed, and
#                         reported when it does not);
#                phase 3  one exclusive Pod for two accelerators;
#                phase 4  four sliced claims sized to fit the pool together — an Instance and a Pod
#                         by memory percentage, a Pod by memory MiB, a Pod by cores percentage;
#                phase 5  two sliced Pods of UNEQUAL shares that provably fit one accelerator
#                         together, on the pool phase 4 gives back, plus — only if the packing policy
#                         opens an idle accelerator instead of joining the one in use — one
#                         whole-accelerator Pod occupying that idle accelerator so a single sliceable
#                         candidate remains. Which of the two routes was taken is computed from the
#                         allocations and printed.
# Expected:    - every claim reaches Running/Ready through the whole chain (webhook fold, Kueue
#                admission, the per-card feasibility gate, the device plugin);
#              - the container holds accelerator device access, and AMD's tooling inside it reports
#                exactly the accelerators the allocation names, matched by PCI address;
#              - an exclusive claim sees each accelerator's full physical memory; a sliced claim sees
#                its share and nothing more;
#              - a two-accelerator claim occupies two distinct accelerators in both readings;
#              - a sliced claim's recorded compute run never spans more of the accelerator than the
#                share it was charged for, and spans exactly that share when the accelerator can
#                express it;
#              - two slices on one accelerator hold disjoint compute windows whenever the delivered
#                lengths leave room for two seats, each container carrying its own window; the
#                accelerator's remaining budget equals its pre-claim budget less both charges. Where
#                the pair lands on two accelerators and co-location cannot be forced, or the delivered
#                lengths cannot both be seated, the properties record SKIP with the arithmetic rather
#                than a verdict — the placement's documented fallback on a full accelerator is the
#                least-overlapping seat, so an overlap there is behaviour, not a defect;
#              - an exclusive Instance's metrics accelerator array matches the allocation by
#                identity, reports the physical capacity, keeps both utilizations in [0,100], and
#                agrees with the container's own temperature and power readings.
# Cleanup:     Trap deletes every Instance and Pod this case created and waits for the pool's
#              whole-accelerator and sliced remaining counts to return to the values captured at
#              setup, so the next case does not start on capacity this one still holds. Runs on pass
#              AND fail, and is safe to re-run: every delete tolerates an absent object and the wait
#              tolerates an already-settled ledger.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-38.sh <NS>}"

# AMD's resource family. Hardcoded rather than derived: the case is gated on the amd manufacturer
# below, and deriving the family here would duplicate a mapping the allocators own.
MANUF=amd
BASE=amd.com/gpu
SLICED=${BASE}.sliced
CORESPCT=${BASE}.sliced.cores-percentage
MEMPCT=${BASE}.sliced.memory-percentage
MEMMIB=${BASE}.sliced.memory-mib

PFX=gpustack-e2e-amd
EX_INST1=${PFX}-ex-inst1
EX_POD1=${PFX}-ex-pod1
EX_INST2=${PFX}-ex-inst2
EX_POD2=${PFX}-ex-pod2
SL_INST=${PFX}-sl-inst
SL_POD_PCT=${PFX}-sl-pod-pct
SL_POD_MIB=${PFX}-sl-pod-mib
SL_POD_CORES=${PFX}-sl-pod-cores
SL_CO_A=${PFX}-sl-co-a
SL_CO_B=${PFX}-sl-co-b
SL_CO_BLOCK=${PFX}-sl-co-block
ALL_INSTANCES=("$EX_INST1" "$EX_INST2" "$SL_INST")
ALL_PODS=("$EX_POD1" "$EX_POD2" "$SL_POD_PCT" "$SL_POD_MIB" "$SL_POD_CORES"
          "$SL_CO_A" "$SL_CO_B" "$SL_CO_BLOCK")

# The claim carrier. A ROCm-family image, because every in-container reading below is taken with
# AMD's own tools (amd-smi, the HIP surface, the bundled CU-mask reader) and a bare base image ships
# none of them. Overridable for another ROCm generation or a registry mirror.
IMAGE="${E2E_AMD_IMAGE:-rocm/dev-ubuntu-22.04:7.2.4}"
# The share the memory rows claim, the MiB the exact-size row claims, and the share the compute row
# claims. Percentages rather than absolute figures so the case travels between accelerator sizes.
MEM_PCT="${E2E_AMD_MEM_PCT:-50}"
MIB_REQ="${E2E_AMD_MEM_MIB:-4096}"
CORES_PCT="${E2E_AMD_CORES_PCT:-50}"
# The compute row also needs a memory budget, kept small so all four sliced claims fit the pool.
CORES_ROW_MEM_PCT=25
# The two shares the co-located pair claims, one accelerator between them. UNEQUAL on purpose: two
# identical windows would satisfy a disjointness test by coinciding at the same start, so an overlap
# bug could pass. Their memory shares sum below one accelerator's budget, so the pair provably fits
# and a spill onto a second accelerator is a real signal rather than arithmetic. Overridable because a
# part with a coarser allocation atom than this pair's compute shares cannot express them.
CO_A_MEM_PCT="${E2E_AMD_CO_A_MEM_PCT:-30}"
CO_A_CORES_PCT="${E2E_AMD_CO_A_CORES_PCT:-20}"
CO_B_MEM_PCT="${E2E_AMD_CO_B_MEM_PCT:-40}"
CO_B_CORES_PCT="${E2E_AMD_CO_B_CORES_PCT:-60}"
# How long a claim may take to reach Running/Ready. Generous: the carrier is a multi-gigabyte image
# and the first claim of a run may be pulling it.
RUN_TRIES="${E2E_AMD_RUN_TRIES:-100}"

# --- Skip gate: a node with a healthy, logically sliceable AMD accelerator.
#     The per-accelerator slicing CAPABILITY lives in Devices.spec (the runtime ledger in .status
#     carries no capability), and only a healthy accelerator reporting a logical slice count belongs
#     to the population the kubelet is offered tokens from.
#
#     Read the ledger ONCE and fail loudly if that read errors: silencing it turns an RBAC or
#     connectivity failure into "no AMD hardware", i.e. a clean exit 0, which is the worst outcome
#     for a regression guard. `pipefail` does not help — the failure would reach the outer `read` as
#     an empty here-string, which succeeds. ---
if ! DEVICES_JSON=$(kubectl get devices -o json 2>&1); then
  echo "== CASE 38 — FAILED (setup) =="
  echo "Reading the Devices ledger failed, so this case cannot tell 'no AMD hardware' from 'the query"
  echo "did not answer'. It refuses to report a skip on either:"
  printf '%s\n' "$DEVICES_JSON" | head -5
  exit 1
fi
#     The parse gets the same treatment as the read, and for the same reason: a parser that fails on a
#     payload it cannot read would otherwise report best='-', which this gate turns into a skip. So its
#     exit status is checked too, rather than only its output. ---
if ! DEVICES_SUMMARY=$(printf '%s' "$DEVICES_JSON" | MANUF="$MANUF" python3 -c "
import json,os,sys
manuf=os.environ['MANUF']
best=('-','-',0,0,0)
for d in json.load(sys.stdin).get('items',[]):
    for g in d.get('spec',{}).get('groups',[]):
        if g.get('manufacturer')!=manuf: continue
        n=sum(1 for a in g.get('accelerators',[])
              if (a.get('status',{}).get('logicalSliced',{}).get('count',0) or 0)>0
              and not a.get('status',{}).get('unhealthy'))
        if n>best[2]:
            best=(d['metadata']['name'], g.get('id',''), n,
                  int(g.get('memory') or 0), int(g.get('cores') or 0))
print(*best)
"); then
  echo "== CASE 38 — FAILED (setup) =="
  echo "The Devices ledger was read but could not be parsed, so this case cannot tell 'no AMD hardware'"
  echo "from 'the answer was unreadable'. It refuses to report a skip on either."
  exit 1
fi
read -r NODE GROUPID NCARDS CARD_MEM_MIB CARD_CU <<<"$DEVICES_SUMMARY"
# TWO accelerators, not one. Several phases deliberately hold claims concurrently — phase 1 keeps an
# exclusive Instance while an exclusive Pod starts, and phase 4 holds two half-accelerator slices
# beside a third claim — so a single-accelerator node cannot satisfy them and the rows would fail for
# want of hardware rather than for a defect. Demanding two here states that honestly, instead of
# admitting the node and failing later.
if [ "${NODE:--}" = "-" ] || [ "${NCARDS:-0}" -lt 2 ]; then
  echo "== CASE 38 — SKIPPED =="
  echo "No node reports two healthy, logically sliceable ${MANUF} accelerators (best='${NODE:-none}',"
  echo "accelerators=${NCARDS:-0}). Phases 1 and 4 hold claims at the same time, so two are needed."
  echo "Every row of this case reads a real accelerator through AMD's own tooling inside the container,"
  echo "which cannot be mocked. Run it on an AMD accelerator host with two accelerators."
  exit 0
fi
if [ "${CARD_MEM_MIB:-0}" -le 0 ] || [ "${CARD_CU:-0}" -le 0 ]; then
  echo "== CASE 38 — FAILED (setup) =="
  echo "Accelerator group '${GROUPID}' on ${NODE} reports memory='${CARD_MEM_MIB:-?}'MiB and"
  echo "cores='${CARD_CU:-?}'. Both size every expectation below, so a zero would make the memory and"
  echo "compute assertions vacuous rather than failing."
  exit 1
fi
echo "[case-38] target: ${NODE}, group '${GROUPID}', ${NCARDS} healthy sliceable accelerator(s)"
echo "[case-38] one accelerator = ${CARD_MEM_MIB}MiB / ${CARD_CU} compute units"

# Every accelerator of the group, by identity: its id and its PCI address. The PCI address is the
# correlation key for every in-container reading — AMD's tooling renumbers its own ordinals to the
# devices the container was granted, so an ordinal says nothing about which physical accelerator was
# seen, while the PCI address is the same string on both sides.
CARD_TABLE=$(printf '%s' "$DEVICES_JSON" | NODE="$NODE" GID="$GROUPID" python3 -c "
import json,os,sys
node=os.environ['NODE']; gid=os.environ['GID']
for d in json.load(sys.stdin).get('items',[]):
    if d['metadata']['name']!=node: continue
    for g in d.get('spec',{}).get('groups',[]):
        if g.get('id')!=gid: continue
        for a in g.get('accelerators',[]):
            if a.get('status',{}).get('unhealthy'): continue
            print(a.get('id',''), a.get('index'), a.get('topology',{}).get('pciBusId',''))
")
echo "[case-38] accelerators (id / index / pci):"
printf '%s\n' "$CARD_TABLE" | sed 's/^/  /'

# bdf_of <accelerator-id> — the PCI address the ledger recorded for that accelerator.
bdf_of() {
  printf '%s\n' "$CARD_TABLE" | awk -v id="$1" '$1==id {print $3; exit}'
}

# The accelerated InstanceType whose POOL this node belongs to, its entrance LocalQueue, and the unit
# resources it sizes a whole-accelerator request by. Must match the target node, or the queue would route
# the claim to another pool. Matched on the pool's identity tuple — accelerator group plus os/arch — see
# the index's note on pool lookup.
read -r IT LQ UNIT_CPU UNIT_RAM <<<"$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null | NODE_GID="$GROUPID" NODE_JSON="$(kubectl get node "$NODE" -o json 2>/dev/null)" python3 -c "
import json,sys,os
gid=os.environ.get('NODE_GID','')
nl=(json.loads(os.environ.get('NODE_JSON') or '{}').get('metadata',{}) or {}).get('labels',{}) or {}
# A pool BACKS this node when every discriminator it carries is a label the node carries too. Those are
# the schedule labels the webhook stamps from PoolScheduleLabels, so this is the pool's whole identity —
# os/arch, the acceleratable boolean, the accelerator group and, under CPU-aware grouping, the general
# group. schedule.gpustack.ai/* is the pool's own bookkeeping and never a node label.
def backs(it):
    d={k:v for k,v in (it['metadata'].get('labels') or {}).items() if not k.startswith('schedule.gpustack.ai/')}
    return bool(d) and all(nl.get(k)==v for k,v in d.items())
def in_group(s):
    return bool(gid) and (s.get('acceleratorGroup') or '').endswith('-'+gid)
for it in json.load(sys.stdin).get('items',[]):
    s=it.get('spec',{}); name=it['metadata']['name']; st=it.get('status',{})
    sd=(st.get('detail',{}) or {}).get('slicedDetail',{}) or {}
    if s.get('acceleratable') and ((sd.get('logical',{}) or {}).get('count',0) or 0)>0 \
       and in_group(s) and backs(it):
        u=s.get('unitResources',{}) or {}
        print(name, st.get('entrance',''), u.get('cpu','') or '-', u.get('ram','') or '-'); break
")"
[ -n "${IT:-}" ] && [ -n "${LQ:-}" ] || {
  echo "== CASE 38 — FAILED (setup) =="
  echo "No logically sliceable InstanceType with an entrance LocalQueue backs ${NODE} in group"
  echo "'${GROUPID}'. Run the CPU-only chain case first to confirm the scheduling chain materialized."
  exit 1
}
echo "[case-38] pool ${IT} via LocalQueue ${LQ}; unit resources cpu=${UNIT_CPU} ram=${UNIT_RAM}"

# The vendor runtimeClass. The Instance controller sets it by itself when the object exists; a raw
# Pod has to say so too, and the container runtime is what turns the allocation's visible-devices
# variable into device nodes inside the container. Identity map from the pool manufacturer, guarded
# on existence so a cluster without the class still runs the case and reports what it saw.
RUNTIMECLASS=""
kubectl get runtimeclass.node.k8s.io "$MANUF" >/dev/null 2>&1 && RUNTIMECLASS="$MANUF"
RTC_LINE=""; [ -n "$RUNTIMECLASS" ] && RTC_LINE="runtimeClassName: ${RUNTIMECLASS}"
echo "[case-38] claim carrier ${IMAGE}; runtimeClass: ${RUNTIMECLASS:-<none>}"

# --- Carrier viability. The Instance carrier mounts a workspace volume, which needs a default
#     StorageClass; without one every Instance row falls back to the Pod carrier and says so. ---
DEFAULT_SC=$(kubectl get storageclass -o json 2>/dev/null | python3 -c "
import json,sys
for sc in json.load(sys.stdin).get('items',[]):
    ann=sc['metadata'].get('annotations') or {}
    if ann.get('storageclass.kubernetes.io/is-default-class')=='true':
        print(sc['metadata']['name']); break
")
INSTANCE_CARRIER=yes
if [ -z "$DEFAULT_SC" ]; then
  INSTANCE_CARRIER=no
  echo "[case-38] no default StorageClass — the Instance carrier is unavailable, Pod carrier only"
else
  echo "[case-38] default StorageClass ${DEFAULT_SC} — the Instance carrier is available"
fi

# Whether an Instance holding TWO accelerators fits the node at all. The Instance webhook sizes a
# whole-accelerator request as the pool's unit CPU/RAM times the accelerator count and, with general
# overcommit on, overrides an explicit smaller request — so a pool whose unit RAM exceeds half the
# node's allocatable memory can never host a two-accelerator Instance, however much accelerator
# capacity is free. Compute that rather than discovering it as a Pending Pod after a long wait.
read -r NODE_CPU_M NODE_RAM_KI <<<"$(kubectl get node "$NODE" -o json 2>/dev/null | python3 -c "
import json,sys
def q(v):
    v=str(v)
    for suf,mul in (('Ki',1024),('Mi',1024**2),('Gi',1024**3),('m',None)):
        if v.endswith(suf):
            n=v[:-len(suf)]
            return int(float(n)*1000) if mul is None else int(float(n)*mul)
    return int(float(v)*1000)
a=json.load(sys.stdin)['status']['allocatable']
print(q(a.get('cpu','0')), int(q(a.get('memory','0'))/1024))
")"
INST2_FITS=no
INST2_WHY=""
if [ "$INSTANCE_CARRIER" = yes ]; then
  read -r INST2_FITS INST2_WHY <<<"$(UNIT_CPU="$UNIT_CPU" UNIT_RAM="$UNIT_RAM" NCPU="${NODE_CPU_M:-0}" NRAM="${NODE_RAM_KI:-0}" python3 -c "
import os
def q(v):
    v=str(v).strip()
    for suf,mul in (('Ki',1024),('Mi',1024**2),('Gi',1024**3),('m',None)):
        if v.endswith(suf):
            n=v[:-len(suf)]
            return int(float(n)*1000) if mul is None else int(float(n)*mul)
    try: return int(float(v)*1000)
    except ValueError: return 0
ucpu=q(os.environ.get('UNIT_CPU','-'))           # milli-CPU
uram=q(os.environ.get('UNIT_RAM','-'))//1024     # KiB
ncpu=int(os.environ.get('NCPU','0')); nram=int(os.environ.get('NRAM','0'))
if not ucpu or not uram:
    print('no', 'the-pool-declares-no-unit-resources')
elif 2*ucpu<=ncpu and 2*uram<=nram:
    print('yes', 'two-units-cpu=%dm-ram=%dKi-within-node-cpu=%dm-ram=%dKi'%(2*ucpu,2*uram,ncpu,nram))
else:
    print('no', 'two-units-cpu=%dm-ram=%dKi-exceed-node-allocatable-cpu=%dm-ram=%dKi'%(2*ucpu,2*uram,ncpu,nram))
")"
fi
echo "[case-38] two-accelerator Instance carrier fits the node: ${INST2_FITS} (${INST2_WHY:-n/a})"

# --- Baselines, captured before this case dirties anything, and restored by the trap. ---
EX_BASE=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.status.accelerator.remaining}' 2>/dev/null)
SL_BASE=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.status.acceleratorSliced.remaining}' 2>/dev/null)
echo "[case-38] pool remaining at start: whole-accelerator=${EX_BASE:-?} sliced=${SL_BASE:-?}"

# wait_pool <view-jsonpath> <want> — poll until the pool view reports the wanted figure.
wait_pool() {
  local jp="$1" want="$2" now=""
  [ -n "$want" ] || return 0
  for _ in $(seq 1 25); do
    now=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath="$jp" 2>/dev/null)
    [ "$now" = "$want" ] && return 0
    sleep 3
  done
  echo "[case-38]   pool ${jp} is '${now:-?}', wanted '${want}'"
  return 1
}

restore() {
  echo
  echo "[case-38] cleanup: deleting the Instances and Pods this case created"
  local n
  for n in "${ALL_INSTANCES[@]}"; do
    kubectl -n default delete instance "$n" --ignore-not-found >/dev/null 2>&1 || true
  done
  for n in "${ALL_PODS[@]}"; do
    kubectl -n default delete pod "$n" --ignore-not-found --force --grace-period=0 >/dev/null 2>&1 || true
  done
  # Give the pool back what this case took, so the next case does not misread the ledger.
  if wait_pool '{.status.accelerator.remaining}' "${EX_BASE:-}" &&
     wait_pool '{.status.acceleratorSliced.remaining}' "${SL_BASE:-}"; then
    echo "[case-38] pool remaining back to whole-accelerator=${EX_BASE:-?} sliced=${SL_BASE:-?}"
  else
    echo "[case-38] WARNING: the pool ledger has not settled back to its starting figures"
  fi
}
trap restore EXIT

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
# verdict <cond-exit-code> <check> <object-on-pass> <object-on-fail>
verdict() { [ "$1" -eq 0 ] && record PASS "$2" "$3" || record FAIL "$2" "$4"; }

# ---------------------------------------------------------------------------------------------
# Claim submission
# ---------------------------------------------------------------------------------------------

# submit_pod <name> <resources-yaml-map>
submit_pod() {
  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: { name: $1, namespace: default, labels: { kueue.x-k8s.io/queue-name: ${LQ} } }
spec:
  schedulerName: default-scheduler
  ${RTC_LINE}
  restartPolicy: Never
  nodeSelector: { kubernetes.io/hostname: ${NODE} }
  containers:
    - name: main
      image: ${IMAGE}
      command: ["sleep", "86400"]
      resources:
        limits:   $2
        requests: $2
EOF
}

# submit_instance <name> <resources-yaml-map>
submit_instance() {
  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata: { name: $1, namespace: default }
spec:
  type: ${IT}
  image: ${IMAGE}
  command: ["sleep", "86400"]
  volume: { ephemeral: { capacity: 1Gi } }
  resources: $2
EOF
}

# wait_pod_running <name> — the backing Pod is Running. The gate matters beyond liveness: the plugin
# writes the allocation annotation during Allocate, BEFORE the container runs, so a container that
# never started still advertises a placement, and reading it back without this gate would report a
# confident accelerator for a claim that never worked.
wait_pod_running() {
  local i
  for i in $(seq 1 "$RUN_TRIES"); do
    [ "$(kubectl -n default get pod "$1" -o jsonpath='{.status.phase}' 2>/dev/null)" = Running ] && return 0
    sleep 3
  done
  echo "[case-38] ${1} never reached Running"
  kubectl -n default get pod "$1" \
    -o jsonpath='  waiting={.status.containerStatuses[0].state.waiting.reason} exit={.status.containerStatuses[0].state.terminated.exitCode}{"\n"}' 2>/dev/null
  kubectl -n default describe pod "$1" 2>/dev/null | tail -12
  return 1
}

# wait_instance_ready <name> — the Instance reports a running phase, and its backing Pod is Running.
wait_instance_ready() {
  local i phase=""
  for i in $(seq 1 "$RUN_TRIES"); do
    phase=$(kubectl -n default get instance "$1" -o jsonpath='{.status.phase}' 2>/dev/null)
    { [ "$phase" = Ready ] || [ "$phase" = Running ]; } && break
    sleep 3
  done
  if [ "$phase" != Ready ] && [ "$phase" != Running ]; then
    echo "[case-38] instance ${1} phase='${phase:-<none>}'"
    kubectl -n default describe instance "$1" 2>/dev/null | tail -15
    return 1
  fi
  wait_pod_running "$1"
}

# ---------------------------------------------------------------------------------------------
# What the operator recorded
# ---------------------------------------------------------------------------------------------

# placement_of <pod> — one line per allocated accelerator: "<id> <mode> <allocated> <runStart> <runLength>".
# A missing compute run prints "- -": an exclusive claim carries none by design.
placement_of() {
  kubectl -n default get pod "$1" -o json 2>/dev/null | python3 -c "
import json,sys
ann=(json.load(sys.stdin)['metadata'].get('annotations') or {}).get('device.gpustack.ai/accelerator.allocated')
if not ann: sys.exit(1)
a=json.loads(ann).get('main') or {}
modes={0:'free',1:'exclusive',2:'shared',3:'sliced',4:'partitioned'}
n=0
for g in a.get('devices',{}).get('groups',[]):
    for c in g.get('accelerators',[]):
        runs=c.get('allocatedLogicalPlacements') or []
        s,l=(runs[0].get('start'),runs[0].get('length')) if runs else ('-','-')
        print(c.get('id',''), modes.get(c.get('mode'),'?'), c.get('allocated'), s, l)
        n+=1
sys.exit(0 if n else 1)
"
}

# metrics_of <instance> — the metrics subresource payload, or empty.
metrics_of() {
  kubectl get --raw "/apis/worker.gpustack.ai/v1/namespaces/default/instances/$1/metrics" 2>/dev/null
}

# accel_remaining <accelerator-id> — that accelerator's remaining budget in the runtime ledger.
# A fully allocated accelerator OMITS the field rather than writing a zero, so an absent value is
# read as 0; defaulting it to the accelerator's budget instead would report a free accelerator for
# one that has nothing left. Prints nothing when the ledger does not mention the accelerator at all,
# which is a different condition and left for the caller to judge.
accel_remaining() {
  kubectl get devices "$NODE" -o json 2>/dev/null | GID="$GROUPID" WANT="$1" python3 -c "
import json,os,sys
gid=os.environ['GID']; want=os.environ['WANT']
for g in json.load(sys.stdin).get('status',{}).get('groups',[]):
    if gid and g.get('id')!=gid: continue
    for a in g.get('accelerators',[]):
        if a.get('id')==want:
            print(int(a.get('remaining') or 0)); sys.exit(0)
"
}

# ---------------------------------------------------------------------------------------------
# What the container sees, with AMD's own tooling
# ---------------------------------------------------------------------------------------------

# ctr_device_access <pod> — "kfd=<0|1> render=<n>": whether the accelerator control device and how
# many render nodes the container holds. Every reading below depends on this, and it is checked
# first so that "the tool reported the wrong figure" is never confused with "the container was given
# no accelerator at all". Retried: a Pod reports Running slightly before its container is attachable.
ctr_device_access() {
  local out i
  for i in $(seq 1 10); do
    out=$(kubectl -n default exec "$1" -c main -- sh -c 'k=0; [ -e /dev/kfd ] && k=1; r=$(ls /dev/dri 2>/dev/null | grep -c "^renderD"); echo "kfd=${k} render=${r:-0}"' 2>/dev/null | tr -d '\r' | awk 'NF{last=$0} END{print last}')
    [ -n "$out" ] && { printf '%s\n' "$out"; return 0; }
    sleep 3
  done
  return 1
}

# ctr_visible_env <pod> — the visible-devices values the allocation injected, read back from inside
# the container. The container runtime turns the first of them into device nodes, so when the
# container has no accelerator device this is the value to look at: naming it turns "the container
# saw nothing" into a diagnosis instead of a symptom.
ctr_visible_env() {
  kubectl -n default exec "$1" -c main -- sh -c \
    'echo "AMD_VISIBLE_DEVICES=${AMD_VISIBLE_DEVICES-<unset>} ROCR_VISIBLE_DEVICES=${ROCR_VISIBLE_DEVICES-<unset>}"' \
    2>/dev/null | tr -d '\r' | awk 'NF{last=$0} END{print last}'
}

# ctr_hsa_cu_mask <pod> — the compute mask the allocation injected, read back from inside the
# container as the variable itself. Kept independent of the bundled reader on purpose: the reader is
# cross-checked against this value, and reading both from the reader would make that cross-check
# vacuous.
ctr_hsa_cu_mask() {
  kubectl -n default exec "$1" -c main -- sh -c 'echo "${HSA_CU_MASK-<unset>}"' \
    2>/dev/null | tr -d '\r' | awk 'NF{last=$0} END{print last}'
}

# ctr_accelerators <pod> — one line per accelerator AMD's tooling reports inside the container:
# "<pci> <totalVramMiB>". PCI address first, because that is the identity this case correlates on.
ctr_accelerators() {
  kubectl -n default exec "$1" -c main -- sh -c '
    export PATH=${PATH}:/opt/rocm/bin
    command -v amd-smi >/dev/null 2>&1 || { echo NOTOOL; exit 0; }
    amd-smi list --json 2>/dev/null
    echo ---
    amd-smi metric --mem-usage --json 2>/dev/null
  ' 2>/dev/null | python3 -c "
import json,sys
raw=sys.stdin.read()
if 'NOTOOL' in raw.split('\n')[0]:
    print('NOTOOL'); sys.exit(0)
head,_,tail=raw.partition('---')
try:
    lst=json.loads(head.strip() or '[]')
except ValueError:
    print('NOPARSE'); sys.exit(0)
mem={}
try:
    for e in (json.loads(tail.strip() or '{}').get('gpu_data') or []):
        v=((e.get('mem_usage') or {}).get('total_vram') or {}).get('value')
        mem[e.get('gpu')]=v
except ValueError:
    pass
for e in lst:
    print(e.get('bdf',''), mem.get(e.get('gpu'),'-'))
"
}

# ctr_temp_power <pod> — one line per accelerator: "<pci> <hotspotC> <socketW>", read with the same
# tool the device manager's own snapshot comes from, so the cross-check compares like with like.
ctr_temp_power() {
  kubectl -n default exec "$1" -c main -- sh -c '
    export PATH=${PATH}:/opt/rocm/bin
    command -v amd-smi >/dev/null 2>&1 || { echo NOTOOL; exit 0; }
    amd-smi list --json 2>/dev/null
    echo ---
    amd-smi metric --temperature --power --json 2>/dev/null
  ' 2>/dev/null | python3 -c "
import json,sys
raw=sys.stdin.read()
if 'NOTOOL' in raw.split('\n')[0]:
    print('NOTOOL'); sys.exit(0)
head,_,tail=raw.partition('---')
try:
    lst=json.loads(head.strip() or '[]'); met=json.loads(tail.strip() or '{}')
except ValueError:
    print('NOPARSE'); sys.exit(0)
tp={}
for e in (met.get('gpu_data') or []):
    t=((e.get('temperature') or {}).get('hotspot') or {}).get('value')
    p=((e.get('power') or {}).get('socket_power') or {}).get('value')
    tp[e.get('gpu')]=(t,p)
for e in lst:
    t,p=tp.get(e.get('gpu'),('-','-'))
    print(e.get('bdf',''), t, p)
"
}

# ctr_hip_total_mib <pod> — the memory total the HIP surface reports for the container's first
# accelerator, in MiB. This is the surface a logical slice's memory cap governs: the vendor's
# system-management tool reads the accelerator's physical capacity out of the kernel and is
# unaffected by the cap, so it is the wrong instrument for this row and amd-smi is deliberately not
# used here. Prints NOHIPCC / NODEV / BUILDFAIL instead of a figure when it cannot measure.
ctr_hip_total_mib() {
  kubectl -n default exec "$1" -c main -- sh -c '
    export PATH=${PATH}:/opt/rocm/bin
    command -v hipcc >/dev/null 2>&1 || { echo NOHIPCC; exit 0; }
    cat > /tmp/vram.cpp <<"CPP"
#include <hip/hip_runtime.h>
#include <cstdio>
int main(){
  int n=0;
  if(hipGetDeviceCount(&n)!=hipSuccess||n<1){printf("NODEV\n");return 0;}
  size_t f=0,t=0; hipSetDevice(0); hipMemGetInfo(&f,&t);
  size_t d=0; hipDeviceTotalMem(&d,0);
  printf("%zu %zu\n",(size_t)(t>>20),(size_t)(d>>20));
  return 0;
}
CPP
    hipcc -o /tmp/vram /tmp/vram.cpp >/tmp/vram.log 2>&1 || { echo BUILDFAIL; exit 0; }
    /tmp/vram
  ' 2>/dev/null | tr -d '\r' | awk 'NF{last=$0} END{print last}'
}

# ctr_cumask <pod> — the bundled CU-mask reader's report: the accelerator topology it sees, the mask
# it was given, and its own conformance verdicts. The one instrument that reads the compute window
# from inside, and it reports the accelerator's compute-unit count too, so the expected window length
# needs no assumption about the part.
ctr_cumask() {
  kubectl -n default exec "$1" -c main -- sh -c '
    [ -x /usr/local/vrocm/rocm-cumask-check ] || { echo NOREADER; exit 0; }
    /usr/local/vrocm/rocm-cumask-check 2>&1
  ' 2>/dev/null | tr -d '\r'
}

# ---------------------------------------------------------------------------------------------
# Shared readings
# ---------------------------------------------------------------------------------------------

# assert_visible <pod> <label> <expected-id-csv> <expect-whole-memory:yes|no> — the container-side
# half of a row: device access, then the accelerators AMD's tooling reports, correlated to the
# allocation by PCI address, and (for a whole-accelerator claim) each one's full physical capacity.
assert_visible() {
  local pod="$1" label="$2" ids="$3" whole="$4"
  local access want_n got_n bdfs_want bdfs_got id
  want_n=$(printf '%s' "$ids" | tr ',' '\n' | grep -c .)

  access=$(ctr_device_access "$pod")
  if [ -z "$access" ]; then
    record FAIL "${label}: the container holds accelerator device access" \
      "could not read /dev inside the container at all"
    record FAIL "${label}: AMD tooling sees exactly the allocated accelerators" "not evaluated"
    [ "$whole" = yes ] && record FAIL "${label}: each visible accelerator reports its full memory" "not evaluated"
    return 1
  fi
  local kfd render
  kfd=$(printf '%s' "$access" | sed -n 's/.*kfd=\([0-9]*\).*/\1/p')
  render=$(printf '%s' "$access" | sed -n 's/.*render=\([0-9]*\).*/\1/p')
  local injected=""
  { [ "${kfd:-0}" = 1 ] && [ "${render:-0}" -eq "$want_n" ]; } || injected=$(ctr_visible_env "$pod")
  { [ "${kfd:-0}" = 1 ] && [ "${render:-0}" -eq "$want_n" ]; }
  verdict $? "${label}: the container holds accelerator device access" \
    "/dev/kfd present and ${render} render node(s) for ${want_n} allocated accelerator(s)" \
    "${access}, expected kfd=1 and ${want_n} render node(s); the allocation injected ${injected:-<unreadable>} — the container runtime built no device set from it"
  if [ "${kfd:-0}" != 1 ] || [ "${render:-0}" -eq 0 ]; then
    record FAIL "${label}: AMD tooling sees exactly the allocated accelerators" \
      "not evaluated — the container was given no accelerator device to read"
    [ "$whole" = yes ] && record FAIL "${label}: each visible accelerator reports its full memory" \
      "not evaluated — the container was given no accelerator device to read"
    return 1
  fi

  local seen
  seen=$(ctr_accelerators "$pod")
  case "$seen" in
    NOTOOL*)
      record SKIP "${label}: AMD tooling sees exactly the allocated accelerators" \
        "the carrier ${IMAGE} ships no amd-smi — set E2E_AMD_IMAGE to a ROCm-family image"
      [ "$whole" = yes ] && record SKIP "${label}: each visible accelerator reports its full memory" \
        "the carrier ships no amd-smi"
      return 1 ;;
    NOPARSE*|"")
      record FAIL "${label}: AMD tooling sees exactly the allocated accelerators" \
        "amd-smi produced nothing this case could read"
      [ "$whole" = yes ] && record FAIL "${label}: each visible accelerator reports its full memory" "not evaluated"
      return 1 ;;
  esac

  bdfs_want=$(for id in $(printf '%s' "$ids" | tr ',' ' '); do bdf_of "$id"; done | sort | tr '\n' ',')
  bdfs_got=$(printf '%s\n' "$seen" | awk 'NF{print $1}' | sort | tr '\n' ',')
  got_n=$(printf '%s\n' "$seen" | awk 'NF' | wc -l | tr -d ' ')
  [ -n "$bdfs_got" ] && [ "$bdfs_got" = "$bdfs_want" ]
  verdict $? "${label}: AMD tooling sees exactly the allocated accelerators" \
    "${got_n} accelerator(s) at PCI {${bdfs_got%,}} == the allocation's {${bdfs_want%,}}" \
    "PCI {${bdfs_got%,}} vs the allocation's {${bdfs_want%,}} — the container is not confined to what was allocated"

  if [ "$whole" = yes ]; then
    local bad=0 line mem
    while read -r _ mem; do
      [ -n "${mem:-}" ] || continue
      case "$mem" in ''|*[!0-9]*) bad=$((bad + 1)); continue ;; esac
      # A whole-accelerator claim must see the physical capacity. Allow a small shortfall: the
      # vendor's tool reports the same figure the detector recorded, but a firmware carve-out can
      # move it by a few MiB between readings.
      [ "$mem" -ge $((CARD_MEM_MIB - 64)) ] || bad=$((bad + 1))
    done <<EOF
$(printf '%s\n' "$seen")
EOF
    line=$(printf '%s\n' "$seen" | awk 'NF{printf "%s=%sMiB ", $1, $2}')
    [ "$bad" -eq 0 ]
    verdict $? "${label}: each visible accelerator reports its full memory" \
      "${line}(physical ${CARD_MEM_MIB}MiB)" \
      "${line}— expected the physical ${CARD_MEM_MIB}MiB on a whole-accelerator claim"
  fi
  return 0
}

# assert_metrics_exclusive <instance> <label> <expected-id-csv> — the metrics subresource on an
# exclusive Instance: one entry per allocated accelerator, matched by identity; the physical
# capacity; both utilizations in range; and temperature/power cross-checked against the container's
# own reading of the same accelerator.
assert_metrics_exclusive() {
  local inst="$1" label="$2" ids="$3"
  local payload want_n summary
  want_n=$(printf '%s' "$ids" | tr ',' '\n' | grep -c .)

  # The accelerator section comes from the device manager's latest snapshot, which lands a monitor
  # period after the claim, so poll rather than read once.
  payload=""
  local i
  for i in $(seq 1 20); do
    payload=$(metrics_of "$inst")
    [ -n "$payload" ] && printf '%s' "$payload" | grep -q '"accelerators"' && break
    sleep 3
  done
  if [ -z "$payload" ]; then
    record FAIL "${label}: metrics subresource answers" "the subresource returned nothing"
    for c in "accelerator entry per allocated accelerator" "entry identity matches the allocation" \
             "reported memory is the physical capacity" "utilizations within [0,100]" \
             "temperature and power reported plausibly"; do
      record FAIL "${label}: ${c}" "not evaluated — no metrics payload"
    done
    return 1
  fi
  record PASS "${label}: metrics subresource answers" "one sample returned for ${inst}"

  summary=$(printf '%s' "$payload" | IDS="$ids" WANT_N="$want_n" MEM="$CARD_MEM_MIB" python3 -c "
import json,os,sys
d=json.load(sys.stdin).get('sample',{})
acc=d.get('accelerators') or []
want=[x for x in os.environ['IDS'].split(',') if x]
wn=int(os.environ['WANT_N']); mem=int(os.environ['MEM'])
print('count', len(acc), wn)
print('ids', ','.join(sorted(a.get('id','') for a in acc)) or '-', ','.join(sorted(want)))
bad_mem=[]; bad_util=[]; tp=[]
for a in acc:
    m=a.get('memoryTotalMiB')
    if m is None or abs(int(m)-mem)>64: bad_mem.append('%s=%s'%(a.get('id'),m))
    for k in ('memoryUtilizationPercent','coresUtilizationPercent'):
        v=a.get(k)
        if v is None or not (0<=int(v)<=100): bad_util.append('%s.%s=%s'%(a.get('id'),k,v))
    tp.append('%s:%s:%s'%(a.get('id'), a.get('temperatureCelsius'), a.get('powerUsageWatts')))
print('mem', ','.join('%s=%sMiB'%(a.get('id'),a.get('memoryTotalMiB')) for a in acc) or '-', ','.join(bad_mem) or '-')
print('util', ','.join('%s/%s'%(a.get('memoryUtilizationPercent'),a.get('coresUtilizationPercent')) for a in acc) or '-', ','.join(bad_util) or '-')
print('tp', ','.join(tp) or '-')
")
  local got_n
  got_n=$(printf '%s\n' "$summary" | awk '$1=="count"{print $2}')
  [ "${got_n:-0}" -eq "$want_n" ]
  verdict $? "${label}: accelerator entry per allocated accelerator" \
    "${got_n} entries for ${want_n} allocated accelerator(s)" \
    "${got_n:-0} entries for ${want_n} allocated accelerator(s)"

  local got_ids want_ids
  got_ids=$(printf '%s\n' "$summary" | awk '$1=="ids"{print $2}')
  want_ids=$(printf '%s\n' "$summary" | awk '$1=="ids"{print $3}')
  [ -n "$got_ids" ] && [ "$got_ids" = "$want_ids" ]
  verdict $? "${label}: entry identity matches the allocation" \
    "ids {${got_ids}} == the allocation's {${want_ids}}" \
    "ids {${got_ids}} vs the allocation's {${want_ids}}"

  local mem_seen mem_bad
  mem_seen=$(printf '%s\n' "$summary" | awk '$1=="mem"{print $2}')
  mem_bad=$(printf '%s\n' "$summary" | awk '$1=="mem"{print $3}')
  [ "$mem_bad" = "-" ]
  verdict $? "${label}: reported memory is the physical capacity" \
    "${mem_seen} (physical ${CARD_MEM_MIB}MiB)" \
    "off-capacity entries: ${mem_bad} (physical ${CARD_MEM_MIB}MiB)"

  local util_seen util_bad
  util_seen=$(printf '%s\n' "$summary" | awk '$1=="util"{print $2}')
  util_bad=$(printf '%s\n' "$summary" | awk '$1=="util"{print $3}')
  [ "$util_bad" = "-" ]
  verdict $? "${label}: utilizations within [0,100]" \
    "memory/cores utilization ${util_seen}" \
    "out-of-range or absent: ${util_bad}"

  # Temperature and power: reported at all, plausible, and the same story the container tells.
  local tp ctr_tp bad_plaus="" bad_cross="" seen_any=0
  tp=$(printf '%s\n' "$summary" | awk '$1=="tp"{print $2}')
  ctr_tp=$(ctr_temp_power "$inst")
  local entry id t p bdf ct cp
  for entry in $(printf '%s' "$tp" | tr ',' ' '); do
    id=${entry%%:*}; t=$(printf '%s' "$entry" | cut -d: -f2); p=$(printf '%s' "$entry" | cut -d: -f3)
    case "${t}:${p}" in
      *[!0-9:]*|:*|*:) continue ;;   # absent or non-numeric: the library reported neither
    esac
    if [ "$t" -eq 0 ] || [ "$p" -eq 0 ]; then
      continue   # not reported by the library at sampling time; the plausibility row says so below
    fi
    seen_any=1
    { [ "$t" -ge 1 ] && [ "$t" -le 125 ] && [ "$p" -ge 1 ] && [ "$p" -le 1000 ]; } \
      || bad_plaus="${bad_plaus}${id}=${t}C/${p}W "
    bdf=$(bdf_of "$id")
    case "$ctr_tp" in NOTOOL*|NOPARSE*|"") continue ;; esac
    read -r ct cp <<<"$(printf '%s\n' "$ctr_tp" | awk -v b="$bdf" '$1==b{print $2, $3; exit}')"
    case "${ct:-}:${cp:-}" in
      *[!0-9:]*|:*|*:)
        bad_cross="${bad_cross}${id}@${bdf}=<container-silent:${ct:-?}/${cp:-?}> "
        continue ;;
    esac
    # Both readings are live samples taken seconds apart, so they agree on the scale, not the value.
    python3 -c "
import sys
t,p,ct,cp=${t},${p},${ct},${cp}
sys.exit(0 if abs(t-ct)<=15 and abs(p-cp)<=max(25, int(0.5*max(p,cp))) else 1)
" || bad_cross="${bad_cross}${id}: metrics ${t}C/${p}W vs container ${ct}C/${cp}W "
  done
  if [ "$seen_any" -eq 0 ]; then
    record SKIP "${label}: temperature and power reported plausibly" \
      "no entry carried both figures (${tp}) — the device library reported neither at sampling time"
    record SKIP "${label}: temperature and power agree with the container's own reading" \
      "not evaluated — the metrics sample carried no temperature/power to compare"
  else
    [ -z "$bad_plaus" ]
    verdict $? "${label}: temperature and power reported plausibly" \
      "${tp}" \
      "implausible: ${bad_plaus}(seen ${tp})"
    [ -z "$bad_cross" ]
    verdict $? "${label}: temperature and power agree with the container's own reading" \
      "metrics ${tp} against the container's own amd-smi" \
      "${bad_cross}"
  fi
  return 0
}

# assert_sliced_memory <pod> <label> <expected-cap-mib> — the memory cap, read from inside through
# the surface the cap governs.
assert_sliced_memory() {
  local pod="$1" label="$2" want="$3"
  local out t d
  out=$(ctr_hip_total_mib "$pod")
  case "$out" in
    NOHIPCC*)
      record SKIP "${label}: the memory the accelerator reports is the requested share" \
        "the carrier ${IMAGE} ships no HIP compiler, so the capped capacity cannot be read from inside; set E2E_AMD_IMAGE to a ROCm devel image"
      return 0 ;;
    NODEV*)
      record FAIL "${label}: the memory the accelerator reports is the requested share" \
        "the HIP surface reports no accelerator inside the container, so there is nothing whose capacity to read"
      return 1 ;;
    BUILDFAIL*)
      record FAIL "${label}: the memory the accelerator reports is the requested share" \
        "the in-container HIP probe failed to build"
      return 1 ;;
  esac
  read -r t d <<<"$out"
  { [ -n "${t:-}" ] && [ "$t" = "$want" ] && [ "${d:-}" = "$want" ]; }
  verdict $? "${label}: the memory the accelerator reports is the requested share" \
    "HIP reports ${t}MiB total (and ${d}MiB device total) == the requested ${want}MiB, below the physical ${CARD_MEM_MIB}MiB" \
    "HIP reports total='${t:-?}'MiB device-total='${d:-?}'MiB, wanted ${want}MiB (physical ${CARD_MEM_MIB}MiB)"
}

# ---------------------------------------------------------------------------------------------
# Phase 1 — exclusive, one accelerator, over both carriers
# ---------------------------------------------------------------------------------------------
echo
echo "[case-38] phase 1: exclusive whole accelerator, one each over the Instance and Pod carriers"

EX1_IDS=""
if [ "$INSTANCE_CARRIER" = yes ]; then
  submit_instance "$EX_INST1" "{ accelerator: \"1\" }"
  if wait_instance_ready "$EX_INST1"; then
    record PASS "exclusive/Instance/1: claim reaches a running phase" "${EX_INST1} running on pool ${IT}"
    EX1_IDS=$(placement_of "$EX_INST1" | awk 'NF{printf "%s,", $1}'); EX1_IDS=${EX1_IDS%,}
    modes=$(placement_of "$EX_INST1" | awk 'NF{printf "%s ", $2}')
    echo "[case-38] ${EX_INST1} → {${EX1_IDS}} mode(s) ${modes}"
    [ -n "$EX1_IDS" ] && [ "$(printf '%s' "$modes" | tr -d ' ')" = exclusive ]
    verdict $? "exclusive/Instance/1: recorded as one exclusive accelerator" \
      "{${EX1_IDS}} mode(s) ${modes}" \
      "ids '{${EX1_IDS}}' mode(s) '${modes}', expected one accelerator held exclusive"
    [ -n "$EX1_IDS" ] && assert_visible "$EX_INST1" "exclusive/Instance/1" "$EX1_IDS" yes
    [ -n "$EX1_IDS" ] && assert_metrics_exclusive "$EX_INST1" "exclusive/Instance/1" "$EX1_IDS"
  else
    record FAIL "exclusive/Instance/1: claim reaches a running phase" "${EX_INST1} never ran"
  fi
else
  for c in "claim reaches a running phase" "recorded as one exclusive accelerator" \
           "the container holds accelerator device access" \
           "AMD tooling sees exactly the allocated accelerators" \
           "each visible accelerator reports its full memory" \
           "metrics subresource answers"; do
    record SKIP "exclusive/Instance/1: ${c}" "no default StorageClass, so no Instance can mount its workspace volume"
  done
fi

submit_pod "$EX_POD1" "{ ${BASE}: \"1\" }"
if wait_pod_running "$EX_POD1"; then
  record PASS "exclusive/Pod/1: claim reaches Running" "${EX_POD1} Running on pool ${IT}"
  P1_IDS=$(placement_of "$EX_POD1" | awk 'NF{printf "%s,", $1}'); P1_IDS=${P1_IDS%,}
  p1_modes=$(placement_of "$EX_POD1" | awk 'NF{printf "%s ", $2}')
  echo "[case-38] ${EX_POD1} → {${P1_IDS}} mode(s) ${p1_modes}"
  [ -n "$P1_IDS" ] && [ "$(printf '%s' "$p1_modes" | tr -d ' ')" = exclusive ]
  verdict $? "exclusive/Pod/1: recorded as one exclusive accelerator" \
    "{${P1_IDS}} mode(s) ${p1_modes}" \
    "ids '{${P1_IDS}}' mode(s) '${p1_modes}', expected one accelerator held exclusive"
  if [ -n "$P1_IDS" ]; then
    assert_visible "$EX_POD1" "exclusive/Pod/1" "$P1_IDS" yes
    # Two single-accelerator exclusive claims coexisting must not name the same accelerator.
    if [ -n "$EX1_IDS" ]; then
      [ "$P1_IDS" != "$EX1_IDS" ]
      verdict $? "two coexisting exclusive claims hold different accelerators" \
        "Instance on {${EX1_IDS}}, Pod on {${P1_IDS}}" \
        "both claims name {${P1_IDS}} — one accelerator was handed out twice"
    else
      record SKIP "two coexisting exclusive claims hold different accelerators" \
        "the Instance-carried claim did not place, so there is only one claim to compare"
    fi
  fi
else
  record FAIL "exclusive/Pod/1: claim reaches Running" "${EX_POD1} never reached Running"
fi

echo "[case-38] releasing the phase 1 claims"
kubectl -n default delete instance "$EX_INST1" --ignore-not-found >/dev/null 2>&1 || true
kubectl -n default delete pod "$EX_POD1" --ignore-not-found --force --grace-period=0 >/dev/null 2>&1 || true
wait_pool '{.status.accelerator.remaining}' "${EX_BASE:-}" || true

# ---------------------------------------------------------------------------------------------
# Phase 2 — exclusive, two accelerators in ONE claim, Instance carrier
# ---------------------------------------------------------------------------------------------
echo
echo "[case-38] phase 2: one exclusive claim for two accelerators, Instance carrier"

TWO_CHECKS=("claim reaches a running phase" "recorded as two DISTINCT accelerators"
            "the container holds accelerator device access"
            "AMD tooling sees exactly the allocated accelerators"
            "each visible accelerator reports its full memory")
if [ "$NCARDS" -lt 2 ]; then
  for c in "${TWO_CHECKS[@]}"; do
    record SKIP "exclusive/Instance/2: ${c}" "${NODE} has ${NCARDS} healthy accelerator(s); two are needed for a two-accelerator claim"
  done
elif [ "$INSTANCE_CARRIER" != yes ]; then
  for c in "${TWO_CHECKS[@]}"; do
    record SKIP "exclusive/Instance/2: ${c}" "no default StorageClass, so no Instance can mount its workspace volume"
  done
elif [ "$INST2_FITS" != yes ]; then
  for c in "${TWO_CHECKS[@]}"; do
    record SKIP "exclusive/Instance/2: ${c}" "the Instance carrier sizes a two-accelerator claim by the pool's unit resources and it does not fit this node (${INST2_WHY}); the Pod carrier covers this row"
  done
else
  submit_instance "$EX_INST2" "{ accelerator: \"2\" }"
  if wait_instance_ready "$EX_INST2"; then
    record PASS "exclusive/Instance/2: claim reaches a running phase" "${EX_INST2} running"
    I2_IDS=$(placement_of "$EX_INST2" | awk 'NF{printf "%s,", $1}'); I2_IDS=${I2_IDS%,}
    i2_n=$(printf '%s' "$I2_IDS" | tr ',' '\n' | grep -c .)
    i2_u=$(printf '%s' "$I2_IDS" | tr ',' '\n' | sort -u | grep -c .)
    echo "[case-38] ${EX_INST2} → {${I2_IDS}}"
    { [ "$i2_n" -eq 2 ] && [ "$i2_u" -eq 2 ]; }
    verdict $? "exclusive/Instance/2: recorded as two DISTINCT accelerators" \
      "{${I2_IDS}}" \
      "{${I2_IDS}} — ${i2_n} entries, ${i2_u} distinct; a two-accelerator claim must occupy two"
    [ -n "$I2_IDS" ] && assert_visible "$EX_INST2" "exclusive/Instance/2" "$I2_IDS" yes
    [ -n "$I2_IDS" ] && assert_metrics_exclusive "$EX_INST2" "exclusive/Instance/2" "$I2_IDS"
  else
    record FAIL "exclusive/Instance/2: claim reaches a running phase" "${EX_INST2} never ran"
  fi
  kubectl -n default delete instance "$EX_INST2" --ignore-not-found >/dev/null 2>&1 || true
  wait_pool '{.status.accelerator.remaining}' "${EX_BASE:-}" || true
fi

# ---------------------------------------------------------------------------------------------
# Phase 3 — exclusive, two accelerators in ONE claim, Pod carrier
# ---------------------------------------------------------------------------------------------
echo
echo "[case-38] phase 3: one exclusive claim for two accelerators, Pod carrier"

if [ "$NCARDS" -lt 2 ]; then
  for c in "${TWO_CHECKS[@]}"; do
    record SKIP "exclusive/Pod/2: ${c}" "${NODE} has ${NCARDS} healthy accelerator(s); two are needed"
  done
else
  submit_pod "$EX_POD2" "{ ${BASE}: \"2\" }"
  if wait_pod_running "$EX_POD2"; then
    record PASS "exclusive/Pod/2: claim reaches Running" "${EX_POD2} Running"
    P2_IDS=$(placement_of "$EX_POD2" | awk 'NF{printf "%s,", $1}'); P2_IDS=${P2_IDS%,}
    p2_n=$(printf '%s' "$P2_IDS" | tr ',' '\n' | grep -c .)
    p2_u=$(printf '%s' "$P2_IDS" | tr ',' '\n' | sort -u | grep -c .)
    echo "[case-38] ${EX_POD2} → {${P2_IDS}}"
    { [ "$p2_n" -eq 2 ] && [ "$p2_u" -eq 2 ]; }
    verdict $? "exclusive/Pod/2: recorded as two DISTINCT accelerators" \
      "{${P2_IDS}}" \
      "{${P2_IDS}} — ${p2_n} entries, ${p2_u} distinct; a two-accelerator claim must occupy two"
    [ -n "$P2_IDS" ] && assert_visible "$EX_POD2" "exclusive/Pod/2" "$P2_IDS" yes
  else
    record FAIL "exclusive/Pod/2: claim reaches Running" "${EX_POD2} never reached Running"
  fi
  kubectl -n default delete pod "$EX_POD2" --ignore-not-found --force --grace-period=0 >/dev/null 2>&1 || true
  wait_pool '{.status.accelerator.remaining}' "${EX_BASE:-}" || true
fi

# ---------------------------------------------------------------------------------------------
# Phase 4 — logical slices
# ---------------------------------------------------------------------------------------------
echo
echo "[case-38] phase 4: logical slices — memory percentage, memory MiB, cores percentage"

MEM_PCT_MIB=$((CARD_MEM_MIB * MEM_PCT / 100))
# The compute run a share of the accelerator's compute units comes to, when the accelerator can
# express that share exactly. Derived from the accelerator's own unit count, never assumed.
WANT_CU=$((CARD_CU * CORES_PCT / 100))
echo "[case-38] ${MEM_PCT}% of ${CARD_MEM_MIB}MiB = ${MEM_PCT_MIB}MiB; ${CORES_PCT}% of ${CARD_CU} CUs = ${WANT_CU} CUs"

# 4a — memory percentage, Instance carrier. Also the sliced-metrics OBSERVATION.
if [ "$INSTANCE_CARRIER" = yes ]; then
  submit_instance "$SL_INST" \
    "{ accelerator: \"1\", acceleratorSlicedMemoryPercentage: ${MEM_PCT}, acceleratorSlicedCoresPercentage: ${MEM_PCT} }"
  if wait_instance_ready "$SL_INST"; then
    record PASS "sliced-memory-%/Instance/1: claim reaches a running phase" "${SL_INST} running at ${MEM_PCT}%"
    SI_IDS=$(placement_of "$SL_INST" | awk 'NF{printf "%s,", $1}'); SI_IDS=${SI_IDS%,}
    si_mode=$(placement_of "$SL_INST" | awk 'NF{print $2; exit}')
    echo "[case-38] ${SL_INST} → {${SI_IDS}} mode ${si_mode}"
    [ "$si_mode" = sliced ]
    verdict $? "sliced-memory-%/Instance/1: recorded as a logical slice" \
      "{${SI_IDS}} mode ${si_mode}" \
      "mode '${si_mode:-?}', expected sliced"
    if [ -n "$SI_IDS" ]; then
      assert_visible "$SL_INST" "sliced-memory-%/Instance/1" "$SI_IDS" no
      assert_sliced_memory "$SL_INST" "sliced-memory-%/Instance/1" "$MEM_PCT_MIB"
    fi

    # OBSERVATION, never a verdict: per-slice accelerator metrics are not offered, so record what
    # the subresource returns — an absent array, or the whole accelerator's figures — and say which.
    obs=$(metrics_of "$SL_INST" | MEM="$CARD_MEM_MIB" python3 -c "
import json,os,sys
try:
    s=json.load(sys.stdin).get('sample',{})
except ValueError:
    print('the subresource returned nothing readable'); sys.exit(0)
acc=s.get('accelerators') or []
mem=int(os.environ['MEM'])
if not acc:
    print('the accelerators array is ABSENT on a sliced Instance'); sys.exit(0)
parts=[]
for a in acc:
    m=a.get('memoryTotalMiB')
    scope='WHOLE-ACCELERATOR' if m is not None and abs(int(m)-mem)<=64 else 'slice-scoped-or-other'
    parts.append('%s memoryTotalMiB=%s (%s) memUtil=%s coresUtil=%s temp=%s power=%s'%(
        a.get('id'), m, scope, a.get('memoryUtilizationPercent'),
        a.get('coresUtilizationPercent'), a.get('temperatureCelsius'), a.get('powerUsageWatts')))
print('the array is PRESENT with %d entries: %s'%(len(acc), '; '.join(parts)))
")
    record SKIP "sliced Instance metrics (OBSERVATION, not a verdict)" "${obs}"
  else
    record FAIL "sliced-memory-%/Instance/1: claim reaches a running phase" "${SL_INST} never ran"
    record SKIP "sliced Instance metrics (OBSERVATION, not a verdict)" "not observed — the sliced Instance did not run"
  fi
else
  for c in "claim reaches a running phase" "recorded as a logical slice" \
           "the container holds accelerator device access" \
           "AMD tooling sees exactly the allocated accelerators" \
           "the memory the accelerator reports is the requested share"; do
    record SKIP "sliced-memory-%/Instance/1: ${c}" "no default StorageClass, so no Instance can mount its workspace volume"
  done
  record SKIP "sliced Instance metrics (OBSERVATION, not a verdict)" "not observed — the Instance carrier is unavailable"
fi

# 4b — memory percentage, Pod carrier.
submit_pod "$SL_POD_PCT" \
  "{ ${SLICED}: \"1\", ${MEMPCT}: \"${MEM_PCT}\", ${CORESPCT}: \"${MEM_PCT}\" }"
if wait_pod_running "$SL_POD_PCT"; then
  record PASS "sliced-memory-%/Pod/1: claim reaches Running" "${SL_POD_PCT} Running at ${MEM_PCT}%"
  SP_IDS=$(placement_of "$SL_POD_PCT" | awk 'NF{printf "%s,", $1}'); SP_IDS=${SP_IDS%,}
  sp_mode=$(placement_of "$SL_POD_PCT" | awk 'NF{print $2; exit}')
  echo "[case-38] ${SL_POD_PCT} → {${SP_IDS}} mode ${sp_mode}"
  [ "$sp_mode" = sliced ]
  verdict $? "sliced-memory-%/Pod/1: recorded as a logical slice" \
    "{${SP_IDS}} mode ${sp_mode}" "mode '${sp_mode:-?}', expected sliced"
  if [ -n "$SP_IDS" ]; then
    assert_visible "$SL_POD_PCT" "sliced-memory-%/Pod/1" "$SP_IDS" no
    assert_sliced_memory "$SL_POD_PCT" "sliced-memory-%/Pod/1" "$MEM_PCT_MIB"
  fi
else
  record FAIL "sliced-memory-%/Pod/1: claim reaches Running" "${SL_POD_PCT} never reached Running"
fi

# 4c — exact memory size, Pod carrier only: an Instance expresses a slice as a percentage, never MiB.
submit_pod "$SL_POD_MIB" "{ ${SLICED}: \"1\", ${MEMMIB}: \"${MIB_REQ}\" }"
if wait_pod_running "$SL_POD_MIB"; then
  record PASS "sliced-memory-MiB/Pod/1: claim reaches Running" "${SL_POD_MIB} Running at ${MIB_REQ}MiB"
  SM_IDS=$(placement_of "$SL_POD_MIB" | awk 'NF{printf "%s,", $1}'); SM_IDS=${SM_IDS%,}
  echo "[case-38] ${SL_POD_MIB} → {${SM_IDS}}"
  if [ -n "$SM_IDS" ]; then
    assert_visible "$SL_POD_MIB" "sliced-memory-MiB/Pod/1" "$SM_IDS" no
    assert_sliced_memory "$SL_POD_MIB" "sliced-memory-MiB/Pod/1" "$MIB_REQ"
  else
    record FAIL "sliced-memory-MiB/Pod/1: the memory the accelerator reports is the requested share" \
      "no allocation was recorded on the Pod"
  fi
else
  record FAIL "sliced-memory-MiB/Pod/1: claim reaches Running" "${SL_POD_MIB} never reached Running"
fi

# 4d — compute share, Pod carrier.
submit_pod "$SL_POD_CORES" \
  "{ ${SLICED}: \"1\", ${CORESPCT}: \"${CORES_PCT}\", ${MEMPCT}: \"${CORES_ROW_MEM_PCT}\" }"
if wait_pod_running "$SL_POD_CORES"; then
  record PASS "sliced-cores-%/Pod/1: claim reaches Running" "${SL_POD_CORES} Running at ${CORES_PCT}% compute"
  read -r SC_ID sc_mode sc_alloc sc_start sc_len <<<"$(placement_of "$SL_POD_CORES" | head -1)"
  echo "[case-38] ${SL_POD_CORES} → ${SC_ID} mode ${sc_mode} compute run start=${sc_start} length=${sc_len}"

  # The recorded compute run. Over-delivery is the failure that matters: the tenant is charged the
  # share it asked for, and a run wider than that share hands out compute nobody accounted. A run
  # NARROWER than the share is the accelerator's allocation atom refusing to divide the request —
  # real, expected on some parts, and not this case's to judge, so it records the delivered share
  # instead of a verdict.
  if [ -z "${sc_len:-}" ] || [ "${sc_len}" = "-" ]; then
    record FAIL "sliced-cores-%/Pod/1: the recorded compute run is the requested share" \
      "no compute run was recorded on the allocation for a cores-percentage claim"
  elif [ "$sc_len" -gt "$WANT_CU" ]; then
    record FAIL "sliced-cores-%/Pod/1: the recorded compute run is the requested share" \
      "run length ${sc_len} of ${CARD_CU} CUs = $((sc_len * 100 / CARD_CU))%, more than the ${CORES_PCT}% charged"
  elif [ "$sc_len" -eq "$WANT_CU" ]; then
    record PASS "sliced-cores-%/Pod/1: the recorded compute run is the requested share" \
      "run [${sc_start},$((sc_start + sc_len))) = ${sc_len} of ${CARD_CU} CUs = ${CORES_PCT}%"
  else
    record SKIP "sliced-cores-%/Pod/1: the recorded compute run is the requested share" \
      "run length ${sc_len} of ${CARD_CU} CUs = $((sc_len * 100 / CARD_CU))% against ${CORES_PCT}% requested — the accelerator's allocation atom does not divide this share; pick a share it can express via E2E_AMD_CORES_PCT"
  fi

  if [ -n "${SC_ID:-}" ]; then
    assert_visible "$SL_POD_CORES" "sliced-cores-%/Pod/1" "$SC_ID" no
    # The compute window, read from inside by the bundled reader — which also reports the
    # accelerator's own compute-unit count, so the two halves are comparable without assumption.
    cm=$(ctr_cumask "$SL_POD_CORES")
    case "$cm" in
      NOREADER*|"")
        record FAIL "sliced-cores-%/Pod/1: the container's compute window matches the record" \
          "the bundled CU-mask reader is not present in the container, so the injected window cannot be read from inside" ;;
      *)
        ctr_cu=$(printf '%s\n' "$cm" | sed -n 's/.*[[:space:]]cu=\([0-9]*\).*/\1/p' | head -1)
        ctr_mask=$(printf '%s\n' "$cm" | sed -n 's/.*value=\([^[:space:]]*\).*/\1/p' | head -1)
        ctr_lo=$(printf '%s' "$ctr_mask" | sed -n 's/^[0-9]*:\([0-9]*\)-\([0-9]*\)$/\1/p')
        ctr_hi=$(printf '%s' "$ctr_mask" | sed -n 's/^[0-9]*:\([0-9]*\)-\([0-9]*\)$/\2/p')
        ctr_fails=$(printf '%s\n' "$cm" | sed -n 's/^FAILS=\([0-9]*\)$/\1/p' | head -1)
        echo "[case-38] in-container CU-mask reader: cu=${ctr_cu:-?} mask=${ctr_mask:-?} FAILS=${ctr_fails:-?}"
        if [ -z "${ctr_lo:-}" ] || [ -z "${ctr_hi:-}" ]; then
          record FAIL "sliced-cores-%/Pod/1: the container's compute window matches the record" \
            "the reader produced no readable mask: $(printf '%s' "$cm" | tr '\n' ' ' | cut -c1-160)"
        else
          ctr_len=$((ctr_hi - ctr_lo + 1))
          { [ "$ctr_len" -eq "$sc_len" ] && [ "$ctr_lo" -eq "$sc_start" ] && \
            [ "${ctr_cu:-0}" -eq "$CARD_CU" ] && [ "${ctr_fails:-1}" -eq 0 ]; }
          verdict $? "sliced-cores-%/Pod/1: the container's compute window matches the record" \
            "mask ${ctr_mask} = ${ctr_len} of ${ctr_cu} CUs, identical to the recorded run [${sc_start},$((sc_start + sc_len))), reader conformance FAILS=${ctr_fails}" \
            "mask '${ctr_mask}' = ${ctr_len} CUs of ${ctr_cu:-?} against the recorded run start=${sc_start} length=${sc_len} (accelerator ${CARD_CU} CUs), reader conformance FAILS=${ctr_fails:-?}"
        fi ;;
    esac
  fi
else
  record FAIL "sliced-cores-%/Pod/1: claim reaches Running" "${SL_POD_CORES} never reached Running"
fi

# ---------------------------------------------------------------------------------------------
# Phase 5 — two logical slices on ONE accelerator
#
# Every sliced row above is one claim on one accelerator, and in that shape the first window always
# starts at 0 — so the placement's real job, seating a new window BESIDE the windows the node's live
# allocations already hold, is never exercised. Two slices on one accelerator is the only shape that
# does, and a window-overlap defect there would hand two containers the same compute units while
# every ledger reading still looked correct.
#
# Both claims are Pods: this phase asserts a property of the placement ledger, not of a carrier, and
# a sliced Instance is sized by the pool's unit RAM, which on a single-node host can exhaust the node
# long before the accelerator's budget is spent.
# ---------------------------------------------------------------------------------------------
echo
echo "[case-38] phase 5: two logical slices coexisting on one physical accelerator"

# Phase 4's claims go back first: this phase seats two shares on ONE accelerator and computes every
# expectation from that accelerator's pre-claim budget, which needs the pool as it started.
kubectl -n default delete instance "$SL_INST" --ignore-not-found >/dev/null 2>&1 || true
for n in "$SL_POD_PCT" "$SL_POD_MIB" "$SL_POD_CORES"; do
  kubectl -n default delete pod "$n" --ignore-not-found --force --grace-period=0 >/dev/null 2>&1 || true
done
wait_pool '{.status.acceleratorSliced.remaining}' "${SL_BASE:-}" || true

# The per-accelerator budgets as they stand BEFORE either slice claims. Snapshotted for every
# accelerator rather than for a chosen one, because which accelerator the plugin picks is the
# plugin's decision and this case must not presume it.
CO_LEDGER_PRE=$(kubectl get devices "$NODE" -o json 2>/dev/null | GID="$GROUPID" python3 -c "
import json,os,sys
gid=os.environ['GID']
for g in json.load(sys.stdin).get('status',{}).get('groups',[]):
    if gid and g.get('id')!=gid: continue
    for a in g.get('accelerators',[]):
        print(a.get('id',''), int(a.get('remaining') or 0), a.get('mode'))
")
echo "[case-38] per-accelerator budget before the pair (id / remaining / mode):"
printf '%s\n' "$CO_LEDGER_PRE" | sed 's/^/  /'
CO_A_MEM_MIB=$((CARD_MEM_MIB * CO_A_MEM_PCT / 100))
CO_B_MEM_MIB=$((CARD_MEM_MIB * CO_B_MEM_PCT / 100))
echo "[case-38] slice A ${CO_A_MEM_PCT}% memory (${CO_A_MEM_MIB}MiB) / ${CO_A_CORES_PCT}% compute; slice B ${CO_B_MEM_PCT}% memory (${CO_B_MEM_MIB}MiB) / ${CO_B_CORES_PCT}% compute; the pair claims $((CO_A_MEM_PCT + CO_B_MEM_PCT))% of one accelerator's memory"

# The five properties that only mean something once both slices sit on ONE accelerator. Named once so
# that every path below records the same row set, whichever way the pair turns out.
CO_PROPS=("the two slices share one physical accelerator"
          "both containers report the same physical accelerator"
          "the two compute windows do not overlap"
          "the accelerator is not over-committed"
          "each container's compute window is its own")
# co_skip_props <reason> — record all five as SKIP, never a vacuous PASS.
co_skip_props() {
  local c
  for c in "${CO_PROPS[@]}"; do record SKIP "sliced-co-located: ${c}" "$1"; done
}

CO_A_ID=""; CO_A_START=""; CO_A_LEN=""; CO_A_ALLOC=""; CO_A_BUDGET=""
submit_pod "$SL_CO_A" \
  "{ ${SLICED}: \"1\", ${MEMPCT}: \"${CO_A_MEM_PCT}\", ${CORESPCT}: \"${CO_A_CORES_PCT}\" }"
if wait_pod_running "$SL_CO_A"; then
  record PASS "sliced-co-located/A: claim reaches Running" \
    "${SL_CO_A} Running at ${CO_A_MEM_PCT}% memory / ${CO_A_CORES_PCT}% compute"
  read -r CO_A_ID co_a_mode CO_A_ALLOC CO_A_START CO_A_LEN <<<"$(placement_of "$SL_CO_A" | head -1)"
  CO_A_BUDGET=$(printf '%s\n' "$CO_LEDGER_PRE" | awk -v id="$CO_A_ID" '$1==id{print $2; exit}')
  echo "[case-38] ${SL_CO_A} → ${CO_A_ID} mode ${co_a_mode} run start=${CO_A_START} length=${CO_A_LEN} charged ${CO_A_ALLOC} of ${CO_A_BUDGET:-?}"
else
  record FAIL "sliced-co-located/A: claim reaches Running" \
    "${SL_CO_A} never reached Running at ${CO_A_MEM_PCT}% memory / ${CO_A_CORES_PCT}% compute"
fi

CO_B_ID=""; CO_B_START=""; CO_B_LEN=""; CO_B_ALLOC=""; CO_ROUTE=""
if [ -z "$CO_A_ID" ]; then
  record SKIP "sliced-co-located/B: claim reaches Running" \
    "slice A recorded no placement, so there is no first window to seat a second one beside"
  co_skip_props "slice A recorded no placement, so the pair never formed"
else
  # The sliced pool with A holding its share — the figure to wait for if B has to be resubmitted
  # below, so the second attempt starts from the same ledger the first one saw.
  SL_AFTER_A=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.status.acceleratorSliced.remaining}' 2>/dev/null)

  submit_pod "$SL_CO_B" \
    "{ ${SLICED}: \"1\", ${MEMPCT}: \"${CO_B_MEM_PCT}\", ${CORESPCT}: \"${CO_B_CORES_PCT}\" }"
  if wait_pod_running "$SL_CO_B"; then
    read -r CO_B_ID co_b_mode CO_B_ALLOC CO_B_START CO_B_LEN <<<"$(placement_of "$SL_CO_B" | head -1)"
    echo "[case-38] ${SL_CO_B} → ${CO_B_ID} mode ${co_b_mode} run start=${CO_B_START} length=${CO_B_LEN} charged ${CO_B_ALLOC}"
    CO_ROUTE="direct (the policy joined the accelerator already in use)"
  fi

  # The packing policy may open an idle accelerator instead of joining the one already in use, and on
  # a multi-accelerator host that is its prerogative rather than a defect. It does leave the property
  # under test unexercised though, so take the other accelerator out of the running and claim again.
  # Whether that is even possible is computed from the live ledger, never assumed.
  if [ -n "$CO_B_ID" ] && [ "$CO_B_ID" != "$CO_A_ID" ]; then
    echo "[case-38] the policy opened ${CO_B_ID} rather than joining ${CO_A_ID}; forcing co-location"
    kubectl -n default delete pod "$SL_CO_B" --ignore-not-found --force --grace-period=0 >/dev/null 2>&1 || true
    wait_pool '{.status.acceleratorSliced.remaining}' "${SL_AFTER_A:-}" || true
    CO_B_ID=""
    CO_ROUTE="forced (the policy opened a free accelerator, so it was occupied whole first)"

    submit_pod "$SL_CO_BLOCK" "{ ${BASE}: \"1\" }"
    if ! wait_pod_running "$SL_CO_BLOCK"; then
      CO_ROUTE="unforceable — the blocking whole-accelerator claim never ran"
    else
      co_block_id=$(placement_of "$SL_CO_BLOCK" | awk 'NF{print $1; exit}')
      co_room=$(accel_remaining "$CO_A_ID")
      co_need=$(( ${CO_A_BUDGET:-0} * CO_B_MEM_PCT / 100 ))
      echo "[case-38] ${SL_CO_BLOCK} occupies ${co_block_id} whole; ${CO_A_ID} has ${co_room:-?} of ${CO_A_BUDGET:-?} left, slice B needs ${co_need}"
      if [ "$co_block_id" = "$CO_A_ID" ]; then
        CO_ROUTE="unforceable — the blocking claim took ${CO_A_ID}, the accelerator the pair had to share"
      elif [ "${co_room:-0}" -lt "$co_need" ]; then
        CO_ROUTE="unforceable — ${CO_A_ID} has ${co_room:-0} of ${CO_A_BUDGET:-?} left and slice B needs ${co_need}"
      else
        submit_pod "$SL_CO_B" \
          "{ ${SLICED}: \"1\", ${MEMPCT}: \"${CO_B_MEM_PCT}\", ${CORESPCT}: \"${CO_B_CORES_PCT}\" }"
        if wait_pod_running "$SL_CO_B"; then
          read -r CO_B_ID co_b_mode CO_B_ALLOC CO_B_START CO_B_LEN <<<"$(placement_of "$SL_CO_B" | head -1)"
          echo "[case-38] ${SL_CO_B} → ${CO_B_ID} mode ${co_b_mode} run start=${CO_B_START} length=${CO_B_LEN} charged ${CO_B_ALLOC}"
        else
          CO_ROUTE="unforceable — slice B did not run with only ${CO_A_ID} left to slice"
        fi
      fi
    fi
  fi
  echo "[case-38] co-location route: ${CO_ROUTE:-slice B never placed}"

  if [ -n "$CO_B_ID" ]; then
    record PASS "sliced-co-located/B: claim reaches Running" \
      "${SL_CO_B} Running at ${CO_B_MEM_PCT}% memory / ${CO_B_CORES_PCT}% compute; route: ${CO_ROUTE}"
  else
    record FAIL "sliced-co-located/B: claim reaches Running" \
      "${SL_CO_B} never reached Running at ${CO_B_MEM_PCT}% memory / ${CO_B_CORES_PCT}% compute; route: ${CO_ROUTE:-none}"
  fi

  # Each slice's own two-sided reading, valid whichever accelerator it landed on: the container holds
  # the accelerator the allocation names, and its memory cap is its own share. The cap is worth
  # re-reading here even though three rows above cover it, because this is the only place two caps are
  # in force on one accelerator at once, where a cap leaking between slices reads as the wrong figure
  # rather than as a missing one.
  [ -n "$CO_A_ID" ] && {
    assert_visible "$SL_CO_A" "sliced-co-located/A" "$CO_A_ID" no
    assert_sliced_memory "$SL_CO_A" "sliced-co-located/A" "$CO_A_MEM_MIB"
  }
  [ -n "$CO_B_ID" ] && {
    assert_visible "$SL_CO_B" "sliced-co-located/B" "$CO_B_ID" no
    assert_sliced_memory "$SL_CO_B" "sliced-co-located/B" "$CO_B_MEM_MIB"
  }

  if [ -z "$CO_B_ID" ]; then
    co_skip_props "the pair never formed — ${CO_ROUTE:-slice B never placed}"
  elif [ "$CO_B_ID" != "$CO_A_ID" ]; then
    # Not a failure: the pair is on two accelerators, which is the packing policy's prerogative. Say
    # so, and skip what only means something on one accelerator — two windows starting at the same CU
    # on two different accelerators are not a conflict.
    co_skip_props "the packing policy placed the pair on two accelerators (${CO_A_ID} and ${CO_B_ID}) and co-location could not be forced: ${CO_ROUTE}"
  else
    record PASS "sliced-co-located: the two slices share one physical accelerator" \
      "both allocations name ${CO_A_ID}; route: ${CO_ROUTE}"

    # The container-side half of the co-location: both containers must be shown the SAME physical
    # accelerator, correlated by PCI address and never by the ordinal each one is renumbered to.
    co_bdf=$(bdf_of "$CO_A_ID")
    co_a_bdfs=$(ctr_accelerators "$SL_CO_A" | awk 'NF{print $1}' | sort | tr '\n' ',')
    co_b_bdfs=$(ctr_accelerators "$SL_CO_B" | awk 'NF{print $1}' | sort | tr '\n' ',')
    { [ -n "$co_bdf" ] && [ "$co_a_bdfs" = "${co_bdf}," ] && [ "$co_b_bdfs" = "${co_bdf}," ]; }
    verdict $? "sliced-co-located: both containers report the same physical accelerator" \
      "both containers' AMD tooling reports exactly {${co_bdf}}, the PCI address of ${CO_A_ID}" \
      "slice A reports {${co_a_bdfs%,}}, slice B reports {${co_b_bdfs%,}}, the allocation's accelerator is {${co_bdf}}"

    # THE assertion this phase exists for. Two windows on one accelerator must not share a compute
    # unit. Whether they CAN be disjoint is arithmetic — the accelerator has to hold both lengths —
    # and the placement seats a window beside the occupied ones only while such a seat exists,
    # falling back to the least-overlapping seat on a full accelerator. So the verdict is gated on the
    # DELIVERED lengths fitting, computed from those lengths rather than from the percentages: compute
    # shares are overcommittable here, the pool advertising a cores-percentage capacity well above one
    # accelerator's worth, so the percentages alone cannot say whether a seat existed.
    if [ -z "${CO_A_LEN:-}" ] || [ "${CO_A_LEN}" = "-" ] || \
       [ -z "${CO_B_LEN:-}" ] || [ "${CO_B_LEN}" = "-" ]; then
      record FAIL "sliced-co-located: the two compute windows do not overlap" \
        "one of the two allocations carries no compute run (A='${CO_A_START:-?}+${CO_A_LEN:-?}' B='${CO_B_START:-?}+${CO_B_LEN:-?}'), so the windows cannot be compared"
      record SKIP "sliced-co-located: each container's compute window is its own" \
        "not evaluated — an allocation carries no compute run to compare a mask against"
    else
      co_a_end=$((CO_A_START + CO_A_LEN))
      co_b_end=$((CO_B_START + CO_B_LEN))
      co_sum=$((CO_A_LEN + CO_B_LEN))
      if [ "$co_sum" -gt "$CARD_CU" ]; then
        record SKIP "sliced-co-located: the two compute windows do not overlap" \
          "the delivered windows are ${CO_A_LEN}+${CO_B_LEN}=${co_sum} CUs on a ${CARD_CU}-CU accelerator, so no two disjoint seats exist and an overlap is the documented fallback rather than a defect; lower the shares via E2E_AMD_CO_A_CORES_PCT/E2E_AMD_CO_B_CORES_PCT"
      else
        { [ "$co_a_end" -le "$CO_B_START" ] || [ "$co_b_end" -le "$CO_A_START" ]; }
        verdict $? "sliced-co-located: the two compute windows do not overlap" \
          "[${CO_A_START},${co_a_end}) and [${CO_B_START},${co_b_end}) are disjoint, ${co_sum} of ${CARD_CU} CUs used on ${CO_A_ID}" \
          "[${CO_A_START},${co_a_end}) and [${CO_B_START},${co_b_end}) share compute units on ${CO_A_ID}, though ${co_sum} of ${CARD_CU} CUs left room to seat both apart"
      fi

      # And each container was handed its OWN window: the mask it carries is its recorded run and not
      # its neighbour's, with the bundled reader agreeing the mask conforms on this accelerator.
      co_mask_a=$(ctr_hsa_cu_mask "$SL_CO_A")
      co_mask_b=$(ctr_hsa_cu_mask "$SL_CO_B")
      co_want_a="0:${CO_A_START}-$((co_a_end - 1))"
      co_want_b="0:${CO_B_START}-$((co_b_end - 1))"
      co_fails_a=$(ctr_cumask "$SL_CO_A" | sed -n 's/^FAILS=\([0-9]*\)$/\1/p' | head -1)
      co_fails_b=$(ctr_cumask "$SL_CO_B" | sed -n 's/^FAILS=\([0-9]*\)$/\1/p' | head -1)
      echo "[case-38] in-container masks: A=${co_mask_a:-?} (recorded ${co_want_a}, reader FAILS=${co_fails_a:-?}); B=${co_mask_b:-?} (recorded ${co_want_b}, reader FAILS=${co_fails_b:-?})"
      { [ "$co_mask_a" = "$co_want_a" ] && [ "$co_mask_b" = "$co_want_b" ] && \
        [ "$co_mask_a" != "$co_mask_b" ] && \
        [ "${co_fails_a:-1}" -eq 0 ] && [ "${co_fails_b:-1}" -eq 0 ]; }
      verdict $? "sliced-co-located: each container's compute window is its own" \
        "A holds ${co_mask_a} and B holds ${co_mask_b}, each its own recorded run and neither the other's, reader conformance FAILS=${co_fails_a}/${co_fails_b}" \
        "A holds '${co_mask_a:-?}' (recorded ${co_want_a}), B holds '${co_mask_b:-?}' (recorded ${co_want_b}), reader conformance FAILS=${co_fails_a:-?}/${co_fails_b:-?}"
    fi

    # The ledger's own arithmetic: the accelerator's remaining budget reflects BOTH charges, and the
    # two together never exceed the budget it started with.
    co_rem=$(accel_remaining "$CO_A_ID")
    co_charged=$(( ${CO_A_ALLOC:-0} + ${CO_B_ALLOC:-0} ))
    co_expect=$(( ${CO_A_BUDGET:-0} - co_charged ))
    { [ -n "${CO_A_BUDGET:-}" ] && [ "${CO_A_BUDGET:-0}" -gt 0 ] && \
      [ "$co_charged" -le "${CO_A_BUDGET}" ] && [ "${co_rem:-1}" -ge 0 ] && \
      [ "${co_rem:-x}" = "$co_expect" ]; }
    verdict $? "sliced-co-located: the accelerator is not over-committed" \
      "${CO_A_ID} charged ${CO_A_ALLOC}+${CO_B_ALLOC}=${co_charged} of ${CO_A_BUDGET}, remaining ${co_rem} == ${co_expect}" \
      "${CO_A_ID} charged ${CO_A_ALLOC:-?}+${CO_B_ALLOC:-?}=${co_charged} of ${CO_A_BUDGET:-?}, remaining ${co_rem:-?}, expected ${co_expect}"
  fi
fi

# The invariant the per-card ledger exists to guarantee, across everything still holding capacity.
echo
echo "[case-38] per-accelerator ledger check"
LEDGER=$(kubectl get devices "$NODE" -o json 2>/dev/null | GID="$GROUPID" python3 -c "
import json,os,sys
gid=os.environ.get('GID','')
bad=0
for g in json.load(sys.stdin).get('status',{}).get('groups',[]):
    if gid and g.get('id')!=gid: continue
    for a in g.get('accelerators',[]):
        rem=int(a.get('remaining') or 0)
        note='ok'
        if rem<0: note='NEGATIVE-REMAINING'; bad+=1
        print('  %-22s mode=%s remaining=%-10s %s'%(a.get('id'), a.get('mode'), rem, note))
sys.exit(1 if bad else 0)
")
ledger_rc=$?
echo "$LEDGER"
(exit $ledger_rc)
verdict $? "no accelerator is over-committed" \
  "every accelerator's remaining budget is non-negative" \
  "see the table above — an accelerator's remaining budget went negative"

echo
echo "== CASE 38 — AMD accelerator claims over both carriers: exclusive whole cards, logical slices, and the Instance metrics array =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). The chain behind every row is: the Pod webhook folds the request, Kueue"
  echo "and the per-card feasibility gate admit it, the device plugin picks accelerators and injects the"
  echo "visible-devices variables plus (for a slice) the memory quota and the compute mask, and the"
  echo "container runtime turns the visible-devices variable into device nodes inside the container."
  echo "A row that says the allocation is right while the container sees nothing points at the last of"
  echo "those: compare the value the allocation injected against the forms the node's container runtime"
  echo "accepts. Raise the device-manager's verbosity BEFORE re-running to see the allocate decision"
  echo "(see the shared troubleshooting reference, 'Component log verbosity')."
  echo "Diagnose: kubectl get devices ${NODE} -o yaml;"
  echo "kubectl -n ${NS} logs ds/gpustack-operator-device-manager-${MANUF} --tail=200"
  exit 1
fi
echo "CASE 38 PASS"
