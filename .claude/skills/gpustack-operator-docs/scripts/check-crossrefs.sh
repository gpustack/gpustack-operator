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
#     that pointer lands nowhere. Unambiguous, so it gates. Two spellings of a pointer are read:
#     parenthesised ("(F6)", "(see F6)", "(per T12b)") and bare after a citing connective
#     ("see F6", "per T16", "from F1", "under P3"). Measured on this corpus: 458 parenthesised
#     against 167 bare, so reading only the first leaves a fifth of the construct unchecked.
#     What rule 1 gates on is recurrence -- specs/ has no dangling pointer in either spelling
#     today, and every one of those 458 + 167 is now held that way.
#   Rule 2 (OPT-IN, never fails) — the citing sentence states a quantity that does not occur in
#     the cited section at all. Set CROSSREFS_QUANTITY_PROMPTS=1 to list them; the summary always
#     says how many there are.
#
#     It is opt-in because it is measured, and the measurement is bad: on this corpus it produces
#     nine findings, none of which is a defect, and it is silent on the instance it was designed
#     for. The nine divide into three mechanisms -- a quantifier read as a count ("one helper",
#     and a target that says "a single distinct profile" instead of "one"), a figure belonging to
#     the clause NEXT to the citation, and a figure that is background ("the five gates"). Its
#     signal is not recoverable by dropping "one": that takes the hit rate from 2-in-9 to 1-in-5.
#
#     Printing it beside rule 3 is what makes it costly rather than merely useless. Rule 3 is
#     8-for-8 on this corpus; interleaving the two puts fifteen lines of noise around eight lines
#     of signal, and what gets switched off then is the pair. A report nobody sees is the shape
#     this check exists to record -- but so is a report everybody learns to skip.
#   Rule 3 (REPORTS) — the same construct in Go comments. A label there names no document at all,
#     so it cannot be resolved by anything, including this script. Scanned rather than skipped:
#     the corpus is not "specs", it is everywhere the construct is written.
#
# Do not "finish" this by promoting rule 2 to a gate without first measuring the corpus, and do not
# report a green run as "cross-references verified". What a green run establishes is rule 1.
#
# Two boundaries rule 1 does NOT reach, both deliberate:
#
#   * "in T3" and "by T13". They are the two commonest English prepositions, and they precede
#     things that are lexically labels and are not citations -- "in A100 mode", "by T13" naming a
#     column. Together they are 121 of the 167 bare pointers here, and admitting them would put
#     that much false-positive risk on a GATE, where the cost of a wrong red is every
#     contributor's. The summary names the gap rather than leaving it to be assumed absent.
#
#   * A reference written as prose: "that coverage claim is established in Alternatives, under
#     'Take drain_jobs in this scope'", or "measured below", or "the table in section 4". There is
#     no label to resolve, so nothing here can follow it. This is not a gap to be closed by a
#     wider regexp -- it is the half of issue 226 that is not mechanically checkable, and the
#     instance that issue was filed for is written this way. Do not add a heuristic that appears
#     to cover it.
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
function fence_line(l,   n, c) {
  if (l !~ /^ {0,3}(`{3,}|~{3,})/) return ""
  sub(/^ +/, "", l)
  c = substr(l, 1, 1)
  n = 0
  while (substr(l, n + 1, 1) == c) n++
  return c n
}
function fence_closes(marker, open) {
  return substr(marker, 1, 1) == substr(open, 1, 1) && \
         (length(marker) - 1) + 0 >= (length(open) - 1) + 0
}
# Is the label at position "at" inside a parenthesised group that closes right after it? That is
# the shape of a pointer, and it covers "(F6)" as well as "(see F6)" and "(per T12b)" -- the words
# before the label are still inside the parens, so the whole group is a citation and NOT a place
# the pointer can land. Reading only the immediate neighbours classified "(see F6)" as a target,
# which let a genuinely dangling "(F6)" elsewhere in the same file pass the gate.
function in_paren_pointer(line, at, len,   post, j, ch, seg) {
  post = substr(line, at + len, 1)
  if (post != ")") return 0
  seg = ""
  for (j = at - 1; j >= 1; j--) {
    ch = substr(line, j, 1)
    if (ch == "(") break
    if (ch == ")") return 0
    if (ch !~ /[A-Za-z0-9 .,]/) return 0
    seg = ch seg
  }
  if (j < 1) return 0
  if (seg == "") return 1

  # More than one label can share a paren group -- "(T24 and T21)" is two citations, and reading
  # only the immediate neighbours classified the second one as a target.
  #
  # But "MetaX (C500, C550)" is a list of model names, and those are lexically identical to labels.
  # What separates the two is a WORD that is not itself a label: "and", "see", "per". Without one,
  # the neighbours are just more names -- and accepting them reports every hardware list in this
  # corpus as a dangling pointer (measured: A30, H100, C550 and six more).
  gsub(/[A-Z]{1,2}[0-9]+[a-z]?/, " ", seg)
  return (seg ~ /[A-Za-z][A-Za-z]/)
}
# Is the label at "at" a BARE pointer -- one written after a citing connective and without
# parentheses, as in "see F6" or "per T16"?
#
# Four connectives, and the shortness is the point: "in" and "by" precede labels that are not
# citations often enough that a gate cannot carry them. The header says why, and the summary
# counts what they would have added.
#
# An unclosed "(" to the left rules this out, because a parenthesised label is in_paren_pointer's
# to judge. Without that, "(measured in T3)" would gate here on a reading the paren rule has
# already declined, and every "(see F6)" would be classified twice.
function bare_pointer(line, at,   j, k, w, ch, depth) {
  depth = 0
  for (j = 1; j < at; j++) {
    ch = substr(line, j, 1)
    if (ch == "(") depth++
    else if (ch == ")" && depth > 0) depth--
  }
  if (depth > 0) return 0
  j = at - 1
  if (substr(line, j, 1) != " ") return 0
  w = ""
  for (k = j - 1; k >= 1; k--) {
    ch = substr(line, k, 1)
    if (ch !~ /[A-Za-z]/) break
    w = ch w
  }
  return (w == "see" || w == "per" || w == "from" || w == "under")
}
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
    # A label inside a fenced code block is a string in a command or a struct field, not a place a
    # prose pointer lands. Counting those as targets masked dangling citations: this corpus has 283
    # label-shaped tokens inside fences.
    fl = fence_line(lines[i])
    if (fl != "") {
      if (!fence) { fence = 1; fopen = fl } else if (fence_closes(fl, fopen)) fence = 0
      continue
    }
    if (fence) continue

    # "Task 16" is where the label "T16" is defined. This corpus writes a task's definition in
    # full and cites it by initial, so reading only the short form reports the citation as
    # dangling -- measured: the one bare pointer in specs/ that looked dangling names a task
    # fifty lines above it under its full name, and the claim it makes is the one that task
    # settles. A gate that reported it would be asking for a correct sentence to be changed.
    t = lines[i]
    while (match(t, /Task[ ]+[0-9]+/)) {
      m = substr(t, RSTART, RLENGTH)
      sub(/^Task[ ]+/, "", m)
      lab2 = "T" m
      if (!((lab2, i) in target)) { target[lab2, i] = 1; ntarget[lab2]++ }
      t = substr(t, RSTART + RLENGTH)
    }

    s = lines[i]
    off = 0
    while (match(s, /[A-Z]{1,2}[0-9]+[a-z]?/)) {
      lab = substr(s, RSTART, RLENGTH)
      pre = (RSTART > 1) ? substr(s, RSTART - 1, 1) : " "
      post = substr(s, RSTART + RLENGTH, 1)
      after = substr(s, RSTART + RLENGTH + 1, 1)
      # The left boundary is not optional. Without it "Cambricon (MLU370)" reads as a pointer to a
      # label "LU370", because the label pattern happily starts mid-word.
      isptr = (pre !~ /[A-Za-z0-9]/ && post == ")" && after != ":" && \
               in_paren_pointer(lines[i], off + RSTART, RLENGTH))
      # A bare pointer is tested on the WHOLE line rather than on the truncated remainder, so its
      # left neighbour is the real one even on the second label of a line.
      bare = 0
      if (!isptr && pre !~ /[A-Za-z0-9]/ && post !~ /[A-Za-z0-9]/) {
        bare = bare_pointer(lines[i], off + RSTART)
      }
      if (isptr || bare) {
        citeat[lab, ++ncite[lab]] = i
        citebare[lab, ncite[lab]] = bare
      } else if (pre !~ /[A-Za-z0-9]/ && post !~ /[A-Za-z0-9]/) {
        if (!((lab, i) in target)) { target[lab, i] = 1; ntarget[lab]++ }
      }
      off += RSTART + RLENGTH - 1
      s = substr(s, RSTART + RLENGTH)
    }
  }
  for (lab in ncite) {
    for (k = 1; k <= ncite[lab]; k++) {
      i = citeat[lab, k]
      # Reachable when some line other than this one carries the label outside a citation.
      if (ntarget[lab] > 1) continue
      if (ntarget[lab] == 1 && !((lab, i) in target)) continue
      # The two spellings are named differently so a reader can find the pointer on the line: the
      # bare one is not written "(T16)" anywhere, and printing it that way sends them looking for
      # a string the file does not contain.
      if (citebare[lab, k]) {
        printf "%s:%d: the bare pointer to %s appears on no other line of this file, so following it lands nowhere\n", FILENAME, i, lab
      } else {
        printf "%s:%d: (%s) appears on no other line of this file, so following it lands nowhere\n", FILENAME, i, lab
      }
    }
  }
}
AWKX
for f in $MD; do
  # Reported, not skipped: these lists are word-split, so a corpus path containing a space arrives
  # as fragments, and skipping them silently is how a file escapes the check with the gate green.
  if [ ! -f "$f" ]; then
    err "the corpus lists '$f', which is not a file — a path with a space in it splits into fragments and would otherwise be skipped silently"
    continue
  fi
  awk "$RULE1" "$f" > "$WORK/r1"
  while IFS= read -r hit; do
    [ -n "$hit" ] && err "$hit"
  done < "$WORK/r1"
done

# --- 2. the quantity in the citing sentence, in the cited section ------------
echo "==> quantities cited across a reference (opt-in; counted either way)"
read -r -d '' RULE2 <<'AWKX' || true
# Does "line" contain "w" as a whole word? index() alone matches substrings, which made a section
# saying "someone" satisfy a claim counting "one" and "twofold" satisfy "two" -- false negatives on
# exactly the side this rule is supposed to prompt about, while the citing side was already bounded.
function has_word(line, w,   p, rest, pre, post, off) {
  rest = line
  off = 0
  while ((p = index(rest, w)) > 0) {
    pre = (off + p > 1) ? substr(line, off + p - 1, 1) : " "
    post = substr(line, off + p + length(w), 1)
    if (pre !~ /[A-Za-z]/ && post !~ /[A-Za-z]/) return 1
    rest = substr(rest, p + length(w))
    off += p + length(w) - 1
  }
  return 0
}
{ lines[FNR] = $0 }
# Every heading, so a section ends at the next heading of its level whether or not that one carries
# a label. The label itself may be the whole heading text ("#### F6"), so the character after it is
# tested rather than required to exist.
/^#+ +/ {
  h = $0
  lvl = 0
  while (substr(h, lvl + 1, 1) == "#") lvl++
  hlvl[FNR] = lvl
  sub(/^#+ +/, "", h)
  if (match(h, /^[A-Z]{1,2}[0-9]+[a-z]?/)) {
    lab = substr(h, RSTART, RLENGTH)
    nxt = substr(h, RSTART + RLENGTH, 1)
    if (nxt == "" || nxt !~ /[A-Za-z0-9]/) {
      # A label introduced twice in one file makes every pointer to it ambiguous, and silently
      # keeping the last one would judge citations against the wrong section.
      if (lab in defline) dup[lab] = dup[lab] " " defline[lab]
      defline[lab] = FNR
      deflvl[lab] = lvl
    }
  }
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
        for (j = defline[lab]; j <= endline[lab]; j++) if (has_word(tolower(lines[j]), fig)) { found = 1; break }
        if (!found && index(miss, fig) == 0) miss = miss (miss == "" ? "" : ", ") fig
      }
      if (miss != "") printf "%s:%d: cites (%s) for a claim counting \"%s\"; the section at line %d never says it\n", FILENAME, i, lab, miss, defline[lab]
    }
  }
  for (lab in dup) {
    printf "%s:%d: (%s) is introduced by more than one heading (also at%s); every pointer to it is ambiguous, and the span checked above is the last one\n", FILENAME, defline[lab], lab, dup[lab]
  }
}
AWKX
: > "$WORK/r2"
for f in $MD; do
  [ -f "$f" ] || continue
  awk "$RULE2" "$f" >> "$WORK/r2"
done
# Counted always, listed only when asked. The count is what keeps the rule visible; the list is
# what buries rule 3 when it runs beside it.
while IFS= read -r hit; do
  [ -n "$hit" ] || continue
  prompts=$((prompts + 1))
  if [ "${CROSSREFS_QUANTITY_PROMPTS:-0}" = "1" ]; then
    printf '  prompt: %s\n' "$hit"
  fi
done < "$WORK/r2"
if [ "${CROSSREFS_QUANTITY_PROMPTS:-0}" != "1" ] && [ "$prompts" -gt 0 ]; then
  printf '  %s finding(s), not listed. Set CROSSREFS_QUANTITY_PROMPTS=1 to see them.\n' "$prompts"
fi

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
OK: every cited label is reachable in its own file, in both spellings -- "(F6)" and bare
    "see F6" / "per F6" / "from F6" / "under F6".

That is all a green run establishes. It does NOT establish that any cited section supports the
claim citing it -- the half that is not mechanically checkable. Neither does it cover a reference
written as prose ("established in Alternatives", "measured below"), which has no label to follow,
nor a bare pointer after "in" or "by", which this gate declines by design (see the header).

Outstanding for a reader:
  $go_prompts  Go comment reference(s) that name no document
  $prompts  quantity claim(s) whose cited section never states the number (measured unreliable;
     CROSSREFS_QUANTITY_PROMPTS=1 lists them)
SUMMARY
