# Migrating from v0.5.x

Releases starting at v0.6.x ([unified-pool refactor](https://github.com/gpustack/gpustack-operator/blob/016fe1d2b2b20bbdaeeec4c4f15d18c349393b24/specs/2026-06-29-instancetype-unified-pool-refactor.md))
rename the scheduling objects managed by GPUStack Operator and remove the Kueue `Cohort` layer.
Running workload `Pod`s continue uninterrupted, while an in-place `helm upgrade` leaves v0.5.x
scheduling objects behind as orphans. Commands below use `0.6.0` as the example target version;
substitute the version you are installing.

## Contents

- [Changes in v0.6](#changes-in-v06)
- [Orphaned objects after upgrade](#orphaned-objects-after-upgrade)
- [Kueue finalizer deadlock](#kueue-finalizer-deadlock)
- [Path A: Uninstall and reinstall](#path-a-uninstall-and-reinstall)
- [Path B: Upgrade in place](#path-b-upgrade-in-place)
- [Verification](#verification)
- [Summary](#summary)

## Changes in v0.6

| Object | v0.5.x | v0.6.x and later |
|---|---|---|
| **`ResourceFlavor`** | Composite CPU+device name with abbreviated `ln-x64` and unit spec (`-4c-16g-98g`) | Split CPU (`gpustack--<cpu>-linux-amd64-4c`) and device (`gpustack--<cpu>--nvidia-tesla-t4-linux-amd64-1d`) flavors |
| **`ClusterQueue`** | Composite name; up to two queues per pool joined by *`spec.cohortName`* | `gpustack--<key>-<os>-<arch>`; one isolated queue per pool with empty *`spec.cohortName`* |
| **`Cohort`** | One per pool | Removed (`CohortReconciler` deleted) |
| **`AdmissionCheck`** | Not present | `gpustack-node-devices` per-accelerator feasibility gate |
| **`InstanceType`** | Virtual aggregated API projected from `ClusterQueue`s | Stored CRD `instancetypes.worker.gpustack.ai` with `.status`, *`spec.acceleratorGroup`*, and *`spec.generalGroup`* |
| **`Node` feature labels** | `general.feature.gpustack.ai/generic-ln-x64`, `.z-*`, and `.cpu`/`.ram`/`.storage` | Per-CPU `general.feature.gpustack.ai/<cpu>` (`.count`, `.capacity`) and `feature.gpustack.ai/acceleratable` |

`Devices` and `Instance` keep their names and update in place without creating orphans.

## Orphaned objects after upgrade

The v0.6.x worker indexes scheduling resources by their new names and ignores v0.5.x-named
`ResourceFlavor`s, `ClusterQueue`s, `Cohort`s, and `LocalQueue`s. Chart `cleanup.sh` runs only on
`helm uninstall`, and subchart migration hooks ([Migrating to Bundled Subcharts](/gpustack-operator/main/docs/operate/migration/bundled-subcharts/index.md))
do not delete v0.5.x pool names.

Stale `Node` labels clear automatically because the worker overwrites `<node>-gpustack-worker`
`NodeFeature` objects in place. All other v0.5.x Kueue objects require either Path A or Path B below.

## Kueue finalizer deadlock

Kueue places `kueue.x-k8s.io/resource-in-use` on every active `ClusterQueue`. If a destructive
teardown removes the Kueue controller while that finalizer remains, the `ClusterQueue` and its CRD
stay stuck in `Terminating`, while Kueue's residual validating webhook (`failurePolicy: Fail`) rejects
manual finalizer edits.

Current operator charts prevent and self-heal this state automatically:

- A `pre-install`/`pre-upgrade` hook `Job` deletes Kueue webhook configurations first, strips orphaned
  `resource-in-use` finalizers, and lets stuck Kueue CRDs finish terminating before installation.
- Upgrades update Kueue via `helm upgrade` rather than uninstalling its controller.

If an older release already wedged Kueue CRDs in `Terminating`, run [Path A](#path-a-uninstall-and-reinstall)
below.

## Path A: Uninstall and reinstall

Use Path A whenever a brief scheduling pause is acceptable. Running workload `Pod`s stay up, new
admissions pause briefly, and deleting the CRDs removes every v0.5.x object cleanly:

```bash
NS=gpustack-system

# 1. Remove v0.5.x releases, CRDs, finalizers, APIServices, and webhooks.
#    If using cleanup.sh from v0.8.6 or earlier, pass explicit arguments instead:
#    bash cleanup.sh "$NS" gpustack-operator-worker-cert gpustack-kueue
bash deploy/gpustack-operator/chart/files/cleanup.sh "$NS"

# 2. Install the target version cleanly:
helm install gpustack-operator gpustack/gpustack-operator -n "$NS" --create-namespace --version 0.6.0
```

## Path B: Upgrade in place

Use Path B when the existing Helm release and custom values must remain in place:

```bash
NS=gpustack-system

# 1. Upgrade the release in place:
helm upgrade gpustack-operator gpustack/gpustack-operator -n "$NS" --version 0.6.0 --reset-then-reuse-values

# 2. Wait for the upgraded worker Deployment:
kubectl -n "$NS" rollout status deploy/gpustack-operator-worker

# 3. Preview and remove v0.5.x scheduling orphans:
bash docs/operate/migration/cleanup-v0.5-orphans.sh --dry-run
bash docs/operate/migration/cleanup-v0.5-orphans.sh
```

[`cleanup-v0.5-orphans.sh`](/gpustack-operator/main/docs/operate/migration/cleanup-v0.5-orphans.sh) matches only the v0.5.x `-<n>c-<n>g` CPU/RAM unit
pattern (such as `-4c-16g`), leaving v0.6.x `-<n>c` and `-<n>d` objects untouched. It deletes objects
in dependency order (`LocalQueue` → `ClusterQueue` → `Cohort` → `ResourceFlavor`) and is safe to
re-run after transient API errors.

If a v0.5.x `ClusterQueue` still holds admitted `Workload`s, the script sets `HoldAndDrain` so Kueue
evicts them and releases finalizers before deletion. Re-submit evicted workloads against the new
pool's `LocalQueue`.

## Verification

Run these checks after completing Path A or Path B:

```bash
# 1. Confirm no Kueue CRD is stuck Terminating (DELETING must be <none>):
kubectl get crd -o custom-columns=NAME:.metadata.name,DELETING:.metadata.deletionTimestamp | grep kueue

# 2. Confirm zero v0.5.x scheduling orphans remain (expect empty output):
kubectl get resourceflavor,clusterqueue,cohort -A -o name | grep -E 'gpustack--.*-[0-9]+c-[0-9]+g'

# 3. Confirm healthy isolated pools, zero Cohorts, Active InstanceTypes, and AdmissionChecks:
kubectl get clusterqueue -o custom-columns='NAME:.metadata.name,COHORT:.spec.cohortName'
kubectl get cohort -A
kubectl get instancetype -o custom-columns='NAME:.metadata.name,PHASE:.status.phase'
kubectl get admissioncheck

# 4. Confirm legacy Node labels cleared (expect empty output):
kubectl get nodes -o json | grep -oE '"[^"]*(\.z-[a-z]+|generic-ln-x64)[^"]*"' | sort -u
```

## Summary

Prefer Path A unless you need to preserve an in-place Helm release, because CRD removal leaves zero
orphan state to audit. On Path B, [`cleanup-v0.5-orphans.sh`](/gpustack-operator/main/docs/operate/migration/cleanup-v0.5-orphans.sh) drains residual
v0.5.x queues cleanly and leaves the v0.6.x scheduling chain intact[^1].

[^1]: Validated on a live v0.6.x cluster against composite v0.5.x `ResourceFlavor`, `ClusterQueue`, `Cohort`, and `LocalQueue` fixtures with zero changes to active v0.6.x objects.

---

**See also** — [Migrating to Bundled Subcharts](/gpustack-operator/main/docs/operate/migration/bundled-subcharts/index.md) (one-time ownership transfer from
v0.7.x or earlier) · [Accelerator Requests](/gpustack-operator/main/docs/modules/devices/requests/index.md) (resource keys and pool
capacity views) · [Migration Troubleshooting](/gpustack-operator/main/docs/operate/migration/troubleshooting/index.md) (recovering wedged upgrades or stuck
namespaces)

**Next** → [Architecture](/gpustack-operator/main/docs/getting-started/architecture/index.md) — review the scheduling chain and pool vocabulary.
