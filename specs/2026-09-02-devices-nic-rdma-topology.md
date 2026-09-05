# Spec: `Devices` NIC/RDMA topology — the relation NFD cannot express

Status: Shipped
Type: Feature

## Summary

The `Devices` ledger already records, per accelerator, where it sits on the machine: its PCI bus id,
the outermost PCI bridge above it (`pciRootId`, which despite its name is **not** a root complex —
see [What is already there, measured](#what-is-already-there-measured)), its NUMA node and the CPUs
close to it. What it does not record is the *other*
side of a collective transport — the network interfaces — and therefore it cannot record the one fact
a distributed workload is actually placed on: **which accelerator and which NIC are near each other,
and how near**.

That fact cannot come from Node Feature Discovery. NFD publishes *what this machine has*; affinity is
*which of the things it has are close to each other* — a relation between two inventories, not a
property of either. And it cannot stay inside `Devices` alone either: Kueue's Topology Aware
Scheduling reads node **labels** and nothing else, so a topology fact that is never compressed into a
label never reaches a scheduling decision.

This spec makes the Device Manager, which is already walking the PCI bus per accelerator, **record the
NIC side of that same walk** into `Devices.spec`, express accelerator↔NIC proximity as the *tightest
scope the two share* rather than as a boolean, compress that into node labels, and **withhold the RDMA
label from a node whose RDMA link does not verify** — so an unusable node is not selected rather than
selected and then failed.

It adds no CRD. `Devices` is an existing type and this is a field addition to it.

## Motivation

### Dependencies

This section exists because nothing else in this repository does the job it does. **No spec under
`specs/` carries one** — verified by `grep -c '^### Dependencies' specs/*.md` across all 33 files: zero
hits. So this is a section being *introduced* here, not a repository convention being followed, and
the reason to introduce it is that its absence has already cost something: a sibling spec was drafted
against a prerequisite CRD that existed only on an unmerged branch, and because the spec said nothing
about it, nothing stopped the work from being started from `main`, where it does not compile.

A spec that does not declare its prerequisites has moved "can this be built today?" from the document
into the reader's memory.

**This spec's prerequisites, each with the ref that carries it:**

| # | Prerequisite | Where it lives | Verified |
|---|---|---|---|
| D1 | `Devices` type with `DevicesSpec.Groups` and `DeviceTopology` | `api/worker/v1alpha1/devices.go:25,86` | read |
| D2 | `device.ConstructTopology`, called by all nine detectors | `pkg/device/helper.go:150`; nine sites under `pkg/devicemanager/detector/*/device.go` | grep, 9/9 |
| D3 | `binding.GetPCIDevices(vendors, classPrefixes)` — sysfs PCI enumeration with a non-linux stub | `binding/helper.go:321`, `binding/helper_linux.go:144`, `binding/helper_other.go:34` | read |
| D4 | **`binding.PCIDevice.Root` + `.Switches` and `binding.ComparePCIDevices`** — the three-level proximity predicate, already implemented | `binding/helper.go:298,310,326`; `binding/helper_linux.go:240-263` | read |
| D5 | `binding.GetNumaNodeByBDF` / `binding.MapNumaNodeStrToCPUAffinity` | `binding/helper.go:84` | grep |
| D6 | The detector's `NodeFeature` + `Devices` sync pass | `pkg/devicemanager/detector/detector.go:422,477,490` | read |
| D7 | Ascend RoCE read via DCMI (`GetIp(ROCE_PORT, 0)`, `GetGateway`) | `pkg/devicemanager/detector/ascend/device.go:213-221`; `binding/dcmi/library_device.go:557,567` | read |
| D8 | The `device-manager preflight` subcommand and its host-exec seam | `pkg/devicemanager/preflight/hostexec.go`, `.../hostexec_crosscheck.go`; specified by `specs/2026-08-28-device-manager-preflight.md` (Shipped) | read |

**Every one of D1–D8 is present on `main` at `4d88673b`, which is this branch's base. There is no
unmerged prerequisite, and that is the reason this work was selectable at all.**

**One prerequisite is deliberately *not* claimed:** Kueue's Topology Aware Scheduling. The vendored
Kueue chart ships the `Topologies` CRD, but **this repository's Go code references it nowhere** —
verified by grep for `TopologyAware`, `topologyName` and `kueue.x-k8s.io/topology` across `pkg/` and
`api/`: zero hits outside the vendored chart. So the labels this spec produces have **no consumer
in-tree on the day they land**. That is stated rather than hidden; see
[F7a](#f7a--the-labels-have-no-in-tree-consumer-yet) and [Open Questions](#open-questions).

### Goals

- **Record the NIC side of the machine in `Devices`**, at the same coordinates the accelerator side
  already uses, so the two can be compared without a translation layer.

- **Express proximity as the tightest shared scope, not as a boolean**, and derive it from the
  coordinates both sides already carry rather than storing a cross-reference.

- **Compress that scope into node labels**, because that is the only surface Kueue's Topology Aware
  Scheduling reads. The compression rule, and what it loses, are documented where they are read.

- **Verify the RDMA link before advertising it, and withhold the label when it does not verify.** A
  node whose RDMA interface is present but whose link is down is worse than a node with no RDMA at all:
  the first is selected and then fails a collective, the second is never selected.

- **Answer it in `preflight` too**, for the bring-up case where there is no cluster and no `Devices`
  object yet.

### Non-Goals

- **No `topologyAffinity: Required` semantics.** A downstream spec will want strong affinity — an
  accelerator that *must* be co-located with the NIC it transports over. Enforcing that needs an
  allocator that can allocate two device classes together and hand a container the specific interface
  it was granted. **This spec produces the facts and the labels such a mechanism would read, and
  implements none of its semantics.**

  Stated here rather than only in the implementation, because a boundary is only useful where it is
  read: the consumer of these labels is a future spec, and the person writing it will read this
  section and not our detector code.

- **No allocation-time decision.** Choosing *which* accelerators to hand a container — preferring a
  set that are `LINK` peers when a workload asks for several — is not done here. F7 establishes that
  this is **not a preference but a consequence**: an equality-matched, eight-element label selector
  cannot express "four accelerators in one group", so the facts this spec publishes can only be acted
  on by the device plugin's preferred-allocation path (node-local) or by a scheduler (cross-node).
  Both are separate work with their own semantics to argue — notably that
  `GetPreferredAllocation` is **advisory**: the kubelet may ignore the hint, as this repository's own
  code comments state (`pkg/deviceplugin/server.go:203,430`). **This spec is that work's
  prerequisite, and that work would have been the only viable consumer of an interconnect group** —
  a two-way dependency, stated so neither is planned as if it stood alone. That the consumer does not
  exist yet is why the group axis is not in this round at all (F1).

- **No VF creation, and no SR-IOV configuration.** SR-IOV state is *read*. Creating a VF is a host
  write with no reliable teardown.

- **No device plugin for NICs, and no new extended resource.** Nothing here is allocatable. NICs are
  recorded and labelled; they are not handed to a container by us. `pkg/worker/kvcache` already mounts
  `/dev/infiniband` wholesale for an RDMA-transport member (`member_workload.go:39-40`), and this spec
  does not change that.

- **No `Devices.status` change.** Status is the allocation ledger and is rebuilt wholesale each
  reconcile from Spec; see [F4](#f4--the-sync-path-the-comparison-that-decides-it-and-the-ordering).

- **Not a continuous NIC health monitor.** The link is verified when the node is profiled and when
  preflight runs. A link that drops between two passes is caught by the next one, on the same cadence
  every other `Devices` fact already has. Continuous monitoring has a different failure mode
  (flapping) and would need a different write path.

- **No host tooling is shipped or vendored.** Where a host binary is the only source of an answer, it
  is reached the way `preflight` already reaches host binaries.

## Proposal

### What is already there, measured

This spec's shape follows from what the ledger already records, so that is stated first rather than
assumed. Read off `main` at `4d88673b`:

`Devices.spec.groups[].accelerators[].topology` is a `DeviceTopology`
(`api/worker/v1alpha1/devices.go:86-104`) carrying:

| field | filled by | for which manufacturer |
|---|---|---|
| `pciBusId`, `pciRootId`, `pciClass` | `device.ConstructTopology` | all nine |
| `numaAffinity`, `cpuAffinity` | `binding.GetNumaNodeByBDF` and its CPU mapping | all nine |
| `roce` (`ip`/`subnetMask`/`gateway`) | Ascend's DCMI RoCE port read | **Ascend only** |

And one level below the API, `binding.PCIDevice` already carries **more** than the ledger keeps:

```go
Root     string    // commented as "the root complex ID of the PCI device"
Switches []string  // every upstream bridge/switch on the path, innermost first
```

with the predicate already written (`binding/helper.go:326`):

```go
ComparePCIDevices(a, b) →  1  // same switch chain
                        →  0  // same Root, different switch chain
                        → -1  // unrelated
```

**So the three-level proximity predicate exists in this repository today, and the API layer throws
two thirds of it away.** `DeviceTopology` keeps `pciRootId` and drops `Switches`, which is the
*tighter* of the two levels. Nothing can currently ask "are this accelerator and this NIC behind the
same switch?" even though the code that answers it is already compiled in.

**And `Root` does not hold what its comment says.** The loop that fills it
(`binding/helper_linux.go:236-251`) walks up from the device's resolved sysfs path, appends each
parent whose name contains two colons, and **reassigns its cursor to that parent every time**. It
then reports the cursor:

| the device sits… | `Root` is |
|---|---|
| behind one or more bridges | **the outermost bridge's address**, e.g. `0000:00:01.0` |
| directly on the root complex | **the device's own address** |

Never the root complex's name (`pci0000:00`) — which sysfs does carry, and which neither side
records. *Boundary on this claim:* it is read off that loop and confirmed against an
identically-structured reimplementation under fixtures covering both rows. It was **not** measured
by running `binding` itself on a Linux host, and the comment on the field is not evidence either
way, since it is the thing being contradicted.

**The walk is shared, not copied.** `binding.ResolvePCITopology` is exported and called by both
inventories. A reimplementation was the obvious alternative and is worse than it looks: no test can
hold two implementations together, because a test can only compare one against a copy of the other
and the copy drifts with it. Sharing makes them identical *by construction* rather than by
discipline — verified by mutating the shared function and observing **both** test suites go red.
Its behaviour is unchanged from the original loop; a dead `parent == sysfsPCIPath` branch that
could never fire was dropped, and the walk's termination is now stated as an invariant (any
absolute path walked upwards reaches `/`, whose base name has no colon).

Two consequences the rest of this spec rests on:

- **The NIC side must derive this identically rather than correctly.** A better-founded derivation
  would compare equal to nothing, and every proximity answer would come out "unrelated" — on every
  node, with no wrong data anywhere to notice it by.
- **For a device on the root complex, `pciRootId` equality is an identity check**, not a
  same-root-complex claim. So the field cannot carry even the weaker level, which is a second and
  independent reason `pciSwitches` is added rather than `pciRootId` reused.

That is the second thing this spec fixes, and it costs one field.

Two facts about the OBJECT rather than the fields, measured on a running cluster because neither is
visible in the types and both change how this feature is verified:

- **`worker.gpustack.ai` is served twice, and only one of the two accepts writes.** An aggregated
  APIService answers at `v1` (backed by the worker) while the CRD serves `v1alpha1`, which is the
  stored version. An unversioned `kubectl get devices.worker.gpustack.ai` therefore reads the
  AGGREGATED view — so a field added here has to be visible through that view before any check
  reading it can see anything, and a check that reads it too early reports a clean run about
  nothing. Writes to the aggregated version fail with `ServiceUnavailable`, reproducibly, while
  reads succeed — which reads like a broken cluster rather than like the wrong endpoint. An
  out-of-band write must name `devices.v1alpha1.worker.gpustack.ai`.
- **A running device manager never re-aligns.** `apply()` compares the stored groups against a fresh
  detect and writes back on any difference, but it runs only inside `reportDevices`, which needs a
  detect trigger. Measured: a capability field edited out of band to a value detection cannot
  produce stayed wrong for 182 seconds across twenty reads with the device manager healthy, and was
  corrected 12 seconds after its pod was deleted. So an out-of-band edit to `Devices.spec` is
  silently divergent until the next trigger or a restart — which is why the operation docs tell an
  administrator to restart the DaemonSet after a mode toggle rather than to edit the object.

  That second fact is also a usable instrument, and the negative half is what makes it one: because
  a running manager does not repair the edit, a repair that DOES appear can only have come from a
  process that ran its report path. Corrupt, restart, observe the repair — that is a signature of a
  fresh process re-deriving the record, which nothing else in this object provides.

### Principles taken from prior art

Two upstream implementations were read while writing this spec: a network device driver built for the
dynamic-resource-allocation model, and a device-scheduling system that publishes a per-device topology
block and jointly allocates accelerators with NICs. A third was read for the hardware this repository
most needs it for: a shared-RDMA device plugin for an NPU platform, which reports RDMA faults and
maps them back to the accelerators they affect. What follows is what they establish, as principles —
each one either adopted below or explicitly declined.

#### P1 — on some platforms the pairing is declared as a product fact, not discovered

On one class of production hardware the accelerator↔NIC relation ships as a **declared table** in a
config file, chosen at load time by the machine's own **product form** — and the form is read from a
vendor-private sysfs attribute on the network interface, not from anything generic. Two forms appear
in the shipped table for an eight-accelerator machine, and their arity differs: one lists exactly one
interface per accelerator, the other lists three.

**What the table's shape actually establishes, stated at the precision the code supports:**

- **The pairing is authored, not measured.** It arrives as configuration keyed by a chassis model.
  A discoverer that only derives is therefore answering a different question than the one this
  platform's operators consider settled.
- **The form identifier is vendor-private.** Reading it means reading a manufacturer-specific sysfs
  attribute and mapping its values — so even *knowing which table applies* is not a generic read.
- **The multi-interface form is still one-to-one at the level the code uses.** Only the **first**
  interface of each list is consumed: the loader builds a reverse index from first-interface →
  accelerator ids, and those first interfaces are **distinct across all eight** accelerators. The
  remaining entries per accelerator are present in the data and have **no consumer anywhere in that
  repository** — verified by grepping every use of the field and of the index it feeds. So the
  ordering is capacity the format reserves, **not a mechanism that upstream operates**.
- **A missing or unreadable table degrades attribution to empty, not to an error** — its own log
  message says the affected-accelerator list will be empty. An absent declaration therefore reads as
  "this NIC affects no accelerator", which is the failure mode any override we add would inherit.

**What it does *not* establish, and this spec must not claim it does:** that bus coordinates
*cannot* reproduce the pairing. The first-interface mapping is one-to-one, and whether our
switch/root/NUMA comparison would produce the same grouping is unknown — it needs that chassis's PCIe
tree, which is not in hand. The honest claim is about **provenance** (authored vs derived), not about
derivability.

*Adopted:* derivation reports **the scope it established**, and `NODE` is a real answer meaning
"these share nothing tighter than the machine" — never a default a reader could mistake for measured
alignment. *Declined this round:* the declared-table override itself — see
[Open Question 4](#4--what-shape-would-the-declared-acceleratornic-override-take) for what stays
open and [Decisions Taken](#decisions-taken) for why an intervening revision that put it in scope was
reversed.

#### P2 — the tightest shared scope is an ordered enum, not a boolean

The scheduling system models proximity as a scope with a **numeric level** — device > PCIe > NUMA node
> node — so a request can demand "at least NUMA-level" and a publisher can report the tightest scope
it found. A boolean can only answer one fixed question, and it answers it for the whole node.

*Adopted, and it is the biggest change to this spec's label design.* Our own `ComparePCIDevices`
already returns three levels; publishing a boolean would discard what it computed.

**The ordered-enum *shape* is what this principle contributes; the *levels* come from
[P15](#p15--bus-distance-and-interconnect-proximity-are-different-axes-and-on-accelerators-with-a-high-speed-link-the-bus-answer-is-not-merely-coarse-it-is-inverted).**
This prior art's scale bottoms out at "same PCIe", which cannot express a high-speed accelerator
interconnect at all.

#### P3 — the publisher publishes coordinates; the consumer chooses the tightness

In that system the required scope is named **by the requesting workload**, not fixed by whatever
discovers the hardware. The discovery side's job is to publish coordinates precise enough that a
consumer can apply its own predicate.

*Adopted.* It is also why P2 matters: pre-compressing to one boolean takes the choice away, and the
richer record has to stay in `Devices` even after the labels are stamped.

#### P4 — a node-level "well planned" assertion is only valid for exclusive requests

That system carries a node label asserting the secondary devices are well planned, and its effect is
to let the scheduler **skip solving for the secondary device separately**. Reading the consumer side:
the shortcut is applied only when the request holds its primary device **exclusively** — a shared or
sliced accelerator does not qualify, because for a fraction of a card the node-level assertion does
not imply that the fraction you get will be the aligned one.

*Adopted as a stated limit.* Our node label has exactly the same reach, and this spec says so rather
than letting a future consumer discover it. This is the precise reason a node label cannot replace
the per-device record.

#### P5 — not every NIC is a PCI device

The NPU-platform plugin resolves an RDMA device to its Ethernet interface through **two** sysfs
layouts, and its own device selector treats the **bus type** as a selection dimension alongside vendor
and device id — with a non-PCI interconnect as a first-class value. Its shipped configuration selects
on that non-PCI bus.

*Adopted as a correctness constraint on enumeration.* An enumeration rooted in the PCI bus does not
merely mis-sort such an interface; it never sees it. This spec's pass therefore enumerates network
interfaces first and treats the PCI device as an attribute that may be absent.

#### P6 — a fault has levels, and "detected but do not act" is one of them

That plugin's fault records carry a level, and two of its levels are explicitly excluded from the
callback that removes a device from the allocatable pool: a fault can be **detected, reported, and
deliberately not acted on**. The reporting surface and the enforcement surface are separated by an
explicit allow-list, not by the presence of a fault.

*Adopted.* Our three link states are the same separation: everything is reported, and only one state
withholds the label.

#### P7 — a fault's first-seen time must be stable

Its detector caches the timestamp of each (device, fault-code) pair, reuses it while the fault
persists, and drops the entry when the fault clears. A timestamp refreshed on every pass would make
"how long has this been broken?" unanswerable — the question an operator actually has.

*Adopted for the failing state.* It also has a second consequence this spec cares about more than the
original did: see P8.

#### P8 — the published record must be ordered, or it is rewritten forever

Both implementations sort before publishing — devices by index, virtual functions by bus id. In our
architecture this is not cosmetic. `devsAlginFn` decides whether to write by `DeepEqual` against the
live object, and sysfs directory order is not guaranteed stable across reads, so an unsorted list
would compare unequal on passes where nothing changed and issue an API write **every detector pass,
forever**.

*Adopted as an acceptance criterion*, not a note. This is the defect most likely to reach production
unnoticed, because its symptom is write volume rather than wrong data.

#### P9 — virtual functions nest under their physical function

That system records VFs as a **group hanging off the PF**, and its enumeration explicitly skips VFs,
reaching them only through the PF's own VF list. Flat sibling entries would make one card appear many
times and leave every consumer to re-group them; the group also carries labels, which is what lets a
request select a VF by property.

*Adopted.*

#### P10 — "unknown" must not collide with a valid value, and normalising it is a bug

One implementation encodes an unreadable socket or NUMA id as a sentinel. But its NUMA read
**normalises the kernel's "no NUMA affinity" to node 0**, which on a multi-socket machine asserts an
affinity the kernel explicitly denied.

*Adopted as a rule, and its mistake declined.* Our `numaAffinity` is a string where empty already
means unknown. Every numeric field this spec adds must distinguish absent from zero — the same
discipline `remaining`'s `omitempty` already demands here.

#### P11 — a sysfs read is a security boundary

That plugin reads every sysfs file through a helper that caps the byte count and validates that the
**resolved** path is still under `/sys`, and applies a size cap plus a real-path check to its config
file. sysfs is a forest of symlinks, so following them is required — which makes validating where you
landed required too.

*Adopted.*

#### P12 — the check registry is data, and checks must not overlap in what they attribute

Its fault checks are a configured list of records, each naming a check method that resolves to a
function, with a dependency field so a check can be skipped when its precondition failed. And the
checks are written not to double-attribute: for a bonded interface, *one* member down is that check's
fault, while *all* members down deliberately reports false there because a different fault code owns
it.

*The non-overlap rule is adopted; the registry is declined, and the reason is a measurement of this
spec's own scale.* The evidence cited for the link check in F5 is a pair of reads of the RDMA
subsystem's own port attributes — not any vendor's interface — so a table keyed by manufacturer
resolves to one entry for every manufacturer that has an RDMA device, and to nothing at all for the
rest. Building it would create a state that does not otherwise exist ("no check is implemented for
this manufacturer") and then require F6 to explain it. One check, and the state is gone rather than
better described.

The non-overlap half survives and has real content here, on a different pair: an interface's
`up` comes from `operstate`/`carrier` and its link state comes from the RDMA port attributes. The
first is descriptive and gates nothing; only the second can withhold a label. A single "the network
is broken" verdict fed by both would let an unplugged management NIC withhold `rdma.capable`.

#### P13 — the direction of the affinity query is NIC → accelerator

Its per-NIC record carries the list of **accelerators affected** by that NIC. The fault originates at
the NIC and the victims are the accelerators, so that is the direction an operator needs.

*Noted, not adopted as a field.* With P2's scope published on both sides the query is answerable in
either direction, and a stored list would be the cross-reference [Alternatives](#alternatives)
rejects. It does change what the docs must show: the worked example goes NIC → accelerators.

#### P14 — one attribute vocabulary, not a third one

The DRA-model driver publishes per interface: interface name, PCI address, PCI vendor and device ids,
NUMA node, MTU, addresses, SR-IOV state (is a PF with VFs, how many, is a VF), whether it is virtual,
whether RDMA is present and the RDMA device's name. Its joint GPU↔NIC allocation is two publishers
each exposing bus and NUMA coordinates plus a selector expression evaluated over them — the same
division of labour as P3.

*Adopted for field naming.* A third name for `rdmaDevice` helps nobody. Addresses are the one item
deliberately deferred — see [Alternatives](#alternatives).

#### P15 — bus distance and interconnect proximity are different axes, and on accelerators with a high-speed link the bus answer is not merely coarse, it is inverted

This one does not come from outside. **The GPUStack runtime component
(<https://github.com/gpustack/runtime>) already carries a node-local topology model, and it has been
sampled on nine real platforms.** Its vocabulary is an ordered distance enum, spaced so a level can
be inserted without renumbering, and it reuses the words an operator already reads out of a vendor
topology matrix:

| level | value | meaning |
|---|---|---|
| `SELF` | 0 | the device itself |
| `LINK` | 5 | **a high-speed accelerator interconnect** — the vendors' three names for it are all this level |
| `PIX` | 10 | at most one PCIe bridge |
| `PXB` | 20 | several PCIe bridges, not crossing the host bridge |
| `PHB` | 30 | crossing the PCIe host bridge (typically the CPU) |
| `NODE` | 40 | crossing between PCIe host bridges **within** one NUMA node |
| `SYS` | 50 | crossing the SMP interconnect **between** NUMA nodes |
| `UNK` | 100 | unknown |

**Eight values, of which six are distances between two *different* devices:** `SELF` is a device
against itself and `UNK` is the absence of an answer, so a pair of distinct accelerators always
reports one of the middle six. The count is a trap worth naming — "six levels", "seven levels" and
"eight values" all describe this one table, depending on which sentinels you exclude. This spec says
**eight values** throughout, because that is the row count and it is checkable against the table.

**Read against its committed real-hardware samples, two of them settle this spec's design:**

- **An eight-accelerator NPU host reports `LINK` between all pairs while its NUMA affinities are
  `6,0,6,0,4,2,4,2`** — the eight accelerators sit across *four* NUMA nodes and are nonetheless
  all-to-all directly linked. A NUMA or PCIe comparison rates those pairs **worst** where the
  hardware rates them **best**. That is not a coarse answer, it is an inverted one.
- **An eight-accelerator host of another manufacturer reports two four-member `LINK` cliques with
  `SYS` between them.** So "all linked" is not a safe default either: a real machine partitions, and
  the partition is exactly what a consumer asking for two well-connected accelerators needs.

Two more samples bound the other end: a two-card host reports `PHB`, and the **local acceptance
machine's two cards report `SYS`** — no high-speed link at all, which makes the local environment a
usable negative case and nothing more.

*Adopted as this spec's distance vocabulary.* **The levels are copied, not imported:** that
component is Python and is not a dependency of this module, so what crosses is the vocabulary and its
numeric spacing, with a test pinning our constants against the table above.

*Also adopted:* the appendix that component records per accelerator on one manufacturer — link count,
link state bitmap, active link count — is the evidence for *why* a pair is `LINK`, and a `LINK` claim
with no active links is a contradiction worth reporting rather than publishing.

### Where the NIC inventory hangs, and why not on a group

A NIC belongs to no accelerator group. `DevicesGroup` is keyed by manufacturer + model + memory, which
a NIC has no meaning under, and a node can carry NICs while carrying no accelerator at all.

So the inventory hangs **on `DevicesSpec` directly**, as a node-level sibling of `Groups`:

```go
type DevicesSpec struct {
    Groups     []DevicesGroup    `json:"groups"`
    Interfaces []DeviceInterface `json:"interfaces,omitempty"`
}
```

And **proximity is not stored as a relation**. Both sides carry `pciRootId`, the new `pciSwitches` and
`numaAffinity`; the scope is computed from them (P2, P3).

### Where the labels come from, and how the gate works

The label chain that exists today, traced — this is why a withheld label *is* a gate:

1. The detector builds the accelerator groups and calls
   `nodefeature.ConstructAcceleratableNodeLabels(eGroups)` (`detector.go:422`), putting them on a
   `NodeFeature` object it owns.
2. NFD merges that `NodeFeature`'s labels onto the `Node`.
3. `NodeFeatureReconciler` (`pkg/worker/controllers/worker/node_feature.go:64`) reads the `Node` and
   emits the capacity/credits labels from them.
4. `ResourceFlavor.spec.nodeLabels` pins a subset of those keys, so a flavor selects exactly the
   nodes carrying them (`nodefeature.PoolFlavorSelector`).

**A label that is not emitted is a node a flavor cannot select.** So gating needs no new admission
gate, no taint and no condition. The failure is recorded in `Devices.spec` — with the reason and the
time it was first seen (P7) — so an operator can see *why* a node stopped being selected, which a
missing label alone would never tell them.

### User Stories

#### Story 1

As an operator running a distributed workload over RDMA, I want the scheduler to be able to prefer a
node whose accelerators and RDMA NIC are behind the same PCIe switch, so that the collective does not
cross a socket for every byte on a node where it did not have to.

#### Story 2

As an operator whose node has an RDMA interface with a dead link, I want that node to not be selected
for an RDMA workload at all, and to be able to read for how long it has been that way, so that I learn
about it from the node's record rather than from a collective that hung.

#### Story 3

As an operator bringing up a node before there is a cluster, I want `preflight` to tell me whether the
RDMA link verifies, so that the answer costs one container run rather than a full install and a failed
job.

#### Story 4

As someone reading a node's `Devices`, I want to see which NICs the machine has, where they sit, and
which accelerators each one is close to, so that "the NIC is on the wrong socket" is something I can
read rather than derive from `lspci` by hand.

#### Story 5

As someone writing the downstream spec that will enforce strong affinity, I want the record to carry
the *tightest* scope each pair shares rather than a node-wide boolean, so that I can implement a
per-device predicate without re-discovering the hardware.

### Core Features & Acceptance Criteria

#### F1 — the accelerator side: the switch path lands, the interconnect axis does not

Two axes were designed for `DeviceTopology` (P15). **One landed.** The bus axis below is in the API
and populated; the interconnect axis after it is a design record with no field and no code behind it.

**The bus axis** — one field mirroring what `binding.PCIDevice` already computes:

```go
// PciSwitches is the upstream PCI bridge/switch path, innermost first. Two devices sharing the
// whole path are behind the same switch, which is tighter than sharing a root complex.
PciSwitches []string `json:"pciSwitches,omitempty"`
```

- Populated in `device.ConstructTopology`'s caller chain for all nine manufacturers, from the same
  `binding.PCIDevice` the detectors already hold. **Deterministically ordered** (innermost-first by
  construction; the acceptance is that two consecutive reads are byte-identical) — P8.
- **A group is pairwise, and the clique property is CHECKED rather than assumed.** Reachability is
  transitive, so `A-B` plus `B-C` puts all three in one connected component while the field promises
  that members are peers. One id per accelerator can only carry a disjoint union of cliques, so a
  component that is not all-to-all is refused and reported as a self-contradiction rather than
  published — telling a consumer that `A` and `C` are peers because both link `B` is the overclaim
  this feature refuses everywhere else. Refusing the whole component is deliberate: the maximal
  cliques of a non-clique component overlap, and an accelerator in two of them cannot be named by a
  single id at all. An earlier revision asserted that a connected component *is* all-to-all, which
  is simply false on a partial mesh, and its unit test pinned the wrong answer as correct.
- **The component is closed over links claimed in EITHER direction**, which is what lets the check
  above see a one-way claim at all. Following only the forward direction made the outcome depend on
  report order: given a claim of `B->A` alone, visiting `B` first assembled the pair and refused it,
  while visiting `A` first produced two single-member components — skipped as "not an interconnect",
  so identical hardware either reported a contradiction or said nothing. A test for the one-way case
  existed and pinned only the orientation that happens to pass, which is the general trap: an
  all-pairs check decides nothing about a pair the traversal never hands it, so the two halves are
  one guarantee and have to be asserted as one.
- **`pciRootId` is the outermost bridge, not a root complex** — see
  [What is already there, measured](#what-is-already-there-measured) for the loop this was read off
  and the boundary on that reading. The field comments on both sides say what the value is, because
  the name does not. A prior art that names its equivalent field after a *PCIe switch* has the same
  gap in the other direction, and this spec repeats neither: `pciSwitches` is added rather than
  either field being reused for a level it cannot carry.
- Absent for a device with no PCI path at all (P5, P10), never an empty-but-present proximity marker.

**The interconnect axis was designed and did not land.** No field carries it, and no code computes
it: an earlier revision of this round added a `DeviceTopology.interconnectGroup` string and the
grouping behind it, and both were withdrawn before merge because nothing produced a value and
nothing read one — the field was empty on every node while its tests passed, which is the shape that
reads as live to the next contributor. What follows is therefore a **record of settled design, not a
description of behavior**; it is kept because
[Open Question 3](#3--which-manufacturers-get-an-interconnect-topology-provider-and-when) has to
answer the producer question, and re-deriving these constraints then would cost more than reading
them. None of it is a commitment that the axis will be implemented in this shape, or at all.

- **It is the connected component at `LINK` level**, per the decision to carry a group id rather than
  an N² matrix. It was exercised against committed real-hardware topology samples — an
  eight-accelerator host whose pairs are all `LINK` yielding **one** group, another manufacturer's
  with two four-member cliques yielding **two**, and a two-card machine reporting `SYS` yielding
  **none** — and those fixtures were removed with the code, so no test asserts any of it today.
  Reducing the matrix to a group is lossless at this level precisely because a clique is all-to-all
  internally.
- **Node-local and cross-node identity are the same field, filled from different sources**, which is
  what makes a cross-node answer reachable at all:
  - *node-local*: the vendor's pairwise topology call, already bound —
    `nvml.GetTopologyCommonAncestor` (`binding/nvml/library_device.go:307`) and
    `mtml.GetTopologyLevel` (`binding/mtml/library_device.go:118`).
  - *cross-node*: for the one manufacturer that offers it, the fabric identity — also already bound:
    `nvml.GetGpuFabricInfoV()` with `V1`/`V2` returning `{ClusterUuid [16]byte, Status, CliqueId,
    State}` (`binding/nvml/library_device.go:319`, `zz_generated.types.go:988`). **Two accelerators on
    two different nodes reporting the same `(ClusterUuid, CliqueId)` are in one interconnect domain**,
    so that half of the cross-node problem is answerable by a node-local read and needs no scheduler.
  - When fabric identity is available it **wins**, because it is the only one of the two that is
    unique beyond this node; the node-local component id is then namespaced by the node.
- **A `LINK` claim with no active links is a contradiction, and is not published as a group** (P15):
  where a manufacturer exposes link counters, an all-links-down accelerator is excluded from
  grouping rather than published as a group that will fail the first collective placed on it.

  **Where that reason would go, since there is no field for it.** `DeviceTopology` carries no
  per-accelerator status string, and adding one for a state that clears on the next pass would cost
  more API surface than it explains — the same call F3 makes for a failed enumeration. The excluded
  accelerators would be named in the log, at `Error`. The cost is stated rather than hidden: **from
  the record alone, "this card has no interconnect" and "this card claims one that does not work"
  would be the same value**, and only the log would separate them.

- **An absent link counter is not an absent link.** A manufacturer exposing no counters cannot be
  asked, and that must not be read as the answer being no — such accelerators still group. The
  converse is equally explicit: an accelerator that never claimed a peer *and* reports no active
  links is ordinary hardware with no interconnect, not a contradiction. Reporting one there would
  send an operator after a defect that does not exist.

- **A contradicted accelerator cannot bridge two others.** Grouping traverses only the eligible set,
  so two accelerators whose only path runs through an excluded card are not published as one group
  whose every collective would route through it.
- `State`/`Status` from the fabric read are carried as the reason: a `NOT_SUPPORTED` fabric state and
  a *failed registration* are different facts, and only the second is a problem to act on.

Acceptance, for whoever implements it: on a lab pair, the eight accelerators report one group and it
matches the committed sample for that platform; on a machine whose cards report `SYS`, both report
**no** group, with a reason, and **not** a group derived from their shared NUMA node. That acceptance
was never run, because the axis did not land — see
[Open Question 3](#3--which-manufacturers-get-an-interconnect-topology-provider-and-when).

#### F2 — `DeviceInterface`, the NIC-side inventory

A new node-level type. Field names follow the established vocabulary (P14).

| field | meaning | note |
|---|---|---|
| `name` | kernel interface name | the identity |
| `pciBusId`, `pciRootId`, `pciSwitches` | bus coordinates | **all absent for a non-PCI interconnect** (P5) |
| `pciVendor`, `pciDevice` | raw hex ids | not a resolved model name — see below |
| `numaAffinity`, `cpuAffinity` | NUMA node and close CPUs | empty means unknown, never normalised to 0 (P10) |
| `mtu` | link MTU | absent ≠ 0 (P10) |
| `up` | operational state | |
| `virtual` | no device behind it (loopback, bridge, veth) | recorded, never dropped |
| `bus` | which interconnect it was found on | so a non-PCI interface is a *kind*, not a hole (P5) |
| `rdma`, `rdmaDevice` | RDMA present, and its name | |
| `sriov`, `virtualFunctions` | is a PF with VFs; the VFs **nested under it** (P9) | not flat siblings |
| `link` | the verification result — see F5 | with reason and first-seen time |

Acceptance:

- Every field is populated on a node carrying at least one physical NIC, agreeing with what the host's
  own `ip -d link show` and RDMA device listing report, recorded on the pull request.
- **`virtual` interfaces are recorded and marked, not dropped.** A node whose only interface is a
  bridge reads as "one virtual interface", never as "no interfaces" — an empty list reads as a node
  that was never profiled.
- **VFs appear only under their PF**, and the enumeration skips them at top level (P9). A PF with
  eight VFs is one entry with eight nested, never nine entries.
- **A VF is its own type, not a nested `DeviceInterface`.** SR-IOV nests exactly one level deep — a
  virtual function cannot itself be partitioned into virtual functions — so a self-referential shape
  would carry a level nothing can ever fill. What a VF shares with its parent is the upstream bridge
  path — `pciRootId`, which is the outermost bridge and **not** a root complex, and `pciSwitches` —
  so those are read from the parent instead of repeated on every VF; only what differs per VF is
  recorded, and a VF is a PCI function of its own with its own address, NUMA node and CPU list.
- **`sriov` is a separate boolean from the VF count.** "Zero VFs configured" and "not a PF" are
  different facts, and inferring the second from `len(vfs) == 0` collapses them (P10 — the same
  reading discipline `remaining`'s `omitempty` already demands).
- `pciVendor`/`pciDevice` are raw hex. `binding.GetPCIDeviceNames` exists (`binding/helper.go:393`) and
  is deliberately unused: it reads a host data file a minimal image may not carry, and a name that
  resolves on one node and not another is worse than a hex id that always resolves.

#### F3 — the NIC pass

A node-level pass in `pkg/devicemanager/detector/`, run alongside the per-manufacturer loop rather
than inside it, because it is not a manufacturer's business.

- **It enumerates network interfaces first and resolves the PCI device as an attribute** — the inverse
  of enumerating the PCI bus and correlating to interfaces. P5 is the whole reason: an interface on a
  non-PCI interconnect is invisible to a PCI-rooted walk, and "invisible" on the platform this
  repository most needs RDMA facts for.
- RDMA is resolved through **more than one sysfs layout**, and the layouts tried are named in the
  reason when none answers (P5).
- **Every sysfs read goes through one helper** that caps the byte count and validates that the
  *resolved* path is still under `/sys` (P11). Following symlinks is unavoidable here, so checking
  where they landed is not optional.
- **Linux-only, with a stub**, following `binding/helper_{linux,other}.go`. The whole module builds and
  tests on darwin, so the pass and its tests must too.
- **A read that fails is not an empty inventory, and telling them apart takes three states rather
  than two.** A pass that enumerated and found nothing writes the empty list, because that IS an
  answer. A pass that could not enumerate at all leaves the previously recorded inventory untouched,
  because an empty list reads as "this worker has no interfaces" and a failed read cannot support
  that claim.

  **On a first pass there is nothing to preserve**, so a worker whose enumeration fails is
  recorded with no interfaces and the failure lives only in the log — at `Error`, the one level no
  verbosity setting hides. There is deliberately no "could not enumerate" field: inventing API
  surface for a state that resolves on the next pass would cost more than it explains. In
  `preflight` the same failure reaches the report's `note` and not the exit code
  ([F8](#f8--the-preflight-link-check)): that code is reserved for an `unavailable` accelerator
  answer, the one state an allocation is refused on, and a link fact is not an allocation
  precondition. A reader who needs the failure to fail a gate reads the note.

  **A partial absence is the same failure mode as a total one.** One interface's virtual function
  set that cannot be listed ends the pass rather than shortening a list: the caller has already
  committed `sriov: true` from a file that answered, so a swallowed listing failure publishes "a PF
  with zero VFs configured" — which this inventory keeps deliberately distinct from "not a PF" — and
  on a host whose RDMA lives only on its virtual functions that absence withdraws `rdma.capable`.
  The rule that separates it from the attribute readers beside it is what the degraded record would
  CLAIM: an unreadable attribute degrades to "unknown", which is true, while an unreadable virtfn
  set has no way to say "unknown" and so degrades to a false count.

  **A non-virtual interface whose `device` link does not resolve ends the pass for the same reason,
  and this one was refused once before being accepted.** The degraded record is `rdma: false` with
  no verdict — byte for byte what a plain Ethernet NIC produces — so on a node whose only RDMA
  interface hits it, `rdma.capable` is withdrawn on the strength of a symlink that could not be
  read, which is precisely what the `unverified` state exists to prevent. Synthesizing `unverified`
  there fails the other way, by inventing a usable endpoint on a node that may carry no RDMA at all.
  Neither claim is available from an unresolvable link, so the pass fails and the previously
  published inventory and labels stand. The earlier refusal was argued from the wrong consequence —
  a virtual function published at top level, violating P9 — which is real but minor beside a
  withdrawn label.
- **The net class directory is not exclusively interfaces, and an entry that is not one is skipped.**
  With the bonding driver loaded it also holds `bonding_masters`, a regular file that is the driver's
  control surface; measured on two RDMA hosts, it is the only non-symlink entry there. Enumerating it
  published a record carrying a name and nothing else, which reads as an interface whose bus could
  not be determined — and the e2e case's first check, which requires every entry to have a name and
  a bus, fails on it. Skipping it is not the "publish an absence" this pass refuses: an entry that
  does not resolve to a directory is not an interface, so recording one INVENTS a presence, the same
  error in the other direction. The test is that the entry resolves to a directory, not that its
  name is on a list, because a list would be a claim about which control files exist.
- **The published list is sorted by interface name, and nested VFs by bus id** (P8).

Acceptance: on the local cluster's two nodes the pass reports each node's real interfaces; two
consecutive passes with no hardware change produce byte-identical output (the P8 criterion); making the
RDMA sysfs trees unreadable yields `rdma: false` with a reason naming what was tried, not a crash and
not a silent empty list.

#### F4 — the sync path, the comparison that decides it, and the ordering

The detector syncs `Devices.spec` through `devsAlginFn` (`detector.go:487-500`). That function's
update decision is **one comparison**:

```go
if !kubemeta.DeepEqual(aDevs.Spec.Groups, eDevs.Spec.Groups) { ... }
```

**`Spec.Interfaces` is not in it, so without an explicit branch a changed interface inventory is
computed every pass and written never.** This is the single most likely way this feature ships broken
while looking healthy, so it is an acceptance criterion rather than a note:

- `devsAlginFn` compares `Spec.Interfaces` and assigns it, independently of the `Groups` comparison.
- A test drives the align function with `Groups` identical and `Interfaces` differing, and asserts
  `skip == false` and the new value assigned. Its mutation check is to **delete** the branch and see
  the test fail — a test that passes without the branch is testing nothing.
- **The converse is equally a criterion (P8):** with nothing changed, `skip` must be `true`. An
  unsorted list makes this fail intermittently and the symptom is an API write on every pass, forever
  — which no functional test would notice.

**Two things about the layer this hangs on, both verified rather than assumed:**

- **It goes in Spec, not Status.** `Devices.status` is the *allocation* ledger: `buildDesiredStatus`
  (`pkg/deviceplugin/controller.go:199-234`) rebuilds it wholesale each reconcile by seeding one
  allocation per **Spec** accelerator and merging Pod annotations in. It carries
  `allocated`/`remaining`/`mode` and nothing descriptive. A topology fact in Status would be
  re-derived from Spec every pass, i.e. a copy; in Spec it is the source. So the "Status is rebuilt
  wholesale, fold your field in" hazard **does not apply to this field** — stated because it was
  checked, not because it was obvious.
- **The hazard that does apply is the ownerRef conflict.** `Devices` is owned by the `Node`
  (`detector.go:483`, a cluster-scoped dependent of a cluster-scoped owner, deliberately). A conflict
  there blocks the **spec** sync while status keeps updating — so the object looks healthy while its
  interface inventory is stale, with a warning event as the only signal. Acceptance therefore reads
  the interface list back **from the API server** after a detector restart, not from the detector's
  own logs.

#### F5 — the RDMA link verification

`rdma: true` says an RDMA device is bound. It does not say the link works, and the two differ on real
hardware — an NPU's RoCE port can be fully configured, with an address, mask and gateway that our
existing DCMI read already returns, while its link is down.

**The aggregation from per-interface states to the one node label is EXISTENTIAL, and it is stated
here because leaving it implicit cost us a defect.** The reduction is over ENDPOINTS, not over bound
interfaces, and the difference is load-bearing: an endpoint is every interface and every virtual
function, and it counts as usable when its verdict is anything but `failed`, falling back to whether
a device is bound only when no verdict was reached. So the unreadable-tree record — `rdma: false`
carrying a synthesized `unverified` verdict — **is** a usable endpoint, and `rdma.capable` is
withheld only when no endpoint is usable. Stating it as "every bound interface reports `failed`"
reads as the same rule and is not: it makes the unbound-but-judged record invisible, which is the one
record the `unverified` state was introduced for. A node with a broken NIC beside a working one keeps the label, for exactly the
reason [P12](#p12--the-check-registry-is-data-and-checks-must-not-overlap-in-what-they-attribute)
already gives: withholding there would let an unplugged second card take a working node out of
scheduling. An RDMA device bound to a virtual function counts the same way, since a VF is nested
under its physical function rather than listed at top level.

The rule was never written down, and three artifacts each assumed a different one: the
implementation and its unit test were existential, the e2e case asserted universal (so it would
have failed a correct implementation on any mixed node), and a reviewer read F5 as universal too.
Only the first was right, and no test could have caught the disagreement because each side was
self-consistent.

**An EXPLICIT verdict outranks the `rdma` flag**, and that ordering is part of the gate rather than
an implementation detail. The unreadable-tree record carries `unverified` with `rdma` false — the
reader will not claim a device it could not read — so a rule that tested the flag first turned "this
port's state could not be read" into "this node has no RDMA" and withheld the label on the strength
of a file that could not be opened. That is the same unsupported claim as publishing `rdma: false`
for an unreadable tree, arrived at one layer further on; only the no-verdict case falls back to the
flag. The fixture that was supposed to cover this pinned a *bound* device with an unverified link —
the one sub-case the flag carries regardless — so the state existed and the gate never saw it.

**A virtual function is its own endpoint in the reduction, carrying its own NUMA affinity.** The
type has one and the sysfs reader fills it from the VF's own device directory; folding the VF into
its parent published the parent's socket for endpoints that never contributed to it. The PCI path is
still inherited, which is a different claim and remains true — a VF sits behind the same bridges.

Three states, and the separation between reporting and enforcement is the explicit state list (P6):

| state | meaning | effect on the label |
|---|---|---|
| `ok` | the link verified | `rdma.capable` is emitted |
| `unverified` | the check ran and could not establish an answer | `rdma.capable` is emitted, and says so |
| `failed` | the check ran and the link is not usable | **`rdma.capable` is withheld** |

- **`unverified` emits the label, deliberately.** A node must not be silently excluded by our
  inability to ask. Withholding there would turn "this port's state could not be read" into "this
  node has no RDMA", a worse lie than the one it prevents. Concretely: `failed` is reached only when
  every port was read and none carried the link — one unreadable port among several down ones leaves
  "all ports are down" unestablished, so that mixed case is `unverified` and says which port was
  unreadable.
- A `failed` result **always** carries the checker's own words, and **the time the failure was first
  seen** — stable across passes while it persists, cleared when it clears (P7). Refreshing it every
  pass makes "how long?" unanswerable, and that is the operator's actual question.
- **That stability is not a property of the checker; it has to be merged in before the comparison,
  and this requirement collides with P8's.** The pass observes the failure but not when it started,
  so a first-seen time taken from the clock makes the F4 comparison never match: an API write on
  every pass, forever, with correct data in the object throughout — the defect P8 names, caused here
  by P7. So the stored inventory's times are merged into this pass's before it is compared, keyed on
  the **state** rather than on the reason: a second port going down changes the reason and is the
  same outage, which is what "how long?" asks about. A stored failure with no time is stamped rather
  than carried, so an object written before the field existed converges instead of staying blank.
  The create path stamps too — an object created carrying a failure with no first-seen time would
  publish an outage with no beginning until a later pass filled it in, contradicting the "always"
  above.
- **The link is read from the RDMA port's own state attributes, and this is not a guess.** Verified in
  prior art: the NPU-platform plugin's HCA-port fault check is exactly a pair of sysfs reads —
  the port's `state` (must report active) and its `phys_state` (must report the physical link up) —
  under `/sys/class/infiniband/<device>/ports/1/`, with both values carried verbatim into the reason
  and an unreadable file reported as `UNKNOWN` rather than as healthy. It runs no vendor CLI. So F5
  needs neither a new binding nor host access: **it is a sysfs read the DaemonSet can already make.**
- **Ethernet-side link state is a second, separate read, and it feeds nothing that gates a label.**
  `operstate` and `carrier` fill the DESCRIPTIVE `up` boolean and stop there; the link verdict comes
  only from the RDMA port attributes above. That is P12 in force, not an omission: a single "the
  network is broken" signal fed by both would let an unplugged management NIC withhold
  `rdma.capable`.
  **The scope of that counter-argument, stated because it is narrower than the rule it defends:** the
  management-NIC case is about aggregating ACROSS interfaces, while the case it does not answer is a
  single RoCE netdev that is administratively down while its own HCA port still reads
  `ACTIVE`/`LinkUp` — there the endpoint counts as usable on the strength of a port whose netdev
  carries nothing. The rule is kept anyway, on the measurement that a RoCE port's state tracks its
  netdev: the ports observed down on real hardware read `DOWN` and `Disabled` rather than `ACTIVE`,
  so the divergent pair may not be reachable at all. That it *may* not be reachable is the honest
  statement — the shape has not been produced on any machine this spec had, so the gap is recorded
  rather than closed.
  ⇒ So an unreadable `operstate` or `carrier` does **not** yield `unverified` — it yields `up: false`,
  and nothing downstream reads it as a verdict. An earlier revision of this criterion said the
  unreadable case reaches `unverified`, which described a design where the two reads shared one
  verdict; that design was dropped for P12's reason, and the requirement is corrected here rather
  than left promising behaviour the implementation deliberately excludes. The prior art's asymmetry
  (an unreadable `operstate` treated as *not* down, an unreadable `carrier` treated as down) is
  likewise not adopted, and now has nothing to be asymmetric about.
- **There is one check, not a registry keyed by manufacturer** (P12). The two attributes above belong
  to the RDMA subsystem rather than to a vendor, so the dispatch table this spec first called for
  would have had a single entry — and would have created the state "no check is implemented for this
  manufacturer", which does not otherwise occur.
- **Every port is consulted, and the verdict is not returned early.** A multi-port HCA whose first
  port is the unused one would otherwise report as unusable. `ok` is returned as soon as any port
  carries the link; `failed` only after every port has been read.
  **Which topology that is right for, and which it is not.** It is right when the device's ports back
  ONE netdev: an unused port there says nothing about the interface. It is wrong when a single RDMA
  device's ports map to DISTINCT netdevs — the InfiniBand-mode shape, where one card presents one
  device with two ports and two IPoIB interfaces — because the verdict is then copied to an interface
  whose own port is down, and the per-interface ledger and the preflight row both overstate it. The
  device-level verdict is kept because attributing a netdev to its port needs
  `ports/*/gid_attrs/ndevs/*`, and **no machine available to this spec presents that shape**: every
  RDMA device observed carries a single port, so the mapping code could be written but not exercised,
  and an unexercised attribution in this position fails by withholding a label. The narrowing is
  recorded here so the next reader does not have to rediscover that the reason above covers one
  topology only.

Acceptance: on the remote lab pair, a node with a verified link carries `rdma.capable`; the same node
with the link administratively down reports `failed` with both state values in the reason, a stable
first-seen time across at least three passes, and **does not** carry the label — and the
`ResourceFlavor` whose `nodeLabels` pin that key stops selecting it. That last clause *is* the
acceptance: the label's absence has to be observed **through a flavor**, because a key going missing
is not the same event as the node dropping out of scheduling.

#### F6 — anything with nothing to check says so

- Every interface appears in the record, and every RDMA device that appears carries a link state in
  words — never an omitted field and never a defaulted `ok`.
- **Two facts, not three.** "No RDMA device at all" and "the RDMA tree or its port state could not be
  read" are distinct and get distinct treatment: the first is already carried by `rdma: false` and
  gets **no** link state, because a verdict there would be a verdict on a check that was never in
  question; the second is `unverified` with the path in the reason. The third fact this spec
  originally listed — "no check is implemented for this manufacturer" — **no longer exists**, because
  the single check of F5 applies to every RDMA device there is. It was removed rather than described
  better.
- This follows the preflight spec's own rule: an empty result reads as a node that passed.

#### F7 — the labels, and the two hard limits on what a label can carry

**Two limits on the consuming end, both verified, and together they decide which of these facts can
ever reach a scheduling decision through this chain:**

1. **`ResourceFlavor.spec.nodeLabels` is an equality selector** — its own contract says it associates
   the flavor with "Nodes that **have the same labels**"
   (`pkg/kubeclients/applyconfiguration/kueue/v1beta2/resourceflavorspec.go:15-25`). So an ordered
   value is *readable* but not *comparable* through it: "at least NUMA-level" cannot be expressed, and
   `distance == PIX` is the only kind of question it can ask.
2. **`nodeLabels` is capped at eight elements** (same contract). And `nodefeature.PoolScheduleLabels`
   already emits up to **five** — the acceleratable boolean, os, arch, the accelerator key, the CPU
   key (`pkg/nodefeature/helper.go:150-167`) — with the flavor additionally carrying a `.count` key
   (`node_devices_admission.go:467` reads exactly that composite). **So the budget left for this
   feature is roughly two keys, not three or four.**

**That cap constrains SELECTOR keys, not Node labels**, and an earlier revision of this section
conflated the two — it justified the small label set with the budget, when a Node may carry any
number of labels and only the ones a flavor pins are counted. The corrected reason the set is small
is that these are the only two facts worth publishing, and the corrected reason only one of them is
selectable is limit 1 above: an equality selector cannot compare an ordered value at all.

So the set below is **one** selector key plus two informational ones:

| label | value | in the flavor selector? |
|---|---|---|
| `…/rdma.capable` | `true` | **yes** — it is the one key F5's gate needs, and it costs one of the two remaining slots |
| `…/rdma.distance` | `LINK`\|`PIX`\|`PXB`\|`PHB`\|`NODE`\|`SYS`\|`UNK` — the **closest** distance any accelerator has to an RDMA-capable interface | no — informational |
| `…/rdma.numa` | e.g. `0` or `0_1` — underscore-joined, for the reason two bullets below | no — informational |

- **The distance vocabulary is the product's existing eight-value one (P15), not a scale invented
  here.** A test pins our constants to those levels and their numeric spacing, so one product carries
  one vocabulary for one question.
- **Not every level in that vocabulary is reachable from the coordinates this feature publishes,
  and which one is missing is not the one an earlier revision assumed.** `PHB` is never produced:
  distinguishing it from `NODE` requires knowing whether two devices share a PCIe host bridge, and
  the coordinates stop at the outermost PCI bridge — the host bridge's own component is not among
  them. Where the two are indistinguishable the **further** is reported, because the value feeds a
  proximity claim and overclaiming closeness is the error nothing downstream can catch. `LINK` and
  `SELF` are likewise never produced here, for the reasons already in P15 and in F1. Reaching `PHB`
  would mean capturing the root-complex component during the PCI walk, which is a change to a value
  nine manufacturers already publish and is therefore not this round's.
- **The two axes are never mixed.** `rdma.distance` is the *bus* answer about an accelerator↔NIC pair.
  Accelerator↔accelerator proximity is the interconnect axis (F1), which does not land in this round
  in any form — no field, and no label. See the next bullet for why a label was never the answer for
  it, which is a finding rather than a preference and outlives the withdrawal.
- **An interconnect group could not reach a scheduling decision through this chain even if it were
  published, and that is the load-bearing consequence of the two limits above.** What a consumer
  wants to ask is "give me four accelerators that are LINK peers" — a *cardinality over a group*.
  Equality matching cannot express it; a laddered encoding (`group-size-ge-4=true`, `…-ge-8=true`)
  could, but every rung is a separate key and the budget is two. **So such a group's only viable
  consumers are the device plugin's preferred allocation (node-local) or a scheduler (cross-node)** —
  neither of which is this spec. That is why the axis was never going to be a label, and it is
  independent of the axis not landing at all.
- `rdma.numa` is a set: a node with RDMA on both sockets carries both values, ordered and joined
  with an **underscore**. An earlier revision of this line said "comma-joined and sanitized
  through `kubemeta.SanitizeLabelValue`", which is self-defeating and was measured to be: **a comma
  is not a valid label value character**, and it does not fail validation — that sanitizer drops it
  silently, so `{0,1}` publishes as `01` and reads as NUMA node 01. A dash was rejected in turn
  because `0-1` reads as a range rather than a pair.
  **The general rule this leaves is worth more than the separator:** a sanitizer applied to a
  *scalar* is a safety net, and applied to a **composite** value it destroys the structure without
  complaining. The value must be valid *before* it is sanitized, which makes the sanitizer a no-op —
  and that, rather than one separator, is what the acceptance asserts.
- **Every label here is node-level, so each loses information on purpose, and one has a validity
  limit.** "*Some* accelerator is `PIX` from *some* NIC" is not "the accelerators this workload gets
  will be". Per-accelerator distance cannot be expressed in a node label at all. And per P4, a
  node-level proximity assertion is only sound for a request holding its accelerator **exclusively**:
  for a sliced or shared accelerator, the fraction you are given need not be the close one. Both
  limits are stated in the spec, in the doc page and on each label's own constant — a reader needing
  the finer answer reads `Devices`, which carries the per-accelerator truth (P3).

##### F7a — the labels have no in-tree consumer yet

**Verified, and it is a product gap rather than an untestable criterion.** Nothing in this repository
reads a Kueue `Topology` or sets `topologyName`: grep for `TopologyAware`, `topologyName` and
`kueue.x-k8s.io/topology` across `pkg/` and `api/` returns zero hits outside the vendored chart, which
does ship the CRD.

So of the three labels exactly one has a live consumer path on the day this lands: `rdma.capable`,
because `ResourceFlavor.spec.nodeLabels` can pin it, which is what makes F5's gate observable.
`rdma.numa` and `rdma.distance` are **published and unconsumed**, and the interconnect axis is not
published in any form: no label — because F7 establishes it could not reach a decision through one
even if we minted it — and, after the withdrawal, no field and no producer either. See
[Open Question 3](#3--which-manufacturers-get-an-interconnect-topology-provider-and-when). Whether the two informational labels should exist at
all is [Open Question 2](#2--is-rdmadistance-worth-publishing-at-all-given-f7s-two-limits).

Recorded honestly as: *the rendering surface does not offer topology-aware scheduling, and whether it
will has not been decided.* It is not "untestable" — their presence and value **are** assertable, and
F7's acceptance asserts them; what is absent is a *scheduling outcome* to assert against. Writing them
off as untestable would make them the cell everyone skips. Naming the gap makes it the entry point for
the decision.

#### F8 — the `preflight` link check

`preflight` already reaches host binaries through a bind-mounted host root and cross-checks our reads
against the host's own vendor CLI. The link check is one more row on that surface.

- One row per RDMA-capable interface **and per RDMA-capable virtual function** (named
  `<interface>/<vf bus id>`), carrying the identity, the RDMA device name, the F5 state and the
  checker's own words. An entry with no RDMA device **and no verdict** gets **no** row — `rdma:
  false` alone already carries that — while an inventory holding an RDMA device with no verdict, or
  a verdict with no device, gets a row saying so rather than being dropped. Reading only the top
  level made this section contradict the node's own labels on an SR-IOV host, where every RDMA
  device is a VF: P9 nests them, so the same traversal the labels needed (F5) is needed here.
- It **reuses F3's whole pass**, not merely F5's checker. "Two callers that agree" is not the
  property F8 needs; "the same computation" is, so the detector's inventory entry point is exported
  and the preflight reports the records it returns. The alternative — exporting the checker and
  enumerating again here — would leave two *interpretations* of the same sysfs able to diverge, which
  is the drift the shared PCI walk was made to eliminate rather than document.

  **What the reuse does not buy is agreement with the published record**, and saying so is the point
  of this paragraph. `PreflightNetwork` performs its own sysfs read when it is invoked, while the
  record on the object is as old as the last pass that had a reason to run — so the two legitimately
  differ, and the preflight row is the newer of the two. This criterion and three doc and comment
  sites each said the preflight "renders the stored records rather than reading sysfs again", and
  all four were wrong the same way: sharing an implementation removes interpretation drift, never
  observational divergence. The property is stated here so it is not reintroduced as an improvement.
- It is a **read**: nothing is configured and no link is brought up, so `--dry-run` changes nothing
  about it (unlike the driver-mode toggles, which `--dry-run` withholds). The entry point therefore
  takes no options at all, which is what makes that unable to drift.
- Depth labelling follows the existing three: `declared` when it is sysfs or the driver answering,
  `measured` when a host CLI ran. Every row here is `declared`, and the field is carried rather than
  omitted so a reader cannot take the answer for a measured one.
- **The rows are a section of their own, and the report's top level changes shape.** A NIC belongs
  to the machine: the per-accelerator row type requires an accelerator id and an allocation mode, and
  an interface has neither, so filling them with empty strings would make a link row
  indistinguishable from a malformed accelerator row. The document's top level therefore becomes a
  map with two sections where it was a bare list. The alternative considered was appending the
  section as a **second YAML document** to the same stream; it was rejected because a reader taking
  only the first document keeps working while silently seeing none of the new section, and a silent
  truncation is worse than a shape a reader fails loudly on. The one parser of this document in the
  repository does fail loudly, which is the evidence for that choice rather than an argument for it.
- **A link row does not decide the exit code, and this is a decision rather than an omission.**
  What the command's failure answers is whether the node can serve the allocation modes its
  allocators offer, and a down link stops none of them — it withholds a node label, which changes
  what a flavor selects rather than what an allocator can hand out. A link row that failed the pass
  would make every script gating an install on `preflight` start refusing nodes that allocate
  perfectly well. A test exists whose only job is to keep that wiring absent.
- **The row's state is the link vocabulary, not `PreflightState`** (P14). The three link values
  already exist and already decide whether the label is emitted; mapping them onto three allocation
  states — each with a different consequence attached — would be a second vocabulary for one fact,
  and the row a reader sees would no longer be the value the node publishes.

#### F9 — documentation

- `docs/architecture/device-discovery.md`: the NIC pass, what `DeviceInterface` records, why
  enumeration starts at the interface rather than the bus (P5), and the sysfs read discipline (P11).
- `docs/architecture/scheduling-chain.md`: the three labels, **the scope ordering**, the two limits
  from F7, and the gate — that a withheld label is how an unusable node stops being selected. The
  worked example runs NIC → accelerators (P13).
- The `Devices` field reference wherever the existing `DeviceTopology` fields are documented,
  including what `pciRootId` actually holds and `pciSwitches` being the tighter fact.
- Routed per the `gpustack-operator-docs` skill, with the index and contents blocks kept in sync.

### Verification

| environment | what it establishes | what it cannot |
|---|---|---|
| **local two-node Kubernetes cluster** (control-plane: 2× RTX 4090, no MIG and no SSH; worker: 2× RX 7800 XT, also the native amd64 builder) | the NIC pass on real hardware; `virtual` classification; the switch/root/NUMA scope computation against `lspci -t`; the P8 idempotence criterion; the `devsAlginFn` write path end to end; **the negative case** — no RDMA hardware ⇒ `rdma: false`, no `rdma.*` labels, no flavor change | anything with `rdma: true`. **Neither node is expected to carry an RDMA NIC** — consumer GPUs on commodity Ethernet — so the positive path is unreachable here. That is a property of the hardware, and it is why the row below is required rather than optional. |
| **remote two-node RKE2 lab pair** (8 NPUs) | `rdma: true` with a real RDMA device name; F5's three states including `failed` with both port state values; the stable first-seen time across passes; the withheld label observed *through* a `ResourceFlavor` ceasing to select the node; F8's preflight rows | the declared-table hardware of P1, which is a different chassis |
| darwin, local | the whole module builds and tests, including the `_other.go` stub path | any sysfs read |

The local rows are runnable without the lab and must not be blocked on it.

**The positive path is measured, and it took no deployment and no cloud machine.** The pass was run
against the remote pair's real sysfs by shipping one read-only binary, which is what the root
parameter on the enumeration exists for. On the heterogeneous host it reported 20 interfaces across
two RDMA drivers, with **`ok` on four bound endpoints and `failed` on two** — the second state
reached without breaking anything, because two of that host's ports are administratively down
already. The `failed` records carry the kernel's own words: `port 1: state="1: DOWN"
phys_state="3: Disabled"`. Three consequences worth keeping:

- **The link-state vocabulary the fixtures assume is the vocabulary the kernel emits**, character for
  character, on two drivers and two kernels. That retires the assumption this section recorded above
  rather than confirming a guess about it.
- **An RDMA device name cannot be derived.** The same driver on the same slot address is named
  `mlx5_0` on one host and after its PCI path on the other, because the name comes from host
  configuration. Taking `filepath.Base` of the resolved directory is correct *because* it derives
  nothing, and no test or case may hardcode a device name.
- **The withholding half is still not observed on real hardware**, and the reason is worth stating:
  the label is aggregated existentially over every endpoint on the NODE, so driving it needs every
  bound port down, and on the heterogeneous host one of those ports is a member of the bond that
  carries the administrative connection. A smaller scope does not exist within a node.

**Two kinds of unverified, which must not share a marker.** They read the same in a checklist and
need completely different things to close.

- **Not verifiable in any environment this project has.** The withholding half of the label gate,
  and the virtual-function path. Both need a node whose every bound RDMA port can be taken down, or
  whose physical functions can be given VFs — that is, a machine with RDMA hardware that nobody else
  is using. Of the two RDMA hosts available, one has such a port enslaved to the bond carrying the
  administrative connection, and the other is serving workloads that are not ours. What closes these
  is a machine, not a work session. They are recorded here rather than as tasks, because a task
  implies it can be picked up.
- **Written and not yet run.** `case-52` as a suite, against a cluster carrying this build. What
  closes it is a deployment window and nothing else.

**What the lab is for changed once F5 became a single file read**, and this correction is recorded
rather than quietly applied: an earlier draft of this section said the lab was the only place F5's
positive and `failed` states could be established. That was true of the per-manufacturer registry it
was written against, whose positive states sat behind a driver handle. With one check reading the
RDMA subsystem's own attributes, **all three states are established under fixtures**, and what only
the lab can establish is narrower and different in kind: that a real HCA's `state` and `phys_state`
carry the values the fixture assumes, and that the withheld label is observable *through* a
`ResourceFlavor` ceasing to select the node. The first is an assumption about the kernel's
vocabulary; the second is not a unit-testable fact at all.

**A fourth source is not an environment but is stronger than one for the distance and interconnect
axes: the committed real-hardware topology samples in the product's runtime component** (P15). They
cover nine platforms, including both of this spec's acceptance machines and the remote lab's, so
T2's and T5's outputs are checked against a recorded reading of the same hardware rather than
against our own second reading of it. One of those samples is load-bearing here:

- the lab platform's sample shows **all pairs `LINK` while the accelerators span four NUMA nodes** —
  which is why T10's distance may never be read as an interconnect answer: on that machine a
  NUMA-derived proximity rates the closest possible pairs as the furthest apart. The sample pins the
  inversion as a measured property of real hardware rather than a hypothetical, and it is the whole
  reason F1's withdrawn axis was never going to be derivable from the bus coordinates that did land.

Using them is a **cross-check, not a substitute**: they were produced by a different implementation
and a mismatch means one of the two is wrong, which is the point. A sample that cannot be reproduced
is a finding to report, never a test to relax.

Reading the kubeconfig on the local cluster's worker needs `sudo cat` to a copy under `/tmp` and
`export KUBECONFIG`, because the login user cannot read the k3s config directly.

### Notes / Constraints / Caveats

- **`make generate` must run on a module-suffixed physical path**, and a failure is destructive. This
  spec adds API fields, so the generate pass is mandatory and is its own task boundary.
- **`make lint` is an edit pass** — it rewrites the whole module — and a cold run exceeds two minutes.
  Do not give it a two-minute timeout.
- **CGO detectors build and test on darwin**; only the image needs linux.
- **The equality the detector compares with treats a nil and an empty slice as equal, and two
  orderings of the same elements as different.** Both measured against `kubemeta.DeepEqual`
  (`equality.Semantic`). So no new slice field needs nil-normalising — and an unstable order is a
  permanent write loop, which is why ordering is an acceptance criterion and not a nicety.
- **`InstanceTypeSpec` must stay comparable** (it is used as a map key). Nothing here touches it, and
  nothing here may add a slice or map to it. Note that `DeviceInterface` and the new `PciSwitches`
  both carry slices — they are on `DevicesSpec`, not on `InstanceTypeSpec`, and must stay there.
- **The DCMI bindings expose no link entry point.** Verified: `binding/dcmi` carries the RoCE address
  and gateway reads and nothing for link or health. F5 needs neither, because the answer is in sysfs
  (P6/F5) — but a future check that *does* need DCMI would have to regenerate the binding tree, which
  drifts several bindings at once and has to be carried back one at a time.
- **Language and idiom.** Go, matching the detectors' existing shape. snake_case file names
  (`device_interface.go`, never `deviceinterface.go`).

### Boundaries

- **Always:**
  - record every interface, including virtual ones and including a failed read, in words;
  - carry the checker's own message on any non-`ok` link state, and a stable first-seen time on a
    `failed` one — `unverified` gets none, because an unread port is not an outage for a clock to
    start (F5);
  - publish sorted, and pin idempotence with a test — an unsorted list is an infinite write loop;
  - compare `Spec.Interfaces` in the align function, or the field is written never;
  - read sysfs through the capped, resolved-path-validated helper;
  - name an interface's bus from the DEVICE's own `subsystem` link, never from where its path sits.
    A USB NIC resolves through `devices/pci0000:00/.../usb1/...`, so a path-derived answer calls it
    PCI and then stores a USB interface address (`1-1:1.0`) as a PCI one and walks bridges it has
    none of — publishing coordinates a distance is computed from. The path still decides VIRTUAL,
    because a virtual interface has no `device` link to ask;
  - trigger a re-detect on the subset that can affect the gate, NOT on the whole record. Every Pod
    that starts brings a `veth` under `devices/virtual/net`, so comparing everything made Pod
    lifecycle a hardware-change signal: the loop left the monitor, reran the manufacturer's driver
    detection and rewrote the cluster-scoped object per Pod event, per manufacturer DaemonSet. The
    inventory still records every interface; only what forces a re-read is narrowed, and a virtual
    interface carrying an RDMA verdict is never exempt. Note the INTERACTION with the fact above:
    a re-detect is also what repairs an out-of-band edit, so narrowing the trigger bought stability
    at the cost of a hand-made divergence surviving longer. Both directions are deliberate — a
    correct record rewritten on every Pod event is worse than a hand-edited one left standing until
    something real changes — but the second half is a cost and is recorded as one;
  - derive proximity from coordinates both inventories carry, and publish the scope, never a
    cross-reference;
  - keep a preflight row and the node's record derived from one checker.

- **Ask first:**
  - touching `binding/` beyond `ResolvePCITopology` and the `PCIDevice` field comments, which were
    entered by an explicit decision (a shared derivation is the only way to make the two
    inventories' coordinates identical rather than merely intended to be); `pkg/device/` and
    `pkg/nodefeature/` are in scope by decision, and only for what T5 and T10 declare;
  - any run against the remote lab pair;
  - any change to the chart's mounts;
  - adding a distance level beyond the product's existing eight, or any label whose meaning nothing
    in-tree can check;
  - spending one of the flavor selector's remaining label slots on anything other than
    `rdma.capable` (F7).

- **Never:**
  - create or configure a VF, or bring a link up;
  - withhold `rdma.capable` on `unverified` — only on `failed`;
  - reach `failed` from an unreadable file;
  - normalise an unknown NUMA id to 0, or let an absent numeric field read as zero;
  - emit an empty interface list for a read that failed;
  - report `pciRootId` equality as switch-level proximity;
  - implement `Required` affinity semantics, or take a dependency on an external device-scheduling
    system;
  - write anything that identifies a lab host into this spec, the docs, a commit message, a PR body
    or an e2e case — an address, a login, a hostname, or a shorthand that names one in context. The
    rule is stated by what it protects rather than by a list of forms, because a list is checked by
    asking "are these present" and everything not on it passes by default. Verifying it means
    scanning for the SHAPES an identifier can take and judging every hit, and a scan that reports
    nothing is worth only as much as its positive control.

### Risks and Mitigations

- **`Spec.Interfaces` computed every pass and written never**, because `devsAlginFn` compares only
  `Groups` → the inventory is permanently whatever the first write contained, and the object looks
  healthy. → *Mitigation:* F4's explicit branch plus a mutation-checked test. Silent in every
  direction.

- **An unsorted list writes the object on every pass, forever** (P8) → API write amplification
  proportional to node count × detector cadence, with no wrong data anywhere to notice it by. →
  *Mitigation:* F3 sorts, and F4's converse criterion asserts `skip == true` on an unchanged pass.

- **A machine whose pairing is authored rather than discoverable reports `NODE`** (P1) → a report
  that is correct about our bus reading and reads as "badly planned hardware" on a chassis whose
  pairing is in fact designed. → *Mitigation this round:* the scope value is documented as "the
  tightest scope *derivable from bus coordinates*", and `NODE` explicitly does not mean "not
  aligned". **Note the residual uncertainty, which is not resolved either way here:** whether that
  chassis's bus tree would in fact yield a tighter scope is unknown — its PCIe topology is not in
  hand — so this risk is that we *report `NODE` on hardware someone believes is aligned*, not that
  we are provably wrong. The fix, if it is one, is Open Question 1.

- **A node-level scope is read as a per-accelerator guarantee**, because it looks like one, and it is
  additionally unsound for shared accelerators (P4). → *Mitigation:* stated in F7, in the doc page and
  on the constant. No code prevents the misreading; a node label cannot carry the finer fact, which is
  why the Non-Goals point elsewhere for it.

- **A withheld label does not withhold anything unless the stale key is removed**, because the
  NodeFeature align path adds and overwrites and never deletes → the capable label reads `true` for
  as long as the object lives while the object's own inventory reports the link as broken, and the
  flavor keeps selecting the node. **This was found by building F7, not by reading it**, and it is
  the single point the whole gate rests on. → *Mitigation:* prefix-scoped removal in the align
  path, skipped when the enumeration failed, with the branch extracted to a seam and asserted in
  both directions. **Residual, stated:** the accelerator labels beside it have the same add-only
  behaviour and the same consequence for a card that is removed from a node. That is pre-existing
  and out of scope here; it is recorded so the asymmetry is a decision rather than an oversight.

- **The link check races the node's own network configuration** — a link brought up between two passes
  reads `failed` until the next one. → *Mitigation this round:* none in code; the detector's cadence is
  the answer and the first-seen time says when it started. The same eventual-consistency posture every
  other `Devices` fact has.

- **The create path's first-seen stamp has no test holding its call site in place.** The function
  that reports one pass needs the whole device manager to run, so no unit test reaches it; the
  contract it depends on is pinned, but its *presence* is not. → *Mitigation:* stated, not closed.
  The consequence is bounded and was measured rather than assumed: without the stamp the merge still
  converges, at the cost of one extra write and a first-seen time one pass late — and, in that
  window, an object publishing a failure with no beginning, which is what F5's "always" forbids. It
  is the same class of gap as everything else in that function, which is why it is recorded here
  rather than answered by extracting a seam that exists only to be asserted on.

- **A non-PCI interconnect is missed entirely** (P5) → the platform this repository most needs RDMA
  facts for reports no interfaces, and reports it as a healthy node with no RDMA. → *Mitigation:* F3
  enumerates interfaces first; the acceptance names the layouts tried in the reason so a miss is
  visible rather than silent. Not fully closable here: **the remote lab pair is one chassis, and P1's
  declared-table chassis is another that this round does not reach.**

- **A many-VF host produces a large object** on a cluster-scoped resource every consumer lists. →
  *Mitigation:* P9's nesting already collapses the top level to one entry per PF, and the acceptance
  run records the object size on a multi-VF host. A per-PF count instead of the nested list was
  considered and declined: a count cannot answer which VF is on which NUMA node.

- **The DaemonSet may not mount what the pass reads.** `/sys` is normally present, but an RDMA class
  tree on a node whose modules load late appears *after* our container started. → *Mitigation:* the
  read is per-pass rather than cached at startup, and F3's third state — enumerated-and-failed,
  which preserves the previous inventory instead of publishing an empty one — distinguishes a late
  module from an absent NIC.

## Design Details

### Commands

```bash
# Go, local (the whole module builds and tests on darwin)
make lint            # an edit pass; rewrites the module; do not give it a 2-minute timeout
make test

# after the API change
make generate        # must run on a module-suffixed physical path; failure is destructive

# on a node, no cluster needed
gpustack-operator device-manager detect
gpustack-operator device-manager preflight --manufacturer ascend
```

### Project Structure

```
api/worker/v1alpha1/devices.go              # DeviceTopology.PciSwitches; DevicesSpec.Interfaces;
                                            # DeviceInterface, nested VFs, link state
pkg/devicemanager/detector/
  network.go                                # the whole pass — enumeration, sysfs reads, PCI
                                            # coordinates, ordering — taking the sysfs root as a
                                            # parameter so all of it runs under fixtures
  network_linux.go                          # the real root; lands with the wiring that calls it
  network_other.go                          # stub; same
  network_test.go
  link.go                                   # the one link check, and the first-seen merge that
                                            # has to run before the align comparison
  interconnect.go                           # the grouping; the per-manufacturer driver reads that
                                            # feed it land in their own detectors
  detector.go                               # wire the pass in; the Spec.Interfaces align branch;
                                            # the pass instant both write paths stamp with
pkg/devicemanager/preflight/
  network.go                                # F8's rows, reusing the F5 checker
docs/architecture/device-discovery.md
docs/architecture/scheduling-chain.md
.claude/skills/gpustack-operator-e2e/cases/case-52.sh
```

Two of those paths sit outside the file surface this branch started with and are **in scope by an
explicit decision** — `pkg/device/helper.go` (T5's single construction point) and
`pkg/nodefeature/helper.go` (T10's label algebra); see [Decisions Taken](#decisions-taken). `binding/`
remains out: F5 needs no new entry point there, and a future check that does would have to regenerate
a tree that drifts several bindings at once.

### Implementation Plan

Nine tasks, plus three that are deferred or withdrawn and keep their numbers. Each declares the paths it owns;
that declaration is the only basis for isolation. **T8 and T9 are not work this round** — see
[Decisions Taken](#decisions-taken) for the reversal and what it answers. Their numbers are held
rather than reclaimed, so a reference to T8 made before this revision still names the same work.

| # | Task | Owns | Blocked by |
|---|---|---|---|
| T1 | API: `PciSwitches`, `DevicesSpec.Interfaces`, `DeviceInterface` with its VFs and link state; then `make generate`. **The distance levels are not an API type** — no field here carries one, so they live where they are computed (T2). An `InterconnectGroup` string was added here and withdrawn before merge with T6 | `api/worker/v1alpha1/devices.go`, generated outputs | — |
| T2 | The distance vocabulary's eight values and their numeric spacing, with the test pinning them against the product's existing table (P15) | `pkg/device/topology_distance.go` | T1 |
| T3 | The NIC pass: interface-first enumeration, multi-layout RDMA resolution, the capped/validated sysfs helper, `virtual`, SR-IOV nesting, deterministic ordering; table-driven tests over sysfs fixtures. **The sysfs root is a parameter**, which is what lets the whole pass run under fixtures; the platform entry points that supply the real root have no caller until T4 and land there | `pkg/devicemanager/detector/network*.go` | T1 |
| T4 | Wire the pass in — including the `network_{linux,other}.go` entry points that select the real root — and **add the `Spec.Interfaces` align branch**, with both the mutation-checked test and the idempotence test | `pkg/devicemanager/detector/detector.go`, `pkg/devicemanager/detector/network_{linux,other}.go` | T3 |
| T5 | Populate `PciSwitches` for all nine manufacturers from the `binding.PCIDevice` they already hold — one argument at each of the nine call sites, which is why they are owned here | `pkg/device/helper.go`, `pkg/devicemanager/detector/*/device.go` | T1 |
| ~~T6~~ | **Withdrawn — nothing of it landed.** The interconnect group: the connected component, the fabric identity for cross-node, the contradiction and the two cases either side of it. It was split in two because the grouping is platform-independent and covered by fixtures while the per-manufacturer driver reads that feed it can be verified nowhere but on the hardware. The second half was never written; the first was written, merged into no release, and then **removed** — a grouping with no production caller and a field empty on every node reads as live to the next contributor while its passing tests assert only fixtures. The design record is in F1 and the producer question is [Open Question 3](#3--which-manufacturers-get-an-interconnect-topology-provider-and-when) | — | — |
| T7 | The one link check and the three states, with first-seen times merged from the stored inventory **before** F4's comparison. **No manufacturer file is touched:** the check reads the RDMA subsystem's own port attributes, so it applies to every RDMA device and dispatches on nothing — the `ascend/device.go` this row first claimed needed no change at all | `pkg/devicemanager/detector/link.go`, and the call sites in `network.go` and `detector.go` | T1 |
| ~~T8~~ | **Deferred.** The declared override's file format and validation. The vendor-private product-form detector that once sat here is dropped outright, not deferred | — | — |
| ~~T9~~ | **Deferred.** The override's precedence against the derived distance, and the "absent declaration ≠ affects nothing" rule (P1) | — | — |
| T10 | The three labels, the distance from that vocabulary, the withhold on `failed` — **including the eight-key budget arithmetic in F7**, and the stale-key removal without which withholding a label does not withhold anything | `pkg/nodefeature/rdma.go`, `pkg/device/topology_distance.go`, the NodeFeature align path in `pkg/devicemanager/detector/detector.go` | T4, T7 |
| T11 | F8's preflight rows, reusing **T3's whole pass** rather than T7's checker alone — which is why the inventory entry point is exported here; plus the report's own section and the exit-code rule | `pkg/devicemanager/preflight/network.go`, `pkg/devicemanager/preflight/preflight.go`, `pkg/devicemanager/cmd.go`, the exported entry point in `detector/network_{linux,other}.go` | T7 |
| T12 | Docs (F9) and e2e `case-52.sh`: the inventory appears on `Devices`, survives a detector restart, is byte-identical across two passes, and the negative label case holds. **The withholding half of F5's gate has never been observed to fail on a real node** — it needs a link that is actually down, and on a node where nothing reports `failed` the refusing branch does not run at all, so the case records that row as a SKIP saying so rather than letting it vanish into the PASS count. Its ability to fail is established against fixtures only | `docs/**`, `.claude/skills/gpustack-operator-e2e/cases/case-52.sh` | T10 |

Case numbers start at 52: `cases/` holds 1–32 and 34–42 today (33 is retired and is not reused —
reusing a retired number desynchronises anyone reading the history), and 43–51 are taken by three
specs in flight. The 1–32/34–42 range is verified by listing the directory; **43–51 being taken is
taken on the orchestrator's word and was not verified here**, since those specs are not on `main`.

### Test Plan

Table-driven, one behaviour per case, fixtures through helpers, asserting observable final state.

| # | Subject | Shape | The mutation that must break it |
|---|---|---|---|
| 1 | sysfs → `DeviceInterface` | table over fixture trees: PF with VFs, a virtual interface, an RDMA-bound interface, an interface with **no PCI device**, an unreadable RDMA tree | drop the non-PCI case ⇒ that interface disappears |
| 2 | VF nesting | a PF with eight VFs | flatten to siblings ⇒ nine top-level entries |
| 3 | `virtual` classification | an interface with no device link | skip virtuals ⇒ empty list |
| 4 | failed read ≠ empty | the enumeration erroring | return `(nil, nil)` on error ⇒ reads as "no NICs" |
| 5 | sysfs read discipline | a symlink escaping `/sys`, and an oversized file | drop the resolved-path check ⇒ the escape is read |
| 6 | ordering / idempotence | two passes over one fixture with directory order shuffled | remove the sort ⇒ outputs differ |
| 7 | **the align branch** | `Groups` equal, `Interfaces` differing | delete the branch ⇒ `skip == true` |
| 8 | **the align converse** | nothing changed | unsorted input ⇒ `skip == false` |
| 9 | link states | table over fixture port trees: active+up, active with the physical link down, all down, a later port carrying the link, no ports directory, an unreadable port, and **one down port beside an unreadable one** | map unreadable → `failed` ⇒ the label is withheld on a missing file |
| 9b | the check reaches the record | enumeration over a fixture with one healthy HCA, one broken, one NIC with no RDMA | delete the call site ⇒ no interface carries a state, and case 9 stays green |
| 10 | first-seen stability | three passes at three instants with the failure persisting, then repaired, through the **real write path** against a client that counts writes | refresh the time each pass ⇒ the second pass writes |
| 11 | `rdma.distance` computation | fixtures for `PIX` / `PXB` / `NODE` / `SYS` and two shapes of `UNK` — the bus axis, which never yields `LINK`, `SELF` **or `PHB`** | collapse to a boolean ⇒ `PIX` and `PXB` become one |
| 11b | the withhold's mechanism | a stored capable key that this pass no longer reports | drop the stale-key removal ⇒ the label survives a link going down, and case 12 stays green |
| 11c | the label value's validity | a NUMA set spanning three nodes | join with a comma ⇒ the sanitizer silently publishes one fused number |
| 12 | the withhold | a `failed` node's label map | emit regardless ⇒ the key appears |
| 13 | preflight row ↔ record | one pass, two renderings | fork the pass ⇒ the two disagree |
| 13b | the section's own account | a failed enumeration, and a node whose interfaces carry no RDMA device | drop either note ⇒ an empty row list reads as a node that passed |
| 13c | the exit code | a `failed` row beside a passing accelerator answer | wire the row into the failure ⇒ a node that allocates fine is refused |

Case 9 needs no seam substitution and no driver handle: the check is a file read, so all three
states — including the positive one — are reachable under a fixture. That is a consequence of F5's
single check rather than a testing convenience; a per-manufacturer registry would have put the
positive states behind a concrete driver handle and left only "the read failed" checkable here.

**Case 10 asserts the number of writes, not the stored value.** In every way this defect can fail,
the object's content is correct — the inventory is recomputed accurately and written again. No
assertion on the value can see it, because after the fix the value is the same value. The write
count is the only observable that moves, so the test drives the real write path against a client
that counts, and asserts zero writes on an unchanged pass and one on a real change. The second half
matters as much as the first: three zero-write passes are evidence of a working comparison only
alongside a change that does write.

Every new test must be shown to fail before the code that makes it pass exists. A new test that is
green on its first run is a warning, not a result. Each is additionally checked by mutating the code
it covers, **with the prediction written before the run** — and an omitted prediction counts as a
mismatch exactly like a wrong one, since a partial prediction set is fitted after the fact for
whatever it left out.

## Alternatives

- **A separate per-node network-topology CRD.** Rejected: a node's NICs are part of what the Device
  Manager already reports about that node, and a second cluster-scoped per-node object doubles the
  ownerRef and GC surface `Devices` already had to get right once (the comment at `detector.go:479-482`
  records that lesson). Nothing is gained that a field on `DevicesSpec` does not give.

- **Store proximity as an explicit cross-reference** (each accelerator listing its aligned interfaces).
  Rejected: a third copy of a fact derivable from coordinates both sides carry, going stale
  independently of both. Both prior arts compare coordinates instead (P3), and one of them makes the
  required tightness the *requester's* choice, which a stored list would foreclose.

- **Publish a boolean "aligned / not aligned" instead of a level.** Rejected: `ComparePCIDevices`
  already distinguishes switch from root, so a boolean discards a level computed for free and leaves
  a consumer no way to ask for a looser or tighter bar (P2).

- **A bus-derived four-level scale (`switch` / `root` / `numa` / `node`).** Rejected: it cannot
  express a high-speed accelerator interconnect, and on hardware that has one the bus answer is
  *inverted* rather than coarse — a committed eight-accelerator sample reports every pair `LINK`
  while those accelerators span four NUMA nodes (P15). It would also put a second, coarser vocabulary
  for one question into a product that already answers it with eight values.

- **Reuse `pciRootId` as the switch identity**, as one prior art's field name suggests it does.
  Rejected: our own field is the outermost bridge (measured — see Proposal), which is neither a
  switch chain nor reliably a root complex, so the name overstates it in both directions. Repeating
  the conflation would advertise proximity nobody measured.

- **Extend `DeviceTopology.RoCE` instead of adding an interface inventory.** Rejected: `RoCE` is a
  property of an *accelerator's* port — the right shape for a platform where the port belongs to the
  card above the accelerator, the wrong shape for a NIC no accelerator owns. Both are needed and they
  are not the same object.

- **Take the whole prior-art attribute set verbatim, including addresses.** Deferred rather than
  rejected: addresses are the most volatile item on that list and nothing in the scheduling chain reads
  them today. `DeviceTopology.RoCE` already carries an address for the one manufacturer where it is a
  hardware fact rather than a lease.

- **Enumerate the PCI bus and correlate to interfaces.** Rejected on P5: it cannot see an interface on a non-PCI interconnect at all, and that is
  the platform this repository most needs RDMA facts for.

- **Ship the declared accelerator↔NIC table now** (P1). Deferred — see
  [Open Question 4](#4--what-shape-would-the-declared-acceleratornic-override-take). It needs a file
  format, precedence against the derived scope, and validation — and **inventing the format without
  that hardware in hand risks a format that does not fit it**. Weighing against it: the prior art's
  own format reserves an ordered fallback list that **nothing in its repository reads**, so copying
  the shape would copy unused capacity, and its loader degrades a missing table to "this NIC affects
  nothing" rather than to an error.

  One item that earlier appeared on that list is gone rather than deferred: a **vendor-private
  product-form detector**. That detector exists in the prior art to choose which of *its own* shipped
  tables applies to the chassis it is running on. We would not be shipping those tables — an operator
  authoring an override knows their own chassis — and "which node gets which content" already has an
  answer in Kubernetes, per-node configuration. Reading a manufacturer-private sysfs attribute to
  select a section of our file would be reinventing node selection, and would be the same kind of
  borrowing as the unread fallback list above.

- **Gate with a taint or a Node condition** instead of a withheld label. Rejected: the label chain
  already *is* the selection mechanism, so withholding needs no new machinery and cannot disagree with
  the flavor. A taint would be a second gate to keep in sync with the first.

## Decisions Taken

Each of these was a fork this spec could not resolve from the code alone. The option not taken is in
[Alternatives](#alternatives) with its reason.

- **`pkg/device/helper.go` and `pkg/nodefeature/helper.go` are both in scope.** So T5 extends
  `ConstructTopology` at the single construction point all nine detectors call, rather than editing
  nine call sites; and T10 puts the label algebra where `CLAUDE.md` says it lives, rather than
  splitting it across two packages. Both paths sit outside the file surface this branch started with
  and neither appears among the paths another window is changing.

- **The distance vocabulary is the product's existing eight-value one** (P15), covering the
  high-speed interconnect as its own level rather than deriving proximity from the bus.

- **Were the accelerator↔accelerator relation carried at all, it would be one group id per
  accelerator, not an N² matrix** (F1). The reduction is lossless at `LINK` level because a clique is
  all-to-all internally, and it keeps a cluster-scoped object O(N) on an eight-accelerator host
  instead of O(N²). This decision stands as a decision; the axis it was taken for was withdrawn from
  this round, so nothing acts on it yet.

- **The declared accelerator↔NIC override is NOT in scope.** An earlier revision of this list put it
  in, taking the plan to twelve tasks. That is reversed here, and the reversal names what it
  overturns rather than only stating a new conclusion — because the entry that pulled it into scope
  did not answer the reason it had been deferred for, which is how a spec ends up contradicting
  itself in three places at once.

  **The overturned reason, quoted from [Alternatives](#alternatives):** *"inventing the format
  without that hardware in hand risks a format that does not fit it"*. Answered point by point:

  1. **Still true, and this spec says so elsewhere.** [Verification](#verification) records that P1's
     declared-table chassis is a different machine which this round does not reach. The reason was
     never rebutted; it was passed over.
  2. **The artefact is less reversible than an API field, not more.** This repository's bar for a
     breaking change is "merged to `main` and not yet in a tag". An API field's old values sit in a
     cluster and can be reconciled or defaulted; an operator-authored **file format** sits on
     machines whose contents we never see. So the usual escape hatch does not apply to it at all.
  3. **A reduced version is not a middle ground.** Shipping a minimal declaration that does not copy
     the prior art's shape was considered and declined: it keeps the whole of the irreversibility and
     only reduces the effort. A compromise has to be on the cost axis, not the effort axis.

  T8 and T9 are therefore marked deferred in the plan and their numbers are not reused, so anything
  that already refers to them keeps referring to the same work. T10's dependency on T9 is removed;
  it was the only edge holding T10 through T12 behind them. What remains open is the format itself —
  [Open Question 4](#4--what-shape-would-the-declared-acceleratornic-override-take).

## Open Questions

### 1 — cross-node interconnect: half is designed in, half is open, and the split is the point

**What this spec *does* answer, by design (F1), so it is not deferred:** for the one manufacturer
that exposes a fabric identity, a **node-local read** yields a **cross-node** group —
`nvml.GetGpuFabricInfoV()` returns `{ClusterUuid, CliqueId, State, Status}`, and two accelerators on
two different nodes carrying the same `(ClusterUuid, CliqueId)` are in one interconnect domain. That
is the pre-design: the field is one field, the cross-node source simply wins over the node-local
component id when present, and **no scheduler is required to establish the identity**.

**What remains open, with the reason each is open rather than merely unwritten:**

- **Every other manufacturer.** The pairwise topology calls that exist in our bindings
  (`nvml.GetTopologyCommonAncestor`, `mtml.GetTopologyLevel`) are **node-local by construction** —
  they take two device handles on this machine. The NPU platform's own interconnect is read today
  only as a RoCE address triple, and `binding/dcmi` exposes no fabric or link entry point (verified:
  grep finds the address and gateway reads and nothing else). So for those platforms a cross-node
  group would have to be **assembled from facts published by several nodes**, which no per-node
  component can do.
- **Who assembles it.** That is a cluster-scoped join over every node's `Devices`, which is a
  controller's or a scheduler's job, not a DaemonSet's. This spec deliberately publishes the
  per-node half in a shape that join can consume (a group id, not a matrix) and does not attempt the
  join.
- **What the join would even key on, for a RoCE mesh.** Same subnet is not same fabric: two nodes on
  one subnet may be several switch hops apart, and hop distance is the fact a collective cares
  about. Answering it needs switch-level topology that no node can see from inside itself — a fabric
  manager, an LLDP/subnet-manager read, or an operator-supplied declaration. **Which of those is
  acceptable is a product decision, and it is the actual blocker**, not the code.

**Note for whoever picks this up:** one field cannot carry both sources without saying which one
filled it. On the fabric manufacturer the identity is genuinely cross-node; everywhere else the
group id is node-local and namespaced by node name. A consumer that joins across nodes over a field
holding both will match node-local ids that were never meant to be comparable, and silently group
accelerators that share nothing. Whichever shape replaces the withdrawn one has to make the source
readable from the value, and its acceptance has to assert both shapes rather than the fabric one.

### 2 — is `rdma.distance` worth publishing at all, given F7's two limits?

F7 establishes that of the three labels only `rdma.capable` can enter a flavor selector, because the
selector is equality-matched and capped at eight keys with five to six already spoken for. That makes
`rdma.distance` and `rdma.numa` node annotations in all but name — readable by a human or by a future
controller, but unable to influence admission through the chain this repository has.

They are cheap and honest, so this spec publishes them. But **if the answer is that the eventual
consumer is a scheduler plugin reading `Devices` directly** (which is where an interconnect group
would have had to go), then these two labels are a surface with no future consumer and would be
better never introduced than deprecated later. **Not answerable by me:** it depends on which consumer gets
built, and that decision is the subject of the allocation-time work named in the Non-Goals.

### 3 — which manufacturers get an interconnect topology provider, and when

F1's grouping is platform-independent and was covered by fixtures. What feeds it — the driver calls
that answer whether two accelerators are peers — is per-manufacturer, and **this round implements
none of them.** T6's second half was planned for NVIDIA and Moore Threads and was not written:
neither could have been exercised anywhere but on the hardware, and neither machine was available —
the NVIDIA cards on hand carry no NVLink at all (it was removed from that consumer generation), and
no machine with the Moore Threads part was reachable.

Landing an unexercisable provider was one alternative, and it is the weaker one: it would put a
driver call nobody has ever run into the detector's hot path, where its failure mode is a per-pass
error on every node carrying that manufacturer's card.

**Keeping the consumer-side half without a producer was the other, and it is what this round first
did and then reversed.** The field and the grouping were merged into this branch, passed 19 fixture
assertions, and were removed before the branch merged. The reason is worth more than the outcome: a
path with full green tests and no production caller **reads as live**, and the tests are the most
deceptive part of it, because they are evidence about fixtures presented in the same form as
evidence about behavior. Nothing in the record distinguished "this grouping works" from "this
grouping has never been asked anything by production code". Retaining it would also have bought
nothing for the eventual implementation, since the shape of the per-manufacturer read is what is
unknown. Whoever lands the first provider should re-derive the shape from that read rather than fit
it to what the grouping happened to need — one string id per accelerator was the grouping's
requirement, not a consumer's.

Two more are within reach and are **deferred rather than declined** — this round does not do them,
and nothing about them is wrong. The pairwise call already exists in the bindings for both, so each
is a provider and not a new binding: AMD exposes a per-pair link type on its device handle, and
Ascend a per-pair topology query on its own. A machine with eight all-to-all AMD accelerators would
give the grouping end-to-end evidence on real hardware.

**Why that is not simply an improvement, which is the part worth recording:** implementing one of
them would change this spec's own statement about F1 from a universal — *this half has not been
verified on hardware* — into a partial negative, and a partial negative reads as "the grouping has
hardware evidence" while withholding the granularity that only one provider's path was ever
executed. A check whose granularity is right at one level can say nothing at a finer one. So the
choice here is between two statements, not between coverage and no coverage, and the honest one is
the universal until every provider is covered.

**The API-compatibility argument that the withdrawal also settles, recorded because it is the one
the retain-it option could not answer.** `api/worker/v1.Devices` aliases `workercore.Devices`, so a
field added to `DeviceTopology` appears on the read-only aggregated `v1` surface at the same moment
it appears on `v1alpha1`. The withdrawn field's own comment stated that its shape might still change
breakingly — a `v1` field promising breaking changes, which no wording on the field could reconcile.
Deferring the field removes the contradiction outright; nothing short of that did. Whoever adds the
axis back inherits the same constraint: on this API, `v1alpha1` is not a private staging area.

### 4 — what shape would the declared accelerator↔NIC override take

P1 establishes that on one class of production hardware the accelerator↔NIC relation is **authored,
not measured**: it ships as a table keyed by chassis model. Our derivation answers a different
question there, and reports `NODE` on a chassis whose operators consider the pairing settled. That
residual is recorded in [Risks and Mitigations](#risks-and-mitigations) and is not closed here.

An override was in scope for one revision of this spec and is [reversed](#decisions-taken). What is
open is not *whether* to have one — it is the part that made the reversal correct:

- **The format.** It is operator-authored, so it is less reversible than an API field: an API field's
  old values are in a cluster and can be reconciled, while a file lives on machines whose contents we
  never see. And the chassis it must fit is not reachable, so the format would be designed against a
  description of hardware rather than against hardware.
- **What it may not copy.** Two shapes in the prior art must not be inherited without a consumer of
  our own: the ordered fallback list per accelerator, which nothing in that repository reads, and the
  degradation of a missing table to "this NIC affects nothing" — an absent declaration must be
  distinguishable from a declaration of none, which is the whole of P1's warning.
- **What is already settled and needs no further question:** the vendor-private product-form detector
  is declined outright rather than deferred, for the reason in [Alternatives](#alternatives) — it
  would reinvent node selection.

**Answerable only with that chassis in hand**, which is why it is a question and not a task.

### 5 — the accelerator side cannot say "NUMA unknown", so one distance can still overclaim

`BusDistance` refuses to read an unknown NUMA node as "the same node", because treating it as node 0
would assert an affinity the kernel denied. **That guard only covers one of the two sides.**

This feature's interface reader maps the kernel's `-1` sentinel to empty, so an interface with no
affinity arrives as unknown. The accelerator side cannot express unknown at all:

```go
// binding/helper_linux.go — getNumaNodeByBDF
n := safeInt(s, -1)
if n > 0 { return strconv.Itoa(n) }
return "0"
```

**Four different readings reach that `"0"`**: the kernel's `-1` for "no affinity"; a genuine node 0;
a `numa_node` whose contents will not parse, because `safeInt`'s default is the same `-1`; and any
negative value. By the time a topology reaches the comparison they are one value.

- **Node 0 is correct here by COINCIDENCE.** The test is `n > 0`, not `n >= 0`, so a genuine node 0
  falls through to the same fallback as every failure. Anyone changing that fallback to `""` — which
  is what the exported doc comment already claims it does — would break node 0 along with the
  unknowns, and nothing in the package would object.
- **The exported contract is already false.** `binding.GetNumaNodeByBDF`'s doc says it returns "an
  empty string if the BDF is invalid **or if there is no associated NUMA node**". The first half
  holds; the second does not, and it is the half a caller reasons about.
- **The non-Linux stub returns `"0"` unconditionally**, so on a development host every device reads
  as being on node 0. A topology conclusion reached in a local test therefore differs from the same
  conclusion on Linux, and neither side reports anything.
- The shape is the one recorded elsewhere as its own hazard: **a zero value doubling as a sentinel.**
  Here the collision is silent in both directions — the value is valid, and the failure it hides is
  a *proximity claim*.

The harm has a direction, and it is the dangerous one: an accelerator with no established affinity
compared against an interface genuinely on node 0 yields `NODE`, so `rdma.distance` **overclaims**.
A node that does not meet a strict topology requirement is published as one that does.

**No change inside this feature can recover it** — the information is gone before the call. The fix
is to make that helper report unknown as unknown, which changes the published `numaAffinity` of
every accelerator on all nine manufacturers and therefore every consumer of `Topology`, well beyond
a NIC inventory. Even the comment correction is deliberately excluded: it carries no behaviour risk,
which makes it tempting, but it would pull a `binding/` file into a NIC inventory's diff and the
review of that file is about the nine manufacturers rather than about this. Both belong in a change
of their own. The comparison's own comment states the asymmetry, so the next reader of either does
not have to rediscover it.

**Not answerable by measurement on this feature's surface**: whether the helper may change is a
question about who owns that surface and what depends on its current answer.
