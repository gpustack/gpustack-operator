#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
source "${ROOT_DIR}/hack/lib/init.sh"
cd "${ROOT_DIR}"

PACKAGE_NAMESPACE=${PACKAGE_NAMESPACE:-gpustack}
PACKAGE_ARCH=${PACKAGE_ARCH:-$(uname -m | sed 's/aarch64/arm64/' | sed 's/x86_64/amd64/')}
PACKAGE_TAG=${PACKAGE_TAG:-dev}
PACKAGE_WITH_CACHE=${PACKAGE_WITH_CACHE:-true}
PACKAGE_PUSH=${PACKAGE_PUSH:-false}

function pack() {
  local tasks
  if [[ "$#" -gt 0 ]]; then
    IFS=" " read -r -a tasks <<<"$*"
  else
    tasks=("gpustack-operator")
  fi

  for task in "${tasks[@]}"; do
    if [[ ! -f "${ROOT_DIR}/pack/${task}/Dockerfile" ]]; then
      gpustack::log::fatal "No Dockerfile for package task: ${task}"
    fi
  done

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
    "org.opencontainers.image.version=${GIT_VERSION}"
    "org.opencontainers.image.revision=${GIT_COMMIT}"
    "org.opencontainers.image.created=$(date +"%Y-%m-%dT%H:%M:%S.%s")"
  )


  for task in "${tasks[@]}"; do
    extra_args=()
    if [[ "${PACKAGE_WITH_CACHE}" == "true" ]]; then
      extra_args+=(
        "--cache-from=type=registry,ref=gpustack/build-cache:${task}-main-linux-${PACKAGE_ARCH}"
      )
    fi
    if [[ "${PACKAGE_PUSH}" == "true" ]]; then
      extra_args+=("--push")
    else
      extra_args+=("--load")
    fi
    for label in "${labels[@]}"; do
      extra_args+=("--label" "${label}")
    done
    # The T-Head PPU SDK is only distributed as a presigned object-storage link, so the
    # thead-ppu-devel image takes it as a build argument instead of hardcoding it, passed
    # through from the environment when set. Only that task consumes it: the `set -x` below
    # traces the whole buildx invocation, so forwarding it to an unrelated image's build
    # would print the presigned value for nothing. Even for this task the value lands in a
    # local build's own terminal output — do not paste that output anywhere. CI does not run
    # this script; there the value arrives through the pack workflow's build-argument input,
    # and keeping it out of the build log is the Dockerfile's job (its SDK stage leaves
    # tracing off across the commands that read the URL).
    if [[ "${task}" == "thead-ppu-devel" ]] && [[ -n "${PPU_SDK_URL:-}" ]]; then
      extra_args+=("--build-arg" "PPU_SDK_URL=${PPU_SDK_URL}")
    fi
    tag="${PACKAGE_NAMESPACE}/${task}:${PACKAGE_TAG}"
    if [[ "${task}" == "model-transfer" ]]; then
      local model_transfer_flavor="${MODEL_TRANSFER_BUILD_FLAVOR:-cuda}"
      local model_transfer_suffix
      local model_transfer_arg
      local -a model_transfer_args=(UBUNTU_IMAGE)
      case "${model_transfer_flavor}" in
        cuda)
          model_transfer_suffix="cuda${CUDA_VERSION:-12.9}${MODEL_TRANSFER_VARIANT:+-${MODEL_TRANSFER_VARIANT}}"
          model_transfer_args+=(CUDA_RUNTIME_IMAGE NIXL_VERSION NIXL_TORCH_VERSION)
          ;;
        rocm)
          model_transfer_suffix="rocm${ROCM_VERSION:-7.2}${MODEL_TRANSFER_VARIANT:+-${MODEL_TRANSFER_VARIANT}}"
          model_transfer_args+=(ROCM_RUNTIME_IMAGE NIXL_ROCM_VERSION NIXL_ROCM_REF NIXL_ROCM_UCX_REF NIXL_ROCM_TORCH_VERSION)
          ;;
        cann)
          model_transfer_suffix="cann${CANN_VERSION:-9.2}-${MODEL_TRANSFER_VARIANT:-a5}"
          model_transfer_args+=(CANN_RUNTIME_IMAGE HIXL_COMMIT HIXL_CMAKE_COMMIT CMAKE_MAX_JOBS)
          ;;
        *) gpustack::log::fatal "Unknown model-transfer build flavor: ${model_transfer_flavor}"; return 1 ;;
      esac
      for model_transfer_arg in "${model_transfer_args[@]}"; do
        if [[ -n "${!model_transfer_arg:-}" ]]; then
          extra_args+=("--build-arg" "${model_transfer_arg}=${!model_transfer_arg}")
        fi
      done
      extra_args+=("--target" "${model_transfer_flavor}")
      tag="${tag}-${model_transfer_suffix}"
    fi
    gpustack::log::info "Building '${tag}' platform 'linux/${PACKAGE_ARCH}'"
    set -x
    docker buildx build \
      --pull \
      --allow network.host \
      --allow security.insecure \
      --builder "gpustack" \
      --platform "linux/${PACKAGE_ARCH}" \
      --tag "${tag}" \
      --build-arg "GPUSTACK_GIT_COMMIT=${GIT_COMMIT}" \
      --build-arg "GPUSTACK_GIT_VERSION=${GIT_VERSION}" \
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
