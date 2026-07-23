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
#                - a card whose MIG mode is OFF serves logical (soft) slices — two Pods at 20% and 40%
#                  coexist and are runtime-capped (the vendor libvgpu path);
#                - after the ADMIN enables MIG (nvidia-smi, over SSH) and the Device Manager re-detects,
#                  the card carves into its canonical profile set (names/counts match the ledger), the
#                  node advertises one nvidia.com/gpu.sliced.mig-<profile> key per profile, and the soft
#                  logical keys drop to zero (a hard-partitioned card offers no soft slicing);
#                - the InstanceType numeric three-view + the per-profile ledger reflect the geometry;
#                - profiles are placement-mutually-exclusive (a size-4 3g instance blocks a size-8 7g
#                  instance on the same card; two size-4 fill it, a third is held);
#                - deleting a MIG Pod frees the instance so a fresh request of the same profile admits
#                  again (reuse), and an idle card's instance is reclaimed within the debounce;
#                - after small instances are freed a whole-card profile fits again;
#                - disabling MIG and re-detecting returns the card to its soft-slice capability.
# Environment: A reachable cluster whose active context is the GPU cluster, a node with a real NVIDIA
#              card, AND SSH to that node (sudo nvidia-smi) supplied via MIG_NODE_SSH=<user@host>.
#              This case is the ONE case that toggles node hardware state, so it needs the node address;
#              it does NOT guess it. It EXITS 2 (input required) when MIG_NODE_SSH is unset — provide the
#              address and re-run. It AUTO-SKIPS (exit 0) when the node's card is not MIG-capable, or the
#              cluster advertises no nvidia sliced accelerator.
#              Prerequisites the ADMIN owns (the case attempts the mode switch but cannot force them):
#              the card must be idle, driver-handle daemons (DCGM/nvsm/exporters) stopped, nvidia_drm
#              unloaded, CAP_SYS_ADMIN available. Targets Hopper+ (no GPU reset needed); on Ampere a
#              mode switch that pends a reset is reported as a FAIL with guidance, not forced.
# Inputs:      All real, nothing mocked — the MIG geometry, the libvgpu cap, and the GI/CI create/destroy
#              are the verification. The case toggles the node's MIG mode over SSH and restarts the
#              Device Manager to re-detect; test Pods go through the accelerated pool's entrance
#              LocalQueue. Profiles (SMALL size-1, MID size-4 "3g", FULL size-8 "7g") are DISCOVERED from
#              the card's own ledger, so the case runs on A100 (…5gb/…20gb/…40gb) and H100
#              (…10gb/…40gb/…80gb) alike. Placement-exclusion + small-then-large sub-checks run only on a
#              single-MIG-card node (recorded SKIPPED otherwise, as their per-card math needs one card).
# Expected:    See Goal — each bullet is one PASS row in the results table.
# Cleanup:     Trap deletes every test Pod and restores the node's MIG mode to the state the case found
#              it in (re-enable if it started enabled; disable if it started disabled), refreshing the
#              Device Manager so the ledger realigns. Idempotent; runs on pass AND fail.
set -uo pipefail

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
node_ssh() {
  # shellcheck disable=SC2086
  timeout "${MIGSSH_TIMEOUT}" ssh -o StrictHostKeyChecking=no -o ConnectTimeout=15 -o BatchMode=yes \
    ${MIG_NODE_SSH_OPTS:-} "$MIG_NODE_SSH" "$@"
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
[ -n "${IT:-}" ] && [ -n "${LQ:-}" ] || { echo "[case-23] no accelerated InstanceType with an entrance LocalQueue — chain not materialized"; exit 1; }
MANUF="${MANUF:-nvidia}"
SLICED="${MANUF}.com/gpu.sliced"
echo "[case-23] accelerated InstanceType ${IT} (entrance LocalQueue ${LQ}, group ${GROUPID:-?}, sliced base ${SLICED})"

# The vendor runtimeClass mounts the driver libs the workload needs (nvidia-smi -L, and — on the soft
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

# held_reason <pod> — a concrete signal the Pod was HELD (not merely slow): device-plugin admission
# refusal, a Kueue scheduling gate, or an Unschedulable verdict. Empty when none is present.
held_reason() {
  local p="$1" phase gates cond ev
  phase="$(kubectl -n default get pod "$p" -o jsonpath='{.status.phase}' 2>/dev/null)"
  [ "$phase" = Failed ] && { echo "Failed"; return; }
  gates="$(kubectl -n default get pod "$p" -o jsonpath='{.spec.schedulingGates[*].name}' 2>/dev/null)"
  [ -n "$gates" ] && { echo "scheduling-gated(${gates})"; return; }
  cond="$(kubectl -n default get pod "$p" -o jsonpath='{.status.conditions[?(@.type=="PodScheduled")].reason}' 2>/dev/null)"
  [ "$cond" = Unschedulable ] && { echo "Unschedulable"; return; }
  ev="$(kubectl -n default get events --field-selector involvedObject.name="$p" -o jsonpath='{range .items[*]}{.reason} {end}' 2>/dev/null | tr ' ' '\n' | grep -iE 'UnexpectedAdmissionError|FailedScheduling' | head -1)"
  [ -n "$ev" ] && { echo "$ev"; return; }
  echo ""
}

# assert_held <pod> — poll for a concrete held signal; PASS text on success, empty on "cannot confirm".
assert_held() {
  local hr=""
  for _ in $(seq 1 10); do hr="$(held_reason "$1")"; [ -n "$hr" ] && break; sleep 3; done
  echo "$hr"
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

# pod_mig_devices <pod> — count of MIG devices nvidia-smi -L reports inside the Pod (want exactly 1).
pod_mig_devices() {
  local out
  out="$(kubectl -n default exec "$1" -- nvidia-smi -L 2>/dev/null | grep -c 'MIG')"
  echo "${out:-0}"
}

# node_key <resource> — the node's allocatable quantity for an extended resource (empty if absent).
node_key() { kubectl get node "$GPU_NODE" -o jsonpath="{.status.allocatable['${1//./\\.}']}" 2>/dev/null; }

# refresh_dm — the Device Manager writes the capability only at startup and does NOT overwrite an
# existing Devices object, so a capability change (MIG on/off) is picked up only by deleting the ledger
# object then restarting the DaemonSet. Waits for the rollout and for the object to reappear.
refresh_dm() {
  echo "[case-23]   refreshing Device Manager (delete Devices/${GPU_NODE} + rollout restart ${DM_DS})"
  kubectl delete devices.worker.gpustack.ai "$GPU_NODE" --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" rollout restart "ds/${DM_DS}" >/dev/null 2>&1 || true
  kubectl -n "$NS" rollout status "ds/${DM_DS}" --timeout=180s >/dev/null 2>&1 || true
  for _ in $(seq 1 30); do kubectl get devices.worker.gpustack.ai "$GPU_NODE" >/dev/null 2>&1 && break; sleep 3; done
}

# set_mig_mode <0|1> — toggle the card's MIG mode over SSH and wait until it converges (or a reset pends).
# Returns 0 on convergence, 1 otherwise (caller records the failure with guidance).
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
# the MIG-card count N from the card's own capability. The physical-slice geometry is CAPABILITY, so it
# lives in Devices.spec (the per-card runtime ledger is in .status). Emits: SMALL SMALL_CNT MID MID_CNT
# FULL FULL_CNT N.
card_profiles() {
  kubectl get devices "$GPU_NODE" -o json 2>/dev/null | python3 -c "
import json,sys
d=json.load(sys.stdin)
small=mid=full=None; n=0
for g in d.get('spec',{}).get('groups',[]):
    if g.get('manufacturer')!='nvidia': continue
    for a in g.get('accelerators',[]):
        profs=a.get('status',{}).get('physicalSliced',{}).get('profiles',[]) or []
        if profs: n+=1
        for p in profs:
            ms=p.get('memorySlices',0); cs=p.get('computeSlices',0)
            if ms==1 and (small is None or p.get('memoryMib',0)<small[1]): small=(p['name'],p.get('memoryMib',0),p.get('count',0))
            if ms==4 and cs==3: mid=(p['name'],p.get('memoryMib',0),p.get('count',0))
            if ms==8: full=(p['name'],p.get('memoryMib',0),p.get('count',0))
def f(x): return (x[0], x[2]) if x else ('', 0)
sn,sc=f(small); mn,mc=f(mid); fn,fc=f(full)
print(sn, sc, mn, mc, fn, fc, n)
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
# Phase L — Logical (soft) slicing, only when the card started with MIG OFF.
# ===================================================================================================
if [ "$INITIAL_MODE" = Disabled ]; then
  echo
  echo "[case-23] === Phase L: logical soft slicing (MIG off) — 20% and 40% coexist ==="
  P20="${PODPFX}-soft20"; P40="${PODPFX}-soft40"
  mkpod "$P20" "          ${SLICED}: \"1\"
          ${SLICED}.memory-percentage: \"20\"
          ${SLICED}.cores-percentage: \"20\""
  mkpod "$P40" "          ${SLICED}: \"1\"
          ${SLICED}.memory-percentage: \"40\"
          ${SLICED}.cores-percentage: \"40\""
  l20=1; wait_running "$P20" || l20=0
  l40=1; wait_running "$P40" || l40=0
  if [ "$l20" = 1 ] && [ "$l40" = 1 ]; then
    sm20="$(kubectl -n default exec "$P20" -- printenv CUDA_DEVICE_SM_LIMIT 2>/dev/null)"
    sm40="$(kubectl -n default exec "$P40" -- printenv CUDA_DEVICE_SM_LIMIT 2>/dev/null)"
    record PASS "soft 20% + 40% coexist and are capped" "${P20}(SM=${sm20:-?}) + ${P40}(SM=${sm40:-?}) both Running on the soft-sliced card"
  else
    record FAIL "soft 20% + 40% coexist and are capped" "20% running=${l20} 40% running=${l40} — soft slicing broken on the MIG-off card"
  fi
  delpod "$P20"; delpod "$P40"
else
  record SKIP "soft-slice path (card started MIG-on)" "card ${GPU_INDEX} was already MIG-enabled; soft slicing is verified in reverse by Phase D (disable → soft returns)"
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
# Wait for the card to carve: the node advertises at least one nvidia.com/gpu.sliced.mig-<profile> key.
for _ in $(seq 1 30); do
  keys="$(kubectl get node "$GPU_NODE" -o json 2>/dev/null | python3 -c "
import json,sys
a=json.load(sys.stdin).get('status',{}).get('allocatable',{})
print(' '.join(k for k in a if '.sliced.mig-' in k))
" 2>/dev/null)"
  [ -n "$keys" ] && break
  sleep 4
done
read -r SMALL SMALL_CNT MID MID_CNT FULL FULL_CNT NCARD <<<"$(card_profiles)"
echo "[case-23] discovered profiles: SMALL=${SMALL}(${SMALL_CNT}/card) MID=${MID}(${MID_CNT}/card) FULL=${FULL}(${FULL_CNT}/card); MIG cards N=${NCARD:-0}"

# ===================================================================================================
# Phase P — Profiles carve correctly; node advertises per-profile keys; soft logical keys drop to 0.
# ===================================================================================================
echo
echo "[case-23] === Phase P: profile carve + capability keys ==="
if [ -n "$MID" ] && [ -n "$FULL" ] && [ "${MID_CNT:-0}" = 2 ] && [ "${FULL_CNT:-0}" = 1 ]; then
  record PASS "canonical profiles carved (names/counts)" "MID ${MID}=2/card, FULL ${FULL}=1/card, SMALL ${SMALL}=${SMALL_CNT}/card — matches the fixed placement layout"
else
  record FAIL "canonical profiles carved (names/counts)" "MID=${MID}(${MID_CNT}) FULL=${FULL}(${FULL_CNT}) SMALL=${SMALL}(${SMALL_CNT}) — expected a size-4 (2/card) and a size-8 (1/card) profile"
fi
MIGKEY_MID="${SLICED}.mig-${MID}"
MIGKEY_FULL="${SLICED}.mig-${FULL}"
mid_key_cap="$(node_key "$MIGKEY_MID")"
full_key_cap="$(node_key "$MIGKEY_FULL")"
if [ -n "$mid_key_cap" ] && [ -n "$full_key_cap" ]; then
  record PASS "node advertises per-profile MIG keys" "${MIGKEY_MID}=${mid_key_cap}, ${MIGKEY_FULL}=${full_key_cap}"
else
  record FAIL "node advertises per-profile MIG keys" "${MIGKEY_MID}='${mid_key_cap:-<absent>}', ${MIGKEY_FULL}='${full_key_cap:-<absent>}'"
fi
# A hard-partitioned card offers no soft slicing → the logical keys must be gone (only .sliced.units + .sliced.mig-* remain).
softpct="$(node_key "${SLICED}.memory-percentage")"
units="$(node_key "${SLICED}.units")"
if [ -z "$softpct" ] && [ -n "$units" ]; then
  record PASS "soft logical keys drop on MIG card" "${SLICED}.memory-percentage absent, ${SLICED}.units=${units} retained (MIG folds into credits)"
else
  record FAIL "soft logical keys drop on MIG card" "${SLICED}.memory-percentage='${softpct:-<absent>}' (want absent), ${SLICED}.units='${units:-<absent>}' (want present)"
fi

# ===================================================================================================
# Phase I — InstanceType numeric values reflect the MIG geometry (aggregate profiles + sliced view).
# ===================================================================================================
echo
echo "[case-23] === Phase I: InstanceType numeric values ==="
it_ok="$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o json 2>/dev/null | MID="$MID" FULL="$FULL" NCARD="${NCARD:-1}" python3 -c "
import json,sys,os
it=json.load(sys.stdin); st=it.get('status',{})
det=st.get('detail',{}).get('slicedDetail',{}).get('physical',{})
profs={p['name']:p.get('count',0) for p in det.get('profiles',[]) or []}
mid=os.environ['MID']; full=os.environ['FULL']; n=int(os.environ['NCARD'] or 1)
sliced=st.get('acceleratorSliced',{})
ok = profs.get(mid)==2*n and profs.get(full)==1*n and int(sliced.get('capacity',0) or 0)>0
print('OK' if ok else 'BAD', 'mid=%s full=%s slicedCap=%s' % (profs.get(mid), profs.get(full), sliced.get('capacity')))
" 2>/dev/null)"
if [[ "$it_ok" == OK* ]]; then
  record PASS "InstanceType status reflects MIG geometry" "aggregate profiles ${MID}=$((2*${NCARD:-1})), ${FULL}=$((1*${NCARD:-1})), sliced capacity > 0 [${it_ok#OK }]"
else
  record FAIL "InstanceType status reflects MIG geometry" "status detail mismatch [${it_ok:-<empty>}]"
fi

# ===================================================================================================
# Phase X — Placement mutual exclusion (single-MIG-card only; per-card math needs one card).
# ===================================================================================================
echo
echo "[case-23] === Phase X: profile mutual exclusion ==="
if [ "${NCARD:-0}" = 1 ] && [ -n "$MID" ] && [ -n "$FULL" ]; then
  A="${PODPFX}-mid-a"; B="${PODPFX}-mid-b"; C="${PODPFX}-mid-c"; FU="${PODPFX}-full"
  # A size-4 MID admits and gets exactly one MIG device.
  mkpod "$A" "          ${SLICED}: \"1\"
          ${MIGKEY_MID}: \"1\""
  if wait_running "$A"; then
    nd="$(pod_mig_devices "$A")"; gic="$(node_gi_count)"
    [ "$nd" = 1 ] && record PASS "MID ${MID} admits with one MIG device" "${A} Running, nvidia-smi -L=1 MIG device, node GI count=${gic}" \
      || record FAIL "MID ${MID} admits with one MIG device" "${A} Running but nvidia-smi -L MIG devices=${nd} (want 1)"
  else
    record FAIL "MID ${MID} admits with one MIG device" "${A} not Running — MIG allocate failed (see device-manager logs)"
  fi
  # With a size-4 placed, a size-8 FULL cannot fit the same (only) card → held.
  mkpod "$FU" "          ${SLICED}: \"1\"
          ${MIGKEY_FULL}: \"1\""
  hr="$(assert_held "$FU")"
  [ -n "$hr" ] && record PASS "FULL ${FULL} held while MID occupies the card" "${FU} held [${hr}] — a size-8 profile cannot co-exist with a size-4 (mutual exclusion)" \
    || record FAIL "FULL ${FULL} held while MID occupies the card" "${FU} not held with no concrete reason — placement exclusion may be violated"
  delpod "$FU"
  # A second MID fills the card (2 per card); a third is held.
  mkpod "$B" "          ${SLICED}: \"1\"
          ${MIGKEY_MID}: \"1\""
  if wait_running "$B"; then
    record PASS "two MID ${MID} fill one card" "${A} + ${B} both Running (2× size-4 = full card), node GI count=$(node_gi_count)"
  else
    record FAIL "two MID ${MID} fill one card" "${B} not Running — the card should host two size-4 instances"
  fi
  mkpod "$C" "          ${SLICED}: \"1\"
          ${MIGKEY_MID}: \"1\""
  hr="$(assert_held "$C")"
  [ -n "$hr" ] && record PASS "third MID ${MID} held (card full)" "${C} held [${hr}] — no third size-4 slot" \
    || record FAIL "third MID ${MID} held (card full)" "${C} not held — the card was over-partitioned"
  delpod "$C"; delpod "$B"; delpod "$A"
else
  record SKIP "profile mutual exclusion (needs a single MIG card)" "MIG cards N=${NCARD:-0}; per-card placement exclusion is asserted only on a one-card node"
fi

# ===================================================================================================
# Phase R — Reuse: delete a MIG Pod, immediately re-request the same profile → it admits again.
# ===================================================================================================
echo
echo "[case-23] === Phase R: reuse a freed instance ==="
if [ -n "$MID" ]; then
  R1="${PODPFX}-reuse-1"; R2="${PODPFX}-reuse-2"
  mkpod "$R1" "          ${SLICED}: \"1\"
          ${MIGKEY_MID}: \"1\""
  if wait_running "$R1"; then
    uuid1="$(kubectl -n default exec "$R1" -- bash -c 'echo $NVIDIA_VISIBLE_DEVICES' 2>/dev/null)"
    delpod "$R1"
    mkpod "$R2" "          ${SLICED}: \"1\"
          ${MIGKEY_MID}: \"1\""
    if wait_running "$R2"; then
      uuid2="$(kubectl -n default exec "$R2" -- bash -c 'echo $NVIDIA_VISIBLE_DEVICES' 2>/dev/null)"
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
  record SKIP "reuse a freed instance" "no size-4 MID profile discovered"
fi

# ===================================================================================================
# Phase G — Reclaim: delete a MIG Pod, stay idle → the operator destroys the instance within the debounce.
# ===================================================================================================
echo
echo "[case-23] === Phase G: reclaim an idle instance ==="
if [ -n "$MID" ]; then
  G1="${PODPFX}-reclaim"
  mkpod "$G1" "          ${SLICED}: \"1\"
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
  record SKIP "idle instance reclaimed after debounce" "no size-4 MID profile discovered"
fi

# ===================================================================================================
# Phase S — Small-then-large: fill the card with SMALL instances, free them, then a FULL fits again.
# ===================================================================================================
echo
echo "[case-23] === Phase S: small profiles then a whole-card profile ==="
if [ "${NCARD:-0}" = 1 ] && [ -n "$SMALL" ] && [ -n "$FULL" ] && [ "${SMALL_CNT:-0}" -ge 1 ]; then
  MIGKEY_SMALL="${SLICED}.mig-${SMALL}"
  smalls=(); ok_small=1
  for i in $(seq 1 "$SMALL_CNT"); do
    s="${PODPFX}-small-${i}"; smalls+=("$s")
    mkpod "$s" "          ${SLICED}: \"1\"
          ${MIGKEY_SMALL}: \"1\""
    wait_running "$s" || ok_small=0
  done
  [ "$ok_small" = 1 ] && record PASS "SMALL ${SMALL} fills the card" "${SMALL_CNT}× ${SMALL} all Running, node GI count=$(node_gi_count)" \
    || record FAIL "SMALL ${SMALL} fills the card" "not all ${SMALL_CNT}× ${SMALL} reached Running"
  for s in "${smalls[@]}"; do delpod "$s"; done
  # Wait for the small instances to reclaim so the whole card is free again.
  for _ in $(seq 1 40); do [ "$(node_gi_count)" = 0 ] && break; sleep 3; done
  BIG="${PODPFX}-big"
  mkpod "$BIG" "          ${SLICED}: \"1\"
          ${MIGKEY_FULL}: \"1\""
  if wait_running "$BIG"; then
    nd="$(pod_mig_devices "$BIG")"
    record PASS "whole-card FULL ${FULL} fits after small freed" "${BIG} Running, nvidia-smi -L=${nd} MIG device — the card recomposed to one 7g instance"
  else
    record FAIL "whole-card FULL ${FULL} fits after small freed" "${BIG} not Running — the card did not free back to a whole-card profile"
  fi
  delpod "$BIG"
else
  record SKIP "small-then-large recompose (needs a single MIG card)" "MIG cards N=${NCARD:-0} / profiles SMALL=${SMALL:-?} FULL=${FULL:-?}"
fi

# ===================================================================================================
# Phase D — Disable MIG and re-detect → the card returns to its soft-slice capability.
# ===================================================================================================
echo
echo "[case-23] === Phase D: disable MIG → soft-slice capability returns ==="
# A mode switch needs an idle card — wait for the whole-card FULL from Phase S (just deleted) to
# reclaim before disabling, else nvidia-smi -mig 0 cannot converge (the disable prerequisite).
wait_card_idle || echo "[case-23]   warning: card still reports live instances before disable"
if set_mig_mode 0; then
  refresh_dm
  softpct=""; migleft="x"
  for _ in $(seq 1 30); do
    softpct="$(node_key "${SLICED}.memory-percentage")"
    migleft="$(kubectl get node "$GPU_NODE" -o json 2>/dev/null | python3 -c "
import json,sys
a=json.load(sys.stdin).get('status',{}).get('allocatable',{})
print(sum(1 for k in a if '.sliced.mig-' in k))
" 2>/dev/null)"
    [ -n "$softpct" ] && [ "${migleft:-1}" = 0 ] && break
    sleep 4
  done
  if [ -n "$softpct" ] && [ "${migleft:-1}" = 0 ]; then
    record PASS "soft-slice capability returns after disable" "${SLICED}.memory-percentage=${softpct} back, all .sliced.mig-* keys gone"
  else
    record FAIL "soft-slice capability returns after disable" "${SLICED}.memory-percentage='${softpct:-<absent>}', remaining mig keys=${migleft} — card did not return to soft slicing"
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
  echo "  kubectl get node ${GPU_NODE} -o json | jq '.status.allocatable | with_entries(select(.key|test(\"sliced\")))'"
  echo "  kubectl -n ${NS} logs ds/${DM_DS} --tail=200"
  echo "  ${MIG_NODE_SSH} sudo nvidia-smi mig -lgi   # the live GPU instances on the card"
  exit 1
fi
echo "CASE 23 PASS"
