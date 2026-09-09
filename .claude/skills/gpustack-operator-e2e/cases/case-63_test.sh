#!/usr/bin/env bash
set -euo pipefail

CASE_DIR="$(cd "$(dirname "$0")" && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

awk '
  /^python3 - "\$T0" "\$LOG_A" "\$LOG_B" "\$LOG_EP"/ { capture=1; next }
  capture && $0 == "PY" { exit }
  capture { print }
' "$CASE_DIR/case-63.sh" >"$WORK/parser.py"
test -s "$WORK/parser.py"
# shellcheck disable=SC2016
grep -Fq 'if [ -z "$OLD_READY" ]; then' "$CASE_DIR/case-63.sh"
# shellcheck disable=SC2016
grep -Fq 'if DELETE_OUT="$(kubectl' "$CASE_DIR/case-63.sh"

printf '99.000 10.0.0.1\n' >"$WORK/endpoints.log"

verdict() {
  local summary="$1" convergence
  convergence="$(awk '/^Path A/ {for (i=1; i<=NF; i++) if ($i ~ /^convergence=/) print $i}' "$summary" | cut -d= -f2)"
  case "$convergence" in
    *s) printf 'PASS\n' ;;
    *) printf 'FAIL\n' ;;
  esac
}

run_parser() {
  local log="$1" summary="$2"
  python3 "$WORK/parser.py" 100 "$log" "$log" "$WORK/endpoints.log" >"$summary"
}

cat >"$WORK/recovered.log" <<'EOF'
SETUP t=98.000 rc=0
PUT t=99.000 rc=0
PUT t=101.000 rc=0
PUT t=102.000 rc=-1
PUT t=105.000 rc=0
EOF
run_parser "$WORK/recovered.log" "$WORK/recovered.summary"
grep -q '^Path A .*puts total=4 .*first-error-after-t0=2.000 .*first-ok-after-t0=1.000 convergence=5.000s$' "$WORK/recovered.summary"
test "$(verdict "$WORK/recovered.summary")" = PASS
echo "PASS: recovery is the first success after the first post-t0 error"

cat >"$WORK/noop.log" <<'EOF'
SETUP t=98.000 rc=0
PUT t=99.000 rc=0
PUT t=101.000 rc=0
PUT t=104.000 rc=0
EOF
run_parser "$WORK/noop.log" "$WORK/noop.summary"
grep -q '^Path A .*puts total=3 .*first-error-after-t0=none .*first-ok-after-t0=1.000 convergence=NO_ERROR_WINDOW$' "$WORK/noop.summary"
test "$(verdict "$WORK/noop.summary")" = FAIL
grep -Fq 'no failed put was observed after the delete, so recovery was not measured' "$CASE_DIR/case-63.sh"
echo "PASS: a no-op deletion with no error window fails"

cat >"$WORK/exception.log" <<'EOF'
SETUP t=98.000 rc=0
PUT t=99.000 rc=0
PUT t=101.000 rc=0
PUT t=102.000 exc=TimeoutError('leader unavailable')
EOF
run_parser "$WORK/exception.log" "$WORK/exception.summary"
grep -q '^Path A .*puts total=3 .*first-error-after-t0=2.000 .*first-ok-after-t0=1.000 convergence=DID_NOT_RECOVER$' "$WORK/exception.summary"
test "$(verdict "$WORK/exception.summary")" = FAIL
grep -Fq "no successful put followed the first failure within \${DEADLINE}s" "$CASE_DIR/case-63.sh"
echo "PASS: exception-only failure lines are counted and fail without recovery"
