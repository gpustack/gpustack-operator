#!/usr/bin/env bash
#
# Remove the leftovers that `helm uninstall gpustack-operator` does NOT clean up by
# itself:
#   - the releases installed outside the operator chart — the one the worker installs
#     from inside its own image, and the per-application releases earlier versions
#     installed before Kueue / Node Feature Discovery / the CSI drivers became
#     subcharts of the operator chart (a compatibility pass: a chart-mode uninstall
#     has already taken those workloads down with the release);
#   - the CRDs those releases and the worker install (kueue / nfd / gpustack);
#   - the finalizers that pin objects once their controllers are gone
#     (Kueue's `kueue.x-k8s.io/resource-in-use`, the operator's
#     `gpustack.ai/controlled` on Instances AND InstanceTypes);
#   - the aggregated APIServices and admission webhooks the worker registers;
#   - the objects a failed migration hook leaves behind, including a cluster-admin
#     ClusterRoleBinding.
#
# Used in two places, both reusing this one file as the single source of truth:
#   - the gpustack-operator-e2e / gpustack-operator-chart-e2e skills run it on the
#     host against the active kube context;
#   - the chart's gated post-delete hook Job (cleanupOnUninstall=true) runs it
#     in-cluster with the operator image (which bundles kubectl + helm).
#
# Idempotent and safe to re-run. It NEVER deletes namespaces, and only touches
# gpustack / kueue / nfd owned objects — never the user's own resources.
set -uo pipefail

NS="${1:-${GPUSTACK_NAMESPACE:-gpustack-system}}"
echo "[cleanup] namespace=${NS}"

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
#    Matched by name pattern rather than an explicit list, which is what reaches Kueue's
#    *.visibility.kueue.x-k8s.io APIService and its kueue-* webhook configurations whatever
#    version they carry. Kueue's and NFD's objects are in scope for the same reason their CRDs are
#    below: this script assumes the release it follows exclusively owned them.
gpustack_pattern='gpustack|kueue|nfd'
for r in apiservice mutatingwebhookconfigurations validatingwebhookconfigurations; do
  kubectl get "${r}" -o name 2>/dev/null \
    | grep -Ei "${gpustack_pattern}" \
    | xargs -r -I{} kubectl delete {} --ignore-not-found 2>/dev/null || true
done

# 3. Strip the finalizers that pin objects once their controllers are gone. Kueue pins
#    workloads/flavors/queues/checks with kueue.x-k8s.io/resource-in-use and the operator pins
#    Instances/InstanceTypes with gpustack.ai/controlled; with the controllers uninstalled these
#    never clear on their own. A CRD delete only completes once every CR of that kind is finalized,
#    so a single strip that races the delete — or is skipped by a transient discovery error while
#    the aggregated APIs drain — leaves a CR Terminating and hangs the CRD delete (most often the
#    ClusterQueue). Strip up front, then (step 4) re-strip while the CRDs drain.
#
#    Instances/InstanceTypes are stripped on the real v1alpha1 CRD explicitly: with the worker gone
#    the aggregated worker.gpustack.ai/v1 proxy is unreachable, so an unversioned get resolves to it
#    and silently returns nothing. (Cohorts are no longer created — nothing to strip.)
strip_gpustack_finalizers() {
  for res in \
    workloads.kueue.x-k8s.io resourceflavors.kueue.x-k8s.io \
    clusterqueues.kueue.x-k8s.io admissionchecks.kueue.x-k8s.io \
    instances.v1alpha1.worker.gpustack.ai instancetypes.v1alpha1.worker.gpustack.ai; do
    kubectl get "${res}" -A -o name 2>/dev/null \
      | xargs -r -I{} kubectl patch {} --type=merge -p '{"metadata":{"finalizers":[]}}' 2>/dev/null || true
  done
}

# 4. Delete the worker / sub-release CRDs (gpustack, kueue, nfd) and drain them. Kick the delete off
#    non-blocking, then keep stripping finalizers so any CR the delete just marked Terminating is
#    released and the CRD drains instead of hanging.
crd_pattern='\.(worker\.)?gpustack\.ai$|\.kueue\.x-k8s\.io$|\.nfd\.k8s-sigs\.io$'
strip_gpustack_finalizers
kubectl get crd -o name 2>/dev/null | grep -E "${crd_pattern}" \
  | xargs -r kubectl delete --ignore-not-found --wait=false 2>/dev/null || true
for _ in $(seq 1 20); do
  [ "$(kubectl get crd -o name 2>/dev/null | grep -Ec "${crd_pattern}")" = "0" ] && break
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

echo "[cleanup] done"