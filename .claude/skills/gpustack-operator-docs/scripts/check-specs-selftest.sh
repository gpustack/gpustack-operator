#!/usr/bin/env bash
# check-specs-selftest.sh — proves check-specs.sh can fail.
#
# Both of its rules pass on every file in this repository, and a check that has never been seen to
# fail is indistinguishable from one that cannot. Each case below builds a throwaway tree, breaks
# exactly one thing, and asserts the finding names it. Nothing in the real tree is touched.
#
# The `go test -run` rule is the one that most needs this: the defect it exists for is "exits 0
# having run nothing", so a checker with the same property would be the second instance of it.
#
# Usage: bash .claude/skills/gpustack-operator-docs/scripts/check-specs-selftest.sh [repo-root]
set -euo pipefail

ROOT="${1:-}"
[ -n "$ROOT" ] || ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
CHECK="$ROOT/.claude/skills/gpustack-operator-docs/scripts/check-specs.sh"

MINI="$(mktemp -d)"
trap 'rm -rf "$MINI"' EXIT
SPEC="$MINI/tree/specs/shipped.md"
BLOCKED="$MINI/tree/specs/blocked.md"

fails=0
pass() { printf 'PASS  %s\n' "$1"; }
fail() { printf 'FAIL  %s\n' "$1"; fails=$((fails + 1)); }

# In-place edits without `sed -i`, whose argument differs between GNU and BSD sed -- this file has
# to run the same on a contributor's macOS and on the ubuntu runner.
setline() { awk -v n="$2" -v t="$3" 'NR == n { print t; next } { print }' "$1" > "$1.tmp" && mv "$1.tmp" "$1"; }
delline() { awk -v n="$2" 'NR != n' "$1" > "$1.tmp" && mv "$1.tmp" "$1"; }
subst() {
  awk -v a="$2" -v b="$3" \
    '{ i = index($0, a); if (i > 0) $0 = substr($0, 1, i - 1) b substr($0, i + length(a)); print }' \
    "$1" > "$1.tmp" && mv "$1.tmp" "$1"
}

# A tree that satisfies both rules: one Shipped spec, one Building spec that states its condition,
# and a Verify command naming a test that exists.
build() {
  rm -rf "${MINI:?}/tree"
  mkdir -p "$MINI/tree/specs" "$MINI/tree/docs" "$MINI/tree/.claude/skills" \
    "$MINI/tree/pkg/thing" "$MINI/tree/pkg/other"
  : > "$MINI/tree/README.md"
  : > "$MINI/tree/CLAUDE.md"
  cat > "$MINI/tree/pkg/thing/thing_test.go" <<'EOF'
package thing

import "testing"

func TestThingWorks(t *testing.T) {}
EOF
  # A second package with tests of its own. Without it, "the name exists somewhere in the tree"
  # and "the package the command names has it" cannot be told apart.
  cat > "$MINI/tree/pkg/other/other_test.go" <<'EOF'
package other

import "testing"

func TestOtherThing(t *testing.T) {}
EOF
  cat > "$SPEC" <<'EOF'
# Spec: Shipped Thing

Status: Shipped
Type: Feature

Verify: `go test ./pkg/thing/ -run TestThingWorks`
EOF
  cat > "$BLOCKED" <<'EOF'
# Spec: Blocked Thing

Status: Building
Blocked on: hardware. It does not move on its own: no code change can lift it.
Type: Feature
EOF
}

run() {
  set +o errexit
  out="$(bash "$CHECK" "$MINI/tree" 2>&1)"
  rc=$?
  set -o errexit
}

expect_hit() {
  local label="$1" needle="$2"
  run
  if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -qF -- "$needle"; then
    pass "$label"
  else
    fail "$label (rc=$rc)
      wanted a finding containing: $needle
      got: $out"
  fi
}

echo "=== positive baseline ==="
build
run
if [ "$rc" -eq 0 ]; then
  pass "a tree that satisfies both rules passes"
else
  fail "the baseline tree should pass, got: $out"
fi

echo
echo "=== Status: the draft-only word is rejected by name ==="
build
setline "$SPEC" 3 "Status: Specified"
expect_hit "Specified is named in the message" "the draft-only word"

echo
echo "=== Status: a word outside the list ==="
build
setline "$SPEC" 3 "Status: Done"
expect_hit "an unknown word is rejected" "Status is 'Done'"

echo
echo "=== Status: the line is not line 3 ==="
build
setline "$SPEC" 3 "Type: Feature"
expect_hit "a missing Status line is caught" "line 3 is not a 'Status:' line"

echo
echo "=== Status: a legal word that is wrong on main, with no condition under it ==="
build
setline "$SPEC" 3 "Status: Built"
expect_hit "Built with no Blocked on block is caught" "must be a 'Blocked on:' block"

echo
echo "=== Status: the escape is load-bearing ==="
build
delline "$BLOCKED" 4
expect_hit "removing the Blocked on line turns Building red" "must be a 'Blocked on:' block"

echo
echo "=== go test -run: a pattern that selects nothing ==="
build
subst "$SPEC" "TestThingWorks" "TestThingWasRenamed"
expect_hit "a renamed test is caught" "selects no test in ./pkg/thing/"

echo
echo "=== go test -run: a real test name, but not in the package the command names ==="
build
subst "$SPEC" "./pkg/thing/ " "./pkg/other/ "
expect_hit "package scope is honoured, not just the name" "selects no test in ./pkg/other/"

echo
echo "=== go test -run: a command whose inline-code span wraps to the next line ==="
build
cat >> "$SPEC" <<'EOF'

Verify: `go test ./pkg/thing/ -run
TestThingWasRenamed`; against a fake client, a second pass issues no update.
EOF
expect_hit "a wrapped command is read, not skipped" "selects no test in ./pkg/thing/"

echo
echo "=== go test -run: two -run flags is its own finding, not 'selects nothing' ==="
build
cat >> "$SPEC" <<'EOF'

Verify: `go test ./pkg/thing/ -run TestThingWorks ./pkg/other/ -run TestOtherThing`
EOF
expect_hit "a second -run is reported as itself" "carries 2 -run flags"

echo
echo "=== go test -run: wrapped prose above a command does not silence it ==="
# The regression this guards is real, and it was measured: a version of this checker tracked "a
# span is open" across lines, one mispaired span offset every span after it, and a whole spec
# produced no findings at all -- a checker that had gone silent while still exiting 0.
#
# The shape below is what reproduces it, and each part is load-bearing: the prose wraps inline code
# several times AND ends on a DANGLING opening backtick, so the stale span swallows the Verify line
# that follows. Without that dangling backtick the parity happens to come out even and the buggy
# version passes too -- this fixture was checked against it, and it is the difference between a
# guard and a decoration.
build
cat >> "$SPEC" <<'EOF'

The `follow-on
that owns it` is named, and a `kubectl
patch` is refused. See `here

Verify: `go test ./pkg/thing/ -run TestThingWasRenamed`
EOF
expect_hit "a command after wrapped prose is still evaluated" "selects no test in ./pkg/thing/"

echo
echo "=== go test -run: NOT RE-RUNNABLE exempts, and says so out loud ==="
# Issue 179 asks for exactly this where the test is gone and nothing replaced it: say so in words
# rather than delete the line, because deleting it deletes the coverage claim too.
build
subst "$SPEC" "TestThingWorks" "TestThingWasDeletedByT12"
cat >> "$SPEC" <<'EOF'

⛔ **THIS IS NOT RE-RUNNABLE, BY DESIGN.** The oracle was temporary and T12 deleted it.
EOF
run
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q 'exempt:.*TestThingWasDeletedByT12'; then
  pass "the marked command passes, and the exemption is printed rather than silent"
else
  fail "expected exit 0 with a printed exemption; rc=$rc out=$out"
fi

echo
echo "=== go test -run: the exemption window has an edge, at the checker's own constant ==="
# Without a bound, one marker anywhere in a spec would exempt every command in it. The filler count
# is READ OUT OF THE CHECKER rather than hard-coded: a hand-tuned 14 would keep passing if the
# window were ever widened, and the case that tests the edge would quietly stop testing it.
WINDOW=$(awk 'match($0, /k <= at \+ [0-9]+/) { s = substr($0, RSTART, RLENGTH); sub(/.*\+ /, "", s); print s; exit }' "$CHECK")
if [ -z "$WINDOW" ]; then
  fail "could not read the exemption window out of $CHECK — this case cannot test an edge it cannot locate"
else
  build
  subst "$SPEC" "TestThingWorks" "TestThingWasDeletedByT12"
  i=0
  while [ "$i" -lt "$WINDOW" ]; do printf '%s\n' "filler prose here." >> "$SPEC"; i=$((i + 1)); done
  printf '**THIS IS NOT RE-RUNNABLE, BY DESIGN.**\n' >> "$SPEC"
  expect_hit "a marker just past the ${WINDOW}-line window does not reach the command" "selects no test in ./pkg/thing/"

  build
  subst "$SPEC" "TestThingWorks" "TestThingWasDeletedByT12"
  i=1
  while [ "$i" -lt "$WINDOW" ]; do printf '%s\n' "filler prose here." >> "$SPEC"; i=$((i + 1)); done
  printf '**THIS IS NOT RE-RUNNABLE, BY DESIGN.**\n' >> "$SPEC"
  run
  if [ "$rc" -eq 0 ]; then
    pass "a marker inside the ${WINDOW}-line window still exempts"
  else
    fail "the marker at the window boundary did not exempt: $out"
  fi
fi

echo
echo "=== go test -run: a package this check cannot resolve is reported, not assumed ==="
# Falling back to "the whole tree" was a false-green path: a stale name that exists anywhere in the
# repository would have passed.
build
subst "$SPEC" "./pkg/thing/ " "github.com/org/repo/pkg/thing/ "
expect_hit "a dotted import path is reported as unevaluable" "package arguments cannot be resolved"

build
subst "$SPEC" "./pkg/thing/ " ""
expect_hit "a command naming no package is reported as unevaluable" "package arguments cannot be resolved"

echo
echo "=== no command is exempted by a marker the document does not contain ==="
# The sharp form of the field-alignment bug. awk emits one row per command; `read` with a tab IFS
# treats tab as IFS WHITESPACE, so an empty column collapses and every later field shifts left.
# With an empty pkgs column the "unresolvable" flag landed in "exempt", and the checker excused the
# command as NOT RE-RUNNABLE although nothing in the file says so. A silent false exemption is
# worse than a false failure: it is a hole that reads as a pass.
#
# Asserting "the exemption still works" does NOT catch this -- under the bug the shifted field
# happens to be 1, so that assertion passes either way, which is how it was caught here. What
# discriminates is the ABSENCE of an exemption when the document carries no marker at all.
build
subst "$SPEC" "./pkg/thing/ " ""
run
if printf '%s' "$out" | grep -q 'exempt:'; then
  fail "a command was exempted although the spec contains no NOT RE-RUNNABLE marker -- got: $out"
else
  pass "no marker in the file, no exemption"
fi

echo
echo "=== Status: a 'Blocked on:' with nothing after it does not satisfy rule 2 ==="
build
setline "$BLOCKED" 4 "Blocked on:   "
expect_hit "an empty condition is caught" "must be a 'Blocked on:' block"

echo
echo "=== the corpus reports a path that is not a file, rather than skipping it ==="
build
mkdir -p "$MINI/tree/docs"
printf '# Doc\n' > "$MINI/tree/docs/a doc with spaces.md"
run
if printf '%s' "$out" | grep -q 'which is not a file'; then
  pass "a space-containing path is reported, not silently skipped"
else
  fail "the split path was skipped silently -- got: $out"
fi

echo
echo "=== go test -run: prose naming the flag is not a command ==="
build
# shellcheck disable=SC2016  # the backticks are markdown inline code, and must not expand
printf '\nA Verify line is worth little because `go test -run` exits 0 on an empty selection,\nand re-running e2e case-5 does not change that.\n' >> "$SPEC"
run
if [ "$rc" -eq 0 ]; then
  pass "prose mentioning -run, and the -run inside \"re-run\", are not read as commands"
else
  fail "prose was read as a command: $out"
fi

echo
if [ "$fails" -gt 0 ]; then
  echo "SELFTEST FAILED: $fails case(s)."
  exit 1
fi
echo "SELFTEST PASSED: every rule was seen to fail on the shape it claims to catch."
