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
EOF
  )
}

# chart_staging stages the upstream Helm charts the operator chart depends on into
# "deploy/gpustack-operator/chart/charts", the same way mod_staging above stages the
# Kubernetes modules: pull, unpack, stamp "_VERSION_", then apply the patches kept under
# "hack/<dest>". A tree whose stamp already matches the pinned version is left untouched,
# so repeated runs are no-ops and a patched tree is never clobbered. The trees are
# committed, which is what keeps "helm install" working from a bare clone and keeps CI
# offline-capable.
#
# A patch that no longer applies fails the whole function. That is the property the
# staging approach rests on: without it an upstream chart that moved under a patch would
# ship as a half-patched tree, and nothing downstream would say so.
#
# Bumping a chart is an edit to the version list below. Mirror the new upstream images
# before bumping, otherwise every install lands in ImagePullBackOff.
function chart_staging() {
  local charts_dir="${ROOT_DIR}/deploy/gpustack-operator/chart/charts"
  mkdir -p "${charts_dir}"

  local line url version dest staging_version patch_dir patch_file archive rejects
  while read -r line; do
    IFS=' ' read -r url version dest <<<"${line}"

    staging_version="$(cat "${dest}/_VERSION_" 2>/dev/null || echo "")"
    if [[ "${staging_version}" == "${version}" ]]; then
      gpustack::log::info "staging chart $(basename "${dest}") is up to date"
      continue
    fi

    gpustack::log::info "staging chart $(basename "${dest}") ${version} ..."
    rm -rf "${dest}"
    mkdir -p "${dest}"
    # A private archive per download: a fixed path under "/tmp" makes two concurrent
    # "make deps" in one checkout overwrite each other's tarball.
    archive="$(mktemp "${TMPDIR:-/tmp}/gpustack-chart.XXXXXXXX")"
    curl --retry 3 --retry-all-errors --retry-delay 3 \
      -o "${archive}" \
      -sSfL "${url}"
    # Every upstream release archives its chart under a single top-level directory,
    # which is stripped so that the tree lands directly in the destination.
    tar -zxf "${archive}" \
      --directory "${dest}" \
      --no-same-owner \
      --strip-components 1
    rm -f "${archive}"
    # The parent chart's README documents the whole configuration surface.
    rm -f "${dest}/README.md"
    echo -n "${version}" >"${dest}/_VERSION_"

    # Patch the freshly unpacked tree if any patches exist. The staged trees are never
    # edited in place, so every change to an upstream chart lives in a patch file.
    patch_dir="${ROOT_DIR}/hack/${dest#"${ROOT_DIR}/"}"
    if [[ ! -d "${patch_dir}" ]]; then
      continue
    fi
    # One "patch" call per file: a single "<${patch_dir}"/*.patch" redirect is an
    # ambiguous redirect the moment a tree carries a second patch.
    for patch_file in "${patch_dir}"/*.patch; do
      # A patch directory may exist without carrying any patch yet.
      if [[ ! -f "${patch_file}" ]]; then
        continue
      fi
      gpustack::log::info "applying $(basename "${patch_file}") to $(basename "${dest}")"
      if ! patch -p1 -N --forward --silent --directory "${dest}" <"${patch_file}"; then
        gpustack::log::error "failed to apply ${patch_file}, the upstream chart moved under it"
        return 1
      fi
      # A rejected hunk does not always cost an exit code, and the ".rej" it leaves is
      # ignored by ".gitignore", so neither the status line nor "git status" can be what
      # notices a half-applied patch. Assert here, while the tree is known freshly unpacked.
      rejects="$(find "${dest}" -type f \( -name '*.rej' -o -name '*.orig' \))"
      if [[ -n "${rejects}" ]]; then
        gpustack::log::error "applying ${patch_file} left rejects: ${rejects//$'\n'/ }"
        return 1
      fi
    done
  done < <(
    cat <<EOF
https://github.com/kubernetes-sigs/kueue/releases/download/v0.18.4/kueue-0.18.4.tgz 0.18.4 ${charts_dir}/kueue
https://github.com/kubernetes-sigs/node-feature-discovery/releases/download/v0.19.0/node-feature-discovery-chart-0.19.0.tgz 0.19.0 ${charts_dir}/node-feature-discovery
https://raw.githubusercontent.com/kubernetes-csi/csi-driver-nfs/refs/heads/master/charts/v4.13.2/csi-driver-nfs-4.13.2.tgz 4.13.2 ${charts_dir}/csi-driver-nfs
https://thxcode.github.io/k8s-csi-s3/charts/csi-s3-0.43.7.tgz 0.43.7 ${charts_dir}/csi-driver-s3
EOF
  )
}

function mod() {
  mod_staging

  # The operator chart's upstream dependencies are staged the same way, and for the same
  # reason, as the Kubernetes modules above.
  chart_staging

  if [[ "$*" =~ update ]]; then
    go get -u ./...
  fi
  go mod tidy
  go mod download
}

gpustack::log::info "+++ MOD +++"

mod "$@"

gpustack::log::info "--- MOD ---"
