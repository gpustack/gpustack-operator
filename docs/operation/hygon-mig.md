# Hygon MIG Operations

> **Purpose** — the administrator runbook for Hygon DCU partitioning *mode* (enable, disable, reboot
> recovery) and the user contract for requesting a partition, mirroring [NVIDIA MIG
> Operations](nvidia-mig.md) and [T-Head MIG Operations](thead-mig.md).
> **Audience** operators, users requesting partitions · **Prerequisites** [Accelerator
> Requests](../accelerator-requests.md) · **Read time** ~10 min

Partitioning *mode* is a **manually managed** node property, as for NVIDIA and T-Head: the operator
*observes* it through the Device Manager and reflects it in the `Devices` ledger and the advertised
capability; it never enables, disables or reconfigures it. With the mode on, it does **carve** the
GPU/Compute Instance pair backing a scheduled workload, and destroys it when the Pod exits.

Hygon names the feature as NVIDIA does — its management library exports NVML's own symbol names, and
`hy-smi mig` mirrors `nvidia-smi mig` flag for flag — so the resource-key segment is the same word,
`mig` (see [Accelerator Requests](../accelerator-requests.md#two-families-two-accelerator-populations)).
What all three share is linked below, not restated.

**Two things are different here, and both change what an operator can do.** The mode is a property of
the **node**, not of a card; and a partitioned node serves **only** partitions. Read
[Limitations](#limitations) before planning a rollout.

## Contents

- [Prerequisites](#prerequisites)
- [How partition profiles are discovered](#how-partition-profiles-are-discovered)
- [Requesting a partition](#requesting-a-partition)
- [Limitations](#limitations)
- [Enabling partitioning on a node](#enabling-partitioning-on-a-node)
- [Disabling partitioning on a node](#disabling-partitioning-on-a-node)
- [Node reboot recovery](#node-reboot-recovery)
- [What GPUStack Operator does *NOT* do](#what-gpustack-operator-does-not-do)

## Prerequisites

**The driver must have been installed with virtualization support.** The vendor's installer takes a
flag for it; a driver installed without it reports no partition profiles at all, and the node simply
never advertises the family.

**Change the mode only on a node free of accelerator workloads, device manager included.** The switch
is refused while any process holds the driver, and the device-manager Pod is such a process — it
holds `/dev/kfd` for its whole life. The vendor tool reports:

```text
Error: Set system level mig mode failed. The device is exist and may be in use.
```

which names no holder. Find them through the control node's open file descriptors:

```console
$ for p in /proc/[0-9]*; do pid=${p#/proc/}; n=$(ls -l "$p"/fd 2>/dev/null | grep -c kfd); \
    [ "$n" -gt 0 ] 2>/dev/null && echo "pid=$pid fds=$n $(tr -d '\0' < "$p"/cmdline)"; done
```

Empty output is the precondition. The vendor's own `hymgr` daemon holds `/dev/mkfd` permanently and
does **not** block the switch; only `/dev/kfd` holders do.

**A mode change is not noticed until the device manager restarts.** The re-detect trigger watches the
device set and health, not the mode, so `hy-smi -mig` never reaches the cluster by itself. This is
pre-existing in the shared detector, already true for [NVIDIA MIG](nvidia-mig.md); restart the node's
device-manager DaemonSet after every change.

## How partition profiles are discovered

**Profiles are discovered from the driver, never computed.** Where [NVIDIA's are a fixed per-product
set](nvidia-mig.md#how-partition-profiles-are-discovered), the detector here offers exactly the
GPU-instance profiles the driver reports for the card in front of it, with the geometry and the legal
placements the driver gives.

On a C-3000 "BW" card — four GPU slices, 80 compute units, 65520 MiB — the driver reports three:

| Profile | Slices | Compute units | Memory | Per card | Legal placements |
| --- | --- | --- | --- | --- | --- |
| `2g.15gb` | 1 | 20 | 16380 MiB | 4 | `0:1 1:1 2:1 3:1` |
| `4g.31gb` | 2 | 40 | 32760 MiB | 2 | `0:2 2:2` |
| `8g.63gb` | 4 | 80 | 65520 MiB | 1 | `0:4` |

A card offering no profile of a given width simply has none — every card measured offers nothing
three slices wide, and that gap is normal rather than a fault.

**A card's compute-unit count comes from the profiles while the mode is on.** The HSA runtime, which
the detector reads it from otherwise, exposes at most one partition per process once the node is
partitioned — so it answers for one card with a partition's geometry rather than for every card with
the card's. Each profile carries the card's real count factored as its own units times the instances
that fill the card, and every profile of a measured card agreed.

### The driver's registry, `/etc/dmi_mig_config`

The driver keeps its own on-disk record of what exists, and this operator reads and depends on all
three parts of it. It is the vendor's directory, created and populated by the driver:

```text
/etc/dmi_mig_config/
├── dev0 … dev<N>     one line per physical card: its PCI address, e.g. 0000:09:00.0
├── gi/               one file per live GPU instance
└── ci/               one file per live compute instance, dev<N>gi<G>ci<C>.conf
```

Two of those are the *only* source for what they carry, because the management library does not answer
for either:

- **`dev<N>` is how a device index becomes a card.** `nvmlDeviceGetPciInfo` returns success and writes
  an empty string, and `nvmlDeviceGetUUID` is not an exported symbol at all — so tying a Multi-Instance
  handle back to a physical card goes through this file or nowhere.
- **A `ci/*.conf` is where a partition's identity lives.** It is the GPU-instance block and the
  compute-instance block concatenated, ending in a `mig_uuid:` line. The library exposes no getter for
  it.

**The device manager mounts this directory writable, and that is the one privilege partitioning adds.**
It creates and destroys instances through the library from inside its own process, and the driver writes
and removes these files as a side effect of those calls — so a read-only mount would fail the create
rather than merely hide the record.

## Requesting a partition

A partition request names **both** the family and the profile, and asks for exactly one of each:

```yaml
resources:
  limits:
    hygon.com/dcu.partitioned: "1"
    hygon.com/dcu.partitioned.mig-2g.15gb: "1"
  requests:
    hygon.com/dcu.partitioned: "1"
    hygon.com/dcu.partitioned.mig-2g.15gb: "1"
```

Naming only the profile allocates nothing; naming only the family is refused at admission with a
message saying a profile is required.

**What the container gets.** The device manager reserves a GPU instance of the profile at a free
placement, creates the whole-instance compute instance inside it, and injects:

- the node's control device nodes, `/dev/kfd` and `/dev/mkfd`;
- the card's own DRM nodes;
- the vendor user-space runtime;
- the instance's registry file, bound at **its own host path**, read-only;
- `DMI_MIG_VISIBLE_DEVICE=MIG-<uuid>`.

The registry file is the load-bearing one. The vendor runtime scans `/etc/dmi_mig_config/ci` by
absolute path, so a file bound anywhere else is not found and the workload reports no devices at all.

A container holding a `2g.15gb` partition sees exactly one device of 20 compute units and 16380 MiB.

## Limitations

**A container can use exactly ONE partition.** The vendor runtime makes one partition visible
whatever it is given: binding two registry files, passing a comma-separated list of `MIG-`-prefixed
identifiers, and passing `all` each yield a single visible device, on two driver generations.

A request granted more than one accelerator is therefore refused rather than half-served — carving on
every card would consume quota the workload can never reach. Split such a workload into one Pod per
partition.

**A partitioned node serves only partitions.** With the mode on, a container given the device nodes
but no registry file finds **no device at all** — measured on a node where five of eight cards held
no instance whatsoever.

So a partitioned node can serve neither whole-card nor logically sliced requests, and the operator
stops advertising both: `hygon.com/dcu` and `hygon.com/dcu.sliced` go to zero while
`hygon.com/dcu.partitioned` carries the node's whole capacity. Plan a node as partitioned or not,
never as mixed.

**The mode is node-wide.** The vendor's switch takes no device selector, and neither does the
library's query. There is no such thing as one card of a host being partitioned while another is
not — the mixed-population layout NVIDIA supports has no analogue here.

**A directory in the registry poisons an instance id.** The driver writes plain files under
`/etc/dmi_mig_config/ci`, and a container is given one of them as a bind mount. A container runtime
asked to bind a source that does not exist creates it — as a *directory* — so an instance destroyed
between its allocation and its container starting leaves a directory sitting on its name.

The driver can then never write that name again: creating a compute instance whose id maps to it
fails with `INSUFFICIENT_RESOURCES`, and because ids are handed back out after a destroy, the failure
outlives everything that caused it. The device manager sweeps such directories away on every reclaim
pass; if you meet one on a node running an older build, remove it with `rmdir` — `rm` refuses it.

**A recreated partition is a new grant.** Unlike NVIDIA's placement-derived MIG UUIDs, this vendor
issues a fresh identity every time an instance is created, even for the same profile at the same
placement on the same card. An identity recorded against a destroyed partition never matches its
replacement, which is what makes the operator's own reuse checks exact — but it also means a
partition identity is not a durable name for a *slot*.

## Enabling partitioning on a node

The mode switch itself is Hygon's procedure, not this operator's: [DCU Multi-Instance
使用手册](https://developer.sourcefind.cn/document/9169ef18-c10d-11f0-b077-0242ac150003?id=4a82aeed-e242-11f0-b9e4-0242ac150003&title=3+DCU+Multi-Instance%E4%BD%BF%E7%94%A8%E6%89%8B%E5%86%8C&version=9169ef18-c10d-11f0-b077-0242ac150003)
is the authority on `hy-smi -mig` and on the driver support it needs.

Two things below correct that manual: the long option is `--multi-instance-gpu`, not the
`--multi-instance-dcu` its screenshot shows, and the switch takes no `-i`.

1. **Drain the node's accelerator workloads**, and park the device-manager DaemonSet — it holds
   `/dev/kfd` and will otherwise block the switch. Confirm with the descriptor scan above.
2. **Turn the mode on**, node-wide:

   ```console
   $ sudo hy-smi -mig 1
   Enabled mig mode successful.
   ```

3. **Carve nothing by hand.** The operator creates and destroys instances per allocation; instances
   left over from manual experimentation are collected as unclaimed on the next partition activity.
4. **Restart the device-manager DaemonSet.** Until it restarts, the cluster still sees the node's
   pre-change capability.
5. **Confirm** the node advertises the family and its profiles, and no longer advertises whole cards:

   ```console
   $ kubectl get node <node> -o jsonpath='{.status.allocatable}' | tr ',' '\n' | grep hygon
   "hygon.com/dcu":"0"
   "hygon.com/dcu.partitioned":"32"
   "hygon.com/dcu.sliced":"0"
   ```

> **Never leave a partitioned node with no instances and expect it to work for anything else.** With
> the mode on and nothing carved, every card is unusable — the software stack fails to initialize.
> That is the state between step 2 and the first allocation, and it is why step 1 drains first.

## Disabling partitioning on a node

The teardown order is forced by the driver, and each step refuses if the next has not happened:

1. **Drain the node's partition workloads.** An instance a process holds cannot be destroyed; the
   driver refuses it, and the operator's reclaim leaves it alone and retries.
2. **Let the operator reclaim its instances**, or destroy them by hand — compute instances first,
   then their GPU instances:

   ```console
   $ sudo hy-smi mig -dci -i <card>
   $ sudo hy-smi mig -dgi -i <card>
   ```

3. **Park the device-manager DaemonSet**, for the same reason as enabling.
4. **Turn the mode off:**

   ```console
   $ sudo hy-smi -mig 0
   ```

   It is refused while any GPU instance survives anywhere on the node.
5. **Restart the device-manager DaemonSet**, and confirm the node advertises whole cards and logical
   slices again.

## Node reboot recovery

Instances do **not** survive a reboot, and neither does the registry: `/etc/dmi_mig_config` is
recreated by the driver when the mode comes up. The mode itself is a driver setting — check it after
a reboot rather than assuming it, and restart the device manager if it changed.

The operator needs no recovery step of its own. Ownership records whose Pods are gone are reclaimed
on the next pass, and a record naming an instance the driver no longer has is refused rather than
acted on.

## What GPUStack Operator does *NOT* do

- **Enable, disable or reconfigure the mode.** It is node-wide, it is refused while the device
  manager is running, and turning it on with nothing carved makes every card unusable. It is a
  provisioning action, like installing the driver.
- **Serve a workload from more than one partition.** See [Limitations](#limitations).
- **Mix partitioned and whole cards on one node.** The hardware does not offer it.
- **Preserve a partition across its Pod.** An instance is created for an allocation and destroyed
  when the allocation goes; nothing is kept warm.

**See also** — [Accelerator Requests](../accelerator-requests.md) (the request vocabulary and the
two-family split) · [NVIDIA MIG Operations](nvidia-mig.md) (the runbook this one mirrors) · [T-Head
MIG Operations](thead-mig.md) (the other vendor sharing the `mig` word) · [Device
Discovery](../architecture/device-discovery.md) (how a card's capability reaches the cluster).
