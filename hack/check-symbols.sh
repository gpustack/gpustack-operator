#!/usr/bin/env bash
# check-symbols.sh — decorative symbols in commented sources.
#
# Scope is code. Markdown is checked by `make lint docs` and is not this gate's subject.
#
# CLAUDE.md asks that comments state the point in words: no emoji, no decorative symbols, no
# circled digits. The rule was written down and nothing was in the room when it was broken, which
# is how ten such lines reached main across seven files, the most recent of them in a file merged
# the same week this check was written.
#
# WHAT IT REPORTS: a character from one of the ranges below, anywhere in a tracked source file.
#
#   U+2460-24FF  circled alphanumerics       U+2700-27BF  dingbats
#   U+2500-259F  box drawing, block elements U+2B00-2BFF  misc symbols and arrows
#   U+25A0-25FF  geometric shapes            U+1F000-1FAFF emoji
#   U+2600-26FF  misc symbols                U+FE0F, U+20E3 variation selector, keycap
#
# WHAT IT DELIBERATELY DOES NOT REPORT, because a check written wide is worse than no check --
# it is disabled the first day it fires on something legitimate:
#
#   U+2000-206F  general punctuation. The em dash and the bullet live here and are ordinary
#                punctuation in this repository. Including this block reports thousands of lines.
#   U+2190-21FF  arrows. 138 comment lines use one to express a mapping ("0 -> unset"), which is
#                what an arrow is for; whether any given one reads better as a verb is a judgement
#                no character range can make.
#   U+2200-22FF  mathematical operators. "<= 100" and "in validValues" carry meaning.
#
# It reads whole files rather than only comments. Deciding what is a comment needs a parser per
# language, and the cheap version of that parser reads a `#` inside a heredoc as a comment. The
# cost of the simpler rule is that a symbol in a user-facing string is reported too; ci.yml is
# exempt below for exactly that reason.
#
# Every exemption is a named path, so that adding one is visible in a diff. A second diagram, or a
# second file of product output, is a line here rather than a widened rule.
#
# Usage: bash hack/check-symbols.sh [repo-root]
set -euo pipefail

ROOT="${1:-}"
[ -n "$ROOT" ] || ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# (*UTF) turns on PCRE2's UTF mode from inside the pattern. Without it, git enables UTF only when
# the ambient locale is one, so the same pattern that works in a terminal is rejected outright in a
# container with no LANG set -- "character code point value in \x{} is too large", every code point
# above U+00FF. Setting LC_ALL here instead would trade this for a dependency on C.UTF-8 existing.
CLASS='(*UTF)[\x{2460}-\x{24FF}\x{2500}-\x{259F}\x{25A0}-\x{25FF}\x{2600}-\x{26FF}\x{2700}-\x{27BF}\x{2B00}-\x{2BFF}\x{1F000}-\x{1FAFF}\x{FE0F}\x{20E3}]'

# SCOPE: this gate covers code. Markdown is out of it by decision rather than by convenience --
# the documentation corpus is governed by its own chain, `make lint docs`, and this check does not
# reach into it. Reporting a symbol in a page would be this gate answering a question that is not
# its own.
#
# The self-test is exempt because it has to carry the characters it asserts on; a fixture that
# could not contain them could not test for them. It is named here rather than covered by a
# hack/** rule, so the exemption stays one file wide.
#
# The rest are named paths. staging/ and binding/ are other people's code: upstream modules,
# generated bindings, and the vendor copyright headers they carry. pkg/extensionroute/swagger/ui/
# is a vendored Swagger UI drop -- one minified 1.5MB line of JavaScript and its assets, which
# nobody here edits. pkg/device/doc.go draws a diagram, where the characters carry the structure
# and words would lose it. ci.yml's release-note titles are product output.
EXCLUDES=(
  ':(exclude)hack/check-symbols-selftest.sh'
  ':(exclude)staging/**'
  ':(exclude)binding/**'
  ':(exclude)gen/binding/**'
  ':(exclude)pkg/extensionroute/swagger/ui/**'
  ':(exclude)pkg/device/doc.go'
  ':(exclude).github/workflows/ci.yml'
  ':(exclude)*.md'
)

# Three states, not two: the tree scans, it carries findings, or git cannot read it at all. The
# last one is reachable in a supported build -- a docker build whose context came from a git
# worktree carries a .git that is a file pointing outside the context, and every git call in the
# container fails. Folding that into "clean" would make this gate silently absent exactly where it
# is hardest to notice, so it is announced instead, in the same three-state shape hack/lint.sh
# already uses for the commit lint.
if ! git -C "$ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  echo "SKIP: git cannot read this tree, so the repository scan did not run. This says nothing"
  echo "      about whether the sources are clean."
  exit 0
fi

# --untracked so a file that exists but has not been staged is covered. Without it the scan reads
# the index, and a new file escapes until the commit that adds it -- while the self-test, which
# stages before every run, would keep reporting the check as working. Ignored files stay out.
#
# git grep exits 1 when it selects nothing, which is this check's success. The outcomes are
# separated explicitly: `|| true` would fold a real failure into the same silent pass.
found=""
rc=0
found="$(git -C "$ROOT" grep --untracked -nP "$CLASS" -- "${EXCLUDES[@]}")" || rc=$?

if [ "$rc" -gt 1 ]; then
  echo "FAIL: git grep exited $rc, so this check did not run. It needs a git built with PCRE (-P)."
  echo "      This is an environment problem, not a finding about the sources."
  exit 2
fi

if [ "$rc" -eq 0 ]; then
  count="$(printf '%s\n' "$found" | wc -l | tr -d ' ')"
  printf '%s\n' "$found"
  echo
  echo "FAIL: $count line(s) carry a decorative symbol. State the point in words instead."
  echo "      If a symbol carries information a word cannot, exempt the path in hack/check-symbols.sh."
  exit 1
fi

# The count is of tracked files outside the exemptions, which is not the same as "source files" --
# it includes go.sum, images and anything else committed. Unstaged files are scanned as well and
# are not in it. Said plainly rather than rounded up, so the number cannot be quoted for more than
# it measures.
echo "OK: no decorative symbols outside the exemptions ($(git -C "$ROOT" ls-files -- "${EXCLUDES[@]}" | wc -l | tr -d ' ') tracked file(s), plus anything unstaged)."
