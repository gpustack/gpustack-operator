#!/usr/bin/env bash
#
# CASE 7 — Unified SSH Instance suite: CPU non-interactive exec + SFTP + TCP forward, exclusive
#          whole-card SSH, logically sliced SSH, and partition-backed SSH sidecar confinement
#          (MUTATING, self-recovering; AUTO-SKIPS without ssh client tools; accelerated legs
#          auto-skip individually when matching Mock or Real pools are absent)
#
#   case-7.sh <NS>
#
# Prerequisites: nodes=1; gpu=none; cluster=any; inputs=E2E_SSH_IMAGE,F11_EXPECT,F11_IMAGE
# Goal:        Verify the SSH-enabled Instance architecture (main workload container + sshd sidecar
#              with ForceCommand /chroot.sh nsenter + capability drop + .visibility co-allocation)
#              across all four workload modes in one self-recovering suite:
#                Leg 1 (CPU-only SSH Instance — runs on every cluster):
#                  2/2 Running, non-interactive `ssh host '<cmd>'` runs in main with zero banner,
#                  CapEff/CapBnd stripped to all-zero, host `mknod` denied (EPERM), interactive
#                  login still prints banner + runs login shell, `sftp` put/get round-trips into
#                  main's /workspace, and `-L` loopback TCP port-forward round-trips;
#                Leg 2 (Exclusive whole-card SSH Instance — Mock or Real DM):
#                  2/2 Running, main holds whole-card resource while sshd holds only
#                  device.gpustack.ai/<mfr>.visibility, SSH session is capability-stripped with
#                  mknod denied, and sees the whole-card grant (via Mock env or nvidia-smi >= 90%);
#                Leg 3 (Logically sliced 60% SSH Instance — Mock or Real DM):
#                  2/2 Running (no >50% AdmissionCheck self-eviction), main holds .sliced while
#                  sshd holds only .visibility, SSH session is capability-stripped with mknod
#                  denied, and sees the sliced grant (via Mock env or nvidia-smi < physical VRAM);
#                Leg 4 (Partition-backed SSH Instance sidecar confinement — Mock or Real MIG):
#                  2/2 Running on a 4-memory-slice hardware/virtual partition (hot-reloading Mock
#                  DM to nvidia-h100-mig when Mock DM is active and no partitioned pool is live),
#                  and sshd's visible-devices environment names EXACTLY main's partition
#                  ((MOCK-)?MIG-...) with zero parent whole-card UUID leak.
# Environment: Any cluster with a materialized general pool and `ssh`, `ssh-keygen`, `sftp` on the
#              runner. Leg 1 runs everywhere; Legs 2, 3, and 4 run automatically against either
#              CPU Mock Device Manager pools or Real GPU pools, recording explicit SKIP rows when
#              matching pools are absent.
# Inputs:      Temporary ed25519 SSH keypair + Secret, plus up to four SSH-enabled Instances in
#              namespace default (override image via E2E_SSH_IMAGE / F11_IMAGE).
# Expected:    Leg 1 and every available accelerated leg record PASS; absent accelerated legs
#              record SKIP.
# Cleanup:     EXIT trap closes all SSH port-forwards, deletes all test Instances and Secrets,
#              restores the original mock ConfigMap if Leg 4 hot-reloaded Mock DM, and removes the
#              temporary key directory.
set -uo pipefail

# shellcheck disable=SC2034 # Read by _rows-lib.sh.
CASE_ID=7
CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=/dev/null
. "${CASE_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_pool-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_ssh-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_mock-dm-lib.sh"

NS="${1:?usage: case-7.sh <NS>}"
SSH_IMAGE="${E2E_SSH_IMAGE:-ubuntu:24.04}"
F11_IMAGE="${F11_IMAGE:-$SSH_IMAGE}"
F11_EXPECT="${F11_EXPECT:-own}"

for _tool in ssh ssh-keygen sftp; do
  if ! command -v "$_tool" >/dev/null 2>&1; then
    echo "== CASE 7 — SKIPPED =="
    echo "missing ssh client tool on runner: ${_tool}"
    exit 0
  fi
done

SECRET_SSH=e2e-c7-ssh-key
INST_CPU=e2e-c7-ssh-cpu
INST_EXCL=e2e-c7-ssh-excl
INST_SLICE=e2e-c7-ssh-slice
INST_PART=e2e-c7-ssh-part

PORT_CPU=22071
FWD_PORT_CPU=22171
PORT_EXCL=22072
PORT_SLICE=22073
PORT_PART=22074

KEYDIR="$(mktemp -d)"
ORIG_CM_FILE="$(mktemp)"
CM_MUTATED=0

IT_GEN="$(kubectl get instancetypes.worker.gpustack.ai \
  -o jsonpath='{.items[?(@.spec.acceleratable==false)].metadata.name}' 2>/dev/null \
  | tr ' ' '\n' | grep -m1 'gpustack-' || true)"
[ -n "$IT_GEN" ] || { echo "no general InstanceType found — run case-1 first to materialize the chain"; exit 1; }

# shellcheck disable=SC2317 # Invoked via EXIT trap.
cleanup() {
  echo
  echo "[case-7] cleanup: closing forwards, deleting test Instances/Secret, restoring mock ConfigMap if mutated"
  ssh_cleanup
  kubectl -n default delete instance "$INST_CPU" "$INST_EXCL" "$INST_SLICE" "$INST_PART" \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n default delete secret "$SECRET_SSH" --ignore-not-found >/dev/null 2>&1 || true
  if [ "$CM_MUTATED" -eq 1 ]; then
    mock_dm_restore_cm "$NS" "$MOCK_DM_CM" "$ORIG_CM_FILE" || true
  fi
  rm -rf "$KEYDIR" "$ORIG_CM_FILE"
}
trap cleanup EXIT

wait_2of2_running() {
  local pod="$1" tries="${2:-45}" ph rd
  for _ in $(seq 1 "$tries"); do
    ph="$(kubectl -n default get pod "$pod" -o jsonpath='{.status.phase}' 2>/dev/null)"
    rd="$(kubectl -n default get pod "$pod" -o jsonpath='{range .status.containerStatuses[*]}{.ready} {end}' 2>/dev/null)"
    [ "$ph" = "Running" ] && [ "$rd" = "true true " ] && return 0
    sleep 4
  done
  return 1
}

node_is_mock() {
  local node="$1"
  [ "$(kubectl get devices.worker.gpustack.ai "$node" \
    -o jsonpath='{.metadata.annotations.device\.gpustack\.ai/mock}' 2>/dev/null)" = "true" ]
}

ctr_env() {
  local pod="$1" ctr="$2" var="$3"
  kubectl -n default exec "$pod" -c "$ctr" -- sh -c "printf '%s' \"\${${var}:-}\"" 2>/dev/null || true
}

ssh_setup_key_secret default "$SECRET_SSH" "${KEYDIR}/id"

# ===================================================================================================
# Leg 1 — CPU-only SSH Instance: non-interactive exec, capability drop, interactive banner, SFTP, TCP forward
# ===================================================================================================
echo
echo "[case-7] === Leg 1: CPU-only SSH Instance (exec, banner, caps, SFTP, TCP forward) ==="
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: Instance
metadata: { name: ${INST_CPU}, namespace: default }
spec:
  type: ${IT_GEN}
  image: ${SSH_IMAGE}
  command: ["sleep", "infinity"]
  resources:
    cpu: "1"
    ram: "2Gi"
    localStorage: "10Gi"
  sshPublicKey: { name: ${SECRET_SSH} }
  volume: { ephemeral: { capacity: 10Gi } }
  volumeMount: /workspace
EOF

if ! wait_2of2_running "$INST_CPU" 45; then
  record FAIL "Leg 1: CPU SSH Instance reaches 2/2 Running" "${INST_CPU} did not reach 2/2 Running"
else
  record PASS "Leg 1: CPU SSH Instance reaches 2/2 Running" "${INST_CPU} main+sshd Running"
  ssh_open_port_forward default "pod/${INST_CPU}" 22 "$PORT_CPU"

  MARK="__case7_exec__"
  EXEC_CMD="echo MARK=${MARK}; grep -E '^Cap(Eff|Bnd):' /proc/self/status; mknod /tmp/c7blk b 8 0 2>/dev/null; echo MKNOD_RC=\$?"
  exec_stdout="$(ssh_exec "${KEYDIR}/id" "$PORT_CPU" "$EXEC_CMD" 2>"${KEYDIR}/exec.err" || true)"
  exec_stderr="$(cat "${KEYDIR}/exec.err" 2>/dev/null || true)"
  exec_capeff="$(printf '%s\n' "$exec_stdout" | awk '/^CapEff:/{print $2}')"
  exec_capbnd="$(printf '%s\n' "$exec_stdout" | awk '/^CapBnd:/{print $2}')"
  exec_mknod="$(printf '%s\n' "$exec_stdout" | sed -n 's/^MKNOD_RC=//p')"

  if printf '%s\n' "$exec_stdout" | grep -q "MARK=${MARK}"; then
    record PASS "Leg 1: non-interactive command runs in main" "MARK=${MARK} returned"
  else
    record FAIL "Leg 1: non-interactive command runs in main" "marker missing from stdout"
  fi

  if printf '%s\n' "$exec_stdout" "$exec_stderr" | grep -q 'System information as of'; then
    record FAIL "Leg 1: no banner on exec channel" "login banner leaked into exec output"
  else
    record PASS "Leg 1: no banner on exec channel" "clean stdout+stderr"
  fi

  if [ "$exec_capeff" = "0000000000000000" ] && [ "$exec_capbnd" = "0000000000000000" ]; then
    record PASS "Leg 1: exec path is capability-stripped" "CapEff=${exec_capeff} CapBnd=${exec_capbnd}"
  else
    record FAIL "Leg 1: exec path is capability-stripped" "CapEff='${exec_capeff:-?}' CapBnd='${exec_capbnd:-?}'"
  fi

  if [ "$exec_mknod" = "1" ]; then
    record PASS "Leg 1: exec path cannot mknod host device" "mknod rc=1 (EPERM)"
  else
    record FAIL "Leg 1: exec path cannot mknod host device" "mknod rc='${exec_mknod:-?}', want 1"
  fi

  int_out="$(ssh_exec "${KEYDIR}/id" "$PORT_CPU" 2>&1 <<'CMDS' || true
echo INT_MARK=__case7_int__
exit
CMDS
)"
  if printf '%s\n' "$int_out" | grep -q 'System information as of' && printf '%s\n' "$int_out" | grep -q 'INT_MARK=__case7_int__'; then
    record PASS "Leg 1: interactive login prints banner + runs shell" "banner + shell output verified"
  else
    record FAIL "Leg 1: interactive login prints banner + runs shell" "banner or shell output missing"
  fi

  SFTP_MARK="__case7_sftp__"
  printf '%s\n' "$SFTP_MARK" >"${KEYDIR}/up.txt"
  ssh_sftp "${KEYDIR}/id" "$PORT_CPU" >/dev/null 2>&1 <<SFTP || true
put ${KEYDIR}/up.txt /workspace/case7-sftp.txt
get /workspace/case7-sftp.txt ${KEYDIR}/down.txt
SFTP
  sftp_seen="$(ssh_exec "${KEYDIR}/id" "$PORT_CPU" 'cat /workspace/case7-sftp.txt' 2>/dev/null || true)"
  if [ -f "${KEYDIR}/down.txt" ] && diff -q "${KEYDIR}/up.txt" "${KEYDIR}/down.txt" >/dev/null 2>&1 \
    && printf '%s\n' "$sftp_seen" | grep -qF "$SFTP_MARK"; then
    record PASS "Leg 1: sftp round-trips file into main's /workspace" "put/get byte-identical in main"
  else
    record FAIL "Leg 1: sftp round-trips file into main's /workspace" "sftp round-trip or /workspace visibility failed"
  fi

  ssh_open_loopback_forward "${KEYDIR}/id" "$PORT_CPU" "$FWD_PORT_CPU" 127.0.0.1 22
  fwd_greeting=""
  for _ in $(seq 1 15); do
    g="$( (exec 3<>"/dev/tcp/127.0.0.1/${FWD_PORT_CPU}" && head -c 8 <&3) 2>/dev/null || true )"
    if printf '%s' "$g" | grep -q '^SSH-2.0'; then
      fwd_greeting="$g"
      break
    fi
    sleep 1
  done
  ssh_cleanup
  if [ -n "$fwd_greeting" ]; then
    record PASS "Leg 1: loopback TCP port-forward round-trips" "greeting=${fwd_greeting}"
  else
    record FAIL "Leg 1: loopback TCP port-forward round-trips" "no SSH greeting over forwarded port"
  fi
fi
kubectl -n default delete instance "$INST_CPU" --ignore-not-found --wait=false >/dev/null 2>&1 || true

# ===================================================================================================
# Leg 2 — Exclusive Whole-Card SSH Instance (Mock or Real DM)
# ===================================================================================================
echo
echo "[case-7] === Leg 2: exclusive whole-card SSH Instance ==="
read -r IT_EXCL CARDMEM_EXCL MANUF_EXCL <<<"$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null | python3 -c "
import json, sys
for it in json.load(sys.stdin).get('items', []):
    s = it.get('spec', {})
    st = it.get('status', {}) or {}
    d = st.get('detail') or {}
    rem = int((st.get('accelerator') or {}).get('remaining', 0) or 0)
    if s.get('acceleratable') and st.get('phase') == 'Active' and st.get('entrance') and rem >= 1:
        print(it['metadata']['name'], d.get('memory', ''), d.get('manufacturer', ''))
        break
" 2>/dev/null)"

if [ -z "${IT_EXCL:-}" ]; then
  record SKIP "Leg 2: exclusive whole-card SSH Instance" "no Active accelerated pool with remaining whole-card capacity >= 1"
else
  PHYS_EXCL_MIB="$(python3 -c "
import re
m = re.match(r'\s*(\d+)\s*([GM])i?', '${CARDMEM_EXCL}')
print(int(m.group(1)) * (1024 if m.group(2) == 'G' else 1) if m else 0)
" 2>/dev/null)"

  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: Instance
metadata: { name: ${INST_EXCL}, namespace: default }
spec:
  type: ${IT_EXCL}
  image: ${SSH_IMAGE}
  command: ["sleep", "infinity"]
  resources:
    cpu: "1"
    ram: "2Gi"
    localStorage: "10Gi"
    accelerator: "1"
  sshPublicKey: { name: ${SECRET_SSH} }
  volume: { ephemeral: { capacity: 5Gi } }
  volumeMount: /workspace
EOF

  if ! wait_2of2_running "$INST_EXCL" 45; then
    record FAIL "Leg 2: exclusive SSH Instance reaches 2/2 Running" "${INST_EXCL} did not reach 2/2 Running"
  else
    record PASS "Leg 2: exclusive SSH Instance reaches 2/2 Running" "${INST_EXCL} main+sshd Running on ${IT_EXCL}"
    excl_shape="$(kubectl -n default get pod "$INST_EXCL" -o json 2>/dev/null | python3 -c "
import json, re, sys
p = json.load(sys.stdin)
main = sshd = {}
for c in p['spec']['containers']:
    lim = c.get('resources', {}).get('limits', {})
    if c['name'] == 'main':
        main = lim
    elif c['name'] == 'sshd':
        sshd = lim
main_excl = any(re.match(r'^[^/]+/[^.]+$', k) and k != 'device.gpustack.ai/rdma' for k in main)
main_sl = any('.sliced' in k or '.partitioned' in k for k in main)
sshd_vis = any(k.startswith('device.gpustack.ai/') and k.endswith('.visibility') for k in sshd)
sshd_acc = any(re.match(r'^[^/]+/[^.]+', k) and not k.startswith('device.gpustack.ai/') for k in sshd)
print('OK' if (main_excl and not main_sl and sshd_vis and not sshd_acc) else 'BAD',
      f'main_excl={main_excl} main_sl={main_sl} sshd_vis={sshd_vis}')
" 2>/dev/null)"
    case "$excl_shape" in
      OK*) record PASS "Leg 2: main holds whole-card resource, sshd holds only .visibility" "${excl_shape#OK }" ;;
      *)   record FAIL "Leg 2: main holds whole-card resource, sshd holds only .visibility" "${excl_shape#BAD }" ;;
    esac

    EXCL_NODE="$(kubectl -n default get pod "$INST_EXCL" -o jsonpath='{.spec.nodeName}' 2>/dev/null)"
    ssh_open_port_forward default "pod/${INST_EXCL}" 22 "$PORT_EXCL"
    ssh_excl_out="$(ssh_exec "${KEYDIR}/id" "$PORT_EXCL" 2>/dev/null <<'CMDS' || true
echo "CAPEFF=$(grep CapEff /proc/self/status | awk '{print $2}')"
echo "MOCK_MODE=${GPUSTACK_MOCK_ALLOCATION_MODE:-}"
echo "MOCK_DEVS=${GPUSTACK_MOCK_ALLOCATED_DEVICES:-}"
echo "SMI=$(command -v nvidia-smi >/dev/null 2>&1 && nvidia-smi --query-gpu=memory.total --format=csv,noheader 2>/dev/null | grep -oE '[0-9]+' | head -1 || true)"
mknod /tmp/c7exclblk b 8 0 2>/dev/null; echo "MKNOD_RC=$?"
exit
CMDS
)"
    ssh_cleanup

    ex_cap="$(printf '%s\n' "$ssh_excl_out" | sed -n 's/^CAPEFF=//p')"
    ex_mknod="$(printf '%s\n' "$ssh_excl_out" | sed -n 's/^MKNOD_RC=//p')"
    ex_mock_mode="$(printf '%s\n' "$ssh_excl_out" | sed -n 's/^MOCK_MODE=//p')"
    ex_mock_devs="$(printf '%s\n' "$ssh_excl_out" | sed -n 's/^MOCK_DEVS=//p')"
    ex_smi="$(printf '%s\n' "$ssh_excl_out" | sed -n 's/^SMI=//p')"

    if [ "$ex_cap" = "0000000000000000" ] && [ -n "$ex_mknod" ] && [ "$ex_mknod" != "0" ]; then
      record PASS "Leg 2: exclusive SSH shell is capability-stripped & mknod denied" "CapEff=${ex_cap} mknod_rc=${ex_mknod}"
    else
      record FAIL "Leg 2: exclusive SSH shell is capability-stripped & mknod denied" "CapEff='${ex_cap}' mknod_rc='${ex_mknod}'"
    fi

    if node_is_mock "$EXCL_NODE"; then
      sshd_vis_devs="$(ctr_env "$INST_EXCL" sshd GPUSTACK_MOCK_ALLOCATED_DEVICES)"
      if [ "$ex_mock_mode" = "exclusive" ] && [ -n "$ex_mock_devs" ] && [ "$ex_mock_devs" = "$sshd_vis_devs" ]; then
        record PASS "Leg 2: exclusive SSH session & sidecar see whole-card grant" "UUID=${ex_mock_devs}"
      else
        record FAIL "Leg 2: exclusive SSH session & sidecar see whole-card grant" \
          "mode='${ex_mock_mode}' main='${ex_mock_devs}' sshd='${sshd_vis_devs}'"
      fi
    elif [ "$MANUF_EXCL" = "nvidia" ] && [ "${PHYS_EXCL_MIB:-0}" -gt 0 ]; then
      whole_min=$(( PHYS_EXCL_MIB * 9 / 10 ))
      if [ -n "$ex_smi" ] && [ "$ex_smi" -ge "$whole_min" ]; then
        record PASS "Leg 2: exclusive SSH session sees whole card (no slice cap)" "${ex_smi}MiB >= ${whole_min}MiB"
      else
        record FAIL "Leg 2: exclusive SSH session sees whole card (no slice cap)" "smi='${ex_smi:-?}' < ${whole_min}MiB"
      fi
    else
      record PASS "Leg 2: exclusive SSH session rendered on ${MANUF_EXCL:-accelerator}" "node=${EXCL_NODE}"
    fi
  fi
  kubectl -n default delete instance "$INST_EXCL" --ignore-not-found --timeout=60s >/dev/null 2>&1 || true
fi

# ===================================================================================================
# Leg 3 — Logically Sliced 60% SSH Instance (Mock or Real DM)
# ===================================================================================================
echo
echo "[case-7] === Leg 3: logically sliced 60% SSH Instance ==="
read -r IT_SL CARDMEM_SL MANUF_SL <<<"$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null | python3 -c "
import json, sys
for it in json.load(sys.stdin).get('items', []):
    s = it.get('spec', {})
    st = it.get('status', {}) or {}
    d = st.get('detail') or {}
    sd = ((d.get('slicedDetail') or {}).get('logical') or {}).get('count', 0) or 0
    omr = int((st.get('acceleratorSliced') or {}).get('onceMaxRequest', 0) or 0)
    if s.get('acceleratable') and st.get('phase') == 'Active' and st.get('entrance') and sd > 0 and omr >= 60:
        print(it['metadata']['name'], d.get('memory', ''), d.get('manufacturer', ''))
        break
" 2>/dev/null)"

if [ -z "${IT_SL:-}" ]; then
  record SKIP "Leg 3: logically sliced SSH Instance" "no Active logically sliceable pool with onceMaxRequest >= 60"
else
  PHYS_SL_MIB="$(python3 -c "
import re
m = re.match(r'\s*(\d+)\s*([GM])i?', '${CARDMEM_SL}')
print(int(m.group(1)) * (1024 if m.group(2) == 'G' else 1) if m else 0)
" 2>/dev/null)"

  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: Instance
metadata: { name: ${INST_SLICE}, namespace: default }
spec:
  type: ${IT_SL}
  image: ${SSH_IMAGE}
  command: ["sleep", "infinity"]
  resources:
    cpu: "1"
    ram: "2Gi"
    localStorage: "10Gi"
    accelerator: "1"
    acceleratorSlicedMemoryPercentage: 60
    acceleratorSlicedCoresPercentage: 100
  sshPublicKey: { name: ${SECRET_SSH} }
  volume: { ephemeral: { capacity: 5Gi } }
  volumeMount: /workspace
EOF

  if ! wait_2of2_running "$INST_SLICE" 45; then
    record FAIL "Leg 3: sliced SSH Instance reaches 2/2 Running" "${INST_SLICE} did not reach 2/2 Running"
  else
    record PASS "Leg 3: sliced SSH Instance reaches 2/2 Running" "${INST_SLICE} main+sshd Running (60% slice)"
    sl_shape="$(kubectl -n default get pod "$INST_SLICE" -o json 2>/dev/null | python3 -c "
import json, sys
p = json.load(sys.stdin)
main = sshd = {}
for c in p['spec']['containers']:
    lim = c.get('resources', {}).get('limits', {})
    if c['name'] == 'main':
        main = lim
    elif c['name'] == 'sshd':
        sshd = lim
main_sl = any(k.endswith('.sliced') for k in main)
sshd_vis = any(k.startswith('device.gpustack.ai/') and k.endswith('.visibility') for k in sshd)
sshd_sl = any('.sliced' in k for k in sshd)
print('OK' if (main_sl and sshd_vis and not sshd_sl) else 'BAD',
      f'main_sl={main_sl} sshd_vis={sshd_vis} sshd_sl={sshd_sl}')
" 2>/dev/null)"
    case "$sl_shape" in
      OK*) record PASS "Leg 3: main holds .sliced, sshd holds only .visibility" "${sl_shape#OK }" ;;
      *)   record FAIL "Leg 3: main holds .sliced, sshd holds only .visibility" "${sl_shape#BAD }" ;;
    esac

    SL_NODE="$(kubectl -n default get pod "$INST_SLICE" -o jsonpath='{.spec.nodeName}' 2>/dev/null)"
    ssh_open_port_forward default "pod/${INST_SLICE}" 22 "$PORT_SLICE"
    ssh_sl_out="$(ssh_exec "${KEYDIR}/id" "$PORT_SLICE" 2>/dev/null <<'CMDS' || true
echo "CAPEFF=$(grep CapEff /proc/self/status | awk '{print $2}')"
echo "CAPBND=$(grep CapBnd /proc/self/status | awk '{print $2}')"
echo "MOCK_MODE=${GPUSTACK_MOCK_ALLOCATION_MODE:-}"
echo "MOCK_DEVS=${GPUSTACK_MOCK_ALLOCATED_DEVICES:-}"
echo "SMI=$(command -v nvidia-smi >/dev/null 2>&1 && nvidia-smi --query-gpu=memory.total --format=csv,noheader 2>/dev/null | grep -oE '[0-9]+' | head -1 || true)"
mknod /tmp/c7slblk b 8 0 2>/dev/null; echo "MKNOD_RC=$?"
exit
CMDS
)"
    ssh_cleanup

    sl_cap="$(printf '%s\n' "$ssh_sl_out" | sed -n 's/^CAPEFF=//p')"
    sl_bnd="$(printf '%s\n' "$ssh_sl_out" | sed -n 's/^CAPBND=//p')"
    sl_mknod="$(printf '%s\n' "$ssh_sl_out" | sed -n 's/^MKNOD_RC=//p')"
    sl_mock_mode="$(printf '%s\n' "$ssh_sl_out" | sed -n 's/^MOCK_MODE=//p')"
    sl_mock_devs="$(printf '%s\n' "$ssh_sl_out" | sed -n 's/^MOCK_DEVS=//p')"
    sl_smi="$(printf '%s\n' "$ssh_sl_out" | sed -n 's/^SMI=//p')"

    if [ "$sl_cap" = "0000000000000000" ] && [ "$sl_bnd" = "0000000000000000" ] && [ -n "$sl_mknod" ] && [ "$sl_mknod" != "0" ]; then
      record PASS "Leg 3: sliced SSH shell is capability-stripped & mknod denied" "CapEff=${sl_cap} mknod_rc=${sl_mknod}"
    else
      record FAIL "Leg 3: sliced SSH shell is capability-stripped & mknod denied" "CapEff='${sl_cap}' CapBnd='${sl_bnd}' mknod='${sl_mknod}'"
    fi

    if node_is_mock "$SL_NODE"; then
      sshd_sl_devs="$(ctr_env "$INST_SLICE" sshd GPUSTACK_MOCK_ALLOCATED_DEVICES)"
      sshd_sl_mode="$(ctr_env "$INST_SLICE" sshd GPUSTACK_MOCK_ALLOCATION_MODE)"
      if [ "$sl_mock_mode" = "sliced" ] && [ "$sshd_sl_mode" = "visibility" ] && [ -n "$sl_mock_devs" ] && [ "$sl_mock_devs" = "$sshd_sl_devs" ]; then
        record PASS "Leg 3: sliced SSH session & sidecar co-allocate same card" "UUID=${sl_mock_devs}"
      else
        record FAIL "Leg 3: sliced SSH session & sidecar co-allocate same card" \
          "main=${sl_mock_mode}/${sl_mock_devs} sshd=${sshd_sl_mode}/${sshd_sl_devs}"
      fi
    elif [ "$MANUF_SL" = "nvidia" ] && [ "${PHYS_SL_MIB:-0}" -gt 0 ]; then
      main_smi="$(kubectl -n default exec "$INST_SLICE" -c main -- nvidia-smi --query-gpu=memory.total --format=csv,noheader 2>/dev/null | grep -oE '[0-9]+' | head -1 || true)"
      if [ -n "$main_smi" ] && [ "$main_smi" -lt "$PHYS_SL_MIB" ] && [ -n "$sl_smi" ] && [ "$sl_smi" -lt "$PHYS_SL_MIB" ]; then
        record PASS "Leg 3: main and SSH session both see capped slice (< physical VRAM)" \
          "main=${main_smi}MiB ssh=${sl_smi}MiB < ${PHYS_SL_MIB}MiB"
      else
        record FAIL "Leg 3: main and SSH session both see capped slice (< physical VRAM)" \
          "main='${main_smi:-?}' ssh='${sl_smi:-?}' phys=${PHYS_SL_MIB}MiB"
      fi
    else
      record PASS "Leg 3: sliced SSH session rendered on ${MANUF_SL:-accelerator}" "node=${SL_NODE}"
    fi
  fi
  kubectl -n default delete instance "$INST_SLICE" --ignore-not-found --timeout=60s >/dev/null 2>&1 || true
fi

# ===================================================================================================
# Leg 4 — Partition-Backed SSH Instance Sidecar Confinement (Mock or Real MIG)
# ===================================================================================================
echo
echo "[case-7] === Leg 4: partition-backed SSH Instance sidecar confinement ==="

discover_partition_target() {
  kubectl get devices.worker.gpustack.ai -o json 2>/dev/null | python3 -c "
import json, sys
for d in json.load(sys.stdin).get('items', []):
    node = d['metadata']['name']
    for g in d.get('spec', {}).get('groups', []):
        gid = g.get('id', '')
        mfr = g.get('manufacturer', '')
        for a in g.get('accelerators', []):
            if (a.get('status') or {}).get('unhealthy'):
                continue
            profs = ((a.get('status') or {}).get('physicalSliced') or {}).get('profiles') or []
            if not profs:
                continue
            mid = next((p['name'] for p in profs if p.get('memorySlices') == 4 and p.get('computeSlices') == 3), '')
            chosen = mid or profs[0]['name']
            print(node, gid, mfr, chosen)
            sys.exit(0)
" 2>/dev/null
}

read -r PART_NODE PART_GID PART_MANUF PART_PROF <<<"$(discover_partition_target || true)"

# If no partitioned card is currently live and Mock DM is active (serving nvidia), hot-reload the
# primary mock node to nvidia-h100-mig temporarily (restored by EXIT trap).
if [ -z "${PART_NODE:-}" ] && mock_dm_detect_env "$NS" && mock_dm_manufacturer_allowed nvidia; then
  if mock_dm_snapshot_cm "$NS" "$MOCK_DM_CM" "$ORIG_CM_FILE"; then
    CM_MUTATED=1
    DEFAULT_C7="  groups: []"
    mock_dm_manufacturer_allowed amd && DEFAULT_C7="  preset: amd-mi300x
  count: 1"
    SPEC_MIG_C7="$(cat <<EOF
default:
${DEFAULT_C7}
nodes:
  ${MOCK_DM_PRIMARY_NODE}:
    preset: nvidia-h100-mig
    count: 1
EOF
)"
    mock_dm_apply_spec "$NS" "$MOCK_DM_CM" "$MOCK_DM_KEY" "$SPEC_MIG_C7"
    mock_dm_wait_devices "$NS" "$MOCK_DM_PRIMARY_NODE" \
      '([.spec.groups[] | select(.manufacturer == "nvidia") | .accelerators[] | select((.status.physicalSliced.profiles // []) | length > 0)] | length == 1)' 90 || true
    mock_dm_wait_node_allocatable "$MOCK_DM_PRIMARY_NODE" '(.["nvidia.com/gpu.partitioned"] // "0" | tonumber) > 0' 90 || true
    read -r PART_NODE PART_GID PART_MANUF PART_PROF <<<"$(discover_partition_target || true)"
  fi
fi

if [ -z "${PART_NODE:-}" ] || [ -z "${PART_PROF:-}" ]; then
  record SKIP "Leg 4: partition-backed SSH sidecar confinement" \
    "no active hardware-partitioned pool or nvidia Mock DM on this cluster"
else
  IT_PART=""
  for _ in $(seq 1 30); do
    IT_PART="$(discover_node_pool "$PART_NODE" "$PART_GID" --require-entrance --fallback-any name 2>/dev/null || true)"
    [ -n "$IT_PART" ] && break
    sleep 3
  done

  if [ -z "$IT_PART" ]; then
    record FAIL "Leg 4: partition-backed SSH Instance reaches 2/2 Running" \
      "no InstanceType backs partitioned node ${PART_NODE} group=${PART_GID}"
  else
    cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: Instance
metadata: { name: ${INST_PART}, namespace: default }
spec:
  type: ${IT_PART}
  nodeName: ${PART_NODE}
  image: ${F11_IMAGE}
  command: ["sleep", "infinity"]
  resources:
    cpu: "1"
    ram: "2Gi"
    localStorage: "10Gi"
    accelerator: "1"
    acceleratorPartitionedProfile: ${PART_PROF}
  sshPublicKey: { name: ${SECRET_SSH} }
  volume: { ephemeral: { capacity: 5Gi } }
  volumeMount: /workspace
EOF

    if ! wait_2of2_running "$INST_PART" 50; then
      record FAIL "Leg 4: partition-backed SSH Instance reaches 2/2 Running" \
        "${INST_PART} did not reach 2/2 Running on profile ${PART_PROF}"
    else
      record PASS "Leg 4: partition-backed SSH Instance reaches 2/2 Running" \
        "${INST_PART} main+sshd Running on ${PART_MANUF} profile ${PART_PROF} (${PART_NODE})"

      vis_env_ctr() {
        # shellcheck disable=SC2016 # Variables expand inside the container's shell.
        kubectl -n default exec "$INST_PART" -c "$1" -- sh -c \
          'printf "NVIDIA=%s CUDA=%s MOCK=%s PPU=%s HIP=%s" "${NVIDIA_VISIBLE_DEVICES:-}" "${CUDA_VISIBLE_DEVICES:-}" "${GPUSTACK_MOCK_ALLOCATED_DEVICES:-}" "${PPU_VISIBLE_DEVICES:-}" "${DMI_MIG_VISIBLE_DEVICE:-}"' \
          2>/dev/null || true
      }

      MAIN_ENV="$(vis_env_ctr main)"
      SSHD_ENV="$(vis_env_ctr sshd)"

      part_handles() {
        printf '%s\n' "$1" | grep -oE '(MOCK-)?MIG-[A-Za-z0-9._-]+' | sort -u | paste -sd, -
      }
      parent_gpus() {
        printf '%s\n' "$1" | grep -oE 'GPU-[0-9a-fA-F-]+' | sort -u | paste -sd, -
      }

      MAIN_MIG="$(part_handles "$MAIN_ENV")"
      SSHD_MIG="$(part_handles "$SSHD_ENV")"
      SSHD_GPU="$(parent_gpus "$SSHD_ENV")"

      ssh_open_port_forward default "pod/${INST_PART}" 22 "$PORT_PART"
      # shellcheck disable=SC2016 # $2 expands inside awk on the remote SSH session.
      ssh_part_cap="$(ssh_exec "${KEYDIR}/id" "$PORT_PART" 'awk "/^CapEff:/{print \$2}" /proc/self/status' 2>/dev/null | tr -d '\r\n' || true)"
      ssh_cleanup

      if [ "$ssh_part_cap" = "0000000000000000" ]; then
        record PASS "Leg 4: partition SSH shell is capability-stripped" "CapEff=${ssh_part_cap}"
      else
        record FAIL "Leg 4: partition SSH shell is capability-stripped" "CapEff='${ssh_part_cap:-?}'"
      fi

      if [ -n "$MAIN_MIG" ] && [ "$SSHD_MIG" = "$MAIN_MIG" ] && [ -z "$SSHD_GPU" ]; then
        VSTATE="own"
        VERDICT="sshd visible-devices names EXACTLY main's partition (${MAIN_MIG}) with zero parent GPU UUID"
      elif [ -n "$SSHD_GPU" ]; then
        VSTATE="whole"
        VERDICT="sshd visible-devices leaked parent GPU (${SSHD_GPU}) instead of partition (${MAIN_MIG:-<none>})"
      else
        VSTATE="other"
        VERDICT="sshd visible-devices (${SSHD_MIG:-<empty>}) differs from main (${MAIN_MIG:-<empty>}); raw main=[${MAIN_ENV}] sshd=[${SSHD_ENV}]"
      fi

      if [ "$F11_EXPECT" = "observe" ]; then
        record PASS "Leg 4: OBSERVED sidecar confinement verdict" "${VERDICT} (F11_EXPECT=observe)"
      elif [ "$VSTATE" = "own" ]; then
        record PASS "Leg 4: sidecar sees exactly main's partition" "$VERDICT"
      else
        record FAIL "Leg 4: sidecar sees exactly main's partition" "$VERDICT"
      fi
    fi
  fi
fi

echo
echo "== CASE 7 — Unified SSH Instance suite (CPU, exclusive, sliced, partition-backed) =="
print_rows
if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s)."
  exit 1
fi
echo "CASE 7 PASS"
