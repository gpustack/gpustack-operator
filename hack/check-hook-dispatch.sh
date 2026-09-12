#!/usr/bin/env bash
# check-hook-dispatch.sh — proves the lint Stop hook sends each kind of dirty path to a target that
# covers it.
#
# The hook's whole content is a routing decision, and a routing decision cannot be checked by
# reading it: every branch looks correct beside the pattern it carries, and what goes wrong is the
# path that matches no branch at all. That is the defect this exists because of -- a turn touching
# only shell fired none of the branches, so a change was verified by a target that does not read
# shell, and it reached main red.
#
# Each case builds a throwaway tree, dirties one path, and asserts the exact set of targets chosen.
# `make` is replaced for the length of the run, so the routing is observed without paying for a lint
# pass and without the editing pass one of those targets performs.
#
# Half the cases assert an ABSENCE, and they are the half that keeps this honest: markdown must not
# reach the code gate, because the code gate excludes markdown by decision and reporting a page
# there would be it answering a question that is not its own. A suite asserting only presence would
# pass against a hook that ran every target every time, which is the same as having no routing.
# Asserting both directions is also why there is nothing separate to self-test here: a dispatch
# stuck at "nothing" fails the presence cases, and one stuck at "everything" fails the absence ones.
#
# Usage: bash hack/check-hook-dispatch.sh [repo-root]
set -euo pipefail

ROOT="${1:-}"
[ -n "$ROOT" ] || ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HOOK="$ROOT/.agents/hooks/gpustack-operator-lint.sh"

# Three states, not two: the cases pass, a case routed wrong, or the hook could not be exercised at
# all. Folding the last into a pass would make this gate silently absent the day the hook moves.
if [ ! -f "$HOOK" ]; then
  echo "FAIL: $HOOK does not exist, so the dispatch was not exercised. This says nothing about"
  echo "      whether the routing is correct."
  exit 2
fi

MINI="$(mktemp -d)"
trap 'rm -rf "$MINI"' EXIT
TREE="$MINI/tree"
BIN="$MINI/bin"
TRACE="$MINI/trace"
NOTES="$MINI/notes"

# The stub records the arguments and succeeds. Succeeding matters: the hook's run_lint reports only
# a command that failed, so a stub that exited non-zero would bury every case in lint output.
mkdir -p "$BIN"
cat > "$BIN/make" <<'STUB'
#!/usr/bin/env bash
echo "$*" >> "${HOOK_DISPATCH_TRACE}"
STUB
chmod +x "$BIN/make"

fails=0
cases=0
pass() { printf 'PASS  %s\n' "$1"; }
fail() { printf 'FAIL  %s\n' "$1"; fails=$((fails + 1)); }

# A committed tree holding one file of each kind the branches name, plus the hook under test at the
# path it really occupies -- the hook has to be able to route an edit to itself, and the path is
# what decides that.
build() {
  rm -rf "${TREE:?}"
  mkdir -p "$TREE/pkg" "$TREE/hack" "$TREE/docs" "$TREE/config" \
    "$TREE/deploy/gpustack-operator/chart" "$TREE/.agents/hooks" "$TREE/.agents/skills/demo"
  printf 'package pkg\n' > "$TREE/pkg/thing.go"
  printf '#!/usr/bin/env bash\necho ok\n' > "$TREE/hack/thing.sh"
  printf '# Page\n' > "$TREE/docs/page.md"
  printf 'image: example\n' > "$TREE/deploy/gpustack-operator/chart/values.yaml"
  printf '# Skill\n' > "$TREE/.agents/skills/demo/SKILL.md"
  cp "$HOOK" "$TREE/.agents/hooks/gpustack-operator-lint.sh"

  git -C "$TREE" init -q
  git -C "$TREE" add -A
  git -C "$TREE" -c user.email=check@example.invalid -c user.name=check \
    -c commit.gpgsign=false commit -qm "tree" >/dev/null
}

# Runs the hook once over the current state of the tree, keeping the targets it invoked and the
# notes it wrote. The hook is report-only and always exits 0, so a non-zero status means it did not
# get as far as deciding anything.
run_hook() {
  : > "$TRACE"
  : > "$NOTES"
  (
    cd "$TREE" &&
      PATH="$BIN:$PATH" CLAUDE_PROJECT_DIR="$TREE" HOOK_DISPATCH_TRACE="$TRACE" \
        bash "$TREE/.agents/hooks/gpustack-operator-lint.sh"
  ) >/dev/null 2>"$NOTES"
}

# The targets the hook chose, sorted and comma-joined so a case states one string. Sorted rather
# than in call order, because which target runs is the contract here and the order is not.
targets() {
  if ! run_hook; then
    echo "<hook exited non-zero>"
    return
  fi
  sort -u "$TRACE" | tr '\n' ',' | sed 's/,$//'
}

# expect <name> <comma-separated target set>
expect() {
  local name="$1" want="$2" got
  cases=$((cases + 1))
  got="$(targets)"
  if [ "$got" = "$want" ]; then
    pass "$name"
  else
    fail "$name: want [$want], got [$got]"
  fi
}

# expect_vendor_note <name> <yes|no>: did the hook warn that a vendoring edit cannot have taken
# effect? That branch reports rather than running a target, so it is read off the hook's notes.
expect_vendor_note() {
  local name="$1" want="$2" got="no"
  cases=$((cases + 1))
  run_hook || true
  if grep -q 'vendoring edit' "$NOTES"; then
    got="yes"
  fi
  if [ "$got" = "$want" ]; then
    pass "$name"
  else
    fail "$name: want warning=[$want], got [$got]"
  fi
}

# --- the code gate is default-in -------------------------------------------

# The reported defect. Shell is read by check-symbols.sh alone, which `make lint` invokes.
build
printf '#!/usr/bin/env bash\necho changed\n' > "$TREE/hack/thing.sh"
expect "a shell source outside every named path runs the code gate" "lint"

# The other reported defect: this path is on the docs branch and holds shell as well as markdown,
# so it used to run the one target that does not read shell.
build
printf '#!/usr/bin/env bash\necho case\n' > "$TREE/.agents/skills/demo/case.sh"
expect "shell under a docs input runs both targets" "lint,lint docs"

# The closing condition, stated as an input: a file type no branch names still has an answer.
build
printf 'key = "value"\n' > "$TREE/config/unnamed.toml"
expect "a file type no pattern names still runs a target" "lint"

# The hook is a tracked shell source that check-symbols.sh does not exclude, so the commit that
# changes the routing is itself routed by it. Before this, editing the hook matched no branch and
# ran nothing: the defect pointed at the file containing it.
build
printf '\n# touched\n' >> "$TREE/.agents/hooks/gpustack-operator-lint.sh"
expect "the hook routes an edit to itself" "lint"

build
printf 'package pkg // changed\n' > "$TREE/pkg/thing.go"
expect "a Go source runs the code gate" "lint"

# --- markdown is the one kind the code gate must not claim ------------------

build
printf '# Page\n\nmore\n' > "$TREE/docs/page.md"
expect "markdown alone runs the documentation contract and nothing else" "lint docs"

# --- a rename line carries two paths, and both count ------------------------

# The name that left is shell and the name that arrived is markdown. Matching the porcelain line
# instead of the paths sees only the arrival, and routes away from the gate the departure needs.
build
git -C "$TREE" mv hack/thing.sh docs/thing.md
expect "a rename is routed by both of its paths" "lint,lint docs"

# --- nothing dirty means nothing to cover -----------------------------------

# `grep -v` reports success on an empty input, so "some dirty path is not markdown" is true of a
# clean tree unless something exits first. Measured, and the reason the hook exits before asking.
build
expect "a clean tree runs no target" ""

# --- the chart keeps its own two branches -----------------------------------

build
printf 'image: changed\n' > "$TREE/deploy/gpustack-operator/chart/values.yaml"
expect "a chart source runs the chart targets and the code gate" "generate chart,lint,lint chart"

# --- the vendoring warning, whose test is anchored ---------------------------

# A patch is dirty and no vendored tree moved, which is what the warning is for: `make deps` is a
# no-op on a tree whose version already matches, so the edit ships as nothing.
build
mkdir -p "$TREE/hack/deploy/gpustack-operator/chart/charts/demo"
printf 'a patch\n' > "$TREE/hack/deploy/gpustack-operator/chart/charts/demo/fix.diff"
expect_vendor_note "a dirty patch beside a clean vendored tree warns" "yes"

# The same edit with the vendored tree dirty too: re-vendoring happened, so there is nothing to
# warn about. This case is what the anchor buys. The patch directory's own path ENDS in the tree
# path being looked for, so an unanchored test would find `hack/deploy/.../charts/` and conclude a
# vendored tree was dirty when none was -- suppressing the warning in exactly the state it exists
# for. Asking it of paths rather than of porcelain lines is what lets the anchor be written at all.
build
mkdir -p "$TREE/hack/deploy/gpustack-operator/chart/charts/demo" \
  "$TREE/deploy/gpustack-operator/chart/charts/demo"
printf 'a patch\n' > "$TREE/hack/deploy/gpustack-operator/chart/charts/demo/fix.diff"
printf 'vendored\n' > "$TREE/deploy/gpustack-operator/chart/charts/demo/values.yaml"
expect_vendor_note "a dirty patch beside a re-vendored tree does not warn" "no"

if [ "$fails" -gt 0 ]; then
  echo
  echo "FAIL: $fails of $cases dispatch case(s) chose the wrong set of targets."
  echo "      A path routed to no target is verified by nothing; one routed to the wrong target is"
  echo "      verified by something that does not read it, which looks the same from the outside."
  exit 1
fi

echo "OK: the lint hook routed all $cases case(s) to the targets that cover them."
