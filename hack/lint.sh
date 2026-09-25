#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
source "${ROOT_DIR}/hack/lib/init.sh"

function chart_lint() {
  # A static assertion over rendered output, needing no cluster, so it belongs to lint
  # rather than to test.
  gpustack::helm::verify_images "${ROOT_DIR}/deploy/gpustack-operator/chart"
  HELM_BIN="$(gpustack::helm::helm::bin)" \
    bash "${ROOT_DIR}/deploy/gpustack-operator/chart/ci/test-topograph.sh"
  bash "${ROOT_DIR}/deploy/gpustack-operator/chart/ci/test-kueue-releases.sh"
  gpustack::helm::lint "${ROOT_DIR}/deploy/gpustack-operator/chart"
}

function docs_lint() {
  local scripts="${ROOT_DIR}/.claude/skills/gpustack-operator-docs/scripts"
  local failed=()

  # The two self-tests run FIRST and are part of the gate. Each builds a throwaway tree, breaks one
  # thing, and asserts the checker names it — because a checker that has only ever been seen to
  # pass cannot be told apart from one that cannot fail, which is the defect half of these rules
  # exist to catch.
  #
  # Then the three contracts, all of them, before the verdict: links and anchors, each page's
  # Contents/header/footer, the docs/README.md index and the size caps (check-docs); the spec
  # Status word and every `go test -run` that names a test (check-specs); and cross-references
  # (check-crossrefs). Every one is bash and awk over the corpus — no cluster, no golangci-lint
  # pass — so running all five costs a few seconds.
  #
  # The five run concurrently: each is self-contained (its own throwaway tree, its own reads of
  # the corpus, nothing written in common), and together they are the whole wall of this target —
  # measured 14 s serial, 7 s concurrent, the longest single check being check-docs at 6 s.
  # Output is buffered per check and replayed in list order, so a failed run prints the same
  # blocks in the same order the serial form printed them.
  #
  # Stopping at the first failure would cost one CI round per finding, which is the same reason
  # docs.yml reports every broken external URL rather than the first.
  local docs_tmp
  docs_tmp="$(mktemp -d)"
  local check
  local pids=() names=()
  for check in check-specs-selftest check-crossrefs-selftest check-docs check-specs check-crossrefs; do
    bash "${scripts}/${check}.sh" "${ROOT_DIR}" >"${docs_tmp}/${check}.out" 2>&1 &
    pids+=("$!")
    names+=("${check}")
  done
  # The skills self-test joins them; check-skills itself runs after, because it is conditional on
  # the self-test's verdict.
  bash "${ROOT_DIR}/hack/check-skills-selftest.sh" "${ROOT_DIR}" >"${docs_tmp}/check-skills-selftest.out" 2>&1 &
  pids+=("$!")
  names+=("check-skills-selftest")

  local rcs=() i rc skills_selftest_rc=""
  for i in "${!names[@]}"; do
    rc=0
    wait "${pids[$i]}" || rc=$?
    rcs+=("${rc}")
  done
  for i in "${!names[@]}"; do
    if [[ -s "${docs_tmp}/${names[$i]}.out" ]]; then
      cat "${docs_tmp}/${names[$i]}.out"
    fi
    if [[ "${rcs[$i]}" -ne 0 ]]; then
      failed+=("${names[$i]}")
      if [[ "${names[$i]}" == "check-skills-selftest" ]]; then
        skills_selftest_rc="${rcs[$i]}"
      fi
    fi
  done
  rm -rf "${docs_tmp}"

  # The skill contract rides here rather than with the Go lint because a SKILL.md is markdown: the
  # Stop hook and docs.yml both already fire on any .md, so this gate reaches every turn that edits
  # a skill without a second trigger to keep in sync. Same self-test-first rule as above.
  if [[ -z "${skills_selftest_rc}" ]]; then
    # Two failures, two messages: a finding about the skills, or a check that could not run at all.
    local skills_rc=0
    bash "${ROOT_DIR}/hack/check-skills.sh" "${ROOT_DIR}" || skills_rc=$?
    if [[ ${skills_rc} -eq 1 ]]; then
      failed+=("check-skills")
    elif [[ ${skills_rc} -gt 1 ]]; then
      failed+=("check-skills (could not run; its diagnostic is above)")
    fi
  fi

  if [[ ${#failed[@]} -gt 0 ]]; then
    gpustack::log::fatal "docs lint failed: ${failed[*]}"
  fi
}

# The covering set for a turn that dirtied only shell under .agents/ outside hooks/. The Stop hook
# routes such a turn here instead of the full code gate: of everything lint() runs, only
# check-symbols.sh reads those files, so it runs beside the shell gate and the steps that read no
# such file are skipped. check-hook-dispatch.sh holds the step-to-inputs table this set must stay
# equal to, and fails when a step of lint() is added without a classification or when this
# function's set disagrees with the table.
function agents_shell_lint() {
  if ! bash "${ROOT_DIR}/hack/check-agents-shell-selftest.sh" "${ROOT_DIR}"; then
    gpustack::log::fatal "the .agents shell check is not trustworthy: its self-test failed"
  fi

  local agents_shell_rc=0
  bash "${ROOT_DIR}/hack/check-agents-shell.sh" "${ROOT_DIR}" || agents_shell_rc=$?
  if [[ ${agents_shell_rc} -eq 1 ]]; then
    gpustack::log::fatal "new syntax errors or shellcheck findings in changed .agents/ shell"
  elif [[ ${agents_shell_rc} -gt 1 ]]; then
    gpustack::log::fatal "the .agents shell check could not run; its diagnostic is above"
  fi

  # The one code-gate step that reads .agents/ shell files. Its own self-test is skipped here on
  # purpose: it exercises fixtures of its own and reads nothing this turn dirtied, and the full
  # code gate still runs it on every turn that dirties anything else.
  local symbols_rc=0
  bash "${ROOT_DIR}/hack/check-symbols.sh" "${ROOT_DIR}" || symbols_rc=$?
  if [[ ${symbols_rc} -eq 1 ]]; then
    gpustack::log::fatal "decorative symbols in commented sources"
  elif [[ ${symbols_rc} -gt 1 ]]; then
    gpustack::log::fatal "the decorative-symbol check could not run; its diagnostic is above"
  fi
}

function lint() {
  if [[ "${1:-}" == "chart" ]]; then
    chart_lint
    return
  fi

  if [[ "${1:-}" == "docs" ]]; then
    docs_lint
    return
  fi

  if [[ "${1:-}" == "agents-shell" ]]; then
    agents_shell_lint
    return
  fi

  local opts=()
  if [[ ${#BUILD_TAGS[@]} -gt 0 ]]; then
    opts+=("--build-tags=\"${BUILD_TAGS[*]}\"")
  fi
  opts+=("./...")
  GOLANGCI_LINT_CACHE="$(go env GOCACHE)/golangci-lint" gpustack::lint::run "${opts[@]}"

  # Comments, which golangci-lint does not read for this and which nothing reads for shell at all.
  # The self-test runs first and is part of the gate, for the same reason it is in docs_lint: a
  # check that has only ever been seen to pass cannot be told apart from one that cannot fail, and
  # this one's whole design is a set of ranges it must NOT report.
  if ! bash "${ROOT_DIR}/hack/check-symbols-selftest.sh" "${ROOT_DIR}"; then
    gpustack::log::fatal "the decorative-symbol check is not trustworthy: its self-test failed"
  fi
  # Two failures, two messages. A check that could not run is not a finding about the sources, and
  # reporting it as one sends the reader looking for a symbol that is not there.
  local symbols_rc=0
  bash "${ROOT_DIR}/hack/check-symbols.sh" "${ROOT_DIR}" || symbols_rc=$?
  if [[ ${symbols_rc} -eq 1 ]]; then
    gpustack::log::fatal "decorative symbols in commented sources"
  elif [[ ${symbols_rc} -gt 1 ]]; then
    gpustack::log::fatal "the decorative-symbol check could not run; its diagnostic is above"
  fi

  # The tool pins, which nothing else asserts. hack/lib/ decides whether a binary already in .sbin
  # may be reused, and accepting one by existence where a version is meant to be compared reuses
  # whatever a pin bump replaced -- a committed artifact then comes out of a generator nobody chose.
  # This hands every validator a binary that is not its pin and asserts the verdict, and it takes
  # the set of validators from the tree rather than from a list it carries, so a tool added without
  # a row fails here instead of going unmeasured. It carries its own fixtures rather than checking
  # the sources, so unlike the symbol check above there is nothing separate to self-test.
  if ! bash "${ROOT_DIR}/hack/check-toolpins-selftest.sh" "${ROOT_DIR}"; then
    gpustack::log::fatal "the tool-pin validators in hack/lib are not trustworthy: their self-test failed"
  fi

  # The Stop hook's routing, which decides whether anything above runs at all on a given turn. It is
  # asserted rather than trusted because a routing rule fails by staying silent: a path matching no
  # branch is verified by nothing, and from the outside that is indistinguishable from a turn with
  # nothing to check. It replaces `make` with a stub, so it costs a second and runs no lint pass. It
  # carries its own fixtures and pins absences as well as presences -- a dispatch stuck at "nothing"
  # and one stuck at "everything" each fail half of it -- so unlike the symbol check above there is
  # nothing separate to self-test.
  local dispatch_rc=0
  bash "${ROOT_DIR}/hack/check-hook-dispatch.sh" "${ROOT_DIR}" || dispatch_rc=$?
  if [[ ${dispatch_rc} -eq 1 ]]; then
    gpustack::log::fatal "the lint hook routes a dirty path to a target that does not cover it"
  elif [[ ${dispatch_rc} -gt 1 ]]; then
    gpustack::log::fatal "the lint hook's dispatch check could not run; its diagnostic is above"
  fi

  # The two review-exclusion lists, which are kept in separate files and which one of them promises
  # to mirror verbatim. Nothing held it to that: the docs gate's page set is README.md, AGENTS.md,
  # docs/** and .claude/skills/**, so `.github/` is covered by no gate at all. The drift
  # is silent -- two reviewers disagreeing about scope produces no error and no missing output, only
  # a review narrower than the sentence describing it, and each file stays internally consistent.
  # Its self-test runs first because both lists are parsed out of text: a parse that matches nothing
  # yields two empty sets that compare equal, so the check has to be shown to report that as a
  # broken instrument rather than as agreement.
  if ! bash "${ROOT_DIR}/hack/check-review-config-selftest.sh" "${ROOT_DIR}"; then
    gpustack::log::fatal "the review-config check is not trustworthy: its self-test failed"
  fi
  local review_config_rc=0
  bash "${ROOT_DIR}/hack/check-review-config.sh" "${ROOT_DIR}" || review_config_rc=$?
  if [[ ${review_config_rc} -eq 1 ]]; then
    gpustack::log::fatal "the two review-exclusion lists disagree"
  elif [[ ${review_config_rc} -gt 1 ]]; then
    gpustack::log::fatal "the review-config check could not run; its diagnostic is above"
  fi

  # The API field descriptions a cluster renders, which no other gate reads. golangci-lint sees the
  # doc comments as comments and the generated files as generated, and neither view asks who the
  # sentence is addressed to. The defect it guards is a sentence about Go reaching whoever runs
  # `kubectl explain`; three such descriptions were found and rewritten by hand, and the same two
  # classes still covered five more, which is the shape of a defect that gets fixed one instance at
  # a time forever. Its self-test runs first because the check is a vocabulary matched against
  # generated text, and both halves fail silently: a term matching nothing reports a clean tree,
  # and a term matched as a bare substring reports so much that the check gets switched off.
  if ! bash "${ROOT_DIR}/hack/check-api-descriptions-selftest.sh" "${ROOT_DIR}"; then
    gpustack::log::fatal "the API-description check is not trustworthy: its self-test failed"
  fi
  local api_descriptions_rc=0
  bash "${ROOT_DIR}/hack/check-api-descriptions.sh" "${ROOT_DIR}" || api_descriptions_rc=$?
  if [[ ${api_descriptions_rc} -eq 1 ]]; then
    gpustack::log::fatal "an API description a cluster renders describes Go rather than the field"
  elif [[ ${api_descriptions_rc} -gt 1 ]]; then
    gpustack::log::fatal "the API-description check could not run; its diagnostic is above"
  fi

  # Shell under .agents/, which the review bot selects nothing from and which nothing above reads
  # for shell syntax or semantics — check-symbols.sh reads it for decorative symbols only. The
  # self-test runs first, for the same reason as every other gate here: a comparison gate that has
  # only ever been seen to pass cannot be told apart from one whose comparison is broken. It
  # checks only the .agents shell files this tree changed, so a clean tree costs one git call.
  if ! bash "${ROOT_DIR}/hack/check-agents-shell-selftest.sh" "${ROOT_DIR}"; then
    gpustack::log::fatal "the .agents shell check is not trustworthy: its self-test failed"
  fi
  local agents_shell_rc=0
  bash "${ROOT_DIR}/hack/check-agents-shell.sh" "${ROOT_DIR}" || agents_shell_rc=$?
  if [[ ${agents_shell_rc} -eq 1 ]]; then
    gpustack::log::fatal "new syntax errors or shellcheck findings in changed .agents/ shell"
  elif [[ ${agents_shell_rc} -gt 1 ]]; then
    gpustack::log::fatal "the .agents shell check could not run; its diagnostic is above"
  fi

  # Three states, not two: the tree is clean, the tree is dirty, or git cannot answer
  # at all. Folding the last into "clean" is what made a build from a git worktree fail
  # — the checkout's .git is a file pointing outside the build context, so every git
  # call inside the image fails, the tree reads as clean, and the commit lint then runs
  # against a repository it cannot open.
  local dirty="false" git_readable="false"
  if [[ -n "$(command -v git)" ]] && git_status=$(git status --porcelain 2>/dev/null); then
    git_readable="true"
    if [[ -n ${git_status} ]]; then
      dirty="true"
    fi
  fi

  if [[ "${git_readable}" == "false" ]]; then
    gpustack::log::info "git cannot read this tree, skipping the commit lint"
  elif [[ "${dirty}" == "false" ]]; then
    gpustack::commit::lint
  fi

  if [[ "$*" =~ dirty ]] || [[ "${LINT_DIRTY:-}" == "true" ]]; then
    if [[ "${dirty}" != "false" ]]; then
      gpustack::log::fatal "the git tree is dirty:\n$(git status --porcelain)"
    fi
  fi
}

gpustack::log::info "+++ LINT +++"
lint "$@"
gpustack::log::info "--- LINT ---"
