# Migration Troubleshooting

Current chart-mode releases use `Recreate` on the worker `Deployment` (`worker.strategyType`), repair
interrupted Helm pending records automatically, and remove system-namespace `APIService`s during
namespace deletion. Image mode still renders `RollingUpdate` on the worker (#128), so the procedures
below apply to image-mode upgrades, older chart releases, and full cluster resets.

## Contents

- [Worker CrashLoopBackOff](#worker-crashloopbackoff)
- [Namespace stuck Terminating](#namespace-stuck-terminating)
- [Kueue CRDs stuck Terminating](#kueue-crds-stuck-terminating)
- [NFD prune Job hangs](#nfd-prune-job-hangs)
- [Full-reset order](#full-reset-order)

## Worker CrashLoopBackOff

**Symptom**: after an in-place upgrade, the new worker `Pod` crash-loops while the `Deployment` still
reports Ready on the old `ReplicaSet`. `kubectl api-resources --api-group=worker.gpustack.ai -o wide`
still serves old verbs and rejects `instancetypes` writes, while the new worker logs:

```text
customresourcedefinitions.apiextensions.k8s.io "admissionchecks.kueue.x-k8s.io" not found
release gpustack-operator-device-manager: ... rolled back due to atomic being set: context canceled
```

**Cause**: an interrupted atomic Helm upgrade of `gpustack-operator-device-manager` rolled back and
deleted adopted Kueue CRDs while the old `ReplicaSet` (`maxUnavailable: 0`) kept holding the install
lease.

**Recovery**:

```bash
NS=gpustack-system

# 1. Confirm a failed/pending release and an older Ready ReplicaSet holding the install lease:
helm -n "$NS" list -a
kubectl -n "$NS" get pods,rs
kubectl -n "$NS" get lease applications.worker.gpustack.ai -o yaml

# 2. Delete the old ReplicaSet so the new worker Pod acquires the lease and repairs the release:
kubectl -n "$NS" delete rs <old-replicaset>

# 3. Wait for rollout completion and verify the updated aggregated API surface:
kubectl -n "$NS" rollout status deploy/gpustack-operator-worker
kubectl api-resources --api-group=worker.gpustack.ai -o wide
```

If the new `Pod` still fails because a Kueue CRD is stuck in `Terminating`, follow
[Kueue CRDs stuck Terminating](#kueue-crds-stuck-terminating) or [Full-reset order](#full-reset-order).

## Namespace stuck Terminating

**Symptom**: `kubectl delete ns gpustack-system` hangs, and `kubectl describe ns gpustack-system`
reports `NamespaceDeletionDiscoveryFailure` on `v1.gpustack.ai`, `v1.worker.gpustack.ai`, or
`visibility.kueue.x-k8s.io` (`ServiceNotFound`).

**Recovery**: on older releases where cluster-scoped `APIService`s remain registered, stop the worker
first (otherwise its ensurer recreates them within ~30 s) and delete `APIService`s targeting the
namespace:

```bash
NS=gpustack-system

# 1. Stop the worker (in image mode, delete deploy/gpustack-operator-worker instead):
kubectl -n "$NS" scale deploy/gpustack-operator-worker --replicas=0

# 2. Delete every APIService backed by the terminating namespace:
kubectl get apiservices -o jsonpath='{.items[?(@.spec.service.namespace=="'"$NS"'")].metadata.name}' \
  | xargs -r kubectl delete apiservice
```

After the namespace finishes terminating, clean up two cluster-scoped leftovers before reinstalling:

- **Residual `MutatingWebhookConfiguration`s and `ValidatingWebhookConfiguration`s** (`failurePolicy: Fail`),
  which otherwise reject cluster-wide `Deployment` writes with `service "kueue-webhook-service" not found`.
  Filter by backing `Service` namespace so external Kueue installs stay untouched:

  ```bash
  kubectl get mutatingwebhookconfigurations,validatingwebhookconfigurations -o name \
    | grep -Ei 'gpustack|kueue|nfd' | while read -r wc; do
        kubectl get "$wc" -o jsonpath='{.webhooks[*].clientConfig.service.namespace}' \
          | grep -qw "$NS" && kubectl delete "$wc"
      done
  ```

- **Residual `ClusterRole`, `ClusterRoleBinding`, and `CSIDriver` objects** carrying stale
  `meta.helm.sh/release-name` annotations, which cause `invalid ownership metadata` on reinstall. Run
  current `deploy/gpustack-operator/chart/files/cleanup.sh "$NS"`, or delete matching objects plus
  `kubectl -n kube-system delete rolebinding kueue-visibility-server-auth-reader`.

**Never force-finalize a stuck namespace** (`kubectl replace --raw .../finalize` or stripping
*`metadata.finalizers`*); doing so orphans remaining namespaced and cluster-scoped objects.

## Kueue CRDs stuck Terminating

**Symptom**: after tearing down image mode or v0.5.x, `kueue.x-k8s.io` CRDs remain `Terminating`
because child objects still hold `kueue.x-k8s.io/resource-in-use`, and a fresh `helm install` fails
before hooks run with `CustomResourceDefinition "admissionchecks.kueue.x-k8s.io" ... cannot be imported`.

**Cause**: uninstalling Kueue deleted its controller and templated CRDs together, and `cleanup.sh` from
v0.8.6 or earlier matched only release name `gpustack-operator` unless passed a third argument.

**Recovery**:

```bash
NS=gpustack-system

# 1. Inspect Terminating Kueue CRDs and their owning release annotation:
kubectl get crd -o custom-columns='NAME:.metadata.name,DELETING:.metadata.deletionTimestamp,RELEASE:.metadata.annotations.meta\.helm\.sh/release-name' \
  | grep kueue

# 2. Run current cleanup.sh (handles gpustack-operator, gpustack-operator-device-manager, and gpustack-kueue):
bash deploy/gpustack-operator/chart/files/cleanup.sh "$NS"
#    On cleanup.sh from v0.8.6 or earlier, pass the owning release name as arg 3 instead:
#    bash cleanup.sh "$NS" gpustack-operator-worker-cert gpustack-operator-device-manager

# 3. Confirm zero Kueue CRDs remain (expect empty output):
kubectl get crd -o custom-columns=NAME:.metadata.name,DELETING:.metadata.deletionTimestamp | grep kueue
```

## NFD prune Job hangs

**Symptom**: during v0.5.x teardown after a failed NFD install, `helm uninstall gpustack-node-feature-discovery`
hangs waiting on `Job/node-feature-discovery-prune` because its `ServiceAccount` is already gone.

**Recovery**: delete the orphaned hook `Job` directly (nothing depends on it, and bundled NFD takes
over node labels on reinstall):

```bash
NS=gpustack-system

kubectl -n "$NS" get job node-feature-discovery-prune
kubectl -n "$NS" delete job node-feature-discovery-prune --ignore-not-found

# During full teardown only (never after an in-place subchart upgrade!):
helm -n "$NS" list -a | grep gpustack-node-feature-discovery \
  && helm -n "$NS" uninstall gpustack-node-feature-discovery --no-hooks
```

## Full-reset order

To wipe an existing worker installation cleanly (for example, before registering the cluster to a
different GPUStack server), execute these four steps in order:

```bash
NS=gpustack-system

# 1. Stop the worker so it stops re-registering APIServices:
kubectl -n "$NS" scale deploy/gpustack-operator-worker --replicas=0

# 2. Run the chart cleanup script (or helm uninstall with cleanupOnUninstall=true in chart mode):
bash deploy/gpustack-operator/chart/files/cleanup.sh "$NS"

# 3. Confirm zero APIServices still target the namespace (expect empty output):
kubectl get apiservices -o jsonpath='{.items[?(@.spec.service.namespace=="'"$NS"'")].metadata.name}'

# 4. Delete the namespace:
kubectl delete ns "$NS"
```

---

**See also** — [Migrating to Bundled Subcharts](/gpustack-operator/v0.12.0/docs/operate/migration/bundled-subcharts/index.md) (subchart ownership transfer) ·
[Migrating from v0.5.x](/gpustack-operator/v0.12.0/docs/operate/migration/from-v0.5/index.md) (Kueue finalizer deadlock background) ·
[Installation Modes](/gpustack-operator/v0.12.0/docs/operate/installation-modes/index.md) (chart mode vs image mode)

**Next** → [High Availability Operations](/gpustack-operator/v0.12.0/docs/operate/high-availability/index.md) — configure multi-replica control-plane failover.
