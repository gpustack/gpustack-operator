# T-Head MIG Operations

Administrators toggle T-Head PPU partitioning mode per card with `ppu-smi` and restart the Device
Manager `DaemonSet` to re-detect. Once enabled, GPUStack dynamically creates and destroys GPU and
Compute Instance pairs for scheduled workloads under `alibabacloud.com/ppu.partitioned.mig-<profile>`
([Accelerator Requests](requests.md#accelerator-resource-families)).

## Contents

- [Prerequisites](#prerequisites)
- [Partition profile discovery](#partition-profile-discovery)
- [Partition requests](#partition-requests)
- [Operations](#operations)
- [Limitations](#limitations)

## Prerequisites

**Stop all accelerator `Pod`s and the Device Manager `Pod` on the `Node` before toggling MIG mode.**
Any process opening the `alixpu` driver holds descriptors across **all** PPUs on the host (the
worker `Pod` also opens PPU nodes briefly at startup), causing `ppu-smi` to fail with `MIG resources
are in use` while `dmesg` logs `[alixpu] PPU00<N> contains <k> active process` (`PPU00<N>` is the
card ordinal `<N>`).

Verify zero open `alixpu` file descriptors before switching (no device reset is required once
holders exit):

```bash
for p in /proc/[0-9]*; do pid=${p#/proc/}; n=$(ls -l "$p"/fd 2>/dev/null | grep -c alixpu); \
  [ "$n" -gt 0 ] 2>/dev/null && echo "pid=$pid fds=$n $(tr -d '\0' < "$p"/cmdline)"; done
```

Restart `gpustack-operator-device-manager-thead` after every mode change so `Devices` updates.

## Partition profile discovery

Device Manager queries profiles and static legal placements from `hgml` (excluding nameless,
invalid-segment, subdivided, or placement-less profiles):

```bash
ppu-smi mig -i <N> -lgip   # profiles, memory, and live Instances Free/Total
ppu-smi mig -i <N> -lgipp  # static legal {Start,...}:Size placements (unchanged by occupancy)
```

**Normalized profile names:** T-Head display names omit the dot separator (`MIG 4g48gb`,
`MIG 8g96gb`). GPUStack normalizes two-number display names to match NVIDIA's spelling in Kubernetes
keys and `InstanceType` inventories (`alibabacloud.com/ppu.partitioned.mig-4g.48gb` and
`mig-8g.96gb`), while `ppu-smi` and raw `Devices` records keep `4g48gb`.

## Partition requests

Request `alibabacloud.com/ppu.partitioned: "1"` and `alibabacloud.com/ppu.partitioned.mig-<profile>: "1"`
on a Kueue-labeled `Pod`, or set *`acceleratorPartitionedProfile`* on an `Instance`:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: thead-partition
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
          alibabacloud.com/ppu.partitioned: "1"
          alibabacloud.com/ppu.partitioned.mig-4g.48gb: "1"
---
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata:
  name: thead-instance
  namespace: team-a
spec:
  type: <thead-instance-type>
  image: ubuntu:24.04
  command:
    - sleep
    - "86400"
  resources:
    accelerator: "1"
    acceleratorPartitionedProfile: 4g.48gb
  volume:
    ephemeral:
      capacity: 1Gi
```

- **Device-node injection**: T-Head uses no environment variable hook; allocation injects the two
  shared control nodes, `/dev/alixpu_ppu<ordinal>` (built from the **card ordinal**, never the driver
  minor number `ordinal + 1` which points to the next PPU), and `/dev/alixpu-caps/alixpu-cap<minor>`
  read live from `/proc/driver/alixpu/capabilities/ppu<N>/mig/gi<G>{,/ci<C>}/access`.
- **Packing, SSH visibility, and reclaim**: Follow the same fungible-token, sidecar-isolation, and
  reclaim rules as [NVIDIA MIG Operations](nvidia-mig.md#partition-requests).

## Operations

### Enabling partitioning

Consult [T-Head's PPU MIG documentation](https://developer.t-head.cn/docs_center/doc_detail/index.html?projectId=33&chapterId=117):

1. Satisfy [Prerequisites](#prerequisites) (zero `alixpu` file-descriptor holders).
2. Enable MIG mode **one PPU ordinal at a time** using top-level `-mig 1` (`ppu-smi mig` rejects
   `-mig`, and comma-separated `-i` lists fail with `No devices were found`):

   ```bash
   ppu-smi -i <N> -mig 1
   ppu-smi -q -i <N> | grep -i -A2 "MIG Mode"
   ```

   Captured output:

   ```text
   Enable MIG mode for PPU <bus-id>.
   All done.
       MIG Mode
           Current                             : Enabled
           Pending                             : Enabled
   ```

3. Restart `gpustack-operator-device-manager-thead` and verify `Devices` publishes partition
   profiles (`logicalSliced` drops on partitioned PPUs while unpartitioned PPUs keep logical
   slicing).

### Disabling partitioning

1. Drain partition `Pod`s on the PPU so instances reclaim, or destroy remaining instances manually
   (**Compute Instance first**, and always pass `-gi <G>` because bare `-dgi` destroys all instances
   on the PPU):

   ```bash
   ppu-smi mig -i <N> -gi <G> -ci <C> -dci
   ppu-smi mig -i <N> -gi <G> -dgi
   ```

2. Stop `alixpu` holders ([Prerequisites](#prerequisites)), run `ppu-smi -i <N> -mig 0`, and restart
   `gpustack-operator-device-manager-thead`.

> **Note:** Leftover `/dev/alixpu-caps/alixpu-cap<minor>` device nodes are normal driver behavior
> after destroy; check live instances with `ppu-smi mig -i <N> -lgi`.

### Node reboot recovery

After a host reboot, re-verify and re-run [Enabling partitioning](#enabling-partitioning), restart
`gpustack-operator-device-manager-thead`, and delete/recreate pre-reboot partition `Pod`s.

## Limitations

GPUStack creates and reclaims single-CI partition instances per `Pod` and cleans idle untracked
instances. It never runs `ppu-smi -mig`, evicts `Pod`s on mode switches, or serves subdivided
instances.

- **Manual mode lifecycle and no out-of-band carving**: Administrators own `ppu-smi -mig` switches;
  hand-carved instances skew node accounting and are reclaimed when idle, while hand-subdivided
  instances block node-wide reclaim.
- **Single partition per PPU request**: Request one `Pod` per partition ([Rule 3](requests.md#rule-3-partition-count)),
  and leave a brief gap when replacing a deleted `Pod` to avoid the reclaim-debounce window.
- **Unconfirmed reboot persistence**: Mode persistence across host reboots is unverified on hardware;
  always verify and re-apply after a reboot.

---

**See also** — [NVIDIA MIG Operations](nvidia-mig.md) (sibling MIG runbook and capacity math) · [Hygon MIG Operations](hygon-mig.md) (node-wide MIG on Hygon DCUs) · [Accelerator Requests](requests.md) (resource keys and request rules)

**Next** → [Accelerator Requests](requests.md) — review the full accelerator request contract.
