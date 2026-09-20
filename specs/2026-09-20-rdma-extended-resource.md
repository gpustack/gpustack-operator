# Spec: RDMA as an allocatable resource

Status: Building
Blocked on: the implementation this document specifies, task by task under
[Implementation Plan](#implementation-plan). The Status becomes `Shipped` when the four resource
keys below are served by the Device Manager, every acceptance criterion under F1-F8 has a passing
test, and the documentation F8 names is written.

Type: Feature

## Summary

The `Devices` ledger already records every RDMA-capable interface this node has, where it sits on
the bus, which NUMA node it is attached to, and whether its link verifies. Nothing allocates them.
A container that needs one gets `/dev/infiniband` bind-mounted and nothing else, which carries the
device node into its mount namespace and leaves the device cgroup denying `open()` — this
repository already says so in the one place it costs something:
`pkg/worker/kvcache/mooncake/member_workload.go:94-99`, where the store is described discovering
zero HCAs and installing TCP instead, reporting no error.

This spec makes the Device Manager advertise **its own RDMA extended resources** — one key per
allocation mode, node-level and vendor-neutral — so an RDMA interface becomes a thing a container
requests, is granted, and can open. Because the grant is a device-plugin allocation, it carries a
`TopologyInfo`, which is the only way a resource participates in the kubelet's NUMA alignment at
all. That is what makes **node-local joint allocation of an accelerator and an RDMA interface**
possible: a container asking for both gets both on one NUMA node, or is refused.

It adds no CRD and no API field. Every fact it **schedules on** is already in
`Devices.spec.interfaces[]`; the one thing it reads outside that record is the character device an
RDMA device name resolves to, read from sysfs at allocation time rather than stored — see
[F5](#f5--the-allocation-response) and [Alternatives](#alternatives) for why that one is not a
field.

## Motivation

### Why we advertise our own rather than have the user install someone else's

Three findings, each with the artifact that carries it.

**Without it, the device does not open.** The mount is not the permission. The comment cited above
records the measurement: `EPERM` even for uid 0 on a node whose file mode already permits everyone,
because the device cgroup has no rule for it, and a device-plugin allocation is what adds that rule
without requiring the privileged flag. The failure is silent — a TCP transport under a spec that
says RDMA.

**Without it, joint allocation is structurally impossible.** The kubelet's device manager refuses to
align a resource that publishes no topology: `GetTopologyHints` — **upstream
`k8s.io/kubernetes`, `pkg/kubelet/cm/devicemanager/topology_hints.go`, not a path in this
repository** — drops a resource whose devices carry no `TopologyInfo` out of the hint merge
entirely. A plugin that does not report NUMA cannot be co-located with anything, however close the
hardware is. The widely deployed shared-RDMA plugin does not report it.

> Every claim about kubelet behaviour in this document is an **upstream** claim, marked as such
> where it appears. None of it is verifiable in this tree, and it is cited in a style that must not
> be mistaken for the in-tree citations beside it — a reader who follows an upstream path expecting
> a local file finds nothing and has no way to tell a stale claim from a mistyped one. The
> statements relied on are: a resource with no `TopologyInfo` is excluded from the hint merge; a
> container whose merged hint cannot be satisfied is refused with `TopologyAffinityError` under
> `restricted` and `single-numa-node`, admitted anyway under `best-effort`, and never consulted
> under `none`; and a refused Pod is not rescheduled onto another node.

**There is no standard to be non-standard against.** Surveying the field turns up four different
granularity models and no agreement on what a quantity means: the same `1` is a share of a pool in
one plugin and an exclusive virtual function in another. Two of those names are already legible in
this repository — `api/worker/v1alpha1/kv_cache_backend_test.go:152-158` accepts
`vpc.amazonaws.com/efa`, `rdma/hca_shared_devices_a` and `openshift.io/mlnxnics` as names a real
cluster might carry, precisely because no name could be hard-coded. Advertising our own therefore
costs no interoperability that existed.

That last point has a consequence this spec acts on: `KVCacheBackendTransport.DeviceResourceName`
(`api/worker/v1alpha1/kv_cache_backend.go:525-550`) exists only because the name had to be declared
by an administrator rather than derived. The keys defined here are the first names this operator can
derive. Deriving them is **not** in this round — see Non-Goals.

### The two asymmetries with the accelerator side, which decide the shape

**The resource is not contended the same way.** Accelerator compute is rival: `Shared` is
oversubscription, which is why `Sliced` exists at all and why it needs an interception library to
enforce a quota. An HCA's multiple queue pairs are how the hardware is meant to be used, and the
isolation is done by firmware and the kernel. **Several processes on one HCA is ordinary use, not
oversubscription.** So the RDMA side needs no injection and no interception, and `Sliced`'s entire
complexity lives there.

**The mode is not ours to choose.** An accelerator's allocation mode is something this operator
puts it into. RDMA's `Partitioned` is a fact of the node: `sriov_numvfs` was written before the
Device Manager started, possibly requiring a driver reload, and nothing here can change it at run
time. So the RDMA side **detects the node's state and offers what that state supports**; it does not
offer the operator a menu. The discriminator is already in the API and already documented as
independent: `DeviceInterface.SRIOV` (`api/worker/v1alpha1/devices.go:349-352`) states that being an
SR-IOV physical function is a separate fact from the length of `VirtualFunctions`, so "a PF with
zero VFs configured" and "not a PF at all" are two states and not one.

### Goals

- **Make an RDMA interface allocatable**, so the device cgroup admits the container that was granted
  one and the fabric is actually used.
- **Report `TopologyInfo` from the interface's own NUMA affinity**, so an accelerator and an RDMA
  interface can be jointly allocated on one NUMA node by the kubelet mechanism that already aligns
  accelerators. The reach of that mechanism is narrower than the sentence suggests and is stated
  exactly in [What this guarantees, and what it does not](#what-this-guarantees-and-what-it-does-not):
  the accelerator side publishes a hint for its whole-accelerator modes and **none at all for a
  hardware partition**, so this goal is met for the former and unreachable for the latter without
  work outside this change.
- **Derive the allocation mode from the node's own state**, so one DaemonSet serves a node with
  SR-IOV virtual functions and a node without, and neither needs configuring.
- **Withhold an interface whose link is known broken**, so a workload fails to schedule rather than
  failing a collective.
- **Write down what a quantity means in each mode**, because the downstream consumer sets a request
  from this document and from nothing else.
- **Say plainly what the mechanism does not guarantee**, because the guarantee stops at a boundary
  a reader would otherwise assume it crosses.

### Non-Goals

- **No engine-side consumption.** Making `KVCacheBackend` or a model deployment request these keys
  by itself is separate work that depends on the contract defined here. It is deliberately not done
  in passing, because a consumer written against a name that is still moving is a consumer that has
  to be rewritten.
- **No cross-mode exclusion.** The accelerator side reports a card held in another mode as
  `Unhealthy` (`pkg/deviceplugin/server.go:202-222`), because a card handed to an opposite-mode Pod
  fails `Allocate` permanently. **That does not transfer.** `Shared` and `Sliced` serving the same
  HCA is the correct semantics, so importing the rule would manufacture a conflict that does not
  exist. The only exclusion that remains is `Partitioned` against the rest, and it is decided by the
  hardware rather than by bookkeeping — see [C1](#c1--the-mode-is-read-off-the-node-never-chosen).
- **No VF creation and no SR-IOV configuration.** `sriov_numvfs` is read, never written. Creating a
  virtual function is a host write with no reliable teardown.
- **No external component is bundled or vendored.** No shared-RDMA device plugin, no topology
  service, no MOFED or DOCA driver installed on the user's behalf.
- **No topology-aware scheduling across nodes.** See
  [What this guarantees, and what it does not](#what-this-guarantees-and-what-it-does-not).
- **No change to `Devices`.** See [C5](#c5--nothing-is-written-to-devices).
- **No continuous link monitor.** The link verdict this feature gates on is the one the detector
  already writes on its own cadence. A link that drops between two passes is caught by the next one.

## Proposal

### The five contracts

Everything below is a contract because a downstream consumer reads it and cannot re-derive it from
the code: a quantity's meaning, a resource's name, and the conditions under which a device is
withheld are not visible from a `kubectl describe node`.

#### C1 — the mode is read off the node, never chosen

Per interface in `Devices.spec.interfaces[]`, exactly one of two branches applies:

| the interface is… | it serves | its endpoints are |
|---|---|---|
| `sriov == true` **and** `len(virtualFunctions) > 0` | `Partitioned` only | each of its virtual functions |
| anything else — a physical function with zero virtual functions configured, or not a physical function at all | `Exclusive`, `Shared`, `Sliced` | the interface itself |

A physical function that has virtual functions configured does **not** also serve the whole-function
modes. That is the one exclusion this feature keeps, and it is kept because the hardware already
made the choice: the node was put into a partitioned state before this process started. It is not
bookkeeping, it is a read, and it therefore needs no ledger, no reservation and no health lie.

The discriminator is `SRIOV` **and** the virtual-function count, both of them, because
`devices.go:349-352` states they are separate facts. Reading only the count would make "a physical
function configured with zero virtual functions" indistinguishable from "an ordinary NIC" — which
happens to reach the same branch here, but by accident rather than by rule, and the accident stops
holding the moment a fourth state is added. Reading only `SRIOV` would send a physical function with
no virtual functions into `Partitioned`, where it has nothing to offer.

#### C2 — the resource names, and what `1` means in each

| key | `1` means | tokens per endpoint |
|---|---|---|
| `device.gpustack.ai/rdma` | one whole physical function, exclusively | 1 |
| `device.gpustack.ai/rdma.shared` | one concurrent use of one HCA | `nodefeature.SharedResourceMaxSize` |
| `device.gpustack.ai/rdma.sliced` | **the same as `.shared`, word for word** | `nodefeature.SharedResourceMaxSize` |
| `device.gpustack.ai/rdma.partitioned` | one SR-IOV virtual function, exclusively | 1 per virtual function |

The prefix is the one this operator already owns for resources it advertises itself —
`nodefeature.VisibilityResourceNamePrefix` (`pkg/nodefeature/knowns.go:87-95`) is
`device.gpustack.ai/`, and `device.gpustack.ai/nvidia.visibility` is its existing inhabitant. The
suffixes mirror the accelerator families so that a workload asking for a sliced accelerator and a
sliced RDMA interface spells both requests the same way.

**`.shared` and `.sliced` draw on the same HCAs and do not decrement each other.** One HCA can
therefore carry `SharedResourceMaxSize` shared holders and `SharedResourceMaxSize` sliced holders at
once. This is stated here, in the contract, because it reads like a defect and is not one: queue-pair
concurrency is how an HCA works, the isolation is the firmware's and the kernel's, and the token
count is a scheduling knob rather than a hardware limit. `Sliced` on the RDMA side is `Shared` with a
different key, per the ruling that the two enums stay aligned; there is no quota to enforce and
therefore nothing to inject.

**The keys are invisible to the accelerator admission rules, by two independent mechanisms.**
`nodefeature.ResourceFamilyOf` (`pkg/nodefeature/knowns.go:603-630`) takes the
`device.gpustack.ai/` branch first and returns from it, so a name under that prefix never reaches
the suffix loop below; and that loop additionally requires the base to be a known accelerator
resource name, which `device.gpustack.ai/rdma` is not. So the "one accelerator family per Pod" rule
neither sees these keys nor can be made to. That is the intended outcome — asking for a shared
accelerator and a sliced RDMA interface in one container is legal — and it is recorded because it is
load-bearing and was measured rather than assumed. `ResourceFamilyOf` is the only **classifier**
that consumes the prefix; `GetAcceleratableResourceName` (`pkg/nodefeature/knowns.go:338`) composes
with it but decides nothing about a name handed to it.

**The other side of that invisibility: Kueue cannot see these keys either.** Node-devices admission
drives off the same classifier — `podSetFamilyDemands` (`pkg/worker/controllers/worker/node_devices_admission.go:178,190`)
classifies each requested key with `ResourceFamilyOf` — so an RDMA request contributes nothing to a
Workload's demand and no flavor or credit can be expressed for it. A queued Workload can therefore
be admitted far past the fleet's real RDMA capacity, and the excess surfaces as Pods that will not
schedule rather than as a queue that waits. That is ordinary Kueue behaviour for an extended
resource nothing covers, and it is stated here because the consumer work in
[Open Question 3](#3--deriving-deviceresourcename-from-these-keys) meets it immediately.

#### C3 — NUMA is reported, or nothing is

Every advertised device carries a `TopologyInfo` built from its endpoint's own `numaAffinity`:

- For a whole interface, `DeviceInterface.NumaAffinity`.
- For a virtual function, `DeviceInterfaceVirtualFunction.NumaAffinity`, falling back to its parent's
  when the virtual function's own is empty. This is not a new rule: `pkg/nodefeature/rdma.go:107-148`
  already resolves a virtual function's affinity exactly this way, and states the reason — a blank
  reading is the kernel declining to answer, not a different answer, and a virtual function sits on
  the same physical function as its parent.
- **When neither answers, no `TopologyInfo` is attached.** An empty affinity is the `-1` a
  virtualized host can report for every device it has. Naming node 0 instead would be worse than
  saying nothing: under the `single-numa-node` policy the hint is what decides admission, so the
  placement would succeed against a proximity claim nobody measured. This mirrors
  `pkg/deviceplugin/server.go:224-239` verbatim rather than inventing a second rule.

This contract is the one the feature's headline promise rests on. A resource with no `TopologyInfo`
is dropped from the kubelet's hint merge, so **an implementation that silently stopped reporting it
would keep allocating, keep passing every functional test, and quietly stop co-locating anything.**
That is why F3's acceptance asserts the value's provenance and not merely its presence.

#### C4 — the link gate

| `link.state` | advertised | health |
|---|---|---|
| `ok` | yes | `Healthy` |
| `unverified` | yes | `Healthy` |
| **absent (`link` is nil)** | **yes** | **`Healthy`** |
| `failed` | yes | **`Unhealthy`** |

The nil row is not a filler. `DeviceInterfaceLink` is documented as "nil when there is no RDMA link
to verify" (`api/worker/v1alpha1/devices.go:361-363`), and an endpoint reaches this gate only by
carrying an RDMA device name — so nil here means a bound device whose verdict this pass did not
record, which is the `unverified` case arriving by a different route. Leaving the row out left the
gate with no rule for a state the record really produces, and an implementation would have picked
one silently.

An endpoint is advertised at all only when it carries an RDMA device name, because that name is what
`Allocate` resolves to a character device; an endpoint with no name has nothing to hand over.

A `failed` endpoint keeps its tokens and reports them `Unhealthy` rather than dropping them from the
response. The kubelet assigns no unhealthy device to a new Pod, so the endpoint is not handed out —
and the existing holder's allocation is untouched, which is the reason the accelerator side already
gives for the same choice (`pkg/deviceplugin/server.go:202-212`): withdrawing an advertised ID
strands the kubelet's checkpointed allocation for whatever container holds it.

**"Untouched" reaches exactly as far as the container keeps running.** The same file records the
other half (`pkg/deviceplugin/server.go:270-276`): the kubelet checkpoints the exact IDs it offered
a container and, on any later allocation for it, refuses to proceed unless every one is **still
healthy** — a check that runs before the "no new devices needed" shortcut. So a container that
restarts, or a node whose kubelet restarts, cannot re-establish a holder's allocation while the link
is down; that Pod stays stuck until the link heals. That is the intended semantics — the link really
is down — but it is an interaction with the checkpoint, not a property of the advertisement, and
quoting only the adjacent paragraph made it read like one.

`unverified` is `Healthy` deliberately, and it is the same call
`DeviceInterfaceLinkStateUnverified` (`api/worker/v1alpha1/devices.go:460-467`) already documents for
the node label: a link this node could not interrogate must not silently exclude itself. Withholding
there turns "the port's state could not be read" into "this node has no RDMA", which is the larger
lie.

#### C5 — nothing is written to `Devices`

No field is added to `Devices.spec` or `Devices.status`, and no RDMA record is written to a Pod
annotation.

`Devices.status` is the accelerator allocation ledger, rebuilt wholesale each reconcile from
`Devices.spec` plus the Pod annotations. It exists because the accelerator side needs it to decide
things: cross-mode exclusion reads it, and the sliced-unit accounting is kept in it. **The RDMA side
decides nothing from a ledger.** C1's mode judgment is a hardware read, C4's gate is a link verdict
already in `Devices.spec`, and the capacity accounting is the kubelet's own count of healthy tokens.
A ledger here would carry no decision and would be a second copy of `Devices.spec` that every
reconcile re-derives — with the failure mode every such copy has, which is going stale without
anything noticing.

What an operator reads instead: the inventory and its link verdicts in `Devices.spec.interfaces[]`,
and, for "which endpoint did this container get", the container itself — the device nodes injected
under `/dev/infiniband` and the `NCCL_IB_HCA` value naming them.

The cost is stated rather than hidden: **there is no cluster-level view mapping a Pod to the HCA it
holds.** Reconstructing one means reading the kubelet's checkpoint or the container. That is accepted
for this round, and the one feature it blocks is recorded in
[Open Questions](#open-questions).

### What this guarantees, and what it does not

|  |  |
|---|---|
| Guaranteed | once a Pod is admitted on a node, the accelerator and the RDMA interface it was granted are on the same NUMA node — **and only when all four conditions below hold** |
| **Not guaranteed** | that the scheduler picks a node where that is satisfiable |

The second follows from the layering and not from an omission here. kube-scheduler and Kueue select
a node by the **quantity** of allocatable resources; neither can see inside a node's topology (and
for these keys Kueue does not select at all — see
[C2](#c2--the-resource-names-and-what-1-means-in-each)). A node whose totals are sufficient but
whose devices are split across two NUMA nodes is therefore selected, and the kubelet then refuses
admission with a `TopologyAffinityError` — **upstream `k8s.io/kubernetes`,
`pkg/kubelet/cm/topologymanager/scope_container.go`, not a path in this repository**. **A Pod
refused that way is not rescheduled onto another node.** This is a known gap, it belongs to a future
dynamic-resource allocation design, and it is out of scope here.

Four limits, each of which a reader would otherwise assume away. **The first is the one this
feature cannot detect on its own, and it voids the guarantee outright.**

- **⛔ A hardware partition contributes no hint, so the guarantee does not reach it.** The
  accelerator side publishes partition tokens with an ID and a health and **no `Topology` at all**
  (`pkg/deviceplugin/server.go:319-330`), which its own comment states is deliberate: a partition
  token "names no accelerator, so a hint would tell the TopologyManager something the token cannot
  honor" (`:258-260`). A container requesting a partition profile together with
  `device.gpustack.ai/rdma.partitioned` therefore offers the merge **exactly one hint — ours** —
  and is admitted aligned to the RDMA side alone, with the partition free to sit on the other
  socket. Nothing fails: the Pod runs, the device opens, the collective completes across the SMP
  interconnect. And **no `TopologyAffinityError` can ever fire**, because there is no second hint to
  conflict with — so the paired negative reading R3 relies on is structurally unobtainable on such
  hardware. This is a property of the accelerator side's partition pool, not something this feature
  can repair; changing it is separate work. Until it changes, **the guarantee covers the accelerator
  modes that publish a hint — `Exclusive`, `Shared` and `Sliced`, via `advertiseCard`
  (`pkg/deviceplugin/server.go:224-239`) — and not `Partitioned`.**
- **The NUMA guarantee is only as strong as the node's kubelet policy, and only two of the four
  policies give one.** `TopologyManager` takes `none` (the default), `best-effort`, `restricted` or
  `single-numa-node`. Under `none` the hints this feature publishes are computed and discarded.
  **Under `best-effort` a container whose hints cannot be satisfied is admitted anyway, misaligned**
  — the same silent shape as `none`, which an earlier revision of this section missed by carving out
  only `none` and implying the other three enforced. The guarantee holds under `restricted` and
  `single-numa-node`. The policy is node-level kubelet configuration and this operator cannot change
  it. F7 makes `preflight` report what the node is set to.
- **Both resources must be requested by the same container.** The kubelet's default topology scope is
  the container, so an accelerator in one container and an RDMA interface in another are aligned with
  each other by nothing.
- **The granularity stops at the NUMA node.** A topology hint's unit is a NUMA node; it cannot
  express a PCIe switch or a root complex. The vendor's own statement about GPUDirect RDMA is that
  sharing an upstream PCIe root complex is the only theoretical requirement, with same-switch as the
  best case — and **this repository cannot measure "same root complex" today**: `PciRootID`
  (`api/worker/v1alpha1/devices.go:301-306`) is documented as the outermost PCI bridge and explicitly
  not a root complex. **So a NUMA-level guarantee is not described here as a correctness guarantee
  for GPUDirect RDMA.** A switch-level preference is reachable through `GetPreferredAllocation` and
  `device.BusDistance`, but it is advisory — the kubelet may ignore it, as
  `pkg/deviceplugin/server.go:203` already states — and it is not implemented in this round.

### User Stories

#### Story 1

As an operator running a distributed workload over RDMA, I want to request an RDMA interface as a
Kubernetes resource, so that my container's device cgroup actually admits `open()` on the HCA instead
of the store reporting zero HCAs and quietly installing a TCP transport.

#### Story 2

As an operator whose node carries an RDMA NIC on each socket, I want the RDMA interface I am granted
to be on the same NUMA node as the accelerators I am granted, so that a collective does not cross the
SMP interconnect for every byte.

#### Story 3

As an operator on a node whose RDMA link is down, I want that interface never handed to my Pod, so
that I fail to schedule rather than failing a collective at run time.

#### Story 4

As an administrator whose nodes are not in the same state — some with SR-IOV virtual functions
configured, some without — I want not to configure the operator per node, so that one DaemonSet
serves both and each node's own state decides what it offers.

#### Story 5

As an administrator bringing a node up, I want to know whether this node's kubelet will actually
enforce the NUMA co-location, so that I learn it from one line of a preflight report rather than from
a workload that is slow for no visible reason.

#### Story 6

As the person who will write the engine-side consumption, I want a resource name whose quantity
semantics are written down per mode, so that I can set a request without guessing whether `1` is a
whole interface or a share of one.

### Core Features & Acceptance Criteria

#### F1 — the four servers, and where they hang

Four device-plugin servers are registered per node, one per allocation mode, each serving the key
[C2](#c2--the-resource-names-and-what-1-means-in-each) names. They are **not** keyed by
manufacturer: a NIC belongs to the machine, and `Devices.spec.interfaces[]` already hangs on
`DevicesSpec` for exactly that reason.

- The servers live in a package of their own beside the vendor allocators, and are started once by
  `Allocator.Start` rather than by the detected-manufacturer loop, which is keyed on a fact RDMA does
  not have. They are gated by the same `--no-shared`/`--no-sliced`/`--no-partitioned` switches the
  vendor families honour.
- They are started on the platforms the vendor allocators are started on, through the same
  `allocator_linux.go` / `allocator_other.go` split. Starting them everywhere would make the device
  manager exit on a platform that today starts no allocator at all, because a failed `net.Listen` on
  the kubelet plugin directory is a fatal error from `Start`.
- A node with no RDMA-capable interface registers the keys and advertises zero devices of them. This
  is deliberate and matches the visibility resource, which every accelerator node already advertises
  whether or not it has an SSH sidecar: a level-based `ListAndWatch` then handles a card that appears
  after a driver loads, with no second mechanism for the same fact.

Acceptance: on a node carrying at least one RDMA-capable interface, `kubectl describe node` lists the
keys with the counts [C2](#c2--the-resource-names-and-what-1-means-in-each) prescribes; on a node
carrying none, the keys are present and zero.

**The zero half needs its own pairing, and saying "the keys are present and zero" is not one.** A
node whose RDMA tree exists but could not be read produces exactly that reading: the detector will
not claim a device it could not read, so such an interface carries no `rdmaDevice`, every endpoint
fails C4's name gate, and the counts are zero — indistinguishable at this level of observation from
a node with no RDMA at all. So the criterion is **the pair**: a fixture inventory with no
RDMA-capable interface advertises zero, **and** a fixture inventory whose interface carries an
`unverified` verdict with no device name advertises zero **for a reason the case names**, while the
same inventory with the device name present advertises the full count. The discipline F4 applies to
health is the same one this row needs for existence, and an earlier revision applied it to one and
not the other.

#### F2 — the mode judgment

C1's table, implemented as a pure function over one `DeviceInterface`, with no I/O and no ledger
read.

Acceptance — **four inputs, one case each**: the three C1 distinguishes, plus one that is the only
thing standing between this judgment and a simplification all three would pass.

| input | `Exclusive`/`Shared`/`Sliced` | `Partitioned` |
|---|---|---|
| `sriov: true` with two virtual functions | advertises nothing | advertises two endpoints |
| `sriov: true` with no virtual functions | advertises the interface | advertises nothing |
| `sriov: false` | advertises the interface | advertises nothing |
| `sriov: false` with virtual functions present | advertises the interface | advertises nothing |

The judgment reads two facts, and one row guards each. **Neither row guards the other's fact** —
measured, by mutating the implementation and running the rows:

- The **middle row** catches a branch reading `SRIOV` and ignoring the count: that implementation
  sends a physical function with no virtual functions into `Partitioned`, where it has nothing to
  offer.
- The **fourth row** catches a branch reading the count and ignoring `SRIOV`. **The first three
  rows do not.** Under a `len(virtualFunctions) > 0` branch all three produce identical output,
  because none of them pairs `sriov: false` with virtual functions. Run that mutation against the
  three rows alone and nothing goes red.

The fourth input is not reachable on a real node: `VirtualFunctions` is documented as "this
physical function's virtual functions, NESTED here" (`devices.go:354-359`), so a record that is not
a physical function does not carry any. That is the reason the row is needed, not a reason to drop
it — on reachable data the two branches are indistinguishable, so reading `SRIOV` here buys no
behaviour, only the refusal to collapse a distinction the record deliberately keeps
(`devices.go:349-352`). Without this row nothing in the suite stops a later reader from deleting
that read as redundant, and the deletion would be invisible.

#### F3 — the NUMA hint

C3, implemented over the same `binding.StrRangeToList` the accelerator path uses, so the two cannot
come to disagree about what an affinity string means.

Acceptance:

- A case whose interface carries `numaAffinity: "1"` asserts the advertised device's `TopologyInfo`
  names node 1 — **read from the interface record**, so a hint hard-coded to any constant fails it.
- A case whose interface carries an empty `numaAffinity` asserts `Topology` is nil, not a
  `TopologyInfo` naming node 0.
- A case whose virtual function carries an empty affinity under a parent carrying `"0"` asserts node
  0, and a case whose virtual function carries `"1"` under the same parent asserts node 1. Both rows
  are needed: the first alone passes an implementation that always reads the parent, and the second
  alone passes one that always reads the virtual function.
- **A case with TWO interfaces on DIFFERENT NUMA nodes, asserting that each advertised device's hint
  names its own interface's node.** Every row above is satisfied by an implementation that reads the
  *wrong* interface consistently — always the first entry of `Devices.spec.interfaces[]`, say — and
  a single-interface fixture cannot tell "reads this endpoint's record" from "reads some endpoint's
  record". The parent/virtual-function pair above applies this discipline one level down; this row
  applies it between siblings, which is where a real node differs from a fixture.

#### F4 — the link gate, with its positive baseline

C4's table.

Acceptance, as one table-driven case per row:

- `failed` → the endpoint's tokens are present and `Unhealthy`.
- **`ok` → present and `Healthy`. `unverified` → present and `Healthy`.** These two rows are not
  decoration. Without them "nothing is ever advertised" passes the `failed` row, and so does "every
  endpoint is unhealthy" — the assertion the gate is worth is the *difference* between the rows, and
  a negative case alone cannot express a difference.
- An endpoint with no RDMA device name is not advertised at all, in any mode.

#### F5 — the allocation response

`Allocate` resolves each granted token back to its endpoint in `Devices.spec.interfaces[]` and
returns a response that injects:

- the endpoint's own verbs character device, resolved from its RDMA device name;
- the node-level RDMA connection-manager device, where the host has one, once per response however
  many endpoints were granted;
- `NCCL_IB_HCA`, naming the granted RDMA devices.

The verbs device is resolved at allocation time rather than recorded in `Devices`, so the answer is
always current and no API field is added for it. The resolution goes through **more than one sysfs
layout, and an unresolvable name reports which layouts were tried** — the discipline the detector's
own RDMA resolution already follows, for the same reason: a single hard-coded path that is wrong on
one distribution fails with no way to tell that from a host that has no RDMA.

The environment variable is the one narrow piece of engine-facing shape this round takes on, and it
is named as such: a container that was granted a device but cannot tell which one has been handed
half a resource. A per-engine translation beyond this belongs to the consumer work.

**The injected SET is evidenced for RoCE and for nothing else, and that limit is the contract here.**
The two entries above are what the record in hand and this repository's own RDMA reading support. No
reading available here says what a classic InfiniBand fabric additionally needs, and **the
verification fleet is structurally incapable of answering it**: the one free RDMA environment is an
Ascend pair, and the host shape the EFA issues are waiting on is EFA — both RoCE. So a container on
an IB host could be granted an endpoint, pass every unit fixture and every row of the verification
table, and still hit `EPERM` inside its own transport library on a node type nothing here ever ran
on — the silent failure this feature exists to end, one layer up. Nothing is added on a guess:
inventing a device node from an unverified claim is the error this document refuses everywhere else.
It is recorded as [Open Question 4](#4--what-an-infiniband-fabric-additionally-needs-injected) and as a
row of the verification table instead, so the gap is a known unknown rather than an assumption.

Acceptance: a case with a fixture sysfs tree asserts the response injects the verbs device belonging
to the granted endpoint and not a sibling's; a case whose name resolves under neither layout asserts
the error names both.

#### F6 — no cross-mode exclusion is introduced

Acceptance is an assertion about behaviour, not about the absence of a call. **"This function is not
called" is vacuously satisfied by an implementation that does not work at all.**

**And the obvious behavioural criterion is unbuildable here, which is why this one is written the
way it is.** There is nowhere for a fixture to record "this endpoint is held in `Shared`":
[C5](#c5--nothing-is-written-to-devices) writes no RDMA allocation record, and the two hold
registers that do exist — `Devices.status` and the in-process reservations in
`pkg/deviceplugin/controller.go` — are both keyed by `Resource{group, device}` over
`Devices.spec.groups`, which no RDMA endpoint appears in. A case phrased over a held endpoint would
degenerate to "`ListAndWatch` over an unchanged inventory returns `Healthy`", which passes against
anything.

So the criterion is phrased over the only observable a wrong implementation could read — the
allocation actually crossing the boundary:

- a case that performs an `Allocate` on the `Shared` server for one endpoint and then asserts that
  the `Sliced` server's next `ListAndWatch` still advertises that endpoint `Healthy`, and the
  mirror-image case with the two modes swapped. This needs no ledger, and it is what fails against
  the implementation the Risks section names: one that counts live allocations in process and flips
  the sibling family's tokens `Unhealthy`, halving the node's capacity with nothing to show for it.

#### F7 — `preflight` reports the TopologyManager policy

The preflight document gains a node-level section reporting what this node's kubelet is configured to
do with the hints [C3](#c3--numa-is-reported-or-nothing-is) publishes.

- It is a **top-level section of its own**, not a row inside a manufacturer's group and not a field
  on the network section. The precedent and its reason are already in the tree: the network section
  exists separately because "the per-accelerator row type requires an accelerator id and an
  allocation mode, and a network interface has neither"
  (`pkg/devicemanager/preflight/network.go:42-48`). A kubelet policy has neither either, and it is
  not a statement about a link.
- It reads the policy through the seam that already reads this node's kubelet configuration:
  `kubeletCRISources` (`pkg/devicemanager/preflight/hostexec.go:266-270`) globs
  `var/lib/kubelet/kubeadm-flags.env`, `var/lib/kubelet/config.yaml` and the distribution drop-in
  under `var/lib/rancher/*/agent/etc/kubelet.conf.d/*.conf`, and `valueAfter` reads a key out of
  them. This adds a key, not a reader.
- **Not found is `unknown`, never `none`.** The default is `none`, but the policy can also be set by
  a command-line flag in a place none of the three sources covers, and reporting the default as if it
  had been read would publish a measurement that was never taken.
- It **never affects the exit code**, for the reason `Report` already gives for the network section:
  the exit code answers whether this node can serve the allocation modes its allocators offer, and a
  permissive topology policy stops none of them.

Acceptance: a fixture host root setting `single-numa-node` in each of the three sources is reported
as such from each; a fixture setting it nowhere is reported `unknown` with a note saying so, and the
exit code is unchanged in every case.

#### F8 — the documentation says what is now true

- `docs/architecture/network-topology.md` gains the resource side of the RDMA story: the four keys,
  the mode judgment, the link gate and the NUMA hint. That page already owns the interface inventory,
  the three link states and the `rdma.*` node labels, and it has the heading room.
- `docs/accelerator-requests.md` gains the request rules: the keys, what a quantity means, the
  same-container requirement, and the TopologyManager prerequisite.
- The page-map table in the documentation skill gains the row routing "the RDMA extended resources"
  to the first of these, so the next change lands there without re-deciding.

Acceptance: `make lint docs` passes, and the two pages stay under the per-page caps.

### Verification

Everything above that a fixture can answer is a unit test and is part of this change. **No e2e case
is written here, and this section is the only record the cluster readings have.** There is no
register, tracker or matrix holding them elsewhere and nobody is carrying them: a reader who wants
to know whether a row has been answered has to ask whoever holds the hardware, and an unanswered row
looks exactly like one nobody has looked at, because that is what it is.

They are unrun because the free fleet does not cover this feature's core path: the local cluster has
consumer NVIDIA cards and no RDMA, and the one free environment with real RDMA is an Ascend pair,
which is the wrong manufacturer for this work. What the rows need is one host carrying both an RDMA
NIC and NVIDIA accelerators. Standing one up is not this change's to do.

Each row states what to read, what the value must be, and **what does not count as passing** — the
last column being the one that makes a row a criterion rather than an invitation to look around.

| # | Read | Passes when | Does NOT count as passing |
|---|---|---|---|
| R0 | `ls /sys/class/infiniband/` on the host, then `Devices.spec.interfaces[]` for the same names | every RDMA device the host lists appears as some interface's or virtual function's `rdmaDevice` | a non-empty `interfaces[]` on its own. R0 is a correspondence, and a list that names none of the host's devices is the failure it is looking for |
| R1 | `kubectl get node <n> -o jsonpath='{.status.allocatable}'` | the four keys are present; counting only endpoints whose link verdict is not `failed`, `rdma` equals the number of whole-function endpoints, `rdma.shared` and `rdma.sliced` each equal that number times `SharedResourceMaxSize`, and `rdma.partitioned` equals the virtual-function count | the keys merely being present. The counts are the contract; a key at `0` on a host R0 found devices on is a failure, not a pass. ⚠️ Counting **every** endpoint is also a failure of the reading rather than of the code: allocatable counts healthy tokens only, so a host carrying one `failed` verdict makes the unconditioned formula wrong — that host is R4's, and R1 must be read with the exclusion applied |
| R2 | in a Pod requesting `device.gpustack.ai/rdma.shared: 1` and mounting nothing by hand: `ibv_devinfo`, and `open()` on the injected `/dev/infiniband/uverbs*` | the device opens and `ibv_devinfo` reports the granted device | an `open()` that succeeds in a **privileged** Pod, or in one that also carries a hostPath mount. Both bypass the cgroup rule this row exists to prove. The paired negative is required: the same Pod **without** the resource request must still be refused with `EPERM`, exactly as recorded in `#348` |
| R3 | with the node's kubelet on `single-numa-node`, a Pod requesting **one whole accelerator** (`Exclusive`, `Shared` or `Sliced` — never a partition profile) and one RDMA endpoint in the **same container**: the granted accelerator's and endpoint's `numaAffinity` in `Devices.spec` | the two name the same NUMA node | a Pod that was admitted on a single-socket host, or on a host whose devices are all on one NUMA node anyway — there the two agree with the mechanism switched off. The row needs a host with devices on at least two NUMA nodes, and the paired reading is a request the topology cannot satisfy being **refused** with `TopologyAffinityError`. ⛔⛔ **A run using a partition profile does not count in either direction**: a partition token carries no hint, so the alignment is not being tested and the refusal can never fire — the row would pass by having nothing to disagree with. If such a run is what the hardware allows, R3 is **not answered** and must be recorded as such |
| R3b | the same Pod shape, but requesting a **partition profile** alongside the RDMA endpoint | — | nothing. This row exists to **document** the gap, not to pass: the expected observation is that the Pod is admitted while the partition and the endpoint sit on different NUMA nodes, with no error anywhere. Recording it is how the limit stops being theoretical. A run that happens to land them on the same node proves nothing and must not be written up as a pass |
| R4 | an interface whose `Devices.spec` link verdict is `failed`, against the node's allocatable count | that endpoint's tokens are advertised and unhealthy: the allocatable count excludes them | the endpoint simply being absent from `interfaces[]` — that is a detector outcome, not this gate. The row needs a `failed` verdict present in the record and the count still excluding it |
| R5 | on a host with `sriov_numvfs > 0`: the four allocatable counts | `rdma.partitioned` equals the virtual-function count, and the three whole-function keys count that physical function **zero** times | a host with no virtual functions configured. That host exercises the other branch of C1 and says nothing about this one |
| R6 | `device-manager preflight` on the node, the `topology` section | it reports the policy the node's kubelet is actually running | a report of `unknown` on a node whose policy **is** discoverable from one of the three sources. `unknown` is a pass only where the policy is set somewhere none of them reads |
| R7 | on a **classic InfiniBand** host (not RoCE, not EFA): a Pod granted one endpoint, running the transport its workload really uses, with every `open()` the library attempts traced | the transport installs and opens everything it needs from the injected set alone | ⛔ **a pass on a RoCE or EFA host**, which is every host the current fleet offers — that is the one reading this row cannot be answered by, since the whole question is what IB additionally needs. Also not a pass: `ibv_devinfo` succeeding. That exercises verbs and says nothing about the rest of the set. Until an IB host exists, R7 is **unanswered**, and [F5](#f5--the-allocation-response) says so rather than implying the set is complete |

**Which rows share one boot, for the coordinating window to merge.** The overlap was read out of the
four issues rather than assumed:

- **`#348`** — R2 **is** its reading, taken from the other side, and **neither blocks the other**.
  Read off the code rather than off the issue: `applyMemberFabric`
  (`pkg/worker/kvcache/mooncake/member_workload.go:918-988`) admits `rdma` and `efa` alike
  (`MemberProtocolIsHostFabric`, `:327-329`), and its early return for a non-`efa` protocol is
  **nested inside `if deviceResource == ""`** (`:964-967`) — so an RDMA member whose transport
  declares `deviceResourceName` renders the request today, and the mount now carries an explicit
  `HostPathDirectory` type (`:941`). What these keys add is therefore a name a cluster can **derive**
  in place of one an administrator must supply after installing somebody else's plugin; it is a
  default becoming available, not a capability appearing. The issue's body reads otherwise because
  it predates `KVCacheBackendTransport.DeviceResourceName`, which is the very candidate it lists as
  unimplemented. R2 needs no member-side change at all — it is a standalone Pod requesting the key
  — so it shares a boot with `#348` rather than queueing behind it.
- **`#285`, `#286`, `#295`** — all three want one EFA-capable cluster, and `#295`'s body states
  its own blocker is an environment rather than a fix. A host of that shape carries NVIDIA
  accelerators alongside its EFA device, and is **expected** to present that device under the RDMA
  subsystem, which is what would put it in this feature's inventory. **That expectation is unmeasured, and no reading
  in hand supports it**: the device names in `#348`'s log were taken on its RDMA branch and say
  nothing about what the EFA half presents. Establishing it is R0's whole job, which is why R0 is
  first. **If R0 holds there, one such host covers R0-R4 and R6 alongside all three issues; if
  it does not, this feature's rows need a host with a conventional RDMA NIC and the three issues
  keep their own cluster.**
- **R5 is not covered by that instance** and needs a host with SR-IOV virtual functions configured.
  An EFA guest is not expected to present them — **an external hardware claim with no reading behind
  it**, stated separately from the EFA-inventory expectation above because R0 does not cover it. It
  is the one row that justifies a second machine, and also the least urgent: C1's middle and lower
  branches are unit-tested, and R5 exercises only the upper one.
- **R7 is covered by no machine any of this names**, and that is the point of listing it. It needs a
  classic InfiniBand host; every environment in reach — the Ascend pair, the EFA instance — is RoCE.
  It shares no boot with anything and should not be merged into one, because a RoCE host reporting a
  pass against it is the one outcome that would retire the question wrongly.
- **R3b shares R3's boot** on any host whose accelerators can be put into a partitioning mode. It
  costs one extra Pod and answers a question no unit test reaches.

### Notes / Constraints / Caveats

- Go, the module's existing toolchain. No new third-party dependency.
- The generation lifecycle a device-plugin server keeps — its socket beside the kubelet's, the gRPC
  server on it, the registration and its retry — is today a set of methods on `ResourceServer`
  (`pkg/deviceplugin/server.go:1882-2175`) and is needed unchanged by the RDMA servers. It is
  extracted into a type within the same package, with `ResourceServer` keeping one-line wrappers at
  the existing signatures. Sharing it makes the two identical **by construction**; a second copy
  would be identical only by discipline, and the prior specification in this area already recorded
  that a test cannot hold two implementations together because it can only compare one against a
  copy of the other.

  **It is not a pure move, and calling it one would be wrong in a way that matters.** The logic
  being extracted derives its socket name from `s.Manufacturer` and `s.AllocationMode`
  (`pkg/deviceplugin/server.go:1932-1933`) and registers under `s.ResourceName()`, which routes
  through `GetAcceleratableResourceName` — and that helper answers for a known manufacturer, not for
  a node-level family. So the socket name and the registered resource name are **parameters** of the
  extracted type, supplied by each caller — and so is a third, which the first reading of this
  missed: the serve loop registers `ResourceServer` itself as the gRPC plugin implementation
  (`pkg/deviceplugin/server.go:1984`) and reads the registration options back off it (`:2139`).
  Both are the accelerator server's facts, not the lifecycle's. Three parameters, not two.

  **The extracted type is embedded in `ResourceServer` by value, and that shape is forced rather
  than chosen.** The suite that may not be edited selects `s.mu` and `s.server` directly
  (`pkg/deviceplugin/server_test.go:4269-4271`, `:4346-4348`, `:4370-4372`, `:4378-4380`), and
  promotion through an embedded field is the only way a field moves to another type and stays
  selectable through the outer one; a named field would not compile against a file this change may
  not touch. The embedded value must also be usable in its zero state, because those same tests
  build `&ResourceServer{…}` literals and call into the lifecycle with no `Start` first — so the
  type has no constructor. For the same reason `Logger` stays on `ResourceServer` and is passed in:
  every vendor builds its server with a composite literal, and a composite literal cannot
  initialize a promoted field.

  What is unchanged is the generation logic itself: the serve loop, the socket probe, the
  registration retry and the teardown ordering. The distinction is the whole basis of T1's
  acceptance criterion, which is that the existing lifecycle tests keep passing without being
  edited.
- Reading sysfs takes the discipline already established here: one helper, a byte cap, and a check
  that the **resolved** path is still inside the tree. sysfs is a forest of symlinks, so following
  them is required, which makes validating where you landed required too.
- The sysfs root is a parameter rather than a constant, for the reason
  `pkg/devicemanager/detector/network.go:28-40` gives: it is what lets the whole path run against a
  fixture on the platform this code is written on.
- **If an EFA device reaches this inventory, these keys and AWS's own `vpc.amazonaws.com/efa`
  describe the same hardware.** Whether it does is unmeasured and is exactly what row R0 of the
  verification table establishes; the rest of this note is what follows **if** it does, and nothing
  here depends on it being so. Nothing breaks — a device
  cgroup rule is additive, so a container granted the device by either plugin can open it, and one
  granted it by both is granted it twice — but the two counts are independent and neither knows
  about the other, so a node's advertised capacity for that hardware is doubled across the two
  names. This is recorded rather than prevented: suppressing an interface because another plugin
  might also advertise it would require knowing what is installed on the node, which this operator
  does not and should not. The consumer picks one name; which one is the consumer work's decision.

### Boundaries

- **Always:** derive the mode from the node's recorded state; report a NUMA hint or none at all;
  state the strength of any claim about topology.
- **Ask first:** any change to the four resource names or to what a quantity means — they are a
  contract a consumer outside this repository reads; any decision to start writing RDMA allocation
  state into `Devices`, since C5 is what several other decisions here lean on.
- **Never:** write to `sriov_numvfs` or create a virtual function; normalize an unknown NUMA
  affinity to node 0; introduce cross-mode exclusion between `Shared` and `Sliced`; describe the
  NUMA-level guarantee as a GPUDirect RDMA correctness guarantee.

### Risks and Mitigations

- **The NUMA hint silently stops being reported** → the feature keeps allocating and stops
  co-locating, with no failing test and no log line. Mitigated by F3 asserting the hint's
  *provenance* — that the value came from the interface record — rather than its presence.
- **The link gate is tested only negatively** → "nothing is ever advertised" passes. Mitigated by
  F4's two positive rows being acceptance criteria rather than extra cases.
- **A reader later "fixes" the shared/sliced overlap** → capacity silently halves. Mitigated by C2
  stating the overlap as intended, with the hardware reason, in the contract a reader meets first.
- **The lifecycle extraction changes registration behaviour** → every accelerator family breaks at
  once, on a node, in a way unit tests would not see. Mitigated by keeping the existing call
  signatures so the existing suite exercises the extracted code unchanged, and by confining the edit
  to parameterizing the socket name, the registered resource name and the plugin implementation,
  which is the only part that cannot be a pure move.
- **The verbs-device path is wrong on some distribution** → an allocation that grants a device the
  container cannot open, which is the exact failure this feature exists to end. Mitigated by F5
  trying more than one layout and naming what was tried.
- **A failing RDMA server takes the node's accelerator servers down with it** → `Allocator.Start`
  returns on the first error any allocator reports (`pkg/devicemanager/allocator/allocator.go:41-47`
  and `:56-58`), so an RDMA server that cannot listen ends every family on that node. Today a node
  runs only the allocators of the manufacturers it detected, so this shared fate is newly extended
  to a family that is started unconditionally. Mitigated only by the platform split F1 describes,
  which is why that split is not optional; the coupling itself is inherited from the existing loop
  and is not re-litigated here.
- **A kernel interface name is not a stable device identity** → the kubelet checkpoints the exact
  device ID strings it offered a container, and an RDMA endpoint's ID is built from
  `DeviceInterface.Name`, "the kernel interface name, and is this interface's identity"
  (`api/worker/v1alpha1/devices.go:291-292`). That name survives far less than the accelerator UUIDs
  the vendor families key on: a NIC replacement or a driver-level rename produces a different ID for
  the same hardware, and the holder's checkpointed allocation cannot be re-established. No mitigation
  is attempted — there is no more stable identity in the record, since a PCI address is absent for a
  non-PCI interface by design — so it is recorded as a known limit rather than solved.

## Design Details

### Commands

**The environment is a local macOS checkout of this worktree.** Everything below runs there and was
smoke-checked before this plan was written: `go test ./pkg/deviceplugin/...` passes in about 37
seconds.

```bash
# Per task, the narrowest command that proves it.
go test ./pkg/deviceplugin/...      # T1 T3 T4 T5 T6
go test ./pkg/nodefeature/...       # T2
go test ./pkg/devicemanager/...     # T7 T8
make lint docs < /dev/null          # T9; it needs its standard input closed

# The whole module, which is what "existing tests stay green" means. This is the target the
# repository itself uses: -race, -cover and -shuffle=on, per hack/test.sh.
make test

# Lint. It is an EDIT pass, so whether it changed anything is decided by comparing the files
# before and after with git hash-object, never by its exit code.
make lint

# Regenerate after an API type change. This change adds none, so it is expected to be a no-op;
# it is listed because a change that drifts into api/ must not skip it.
make generate
```

**No task's `Verify` line carries `go test -run`.** The spec gate reads every `go test ... -run` in
the markdown corpus and fails one whose pattern selects no existing test — and every test named in
this plan is one that does not exist yet. A task-level command is therefore a package command. A
later editor who "improves" one into a `-run` turns this document red.

⚠️ **One hole, stated rather than papered over: `_linux.go` files have no gate on this machine.**
`GOOS=linux CGO_ENABLED=0 go build ./pkg/devicemanager/allocator/...` fails inside the `binding`
tree, and a CGO cross-build has no Linux toolchain here. The one line T7 adds to
`allocator_linux.go` is therefore first compiled by the container build, not locally. T7's
acceptance says so; no command in this section covers it.

### Project Structure

```text
pkg/deviceplugin/
  serving.go              # T1  the generation lifecycle, parameterized by socket + resource name
  server.go               # T1  ResourceServer, delegating its lifecycle through one-line wrappers
  rdma_endpoint.go        # T3  C1's judgment, the endpoint/token vocabulary, C3's NUMA resolution
  rdma_devices.go         # T4  RDMA device name -> verbs character device, two sysfs layouts
  rdma_server.go          # T5  ListAndWatch, and NewRDMAServer -- the one new exported symbol
  rdma_allocate.go        # T6  Allocate and the container response
pkg/nodefeature/
  rdma.go                 # T2  the four resource names, beside the rdma.* node labels
pkg/devicemanager/allocator/
  rdma/allocator.go       # T7  the device.Allocator owning the four servers, as a vendor does
  allocator.go            # T7  starts it beside the per-manufacturer ones
  allocator_linux.go      # T7  the creator; allocator_other.go is its absent counterpart
pkg/devicemanager/preflight/
  topology.go             # T8  the TopologyManager policy section
  preflight.go            # T8  the report gains that section
docs/architecture/network-topology.md    # T9
docs/accelerator-requests.md             # T9
```

**The split between the two RDMA locations is forced, not stylistic.** The servers need
`DevicesReconciler.getDevices` and `getReconcileNotifier` (`pkg/deviceplugin/controller.go:484` and
`:451`), both unexported and neither reachable by any vendor package today — vendors receive a
`*Devices` as a parameter instead. And `pkg/deviceplugin` cannot import
`pkg/devicemanager/controllers` to fetch the reconciler itself, because that package already
imports `pkg/deviceplugin` (`pkg/devicemanager/controllers/setup.go:7`) and the edge would close a
cycle. So the servers live in `pkg/deviceplugin` and the thin `device.Allocator` that resolves the
reconciler lives beside the vendors, exactly as they do.

The alternative — exporting the two reconciler methods and putting everything under
`allocator/rdma/` — was declined on its maintenance bill: `getReconcileNotifier` hands back a
channel and a release function, so exporting it publishes a concurrency surface for one caller.
`NewRDMAServer` is one exported constructor instead.

### Code Style

A token names a locator into the inventory and nothing else, reusing the type the framework already
reserved for it — `pkg/deviceplugin/resource.go:13-31` says a `Resource` "names a Device, not an
Accelerator" and names an InfiniBand port as the case it was kept general for:

```go
// endpointOf returns the Resource naming one RDMA endpoint: its physical function, and the
// endpoint itself. The two are equal for an interface serving a whole-function mode, and differ
// for a virtual function, which is what lets one lookup reach both without a second key.
//
// It is a locator, not a record: everything a response needs -- the RDMA device name, the NUMA
// affinity, the link verdict -- is read back out of Devices.spec.interfaces[] at the time it is
// needed, so a token can never carry a stale copy of a fact the detector has since corrected.
func endpointOf(iface *workercore.DeviceInterface, endpoint string) Resource {
	return Resource{Group: iface.Name, Device: endpoint}
}
```

And one rule the allocation response has to carry in a comment, because the convenient helper beside
it is the wrong one:

```go
// Devices are injected ONE ENDPOINT AT A TIME with NewRWDevice, never with NewDevicesIn over
// /dev/infiniband. That helper injects every entry of a directory and returns nil when it cannot
// read it (pkg/deviceplugin/inject.go:60-64), so using it here would hand every container every
// HCA on the node -- and would degrade to handing it none, silently -- which is the opposite of
// the per-endpoint grant this resource exists to make.
```

Conventions that apply throughout: comments state the rule and the reason beside it, and never carry
a task identifier; an unreadable value degrades to "unknown" and never to a value that asserts
something; exported behaviour documents its constraints, not its implementation.

### Implementation Plan

T1 is a prefactor: it makes the feature tasks possible rather than merely tidier, so it comes first
and blocks what needs it. T2, T3, T4 and T8 depend on nothing and on each other's paths not at all,
so five tasks are unblocked at the start.

**No proof-of-concept task is ordered.** The one item of uncertain feasibility is the sysfs layout
T4 reads, and it cannot be settled without hardware this plan does not have. It is mitigated in
T4's own acceptance (two layouts, and an error that names both), and it is settled on a real host
by **R2**, not by R0: R0 is a correspondence between the device *names* a host lists and the names
the record carries, and says nothing about where a verbs node lives. R2 opens the injected node and
requires `ibv_devinfo` to report **the granted device**, which is what a wrong resolution fails.
⚠️ Even R2 leaves a residue: it proves the resolution answered on the host it ran on, through
whichever of the two layouts that host presents, and **cannot say which one answered** — so a pass
does not retire the other layout, and neither layout is retired by any reading this plan can take.
A spike that cannot reach the uncertainty would be a task that is green by construction.

- [x] **T1 · Extract the serving lifecycle**
      Blocked by: None
      Owns: `pkg/deviceplugin/serving.go`, `pkg/deviceplugin/server.go`
      Gate: review
      Acceptance: the socket/gRPC/registration generation lives in a type of its own, taking the
      socket name, the registered resource name and the plugin implementation as parameters, and
      embedded in `ResourceServer` by value so the unedited suite still selects `s.mu` and
      `s.server` through it; `ResourceServer` keeps `Start`,
      `Stop`, `isRegistered`, `publish`, `retire` and `register` at their present signatures as
      one-line delegations. **`pkg/deviceplugin/server_test.go` is not edited** — `git diff` against
      the merge base reports no change to that file — which is what makes the existing suite a test
      of the extracted code rather than of a copy.
      Verify: `go test ./pkg/deviceplugin/...`

- [x] **T2 · The four resource names**
      Blocked by: None
      Owns: `pkg/nodefeature/rdma.go`, `pkg/nodefeature/rdma_test.go`
      Gate: review
      Acceptance: a helper returns the four keys of [C2](#c2--the-resource-names-and-what-1-means-in-each)
      per mode, and empty for the modes that have none. Each key is asserted **as a literal string**,
      never recomposed from the constants the implementation uses — a test that rebuilds the name
      from the same pieces cannot catch a wrong piece. A second case asserts `ResourceFamilyOf`
      returns `ResourceFamilyNone` for all four.
      Verify: `go test ./pkg/nodefeature/...`

- [x] **T3 · The endpoint vocabulary, the mode judgment and the NUMA resolution**
      Blocked by: None
      Owns: `pkg/deviceplugin/rdma_endpoint.go`, `pkg/deviceplugin/rdma_endpoint_test.go`
      Acceptance: [F2](#f2--the-mode-judgment)'s three rows; the advertisement predicate (an
      endpoint with no RDMA device name is not one); and [F3](#f3--the-numa-hint)'s cases including
      the two-interface sibling row. Pure functions over one `DeviceInterface`: no I/O, no ledger
      read.
      Verify: `go test ./pkg/deviceplugin/...`

- [x] **T4 · Resolve an RDMA device name to its character device**
      Blocked by: None
      Owns: `pkg/deviceplugin/rdma_devices.go`, `pkg/deviceplugin/rdma_devices_test.go`
      Acceptance: resolves a device name to its verbs node under a fixture root through **two**
      sysfs layouts; a name resolvable under neither returns an error **naming both layouts tried**;
      a fixture whose symlink escapes the root is refused. The root is a parameter, which is what
      lets all of this run on the platform this is written on.
      Verify: `go test ./pkg/deviceplugin/...`

- [ ] **T5 · The RDMA server: ListAndWatch**
      Blocked by: T1, T2, T3
      Owns: `pkg/deviceplugin/rdma_server.go`, `pkg/deviceplugin/rdma_server_test.go`
      Gate: review
      Acceptance: [F1](#f1--the-four-servers-and-where-they-hang)'s counts **and its paired zero
      rows**; [F4](#f4--the-link-gate-with-its-positive-baseline)'s four health rows including both
      positive baselines and the nil-link row; [F6](#f6--no-cross-mode-exclusion-is-introduced)'s
      allocate-then-advertise pair; and two consecutive calls over an unchanged inventory returning
      byte-identical responses. `NewRDMAServer` is the only new exported symbol.
      Verify: `go test ./pkg/deviceplugin/...`

- [ ] **T6 · The RDMA server: Allocate and the container response**
      Blocked by: T4, T5
      Owns: `pkg/deviceplugin/rdma_allocate.go`, `pkg/deviceplugin/rdma_allocate_test.go`
      Acceptance: [F5](#f5--the-allocation-response) — the response injects the granted endpoint's
      verbs device **and not a sibling's**; the connection-manager device appears once per response
      however many endpoints were granted; the environment variable names the granted devices; an
      unknown or unparseable token is refused rather than silently dropped. Devices are injected one
      at a time, never by directory.
      Verify: `go test ./pkg/deviceplugin/...`

- [ ] **T7 · Wire the allocator**
      Blocked by: T5
      Owns: `pkg/devicemanager/allocator/rdma/**`, `pkg/devicemanager/allocator/allocator.go`,
      `pkg/devicemanager/allocator/allocator_linux.go`,
      `pkg/devicemanager/allocator/allocator_other.go`
      Acceptance: the four servers are constructed per the `--no-shared`/`--no-sliced`/
      `--no-partitioned` switches and started once by `Allocator.Start`, following the existing
      per-vendor server-set test. ⚠️ The one line in `allocator_linux.go` has **no gate on the
      development machine** and is first compiled by the container build; that is a stated
      limitation of this task, not something its Verify covers.
      Verify: `go test ./pkg/devicemanager/...`

- [x] **T8 · `preflight` reports the TopologyManager policy**
      Blocked by: None
      Owns: `pkg/devicemanager/preflight/topology.go`,
      `pkg/devicemanager/preflight/topology_test.go`,
      `pkg/devicemanager/preflight/preflight.go`, `pkg/devicemanager/preflight/preflight_test.go`,
      `pkg/devicemanager/cmd.go`, `pkg/devicemanager/preflight/network_test.go`
      — the last because a top-level section changes `Report`'s signature, and that file holds one
      of its three call sites
      Acceptance: [F7](#f7--preflight-reports-the-topologymanager-policy) — each of the three
      kubelet-configuration sources answers when it carries the key; none answering yields `unknown`
      with a note saying why, never the default reported as if read; and a case with a failing
      topology row asserts the report still returns success, so the exit code is unmoved.
      Verify: `go test ./pkg/devicemanager/...`

- [ ] **T9 · Documentation**
      Blocked by: T2, T6, T8
      Owns: `docs/architecture/network-topology.md`, `docs/accelerator-requests.md`,
      `.claude/skills/gpustack-operator-docs/references/page-map.md`
      Acceptance: [F8](#f8--the-documentation-says-what-is-now-true), and both pages stay under the
      per-page caps — the first has four `##` slots and about seven hundred lines of room, the
      second three slots and about six hundred.
      Verify: `make lint docs < /dev/null`

**Checkpoints.** After T1, the tree is green and no feature exists yet — the prefactor is separately
revertible. After T7, the node advertises the keys and an allocation works end to end. After T9, the
change is describable to someone who did not write it.

`Status: Shipped` is set as part of shipping, per the brief that commissioned this work; it is not a
task here because it is one word and belongs with the pull request rather than with the code.

### Test Plan

[ ] I/we understand the owners of the involved components may require updates to existing tests to
make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

**None.** The existing `pkg/deviceplugin` suite must keep passing **unedited** through T1, and that
is T1's own acceptance criterion rather than a preparatory task: a test changed to accommodate the
extraction would stop being evidence that the extraction preserved behaviour.

#### Unit tests

Baselines measured on 2026-09-20, on the environment [Commands](#commands) pins. They are a floor
against a whole new file going untested, **not** the criterion — each task's own acceptance is that.

- `pkg/deviceplugin`: 2026-09-20 - 85.0% — must not fall
- `pkg/nodefeature`: 2026-09-20 - 92.3% — must not fall
- `pkg/devicemanager/preflight`: 2026-09-20 - 93.1% — must not fall
- `pkg/devicemanager/allocator/rdma`: new package, no baseline — target the existing vendor
  packages' median, about 85% (they span 54.2% to 90.1%)
- `pkg/devicemanager/allocator`: no test files today, and this change adds none to the root package;
  its behaviour is covered through the per-family package above

The cases themselves are enumerated per task in [Implementation Plan](#implementation-plan) and per
feature in [Core Features & Acceptance Criteria](#core-features--acceptance-criteria); they are not
repeated here.

#### Integration tests

One, and it earns its place: a case driving the RDMA server's `Start` against the fake kubelet the
existing suite already stands up, asserting that **all four resource names register** and that each
lands on its own socket. This is the only evidence that the lifecycle T1 extracts is genuinely
shared — with `ResourceServer` as its sole caller, "shared by construction" is a claim nothing
exercises.

#### e2e tests

**None, deliberately.** An e2e case here would need a node with both an RDMA NIC and an accelerator;
the free fleet has neither together. Writing one against a node without RDMA produces a case that
passes by finding nothing — the permanently-green shape this repository's own gates exist to catch.
The cluster readings live in [Verification](#verification) as rows R0 through R7, each with what to
read, what passes, and what does not count as passing. That section is their only record; none of
them has been taken, and nothing outside this document is tracking them.

## Alternatives

**Have the administrator install a third-party RDMA device plugin.** Rejected on the second finding
in the Motivation: the widely deployed shared plugin reports no `TopologyInfo`, so joint allocation
is not merely unimplemented there but structurally unreachable. It would also leave
`DeviceResourceName` permanently declared rather than derivable.

**Give RDMA its own allocation-mode enum.** Rejected by ruling, and it is the right ruling: a second
enum means every consumer that reasons about modes has two vocabularies to map between, for a domain
where four of the five values mean the same thing. The one value that does not map cleanly —
`Visibility` — is handled by leaving it unimplemented and saying so, which costs one open question
instead of a parallel type.

**Model `Sliced` as something other than `Shared`.** There is nothing for it to be. Slicing on the
accelerator side is a quota enforced by an interception library; an HCA has no such quota to enforce,
so a distinct `Sliced` implementation would be `Shared` with extra machinery that does nothing.

**Advertise one resource per HCA**, e.g. `device.gpustack.ai/rdma.mlx5_0`. Rejected: the key set
would then differ per node, which a `ResourceFlavor` selector and a Kueue quota cannot express, and
the per-HCA choice is exactly what the token layer plus the NUMA hint already make.

**Carry the verbs character-device path in `Devices.spec.interfaces[]`.** Rejected: it is an API
field for a value that is cheap to read when it is needed and stale whenever it is not, and it would
make a detector pass a prerequisite for an allocation that has everything else it needs.

**Drop a `failed` endpoint's tokens instead of reporting them unhealthy.** Rejected for the reason
the accelerator side already records: removing an advertised ID strands the kubelet's checkpointed
allocation for whatever container holds it.

## Open Questions

### 1 — `Visibility` is not implemented, and C5 is why

The accelerator side has a `Visibility` mode that co-allocates a second container in the same Pod to
the very device its sibling holds. The RDMA equivalent would be useful — a sidecar that needs to see
the same HCA — and it is not implemented here.

The obstacle is precise rather than general: a visibility allocation has to know **which** endpoint
the owner container was granted, and the only durable records of that are the ledger and the Pod
annotation that [C5](#c5--nothing-is-written-to-devices) decides not to write. So implementing it
means reopening C5, not adding a fifth server. What it would cost is therefore known: a per-endpoint
allocation record on `Devices.status` or on the Pod, rebuilt the way the accelerator ledger is, plus
the reservation path that keeps a just-granted endpoint visible to a sibling Allocate in the same
cycle.

This is recorded rather than passed over, per the ruling that it be left open if it could not be
implemented.

### 2 — the switch-level preference is not implemented

`GetPreferredAllocation` is declared available by the accelerator servers and could rank RDMA
endpoints by `device.BusDistance` to the accelerators a Pod already holds, preferring an endpoint
behind the same PCIe switch. It is not done here, and the RDMA servers declare it unavailable.

Two reasons, both of which would have to be written down anyway if it were implemented. It is
**advisory** — `pkg/deviceplugin/server.go:203` states the kubelet is free to ignore the answer — so
it can express a preference and never a guarantee. And the distance it would rank by cannot reach the
level that matters most: `PciRootID` is the outermost bridge rather than a root complex, so
`BusDistance` never answers at host-bridge level, and the "same root complex" condition the vendor
names for GPUDirect RDMA is not measurable from what this repository records today.

Implementing it is additive and blocks nothing; what it needs first is a decision about whether a
preference whose strength cannot be stated is worth publishing.

### 3 — deriving `DeviceResourceName` from these keys

`KVCacheBackendTransport.DeviceResourceName` exists because no name could be hard-coded. These keys
are the first names this operator can derive, so an RDMA-protocol member could default to one instead
of requiring the administrator to declare it. That is consumer work with its own admission questions
— chiefly what a member should do on a node that advertises the key with a count of zero — and it is
deliberately not decided here.

Two facts a reader of that work should have from here. It is **not blocked by this change**: an RDMA
member whose transport already declares `deviceResourceName` renders the request today, because the
early return for a non-`efa` protocol in `applyMemberFabric` is nested inside `if deviceResource ==
""` (`pkg/worker/kvcache/mooncake/member_workload.go:964-967`, with both protocols admitted at
`:327-329`). What these keys add is a derivable default, not a new capability. And the open issue
describing that function reads otherwise because it predates the field.

### 4 — what an InfiniBand fabric additionally needs injected

[F5](#f5--the-allocation-response) injects the endpoint's verbs character device and the node-level
connection-manager device. That set is evidenced for RoCE. **Whether a classic InfiniBand fabric
needs more is unknown here**, and it cannot be settled from this repository: no reading in the tree
describes an IB transport's device usage, and no environment in reach is IB.

What makes this a question rather than a note is the asymmetry of the failure. If the set is
complete, nothing happens. If it is short, an IB host grants an endpoint that the workload's own
transport library then fails to open — the same silent fallback this feature exists to end,
relocated from the device cgroup to one layer above it, and invisible to every test and every
verification row the current fleet can run. Row R7 is what would answer it.

Deliberately **not** resolved by adding candidate device nodes speculatively. A node injected on a
guess is a permission granted on a guess, and the rest of this document refuses exactly that.
