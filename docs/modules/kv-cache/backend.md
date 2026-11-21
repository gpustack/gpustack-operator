# KV Cache Backend

A `KVCacheBackend` configures shared KV cache storage for inference workloads. On a managed backend,
the operator runs a metadata leader and one store member per selected `Node` in each member group.
An external backend connects to an existing service.

Mooncake calls the leader a `master`, and its flags, environment variables, and metrics retain that
name.

## Contents

- [Configuration](#configuration)
- [Backend images](#backend-images)
- [Hardware requirements](#hardware-requirements)
- [Status and conditions](#status-and-conditions)
- [Group resizing](#group-resizing)
- [Operations](#operations)

## Configuration

### Connections and storage media

*`spec.connection`* selects either operator-managed workloads or an external endpoint pair. On
managed backends, *`members[].medium`* selects host memory (`DRAM`) or device memory (`VRAM`).

```yaml
apiVersion: worker.gpustack.ai/v1
kind: KVCacheBackend  # cluster-scoped, short name kvcb
metadata:
  name: mooncake-dram
spec:
  type: Mooncake  # required; enum: Mooncake
  image: docker.io/kvcacheai/mooncake:0.3.13  # optional; defaults from kv-cache-backend-image Setting
  connection:
    managed:  # required: set exactly one of managed or external
      leader:
        electionBackend: None  # upstream image lacks Kubernetes Lease support
      members:
        - nodeSelector:
            kubernetes.io/os: linux
          medium: DRAM  # required, immutable; enum: DRAM, VRAM
          capacityPerMember: 4Gi  # required per-node segment size
```

- **Cluster scope**: `KVCacheBackend` is cluster-scoped because it claims host memory, host paths,
  and fabric devices. Multiple `KVCachePool`s can share one backend ([Limitations](pool.md#limitations)).
- **Member groups**: Declare one or more groups; at most one group can configure a
  [local disk tier](local-disk-tier.md).
- **Medium immutability**: *`members[].medium`* cannot change in place; add a separate group to mix
  host and device memory.

| Value | Meaning | Constraints |
|---|---|---|
| `DRAM` | Host RAM segment | Adds `capacityPerMember + localBufferSize` to `Pod` `requests.memory` and sets `void` device visibility. |
| `VRAM` | Device memory segment | Accounting-only for custom `USE_VRAM_SEGMENT=ON` images; charges only *`localBufferSize`* and omits `void` hiding. |

### Metadata discovery

The metadata plane always uses peer-to-peer handshakes (`P2PHANDSHAKE`) and needs no external etcd
or Redis service (*`spec.metadata`* does not exist). Configure leader election separately through
[`leader.electionBackend`](leader.md#high-availability) (default `Kubernetes`).

### Member groups

Each *`members[]`* entry reconciles one `OnDelete` `DaemonSet` matching *`members[].nodeSelector`*.
Two groups can select the same `Node` when their network identities do not collide.

*`spec.transport.protocol`* accepts `Auto` (default, resolving to `TCP`), `TCP`, `RDMA`, `EFA`,
`CANN`, `ROCM`, `MUSA`, and `MACA`. *`members[].transport.protocol`* overrides the protocol for one
group; see [Engine Compatibility](../model-deployment/engine-versions.md#transport-compatibility).

- **TCP**: Needs no host privileges, but copies payloads through the kernel network stack.
- **Host fabrics (`RDMA`, `EFA`)**: Enable `hostNetwork: true`, `IPC_LOCK`, `SYS_RESOURCE`, and
  fabric resource requests (`device.gpustack.ai/rdma.shared` for `1`, `device.gpustack.ai/rdma` for
  `> 1`, or `vpc.amazonaws.com/efa`) sized by *`members[].fabricInterfaceCount`* (default `1`). Do
  not mount `/dev/infiniband` manually.
- **Ports**: Group `<i>` serves HTTP on `8080 + <i>` (bound on the host network when `hostNetwork`
  is active). Data transfers use ephemeral ports negotiated via `P2PHANDSHAKE`, so open ephemeral
  port ranges in firewalls and `NetworkPolicy` rules.
- **Extra flags**: *`members[].extraArgs`* renders as `-D key=value` and *`leader.extraArgs`*
  renders as `-key=value`; CXL flags are refused. Never place secrets in *`extraArgs`*.
- **Disk offload retries**: Clients writing to a [local disk tier](local-disk-tier.md) must retry
  when a memory segment fills ahead of asynchronous bucket eviction.

### External Mooncake endpoints

*`connection.external`* attaches to an existing Mooncake service without creating workloads. Supply
both `Client` and `Admin` endpoints as `host:port`:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: KVCacheBackend
metadata:
  name: external-mooncake
spec:
  connection:
    external:
      endpoints:
        - name: Client  # engine RPC endpoint
          address: mooncake.example:50051
        - name: Admin  # operator health, metrics, and tenant API endpoint
          address: mooncake.example:9003
```

- **Immediate error state**: Unreachable external endpoints report `phase: Error` immediately (never
  `Provisioning`), and HTTP `3xx` redirects from `Admin` are refused.
- **Single-backend ownership**: Never point two `KVCacheBackend` objects at the same external leader
  with overlapping *`domain.name`* bindings; ceilings will oscillate and mismatched *`blockSize`* or
  *`dtype`* values corrupt tensors.

## Backend images

*`members[].image`* overrides *`spec.image`*, which falls back to the `kv-cache-backend-image`
`Setting` (defaulting to the project's `-cpu` build). Admission refuses create when all three are
empty; clearing the `Setting` later does not block unrelated updates or deletion.

Set *`spec.imagePullSecrets`* and *`spec.imagePullPolicy`* on the backend to apply registry pull
settings across the leader and all member groups.

### GPUStack build variants

GPUStack publishes `mirrored-mooncake` variants tagged `<mooncake-version>-<variant><toolchain>`
(such as `0.3.13.post1-cuda13.0`). All official builds set `USE_VRAM_SEGMENT=OFF`, allocate Host RAM
(`DRAM`) only, and compile Kubernetes `Lease` [high availability](leader.md#high-availability):

- **`-cpu` default**: Bundles `libibverbs` and AWS `libfabric` for `TCP`, `RDMA`, and `EFA` Host-RAM
  stores on NVIDIA and AMD `Node`s.
- **`-cann` variant**: Required on Ascend because `vllm-ascend` uses HIXL (`protocol: ascend`) over
  ACL/HIXL Host RAM.
- **`-rocm` variant**: Adds ROCm transport support alongside Host-RAM storage.
- **`0.3.10.post2` variant**: Supports legacy engines below the
  [supported floor](../model-deployment/engine-versions.md); set `leader.electionBackend: None` and
  `leader.multiTenancy: false`.

### Version compatibility

**Match the store image and the engine's embedded Mooncake client to the same minor line.** The wire
handshake reports `2.0.0` across `0.3.x` releases, so mismatched lines (such as `0.3.12` and
`0.3.13`) pass probes and fail transfers with `RPC_FAIL (-900)`. Check runner pairings in
[Engine Compatibility](../model-deployment/engine-versions.md).

Direct *prefill/decode (PD)* transfer (`MooncakeConnector` without *`spec.kvCache`*) runs engine to
engine and does not use a store. Leader `Lease` election requires Mooncake `0.3.12` or newer.

## Hardware requirements

### Ascend

`vllm-ascend` requires `protocol: CANN` and a `-cann` member image (`USE_ASCEND_DIRECT`) with
`medium: DRAM`. Starting a `-cann` image on `TCP` or `RDMA` fails with
`cannot allocate local segment, ret: -1`. Idle `CANN` members use zero AICore compute.

- **Ascend A2/A3 and A5 UB/UBG/UBoE**: Omit *`fabricInterfaceCount`*. For A5 UBG or UBoE, set
  `ASCEND_GLOBAL_RESOURCE_CONFIG` in *`members[].extraEnv`* with `"comm_resource_config.protocol_desc"`
  set to `["ub_rtp:device"]` or `["uboe:device"]`.
- **Ascend A5 Host RoCE (`roce:host`)**: Set `fabricInterfaceCount: 1` (or higher) on
  `protocol: CANN` after preparing
  [Ascend host prerequisites](../../getting-started/vendor-prerequisites.md#ascend). The operator
  grants `hostNetwork`, `IPC_LOCK`, `SYS_RESOURCE`, `device.gpustack.ai/rdma*`, read-only
  `/etc/hixlep`, and defaulted `ASCEND_LOCAL_COMM_RES=""` plus `ASCEND_GLOBAL_RESOURCE_CONFIG`.

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
      leader: {}  # uses default replicas, allocationStrategy, and multiTenancy
      members:
        - nodeSelector:
            kvcache-ascend: "true"
          medium: DRAM
          image: gpustack/mirrored-mooncake:0.3.14-cann9.1-910b  # match 910B, 910C, or A5 SoC
          capacityPerMember: 32Gi
          # fabricInterfaceCount: 1  # enable for Ascend A5 Host RoCE (roce:host)
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

Verify EFA support on the instance type with `fi_info -p efa` before deploying an `EFA` group, and
never mount `/dev/infiniband` (partial device visibility causes `No available EFA devices`). Place
all members and engines in one AWS cluster placement group and build an EFA-capable engine image per
[RDMA Operations](../rdma/operations.md#efa-engine-images).

### Custom VRAM builds

`medium: VRAM` is accounting-only for user-built `USE_VRAM_SEGMENT=ON` images. Grant user-space
driver access via *`runtimeClassName`*, *`extraEnv`*, or *`hostPaths`*, and deduct
*`capacityPerMember`* from the engine's `gpu_memory_utilization` budget:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: KVCacheBackend
metadata:
  name: shared-cache
spec:
  connection:
    managed:
      leader: {}
      members:
        - nodeSelector:
            kvcache-vram: "true"
          medium: VRAM  # custom USE_VRAM_SEGMENT=ON image required
          image: custom-registry.example/mooncake-vram:0.3.14-cuda13.0
          capacityPerMember: 8Gi
          extraEnv:
            - name: NVIDIA_VISIBLE_DEVICES
              value: all  # requires accept-nvidia-visible-devices-envvar-when-unprivileged or runtimeClassName: nvidia
```

Pairing a custom `VRAM` group with a [local disk tier](local-disk-tier.md) is verified on NVIDIA
and unmeasured on Ascend and AMD.

## Status and conditions

Inspect backend phase, client endpoint, observed capacity, and conditions:

```bash
kubectl get kvcb
kubectl describe kvcb <name>
```

Captured output:

```text
NAME            TYPE       PHASE   ENDPOINT                                        CAPACITY
mooncake-dram   Mooncake   Ready   mooncake-dram-leader.gpustack-system.svc:50051  12Gi
```

Phases are `Provisioning`, `Ready`, `Degraded`, `Error`, and `Deleting`. Status polls the leader
every `15 s`; *`status.capacity`* comes from live leader metrics once `service_ready` is `true`,
never from multiplying *`capacityPerMember`*.

| Condition | Moves phase | Meaning and action |
|---|---|---|
| `LeaderAvailable` | Yes | Leader health check passed. If `False`, inspect leader `Deployment` `Pod`s or external `Admin` reachability. |
| `MembersMounted` | Yes | Ready member `Pod`s match listed segments. On `AmbiguousMemberIdentity`, move host-network groups onto disjoint `Node`s; pre-`0.3.12` masters report `Unknown/SegmentListingNotServed`. |
| `CapacityObserved` | Yes | Leader `:9003/metrics` scraped while ready. If `False`, check leader admin port reachability. |
| `PoolWrites` | Yes | `True` after a put completes or `allocator_used_bytes > 0`; `False` if puts revoke with zero allocated bytes (retry initial writes after tenant ledger propagation). |
| `Deletable` | Yes | `False` while *`status.usedBy`* names live `KVCachePool` or `KVCachePoolBinding` objects; delete those consumers first. |
| `RolloutComplete` | No | Informational: `False` while an `OnDelete` member `DaemonSet` rollout is incomplete. |
| `ElectionObserved` | No | Informational (under `electionBackend: Kubernetes`): confirms `Lease` holder observation. |
| `TierWasEmpty` | No | Informational (when *`localDisks`* is set): records whether the disk directory started empty. |

Pool and binding usage fields lag leader `:9003/metrics` and `/api/v1/tenant_quotas` by one
reconcile pass. For disk-tier write checks, see [Bucket writes](local-disk-tier.md#bucket-writes).

## Group resizing

Widening *`members[].nodeSelector`* adds member `Pod`s without restarting existing ones; existing keys
do not rebalance (`allocationStrategy: FreeRatioFirst` steers new writes toward emptier members).
Editing image, argv, environment, resources, or fabric settings updates the `OnDelete` `DaemonSet`,
requiring manual `Pod` deletion to roll each `Node`.

Shrinking a group or narrowing *`nodeSelector`* **discards** cached keys on removed `Node`s. Member
groups are indexed by list position:

| Edit | Outcome |
|---|---|
| Reordering entries or deleting an entry before the tail | Refused at admission |
| Appending or removing at the end of *`members`* | Allowed |
| Editing in place (including narrowing *`nodeSelector`* to zero `Node`s) | Allowed |

To retire a middle group without shifting later indices, narrow its *`nodeSelector`* to match zero
`Node`s. Before shrinking a group with *`localDisks`*, set *`scaleIn.gracePeriodSeconds`* (`0` to
`3600`, setting `Pod` termination grace to `gracePeriodSeconds + 60`) and roll the `Pod`s so the
`preStop` hook deregisters segments cleanly:

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: KVCacheBackend
 metadata:
   name: mooncake-dram
 spec:
   connection:
     managed:
+      scaleIn:
+        gracePeriodSeconds: 30  # 0..3600; applies to localDisks groups
```

## Operations

- **Benign client warning**: `E transfer_metadata.cpp:991] Local segment descriptor not found` logs
  once on engine startup and is harmless.
- **Reserved metrics env**: `MC_TE_METRIC=1` is rendered on every member group and refused in
  *`members[].extraEnv`*. Configure `replica_num` on the engine client, not on `KVCacheBackend`.
- **Consumer holds**: While *`status.usedBy`* is non-empty, deletion waits in `Deleting` (leaving
  workloads running) and disabling *`leader.multiTenancy`* is refused.

---

**See also** — [KV Cache Election and Failover](leader.md) (metadata leader, probes, and `Lease` election) · [KV Cache Local Disk Tier](local-disk-tier.md) (disk buckets, cleanup, and disk-heavy `Node`s) · [KV Cache Pools and Bindings](pool.md) (namespace grants and reuse domains) · [Installation Modes](../../operate/installation-modes.md) (chart and worker CRD ownership)

**Next** → [KV Cache Local Disk Tier](local-disk-tier.md) — add local NVMe offload to a member group.
