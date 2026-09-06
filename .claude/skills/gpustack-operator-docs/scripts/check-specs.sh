#!/usr/bin/env bash
# check-specs.sh — the spec contract for this repository.
#
# check-docs.sh covers the pages under docs/. Nothing read specs/ at all, which is how a spec
# reached main carrying a draft-only Status word and how four Verify commands came to name tests
# that no longer exist. Both are the same shape: a convention that was written down, and nothing
# in the room when it was broken.
#
# Fails on:
#   1. line 3 of a spec is not "Status: <word>", or the word is not one this repository uses;
#      `Specified` is rejected by name, since it is legal on a branch and wrong on main;
#   2. a spec whose Status is not `Shipped` and which states no release condition under it. A spec
#      reaches main through a pull request, and a pull request may not be opened until the spec
#      reads `Shipped` — so anything else on main is a promise, and a promise needs its condition
#      written where a reader will meet it;
#   3. a `go test ... -run` command in the markdown corpus whose pattern selects no test in the
#      packages that command names. `go test` exits 0 when -run matches nothing, so such a line is
#      a permanently green re-validation path and is indistinguishable from one that works.
#      A command carrying two `-run` flags is reported separately, because only the last one is in
#      force and the packages after the first flag are never tested -- a different fix.
#
# Rule 3 reads the whole markdown corpus, not just specs/: the construct is a command someone
# re-runs, and it goes stale wherever it is written.
#
# Rule 3 has ONE escape: the upper-case phrase "NOT RE-RUNNABLE" within twelve lines below the
# command. Issue 179 asks for exactly that where the test is gone and nothing replaced it, since
# deleting the line would delete the coverage claim with it. Every exemption is printed and counted
# in the summary -- an escape nobody can see is how a gate stops being one.
#
# Usage: bash .claude/skills/gpustack-operator-docs/scripts/check-specs.sh [repo-root]
set -euo pipefail

ROOT="${1:-}"
[ -n "$ROOT" ] || ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
cd "$ROOT"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

errors=0
err() { printf '  %s\n' "$*"; errors=$((errors + 1)); }

SPECS=""
if [ -d specs ]; then
  SPECS=$(find specs -name '*.md' | sort)
fi
# Rule 3's corpus. Everything a reader might copy a command out of.
CORPUS="README.md
CLAUDE.md
$SPECS"
for d in docs .claude/skills; do
  if [ -d "$d" ]; then
    CORPUS="$CORPUS
$(find "$d" -name '*.md' | sort)"
  fi
done

# The words this repository uses on a spec's Status line. `Specified` is deliberately absent and
# deliberately named in the message below: it is the word a draft carries on a branch, and its
# failure mode is that it is legal there and says nothing on main.
STATUS_WORDS='Shipped Building Planned Built'

# --- 1, 2. the Status line ---------------------------------------------------
echo "==> spec Status"
for f in $SPECS; do
  line3=$(sed -n '3p' "$f")
  case "$line3" in
    "Status: "*) ;;
    *)
      err "$f:3: line 3 is not a 'Status:' line, it is: ${line3:-<empty>}"
      continue
      ;;
  esac

  # The value is the first word; a Shipped line may carry " — <url>" after it.
  word=$(printf '%s\n' "$line3" | sed -e 's/^Status: *//' -e 's/[ 	].*$//')

  legal=0
  for w in $STATUS_WORDS; do
    [ "$word" = "$w" ] && legal=1
  done
  if [ "$legal" -eq 0 ]; then
    if [ "$word" = "Specified" ]; then
      err "$f:3: Status is 'Specified', the draft-only word. It is legal on a branch and says nothing on main — a reader of main needs to know whether the work is planned, underway or done. Use one of: $STATUS_WORDS"
    else
      err "$f:3: Status is '$word', which is not one of: $STATUS_WORDS"
    fi
    continue
  fi

  # Rule 2. Anything other than Shipped is a promise, and a promise carries its condition.
  #
  # The condition has to have content: a bare "Blocked on:" satisfies a prefix test while saying
  # nothing, which is the thing this rule exists to prevent, so the remainder is checked too.
  if [ "$word" != "Shipped" ]; then
    line4=$(sed -n '4p' "$f")
    condition=$(printf '%s\n' "${line4#Blocked on:}" | tr -d ' \t')
    if [ "$(printf '%s' "$line4" | cut -c1-11)" != "Blocked on:" ] || [ -z "$condition" ]; then
      err "$f:3: Status is '$word', so line 4 must be a 'Blocked on:' block saying what lifts it. A spec only reaches main through a pull request, and the rule is that it reads 'Shipped' before that pull request is opened — so a spec on main that reads anything else is waiting on something, and what it waits on has to be legible without reading the whole document."
    fi
  fi
done

# --- 3. every `go test -run` selects something -------------------------------
echo "==> go test -run patterns"

# dir<TAB>test-name for every top-level test function in this module. Top-level only: -run splits
# its pattern on "/" and matches the first element against these names.
#
# staging/ is excluded because it is not this module: each subtree there is its own go.mod, wired
# in by a replace directive, so `go test ./...` never reaches it. Counting its test names would let
# a pattern that only matches something in staging report as selecting something -- a check that
# passes where the command it stands for runs nothing, which is the defect this rule exists for.
find . -name '*_test.go' -not -path './.git/*' -not -path './staging/*' -exec awk '
  FNR == 1 {
    d = FILENAME
    sub(/\/[^\/]*$/, "", d)
    sub(/^\.\//, "", d)
  }
  /^func (Test|Benchmark|Fuzz|Example)[A-Za-z0-9_]*\(/ {
    n = $2
    sub(/\(.*$/, "", n)
    print d "\t" n
  }
' {} + > "$WORK/index"

read -r -d '' EXTRACT <<'AWKX' || true
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
# How many real `-run` flags a command carries. Only " -run" with a space before it counts, so the
# "-run" inside "re-run e2e case-5" is not one.
function count_run(cmd,   n, s) {
  n = 0
  s = cmd
  while (match(s, / -run[= ]/)) { n++; s = substr(s, RSTART + RLENGTH) }
  return n
}
# Report one command, at the line its text starts on.
function emit(cmd, at,   j, pat, m, pkgs, n, t, k, nrun) {
  if (index(cmd, "go test") == 0) return
  cmd = substr(cmd, index(cmd, "go test"))
  j = index(cmd, " #")
  if (j > 0) cmd = substr(cmd, 1, j - 1)

  nrun = count_run(cmd)
  pat = ""
  if (match(cmd, / -run[= ]*'[^']*'/)) {
    m = substr(cmd, RSTART, RLENGTH)
    sub(/^ -run[= ]*'/, "", m)
    sub(/'$/, "", m)
    pat = m
  } else if (match(cmd, / -run[= ]+[^ ]+/)) {
    m = substr(cmd, RSTART, RLENGTH)
    sub(/^ -run[= ]+/, "", m)
    pat = m
  } else {
    return
  }
  cmd = substr(cmd, 1, RSTART - 1) " " substr(cmd, RSTART + RLENGTH)
  if (pat == "") return

  # Package arguments. Only "./"-relative paths can be resolved against the index built above, so
  # anything else -- a dotted import path like github.com/org/repo/pkg/..., or no package at all --
  # is reported as UNEVALUABLE rather than quietly falling back to "the whole tree". That fallback
  # was a false-green path for exactly the defect this rule exists to catch: a stale test name that
  # happens to exist somewhere else in the repository would have passed.
  pkgs = ""
  unresolvable = 0
  n = split(cmd, t, /[ \t]+/)
  for (k = 1; k <= n; k++) {
    if (substr(t[k], 1, 2) == "./") {
      pkgs = pkgs (pkgs == "" ? "" : " ") t[k]
    } else if (index(t[k], "/") > 0 && substr(t[k], 1, 1) != "-") {
      unresolvable = 1
    }
  }
  if (pkgs == "") unresolvable = 1
  # A sentinel, never an empty field. `read` with a tab IFS treats the tab as IFS WHITESPACE, so it
  # collapses runs and drops empty fields -- an empty pkgs would shift nrun into pkgs and exempt
  # into nrun, silently losing an exemption and hiding a duplicate -run. Emitting a placeholder is
  # what keeps the field count fixed.
  if (pkgs == "") pkgs = "-"

  # The escape, and the only one. A command may name a test that no longer exists when the spec
  # SAYS SO -- issue 179 asks for exactly that where no replacement exists, because deleting the
  # line would delete the coverage claim with it. The marker is the literal upper-case phrase
  # "NOT RE-RUNNABLE" within twelve lines below the command.
  #
  # Upper case on purpose: it cannot be reached by writing ordinary prose, so exempting a command
  # stays a deliberate act. And an exemption is PRINTED and counted, never silent -- an escape
  # nobody can see is how a gate stops being one.
  exempt = 0
  for (k = at; k <= at + 12 && k <= FNR; k++) {
    if (index(lines[k], "NOT RE-RUNNABLE") > 0) exempt = 1
  }
  printf "%d\t%s\t%s\t%d\t%d\t%d\n", at, pat, pkgs, nrun, exempt, unresolvable
}
{ lines[FNR] = $0 }
END {
  for (i = 1; i <= FNR; i++) {
    l = lines[i]
    fl = fence_line(l)
    if (fl != "") {
      if (!fence) { fence = 1; fopen = fl } else if (fence_closes(fl, fopen)) fence = 0
      continue
    }
    # Inside a fenced block the line is the command. Outside it, only what is inside backticks is --
    # prose that merely names the flag, as in "`go test -run` exits 0", is not a command anyone runs.
    if (fence) { emit(l, i); continue }

    rest = l
    # RSTART/RLENGTH are global and emit() runs match() of its own, so this loop's position has to
    # be taken before the call. Reading them after it advances by whatever emit() last matched --
    # which, on a failed match, is RSTART 0 and RLENGTH -1, leaving rest unchanged forever.
    while (match(rest, /`[^`]*`/)) {
      span_at = RSTART
      span_len = RLENGTH
      emit(substr(rest, span_at + 1, span_len - 2), i)
      rest = substr(rest, span_at + span_len)
    }

    # An inline-code span may WRAP, and a Verify line long enough to hold two package arguments is
    # exactly the one that wraps -- so reading one line at a time skips the commands most likely to
    # be wrong. This joins the NEXT line only, and only when what follows the dangling backtick is
    # a command.
    #
    # Deliberately no carried-over "span is open" state. This prose wraps inline code constantly
    # (one spec here has 39 lines with an odd backtick count), so tracking parity across the file
    # mispairs one span and then every span after it -- measured: it silently emptied a whole file.
    # Bounding the join to one line means a stray backtick costs at most one wrong reading.
    k = index(rest, "`")
    if (k > 0 && i < FNR) {
      tail = substr(rest, k + 1)
      if (index(tail, "go test") > 0) {
        nxt = lines[i + 1]
        m = index(nxt, "`")
        emit(tail " " (m > 0 ? substr(nxt, 1, m - 1) : nxt), i)
      }
    }
  }
}
AWKX

# The test names reachable from one `go test` package argument list, one per line. The caller has
# already rejected a list this cannot resolve, so every path here is "./"-relative.
candidates() {
  for p in $1; do
    base="${p#./}"
    case "$base" in
      *"/...")
        base="${base%/...}"
        awk -F'\t' -v b="$base" '$1 == b || index($1, b "/") == 1 { print $2 }' "$WORK/index"
        ;;
      "...")
        cut -f2 "$WORK/index"
        ;;
      *)
        base="${base%/}"
        awk -F'\t' -v b="$base" '$1 == b { print $2 }' "$WORK/index"
        ;;
    esac
  done
}

checked=0
exempted=0
for f in $CORPUS; do
  # A path that matches no file is REPORTED, not skipped. These lists are word-split, so a corpus
  # path containing a space would arrive here as fragments -- and silently skipping them is a way
  # for a file to escape rule 3 with the gate still green.
  if [ ! -f "$f" ]; then
    err "the corpus lists '$f', which is not a file — a path with a space in it splits into fragments and would otherwise be skipped silently"
    continue
  fi
  awk "$EXTRACT" "$f" > "$WORK/cmds"
  while IFS="$(printf '\t')" read -r at pat pkgs nrun exempt unresolvable; do
    [ -n "$pat" ] || continue
    checked=$((checked + 1))
    [ "$pkgs" = "-" ] && pkgs=""

    if [ "${exempt:-0}" -eq 1 ]; then
      printf '  exempt: %s:%s: -run %s is marked NOT RE-RUNNABLE, so it is not required to select anything\n' \
        "$f" "$at" "'$pat'"
      exempted=$((exempted + 1))
      continue
    fi

    # A second -run is a different defect from a -run that selects nothing, and it needs a
    # different fix, so it is reported as itself rather than folded into the message below.
    # Measured on `go test ./pkg/worker/kvcache/ -run LeaderWorkload
    # ./pkg/worker/controllers/worker/ -run KVCacheBackend`: ONE package runs, the second is
    # consumed as an argument to the test binary, and the last -run is the one in force.
    if [ "${nrun:-1}" -gt 1 ]; then
      err "$f:$at: this command carries ${nrun} -run flags. Only the last one is in force, and every package after the first flag is passed to the test binary instead of being tested, so most of what the line names never runs -- and it still exits 0."
      continue
    fi

    if [ "${unresolvable:-0}" -eq 1 ]; then
      err "$f:$at: this command's package arguments cannot be resolved (only ./-relative paths are), so whether -run '$pat' selects anything is unknown. Name the packages as ./-relative paths."
      continue
    fi

    # -run splits on "/" and matches each element against one nesting level; only the first
    # element decides whether any top-level test is selected at all.
    top="${pat%%/*}"
    candidates "$pkgs" | sort -u > "$WORK/names"
    if [ ! -s "$WORK/names" ]; then
      err "$f:$at: 'go test $pkgs' matches no package that has tests, so -run '$pat' can select nothing"
      continue
    fi
    set +e
    grep -cE -- "$top" "$WORK/names" > "$WORK/hits"
    rc=$?
    set -e
    # grep exits 1 for "no match" and 2 or more for a bad pattern. Folding the second into the
    # first would report a pattern this checker cannot parse as a pattern that selects nothing.
    # This check reads the pattern as a POSIX ERE while `go test` reads it as RE2. The two agree on
    # the constructs this corpus uses -- literals and alternation, measured -- but diverge on \b,
    # (?i) and some escapes, so the message says which engine judged it.
    if [ "$rc" -ge 2 ]; then
      err "$f:$at: -run '$pat' is not a pattern this check can evaluate (grep -E, a POSIX ERE, rejected it; go test reads -run as RE2, so hand-check this one)"
      continue
    fi
    if [ "$rc" -eq 1 ]; then
      err "$f:$at: -run '$pat' selects no test in ${pkgs:-the tree}. go test exits 0 having run nothing, so this line reports success either way."
    fi
  done < "$WORK/cmds"
done

echo
if [ "$errors" -gt 0 ]; then
  echo "FAIL: $errors problem(s)."
  exit 1
fi
printf 'OK: %s specs checked; %s "go test -run" command(s) matched against %s top-level test names,\n    of which %s are exempt as NOT RE-RUNNABLE and were not required to select anything.\n' \
  "$(printf '%s\n' "$SPECS" | awk 'NF { n++ } END { print n + 0 }')" \
  "$checked" \
  "$(cut -f2 "$WORK/index" | sort -u | wc -l | tr -d ' ')" \
  "$exempted"
