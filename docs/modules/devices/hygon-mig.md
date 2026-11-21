# Hygon MIG Operations

Administrators toggle Hygon DCU partitioning mode **node-wide** with `hy-smi` and restart the
Device Manager `DaemonSet` to re-detect. Once enabled, GPUStack dynamically creates and destroys GPU
and Compute Instance pairs for scheduled workloads under `hygon.com/dcu.partitioned.mig-<profile>`
([Accelerator Requests](requests.md#two-families-two-populations)).

## Contents

- [Prerequisites](#prerequisites)
- [Partition profile discovery](#partition-profile-discovery)
- [Partition requests](#partition-requests)
- [Limitations](#limitations)
- [Enabling partitioning](#enabling-partitioning)
- [Disabling partitioning](#disabling-partitioning)
- [Node reboot recovery](#node-reboot-recovery)
- [Operator boundaries](#operator-boundaries)

## Prerequisites

- **Virtualization driver flag**: Install the Hygon driver with virtualization support enabled;
  otherwise the driver exposes zero partition profiles.
- **Zero `/dev/kfd` holders (including Device Manager)**: `hy-smi -mig` fails with `Set system level
  mig mode failed. The device is exist and may be in use.` while any process (including the running
  Device Manager `Pod`) holds `/dev/kfd` (`hymgr` holding `/dev/mkfd` does not block). Verify empty
  output before switching:

  ```bash
  for p in /proc/[0-9]*; do pid=${p#/proc/}; n=$(ls -l "$p"/fd 2>/dev/null | grep -c kfd); \
    [ "$n" -gt 0 ] 2>/dev/null && echo "pid=$pid fds=$n $(tr -d '\0' < "$p"/cmdline)"; done
  ```

- **Device Manager restart**: Restart `gpustack-operator-device-manager-hygon` after every mode
  change so `Devices` re-detects.

## Partition profile discovery

Device Manager queries supported profiles and legal placements directly from the driver (computing
full-card CU counts from `profile CUs × max instances` while MIG mode is active). Measured on a
C-3000 "BW" DCU (`4` slices, `80` CUs, `65520 MiB`):

| Profile | Slices | Compute units | Memory | Max per card | Legal placements |
|---|---|---|---|---|---|
| `2g.15gb` | `1` | `20` | `16380 MiB` | `4` | `0:1 1:1 2:1 3:1` |
| `4g.31gb` | `2` | `40` | `32760 MiB` | `2` | `0:2 2:2` |
| `8g.63gb` | `4` | `80` | `65520 MiB` | `1` | `0:4` |

### Driver registry

The Device Manager `DaemonSet` mounts `/etc/dmi_mig_config` **writable** so driver library calls can
create and remove instance records, and reads `dev<N>` (PCI bus mapping) plus
`ci/dev<N>gi<G>ci<C>.conf` (`mig_uuid:`) to bind partition handles back to physical cards.

## Partition requests

Request both `hygon.com/dcu.partitioned: "1"` and `hygon.com/dcu.partitioned.mig-<profile>: "1"` on
a Kueue-labeled `Pod` (or set `acceleratorPartitionedProfile` on an `Instance`):

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: hygon-partition
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
          hygon.com/dcu.partitioned: "1"
          hygon.com/dcu.partitioned.mig-2g.15gb: "1"
        requests:
          hygon.com/dcu.partitioned: "1"
          hygon.com/dcu.partitioned.mig-2g.15gb: "1"
```

Device Manager carves the GPU/Compute Instance pair and injects `/dev/kfd`, `/dev/mkfd`, card DRM
nodes, the user-space runtime, a read-only bind mount of `/etc/dmi_mig_config/ci/dev<N>gi<G>ci<C>.conf`
at its exact host path, and `DMI_MIG_VISIBLE_DEVICE=MIG-<uuid>`.

## Limitations

- **Node-wide, partition-only mode**: `hy-smi -mig` takes no `-i` card flag. Once enabled, every
  card on the `Node` requires a MIG registry file (`hygon.com/dcu` and `hygon.com/dcu.sliced` drop
  to `0`); mixed whole-card and partitioned DCUs on one `Node` are impossible.
- **Single partition per container**: The Hygon runtime exposes at most one partition per container
  regardless of `DMI_MIG_VISIBLE_DEVICE`; multi-partition workloads must use one `Pod` per
  partition.
- **Registry directory poisoning**: If a container starts after its `.conf` file was removed, the
  container runtime creates an empty directory at `ci/dev<N>...conf`, causing future creates to fail
  with `INSUFFICIENT_RESOURCES`. Current Device Manager builds sweep stray directories automatically
  (use `rmdir` manually on older builds).
- **Ephemeral MIG UUIDs**: Hygon generates a fresh `mig_uuid` on every create, even at the same slot.

## Enabling partitioning

Consult the vendor [DCU Multi-Instance manual](https://developer.sourcefind.cn/document/9169ef18-c10d-11f0-b077-0242ac150003?id=4a82aeed-e242-11f0-b9e4-0242ac150003&title=3+DCU+Multi-Instance%E4%BD%BF%E7%94%A8%E6%89%8B%E5%86%8C&version=9169ef18-c10d-11f0-b077-0242ac150003)
(note: the long flag is `--multi-instance-gpu`, and `-i` is not supported):

1. Drain accelerator `Pod`s, scale down or cordon `gpustack-operator-device-manager-hygon` on the
   `Node`, and verify zero `/dev/kfd` holders ([Prerequisites](#prerequisites)).
2. Enable node-wide MIG mode (do not pre-carve instances manually):

   ```bash
   sudo hy-smi -mig 1
   ```

   Captured output:

   ```text
   Enabled mig mode successful.
   ```

3. Restore and restart `gpustack-operator-device-manager-hygon`, then verify allocatable keys:

   ```bash
   kubectl get node <node> -o jsonpath='{.status.allocatable}' | tr ',' '\n' | grep hygon
   ```

   Captured output:

   ```text
   "hygon.com/dcu":"0"
   "hygon.com/dcu.partitioned":"32"
   "hygon.com/dcu.sliced":"0"
   ```

## Disabling partitioning

Follow the strict teardown order required by the driver:

1. Drain all partition `Pod`s and wait for automatic reclaim (or destroy manually per card, compute
   instances before GPU instances):

   ```bash
   sudo hy-smi mig -dci -i <card>
   sudo hy-smi mig -dgi -i <card>
   ```

2. Stop `gpustack-operator-device-manager-hygon` on the `Node` so `/dev/kfd` is closed.
3. Disable node-wide MIG mode (`sudo hy-smi -mig 0`).
4. Start `gpustack-operator-device-manager-hygon` and confirm `hygon.com/dcu` and
   `hygon.com/dcu.sliced` return.

## Node reboot recovery

MIG instances and `/etc/dmi_mig_config` reset on host reboot. Verify `hy-smi` MIG mode after reboot,
restart `gpustack-operator-device-manager-hygon` if needed, and recreate pre-reboot partition
`Pod`s (stale ownership markers are reclaimed automatically).

## Operator boundaries

GPUStack creates and reclaims single-CI partition instances per `Pod`. It never toggles node-wide
`hy-smi -mig` mode, mixes whole and partitioned DCUs on one `Node`, grants multiple partitions to
one container, or keeps warm unclaimed partitions.

---

**See also** — [Accelerator Requests](requests.md) (request rules and family split) · [NVIDIA MIG Operations](nvidia-mig.md) (per-GPU MIG runbook) · [T-Head MIG Operations](thead-mig.md) (PPU MIG runbook)

**Next** → [T-Head MIG Operations](thead-mig.md) — enable and operate hardware partitions on T-Head PPUs.
