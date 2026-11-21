# NVIDIA MIG Operations

You drive MIG **mode** with `nvidia-smi`; GPUStack catches up when the node's Device Manager re-detects.
NVIDIA *Multi-Instance GPU (MIG)* mode is a **manually managed** node property: the operator observes
the geometry into the `Devices` ledger and the advertised partitioning capability, but never enables,
disables or reconfigures it. It does dynamically allocate the instances that back scheduled workloads;
see [Requesting a partition](#requesting-a-partition).

A capability change enters the cluster **only** through Device Manager re-detection, and the detect loop's
trigger watches the device set and health, not the partitioning mode, so a mode toggle needs a **`DaemonSet`
restart** (see [Operator boundaries](#operator-boundaries)).

MIG is GPUStack's one implemented **physical partitioning** backend, a resource family of its own
(`nvidia.com/gpu.partitioned*`) disjoint from the logical (software) slicing family `nvidia.com/gpu.sliced*`:
a MIG-enabled GPU serves only partition requests, and an unpartitioned GPU only whole-GPU, shared and
logical-slice requests. The full key set and request rules live in
[Accelerator Requests](requests.md).

## Contents

- [Prerequisites](#prerequisites)
- [Partition profile discovery](#partition-profile-discovery)
- [Requesting a partition](#requesting-a-partition)
- [Limitations](#limitations)
- [Enabling partitioning](#enabling-partitioning)
- [Disabling partitioning](#disabling-partitioning)
- [Node reboot recovery](#node-reboot-recovery)
- [Operator boundaries](#operator-boundaries)
- [Three MIG configurations](#three-mig-configurations)

## Prerequisites

The Device Manager `Pod` must be processed by the **NVIDIA container runtime**, which the chart's
*`runtimeClassName`* plus its `NVIDIA_MIG_CONFIG_DEVICES` / `NVIDIA_MIG_MONITOR_DEVICES` declarations
arrange. Without them that runtime hides the driver's MIG capabilities from the `Pod`: carving fails with
`NO_PERMISSION`, and an instance carved outside the `Pod` is invisible to it.

Overriding either variable through `deviceManager.env`, or pointing the `nvidia` `RuntimeClass` handler
elsewhere, brings that back ([NVIDIA prerequisites](../../getting-started/vendor-prerequisites.md#nvidia)).

Before switching a GPU's MIG mode (enable or disable):

- The GPU's instances must be **idle**: stop the using `Pod` first and let no process hold the GPU. A family's
  tokens exist only while the GPU's capability reports it, so flipping the mode *removes* the old family's
  tokens.
- All daemons holding a driver handle (DCGM, `nvsm`, exporters, …) must be **stopped first**, or the switch
  hangs pending.
- The `nvidia_drm` kernel module must **not** be loaded, or the GPU reset fails.
- The operation requires **`CAP_SYS_ADMIN`**.
- **Ampere (A100/A30)** requires a **GPU reset** after the mode switch. **Hopper and newer** need no reset.

Once the mode is on, instance (GI/CI) create/destroy is dynamic and online: destroying one returns `IN_USE`
only when *that* instance has active processes; sibling workloads are unaffected.

## Partition profile discovery

**Profiles come from a fixed per-product set, not from the driver's own answer** — the one place NVIDIA
differs from [T-Head](thead-mig.md#partition-profile-discovery) and
[Hygon](hygon-mig.md#partition-profile-discovery), whose detectors ask the card.

A profile's name states its geometry: `3g.40gb` is three of the GPU's compute slices and 40 GB of its
memory, and one shape is named for the memory the product carries — `3g.20gb` on an A100-40GB is the
`3g.40gb` of an H100-80GB.

NVIDIA lists the supported profiles for each MIG-capable product and memory variant: [Supported MIG
Profiles](https://docs.nvidia.com/datacenter/tesla/mig-user-guide/supported-mig-profiles.html). Read the
"GPU Instance Profiles" table for your product before planning a node: its Instances column is the
maximum a GPU can host, and its placement column is what the rules below act on.

The following rules govern what reaches GPUStack from that set; they are shared with the other
partitioning manufacturers:

> **The `+me` / `+me.all` / `+gfx` variants are not supported.** A Kubernetes resource name may not contain
> `+`, and GPUStack never rewrites a name to make it key-safe: one that is not already a valid resource-name
> segment is **excluded** from the inventory, so a key always maps back by a plain prefix strip. They reach
> no `Devices` ledger, capacity key or `InstanceType` inventory, and cannot be requested.

> **A partition is one whole GPU instance; subdividing it is not supported.** Every profile is created
> as a GPU instance plus a single compute instance covering all of it. GPUStack addresses a partition by one
> device id, and a GPU instance you subdivide by hand (`nvidia-smi mig -cci`) has several — a share of the
> compute each, all on the same memory — so it is refused rather than half-addressed.

> **A profile with no legal placement is not offered.** Its span could only be guessed, and the ledger being
> placement-derived, such a key could never allocate.

**Placement is what makes the inventory move.** GPU instances occupy hardcoded slots, in memory-slice units,
and a combination is legal only when the occupied intervals do not overlap — so a profile's maximum per GPU
is the length of its start list, and **two profiles covering the same slices need not get the same placement
freedom**.

On an A100/H100 a `4g` covers four slices as a `3g` does yet gets only one slot, and a `2g`
covers two as the same-size `1g` does yet gets three slots to its four.

A live instance therefore removes slots from *other* profiles: a `3g` at 0–3 takes the GPU's only `4g` slot
with it, while leaving room for a second `3g` at 4–7. That is why the `Devices` ledger reports each MIG
GPU's inventory as static per-profile counts (the maximum it could host) plus a **placement-aware
`Remaining`**, how many still fit (see [Requesting a partition](#requesting-a-partition)).

## Requesting a partition

A workload asks for **one hardware instance of a named profile**. GPUStack materializes it and injects
only the MIG device: no logical-slicing runtime (`libvgpu.so`), no fractional translation. The
**request shape**: one partitioned GPU plus the profile, one instance per `Pod`:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: nvidia-partition
spec:
  containers:
    - name: main
      resources:
        limits:
          nvidia.com/gpu.partitioned: "1"              # always exactly 1 GPU
          nvidia.com/gpu.partitioned.mig-1g.10gb: "1"  # always exactly 1 instance of this profile
```

- `mig` is NVIDIA's name for hardware partitioning, carried as `<kind>`; another manufacturer registers its
  own under the same `.partitioned.<kind>-<profile>` shape.
- **Name the exact profile** from [Partition profile discovery](#partition-profile-discovery) (e.g. `1g.10gb`). There is no
  memory→profile translation: you request `mig-<profile>`, not a fraction or a MiB amount.
- Both keys **must be exactly `1`** (a scope decision, not a hardware limit); a multi-partition workload
  asks for several `Pod`s.
- The partition family is **mutually exclusive** with every other accelerator family `Pod`-wide (a `Pod` also
  asking for a whole GPU, a shared unit or a logical slice is rejected at admission), it is **one profile
  shape per `Pod`**, and the accelerator claims must sit in **one container group**. The full rule set, with an
  accepted and a rejected example, is in [Accelerator Requests](requests.md#request-rules).
- **`nvidia.com/gpu.partitioned.units` is webhook-derived**: do not set it. The webhook folds the profile's
  VRAM into it, and quota is charged on the manufacturer's `credits` resource folded from it, so a `3g.40gb`
  and a logical `40Gi` slice cost the same.
- A MIG request must go through **Kueue** — a `Pod` (or workload) on a `LocalQueue`, or an `Instance` (below).
  Rules and folds bind only `Pod`s carrying the `kueue.x-k8s.io/queue-name` label.

**Through the `Instance` API.** *`spec.resources.acceleratorPartitionedProfile`* names the profile, and the
controller shapes it into the two keys above:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata:
  name: mig-instance
spec:
  type: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
  image: ubuntu:24.04
  command:
    - "sleep"
    - "86400"
  resources:
    accelerator: "1"  # a partition is always one GPU
    acceleratorPartitionedProfile: 3g.40gb
  volume:
    ephemeral:
      capacity: 1Gi
```

- The profile is validated at admission against the pool's inventory: one it does not offer is rejected with
  the offered set in the message, never left Pending.
- It excludes *`acceleratorSlicedMemoryPercentage`* / *`acceleratorSlicedCoresPercentage`* — the two cannot both
  apply to one GPU.
- Host CPU and RAM are sized by the profile's share of the GPU's VRAM, so a `1g` partition does not ask for
  a whole GPU's worth.

**GPU selection is the plugin's, not the kubelet's.** A `.partitioned` token is a fungible count, unlike the
three GPU-bound families: the plugin picks the GPU against the live partition geometry and records it. It
**packs** (the most-occupied GPU that still fits wins, keeping a sibling whole for a later whole-GPU
profile), so a rejection from `Allocate` means the node has no room, not a wrong GPU from the kubelet.

**Scheduling and the `Remaining` ledger.** Each MIG GPU advertises `allocated + remaining` per profile,
mirrored onto the node as `nvidia.com/gpu.partitioned.mig-<profile>`. Admission is placement-aware: the
per-GPU `AdmissionCheck` admits a `Pod` only while the requested profile has a free slot, and **retries** (never
hard-rejects) while the ledger is unpopulated.

> **Why both terms** — publishing only what is free would make the scheduler subtract every live
> instance twice; see [Capacity accounting](#capacity-accounting).

**Reclaim.** When the `Pod` exits, the operator destroys its compute instance then its GPU instance, and the
profile's `Remaining` restores within one reclaim cycle. A destroy racing a residual process returns
`NVML_ERROR_IN_USE`; the operator retries with bounds, never blocking siblings or other GPUs' allocations,
and logs a straggler past the bound. The node key frees on **`Pod` deletion**, ahead of the
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

## Limitations

- The administrator owns the *mode* lifecycle; GPUStack drives none of it, and evicts nothing when the mode
  changes (see [Operator boundaries](#operator-boundaries)).
- **Carving an instance out of band is unsupported on a managed node**: let GPUStack materialize them, and
  it reuses any already on a GPU it manages. Every node-level number (per-profile capacity key, partition
  token health, `AdmissionCheck`) derives from the allocation annotations the device plugin writes, and
  `nvidia-smi mig -cgi` by hand produces none.

  So while a GPUStack workload holds the GPU the node advertises room it does not have, and that
  **never converges**: placement reads live NVML and will not double-book it, but the accounting above
  stays wrong. Once the GPU is drained the reverse happens: after its debounce the reclaimer **destroys**
  an instance no allocation accounts for as an orphan, including one it never created.

  An instance with something **running on it** is the exception: it is left alone and re-checked each
  cycle, so one you carved by hand survives for as long as you are using it.

- **A same-profile replacement submitted the instant its predecessor is deleted can fail to start.** Leave a
  gap between the delete and the replacement, or let the replacement's own restart handle it.

  `Node` accounting is rebuilt from `Pod` annotations, so a deleted `Pod`'s slot reappears in the per-profile key
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
- The per-profile node keys are independently subtracted scalars, so two `Pod`s asking for mutually exclusive
  [placements](#partition-profile-discovery) can both land on a node that hosts only one of them; the second fails
  its allocation closed and is retried.
- The `+me` / `+me.all` / `+gfx` profile variants are excluded from the inventory (see
  [Partition profile discovery](#partition-profile-discovery)).
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
  even a rejection by name — a `Pod` carrying the old shape never schedules. See
  [Pre-release breaks](requests.md#pre-release-breaks), which also explains why a node must be
  **drained before its Device Manager is upgraded**.

## Enabling partitioning

The mode switch itself is NVIDIA's procedure, not this operator's: [Getting Started with
MIG](https://docs.nvidia.com/datacenter/tesla/mig-user-guide/getting-started-with-mig.html) is the
authority on the reset requirement and on what differs between GPU generations. The steps below are that
procedure with the two GPUStack-side steps it does not know about — the re-detect, and what to check
afterwards.

1. Satisfy the [Prerequisites](#prerequisites) — stop the using `Pod` and the driver-handle daemons.
2. Enable the mode with `nvidia-smi`, per GPU or for all GPUs:

   ```bash
   # one GPU by index
   nvidia-smi -i <id> -mig 1

   # all GPUs
   nvidia-smi -mig 1
   ```

3. **Ampere:** perform the GPU reset; if `nvidia_drm` is loaded or a passthrough VM blocks it, reboot the
   node/VM instead. **Hopper and newer:** no reset.
4. Restart the node's Device Manager `Pod` to re-detect. You need **not** delete the node's `Devices` object
   (the detector rewrites the group's capability in place), nor pre-create instances with
   `nvidia-smi mig -cgi` — the operator materializes them as workloads are admitted, reusing any you made.
5. Verify the `Devices` capability now reports those GPUs' MIG profiles and **zero logical slicing** — a
   hardware-partitioned GPU offers no software slicing.

### Fleet-wide MIG Manager

The steps above are per node and go through SSH. The NVIDIA GPU Operator ships a **MIG Manager** that does
the same job declaratively: each node carries an `nvidia.com/mig.config` label naming a profile, and the
manager makes that node's cards match it.

[Vendor Prerequisites](../../getting-started/vendor-prerequisites.md#nvidia) has that component off, because every other
profile also creates instances, which GPUStack must create during allocation. Enable MIG Manager with
`--set migManager.enabled=true` and select a profile that only enables MIG mode.

Use the profile named **`all-enabled`**. It enables MIG mode and creates no instances. GPUStack creates
instances during allocation. Other shipped profiles (`all-balanced`, `all-1g.5gb`, …) pre-create a fixed
geometry and conflict with that allocation.

```bash
kubectl label node <node> nvidia.com/mig.config=all-enabled --overwrite
```

The label is per node, so a fleet can mix freely: nodes labelled `all-enabled` serve partitions, nodes
labelled `all-disabled` (or carrying no label at all) stay whole-card.

**Restarting the Device Manager is still yours to do**, on every node whose label you changed, exactly as
in step 4 above. The MIG Manager restarts the GPU clients it owns, and this operator's Device Manager is
not one of them, so until it restarts the node keeps advertising the capability it detected before the
switch.

Three things to know before reaching for it:

- **Its default is `all-disabled`**, and the operator writes that label itself when the manager first
  runs. Arriving on a node whose cards you had already enabled by hand, it turns them back off.
- **It is placed only on nodes labelled `nvidia.com/mig.capable=true`**, which GPU Feature Discovery
  publishes. Without that component the `DaemonSet` exists but schedules nowhere; setting the label by hand
  is enough to place it.
- **It restarts the node's GPU clients, the kubelet included**, to apply a change, and marks
  `nvidia.com/mig.config.state=failed` when a service it wants to restart is masked — which is the state
  the readiness steps above deliberately leave DCGM in. Read the cards' own modes before believing that
  label.

## Disabling partitioning

Run the inverse: with no `Pod` using the GPU's instances, destroy them, then

```bash
nvidia-smi -i <id> -mig 0   # or `-mig 0` for all GPUs
```

apply the same reset rules (Ampere reset or reboot; Hopper none), and restart the node's Device Manager `Pod`
so the ledger returns the GPU to its whole-GPU / logical-slice capability.

## Node reboot recovery

If you did **not** reset MIG before a reboot ([Limitations](#limitations) says what persists):

- On the way back up the instances are gone (and, on Hopper+, so is the mode).
- Redo the [enable sequence](#enabling-partitioning), **including the Device Manager `DaemonSet` restart**
  — nothing else makes the post-reboot hardware reach the cluster.

A workload running before the reboot lost its instance, so **resubmit it** — delete and re-create the
`Pod`/workload, and the operator materializes a fresh instance on admission. A lingering pre-reboot `Pod` keeps
its ownership record but has no live instance, so its device allocation fails closed until it is recreated.

## Operator boundaries

- Enable, disable or reconfigure MIG mode — `nvidia-smi` operations you run.
- Trigger on nodeconfig or labels, flip the mode automatically, rewrite its geometry, or deschedule and
  evict `Pod`s on a mode change.
- Account for an instance you carved by hand — though it does delete it as an orphan once its GPU is
  idle and nothing is running on it ([Limitations](#limitations)).
- Hand out a subdivided instance, or subdivide one itself — it creates a GPU instance with a single
  compute instance covering all of it, and refuses one somebody else subdivided
  ([Partition profile discovery](#partition-profile-discovery)).

It does create and destroy the instances backing scheduled workloads
([Requesting a partition](#requesting-a-partition)).

## Three MIG configurations

Logical slicing and hardware partitioning operate on **disjoint GPU populations**. Partitioning a
GPU moves its capacity from `EX`/`SH`/`SL` to `PT` in `kubectl get instancetypes` rather than
adding capacity. On an eight-GPU H100-80GB node (`node-h100`), the three configurations observe:

| Configuration | Partitioned GPUs | `EX/SH/SL/PT` | Advertised node keys |
|---|---|---|---|
| All-logical | 0 of 8 | `8/8 80/80 100/800 0/0` | Whole/shared + `.sliced.*` (`1024` slices, `12800k` units); `.partitioned` is `0` |
| All-physical | 8 of 8 | `0/0 0/0 0/0 1/56` | `.partitioned: 56` and per-profile `.partitioned.mig-*` (`12800k` units); logical keys `0` |
| Mixed | 3 of 8 | `5/5 50/50 100/500 1/21` | Five GPUs under logical/whole keys (`640` slices); three GPUs under `.partitioned.*` (`21`) |

### Verification summary

After enabling or disabling MIG on selected GPUs, restart the Device Manager `DaemonSet` and wait
for the device plugin to re-register before submitting workloads:

```bash
kubectl -n gpustack-system rollout restart ds/gpustack-operator-device-manager-nvidia
kubectl -n gpustack-system rollout status ds/gpustack-operator-device-manager-nvidia
kubectl get instancetypes gpustack--nvidia-h100-80gb-hbm3-linux-amd64
kubectl get node <node> -o json | jq '.status.allocatable | with_entries(select(.key|test("nvidia.com/gpu")))'
```

> **Wait for plugin re-registration.** The operator publishes new capacity keys as soon as the
> ledger changes, ahead of kubelet registration. Wait for the `Pod` to be Ready and `.partitioned`
> keys to appear before submitting partition requests, or admission can fail with
> `UnexpectedAdmissionError`.

In a mixed cluster (3 partitioned GPUs, 5 logical GPUs), submitting one `3g.40gb` partition `Pod`
and one 50% logical-slice `Pod` places them on disjoint GPUs (`EX 5→4`, `SH 50→40`, `SL 500→450`,
`PT 21→17`). Confirm the carved device inside the `Pod` and its allocation annotation:

```bash
kubectl exec <partition-pod> -- nvidia-smi -L
kubectl get pod <partition-pod> -o jsonpath='{.metadata.annotations.device\.gpustack\.ai/accelerator\.allocated}'
```

Captured output (from an eight-GPU H100 node with one `3g.40gb` carved on GPU 0):

```text
GPU 0: NVIDIA H100 80GB HBM3 (UUID: GPU-950792bf-a01c-3f1a-e122-3473e67f54b2)
  MIG 3g.40gb     Device  0: (UUID: MIG-b3061c09-2a4c-5026-a575-79f86a5bb12c)
```

### Capacity accounting

Per GPU, placement is interval overlap over the eight memory slices. When a live `3g.40gb` occupies
slices `0–3`, only non-overlapping start slots remain free:

```text
the carved GPU's 8 memory slices

  [0][1][2][3]  taken by the live 3g.40gb          [4][5][6][7]  free
```

| Profile | Slot size | Legal starts | Free starts | Key addition |
|---|---|---|---|---|
| `1g.10gb` | 1 | 0 1 2 3 4 5 6 | 4, 5, 6 | 3 |
| `1g.20gb` | 2 | 0 2 4 6 | 4, 6 | 2 |
| `2g.20gb` | 2 | 0 2 4 | 4 | 1 |
| `3g.40gb` | 4 | 0 4 | 4 | 1 allocated + 1 free |
| `4g.40gb` | 4 | 0 | — | 0 |
| `7g.80gb` | 8 | 0 | — | 0 |

Across seven untouched GPUs plus that one carved GPU, the four reported numbers aggregate as
follows (each GPU contributes its **maximum** per-profile free count, never the sum across
competing profiles):

| Number | Example | Aggregation | Rationale |
|---|---|---|---|
| `…partitioned.mig-<profile>` | `1g.10gb: 52`, `3g.40gb: 16` | Profile's `allocated + remaining` | Scheduler subtracts live requests, so keys include them |
| `nvidia.com/gpu.partitioned` | `53` | `allocated +` GPU's largest free count | One pool key for the family; same scheduler-fit rule |
| `InstanceType` `PT` remaining | `52` | Largest free count alone (no allocated term) | Additional instances available now |
| `InstanceType` `PT` onceMaxRequest | `1` | `1` while any GPU can host an instance, else `0` | One request builds one instance on one GPU |

*`status.detail.slicedDetail`* derives strictly from the `Devices` **spec** (empty-pool capability,
unchanged after carving); read the node keys and `PT` for live remaining capacity.

---

**See also** — [Accelerator Requests](requests.md) (keys and request rules) ·
[Hygon MIG Operations](hygon-mig.md) (node-wide MIG on Hygon DCUs) ·
[T-Head MIG Operations](thead-mig.md) (hardware partitioning on T-Head PPUs) ·
[Preflight Checks](preflight.md) (verify MIG preconditions before workloads arrive)

**Next** → [Walkthrough](../../walkthroughs/devices/scheduling.md) — the logical-slicing counterpart on a live cluster.
