#!/usr/bin/env bash
# check-agents-shell.sh — syntax and shell semantics for the agent shell under .agents/, checked
# on the files a change actually touched.
#
# Why this gate exists: OpenCodeReview selects nothing under .agents/, and no other check reads
# those files for shell syntax or shell semantics — check-symbols.sh reads them for decorative
# symbols only. A PR that quietly broke e2e cases reached main for exactly this reason.
#
# What runs, per changed .sh file under .agents/ (hooks included):
#   1. bash -n — a syntax check under the bash running this script. It parses syntax only, so a
#      construct a different bash generation rejects (the mapfile-on-macOS class) is not caught.
#   2. shellcheck --severity=info — findings NEW relative to the base version of the file. Info
#      rather than warning because the classes a survey of this corpus actually surfaced —
#      unquoted expansions, unreachable commands — sit at info in shellcheck 0.10. A finding is
#      identified by its SC code plus its source line with whitespace collapsed, compared by
#      count, so a line inserted above a pre-existing warning does not re-report it and a
#      duplicated offending line is still counted as one more. Findings the base already carried
#      are not this change's to fix and do not block.
#
# The escape for a finding that is intentional: an inline "# shellcheck disable=SCxxxx" on the
# finding's line or the line above, with the reason stated beside it — the form the corpus
# already carries (run-partition-block.sh among others).
#
# Four states, not two: findings (exit 1), nothing to report (exit 0), could not run (exit 2 —
# git cannot answer, or the pinned shellcheck cannot be resolved, installed, or forced through
# SHELLCHECK_BIN), and skipped (exit 3). A gate that silently passes when its instrument is
# missing reports nothing about the sources.
#
# Skipped is the image build from a git worktree, and nothing else: the Dockerfile declares
# GPUSTACK_IMAGE_BUILD=true, the mode is local, and .git is a worktree pointer file whose gitdir
# the build context does not carry. Stepping aside there loses no verdict: local mode checks only
# uncommitted changes, and an image builds a committed tree, so even a build from a clean clone
# checks an empty set. The verdict on committed .agents shell belongs to agents-shell.yml, which
# runs --base, and to make lint on the host. A .git that is missing outright, a git that is not
# installed, or the declaration in --base mode is still could not run, and a tree git can read is
# checked in full whatever the declaration says. Exit 3 is its own code, printed with its reason,
# so no caller can take it for a pass.
#
# Usage:
#   bash hack/check-agents-shell.sh [repo-root]               # uncommitted files vs HEAD
#   bash hack/check-agents-shell.sh --base <ref> [repo-root]  # worktree files vs <ref>
set -euo pipefail

BASE=""
ROOT=""
args=("$@")
i=0
while [ "$i" -lt "${#args[@]}" ]; do
  arg="${args[$i]}"
  case "$arg" in
    --base)
      i=$((i + 1))
      if [ "$i" -ge "${#args[@]}" ]; then
        echo "--base needs a value" >&2
        exit 2
      fi
      BASE="${args[$i]}"
      ;;
    --base=*)
      BASE="${arg#--base=}"
      ;;
    -*)
      echo "unknown flag: $arg" >&2
      exit 2
      ;;
    *)
      ROOT="$arg"
      ;;
  esac
  i=$((i + 1))
done
[ -n "$ROOT" ] || ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

if ! command -v git >/dev/null 2>&1 || ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  # The one shape that is skipped rather than not run; the header states why and what it excludes.
  # A relative gitdir resolves against the tree root, which is the working directory here.
  if [ -z "${BASE}" ] && [ "${GPUSTACK_IMAGE_BUILD:-}" = "true" ] && command -v git >/dev/null 2>&1 && [ -f .git ]; then
    # An unreadable .git leaves gitdir empty, which is could not run, rather than tripping errexit.
    gitdir="$(sed -n 's/^gitdir: //p' .git 2>/dev/null | head -n 1 || true)"
    if [ -n "${gitdir}" ] && [ ! -e "${gitdir}" ]; then
      echo "SKIPPED: an image build from a git worktree: .git points at ${gitdir}, which the build context does not carry, so there is no base to diff against. agents-shell.yml and make lint on the host check this tree's .agents shell."
      exit 3
    fi
  fi
  echo "COULD-NOT-RUN: git cannot read this tree, so the changed files cannot be listed." >&2
  exit 2
fi

# The instrument is the pinned shellcheck, resolved like every other pinned tool in hack/lib: the
# repo's .sbin copy when its version matches the pin, else downloaded from the pinned release into
# .sbin on first use, so a machine with no shellcheck installed anywhere still runs the gate on
# the same analyzer version as CI — availability and parity are the same mechanism. An explicit
# SHELLCHECK_BIN is used verbatim; one that does not exist is a loud not-run, never a pass and
# never a silent fall-back to some other version.
if [ -n "${SHELLCHECK_BIN:-}" ]; then
  if [ ! -x "${SHELLCHECK_BIN}" ]; then
    echo "COULD-NOT-RUN: SHELLCHECK_BIN points at ${SHELLCHECK_BIN}, which is not an executable." >&2
    exit 2
  fi
else
  # The pin lives in hack/lib/style.sh with every other tool pin; log and util are what its
  # resolver calls. init.sh itself is deliberately not sourced: it resolves the Go toolchain
  # variables on the way in, and a shell-only gate has no business requiring a Go install.
  # shellcheck source=lib/util.sh
  # shellcheck source=lib/log.sh
  # shellcheck source=lib/style.sh
  LIB="$(cd "$(dirname "${BASH_SOURCE[0]}")/lib" && pwd)"
  ROOT_DIR="$(cd "${LIB}/../.." && pwd)"
  # shellcheck disable=SC1091
  source "${LIB}/util.sh"
  # shellcheck disable=SC1091
  source "${LIB}/log.sh"
  # shellcheck disable=SC1091
  source "${LIB}/style.sh"
  if ! gpustack::lint::shellcheck::validate; then
    echo "COULD-NOT-RUN: the pinned shellcheck cannot be resolved or installed; its diagnostic is above." >&2
    exit 2
  fi
  SHELLCHECK_BIN="$(gpustack::lint::shellcheck::bin)"
fi

# The changed .sh files under .agents/, added or modified — deleted files have nothing to check.
# In --base mode the list comes from the diff against that ref. In local mode it comes from the
# worktree status, read as NUL-terminated records so a path carrying a space arrives unquoted
# (git quotes it in the non-NUL form and the quotes would defeat the .sh test); a rename
# contributes its arriving path, and the bare original that follows it is stepped over.
CHANGED=()
if [ -n "${BASE}" ]; then
  while IFS= read -r f; do
    [ -n "$f" ] && CHANGED+=("$f")
  done < <(git diff --name-only --diff-filter=ACMRT "${BASE}" -- ':(glob).agents/**/*.sh' 2>/dev/null)
else
  take_original=0
  while IFS= read -r -d '' record; do
    if [ "${take_original}" -eq 1 ]; then
      take_original=0
      continue
    fi
    case "${record:0:1}" in [RC]) take_original=1 ;; esac
    f="${record:3}"
    case "$f" in
      .agents/*.sh) ;;
      *) continue ;;
    esac
    [ -f "$f" ] && CHANGED+=("$f")
  done < <(git status --porcelain -z --untracked-files=all 2>/dev/null)
fi

if [ "${#CHANGED[@]}" -eq 0 ]; then
  echo "OK: no changed .sh file under .agents/ to check."
  exit 0
fi

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

# findings <base|worktree> <file>: one "SCcode|collapsed-source-line" key per finding, on stdout.
#
# The gcc format is path:line:col:level:message [SCnnnn]. The path is the throwaway copy this
# function linted, so only the line number and the code are read from it; the source line the
# finding sits on is read from the copy itself, because line numbers shift with any insertion
# above and the source text is what survives that shift.
findings() {
  local which="$1" f="$2" copy line code srcline fl
  copy="${WORK}/copy"
  if [ "$which" = base ]; then
    git show "${BASE:-HEAD}:$f" >"${copy}" 2>/dev/null || true
  else
    cat "$f" >"${copy}" 2>/dev/null || true
  fi
  [ -s "${copy}" ] || return 0
  while IFS= read -r fl; do
    [ -n "$fl" ] || continue
    line="$(cut -d: -f2 <<<"$fl")"
    code="${fl##*[}"
    code="${code%]}"
    case "$code" in
      SC*) ;;
      *) continue ;;
    esac
    case "$line" in
      '' | *[!0-9]*) continue ;;
    esac
    srcline="$(sed -n "${line}p" "${copy}" | tr -s '[:space:]' ' ')"
    printf '%s|%s\n' "$code" "$srcline"
  done < <("${SHELLCHECK_BIN}" --severity=info -f gcc "${copy}" 2>/dev/null || true)
}

# grew <base-keys-file> <new-keys-file>: the keys whose count rose, as "code|line  +count".
# awk carries the whole comparison because it is the one tool here whose arrays are portable.
# The two files are told apart by FILENAME, not by the NR==FNR idiom: an empty base file is the
# common case here (a file with no findings on the base side), and on an empty first file that
# idiom counts the first line of the SECOND file as base, which turns every new finding green.
grew() {
  awk -v basefile="$1" '
    FILENAME == basefile { base[$0]++; next }
    { now[$0]++ }
    END {
      for (k in now) {
        if (now[k] > base[k] + 0) {
          print k "  +" (now[k] - base[k])
        }
      }
    }
  ' "$1" "$2"
}

failures=0
for f in "${CHANGED[@]}"; do
  # Syntax first: a file that cannot parse has no findings worth comparing.
  if ! "$BASH" -n "$f" 2>"${WORK}/syntax"; then
    echo "FAIL: $f: bash -n rejects it:" >&2
    sed 's/^/      /' "${WORK}/syntax" >&2
    failures=$((failures + 1))
    continue
  fi

  : >"${WORK}/base.keys"
  : >"${WORK}/new.keys"
  findings base "$f" >"${WORK}/base.keys" || true
  findings worktree "$f" >"${WORK}/new.keys" || true

  bad="$(grew "${WORK}/base.keys" "${WORK}/new.keys")"
  if [ -n "${bad}" ]; then
    echo "FAIL: $f: shellcheck findings this change adds:" >&2
    while IFS= read -r line; do
      [ -n "$line" ] && printf '      %s\n' "$line" >&2
    done <<<"${bad}"
    failures=$((failures + 1))
  else
    echo "OK: $f"
  fi
done

if [ "${failures}" -gt 0 ]; then
  echo >&2
  echo "FAIL: ${failures} changed .agents/ shell file(s) add a syntax error or a shellcheck finding." >&2
  echo "      Findings the base already carried are not reported; only what this change adds." >&2
  exit 1
fi
echo "OK: ${#CHANGED[@]} changed .agents/ shell file(s) add no syntax error and no shellcheck finding."
exit 0
