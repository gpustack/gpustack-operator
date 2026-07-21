# NVIDIA MIG Operations

GPUStack treats NVIDIA **MIG** (Multi-Instance GPU) as a **manually managed** node property. The operator
*observes* MIG geometry through the Device Manager and reflects it in the `Devices` ledger and the advertised
slicing capability; it never enables, disables, or repartitions MIG on your behalf. This page is the
administrator runbook for changing MIG on a node of a Kubernetes cluster running GPUStack.

The rule of thumb: **you drive `nvidia-smi`; GPUStack catches up on the next Device Manager detection.** A
capability change enters the cluster only when the node's Device Manager pod re-detects the hardware (on
restart, or when the device set / health changes) — there are no nodeconfig or label triggers, no automatic
mode flips, no geometry rewrites, and no descheduling.

## Supported profiles

A MIG-enabled card is partitioned into hardware instances drawn from a fixed profile set. Both A100-40GB and
H100-80GB expose **7 compute (SM) slices** and **8 memory slices**; one memory slice is 5 GB on A100-40GB and
10 GB on H100-80GB. The tables below ignore the `+me` (dedicated media engines) and `+gfx` (graphics-capable)
variants.

**A100-40GB**

| Profile | Memory | Compute slices (of 7) | Memory slices (of 8) | Max instances/card |
|---|---|---|---|---|
| 1g.5gb  | 5 GB  | 1 | 1 | 7 |
| 1g.10gb | 10 GB | 1 | 2 | 4 |
| 2g.10gb | 10 GB | 2 | 2 | 3 |
| 3g.20gb | 20 GB | 3 | 4 | 2 |
| 4g.20gb | 20 GB | 4 | 4 | 1 |
| 7g.40gb | 40 GB | 7 | 8 | 1 |

**H100-80GB**

| Profile | Memory | Compute slices (of 7) | Memory slices (of 8) | Max instances/card |
|---|---|---|---|---|
| 1g.10gb | 10 GB | 1 | 1 | 7 |
| 1g.20gb | 20 GB | 1 | 2 | 4 |
| 2g.20gb | 20 GB | 2 | 2 | 3 |
| 3g.40gb | 40 GB | 3 | 4 | 2 |
| 4g.40gb | 40 GB | 4 | 4 | 1 |
| 7g.80gb | 80 GB | 7 | 8 | 1 |

Instances also occupy **hardcoded placement slots**. Because A100 and H100 share the same 8-memory-slice
layout, the legal `start:size` positions (in memory-slice units) are identical across the two — only the GB
per slice differs. A combination is legal only when the occupied slot intervals do not overlap:

- size 1 (`1g.5gb` / `1g.10gb`) — any start 0–6
- size 2 (`1g.10gb` / `2g.10gb` on A100; `1g.20gb` / `2g.20gb` on H100) — starts 0/2/4/6
- size 4 (`3g.20gb` / `4g.20gb` on A100; `3g.40gb` / `4g.40gb` on H100) — starts 0 or 4
- size 8 (`7g.40gb` / `7g.80gb`) — start 0

The per-card `Devices` ledger reports each MIG card's profile inventory as static per-profile *counts* (the
maximum instances of each profile the card could host). Placement-aware free accounting is out of scope for
this release.

For the full profile tables of every MIG-capable GPU (including other memory variants and the `+me` / `+gfx`
profiles), see NVIDIA's
[Supported MIG Profiles](https://docs.nvidia.com/datacenter/tesla/mig-user-guide/supported-mig-profiles.html).

## Prerequisites

Before switching a card's MIG mode (enable or disable):

- The card's instances must be **idle** — no Pod or process may be using the card whose mode you are changing
  (stop the using Pod first).
- All daemons holding a driver handle (DCGM, `nvsm`, exporters, …) must be **stopped first**, or the switch
  hangs pending.
- The `nvidia_drm` kernel module must **not** be loaded, or the GPU reset fails.
- The operation requires **`CAP_SYS_ADMIN`**.
- **Ampere (A100/A30)** requires a **GPU reset** after the mode switch. **Hopper and newer** need no reset.

Instance (GI/CI) create/destroy, once the mode is on, is dynamic and online: destroying a GPU/compute instance
returns `IN_USE` only when *that* instance still has active processes — workloads on sibling instances of the
same card are unaffected.

## Limitations

- GPUStack **never** enables, disables, or repartitions MIG, and **never** deschedules or evicts Pods on a MIG
  change — the administrator owns the lifecycle. A capability change reaches the cluster only through Device
  Manager restart or re-detection.
- On **Ampere** the mode persists across reboots (stored in InfoROM); on **Hopper and newer** the mode is
  **not** persistent across reboots.
- MIG **instances never survive a reboot** on any generation.
- In a **passthrough VM** the hypervisor may forbid the GPU reset entirely — reboot the node/VM instead.
- Profile combinations are constrained by the fixed placement slots above; the ledger reports static
  per-profile counts, **not** placement-aware free accounting (that is follow-up work).
- Whether an already-allocated Pod's instance can be **recreated** after a reboot depends on the follow-up
  allocation work; this release only guarantees the ledger reflects reality after re-detection.

## Enabling MIG on a node

1. Satisfy the [Prerequisites](#prerequisites) above (stop the using Pod and driver-handle daemons on the
   node).
2. Enable the mode with `nvidia-smi`, per card or for all cards:

   ```console
   # one card by index
   $ nvidia-smi -i <id> -mig 1
   # all cards
   $ nvidia-smi -mig 1
   ```

3. **Ampere:** perform the required GPU reset. If `nvidia_drm` is loaded or a passthrough-VM restriction blocks
   the reset, reboot the node/VM instead. **Hopper and newer:** no reset is needed.
4. Create the instances you want with `nvidia-smi mig -cgi ... -C` (the geometry is yours to choose within the
   profile / placement rules above).
5. Restart the node's Device Manager pod so it re-detects the hardware.
6. Verify the `Devices` ledger now reports the card's MIG profiles and **zero soft-slice capability** on those
   cards (a MIG-enabled card is hard-partitioned and offers no logical soft slicing).

## Disabling MIG on a node

Run the inverse sequence: ensure no Pod is using the card's instances, destroy the instances, then

```console
$ nvidia-smi -i <id> -mig 0   # or `-mig 0` for all cards
```

apply the same reset rules (Ampere reset / reboot; Hopper needs none), and restart the node's Device Manager
pod so the ledger returns the card to its whole-card / soft-slice capability.

## Node reboot recovery

MIG instances never survive a reboot, and on Hopper and newer the mode itself does not persist either. If you
did **not** reset MIG before a reboot:

- On the way back up the instances are gone (and, on Hopper+, so is the mode).
- Redo the enable sequence above and restart the node's Device Manager pod. It re-detects and realigns the
  `Devices` ledger to the actual post-reboot hardware.

This release guarantees only that the ledger reflects reality after re-detection. Whether an
already-allocated Pod's instance can then be *recreated* — and the exact Pod-state/ledger interaction on
restart — depends on the ledger realigning with what that Pod holds, and is the job of the follow-up
allocation work, not this release.

## What GPUStack Operator does *NOT* do

- It does not enable, disable, or repartition MIG — those are `nvidia-smi` operations you run.
- It does not trigger on nodeconfig or labels, does not flip MIG mode automatically, and does not rewrite
  geometry.
- It does not deschedule or evict Pods when MIG changes.

A capability change reaches the cluster **only** through Device Manager restart or re-detection.
