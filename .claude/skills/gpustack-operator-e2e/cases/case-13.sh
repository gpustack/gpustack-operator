#!/usr/bin/env bash
#
# CASE 13 — SSH-enabled sliced Instance: slice visible over SSH + confined shell (Stories 1/4)
#   (MUTATING, self-recovering)
#
#   case-13.sh <NS>
#
# Regression for specs/2026-07-04-ssh-instance-accelerator-slicing: an SSH-enabled Instance renders a
# two-container Pod (main = workload, sshd = Alpine sidecar that nsenter+chroots into main). The fix
# co-locates the sliced accelerator + HAMI artifacts + workload on `main`, gives `sshd` only the
# device-only `device.gpustack.ai/<manufacturer>.visibility` resource (quantity = main's card count),
# and drops all capabilities before the interactive shell. This case proves, on REAL accelerator
# hardware, that:
#   - the rendered Pod places `.sliced*` on `main` and `.visibility` (not the sliced resource) on `sshd`;
#   - `main` (the workload container) sees the slice: nvidia-smi total < physical card;
#   - an interactive SSH login sees the SAME slice (the interception library is preloaded in main's
#     namespace — the bug was the whole card showing here); and
#   - the SSH shell is confined: empty effective/bounding capability set, host `mknod` denied.
#
# Like CASE 8 it needs REAL accelerator hardware (the HAMI runtime cap cannot be mocked) and AUTO-SKIPS
# with a message when no `*.sliced` resource is advertised. It requires an ssh client on the runner.
#
# The slice is intentionally 40% (memory): the node-devices AdmissionCheck's whole-card feasibility
# ledger currently counts a workload's own allocation against itself, so a > 50% single-card slice
# periodically self-evicts (a pre-existing issue in nodeDevicesFeasibility, independent of this fix).
# 40% stays comfortably within one card's budget so the Pod is stable for the assertions.
#
# Self-recovering: deletes the test Instance, its SSH secret, and the port-forward on exit.
set -uo pipefail

NS="${1:?usage: case-13.sh <NS>}"
INST=gpustack-e2e-ssh-slice
SECRET=gpustack-e2e-ssh-slice-key
MEM_PCT=40
LOCAL_PORT=22022
KEYDIR="$(mktemp -d)"

command -v ssh >/dev/null 2>&1 || { echo "== CASE 13 — SKIPPED =="; echo "no ssh client on the runner"; exit 0; }

# --- Skip gate: real sliced accelerator required. ---
sliced_node=$(kubectl get nodes -o json 2>/dev/null | python3 -c "
import json,sys
for n in json.load(sys.stdin).get('items',[]):
    a=n.get('status',{}).get('allocatable',{})
    for k,v in a.items():
        if k.endswith('/gpu.sliced') and int(v)>0:
            print(n['metadata']['name']); sys.exit(0)
" 2>/dev/null)
if [ -z "$sliced_node" ]; then
  echo "== CASE 13 — SKIPPED =="
  echo "No node advertises a *.sliced accelerator resource — this case needs real accelerator hardware."
  exit 0
fi
echo "[case-13] real sliced accelerator found on ${sliced_node}"

# A sliceable accelerated InstanceType and its per-card memory (for the below-physical assertion).
read -r IT CARDMEM <<<"$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null | python3 -c "
import json,sys
for it in json.load(sys.stdin).get('items',[]):
    s=it.get('spec',{})
    if s.get('acceleratable') and s.get('sliceable'):
        print(it['metadata']['name'], s.get('memory','')); break
")"
[ -n "$IT" ] || { echo "no sliceable accelerated InstanceType found"; exit 1; }
PHYS_MIB=$(python3 -c "
import re
m = re.match(r'\s*(\d+)\s*([GM])i?', '${CARDMEM}')
print(int(m.group(1)) * (1024 if m.group(2) == 'G' else 1) if m else 0)
" 2>/dev/null)
echo "[case-13] sliceable InstanceType ${IT} (card memory ${CARDMEM} = ${PHYS_MIB}MiB)"

PF_PID=""
restore() {
  echo
  echo "[case-13] cleanup"
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null
  kubectl -n "$NS" delete instance "$INST" --ignore-not-found --wait=false 2>/dev/null || true
  kubectl -n "$NS" delete secret "$SECRET" --ignore-not-found 2>/dev/null || true
  rm -rf "$KEYDIR"
}
trap restore EXIT

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

print_and_exit() {
  echo
  echo "== CASE 13 — SSH-enabled sliced Instance: slice visible over SSH + confined shell (Stories 1/4) =="
  {
    echo "STATUS|CHECK|OBJECT"
    printf '%s\n' "${ROWS[@]}"
  } | column -t -s '|'
  if [ "$FAILS" -ne 0 ]; then
    echo
    echo "FAILED ${FAILS} check(s). Diagnose: kubectl -n ${NS} describe pod ${INST};"
    echo "kubectl -n ${NS} exec ${INST} -c main -- cat /etc/ld.so.preload"
    exit 1
  fi
  echo "CASE 13 PASS"
  exit 0
}

# 1. SSH key + secret, then a sliced SSH-enabled Instance on the sliceable pool.
ssh-keygen -t ed25519 -f "$KEYDIR/id" -N "" -q
kubectl -n "$NS" delete secret "$SECRET" --ignore-not-found >/dev/null 2>&1
kubectl -n "$NS" create secret generic "$SECRET" --from-file=authorized_keys="$KEYDIR/id.pub" >/dev/null
echo "[case-13] creating Instance ${INST}: ${MEM_PCT}% memory slice, SSH enabled, on ${IT}"
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: Instance
metadata: { name: ${INST}, namespace: ${NS} }
spec:
  type: ${IT}
  image: ubuntu:24.04
  command: ["sleep", "infinity"]
  resources:
    cpu: "2"
    ram: "8Gi"
    localStorage: "20Gi"
    accelerator: "1"
    acceleratorSlicedMemoryPercentage: ${MEM_PCT}
    acceleratorSlicedCoresPercentage: 100
  sshPublicKey: { name: ${SECRET} }
  volume: { ephemeral: { capacity: 10Gi } }
  volumeMount: /workspace
EOF

# 2. Wait for the backing Pod to be 2/2 Running (main + sshd).
POD="$INST"
ready=""
for _ in $(seq 1 40); do
  phase=$(kubectl -n "$NS" get pod "$POD" -o jsonpath='{.status.phase}' 2>/dev/null)
  readies=$(kubectl -n "$NS" get pod "$POD" -o jsonpath='{range .status.containerStatuses[*]}{.ready} {end}' 2>/dev/null)
  if [ "$phase" = "Running" ] && [ "$readies" = "true true " ]; then ready=1; break; fi
  sleep 5
done
if [ -z "$ready" ]; then
  record FAIL "sliced SSH Instance reaches 2/2 Running" "${POD} not Running — check Kueue admission / device plugin"
  print_and_exit
fi
record PASS "sliced SSH Instance reaches 2/2 Running" "${POD} main+sshd Running"

# 3. Rendered Pod shape: main carries the sliced resource; sshd carries only the visibility resource.
shape=$(kubectl -n "$NS" get pod "$POD" -o json 2>/dev/null | python3 -c "
import json,sys
p=json.load(sys.stdin)
main=sshd=None
for c in p['spec']['containers']:
    lim=c.get('resources',{}).get('limits',{})
    if c['name']=='main': main=lim
    if c['name']=='sshd': sshd=lim
main_sliced=any(k.endswith('/gpu.sliced') for k in (main or {}))
sshd_vis=any(k.startswith('device.gpustack.ai/') and k.endswith('.visibility') for k in (sshd or {}))
sshd_sliced=any(k.endswith('/gpu.sliced') for k in (sshd or {}))
print('OK' if (main_sliced and sshd_vis and not sshd_sliced) else 'BAD', 'main_sliced=%s sshd_vis=%s sshd_sliced=%s'%(main_sliced,sshd_vis,sshd_sliced))
")
case "$shape" in
  OK*) record PASS "main holds .sliced, sshd holds only .visibility" "${shape#OK }" ;;
  *)   record FAIL "main holds .sliced, sshd holds only .visibility" "${shape#BAD }" ;;
esac

# 4. main (workload container) sees the slice: nvidia-smi total < physical.
main_smi=$(kubectl -n "$NS" exec "$POD" -c main -- nvidia-smi --query-gpu=memory.total --format=csv,noheader 2>/dev/null | grep -oE '[0-9]+' | head -1)
if [ -n "$main_smi" ] && [ "${PHYS_MIB:-0}" -gt 0 ] && [ "$main_smi" -lt "$PHYS_MIB" ]; then
  record PASS "main nvidia-smi capped below physical" "main total=${main_smi}MiB < physical ${PHYS_MIB}MiB"
else
  record FAIL "main nvidia-smi capped below physical" "main total='${main_smi:-?}' not < physical '${PHYS_MIB:-?}'MiB"
fi

# 5. SSH login (real sshd -> ForceCommand /chroot.sh -> nsenter into main -> setpriv login shell):
#    the slice is visible, the shell has no capabilities, and host mknod is denied.
kubectl -n "$NS" port-forward "pod/$POD" "${LOCAL_PORT}:22" >/dev/null 2>&1 &
PF_PID=$!
for _ in $(seq 1 20); do (exec 3<>"/dev/tcp/127.0.0.1/${LOCAL_PORT}") 2>/dev/null && { exec 3>&- 3<&-; break; }; sleep 1; done
ssh_out=$(ssh -T -p "$LOCAL_PORT" -i "$KEYDIR/id" \
  -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes -o ConnectTimeout=15 \
  root@127.0.0.1 2>/dev/null <<'CMDS'
echo "CAPEFF=$(grep CapEff /proc/self/status | awk '{print $2}')"
echo "CAPBND=$(grep CapBnd /proc/self/status | awk '{print $2}')"
echo "SMI=$(nvidia-smi --query-gpu=memory.total --format=csv,noheader 2>/dev/null | grep -oE '[0-9]+' | head -1)"
mknod /tmp/case13blk b 8 0 2>/dev/null; echo "MKNOD_RC=$?"
exit
CMDS
)
kill "$PF_PID" 2>/dev/null; PF_PID=""

ssh_smi=$(printf '%s\n' "$ssh_out" | sed -n 's/^SMI=//p')
ssh_capeff=$(printf '%s\n' "$ssh_out" | sed -n 's/^CAPEFF=//p')
ssh_capbnd=$(printf '%s\n' "$ssh_out" | sed -n 's/^CAPBND=//p')
ssh_mknod=$(printf '%s\n' "$ssh_out" | sed -n 's/^MKNOD_RC=//p')

if [ -n "$ssh_smi" ] && [ "${PHYS_MIB:-0}" -gt 0 ] && [ "$ssh_smi" -lt "$PHYS_MIB" ]; then
  record PASS "SSH session sees the slice (not the whole card)" "SSH nvidia-smi total=${ssh_smi}MiB < physical ${PHYS_MIB}MiB"
else
  record FAIL "SSH session sees the slice (not the whole card)" "SSH nvidia-smi total='${ssh_smi:-?}' not < physical '${PHYS_MIB:-?}'MiB"
fi
if [ "$ssh_capeff" = "0000000000000000" ] && [ "$ssh_capbnd" = "0000000000000000" ]; then
  record PASS "SSH shell has no capabilities" "CapEff=${ssh_capeff} CapBnd=${ssh_capbnd}"
else
  record FAIL "SSH shell has no capabilities" "CapEff='${ssh_capeff:-?}' CapBnd='${ssh_capbnd:-?}', want all-zero"
fi
if [ "$ssh_mknod" != "0" ] && [ -n "$ssh_mknod" ]; then
  record PASS "SSH shell cannot mknod a host device" "mknod rc=${ssh_mknod} (denied)"
else
  record FAIL "SSH shell cannot mknod a host device" "mknod rc='${ssh_mknod:-?}', want non-zero (EPERM)"
fi

print_and_exit
