#!/usr/bin/env bash
#
# Tear down an E2E deployment and remove every leftover `helm uninstall` does not take.
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
# Prefer the client hack/lib/helm.sh pins (3.21+). A PATH helm can be old enough to lack
# flags this suite needs — a 3.13 client has no --take-ownership at all.
HELM=helm
[ -x .sbin/helm ] && HELM=.sbin/helm

echo "[teardown] namespace=${NS}"

# 0. E2E test artifacts this skill creates. Delete the test Instance before the
#    NodeFeatures so its Pod/Workload drain cleanly. Deleting the Worker-authored
#    <node>-gpustack-worker NodeFeature also discards any injected label edit.
kubectl -n default delete instance gpustack-e2e-instance --ignore-not-found 2>/dev/null || true
kubectl -n "$NS" delete nodefeature --all 2>/dev/null || true

# 1. The operator's own release (worker, device-managers, RBAC, webhooks).
if command -v "$HELM" >/dev/null 2>&1 && "$HELM" status gpustack-operator -n "$NS" >/dev/null 2>&1; then
  echo "[teardown] helm uninstall gpustack-operator"
  "$HELM" uninstall gpustack-operator -n "$NS" 2>/dev/null || true
fi

# 2. The releases the operator chart does not own. gpustack-operator-device-manager is the one
#    the worker installs from the chart bundled into its own image (image mode); the four
#    gpustack-<application> names are the per-application releases earlier versions installed
#    before Kueue / NFD / the CSI drivers became subcharts. Any of them may be absent — the
#    status guard skips those — and after a chart-mode uninstall all of them usually are.
if command -v "$HELM" >/dev/null 2>&1; then
  for r in gpustack-operator-device-manager gpustack-csi-driver-nfs gpustack-csi-driver-s3 gpustack-node-feature-discovery gpustack-kueue; do
    if "$HELM" status "$r" -n "$NS" >/dev/null 2>&1; then
      echo "[teardown] helm uninstall ${r}"
      "$HELM" uninstall "$r" -n "$NS" --wait --timeout 120s 2>/dev/null \
        || "$HELM" uninstall "$r" -n "$NS" 2>/dev/null || true
    fi
  done
fi

# 3. Delete the aggregated APIServices and admission webhooks these releases registered, BEFORE
#    stripping finalizers or deleting CRDs. Once the workloads are gone their backing Services are
#    unreachable, so a finalizer-clearing patch (an update) would be rejected by a still-registered
#    validating webhook (failurePolicy: Fail) and hang; and leaving the aggregated
#    worker.gpustack.ai/v1 proxy registered makes an unversioned `kubectl get instancetypes`
#    resolve to it and fail, silently skipping the strip below.
#
#    Matched by name pattern rather than an explicit list, which is what reaches Kueue's
#    *.visibility.kueue.x-k8s.io APIServices and its kueue-* webhook configurations whatever
#    version they carry — they belong to the operator release now, so a partial uninstall
#    strands them exactly like the worker's own.
gpustack_pattern='gpustack|kueue|nfd'
for r in apiservice mutatingwebhookconfigurations validatingwebhookconfigurations; do
  kubectl get "$r" -o name 2>/dev/null \
    | grep -Ei "$gpustack_pattern" \
    | xargs -r -I{} kubectl delete {} --ignore-not-found 2>/dev/null || true
done

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

# 5. Delete the operator's and its applications' CRDs (gpustack, kueue, nfd) and drain them. Kick
#    the delete off non-blocking, then keep stripping finalizers so any CR the delete just marked
#    Terminating is released and the CRD drains instead of hanging.
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
#    leaves them behind, and so are Kueue's three where cert-manager issued them (fixed names, the
#    subchart being pinned to a "kueue" prefix). Delete by exact name, never a label sweep, so a
#    co-located standalone GPUStack app's own Secrets are untouched.
worker_cert_secret="${2:-gpustack-operator-worker-cert}"
for s in "$worker_cert_secret" gpustack-settings \
  kueue-webhook-server-cert kueue-metrics-server-cert kueue-visibility-server-cert; do
  kubectl -n "$NS" delete secret "$s" --ignore-not-found 2>/dev/null || true
done

# 7. Delete the Leases the worker holds at runtime — the lock that serializes the application
#    install across replicas, and the control-plane leader election. Both are created by the worker
#    rather than by the chart, so `helm uninstall` leaves them, and a released lock is an object
#    that outlives the release it guarded. Mirrors the same step in the chart's cleanup.sh.
for l in applications.worker.gpustack.ai worker.gpustack.ai; do
  kubectl -n "$NS" delete lease "$l" --ignore-not-found 2>/dev/null || true
done

# 8. Delete what a FAILED migration hook leaves behind. Helm removes its hook objects once they
#    succeed, but not when they fail — deliberately, so the logs survive — and among them is a
#    ClusterRoleBinding to cluster-admin. Selected by our own label and then by name.
for r in jobs configmaps serviceaccounts; do
  kubectl -n "$NS" get "$r" -l app.kubernetes.io/part-of=gpustack-operator -o name 2>/dev/null \
    | grep -E 'migrate' \
    | xargs -r -I{} kubectl -n "$NS" delete {} --ignore-not-found 2>/dev/null || true
done
kubectl get clusterrolebindings -l app.kubernetes.io/part-of=gpustack-operator -o name 2>/dev/null \
  | grep -E 'migrate' \
  | xargs -r -I{} kubectl delete {} --ignore-not-found 2>/dev/null || true

# 9. E2E-ONLY (NOT part of the cleanup.sh mirror): reverse-patch the Node extended resources GPUStack
#    advertised, so the NEXT case sees a pristine node. Node extended resources do not self-remove when
#    their advertiser is gone, and the two families clear differently — the removal below is genuinely
#    needed for one and only cosmetic for the other:
#      - reconciler-owned counting keys ("<vendor>.com/gpu.sliced.units|.cores-percentage|
#        .memory-percentage|.memory-mib", "<vendor>.com/gpu.partitioned.units",
#        "<vendor>.com/gpu.partitioned.<kind>-<profile>") are written by the NodeCapacityReconciler.
#        Nothing removes them once the worker is uninstalled, so this JSON patch is the ONLY thing that
#        clears them;
#      - device-plugin-owned pool keys ("<vendor>.com/gpu.shared", "<vendor>.com/gpu.sliced",
#        "<vendor>.com/gpu.partitioned", "device.gpustack.ai/<vendor>.visibility") only ZERO OUT when the
#        plugin exits. The patch removes the entry, but the kubelet re-adds a zero-valued one on its next
#        status sync — full removal needs a kubelet restart, which this script deliberately does not do.
#    So the sweep leaves the node clean for the next case, NOT provably key-free.
#    Sweep the GPUStack-OWNED keys from status.capacity+allocatable on every node: device.gpustack.ai/*,
#    any "/gpu.sliced" key, any "/gpu.partitioned" key, and "*/gpu.shared". The "/gpu.sliced" prefix match
#    also catches the pre-split per-profile MIG shape — a "mig-<profile>" segment appended to the
#    LOGICAL family's key, which the split replaced and which no component owns any more, so a
#    development node an earlier build wrote it onto keeps it otherwise. The bare
#    whole-card "<vendor>.com/gpu" is deliberately LEFT ALONE — it is name-identical to a real vendor
#    device-plugin's resource, so removing it generically is unsafe; it zeroes out on the GPUStack
#    plugin's exit. Requires python3 (already a hard dependency of the case scripts) and a kubectl new
#    enough for --subresource.
for node in $(kubectl get nodes -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
  patch=$(kubectl get node "$node" -o json 2>/dev/null | python3 -c '
import json, sys
o = json.load(sys.stdin); ops = []
esc = lambda k: k.replace("~", "~0").replace("/", "~1")
owned = lambda k: (
    k.startswith("device.gpustack.ai/")
    or "/gpu.sliced" in k
    or "/gpu.partitioned" in k
    or k.endswith("/gpu.shared")
)
for sect in ("capacity", "allocatable"):
    for k in o.get("status", {}).get(sect, {}):
        if owned(k):
            ops.append({"op": "remove", "path": "/status/%s/%s" % (sect, esc(k))})
print(json.dumps(ops))
' 2>/dev/null)
  if [ -n "$patch" ] && [ "$patch" != "[]" ]; then
    echo "[teardown] reverse-patching gpustack extended resources on node ${node}"
    kubectl patch node "$node" --subresource=status --type=json -p "$patch" >/dev/null 2>&1 || true
  fi
done

echo "[teardown] done (namespace ${NS} kept on purpose)"
