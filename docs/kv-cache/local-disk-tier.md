# KV Cache Local Disk Tier

> **Purpose** — the optional disk layer on a `KVCacheBackend` member group: what it renders, the
> bucket that is its write unit, what it does when it fills, what the host directory must already be,
> and the cost nothing in Kubernetes accounts for.
> **Audience** operators, contributors · **Prerequisites** [KV Cache Backend](backend.md) ·
> **Read time** ~10 min

A member group may declare a directory on each of its nodes, which configures the store client's
offload keys to point at it. It is **two halves and admission requires both**, because either alone
is accepted by the store and then does nothing it reports:

```yaml
spec:
  connection:
    managed:
      leader:
        offload:
          enabled: true                # the leader's half
          onEvict: true                # optional; requires enabled
      members:
      - nodeSelector: { kvcache: "true" }
        medium: DRAM
        capacityPerMember: 500Gi
        localDisk:                     # the members' half
          path: /var/lib/kvcache
          capacity: 4Ti                # optional; unset means the store's own ceiling
          keyLimit: 10000000           # optional; the same, on the key count
          eviction:                    # optional; unset means the store's own behaviour
            enabled: true              # default; false fills the tier and then stops writing
            policy: LRU                # FIFO | LRU; unset means the store's own, which is FIFO
            watermark:                 # optional; percentages of capacity
              high: 90
              low: 80
```

| what it renders | where |
|---|---|
| `MOONCAKE_OFFLOAD_ENABLED` and `..._FILE_STORAGE_PATH`, plus `..._BUCKET_SIZE_LIMIT_BYTES` and `..._BUCKET_KEYS_LIMIT` | the member container |
| `..._TOTAL_SIZE_LIMIT_BYTES` **and** `..._BUCKET_MAX_TOTAL_SIZE`, both from `capacity`; `..._TOTAL_KEYS_LIMIT` from `keyLimit` | the member container |
| `..._BUCKET_EVICTION_POLICY`, `..._ENABLE_DISK_WATERMARK_EVICTION` and the two ratio variables, from `eviction` | the member container |
| a `hostPath` volume and mount at `localDisk.path` | the member Pod |
| `-enable_offload=true`, `-offload_on_evict=true` | the leader's argv |
| a `preStop` hook, and a termination window derived from `scaleIn.gracePeriodSeconds` | the member Pod |

**A tier is a layer on a member group, never a group of its own** — see
[The two axes](backend.md#the-two-axes) for why the shape has to be this way, and
[KV Cache on Disk-Heavy Nodes](disk-heavy-nodes.md) for what to write on a node that is mostly disk.

## Contents

- [The tier is written one bucket at a time](#the-tier-is-written-one-bucket-at-a-time)
- [What the tier does when it fills](#what-the-tier-does-when-it-fills)
- [The directory has to exist, and be writable by the image's user](#the-directory-has-to-exist-and-be-writable-by-the-images-user)
- [What the tier costs that nothing accounts for](#what-the-tier-costs-that-nothing-accounts-for)

## The tier is written one bucket at a time

The store does not write an offloaded object on its own. It **assembles objects into a bucket and
writes nothing until that bucket is full** — by bytes or by object count — and what is short of the
threshold is carried to the next attempt, indefinitely.

⛔ **The store's own thresholds are 256 MB and 500 objects, and a backend that never reaches either
has a tier that holds nothing while looking healthy.** The member Pods are Ready, the leader logs the
mount and reports objects deferred for offload, and `status.capacity` shows the size the tier
declared — because that figure is **capacity, not usage** (see
[What status reports](backend.md#what-status-reports)).

This is what [issue #200](https://github.com/gpustack/gpustack-operator/issues/200) was filed for;
the history of what was and was not observed on the way to finding it is in
[the spec](../../specs/2026-09-05-kv-cache-media-and-scaling.md#the-one-item-that-did-not-pass-no-byte-reached-the-disk).

**The operator renders a smaller pair of its own**, so a modest backend closes buckets. They are
**not in the API** and `extraEnvs` refuses them: moving them is a tuning decision that would need a
field, not an escape hatch that silently defines the same variable twice.

Three bounds follow from the bucket being the unit, all enforced at apply time, all naming the same
figure:

- `capacityPerMember` must hold **one bucket** on a group that declares a tier — the bytes are held in
  the memory segment until the bucket is complete.
- `localDisk.capacity` and `localDisk.keyLimit`, when set, must hold one bucket and one bucket's worth
  of keys.

> **Why a refusal rather than a default** — the store stops taking offload work as soon as one more
> bucket would not fit under a declared ceiling, and it reports that by doing nothing. A tier below
> any of these is a configuration that cannot work under any workload.

**Read `master_allocated_file_size_bytes` to see what the tier actually holds:**

```console
$ kubectl exec -n gpustack-system deploy/<backend>-leader -- \
    python3 -c "import urllib.request;print([l for l in \
    urllib.request.urlopen('http://127.0.0.1:9003/metrics').read().decode().splitlines() \
    if l.startswith('master_allocated_file_size_bytes')])"
```

`master_allocated_file_size_bytes` is **bytes actually written to the tier**, so `0` on a tier you
expect to be filling means the data path is not working, whatever the rest of the object says.

## What the tier does when it fills

`localDisk.eviction` is one choice with two outcomes, not a set of knobs:

| `eviction` | what the tier does when full |
|---|---|
| unset | whatever the store does by default |
| `enabled: true` (the default when the block is present) | drops what it holds and goes on accepting writes |
| `enabled: false` | stops accepting writes; what is there stays and stays readable |

- **`policy`** is the order entries leave in — `FIFO` drops the oldest written, `LRU` the least
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

**Settings this API does not name are reachable through `members[].extraEnvs`**, which `extraArgs`
cannot reach: that map renders config-key overrides, and this family is read from the environment
only. A name the operator already renders is refused there, because Kubernetes takes a container
carrying one name twice and leaves the winner to the runtime. ⛔ **Every value is world-readable**, on
the cluster-scoped object and again in the Pod — no credential belongs there.

## The directory has to exist, and be writable by the image's user

`localDisk.path` is mounted with `type: Directory`, so **the directory must already exist on every
node the group selects**. This is deliberate: a directory the kubelet creates is owned by `root` with
mode `0755`, while the published store image runs as **uid 65532**, and the member then starts and
cannot write to it. `fsGroup` does not help — it does not apply to `hostPath` volumes.

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

There is **no switch that makes the operator do this for you.**

> **Why** — an init container would have to name a single uid, and the command above is the evidence
> against that: the uid is a property of the image, and `members[].image` can differ per group. The
> decision and the alternative that was weighed against it are recorded in
> `specs/2026-09-05-kv-cache-media-and-scaling.md`.

Five rules the path has to satisfy, all enforced at apply time:

- It must be **absolute**.
- It must not be the **root directory**.
- It **may not overlap `/dev/infiniband`** — equal to it, inside it, or containing it. A sibling such
  as `/dev/infiniband-cache` is fine.
- It **may not contain a `..` component**.
- It **may not begin or end with whitespace**, spaces and tabs alike.

> **Why** — the root directory would mount the node's whole filesystem into a third-party container.
> The RDMA and EFA transports mount `/dev/infiniband` into this same container; two mounts on one
> path are resolved by the kubelet with one shadowing the other, which nothing on the object would
> record. That rule holds whatever
> `spec.transport.protocol` says today, because the field is editable. The
> `..` rule mirrors the store's own, which refuses such a path before checking whether the directory
> exists. The whitespace rule exists because the path is mounted exactly as written, so a trailing
> space produces a different directory than the one an operator read on the screen.

⛔ **The tier is frozen once a group has it: it cannot be added to a running group, removed from one,
or moved to another `path`.** Members would have to restart to mount the directory, and whatever they
already wrote would stay on their nodes with nothing addressing it. **What the tier may hold is not
frozen**: `capacity`, `keyLimit` and `eviction` each move either way, re-rendering the variables in
the table above, and the tier's contents survive the restart that follows.

⛔ **`leader.offload.enabled` cannot be turned off on its own while a group carries a tier**, because
the pair rule refuses the half-configuration in both directions. It comes off only together with the
tier, in the one edit below.

**There is exactly one exit, and it needs the tier on the last group.** The rules pair groups **by
position** and stop at the end of the new list, so an update that drops the **last** group and clears
`leader.offload` in the same edit is admitted. Dropping an earlier group is refused: every position
after it would be compared against a different group's spec, which is also why reordering `members`
is refused. That message is accurate rather than confused about which group you meant.

⛔ **A backend whose only group carries a tier has no exit but deletion.** `members` requires at least
one entry, so that group cannot be removed, and replacing it in place is the forbidden edit. Deleting
the `KVCacheBackend` is what is left, and it takes the leader and every member with it. Put a tier on
the last group if you want to be able to take it off.

**Three failure modes, all loud:**

| Symptom | Cause | Fix |
|---|---|---|
| Refused at `kubectl apply` | the path breaks one of the five rules above, or the tier was added, removed or repathed on a running group | fix the path, or leave the tier alone; the message names which rule |
| Member Pod stuck, event says `FailedMount ... hostPath type check failed` | the directory does not exist on that node | create it as above |
| Member Pod runs but never becomes Ready; container log carries `FileStorageConfig: no write permission on directory: <path>` and `Store startup failed (attempt N): Invalid FileStorage configuration` | the directory exists but the image's user cannot write to it | `chown` it to the uid above |

The last one never reaches a Ready state — the member's REST port opens only after the store mounts,
so the readiness probe never passes — and `MembersMounted` reports the shortfall rather than the
backend looking healthy.

## What the tier costs that nothing accounts for

The `hostPath` is **not** counted into any resource request, and it cannot be: the kubelet's
ephemeral-storage accounting covers the container filesystem, `emptyDir` volumes and logs, never a
`hostPath`. A request against it would reserve a figure nothing polices and would keep the member off
the very node that has the disk.

**Watching that filesystem is yours.** `localDisk.capacity` renders the store's own ceiling, which is
the only bound on what the tier writes; nothing in Kubernetes will evict or throttle the member when
the node's disk fills.

---

**See also** — [KV Cache Backend](backend.md) (the object this tier is a layer on, and where its
leader, members and status are documented) · [KV Cache on Disk-Heavy Nodes](disk-heavy-nodes.md) (the
configuration guide for a node whose capacity is disk rather than memory) · [KV Cache
Pool](pool.md) (how a namespace is granted a quota on the store this tier belongs to)

**Next** → [KV Cache on Disk-Heavy Nodes](disk-heavy-nodes.md) — the thin-segment shape and the
memory floor that makes this tier reachable.
