# NVIDIA MIG Operations

GPUStack treats a card's NVIDIA **MIG** (Multi-Instance GPU) *mode* as a **manually managed** node property:
the operator *observes* MIG geometry through the Device Manager and reflects it in the `Devices` ledger and the
advertised slicing capability; it never enables, disables, or reconfigures MIG *mode* on your behalf. Once a
card is MIG-enabled, though, the operator **dynamically allocates** the hardware instances that back scheduled
workloads — it materializes a GPU/compute instance of the requested profile when a Pod is admitted and reclaims
it when the Pod exits (see [Requesting a MIG instance](#requesting-a-mig-instance)). This page is both the
administrator runbook for changing MIG *mode* on a node of a Kubernetes cluster running GPUStack and the user
contract for requesting a MIG instance.

The rule of thumb for **mode**: **you drive `nvidia-smi`; GPUStack catches up on the next Device Manager
detection.** A capability change enters the cluster only when the node's Device Manager pod re-detects the
hardware (on restart, or when the device set / health changes) — there are no nodeconfig or label triggers, no
automatic mode flips, no geometry rewrites, and no descheduling.

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
maximum instances of each profile the card could host) and, per profile, a **placement-aware `Remaining`**
count of how many instances still fit given what is already allocated — a `3g.40gb` at slots 0–3 removes the
`1g.10gb` slots it overlaps (see [Requesting a MIG instance](#requesting-a-mig-instance)).

For the full profile tables of every MIG-capable GPU (including other memory variants and the `+me` / `+gfx`
profiles), see NVIDIA's
[Supported MIG Profiles](https://docs.nvidia.com/datacenter/tesla/mig-user-guide/supported-mig-profiles.html).

## Requesting a MIG instance

Once a card is MIG-enabled, a workload asks for **one hardware instance of a named profile**. GPUStack
materializes that instance on demand and injects only the MIG device — there is no soft-slicing runtime
(`libvgpu.so`) and no fractional translation.

**Request shape.** A Pod requests the number of MIG-capable cards to slice plus the profile, one instance per
card:

```yaml
resources:
  limits:
    nvidia.com/gpu.sliced: "1"              # number of cards to slice
    nvidia.com/gpu.sliced.mig-1g.10gb: "1"  # one instance of this profile per card
```

- **Name the exact profile** from the [Supported profiles](#supported-profiles) tables (e.g. `1g.10gb`); there
  is no memory→profile translation — you request `mig-<profile>`, not a fraction or a MiB amount.
- The `mig-<profile>` value **must be exactly `1`** (one instance per card; request more cards for more
  instances) and is **mutually exclusive** with the soft-slice keys (`.sliced.cores-percentage`,
  `.sliced.memory-percentage`, `.sliced.memory-mib`) on the same container.
- **At most one profile per Pod** — every container and init container must name the same `mig-<profile>`; a Pod
  naming two different profiles is rejected by the admission webhook.
- A MIG request must go through **Kueue**: submit it as a Pod (or workload) on a `LocalQueue`. The `Instance`
  object cannot carry a MIG request (it exposes only the logical percentage budgets), so MIG reaches the
  cluster only as a raw Pod on a LocalQueue.

**Scheduling and the `Remaining` ledger.** Each MIG card advertises, per profile, how many instances still fit
— the `Devices` status `RemainingProfiles`, mirrored onto the node as the `nvidia.com/gpu.sliced.mig-<profile>`
extended resource for scheduler fit. Admission is placement-aware: the check admits a Pod only while the
requested profile has a free placement slot, and returns a **retry** (never a hard reject) while the ledger has
not yet been populated. Quota is charged on the manufacturer's `credits` resource — a MIG instance folds into
`.sliced.units` by its memory exactly like a same-VRAM soft slice (a `3g.40gb` and a soft `40Gi` request cost
the same), so MIG and logical slices share one credit scale.

**Reclaim.** When the Pod exits, the operator destroys its compute instance then its GPU instance, and the
profile's `Remaining` restores within one reclaim cycle. A destroy that races a residual process returns
`NVML_ERROR_IN_USE`; the operator retries with bounds — never blocking sibling instances on the same card or
allocations on any other card — and surfaces an operator-visible log if a straggler still holds the instance
past the bound.

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

- GPUStack **never** enables, disables, or reconfigures MIG *mode*, and **never** deschedules or evicts Pods on
  a mode change — the administrator owns the *mode* lifecycle. (The operator does create and destroy the
  *instances* that back scheduled workloads; see [Requesting a MIG instance](#requesting-a-mig-instance).) A
  capability change reaches the cluster only through Device Manager restart or re-detection.
- On **Ampere** the mode persists across reboots (stored in InfoROM); on **Hopper and newer** the mode is
  **not** persistent across reboots.
- MIG **instances never survive a reboot** on any generation.
- In a **passthrough VM** the hypervisor may forbid the GPU reset entirely — reboot the node/VM instead.
- Profile combinations are constrained by the fixed placement slots above; the ledger's `Remaining` is
  **placement-aware**, so a wide instance removes the smaller slots it overlaps.
- MIG instances do not survive a reboot; after re-enabling mode and restarting the Device Manager,
  **resubmit** any MIG workloads that were running before the reboot (see
  [Node reboot recovery](#node-reboot-recovery)).

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
4. Restart the node's Device Manager pod so it re-detects the hardware. You do **not** need to pre-create
   instances with `nvidia-smi mig -cgi` — the operator materializes each profile's instances on demand as
   workloads are admitted (it reuses any you did pre-create).
5. Verify the `Devices` ledger now reports the card's MIG profiles and **zero soft-slice capability** on those
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

A MIG workload that was running before the reboot lost its instance, so **resubmit it** (delete and re-create
the Pod/workload): the operator materializes a fresh instance on admission. A pre-reboot Pod that lingers with
its on-disk ownership record but no live instance fails its device allocation closed until it is recreated —
resubmitting is the clean recovery.

## Walkthrough: a MIG lifecycle on a node

A recorded run of the whole mode transition on a live Kubernetes cluster, in the same style as the
[scheduling-chain walkthrough](../walkthrough.md): every command is the real `kubectl` (or on-node
`nvidia-smi`) invocation and its real output, and each capability change is shown as a **before /
after**. The node is genericized as `node-h100` — a single H100-80GB card — and the operator runs the
defaults. The card starts with **MIG mode off** (soft-sliceable).

The one column to keep in mind: `kubectl get instancetypes` shows the accelerator three-view
**E**xclusive / **S**hared / **P**artitioned as `onceMax/remaining` groups, and that three-view is
**credit-based** — it tracks each card's per-card credit budget, so a *free* MIG card and a *free*
soft-sliced card read **identically** (`1/1 10/10 100/100`). The MIG-specific geometry is not in that
row; it surfaces in the `Devices` ledger and the node's per-profile `nvidia.com/gpu.sliced.mig-*`
resource keys, shown below.

### 1. Initial state — MIG off, soft-sliceable

The card offers logical soft slicing. Its InstanceType shows the free credit three-view, and the node
advertises the soft-slice capability keys (percentage / MiB budgets), **no** `mig-*` keys:

```console
$ kubectl get instancetypes
NAME                                          ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(E/S/P)   CPU     PHASE
gpustack--generic-linux-amd64                 gpustack-fnv64-3b93966fd73eb9ec   1/2Gi/100Gi             0/0 0/0 0/0          16/20   Active
gpustack--nvidia-h100-80gb-hbm3-linux-amd64   gpustack-fnv64-e4768a65ca0ce96b   4/16Gi/100Gi            1/1 10/10 100/100    0/0     Active

$ kubectl get node node-h100 -o json | jq '.status.allocatable | with_entries(select(.key|test("gpu.sliced.")))'
{
  "nvidia.com/gpu.sliced.cores-percentage": "12800",
  "nvidia.com/gpu.sliced.memory-mib": "81920",
  "nvidia.com/gpu.sliced.memory-percentage": "100",
  "nvidia.com/gpu.sliced.units": "1600k"
}
```

The `Devices` capability (in `spec`; the per-card runtime ledger is in `status`) reports the card as
logically sliceable and carries no physical profiles:

```console
$ kubectl get devices node-h100 -o jsonpath='{.spec.groups[0].accelerators[0].status}' | jq
{
  "logicalSliced": { "coresPercentageOvercommit": true, "count": 128 },
  "physicalSliced": {}
}
```

### 2. Administrator enables MIG on the node

Following the [Enabling MIG on a node](#enabling-mig-on-a-node) runbook, the admin flips the mode with
`nvidia-smi` on the node itself (Hopper needs no GPU reset). GPUStack never runs this:

```console
# On node-h100 (via SSH), the card idle and driver-handle daemons stopped:
$ sudo nvidia-smi -i 0 -mig 1
Enabled MIG Mode for GPU 00000000:00:04.0
All done.
```

The cluster does **not** react yet — a capability change enters only through Device Manager
re-detection.

### 3. Re-detect: delete the ledger object, restart the Device Manager

The Device Manager writes a node's capability **only at startup** and does not overwrite an existing
`Devices` object in place, so picking up a mode change is a two-step: delete the stale ledger, then
restart the DaemonSet pod so it re-detects onto a fresh object.

```console
$ kubectl delete devices node-h100
devices.worker.gpustack.ai "node-h100" deleted

$ kubectl -n gpustack-system rollout restart ds/gpustack-operator-device-manager-nvidia
daemonset.apps/gpustack-operator-device-manager-nvidia restarted

$ kubectl -n gpustack-system rollout status ds/gpustack-operator-device-manager-nvidia
daemon set "gpustack-operator-device-manager-nvidia" successfully rolled out
```

### 4. After re-detect — the card carves into MIG profiles

The `Devices` capability now reports the card's physical profiles (canonical names, per-card counts,
and each profile's legal placement slots) and **zero soft-slice capability** — a hard-partitioned card
offers no logical slicing:

```console
$ kubectl get devices node-h100 -o jsonpath='{.spec.groups[0].accelerators[0].status}' | jq
{
  "logicalSliced": {},
  "physicalSliced": {
    "count": 7,
    "profiles": [
      { "name": "1g.10gb", "computeSlices": 1, "memorySlices": 1, "count": 7,
        "placements": [ {"start":0,"length":1}, {"start":1,"length":1}, {"start":2,"length":1},
                        {"start":3,"length":1}, {"start":4,"length":1}, {"start":5,"length":1},
                        {"start":6,"length":1} ] },
      { "name": "1g.20gb", "computeSlices": 1, "memorySlices": 2, "count": 4,
        "placements": [ {"start":0,"length":2}, {"start":2,"length":2}, {"start":4,"length":2}, {"start":6,"length":2} ] },
      { "name": "2g.20gb", "computeSlices": 2, "memorySlices": 2, "count": 3,
        "placements": [ {"start":0,"length":2}, {"start":2,"length":2}, {"start":4,"length":2} ] },
      { "name": "3g.40gb", "computeSlices": 3, "memorySlices": 4, "count": 2,
        "placements": [ {"start":0,"length":4}, {"start":4,"length":4} ] },
      { "name": "4g.40gb", "computeSlices": 4, "memorySlices": 4, "count": 1,
        "placements": [ {"start":0,"length":4} ] },
      { "name": "7g.80gb", "computeSlices": 7, "memorySlices": 8, "count": 1,
        "placements": [ {"start":0,"length":8} ] }
    ]
  }
}
```

The node's capability keys flip accordingly — the three soft-slice keys are gone, replaced by one
`mig-<profile>` key per profile (the per-card count, the scheduler-fit capacity); `.sliced.units`
stays, because a MIG instance still folds into the manufacturer's credits by its memory:

```console
$ kubectl get node node-h100 -o json | jq '.status.allocatable | with_entries(select(.key|test("gpu.sliced.")))'
{
  "nvidia.com/gpu.sliced.mig-1g.10gb": "7",
  "nvidia.com/gpu.sliced.mig-1g.20gb": "4",
  "nvidia.com/gpu.sliced.mig-2g.20gb": "3",
  "nvidia.com/gpu.sliced.mig-3g.40gb": "2",
  "nvidia.com/gpu.sliced.mig-4g.40gb": "1",
  "nvidia.com/gpu.sliced.mig-7g.80gb": "1",
  "nvidia.com/gpu.sliced.units": "1600k"
}
```

### 5. InstanceType — still the credit three-view

The schedulable pool is unchanged in the three-view (the free card still reads `1/1 10/10 100/100`) —
MIG geometry lives in the ledger above, not this row. The `status.detail` does carry the aggregated
per-profile inventory that fed the node keys:

```console
$ kubectl get instancetypes gpustack--nvidia-h100-80gb-hbm3-linux-amd64
NAME                                          ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(E/S/P)   CPU   PHASE
gpustack--nvidia-h100-80gb-hbm3-linux-amd64   gpustack-fnv64-e4768a65ca0ce96b   4/16Gi/100Gi            1/1 10/10 100/100    0/0   Active

$ kubectl get instancetypes gpustack--nvidia-h100-80gb-hbm3-linux-amd64 \
    -o jsonpath='{.status.detail.slicedDetail.physical.profiles}' | jq -c '.[]'
{"name":"1g.10gb","count":7,"memoryMib":9984}
{"name":"1g.20gb","count":4,"memoryMib":20096}
{"name":"2g.20gb","count":3,"memoryMib":20096}
{"name":"3g.40gb","count":2,"memoryMib":40448}
{"name":"4g.40gb","count":1,"memoryMib":40448}
{"name":"7g.80gb","count":1,"memoryMib":81152}
```

### 6. Request a MIG instance

Submit a Pod on the pool's entrance `LocalQueue` (MIG reaches the cluster only as a Kueue-managed Pod;
see [Requesting a MIG instance](#requesting-a-mig-instance)), asking for one `3g.40gb` instance:

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
          nvidia.com/gpu.sliced: "1"              # one card to slice
          nvidia.com/gpu.sliced.mig-3g.40gb: "1"  # one 3g.40gb instance on it
```

Kueue admits it, the node-devices AdmissionCheck confirms a free `3g.40gb` placement, and the device
plugin **materializes** the GPU/compute instance on the card and injects only that MIG device:

```console
$ kubectl apply -f mig-demo.yaml
pod/mig-demo created

$ kubectl get pod mig-demo
NAME       READY   STATUS    RESTARTS   AGE
mig-demo   1/1     Running   0          18s

$ kubectl exec mig-demo -- nvidia-smi -L
GPU 0: NVIDIA H100 80GB HBM3 (UUID: GPU-95935384-86ff-4fd9-69ff-707ec6b08d11)
  MIG 3g.40gb Device 0: (UUID: MIG-41b3c2a7-0e9d-5f18-b6a2-9d0f7c5e1a84)
```

The three-view now reflects the consumed half-card. A `3g.40gb` takes ~40 GB of the ~80 GB card, so
the partitioned view (P), read as `onceMax/remaining`, drops to **50/50**; the card is no longer whole
(E) or shareable (S):

```console
$ kubectl get instancetypes gpustack--nvidia-h100-80gb-hbm3-linux-amd64
NAME                                          ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(E/S/P)   CPU   PHASE
gpustack--nvidia-h100-80gb-hbm3-linux-amd64   gpustack-fnv64-e4768a65ca0ce96b   4/16Gi/100Gi            0/0 0/0 50/50         0/0   Active
```

The card's placement-aware `RemainingProfiles` show the mutual exclusion — with a `3g.40gb` occupying
slots 0–3, the `7g.80gb` (needs all 8) and a second `4g.40gb` no longer fit, so they **drop out of the
ledger entirely** (only profiles that still fit are listed), while a second `3g.40gb` (slots 4–7) still
does:

```console
$ kubectl get devices node-h100 \
    -o jsonpath='{.status.groups[0].accelerators[0].remainingProfiles}' | jq -c '.[]'
{"name":"1g.10gb","count":3}
{"name":"1g.20gb","count":2}
{"name":"2g.20gb","count":1}
{"name":"3g.40gb","count":1}
```

### 7. Delete the Pod — the instance is reclaimed

Deleting the Pod releases the credits immediately; the operator then destroys the compute instance and
the GPU instance, and the profile's `Remaining` restores within one reclaim cycle:

```console
$ kubectl delete pod mig-demo
pod "mig-demo" deleted

$ kubectl get instancetypes gpustack--nvidia-h100-80gb-hbm3-linux-amd64
NAME                                          ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(E/S/P)   CPU   PHASE
gpustack--nvidia-h100-80gb-hbm3-linux-amd64   gpustack-fnv64-e4768a65ca0ce96b   4/16Gi/100Gi            1/1 10/10 100/100    0/0   Active

# On node-h100 (via SSH) the card holds no GPU instances once reclaim has run:
$ sudo nvidia-smi mig -lgi
No GPU instances found: Not Found
```

### 8. Administrator disables MIG — soft slicing returns

Reverse the runbook ([Disabling MIG on a node](#disabling-mig-on-a-node)): with the card idle, flip the
mode off and re-detect. The card returns to its whole-card / soft-slice capability.

```console
# On node-h100 (via SSH):
$ sudo nvidia-smi -i 0 -mig 0
Disabled MIG Mode for GPU 00000000:00:04.0
All done.

$ kubectl delete devices node-h100 && \
  kubectl -n gpustack-system rollout restart ds/gpustack-operator-device-manager-nvidia
devices.worker.gpustack.ai "node-h100" deleted
daemonset.apps/gpustack-operator-device-manager-nvidia restarted
```

The `mig-*` keys are gone and the soft-slice capability keys are back — the mirror image of step 4:

```console
$ kubectl get node node-h100 -o json | jq '.status.allocatable | with_entries(select(.key|test("gpu.sliced.")))'
{
  "nvidia.com/gpu.sliced.cores-percentage": "12800",
  "nvidia.com/gpu.sliced.memory-mib": "81920",
  "nvidia.com/gpu.sliced.memory-percentage": "100",
  "nvidia.com/gpu.sliced.units": "1600k"
}
```

---

## What GPUStack Operator does *NOT* do

- It does not enable, disable, or reconfigure MIG *mode* — those are `nvidia-smi` operations you run. (It
  *does* create and destroy the GPU/compute *instances* that back scheduled workloads; see
  [Requesting a MIG instance](#requesting-a-mig-instance).)
- It does not trigger on nodeconfig or labels, does not flip MIG mode automatically, and does not rewrite
  the mode geometry.
- It does not deschedule or evict Pods when MIG *mode* changes.

A capability change reaches the cluster **only** through Device Manager restart or re-detection.
