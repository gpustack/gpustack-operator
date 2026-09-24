# Network Topology

> **Purpose** — what the Device Manager records about the node's network interfaces and its
> accelerators' scale-up fabric, how an RDMA link is verified, and which of those facts can reach a
> scheduling decision.
> **Audience** contributors touching `pkg/devicemanager/detector`, operators debugging a missing
> `rdma.capable` · **Prerequisites** [Device Discovery](device-discovery.md) · **Read time** ~10 min

## Contents

- [The interface inventory](#the-interface-inventory)
- [`pciRootId` is the outermost bridge; `pciSwitches` is the tighter fact](#pcirootid-is-the-outermost-bridge-pciswitches-is-the-tighter-fact)
- [The RDMA link is checked, because a bound device is not a working link](#the-rdma-link-is-checked-because-a-bound-device-is-not-a-working-link)
- [The three node labels, and what a label can carry](#the-three-node-labels-and-what-a-label-can-carry)
- [The RDMA resource keys, and what each endpoint serves](#the-rdma-resource-keys-and-what-each-endpoint-serves)
- [What an allocation hands over](#what-an-allocation-hands-over)
- [The scale-up fabric is a second network, and a different shape](#the-scale-up-fabric-is-a-second-network-and-a-different-shape)
- [Reading it yourself](#reading-it-yourself)

## The interface inventory

`Devices.spec.interfaces` holds one entry per kernel interface: its bus, PCI coordinates, NUMA and
CPU affinity, MTU, whether it is up, whether it is virtual, the RDMA device bound to it if any, and
its SR-IOV virtual functions **nested under their physical function** so one card appears once.

**Enumeration starts at the interface, not at the PCI bus.** `/sys/class/net` is the list, and each
interface's PCI device is resolved as an *attribute* of it. The inverse — walking the bus and
correlating back — cannot see an interface that is not a PCI device at all, and that is the case
the inventory most needs: on some accelerator platforms the RDMA-capable port is not on the bus.

Such an interface reports its own bus in `bus` and leaves every PCI field empty, so the absence
reads as a **kind** of interface rather than as a failed lookup.

**Every sysfs read is capped, and its resolved path is validated.** sysfs is a forest of symlinks,
so following them is unavoidable — which makes checking where you landed unavoidable too. A path
resolving outside the tree root is refused, and one attribute read is capped at 64 KiB.

A failure to enumerate is **not** an empty inventory. An empty list would claim the node has no
interfaces, which a failed read cannot support, so the recorded list is kept and the failure is
logged at `Error`. A pass that enumerated and found none *does* write the empty list.

> **Why the published list is sorted** — the detector decides whether to write by comparing against
> the live object, and sysfs directory order is not guaranteed stable. An unsorted list would
> compare unequal on passes where nothing changed and issue an API write on every pass, forever,
> with correct data in the object the whole time.

## `pciRootId` is the outermost bridge; `pciSwitches` is the tighter fact

`DeviceTopology.pciRootId` holds the **address of the outermost PCI bridge** above the device. It is
not the root complex, despite the name, and reading it as one advertises closeness nobody measured.

`pciSwitches` is the full upstream bridge path, innermost first. Two devices sharing the whole path
sit behind the same switch, which is strictly tighter than sharing the outermost bridge. The two
fields answer different questions and are never read as the same claim.

> **Why one implementation, not two** — the interface side and all nine accelerator detectors derive
> these coordinates from the same walk. The values are only useful when they compare **equal**
> across the two sides, so sharing the code makes them identical by construction rather than by
> discipline.

## The RDMA link is checked, because a bound device is not a working link

`rdma: true` says an RDMA device is bound. It does not say the link works, and on real hardware the
two differ: a port can be fully configured, with an address and a gateway, while its link is down.

Each RDMA device's ports are read for their transport state and their physical link state:

| state | meaning | does this interface count as usable? |
|---|---|---|
| `ok` | some port is active with the physical link up | yes |
| `unverified` | the check ran and could not establish an answer | yes, and the reason says why |
| `failed` | every port was read and none carried the link | **no** |

A port's state is read as the whole enum name, not as a substring: `ACTIVE_DEFER` is a port that
lost its link and is not carrying traffic, so accepting it as `ACTIVE` would publish the label over
a link nothing can use.

**The node label aggregates existentially: `rdma.capable` is emitted when AT LEAST ONE endpoint is
usable, not when every one is.**

An endpoint is every interface and every virtual function, and it is usable when its verdict is
anything other than `failed` — falling back to whether a device is bound only when there is no
verdict at all.

So an explicit verdict outranks the `rdma` flag in both directions: an unreadable-tree record
carries `rdma: false` with an `unverified` verdict and **is** usable, while a bound device whose
verdict is `failed` is not. Only a node where every endpoint is unusable loses the key, which is
stricter than "every bound interface reports `failed`".

A node with a broken NIC beside a working one keeps the label, because it can still serve an RDMA
workload. Withholding there would let an unplugged second card take a working node out of
scheduling — the same error as withholding on an unreadable file. A consumer that needs to know
which interface is broken reads `Devices`; a node label cannot carry it.

An RDMA device bound to an **SR-IOV virtual function** counts the same way. A VF is nested under its
physical function rather than listed at top level, so a node whose only RDMA devices are VFs would
otherwise report none.

A VF is its own PCI function with its own address. What it shares with the parent is the **upstream
bridge path**, which is what the distance is computed from — so inheriting the parent's `pciRootId`
and `pciSwitches` claims nothing extra. Its NUMA node is its own, and is published as such.

**`failed` is never reached from a file that could not be read.** One unreadable port beside several
down ones leaves "all ports are down" unestablished, so that mixture is `unverified`. An inability
to ask must not read as an answer of no, because withholding the label removes the node from
scheduling.

A `failed` result carries the port values verbatim and **the time the failure was first seen**,
stable for as long as it persists so an operator can answer "how long has this been broken?". That
time is merged from what is already recorded *before* the inventory is compared — taking the clock
each pass would make the comparison never match and rewrite the object forever.

The check is not per-manufacturer: it reads the RDMA subsystem's own port attributes, so it applies
to every RDMA device there is. `preflight` runs this same pass rather than reimplementing it, which
makes the two readings interpret the host identically — not identical to each other, since
`preflight` reads sysfs when it is invoked and the published record is as old as the last pass that
had a reason to run. See [Preflight operations](../operation/preflight.md).

## The three node labels, and what a label can carry

| label | value | in a flavor selector? |
|---|---|---|
| `feature.gpustack.ai/rdma.capable` | `true` | **yes** — the one key the link gate needs |
| `feature.gpustack.ai/rdma.distance` | the closest bus distance any accelerator has to an RDMA-capable interface | no — informational |
| `feature.gpustack.ai/rdma.numa` | the NUMA nodes carrying one, joined with `_` | no — informational |

Only the gate key is unconditional. **`rdma.distance` is omitted on a node with no accelerator**,
because a distance is a statement about a pair and there is none to measure — not an unknown one.
**`rdma.numa` is omitted when the kernel gave no affinity** for any capable interface: publishing a
member nobody read would assert a NUMA node, and normalising the blank to `0` would assert the one
the kernel declined to give.

The distance vocabulary is the product's existing one — `SELF` `LINK` `PIX` `PXB` `PHB` `NODE`
`SYS` `UNK`, smaller being closer. Three of its levels are never produced from bus coordinates:
`LINK` is not derivable from a bus reading at all, `SELF` would mean an accelerator is the same
device as a NIC, and `PHB` cannot be told from `NODE` without knowing whether the two share a PCIe
host bridge — the coordinates stop at the outermost bridge.

Where two levels are indistinguishable the **further** is reported, because the value feeds a
proximity claim and overclaiming closeness is the error nothing downstream can catch. A missing
coordinate yields `UNK`, never a distance.

**Withholding a label is implemented as removing it.** The NodeFeature the DM writes is otherwise
add-only, so a key that stops being reported would not stop existing — the label would read `true`
for as long as the object lived while the same pass's inventory reported the link as broken. The
removal is scoped to the `rdma.` prefix and is skipped entirely when the enumeration failed:
deleting on a failed read makes the same unsupported claim as publishing an empty inventory on one.

> **Every label here is node-level, so each loses information on purpose.** "*Some* accelerator is
> `PIX` from *some* NIC" is not "the accelerators this workload gets will be", and per-accelerator
> distance cannot be expressed in a node label at all. A node-level proximity assertion is only
> sound for a request holding its accelerator **exclusively**: for a sliced or shared one, the
> fraction you are given need not be the close one. A consumer needing the finer answer reads
> `Devices`, which carries the per-interface truth.

The `rdma.numa` set is joined with an **underscore**, not a comma: a comma is not a valid label value
character and it does not fail validation — the sanitizer every label value passes through drops it
silently, so `{0,1}` would publish as `01` and read as node 01.

## The RDMA resource keys, and what each endpoint serves

The facts above make an RDMA endpoint visible. Three device-plugin resources make one allocatable:
the interface inventory decides which endpoints each key serves, the link verdict decides their
health, and every token carries its endpoint's NUMA affinity.

The keys, named by `GetRDMAResourceName` (`pkg/nodefeature/rdma.go`), are node-level and carry no
manufacturer — a network interface belongs to the node rather than to a vendor:

| key | allocation mode | `1` means | tokens per endpoint |
|---|---|---|---|
| `device.gpustack.ai/rdma` | exclusive | one whole interface | 1 |
| `device.gpustack.ai/rdma.shared` | shared | one concurrent use of one interface | 64 |
| `device.gpustack.ai/rdma.partitioned` | partitioned | one SR-IOV virtual function | 1 per virtual function |

EFA is allocated under none of these keys. AWS's EFA device plugin advertises its own
`vpc.amazonaws.com/efa` key, and an EFA allocation follows that plugin's capacity rather than
`Devices.spec.interfaces[]`.

The mode is **read off the node, never chosen** (`pkg/deviceplugin/rdma_endpoint.go`):

| the interface is… | it serves | its endpoints are |
|---|---|---|
| an SR-IOV physical function with virtual functions configured | `partitioned` only | each of its virtual functions |
| anything else — a physical function with none configured, or not a physical function | `exclusive`, `shared` | the interface itself |

A physical function with virtual functions configured does not also serve the whole-function modes:
the node was put into that state before the Device Manager started, and nothing here can change it
at run time. Being a physical function and having virtual functions configured are separate facts
in the record, and both are read — either alone sends a record the other distinguishes into the
wrong branch.

An endpoint with no bound RDMA device name is advertised in no mode at all: the name is what an
allocation resolves to a character device, so an endpoint without one has nothing to hand over. A
node whose RDMA tree exists but could not be read produces exactly that shape — an `unverified`
record with no device — and advertises zero.

The link verdict above gates health, not existence (`pkg/deviceplugin/rdma_server.go`): `ok`,
`unverified` and no record at all advertise `Healthy`; `failed` advertises the endpoint's tokens
`Unhealthy`. No verdict is not a verdict of failure — an endpoint reaches this gate only by carrying
a bound device, so a missing link record is the `unverified` case arriving by a different route.

**A `failed` endpoint keeps its tokens rather than dropping them.** The kubelet checkpoints the
exact device IDs it offered a container, and withdrawing an advertised ID strands that checkpoint.
The holder keeps its allocation while new Pods are granted none — and a container that restarts
while the link is down cannot re-establish its allocation, because the checkpoint check requires
every offered ID still healthy. It stays stuck until the link heals.

Every advertised token carries a `TopologyInfo` naming its endpoint's own NUMA node. A virtual
function falls back to its parent's affinity when its own is blank — a blank reading is the kernel
declining to answer — and when neither answers, **no hint is attached rather than one naming node
0**: under `single-numa-node` the hint decides admission, so a stand-in value would place a
container against a proximity nobody measured.

A hint's unit is the NUMA node; a shared PCIe switch is finer than a hint can express (see
[`pciRootId` above](#pcirootid-is-the-outermost-bridge-pciswitches-is-the-tighter-fact)).

The `partitioned` family is the one place an RDMA token publishes a hint where its accelerator
counterpart publishes none: an RDMA partition token names exactly one virtual function, whose
affinity a hint can honor, while an accelerator partition token names no accelerator at all. What
that means for a request pairing the two is stated with the request rules
([Accelerator Requests](../accelerator-requests.md#co-locating-an-accelerator-and-an-rdma-interface)).

> **Why** one interface serves many shared holders — its multiple queue pairs are how the hardware
> is meant to be used, with the isolation done by firmware and kernel, so several processes on one
> is ordinary use rather than oversubscription. The count is a scheduling knob, not a hardware
> limit, and nothing is injected or intercepted to enforce it.
>
> It matches the default the ecosystem's own shared-RDMA device plugin ships, so a workload written
> against that plugin meets no tighter limit here. There is one pooled key rather than two because
> two would draw on the same interfaces without decrementing each other, leaving neither count a
> ceiling.

Nothing an allocation does is written down: no entry in `Devices.status`, no Pod annotation, no
in-process reservation. Every response is recomputed from `Devices.spec.interfaces[]`, so a fact
the detector has since corrected cannot survive into a response built from an older one. The cost
is that no cluster-level view maps a Pod to the interface it holds — the container itself is the
record, through its injected device nodes and its `NCCL_IB_HCA` value.

One server per mode registers its key with kubelet, started once by `Allocator.Start` beside the
per-manufacturer allocators rather than by the detected-manufacturer loop, which is keyed on a fact
a network interface does not have (`pkg/devicemanager/allocator/rdma/`). Every node the Device
Manager serves on Linux runs the servers, accelerator or not, so a node with no RDMA-capable
interface registers all three keys with zero devices.

Zero advertisement is level-based, not an absence: a `ListAndWatch` re-reads the inventory, so a
device that appears when a driver loads is picked up by the next pass with no second mechanism for
the same fact. The `--no-shared` and `--no-partitioned` switches drop the matching
families; exclusive is ungated.

## What an allocation hands over

A granted token is resolved back to its endpoint in `Devices.spec.interfaces[]` at allocation
time, and the response hands the container three things
(`pkg/deviceplugin/rdma_allocate.go`):

- the endpoint's own verbs character device, resolved from its RDMA device name;
- the node-level connection-manager device (`rdma_cm`), once per response however many endpoints
  were granted, and only where the host has one;
- `NCCL_IB_HCA`, naming the granted RDMA devices, comma-joined.

The verbs device is resolved through two sysfs layouts, in order
(`pkg/deviceplugin/rdma_devices.go`): `class/infiniband_verbs/uverbsN` matched by its `ibdev`
attribute, then the `infiniband_verbs` directory under the RDMA device's own hardware parent.

> **Why that second path** — a class device sits at `<parent>/<class>/<name>`, never directly under
> the parent, the same rule that puts the RDMA device at `<parent>/infiniband/<name>`.

A name that resolves under neither fails the allocation naming both layouts — a single hard-coded
path that is wrong on one distribution fails exactly like a host that has no RDMA at all.

Devices are injected one endpoint at a time, never the whole `/dev/infiniband` directory: injecting
a directory hands every container every adapter on the node, and degrades to handing it none with
no error. A granted token that parses to no endpoint of the family is refused rather than silently
dropped.

Two limits, stated rather than omitted. The injected set is evidenced for RoCE and for nothing
else — no reading behind this repository says what a classic InfiniBand fabric additionally needs,
so a container on such a host can be granted an endpoint, open it, and still fail inside its own
transport library. And the two layouts above are exercised against fixture trees; which one a live
host answers through has not been read yet.

## The scale-up fabric is a second network, and a different shape

Everything above is the node's *Ethernet* view: interfaces the kernel enumerates, and RDMA over them.
Beside it, several accelerator generations carry a **scale-up fabric** of their own — an interconnect
over which accelerators address one another's memory directly, and which on the newest generations
**spans machines**. That fabric is invisible to the interface inventory: it has no kernel interface,
no PCI function of its own, and no IP.

It is recorded per accelerator, in `spec.groups[].accelerators[].topology.fabric`:

| Field | What it is |
|---|---|
| `kind` | the interconnect — `ub`, `nvlink` or `xgmi`. Part of every comparison: two ids from different interconnects share no namespace |
| `id` | the domain's identity, **comparable across nodes** — which is the only reason publishing it is worth anything |
| `type` | the domain's shape as a word (`pod-1d`, `server-8p`, `card-4p`) |
| `cliqueId` | the subset that can actually address one another, where the domain is partitioned |
| `memberCount` | how many members the domain reports |
| `nodeIndex`, `rackId` | where this machine sits inside the domain, as the domain numbers them |
| `endpoints` | this accelerator's own addresses on the fabric |

**Four of these are published only for a super pod.** `id`, `memberCount`, `nodeIndex` and `rackId`
are withheld on an Ascend node that is in none — and the driver answers the super pod query even
there, since that answer is what establishes the shape, so the shape alone does not decide it. The
rule is the vendor's own:

- the two pod shapes are in a super pod;
- a `server-8p`, `card-1p` or `card-4p` is in one exactly when the driver reports an id and a size
  that are not its invalid markers — the "super server", whose id the vendor's own rank table spans
  machines with;
- a `server-16p` or `server-32p` never is.

Membership is established by `id` alone, so each of the other three carries its own check on top of
that: a domain can be identified while its size, this machine's index in it, or its rack are not.
`kind`, `type` and `endpoints` travel either way. Publishing an invalid marker would hand one domain
to every unrelated machine carrying it — or sort this one as the pod's four-billionth member — while
reading the shape alone would cost a real super server the domain it is in.

> **Why per accelerator, when a domain plainly spans machines** — because it also **cross-cuts** a
> machine. NVIDIA reports a clique per GPU and one node can hold several; AMD's hive id is likewise
> read per GPU. A node- or group-level field would flatten that. The domain itself is a **derived
> aggregation** — the accelerators sharing `kind` and `id` — which is the same rule the interface
> inventory follows: publish comparable coordinates, never a stored cross-reference.

**What each manufacturer publishes differs, and absence is not uniform.**

| Manufacturer | Concept | What it reports | Recorded today |
|---|---|---|---|
| Ascend A5 (950) | super pod, over the UB fabric | domain id, shape, member count, server index, rack, per-accelerator endpoints | Yes |
| NVIDIA | NVLink / MNNVL domain | fabric cluster uuid, clique id | Yes — once the fabric manager has registered the accelerator, and not before |
| AMD | XGMI hive | hive id | No — the record is designed for it, the detector does not fill it |
| Cambricon | MLU-Link | **no domain identity at all**, only per-link remote information | No — it describes an *edge list*, which this record cannot hold |

**On NVIDIA the record appears only after registration completes.** The driver reports a registration
state beside the two identifiers, and the fabric manager is what fills those in. A generation with no
fabric, one that has not started registering and one still negotiating have no cluster assigned yet;
an ordinary card answers the query with the not-supported state and zeros. Publishing that would give
every machine in that state one shared domain — and this id is compared across machines.

A registration that completed with an error, and an all-zero cluster uuid, are refused on the same
grounds: neither identifies a cluster.

**RoCE is unreadable on Ascend A5, by construction.** `topology.roce` is always absent there, and that
is not a gap to fill: the dcmi V2 API that generation serves declares no device-IP and no
device-gateway entry point, so the question cannot be asked. Its cross-machine addressing lives in
the `endpoints` above instead.

> **Why `endpoints` are published unparsed** — each is a UB endpoint identifier as 32 lowercase hex
> characters, and every derived field a consumer wants (the function entity, the die, the port,
> whether the endpoint carries device-to-device traffic) is a bit field *inside* those bytes. Parsing
> them here would mean tracking a vendor bit layout that only the vendor may change, in an API that
> cannot change with it. A consumer assembling a communication plan derives what it needs; this
> record's job is to make sure it has the bytes to derive it from.

**Two node labels**, alongside the three above:

| Label | Value | For |
|---|---|---|
| `feature.gpustack.ai/fabric.domain` | `<kind>-<id>`, and `<kind>-<id>-<clique>` where the manufacturer partitions a domain — `ub-7`, `nvlink-<32 hex>-1` | a selector pinning "same domain" |
| `feature.gpustack.ai/fabric.members` | the member count | informational |

The kind is inside the **value**, not just the key, because a bare `7` could name an Ascend super pod
and an AMD hive alike. `fabric.domain` is withheld unless **every** accelerator on the node reports
the same domain: a node whose accelerators sit in different domains has no single answer, and
publishing one of them would advertise co-location the hardware does not offer.

The clique is in the value for that same reason one level down. Two NVIDIA accelerators sharing a
cluster uuid but not the clique inside it are on one fabric and still cannot address each other, so
naming the cluster alone would promise co-location to the nodes on the far side of a partition —
which is exactly where this value is compared. A manufacturer reporting no clique keeps the two-part
form.

`fabric.members` needs that and a nonzero count every accelerator agrees on, so it can be absent
where `fabric.domain` is present: a manufacturer that names a domain without sizing it, or two cards
in one domain reporting different sizes, publishes the domain alone rather than a count that depends
on which card was read first.

Like `rdma.capable`, a key that stops being reported is **removed**, so a node taken out of its super
pod loses the label rather than keeping a domain it has left.

That removal is what forces the reduction over the **whole node** rather than one detect pass. A
mixed-vendor node has one device-manager DaemonSet per manufacturer writing this one object, so a
per-pass answer would claim the GPU is in the NPU's super pod, and the two passes would delete each
other's key forever. A pass that cannot read the node's other manufacturers withholds nothing and
leaves the labels as published.

## Reading it yourself

```bash
# the fabric domain of every accelerator on one node
kubectl get devices <node> -o json |
  jq '[.spec.groups[].accelerators[] | {id, fabric: .topology.fabric}]'

# which nodes are in one Ascend super pod, and which are in one NVLink clique
kubectl get nodes -l feature.gpustack.ai/fabric.domain=ub-7
kubectl get nodes -l feature.gpustack.ai/fabric.domain=nvlink-5b0e112233445566778899aabbccddef-1

# the inventory and every link verdict, for one node
kubectl get devices <node> -o jsonpath='{.spec.interfaces}' | jq

# every RDMA endpoint and its link state — nested virtual functions included, and selected by
# "bound OR carrying a verdict", because an unreadable tree records `rdma: false` with a verdict
# and still counts toward the label
kubectl get devices <node> -o json |
  jq '[.spec.interfaces[] | (., (.virtualFunctions // [])[])]
      | map(select(.rdma or .link)) | map({name, pciBusId, rdmaDevice, link})'

# the three RDMA resource keys and their healthy-token counts on one node — partitioned counts
# virtual functions; shared counts 64 per whole-function endpoint
kubectl get node <node> -o json |
  jq '.status.allocatable | with_entries(select(.key | contains("gpustack.ai/rdma")))'

# which nodes a flavor pinning the gate would select
kubectl get nodes -l feature.gpustack.ai/rdma.capable=true
```

A node whose link is broken carries the state in `Devices` and **not** the label, which is the pair
to check when a flavor stops selecting a node that still has the hardware.

---

**See also** — [Device Discovery](device-discovery.md) (the accelerator side of the same ledger) ·
[Scheduling Chain](scheduling-chain.md) (how a node label reaches a flavor selector) ·
[Accelerator Requests](../accelerator-requests.md) (the request rules the RDMA keys obey) ·
[RDMA Operations](../operation/rdma.md) (how many endpoints a workload asks for, and the kubelet
policy that aligns them with its accelerators) ·
[Preflight Operations](../operation/preflight.md) (the same link check, before anything is installed)

**Next** → [Scheduling Chain](scheduling-chain.md) — how these labels become ResourceFlavors.
