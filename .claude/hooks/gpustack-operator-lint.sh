#!/usr/bin/env bash
#
# gpustack-operator-lint: Stop hook. Runs `make lint` once at the end of a turn, but only when
# Go files are dirty in the working tree. Report-only — it surfaces lint output but
# always exits 0 so it never blocks the turn (and never risks a Stop-hook loop).

set -o pipefail

cd "${CLAUDE_PROJECT_DIR:-.}" || exit 0

# Skip unless at least one .go file is changed/added in the working tree.
if ! git status --porcelain --untracked-files=all 2>/dev/null | grep -qE '\.go$'; then
  exit 0
fi

output="$(make lint 2>&1)"
status=$?

if [ "${status}" -ne 0 ]; then
  {
    echo "gpustack-operator-lint: 'make lint' reported issues (non-blocking):"
    echo "${output}"
  } >&2
fi

exit 0
