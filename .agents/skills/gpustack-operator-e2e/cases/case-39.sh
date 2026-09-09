#!/usr/bin/env bash
#
# CASE 39 — T-Head PPU: a pinned claim lands exactly where it was told, and a logical slice is capped inside the container   (MUTATING, self-recovering; AUTO-SKIPS without >=2 idle T-Head PPU accelerators)
#
#   case-39.sh <NS>                            # when run on the accelerator node itself
#   PPU_NODE_SSH=<user@host> case-39.sh <NS>   # otherwise
#
# Goal:        ASSERTS two contracts on real T-Head PPU hardware, each proven from BOTH sides —
#              what the operator recorded, and what the container can see:
#                (1) PINNING. A claim carrying the preferred-accelerator-id annotation lands on
#                    EXACTLY the accelerator(s) that annotation named, for one card and for two.
#                    Proven from the allocation the device plugin wrote on the Pod AND from the
#                    accelerator identities ppu-smi reports inside the container. Identity is the
#                    PCI bus address, never the card index: a container is offered its accelerator
#                    renumbered from 0, so an index comparison would pass on the wrong card.
#                (2) LOGICAL-SLICE CAPS. A slice asking for a share of VRAM sees exactly that share
#                    as its total, and a slice asking for a share of compute is capped at exactly
#                    that share — the two dimensions independent, so neither can be reported by
#                    accident from the other.
#
#              Every accelerator this case claims is verified IDLE against the vendor tool
#              immediately before the claim, and verified RELEASED in the ledger afterwards. That
#              is not hygiene, it is the safety property: an accelerator can be busy with work that
#              never went through the device plugin, in which case the operator's ledger calls it
#              free while it is not. The case names every ordinal it touched, and asserts as its
#              last verdict that no claim ever landed on an accelerator that was not idle first.
# Environment: Needs REAL T-Head PPU hardware on ONE node and >=2 accelerators IDLE at the moment
#              each claim is made, judged from the vendor tool in the NODE's host context — only that
#              view shows work that bypassed the device plugin, because the ledger carries no
#              accelerator memory usage and so cannot tell a busy accelerator from a free one.
#              That context is reached directly when this script already runs ON the node (a
#              single-node accelerator lab, the common case), and otherwise over
#              PPU_NODE_SSH=<user@host>, which it will not guess: it EXITS 2 (input required) when it
#              is neither. Pass the address inline at run time and never write it into a file. The
#              address is asked for only AFTER the hardware gate, so a cluster with no T-Head
#              accelerator skips cleanly without being asked for one it has no use for.
#              AUTO-SKIPS (exit 0) when the cluster reports no T-Head accelerator group or fewer than
#              two accelerators are idle, and skips individual claims when too few are idle at that
#              claim's turn. A Devices read that ERRORS fails setup rather than skipping: "no T-Head
#              hardware" must not be indistinguishable from "the query did not answer".
#
#              T-Head is the vendor whose logical slicing needs NO runtimeClass — it has no
#              container-runtime hook at all, so the injected device nodes plus the shim's
#              /etc/ld.so.preload are the whole of the container's access, and a bare image runs a
#              slice without exiting 127. The claim carrier must nonetheless SHIP THE VENDOR TOOL,
#              because every in-container reading here is that tool's output; override with
#              E2E_SLICE_IMAGE=<ref>. Pre-pull it on the node (through the CRI, so the node's
#              registry mirrors apply) if the first claim is slow.
#
#              Presumes the node's kubelet runs topologyManagerPolicy=none, the suite's baseline.
#              The pin is served by the device plugin's preferred-allocation hint, which is advisory
#              by API contract; under a restrictive policy the kubelet picks the NUMA-aligned set
#              before consulting the plugin and the hint never applies.
# Inputs:      All real, nothing mocked. Raw Pods on the T-Head pool's entrance LocalQueue, one at a
#              time, each deleted and its accelerators verified released before the next:
#                a 25%-VRAM slice; a 30%-compute + 10%-VRAM slice; a 1-accelerator exclusive claim;
#                a 2-accelerator exclusive claim. Each carries the preferred-accelerator-id
#                annotation, with ids COPIED from the live Devices object rather than composed — the
#                hint is taken at face value, so an id naming no accelerator is never corrected.
#              The slice claims run FIRST on purpose: a slice shares an accelerator instead of
#              taking it, so it is the cheapest way to learn whether the kubelet honours this
#              plugin family's hint at all on this node, and the exclusive claims are withheld until
#              it has.
#              Additionally an exclusive Instance and a sliced Instance, for the four-view and the
#              metrics subresource — but only on a node where EVERY accelerator is idle, because the
#              Instance API exposes no accelerator pin and its backing Pod carries no annotation to
#              patch one onto, so an Instance claim cannot be steered away from a busy accelerator.
#              Where it cannot run, those verdicts are SKIP with that reason, never a vacuous PASS.
# Expected:    - each claim's recorded allocation names exactly the pinned accelerator identities;
#              - ppu-smi inside each container reports exactly those identities, and nothing else;
#              - the 25%-VRAM slice reports 25% of the accelerator's VRAM as its total;
#              - the compute-capped slice enforces exactly the requested compute share, with its
#                VRAM cap still following its own request;
#              - a sliced request naming no VRAM budget is refused by admission;
#              - each exclusive claim lowers the pool's Accelerator remaining by its size;
#              - every accelerator is back to free in the ledger after its claim is deleted;
#              - no claim ever landed on an accelerator that was not idle beforehand.
# Cleanup:     Trap deletes the test Pods and Instances and waits for the pool's Accelerator and
#              AcceleratorSliced remaining to return to the values captured at setup, so the next
#              case does not start on a pool this one still holds. Idempotent, runs on pass AND
#              fail, safe to re-run.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-39.sh <NS>}"

# The vendor's resource family. Hardcoded rather than derived: the case is gated on the thead
# manufacturer below, and deriving the family here would duplicate a mapping the allocators own.
BASE=alibabacloud.com/ppu
SLICED=${BASE}.sliced
CORESPCT=${BASE}.sliced.cores-percentage
MEMPCT=${BASE}.sliced.memory-percentage
PREFERRED_ID_ANNO=device.gpustack.ai/accelerator.preferred-id
ALLOCATED_ANNO=device.gpustack.ai/accelerator.allocated
PFX=gpustack-e2e-ppu-pin

# The claim carrier. Not a bare base image: every in-container reading below is the vendor tool's
# output, so the image has to ship it. T-Head needs no runtimeClass and no driver-lib mounts beyond
# the device nodes the plugin injects, which is why a devel image alone is enough here.
IMAGE="${E2E_SLICE_IMAGE:-docker.io/gpustack/thead-ppu-devel:2.1.1}"

# An accelerator counts as idle when the vendor tool reports at most this much VRAM in use. Not
# zero: the driver charges a resident accelerator about 1 MiB with nothing running on it.
IDLE_MIB="${PPU_IDLE_MIB:-64}"
# ... and at most this much utilization. A percentage rather than zero, so a sampling blip on an
# otherwise idle accelerator does not disqualify it.
IDLE_UTIL=5

# The slice shares, chosen so no two readings can be confused for one another: the VRAM share of
# the compute-capped slice differs from both its own compute share and from the other slice's VRAM
# share, so a figure copied from the wrong dimension cannot pass.
SLICE_MEM_PCT=25
SLICE_CORES_PCT=30
SLICE_CORES_MEM_PCT=10

PPU_NODE_SSH="${PPU_NODE_SSH:-}"
PPU_NODE_SSH_OPTS="${PPU_NODE_SSH_OPTS:--o BatchMode=yes -o StrictHostKeyChecking=no -o ConnectTimeout=20}"
PPU_SMI="${PPU_SMI:-ppu-smi}"

# --- Skip gate: a node reporting a healthy T-Head accelerator group. ---
if ! DEVICES_JSON=$(kubectl get devices.v1alpha1.worker.gpustack.ai -o json 2>&1); then
  echo "== CASE 39 — FAILED (setup) =="
  echo "Reading the Devices ledger failed, so this case cannot tell 'no T-Head hardware' from 'the"
  echo "query did not answer'. It refuses to report a skip on either:"
  printf '%s\n' "$DEVICES_JSON" | head -5
  exit 1
fi
read -r NODE GROUPID NCARDS <<<"$(printf '%s' "$DEVICES_JSON" | python3 -c "
import json,sys
best=('-','-',0)
for d in json.load(sys.stdin).get('items',[]):
    for g in d.get('spec',{}).get('groups',[]):
        if g.get('manufacturer')!='thead': continue
        n=sum(1 for a in g.get('accelerators',[]) if not a.get('status',{}).get('unhealthy'))
        if n>best[2]: best=(d['metadata']['name'], g.get('id',''), n)
print(best[0], best[1], best[2])
")"
if [ "${NODE:--}" = "-" ] || [ "${NCARDS:-0}" -lt 2 ]; then
  echo "== CASE 39 — SKIPPED =="
  echo "No node reports >=2 healthy T-Head accelerators in one group (best='${NODE:-none}',"
  echo "accelerators=${NCARDS:-0}). This case needs two so that a two-accelerator pin has somewhere"
  echo "to land. Run it on such a node."
  exit 0
fi
echo "[case-39] target: ${NODE}, group '${GROUPID}', ${NCARDS} healthy T-Head accelerator(s)"

# The group's accelerators, by IDENTITY: the PCI bus address is the only key shared by the ledger
# and the vendor tool, so it is what maps one to the other. Emitted as "<index> <busid> <id>" with
# the bus address normalized to its bus:device.function part, lower-case — the ledger writes a
# 4-digit PCI domain and the vendor tool an 8-digit one, and neither is more correct.
ACCEL_TABLE=$(printf '%s' "$DEVICES_JSON" | NODE="$NODE" GID="$GROUPID" python3 -c "
import json,os,sys
node=os.environ['NODE']; gid=os.environ['GID']
for d in json.load(sys.stdin).get('items',[]):
    if d['metadata']['name']!=node: continue
    for g in d.get('spec',{}).get('groups',[]):
        if g.get('id')!=gid: continue
        for a in g.get('accelerators',[]):
            if a.get('status',{}).get('unhealthy'): continue
            bus=(a.get('topology',{}) or {}).get('pciBusId','')
            if ':' in bus: bus=bus.split(':',1)[1].lower()
            print(a.get('index'), bus, a.get('id'))
")
[ -n "$ACCEL_TABLE" ] || { echo "no accelerator carries a PCI bus address, so ledger and vendor tool cannot be correlated"; exit 1; }
CARD_MEM_MIB=$(printf '%s' "$DEVICES_JSON" | NODE="$NODE" GID="$GROUPID" python3 -c "
import json,os,sys
node=os.environ['NODE']; gid=os.environ['GID']
for d in json.load(sys.stdin).get('items',[]):
    if d['metadata']['name']!=node: continue
    for g in d.get('spec',{}).get('groups',[]):
        if g.get('id')==gid: print(int(g.get('memory') or 0)); sys.exit(0)
print(0)
")
[ "${CARD_MEM_MIB:-0}" -gt 0 ] || { echo "the accelerator group reports no memory size, so no VRAM expectation can be computed"; exit 1; }
echo "[case-39] one accelerator = ${CARD_MEM_MIB}MiB VRAM"

# The T-Head pool's InstanceType and its entrance LocalQueue. Matched on the pool's identity tuple, or
# the queue would route the claim to a different pool's nodes. The InstanceType qualifies the group with
# the manufacturer where the per-node group id does not, so the manufacturer is supplied here and the
# test is equality — see the index's note on pool lookup for why containment is not enough.
read -r IT LQ <<<"$(kubectl get instancetypes.v1alpha1.worker.gpustack.ai -o json 2>/dev/null | NODE_GID="$GROUPID" NODE_JSON="$(kubectl get node "$NODE" -o json 2>/dev/null)" python3 -c "
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
    s=it.get('spec',{}); st=it.get('status',{})
    if s.get('acceleratable') and in_group(s) and backs(it) and st.get('entrance'):
        print(it['metadata']['name'], st['entrance']); break
")"
[ -n "${IT:-}" ] && [ -n "${LQ:-}" ] || { echo "no T-Head InstanceType with an entrance LocalQueue backs ${NODE} in group '${GROUPID}'"; exit 1; }
echo "[case-39] pool ${IT} via LocalQueue ${LQ}; slice pods carry no runtimeClass (this vendor needs none)"

# --- How the node's host context is reached: directly when this script is already running ON the
#     target node, otherwise over the SSH address it will not guess. Resolved only now, after the
#     hardware gate, so a cluster with no T-Head accelerator skips cleanly without being asked for
#     an address it has no use for. ---
ON_NODE=0
if [ "$(hostname 2>/dev/null)" = "$NODE" ] && command -v "$PPU_SMI" >/dev/null 2>&1; then
  ON_NODE=1
  echo "[case-39] reading the vendor tool directly: this script is running on ${NODE}"
elif [ -z "$PPU_NODE_SSH" ]; then
  echo "== CASE 39 — INPUT REQUIRED =="
  echo "This case reads the vendor tool on ${NODE} to learn which accelerators are genuinely idle,"
  echo "because the operator's ledger carries no accelerator memory usage and so cannot tell an"
  echo "accelerator busy with work that bypassed the device plugin from a free one. Claiming without"
  echo "that reading could take an accelerator somebody else is using. This script is not running on"
  echo "${NODE}, so it needs the node's address, supplied inline and never written into a file:"
  echo "  PPU_NODE_SSH=<user@host> bash \$0 ${NS}"
  echo "  PPU_NODE_SSH_OPTS='-i ...' # extra ssh options (identity file, port, jump host, …)"
  echo "  PPU_SMI=<path>             # when the vendor tool is not on the node's PATH"
  exit 2
fi
# node_exec <cmd…> — run a command in the node's host context.
node_exec() {
  if [ "$ON_NODE" -eq 1 ]; then
    "$@" 2>/dev/null
  else
    # shellcheck disable=SC2086  # the options are a word list on purpose
    ssh $PPU_NODE_SSH_OPTS "$PPU_NODE_SSH" "$@" 2>/dev/null
  fi
}
if [ "$ON_NODE" -eq 0 ] && ! node_exec true >/dev/null 2>&1; then
  echo "== CASE 39 — FAILED (setup) =="
  echo "Cannot reach the node's host context over the supplied address, so no accelerator can be"
  echo "confirmed idle and nothing may be claimed. Check the address, the key, and PPU_NODE_SSH_OPTS."
  exit 1
fi

# --- The host view: which accelerators are genuinely idle, right now. ---
#
# "<ordinal> <busid> <totalMiB> <usedMiB> <utilPct> <processes>" per accelerator, parsed from the
# vendor tool's table on the node. An accelerator occupies two table rows — identity on the first,
# usage on the second — and the process table at the bottom attributes running work by ordinal.
host_cards() {
  node_exec "$PPU_SMI" | python3 -c "
import re,sys
txt=sys.stdin.read()
cards={}; cur=None
for line in txt.splitlines():
    m=re.match(r'^\|\s+(\d+)\s+\S+.*\|\s*([0-9A-Fa-f]{4,8}:[0-9A-Fa-f]{2}:[0-9A-Fa-f]{2}\.[0-9A-Fa-f])\s*\|', line)
    if m:
        cur=int(m.group(1))
        bus=m.group(2).lower()
        cards[cur]={'bus':bus.split(':',1)[1], 'total':0, 'used':-1, 'util':-1, 'procs':0}
        continue
    if cur is not None:
        mm=re.search(r'(\d+)MiB\s*/\s*(\d+)MiB', line)
        if mm:
            cards[cur]['used']=int(mm.group(1)); cards[cur]['total']=int(mm.group(2))
            mu=re.search(r'\|\s*(\d+)%', line)
            cards[cur]['util']=int(mu.group(1)) if mu else -1
            cur=None
inproc=False
for line in txt.splitlines():
    if 'Processes:' in line: inproc=True; continue
    if not inproc: continue
    mp=re.match(r'^\|\s*(\d+)\s+\S+\s+\S+\s+(\d+)\s', line)
    if mp:
        i=int(mp.group(1))
        if i in cards: cards[i]['procs']+=1
for i in sorted(cards):
    c=cards[i]
    print(i, c['bus'], c['total'], c['used'], c['util'], c['procs'])
"
}

# idle_now — the ordinals the host view currently reports idle, space separated, in ordinal order.
idle_now() {
  host_cards | IDLE_MIB="$IDLE_MIB" IDLE_UTIL="$IDLE_UTIL" python3 -c "
import os,sys
lim=int(os.environ['IDLE_MIB']); util=int(os.environ['IDLE_UTIL'])
out=[]
for line in sys.stdin:
    f=line.split()
    if len(f)<6: continue
    i,used,u,procs=f[0],int(f[3]),int(f[4]),int(f[5])
    if 0<=used<=lim and 0<=u<=util and procs==0: out.append(i)
print(' '.join(out))
"
}

HOST_VIEW=$(host_cards)
if [ -z "$HOST_VIEW" ]; then
  echo "== CASE 39 — FAILED (setup) =="
  echo "The vendor tool on ${NODE} produced no readable accelerator table over the supplied SSH"
  echo "address, so no accelerator can be confirmed idle and nothing may be claimed. Check that the"
  echo "address reaches the node and that '${PPU_SMI}' is on its PATH (override with PPU_SMI=<path>)."
  exit 1
fi
echo "[case-39] host view (ordinal bus total used util procs):"
printf '%s\n' "$HOST_VIEW" | sed 's/^/    /'
IDLE_AT_START=$(idle_now)
IDLE_COUNT=$(printf '%s\n' "$IDLE_AT_START" | wc -w | tr -d ' ')
HOST_COUNT=$(printf '%s\n' "$HOST_VIEW" | wc -l | tr -d ' ')
echo "[case-39] idle ordinals: {${IDLE_AT_START// /,}} — ${IDLE_COUNT} of ${HOST_COUNT} on the host"
if [ "${IDLE_COUNT:-0}" -lt 2 ]; then
  echo "== CASE 39 — SKIPPED =="
  echo "Fewer than two accelerators on ${NODE} are idle (idle={${IDLE_AT_START// /,}}). Every claim"
  echo "here takes a whole accelerator or a share of one, and the two-accelerator pin needs two, so"
  echo "there is nothing this case may safely take. Re-run when the node is quieter."
  exit 0
fi

# ledger_json — the target group's per-accelerator runtime ledger, keyed by accelerator id. A
# missing `remaining` means ZERO, not a whole accelerator: the field is omitempty, so a fully
# allocated accelerator omits it and defaulting the other way reads a full accelerator as untouched.
ledger_json() {
  kubectl get devices.v1alpha1.worker.gpustack.ai "$NODE" -o json 2>/dev/null \
    | GID="$GROUPID" python3 -c "
import json,os,sys
gid=os.environ.get('GID','')
out={}
for g in json.load(sys.stdin).get('status',{}).get('groups',[]):
    if gid and g.get('id')!=gid: continue
    for a in g.get('accelerators',[]):
        out[a.get('id')]={'index':a.get('index'),'mode':a.get('mode',0),
                          'remaining':int(a.get('remaining') or 0)}
print(json.dumps(out))
"
}
# One whole accelerator's unit budget, read back from a free accelerator rather than assumed.
MAX_UNITS=$(ledger_json | python3 -c "
import json,sys
free=[v['remaining'] for v in json.load(sys.stdin).values() if v['mode']==0]
print(max(free) if free else 0)
")
if [ "${MAX_UNITS:-0}" -le 0 ]; then
  echo "== CASE 39 — FAILED (setup) =="
  echo "No accelerator in the ledger is free, so one accelerator's unit budget cannot be read back"
  echo "and the release assertions below would compare against a zero. Let the pool drain and re-run."
  ledger_json
  exit 1
fi

# pool <view> — the pool's remaining capacity in one of the four views.
pool() { kubectl get instancetypes.v1alpha1.worker.gpustack.ai "$IT" -o jsonpath="{.status.$1.remaining}" 2>/dev/null; }
EX_BASE=$(pool accelerator)
SL_BASE=$(pool acceleratorSliced)
echo "[case-39] one accelerator = ${MAX_UNITS} units; pool remaining at start: EX=${EX_BASE:-?} SL=${SL_BASE:-?}"

PODS=("${PFX}-slice-mem" "${PFX}-slice-cores" "${PFX}-excl-1" "${PFX}-excl-2")
INSTANCES=("${PFX}-inst-excl" "${PFX}-inst-slice")

restore() {
  echo
  echo "[case-39] cleanup: deleting test Pods and Instances"
  local p
  for p in "${PODS[@]}"; do
    kubectl -n default delete pod "$p" --ignore-not-found --force --grace-period=0 >/dev/null 2>&1 || true
  done
  for p in "${INSTANCES[@]}"; do
    kubectl -n default delete instance "$p" --ignore-not-found >/dev/null 2>&1 || true
  done
  # Wait for the pool to give back what this case took, so the next case does not misread the pool.
  local ex sl
  for _ in $(seq 1 25); do
    ex=$(pool accelerator); sl=$(pool acceleratorSliced)
    if [ -n "$ex" ] && [ "$ex" = "${EX_BASE:-}" ] && [ -n "$sl" ] && [ "$sl" = "${SL_BASE:-}" ]; then
      echo "[case-39] pool remaining back to EX=${ex} SL=${sl}"
      return 0
    fi
    sleep 3
  done
  echo "[case-39] WARNING: pool remaining is EX='${ex:-?}' SL='${sl:-?}', expected EX='${EX_BASE:-?}' SL='${SL_BASE:-?}' — the ledger has not settled back"
}
trap restore EXIT

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
# verdict <cond-exit-code> <check> <object-on-pass> <object-on-fail>
verdict() { [ "$1" -eq 0 ] && record PASS "$2" "$3" || record FAIL "$2" "$4"; }

# The ordinals this run touched, and whether any claim ever landed outside the idle set. The second
# is the safety property the whole case is built around, so it is asserted last and explicitly.
TOUCHED=""
UNSAFE=0
# Whether the kubelet was observed honouring this plugin family's pin. The exclusive claims are
# withheld until a slice — which shares an accelerator rather than taking it — has shown that it does.
PIN_HONOURED=0

# ids_for <ordinals…> — the accelerator identities of those ordinals, comma separated, taken from
# the live ledger. Never composed: the pin is honoured at face value, so an id naming no accelerator
# on this node is never corrected and would silently degrade the placement to arbitrary.
ids_for() {
  local want=" $* "
  printf '%s\n' "$ACCEL_TABLE" | while read -r idx _bus id; do
    case "$want" in *" ${idx} "*) echo "$id" ;; esac
  done | paste -sd, -
}
# buses_for <ordinals…> — the same accelerators' normalized PCI bus addresses, sorted, comma separated.
buses_for() {
  local want=" $* "
  printf '%s\n' "$ACCEL_TABLE" | while read -r idx bus _id; do
    case "$want" in *" ${idx} "*) echo "$bus" ;; esac
  done | sort | paste -sd, -
}
# ordinals_for <ids-csv> — back the other way, for reporting which ordinals a claim reached.
ordinals_for() {
  local want=",$1,"
  printf '%s\n' "$ACCEL_TABLE" | while read -r idx _bus id; do
    case "$want" in *",${id},"*) echo "$idx" ;; esac
  done | sort -n | paste -sd, -
}
# csv_sorted <csv> — the same members in a canonical order, so two identity SETS can be compared as
# strings. Necessary because the pin is written in ordinal order for readability while the plugin
# records its allocation sorted by identity, and comparing those two verbatim fails on a correct
# multi-accelerator placement.
csv_sorted() { printf '%s\n' "$1" | tr ',' '\n' | grep -v '^$' | sort | paste -sd, -; }

# pick_idle <n> — n ordinals confirmed idle by the host view RIGHT NOW, or empty when too few are.
# Re-read per claim rather than once at setup: an accelerator can pick up foreign work at any
# moment, and a stale idle set is exactly how a case takes an accelerator somebody else is using.
pick_idle() {
  local n="$1" now
  now=$(idle_now)
  [ "$(printf '%s\n' "$now" | wc -w | tr -d ' ')" -ge "$n" ] || return 1
  echo "$now" | tr ' ' '\n' | head -"$n" | paste -sd' ' -
}

# wait_running <pod> — poll until Running, with diagnostics when it never gets there. The gate
# matters beyond liveness: the plugin writes the allocation annotation during Allocate, BEFORE the
# container runs, so a container that died still advertises a placement, and reading it back without
# this gate would report a confident accelerator for a claim that never worked.
wait_running() {
  local pod="$1"
  for _ in $(seq 1 40); do
    [ "$(kubectl -n default get pod "$pod" -o jsonpath='{.status.phase}' 2>/dev/null)" = Running ] && return 0
    sleep 3
  done
  echo "[case-39] ${pod} never reached Running"
  kubectl -n default get pod "$pod" \
    -o jsonpath='  exit={.status.containerStatuses[0].state.terminated.exitCode} reason={.status.containerStatuses[0].state.terminated.reason}{"\n"}' 2>/dev/null
  kubectl -n default logs "$pod" --tail=5 2>&1 | sed 's/^/  /' | head -6
  echo "  a missing image is the usual cause on a node whose registry mirrors the CLI in use does not"
  echo "  read — pre-pull ${IMAGE} through the CRI and re-run."
  kubectl -n default describe pod "$pod" 2>/dev/null | tail -12
  return 1
}

# pod_yaml <name> <pinned-ids-csv-or-empty> <resources-inline> — the claim carrier: on the pool's
# entrance LocalQueue, pinned to the target node, and carrying the preferred-accelerator-id
# annotation when one is given. Deliberately NO runtimeClassName: this vendor has no
# container-runtime hook, so the device nodes the plugin injects plus the shim's /etc/ld.so.preload
# are the whole of the container's access, and adding one would only fail on a node without it.
pod_yaml() {
  local anno=""
  [ -n "$2" ] && anno="  annotations: { ${PREFERRED_ID_ANNO}: \"$2\" }"
  cat <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $1
  namespace: default
  labels: { kueue.x-k8s.io/queue-name: ${LQ} }
${anno}
spec:
  schedulerName: default-scheduler
  restartPolicy: Never
  nodeSelector: { kubernetes.io/hostname: ${NODE} }
  containers:
    - name: main
      image: ${IMAGE}
      command: ["sleep", "86400"]
      resources:
        limits:   $3
        requests: $3
EOF
}
# submit_pod <name> <pinned-ids-csv> <resources-inline> — apply the claim and wait for it to run.
submit_pod() {
  pod_yaml "$1" "$2" "$3" | kubectl apply -f - >/dev/null
  wait_running "$1"
}
# record_all <STATUS> <object> <check>… — one verdict for every check of a block that could not run,
# so a skipped or failed block still accounts for each contract it was meant to cover.
record_all() {
  local status="$1" object="$2" c
  shift 2
  for c in "$@"; do record "$status" "$c" "$object"; done
}

# allocated_of <pod> — "<ids-csv> <modes-csv>" from the allocation the plugin recorded, identities
# sorted. The device id the kubelet was handed is "<group>:<accelerator-id>:<token>", so the
# identity is read out of it directly and cross-checked against the per-accelerator record.
allocated_of() {
  kubectl -n default get pod "$1" -o json 2>/dev/null | ANNO="$ALLOCATED_ANNO" python3 -c "
import json,os,sys
ann=(json.load(sys.stdin)['metadata'].get('annotations') or {}).get(os.environ['ANNO'])
if not ann: sys.exit(1)
modes={0:'free',1:'exclusive',2:'shared',3:'sliced',4:'partitioned'}
ids=set(); md=[]
for cval in json.loads(ann).values():
    for d in cval.get('deviceIDs') or []:
        p=d.split(':')
        if len(p)==3: ids.add(p[1])
    for g in cval.get('devices',{}).get('groups',[]):
        for a in g.get('accelerators',[]):
            ids.add(a.get('id')); md.append(modes.get(a.get('mode'),'?'))
ids.discard(None)
if not ids: sys.exit(1)
print(','.join(sorted(ids)), ','.join(sorted(set(md))) or '-')
"
}

# smi_buses <pod> — the accelerator identities ppu-smi reports INSIDE the container, normalized and
# sorted. Retried: a Pod reports Running slightly before its container is attachable, and every
# reading here feeds a verdict, so without the retry a timing artifact is recorded as a defect.
smi_buses() {
  local out
  for _ in $(seq 1 10); do
    out=$(kubectl -n default exec "$1" -c main -- "$PPU_SMI" 2>/dev/null \
      | grep -oE '[0-9A-Fa-f]{4,8}:[0-9A-Fa-f]{2}:[0-9A-Fa-f]{2}\.[0-9A-Fa-f]' \
      | tr 'A-F' 'a-f' | cut -d: -f2- | sort -u | paste -sd, -)
    [ -n "$out" ] && { echo "$out"; return 0; }
    sleep 3
  done
  return 1
}
# smi_total_mib <pod> — the VRAM total ppu-smi reports inside the container. Under a slice the shim
# answers this getter with the quota, so it is the slice's own ceiling rather than the accelerator's.
smi_total_mib() {
  local out
  for _ in $(seq 1 10); do
    out=$(kubectl -n default exec "$1" -c main -- "$PPU_SMI" 2>/dev/null \
      | awk 'match($0,/[0-9]+MiB \/ [0-9]+MiB/){m=substr($0,RSTART,RLENGTH);split(m,a,/MiB \/ /);sub(/MiB/,"",a[2]);print a[2]+0;exit}')
    [ -n "$out" ] && [ "$out" != 0 ] && { echo "$out"; return 0; }
    sleep 3
  done
  return 1
}
# env_of <pod> <var> — one injected environment variable, retried for the same reason.
env_of() {
  local out
  for _ in $(seq 1 8); do
    out=$(kubectl -n default exec "$1" -c main -- printenv "$2" 2>/dev/null | tr -d '\r' | awk 'NF{last=$0} END{print last}')
    [ -n "$out" ] && { echo "$out"; return 0; }
    sleep 3
  done
  return 1
}

# check_placement <pod> <label> <pinned-ordinals> — the two halves of the pinning contract, plus the
# safety check that the claim stayed inside the idle set. Returns non-zero when the claim landed
# somewhere it should not have, so the caller can stop rather than pile more claims on top.
check_placement() {
  local pod="$1" label="$2" ords="$3"
  local want_ids want_buses got_ids got_modes got_buses got_ords rc=0
  want_ids=$(ids_for $ords)
  want_buses=$(buses_for $ords)
  read -r got_ids got_modes <<<"$(allocated_of "$pod")"
  if [ -z "${got_ids:-}" ]; then
    record FAIL "${label}: lands on exactly the pinned accelerator(s) — recorded allocation" \
      "no allocation recorded on the Pod, so the plugin never answered for this claim"
    record FAIL "${label}: lands on exactly the pinned accelerator(s) — ppu-smi in the container" \
      "not evaluated — no allocation recorded"
    return 1
  fi
  got_ords=$(ordinals_for "$got_ids")
  echo "[case-39] ${label} → ordinals {${got_ords}} mode=${got_modes}"
  TOUCHED="${TOUCHED} ${got_ords}"

  # Safety first: an accelerator outside the idle set was busy with work the ledger does not know
  # about, so landing there is the one outcome this case must never quietly continue past.
  local idle_ids o
  idle_ids=$(ids_for $(idle_now) $ords)
  for o in $(echo "$got_ids" | tr ',' ' '); do
    case ",${idle_ids}," in *",${o},"*) ;; *) UNSAFE=$((UNSAFE + 1)); rc=1 ;; esac
  done

  [ "$got_ids" = "$(csv_sorted "$want_ids")" ]
  local placed=$?
  verdict $placed "${label}: lands on exactly the pinned accelerator(s) — recorded allocation" \
    "ordinals {${got_ords}} == pinned {${ords// /,}} (mode=${got_modes})" \
    "ordinals {${got_ords}}, but the pin named {${ords// /,}} — the preferred-accelerator hint was not honoured"
  [ $placed -eq 0 ] && PIN_HONOURED=1

  got_buses=$(smi_buses "$pod")
  [ -n "$got_buses" ] && [ "$got_buses" = "$want_buses" ]
  verdict $? "${label}: lands on exactly the pinned accelerator(s) — ppu-smi in the container" \
    "ppu-smi sees {${got_buses}} == the pinned accelerators' bus addresses, and nothing else" \
    "ppu-smi sees '{${got_buses:-<unreadable>}}' vs the pinned {${want_buses}}"
  return $rc
}

# release_and_check <pod> <label> <ids-csv> — delete the claim and require BOTH ledgers to hand every
# accelerator back: the per-accelerator one (mode free again, full unit budget) and the pool's four
# views (remaining back to what setup captured). Deleting is part of the assertion, not cleanup: a
# claim that never releases strands the accelerator for everything that follows.
#
# The pool's views are asserted here rather than left to the trap because they lag the
# per-accelerator ledger by a reconcile, and the next claim measures its own effect as a DELTA on
# them — reading a baseline that has not settled would score a correct claim as having consumed
# nothing. So this is where the case waits, once, for the whole chain to come back to rest.
release_and_check() {
  local pod="$1" label="$2" ids="$3"
  kubectl -n default delete pod "$pod" --ignore-not-found --force --grace-period=0 >/dev/null 2>&1
  local left="" ex="" sl=""
  for _ in $(seq 1 25); do
    left=$(ledger_json | IDS="$ids" MX="$MAX_UNITS" python3 -c "
import json,os,sys
ids=[x for x in os.environ['IDS'].split(',') if x]
mx=int(os.environ['MX']); led=json.load(sys.stdin); bad=[]
for i in ids:
    v=led.get(i)
    if v is None: continue
    if v['mode']!=0 or v['remaining']!=mx: bad.append(str(v['index']))
print(','.join(bad))
")
    ex=$(pool accelerator); sl=$(pool acceleratorSliced)
    [ -z "$left" ] && [ "$ex" = "${EX_BASE:-}" ] && [ "$sl" = "${SL_BASE:-}" ] && break
    sleep 3
  done
  { [ -z "$left" ] && [ "$ex" = "${EX_BASE:-}" ] && [ "$sl" = "${SL_BASE:-}" ]; }
  verdict $? "${label}: the accelerator(s) are released once the claim is deleted" \
    "every accelerator back to free with its full budget of ${MAX_UNITS} units, and the pool back to EX=${ex} SL=${sl}" \
    "ordinals {${left:-none}} still held, and the pool reads EX=${ex:-?} SL=${sl:-?} against EX=${EX_BASE:-?} SL=${SL_BASE:-?}"
}

# --- Claim 1: a VRAM-share slice. Run first: a slice shares an accelerator rather than taking it,
#     so it is the least invasive way to learn whether the kubelet honours the pin on this node. ---
MEM_LABEL="slice ${SLICE_MEM_PCT}% VRAM"
MEM_CHECKS=("${MEM_LABEL}: admitted and running"
  "${MEM_LABEL}: lands on exactly the pinned accelerator(s) — recorded allocation"
  "${MEM_LABEL}: lands on exactly the pinned accelerator(s) — ppu-smi in the container"
  "${MEM_LABEL}: the container's VRAM total equals the requested share"
  "${MEM_LABEL}: the slicing shim is loaded into the container"
  "${MEM_LABEL}: the accelerator(s) are released once the claim is deleted")
ords=$(pick_idle 1)
if [ -z "${ords:-}" ]; then
  record_all SKIP "no accelerator was idle at claim time" "${MEM_CHECKS[@]}"
else
  ids=$(ids_for $ords)
  echo "[case-39] claim 1: ${MEM_LABEL} pinned to ordinal {${ords}} (${ids})"
  if submit_pod "${PODS[0]}" "$ids" "{ ${SLICED}: \"1\", ${MEMPCT}: \"${SLICE_MEM_PCT}\" }"; then
    record PASS "${MEM_LABEL}: admitted and running" \
      "${PODS[0]} Running (webhook fold → Kueue → AdmissionCheck → schedule → device plugin)"
    check_placement "${PODS[0]}" "$MEM_LABEL" "$ords"

    want_mib=$((CARD_MEM_MIB * SLICE_MEM_PCT / 100))
    got_mib=$(smi_total_mib "${PODS[0]}")
    cap=$(env_of "${PODS[0]}" HGGC_DEVICE_MEMORY_LIMIT_0)
    { [ "${got_mib:-0}" = "$want_mib" ] && [ "${cap:-0}" = "$want_mib" ]; }
    verdict $? "${MEM_LABEL}: the container's VRAM total equals the requested share" \
      "ppu-smi total=${got_mib}MiB and the injected cap=${cap}MiB, both ${SLICE_MEM_PCT}% of ${CARD_MEM_MIB}MiB" \
      "ppu-smi total='${got_mib:-?}'MiB, injected cap='${cap:-<unset>}', want ${want_mib}MiB (${SLICE_MEM_PCT}% of ${CARD_MEM_MIB}MiB)"

    # The cap is only real if the shim enforcing it is actually loaded — an environment variable
    # nothing reads would leave ppu-smi reporting the accelerator's own figure instead.
    preload=$(kubectl -n default exec "${PODS[0]}" -c main -- cat /etc/ld.so.preload 2>/dev/null | tr '\n' ' ')
    case "$preload" in *hgml_dlsym_hook.so*hggc_quota.so*) rc=0 ;; *) rc=1 ;; esac
    verdict "$rc" "${MEM_LABEL}: the slicing shim is loaded into the container" \
      "/etc/ld.so.preload names both shim objects: ${preload}" \
      "/etc/ld.so.preload reads '${preload:-<absent>}' — the reported cap is not being enforced by the shim"

    release_and_check "${PODS[0]}" "$MEM_LABEL" "$ids"
  else
    record_all FAIL "the claim never reached Running (reason logged above)" "${MEM_CHECKS[@]}"
  fi
fi

# --- Claim 2: a compute-share slice. It carries a VRAM budget as well, and deliberately a different
#     one from claim 1's, because the two dimensions are independent and admission REQUIRES the VRAM
#     side: without it a slice would be indistinguishable from one entitled to the whole
#     accelerator. That refusal is asserted first, so the shape below is explained by a verdict
#     rather than only by a comment. ---
res_bad="{ ${SLICED}: \"1\", ${CORESPCT}: \"${SLICE_CORES_PCT}\" }"
bad_out=$(pod_yaml "${PFX}-invalid" "" "$res_bad" | kubectl apply --dry-run=server -f - 2>&1)
bad_rc=$?
# A non-zero status alone proves nothing: a connectivity, authorization or discovery failure exits
# non-zero too, and would pass this check while the webhook was never consulted. So the output has to
# name an admission decision as well.
printf '%s' "$bad_out" | grep -qiE 'admission webhook|denied the request|is invalid|forbidden: .*(vram|memory|budget)'
bad_denied=$?
[ "$bad_rc" -ne 0 ] && [ "$bad_denied" -eq 0 ]
verdict $? "a sliced request naming no VRAM budget is refused" \
  "admission rejected it: $(printf '%s' "$bad_out" | tr '\n' ' ' | cut -c1-160)" \
  "$([ "$bad_rc" -eq 0 ] && echo "admission accepted it" || echo "the request failed without an admission decision (rc=${bad_rc}), so the webhook was never shown to have refused it"): $(printf '%s' "$bad_out" | tr '\n' ' ' | cut -c1-160)"

CORES_LABEL="slice ${SLICE_CORES_PCT}% compute"
CORES_CHECKS=("${CORES_LABEL}: admitted and running"
  "${CORES_LABEL}: lands on exactly the pinned accelerator(s) — recorded allocation"
  "${CORES_LABEL}: lands on exactly the pinned accelerator(s) — ppu-smi in the container"
  "${CORES_LABEL}: the enforced compute share equals the request"
  "${CORES_LABEL}: the VRAM cap follows its own request, not the compute one"
  "${CORES_LABEL}: the accelerator(s) are released once the claim is deleted")
ords=$(pick_idle 1)
if [ -z "${ords:-}" ]; then
  record_all SKIP "no accelerator was idle at claim time" "${CORES_CHECKS[@]}"
else
  ids=$(ids_for $ords)
  echo "[case-39] claim 2: ${CORES_LABEL} + ${SLICE_CORES_MEM_PCT}% VRAM pinned to ordinal {${ords}} (${ids})"
  if submit_pod "${PODS[1]}" "$ids" \
      "{ ${SLICED}: \"1\", ${CORESPCT}: \"${SLICE_CORES_PCT}\", ${MEMPCT}: \"${SLICE_CORES_MEM_PCT}\" }"; then
    record PASS "${CORES_LABEL}: admitted and running" "${PODS[1]} Running"
    check_placement "${PODS[1]}" "$CORES_LABEL" "$ords"

    sm=$(env_of "${PODS[1]}" HGGC_DEVICE_SM_LIMIT)
    [ "${sm:-}" = "$SLICE_CORES_PCT" ]
    verdict $? "${CORES_LABEL}: the enforced compute share equals the request" \
      "the shim's compute cap reads ${sm}, the requested ${SLICE_CORES_PCT}% (how hard it throttles is the slicing-shim suite's subject, not this one's)" \
      "the shim's compute cap reads '${sm:-<unset>}', want ${SLICE_CORES_PCT}"

    want_mib=$((CARD_MEM_MIB * SLICE_CORES_MEM_PCT / 100))
    got_mib=$(smi_total_mib "${PODS[1]}")
    cap=$(env_of "${PODS[1]}" HGGC_DEVICE_MEMORY_LIMIT_0)
    { [ "${got_mib:-0}" = "$want_mib" ] && [ "${cap:-0}" = "$want_mib" ]; }
    verdict $? "${CORES_LABEL}: the VRAM cap follows its own request, not the compute one" \
      "ppu-smi total=${got_mib}MiB and the injected cap=${cap}MiB, both ${SLICE_CORES_MEM_PCT}% of ${CARD_MEM_MIB}MiB while compute asked for ${SLICE_CORES_PCT}%" \
      "ppu-smi total='${got_mib:-?}'MiB, injected cap='${cap:-<unset>}', want ${want_mib}MiB (${SLICE_CORES_MEM_PCT}% of ${CARD_MEM_MIB}MiB)"

    release_and_check "${PODS[1]}" "$CORES_LABEL" "$ids"
  else
    record_all FAIL "the claim never reached Running (reason logged above)" "${CORES_CHECKS[@]}"
  fi
fi

# --- Claims 3 and 4: exclusive whole-accelerator claims, one accelerator then two. Withheld unless
#     a slice already showed the kubelet honouring the pin: an exclusive claim that ignores the pin
#     takes a whole accelerator of somebody else's choosing, and on a node carrying work the ledger
#     cannot see that is not a risk worth taking to learn what the slice already answered. ---
exclusive_claim() {
  local n="$2" pod="${PODS[$1]}" label="exclusive ${2}-accelerator claim"
  local checks=("${label}: admitted and running"
    "${label}: lands on exactly the pinned accelerator(s) — recorded allocation"
    "${label}: lands on exactly the pinned accelerator(s) — ppu-smi in the container"
    "${label}: the pool's Accelerator remaining falls by ${n}"
    "${label}: the accelerator(s) are released once the claim is deleted")
  if [ "$PIN_HONOURED" -ne 1 ]; then
    record_all SKIP "the pin was not observed honoured on a shared claim, so a whole-accelerator claim could take an accelerator of the kubelet's choosing — withheld deliberately" \
      "${checks[@]}"
    return 0
  fi
  if [ "$UNSAFE" -ne 0 ]; then
    record_all SKIP "an earlier claim landed outside the idle set, so no further accelerator is claimed" "${checks[@]}"
    return 0
  fi
  local ords ids before after
  ords=$(pick_idle "$n")
  if [ -z "${ords:-}" ]; then
    record_all SKIP "fewer than ${n} accelerator(s) were idle at claim time" "${checks[@]}"
    return 0
  fi
  ids=$(ids_for $ords)
  before=$(pool accelerator)
  echo "[case-39] claim: ${label} pinned to ordinals {${ords// /,}} (${ids})"
  if ! submit_pod "$pod" "$ids" "{ ${BASE}: \"${n}\" }"; then
    record_all FAIL "the claim never reached Running (reason logged above)" "${checks[@]}"
    return 0
  fi
  record PASS "${label}: admitted and running" "${pod} Running"
  check_placement "$pod" "$label" "$ords"

  # The pool's exclusive view must lose exactly this claim's accelerators. NOT the node's <base>
  # allocatable: that counts ADVERTISED devices and the kubelet accounts an allocated one as in-use,
  # so it does not move here.
  after=""
  for _ in $(seq 1 20); do
    after=$(pool accelerator)
    [ -n "$after" ] && [ -n "$before" ] && [ "$after" -eq $((before - n)) ] && break
    sleep 3
  done
  { [ -n "$after" ] && [ -n "$before" ] && [ "$after" -eq $((before - n)) ]; }
  verdict $? "${label}: the pool's Accelerator remaining falls by ${n}" \
    "accelerator.remaining ${before} -> ${after}" \
    "accelerator.remaining '${before:-?}' -> '${after:-?}', expected $((${before:-0} - n))"

  release_and_check "$pod" "$label" "$ids"
}
exclusive_claim 2 1
exclusive_claim 3 2

# --- The Instance carrier and the metrics subresource.
#
#     An Instance is the operator's own claim carrier, and the metrics subresource is the only place
#     an allocated accelerator's utilization is served. But the Instance API carries no
#     accelerator-pin input, and its backing Pod is rendered without annotations, so an Instance
#     claim cannot be aimed: it goes wherever the plugin's own placement sends it. That is fine on a
#     node whose accelerators are all genuinely idle, and not fine on one carrying work the ledger
#     does not know about — there the claim could take that accelerator. So the branch is gated on
#     the host view rather than on a flag, and records SKIP with that reason when it cannot run. ---
INST_CHECKS=("exclusive Instance: reaches Ready"
  "exclusive Instance: ppu-smi in the container agrees with the recorded allocation"
  "Instance metrics: accelerators present, one entry per allocated accelerator"
  "Instance metrics: every entry's id matches the allocation and the container's view"
  "Instance metrics: memoryTotalMiB equals the accelerator's physical VRAM"
  "Instance metrics: utilization percentages within [0,100]")
IDLE_NOW_COUNT=$(printf '%s\n' "$(idle_now)" | wc -w | tr -d ' ')
if [ "${IDLE_NOW_COUNT:-0}" -lt "${HOST_COUNT:-0}" ] || [ "$UNSAFE" -ne 0 ]; then
  record_all SKIP "${IDLE_NOW_COUNT} of ${HOST_COUNT} accelerators on ${NODE} are idle — an Instance claim cannot be pinned (no such input on the API, and its Pod is rendered without annotations), so it is not made while any accelerator is busy" \
    "${INST_CHECKS[@]}"
  record SKIP "OBSERVED: sliced Instance metrics" \
    "not evaluated — the Instance carrier is not used on this node, for the reason above"
else
  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata: { name: ${INSTANCES[0]}, namespace: default }
spec:
  type: ${IT}
  image: ${IMAGE}
  command: ["sleep", "86400"]
  nodeName: ${NODE}
  volume: { ephemeral: { capacity: 1Gi } }
  resources:
    accelerator: "1"
EOF
  phase=""
  for _ in $(seq 1 60); do
    phase=$(kubectl -n default get instance "${INSTANCES[0]}" -o jsonpath='{.status.phase}' 2>/dev/null)
    { [ "$phase" = Ready ] || [ "$phase" = Running ]; } && break
    sleep 3
  done
  { [ "$phase" = Ready ] || [ "$phase" = Running ]; }
  verdict $? "exclusive Instance: reaches Ready" \
    "${INSTANCES[0]} phase=${phase}" \
    "${INSTANCES[0]} phase='${phase:-<none>}' — the accelerated Instance chain did not complete"

  read -r inst_ids inst_modes <<<"$(allocated_of "${INSTANCES[0]}")"
  inst_buses=$(smi_buses "${INSTANCES[0]}")
  want_buses=""
  [ -n "${inst_ids:-}" ] && want_buses=$(buses_for $(ordinals_for "$inst_ids" | tr ',' ' '))
  [ -n "$inst_buses" ] && [ -n "$want_buses" ] && [ "$inst_buses" = "$want_buses" ]
  verdict $? "exclusive Instance: ppu-smi in the container agrees with the recorded allocation" \
    "ppu-smi sees {${inst_buses}} == the recorded accelerators {${want_buses}} (mode=${inst_modes:-?})" \
    "ppu-smi sees '{${inst_buses:-<unreadable>}}' vs the recorded {${want_buses:-<none>}}"
  [ -n "${inst_ids:-}" ] && TOUCHED="${TOUCHED} $(ordinals_for "$inst_ids" | tr ',' ' ')"

  RAW="/apis/worker.gpustack.ai/v1/namespaces/default/instances/${INSTANCES[0]}/metrics"
  metrics=""
  for _ in $(seq 1 20); do
    metrics=$(kubectl get --raw "$RAW" 2>/dev/null)
    [ -n "$metrics" ] && [ "$(printf '%s' "$metrics" | jq -r '(.sample.accelerators // []) | length')" -gt 0 ] && break
    sleep 5
  done
  n_alloc=0
  [ -n "${inst_ids:-}" ] && n_alloc=$(printf '%s' "$inst_ids" | tr ',' '\n' | grep -c .)
  n_metrics=$(printf '%s' "$metrics" | jq -r '(.sample.accelerators // []) | length' 2>/dev/null)
  { [ -n "${n_metrics:-}" ] && [ "$n_metrics" -eq "${n_alloc:-0}" ] && [ "$n_metrics" -gt 0 ]; }
  verdict $? "Instance metrics: accelerators present, one entry per allocated accelerator" \
    "${n_metrics} entry/entries for ${n_alloc} allocated accelerator(s)" \
    "the subresource served ${n_metrics:-<nothing>} accelerator entry/entries for ${n_alloc:-?} allocated accelerator(s)"

  metric_ids=$(printf '%s' "$metrics" | jq -r '[.sample.accelerators[]?.id] | sort | join(",")' 2>/dev/null)
  metric_buses=""
  [ -n "$metric_ids" ] && metric_buses=$(buses_for $(ordinals_for "$metric_ids" | tr ',' ' '))
  { [ -n "$metric_ids" ] && [ "$metric_ids" = "$(csv_sorted "${inst_ids:-}")" ] \
      && [ "$metric_buses" = "$inst_buses" ]; }
  verdict $? "Instance metrics: every entry's id matches the allocation and the container's view" \
    "ids {${metric_ids}} == the allocation's, and their bus addresses {${metric_buses}} == the container's" \
    "ids '{${metric_ids:-<none>}}' vs the allocation's '{${inst_ids:-<none>}}', bus addresses '{${metric_buses:-<none>}}' vs the container's '{${inst_buses:-<none>}}'"

  bad_mem=$(printf '%s' "$metrics" | jq -r --arg mx "$CARD_MEM_MIB" \
    '[.sample.accelerators[]? | select((.memoryTotalMiB // 0) != ($mx|tonumber)) | .id] | join(",")' 2>/dev/null)
  { [ -n "$metric_ids" ] && [ -z "$bad_mem" ]; }
  verdict $? "Instance metrics: memoryTotalMiB equals the accelerator's physical VRAM" \
    "every entry reports ${CARD_MEM_MIB}MiB, the accelerator's physical capacity" \
    "entries {${bad_mem:-<none read>}} do not report the physical ${CARD_MEM_MIB}MiB"

  bad_pct=$(printf '%s' "$metrics" | jq -r \
    '[.sample.accelerators[]? | select(((.memoryUtilizationPercent // 0) > 100) or ((.coresUtilizationPercent // 0) > 100)) | .id] | join(",")' 2>/dev/null)
  { [ -n "$metric_ids" ] && [ -z "$bad_pct" ]; }
  verdict $? "Instance metrics: utilization percentages within [0,100]" \
    "memory and cores utilization in range for every entry: $(printf '%s' "$metrics" | jq -c '[.sample.accelerators[]? | {id, memoryUtilizationPercent, coresUtilizationPercent}]' 2>/dev/null)" \
    "entries {${bad_pct:-<none read>}} report a utilization outside [0,100]"

  # A sliced Instance's metrics are an OBSERVATION: per-slice accelerator figures are not something
  # the subresource claims to serve, so whatever it answers is recorded rather than judged.
  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata: { name: ${INSTANCES[1]}, namespace: default }
spec:
  type: ${IT}
  image: ${IMAGE}
  command: ["sleep", "86400"]
  nodeName: ${NODE}
  volume: { ephemeral: { capacity: 1Gi } }
  resources:
    acceleratorSlicedMemoryPercentage: ${SLICE_MEM_PCT}
    acceleratorSlicedCoresPercentage: ${SLICE_CORES_PCT}
EOF
  sphase=""
  for _ in $(seq 1 60); do
    sphase=$(kubectl -n default get instance "${INSTANCES[1]}" -o jsonpath='{.status.phase}' 2>/dev/null)
    { [ "$sphase" = Ready ] || [ "$sphase" = Running ]; } && break
    sleep 3
  done
  # Keep the query's own status. "The array is absent" and "the subresource never answered" are
  # different observations, and only the first says anything about the product — recording the second
  # as the first would put a finding in the report that the run never established.
  smetrics=""; sanswered=0
  for _ in $(seq 1 12); do
    if smetrics=$(kubectl get --raw \
      "/apis/worker.gpustack.ai/v1/namespaces/default/instances/${INSTANCES[1]}/metrics" 2>/dev/null); then
      sanswered=1
      [ -n "$smetrics" ] && break
    fi
    sleep 5
  done
  s_n=$(printf '%s' "$smetrics" | jq -r '(.sample.accelerators // []) | length' 2>/dev/null)
  s_mem=$(printf '%s' "$smetrics" | jq -r '[.sample.accelerators[]?.memoryTotalMiB] | join(",")' 2>/dev/null)
  if [ "$sanswered" = 0 ] || [ -z "$smetrics" ]; then
    record SKIP "OBSERVED: sliced Instance metrics" \
      "phase=${sphase:-?}: the metrics subresource never answered, so nothing is observed here — this is not evidence that it serves no per-slice figures"
  elif [ "${s_n:-0}" -eq 0 ]; then
    record PASS "OBSERVED: sliced Instance metrics" \
      "phase=${sphase:-?}: the accelerators array is ABSENT for a sliced claim — the subresource serves no per-slice figures"
  elif [ "$s_mem" = "$CARD_MEM_MIB" ]; then
    record PASS "OBSERVED: sliced Instance metrics" \
      "phase=${sphase:-?}: ${s_n} entry/entries carrying WHOLE-ACCELERATOR figures (memoryTotalMiB=${s_mem}MiB), not the slice's share"
  else
    record PASS "OBSERVED: sliced Instance metrics" \
      "phase=${sphase:-?}: ${s_n} entry/entries with memoryTotalMiB={${s_mem}} against a physical ${CARD_MEM_MIB}MiB"
  fi
fi

# --- The safety property the whole case is built around, asserted explicitly rather than implied. ---
TOUCHED_SORTED=$(printf '%s\n' "$TOUCHED" | tr ' ,' '\n\n' | grep -E '^[0-9]+$' | sort -n -u | paste -sd, -)
echo
echo "[case-39] ordinals this run claimed: {${TOUCHED_SORTED:-none}} on ${NODE}"
[ "$UNSAFE" -eq 0 ]
verdict $? "no claim landed on an accelerator that was not idle beforehand" \
  "every claim stayed inside the idle set; ordinals touched: {${TOUCHED_SORTED:-none}}" \
  "${UNSAFE} claim(s) landed on an accelerator the host view did not report idle — the pin is not steering placement"

echo
echo "== CASE 39 — T-Head PPU: a pinned claim lands exactly where it was told, and a logical slice is capped inside the container =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). The pin is served by the device plugin's preferred-allocation hint,"
  echo "which the kubelet may decline: a hint it cannot parse is discarded silently and placement"
  echo "degrades to arbitrary, which shows up here as a claim landing off its pinned accelerator. The"
  echo "slice caps come from the shim the plugin preloads, so a cap that reads wrong is either the fold"
  echo "in the Pod webhook or the injection at Allocate. Raise the device-manager's verbosity BEFORE"
  echo "re-running to see the hint in full (see the shared troubleshooting reference, 'Component log"
  echo "verbosity') — a device id in it WITHOUT a trailing ':<token>' segment is the defect itself."
  echo "Diagnose: kubectl get devices.v1alpha1.worker.gpustack.ai ${NODE} -o yaml;"
  echo "kubectl -n ${NS} logs ds/gpustack-operator-device-manager-thead --tail=100"
  exit 1
fi
echo "CASE 39 PASS"
