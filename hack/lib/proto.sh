#!/usr/bin/env bash

# -----------------------------------------------------------------------------
# Protoc variables helpers. These functions need the
# following variables:
#
#                   PROTOC_VERSION  -  The protoc version, default is v33.5.
#            PROTOC_GEN_GO_VERSION  -  The protoc-gen-go version, default is v1.36.11.
#          PROTOC_GEN_GOGO_VERSION  -  The protoc version, default is master.
#       PROTOC_GEN_GO_GRPC_VERSION  -  The protoc-gen-go-grpc version, default is v1.78.0.
#      PROTOC_GEN_VALIDATE_VERSION  -  The protoc-gen-validate version, default is v1.3.0.
#  PROTOC_GEN_GRPC_GATEWAY_VERSION  -  The protoc-gen-grpc-gateway version, default is v2.27.7.
#
# Refs:
# - https://grpc.io/docs/protoc-installation/
# - https://grpc.io/docs/languages/go/quickstart/

protoc_version=${PROTOC_VERSION:-"v33.5"}
protoc_gen_go_version=${PROTOC_GEN_GO_VERSION:-"v1.36.11"}
# LIMITED: a commit hash rather than a version, for two reasons:
#   - the project is unmaintained; this is the head of master (2022-10-24) and nothing
#     has landed since;
#   - its last tag (v1.3.2, 2020) predates commits this repository depends on.
# As with goimports, a floating "master" would move the drift gate baseline.
protoc_gen_gogo_version=${PROTOC_GEN_GOGO_VERSION:-"f67b8970b736"}
protoc_gen_go_grpc_version=${PROTOC_GEN_GO_GRPC_VERSION:-"v1.78.0"}
protoc_gen_validate_version=${PROTOC_GEN_VALIDATE_VERSION:-"v1.3.0"}
protoc_gen_grpc_gateway_version=${PROTOC_GEN_GRPC_GATEWAY_VERSION:-"v2.27.7"}

function gpustack::protoc::protoc::install() {
  local os
  os="$(gpustack::util::get_raw_os)"
  if [[ "${os}" == "darwin" ]]; then
    os="osx"
  fi
  local arch
  arch="$(gpustack::util::get_raw_arch)"
  if [[ "${arch}" == "arm64" ]]; then
    arch="aarch_64"
  fi
  if [[ "${arch}" == "amd64" ]]; then
    arch="x86_64"
  fi
  local url
  url="https://github.com/protocolbuffers/protobuf/releases/download/${protoc_version}/protoc-${protoc_version#v}-${os}-${arch}.zip"
  gpustack::log::info "downloading protoc ${protoc_version} from ${url}"
  curl --retry 3 --retry-all-errors --retry-delay 3 \
    -o /tmp/protoc.zip \
    -sSfL "${url}"
  unzip -qou /tmp/protoc.zip -d "${ROOT_DIR}/.sbin/protoc"
  chmod a+x "${ROOT_DIR}/.sbin/protoc/bin/protoc"
  file "${ROOT_DIR}/.sbin/protoc/bin/protoc"
}

function gpustack::protoc::protoc::validate() {
  # shellcheck disable=SC2046
  if [[ -n "$(command -v $(gpustack::protoc::protoc::bin))" ]]; then
    if [[ "$($(gpustack::protoc::protoc::bin) --version 2>&1)" == "libprotoc ${protoc_version#v}" ]]; then
      return 0
    fi
  fi

  gpustack::log::info "installing protoc ${protoc_version}"
  if gpustack::protoc::protoc::install; then
    gpustack::log::info "protoc $($(gpustack::protoc::protoc::bin) --version 2>&1 | cut -d " " -f 2 2>&1 | head -n 1)"
    return 0
  fi
  gpustack::log::error "no protoc available"
  return 1
}

function gpustack::protoc::protoc::bin() {
  local bin="protoc"
  if [[ -f "${ROOT_DIR}/.sbin/protoc/bin/protoc" ]]; then
    bin="${ROOT_DIR}/.sbin/protoc/bin/protoc"
  fi
  echo -n "${bin}"
}

function gpustack::protoc::protoc_gen_go::install() {
  local os
  os="$(gpustack::util::get_raw_os)"
  local arch
  arch="$(gpustack::util::get_raw_arch)"
  local url
  url="https://github.com/protocolbuffers/protobuf-go/releases/download/${protoc_gen_go_version}/protoc-gen-go.${protoc_gen_go_version}.${os}.${arch}.tar.gz"
  gpustack::log::info "downloading protoc-gen-go ${protoc_gen_go_version} from ${url}"
  curl --retry 3 --retry-all-errors --retry-delay 3 \
    -o /tmp/protoc-gen.tar.gz \
    -sSfL "${url}"
  tar -zxvf /tmp/protoc-gen.tar.gz \
    --directory "${ROOT_DIR}/.sbin/protoc/bin" \
    --no-same-owner \
    --exclude ./LICENSE \
    --exclude ./README.md
  chmod a+x "${ROOT_DIR}/.sbin/protoc/bin/protoc-gen-go"
  file "${ROOT_DIR}/.sbin/protoc/bin/protoc-gen-go"
}

function gpustack::protoc::protoc_gen_go::validate() {
  # shellcheck disable=SC2046
  if [[ -n "$(command -v $(gpustack::protoc::protoc_gen_go::bin))" ]]; then
    if [[ "$($(gpustack::protoc::protoc_gen_go::bin) --version 2>&1)" == "protoc-gen-go ${protoc_gen_go_version}" ]]; then
      return 0
    fi
  fi

  gpustack::log::info "installing protoc-gen-go ${protoc_gen_go_version}"
  if gpustack::protoc::protoc_gen_go::install; then
    gpustack::log::info "protoc-gen-go installed"
    return 0
  fi
  gpustack::log::error "no protoc-gen-go available"
  return 1
}

function gpustack::protoc::protoc_gen_go::bin() {
  local bin="protoc-gen-go"
  if [[ -f "${ROOT_DIR}/.sbin/protoc/bin/protoc-gen-go" ]]; then
    bin="${ROOT_DIR}/.sbin/protoc/bin/protoc-gen-go"
  fi
  echo -n "${bin}"
}

function gpustack::protoc::protoc_gen_gogo::install() {
  local bin="${ROOT_DIR}/.sbin/protoc/bin"
  mkdir -p "${bin}"
  GOBIN="${bin}" go install "github.com/gogo/protobuf/protoc-gen-gogo@${protoc_gen_gogo_version}"
}

function gpustack::protoc::protoc_gen_gogo::validate() {
  # protoc-gen-gogo has no version flag -- it is a protoc plugin and answers --version with
  # "no files to generate" on exit 1 -- so the pin is compared against the module version
  # `go install` stamped into the binary. Existence alone would keep a .sbin populated before a
  # pin bump, and `make generate` would run the generator the bump replaced.
  # shellcheck disable=SC2046
  if [[ -n "$(command -v $(gpustack::protoc::protoc_gen_gogo::bin))" ]]; then
    # shellcheck disable=SC2046
    if gpustack::util::go_module_version_is \
      "$(gpustack::util::go_module_version $(gpustack::protoc::protoc_gen_gogo::bin))" \
      "${protoc_gen_gogo_version}"; then
      return 0
    fi
  fi

  gpustack::log::info "installing protoc-gen-gogo"
  if gpustack::protoc::protoc_gen_gogo::install; then
    gpustack::log::info "protoc-gen-gogo installed"
    return 0
  fi
  gpustack::log::error "no protoc-gen-gogo available"
  return 1
}

function gpustack::protoc::protoc_gen_gogo::bin() {
  local bin="protoc-gen-gogo"
  if [[ -f "${ROOT_DIR}/.sbin/protoc/bin/protoc-gen-gogo" ]]; then
    bin="${ROOT_DIR}/.sbin/protoc/bin/protoc-gen-gogo"
  fi
  echo -n "${bin}"
}

function gpustack::protoc::protoc_gen_go_grpc::install() {
  local bin="${ROOT_DIR}/.sbin/protoc/bin"
  mkdir -p "${bin}"
  GOBIN="${bin}" go install "google.golang.org/grpc/cmd/protoc-gen-go-grpc@${protoc_gen_go_grpc_version}"
}

function gpustack::protoc::protoc_gen_go_grpc::validate() {
  # shellcheck disable=SC2046
  if [[ -n "$(command -v $(gpustack::protoc::protoc_gen_go_grpc::bin))" ]]; then
    if [[ "$($(gpustack::protoc::protoc_gen_go_grpc::bin) --version 2>&1)" == "protoc-gen-go-grpc ${protoc_gen_go_grpc_version#v}" ]]; then
      return 0
    fi
  fi

  gpustack::log::info "installing protoc-gen-go-grpc ${protoc_gen_go_grpc_version}"
  if gpustack::protoc::protoc_gen_go_grpc::install; then
    gpustack::log::info "protoc-gen-go-grpc installed"
    return 0
  fi
  gpustack::log::error "no protoc-gen-go-grpc available"
  return 1
}

function gpustack::protoc::protoc_gen_go_grpc::bin() {
  local bin="protoc-gen-go-grpc"
  if [[ -f "${ROOT_DIR}/.sbin/protoc/bin/protoc-gen-go-grpc" ]]; then
    bin="${ROOT_DIR}/.sbin/protoc/bin/protoc-gen-go-grpc"
  fi
  echo -n "${bin}"
}

function gpustack::protoc::protoc_gen_validate::install() {
  local bin="${ROOT_DIR}/.sbin/protoc/bin"
  mkdir -p "${bin}"
  GOBIN="${bin}" go install "github.com/envoyproxy/protoc-gen-validate@${protoc_gen_validate_version}"
}

function gpustack::protoc::protoc_gen_validate::validate() {
  # shellcheck disable=SC2046
  if [[ -n "$(command -v $(gpustack::protoc::protoc_gen_validate::bin))" ]]; then
    return 0
  fi

  gpustack::log::info "installing protoc-gen-validate ${protoc_gen_validate_version}"
  if gpustack::protoc::protoc_gen_validate::install; then
    gpustack::log::info "protoc-gen-validate installed"
    return 0
  fi
  gpustack::log::error "no protoc-gen-validate available"
  return 1
}

function gpustack::protoc::protoc_gen_validate::bin() {
  local bin="protoc-gen-validate"
  if [[ -f "${ROOT_DIR}/.sbin/protoc/bin/protoc-gen-validate" ]]; then
    bin="${ROOT_DIR}/.sbin/protoc/bin/protoc-gen-validate"
  fi
  echo -n "${bin}"
}

function gpustack::protoc::protoc_gen_grpc_gateway::install() {
  local os
  os="$(gpustack::util::get_raw_os)"
  local arch
  arch="$(gpustack::util::get_raw_arch)"
  if [[ "${arch}" == "amd64" ]]; then
    arch="x86_64"
  fi
  local suffix=""
  if [[ "${os}" == "windows" ]]; then
    suffix=".exe"
  fi
  local url
  url="https://github.com/grpc-ecosystem/grpc-gateway/releases/download/${protoc_gen_grpc_gateway_version}/protoc-gen-grpc-gateway-${protoc_gen_grpc_gateway_version}-${os}-${arch}${suffix}"
  gpustack::log::info "downloading protoc-gen-grpc-gateway ${protoc_gen_grpc_gateway_version} from ${url}"
  curl --retry 3 --retry-all-errors --retry-delay 3 \
    -o /tmp/protoc-gen-grpc-gateway \
    -sSfL "${url}"
  mv /tmp/protoc-gen-grpc-gateway "${ROOT_DIR}/.sbin/protoc/bin/protoc-gen-grpc-gateway"
  chmod a+x "${ROOT_DIR}/.sbin/protoc/bin/protoc-gen-grpc-gateway"
  file "${ROOT_DIR}/.sbin/protoc/bin/protoc-gen-grpc-gateway"
  url="https://github.com/grpc-ecosystem/grpc-gateway/releases/download/${protoc_gen_grpc_gateway_version}/protoc-gen-openapiv2-${protoc_gen_grpc_gateway_version}-${os}-${arch}${suffix}"
  gpustack::log::info "downloading protoc-gen-openapiv2 ${protoc_gen_grpc_gateway_version} from ${url}"
  curl --retry 3 --retry-all-errors --retry-delay 3 \
    -o /tmp/protoc-gen-swagger \
    -sSfL "${url}"
  mv /tmp/protoc-gen-swagger "${ROOT_DIR}/.sbin/protoc/bin/protoc-gen-openapiv2"
  chmod a+x "${ROOT_DIR}/.sbin/protoc/bin/protoc-gen-openapiv2"
  file "${ROOT_DIR}/.sbin/protoc/bin/protoc-gen-openapiv2"
}

function gpustack::protoc::protoc_gen_grpc_gateway::validate() {
  # shellcheck disable=SC2046
  if [[ -n "$(command -v $(gpustack::protoc::protoc_gen_grpc_gateway::bin))" ]]; then
    if [[ "$($(gpustack::protoc::protoc_gen_grpc_gateway::bin) --version 2>&1 | cut -d " " -f 2 2>&1 | head -n 1)" == "${protoc_gen_grpc_gateway_version#v}," ]]; then
      return 0
    fi
  fi

  gpustack::log::info "installing protoc-gen-grpc-gateway ${protoc_gen_grpc_gateway_version}"
  if gpustack::protoc::protoc_gen_grpc_gateway::install; then
    gpustack::log::info "protoc-gen-grpc-gateway installed"
    return 0
  fi
  gpustack::log::error "no protoc-gen-grpc-gateway available"
  return 1
}

function gpustack::protoc::protoc_gen_grpc_gateway::bin() {
  local bin="protoc-gen-grpc-gateway"
  if [[ -f "${ROOT_DIR}/.sbin/protoc/bin/protoc-gen-grpc-gateway" ]]; then
    bin="${ROOT_DIR}/.sbin/protoc/bin/protoc-gen-grpc-gateway"
  fi
  echo -n "${bin}"
}

function gpustack::protoc::generate() {
  if ! gpustack::protoc::protoc::validate; then
    gpustack::log::error "cannot execute protoc as it hasn't installed"
    return 1
  fi

  if ! gpustack::protoc::protoc_gen_go::validate; then
    gpustack::log::error "cannot execute protoc-gen-go as it hasn't installed"
    return 1
  fi

  if ! gpustack::protoc::protoc_gen_go_grpc::validate; then
    gpustack::log::error "cannot execute protoc-gen-go-grpc as it hasn't installed"
    return 1
  fi

  if ! gpustack::protoc::protoc_gen_validate::validate; then
    gpustack::log::error "cannot execute protoc-gen-validate as it hasn't installed"
    return 1
  fi

  if ! gpustack::protoc::protoc_gen_grpc_gateway::validate; then
    gpustack::log::error "cannot execute protoc-gen-grpc-gateway as it hasn't installed"
    return 1
  fi

  local filepath="${1:-}"
  if [[ ! -f ${filepath} ]]; then
    gpustack::log::error "${filepath} is required"
    return 1
  fi
  local filedir
  filedir=$(dirname "${filepath}")
  local filename
  filename=$(basename "${filepath}" ".proto")

  # generate
  $(gpustack::protoc::protoc::bin) \
    --proto_path="${filedir}" \
    --proto_path="${ROOT_DIR}/.sbin/protoc/include" \
    --proto_path="${ROOT_DIR}/staging" \
    --go_out="paths=source_relative:${filedir}" \
    --go-grpc_out="paths=source_relative:${filedir}" \
    --validate_out="lang=go,paths=source_relative:${filedir}" \
    --grpc-gateway_out="paths=source_relative:${filedir}" \
    "${filepath}"

  # rename
  mv "${filedir}/${filename}_grpc.pb.go" "${filedir}/${filename}.pb.grpc.go"
}
