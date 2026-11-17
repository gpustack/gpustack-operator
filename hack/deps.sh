#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
source "${ROOT_DIR}/hack/lib/init.sh"

function mod_staging() {
  local staging_dir="${ROOT_DIR}/staging"
  mkdir -p "${staging_dir}"

  local line repo version dest patch_dir staging_version rel_mod cache_tar cache_git cache_dir
  while read -r line; do
    IFS=' ' read -r repo version dest <<<"${line}"
    mkdir -p "$(dirname "${dest}")"
    patch_dir="${ROOT_DIR}/hack/${dest#"${ROOT_DIR}/"}"
    staging_version="$(cat "${dest}/_VERSION_" 2>/dev/null || echo "")"
    if [[ "${staging_version}" == "${version}" ]]; then
      gpustack::log::info "staging ${repo} modules are up to date"
      continue
    fi

    rel_mod="${dest#"${staging_dir}/"}"
    if [[ -n "${GPUSTACK_DEPS_CACHE_DIR:-}" ]]; then
      cache_tar="${GPUSTACK_DEPS_CACHE_DIR}/staging/${rel_mod}-${version}.tar.gz"
      cache_git="${GPUSTACK_DEPS_CACHE_DIR}/mods/${rel_mod}"
      cache_dir="${GPUSTACK_DEPS_CACHE_DIR}/staging/${rel_mod}@${version}"
      if [[ -f "${cache_tar}" ]]; then
        gpustack::log::info "staging ${repo} ${version} from cache tarball ${cache_tar}"
        rm -rf "${dest}" || true
        mkdir -p "${dest}"
        tar -zxf "${cache_tar}" --directory "${dest}"
      elif [[ -d "${cache_git}" ]]; then
        gpustack::log::info "staging ${repo} ${version} from cache git mirror ${cache_git}"
        rm -rf "${dest}" || true
        git -C "$(dirname "${dest}")" clone --recursive --shallow-submodules --depth 1 --branch "${version}" --single-branch "file://${cache_git}"
        rm -rf "${dest}/.git" "${dest}/.github" "${dest}/docs"
      elif [[ -d "${cache_dir}" ]]; then
        gpustack::log::info "staging ${repo} ${version} from cache directory ${cache_dir}"
        rm -rf "${dest}" || true
        cp -R "${cache_dir}" "${dest}"
        rm -rf "${dest}/.git" "${dest}/.github" "${dest}/docs"
      elif [[ "${GPUSTACK_DEPS_OFFLINE:-false}" == "true" ]]; then
        gpustack::log::error "offline cache miss for module ${rel_mod} ${version} in ${GPUSTACK_DEPS_CACHE_DIR}"
        return 1
      else
        rm -rf "${dest}" || true
        git -C "$(dirname "${dest}")" clone --recursive --shallow-submodules --depth 1 --branch "${version}" --single-branch "${repo}"
        rm -rf "${dest}/.git" "${dest}/.github" "${dest}/docs"
        if [[ -d "${GPUSTACK_DEPS_CACHE_DIR}" ]]; then
          mkdir -p "$(dirname "${cache_tar}")"
          tar -zcf "${cache_tar}" -C "${dest}" .
        fi
      fi
    else
      rm -rf "${dest}" || true
      git -C "$(dirname "${dest}")" clone --recursive --shallow-submodules --depth 1 --branch "${version}" --single-branch "${repo}"
      rm -rf "${dest}/.git"
      rm -rf "${dest}/.github"
      rm -rf "${dest}/docs"
    fi

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
https://github.com/kubernetes/code-generator v0.36.5 ${staging_dir}/k8s.io/code-generator
https://github.com/kubernetes/api v0.36.5 ${staging_dir}/k8s.io/api
https://github.com/kubernetes/apimachinery v0.36.5 ${staging_dir}/k8s.io/apimachinery
https://github.com/kubernetes/kube-aggregator v0.36.5 ${staging_dir}/k8s.io/kube-aggregator
https://github.com/kubernetes/apiextensions-apiserver v0.36.5 ${staging_dir}/k8s.io/apiextensions-apiserver
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
  local chart_name archive_name cached_archive candidate
  while read -r line; do
    IFS=' ' read -r url version dest <<<"${line}"

    staging_version="$(cat "${dest}/_VERSION_" 2>/dev/null || echo "")"
    if [[ "${staging_version}" == "${version}" ]]; then
      gpustack::log::info "staging chart $(basename "${dest}") is up to date"
      continue
    fi

    gpustack::log::info "staging chart $(basename "${dest}") ${version} ..."
    chart_name="$(basename "${dest}")"
    archive_name="$(basename "${url}")"
    # A private archive per download: a fixed path under "/tmp" makes two concurrent
    # "make deps" in one checkout overwrite each other's tarball.
    archive="$(mktemp "${TMPDIR:-/tmp}/gpustack-chart.XXXXXXXX")"
    if [[ -n "${GPUSTACK_DEPS_CACHE_DIR:-}" ]]; then
      cached_archive=""
      for candidate in \
        "${GPUSTACK_DEPS_CACHE_DIR}/charts/${chart_name}-${version}.tgz" \
        "${GPUSTACK_DEPS_CACHE_DIR}/charts/${archive_name}" \
        "${GPUSTACK_DEPS_CACHE_DIR}/${archive_name}"; do
        if [[ -f "${candidate}" ]]; then
          cached_archive="${candidate}"
          break
        fi
      done
      if [[ -n "${cached_archive}" ]]; then
        gpustack::log::info "using cached chart archive ${cached_archive}"
        cp "${cached_archive}" "${archive}"
      elif [[ "${GPUSTACK_DEPS_OFFLINE:-false}" == "true" ]]; then
        rm -f "${archive}"
        gpustack::log::error "offline cache miss for chart ${chart_name} ${version} (${archive_name}) in ${GPUSTACK_DEPS_CACHE_DIR}"
        return 1
      else
        curl --retry 3 --retry-all-errors --retry-delay 3 \
          -o "${archive}" \
          -sSfL "${url}"
        if [[ -d "${GPUSTACK_DEPS_CACHE_DIR}" ]]; then
          mkdir -p "${GPUSTACK_DEPS_CACHE_DIR}/charts"
          cp "${archive}" "${GPUSTACK_DEPS_CACHE_DIR}/charts/${chart_name}-${version}.tgz"
        fi
      fi
    else
      curl --retry 3 --retry-all-errors --retry-delay 3 \
        -o "${archive}" \
        -sSfL "${url}"
    fi
    rm -rf "${dest}"
    mkdir -p "${dest}"
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
      # "-F0" demands that every context line match exactly, so a patch may land at a shifted
      # line number but never on drifted context, which is the case where "patch" would
      # otherwise guess. That makes a shift harmless, and "--no-backup-if-mismatch" drops the
      # ".orig" one leaves behind: the content is right, only the recorded line numbers moved,
      # and a patch is content. Two patches touching one file shift each other, so without
      # this an edit to either would demand re-capturing the other for nothing.
      if ! patch -p1 -N --forward --silent -F0 --no-backup-if-mismatch \
        --directory "${dest}" <"${patch_file}"; then
        gpustack::log::error "failed to apply ${patch_file}, the upstream chart moved under it"
        return 1
      fi
      # A rejected hunk does not always cost an exit code, and the ".rej" it leaves is
      # ignored by ".gitignore", so neither the status line nor "git status" can be what
      # notices a half-applied patch. Assert here, while the tree is known freshly unpacked.
      rejects="$(find "${dest}" -type f -name '*.rej')"
      if [[ -n "${rejects}" ]]; then
        gpustack::log::error "applying ${patch_file} left rejects: ${rejects//$'\n'/ }"
        return 1
      fi
    done
  done < <(
    cat <<EOF
https://github.com/kubernetes-sigs/kueue/releases/download/v0.18.11/kueue-0.18.11.tgz 0.18.11 ${charts_dir}/kueue
https://github.com/kubernetes-sigs/node-feature-discovery/releases/download/v0.19.0/node-feature-discovery-chart-0.19.0.tgz 0.19.0 ${charts_dir}/node-feature-discovery
https://raw.githubusercontent.com/kubernetes-csi/csi-driver-nfs/refs/heads/master/charts/v4.13.4/csi-driver-nfs-4.13.4.tgz 4.13.4 ${charts_dir}/csi-driver-nfs
https://thxcode.github.io/k8s-csi-s3/charts/csi-s3-0.43.7.tgz 0.43.7 ${charts_dir}/csi-driver-s3
https://dsx-ai-factory.github.io/topograph/topograph-1.0.0.tgz 1.0.0 ${charts_dir}/topograph
EOF
  )
}

function mod() {
  local stage_only="${GPUSTACK_DEPS_STAGE_ONLY:-all}"
  case "${stage_only}" in
    charts)
      chart_staging
      return
      ;;
    mods)
      mod_staging
      return
      ;;
    all)
      mod_staging

      # The operator chart's upstream dependencies are staged the same way, and for the same
      # reason, as the Kubernetes modules above.
      chart_staging
      ;;
    *)
      gpustack::log::error "unsupported GPUSTACK_DEPS_STAGE_ONLY=${stage_only} (expected charts, mods, or all)"
      return 1
      ;;
  esac

  if [[ "$*" =~ update ]]; then
    go get -u ./...
  fi
  if [[ "${GPUSTACK_DEPS_OFFLINE:-false}" == "true" ]]; then
    GOPROXY="${GOPROXY:-off}" go mod tidy
  else
    go mod tidy
    go mod download
  fi
}

gpustack::log::info "+++ MOD +++"

mod "$@"

gpustack::log::info "--- MOD ---"
