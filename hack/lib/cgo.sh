#!/usr/bin/env bash

# -----------------------------------------------------------------------------
# CGO variables helpers. These functions need the
# following variables:
#
#     C_FOR_GO_VERSION  -  The c-for-go version, default is cef5ec7833f3274488b3edd519f46ddfb57d5735.
# C_FOR_GO_C99_VERSION  -  The c-for-go-c99 version, default is 6dd1c017f91eea5364499f7926d94583eaddaadb.
#
# Both are commit hashes rather than tags: the two binaries are the same project at two points in
# its history, and the c99 one is ahead of every published tag.

c_for_go_version=${C_FOR_GO_VERSION:-"cef5ec7833f3274488b3edd519f46ddfb57d5735"}
# Was "master". A floating specifier makes the baseline move, so the same commit can produce a
# different binding/ tree tomorrow and a drift check reports it as drift the author did not cause.
# This hash is what master resolved to when it was pinned (2026-03-05, the branch tip), so the pin
# records current behaviour rather than changing it: a regeneration at the hash and one at master
# produce the same binding/ diff, down to the three address-valued constants that differ between
# any two runs (amdsmi LIB_VERSION_STRING, cndev EXPORT, rsmi MAX_NUM_POWER_PROFILES -- c-for-go
# bakes in a pointer for a macro that is not a compile-time constant, so "byte identical" is not a
# property this generator has in the first place).
c_for_go_c99_version=${C_FOR_GO_C99_VERSION:-"6dd1c017f91eea5364499f7926d94583eaddaadb"}

function gpustack::cgo::c_for_go::install() {
  local bin="${ROOT_DIR}/.sbin"
  mkdir -p "${bin}"
  GOBIN="${bin}" go install "github.com/xlab/c-for-go@${c_for_go_version}"
}

function gpustack::cgo::c_for_go::validate() {
  # c-for-go has no version flag, so the pin is compared against the module version `go install`
  # stamped into the binary. Existence alone accepts a .sbin populated before a pin bump, and
  # hack/generate.sh would then produce binding/ with the generator the bump replaced.
  # shellcheck disable=SC2046
  if [[ -n "$(command -v $(gpustack::cgo::c_for_go::bin))" ]]; then
    # shellcheck disable=SC2046
    if gpustack::util::go_module_version_is \
      "$(gpustack::util::go_module_version $(gpustack::cgo::c_for_go::bin))" \
      "${c_for_go_version}"; then
      return 0
    fi
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
  # Same as c_for_go above. This one matters more: c-for-go-c99 was floating on master until it
  # was pinned, so a .sbin from before the pin holds whatever master was that day and existence
  # alone would keep running it.
  # shellcheck disable=SC2046
  if [[ -n "$(command -v $(gpustack::cgo::c_for_go_c99::bin))" ]]; then
    # shellcheck disable=SC2046
    if gpustack::util::go_module_version_is \
      "$(gpustack::util::go_module_version $(gpustack::cgo::c_for_go_c99::bin))" \
      "${c_for_go_c99_version}"; then
      return 0
    fi
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