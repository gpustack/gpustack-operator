#!/usr/bin/env bash
# pack/ssh-server/lib_test.sh - the focused test for the waits in lib.sh.
#
# Entirely offline: it exercises the wait helper, which is where the contract's failure
# reporting lives. Nothing here needs a container, a daemon or a network.
#
#   bash pack/ssh-server/lib_test.sh
#
# The helper has three ways to end, and the cases below are them: the condition already
# holds, the condition holds after a delay, and the condition never holds. The last one is
# the case that matters most, because a wait that cannot fail is a wait that reports a
# timeout it never detected.
#
# Two edges of the contract are pinned down as well: a timeout of zero still asks the
# question once, and a container counts as ready only when the container itself produced
# output.
set -uo pipefail

# shellcheck source=lib.sh
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
source "${script_dir}/lib.sh"

fails=0

function pass() {
  printf 'PASS  %s\n' "$1"
}

function fail() {
  printf 'FAIL  %s\n' "$1"
  fails=$((fails + 1))
}

function check() {
  if [[ "$2" == "$3" ]]; then
    pass "$1"
  else
    fail "$1 (want [$2], got [$3])"
  fi
}

# check_at_least <description> <limit> <actual>
function check_at_least() {
  if (($3 >= $2)); then
    pass "$1"
  else
    fail "$1 (want at least [$2], got [$3])"
  fi
}

# check_at_most <description> <limit> <actual>
function check_at_most() {
  if (($3 <= $2)); then
    pass "$1"
  else
    fail "$1 (want at most [$2], got [$3])"
  fi
}

# count_lines <file>: how many lines the file holds.
function count_lines() {
  wc -l < "$1" | tr -d '[:space:]'
}

# The conditions the cases poll for.
function always_true() {
  return 0
}

function never_true() {
  return 1
}

function marker_exists() {
  [[ -f "$1" ]]
}

# counted_condition <answer>: a condition that records every time it was asked and then
# answers as it was told to. The record is what says how many times the wait asked.
function counted_condition() {
  printf 'asked\n' >> "$questions"
  [[ "$1" == "holds" ]]
}

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
questions="${work}/questions"

echo "== the wait succeeds when the condition already holds =="
start=$SECONDS
if wait_for "a condition that already holds" 10 always_true; then
  pass "the wait succeeds when the condition already holds"
else
  fail "the wait succeeds when the condition already holds"
fi
check_at_most "the wait returns without waiting out its timeout" 1 "$((SECONDS - start))"

echo
echo "== the wait succeeds when the condition holds after a delay =="
marker="${work}/delayed-condition"
( sleep 2; touch "$marker" ) &
delayer=$!
start=$SECONDS
if wait_for "a condition that holds after a delay" 15 marker_exists "$marker"; then
  pass "the wait succeeds when the condition holds after a delay"
else
  fail "the wait succeeds when the condition holds after a delay"
fi
elapsed=$((SECONDS - start))
check_at_least "the wait waited for the delay instead of failing at once" 2 "$elapsed"
check_at_most "the wait returned before its timeout" 14 "$elapsed"
wait "$delayer" 2> /dev/null || true

echo
echo "== the wait fails when the condition never holds =="
start=$SECONDS
if wait_for "a condition that never holds" 3 never_true; then
  fail "the wait fails when the condition never holds"
else
  pass "the wait fails when the condition never holds"
fi
elapsed=$((SECONDS - start))
check_at_least "the wait ran until its timeout" 3 "$elapsed"
check_at_most "the wait did not run past its timeout" 10 "$elapsed"

echo
echo "== the timeout names what was waited for =="
message="$(wait_for "a condition that never holds" 2 never_true 2>&1)"
case "$message" in
  *"a condition that never holds"*)
    pass "the timeout names what was waited for"
    ;;
  *)
    fail "the timeout names what was waited for (got [$message])"
    ;;
esac

echo
echo "== a zero timeout still asks the condition once =="
: > "$questions"
if wait_for "a condition that holds at once" 0 counted_condition holds; then
  pass "a zero timeout succeeds when the condition holds"
else
  fail "a zero timeout succeeds when the condition holds"
fi
check "a zero timeout asks the condition exactly once" "1" "$(count_lines "$questions")"

: > "$questions"
message="$(wait_for "a condition that never holds" 0 counted_condition does-not-hold 2>&1)"
status=$?
check "a zero timeout fails when the condition never holds" "1" "$status"
check "a zero timeout asks the condition exactly once when it fails" "1" \
  "$(count_lines "$questions")"
check "a zero timeout names what was waited for" \
  "gave up after 0s waiting for a condition that never holds" "$message"

echo
echo "== a container is ready only when the container produced output =="
# A stand-in for docker, so the cases need neither a container nor a daemon. The container
# name selects the behavior.
fake_bin="${work}/bin"
mkdir -p "$fake_bin"
cat > "${fake_bin}/docker" <<'FAKE'
#!/usr/bin/env bash
# The container name selects what docker does with it.
case "${2:-}" in
  writes-to-stdout)
    echo "the entrypoint wrote its configuration"
    ;;
  writes-to-stderr)
    echo "the daemon wrote its banner" >&2
    ;;
  writes-nothing) ;;
  *)
    echo "Error: No such container: ${2:-}" >&2
    exit 1
    ;;
esac
exit 0
FAKE
chmod +x "${fake_bin}/docker"
original_path="${PATH}"
export PATH="${fake_bin}:${original_path}"

# The container cases: the container, whether it counts as ready, and what it shows.
container_cases=(
  "writes-to-stdout|ready|the container wrote to stdout"
  "writes-to-stderr|ready|the container wrote to stderr"
  "writes-nothing|not-ready|the container wrote nothing"
  "missing|not-ready|docker could not read the container"
)

for entry in "${container_cases[@]}"; do
  IFS='|' read -r container expected description <<< "$entry"
  if container_has_output "$container"; then
    actual="ready"
  else
    actual="not-ready"
  fi
  check "$description" "$expected" "$actual"
done

PATH="${original_path}"

echo
if [[ $fails -gt 0 ]]; then
  echo "the waits FAILED: ${fails} case(s)"
  exit 1
fi
echo "the waits passed"
