# NVIDIA MIG Operations

> **Purpose** — the administrator runbook for MIG *mode* on a node (enable, disable, reboot recovery)
> and the user contract for requesting a MIG instance, with a recorded three-configuration walkthrough.
> **Audience** operators, users requesting partitions · **Prerequisites** [Accelerator
> Requests](../accelerator-requests.md) · **Read time** ~30 min

GPUStack treats a card's NVIDIA **MIG** (Multi-Instance GPU) *mode* as a **manually managed** node property:
the operator *observes* MIG geometry through the Device Manager and reflects it in the `Devices` ledger and the
advertised partitioning capability; it never enables, disables, or reconfigures MIG *mode* on your behalf. Once a
card is MIG-enabled, though, the operator **dynamically allocates** the hardware instances that back scheduled
workloads — it materializes a GPU/compute instance of the requested profile when a Pod is admitted and reclaims
it when the Pod exits (see [Requesting a MIG instance](#requesting-a-mig-instance)). This page is both the
administrator runbook for changing MIG *mode* on a node of a Kubernetes cluster running GPUStack and the user
contract for requesting a MIG instance.

MIG is GPUStack's one implemented **physical partitioning** backend. It is a resource family of its own
(`nvidia.com/gpu.partitioned*`), disjoint from the logical (software) slicing family `nvidia.com/gpu.sliced*`:
a MIG-enabled card serves *only* partition requests, and an unpartitioned card serves *only* whole-card, shared
and logical-slice requests. The full key set and the normative request rules live in
[Accelerator Requests](../accelerator-requests.md).

The rule of thumb for **mode**: **you drive `nvidia-smi`; GPUStack catches up when the node's Device Manager
re-detects.** A capability change enters the cluster only through Device Manager re-detection — and because the
detect loop's re-detect trigger watches the device set and health rather than the partitioning mode, a mode
toggle needs a **DaemonSet restart** to be picked up. There are no nodeconfig or label triggers, no automatic
mode flips, no geometry rewrites, and no descheduling.

## Contents

- [Supported profiles](#supported-profiles)
- [Requesting a MIG instance](#requesting-a-mig-instance)
- [Prerequisites](#prerequisites)
- [Limitations](#limitations)
- [Enabling MIG on a node](#enabling-mig-on-a-node)
- [Disabling MIG on a node](#disabling-mig-on-a-node)
- [Node reboot recovery](#node-reboot-recovery)
- [Walkthrough: three MIG configurations on one node](#walkthrough-three-mig-configurations-on-one-node)
- [What GPUStack Operator does *NOT* do](#what-gpustack-operator-does-not-do)

## Supported profiles

A MIG-enabled card is partitioned into hardware instances drawn from a fixed profile set. Both A100-40GB and
H100-80GB expose **7 compute (SM) slices** and **8 memory slices**; one memory slice is 5 GB on A100-40GB and
10 GB on H100-80GB.

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

> **The `+me` / `+me.all` / `+gfx` variants are not supported.** A Kubernetes resource name may not contain
> `+`, and GPUStack never rewrites a profile name to make it key-safe — a profile whose name is not already a
> valid resource-name segment is **excluded** from the card's inventory instead, so that a key always maps back
> to its profile by a plain prefix strip. Those variants therefore do not appear in the `Devices` ledger, in the
> node's capacity keys, or in the `InstanceType` inventory, and cannot be requested.

Instances also occupy **hardcoded placement slots**. Because A100 and H100 share the same 8-memory-slice
layout, the legal `start:size` positions (in memory-slice units) are identical across the two — only the GB
per slice differs. A combination is legal only when the occupied slot intervals do not overlap:

- size 1 (`1g.5gb` on A100; `1g.10gb` on H100) — any start 0–6
- size 2, the `1g` profiles (`1g.10gb` on A100; `1g.20gb` on H100) — starts 0/2/4/6
- size 2, the `2g` profiles (`2g.10gb` on A100; `2g.20gb` on H100) — starts 0/2/4
- size 4, the `3g` profiles (`3g.20gb` on A100; `3g.40gb` on H100) — starts 0 or 4
- size 4, the `4g` profiles (`4g.20gb` on A100; `4g.40gb` on H100) — start 0 only
- size 8 (`7g.40gb` / `7g.80gb`) — start 0

**A profile's slot count is its "max instances/card" from the tables above** — read that column as the
length of its placement list, which is what makes the two consistent.

**Two profiles covering the same memory slices do not always get the same placement freedom.** A `4g`
covers four slices exactly as a `3g` does, yet is offered only the one slot; a `2g` covers two exactly
as the same-size `1g` does, yet is offered three slots to its four. The practical consequence is that a
live instance can remove slots from profiles other than its own: a `3g` at slots 0–3 takes the card's
only `4g` slot with it, while still leaving room for a second `3g` at 4–7.

The per-card `Devices` ledger reports each MIG card's profile inventory as static per-profile *counts* (the
maximum instances of each profile the card could host) and, per profile, a **placement-aware `Remaining`**
count of how many instances still fit given what is already allocated — a `3g.40gb` at slots 0–3 removes the
`1g.10gb` slots it overlaps (see [Requesting a MIG instance](#requesting-a-mig-instance)).

For the full profile tables of every MIG-capable GPU (including other memory variants and the `+me` / `+gfx`
profiles), see NVIDIA's
[Supported MIG Profiles](https://docs.nvidia.com/datacenter/tesla/mig-user-guide/supported-mig-profiles.html).

## Requesting a MIG instance

Once a card is MIG-enabled, a workload asks for **one hardware instance of a named profile**. GPUStack
materializes that instance on demand and injects only the MIG device — there is no logical-slicing runtime
(`libvgpu.so`) and no fractional translation.

**Request shape.** A Pod requests one partitioned card plus the profile, one instance per Pod:

```yaml
resources:
  limits:
    nvidia.com/gpu.partitioned: "1"              # always exactly 1 card
    nvidia.com/gpu.partitioned.mig-1g.10gb: "1"  # always exactly 1 instance of this profile
```

`mig` is NVIDIA's own name for hardware partitioning, carried in the key as `<kind>`; another vendor's
partitioning would register its own kind under the same `.partitioned.<kind>-<profile>` shape.

- **Name the exact profile** from the [Supported profiles](#supported-profiles) tables (e.g. `1g.10gb`); there
  is no memory→profile translation — you request `mig-<profile>`, not a fraction or a MiB amount.
- Both `nvidia.com/gpu.partitioned` and the `mig-<profile>` key **must be exactly `1`**. A multi-partition
  workload asks for several Pods; the cap is a scope decision, not a hardware limit.
- The partition family is **mutually exclusive** with every other accelerator family Pod-wide — a Pod
  requesting both a partition and a whole card, a shared unit or a logical slice is rejected at admission.
- **One profile shape per Pod**, and the accelerator claims must sit in **one container group**. The full rule
  set, with an accepted and a rejected example each, is in
  [Accelerator Requests](../accelerator-requests.md#the-request-rules).
- **`nvidia.com/gpu.partitioned.units` is webhook-derived** — do not set it. The Pod webhook folds the
  profile's VRAM into it so a partition charges credits on the same scale a same-VRAM logical slice does.
- A MIG request must go through **Kueue**: submit it as a Pod (or workload) on a `LocalQueue`, or as an
  `Instance` (below). The rules and the folds bind only Pods carrying the `kueue.x-k8s.io/queue-name` label.

**Through the `Instance` API.** An `Instance` requests a partition with
`spec.resources.acceleratorPartitionedProfile`; the controller shapes it into the two keys above:

```yaml
kind: Instance
metadata:
  name: mig-instance
spec:
  type: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
  image: ubuntu:24.04
  command: ["sleep", "86400"]
  resources:
    accelerator: "1"                       # a partition is always one card
    acceleratorPartitionedProfile: 3g.40gb
  volume:
    ephemeral:
      capacity: 1Gi
```

The profile is validated against the pool's observed inventory at admission: a profile the pool does not offer
is rejected with the offered set in the message, rather than being admitted and left Pending forever. It is
mutually exclusive with `acceleratorSlicedMemoryPercentage` / `acceleratorSlicedCoresPercentage` — hardware
partitioning and software slicing cannot both apply to one card. The instance's host CPU and RAM are sized by
the profile's share of the card's VRAM, so a `1g` partition does not ask for a whole card's worth of either.

**Card selection is the plugin's, not the kubelet's.** Unlike the three card-bound families, a
`.partitioned` token is a fungible count: the device plugin picks the card itself, against the live partition
geometry, and records the card it actually used. It **packs** — the most-occupied card that still fits wins, so
a sibling stays whole for a later whole-card profile — and a rejection from `Allocate` therefore means the
whole node has no room, never that the kubelet offered the wrong card.

**Scheduling and the `Remaining` ledger.** Each MIG card advertises, per profile, how many instances it
accounts for — `allocated + remaining`, mirrored onto the node as the
`nvidia.com/gpu.partitioned.mig-<profile>` extended resource. Both terms are needed: the scheduler fits a Pod
by subtracting the requests of the Pods already on the node, so publishing only what is free would subtract
every live instance twice. Admission is placement-aware: the per-card AdmissionCheck admits a Pod only while
the requested profile has a free placement slot, and returns a **retry** (never a hard reject) while the ledger
has not yet been populated. Quota is charged on the manufacturer's `credits` resource, folded from
`nvidia.com/gpu.partitioned.units` — a `3g.40gb` and a logical `40Gi` slice cost the same, so both families
share one credit scale.

**Reclaim.** When the Pod exits, the operator destroys its compute instance then its GPU instance, and the
profile's `Remaining` restores within one reclaim cycle. A destroy that races a residual process returns
`NVML_ERROR_IN_USE`; the operator retries with bounds — never blocking sibling instances on the same card or
allocations on any other card — and surfaces an operator-visible log if a straggler still holds the instance
past the bound. The node key frees on **Pod deletion** while the instance survives up to a few reclaim misses,
so a same-profile replacement scheduled inside that window can fail its allocation closed and is retried by
Kueue; the window closes on its own.

**An SSH-enabled `Instance` sees its partition, not the card.** With SSH enabled the workload runs in `main`
and the SSH server in an `sshd` sidecar that `nsenter`s into `main`, so the sidecar's own device allocation is
a device-cgroup grant and nothing else — the session inherits `main`'s environment. That grant is **the MIG
instance `main` holds**, never the parent card, which would otherwise expose every other partition carved on
it. The identity comes from the same on-disk ownership marker the allocator writes when it materializes the
instance — the record that already drives reuse and reclaim, and the one that survives a Device Manager
restart — so the marker is the visibility path's authority too. The read is **liveness-checked under the card
lock** before anything is injected: the recorded GPU instance must still exist *and* still carry the recorded
MIG UUID, since a destroyed instance's id can be reassigned to somebody else's partition. A marker that is
missing, names a different card, records a profile the card no longer offers, or whose instance is gone or has
been recreated fails the sidecar's allocation closed — there is no fallback to the card.

## Prerequisites

Before switching a card's MIG mode (enable or disable):

- The card's instances must be **idle** — no Pod or process may be using the card whose mode you are changing
  (stop the using Pod first). Draining is not just the hardware's requirement: a family's tokens exist only
  while the card's capability reports that family, so flipping the mode *removes* the old family's tokens.
- All daemons holding a driver handle (DCGM, `nvsm`, exporters, …) must be **stopped first**, or the switch
  hangs pending.
- The `nvidia_drm` kernel module must **not** be loaded, or the GPU reset fails.
- The operation requires **`CAP_SYS_ADMIN`**.
- **Ampere (A100/A30)** requires a **GPU reset** after the mode switch. **Hopper and newer** need no reset.

Instance (GI/CI) create/destroy, once the mode is on, is dynamic and online: destroying a GPU/compute instance
returns `IN_USE` only when *that* instance still has active processes — workloads on sibling instances of the
same card are unaffected.

## Limitations

- GPUStack **never** enables, disables, or reconfigures MIG *mode*, and **never** deschedules or evicts Pods on
  a mode change — the administrator owns the *mode* lifecycle. (The operator does create and destroy the
  *instances* that back scheduled workloads; see [Requesting a MIG instance](#requesting-a-mig-instance).) A
  capability change reaches the cluster only through Device Manager restart or re-detection.
- **Carving an instance out of band is unsupported on a managed node** — the accounting will not see it, and
  GPUStack will eventually delete it. Every node-level number — the per-profile capacity key, the partition
  token health and the AdmissionCheck — is derived from the allocation annotations the device plugin writes,
  and an instance created by hand with `nvidia-smi mig -cgi` produces none. So while any GPUStack workload
  holds the card, the node keeps advertising room it does not have and, unlike a transient
  over-advertisement, that **never converges**. Placement reads live NVML and so will not double-book such an
  instance, but the accounting above it stays wrong. Then, once the card is fully drained of GPUStack
  workloads, the opposite happens: an instance no allocation accounts for is an orphan, and the reclaimer
  **destroys it** after its debounce — including one it never created, and including one your own process is
  still using. Let GPUStack materialize the instances; it reuses any that already exist on a card it manages.
- **A same-profile replacement submitted the instant its predecessor is deleted can fail to start.** Node
  accounting is rebuilt from Pod annotations, so a deleted Pod's slot reappears in the per-profile key and
  in the healthy token count **immediately**, while the reclaimer destroys the hardware instance on its
  own debounce. A replacement admitted inside that gap can be handed the outgoing instance, which is then
  destroyed under it: the container fails to create (`failed to get device handle from UUID: Not Found`)
  or the Pod ends in `UnexpectedAdmissionError`. It is a startup failure, not corruption, and a resubmit
  after the reclaim lands succeeds — but for a workload that recycles partitions rapidly, leave a gap
  between the delete and the replacement, or let the replacement's own restart handle it.
- **A managed provider may health-check the GPUs and reboot the node for you.** Some managed Kubernetes
  offerings probe the NVLink topology matrix and expect `N × (N-1)` healthy pairs; a card in a
  partitioning mode drops out of that matrix, so the probe reads short and the provider's node agent can
  cordon and then restart the node. Check the provider's node conditions before blaming the operator when
  several workloads fail at once, and note that anything you merely *stopped* during node preparation
  (a telemetry service holding driver handles) comes back on that reboot.
- On **Ampere** the mode persists across reboots (stored in InfoROM); on **Hopper and newer** the mode is
  **not** persistent across reboots.
- MIG **instances never survive a reboot** on any generation; after re-enabling mode and restarting the Device
  Manager, **resubmit** any MIG workloads that were running before the reboot (see
  [Node reboot recovery](#node-reboot-recovery)).
- In a **passthrough VM** the hypervisor may forbid the GPU reset entirely — reboot the node/VM instead.
- Profile combinations are constrained by the fixed placement slots above; the ledger's `Remaining` is
  **placement-aware**, so a wide instance removes the smaller slots it overlaps. The per-profile node keys are
  independently subtracted scalars, so two Pods asking for mutually exclusive profiles can still both be
  scheduled onto a node that can host only one of them — the second fails its allocation closed and is retried.
- The `+me` / `+me.all` / `+gfx` profile variants are excluded from the inventory (see
  [Supported profiles](#supported-profiles)).
- **A profile the driver enumerates no legal placement for is excluded too**, along with one whose
  placement query failed. A profile's memory-slice span is read from its placement records and has no
  other source, so without them the span could only be guessed — and the span is what an instance's
  identity is matched by. Nothing is lost: the per-profile ledger is placement-aware, so such a profile
  would be a requestable key whose allocation could never succeed. The card and profile ids are named in
  a warning.
- **The old per-profile key is gone.** A MIG profile used to be requested through the *logical* slicing
  family, as a `mig-<profile>` segment on `nvidia.com/gpu.sliced`; it is now
  `nvidia.com/gpu.partitioned.mig-<profile>`, with no translation and not even a rejection by name — a Pod
  carrying the old shape simply never schedules. See
  [Pre-release breaks](../accelerator-requests.md#pre-release-breaks), which also explains why a node must be
  **drained before its device manager is upgraded**.

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
4. Restart the node's Device Manager pod so it re-detects the hardware. You do **not** need to delete the
   node's `Devices` object — the detector rewrites an existing group's capability in place — and you do **not**
   need to pre-create instances with `nvidia-smi mig -cgi`: the operator materializes each profile's instances
   on demand as workloads are admitted (it reuses any you did pre-create).
5. Verify the `Devices` capability now reports the card's MIG profiles and **zero logical slicing** on those
   cards (a MIG-enabled card is hardware-partitioned and offers no software slicing).

## Disabling MIG on a node

Run the inverse sequence: ensure no Pod is using the card's instances, destroy the instances, then

```console
$ nvidia-smi -i <id> -mig 0   # or `-mig 0` for all cards
```

apply the same reset rules (Ampere reset / reboot; Hopper needs none), and restart the node's Device Manager
pod so the ledger returns the card to its whole-card / logical-slice capability.

## Node reboot recovery

MIG instances never survive a reboot, and on Hopper and newer the mode itself does not persist either. If you
did **not** reset MIG before a reboot:

- On the way back up the instances are gone (and, on Hopper+, so is the mode).
- Redo the enable sequence above and restart the node's Device Manager pod. It re-detects and realigns the
  `Devices` ledger to the actual post-reboot hardware.

A MIG workload that was running before the reboot lost its instance, so **resubmit it** (delete and re-create
the Pod/workload): the operator materializes a fresh instance on admission. A pre-reboot Pod that lingers with
its on-disk ownership record but no live instance fails its device allocation closed until it is recreated —
resubmitting is the clean recovery.

## Walkthrough: three MIG configurations on one node

A recorded run on a live Kubernetes cluster, in the same style as the
[scheduling-chain walkthrough](../walkthrough.md): every command is the real `kubectl` (or on-node
`nvidia-smi`) invocation and its real output. The node is genericized as `node-h100` — **eight**
H100-80GB cards — and the operator runs the defaults.

Eight cards is the point. A single-card node cannot show the property that matters most here: the two
families are served by **disjoint card populations**, so partitioning *some* cards moves exactly those
cards from one family to the other and leaves the rest untouched. The walkthrough therefore covers the
three configurations a node can be in:

| | Configuration | Cards in a partitioning mode |
|---|---|---|
| **1** | [All-logical](#1-all-logical--every-card-mig-off) | none |
| **2** | [All-physical](#2-all-physical--every-card-mig-on) | all 8 |
| **3** | [Mixed](#3-mixed--part-logical-part-physical) | 3 of 8 |

The one column to keep in mind: `kubectl get instancetypes` shows the accelerator four-view
**EX**clusive / **SH**ared / **SL**iced (logical) / **PT** (physically partitioned) as
`onceMaxRequest/remaining` groups. A card feeds exactly one side of that split, so partitioning a card
**moves** its capacity from the first three groups to the fourth rather than adding to it. The first
three are credit-based (they track each card's per-card credit budget); `PT` counts hardware instances
the cards can still host.

### 1. All-logical — every card MIG off

Every card offers logical slicing and none offers partitions. The pool shows all three logical views
populated and an empty partition view:

```console
$ kubectl get instancetypes
NAME                                          ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU       PHASE
gpustack--generic-linux-amd64                 gpustack-fnv64-3b93966fd73eb9ec   1/2Gi/100Gi             0/0 0/0 0/0 0/0            128/132   Active
gpustack--nvidia-h100-80gb-hbm3-linux-amd64   gpustack-fnv64-e4768a65ca0ce96b   12/192Gi/100Gi          8/8 80/80 100/800 0/0      0/0       Active
```

Read the accelerated row as: **8** whole cards, **80** shares (10 per card), **800 %** of logical slice
budget (100 % per card), and **no** partition capacity. The node advertises the logical key family and
`nvidia.com/gpu.partitioned` sits at 0, which is what keeps every `.partitioned.*` counting key absent:

```console
$ kubectl get node node-h100 -o json | jq '.status.allocatable | with_entries(select(.key|test("nvidia.com/gpu")))'
{
  "nvidia.com/gpu": "8",
  "nvidia.com/gpu.partitioned": "0",
  "nvidia.com/gpu.shared": "80",
  "nvidia.com/gpu.sliced": "1024",
  "nvidia.com/gpu.sliced.cores-percentage": "102400",
  "nvidia.com/gpu.sliced.memory-mib": "655360",
  "nvidia.com/gpu.sliced.memory-percentage": "800",
  "nvidia.com/gpu.sliced.units": "12800k"
}
```

Every number is per-card × 8: `1024 = 8 × 128` logical slices, `12800k = 8 × 1600k` credits,
`655360 MiB = 8 × 81920`. The `Devices` capability (in `spec`; the per-card runtime ledger is in
`status`) says the same thing card by card:

```console
$ kubectl get devices node-h100 -o jsonpath='{.spec.groups[0].accelerators[*].status}' | jq -c
group NVIDIA H100 80GB HBM3: 8 card(s); logically sliceable indices [0, 1, 2, 3, 4, 5, 6, 7]; partitioned indices -
  card 0 logicalSliced: {"coresPercentageOvercommit": true, "count": 128}
  …                     (cards 1-7 identical)

$ kubectl get instancetypes gpustack--nvidia-h100-80gb-hbm3-linux-amd64 -o jsonpath='{.status.detail.slicedDetail}' | jq
{
  "logical": { "coresPercentageOvercommit": true, "count": 1024 },
  "physical": {}
}
```

### 2. All-physical — every card MIG on

#### 2.1 Enable the mode, then re-detect

Following the [Enabling MIG on a node](#enabling-mig-on-a-node) runbook, flip the mode with `nvidia-smi`
on the node itself (Hopper needs no GPU reset). GPUStack never runs this:

```console
# On node-h100 (via SSH), the cards idle and driver-handle daemons stopped:
$ for i in $(seq 0 7); do sudo nvidia-smi -i "$i" -mig 1; done
Enabled MIG Mode for GPU 00000000:8D:00.0
All done.
…                                                    (cards 1-7 identical)
```

The cluster does **not** react yet — a capability change enters only through Device Manager
re-detection, and the detect loop's trigger watches the device set and health, not the partitioning
mode. Restarting the DaemonSet pod is the whole procedure. The detector rewrites an existing group's
capability **in place**, so deleting the node's `Devices` object is **not** required (it was, in earlier
builds):

```console
$ kubectl -n gpustack-system rollout restart ds/gpustack-operator-device-manager-nvidia
daemonset.apps/gpustack-operator-device-manager-nvidia restarted

$ kubectl -n gpustack-system rollout status ds/gpustack-operator-device-manager-nvidia
daemon set "gpustack-operator-device-manager-nvidia" successfully rolled out
```

> **Wait for the device plugin to re-register, not just for the rollout.** The node's capacity keys are
> republished by the operator as soon as the ledger changes, which is *earlier* than the moment the
> kubelet has the plugin's new token set. A Pod admitted in that gap is allocated against the previous
> registration and dies with `UnexpectedAdmissionError`. Wait for the new Device Manager pod to be
> Ready **and** the `.partitioned` key to appear, then give the kubelet a moment.

#### 2.2 The whole node changes families

All three logical views drop to zero and the partition view takes over. This is not cosmetic: an
exclusive tenant on a MIG-mode card would get a GPU CUDA cannot use, so the views now report what is
actually servable.

```console
$ kubectl get instancetypes
NAME                                          ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU       PHASE
gpustack--generic-linux-amd64                 gpustack-fnv64-3b93966fd73eb9ec   1/2Gi/100Gi             0/0 0/0 0/0 0/0            128/132   Active
gpustack--nvidia-h100-80gb-hbm3-linux-amd64   gpustack-fnv64-e4768a65ca0ce96b   12/192Gi/100Gi          0/0 0/0 0/0 1/56           0/0       Active
```

`PT 1/56` reads: **1** is the most a single request can ask for — a partition request is validated to
be exactly one instance on exactly one card, so any larger `onceMaxRequest` would advertise a value
every ingress path rejects — and **56** is what the whole node can still host (`8 × 7`). Read `1` as
"there is room", `0` as "there is none". The node's keys flip families wholesale — every
`.sliced.*` key is gone, and one `partitioned.mig-<profile>` key appears per profile alongside
`.partitioned.units`, which values a partitioned card at a whole card's credits exactly as
`.sliced.units` valued a logically sliceable one:

```console
$ kubectl get node node-h100 -o json | jq '.status.allocatable | with_entries(select(.key|test("nvidia.com/gpu")))'
{
  "nvidia.com/gpu": "0",
  "nvidia.com/gpu.partitioned": "56",
  "nvidia.com/gpu.partitioned.mig-1g.10gb": "56",
  "nvidia.com/gpu.partitioned.mig-1g.20gb": "32",
  "nvidia.com/gpu.partitioned.mig-2g.20gb": "24",
  "nvidia.com/gpu.partitioned.mig-3g.40gb": "16",
  "nvidia.com/gpu.partitioned.mig-4g.40gb": "8",
  "nvidia.com/gpu.partitioned.mig-7g.80gb": "8",
  "nvidia.com/gpu.partitioned.units": "12800k",
  "nvidia.com/gpu.shared": "0",
  "nvidia.com/gpu.sliced": "0"
}
```

Each per-profile key is 8 × that profile's per-card count from the
[profile table](#supported-profiles) — `56 = 8 × 7` one-slice instances, `8 = 8 × 1` whole-card ones.
`nvidia.com/gpu`, `.shared` and `.sliced` read `0` rather than disappearing: a device-plugin pool key
zeroes out, it is never removed.

```console
$ kubectl get devices node-h100 -o jsonpath='{.spec.groups[0].accelerators[*].status}' | jq -c
group NVIDIA H100 80GB HBM3: 8 card(s); logically sliceable indices -; partitioned indices [0, 1, 2, 3, 4, 5, 6, 7]
  card 0 partitioned: count=7 profiles=1g.10gb(7),1g.20gb(4),2g.20gb(3),3g.40gb(2),4g.40gb(1),7g.80gb(1)
  …                   (cards 1-7 identical)
```

#### 2.3 Request a MIG instance

Submit a Pod on the pool's entrance `LocalQueue` (see
[Requesting a MIG instance](#requesting-a-mig-instance); an `Instance` with
`acceleratorPartitionedProfile: 3g.40gb` produces the same two keys):

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: mig-demo
  namespace: default
  labels:
    kueue.x-k8s.io/queue-name: gpustack-fnv64-e4768a65ca0ce96b
spec:
  runtimeClassName: nvidia
  restartPolicy: Never
  containers:
    - name: main
      image: ubuntu:24.04
      command: ["sleep", "86400"]
      resources:
        limits:
          nvidia.com/gpu.partitioned: "1"              # one partitioned card
          nvidia.com/gpu.partitioned.mig-3g.40gb: "1"  # one 3g.40gb instance on it
```

Kueue admits it, the node-devices AdmissionCheck confirms a free `3g.40gb` placement, and the device
plugin **materializes** the GPU/compute instance on a card it selects, injecting only that MIG device:

```console
$ kubectl get pod mig-demo
NAME       READY   STATUS    RESTARTS   AGE
mig-demo   1/1     Running   0          8s

$ kubectl exec mig-demo -- nvidia-smi -L
GPU 0: NVIDIA H100 80GB HBM3 (UUID: GPU-950792bf-a01c-3f1a-e122-3473e67f54b2)
  MIG 3g.40gb     Device  0: (UUID: MIG-b3061c09-2a4c-5026-a575-79f86a5bb12c)
```

#### 2.4 What one instance costs the node

One `3g.40gb` occupies memory slices 0–3 of **one** card. Only that card's arithmetic changes, and it
changes per profile according to the [placement rules](#supported-profiles):

```console
$ kubectl get instancetypes gpustack--nvidia-h100-80gb-hbm3-linux-amd64
NAME                                          ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU   PHASE
gpustack--nvidia-h100-80gb-hbm3-linux-amd64   gpustack-fnv64-e4768a65ca0ce96b   12/192Gi/100Gi          0/0 0/0 0/0 7/52   0/0   Active

$ kubectl get node node-h100 -o json | jq '.status.allocatable | with_entries(select(.key|test("gpu\\.partitioned")))'
{
  "nvidia.com/gpu.partitioned": "53",
  "nvidia.com/gpu.partitioned.mig-1g.10gb": "52",
  "nvidia.com/gpu.partitioned.mig-1g.20gb": "30",
  "nvidia.com/gpu.partitioned.mig-2g.20gb": "22",
  "nvidia.com/gpu.partitioned.mig-3g.40gb": "16",
  "nvidia.com/gpu.partitioned.mig-4g.40gb": "7",
  "nvidia.com/gpu.partitioned.mig-7g.80gb": "7",
  "nvidia.com/gpu.partitioned.units": "12800k"
}
```

Every one of those numbers is `Σ over cards of (allocated + remaining)`, which is what makes them
subtract correctly — the scheduler fits a Pod by subtracting the requests of the Pods already on the
node, so publishing bare *remaining* would subtract each live instance twice:

| Profile | 7 untouched cards | the carved card | total |
|---|---|---|---|
| `1g.10gb` | `7 × 7 = 49` | slices 4–7 free → 3 | **52** |
| `1g.20gb` | `7 × 4 = 28` | slots `{4,2}`, `{6,2}` → 2 | **30** |
| `2g.20gb` | `7 × 3 = 21` | slot `{4,2}` → 1 | **22** |
| `3g.40gb` | `7 × 2 = 14` | 1 allocated + 1 remaining | **16** |
| `4g.40gb` | `7 × 1 = 7` | its one slot is taken → 0 | **7** |
| `7g.80gb` | `7 × 1 = 7` | needs all 8 slices → 0 | **7** |

Read the `3g.40gb` row as `allocated (1) + remaining (1)`: the scheduler subtracts `mig-demo`'s own
request of 1 and correctly sees **one** more fitting on that card. `7g.80gb` and `4g.40gb` each lost
exactly the carved card, while `3g.40gb` lost nothing at all — [2.5](#25-where-those-numbers-come-from)
walks why, one profile at a time.

Deleting the Pod releases the credits immediately; the operator then destroys the compute instance and
the GPU instance, and the profile keys restore within one reclaim cycle.

> **Do not re-request the freed slot instantly.** The accounting frees on Pod deletion while the
> hardware is destroyed on the reclaimer's debounce, so a same-profile replacement submitted in that gap
> can be handed an instance that is about to disappear — see [Limitations](#limitations).

#### 2.5 Where those numbers come from

Four kinds of number on this page count that one carved card, and they do not agree — deliberately.
Walking the arithmetic once makes the rest of the page read cleanly.

**Per card it is interval overlap, nothing more.** A profile may only start at one of its hardcoded
slots, and the slot count *is* its per-card maximum ([Supported profiles](#supported-profiles)). An
instance still fits when its slot's interval overlaps nothing already taken — no device access and no
driver call, just intervals:

```text
the carved card's 8 memory slices

  [0][1][2][3]  taken by the live 3g.40gb          [4][5][6][7]  free
```

Every profile is then re-counted against that one occupied interval:

| Profile | Slot size | Legal starts | Blocked by the live `3g.40gb` | Still free | Adds to *that profile's* key |
|---|---|---|---|---|---|
| `1g.10gb` | 1 | 0 1 2 3 4 5 6 | 0, 1, 2, 3 | 4, 5, 6 | 3 |
| `1g.20gb` | 2 | 0 2 4 6 | 0, 2 | 4, 6 | 2 |
| `2g.20gb` | 2 | 0 2 4 | 0, 2 | 4 | 1 |
| `3g.40gb` | 4 | 0 4 | — (start 0 *is* the live one) | 4 | 1 allocated + 1 free |
| `4g.40gb` | 4 | 0 | 0 | — | 0 |
| `7g.80gb` | 8 | 0 | 0 | — | 0 |

Two rows are worth reading twice. `2g.20gb` has three slots, not four, so it loses its last one to a
`3g` that a `1g.20gb` survives. And `4g.40gb` contributes **0 allocated**, not 1: the ledger keys an
allocation by the profile actually built, which is `3g.40gb`.

**The four numbers, and what each one sums.** Take the whole node — seven untouched cards plus the
carved one:

| Number | Value here | Sums, over the node's partitioned cards | Why that shape |
|---|---|---|---|
| `…partitioned.mig-<profile>` | the table above | that profile's `allocated + remaining` | the scheduler subtracts the node's existing requests, so the key must include them |
| `nvidia.com/gpu.partitioned` | `53` | `allocated +` the card's **largest** per-profile free count | one pool key for the family, same scheduler-fit reason |
| `InstanceType` `PT` remaining | `52` | that largest free count alone, **no allocated term** | a user-facing "how many more can I start" |
| `InstanceType` `PT` onceMaxRequest | `1` | not a sum at all: `1` while any card can still host an instance, else `0` | one request builds exactly one instance on exactly one card, and both ingress paths reject any other count |

`53` and `52` differ by exactly the one live instance — the pool key carries it, the user-facing view
does not. And `onceMaxRequest` is `1`, not `52`, because no single request could ever consume the
node's remainder — nor even two instances of it.

**A card's contribution is a maximum, never a sum.** To those last three numbers the carved card
contributes `3` — not `3 + 2 + 1 + 1 = 7`. Its profiles compete for the same physical slices, so
creating an instance of one consumes placements of the others, and adding them would count the same
hardware several times over. The largest per-profile free count is the honest answer to "how many more
instances can this card host": three `1g.10gb`, or fewer larger ones instead — never both.

**The capability snapshot does not move at all.** `status.detail.slicedDetail` on the `InstanceType`
still reports `physical.count: 56` and `4g.40gb: 8` after the carve, unchanged from
[step 2.2](#22-the-whole-node-changes-families). It is derived only from the `Devices` **spec** — the
detector's record of what these cards could host when empty — and never reads the runtime ledger. Every
number in the table above is instead a join of that spec capability with the `Devices` **status**
ledger. Use `slicedDetail` to answer "what is this pool made of", and the node keys or the `PT` view to
answer "what is still free". Reading the capability as a remainder is the easiest mistake to make here.

### 3. Mixed — part logical, part physical

Disabling MIG on five of the eight cards leaves three partitioned. This is the configuration that shows
the disjoint populations directly, because **both** families are advertised by one node at once:

```console
# On node-h100 (via SSH):
$ for i in 3 4 5 6 7; do sudo nvidia-smi -i "$i" -mig 0; done
$ kubectl -n gpustack-system rollout restart ds/gpustack-operator-device-manager-nvidia

$ sudo nvidia-smi --query-gpu=index,mig.mode.current --format=csv
index, mig.mode.current
0, Enabled
1, Enabled
2, Enabled
3, Disabled
4, Disabled
5, Disabled
6, Disabled
7, Disabled
```

```console
$ kubectl get instancetypes gpustack--nvidia-h100-80gb-hbm3-linux-amd64
NAME                                          ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU   PHASE
gpustack--nvidia-h100-80gb-hbm3-linux-amd64   gpustack-fnv64-e4768a65ca0ce96b   12/192Gi/100Gi          5/5 50/50 100/500 7/21   0/0   Active
```

`5/5 50/50 100/500` covers the **five** whole cards; `7/21` covers the **three** partitioned ones
(`3 × 7`). No card is counted twice, and no card is missing:

```console
$ kubectl get node node-h100 -o json | jq '.status.allocatable | with_entries(select(.key|test("nvidia.com/gpu")))'
{
  "nvidia.com/gpu": "5",
  "nvidia.com/gpu.partitioned": "21",
  "nvidia.com/gpu.partitioned.mig-1g.10gb": "21",
  "nvidia.com/gpu.partitioned.mig-1g.20gb": "12",
  "nvidia.com/gpu.partitioned.mig-2g.20gb": "9",
  "nvidia.com/gpu.partitioned.mig-3g.40gb": "6",
  "nvidia.com/gpu.partitioned.mig-4g.40gb": "3",
  "nvidia.com/gpu.partitioned.mig-7g.80gb": "3",
  "nvidia.com/gpu.partitioned.units": "4800k",
  "nvidia.com/gpu.shared": "50",
  "nvidia.com/gpu.sliced": "640",
  "nvidia.com/gpu.sliced.cores-percentage": "64k",
  "nvidia.com/gpu.sliced.memory-mib": "409600",
  "nvidia.com/gpu.sliced.memory-percentage": "500",
  "nvidia.com/gpu.sliced.units": "8M"
}
```

Every logical number is now **five** cards' worth (`640 = 5 × 128`, `8M = 5 × 1600k`,
`409600 MiB = 5 × 81920`) and every partition number **three** (`21 = 3 × 7`, `4800k = 3 × 1600k`). The
`Devices` capability names which card is in which population:

```console
$ kubectl get devices node-h100 -o jsonpath='{.spec.groups[0].accelerators[*].status}' | jq -c
group NVIDIA H100 80GB HBM3: 8 card(s); logically sliceable indices [3, 4, 5, 6, 7]; partitioned indices [0, 1, 2]
  card 0 partitioned:    count=7 profiles=1g.10gb(7),1g.20gb(4),2g.20gb(3),3g.40gb(2),4g.40gb(1),7g.80gb(1)
  card 1 partitioned:    count=7 profiles=1g.10gb(7),1g.20gb(4),2g.20gb(3),3g.40gb(2),4g.40gb(1),7g.80gb(1)
  card 2 partitioned:    count=7 profiles=1g.10gb(7),1g.20gb(4),2g.20gb(3),3g.40gb(2),4g.40gb(1),7g.80gb(1)
  card 3 logicalSliced:  {"coresPercentageOvercommit": true, "count": 128}
  …                      (cards 4-7 identical)
```

#### 3.1 Both kinds of workload, side by side

A partition request and a logical-slice request submitted together land on **different** cards, because
the resource names they carry are advertised by disjoint populations — the scheduler cannot even
consider a card that does not offer the key:

```console
$ kubectl get pods
NAME                  READY   STATUS    RESTARTS   AGE
mixed-logical         1/1     Running   0          5s
mixed-partition       1/1     Running   0          9s

$ kubectl exec mixed-partition -- nvidia-smi -L
GPU 0: NVIDIA H100 80GB HBM3 (UUID: GPU-950792bf-a01c-3f1a-e122-3473e67f54b2)
  MIG 3g.40gb     Device  0: (UUID: MIG-b3061c09-2a4c-5026-a575-79f86a5bb12c)

$ kubectl exec mixed-logical -- nvidia-smi -L
GPU 0: NVIDIA H100 80GB HBM3 (UUID: GPU-4e24fa00-9f4f-68c4-bfcd-581cd3cb7de6)
```

The partition workload sees **only its MIG device**; the logical-slice workload sees a **whole card**
(capped at runtime by the preload library, not by what `nvidia-smi -L` lists). The allocation record
names the card each landed on:

```console
$ kubectl get pod <name> -o jsonpath='{.metadata.annotations.device\.gpustack\.ai/accelerator\.allocated}'
mixed-partition  -> index=0  id=GPU-950792bf-…  profile=3g.40gb     # a PARTITIONED card
mixed-logical    -> index=5  id=GPU-4e24fa00-…  profile=-           # a WHOLE card
```

Both views move, independently, in their own family:

```console
$ kubectl get instancetypes gpustack--nvidia-h100-80gb-hbm3-linux-amd64
NAME                                          ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU   PHASE
gpustack--nvidia-h100-80gb-hbm3-linux-amd64   gpustack-fnv64-e4768a65ca0ce96b   12/192Gi/100Gi          4/4 40/40 100/450 7/17   0/0   Active
```

- `EX 5→4` and `SH 50→40`: the logical slice put card 5 in use, so one fewer whole card can be claimed.
- `SL 500→450`: the slice took 50 % of card 5; `450 = 4 × 100 + 50`.
- `PT 21→17`: card 0 now holds a `3g.40gb`, leaving three `1g.10gb` slots; `17 = 2 × 7 + 3`.

```console
$ kubectl get node node-h100 -o json | jq '.status.allocatable | with_entries(select(.key|test("gpu\\.partitioned")))'
{
  "nvidia.com/gpu.partitioned": "18",
  "nvidia.com/gpu.partitioned.mig-1g.10gb": "17",
  "nvidia.com/gpu.partitioned.mig-1g.20gb": "10",
  "nvidia.com/gpu.partitioned.mig-2g.20gb": "7",
  "nvidia.com/gpu.partitioned.mig-3g.40gb": "6",
  "nvidia.com/gpu.partitioned.mig-4g.40gb": "2",
  "nvidia.com/gpu.partitioned.mig-7g.80gb": "2",
  "nvidia.com/gpu.partitioned.units": "4800k"
}
```

Only the partitioned cards' keys moved — the five whole cards' `.sliced.*` keys are untouched, because
the `3g.40gb` was carved on a card that is not in their population.

The placement is not luck: a partition token only exists on a partitioned card and a `.sliced` token
only on a whole one, so the resource name rules out the wrong population before the kubelet is
involved. This is the failure the split exists to remove — a single pool used to let the kubelet hand a
partition request a token from a card that could not host one, and the Pod died with a terminal
`UnexpectedAdmissionError`.

### 4. Back to all-logical

Reverse the runbook ([Disabling MIG on a node](#disabling-mig-on-a-node)): with the cards idle, flip the
remaining modes off and re-detect. The `partitioned` keys go to zero, the logical capability keys
return, and the pool is back to where [step 1](#1-all-logical--every-card-mig-off) started:

```console
# On node-h100 (via SSH):
$ for i in 0 1 2; do sudo nvidia-smi -i "$i" -mig 0; done
Disabled MIG Mode for GPU 00000000:8D:00.0
All done.
…

$ kubectl -n gpustack-system rollout restart ds/gpustack-operator-device-manager-nvidia
daemonset.apps/gpustack-operator-device-manager-nvidia restarted

$ kubectl get instancetypes gpustack--nvidia-h100-80gb-hbm3-linux-amd64
NAME                                          ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU   PHASE
gpustack--nvidia-h100-80gb-hbm3-linux-amd64   gpustack-fnv64-e4768a65ca0ce96b   12/192Gi/100Gi          8/8 80/80 100/800 0/0   0/0   Active
```

---

## What GPUStack Operator does *NOT* do

- It does not enable, disable, or reconfigure MIG *mode* — those are `nvidia-smi` operations you run. (It
  *does* create and destroy the GPU/compute *instances* that back scheduled workloads; see
  [Requesting a MIG instance](#requesting-a-mig-instance).)
- It does not trigger on nodeconfig or labels, does not flip MIG mode automatically, and does not rewrite
  the mode geometry.
- It does not deschedule or evict Pods when MIG *mode* changes.
- It does not account for an instance you carved by hand. (It *does* delete it: an instance no allocation
  accounts for is reclaimed as an orphan once its card is idle — see [Limitations](#limitations).)

A capability change reaches the cluster **only** through Device Manager restart or re-detection.

---

**See also** — [Accelerator Requests](../accelerator-requests.md) (the full key set and request rules) ·
[Admission](../architecture/admission.md#capability-versus-availability) (which status field answers
"what can I still get") · [Device Discovery](../architecture/discovery.md#the-partitioned-family-fungible-tokens)
(how a partition is placed and reclaimed)

**Next** → [Walkthrough](../walkthrough.md) — the logical-slicing counterpart on a live cluster.
