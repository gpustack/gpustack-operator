# Topology-Aware Scheduling

> **Purpose** — how topology inventory becomes Kueue topology-aware admission for each
> `ModelDeployment` replica group.
> **Audience** operators, contributors · **Prerequisites** [Scheduling
> Chain](scheduling-chain.md) · **Read time** ~10 min

Topology is a capacity boundary, not a placement hint. GPUStack first establishes an ordered,
validated hierarchy for each Node, then makes every generated queue topology-aware so Kueue admits
the complete PodSet only when one requested domain has enough capacity.

## Contents

- [The discovery boundary](#the-discovery-boundary)
- [TopologySource normalizes other inventories](#topologysource-normalizes-other-inventories)
- [One hierarchy becomes one profile](#one-hierarchy-becomes-one-profile)
- [Profiles enter the Kueue chain](#profiles-enter-the-kueue-chain)
- [A ModelDeployment request is per replica](#a-modeldeployment-request-is-per-replica)
- [Capacity and lifecycle limits](#capacity-and-lifecycle-limits)
- [Failure surfaces](#failure-surfaces)

## The discovery boundary

GPUStack does not infer physical locality. It consumes facts from one of two boundaries:

| Boundary | What discovers locality | What GPUStack owns |
|---|---|---|
| Topograph | A selected Topograph provider and its Kubernetes engine | The pinned optional chart, selection of the labels that form a hierarchy, and their Kueue projection |
| `TopologySource` | Existing Node labels, a ConfigMap snapshot, or an HTTPS inventory endpoint | Input validation, writing-source ownership, profile assignment, and Kueue projection |

Topograph exists to discover relationships that ordinary cloud and Kubernetes metadata do not
express, including fabric and accelerator domains. Its Kubernetes engine owns labels below
`fabric.topograph.run/` and `accelerator.topograph.run/`; GPUStack consumes them and never rewrites
them. Topograph is provider-neutral even though NVIDIA initiated it.

Cloud providers may supply `topology.kubernetes.io/region` and `topology.kubernetes.io/zone`;
Kubernetes supplies `kubernetes.io/hostname`. These keys are read-only inputs to GPUStack. A
ConfigMap or webhook inventory publishes its own region, zone, and rack under
`topology.gpustack.ai/region`, `topology.gpustack.ai/zone`, and `topology.gpustack.ai/rack`.

There is no well-known Kubernetes rack key. `spec.levels` names the keys actually used by that
source; GPUStack never copies values between the standard and private keys.

## TopologySource normalizes other inventories

`TopologySource` is cluster-scoped because its output changes cluster-wide admission. Its
`nodeSelector` chooses the Nodes it may describe, `levels` lists label keys from coarsest to finest,
and exactly one source arm supplies the inventory:

| Arm | Behavior |
|---|---|
| `nodeLabels` | Read-only validation of labels that already exist on selected Nodes |
| `configMap` | Reads a versioned YAML or JSON snapshot from a referenced key |
| `webhook` | Polls an authenticated HTTPS endpoint for the same snapshot |

Creating a `TopologySource` is a privileged operation. For webhook sources, the endpoint is trusted
to receive its referenced bearer token or client certificate, and the worker makes requests from
the cluster network. Redirects are disabled so credentials are not forwarded to another endpoint.

The writing arms publish `topology.gpustack.ai/*` (except the generated profile key) and, when
configured, one administrator-owned prefix. They cannot publish GPUStack feature labels,
Topograph labels, Kubernetes well-known labels, or arbitrary third-party keys.

Each writing source reconciles a separate NFD `NodeFeature` per described Node. Its `spec.labels`
contains exactly that source's writable snapshot labels; NFD projects those labels onto the Node.
The source controller does not directly write Node labels or an ownership-ledger annotation.
Snapshot omission, source deletion, and expiry remove the owned `NodeFeature`; an unexpected
`NodeFeature` edit is repaired on reconciliation.

The existing `status.mutatedNodes` field counts NodeFeatures changed in the latest reconciliation;
it no longer means the source controller directly mutated those Nodes.

Writing sources reconcile on inventory changes, their owned `NodeFeature` changes, and Node
creation or deletion; they do not watch Node label drift. Read-only `nodeLabels` sources continue
to watch Node label changes. After changing an existing Node's selector labels, refresh the
ConfigMap snapshot or update the source to trigger a writing-source reconciliation.

A snapshot may repeat an existing standard region or zone value as a read-only parent of a finer
level. The source checks that the value already matches the Node. Missing or differing values reject
the whole snapshot; GPUStack neither creates nor changes those cloud labels.

The complete snapshot is rejected before any `NodeFeature` changes when it names an unselected Node, an
undeclared level, an invalid label, an incomplete parent chain, or one child value under two parent
tuples. `revision` is an opaque, non-empty identifier; it establishes which complete snapshot was
last applied, not an ordering scheme.

## One hierarchy becomes one profile

`NodeTopologyReconciler` selects exactly one Ready `TopologySource` for a Node. Zero or multiple
matching sources deliberately fall back to a hostname-only hierarchy; this prevents ambiguous
inventories from being flattened into a false tree.

The reconciler takes the longest populated prefix of the selected level list and always appends
`kubernetes.io/hostname`. Missing a finer suffix is valid, but a child without its parent is not.
Contiguous Topograph fabric tiers are normalized to coarsest-to-finest order before validation.

The ordered label-key list is hashed into `topology.gpustack.ai/profile`. Nodes with the same list
share a generated Kueue `Topology`; the domain values remain opaque Node labels. A profile therefore
describes the hierarchy shape, not a particular region, zone, rack, switch, or host.

The profile value is `fnv64-` followed by 16 lowercase hexadecimal digits from FNV-1a 64-bit over
the NUL-delimited ordered level keys. The generated Topology is named `gpustack-<profile>`.
`NodeDevicesReconciler` mirrors the Node profile onto its `Devices` ledger so accelerator admission
checks the same profile as the flavor and Kueue placement.

## Profiles enter the Kueue chain

Every generated `ResourceFlavor` pins the Node's topology profile and references the matching Kueue
`Topology`. Hardware identity and topology profile together form flavor identity, so capacity from
different hierarchy shapes is never silently combined.

`NodeQueueReconciler` admits a queue only when every flavor is topology-aware, its referenced
Topology exists, selectors do not overlap, and quota is conserved across the split. The resulting
ClusterQueue is TAS-only: Kueue evaluates the full PodSet requests, including CPU, memory, and
GPUStack resources, against the selected domains.

Kueue assigns one flavor per covered resource in a PodSet. Capacity fragmented across incompatible
profiles cannot be added together to satisfy one PodSet, even when the ClusterQueue's aggregate
quota appears sufficient.

## A ModelDeployment request is per replica

`roles[].topology.requiredLevel` names a level label key, not a concrete domain value. GPUStack puts
the Kueue `podset-required-topology` annotation on every Pod in that replica group; Kueue's Pod
integration records it as the generated Workload PodSet's required topology request.

A role replica owns one Workload and its `size` Pods form the fate-sharing PodSet. For example,
`replicas: 3` and `size: 8` produces three independent eight-Pod topology decisions, not one
twenty-four-Pod decision. Different roles and replicas are not required to share a domain.

Several roles still admit as one set. The [joint admission check](../reference/model-deployment.md#prefill-and-decode)
holds every group until the whole set has reserved quota, and no role Pod binds to a Node before
then. While the set waits, a group that fits keeps its quota reservation and topology assignment,
so the domain it holds stays idle. When no role fits, no Workload of the set reserves anything.

> **Why the check answers `Pending`, never `Retry`** — a `Retry` makes Kueue evict the Workload, and
> sibling roles waiting on each other would trade the same quota back and forth. The hold ends when
> the set is admitted, when the deployment is deleted, or when the check parks a set that has not
> assembled for 30 minutes (`QuotaReserved` reason `Parked` in the
> [status reference](../reference/model-deployment-status.md#status)).

Omitting `requiredLevel` adds no explicit topology request. The queue is still topology-aware, and
Kueue may choose any compatible hierarchy. The [field contract](../reference/model-deployment.md#topology-placement)
defines the implicit hostname level.

## Capacity and lifecycle limits

Kueue allows at most 64 flavors in one resource group and forbids repeating a covered resource in
another group as overflow. GPUStack validates the complete queue plan and refuses a partial update
when hardware identity multiplied by topology profiles exceeds that limit.

This release creates fresh TAS queues and supports changes to the ordered level keys of a live
profile. A change such as region → zone → rack creates replacement Topologies and ResourceFlavors;
the managed ClusterQueue keeps its name and UID.

When the new plan drops a flavor that Kueue still reports reservation or usage on, or does not
report at all, `NodeQueueReconciler` sets `HoldAndDrain`, waits for Kueue to report zero reserving
Workloads, switches the complete flavor plan, and restores the queue's previous stop policy. Kueue
owns eviction and readmission; serving workloads can be interrupted.

A plan with no flavor left is the exception: the emptied queue stays held until a flavor returns,
because [a queue without resource groups admits every Workload](admission.md#known-behavior-the-deployed-kueue-configuration).

A dropped flavor that Kueue reports idle, an added flavor, or a quota-only change is updated in
place without a hold. The decision reads only a queue status Kueue wrote for the current generation;
until then the queue reports `TopologyReady=Unknown` with reason `AwaitingQueueStatus`. Old flavors
and Topologies retire after their references clear.

Generated Kueue Topology and ResourceFlavor topology fields are immutable. GPUStack reports drift
instead of modifying them. Queues created by a previously published version are not adopted or
upgraded by this path; create fresh managed objects for this release.

## Failure surfaces

| Symptom | Read first | Meaning |
|---|---|---|
| `TopologySource` `Ready=False` | Its `Valid` and `OwnershipConflict` conditions | The inventory is invalid, stale, expired, or contends with another writer |
| ClusterQueue `TopologyReady=False` | Condition reason and referenced flavors/Topologies | A flavor is missing TAS metadata, selectors overlap, quota is not conserved, or the flavor limit was exceeded |
| ClusterQueue `TopologyReady=Unknown`, reason `AwaitingQueueStatus` | The queue's Kueue `Active` condition and the Kueue controller | Kueue has not written the queue status for the current generation, so dropping a flavor waits for it |
| ModelDeployment remains Pending | Its progress message and generated Workload conditions | No one domain at the required level fits the complete PodSet |
| Aggregate quota looks sufficient | Flavor profiles and Workload topology assignment | The request cannot combine capacity from different profiles or domains |

GPUStack reflects Kueue's inadmissible message with the role and replica; it does not translate
Kueue prose into a new stable reason. Operational checks and source examples are in
[Topology-Aware Scheduling Operations](../operation/topology-aware-scheduling.md).

---

**See also** — [Scheduling Chain](scheduling-chain.md) (the flavor and queue owners) ·
[Admission](admission.md) (the gates after queue admission) · [Model Deployment
Reference](../reference/model-deployment.md) (the user-facing field)

**Next** → [Admission](admission.md) — the remaining gates after topology-aware quota reservation.
