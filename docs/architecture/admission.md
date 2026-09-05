# Admission

> **Purpose** — the five gates a workload passes before a container gets a device, the ledger beneath
> them, and how to read the capacity an `InstanceType` reports.
> **Audience** contributors, operators debugging a stuck workload · **Prerequisites**
> [Architecture](../architecture.md), [Scheduling Chain](scheduling-chain.md) · **Read time** ~8 min

## Contents

- [The five gates](#the-five-gates)
- [The `Devices` ledger beneath the gates](#the-devices-ledger-beneath-the-gates)
- [Four-view status](#four-view-status)
- [Capability versus availability](#capability-versus-availability)
- [The InstanceType and Instance webhooks](#the-instancetype-and-instance-webhooks)
- [The KV cache injection webhook is not a gate](#the-kv-cache-injection-webhook-is-not-a-gate)
- [Running-instance stop](#running-instance-stop)
- [Known behavior: the deployed Kueue Configuration](#known-behavior-the-deployed-kueue-configuration)

## The five gates

A layered **five-gate admission** model in which **Kueue is a coarse gate, not the ledger**. Each gate
produces or consumes the `.sliced.*` / `.partitioned.*` values at a distinct point on the path.

| # | Gate | What it can see | What it cannot |
|---|---|---|---|
| 1 | Pod webhook (Worker) | the request's shape; folds memory into credits | cluster-wide capacity |
| 2 | Kueue `credits` | the pool's aggregate total | per-accelerator fragmentation |
| 3 | `NodeDevicesAdmission` AdmissionCheck | every accelerator of the assigned flavor, via the ledger | which node the scheduler will pick |
| 4 | Default scheduler / kubelet | per-node remaining capacity keys | which accelerator, for the partitioned family |
| 5 | Device-plugin allocator | the live accelerator state, under a per-node mutex | anything upstream of its own node |

### Gate 1 — the Pod webhook

A `pods` CREATE webhook (objectSelector `kueue.x-k8s.io/queue-name`, `failurePolicy: Fail`) enforces the
[normative request rules](../accelerator-requests.md#the-request-rules) and folds each family's credit
input:

- **logical slice** — `.sliced.memory-percentage` / `.sliced.memory-mib` into `.sliced.units`: the
  memory demand over the pool InstanceType's per-accelerator `Memory`;
- **hardware partition** — the profile's VRAM into `.partitioned.units` by that same VRAM-anchored fold,
  so a `3g.40gb` partition costs what a same-VRAM slice costs.

Memory thus reaches the `credits` fold-down before Kueue scores it. The webhook defaults
`.sliced.cores-percentage` to 100, prefers `.sliced.memory-percentage` over `.sliced.memory-mib`, and
always recomputes the fold, since no trusted path sets it.

It then validates the [seven request rules](../accelerator-requests.md#the-request-rules), which that
page states with an accepted and a rejected example each.

The divisor is the operator-owned **InstanceType**'s `spec.memory`, found via the
`schedule.gpustack.ai/queue-entrance` label — **never** the user-writable LocalQueue. Its
`MutatingWebhookConfiguration` sorts before `kueue-mutating-webhook-configuration`, so the fold precedes
Kueue's resource hash.

### Gate 2 — Kueue `credits`

Coarse total admission by fractional scoring (`capacity × M`); a scalar total cannot see per-accelerator
fragmentation.

### Gate 3 — the per-accelerator AdmissionCheck

`NodeDevicesAdmissionReconciler` (`node_devices_admission.go`) introspects the assigned ResourceFlavor's
nodes through the `Devices` ledger, closing the credits "over-admit exclusive" gap: a scalar total cannot
see that 8 accelerators each 50 %-sliced satisfy no 5-exclusive request.

The worker's `Prepare()` applies the `gpustack-node-devices` AdmissionCheck last at startup, retrying
until Kueue's CRD is established — the chart cannot ship it, since Kueue templates its own CRDs and
nothing orders them ahead of a custom resource in the same render (see [Install
modes](installation-modes.md#the-chart-deploys-workloads-the-worker-applies-the-custom-resources)).

This reconciler keeps it `Active`; the accelerated queue references it in `spec.admissionChecksStrategy`
only once it is ([`NodeQueueReconciler`](scheduling-chain.md#nodequeuereconciler-node_queuego)).

Once Kueue reserves quota, the check reads the pool's `Devices` ledger (uncached, via `APIReader`) for
per-accelerator `Remaining ≥ demand`: a whole accelerator for exclusive, `.sliced.units` for sliced, an
owner share for shared, a free placement of the profile for a partition.

Each family gets one correlated `(accelerators, per-accelerator demand, profile)` tuple scoped to the
accelerators that can serve it, so an exclusive or shared request is never judged feasible against a
partitioned accelerator: it stays queued, not admitted into a permanent `Pending`. For partitions
`accelerators` counts *instances*, not accelerators — one hosts as many replicas as its remaining
geometry allows.

**Feasibility is per role.** A Workload composed from a pod group carries one PodSet per role, and
[each PodSet gets its own ResourceFlavor](scheduling-chain.md#stage-4-the-kueue-chain) — so a demand
is judged against the accelerators of the flavor **its own** PodSet was assigned, never against every
accelerator in the pool.

- Two PodSets' demands merge only when the flavor agrees as well as the family.
- An accelerator is eligible for a demand only when its own key is one that demand's flavor covers.
- The per-accelerator budget stays global to the Workload, so two roles on one flavor cannot spend one
  accelerator twice.
- The verdict message names the role that is starved.

The ledger seeds every accelerator at `M`, so an exclusive over-admit coarse `credits` let through is
caught exactly and held with `Retry`, transient and self-healing once Kueue re-admits after the backoff.
A **check-only** gate: never preempts, never `Rejected`.

> **Why it skips an evicted Workload** — Kueue resets the checks to `Pending` and drops the quota
> reservation in two separate writes. Between them an evicted Workload still reports a reservation, so
> a verdict written there overwrites the reset — and whichever writer wins is final:
>
> - Kueue stops resetting while the eviction condition is set;
> - its scheduler will not reserve quota while a check is `Retry`;
> - this reconciler stops evaluating without a reservation.
>
> So the backoff loop deadlocks instead of self-healing. Re-reserving quota clears the eviction
> condition and re-opens evaluation.

### Gate 4 — default scheduler / kubelet

Node-level counting of each family's remaining keys — the bare `.sliced` / `.partitioned` token
plus its [logical](scheduling-chain.md#logical-slicing-capacities) or
[partitioned](scheduling-chain.md#hardware-partitioning-capacities) counting keys — picks the
best-fitting node of that ResourceFlavor.

Disjoint accelerator populations advertise the two families' keys, so the resource name alone rules out
a node that cannot serve the kind at all — the one placement error `Allocate` can never repair.

### Gate 5 — the device-plugin allocator

At `Allocate`, the Device Manager settles the accelerator, injects the container's visibility env and
runtime isolation, and records the allocation in the `Devices` ledger. "Settles" differs by family:

- **accelerator-bound** — the kubelet chose the accelerator by choosing the token, so the allocator only
  refuses one another mode holds;
- **partitioned** — the tokens are a fungible count, so the allocator picks the accelerator itself and
  materializes the hardware instance on it.

Both paths, and the per-manufacturer isolation each slice gets — it covers every sliceable manufacturer
— are in [Device Discovery](device-discovery.md#the-device-plugin-allocator). The Pod webhook caps
`.sliced` and `.partitioned` at exactly **1**, so the manufacturer-specific multi-slice divergence that
cap hid can no longer be requested.

## The `Devices` ledger beneath the gates

The `Devices` CR `AcceleratorAllocation` ledger is the single authoritative accounting, written below the
kubelet by the device-plugin `Allocate` for every allocation, Kueue-routed or not. It drives the
four-view and backs gate 3, but is not itself a gate.

## Four-view status

`InstanceType.status` carries four per-accelerator bin-packing projections from the `Devices` ledger, not
a credits fold-down:

| View | Column | What it counts |
|---|---|---|
| `Accelerator` | **EX** | free whole accelerators |
| `AcceleratorShared` | **SH** | shareable ownership slots |
| `AcceleratorSliced` | **SL** | logically sliceable VRAM-percent units |
| `AcceleratorPartitioned` | **PT** | hardware partition instances the pool's partitioned accelerators can still host |

Each accelerator feeds **exactly one** group, by the capability it reports and never its scalar ledger:
`EX`/`SH`/`SL` count unpartitioned accelerators, `PT` partitioned ones. A partition-only pool thus reads
`0/0 0/0 0/0` for the first three, a logical-only pool `0/0` for `PT`, and an exclusive tenant is never
shown capacity a partitioned accelerator could not serve.

`OnceMaxRequest` differs per view:

- `EX`, `SH` — the largest single *node*'s availability; one request can span a node's accelerators;
- `SL` — the freest single *accelerator*'s; a slice targets one accelerator;
- `PT` — `1` while any accelerator can host an instance, else `0`; a partition request is validated to
  be exactly one instance on one accelerator, so nothing larger is requestable.

`kubectl get instancetypes` folds them into the `Accelerator(EX/SH/SL/PT)` column as four
`onceMaxRequest/remaining` groups.

## Capability versus availability

**Which field answers "what can I still get".** Neither `PT` number, for a partitioned pool:
`onceMaxRequest` is only the `1`/`0` "is there room at all", and `remaining` is a best case over profiles
competing for the same physical slices, each accelerator contributing its largest per-profile free count,
never a per-profile total.

The per-profile answer is `status.acceleratorPartitioned.remainingProfiles`, paired with
`allocatedProfiles`: the pool-level Σ by profile name of the per-accelerator ledger on
`Devices.status`. **Every profile the pool offers is listed even at zero**, so one a sibling's instance
filled reads `0` instead of vanishing, keeping "offered but full" distinct from "not offered".

`kubectl get instancetypes -o wide` shows the same list as the `PARTITIONS` column; the worker gateway
sums it across clusters (Active members only, like every availability dimension).

**Do not read `status.detail.slicedDetail` for this.** The static slicing **capability** catalog,
aggregated from the `Devices` **spec** side, by design does not move as instances are carved and
released. The Instance webhook needs the whole catalog: to reject an unoffered profile while naming the
offered set, and to size a request from its `MemoryMib`, which the ledger lacks.

Repurposing those counts as availability would make a momentarily-full profile vanish from the offered
set, turning a request that should stay `Retry` at the AdmissionCheck into a permanent rejection.

The partition views are likewise enumerated from the capability side, scoped to the pool's own
accelerator group (a node can carry several models) and joined to the ledger. An accelerator the detector
reported is never dropped for a missing ledger row, nor read as full for an empty one: it falls back to
its catalog ceilings, as the node's per-profile capacity keys do.

> **Why the status lives on a real CRD** — the reconciler watches the `Devices` CR and writes into a
> real CRD's `.status`, so `kubectl get instancetype -w` observes capacity move as pods allocate and
> free. A read-only projection over the ClusterQueue could not: it borrows the CQ `resourceVersion`,
> unchanged on a `Devices`-only allocation.

The v1 (`worker.gpustack.ai/v1`) InstanceType is a thin proxy + conversion over the real `v1alpha1` CRD.

## The InstanceType and Instance webhooks

The unit spec lives **only** on the InstanceType: a derived type is stamped with its [per-product
preset](scheduling-chain.md#the-unit-spec-is-not-derived-from-node-capacity) at creation, and an admin
edit touches only the InstanceType, never a Node or the ClusterQueue notes.

- **InstanceType validating, create** — requires the complete input, read independently of any editable
  setting: `acceleratorGroup` (only when `acceleratable`), `os`, `arch`, the unit triple
  (`unitResources.cpu`/`.ram` + `localStorage`); empty or partial is rejected. A CPU-only
  (`acceleratable=false`) type's unit CPU must be exactly 1 core, an accelerated type any unitless
  positive integer.
- **InstanceType validating, update** — **freezes the spec**: every field immutable except
  `displayName` (rename) and `inactive` (in/out of service), so re-sizing or re-pointing a pool means
  delete and re-create. Only immutability is re-checked, never the create-time shape, so a legacy type
  stored before a tightened rule can still be renamed or deactivated.
- **InstanceType defaulting** — an empty `generalGroup` to the `generic` sentinel; the pool's schedule
  labels (`nodefeature.PoolScheduleLabels`, grouped by `instance-type-aware-cpu-manufacturer`) and
  `schedule.gpustack.ai/queue-entrance` (`nodefeature.FormatLocalQueueName(name)`); descriptors enriched
  from a matching ResourceFlavor; and when awareness is on, that flavor's `cpuDetail` note folded into
  `spec.cpu` (generic) or `spec.accelerator.cpu` (accelerated).
- **Instance validating** — enforces the unit spec on **Create and Update**: a submission's RAM must not
  exceed `unitRAM × count`, its local storage not the InstanceType's `LocalStorage`.

## The KV cache injection webhook is not a gate

A second mutating webhook on Pods writes the client configuration an inference engine needs to use a
[KV cache pool](../reference/kv-cache-injection.md). It sits **outside** the five gates:

- It admits or refuses on its own inputs, consumes no `.sliced.*` or `.partitioned.*` value, produces
  none, and never touches `resources`.
- It cannot be a branch of gate 1, because the two select on independent criteria: gate 1 fires on
  `kueue.x-k8s.io/queue-name`, this one on `kvcache.gpustack.ai/inject`, and a `LabelSelector` cannot
  express the union. Independent, not disjoint — a Pod may carry both labels and be served by both
  entries, which is exactly what the next point is about.
- Both entries live in the single `gpustack-worker-mutation` configuration, whose name sorts before
  Kueue's on purpose. Their order within it is immaterial, which a test asserts by running both over
  one Pod in both orders.

## Running-instance stop

Before (re)creating an Instance's Pod, the `InstanceReconciler` (`instance.go`) reads the backing
`ClusterQueue`'s `StopPolicy` and **stops** the Instance (`spec.stop=true`) rather than recreate a Pod
the queue can never admit — when the queue is `HoldAndDrain` (a pool drain or a teardown evicting
admitted workloads), or the `InstanceType` is being deleted or gone.

An admin `Hold` (the `Inactive` switch) is deliberately **not** a stop: running Pods keep running, a new
Instance stays pending.

> **Why it keys on `StopPolicy`** — the InstanceType phase collapses both `Hold` and a fully-drained
> `HoldAndDrain` to `Inactive`, and a fast drain clears the reservation before a durable `Draining`
> phase is ever observed.

A `ClusterQueue` watch (on `StopPolicy`) re-enqueues the type's Instances so the stop is prompt even when
no Pod event fires; the `InstanceType` watch is narrowed to the deletion signal for a prompt teardown
stop.

## Known behavior: the deployed Kueue Configuration

The feature gate `AssignQueueLabelsForPods` is disabled in the deployed Kueue Configuration
(`kueue.managerConfig.controllerManagerConfigYaml` in the chart's `values.yaml`), so Kueue never copies
cluster/local queue names onto Pod labels; long ClusterQueue names would not fit a label value.

It also sets `resources.quotaCheckStrategy: IgnoreUndeclared`, so a single-dimension queue (only `cpu`,
or only the manufacturer `credits`) does not reject a Workload for the Pod resources it does not cover
(`memory`/`ephemeral-storage`). Its `resources.transformations` list is generated from `pkg/nodefeature`
by `make generate chart`.

---

**See also** — [Accelerator Requests](../accelerator-requests.md) (the normative request contract) ·
[Device Discovery](device-discovery.md#the-device-plugin-allocator) (gate 5 in detail) ·
[Walkthrough](../walkthrough.md) (the four views moving on a live cluster)

**Next** → [Installation Modes](installation-modes.md) — how the chain gets deployed.
