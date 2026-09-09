#!/usr/bin/env bash
set -euo pipefail

CASE_DIR="$(cd "$(dirname "$0")" && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

for case_name in case-62 case-64; do
  case_file="$CASE_DIR/${case_name}.sh"
  function_file="$WORK/${case_name}-holder.sh"
  awk '
    /^holder_pod_uid\(\) \{/ { capture=1 }
    capture { print }
    capture && /^}$/ { exit }
  ' "$case_file" >"$function_file"
  test -s "$function_file"

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
  grep -Fq 'no separate Lease movement timestamp was observed' "$case_file"
  if grep -Fq '"240s after' "$case_file"; then
    echo "FAIL: ${case_name} still reports the loop counter as wall time" >&2
    exit 1
  fi
  # shellcheck disable=SC2016
  grep -Fq 'if DELETE_OUT="$(kubectl' "$case_file"
  echo "PASS: ${case_name} separates Lease movement from readiness and uses a wall-clock deadline"
done
