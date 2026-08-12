#!/usr/bin/env bash
#
# gpustack-operator-lint: Stop hook. Runs the checks CI enforces once at the end of a turn, but
# only the ones the dirty files call for: `make lint` (Go), `make lint chart` (Helm chart),
# `make generate chart` (the chart's generated files), and a warning when a vendoring edit has
# not been re-vendored. Report-only — it surfaces output but always exits 0 so it never blocks
# the turn (and never risks a Stop-hook loop).

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

# dirty_matches <extended-regex>: does any porcelain line match? Fed by here-string rather than a
# pipe, because `grep -q` exits at the first match and `set -o pipefail` would then turn the
# writer's SIGPIPE into a false "no match" on a large status.
dirty_matches() { grep -qE "$1" <<<"${dirty}"; }

# Lint Go sources when at least one .go file is changed/added.
if dirty_matches '\.go( -> |$)'; then
  run_lint "make lint" make lint
fi

# Check the documentation contract when any markdown, or the checker itself, is changed/added.
# A docs-only turn touches no .go file, so without this branch nothing here would run at all.
if dirty_matches '\.md( -> |$)|/check-docs\.sh( -> |$)'; then
  run_lint "make lint docs" make lint docs
fi

# Lint the Helm chart when the chart or its helm tooling is changed/added.
if dirty_matches 'deploy/gpustack-operator/chart/|hack/lib/helm\.sh'; then
  run_lint "make lint chart" make lint chart
fi

# Regenerate the chart's generated files when one of their three sources is changed, and say so
# when that moved anything. CI's "Verify Generated" compares exactly these files, and a stale
# values.schema.json is not a lint error — it makes `helm template` reject the values outright,
# which reads as a template bug until you remember the schema.
CHART_GENERATED=(
  deploy/gpustack-operator/chart/README.md
  deploy/gpustack-operator/chart/values.schema.json
  deploy/gpustack-operator/chart/values.yaml
)
if dirty_matches 'deploy/gpustack-operator/chart/(values\.yaml|README\.md\.gotmpl|Chart\.yaml)'; then
  before="$(cat "${CHART_GENERATED[@]}" 2>/dev/null | cksum)"
  run_lint "make generate chart" make generate chart
  if [[ "$(cat "${CHART_GENERATED[@]}" 2>/dev/null | cksum)" != "${before}" ]]; then
    echo "gpustack-operator-lint: the chart's generated files were stale and have been" \
      "regenerated — review and stage them (CI compares README.md, values.schema.json," \
      "values.yaml)" >&2
  fi
fi

# Warn when a vendoring edit cannot have taken effect. `make deps` skips any tree whose _VERSION_
# already matches the pinned version, so editing a patch — or anything else under a chart's patch
# directory — does nothing at all until that tree is deleted. The tell is a dirty patch beside a
# clean vendored tree. The path test is anchored past `git status --porcelain`'s three-character
# status prefix, because the patch directory's own path ends in the tree path being looked for.
if dirty_matches 'hack/deps\.sh|hack/deploy/gpustack-operator/chart/charts/' &&
  ! dirty_matches '^...deploy/gpustack-operator/chart/charts/'; then
  echo "gpustack-operator-lint: a vendoring edit (hack/deps.sh or a chart patch) is dirty but no" \
    "vendored tree changed. make deps is a no-op on an up-to-date tree — rm -rf" \
    "deploy/gpustack-operator/chart/charts/<name> and re-run it, or the edit ships as nothing." >&2
fi

exit 0
