# Scheduling Chain

> **Purpose** — how node and device signals become capacity labels, and how five controllers
> materialize them into a Kueue `ResourceFlavor` → `ClusterQueue` → `LocalQueue` chain plus an
> `InstanceType` CRD.
> **Audience** contributors · **Prerequisites** [Architecture](../architecture.md),
> [Device Discovery](device-discovery.md) · **Read time** ~9 min

Stages 3 and 4 of the four-stage chain; what a request must then pass is in [Admission](admission.md).

## Contents

- [Stage 3: capacity profiling](#stage-3-capacity-profiling)
- [The unit spec is not derived from node capacity](#the-unit-spec-is-not-derived-from-node-capacity)
- [Stage 4: the Kueue chain](#stage-4-the-kueue-chain)
- [Naming and grouping](#naming-and-grouping)
- [The controllers](#the-controllers)

## Stage 3: capacity profiling

Two Worker controllers turn Node + `Devices` signals into the capacity labels the chain consumes.

- **`NodeFeatureReconciler`** (`node_feature.go`) reports a NodeFeature `${NODE_NAME}-gpustack-worker`
  per Node, stamping `gpustack.ai/managed=true`. Under `GPUSTACK_NODE_MANAGEMENT_MANUAL=true` (read
  per-reconcile) it skips that injection, honoring only an admin-set `managed` label, so onboarding is
  gated node-by-node.
- **`NodeCapacityReconciler`** (`node_capacity.go`) builds them via
  `nodefeature.ConstructNodeCapacityLabels` from the Node and its same-named `Devices` CR: the
  general(CPU) presence marker plus both families' per-accelerator capacities.

### Logical-slicing capacities

Per accelerator model whose `.sliced` resource is present and > 0 in the Node capacity, four capacities
the default scheduler / kubelet count at admission:

| Label suffix (`acceleratable.${prefix}${aKey}.…`) | Value |
|----------------------------------------------------|--------|
| `.sliced.units`             | `count × M` (M = 1,600,000 credit units per whole accelerator) |
| `.sliced.cores-percentage`  | `Σ per-accelerator slices × 100` (compute overcommit) or `count × 100` (compute non-overcommittable) |
| `.sliced.memory-percentage` | `count × 100` |
| `.sliced.memory-mib`        | `Σ count × per-model VRAM MiB` (weighted per model so mixed-VRAM models sum correctly) |

### Hardware-partitioning capacities

Symmetrically per model whose `.partitioned` resource is present and > 0, over the *disjoint* population
of accelerators in a partitioning mode, so no accelerator counts in both families:

| Label suffix (`acceleratable.${prefix}${aKey}.…`) | Value |
|----------------------------------------------------|--------|
| `.partitioned.units`               | `partitioned accelerators × M` (a partitioned accelerator is worth a whole accelerator's credits, exactly as a logically sliceable one is) |
| `.partitioned.<kind>-<profile>`    | `Σ (allocated + remaining)` instances of that profile over the node's partitioned accelerators |

The per-profile key is **geometry-aware and ledger-derived**, not a static ceiling: with one `3g.40gb`
carved on an 80 GB accelerator, `…partitioned.mig-7g.80gb` reads 0 while `…partitioned.mig-3g.40gb`
still reads 1 free instance.

> **Why both terms** — the scheduler fits a Pod by subtracting the requests of the Pods already on the
> node, so bare `remaining` would subtract every live instance twice. An accelerator whose ledger is not
> published yet falls back to its static per-profile ceiling, not to zero, so a fresh node advertises
> room instead of nothing.

`<profile>` is the **published** name, not always what the manufacturer's CLI prints:

- a manufacturer that writes its two-number geometry without a separator has one added — T-Head's
  `4g48gb` publishes as `4g.48gb`, matching how NVIDIA already writes `3g.40gb`;
- the rule is keyed on the manufacturer, not the shape, so the same string from NVIDIA is published
  untouched;
- any other shape is published as the driver reports it.

So either manufacturer's partition reads alike in a Pod spec, in the `InstanceType`'s offered inventory
and in the per-profile ledgers.

Below that boundary every layer keeps the manufacturer's spelling — the `Devices` record, the on-disk
ownership markers, every call into its library — since a name the library does not report cannot create
a partition. See [T-Head MIG
Operations](../operation/thead-mig.md#how-partition-profiles-are-discovered).

### Per-accelerator slice counts, per manufacturer

| Manufacturer | Slices per accelerator | Compute |
|---|---|---|
| NVIDIA | 128 | time-shared, so overcommittable |
| Ascend | 63 | time-shared, so overcommittable |
| Cambricon | 16 | not overcommittable |
| Hygon | 4 | not overcommittable — a hard spatial partition (`vdev.conf` assigns each slice a disjoint CU bitmask, so the sum stays within one accelerator) |
| MThreads | 16 | not overcommittable — `cores%` is a best-effort relative weight, not a hard partition |
| MetaX | 16 | not overcommittable |

Each count is the maximum the Device Manager records on **each accelerator's** `Status.LogicalSliced`,
bounded by the manufacturer runtime's per-device user-process limit (`pkg/devicemanager/detector`);
`AcceleratorSlicedDetail` aggregates them per group. A MIG-enabled accelerator reports zero logical
count and its physical MIG profiles instead.

An overcommittable manufacturer advertises `.sliced.cores-percentage = Σ slice count × 100`, each slice
able to claim a full 100 %; the others cap it at `count × 100`.

### Presence-gating on capacity

Both families' counting keys are **presence-gated on capacity**: patched only while the bare pool
(`.sliced` / `.partitioned`) is present and positive in `Node.status.capacity`, reverse-patched away
when it disappears or reaches 0. A model with no logical slicing gets none of the four `.sliced.*`
capacities; one with no partitioned accelerator gets no `.partitioned.*` key.

> **Why capacity and not allocatable** — allocatable also falls to zero when a family is merely
> saturated, which would delete the keys while instances are live.

Stale cleanup covers all four `.sliced.*` suffixes, `.partitioned.units` and every per-profile key.
Enabling or disabling hardware partitioning is manual, through the manufacturer's CLI (`nvidia-smi`,
`ppu-smi mig` for T-Head), and the operator sees it only on the next Device Manager detection.

The [three-configuration
walkthrough](../operation/nvidia-mig.md#walkthrough-three-mig-configurations-on-one-node) in [NVIDIA MIG
Operations](../operation/nvidia-mig.md) shows the disjoint populations on a recorded 8-accelerator node,
including a **mixed** one advertising both families;
[T-Head](../operation/thead-mig.md) covers the same procedure.

## The unit spec is not derived from node capacity

The **unit spec** (unitCPU / unitRAM / localStorage) is **not derived from node capacity at all**. The
InstanceType default follows acceleratable-ness: `1c / 2Gi / 100Gi` non-accelerated; accelerated, a
**per-product preset** keyed by the accelerator's manufacturer and product name, falling back to
`4c / 16Gi / 100Gi`.

The tier per product family is in [Instance Type Unit Resources
Reference](../reference/instance-type-unit-resources.md); an admin overrides it through the InstanceType
API without touching any Node.

## Stage 4: the Kueue chain

The capacity labels drive a Kueue chain built by `pkg/worker/controllers/worker`. **One isolated
ClusterQueue per pool**: with exclusive / shared / sliced / partitioned in one queue there is
no cross-queue borrowing to broker, so `spec.cohortName` stays empty.

## Naming and grouping

The **`ResourceFlavor` is the finest grain and setting-independent**: always the CPU key, plus the
accelerator key when accelerated, so the aggregation layer re-groups without rewriting a flavor. That
layer (`ClusterQueue` / `InstanceType` / `InstanceTypeFlavor`) is grouped by the editable
[`instance-type-aware-cpu-manufacturer`](../settings.md#online-adjustable-settings) (default `false`):

```
ResourceFlavor (always the finest grain, setting-independent):
  gpustack--${gKey}-${os}-${arch}-${cpu}c              # non-accelerated (c = CPU cores)
  gpustack--${gKey}--${aKey}-${os}-${arch}-${acc}d     # accelerated     (d = devices)

ClusterQueue / InstanceType — grouped by instance-type-aware-cpu-manufacturer:
  aware=false (default):  gpustack--generic-${os}-${arch}           # all CPUs collapse into one generic pool
                          gpustack--${aKey}-${os}-${arch}           # one pool per accelerator (CPU ignored)
  aware=true:             gpustack--${gKey}-${os}-${arch}           # split per CPU
                          gpustack--${gKey}--${aKey}-${os}-${arch}  # split per (CPU, accelerator)

InstanceTypeFlavor (catalog, no os/arch): mirrors the same grouping —
  gpustack--generic / gpustack--${aKey}   (aware=false)
  gpustack--${gKey}  / gpustack--${gKey}--${aKey}   (aware=true)
```

Two discriminators keep the pools clean:

- `feature.gpustack.ai/acceleratable=true|false`, on every flavor and queue, lets a collapsed generic
  queue select "all non-accelerated flavors" and stops an *aware* generic queue (`general.${gKey}=true`)
  matching an accelerated flavor carrying the same key.
- `note.gpustack.ai/cpuDetail` carries the raw CPU detail: always on a CPU flavor, on an accelerated one
  only when awareness is on. The defaulting webhook folds it back into the type's spec — see
  [Admission](admission.md#the-instancetype-and-instance-webhooks).

## The controllers

```mermaid
flowchart LR
    NODE["Node<br/>(general./acceleratable. capacity labels + gpustack.ai/managed)"]
    DEV["Devices CR<br/>(per-accelerator AcceleratorAllocation ledger)"]

    subgraph controllers["WK controllers"]
        NFR["NodeFlavorReconciler"]
        ITR["InstanceTypeReconciler"]
        NQR["NodeQueueReconciler"]
        NQE["NodeQueueEntranceReconciler"]
        AC["NodeDevicesAdmissionReconciler"]
    end

    NODE --> NFR
    NFR -- "one per (key,os,arch,count)<br/>capacity = nodes × count" --> RF["ResourceFlavor"]
    NFR -- "authors (create-only)<br/>when derived-from-node" --> IT["InstanceType CRD"]
    IT --> ITR
    DEV --> ITR
    ITR -- "owns: existence + schedule labels<br/>+ isolation + admin Hold↔None sync;<br/>Devices-driven .status; recreate-on-delete;<br/>delete-then-wait teardown" --> CQ["ClusterQueue<br/>(isolated, no cohort)"]
    RF --> NQR
    NQR -- "owns: resource groups + HoldAndDrain<br/>+ AdmissionCheck ref; drain-on-delete;<br/>drain/empty when no live flavors" --> CQ
    CQ --> ITR
    CQ --> NQE
    NS["Namespace (non-system)"] --> NQE
    NQE -- "one per Namespace<br/>named gpustack-fnv64-HASH" --> LQ["LocalQueue"]
    DEV --> AC
    AC -- "per-accelerator feasibility<br/>Retry when over-admitted" --> CQ
```

### `NodeFlavorReconciler` (`node_flavor.go`)

Indexes managed nodes by `(key, os, arch, count)`, one `ResourceFlavor` per group. `spec.nodeLabels`
pins workloads — the feature key `{general.|acceleratable.}feature.gpustack.ai/${key}=true`, full
`kubernetes.io/os|arch` — plus a blanket `{Operator: Exists}` toleration, eligibility being by
nodeLabels, not taints.

Labels carry the pool identity (`.count`, `.capacity = contributing nodes × count`);
`note.gpustack.ai/*` annotations the per-accelerator VRAM and device descriptors — device information
only, no unit spec.

A flavor whose group has **no** contributing node is **deleted**; no drain-tombstone anymore. Identity
comes from the first contributing node — every contributor to a flavor name shares it — so there is no
min-capacity-node selection.

After syncing a flavor, and only under `instance-type-derived-from-node=true` (default), it **authors
the pool's `InstanceType`** — **create-only**, at the setting-correct name and identity
(`generalGroup`/`acceleratorGroup`/`acceleratable`/`os`/`arch`; the CPU key is the `generic` sentinel
when awareness is off) with the [default unit spec](#the-unit-spec-is-not-derived-from-node-capacity).

An existing type is untouched, admin- or operator-authored, leaving the `InstanceTypeReconciler` sole
owner of an InstanceType's lifecycle. It also **watches the types it authored and re-authors a deleted
one**, the safeguard the `InstanceTypeReconciler` applies to the backing queue.

> **Why nothing else would** — its own inputs never change when an output is destroyed, and the periodic
> informer resync is no fallback, since it re-delivers the flavor unchanged and the update filter drops
> it. A definition lost at runtime deletes every derived type at once, exactly that case.

### `InstanceTypeReconciler` (`instance_type.go`)

Owns the backing `ClusterQueue`'s **existence and metadata** and the materialized `InstanceType`'s
status — not its quota. It does **not** author InstanceTypes (the `NodeFlavorReconciler` does) and never
deletes one for lack of flavors.

`ensureClusterQueue` creates the name-identical queue when missing, stamping at creation the pool's
schedule labels (`nodefeature.PoolScheduleLabels`: the `feature.gpustack.ai/acceleratable` boolean, the
feature key(s) selected by `instance-type-aware-cpu-manufacturer`, `kubernetes.io/os|arch` — all from
the InstanceType **spec** identity) and the fixed no-borrow **isolation** (empty cohort, no
reclaim/borrow preemption).

It never fills the resource groups or references the AdmissionCheck (the `NodeQueueReconciler` owns
those), and prunes a stale feature-key label when the group/acceleratable changes so the re-pointed
queue's selectors match.

It watches the queue to keep `.status` fresh (the [four-view](admission.md#four-view-status) / CPU
projection + `status.entrance`, DeepEqual-guarded) and to **recreate a queue an admin deleted** while
the InstanceType still lives.

On delete, a `gpustack.ai/controlled` finalizer runs a **delete-then-wait teardown**: mark the type
`Inactive`, delete the queue once, hold the finalizer until Kueue has removed it. It does not drain; the
`NodeQueueReconciler` sees the deletion and drives that.

It also syncs `it.Spec.Inactive` with the queue's `StopPolicy` for the **admin `Inactive`** path: `Hold`
when `Inactive` (blocking new admission without evicting running workloads, never `HoldAndDrain`),
`None` when an admin reactivates, and `Inactive=true` backfilled one-way and stickily whenever the queue
is stopped by any means. So `Hold↔None` is owned here, `HoldAndDrain` by the `NodeQueueReconciler`.

### `NodeQueueReconciler` (`node_queue.go`)

Owns the backing `ClusterQueue`'s **quota and admission gating** — resource groups, the `HoldAndDrain`
drain policy (admin `Hold↔None` belongs to the `InstanceTypeReconciler`), the AdmissionCheck reference —
resolved from the pool's ResourceFlavors alone, never the owning InstanceType.

- **Groups** — from the live flavors, smallest per-node count first so Kueue packs small nodes first. An
  accelerated queue advertises only `credits.gpustack.ai/${manufacturer}` (nominal `capacity × M`; one
  whole accelerator = `M = 1,600,000` credits, so Kueue's int64 accounting never rounds fractional
  shared/sliced credits up to 1), a non-accelerated queue only CPU.
- **AdmissionCheck** — `gpustack-node-devices`, referenced on an accelerated derived queue once Active.
- **Finalizing flavor** — **absent**. Its nodes left, so `NodeFlavorReconciler` deleted it, but Kueue
  holds `resource-in-use` until no ClusterQueue references it, and dropping it from the groups is the
  update Kueue waits for. A workload on a dropped *partial-pool* flavor is evicted and re-admitted on
  the pool's remaining flavors — its node has left, so it must move regardless.
- **Queue being deleted** (admin delete or InstanceType teardown) — `HoldAndDrain` unconditionally, so
  Kueue evicts the admitted workloads and can drop its own finalizer and remove the queue; Kueue never
  evicts on delete by itself.
- **No live flavor left** while the queue carries quota — gated by
  `instance-type-drain-when-no-flavors` (default true): `HoldAndDrain`, requeue until every reservation
  clears, then empty the groups so Kueue's counters never go negative.

It **reactivates** (StopPolicy `None`) a queue *it* drained to empty — a `HoldAndDrain`, never an admin
`Hold` — once flavors return, though the `InstanceTypeReconciler`'s sticky `Inactive` backfill re-holds
it, so a recovered pool stays inactive until an admin clears `Inactive`. A drained queue also stops its
running Instances — see [Running-instance stop](admission.md#running-instance-stop).

### `NodeQueueEntranceReconciler` (`node_queue_entrance.go`)

Watches ClusterQueues and Namespaces, creating a `LocalQueue` in every non-system Namespace so workloads
can submit from anywhere.

Workloads reference it through the `kueue.x-k8s.io/queue-name` **label** (63-char limit) while
ClusterQueue names may be longer, so it is named `gpustack-fnv64-${fnv64a(ClusterQueue name)}` — always
31 characters — and records the full name in the `schedule.gpustack.ai/queue` annotation.

### `NodeDevicesAdmissionReconciler` (`node_devices_admission.go`)

The per-accelerator **AdmissionCheck**, third of the five gates; its behavior is in
[Admission](admission.md#gate-3--the-per-accelerator-admissioncheck).

---

**See also** — [Device Discovery](device-discovery.md) (where the capacity signals come from) ·
[Walkthrough](../walkthrough.md) (the same objects on a live cluster) ·
[Settings](../settings.md#online-adjustable-settings)

**Next** → [Admission](admission.md) — the five gates a request passes.
