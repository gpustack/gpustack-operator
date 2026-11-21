# KV Cache Local Disk Tier

At most one member group on a managed `KVCacheBackend` can declare a single *`localDisks`* entry to
offload evicted memory blocks onto node-local storage. Declaring *`localDisks`* automatically
enables deferred offload (`-enable_offload=true -offload_on_evict=true`) on the leader.

## Contents

- [Configuration](#configuration)
- [Bucket writes](#bucket-writes)
- [Eviction](#eviction)
- [Cleanup on deletion](#cleanup-on-deletion)
- [Disk and memory sizing](#disk-and-memory-sizing)

## Configuration

```yaml
apiVersion: worker.gpustack.ai/v1
kind: KVCacheBackend
metadata:
  name: shared-mooncake
spec:
  connection:
    managed:
      members:
        - nodeSelector:
            kvcache: "true"
          medium: DRAM  # official images use DRAM; custom VRAM images see Backend guide
          capacityPerMember: 500Gi  # must be >= 16Mi when localDisks is set
          localDisks:  # at most one group per backend, single entry keyed by path
            - path: /var/lib/kvcache  # required; must pre-exist on every selected Node
              capacity: 4Ti  # optional; defaults to store ceiling (>= 16Mi when set)
              keyLimit: 10000000  # optional; defaults to store limit (>= 64 when set)
              cleanAfterDelete: false  # optional, default: false
              eviction:  # optional
                enabled: true  # optional, default: true when eviction block is set
                policy: LRU  # optional, default: FIFO; enum: FIFO, LRU
                watermark:  # optional; percentages of capacity (low < high)
                  high: 90
                  low: 80
```

| Rendered setting | Target |
|---|---|
| `MOONCAKE_OFFLOAD_ENABLED`, `..._FILE_STORAGE_PATH`, `..._BUCKET_SIZE_LIMIT_BYTES` (`16 MiB`), `..._BUCKET_KEYS_LIMIT` (`64`) | Member container |
| `..._TOTAL_SIZE_LIMIT_BYTES`, `..._BUCKET_MAX_TOTAL_SIZE` (from *`capacity`*), `..._TOTAL_KEYS_LIMIT` (from *`keyLimit`*) | Member container |
| `..._BUCKET_EVICTION_POLICY`, `..._ENABLE_DISK_WATERMARK_EVICTION`, watermark ratios | Member container |
| `Directory` *`hostPath`* mount at *`localDisks[].path`*, `preStop` hook, and *`scaleIn.gracePeriodSeconds`* window | Member `Pod` |
| `-enable_offload=true`, `-offload_on_evict=true` (refused in *`leader.extraArgs`*) | Leader argv |

The tier always runs in deferred mode, keeping queued objects in memory until their disk bucket
flushes.

### Host directory requirements

*`localDisks[].path`* mounts with `hostPath.type: Directory` (`fsGroup` does not apply to
`hostPath`). Pre-create the directory on every selected `Node`, owned by the member image's UID
(`65532` on official images):

```bash
docker run --rm --entrypoint id <your-member-image>
install -d -o 65532 -g 0 -m 0750 /var/lib/kvcache
```

- **Path validation**: Must be an absolute path without leading/trailing whitespace or `..`, not
  `/`, not overlapping `/dev/infiniband`, and (on `CANN` groups with *`fabricInterfaceCount`*) not
  overlapping `/etc/hixlep`.
- **Tier immutability**: Adding, removing, or repathing *`localDisks`* on an existing group is
  refused (*`capacity`*, *`keyLimit`*, *`cleanAfterDelete`*, and *`eviction`* remain editable). To
  remove a tier without deleting the whole `KVCacheBackend`, place the tier on the **last** entry in
  *`members[]`* so that group can be removed from the tail ([Group resizing](/gpustack-operator/main/docs/modules/kv-cache/backend/index.md#group-resizing)).

| Symptom | Signal | Resolution |
|---|---|---|
| Refused at `kubectl apply` | Admission error on *`localDisks`* | Fix the path rule violation, or avoid adding/removing/repathing *`localDisks`* in place. |
| Member `Pod` stuck `Pending` / `ContainerCreating` | `FailedMount ... hostPath type check failed` | Create *`localDisks[].path`* on the `Node` before starting the member. |
| Member `Pod` running, `0/1` Ready (`MembersMounted=False`) | Log: `FileStorageConfig: no write permission on directory` | `chown` the host directory to the member container's UID. |

## Bucket writes

Mooncake batches offloaded objects per member and writes to disk only when a member's bucket reaches
the operator-rendered threshold of **`16 MiB` or `64` keys** (upstream's `256 MB` / `500`-key
defaults leave smaller workloads unwritten; bucket env vars are owned and refused in *`extraEnv`*).

Admission enforces a one-bucket floor so no declared ceiling silently disables offload:
*`capacityPerMember`* (`>= 16Mi`), *`localDisks[].capacity`* (`>= 16Mi` when set), and
*`localDisks[].keyLimit`* (`>= 64` when set).

Because *`status.capacity`* reports declared capacity rather than written bytes, verify actual disk
writes with `master_allocated_file_size_bytes` on the leader:

```bash
kubectl exec -n gpustack-system deploy/<backend>-leader -- \
  python3 -c "import urllib.request;print([l for l in \
  urllib.request.urlopen('http://127.0.0.1:9003/metrics').read().decode().splitlines() \
  if l.startswith('master_allocated_file_size_bytes')])"
```

| Offered data | Zero gauge meaning |
|---|---|
| Below `16 MiB` / `64` keys per member | Expected: no member bucket has closed yet. |
| At least one bucket per member (after `~30 s`) | **Disk tier is not writing**: inspect member logs and directory permissions. |

## Eviction

*`localDisks[].eviction`* controls full-disk behavior:

| Setting | Full-disk behavior |
|---|---|
| Omitted | Uses Mooncake's default (`FIFO` eviction). |
| `enabled: true` (block default) | Evicts `FIFO` (oldest written) or `LRU` (least recently read) entries between `watermark.high` and `watermark.low`. |
| `enabled: false` | Stops accepting new disk writes while keeping existing disk entries readable. |

Admission rejects four no-op combinations: `enabled: false` with *`policy`*, `enabled: false` with
*`watermark`*, *`watermark`* without *`localDisks[].capacity`*, and `watermark.low >= watermark.high`.
Unmodeled Mooncake file-storage variables can be passed in *`members[].extraEnv`* (non-rendered keys
only; world-readable, so never pass credentials).

## Cleanup on deletion

By default (`cleanAfterDelete: false`), deleting a `KVCacheBackend` leaves *`localDisks[].path`*
untouched. Set `cleanAfterDelete: true` (requires `sh` in the member image, present in official
builds) to empty the directory's contents on selected `Node`s during backend deletion while keeping
the directory itself:

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: KVCacheBackend
 metadata:
   name: shared-mooncake
 spec:
   connection:
     managed:
       members:
         - medium: DRAM
           capacityPerMember: 500Gi
           localDisks:
             - path: /var/lib/kvcache
               capacity: 4Ti
+              cleanAfterDelete: true
```

### Retained cache files

Pointing a new `KVCacheBackend` at a non-empty directory lets Mooncake adopt leftover buckets, which
can serve stale bytes on key collisions. Each backend's initial member `Pod`s survey the directory
once at first start and write `TierWasEmpty=False` (with a Warning Event) if any `Node` had prior
files, or `TierWasEmpty=True` once all desired `Node`s report empty:

```bash
kubectl get kvcachebackend <name> -o jsonpath='{.status.conditions[?(@.type=="TierWasEmpty")]}{"\n"}'
```

### Cleanup limitations

Delete the `KVCacheBackend` **before** narrowing *`nodeSelector`*; cleanup runs only on `Node`s
still matched at deletion time. Cleanup does not block backend deletion indefinitely and records
`Node` Warning Events when skipped or cut short:

- **Per-node `5m` deadline**: Unreachable `Node`s (`KVCacheTierNotCleaned`) or large directories
  exceeding `5m` (`KVCacheTierPartlyEmptied`) cancel their cleanup `Pod` so a new backend cannot
  lose fresh data.
- **Overlapping path guard**: If another live `KVCacheBackend` uses an identical or nested
  *`localDisks[].path`*, cleanup skips or stops (`KVCacheTierSharedPath`).

```bash
kubectl describe node <node> | grep -E 'KVCacheTierNotCleaned|KVCacheTierSharedPath|KVCacheTierPartlyEmptied'
```

## Disk and memory sizing

A `Node` with terabytes of NVMe and limited spare RAM still requires a memory segment on the same
member group. Mooncake routes offload tasks to the client holding the key's **memory** replica, so a
group without memory would report disk capacity while storing nothing.

Kubernetes ephemeral-storage limits do not track or evict *`hostPath`* usage. Monitor the backing
filesystem yourself and set *`localDisks[].capacity`* inside the disk's free space.

Configure one group combining a thin `DRAM` segment with a large *`localDisks[].capacity`*:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: KVCacheBackend
metadata:
  name: mooncake-disk
spec:
  type: Mooncake
  connection:
    managed:
      leader: {}
      members:
        - nodeSelector:
            kvcache: "true"
          medium: DRAM  # required memory segment fronting the disk tier
          capacityPerMember: 8Gi  # sized for in-flight offload buckets plus hot reads
          localDisks:
            - path: /var/lib/kvcache
              capacity: 4Ti
              eviction:
                enabled: true
                policy: LRU
```

### Minimum memory segment

Admission enforces `capacityPerMember >= 16Mi` (one bucket) whenever *`capacityPerMember`* is
created or edited on a group with *`localDisks`* (existing smaller values are not re-judged on
unrelated edits).

`16Mi` only prevents silent non-writes; it leaves room for one in-flight bucket and zero memory
cache. Size *`capacityPerMember`* to hold concurrent in-flight offload buckets plus your hot memory
working set, while keeping `capacityPerMember + localBufferSize` within the `Node`'s allocatable RAM
so the member `Pod` schedules.

---

**See also** — [KV Cache Backend](/gpustack-operator/main/docs/modules/kv-cache/backend/index.md) (leader, member transports, and group resizing) · [KV Cache Pools and Bindings](/gpustack-operator/main/docs/modules/kv-cache/pool/index.md) (namespace grants and reuse domains)

**Next** → [KV Cache Pools and Bindings](/gpustack-operator/main/docs/modules/kv-cache/pool/index.md) — grant namespaces quota on this backend.
