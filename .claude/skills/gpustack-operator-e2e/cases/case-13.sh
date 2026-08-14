#!/usr/bin/env bash
#
# CASE 13 — SSH-enabled sliced Instance: slice visible over SSH + confined shell
#   (MUTATING, self-recovering; AUTO-SKIPS without real sliced hardware or an ssh client)
#
#   case-13.sh <NS>
#
# Goal:        A sliced SSH-enabled Instance renders a two-container Pod (main = workload, sshd = Alpine
#              sidecar that nsenter+chroots into main) that co-locates the sliced accelerator + HAMI
#              artifacts + workload on `main` and gives `sshd` only the device-only
#              `device.gpustack.ai/<manufacturer>.visibility` resource (quantity = main's card count),
#              dropping all capabilities before the interactive shell. The slice is visible in both
#              `main` and a real SSH login, and the SSH shell is capability-stripped. The 60% slice
#              (>50% single-card) also guards the AdmissionCheck fix that stops it counting a Workload's
#              own allocation against itself (which previously self-evicted >50% slices in a recreate loop).
# Environment: Needs REAL accelerator hardware advertising a *.sliced resource (the HAMI cap cannot be
#              mocked) AND an ssh client on the runner. AUTO-SKIPS (exit 0) when either is missing.
# Inputs:      All real, nothing mocked — an SSH key + secret; a 60%-memory-slice SSH-enabled Instance
#              (ubuntu, accelerator=1, cores%=100) on the sliceable pool; a port-forward + real SSH login.
# Expected:    - the Pod reaches 2/2 Running (main + sshd);
#              - main carries `.sliced`, sshd carries only `.visibility` (not the sliced resource);
#              - main nvidia-smi total < physical card;
#              - the SSH session sees the SAME slice (nvidia-smi total < physical);
#              - the SSH shell has empty CapEff/CapBnd and host `mknod` is denied.
# Cleanup:     Trap kills the port-forward, deletes the test Instance and its SSH secret, removes the
#              temp key dir.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-13.sh <NS>}"
INST=gpustack-e2e-ssh-slice
SECRET=gpustack-e2e-ssh-slice-key
MEM_PCT=60
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
  kubectl -n default delete instance "$INST" --ignore-not-found --wait=false 2>/dev/null || true
  kubectl -n default delete secret "$SECRET" --ignore-not-found 2>/dev/null || true
  rm -rf "$KEYDIR"
}
trap restore EXIT

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

print_and_exit() {
  echo
  echo "== CASE 13 — SSH-enabled sliced Instance: slice visible over SSH + confined shell =="
  {
    echo "STATUS|CHECK|OBJECT"
    printf '%s\n' "${ROWS[@]}"
  } | column -t -s '|'
  if [ "$FAILS" -ne 0 ]; then
    echo
    echo "FAILED ${FAILS} check(s). Diagnose: kubectl -n default describe pod ${INST};"
    echo "kubectl -n default exec ${INST} -c main -- cat /etc/ld.so.preload"
    exit 1
  fi
  echo "CASE 13 PASS"
  exit 0
}

# 1. SSH key + secret, then a sliced SSH-enabled Instance on the sliceable pool.
ssh-keygen -t ed25519 -f "$KEYDIR/id" -N "" -q
kubectl -n default delete secret "$SECRET" --ignore-not-found >/dev/null 2>&1
kubectl -n default create secret generic "$SECRET" --from-file=authorized_keys="$KEYDIR/id.pub" >/dev/null
echo "[case-13] creating Instance ${INST}: ${MEM_PCT}% memory slice, SSH enabled, on ${IT}"
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: Instance
metadata: { name: ${INST}, namespace: default }
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
  phase=$(kubectl -n default get pod "$POD" -o jsonpath='{.status.phase}' 2>/dev/null)
  readies=$(kubectl -n default get pod "$POD" -o jsonpath='{range .status.containerStatuses[*]}{.ready} {end}' 2>/dev/null)
  if [ "$phase" = "Running" ] && [ "$readies" = "true true " ]; then ready=1; break; fi
  sleep 5
done
if [ -z "$ready" ]; then
  record FAIL "sliced SSH Instance reaches 2/2 Running" "${POD} not Running — check Kueue admission / device plugin"
  print_and_exit
fi
record PASS "sliced SSH Instance reaches 2/2 Running" "${POD} main+sshd Running"

# 3. Rendered Pod shape: main carries the sliced resource; sshd carries only the visibility resource.
shape=$(kubectl -n default get pod "$POD" -o json 2>/dev/null | python3 -c "
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
main_smi=$(kubectl -n default exec "$POD" -c main -- nvidia-smi --query-gpu=memory.total --format=csv,noheader 2>/dev/null | grep -oE '[0-9]+' | head -1)
if [ -n "$main_smi" ] && [ "${PHYS_MIB:-0}" -gt 0 ] && [ "$main_smi" -lt "$PHYS_MIB" ]; then
  record PASS "main nvidia-smi capped below physical" "main total=${main_smi}MiB < physical ${PHYS_MIB}MiB"
else
  record FAIL "main nvidia-smi capped below physical" "main total='${main_smi:-?}' not < physical '${PHYS_MIB:-?}'MiB"
fi

# 5. SSH login (real sshd -> ForceCommand /chroot.sh -> nsenter into main -> setpriv login shell):
#    the slice is visible, the shell has no capabilities, and host mknod is denied.
kubectl -n default port-forward "pod/$POD" "${LOCAL_PORT}:22" >/dev/null 2>&1 &
PF_PID=$!
for _ in $(seq 1 20); do (exec 3<>"/dev/tcp/127.0.0.1/${LOCAL_PORT}") 2>/dev/null && { exec 3>&- 3<&-; break; }; sleep 1; done
ssh_out=$(ssh -T -p "$LOCAL_PORT" -i "$KEYDIR/id" -o IdentitiesOnly=yes \
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
