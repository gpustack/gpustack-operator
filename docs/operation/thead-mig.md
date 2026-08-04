# T-Head PPU Partitioning Operations

> **Purpose** — the administrator runbook for T-Head PPU hardware partitioning *mode* on a node (enable,
> disable, reboot recovery) and the user contract for requesting a partition — the T-Head counterpart to
> [NVIDIA MIG Operations](nvidia-mig.md).
> **Audience** operators, users requesting partitions · **Prerequisites** [Accelerator
> Requests](../accelerator-requests.md) · **Read time** ~15 min

GPUStack treats a T-Head card's hardware partitioning *mode* as a **manually managed** node property,
exactly as it does for NVIDIA MIG: the operator *observes* partitioning geometry through the Device
Manager and reflects it in the `Devices` ledger and the advertised partitioning capability; it never
enables, disables, or reconfigures the mode on your behalf. Once a card's mode is on, though, the
operator **dynamically carves** the GPU Instance and Compute Instance that back a scheduled workload and
destroys them when the Pod exits. T-Head names the feature the same way NVIDIA does — its management
library `hgml` mirrors NVML's MIG API function for function, and `ppu-smi mig` mirrors `nvidia-smi mig`
flag for flag — so the resource-key segment is the same word, `mig`, for both vendors (see [Accelerator
Requests](../accelerator-requests.md#two-families-two-card-populations)).

This page states the two prerequisites administrators get wrong first, then the shape of the feature and
its limits, then the enable/disable/recovery procedures. For everything the two vendors share — the
resource keys, the request rules, the four-view `InstanceType` status, the reclaim guarantees — this page
links to [NVIDIA MIG Operations](nvidia-mig.md) and the architecture pages rather than restating it.

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

**The mode is changed only while the node is free of accelerator workloads, including the device
manager itself.** The vendor driver refuses a mode change on a card that has any active process, and
because a process that merely opens the driver may hold *every* card's node — not only the one it is
using — a workload running on one card can block the mode change on a completely different card. Stop
every Pod using the node's accelerators, and stop the node's device-manager Pod too, before touching the
mode.

The driver reports the holder(s) in the kernel log rather than through `ppu-smi` itself: a line naming
the card and how many processes are still active on it, then one line per holding process, then a
failure line, while the command prints a busy error of its own. Observed on a 16-card host, with the
identifiers replaced by placeholders:

```text
[alixpu] PPU00<N> contains 3 active process, please try reset later ...
[alixpu] Process[<comm>], pid:<pid>, uki_version:<n>
[alixpu] Process[<comm>], pid:<pid>, uki_version:<n>
[alixpu] PPU00<N> set MIG mode failed
```

and on the command's own output:

```text
MIG resources are in use, please destroy related GPU instances and try again
Failed to Enable MIG mode for PPU <bus-id>: An operation cannot be performed because the PPU is currently in use.
```

Two things about that log are worth knowing before you read one. **The process count is not the length of
the list** — the run above reported three active processes and printed two holder lines, so treat the
count as a lower bound on what you must still stop, and go back to the file descriptors. And **the card in
`PPU00<N>` is named by its ordinal**, the same number `ppu-smi -i <N>` takes and the same one that names
`/dev/alixpu_ppu<N>` — not by the minor number the card reports, which is a different value (see
[Requesting a partition](#requesting-a-partition)).

The first line's advice to "destroy related GPU instances" applies only when instances exist; a mode change
on a card with no instances fails for holders alone, and **needs no device reset** once they are gone —
enabling and disabling on an idle card both complete in place.

**Count the operator's own control plane among the holders.** Besides the obvious ones — the accelerator
workloads and the node's device-manager Pod — the operator's worker Pod was observed opening every card node
briefly while starting up, then releasing them. So a mode change attempted in the seconds after the control
plane restarts can fail for a holder that is already on its way out. Re-check the descriptors rather than
concluding the mode is unchangeable.

**Diagnose from the kernel log and from the open file descriptors of the holding processes — never from
`ppu-smi`'s own output alone.** `ppu-smi` reports the mode and the profiles it can already carve; it does
not enumerate who is holding a card open. Once the kernel log has named the busy card(s), find the
holding processes on the node and stop them before retrying the mode change. The listing that answers both
questions at once — is anything holding the driver, and if so what:

```console
$ for p in /proc/[0-9]*; do pid=${p#/proc/}; n=$(ls -l "$p"/fd 2>/dev/null | grep -c alixpu); \
    [ "$n" -gt 0 ] 2>/dev/null && echo "pid=$pid fds=$n $(tr -d '\0' < "$p"/cmdline)"; done
```

Empty output is the precondition for a mode change; anything printed is a process to stop first. Note the
`fds` count: a holder typically has one descriptor per card plus the shared control nodes, which is what
makes a workload on one card block a mode change on another.

**A mode change is not noticed until the device manager restarts.** The Device Manager's re-detect
trigger watches the device set and health, not the partitioning mode, so flipping the mode with `ppu-smi
mig` does not, by itself, reach the cluster. This is a pre-existing property of the shared detector — it
is already true for [NVIDIA MIG](nvidia-mig.md) — and it is exactly as true here: restart the node's
device-manager DaemonSet to pick the change up (see [Enabling partitioning on a
node](#enabling-partitioning-on-a-node)).

## How partition profiles are discovered

**Partition profiles are discovered from the driver and never computed.** Unlike the NVIDIA MIG page's
[Supported profiles](nvidia-mig.md#supported-profiles) table, there is no static per-product profile
table for T-Head here: the detector reads whatever GPU-instance profiles the driver reports for the
card in front of it, and offers exactly those.

- **A profile the driver does not name is not offered.** The published resource key is the profile's
  name, and the driver seam resolves that key back to a vendor profile id by probing every id and
  matching the driver's own name for it — a profile the driver reports without a name can never complete
  that round trip, so it is dropped (with a warning naming the card and the profile id) rather than
  published under a synthesized name. This rule is shared with NVIDIA.
- **A profile whose displayed name cannot become a valid resource-name segment is excluded**, the same
  normalization rule NVIDIA's `+me` / `+gfx` variants are excluded by (see [Supported
  profiles](nvidia-mig.md#supported-profiles)) — the vendor's own display name carries a feature-name
  prefix and whitespace, stripped by the same shared normalization both the detector and the driver seam
  call, so the round trip from resource key back to vendor profile stays closed.
- **A profile's memory-slice span is read from the driver's own placement records**, never divided out of
  a hardcoded per-card slice count — and a profile the driver enumerates *no* legal placement for is not
  offered at all, since the span then has no source and could only be guessed. Nothing is lost by that:
  the per-profile ledger is placement-derived, so such a profile would be a requestable key whose
  allocation could never succeed. The card names it in a warning. This rule is shared with NVIDIA.

To see what a card offers, and to read the answer correctly:

```console
$ ppu-smi mig -i <N> -lgip     # the profiles, with Instances Free/Total and memory per instance
$ ppu-smi mig -i <N> -lgipp    # the legal placements per profile, as {Start,Start...}:Size
```

**`-lgipp` does not tell you what is free.** The placement list is every *legal* placement for a profile,
irrespective of what is currently occupied: on a card carved to capacity it still lists every start, and it
still lists placements for a profile that can no longer be created at all. Occupancy lives in `-lgip`'s
`Instances Free/Total` column and in `-lgi`. This is the property the operator's own ledger depends on — it
sizes a profile's capacity from the placement count precisely because that count does not move with
occupancy — so if a future driver ever made `-lgipp` occupancy-sensitive, that would be a defect to report
rather than a behaviour to adapt to.

One product's inventory, as an illustration of the shape rather than a table to rely on: a card reporting
two profiles, `MIG 4g48gb` (two instances of four memory slices) and `MIG 8g96gb` (one instance spanning all
eight), where creating either takes the other to zero free. Note too that the driver's profile ids are sparse
— 5 and 3 for those two — which is why nothing in the operator indexes profiles by position.

**The published profile name carries a separator the vendor's own does not.** This vendor's display names
carry a `MIG ` prefix and a space, which normalization drops, and they spell the geometry with no separator
between its two numbers where NVIDIA writes one. So that a partition of either vendor reads the same way in
a Pod spec, the operator publishes the separator: `MIG 4g48gb` is advertised and requested as
`<base>.partitioned.mig-4g.48gb`, and `MIG 8g96gb` as `<base>.partitioned.mig-8g.96gb`. The same published
name appears in the InstanceType's offered inventory and in its per-profile ledgers, so the name you read is
the name you write. Everything below that boundary keeps the driver's own spelling — `ppu-smi` output, the
`Devices` record, and the operator's on-disk ownership markers all say `4g48gb` — so a name seen on the host
and a name written in a manifest differ by exactly that separator and nothing else. A profile name outside
this two-number shape is published exactly as the driver reports it.

## Requesting a partition

Once a card's mode is on, a workload asks for **one hardware instance of a named profile**, exactly as
for NVIDIA — the same request shape, the same `<base>.partitioned` / `<base>.partitioned.<kind>-<profile>`
key pair, and the same seven rules in [Accelerator Requests](../accelerator-requests.md#the-request-rules)
(one card, one profile shape, one container group, `.units` webhook-derived, mutually exclusive with every
other family):

```yaml
resources:
  limits:
    alibabacloud.com/ppu.partitioned: "1"             # always exactly 1 card
    alibabacloud.com/ppu.partitioned.mig-<profile>: "1"  # always exactly 1 instance of this profile
```

Or through the `Instance` API, the same way as [NVIDIA MIG's `acceleratorPartitionedProfile`
field](nvidia-mig.md#requesting-a-mig-instance):

```yaml
kind: Instance
spec:
  type: <a T-Head InstanceType from your pool>
  resources:
    accelerator: "1"
    acceleratorPartitionedProfile: <profile>
```

**One partition per allocated card.** As with NVIDIA, a request builds exactly one GPU Instance / Compute
Instance pair on exactly one card the device plugin selects itself — never a partition split across two
cards, and never more than one partition handed to a single container.

**The container receives device nodes, not an environment variable.** T-Head has no container-runtime
hook to inject visibility through, so the allocation response carries the device specifications directly:
the vendor's two shared control nodes, the parent card's own device node, and the capability nodes of both
the GPU Instance and its Compute Instance. Every node in that set is required — a node that is missing, is
not a character device, or carries the wrong number fails the allocation and rolls back, rather than
handing the container a shorter, silently incomplete device set.

**Two numbers name a card, and only one of them names a path.** A card's device node and its capability
subtree are both named by the card's **ordinal** — the number `ppu-smi -i <N>` takes, the `<N>` in
`/dev/alixpu_ppu<N>`, and the `ppu<N>` under the procfs capability tree. The **minor number** the driver
reports for the same card is a *different* value: the shared `/dev/alixpu` control node occupies minor 0 of
the same character-device major as the cards, so a card's minor is its ordinal plus one on the host this was
measured on (ordinal 14 → minor 15, uniformly across sixteen cards). Note this contradicts the vendor
documentation, which states the node-naming rule as `/dev/alixpu_ppu[minor number]`; on a real driver the
node carries the ordinal, and building a path from the reported minor addresses **the next card**. The
operator uses the ordinal for both paths and uses the reported minor only to prove the ordinal still
addresses the card the detector measured — it stats the node and refuses the allocation if that node's own
minor disagrees with the recorded one, rather than assuming any offset.

**The capability minor numbers are read, never computed.** Each instance's minor comes from the driver's
live procfs record at allocation time — `/proc/driver/alixpu/capabilities/ppu<N>/mig/gi<G>/access` for the
GPU Instance and `.../gi<G>/ci<C>/access` for its Compute Instance — and names
`/dev/alixpu-caps/alixpu-cap<minor>`, which the driver creates with the instance. The values are large and
bear no resemblance to the instance ids (a first instance observed at 229632, its compute instance at
229633), and while they proved deterministic on one host, nothing in the vendor's contract promises that;
reading them costs one file per allocation and removes the assumption entirely.

Card selection, the fungible-token `Partitioned` family, the placement-aware `Remaining` ledger, quota on
the shared `credits` resource, and reclaim on Pod deletion all work exactly as described for NVIDIA in
[Requesting a MIG instance](nvidia-mig.md#requesting-a-mig-instance) and [Device
Discovery](../architecture/discovery.md#the-partitioned-family-fungible-tokens) — including the SSH
sidecar seeing the partition rather than the parent card, and the reclaim race a same-profile replacement
submitted the instant its predecessor is deleted can hit (see
[Limitations](nvidia-mig.md#limitations)). The device-node injection above is the one place T-Head's
response differs in *shape* from NVIDIA's environment-variable injection; nothing about who selects the
card, who owns the credits, or when reclaim runs is different.

## Limitations

- **GPUStack never enables, disables, or reconfigures partition *mode***, and never deschedules or evicts
  Pods on a mode change — the administrator owns the mode lifecycle, exactly as for
  [NVIDIA](nvidia-mig.md#limitations). A capability change reaches the cluster only through Device Manager
  restart or re-detection.
- **A profile the driver did not name, or whose name cannot be normalized into a valid resource-name
  segment, never appears in the inventory** and cannot be requested (see [How partition profiles are
  discovered](#how-partition-profiles-are-discovered)).
- **One partition per allocated card.** A multi-partition workload asks for several Pods, the same scope
  decision NVIDIA makes (see [Accelerator Requests, Rule
  3](../accelerator-requests.md#rule-3--basepartitioned-is-exactly-1)).
- **Hand-carving a partition outside GPUStack is unsupported on a managed node** — every node-level number
  is derived from the allocation annotations the device plugin writes, and an instance created by hand
  with `ppu-smi mig` produces none; see the general rule and its consequences in [Accelerator
  Requests](../accelerator-requests.md#limitations).
- **A same-profile replacement submitted the instant its predecessor is deleted can fail to start**, for
  the same reclaim-debounce reason documented for NVIDIA (see [NVIDIA MIG
  Operations](nvidia-mig.md#limitations)): T-Head runs the same reclaim loop, with the same bounded
  busy-destroy retry. What that retry waits out on this vendor is narrower than the name suggests. A
  process merely holding the card's device node does **not** block an instance destroy — measured with the
  device manager holding all sixteen card nodes, a destroy succeeded on the first attempt — so the retry is
  not there to outlast a co-resident holder. What does block a destroy is a condition, not a delay: a GPU
  Instance with a live Compute Instance under it refuses to be destroyed until that instance is gone, which
  the reclaim handles by destroying compute instances first. Treat a busy destroy that survives the retry as
  a real fault to investigate rather than a window to widen.
- **Whether the mode persists across a host reboot is not yet confirmed on real hardware.** Until your own
  testing on your product confirms otherwise, treat the mode as needing to be re-applied after any reboot,
  and always restart the node's device-manager DaemonSet afterward regardless of what the mode itself did.
  A mode switch itself needs **no** device reset: on a card free of instances and holders, enabling and
  disabling both complete in place, despite the driver's busy message suggesting a reset.

## Enabling partitioning on a node

1. Satisfy the [Prerequisites](#prerequisites) above (stop every Pod using the node's accelerators, and
   stop the node's device-manager Pod).
2. Enable the mode. **`-mig` is a top-level device-modification flag, not a `ppu-smi mig` subcommand
   flag** — `ppu-smi mig` carries the instance operations (`-lgip`, `-cgi`, `-dgi`, …) and does not accept
   `-mig` at all, so the subcommand form fails:

   ```console
   # one card by ordinal
   $ ppu-smi -i <N> -mig 1
   Enable MIG mode for PPU <bus-id>.
   All done.
   ```

   **One card per invocation.** `-i` does not take a card list for this flag: a comma-separated
   form is answered with `No devices were found` and changes nothing, and because that reads like
   a missing-hardware failure rather than a rejected argument, confirm the mode afterwards rather
   than trusting the command's own output. Several cards means several calls.

   Then confirm, because a successful call is not the same as an applied mode:

   ```console
   $ ppu-smi -q -i <N> | grep -i -A2 "MIG Mode"
       MIG Mode
           Current                             : Enabled
           Pending                             : Enabled
   ```

3. Restart the node's Device Manager pod (`gpustack-operator-device-manager-thead`) so it re-detects the
   hardware. As with NVIDIA, you do **not** need to delete the node's `Devices` object — the detector
   rewrites an existing group's capability in place — and you do **not** need to pre-create instances with
   `ppu-smi mig`: the operator materializes each profile's instances on demand as workloads are admitted.
4. Verify the `Devices` capability now reports the card's partition profiles. Unlike NVIDIA, an unpartitioned
   T-Head card reports **no** slicing capability at all (T-Head has no logical/software slicing to fall
   back to today), so there is nothing to check has gone to zero on the way in — only that the physical
   profiles now appear.

## Disabling partitioning on a node

Run the inverse sequence: ensure no Pod is using the card's partitions, let them reclaim, then remove any
instance that remains and flip the mode back.

**Destroy the Compute Instance before its GPU Instance.** The driver refuses a GPU-instance destroy while
the instance still has a compute instance under it — `GPU instance <G> is in use, please destory related
compute instances then try again` (the vendor's own spelling), with a non-zero exit. The operator's reclaim
already does this in the right order; by hand it is two steps:

```console
$ ppu-smi mig -i <N> -gi <G> -ci <C> -dci
$ ppu-smi mig -i <N> -gi <G> -dgi
```

**`-dgi` without `-gi` destroys every GPU Instance on the card**, not the one you were looking at. That is
convenient for clearing a card you own outright and a hazard everywhere else — on a node GPUStack manages,
other partitions on the same card may belong to running Pods.

Then, with the card free of instances **and** of holders (the same [Prerequisites](#prerequisites) as
enabling — the mode cannot be flipped back while any process holds the driver):

```console
$ ppu-smi -i <N> -mig 0
```

and restart the node's Device Manager pod so the ledger returns the card to its unpartitioned capability.

**Stale capability device nodes are expected and are not live partitions.** The driver creates
`/dev/alixpu-caps/alixpu-cap<minor>` when an instance is created and does **not** remove it when the
instance is destroyed, so a node that has hosted partitions accumulates entries there. They are harmless:
the minor a given instance gets is deterministic and is reused when the same instance is recreated, and the
operator never trusts these entries as evidence of anything — it reads each allocation's minor from the
driver's live procfs record, which disappears with the instance. Do not read `/dev/alixpu-caps/` to find out
what is partitioned; read `ppu-smi mig -i <N> -lgi`.

## Node reboot recovery

Partitions do not survive a Device Manager restart that finds different hardware state underneath them,
and — per [Limitations](#limitations) — whether the mode itself survives a host reboot is not yet
confirmed for this vendor. Treat every reboot as if the mode did not survive it:

1. Re-run the [Enabling partitioning on a node](#enabling-partitioning-on-a-node) sequence.
2. Restart the node's Device Manager pod regardless of whether the mode actually needed re-enabling — it
   re-detects and realigns the `Devices` ledger to the actual post-reboot hardware either way.
3. **Resubmit** any partition workloads that were running before the reboot (delete and re-create the
   Pod/workload): the operator materializes a fresh instance on admission. A pre-reboot Pod that lingers
   with its on-disk ownership record but no live instance fails its device allocation closed until it is
   recreated.

## What GPUStack Operator does *NOT* do

- It does not enable, disable, or reconfigure partition *mode* — those are `ppu-smi mig` operations you
  run. (It *does* create and destroy the GPU/Compute Instances that back scheduled workloads; see
  [Requesting a partition](#requesting-a-partition).)
- It does not trigger on nodeconfig or labels, does not flip the mode automatically, and does not rewrite
  the partition geometry.
- It does not deschedule or evict Pods when the mode changes.
- It does not account for an instance you carved by hand. (It *does* delete it: an instance no allocation
  accounts for is reclaimed as an orphan once its card is idle — see [Limitations](#limitations).)

A capability change reaches the cluster **only** through Device Manager restart or re-detection.

---

**See also** — [NVIDIA MIG Operations](nvidia-mig.md) (the sibling runbook this page mirrors) ·
[Accelerator Requests](../accelerator-requests.md) (the full key set and request rules, shared by both
vendors) · [Device Discovery](../architecture/discovery.md#the-partitioned-family-fungible-tokens) (how a
partition is placed and reclaimed)

**Next** → [Accelerator Requests](../accelerator-requests.md) — the normative request contract.
