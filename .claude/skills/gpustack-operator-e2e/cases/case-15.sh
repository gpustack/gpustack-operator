#!/usr/bin/env bash
#
# CASE 15 — Exclusive whole-card SSH Instance still works (regression)
#   (MUTATING, self-recovering; AUTO-SKIPS without a real accelerator or an ssh client)
#
#   case-15.sh <NS>
#
# Goal:        The co-location + capability-drop change must not break the non-sliced SSH path — an
#              exclusive (whole-card) SSH-enabled Instance places the accelerator on `main` and the
#              device-only `.visibility` resource on `sshd`, and through a real SSH login the shell sees
#              the WHOLE card (no HAMI cap) and is still capability-stripped.
# Environment: Needs REAL accelerator hardware advertising a whole-card (.../gpu) resource AND an ssh
#              client on the runner. AUTO-SKIPS (exit 0) when either is missing.
# Inputs:      All real, nothing mocked — an SSH key + secret; an exclusive (no slice %) SSH-enabled
#              Instance (ubuntu, accelerator=1) on the acceleratable pool; a port-forward + real SSH login.
# Expected:    - the Pod reaches 2/2 Running (main + sshd);
#              - main carries the whole-card .../gpu resource (not `.sliced`); sshd carries only `.visibility`;
#              - the SSH session sees the whole card (nvidia-smi total >= 90% of physical, no slice cap);
#              - the SSH shell has empty CapEff and host `mknod` is denied.
# Cleanup:     Trap kills the port-forward, deletes the test Instance and its SSH secret, removes the
#              temp key dir.
set -uo pipefail

NS="${1:?usage: case-15.sh <NS>}"
INST=gpustack-e2e-ssh-exclusive
SECRET=gpustack-e2e-ssh-exclusive-key
LOCAL_PORT=22015
KEYDIR="$(mktemp -d)"

command -v ssh >/dev/null 2>&1 || { echo "== CASE 15 — SKIPPED =="; echo "no ssh client on the runner"; exit 0; }

# --- Skip gate: a real whole-card (exclusive) accelerator must be advertised. ---
gpu_node=$(kubectl get nodes -o json 2>/dev/null | python3 -c "
import json,sys
for n in json.load(sys.stdin).get('items',[]):
    a=n.get('status',{}).get('allocatable',{})
    for k,v in a.items():
        if k.endswith('/gpu') and int(v)>0:
            print(n['metadata']['name']); sys.exit(0)
" 2>/dev/null)
if [ -z "$gpu_node" ]; then
  echo "== CASE 15 — SKIPPED =="
  echo "No node advertises a whole-card accelerator resource — this case needs real accelerator hardware."
  exit 0
fi
echo "[case-15] real accelerator found on ${gpu_node}"

read -r IT CARDMEM <<<"$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null | python3 -c "
import json,sys
for it in json.load(sys.stdin).get('items',[]):
    s=it.get('spec',{})
    if s.get('acceleratable'):
        print(it['metadata']['name'], s.get('memory','')); break
")"
[ -n "$IT" ] || { echo "no acceleratable InstanceType found"; exit 1; }
PHYS_MIB=$(python3 -c "
import re
m = re.match(r'\s*(\d+)\s*([GM])i?', '${CARDMEM}')
print(int(m.group(1)) * (1024 if m.group(2) == 'G' else 1) if m else 0)
" 2>/dev/null)
# A whole card reports close to physical (the driver reserves a little, e.g. T4 15360 of 16384).
WHOLE_MIN=$(python3 -c "print(int(${PHYS_MIB:-0} * 0.9))" 2>/dev/null)
echo "[case-15] acceleratable InstanceType ${IT} (card memory ${CARDMEM} = ${PHYS_MIB}MiB; whole-card >= ${WHOLE_MIN}MiB)"

PF_PID=""
restore() {
  echo
  echo "[case-15] cleanup"
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
  echo "== CASE 15 — Exclusive whole-card SSH Instance still works (regression) =="
  {
    echo "STATUS|CHECK|OBJECT"
    printf '%s\n' "${ROWS[@]}"
  } | column -t -s '|'
  if [ "$FAILS" -ne 0 ]; then
    echo
    echo "FAILED ${FAILS} check(s). Diagnose: kubectl -n default describe pod ${INST}"
    exit 1
  fi
  echo "CASE 15 PASS"
  exit 0
}

# 1. SSH key + secret, then an exclusive (no slice %) SSH-enabled Instance.
ssh-keygen -t ed25519 -f "$KEYDIR/id" -N "" -q
kubectl -n default delete secret "$SECRET" --ignore-not-found >/dev/null 2>&1
kubectl -n default create secret generic "$SECRET" --from-file=authorized_keys="$KEYDIR/id.pub" >/dev/null
echo "[case-15] creating exclusive whole-card SSH Instance ${INST} on ${IT}"
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
  sshPublicKey: { name: ${SECRET} }
  volume: { ephemeral: { capacity: 10Gi } }
  volumeMount: /workspace
EOF

POD="$INST"
ready=""
for _ in $(seq 1 40); do
  phase=$(kubectl -n default get pod "$POD" -o jsonpath='{.status.phase}' 2>/dev/null)
  readies=$(kubectl -n default get pod "$POD" -o jsonpath='{range .status.containerStatuses[*]}{.ready} {end}' 2>/dev/null)
  if [ "$phase" = "Running" ] && [ "$readies" = "true true " ]; then ready=1; break; fi
  sleep 5
done
if [ -z "$ready" ]; then
  record FAIL "exclusive SSH Instance reaches 2/2 Running" "${POD} not Running"
  print_and_exit
fi
record PASS "exclusive SSH Instance reaches 2/2 Running" "${POD} main+sshd Running"

# 2. Shape: main carries the exclusive whole-card resource (not .sliced); sshd carries only .visibility.
shape=$(kubectl -n default get pod "$POD" -o json 2>/dev/null | python3 -c "
import json,sys
p=json.load(sys.stdin)
main=sshd=None
for c in p['spec']['containers']:
    lim=c.get('resources',{}).get('limits',{})
    if c['name']=='main': main=lim
    if c['name']=='sshd': sshd=lim
main_excl=any(k.endswith('/gpu') for k in (main or {}))
main_sliced=any(k.endswith('/gpu.sliced') for k in (main or {}))
sshd_vis=any(k.startswith('device.gpustack.ai/') and k.endswith('.visibility') for k in (sshd or {}))
print('OK' if (main_excl and not main_sliced and sshd_vis) else 'BAD', 'main_excl=%s main_sliced=%s sshd_vis=%s'%(main_excl,main_sliced,sshd_vis))
")
case "$shape" in
  OK*) record PASS "main holds whole-card resource, sshd holds only .visibility" "${shape#OK }" ;;
  *)   record FAIL "main holds whole-card resource, sshd holds only .visibility" "${shape#BAD }" ;;
esac

# 3. SSH login: the shell sees the WHOLE card (not a slice) and is capability-stripped.
kubectl -n default port-forward "pod/$POD" "${LOCAL_PORT}:22" >/dev/null 2>&1 &
PF_PID=$!
for _ in $(seq 1 20); do (exec 3<>"/dev/tcp/127.0.0.1/${LOCAL_PORT}") 2>/dev/null && { exec 3>&- 3<&-; break; }; sleep 1; done
ssh_out=$(ssh -T -p "$LOCAL_PORT" -i "$KEYDIR/id" -o IdentitiesOnly=yes \
  -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes -o ConnectTimeout=15 \
  root@127.0.0.1 2>/dev/null <<'CMDS'
echo "CAPEFF=$(grep CapEff /proc/self/status | awk '{print $2}')"
echo "SMI=$(nvidia-smi --query-gpu=memory.total --format=csv,noheader 2>/dev/null | grep -oE '[0-9]+' | head -1)"
mknod /tmp/case15blk b 8 0 2>/dev/null; echo "MKNOD_RC=$?"
exit
CMDS
)
kill "$PF_PID" 2>/dev/null; PF_PID=""

ssh_smi=$(printf '%s\n' "$ssh_out" | sed -n 's/^SMI=//p')
ssh_capeff=$(printf '%s\n' "$ssh_out" | sed -n 's/^CAPEFF=//p')
ssh_mknod=$(printf '%s\n' "$ssh_out" | sed -n 's/^MKNOD_RC=//p')

if [ -n "$ssh_smi" ] && [ "${WHOLE_MIN:-0}" -gt 0 ] && [ "$ssh_smi" -ge "$WHOLE_MIN" ]; then
  record PASS "SSH session sees the whole card (no slice cap)" "SSH nvidia-smi total=${ssh_smi}MiB >= ${WHOLE_MIN}MiB (whole card)"
else
  record FAIL "SSH session sees the whole card (no slice cap)" "SSH nvidia-smi total='${ssh_smi:-?}' not >= ${WHOLE_MIN:-?}MiB"
fi
if [ "$ssh_capeff" = "0000000000000000" ]; then
  record PASS "SSH shell has no capabilities" "CapEff=${ssh_capeff}"
else
  record FAIL "SSH shell has no capabilities" "CapEff='${ssh_capeff:-?}', want all-zero"
fi
if [ "$ssh_mknod" != "0" ] && [ -n "$ssh_mknod" ]; then
  record PASS "SSH shell cannot mknod a host device" "mknod rc=${ssh_mknod} (denied)"
else
  record FAIL "SSH shell cannot mknod a host device" "mknod rc='${ssh_mknod:-?}', want non-zero (EPERM)"
fi

print_and_exit
