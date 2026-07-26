#!/usr/bin/env bash
#
# _partition-lib.sh — shared discovery and assertion helpers for the hardware-partition cases.
#
# NOT A CASE. The leading underscore keeps it out of the `case-N.sh` namespace: it carries no case
# header, no trap and no results table. Every one of those stays with the case that sources it, so a
# case still reads end to end on its own terms; this file only removes eleven copies of the same node
# correlation, profile discovery and pod plumbing.
#
# A case uses it as:
#
#     set -uo pipefail
#     NS="${1:?usage: ...}"
#     CASE_ID=24
#     # shellcheck source=/dev/null
#     . "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_partition-lib.sh"
#     part_require_node_ssh "case-24.sh"     # exits 2 when MIG_NODE_SSH is unset
#     part_require_mig_capable               # skips (exit 0) when the card is not MIG-capable
#     part_discover                          # correlates the k8s node, pool, queue and key bases
#
# Globals the case may read after those three calls:
#   GPU_INDEX GPU_NODE GROUPID IT LQ MANUF SLICED PARTITIONED RUNTIMECLASS RTC_LINE INITIAL_MODE
#   FAILS ROWS TESTPODS
#
# Environment the caller may set (all optional except MIG_NODE_SSH):
#   MIG_NODE_SSH=<user@host>    REQUIRED. The node's SSH address; never written into the repo.
#   MIG_NODE_SSH_OPTS='-i ...'  extra ssh options (identity file, port, jump host, …)
#   MIG_NODE_NAME=<k8s node>    disambiguate when several nodes carry an accelerator
#   MIG_GPU_INDEX=<n>           the card these helpers act on by default (default 0)
#   MIG_SSH_TIMEOUT=<secs>      per-ssh timeout (default 90)
#   IMAGE=<image>               test-pod image (default ubuntu:24.04)

MIGSSH_TIMEOUT="${MIG_SSH_TIMEOUT:-90}"
GPU_INDEX="${MIG_GPU_INDEX:-0}"
IMAGE="${IMAGE:-ubuntu:24.04}"
DM_DS=gpustack-operator-device-manager-nvidia
ANNO=device.gpustack.ai/accelerator.allocated

CASE_ID="${CASE_ID:-?}"
FAILS=0
ROWS=()
TESTPODS=()

# ---------------------------------------------------------------------------------------------------
# Result bookkeeping — one row per check, printed as the case's results table at the end.
# ---------------------------------------------------------------------------------------------------

record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

# part_results <title> — the results banner + table. The case owns the FAIL footer and the exit code.
part_results() {
  echo
  echo "== CASE ${CASE_ID} — $1 =="
  {
    echo "STATUS|CHECK|OBJECT"
    [ "${#ROWS[@]}" -gt 0 ] && printf '%s\n' "${ROWS[@]}"
  } | column -t -s '|'
}

# part_skip <lines...> — self-skip (exit 0): the cluster does not provide what the case needs.
part_skip() {
  echo "== CASE ${CASE_ID} — SKIPPED =="
  printf '%s\n' "$@"
  exit 0
}

# ---------------------------------------------------------------------------------------------------
# Node SSH — the address the cases must NOT guess.
# ---------------------------------------------------------------------------------------------------

# node_ssh <cmd...> — run a command on the accelerator node (non-interactive, bounded). sudo is the
# caller's responsibility (nvidia-smi mode switches and MIG instance management need it).
# One shared control connection for the whole case. wait_card_idle alone polls up to 40 times, and
# every poll otherwise pays a full TCP + auth handshake to a remote node; multiplexing collapses them
# onto a single handshake. ControlPersist outlives the `timeout` killing a slow client, so the master
# survives an individual call being cut short. node_ssh_close tears it down from the trap.
MIGSSH_CTL="${TMPDIR:-/tmp}/gpustack-e2e-mig-$$.sock"
node_ssh() {
  # shellcheck disable=SC2086
  timeout "${MIGSSH_TIMEOUT}" ssh -o StrictHostKeyChecking=no -o ConnectTimeout=15 -o BatchMode=yes \
    -o ControlMaster=auto -o ControlPath="$MIGSSH_CTL" -o ControlPersist=300 \
    ${MIG_NODE_SSH_OPTS:-} "$MIG_NODE_SSH" "$@"
}
node_ssh_close() {
  ssh -O exit -o ControlPath="$MIGSSH_CTL" "${MIG_NODE_SSH:-}" >/dev/null 2>&1 || true
}

# part_require_node_ssh <script-name> — EXIT 2 (input required) when MIG_NODE_SSH is unset, and skip
# when the address is set but unreachable. The address is supplied at run time and stays out of the
# repo; the case never auto-discovers it.
part_require_node_ssh() {
  local script="$1"
  if [ -z "${MIG_NODE_SSH:-}" ]; then
    echo "== CASE ${CASE_ID} — INPUT REQUIRED (not run) =="
    echo
    echo "This case reaches the accelerator node over SSH (partitioning mode, live instances, kubelet),"
    echo "so it needs the node's SSH address. It will not guess it. Provide it and re-run:"
    echo
    echo "    MIG_NODE_SSH=<user@host> bash .claude/skills/gpustack-operator-e2e/cases/${script} ${NS}"
    echo
    echo "Optional overrides:"
    echo "    MIG_NODE_NAME=<k8s node>   # disambiguate when several nodes carry an accelerator"
    echo "    MIG_NODE_SSH_OPTS='-i ...' # extra ssh options (identity file, port, jump host, …)"
    echo "    MIG_GPU_INDEX=<n>          # which card the case acts on (default 0)"
    echo "    MIG_SSH_TIMEOUT=<secs>     # per-ssh timeout (default 90)"
    echo
    echo "The address stays out of this script and out of the repo — it is passed at run time only."
    exit 2
  fi
  if ! node_ssh true >/dev/null 2>&1; then
    part_skip "Cannot SSH to '${MIG_NODE_SSH}' (BatchMode, ${MIGSSH_TIMEOUT}s). Check the address / key / MIG_NODE_SSH_OPTS."
  fi
}

# ---------------------------------------------------------------------------------------------------
# Hardware partitioning mode.
# ---------------------------------------------------------------------------------------------------

# mig_mode <index> — the card's current partitioning mode as nvidia-smi reports it: Enabled, Disabled,
# or something else (including empty) on a card that has no such mode.
mig_mode() {
  node_ssh sudo nvidia-smi -i "$1" --query-gpu=mig.mode.current --format=csv,noheader 2>/dev/null \
    | tr -d '[:space:]'
}

# part_require_mig_capable — skip unless the default card reports a partitioning mode. Sets
# INITIAL_MODE to the mode the case found, which every mutating case restores in its trap.
part_require_mig_capable() {
  INITIAL_MODE="$(mig_mode "$GPU_INDEX")"
  case "$INITIAL_MODE" in
    Enabled | Disabled) ;;
    *)
      part_skip \
        "Card ${GPU_INDEX} on ${MIG_NODE_SSH} reports no hardware partitioning mode (got '${INITIAL_MODE:-<none>}')." \
        "Hardware partitioning needs an A100/A30/H100/H200/B-series (or newer) data-center card." \
        "Run this case on such a node."
      ;;
  esac
  echo "[case-${CASE_ID}] node ${MIG_NODE_SSH} card ${GPU_INDEX}: partitioning mode currently ${INITIAL_MODE}"
}

# set_mig_mode <index> <0|1> — toggle a card's partitioning mode and wait for it to converge.
# Returns 0 on convergence, 1 otherwise (the caller records the failure with guidance).
set_mig_mode() {
  local idx="$1" want="$2" target cur
  [ "$want" = 1 ] && target=Enabled || target=Disabled
  echo "[case-${CASE_ID}]   nvidia-smi -i ${idx} -mig ${want} on ${MIG_NODE_SSH}"
  node_ssh sudo nvidia-smi -i "$idx" -mig "$want" >/dev/null 2>&1 || true
  for _ in $(seq 1 20); do
    cur="$(mig_mode "$idx")"
    [ "$cur" = "$target" ] && return 0
    sleep 3
  done
  return 1
}

# refresh_dm — the Device Manager writes each card's capability at startup, and its detect loop
# compares only {manufacturer, id, unhealthy}, which a partitioning-mode toggle does not change — so a
# mode flip is picked up by RESTARTING the DaemonSet. Deleting the Devices object is deliberately NOT
# done: an existing group's capability is rewritten in place, and deleting it would hide a regression
# of that.
refresh_dm() {
  echo "[case-${CASE_ID}]   refreshing Device Manager (rollout restart ${DM_DS}; the Devices object is kept)"
  kubectl -n "$NS" rollout restart "ds/${DM_DS}" >/dev/null 2>&1 || true
  kubectl -n "$NS" rollout status "ds/${DM_DS}" --timeout=180s >/dev/null 2>&1 || true
  for _ in $(seq 1 30); do kubectl get devices.worker.gpustack.ai "$GPU_NODE" >/dev/null 2>&1 && break; sleep 3; done
}

# node_gi_count — the number of live GPU instances the node's cards actually hold (ground truth from
# the hardware, not from the ledger). grep -c prints "0" AND exits non-zero on no match, so the value
# is captured (a bare `|| echo 0` would append a SECOND "0" under pipefail).
node_gi_count() {
  local out
  out="$(node_ssh sudo nvidia-smi mig -lgi 2>/dev/null | grep -cE '^\|[[:space:]]+[0-9]+')"
  echo "${out:-0}"
}

# wait_card_idle — poll until no live GPU instance is left, bounded ~120s. A mode switch needs an idle
# card, so callers wait for the last workload's instance to reclaim before toggling.
wait_card_idle() {
  for _ in $(seq 1 40); do
    [ "$(node_gi_count)" = 0 ] && return 0
    sleep 3
  done
  return 1
}

# ---------------------------------------------------------------------------------------------------
# Cluster-side correlation: the k8s node behind the SSH address, its pool, queue and key bases.
# ---------------------------------------------------------------------------------------------------

part_discover() {
  GPU_NODE="${MIG_NODE_NAME:-}"
  if [ -z "$GPU_NODE" ]; then
    local _nv=() _n
    while IFS= read -r _n; do [ -n "$_n" ] && _nv+=("$_n"); done < <(kubectl get devices -o json 2>/dev/null | python3 -c "
import json,sys
for d in json.load(sys.stdin).get('items',[]):
    for g in d.get('spec',{}).get('groups',[]):
        if g.get('manufacturer')=='nvidia' and g.get('accelerators'):
            print(d['metadata']['name']); break
" 2>/dev/null)
    if [ "${#_nv[@]}" -eq 0 ]; then
      part_skip "No Devices object reports an nvidia accelerator group — the operator chain is not observing a GPU node."
    fi
    if [ "${#_nv[@]}" -gt 1 ]; then
      echo "[case-${CASE_ID}] several nvidia GPU nodes (${_nv[*]}); set MIG_NODE_NAME=<the node behind ${MIG_NODE_SSH}> and re-run"
      exit 2
    fi
    GPU_NODE="${_nv[0]}"
  fi
  echo "[case-${CASE_ID}] correlated k8s node: ${GPU_NODE}"

  GROUPID=$(kubectl get devices "$GPU_NODE" -o json 2>/dev/null | python3 -c "
import json,sys
for g in json.load(sys.stdin).get('spec',{}).get('groups',[]):
    if g.get('manufacturer')=='nvidia' and g.get('accelerators'):
        print(g.get('id','')); break
" 2>/dev/null)

  read -r IT LQ MANUF <<<"$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null | NODEGID="$GROUPID" python3 -c "
import json,sys,os
gid=os.environ.get('NODEGID','')
items=json.load(sys.stdin).get('items',[])
for it in items:
    s=it.get('spec',{}); st=it.get('status',{})
    if s.get('acceleratable') and gid and gid in it['metadata']['name'] and st.get('entrance'):
        print(it['metadata']['name'], st['entrance'], s.get('manufacturer','nvidia')); sys.exit(0)
for it in items:
    s=it.get('spec',{}); st=it.get('status',{})
    if s.get('acceleratable') and st.get('entrance'):
        print(it['metadata']['name'], st['entrance'], s.get('manufacturer','nvidia')); sys.exit(0)
")"
  if [ -z "${IT:-}" ] || [ -z "${LQ:-}" ]; then
    echo "[case-${CASE_ID}] no accelerated InstanceType with an entrance LocalQueue — chain not materialized"
    exit 1
  fi
  MANUF="${MANUF:-nvidia}"

  # The two disjoint accelerator families. LOGICAL slicing (software, the vendor preload library) is
  # served only by a card that is NOT in a hardware partitioning mode; PHYSICAL partitioning (the
  # hardware) only by a card that is.
  EXCLUSIVE="${MANUF}.com/gpu"
  SHARED="${MANUF}.com/gpu.shared"
  SLICED="${MANUF}.com/gpu.sliced"
  PARTITIONED="${MANUF}.com/gpu.partitioned"
  echo "[case-${CASE_ID}] accelerated InstanceType ${IT} (entrance LocalQueue ${LQ}, group ${GROUPID:-?}, logical base ${SLICED}, partition base ${PARTITIONED})"

  # The vendor runtimeClass mounts the driver libs the workload needs (nvidia-smi -L, and — on the
  # logically sliced path — the LD_PRELOAD'd libvgpu.so, which exits 127 without it).
  RUNTIMECLASS=""
  if kubectl get runtimeclass.node.k8s.io "$MANUF" >/dev/null 2>&1; then RUNTIMECLASS="$MANUF"; fi
  RTC_LINE=""
  [ -n "$RUNTIMECLASS" ] && RTC_LINE="runtimeClassName: ${RUNTIMECLASS}"
  echo "[case-${CASE_ID}] test-pod runtimeClass: ${RUNTIMECLASS:-<none>}"
}

# ---------------------------------------------------------------------------------------------------
# Node resource keys.
# ---------------------------------------------------------------------------------------------------

# node_key <resource> — the node's ALLOCATABLE quantity for an extended resource (empty if absent).
node_key() { kubectl get node "$GPU_NODE" -o jsonpath="{.status.allocatable['${1//./\\.}']}" 2>/dev/null; }

# node_key_cap <resource> — the node's CAPACITY quantity. The operator writes capacity and the kubelet
# derives allocatable from it, so a key that is merely saturated has capacity intact and allocatable low.
node_key_cap() { kubectl get node "$GPU_NODE" -o jsonpath="{.status.capacity['${1//./\\.}']}" 2>/dev/null; }

# node_key_gone <resource> — true when a key is absent OR reads 0. Reconciler-owned counting keys are
# genuinely REMOVED when their family leaves the node, but a device-plugin pool key only zeroes out —
# the kubelet keeps the entry until it restarts — so "gone" must accept both for the pool keys.
node_key_gone() { local v; v="$(node_key "$1")"; [ -z "$v" ] || [ "$v" = 0 ]; }

# partition_profile_keys — every "<base>.partitioned.<kind>-<profile>" key the node advertises (never
# ".partitioned.units", which is a counting key, not a profile). The kind segment is the manufacturer's
# own name for hardware partitioning and is read off the node rather than assumed, so no case hardcodes
# a vendor's spelling of it.
partition_profile_keys() {
  kubectl get node "$GPU_NODE" -o json 2>/dev/null | PFX="${PARTITIONED}." python3 -c "
import json,os,sys
pfx=os.environ['PFX']
a=json.load(sys.stdin).get('status',{}).get('allocatable',{})
print(' '.join(sorted(k for k in a if k.startswith(pfx) and not k.endswith('.units'))))
" 2>/dev/null
}

# profile_key <profile> — the advertised per-profile key whose profile segment is <profile>, or empty.
profile_key() {
  local want="$1" k
  for k in $(partition_profile_keys); do
    case "$k" in *-"$want") echo "$k"; return ;; esac
  done
  echo ""
}

# wait_partition_keys — poll until the node advertises at least one per-profile partition key.
wait_partition_keys() {
  local keys=""
  for _ in $(seq 1 30); do
    keys="$(partition_profile_keys)"
    [ -n "$keys" ] && break
    sleep 4
  done
  echo "$keys"
}

# ---------------------------------------------------------------------------------------------------
# Card geometry, discovered from the cards' own capability (never hardcoded).
# ---------------------------------------------------------------------------------------------------

# card_profiles — the SMALL (1 memory slice) / MID (4 memory slices, 3 compute) / FULL (8 memory
# slices) profiles with their per-card instance counts, the partitioned-card count N and the group's
# total card count NTOT. The geometry is CAPABILITY, so it lives in Devices.spec; the runtime ledger is
# in .status. Emits: SMALL SMALL_CNT MID MID_CNT FULL FULL_CNT N NTOT.
card_profiles() {
  kubectl get devices "$GPU_NODE" -o json 2>/dev/null | python3 -c "
import json,sys
d=json.load(sys.stdin)
small=mid=full=None; n=0; ntot=0
for g in d.get('spec',{}).get('groups',[]):
    if g.get('manufacturer')!='nvidia': continue
    for a in g.get('accelerators',[]):
        ntot+=1
        profs=a.get('status',{}).get('physicalSliced',{}).get('profiles',[]) or []
        if profs: n+=1
        for p in profs:
            ms=p.get('memorySlices',0); cs=p.get('computeSlices',0)
            if ms==1 and (small is None or p.get('memoryMib',0)<small[1]): small=(p['name'],p.get('memoryMib',0),p.get('count',0))
            if ms==4 and cs==3: mid=(p['name'],p.get('memoryMib',0),p.get('count',0))
            if ms==8: full=(p['name'],p.get('memoryMib',0),p.get('count',0))
def f(x): return (x[0], x[2]) if x else ('', 0)
sn,sc=f(small); mn,mc=f(mid); fn,fc=f(full)
print(sn, sc, mn, mc, fn, fc, n, ntot)
" 2>/dev/null
}

# partition_ceiling — Σ over the node's partitioned cards of each card's physical-slice ceiling (the
# largest instance count across its profiles). It is what the partition token pool is sized from on an
# empty node, before any instance is carved.
partition_ceiling() {
  kubectl get devices "$GPU_NODE" -o json 2>/dev/null | python3 -c "
import json,sys
tot=0
for g in json.load(sys.stdin).get('spec',{}).get('groups',[]):
    for a in g.get('accelerators',[]):
        ps=a.get('status',{}).get('physicalSliced',{}) or {}
        if ps.get('profiles'): tot+=int(ps.get('count',0) or 0)
print(tot)
" 2>/dev/null
}

# partitioned_cards — the "<group>:<accelerator-id>" of every card whose capability reports hardware
# partition profiles, space-separated with spaces inside an id folded to '~' (the same folding
# pod_cards uses, so the two sets compare by shell word-splitting).
partitioned_cards() { _cards_by_state 1; }

# unpartitioned_cards — the complement: cards that report no partition profiles and therefore serve the
# whole-card, shared and logical-slice families instead.
unpartitioned_cards() { _cards_by_state 0; }

_cards_by_state() {
  kubectl get devices "$GPU_NODE" -o json 2>/dev/null | WANT="$1" python3 -c "
import json,os,sys
want=os.environ['WANT']=='1'
out=[]
for g in json.load(sys.stdin).get('spec',{}).get('groups',[]):
    if not g.get('accelerators'): continue
    gid=g.get('id','')
    for a in g.get('accelerators',[]):
        profs=a.get('status',{}).get('physicalSliced',{}).get('profiles',[]) or []
        if bool(profs)==want:
            out.append(('%s:%s' % (gid, a.get('id',''))).replace(' ','~'))
print(' '.join(sorted(out)))
" 2>/dev/null
}

# card_indexes — every accelerator index the group reports, ascending. These are the nvidia-smi indexes
# a case passes to set_mig_mode.
card_indexes() {
  kubectl get devices "$GPU_NODE" -o json 2>/dev/null | python3 -c "
import json,sys
idx=[]
for g in json.load(sys.stdin).get('spec',{}).get('groups',[]):
    if g.get('manufacturer')!='nvidia': continue
    for a in g.get('accelerators',[]):
        idx.append(int(a.get('index',0)))
print(' '.join(str(i) for i in sorted(idx)))
" 2>/dev/null
}

# ---------------------------------------------------------------------------------------------------
# Establishing and restoring the partitioning state.
# ---------------------------------------------------------------------------------------------------

# part_ensure_partitioned_card — make sure at least one card is in the partitioning mode, toggling the
# default card ONLY when none is. A card found already partitioned is left as found, so a lead who
# partitioned a card once can run every partition case without paying a mode switch per case. Any card
# this helper toggles is appended to TOGGLED, which part_restore_toggled reverses from the case's trap.
part_ensure_partitioned_card() {
  TOGGLED="${TOGGLED:-}"
  if [ -n "$(partitioned_cards)" ]; then
    echo "[case-${CASE_ID}] a partitioned card is already present — left as found"
    return 0
  fi
  echo "[case-${CASE_ID}] no partitioned card yet — putting card ${GPU_INDEX} into the partitioning mode"
  if ! set_mig_mode "$GPU_INDEX" 1; then
    part_skip \
      "Card ${GPU_INDEX} did not converge to the partitioning mode (a pending GPU reset, a busy card," \
      "or a loaded nvidia_drm blocks it). Drain the card and re-run."
  fi
  TOGGLED="${TOGGLED}${GPU_INDEX} "
  refresh_dm
  wait_partition_keys >/dev/null
}

# part_restore_toggled — put back exactly the cards part_ensure_partitioned_card switched, and no
# others. Safe to call when nothing was toggled.
#
# MIG_KEEP_MODE=1 suppresses the restore and leaves the card partitioned. A mode switch costs a
# `nvidia-smi` round trip plus a Device Manager rollout and its settle — minutes, and the dominant
# cost of a short case. Run standalone, a case must restore what it changed, so the default stays
# "restore". Run as a BLOCK, every case pays that twice only for the next case to pay it again, so
# the block-level runner sets this and restores once at the end. It changes no assertion: cases that
# require an unpartitioned start (CASE 27) skip rather than silently weaken when they find one.
part_restore_toggled() {
  if [ -z "${TOGGLED:-}" ]; then
    node_ssh_close
    return 0
  fi
  if [ "${MIG_KEEP_MODE:-0}" = 1 ]; then
    echo "[case-${CASE_ID}] MIG_KEEP_MODE=1 — leaving card(s) '${TOGGLED}' partitioned for the next case"
    node_ssh_close
    return 0
  fi
  wait_card_idle || true
  local i
  for i in $TOGGLED; do set_mig_mode "$i" 0 || true; done
  refresh_dm
  node_ssh_close
}

# ---------------------------------------------------------------------------------------------------
# Test pods.
# ---------------------------------------------------------------------------------------------------

# mkpod <name> <resource-lines> [image] [restartPolicy] [command-json] — a Pod on the accelerator node,
# submitted through the pool's entrance LocalQueue. <resource-lines> is the indented block that goes
# under both limits and requests.
mkpod() {
  local name="$1" reslines="$2" image="${3:-$IMAGE}" rp="${4:-Never}" cmd="${5:-[\"sleep\", \"86400\"]}"
  TESTPODS+=("$name")
  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: { name: ${name}, namespace: default, labels: { kueue.x-k8s.io/queue-name: ${LQ} } }
spec:
  schedulerName: default-scheduler
  ${RTC_LINE}
  restartPolicy: ${rp}
  nodeSelector: { kubernetes.io/hostname: ${GPU_NODE} }
  containers:
    - name: main
      image: ${image}
      command: ${cmd}
      resources:
        limits:
${reslines}
        requests:
${reslines}
EOF
}

# partition_reslines <profile-key> — the resource block for a one-instance partition request.
partition_reslines() {
  printf '          %s: "1"\n          %s: "1"' "$PARTITIONED" "$1"
}

# sliced_reslines <memory-pct> [cores-pct] — the resource block for a one-card logical slice request.
sliced_reslines() {
  printf '          %s: "1"\n          %s.memory-percentage: "%s"\n          %s.cores-percentage: "%s"' \
    "$SLICED" "$SLICED" "$1" "$SLICED" "${2:-$1}"
}

delpod() { kubectl -n default delete pod "$1" --ignore-not-found --force --grace-period=0 >/dev/null 2>&1 || true; }

delpods() { local p; for p in "$@"; do delpod "$p"; done; }

# delete_test_pods — delete everything mkpod created. Guarding on the length keeps it safe on a shell
# where expanding an empty array under `set -u` is an error, which is exactly the state a trap firing
# before the first Pod is created would hit.
delete_test_pods() { [ "${#TESTPODS[@]}" -gt 0 ] && delpods "${TESTPODS[@]}"; return 0; }

phase()   { kubectl -n default get pod "$1" -o jsonpath='{.status.phase}' 2>/dev/null; }
running() { [ "$(phase "$1")" = "Running" ]; }

# wait_running <pod> [tries] — poll until the Pod is Running (default ~120s).
wait_running() { local t="${2:-40}"; for _ in $(seq 1 "$t"); do running "$1" && return 0; sleep 3; done; return 1; }

# pod_events <pod> — the reasons of the events recorded against a Pod, one per line.
pod_events() {
  kubectl -n default get events --field-selector involvedObject.name="$1" \
    -o jsonpath='{range .items[*]}{.reason}{"\n"}{end}' 2>/dev/null
}

# pod_unexpected_admission <pod> — non-empty when the Pod hit the terminal device-plugin admission
# failure this whole family split exists to remove.
pod_unexpected_admission() {
  local r
  r="$(pod_events "$1" | grep -c 'UnexpectedAdmissionError')"
  [ "${r:-0}" = 0 ] && echo "" || echo "$r"
}

# workload_refusal <pod> — the Pod's own Kueue Workload verdict, when an AdmissionCheck has actually
# rendered one: "admission-check(<name>/<state>)" for Retry or Rejected, empty otherwise. This is the
# product saying "I refuse to admit this", so it is conclusive the moment it appears.
workload_refusal() {
  kubectl -n default get workloads.kueue.x-k8s.io -o json 2>/dev/null | python3 -c "
import json,sys
pod='$1'
for wl in json.load(sys.stdin).get('items',[]):
    if not any(o.get('name')==pod for o in wl.get('metadata',{}).get('ownerReferences',[])):
        continue
    st=wl.get('status',{})
    if any(c.get('type')=='Admitted' and c.get('status')=='True' for c in st.get('conditions',[])):
        break
    for c in st.get('admissionChecks',[]):
        if c.get('state') in ('Retry','Rejected'):
            print('admission-check(%s/%s)' % (c.get('name',''), c.get('state')))
            break
    break
" 2>/dev/null
}

# held_reason <pod> — a DECISIVE signal that the Pod was refused: a terminal failure, a device-plugin
# admission refusal, an Unschedulable verdict, or an AdmissionCheck that declined it. Empty when none
# is present. The bare Kueue scheduling gate is deliberately NOT among these — see assert_held.
held_reason() {
  local p="$1" ph cond ev wl
  ph="$(phase "$p")"
  [ "$ph" = Failed ] && { echo "Failed/$(kubectl -n default get pod "$p" -o jsonpath='{.status.reason}' 2>/dev/null)"; return; }
  cond="$(kubectl -n default get pod "$p" -o jsonpath='{.status.conditions[?(@.type=="PodScheduled")].reason}' 2>/dev/null)"
  [ "$cond" = Unschedulable ] && { echo "Unschedulable"; return; }
  ev="$(pod_events "$p" | grep -iE 'UnexpectedAdmissionError|FailedScheduling' | head -1)"
  [ -n "$ev" ] && { echo "$ev"; return; }
  wl="$(workload_refusal "$p")"
  [ -n "$wl" ] && { echo "$wl"; return; }
  echo ""
}

# assert_held <pod> [polls] — confirm the Pod is HELD rather than merely slow; prints the evidence, or
# empty on "cannot confirm".
#
# Kueue's pod integration gates EVERY Pod born in this namespace, so the bare kueue.x-k8s.io/admission
# scheduling gate proves nothing on its own: a Pod that is about to admit normally carries it too, for
# the second or two before Kueue clears it. Evidence therefore comes in two grades:
#
#   DECISIVE  — a verdict has been rendered against the Pod (held_reason). Conclusive on sight.
#   ENDURING  — nothing but the birth gate. It means "held" only once it OUTLASTS the window a healthy
#               admission needs, so it must survive every poll of the window.
#
# The window is always polled to its end unless the Pod resolves, because the device-plugin's
# UnexpectedAdmissionError backstop fires only AFTER Kueue clears the gate. Reaching Running at any
# point returns empty: the Pod was never held.
assert_held() {
  local p="$1" t="${2:-20}" hr gate last_gate="" gated_throughout=1
  for _ in $(seq 1 "$t"); do
    running "$p" && { echo ""; return; }
    hr="$(held_reason "$p")"
    [ -n "$hr" ] && { echo "$hr"; return; }
    gate="$(kubectl -n default get pod "$p" -o jsonpath='{.spec.schedulingGates[*].name}' 2>/dev/null)"
    if [ -n "$gate" ]; then last_gate="$gate"; else gated_throughout=0; fi
    sleep 3
  done
  [ "$gated_throughout" = 1 ] && [ -n "$last_gate" ] &&
    { echo "scheduling-gated(${last_gate}) sustained across ${t} polls"; return; }
  echo ""
}

# pod_cards <pod> — the sorted "<group>:<device>" physical card(s) the device plugin allocated, read
# from the Pod's allocation annotation (present once Allocate ran, even before the container starts).
# The annotation's value is keyed by container name; every container's entry is folded in. Device IDs
# may contain spaces, so each card token is emitted with spaces folded to '~'.
pod_cards() {
  kubectl -n default get pod "$1" -o json 2>/dev/null | ANNO="$ANNO" python3 -c "
import json,sys,os
try: o=json.load(sys.stdin)
except Exception: sys.exit(0)
ann=o.get('metadata',{}).get('annotations',{}).get(os.environ['ANNO'],'')
if not ann: sys.exit(0)
try: st=json.loads(ann)
except Exception: sys.exit(0)
cards=set()
for c in st.values():
    for g in (c.get('devices') or {}).get('groups',[]):
        gid=g.get('id','')
        for a in g.get('accelerators',[]):
            cards.add(('%s:%s' % (gid, a.get('id',''))).replace(' ','~'))
print(' '.join(sorted(cards)))
"
}

# pod_profiles <pod> — the partition profile name(s) the plugin recorded against this Pod's cards, from
# the same annotation. This is the durable record of WHAT was actuated, as opposed to which card.
pod_profiles() {
  kubectl -n default get pod "$1" -o json 2>/dev/null | ANNO="$ANNO" python3 -c "
import json,sys,os
try: o=json.load(sys.stdin)
except Exception: sys.exit(0)
ann=o.get('metadata',{}).get('annotations',{}).get(os.environ['ANNO'],'')
if not ann: sys.exit(0)
try: st=json.loads(ann)
except Exception: sys.exit(0)
out=set()
for c in st.values():
    for g in (c.get('devices') or {}).get('groups',[]):
        for a in g.get('accelerators',[]):
            p=a.get('allocatedPhysicalProfile','')
            if p: out.add(p)
print(' '.join(sorted(out)))
"
}

# wait_pod_cards <pod> [tries] — poll until the Pod's allocation annotation names a card; prints it.
wait_pod_cards() {
  local c="" t="${2:-20}"
  for _ in $(seq 1 "$t"); do c="$(pod_cards "$1")"; [ -n "$c" ] && break; sleep 3; done
  echo "$c"
}

# pod_mig_devices <pod> — how many MIG devices nvidia-smi -L reports inside the Pod (want exactly 1 for
# a single-instance partition request).
pod_mig_devices() {
  local out
  out="$(kubectl -n default exec "$1" -- nvidia-smi -L 2>/dev/null | grep -c 'MIG')"
  echo "${out:-0}"
}

# in_set <token> <padded-set> — membership test against a " a b c " style set.
in_set() { case "$2" in *" $1 "*) return 0 ;; esac; return 1; }

# pad_set <tokens...> — wrap a token list into the " a b c " shape in_set expects.
pad_set() { local out=" " t; for t in "$@"; do out="${out}${t} "; done; echo "$out"; }

# ---------------------------------------------------------------------------------------------------
# Node status write-volume probe — the ledger-derived per-profile key re-patches node capacity on every
# partition allocate/release, so its write cost is measured rather than assumed.
# ---------------------------------------------------------------------------------------------------

NODE_WRITE_LOG=""
NODE_WRITE_PID=""

start_node_write_probe() {
  NODE_WRITE_LOG="$(mktemp)"
  (
    last=""
    while :; do
      rv="$(kubectl get node "$GPU_NODE" -o jsonpath='{.metadata.resourceVersion}' 2>/dev/null)"
      if [ -n "$rv" ] && [ "$rv" != "$last" ]; then echo "$rv"; last="$rv"; fi
      sleep 1
    done
  ) >"$NODE_WRITE_LOG" 2>/dev/null &
  NODE_WRITE_PID=$!
}

stop_node_write_probe() {
  [ -n "$NODE_WRITE_PID" ] || return 0
  kill "$NODE_WRITE_PID" >/dev/null 2>&1 || true
  wait "$NODE_WRITE_PID" 2>/dev/null || true
  NODE_WRITE_PID=""
}

# node_write_count — observed Node-object writes since the probe started. The first sample is the
# starting resourceVersion, so the transition count is one less than the sample count.
node_write_count() {
  local n
  n="$(wc -l <"$NODE_WRITE_LOG" 2>/dev/null | tr -d '[:space:]')"
  [ -z "$n" ] && { echo 0; return; }
  [ "$n" -le 1 ] && { echo 0; return; }
  echo "$((n - 1))"
}
