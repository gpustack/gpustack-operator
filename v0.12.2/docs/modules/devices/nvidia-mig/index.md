# NVIDIA MIG Operations

Administrators toggle NVIDIA *Multi-Instance GPU (MIG)* **mode** per GPU with `nvidia-smi` (or GPU
Operator's MIG Manager) and restart the node's Device Manager `DaemonSet` to re-detect. Once MIG
mode is active, GPUStack dynamically creates and destroys GPU/Compute Instance pairs for scheduled
workloads.

MIG-enabled GPUs advertise `nvidia.com/gpu.partitioned*` keys exclusively, while unpartitioned GPUs
on the same `Node` continue serving whole-GPU, shared, and logical-slice requests
([Accelerator Requests](/gpustack-operator/v0.12.2/docs/modules/devices/requests/index.md)).

## Contents

- [Prerequisites](#prerequisites)
- [Partition profile discovery](#partition-profile-discovery)
- [Partition requests](#partition-requests)
- [Operations](#operations)
- [MIG capacity scenarios](#mig-capacity-scenarios)
- [Limitations](#limitations)

## Prerequisites

The chart configures `gpustack-operator-device-manager-nvidia` with `runtimeClassName: nvidia` and
`NVIDIA_MIG_CONFIG_DEVICES` / `NVIDIA_MIG_MONITOR_DEVICES` so NVML exposes MIG management (do not
override these in `deviceManager.env`; see
[Vendor Prerequisites](/gpustack-operator/v0.12.2/docs/getting-started/vendor-prerequisites/index.md#nvidia)).

Before toggling MIG mode on or off:

- **Drain workloads and driver handles**: Stop all `Pod`s on target GPUs and stop host daemons
  holding NVML handles (such as DCGM or `nvsm`) so the switch does not stay pending.
- **Unload `nvidia_drm` and run with `CAP_SYS_ADMIN`**: Loaded `nvidia_drm` blocks GPU resets.
- **Generation reset rule**: **Ampere (`A100`/`A30`)** requires a GPU reset (or VM reboot on
  passthrough hypervisors) after toggling MIG mode; **Hopper (`H100`) and newer** switch without a
  reset.

## Partition profile discovery

Unlike [T-Head](/gpustack-operator/v0.12.2/docs/modules/devices/thead-mig/index.md#partition-profile-discovery) and
[Hygon](/gpustack-operator/v0.12.2/docs/modules/devices/hygon-mig/index.md#partition-profile-discovery), NVIDIA MIG profiles come from a fixed per-product
catalog matching NVIDIA's
[Supported MIG Profiles](https://docs.nvidia.com/datacenter/tesla/mig-user-guide/supported-mig-profiles.html)
(for example, `3g.20gb` on `A100-40GB` and `3g.40gb` on `H100-80GB`):

- **Excluded variants**: Profiles containing `+` (`+me`, `+me.all`, `+gfx`) or lacking legal
  placement slots are omitted from the inventory.
- **Whole GPU instances only**: GPUStack creates one Compute Instance spanning the full GPU
  Instance; manually subdivided instances (`nvidia-smi mig -cci`) are refused.
- **Placement-aware `Remaining`**: Because profiles compete for fixed memory-slice intervals (for
  example, one `3g.40gb` at `0–3` blocks the single `4g.40gb` slot at `0–3` while leaving `4–7`
  free), `Devices` tracks both static profile ceilings and live `Remaining` counts.

## Partition requests

Request one partitioned GPU and one exact profile on a Kueue-labeled `Pod` (`.partitioned.units` is
derived automatically by the webhook):

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: nvidia-partition
  namespace: team-a
  labels:
    kueue.x-k8s.io/queue-name: <local-queue>
spec:
  containers:
    - name: main
      image: ubuntu:24.04
      command:
        - sleep
        - "86400"
      resources:
        limits:
          nvidia.com/gpu.partitioned: "1"  # always "1"
          nvidia.com/gpu.partitioned.mig-1g.10gb: "1"  # always "1"
```

Or request the profile on an `Instance` via *`spec.resources.acceleratorPartitionedProfile`* (mutually
exclusive with slice percentages; host CPU/RAM scale by the profile's VRAM share):

```yaml
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata:
  name: mig-instance
  namespace: team-a
spec:
  type: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
  image: ubuntu:24.04
  command:
    - sleep
    - "86400"
  resources:
    accelerator: "1"
    acceleratorPartitionedProfile: 3g.40gb
  volume:
    ephemeral:
      capacity: 1Gi
```

- **Packing and reclaim**: Device Manager bin-packs onto the most-occupied MIG GPU with a valid
  slot, keeping sibling GPUs free for wider profiles. On `Pod` exit, the reclaimer destroys the
  Compute Instance then the GPU Instance (retrying `NVML_ERROR_IN_USE` with bounded backoff).
- **SSH sidecar isolation**: On SSH-enabled `Instance`s, the `sshd` sidecar verifies the on-disk
  ownership marker and MIG UUID under the GPU lock and receives **only** `main`'s MIG instance,
  never the parent GPU.

## Operations

### Enabling partitioning

1. Satisfy [Prerequisites](#prerequisites), enable MIG mode per GPU or across all GPUs, and reset on
   Ampere:

   ```bash
   nvidia-smi -i <id> -mig 1  # or: nvidia-smi -mig 1
   # On Ampere (A100/A30) only: nvidia-smi -i <id> -r
   ```

2. Restart the NVIDIA Device Manager `DaemonSet` so the detector updates `Devices` in place (do not
   delete `Devices` or pre-carve instances):

   ```bash
   kubectl -n gpustack-system rollout restart ds/gpustack-operator-device-manager-nvidia
   kubectl -n gpustack-system rollout status ds/gpustack-operator-device-manager-nvidia
   ```

### Fleet-wide MIG Manager

To toggle MIG mode declaratively through NVIDIA GPU Operator, enable `--set migManager.enabled=true`
and label target `Node`s with **`all-enabled`** (which enables MIG mode without pre-creating
instances; never use `all-balanced` or `all-1g.*`):

```bash
kubectl label node <node> nvidia.com/mig.config=all-enabled --overwrite
```

- **Default `all-disabled` caution**: Unlabeled `Node`s default to `all-disabled` when MIG Manager
  starts, turning off hand-enabled MIG GPUs; ensure `nvidia.com/mig.capable=true` is present so MIG
  Manager schedules.
- **Manual Device Manager restart still required**: Restart `gpustack-operator-device-manager-nvidia`
  after MIG Manager finishes switching a `Node` (check `nvidia-smi` directly if masked DCGM causes
  `nvidia.com/mig.config.state=failed`).

### Disabling partitioning

Drain partition `Pod`s so instances reclaim (or destroy remaining instances manually), disable MIG
mode, apply any Ampere reset, and restart `gpustack-operator-device-manager-nvidia`:

```bash
nvidia-smi -i <id> -mig 0  # or: nvidia-smi -mig 0
```

### Node reboot recovery

After an unplanned reboot, re-run [Enabling partitioning](#enabling-partitioning) (including the
Device Manager `DaemonSet` restart), then delete and recreate pre-reboot partition `Pod`s so fresh
MIG instances materialize.

## MIG capacity scenarios

Partitioning GPUs shifts capacity from `EX`/`SH`/`SL` to `PT` on `InstanceType` (`SH` below shows
legacy `10`-share display; current builds display card count `8/80` and `5/50`, see
[Legacy request migration](/gpustack-operator/v0.12.2/docs/modules/devices/requests/index.md#legacy-request-migration)). An 8-GPU `H100-80GB` `Node`
reports these capacity views across three layouts:

| Configuration | Partitioned GPUs | `EX/SH/SL/PT` | Advertised node keys |
|---|---|---|---|
| All-logical | `0` of `8` | `8/8 80/80 100/800 0/0` | Whole/shared + `.sliced.*` (`1024` slices, `12800k` units); `.partitioned: 0` |
| All-physical | `8` of `8` | `0/0 0/0 0/0 1/56` | `.partitioned: 56` + `.partitioned.mig-*` (`12800k` units); logical keys `0` |
| Mixed | `3` of `8` | `5/5 50/50 100/500 1/21` | `5` GPUs under logical/whole (`640` slices); `3` GPUs under `.partitioned.*` (`21`) |

### Verification

Wait for `gpustack-operator-device-manager-nvidia` to be `Ready` and `.partitioned` keys to register
on the `Node` before submitting `Pod`s, then verify carved UUIDs:

```bash
kubectl get instancetypes gpustack--nvidia-h100-80gb-hbm3-linux-amd64 -o wide
kubectl get node <node> -o json | jq '.status.allocatable | with_entries(select(.key|test("nvidia.com/gpu")))'
kubectl exec <partition-pod> -- nvidia-smi -L
kubectl get pod <partition-pod> -o jsonpath='{.metadata.annotations.device\.gpustack\.ai/accelerator\.allocated}{"\n"}'
```

Captured output (`1 × 3g.40gb` carved on GPU 0):

```text
GPU 0: NVIDIA H100 80GB HBM3 (UUID: GPU-950792bf-a01c-3f1a-e122-3473e67f54b2)
  MIG 3g.40gb     Device  0: (UUID: MIG-b3061c09-2a4c-5026-a575-79f86a5bb12c)
```

### Capacity accounting

When one live `3g.40gb` occupies slices `0–3` on an 8-slice H100 (`4–7` free), free start slots per
profile on that GPU are:

| Profile | Slice width | Legal starts | Free starts | Node key contribution |
|---|---|---|---|---|
| `1g.10gb` | `1` | `0 1 2 3 4 5 6` | `4, 5, 6` | `3` |
| `1g.20gb` | `2` | `0 2 4 6` | `4, 6` | `2` |
| `2g.20gb` | `2` | `0 2 4` | `4` | `1` |
| `3g.40gb` | `4` | `0 4` | `4` | `1` allocated + `1` free |
| `4g.40gb` | `4` | `0` | — | `0` |
| `7g.80gb` | `8` | `0` | — | `0` |

Across 7 empty GPUs plus that 1 carved GPU:

| Surface | Value | Formula |
|---|---|---|
| `.partitioned.mig-<profile>` | `1g.10gb: 52`, `3g.40gb: 16` | Sum of `allocated + remaining` (scheduler subtracts live `Pod`s) |
| `nvidia.com/gpu.partitioned` | `53` | Sum of `allocated + max(free profile count)` per GPU |
| `InstanceType` `PT` (`onceMaxRequest/remaining`) | `1/52` | `1` while any slot is free; sum of `max(free profile count)` without allocated |

*`status.detail.slicedDetail`* reflects static empty-pool capability from `Devices.spec`; read `PT`
and `PARTITIONS` (`-o wide`) for live remaining slots.

## Limitations

GPUStack creates and reclaims single-CI MIG instances for scheduled workloads and cleans idle
untracked instances. It never toggles MIG mode, resets GPUs, evicts `Pod`s on mode switches, or
serves subdivided `-cci` instances.

- **No manual `nvidia-smi mig -cgi` or `-cci`**: Hand-carved instances desynchronize node capacity
  keys and are destroyed as orphans once idle; subdivided `-cci` instances halt node-wide reclaim
  until removed.
- **Immediate replacement race**: Node capacity keys free on `Pod` deletion slightly ahead of
  debounced hardware destroy; leave a brief gap before submitting an identical replacement or allow
  it to restart once.
- **Reboot persistence**: MIG **instances** never survive a host reboot on any generation, and MIG
  **mode** persists across reboots on Ampere (InfoROM) but resets to disabled on Hopper and newer.
- **Provider NVLink health checks**: Some managed cloud agents cordon/reboot `Node`s when MIG drops
  GPUs out of full NVLink pair checks; inspect provider node conditions if a MIG `Node` reboots.

---

**See also** — [Accelerator Requests](/gpustack-operator/v0.12.2/docs/modules/devices/requests/index.md) (keys and request rules) · [Hygon MIG Operations](/gpustack-operator/v0.12.2/docs/modules/devices/hygon-mig/index.md) (node-wide MIG on Hygon DCUs) · [T-Head MIG Operations](/gpustack-operator/v0.12.2/docs/modules/devices/thead-mig/index.md) (MIG partitioning on T-Head PPUs) · [Preflight Checks](/gpustack-operator/v0.12.2/docs/modules/devices/preflight/index.md) (bare-host MIG checks)

**Next** → [Heterogeneous Devices Walkthrough](/gpustack-operator/v0.12.2/docs/walkthroughs/devices/scheduling/index.md) — trace logical slicing and scheduling on a live cluster.
