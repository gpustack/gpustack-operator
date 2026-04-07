#!/usr/bin/env bash

readonly DEFAULT_BUILD_TAGS=(
  "goccy"
  "netgo"
)

function gpustack::target::get_target_vars() {
  # shellcheck disable=SC2034
  if [[ -n "${BUILD_TAGS:-}" ]]; then
    IFS=" " read -r -a BUILD_TAGS <<<"${BUILD_TAGS}"
  else
    BUILD_TAGS=("${DEFAULT_BUILD_TAGS[@]}")
  fi

  # shellcheck disable=SC2034
  if [[ -z "${BUILD_OS:-}" ]]; then
     BUILD_OS="$(go env GOHOSTOS)"
  fi
  if [[ -z "${BUILD_ARCH:-}" ]]; then
     BUILD_ARCH="$(go env GOHOSTARCH)"
  fi
  if [[ -n "${BUILD_PLATFORMS:-}" ]]; then
    IFS=" " read -r -a BUILD_PLATFORMS <<<"${BUILD_PLATFORMS}"
  else
    BUILD_PLATFORMS=("${BUILD_OS}/${BUILD_ARCH}")
  fi
}
