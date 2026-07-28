#!/usr/bin/env bash
#
# Post-upgrade hook: retire the per-application releases this chart's subcharts replaced.
# It runs only after the upgrade (and therefore the ownership transfer) succeeded, and only
# ever touches objects those releases labelled as their own.
#
#   1. Delete their release records — never `helm uninstall`, which would delete the very
#      objects the parent release just adopted. Left in place, `helm list` keeps reporting
#      releases that point at the parent's objects, and a later `helm uninstall
#      gpustack-kueue` would destroy them.
#
#   2. Prune what the legacy releases created and the new render does not contain. Helm's
#      adoption rewrites the labels of every object the new render resolves, so an object
#      still carrying `app.kubernetes.io/instance=<legacy release>` afterwards is one the
#      new render never mentions: unowned from here on, and invisible to `helm uninstall`.
#      CRDs are deliberately excluded — deleting one takes every custom resource with it —
#      and namespaced kinds are swept inside the release namespace only, so an object of
#      the same name in another namespace is never touched.
#
# Idempotent: after the first migration nothing carries a legacy instance label, and every
# step becomes a no-op.
#
# Every input arrives as an environment variable, all set by the hook Job:
#   GPUSTACK_NAMESPACE  release namespace
set -uo pipefail

NS="${GPUSTACK_NAMESPACE:-gpustack-system}"

# The releases the worker installed per application before the subchart layout.
LEGACY_RELEASES=(
  gpustack-kueue
  gpustack-node-feature-discovery
  gpustack-csi-driver-nfs
  gpustack-csi-driver-s3
)

# The kinds those releases created, swept one at a time: a comma-joined `kubectl get a,b,c`
# fails as a whole when any one kind is unknown to the cluster, which would silently skip the
# rest. Every kind here is built into Kubernetes for the same reason. CRDs are absent on purpose
# (see the header), as are PersistentVolumes and PersistentVolumeClaims, which carry user data and
# were never theirs.
NAMESPACED_KINDS=(
  deployments daemonsets statefulsets services serviceaccounts configmaps secrets
  roles rolebindings poddisruptionbudgets jobs networkpolicies
)
CLUSTER_KINDS=(
  clusterroles clusterrolebindings mutatingwebhookconfigurations
  validatingwebhookconfigurations csidrivers storageclasses apiservices
)

# One set-based selector covers all four releases at once, so each kind is asked for exactly
# once. `managed-by=Helm` is required as well, so an object a user labelled by hand is never
# swept up.
PRUNE_SELECTOR="app.kubernetes.io/instance in ($(
  IFS=,
  echo "${LEGACY_RELEASES[*]}"
)),app.kubernetes.io/managed-by=Helm"

log() { echo "[migrate-post] $*"; }

log "namespace=${NS}"

# --- 1. Retire the legacy release records -------------------------------------------------

for release in "${LEGACY_RELEASES[@]}"; do
  records="$(kubectl -n "${NS}" get secret -l "owner=helm,name=${release}" -o name 2>/dev/null)"
  [[ -n "${records}" ]] || continue
  log "retiring the release record of ${release}"
  kubectl -n "${NS}" delete secret -l "owner=helm,name=${release}" --ignore-not-found ||
    log "WARNING: could not delete the release record of ${release}"
done

# --- 2. Prune the objects the new render does not contain ---------------------------------

# prune deletes the objects of one kind that still carry a legacy release's instance label. Any
# further arguments scope the lookup (`-n <namespace>` for a namespaced kind).
prune() {
  local kind="$1"
  shift
  local found

  if ! found="$(kubectl get "${kind}" "$@" -l "${PRUNE_SELECTOR}" -o name --ignore-not-found 2>&1)"; then
    log "WARNING: could not list ${kind}: ${found}"
    return 0
  fi
  [[ -n "${found}" ]] || return 0

  log "pruning $(echo "${found}" | wc -l | tr -d ' ') orphaned ${kind}:"
  echo "${found}" | sed 's/^/  /'
  kubectl delete "${kind}" "$@" -l "${PRUNE_SELECTOR}" --ignore-not-found ||
    log "WARNING: could not prune every orphaned ${kind}"
}

for kind in "${NAMESPACED_KINDS[@]}"; do
  prune "${kind}" -n "${NS}"
done
for kind in "${CLUSTER_KINDS[@]}"; do
  prune "${kind}"
done

log "done"
