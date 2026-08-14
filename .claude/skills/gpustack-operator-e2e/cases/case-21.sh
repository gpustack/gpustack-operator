#!/usr/bin/env bash
#
# CASE 21 — SSH Instance serves non-interactive SSH; interactive login unchanged
#   (MUTATING, self-recovering; AUTO-SKIPS without the ssh client tools)
#
#   case-21.sh <NS>
#
# Goal:        An SSH-enabled Instance must behave like a stock SSH server on the exec channel: a
#              non-interactive `ssh host '<cmd>'` runs the command inside `main` and returns only the
#              command's output — no login banner — while a plain interactive `ssh host` still prints
#              the banner and a login shell in `main`. Every path stays confined (empty capabilities
#              in `main`'s rootfs, host `mknod` denied); an `sftp` put/get round-trips through the
#              bundled static sftp-server into `main`'s own filesystem, and a loopback TCP port-forward
#              through the Instance round-trips. Guards the regression where `chroot.sh` ignored the requested
#              command and always launched an interactive login shell, so VS Code Remote-SSH / scp /
#              rsync never ran their bootstrap command.
# Environment: Any cluster with a materialized general (CPU-only) pool; needs a real cluster and the
#              `ssh`/`ssh-keygen`/`sftp` client tools on the runner. No GPU/accelerator required.
#              AUTO-SKIPS (exit 0) when any of those tools is missing.
# Inputs:      All real, nothing mocked — an SSH key + secret; the general InstanceType unit spec; a
#              CPU-only SSH-enabled Instance (ubuntu, no accelerator) on the general pool; a
#              port-forward and real SSH connections (exec channel, interactive PTY-less login, an `sftp`
#              subsystem session, and a `-L` TCP forward).
# Expected:    - the Pod reaches 2/2 Running (main + sshd);
#              - `ssh host '<cmd>'` returns the command's output (marker present), with no banner on
#                stdout or stderr;
#              - that exec path runs capability-stripped (empty CapEff/CapBnd) and host `mknod` is denied;
#              - a plain interactive `ssh host` still prints the login banner and runs a shell in `main`;
#              - an `sftp` put/get round-trips and the uploaded file is visible in `main`'s /workspace;
#              - a loopback TCP port-forward through the Instance round-trips (reaches the in-Pod sshd).
# Cleanup:     Trap kills the port-forwards, deletes the test Instance and its SSH secret, removes the
#              temp key dir. The general InstanceType unit spec is left set (idempotent; shared with the
#              general-pool cases).
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-21.sh <NS>}"
INST=gpustack-e2e-ssh-noninteractive
SECRET=gpustack-e2e-ssh-noninteractive-key
LOCAL_PORT=22021
FWD_PORT=22121
MARK=__case21_exec__
KEYDIR="$(mktemp -d)"

for _tool in ssh ssh-keygen sftp; do
  command -v "$_tool" >/dev/null 2>&1 || { echo "== CASE 21 — SKIPPED =="; echo "missing ssh client tool: $_tool"; exit 0; }
done

IT=$(kubectl get instancetypes.worker.gpustack.ai \
  -o jsonpath='{.items[?(@.spec.acceleratable==false)].metadata.name}' 2>/dev/null | tr ' ' '\n' | grep -m1 'gpustack-')
[ -n "$IT" ] || { echo "no general InstanceType found — run case-1 first to materialize the chain"; exit 1; }

PF_PID=""
FWD_PID=""
restore() {
  echo
  echo "[case-21] cleanup"
  [ -n "$FWD_PID" ] && kill "$FWD_PID" 2>/dev/null
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
  echo "== CASE 21 — SSH Instance serves non-interactive SSH; interactive login unchanged =="
  {
    echo "STATUS|CHECK|OBJECT"
    printf '%s\n' "${ROWS[@]}"
  } | column -t -s '|'
  if [ "$FAILS" -ne 0 ]; then
    echo
    echo "FAILED ${FAILS} check(s). Diagnose: kubectl -n default describe pod ${INST};"
    echo "kubectl -n default exec ${INST} -c sshd -- cat /chroot.sh"
    exit 1
  fi
  echo "CASE 21 PASS"
  exit 0
}

# The general InstanceType carries no unit spec by default; the Instance webhook needs one to size the
# Pod. Set it and confirm it stuck (the validating webhook may be briefly unready after deploy).
unit_ram=""
for _ in $(seq 1 15); do
  kubectl patch instancetypes.worker.gpustack.ai "$IT" --type=merge \
    -p '{"spec":{"unitResources":{"cpu":"1","ram":"2Gi"},"localStorage":"10Gi"}}' >/dev/null 2>&1
  unit_ram=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.spec.unitResources.ram}' 2>/dev/null)
  [ -n "$unit_ram" ] && break
  sleep 3
done
[ -n "$unit_ram" ] || { echo "no unit spec on ${IT} (validating webhook not ready?)"; exit 1; }

# 1. SSH key + secret, then a CPU-only SSH-enabled Instance on the general pool.
ssh-keygen -t ed25519 -f "$KEYDIR/id" -N "" -q
kubectl -n default delete secret "$SECRET" --ignore-not-found >/dev/null 2>&1
kubectl -n default create secret generic "$SECRET" --from-file=authorized_keys="$KEYDIR/id.pub" >/dev/null
echo "[case-21] creating CPU-only SSH Instance ${INST} on ${IT}"
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: Instance
metadata: { name: ${INST}, namespace: default }
spec:
  type: ${IT}
  image: ubuntu:24.04
  command: ["sleep", "infinity"]
  resources:
    cpu: "1"
    ram: "2Gi"
    localStorage: "10Gi"
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
  record FAIL "CPU-only SSH Instance reaches 2/2 Running" "${POD} not Running — check Kueue admission"
  print_and_exit
fi
record PASS "CPU-only SSH Instance reaches 2/2 Running" "${POD} main+sshd Running"

# 3. Open a port-forward to the sshd sidecar for the SSH assertions below.
kubectl -n default port-forward "pod/$POD" "${LOCAL_PORT}:22" >/dev/null 2>&1 &
PF_PID=$!
for _ in $(seq 1 20); do (exec 3<>"/dev/tcp/127.0.0.1/${LOCAL_PORT}") 2>/dev/null && { exec 3>&- 3<&-; break; }; sleep 1; done

SSH_OPTS=(-p "$LOCAL_PORT" -i "$KEYDIR/id" -o IdentitiesOnly=yes
  -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes -o ConnectTimeout=15)

# 4. Exec channel: `ssh host '<cmd>'` must run the command in `main`, capability-stripped, no banner.
#    The whole command rides $SSH_ORIGINAL_COMMAND; a stock server runs it, the old chroot.sh discarded
#    it and launched an interactive login shell instead (so stdout came back empty).
EXEC_CMD='echo MARK='"$MARK"'; grep -E "^Cap(Eff|Bnd):" /proc/self/status; mknod /tmp/case21blk b 8 0 2>/dev/null; echo MKNOD_RC=$?'
exec_stdout=$(ssh -T "${SSH_OPTS[@]}" root@127.0.0.1 "$EXEC_CMD" 2>"$KEYDIR/exec.err")
exec_stderr=$(cat "$KEYDIR/exec.err" 2>/dev/null)

exec_capeff=$(printf '%s\n' "$exec_stdout" | awk '/^CapEff:/{print $2}')
exec_capbnd=$(printf '%s\n' "$exec_stdout" | awk '/^CapBnd:/{print $2}')
exec_mknod=$(printf '%s\n' "$exec_stdout" | sed -n 's/^MKNOD_RC=//p')

if printf '%s\n' "$exec_stdout" | grep -q "MARK=${MARK}"; then
  record PASS "non-interactive command runs in main" "ssh host '<cmd>' returned its output (MARK present)"
else
  record FAIL "non-interactive command runs in main" "command discarded — no marker in stdout (interactive shell launched instead)"
fi
if printf '%s\n' "$exec_stdout" "$exec_stderr" | grep -q 'System information as of'; then
  record FAIL "no banner on the exec channel" "login banner leaked into a non-interactive stream"
else
  record PASS "no banner on the exec channel" "clean stdout+stderr (banner suppressed for command exec)"
fi
if [ "$exec_capeff" = "0000000000000000" ] && [ "$exec_capbnd" = "0000000000000000" ]; then
  record PASS "exec path is capability-stripped" "CapEff=${exec_capeff} CapBnd=${exec_capbnd}"
else
  record FAIL "exec path is capability-stripped" "CapEff='${exec_capeff:-?}' CapBnd='${exec_capbnd:-?}', want all-zero"
fi
if [ "$exec_mknod" = "1" ]; then
  record PASS "exec path cannot mknod a host device" "mknod rc=1 (EPERM, denied)"
else
  record FAIL "exec path cannot mknod a host device" "mknod rc='${exec_mknod:-?}', want 1 (EPERM — 127 would mean mknod absent, not denied)"
fi

# 5. Interactive path (regression): no command, stdin over a heredoc → banner + login shell, unchanged.
int_out=$(ssh -T "${SSH_OPTS[@]}" root@127.0.0.1 2>&1 <<'CMDS'
echo INT_MARK=__case21_int__
exit
CMDS
)
if printf '%s\n' "$int_out" | grep -q 'System information as of' && printf '%s\n' "$int_out" | grep -q 'INT_MARK=__case21_int__'; then
  record PASS "interactive login still prints banner + runs shell" "banner present and login shell ran the input"
else
  record FAIL "interactive login still prints banner + runs shell" "banner or login-shell output missing — interactive path regressed"
fi

# 6. SFTP subsystem: `sftp` requests the Subsystem, which under ForceCommand reaches chroot.sh with
#    $SSH_ORIGINAL_COMMAND set to the configured sftp-server path. chroot.sh stages the bundled static
#    sftp-server into `main` and serves it there (not the musl sidecar), so a put/get round-trips and the
#    uploaded file is visible in `main`'s own filesystem. The old chroot.sh discarded the subsystem request.
SFTP_OPTS=(-P "$LOCAL_PORT" -i "$KEYDIR/id" -o IdentitiesOnly=yes
  -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes -o ConnectTimeout=15)
SFTP_MARK=__case21_sftp__
printf '%s\n' "$SFTP_MARK" >"$KEYDIR/up.txt"
sftp "${SFTP_OPTS[@]}" root@127.0.0.1 >/dev/null 2>&1 <<SFTP
put $KEYDIR/up.txt /workspace/case21-sftp.txt
get /workspace/case21-sftp.txt $KEYDIR/down.txt
SFTP
sftp_seen=$(ssh -T "${SSH_OPTS[@]}" root@127.0.0.1 'cat /workspace/case21-sftp.txt' 2>/dev/null)
if [ -f "$KEYDIR/down.txt" ] && diff -q "$KEYDIR/up.txt" "$KEYDIR/down.txt" >/dev/null 2>&1; then
  record PASS "sftp subsystem round-trips a file" "put/get returned identical bytes via the bundled sftp-server"
else
  record FAIL "sftp subsystem round-trips a file" "sftp put/get did not round-trip — subsystem discarded (old chroot.sh)"
fi
if printf '%s\n' "$sftp_seen" | grep -qF "$SFTP_MARK"; then
  record PASS "sftp writes into main's filesystem" "uploaded file readable in main at /workspace"
else
  record FAIL "sftp writes into main's filesystem" "uploaded file not visible in main — sftp served the sidecar, not the target"
fi

# 7. Loopback TCP port-forward: forward a local port through the Instance to the in-Pod sshd (:22) and
#    read its protocol greeting back. Proves TCP forwarding round-trips (direct-tcpip, ForceCommand-agnostic).
ssh -N -T "${SSH_OPTS[@]}" -o ExitOnForwardFailure=yes \
  -L "127.0.0.1:${FWD_PORT}:127.0.0.1:22" root@127.0.0.1 >/dev/null 2>&1 &
FWD_PID=$!
fwd_greeting=""
for _ in $(seq 1 15); do
  g=$( (exec 3<>"/dev/tcp/127.0.0.1/${FWD_PORT}" && head -c 8 <&3) 2>/dev/null )
  if printf '%s' "$g" | grep -q '^SSH-2.0'; then fwd_greeting="$g"; break; fi
  sleep 1
done
kill "$FWD_PID" 2>/dev/null; FWD_PID=""
kill "$PF_PID" 2>/dev/null; PF_PID=""
if [ -n "$fwd_greeting" ]; then
  record PASS "loopback TCP port-forward round-trips" "forwarded port reached the in-Pod sshd (${fwd_greeting})"
else
  record FAIL "loopback TCP port-forward round-trips" "no SSH greeting through the forward — TCP forwarding may be disabled"
fi

print_and_exit
