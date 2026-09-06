#!/usr/bin/env bash
# check-crossrefs-selftest.sh — proves check-crossrefs.sh does the half that matters.
#
# The failure that prompted the checker had a real target: the label existed, in the same document,
# on an adjacent topic. So "rule 1 is green on it" is not a bug to fix, it is the whole reason
# rule 2 has to exist -- and a later edit that quietly drops rule 2 would leave a checker that
# reports green on exactly the case it was built for. These cases are what stops that.
#
# Usage: bash .claude/skills/gpustack-operator-docs/scripts/check-crossrefs-selftest.sh [repo-root]
set -euo pipefail

ROOT="${1:-}"
[ -n "$ROOT" ] || ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
CHECK="$ROOT/.claude/skills/gpustack-operator-docs/scripts/check-crossrefs.sh"

MINI="$(mktemp -d)"
trap 'rm -rf "$MINI"' EXIT
TREE="$MINI/tree"

fails=0
pass() { printf 'PASS  %s\n' "$1"; }
fail() { printf 'FAIL  %s\n' "$1"; fails=$((fails + 1)); }

subst() {
  awk -v a="$2" -v b="$3" \
    '{ i = index($0, a); if (i > 0) $0 = substr($0, 1, i - 1) b substr($0, i + length(a)); print }' \
    "$1" > "$1.tmp" && mv "$1.tmp" "$1"
}

build() {
  rm -rf "$TREE"
  mkdir -p "$TREE/specs" "$TREE/docs" "$TREE/.claude/skills" "$TREE/pkg"
  : > "$TREE/README.md"
  : > "$TREE/CLAUDE.md"
  # A citation that lands where it should, and whose figure the target does state.
  cat > "$TREE/specs/good.md" <<'EOF'
# Spec: Good

The gauge covers two of the tiers, which is measured below (F6).

#### F6 - status.capacity with two tiers

Two independent problems, and the second is a design gap.

#### F7 - something else
EOF
  # The shape of the known failure: F6 exists, shares the vocabulary, and never says "five".
  cat > "$TREE/specs/figure.md" <<'EOF'
# Spec: Figure

It silently covers only two of the five replica types, which is measured below (F6) and
makes the operation's own success signal untrustworthy.

#### F6 - status.capacity with two tiers

Two independent problems, and the second is a design gap.

#### F7 - something else
EOF
  # A pointer with no target anywhere in the file.
  cat > "$TREE/specs/dangling.md" <<'EOF'
# Spec: Dangling

The allocatable path is covered by the capacity-key case (Z9).
EOF
}

run() {
  set +o errexit
  out="$(bash "$CHECK" "$TREE" 2>&1)"
  rc=$?
  set -o errexit
}

echo "=== rule 1 fires on a pointer that leads nowhere ==="
build
run
if printf '%s' "$out" | grep -q 'dangling.md:3: (Z9) appears on no other line'; then
  pass "(Z9) is reported"
else
  fail "(Z9) is not reported -- got: $out"
fi

echo
echo "=== rule 1 stays quiet on the pointers that do land ==="
if printf '%s' "$out" | grep -q 'good.md.*lands nowhere'; then
  fail "good.md's (F6) was wrongly reported"
else
  pass "good.md's (F6) is not reported"
fi
if printf '%s' "$out" | grep -q 'figure.md.*lands nowhere'; then
  fail "figure.md's (F6) was wrongly reported by rule 1"
else
  pass "figure.md's (F6) is not reported by rule 1 -- the label DOES exist, which is the trap"
fi

echo
echo "=== rule 2 fires on the known shape, and only on it ==="
if printf '%s' "$out" | grep -q 'figure.md:3: cites (F6) for a claim counting "five"'; then
  pass "a claim counting \"five\" against a section that never says it is reported"
else
  fail "rule 2 missed the known shape -- got: $out"
fi
if printf '%s' "$out" | grep -q 'good.md.*cites (F6)'; then
  fail "good.md was reported although its section does state \"two\""
else
  pass "good.md is quiet -- rule 2 keys on the figure, not on the citation"
fi

echo
echo "=== rule 2 goes quiet when the section does state the figure ==="
subst "$TREE/specs/figure.md" "two of the five replica" "two of the two replica"
run
if printf '%s' "$out" | grep -q 'figure.md:3: cites (F6)'; then
  fail "still reported after the figure was changed to one the section states"
else
  pass "changing \"five\" to \"two\" removes the finding"
fi

echo
echo "=== rule 2 reports, it does not gate ==="
build
rm "$TREE/specs/dangling.md"
run
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q 'cites (F6) for a claim counting "five"'; then
  pass "exit 0 with a prompt outstanding, and the summary says what green does not establish"
else
  fail "expected exit 0 with the prompt still printed; rc=$rc out=$out"
fi

echo
echo "=== Go comments are scanned, not skipped ==="
build
rm "$TREE/specs/dangling.md"
mkdir -p "$TREE/pkg/thing"
printf 'package thing\n\n// The master rewrites its own copy on every admin-API change (F6).\nfunc Thing() {}\n' \
  > "$TREE/pkg/thing/thing.go"
run
if printf '%s' "$out" | grep -q 'thing.go:3: (F6) names no document'; then
  pass "a label in a Go comment is reported as naming no document"
else
  fail "the Go comment reference was not reported -- got: $out"
fi

echo
if [ "$fails" -gt 0 ]; then
  echo "SELFTEST FAILED: $fails case(s)."
  exit 1
fi
echo "SELFTEST PASSED: rule 1 gates, rule 2 catches what rule 1 cannot, and neither claims the other's ground."
