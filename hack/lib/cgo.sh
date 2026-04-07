#!/usr/bin/env bash

# -----------------------------------------------------------------------------
# CGO variables helpers. These functions need the
# following variables:
#
#     C_FOR_GO_VERSION  -  The c-for-go version, default is cef5ec7833f3274488b3edd519f46ddfb57d5735.
# C_FOR_GO_C99_VERSION  -  The c-for-go-c99 version, default is master.

c_for_go_version=${C_FOR_GO_VERSION:-"cef5ec7833f3274488b3edd519f46ddfb57d5735"}
c_for_go_c99_version=${C_FOR_GO_C99_VERSION:-"master"}

function gpustack::cgo::c_for_go::install() {
  local bin="${ROOT_DIR}/.sbin"
  mkdir -p "${bin}"
  GOBIN="${bin}" go install "github.com/xlab/c-for-go@${c_for_go_version}"
}

function gpustack::cgo::c_for_go::validate() {
  # shellcheck disable=SC2046
  if [[ -n "$(command -v $(gpustack::cgo::c_for_go::bin))" ]]; then
    return 0
  fi

  gpustack::log::info "installing c-for-go"
  if gpustack::cgo::c_for_go::install; then
    gpustack::log::info "c-for-go installed"
    return 0
  fi
  gpustack::log::error "no c-for-go available"
  return 1
}

function gpustack::cgo::c_for_go::bin() {
  local bin="c-for-go"
  if [[ -f "${ROOT_DIR}/.sbin/c-for-go" ]]; then
    bin="${ROOT_DIR}/.sbin/c-for-go"
  fi
  echo -n "${bin}"
}

function gpustack::cgo::c_for_go_c99::install() {
  local bin="${ROOT_DIR}/.sbin"
  mkdir -p "${bin}"
  GOBIN="${bin}" go install "github.com/xlab/c-for-go@${c_for_go_c99_version}"
  mv "${bin}/c-for-go" "${bin}/c-for-go-c99"
}

function gpustack::cgo::c_for_go_c99::validate() {
  # shellcheck disable=SC2046
  if [[ -n "$(command -v $(gpustack::cgo::c_for_go_c99::bin))" ]]; then
    return 0
  fi

  gpustack::log::info "installing c-for-go-c99"
  if gpustack::cgo::c_for_go_c99::install; then
    gpustack::log::info "c-for-go-c99 installed"
    return 0
  fi
  gpustack::log::error "no c-for-go-c99 available"
  return 1
}

function gpustack::cgo::c_for_go_c99::bin() {
  local bin="c-for-go-c99"
  if [[ -f "${ROOT_DIR}/.sbin/c-for-go-c99" ]]; then
    bin="${ROOT_DIR}/.sbin/c-for-go-c99"
  fi
  echo -n "${bin}"
}