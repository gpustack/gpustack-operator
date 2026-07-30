#!/usr/bin/env bash
#
# Pre-install / pre-upgrade hook: the two things Helm's own ownership transfer
# (`helm upgrade --take-ownership`) does not cover. Both steps are idempotent and both
# are no-ops on a healthy cluster.
#
#   1. Reap a stranded Kueue. A Kueue controller torn down while its custom resources
#      still carry the kueue.x-k8s.io/resource-in-use finalizer leaves its CRDs
#      Terminating forever, and then every (re)install of this chart fails on them.
#      Ordering is load-bearing: Kueue's validating webhook (failurePolicy: Fail)
#      rejects the finalizer-clearing patch once its Service has no endpoints, so the
#      webhook configurations are deleted first.
#
#   2. Server-side-apply the vendored subcharts' crds/. Helm applies crds/ on install
#      only — an upgrade never touches them — so without this a Node Feature Discovery
#      enabled by an upgrade runs with no CRDs at all, and NFD's CRD schema changes
#      never land. Skipped on install, where Helm has just applied them itself.
#
#   3. Delete the workloads a legacy per-application release owns whose spec.selector
#      still names that release. Adoption rewrites app.kubernetes.io/instance, and on a
#      Deployment, DaemonSet or StatefulSet that label sits inside spec.selector, which
#      the API server refuses to change. Skipped on install, where nothing is adopted.
#
# Every input arrives as an environment variable, all set by the hook Job:
#   GPUSTACK_NAMESPACE      release namespace
#   GPUSTACK_RELEASE        release name, which is how this operator's Kueue is told
#                           apart from one the user brought themselves
#   GPUSTACK_PHASE          "install" or "upgrade"
#   GPUSTACK_CHART_VERSION  version of the chart being installed
#
# The packaged chart is read from ${GPUSTACK_CONF_DIR}/charts, the same location the worker
# installs its own bundled chart from.
set -uo pipefail

NS="${GPUSTACK_NAMESPACE:-gpustack-system}"
RELEASE="${GPUSTACK_RELEASE:-gpustack-operator}"
PHASE="${GPUSTACK_PHASE:-upgrade}"
CHART_VERSION="${GPUSTACK_CHART_VERSION:-}"
CHARTS_DIR="${GPUSTACK_CONF_DIR:-/etc/gpustack}/charts"

# The Helm releases that may own a Kueue this operator installed: the chart release itself,
# the release the worker installs from inside the operator image, and the standalone Kueue
# release earlier versions installed. A Kueue belonging to any other release is a user's own
# and is never touched.
KUEUE_RELEASES="${RELEASE},gpustack-operator-device-manager,gpustack-kueue"

# The releases the worker installed per application before the subchart layout, which are the
# only ones this chart ever adopts from. Kept in step with migrate-post.sh, which retires their
# records once the upgrade has succeeded.
LEGACY_RELEASES=(
  gpustack-kueue
  gpustack-node-feature-discovery
  gpustack-csi-driver-nfs
  gpustack-csi-driver-s3
)

# One set-based selector covers all four at once. `managed-by=Helm` is required as well, so an
# object a user labelled by hand is never deleted.
LEGACY_SELECTOR="app.kubernetes.io/instance in ($(
  IFS=,
  echo "${LEGACY_RELEASES[*]}"
)),app.kubernetes.io/managed-by=Helm"

# The kinds whose spec.selector is immutable, and therefore the only ones that can collide.
SELECTOR_KINDS=(deployments daemonsets statefulsets)

# How long to wait for the freed CRDs to finish deleting.
DRAIN_TIMEOUT=90
DRAIN_INTERVAL=3

log() { echo "[migrate-pre] $*"; }
die() {
  echo "[migrate-pre] FATAL: $*" >&2
  exit 1
}

log "namespace=${NS} release=${RELEASE} phase=${PHASE}"

# --- 1. Reap a stranded Kueue -------------------------------------------------------------

# stuck_kueue_crds prints "<name> <plural> <storageVersion>" for every Terminating
# kueue.x-k8s.io CRD. The storage version is the one to list and patch: a served
# non-storage version is materialized through the CRD's conversion webhook, which is
# exactly what is unreachable in this state.
stuck_kueue_crds() {
  kubectl get crd -o json |
    jq -r '.items[]
           | select(.spec.group == "kueue.x-k8s.io")
           | select(.metadata.deletionTimestamp != null)
           | [ .metadata.name,
               .spec.names.plural,
               (([.spec.versions[] | select(.storage == true) | .name] | first) // .spec.versions[0].name)
             ] | @tsv'
}

# strip_finalizers clears the finalizers of every Terminating custom resource of one
# resource. Live resources are left alone, so a healthy queue keeps its accounting
# finalizer.
# The name comes first and the namespace second, because the namespace is what may be empty:
# a leading tab is IFS whitespace to `read`, which would collapse it and shift the name into
# the namespace, while a trailing empty field simply reads as empty.
strip_finalizers() {
  local resource="$1"
  local namespace name scope=()

  while IFS=$'\t' read -r name namespace; do
    [[ -n "${name}" ]] || continue
    # An empty namespace addresses a cluster-scoped resource (e.g. ClusterQueue).
    scope=()
    [[ -z "${namespace}" ]] || scope=(-n "${namespace}")
    log "clearing finalizers on ${resource} ${namespace:-<cluster>}/${name}"
    kubectl patch "${resource}" "${scope[@]}" "${name}" \
      --type=merge -p '{"metadata":{"finalizers":[]}}' >/dev/null ||
      log "WARNING: could not clear the finalizers of ${resource} ${name}"
  done < <(
    kubectl get "${resource}" -A -o json |
      jq -r '.items[]
             | select(.metadata.deletionTimestamp != null)
             | select(((.metadata.finalizers // []) | length) > 0)
             | [ .metadata.name, (.metadata.namespace // "") ] | @tsv'
  )
}

# wait_crds_deleted blocks until none of the named CRDs exists any more. It asks for one
# CRD at a time so the poll stays a table lookup instead of re-downloading every CRD
# schema in the cluster, which for Kueue's is megabytes a round.
wait_crds_deleted() {
  local waited=0 name remaining

  while :; do
    remaining=0
    for name in "$@"; do
      kubectl get crd "${name}" >/dev/null 2>&1 && remaining=$((remaining + 1))
    done
    [[ ${remaining} -eq 0 ]] && return 0

    [[ ${waited} -lt ${DRAIN_TIMEOUT} ]] ||
      die "${remaining} Kueue CRD(s) are still terminating after ${waited}s"
    sleep "${DRAIN_INTERVAL}"
    waited=$((waited + DRAIN_INTERVAL))
  done
}

reap_stranded_kueue() {
  local stuck names=() name plural version

  stuck="$(stuck_kueue_crds)" || die "list the Kueue CRDs"
  if [[ -z "${stuck}" ]]; then
    log "no Kueue CRD is terminating, nothing to reap"
    return 0
  fi

  # The webhook configurations first — see the ordering note in this file's header.
  log "deleting the Kueue webhook configurations of releases: ${KUEUE_RELEASES}"
  kubectl delete validatingwebhookconfigurations,mutatingwebhookconfigurations \
    -l "app.kubernetes.io/instance in (${KUEUE_RELEASES})" --ignore-not-found ||
    die "delete the Kueue webhook configurations"

  while IFS=$'\t' read -r name plural version; do
    [[ -n "${name}" ]] || continue
    names+=("${name}")
    strip_finalizers "${plural}.${version}.kueue.x-k8s.io"
  done <<<"${stuck}"

  log "waiting for ${#names[@]} freed Kueue CRD(s) to drain"
  wait_crds_deleted "${names[@]}"
  log "the stranded Kueue is reaped"
}

reap_stranded_kueue

# --- 2. Apply the vendored subcharts' CRDs ------------------------------------------------

apply_subchart_crds() {
  local chart="${CHARTS_DIR}/gpustack-operator-${CHART_VERSION}.tgz"
  local dir crds=()

  if [[ ! -f "${chart}" ]]; then
    # Fall back to whatever packaged chart the image carries: an overridden hook image
    # need not be the version being installed.
    chart="$(find "${CHARTS_DIR}" -maxdepth 1 -name 'gpustack-operator-*.tgz' 2>/dev/null | sort | tail -1)"
  fi
  [[ -n "${chart}" && -f "${chart}" ]] ||
    die "no packaged gpustack-operator chart under ${CHARTS_DIR}; the hook image must be an operator image"

  dir="$(mktemp -d)" || die "create a temporary directory"
  # shellcheck disable=SC2064
  trap "rm -rf '${dir}'" EXIT
  tar -xzf "${chart}" -C "${dir}" || die "unpack ${chart}"

  shopt -s nullglob
  crds=("${dir}"/*/charts/*/crds/*.yaml "${dir}"/*/charts/*/crds/*.yml)
  shopt -u nullglob
  if [[ ${#crds[@]} -eq 0 ]]; then
    log "no subchart ships a crds/ directory, nothing to apply"
    return 0
  fi

  local args=() file
  for file in "${crds[@]}"; do
    args+=(-f "${file}")
  done

  log "applying ${#crds[@]} subchart CRD file(s) from $(basename "${chart}")"
  # Server-side, forcing conflicts: these CRDs were last written by Helm's client-side
  # apply, which owns their fields until this hand-over.
  kubectl apply --server-side --force-conflicts "${args[@]}" ||
    die "apply the subchart CRDs"
}

# --- 3. Free the workloads whose selector adoption cannot rewrite -------------------------

# free_immutable_selectors deletes the workloads a legacy release owns whose spec.selector still
# names that release.
#
# Adoption rewrites app.kubernetes.io/instance to the adopting release, and Kueue's and NFD's
# charts put that label inside spec.selector, which the API server refuses to change. Helm reports
# it as `field is immutable`; the upgrade fails, and because it is atomic the rollback then
# deletes every object the previous revision did not carry — the applications themselves, their
# webhook configurations, their APIServices. Deleting these few here instead lets the SAME upgrade
# create them, in one action, and leaves everything else to be adopted in place.
#
# Only the workloads whose selector actually names a legacy release are touched: the CSI drivers
# select on a release-independent label, so theirs are adopted untouched. No custom resource and
# no CRD is touched either, so Kueue's finalizers stay on their objects and the controller this
# upgrade recreates is what clears them. These workloads are down from here until the upgrade
# reports them Ready, which is a shorter gap than the failed-upgrade-and-restart it replaces.
free_immutable_selectors() {
  local kind names

  for kind in "${SELECTOR_KINDS[@]}"; do
    names="$(
      kubectl get "${kind}" -n "${NS}" -l "${LEGACY_SELECTOR}" -o json |
        jq -r --arg adopting "${RELEASE}" '
          .items[]
          | select((.spec.selector.matchLabels["app.kubernetes.io/instance"] // "")
                   | . != "" and . != $adopting)
          | .metadata.name'
    )" || die "list the ${kind} of the legacy releases"
    [[ -n "${names}" ]] || continue

    log "deleting ${kind} whose selector adoption cannot rewrite: ${names//$'\n'/ }"
    # shellcheck disable=SC2086 # deliberate word splitting: one name per line, none can contain spaces
    kubectl delete "${kind}" -n "${NS}" ${names} --ignore-not-found ||
      die "delete the ${kind} a legacy release still selects"
  done
}

if [[ "${PHASE}" == "upgrade" ]]; then
  apply_subchart_crds
  free_immutable_selectors
else
  log "install: Helm applies the subchart CRDs itself and nothing is adopted, skipping"
fi

log "done"
