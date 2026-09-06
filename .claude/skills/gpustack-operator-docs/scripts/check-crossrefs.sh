#!/usr/bin/env bash
# check-crossrefs.sh — internal cross-references: "(F6)", "(T12b)", "(P5)".
#
# READ THIS BEFORE CHANGING WHAT THIS SCRIPT REPORTS.
#
# A cross-reference is a claim of the form "section X establishes Y", and it has two halves:
#
#   half one — does X exist?            grep-able.
#   half two — does X support Y?        not grep-able, and it is the half that actually bit.
#
# The failure this script was written for had a real F6, in the same document, on an adjacent
# topic, in the same vocabulary. A checker that only did half one would have been GREEN on it, and
# a green run would then have read as "cross-references verified" — turning an unexamined area into
# one someone believes was examined. That is worse than no checker at all.
#
# So this script does both, and they are deliberately not the same kind of thing:
#
#   Rule 1 (FAILS the run) — a cited label that appears nowhere else in its own file. Following
#     that pointer lands nowhere. Unambiguous, so it gates.
#   Rule 2 (REPORTS, never fails) — the citing sentence states a quantity that does not occur in
#     the cited section at all. The instance above had exactly this tell: the claim said "two of
#     the five" and the cited section contains no "five". It produces false positives — a sentence
#     may legitimately count something the target does not restate — which is why it is a prompt
#     for a reader and not a gate. It is printed on every run, and the summary says how many are
#     outstanding, because a report nobody sees is the shape this whole check exists to record.
#   Rule 3 (REPORTS) — the same construct in Go comments. A label there names no document at all,
#     so it cannot be resolved by anything, including this script. Scanned rather than skipped:
#     the corpus is not "specs", it is everywhere the construct is written.
#
# Do not "finish" this by promoting rule 2 to a gate without first measuring the corpus, and do not
# report a green run as "cross-references verified". What a green run establishes is rule 1.
#
# Usage: bash .claude/skills/gpustack-operator-docs/scripts/check-crossrefs.sh [repo-root]
set -euo pipefail

# awk here does byte-wise regex work over UTF-8 prose; the C locale is what keeps a multibyte
# character from aborting the run.
export LC_ALL=C

ROOT="${1:-}"
[ -n "$ROOT" ] || ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
cd "$ROOT"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

errors=0
prompts=0
err() { printf '  %s\n' "$*"; errors=$((errors + 1)); }

MD="README.md
CLAUDE.md"
for d in specs docs .claude/skills; do
  if [ -d "$d" ]; then
    MD="$MD
$(find "$d" -name '*.md' | sort)"
  fi
done
GO=""
for d in api cmd gen pkg; do
  if [ -d "$d" ]; then
    GO="$GO
$(find "$d" -name '*.go' -not -name 'zz_generated*' -not -name '*.pb.go' | sort)"
  fi
done

# --- 1. a cited label leads somewhere ---------------------------------------
echo "==> cross-reference targets"
read -r -d '' RULE1 <<'AWKX' || true
{ lines[FNR] = $0 }
END {
  # Two shapes, and they are told apart by one character. "(R2):" introduces the thing it trails --
  # "Readiness must admit an empty Detail (R2): ..." -- so it is where a pointer LANDS. A bare
  # "(R2)" mid-sentence is the pointer. Everything else carrying the label (a heading, a table
  # cell, a bold run) is also somewhere it can land.
  #
  # What is decidable, and is the defect worth gating, is a label that occurs nowhere in this file
  # except in the pointer itself.
  for (i = 1; i <= FNR; i++) {
    s = lines[i]
    while (match(s, /[A-Z]{1,2}[0-9]+[a-z]?/)) {
      lab = substr(s, RSTART, RLENGTH)
      pre = (RSTART > 1) ? substr(s, RSTART - 1, 1) : " "
      post = substr(s, RSTART + RLENGTH, 1)
      after = substr(s, RSTART + RLENGTH + 1, 1)
      isptr = (pre == "(" && post == ")" && after != ":")
      if (isptr) {
        citeat[lab, ++ncite[lab]] = i
      } else if (pre !~ /[A-Za-z0-9]/ && post !~ /[A-Za-z0-9]/) {
        if (!((lab, i) in target)) { target[lab, i] = 1; ntarget[lab]++ }
      }
      s = substr(s, RSTART + RLENGTH)
    }
  }
  for (lab in ncite) {
    for (k = 1; k <= ncite[lab]; k++) {
      i = citeat[lab, k]
      # Reachable when some line other than this one carries the label outside a citation.
      if (ntarget[lab] > 1) continue
      if (ntarget[lab] == 1 && !((lab, i) in target)) continue
      printf "%s:%d: (%s) appears on no other line of this file, so following it lands nowhere\n", FILENAME, i, lab
    }
  }
}
AWKX
for f in $MD; do
  [ -f "$f" ] || continue
  awk "$RULE1" "$f" > "$WORK/r1"
  while IFS= read -r hit; do
    [ -n "$hit" ] && err "$hit"
  done < "$WORK/r1"
done

# --- 2. the quantity in the citing sentence, in the cited section ------------
echo "==> quantities cited across a reference (report only)"
read -r -d '' RULE2 <<'AWKX' || true
{ lines[FNR] = $0 }
/^#+ +[A-Z]{1,2}[0-9]+[a-z]?[^A-Za-z0-9]/ {
  h = $0
  lvl = 0
  while (substr(h, lvl + 1, 1) == "#") lvl++
  sub(/^#+ +/, "", h)
  if (match(h, /^[A-Z]{1,2}[0-9]+[a-z]?/)) {
    lab = substr(h, RSTART, RLENGTH)
    defline[lab] = FNR
    deflvl[lab] = lvl
  }
  hlvl[FNR] = lvl
}
END {
  # Only a label a heading introduces has a section with edges. For a label defined in a table row
  # or a bold run there is no principled span to search, and guessing one would produce findings
  # whose scope nobody could check.
  for (lab in defline) {
    e = FNR
    for (i = defline[lab] + 1; i <= FNR; i++) if ((i in hlvl) && hlvl[i] <= deflvl[lab]) { e = i - 1; break }
    endline[lab] = e
  }
  for (i = 1; i <= FNR; i++) {
    s = lines[i]
    while (match(s, /\([A-Z]{1,2}[0-9]+[a-z]?\)/)) {
      lab = substr(s, RSTART + 1, RLENGTH - 2)
      s = substr(s, RSTART + RLENGTH)
      if (!(lab in defline)) continue
      if (i >= defline[lab] && i <= endline[lab]) continue
      # The citing sentence usually wraps, so the line before it is part of the claim.
      sent = ((i > 1) ? lines[i - 1] " " : "") lines[i]
      # Spelled-out numbers only. Digits in this corpus are mostly line numbers, versions and
      # identifiers, and they drown the signal; a claim that counts something ("two of the five")
      # is written in words.
      miss = ""
      t = sent
      while (match(t, /one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve/)) {
        fig = substr(t, RSTART, RLENGTH)
        pre = (RSTART > 1) ? substr(t, RSTART - 1, 1) : " "
        post = substr(t, RSTART + RLENGTH, 1)
        t = substr(t, RSTART + RLENGTH)
        if (pre ~ /[A-Za-z]/ || post ~ /[A-Za-z]/) continue
        found = 0
        for (j = defline[lab]; j <= endline[lab]; j++) if (index(tolower(lines[j]), fig) > 0) { found = 1; break }
        if (!found && index(miss, fig) == 0) miss = miss (miss == "" ? "" : ", ") fig
      }
      if (miss != "") printf "%s:%d: cites (%s) for a claim counting \"%s\"; the section at line %d never says it\n", FILENAME, i, lab, miss, defline[lab]
    }
  }
}
AWKX
: > "$WORK/r2"
for f in $MD; do
  [ -f "$f" ] || continue
  awk "$RULE2" "$f" >> "$WORK/r2"
done
while IFS= read -r hit; do
  [ -n "$hit" ] || continue
  printf '  prompt: %s\n' "$hit"
  prompts=$((prompts + 1))
done < "$WORK/r2"

# --- 3. the same construct in Go comments -----------------------------------
echo "==> cross-references in Go comments (report only)"
: > "$WORK/r3"
if [ -n "$GO" ]; then
  # shellcheck disable=SC2086  # the list is paths from find, one per line, no globbing wanted
  awk '
    /^[ \t]*\/\// {
      s = $0
      while (match(s, /\([A-Z]{1,2}[0-9]+[a-z]?\)/)) {
        printf "%s:%d: %s names no document, so it resolves against whichever spec the reader guesses\n", FILENAME, FNR, substr(s, RSTART, RLENGTH)
        s = substr(s, RSTART + RLENGTH)
      }
    }
  ' $GO >> "$WORK/r3"
fi
go_prompts=0
while IFS= read -r hit; do
  [ -n "$hit" ] || continue
  printf '  prompt: %s\n' "$hit"
  go_prompts=$((go_prompts + 1))
done < "$WORK/r3"

echo
if [ "$errors" -gt 0 ]; then
  echo "FAIL: $errors cross-reference(s) lead nowhere."
  exit 1
fi
cat <<SUMMARY
OK: every cited label is reachable in its own file.

That is all a green run establishes. It does NOT establish that any cited section supports the
claim citing it -- the half that is not mechanically checkable. Outstanding prompts for a reader:
  $prompts  quantity claim(s) whose cited section never states the number
  $go_prompts  Go comment reference(s) that name no document
SUMMARY
