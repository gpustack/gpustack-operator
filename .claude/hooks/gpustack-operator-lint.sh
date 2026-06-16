#!/usr/bin/env bash
#
# gpustack-operator-lint: Stop hook. Runs `make lint` (Go) and/or `make lint chart` (Helm chart)
# once at the end of a turn, but only for the file kinds that are dirty in the working tree.
# Report-only — it surfaces lint output but always exits 0 so it never blocks the turn
# (and never risks a Stop-hook loop).

set -o pipefail

cd "${CLAUDE_PROJECT_DIR:-.}" || exit 0

dirty="$(git status --porcelain --untracked-files=all 2>/dev/null)"

# run_lint <label> <command...>: run a lint command, reporting issues to stderr without ever blocking.
run_lint() {
  local label="$1"
  shift

  local output
  if ! output="$("$@" 2>&1)"; then
    {
      echo "gpustack-operator-lint: '${label}' reported issues (non-blocking):"
      echo "${output}"
    } >&2
  fi
}

# Lint Go sources when at least one .go file is changed/added.
if echo "${dirty}" | grep -qE '\.go$'; then
  run_lint "make lint" make lint
fi

# Lint the Helm chart when the chart or its helm tooling is changed/added.
if echo "${dirty}" | grep -qE 'deploy/gpustack-operator/chart/|hack/lib/helm\.sh'; then
  run_lint "make lint chart" make lint chart
fi

exit 0
