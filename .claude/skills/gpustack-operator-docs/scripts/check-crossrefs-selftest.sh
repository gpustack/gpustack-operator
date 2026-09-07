#!/usr/bin/env bash
# check-crossrefs-selftest.sh — proves check-crossrefs.sh does the half that matters.
#
# The failure that prompted the checker had a real target: the label existed, in the same document,
# on an adjacent topic. So "rule 1 is green on it" is not a bug to fix, it is the whole reason
# rule 2 has to exist -- and a later edit that quietly drops rule 2 would leave a checker that
# reports green on exactly the case it was built for. These cases are what stops that.
#
# What these cases establish is that each rule CAN fail. That is not the same as either rule being
# useful on specs/: rule 2 fires here on a constructed shape and has no true positive on the real
# corpus, and the instance in the issue that asked for it is written as prose, which no rule here
# reads. Both facts are in check-crossrefs.sh's header. A green self-test means the teeth exist,
# not that they bite anything currently in the tree.
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

# Rule 2 is opt-in, so a case asserting on its findings has to ask for them. One that forgot would
# pass for the wrong reason: the finding missing because nobody requested it reads exactly like the
# finding missing because the rule stopped working.
run_prompts() {
  set +o errexit
  out="$(CROSSREFS_QUANTITY_PROMPTS=1 bash "$CHECK" "$TREE" 2>&1)"
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
echo "=== rule 2 is counted but not listed unless asked ==="
if printf '%s' "$out" | grep -qE '^  [0-9]+ finding\(s\), not listed'; then
  pass "the default run states the count and withholds the list"
else
  fail "the default run did not state a withheld count -- got: $out"
fi
if printf '%s' "$out" | grep -q 'cites (F6) for a claim counting'; then
  fail "rule 2 listed a finding without CROSSREFS_QUANTITY_PROMPTS -- got: $out"
else
  pass "no rule 2 finding is listed by default"
fi

echo
echo "=== rule 2 fires on the known shape, and only on it ==="
run_prompts
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
run_prompts
if printf '%s' "$out" | grep -q 'figure.md:3: cites (F6)'; then
  fail "still reported after the figure was changed to one the section states"
else
  pass "changing \"five\" to \"two\" removes the finding"
fi

echo
echo "=== rule 2 reports, it does not gate ==="
build
rm "$TREE/specs/dangling.md"
run_prompts
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
echo "=== rule 1: two labels in one paren group are both pointers ==="
# "(T24 and T21)" is two citations. Reading only the immediate neighbours made the second one a
# target, so a dangling first one passed the gate. Both shapes are in the corpus.
build
rm "$TREE/specs/dangling.md"
cat > "$TREE/specs/pair.md" <<'EOF'
# Spec: Pair

The rebase happened twice on the same branch (Y8 and Z9), each time for a patch.

#### Y8 - the one that exists

Content.
EOF
run
if printf '%s' "$out" | grep -q 'pair.md.*(Z9) appears on no other line'; then
  pass "the second label in \"(Y8 and Z9)\" is a pointer, not a target"
else
  fail "(Z9) was masked by sharing a paren group with (Y8) -- got: $out"
fi

echo
echo "=== rule 1: a list of model names in one paren group is NOT a citation ==="
# The counterweight to the case above, and the reason the rule keys on a connective WORD rather
# than on "another label is nearby": model names are lexically identical to labels. Widening the
# scan to accept digits reported A30, H100 and C550 in this corpus.
build
rm "$TREE/specs/dangling.md"
printf '# Spec: Vendors\n\nSupported: MetaX (C500, C550), NVIDIA (A100 / H100), Hygon (Z100, Z100L).\n' \
  > "$TREE/specs/vendors.md"
run
if printf '%s' "$out" | grep -qE 'vendors.md.*\((C550|H100|Z100L)\)'; then
  fail "a model name in a list was read as a dangling pointer -- got: $out"
else
  pass "(C500, C550) and (A100 / H100) are lists, not citations"
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
run_prompts
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
run_prompts
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
run_prompts
if printf '%s' "$out" | grep -q 'dupe.md.*(F6) is introduced by more than one heading'; then
  pass "the duplicate heading is reported"
else
  fail "the duplicate was silently overwritten -- got: $out"
fi

echo
echo "=== rule 1: a bare pointer after a connective is a pointer ==="
build
rm "$TREE/specs/dangling.md"
printf '# Spec: Bare Pointer\n\nThe allocatable path is covered per Z9, which settles it.\n' \
  > "$TREE/specs/barecite.md"
run
if printf '%s' "$out" | grep -q 'barecite.md:3: the bare pointer to Z9 appears on no other line'; then
  pass "\"per Z9\" with no target is reported"
else
  fail "a bare pointer was not read as a pointer -- got: $out"
fi

echo
echo "=== rule 1: a bare pointer that lands is quiet ==="
build
rm "$TREE/specs/dangling.md"
cat > "$TREE/specs/barelands.md" <<'EOF'
# Spec: Bare Lands

The allocatable path is covered per Z9, which settles it.

#### Z9 - the section it names

Content.
EOF
run
if printf '%s' "$out" | grep -q 'barelands.md.*Z9'; then
  fail "a bare pointer with a heading target was reported -- got: $out"
else
  pass "\"per Z9\" is quiet when Z9 has a heading"
fi

echo
echo "=== rule 1: \"in\" and \"by\" are NOT gated, by design ==="
# Both precede labels that are not citations -- "in A100 mode", "by T13" naming a column -- so
# they stay out of a gate. If a later change admits them, this case is the record of the choice.
build
rm "$TREE/specs/dangling.md"
printf '# Spec: Loose\n\nThe key is set in Z9 and consumed by Y8.\n' > "$TREE/specs/loose.md"
run
if printf '%s' "$out" | grep -qE 'loose.md.*(Z9|Y8)'; then
  fail "\"in Z9\" or \"by Y8\" was gated -- got: $out"
else
  pass "\"in\" and \"by\" do not make a pointer"
fi

echo
echo "=== rule 1: \"Task 16\" is where \"T16\" is defined ==="
# This corpus writes a task's definition in full and cites it by initial, so reading only the
# short form reports a correct sentence as dangling.
build
rm "$TREE/specs/dangling.md"
cat > "$TREE/specs/tasknum.md" <<'EOF'
# Spec: Task Number

- [x] **Task 16:** Preserve the admin-authored label so it reaches Node.Labels.

The e2e asserts the label survives reconcile per T16.
EOF
run
if printf '%s' "$out" | grep -q 'tasknum.md.*T16'; then
  fail "\"per T16\" was reported although \"Task 16\" defines it -- got: $out"
else
  pass "\"Task 16\" registers as the target of \"T16\""
fi

echo
echo "=== rule 1: a connective inside parens stays the paren rule's to judge ==="
build
rm "$TREE/specs/dangling.md"
printf '# Spec: Parens\n\nThe budget is capped (measured in Z9) and holds.\n' > "$TREE/specs/inparen.md"
run
if printf '%s' "$out" | grep -q 'inparen.md.*the bare pointer to Z9'; then
  fail "a parenthesised label was judged by the bare rule -- got: $out"
else
  pass "\"(measured in Z9)\" is not read by the bare rule"
fi

echo
echo "=== rule 1: the known cost -- a model name after a connective, mentioned once ==="
# "from A100" is lexically a bare pointer, and A100 is not a label. The four connectives were
# chosen partly because this combination does not occur in specs/; this case records that the
# checker WOULD report it, so the cost is written down rather than met later as a surprise.
build
rm "$TREE/specs/dangling.md"
printf '# Spec: Model After Connective\n\nThe figure is taken from A100 datasheets.\n' \
  > "$TREE/specs/modelconn.md"
run
if printf '%s' "$out" | grep -q 'modelconn.md.*the bare pointer to A100'; then
  pass "reported -- the documented limit of keying on a connective alone"
else
  fail "expected the known false positive; if it is now handled, update this case -- got: $out"
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
echo "SELFTEST PASSED: rule 1 gates in both spellings, rule 2 can still fail on the shape it was"
echo "                 built for, and neither claims the other's ground."
