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
  # Stopping at the first failure would cost one CI round per finding, which is the same reason
  # docs.yml reports every broken external URL rather than the first.
  for check in check-specs-selftest check-crossrefs-selftest check-docs check-specs check-crossrefs; do
    if ! bash "${scripts}/${check}.sh" "${ROOT_DIR}"; then
      failed+=("${check}")
    fi
  done

  if [[ ${#failed[@]} -gt 0 ]]; then
    gpustack::log::fatal "docs lint failed: ${failed[*]}"
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
