# KV Cache Backend Internals

Evidence: contract. Detailed `KVCacheBackend` rendering, environment mapping, fabric privilege
derivation, status polling algebra, condition state machines, positional member-group identity and
teardown mechanics.

## Contents

- [Medium schema history](#medium-schema-history)
- [Member rendering](#member-rendering)
- [Fabric and port rules](#fabric-and-port-rules)
- [Status and conditions](#status-and-conditions)
- [Group identity and shrink](#group-identity-and-shrink)
- [External mode and teardown](#external-mode-and-teardown)

## Medium schema history

All official `mirrored-mooncake` builds compile with `USE_VRAM_SEGMENT=OFF` and allocate segments
in Host RAM (`DRAM`). `VRAM` stays in the schema as an accounting-only mode for user-supplied
`USE_VRAM_SEGMENT=ON` images. An earlier development schema offered four additional values that are
not member groups:

| Former `medium` | Backing tier | Replacement |
|---|---|---|
| `LocalDisk` | Tier on members already holding a memory replica | `members[].localDisks` |
| `NoF` | NVMe-oF target coordinate registered once with no `Pod` | No API surface |
| `CXL` | DAX device allocated by the leader process | Refused (`enable_cxl`, `cxl_path`, `cxl_size` rejected in *`leader.extraArgs`*) |
| `DFS` | Distributed filesystem configured in leader env | Not rendered by this API |

### CRD validation ratcheting

Deleting a development-cluster object written against the removed enum values depends on the API
server's `CRDValidationRatcheting` feature gate:

| Kubernetes version | Gate state | Update behavior |
|---|---|---|
| v1.33+ | Locked on | Admitted; finalizer removal and deletion succeed |
| v1.30–v1.32 | On by default | Admitted unless explicitly disabled |
| v1.28–v1.29 | Off by default | Admitted only when enabled |
| Earlier | Absent | Refused including finalizer removal; delete object before upgrading CRD |

## Member rendering

Each `members[]` entry renders one `OnDelete` `DaemonSet` with no `ConfigMap`, no volume (beyond
configured `hostPaths` / `localDisks` / conditional `/etc/hixlep`), and no init container.

### Environment and argv mapping

| Mooncake key | Rendered variable | Source |
|---|---|---|
| `local_hostname` | `MOONCAKE_LOCAL_HOSTNAME` | Downward API (`status.podIP`) |
| `metadata_server` | `MOONCAKE_TE_META_DATA_SERVER` | Literal `P2PHANDSHAKE` (exact spelling with underscore inside `META_DATA`) |
| `master_server_address` | `MOONCAKE_MASTER` | Leader `Service` address |
| `protocol` | `MOONCAKE_PROTOCOL` | Effective group protocol |
| `global_segment_size` | `MOONCAKE_GLOBAL_SEGMENT_SIZE` | *`members[].capacityPerMember`* |
| `local_buffer_size` | `MOONCAKE_LOCAL_BUFFER_SIZE` | *`members[].localBufferSize`* |
| `device_name` | `MOONCAKE_DEVICE` | Deliberately unset (empty means use every discovered device; `auto-discovery` filters out all devices) |
| Transfer metrics | `MC_TE_METRIC` | Fixed `"1"`, reserved in *`members[].extraEnv`* |

- **Arguments**: *`members[].extraArgs`* renders into container argv as `-D key=value`;
  *`leader.extraArgs`* renders as `-key=value`. Both are world-readable on the cluster-scoped CR
  and rendered workloads; never place credentials there.
- **Metadata plane**: `spec.metadata` does not exist; strict clients fail with an unknown-field
  error, while non-strict clients silently prune it. `MC_METADATA_SERVER` is the low-level transfer
  engine knob, not the store client variable (`MOONCAKE_TE_META_DATA_SERVER`).
- **Image pull policy**: *`spec.imagePullSecrets`* and *`spec.imagePullPolicy`* apply backend-wide
  to the leader and every member group. Omitted *`imagePullPolicy`* resolves to `Always` for
  `:latest` or untagged images and `IfNotPresent` otherwise, re-evaluated on image changes.
- **Stub versus real CUDA library**: Master-only execution tolerates a stub `libcuda.so.1`, whereas
  members require a real `libcudart.so.12` exporting `cudaFreeHost`.
- **DRAM versus VRAM accounting**: `DRAM` adds `capacityPerMember + localBufferSize` to
  `requests.memory` and sets vendor device visibility variables to `void`. `VRAM` adds only
  *`localBufferSize`* to `requests.memory`, omits `void` hiding, and requests no accelerator
  extended resource (a single member allocates its segment on one visible device).

## Fabric and port rules

Effective group protocol (`members[].transport.protocol`, inheriting `spec.transport.protocol`,
default `Auto` → `TCP`) selects network privileges, device resources and ports independently of
*`medium`*:

| Effective protocol | Network & capabilities | Device request | Notes |
|---|---|---|---|
| `TCP`, `Auto` | Pod network; no extra caps | None | `Auto` always resolves to `TCP`; `fabricInterfaceCount` warns and requests nothing |
| `RDMA` | `hostNetwork: true`, `IPC_LOCK`, `SYS_RESOURCE` | `device.gpustack.ai/rdma.shared` (`1`) or `device.gpustack.ai/rdma` (`> 1`) | Default count `1`; never mounts `/dev/infiniband` |
| `EFA` | `hostNetwork: true`, `IPC_LOCK`, `SYS_RESOURCE` | `vpc.amazonaws.com/efa` (count default `1`) | Requires EFA-capable libfabric in image and single AWS cluster placement group; `/dev/infiniband` breaks partial grants with `EPERM` |
| `CANN` | Pod network unless `fabricInterfaceCount >= 1` | None unless `fabricInterfaceCount >= 1` (`rdma.shared` / `rdma`) | Requires `-cann` image (`USE_ASCEND_DIRECT`); see [Ascend A5 Matrix](ascend-a5-matrix.md#kvcachebackend-matrix) for Host RoCE |
| `ROCM`, `MUSA`, `MACA` | Pod network; no extra caps | None | `MUSA`/`MACA` are intra-node IPC; `fabricInterfaceCount` warns |

- **Management port**: Each group serves HTTP on `8080 + <group index>` (`8080` for index 0, `8081`
  for index 1). Host-network groups bind that port on the host namespace.
- **Data-plane ports**: Random ephemeral ports negotiated via `P2PHANDSHAKE`; no fixed
  *`containerPort`* is declared. Firewall and `NetworkPolicy` rules must permit a port range.
- **Security context merge**: *`members[].securityContext`* merges field by field onto protocol
  grants with `capabilities.add` unioned so `IPC_LOCK` and `SYS_RESOURCE` are preserved.
- **`hostPaths[]` rules**: Entries declare `{path, mountPath, type, readOnly}` and cannot duplicate
  mounts, target `/dev/infiniband`, or collide with *`localDisks[].path`*.

## Status and conditions

The controller polls the leader over HTTP every `15 s` after the prior pass finishes (up to three
sequential reads: health, `/metrics`, and segment listing).

### Condition catalog

| Condition | Moves phase | `True` meaning | Non-True reasons |
|---|---|---|---|
| `LeaderAvailable` | Yes | Leader health endpoint answers `service_ready` | Leader `Deployment` not ready or external `Admin` unreachable (`Error` in external mode) |
| `MembersMounted` | Yes | Ready member `Pod`s match listed segments | `ListingFailed` (keeps prior list), `ListingTooLarge` (withholds oversized update), `AmbiguousMemberIdentity` (shared host-network IP), `NoSegments`, `SegmentListingNotServed` (`Unknown` on pre-0.3.12 masters) |
| `CapacityObserved` | Yes | Leader `/metrics` scraped while `service_ready` is true | Missing/failed scrape clears capacity pointers (unlike retained member list) |
| `PoolWrites` | Yes | Leader saw put end in process lifetime or member reports `allocator_used_bytes > 0` | `False` when put start + put revoke seen with zero allocated bytes; `Unknown` before first put start, during unfinished put, or on pre-0.3.12 listing absence |
| `Deletable` | Yes | *`status.usedBy`* empty | Claimed by live pool/binding consumers (holds `phase: Deleting`) |
| `RolloutComplete` | No | All member `DaemonSet` `Pod`s match current `pod-spec-hash` | Rollout in progress (`OnDelete`) |
| `ElectionObserved` | No | Appears when `leader.electionBackend: Kubernetes`; leader holds `Lease` | Election not yet observed |
| `TierWasEmpty` | No | Appears when `localDisks` configured; tier started empty | Disk tier held pre-existing data |

### Observation nuances

- **Readiness probe gating**: Member readiness probes the entrypoint REST port, which listens only
  after segment mount completes. Starting `Pod`s pulling images do not mark the backend `Degraded`;
  unschedulable or crash-looping `Pod`s do and surface their reason in *`status.phaseMessage`*.
- **Capacity vs usage**: *`status.capacity.total`* reads `master_total_capacity_bytes` (plus
  `master_total_file_capacity_bytes` when a local disk tier exists, or both pools in external mode).
  Publishing is gated on `service_ready` so a starting leader's zero-valued `/metrics` exposition is
  not mistaken for an empty cache.
- **Listing vs capacity failure asymmetry**: A failed segment listing retains the previous
  *`status.members[]`* with `MembersMounted=False`, whereas a failed capacity scrape clears the
  capacity pointers.
- **Ambiguous host-network identity**: Two host-network member `Pod`s on one node share an IP and
  cannot be disambiguated back to *`status.members[]`* entries, setting `AmbiguousMemberIdentity`;
  each row retains only node/medium fields that all candidate `Pod`s agree on.
- **First-write race and lag**: A first write to a fresh store can return `500` (put start with no
  put end) before the tenant ledger propagates; retry once the tenant appears. *`binding.usage`* and
  *`pool.usage`* lag synchronous leader `:9003` `/metrics` and `/api/v1/tenant_quotas` by one
  reconcile tick.

## Group identity and shrink

Member groups have no name; list index determines `DaemonSet` name, immutable selector labels, and
management port (`8080 + index`).

- **Positional edit validation**: Appending, deleting from the tail, and in-place edits are allowed;
  swapping or deleting an earlier entry is refused. Reorders combined with edits, or removing an
  entry when a following entry is identical (`[A, B, C]` → `[A, C, C]`), cannot be distinguished
  without names and remain admitted.
- **Graceful shrink (`scaleIn.gracePeriodSeconds`)**: Range `0` to `3600` (the member endpoint
  rejects values above `3600` with `400`). Only groups with `localDisks` render the `preStop`
  deregistration hook; `Pod` `terminationGracePeriodSeconds` derives as `gracePeriodSeconds + 60`.
  Because `DaemonSet` uses `OnDelete`, changing *`scaleIn.gracePeriodSeconds`* requires member
  recreation (`pod-spec-hash` change) before narrowing *`nodeSelector`*.
- **Drain API exclusion**: Mooncake's drain job API reaches memory and NVMe-oF replicas only and
  skips disk-tier segments while reporting success, so it is not exposed here.

## External mode and teardown

- **External `Admin` redirect refusal**: The controller never follows HTTP `3xx` redirects from an
  external `Admin` endpoint to avoid SSRF with operator network identity.
- **Duplicate external objects**: Two external `KVCacheBackend` objects pointing to one leader are
  not rejected at admission (tracked in issue #288). Sharing a *`domain.name`* across their Bindings
  causes quota oscillation, silent eviction without error counters, and cross-Binding cache
  corruption when *`blockSize`* or `dtype` differs.
- **Active-consumer guards**: Deletion holds at `phase: Deleting` while *`status.usedBy`* is
  non-empty; turning *`leader.multiTenancy`* off is refused while *`status.usedBy`* is non-empty
  (even for stale entries until cleared).
- **Ordered bounded teardown**: Teardown deletes owned workloads (verified by ownership note rather
  than name alone) and waits per workload for its `Pod` `terminationGracePeriodSeconds` plus a few
  minutes before emitting a warning Event and removing the finalizer.

---

**See also** — [KV Cache Backend](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/kv-cache/backend/index.md) (operator configuration and troubleshooting) · [KV Cache Election and Failover](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/kv-cache/leader/index.md) (leader probes and Lease election) · [KV Cache Local Disk Tier](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/kv-cache/local-disk-tier/index.md) (disk buckets and eviction) · [Ascend A5 Matrix](ascend-a5-matrix.md#kvcachebackend-matrix) (Ascend A5 CANN scenarios)
