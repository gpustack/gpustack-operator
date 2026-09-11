# KV Cache on Disk-Heavy Nodes

> **Purpose** — what to configure on a node whose capacity is disk rather than memory: why no member
> group can be disk alone, how the shape that works is written, and how small its memory segment may be.
> **Audience** operators · **Prerequisites** [KV Cache Backend](backend.md) · **Read time** ~5 min

A node with terabytes of NVMe and little spare RAM is the case this page is for. The configuration
that serves it exists, and it is not the one most people reach for first.

## Contents

- [There is no disk-only member](#there-is-no-disk-only-member)
- [The shape that works](#the-shape-that-works)
- [How small the memory segment may be](#how-small-the-memory-segment-may-be)

## There is no disk-only member

**Every member group mounts a memory segment, and a disk tier is a layer on a group that already
holds one.** `members[].medium` carries the single value `DRAM`, and the disk is declared beside it
in `members[].localDisk` — see [the two axes](backend.md#the-two-axes) for the full shape.

That is the store's own data flow rather than a simplification made here: the leader routes an
offload task to the client holding the key's **memory** replica, so a group with none is never
chosen. ⛔ It would still report its disk capacity, so what a disk-only group produces is not an
error but a tier that looks healthy and holds nothing.

Why the API is shaped that way is [the two axes](backend.md#the-two-axes); the upstream source both
halves were read from is in
[the spec](../../specs/2026-09-05-kv-cache-media-and-scaling.md#the-finding-that-decides-the-shape-offload-is-routed-to-the-memory-replicas-owner).

⇒ The question a disk-heavy node poses is therefore not "how do I declare a disk group" but "how
little memory does a group need in order to drive its disk".

## The shape that works

**Declare one group with a thin memory segment and a thick tier.** The terabytes go on
`localDisk.capacity`; `capacityPerMember` is sized to drive the tier rather than to hold the cache,
and both halves of the tier — the leader's and the group's — have to be present or the object is
refused at admission.

```yaml
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCacheBackend
metadata:
  name: mooncake-disk
spec:
  type: Mooncake
  image: docker.io/kvcacheai/mooncake:0.3.13
  connection:
    managed:
      leader:
        offload:
          enabled: true                # the leader's half; without it nothing is ever enqueued
          onEvict: true
      members:
        - nodeSelector: {kvcache: "true"}
          medium: DRAM                 # the SEGMENT, which is memory on every group
          capacityPerMember: 8Gi       # sized below — NOT a figure to copy
          localDisk:
            path: /var/lib/kvcache     # must already exist on every selected node
            capacity: 4Ti              # where the node's disk is declared
            eviction:
              enabled: true
              policy: LRU
```

⛔ **That is the supported shape, not a promise the tier fills.** Rendering it correctly is not
sufficient to make the tier hold data, and `status.capacity` will not tell you either way — what
decides it, and the figure to read instead, are in
[the tier is written one bucket at a time](local-disk-tier.md#the-tier-is-written-one-bucket-at-a-time).
Read that section before concluding this backend works.

Three things that manifest depends on and does not state:

- **The path must already exist on every selected node, owned by the image's user** — a member that
  cannot write it never becomes Ready. See
  [the directory has to exist](local-disk-tier.md#the-directory-has-to-exist-and-be-writable-by-the-images-user).
- **The path is chosen once, at the first apply.** Adding a tier to a running group, removing it from
  one, or repathing it are each refused; the tier's other settings stay editable. See
  [The local disk tier](local-disk-tier.md).
- **Nothing in Kubernetes accounts for what the tier writes** — watching that filesystem is yours.
  See [what the tier costs](local-disk-tier.md#what-the-tier-costs-that-nothing-accounts-for).

## How small the memory segment may be

**`capacityPerMember` has a floor of 16Mi on a group declaring `localDisk`.** That is one bucket —
the unit the tier is written in, and the figure this operator renders as `MemberBucketSizeLimit` in
`pkg/worker/kvcache/mooncake/member_workload.go`. Why a segment below one bucket can never fill one
is in [the tier is written one bucket at a time](local-disk-tier.md#the-tier-is-written-one-bucket-at-a-time).

⚠️ **The bound is judged only when a write moves the value** — on creation, or on an update that
changes it. An object already carrying a smaller figure is admitted for every other edit, including
the controller's own, because re-judging a figure the update left alone would strand an object
admitted before the bound existed.

⛔ **16Mi is where the failure stops being silent, not where a backend starts working.** It exists so
a segment too small to ever complete a bucket is refused at `kubectl apply` instead of producing a
tier that reports its capacity and holds nothing. A group sized at the floor has room for exactly one
bucket in flight and no cache in front of it.

**Size it against concurrent offload plus whatever the group should serve from memory**, both of
which are properties of the workload rather than of the disk. Every key on its way to the tier
occupies the segment until its bucket closes, so the segment has to hold the buckets in flight; a
segment holding only those is a write-through path to disk with no memory cache ahead of it.

⚠️ **`capacityPerMember` is also a scheduling constraint.** It is counted into the member Pod's own
memory request, so a member that does not fit stays Pending rather than overcommitting the node —
which is what makes a thin segment worth having on a node whose RAM is already spoken for, and what
stops it from being free to raise later.

The tier's own ceilings carry a matching floor, which a disk-heavy node is nowhere near: `capacity`
sits far above one bucket, and the example leaves `keyLimit` unset, so the store's own applies and
there is no declared ceiling to be near. Those two rules belong to
[the tier is written one bucket at a time](local-disk-tier.md#the-tier-is-written-one-bucket-at-a-time); the
floor that binds here is the memory one above.

---

**See also** — [KV Cache Backend](backend.md) (the API this page configures, and where the tier's own
knobs, failure modes and exits are documented) · [KV Cache Pool](pool.md) (how a namespace is granted
a quota on the store this backend runs) · [Settings & Environment Variables](../settings.md) (the
`kv-cache-backend-image` Setting)

**Next** → [KV Cache Pool](pool.md) — granting a namespace a quota on the store configured here.
