# KV Cache Backend

A `KVCacheBackend` configures shared KV cache for inference workloads. With a managed backend, the
operator runs a leader for metadata and a member group with one store process per selected node. An
external backend connects to an existing service.

Mooncake calls the leader a master. Its flags, environment variables and metrics use that name.

## Contents

- [Connection and medium](#connection-and-medium)
- [Image](#image)
- [Metadata plane](#metadata-plane)
- [Members](#members)
- [Vendor-specific behavior](#vendor-specific-behavior)
- [Status and conditions](#status-and-conditions)
- [Resizing a group](#resizing-a-group)
- [External mode](#external-mode)
- [Operating notes](#operating-notes)

## Connection and medium

`connection` chooses whether the operator manages the backend or connects to an external service.
For managed backends, *`members[].medium`* chooses host memory (`DRAM`) or accelerator memory
(`VRAM`).

```yaml
apiVersion: worker.gpustack.ai/v1
kind: KVCacheBackend  # cluster-scoped, short name kvcb
metadata:
  name: mooncake-dram
spec:
  type: Mooncake
  image: docker.io/kvcacheai/mooncake:0.3.13
  connection:
    managed:  # exactly one of managed or external
      leader:
        electionBackend: None  # The upstream image has no Kubernetes Lease backend.
      members:
        - nodeSelector:
            kubernetes.io/os: linux
          medium: DRAM  # memory segment medium: DRAM or VRAM
          capacityPerMember: 4Gi
```

- **Connection**: Set exactly one of *`connection.managed`* or *`connection.external`*.
- **Groups**: Multiple member groups are allowed. At most one may have a
  [local disk tier](local-disk-tier.md).
- **Medium**: *`members[].medium`* is immutable. Use separate groups for host and device memory.
- **Image**: Check [Engine Compatibility](../model-deployment/engine-versions.md) before choosing
  *`spec.image`*.
- **Election**: The upstream image needs *`electionBackend: None`* and one leader.

| Value | Meaning | Constraints |
|---|---|---|
| `DRAM` | Host memory segment | `Pod` `requests.memory` includes *`capacityPerMember`*; hides vendor accelerators (`void`). |
| `VRAM` | Device memory segment | Accounting-only for custom `USE_VRAM_SEGMENT=ON` images; omits *`capacityPerMember`* and `void` hiding. |

All official `mirrored-mooncake` builds compile with `USE_VRAM_SEGMENT=OFF` and allocate Host RAM
(`DRAM`) only. `KVCacheBackend` is cluster-scoped because it selects nodes, host memory, host
paths, and fabric privileges. Several `KVCachePool`s can reference one backend; see
[KV Cache Pools and Bindings](pool.md#limitations) for tenancy boundaries.

## Image

*`spec.image`* falls back to the `kv-cache-backend-image` `Setting` (defaulting to the project's
`-cpu` build). Admission rejects an empty image when both are unset, though clearing the `Setting`
does not block unrelated updates or finalizer removal.

The master needs no accelerator runtime; each member needs the runtime for its transport (`-cpu`
carries `libibverbs` and AWS `libfabric` for `TCP`/`RDMA`/`EFA`; `-cann` carries CANN for `ascend`).
[High availability](leader.md#high-availability) requires a project build supporting Kubernetes
`Lease` election.

### GPUStack build variants

Beside the `-cpu` default, GPUStack publishes `mirrored-mooncake` per vendor (`cuda`, `cann`,
`rocm`) tagged `<mooncake-version>-<variant><toolchain>` (such as `0.3.13.post1-cuda13.0`). All
official variants disable `USE_VRAM_SEGMENT` and allocate Host RAM (`DRAM`) only.

- **`-cpu` default**: Links `libibverbs` and AWS `libfabric` for `TCP`, `RDMA`, and `EFA` Host-RAM
  stores on NVIDIA and AMD nodes.
- **`-cann` variant**: Required on Ascend because `vllm-ascend` speaks HIXL (`protocol: ascend`),
  allocating Host RAM via ACL/HIXL.
- **`0.3.10.post2` variant**: Serves engines below the
  [supported minimum](../model-deployment/engine-versions.md); set `leader.electionBackend: None`
  and `leader.multiTenancy: false`.

*`members[].image`* overrides *`spec.image`*, which overrides the `Setting`. For private registries,
set *`spec.imagePullSecrets`* and *`spec.imagePullPolicy`* on the backend; both apply to the leader
and every member group.

### Version compatibility

**A store and an engine-embedded Mooncake client interoperate only within one minor line.** The
handshake reports `2.0.0` across 0.3.x releases, so mismatched minor lines (such as `0.3.12` versus
`0.3.13`) pass probes and fail transfers with `RPC_FAIL (-900)`. Match your store image to the
engine runner in [Engine Compatibility](../model-deployment/engine-versions.md).

Direct *prefill/decode (PD)* transfer (`MooncakeConnector` without *`spec.kvCache`*) is engine to
engine and exempt from store version matching. High availability requires a master from `0.3.12`
onward; see [High availability](leader.md#high-availability).

## Metadata plane

The metadata plane is unconditionally peer-to-peer (`P2PHANDSHAKE`) and requires no etcd, Redis, or
CR configuration (`spec.metadata` does not exist). Leader election is configured separately via
[`leader.electionBackend`](leader.md#high-availability) and defaults to a Kubernetes `Lease`.

## Members

Each `members[]` entry renders one `OnDelete` `DaemonSet` over *`members[].nodeSelector`*. Two
groups can select the same node (such as one `DRAM` group and one `VRAM` group) and each renders
its own `DaemonSet`.

*`spec.transport.protocol`* accepts `Auto`, `TCP`, `RDMA`, `EFA`, `CANN`, `ROCM`, `MUSA` and `MACA`
(default `Auto`, which resolves to `TCP`). *`members[].transport.protocol`* overrides the protocol
for one group. See [Engine Compatibility](../model-deployment/engine-versions.md#transport-compatibility)
for engine-to-transport support.

- **TCP**: Complete and requires no host privileges, but transfers traverse the kernel stack and use
  higher host CPU than `RDMA`.
- **Host fabrics (`RDMA`, `EFA`)**: Grant `hostNetwork: true`, `IPC_LOCK`, `SYS_RESOURCE`, and the
  fabric device resource (`device.gpustack.ai/rdma.shared` for 1 interface, `device.gpustack.ai/rdma`
  for `> 1`, or `vpc.amazonaws.com/efa`), controlled by *`members[].fabricInterfaceCount`* (default
  `1`). Never mount `/dev/infiniband`; the device plugin injects granted devices.
- **Memory accounting**: `DRAM` charges `capacityPerMember + localBufferSize` to `requests.memory`
  and hides accelerators (`void`); `VRAM` charges only *`localBufferSize`* and omits `void` hiding.
  Neither requests an accelerator extended resource.
- **Ports**: Each group serves HTTP on `8080 + <group index>` (`8080`, `8081`, …), bound on the host
  network for host-fabric groups. Data-plane ports are random ephemeral ports negotiated via
  `P2PHANDSHAKE`, so firewall and `NetworkPolicy` rules must allow a port range.
- **Extra arguments**: *`members[].extraArgs`* renders into argv (`-D key=value`) and
  *`leader.extraArgs`* renders as `-key=value`. Never place credentials in *`extraArgs`*.
- **Disk tier retries**: Writers against a [local disk tier](local-disk-tier.md) must retry when a
  put hits a full memory segment while async eviction catches up.

## Vendor-specific behavior

Platform transports and custom accelerator images add host mounts, fabric grants and environment
defaults beyond generic Host-RAM members.

### Ascend

`vllm-ascend` requires `protocol: CANN` (verified at `v0.23.0` and `v0.26.0rc1`) and a `-cann`
member image (`USE_ASCEND_DIRECT`), which allocates Host RAM (`medium: DRAM`) via ACL/HIXL.
Running a `-cann` image over `TCP` or `RDMA` fails at startup with
`cannot allocate local segment, ret: -1`. Idle `CANN` members consume zero AICore compute.

- **Ascend A2/A3 and A5 UB/UBG/UBoE**: Leave *`fabricInterfaceCount`* unset. For A5 UBG or UBoE,
  set `ASCEND_GLOBAL_RESOURCE_CONFIG` in *`members[].extraEnv`* with
  `"comm_resource_config.protocol_desc"` (`["ub_rtp:device"]` or `["uboe:device"]`).
- **Ascend A5 Host RoCE (`roce:host`)**: Set *`fabricInterfaceCount >= 1`* on `protocol: CANN`. The
  operator grants `hostNetwork`, `IPC_LOCK`, `SYS_RESOURCE`, `device.gpustack.ai/rdma*`, read-only
  `/etc/hixlep`, and defaulted `ASCEND_LOCAL_COMM_RES=""` + `ASCEND_GLOBAL_RESOURCE_CONFIG` (user
  *`extraEnv`* or *`hostPaths`* at `/etc/hixlep` yields the corresponding default). Provision
  [host endpoint files](../../getting-started/vendor-prerequisites.md#a5-host-roce-endpoints) first.

Worked `CANN` example (substitute a `910C` or A5 image tag for your SoC; add
`fabricInterfaceCount: 1` for Ascend A5 Host RoCE):

```yaml
apiVersion: worker.gpustack.ai/v1
kind: KVCacheBackend
metadata:
  name: ascend-shared-cache
spec:
  transport:
    protocol: CANN
  connection:
    managed:
      leader: {}  # replicas, allocationStrategy and multiTenancy default
      members:
        - nodeSelector:
            kvcache-ascend: "true"
          medium: DRAM
          image: gpustack/mirrored-mooncake:0.3.14-cann9.1-910b  # use 910C or A5 build for those SoCs
          capacityPerMember: 32Gi
          # fabricInterfaceCount: 1  # uncomment for Ascend A5 Host RoCE (roce:host)
          hostPaths:
            - path: /usr/local/Ascend/driver
              mountPath: /usr/local/Ascend/driver
              type: Directory
              readOnly: true
            - path: /usr/local/dcmi
              mountPath: /usr/local/dcmi
              type: Directory
              readOnly: true
            - path: /usr/local/bin/npu-smi
              mountPath: /usr/local/bin/npu-smi
              type: File
              readOnly: true
```

### AWS EFA

Check instance-size EFA support with `fi_info -p efa` before deploying an `EFA` group. Never mount
`/dev/infiniband`: a partial EFA grant causes `libfabric` to fail with `No available EFA devices`.
Place all communicating members and engines in one AWS cluster placement group (one availability
zone alone allows the handshake to complete while work requests hang), and supply an EFA-enabled
engine image per [RDMA Operations](../rdma/operations.md#efa-engine-images).

### Custom VRAM builds

`members[].medium: VRAM` is accounting-only for custom images built with `USE_VRAM_SEGMENT=ON`.
Supply user-space driver access via *`runtimeClassName`*, *`extraEnv`*, or *`hostPaths`*, and size
*`capacityPerMember`* against the engine's `gpu_memory_utilization` so both fit on the device.

```yaml
apiVersion: worker.gpustack.ai/v1
kind: KVCacheBackend
metadata:
  name: shared-cache
spec:
  connection:
    managed:
      leader: {}  # replicas, allocationStrategy and multiTenancy default
      members:
        - nodeSelector:
            kvcache-vram: "true"
          medium: VRAM  # accounting-only for custom USE_VRAM_SEGMENT=ON images
          image: custom-registry.example/mooncake-vram:0.3.14-cuda13.0
          capacityPerMember: 8Gi  # leave this memory free in the engine's GPU budget
          extraEnv:
            - name: NVIDIA_VISIBLE_DEVICES
              value: all
```

`NVIDIA_VISIBLE_DEVICES` requires `accept-nvidia-visible-devices-envvar-when-unprivileged`;
otherwise set *`runtimeClassName: nvidia`*. Pairing a custom `VRAM` group with a
[local disk tier](local-disk-tier.md) is measured on NVIDIA and unmeasured on Ascend and AMD.

## Status and conditions

Inspect backend phase, endpoint, observed capacity and conditions:

```bash
kubectl get kvcb
kubectl describe kvcb <name>
```

Captured output:

```text
NAME            TYPE       PHASE   ENDPOINT                                        CAPACITY
mooncake-dram   Mooncake   Ready   mooncake-dram-leader.gpustack-system.svc:50051  12Gi
```

Phases are `Provisioning`, `Ready`, `Degraded`, `Error` and `Deleting`. Status polls the leader
every `15 s`; *`status.capacity`* is **observed** from the leader's metrics (never multiplied from
*`capacityPerMember`*) and appears once `service_ready` is true.

| Condition | Moves phase | Meaning and action |
|---|---|---|
| `LeaderAvailable` | Yes | Leader health check passed. If `False`, inspect leader `Deployment` `Pod`s or external `Admin` reachability. |
| `MembersMounted` | Yes | Ready member `Pod`s match listed segments. Check `NoSegments`, `ListingFailed`, `ListingTooLarge`, or `AmbiguousMemberIdentity` (separate host-network groups onto distinct nodes). Pre-0.3.12 masters report `Unknown/SegmentListingNotServed`. |
| `CapacityObserved` | Yes | Leader `/metrics` scraped while ready. Clears on scrape failure; inspect leader `:9003/metrics`. |
| `PoolWrites` | Yes | `True` after a put end or positive `allocator_used_bytes`; `False` if puts revoke with zero bytes allocated. A first write on a fresh store can answer `500` until the tenant ledger propagates — retry once listed. |
| `Deletable` | Yes | `False` while *`status.usedBy`* lists live `KVCachePool` or `KVCachePoolBinding` consumers. |
| `RolloutComplete` | No | Informational: `False` while an `OnDelete` member rollout is in progress. |
| `ElectionObserved` | No | Informational (when `electionBackend: Kubernetes`): confirms `Lease` observation. |
| `TierWasEmpty` | No | Informational (when `localDisks` is set): reports whether the disk tier started empty. |

*`binding.usage`* and *`pool.usage`* lag synchronous leader `:9003/metrics` and
`/api/v1/tenant_quotas` by one reconcile tick. To inspect disk-tier data, see
[Bucket writes](local-disk-tier.md#bucket-writes).

## Resizing a group

Widening *`members[].nodeSelector`* adds members on matching nodes without restarting existing
`Pod`s; existing cached keys are not rebalanced (`allocationStrategy: FreeRatioFirst` biases new
writes toward emptier members). Changing image, argv, environment, resources, or fabric recreates
members under `OnDelete`.

Shrinking a group or narrowing *`nodeSelector`* **immediately discards** the cache held by removed
members; the operator does not migrate cached data. Member groups are identified by list index:

| Edit | Outcome |
|---|---|
| Swapping two entries or removing ahead of others | Refused |
| Appending or removing from the end | Allowed |
| Editing in place (including narrowing *`nodeSelector`* to match zero nodes) | Allowed |

To retire a middle group without rebuilding later groups, narrow its *`nodeSelector`* until it
matches zero nodes. On a group with a [local disk tier](local-disk-tier.md), set
*`scaleIn.gracePeriodSeconds`* (`0` to `3600`; derives `Pod` termination grace as
`gracePeriodSeconds + 60`) and wait for member recreation before shrinking so the `preStop` hook
deregisters the tier cleanly:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: KVCacheBackend
metadata:
  name: mooncake-dram
spec:
  connection:
    managed:
      leader: {}  # replicas, allocationStrategy and multiTenancy default
      members:
        - nodeSelector:
            kvcache-dram: "true"
          medium: DRAM
          capacityPerMember: 64Gi
      scaleIn:
        gracePeriodSeconds: 30  # 0 to 3600 seconds; applies to localDisks groups only
```

## External mode

*`connection.external`* connects to a backend managed outside GPUStack; the operator creates no
workloads and only observes. Both `Client` (for inference engines) and `Admin` (for operator health,
metrics, and segment listing) endpoints are required as `host:port`:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: KVCacheBackend
metadata:
  name: external-mooncake
spec:
  connection:
    external:
      endpoints:
        - name: Client
          address: mooncake.example:50051
        - name: Admin
          address: mooncake.example:9003
```

- An unreachable external endpoint reports `phase: Error` immediately (never `Provisioning`), and
  HTTP `3xx` redirects from `Admin` are refused.
- Do not point two `KVCacheBackend` objects at the same external leader with Bindings sharing a
  *`domain.name`*: quota ceilings oscillate, excess writes silently evict older keys, and differing
  *`blockSize`* or `dtype` values corrupt shared blocks.

## Operating notes

- **Benign startup log**: `E transfer_metadata.cpp:991] Local segment descriptor not found` appears
  on every client start and is harmless.
- **Metrics variable**: `MC_TE_METRIC=1` is rendered and reserved on every member group; do not
  redeclare it in *`members[].extraEnv`*. `replica_num` is configured on the engine client, not on
  `KVCacheBackend`.
- **Consumer guards**: A backend with non-empty *`status.usedBy`* cannot be deleted (stays in
  `phase: Deleting` while workloads keep serving) and cannot turn *`leader.multiTenancy`* off until
  all consumers are removed.
- **Teardown order**: Deleting a backend deletes owned workloads first (bounded by each workload's
  termination grace plus a few minutes) and removes the finalizer last.

---

**See also** — [KV Cache Election and Failover](leader.md) (metadata process, probes and `Lease`
election) · [KV Cache Local Disk Tier](local-disk-tier.md) (disk buckets and eviction) ·
[KV Cache Pools and Bindings](pool.md) (namespace grants and quota ceilings) ·
[Installation Modes](../../operate/installation-modes.md) (chart versus worker CRD ownership)

**Next** → [KV Cache Local Disk Tier](local-disk-tier.md) — optional local disk offload for a member group.
