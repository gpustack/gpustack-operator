#!/usr/bin/env bash
#
# Remove the leftovers that `helm uninstall gpustack-operator` does NOT clean up by
# itself:
#   - the releases installed outside the operator chart — the one the worker installs
#     from inside its own image, and the per-application releases earlier versions
#     installed before Kueue / Node Feature Discovery / the CSI drivers became
#     subcharts of the operator chart (a compatibility pass: a chart-mode uninstall
#     has already taken those workloads down with the release);
#   - the CRDs this release installed (gpustack, and Kueue when this release installed it);
#   - the finalizers that pin objects once their controllers are gone
#     (Kueue's `kueue.x-k8s.io/resource-in-use`, the operator's
#     `gpustack.ai/controlled` on Instances AND InstanceTypes);
#   - the aggregated APIServices and admission webhooks the worker registers;
#   - the orphaned cluster-scoped objects a release whose record died with the namespace
#     leaves behind (ClusterRoles/Bindings, CSIDrivers, Kueue's kube-system RoleBinding) —
#     their stale meta.helm.sh/release-name annotations make a later reinstall fail on
#     "invalid ownership metadata";
#   - the objects a failed migration hook leaves behind, including a cluster-admin
#     ClusterRoleBinding.
#
# Used in two places, both reusing this one file as the single source of truth:
#   - the gpustack-operator-e2e / gpustack-operator-chart-e2e skills run it on the
#     host against the active kube context;
#   - the chart's gated post-delete hook Job (cleanupOnUninstall=true) runs it
#     in-cluster with the operator image (which bundles kubectl + helm).
#
# Idempotent and safe to re-run. It NEVER deletes namespaces, and touches only the gpustack CRDs plus
# the Kueue ones THIS release installed — a Kueue the cluster already had is left alone, as are the
# user's own resources. NFD's CRDs are left in place entirely; see owned_crds for why.
set -uo pipefail

NS="${1:-${GPUSTACK_NAMESPACE:-gpustack-system}}"
# The Helm release this install belongs to, used to tell a subchart's Kueue/NFD CRDs from a
# standalone one's. Defaults to the conventional name for the same reason $2 does: the e2e skills run
# this script on a host with no release to ask.
RELEASE="${3:-gpustack-operator}"
echo "[cleanup] namespace=${NS} release=${RELEASE}"

# 0. Preflight. Every sweep below swallows errors by design, so against a wrong or absent
#    context the script would print "done" having deleted nothing — or worse, sweep a cluster
#    the caller never meant to touch (the patterns are broad by assumption). Refuse to run
#    blind, and name the context so a surprise is visible before anything is deleted.
if ! kubectl get --raw=/healthz --request-timeout=10s >/dev/null 2>&1; then
  echo "[cleanup] FATAL: cannot reach the API server of the current kubectl context" \
    "($(kubectl config current-context 2>/dev/null || echo 'none set')) — fix KUBECONFIG/context and re-run" >&2
  exit 1
fi
echo "[cleanup] context=$(kubectl config current-context 2>/dev/null || echo unknown)"

# 1. Uninstall the releases the operator chart does not own. gpustack-operator-device-manager is
#    the release the worker installs from the chart bundled into its own image (image mode, and the
#    device-manager-only install earlier versions did); the four gpustack-<application> names are
#    the pre-subchart per-application releases. Any of them may be absent — the helm status guard
#    skips those — and after a chart-mode uninstall all of them usually are.
if command -v helm >/dev/null 2>&1; then
  for r in gpustack-operator-device-manager gpustack-csi-driver-nfs gpustack-csi-driver-s3 gpustack-node-feature-discovery gpustack-kueue; do
    if helm status "${r}" -n "${NS}" >/dev/null 2>&1; then
      echo "[cleanup] helm uninstall ${r}"
      helm uninstall "${r}" -n "${NS}" --wait --timeout 120s 2>/dev/null \
        || helm uninstall "${r}" -n "${NS}" 2>/dev/null || true
    fi
  done
fi

# 2. Delete the aggregated APIServices and admission webhooks these releases registered, BEFORE
#    stripping finalizers or deleting CRDs. Once the workloads are gone their backing Services are
#    unreachable, so a finalizer-clearing patch (an update) would be rejected by a still-registered
#    validating webhook (failurePolicy: Fail) and hang; and leaving the aggregated
#    worker.gpustack.ai/v1 proxy registered makes an unversioned `kubectl get instancetypes`
#    resolve to it and fail, silently skipping the strip below.
#
#    Name patterns only NOMINATE: an external Kueue/NFD install is a supported configuration
#    (docs/migration/to-subcharts.md) whose objects match the same names, so a candidate is
#    deleted only once the namespace of the Service it points at confirms it belongs to THIS
#    install. That confirmation is also version-blind — Helm's ownership annotations would do
#    for the chart-installed objects, but the worker registers its own at runtime and those
#    carry none.
for r in mutatingwebhookconfigurations validatingwebhookconfigurations; do
  kubectl get "${r}" -o name 2>/dev/null \
    | grep -Ei 'gpustack|kueue|nfd' \
    | while read -r obj; do
        kubectl get "${obj}" -o jsonpath='{.webhooks[*].clientConfig.service.namespace}' 2>/dev/null \
          | grep -qw "${NS}" || continue
        kubectl delete "${obj}" --ignore-not-found 2>/dev/null || true
      done
done
#    An APIService gives itself away by the namespace of the Service it proxies to — the
#    operator's aggregated APIs and Kueue's visibility pair included, whatever version
#    registered them. Plain xargs, no -I{}: the jsonpath emits the names as ONE
#    space-separated line, which -I{} would pass as a single, invalid name.
kubectl get apiservices \
  -o jsonpath='{.items[?(@.spec.service.namespace=="'"${NS}"'")].metadata.name}' 2>/dev/null \
  | xargs -r kubectl delete apiservice --ignore-not-found 2>/dev/null || true

# 3. Strip the finalizers that pin objects once their controllers are gone. Kueue pins
#    workloads/flavors/queues/checks with kueue.x-k8s.io/resource-in-use and the operator pins
#    Instances/InstanceTypes with gpustack.ai/controlled; with the controllers uninstalled these
#    never clear on their own. A CRD delete only completes once every CR of that kind is finalized,
#    so a single strip that races the delete — or is skipped by a transient discovery error while
#    the aggregated APIs drain — leaves a CR Terminating and hangs the CRD delete (most often the
#    ClusterQueue). Strip up front, then (step 4) re-strip while the CRDs drain.
#
#    The kinds are DISCOVERED from the CRDs matching the groups this release owns, not listed: a
#    list has to be edited every time a kind is added, and the one time it is forgotten the symptom
#    is an uninstall that hangs on a CRD nobody thought about.
#
#    Each is addressed as plural.VERSION.group rather than plural.group, and the version is the
#    CRD's own STORAGE version. With the worker gone the aggregated worker.gpustack.ai/v1 proxy is
#    unreachable, so an unversioned name resolves to it and silently returns nothing — which is why
#    the version is read off the CRD and put back into the name. Kueue's and NFD's kinds have no
#    aggregated proxy and are unharmed by the same treatment.
#
#    owned_crds is the one place ownership is decided, and step 4 deletes from it too — so a kind
#    can only be stripped here if it would also be deleted there.
#
#    A GROUP is not ownership. The gpustack groups are ours however they were installed, but Kueue
#    and NFD are SUBCHARTS: the CRDs this release installed carry its Helm ownership, while a Kueue
#    or NFD the cluster already had carries another release's or none at all. Matching the group
#    alone reached both — an uninstall would strip every finalizer in those groups and then delete
#    the CRDs, taking a working external installation down with it.
#
#    BOTH Helm ownership annotations, because either alone is satisfied by somebody else's release:
#    a standalone Kueue installed into this same namespace carries the same release-namespace, and a
#    release of the same name elsewhere carries the same release-name. The pair identifies exactly
#    one release, which is the only thing that authorises deleting a CRD.
#
#    NFD IS DELIBERATELY ABSENT, and its CRDs are left in place. Its subchart ships them under
#    crds/, which Helm installs verbatim and never annotates — so there is no ownership to read and
#    no way to tell this install's from one the cluster already had. Deleting them on that basis
#    would be a guess with a cluster-wide blast radius. Nothing hangs as a result: NFD's CRs carry no
#    finalizer of ours, which is why the enumeration this function replaced never listed them either.
#    An operator removing NFD entirely removes its chart, which is where that decision belongs.
owned_crds() {
  kubectl get crd \
    -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.names.plural}.{.spec.versions[?(@.storage==true)].name}.{.spec.group}{" "}{.metadata.annotations.meta\.helm\.sh/release-namespace}{" "}{.metadata.annotations.meta\.helm\.sh/release-name}{"\n"}{end}' \
    2>/dev/null \
    | while read -r crd res owner_ns owner_name; do
        case "${crd}" in
          *.gpustack.ai) ;;
          *.kueue.x-k8s.io)
            [ "${owner_ns}" = "${NS}" ] && [ "${owner_name}" = "${RELEASE}" ] || continue ;;
          *) continue ;;
        esac
        echo "${crd} ${res}"
      done
}

strip_gpustack_finalizers() {
  owned_crds | while read -r _ res; do
    # -o name drops the namespace, so `kubectl patch` on its output runs against whatever namespace
    # is current and silently misses every namespaced object — which is the hang this function
    # exists to prevent. The namespace is read out alongside the name and passed back explicitly.
    #
    # Separated by "/", which neither a namespace nor a name may contain, and NOT by a space: a
    # cluster-scoped object has an empty namespace, so a space-separated line starts with a blank
    # field that `read` folds away — putting the NAME in the namespace variable and dropping the
    # object. KVCacheBackend is cluster-scoped, so that is the very kind this would have missed.
    kubectl get "${res}" -A \
      -o jsonpath='{range .items[*]}{.metadata.namespace}{"/"}{.metadata.name}{"\n"}{end}' 2>/dev/null \
      | while IFS='/' read -r ns name; do
          [ -n "${name}" ] || continue
          kubectl patch "${res}" "${name}" ${ns:+-n "${ns}"} \
            --type=merge -p '{"metadata":{"finalizers":[]}}' 2>/dev/null || true
        done
    done
}

# 4. Delete the CRDs this release owns (gpustack, and Kueue when ours) and drain them. Kick it off
#    non-blocking, then keep stripping finalizers so any CR the delete just marked Terminating is
#    released and the CRD drains instead of hanging.
strip_gpustack_finalizers
owned_crds | while read -r crd _; do
  kubectl delete crd "${crd}" --ignore-not-found --wait=false 2>/dev/null || true
done
for _ in $(seq 1 20); do
  [ -z "$(owned_crds)" ] && break
  strip_gpustack_finalizers
  sleep 3
done

# 5. Delete the Secrets the worker provisions at runtime — its cert-manager webhook serving cert and
#    the delegated editable-settings store (gpustack-settings, a fixed name set in code). The cert
#    Secret is named "<worker-fullname>-cert", release-dependent via the chart fullname; the post-delete
#    hook passes the resolved name as $2, falling back to the conventional gpustack-operator-worker-cert.
#    Neither Secret is helm-owned (cert-manager and the worker create them), so `helm uninstall` leaves
#    them behind. Kueue's three are on the list for the same reason and carry fixed names, the subchart
#    being pinned to a "kueue" prefix; they exist only where cert-manager issued them, since a
#    self-managing Kueue has Helm own the one Secret and `helm uninstall` already took it.
#    Delete by exact name, never a label sweep, so a co-located standalone GPUStack app's
#    own Secrets are untouched.
worker_cert_secret="${2:-gpustack-operator-worker-cert}"
for s in "${worker_cert_secret}" gpustack-settings \
  kueue-webhook-server-cert kueue-metrics-server-cert kueue-visibility-server-cert; do
  kubectl -n "${NS}" delete secret "${s}" --ignore-not-found 2>/dev/null || true
done

# 6. Delete the Leases the worker holds at runtime — the lock that serializes the application
#    install across replicas, and the control-plane leader election. Both are created by the
#    worker rather than by the chart, so `helm uninstall` leaves them, and this script never
#    deletes the namespace that would take them with it.
for l in applications.worker.gpustack.ai worker.gpustack.ai; do
  kubectl -n "${NS}" delete lease "${l}" --ignore-not-found 2>/dev/null || true
done

# 7. Delete what a FAILED migration hook leaves behind. Helm removes its hook objects once they
#    succeed, but not when they fail — deliberately, so the logs survive — and among them is a
#    ClusterRoleBinding to cluster-admin. Selected by our own label and then by name, so this
#    never touches the cleanup hook's own ServiceAccount and binding, which it is running as.
for r in jobs configmaps serviceaccounts; do
  kubectl -n "${NS}" get "${r}" -l app.kubernetes.io/part-of=gpustack-operator -o name 2>/dev/null \
    | grep -E 'migrate' \
    | xargs -r -I{} kubectl -n "${NS}" delete {} --ignore-not-found 2>/dev/null || true
done
kubectl get clusterrolebindings -l app.kubernetes.io/part-of=gpustack-operator -o name 2>/dev/null \
  | grep -E 'migrate' \
  | xargs -r -I{} kubectl delete {} --ignore-not-found 2>/dev/null || true

# 8. Sweep orphaned cluster-scoped objects whose release record is already gone (it lives in the
#    namespace, so a namespace deletion kills the record but not these): ClusterRoles and
#    ClusterRoleBindings of the runtime releases and their subchart components, the CSI drivers'
#    CSIDriver objects, and the one RoleBinding Kueue's visibility server plants in kube-system.
#    Their stale meta.helm.sh/release-name annotations otherwise fail a later REINSTALL with
#    "invalid ownership metadata", and no adoption re-fires for them (TakeOwnership gates on the
#    legacy release records, which are exactly what is gone). The pattern is wider than step 2's:
#    node-feature-discovery carries no "nfd" substring and the CSI RBAC is named after the driver.
#    The "-cleanup" exclusion keeps the post-delete hook's own ClusterRoleBinding
#    ("<release>-cleanup", cluster-admin) out of the sweep: deleting it mid-run would revoke the
#    permissions every later delete in this script needs, and its lifecycle is the chart's, not
#    this script's.
#    The name pattern only nominates: a co-located standalone Kueue/NFD/CSI install in another
#    namespace matches it too, so a candidate is deleted only when Helm's ownership annotation
#    points its release at THIS namespace — everything the sweep targets is Helm-owned.
orphan_sweep() {
  local kind="$1"
  kubectl get "${kind}" -o name 2>/dev/null \
    | grep -Ei 'gpustack|kueue|nfd|node-feature|csi-nfs|csi-s3' \
    | grep -vE -- '-cleanup$' \
    | while read -r obj; do
        owner_ns="$(kubectl get "${obj}" \
          -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-namespace}' 2>/dev/null)"
        [ "${owner_ns}" = "${NS}" ] || continue
        echo "[cleanup] delete orphaned ${obj}"
        kubectl delete "${obj}" --ignore-not-found 2>/dev/null || true
      done
}
for r in clusterrole clusterrolebinding csidriver; do
  orphan_sweep "${r}"
done
#    Kueue's visibility RoleBinding in kube-system gets the same ownership guard.
if [ "$(kubectl -n kube-system get rolebinding kueue-visibility-server-auth-reader \
      -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-namespace}' 2>/dev/null)" = "${NS}" ]; then
  kubectl -n kube-system delete rolebinding kueue-visibility-server-auth-reader --ignore-not-found 2>/dev/null || true
fi

echo "[cleanup] done"