# Admission

A workload passes five admission gates before a container receives a device. Kueue accounts for
pool capacity, while GPUStack checks whether individual accelerators can satisfy the request.

## Contents

- [The five gates](#the-five-gates)
- [The `Devices` ledger beneath the gates](#the-devices-ledger-beneath-the-gates)
- [Four-view status](#four-view-status)
- [Capability versus availability](#capability-versus-availability)
- [The InstanceType and Instance webhooks](#the-instancetype-and-instance-webhooks)
- [The KV cache injection webhook](#the-kv-cache-injection-webhook)
- [Update validation while an object is deleted](#update-validation-while-an-object-is-deleted)
- [Running-instance stop](#running-instance-stop)
- [Known behavior: the deployed Kueue Configuration](#known-behavior-the-deployed-kueue-configuration)

## The five gates

A layered five-gate admission model: Kueue is a coarse gate, and the per-accelerator ledger is the
fine one. Each gate produces or consumes the `.sliced.*` / `.partitioned.*` values at a distinct
point on the path.

| # | Gate | Sees | Cannot see |
|---|---|---|---|
| 1 | Pod webhook (Worker) | the request's shape; folds memory into credits | cluster-wide capacity |
| 2 | Kueue `credits` | the pool's aggregate total | per-accelerator fragmentation |
| 3 | `NodeDevicesAdmission` AdmissionCheck | the accelerators of the node Kueue TAS assigned, via the ledger | how to move a Workload off that node: for an unpinned Workload, TAS re-places a `Retry` from the same per-node totals |
| 4 | Default scheduler / kubelet | per-node remaining capacity keys | which accelerator, for the partitioned family |
| 5 | Device-plugin allocator | the live accelerator state, under a per-node mutex | anything upstream of its own node |

### Gate 1 — the Pod webhook

A `pods` CREATE webhook (objectSelector `kueue.x-k8s.io/queue-name`, `failurePolicy: Fail`) enforces the
[normative request rules](/gpustack-operator/v0.9.0/docs/modules/devices/requests/index.md#the-request-rules) and folds each family's credit
input:

- **logical slice** — `.sliced.memory-percentage` / `.sliced.memory-mib` into `.sliced.units`: the
  memory demand over the pool InstanceType's per-accelerator `Memory`;
- **hardware partition** — the profile's VRAM into `.partitioned.units` by that same VRAM-anchored fold,
  so a `3g.40gb` partition costs what a same-VRAM slice costs.

Memory thus reaches the `credits` fold-down before Kueue scores it. The webhook defaults
`.sliced.cores-percentage` to 100, prefers `.sliced.memory-percentage` over `.sliced.memory-mib`, and
always recomputes the fold, since no trusted path sets it.

It then validates the [seven request rules](/gpustack-operator/v0.9.0/docs/modules/devices/requests/index.md#the-request-rules), which that
page states with an accepted and a rejected example each.

The divisor is the operator-owned **InstanceType**'s `spec.memory`, found via the
`schedule.gpustack.ai/queue-entrance` label — **never** the user-writable LocalQueue. Its
`MutatingWebhookConfiguration` sorts before `kueue-mutating-webhook-configuration`, so the fold precedes
Kueue's resource hash.

### Gate 2 — Kueue `credits`

Coarse total admission by fractional scoring (`capacity × M`); a scalar total cannot see per-accelerator
fragmentation.

### Gate 3 — the per-accelerator AdmissionCheck

When Kueue reserves quota, the `gpustack-node-devices` AdmissionCheck reads the `Devices` ledger of
every node the assigned ResourceFlavor names and checks the fit per accelerator. The `credits` gate
before it sees only a scalar total, so it cannot tell that eight accelerators sliced to 50 % each
cannot serve a request for five whole accelerators. This check can, and holds
the Workload with `Retry` when the fit fails.

The worker applies the `gpustack-node-devices` AdmissionCheck at startup, retrying
until Kueue's CRD is established — the chart cannot ship it, since Kueue templates its own CRDs and
nothing orders them ahead of a custom resource in the same render (see [Install
modes](../../operate/installation-modes.md#chart-deployed-and-worker-applied-resources)).

The worker keeps it `Active`; an accelerated queue references it in `spec.admissionChecksStrategy`
only once it is ([Queue quota and draining](/gpustack-operator/v0.9.0/docs/modules/devices/scheduling/index.md#queue-quota-and-draining)).

Once Kueue reserves quota, the check reads each node's `Devices` ledger for per-accelerator
`Remaining ≥ demand`: a whole accelerator for exclusive, `.sliced.units` for sliced, an owner share for
shared, a free placement of the profile for a partition. It rebuilds that ledger from the Pods bound to
the node, one uncached read, with the device manager's own aggregation.

**A Workload answered `Ready` holds its accelerators before its Pods do.** The ledger moves only when
`Allocate` records a Pod, after Kueue admits the Workload, the scheduler binds its Pods and the kubelet
admits them. A second Workload judged in that window would see the first one's accelerators as free.

- So the check first fits every other Workload on the node that is admitted or holds this check's
  `Ready`: the Pods its topology assignment puts there, minus those the rebuilt ledger already holds.
- A Pod is held once its allocation record names every container asking for an accelerator. The
  `kueue.x-k8s.io/workload` annotation and `kueue.x-k8s.io/podset` label tie it to its Workload.
- The ledger and the held Pods come from the same read, so no Pod is counted twice or missed.
- A finished Pod, in phase `Succeeded` or `Failed`, still counts as held, while the ledger stops
  charging the cards the kubelet took back from it ([device discovery](/gpustack-operator/v0.9.0/docs/modules/devices/discovery/index.md#container-identification-and-cross-mode-exclusion)).
  A replacement Pod its Workload creates is then counted by neither until `Allocate` records it.
- The check judges one Workload at a time, and remembers each `Ready` it wrote until the cache shows it.

> **Known behavior: the inflight count follows the allocator's hint.** An inflight slice is fitted on
> the accelerator the allocator's packing order prefers, which the kubelet normally takes. A kubelet
> that ignores the hint can put it elsewhere, and a slice judged after it can then find no room on
> that accelerator: `Allocate` refuses it and the Pod fails with `UnexpectedAdmissionError`. A Workload
> without a hostname-level assignment is not counted as inflight; every queue this operator derives
> assigns one.

Slices share an accelerator the way the allocator packs them: each is charged its `.sliced.units` and
one of the accelerator's slice slots on the fullest accelerator that still fits it, so two 30 % slices
fit one free accelerator and two 60 % slices do not. Only a free or already-sliced accelerator takes one.

The slots are counted from the ledger's `allocatedSlices` as well as this Workload's own slices, so a
Hygon accelerator holding its four slices takes no fifth however much memory it has left.

**Under TAS the check judges the node already assigned.** Every queue this operator derives is TAS-only
and ends at `kubernetes.io/hostname`, so Kueue writes the node into the Workload's
`podSetAssignments[].topologyAssignment` as it reserves quota, before any AdmissionCheck settles. Each
node named there must host its own share of the PodSet from its own accelerators.

- A PodSet without a hostname-level assignment is judged across the flavor's whole node pool.
- A node whose `Node` object is gone serves no node-scoped demand, so the Workload is held.

> **Known behavior: a fragmented node is skipped only when its Workload is pinned.** TAS sums each
> node's capacity keys, so it cannot see how the free capacity is spread over the node's accelerators:
>
> - a node's free `.sliced.units` may be spread over accelerators none of which fits the slice;
> - a node's 10 shared tokens per accelerator let a node with too few accelerators take `.shared: N`.
>   The Pod webhook's card-count pin keeps such a request off those nodes and their flavors. Where the
>   pin misses ([Limitations](/gpustack-operator/v0.9.0/docs/modules/devices/requests/index.md#limitations)) it never frees, since Kueue
>   restarts the flavor scan at the smallest node after every eviction.
>
> Left alone, TAS prefers such a node, the check answers `Retry`, and TAS places the Workload there
> again every 30 s. The [per-card fit labels](/gpustack-operator/v0.9.0/docs/modules/devices/scheduling/index.md#per-card-fit-labels) close that
> loop for a pinned Workload: TAS skips the node, and when no node fits, the Workload stays pending
> on `excluded: affinity` and holds no quota.
>
> Reading the pool instead let such a request through to `Allocate`, which refuses a slice its
> accelerator cannot hold and a shared request its node cannot spread over distinct accelerators,
> failing the Pod.

The Workload fit pin: a mutating webhook on Kueue `Workload` CREATE and UPDATE adds the pin to each
PodSet, so a logical slice of `U` units per accelerator gets `sliced-max-free-units… Gt U-1`, and a
shared request of `N >= 2` accelerators gets `shared-free-cards… Gt N-1`. `N` is the largest count
any one container asks for.

- It changes only a Workload whose LocalQueue points at a ClusterQueue carrying this operator's
  InstanceType mark, with a same-named InstanceType naming an accelerator group, so other tenants'
  Workloads in a shared Kueue are out of its reach.
- It acts on an UPDATE only while the old Workload holds no quota reservation, which is when Kueue
  rebuilds a suspended job's spec and still lets PodSets change.
- It never denies. Its `failurePolicy` is `Ignore`, and the setting
  [`workload-fit-affinity`](/gpustack-operator/v0.9.0/docs/reference/settings/index.md#online-adjustable-settings) turns it off while the labels keep being published.
- The pin never reaches the Pod or its owner's template: Kueue copies only labels, annotations, a
  `nodeSelector`, tolerations and scheduling gates onto them. A Pod-level pin would be unsafe, because
  the kubelet re-admits running Pods against current node labels when it restarts.

What still retries:

- **A label not yet refreshed.** A Workload placed before the label follows an allocation still reaches
  this check and retries once; the next placement reads the new value.
- **Room promised to a Workload that is still starting.** The label cannot see it, so TAS may place a
  Workload on those accelerators. The check holds it until the first Workload's Pods are allocated.
- **Several Pods, or several accelerators, on one node.** The label admits a node when one accelerator
  fits one Pod, so two Pods of one PodSet, two PodSets, or one template-built Pod asking for two
  sliced accelerators can still be placed where only one fits. That does not converge by itself.
- **An unpinned Workload.** One created while the webhook was unavailable or the setting was off, one
  created before the upgrade, and a template-built slice whose template carries no folded units keep
  the loop described above. A `Retry` does not repair a missed pin, because Kueue requeues that same
  object; only a spec update before reservation re-pins it.

Each family gets one correlated `(accelerators, per-accelerator demand, profile)` tuple scoped to the
accelerators that can serve it, so an exclusive or shared request is never judged feasible against a
partitioned accelerator: it stays queued, not admitted into a permanent `Pending`. For partitions
`accelerators` counts *instances*, not accelerators — one hosts as many replicas as its remaining
geometry allows.

**Feasibility is per role.** A Workload composed from a pod group carries one PodSet per role, and
[each PodSet gets its own ResourceFlavor](/gpustack-operator/v0.9.0/docs/modules/devices/scheduling/index.md#stage-4-the-kueue-chain) — so a demand
is judged against the accelerators of the flavor its own PodSet was assigned, never against every
accelerator in the pool.

- Two PodSets' demands merge only when the flavor agrees as well as the family.
- An accelerator is eligible for a demand only when its own key is one that demand's flavor covers.
- The per-accelerator budget stays global to the Workload, so two roles on one flavor cannot spend one
  accelerator twice.
- The verdict message names the role that is starved.

> **Nothing this operator renders produces a multi-PodSet Workload today.** A `ModelDeployment`
> admits each replica as a pod group of its own, and every member of that group carries the same
> role — so the Workload holds one PodSet, whose count is the role's `size`, never one per role.
>
> The reasoning above therefore describes the check's shape rather than a state its own workloads
> reach. It stands because the check answers for every workload in the queues it is referenced
> from, not only for the ones rendered here.

The ledger seeds every accelerator at `M`, so an exclusive over-admit that coarse `credits` let
through is caught exactly, held with `Retry`, and self-heals once Kueue re-admits after the backoff.
The gate only checks: it never preempts and never answers `Rejected`.

The check never judges an evicted Workload: Kueue resets its checks and quota reservation in two
separate writes, and a verdict written between them would overwrite the reset and deadlock the
backoff loop instead of letting it self-heal. Re-reserving quota clears the eviction condition and
re-opens evaluation.

### Gate 4 — default scheduler / kubelet

Node-level counting of each family's remaining keys — the bare `.sliced` / `.partitioned` token
plus its [logical](/gpustack-operator/v0.9.0/docs/modules/devices/scheduling/index.md#logical-slicing-capacities) or
[partitioned](/gpustack-operator/v0.9.0/docs/modules/devices/scheduling/index.md#hardware-partitioning-capacities) counting keys — checks the node
against those keys.

On a TAS queue it does not choose the node: TAS assigned one at quota reservation and the Pod is
pinned to it, so a node without room leaves the Pod `Pending` rather than moving it elsewhere.

Disjoint accelerator populations advertise the two families' keys, so the resource name alone rules out
a node that cannot serve the kind at all — the one placement error `Allocate` can never repair.

### Gate 5 — the device-plugin allocator

At `Allocate`, the Device Manager settles the accelerator, injects the container's visibility env and
runtime isolation, and records the allocation in the `Devices` ledger. "Settles" differs by family:

- **accelerator-bound** — the kubelet chose the accelerator by choosing the token, so the allocator
  refuses one another mode holds and, for a logical slice, one without a free slot or the units the
  slice needs;
- **partitioned** — the tokens are a fungible count, so the allocator picks the accelerator itself and
  materializes the hardware instance on it.

The slice refusal is the one gate every path reaches — a Pod outside the scheduling chain, or a hint
the kubelet declined — so a slice the gates above let through by mistake fails its Pod rather than
oversubscribing an accelerator. Its controller recreates the Pod; the refusal and its off switch are
in [Container identification](/gpustack-operator/v0.9.0/docs/modules/devices/discovery/index.md#container-identification-and-cross-mode-exclusion).

Both paths, and the per-manufacturer isolation each slice gets — it covers every sliceable manufacturer
— are in [Device Discovery](/gpustack-operator/v0.9.0/docs/modules/devices/discovery/index.md#the-device-plugin-allocator). The Pod webhook caps
`.sliced` and `.partitioned` at exactly **1**, so the manufacturer-specific multi-slice divergence that
cap hid can no longer be requested.

## The `Devices` ledger beneath the gates

The `Devices` CR `AcceleratorAllocation` ledger is the single authoritative accounting, written below the
kubelet by the device-plugin `Allocate` for every allocation, Kueue-routed or not. It drives the
four-view and backs gate 3, but is not itself a gate.

## Four-view status

`InstanceType.status` carries four per-accelerator bin-packing projections from the `Devices` ledger, not
a credits fold-down:

| View | Column | Counts |
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

- `EX` — the largest single *node*'s free accelerators; one request can span a node's accelerators;
- `SH` — the largest single *node*'s accelerators that still have a free share, since a shared request
  of N names N distinct accelerators on one node, and a node's spare shares on one accelerator do not
  add;
- `SL` — the freest single *accelerator*'s; a slice targets one accelerator;
- `PT` — `1` while any accelerator can host an instance, else `0`; a partition request is validated to
  be exactly one instance on one accelerator, so nothing larger is requestable.

`kubectl get instancetypes` folds them into the `Accelerator(EX/SH/SL/PT)` column as four
`onceMaxRequest/remaining` groups.

## Capability versus availability

For a partitioned pool, neither `PT` number reports which profiles are still available.
`onceMaxRequest` is the `1`/`0` answer to whether any partition fits. `remaining` sums each
accelerator's largest free count across profiles that compete for the same physical slices;
it does not give a total for each profile.

The per-profile answer is `status.acceleratorPartitioned.remainingProfiles`, paired with
`allocatedProfiles`: the pool-level Σ by profile name of the per-accelerator ledger on
`Devices.status`. **Every profile the pool offers is listed even at zero**, so one a sibling's instance
filled reads `0` instead of vanishing, keeping "offered but full" distinct from "not offered".

`kubectl get instancetypes -o wide` shows the same list as the `PARTITIONS` column; the worker gateway
sums it across clusters (Active members only, like every availability dimension).

**Do not read `status.detail.slicedDetail` for this.** It aggregates the static slicing **capability**
catalog from the `Devices` **spec** side, and that catalog deliberately does not move as instances are
carved and released. The Instance webhook uses the catalog to reject an unoffered profile while
naming the offered set, and to size a request from the profile's `MemoryMib`, which the ledger does
not carry.

Repurposing those counts as availability would make a momentarily-full profile vanish from the offered
set, turning a request that should stay `Retry` at the AdmissionCheck into a permanent rejection.

The partition views are likewise enumerated from the capability side, scoped to the pool's own
accelerator group (a node can carry several models) and joined to the ledger. An accelerator the detector
reported is never dropped for a missing ledger row, nor read as full for an empty one: it falls back to
its catalog ceilings, as the node's per-profile capacity keys do.

> **Why the status lives on a real CRD** — the worker watches the `Devices` resource and writes into
> a real CRD's `.status`, so `kubectl get instancetype -w` observes capacity move as pods allocate
> and free. A read-only projection over the ClusterQueue could not.

The InstanceType API proxies the controller-managed resource and converts it to the public
representation.

## The InstanceType and Instance webhooks

The unit spec lives **only** on the InstanceType: a derived type is stamped with its [per-product
preset](scheduling.md#unit-spec-defaults) at creation, and an admin
edit touches only the InstanceType, never a Node or the ClusterQueue notes.

- **InstanceType validating, create** — requires the complete input, read independently of any editable
  setting: `acceleratorGroup` (only when `acceleratable`), `os`, `arch`, the unit triple
  (`unitResources.cpu`/`.ram` + `localStorage`); empty or partial is rejected. A CPU-only
  (`acceleratable=false`) type's unit CPU must be exactly 1 core, an accelerated type any unitless
  positive integer.
- **InstanceType validating, update** — freezes the spec: every field immutable except
  `displayName` (rename) and `inactive` (in/out of service), so re-sizing or re-pointing a pool means
  delete and re-create. Only immutability is re-checked, never the create-time shape, so a legacy type
  stored before a tightened rule can still be renamed or deactivated.
- **InstanceType defaulting** — an empty `generalGroup` to the `generic` sentinel; the pool's schedule
  labels (grouped by `instance-type-aware-cpu-manufacturer`) and
  `schedule.gpustack.ai/queue-entrance` (the LocalQueue name format); descriptors enriched
  from a matching ResourceFlavor; and when awareness is on, that flavor's `cpuDetail` note folded into
  `spec.cpu` (generic) or `spec.accelerator.cpu` (accelerated).
- **Instance validating** — enforces the unit spec on **Create and Update**: a submission's RAM must not
  exceed `unitRAM × count`, its local storage not the InstanceType's `LocalStorage`.
  - Changing `spec.type`, `spec.resources` or any other template field requires an already stored
    `spec.stop: true`. Only `spec.volume` is immutable regardless of `spec.stop`.
    Stop the Instance before editing its configuration.
    Starting requires `status.phase: Stopped` and revalidates the updated resource request.
  - On a non-accelerated type, create and start also bound the CPU request by the pool's CPU
    **capacity** (`status.cpu.capacity`), never by what is unrequested right now: an Instance submitted
    while every core is requested is admitted and waits in its queue.
  - **The CPU bound is the pool's total, not one node's.** A request above the largest node's cores but
    within the pool's total is admitted and cannot run until a node that large joins, because one Pod's
    cores come from one node and `InstanceType.status` carries no per-node capacity to refuse it with.

## The KV cache injection webhook

A second mutating webhook on Pods writes the client configuration an inference engine needs to use a
[KV cache pool](/gpustack-operator/v0.9.0/docs/modules/kv-cache/injection/index.md). It sits **outside** the five gates:

- It admits or refuses on its own inputs, consumes no `.sliced.*` or `.partitioned.*` value, produces
  none, and never touches `resources`.
- It cannot be a branch of gate 1, because the two select on independent criteria: gate 1 fires on
  `kueue.x-k8s.io/queue-name`, this one on `kvcache.gpustack.ai/inject`, and a `LabelSelector` cannot
  express the union. The two are independent but not disjoint: a Pod may carry both labels and be
  served by both entries, which is exactly what the next point is about.
- Both entries live in the single `gpustack-worker-mutation` configuration, whose name sorts before
  Kueue's so that gate 1 folds a Pod's units before Kueue hashes its resources. Their order within it
  is immaterial, and both run over one Pod whichever order the engine picks.
- Before it adds connector arguments, the KV cache webhook removes the transparent launcher prefixes
  it knows and accepts only the entry point for the declared engine: `vllm` for the vLLM family or
  `python3 -m sglang.launch_server` for SGLang. An unrecognised launcher, launch form, or mismatched
  engine is refused, so a missing parser entry is visible at admission rather than producing a Pod
  marked injected whose engine never receives the connector arguments.
- A Pod author may set `kvcache.gpustack.ai/launch-args-forwarded: "true"` only to declare that an
  unrecognised launcher, script, or command line hidden in one argument forwards appended arguments
  to the engine. It does not exempt a shell command mode such as `sh -c`: admission knows that the
  appended arguments become shell positional parameters and never reach the engine.
- The injected record includes `launchProgram` and `launchArgsForwarded`, so a JSONPath query can show
  the resolved executable and whether that author declaration admitted the launch.
- A pool whose member groups offer two or more transports is refused for an engine that declares no
  required transport, because such an engine is configured with one transport and reads a block from
  any group. The `ModelDeployment` validating webhook refuses the same binding for the same reason,
  since its own engine resolution happens there rather than on the Pod.

## Update validation while an object is deleted

An UPDATE is neither validated nor defaulted once its object carries a `metadata.deletionTimestamp`,
unless the handler opts out. The guard exists because an update that clears a finalizer is an UPDATE,
so a rule reading another object and refusing when it is absent could hold an object past its own
teardown. Every handler of this operator except `Instance` opts out where its rules are safe, and
those rules still validate during deletion.

`Instance` is the one that keeps the guard, so its UPDATE checks are skipped while it is being
deleted. The reason is its mutating half rather than its validation: its defaulting reads the
referenced `InstanceType` and is registered `failurePolicy: Fail` on UPDATE. An `InstanceType` deleted
ahead of its Instances would leave each one undeletable.

## Running-instance stop

Before (re)creating an Instance's Pod, the worker reads the backing
`ClusterQueue`'s `StopPolicy` and **stops** the Instance (`spec.stop=true`) rather than recreate a Pod
the queue can never admit. That covers a queue in `HoldAndDrain` (a pool drain, or a teardown evicting
admitted workloads), an `InstanceType` being deleted, and an `InstanceType` already gone.

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

`TopologyAwareScheduling` is enabled. Generated ResourceFlavors reference Kueue Topologies, and the
Pod integration converts a `ModelDeployment` role's required-level annotation into the Workload
PodSet request described in [Topology-Aware Scheduling](/gpustack-operator/v0.9.0/docs/modules/topology/scheduling/index.md).

It also sets `resources.quotaCheckStrategy: IgnoreUndeclared`, so a single-dimension queue (only `cpu`,
or only the manufacturer `credits`) does not reject a Workload for the Pod resources it does not cover
(`memory`/`ephemeral-storage`). Its `resources.transformations` list is generated from the worker's
node-feature tables by `make generate chart`.

The same strategy makes a queue with **no resource groups** admit every Workload: it declares no
resource, so nothing is checked, the Workload gets no flavor, and with no flavor it carries no
AdmissionCheck. The operator never leaves such a queue admitting — see
[Queue quota and draining](/gpustack-operator/v0.9.0/docs/modules/devices/scheduling/index.md#queue-quota-and-draining).

---

**See also** — [Accelerator Requests](/gpustack-operator/v0.9.0/docs/modules/devices/requests/index.md) (the normative request contract) ·
[Device Discovery](/gpustack-operator/v0.9.0/docs/modules/devices/discovery/index.md#the-device-plugin-allocator) (gate 5 in detail) ·
[Walkthrough](/gpustack-operator/v0.9.0/docs/walkthroughs/devices/scheduling/index.md) (the four views moving on a live cluster)

**Next** → [Installation Modes](/gpustack-operator/v0.9.0/docs/operate/installation-modes/index.md) — how the chain gets deployed.
