#!/usr/bin/env bash
#
# CASE 23 — NVIDIA MIG dynamic-allocation lifecycle
#   (MUTATING, self-recovering; AUTO-SKIPS without a MIG-capable NVIDIA card AND a node-SSH address)
#
#   MIG_NODE_SSH=<user@host> case-23.sh <NS>
#
# Goal:        On a MIG-capable NVIDIA card the operator observes MIG geometry and dynamically
#              allocates the hardware GPU/compute instances that back scheduled workloads. This case
#              proves, end to end on real hardware, that:
#                - a card whose MIG mode is OFF serves logical slices — two Pods at 20% and 40%
#                  coexist and are runtime-capped (the vendor libvgpu path);
#                - after the ADMIN enables MIG (nvidia-smi, over SSH) and the Device Manager re-detects,
#                  the card carves into its canonical profile set (names/counts match the ledger) and
#                  MOVES FAMILY: the node advertises one nvidia.com/gpu.partitioned.<kind>-<profile> key
#                  per profile plus nvidia.com/gpu.partitioned.units, while the whole logical family
#                  (nvidia.com/gpu.sliced + its .units/.cores-percentage/.memory-percentage/.memory-mib
#                  counting keys) DISAPPEARS — a partitioned card serves no logical slice, so it leaves
#                  the logical population entirely rather than keeping a units key;
#                - the InstanceType numeric four-view (EX/SH/SL/PT) + the per-profile ledger reflect the
#                  geometry: PT carries the partition instances and SL collapses to zero once every card
#                  of the pool is partitioned;
#                - profiles are placement-mutually-exclusive (a size-4 3g instance blocks a size-8 7g
#                  instance on the same card; two size-4 fill it, a third is held);
#                - deleting a MIG Pod frees the instance so a fresh request of the same profile admits
#                  again (reuse), and an idle card's instance is reclaimed within the debounce;
#                - after small instances are freed a whole-card profile fits again;
#                - disabling MIG and re-detecting returns the card to its logical-slice capability, and
#                  every .partitioned key disappears in the same move.
# Environment: A reachable cluster whose active context is the GPU cluster, a node with a real NVIDIA
#              card, AND SSH to that node (sudo nvidia-smi) supplied via MIG_NODE_SSH=<user@host>.
#              This case is the ONE case that toggles node hardware state, so it needs the node address;
#              it does NOT guess it. It EXITS 2 (input required) when MIG_NODE_SSH is unset — provide the
#              address and re-run. It AUTO-SKIPS (exit 0) when the node's card is not MIG-capable, or the
#              cluster advertises no nvidia accelerator.
#              Prerequisites the ADMIN owns (the case attempts the mode switch but cannot force them):
#              the card must be idle, driver-handle daemons (DCGM/nvsm/exporters) stopped, nvidia_drm
#              unloaded, CAP_SYS_ADMIN available. Targets Hopper+ (no GPU reset needed); on Ampere a
#              mode switch that pends a reset is reported as a FAIL with guidance, not forced.
# Inputs:      All real, nothing mocked — the MIG geometry, the libvgpu cap, and the GI/CI create/destroy
#              are the verification. The case toggles the node's MIG mode over SSH and restarts the
#              Device Manager to re-detect (a DaemonSet restart is sufficient — an existing group's
#              capability is rewritten in place); test Pods go through the accelerated pool's entrance
#              LocalQueue. Profiles (SMALL size-1, MID size-4 "3g", FULL size-8 "7g") are DISCOVERED from
#              the card's own ledger, so the case runs on A100 (…5gb/…20gb/…40gb) and H100
#              (…10gb/…40gb/…80gb) alike. Placement-exclusion + small-then-large sub-checks run only on a
#              single-MIG-card node (recorded SKIPPED otherwise, as their per-card math needs one card).
# Expected:    See Goal — each bullet is one PASS row in the results table.
# Cleanup:     Trap deletes every test Pod and restores the node's MIG mode to the state the case found
#              it in (re-enable if it started enabled; disable if it started disabled), refreshing the
#              Device Manager so the ledger realigns. Idempotent; runs on pass AND fail.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: MIG_NODE_SSH=<user@host> case-23.sh <NS>}"

PODPFX=gpustack-e2e-mig
DM_DS=gpustack-operator-device-manager-nvidia
MIGSSH_TIMEOUT="${MIG_SSH_TIMEOUT:-90}"

# ---------------------------------------------------------------------------------------------------
# Env gate — the node address the case must NOT guess. Without it the case cannot toggle MIG mode, so
# it stops here (exit 2, "input required") rather than proceeding blind. Supply MIG_NODE_SSH and re-run.
# ---------------------------------------------------------------------------------------------------
if [ -z "${MIG_NODE_SSH:-}" ]; then
  echo "== CASE 23 — INPUT REQUIRED (not run) =="
  echo
  echo "This case toggles a node's NVIDIA MIG mode over SSH, so it needs the node's SSH address."
  echo "It will not guess or auto-discover it. Provide it and re-run:"
  echo
  echo "    MIG_NODE_SSH=<user@host> bash .claude/skills/gpustack-operator-e2e/cases/case-23.sh ${NS}"
  echo
  echo "Optional overrides:"
  echo "    MIG_NODE_NAME=<k8s node>   # disambiguate when several nodes carry an NVIDIA card"
  echo "    MIG_NODE_SSH_OPTS='-i ...' # extra ssh options (identity file, port, jump host, …)"
  echo "    MIG_GPU_INDEX=<n>          # which card to toggle (default 0)"
  echo "    MIG_SSH_TIMEOUT=<secs>     # per-ssh timeout (default 90)"
  echo
  echo "The address stays out of this script and out of the repo — it is passed at run time only."
  exit 2
fi

GPU_INDEX="${MIG_GPU_INDEX:-0}"

# node_ssh <cmd...> — run a command on the node (non-interactive, bounded). The caller passes a full
# command line; sudo is the caller's responsibility (nvidia-smi mode switches need it).
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

# --- Confirm SSH reachability + sudo before mutating anything. ---
if ! node_ssh true >/dev/null 2>&1; then
  echo "== CASE 23 — SKIPPED =="
  echo "Cannot SSH to '${MIG_NODE_SSH}' (BatchMode, ${MIGSSH_TIMEOUT}s). Check the address / key / MIG_NODE_SSH_OPTS."
  exit 0
fi

# --- MIG-capability gate: the card must report a MIG mode field ('Enabled'/'Disabled'), not '[N/A]'. ---
INITIAL_MODE="$(node_ssh sudo nvidia-smi -i "$GPU_INDEX" --query-gpu=mig.mode.current --format=csv,noheader 2>/dev/null | tr -d '[:space:]')"
case "$INITIAL_MODE" in
  Enabled|Disabled) ;;
  *)
    echo "== CASE 23 — SKIPPED =="
    echo "Card ${GPU_INDEX} on ${MIG_NODE_SSH} does not report a MIG mode (got '${INITIAL_MODE:-<none>}') — not MIG-capable."
    echo "MIG needs an A100/A30/H100/H200/B-series (or newer) data-center card. Run this case on such a node."
    exit 0
  esac
echo "[case-23] node ${MIG_NODE_SSH} card ${GPU_INDEX}: MIG mode currently ${INITIAL_MODE}"

# --- The nvidia GPU node in the cluster + its accelerated pool. Correlate the SSH host to a k8s node so
#     the ledger/InstanceType assertions read the right object. Require a single nvidia-accelerated node
#     unless MIG_NODE_NAME disambiguates. ---
GPU_NODE="${MIG_NODE_NAME:-}"
if [ -z "$GPU_NODE" ]; then
  _nv=()
  while IFS= read -r _n; do [ -n "$_n" ] && _nv+=("$_n"); done < <(kubectl get devices -o json 2>/dev/null | python3 -c "
import json,sys
for d in json.load(sys.stdin).get('items',[]):
    for g in d.get('spec',{}).get('groups',[]):
        if g.get('manufacturer')=='nvidia' and g.get('accelerators'):
            print(d['metadata']['name']); break
" 2>/dev/null)
  if [ "${#_nv[@]}" -eq 0 ]; then
    echo "== CASE 23 — SKIPPED =="
    echo "No Devices object reports an nvidia accelerator group — the operator chain is not observing a GPU node."
    exit 0
  fi
  if [ "${#_nv[@]}" -gt 1 ]; then
    echo "[case-23] several nvidia GPU nodes (${_nv[*]}); set MIG_NODE_NAME=<the node behind ${MIG_NODE_SSH}> and re-run"
    exit 2
  fi
  GPU_NODE="${_nv[0]}"
fi
echo "[case-23] correlated k8s node: ${GPU_NODE}"

# Accelerated InstanceType for this node's nvidia group + its entrance LocalQueue + the manufacturer.
GROUPID=$(kubectl get devices "$GPU_NODE" -o json 2>/dev/null | python3 -c "
import json,sys
for g in json.load(sys.stdin).get('spec',{}).get('groups',[]):
    if g.get('manufacturer')=='nvidia' and g.get('accelerators'):
        print(g.get('id','')); break
" 2>/dev/null)
read -r IT LQ MANUF <<<"$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null | NODE_GID="$GROUPID" NODE_JSON="$(kubectl get node "$GPU_NODE" -o json 2>/dev/null)" python3 -c "
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
items=json.load(sys.stdin).get('items',[])
# The pool identity this node belongs to, not its rendered name — see the index's note on pool lookup.
for it in items:
    s=it.get('spec',{}); st=it.get('status',{})
    if s.get('acceleratable') and in_group(s) and backs(it) and st.get('entrance'):
        print(it['metadata']['name'], st['entrance'], s.get('manufacturer','nvidia')); sys.exit(0)
for it in items:
    s=it.get('spec',{}); st=it.get('status',{})
    if s.get('acceleratable') and st.get('entrance'):
        print(it['metadata']['name'], st['entrance'], s.get('manufacturer','nvidia')); sys.exit(0)
")"
[ -n "${IT:-}" ] && [ -n "${LQ:-}" ] || { echo "[case-23] no accelerated InstanceType with an entrance LocalQueue — chain not materialized"; exit 1; }
MANUF="${MANUF:-nvidia}"
# The two disjoint accelerator families. LOGICAL slicing (software, the vendor preload library) is
# served only by a card that is NOT in a hardware partitioning mode; PHYSICAL partitioning (the MIG
# hardware) only by a card that is. Enabling MIG moves the card from the first family to the second.
SLICED="${MANUF}.com/gpu.sliced"
PARTITIONED="${MANUF}.com/gpu.partitioned"
echo "[case-23] accelerated InstanceType ${IT} (entrance LocalQueue ${LQ}, group ${GROUPID:-?}, logical base ${SLICED}, partition base ${PARTITIONED})"

# The vendor runtimeClass mounts the driver libs the workload needs (nvidia-smi -L, and — on the logically sliced
# path — the LD_PRELOAD'd libvgpu.so, which exits 127 without it). Derive it from the manufacturer.
RUNTIMECLASS=""
if kubectl get runtimeclass.node.k8s.io "$MANUF" >/dev/null 2>&1; then RUNTIMECLASS="$MANUF"; fi
RTC_LINE=""; [ -n "$RUNTIMECLASS" ] && RTC_LINE="runtimeClassName: ${RUNTIMECLASS}"
echo "[case-23] test-pod runtimeClass: ${RUNTIMECLASS:-<none>}"

# ---------------------------------------------------------------------------------------------------
# Bookkeeping + helpers.
# ---------------------------------------------------------------------------------------------------
FAILS=0
ROWS=()
TESTPODS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

running()      { [ "$(kubectl -n default get pod "$1" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Running" ]; }
wait_running() { for _ in $(seq 1 40); do running "$1" && return 0; sleep 3; done; return 1; }

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

# held_reason <pod> — a DECISIVE signal that the Pod was refused: a terminal failure, an Unschedulable
# verdict, a device-plugin admission refusal, or an AdmissionCheck that declined it. Empty when none is
# present. The bare Kueue scheduling gate is deliberately NOT among these — see assert_held.
held_reason() {
  local p="$1" phase cond ev wl
  phase="$(kubectl -n default get pod "$p" -o jsonpath='{.status.phase}' 2>/dev/null)"
  [ "$phase" = Failed ] && { echo "Failed"; return; }
  cond="$(kubectl -n default get pod "$p" -o jsonpath='{.status.conditions[?(@.type=="PodScheduled")].reason}' 2>/dev/null)"
  [ "$cond" = Unschedulable ] && { echo "Unschedulable"; return; }
  ev="$(kubectl -n default get events --field-selector involvedObject.name="$p" -o jsonpath='{range .items[*]}{.reason} {end}' 2>/dev/null | tr ' ' '\n' | grep -iE 'UnexpectedAdmissionError|FailedScheduling' | head -1)"
  [ -n "$ev" ] && { echo "$ev"; return; }
  wl="$(workload_refusal "$p")"
  [ -n "$wl" ] && { echo "$wl"; return; }
  echo ""
}

# assert_held <pod> [polls] — confirm the Pod is HELD rather than merely slow; PASS text on success,
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

# node_gi_count — the number of live GPU instances the card actually holds (node ground truth).
# grep -c prints "0" AND exits non-zero on no match, so capture it (a bare `|| echo 0` would append a
# SECOND "0" under pipefail, yielding a two-line "0\n0" that never compares equal to "0").
node_gi_count() {
  local out
  out="$(node_ssh sudo nvidia-smi mig -lgi 2>/dev/null | grep -cE '^\|[[:space:]]+[0-9]+')"
  echo "${out:-0}"
}

# wait_card_idle — poll until the card holds no live GPU instances, bounded ~120s. A MIG mode switch
# needs an idle card (the disable prerequisite), so callers wait for the last workload's instance to
# reclaim before toggling the mode. Returns 0 when idle, 1 on timeout.
wait_card_idle() {
  for _ in $(seq 1 40); do
    [ "$(node_gi_count)" = 0 ] && return 0
    sleep 3
  done
  return 1
}

# exec_out <pod> <cmd>... — run a command in the Pod and echo its output, retried.
#
# A Pod reports Running slightly before its container is attachable, so an exec issued the instant
# wait_running returns can come back empty — or fail outright with `container not found` — on a Pod
# whose allocation is perfectly correct. That matters most in pod_mig_devices below: `grep -c` prints
# 0 even when the exec failed, so an attach race reads as "no MIG device inside the Pod" and fails a
# correct allocation.
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

# pod_mig_devices <pod> — count of MIG devices nvidia-smi -L reports inside the Pod (want exactly 1).
pod_mig_devices() {
  local out
  out="$(exec_out "$1" nvidia-smi -L | grep -c 'MIG')"
  echo "${out:-0}"
}

# node_key <resource> — the node's allocatable quantity for an extended resource (empty if absent).
node_key() { kubectl get node "$GPU_NODE" -o jsonpath="{.status.allocatable['${1//./\\.}']}" 2>/dev/null; }

# node_key_gone <resource> — true when a key is absent OR reads 0. Reconciler-owned counting keys are
# genuinely REMOVED when their family leaves the node, but a device-plugin pool key only zeroes out —
# the kubelet keeps the entry until it restarts — so "gone" must accept both for the pool keys.
node_key_gone() { local v; v="$(node_key "$1")"; [ -z "$v" ] || [ "$v" = 0 ]; }

# partition_profile_keys — the per-profile partition keys the node currently advertises, i.e. every
# "<base>.partitioned.<kind>-<profile>" (never ".partitioned.units", which is a counting key, not a
# profile). The kind segment is the manufacturer's own name for hardware partitioning and is read off
# the node rather than assumed, so the case does not hardcode a vendor's spelling of it.
partition_profile_keys() {
  kubectl get node "$GPU_NODE" -o json 2>/dev/null | PFX="${PARTITIONED}." python3 -c "
import json,os,sys
pfx=os.environ['PFX']
a=json.load(sys.stdin).get('status',{}).get('allocatable',{})
print(' '.join(k for k in a if k.startswith(pfx) and not k.endswith('.units')))
" 2>/dev/null
}

# profile_key <profile> — the advertised per-profile key whose profile segment is <profile>, or empty.
profile_key() {
  local want="$1" k
  for k in $(partition_profile_keys); do
    case "$k" in *-"$want") echo "$k"; return;; esac
  done
  echo ""
}

# refresh_dm — the Device Manager writes the capability at startup, and its detect loop compares only
# {manufacturer, id, unhealthy}, which a partitioning-mode toggle does not change — so a mode flip is
# picked up by RESTARTING the DaemonSet. Deleting the Devices object is deliberately NOT done: an
# existing group's capability is rewritten in place, and deleting it here would hide a regression of
# that. Waits for the rollout and for the object to be present.
# Both halves are measured, not assumed: on an 8-card node the flip alone left the object at the same
# generation and resourceVersion, unchanged again minutes later, while each restart moved it exactly
# one generation — logical to partitioned and back — with the object's uid and creationTimestamp never
# changing. So the restart is required, and it is also sufficient.
refresh_dm() {
  echo "[case-23]   refreshing Device Manager (rollout restart ${DM_DS}; the Devices object is kept)"
  kubectl -n "$NS" rollout restart "ds/${DM_DS}" >/dev/null 2>&1 || true
  kubectl -n "$NS" rollout status "ds/${DM_DS}" --timeout=180s >/dev/null 2>&1 || true
  for _ in $(seq 1 30); do kubectl get devices.worker.gpustack.ai "$GPU_NODE" >/dev/null 2>&1 && break; sleep 3; done
}

# set_mig_mode <0|1> — toggle the card's MIG mode over SSH and wait until it converges (or a reset pends).
# Returns 0 on convergence, 1 otherwise (caller records the failure with guidance).
# Converged here means only that the CARD reports the new mode. The toggle on its own is invisible to
# the operator, so a caller that stops at this point asserts against the capability the card had
# BEFORE the flip and reads a stale answer as a real one. Follow every successful toggle with
# refresh_dm.
set_mig_mode() {
  local want="$1" target; [ "$want" = 1 ] && target=Enabled || target=Disabled
  echo "[case-23]   nvidia-smi -i ${GPU_INDEX} -mig ${want} on ${MIG_NODE_SSH}"
  node_ssh sudo nvidia-smi -i "$GPU_INDEX" -mig "$want" >/dev/null 2>&1 || true
  for _ in $(seq 1 20); do
    local cur; cur="$(node_ssh sudo nvidia-smi -i "$GPU_INDEX" --query-gpu=mig.mode.current --format=csv,noheader 2>/dev/null | tr -d '[:space:]')"
    [ "$cur" = "$target" ] && return 0
    sleep 3
  done
  return 1
}

# card_profiles — DISCOVER the SMALL(size-1)/MID(size-4,3g)/FULL(size-8,7g) profiles + per-card counts +
# the partitioned-card count N and the group's TOTAL card count NTOT, from the cards' own capability.
# The physical-slice geometry is CAPABILITY, so it lives in Devices.spec (the per-card runtime ledger is
# in .status). NTOT is what tells the case whether the WHOLE pool moved family (N == NTOT) or only part
# of it, which decides whether the logical keys may legitimately still be present. Emits:
# SMALL SMALL_CNT MID MID_CNT FULL FULL_CNT N NTOT.
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

restore() {
  echo
  echo "[case-23] cleanup: deleting test Pods and restoring MIG mode to '${INITIAL_MODE}'"
  for p in "${TESTPODS[@]}"; do kubectl -n default delete pod "$p" --ignore-not-found --force --grace-period=0 >/dev/null 2>&1 || true; done
  local now; now="$(node_ssh sudo nvidia-smi -i "$GPU_INDEX" --query-gpu=mig.mode.current --format=csv,noheader 2>/dev/null | tr -d '[:space:]')"
  if [ "$now" != "$INITIAL_MODE" ]; then
    # A disable needs an idle card; wait for any just-deleted workload's instance to reclaim first.
    [ "$INITIAL_MODE" = Enabled ] || wait_card_idle || true
    [ "$INITIAL_MODE" = Enabled ] && set_mig_mode 1 || set_mig_mode 0
    refresh_dm
  fi
  node_ssh_close
}
trap restore EXIT

# mkpod <name> <extra-resource-lines> — a Pod on the GPU node, through the pool's LocalQueue.
mkpod() {
  local name="$1" reslines="$2"; TESTPODS+=("$name")
  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: { name: ${name}, namespace: default, labels: { kueue.x-k8s.io/queue-name: ${LQ} } }
spec:
  schedulerName: default-scheduler
  ${RTC_LINE}
  restartPolicy: Never
  nodeSelector: { kubernetes.io/hostname: ${GPU_NODE} }
  containers:
    - name: main
      image: ubuntu:24.04
      command: ["sleep", "86400"]
      resources:
        limits:
${reslines}
        requests:
${reslines}
EOF
}
delpod() { kubectl -n default delete pod "$1" --ignore-not-found --force --grace-period=0 >/dev/null 2>&1 || true; }

# ===================================================================================================
# Phase L — Logical slicing, only when the card started with MIG OFF.
# ===================================================================================================
if [ "$INITIAL_MODE" = Disabled ]; then
  echo
  echo "[case-23] === Phase L: logical slicing (MIG off) — 20% and 40% coexist ==="
  P20="${PODPFX}-logical20"; P40="${PODPFX}-logical40"
  mkpod "$P20" "          ${SLICED}: \"1\"
          ${SLICED}.memory-percentage: \"20\"
          ${SLICED}.cores-percentage: \"20\""
  mkpod "$P40" "          ${SLICED}: \"1\"
          ${SLICED}.memory-percentage: \"40\"
          ${SLICED}.cores-percentage: \"40\""
  l20=1; wait_running "$P20" || l20=0
  l40=1; wait_running "$P40" || l40=0
  if [ "$l20" = 1 ] && [ "$l40" = 1 ]; then
    sm20="$(exec_out "$P20" printenv CUDA_DEVICE_SM_LIMIT)"
    sm40="$(exec_out "$P40" printenv CUDA_DEVICE_SM_LIMIT)"
    record PASS "logical 20% + 40% coexist and are capped" "${P20}(SM=${sm20:-?}) + ${P40}(SM=${sm40:-?}) both Running on the logical-sliced card"
  else
    record FAIL "logical 20% + 40% coexist and are capped" "20% running=${l20} 40% running=${l40} — logical slicing broken on the MIG-off card"
  fi
  delpod "$P20"; delpod "$P40"
else
  record SKIP "logical-slice path (card started MIG-on)" "card ${GPU_INDEX} was already MIG-enabled; logical slicing is verified in reverse by Phase D (disable → logical returns)"
fi

# ===================================================================================================
# Phase E — Enable MIG (if not already) and re-detect.
# ===================================================================================================
echo
echo "[case-23] === Phase E: enable MIG + Device Manager re-detect ==="
CUR="$(node_ssh sudo nvidia-smi -i "$GPU_INDEX" --query-gpu=mig.mode.current --format=csv,noheader 2>/dev/null | tr -d '[:space:]')"
if [ "$CUR" != Enabled ]; then
  if ! set_mig_mode 1; then
    record FAIL "enable MIG mode" "nvidia-smi -mig 1 did not converge to Enabled (a pending GPU reset on Ampere, a busy card, or a loaded nvidia_drm blocks it) — see Prerequisites"
    echo; echo "== CASE 23 — cannot proceed without MIG enabled =="; exit 1
  fi
fi
refresh_dm
MIGKEY_FULL=""
# Wait for the card to move family: the node advertises at least one per-profile partition key.
for _ in $(seq 1 30); do
  keys="$(partition_profile_keys)"
  [ -n "$keys" ] && break
  sleep 4
done
read -r SMALL SMALL_CNT MID MID_CNT FULL FULL_CNT NCARD NTOT <<<"$(card_profiles)"
echo "[case-23] discovered profiles: SMALL=${SMALL}(${SMALL_CNT}/card) MID=${MID}(${MID_CNT}/card) FULL=${FULL}(${FULL_CNT}/card); partitioned cards N=${NCARD:-0} of ${NTOT:-0}"
echo "[case-23] advertised per-profile partition keys: ${keys:-<none>}"

# ===================================================================================================
# Phase P — Profiles carve correctly; node advertises per-profile keys; logical keys drop to 0.
# ===================================================================================================
echo
echo "[case-23] === Phase P: profile carve + capability keys ==="
if [ -n "$MID" ] && [ -n "$FULL" ] && [ "${MID_CNT:-0}" = 2 ] && [ "${FULL_CNT:-0}" = 1 ]; then
  record PASS "canonical profiles carved (names/counts)" "MID ${MID}=2/card, FULL ${FULL}=1/card, SMALL ${SMALL}=${SMALL_CNT}/card — matches the fixed placement layout"
else
  record FAIL "canonical profiles carved (names/counts)" "MID=${MID}(${MID_CNT}) FULL=${FULL}(${FULL_CNT}) SMALL=${SMALL}(${SMALL_CNT}) — expected a size-4 (2/card) and a size-8 (1/card) profile"
fi
MIGKEY_MID="$(profile_key "$MID")"
MIGKEY_FULL="$(profile_key "$FULL")"
mid_key_cap="$(node_key "$MIGKEY_MID")"
full_key_cap="$(node_key "$MIGKEY_FULL")"
if [ -n "$MIGKEY_MID" ] && [ -n "$MIGKEY_FULL" ] && [ -n "$mid_key_cap" ] && [ -n "$full_key_cap" ]; then
  record PASS "node advertises per-profile partition keys" "${MIGKEY_MID}=${mid_key_cap}, ${MIGKEY_FULL}=${full_key_cap}"
else
  record FAIL "node advertises per-profile partition keys" "MID key='${MIGKEY_MID:-<absent>}'=${mid_key_cap:-<absent>}, FULL key='${MIGKEY_FULL:-<absent>}'=${full_key_cap:-<absent>} — a partitioned card must advertise one key per profile it can host"
fi
# The partition family's counting key comes with them: a partitioned card is worth a whole card's units.
punits="$(node_key "${PARTITIONED}.units")"
[ -n "$punits" ] \
  && record PASS "node advertises the partition units key" "${PARTITIONED}.units=${punits}" \
  || record FAIL "node advertises the partition units key" "${PARTITIONED}.units absent — the partition family's credit input is missing"
# THE FAMILY SWAP. A partitioned card serves no logical slice, so it leaves the logical population
# entirely: the reconciler-owned logical counting keys are REMOVED (not retained, not zeroed) and the
# device-plugin's logical token pool drains to nothing. This can only be asserted when EVERY card of
# the group is partitioned — one unpartitioned sibling legitimately keeps the logical family alive.
logicalpct="$(node_key "${SLICED}.memory-percentage")"
units="$(node_key "${SLICED}.units")"
if [ "${NCARD:-0}" != "${NTOT:-0}" ] || [ "${NTOT:-0}" = 0 ]; then
  record SKIP "logical family leaves the MIG card" "only ${NCARD:-0} of ${NTOT:-0} card(s) partitioned — the unpartitioned sibling(s) keep the logical keys alive, so their absence is not assertable here"
elif [ -z "$logicalpct" ] && [ -z "$units" ] && node_key_gone "${SLICED}"; then
  record PASS "logical family leaves the MIG card" "${SLICED}.memory-percentage and ${SLICED}.units removed, ${SLICED} pool drained — the card now serves only ${PARTITIONED}"
else
  record FAIL "logical family leaves the MIG card" "${SLICED}.memory-percentage='${logicalpct:-<absent>}', ${SLICED}.units='${units:-<absent>}' (both want absent), ${SLICED}='$(node_key "${SLICED}")' (want absent or 0)"
fi

# ===================================================================================================
# Phase I — InstanceType numeric values reflect the MIG geometry (aggregate profiles + PT/SL views).
# ===================================================================================================
echo
echo "[case-23] === Phase I: InstanceType numeric values ==="
# PT (acceleratorPartitioned) is the partitioned cards' view and must be non-zero. SL
# (acceleratorSliced) is the unpartitioned cards' view, and must collapse to zero — but only when every
# card of the pool is partitioned; with an unpartitioned sibling the SL check is not applicable.
#
# The POOL decides both of those, not the node. A pool spanning several GPU nodes keeps its logical
# view alive on the sibling nodes' whole cards while one node's card is partitioned, and its aggregate
# profile counts sum over every partitioned card it has. NCARD/NTOT are the toggled node's own group —
# right for the node-key assertions above, wrong here — so count the pool's cards from every Devices
# object reporting its group.
read -r POOL_PART POOL_TOT <<<"$(kubectl get devices.v1alpha1.worker.gpustack.ai -o json 2>/dev/null | GRP="${GROUPID}" python3 -c "
import json, os, sys
grp = os.environ['GRP']
tot = part = 0
for d in json.load(sys.stdin).get('items', []):
    for g in d.get('spec', {}).get('groups', []) or []:
        if g.get('id') != grp:
            continue
        for a in g.get('accelerators', []) or []:
            tot += 1
            if ((a.get('status') or {}).get('physicalSliced') or {}).get('count', 0) > 0:
                part += 1
print(part, tot)
" 2>/dev/null)"
echo "[case-23]   pool-wide partitioned cards: ${POOL_PART:-0} of ${POOL_TOT:-0} (node-local was ${NCARD:-0} of ${NTOT:-0})"
it_ok="$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o json 2>/dev/null | MID="$MID" FULL="$FULL" NCARD="${POOL_PART:-1}" ALLMIG="$([ "${POOL_PART:-0}" = "${POOL_TOT:-0}" ] && [ "${POOL_TOT:-0}" != 0 ] && echo 1 || echo 0)" python3 -c "
import json,sys,os
it=json.load(sys.stdin); st=it.get('status',{})
det=st.get('detail',{}).get('slicedDetail',{}).get('physical',{})
profs={p['name']:p.get('count',0) for p in det.get('profiles',[]) or []}
mid=os.environ['MID']; full=os.environ['FULL']; n=int(os.environ['NCARD'] or 1)
allmig=os.environ['ALLMIG']=='1'
sliced=st.get('acceleratorSliced',{}); part=st.get('acceleratorPartitioned',{})
scap=int(sliced.get('capacity',0) or 0); pcap=int(part.get('capacity',0) or 0)
ok = profs.get(mid)==2*n and profs.get(full)==1*n and pcap>0 and (scap==0 or not allmig)
print('OK' if ok else 'BAD', 'mid=%s full=%s ptCap=%s slCap=%s allPartitioned=%s' % (profs.get(mid), profs.get(full), pcap, scap, allmig))
" 2>/dev/null)"
if [[ "$it_ok" == OK* ]]; then
  record PASS "InstanceType four-view reflects MIG geometry" "aggregate profiles ${MID}=$((2*${POOL_PART:-1})), ${FULL}=$((1*${POOL_PART:-1})), PT capacity > 0 over ${POOL_PART:-0}/${POOL_TOT:-0} partitioned card(s) [${it_ok#OK }]"
else
  record FAIL "InstanceType four-view reflects MIG geometry" "status detail mismatch [${it_ok:-<empty>}] — PT must carry the partition instances and SL must read 0 on an all-partitioned pool"
fi

# ===================================================================================================
# Phase X — Placement mutual exclusion (single-MIG-card only; per-card math needs one card).
# ===================================================================================================
echo
echo "[case-23] === Phase X: profile mutual exclusion ==="
if [ "${NCARD:-0}" = 1 ] && [ -n "$MIGKEY_MID" ] && [ -n "$MIGKEY_FULL" ]; then
  A="${PODPFX}-mid-a"; B="${PODPFX}-mid-b"; C="${PODPFX}-mid-c"; FU="${PODPFX}-full"
  # A size-4 MID admits and gets exactly one MIG device.
  mkpod "$A" "          ${PARTITIONED}: \"1\"
          ${MIGKEY_MID}: \"1\""
  if wait_running "$A"; then
    nd="$(pod_mig_devices "$A")"; gic="$(node_gi_count)"
    [ "$nd" = 1 ] && record PASS "MID ${MID} admits with one MIG device" "${A} Running, nvidia-smi -L=1 MIG device, node GI count=${gic}" \
      || record FAIL "MID ${MID} admits with one MIG device" "${A} Running but nvidia-smi -L MIG devices=${nd} (want 1)"
  else
    record FAIL "MID ${MID} admits with one MIG device" "${A} not Running — MIG allocate failed (see device-manager logs)"
  fi
  # With a size-4 placed, a size-8 FULL cannot fit the same (only) card → held.
  mkpod "$FU" "          ${PARTITIONED}: \"1\"
          ${MIGKEY_FULL}: \"1\""
  hr="$(assert_held "$FU")"
  [ -n "$hr" ] && record PASS "FULL ${FULL} held while MID occupies the card" "${FU} held [${hr}] — a size-8 profile cannot co-exist with a size-4 (mutual exclusion)" \
    || record FAIL "FULL ${FULL} held while MID occupies the card" "${FU} not held with no concrete reason — placement exclusion may be violated"
  delpod "$FU"
  # A second MID fills the card (2 per card); a third is held.
  mkpod "$B" "          ${PARTITIONED}: \"1\"
          ${MIGKEY_MID}: \"1\""
  if wait_running "$B"; then
    record PASS "two MID ${MID} fill one card" "${A} + ${B} both Running (2× size-4 = full card), node GI count=$(node_gi_count)"
  else
    record FAIL "two MID ${MID} fill one card" "${B} not Running — the card should host two size-4 instances"
  fi
  mkpod "$C" "          ${PARTITIONED}: \"1\"
          ${MIGKEY_MID}: \"1\""
  hr="$(assert_held "$C")"
  [ -n "$hr" ] && record PASS "third MID ${MID} held (card full)" "${C} held [${hr}] — no third size-4 slot" \
    || record FAIL "third MID ${MID} held (card full)" "${C} not held — the card was over-partitioned"
  delpod "$C"; delpod "$B"; delpod "$A"
else
  record SKIP "profile mutual exclusion (needs a single partitioned card)" "partitioned cards N=${NCARD:-0}, MID key='${MIGKEY_MID:-<none>}', FULL key='${MIGKEY_FULL:-<none>}'; per-card placement exclusion is asserted only on a one-card node"
fi

# ===================================================================================================
# Phase R — Reuse: delete a MIG Pod, immediately re-request the same profile → it admits again.
# ===================================================================================================
echo
echo "[case-23] === Phase R: reuse a freed instance ==="
if [ -n "$MIGKEY_MID" ]; then
  R1="${PODPFX}-reuse-1"; R2="${PODPFX}-reuse-2"
  # Phase X force-deleted its Pods but the instances behind them are destroyed on the reclaim
  # debounce, not on Pod deletion. Requesting R1 before that lands asks for a slot on a card that is
  # still full: the AdmissionCheck refuses it and the retry rides Kueue's backoff, which can outlast
  # this phase's own wait. Wait for the card to go idle first — the same precondition Phases S and D
  # already take.
  wait_card_idle || echo "[case-23]   warning: card still reports live instances entering the reuse phase"
  mkpod "$R1" "          ${PARTITIONED}: \"1\"
          ${MIGKEY_MID}: \"1\""
  if wait_running "$R1"; then
    uuid1="$(exec_out "$R1" bash -c 'echo $NVIDIA_VISIBLE_DEVICES')"
    delpod "$R1"
    mkpod "$R2" "          ${PARTITIONED}: \"1\"
          ${MIGKEY_MID}: \"1\""
    if wait_running "$R2"; then
      uuid2="$(exec_out "$R2" bash -c 'echo $NVIDIA_VISIBLE_DEVICES')"
      record PASS "freed instance reused by a fresh request" "${R2} Running on the freed slot (MIG dev before=${uuid1:-?} after=${uuid2:-?})"
    else
      record FAIL "freed instance reused by a fresh request" "${R2} not Running after ${R1} freed the slot"
    fi
    delpod "$R2"
  else
    record FAIL "freed instance reused by a fresh request" "${R1} not Running — could not set up the reuse precondition"
    delpod "$R1"
  fi
else
  record SKIP "reuse a freed instance" "no size-4 MID profile key advertised"
fi

# ===================================================================================================
# Phase G — Reclaim: delete a MIG Pod, stay idle → the operator destroys the instance within the debounce.
# ===================================================================================================
echo
echo "[case-23] === Phase G: reclaim an idle instance ==="
if [ -n "$MIGKEY_MID" ]; then
  G1="${PODPFX}-reclaim"
  mkpod "$G1" "          ${PARTITIONED}: \"1\"
          ${MIGKEY_MID}: \"1\""
  if wait_running "$G1"; then
    before="$(node_gi_count)"
    delpod "$G1"
    reclaimed=0; waited=0
    for _ in $(seq 1 40); do   # up to ~120s for the reclaim debounce
      gic="$(node_gi_count)"
      if [ "${gic:-1}" = 0 ]; then reclaimed=1; break; fi
      sleep 3; waited=$((waited + 3))
    done
    [ "$reclaimed" = 1 ] && record PASS "idle instance reclaimed after debounce" "node GI count ${before}→0 within ~${waited}s of the Pod exiting (no re-request)" \
      || record FAIL "idle instance reclaimed after debounce" "node GI count still $(node_gi_count) ~${waited}s after the Pod exited — the instance was not reclaimed"
  else
    record FAIL "idle instance reclaimed after debounce" "${G1} not Running — could not set up the reclaim precondition"
    delpod "$G1"
  fi
else
  record SKIP "idle instance reclaimed after debounce" "no size-4 MID profile key advertised"
fi

# ===================================================================================================
# Phase S — Small-then-large: fill the card with SMALL instances, free them, then a FULL fits again.
# ===================================================================================================
echo
echo "[case-23] === Phase S: small profiles then a whole-card profile ==="
MIGKEY_SMALL="$(profile_key "$SMALL")"
if [ "${NCARD:-0}" = 1 ] && [ -n "$MIGKEY_SMALL" ] && [ -n "$MIGKEY_FULL" ] && [ "${SMALL_CNT:-0}" -ge 1 ]; then
  smalls=(); ok_small=1
  for i in $(seq 1 "$SMALL_CNT"); do
    s="${PODPFX}-small-${i}"; smalls+=("$s")
    mkpod "$s" "          ${PARTITIONED}: \"1\"
          ${MIGKEY_SMALL}: \"1\""
    wait_running "$s" || ok_small=0
  done
  [ "$ok_small" = 1 ] && record PASS "SMALL ${SMALL} fills the card" "${SMALL_CNT}× ${SMALL} all Running, node GI count=$(node_gi_count)" \
    || record FAIL "SMALL ${SMALL} fills the card" "not all ${SMALL_CNT}× ${SMALL} reached Running"
  for s in "${smalls[@]}"; do delpod "$s"; done
  # Wait for the small instances to reclaim so the whole card is free again.
  for _ in $(seq 1 40); do [ "$(node_gi_count)" = 0 ] && break; sleep 3; done
  BIG="${PODPFX}-big"
  mkpod "$BIG" "          ${PARTITIONED}: \"1\"
          ${MIGKEY_FULL}: \"1\""
  if wait_running "$BIG"; then
    nd="$(pod_mig_devices "$BIG")"
    record PASS "whole-card FULL ${FULL} fits after small freed" "${BIG} Running, nvidia-smi -L=${nd} MIG device — the card recomposed to one 7g instance"
  else
    record FAIL "whole-card FULL ${FULL} fits after small freed" "${BIG} not Running — the card did not free back to a whole-card profile"
  fi
  delpod "$BIG"
else
  record SKIP "small-then-large recompose (needs a single partitioned card)" "partitioned cards N=${NCARD:-0} / profile keys SMALL='${MIGKEY_SMALL:-<none>}' FULL='${MIGKEY_FULL:-<none>}'"
fi

# ===================================================================================================
# Phase D — Disable MIG and re-detect → the card returns to its logical-slice capability.
# ===================================================================================================
echo
echo "[case-23] === Phase D: disable MIG → logical-slice capability returns ==="
# A mode switch needs an idle card — wait for the whole-card FULL from Phase S (just deleted) to
# reclaim before disabling, else nvidia-smi -mig 0 cannot converge (the disable prerequisite).
wait_card_idle || echo "[case-23]   warning: card still reports live instances before disable"
if set_mig_mode 0; then
  refresh_dm
  logicalpct=""; partleft="x"; punits_left="x"
  for _ in $(seq 1 30); do
    logicalpct="$(node_key "${SLICED}.memory-percentage")"
    partleft="$(partition_profile_keys | wc -w | tr -d '[:space:]')"
    punits_left="$(node_key "${PARTITIONED}.units")"
    [ -n "$logicalpct" ] && [ "${partleft:-1}" = 0 ] && [ -z "$punits_left" ] && break
    sleep 4
  done
  if [ -n "$logicalpct" ] && [ "${partleft:-1}" = 0 ] && [ -z "$punits_left" ]; then
    record PASS "logical family returns after disable" "${SLICED}.memory-percentage=${logicalpct} back; every per-profile partition key and ${PARTITIONED}.units gone — the card moved back to the logical population"
  else
    record FAIL "logical family returns after disable" "${SLICED}.memory-percentage='${logicalpct:-<absent>}', per-profile partition keys left=${partleft}, ${PARTITIONED}.units='${punits_left:-<absent>}' — the card did not return to logical slicing"
  fi
else
  record FAIL "disable MIG mode" "nvidia-smi -mig 0 did not converge to Disabled"
fi

# ===================================================================================================
# Results.
# ===================================================================================================
echo
echo "== CASE 23 — NVIDIA MIG dynamic-allocation lifecycle =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). The operator observes MIG geometry and dynamically creates/destroys the"
  echo "GPU/compute instances backing scheduled Pods. Diagnose:"
  echo "  kubectl get devices ${GPU_NODE} -o yaml   # per-card physicalSliced profiles + RemainingProfiles"
  echo "  kubectl get node ${GPU_NODE} -o json | jq '.status.allocatable | with_entries(select(.key|test(\"sliced|partitioned\")))'"
  echo "  kubectl -n ${NS} logs ds/${DM_DS} --tail=200"
  echo "  ${MIG_NODE_SSH} sudo nvidia-smi mig -lgi   # the live GPU instances on the card"
  exit 1
fi
echo "CASE 23 PASS"
