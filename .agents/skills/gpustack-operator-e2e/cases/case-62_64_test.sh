#!/usr/bin/env bash
set -euo pipefail

CASE_DIR="$(cd "$(dirname "$0")" && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

extract_function() {
  local name="$1" source="$2" destination="$3"
  awk -v signature="${name}() {" '
    $0 == signature { capture=1 }
    capture { print }
    capture && /^}$/ { exit }
  ' "$source" >"$destination"
  test -s "$destination"
}

for case_name in case-62 case-64; do
  case_file="$CASE_DIR/${case_name}.sh"
  function_file="$WORK/${case_name}-holder.sh"
  observation_file="$WORK/${case_name}-observation.sh"
  bound_file="$WORK/${case_name}-bound.sh"
  extract_function holder_pod_uid "$case_file" "$function_file"
  extract_function observe_handoff_snapshot "$case_file" "$observation_file"
  extract_function record_failover_bound "$case_file" "$bound_file"

  (
    export NS=test LEADER_SEL=test
    POD_ROWS=$'old|10.0.0.1|deleting|Running\nnew|10.0.0.1||Running'
    # shellcheck disable=SC2317
    kubectl() { printf '%s\n' "$POD_ROWS"; }
    # shellcheck source=/dev/null
    source "$function_file"

    test "$(holder_pod_uid '10.0.0.1:50051')" = new
    POD_ROWS='ipv6|fd00::1||Running'
    test "$(holder_pod_uid '[fd00::1]:50051')" = ipv6
    test "$(holder_pod_uid 'fd00::1:50051')" = ipv6
  )

  # shellcheck disable=SC2034,SC2317
  (
    # shellcheck source=/dev/null
    source "$observation_file"
    # shellcheck source=/dev/null
    source "$bound_file"

    reset_observation() {
      OLD_HOLDER='10.0.0.1:50051'
      OLD_READY_UID=old-uid
      NEW_HOLDER="$OLD_HOLDER"
      NEW_HOLDER_POD_UID=new-uid
      NEW_RENEW_TIME=renew-1
      NEW_READY_UID=''
      REUSED_HOLDER_POD_UID=''
      REUSED_HOLDER_RENEW_TIME=''
      HOLDER_MOVED_AT=''
      MOVED_HOLDER=''
      MOVED_HOLDER_POD_UID=''
      MOVE_EVIDENCE=''
      HANDOFF_OBSERVED=0
    }

    reset_observation
    observe_handoff_snapshot 105
    test -z "$HOLDER_MOVED_AT"
    test "$HANDOFF_OBSERVED" = 0
    test "$REUSED_HOLDER_POD_UID" = new-uid
    test "$REUSED_HOLDER_RENEW_TIME" = renew-1

    NEW_RENEW_TIME=renew-2
    observe_handoff_snapshot 110
    test "$HOLDER_MOVED_AT" = 110
    test "$HANDOFF_OBSERVED" = 0
    NEW_READY_UID=new-uid
    observe_handoff_snapshot 130
    test "$HOLDER_MOVED_AT" = 110
    test "$HANDOFF_OBSERVED" = 1

    CAPTURED_STATUS=''
    CAPTURED_OBJECT=''
    record() { CAPTURED_STATUS="$1"; CAPTURED_OBJECT="$3"; }
    DELETE_EPOCH=0
    FAILOVER_BOUND_SECS=120

    reset_observation
    NEW_READY_UID=new-uid
    observe_handoff_snapshot 130
    test "$HOLDER_MOVED_AT" = 130
    record_failover_bound
    test "$CAPTURED_STATUS" = SKIP
    test "$CAPTURED_OBJECT" = '130s after the failover trigger; the Ready replacement upper bound includes Pod scheduling, image pulling, and readiness probing'

    reset_observation
    NEW_HOLDER='10.0.0.2:50051'
    NEW_READY_UID=new-uid
    observe_handoff_snapshot 120
    test "$HOLDER_MOVED_AT" = 120
    test "$MOVE_EVIDENCE" = 'holderIdentity changed'
    record_failover_bound
    test "$CAPTURED_STATUS" = PASS
    test "$CAPTURED_OBJECT" = '120s <= 120s; holderIdentity changed'

    reset_observation
    HOLDER_MOVED_AT=1
    MOVE_EVIDENCE=unrecognized
    record_failover_bound
    test "$CAPTURED_STATUS" = FAIL
    test "$CAPTURED_OBJECT" = 'unrecognized move evidence: unrecognized'

    reset_observation
    NEW_HOLDER='10.0.0.2:50051'
    NEW_READY_UID=new-uid
    observe_handoff_snapshot 121
    test "$HOLDER_MOVED_AT" = 121
    test "$MOVE_EVIDENCE" = 'holderIdentity changed'
    record_failover_bound
    test "$CAPTURED_STATUS" = FAIL
    test "$CAPTURED_OBJECT" = '121s > 120s -- holderIdentity changed; the election still works but no longer meets the failover budget'
  )

  # shellcheck disable=SC2016
  grep -Fq 'OLD_HOLDER_POD_UID="$(holder_pod_uid "$OLD_HOLDER")"' "$case_file"
  # shellcheck disable=SC2016
  grep -Fq '[ "$OLD_HOLDER_POD_UID" != "$OLD_READY_UID" ]' "$case_file"
  grep -Fq 'for ((i = 0; i < 10; i++)); do' "$case_file"
  # shellcheck disable=SC2016
  grep -Fq '[ "$NEW_HOLDER_POD_UID" = "$NEW_READY_UID" ]' "$case_file"
  # shellcheck disable=SC2016
  grep -Fq 'if [ -n "$NEW_HOLDER_POD_UID" ] && [ "$NEW_HOLDER_POD_UID" != "$OLD_READY_UID" ]' "$case_file"
  # shellcheck disable=SC2016
  grep -Fq '[ "$NEW_HOLDER" != "$OLD_HOLDER" ]' "$case_file"
  # shellcheck disable=SC2016
  grep -Fq '[ "$NEW_RENEW_TIME" != "$REUSED_HOLDER_RENEW_TIME" ]' "$case_file"
  # shellcheck disable=SC2016
  grep -Fq 'HANDOFF_DEADLINE=$((DELETE_EPOCH + 240))' "$case_file"
  grep -Fq 'the new Lease holder is the Ready replica' "$case_file"
  grep -Fq 'case "$MOVE_EVIDENCE" in' "$case_file"
  grep -Fq '"holderIdentity changed"|"replacement renewed the reused holderIdentity")' "$case_file"
  grep -Fq '"Ready replacement confirmed the reused holderIdentity")' "$case_file"
  grep -Fq 'record SKIP "the Lease moves within the failover bound"' "$case_file"
  if grep -Fq '"240s after' "$case_file"; then
    echo "FAIL: ${case_name} still reports the loop counter as wall time" >&2
    exit 1
  fi
  # shellcheck disable=SC2016
  grep -Fq 'if DELETE_OUT="$(kubectl' "$case_file"
  echo "PASS: ${case_name} executes failover evidence and bound decisions against wall-clock snapshots"
done
