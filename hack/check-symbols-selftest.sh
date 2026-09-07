#!/usr/bin/env bash
# check-symbols-selftest.sh — proves check-symbols.sh can fail, and proves where it must not.
#
# The check passes on every file in this repository, and a check that has never been seen to fail
# is indistinguishable from one that cannot. Each case below builds a throwaway tree, plants one
# character, and asserts the verdict. Nothing in the real tree is touched.
#
# Half the cases assert the opposite, and they are the more important half. A symbol check written
# one range too wide reports the em dash, which appears on thousands of lines here; it would be
# switched off the day it landed, and a switched-off check reads exactly like a clean tree. So the
# em dash, the arrow and the mathematical operators are pinned as MUST NOT REPORT, next to the
# exempt paths, which are pinned the same way.
#
# Usage: bash hack/check-symbols-selftest.sh [repo-root]
set -euo pipefail

ROOT="${1:-}"
[ -n "$ROOT" ] || ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHECK="$ROOT/hack/check-symbols.sh"

MINI="$(mktemp -d)"
trap 'rm -rf "$MINI"' EXIT
TREE="$MINI/tree"

fails=0
pass() { printf 'PASS  %s\n' "$1"; }
fail() { printf 'FAIL  %s\n' "$1"; fails=$((fails + 1)); }

# A tree the check is clean on: ordinary sources, ordinary punctuation, plus one file under each
# exempt path so that the exemptions are exercised rather than merely declared.
build() {
  rm -rf "${TREE:?}"
  mkdir -p "$TREE/pkg/thing" "$TREE/hack" "$TREE/staging/up" "$TREE/binding/dmi" \
    "$TREE/gen/binding/dmi" "$TREE/pkg/device" "$TREE/.github/workflows"
  printf '// Package thing does one thing.\npackage thing\n' > "$TREE/pkg/thing/thing.go"
  printf '#!/usr/bin/env bash\n# A script.\necho ok\n' > "$TREE/hack/run.sh"
  git -C "$TREE" init -q
  git -C "$TREE" add -A
}

# Runs the check over the throwaway tree and names the outcome. A check that could not run at all
# is "broken", and its own diagnostic goes to stderr rather than being swallowed: the first time
# this suite failed in CI it reported twenty-five identical "got broken" lines and not one word of
# why, which cost a full round to find out.
verdict() {
  local out rc=0
  git -C "$TREE" add -A
  out="$(bash "$CHECK" "$TREE" 2>&1)" || rc=$?
  case "$rc" in
    0) echo "clean" ;;
    1) echo "reported" ;;
    *) echo "broken"; printf '      %s\n' "$out" >&2 ;;
  esac
}

# plant <path> <line> <want: reported|clean> <label>
plant() {
  build
  mkdir -p "$(dirname "$TREE/$1")"
  printf '%s\n' "$2" >> "$TREE/$1"
  got="$(verdict)"
  if [ "$got" = "$3" ]; then
    pass "$4"
  else
    fail "$4 (wanted $3, got $got)"
  fi
}

echo "=== the tree the check is meant to pass on ==="
build
got="$(verdict)"
if [ "$got" = "clean" ]; then
  pass "an ordinary tree is clean"
else
  fail "an ordinary tree is clean (got $got)"
fi

echo
echo "=== MUST REPORT: a decorative symbol in a comment ==="
plant pkg/thing/thing.go '// 🚀 ships it' reported "an emoji outside the BMP, in a Go comment"
plant pkg/thing/thing.go '// gate (switch ①) is on' reported "a circled digit, which CLAUDE.md names"
plant hack/run.sh '# ⛔ do not do this' reported "a no-entry sign opening a shell comment"
plant hack/run.sh '# ⭐ the good part' reported "a star opening a shell comment"
plant hack/run.sh '# ── a section ───────' reported "box drawing used as a divider"
plant pkg/thing/thing.go '// ✓ done' reported "a dingbat check mark"
# Whole files are read, not only comments. That is a design decision rather than an accident, so it
# is pinned here: it is the reason ci.yml needs a named exemption at all.
plant pkg/thing/thing.go 's := "🚀"' reported "a symbol in a string literal, not a comment"

echo
echo "=== MUST NOT REPORT: ordinary punctuation and meaning-bearing symbols ==="
plant pkg/thing/thing.go '// A sentence — with an em dash — reads normally.' clean "the em dash (U+2014), on thousands of lines here"
plant pkg/thing/thing.go '// A list: • one • two' clean "the bullet (U+2022)"
plant pkg/thing/thing.go '// Empty string → the field is unset.' clean "an arrow expressing a mapping"
plant hack/run.sh '# stop → edit → start, in that order' clean "an arrow expressing a sequence"
plant pkg/thing/thing.go '// Holds while requested ≤ max(valid).' clean "a mathematical operator"
plant pkg/thing/thing.go '// min(ceil(x) ∈ valid), and −1 means unset.' clean "set membership and a minus sign"

echo
echo "=== MUST NOT REPORT: the exempt paths, each for its own reason ==="
plant staging/up/mod.go '// ⛔ upstream wrote this' clean "staging/ is upstream code"
plant binding/dmi/dmi.h '/// @defgroup ↓ ⛔ vendor header ↓' clean "binding/ is generated from vendor headers"
plant gen/binding/dmi/dmi.h '/// ⛔ vendor header, generated copy' clean "gen/binding/ is the generated copy"
plant pkg/device/doc.go '//	┌──────────┐' clean "pkg/device/doc.go draws a diagram"
plant pkg/extensionroute/swagger/ui/swagger-ui-bundle.js '// 🚀 vendored, minified' clean "the vendored swagger bundle"
plant .github/workflows/ci.yml '        { "title": "## 🚀 Features" },' clean "ci.yml release-note titles are product output"
plant docs/guide.md '# 🚀 Getting started' clean "markdown is covered by the docs checks"
plant hack/check-symbols-selftest.sh '# ⛔ a fixture must carry what it asserts on' clean "this file, which has to contain them"

echo
echo "=== the controls that give the exemptions their meaning ==="
# Each exempt case above passes if the exemption works -- and also if the file was never tracked,
# or if the extension is not one the check looks at. Those read identically. Planting the same
# character at a path that is NOT exempt, with the same extension, separates them: it must be
# reported, so the silence above is the exemption doing its job.
plant csrc/shim.h '/// ⛔ ours, not a vendor header' reported "the same .h line outside binding/ is reported"
plant .github/workflows/release.yml '        { "title": "## 🚀 Features" },' reported "the same yml line outside ci.yml is reported"
plant docs/guide.txt '# 🚀 Getting started' reported "the same text outside markdown is reported"
plant pkg/device/other.go '//	┌──────────┐' reported "the same diagram outside doc.go is reported"
plant hack/check-symbols.sh '# ⛔ the checker itself is not exempt' reported "the exemption is this file only, not hack/**"
plant pkg/extensionroute/swagger/other.js '// 🚀 ours' reported "the same .js outside the bundle path is reported"

echo
echo "=== a file that exists but was never staged ==="
# Deliberately not routed through plant/verdict: those stage the tree first, which is the exact
# step that would hide an index-only scan. A developer's worktree does not look like that.
build
printf '// ⛔ never staged\n' > "$TREE/pkg/thing/unstaged.go"
if bash "$CHECK" "$TREE" >/dev/null 2>&1; then
  fail "an unstaged new file is still covered (the scan read the index only)"
else
  pass "an unstaged new file is still covered"
fi

echo
if [ "$fails" -gt 0 ]; then
  echo "SELFTEST FAILED: $fails case(s)."
  exit 1
fi
echo "SELFTEST PASSED: the check was seen to fire on each shape it claims to catch, and to stay"
echo "                 silent on the ordinary punctuation and exempt paths that would retire it."
