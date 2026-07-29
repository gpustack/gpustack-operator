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

# gpustack::helm::verify_images::render renders the given chart at its defaults, which is
# what a user installing it gets: every component the operator deploys is on by default, and
# every component an upstream chart ships switched off stays off, because the operator only
# deploys the parts it uses. So this passes no component switch at all — anything the
# assertion cannot see at the defaults is something the chart does not ship. Arguments past
# the target are passed to helm verbatim. The Kubernetes version is pinned so the render is
# deterministic; which version it is does not matter here, as no image reference varies with
# it.
function gpustack::helm::verify_images::render() {
  local target="$1"
  shift 1

  $(gpustack::helm::helm::bin) template gpustack-operator "${target}" \
    --kube-version "1.33.0" \
    "$@"
}

# gpustack::helm::verify_images::values extracts the values of one rendered field. A key
# carrying no value is skipped: the Kueue Workload CRD embeds a PodSpec schema whose
# "image" and "imagePullPolicy" property names would otherwise read as container fields.
function gpustack::helm::verify_images::values() {
  local field="$1"

  grep -E "^[[:space:]]*${field}:[[:space:]]*[^[:space:]]" |
    sed -E "s/^[[:space:]]*${field}:[[:space:]]*//" |
    tr -d "\"'" |
    sort -u
}

# gpustack::helm::verify_images::field fails when a rendered value of the given field is
# not the expected one, or when the field renders nothing at all — a check that passes
# because it matched nothing is worse than no check.
function gpustack::helm::verify_images::field() {
  local field="$1" match="$2" expected="$3" rendered="$4"

  local values=()
  while IFS= read -r value; do
    [[ -n "${value}" ]] && values+=("${value}")
  done < <(gpustack::helm::verify_images::values "${field}" <<<"${rendered}")

  if [[ ${#values[@]} -eq 0 ]]; then
    gpustack::log::error "no ${field} rendered, the check would pass vacuously"
    return 1
  fi

  local failed=0 value
  for value in "${values[@]}"; do
    if [[ "${match}" == "prefix" && "${value}" == "${expected}"* ]]; then
      continue
    fi
    if [[ "${match}" == "exact" && "${value}" == "${expected}" ]]; then
      continue
    fi
    gpustack::log::error "${field} \"${value}\" does not honour the global override"
    failed=1
  done

  if [[ ${failed} -ne 0 ]]; then
    return 1
  fi
  gpustack::log::info "${#values[@]} distinct ${field} values honour the global override"
}

# gpustack::helm::verify_images::pull_secrets fails when a workload renders no image pull
# secret, or renders one that is not the parent's. Two shapes are in play: a YAML list, and
# the single-line JSON that the Node Feature Discovery chart emits natively.
function gpustack::helm::verify_images::pull_secrets() {
  local expected="$1" rendered="$2"

  local workloads secrets names
  workloads="$(grep -cE '^kind: (Deployment|DaemonSet|Job|StatefulSet)$' <<<"${rendered}" || true)"
  # The indentation bound keeps the Kueue Workload CRD's PodSpec schema property out.
  secrets="$(grep -cE '^ {1,10}imagePullSecrets:' <<<"${rendered}" || true)"

  if [[ "${workloads}" -eq 0 ]] || [[ "${secrets}" -ne "${workloads}" ]]; then
    gpustack::log::error "${secrets} of ${workloads} workloads carry an image pull secret"
    return 1
  fi

  names="$( {
    grep -oE '\{"name":"[^"]+"\}' <<<"${rendered}" | sed -E 's/.*"name":"([^"]+)".*/\1/'
    awk '/^ {1,10}imagePullSecrets:$/{ found = 1; next } found && /^ *- +name: /{ print $3; next } { found = 0 }' <<<"${rendered}"
  } | sort -u)"

  if [[ "${names}" != "${expected}" ]]; then
    gpustack::log::error "image pull secrets resolve to \"${names//$'\n'/, }\""
    return 1
  fi
  gpustack::log::info "all ${workloads} workloads carry the global image pull secret"
}

# gpustack::helm::verify_images asserts that the given chart's "global.*" image knobs reach
# every image reference it renders, the ones its staged subcharts contribute included.
#
# The chart is rendered with every subchart enabled and with the components upstream ships
# switched off — KueueViz, the NFD topology updater, the NFS snapshot controller — switched
# on, because a patched image field nobody renders is a field nobody verifies. Every value
# is then extracted and asserted, so a missed field fails here instead of surfacing as an
# ImagePullBackOff in an airgapped cluster.
function gpustack::helm::verify_images() {
  if ! gpustack::helm::helm::validate; then
    gpustack::log::error "cannot render the chart as helm hasn't installed"
    return 1
  fi

  local target="$1"
  local registry="reg.local" namespace="mirror"
  local pull_policy="Always" pull_secret="mirror-pull-secret"

  gpustack::log::info "verifying ${target} images ..."

  # Every assertion spans the whole render, the chart's own workloads and its subcharts'
  # alike: one global knob is supposed to mean one behaviour everywhere, so a check that
  # exempted the parent would be conceding the thing worth proving.
  local failed=0 rendered
  rendered="$(gpustack::helm::verify_images::render "${target}" \
    --set "global.imageRegistry=${registry}" \
    --set "global.imageNamespace=${namespace}" \
    --set "global.imagePullPolicy=${pull_policy}" \
    --set "global.imagePullSecrets[0].name=${pull_secret}")"

  gpustack::helm::verify_images::field \
    "image" prefix "${registry}/${namespace}/" "${rendered}" || failed=1
  gpustack::helm::verify_images::field \
    "imagePullPolicy" exact "${pull_policy}" "${rendered}" || failed=1
  gpustack::helm::verify_images::pull_secrets \
    "${pull_secret}" "${rendered}" || failed=1

  return "${failed}"
}

# gpustack::helm::lint lints the given chart with chart-testing in a container.
function gpustack::helm::lint() {
  local target="$1"

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
#
# The namespace is chart-testing's own, generated per release: nothing in the chart names a
# namespace any more. Kueue exits when its managedJobsNamespaceSelector matches the namespace
# it runs in, and that selector used to exclude gpustack-system by name because Helm cannot
# template a subchart's value — so this install used to have to be pinned to that one
# namespace. The parent now renders the selector from .Release.Namespace, and a random
# namespace here is what proves it.
function gpustack::helm::test() {
  local target="$1"

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
    --helm-extra-args '--timeout 600s'
}
