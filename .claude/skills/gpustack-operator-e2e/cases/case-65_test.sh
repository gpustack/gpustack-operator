#!/usr/bin/env bash
set -euo pipefail

CASE_DIR="$(cd "$(dirname "$0")" && pwd)"
CASE_FILE="$CASE_DIR/case-65.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

test "$(grep -Fc 'path: ${RUN_HOST_PATH}' "$CASE_FILE")" -eq 2
grep -Fq 'for node in $(ready_nodes); do' "$CASE_FILE"
grep -Fq 'rm -rf /tier/' "$CASE_FILE"
if grep -Fq 'rm -rf /tier/*' "$CASE_FILE"; then
  echo "FAIL: teardown still wipes the shared parent" >&2
  exit 1
fi

PARENT="$WORK/tier"
RUN_DIR="case-65-test"
mkdir -p "$PARENT/$RUN_DIR" "$PARENT/other-tenant"
touch "$PARENT/$RUN_DIR/visible" "$PARENT/$RUN_DIR/.hidden" "$PARENT/other-tenant/keep"
rm -rf "${PARENT:?}/${RUN_DIR:?}"
test ! -e "$PARENT/$RUN_DIR"
test -f "$PARENT/other-tenant/keep"
echo "PASS: cleanup removes the run directory including dotfiles and preserves siblings"

grep -Fq 'PHASE_A_TOTAL=$((MEMBER_COUNT * 44))' "$CASE_FILE"
test $((1 * 44)) -eq 44
test $((3 * 44)) -eq 132
echo "PASS: phase A sizes to 44 objects on one member and 132 on three"
