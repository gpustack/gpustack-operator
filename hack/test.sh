#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
source "${ROOT_DIR}/hack/lib/init.sh"
cd "${ROOT_DIR}"

TEST_DIR="${ROOT_DIR}/.dist/test"
mkdir -p "${TEST_DIR}"

function test() (
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
  gpustack::build::setup_env "${target_os}" "$(go env GOARCH)"
  if [[ "${target_os}" == "linux" ]]; then
    extldflags=" -extldflags=-Wl,-z,lazy"
  fi

  local opts=(
    "-v"
    "-buildvcs=false"
    "-failfast"
    "-cover"
    # Order-dependent tests hide behind a fixed run order; shuffle so every run probes it.
    # A failure banner prints the seed, and -shuffle=<seed> reproduces that order.
    "-shuffle=on"
    "-timeout=30m"
    "-ldflags=${ldflags[*]}${extldflags}"
    "-coverprofile=${TEST_DIR}/coverage.out"
  )
  # RACE=false drops the race detector and leaves the rest of the run unchanged.
  if [[ "${RACE:-}" != "false" ]]; then
    opts+=("-race")
  fi
  local list_opts=()
  if [[ ${#BUILD_TAGS[@]} -gt 0 ]]; then
    opts+=("-tags=${BUILD_TAGS[*]}")
    list_opts+=("-tags=${BUILD_TAGS[*]}")
  fi

  local packages=("./...")
  if [[ $# -gt 0 ]]; then
    local listing package pattern
    listing="$(go list "${list_opts[@]}" ./...)"
    pattern="$(gpustack::util::join_array "|" "$@")"
    packages=()
    while IFS= read -r package; do
      if [[ -n "${package}" && ! "${package}" =~ ${pattern} ]]; then
        packages+=("${package}")
      fi
    done <<<"${listing}"
    if [[ ${#packages[@]} -eq 0 ]]; then
      gpustack::log::info "No test packages remain after exclusions"
      return
    fi
  fi
  set -x
  go test "${opts[@]}" "${packages[@]}"
  set +x
)

gpustack::log::info "+++ TEST +++"
test "$@"
gpustack::log::info "--- TEST ---"
