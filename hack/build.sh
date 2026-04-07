#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
source "${ROOT_DIR}/hack/lib/init.sh"

BUILD_DIR="${ROOT_DIR}/.dist/build"
mkdir -p "${BUILD_DIR}"

function build() {
  local ldflags=(
    "-X gpustack.ai/gpustack/pkg/utils/version.Version=${GIT_VERSION}"  # Inject the git version.
    "-X gpustack.ai/gpustack/pkg/utils/version.GitCommit=${GIT_COMMIT}" # Inject the git commit.
    "-w -s"
  )

  local tasks
  if [[ "$#" -gt 0 ]]; then
    IFS=" " read -r -a tasks <<<"$*"
  else
    tasks=("gpustack")
  fi

  for task in "${tasks[@]}"; do
    for platform in "${BUILD_PLATFORMS[@]}"; do
      local target_os_arch
      IFS="/" read -r -a target_os_arch <<<"${platform}"
      local target_os="${target_os_arch[0]}"
      local target_arch="${target_os_arch[1]}"

      local output
      output="${BUILD_DIR}/${task}-${target_os}-${target_arch}"
      if [[ "${#BUILD_PLATFORMS[*]}" -eq 1 ]]; then
        output="${BUILD_DIR}/${task}"
      fi
      if [[ "${target_os}" == "windows" ]]; then
        output="${output}.exe"
      fi

      local extldflags=""
      if [[ "${target_os}" == "linux" ]]; then
        extldflags=" -extldflags=-Wl,-z,lazy"
      fi

      local opts=(
        "-trimpath"
        "-ldflags=${ldflags[*]}${extldflags}"
        "-o=${output}"
      )
      if [[ ${#BUILD_TAGS[@]} -gt 0 ]]; then
        opts+=("-tags=\"${BUILD_TAGS[*]}\"")
      fi
      set -x
      GOOS=${target_os} GOARCH=${target_arch} CGO_ENABLED=1 \
        go build "${opts[@]}" "${ROOT_DIR}/cmd/${task}"
      set +x
    done
  done
}

gpustack::log::info "+++ BUILD +++" "info: ${GIT_VERSION},${GIT_COMMIT:0:7},${GIT_TREE_STATE},${BUILD_DATE}"
build "$@"
gpustack::log::info "--- BUILD ---"
