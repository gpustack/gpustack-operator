#!/usr/bin/env bash
#
# Remove the cluster-scoped, runtime-installed leftovers that `helm uninstall
# gpustack-operator` does NOT clean up by itself:
#   - the Helm releases the worker installs at runtime (Kueue, Node Feature
#     Discovery, the CSI drivers) — each a separate release;
#   - the CRDs those releases and the worker install (kueue / nfd / gpustack);
#   - the finalizers that pin objects once their controllers are gone
#     (Kueue's `kueue.x-k8s.io/resource-in-use`, the operator's
#     `gpustack.ai/controlled` on Instances AND InstanceTypes);
#   - the aggregated APIServices and admission webhooks the worker registers.
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

# 1. Uninstall the Helm releases the worker installed at runtime. This includes the device-manager,
#    which the worker installs as its own release (gpustack-operator-device-manager) from the bundled
#    operator chart when deviceManager.enabled=false; it is absent when the chart rendered the
#    device-manager directly, and the helm status guard simply skips it then.
if command -v helm >/dev/null 2>&1; then
  for r in gpustack-operator-device-manager gpustack-csi-driver-nfs gpustack-csi-driver-s3 gpustack-node-feature-discovery gpustack-kueue; do
    if helm status "${r}" -n "${NS}" >/dev/null 2>&1; then
      echo "[cleanup] helm uninstall ${r}"
      helm uninstall "${r}" -n "${NS}" --wait --timeout 120s 2>/dev/null \
        || helm uninstall "${r}" -n "${NS}" 2>/dev/null || true
    fi
  done
fi

# 2. Delete the aggregated APIServices and admission webhooks the worker registered at runtime,
#    BEFORE stripping finalizers or deleting CRDs. Once the worker is gone their backing Service
#    is unreachable, so a finalizer-clearing patch (an update) would be rejected by the still-
#    registered validating webhook (failurePolicy: Fail) and hang; and leaving the aggregated
#    worker.gpustack.ai/v1 proxy registered makes an unversioned `kubectl get instancetypes`
#    resolve to it and fail, silently skipping the strip below.
for a in v1.gpustack.ai v1.worker.gpustack.ai; do
  kubectl delete apiservice "${a}" --ignore-not-found 2>/dev/null || true
done
kubectl get mutatingwebhookconfigurations,validatingwebhookconfigurations -o name 2>/dev/null \
  | grep -i gpustack \
  | xargs -r -I{} kubectl delete {} --ignore-not-found 2>/dev/null || true

# 3. Strip finalizers that block deletion once the controllers above are gone. Kueue pins
#    workloads/flavors/queues AND admission checks with kueue.x-k8s.io/resource-in-use; the
#    node-devices AdmissionCheck would otherwise hang the admissionchecks CRD delete below.
for k in workloads resourceflavors clusterqueues admissionchecks; do
  kubectl get "${k}.kueue.x-k8s.io" -A -o name 2>/dev/null \
    | xargs -r -I{} kubectl patch {} --type=merge -p '{"metadata":{"finalizers":[]}}' 2>/dev/null || true
done
# The operator's own CRs carry gpustack.ai/controlled: Instances AND InstanceTypes. An
# InstanceType also drives its backing ClusterQueue through HoldAndDrain, so without the operator
# running its finalizer never clears and the CR hangs Terminating — strip it so the CRD delete
# below does not block. Target the real v1alpha1 CRD explicitly: with the worker gone the
# aggregated worker.gpustack.ai/v1 proxy is unreachable, so an unversioned get resolves to it and
# silently returns nothing. (Cohorts are no longer created — nothing to strip.)
for k in instances instancetypes; do
  kubectl get "${k}.v1alpha1.worker.gpustack.ai" -A -o name 2>/dev/null \
    | xargs -r -I{} kubectl patch {} --type=merge -p '{"metadata":{"finalizers":[]}}' 2>/dev/null || true
done

# 4. Delete the CRDs the worker / sub-releases installed (gpustack, kueue, nfd).
#    Their CRs are cascade-deleted; the finalizer strip above keeps that from hanging.
kubectl get crd -o name 2>/dev/null \
  | grep -E '\.(worker\.)?gpustack\.ai$|\.kueue\.x-k8s\.io$|\.nfd\.k8s-sigs\.io$' \
  | xargs -r -I{} kubectl delete {} --ignore-not-found 2>/dev/null || true

echo "[cleanup] done"