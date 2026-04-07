#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
source "${ROOT_DIR}/hack/lib/init.sh"

PACKAGE_NAMESPACE=${PACKAGE_NAMESPACE:-gpustack}
PACKAGE_ARCH=${PACKAGE_ARCH:-$(uname -m | sed 's/aarch64/arm64/' | sed 's/x86_64/amd64/')}
PACKAGE_TAG=${PACKAGE_TAG:-dev}
PACKAGE_WITH_CACHE=${PACKAGE_WITH_CACHE:-true}
PACKAGE_PUSH=${PACKAGE_PUSH:-false}

function pack() {
  if ! command -v docker &>/dev/null; then
    gpustack::log::fatal "Docker is not installed. Please install Docker to use this target."
    exit 1
  fi

  if ! docker buildx inspect --builder "gpustack" &>/dev/null; then
    gpustack::log::info "Creating new buildx builder 'gpustack'"
    docker run --rm --privileged tonistiigi/binfmt:qemu-v10.2.1-65 --uninstall all
    docker run --rm --privileged tonistiigi/binfmt:qemu-v10.2.1-65 --install all
    docker buildx create \
      --name "gpustack" \
      --driver "docker-container" \
      --driver-opt "network=host,default-load=true,env.BUILDKIT_STEP_LOG_MAX_SIZE=-1,env.BUILDKIT_STEP_LOG_MAX_SPEED=-1" \
      --buildkitd-flags "--allow-insecure-entitlement=security.insecure --allow-insecure-entitlement=network.host --oci-worker-net=host --oci-worker-gc-keepstorage=204800" \
      --bootstrap
  fi

  labels=(
    "org.opencontainers.image.source=https://github.com/gpustack/gpustack-operator"
    "org.opencontainers.image.version=dev"
    "org.opencontainers.image.revision=$(git rev-parse HEAD 2>/dev/null || echo "unknown")"
    "org.opencontainers.image.created=$(date +"%Y-%m-%dT%H:%M:%S.%s")"
  )

  local tasks
  if [[ "$#" -gt 0 ]]; then
    IFS=" " read -r -a tasks <<<"$*"
  else
    tasks=("gpustack-operator")
  fi

  for task in "${tasks[@]}"; do
    if [[ ! -f "${ROOT_DIR}/pack/${task}/Dockerfile" ]]; then
      continue
    fi

    extra_args=()
    if [[ "${PACKAGE_WITH_CACHE}" == "true" ]]; then
      extra_args+=(
        "--cache-from=type=registry,ref=gpustack/build-cache:${task}-dev"
        "--cache-from=type=registry,ref=gpustack/${task}:build-cache"
      )
    fi
    if [[ "${PACKAGE_PUSH}" == "true" ]]; then
      extra_args+=("--push")
    fi
    for label in "${labels[@]}"; do
      extra_args+=("--label" "${label}")
    done
    tag="${PACKAGE_NAMESPACE}/${task}:${PACKAGE_TAG}"
    gpustack::log::info "Building '${tag}' platform 'linux/${PACKAGE_ARCH}'"
    set -x
    docker buildx build \
      --pull \
      --allow network.host \
      --allow security.insecure \
      --builder "gpustack" \
      --platform "linux/${PACKAGE_ARCH}" \
      --tag "${tag}" \
      --file "${ROOT_DIR}/pack/${task}/Dockerfile" \
      --ulimit nofile=65536:65536 \
      --shm-size 16G \
      --progress plain \
      "${extra_args[@]}" \
      "${ROOT_DIR}"
    set +x
  done
}

gpustack::log::info "+++ PACKAGE +++"
pack "$@"
gpustack::log::info "--- PACKAGE ---"
