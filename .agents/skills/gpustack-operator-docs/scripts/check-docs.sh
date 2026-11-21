#!/usr/bin/env bash
# check-docs.sh — the documentation contract for this repository.
#
# Scope:
#   * links and #anchors — README.md, AGENTS.md, Copilot instructions, docs/**/*.md, .agents/skills/**/*.md
#   * page structure and index coverage — docs/**/*.md only
#   * strict three-zone governance (activates when docs/reference/README.md exists):
#       - max heading depth H3 across docs/**/*.md (including blockquoted H4+);
#       - public-page H2 cap (<= 8), no layout-container heading nouns, no leading articles on non-Step H2-H3;
#       - no blank "not recorded" environment rows or dev-journal "recorded on" stamps in public config guides;
#       - architecture.md <= 200 lines and ## Components synced with cmd/ subcommands and staged runtime packages;
#       - flat docs/reference/ layout (zero subdirectories);
#       - one-way link isolation from public docs/ and site/content/** pages;
#       - three-table docs/README.md coverage (All pages, Reference, Contribute) + H1 label parity;
#       - docs/reference/README.md hub routing/aliases/evidence checks and topic-page shape;
#       - authored-prose kind-plural check across docs/** and site/content/**.
#
# Usage: bash .agents/skills/gpustack-operator-docs/scripts/check-docs.sh [--report] [repo-root]
#
# --report downgrades rules 5-8 to warnings and prints the per-page metrics, so one page can be
# measured while the rest of the corpus is still unrefined. Rules 1-4 and 9+ are fatal in both modes.
set -euo pipefail

REPORT=0
SELFTEST=0
ROOT=""
for arg in "$@"; do
  case "$arg" in
    --report) REPORT=1 ;;
    --selftest) SELFTEST=1 ;;
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

STRICT_ZONES=0
if [ -f "docs/reference/README.md" ]; then
  STRICT_ZONES=1
fi

# Pages whose structure is under contract.
PAGES=$(find docs -name '*.md' | sort)
SITE_CONTENT_PAGES=""
if [ -d "site/content" ]; then
  SITE_CONTENT_PAGES=$(find site/content -name '*.md' | sort)
fi
HEADING_PAGES=$( {
  printf '%s\n' "$PAGES"
  if [ -n "$SITE_CONTENT_PAGES" ]; then
    printf '%s\n' "$SITE_CONTENT_PAGES"
  fi
  printf '%s\n' README.md ADOPTERS.md \
    csrc/amd/rocm-slicing-shim/README.md \
    csrc/thead/ppu-slicing-shim/README.md
} | awk 'NF' | sort -u )

SKILL_MD_PAGES=""
if [ -d ".agents/skills" ]; then
  SKILL_MD_PAGES=$(find .agents/skills -name '*.md' | sort)
fi
LINKED_PAGES=$( {
  printf '%s\n' "README.md" "AGENTS.md" ".github/copilot-instructions.md" "$PAGES"
  if [ -n "$SKILL_MD_PAGES" ]; then
    printf '%s\n' "$SKILL_MD_PAGES"
  fi
} | awk 'NF' )

# Shape caps (rules 5-7).
PARA_LINE_CAP=5     # source lines in one prose paragraph
PARA_CHAR_CAP=500   # characters of rendered text in one prose paragraph
PAGE_LINE_CAP=1000  # lines in one page — a backstop against a page nobody split, not a budget
ARCH_LINE_CAP=200   # max lines in docs/getting-started/architecture.md in strict three-zone mode
H2_CAP=10           # "##" sections in one page (contribute / legacy mode)
PUBLIC_H2_CAP=8     # "##" sections in one public page in strict three-zone mode
HEADING_WORD_CAP=4  # visible words after a numbered prefix, if any
HEADING_CHAR_CAP=32 # visible characters after a numbered prefix, if any
MAX_HEADING_DEPTH=3 # H1-H3 ceiling across docs/** in strict three-zone mode

# Cap exemptions, declared here rather than escaped inline. Each is a space-separated glob list.
LINE_CAP_EXEMPT='docs/walkthroughs/devices/scheduling.md docs/operate/* docs/modules/devices/*-mig.md'
H2_CAP_EXEMPT='docs/README.md docs/reference/*'

# Shared awk helpers (POSIX / macOS onetrueawk compatible; no gawk extensions).
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
function fence_open(l,   n, c, rest) {
  sub(/^( {0,3}> ?)+/, "", l)
  if (l !~ /^ {0,3}(`{3,}|~{3,})/) return ""
  sub(/^ +/, "", l)
  c = substr(l, 1, 1)
  n = 0
  while (substr(l, n + 1, 1) == c) n++
  rest = substr(l, n + 1)
  if (c == "`" && index(rest, "`") > 0) return ""
  return c n
}
function fence_close(l, open,   n, c, rest) {
  sub(/^( {0,3}> ?)+/, "", l)
  if (l !~ /^ {0,3}(`{3,}|~{3,})/) return 0
  sub(/^ +/, "", l)
  c = substr(l, 1, 1)
  if (c != substr(open, 1, 1)) return 0
  n = 0
  while (substr(l, n + 1, 1) == c) n++
  rest = substr(l, n + 1)
  if (rest !~ /^[ \t]*$/) return 0
  return n >= (substr(open, 2) + 0)
}
function skip_code_span(s, pos,   n, p, tlen, rs) {
  n = length(s); p = pos
  while (p <= n && substr(s, p, 1) == "`") p++
  tlen = p - pos
  while (p <= n) {
    if (substr(s, p, 1) == "`") {
      rs = p
      while (p <= n && substr(s, p, 1) == "`") p++
      if (p - rs == tlen) return p
      continue
    }
    p++
  }
  return pos + tlen
}
# Strip HTML comments while skipping inline code spans so "`<!--`" stays literal code.
function uncomment(line,   n, pos, ch, cend, end_pos, out) {
  out = ""
  n = length(line)
  pos = 1
  while (pos <= n) {
    if (html_comment) {
      end_pos = index(substr(line, pos), "-->")
      if (!end_pos) return out
      html_comment = 0
      pos += end_pos + 2
      continue
    }
    ch = substr(line, pos, 1)
    if (ch == "\\" && pos < n) {
      out = out substr(line, pos, 2)
      pos += 2
      continue
    }
    if (ch == "`") {
      cend = skip_code_span(line, pos)
      out = out substr(line, pos, cend - pos)
      pos = cend
      continue
    }
    if (substr(line, pos, 4) == "<!--") {
      html_comment = 1
      pos += 4
      continue
    }
    out = out ch
    pos++
  }
  return out
}
# Track standalone 4-space/tab indented code blocks while preserving list/footnote continuations.
function is_indented_code_line(raw,   is_ind) {
  if (raw ~ /^[ \t]*$/) {
    md_prev_blank = 1
    return 0
  }
  is_ind = (raw ~ /^(    |\t)/)
  if (md_in_indent) {
    if (is_ind) {
      md_prev_blank = 0
      return 1
    }
    md_in_indent = 0
  } else if (md_prev_blank && md_container == "" && is_ind) {
    md_in_indent = 1
    md_prev_blank = 0
    return 1
  }
  if (raw ~ /^( {0,3}> ?)* {0,3}([-*+]|[0-9]+[.)])[ \t]/) md_container = "list"
  else if (raw ~ /^( {0,3}> ?)* {0,3}\[\^[^]]+\]:/) md_container = "footnote"
  else if (!is_ind) md_container = ""
  md_prev_blank = 0
  return 0
}
function slug(t,   a) {
  a = tolower(strip_links(t))
  gsub(/`|\*/, "", a)
  gsub(/[^a-z0-9 _-]/, "", a)
  sub(/^ +/, "", a); sub(/ +$/, "", a)
  gsub(/ /, "-", a)
  return a
}
function norm_relpath(p,   n, a, i, m, st, out) {
  n = split(p, a, "/"); m = 0
  for (i = 1; i <= n; i++) {
    if (a[i] == "." || a[i] == "") continue
    if (a[i] == "..") {
      if (m > 0 && st[m] != "..") { m--; continue }
    }
    st[++m] = a[i]
  }
  out = ""
  for (i = 1; i <= m; i++) out = out (i > 1 ? "/" : "") st[i]
  return out
}
function hex_val(c,   p) {
  p = index("0123456789abcdef", tolower(c))
  return (p > 0) ? (p - 1) : -1
}
function url_decode(s,   n, i, out, h1, h2, code) {
  out = ""
  n = length(s)
  i = 1
  while (i <= n) {
    if (substr(s, i, 1) == "%" && i + 2 <= n) {
      h1 = hex_val(substr(s, i + 1, 1))
      h2 = hex_val(substr(s, i + 2, 1))
      if (h1 >= 0 && h2 >= 0) {
        code = h1 * 16 + h2
        out = out sprintf("%c", code)
        i += 3
        continue
      }
    }
    out = out substr(s, i, 1)
    i++
  }
  return out
}
AWKLIB

# Resolve "a/b/../c" without touching the filesystem.
normpath() {
  awk -v p="$1" "$AWK_LIB"'BEGIN { print norm_relpath(p) }'
}

# Every heading as a GitHub anchor slug, allocating duplicates against all emitted IDs.
anchors_of() {
  awk "$AWK_LIB"'
    BEGIN { md_prev_blank = 1 }
    FNR == 1 && $0 == "---" { frontmatter = 1; next }
    frontmatter { if ($0 == "---") frontmatter = 0; next }
    !html_comment && !md_in_indent {
      if (!fence) {
        fo = fence_open($0)
        if (fo != "") { fence = 1; fopen = fo; md_prev_blank = 0; next }
      } else {
        if (fence_close($0, fopen)) fence = 0
        md_prev_blank = 0
        next
      }
    }
    fence { next }
    !html_comment && is_indented_code_line($0) { next }
    {
      line = uncomment($0)
      sub(/^( {0,3}> ?)+/, "", line)
      if (line ~ /^ {0,3}#{1,6}[ \t]+/) {
        sub(/^ {0,3}#+[ \t]+/, "", line)
        sub(/[ \t]+#+[ \t]*$/, "", line)
        a = slug(line)
        cand = a
        k = 1
        while (cand in used) {
          cand = a "-" k
          k++
        }
        used[cand] = 1
        print cand
      }
    }
  ' "$1"
}

# The ## headings only, as anchors, with "Contents" itself dropped.
h2_anchors_of() {
  awk "$AWK_LIB"'
    BEGIN { md_prev_blank = 1 }
    !html_comment && !md_in_indent {
      if (!fence) {
        fo = fence_open($0)
        if (fo != "") { fence = 1; fopen = fo; md_prev_blank = 0; next }
      } else {
        if (fence_close($0, fopen)) fence = 0
        md_prev_blank = 0
        next
      }
    }
    fence { next }
    !html_comment && is_indented_code_line($0) { next }
    {
      line = uncomment($0)
      if (line ~ /^## /) {
        sub(/^## +/, "", line)
        if (line == "Contents") next
        print slug(line)
      }
    }
  ' "$1"
}

# The anchors the "## Contents" list links to, in order.
toc_anchors_of() {
  awk "$AWK_LIB"'
    BEGIN { md_prev_blank = 1 }
    !html_comment && !md_in_indent {
      if (!fence) {
        fo = fence_open($0)
        if (fo != "") { fence = 1; fopen = fo; md_prev_blank = 0; next }
      } else {
        if (fence_close($0, fopen)) fence = 0
        md_prev_blank = 0
        next
      }
    }
    fence { next }
    !html_comment && is_indented_code_line($0) { next }
    {
      line = uncomment($0)
      if (line ~ /^## Contents([ \t]*$)/) { intoc = 1; next }
      if (intoc && line ~ /^#{1,6} /) { intoc = 0 }
      if (intoc && line ~ /^- \[/) {
        if (match(line, /\(#[^)]*\)/)) print substr(line, RSTART + 2, RLENGTH - 3)
      }
    }
  ' "$1"
}

# Every link target outside code fences, standalone indented code blocks, inline code and comments.
links_of() {
  awk "$AWK_LIB"'
    function extract_inline(s, at,   n, pos, ch, q, depth, inner, d, gt, pdepth, dest, p, auto) {
      n = length(s); pos = 1
      while (pos <= n) {
        ch = substr(s, pos, 1)
        if (ch == "\\") { pos += 2; continue }
        if (ch == "`") { pos = skip_code_span(s, pos); continue }
        if (ch == "<") {
          gt = pos + 1
          while (gt <= n && substr(s, gt, 1) !~ /[ \t<>]/) gt++
          if (gt <= n && substr(s, gt, 1) == ">") {
            auto = substr(s, pos + 1, gt - pos - 1)
            if (auto ~ /^[Hh][Tt][Tt][Pp][Ss]?:\/\/[^ \t<>]+$/) {
              printf "%d\t%s\n", at, auto
              pos = gt + 1
              continue
            }
          }
        }
        if (ch == "[") {
          q = pos + 1; depth = 1
          while (q <= n && depth > 0) {
            ch = substr(s, q, 1)
            if (ch == "\\") { q += 2; continue }
            if (ch == "`") { q = skip_code_span(s, q); continue }
            if (ch == "[") depth++
            else if (ch == "]") depth--
            if (depth > 0) q++
          }
          if (depth == 0) {
            inner = substr(s, pos + 1, q - pos - 1)
            if (index(inner, "[") > 0) extract_inline(inner, at)
            if (substr(s, q + 1, 1) == "(") {
              d = q + 2
              while (d <= n && substr(s, d, 1) ~ /[ \t]/) d++
              dest = ""; p = d
              if (substr(s, d, 1) == "<") {
                gt = d + 1
                while (gt <= n && substr(s, gt, 1) != ">") {
                  if (substr(s, gt, 1) == "\\") gt++
                  gt++
                }
                if (gt <= n) {
                  dest = substr(s, d + 1, gt - d - 1)
                  p = gt + 1
                }
              } else {
                pdepth = 0
                while (p <= n) {
                  ch = substr(s, p, 1)
                  if (ch == "\\") { p += 2; continue }
                  if (ch ~ /[ \t]/) break
                  if (ch == "(") pdepth++
                  else if (ch == ")") {
                    if (pdepth == 0) break
                    pdepth--
                  }
                  p++
                }
                dest = substr(s, d, p - d)
              }
              while (p <= n && substr(s, p, 1) != ")") {
                if (substr(s, p, 1) == "\\") p++
                p++
              }
              if (p <= n && dest != "") {
                printf "%d\t%s\n", at, dest
                pos = p + 1
                continue
              }
            }
          }
        }
        pos++
      }
    }
    function parse_ref_dest(rest,   gt, p, ch) {
      sub(/^[ \t]+/, "", rest)
      if (rest == "") return ""
      if (substr(rest, 1, 1) == "<") {
        gt = index(substr(rest, 2), ">")
        if (gt > 0) return substr(rest, 2, gt - 1)
        return ""
      }
      p = 1
      while (p <= length(rest)) {
        ch = substr(rest, p, 1)
        if (ch ~ /[ \t]/) break
        p++
      }
      return substr(rest, 1, p - 1)
    }
    function flush_para() {
      if (para != "") {
        extract_inline(para, para_start)
        para = ""
      }
    }
    BEGIN { md_prev_blank = 1 }
    FNR == 1 && $0 == "---" { frontmatter = 1; next }
    frontmatter { if ($0 == "---") frontmatter = 0; next }
    !html_comment && !md_in_indent {
      if (!fence) {
        fo = fence_open($0)
        if (fo != "") { flush_para(); pending_ref = 0; fence = 1; fopen = fo; md_prev_blank = 0; next }
      } else {
        if (fence_close($0, fopen)) fence = 0
        md_prev_blank = 0
        next
      }
    }
    fence { next }
    !html_comment && !pending_ref && is_indented_code_line($0) { flush_para(); next }
    {
      line = uncomment($0)
      if (line ~ /^[ \t]*$/) { flush_para(); pending_ref = 0; next }
      stripped = line
      sub(/^( {0,3}> ?)+/, "", stripped)
      if (pending_ref) {
        rd = parse_ref_dest(stripped)
        if (rd != "") printf "%d\t%s\n", pending_ref, rd
        pending_ref = 0
        next
      }
      if (stripped ~ /^ {0,3}\[[^\^][^]]*\]:[ \t]*/) {
        flush_para()
        rest = stripped
        sub(/^ {0,3}\[[^\^][^]]*\]:[ \t]*/, "", rest)
        if (rest == "") {
          pending_ref = FNR
        } else {
          rd = parse_ref_dest(rest)
          if (rd != "") printf "%d\t%s\n", FNR, rd
        }
        next
      }
      if (para == "") { para = line; para_start = FNR }
      else { para = para " " line }
    }
    END { flush_para() }
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

# True if a page carries a top-level "**Next**" footer outside code fences and HTML comments.
has_next_footer() {
  awk "$AWK_LIB"'
    BEGIN { md_prev_blank = 1 }
    !html_comment && !md_in_indent {
      if (!fence) {
        fo = fence_open($0)
        if (fo != "") { fence = 1; fopen = fo; md_prev_blank = 0; next }
      } else {
        if (fence_close($0, fopen)) fence = 0
        md_prev_blank = 0
        next
      }
    }
    fence { next }
    !html_comment && is_indented_code_line($0) { next }
    {
      line = uncomment($0)
      if (line ~ /^\*\*Next\*\*/) { found = 1; exit }
    }
    END { exit (found ? 0 : 1) }
  ' "$1"
}

# Check docs/reference/README.md for alias and evidence-label sections/markers.
check_reference_hub_sections() {
  awk "$AWK_LIB"'
    BEGIN { md_prev_blank = 1 }
    !html_comment && !md_in_indent {
      if (!fence) {
        fo = fence_open($0)
        if (fo != "") { fence = 1; fopen = fo; md_prev_blank = 0; next }
      } else {
        if (fence_close($0, fopen)) fence = 0
        md_prev_blank = 0
        next
      }
    }
    fence { next }
    !html_comment && is_indented_code_line($0) { next }
    {
      line = uncomment($0)
      low = tolower(line)
      if (low ~ /^#{2,3}[ \t]+.*alias/ || low ~ /\*\*[^*]*alias[^*]*\*\*/) has_alias = 1
      if (low ~ /^#{2,3}[ \t]+.*evidence/ || low ~ /\*\*[^*]*evidence[^*]*\*\*/) has_evidence_hdr = 1
      if (low ~ /(^|[^a-z])(contract|recorded capture|synthetic simulation)([^a-z]|$)/) has_evidence_label = 1
    }
    END {
      if (!has_alias) print "docs/reference/README.md: missing exact aliases section or marker"
      if (!has_evidence_hdr || !has_evidence_label) print "docs/reference/README.md: missing evidence labels section or evidence label (contract, recorded capture, or synthetic simulation)"
    }
  ' "$1"
}

# How many "##" sections a page has, fenced blocks excluded.
h2_count_of() {
  awk "$AWK_LIB"'
    BEGIN { md_prev_blank = 1 }
    !html_comment && !md_in_indent {
      if (!fence) {
        fo = fence_open($0)
        if (fo != "") { fence = 1; fopen = fo; md_prev_blank = 0; next }
      } else {
        if (fence_close($0, fopen)) fence = 0
        md_prev_blank = 0
        next
      }
    }
    fence { next }
    !html_comment && is_indented_code_line($0) { next }
    {
      line = uncomment($0)
      if (line ~ /^## / && line != "## Contents") c++
    }
    END { print c + 0 }
  ' "$1"
}

# Prose paragraphs over the cap, as "startline<TAB>lines<TAB>chars".
long_paragraphs_of() {
  awk -v lcap="$PARA_LINE_CAP" -v ccap="$PARA_CHAR_CAP" "$AWK_LIB"'
    function flush() {
      if (n > 0 && (n > lcap || chars > ccap)) printf "%d\t%d\t%d\n", start, n, chars
      n = 0; chars = 0
    }
    !html_comment {
      if (!fence) {
        fo = fence_open($0)
        if (fo != "") { flush(); fence = 1; fopen = fo; next }
      } else {
        if (fence_close($0, fopen)) fence = 0
        next
      }
    }
    fence                            { next }
    html_comment                     { if ($0 ~ /^[ \t]*-->[ \t]*$/) html_comment = 0; next }
    /^[ \t]*<!--/                    { flush(); if ($0 !~ /-->[ \t]*$/) html_comment = 1; next }
    /^[ \t]*$/                       { flush(); next }
    /^(    |\t)/                     { flush(); next }
    /^[ \t]*[#>|]/                   { flush(); next }
    /^[ \t]*([-*+]|[0-9]+[.)]) /     { flush(); next }
    /^\*\*(See also|Next)\*\*/       { flush(); footer = 1; next }
    footer                           { next }
    {
      line = $0
      sub(/^[ \t]+/, "", line)
      if (n == 0) start = FNR
      n++
      chars += length(strip_links(line)) + 1
    }
    END { flush() }
  ' "$1"
}

# H1-H6 headings in authored reader Markdown (including blockquoted H4+ in strict depth mode).
heading_issues_of() {
  awk -v wcap="$HEADING_WORD_CAP" -v ccap="$HEADING_CHAR_CAP" \
      -v interrogatives="${2:-0}" -v max_depth="${3:-0}" \
      -v strict_heading_style="${4:-0}" "$AWK_LIB"'
    function trim(t) {
      sub(/^[[:space:]]+/, "", t)
      sub(/[[:space:]]+$/, "", t)
      return t
    }
    function word_count(t,   n, rest) {
      n = 0
      rest = t
      while (match(rest, /[[:alnum:]]+/)) {
        n++
        rest = substr(rest, RSTART + RLENGTH)
      }
      return n
    }
    BEGIN { md_prev_blank = 1 }
    FNR == 1 && $0 == "---" { frontmatter = 1; next }
    frontmatter { if ($0 == "---") frontmatter = 0; next }
    !html_comment && !md_in_indent {
      if (!fence) {
        fo = fence_open($0)
        if (fo != "") { fence = 1; fopen = fo; md_prev_blank = 0; next }
      } else {
        if (fence_close($0, fopen)) fence = 0
        md_prev_blank = 0
        next
      }
    }
    fence { next }
    !html_comment && is_indented_code_line($0) { next }
    {
      line = uncomment($0)
      sub(/^( {0,3}> ?)+/, "", line)
      if (line !~ /^ {0,3}#{1,6}[[:space:]]/) next
      sub(/^ {0,3}/, "", line)
      level = line; sub(/[[:space:]].*/, "", level)
      raw = line
      sub(/^#+[[:space:]]+/, "", raw)
      sub(/[[:space:]]+#+[[:space:]]*$/, "", raw)
      visible = strip_links(raw)
      gsub(/`|\*|~/, "", visible)
      sub(/^__/, "", visible); sub(/__$/, "", visible)
      visible = trim(visible)
      if (max_depth > 0 && length(level) > max_depth) {
        printf "%d\tdepth\tH%d\t%s\n", FNR, length(level), visible
      }
      if (interrogatives) {
        bad = tolower(visible)
        gsub(/[^[:alnum:]]+/, " ", bad)
        n = split(bad, tokens, /[[:space:]]+/)
        for (i = 1; i <= n; i++)
          if (tokens[i] ~ /^(what|where|when|how|which|why|who|whose|whom)$/) {
            printf "%d\tword\t%s\t%s\n", FNR, tokens[i], visible
            break
          }
      }
      if (length(level) == 1) next
      topic = visible
      is_step = (topic ~ /^Step[[:space:]]+[[:alnum:].-]+:[[:space:]]*/)
      if (sub(/^(Step|Stage|Gate|Rule|Suite|State|Path)[[:space:]]+[[:alnum:].-]+:[[:space:]]*/, "", topic)) {
        topic = trim(topic)
        first = topic
        sub(/[[:space:]].*$/, "", first)
        if (topic ~ /^[[:lower:]]/ && first !~ /^(vLLM|llm-d-router|eBPF)$/)
          printf "%d\tcase\t\t%s\n", FNR, visible
      }
      if (strict_heading_style) {
        cbad = tolower(topic)
        gsub(/[^[:alnum:]]+/, " ", cbad)
        cn = split(cbad, ctokens, /[[:space:]]+/)
        for (ci = 1; ci <= cn; ci++) {
          if (ctokens[ci] ~ /^(rows?|tables?|columns?|guides?|entries|sections?)$/) {
            printf "%d\tcontainer\t%s\t%s\n", FNR, ctokens[ci], visible
            break
          }
        }
        if (!is_step && tolower(topic) ~ /^(a|an|the)[[:space:]]+/) {
          art = tolower(topic)
          sub(/[[:space:]].*$/, "", art)
          printf "%d\tarticle\t%s\t%s\n", FNR, art, visible
        }
      }
      words = word_count(topic)
      chars = length(topic)
      if (words > wcap || chars > ccap)
        printf "%d\tlength\t%d words / %d characters\t%s\n", FNR, words, chars, visible
    }
  ' "$1"
}

# Authored-prose noise findings on public docs as "line<TAB>kind<TAB>snippet".
# check_recorded_on=1 bans dev-journal "recorded/verified on <version>" stamps in non-benchmark guides.
prose_noise_issues_of() {
  awk -v check_recorded_on="${2:-0}" "$AWK_LIB"'
    BEGIN { md_prev_blank = 1 }
    FNR == 1 && $0 == "---" { frontmatter = 1; next }
    frontmatter { if ($0 == "---") frontmatter = 0; next }
    !html_comment && !md_in_indent {
      if (!fence) {
        fo = fence_open($0)
        if (fo != "") { fence = 1; fopen = fo; md_prev_blank = 0; next }
      } else {
        if (fence_close($0, fopen)) fence = 0
        md_prev_blank = 0
        next
      }
    }
    fence { next }
    !html_comment && is_indented_code_line($0) { next }
    {
      line = uncomment($0)
      low = tolower(line)
      if (low ~ /not recorded with this capture|versions? not recorded|\|[[:space:]]*not[[:space:]]+recorded[[:space:]]*\|/) {
        printf "%d\tblank_recorded\t%s\n", FNR, line
      }
      if (check_recorded_on && (low ~ /(^|[^[:alnum:]_])(recorded|verified)[[:space:]]+on[[:space:]]+([[:alnum:]_-]+[[:space:]]+)?([[:alnum:]_-]+[[:space:]]+)?(v[0-9]+\.[0-9]+|[0-9]+\.[0-9]+\.[0-9]+|[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9])/ || low ~ /(^|[^[:alnum:]_])(recorded|verified)[[:space:]]+on[[:space:]]+(vllm|sglang|kubernetes|k8s|kueue|helm|cuda|rocm|cann|ubuntu|python)[[:space:]]+v?[0-9]+\.[0-9]+/)) {
        printf "%d\trecorded_on\t%s\n", FNR, line
      }
    }
  ' "$1"
}

# Extract exact inline-code tokens from ## Components of docs/getting-started/architecture.md.
arch_component_tokens_of() {
  awk "$AWK_LIB"'
    BEGIN { md_prev_blank = 1 }
    !html_comment && !md_in_indent {
      if (!fence) {
        fo = fence_open($0)
        if (fo != "") { fence = 1; fopen = fo; md_prev_blank = 0; next }
      } else {
        if (fence_close($0, fopen)) fence = 0
        md_prev_blank = 0
        next
      }
    }
    fence { next }
    !html_comment && is_indented_code_line($0) { next }
    {
      line = uncomment($0)
      if (line ~ /^## Components([ \t]*$)/) { in_sec = 1; next }
      if (in_sec && line ~ /^## /) { in_sec = 0 }
      if (!in_sec) next
      rest = line
      while (match(rest, /`[^`]+`/)) {
        tok = substr(rest, RSTART + 1, RLENGTH - 2)
        gsub(/^[[:space:]]+|[[:space:]]+$/, "", tok)
        if (tok != "") print tok
        rest = substr(rest, RSTART + RLENGTH)
      }
    }
  ' "$1"
}

# Authored-prose kind-plural findings as "line<TAB>found<TAB>expected".
kind_plural_issues_of() {
  awk "$AWK_LIB"'
    BEGIN { md_prev_blank = 1 }
    FNR == 1 && $0 == "---" { frontmatter = 1; next }
    frontmatter { if ($0 == "---") frontmatter = 0; next }
    !html_comment && !md_in_indent {
      if (!fence) {
        fo = fence_open($0)
        if (fo != "") { fence = 1; fopen = fo; md_prev_blank = 0; next }
      } else {
        if (fence_close($0, fopen)) fence = 0
        md_prev_blank = 0
        next
      }
    }
    fence { next }
    !html_comment && is_indented_code_line($0) { next }
    {
      line = uncomment($0)
      n = length(line)
      i = 1
      while (i <= n) {
        ch = substr(line, i, 1)
        if (ch == "\\") { i += 2; continue }
        if (ch == "`") {
          j = i
          while (j <= n && substr(line, j, 1) == "`") j++
          tlen = j - i
          k = j
          closed = 0
          while (k <= n) {
            if (substr(line, k, 1) == "`") {
              ks = k
              while (k <= n && substr(line, k, 1) == "`") k++
              if (k - ks == tlen) { closed = ks; break }
              continue
            }
            k++
          }
          if (closed > 0) {
            inner = substr(line, j, closed - j)
            if (length(inner) >= 2 && substr(inner, 1, 1) == " " && substr(inner, length(inner), 1) == " " && inner !~ /^ +$/) {
              inner = substr(inner, 2, length(inner) - 2)
            }
            if (inner ~ /^(Pods|Nodes|Instances|ModelDeployments|ModelArtifacts|Workloads)$/) {
              sing = substr(inner, 1, length(inner) - 1)
              printf "%d\t`%s`\t`%s`s\n", FNR, inner, sing
            } else if (inner == "Topology" || inner == "Devices") {
              after = substr(line, k)
              sub(/^(\*\*|__)/, "", after)
              if (after ~ /^s([^[:alnum:]]|$)/) {
                fix = (inner == "Topology") ? "`Topology` resources" : "`Devices` objects"
                printf "%d\t`%s`s\t%s\n", FNR, inner, fix
              }
            }
            i = k
            continue
          }
          i = j
          continue
        }
        i++
      }
    }
  ' "$1"
}

# Table header findings as line number, issue and visible cell text.
table_header_issues_of() {
  awk "$AWK_LIB"'
    function inspect(header, at,   n, cells, i, cell, words, count, tokens, j, word, bad, escaped_pipe) {
      sub(/^[[:space:]]*\|/, "", header)
      sub(/\|[[:space:]]*$/, "", header)
      escaped_pipe = sprintf("%c", 28)
      gsub(/\\\|/, escaped_pipe, header)
      n = split(header, cells, /\|/)
      for (i = 1; i <= n; i++) {
        cell = strip_links(cells[i])
        gsub(escaped_pipe, "|", cell)
        gsub(/[*_`]/, "", cell)
        sub(/^[[:space:]]+/, "", cell); sub(/[[:space:]]+$/, "", cell)
        count = cell == "" ? 0 : split(cell, words, /[[:space:]]+/)
        if (count < 1 || count > 3)
          printf "%d\tlength\t%d words\t%s\n", at, count, cell
        bad = tolower(cell)
        gsub(/[^[:alnum:]]+/, " ", bad)
        split(bad, tokens, /[[:space:]]+/)
        for (j in tokens) {
          word = tokens[j]
          if (word ~ /^(a|an|the|this|that|these|those|what|where|when|how|which|why|who|whose|whom)$/) {
            printf "%d\tword\t%s\t%s\n", at, word, cell
            break
          }
        }
      }
    }
    {
      line = $0
      if (!html_comment) {
        if (!fence) {
          fo = fence_open(line)
          if (fo != "") { fence = 1; fopen = fo; previous = ""; next }
        } else {
          if (fence_close(line, fopen)) fence = 0
          previous = ""; next
        }
      }
      if (fence) { previous = ""; next }
      line = uncomment(line)
      if (previous ~ /\|/ && line ~ /^[[:space:]]*\|?[[:space:]]*:?-+(:?[[:space:]]*\|[[:space:]]*:?-+)+[[:space:]]*\|?[[:space:]]*$/)
        inspect(previous, FNR - 1)
      previous = line
    }
  ' "$1"
}

# Exercise style checks independently of the repository content.
run_selftest() {
  cat > "$WORK/style-self-test.md" <<'STYLE_TEST'
# WHAT is included
## Why now
## Host-state rows
## The resource keys
## Step 1: Create the pod
#### Too deep
Bad `Pods` and `Topology`s but good `Pod`s and `--nodes`.
| Valid | Which mode | The value |
|---|---|---|
```
# Where ignored
## Fenced rows
#### Fence H4
Bad `Nodes`
| Who ignored |
|---|
```

    #### Indented H4 `Instances`

<!--
## When ignored
## Comment table
#### Comment H4 `Workloads`
-->
STYLE_TEST
  heading_issues_of "$WORK/style-self-test.md" 1 3 1 > "$WORK/heading-test.out"
  table_header_issues_of "$WORK/style-self-test.md" > "$WORK/table-test.out"
  kind_plural_issues_of "$WORK/style-self-test.md" > "$WORK/plural-test.out"
  [ "$(wc -l < "$WORK/heading-test.out" | tr -d ' ')" -eq 5 ] &&
    grep -q 'word.*what' "$WORK/heading-test.out" &&
    grep -q 'word.*why' "$WORK/heading-test.out" &&
    grep -q 'container.*rows' "$WORK/heading-test.out" &&
    grep -q 'article.*the' "$WORK/heading-test.out" &&
    grep -q 'depth.*H4' "$WORK/heading-test.out" &&
    [ "$(wc -l < "$WORK/table-test.out" | tr -d ' ')" -eq 2 ] &&
    grep -q 'word.*which' "$WORK/table-test.out" &&
    grep -q 'word.*the' "$WORK/table-test.out" &&
    [ "$(wc -l < "$WORK/plural-test.out" | tr -d ' ')" -eq 2 ]
}

if [ "$SELFTEST" -eq 1 ]; then
  if run_selftest; then echo "OK: documentation style self-test passed."; exit 0; fi
  echo "FAIL: documentation style self-test failed." >&2
  exit 1
fi

# Extract rows of a "## <Section>" table in docs/README.md as "lineno<TAB>target<TAB>label".
index_section_rows() {
  awk -v sec="$1" '
    $0 == "## " sec   { intable = 1; found = 1; next }
    intable && /^## / { intable = 0 }
    intable && /^\| *\[/ {
      if (!match($0, /\[[^]]*\]\([^)]*\)/)) next
      m = substr($0, RSTART, RLENGTH)
      label = m; sub(/^\[/, "", label); sub(/\]\([^)]*\)$/, "", label)
      target = m; sub(/^\[[^]]*\]\(/, "", target); sub(/\)$/, "", target)
      sub(/#.*$/, "", target)
      sub(/^\.\//, "", target)
      printf "%d\t%s\t%s\n", FNR, target, label
    }
    END { if (!found) exit 1 }
  ' docs/README.md
}

# Expected docs/README.md section for a docs/<rel> file in strict three-zone mode.
expected_zone_section() {
  case "$1" in
    reference/*) printf 'Reference\n' ;;
    contribute/*) printf 'Contribute\n' ;;
    *) printf 'All pages\n' ;;
  esac
}

for f in $LINKED_PAGES; do
  [ -f "$f" ] || continue
  anchors_of "$f" > "$(store_for "$f")"
done

# Rule 1: links and strict three-zone isolation
echo "==> links"
for f in $LINKED_PAGES; do
  [ -f "$f" ] || continue
  dir=$(dirname "$f")
  while IFS="$(printf '\t')" read -r _lineno target; do
    case "$target" in
      http://*|https://*|HTTP://*|HTTPS://*|mailto:*|//*|/*|"") continue ;;
    esac
    path="${target%%#*}"
    path="${path%%\?*}"
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

if [ "$STRICT_ZONES" -eq 1 ]; then
  if [ -d "docs/reference" ]; then
    while IFS= read -r subdir; do
      [ -n "$subdir" ] || continue
      err "$subdir: subdirectory forbidden under flat docs/reference/"
    done < <(find docs/reference -mindepth 1 -type d | sort)
  fi

  PUBLIC_ZONE_PAGES=$( {
    for f in $PAGES; do
      case "$f" in
        docs/README.md|docs/reference/*|docs/contribute/*) continue ;;
      esac
      printf '%s\n' "$f"
    done
    if [ -n "$SITE_CONTENT_PAGES" ]; then
      printf '%s\n' "$SITE_CONTENT_PAGES"
    fi
  } | awk 'NF' )

  for f in $PUBLIC_ZONE_PAGES; do
    [ -f "$f" ] || continue
    links_of "$f" | awk -F'\t' -v src="$f" "$AWK_LIB"'
      function dir_of(p,   idx, k) {
        k = 0
        for (idx = length(p); idx >= 1; idx--) {
          if (substr(p, idx, 1) == "/") { k = idx; break }
        }
        return (k > 0) ? substr(p, 1, k - 1) : "."
      }
      function strip_default_port(hostport, sch,   colon, host, port) {
        hostport = tolower(hostport)
        colon = index(hostport, ":")
        if (!colon) return hostport
        host = substr(hostport, 1, colon - 1)
        port = substr(hostport, colon + 1)
        if ((sch == "https" && port == "443") || (sch == "http" && port == "80") || (sch == "" && (port == "80" || port == "443"))) {
          return host
        }
        return hostport
      }
      function is_excluded(u,   sch, rest, slash, host, upath, np, fdir, sc_sub, sc_dir, site_np) {
        sub(/#.*$/, "", u)
        sub(/\?.*$/, "", u)
        if (u == "" || tolower(u) ~ /^(mailto|tel|javascript|data):/) return 0
        if (u ~ /^([Hh][Tt][Tt][Pp][Ss]?:)?\/\//) {
          sch = ""
          rest = u
          if (tolower(rest) ~ /^https:\/\//) { sch = "https"; sub(/^[Hh][Tt][Tt][Pp][Ss]:\/\//, "", rest) }
          else if (tolower(rest) ~ /^http:\/\//) { sch = "http"; sub(/^[Hh][Tt][Tt][Pp]:\/\//, "", rest) }
          else { sub(/^\/\//, "", rest) }
          slash = index(rest, "/")
          if (slash == 0) return 0
          host = strip_default_port(substr(rest, 1, slash - 1), sch)
          upath = norm_relpath(url_decode(substr(rest, slash + 1)))
          if (host == "docs.gpustack.ai") {
            return (upath ~ /^(gpustack-operator\/([^\/]+\/)?)?(reference|docs\/reference|docs\/contribute)(\/|$)/)
          }
          if (host == "github.com") {
            return (upath ~ /^gpustack\/gpustack-operator\/(blob|tree)\/[^\/]+\/docs\/(reference|contribute)(\/|$)/)
          }
          return 0
        }
        if (substr(u, 1, 1) == "/") {
          np = norm_relpath(url_decode(substr(u, 2)))
          return (np ~ /^(gpustack-operator\/([^\/]+\/)?)?(reference|docs\/reference|docs\/contribute)(\/|$)/)
        }
        u = url_decode(u)
        fdir = dir_of(src)
        np = norm_relpath((fdir == "." ? "" : fdir "/") u)
        if (np ~ /^docs\/(reference|contribute)(\/|$)/) return 1
        if (src ~ /^site\/content\//) {
          sc_sub = substr(src, length("site/content/") + 1)
          sc_dir = dir_of(sc_sub)
          site_np = norm_relpath((sc_dir == "." ? "" : sc_dir "/") u)
          if (site_np ~ /^(reference|docs\/reference|docs\/contribute)(\/|$)/) return 1
        }
        return 0
      }
      {
        if (is_excluded($2)) printf "%s:%s: public page links to excluded zone: %s\n", src, $1, $2
      }
    ' > "$WORK/iso"
    while IFS= read -r issue; do
      [ -n "$issue" ] && err "$issue"
    done < "$WORK/iso"
  done
fi

# Rules 2 and 3: Contents, footer, and reference hub checks
echo "==> page structure"
for f in $PAGES; do
  [ "$f" = "docs/README.md" ] && continue

  tail -n 12 "$f" | grep -q '^\*\*See also\*\*' \
    || err "$f: no '**See also**' footer at the end of the page"

  if [ "$STRICT_ZONES" -eq 1 ]; then
    case "$f" in
      docs/reference/*)
        if has_next_footer "$f"; then
          err "$f: reference page must omit '**Next**' footer"
        fi
        ;;
    esac
  fi

  if [ "$STRICT_ZONES" -eq 1 ] && [ "$f" = "docs/reference/README.md" ]; then
    if ! grep -q '^## Contents' "$f"; then
      continue
    fi
  elif ! grep -q '^## Contents' "$f"; then
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

if [ "$STRICT_ZONES" -eq 1 ]; then
  links_of "docs/reference/README.md" | awk -F'\t' "$AWK_LIB"'
    {
      u = $2
      sub(/#.*$/, "", u)
      sub(/\?.*$/, "", u)
      if (u == "" || u ~ /^([Hh][Tt][Tt][Pp][Ss]?:|mailto:|\/)/) next
      print norm_relpath("docs/reference/" url_decode(u))
    }
  ' | sort -u > "$WORK/hub-links"
  for f in $PAGES; do
    case "$f" in
      docs/reference/README.md) continue ;;
      docs/reference/*.md)
        if ! grep -qxF "$f" "$WORK/hub-links"; then
          err "$f: not linked from reference hub docs/reference/README.md"
        fi
        ;;
    esac
  done
  while IFS= read -r hub_issue; do
    [ -n "$hub_issue" ] && err "$hub_issue"
  done < <(check_reference_hub_sections "docs/reference/README.md")
fi

# Rule 4: docs/README.md index coverage
echo "==> docs/README.md index coverage"
if [ "$STRICT_ZONES" -eq 0 ]; then
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
else
  : > "$WORK/strict-labels"
  for sec in "All pages" "Reference" "Contribute"; do
    sec_file="$WORK/sec-$(printf '%s' "$sec" | tr ' ' '_')"
    if ! index_section_rows "$sec" > "$sec_file"; then
      err "docs/README.md: no '## $sec' section"
      : > "$sec_file"
      continue
    fi
    awk -F'\t' -v sec="$sec" '
      {
        seen[$2]++
        if (seen[$2] > 1) printf "docs/README.md:%s: duplicate entry '\''%s'\'' in '\''## %s'\''\n", $1, $2, sec
      }
    ' "$sec_file" > "$WORK/sec-dups"
    while IFS= read -r dup_msg; do
      [ -n "$dup_msg" ] && err "$dup_msg"
    done < "$WORK/sec-dups"

    while IFS="$(printf '\t')" read -r lineno rel label; do
      [ -n "$rel" ] || continue
      printf '%s\t%s\t%s\n' "$sec" "$rel" "$label" >> "$WORK/strict-labels"
      if [ ! -f "docs/$rel" ]; then
        err "docs/README.md:$lineno: indexed page 'docs/$rel' in '## $sec' does not exist"
        continue
      fi
      want_sec=$(expected_zone_section "$rel")
      if [ "$want_sec" != "$sec" ]; then
        err "docs/README.md:$lineno: 'docs/$rel' belongs in '## $want_sec', not '## $sec'"
      fi
    done < "$sec_file"
  done

  for f in $PAGES; do
    [ "$f" = "docs/README.md" ] && continue
    rel="${f#docs/}"
    want_sec=$(expected_zone_section "$rel")
    sec_file="$WORK/sec-$(printf '%s' "$want_sec" | tr ' ' '_')"
    if ! awk -F'\t' -v r="$rel" '$2 == r { found = 1; exit } END { exit (found ? 0 : 1) }' "$sec_file"; then
      err "$f: not in the '## $want_sec' table of docs/README.md"
    fi
  done
fi

# Rules 5-7: paragraph cap, line cap, section cap
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
  if [ "$STRICT_ZONES" -eq 1 ] && [ "$f" = "docs/getting-started/architecture.md" ] && [ "$lines" -gt "$ARCH_LINE_CAP" ]; then
    shape_err "$f: $lines lines, over the ${ARCH_LINE_CAP}-line architecture overview cap"
  fi
  page_h2_cap="$H2_CAP"
  if [ "$STRICT_ZONES" -eq 1 ]; then
    case "$f" in
      docs/README.md|docs/reference/*|docs/contribute/*) ;;
      *) page_h2_cap="$PUBLIC_H2_CAP" ;;
    esac
  fi
  exempt "$f" "$H2_CAP_EXEMPT" || [ "$h2" -le "$page_h2_cap" ] \
    || shape_err "$f: $h2 '##' sections, over the cap of $page_h2_cap"

  printf '%s\t%s\t%s\t%s\t%s\n' \
    "$f" "$bytes" "$lines" "$h2" "$(wc -l < "$WORK/paras" | tr -d ' ')" >> "$WORK/metrics"
done

# Rule 8: index label matches page H1
echo "==> docs/README.md index labels"
if [ "$STRICT_ZONES" -eq 0 ]; then
  index_section_rows "All pages" > "$WORK/labels" || : > "$WORK/labels"
  for f in $PAGES; do
    [ "$f" = "docs/README.md" ] && continue
    label=$(awk -F'\t' -v r="${f#docs/}" '$2 == r { print $3; exit }' "$WORK/labels")
    [ -n "$label" ] || continue
    h1=$(h1_of "$f")
    [ "$label" = "$h1" ] \
      || shape_err "$f: index label '$label' is not the page's H1 '$h1'"
  done
else
  while IFS="$(printf '\t')" read -r sec rel label; do
    [ -f "docs/$rel" ] || continue
    h1=$(h1_of "docs/$rel")
    [ "$label" = "$h1" ] \
      || shape_err "docs/$rel: index label '$label' in '## $sec' is not the page's H1 '$h1'"
  done < "$WORK/strict-labels"
fi

# Rule 9: paths named as skill inputs
echo "==> skill document paths"
if [ -d ".agents/skills" ]; then
  while IFS= read -r path; do
    [ -n "$path" ] || continue
    [ -f "$path" ] || err "$path: named in a skill but no longer exists"
  done < <(grep -RhoE 'docs/[[:alnum:]_./-]+\.md' .agents/skills --include='*.md' | sort -u || true)
fi

# Rule 10: authored heading limits, depth ceiling, and numbered heading case
echo "==> heading limits and numbered heading case"
for f in $HEADING_PAGES; do
  [ -f "$f" ] || continue
  check_interrogatives=0
  max_depth=0
  strict_heading_style=0
  case "$f" in README.md|docs/*) check_interrogatives=1 ;; esac
  if [ "$STRICT_ZONES" -eq 1 ]; then
    case "$f" in docs/*) max_depth="$MAX_HEADING_DEPTH" ;; esac
    case "$f" in docs/README.md) ;; docs/*) strict_heading_style=1 ;; esac
  fi
  while IFS="$(printf '\t')" read -r at kind detail title; do
    case "$kind" in
      depth) err "$f:$at: heading level $detail exceeds max depth H${MAX_HEADING_DEPTH}: '$title'" ;;
      case) err "$f:$at: capitalize the first word after the numbered prefix: '$title'" ;;
      length) err "$f:$at: heading topic is $detail; cap is ${HEADING_WORD_CAP} words / ${HEADING_CHAR_CAP} characters: '$title'" ;;
      word) err "$f:$at: heading contains interrogative word '$detail': '$title'" ;;
      container) err "$f:$at: heading uses layout-container noun '$detail'; name the topic instead: '$title'" ;;
      article) err "$f:$at: non-Step heading starts with article '$detail'; omit leading articles: '$title'" ;;
    esac
  done < <(heading_issues_of "$f" "$check_interrogatives" "$max_depth" "$strict_heading_style")
done

# Rule 11: table header limits
echo "==> table header limits"
cat > "$WORK/table-self-test.md" <<'TABLE_TEST'
| Four word header here | The value | [This](https://example.com) | **Valid** |
|---|---|---|---|
| Escaped \| pipe | [Safe label](https://example.com/the) |
|---|---|
```
| The ignored header | Four ignored words here |
|---|---|
```
<!--
| The ignored header | Four ignored words here |
|---|---|
-->
TABLE_TEST
table_header_issues_of "$WORK/table-self-test.md" > "$WORK/table-self-test.out"
if [ "$(wc -l < "$WORK/table-self-test.out" | tr -d ' ')" -ne 3 ] || \
   ! grep -q 'length.*4 words' "$WORK/table-self-test.out" || \
   ! grep -q 'word.*the' "$WORK/table-self-test.out" || \
   ! grep -q 'word.*this' "$WORK/table-self-test.out"; then
  err "table header checker self-test failed"
fi
for f in README.md $PAGES; do
  [ -f "$f" ] || continue
  while IFS="$(printf '\t')" read -r at kind detail cell; do
    case "$kind" in
      length) err "$f:$at: table header has $detail; expected 1 to 3: '$cell'" ;;
      word) err "$f:$at: table header contains forbidden word '$detail': '$cell'" ;;
    esac
  done < <(table_header_issues_of "$f")
done

# Rule 12: authored-prose kind-plural check, public noise filter, and architecture component sync (strict mode)
if [ "$STRICT_ZONES" -eq 1 ]; then
  echo "==> authored-prose kind plurals"
  PLURAL_PAGES=$( {
    printf '%s\n' "$PAGES"
    if [ -n "$SITE_CONTENT_PAGES" ]; then
      printf '%s\n' "$SITE_CONTENT_PAGES"
    fi
  } | awk 'NF' )
  for f in $PLURAL_PAGES; do
    [ -f "$f" ] || continue
    while IFS="$(printf '\t')" read -r at bad fix; do
      err "$f:$at: malformed kind plural $bad in authored prose; write $fix"
    done < <(kind_plural_issues_of "$f")
  done

  echo "==> public prose noise and architecture components"
  for f in $PAGES; do
    case "$f" in
      docs/README.md|docs/reference/*|docs/contribute/*) continue ;;
    esac
    check_recorded_on=1
    case "$f" in
      *-benchmark.md|docs/modules/model-deployment/engine-versions.md) check_recorded_on=0 ;;
    esac
    while IFS="$(printf '\t')" read -r at kind _snippet; do
      case "$kind" in
        blank_recorded) err "$f:$at: omit blank 'not recorded' placeholder rows from public docs" ;;
        recorded_on) err "$f:$at: avoid dev-journal 'recorded on' stamps in public guides; state contract or version floor directly" ;;
      esac
    done < <(prose_noise_issues_of "$f" "$check_recorded_on")
  done

  if [ -f "docs/getting-started/architecture.md" ]; then
    arch_component_tokens_of "docs/getting-started/architecture.md" > "$WORK/arch-tokens"
    if [ -f "cmd/gpustack-operator/main.go" ]; then
      while IFS= read -r pkgname; do
        [ -n "$pkgname" ] || continue
        subcmd=$(awk '
          /Use:[[:space:]]*"/ {
            s = $0
            sub(/^.*Use:[[:space:]]*"/, "", s)
            sub(/".*$/, "", s)
            sub(/[[:space:]].*$/, "", s)
            print s
            exit
          }
        ' "pkg/${pkgname}"/*.go 2>/dev/null || true)
        [ -n "$subcmd" ] || continue
        if ! grep -qxF "$subcmd" "$WORK/arch-tokens"; then
          err "docs/getting-started/architecture.md: ## Components missing runtime subcommand '$subcmd' (from cmd/gpustack-operator/main.go)"
        fi
      done < <(awk '
        /AddCommand\([[:alnum:]_]+\.NewCommand\(\)\)/ {
          s = $0
          sub(/^.*AddCommand\(/, "", s)
          sub(/\.NewCommand\(\)\).*$/, "", s)
          print s
        }
      ' "cmd/gpustack-operator/main.go")
    fi
    for pyproj in */python/pyproject.toml; do
      [ -f "$pyproj" ] || continue
      py_dist=$(python3 - "$pyproj" <<'PY'
import sys, tomllib
try:
    with open(sys.argv[1], "rb") as fh:
        data = tomllib.load(fh)
    proj = data.get("project")
    name = proj.get("name") if isinstance(proj, dict) else None
    if isinstance(name, str) and name.strip():
        print(name.strip())
except Exception:
    sys.exit(1)
PY
      ) || py_dist=""
      if [ -z "$py_dist" ]; then
        err "docs/getting-started/architecture.md: unable to parse [project] name from ${pyproj}"
        continue
      fi
      py_mod=$(printf '%s' "$py_dist" | tr '-' '_')
      if ! grep -qxF "$py_dist" "$WORK/arch-tokens" && ! grep -qxF "$py_mod" "$WORK/arch-tokens"; then
        err "docs/getting-started/architecture.md: ## Components missing staged Python package '${py_mod}' (from ${pyproj})"
      fi
    done
  fi
fi

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
summary="OK: $(printf '%s\n' "$PAGES" | awk 'NF { n++ } END { print n + 0 }') docs pages checked; links also verified across README.md, AGENTS.md, Copilot instructions and .agents/skills."
[ "$warnings" -eq 0 ] || summary="$summary
WARN: $warnings shape warning(s); rules 5-8 are advisory under --report."
echo "$summary"
