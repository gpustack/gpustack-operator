# T-Head MIG Operations

> **Purpose** — the administrator runbook for T-Head PPU partitioning *mode* (enable, disable, reboot
> recovery) and the user contract for requesting a partition, mirroring [NVIDIA MIG
> Operations](nvidia-mig.md).
> **Audience** operators, users requesting partitions · **Prerequisites** [Accelerator
> Requests](../accelerator-requests.md) · **Read time** ~10 min

Partitioning *mode* is a **manually managed** node property, as for NVIDIA MIG: the operator *observes*
the geometry through the Device Manager and reflects it in the `Devices` ledger and the advertised
capability; it never enables, disables or reconfigures it. With the mode on, it does **carve** the
GPU/Compute Instance pair backing a scheduled workload, and destroys it when the Pod exits.

T-Head names the feature as NVIDIA does — `hgml` mirrors NVML's MIG API function for function, `ppu-smi
mig` mirrors `nvidia-smi mig` flag for flag — so the resource-key segment is the same word, `mig`, for both
(see [Accelerator Requests](../accelerator-requests.md#two-families-two-accelerator-populations)). What
both share is linked below, not restated.

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

**Change the mode only on a node free of accelerator workloads, device manager included.** The driver
refuses it on a PPU with any active process, and a process that merely opens the driver can hold *every*
PPU node — so one PPU's workload blocks another's change. Stop every Pod using the node's accelerators,
and the device-manager Pod.

The kernel log names the holders, `ppu-smi` does not: the PPU and its active-process count, one line per
holder, then a failure line, while the command prints its own busy error. On a 16-PPU host, redacted:

```text
[alixpu] PPU00<N> contains 3 active process, please try reset later ...
[alixpu] Process[<comm>], pid:<pid>, uki_version:<n>
[alixpu] Process[<comm>], pid:<pid>, uki_version:<n>
[alixpu] PPU00<N> set MIG mode failed
```

and its own output:

```text
MIG resources are in use, please destroy related GPU instances and try again
Failed to Enable MIG mode for PPU <bus-id>: An operation cannot be performed because the PPU is currently in use.
```

Two notes:

- **The count is not the list length** — three processes reported, two holder lines printed. Treat it as
  a lower bound and go to the file descriptors.
- **`PPU00<N>` names the card ordinal** — what `ppu-smi -i <N>` takes, the `<N>` in `/dev/alixpu_ppu<N>` —
  not the reported minor number, a different value (see [Requesting a
  partition](#requesting-a-partition)).

That advice to "destroy related GPU instances" applies only when instances exist; with none it fails for
holders alone, and **needs no device reset**.

**Count the operator's control plane among the holders.** The worker Pod was observed opening every PPU
node briefly at startup, then releasing them, so a change attempted seconds after a control-plane restart
can fail for a holder on its way out. Re-check the descriptors.

**Diagnose from the kernel log and the holders' open file descriptors, never from `ppu-smi` alone**, which
reports the mode and carvable profiles, not who holds a PPU open:

```console
$ for p in /proc/[0-9]*; do pid=${p#/proc/}; n=$(ls -l "$p"/fd 2>/dev/null | grep -c alixpu); \
    [ "$n" -gt 0 ] 2>/dev/null && echo "pid=$pid fds=$n $(tr -d '\0' < "$p"/cmdline)"; done
```

Empty output is the precondition; anything printed is a process to stop. A holder typically has one
descriptor per PPU plus the shared control nodes — hence the cross-PPU blocking.

**A mode change is not noticed until the device manager restarts.** The re-detect trigger watches the
device set and health, not the mode, so `ppu-smi mig` never reaches the cluster by itself. This is
pre-existing in the shared detector, already true for [NVIDIA MIG](nvidia-mig.md); restart the node's
device-manager DaemonSet.

## How partition profiles are discovered

**Profiles are discovered from the driver, never computed.** There is no static per-product table as on
NVIDIA's [Supported profiles](nvidia-mig.md#supported-profiles): the detector offers exactly the
GPU-instance profiles the driver reports for the PPU in front of it.

- **A profile is offered only if its name round-trips.** The published key is the profile's name, resolved
  back to a driver profile id by probing every id and matching the driver's own name — so a nameless
  profile is dropped with a warning naming the PPU and the id, never renamed, and a name that cannot
  become a valid resource-name segment is excluded by the normalization (feature-name prefix and
  whitespace, applied by both the detector and the driver seam) that also drops NVIDIA's `+me` / `+gfx`
  variants.
- **A memory-slice span is read from the driver's placement records**, never divided out of a hardcoded
  per-PPU slice count; a profile with *no* legal placement is not offered, since the span could only be
  guessed — and the ledger being placement-derived, such a key could never allocate. The PPU names it in
  a warning.

Both rules are shared with NVIDIA.

What a PPU offers:

```console
$ ppu-smi mig -i <N> -lgip     # the profiles, with Instances Free/Total and memory per instance
$ ppu-smi mig -i <N> -lgipp    # the legal placements per profile, as {Start,Start...}:Size
```

**`-lgipp` does not tell you what is free.** It lists every *legal* placement irrespective of occupancy: a
PPU carved to capacity still lists every start, and lists placements for a profile that can no longer be
created. Occupancy lives in `-lgip`'s `Instances Free/Total` and in `-lgi`.

The ledger relies on that: it sizes capacity from the placement count because that count does not move
with occupancy, so an occupancy-sensitive `-lgipp` would be a defect to report, not a behaviour to adapt
to.

One PPU observed: `MIG 4g48gb` (id 5, two instances of four memory slices) and `MIG 8g96gb` (id 3, one
spanning all eight), creating either taking the other to zero free; those sparse ids are why nothing
indexes profiles by position.

**The published name carries a separator the manufacturer's does not.** Display names carry a `MIG `
prefix and a space, dropped by normalization, and spell the geometry with no separator between its two
numbers where NVIDIA writes one. So both read alike in a Pod spec, the operator publishes it: `MIG 4g48gb`
is advertised and requested as `<base>.partitioned.mig-4g.48gb`, and `MIG 8g96gb` as
`<base>.partitioned.mig-8g.96gb`.

The InstanceType's offered inventory and per-profile ledgers carry that name too, so the name you read is
the name you write. Below that boundary the driver's spelling stays — `ppu-smi`, the `Devices` record and
the on-disk ownership markers all say `4g48gb`. A name outside this two-number shape is published as the
driver reports it.

## Requesting a partition

With the mode on, a workload asks for **one hardware instance of a named profile**, as for NVIDIA: the
same `<base>.partitioned` / `<base>.partitioned.<kind>-<profile>` key pair and the same seven [request
rules](../accelerator-requests.md#the-request-rules) — one PPU, one profile shape, one container group,
`.units` webhook-derived, exclusive of every other family:

```yaml
resources:
  limits:
    alibabacloud.com/ppu.partitioned: "1"             # always exactly 1 PPU
    alibabacloud.com/ppu.partitioned.mig-<profile>: "1"  # always exactly 1 instance of this profile
```

Or through the `Instance` API, as with [NVIDIA's
`acceleratorPartitionedProfile`](nvidia-mig.md#requesting-a-mig-instance):

```yaml
kind: Instance
spec:
  type: <a T-Head InstanceType from your pool>
  resources:
    accelerator: "1"
    acceleratorPartitionedProfile: <profile>
```

**One partition per allocated PPU**, on the PPU the device plugin picks itself — never a pair split across
two PPUs, never two partitions in one container.

**The container receives device nodes, not an environment variable.** T-Head has no runtime hook for
injecting visibility, so the allocation response carries the device specifications: the two shared control
nodes, the parent PPU's node, and the capability nodes of the GPU Instance and its Compute Instance. Each
is required — missing, not a character device, or the wrong number fails the allocation and rolls back
rather than handing over an incomplete set.

**Two numbers name a PPU, and only one names a path.** The device node and the capability subtree both
carry the **card ordinal** — what `ppu-smi -i <N>` takes, the `<N>` in `/dev/alixpu_ppu<N>`, the `ppu<N>` in
the procfs capability tree. The ordinal keeps the word *card*: it addresses a PPU, not identifies one.

The **minor number** the driver reports is a *different* value: the shared `/dev/alixpu` control node
takes minor 0 of the same character-device major, so a PPU's minor is its ordinal plus one on the host
measured (ordinal 14 → minor 15, across all sixteen PPUs). Contradicting the manufacturer documentation's
`/dev/alixpu_ppu[minor number]`, a path built from the minor addresses **the next PPU**.

The operator builds both paths from the ordinal and reads the minor only to confirm the ordinal still
addresses the PPU the detector measured: it stats the node and refuses the allocation on a mismatch.

**The capability minors are read, never computed.** Each comes from the driver's live procfs record at
allocation time — `/proc/driver/alixpu/capabilities/ppu<N>/mig/gi<G>/access`, and `.../gi<G>/ci<C>/access`
for its Compute Instance — naming `/dev/alixpu-caps/alixpu-cap<minor>`, created with the instance. They are
large and unlike the instance ids (229632, its compute instance 229633), deterministic on one host but
promised by no contract; a read costs one file.

PPU selection, the fungible-token `Partitioned` family, the placement-aware `Remaining` ledger, `credits` quota
and reclaim on Pod deletion work as for NVIDIA — see [Requesting a MIG instance](nvidia-mig.md#requesting-a-mig-instance)
and [Device Discovery](../architecture/device-discovery.md#the-partitioned-family-fungible-tokens), including
the SSH sidecar seeing the partition rather than the parent PPU, and the reclaim race a same-profile
replacement can hit.

Device-node injection is the one place T-Head's response differs in *shape* from NVIDIA's.

## Limitations

- **GPUStack never enables, disables or reconfigures partition *mode***, and never evicts Pods on a mode
  change — the administrator owns the mode lifecycle, as for [NVIDIA](nvidia-mig.md#limitations).
- **A profile whose name the driver did not report, or cannot be normalized into a valid resource-name
  segment, never appears in the inventory** and cannot be requested (see [How partition profiles are
  discovered](#how-partition-profiles-are-discovered)).
- **One partition per allocated PPU.** A multi-partition workload asks for several Pods, the scope decision
  NVIDIA makes too (see [Rule 3](../accelerator-requests.md#rule-3--basepartitioned-is-exactly-1)).
- **Hand-carving a partition outside GPUStack is unsupported on a managed node** — every node-level number
  derives from the allocation annotations the device plugin writes, and a hand-carved instance produces
  none; see [Accelerator Requests](../accelerator-requests.md#limitations).
- **A same-profile replacement submitted the instant its predecessor is deleted can fail to start**, the
  reclaim-debounce reason documented for [NVIDIA](nvidia-mig.md#limitations); T-Head runs the same reclaim
  loop and bounded busy-destroy retry.

  The retry is narrower than its name: merely holding the PPU's device node does **not** block a destroy
  (with the device manager holding all sixteen nodes, one succeeded first try). What blocks a destroy is
  a condition — a GPU Instance refuses it while a live Compute Instance is under it, which the reclaim
  handles by destroying compute instances first. A busy destroy that survives the retry is a real fault,
  not a window to widen.
- **Whether the mode persists across a host reboot is not yet confirmed on real hardware.** Until your own
  testing says otherwise, re-apply it after any reboot, and restart the device-manager DaemonSet
  regardless. A mode switch needs **no** device reset: on a PPU free of instances and holders, enabling and
  disabling complete in place, despite the driver's busy message.

## Enabling partitioning on a node

1. Satisfy the [Prerequisites](#prerequisites): no accelerator workloads, no device-manager Pod.
2. Enable the mode. **`-mig` is a top-level flag, not a `ppu-smi mig` subcommand flag** — the subcommand
   carries the instance operations (`-lgip`, `-cgi`, `-dgi`, …) and rejects `-mig`:

   ```console
   # one PPU by card ordinal
   $ ppu-smi -i <N> -mig 1
   Enable MIG mode for PPU <bus-id>.
   All done.
   ```

   **One PPU per invocation.** `-i` takes no PPU list for this flag: a comma-separated form answers `No
   devices were found` and changes nothing — which reads like missing hardware, not a rejected argument.
   Several PPUs, several calls.

   Then confirm, because a successful call is not an applied mode:

   ```console
   $ ppu-smi -q -i <N> | grep -i -A2 "MIG Mode"
       MIG Mode
           Current                             : Enabled
           Pending                             : Enabled
   ```

3. Restart the node's Device Manager pod (`gpustack-operator-device-manager-thead`) to re-detect. As with
   NVIDIA, you need neither to delete the node's `Devices` object — the detector rewrites the group's
   capability in place — nor to pre-create instances: the operator materializes them on demand.
4. Verify the `Devices` capability now reports the PPU's partition profiles. Unlike NVIDIA, an
   unpartitioned T-Head PPU reports **no** slicing capability at all (there is no logical slicing to fall
   back to), so nothing has to go to zero — only the profiles have to appear.

## Disabling partitioning on a node

Run the inverse: let every Pod using the PPU's partitions go and reclaim, then remove any instance that
remains and flip the mode back.

**Destroy the Compute Instance before its GPU Instance.** The driver refuses otherwise — `GPU instance <G>
is in use, please destory related compute instances then try again` (the manufacturer's own spelling), with
a non-zero exit. The reclaim does this in order; by hand it is two steps:

```console
$ ppu-smi mig -i <N> -gi <G> -ci <C> -dci
$ ppu-smi mig -i <N> -gi <G> -dgi
```

**`-dgi` without `-gi` destroys every GPU Instance on the PPU**, not the one you were looking at:
convenient for a PPU you own outright, a hazard on a managed node, where other partitions may belong to
running Pods.

Then, with the PPU free of instances **and** of holders — the same [Prerequisites](#prerequisites) as
enabling, since the mode cannot be flipped back while a process holds the driver:

```console
$ ppu-smi -i <N> -mig 0
```

then restart the Device Manager pod so the ledger returns the PPU to unpartitioned capability.

**Stale capability device nodes are expected and are not live partitions.** The driver creates
`/dev/alixpu-caps/alixpu-cap<minor>` with an instance and does **not** remove it on destroy, so a node that
has hosted partitions accumulates entries. They are harmless: the minor is deterministic and reused when
the instance is recreated, and the operator never reads them as evidence — it takes each allocation's minor
from the live procfs record, which disappears with the instance.

**Do not read `/dev/alixpu-caps/` to find out what is partitioned** — those nodes outlive the instances.
Read `ppu-smi mig -i <N> -lgi`.

## Node reboot recovery

Partitions do not survive a Device Manager restart that finds different hardware state underneath them,
and — per [Limitations](#limitations) — whether the mode survives a host reboot is unconfirmed. Treat a
reboot as if it did not:

1. Re-run [Enabling partitioning on a node](#enabling-partitioning-on-a-node).
2. Restart the node's Device Manager pod whether or not the mode needed re-enabling — it realigns the
   `Devices` ledger to the post-reboot hardware either way.
3. **Resubmit** any partition workloads running before the reboot (delete and re-create the Pod/workload):
   the operator materializes a fresh instance on admission. A pre-reboot Pod that lingers with its on-disk
   ownership record but no live instance fails its device allocation closed until recreated.

## What GPUStack Operator does *NOT* do

- Enable, disable or reconfigure partition *mode* — those are `ppu-smi mig` operations you run. (It *does*
  create and destroy the Instances backing scheduled workloads.)
- Trigger on nodeconfig or labels, flip the mode automatically, or rewrite the geometry.
- Deschedule or evict Pods when the mode changes.
- Account for an instance you carved by hand. (It *does* delete it: an instance no allocation accounts for
  is reclaimed as an orphan once its PPU is idle.)

A capability change reaches the cluster **only** through Device Manager restart or re-detection.

---

**See also** — [NVIDIA MIG Operations](nvidia-mig.md) (the sibling runbook) ·
[Accelerator Requests](../accelerator-requests.md) (the full key set and request rules) ·
[Device Discovery](../architecture/device-discovery.md#the-partitioned-family-fungible-tokens) (how a
partition is placed and reclaimed)

**Next** → [Accelerator Requests](../accelerator-requests.md) — the normative request contract.
