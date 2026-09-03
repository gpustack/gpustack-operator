# Network Topology

> **Purpose** — what the Device Manager records about the node's network interfaces, how an RDMA
> link is verified, and which of those facts can reach a scheduling decision.
> **Audience** contributors touching `pkg/devicemanager/detector`, operators debugging a missing
> `rdma.capable` · **Prerequisites** [Device Discovery](device-discovery.md) · **Read time** ~7 min

## Contents

- [The interface inventory](#the-interface-inventory)
- [`pciRootId` is the outermost bridge; `pciSwitches` is the tighter fact](#pcirootid-is-the-outermost-bridge-pciswitches-is-the-tighter-fact)
- [The RDMA link is checked, because a bound device is not a working link](#the-rdma-link-is-checked-because-a-bound-device-is-not-a-working-link)
- [The three node labels, and what a label can carry](#the-three-node-labels-and-what-a-label-can-carry)
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

## Reading it yourself

```bash
# the inventory and every link verdict, for one node
kubectl get devices <node> -o jsonpath='{.spec.interfaces}' | jq

# every RDMA endpoint and its link state — nested virtual functions included, and selected by
# "bound OR carrying a verdict", because an unreadable tree records `rdma: false` with a verdict
# and still counts toward the label
kubectl get devices <node> -o json |
  jq '[.spec.interfaces[] | (., (.virtualFunctions // [])[])]
      | map(select(.rdma or .link)) | map({name, pciBusId, rdmaDevice, link})'

# which nodes a flavor pinning the gate would select
kubectl get nodes -l feature.gpustack.ai/rdma.capable=true
```

A node whose link is broken carries the state in `Devices` and **not** the label, which is the pair
to check when a flavor stops selecting a node that still has the hardware.

---

**See also** — [Device Discovery](device-discovery.md) (the accelerator side of the same ledger) ·
[Scheduling Chain](scheduling-chain.md) (how a node label reaches a flavor selector) ·
[Preflight Operations](../operation/preflight.md) (the same link check, before anything is installed)

**Next** → [Scheduling Chain](scheduling-chain.md) — how these labels become ResourceFlavors.
