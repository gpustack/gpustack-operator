#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
source "${ROOT_DIR}/hack/lib/init.sh"

function mod_staging() {
  staging_dir="${ROOT_DIR}/staging"
  mkdir -p "${staging_dir}"

  while read -r line; do
    IFS=' ' read -r repo version dest <<<"${line}"
    mkdir -p "${dest}"
    patch_dir="${ROOT_DIR}/hack/${dest#"${ROOT_DIR}/"}"
    staging_version="$(cat "${dest}/_VERSION_" 2>/dev/null || echo "")"
    if [[ "${staging_version}" == "${version}" ]]; then
      gpustack::log::info "staging ${repo} modules are up to date"
      continue
    fi
    rm -rf "${dest}" || true
    git -C "$(dirname "${dest}")" clone --recursive --shallow-submodules --depth 1 --branch "${version}" --single-branch "${repo}"
    rm -rf "${dest}/.git"
    rm -rf "${dest}/.github"
    rm -rf "${dest}/docs"
    echo -n "${version}" >"${dest}/_VERSION_"
    # patch existing staging if any patches exist.
    if [[ -d "${patch_dir}" ]]; then
      gpustack::log::info "applying ${patch_dir} patches for ${repo}"
      pushd "${dest}" >/dev/null 2>&1 &&
        patch -p1 -N --forward --silent <"${patch_dir}"/*.patch &&
        popd >/dev/null 2>&1
    fi
  done < <(
    cat <<EOF
https://github.com/kubernetes/code-generator v0.35.3 ${staging_dir}/k8s.io/code-generator
https://github.com/kubernetes/api v0.35.3 ${staging_dir}/k8s.io/api
https://github.com/kubernetes/apimachinery v0.35.3 ${staging_dir}/k8s.io/apimachinery
https://github.com/kubernetes/kube-aggregator v0.35.3 ${staging_dir}/k8s.io/kube-aggregator
https://github.com/kubernetes/apiextensions-apiserver v0.35.3 ${staging_dir}/k8s.io/apiextensions-apiserver
https://github.com/kubernetes/klog v2.140.0 ${staging_dir}/k8s.io/klog
https://github.com/go-logr/logr v1.4.3 ${staging_dir}/github.com/go-logr/logr
https://github.com/gogo/protobuf v1.3.2 ${staging_dir}/github.com/gogo/protobuf
https://github.com/dexidp/dex v2.44.0 ${staging_dir}/github.com/dexidp/dex
EOF
  )
}

function mod() {
  mod_staging

  if [[ "$*" =~ update ]]; then
    go get -u ./...
  fi
  go mod tidy
  go mod download
}

gpustack::log::info "+++ MOD +++"

mod "$@"

gpustack::log::info "--- MOD ---"
