#!/usr/bin/env bash

# -----------------------------------------------------------------------------
# Lint variables helpers. These functions need the
# following variables:
#
# GOIMPORT_REVISER_VERSION  -  The Goimports-reviser version, default is v3.12.6.
#    GOLANGCI_LINT_VERSION  -  The Golangci-lint version, default is v2.11.4.
#        COMMITSAR_VERSION  -  The Commitsar version, default is v1.0.2.
#         GOIMPORT_VERSION  -  The Goimports version, default is master.

goimports_reviser_version=${GOIMPORT_REVISER_VERSION:-"v3.12.6"}
golangci_lint_version=${GOLANGCI_LINT_VERSION:-"v2.11.4"}
commitsar_version=${COMMITSAR_VERSION:-"v1.0.3"}
goimports_version=${GOIMPORT_VERSION:-"latest"}

function gpustack::lint::golangci_lint::install() {
  curl --retry 3 --retry-all-errors --retry-delay 3 \
    -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b "${ROOT_DIR}/.sbin" "${golangci_lint_version}"
}

function gpustack::lint::golangci_lint::validate() {
  # shellcheck disable=SC2046
  if [[ -n "$(command -v $(gpustack::lint::golangci_lint::bin))" ]]; then
    if [[ $($(gpustack::lint::golangci_lint::bin) --version 2>&1 | cut -d " " -f 4 2>&1 | head -n 1) == "${golangci_lint_version#v}" ]]; then
      return 0
    fi
  fi

  gpustack::log::info "installing golangci-lint ${golangci_lint_version}"
  if gpustack::lint::golangci_lint::install; then
    gpustack::log::info "golangci-lint $($(gpustack::lint::golangci_lint::bin) --version 2>&1 | cut -d " " -f 4 2>&1 | head -n 1)"
    return 0
  fi
  gpustack::log::error "no golangci-lint available"
  return 1
}

function gpustack::lint::golangci_lint::bin() {
  local bin="golangci-lint"
  if [[ -f "${ROOT_DIR}/.sbin/golangci-lint" ]]; then
    bin="${ROOT_DIR}/.sbin/golangci-lint"
  fi
  echo -n "${bin}"
}

function gpustack::lint::goimports_reviser::install() {
  local os
  os="$(gpustack::util::get_raw_os)"
  local arch
  arch="$(gpustack::util::get_raw_arch)"
  curl --retry 3 --retry-all-errors --retry-delay 3 \
    -o /tmp/commitsar.tar.gz \
    -sSfL "https://github.com/incu6us/goimports-reviser/releases/download/${goimports_reviser_version}/goimports-reviser_${goimports_reviser_version#v}_${os}_${arch}.tar.gz"
  tar -zxvf /tmp/commitsar.tar.gz \
    --directory "${ROOT_DIR}/.sbin" \
    --no-same-owner \
    --exclude ./LICENSE \
    --exclude ./README.md
  chmod a+x "${ROOT_DIR}/.sbin/goimports-reviser"
}

function gpustack::lint::goimports_reviser::validate() {
  # shellcheck disable=SC2046
  if [[ -n "$(command -v $(gpustack::lint::goimports_reviser::bin))" ]]; then
    if [[ $($(gpustack::lint::goimports_reviser::bin) -version | grep tag | cut -d " " -f 2 2>&1 | head -n 1) == "${goimports_reviser_version}" ]]; then
      return 0
    fi
  fi

  gpustack::log::info "installing goimports-reviser"
  if gpustack::lint::goimports_reviser::install; then
    gpustack::log::info "goimports-reviser $($(gpustack::lint::goimports_reviser::bin) -version | grep tag | cut -d " " -f 2 2>&1 | head -n 1)"
    return 0
  fi
  gpustack::log::error "no goimports-reviser available"
  return 1
}

function gpustack::lint::goimports_reviser::bin() {
  local bin="goimports-reviser"
  if [[ -f "${ROOT_DIR}/.sbin/goimports-reviser" ]]; then
    bin="${ROOT_DIR}/.sbin/goimports-reviser"
  fi
  echo -n "${bin}"
}

function gpustack::lint::run() {
  if ! gpustack::lint::goimports_reviser::validate; then
    gpustack::log::fatal "cannot execute goimports-reviser as client is not found"
  fi

  local goimport_target="${*:$#}"
  goimport_target="${goimport_target//\/.../}"
  local goimports_opts=(
    "-rm-unused"
    "-use-cache"
    "-set-alias"
    "-imports-order=std,general,company,project,blanked,dotted"
    "-output=file"
  )
  local goimports_excludes=()
  for arg in "$@"; do
    if [[ "${arg}" == "--exclude-dirs="* ]]; then
      arg="${arg//--exclude-dirs=/}"
      goimports_excludes+=("${arg}")
    fi
  done
  if [[ -n "${goimports_excludes[*]}" ]]; then
    goimports_opts+=("-excludes=$(gpustack::util::join_array "," "${goimports_excludes[@]}")")
  fi
  if [[ "${goimport_target}" == "${ROOT_DIR}" ]]; then
    gpustack::log::debug "go list -f \"{{.Dir}}\" ./... | xargs -I {} find {} -maxdepth 1 -type f -name '*.go' | grep -vE '_linux(_test)?\.go$' | xargs -I {} goimports-reviser ${goimports_opts[*]} {}"
    go list -f "{{.Dir}}" ./... | xargs -I {} find {} -maxdepth 1 -type f -name '*.go' | grep -vE '_linux(_test)?\.go$' | xargs -I {} "$(gpustack::lint::goimports_reviser::bin)" "${goimports_opts[@]}" {}>/dev/null 2>&1
  else
    gpustack::log::debug "pushd \"${goimport_target}\" >/dev/null 2>&1; go list -f \"{{.Dir}}\" ./... | xargs -I {} find {} -maxdepth 1 -type f -name '*.go' | grep -vE '_linux(_test)?\.go$' | xargs -I {} goimports-reviser ${goimports_opts[*]} {}; popd"
    # shellcheck disable=SC2164
    pushd "${goimport_target}" >/dev/null 2>&1
    go list -f "{{.Dir}}" ./... | xargs -I {} find {} -maxdepth 1 -type f -name '*.go' | grep -vE '_linux(_test)?\.go$' | xargs -I {} "$(gpustack::lint::goimports_reviser::bin)" "${goimports_opts[@]}" {}>/dev/null 2>&1
    # shellcheck disable=SC2164
    popd >/dev/null 2>&1
  fi

  if ! gpustack::lint::golangci_lint::validate; then
    gpustack::log::fatal "cannot execute golangci-lint as client is not found"
  fi

  local golangci_lint_opts=(
    "--fix"
  )
  gpustack::log::debug "golangci-lint run ${golangci_lint_opts[*]} $*"
  $(gpustack::lint::golangci_lint::bin) run "${golangci_lint_opts[@]}" "$@"
}

function gpustack::commit::commitsar::install() {
  local os
  os="$(gpustack::util::get_raw_os)"
  local arch
  arch="$(gpustack::util::get_raw_arch)"
  curl --retry 3 --retry-all-errors --retry-delay 3 \
    -o /tmp/commitsar.tar.gz \
    -sSfL "https://github.com/aevea/commitsar/releases/download/${commitsar_version}/commitsar_${commitsar_version#v}_${os}_${arch}.tar.gz"
  tar -zxvf /tmp/commitsar.tar.gz \
    --directory "${ROOT_DIR}/.sbin" \
    --no-same-owner \
    --exclude ./LICENSE \
    --exclude ./README.md
  chmod a+x "${ROOT_DIR}/.sbin/commitsar"
}

function gpustack::commit::commitsar::validate() {
  # shellcheck disable=SC2046
  if [[ -n "$(command -v $(gpustack::commit::commitsar::bin))" ]]; then
    if [[ $($(gpustack::commit::commitsar::bin) version 2>&1 | cut -d " " -f 7 2>&1 | head -n 1 | xargs echo -n) == "${commitsar_version#v}" ]]; then
      return 0
    fi
  fi

  gpustack::log::info "installing commitsar ${commitsar_version}"
  if gpustack::commit::commitsar::install; then
    gpustack::log::info "commitsar $($(gpustack::commit::commitsar::bin) version 2>&1 | cut -d " " -f 7 2>&1 | head -n 1 | xargs echo -n)"
    return 0
  fi
  gpustack::log::error "no commitsar available"
  return 1
}

function gpustack::commit::commitsar::bin() {
  local bin="commitsar"
  if [[ -f "${ROOT_DIR}/.sbin/commitsar" ]]; then
    bin="${ROOT_DIR}/.sbin/commitsar"
  fi
  echo -n "${bin}"
}

function gpustack::commit::lint() {
  if ! gpustack::commit::commitsar::validate; then
    gpustack::log::fatal "cannot execute commitsar as client is not found"
  fi

  gpustack::log::debug "commitsar $*"
  $(gpustack::commit::commitsar::bin) "$@"
}

function gpustack::lint::goimports::install() {
  local bin="${ROOT_DIR}/.sbin"
  mkdir -p "${bin}"
  GOBIN="${bin}" go install "golang.org/x/tools/cmd/goimports@${goimports_version}"
}

function gpustack::lint::goimports::validate() {
  # shellcheck disable=SC2046
  if [[ -n "$(command -v $(gpustack::lint::goimports::bin))" ]]; then
    return 0
  fi

  gpustack::log::info "installing goimports"
  if gpustack::lint::goimports::install; then
    gpustack::log::info "goimports installed"
    return 0
  fi
  gpustack::log::error "no goimports available"
  return 1
}

function gpustack::lint::goimports::bin() {
  local bin="goimports"
  if [[ -f "${ROOT_DIR}/.sbin/goimports" ]]; then
    bin="${ROOT_DIR}/.sbin/goimports"
  fi
  echo -n "${bin}"
}