# Admission

> **Purpose** — the five gates a workload passes before a container gets a device, the ledger beneath
> them, and how to read the capacity an `InstanceType` reports.
> **Audience** contributors, operators debugging a stuck workload · **Prerequisites**
> [Architecture](../architecture.md), [Scheduling Chain](scheduling-chain.md) · **Read time** ~15 min

## Contents

- [The five gates](#the-five-gates)
- [The `Devices` ledger beneath the gates](#the-devices-ledger-beneath-the-gates)
- [Four-view status](#four-view-status)
- [Capability versus availability](#capability-versus-availability)
- [The InstanceType and Instance webhooks](#the-instancetype-and-instance-webhooks)
- [Running-instance stop](#running-instance-stop)
- [Known behavior: the deployed Kueue Configuration](#known-behavior-the-deployed-kueue-configuration)

## The five gates

Together these form a layered **five-gate admission** model where **Kueue is a coarse gate, not the
ledger** — each gate produces or consumes the `.sliced.*` / `.partitioned.*` values at a distinct point
along the path.

| # | Gate | What it can see | What it cannot |
|---|---|---|---|
| 1 | Pod webhook (Worker) | the request's shape; folds memory into credits | cluster-wide capacity |
| 2 | Kueue `credits` | the pool's aggregate total | per-accelerator fragmentation |
| 3 | `NodeDevicesAdmission` AdmissionCheck | every accelerator of the assigned flavor, via the ledger | which node the scheduler will pick |
| 4 | Default scheduler / kubelet | per-node remaining capacity keys | which accelerator, for the partitioned family |
| 5 | Device-plugin allocator | the live accelerator state, under a per-node mutex | anything upstream of its own node |

### Gate 1 — the Pod webhook

A `pods` CREATE webhook (objectSelector on `kueue.x-k8s.io/queue-name`, `failurePolicy: Fail`) enforces
the [normative request rules](../accelerator-requests.md#the-request-rules) and folds each family's
credit input:

- for a **logical slice**, `.sliced.memory-percentage` / `.sliced.memory-mib` into `.sliced.units`,
  dividing the memory demand by the pool InstanceType's per-accelerator `Memory`;
- for a **hardware partition**, the requested profile's VRAM into `.partitioned.units` through the same
  VRAM-anchored fold, so a `3g.40gb` partition and a same-VRAM logical slice cost identical credits.

Either way the memory participates in the Kueue `credits` fold-down before Kueue ever scores it. It
also defaults `.sliced.cores-percentage` to 100; `.sliced.memory-percentage` wins over
`.sliced.memory-mib`, and the folded value is always recomputed, since no trusted path sets it.

It then validates the request against the seven normative rules: one family per Pod in exactly one
container group, `.sliced` / `.partitioned` / every per-profile key exactly 1, one profile shape, at
most one slicing container, and no accelerator on a restartable init container. The rules, with an
accepted and a rejected example each, are in [Accelerator
Requests](../accelerator-requests.md#the-request-rules).

The per-accelerator VRAM divisor for the fold is read from the operator-owned **InstanceType**'s
`spec.memory` (reverse-looked-up by the `schedule.gpustack.ai/queue-entrance` label), **never** from
the user-writable LocalQueue. Its `MutatingWebhookConfiguration` name sorts before
`kueue-mutating-webhook-configuration` so the fold runs before Kueue hashes container resources.

### Gate 2 — Kueue `credits`

Coarse total admission by fractional scoring (`capacity × M`); ensures the pool has enough aggregate
capacity, but a scalar total cannot see per-accelerator fragmentation.

### Gate 3 — the per-accelerator AdmissionCheck

`NodeDevicesAdmissionReconciler` (`node_devices_admission.go`) introspects every node of the
assigned ResourceFlavor through the `Devices` ledger for per-accelerator feasibility, closing the
credits "over-admit exclusive" gap (a scalar total cannot see that 8 accelerators each 50 %-sliced
satisfy no 5-exclusive request).

The worker's `Prepare()` applies the `gpustack-node-devices` AdmissionCheck object as its last startup
step, retrying until Kueue's CRD is established (the chart cannot ship it — Kueue templates its own
CRDs, so nothing orders them ahead of a custom resource in the same render, see [Install
modes](install-modes.md#the-chart-deploys-workloads-the-worker-applies-the-custom-resources)); the
reconciler keeps it `Active`, and the `NodeQueueReconciler` makes the accelerated queue reference it in
`spec.admissionChecksStrategy` **only once it is Active**.

After Kueue reserves quota, the check reads the assigned pool's `Devices` ledger (uncached, via
`APIReader`) and computes per-accelerator feasibility (`Remaining ≥ demand`: a whole accelerator for
exclusive, `.sliced.units` for sliced, an owner share for shared, and — for a partition request — a
free placement of the requested profile).

It carries one correlated `(accelerators, per-accelerator demand, profile)` demand tuple **per
family** and scopes every family to the accelerators that can serve it, so an exclusive or shared
request is never judged feasible against a partitioned accelerator and is left queued instead of
being admitted into a permanent `Pending`. On the partition side a request's `accelerators` term
counts *instances*, not distinct accelerators: one accelerator hosts as many replicas as its
remaining geometry allows.

Since the ledger seeds every accelerator at `M`, an exclusive over-admit that coarse `credits` let
through (a scalar total can hide that no *single* accelerator is free) is caught exactly and held
with `Retry` — a transient state that self-heals when Kueue re-admits after the backoff.

> **Why it skips an evicted Workload** — that self-healing is the reason. Kueue resets the checks to
> `Pending` and drops the quota reservation in two separate writes, so between them an evicted Workload
> still reports a reservation, and a verdict written into that window overwrites the reset. Whichever
> of the two writers wins is then final — Kueue stops resetting checks while the eviction condition is
> set, its scheduler will not reserve quota while a check is `Retry`, and without a reservation this
> reconciler stops evaluating — so the backoff loop would deadlock instead of self-healing.
> Re-reserving quota clears the eviction condition, which is what re-opens evaluation.

This is a **check-only** gate: it never preempts and never `Rejected`.

### Gate 4 — default scheduler / kubelet

Node-level counting of the remaining `.sliced` / `.sliced.units` / `.sliced.cores-percentage` /
`.sliced.memory-*` capacities (logical) or `.partitioned` / `.partitioned.units` /
`.partitioned.<kind>-<profile>` capacities (physical) picks the best-fitting node among that
ResourceFlavor's nodes.

Because the two families' keys are advertised by disjoint accelerator populations, the resource name
alone rules out a node that cannot serve the kind at all — the one placement error `Allocate` can
never repair.

### Gate 5 — the device-plugin allocator

At `Allocate`, the Device Manager settles the accelerator, injects the container's visibility env
and runtime isolation, and records the allocation into the `Devices` ledger. What "settles" means
differs by family: for the accelerator-bound families the kubelet already chose the accelerator by
choosing the token, and the allocator only refuses one another mode holds; for the **partitioned**
family the tokens are a fungible count, so the allocator picks the accelerator itself and
materializes the hardware instance on it. Both paths, and the per-manufacturer isolation each slice
gets, are in [Device Discovery](discovery.md#the-device-plugin-allocator).

Per-slice runtime isolation covers every sliceable manufacturer. `.sliced` and `.partitioned` are
both capped at exactly **1** by the Pod webhook, so the manufacturer-specific multi-slice divergence
that cap used to hide can no longer be requested at all.

## The `Devices` ledger beneath the gates

The `Devices` CR `AcceleratorAllocation` ledger is the single authoritative accounting, written below
the kubelet by the device-plugin `Allocate` for every allocation (Kueue-routed or not); it drives the
four-view and is the store beneath gate 3, not a numbered gate itself.

## Four-view status

`InstanceType.status` carries four per-accelerator bin-packing projections computed from the
`Devices` ledger (not a credits fold-down):

| View | Column | What it counts |
|---|---|---|
| `Accelerator` | **EX** | free whole accelerators |
| `AcceleratorShared` | **SH** | shareable ownership slots |
| `AcceleratorSliced` | **SL** | logically sliceable VRAM-percent units |
| `AcceleratorPartitioned` | **PT** | hardware partition instances the pool's partitioned accelerators can still host |

Each accelerator feeds **exactly one** of the two groups, decided by the capability it reports and
never by its scalar ledger: `EX`/`SH`/`SL` count unpartitioned accelerators only, `PT` partitioned
accelerators only. So a partition-only pool reads `0/0 0/0 0/0` for the first three, a logical-only
pool reads `0/0` for `PT`, and an exclusive tenant is never shown capacity a partitioned accelerator
could not actually serve.

`EX` and `SH`'s `OnceMaxRequest` is the largest single *node*'s availability (one request can span a
node's accelerators); `SL`'s is the freest single *accelerator*'s, since a slice targets one
accelerator; and `PT`'s is `1` while any accelerator can still host an instance, else `0`, because a
partition request is validated to be exactly one instance on exactly one accelerator, so no larger
value is ever requestable. `kubectl get instancetypes` folds them into the
`Accelerator(EX/SH/SL/PT)` column as four `onceMaxRequest/remaining` groups.

## Capability versus availability

**Which field answers "what can I still get".** Neither `PT` number can answer it for a
hardware-partitioned pool: its `onceMaxRequest` is only the `1`/`0` "is there room at all", and its
`remaining` is a best case over profiles that compete for the same physical slices — each
accelerator contributes its largest per-profile free count, never a per-profile total.

The per-profile answer is `status.acceleratorPartitioned.remainingProfiles`, paired with
`allocatedProfiles` — the pool-level Σ, by profile name, of the per-accelerator ledger on
`Devices.status`. **Every profile the pool offers is listed even at zero**, so a profile whose room
a sibling's instance consumed reads `0` instead of vanishing and "offered but currently full" stays
distinguishable from "not offered at all"; `kubectl get instancetypes -o wide` shows the same list
as the `PARTITIONS` column, and the worker gateway sums it across clusters (Active members only,
like every other availability dimension).

**Do not read `status.detail.slicedDetail` for this.** It is the static slicing **capability** catalog,
aggregated from the `Devices` **spec** side, and by design does not move as instances are carved and
released — the Instance webhook needs the whole catalog both to reject an unoffered profile while
naming the offered set and to size a request from that profile's `MemoryMib`, which the ledger does not
carry. Repurposing the catalog's counts as availability would make a momentarily-full profile disappear
from the offered set, turning a request that should stay `Retry` at the AdmissionCheck into a permanent
rejection.

Symmetrically, the partition views are enumerated from the capability side, scoped to the pool's own
accelerator group (a node can carry several models) and joined to the ledger, so an accelerator the
detector has reported is never dropped for lacking a ledger row nor read as full for carrying an
empty one: either way it falls back to its catalog ceilings, the same fallback the node's
per-profile capacity keys take.

> **Why the status lives on a real CRD** — because the reconciler watches the `Devices` CR and writes
> the result into a real CRD's `.status`, a `kubectl get instancetype -w` observes capacity move as pods
> allocate and free. A read-only projection over the ClusterQueue could not: it borrows the CQ
> `resourceVersion`, which is unchanged on a `Devices`-only allocation.

The v1 (`worker.gpustack.ai/v1`) InstanceType is a thin proxy + conversion over the real `v1alpha1`
CRD.

## The InstanceType and Instance webhooks

The unit spec lives **only** on the InstanceType — a derived type is stamped with its per-product preset
at creation, and an admin edit changes only the InstanceType (never a Node or the ClusterQueue notes).

**The InstanceType validating webhook requires the complete input on create** — `acceleratorGroup`
(only when `acceleratable`) / `os` / `arch` plus the unit triple (`unitResources.cpu`/`.ram` +
`localStorage`; an empty or partial spec is rejected), read independently of any editable setting, and
a **CPU-only (`acceleratable=false`) type's unit CPU must be exactly 1 core** (an accelerated type keeps
any unitless positive integer).

**It freezes the spec on update**: every field is **immutable except `displayName` (rename) and
`inactive` (take in/out of service)**, so to re-size or re-point a pool you delete and re-create the
type. Create-time shape checks are not re-run on update (only immutability is), so a legacy type stored
before a tightened rule can still be renamed or deactivated.

**The defaulting webhook** defaults an empty `generalGroup` to the `generic` sentinel, stamps the pool's
schedule labels (`nodefeature.PoolScheduleLabels`, grouped by `instance-type-aware-cpu-manufacturer`)
and the `schedule.gpustack.ai/queue-entrance` label (`nodefeature.FormatLocalQueueName(name)`), enriches
the descriptors from a matching ResourceFlavor, and — when awareness is on — folds that flavor's
`cpuDetail` note into `spec.cpu` (generic) or `spec.accelerator.cpu` (accelerated).

**The Instance validating webhook** enforces the unit spec on **Create and Update**: a submission's RAM
must not exceed `unitRAM × count` and its local storage must not exceed the InstanceType's
`LocalStorage`.

## Running-instance stop

Before (re)creating an Instance's Pod, the `InstanceReconciler` (`instance.go`) reads the backing
`ClusterQueue`'s `StopPolicy` and **stops** the Instance (`spec.stop=true`) — rather than recreating a
Pod the queue can never admit — when that queue is `HoldAndDrain` (a pool drain or a teardown that
evicts admitted workloads), or when the `InstanceType` is being deleted or gone.

An admin `Hold` (the `Inactive` switch) is deliberately **not** a stop: already-running Pods keep
running and a new Instance simply stays pending.

> **Why it keys on `StopPolicy`** — the InstanceType phase cannot drive this: it collapses both `Hold`
> and a fully-drained `HoldAndDrain` to `Inactive`, and a fast drain clears the reservation before a
> durable `Draining` phase is ever observed.

A `ClusterQueue` watch (on its `StopPolicy`) re-enqueues the type's Instances so the stop is prompt even
when no Pod event fires; the `InstanceType` watch is narrowed to the deletion signal for a prompt
teardown stop.

## Known behavior: the deployed Kueue Configuration

The Kueue feature gate `AssignQueueLabelsForPods` is disabled in the deployed Kueue Configuration
(`kueue.managerConfig.controllerManagerConfigYaml` in the chart's `values.yaml`), so Kueue never copies
cluster/local queue names onto Pod labels — long ClusterQueue names would not fit a label value.

That Configuration also sets `resources.quotaCheckStrategy: IgnoreUndeclared`, so a single-dimension
queue (only `cpu`, or only the manufacturer `credits`) does not reject a Workload for the other Pod
resources (`memory`/`ephemeral-storage`) it does not cover, and its `resources.transformations` list is
generated from `pkg/nodefeature` by `make generate chart`.

---

**See also** — [Accelerator Requests](../accelerator-requests.md) (the normative request contract) ·
[Device Discovery](discovery.md#the-device-plugin-allocator) (gate 5 in detail) ·
[Walkthrough](../walkthrough.md) (the four views moving on a live cluster)

**Next** → [Two install modes](install-modes.md) — how the chain gets deployed.
