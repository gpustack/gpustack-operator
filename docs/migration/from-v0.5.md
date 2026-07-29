# Migrating the GPUStack Operator from v0.5.x to a higher version

v0.5.x is superseded by higher versions that fundamentally reshape the scheduling objects the
operator materializes. The first such release, v0.6.x ("unified-pool refactor",
[`specs/2026-06-29-instancetype-unified-pool-refactor.md`](../../specs/2026-06-29-instancetype-unified-pool-refactor.md)),
renames nearly every Kueue/NFD object it manages and removes the `Cohort` layer; later versions may
reshape the v0.5.x structures further. Every such upgrade is **breaking** for the scheduling objects
but backward compatible for your **workloads** (running Pods are never touched), so a plain
`helm upgrade` leaves the entire v0.5.x object set behind as **orphans**.

This guide explains what changes and gives two supported upgrade paths. It uses v0.6.x as the concrete
worked example throughout; substitute your actual target version in the commands.

## What changes (v0.5.x → v0.6.x, the first higher version)

| Object | v0.5.x | v0.6.x |
|---|---|---|
| **ResourceFlavor** | `gpustack--generic-ln-x64-4c-16g-98g--nvidia-tesla-t4-1d` — **double dash**, abbreviated `ln-x64`, CPU+device **composite**, unit-spec (`-4c-16g-98g`) baked into the name | **double dash**, full `os`/`arch`, CPU and device **split** into two flavors, `count`-suffixed: CPU `gpustack--<cpu>-linux-amd64-4c`, device `gpustack--<cpu>--nvidia-tesla-t4-linux-amd64-1d` |
| **ClusterQueue** | composite name; up to two per pool; joined by `spec.cohortName` | `gpustack--<key>-<os>-<arch>`; **one isolated CQ per pool**, `spec.cohortName` empty (**zero Cohort**) |
| **Cohort** | one per pool | **removed** — the `CohortReconciler` is gone |
| **AdmissionCheck** | — (did not exist) | `gpustack-node-devices` (per-card feasibility gate) |
| **InstanceType** | aggregated **virtual** API (projected from ClusterQueues; nothing stored) | a real **CRD** `instancetypes.worker.gpustack.ai` with a `.status` subresource; `spec.group` renamed `spec.acceleratorGroup`, `spec.generalGroup` added |
| **Node feature labels** | `general.feature.gpustack.ai/generic-ln-x64` + `.z-flavor`/`.z-queue`/`.z-cohort` + per-unit `.cpu`/`.ram`/`.storage` | real per-CPU key `general.feature.gpustack.ai/<cpu>` + `.count`/`.capacity`; the `feature.gpustack.ai/acceleratable` boolean; `.z-*` and `.cpu`/`.ram`/`.storage` **dropped** |

`Devices` and `Instance` keep their per-node / same names (the `Devices` schema only grows a
per-card allocation ledger), so they update in place — no orphaning there.

## Why a plain `helm upgrade` is not enough

The upgraded v0.6.x operator indexes objects by their **new** names, so it never sees — and never
reconciles or garbage-collects — the v0.5.x-named ResourceFlavors, ClusterQueues, Cohorts and
LocalQueues. And no upgrade hook cleans them: the chart's post-delete `cleanup.sh` fires only on
`helm uninstall`, and the migration hooks that do run on an upgrade address the pre-subchart release
layout (see [Migrating to the bundled subcharts](./to-subcharts.md)), not v0.5.x object names. The
result of an in-place upgrade is **both object sets coexisting**:
the working v0.6.x set plus a leaked v0.5.x set (dead ResourceFlavors, ClusterQueues, Cohorts, and a
LocalQueue in every namespace). The v0.5.x Cohorts in particular are never cleaned by anything.

The stale **node labels** are the one exception: they self-heal, because the same-named
`<node>-gpustack-worker` NodeFeature is overwritten on upgrade and NFD drops the removed labels.

## Kueue finalizer deadlock (self-healed automatically)

A running v0.5.x `Instance` uses an `InstanceType`-backed `ClusterQueue`, and Kueue stamps every
ClusterQueue with the `kueue.x-k8s.io/resource-in-use` finalizer. If the worker's runtime Kueue
(re)install ever tears Kueue down while that finalizer is still held — the destructive
`helm uninstall` path removes the controller — the ClusterQueue **and its CRD** get stuck
`Terminating` with nothing left to clear the finalizer. The Kueue install can then never recreate the
CRD, and because the worker gates startup on installing its applications, the operator itself fails to
start. The finalizer cannot even be force-stripped by hand: Kueue's validating webhook
(`failurePolicy: Fail`) is still registered but its Service has no endpoints, so any update to a
ClusterQueue is rejected.

Higher-version operators self-heal this before Kueue is deployed, so the upgrade completes without
manual intervention:

- A `pre-install`/`pre-upgrade` hook Job reaps an orphaned Kueue when it detects a Kueue CRD stuck
  `Terminating`: it deletes the Kueue admission-webhook configurations **first** (so the finalizer
  strip is no longer rejected), then strips the orphaned `resource-in-use` finalizers, then lets the
  Terminating CRDs drain so the install can recreate them. It is a no-op on a healthy cluster, and it
  runs on a fresh install too — a first install onto a cluster left in this state has no other way
  forward. (Versions before the subchart layout did the same thing inside the worker's Kueue
  installer.)
- Kueue is then repaired with a `helm upgrade` rather than a destructive `helm uninstall`+install, so
  the controller stays alive to clear finalizers and no CRD is stranded.

If you are on an older operator that predates this fix and are **already** wedged (a Kueue
CRD/ClusterQueue stuck `Terminating`), recover with Path A below: the chart's `cleanup.sh` deletes the
Kueue webhook configurations before stripping finalizers — the same load-bearing order — after which a
fresh install comes up cleanly.

## Path A — uninstall then reinstall (recommended)

The cleanest path, and the only one with **zero residue** by construction. Best when you can tolerate
a short scheduling gap (already-running Pods keep running; only new admissions pause until v0.6.x is up).

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

Deleting the CRDs deletes all their CRs (including every v0.5.x-named object), so nothing is left to
orphan. The v0.6.x worker then re-materializes the chain from the nodes.

## Path B — in-place upgrade, then remove the orphans

Use this when you must keep the release in place (e.g. to preserve custom `values`). Upgrade normally,
then run the migration cleanup script to strip the leaked v0.5.x objects.

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

[`cleanup-v0.5-orphans.sh`](./cleanup-v0.5-orphans.sh) removes **only** the v0.5.x orphans. Because
v0.6.x names are **also** double-dash, a bare `gpustack--` match is not safe — it would delete the
healthy chain. The reliable v0.5.x-only signal is the CPU/RAM **unit-spec** baked into every v0.5.x
flavor/queue/cohort name — a `-<n>c-<n>g` pair (e.g. `-4c-16g`); v0.6.x names carry only a trailing
`-<n>c` (CPU) or `-<n>d` (device) **count** and never a `-<n>g` gibibyte segment, so they never match.
The script deletes the matching objects in dependency order (LocalQueue → ClusterQueue → Cohort →
ResourceFlavor, so Kueue releases the `kueue.x-k8s.io/resource-in-use` finalizer before the flavors are
deleted). v0.5.x `InstanceType`s were a virtual API (no stored CRs), so it never touches InstanceType
objects. It never touches the v0.6.x objects, your namespaces, Pods, or `Instance`s. It is idempotent
and safe to re-run; a transient API error just means you re-run it.

If an old ClusterQueue still holds admitted workloads (a v0.5.x `Instance` ran across the upgrade),
the script first sets it to `HoldAndDrain` — the same graceful retirement the operator itself uses for
an InstanceType (`pkg/worker/controllers/worker/instance_type.go`) — so Kueue evicts those workloads and
releases the `resource-in-use` finalizer, then deletes the drained queue. Because the queue names
changed in v0.6.x, the evicted workloads must be re-created under the **new** pool's queue (see Notes).
An old queue with no workloads is deleted directly.

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

- **Workloads still on a v0.5.x queue are drained, not silently abandoned.** If an old ClusterQueue
  still holds admitted workloads, the cleanup drains it (`HoldAndDrain`) before deleting, so Kueue
  evicts them cleanly instead of leaving orphaned accounting behind still-running Pods. Because the
  v0.6.x queue names differ, **re-submit those workloads against the new pool's LocalQueue afterwards**.
  An old queue with no workloads is removed with no disruption. (Path A sidesteps this — a full
  uninstall stops everything up front.)
- **Prefer Path A** unless you have a specific reason to keep the release in place — it has no orphan
  class to reason about.
- This procedure was validated on a live v0.6.x cluster: a hand-created v0.5.x object set (double-dash
  composite ResourceFlavors + a ClusterQueue + a Cohort + a LocalQueue) was removed by the script with
  zero residue, and the healthy v0.6.x chain (its double-dash split flavors, queues, and InstanceTypes)
  was left byte-for-byte identical.
