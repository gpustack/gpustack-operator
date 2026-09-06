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
echo "=== rule 1: a word inside the parens does not turn a pointer into a target ==="
# "(see F6)" is still a pointer. Reading only the immediate neighbours classified it as a target,
# which let a genuinely dangling "(F6)" in the same file pass the gate.
build
rm "$TREE/specs/dangling.md"
cat > "$TREE/specs/seeref.md" <<'EOF'
# Spec: See Ref

The classification is measured below (Z9), and the drain path is covered (see Z9).
EOF
run
if printf '%s' "$out" | grep -q 'seeref.md.*(Z9) appears on no other line'; then
  pass "(see Z9) does not count as somewhere (Z9) can land"
else
  fail "(see Z9) was treated as a target, masking the dangling (Z9) -- got: $out"
fi

echo
echo "=== rule 1: a label inside a fenced code block is not a target ==="
build
rm "$TREE/specs/dangling.md"
# shellcheck disable=SC2016  # the backticks are a markdown fence, and must not expand
printf '# Spec: Fenced\n\nThe drain path is covered (Z9).\n\n```go\n// Z9 is a struct field name here, not a section.\ntype Z9 struct{}\n```\n' \
  > "$TREE/specs/fenced.md"
run
if printf '%s' "$out" | grep -q 'fenced.md.*(Z9) appears on no other line'; then
  pass "a fenced occurrence does not mask a dangling pointer"
else
  fail "the fenced Z9 was counted as a target -- got: $out"
fi

echo
echo "=== rule 1: a label pattern starting mid-word is not a label ==="
# "Cambricon (MLU370)" must not read as a pointer to "LU370".
build
rm "$TREE/specs/dangling.md"
printf '# Spec: Model\n\nSupported: Cambricon (MLU370), Hygon (Z100).\n' > "$TREE/specs/model.md"
run
if printf '%s' "$out" | grep -qE 'model.md.*\((LU370|100)\)'; then
  fail "a model number was read as a label -- got: $out"
else
  pass "(MLU370) and (Z100) are not read as labels"
fi

echo
echo "=== rule 2: the cited section must contain the figure as a WORD ==="
# "someone" contains "one"; a substring match made it satisfy a claim counting "one".
build
rm "$TREE/specs/dangling.md" "$TREE/specs/figure.md"
cat > "$TREE/specs/word.md" <<'EOF'
# Spec: Word

The gauge covers only one of the tiers, which is measured below (F6).

#### F6 - status.capacity

Someone has to classify the media, and twofold growth is expected.

#### F7 - something else
EOF
run
if printf '%s' "$out" | grep -q 'word.md:3: cites (F6) for a claim counting "one"'; then
  pass "\"someone\" does not satisfy a claim counting \"one\""
else
  fail "the substring match swallowed the finding -- got: $out"
fi

echo
echo "=== rule 2: a heading that is only the label still defines it ==="
build
rm "$TREE/specs/dangling.md" "$TREE/specs/figure.md"
cat > "$TREE/specs/bare.md" <<'EOF'
# Spec: Bare

The gauge covers two of the five tiers, which is measured below (F6).

#### F6

Two independent problems.

#### F7 - something else
EOF
run
if printf '%s' "$out" | grep -q 'bare.md:3: cites (F6) for a claim counting "five"'; then
  pass "a bare \"#### F6\" heading registers as the definition"
else
  fail "the bare heading was not registered -- got: $out"
fi

echo
echo "=== rule 2: a label introduced twice is reported, not silently overwritten ==="
build
rm "$TREE/specs/dangling.md" "$TREE/specs/figure.md"
cat > "$TREE/specs/dupe.md" <<'EOF'
# Spec: Dupe

#### F6 - the first one

Content.

#### F6 - the second one

Content.
EOF
run
if printf '%s' "$out" | grep -q 'dupe.md.*(F6) is introduced by more than one heading'; then
  pass "the duplicate heading is reported"
else
  fail "the duplicate was silently overwritten -- got: $out"
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
