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

# 3. Strip finalizers that block deletion once the controllers above are gone.
for k in workloads resourceflavors clusterqueues cohorts; do
  kubectl get "${k}.kueue.x-k8s.io" -A -o name 2>/dev/null \
    | xargs -r -I{} kubectl patch {} --type=merge -p '{"metadata":{"finalizers":[]}}' 2>/dev/null || true
done
kubectl get instances.worker.gpustack.ai -A -o name 2>/dev/null \
  | xargs -r -I{} kubectl patch {} --type=merge -p '{"metadata":{"finalizers":[]}}' 2>/dev/null || true

# 4. Delete the CRDs the worker / sub-releases installed (gpustack, kueue, nfd).
#    Their CRs cascade-delete; the finalizer strip above keeps that from hanging.
kubectl get crd -o name 2>/dev/null \
  | grep -E '\.(worker\.)?gpustack\.ai$|\.kueue\.x-k8s\.io$|\.nfd\.k8s-sigs\.io$' \
  | xargs -r -I{} kubectl delete {} --ignore-not-found 2>/dev/null || true

# 5. Delete the aggregated APIServices and webhooks the worker registered at runtime.
for a in v1.gpustack.ai v1.worker.gpustack.ai; do
  kubectl delete apiservice "$a" --ignore-not-found 2>/dev/null || true
done
kubectl get mutatingwebhookconfigurations,validatingwebhookconfigurations -o name 2>/dev/null \
  | grep -i gpustack \
  | xargs -r -I{} kubectl delete {} --ignore-not-found 2>/dev/null || true

echo "[teardown] done (namespace ${NS} kept on purpose)"
