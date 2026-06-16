#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
source "${ROOT_DIR}/hack/lib/init.sh"

TEST_DIR="${ROOT_DIR}/.dist/test"
mkdir -p "${TEST_DIR}"

function test() {
  if [[ "${1:-}" == "chart" ]]; then
    gpustack::helm::test "${ROOT_DIR}/deploy/gpustack-operator/chart"
    return
  fi

  local ldflags=(
    "-X gpustack.ai/gpustack/pkg/utils/version.Version=${GIT_VERSION}"  # Inject the git version.
    "-X gpustack.ai/gpustack/pkg/utils/version.GitCommit=${GIT_COMMIT}" # Inject the git commit.
  )

  local target_os
  target_os="$(go env GOOS)"

  local extldflags=""
  if [[ "${target_os}" == "linux" ]]; then
    extldflags=" -extldflags=-Wl,-z,lazy"
  fi

  local opts=(
    "-v"
    "-failfast"
    "-race"
    "-cover"
    "-timeout=30m"
    "-ldflags=${ldflags[*]}${extldflags}"
    "-coverprofile=${TEST_DIR}/coverage.out"
  )
  if [[ ${#BUILD_TAGS[@]} -gt 0 ]]; then
    opts+=("-tags=\"${BUILD_TAGS[*]}\"")
  fi

  set -x
  if [[ $# -gt 0 ]]; then
    GODEBUG=gotypesalias=0 CGO_ENABLED=1 \
      go list ./... | grep -v -E "$(gpustack::util::join_array "|" "$@")" | xargs -I {} go test "${opts[@]}" {}"/..."
  else
    GODEBUG=gotypesalias=0 CGO_ENABLED=1 \
      go test "${opts[@]}" ./...
  fi
  set +x
}

gpustack::log::info "+++ TEST +++"
test "$@"
gpustack::log::info "--- TEST ---"
