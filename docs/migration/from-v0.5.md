# Migrating from v0.5.x

> **Purpose** — the two supported upgrade paths across the scheduling-chain refactor, and how to clear
> the v0.5.x orphans a plain `helm upgrade` leaves behind.
> **Audience** operators on a v0.5.x install · **Prerequisites** [Architecture](../architecture.md) ·
> **Read time** ~5 min

v0.5.x is superseded by versions that reshape the scheduling objects the operator materializes.
The first, v0.6.x ("unified-pool refactor",
[`specs/2026-06-29-instancetype-unified-pool-refactor.md`](../../specs/2026-06-29-instancetype-unified-pool-refactor.md)),
renames nearly every Kueue/NFD object it manages and drops the `Cohort` layer; later versions may
reshape v0.5.x structures further.

Each is **breaking** for the scheduling objects yet backward compatible for your **workloads**
(running Pods are never touched), so a plain `helm upgrade` leaves the entire v0.5.x object set
behind as **orphans**. v0.6.x is the worked example throughout; substitute your target version.

## Contents

- [What changes (v0.5.x → v0.6.x, the first higher version)](#what-changes-v05x--v06x-the-first-higher-version)
- [Why a plain helm upgrade is not enough](#why-a-plain-helm-upgrade-is-not-enough)
- [Kueue finalizer deadlock (self-healed automatically)](#kueue-finalizer-deadlock-self-healed-automatically)
- [Path A — uninstall then reinstall (recommended)](#path-a--uninstall-then-reinstall-recommended)
- [Path B — in-place upgrade, then remove the orphans](#path-b--in-place-upgrade-then-remove-the-orphans)
- [Verify](#verify)
- [Notes](#notes)

## What changes (v0.5.x → v0.6.x, the first higher version)

| Object | v0.5.x | v0.6.x |
|---|---|---|
| **ResourceFlavor** | `gpustack--generic-ln-x64-4c-16g-98g--nvidia-tesla-t4-1d` — **double dash**, abbreviated `ln-x64`, CPU+device **composite**, unit-spec (`-4c-16g-98g`) baked into the name | **double dash**, full `os`/`arch`, CPU and device **split** into two flavors, `count`-suffixed: CPU `gpustack--<cpu>-linux-amd64-4c`, device `gpustack--<cpu>--nvidia-tesla-t4-linux-amd64-1d` |
| **ClusterQueue** | composite name; up to two per pool; joined by `spec.cohortName` | `gpustack--<key>-<os>-<arch>`; **one isolated CQ per pool**, `spec.cohortName` empty (**zero Cohort**) |
| **Cohort** | one per pool | **removed** — the `CohortReconciler` is gone |
| **AdmissionCheck** | — (did not exist) | `gpustack-node-devices` (per-accelerator feasibility gate) |
| **InstanceType** | aggregated **virtual** API (projected from ClusterQueues; nothing stored) | a real **CRD** `instancetypes.worker.gpustack.ai` with a `.status` subresource; `spec.group` renamed `spec.acceleratorGroup`, `spec.generalGroup` added |
| **Node feature labels** | `general.feature.gpustack.ai/generic-ln-x64` + `.z-flavor`/`.z-queue`/`.z-cohort` + per-unit `.cpu`/`.ram`/`.storage` | real per-CPU key `general.feature.gpustack.ai/<cpu>` + `.count`/`.capacity`; the `feature.gpustack.ai/acceleratable` boolean; `.z-*` and `.cpu`/`.ram`/`.storage` **dropped** |

`Devices` and `Instance` keep their per-node / same names (the `Devices` schema only grows a
per-accelerator allocation ledger), so they update in place — nothing orphaned.

## Why a plain `helm upgrade` is not enough

The v0.6.x operator indexes objects by their **new** names, so it never sees — never reconciles,
never garbage-collects — the v0.5.x-named ResourceFlavors, ClusterQueues, Cohorts and LocalQueues.
No hook cleans them: the chart's post-delete `cleanup.sh` fires only on `helm uninstall`, and the
upgrade's own migration hooks address the pre-subchart release layout ([Migrating to the bundled
subcharts](./to-subcharts.md)), not v0.5.x names.

An in-place upgrade therefore leaves **both sets**: the working v0.6.x one plus a leaked v0.5.x one
— dead ResourceFlavors, ClusterQueues, Cohorts, a LocalQueue in every namespace, the Cohorts never
cleaned by anything.

Stale **node labels** are the one exception: the same-named `<node>-gpustack-worker` NodeFeature is
overwritten on upgrade and NFD drops the removed labels.

## Kueue finalizer deadlock (self-healed automatically)

A v0.5.x `Instance` runs on an `InstanceType`-backed `ClusterQueue`, and Kueue stamps every
ClusterQueue with the `kueue.x-k8s.io/resource-in-use` finalizer. Tearing Kueue down while it is held
— the destructive `helm uninstall` path removes the controller — wedges the ClusterQueue **and its
CRD** `Terminating` with nothing left to clear it: Kueue can never recreate the CRD, and the worker,
gating startup on installing its applications, never starts.

Nor can the finalizer be stripped by hand: Kueue's validating webhook (`failurePolicy: Fail`) is
still registered with no Service endpoints, so every ClusterQueue update is rejected.

Higher-version operators self-heal this before Kueue is deployed, so the upgrade needs no manual
work:

- A `pre-install`/`pre-upgrade` hook Job detects a Kueue CRD stuck `Terminating`, deletes the Kueue
  admission-webhook configurations **first** (so the finalizer strip is not rejected), strips the
  orphaned `resource-in-use` finalizers, and lets the CRDs drain for recreation. A no-op on a healthy
  cluster, and it runs on fresh installs too — the only way onto one left in this state. (Before the
  subchart layout, the worker's Kueue installer did this.)
- Kueue is then repaired by `helm upgrade`, not a destructive `helm uninstall`+install, so the
  controller stays alive to clear finalizers and no CRD is stranded.

Already wedged on an operator predating the fix (a Kueue CRD/ClusterQueue stuck `Terminating`)?
Recover with Path A below: `cleanup.sh` deletes the Kueue webhook configurations before stripping
finalizers — the same load-bearing order — and a fresh install then comes up cleanly.

## Path A — uninstall then reinstall (recommended)

The cleanest path, and the only one with **zero residue** by construction. Best when a short
scheduling gap is tolerable: running Pods keep running, only new admissions pause until v0.6.x is up.

```bash
NS=gpustack-system

# 1. Remove v0.5.x completely, including the runtime-installed Kueue/NFD/CSI sub-releases,
#    their CRDs/finalizers, and the aggregated APIServices/webhooks. cleanup.sh ships in the
#    chart under files/; run it against your active context.
bash deploy/gpustack-operator/chart/files/cleanup.sh "$NS"
# (or, if you enabled it, the gated post-delete hook: helm uninstall with cleanupOnUninstall=true)

# 2. Fresh install of v0.6.x.
helm install gpustack-operator gpustack/gpustack-operator -n "$NS" --create-namespace --version 0.6.0
```

Deleting the CRDs deletes all their CRs, every v0.5.x-named object included, so nothing is left to
orphan; the v0.6.x worker re-materializes the chain from the nodes.

## Path B — in-place upgrade, then remove the orphans

Use this when the release must stay in place, to preserve custom `values`. Upgrade normally, then
run the cleanup script to strip the leaked v0.5.x objects.

```bash
NS=gpustack-system

# 1. Upgrade in place.
helm upgrade gpustack-operator gpustack/gpustack-operator -n "$NS" --version 0.6.0 --reuse-values

# 2. Wait for the v0.6.x worker to be healthy.
kubectl -n "$NS" rollout status deploy/gpustack-operator-worker

# 3. Preview what the cleanup will remove (changes nothing), then run it.
bash docs/migration/cleanup-v0.5-orphans.sh --dry-run
bash docs/migration/cleanup-v0.5-orphans.sh
```

[`cleanup-v0.5-orphans.sh`](./cleanup-v0.5-orphans.sh) removes **only** the v0.5.x orphans. v0.6.x
names are double-dash too, so a bare `gpustack--` match would take the healthy chain; the v0.5.x-only
signal is the CPU/RAM **unit-spec** in every v0.5.x flavor/queue/cohort name — a `-<n>c-<n>g` pair
(e.g. `-4c-16g`), where v0.6.x names carry only a trailing `-<n>c` (CPU) or `-<n>d` (device)
**count**, never `-<n>g`.

Deletion follows dependency order (LocalQueue → ClusterQueue → Cohort → ResourceFlavor, so Kueue
releases the finalizer before the flavors go). v0.5.x `InstanceType`s were virtual (nothing stored),
so InstanceTypes are never touched — nor v0.6.x objects, your namespaces, Pods or `Instance`s. It is
idempotent: a transient API error just means re-run it.

If an old ClusterQueue still holds admitted workloads (a v0.5.x `Instance` ran across the upgrade),
the script sets `HoldAndDrain` first — the graceful retirement the operator uses for an InstanceType
(`pkg/worker/controllers/worker/instance_type.go`) — so Kueue evicts them and releases the finalizer,
then deletes the drained queue; a queue with no workloads goes directly. The names changed, so
**re-create the evicted workloads under the new pool's queue**.

## Verify

```bash
# Nothing is wedged Terminating (the DELETING column is <none> for every kueue CRD — see the
# finalizer-deadlock section; a timestamp on an older operator needs the Path A recovery):
kubectl get crd -o custom-columns=NAME:.metadata.name,DELETING:.metadata.deletionTimestamp | grep kueue

# No v0.5.x orphans remain (expect empty) — match the v0.5.x-only "-<n>c-<n>g" unit-spec, NOT a bare
# "gpustack--" (which would also list the healthy v0.6.x objects):
kubectl get resourceflavor,clusterqueue,cohort -A -o name | grep -E 'gpustack--.*-[0-9]+c-[0-9]+g'

# The v0.6.x chain is healthy: one isolated CQ per pool, zero Cohort, InstanceTypes Active:
kubectl get clusterqueue -o custom-columns='NAME:.metadata.name,COHORT:.spec.cohortName'
kubectl get cohort -A                       # expect: No resources found
kubectl get instancetype -o custom-columns='NAME:.metadata.name,PHASE:.status.phase'
kubectl get admissioncheck                  # expect: gpustack-node-devices

# Node labels self-healed (expect empty):
kubectl get nodes -o json | grep -oE '"[^"]*(\.z-[a-z]+|generic-ln-x64)[^"]*"' | sort -u
```

## Notes

- **Workloads still on a v0.5.x queue are drained, not silently abandoned** — a clean eviction, no
  orphaned accounting left behind still-running Pods; Path A sidesteps it by stopping everything up
  front.
- **Prefer Path A** unless you must keep the release in place: no orphan class to reason about.
- Validated on a live v0.6.x cluster: a hand-created v0.5.x set (double-dash composite
  ResourceFlavors, a ClusterQueue, a Cohort, a LocalQueue) was removed with zero residue, the healthy
  v0.6.x chain left byte-for-byte identical.

---

**See also** — [Migrating to the bundled subcharts](to-subcharts.md) (the other one-time upgrade, from
v0.7.x or earlier) · [Scheduling Chain](../architecture/scheduling-chain.md#naming-and-grouping) (the
object names this upgrade moves to)

**Next** → [Architecture](../architecture.md) — what the new object set means.
