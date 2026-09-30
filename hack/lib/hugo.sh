#!/usr/bin/env bash

# Hugo renders the documentation site. Keep its version beside the installer, as with Helm.
hugo_version=${HUGO_VERSION:-"v0.152.2"}

function gpustack::hugo::bin() {
  local bin="hugo"
  if [[ -f "${ROOT_DIR}/.sbin/hugo" ]]; then
    bin="${ROOT_DIR}/.sbin/hugo"
  fi
  echo -n "${bin}"
}

function gpustack::hugo::install() {
  GOBIN="${ROOT_DIR}/.sbin" CGO_ENABLED=1 \
    go install -tags extended "github.com/gohugoio/hugo@${hugo_version}"
}

function gpustack::hugo::validate() {
  local bin
  bin=$(gpustack::hugo::bin)
  if [[ -n "$(command -v "${bin}")" ]] && \
    gpustack::util::go_module_version_is \
      "$(gpustack::util::go_module_version "${bin}")" "${hugo_version}"; then
    return 0
  fi

  gpustack::log::info "installing hugo ${hugo_version}"
  if gpustack::hugo::install; then
    return 0
  fi
  gpustack::log::error "no hugo available"
  return 1
}
