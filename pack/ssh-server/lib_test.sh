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

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

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
if [[ $fails -gt 0 ]]; then
  echo "the waits FAILED: ${fails} case(s)"
  exit 1
fi
echo "the waits passed"
