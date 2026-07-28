#!/usr/bin/env bash

# Helm chart helpers for vendoring the dependencies of, documenting, scheming,
# linting and testing the charts under "deploy/". These functions rely on the
# following tunable versions:
#
#            HELM_VERSION  -  The Helm CLI version, default is v3.13.3.
#       HELM_DOCS_VERSION  -  The norwoodj/helm-docs version, default is v1.14.2.
#     HELM_SCHEMA_VERSION  -  The dadav/helm-schema version. It only ships Go
#                             pseudo-versions, so a pinned one (helm-schema 0.23.4)
#                             is used by default for reproducibility.
#   CHART_TESTING_VERSION  -  The helm/chart-testing version, default is v3.14.0.

helm_version=${HELM_VERSION:-"v3.21.0"}
helm_docs_version=${HELM_DOCS_VERSION:-"v1.14.2"}
helm_schema_version=${HELM_SCHEMA_VERSION:-"v0.0.0-20260612175628-fff8d76bdcd4"}
chart_testing_version=${CHART_TESTING_VERSION:-"v3.14.0"}

#
# Helm CLI.
#

function gpustack::helm::helm::bin() {
  local bin="helm"
  if [[ -f "${ROOT_DIR}/.sbin/helm" ]]; then
    bin="${ROOT_DIR}/.sbin/helm"
  fi
  echo -n "${bin}"
}

function gpustack::helm::helm::install() {
  local os
  os=$(gpustack::util::get_os)
  local arch
  arch=$(gpustack::util::get_arch)

  curl --retry 3 --retry-all-errors --retry-delay 3 \
    -o /tmp/helm.tar.gz \
    -sSfL "https://get.helm.sh/helm-${helm_version}-${os}-${arch}.tar.gz"

  tar -zxvf /tmp/helm.tar.gz \
    --directory "${ROOT_DIR}/.sbin" \
    --no-same-owner \
    --strip-components 1 \
    "${os}-${arch}/helm"
  chmod a+x "${ROOT_DIR}/.sbin/helm"
}

function gpustack::helm::helm::validate() {
  # shellcheck disable=SC2046
  if [[ -n "$(command -v $(gpustack::helm::helm::bin))" ]]; then
    return 0
  fi

  gpustack::log::info "installing helm ${helm_version}"
  if gpustack::helm::helm::install; then
    return 0
  fi
  gpustack::log::error "no helm available"
  return 1
}

#
# helm-docs (norwoodj/helm-docs).
#

function gpustack::helm::docs::bin() {
  local bin="helm-docs"
  if [[ -f "${ROOT_DIR}/.sbin/helm-docs" ]]; then
    bin="${ROOT_DIR}/.sbin/helm-docs"
  fi
  echo -n "${bin}"
}

function gpustack::helm::docs::install() {
  GOBIN="${ROOT_DIR}/.sbin" go install "github.com/norwoodj/helm-docs/cmd/helm-docs@${helm_docs_version}"
}

function gpustack::helm::docs::validate() {
  # shellcheck disable=SC2046
  if [[ -n "$(command -v $(gpustack::helm::docs::bin))" ]]; then
    return 0
  fi

  gpustack::log::info "installing helm-docs ${helm_docs_version}"
  if gpustack::helm::docs::install; then
    return 0
  fi
  gpustack::log::error "no helm-docs available"
  return 1
}

#
# helm-schema (dadav/helm-schema), generates values.schema.json from values.yaml.
#

function gpustack::helm::schema::bin() {
  local bin="helm-schema"
  if [[ -f "${ROOT_DIR}/.sbin/helm-schema" ]]; then
    bin="${ROOT_DIR}/.sbin/helm-schema"
  fi
  echo -n "${bin}"
}

function gpustack::helm::schema::install() {
  GOBIN="${ROOT_DIR}/.sbin" go install "github.com/dadav/helm-schema/cmd/helm-schema@${helm_schema_version}"
}

function gpustack::helm::schema::validate() {
  # shellcheck disable=SC2046
  if [[ -n "$(command -v $(gpustack::helm::schema::bin))" ]]; then
    return 0
  fi

  gpustack::log::info "installing helm-schema ${helm_schema_version}"
  if gpustack::helm::schema::install; then
    return 0
  fi
  gpustack::log::error "no helm-schema available"
  return 1
}

#
# Public functions.
#

# gpustack::helm::vendor vendors the upstream charts that the operator chart depends
# on into "deploy/gpustack-operator/chart/charts", mirroring how "hack/deps.sh" vendors
# the staging modules: pull, unpack, stamp "_VERSION_", then apply the patches kept
# under "hack/<dest>". A tree whose stamp already matches the pinned version is left
# untouched, so repeated runs are no-ops and a patched tree is never clobbered.
#
# Bumping a chart is an edit to the version list below. Mirror the new upstream images
# before bumping, otherwise every install lands in ImagePullBackOff.
function gpustack::helm::vendor() {
  local charts_dir="${ROOT_DIR}/deploy/gpustack-operator/chart/charts"
  mkdir -p "${charts_dir}"

  local line url version dest vendored_version patch_dir patch_file
  while read -r line; do
    IFS=' ' read -r url version dest <<<"${line}"

    vendored_version="$(cat "${dest}/_VERSION_" 2>/dev/null || echo "")"
    if [[ "${vendored_version}" == "${version}" ]]; then
      gpustack::log::info "vendored chart $(basename "${dest}") is up to date"
      continue
    fi

    gpustack::log::info "vendoring chart $(basename "${dest}") ${version} ..."
    rm -rf "${dest}"
    mkdir -p "${dest}"
    curl --retry 3 --retry-all-errors --retry-delay 3 \
      -o /tmp/chart.tgz \
      -sSfL "${url}"
    # Every upstream release archives its chart under a single top-level directory,
    # which is stripped so that the tree lands directly in the destination.
    tar -zxf /tmp/chart.tgz \
      --directory "${dest}" \
      --no-same-owner \
      --strip-components 1
    # The parent chart's README documents the whole configuration surface.
    rm -f "${dest}/README.md"
    echo -n "${version}" >"${dest}/_VERSION_"

    # Patch the freshly unpacked tree if any patches exist. The vendored trees are never
    # edited in place, so every change to an upstream chart lives in a patch file.
    patch_dir="${ROOT_DIR}/hack/${dest#"${ROOT_DIR}/"}"
    if [[ ! -d "${patch_dir}" ]]; then
      continue
    fi
    for patch_file in "${patch_dir}"/*.patch; do
      # A patch directory may exist without carrying any patch yet.
      if [[ ! -f "${patch_file}" ]]; then
        continue
      fi
      gpustack::log::info "applying $(basename "${patch_file}") to $(basename "${dest}")"
      patch -p1 -N --forward --silent --directory "${dest}" <"${patch_file}"
    done
  done < <(
    cat <<EOF
https://github.com/kubernetes-sigs/kueue/releases/download/v0.18.4/kueue-0.18.4.tgz 0.18.4 ${charts_dir}/kueue
https://github.com/kubernetes-sigs/kueue/releases/download/v0.17.8/kueue-0.17.8.tgz 0.17.8 ${charts_dir}/kueue-legacy
https://github.com/kubernetes-sigs/node-feature-discovery/releases/download/v0.19.0/node-feature-discovery-chart-0.19.0.tgz 0.19.0 ${charts_dir}/node-feature-discovery
https://raw.githubusercontent.com/kubernetes-csi/csi-driver-nfs/refs/heads/master/charts/v4.13.2/csi-driver-nfs-4.13.2.tgz 4.13.2 ${charts_dir}/csi-driver-nfs
https://thxcode.github.io/k8s-csi-s3/charts/csi-s3-0.43.7.tgz 0.43.7 ${charts_dir}/csi-driver-s3
EOF
  )
}

# gpustack::helm::deps resolves the chart dependencies into "<chart>/charts".
# It uses "dependency update" so the classic (https) repositories declared in
# Chart.yaml are resolved without requiring a prior "helm repo add".
function gpustack::helm::deps() {
  if ! gpustack::helm::helm::validate; then
    gpustack::log::error "cannot execute helm as it hasn't installed"
    return 1
  fi

  local target="$1"

  gpustack::log::info "resolving dependencies of ${target} ..."
  $(gpustack::helm::helm::bin) dependency update "${target}"
}

# gpustack::helm::docs generates the README.md of the given chart from its
# README.md.gotmpl template and value annotations.
function gpustack::helm::docs() {
  if ! gpustack::helm::docs::validate; then
    gpustack::log::error "cannot execute helm-docs as it hasn't installed"
    return 1
  fi

  local target="$1"

  gpustack::log::info "documenting ${target} ..."
  # NB(thxCode): restrict the generation to the given chart. The search root also finds
  # the vendored subcharts under "charts/", and helm-docs would otherwise write a
  # generated README.md into every one of them.
  $(gpustack::helm::docs::bin) \
    --log-level=warning \
    --sort-values-order=file \
    --document-dependency-values=false \
    --template-files=README.md.gotmpl \
    --chart-search-root="${target}" \
    --chart-to-generate="${target}"
}

# gpustack::helm::schema generates the values.schema.json of the given chart.
function gpustack::helm::schema() {
  if ! gpustack::helm::schema::validate; then
    gpustack::log::error "cannot execute helm-schema as it hasn't installed"
    return 1
  fi

  local target="$1"

  gpustack::log::info "scheming ${target} ..."
  # NB(thxCode): skip auto-generating "additionalProperties: false" so that the
  # free-form maps (env, nodeSelector, resources, labels, ...) and subchart value
  # passthrough are not rejected during values validation.
  #
  # NB(thxCode): "--dependencies-filter" names no declared dependency on purpose. It is
  # what keeps helm-schema from parsing the values.yaml of the vendored subcharts, one of
  # which (node-feature-discovery) carries a comment its parser rejects, failing the whole
  # run. "--no-dependencies" only keeps those values out of the parent schema; it does not
  # prevent the parse.
  $(gpustack::helm::schema::bin) \
    --chart-search-root="${target}" \
    --no-dependencies \
    --dependencies-filter=none \
    --skip-auto-generation=additionalProperties \
    --add-schema-reference
}

# gpustack::helm::ct::chart_repos echoes the classic (https) dependency
# repositories declared in the chart's Chart.yaml as a "name=url,..." list, so
# chart-testing can register them before building dependencies. OCI repositories
# do not need registration and are skipped.
function gpustack::helm::ct::chart_repos() {
  local target="$1"

  # `|| true`: a chart with no classic (https) dependency repositories yields no
  # matches, and a non-zero grep under `set -o pipefail` would otherwise abort lint.
  { grep -E '^[[:space:]]*repository:[[:space:]]*https' "${target}/Chart.yaml" 2>/dev/null || true; } |
    awk '{print $2}' |
    awk '{print "gpustack-dep-" NR "=" $0}' |
    paste -sd, -
}

# gpustack::helm::lint lints the given chart with chart-testing in a container.
function gpustack::helm::lint() {
  local target="$1"

  # The dependencies are vendored, patched trees: "dependency update" would clobber them.
  if ! gpustack::helm::vendor; then
    return 1
  fi

  local chart_repos
  chart_repos=$(gpustack::helm::ct::chart_repos "${target}")

  gpustack::log::info "linting ${target} ..."
  docker run \
    --rm \
    --network host \
    --volume "${ROOT_DIR}:/workspace" \
    --workdir /workspace \
    "quay.io/helmpack/chart-testing:${chart_testing_version}" \
    ct lint \
    --charts "${target#"${ROOT_DIR}/"}" \
    --chart-repos "${chart_repos}" \
    --validate-maintainers=false \
    --check-version-increment=false
}

# gpustack::helm::test installs the given chart onto the current cluster with
# chart-testing in a container. Requires a reachable cluster (e.g. kind).
function gpustack::helm::test() {
  local target="$1"

  # The dependencies are vendored, patched trees: "dependency update" would clobber them.
  if ! gpustack::helm::vendor; then
    return 1
  fi

  local chart_repos
  chart_repos=$(gpustack::helm::ct::chart_repos "${target}")

  gpustack::log::info "testing ${target} ..."
  docker run \
    --rm \
    --network host \
    --volume "${ROOT_DIR}:/workspace" \
    --volume "${HOME}/.kube/config:/root/.kube/config:ro" \
    --workdir /workspace \
    "quay.io/helmpack/chart-testing:${chart_testing_version}" \
    ct install \
    --charts "${target#"${ROOT_DIR}/"}" \
    --chart-repos "${chart_repos}" \
    --helm-extra-args "--timeout 600s"
}
