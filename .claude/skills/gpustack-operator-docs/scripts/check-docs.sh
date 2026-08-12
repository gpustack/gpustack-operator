#!/usr/bin/env bash
# check-docs.sh — the documentation contract for this repository.
#
# Scope:
#   * links and #anchors — README.md, CLAUDE.md, docs/**/*.md, .claude/skills/**/*.md
#   * page structure and index coverage — docs/**/*.md only
#
# Fails on:
#   1. a relative link whose file does not exist, or whose #anchor no heading produces;
#   2. a "## Contents" list that no longer matches the page's ## headings (missing, extra, reordered);
#   3. a docs/ page whose header block lacks Purpose / Audience / Prerequisites / Read time, or that
#      has no "**See also**" footer at its end;
#   4. a docs/ page missing from the "## All pages" table in docs/README.md;
#   5. a prose paragraph longer than the cap (a set of items belongs in a list or a table);
#   6. a page longer than the line cap;
#   7. a page with more "##" sections than the cap;
#   8. an index label in docs/README.md that differs from the page's own H1.
#
# Usage: bash .claude/skills/gpustack-operator-docs/scripts/check-docs.sh [--report] [repo-root]
#
# --report downgrades rules 5-8 to warnings and prints the per-page metrics, so one page can be
# measured while the rest of the corpus is still unrefined. Rules 1-4 are fatal in both modes.
set -euo pipefail

REPORT=0
ROOT=""
for arg in "$@"; do
  case "$arg" in
    --report) REPORT=1 ;;
    -*) echo "unknown flag: $arg" >&2; exit 2 ;;
    *) ROOT="$arg" ;;
  esac
done
[ -n "$ROOT" ] || ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
cd "$ROOT"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

errors=0
warnings=0
err() { printf '  %s\n' "$*"; errors=$((errors + 1)); }
# Rules 5-8 report shape rather than breakage, so --report demotes them to warnings.
shape_err() {
  if [ "$REPORT" -eq 1 ]; then
    printf '  warn: %s\n' "$*"
    warnings=$((warnings + 1))
  else
    err "$@"
  fi
}

# Pages whose structure is under contract.
PAGES=$(find docs -name '*.md' | sort)
# Everything whose links are checked.
LINKED_PAGES=$(printf '%s\n%s\n%s\n%s\n' \
  "README.md" "CLAUDE.md" "$PAGES" "$(find .claude/skills -name '*.md' | sort)")

# Shape caps (rules 5-7).
PARA_LINE_CAP=5    # source lines in one prose paragraph
PARA_CHAR_CAP=500  # characters of rendered text in one prose paragraph
PAGE_LINE_CAP=450  # lines in one page
H2_CAP=10          # "##" sections in one page

# Cap exemptions, declared here rather than escaped inline. Each is a space-separated glob list.
# The two recorded pages are mostly captured terminal output, which is not ours to shorten.
LINE_CAP_EXEMPT='docs/walkthrough.md docs/operation/nvidia-mig.md'
# The index is a table of contents and a reference page is a lookup table: both are meant to be flat.
H2_CAP_EXEMPT='docs/README.md docs/reference/*'

# --- helpers ----------------------------------------------------------------

# awk prelude: strip_links() removes [text](url) down to text — awk's gsub has no backreferences,
# so it is done with a match() loop; slug() is GitHub's heading-anchor form.
read -r -d '' AWK_LIB <<'AWKLIB' || true
function strip_links(t,   m, txt) {
  while (match(t, /\[[^]]*\]\([^)]*\)/)) {
    m = substr(t, RSTART, RLENGTH)
    txt = m
    sub(/^\[/, "", txt)
    sub(/\]\([^)]*\)$/, "", txt)
    t = substr(t, 1, RSTART - 1) txt substr(t, RSTART + RLENGTH)
  }
  return t
}
function slug(t,   a) {
  a = tolower(strip_links(t))
  gsub(/`|\*/, "", a)
  gsub(/[^a-z0-9 _-]/, "", a)
  sub(/^ +/, "", a); sub(/ +$/, "", a)
  gsub(/ /, "-", a)
  return a
}
AWKLIB

# Resolve "a/b/../c" without touching the filesystem.
normpath() {
  awk -v p="$1" 'BEGIN {
    n = split(p, a, "/"); m = 0
    for (i = 1; i <= n; i++) {
      if (a[i] == "." || a[i] == "") continue
      if (a[i] == "..") { if (m > 0) m--; continue }
      st[++m] = a[i]
    }
    out = ""
    for (i = 1; i <= m; i++) out = out (i > 1 ? "/" : "") st[i]
    print out
  }'
}

# Every heading as a GitHub anchor slug, duplicates suffixed the way GitHub does.
anchors_of() {
  awk "$AWK_LIB"'
    /^```/ { fence = !fence; next }
    fence  { next }
    /^#{1,6} / {
      sub(/^#+ +/, "")
      a = slug($0)
      seen[a]++
      print (seen[a] > 1) ? a "-" (seen[a] - 1) : a
    }
  ' "$1"
}

# The ## headings only, as anchors, with "Contents" itself dropped.
h2_anchors_of() {
  awk "$AWK_LIB"'
    /^```/ { fence = !fence; next }
    fence  { next }
    /^## / {
      sub(/^## +/, "")
      if ($0 == "Contents") next
      print slug($0)
    }
  ' "$1"
}

# The anchors the "## Contents" list links to, in order.
toc_anchors_of() {
  awk '
    /^## Contents/ { intoc = 1; next }
    intoc && /^#{1,6} / { intoc = 0 }
    intoc && /^- \[/ {
      if (match($0, /\(#[^)]*\)/)) print substr($0, RSTART + 2, RLENGTH - 3)
    }
  ' "$1"
}

# Every link target outside code fences, one per line.
links_of() {
  awk '
    /^```/ { fence = !fence; next }
    fence  { next }
    {
      line = $0
      while (match(line, /\]\([^)]+\)/)) {
        print substr(line, RSTART + 2, RLENGTH - 3)
        line = substr(line, RSTART + RLENGTH)
      }
    }
  ' "$1"
}

store_for() { printf '%s/%s.anchors' "$WORK" "$(printf '%s' "$1" | tr / _)"; }

# Does a page match any glob in a space-separated exemption list?
exempt() {
  for pat in $2; do
    # shellcheck disable=SC2254  # the pattern is a glob on purpose: docs/reference/*
    case "$1" in $pat) return 0 ;; esac
  done
  return 1
}

# The page's H1, without the leading hashes.
h1_of() { awk '/^# / { sub(/^# +/, ""); print; exit }' "$1"; }

# How many "##" sections a page has, fenced blocks excluded.
h2_count_of() {
  awk '
    /^[ \t]*```/ { fence = !fence; next }
    fence  { next }
    /^## /  { c++ }
    END     { print c + 0 }
  ' "$1"
}

# Prose paragraphs over the cap, as "startline<TAB>lines<TAB>chars". Fenced blocks, headings, tables,
# lists, blockquotes, indented continuations and the See also / Next footer are not prose: each ends
# the paragraph it interrupts. Characters are counted on the rendered text, so a link costs its label.
long_paragraphs_of() {
  awk -v lcap="$PARA_LINE_CAP" -v ccap="$PARA_CHAR_CAP" "$AWK_LIB"'
    function flush() {
      if (n > 0 && (n > lcap || chars > ccap)) printf "%d\t%d\t%d\n", start, n, chars
      n = 0; chars = 0
    }
    /^[ \t]*```/               { flush(); fence = !fence; next }
    fence                      { next }
    /^[ \t]*$/                 { flush(); next }
    /^[ \t]/                   { flush(); next }
    /^[#>|]/                   { flush(); next }
    /^([-*+]|[0-9]+[.)]) /     { flush(); next }
    /^\*\*(See also|Next)\*\*/ { flush(); footer = 1; next }
    footer                     { next }
    {
      if (n == 0) start = FNR
      n++
      chars += length(strip_links($0)) + 1
    }
    END { flush() }
  ' "$1"
}

# The "## All pages" rows of the index, as "path<TAB>label".
index_labels() {
  awk '
    /^## All pages/   { intable = 1; next }
    intable && /^## / { intable = 0 }
    intable && /^\| *\[/ {
      if (!match($0, /\[[^]]*\]\([^)]*\)/)) next
      m = substr($0, RSTART, RLENGTH)
      label = m; sub(/^\[/, "", label); sub(/\]\([^)]*\)$/, "", label)
      target = m; sub(/^\[[^]]*\]\(/, "", target); sub(/\)$/, "", target)
      sub(/#.*$/, "", target)
      print target "\t" label
    }
  ' docs/README.md
}

for f in $LINKED_PAGES; do
  [ -f "$f" ] || continue
  anchors_of "$f" > "$(store_for "$f")"
done

# --- 1. links ---------------------------------------------------------------
echo "==> links"
for f in $LINKED_PAGES; do
  [ -f "$f" ] || continue
  dir=$(dirname "$f")
  while IFS= read -r target; do
    case "$target" in
      http://*|https://*|mailto:*|"") continue ;;
    esac
    path="${target%%#*}"
    anchor=""
    case "$target" in *#*) anchor="${target#*#}" ;; esac

    if [ -z "$path" ]; then
      resolved="$f"
    else
      if [ "$dir" = "." ]; then resolved=$(normpath "$path"); else resolved=$(normpath "$dir/$path"); fi
      if [ ! -e "$resolved" ]; then
        err "$f -> $target (no such file: $resolved)"
        continue
      fi
    fi

    case "$resolved" in
      *.md)
        if [ -n "$anchor" ]; then
          st=$(store_for "$resolved")
          [ -f "$st" ] || anchors_of "$resolved" > "$st"
          grep -qxF "$anchor" "$st" || err "$f -> $target (no such anchor in $resolved)"
        fi
        ;;
    esac
  done < <(links_of "$f")
done

# --- 2. Contents, 3. header block and footer --------------------------------
echo "==> page structure"
for f in $PAGES; do
  [ "$f" = "docs/README.md" ] && continue

  header=$(sed -n '2,8p' "$f")
  for field in Purpose Audience Prerequisites 'Read time'; do
    printf '%s\n' "$header" | grep -q "\*\*${field}\*\*" \
      || err "$f: the header block below the H1 does not state **${field}**"
  done
  tail -n 12 "$f" | grep -q '^\*\*See also\*\*' \
    || err "$f: no '**See also**' footer at the end of the page"

  if ! grep -q '^## Contents' "$f"; then
    err "$f: missing the '## Contents' list"
    continue
  fi

  h2_anchors_of "$f" > "$WORK/h2"
  toc_anchors_of "$f" > "$WORK/toc"
  if ! diff -q "$WORK/h2" "$WORK/toc" >/dev/null; then
    err "$f: '## Contents' does not match the page's ## headings:"
    diff "$WORK/toc" "$WORK/h2" \
      | sed -e 's/^</      only in Contents: /' -e 's/^>/      only in headings: /' \
      | grep -E 'only in' || true
  fi
done

# --- 4. index coverage ------------------------------------------------------
echo "==> docs/README.md index coverage"
# Only the "## All pages" table counts — a mention on a reading path is not a registration.
awk '/^## All pages/ { intable = 1; next } intable && /^## / { intable = 0 } intable' \
  docs/README.md > "$WORK/allpages"
[ -s "$WORK/allpages" ] || err "docs/README.md: no '## All pages' section"
for f in $PAGES; do
  [ "$f" = "docs/README.md" ] && continue
  rel="${f#docs/}"
  escaped=$(printf '%s' "$rel" | sed 's/[].[^$*\\/]/\\&/g')
  if ! grep -qE "\(${escaped}[)#]" "$WORK/allpages"; then
    err "$f: not in the '## All pages' table of docs/README.md"
  fi
done

# --- 5. paragraph cap, 6. line cap, 7. section cap --------------------------
echo "==> size and shape"
: > "$WORK/metrics"
for f in $PAGES; do
  bytes=$(wc -c < "$f" | tr -d ' ')
  lines=$(wc -l < "$f" | tr -d ' ')
  h2=$(h2_count_of "$f")
  long_paragraphs_of "$f" > "$WORK/paras"

  while IFS="$(printf '\t')" read -r at n chars; do
    shape_err "$f:$at: prose paragraph of $n lines / $chars chars, over the ${PARA_LINE_CAP}-line / ${PARA_CHAR_CAP}-char cap"
  done < "$WORK/paras"

  exempt "$f" "$LINE_CAP_EXEMPT" || [ "$lines" -le "$PAGE_LINE_CAP" ] \
    || shape_err "$f: $lines lines, over the ${PAGE_LINE_CAP}-line cap"
  exempt "$f" "$H2_CAP_EXEMPT" || [ "$h2" -le "$H2_CAP" ] \
    || shape_err "$f: $h2 '##' sections, over the cap of $H2_CAP"

  printf '%s\t%s\t%s\t%s\t%s\n' \
    "$f" "$bytes" "$lines" "$h2" "$(wc -l < "$WORK/paras" | tr -d ' ')" >> "$WORK/metrics"
done

# --- 8. index label matches the page H1 -------------------------------------
echo "==> docs/README.md index labels"
index_labels > "$WORK/labels"
for f in $PAGES; do
  [ "$f" = "docs/README.md" ] && continue
  label=$(awk -F'\t' -v r="${f#docs/}" '$1 == r { print $2; exit }' "$WORK/labels")
  # An absent row is rule 4's finding, not this one's.
  [ -n "$label" ] || continue
  h1=$(h1_of "$f")
  [ "$label" = "$h1" ] \
    || shape_err "$f: index label '$label' is not the page's H1 '$h1'"
done

if [ "$REPORT" -eq 1 ]; then
  echo
  echo "==> report"
  awk -F'\t' '
    BEGIN { printf "  %-48s %7s %6s %4s %6s\n", "page", "bytes", "lines", "##", "paras" }
          { printf "  %-48s %7d %6d %4d %6d\n", $1, $2, $3, $4, $5; b += $2; l += $3; p += $5 }
    END   { printf "  %-48s %7d %6d %4s %6d\n", "total", b, l, "-", p }
  ' "$WORK/metrics"
fi

echo
if [ "$errors" -gt 0 ]; then
  echo "FAIL: $errors problem(s)."
  exit 1
fi
summary="OK: $(printf '%s\n' "$PAGES" | wc -l | tr -d ' ') docs pages checked; links also verified across README.md, CLAUDE.md and .claude/skills."
[ "$warnings" -eq 0 ] || summary="$summary
WARN: $warnings shape warning(s); rules 5-8 are advisory under --report."
echo "$summary"
