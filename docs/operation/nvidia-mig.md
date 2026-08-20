# NVIDIA MIG Operations

> **Purpose** — the runbook for MIG *mode* on a node (enable, disable, reboot recovery), the contract for
> requesting a MIG instance, and a recorded three-configuration walkthrough.
> **Audience** operators, users requesting partitions · **Prerequisites** [Accelerator
> Requests](../accelerator-requests.md) · **Read time** ~21 min

You drive MIG **mode** with `nvidia-smi`; GPUStack catches up when the node's Device Manager re-detects.
NVIDIA **MIG** (Multi-Instance GPU) mode is a **manually managed** node property: the operator *observes*
the geometry into the `Devices` ledger and the advertised partitioning capability, but never enables,
disables or reconfigures it. It *does* **dynamically allocate** the instances that back scheduled workloads
— see [Requesting a MIG instance](#requesting-a-mig-instance).

A capability change enters the cluster **only** through Device Manager re-detection, and the detect loop's
trigger watches the device set and health, not the partitioning mode, so a mode toggle needs a **DaemonSet
restart** (see [what it does *NOT* do](#what-gpustack-operator-does-not-do)).

MIG is GPUStack's one implemented **physical partitioning** backend, a resource family of its own
(`nvidia.com/gpu.partitioned*`) disjoint from the logical (software) slicing family `nvidia.com/gpu.sliced*`:
a MIG-enabled GPU serves *only* partition requests, an unpartitioned GPU *only* whole-GPU, shared and
logical-slice requests. The full key set and request rules live in
[Accelerator Requests](../accelerator-requests.md).

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

A MIG-enabled GPU is partitioned into instances from a fixed profile set. A100-40GB and H100-80GB both
expose **7 compute (SM) slices** and **8 memory slices** — 5 GB each on A100-40GB, 10 GB on H100-80GB — so
each row below pairs the two products' names for one shape.

| Profile (A100-40GB / H100-80GB) | Memory | Compute slices (of 7) | Memory slices (of 8) | Legal starts | Max instances/GPU |
|---|---|---|---|---|---|
| 1g.5gb / 1g.10gb  | 5 / 10 GB  | 1 | 1 | 0–6 | 7 |
| 1g.10gb / 1g.20gb | 10 / 20 GB | 1 | 2 | 0/2/4/6 | 4 |
| 2g.10gb / 2g.20gb | 10 / 20 GB | 2 | 2 | 0/2/4 | 3 |
| 3g.20gb / 3g.40gb | 20 / 40 GB | 3 | 4 | 0 or 4 | 2 |
| 4g.20gb / 4g.40gb | 20 / 40 GB | 4 | 4 | 0 | 1 |
| 7g.40gb / 7g.80gb | 40 / 80 GB | 7 | 8 | 0 | 1 |

> **The `+me` / `+me.all` / `+gfx` variants are not supported.** A Kubernetes resource name may not contain
> `+`, and GPUStack never rewrites a name to make it key-safe: one that is not already a valid resource-name
> segment is **excluded** from the inventory, so a key always maps back by a plain prefix strip. They reach
> no `Devices` ledger, capacity key or `InstanceType` inventory, and cannot be requested.

> **A partition is one whole GPU instance; subdividing it is not supported.** Every profile above is created
> as a GPU instance plus a single compute instance covering all of it. GPUStack addresses a partition by one
> device id, and a GPU instance you subdivide by hand (`nvidia-smi mig -cci`) has several — a share of the
> compute each, all on the same memory — so it is refused rather than half-addressed.

Instances occupy **hardcoded placement slots** — the *legal starts* above, in memory-slice units — and a
combination is legal only when the occupied intervals do not overlap. A profile's **max instances/GPU is the
length of its start list**.

**Two profiles covering the same slices need not get the same placement freedom.** A `4g` covers four slices
as a `3g` does, yet gets only one slot; a `2g` covers two as the same-size `1g` does, yet gets three slots to
its four. So a live instance removes slots from other profiles: a `3g` at 0–3 takes the GPU's only `4g` slot
with it, while leaving room for a second `3g` at 4–7.

The `Devices` ledger reports each MIG GPU's inventory as static per-profile *counts* (the maximum it could
host) plus a **placement-aware `Remaining`**, how many still fit (see
[Requesting a MIG instance](#requesting-a-mig-instance)).

Full tables for every MIG-capable GPU, other memory variants and `+me` / `+gfx` included, are in NVIDIA's
[Supported MIG Profiles](https://docs.nvidia.com/datacenter/tesla/mig-user-guide/supported-mig-profiles.html).

## Requesting a MIG instance

A workload asks for **one hardware instance of a named profile**. GPUStack materializes it and injects
only the MIG device: no logical-slicing runtime (`libvgpu.so`), no fractional translation. The
**request shape**: one partitioned GPU plus the profile, one instance per Pod:

```yaml
resources:
  limits:
    nvidia.com/gpu.partitioned: "1"              # always exactly 1 GPU
    nvidia.com/gpu.partitioned.mig-1g.10gb: "1"  # always exactly 1 instance of this profile
```

- `mig` is NVIDIA's name for hardware partitioning, carried as `<kind>`; another manufacturer registers its
  own under the same `.partitioned.<kind>-<profile>` shape.
- **Name the exact profile** from [Supported profiles](#supported-profiles) (e.g. `1g.10gb`). There is no
  memory→profile translation: you request `mig-<profile>`, not a fraction or a MiB amount.
- Both keys **must be exactly `1`** — a scope decision, not a hardware limit; a multi-partition workload
  asks for several Pods.
- The partition family is **mutually exclusive** with every other accelerator family Pod-wide (a Pod also
  asking for a whole GPU, a shared unit or a logical slice is rejected at admission), it is **one profile
  shape per Pod**, and the accelerator claims must sit in **one container group**. The full rule set, with an
  accepted and a rejected example, is in [Accelerator Requests](../accelerator-requests.md#the-request-rules).
- **`nvidia.com/gpu.partitioned.units` is webhook-derived** — do not set it. The webhook folds the profile's
  VRAM into it, and quota is charged on the manufacturer's `credits` resource folded from it, so a `3g.40gb`
  and a logical `40Gi` slice cost the same.
- A MIG request must go through **Kueue** — a Pod (or workload) on a `LocalQueue`, or an `Instance` (below).
  Rules and folds bind only Pods carrying the `kueue.x-k8s.io/queue-name` label.

**Through the `Instance` API.** `spec.resources.acceleratorPartitionedProfile` names the profile, and the
controller shapes it into the two keys above:

```yaml
kind: Instance
metadata:
  name: mig-instance
spec:
  type: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
  image: ubuntu:24.04
  command: ["sleep", "86400"]
  resources:
    accelerator: "1"                       # a partition is always one GPU
    acceleratorPartitionedProfile: 3g.40gb
  volume:
    ephemeral:
      capacity: 1Gi
```

- The profile is validated at admission against the pool's inventory: one it does not offer is rejected with
  the offered set in the message, never left Pending.
- It excludes `acceleratorSlicedMemoryPercentage` / `acceleratorSlicedCoresPercentage` — the two cannot both
  apply to one GPU.
- Host CPU and RAM are sized by the profile's share of the GPU's VRAM, so a `1g` partition does not ask for
  a whole GPU's worth.

**GPU selection is the plugin's, not the kubelet's.** A `.partitioned` token is a fungible count, unlike the
three GPU-bound families: the plugin picks the GPU against the live partition geometry and records it. It
**packs** — the most-occupied GPU that still fits wins, keeping a sibling whole for a later whole-GPU
profile — so a rejection from `Allocate` means the node has no room, not a wrong GPU from the kubelet.

**Scheduling and the `Remaining` ledger.** Each MIG GPU advertises `allocated + remaining` per profile,
mirrored onto the node as `nvidia.com/gpu.partitioned.mig-<profile>`. Admission is placement-aware: the
per-GPU AdmissionCheck admits a Pod only while the requested profile has a free slot, and **retries** (never
hard-rejects) while the ledger is unpopulated.

> **Why both terms** — publishing only what is free would make the scheduler subtract every live
> instance twice; see [Scheduling Chain](../architecture/scheduling-chain.md#hardware-partitioning-capacities).

**Reclaim.** When the Pod exits, the operator destroys its compute instance then its GPU instance, and the
profile's `Remaining` restores within one reclaim cycle. A destroy racing a residual process returns
`NVML_ERROR_IN_USE`; the operator retries with bounds, never blocking siblings or other GPUs' allocations,
and logs a straggler past the bound. The node key frees on **Pod deletion**, ahead of the
hardware; see [Limitations](#limitations).

**An SSH-enabled `Instance` sees its partition, not the GPU.** The workload runs in `main`, the SSH server in
an `sshd` sidecar that `nsenter`s into `main`: the sidecar's device allocation is a device-cgroup grant and
nothing else, and the session inherits `main`'s environment. That grant is **the MIG instance `main` holds**,
never the parent GPU, which would expose every other partition on it.

The identity comes from the on-disk ownership marker the allocator writes with the instance — the record that
drives reuse and reclaim and survives a Device Manager restart. The read is **liveness-checked under the GPU
lock** before injection: the recorded GPU instance must still exist *and* carry the recorded MIG UUID, since
a destroyed instance's id can be reassigned.

A marker that is missing, names a different GPU, records a profile the GPU no longer offers, or whose instance
is gone or recreated fails the sidecar's allocation closed — no fallback to the GPU.

## Prerequisites

The Device Manager Pod must be processed by the **NVIDIA container runtime**, which the chart's
`runtimeClassName` plus its `NVIDIA_MIG_CONFIG_DEVICES` / `NVIDIA_MIG_MONITOR_DEVICES` declarations
arrange. Without them that runtime hides the driver's MIG capabilities from the Pod: carving fails with
`NO_PERMISSION`, and an instance carved outside the Pod is invisible to it.

Overriding either variable through `deviceManager.env`, or pointing the `nvidia` RuntimeClass handler
elsewhere, brings that back ([NVIDIA prerequisites](../vendor-prerequisites.md#nvidia)).

Before switching a GPU's MIG mode (enable or disable):

- The GPU's instances must be **idle**: stop the using Pod first and let no process hold the GPU. A family's
  tokens exist only while the GPU's capability reports it, so flipping the mode *removes* the old family's
  tokens.
- All daemons holding a driver handle (DCGM, `nvsm`, exporters, …) must be **stopped first**, or the switch
  hangs pending.
- The `nvidia_drm` kernel module must **not** be loaded, or the GPU reset fails.
- The operation requires **`CAP_SYS_ADMIN`**.
- **Ampere (A100/A30)** requires a **GPU reset** after the mode switch. **Hopper and newer** need no reset.

Once the mode is on, instance (GI/CI) create/destroy is dynamic and online: destroying one returns `IN_USE`
only when *that* instance has active processes; sibling workloads are unaffected.

## Limitations

- The administrator owns the *mode* lifecycle; GPUStack drives none of it, and evicts nothing when the mode
  changes (see [What GPUStack Operator does *NOT* do](#what-gpustack-operator-does-not-do)).
- **Carving an instance out of band is unsupported on a managed node**: let GPUStack materialize them, and
  it reuses any already on a GPU it manages. Every node-level number (per-profile capacity key, partition
  token health, AdmissionCheck) derives from the allocation annotations the device plugin writes, and
  `nvidia-smi mig -cgi` by hand produces none.

  So while a GPUStack workload holds the GPU the node advertises room it does not have, and that **never
  converges** — placement reads live NVML and will not double-book it, but the accounting above stays
  wrong. Once the GPU is drained the reverse happens: after its debounce the reclaimer **destroys** an
  instance no allocation accounts for as an orphan, including one it never created.

  An instance with something **running on it** is the exception: it is left alone and re-checked each
  cycle, so one you carved by hand survives for as long as you are using it.

- **A same-profile replacement submitted the instant its predecessor is deleted can fail to start.** Leave a
  gap between the delete and the replacement, or let the replacement's own restart handle it.

  Node accounting is rebuilt from Pod annotations, so a deleted Pod's slot reappears in the per-profile key
  and the healthy token count **immediately**, while the reclaimer destroys the hardware on its own
  debounce. A replacement admitted in that gap is handed the outgoing instance and fails to create
  (`failed to get device handle from UUID: Not Found`) or ends in `UnexpectedAdmissionError` — a startup
  failure, not corruption; a resubmit after the reclaim succeeds.
- **A managed provider may health-check the GPUs and reboot the node for you.** Some managed Kubernetes
  offerings probe the NVLink topology matrix for `N × (N-1)` healthy pairs; a partitioning-mode GPU drops out
  of it, the probe reads short, and the provider's node agent can cordon and restart the node. Check the
  provider's node conditions before blaming the operator, and note that anything you merely *stopped* during
  preparation (a telemetry service holding driver handles) returns on that reboot.
- On **Ampere** the mode persists across reboots (stored in InfoROM); on **Hopper and newer** it does not.
- MIG **instances never survive a reboot** on any generation; after re-enabling mode and restarting the
  Device Manager, **resubmit** the workloads that were running
  ([Node reboot recovery](#node-reboot-recovery)).
- In a **passthrough VM** the hypervisor may forbid the GPU reset entirely — reboot the node/VM instead.
- The per-profile node keys are independently subtracted scalars, so two Pods asking for mutually exclusive
  [placements](#supported-profiles) can both land on a node that hosts only one of them; the second fails
  its allocation closed and is retried.
- The `+me` / `+me.all` / `+gfx` profile variants are excluded from the inventory (see
  [Supported profiles](#supported-profiles)).
- **A hand-subdivided GPU instance stops the sweep for the whole node.** One carrying more than one compute
  instance (`nvidia-smi mig -cci`) cannot be addressed by a single device id, so its GPU is refused — and the
  reclaim pass, which lists every GPU before deciding anything, ends there. Nothing is reclaimed on any GPU
  of that node, with an error each pass, until that instance is back to one compute instance.
- **A profile the driver enumerates no legal placement for is excluded too**, as is one whose placement query
  failed; the GPU and profile ids are named in a warning. A profile's memory-slice span has no source but its
  placement records, and that span is what matches an instance's identity. Nothing is lost: such a profile
  would be a key whose allocation could never succeed.
- **The old per-profile key is gone.** A MIG profile used to be a `mig-<profile>` segment on the *logical*
  `nvidia.com/gpu.sliced`; it is now `nvidia.com/gpu.partitioned.mig-<profile>`, with no translation and not
  even a rejection by name — a Pod carrying the old shape never schedules. See
  [Pre-release breaks](../accelerator-requests.md#pre-release-breaks), which also explains why a node must be
  **drained before its device manager is upgraded**.

## Enabling MIG on a node

1. Satisfy the [Prerequisites](#prerequisites) — stop the using Pod and the driver-handle daemons.
2. Enable the mode with `nvidia-smi`, per GPU or for all GPUs:

   ```console
   # one GPU by index
   $ nvidia-smi -i <id> -mig 1
   # all GPUs
   $ nvidia-smi -mig 1
   ```

3. **Ampere:** perform the GPU reset; if `nvidia_drm` is loaded or a passthrough VM blocks it, reboot the
   node/VM instead. **Hopper and newer:** no reset.
4. Restart the node's Device Manager pod to re-detect. You need **not** delete the node's `Devices` object
   (the detector rewrites the group's capability in place), nor pre-create instances with
   `nvidia-smi mig -cgi` — the operator materializes them as workloads are admitted, reusing any you made.
5. Verify the `Devices` capability now reports those GPUs' MIG profiles and **zero logical slicing** — a
   hardware-partitioned GPU offers no software slicing.

### Across a fleet, with the NVIDIA GPU Operator

The steps above are per node and go through SSH. The NVIDIA GPU Operator ships a **MIG Manager** that does
the same job declaratively: each node carries an `nvidia.com/mig.config` label naming a profile, and the
manager makes that node's cards match it.

[Vendor Prerequisites](../vendor-prerequisites.md#nvidia) has that component off, because every other
profile hands it the instances as well and those are this operator's to create. Turning it back on with
`--set migManager.enabled=true` is what this section asks for, and the profile below is what keeps the
division of labour intact.

Use the profile named **`all-enabled`**. It turns MIG mode on and creates *no* instances — exactly the
division of labour this page describes, where the mode is yours and the instances are GPUStack's. Every
other shipped profile (`all-balanced`, `all-1g.5gb`, …) pre-creates a fixed geometry instead, and takes
that job away.

```console
$ kubectl label node <node> nvidia.com/mig.config=all-enabled --overwrite
```

The label is per node, so a fleet can mix freely: nodes labelled `all-enabled` serve partitions, nodes
labelled `all-disabled` — or carrying no label at all — stay whole-card.

**Restarting the Device Manager is still yours to do**, on every node whose label you changed, exactly as
in step 4 above. The MIG Manager restarts the GPU clients it owns, and this operator's Device Manager is
not one of them, so until it restarts the node keeps advertising the capability it detected before the
switch.

Three things to know before reaching for it:

- **Its default is `all-disabled`**, and the operator writes that label itself when the manager first
  runs. Arriving on a node whose cards you had already enabled by hand, it turns them back off.
- **It is placed only on nodes labelled `nvidia.com/mig.capable=true`**, which GPU Feature Discovery
  publishes. Without that component the DaemonSet exists but schedules nowhere; setting the label by hand
  is enough to place it.
- **It restarts the node's GPU clients, the kubelet included**, to apply a change, and marks
  `nvidia.com/mig.config.state=failed` when a service it wants to restart is masked — which is the state
  the readiness steps above deliberately leave DCGM in. Read the cards' own modes before believing that
  label.

## Disabling MIG on a node

Run the inverse: with no Pod using the GPU's instances, destroy them, then

```console
$ nvidia-smi -i <id> -mig 0   # or `-mig 0` for all GPUs
```

apply the same reset rules (Ampere reset or reboot; Hopper none), and restart the node's Device Manager pod
so the ledger returns the GPU to its whole-GPU / logical-slice capability.

## Node reboot recovery

If you did **not** reset MIG before a reboot ([Limitations](#limitations) says what persists):

- On the way back up the instances are gone (and, on Hopper+, so is the mode).
- Redo the [enable sequence](#enabling-mig-on-a-node), **including the Device Manager DaemonSet restart**
  — nothing else makes the post-reboot hardware reach the cluster.

A workload running before the reboot lost its instance, so **resubmit it** — delete and re-create the
Pod/workload, and the operator materializes a fresh instance on admission. A lingering pre-reboot Pod keeps
its ownership record but has no live instance, so its device allocation fails closed until it is recreated.

## Walkthrough: three MIG configurations on one node

A recorded run on a live Kubernetes cluster, in the style of the
[scheduling-chain walkthrough](../walkthrough.md): every command is the real `kubectl` (or on-node
`nvidia-smi`) invocation and its real output, on `node-h100`, a genericized node of **eight** H100-80GB GPUs
running operator defaults. Eight is the point: the families are served by **disjoint GPU populations**, so
partitioning *some* GPUs moves exactly those.

| | Configuration | GPUs in a partitioning mode |
|---|---|---|
| **1** | [All-logical](#1-all-logical--every-gpu-mig-off) | none |
| **2** | [All-physical](#2-all-physical--every-gpu-mig-on) | all 8 |
| **3** | [Mixed](#3-mixed--part-logical-part-physical) | 3 of 8 |

One column to keep in mind: `kubectl get instancetypes` shows the accelerator four-view
**EX**clusive / **SH**ared / **SL**iced (logical) / **PT** (physically partitioned) as
`onceMaxRequest/remaining` groups. A GPU feeds one side of that split, so partitioning it **moves** capacity
from the first three groups to the fourth rather than adding to it. The first three are credit-based (each
GPU's credit budget); `PT` counts hardware instances the GPUs can still host.

### 1. All-logical — every GPU MIG off

Every GPU offers logical slicing and none offers partitions: three logical views populated, partition view
empty:

```console
$ kubectl get instancetypes
NAME                                          ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU       PHASE
gpustack--generic-linux-amd64                 gpustack-fnv64-3b93966fd73eb9ec   1/2Gi/100Gi             0/0 0/0 0/0 0/0            128/132   Active
gpustack--nvidia-h100-80gb-hbm3-linux-amd64   gpustack-fnv64-e4768a65ca0ce96b   12/192Gi/100Gi          8/8 80/80 100/800 0/0      0/0       Active
```

The accelerated row reads **8** whole GPUs, **80** shares (10 per GPU), **800 %** of logical slice budget
(100 % per GPU), **no** partition capacity. The node advertises the logical key family, and
`nvidia.com/gpu.partitioned` at 0 keeps every `.partitioned.*` counting key absent:

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

Every number is per-GPU × 8: `1024 = 8 × 128` logical slices, `12800k = 8 × 1600k` credits,
`655360 MiB = 8 × 81920`. The `Devices` capability (in `spec`; the runtime ledger is in `status`) says the
same, GPU by GPU:

```console
$ kubectl get devices node-h100 -o jsonpath='{.spec.groups[0].accelerators[*].status}' | jq -c
group NVIDIA H100 80GB HBM3: 8 GPU(s); logically sliceable indices [0, 1, 2, 3, 4, 5, 6, 7]; partitioned indices -
  GPU 0 logicalSliced: {"coresPercentageOvercommit": true, "count": 128}
  …                    (GPUs 1-7 identical)

$ kubectl get instancetypes gpustack--nvidia-h100-80gb-hbm3-linux-amd64 -o jsonpath='{.status.detail.slicedDetail}' | jq
{
  "logical": { "coresPercentageOvercommit": true, "count": 1024 },
  "physical": {}
}
```

### 2. All-physical — every GPU MIG on

#### 2.1 Enable the mode, then re-detect

Following the [runbook](#enabling-mig-on-a-node), flip the mode with `nvidia-smi` on the node itself (Hopper
needs no reset). GPUStack never runs this:

```console
# On node-h100 (via SSH), the GPUs idle and driver-handle daemons stopped:
$ for i in $(seq 0 7); do sudo nvidia-smi -i "$i" -mig 1; done
Enabled MIG Mode for GPU 00000000:8D:00.0
All done.
…                                                    (GPUs 1-7 identical)
```

The cluster does **not** react yet. Restart the DaemonSet pod; deleting the node's `Devices` object is
**not** required (it was, in earlier builds):

```console
$ kubectl -n gpustack-system rollout restart ds/gpustack-operator-device-manager-nvidia
daemonset.apps/gpustack-operator-device-manager-nvidia restarted

$ kubectl -n gpustack-system rollout status ds/gpustack-operator-device-manager-nvidia
daemon set "gpustack-operator-device-manager-nvidia" successfully rolled out
```

> **Wait for the device plugin to re-register, not just for the rollout.** The operator republishes the
> node's capacity keys as soon as the ledger changes, *earlier* than the kubelet has the plugin's new token
> set, so a Pod admitted in that gap is allocated against the old registration and dies with
> `UnexpectedAdmissionError`. Wait for the new pod to be Ready **and** the `.partitioned` key to appear,
> then give the kubelet a moment.

#### 2.2 The whole node changes families

All three logical views drop to zero and the partition view takes over. Not cosmetic: an exclusive tenant on
a MIG-mode GPU would get a device CUDA cannot use, so the views report what is servable.

```console
$ kubectl get instancetypes
NAME                                          ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU       PHASE
gpustack--generic-linux-amd64                 gpustack-fnv64-3b93966fd73eb9ec   1/2Gi/100Gi             0/0 0/0 0/0 0/0            128/132   Active
gpustack--nvidia-h100-80gb-hbm3-linux-amd64   gpustack-fnv64-e4768a65ca0ce96b   12/192Gi/100Gi          0/0 0/0 0/0 1/56           0/0       Active
```

`PT 1/56` reads: **1** is the most one request can ask for, **56** what the node can still host (`8 × 7`).
Read `1` as "there is room", `0` as "none"; [2.5](#25-where-those-numbers-come-from) explains both.

The keys flip families wholesale: every `.sliced.*` key is gone, and one `partitioned.mig-<profile>` key
appears per profile alongside `.partitioned.units`, which values a partitioned GPU at a whole GPU's credits
just as `.sliced.units` valued a logically sliceable one:

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

Each per-profile key is 8 × that profile's count in the [profile table](#supported-profiles): `56 = 8 × 7`
one-slice instances, `8 = 8 × 1` whole-GPU ones. `nvidia.com/gpu`, `.shared` and `.sliced` read `0` rather
than disappearing — a device-plugin pool key zeroes out, never gets removed.

```console
$ kubectl get devices node-h100 -o jsonpath='{.spec.groups[0].accelerators[*].status}' | jq -c
group NVIDIA H100 80GB HBM3: 8 GPU(s); logically sliceable indices -; partitioned indices [0, 1, 2, 3, 4, 5, 6, 7]
  GPU 0 partitioned: count=7 profiles=1g.10gb(7),1g.20gb(4),2g.20gb(3),3g.40gb(2),4g.40gb(1),7g.80gb(1)
  …                  (GPUs 1-7 identical)
```

#### 2.3 Request a MIG instance

Submit a Pod on the pool's entrance `LocalQueue` (an `Instance` with
`acceleratorPartitionedProfile: 3g.40gb` gives the same two keys):

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
          nvidia.com/gpu.partitioned: "1"              # one partitioned GPU
          nvidia.com/gpu.partitioned.mig-3g.40gb: "1"  # one 3g.40gb instance on it
```

Kueue admits it, the node-devices AdmissionCheck confirms a free `3g.40gb` placement, and the plugin
**materializes** the GPU/compute instance pair on a GPU it selects, injecting only that device:

```console
$ kubectl get pod mig-demo
NAME       READY   STATUS    RESTARTS   AGE
mig-demo   1/1     Running   0          8s

$ kubectl exec mig-demo -- nvidia-smi -L
GPU 0: NVIDIA H100 80GB HBM3 (UUID: GPU-950792bf-a01c-3f1a-e122-3473e67f54b2)
  MIG 3g.40gb     Device  0: (UUID: MIG-b3061c09-2a4c-5026-a575-79f86a5bb12c)
```

#### 2.4 What one instance costs the node

One `3g.40gb` occupies memory slices 0–3 of **one** GPU, so only that GPU's arithmetic changes, per profile:

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

Every number is `Σ over GPUs of (allocated + remaining)`, which makes them subtract correctly
([why both terms](#requesting-a-mig-instance)):

| Profile | 7 untouched GPUs | the carved GPU | total |
|---|---|---|---|
| `1g.10gb` | `7 × 7 = 49` | slices 4–7 free → 3 | **52** |
| `1g.20gb` | `7 × 4 = 28` | slots `{4,2}`, `{6,2}` → 2 | **30** |
| `2g.20gb` | `7 × 3 = 21` | slot `{4,2}` → 1 | **22** |
| `3g.40gb` | `7 × 2 = 14` | 1 allocated + 1 remaining | **16** |
| `4g.40gb` | `7 × 1 = 7` | its one slot is taken → 0 | **7** |
| `7g.80gb` | `7 × 1 = 7` | needs all 8 slices → 0 | **7** |

Read the `3g.40gb` row as `allocated (1) + remaining (1)`: the scheduler subtracts `mig-demo`'s request and
sees **one** more fitting there. `7g.80gb` and `4g.40gb` each lost exactly the carved GPU; `3g.40gb` lost
nothing — [2.5](#25-where-those-numbers-come-from) walks why.

Deleting the Pod releases the credits immediately; the hardware and the profile keys follow within one
[reclaim cycle](#requesting-a-mig-instance).

> **Do not re-request the freed slot instantly.** The accounting frees on Pod deletion while the hardware
> dies on the reclaimer's debounce, so a replacement submitted in that gap can be handed an instance about
> to disappear — see [Limitations](#limitations).

#### 2.5 Where those numbers come from

Four kinds of number count that one carved GPU, and they disagree — deliberately.

**Per GPU it is interval overlap, nothing more.** A profile may only start at one of its
[hardcoded slots](#supported-profiles), and an instance fits when its interval overlaps nothing already
taken — no device access, no driver call:

```text
the carved GPU's 8 memory slices

  [0][1][2][3]  taken by the live 3g.40gb          [4][5][6][7]  free
```

Every profile is re-counted against that occupied interval:

| Profile | Slot size | Legal starts | Blocked by the live `3g.40gb` | Still free | Adds to *that profile's* key |
|---|---|---|---|---|---|
| `1g.10gb` | 1 | 0 1 2 3 4 5 6 | 0, 1, 2, 3 | 4, 5, 6 | 3 |
| `1g.20gb` | 2 | 0 2 4 6 | 0, 2 | 4, 6 | 2 |
| `2g.20gb` | 2 | 0 2 4 | 0, 2 | 4 | 1 |
| `3g.40gb` | 4 | 0 4 | — (start 0 *is* the live one) | 4 | 1 allocated + 1 free |
| `4g.40gb` | 4 | 0 | 0 | — | 0 |
| `7g.80gb` | 8 | 0 | 0 | — | 0 |

One row repays a second read: `4g.40gb` contributes **0 allocated**, not 1, since the ledger keys an
allocation by the profile actually built.

**The four numbers, and what each sums** over the node's seven untouched GPUs plus the carved one:

| Number | Value here | Sums, over the node's partitioned GPUs | Why that shape |
|---|---|---|---|
| `…partitioned.mig-<profile>` | the table above | that profile's `allocated + remaining` | the scheduler subtracts existing requests, so the key must include them |
| `nvidia.com/gpu.partitioned` | `53` | `allocated +` the GPU's **largest** per-profile free count | one pool key for the family, same scheduler-fit reason |
| `InstanceType` `PT` remaining | `52` | that largest free count alone, **no allocated term** | a user-facing "how many more can I start" |
| `InstanceType` `PT` onceMaxRequest | `1` | not a sum: `1` while any GPU can host an instance, else `0` | one request builds one instance on one GPU, and both ingress paths reject any other count |

`53` and `52` differ by exactly the one live instance, which only the pool key carries. And `onceMaxRequest`
is `1`, not `52`, because no single request could consume the node's remainder — nor even two instances.

**A GPU's contribution is a maximum, never a sum.** The carved GPU contributes `3` to those last three
numbers, not `3 + 2 + 1 + 1 = 7`: its profiles compete for the same slices, so creating one consumes
placements of the others. Three `1g.10gb`, or fewer larger ones — never both.

**The capability snapshot does not move at all.** `status.detail.slicedDetail` still reports
`physical.count: 56` and `4g.40gb: 8` after the carve, unchanged from
[step 2.2](#22-the-whole-node-changes-families): it derives only from the `Devices` **spec**, the detector's
record of what these GPUs could host when empty, and never reads the runtime ledger. Every number in the
table above joins that spec capability with the `Devices` **status** ledger.

So read `slicedDetail` as "what is this pool made of", never as a remainder — the node keys and `PT` answer
"what is still free".

### 3. Mixed — part logical, part physical

Disabling MIG on five of the eight leaves three partitioned — the configuration where **both** families are
advertised at once:

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

`5/5 50/50 100/500` covers the **five** whole GPUs, `7/21` the **three** partitioned ones (`3 × 7`); no GPU
is counted twice or missing:

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

Every logical number is now **five** GPUs' worth (`640 = 5 × 128`, `8M = 5 × 1600k`,
`409600 MiB = 5 × 81920`), every partition number **three** (`21 = 3 × 7`, `4800k = 3 × 1600k`). The
`Devices` capability names which GPU is in which:

```console
$ kubectl get devices node-h100 -o jsonpath='{.spec.groups[0].accelerators[*].status}' | jq -c
group NVIDIA H100 80GB HBM3: 8 GPU(s); logically sliceable indices [3, 4, 5, 6, 7]; partitioned indices [0, 1, 2]
  GPU 0 partitioned:     count=7 profiles=1g.10gb(7),1g.20gb(4),2g.20gb(3),3g.40gb(2),4g.40gb(1),7g.80gb(1)
  GPU 1 partitioned:     count=7 profiles=1g.10gb(7),1g.20gb(4),2g.20gb(3),3g.40gb(2),4g.40gb(1),7g.80gb(1)
  GPU 2 partitioned:     count=7 profiles=1g.10gb(7),1g.20gb(4),2g.20gb(3),3g.40gb(2),4g.40gb(1),7g.80gb(1)
  GPU 3 logicalSliced:   {"coresPercentageOvercommit": true, "count": 128}
  …                      (GPUs 4-7 identical)
```

#### 3.1 Both kinds of workload, side by side

A partition request and a logical-slice request submitted together land on **different** GPUs: their
resource names come from disjoint populations, and the scheduler cannot consider a GPU that lacks the key:

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

The partition workload sees **only its MIG device**, the logical-slice workload a **whole GPU** (capped at
runtime by the preload library, not by `nvidia-smi -L`). The allocation record names each GPU:

```console
$ kubectl get pod <name> -o jsonpath='{.metadata.annotations.device\.gpustack\.ai/accelerator\.allocated}'
mixed-partition  -> index=0  id=GPU-950792bf-…  profile=3g.40gb     # a PARTITIONED GPU
mixed-logical    -> index=5  id=GPU-4e24fa00-…  profile=-           # a WHOLE GPU
```

Both views move independently, in their own family:

```console
$ kubectl get instancetypes gpustack--nvidia-h100-80gb-hbm3-linux-amd64
NAME                                          ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU   PHASE
gpustack--nvidia-h100-80gb-hbm3-linux-amd64   gpustack-fnv64-e4768a65ca0ce96b   12/192Gi/100Gi          4/4 40/40 100/450 7/17   0/0   Active
```

- `EX 5→4` and `SH 50→40`: the logical slice put GPU 5 in use, leaving one fewer whole GPU to claim.
- `SL 500→450`: the slice took 50 % of GPU 5; `450 = 4 × 100 + 50`.
- `PT 21→17`: GPU 0 now holds a `3g.40gb`, leaving three `1g.10gb` slots; `17 = 2 × 7 + 3`.

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

Only the partitioned GPUs' keys moved: the `3g.40gb` was carved outside the whole GPUs' population, so their
`.sliced.*` keys are untouched. The placement is not luck — a partition token exists only on a
partitioned GPU, a `.sliced` token only on a whole one, so the resource name rules out the wrong population
before the kubelet is involved.

> **Why the split exists** — a single pool used to let the kubelet hand a partition request a token from a
> GPU that could not host one, and the Pod died with a terminal `UnexpectedAdmissionError`.

### 4. Back to all-logical

Reverse the runbook ([Disabling MIG on a node](#disabling-mig-on-a-node)): with the GPUs idle, flip the
remaining modes off and re-detect. The `partitioned` keys go to zero, the logical keys return, and the pool
is back at [step 1](#1-all-logical--every-gpu-mig-off):

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

## What GPUStack Operator does *NOT* do

- Enable, disable or reconfigure MIG *mode* — `nvidia-smi` operations you run.
- Trigger on nodeconfig or labels, flip the mode automatically, rewrite its geometry, or deschedule and
  evict Pods on a *mode* change.
- Account for an instance you carved by hand — though it *does* delete it as an orphan once its GPU is
  idle and nothing is running on it ([Limitations](#limitations)).
- Hand out a *subdivided* instance, or subdivide one itself — it creates a GPU instance with a single
  compute instance covering all of it, and refuses one somebody else subdivided
  ([Supported profiles](#supported-profiles)).

It *does* create and destroy the *instances* backing scheduled workloads
([Requesting a MIG instance](#requesting-a-mig-instance)).

---

**See also** — [Accelerator Requests](../accelerator-requests.md) (keys and request rules) ·
[Admission](../architecture/admission.md#capability-versus-availability) (which status field answers
"what can I still get") · [Device Discovery](../architecture/device-discovery.md#the-partitioned-family-fungible-tokens)
(how a partition is placed and reclaimed)

**Next** → [Walkthrough](../walkthrough.md) — the logical-slicing counterpart on a live cluster.
