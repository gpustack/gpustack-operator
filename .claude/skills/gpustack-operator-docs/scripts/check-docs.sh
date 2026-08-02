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
#   4. a docs/ page missing from the "## All pages" table in docs/README.md.
#
# Usage: bash .claude/skills/gpustack-operator-docs/scripts/check-docs.sh [repo-root]
set -euo pipefail

ROOT="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)}"
cd "$ROOT"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

errors=0
err() { printf '  %s\n' "$*"; errors=$((errors + 1)); }

# Pages whose structure is under contract.
PAGES=$(find docs -name '*.md' | sort)
# Everything whose links are checked.
LINKED_PAGES=$(printf '%s\n%s\n%s\n%s\n' \
  "README.md" "CLAUDE.md" "$PAGES" "$(find .claude/skills -name '*.md' | sort)")

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

echo
if [ "$errors" -gt 0 ]; then
  echo "FAIL: $errors problem(s)."
  exit 1
fi
echo "OK: $(printf '%s\n' "$PAGES" | wc -l | tr -d ' ') docs pages checked; links also verified across README.md, CLAUDE.md and .claude/skills."
