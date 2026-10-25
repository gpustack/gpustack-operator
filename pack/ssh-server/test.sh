#!/usr/bin/env bash
# pack/ssh-server/test.sh - the runtime contract of the SSH server image.
#
# The image is not a shell in a box. It is an SSH server that runs beside a workload container
# and serves THAT container: the session is forced through /chroot.sh, which finds the workload
# process in the shared PID namespace and enters its mount, network, UTS, IPC and PID namespaces
# before dropping every capability. The SFTP server is a Go program staged into the workload's
# filesystem so it serves the workload's files rather than the sidecar's.
#
# A container that answers an SSH login proves none of that. A login that returns the workload's
# filesystem, an SFTP transfer that lands in the workload's /tmp, and a forwarded connection that
# reaches the workload's listener on the shared Pod network namespace do. Those are the assertions
# here, and they run against the image's own entrypoint, sshd configuration and scripts: the
# contract is what the product does, so the recipe uses the product and never relaxes it.
#
# What the recipe covers:
#
#   image        the runtime version, the sshd version, and the image's architecture
#   sshd config  the effective configuration, including the keywords the image overrides
#   login        an authenticated session that lands in the workload container
#   denial       an invalid key is refused
#   sftp         a byte-exact round trip that lands in the workload container
#   forwarding   a forwarded connection that reaches the workload's listener on the shared Pod
#                network namespace
#   users        the existing user and permission behavior of the entrypoint
#
# The Go build information of the bundled SFTP binary is NOT checked here. The runtime image
# carries no Go, so reading it needs a tool chain from outside the image, and a smoke recipe
# that needs one stops being a recipe that runs anywhere. It is a recorded validation of the
# build, not a property of the runtime, and it belongs beside the build command.
#
# Everything it creates is named after one unique prefix and removed at the end. It writes no
# configuration, no image and no persistent file.
#
# Usage: bash pack/ssh-server/test.sh <image-reference>
set -euo pipefail

if [[ $# -ne 1 || -z "${1:-}" ]]; then
  echo "usage: bash pack/ssh-server/test.sh <image-reference>" >&2
  exit 2
fi

IMAGE="$1"

# The runtime and the SSH daemon the image is expected to carry. Both are the point of the
# upgrade, so both are asserted rather than reported: a recipe that only prints them proves
# nothing. They are overridable so that the same recipe can be run against the image this one
# replaces, which is how the upgrade is shown not to have changed the behavior.
EXPECTED_ALPINE="${EXPECTED_ALPINE:-3.24.3}"
EXPECTED_OPENSSH="${EXPECTED_OPENSSH:-OpenSSH_10.3}"

fails=0
pass() {
  printf 'PASS  %s\n' "$1"
}
fail() {
  printf 'FAIL  %s\n' "$1"
  fails=$((fails + 1))
}

# check <description> <expected> <actual>
function check() {
  if [[ "$2" == "$3" ]]; then
    pass "$1"
  else
    fail "$1 (want [$2], got [$3])"
  fi
}

# check_contains <description> <needle> <haystack>
function check_contains() {
  if [[ "$3" == *"$2"* ]]; then
    pass "$1"
  else
    fail "$1 (want to contain [$2], got [$3])"
  fi
}

function require_tool() {
  if ! command -v "$1" > /dev/null 2>&1; then
    echo "missing required tool: $1" >&2
    exit 2
  fi
}

# sha256_of <file>: one digest for one file, on a host whose tool is named differently.
function sha256_of() {
  if command -v sha256sum > /dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

# free_local_port: a local port for the forwarded listener. The kernel picks it while holding
# the socket, so it cannot be a port another process is already using.
function free_local_port() {
  local port
  port="$(python3 -c 'import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()' 2> /dev/null || true)"
  if [[ -z "$port" ]]; then
    port=$((20000 + RANDOM % 40000))
  fi
  printf '%s' "$port"
}

for tool in docker ssh ssh-keygen sftp curl; do
  require_tool "$tool"
done

# One prefix for every resource this run creates, so the cleanup can name exactly what it made
# and nothing that existed before it.
PREFIX="gpustack-ssh-smoke-$$-$(date +%s)"
PAUSE="${PREFIX}-pause"
TARGET="${PREFIX}-target"
SIDECAR="${PREFIX}-ssh"
AUTHCMD="${PREFIX}-authcmd"
PREEXISTING="${PREFIX}-preexisting"
WORK="$(mktemp -d)"
FORWARD_PID=""

# The workload's marker file. It exists only in the workload container, so reading it from a
# session proves the session landed in the workload rather than in the sidecar.
MARKER_NAME="gpustack-ssh-smoke-marker.txt"
MARKER_PATH="/${MARKER_NAME}"
MARKER_CONTENT="GPUSTACK_SSH_SMOKE_WORKLOAD_MARKER"

function cleanup() {
  local rc=$?
  if [[ -n "$FORWARD_PID" ]]; then
    kill "$FORWARD_PID" > /dev/null 2>&1 || true
    wait "$FORWARD_PID" 2> /dev/null || true
  fi
  # Only the containers this run created. The image itself is the input and is left alone.
  docker rm -f "$PAUSE" "$TARGET" "$SIDECAR" "$AUTHCMD" "$PREEXISTING" > /dev/null 2>&1 || true
  rm -rf "$WORK"
  return $rc
}
trap cleanup EXIT

echo "== the image under test =="
check "an image reference was given" "yes" "$([ -n "$IMAGE" ] && echo yes || echo no)"

image_id="$(docker image inspect "$IMAGE" --format '{{.Id}}' 2> /dev/null || true)"
if [[ -z "$image_id" ]]; then
  echo "no local image named [$IMAGE]" >&2
  exit 1
fi
image_platform="$(docker image inspect "$IMAGE" --format '{{.Os}}/{{.Architecture}}')"
host_platform="$(docker version --format '{{.Server.Os}}/{{.Server.Arch}}')"
echo "image:   $IMAGE"
echo "id:      $image_id"
echo "platform: $image_platform (docker server: $host_platform)"

alpine_release="$(docker run --rm --entrypoint /bin/sh "$IMAGE" -c 'cat /etc/alpine-release')"
check "the runtime is Alpine ${EXPECTED_ALPINE}" "$EXPECTED_ALPINE" "$alpine_release"

sshd_version="$(docker run --rm --entrypoint /bin/sh "$IMAGE" -c '/usr/sbin/sshd -V 2>&1 | head -1')"
check_contains "sshd is ${EXPECTED_OPENSSH}" "$EXPECTED_OPENSSH" "$sshd_version"

# The image is loaded for one platform, so the architecture it was built for has to be the one
# it is about to be run on. A cross-loaded image would fail here rather than in a later check.
check "the image architecture matches the docker server" "$host_platform" "$image_platform"

echo
echo "== the sshd configuration the image ships =="
# The SFTP binary the image ships is copied out so the staged copy can be compared with it.
# Comparing the bytes proves the server that served the transfer is the image's own program
# and not the SSH daemon's internal one.
sftp_bin="${WORK}/gpustack-sftp-server"
sftp_cid="$(docker create "$IMAGE")"
docker cp "${sftp_cid}:/usr/local/bin/gpustack-sftp-server" "$sftp_bin" > /dev/null
docker rm "$sftp_cid" > /dev/null
# The configuration is read from a container that runs the image's own entrypoint, so the
# AuthorizedKeysCommand the entrypoint adds is part of what is read.
authkeys="${WORK}/authorized_keys"
ssh-keygen -q -t ed25519 -N '' -f "${WORK}/id" <<< y > /dev/null 2>&1
cp "${WORK}/id.pub" "$authkeys"
chmod 600 "$authkeys"

docker run -d --name "$AUTHCMD" -e SSH_PUBLIC_KEY="$(cat "$authkeys")" "$IMAGE" > /dev/null
sleep 2

sshd_config="$(docker exec "$AUTHCMD" cat /etc/ssh/sshd_config 2> /dev/null || true)"
check_contains "the image enables TCP forwarding" "AllowTcpForwarding yes" "$sshd_config"
check_contains "the image pins the SFTP subsystem" "Subsystem sftp /usr/lib/ssh/sftp-server" "$sshd_config"
check_contains "the image forces the session through the workload" "ForceCommand /chroot.sh" "$sshd_config"

# sshd honours the first value of a keyword and rejects a duplicate Subsystem, so the shipped
# configuration must carry exactly one of each. A second one is a startup failure, not a warning.
subsystem_count="$(printf '%s\n' "$sshd_config" | grep -cE '^[[:space:]]*Subsystem[[:space:]]+sftp' || true)"
check "exactly one SFTP subsystem is configured" "1" "$subsystem_count"

# The effective configuration is what sshd will act on, which is not the same as the file.
effective="$(docker exec "$AUTHCMD" /usr/sbin/sshd -T 2> /dev/null || true)"
check_contains "password authentication is off" "passwordauthentication no" "$effective"
check_contains "root login by key is allowed" "permitrootlogin yes" "$effective"
check_contains "TCP forwarding is on" "allowtcpforwarding yes" "$effective"
check_contains "stream-local forwarding is on" "allowstreamlocalforwarding yes" "$effective"
check_contains "the session is forced through the workload" "forcecommand /chroot.sh" "$effective"
check_contains "the SFTP subsystem is pinned" "subsystem sftp /usr/lib/ssh/sftp-server" "$effective"
check_contains "the server listens on every interface" "listenaddress 0.0.0.0:22" "$effective"
check_contains "host keys are generated" "hostkey /etc/ssh/ssh_host_ed25519_key" "$effective"

echo
echo "== the topology the image is built for =="
# A pod's containers share a PID namespace and a network namespace. Docker expresses that with
# one container that holds the namespaces and two that join them: the first joined container is
# the workload, the second is the SSH sidecar.
#
# The pause container holds the namespaces AND publishes the port, because the port has to be
# published by the container that owns the network namespace the sidecar ends up in.
#
# The sidecar is given SYS_ADMIN and nothing else. It needs it to cross the namespaces, and
# chroot.sh drops the whole capability set before the workload program starts.
# The marker file exists only in the workload container, so reading it from a session proves
# the session landed in the workload rather than in the sidecar.
printf '%s\n' "$MARKER_CONTENT" > "${WORK}/${MARKER_NAME}"

docker run -d --name "$PAUSE" -p 127.0.0.1::22 --entrypoint /bin/sh "$IMAGE" -c 'exec sleep 3600' > /dev/null
sleep 1
docker run -d --name "$TARGET" \
  --pid="container:$PAUSE" \
  --network="container:$PAUSE" \
  -v "${WORK}/${MARKER_NAME}:${MARKER_PATH}:ro" \
  --entrypoint /bin/sh "$IMAGE" -c 'exec sleep 3600' > /dev/null
docker run -d --name "$SIDECAR" \
  --pid="container:$PAUSE" \
  --network="container:$PAUSE" \
  --cap-add SYS_ADMIN \
  -e SSH_PUBLIC_KEY="$(cat "$authkeys")" \
  "$IMAGE" > /dev/null
sleep 2

SSH_PORT="$(docker port "$PAUSE" 22/tcp 2> /dev/null | head -1 | sed 's/.*://')"
if [[ -z "$SSH_PORT" ]]; then
  echo "the published SSH port could not be read" >&2
  exit 1
fi
echo "ssh port: 127.0.0.1:${SSH_PORT}"

# The sidecar is not ready until it accepts a connection.
for _ in $(seq 1 30); do
  if (exec 3<>/dev/tcp/127.0.0.1/"$SSH_PORT") 2> /dev/null; then
    exec 3<&- 3>&-
    break
  fi
  sleep 1
done

# IdentitiesOnly keeps the client from offering every key it owns: sshd allows a limited
# number of attempts, and an agent full of keys would fail before the right one is offered.
SSH_OPTS=(
  -o StrictHostKeyChecking=no
  -o UserKnownHostsFile=/dev/null
  -o BatchMode=yes
  -o IdentitiesOnly=yes
  -o LogLevel=ERROR
  -o ConnectTimeout=10
)

echo
echo "== an authenticated session lands in the workload container =="
login_out="$(ssh "${SSH_OPTS[@]}" -i "${WORK}/id" -p "$SSH_PORT" root@127.0.0.1 \
  'cat /'"$MARKER_NAME"'; echo "---"; cat /etc/alpine-release; echo "---"; ls /proc | grep -c "^[0-9]"' 2>&1 || true)"
check_contains "the session reads the workload's own file" "$MARKER_CONTENT" "$login_out"
check_contains "the session runs in the workload's Alpine" "$EXPECTED_ALPINE" "$login_out"
session_rc=0
ssh "${SSH_OPTS[@]}" -i "${WORK}/id" -p "$SSH_PORT" root@127.0.0.1 true > /dev/null 2>&1 || session_rc=$?
check "the session succeeds" "0" "$session_rc"

echo
echo "== an invalid key is refused =="
ssh-keygen -q -t ed25519 -N '' -f "${WORK}/wrong" <<< y > /dev/null 2>&1
denied_out="$(ssh "${SSH_OPTS[@]}" -i "${WORK}/wrong" -p "$SSH_PORT" root@127.0.0.1 'echo SHOULD_NOT_RUN' 2>&1 || true)"
denied_rc=0
ssh "${SSH_OPTS[@]}" -i "${WORK}/wrong" -p "$SSH_PORT" root@127.0.0.1 'echo SHOULD_NOT_RUN' > /dev/null 2>&1 || denied_rc=$?
check "the invalid key is refused" "nonzero" "$([ "$denied_rc" -ne 0 ] && echo nonzero || echo zero)"
check_contains "the refusal is reported as a permission failure" "Permission denied" "$denied_out"
if [[ "$denied_out" == *"SHOULD_NOT_RUN"* ]]; then
  fail "the refused session still ran the command"
else
  pass "the refused session ran nothing"
fi

echo
echo "== an SFTP round trip is byte exact and lands in the workload =="
payload="${WORK}/payload.bin"
head -c 200000 /dev/urandom > "$payload"
payload_sha="$(sha256_of "$payload")"
remote_file="/tmp/gpustack-ssh-smoke-payload.bin"

batch="${WORK}/batch.txt"
printf 'put %s %s\n' "$payload" "$remote_file" > "$batch"
printf 'get %s %s\n' "$remote_file" "${WORK}/roundtrip.bin" >> "$batch"
sftp "${SSH_OPTS[@]}" -i "${WORK}/id" -P "$SSH_PORT" -b "$batch" root@127.0.0.1 > /dev/null 2>&1 || true

if [[ -f "${WORK}/roundtrip.bin" ]]; then
  check "the round trip is byte exact" "$payload_sha" "$(sha256_of "${WORK}/roundtrip.bin")"
  if cmp -s "$payload" "${WORK}/roundtrip.bin"; then
    pass "the round trip bytes compare equal"
  else
    fail "the round trip bytes differ"
  fi
else
  fail "the round trip returned no file"
fi

# The file has to be in the workload container, not in the sidecar. The copy is read with
# docker cp rather than a command inside the container: a command would be a process in the
# shared PID namespace, and the workload detection in chroot.sh picks the first such process
# that is not the SSH daemon, so it could be chosen as the session's target instead of the
# workload. The copy has no such effect.
if docker cp "${TARGET}:${remote_file}" "${WORK}/from-workload.bin" > /dev/null 2>&1; then
  check "the file landed in the workload container" "$payload_sha" "$(sha256_of "${WORK}/from-workload.bin")"
else
  fail "the file did not land in the workload container"
fi
if docker cp "${SIDECAR}:${remote_file}" "${WORK}/from-sidecar.bin" > /dev/null 2>&1; then
  fail "the file landed in the sidecar container as well"
else
  pass "the file did not land in the sidecar container"
fi

# The SFTP server the image ships is staged into the workload's filesystem and run there. Its
# presence in the workload and its absence from the sidecar is what makes the transfer serve
# the workload's files instead of the sidecar's.
if docker cp "${TARGET}:/tmp/.gpustack-sftp-server" "${WORK}/staged-server" > /dev/null 2>&1; then
  pass "the bundled SFTP server is staged in the workload container"
  check "the staged SFTP server is the image's binary" "$(sha256_of "$sftp_bin")" \
    "$(sha256_of "${WORK}/staged-server")"
else
  fail "the bundled SFTP server was not staged in the workload container"
fi
if docker cp "${SIDECAR}:/tmp/.gpustack-sftp-server" "${WORK}/staged-sidecar" > /dev/null 2>&1; then
  fail "the bundled SFTP server is also staged in the sidecar container"
else
  pass "the bundled SFTP server is not staged in the sidecar container"
fi

echo
echo "== a forwarded connection reaches the workload's listener on the shared Pod network =="
# The listener runs in the workload container. The forwarding channel is opened by the SSH
# daemon in the sidecar, and both containers live in the pod's shared network namespace, so
# the forward reaches the workload's listener on the loopback address of that namespace. The
# workload's namespace and the sidecar's are one namespace here, so what the forward proves
# is that the channel reaches the workload's listener.
listener_port=9000
docker exec -d "$TARGET" /bin/sh -c \
  "while true; do printf 'HTTP/1.1 200 OK\r\nContent-Length: 9\r\n\r\nWORKLOAD!' | nc -l -p ${listener_port} -s 0.0.0.0 2> /dev/null; done" > /dev/null 2>&1 || true
sleep 2

forward_port="$(free_local_port)"
ssh "${SSH_OPTS[@]}" -i "${WORK}/id" -p "$SSH_PORT" -N \
  -L "127.0.0.1:${forward_port}:127.0.0.1:${listener_port}" root@127.0.0.1 > /dev/null 2>&1 &
FORWARD_PID=$!
sleep 3

forward_out="$(curl -s --max-time 10 "http://127.0.0.1:${forward_port}/" 2>&1 || true)"
check_contains "the forwarded connection reaches the workload's listener" "WORKLOAD!" "$forward_out"

kill "$FORWARD_PID" > /dev/null 2>&1 || true
wait "$FORWARD_PID" 2> /dev/null || true
FORWARD_PID=""

echo
echo "== the existing user and permission behavior =="
# The entrypoint writes the key from the environment. The key itself is private to its owner,
# and root's home is private to root. The directory that holds the key is left at the mode
# mkdir gives it, which is not asserted: the home directory above it is what makes the path
# unreachable to anyone else.
perms="$(docker exec "$SIDECAR" stat -c '%a %n' /root /root/.ssh /root/.ssh/authorized_keys 2> /dev/null || true)"
check_contains "root's home is private to its owner" "700 /root" "$perms"
check_contains "the authorized key is readable by its owner only" "600 /root/.ssh/authorized_keys" "$perms"

# A key that is already there wins over the environment, and the entrypoint says so.
docker run -d --name "$PREEXISTING" \
  -e SSH_PUBLIC_KEY="$(cat "${WORK}/wrong.pub")" \
  -v "${authkeys}:/root/.ssh/authorized_keys:ro" \
  "$IMAGE" > /dev/null
sleep 2
preexisting_logs="$(docker logs "$PREEXISTING" 2>&1 || true)"
check_contains "a key that is already there wins over the environment" "already exists" "$preexisting_logs"

# The AuthorizedKeysCommand path: the key comes from the mounted file instead of the
# environment, and the entrypoint points sshd at the script that reads it.
docker rm -f "$AUTHCMD" > /dev/null 2>&1 || true
docker run -d --name "$AUTHCMD" \
  -v "${authkeys}:/var/run/sshd-authorized-keys/authorized_keys:ro" \
  "$IMAGE" > /dev/null
sleep 2
cmd_effective="$(docker exec "$AUTHCMD" /usr/sbin/sshd -T 2> /dev/null || true)"
check_contains "the entrypoint points sshd at the authorized keys command" \
  "authorizedkeyscommand /get-authorized-keys.sh" "$cmd_effective"
check_contains "the authorized keys command runs as root" \
  "authorizedkeyscommanduser root" "$cmd_effective"

echo
if [[ $fails -gt 0 ]]; then
  echo "SSH image contract FAILED: ${fails} case(s)"
  exit 1
fi
echo "SSH image contract passed"
