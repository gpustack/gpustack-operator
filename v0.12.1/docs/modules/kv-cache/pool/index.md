# KV Cache Pools and Bindings

A `KVCacheBackend` runs the store, a cluster-scoped `KVCachePool` publishes its capacity, and a
namespaced `KVCachePoolBinding` grants a namespace a byte ceiling under one reuse domain.

**A Binding provisions capacity; it does not enforce network isolation.** Review
[Limitations](#limitations) before sharing one backend across untrusted tenants. In GPUStack APIs,
the tenant partition is called a *reuse domain*; Mooncake flags, metrics, and errors call it a
*tenant*.

## Contents

- [Pools and bindings](#pools-and-bindings)
- [Cache reuse domains](#cache-reuse-domains)
- [Capacity grants](#capacity-grants)
- [Pod injection](#pod-injection)
- [Quota and deletion checks](#quota-and-deletion-checks)
- [Limitations](#limitations)

## Pools and bindings

`KVCachePool` and `KVCachePoolBinding` mirror `ClusterQueue` and `LocalQueue`: the cluster object
owns backend capacity, and the namespaced object grants quota and RBAC scope to one namespace.

```yaml
apiVersion: worker.gpustack.ai/v1
kind: KVCachePool  # cluster-scoped, short name kvcp
metadata:
  name: shared-dram
spec:
  backends:  # required; single-item list naming one KVCacheBackend
    - mooncake-dram
  quota:
    total: 900Gi  # optional pool quota target
---
apiVersion: worker.gpustack.ai/v1
kind: KVCachePoolBinding  # namespaced, short name kvcpb
metadata:
  name: team-a
  namespace: team-a
spec:
  poolRef:  # required; immutable
    name: shared-dram
  quota:  # required
    ceiling: 600Gi  # requested ceiling; effective grant scales proportionally on oversubscription
  domain:  # required; all fields are immutable
    name: qwen-72b-v2  # optional, default: "default"
    blockSize: 64  # required
    dtype: fp8_e4m3  # required; "auto" refused on new Bindings
```

## Cache reuse domains

Each `KVCachePoolBinding` registers one reuse domain on its backend's leader:

- **Default name**: Omitting *`spec.domain.name`* registers `default`. Two Bindings omitting
  *`domain.name`* on the same backend collide, so name each domain when multiple Bindings share a
  leader.
- **Per-leader uniqueness**: Admission rejects a Binding whose *`domain.name`* is already claimed by
  another Binding on the same `KVCacheBackend` (even across different `KVCachePool`s). Separate
  backends keep separate ledgers and can reuse names.
- **Multi-tenancy requirement**: Admission rejects a second distinct domain when the backend has no
  tenant ledger (`leader.multiTenancy: false` or observed absent). When ledger support is unconfirmed,
  admission warns and requires an engine image meeting [Tenant compatibility](#tenant-compatibility).
- **Admin-only registration**: Workloads cannot mint new tenants at runtime; an unregistered domain
  receives `TENANT_NOT_REGISTERED`.

The pool lists registered domains in *`status.domains`*. On the Binding, `DomainExclusive` confirms
name ownership and `QuotaGranted` confirms a positive effective grant.

### Immutable domain fields

*`spec.poolRef`* and every field under *`spec.domain`* (*`name`*, *`blockSize`*, and *`dtype`*) are
immutable after creation because changing them strands ledger entries or corrupts cached block
layouts. Delete and recreate the Binding to change them; existing cached blocks do not carry over.

### Engine cache precision

When the `model-deployment-kv-cache-dtype-owned` `Setting` is `true` (default), the operator passes
*`spec.domain.dtype`* verbatim as `--kv-cache-dtype <dtype>` to every managed `ModelDeployment` role
and [injected `Pod`](#pod-injection), refusing conflicting user flags
([Operator-owned keys](/gpustack-operator/v0.12.1/docs/modules/model-deployment/configuration/index.md#operator-owned-keys)).

> **Why**: Store keys omit the element type, and an engine reading a narrower cached block into a
> wider buffer receives a partial payload without an error.

- **`auto` refusal**: Admission rejects `dtype: auto` on new Bindings because different models
  resolve `auto` to different types. Legacy `auto` Bindings remain updatable; see
  [Upgrading to an Enforced Binding Dtype](/gpustack-operator/v0.12.1/docs/operate/migration/enforced-binding-dtype/index.md).
- **Cross-engine spelling**: When both engines share one Binding, pick a shared spelling
  (`bfloat16`, `fp8_e4m3`, `fp8_e5m2`, or `nvfp4`).

| Engine | Accepted `dtype` values | Rejected aliases |
|---|---|---|
| `vLLM` (including Ascend) | `bfloat16`, `float16`, `fp8`, `fp8_e4m3`, `fp8_e5m2`, `fp8_inc`, `fp8_ds_mla`, `nvfp4` | `bf16` |
| `SGLang` | `bfloat16`, `bf16`, `fp8_e4m3`, `fp8_e5m2`, `mxfp8`, `nvfp4`, `fp4_mx_block16` | `fp8`, `float16` |

Values above reflect vLLM `v0.29.0` and SGLang `v0.5.18`; the running engine image is authoritative.

## Capacity grants

A namespace has no quota on a pool until an administrator creates a `KVCachePoolBinding` in that
namespace. The Binding registers *`spec.domain.name`* as a tenant ID in the store's ledger and sets
its ceiling.

At runtime, workloads send the reuse domain name, not the Binding name. Workloads sharing a domain
name share cached blocks and one quota ledger; workloads with distinct domains keep separate caches
and ledgers.

### Requested and effective quota

*`spec.quota.ceiling`* is the requested namespace limit (required because the store rejects tenants
without a policy). *`status.effectiveQuota`* is the byte grant computed by the store:

- **Within allocatable capacity**: *`effectiveQuota`* equals *`ceiling`*.
- **Oversubscribed**: The store scales each domain's grant proportionally to its requested
  *`ceiling`*.

On `KVCachePool`, `QuotaWithinTotal` turns `False` (`Oversubscribed`) when Binding ceilings exceed
*`spec.quota.total`* without blocking writes. `QuotaWithinTotal=True` compares ceilings only against
*`spec.quota.total`*; if mounted member capacity is smaller, *`effectiveQuota`* still scales down to
what members mounted.

### Full-quota behavior

Reaching *`effectiveQuota`* triggers per-domain eviction rather than immediate write failures:

- **Silent replacement**: New writes discard unleased objects in the same domain (in arbitrary shard
  order) while global eviction counters remain `0` and *`status.usage`* stays at *`effectiveQuota`*.
- **Lease exhaustion (`TENANT_QUOTA_EXCEEDED`)**: Reading an object leases it (`5m` by default; see
  [KV Cache Election and Failover](/gpustack-operator/v0.12.1/docs/modules/kv-cache/leader/index.md#leader-configuration)). Writes fail only when every
  object in a full grant holds an active lease.
- **`status.overQuota` meaning**: Because writes cannot push usage past the grant, *`overQuota`*
  turns `true` only when a proportional recalculation cuts *`effectiveQuota`* below bytes the domain
  already holds. Compare *`status.usage`* against *`status.effectiveQuota`* to detect saturation.

Neither `KVCachePool` nor `KVCachePoolBinding` exposes eviction settings or counters because store
eviction watermarks and metrics are process-global on the leader. Configure global eviction flags in
*`spec.connection.managed.leader.extraArgs`* ([KV Cache Election and Failover](/gpustack-operator/v0.12.1/docs/modules/kv-cache/leader/index.md)).

### Quota policy file

The pool controller renders all Binding ceilings on a backend into one seed `ConfigMap`. An init
container copies that seed onto a writable volume (writing an empty policy document if no pool has
bound the backend yet) so the leader can atomically rename updated policy files on admin API writes.

If `QuotaPolicyWritable` on `KVCachePool` is `False`, policy updates have stopped reaching the
leader; inspect the pool condition message and leader logs.

### Unavailable store capacity

When no member segments are mounted (on initial startup or for `~5–30 s` while members remount after
a leader restart), allocatable capacity is zero. Both the pool (`CapacityAllocatable=False`, reason
`NothingToAllocate`) and its Bindings (`QuotaGranted=False`) leave `Ready` until segments remount so
workloads do not treat a zero-grant store as healthy.

Single-tenant backends (`leader.multiTenancy: false` or external masters without a ledger) are the
one exception: `QuotaLedgerAvailable` reports `False/MultiTenancyDisabled` while the pool stays
`Ready` for its single domain.

## Pod injection

Standalone `Pod`s can attach to a `KVCachePoolBinding` via the opt-in mutating webhook. `Pod`s
without `kvcache.gpustack.ai/inject: "true"` are ignored.

### Pod injection configuration

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: chat-0
  namespace: team-a
  labels:
    kvcache.gpustack.ai/inject: "true"  # required opt-in label; immutable
  annotations:
    kvcache.gpustack.ai/binding: team-a  # required KVCachePoolBinding in this namespace
    kvcache.gpustack.ai/engine: vLLM  # required; enum: vLLM, SGLang
    kvcache.gpustack.ai/manufacturer: ascend  # optional; vLLM only (selects vLLM-Ascend)
    kvcache.gpustack.ai/role: Prefill  # optional; vLLM only (Prefill or Decode; omit for plain server)
    kvcache.gpustack.ai/container: server  # required when Pod has multiple containers
spec:
  containers:
    - name: server
      image: vllm/vllm-openai:v0.29.0
      command:
        - vllm
      args:
        - serve
        - --model
        - Qwen/Qwen2.5-7B-Instruct
      resources:
        requests:
          memory: 16Gi  # include +128 MiB (vLLM) or +16 MiB (SGLang) staging overhead
        limits:
          memory: 16Gi
```

- **Same-namespace scope**: `kvcache.gpustack.ai/binding` resolves only in the `Pod` namespace (`/`
  is refused), and `inject`, `injected`, and `client-config` freeze after creation.
- **Injected flags**: vLLM receives `--kv-transfer-config`, `MOONCAKE_CONFIG_PATH`,
  `--kv-cache-dtype`, and `/etc/gpustack/kvcache` (never pass `kv_offloading_size`). SGLang receives
  `--hicache-storage-backend mooncake`, `--enable-hierarchical-cache`, `--kv-cache-dtype`, and
  `MOONCAKE_*` variables (reserving `hicache_ratio` `2.0` × device KV pool + `10 GiB` host RAM).

### Refused engine launch

The webhook fails closed when injection or pool readiness is invalid. Inspect `Warning` events on
the owning controller (`Deployment` or `ReplicaSet`) and container logs:

```bash
kubectl -n <namespace> get events --field-selector type=Warning
kubectl -n <namespace> describe rs <replicaset-name>
kubectl -n <namespace> logs <pod-name> -c <container-name>
```

| Symptom | Expected signal | Resolution |
|---|---|---|
| Missing required annotation | Refusal naming `binding` or `engine` | Set `kvcache.gpustack.ai/binding` (same namespace) and `kvcache.gpustack.ai/engine` (`vLLM` or `SGLang`). |
| Invalid role or target container | Refusal naming `Server`, missing `container`, or init container | Omit `role` on standard servers (`Server` is refused); name an app container in `kvcache.gpustack.ai/container`. |
| Pool or ledger not ready | Refusal naming `QuotaLedgerAvailable` or `status.clientEndpoint` | Restore the `KVCacheBackend` leader and wait for `KVCachePool` *`status.clientEndpoint`*. |
| Conflicting flag, env, or mount | Refusal naming `--kv-transfer-config`, `MOONCAKE_CONFIG_PATH`, `--hicache-storage-backend`, `--kv-cache-dtype`, or `/etc/gpustack/kvcache` | Remove the duplicate flag, env var, or mount from the `Pod` spec so it matches *`spec.domain.dtype`*. |
| Shell `-c` or `env -S` launch | Refusal rejecting `sh -c`, `bash -c`, or `env -S` | Put the engine binary in *`command`* and its flags in *`args`*; the forwarding annotation does not override shell `-c` refusals. |
| Wrapper script or bare `args` | Refusal rejecting bare `args` or a `.sh` wrapper | Launch the engine binary directly, or set `kvcache.gpustack.ai/launch-args-forwarded: "true"` only when the wrapper forwards appended arguments (`"$@"`). |
| Transport mismatch | Admission `NotSupportedTransport` or vLLM-Ascend `NotImplementedError` for `tcp` | Set `spec.transport.protocol: CANN` on Ascend backends or align member group protocols. |

### Pod binding verification

Verify injection on an admitted `Pod` by reading `kvcache.gpustack.ai/injected`:

```bash
kubectl -n <namespace> get pod <pod-name> -o jsonpath='{.metadata.annotations.kvcache\.gpustack\.ai/injected}{"\n"}'
```

Captured output:

```text
{"binding":"chat","engine":"vllm","vehicle":"file","domain":"team-a-chat",
 "tenantInjected":true,"launchProgram":"vllm","launchArgsForwarded":false}
```

Confirm `"binding"` and `"domain"` match your `KVCachePoolBinding` and `"tenantInjected"` is `true`
(for a `ModelDeployment`, also check `CacheAttached` in *`status.conditions`*). On vLLM `Pod`s,
confirm the image carries the Mooncake store connector:

```bash
kubectl -n <namespace> exec <pod-name> -- python3 -c \
  "import vllm.distributed.kv_transfer.kv_connector.v1.mooncake.store.worker; print('reader present')"
```

### Tenant compatibility

`tenantInjected: true` confirms only that the webhook wrote `tenant_id` (vLLM) or
`MOONCAKE_TENANT_ID` (SGLang); the engine image must also support tenant arguments:

- **Minor-line match**: Verify `pip show mooncake-transfer-engine` (or `mooncake-transfer-engine-npu`
  on Ascend) matches the backend store's minor line ([Version compatibility](/gpustack-operator/v0.12.1/docs/modules/kv-cache/backend/index.md#version-compatibility)).
- **Non-`default` domain floor**: A *`spec.domain.name`* other than `default` requires
  `mooncake-transfer-engine >= 0.3.13`; older builds fail startup with
  `TypeError: setup(): incompatible function arguments`.
- **Legacy image fallback**: Omit *`spec.domain.name`* (or set `default`) when running pre-`0.3.13`
  engine images.

## Quota and deletion checks

Compare *`effectiveQuota`* and *`usage`* with `kubectl get kvcpb` (`EFFECTIVE` and `USAGE` stay
blank when `multiTenancy` is `false`):

```text
NAME     POOL          DOMAIN        EFFECTIVE   USAGE    PHASE   AGE
team-a   shared-dram   qwen-72b-v2   450Gi       280Gi    Ready   6d
```

When deleting a `KVCachePoolBinding` or `KVCachePool` stays in `Deleting`, inspect `Releasable=False`
and act on the owner named by its reason:

| `Releasable` reason | Cause | Resolution |
|---|---|---|
| `HeldByWorkloads` | Workloads in *`status.usedBy`* still reference the Binding | Delete or detach the named workloads in the namespace. |
| `LedgerNotReleased` | Domain still holds bytes on the leader, or leader is unreachable during pool teardown | Drain the domain or restore leader `:9003` reachability (retries every `30 s`). |
| `LedgerRequestFailed` | Admin API call failed, leaving ledger state unknown | Inspect `KVCachePool` and `KVCacheBackend` conditions and restore the leader. |

If an external or permanently destroyed leader will never return, clear the pool finalizer manually
(this orphans quota entries if the leader is still live):

```bash
kubectl patch kvcp <pool> --type=merge -p '{"metadata":{"finalizers":null}}'
```

## Limitations

**A Binding is a provisioning and accounting record, not an access boundary.** Any `Pod` in the
cluster that can reach the leader `Service` and knows a registered domain name can read or write
that domain's blocks without credentials. Do not place mutually distrusting tenants on a shared
backend without external network isolation.

Managed backends enable the tenant ledger by default unless `leader.multiTenancy: false` is set (required
below Mooncake `0.3.12`; see [GPUStack build variants](/gpustack-operator/v0.12.1/docs/modules/kv-cache/backend/index.md#gpustack-build-variants)). When a
backend runs without a tenant ledger:

- Admission allows a `KVCachePool` on a managed single-tenant backend with a warning that per-tenant
  quotas are inactive (external backends are discovered at reconcile time).
- Workloads omit the tenant ID and share the store's single default tenant.

---

**See also** — [KV Cache Backend](/gpustack-operator/v0.12.1/docs/modules/kv-cache/backend/index.md) (store configuration and member transports) · [Model Deployment Configuration](/gpustack-operator/v0.12.1/docs/modules/model-deployment/configuration/index.md) (attaching roles through *`spec.kvCache.poolRef`*) · [Shared Cache Walkthrough](/gpustack-operator/v0.12.1/docs/walkthroughs/kv-cache/shared-cache/index.md) (end-to-end setup and verification)

**Next** → [Accelerator Requests](/gpustack-operator/v0.12.1/docs/modules/devices/requests/index.md) — request whole, sliced, or partitioned accelerators.
