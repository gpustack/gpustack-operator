#!/usr/bin/env bash
# pack/ssh-server/lib.sh - the waits the SSH image contract needs.
#
# Everything this contract starts is started asynchronously. A container's entrypoint writes
# its configuration and its messages after `docker run -d` returns; a detached exec binds a
# listener after `docker exec -d` returns; a backgrounded ssh opens its forward after it is
# forked. A fixed sleep is the only thing standing between the start and the assertion that
# depends on it, so a slow start is reported as whatever assertion happened to run next: a
# forwarding failure for a listener that never bound, an authentication failure for a sidecar
# that never listened.
#
# wait_for replaces the sleep with a poll that stops on its own and names what it was waiting
# for. The SSH daemon exposes no readiness endpoint, so a poll is the strongest signal
# available, and a poll that gives up is what keeps a broken start from running on into
# assertions that cannot say what went wrong.
#
# The predicates below are the conditions this contract waits for. Each one answers a yes or a
# no about state that already exists, so each one can be polled.
#
# Sourced by test.sh. The helper is testable on its own: see lib_test.sh.

# wait_for <what> <timeout-seconds> <command...>
# Runs <command> once a second until it succeeds or the timeout has passed, and returns 0 when
# the command succeeded. On a timeout it writes one line naming what was waited for to stderr
# and returns 1. The command is run at least once, so a timeout of zero still asks the
# question once. The timeout is how long the wait looks for the condition; an attempt that
# takes longer than that is still waited out.
function wait_for() {
  local what="$1" timeout="$2"
  shift 2
  local deadline=$((SECONDS + timeout))
  while :; do
    if "$@" > /dev/null 2>&1; then
      return 0
    fi
    if ((SECONDS >= deadline)); then
      break
    fi
    sleep 1
  done
  echo "gave up after ${timeout}s waiting for ${what}" >&2
  return 1
}

# port_accepts <port>: whether a connection to the loopback port is accepted. The connection is
# made in a subshell, so the descriptor it opens closes with the subshell.
function port_accepts() {
  local port="$1"
  (exec 3<>/dev/tcp/127.0.0.1/"$port") 2> /dev/null
}

# container_has_output <container>: whether the container has produced any output. The
# entrypoint writes its configuration before it starts the daemon, and the daemon is what
# writes the output, so output at all means the entrypoint has finished what it writes.
function container_has_output() {
  local container="$1"
  local output
  output="$(docker logs "$container" 2>&1 || true)"
  [[ -n "$output" ]]
}

# container_listens_on <container> <port>: whether the container's network namespace has a
# listener on <port>. Containers in the shared namespace see the same sockets, so the
# workload's listener is visible from the container that holds the namespace.
#
# This reads the namespace through an exec, which is a process in the shared PID namespace.
# That is safe here and only here: the workload's own detection in chroot.sh picks a process
# whose parent is init, so an exec run while a session channel is opening could be picked
# instead of the workload. Every session channel in the contract has already finished by the
# time this is called.
function container_listens_on() {
  local container="$1" port="$2"
  local listeners
  listeners="$(docker exec "$container" netstat -tln 2>/dev/null || true)"
  [[ "$listeners" == *":${port} "* ]]
}
