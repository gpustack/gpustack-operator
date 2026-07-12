#!/usr/bin/env bash
#
# Tear down an E2E deployment and remove every runtime-installed leftover.
# MUTATING — the skill confirms before running this.
#
#   teardown.sh <NS>
#
# SELF-CONTAINED ON PURPOSE: the cleanup logic below mirrors
# deploy/gpustack-operator/chart/files/cleanup.sh (the chart's post-delete hook
# source) but is duplicated here so the skill does not depend on the deploy/
# tree. If the cleanup contract changes, update BOTH this file and that one.
#
# Idempotent and safe to re-run. NEVER deletes namespaces, and only touches
# gpustack / kueue / nfd owned objects — never the user's own resources. The
# gpustack-system namespace is intentionally KEPT (deleting it can hang in
# Terminating on the orphaned aggregated APIServices).
set -uo pipefail

NS="${1:?usage: teardown.sh <NS>}"
echo "[teardown] namespace=${NS}"

# 0. E2E test artifacts this skill creates. Delete the test Instance before the
#    NodeFeatures so its Pod/Workload drain cleanly. Deleting the Worker-authored
#    <node>-gpustack-worker NodeFeature also discards any injected label edit.
kubectl -n default delete instance gpustack-e2e-instance --ignore-not-found 2>/dev/null || true
kubectl -n "$NS" delete nodefeature --all 2>/dev/null || true

# 1. The operator's own release (worker, device-managers, RBAC, webhooks).
if command -v helm >/dev/null 2>&1 && helm status gpustack-operator -n "$NS" >/dev/null 2>&1; then
  echo "[teardown] helm uninstall gpustack-operator"
  helm uninstall gpustack-operator -n "$NS" 2>/dev/null || true
fi

# 2. The Helm releases the worker installed at runtime. gpustack-operator-device-manager
#    exists only when installed with deviceManager.enabled=false; the status guard skips
#    it otherwise.
if command -v helm >/dev/null 2>&1; then
  for r in gpustack-operator-device-manager gpustack-csi-driver-nfs gpustack-csi-driver-s3 gpustack-node-feature-discovery gpustack-kueue; do
    if helm status "$r" -n "$NS" >/dev/null 2>&1; then
      echo "[teardown] helm uninstall ${r}"
      helm uninstall "$r" -n "$NS" --wait --timeout 120s 2>/dev/null \
        || helm uninstall "$r" -n "$NS" 2>/dev/null || true
    fi
  done
fi

# 3. Delete the aggregated APIServices and admission webhooks the worker registered at runtime,
#    BEFORE stripping finalizers or deleting CRDs. Once the worker is gone their backing Service
#    is unreachable, so a finalizer-clearing patch (an update) would be rejected by the still-
#    registered validating webhook (failurePolicy: Fail) and hang; and leaving the aggregated
#    worker.gpustack.ai/v1 proxy registered makes an unversioned `kubectl get instancetypes`
#    resolve to it and fail, silently skipping the strip below.
for a in v1.gpustack.ai v1.worker.gpustack.ai; do
  kubectl delete apiservice "$a" --ignore-not-found 2>/dev/null || true
done
kubectl get mutatingwebhookconfigurations,validatingwebhookconfigurations -o name 2>/dev/null \
  | grep -i gpustack \
  | xargs -r -I{} kubectl delete {} --ignore-not-found 2>/dev/null || true

# 4. Strip the finalizers that pin objects once their controllers are gone. Kueue pins
#    workloads/flavors/queues/checks with kueue.x-k8s.io/resource-in-use and the operator pins
#    Instances/InstanceTypes with gpustack.ai/controlled; with the controllers uninstalled these
#    never clear on their own. A CRD delete only completes once every CR of that kind is finalized,
#    so a single strip that races the delete — or is skipped by a transient discovery error while
#    the aggregated APIs drain — leaves a CR Terminating and hangs the CRD delete (most often the
#    ClusterQueue). Strip up front, then (step 5) re-strip while the CRDs drain.
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

# 5. Delete the worker / sub-release CRDs (gpustack, kueue, nfd) and drain them. Kick the delete off
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

# 6. Delete the Secrets the worker provisions at runtime — its cert-manager webhook serving cert and
#    the delegated editable-settings store (gpustack-settings, a fixed name set in code). The cert
#    Secret is named "<worker-fullname>-cert", release-dependent via the chart fullname; accept the
#    resolved name as $2 (mirroring cleanup.sh), defaulting to gpustack-operator-worker-cert as this
#    harness always installs under that release. Neither Secret is helm-owned, so `helm uninstall`
#    leaves them behind. Delete by exact name, never a label sweep, so a co-located standalone GPUStack
#    app's own Secrets are untouched.
worker_cert_secret="${2:-gpustack-operator-worker-cert}"
for s in "$worker_cert_secret" gpustack-settings; do
  kubectl -n "$NS" delete secret "$s" --ignore-not-found 2>/dev/null || true
done

echo "[teardown] done (namespace ${NS} kept on purpose)"
