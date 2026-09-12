#!/usr/bin/env bash
#
# gpustack-operator-lint: Stop hook. Runs the checks CI enforces once at the end of a turn, but
# only the ones the dirty files call for: `make lint` (code, which is everything that is not
# markdown), `make lint docs` (the documentation contract), `make lint chart` (Helm chart),
# `make generate chart` (the chart's generated files), and a warning when a vendoring edit has
# not been re-vendored. Report-only — it surfaces output but always exits 0 so it never blocks
# the turn (and never risks a Stop-hook loop).
#
# hack/check-hook-dispatch.sh asserts the routing below, per kind of dirty path and in both
# directions. Change a pattern here and run `make lint`.

set -o pipefail

project_dir="${CLAUDE_PROJECT_DIR:-}"
if [[ -z "${project_dir}" ]]; then
  project_dir="$(git rev-parse --show-toplevel 2>/dev/null)" || exit 0
fi
cd "${project_dir}" || exit 0

dirty="$(git status --porcelain --untracked-files=all 2>/dev/null)"

# Nothing is dirty, so no target has anything to cover. This exit comes before the questions below
# rather than being folded into them, because the code question is asked the other way round and an
# empty list answers it yes: `grep -v` finds no line to reject and reports success, so "some dirty
# path is not markdown" is true of a clean tree. Without this, every turn that changed nothing would
# run the code gate.
if [[ -z "${dirty}" ]]; then
  exit 0
fi

# The dirty paths, one per line. The questions below are asked per file, so they are asked of paths
# rather than of the porcelain lines carrying them: a rename line carries two, and both count -- a
# shell script renamed to markdown still needs the code gate, because what may have broken is
# whatever referred to the name that left. Stripping the three-character status prefix here also
# keeps it out of every pattern.
#
# A path git chose to quote (non-ASCII, a control character) arrives with its quotes attached and so
# stops looking like markdown. That is the safe direction: it makes the code gate run.
dirty_paths="$(awk '{
  line = substr($0, 4)
  arrow = index(line, " -> ")
  if (arrow > 0) {
    print substr(line, 1, arrow - 1)
    print substr(line, arrow + 4)
  } else {
    print line
  }
}' <<<"${dirty}")"

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

# paths_match <extended-regex>: does any dirty path match? Fed by here-string rather than a pipe,
# because `grep -q` exits at the first match and `set -o pipefail` would then turn the writer's
# SIGPIPE into a false "no match" on a large status.
paths_match() { grep -qE "$1" <<<"${dirty_paths}"; }

# paths_beyond <extended-regex>: is any dirty path NOT matched by it? This is not `! paths_match`.
# On a list of more than one entry, "nothing here is inside the set" and "something here is outside
# the set" are different facts, and the dirty list is rarely one entry.
paths_beyond() { grep -qvE "$1" <<<"${dirty_paths}"; }

# Lint code when any dirty path is not markdown. Asked this way round on purpose: the boundary is
# not a list of file types maintained here, it is read out of the gate being run. check-symbols.sh,
# which `make lint` invokes and which is the only thing in this repository that reads shell at all,
# selects `git ls-files` minus a named exclusion list whose last entry is `:(exclude)*.md`, under a
# comment reading "this gate covers code". So "not markdown" IS that gate's own scope, asked as one
# question instead of copied here as an enumeration that then has to be kept in step.
#
# That is what the shape buys, and it is the whole point: the two lists below become ADDITIVE. An
# input nobody named means that target does not ALSO run for it -- never that nothing runs, because
# a path that is not markdown has already asked for this one. An enumeration here meant the
# opposite, and it showed: a turn that touched only shell used to fire none of the branches.
if paths_beyond '\.md$'; then
  run_lint "make lint" make lint
fi

# Check the documentation contract when any markdown, or one of the docs gate's own non-markdown
# inputs, is changed/added. Markdown is the one kind of path the branch above deliberately does not
# cover, so for markdown this branch is not an addition but the whole of it.
# `make lint docs` also carries the skill contract, and the two non-markdown inputs it compares --
# the Codex invocation policy beside each skill, and the exclusion list in .opencodereview -- are
# named here so that a turn editing only one of them gets the target that compares them, rather
# than only the code gate, which does not read them.
if paths_match '\.md$|/check-(docs|skills)\.sh$|\.agents/skills/|\.opencodereview/rule\.json'; then
  run_lint "make lint docs" make lint docs
fi

# Lint the Helm chart when the chart or its helm tooling is changed/added.
if paths_match 'deploy/gpustack-operator/chart/|hack/lib/helm\.sh'; then
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
if paths_match 'deploy/gpustack-operator/chart/(values\.yaml|README\.md\.gotmpl|Chart\.yaml)'; then
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
# clean vendored tree. The second test is anchored at the start of the path, because the patch
# directory's own path ends in the tree path being looked for: unanchored, `hack/deploy/.../charts/`
# would answer for `deploy/.../charts/` and the warning would never fire.
#
# `! paths_match` is the right operator here and paths_beyond is not: this asks whether NO vendored
# tree is dirty, which is the negation of the match.
if paths_match 'hack/deps\.sh|hack/deploy/gpustack-operator/chart/charts/' &&
  ! paths_match '^deploy/gpustack-operator/chart/charts/'; then
  echo "gpustack-operator-lint: a vendoring edit (hack/deps.sh or a chart patch) is dirty but no" \
    "vendored tree changed. make deps is a no-op on an up-to-date tree — rm -rf" \
    "deploy/gpustack-operator/chart/charts/<name> and re-run it, or the edit ships as nothing." >&2
fi

exit 0
