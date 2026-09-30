# KV Cache Local Disk Tier

A member group may declare a directory on each of its nodes, which configures the store client's
offload keys to point at it. **Declaring the entry is the whole switch**: the leader has no field of
its own and takes its offload flags from this list's presence. The list holds one entry, keyed by
`path` (`status.capacity` reports one pair of figures for the whole backend, so a second tier could
not be attributed).

```yaml
spec:
  connection:
    managed:
      members:
        - nodeSelector:
            kvcache: "true"
          medium: DRAM
          capacityPerMember: 500Gi
          localDisks:                    # declaring an entry is what turns the tier on
            - path: /var/lib/kvcache
              capacity: 4Ti              # optional; unset means the store's own ceiling
              keyLimit: 10000000         # optional; the same, on the key count
              eviction:                  # optional; unset means the store's own behaviour
                enabled: true            # default; false fills the tier and then stops writing
                policy: LRU              # FIFO | LRU; unset means the store's own, which is FIFO
                watermark:               # optional; percentages of capacity
                  high: 90
                  low: 80
```

| Rendered | Location |
|---|---|
| `MOONCAKE_OFFLOAD_ENABLED` and `..._FILE_STORAGE_PATH`, plus `..._BUCKET_SIZE_LIMIT_BYTES` and `..._BUCKET_KEYS_LIMIT` | the member container |
| `..._TOTAL_SIZE_LIMIT_BYTES` **and** `..._BUCKET_MAX_TOTAL_SIZE`, both from `capacity`; `..._TOTAL_KEYS_LIMIT` from `keyLimit` | the member container |
| `..._BUCKET_EVICTION_POLICY`, `..._ENABLE_DISK_WATERMARK_EVICTION` and the two ratio variables, from `eviction` | the member container |
| a `hostPath` volume and mount at `localDisks[].path` | the member Pod |
| `-enable_offload=true`, `-offload_on_evict=true` | the leader's argv, derived from this declaration |
| a `preStop` hook, and a termination window derived from `scaleIn.gracePeriodSeconds` | the member Pod |

**The mode is not selectable: the tier always runs deferred.** The leader renders
`-enable_offload=true -offload_on_evict=true` together, and only when a group declares a tier. The
flags come from the declaration rather than any field, and `leader.extraArgs` refuses both keys so
the leader cannot drift out of step with the members' declaration.

The store keeps an object queued for offload in memory until the disk write lands, which is the
branch `-offload_on_evict` selects. The write-through alternative, where an eviction can destroy the
sole replica of an object whose bucket has not been flushed, is not renderable by any shape of this
object, so there is nothing to opt into.

**A tier is a layer on a member group, never a group of its own.** See
[Connection and medium](/gpustack-operator/v0.9.0-rc3/docs/modules/kv-cache/backend/index.md#connection-and-medium) for why the shape has to be this way, and
[KV Cache on Disk-Heavy Nodes](/gpustack-operator/v0.9.0-rc3/docs/modules/kv-cache/disk-heavy-nodes/index.md) for what to write on a node that is mostly disk.

**Every example on this page is a `DRAM` group, and that is not incidental.** What is and is not
measured about pairing the tier with a `VRAM` group is stated once, beside the medium it belongs to,
at [VRAM group accelerator access](/gpustack-operator/v0.9.0-rc3/docs/modules/kv-cache/backend/index.md#vram-group-accelerator-access). Read it before writing
one.

## Contents

- [Bucket writes](#bucket-writes)
- [Eviction](#eviction)
- [Directory requirements](#directory-requirements)
- [Cleanup on deletion](#cleanup-on-deletion)
- [Unaccounted disk usage](#unaccounted-disk-usage)

## Bucket writes

The store does not write an offloaded object on its own. It **assembles objects into a bucket and
writes nothing until that bucket is full** (by bytes or by object count), and what is short of the
threshold is carried to the next attempt, indefinitely.

**The store's own thresholds are 256 MB and 500 objects, and a backend that never reaches either
has a tier that holds nothing while looking healthy.** The member Pods are Ready, the leader logs the
mount and reports objects deferred for offload, and `status.capacity` shows the size the tier
declared, because that figure is **capacity, not usage** (see
[Status and conditions](/gpustack-operator/v0.9.0-rc3/docs/modules/kv-cache/backend/index.md#status-and-conditions)).

This is what [issue #200](https://github.com/gpustack/gpustack-operator/issues/200) tracks.

**The operator renders a smaller pair of its own**, so a modest backend closes buckets. They are
**not in the API** and `extraEnv` refuses them: moving them is a tuning decision that would need a
field, not an escape hatch that silently defines the same variable twice.

Three bounds follow from the bucket being the unit, all enforced at apply time, all naming the same
figure:

- `capacityPerMember` must hold **one bucket** on a group that declares a tier, because the bytes are
  held in the memory segment until the bucket is complete.
- `localDisks[].capacity` and `localDisks[].keyLimit`, when set, must hold one bucket and one bucket's worth
  of keys.

> **Why a refusal rather than a default** — the store stops taking offload work as soon as one more
> bucket would not fit under a declared ceiling, and it reports that by doing nothing. A tier below
> any of these is a configuration that cannot work under any workload.

**Read `master_allocated_file_size_bytes` to see what the tier holds:**

```console
$ kubectl exec -n gpustack-system deploy/<backend>-leader -- \
    python3 -c "import urllib.request;print([l for l in \
    urllib.request.urlopen('http://127.0.0.1:9003/metrics').read().decode().splitlines() \
    if l.startswith('master_allocated_file_size_bytes')])"
```

`master_allocated_file_size_bytes` is **bytes actually written to the tier**. It moves when a bucket
is closed and written, and it sums the whole backend.

**A `0` is not on its own a verdict, and the section above is why.** A backend that has not filled
a bucket has written nothing **legitimately**, and that is indistinguishable at this gauge from a
tier that cannot write at all.

**The threshold that decides which reading applies is per member, while the gauge is not.** Each
member client fills its own bucket, so traffic spread over four members has to reach four buckets'
worth before every one of them closes (the figure the group's
[`capacityPerMember` floor](/gpustack-operator/v0.9.0-rc3/docs/modules/kv-cache/disk-heavy-nodes/index.md) is taken from). Judge the two together: compare
the data each member has been offered since the tier started with the gauge.

| Offered data | Gauge stays 0 |
|---|---|
| below one bucket | the expected reading; waiting does not change it, because nothing is due |
| one bucket or more | **the tier is not taking writes** — this is the reading to act on |

The gauge updates when a bucket closes. An immediate read after a `put` can still show `0` on a
working tier; it catches up within tens of seconds. If each member has received a bucket's worth
of data and the gauge remains `0` past that delay, the tier is not taking writes.

## Eviction

`localDisks[].eviction` is one choice with two outcomes, not a set of knobs:

| `eviction` | Behavior when full |
|---|---|
| unset | whatever the store does by default |
| `enabled: true` (the default when the block is present) | drops what it holds and goes on accepting writes |
| `enabled: false` | stops accepting writes; what is there stays and stays readable |

- **`policy`** is the order entries leave in: `FIFO` drops the oldest written, `LRU` the least
  recently read. Left unset, the store's own applies, which is `FIFO`.
- **`watermark`** is when eviction runs: it starts once the tier passes `high` and stops once it is
  back under `low`, both **percentages of `capacity`**. `low` must be below `high`.

Four combinations are refused at apply time, each because the store would accept them and then not
act on them:

| Refused | Why |
|---|---|
| `enabled: false` with a `policy` | there is no order in which nothing leaves |
| `enabled: false` with a `watermark` | there is nothing for the marks to start and stop |
| `watermark` with no `capacity` | the marks are a percentage of it, and the store's own default for the quota they are taken against is a value its eviction path reads as switched off |
| `low` at or above `high` | every write past the mark would evict; the member's own startup check refuses the pair, inside a container log |

> **Why the enum has only two values** — the store maps a policy string it does not recognise onto
> **no eviction at all**, with no error, no warning and no failure to start. A neutral two-value enum
> is what keeps a typo from being a silently disabled cache. Turning eviction off is `enabled: false`
> rather than a third enum value, so there is exactly one way to say it.

**Settings this API does not name are reachable through `members[].extraEnv`**, which `extraArgs`
cannot reach: its entries render as the entrypoint's config-key overrides, and this family is read
from the environment only. A name the operator already renders is refused there, because Kubernetes
takes a container carrying one name twice and leaves the winner to the runtime. **Every value is
world-readable**, on the cluster-scoped object and again in the Pod. No credential belongs there.

## Directory requirements

`localDisks[].path` is mounted with `type: Directory`, so **the directory must already exist on every
node the group selects**. This is deliberate: a directory the kubelet creates is owned by `root` with
mode `0755`, while the published store image runs as **uid 65532**, and the member then starts and
cannot write to it. `fsGroup` does not help, because it does not apply to `hostPath` volumes.

**The uid depends on the image**, since `members[].image` may put a different vendor's build on a
group. Read it off the image you are using:

```console
$ docker run --rm --entrypoint id <your-member-image>
uid=65532 gid=0(root) groups=0(root)
```

Then create the directory on each node with that uid:

```console
$ install -d -o 65532 -g 0 -m 0750 /var/lib/kvcache
```

There is **no switch that makes the operator create the directory for you.** An init container would
have to name a single uid for every group, and the uid is a property of whichever image a group
runs, so the command above is the preparation.

That objection is about ownership, so it reaches creating the directory and not emptying it: removing
content needs no uid. Emptying is a switch, and it is
[below](#cleanup-on-deletion).

Five rules the path has to satisfy, all enforced at apply time:

- It must be **absolute**.
- It must not be the **root directory**.
- It **may not overlap `/dev/infiniband`** (equal to it, inside it, or containing it). A sibling such
  as `/dev/infiniband-cache` is fine.
- It **may not contain a `..` component**.
- It **may not begin or end with whitespace**, spaces and tabs alike.

> **Why** — the root directory would mount the node's whole filesystem into a third-party container.
> A host fabric's device plugin injects the granted device nodes under `/dev/infiniband` in this
> same container; a tier on that path would shadow them or be shadowed, which nothing on the object
> would record. That rule holds whatever
> `spec.transport.protocol` says today, because the field is editable. The
> `..` rule mirrors the store's own, which refuses such a path before checking whether the directory
> exists. The whitespace rule exists because the path is mounted exactly as written, so a trailing
> space produces a different directory than the one an operator read on the screen.

**The tier is frozen once a group has it: it cannot be added to a running group, removed from one,
or moved to another `path`.** Members would have to restart to mount the directory, and whatever they
already wrote would stay on their nodes with nothing addressing it. **What the tier may hold is not
frozen**: `capacity`, `keyLimit` and `eviction` each move either way, re-rendering the variables in
the table above, and the tier's contents survive the restart that follows.

**There is exactly one exit, and it needs the tier on the last group.** The immutability rule reads
the list **by position** (the position identifies a group everywhere else, so it identifies the
tier here too), and an update that drops the **last** group takes its tier with it.

Dropping an earlier group is refused: every position after it would be compared against a different
group's spec, which is also why reordering `members` is refused. The refusal message is unambiguous
about which group it means.

**A backend whose only group carries a tier has no exit but deletion.** `members` requires at least
one entry, so that group cannot be removed, and replacing it in place is the forbidden edit. Deleting
the `KVCacheBackend` is what is left, and it takes the leader and every member with it. Put a tier on
the last group if you want to be able to take it off.

**Three failure modes, all loud:**

| Symptom | Cause | Fix |
|---|---|---|
| Refused at `kubectl apply` | the path breaks one of the five rules above, or the tier was added, removed or repathed on a running group | fix the path, or leave the tier alone; the message names which rule |
| Member Pod stuck, event says `FailedMount ... hostPath type check failed` | the directory does not exist on that node | create it as above |
| Member Pod runs but never becomes Ready; container log carries `FileStorageConfig: no write permission on directory: <path>` and `Store startup failed (attempt N): Invalid FileStorage configuration` | the directory exists but the image's user cannot write to it | `chown` it to the uid above |

The last one never reaches a Ready state (the member's REST port opens only after the store mounts,
so the readiness probe never passes), and `MembersMounted` reports the shortfall rather than the
backend looking healthy.

## Cleanup on deletion

Deleting a `KVCacheBackend` leaves the tier directory exactly as it was. Set
`members[].localDisks[].cleanAfterDelete: true` and the operator empties it as part of the deletion, on
every node that group's `nodeSelector` picks at the moment you delete it.

```yaml
      members:
        - medium: DRAM
          capacityPerMember: 64Gi
          localDisks:
            - path: /var/lib/kvcache
              capacity: 512Gi
              cleanAfterDelete: true
```

**It defaults to `false`, which is how every release before it behaved.** What is on that disk is
yours; removing it is not a decision this operator takes on your behalf. Turned on, the decision is
still yours and the operator only carries it out.

**The content goes, the directory stays.** You created it, gave it an owner, and may have mounted a
filesystem there.

**Declaring a tier requires a shell in the group's image.** An init container surveys the directory
before the member starts, and it runs `sh -c`; an image without a shell keeps the member from
starting. The store images this project ships have one.

### When cleanup is disabled

A later backend pointed at the same `path` starts on whatever the previous one left. The store
claims those buckets, so a key the new backend has written can read back as the **old backend's
bytes** instead of as a miss, which is the part that makes it hard to spot.

The operator reports this rather than acting on it. A backend whose tier directory is not empty when
it first starts records it in `status.conditions` as `TierWasEmpty=False`, naming each node and the
number of entries found, and emits one warning event beside it. Nothing is removed and nothing is
blocked.

```console
$ kubectl get kvcachebackend <name> -o jsonpath='{.status.conditions[?(@.type=="TierWasEmpty")]}'
```

**The condition is written once and then left alone**, unlike everything else in that status. After
the first write the question stops being answerable: the backend's own data on that disk is
indistinguishable from what it found there, so a member restarting later cannot be told apart from a
fresh reuse.

Because that write is permanent, `TierWasEmpty=True` waits until **every** node carrying the tier has
reported. `False` does not: one node holding content settles it whatever the others say. So the
condition being **absent** means the question is still open: a member still starting, or one whose
survey cannot run at all. Absent is not a clean tier.

Only a backend's **first** member Pods are believed. A Pod replaced later (a node reboot, an
eviction, an image change) re-runs the same survey against a tier this backend has since filled, and
its entries are not evidence about what the backend found. Such readings are ignored, so a backend
whose condition was never settled early does not end up accusing a predecessor of its own content.

"Every node" means every node the member DaemonSet wants a Pod on, not every Pod that happens to
exist yet. A node still scheduling or still pulling holds the `True` verdict open rather than
being skipped over. A node whose only reading came from a replacement counts as one that has not
answered, which is why a backend can end up with no verdict at all.

### Cleanup limitations

**A node it cannot reach in time keeps its content.** The deletion is not held open for it: a
deletion waiting on a node that is gone is an object nobody can delete. The node is left as it is and
gets a warning event, recorded **on the node** rather than on the backend, because the backend is
about to stop existing and the leftover data is not.

**"In time" is five minutes, counted per node from that node's own cleanup Pod.** It is counted
neither from the oldest Pod of the pass nor from the deletion: a Pod created on a later pass would
otherwise be reported as abandoned with most of its five minutes unspent, and a clock started at the
deletion would spend its budget on the teardown wait instead of the cleanup.

**A cleanup still running at the deadline is stopped, not left to finish.** Emptying a very large
tier can outlast the five minutes, and ending it there leaves the directory partly emptied. Leaving
it running is worse: it keeps deleting from a path this backend no longer holds, and nothing stops a
new backend claiming that directory the moment the deletion completes, so its fresh data would be
deleted the same way.

**A node the group has stopped selecting is not cleaned, and not reported either.** The spec is the
only record of which nodes a group covered. Nothing keeps the selector's history, and the members
are gone by the time the cleanup runs, so a node dropped by narrowing `nodeSelector`, or by
removing the `localDisks` entry, cannot be named, let alone reached. **Delete the backend first and
edit afterwards**; editing first silently takes those nodes out of the cleanup.

**A tier with no image to empty it is given up on immediately**, with the same event. When neither
the member group nor the backend names an image and the operator has no default, there is nothing to
run and no clock to start; waiting would hold the backend open for as long as that stays true. The
event names the image as the reason rather than the node.

**A path another backend's tier overlaps is skipped**, with the same kind of event: a directory
nested inside another backend's tier counts, not only an identical path. Nothing refuses two backends
naming one directory, and emptying it for the one being deleted would take the other one's live data
with it. A removal already under way when the path becomes shared is **stopped**, not left to finish.

> It is a check and not a lock, re-run on every pass rather than only before the removal starts.
> Stopping a removal only cancels the cleanup Pod; the `rm` keeps running until the check catches it
> and on through that Pod's own termination, so everything deleted in that span is lost. That is
> what the second event reports. Closing the window entirely needs an atomic claim on the path,
> which this operator does not take.

```console
$ kubectl describe node <node> | grep -E 'KVCacheTierNotCleaned|KVCacheTierSharedPath|KVCacheTierPartlyEmptied'
```

## Unaccounted disk usage

The `hostPath` is **not** counted into any resource request, and it cannot be: the kubelet's
ephemeral-storage accounting covers the container filesystem, `emptyDir` volumes and logs, never a
`hostPath`. A request against it would reserve a figure nothing polices and would keep the member off
the very node that has the disk.

**Watching that filesystem is yours.** `localDisks[].capacity` renders the store's own ceiling, which is
the only bound on what the tier writes; nothing in Kubernetes will evict or throttle the member when
the node's disk fills.

---

**See also** — [KV Cache Backend](/gpustack-operator/v0.9.0-rc3/docs/modules/kv-cache/backend/index.md) (the object this tier is a layer on, and where its
leader, members and status are documented) · [KV Cache on Disk-Heavy Nodes](/gpustack-operator/v0.9.0-rc3/docs/modules/kv-cache/disk-heavy-nodes/index.md) (the
configuration guide for a node whose capacity is disk rather than memory) · [KV Cache
Pool](pool.md) (how a namespace is granted a quota on the store this tier belongs to)

**Next** → [KV Cache on Disk-Heavy Nodes](/gpustack-operator/v0.9.0-rc3/docs/modules/kv-cache/disk-heavy-nodes/index.md) — the thin-segment shape and the
memory floor that makes this tier reachable.
