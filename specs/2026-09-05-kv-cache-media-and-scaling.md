# Spec: KV Cache Media Tiering and Scale-In

Status: Shipped
Type: Feature

> **What this delivered, and what it did not.** The API surface, the rendering, the admission rules,
> the capacity reporting and the shutdown path are all implemented and verified against a running
> store. **Data reaching the disk tier is not verified** — the leader accepts the member's tier,
> publishes its declared capacity and runs eviction, and nothing was ever written there in either
> configuration this API can render. The cause is not established. See
> [The one item that did not pass](#the-one-item-that-did-not-pass-no-byte-reached-the-disk), which
> records what the claim is bounded to and what does not count as closing it.

## Summary

A `KVCacheBackend` today runs one medium. `members[].medium` accepts five names — `DRAM`,
`LocalDisk`, `NoF`, `CXL`, `DFS` — and admission refuses four of them, because nothing renders what
they would need. Shrinking a backend has no API surface at all: an operator narrows
`members[].nodeSelector` and the member that leaves takes its cache with it.

This spec closes both gaps, and it closes them differently from how the shape was expected to close,
because reading the store's own source at the version this project pins says the expected shape does
not exist. **Four of the five media are not member groups at all.** A local SSD is a *layer inside*
the member that already holds the memory segment; an NVMe-oF namespace is a target coordinate
registered once by anything at all; CXL and a distributed filesystem are properties of the *leader
process*. Only `DRAM` is a thing a group of nodes contributes.

So the work here is: give the member group a **local disk layer** with the flags, the mount, the
`preStop` and the capacity accounting it actually needs; give scale-in a **grace period** that the
one drainable layer can honour and that the Pod's own termination window is held above; fix a
capacity-family classification that is wrong for one medium and would publish another tier's figure;
and settle each remaining enum value by naming the object that would carry it, rather than leaving
four values whose only behaviour is to be refused.

Every claim below about the store is cited to a file and line in the artifact's own source at
v0.3.13 (`e5598b09`), which is the version the image this project runs is built from.

## Motivation

### Goals

- **G1 (primary)** A backend can hold a hot memory tier and a cold local-SSD tier, declared in one
  object, and the two tiers are wired the way the store actually implements tiering — not the way the
  API's enum suggested.
- **G2** Every value `members[].medium` accepts either **renders** or is **gone**. A value whose only
  behaviour is to be refused teaches a reader that a feature exists, which is the failure the backend
  spec named and this one inherits.
- **G3** Shrinking says what it costs and offers the one mitigation the artifact supports. A member
  leaving still drops its memory segment; what it must not do is drop offload reads that are in
  flight, and it must not be cut short by a termination window smaller than the grace it was given.
- **G4** `status.capacity` keeps meaning what it says once a backend has two tiers. Today the family
  is picked from `members[0].medium`, which is a correct answer only while there is exactly one group.
- **G5** No field is added that nothing reads. This is stated as a goal because three separate
  candidates in this scope fail it, and each is refused below by name with the reason attached.

### Non-Goals

Each is out of scope by decision. The subject that owns it is named so "not yet" is legible as
"not yet".

- **Migrating data off a member before it goes away.** The store's `POST /api/v1/drain_jobs` moves a
  key's replica from one segment to another and is the only mechanism that could make a shrink
  lossless. It is not taken here for two reasons that compound: it is **stateful orchestration** —
  create a job, poll it, then scale — which is a reconcile shape nothing in this operator has yet;
  and it **silently covers only two of the five replica types**, which makes the operation's own
  success signal untrustworthy for exactly the tier this spec adds. That coverage claim is
  established in Alternatives, under "Take `drain_jobs` in this scope" — it names the two types and
  cites the artifact source that decides them. It belongs to the leader high-availability spec, which
  owns the drain axis.
- **`scaleIn.policy` as an enum.** The design draft carried `GracePeriod | Migrate`. With `Migrate`
  deferred above, the enum would ship with one value, which is a knob nobody can turn. The grace
  period ships as a **number**, and `policy` arrives with the second value that makes it a choice.
- **Rendering `CXL`, `DFS` or `NoF`.** Each is settled in F2 by naming what would carry it. None is a
  member group, so none is reachable by widening the renderer this spec touches; each needs an API
  surface that does not exist and is not invented here.
- **First-class fields for the offload *tuning* knobs.** `-offload_cap_ratio`,
  `-offloading_queue_limit`, `-offload_force_evict` and the promotion knobs stay on the leader's
  `extraArgs`. What this spec adds is the pair that **turns the tier on**, which is different in
  kind: without it the layer does not exist, whereas without a ratio it merely runs at the
  artifact's default.
- **Fields for the eviction watermarks** — `eviction_high_watermark_ratio` and `eviction_ratio`.
  They stay on `leader.extraArgs`, and the reason travels with the exclusion because a bare "not
  doing it" is what a later spec reads as an oversight and puts back. **Nothing reads them.** The
  rule is the one `leader.multiTenancy` established: it is a field rather than an `extraArgs` key
  because a *different* API's admission reads it — a `KVCachePool` is refused when its backend has
  no tenant ledger — and a webhook judging an unschema'd `"true"` / `"1"` / `"True"` would be ruling
  on a value domain that is not its own. The watermarks have no second reader: verified by search
  across `api/` and `pkg/`, where the only occurrence of the word is a doc comment on `spec.type`.
  A knob that is a start-time gflag, cannot be changed at runtime, and is shaped exactly like the
  several dozen others beside it needs the escape hatch and nothing more. F7 records the one
  candidate reader this spec would make possible, and why it does not clear the bar either.
- **Anything about pools, bindings, quota or workloads.** `KVCachePool` reads `leader.multiTenancy`
  and nothing else on this object; that stays true. Nothing here reads or writes a pool.
- **Several members per node.** Still one member per selected node, still a DaemonSet.
  `capacityPerMember` keeps its name for the same reason it was given it.

## Proposal

### What the store actually implements, and where the API's shape disagrees

This section is the load-bearing one. Every row was read in the artifact's own source at v0.3.13.

**The store has five replica types, not five media**, and they are not parallel constructs. From the
stream operator that prints one (`mooncake-store/include/replica.h:783-812`): `MEMORY`, `NOF_SSD`,
`DISK`, `LOCAL_DISK`, `DFS`. What differs between them is **who owns the bytes and who was told
about them**, and that is what decides whether a thing can be a member group at all:

| API `medium` | store construct | who configures it | is it a member group? |
|---|---|---|---|
| `DRAM` | `MEMORY` replica in a `Segment` a client mounts | the member's own `MOONCAKE_GLOBAL_SEGMENT_SIZE` | **yes** — this is the one the shape fits |
| `LocalDisk` | `LOCAL_DISK` replica, owned by **the client that already holds the memory replica** | the member client's `enable_ssd_offload` + `ssd_offload_path`, paired with the leader's `-enable_offload` | **no** — it is a layer *inside* a group, see below |
| `NoF` | `NOF_SSD` replica in a `NoFSegment`, a **separate type with its own manager** (`master_service.h:2683`) | a one-shot registration call carrying an NVMe-oF target coordinate: `real_register(nqn, nsid, traddr, trsvcid, base, size, master_addr)` (`mooncake-integration/store/store_py.cpp:2144-2152`) | **no** — the parameters are a target's coordinates; no node selects them and no Pod need outlive the call |
| `CXL` | a DAX allocator **inside the master process** (`master_service.cpp:549-553`) | the leader's `-enable_cxl` / `-cxl_path` / `-cxl_size`, which also swap the allocation strategy for `CxlAllocationStrategy` | **no** — it is a property of the leader |
| `DFS` | a `DfsGlobalAllocator` **inside the master process** (`master_service.cpp:557-591`) | the master process's own `MOONCAKE_DFS_*` environment variables | **no** — it is a property of the leader |

Two further facts about that table, both of which change what this spec can offer:

- **`DISK` is a sixth thing the API never named.** The leader's `-root_fs_dir` plus
  `-global_file_segment_size` is a *legacy shared-filesystem* path (`master_service.cpp:515-524`),
  distinct from `DFS`. The design report attributed those two flags to `DFS`; they are `DISK`'s. The
  `DFS` path took its own abstraction upstream in 0.3.12 and is configured entirely by environment.
- **`DFS` refuses to come up in the mode this project's pools require.** Its initializer reads
  `MOONCAKE_DFS_SINGLE_TENANT`, and where the config is not single-tenant it logs an error, sets
  `enable_dfs_ = false` and **returns normally** (`master_service.cpp:568-574`). `leader.multiTenancy`
  is exactly what a `KVCachePool` requires of its backend. So a hypothetical `DFS` tier under a pool
  is not merely unimplemented here — it is a **silent** degradation upstream.

#### The finding that decides the shape: offload is routed to the memory replica's owner

A local-SSD tier cannot be a member group of its own, and this is not a preference. When the leader
decides a key should go to disk it looks up the segment holding that key's **memory** replica, asks
the segment manager who owns that segment, and enqueues the offload task **for that client**:

```cpp
// mooncake-store/src/master_service.cpp:5541-5560, PushOffloadingQueue
const auto& segment_names = replica.get_segment_names();
...
auto client_id = allocator_access.GetOwnerClientId(segment_name_it.value());
...
local_ssd_manager_.EnqueueOffload(*client_id, OffloadTaskItem{...}, offloading_queue_limit_);
```

A member group with `global_segment_size: 0` — which is what "a group that is disk and not memory"
would render — owns no memory segment, is therefore never the owner of one, and **receives no
offload task, ever**. Its disk stays empty.

It would not look empty. The client reports its SSD capacity to the leader on its own
(`master_service.cpp:7340-7362`, `local_ssd_manager_.ReportCapacity`), and the leader adds that
figure to `master_total_file_capacity_bytes` — the very gauge `status.capacity` would read for a
file-backed group. So the object would report a healthy multi-terabyte cold tier, the Pods would be
Ready, and nothing would ever be written there. **That is the exact silent failure this project
catalogues and refuses to ship**, and rendering `medium: LocalDisk` as a second group is how we
would build one on purpose.

The store's own tests say the same thing from the other side: `enable_ssd_offload` is commented
`required for FileStorage / LOCAL_DISK` on a client that **also** mounts a memory segment
(`mooncake-wheel/tests/test_offload_on_eviction.py:55`,
`mooncake-wheel/tests/test_promotion_on_hit.py:87`).

#### Therefore: a tier is a layer on a group, not a second group

```yaml
spec:
  connection:
    managed:
      leader:
        offload:                       # NEW. Turns the disk tier on, leader side.
          enabled: true                # -enable_offload
          onEvict: true                # -offload_on_evict
      members:
      - nodeSelector: { kvcache: "true" }
        medium: DRAM                   # unchanged: what the group's SEGMENT is
        capacityPerMember: 500Gi
        localBufferSize: 4Gi
        localDisk:                     # NEW. The same members' SSD tier.
          path: /var/lib/kvcache       # ssd_offload_path; hostPath-mounted
          capacity: 4Ti                # MOONCAKE_OFFLOAD_TOTAL_SIZE_LIMIT_BYTES
      scaleIn:                         # NEW.
        gracePeriodSeconds: 30
```

Two objects, one decision each, and neither invents a mechanism:

- `leader.offload` is on the leader because `-enable_offload` is the leader's flag and gates the
  whole feature: `offload_on_evict_` and `promotion_on_hit_` are both ANDed with it
  (`master_service.cpp:361`, `:376`), and every offload entry point returns early without it
  (`:7206`, `:7237`).
- `members[].localDisk` is on the group because `ssd_offload_path` is a **path on a node**, and which
  nodes is precisely what a group's `nodeSelector` says.

The two must be set together or the tier is inert in a way nothing reports, so admission refuses
either one alone (F4).

### Four decisions this spec makes, each by one rule

The rule is the one the backend spec established for `leader.multiTenancy` and this spec applies
four more times: **a field exists when something reads it, or when its value domain has a second
value.** Stated as a question — what breaks if this is an `extraArgs` key, or absent entirely?

| # | Question | Answer | Why |
|---|---|---|---|
| **D1** | Is a local SSD a second member group or a layer on one? | **A layer** | Measured above: a group with no memory segment never receives an offload task, and reports capacity anyway |
| **D2** | Do `LocalDisk` / `NoF` / `CXL` / `DFS` stay in the `medium` enum? | **They are removed; the enum becomes `DRAM`** | Each names a thing that is not a member group. Keeping them is the failure the backend spec's own F1 named: an enum value whose only behaviour is to be refused. See F2 for where each goes instead, and Alternatives for the conservative option |
| **D3** | Do `eviction_high_watermark_ratio` / `eviction_ratio` get fields? | **No — they stay on `leader.extraArgs`** | Nothing reads them, and the reason is in Non-Goals with the rule it comes from. One candidate reader exists and is described in F7, because this spec is what would make it possible; it does not clear the bar either |
| **D4** | Does `scaleIn.policy` ship? | **No — only `gracePeriodSeconds`** | With `Migrate` deferred, the enum has one value |

D1 is a measurement, not a preference. D2 is the one where a reasonable reader could land elsewhere,
and it is marked as a decision for that reason; the Alternatives section carries the conservative
option in full.

**D2 and D4 look like they contradict each other, and the distinction that resolves them also
resolves D3.** D2 narrows an enum to one value and keeps the field; D4 refuses to ship an enum with
one value. Both are right, under one test:

> A single-valued enum is legitimate when it is an **identity** — it answers *what is this object* —
> and illegitimate when it is a **choice** — it answers *how should this be done*.

- `spec.type: Mooncake` and `members[].medium: DRAM` are identities. The object has to say what it
  is, so that a second implementation or a second medium **widens an enum** rather than
  reinterpreting an absent field, and so that a manifest is readable without knowing which defaults
  were in force when it was written. Neither field asks the operator to decide anything.
- `scaleIn.policy: GracePeriod` would be a choice, and a policy is *nothing but* a choice. With one
  option it carries no information at all — a knob that cannot turn, dressed as one that can.

The same test settles D3 from the other side. The eviction watermarks are not a field because
nothing reads them; if they were made fields they would be neither identity nor choice but a pair of
numbers passed straight through, which is exactly what an escape hatch is for. **One test, three
decisions**, which is why it is stated here once instead of three times.

### User Stories

#### Story 1

As a cluster administrator with 500 GiB of RAM and 4 TiB of NVMe on each cache node, I want to
declare both in one member group, so that keys evicted from memory land on the same node's SSD
instead of being recomputed.

#### Story 2

As a cluster administrator, I want a member Pod that is going away to finish the offload reads its
peers already asked it for, so that a rolling node drain does not turn into a burst of cache misses
on keys that are still on disk.

#### Story 3

As a cluster administrator who set a 30-second grace period, I want the Pod's own termination window
to be **larger** than that grace, so that the kubelet does not kill the container in the middle of
the wait I configured. I want that relationship enforced at admission rather than discovered as a
truncated drain.

#### Story 4

As a cluster administrator reading `kubectl explain`, I want `members[].medium` to list only values
that run, so that I do not build a plan around a cold tier the API appeared to offer.

#### Story 5

As a cluster administrator whose backend has a memory tier and a disk tier, I want
`status.capacity` to describe the backend rather than whichever tier happens to be listed first.

### Core Features & Acceptance Criteria

#### F1 — `members[].localDisk`: the tier, its path, and its size

- `localDisk` is an optional block on a member group with two fields:
  - `path` — a **required, absolute** path on the node, rendered as the client's `ssd_offload_path`
    and hostPath-mounted at the same location into the member container. There is no default: the
    artifact's own default is `/data/file_storage`
    (`mooncake-store/include/storage_backend.h:337`), and defaulting a **hostPath** on somebody
    else's nodes is not a decision an operator makes on their behalf.
  - `capacity` — an optional `resource.Quantity`, rendered as
    `MOONCAKE_OFFLOAD_TOTAL_SIZE_LIMIT_BYTES`. Left unset, the artifact's own 2 TiB ceiling applies
    (`storage_backend.h:350`) and nothing is rendered, per the rule that a flag this API does not
    address is absent rather than restated at its default.
- The mount is a `hostPath` with **`type: Directory`**, not an `emptyDir` and not
  `DirectoryOrCreate`. An `emptyDir` lives in the Pod's own lifecycle, so every member restart would
  discard the tier the feature exists to keep, and an `emptyDir` with no `sizeLimit` on a node whose
  disk fills gets the Pod evicted by the kubelet.
- **The directory has to exist already, owned by the uid the member image runs as, and this is
  measured rather than cautious.** `DirectoryOrCreate` is the convenient choice and it does not
  work: the kubelet creates the directory **root-owned with mode 0755**, while the published store
  image runs as **uid 65532** — so the member starts, finds `no write permission on directory`, and
  retries its store setup until it gives up. Nothing the renderer can add fixes it from inside the
  Pod: `fsGroup` **does not apply to hostPath** (measured on a cluster — the directory stays
  root-owned and unwritable), and every remaining option means granting a container root on the node
  to chown a host directory.
  - **The failure is loud on two independent paths, which is what makes requiring the directory
    acceptable.** A missing directory is a `FailedMount` the Pod stops at. An unwritable one lets
    the container start, but the REST port never opens — the entrypoint serves it only after the
    store mounts — so the readiness probe never passes and `MembersMounted` reports the shortfall.
  - **Which uid depends on `members[].image`**, since a group may run a different vendor's build. The
    documentation therefore states how to read it off the image being used rather than printing one
    number (F8).
  - Granting the operator a way to prepare the directory itself is an Open Question, not a gap: it
    would need an explicit opt-in, because privilege is requested and never inferred on an
    administrator's behalf — the same rule that keeps `transport.protocol: Auto` from promoting
    itself to RDMA.
- The tier is **not** counted into `resources.requests.ephemeral-storage`. A hostPath is not
  ephemeral storage: the kubelet's ephemeral-storage accounting covers the container filesystem,
  `emptyDir` volumes and logs, and never a hostPath. Requesting against it would reserve a number the
  kubelet does not police, on a resource the scheduler cannot see, and would then make the member
  unschedulable on a node that has the disk. The honest rendering is to request nothing for it and
  say so in the documentation, which is what F8 states.
  - **This corrects a rule that is in the code today.** `memberRequests`
    (`pkg/worker/kvcache/mooncake/member_workload.go:353-362`) switches the request from `memory` to
    `ephemeral-storage` when the group's medium is `LocalDisk`, comparing against a bare string
    literal rather than going through either medium helper. The rule was never reachable — a
    `LocalDisk` group is refused at admission — and it was **also wrong for the value it named**, for
    the reason above: whatever a disk-backed group would have mounted, it would not have been
    ephemeral storage. Both the branch and the medium-keyed shape go: after this spec a member's
    request is `capacityPerMember + localBufferSize` as memory, unconditionally, because the memory
    segment is the only thing a member asks the scheduler for.
- `capacityPerMember` keeps meaning the **memory** segment, unchanged. The disk figure is
  `localDisk.capacity` and the two are never added: they land in different gauges on the leader.
- **Acceptance:** a group with `localDisk` renders a member container carrying
  `MOONCAKE_OFFLOAD_ENABLED=true`, `MOONCAKE_OFFLOAD_FILE_STORAGE_PATH=<path>`, the hostPath volume
  and its mount, and — where `capacity` is set — `MOONCAKE_OFFLOAD_TOTAL_SIZE_LIMIT_BYTES` as a byte
  count. A group without it renders **none of the five**, asserted as a whole rather than key by key,
  so a future path that turned one on silently would fail here.
- **Acceptance:** the memory request is `capacityPerMember + localBufferSize` exactly as before, with
  `localDisk.capacity` contributing nothing to it and nothing to `ephemeral-storage`.

#### F2 — The `medium` enum collapses to what runs, and each removed value is placed

`members[].medium` becomes an enum of exactly `DRAM`. The webhook's per-medium refusal
(`pkg/worker/webhooks/worker/kv_cache_backend.go:409`) and `MemberMediumIsReconciled` go with it: a
rule that can no longer be reached is dead code, and the schema now says what the webhook was saying.

This is a **breaking change to an unreleased API**, and it clears the three gates such a change has
to clear.

**Gate one — it is genuinely unreleased.** `api/worker/v1alpha1/kv_cache_backend.go` does not exist
in any tag from `v0.8.0` through `v0.8.6`, checked per tag rather than inferred from a date; the
commit adding it landed after `v0.8.6`. So no cluster outside this project's own development has
ever had the type at all.

That is the load-bearing half, and it is worth separating from the weaker argument beside it. "The
four values were refused at admission" is **also** true, but it rests on the webhook having been in
force: the refusal was the webhook's, not the schema's, so an object created against a cluster where
the CRD was installed and the webhook was not could have carried one. Such an object would fail
validation on every write after this change — including the controller's own status writes — and
wedge permanently. **Nothing here would rescue it**, and that is accepted rather than overlooked:
the type is unreleased, so the only clusters that could hold one are this project's own, where
deleting the object is the fix. A released API would need a stored-version migration instead, and
this note is here so the next narrowing does not inherit the reasoning without the premise.

**Gate two — every reader is accounted for, one line each.** Not "nothing seems to use it": each
site below was opened and read for **which values it takes**, searched twice over `api/` and `pkg/`
— once for the `Medium` identifier and once for the four string literals, because a site that
compares against a bare `"LocalDisk"` is invisible to the first search and a site that passes the
field to a helper is invisible to the second.

| site | takes | after this spec |
|---|---|---|
| `mooncake/member_workload.go:358` — `memberRequests` | a bare literal `"LocalDisk"`, choosing `ephemeral-storage` over `memory` | **deleted.** It is the one site the identifier search alone would have missed, and F1 explains why its rule was wrong even for the value it named |
| `mooncake/member_workload.go:415-424` — `memberFileBackedMedia`, `MemberMediumIsFileBacked` | `LocalDisk`, `NoF`, `DFS` | **deleted** with its only caller (below); the `NoF` row was wrong, see F6 |
| `mooncake/member_workload.go:427-440` — `MemberMediumDRAM`, `MemberMediumIsReconciled` | `DRAM` | **deleted.** The schema now refuses what this refused |
| `webhooks/worker/kv_cache_backend.go:409-415` — the per-medium refusal | reads `MemberMediumIsReconciled` | **deleted.** A webhook rule no request can reach is dead code, and its message would now contradict the schema's |
| `webhooks/worker/kv_cache_backend.go:554` — medium immutability | compares old to new; takes **no** specific value | **kept**, deliberately. It is unreachable while the enum has one value, and it becomes correct again on the day the enum widens. Deleting it would make widening the enum a change that silently permits mutating a medium under mounted segments — the failure that only appears in one merge order |
| `controllers/worker/kv_cache_backend.go:757`, `:1000` — the status join | copies the group's value; takes **no** specific value | **kept**, unchanged. `status.members[].medium` carries no enum marker by design, so it is unaffected |
| `controllers/worker/kv_cache_backend.go:1070` — `selectKVCacheBackendCapacity` | reads `MemberMediumIsFileBacked` | **replaced** by F6's rule, which asks about the disk tier rather than about a medium name |
| `api/worker/v1alpha1/zz_generated.crds.go:2437-2449` | the five enum values, as generated schema | **regenerated** |

**Gate three — the generated artifacts travel with it.** The CRD schema, the protobuf marshalling,
deepcopy, openapi and the applyconfiguration clients all carry this field, and a change that lands
in the Go type without them is a silent inconsistency rather than a compile error. T1 owns all of
them and its verification is a second `make generate` producing an empty diff.

**Why the field survives with one value** is the identity-versus-choice test stated with the
decisions above: `medium: DRAM` says the group contributes host memory, which is a fact about the
group and not a knob, and it is the field a second medium widens rather than a field a second medium
would have to be inferred from.

Where each removed value goes, so a reader can tell "moved" from "dropped":

| removed value | where the capability lives now | what it would take to ship |
|---|---|---|
| `LocalDisk` | `members[].localDisk` (F1) — same members, second tier | shipped by this spec |
| `NoF` | nowhere in this API | a registration surface carrying `nqn` / `nsid` / `traddr` / `trsvcid` / `base` / `size`. It has no node affinity and no Pod, so it is not a member group; whether it is a field on the leader or an object of its own is open |
| `CXL` | `leader.extraArgs`: `enable_cxl`, `cxl_path`, `cxl_size` | a leader-side block plus a DAX device on the leader's node, and a note that it also replaces the allocation strategy |
| `DFS` | `leader` environment, which this API does not render at all today | a leader-side block for `MOONCAKE_DFS_*`, plus admission refusing it together with `multiTenancy`, which the store degrades on silently |

- **Acceptance:** `medium: LocalDisk` is refused **by the CRD schema** with
  `Unsupported value: "LocalDisk": supported values: "DRAM"`, before any webhook runs. The message is
  worse than the webhook's was, and the field's own description is where the guidance moves to — the
  same trade the backend spec made for `spec.type`, and for the same reason: a value outside an enum
  never reaches a webhook.
- **Acceptance:** the description of `medium` names `localDisk` as where a disk tier is declared, so
  `kubectl explain` answers the question the removed value used to raise.
- **Both of the above are held by one test, and it is the only thing that can hold them.**
  `TestKVCacheBackendMediumEnumCarriesOnlyWhatRuns` reads the generated CRD and asserts the enum is
  exactly `["DRAM"]` and that the description names `localDisk`. No admission test can cover either,
  because the values are refused in `rest.BeforeCreate` before the webhook chain — the same fact that
  made the old per-medium webhook rule unreachable and deleted it. Without this test **putting a
  removed value back would go green**, and it is what caught the description saying a tier is
  declared "as `LocalDisk` below", naming the value that was removed rather than the field that
  replaced it, on the one channel that reaches such a reader at all.

#### F3 — `leader.offload`: the pair that turns the tier on

- `offload` is an optional block on the leader with two booleans:
  - `enabled` — `-enable_offload`. Rendered only when true; the artifact's default is false and an
    explicit `false` would move the command line of every backend that never asked for the feature.
  - `onEvict` — `-offload_on_evict`. Defers the write to the SSD from `PutEnd` to eviction time.
- `onEvict` without `enabled` is refused at admission rather than rendered, because the artifact ANDs
  the two (`master_service.cpp:361`) and would accept the flag, log it, and do nothing with it.
- The tuning knobs stay on `extraArgs`, and both new flags join `LeaderExtraArgsRules.Derived` so the
  hatch cannot fight the fields.
- **Acceptance:** the rendered leader argv carries `-enable_offload=true` and, where asked,
  `-offload_on_evict=true`, in the renderer's deterministic order and before the `extraArgs` tail.
  A backend with no `offload` block renders neither, asserted by name.
- **Acceptance:** `extraArgs: {enable_offload: "true"}` is refused with the message naming the field
  that owns the flag.

#### F4 — Admission: the pairings the artifact degrades on silently

Each rule below refuses a combination the artifact **accepts and then quietly does not honour**.
That is the bar for putting a rule in a webhook rather than in the schema: the schema can say a
value is wrong, only a webhook can say a pair is.

| rule | refused because |
|---|---|
| `members[].localDisk` set while `leader.offload.enabled` is not | the leader never enqueues an offload task, so the disk stays empty while the member reports its capacity into `master_total_file_capacity_bytes` — a cold tier that reads as present and is not |
| `leader.offload.enabled` while **no** member group carries `localDisk` | the mirror image. The leader queues offload tasks for clients that have no local-disk segment; the master's own guard drops them (`master_service.cpp:4688`, `:7430`) and nothing on the object says so |
| `leader.offload.onEvict` without `enabled` | ANDed away at `master_service.cpp:361` |
| `localDisk.path` that is not absolute, is `/`, is blank, or carries surrounding space | it becomes a hostPath. A relative path is refused by the kubelet inside a reconcile, where the only trace is a line in a log; `/` mounts the node's root filesystem into a third-party container; and a space is not trimmed, because a validating webhook returns a verdict and cannot write, so normalising would admit the untrimmed value and mount it as written |
| `scaleIn.gracePeriodSeconds` above 3600 | the entrypoint refuses it with HTTP 400 (`mooncake-wheel/mooncake/mooncake_store_service.py:18`, `:652-668`), so a larger value is a `preStop` that fails on every shutdown |
| **two** member groups that **both** carry `localDisk` | see F6. One disk tier per backend is what a single `status.capacity` can describe; two would land in one gauge family with no way to tell them apart. A backend where only **one** of two groups carries a tier is admitted — the gauge then means that one tier |

**There is deliberately no rule refusing a grace at or above the Pod's termination window**, although
Story 3 asks for exactly that relationship. The window is **derived** from the grace (F5), so the
relationship holds by construction and a check for it would be a gate whose trigger is a tautology —
the kind that reads as protection and can never fire. Deriving is what makes the guarantee; a rule
would only have made it checkable.

- **Acceptance:** every rule is asserted by its **message**, not merely by the rejection. These are
  in a webhook precisely because the message can say what to do.
- **Acceptance:** `localDisk` is added to the immutable set beside `medium`: a group's disk tier
  cannot be turned on or off, or moved to another path, under members that are already running with
  data on the old one. Changing `localDisk.capacity` **is** permitted, since it re-renders one
  environment variable and the fingerprint restarts the group by the mechanism that already exists.

#### F5 — Scale-in: a grace period, the one thing it can drain, and the window that must hold it

`spec.connection.managed.scaleIn.gracePeriodSeconds` is an optional integer, and it is honoured by
exactly one mechanism, which is the only one the artifact offers to a Pod that is going away:

- The member container gets a `preStop` hook calling `POST /api/unmount_local_disk` on its own REST
  port with `{"grace_period_seconds": N}`. The handler's own docstring states what it buys: the
  master stops naming this store as the owner of its offloaded keys, so a reader gets a clean miss
  instead of a peer that is about to disappear, and the call then holds for the grace so offload
  reads already in flight finish here
  (`mooncake-wheel/mooncake/mooncake_store_service.py:628-636`).
- **The hook is an `exec`, not an `httpGet`, and the reason is a hard constraint rather than a
  preference.** A Kubernetes `httpGet` lifecycle handler sends no request body and cannot set a
  method; this endpoint is a POST whose handler calls `request.json()` first and answers **400 on a
  malformed or absent body** (`mooncake_store_service.py:637-643`). An `httpGet` hook would
  therefore fail on every single shutdown, and a failing `preStop` is logged as an event and
  otherwise ignored — the container is killed anyway. So the hook would look configured and drain
  nothing.
- The `exec` runs the image's own Python, calling the endpoint through the standard library only.
  The interpreter is not an assumption about an unrelated image: this container's `command` is
  `mc_store_rest_server`, a Python console script, so an image that cannot run `python3` cannot run
  the member either. **It is still verified rather than reasoned about** — T7 runs the hook's exact
  argv inside a real member container, because "the entrypoint is Python" does not by itself
  establish which interpreter name is on `PATH`.
- The hook is rendered **only for a group that carries `localDisk`**. On a group with no disk tier it
  would be a request the entrypoint answers with an error on every single shutdown.
- `terminationGracePeriodSeconds` stops being the hardcoded 60 and becomes
  `gracePeriodSeconds + 60`, where the 60 is the window the backend spec established for the
  entrypoint's own `store.close()`. Held that way the relationship cannot be violated: the field is
  derived, and admission refuses an operator who sets a grace at or above a window they cannot set.

**What this deliberately does NOT claim.** The memory segment is still dropped, not drained, and the
reason is unchanged from the backend spec except that this scope re-checked it and found the
counter-argument stronger than expected — which is why it is written out rather than referenced:

- `POST /api/unmount` on the member **does** take `{"segment_ids": [...], "grace_period_seconds": N}`
  and does reach `unmount_and_free_segment` (`mooncake_store_service.py:593-607`). The backend
  spec's reason for calling this unreachable — that a fresh client would not know the segment id —
  does not apply to a `preStop`, which talks to the running process itself.
- It is still unreachable, for a different reason: **`segment_ids` is required and there is no route
  that returns them.** The entrypoint's route table (`mooncake_store_service.py:238-268`) has no GET
  that lists this client's own segments, and the segment's name is not derivable — the leader
  appends a port of its own choosing that is fresh on every start.
- So the memory tier's graceful unmount is blocked on an upstream route that does not exist, and
  **that, not the client-identity argument, is the thing that would have to change**. Recorded here
  as an Open Question so the next reader tests the right claim.

- **Acceptance:** a group with `localDisk` and a 30-second grace renders a `preStop` httpGet-free
  exec or HTTP POST carrying exactly `{"grace_period_seconds": 30}` to the member's own REST port,
  and `terminationGracePeriodSeconds: 90`.
- **Acceptance:** a group without `localDisk` renders no `preStop` at all and keeps
  `terminationGracePeriodSeconds` at 60, so an existing backend's Pod template is **byte-identical**
  before and after this spec — asserted by rendering a canonical pre-change spec and diffing.

#### F6 — `status.capacity` with two tiers, and one classification that is wrong today

Two independent problems, and they are separate because one is a bug and one is a design gap.

**The bug.** `memberFileBackedMedia` (`pkg/worker/kvcache/mooncake/member_workload.go:415-419`)
classifies `NoF` as file-backed, so a `NoF` group would be read from
`master_total_file_capacity_bytes`. The leader keeps a **third** family for it —
`master_total_nof_capacity_bytes`, plus a per-segment variant
(`mooncake-store/src/master_metric_manager.cpp:39`, `:45`) — which nothing here reads. The
classification was written from the flags each medium documents, and NoF's flags look file-shaped;
its gauges are not. With D2 the `NoF` value is removed and the wrong row goes with it, so the fix is
a deletion rather than a repair. The row is recorded because the **test that covered it agreed with
it** (`capacity_file_medium` exercises the selection directly), and a classification with a green
test is the kind that survives a rewrite unnoticed.

`LocalDisk`'s classification, by contrast, is **right** and stays: the client's `ReportCapacity`
lands in the file family (`master_service.cpp:7340-7362`), which is the same family `DISK`'s
`root_fs_dir` capacity lands in.

**The gap.** `selectKVCacheBackendCapacity` (`pkg/worker/controllers/worker/kv_cache_backend.go:1057`)
reads `managed.Members[0].Medium`. With a memory tier and a disk tier on **the same** group, there is
no single family to pick: the memory pair describes the segments and the file pair describes the SSD,
and both are real.

- A managed backend with **no** `localDisk` anywhere reads the memory pair, exactly as today.
- A managed backend with a `localDisk` tier reads **the sum of both pairs**, by the rule the external
  branch already uses and for the same reason: the backend is made of both, the leader serializes
  both unconditionally, and a tier that is not in use contributes its zero.
- `sumCapacityGauges` already exists, already saturates rather than wraps, and is reused unchanged.
- **The medium-per-group selection therefore disappears entirely**, which is why F4 caps the disk
  tier at one group: the moment two groups could carry different tiers, a single `status.capacity`
  stops being able to describe them and the field would need a per-tier shape this spec does not add.

- **Acceptance:** a backend with a disk tier whose exposition carries `1Ti` memory and `4Ti` file
  publishes `5Ti`, and a backend with no disk tier whose exposition carries the same publishes `1Ti`
  — one exposition, two specs, two answers, so the rule is asserted on the spec and not on the body.
- **Acceptance:** every absent-versus-zero rule of the backend spec survives: a failed scrape leaves
  capacity absent, a scrape gated by `service_ready: false` leaves it absent, and neither publishes a
  zero.

#### F7 — The eviction watermarks stay on the escape hatch (D3)

`eviction_high_watermark_ratio` and `eviction_ratio` get **no fields**. Non-Goals states the rule and
the search that backs it; this feature records the one candidate reader, because this spec is what
would make that reader possible and a later reader who finds it will otherwise mistake the omission
for an oversight.

The failure it would guard is real and measured elsewhere in this project's research: with **no** SSD
tier, raising the high watermark evicts the five to ten percent long tail that would have been read
again within the half hour, and the hit rate falls off a cliff. The guard would be: refuse raising
the watermark on a backend that has no disk tier. Before this spec, "has no disk tier" is true of
**every** backend, so the rule is a tautology and could not have been written. After F1 it becomes a
real predicate — which is exactly why it is written down here rather than left for someone to
rediscover.

It still does not justify a field, and the difference from `multiTenancy` is the whole argument: that
reader would be **this same object's own webhook**, reading a value it also renders, whereas
`multiTenancy`'s reader is a **different API** whose admission would otherwise have to rule on an
unschema'd string it does not own. A webhook reading its own `extraArgs` map needs the value
*parsed*, not *typed*, and parsing two keys is not an API change.

- **Acceptance:** no field is added, and the documentation carries the watermark warning next to the
  disk tier, so the operator most likely to raise the watermark reads why not to.
- **Recorded, not scheduled:** whether to add the parse-and-refuse rule to the webhook is left open
  (Open Questions). It costs no API change in either direction, so deferring it forecloses nothing.

#### F8 — Documentation

`docs/kv-cache/backend.md` gains: the tiering model as the store implements it, stated as **offload
is routed to the owner of the memory replica**, with the consequence that a disk tier is a layer on a
group and never a group of its own; the two fields that turn it on and the admission rule that keeps
them paired; that the hostPath is **not** counted into any resource request and therefore that node
disk pressure is the operator's to watch; that `status.capacity` is the sum of both tiers once a disk
tier exists; the grace period, what it drains (the SSD tier's registration) and what it does not (the
memory segment, still dropped); the watermark warning of F7; and the table of F2 saying where each
removed `medium` value went. `docs/README.md`'s index is unchanged, since no page is added.

### Verification

**Hardware: a single-node Kubernetes cluster is sufficient for the first column below and cannot
answer the second.** The split is stated before any task is written, because an acceptance item that
cannot run on the cluster at hand is the kind that gets quietly closed by a fixture.

| Verifiable on a single-node cluster | Requires more than one node |
|---|---|
| A group with `localDisk` renders the volume, the mount, the five environment keys and the `preStop`; a group without renders none of them | **Growth**: widening `nodeSelector` adds a member — one node cannot grow |
| `terminationGracePeriodSeconds` is `grace + 60` | Whether an offload read **in flight from a peer** completes during the grace — it needs a peer |
| `POST /api/unmount_local_disk` returns 200 against a real member, holds for the grace, and answers 400 above 3600 seconds | Cross-node segment mount and unmount timing |
| The member registers a local disk segment the leader accepts, and `master_total_file_capacity_bytes` becomes the declared ceiling | Whether `drain_jobs` moves data — deferred entirely (Non-Goals), and unverifiable here regardless |
| `status.capacity` is the sum of both families with a disk tier and the memory family without | |
| Every admission rule of F4 | |
| **Not achieved: a key evicted from memory being read back off the disk.** See below — it is the one row that asks whether the tier does its job, and it did not pass | |

**What does not count as filling the second column.** Each of the deferred items is a claim about
**two machines**. A unit test over a rendered object, a fixture that replays a recorded body, or a
single-node run that exercises the same code path proves the renderer and proves nothing about the
claim. The gap closes when the item runs on a cluster with at least two nodes carrying the disk
tier, and until then the row stays open in this spec's Test Plan with that sentence attached. The
first column's last row is the one that must not be substituted for: **a key surviving eviction and
being read back off disk is the only item that asks whether the tier does its job**, and every other
row asks the system about itself.

#### The one item that did not pass: no byte reached the disk

Everything this operator renders was confirmed against a running store, and the tier still never
took a byte. Recorded in full because the gap is the finding, and because a later reader will
otherwise assume the acceptance simply was not attempted.

**Confirmed working, on a single-node cluster:**

- The three environment keys reach the client. The leader logs
  `Mount local disk segment with client id ... enable offloading is: 1`, so the member registered a
  local disk segment and the leader accepted it.
- `master_total_file_capacity_bytes` becomes exactly the declared ceiling, so `localDisk.capacity`
  travels end to end.
- The leader runs with `enable_offload=1` and, in one of the two runs, `offload_on_evict=1`.
- Eviction fires: the leader logs `[EVICT-TRIGGER] memory_ratio=0.975692 high_watermark=0.3`
  thousands of times against a deliberately small segment.

**And yet:**

```
[EVICT] No memory freed this cycle; 49 objects deferred for disk offload.
master_allocated_file_size_bytes 0        # after filling a 64 MiB segment to 97%
find <tier path> -type f | wc -l -> 0
```

Two configurations were tried and neither wrote anything: `offload_on_evict=true`, where the write
is deferred to eviction time, and the default, where it happens at `PutEnd`. The objects reach the
"deferred for disk offload" state and stay there.

**What this claim is bounded to.** It is *"in the two configurations reachable from this API, with
writes issued through the member's own REST API, on one node, nothing was written to the tier"*. It
is **not** "upstream offload is broken": there may be a precondition not set here, the REST write
path may not be the one that triggers an offload, and the behaviour was not tried against a native
client or a second node. What is established is that **rendering the tier correctly is not
sufficient to make it hold data**, which is the assumption this spec was written on.

**What would close it, and what would not.** It closes when a key written to a backend with a tier
is read back after its memory replica is gone, with a matching digest — on any cluster. It is **not**
closed by: the member registering its segment (that is confirmed and was never the question), the
file gauge reporting the ceiling (that is the declared figure, not an observed one), a unit test
over the rendered objects, or a fixture replaying a recorded exposition. Every one of those passes
today against a tier that holds nothing.

#### What the shutdown run established

Run on a single-node Kubernetes cluster against `kvcacheai/mooncake:0.3.13`, with the member's
environment and `preStop` argv taken **verbatim from the renderer's own output** rather than written
by hand for the run.

| Input | Observed |
|---|---|
| `grace_period_seconds: 0` | HTTP 200 in **0.02 s** |
| `grace_period_seconds: 8` | HTTP 200 in **8.02 s** |
| `grace_period_seconds: 3601` | HTTP **400**, `must be a non-negative integer no greater than 3600` |
| deleting the Pod with a grace of 8 | gone after **10 s**, and **no `FailedPreStopHook` event** |

The two grace values are what carry the finding. One measurement would say the call returned; the
pair says the wait is **driven by the grace** rather than by an endpoint that is simply slow. The
deletion timing is the separate claim that the kubelet actually waits for the hook, and the absent
event is what says the hook ran rather than failing into a log — `python3` is on the path, the
script parses, and the route is right.

**And the leader served all three capacity families at once**, which settles two claims this spec
makes from source alone:

```
master_total_capacity_bytes      268435456        # the memory segment
master_total_nof_capacity_bytes  0                # NoF has its OWN family
master_total_file_capacity_bytes 4398046511104    # exactly what the member reported for its SSD
```

So `NoF` was misclassified as file-backed (F6) and `LocalDisk` was classified correctly, and F6's
rule — a backend with a tier reads both families — is reading two figures that a real leader really
does publish side by side.

### Notes / Constraints / Caveats

- **The store's source is the citation, not `--help`.** Every artifact claim here names a file and
  line at v0.3.13 (`e5598b09`). The two disagree in this area: the design report attributed
  `-root_fs_dir` and `-global_file_segment_size` to `DFS`, and they belong to a different, older
  file-backed path.
- **`DISK` and `LOCAL_DISK` share one capacity gauge.** Both `root_fs_dir`'s configured size and each
  client's reported SSD capacity are added to `master_total_file_capacity_bytes`
  (`master_service.cpp:521`, `:7358`). This operator renders no `root_fs_dir`, so the gauge means the
  SSD tier and nothing else — a property worth stating because it stops being true the day a
  `DISK` tier is added.
- **`enable_disk_eviction` defaults to true** (`master.cpp:352`), so the SSD tier evicts on its own
  and needs no lifecycle of ours.
- **Replica counts per tier are engine-side.** `ReplicateConfig` carries `replica_num`,
  `nof_replica_num` and `dfs_replica_num`, all per-`Put` arguments the caller supplies
  (`store_py.cpp:1922-1931`). No field on this CR can set a per-tier redundancy.
- **Protobuf tags continue contiguously** from what each message already carries — nothing here has
  been released, so no number is reserved: `KVCacheBackendLeader` continues from 4,
  `KVCacheBackendMember` from 6, `KVCacheBackendManaged` from 2.
- **`make generate` is a single writer** and is contended while a parallel subject also edits
  `api/`. T1 takes the lock, runs it, and commits the generated output before anything else starts.
- Reconciliation stays level-based and idempotent: the new fields render into the same objects
  through the same aligners, and the member fingerprint already covers the volume, the mount, the
  environment and the `preStop`, so a change to any of them restarts exactly the members it must.
- External references, which are the claims here a reader can verify independently:
  - Mooncake repository, at the pinned version — <https://github.com/kvcache-ai/Mooncake>
  - Mooncake Store design — <https://kvcache-ai.github.io/Mooncake/design/mooncake-store.html>
  - The backend spec this one extends — `specs/2026-08-28-kv-cache-backend.md`
  - The pool spec that reads `leader.multiTenancy` — `specs/2026-08-28-kv-cache-pool.md`

### Boundaries

- **Always:** cite an artifact claim to a source line at the pinned version; keep a tier's
  configuration on the object that owns the thing it configures — the path on the group, the switch
  on the leader; refuse a pair the artifact degrades on rather than rendering it; leave a capacity
  figure absent rather than publishing zero or a stale one; keep `capacityPerMember` meaning the
  memory segment.
- **Ask first:** adding a field for any offload or promotion tuning knob; adding a per-tier shape to
  `status.capacity`; raising the member Pod's privileges beyond the hostPath this spec adds;
  rendering anything that would make a second member group carry a second disk tier.
- **Never:** render a member group that owns no memory segment and call it a medium; mount the disk
  tier as an `emptyDir`; count a hostPath into a resource request; default a hostPath location;
  render a `preStop` on a group with no disk tier; add an enum value whose only behaviour is to be
  refused.

### Risks and Mitigations

- **The disk tier fills the node's root filesystem and evicts everything on it** — this is the real
  cost of a hostPath that no request polices. Mitigated by making `path` required and undefaulted, so
  the operator names the filesystem; by `localDisk.capacity` rendering the artifact's own ceiling;
  and by the documentation stating plainly that node disk pressure is not accounted anywhere in
  Kubernetes for this volume.
- **The tier's directory is missing or unwritable on a node, and the operator cannot tell why.**
  Mitigated on two paths that both fail loudly — `FailedMount` for a missing directory, and a
  readiness probe that never passes for an unwritable one — and by documentation that carries the
  container's **own log line** verbatim next to what to do about it, since a message that does not
  name the fix is a message that costs a support round trip.
- **The tier is configured on one side only and reads as healthy** — the failure the whole shape
  exists to avoid. Mitigated by F4's two mirror-image admission rules, which make either half alone
  a refusal at apply time.
- **An operator raises the eviction watermark on a backend with no disk tier** and takes a hit-rate
  cliff. Mitigated by documentation in this scope (F7); a webhook rule is available and is D3.
- **`gracePeriodSeconds` is set above the Pod's window and the drain is cut short** — mitigated by
  deriving the window from the grace rather than letting the two be set independently.
- **A reader concludes from `preStop` that a shrink is now lossless.** Mitigated by stating in the
  field's own doc comment, in F5 and in the documentation that the **memory segment is still
  dropped**, and by recording why (no route returns a client's own segment ids) so the belief is
  falsifiable rather than folkloric.
- **Removing four enum values is read as removing four capabilities.** Mitigated by F2's table
  naming where each went, and by the fact that all four are refused at admission today, so nothing
  that ran stops running.
- **The offload path is exercised only on one node**, so a peer-served offload read is unproven.
  Recorded in the Verification split as a deferred item with what would close it.

## Design Details

### Commands

```bash
go build ./api/... ./pkg/...
make lint
make test
go test ./api/worker/v1alpha1/... ./pkg/worker/webhooks/worker/... \
        ./pkg/worker/controllers/worker/... ./pkg/worker/kvcache/...
make lint docs
make generate                 # T1 only, under the generate lock
```

`make generate` derives package paths GOPATH-style and requires a working directory ending in
`gpustack.ai/gpustack`; run it in a checkout that satisfies that and sync the delta back.

### Project Structure

```
api/worker/v1alpha1/
  kv_cache_backend.go                  # + localDisk, + leader.offload, + scaleIn; medium enum narrowed
  zz_generated.*                       # regenerated
pkg/worker/kvcache/mooncake/
  keys.go                              # + the two offload flags in Derived
  leader_flags.go                      # + the offload flags
  member_workload.go                   # + the disk tier: env, volume, mount, preStop, grace window
                                       # - MemberMediumIsReconciled, - memberFileBackedMedia
pkg/worker/webhooks/worker/
  kv_cache_backend.go                  # + F4's rules; - the per-medium refusal
pkg/worker/controllers/worker/
  kv_cache_backend.go                  # selectKVCacheBackendCapacity: sum where a disk tier exists
docs/kv-cache/backend.md               # F8
```

### Code Style

```go
// KVCacheBackendMemberLocalDisk is the local SSD tier this member group's nodes contribute.
//
// It is a LAYER on the group rather than a group of its own, and that is the store's shape, not a
// simplification. The leader routes an offload task to the client that owns the key's MEMORY
// replica, so a member holding no memory segment is never chosen and its disk stays empty — while
// still reporting its capacity into the leader's file gauges, which is a cold tier that reads as
// present and is not.
type KVCacheBackendMemberLocalDisk struct {
	// Path is the directory on each selected node that holds this tier. It is hostPath-mounted at
	// the same location in the member container.
	//
	// It is REQUIRED and has no default. The artifact defaults it to /data/file_storage, and
	// choosing a host directory on somebody else's nodes is not a default this operator may pick:
	// the wrong one fills a filesystem nothing here accounts for.
	//
	// +required
	// +k8s:validation:maxLength=4096
	Path string `json:"path" protobuf:"bytes,1,name=path"`

	// Capacity caps what this tier stores, rendered as the client's own total size limit. Left
	// unset, the artifact's 2 TiB ceiling applies and nothing is rendered.
	//
	// It is NOT counted into the Pod's resource requests. A hostPath is not ephemeral storage, so a
	// request against it would reserve a figure the kubelet does not police and make the member
	// unschedulable on the very node that has the disk.
	Capacity resource.Quantity `json:"capacity,omitempty" protobuf:"bytes,2,opt,name=capacity"`
}

// KVCacheBackendLeaderOffload turns the local-disk tier on, leader side.
//
// Both flags are the leader's, and Enabled gates the feature outright: the artifact ANDs
// offload_on_evict and promotion_on_hit with it, and every offload entry point returns early
// without it. A tier configured on the member alone is inert, so admission refuses either half.
type KVCacheBackendLeaderOffload struct {
	// Enabled turns on offloading to members' local disks.
	Enabled bool `json:"enabled,omitempty" protobuf:"varint,1,opt,name=enabled"`

	// OnEvict defers the write to disk from PutEnd to eviction time, so a key is written once if it
	// is never evicted. It requires Enabled; the artifact ANDs the two and would otherwise accept
	// the flag, log it back, and do nothing.
	OnEvict bool `json:"onEvict,omitempty" protobuf:"varint,2,opt,name=onEvict"`
}

// KVCacheBackendScaleIn is what a member does on its way out.
//
// It carries a duration and NOT a policy enum. The draft's second policy, migrating a member's data
// before it leaves, needs the store's drain job API — stateful orchestration this scope does not
// enter — so a policy field would ship with one value, which is a knob nobody can turn. It arrives
// when there are two.
type KVCacheBackendScaleIn struct {
	// GracePeriodSeconds is how long a departing member holds its local-disk tier open after
	// deregistering it, so offload reads already in flight finish there rather than failing.
	//
	// It reaches only the disk tier: the memory segment is still dropped, not drained, because no
	// route returns a client its own segment ids and the segment name is not derivable.
	//
	// The Pod's terminationGracePeriodSeconds is DERIVED from this rather than set beside it, so
	// the kubelet cannot kill the container in the middle of the wait this configures.
	//
	// A plain int32 and not a pointer: unset and zero mean the same thing here. Zero still
	// deregisters the tier, it just does not wait afterwards, which is exactly what a member with
	// no grace configured should do.
	//
	// The upper bound is the entrypoint's own: it refuses a larger value with HTTP 400, so a
	// manifest above it would render a preStop that fails on every shutdown.
	//
	// +k8s:validation:minimum=0
	// +k8s:validation:maximum=3600
	GracePeriodSeconds int32 `json:"gracePeriodSeconds,omitempty" protobuf:"varint,1,opt,name=gracePeriodSeconds"`
}
```

Conventions carried from the backend spec unchanged: enum markers on the spec side only; an observed
figure is a pointer and an absence is never rendered as zero; the artifact's snake_case spelling
lives only in the renderer.

### Implementation Plan

Generated artifacts land alone and first. Then admission, then the two renderers, then the
observation change, then the run and the page.

Checkpoints: after T1 (the types exist, a second `make generate` is a no-op); after T4 (a backend
with a disk tier renders completely); after T6 (capacity describes both tiers); after T8 (the first
column of Verification is met).

- [x] **T1 · The API types, the narrowed enum, and `make generate` green**
      Blocked by: none
      Owns: `api/worker/v1alpha1/kv_cache_backend.go`, `api/worker/v1alpha1/zz_generated.*`,
      `api/worker/v1alpha1/generated.*`, `api/worker/zz_generated.openapi.go`, `pkg/kubeclients/**`
      Gate: **the generate lock, taken before the task starts and released by its commit**
      Acceptance: `KVCacheBackendMemberLocalDisk`, `KVCacheBackendLeaderOffload` and
      `KVCacheBackendScaleIn` exist with the markers of Code Style; `members[].medium` is an enum of
      exactly `DRAM`; protobuf tags continue contiguously with no reserved gaps; **no behaviour is
      added in this task** — nothing renders, nothing validates, nothing is deleted from the packages
      that will lose code in T2 and T3. **All five generated families land in this one commit** —
      CRD schema, protobuf, deepcopy, openapi and applyconfiguration — because a Go type that moves
      without them is a silent inconsistency rather than a build failure, and because the generate
      lock is held by exactly one window at a time and is released by this commit.
      Verify: `make generate`, then a second `make generate` with `git diff --exit-code` clean;
      `go build ./api/... ./pkg/kubeclients/...`; `go test ./api/worker/v1alpha1/...`. The build is
      expected to still pass with the four values gone, because every site that consumed them is
      listed in F2 and none of them is deleted here — that list, not the compiler, is what makes the
      removal safe.

- [x] **T2 · Admission: the pairings, the path, the window, and the rule that dies**
      Blocked by: T1
      Owns: `pkg/worker/webhooks/worker/kv_cache_backend.go`,
      `pkg/worker/webhooks/worker/kv_cache_backend_test.go`
      Gate: review
      Acceptance: every rule of F4, each asserted by its message; `localDisk` added to the immutable
      set beside `medium` while `localDisk.capacity` stays mutable; the per-medium refusal removed
      along with its test cases, since the schema now refuses what it refused and a webhook rule no
      request can reach is dead code. The two-group limit relaxes **only** where no group carries
      `localDisk`, so the message an operator gets for two disk tiers names the capacity shape that
      would have to change rather than repeating the tiering follow-on.
      Verify: `go test ./pkg/worker/webhooks/worker/ -run KVCacheBackend`. Each new case is paired
      with a positive baseline that differs in exactly the field under test, so a construction error
      cannot make a whole group of refusals pass for the wrong reason.

- [x] **T3 · The member renderer: the tier, the mount, the preStop and the window**
      Blocked by: T1
      Owns: `pkg/worker/kvcache/mooncake/member_workload.go`,
      `pkg/worker/kvcache/mooncake/member_workload_test.go`
      Gate: review
      Acceptance: F1's five rendered items and F5's `preStop` plus derived
      `terminationGracePeriodSeconds`; the three dead medium sites of F2's table deleted —
      `MemberMediumIsReconciled`, `memberFileBackedMedia` and `memberRequests`'s bare
      `"LocalDisk"` branch — so a member's request becomes memory unconditionally; a group with no
      `localDisk` renders a Pod template **byte-identical** to what this file rendered before the
      change. `member_requests_local_disk`, the case covering the deleted branch, goes with it and is
      replaced by `member_requests_exclude_disk`, which asserts the opposite rule: a group **with** a
      disk tier still requests only memory.
      Verify: `go test ./pkg/worker/kvcache/mooncake/ -run MemberWorkload` — the tier asserted as a
      whole (all five present together, all five absent together), the byte-identical case against a
      **recorded** pre-change template (never against a fresh render from the changed code, which
      moves with the change and is green by construction), the grace window asserted at two different
      grace values so the derivation is tested rather than one arithmetic result, and a fingerprint
      case asserting the hash moves for each new field independently.

- [x] **T4 · The leader renderer: the offload flags**
      Blocked by: T1
      Owns: `pkg/worker/kvcache/mooncake/leader_flags.go`,
      `pkg/worker/kvcache/mooncake/leader_flags_test.go`, `pkg/worker/kvcache/mooncake/keys.go`
      Acceptance: F3's two flags rendered in deterministic order before the `extraArgs` tail and
      absent when unasked; both keys added to `LeaderExtraArgsRules.Derived`.
      Verify: `go test ./pkg/worker/kvcache/mooncake/ -run LeaderFlags` — including the existing
      absence-by-name test extended with the two new flags, so a backend that did not ask for the
      tier is asserted to carry neither.

- [x] **T5 · `status.capacity` across two tiers**
      Blocked by: T3
      Owns: `pkg/worker/controllers/worker/kv_cache_backend.go`,
      `pkg/worker/controllers/worker/kv_cache_backend_test.go`
      Gate: review
      Acceptance: F6 — the medium-per-group selection replaced by "sum where a disk tier exists,
      memory family otherwise", reusing `sumCapacityGauges` unchanged; every absent-versus-zero rule
      preserved. **The defect and its instrument are handled together**: the wrong `NoF`
      classification leaves with the enum value, and `capacity_file_medium` — the case that exercised
      the classification and agreed with it — is **rewritten rather than deleted or left standing**.
      Left standing it would go on passing against a medium that no longer exists, which is a green
      test measuring nothing; deleted, the selection rule would lose its only direct coverage. It
      becomes the disk-tier case: the same exposition, a spec that carries `localDisk`, asserting the
      file family is now reached through the tier's presence and not through a medium name.
      Verify: `go test ./pkg/worker/controllers/worker/ -run KVCacheBackendCapacity` — one exposition
      read under two specs giving two answers; the failed scrape, the family-missing and the
      `service_ready: false` cases re-run against the new selection so none of them regressed to a
      published zero.

- [x] **T6 · Reconcile: the tier's lifecycle across a spec change**
      Blocked by: T3, T5
      Owns: `pkg/worker/controllers/worker/kv_cache_backend.go`,
      `pkg/worker/controllers/worker/kv_cache_backend_test.go`
      Gate: review
      Acceptance: a capacity change re-renders one environment variable and the fingerprint restarts
      exactly the members of that group; widening `nodeSelector` still moves no fingerprint; the
      leader Deployment is byte-identical across a member-side capacity change; a second reconcile
      over settled state writes nothing.
      Verify: `go test ./pkg/worker/controllers/worker/ -run KVCacheBackendScale`.

- [x] **T7 · The `preStop` against a real member**
      Blocked by: T3
      Owns: no source paths — a recorded run, folded back into Verification
      Acceptance: `POST /api/unmount_local_disk` against a running member returns 200 with
      `{"grace_period_seconds": 30}`, returns 400 above 3600, and the call is observed to **hold**
      for approximately the grace before returning. The last half is what distinguishes the route
      being present from the grace being honoured, and only a timed call can tell them apart.
      Verify: the recorded transcript — the request, the status, and the wall-clock duration.

- [~] **T8 · The acceptance run: a tier that actually takes a key**
      Blocked by: T2, T4, T5, T6, T7
      Owns: no source paths — the recorded run and its figures
      Acceptance: every row of Verification's **first** column on a single-node cluster, ending with
      the one that is not about the system's own account of itself: a key written, evicted from
      memory under a watermark low enough to force it, and **read back** with a matching digest.
      Each row cites an observed effect — a served body, a moved gauge, a refused apply — never a
      flag being accepted or a log line echoing it.
      **Partially met, and the shortfall is the last row.** Every other row passed and is recorded
      in Verification. The read-back did not: the member registers its tier and the leader accepts
      it, eviction fires thousands of times, the objects reach `deferred for disk offload` — and
      `master_allocated_file_size_bytes` stays at zero with no file on the tier, in both of the
      configurations this API can render. Verification carries the transcript, what the claim is
      bounded to, and what does **not** count as closing it.
      Verify: the recorded transcript and the leader's `/metrics` before and after.

- [x] **T9 · Documentation**
      Blocked by: T8
      Owns: `docs/kv-cache/backend.md`
      Acceptance: F8's list, with the routing sentence — offload goes to the owner of the memory
      replica — stated first, because every other consequence follows from it.
      Verify: `make lint docs`.

### Test Plan

[ ] I/we understand the owners of the involved components may require updates to existing tests to
make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

- The member renderer's tests need a **recorded pre-change Pod template** for the no-disk-tier case.
  Comparing against a freshly rendered one would be a tautology; the point is that an existing
  backend's members do not roll when this ships.
- The webhook tests need a positive baseline per new refusal — a spec differing in exactly the field
  under test and expected to be **admitted**. A group of refusals that all fail for an unrelated
  construction error reads identically to a group of guards that all work.
- The capacity tests need one exposition body reused across two specs, rather than two bodies. The
  rule under test is a property of the spec, and giving each case its own body would let a wrong rule
  pass on the strength of a differently-shaped input.

#### Unit tests

**Admission** (`pkg/worker/webhooks/worker`) — asserted on the message, not only the verdict.

| Case | Input | Expected |
|---|---|---|
| `medium_local_disk` | `medium: LocalDisk` | refused by the **schema**, not here; no webhook case exists or can. Recorded so the removal of the old case is deliberate rather than lost |
| `local_disk_without_leader_offload` | a group with `localDisk`, no `leader.offload.enabled` | refused; message names `leader.offload.enabled` and says the tier would never receive a write |
| `leader_offload_without_any_local_disk` | `offload.enabled: true`, no group carries `localDisk` | refused; message names `members[].localDisk` |
| `on_evict_without_enabled` | `offload: {onEvict: true}` | refused; message says the artifact ANDs the two |
| `local_disk_path_relative` | `path: var/lib/x` | refused; message says it becomes a hostPath |
| `local_disk_path_root` | `path: /` | refused; message names the node's root filesystem |
| `local_disk_capacity_negative` | `capacity: -1Gi` | refused. A `resource.Quantity` is a string in the schema, so this is the only place it can be refused |
| `grace_above_upstream_max` | `gracePeriodSeconds: 3601` | refused; message names the entrypoint's own 3600 ceiling and that a larger value fails on every shutdown |
| `grace_without_a_disk_tier` | a grace on a backend with no tier | **admitted**. It is inert — no hook is rendered — and refusing it would make declaring a policy depend on the order two independent fields are edited in |
| `two_groups_no_disk_tier` | two `DRAM` groups, neither with `localDisk` | **admitted** — the one-group limit relaxes here |
| `two_groups_one_disk_tier` | two groups, **one** with `localDisk` | **admitted**. This is the case that keeps the rule below from being read as "two groups are refused again" |
| `two_groups_two_disk_tiers` | two groups, both with `localDisk` | refused; message names the capacity shape, not the tiering subject |
| `local_disk_immutable` | `localDisk` added to a running group | refused; message says data already sits under the old configuration |
| `local_disk_capacity_mutable` | `localDisk.capacity` raised, and separately lowered | admitted both ways. The rule is that the ceiling is not part of the group's identity, not that it may grow; testing only the raise would leave a later edit free to refuse a lowering with nothing going red |
| `disk_tier_exit_by_position` | the tier on the **last** group, dropped together with `leader.offload`; the same drop leaving offload on; the tier on an **earlier** group, dropped; a **sole** tiered group replaced in place | admitted, then three refusals. This is the only way a declared tier comes off, and it exists because the pairing stops at the end of the new list — a loop bound, which these cases turn into a contract before the documentation describes it |
| `no_local_disk_unchanged` | a canonical pre-change spec | admitted, byte-for-byte the same verdict as before this spec |

**Rendering** (`pkg/worker/kvcache/mooncake`)

| Case | Condition | Expected |
|---|---|---|
| `member_disk_tier_whole` | a group with `localDisk` | all five items present together: the two offload env keys, the size limit, the volume, the mount |
| `member_no_disk_tier_whole` | a group without | **none** of the five, asserted as a whole so a future path granting one silently fails here |
| `member_template_unchanged` | a canonical pre-change spec | Pod template byte-identical to the recorded pre-change template |
| `member_disk_capacity_unset` | `localDisk` with no `capacity` | the size-limit variable absent, not rendered at the artifact's 2 TiB default |
| `member_requests_exclude_disk` | `localDisk.capacity: 4Ti` | memory request is `capacityPerMember + localBufferSize`; no `ephemeral-storage` request at all |
| `member_prestop_body` | grace 30 | `preStop` posts exactly `{"grace_period_seconds": 30}` to the member's own REST port |
| `member_prestop_absent` | no `localDisk` | no `preStop`, and `terminationGracePeriodSeconds` still 60 |
| `member_grace_window_derived` | grace 10 and grace 300 | window 70 and 360 — two values, so the derivation is tested rather than one result |
| `member_fingerprint_per_field` | each new field moved independently | the hash moves for each; asserted per field, because a fingerprint over too little is indistinguishable from a correct one until the field it misses is the one that changed |
| `leader_offload_flags` | `offload: {enabled: true, onEvict: true}` | both flags, in order, before the `extraArgs` tail |
| `leader_offload_absent_by_name` | no `offload` block | neither flag rendered, asserted by name alongside the existing absence list |
| `member_rest_port_per_group` | groups 0 and 1 | `8080` and `8081`, asserted in all three spellings together — argv, readiness probe, `preStop` target — because one of the three lagging is the shape that makes a member unreachable on the port it was probed on. The first group renders no `--port`, so an existing member's fingerprint does not move |
| `member_grace_bound_matches_schema` | the Go ceiling and the generated CRD | equal, with the bound **read out of** `zz_generated.crds.go` rather than retyped. Two literals for one number drift silently, and the webhook message quotes the constant while the schema does the refusing |

**Observation** (`pkg/worker/controllers/worker`)

| Case | Condition | Expected |
|---|---|---|
| `capacity_sums_with_disk_tier` | one exposition, spec **with** `localDisk` | memory plus file |
| `capacity_memory_only_without_tier` | the same exposition, spec **without** | memory family alone |
| `capacity_scrape_failed_with_tier` | scrape errors, disk tier present | absent, `CapacityObserved=False` — not a partial sum |
| `capacity_zero_while_starting_with_tier` | clean all-zero exposition, `service_ready: false` | absent, not zero |
| `disk_capacity_change_restarts_group` | `localDisk.capacity` raised | that group's member Pods deleted, leader Deployment byte-identical |
| `widen_selector_still_no_restart` | selector widened on a group with a disk tier | no fingerprint moves, nothing deleted |
| `shared_identity_not_guessed` | two ready member Pods on one node, two segments | `MembersMounted=False` with `AmbiguousMemberIdentity`; the message names the shared key, both Pods and the action. `status.members[]` carries **no** node and **no** medium. Asserted on the **absence of an assignment** rather than on the condition being False, so restoring any of the three attribution versions — or adding a "credit at least one" fallback to make the status look better — turns this red instead of passing on a nicer guess |
| `shared_identity_not_healthy` | the same two Pods, **one** segment | still `AmbiguousMemberIdentity`, phase `Degraded`. This is the direction that matters more: a permanent false shortfall is noisy, while a missing member reported as healthy is silent, and one segment behind a shared key says nothing about the second member |
| `shared_key_no_segment_uses` | two **TCP** groups on one node, each segment on its own pod IP | `Mounted`, both segments carrying the node. The two Pods share the node-name key and nothing arrives on it, so the ambiguity is judged on the listing rather than on the index. `Auto` resolves to TCP, so this is what two groups on a node ordinarily look like |
| `pod_indexed_twice_keeps_collision` | two ready Pods on a node whose **name is its address** | `AmbiguousMemberIdentity` naming **two** pods. Such a Pod is filed under one key twice; the second filing must not displace the entry recording the collision, or a reported ambiguity turns back into a silent guess |
| `unready_pod_does_not_take_the_segment` | one ready and one **starting** Pod on one key, one segment, the starting one filed last | `Mounted`, the segment carrying the ready Pod's node. Reading whichever Pod was filed last credits the starting one, and the ready member — the only one that could have produced the segment — is then reported short on a fully mounted backend |

#### Integration tests

- Reconcile against a fake ctrl client plus the fake admin round-tripper: create with a disk tier,
  both workloads rendered, `/health` ready, capacity observed as the sum, `Ready`; then the scrape
  fails and capacity goes absent while the phase stays `Ready`.
- Teardown with a disk tier: the hostPath is not something this operator removes, and the test
  asserts that it does not try — deleting a node's directory is not an operation a controller may
  perform on teardown, and the documentation says the data is left behind.

#### e2e tests

One run on a single-node Kubernetes cluster covering **the first column of Verification only**, each
row judged on an observed effect. The second column stays open, and this spec's Verification section
carries what would close it: a cluster with at least two nodes carrying the disk tier. A fixture,
a unit test over a rendered object, or a repeat of the single-node run **does not** close it, and
that sentence travels with the row so a later reader cannot mistake a green suite for a closed gap.

## Alternatives

- **Render `medium: LocalDisk` as a second member group.** This is the shape the enum suggested and
  the HANDOFF assumed, and it is rejected on measurement rather than on taste: the leader routes
  every offload task to the owner of the key's memory replica
  (`master_service.cpp:5541-5560`), so a group with no memory segment never receives one. The group
  would still report its SSD capacity into the leader's file gauges, so `status.capacity` would show
  a cold tier that never takes a byte. Building that deliberately is the silent failure this project
  refuses everywhere else.
- **Keep the five-value enum and rewrite the refusal messages** to say where each medium really
  lives. Conservative, and it is the fallback if narrowing an unreleased enum is judged too
  disruptive. Not preferred: it leaves four values whose only behaviour is to be refused, which the
  backend spec's own F1 argued teaches a reader that a feature exists. The messages would say
  "this is not a medium" while the schema says it is one, which is a contradiction inside one object.
- **Give `LocalDisk` a `medium` value that renders the tier onto the group it names, keeping the
  group count at one.** In other words, `medium: LocalDisk` meaning "this group is memory *and*
  disk". Rejected as a name that lies: the group still contributes a memory segment sized by
  `capacityPerMember`, so a value saying `LocalDisk` would misdescribe the majority of what the
  group does, and `DRAM` plus `localDisk` says both things without either being wrong.
- **Ship `scaleIn.policy` with `GracePeriod` alone**, so the field is there when `Migrate` arrives.
  Rejected by the rule this spec applies four times, and because the value costs nothing to add
  later: widening an enum is not a breaking change, while shipping a one-valued one is a field that
  reads as a choice and is not.
- **Drain the memory segment on `preStop` via `POST /api/unmount`.** Attractive — the route exists,
  takes a grace period, and reaches `unmount_and_free_segment`. Rejected because `segment_ids` is
  required and no route returns a client its own segment ids, and the name is not derivable since
  the leader appends a fresh port on every start. Recorded as an Open Question with the specific
  upstream change that would unblock it, so the next attempt tests the right thing.
- **Count the disk tier into `resources.requests.ephemeral-storage`.** Rejected: a hostPath is
  outside the kubelet's ephemeral-storage accounting entirely, so the request would reserve a figure
  nothing polices, and would then keep the member off the node that has the disk.
- **Use an `emptyDir` for the tier** so nothing touches the host. Rejected: the tier's whole value is
  surviving a restart, and an `emptyDir` is deleted with the Pod. An `emptyDir` with a `sizeLimit`
  would also make the kubelet evict the member when the tier fills, which is the opposite of the
  intended behaviour.
- **Add the eviction watermarks as fields now** (D3). Rejected, and the reason is the rule rather
  than the effort: a field exists when something reads it, and nothing does. The case *for* was that
  this spec makes a guard writable for the first time — refuse raising the watermark where there is
  no disk tier — and the case against is that the reader would be this object's own webhook, which
  needs the value parsed rather than typed. Recorded at length in F7 so the next spec does not read
  the absence as an oversight and put the fields back.
- **Take `drain_jobs` in this scope** so a shrink can be lossless. Rejected on two compounding
  facts: it is stateful orchestration, and it covers only `MEMORY` and `NOF_SSD` replicas. This is
  the section that establishes that coverage claim, so its coordinates carry the version they were
  read at — all of them **v0.3.13**, the artifact this spec reads throughout:
  - `Replica::get_segment_names()` returns an empty vector for `DISK`, `LOCAL_DISK` and `DFS`
    (`mooncake-store/include/replica.h:759-781`), and `GetReplicaSegmentNames()` keeps only the
    values it yields (`mooncake-store/include/master_service.h:1517-1528`), so those three replica
    types contribute no name at all.
  - The drain planner then skips a key whose segments it cannot name **without counting it as
    blocked** (`mooncake-store/src/master_service.cpp:12518-12524`). That is what makes the loss
    silent rather than merely reported, and it is decidable rather than inferred: the two branches
    immediately after it — a hard-pinned or unexpired key at `:12526-12532`, and no eligible target
    at `:12537-12540` — both `insert` into `blocked_unit_keys` before their `continue`, while this
    one does not. The same function is what the job's own "remaining" tally counts
    (`:12600-12609`), so such a key appears in neither figure.

  So a drain over a backend with a disk tier reports success while leaving that tier's data where it
  was. Shipping it next to the tier this spec adds would pair a lossless-sounding operation with the
  one tier it is silently lossy for.
- **Attribute each segment to one member Pod when two Pods answer to the same index key.** Allowing
  more than one member group made this reachable: two groups can be scheduled onto one node, and on
  the RDMA path both Pods hold the host's network namespace, so both answer to that node's name and
  to its address — which are the two keys the status join indexes on. Three versions of a rule that
  picks a winner were written, and each had a defect the next review round found. A plain map write
  kept the last Pod and dropped the other, which **under-counted** and left a healthy backend
  reporting a shortfall forever. Crediting the whole collision set on one match **over-counted**,
  which is the worse direction, because a member that really is missing then reads as healthy.
  Crediting as many of a key's Pods as that key produced segments credited one Pod twice across its
  two keys, and made a Pod collide with itself on a cluster whose node names are addresses. The
  correct rule is a bipartite matching over segments and Pods — and it would still be a guess. The
  leader reports a segment as `<host>:<transfer port>` in **both** of the fields it offers, the
  transfer port is bound at random, and it is not a fact any Pod carries (the members section of
  [`docs/kv-cache/backend.md`](../docs/kv-cache/backend.md) records four observed values, none of
  them configured). Two Pods behind one host are therefore indistinguishable in every observable
  field: the input does not determine the output, so every version was producing an approximation,
  and an approximation has an unbounded supply of holes — which is why each round found a new one
  rather than the last one again. What ships instead reports the ambiguity itself, as
  `MembersMounted=False` with reason `AmbiguousMemberIdentity`, naming the shared key and the Pods
  sharing it. **This is not a deferred feature and not a known limitation**: in that configuration
  the information an attribution needs does not exist, and the status says so rather than publishing
  a number.
  Two qualifiers make the rule narrow enough to be true, and both were learned by getting them wrong.
  Only **ready** Pods are candidates, because only a ready member can hold a segment — a key shared by
  one ready and one starting Pod has exactly one candidate and resolves normally. And the ambiguity is
  judged on the **listing**, not on the index: a shared key that no segment ever arrives on is not a
  problem, which is the ordinary state of two TCP groups on one node, since each advertises its own
  address while both answer to the node's name. Judging the index instead reports a healthy backend as
  Degraded and tells the operator to separate two groups that never collided.
- **Refuse the overlap at admission**, by having the webhook reject a backend whose member groups can
  both take the RDMA path. Rejected on information rather than on strictness: a real collision needs
  three conjuncts — the RDMA path, more than one group, and node selectors that actually put two
  groups on one node — and a webhook can evaluate the first two and not the third, so it would refuse
  configurations that never collide. The condition above is the same rule placed in the layer that
  can evaluate the third, because it fires only once a key really is shared.

## Open Questions

- **Whether `leader.offload.enabled` should become the escape valve from a declared tier.** As shipped,
  the pair rule and the tier's immutability compose into a lock: a group that has `localDisk` cannot
  give it up, and offload cannot be turned off while a group has one. The exit is narrow and depends on
  position — an update that drops the **last** group and clears `leader.offload` in the same edit is
  admitted, because the immutability rules pair groups by position and stop at the end of the new list;
  dropping an earlier group is refused, and a backend whose only group carries a tier has no exit but
  deleting the object. The case for relaxing it is that offload is a leader flag whose change strands no
  data, so an operator who concludes the tier is not working could stop writing to it without deleting
  the backend. The case against is that the resulting state — a tier configured correctly and receiving
  nothing — is the exact silent failure this spec refuses everywhere else, and the pair rule exists to
  make it unrepresentable. It is a judgement about which failure is worse, not a defect, so it is
  recorded with both sides rather than settled here. The behaviour as shipped is pinned by tests and
  documented, so relaxing it later is a decision rather than a discovery.
- **Whether this operator should be able to prepare the disk tier's directory itself**, under an
  explicit opt-in — something like an `initializePermissions` switch that renders an init container
  to create and chown the path. It is not done here because both ways of writing it are wrong in a
  way that would have to be apologised for in the documentation: `chown` has to name a uid, and
  `members[].image` can put a different vendor's build (and a different uid) on each group, while
  `chmod 0777` opens the directory to every process on the node. A design where either choice needs
  a caveat is one that is not settled yet. It stays an Open Question rather than a defect because a
  single answer — should the switch exist, and which semantics — closes it.
- **Whether the memory segment can ever be gracefully unmounted from a `preStop`.** It needs an
  upstream route returning a client its own segment ids — `POST /api/unmount` already accepts a
  grace period, so the id is the only missing input. Whether to ask upstream for it, or to derive it
  by having the reconciler read `/get_segments_detail` and pass the id into the hook at render time,
  is open. The second is expressible today but binds a Pod template to an observation, which is a
  shape nothing here has.
- **Whether `NoF` deserves an object of its own.** Its registration carries a target coordinate and
  no node affinity, so it is not a member group; whether it is a leader field, a list on the backend,
  or a separate CR is undecided, and nothing needs it yet.
- **Whether a `DFS` tier is reachable at all under this project's pools.** The store disables it
  where the config is not single-tenant, and `leader.multiTenancy` is what a `KVCachePool` requires
  of its backend. So the two are mutually exclusive upstream, and whether that is a permanent
  property or a gap upstream intends to close has not been asked.
- **Whether the webhook should parse and bound the eviction watermarks** without giving them fields
  (F7). It is now a writable rule for the first time, costs no API change either way, and is left
  open rather than scheduled.
- **What the disk tier's eviction interacts with.** `enable_disk_eviction` defaults true and the
  tier evicts on its own; whether its watermarks need surfacing beside the memory ones — and whether
  they share the reader question of F7 — is not settled here.
- **Whether a second disk tier will ever be wanted**, which is what F4's one-group cap defers. It
  would need `status.capacity` to carry a per-tier shape, which is an API change to a published
  status and therefore not a decision to take speculatively.
