# Migration Troubleshooting

> **Purpose** — recovering from the two failures an in-place operator upgrade or a cluster reset can
> leave behind: a worker stuck in CrashLoopBackOff while the old replica keeps serving, and a namespace
> that never finishes deleting.
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

**Never** force-finalize a stuck namespace (`kubectl replace --raw .../finalize`, or patching
`metadata.finalizers` away): the namespace object vanishes while whatever the deletion had not reached
stays behind — CRs, Secrets, the very APIServices above — orphaned for good.

## The safe full-reset order

Re-registering a cluster against a different GPUStack server means wiping the worker install. Done in
this order, the namespace never wedges:

```bash
NS=gpustack-system

# 1. Stop the worker, as above.
kubectl -n "$NS" scale deploy/gpustack-operator-worker --replicas=0

# 2. Run the chart's cleanup script — runtime-installed releases, CRDs and their finalizers,
#    APIServices and webhooks. It ships under files/ in the chart.
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
