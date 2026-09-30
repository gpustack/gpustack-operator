# KV Cache on Disk-Heavy Nodes

A node with terabytes of NVMe and little spare RAM still needs a memory segment for its cache
member. Add local disks to that member, then size the memory segment as described below.

## Contents

- [Memory segment requirement](#memory-segment-requirement)
- [Configuration](#configuration)
- [Memory segment floor](#memory-segment-floor)

## Memory segment requirement

**Every member group mounts a memory segment, and a disk tier is a layer on a group that already
holds one.** `members[].medium` names which memory (`DRAM` for the host's, `VRAM` for the
accelerator's), and the disk is declared beside it in `members[].localDisks`, a layer on either one.
See [Connection and medium](/gpustack-operator/main/docs/modules/kv-cache/backend/index.md#connection-and-medium) for the full shape.

The shape is the store's own data flow: the leader routes an offload task to the client holding the
key's **memory** replica, so a group with none is never chosen. It would still report its disk
capacity, so the failure is quiet: the tier looks healthy and holds nothing.

A disk-heavy node still needs enough memory to drive its disk tier. The next section gives the
group shape and the memory floor.

## Configuration

**Declare one group with a thin memory segment and a thick tier.** The terabytes go on
`localDisks[].capacity`; `capacityPerMember` is sized to drive the tier rather than to hold the cache,
and **declaring the tier on the group is the whole switch**: the leader's offload flags are derived
from that declaration, so there is no second half to forget.

```yaml
apiVersion: worker.gpustack.ai/v1
kind: KVCacheBackend
metadata:
  name: mooncake-disk
spec:
  type: Mooncake
  image: docker.io/kvcacheai/mooncake:0.3.13
  connection:
    managed:
      members:
        - nodeSelector:
            kvcache: "true"
          medium: DRAM                 # the SEGMENT, which is memory on every group
          capacityPerMember: 8Gi       # sized below — NOT a figure to copy
          localDisks:                  # declaring an entry is what turns the tier on
            - path: /var/lib/kvcache   # must already exist on every selected node
              capacity: 4Ti            # where the node's disk is declared
              eviction:
                enabled: true
                policy: LRU
```

Pin the example's `spec.image` per [The store version must match the engine's
client](backend.md#the-store-version-must-match-the-engines-client) before copying it.

**Rendering this shape correctly does not make the tier fill.** `status.capacity` will not tell
you either way. What decides it, and the figure to read instead, are in
[bucket writes](/gpustack-operator/main/docs/modules/kv-cache/local-disk-tier/index.md#bucket-writes).
Read that section before concluding this backend works.

Three things that manifest depends on and does not state:

- **The path must already exist on every selected node, owned by the image's user.** A member that
  cannot write it never becomes Ready. See
  [directory requirements](/gpustack-operator/main/docs/modules/kv-cache/local-disk-tier/index.md#directory-requirements).
- **The path is chosen once, at the first apply.** Adding a tier to a running group, removing it from
  one, or repathing it are each refused; the tier's other settings stay editable. See
  [The local disk tier](/gpustack-operator/main/docs/modules/kv-cache/local-disk-tier/index.md).
- **Nothing in Kubernetes accounts for what the tier writes.** Watching that filesystem is yours.
  See [unaccounted disk usage](/gpustack-operator/main/docs/modules/kv-cache/local-disk-tier/index.md#unaccounted-disk-usage).

## Memory segment floor

**`capacityPerMember` has a floor of 16Mi on a group declaring `localDisks`.** That is one bucket,
the unit the tier is written in. Why a segment below one bucket can never fill one is in
[bucket writes](/gpustack-operator/main/docs/modules/kv-cache/local-disk-tier/index.md#bucket-writes).

**The bound is judged only when a write moves the value** (on creation, or on an update that
changes it). An object already carrying a smaller figure is admitted for every other edit, including
the controller's own, because re-judging a figure the update left alone would strand an object
admitted before the bound existed.

**16Mi is where the failure stops being silent, not where a backend starts working.** It exists so
a segment too small to ever complete a bucket is refused at `kubectl apply` instead of producing a
tier that reports its capacity and holds nothing. A group sized at the floor has room for exactly one
bucket in flight and no cache in front of it.

**Size it against concurrent offload plus whatever the group should serve from memory**, both of
which are properties of the workload rather than of the disk. Every key on its way to the tier
occupies the segment until its bucket closes, so the segment has to hold the buckets in flight; a
segment holding only those is a write-through path to disk with no memory cache ahead of it.

**`capacityPerMember` is also a scheduling constraint.** It is counted into the member Pod's own
memory request, so a member that does not fit stays Pending rather than overcommitting the node.
That is what makes a thin segment worth having on a node whose RAM is already spoken for, and what
keeps the value from being free to raise later.

The tier's own ceilings carry a matching floor, and a disk-heavy node is nowhere near it. `capacity`
sits far above one bucket, and the example leaves `keyLimit` unset, so the store's own applies and
there is no declared ceiling to be near. Those two rules belong to
[bucket writes](/gpustack-operator/main/docs/modules/kv-cache/local-disk-tier/index.md#bucket-writes); the
floor that binds here is the memory one above.

---

**See also** — [KV Cache Backend](/gpustack-operator/main/docs/modules/kv-cache/backend/index.md) (the API this page configures, and where the tier's own
knobs, failure modes and exits are documented) · [KV Cache Pool](/gpustack-operator/main/docs/modules/kv-cache/pool/index.md) (how a namespace is granted
a quota on the store this backend runs) · [Settings & Environment Variables](/gpustack-operator/main/docs/reference/settings/index.md) (the
`kv-cache-backend-image` Setting)

**Next** → [KV Cache Pool](/gpustack-operator/main/docs/modules/kv-cache/pool/index.md) — granting a namespace a quota on the store configured here.
