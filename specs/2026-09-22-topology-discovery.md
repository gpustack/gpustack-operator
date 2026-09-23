# Spec: Cluster Topology Discovery and Topology-Aware Model Deployment

Status: Shipped
Type: Feature

## Summary

GPUStack Operator will acquire physical topology from the optional Topograph subchart or from a
GPUStack-defined ConfigMap/Webhook contract, normalize the result into Kubernetes Node labels, and
turn every managed InstanceType queue into a Kueue topology-aware scheduling queue without counting
the same node capacity twice. A `ModelDeployment` role may then require each multi-Pod replica to fit
inside one region, zone, rack, fabric tier, or scale-up accelerator domain. This supersedes the
narrow proposal in [issue #483](https://github.com/gpustack/gpustack-operator/issues/483): the missing
consumer is still fixed, but topology discovery and non-cloud extensibility are now part of the
contract rather than assumed to exist elsewhere.

## Motivation

### Goals

- Discover cloud, data-center, switch-fabric, and accelerator-domain locality instead of limiting
  topology to the existing manufacturer detector's scale-up domain.
- Read existing `topology.kubernetes.io/region`, `topology.kubernetes.io/zone`, and
  `kubernetes.io/hostname` for their standardized meanings. Inventory writers publish region, zone,
  and rack only under project-owned or explicitly delegated keys; they NEVER publish the standard
  Kubernetes region and zone labels.
- Bundle a pinned Topograph Helm chart and use its provider/engine separation for environments it
  supports, including NVIDIA accelerator domains and cloud or InfiniBand fabric hierarchy.
- Give non-cloud and unsupported platforms one documented topology snapshot schema that can be
  supplied through a ConfigMap or an HTTPS webhook and produces the same scheduler-facing labels.
- Use Kueue TAS for admission and placement. A workload that omits an explicit level remains
  eligible for implicit TAS placement, subject to the capacity fragmentation of its one assigned
  topology-profile flavor.
- Let a `ModelDeployment` role name the Node label level at which every Pod in one replica must be
  co-located, without making the caller choose a concrete region, zone, rack, or domain value.
- Keep unlabeled or partially labeled nodes available to unconstrained workloads, but NEVER invent
  a rack, fabric, region, or accelerator domain for an explicit topology request.

### Non-Goals

- Intra-node NUMA, PCIe, NIC-to-accelerator, or kubelet Topology Manager placement.
- Selecting a particular domain value such as `rack-a`; TAS chooses a fitting domain value.
- Guaranteeing that all independent replicas of a role, or different roles, share one domain. The
  current scheduling unit is one replica group, so changing that boundary is a separate API and
  controller redesign.
- Making arbitrary Node annotations scheduler inputs. Raw metadata may use annotations, but Kueue
  topology levels always consume validated Node labels.

### Live feasibility gate update

The fresh CPU-only EKS gate proved that the bundled Kueue 0.18.9 controller can admit a Kueue-created
Job Workload with a TAS `TopologyAssignment`, constrain a two-Pod group to one availability zone, and
refuse a three-Pod group when each individual zone can fit only two Pods even though aggregate cluster
CPU and memory are sufficient. It also proved implicit TAS and a synthetic GPUStack credit-resource
fixture. The synthetic fixture patches only disposable test Node status; it is evidence about Kueue's
covered-resource accounting, not a claim that the CPU test Nodes have accelerators.

The same gate live-proved the positive multi-role path: a real two-role `ModelDeployment` produced two
distinct Kueue Workloads, both acquired quota, and both reported the existing joint-admission
AdmissionCheck as `Ready` before their role Pods ran. Repository review corrects an earlier assumption:
the existing `ModelDeployment` controller deliberately renders one
Kueue Workload per replica and uses the existing ModelDeployment joint-admission AdmissionCheck to
hold every group of a multi-role deployment until the complete set has quota reservations. A role name
identifies the PodSet/role hash, while a role's `quotaReserved` status can range from zero through its
desired replica count. This is an operator-level barrier above Kueue's individual Workload admission,
not a JobSet requirement.

The fresh run also proved the no-capacity multi-role path. A full deployment consumed the fresh queue's
eight CPU units, then a second two-role deployment created twelve Kueue Workloads. The test resolved each
Workload through its owning Pod, rather than incorrectly looking for a direct ModelDeployment owner. Across
two observation intervals, all twelve AdmissionChecks remained `Pending`, no Workload had an admission or
quota reservation, and no role Pod bound to a Node. This passes T2's atomicity gate. The implementation
MUST NOT claim this behavior from the standalone Job fixtures above; they establish only Kueue TAS
semantics.
- Inventing reserved-looking keys under `kubernetes.io` or `k8s.io` for rack or fabric topology.
- Copying Topograph source into this repository or maintaining a GPUStack fork of its provider and
  graph implementation.
- Enabling privileged InfiniBand discovery or cloud credentials without an explicit administrator
  configuration.
- Adopting, upgrading, migrating, draining, or rolling back a `ClusterQueue`, `ResourceFlavor`, or
  Kueue `Topology` created by a previously published GPUStack Operator version. Every end-to-end run
  still starts from clean objects created by this implementation; cross-version upgrade behavior is
  not inferred from the supported live-profile transition below.
- Zero-disruption admission while Nodes change topology profile. The operator preserves the managed
  ClusterQueue's identity and converges its flavor references in place, but it deliberately uses
  Kueue `HoldAndDrain` to evict and drain reservations before removing an old flavor.

## Proposal

### Acquisition and normalization

The operator chart gains Topograph chart `1.0.0` with application `v1.0.0` as a pinned dependency,
disabled by default and selected with `topograph.enabled`. When enabled, the parent chart requires a
production provider configuration and uses Topograph's Kubernetes engine. Topograph remains the
owner of the labels it publishes:
`fabric.topograph.run/tier-N`, `accelerator.topograph.run/domain`, and
`accelerator.topograph.run/sub-domain`. GPUStack consumes those labels but does not rewrite them.

Topograph is vendor-neutral even though NVIDIA initiated it. Its providers cover cloud and
on-premises topology, and its Kubernetes engine is useful on non-NVIDIA nodes. The whole subchart is
therefore controlled by an explicit feature setting, not by the presence of an NVIDIA node. Helm
renders before live Node inventory is known and cannot reliably install a dependency in response to
later node discovery. Provider-specific node agents can use a Node selector; the NVIDIA example uses
NFD's NVIDIA PCI-presence label so the node-data-broker runs only where the NVIDIA source is usable.
The Topograph API server and observer remain one shared control plane while the feature is enabled.

Platforms that Topograph does not support use a new cluster-scoped `TopologySource` API. Creating
or updating this object is a cluster-administrator operation because its output changes cluster-wide
scheduling. Every source has `spec.nodeSelector`, a coarsest-to-finest `spec.levels` list, and
exactly one member of the following tagged union:

- `nodeLabels` observes labels already written by the cloud provider, kubelet, Topograph, the
  GPUStack Device Manager, or an administrator. It is read-only and is also how an administrator
  declares which one of Topograph's independently discovered dimensions is the active TAS hierarchy.
- `configMap` reads a versioned topology snapshot from
  `configMapRef.namespace`, `configMapRef.name`, and `configMapRef.key`. This is the Kubernetes form
  of the requested file input and does not grant the controller arbitrary host filesystem access.
- `webhook` performs an authenticated HTTPS `GET` and expects the same snapshot schema. It supports
  `url`, `pollInterval`, `timeout`, `maxStaleness`, a CA-bundle ConfigMap reference, and either a
  bearer-token or TLS client-certificate Secret reference. Plaintext HTTP, URL user information,
  redirects, proxy-derived destinations, and inline credentials are rejected. Responses have a
  fixed maximum body size and must complete within the configured timeout.

ConfigMap, CA, and credential references are restricted to the namespace in which the worker is
installed. The controller receives `get`, `list`, and `watch` only for the referenced resource
kinds in that namespace; it does not receive cross-namespace Secret read access. The installation
guide must pair webhook use with explicit egress policy and explain that the cluster administrator,
not an ordinary tenant, is authorizing the endpoint. The controller resolves and connects to the
validated HTTPS URL itself and never forwards caller-controlled authorization headers.

ConfigMap and webhook use one JSON or YAML snapshot schema:

```yaml
apiVersion: topology.gpustack.ai/v1alpha1
revision: inventory-42
nodes:
  worker-a:
    topology.gpustack.ai/region: us-east-1
    topology.gpustack.ai/zone: us-east-1a
    topology.gpustack.ai/rack: rack-01
```

Webhook credential object data keys are fixed to Kubernetes conventions: the optional CA ConfigMap
uses `ca.crt`, bearer-token Secret uses `token`, and mTLS Secret uses `tls.crt` and `tls.key`.

`revision` is an opaque non-empty identifier. `nodes` is keyed by the exact Kubernetes Node name;
its values may contain only declared levels. Unknown Nodes, duplicate keys after YAML decoding,
undeclared levels, invalid label keys or values, and incomplete parent chains reject the whole
revision before any `NodeFeature` is changed. The ordered level list is the asserted hierarchy. The
controller always appends `kubernetes.io/hostname` to the Kueue hierarchy and rejects a source that
tries to assign or redefine hostname.

Writing sources publish `topology.gpustack.ai/*` (except the generated profile key) or an explicitly
configured administrator-owned DNS prefix through NFD `NodeFeature` objects. They NEVER publish
`topology.kubernetes.io/region`, `topology.kubernetes.io/zone`, `feature.gpustack.ai/*`,
`fabric.topograph.run/*`, `accelerator.topograph.run/*`, `kubernetes.io/hostname`, or another
product's prefix. This prevents the source reconciler from competing with Device Manager/NFD stale
cleanup or Topograph's Kubernetes engine.

Each writing source reconciles one distinct NFD `NodeFeature` per described Node in the worker
namespace. The object has the NFD node-name metadata label, a deterministic bounded name derived
from source UID and Node name, a controller owner reference to the cluster-scoped source, and exact
writable snapshot labels in `spec.labels`. It never updates the existing hardware `NodeFeature` or
the Node directly. Reconciliation repairs a changed `NodeFeature`; snapshot omission, source deletion,
and expiry delete the source-owned object so NFD can remove its projected Node labels. A matching
standard region or zone value may appear in the snapshot as a read-only parent, but the value MUST
already exist on the Node and is never copied into `NodeFeature.spec.labels`. Missing or differing
standard values reject the complete revision. More than one writing source may exist only when their
owned level and Node-selector scopes do not overlap. Runtime overlap reports a conflict and
publishes neither claimant. `nodeLabels` sources own no `NodeFeature` and publish nothing.

Writing sources react to inventory changes, owned `NodeFeature` changes, and Node creation or
deletion, but not to arbitrary Node label drift. A ConfigMap source needs a snapshot or source
refresh after an existing Node's selector labels change; a webhook source polls on its configured
interval. The read-only `nodeLabels` arm still watches Node label changes.

A Node may match exactly one ready source hierarchy. If selectors overlap at runtime, neither
hierarchy claims the Node and it uses the hostname-only profile until the conflict is resolved.
This rule lets NVIDIA, another accelerator manufacturer, and general CPU nodes use different proven
hierarchies without presenting the same capacity through parallel TAS flavors.

Webhook or ConfigMap failure retains the last valid snapshot until the source's required
`maxStaleness` duration expires. ConfigMap sources configure the same field on the source rather
than inside the ConfigMap. While retained, `Ready=False, Reason=Stale` exposes the age and last
successful revision. At expiry, the controller deletes its owned `NodeFeature` objects and reports
`Ready=False, Reason=Expired`; it does not preserve topology indefinitely and does not replace it
with synthetic values. Status also records `ObservedGeneration`, source kind, last successful
revision and refresh time, selected Node count, NodeFeature mutation count in the existing
`mutatedNodes` field, conflicted Node count, and conditions for `Ready`,
`Valid`, and `OwnershipConflict`.

### Label contract

The normalized contract uses existing keys when they already have a stable owner:

| Meaning | Scheduler-facing Node label |
| --- | --- |
| Cloud region (read-only) | `topology.kubernetes.io/region` |
| Cloud zone (read-only) | `topology.kubernetes.io/zone` |
| Inventory-owned region | `topology.gpustack.ai/region` |
| Inventory-owned zone | `topology.gpustack.ai/zone` |
| Node | `kubernetes.io/hostname` |
| GPUStack data-center block | `topology.gpustack.ai/block` |
| GPUStack rack | `topology.gpustack.ai/rack` |
| GPUStack fabric tier | `topology.gpustack.ai/fabric-tier-N` |
| Existing scale-up domain | `feature.gpustack.ai/fabric.domain` |
| Topograph fabric tier | `fabric.topograph.run/tier-N` |
| Topograph accelerator domain | `accelerator.topograph.run/domain` |
| Topograph accelerator sub-domain | `accelerator.topograph.run/sub-domain` |

The ConfigMap/Webhook snapshot may publish GPUStack topology keys and one administrator-owned DNS
prefix, but NEVER the well-known region and zone keys. The existing
`feature.gpustack.ai/fabric.domain` key is read-only input owned by Device Manager/NFD; it is not a
permitted snapshot output. `spec.levels` selects the labels actually present; no implicit mapping
or duplication occurs between cloud and inventory-owned region/zone keys. Topograph's own
labels are also read-only inputs because their ownership, tier ordering, and cleanup behavior are
part of its public contract.

The ordered level list is an assertion that the values form a tree. For each observed child value,
all Nodes carrying it must have the same parent tuple. A source that says one rack belongs to two
zones, or lists a finer level without its parent, is invalid and changes no Node labels. Topograph
fabric tiers arrive closest-first, so the adapter reverses them when constructing Kueue levels,
which are coarsest-first. Accelerator domains and fabric tiers are not silently combined: they are
separate physical dimensions unless an administrator supplies and validates one nesting order in a
`nodeLabels` source.

### Kueue topology profiles and stable queue reconciliation

One Kueue `Topology` is reconciled for each distinct, valid ordered subset of its selected source's
level keys present on Nodes, always ending in `kubernetes.io/hostname`. A Node missing a finer
suffix still uses its valid coarser prefix; a Node with no discovered region, zone, rack, or fabric
data belongs to a hostname-only profile. GPUStack stamps a stable
`topology.gpustack.ai/profile` label whose value is `fnv64-<16 lowercase hexadecimal digits>`. The
digest input is the coarsest-to-finest ordered level-key list, including the appended hostname key,
with keys separated by NUL bytes and hashed with the project's FNV-1a 64-bit helper. Level order is
significant, while domain values such as `rack-a`
or `us-east-1a` are never part of the digest. The matching Kueue Topology is named
`gpustack-fnv64-<16 lowercase hexadecimal digits>`. This deliberately follows the project's compact
FNV-1a 64-bit naming style instead of exposing the previous long SHA-256/base32 form. The profile
label partitions nodes by topology schema while the actual topology labels carry their locations.
Before reusing either deterministic name, reconciliation compares the stored ordered levels with the
candidate input. A digest collision is an actionable error and NEVER aliases two schemas or changes
an immutable Topology in place.

Managed ResourceFlavors are split by topology profile as well as their existing hardware identity.
Each resulting flavor selects exactly one profile and references that profile's Kueue `Topology`.
The Node capacities assigned to these flavors are disjoint, and their sum equals the capacity the
unsplit hardware identity would advertise. GPUStack MUST NOT create a full-capacity non-TAS flavor
beside a full-capacity TAS flavor for the same nodes, because Kueue would see two quotas for one
physical resource.

Every flavor in a newly created managed ClusterQueue is a TAS flavor from its first successful
reconcile, and later topology-profile changes replace its flavor references without replacing the
ClusterQueue. Kueue therefore treats a Workload with no explicit topology request as implicit TAS.
This means no domain level is required, but it does not mean that one PodSet can combine quota from
multiple topology-profile flavors: Kueue assigns one flavor per covered resource in a PodSet.
Splitting heterogeneous profile schemas can therefore fragment usable capacity even though total
nominal quota is conserved. The implementation and documentation MUST NOT promise that an omitted
request can consume the arithmetic sum of otherwise incompatible profiles.

A Workload that explicitly requires a label level is considered only for profiles containing that
level. Nodes without discovered region, zone, rack, fabric, or accelerator information remain real
hostname-only TAS capacity for implicit requests, but cannot satisfy an explicit richer level.

This release never adopts or mutates a non-TAS queue from an earlier published version. A newly
managed ResourceFlavor's immutable selector, tolerations, and `topologyName` are correct at creation
time, and neither ResourceFlavor nor Kueue Topology is mutated when its ordered profile key set
changes. Instead, the operator reconciles an online dependency replacement while preserving the
operator-managed ClusterQueue's name and UID:

1. Compute the complete desired hardware-identity × topology-profile plan and validate disjoint
   selectors, conserved quota, available Topologies, and the 64-flavor limit before changing the
   ClusterQueue.
2. Create every missing immutable Kueue Topology and profile-qualified ResourceFlavor required by
   that plan. Existing ClusterQueue references remain unchanged while any dependency is absent or
   invalid.
3. Set the ClusterQueue's stop policy to Kueue `HoldAndDrain` before every flavor-reference switch,
   then wait until the observed `status.reservingWorkloads` is zero. Holding even an empty queue
   closes the race in which a new reservation could be admitted between a zero check and the switch.
   Removing a flavor from a ClusterQueue does not itself evict an already admitted Workload, so this
   drain is REQUIRED before switching references. The ClusterQueue name and UID remain unchanged
   while draining.
4. Update the existing ClusterQueue's `spec.resourceGroups` to the complete desired plan while it is
   held. This is one API update against the same object; the queue MUST NOT expose a partially
   assembled mix caused by incrementally appending flavors. Record the exact pre-migration stop
   policy, including unset, `None`, `Hold`, or `HoldAndDrain`, and restore that value only after the
   complete new plan is observed. While the controller-owned migration marker exists,
   `HoldAndDrain` is authoritative; a conflicting external edit does not bypass the transition.
5. After the ClusterQueue no longer references an old flavor, retire that ResourceFlavor only when
   it has no contributing Nodes. Delete its generated Topology only after no ResourceFlavor refers
   to it. Kueue finalizers may keep either object terminating; this delays garbage collection but
   MUST NOT roll back or replace the ClusterQueue.

These steps are level-based and retryable. A failure before the hold leaves the last complete queue
plan serving while newly created dependencies are harmlessly unused. A failure after entering
`HoldAndDrain` leaves the old complete plan safely held and resumes waiting or retrying; it MUST NOT
restore admission while old reservations remain. A conflict or transient API failure during the
resource-group switch retries from observed state. A failure after the switch leaves the new
complete queue plan held until its normal stop policy can be restored, then retries retirement
independently. If the source changes again mid-transition, reconciliation recomputes the desired
plan from current Node labels, but does not bypass an in-progress drain. Immutable drift in a
generated Topology or ResourceFlavor remains an actionable error; the controller does not rewrite
the object or switch the ClusterQueue to an unverified replacement.

Kueue owns eviction under `HoldAndDrain` and later readmission after the normal stop policy is
restored. GPUStack does not patch Workloads or promise uninterrupted Pods. The supported invariant
is stable ClusterQueue identity, zero old reservations at the flavor-reference switch, and eventual
convergence without double-counted capacity, not zero-disruption Workloads.

Kueue permits no more than 64 flavors in one resource group, and the same covered resource cannot
be repeated in another group as an overflow mechanism. Before creating or updating a managed queue,
GPUStack computes the complete hardware-identity × topology-profile set. If any covered resource
would need more than 64 flavors, it sets an actionable condition on the InstanceType and creates no
partial ClusterQueue. The current 16-flavor chunking helper must be replaced for this path; it may
not create multiple groups with duplicate `coveredResources`.

The operator owns each generated Kueue `Topology` independently of any one pool. A dedicated
topology reconciler watches Nodes, TopologySources, and Kueue Topologies. Existing NodeFlavor and
NodeQueue reconcilers remain the sole writers of ResourceFlavor specs and ClusterQueue resource
groups respectively; the topology reconciler supplies resolved profile input and does not compete
for those fields. Generated objects carry deterministic names and management labels, but no owner
reference to a pool-scoped object.

### ModelDeployment topology request

`ModelDeploymentRole` gains one optional field:

```yaml
topology:
  requiredLevel: topology.gpustack.ai/rack
```

`requiredLevel` is a Kubernetes label key, not a fixed `region|zone|rack` enum. This keeps the API
compatible with well-known keys, Topograph's variable-depth tiers, the existing scale-up domain,
and administrator-owned finer levels. Validation checks label-key syntax. Whether the configured
queue actually exposes the level is dynamic and is reported by Kueue admission.

The controller adds `kueue.x-k8s.io/podset-required-topology=<requiredLevel>` to every Pod in that
replica's Kueue pod group. Kueue's Pod integration converts the annotation into the Workload
PodSet's `topologyRequest.required`; GPUStack does not create or patch the Workload directly.
Omission adds no explicit annotation, so the TAS-only ClusterQueue supplies unconstrained placement.

The contract applies independently to each replica because a role's `replicas` are independent
serving instances and each replica owns one Kueue Workload. The `size` Pods inside that replica are
the fate-sharing PodSet placed in one domain. For example, `replicas: 3, size: 8` creates three
independent eight-Pod topology decisions, not one 24-Pod decision.

An unsatisfied required level remains Pending in Kueue. GPUStack reflects the downstream Workload's
inadmissible reason in the existing ModelDeployment progress message and identifies the role and
replica; it does not parse Kueue prose into a new stable reason enum. Removing or changing the field
recreates only the affected replica groups under the existing render-hash rollout behavior.

### User Stories

#### Story 1

As a cloud cluster operator, I want the standard region and zone labels plus discovered rack or
fabric locality to feed one scheduling hierarchy, so that workloads can request the right failure
or communication domain without knowing a concrete domain value.

#### Story 2

As an on-premises cluster operator, I want to publish a versioned topology snapshot through a
ConfigMap or HTTPS webhook, so that unsupported hardware and data-center inventory systems have the
same scheduling contract as built-in providers.

#### Story 3

As an NVIDIA cluster operator, I want a pinned Topograph deployment to discover supported cloud,
InfiniBand, and accelerator topology and write its established Node labels, so that GPUStack does
not build a second vendor-specific graph discovery stack.

#### Story 4

As a model operator, I want each distributed serving replica to require one region, zone, rack,
fabric tier, or scale-up domain, so that its cooperating Pods are admitted only where the requested
locality has enough real capacity.

#### Story 5

As an existing user, I want a ModelDeployment with no topology field to retain unconstrained
placement across any compatible fresh TAS flavor, so that topology is optional in the API. I accept
that one PodSet receives one flavor and cannot aggregate fragmented capacity across incompatible
topology profiles.

#### Story 6

As a cluster operator, I want adding rack or another ordered level to a live source hierarchy to
preserve each operator-managed ClusterQueue's name and UID, so that queue references and operational
identity remain stable while immutable Topologies and ResourceFlavors are safely replaced. I accept
that the operator places the queue in `HoldAndDrain`, Kueue evicts admitted Workloads, and admission
resumes only after old reservations reach zero and the complete flavor plan is switched.

### Core Features & Acceptance Criteria

#### F1 - Topograph is a pinned, optional provider stack

The parent chart vendors a released Topograph chart through the existing dependency staging and
patch workflow, defaults it off, prevents an enabled production install from using the test
provider, and exposes provider credentials, engine parameters, selectors, security context, and
image mirroring through documented parent values.

Acceptance: dependency regeneration is repeatable; disabled renders contain no Topograph objects;
enabled renders use the Kubernetes engine; NVIDIA and generic provider examples render the intended
broker placement; `helm test` and the operator chart tests pass; the chart records Apache-2.0
notices required by the upstream distribution.

#### F2 - Non-cloud sources share one validated schema

ConfigMap and webhook sources accept identical versioned snapshots, reconcile source-owned NFD
`NodeFeature` objects, expose revision/readiness/staleness status, reject partial invalid updates,
and remove expired or omitted source-owned objects without directly editing Nodes.

Acceptance: tests cover read-only Node labels, valid input, invalid label syntax, an unknown Node,
ambiguous hierarchy, selector overlap, ownership conflict, webhook authentication, transient
failure before expiry, expiry, recovery, and stale label cleanup. A writing source cannot write
reserved keys other than the allow-listed well-known region and zone keys.

#### F3 - Label meanings and hierarchy are stable

Region, zone, and hostname retain Kubernetes meanings; GPUStack keys cover only unstandardized
concepts; Topograph keys retain Topograph ownership. Every schedulable ordered level set is proven
to form a tree and ends in hostname.

Acceptance: conformance tests feed cloud-only, on-premises rack, variable-depth fabric, accelerator-
domain, partial, and contradictory snapshots. Contradictory input leaves the last valid labels and
sets a condition instead of producing a Kueue Topology.

#### F4 - Managed queue capacity is TAS-only, counted once, and identity-stable

Every newly created managed queue starts with topology-profile flavors whose node sets are
disjoint. Their per-resource nominal quotas sum to the capacity derived from Nodes. A hostname-only
profile carries nodes with no richer topology. When the ordered level-key set changes, immutable
replacement Topologies and ResourceFlavors are prepared first, the same ClusterQueue is updated to
the complete replacement plan, and unused old objects are retired afterward. No adoption or
cross-published-version migration path is provided.

Acceptance: controller tests prove no mixed TAS/non-TAS queue state, no overlapping profile
selectors, exact quota conservation, correct immutable flavor creation, self-healing after a
generated Topology deletion before use, rejection above 64 flavors for one covered resource, and
an actionable condition instead of mutation when an immutable managed object drifts. A live
region-to-zone profile changed to region-to-zone-to-rack MUST preserve ClusterQueue name and UID,
enter `HoldAndDrain`, wait for `status.reservingWorkloads=0`, atomically replace its flavor references
only after every new dependency exists, restore admission, and eventually release the old
no-contributor ResourceFlavor and Topology. Tests inject failure before, during, and after each phase
and prove retries never duplicate quota, switch with a live reservation, or require queue recreation.

#### F5 - ModelDeployment expresses a required level per replica

An optional role field accepts a label key and renders the Kueue required-topology annotation on all
members of each replica group. Omission renders no explicit request. The field applies to `size`
members independently for every `replicas` instance.

Acceptance: API, webhook, render, and Kueue Workload tests cover omission, region, zone, rack,
Topograph tier, existing scale-up domain, invalid key, a one-Pod replica, a multi-Pod replica,
multiple replicas, and field updates. Tests inspect the Workload created by Kueue's Pod integration,
not a Workload synthesized by GPUStack test code.

#### F6 - Real placement, refusal, and compatibility are observable

Kueue admits a fitting replica inside one requested domain and leaves a replica Pending when only
aggregate cross-domain capacity would fit. A request without an explicit level can use any one
compatible hostname-only or richer TAS flavor; it is not promised to aggregate them. ModelDeployment
progress points to the affected replica and downstream Workload.

Acceptance: a fresh CPU-only EKS end-to-end test observes Workload `TopologyAssignment` and final
Pod nodes for a fitting case, observes no admission for a cross-AZ-only case, proves a hostname-only
profile is not a fallback for an explicit richer request, and proves omission uses a compatible TAS
flavor without claiming cross-profile aggregation. A joint-admission multi-role case proves that
partial domain reservations do not deadlock admission. Separate controller coverage includes the
existing two-segment and three-segment manufacturer domain values without parsing or truncating
them.

### Notes / Constraints / Caveats

- Topograph's purpose and boundary are confirmed by its
  [provider/graph/engine architecture](https://github.com/NVIDIA/topograph/blob/main/docs/architecture.md)
  and [Helm chart](https://github.com/NVIDIA/topograph/blob/main/charts/topograph/README.md).
  Providers discover environment facts, a canonical graph carries switch tiers and accelerator
  domains, and engines translate that graph. The Kubernetes engine writes labels; it does not
  create Kueue objects.
- Topograph does not currently provide the requested general production file/Webhook source. Its
  test/simulation inputs are not an operational contract. GPUStack supplies that missing input path
  without changing Topograph's graph or building a fork.
- Kubernetes has
  [well-known region and zone labels](https://kubernetes.io/docs/reference/labels-annotations-taints/),
  but no adopted generic rack or fabric label. Topograph's
  [label reference](https://github.com/NVIDIA/topograph/blob/main/docs/reference/node-labels.md)
  records that KEP-4962 closed without adoption, so it does not authorize use of the reserved
  Kubernetes prefix.
- The compile-time Kueue module is v0.17.1 while the bundled runtime is v0.18.9. The
  [v0.17 TAS contract](https://kueue.sigs.k8s.io/v0.17/docs/concepts/topology_aware_scheduling/)
  contains the APIs used here. Unit tests run against the module source; end-to-end tests run
  against the bundled controller, and neither result substitutes for the other.
- A Kueue Topology is a single ordered hierarchy. Parallel dimensions such as switch proximity and
  accelerator partitioning are not assumed to nest. A configured hierarchy must choose or prove an
  ordering; duplicating capacity into multiple topology flavors is forbidden.
- Once a ClusterQueue is TAS-only, Kueue fits the complete PodSet resource request against the
  selected topology domains, not only the coarse CPU or accelerator-credit resource currently used
  to size GPUStack queues. The implementation is gated on a live proof covering ordinary CPU,
  memory, and GPUStack pseudo-resource requests so the existing queue model does not silently admit
  or reject a different workload set.
- Topograph requires Kubernetes 1.27 or newer. The parent chart keeps its current baseline when the
  dependency is disabled and rejects enabling Topograph on an older cluster.
- The supported lifecycle starts with freshly created objects from this implementation. After that
  point, source hierarchy and Node profile changes are reconciled by replacing immutable
  Topologies/ResourceFlavors around an in-place ClusterQueue resource-group update. Objects created
  by an earlier published operator version remain outside this release's adoption and upgrade
  contract.

### Boundaries

- **Always:** preserve source ownership, validate hierarchy before publishing NodeFeatures, end every Kueue
  topology in hostname, and conserve quota across profile splits.
- **Always:** treat the complete domain value as opaque; `nvlink-id-clique` and `ub-id` are values,
  not strings to parse into a hierarchy.
- **Ask first:** add another mutable external source type, enable privileged discovery by default,
  or change the replica-group admission boundary.
- **Never:** synthesize region, zone, rack, fabric, or accelerator membership; duplicate physical
  capacity across topology choices; use annotations as Kueue levels; or dynamically install Helm
  dependencies from a controller.
- **Never:** let a `TopologySource` write Device Manager/NFD or Topograph label prefixes, follow a
  webhook redirect, or read a credential Secret outside the worker namespace.

### Risks and Mitigations

| Risk | Mitigation |
| --- | --- |
| Two sources fight over one Node label or hierarchy | Track ownership and selector scope, reject known overlap, and place runtime conflicts in the hostname-only profile without overwriting either value. |
| A stale inventory causes false locality | Require `maxStaleness`, report age, and remove owned labels after expiry. |
| Profile splitting inflates queue quota | Require disjoint selectors and assert exact pre/post quota sums before updating the ClusterQueue. |
| One PodSet cannot combine capacity split across topology profiles | Document single-flavor assignment, keep profiles minimal, test omission honestly, and do not claim aggregate capacity is schedulable. |
| More than 64 profiles would repeat one covered resource across resource groups | Reject the complete queue plan before creating a partial queue and report the exact profile count and limit. |
| Two ordered schemas produce the same compact FNV-1a digest | Compare ordered levels before name reuse, report an actionable collision, and never alias or rewrite an immutable Topology. |
| TAS flavor and Topology immutability blocks in-place dependency edits after profile drift | Create the complete replacement dependency set first, atomically switch the same ClusterQueue's resource groups, then retire unreferenced old dependencies. |
| A transition fails between dependency creation, queue update, and garbage collection | Make every phase level-based and idempotent; keep the last complete queue plan before the switch and the new complete plan after it, and retry unused-object cleanup independently. |
| Existing reservations name a flavor that must leave the ClusterQueue | Set `HoldAndDrain`, wait for observed `reservingWorkloads=0`, switch the complete flavor plan while held, then restore admission; promise stable queue identity, not uninterrupted Workloads. |
| TAS fits the full PodSet request rather than only the queue's coarse resource | Gate implementation on the CPU/memory/pseudo-resource proof and retain the current queue model only if observed admission is correct. |
| A non-nested pair of topology dimensions is flattened into a false tree | Validate parent tuples and require one explicit hierarchy; keep other labels observable but unschedulable in that profile. |
| Enabling Topograph deploys privileged code unexpectedly | Default the subchart off and require explicit provider-specific security settings. |
| Webhook access becomes a cluster-admin confused deputy or SSRF path | Restrict object authorship and credential namespace, require HTTPS, disable redirects, bound time and body size, and document egress policy. |
| Joint admission reserves incompatible domains for separate roles | Add a live multi-role admission case and block release if the Pod integration cannot converge without leaked reservations. |

## Design Details

### Commands

```sh
# Local source and controller verification in this worktree.
GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test ./api/worker/v1alpha1/... \
  ./pkg/worker/webhooks/worker/... \
  ./pkg/worker/controllers/worker/...
make lint chart
make lint
make lint docs

# Dependency regeneration after changing the bundled chart.
make deps

# make generate refuses this Orca worktree path. Run it only from a disposable copy whose
# absolute path ends in /gpustack.ai/gpustack, then compare and copy generated outputs back.
make generate

# Read-only AWS/EKS preflight before a paid test cluster is created.
aws sts get-caller-identity
aws configure get region
aws ec2 describe-availability-zones --region us-east-1 \
  --filters Name=state,Values=available
aws ec2 describe-instance-type-offerings --region us-east-1 \
  --location-type availability-zone \
  --filters Name=instance-type,Values=t3a.medium
aws service-quotas get-service-quota --region us-east-1 \
  --service-code ec2 --quota-code L-1216C47A

# From testing/infra/clusters/eks after T1 adds availability_zone_index.
terraform init
terraform fmt -check -recursive
terraform validate
terraform plan \
  -var='region=us-east-1' \
  -var='gpu_instance_types={}' \
  -var='cpu_instance_types={topology-a={instance_types=["t3a.medium"],node_count=2,availability_zone_index=0},topology-b={instance_types=["t3a.medium"],node_count=2,availability_zone_index=1}}' \
  -var='efa_enabled=false' \
  -var='switch_kube_context=false'

# Run the named gpustack-operator-chart-e2e and gpustack-operator-e2e skills for live tests.
# The EKS apply and final destroy or state handoff belong to the implementation phase.
```

The confirmed test shape is four `t3a.medium` x86_64 Nodes, two in each of two EKS availability
zones in `us-east-1`. Each instance has 2 vCPU and 4 GiB memory. The planning-time Linux on-demand
quote is USD 0.0376/hour per instance, or USD 0.1504/hour for compute; EKS control-plane, NAT, EBS,
and transfer charges are additional and the quote must be refreshed before apply. `t3a.medium` is
offered in the candidate zones, the requested 8 vCPU are below the account's 640-vCPU
standard-instance quota, and EKS 1.34 is in standard support. Every paid run records elapsed create
and ready durations. Its final owner either destroys the cluster and verifies charge-bearing resources
are absent, or transfers the only Terraform state copy to a named next owner who accepts the running
cluster and its cost.

### Project Structure

```text
api/worker/v1alpha1/
  topology_source.go                       # cluster topology input/status contract
  model_deployment.go                      # role requiredLevel API
pkg/worker/controllers/worker/
  topology_source.go                       # source lifecycle and snapshot validation
  topology_source_node_feature.go          # per-source NodeFeature publication and cleanup
  topology_source_configmap.go             # ConfigMap snapshot adapter
  topology_source_webhook.go               # bounded HTTPS adapter
  node_topology.go                         # profiles and Kueue Topology objects
  node_flavor.go                           # immutable hardware x profile ResourceFlavors
  node_queue.go                            # stable TAS-only queue groups, quota, and transition
  model_deployment_pod_group.go            # per-replica Kueue annotation
pkg/worker/webhooks/worker/
  topology_source.go                       # source union and ownership validation
  model_deployment.go                      # requiredLevel syntax validation
deploy/gpustack-operator/chart/             # parent values and optional Topograph dependency
hack/deploy/gpustack-operator/chart/charts/topograph/
                                             # reproducible patches only when upstream needs adaptation
testing/infra/clusters/eks/                 # deterministic cross-AZ CPU groups and AWS Pod Identity
.agents/skills/gpustack-operator-e2e/cases/ # source-to-admission live cases
.agents/skills/gpustack-operator-chart-e2e/ # disabled/enabled chart installation cases
docs/architecture/                          # discovery, normalization, and scheduling-chain contract
```

Generated CRDs, deepcopy, conversion, protobuf, OpenAPI, API-service, and webhook artifacts remain
in their existing generator-owned locations. They are not hand-edited. Because this worktree's
absolute path does not satisfy the repository generator guard, the implementation creates a
disposable physical copy ending in `/gpustack.ai/gpustack`, runs `make generate` there, copies only
generator-owned diffs back, and verifies both copies have identical hashes for every generated
file. The primary checkout is never used as scratch space.

### Code Style

```go
if level := role.Topology.RequiredLevel; level != "" {
	meta.Annotations[kueue.PodSetRequiredTopologyAnnotation] = level
}
```

The annotation is placed on the Pod metadata consumed by Kueue's Pod integration. Use Kueue's
constant, keep the label key opaque, and do not build a `PodSetTopologyRequest` in GPUStack code.
Exported API fields document the per-replica boundary and the Pending behavior. Go comments contain
no spec task identifiers.

### Implementation Plan

- [x] **T1 - Make the EKS fixture produce deterministic low-cost cross-AZ CPU nodes**
      Blocked by: None
      Owns: `testing/infra/clusters/eks/`
      Suggested worker: Qwen under `$my-crew`; the lead reviews all Terraform and performs paid actions.
      Gate: infrastructure and cost review before `terraform apply`
      Acceptance: an optional `availability_zone_index` on an ordinary CPU node-group selects
      exactly one of the module's public subnets; omission preserves current behavior. The fixture
      accepts two 2-Node `t3a.medium` groups pinned to different zones with GPU groups empty and EFA
      disabled. An optional, disabled-by-default EKS Pod Identity path sets the managed Nodes'
      IMDSv2 response hop limit to 2 so the per-node broker can read its identity, and binds only
      the Topograph API `gpustack-system/gpustack-operator-topograph` ServiceAccount to an IAM role
      granting `ec2:DescribeInstanceTopology`; no wildcard action or node-instance credential reuse
      is introduced. Outputs expose cluster name, region, selected
      zones, node-group names, and kubeconfig command without switching the caller's context.
      Verify: `terraform fmt -check -recursive`, `terraform init`, `terraform validate`, and the
      exact `terraform plan` under Commands. The generated VPC subnet IDs are unknown until apply,
      so inspect the JSON plan's `selected_zones` output to prove the two one-subnet selections are
      distinct, then prove desired/min/max size 2, no GPU group, no EFA group, and no Pod Identity
      resources unless enabled. Re-run the plan with legacy inputs and prove no forced change.

- [x] **T2 - Prove the queue model against live Kueue TAS before product implementation**
      Blocked by: T1
      Owns: `.agents/skills/gpustack-operator-e2e/cases/case-86.sh` and
      `.agents/skills/gpustack-operator-e2e/cases/_topology-tas-lib.sh`; no production controller
      files
      Suggested worker: lead agent; this is a core decision gate.
      Gate: TAS feasibility review; stop and amend this spec if any assertion fails
      Acceptance: on a fresh EKS 1.34 cluster with bundled Kueue 0.18.9, hand-built disposable
      Topology/ResourceFlavor/ClusterQueue/LocalQueue objects prove: implicit TAS without an explicit
      level; explicit zone placement; a two-Pod group fitting in one two-Node zone; a three-Pod group
      remaining Pending although four Nodes exist across the cluster; CPU and memory fitting from
      the complete PodSpec; behavior of the GPUStack pseudo-resource used as the queue's covered
      resource; one-flavor-per-covered-resource profile fragmentation; and multi-role joint
      admission without leaked partial reservations. The run uses only newly created queues and
      deletes every case fixture afterward. The cluster lifecycle follows the final destroy or
      explicit state-handoff gate.
      Verify: the case reads Kueue-created Workloads, `status.admission`,
      `status.admissionChecks`, `status.conditions`, and `status.topologyAssignment`, then reads each
      bound Pod's Node and that Node's zone. Positive assertions compare all Pod zones; the negative
      assertion also proves total cluster capacity was otherwise sufficient. Capture controller
      versions and object YAML. The final cluster owner either runs `terraform destroy` and verifies
      absence through AWS list/describe calls, or validates a single transferred state in the next
      worktree.

- [x] **T3 - Add stable public API and admission contracts**
      Blocked by: T2
      Owns: `api/worker/v1alpha1/topology_source.go`, the `ModelDeploymentRole` topology field in
      `api/worker/v1alpha1/model_deployment.go`,
      `pkg/worker/webhooks/worker/topology_source.go`, ModelDeployment validation, and generator-owned
      outputs
      Suggested worker: lead agent.
      Gate: API compatibility and security review
      Acceptance: the cluster-scoped `TopologySource` expresses selector, ordered levels, exactly
      one of node-label/ConfigMap/webhook source, namespaced references, intervals, timeout,
      staleness, CA and one credential mode, and the documented status fields. `ModelDeploymentRole`
      adds optional `topology.requiredLevel`. Omission round-trips without changing existing YAML.
      Validation rejects an empty hierarchy, hostname assignment, non-HTTPS or credential-bearing
      URLs, redirects as configuration, cross-namespace references, multiple authentication modes,
      prohibited prefixes, invalid durations, and invalid label keys.
      Verify: table-driven API/webhook tests include one valid and one rejecting case for every
      union arm and predicate. Run `make generate` in the disposable module-suffixed copy, copy back
      only generated outputs, compare SHA-256 hashes between copies, then run
      `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test ./api/worker/v1alpha1/... ./pkg/worker/webhooks/worker/...`
      and `make lint`.

- [x] **T4 - Implement read-only node-label discovery and safe label ownership**
      Blocked by: T3
      Owns: `pkg/worker/controllers/worker/topology_source.go`, shared snapshot validation and ownership
      helpers, RBAC/watch setup, and focused tests
      Suggested worker: lead agent.
      Gate: controller ownership review
      Acceptance: node-label sources select but never mutate Nodes; writing-source validation is
      transactional; a writing source publishes only private or delegated labels through a distinct
      NFD NodeFeature, never by directly changing a Node; omission, expiry, source deletion, and
      finalization remove its NodeFeature; a matching pre-existing standard region/zone label can
      be read as a snapshot parent but is never published or cleaned up;
      prohibited Device Manager/NFD,
      Topograph, hostname, and foreign prefixes are never written. Overlapping selectors or owned
      keys produce conditions and no contested writes.
      Verify: fake-client and envtest tables cover create/update/delete, NodeFeature drift repair,
      stale finalizer, overlapping sources, selector changes, unknown Nodes,
      incomplete parent tuples, and prefix rejection. A deliberately seeded foreign label must
      survive cleanup. Run focused controller tests with `-count=1` and `make lint`.

- [x] **T5 - Add the ConfigMap snapshot adapter**
      Blocked by: T4
      Owns: `pkg/worker/controllers/worker/topology_source_configmap.go` and focused tests
      Suggested worker: Kimi under `$my-crew`.
      Gate: none; lead review before merge
      Acceptance: JSON and YAML decode into the documented version/revision/nodes schema, duplicate
      YAML keys and unknown fields are rejected, watches are limited to referenced ConfigMaps, a
      valid revision is applied atomically, the last valid revision is retained only through
      `maxStaleness`, and expiry removes source-owned NodeFeatures without editing Nodes.
      Verify: table-driven tests use real ConfigMap objects and a controllable clock for missing key,
      malformed payload, duplicate key, unknown Node, invalid hierarchy, valid refresh, stale,
      expiry, and recovery. Run the focused package tests and `make lint`.

- [x] **T6 - Add the bounded authenticated HTTPS webhook adapter**
      Blocked by: T4
      Owns: `pkg/worker/controllers/worker/topology_source_webhook.go` and focused tests
      Suggested worker: lead agent because the network and credential boundary is security-critical.
      Gate: security review
      Acceptance: the client accepts only validated HTTPS destinations, uses referenced CA and
      exactly one bearer-token or client-certificate credential, does not follow redirects, ignores
      environment proxy configuration for the destination, bounds response bytes and request time,
      redacts credentials and response bodies from events/status/logs, and shares ConfigMap's atomic
      validation and staleness behavior. Secret and CA watches are namespace- and reference-scoped.
      Verify: an `httptest` TLS server covers custom CA, bearer and mTLS success, bad certificate,
      redirect, timeout, oversized body, 4xx/5xx, rotation, recovery, and redaction. A malicious URL
      table covers HTTP, userinfo, fragments, and unsupported schemes. Run focused tests with the
      race detector where supported and `make lint`.

- [x] **T7 - Vendor the optional Topograph chart without runtime installation logic**
      Blocked by: None
      Owns: `hack/deps.sh`, `deploy/gpustack-operator/chart/`, Topograph dependency lock/archive and
      reproducible files under `hack/deploy/gpustack-operator/chart/charts/topograph/`
      Suggested worker: Qwen under `$my-crew`.
      Gate: chart, licensing, and Kubernetes-version review
      Acceptance: Topograph chart 1.0.0/application v1.0.0 is pinned, disabled by default, and
      rejected below Kubernetes 1.27 when enabled. Production values cannot select the test provider.
      Parent values expose provider, Kubernetes engine, registry/image overrides, broker selector,
      security context, and credentials without copying upstream source. The NVIDIA example selects
      NFD's NVIDIA PCI-present label so broker Pods exist only on matching Nodes; the shared control
      plane is governed solely by `topograph.enabled`. The AWS example uses exactly
      the chart-managed `gpustack-system/gpustack-operator-topograph` ServiceAccount required by
      T1's optional Pod Identity association. The broker retains its distinct, chart-managed
      Kubernetes identity and uses node-local IMDSv2.
      Verify: `make deps` twice produces no second diff; disabled, generic, NVIDIA, and AWS renders
      are asserted; Kubernetes 1.26 enabled rendering fails and 1.27 succeeds; license notices are
      present; run `make lint chart` and upstream-compatible Helm tests. T12 owns and runs the
      enabled chart e2e install case against the fresh test cluster.

- [x] **T8 - Resolve stable topology profiles and reconcile Kueue Topology objects**
      Blocked by: T4
      Owns: `pkg/worker/controllers/worker/node_topology.go`, the operator-owned profile label, and
      focused tests
      Suggested worker: lead agent.
      Gate: topology semantics review
      Acceptance: a ready source gives each selected Node exactly one deterministic profile based on
      its ordered present-key set; partial Nodes use the longest valid prefix; unclaimed/conflicted
      Nodes use hostname-only; Topograph closest-first fabric tiers are reversed only in the adapter;
      independent dimensions are never guessed into one tree. One deterministic Kueue Topology per
      ordered profile always ends in hostname. Deletion before queue use self-heals. The profile
      label is operator-owned and does not enter a source NodeFeature. Its value is
      `fnv64-<16 lowercase hexadecimal digits>` and its Topology is named
      `gpustack-fnv64-<16 lowercase hexadecimal digits>`, both from the project's FNV-1a 64-bit
      helper over the NUL-delimited ordered level keys. Swapping two keys changes the identity;
      changing only domain values does
      not. An immutable generated Topology drift fails reconciliation instead of being rewritten. A
      later ordered-key change creates a different deterministic profile and Topology; it does not
      mutate the old object. T9 and T10 own replacement flavor creation, the stable-identity queue
      switch, and retirement ordering.
      Verify: tables cover cloud-only, rack, variable-depth fabric, accelerator domain, partial,
      contradictory, overlapping, and hostname-only inputs. Tests inspect ordered Kueue levels and
      actual Node labels, not derived summaries. Deleting a generated Topology must recreate an
      equivalent object; injecting immutable Topology drift must leave the Node profile unchanged.
      Changing a source from region/zone to region/zone/rack must produce a distinct replacement
      profile and leave the old Topology available until no ResourceFlavor refers to it. Golden
      vectors cover hostname-only, region/zone/hostname, reversed-order keys, NUL separation, and
      identical keys with different domain values; no generated profile or Topology name uses the
      prior SHA-256/base32 form. A forced digest-collision fixture must fail without reusing or
      rewriting an object for the other ordered schema.

- [x] **T9 - Create and retire immutable profile-qualified ResourceFlavors safely**
      Blocked by: T2, T8
      Owns: `pkg/worker/controllers/worker/node_flavor.go` and focused tests
      Suggested worker: lead agent.
      Gate: Kueue ResourceFlavor review
      Acceptance: the existing hardware identity is multiplied only by non-empty, disjoint topology
      profiles; every new flavor selects its hardware and operator profile labels and references the
      matching Topology at initial creation. Tolerations and selectors preserve current semantics.
      No legacy full-capacity flavor is created. Immutable drift returns an actionable error rather
      than rewriting the object. A post-creation profile change creates the complete replacement
      flavor set without mutating the old set; referenced old flavors remain until T10 switches the
      ClusterQueue, and no-contributor old flavors are then deleted. ResourceFlavor has no status
      subresource and T9 does not compete with the InstanceType status writer; T10 owns the
      user-visible queue transition and failure state.
      Verify: reconcile fresh fixtures twice for idempotence; compare union/intersection of selected
      Nodes for disjointness and completeness; inject immutable field drift and assert no update.
      Change a live profile and prove the new flavor exists before the referenced old flavor can be
      deleted; after removing the old ClusterQueue reference, prove the no-contributor flavor is
      deleted and its finalizer may delay completion. The old managed Topology is deleted only after
      no Node profile and no ResourceFlavor reference remain; an unrelated Topology is preserved.
      Focused tests must include CPU-only and
      accelerator identity fixtures, then run `make lint`.

- [x] **T10 - Reconcile identity-stable TAS-only ClusterQueues with conserved quota**
      Blocked by: T9
      Owns: `pkg/worker/controllers/worker/node_queue.go` and focused tests
      Suggested worker: lead agent.
      Gate: queue ownership and admission review
      Acceptance: NodeQueue remains the only writer of ClusterQueue resource groups. On creation,
      every flavor is TAS-enabled and each physical Node contributes quota exactly once. On a later
      profile-key change, NodeQueue waits for the complete verified replacement flavor set and then
      updates `spec.resourceGroups` on the same managed ClusterQueue; its name and UID remain
      unchanged. It never deletes and recreates the ClusterQueue, and never requires deletion of the
      InstanceType or LocalQueue. Old no-contributor flavors are retired only after the successful
      switch. NodeQueue always sets `HoldAndDrain`, waits for observed
      `status.reservingWorkloads=0`, switches the complete resource groups while held, and restores
      the exact pre-migration stop policy afterward. It NEVER relies on flavor removal to trigger eviction. The
      implementation replaces the current 16-item chunking behavior for this path and emits at most
      one resource group per covered resource with at most 64 flavors. A 65th required flavor,
      duplicate covered resource, overlapping selector, missing Topology, immutable drift, or
      non-conserved quota leaves the last complete queue plan unchanged and reports the exact cause.
      ClusterQueues created by a previous published version are never adopted.
      Verify: table-driven tests cover 0, 1, 16, 17, 64, and 65 flavors; an injected duplicate
      profile must fail. Compare per-resource quota to independently counted Node capacity and
      inspect the actual ClusterQueue. Record a managed ClusterQueue's name and UID, change its
      hierarchy from region/zone to region/zone/rack, and assert both identities are unchanged while
      all flavor references and quota converge. Inject failures before dependency completion, on
      the hold update, while reservations remain, on the resource-group switch, while restoring
      admission, and during old-flavor deletion. Each retry must expose one complete queue plan and
      eventually release the old ResourceFlavor and Topology. Include an admitted Workload
      reservation on an old flavor and prove resource groups do not switch until Kueue has drained
      `reservingWorkloads` to zero; GPUStack never patches the Workload itself. Seed a pre-existing
      non-TAS queue attributed to an earlier release and assert it is untouched with an
      unsupported-existing-object condition. Run focused tests and `make lint`.

- [x] **T11 - Render and observe per-replica ModelDeployment topology requests**
      Blocked by: T3, T10
      Owns: `pkg/worker/controllers/worker/model_deployment_pod_group.go`, the narrow existing status
      path it calls, and focused tests
      Suggested worker: Kimi under `$my-crew`; lead owns the API/status review.
      Gate: exported behavior and rollout review
      Acceptance: every Pod in one replica group receives Kueue's
      `kueue.x-k8s.io/podset-required-topology` annotation before the existing PodSpec hash is
      computed; omission emits no annotation; each replica remains an independent Workload; changing
      the field rolls only affected groups. GPUStack never synthesizes a Workload. Existing progress
      text identifies the role, replica, and downstream inadmissible Workload without inventing a
      stable parsed reason enum.
      Verify: focused render/controller tables cover omission, standard region/zone, GPUStack rack,
      Topograph tier, existing scale-up domain, one/many Pods, one/many replicas, invalid key, and
      update, and prove the annotation participates in rollout hashing. This repository has no
      envtest manager that runs Kueue's Pod integration; T12 therefore owns the non-simulated check
      of the Kueue-created Workload PodSet's `topologyRequest.required` on the live test cluster.

- [x] **T12 - Prove source-to-placement behavior on the fresh CPU EKS topology**
      Blocked by: T5, T6, T7, T10, T11
      Owns: `.agents/skills/gpustack-operator-e2e/cases/case-87.sh`,
      `.agents/skills/gpustack-operator-e2e/cases/case-88.sh`,
      `.agents/skills/gpustack-operator-e2e/cases/case-19.sh`,
      `.agents/skills/gpustack-operator-chart-e2e/cases/case-8.sh`, and topology-specific manifests
      referenced only by those files; it consumes but does not modify T2's shared TAS helper
      Owner: lead agent runs the cluster and adjudicates failures.
      Gate: end-to-end release review
      Acceptance: one paid run on the exact four-Node/two-AZ shape covers native EKS region/zone
      labels through a node-label source, a ConfigMap-provided rack hierarchy, an authenticated test
      webhook refresh/expiry/recovery, and Topograph AWS provider readiness using optional Pod
      Identity. A `size: 2` CPU ModelDeployment constrained to zone is admitted and both Pods bind in
      one zone; a `size: 3` request remains Pending because neither zone has three Nodes although the
      cluster has four; omission is admitted on one compatible profile; hostname-only capacity is
      excluded from an explicit zone/rack request; multi-role joint admission completes or cleanly
      remains unadmitted without leaked reservations. During the same run, adding rack to the live
      source hierarchy records the managed ClusterQueue name and UID before the change, proves both
      remain identical after its flavor references converge, and proves the old no-contributor
      ResourceFlavor and Topology are eventually released. Tests read actual labels, Topologies,
      flavors, queue quotas, Workload assignments, and Pod Nodes.
      Verify: run the named chart-e2e and operator-e2e cases against the captured kubeconfig context,
      with unique fresh object names. Re-run the negative observation for at least two reconciliation
      intervals, delete/recreate one generated unused Topology, rotate webhook credentials, and
      inspect all conditions. Run the live profile transition once without reservations and once
      while a Workload holds quota on the old flavor. In the latter case, observe `HoldAndDrain`,
      prove the old flavor remains referenced until `status.reservingWorkloads` reaches zero, then
      prove the complete switch, normal admission restoration, and eventual Kueue readmission. The
      operator must not recreate the ClusterQueue or leak old objects after references clear. A real
      GPU Instance and ModelDeployment must prove the Node, Devices ledger, flavor, admission, and
      bound Pod agree on one profile. Complete the destroy or state-handoff gate from T2.

- [x] **T13 - Document creation and live-profile transition operations**
      Blocked by: T12
      Owns: the pages selected through the `gpustack-operator-docs` skill and `docs/README.md`
      Suggested worker: lead agent.
      Gate: documentation and handoff review
      Acceptance: docs explain Topograph's purpose and boundary, enablement/version/provider choices,
      NVIDIA broker selection, AWS permission, generic snapshot schema, webhook trust/RBAC/egress,
      label ownership and NodeFeature cleanup, hierarchy validation, profile fragmentation, 64-flavor
      limit, full-PodSpec TAS fitting, per-replica `requiredLevel`, Pending diagnosis, EKS validation
      shape and cost caveat, stable ClusterQueue name/UID during profile changes, immutable
      dependency replacement and retirement order, the `HoldAndDrain` service interruption and
      Kueue-owned eviction/readmission semantics, compact FNV-1a 64-bit profile naming, and the
      explicit absence of adoption or upgrade support for objects from previously published versions.
      Examples never use host-specific paths, timestamps, account IDs, or cluster IDs.
      Verify: follow every example on a clean fixture where practical, check page Contents/footer and
      `docs/README.md` index ownership, then run `make lint docs` and search for stale queue-recreation,
      zero-disruption, or aggregate-capacity promises.

- [x] **T14 - Publish private topology through NodeFeatures**
      Blocked by: T13
      Owns: `pkg/worker/controllers/worker/topology_source*.go`, focused tests,
      `.agents/skills/gpustack-operator-e2e/cases/case-87.sh`, and the topology architecture and
      operations pages.
      Gate: local controller tests, lint, documentation checks, and shell syntax validation.
      Acceptance: ConfigMap and webhook sources publish only private or delegated labels in a
      source-owned NFD `NodeFeature` for each described Node. The default inventory keys are
      `topology.gpustack.ai/region`, `/zone`, and `/rack`. The source never directly edits a Node or
      publishes `topology.kubernetes.io/region` and `/zone`; those keys are read-only parents only
      when already present with matching values. An invalid parent rejects the complete snapshot
      before any `NodeFeature` changes. NFD projects the private labels to Nodes, after which the
      existing profile, ResourceFlavor, ClusterQueue, and TAS path consumes them unchanged.
      Snapshot omission, expiry, source deletion, and switching to `nodeLabels` remove the
      source-owned `NodeFeature`, while drift in that object is repaired. Read-only `nodeLabels`
      sources and the existing hardware
      `NodeFeature` remain unchanged. Test documentation and API examples use the same contract.
      Verify: focused fake-client tests cover publication, NFD metadata, no direct Node edits,
      standard-parent matching/missing/mismatch, drift repair, omission, expiry, and webhook
      recovery, and switching from a writing source to `nodeLabels`. Run `make lint`,
      `make lint docs`, shell syntax/lint, and the controller tests locally. Update Case 87 to assert
      source-owned NodeFeatures and the absence of standard region/zone labels in their specs.

### Test Plan

- [x] I/we understand the repository's tests, generation guard, chart dependency workflow, and
      named EKS/chart end-to-end skills, and will add coverage at the lowest layer that can observe
      each contract.

#### Prerequisite testing updates

- Extend `testing/infra/clusters/eks` with optional per-CPU-group AZ selection and optional minimal
  Topograph AWS Pod Identity. Validate both new and legacy plans before creating paid resources.
- Add a reusable live assertion helper that reads the actual Workload `TopologyAssignment`, bound
  Pod Node names, and Node label values. It must not infer placement from object names or from the
  test's requested zone.
- Add fixtures that create unique ResourceFlavors and ClusterQueues for every run. No test upgrades
  a queue from a previously published version. Within one run, mutate the source hierarchy and
  assert the newly created managed ClusterQueue itself is updated rather than replaced.
- Add a deliberate failing fixture for the 65-flavor limit and a cross-AZ `size: 3` workload whose
  total cluster capacity is sufficient, preventing vacuous Pending assertions.

#### Unit tests

- `api/worker/v1alpha1` and `pkg/worker/webhooks/worker`: by 2026-09-22, cover at least 85% of new or
  changed non-generated statements. Tables cover tagged-union cardinality, namespaced references,
  duration bounds, label/prefix rules, HTTPS/auth rules, and `requiredLevel` syntax/omission.
- `pkg/worker/controllers/worker` source adapters: by 2026-09-22, cover at least 85% of new or changed
  non-generated statements. Include transactional validation, NodeFeature ownership and cleanup, finalizer,
  stale/expired/recovery clocks, TLS/auth/redirect/timeout/body bounds, overlap, and restart.
- `pkg/worker/controllers/worker` topology/flavor/queue/ModelDeployment paths: by 2026-09-22, cover at
  least 80% of new or changed non-generated statements. Include every profile shape, exact quota,
  immutable drift, stable queue identity across a profile transition, dependency creation/switch/
  retirement failure injection, `HoldAndDrain` reservation gating, FNV-1a naming golden vectors,
  64/65 boundary, one-writer ownership, rollout hash, and downstream progress.
- `testing/infra/clusters/eks`: no statement-coverage target because it is Terraform. Required gates
  are format, validate, JSON-plan structural assertions, legacy-input no-regression, and a verified
  final destroy or exclusive state handoff.
- Helm/chart templates: no statement-coverage target. Golden/render tests cover disabled, generic,
  NVIDIA, AWS, invalid test provider, Kubernetes 1.26 rejection, Kubernetes 1.27 acceptance, and
  registry/image overrides.

#### Integration tests

- Envtest runs source reconciliation against real Node, ConfigMap, Secret, CRD, finalizer, and status
  objects. At least one cleanup test changes a label out-of-band and proves it survives.
- Envtest with Kueue's real Pod integration creates Pods, waits for Kueue-created Workloads, and
  inspects PodSet topology requests. GPUStack test code MUST NOT create the Workload under assertion.
- Controller integration creates a fresh InstanceType chain and checks actual Kueue Topology,
  ResourceFlavor, ClusterQueue, and LocalQueue objects. It independently counts selected Nodes and
  compares quota; a duplicate-profile mutation and 65th flavor must be rejected. It then changes the
  source hierarchy, records the ClusterQueue name/UID, and verifies dependency-first replacement,
  `HoldAndDrain` until `reservingWorkloads=0`, the complete in-place resource-group switch, admission
  restoration, and ordered old dependency retirement.
- The T2 feasibility case runs before controllers are changed and answers full-PodSpec fit,
  pseudo-resource coverage, implicit TAS/profile fragmentation, explicit domains, and joint
  admission. A failure changes the design before T3 begins.

#### End-to-end tests

- Use a clean EKS 1.34 cluster in `us-east-1` with two `t3a.medium` Nodes in AZ index 0 and two in AZ
  index 1. Begin with GPU node groups empty and EFA disabled; add one NVIDIA T4 Node for accelerator
  admission after the CPU topology proof. All tested ClusterQueues are newly created for this run.
- Prove native region/zone observation, ConfigMap rack writing/cleanup, authenticated webhook
  refresh/staleness/expiry/recovery, and Topograph AWS readiness with the API limited to
  `ec2:DescribeInstanceTopology` through Pod Identity and the broker limited to node-local IMDSv2.
- Positive placement: `size: 2` requires zone, receives admission and a TopologyAssignment, and both
  Pods bind to Nodes whose observed zone values are identical.
- Negative placement: `size: 3` requires zone, remains unadmitted for at least two reconciliation
  intervals, while the test independently proves four schedulable Nodes and sufficient aggregate
  CPU/memory exist. It MUST NOT merely assert that Pods are Pending.
- Compatibility: omission creates no explicit request and can use one compatible TAS flavor;
  hostname-only Nodes cannot satisfy explicit zone/rack; tests do not claim capacity aggregation
  across profiles.
- Joint admission: a multi-role ModelDeployment either admits a mutually compatible set or remains
  wholly unadmitted without leaked reservations. Deleting it returns all quota.
- Live hierarchy transition: add rack after admission, assert the managed ClusterQueue name and UID
  do not change, observe `HoldAndDrain` and zero reserving Workloads before the flavor switch, then
  observe admission restoration, replacement placement, and eventual release of the old
  ResourceFlavor and Topology. Existing matching cloud region/zone labels serve as read-only parents
  in the rack snapshot and survive source deletion. The profile label and Topology name must match
  the FNV-1a golden calculation from ordered keys, not contain a domain value, and not use the old
  long hash form.
- Accelerator admission: a real GPU Pod requests `nvidia.com/gpu`, Kueue assigns a TAS flavor on the
  NVIDIA Node, and the Node, Devices ledger, flavor selector, and Pod placement agree on the profile.
  A real GPU Instance reaches Ready on the same node.
- Every paid test run captures versions and relevant object YAML. This run transfers the EKS
  Terraform state to the designated follow-on worktree and verifies it there; the new owner inherits
  the running cluster and eventual destroy obligation. A run without a handoff destroys the cluster
  and independently verifies charge-bearing fixture resources are absent.

#### Verification outcome

- Controller and API tests, `make lint`, `make lint chart`, and `make lint docs` passed. The EKS
  fixture passed `terraform fmt -check -recursive` and `terraform validate` before the state handoff.
- Operator E2E Case 87 passed every row on four CPU Nodes in two zones, including a watched
  `HoldAndDrain` → zero-reservations → in-place flavor switch, old dependency retirement, webhook
  expiry and recovery, actual same-zone Pod binding, cross-zone refusal, and both samples of the
  multi-role shortage case. The test uses fresh queues and leaves no case objects or test labels.
- Operator E2E Case 88 passed on a real NVIDIA Node: Node and Devices profiles, ResourceFlavor,
  stable ClusterQueue identity, Kueue TAS assignment, per-card admission, and bound GPU Pod agreed.
  Operator E2E Case 19 passed a real GPU Instance through the aware type and confirmed the card
  inside its Pod. Chart E2E Case 8 passed the AWS provider, Pod Identity, and every node broker.
- The final EKS fixture remains running for the designated next owner. Its current Terraform state
  and backup were moved to that worktree, hashes matched, and `terraform state list` there returned
  114 addresses after initialization. The originating worktree retains no state copy. The handoff
  warns that the receiving Terraform module must be aligned before any plan, apply, or destroy.

## Alternatives

### Reimplement all Topograph providers in GPUStack

Rejected. Cloud APIs, fabric-management tools, node brokers, graph normalization, and stale-label
cleanup are already Topograph's maintained concern. GPUStack only adds the generic source contract
Topograph lacks and the Kueue/ModelDeployment consumer it does not attempt to provide.

### Fork Topograph to add file and webhook providers

Rejected. It couples the operator release to a second Go service fork and image pipeline. The
GPUStack source controller is small, owns GPUStack labels, and can coexist with an unmodified pinned
Topograph release. An upstream generic provider can replace it later only through an explicit
migration of label ownership.

### Deploy Topograph only after an NVIDIA Node appears

Rejected. Helm dependencies are selected at release render time, while Nodes appear and disappear
at runtime. Topograph is also useful for non-NVIDIA cloud and fabric topology. Explicit subchart
enablement plus node-selected provider agents has deterministic lifecycle and follows the existing
per-manufacturer DaemonSet pattern.

### Add topology labels to the existing ResourceFlavor selector

Rejected. Equality selectors answer "this named rack" rather than "one rack, whichever fits" and
would multiply flavors by every domain value. Kueue TAS is the component that selects a domain for
the complete PodSet.

### Keep both the old flavor and a full-capacity TAS flavor

Rejected. Both flavors describe the same nodes, so their nominal quotas double one physical pool.
Profile-partitioned TAS-only queues count capacity once and let omission remain implicit TAS, while
retaining Kueue's one-flavor-per-covered-resource fragmentation boundary.

### Put every discovered topology dimension into one Kueue Topology

Rejected. Kueue requires one ordered tree, while an accelerator domain and a switch hierarchy can
cross. Only a validated hierarchy becomes schedulable; other labels remain observable for a
separately designed policy rather than being flattened into a false parent-child relation.

### Apply one topology request to all replicas in a role

Rejected for the current API. Each replica is an independent Kueue Workload, so there is no single
PodSet spanning the role. The proposal accurately constrains the `size` Pods that already share one
admission unit.

## Open Questions

None. The feature intentionally starts with one required level per replica. Preferred placement,
multi-layer slices, cross-role PodSet groups, and multiple simultaneous non-nested topology
dimensions require separate user stories and Kueue admission designs.

## What Does Not Close This

- Adding labels or a Topograph chart without connecting managed ResourceFlavors and ModelDeployment
  pod groups to Kueue TAS.
- Adding levels while leaving unlabeled nodes, overlapping sources, and stale topology undefined.
- Creating a second full-capacity flavor for TAS and thereby counting the same nodes twice.
- A fixture or rendered-object unit test without a live Kueue admission that proves fitting Pods stay
  in one domain and a cross-domain-only request remains Pending.
- A reading on only one manufacturer domain shape. Both the existing two-segment and three-segment
  values must pass through as opaque identities.
- Claiming the issue is closed because `feature.gpustack.ai/fabric.domain` already exists. The issue
  is about discovery breadth and the scheduling consumer.
- An upgrade or migration-only demonstration. This iteration closes through freshly created TAS
  objects, live ModelDeployment placement, and a subsequent profile transition that preserves the
  managed ClusterQueue name/UID while safely draining reservations and retiring old dependencies.
- A Pending assertion that does not independently prove aggregate cluster capacity is sufficient,
  or a placement assertion inferred from requested labels instead of bound Pod Nodes.
