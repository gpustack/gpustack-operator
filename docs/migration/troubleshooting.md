# Migration Troubleshooting

> **Purpose** — recovering from the four failures an in-place operator upgrade or a cluster reset can
> leave behind: a worker stuck in CrashLoopBackOff while the old replica keeps serving, a namespace
> that never finishes deleting, Kueue CRDs left Terminating by a teardown, and an NFD prune Job that
> never finishes.
> **Audience** operators · **Prerequisites** [Migrating to Bundled Subcharts](to-subcharts.md) ·
> **Read time** ~8 min

Chart-mode installs are fixed in current releases: the worker Deployment defaults to `Recreate`
(`worker.strategyType`), abandoned Helm pending records repair without a destructive rollback,
and a terminating worker removes the APIServices backed by its namespace. Image mode is not —
the server still renders `RollingUpdate` (#128), so the wedged-upgrade recovery below stays
live guidance, for older releases and for the reset any release can meet too.

## Contents

- [Worker CrashLoopBackOff after an upgrade](#worker-crashloopbackoff-after-an-upgrade)
- [Namespace stuck Terminating](#namespace-stuck-terminating)
- [Kueue CRDs stuck Terminating after a teardown](#kueue-crds-stuck-terminating-after-a-teardown)
- [NFD prune Job that never finishes](#nfd-prune-job-that-never-finishes)
- [The safe full-reset order](#the-safe-full-reset-order)

## Worker CrashLoopBackOff after an upgrade

The symptom: after an in-place upgrade — re-applied server-rendered manifests in image mode, `helm
upgrade` in chart mode — the new worker pod never leaves `CrashLoopBackOff`, yet the Deployment reports
ready, because the old ReplicaSet's pod is still serving. The aggregated API answers with the OLD
binary's surface: `kubectl api-resources --api-group=worker.gpustack.ai -o wide` shows the old verbs,
and writes to `instancetypes` are refused.

The new pod's log carries the chain:

```
customresourcedefinitions.apiextensions.k8s.io "admissionchecks.kueue.x-k8s.io" not found
release gpustack-operator-device-manager: ... rolled back due to atomic being set: context canceled
```

What happened: the boot's atomic Helm upgrade of `gpustack-operator-device-manager` was interrupted and
rolled back, and the rollback deleted Kueue CRDs the interrupted upgrade had just adopted. Every later
boot then fails patching a CRD the release record still references but the cluster no longer has, and a
pod that fails its install never becomes Ready.

> **Why the old pod keeps serving** — the pre-fix worker Deployment rolled with maxUnavailable 0, so the
> old pod is held Ready until the new one passes its probes. The new one never does: its install fails
> before it opens its port.

Recover:

```bash
NS=gpustack-system

# 1. Confirm the wedge: the device-manager release is failed/pending, and an older worker ReplicaSet
#    is still Ready beside the crash-looping one.
helm -n "$NS" list -a
kubectl -n "$NS" get pods,rs
kubectl -n "$NS" get lease applications.worker.gpustack.ai -o yaml   # who holds the install lease

# 2. Free the lease: delete the OLD ReplicaSet (its pod predates the upgrade). With the old holder
#    stopped, the new pod's next restart repairs the release with exclusive access.
kubectl -n "$NS" delete rs <old-replicaset>

# 3. Watch the new pod converge, then verify the aggregated API flipped to the new surface.
kubectl -n "$NS" rollout status deploy/gpustack-operator-worker
kubectl api-resources --api-group=worker.gpustack.ai -o wide
```

If the pod still fails on a missing Kueue CRD, check whether the CRD itself is stuck `Terminating`
(`kubectl get crd | grep kueue`) — that is the finalizer deadlock of [Migrating from
v0.5.x](from-v0.5.md#kueue-finalizer-deadlock-self-healed-automatically), which the chart's
migrate-pre hook reaps on the next boot. When neither repair converges, take the full reset below.

## Namespace stuck Terminating

The symptom: `kubectl delete ns gpustack-system` never finishes, and `kubectl describe ns
gpustack-system` reports `NamespaceDeletionDiscoveryFailure` — the APIServices `v1.gpustack.ai`,
`v1.worker.gpustack.ai` and Kueue's two `visibility.kueue.x-k8s.io` ones stand at `False
(ServiceNotFound)`. They are cluster-scoped and outlive their namespaced backing Services, and
namespace GC cannot finish discovery while they do.

Current releases remove all four themselves once the namespace is Terminating
(`deregisterOnTeardown` in `pkg/worker/worker.go` deletes every APIService backed by the
namespace, Kueue's pair included). On an older release — or wherever one is left — delete them
by backing Service, not by name, and stop the worker FIRST: its ensurer recreates them within
~30 seconds while it runs.

```bash
NS=gpustack-system

# 1. Stop the worker so nothing re-registers (chart mode shown; in image mode delete the worker
#    Deployment instead).
kubectl -n "$NS" scale deploy/gpustack-operator-worker --replicas=0

# 2. Delete every APIService proxying into the namespace.
kubectl get apiservices -o jsonpath='{.items[?(@.spec.service.namespace=="'"$NS"'")].metadata.name}' \
  | xargs -r kubectl delete apiservice
```

The namespace then finalizes within about a minute. If it still hangs, describe it again — the
condition names the next discovery group that cannot be listed, and the same two steps clear it.

Two more kinds of debris outlive the namespace and bite **after** it is gone. Neither blocks the
finalizer, which is why they surface only on the next operation:

- The `kueue-*` and `gpustack-worker-*` Mutating/ValidatingWebhookConfigurations keep
  intercepting creates and updates **cluster-wide** (Deployments included) with
  `failurePolicy: Fail`, calling Services that no longer exist — the next `kubectl apply` of any
  Deployment fails with `service "kueue-webhook-service" not found`. Name patterns only nominate
  the sweep — an external Kueue install matches them too — so confirm by the backing Service's
  namespace before deleting:

  ```bash
  kubectl get mutatingwebhookconfigurations,validatingwebhookconfigurations -o name \
    | grep -Ei 'gpustack|kueue|nfd' | while read -r wc; do
        kubectl get "$wc" -o jsonpath='{.webhooks[*].clientConfig.service.namespace}' \
          | grep -qw "$NS" && kubectl delete "$wc"
      done
  ```

- Orphaned cluster-scoped RBAC and `CSIDriver` objects still carry
  `meta.helm.sh/release-name` of a release whose record died with the namespace, so a reinstall
  CrashLoops the worker on `invalid ownership metadata`, and no adoption re-fires (it gates on
  the legacy release records — exactly what is gone). Current `cleanup.sh` sweeps them; on an
  older copy the same pattern over `clusterrole,clusterrolebinding,csidriver` clears them, plus
  `kubectl -n kube-system delete rolebinding kueue-visibility-server-auth-reader`.

**Never** force-finalize a stuck namespace (`kubectl replace --raw .../finalize`, or patching
`metadata.finalizers` away): the namespace object vanishes while whatever the deletion had not reached
stays behind — CRs, Secrets, the very APIServices above — orphaned for good.

## Kueue CRDs stuck Terminating after a teardown

The symptom: after an image-mode teardown, or after removing a v0.5.x install, some
`kueue.x-k8s.io` CRDs stay `Terminating`, and their ClusterQueues, ResourceFlavors, Topologies or
AdmissionChecks still carry `kueue.x-k8s.io/resource-in-use`. The next install under another release
name, such as a chart-mode `helm install`, fails on `CustomResourceDefinition
"admissionchecks.kueue.x-k8s.io" ... exists and cannot be imported into the current release`.

What happened: uninstalling the release that owns Kueue removes the controller and its templated
CRDs in one pass, so nothing is left to clear the finalizers. `cleanup.sh` strips them, but a copy
from v0.8.6 or earlier recognizes only a Kueue owned by the release in its third argument,
`gpustack-operator` by default. It skips the `gpustack-operator-device-manager` (image mode) and
`gpustack-kueue` (v0.5.x) ones.

The chart's migrate-pre hook cannot reap them either: Helm refuses the install on ownership before
any hook runs.

Recover:

```bash
NS=gpustack-system

# 1. Confirm the wedge: which Kueue CRDs are Terminating, and which release owns them.
kubectl get crd -o custom-columns='NAME:.metadata.name,DELETING:.metadata.deletionTimestamp,RELEASE:.metadata.annotations.meta\.helm\.sh/release-name' \
  | grep kueue

# 2. Run the current cleanup script, which recognizes all three release names.
bash deploy/gpustack-operator/chart/files/cleanup.sh "$NS"
#    With a copy from v0.8.6 or earlier, name the owning release from step 1 as the third
#    argument instead (the second is the worker's certificate Secret):
bash cleanup.sh "$NS" gpustack-operator-worker-cert gpustack-operator-device-manager

# 3. Verify (expect no row).
kubectl get crd -o custom-columns=NAME:.metadata.name,DELETING:.metadata.deletionTimestamp | grep kueue
```

## NFD prune Job that never finishes

The symptom: removing a v0.5.x install whose own Node Feature Discovery install had failed, the
teardown spends minutes on `helm uninstall gpustack-node-feature-discovery`, never reports that
release as uninstalled, and leaves a Job `node-feature-discovery-prune` in the namespace that never
completes.

What happened: the NFD chart runs that Job as its post-delete hook, to strip NFD's labels from the
nodes, and Helm waits for it. Here it cannot finish, for example because its ServiceAccount is
already gone. A healthy v0.5.x install, a fresh install and a current chart's uninstall do not meet
this.

It blocks nothing. No component reads the Job, the namespace still deletes, and a later install
still succeeds: the bundled NFD takes the node labels over, and its own hook replaces a Job of that
name. Deleting it is safe.

Recover:

```bash
NS=gpustack-system

# 1. Confirm: the Job exists and has not completed.
kubectl -n "$NS" get job node-feature-discovery-prune

# 2. Delete it. Nothing waits on it, and nothing else goes with it.
kubectl -n "$NS" delete job node-feature-discovery-prune --ignore-not-found

# 3. During a teardown only: if the legacy release is still listed, finish its removal without
#    running the hook again. After an upgrade, leave it to the chart's migrate-post hook, which
#    retires legacy release records; uninstalling one then deletes objects the chart adopted.
helm -n "$NS" list -a | grep gpustack-node-feature-discovery \
  && helm -n "$NS" uninstall gpustack-node-feature-discovery --no-hooks
```

## The safe full-reset order

Re-registering a cluster against a different GPUStack server means wiping the worker install. Done in
this order, the namespace never wedges:

```bash
NS=gpustack-system

# 1. Stop the worker, as above.
kubectl -n "$NS" scale deploy/gpustack-operator-worker --replicas=0

# 2. Run the chart's cleanup script — runtime-installed releases, CRDs and their finalizers,
#    APIServices and webhooks. It ships under files/ in the chart. A copy from v0.8.6 or earlier
#    leaves an image-mode Kueue Terminating; see the section above.
bash deploy/gpustack-operator/chart/files/cleanup.sh "$NS"
#    Chart-mode alternative: helm uninstall gpustack-operator -n "$NS" with cleanupOnUninstall=true.

# 3. Verify nothing still proxies into the namespace (expect empty).
kubectl get apiservices -o jsonpath='{.items[?(@.spec.service.namespace=="'"$NS"'")].metadata.name}'

# 4. Delete the namespace.
kubectl delete ns "$NS"
```

---

**See also** — [Migrating to Bundled Subcharts](to-subcharts.md) (the ownership transfer whose
interruption the CrashLoop section repairs) · [Migrating from v0.5.x](from-v0.5.md) (the Kueue
finalizer deadlock behind a stuck CRD) · [Installation Modes](../architecture/installation-modes.md)
(the two modes the commands mark at each step)

**Next** → [High Availability Operations](../operation/high-availability.md) — the replica knobs a
recovered install is worth revisiting.
