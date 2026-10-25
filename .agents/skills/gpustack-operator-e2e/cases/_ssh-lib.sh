#!/usr/bin/env bash
#
# _ssh-lib.sh — how the runner reaches an SSH-enabled Instance: the keypair and its Secret, a local
# port in front of the Instance's sshd, and the sessions themselves.
#
# NOT A CASE. The leading underscore keeps it out of the `case-N.sh` namespace: it carries no case
# header, no trap and no results table. Every one of those stays with the case that sources it, so a
# case still reads end to end on its own terms.
#
# A case uses it as:
#
#     # shellcheck source=/dev/null
#     . "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_ssh-lib.sh"
#     ssh_setup_key_secret default "$SECRET" "$KEYDIR/id"
#     ssh_open_port_forward default "pod/$POD" 22 "$LOCAL_PORT"
#     out="$(ssh_exec "$KEYDIR/id" "$LOCAL_PORT" 'cat /etc/hostname')"
#     ssh_cleanup
#
# The temporary key and the local port are the caller's: every case brings its own, so nothing here
# is state two cases share.
# ------------------------------------------------------------------------------------------------

# The pids of the forwards this harness opened, and the options every session in it uses. The port
# goes in as -o Port= rather than as -p, so that ssh and sftp take one list.
PF_PID=""
FWD_PID=""
SSH_OPTS=()

_ssh_opts() {
  SSH_OPTS=(-o "Port=$2" -i "$1" -o IdentitiesOnly=yes
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes -o ConnectTimeout=15)
}

# ssh_setup_key_secret <ns> <secret-name> <key-path> — write an ed25519 keypair at <key-path> and
# publish its public half as the authorized_keys of the Secret <secret-name> in namespace <ns>.
ssh_setup_key_secret() {
  local ns="$1" secret="$2" key="$3"
  ssh-keygen -t ed25519 -f "$key" -N "" -q
  kubectl -n "$ns" delete secret "$secret" --ignore-not-found >/dev/null 2>&1
  kubectl -n "$ns" create secret generic "$secret" --from-file=authorized_keys="${key}.pub" >/dev/null
}

# ssh_open_port_forward <ns> <target> <remote-port> <local-port> — forward a local port to the
# Instance's sshd through kubectl, and wait until the local port accepts a TCP connection.
ssh_open_port_forward() {
  local ns="$1" target="$2" remote_port="$3" local_port="$4"
  kubectl -n "$ns" port-forward "$target" "${local_port}:${remote_port}" >/dev/null 2>&1 &
  PF_PID=$!
  for _ in $(seq 1 20); do
    (exec 3<>"/dev/tcp/127.0.0.1/${local_port}") 2>/dev/null && { exec 3>&- 3<&-; break; }
    sleep 1
  done
}

# ssh_exec <key-path> <local-port> [command...] — run a command on the Instance over SSH. With no
# command, the session reads the caller's stdin, which is how a login shell is driven.
ssh_exec() {
  local key="$1" port="$2"
  shift 2
  _ssh_opts "$key" "$port"
  ssh -T "${SSH_OPTS[@]}" root@127.0.0.1 "$@"
}

# ssh_sftp <key-path> <local-port> — an sftp session on the Instance; the batch commands come from
# the caller's stdin.
ssh_sftp() {
  local key="$1" port="$2"
  _ssh_opts "$key" "$port"
  sftp "${SSH_OPTS[@]}" root@127.0.0.1
}

# ssh_open_loopback_forward <key-path> <local-port> <forward-local-port> <remote-host> <remote-port>
# — forward a local TCP port through the SSH session to a port inside the Instance.
ssh_open_loopback_forward() {
  local key="$1" port="$2" local_port="$3" remote_host="$4" remote_port="$5"
  _ssh_opts "$key" "$port"
  ssh -N -T "${SSH_OPTS[@]}" -o ExitOnForwardFailure=yes \
    -L "127.0.0.1:${local_port}:${remote_host}:${remote_port}" root@127.0.0.1 >/dev/null 2>&1 &
  FWD_PID=$!
}

# ssh_cleanup — stop every forward this harness opened. A forward that already exited needs no kill,
# and its pid is cleared either way, so a second call finds nothing to do.
ssh_cleanup() {
  [ -n "$FWD_PID" ] && kill "$FWD_PID" 2>/dev/null
  FWD_PID=""
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null
  PF_PID=""
  return 0
}
