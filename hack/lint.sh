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
  # The documentation contract: links and anchors, each page's Contents/header/footer, the
  # docs/README.md index and its labels, and the three size caps. Pure bash + awk, so it needs
  # neither the Go toolchain nor a cluster, and runs in about a second.
  bash "${ROOT_DIR}/.claude/skills/gpustack-operator-docs/scripts/check-docs.sh" "${ROOT_DIR}"
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
