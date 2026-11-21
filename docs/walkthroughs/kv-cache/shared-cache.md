# Shared Cache Walkthrough

Stand up a Mooncake cache backend, pool, namespace binding, and serving deployment in dependency
order, then add leader high availability, a local disk tier, and multi-tenant bindings.

## Contents

- [Summary](#summary)
- [Environment](#environment)
- [Step 1: Create the store](#step-1-create-the-store)
- [Step 2: Create pool and binding](#step-2-create-pool-and-binding)
- [Step 3: Create the workload](#step-3-create-the-workload)
- [Step 4: Enable high availability](#step-4-enable-high-availability)
- [Step 5: Add a disk tier](#step-5-add-a-disk-tier)
- [Step 6: Add a second team](#step-6-add-a-second-team)
- [Limitations](#limitations)
- [Troubleshooting](#troubleshooting)

## Summary

A shared Mooncake cache lets serving replicas reuse prefix KV blocks across DRAM and local disk tiers.
Create the four objects in order because each resource references the one above it
([Two kinds by scope](../../modules/kv-cache/pool.md#two-kinds-by-scope)):

| Object | Scope | Creator | Purpose |
|---|---|---|---|
| `KVCacheBackend` | cluster | administrator | Deploys Mooncake leader and per-node memory/disk members |
| `KVCachePool` | cluster | administrator | Caps total cache capacity exposed from one backend |
| `KVCachePoolBinding` | namespace | administrator | Grants one namespace a reuse domain and quota ceiling |
| `ModelDeployment` | namespace | user | Attaches serving roles to the namespace `KVCachePoolBinding` |

Workloads always set *`kvCache.poolRef.name`* to the namespace `KVCachePoolBinding`, never the
cluster-scoped `KVCachePool`.

## Environment

| Item | Value |
|---|---|
| Cluster | Kubernetes with GPUStack installed |
| Contributing nodes | Matched by *`members[].nodeSelector`* (`kubernetes.io/os: linux` in Step 1; `kvcache: "true"` in Step 5) |
| `AcceleratorType` | An `Active` pool from `kubectl get acctype` |
| Model and engine | `Qwen/Qwen2.5-7B-Instruct` on vLLM `gpustack/runner:cuda13.0-vllm0.29.0` |

## Step 1: Create the store

Save `mooncake-dram.yaml`, replacing `kubernetes.io/os: linux` with labels selecting the nodes that
should contribute DRAM:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: KVCacheBackend
metadata:
  name: mooncake-dram
spec:
  type: Mooncake
  connection:
    managed:
      leader: {}  # multiTenancy defaults to true; spec.image uses Setting kv-cache-backend-image
      members:
        - nodeSelector:
            kubernetes.io/os: linux
          medium: DRAM
          capacityPerMember: 8Gi  # requested from node host memory per matched node
```

Keep *`spec.image`* unset so `Setting/kv-cache-backend-image` supplies GPUStack's Mooncake build,
which supports `Lease` leader election in Step 4 ([High availability](../../modules/kv-cache/leader.md#high-availability))
and per-tenant quota ledgers (`multiTenancy: true`, supported from Mooncake 0.3.12; see
[GPUStack build variants](../../modules/kv-cache/backend.md#gpustack-build-variants)).

Apply `mooncake-dram.yaml` and wait for `PHASE=Ready`:

```bash
kubectl apply -f mooncake-dram.yaml
kubectl get kvcb mooncake-dram -w
```

Captured output:

```text
NAME            TYPE       PHASE   ENDPOINT                                         CAPACITY
mooncake-dram   Mooncake   Ready   mooncake-dram-leader.gpustack-system.svc:50051   16Gi
```

`CAPACITY` reflects live member registrations reported by the Mooncake master (`2 × 8Gi = 16Gi` here).
If `CAPACITY` is smaller than expected or `PHASE` stays `Degraded`, run `kubectl describe kvcb mooncake-dram`
and inspect `MembersMounted` in [Troubleshooting](#troubleshooting).

## Step 2: Create pool and binding

Create namespace `team-a` if needed, then apply `pool-and-binding.yaml`:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: KVCachePool
metadata:
  name: shared-dram
spec:
  backends:  # exactly one backend; immutable
    - mooncake-dram
  quota:
    total: 16Gi
---
apiVersion: worker.gpustack.ai/v1
kind: KVCachePoolBinding
metadata:
  name: team-a
  namespace: team-a
spec:
  poolRef:
    name: shared-dram
  quota:
    ceiling: 8Gi  # cap on concurrent cached bytes, not a static reservation
  domain:  # immutable compatibility key; omitted domain.name defaults to "default"
    blockSize: 64
    dtype: bfloat16
```

```bash
kubectl apply -f pool-and-binding.yaml
kubectl -n team-a get kvcachepoolbinding team-a \
  -o jsonpath='{.status.effectiveQuota}{"\t"}{.status.conditions[?(@.type=="QuotaGranted")].status}{"\n"}'
```

Example output (illustrative):

```text
8Gi	True
```

Key constraints on *`domain`* and *`quota`*:

- **Immutable reuse domain**: workloads sharing one Binding share KV blocks, so `name`, *`blockSize`*,
  and `dtype` cannot be edited in place. Create a separate Binding for a different model or `dtype`.
- **Quota ceiling**: reaching *`ceiling`* evicts older entries rather than failing writes
  ([Full-quota behavior](../../modules/kv-cache/pool.md#full-quota-behavior)). If a backend sets
  `leader.multiTenancy: false`, `QuotaGranted=True/Unenforced` warns that quotas are unledgered.
- **Tenant compatibility**: a multi-tenant master rejects unregistered tenant IDs, so engines must
  honor injected tenant settings (for non-`default` domains, `mooncake-transfer-engine >= 0.3.13`; see
  [Tenant compatibility](../../modules/kv-cache/pool.md#tenant-compatibility)).

## Step 3: Create the workload

Substitute an `Active` `AcceleratorType` from `kubectl get acctype` and apply `qwen-chat.yaml`:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: qwen-chat
  namespace: team-a
spec:
  model:
    name: Qwen/Qwen2.5-7B-Instruct
  engine:
    name: vLLM
    image: gpustack/runner:cuda13.0-vllm0.29.0  # Mooncake client minor version must match backend store
  kvCache:
    poolRef:
      name: team-a  # KVCachePoolBinding in team-a
  roles:
    - name: server
      replicas: 2
      acceleratorType: <one from kubectl get acctype>
      resources:
        accelerator: "1"
```

```bash
kubectl apply -f qwen-chat.yaml
kubectl -n team-a get md qwen-chat -o json |
  jq '{binding: .status.kvCache.binding, pool: .status.kvCache.pool,
       attached: (.status.conditions[] | select(.type=="CacheAttached") | {status, reason})}'
```

Example output (illustrative):

```json
{
  "binding": "team-a",
  "pool": "shared-dram",
  "attached": {
    "status": "True",
    "reason": "CacheActive"
  }
}
```

Confirm *`status.kvCache.binding`* equals `"team-a"` and `CacheAttached` is `True` (or `Unknown/NoObservationAvailable`
before traffic arrives). On a standalone `Pod` using webhook injection
([Pod injection](../../modules/kv-cache/pool.md#pod-injection)), verify the injected binding identity
on the admitted `Pod`:

```bash
kubectl -n team-a get pod <pod-name> -o jsonpath='{.metadata.annotations.kvcache\.gpustack\.ai/injected}{"\n"}'
```

On vLLM, confirm the runner image carries the Mooncake store connector:

```bash
kubectl -n team-a exec <pod-name> -- python3 -c \
  "import vllm.distributed.kv_transfer.kv_connector.v1.mooncake.store.worker; print('reader present')"
```

If creation fails with `FailedCreate` or `TypeError` at startup, match the symptom in
[Troubleshooting](#troubleshooting) ([Version compatibility](../../modules/kv-cache/backend.md#version-compatibility)).

## Step 4: Enable high availability

Scale the Mooncake leader from one replica to three without rolling existing member `DaemonSet` `Pod`s:

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: KVCacheBackend
 metadata:
   name: mooncake-dram
 spec:
   type: Mooncake
   connection:
     managed:
-      leader: {}
+      leader:
+        replicas: 3
       members:
         - nodeSelector:
             kubernetes.io/os: linux
           medium: DRAM
           capacityPerMember: 8Gi
```

Verify election and rollout conditions after applying the change:

```bash
kubectl get kvcb mooncake-dram \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'
```

In steady state, `Deployment/mooncake-dram-leader` shows **`1/3` Ready** intentionally: only the
active `Lease` holder reports Ready so the leader `Service` never routes traffic to standbys. Watch
two conditions ([Health checks](../../modules/kv-cache/leader.md#health-checks)):

| Condition | `True` meaning | `False` meaning |
|---|---|---|
| `ElectionObserved` | Active `Lease` names a holder | Ready leader with empty `Lease` (wrong image, missing RBAC, or initial election in flight) |
| `RolloutComplete` | All replicas run the current template | Rollout in progress or stalled |

### Failover behavior

Standby leaders hold no in-memory index. After a leader restart or failover, members remount their
segments automatically (~30 s), and cached blocks start cold ([High availability](../../modules/kv-cache/leader.md#high-availability)).
Master snapshot flags are refused in *`leader.extraArgs`* to prevent stale key collisions.

## Step 5: Add a disk tier

Admission rejects adding *`localDisks`* to an existing member group in place. Prepare the host
directory on every `kvcache: "true"` node first (owned by UID `65532` for the default image; see
[Directory requirements](../../modules/kv-cache/local-disk-tier.md#directory-requirements)):

```bash
install -d -o 65532 -g 0 -m 0750 /var/lib/kvcache
```

Then create a new `KVCacheBackend` (along with its own `KVCachePool` and `KVCachePoolBinding`, since
*`pool.spec.backends`* is immutable):

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: KVCacheBackend
 metadata:
-  name: mooncake-dram
+  name: mooncake-disk
 spec:
   type: Mooncake
   connection:
     managed:
       leader: {}
       members:
         - nodeSelector:
-            kubernetes.io/os: linux
+            kvcache: "true"
           medium: DRAM
           capacityPerMember: 8Gi
+          localDisks:
+            - path: /var/lib/kvcache
+              capacity: 100Gi
```

Verify `kubectl get kvcb mooncake-disk` reaches `Ready` with combined DRAM + disk `CAPACITY`. Because
the disk tier flushes in buckets, small workloads may keep disk usage at zero initially
([Bucket writes](../../modules/kv-cache/local-disk-tier.md#bucket-writes)).

## Step 6: Add a second team

Grant `team-b` an isolated reuse domain on `shared-dram`:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: KVCachePoolBinding
metadata:
  name: team-b
  namespace: team-b
spec:
  poolRef:
    name: shared-dram
  quota:
    ceiling: 8Gi
  domain:
    name: llama-8b-v1  # unique per master
    blockSize: 16
    dtype: bfloat16
```

To run an `fp8_e4m3` model in `team-a`, create a second Binding with its own *`domain.name`* and
`dtype`:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: KVCachePoolBinding
metadata:
  name: qwen-fp8
  namespace: team-a
spec:
  poolRef:
    name: shared-dram
  quota:
    ceiling: 4Gi
  domain:
    name: qwen-fp8-v1
    blockSize: 16
    dtype: fp8_e4m3
---
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: qwen-chat-fp8
  namespace: team-a
spec:
  model:
    name: Qwen/Qwen2.5-7B-Instruct
  engine:
    name: vLLM
    image: gpustack/runner:cuda13.0-vllm0.29.0
  kvCache:
    poolRef:
      name: qwen-fp8
  roles:
    - name: server
      replicas: 1
      acceleratorType: <one from kubectl get acctype>
      resources:
        accelerator: "1"
```

The operator injects `--kv-cache-dtype fp8_e4m3` onto `qwen-chat-fp8` and rejects manual
`--kv-cache-dtype` overrides ([Engine dtype](../../modules/kv-cache/pool.md#engine-dtype)).

Because the three ceilings (`8Gi + 8Gi + 4Gi = 20Gi`) exceed `shared-dram`'s `16Gi` total, `shared-dram`
reports `QuotaWithinTotal=False/Oversubscribed` and scales each Binding's *`status.effectiveQuota`*
proportionally ([Ceiling and grant](../../modules/kv-cache/pool.md#ceiling-and-grant)).

## Limitations

- Reuse-domain isolation, engine client floors, and `dtype` / *`blockSize`* matching are covered in
  [Limitations](../../modules/kv-cache/pool.md#limitations) and
  [Tenant compatibility](../../modules/kv-cache/pool.md#tenant-compatibility).
- Active workloads hold a finalizer on their `KVCachePoolBinding` (`Releasable=False/HeldByWorkloads`)
  until deleted ([Operating notes](../../modules/kv-cache/pool.md#operating-notes)).

## Troubleshooting

| Symptom | Signal | Resolution |
|---|---|---|
| `Pod` or `ReplicaSet` injection refused | Event `FailedCreate` naming `binding`, `engine`, flag collision, or `QuotaLedgerAvailable` | Update the owning `Deployment` or `Pod` spec: fix the `kvcache.gpustack.ai/binding` annotation, remove owned `--kv-transfer-config` / `--kv-cache-dtype` flags, or wait for pool readiness ([Refused engine launch](../../modules/kv-cache/pool.md#refused-engine-launch)). |
| `vllm-ascend` fails on startup | `NotImplementedError: MooncakeBackend does not support protocol 'tcp'` | Patch `KVCacheBackend` *`spec.transport.protocol`* to `CANN`. |
| Engine fails on `setup()` | `TypeError: setup(): incompatible function arguments` | Runner image has `mooncake-transfer-engine < 0.3.13`; upgrade the runner image or use a Binding with `domain.name: default`. |
| Cache writes fail with `TENANT_NOT_REGISTERED` | Engine log error or missing connector `ImportError` | Confirm the `KVCachePoolBinding` is `QuotaGranted=True` in the workload's namespace and the runner carries `MooncakeStoreConnector`. |
| `KVCacheBackend` stuck in `Provisioning` or `Error` | `LeaderAvailable=False` | Inspect `Deployment/<name>-leader` logs and image, or verify external endpoint reachability. |
| `KVCacheBackend` stays `Degraded` | `MembersMounted=False` (`SegmentsShort` or `NoSegments`) | Check node memory capacity and member `DaemonSet` logs on matched nodes. |
| `KVCachePool` reports `Error` | `BackendResolved=False` or `CapacityAllocatable=False` | Create or repair the referenced `KVCacheBackend` so its `CAPACITY` is non-zero. |
| `KVCachePool` `QuotaLedgerAvailable=False` | `Reason: MultiTenancyDisabled` | Expected when `KVCacheBackend` sets `leader.multiTenancy: false`; enable `multiTenancy` to enforce ceilings. |
| `KVCachePoolBinding` shows `effectiveQuota: 0` | `QuotaGranted=False/ZeroGranted` | Wait ~30 s if the leader just restarted (`NothingToAllocate`), or raise pool *`quota.total`* / lower sibling ceilings. |
| Pool reports `Oversubscribed` | `QuotaWithinTotal=False/Oversubscribed` | Expected proportional recut when ceilings exceed *`quota.total`*; inspect *`status.effectiveQuota`* on each Binding. |
| Binding apply rejected on `domain.name` | `Duplicate` on *`spec.domain.name`* | Another Binding on the same backend already claims that domain (`default` when omitted); set a unique *`domain.name`*. |
| Two Bindings claim one domain | `DomainExclusive=False/DomainClaimedByMultipleBindings` | Delete one of the racing `KVCachePoolBinding` objects. |
| Disk-tier member stuck or unready | `FailedMount` or `no write permission on directory` | Create the host directory on every matched node with `install -d -o 65532 -g 0 -m 0750 <path>` ([Directory requirements](../../modules/kv-cache/local-disk-tier.md#directory-requirements)). |
| Disk-tier init container crash-loops | `MembersMounted=False/MemberInitCrashLooping` | Use a member image that includes `sh` for the directory survey init container. |
| Disk tier warns of preexisting files | `TierWasEmpty=False/PreexistingContent` | Empty stale files from the host path manually (`cleanAfterDelete: true` cleans only on backend deletion). |
| HA leader `Deployment` shows `1/3` Ready | Expected leader-election steady state | No action needed; two standbys remain intentionally unready until failover. |
| `KVCachePoolBinding` deletion hangs | `Releasable=False/HeldByWorkloads` | Delete attached `ModelDeployment`s and injected `Pod`s before deleting the Binding. |

---

**See also** — [KV Cache Backend](../../modules/kv-cache/backend.md) (backend fields and status) ·
[KV Cache Election and Failover](../../modules/kv-cache/leader.md) (leader election and member
addressing) · [KV Cache Pools and Bindings](../../modules/kv-cache/pool.md) (quotas, domains, and
injection) · [KV Cache Local Disk Tier](../../modules/kv-cache/local-disk-tier.md) (disk directory and
bucket rules) · [Configuration](../../modules/model-deployment/configuration.md) (`ModelDeployment`
role configuration)

**Next** → [Model Prefetch Walkthrough](../model-delivery/prefetch.md) — cache weights on GPU nodes before serving.
