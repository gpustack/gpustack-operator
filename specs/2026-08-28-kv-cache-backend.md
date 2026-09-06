# Spec: KVCacheBackend — Managed Orchestration of a Mooncake Store Backend

Status: Shipped
Type: Feature

## Summary

A KV cache backend is a set of machines that contribute a medium — host DRAM, a local disk, an
NVMe-oF namespace, a CXL DAX device, a distributed filesystem — to one pooled key/value store that
inference engines read and write. Today an operator who wants one deploys it by hand: a master
process, N store members, a metadata plane they all agree on, and a set of flags nobody can read back
off the cluster afterwards.

This spec adds one cluster-scoped CRD, `KVCacheBackend` in `worker.gpustack.ai/v1alpha1`, with its
reconciler and its validating webhook. The CR declares **which machines contribute what medium**; the
reconciler turns that into a running [Mooncake Store](https://github.com/kvcache-ai/Mooncake) cluster
— one leader Deployment plus one member workload per member group — and reports the cluster's
**observed** state back: the endpoint, the capacity the master itself reports, the members that
mounted, and who is using the backend. Two axes are kept apart and never collapsed: `spec.type` names
**who manages the store**, `members[].medium` names **where the bytes live**. Every capability the CR
offers was measured against the shipped artifact, and every capability the artifact refuses is
refused at admission with an actionable message rather than accepted and silently degraded.

## Motivation

### Goals

- **G1 (primary)** One declarative object describes a KV cache backend completely enough that the
  cluster can be rebuilt from it, and its `status` answers "is it up, how big is it, who is using it"
  without reading any other object.
- **G2** The CR offers **no value the artifact will not accept**. Every enum is a measured enum; a
  value the binary refuses is refused at admission with a message naming the measured refusal.
- **G3** `status` is **observed, not restated**. Capacity comes from the master's own Prometheus
  counters, health from the master's own `/health`, membership from the members' own state — never
  from summing the spec back to the user.
- **G4** Growing a backend is a spec edit, and it costs **no restart** of the master or of the
  members already running.
- **G5** A backend that something is using cannot be deleted out from under it. Referential integrity
  is carried in `status.usedBy` and enforced by a finalizer.
- **G6** Every enum and every connector has an **escape hatch**, so an operator who needs a shape we
  did not enumerate reaches it through the CR instead of patching the objects the reconciler owns.
- **G7** The medium axis is **vendor-neutral**. DRAM, local disk, NVMe-oF, CXL and a distributed
  filesystem are host resources; a backend built from them runs on any node in any cluster,
  independent of which accelerator the node carries. The **transport** a member uses is a separate
  axis and is *not* vendor-neutral (F2, F9): the master image needs no accelerator runtime, while a
  member image needs the runtime of the transport it uses.

### Non-Goals

Each of these is out of scope by decision, not by omission. The follow-on subject that owns it is
named so a reader can tell "not yet" from "not ever".

- **A backend nobody manages.** An existing RWX volume the engine's own `fs://` layer drives is not a
  value of `spec.type` here — not even a reserved one. Nothing implements it, and an enum value whose
  only behaviour is to be refused teaches a reader that the feature exists.
- **High availability of the leader beyond a single replica.** `spec.connection.managed.leader.replicas`
  accepts `1` and only `1` in this scope. Multi-replica election, `-enable_ha`, an HA backend store and
  leader-follower status belong to the **master-HA** spec. The HA backend store — `-enable_ha`
  with `-ha_backend_type=etcd|redis|k8s` — is a **different axis from the metadata plane** (F4) and
  exists only at `replicas > 1`, so it belongs to S5 rather than here. One measured fact travels with
  the exclusion, because it decides whether S5 can ship on an official artifact at all: on the
  published wheel's master, `-ha_backend_type=k8s` is refused at startup with
  `UNAVAILABLE_IN_CURRENT_MODE` — the Kubernetes-lease backend is declared but not compiled in — so a
  lease-based HA mode requires a self-built image. The refusal is identical on two published artifacts
  across two CUDA generations, and `k8s_lease_helper.cpp` does appear in the binary's strings — the
  backend is declared and not compiled in, which is why the flag is accepted before it is refused. The
  etcd backend, by contrast, ships: `libetcd_wrapper.so` (22 MB) and `EtcdTenantQuotaPolicyStore` are
  both in the wheel and `etcd` is the flag's own default. It was not run, so S5 owns proving it.

  **How urgent that follow-on is has not been measured, and the measurement comes first.** What a
  leader restart costs is unrecorded: the members hold the bytes, the leader holds the index of which
  member holds which key, and whether the members re-mount to a restarted leader on their own is
  exactly what nobody has run. The one hint is the flag's own help text — `-cluster_id` is documented
  as "used for kvcache persistence **in HA mode**" — which points at a non-HA leader keeping that
  index only in memory. Its answer decides everything downstream: if the index heals by itself, a
  single replica plus ordinary rescheduling is enough for a long time; if it does not, high
  availability becomes a requirement, and it forces a choice between building an image to get the
  Kubernetes-lease path and giving the product an etcd dependency it does not have today. That
  experiment is scheduled directly after this spec.
- **Tuning the RDMA path beyond selecting it.** `transport` carries `protocol` and nothing else. A
  topology-affinity or device-preference knob would have to name values some artifact accepts, and
  none was measured — an unmeasured enum is the thing this spec refuses everywhere else. Selecting
  RDMA is measured and lands here; tuning it needs a measurement first.
- **Multiple media per backend, and scale-out or scale-in with data migration.** The API *shape* lands
  here — `members` is a list, each entry carries its own `medium` — so the shape never has to change
  later. This scope reconciles **exactly one member group**, and the webhook refuses a second one with
  a message naming the follow-on. Data migration between media, and the master's `drain_jobs` admin
  API, belong to the tiering-and-drain spec, since shipped as
  `specs/2026-09-05-kv-cache-media-and-scaling.md`.
- **More than one backend behind one quota domain.** A quota domain fronting several backends belongs
  to the **master-HA / multi-backend** spec.
- **Quota, tenants and reuse domains.** `tenant_id`, `/api/v1/tenant_quotas` and per-tenant accounting
  belong to the pool-and-quota spec, since shipped as `specs/2026-08-28-kv-cache-pool.md`.
  `KVCacheBackend` carries no tenant field.
- **Anything about workloads, engines, routers or prefill/decode disaggregation.** Those belong to the
  **engine** specs (S6/S7/S8). Nothing here reads or writes a workload object.
- **Building a Mooncake container image.** This scope *consumes* an image named in `spec.image`. Which
  vendor variant that image is built from is an operational decision, informed by F2's measured table
  but made outside this spec.
- **Liveness checking for NVMe-oF members — Mooncake already does it.** The master ships
  `-nof_heartbeat_interval_sec` (10), `-nof_heartbeat_probe_timeout_ms` (1000) and
  `-nof_heartbeat_failures_threshold` (3 consecutive failures before the segment is unmounted), plus
  independent `-nof_eviction_high_watermark_ratio` / `-nof_eviction_ratio`. We add no probe of our own
  for that medium. This is a subtraction, recorded so nobody implements it twice.
- **Controlling the replica count of a stored object.** Mooncake's `replica_num` is a per-`Put`
  argument supplied by the caller, so no field on this CR can enforce a redundancy level. It is
  engine-side, set by the workload's connector configuration.

## Proposal

A `KVCacheBackend` is a cluster-scoped, admin-owned declaration of a physical resource. It names
nodes, claims host memory and host paths, and on the RDMA path needs `hostNetwork` plus
`/dev/infiniband`. Only a cluster administrator can legitimately declare one — which is why the
object is cluster-scoped and carries no namespace. Data-plane isolation between tenants is a
*different* axis, handled one layer up by the pool objects the quota spec adds, exactly as Kueue
separates `ClusterQueue` from `LocalQueue`
(<https://kueue.sigs.k8s.io/docs/concepts/>). One backend can be referenced by several pools; that is
the only reason a backend and a quota domain are separate objects at all.

```yaml
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCacheBackend                 # cluster-scoped
metadata:
  name: mooncake-dram
spec:
  # type: Mooncake                   # defaulted; who MANAGES the store — see F2
  # image: <ref>                     # optional; falls back to the pinned Setting — see F2
  connection:                        # exactly one of managed | external
    managed:
      leader: {}                     # replicas and allocationStrategy are both defaulted
      members:
      - nodeSelector: { kvcache-dram: "true" }
        medium: DRAM                 # where the BYTES live
        capacityPerMember: 500Gi
        localBufferSize: 4Gi
    # external:
    #   endpoints:
    #   - { name: Client, address: "mooncake.example:50051" }
    #   - { name: Admin,  address: "mooncake.example:9003" }
  # transport:
  #   protocol: Auto                 # defaulted; resolved per member — see F9
status:
  phase: Ready
  phaseMessage: ""
  endpoints:
  - { name: Client, address: "mooncake-dram-leader.gpustack-system.svc:50051" }
  - { name: Admin,  address: "mooncake-dram-leader.gpustack-system.svc:9003" }   # see F7
  capacity:
    total: 8Ti
    used: 5.1Ti
  members:
  - nodeName: n7
    segmentName: n7-dram
    medium: DRAM
    protocol: TCP                    # what the master reports, not the requested Auto
    state: OK                        # the master's own segment state, passed through
  usedBy:
  - apiGroup: worker.gpustack.ai
    kind: KVCachePool
    name: team-a-pool
  conditions: [ ... ]
```

**Two vocabularies, kept apart on purpose.** This API says **leader** — the field, the type, the
condition, the rendered Deployment and the source files all use it. The artifact says **master** — the
binary, its flags, its metrics and its log lines — and every quotation of those keeps that word
verbatim. Renaming inside a measurement would corrupt the evidence it is there to carry, so a "master"
below is the artifact talking, not a spelling that escaped.

**The two axes.** `spec.type` is the store **implementation** — who does placement, eviction,
replication and metadata. `members[].medium` is the **medium** — what a member group physically
contributes.

| field | meaning | values |
|---|---|---|
| `spec.type` | the backend implementation | `Mooncake`, the only value; defaulted, so a manifest need not carry it. An unmanaged backend is a Non-Goal, not a reserved value. |
| `members[].medium` | the medium this member group is | `DRAM` / `LocalDisk` / `NoF` / `CXL` / `DFS` (`DFS` covers NFS and 3FS) |

The master's own flags are the evidence that these are two layers and not one:
`-global_file_segment_size` is documented as "Size of global **NFS/3FS** segment in bytes", and the
metric `master_total_file_capacity_bytes` as "Total capacity for file storage in **3fs/nfs**". So NFS
and 3FS are not alternatives to Mooncake — they are media a Mooncake segment sits on. "I want to use
NFS" is therefore not a choice of `type`; it is a member group with `medium: DFS`.

**`capacityPerMember`, not per node.** The field sizes a *member*, because a node can eventually run
several members — one per NUMA domain, for instance. This scope places exactly one member per
selected node, and the field keeps its name so it need not be renamed when that changes.

### User Stories

#### Story 1

As a cluster administrator, I want to declare "these three nodes each contribute 500 GiB of DRAM to a
KV cache" in one object, so that a Mooncake master and three store members come up without me writing
a Deployment, a Service, a DaemonSet and a flag list by hand.

#### Story 2

As a cluster administrator, I want `status.capacity.total` to be the number the **master** reports, so
that when it disagrees with the sum of what I asked for I learn that a member failed to mount instead
of reading my own spec back as if it were an observation.

#### Story 3

As a cluster administrator, I want to add a fourth node by editing `members[].nodeSelector`, so that
the cache grows while the engines keep reading — no master restart, no member restart, no cache flush.

#### Story 4

As a cluster administrator, I want a delete of a backend that a pool is still using to be **refused**
with a message naming the pool, so that I cannot destroy a running fleet's cache with one `kubectl
delete`.

#### Story 5

As a cluster administrator who assumed a KV cache needs an etcd, I want the API's **default** to be
the metadata plane that is measured to work with nothing else deployed, and I want the peer-to-peer plane:
etcd` to be refused at `kubectl apply` time with a message naming the follow-on that owns it, so that
I neither stand up an etcd I do not need nor spend a day reading logs of a master that came up on a
different metadata plane than I asked for.

#### Story 6

As a cluster administrator with a flag we did not enumerate — an offload ratio, a promotion threshold,
an allocator choice — I want to set it through the CR, so that I never have to `kubectl patch` a
Deployment the reconciler owns and watch my edit disappear on the next reconcile.

### Core Features & Acceptance Criteria

#### F1 — The object: cluster-scoped, two axes, one escape value

- `KVCacheBackend` is cluster-scoped, `subResources=["status"]`, short name `kvcb`, in
  `worker.gpustack.ai/v1alpha1` beside `Devices`, `Instance` and `InstanceType`.
- `spec.type` is an enum of exactly `Mooncake`, defaulted to it and optional. It carries one value on
  purpose: the field exists so the object states what it is and so a second implementation widens an
  enum rather than reinterpreting an absent field. An unmanaged backend — where nobody does placement
  and the engine's own `fs://` layer does the work — is a Non-Goal and is **not** a reserved enum
  value, because a value whose only behaviour is to be refused is an enum entry that teaches nothing.
- `members[].medium` is an enum of `DRAM`, `LocalDisk`, `NoF`, `CXL`, `DFS`. The **schema** carries
  all five, because all five have measured backing in the master's own flags and a tiered backend
  must not have to change the shape later. **Admission reconciles one of them, `DRAM`, and refuses
  the other four.** Each of the four additionally needs the leader's file or DAX flags and a mount on
  the member, and this scope renders neither, so a group naming one would come up holding its segment
  in DRAM while `status` read the file gauges — two halves disagreeing, in silence, on what the
  backend is made of. The refusal names what would have to render it, so "not yet" is legible as
  "not yet" and not as "not supported". What each would need, and why the enum keeps all five:
  - `DRAM` / `LocalDisk` — of the three file backends `DISK` / `LOCAL_DISK` / `DFS`, only the
    combination `DISK` + `LOCAL_DISK` is explicitly forbidden.
  - `NoF` — NVMe-oF, with its own heartbeat and eviction flags (see Non-Goals).
  - `CXL` — `-enable_cxl` plus `-cxl_path` (a DAX device, default `/dev/dax0.0`) plus `-cxl_size`, and
    `-allocation_strategy` carries a dedicated `cxl` strategy.
  - `DFS` — NFS / 3FS, via `-global_file_segment_size` and `-root_fs_dir`.

  The medium still selects the capacity family a group is read from (F7). That half is written and
  tested now rather than deferred, because it is the half the refusal above is protecting: the day
  the four are rendered, the gauge each is read from is already decided and already covered.
- **Acceptance:** a manifest with `type: nfs`, `type: 3fs` or any medium name on `spec.type` is
  rejected by the **CRD schema**, before any webhook runs: structural validation happens in
  `rest.BeforeCreate` and the validating admission chain runs after it
  (`k8s.io/apiserver/pkg/registry/generic/registry/store.go`, `BeforeCreate` then `createValidation`),
  so a value outside an enum never reaches a webhook and a webhook cannot improve its message. The
  guidance instead lives where someone looks for it — the field's own description, which the schema
  carries and `kubectl explain spec.type` prints, states that the medium is `members[].medium`.
- **Acceptance:** an object with two member groups, `DRAM` and `DFS`, **parses and validates against
  the schema** (the shape is expressible) and is then refused by the webhook for carrying more than
  one group, with a message naming the tiering follow-on. A shape that validates but is not yet
  reconciled fails loudly at admission, never half-way through a reconcile.
- **Acceptance:** `kubectl get kvcb` prints name, type, phase, endpoint and capacity. The endpoint
  column selects the `Client` entry out of `status.endpoints` by name, since that is the address a
  reader wants and the list is keyed rather than ordered. This is the repository's **first** use of
  the `+k8s:crd-gen:printcolumn` marker, and standing it up required a one-line fix to the generator:
  it emitted `Priority` — an `int32` — wrapped in quotes, so any CRD that declared a printer column
  produced a Go file that did not compile. Both halves were then measured on a live cluster rather
  than reasoned about, because a JSONPath filter is exactly the kind of expression a printer either
  accepts or silently blanks:

  ```console
  $ kubectl get kvcb
  NAME             TYPE       PHASE   ENDPOINT                                          CAPACITY
  printcol-probe   Mooncake   Ready   printcol-probe-leader.gpustack-system.svc:50051   12Gi
  ```

#### F2 — `spec.image` is explicit and optional, and never derived

`spec.image` is an **optional** string. Left unset, the reconciler uses the cluster-wide default
pinned in a new `kv-cache-backend-image` Setting, following the existing `instance-ssh-server-image`
precedent (`pkg/worker/settings/value.go`, read at render time through `ShouldValue`). A version this
project has verified belongs in one place an admin pins once, not restated on every object; setting
the field overrides that default for one backend. Unset in **both** places is refused at admission,
with a message naming both.

**The Setting itself ships blank, and the reason is the leader/member split rather than a want of a
verified image.** One image *is* measured end to end below. But one value has to be right for every
backend in the cluster at once, and the two roles do not want the same thing: the leader needs no
accelerator runtime at all, while a member's transports and the runtime it links are compiled into its
wheel. A backend on the `Ascend` transport — the transport is backend-wide — or a member group placed
on other accelerator hardware needs a build nothing here can guess at. Blank makes that mismatch an
admission refusal naming both places to fix; a default would make it a loader error at runtime. The
measured image is also a third party's, unlike every other image Setting in this repo, each of which
points at something the project publishes.

What it is never is **derived** from the operator's own image, which breaks deliberately with how this
repo derives the Device Manager image from the worker image.

The reason is that the master's link-time dependencies differ per published vendor variant. Measured
on 2026-08-28 against two official artifacts — the PyPI `mooncake_transfer_engine` wheel
0.3.12.post1 for cp312/x86_64, and a CUDA-13 vLLM image that already carries mooncake. Both gave
identical answers on every negative item.

```
$ ldd mooncake_master | grep -E "not found"
        libcuda.so.1 => not found
        libcudart.so.12 => not found
$ ./mooncake_master --help
./mooncake_master: error while loading shared libraries: libcuda.so.1: cannot open shared object file
```

`libibverbs.so.1` and `libmlx5.so.1` are hard `DT_NEEDED` entries too. Two **empty stub `.so` files**
with the right SONAMEs make the master fully functional — `/metrics`, `/health` and the admin API all
served — which proves the dependency is **link-time only: the master never calls CUDA at runtime**.
The same stubs are *not* sufficient for the Python client `mooncake/store.so`, which needs a
versioned symbol (`ImportError: undefined symbol: cudaFreeHost, version libcudart.so.12`).

That is a property of one variant's *packaging*, not of the master. PyPI carries five official vendor
variants, in lockstep at 0.3.13
(<https://pypi.org/project/mooncake-transfer-engine/>):

| package | what it is | files | arch coverage |
|---|---|---|---|
| `mooncake-transfer-engine` | base package — actually a CUDA 12 build | 8 | x86_64 + aarch64, cp310–313 |
| `mooncake-transfer-engine-cuda13` | CUDA 13 | 8 | x86_64 + aarch64 |
| `mooncake-transfer-engine-npu` | Ascend NPU (the name is `-npu`; `-ascend` does not exist) | 10 | x86_64 + aarch64 |
| `mooncake-transfer-engine-rocm` | AMD ROCm | 4 | x86_64 only |
| `mooncake-transfer-engine-musa` | Moore Threads MUSA | 1 file (cp310/x86_64) | a maturity signal |

`readelf -d` on each variant's `mooncake_master`, on one CUDA-less host:

| variant | accelerator libs in `DT_NEEDED` | unresolved |
|---|---|---|
| base (CUDA 12) | `libcuda.so.1` + `libcudart.so.12` + `libmlx5.so.1` + `libibverbs.so.1` | 2 |
| `-rocm` | none — only `libibverbs.so.1`; not one ROCm/HIP library | 0 |
| `-npu` | none — not even `libibverbs.so.1`; the leanest | 0 |

- The master is a **pure metadata service**, so the master image does not have to match the cluster's
  accelerator vendor: an `-npu`-built master on an all-NVIDIA cluster is legitimate. Measured further,
  the `-npu` variant's `mooncake_master --help` runs on a host carrying **neither CUDA nor CANN** and
  prints 138 flags, including `enable_multi_tenants`, `ha_backend_type`, `enable_kv_events` and
  `tenant_quota_connector_type` — so the accelerator-free variants are **flag-compatible** with the
  base build and need no accelerator runtime at all.
- Using the base package's master requires either a CUDA runtime base image or two stub `.so` files
  in the image. Either is the image builder's choice, made outside this spec — and the project makes
  it, so nothing has to be built to run this. `docker.io/kvcacheai/mooncake:0.3.13` is published for
  amd64 and arm64 from `docker/master.Dockerfile`, and it takes the **mixed** route: a stub
  `libcuda.so.1` from the CUDA image's `stubs/` directory, and the **real** `libcudart.so.12` from
  outside it. That distinction is what makes one image serve both sides — the stub is enough for the
  master, and the real runtime is what the client's `cudaFreeHost` versioned symbol needs. Neither
  library reaches the driver, so the image runs on a host with no GPU.
  Two further properties of that image matter to anyone reproducing a run. `pip install` puts the
  whole wheel in, so `mooncake_master` and `mc_store_rest_server` are both present and **one
  `spec.image` genuinely serves the leader and the members**. And `ibverbs-providers` is installed
  because `libmlx5.so.1` is a hard `DT_NEEDED` on `engine.so` since 0.3.12 — `import mooncake.engine`
  fails outright without it, *even for TCP-only use* — so an RDMA-free cluster needs no RDMA hardware
  but the image must still carry the library.

The **client** side is where the vendor lives, and it lives in the published *wheel* rather than in a
custom build. Measured on the same day:

| variant | client layout | transports compiled in |
|---|---|---|
| base (CUDA 12) | everything static in `store.so` (18.8 MB) | `RdmaTransport`, `TcpTransport` — and nothing else |
| `-rocm` | `store.so` 19.3 MB | `HipTransport`, `RdmaTransport`, `TcpTransport` |
| `-npu` | thin Python shims (`store.so` 1.4 MB, `engine.so` 3.3 MB) over `libmooncake_store.so` (22.3 MB), `libtransfer_engine.so` and `libmooncake_common.so` | plus a **separate `ascend_transport.so`** (317 KB) |

- **No custom build is needed for AMD or for Ascend.** The per-vendor wheel *is* the vendor-transport
  story: `-rocm` carries `HipTransport` out of the box, and `-npu` ships `ascend_transport.so`.
- What Ascend needs instead is a **CANN runtime in the member container**: `readelf -d
  ascend_transport.so` gives `DT_NEEDED` `libascendcl.so`, `libllm_datadist.so` and `libmetadef.so`.
  Confirmed by behaviour and not by inspection alone — the `-npu` `transfer_engine_bench` refuses to
  start on a CANN-less host with `error while loading shared libraries: libascendcl.so`, while the
  `-npu` **master** on that same host runs fine.
- **The master image needs no accelerator runtime; a member image needs the runtime of the transport
  it uses.** That is the sentence whoever chooses an image needs, and it is why the image is three
  two fields rather than one: `spec.image` is the default and `members[].image` overrides it per member
  group. That is the split a single field genuinely cannot express — two groups on different transports
  need different runtimes. There is deliberately no per-leader override: the leader needs no
  accelerator runtime, so running it on the heaviest member's image costs one node a larger pull and
  nothing else, which is not worth an API field. The override is optional and falls back to
  `spec.image`, so the simple case stays one field.
- **A parser that accepts a protocol string is not evidence that the transport is compiled in.** The
  client config parser accepts `rdma`, `barex`, `tcp`, `efa`, `nvlink`, `nvlink_intra`, `hip`,
  `ascend`, `maca`, `cxl`, `nvmeof`, `sunrise_link`, `ub`, `ubshmem` and `rpc_only`. That is a parser
  set; the table above is the capability list, and only the table may back an enum this CR offers.
- The medium axis stays vendor-neutral (G7) — every medium in the enum is a host resource. The axis
  that carries a vendor dependency is the **transport** (F9), not the medium and not the backend.
- **Acceptance:** a spec with no `image` and an unset `kv-cache-backend-image` Setting is rejected at
  admission, with a message naming both places. The reconciler contains **no** fallback that derives
  an image from the operator image or from any default registry — asserted by a test that renders the
  workloads from a spec whose image is a sentinel and finds that sentinel and nothing else, and by a
  second that leaves the field empty and finds the Setting's value and nothing else.

#### F3 — `connection`: exactly one of `managed` or `external`

- `connection.managed` and `connection.external` are both pointers; exactly one must be set. Neither
  set, or both set, is rejected at admission with a message naming the two.
- `external.endpoints` is the escape hatch for a backend this operator does not manage: the
  reconciler renders **no workload at all**, mirrors the declared endpoints into `status.endpoints`,
  and reaches `Ready` only once the `Admin` endpoint reports `service_ready: true` (F6).
  `status.capacity` and `status.members` are read from that endpoint on the same three routes a
  managed backend is read through, and follow the same rules: capacity **absent** where the scrape
  fails, the listing **kept** where it fails. The two fields the listing cannot supply — node name and
  medium — stay empty, because the Pods behind an external backend are somebody else's and there is
  nothing to join them to. Its capacity is read differently for the same reason (F7).
- ⛔ An external address that does not answer is a **fault from the first pass**, and this is the one
  verdict that differs from the managed side. A managed leader is excused while its own Deployment
  reports no ready replica (F6) — this operator created that Pod moments ago — and an external backend
  has no such excuse: the address was declared to name something that already runs, so there is no Pod
  of ours to wait on. A mistyped or firewalled endpoint reported as `Provisioning` would wait forever
  for a start that is not coming, so it is `Error` with `LeaderUnreachable` and the address in the
  message.
- `managed.leader.replicas` is defaulted to `1` by the CRD schema and floored at `1` by it; the
  refusal of anything ABOVE one is the **webhook's**, not an enum's, and that split is deliberate.
  An enum would answer `Unsupported value: 2: supported values: "1"`, which tells an operator what is
  rejected and nothing about why or what to reach for; the webhook answers by naming the master-HA
  follow-on. Verified on a cluster — the schema carries `{default: 1, minimum: 1}` and no `enum`, and
  a `replicas: 2` apply comes back with the follow-on named. `-enable_ha` is therefore never set by
  this scope, and no HA backend store is engaged.
- `managed.members` must carry exactly one group (F1).
- **Acceptance:** an `external` backend reaches `Ready` with zero Deployments, Services and DaemonSets
  created, asserted against a fake client that fails the test on any create.

#### F4 — The metadata plane is peer-to-peer, and it has no field

The **transfer-engine metadata plane** is how the store's clients find one another and publish their
segment descriptors. For a store member it is the `metadata_server` config key
(`MOONCAKE_TE_META_DATA_SERVER`, F9), and this scope renders it as the literal `P2PHANDSHAKE`,
unconditionally. **There is no API field**: one value with no alternative is a knob nobody can turn,
and it would sit three levels deep configuring nothing. A server-backed plane arrives as a field when
something ships one.

**Two axes get confused here, so this states both.** The metadata plane above is one. The other is the
**HA backend store** — `-enable_ha` with `-ha_backend_type=etcd|redis|k8s` — which is how *several*
leader replicas elect one among them. That is where a **Kubernetes lease** lives, and it is why no
Kubernetes option appears on the metadata plane: leader election is not metadata discovery. It exists
only at `replicas > 1`, which this scope refuses, so it belongs to the master-HA follow-on.

And when S5 gets there, one measured fact decides what it can ship on: **`-ha_backend_type=k8s` is
refused at startup by the published artifact**, with `UNAVAILABLE_IN_CURRENT_MODE, backend_type=k8s`,
identically on two artifacts across two CUDA generations. `k8s_lease_helper.cpp` does appear in the
binary's strings — the backend is declared and not compiled in, which is why the flag is accepted
before it is refused. So Kubernetes-lease election needs a self-built image. The etcd backend, by
contrast, ships: `libetcd_wrapper.so` (22 MB) and `EtcdTenantQuotaPolicyStore` are both in the wheel
and `etcd` is that flag's own default. It was not run, so S5 owns proving it.

Keeping the two apart is what stops an operator deploying an etcd for a single-leader backend that
never contacts one.

⛔ **"No field" is not the same as "refused", and the difference was measured on a cluster.** A
manifest carrying `spec.metadata.mode: etcd` meets one of two fates, decided by the CLIENT and not by
this operator:

- a strict client — which is `kubectl apply`'s default — is refused by the API server with
  `strict decoding error: unknown field "spec.metadata"`;
- a client with validation off has the whole block **silently pruned**, and the object is admitted
  and reconciled as though the operator had never written it.

Neither path reaches a webhook, so neither names the follow-on: schema decoding precedes admission,
and a rule written here for a field that does not exist would be code no request can reach. The second
path is the one that matters, because it is indistinguishable from success at the point of apply — the
protection against it is not a message but the documentation (T13) stating that the metadata plane is
peer-to-peer and takes no configuration at all.

**Measured: a single-master backend needs no metadata service at all.** In the capability experiment
the master ran with `enable_ha=0` and never contacted etcd, while two store clients set up with
`metadata_server=P2PHANDSHAKE` completed put/get against it, including cross-tenant isolation.
Observed on a client:

```
Transfer Engine RPC using P2P handshake, listening on 127.0.0.1:15002
Transfer engine auto discovery is disabled for protocol: tcp
TcpTransport: listen on port 15995
Registering local memory: 67108864 bytes
Mounting segment: 268435456 bytes, 268435456 of 268435456
```

- **Acceptance:** the rendered member environment carries `MOONCAKE_TE_META_DATA_SERVER=P2PHANDSHAKE`
  and the rendered leader argv carries no metadata flag, asserted byte-for-byte. Nothing in the spec
  can change either, because nothing in the spec addresses them.
- **This scope deploys no etcd and requires none.** A single-leader `KVCacheBackend` has **zero
  external dependencies** beyond its image — measured, not assumed. Who would provide the etcd, when a
  server-backed plane ships, is the follow-on's question and is recorded as an Open Question.
- The leader's own alternative — `-enable_http_metadata_server` with
  `-http_metadata_server_{host,port}` (8080) — is what a server-backed plane would render. It is
  measured as *present* and not as *sufficient*, which is why no field offers it: an unproven mechanism
  behind an enum value reads as a supported one. It stays reachable through `extraArgs` (F8) for
  anyone establishing sufficiency, and is recorded as an Open Question.

#### F5 — The leader flag renderer, and why the flags are argv

The reconciler renders the leader's command line from the spec. Rendered as **argv, not environment
variables**, and the reason is that gflags' `-fromenv` / `-tryfromenv` take a comma-separated list of
flag *names* and then read `FLAGS_<name>` for each: choosing env vars costs one env var **plus** one
argv entry per flag, so it does not shorten the command line — it only splits the truth across two
places. argv is also what `kubectl get deploy -o yaml` shows, so what the master runs is diffable
without entering the container.

The **member** is rendered the other way round, as environment variables (F9), and the asymmetry is
not an inconsistency: the master is a gflags binary with no named variables of its own, so env buys
it nothing, while every member config key has a real named `MOONCAKE_*` variable the client reads
directly, so env buys the member the whole ConfigMap. Each side is rendered in the form its own
binary actually accepts.

The master's measured flag surface — from `mooncake_master --help` and the running process — of which
this scope renders a subset:

```
-rpc_port                  RPC port (0 = use -port); -port 50051 is deprecated
-rpc_address               bind address; REQUIRED in HA mode
-rpc_interface             network interface NAME; when set, its IPv4 overrides -rpc_address
-rpc_thread_num            0 = use -max_threads
-metrics_port              9003 by default — this is ALSO where the HTTP admin API lives
-client_ttl                10s — seconds a client stays alive after its last heartbeat; on expiry the
                           master treats it as disconnected and may unmount its segments
-allocation_strategy       random | free_ratio_first | cxl | ssd_free_ratio_first | local_first
-memory_allocator          cachelib | offset   (default offset)
-eviction_high_watermark_ratio 0.9      -eviction_ratio 0.05
-default_kv_lease_ttl      "10000" (ms; accepts ms/s/m/h suffixes)
-default_kv_soft_pin_ttl   "1800000"
-allow_evict_soft_pinned_objects  default TRUE
-enable_ha                 default false
-ha_backend_type           etcd | redis | k8s   (default "etcd")
-ha_backend_connstring     if unset, only backend_type=etcd falls back to -etcd_endpoints
-etcd_endpoints            semicolon-separated
-cluster_id                "mooncake_cluster"; used for kvcache persistence in HA mode
-root_fs_dir               used in HA mode
-pod_name / -pod_namespace for K8s label-based routing (default $POD_NAME / $POD_NAMESPACE)
-enable_http_metadata_server / -http_metadata_server_{host,port}   (8080)
-enable_metadata_cleanup_on_timeout   cleans mooncake/ram/*, mooncake/rpc_meta/* on client
                           heartbeat timeout
-quota_bytes               storage-backend quota (0 = 90% of capacity) — global, NOT per tenant
-use_mmap_arena_allocator / -mmap_arena_pool_size
```

- The renderer is a **pure function** from spec to `[]string`, with a deterministic order, so its
  whole surface is unit-testable without a cluster.
- `-rpc_port` is rendered; `-port` is not, because it is deprecated. Rendering it is **not**
  redundant with the artifact's default: in the source `rpc_port` defaults to `0`, and `0` means
  "fall back to `-port`" — so leaving it unset routes the port through the deprecated flag. An
  explicit value is what keeps it off that path.
- **No metadata flag is rendered at all.** The metadata plane is peer-to-peer (F4), so the master has
  no metadata store to point at and `-etcd_endpoints` is absent — as are `-enable_ha`,
  `-ha_backend_type` and `-ha_backend_connstring`, which belong to the HA axis this scope does not
  enter.
- `spec.connection.managed.leader.allocationStrategy` offers `Random` and `FreeRatioFirst` and maps
  each onto `-allocation_strategy`'s own spelling in one table. The flag accepts three more —
  `cxl`, `ssd_free_ratio_first`, `local_first` — and this API deliberately does not: each is specific
  to one medium or one locality model, so putting them in the enum would fix the API to one
  implementation's vocabulary. They stay reachable through the leader's `extraArgs`, and widening the
  enum later is not an API change. **The CR still offers no value the binary will not accept.**
- `-pod_name` / `-pod_namespace` are rendered explicitly, referring to environment variables the
  leader Pod fills from its own downward API. The variables carry this repository's own names —
  `KUBERNETES_POD_NAME` and `KUBERNETES_POD_NAMESPACE`, as `pkg/devicemanager/exporter/role.go`
  already spells them — rather than the bare `POD_NAME` / `POD_NAMESPACE` the flag documents as its
  default source. Nothing is lost by the difference: rendering the flags explicitly means the
  artifact never falls back to reading those variables itself.
- **Acceptance:** the renderer's output for a canonical spec is asserted argument by argument; a flag
  the spec does not mention is absent rather than rendered at its default, so a `--help` default
  change never turns into a silent behaviour change of ours.

#### F6 — Readiness, phase and the member listing come from the master's own admin API

One port — `-metrics_port` — serves the Prometheus exposition and the HTTP admin API both. The routes
below are read from the master's own registration table rather than probed for; the bodies are as
observed.

| Path | Method | What it gives |
|---|---|---|
| `/health` | GET | `{"status":"ok","role":"leader","ha_state":"serving","service_ready":true,"leader_address":null,"view_version":null}` |
| `/metrics` | GET | full Prometheus text exposition |
| `/get_segments_detail` | GET | JSON: `total_segments`, plus per segment `segment_name`, `segment_id`, `client_id`, `base_address`, `size_bytes`, `size_human`, `te_endpoint`, `protocol`, `status`, `allocator_used_bytes`, `allocator_capacity_bytes`, `allocator_usage_percent` |
| `/get_all_segments` | GET | plain text, one segment name per line |
| `/query_segment?segment=<name>` | GET | plain text: that segment's used and capacity bytes |
| `/api/v1/segments/status` | GET | one segment's status |
| `/api/v1/drain_jobs` | POST | creates a drain job |
| `/api/v1/drain_jobs/query` | GET | a drain job's state |
| `/api/v1/drain_jobs/cancel` | POST | cancels one |
| `/kv_events/status`, `/role`, `/ha_status`, `/leader`, `/query_key`, `/get_all_keys` | GET | present; not read by this scope |
| `/api/v1/tenant_quotas` | GET/PUT/DELETE | the quota spec's concern, not ours |
| `/` , `/tenant_quota_policy`, `/api/v1/cluster` | GET | 404 — no such route is registered |

**Two classes of route, and the difference decides how a starting master reads.** `/health`,
`/metrics`, `/leader`, `/role` and `/ha_status` answer **200 whatever state the master is in** — they
report that state rather than gate on it. Every other route above passes through the master's
`WithActiveService` wrapper and answers **503 `service plane is not active`** until the service plane
comes up.

The 503 carries no information `/health` does not already have: the wrapper's own test is
`service_available`, which is the field `/health` publishes as `service_ready`. The two agree by
construction, so the reconciler reads the one field and treats a 503 as agreement rather than as an
error. What it must not do is collapse 503 into 404 — the first is a phase, the second would be a bug
in this operator.

- `/health` is **what `status` is derived from** — it reports role, HA state, `service_ready` and
  `leader_address`, which is exactly the four-field view `status` needs. No document of our own is
  invented in its place.
- ⛔ **`"status": "ok"` in that body is a constant.** The master assigns it unconditionally before
  filling anything else in, so it reads `ok` on a master that is not serving, has no leader and holds
  no segment. **`service_ready` is the only field in the document that carries a verdict** — the rest
  describe, they do not judge. Nothing in this scope branches on `status`.
- ⛔ **…and for the same reason `/health` cannot be the `readinessProbe`.** It is in the ungated class:
  it answers **200 in every state**, so an `httpGet` probe pointed at it passes on a master that is not
  serving. The probe would not be weak, it would be inert — the Pod would go Ready the moment the HTTP
  server binds, and the Service would publish an endpoint that refuses RPC.
- **So the two probes take different paths, each matching what the probe is asking.**
  `readinessProbe` is `GET /get_all_segments`, which is in the gated class: 503 until the service
  plane is up, 200 (with a possibly empty body) after — exactly the question "may traffic go here?".
  `livenessProbe` is `GET /health` with a longer failure threshold, which asks the different question
  "is this process still answering at all?" — and must stay ungated, or a master that is slow to
  activate would be killed and restarted for being slow.
- `status.phase` is derived from that document plus the workloads' own state, and there are **five**:
  `Provisioning` (the workload is not scheduled yet, is pulling, or is running with `service_ready`
  false) → `Ready` (`service_ready` true and the master lists at least one segment) → `Degraded`
  (master serving, a member group short of its selected nodes) → `Error` (master unschedulable, or
  `/health` unreachable past its threshold) → `Deleting`. There is no separate `Pending`: to a reader
  a workload that has not been scheduled and a master that has not finished starting are the same
  state, and `phaseMessage` is what tells them apart.
  `conditions` carries the finer view, one condition per axis: `LeaderAvailable`, `MembersMounted`,
  `CapacityObserved`, `Deletable`.
- **What brings the next observation is two things, and it has to be two.** The reconciler watches
  the objects it renders, which covers a leader restarting or a node joining a group; it also watches
  the **leader's Pods**, because the one fault only ever written there is the scheduler's
  `PodScheduled=False`, and that write moves nothing on the Deployment — whose own account of it
  arrives ten minutes later as `ProgressDeadlineExceeded`. A leader's Pod carries no resource note
  (the Deployment's controller makes it from a template, and the note sits on the Deployment), so
  that watch matches the identity labels instead, which are the Deployment's immutable selector.
  Neither watch fires for a store whose Pods are steady while its **contents** move, and an external
  backend has no workload of ours to watch at all — so every pass also requeues on a **15-second**
  timer, the interval `InstanceType` re-summarizes on. Without it `status` would be whatever the
  first pass saw, which for an external backend means forever.
- **Each of the three admin reads gets its own deadline, and each response is bounded.** The reads
  are sequential, so one deadline around all of them is not a bound on any of them: a slow `/health`
  would leave the capacity scrape and the segment listing an almost-expired context and both would be
  reported as failures of their own, turning a leader that is merely slow into a leader whose gauges
  and listing are broken. Each response is also read through a cap, because an **external** address
  names a process an administrator declared and a wrong one can name anything at all — a truncated
  body fails its decode and is reported as a scrape failure, which is the right verdict for it.
- **Acceptance:** a `/health` body with `service_ready: false` never yields `Ready`, whatever the Pod
  phase says; a Pod that is `Running` with an unready master reads `Provisioning`.
- **Acceptance:** the phase and the conditions are derived from **observed** documents only. A test
  feeds recorded `/health` bodies (including a malformed one and a connection refusal) and asserts the
  phase, the message and every condition.
- **`status.members[]` is read from the master, not inferred from the Pods.**
  `/get_segments_detail` is the authoritative listing: one entry per mounted segment, each carrying
  that segment's own `status` and the `protocol` it actually resolved to. Both are published into
  `status.members[]` in this API's casing. The states the store defines are `OK`, `DRAINING`,
  `DRAINED`, `GRACEFULLY_UNMOUNTING`, `UNMOUNTING` and `UNDEFINED` — a shrink passes through the
  draining and unmounting ones, which is why the field carries the store's vocabulary rather than a
  three-value summary of it.
- **`state` carries no enum marker**, so a state this version does not recognise is published verbatim
  rather than rejected. The store owns that vocabulary and this API only transcribes it; Code Style
  states the rule and what it costs to break.
- A member Pod that is running but whose segment the master does not list is **absent** from
  `status.members[]`, and that is the honest rendering: the master is what allocation goes through, so
  a segment it does not know about holds nothing. `MembersMounted` counts the listing against the
  DaemonSet's ready Pods and carries the shortfall in its message.
- A scrape of the listing that fails leaves `members[]` **at its previous value** and sets
  `MembersMounted=False` with the failure in the message. This is deliberately the opposite of what a
  failed capacity scrape does (F7), and the difference is in the types: capacity is two pointers, so
  it has an `absent` that means "not observed", and it uses it. A list has no such state — an empty
  list is a legible value meaning "the master lists no segments" — so clearing it on a failed scrape
  would publish a falsehood. The stale list plus a `False` condition says what actually happened; the
  condition is what makes the staleness readable.

#### F7 — `status.capacity` is read from the master's Prometheus counters

The metrics the master exposes that bear on this scope — of which the reconciler reads the first two
rows and nothing else:

```
master_total_capacity_bytes            master_allocated_bytes
master_total_file_capacity_bytes       master_allocated_file_size_bytes
master_key_count                       master_soft_pin_key_count
master_active_clients
master_{put_start,put_end,get,exist_key,del,...}_{requests,failures}_total
```

- `status.capacity.total` is `master_total_capacity_bytes` for a memory medium and
  `master_total_file_capacity_bytes` for a file medium; `status.capacity.used` is
  `master_allocated_bytes` / `master_allocated_file_size_bytes` respectively. The reconciler
  **publishes the master's number** and never sums the spec.
- An **external** backend names no medium — this API says how to reach a backend, not what it is made
  of — so there is no pair to pick and the two are **added**: `total` is
  `master_total_capacity_bytes + master_total_file_capacity_bytes` and `used` the corresponding sum.
  The master serializes both families unconditionally whatever it uses — measured in
  `serialize_metrics`, which calls every gauge in turn — so the pool a single-tier backend does not
  use contributes its zero and the sum equals what the medium rule would have picked. Only a tiered
  external backend tells the two rules apart, and there the sum is the figure that describes it. This
  is still an observed number: it adds two gauges from one scrape, never a spec field.
- **Acceptance:** with one DRAM member group of N mounted members, `status.capacity.total` *equals*
  `N × capacityPerMember` — asserted as a check on the master's number, not as the way the number was
  produced. A test that replaces the scrape with a body reporting a different total asserts that the
  published value follows the **body**, not the spec.
- **Acceptance:** a scrape that fails leaves `capacity` **absent** and sets `CapacityObserved=False`
  with the failure in its message. It does not publish zero, and it does not keep publishing a stale
  value as if it were current.
- ⛔ **A successful scrape is not enough to publish, because a starting master scrapes clean and
  reports zero.** `/metrics` does not gate on the service plane the way the segment routes do: it
  serializes whatever the gauges hold and answers 200 in every state. The capacity gauges move only as
  segments mount, so a master that is up but not yet serving returns a well-formed exposition in which
  `master_total_capacity_bytes` is `0` — a value indistinguishable, at the parser, from a real empty
  cache.
  ⇒ **the gate on publishing capacity is `service_ready`, not the scrape's success.** With
  `service_ready: false`, `capacity` stays **absent** and `CapacityObserved=False` says the master is
  not serving yet. Reading a zero as an observation is precisely the failure the two rules above exist
  to prevent, and it is the one that would have slipped through them.
- `master_key_count`, `master_soft_pin_key_count`, `master_active_clients` and the request/failure
  counters are **not** copied into `status`; the master already exposes them on a scrapeable endpoint,
  and a status field would be a second, staler copy of them. `master_active_clients` in particular has
  **no reader here**: membership comes from the master's segment listing (F6), so a client count would
  be a second and weaker signal for the same thing.

#### F8 — The escape hatch: `extraArgs`, and why the reconcile loop makes it necessary

`spec.connection.managed.leader.extraArgs` and `spec.connection.managed.members[].extraArgs` are
`map[string]string` passthroughs. The master's are rendered as `-<key>=<value>` after the derived
flags. The member's are rendered as the entrypoint's own measured per-key override, `-D <key>=<value>`
(F9), and are keyed by **config key** rather than by environment-variable name — one namespace per
side, each the one its binary documents.

**One key is refused outright, and it is not a name collision.** `config_path` points the leader at a
YAML or JSON config file, and in the artifact's source `main()` loads that file first and then calls
`LoadConfigFromCmdline(config, conf_set)`, which guards most of its assignments with `if (!conf_set)`.
A config file therefore makes the command line largely **inert** — the reverse of the usual
precedence, and silently so. Through the escape hatch it would void every flag rendered from this
spec: the ports, the allocation strategy, the pod identity, with nothing reporting it. The webhook
refuses the key and says why. The member side has no equivalent hole, because its extraArgs renders
as a per-key override rather than as a flag: a key named after a config file sets a config key of
that name.

The reconcile loop is level-based: it renders the leader Deployment and the member workload from the
spec on every pass and converges them, so a hand `kubectl patch` of a rendered object is reverted
without notice. That is correct controller behaviour and it is exactly why the passthrough exists.
The failure mode being designed against is real and observed elsewhere: an upstream inference
operator's over-narrow enums pushed its users to `kubectl patch` the rendered objects, and its
reconcile loop then silently overwrote them. **Design for the patch you will otherwise receive.**

The knobs this reaches, none of which is a first-class field in this scope, are the tiering knobs
Mooncake already implements — surfaced, not reimplemented:

- Offload: `-enable_offload`, `-offload_on_evict` (defer the LOCAL_DISK offload to eviction time
  instead of `PutEnd`), `-offload_cap_ratio` (0.5), `-offloading_queue_limit` (50000),
  `-offload_force_evict`.
- Promotion: `-promotion_on_hit`, `-promotion_admission_threshold` (a CountMinSketch count, default 2
  — "promote on the second touch"), `-promotion_max_per_heartbeat` (default 1; the flag's own help
  states each promotion task is a **synchronous SSD read plus an RDMA write on the client**,
  serialized so as not to exceed the client-liveness window), `-promotion_queue_limit`.
- `-quota_bytes`, the **global** storage-backend quota (0 means 90% of capacity). It is deliberately
  not a field: naming it here would collide with the per-tenant quota vocabulary the quota spec owns.

- **Acceptance:** an `extraArgs` key that collides with a flag the renderer derives from a spec field
  is **rejected at admission**, with a message naming the field that owns the flag. Two sources for
  one flag would make the rendered argv ambiguous and the CR's own field a lie.
- **Acceptance:** a flag set only through `extraArgs` appears in the rendered Deployment, and
  survives an unchanged reconcile — asserted by reconciling twice and diffing the rendered object.

#### F9 — The member workload: entrypoint, environment, security context, and the port range

- One member group renders one **DaemonSet** over `members[].nodeSelector`. The semantic is what
  picks the shape: a member contributes *a node's* medium — it claims that node's host memory and
  host paths, and on the RDMA path that node's `/dev/infiniband` — so its identity **is** the node,
  which is what a DaemonSet expresses and a Deployment with anti-affinity only approximates. Kueue's
  Topology-Aware Scheduling accounts for either shape, so it is not an input to this choice.
- The member Pod declares its claim so capacity planning sees it: `resources.requests.memory` is
  `capacityPerMember + localBufferSize` for a memory medium, and `ephemeral-storage` for a local-disk
  medium. A member that cannot fit stays `Pending` and the backend reads `Degraded`, which is the
  honest outcome.
- **The member entrypoint is measured, not assumed:** the image's console script
  `mc_store_rest_server`, which is `mooncake.mooncake_store_service:sync_main`. It constructs a
  `MooncakeDistributedStore`, mounts a segment, then serves an aiohttp HTTP API. Its CLI:

```
--config <file>        client config (JSON)
-D key=value           repeatable per-key override
--port <int>           HTTP API port, default 8080
--max-wait-time <s>    default 60
```

  Its `main()` installs shutdown signal handlers, awaits a shutdown event, and its `stop()` calls
  `store.close()` — so it **handles `SIGTERM` gracefully and unmounts on exit by itself**. The drain
  `preStop` of F10 is therefore belt-and-braces over an unmount that already happens, and not the only
  path by which a segment is released.
- **Readiness is a TCP connect to that REST port, and the entrypoint's own ordering is what makes it
  mean something.** `main()` awaits `start_store_service()` — where `store.setup()` registers the
  segment with the master — and calls `start_http_service()` only once it returns true. The port
  opening *is* the mount completing, so a connect proves the mount rather than the process.
  - **Without it the two membership rows of F6 stop holding.** A container with no probe is Ready the
    moment it runs, and the shortfall holds every ready member Pod against the master's listing — so
    the whole startup window would read as a shortfall and put a healthy backend in `Degraded` on
    every rollout. This is the mechanism behind "a member that is starting is not a shortfall"; that
    rule is a claim about mount state, and `PodReady` only carries mount state because of this probe.
  - **TCP and not HTTP.** The API registers only `/api/*` data verbs — POST `mount`/`unmount`, PUT
    `put`, GET `get/{key}`, DELETE `remove`. There is no route to GET without a key or a side effect,
    and an unrouted path answers 404, which a probe never accepts.
  - The port is the entrypoint's `--port` default and nothing in this API can move it: the renderer
    passes no `--port`, and an `extraArgs` entry renders as `-D <config key>`, a different setting.
- **`MOONCAKE_LOCAL_HOSTNAME` is an ADDRESS, not a label, and it resolves from `status.podIP`.** It
  becomes the host half of the segment's `te_endpoint` — what the master hands a client to dial.
  Measured on a two-node cluster, advertising `spec.nodeName` instead:

```
  te_endpoint = gpuhost-worker:16608          # what the master advertised
  LISTEN 0.0.0.0:16608                        # inside the member pod's netns
  from a client pod:
    gpuhost-worker:16608  ECONNREFUSED
    <node IP>:16608       ECONNREFUSED
    <pod IP>:16608        CONNECTED
```

  The engine binds inside the pod's network namespace, so the node name named a port nothing listens
  on there. With `status.podIP` the same client connected to the advertised endpoint directly.
  - **It costs no stability.** The master appends a port of its own to build the segment name, and
    that port is fresh on every start — one restart moved a segment from `<host>:13720` to
    `<host>:14071` — so "the node name keeps the segment name durable" was never true. On the RDMA
    path the pod holds the host's network namespace and this resolves to the node's address anyway.
  - ⛔ **No acceptance item in T12 would have caught this**: all six stop at Ready, endpoint
    reachability, the capacity families, growth, deletion refusal and schema pruning. **None moves a
    byte through the data plane.** That gap is the finding, not just the address.
  - **Closed by measurement, not by argument.** With the address moved to `status.podIP`, an
    independent client Pod holding no segment of its own completed a real round trip against the
    master — a 1 MiB `put` returned a matching sha256 on `get`, and the member's
    `allocator_used_bytes` went 0 → 1048576 → 9437184 across a second 8 MiB write, against a segment
    whose endpoint is the member's pod IP. That run is acceptance item 7, added so the gap cannot
    reopen silently.
- The RDMA security context **adds** two capabilities; it does not reduce the container to them. Add
  is layered over the runtime's default set and nothing here drops that set, so "grants `IPC_LOCK` and
  `SYS_RESOURCE`" is a statement about what is requested, never about what the container ends up
  holding. Tightening it with `drop: ["ALL"]` is a separate, unverified change: the RDMA path itself
  has had no end-to-end run, so nothing establishes which of the runtime defaults the entrypoint needs.
- **The member's whole configuration renders as environment variables** — no ConfigMap, no volume, no
  init container, and neither `--config` nor a `-D` override is needed for a derived key. Every config
  key the client reads has a real named variable:

| config key | environment variable |
|---|---|
| `local_hostname` | `MOONCAKE_LOCAL_HOSTNAME` |
| `metadata_server` | `MOONCAKE_TE_META_DATA_SERVER` |
| `master_server_address` | `MOONCAKE_MASTER` |
| `protocol` | `MOONCAKE_PROTOCOL` |
| `device_name` | `MOONCAKE_DEVICE` |
| `global_segment_size` | `MOONCAKE_GLOBAL_SEGMENT_SIZE` |
| `local_buffer_size` | `MOONCAKE_LOCAL_BUFFER_SIZE` |
| `tenant_id` | `MOONCAKE_TENANT_ID` |
| `enable_ssd_offload` | `MOONCAKE_OFFLOAD_ENABLED` |
| `ssd_offload_path` | `MOONCAKE_OFFLOAD_FILE_STORAGE_PATH` |
| `enable_client_http_server` | `MOONCAKE_ENABLE_CLIENT_HTTP_SERVER` |
| `client_http_port` | `MOONCAKE_CLIENT_HTTP_PORT` |
| the JSON file `--config` would otherwise point at | `MOONCAKE_CONFIG_PATH` |

  This does not contradict the master being rendered as argv (F5): the master's `-fromenv` still needs
  the flag-name list in argv, so env buys it nothing, while the member has real named variables. Each
  side is rendered in the form its own binary accepts.
- **`MOONCAKE_TE_META_DATA_SERVER` carries an underscore inside `META_DATA`.** It is not
  `MOONCAKE_TE_METADATA_SERVER`, and normalising it to the spelling that reads correctly is the
  obvious mistake to make. Getting it wrong does not error — it **silently degrades the metadata
  plane** — so the name is a no-entry item, called out here and asserted byte-for-byte by its own unit
  test. Its value is the literal `P2PHANDSHAKE`, unconditionally (F4).
- `tenant_id` is left at its default `default`; the quota spec owns it. The pybind `setup()` entry
  point spells the RDMA device argument `rdma_devices`, while the config key and the environment
  variable are `device_name` / `MOONCAKE_DEVICE` — two names for one thing on two surfaces. The
  reconciler configures the member through the environment, so `MOONCAKE_DEVICE` is what it renders;
  this is recorded so nobody "corrects" it to match the Python signature.
- `spec.transport.protocol` is `auto` plus the transports **measured as compiled into a published
  wheel** (F2): `tcp`, `rdma`, `hip`, `ascend`. The flag help lists more —

```
-protocol (Transfer protocol: rdma|barex|tcp|efa|nvlink|nvlink_intra|hip|sunrise_link) default "rdma"
```

  — and the client's config parser accepts more still, but neither is a capability list. A value
  outside the four is refused at admission with a message saying that the parser accepts it and no
  measured wheel compiles it in.
- `hip` comes from the `-rocm` wheel and `ascend` from the `-npu` wheel's `ascend_transport.so`; each
  needs its vendor runtime **in the member container** — Ascend concretely needs CANN
  (`libascendcl.so`). The webhook cannot see inside `spec.image`, so this is a documented image
  requirement rather than an admission rule: `ascend` with a CANN-less image fails as a loader error
  in the member, and that container's own message is what `phaseMessage` carries.
  - **That last sentence costs two things, and neither is optional.** The container is rendered with
    `terminationMessagePolicy: FallbackToLogsOnError`, because the default reads only
    `/dev/termination-log` — which no artifact here writes — and would report the loader failure as an
    empty message with the reason `Error`. And the status reader takes the **last termination** before
    the waiting state: a crash-looping container is also waiting, and its waiting message is the
    kubelet's own `back-off … restarting failed container=…` boilerplate.
- **`Auto` is the CR default and resolves to `tcp`.** It is not a per-node probe that promotes
  itself, and two independent facts make that the only honest reading:
  - A member group renders **one DaemonSet**, so one Pod template covers every node the group
    selects. A per-node transport would need a per-node Pod spec, which that shape cannot express —
    and the transport is not one variable but four: `MOONCAKE_PROTOCOL`, `hostNetwork`, the
    `/dev/infiniband` mount and the two capabilities.
  - Promoting to `rdma` means **granting `hostNetwork` plus `IPC_LOCK` and `SYS_RESOURCE`**. A
    privilege is requested, never inferred on an operator's behalf, however available the fabric is.
  - There is also no input to probe with: nothing in this project reports a node's RDMA fabric
    today. That is the device-discovery chain's subject, and `Auto` is where it would attach later
    without an API change.
  - `rdma`, `hip` and `ascend` are therefore reached by **naming them**, and naming one is also what
    accepts the security context that comes with it.
- ⛔ **The artifact has no `auto` protocol**, so this resolution happens here rather than being
  passed through: the transfer engine looks its protocol string up in a transport map, and
  `MOONCAKE_PROTOCOL` defaults to `tcp` on the client's side. Rendering the literal `auto` would
  reach a lookup that finds nothing. `status.members[].protocol` is read back from the leader (F6),
  so what the member actually came up on is observed and not inferred from this field either way.
- **Security context, and it is genuinely modest:** the RDMA path needs `hostNetwork: true`, a
  hostPath mount of `/dev/infiniband`, and the `IPC_LOCK` + `SYS_RESOURCE` capabilities — but **not**
  `privileged`. The rendered member Pod sets exactly those three and nothing more, and a member group
  whose resolved protocol is `tcp` sets **none** of them.
- **The transfer engine picks its data ports at random, and the peer-to-peer plane makes a
  port range unavoidable rather than merely prudent.** One observed run under `P2PHANDSHAKE` bound
  `127.0.0.1:15002` (P2P RPC) and `15995` (TCP transport), and a second client `16566` / `16655` —
  **none of them configured**. The metadata plane this scope ships is the one that binds those ports,
  so there is no configuration under which a fixed list would become correct.
  - **Acceptance:** the rendered member Pod declares **no fixed data-plane `containerPort`**, because
    a fixed list would be a false statement about which ports the process uses.
  - **Acceptance:** the documentation states the reachability requirement as a **port range** between
    member nodes and from engine clients, never as a list of fixed ports, and says the same for any
    NetworkPolicy or firewall rule an operator writes. A `hostNetwork` port reservation is likewise a
    range.
- **Acceptance:** the rendered member container runs `mc_store_rest_server`, carries the `MOONCAKE_*`
  variables of the table above plus `extraArgs` as `-D <key>=<value>`, and declares **no ConfigMap, no
  volume and no init container**. `MOONCAKE_TE_META_DATA_SERVER` is asserted byte-for-byte.

#### F10 — Segment lifecycle: growth without a restart, and a shrink that says what it costs

The master's segment verbs are `MountSegment`, `UnmountSegment`, `ReMountSegment` and
`GracefulUnmountSegment`, the last taking a `grace_period_ms`. A newly mounted segment **participates
in subsequent allocation immediately — no master restart and no client restart is required.**

- **Acceptance:** widening `members[].nodeSelector` to cover a third node adds a member and raises
  `status.capacity.total`, with **no restart** of the master and no restart of the members already
  running — asserted on the observed Pod UIDs and restart counts of both before and after, and on the
  master's `master_total_capacity_bytes` moving.

⛔ **That acceptance does not hold under a DaemonSet's default update strategy, and the reason is
worth stating because it is not specific to this API.** A DaemonSet's `status` separates two
independent axes: placement (`DesiredNumberScheduled` and friends — "one copy on every node that
matches **the template's node selector**") and revision (`UpdatedNumberScheduled` — whether a Pod's
`controller-revision-hash` matches the current one). `nodeSelector` belongs to BOTH: it decides
placement, and it lives in `spec.template`, so changing it also invalidates every running Pod's
revision hash. Under `RollingUpdate` a widening therefore adds the new member **and rolls every
existing one**, unmounting and re-mounting each segment.

This is not a DaemonSet defect. Deployment, StatefulSet and DaemonSet all put node selection inside
the pod template, and the template is what triggers a roll — so **none of the three can change where
a workload runs without restarting what already runs.** StatefulSet's `partition` freezes by
ORDINAL, which has no meaning when the identity is a node.

⇒ **The member DaemonSet uses `updateStrategy: OnDelete`, and this operator decides when to
restart.** It fingerprints everything in the pod template EXCEPT the node selector and carries that
fingerprint on the template as an annotation, so every Pod inherits it. A pass then deletes only the
Pods whose fingerprint has moved. The result is a more precise update policy than the built-in one:
it restarts on **what changed** rather than on **that the template changed**.
  - A widening changes only the node selector, so no fingerprint moves and nothing is deleted; the
    DaemonSet's placement axis adds the new node's Pod on its own.
  - An image, argv, environment, resource or fabric change moves the fingerprint, and every member is
    deleted so the DaemonSet recreates it from the new template. `OnDelete` alone would have left
    that change written but never applied, which is the silent state this avoids.
- **Existing objects are not rebalanced.** How fast the cluster converges after a growth depends on
  `allocationStrategy` — `FreeRatioFirst` biases new writes toward the emptier member, `Random` does
  not. This is documented on the architecture page, not left to be discovered.
- ⛔ **Shrinking a member group discards that member's cache.** The unmount is not graceful and this
  scope does not make it so, because nothing reachable from a Pod's shutdown can make it graceful:
  - `mc_store_rest_server` handles `SIGTERM` and calls `store.close()`, which reaches `tearDownAll()`
    — a path that takes **no grace period at all**.
  - The Python binding that does take one, `unmount_segment(segment_ids, grace_period_seconds=0)`,
    defaults to zero, and a zero goes straight to the immediate unmount rather than to
    `GracefulUnmountSegment`.
  - No shipped console script can issue that RPC. **A `preStop` is blocked too, but not for the
    reason this spec originally gave.** The original reason — that a fresh client would not know the
    segment id the running process holds — is **wrong, and is corrected here rather than deleted
    because it was load-bearing**: a `preStop` hook runs against the *same* process that mounted the
    segments, so client identity was never the obstacle. The real obstacle is upstream and narrower:
    `segment_ids` is required, **no route returns a client its own ids**, and the name is not
    derivable because the leader appends a fresh port on every start. One upstream route — a way to
    read back your own segment ids — is the whole of what unblocks this, which is what a later
    attempt should test rather than re-testing the client-identity claim.
  - What the master DOES offer is `POST /api/v1/drain_jobs`, which **migrates** a segment's data to
    named target segments rather than unmounting it. That is a different and stronger operation than
    a graceful unmount, it needs the remaining members to have room, and it is a stateful
    orchestration — create, poll, then scale. It is a follow-on, not this scope.
  - So `terminationGracePeriodSeconds` is set to let the entrypoint finish its own shutdown and
    deregister cleanly, and the **documentation states plainly that shrinking a group drops the
    cache that member held**. For a cache that is a cost, not a fault: the data is recomputable, and
    saying so is better than implying a drain that does not happen.
- **`-client_ttl` is 10s**: a client that stops heartbeating for that long is treated as disconnected
  and its segments may be unmounted. So a member Pod that restarts must **re-mount** rather than
  assume its segment survived — `mc_store_rest_server` constructs the store and mounts a segment on
  every start, so the re-mount is the entrypoint's own doing. The reconciler needs no rule for this
  case: it publishes the master's listing, so a segment that has gone and not yet come back is simply
  not in it, and one that has come back is.

#### F11 — Referential integrity: `status.usedBy` and the finalizer

- `status.usedBy` is a list of `core.TypedLocalObjectReference` — `apiGroup`, `kind`, `name` — written
  by the objects that consume the backend (the pool objects a later spec adds). This scope defines the
  field, keeps it in status, and **enforces** it.
- **It is deliberately not `core.ObjectReference`.** Five of that type's seven fields mean nothing for
  a cluster-scoped claimant, every field on it is optional — so a wholly empty entry would validate
  against the one field a finalizer refuses deletion on — and upstream's own comment on it tells new
  APIs not to embed an underspecified type they do not control. `TypedLocalObjectReference` carries no
  such warning, is already this group's idiom for a reference (`core.LocalObjectReference` appears six
  times in `instance.go`), and `apiGroup` is what keeps a bare `kind` unambiguous. "Local" means only
  that there is no namespace field, which is correct: a backend is cluster-scoped and so is everything
  that claims one.
- The reconciler locks a live `KVCacheBackend` with the repo's own finalizer
  (`systemmeta.Lock`/`Unlock`) and, on deletion, refuses to release the lock while `usedBy` is
  non-empty.
- **Acceptance:** `kubectl delete kvcb <name>` on a backend whose `status.usedBy` names one consumer
  leaves the object present with `phase: Deleting`, a `Deletable=False` condition, and a message
  naming the consumer's kind and name. Clearing the last reference lets the teardown complete: the
  master Deployment, its Service and the member DaemonSet are deleted, then the finalizer is removed.
- **The finalizer waits for the workloads to be gone, not for the deletes to be accepted.** A delete
  call returns as soon as the API server records the intent, so deletion is requested with foreground
  propagation and the lock is held until the objects are actually absent. Releasing on the acceptance
  would let the object disappear while the leader is still serving and the members still hold their
  memory — the exact claim the ordering exists to make true.
- **Nothing is deleted on a derived name alone.** Every object is read first and deleted only if it
  carries this backend's own resource note. The names here are derived, so an unrelated object can
  already hold one — and the align path cannot be relied on to have adopted it, because it never
  writes the immutable `spec.selector`: a same-name workload whose selector differs has its every
  update rejected and never acquires the note.
  - The member sweep lists on the **resource-type label**, not on the backend's identity labels, so
    that what finds the objects and what proves they are ours are the same mechanism. Converging the
    identity labels is not enough by itself: a delete runs the teardown branch and never reaches the
    aligner, so one dropped beforehand is never put back.
- Teardown is idempotent and level-based: a partially-deleted backend converges on repeat.
- The rendered objects are namespaced dependents of a cluster-scoped owner, which garbage-collects
  correctly; the direction that does not work — a cluster-scoped dependent under a namespaced owner —
  is not used here. The teardown deletes them **explicitly** all the same, and the ordering above is
  why: between the finalizer coming off and the collector running, the leader is still serving on an
  address no object accounts for and the members still hold the node memory they claimed. Ownership
  is the safety net, not the mechanism.

#### F12 — Documentation

A new page under `docs/architecture/` states: the two axes and why they are two; the cluster-scoped
argument and the Kueue precedent; the measured master-variant and client-layout tables, why
`spec.image` is explicit, and the split that **the master image needs no accelerator runtime while a
member image needs the runtime of the transport it uses**; that the metadata plane and the master's HA
backend store are different axes, that the plane is peer-to-peer and that a single-leader backend
therefore has **no external dependencies**, and that a manifest trying to configure that plane is
refused by the SCHEMA or silently pruned rather than refused by a webhook naming a follow-on (F4); the
member's environment-variable contract with `MOONCAKE_TE_META_DATA_SERVER` spelled out as a no-entry
item; the `/health` four-field view, that its `status` is a constant and the two probes therefore take
different paths; that capacity is observed and never summed, and is absent rather than zero while the
master is still starting; the port-range
requirement and why the peer-to-peer plane makes it unavoidable; that growth does not rebalance and
does not restart existing members; that **shrinking discards that member's cache**; that
`replica_num` is engine-side; and the benign startup line below. `docs/README.md`'s index gains the
page.

**One benign-looking startup ERROR, documented so nobody files it as a bug:**

```
E transfer_metadata.cpp:991] Local segment descriptor not found
```

It appears on every client start and is harmless.

Client-side environment knobs observed at startup, recorded on the same page:

```
MC_TE_METRIC=1                        enable transfer-engine metrics (OFF by default)
MC_STORE_CLIENT_METRIC_BANDWIDTH      client bandwidth summary
MC_STORE_MEMCPY                       unset => auto-detected ("TCP-only environment, memcpy enabled")
MC_METADATA_SERVER / P2PHANDSHAKE     the transfer engine's own low-level metadata knob
```

`MC_METADATA_SERVER` is **not** the variable the reconciler renders: the member is configured through
the store client's own key, `MOONCAKE_TE_META_DATA_SERVER` (F9). The page says so, because two names
for the metadata plane is exactly the kind of near-miss that gets one of them typed into a template.

### Verification

**Hardware: a local Kubernetes cluster is sufficient — two nodes to start and a third to grow onto.
No GPU, no RDMA, no cloud — and no etcd.** The capability experiment behind F2 and F4 ran entirely without GPU compute — and the fact
that one host had neither CUDA nor CANN is precisely what surfaced both the master's link-time CUDA
dependency and the accelerator-free variants' flag compatibility. The metadata plane is
peer-to-peer, which needs nothing deployed, so the run stands up no supporting service. The
acceptance path is TCP-only, so `transport.protocol: Auto` resolves to `TCP` and the RDMA security
context is never rendered.

The seven acceptance items, in the order they are run. **Item 7 is the one the other six cannot
substitute for**: everything above it asks the system about itself, and only it asks whether the
thing the system exists to do actually happens.

1. A `KVCacheBackend` with one DRAM member group and `master.replicas: 1` reaches
   `status.phase: Ready`, and both `status.endpoints` entries are reachable.
2. `status.capacity.total` equals the sum of `capacityPerMember` across mounted members **and is read
   from `master_total_capacity_bytes`** — the equality is the assertion, the metric is the source.
3. Growing `members[].nodeSelector` to cover a third node adds a member and raises
   `status.capacity.total` without restarting the master or the existing members.
4. Deleting the object while `status.usedBy` is non-empty is refused, with a message naming the
   consumer.
5. No metadata-plane or HA field exists to set wrongly and the rendered leader argv carries no
   metadata flag, so a manifest that tries to name one is refused by the SCHEMA or pruned — never
   refused by a webhook naming a follow-on, because there is no field for a webhook to see (F4). The
   whole run completes with **no etcd anywhere in the cluster**.
6. **Every capability assertion is judged on artifact behaviour — never on a flag being accepted, a
   log line echoing it back, or a build exiting 0.**
7. ⛔ **A byte moves through the data plane.** An independent client — a separate Pod, in its own
   network namespace, mounting **no segment of its own** (`global_segment_size: 0`, so anything it
   writes must land in a member's segment and travel there) — connects to the master, `put`s a
   payload and `get`s it back with a matching digest, and the member's `allocator_used_bytes` rises
   by exactly what was written.

   This item exists because its absence hid a complete failure. Items 1–6 all passed against a
   backend whose data plane was **unreachable from anywhere a client would dial** (F9): they check
   `Ready`, the master's own control-plane endpoint, the master's own counters, growth, deletion
   refusal, and schema pruning — every one of them a question the system answers about itself.

#### The recorded run

Run on a three-node k3s v1.34.9 cluster — one server, two agents, no RDMA on any of them
(`/sys/class/infiniband` empty), no cloud, and nothing deployed for the metadata plane. Two of the
three hosts carry consumer GPUs, and the run is still a no-accelerator one **in the sense that
matters**: k3s only gives a Pod the vendor runtime when it names a `runtimeClassName`, and nothing
here does. That was checked directly rather than reasoned about — inside a plain container on one of
those hosts there are no `/dev/nvidia*` nodes and no `nvidia-smi`, and in that container
`mooncake_master --help` exits 0 and `import mooncake.engine, mooncake.store` succeeds.

Nothing had to be built to run it. `docker.io/kvcacheai/mooncake:0.3.13` is published for both
architectures and carries `mooncake_master` and `mc_store_rest_server` in one image, which is what
lets a single `spec.image` serve the leader and the members (F2).

| # | Result |
|---|---|
| 1 | `Ready` **20 seconds** after apply, `phaseMessage` empty, all four conditions `True`; the leader Deployment 1/1 and the member DaemonSet 2/2 |
| 2 | `capacity.total: 8Gi` against `capacityPerMember: 4Gi` × 2 mounted members, and `12Gi` after the third joined — read from `master_total_capacity_bytes`, which the leader serves as `0` for the file pair throughout |
| 3 | A third node added: the DaemonSet went 2/2 → 3/3 and **every pre-existing Pod kept its UID and its `restartCount: 0`**, leader included, compared line-by-line against a baseline taken before the apply |
| 4 | A delete with `usedBy` set left the object present at `phase: Deleting`, message `in use by KVCachePool/team-a-pool`, finalizer held and both workloads alive; clearing the claim let it complete and the Deployment, Service and DaemonSet were all garbage-collected |
| 5 | `spec.metadata.mode: etcd` — refused by the SCHEMA for a strict client and **silently pruned** for a non-strict one (F4). No etcd was deployed and none was contacted |
| 6 | Every row above is an observed effect: a served body, a moved counter, a Pod UID, a refused apply |

Three things the run established that no fixture had:

- ⛔ **It found a real defect.** `status.members[]` published an empty `nodeName` and `medium` on every
  member. The leader reports a segment's `te_endpoint` as `<node>:<port>`, because this renderer sets
  `MOONCAKE_LOCAL_HOSTNAME` from `spec.nodeName` — while the join indexed member Pods by address. The
  fixtures had guessed addresses, so they agreed with the bug. The join now indexes both, and the
  fixtures carry the recorded shape. **The failure was silent**: an unjoined member looks exactly like
  the legitimate "no Pod matched" case this API deliberately leaves blank, so nothing short of running
  it would have shown it.
  - That run fixed the JOIN and left the address alone. A later one established that the address was
    itself the defect — the node name is not reachable — and moved it to `status.podIP` (F9). The
    join still indexes both, which is what keeps an external backend reporting a node name joinable.
- The `/health` and `/get_segments_detail` bodies a real leader serves are **byte-identical** to the
  recorded fixtures, so the decoders were right about everything else.
- ⛔ **`service_ready: false` is not observable in this scope.** A single leader reports
  `service_ready: true` from its first answer, because the non-HA path calls `SetServiceAvailable(true)`
  unconditionally three lines after the admin server starts. The gate stays — it is what HA needs, and
  a starting leader that answered `/metrics` with zeroes would still be misread without it — but the
  spec should not be read as describing a state this scope will show anyone.

Item 6 is not a style preference. In the same project one undeclared build switch fails loudly
(`TENT backend is not enabled. Please rebuild with -DUSE_TENT=ON`) while another fails **silently**:
`--enable_kv_events=true` is accepted, the master logs `enable_kv_events=1`, and yet
`/kv_events/status` reports `{"enabled":false}` and no socket is ever bound. **One switch's failure
mode cannot be inferred from another's.** So every claim this spec makes about the artifact is tied to
an observed effect — a served endpoint, a moved counter, a bound port, a refused config — and the
end-to-end run records the observation next to the claim.

### Notes / Constraints / Caveats

- **The CRD manifest is generated Go, not a chart file.** `api/worker/v1alpha1/zz_generated.crds.go`
  carries every CRD in the group, `GetCustomResourceDefinitions()` returns them, and
  `pkg/worker/apis/setup.go` installs and re-ensures them at worker startup. A new type therefore
  needs **no chart manifest**. The worker's ServiceAccount is bound to `cluster-admin`
  (`deploy/gpustack-operator/chart/templates/worker/serviceaccount.yaml`), so it needs **no chart RBAC
  change** either. The only chart-visible consequence is that `helm install` must end with the new
  CRD present, which the end-to-end run asserts.
- **Status shape follows the group's convention.** The types in `api/worker/v1alpha1` carry a flat
  `Phase` + `PhaseMessage` pair, where phase is the summary of conditions. This spec keeps that pair
  and adds `Conditions`, reusing the existing `api/v1.Condition` type rather than declaring a new one.
- **Defaults live in the CRD schema, so there is no mutating webhook.** `+k8s:validation:default=`
  covers `replicas: 1`, the peer-to-peer plane, `allocationStrategy: FreeRatioFirst` and
  `protocol: Auto`; nothing needs a cross-object read at admission. One validating webhook is the
  whole admission surface.
- **A single-master backend has zero external dependencies**, measured: the master runs with
  `enable_ha=0` and never contacts a metadata service, and clients set up with
  `metadata_server=P2PHANDSHAKE` complete put/get against it. Nothing but `spec.image` and the nodes
  is required — no etcd, no Redis, no lease object.
- **The master is configured by argv and the member by environment variables**, because that is the
  form each binary accepts: the master is a gflags binary whose `-fromenv` still needs the flag-name
  list in argv, while every member config key has a real named `MOONCAKE_*` variable. The member
  therefore needs no ConfigMap, no volume and no init container.
- **File naming.** `kv_cache_backend.go` in each of the three homes — snake_case per repo convention,
  never `kvcachebackend.go`.
- `-rpc_interface` takes an interface **name** and its IPv4 overrides `-rpc_address` when set; the two
  are therefore mutually exclusive in the renderer, and setting both through `extraArgs` is rejected.
- `-metrics_port` (9003) serves both the Prometheus exposition **and** the HTTP admin API. One port,
  two surfaces; the Service exposes it once.
- The master is not a data-plane process, so its Pod is **not** `hostNetwork` and needs no device
  mounts; only members do. A hostNetwork member reaches the master's ClusterIP normally.
- **`replica_num` is not controllable from the CR** — it is a per-`Put` argument the caller supplies,
  so redundancy is set by the workload's connector configuration, engine-side.
- **`/api/v1/drain_jobs` and its siblings answer 404 on GET**, so they are not GET endpoints. Nothing
  in this scope calls them.
- Reconciliation is level-based and idempotent throughout: every rendered object is converged with the
  repo's `kubeclientset` create-or-update helpers under a DeepEqual guard, and a status write happens
  only when the computed status differs from the stored one.
- External references, which are the only claims in this document a reader can verify independently:
  - Mooncake repository — <https://github.com/kvcache-ai/Mooncake>
  - Mooncake Store design — <https://kvcache-ai.github.io/Mooncake/design/mooncake-store.html>
  - Multi-tenancy deployment (quota connectors, admin API) —
    <https://kvcache-ai.github.io/Mooncake/deployment/multi-tenancy.html>
  - Transfer Engine MPComm transport design —
    <https://kvcache-ai.github.io/Mooncake/design/transfer-engine/mpcomm_transport.html>
  - PyPI package (wheel layout, per-arch availability) —
    <https://pypi.org/project/mooncake-transfer-engine/>
  - Kueue `ClusterQueue` / `LocalQueue` split, the precedent for the cluster-versus-namespace scoping
    argument — <https://kueue.sigs.k8s.io/docs/concepts/>

### Boundaries

- **Always:** keep `spec.type` and `members[].medium` as two axes; keep the metadata plane and the
  master's HA backend store as two axes; publish observed capacity and never a spec sum; refuse a
  value the artifact refuses, at admission, with the measured refusal in the message; leave a figure
  **absent** rather than publish zero or a stale one; keep every rendered object convergeable from the
  spec alone; render each side in the form its own binary accepts; state a port requirement as a
  range.
- **Ask first:** adding a first-class field for any offload or promotion knob (it moves scope toward
  the tiering spec); exposing `-quota_bytes` under any name (it collides with the quota spec's
  vocabulary); deploying an etcd on the operator's behalf; anything that raises the member Pod's
  privileges past `hostNetwork` +
  `/dev/infiniband` + `IPC_LOCK` + `SYS_RESOURCE`; changing the master Deployment's replica ceiling.
- **Never:** derive `spec.image` from the operator image or any default; require an etcd for a
  single-leader backend; offer a metadata-plane or HA option that no measured artifact serves; spell `MOONCAKE_TE_META_DATA_SERVER` without the underscore inside `META_DATA`;
  claim a capability from a flag being accepted, a parser taking a string, or a log line echoing it;
  declare a fixed data-plane `containerPort`; delete a backend whose `status.usedBy` is non-empty;
  render `privileged: true`; write a tenant field on this object.

### Risks and Mitigations

- **The image's master variant carries an unresolvable accelerator dependency** → `spec.image` is
  explicit and required, the measured variant table is in the docs with the accelerator-free
  recommendation, and a master that cannot start is reported as `Error` with the container's own
  loader message in `phaseMessage` rather than as a generic timeout.
- **An operator stands up an etcd this backend never contacts** → the plane is peer-to-peer,
  measured to work with zero external dependencies, and `etcd` is refused at admission with the
  follow-on named, so the message teaches rather than merely denies.
- **The member image lacks the runtime of the transport it was configured with** → the docs carry the
  split in one sentence (the master needs no accelerator runtime, a member needs the transport's), and
  the failure surfaces as the member container's own loader message in `phaseMessage` — `error while
  loading shared libraries: libascendcl.so` for the Ascend case — rather than as a generic timeout.
- **`MOONCAKE_TE_META_DATA_SERVER` is normalised to `MOONCAKE_TE_METADATA_SERVER`** → the wrong
  spelling does not error, it silently degrades the metadata plane, so the name is a stated no-entry
  item and is asserted byte-for-byte by a unit test rather than left to review.
- **The transfer engine's random ports break a NetworkPolicy or firewall written from the rendered
  Pod spec** → no fixed data-plane port is declared, and the docs state the requirement as a range;
  the peer-to-peer plane is what makes the range unavoidable.
- **A secret-bearing flag in argv is world-readable in the Pod spec** → this scope renders none, since
  it renders no metadata store to connect to; the risk arrives with `extraArgs` and with the
  server-backed modes, and is recorded as an Open Question.
- **A failed scrape publishes a zero capacity that reads as "the cache is empty"** → capacity is
  absent on a failed scrape with `CapacityObserved=False`, never zero and never stale.
- **A single master replica is a single point of failure** → stated, not hidden: `replicas` accepts
  only `1` here and the master-HA spec owns the rest. `phase: Error` names the master when it is down,
  so the blast radius is legible.
- **Growth does not rebalance, so a freshly added member stays cold** → documented against
  `allocationStrategy`, and `status.capacity` shows total and used per backend so the imbalance is
  visible.
- **A member Pod restart loses its segment because `-client_ttl` lapsed** → the member re-mounts on
  startup, and `status.members[]` is the master's own listing, so a segment that is gone is absent
  from it and `Ready` is never reported over one.
- **An operator patches a rendered object and loses the edit on the next reconcile** → the
  `extraArgs` passthrough exists for exactly that, and the collision rule keeps the rendered argv
  unambiguous.
- **`state` could be inferred from the Pods and then read as the master's own view** → it is not
  inferred at all. `/get_segments_detail` is the master's listing and `status.members[]` passes it
  through, so the field means what its name says. A Pod-side inference would have been indistinguishable
  in the API and wrong exactly when it mattered — during a drain.
- **A capability is claimed because a flag was accepted** → the verification discipline is a stated
  acceptance criterion with the `enable_kv_events` counter-example attached, and every task's
  `Verify:` names an observed effect.

## Design Details

### Commands

Build, lint and test run locally on darwin; the whole module builds there.

```bash
go build ./api/... ./pkg/...
make lint                     # golangci-lint over the whole module; cold runs are slow
make test                     # go test -v -failfast -race -cover -timeout=30m ./...
go test ./api/worker/v1alpha1/... ./pkg/worker/webhooks/worker/... \
        ./pkg/worker/controllers/worker/... ./pkg/worker/kvcache/...
make lint docs
make lint chart
```

**Code generation runs from a module-suffixed physical path**, not from an arbitrary worktree:
`make generate` derives package paths GOPATH-style and requires a working directory ending in
`gpustack.ai/gpustack`. Run it in a checkout that satisfies that and sync the delta back; when
syncing with `rsync`, use `--filter='P .git'` and **not** `--exclude '.git/'` — a worktree's `.git` is
a *file*, which the latter misses, and combined with `--delete` it destroys the receiver's repository.

```bash
make generate                 # T1, T2 — deepcopy, register, CRD, protobuf, openapi, webhooks, clients
```

`make test chart` installs into the **current** kube context and is not part of this spec's loop; the
chart check here is `make lint chart` plus the end-to-end install in T12.

### Project Structure

```
api/worker/v1alpha1/
  kv_cache_backend.go                  # KVCacheBackend, its spec/status, enums, validation markers
  zz_generated.{crds,deepcopy,register,model_name}.go
  generated.{proto,pb.go,protomessage.pb.go}
api/worker/zz_generated.openapi.go
pkg/kubeclients/**                     # generated typed client, listers, informers, applyconfiguration
pkg/worker/webhooks/worker/
  kv_cache_backend.go                  # the validating webhook: every measured refusal
  zz_generated.webhooks.go             # regenerated registration
pkg/worker/webhooks/setup.go           # + new(worker.KVCacheBackendWebhook)
pkg/worker/controllers/worker/
  kv_cache_backend.go                  # the reconciler: render, observe, finalize
pkg/worker/controllers/setup.go        # + new(worker.KVCacheBackendReconciler)
pkg/worker/kvcache/
  keys.go                              # the derived flag / config-key sets, shared with admission
  leader_flags.go                      # pure spec -> argv renderer
  leader_workload.go                   # Deployment + Service
  member_workload.go                   # DaemonSet, security context, requests
  admin.go                             # /health decode + /metrics parse over an http.Client
docs/kv-cache/backend.md  # the new page
docs/README.md                         # index entry
```

`pkg/worker/kvcache/` is a new package because the renderers and the admin client are pure functions
over recorded inputs: keeping them out of the reconciler is what lets the whole flag surface and the
whole status derivation be tested without a cluster.

### Code Style

The API type, following the group's discipline — a doc comment states behaviour and the reason for it
rather than restating the field name; every enum is a measured enum, spelled CamelCase or all-caps as
every other enum in this group is, with the mapping onto the artifact's own snake_case living in the
renderer; an observed figure is a pointer that stays absent when nothing observed it. The full type is
in `api/worker/v1alpha1/kv_cache_backend.go`; the load-bearing shapes are below.

```go
// KVCacheBackendSpec defines the desired spec of KVCacheBackend.
type KVCacheBackendSpec struct {
	// Type is the backend IMPLEMENTATION — who does placement, eviction, replication and
	// metadata. It is NOT the medium: where the bytes live is members[].medium, and collapsing
	// the two is the category error this field exists to make impossible.
	//
	// One value ships. It is spelled out rather than assumed so the object says what it is, and
	// so a second implementation widens an enum instead of reinterpreting an absent field.
	//
	// +k8s:validation:default="Mooncake"
	// +k8s:validation:enum=["Mooncake"]
	Type string `json:"type,omitempty" protobuf:"bytes,1,opt,name=type"`

	// Image is the container image every role of this backend runs.
	//
	// It is OPTIONAL. Left unset, the reconciler uses the cluster-wide default pinned in the
	// "kv-cache-backend-image" Setting, which is where a version this project has verified
	// belongs — an admin pins it once instead of restating it on every object. Set here, it
	// overrides that default for this backend alone.
	//
	// It is never DERIVED from the operator's own image the way the Device Manager's is: the
	// master and the engine client can be builds against different accelerator generations —
	// the base wheel's master links CUDA 12 while a current vLLM image carries CUDA 13 — so a
	// derived image would silently pair a master with a runtime it cannot load. Unset here AND
	// unset in the Setting is refused at admission, naming both places.
	//
	// +k8s:validation:maxLength=512
	Image string `json:"image,omitempty" protobuf:"bytes,2,opt,name=image"`

	// Connection describes how this backend is reached: managed by this operator, or external.
	// Exactly one is set, enforced by the webhook.
	//
	// +required
	Connection KVCacheBackendConnection `json:"connection" protobuf:"bytes,3,name=connection"`

	// Transport describes the data plane the members use.
	Transport KVCacheBackendTransport `json:"transport,omitempty" protobuf:"bytes,4,opt,name=transport"`
}

// KVCacheBackendConnection is how the backend is reached. Exactly one branch is set; the webhook
// refuses both and neither, because a spec with no branch describes nothing and a spec with two
// describes two different backends.
type KVCacheBackendConnection struct {
	// Managed asks this operator to run the leader and the store members.
	Managed *KVCacheBackendManaged `json:"managed,omitempty" protobuf:"bytes,1,opt,name=managed"`

	// External names a backend somebody else runs. Nothing is rendered for it; the reconciler
	// only observes.
	External *KVCacheBackendExternal `json:"external,omitempty" protobuf:"bytes,2,opt,name=external"`
}

// KVCacheBackendExternal is a backend this operator does not run.
type KVCacheBackendExternal struct {
	// Endpoints are the addresses of a backend somebody else runs, one entry per named role.
	// Both roles are required here, and for the same reason they are two entries and not one
	// string: this operator reads the Admin address and publishes the Client address, so an
	// external backend that named only one leaves either the scrape or every engine with
	// nothing to point at.
	//
	// It is a list rather than a single address so that a multi-leader backend needs no API
	// change to describe.
	//
	// +required
	// +k8s:validation:minItems=1
	// +listType=map
	// +listMapKey=name
	Endpoints []KVCacheBackendEndpoint `json:"endpoints" protobuf:"bytes,1,rep,name=endpoints"`
}

// KVCacheBackendEndpoint is one named address of a backend. The same type serves the external
// branch's input and the status's output, so a reader learns one shape.
type KVCacheBackendEndpoint struct {
	// Address is host:port.
	//
	// +required
	// +k8s:validation:maxLength=253
	Address string `json:"address" protobuf:"bytes,1,name=address"`

	// Name says who the address is for, and the two readers want different things. Client is
	// what an inference engine connects to. Admin is the port serving the Prometheus exposition
	// and the HTTP admin API both, which is what THIS OPERATOR reads. A consumer handed the
	// wrong one fails at connect time with nothing to point at, which is why the distinction is
	// carried in the API rather than left to a convention.
	//
	// +required
	// +k8s:validation:enum=["Client","Admin"]
	Name string `json:"name" protobuf:"bytes,2,name=name"`
}

// KVCacheBackendLeader is the leader process: how many of it, how it places new writes, and the
// escape hatch for flags this API does not enumerate.
type KVCacheBackendLeader struct {
	// Replicas is how many leader processes run. One, and only one, in this scope: electing a
	// leader among several needs a backend store this scope does not enter, and the webhook
	// refuses anything else while naming that follow-on rather than silently running one anyway.
	//
	// +k8s:validation:default=1
	// +k8s:validation:minimum=1
	Replicas *int32 `json:"replicas,omitempty" protobuf:"varint,1,opt,name=replicas"`

	// AllocationStrategy is how the leader picks which member takes a new write. Random spreads
	// them; FreeRatioFirst biases toward the emptier member.
	//
	// The enum is deliberately the two that any pooled store would have, rather than every value
	// the current artifact's flag accepts: the others it accepts are specific to one medium or
	// one locality model, are reachable through ExtraArgs for anyone who needs them, and would
	// otherwise fix this API to one implementation's vocabulary. Widening the enum later is not
	// a breaking change.
	//
	// +k8s:validation:default="FreeRatioFirst"
	// +k8s:validation:enum=["Random","FreeRatioFirst"]
	AllocationStrategy string `json:"allocationStrategy,omitempty" protobuf:"bytes,2,opt,name=allocationStrategy"`

	// ExtraArgs passes flags this API does not enumerate straight through to the leader, after
	// the derived ones. A key that collides with a flag rendered from a field above is refused
	// at admission, because two sources for one flag make the rendered command ambiguous.
	ExtraArgs map[string]string `json:"extraArgs,omitempty" protobuf:"bytes,3,rep,name=extraArgs"`
}

// KVCacheBackendTransport is the data plane the members use.
type KVCacheBackendTransport struct {
	// Protocol is the transport the members use. Auto resolves to TCP, and
	// status.members[].protocol reports what the leader says each member actually came up on, so
	// the outcome is observed rather than assumed from this field.
	//
	// Auto is deliberately NOT a per-node probe that promotes itself to a faster fabric, for two
	// reasons. A member group renders one DaemonSet, so a single Pod template covers every node the
	// group selects and cannot carry a different transport per node. And promoting to RDMA means
	// granting hostNetwork and two capabilities: a privilege is requested, never inferred on an
	// operator's behalf.
	//
	// TCP is the universal fallback. RDMA, HIP and Ascend are peers of one another — each is a
	// fabric- or vendor-specific fast path, not a spelling of TCP: the ROCm build compiles a HIP
	// transport in, and the NPU build ships a separate Ascend transport library linking the CANN
	// runtime.
	//
	// The bar for membership here is "measured as compiled into a published artifact", which is
	// what excludes the other ten strings that artifact's config parser accepts. It is NOT
	// "measured to move bytes": only TCP has been exercised end to end, and RDMA, HIP and Ascend
	// each await a run on that hardware. A member also needs the runtime its transport links —
	// Ascend needs CANN in the member image — and the webhook cannot see inside an image, so
	// that pairing is the operator's to get right.
	//
	// +k8s:validation:default="Auto"
	// +k8s:validation:enum=["Auto","TCP","RDMA","HIP","Ascend"]
	Protocol string `json:"protocol,omitempty" protobuf:"bytes,1,opt,name=protocol"`
}

// KVCacheBackendMember is one group of store members: the nodes it selects, the medium each
// contributes, and how much.
type KVCacheBackendMember struct {
	// NodeSelector selects the nodes that contribute this medium. One member runs per selected
	// node; widening the selector adds members and the leader admits their segments into
	// subsequent allocation immediately, with no leader or member restart.
	//
	// +required
	NodeSelector map[string]string `json:"nodeSelector" protobuf:"bytes,1,rep,name=nodeSelector"`

	// Medium is what this member group physically contributes. DFS covers NFS and 3FS, which
	// are media rather than backend implementations.
	//
	// +required
	// +k8s:validation:enum=["DRAM","LocalDisk","NoF","CXL","DFS"]
	Medium string `json:"medium" protobuf:"bytes,2,name=medium"`

	// CapacityPerMember sizes ONE member, not one node: a node can eventually run several
	// members, one per NUMA domain. It becomes the member's global segment size and is counted
	// into the member Pod's own resource request, so a member that does not fit stays Pending
	// instead of overcommitting the node.
	//
	// +required
	CapacityPerMember resource.Quantity `json:"capacityPerMember" protobuf:"bytes,3,name=capacityPerMember"`

	// LocalBufferSize is the member client's local staging buffer, counted into the Pod's
	// memory request beside CapacityPerMember.
	LocalBufferSize resource.Quantity `json:"localBufferSize,omitempty" protobuf:"bytes,4,opt,name=localBufferSize"`

	// ExtraArgs passes config keys this API does not enumerate straight through to the member. It
	// is keyed by CONFIG KEY rather than by environment-variable name — one namespace per side,
	// each the one its own binary documents. A key that collides with one derived from a field
	// above is refused at admission.
	ExtraArgs map[string]string `json:"extraArgs,omitempty" protobuf:"bytes,5,rep,name=extraArgs"`

	// Image overrides the backend's Image for this member group only. Left unset, the group runs
	// the backend's Image.
	//
	// A group's NodeSelector is what makes this necessary: two groups can select nodes of different
	// accelerator vendors or generations, and the store's client ships as one wheel per vendor —
	// CUDA 12, CUDA 13, ROCm, NPU — each carrying the transports it was compiled with and the
	// runtime it links. The transport itself is backend-wide, so this is not a per-group transport;
	// it is the per-group runtime that one transport needs on differing hardware.
	//
	// +k8s:validation:maxLength=512
	Image string `json:"image,omitempty" protobuf:"bytes,6,opt,name=image"`
}
```

Conventions: `Phase` + `PhaseMessage` is the summary, `Conditions` (reusing `api/v1.Condition`) is the
per-axis detail; an enum's Go value is CamelCase or all-caps, matching every other enum in this group,
and the mapping onto the artifact's snake_case value lives in exactly one table; an observed figure is
a pointer and an absence is never
rendered as zero.

**Enum markers go on the spec side only.** `spec` is this API's own vocabulary, so a value outside it
is an operator's mistake and the schema should say so at admission. `status` is a transcription of
what something else reports — the store's segment states, this operator's own phases — so an enum
marker there would let a store upgrade reject the **whole** status write and freeze every field on it.
`status.phase` and `status.members[].state` therefore carry their values in a doc comment and no
marker.

The one marked enum under a `status` anywhere in this group is `api/v1.Condition.Status`, and it is
marked for the opposite reason: `True` / `False` / `Unknown` is a closed set this repository defines
itself, so nothing upstream of it can add a fourth value.

### Implementation Plan

The generated artifacts land alone and first, so a regeneration failure is never entangled with
behaviour. Then admission, then the reconciler skeleton, then the two renderers, then the two
observation paths, then the modes and the run.

Checkpoints: after T1 (the type exists, generation is a no-op on a second run); after T3 (an object
can be created, locked and deleted, with no workload); after T7 (a backend comes up on a cluster);
after T10 (status is fully observed); after T12 (every acceptance item is met).

- [x] **T1 · The API type, and `make generate` green**
      Blocked by: none
      Owns: `api/worker/v1alpha1/kv_cache_backend.go`,
      `api/worker/v1alpha1/zz_generated.{crds,deepcopy,register,model_name}.go`,
      `api/worker/v1alpha1/generated.{proto,pb.go,protomessage.pb.go}`,
      `api/worker/zz_generated.openapi.go`, `pkg/kubeclients/**`
      Gate: none
      Acceptance: `KVCacheBackend`, its spec, its status and every nested type exist with enum,
      default, required and length markers as in Code Style; the type is cluster-scoped with a
      `status` subresource, category `gpustack` and short name `kvcb`; `GetCustomResourceDefinitions()`
      returns a fourth entry keyed `KVCacheBackend`; protobuf tags are contiguous from 1 with no
      reserved gaps, since nothing here has been released; **no behaviour is added in this task** —
      no controller, no webhook, no registration in either setup list.
      Verify: `make generate` from a module-suffixed physical path, then a second `make generate` with
      `git diff --exit-code` clean; `go build ./api/... ./pkg/kubeclients/...`;
      `go test ./api/worker/v1alpha1/...`.

- [x] **T2 · The validating webhook: every measured refusal, with its message**
      Blocked by: T1
      Owns: `pkg/worker/webhooks/worker/kv_cache_backend.go`,
      `pkg/worker/webhooks/worker/kv_cache_backend_test.go`,
      `pkg/worker/webhooks/worker/zz_generated.webhooks.go`, `pkg/worker/webhooks/setup.go`,
      `pkg/worker/settings/value.go`, `pkg/worker/kvcache/keys.go`
      Gate: review
      Acceptance: a `KVCacheBackendWebhook` with validating markers only (no mutating half — the
      defaults are CRD-schema defaults) refuses, each with a message an operator can act on: a medium
      name on `spec.type`; an `image` absent from both the spec and the Setting; neither or both of
      `connection.managed` / `connection.external`; `replicas` other than 1; more than one member
      group; an `external.endpoints` missing either the `Client` or the `Admin` role, or carrying two
      entries of one role; an `extraArgs` key that collides with a derived flag; `config_path` in the
      leader's `extraArgs`, which is refused for a different reason and says so — it makes the
      artifact ignore the rest of its command line; both
      `-rpc_address` and `-rpc_interface` in `extraArgs`. Update freezes `type`, `connection`'s branch
      and `members[].medium`; it permits `image`, `members[].image`, `nodeSelector`,
      `capacityPerMember`, `extraArgs` and the transport block. This task also declares the
      `kv-cache-backend-image` Setting in `pkg/worker/settings/value.go`, since the image refusal is
      the first rule that reads it, and the derived flag and config-key sets in
      `pkg/worker/kvcache/keys.go`, because admission and the renderers must agree on them and one
      list in a place neither owns is what makes "two sources for one flag" impossible.
      Every refusal is asserted by its **message**, not merely by being an error: these rules sit in a
      webhook rather than in the schema precisely because the message can say what to do, so a test
      that checks only "an error happened" would not be testing why the code is here.
      Verify: `make generate` (registration only) then `go test ./pkg/worker/webhooks/worker/...`;
      `git diff --stat` on generated files shows the webhook registration and nothing else.

- [x] **T3 · The reconciler skeleton: lock, finalize, refuse a used delete**
      Blocked by: T1
      Owns: `pkg/worker/controllers/worker/kv_cache_backend.go`,
      `pkg/worker/controllers/worker/kv_cache_backend_test.go`,
      `pkg/worker/controllers/setup.go`
      Gate: review
      Acceptance: the reconciler locks a live object with `systemmeta.Lock`, computes and writes a
      `Provisioning` status under a DeepEqual guard, and on deletion refuses to unlock while
      `status.usedBy` is non-empty — leaving `phase: Deleting`, `Deletable=False` and a message naming
      the consumer's kind and name. The refusal is a status write and **nothing else**: no requeue and
      no timer, because `usedBy` is written by the consumer's own controller onto this status and that
      write is the event that resumes the teardown. It renders no workload yet, and reports no
      condition it cannot observe — `LeaderAvailable`, `MembersMounted` and `CapacityObserved` arrive
      with the tasks that can measure them. Conditions are read and written through
      `pkg/kubeapistatus`, whose accessors are reflective and therefore work on this group's own
      condition type. Reconcile is idempotent: a second pass over settled state writes nothing,
      asserted on the object's resourceVersion rather than by counting calls, and paired with an
      assertion that the FIRST pass does write — otherwise the test would pass on a reconciler that
      does nothing at all.
      Verify: `go test ./pkg/worker/controllers/worker/ -run KVCacheBackend`.

- [x] **T4 · The leader flag renderer**
      Blocked by: T1
      Owns: `pkg/worker/kvcache/leader_flags.go`, `pkg/worker/kvcache/leader_flags_test.go`
      Acceptance: a pure function from spec to a deterministically ordered `[]string`, rendering
      `-rpc_port` (never the deprecated `-port`), `-metrics_port`, `-allocation_strategy` through the
      one camelCase↔snake_case table, `-pod_name` / `-pod_namespace` from the downward API, and
      `extraArgs` last as `-<key>=<value>`. A flag the spec does not mention is **absent**, never
      rendered at the artifact's default. No metadata flag is rendered at all — `-etcd_endpoints` is
      absent, since no metadata store exists to point at — and `-enable_ha` / `-ha_backend_type` are
      never set.
      The RPC and metrics ports are pinned as constants rather than left to the artifact's defaults:
      the Service and the published endpoints have to name a number, and a default that moved would
      move them silently. The absences are asserted by name — a test lists every flag this scope does
      not run and fails if one appears — because an addition here is a behaviour change nobody asked
      for, and a golden argv alone would only say "different".
      Verify: `go test ./pkg/worker/kvcache/mooncake/ -run LeaderFlags` — a golden argv per case, asserted
      element by element, plus a determinism case: the reconciler converges the Deployment every
      pass, so an argv whose order wandered would rewrite the object forever.

- [x] **T5 · The leader workload: Deployment, Service, probes**
      Blocked by: T3, T4
      Owns: `pkg/worker/kvcache/leader_workload.go`, `pkg/worker/kvcache/leader_workload_test.go`,
      `pkg/worker/controllers/worker/kv_cache_backend.go`
      Gate: review
      Acceptance: a one-replica Deployment in the operator's own namespace running `spec.image` where
      set and the `kv-cache-backend-image` Setting otherwise, with
      T4's argv, plus a ClusterIP Service exposing the RPC port and `metrics_port`; readiness is
      `GET /get_all_segments` on `metrics_port` — **not** `/health`, which answers 200 in every state
      and would make the probe inert — and liveness is `GET /health` on the same port with a longer
      failure threshold, which is the one question that path can answer (F6); the
      Pod is **not** `hostNetwork` and mounts no device; both objects carry the backend's owner
      reference and the repo's resource notes, and are converged with the `kubeclientset` helpers.
      `status.endpoints` carries the `Client` entry (the Service DNS name and the RPC port) and the
      `Admin` entry (the same name on `metrics_port`) — what **this operator**
      reads, since that one port serves the Prometheus exposition and the HTTP admin API both (F7).
      They are two fields because a consumer handed the admin address fails at connect time with
      nothing to point at, and the quota spec republishes both for its own two readers.
      Verify: `go test ./pkg/worker/kvcache/mooncake/ -run LeaderWorkload` and
      `go test ./pkg/worker/controllers/worker/ -run KVCacheBackend`; against a fake client, a second
      reconcile issues no update. The two probe paths
      are asserted as **different** paths, since the whole point is that one gates and the other does
      not, and a render that pointed both at `/health` would look right and probe nothing.

- [x] **T6 · The member workload: DaemonSet, requests, security context, protocol resolution**
      Blocked by: T3, T4
      Owns: `pkg/worker/kvcache/member_workload.go`, `pkg/worker/kvcache/member_workload_test.go`
      Gate: review
      Acceptance: one DaemonSet per member group over `nodeSelector`, running that group's
      `image` where set and `spec.image` otherwise, entering the image's console
      script `mc_store_rest_server` and requesting `capacityPerMember + localBufferSize` as memory for
      a memory medium and as ephemeral-storage for a local-disk medium; the whole client config
      supplied as `MOONCAKE_*` **environment variables** per F9's table — with
      `MOONCAKE_TE_META_DATA_SERVER` spelled with the underscore inside `META_DATA` and set to
      `P2PHANDSHAKE` — and **no ConfigMap, no volume and no init container**; member `extraArgs`
      rendered as `-D <key>=<value>`; `Auto` resolved to `tcp` and every other value rendered as
      itself, since the artifact has no `auto` and a Pod template cannot vary per node (F9); the
      RDMA path setting exactly `hostNetwork: true`, a `/dev/infiniband` hostPath and the `IPC_LOCK` +
      `SYS_RESOURCE` capabilities and **never** `privileged`; the TCP path setting none of them; **no
      fixed data-plane `containerPort`** on any path.
      Verify: `go test ./pkg/worker/kvcache/mooncake/ -run MemberWorkload` — one case per medium, one per
      protocol including `Auto`, one asserting `Auto` and `TCP` render an IDENTICAL Pod spec (the
      resolution is a rename, not a second code path), one asserting the rendered Pod declares no
      data-plane port, one asserting a group's `image` override wins over `spec.image` while an unset
      one falls back, and one asserting the environment byte-for-byte including the `META_DATA`
      spelling. The security context is asserted as a **whole**: that the TCP path sets none of the
      three, so a future path that granted one silently would fail here.

- [x] **T7 · The admin client: `/health` decode and `/metrics` parse**
      Blocked by: T1
      Owns: `pkg/worker/kvcache/admin.go`, `pkg/worker/kvcache/admin_test.go`,
      `pkg/worker/kvcache/testdata/**`
      Acceptance: three pure decoders over recorded payloads — the measured `/health` document
      (`status`, `role`, `ha_state`, `service_ready`, `leader_address`, `view_version`); the
      Prometheus exposition, from which only `master_total_capacity_bytes`,
      `master_total_file_capacity_bytes`, `master_allocated_bytes` and
      `master_allocated_file_size_bytes` are read — not `master_active_clients`, which has no reader
      once membership comes from the listing; and
      `/get_segments_detail`, from which the segment name, `status`, `protocol` and `te_endpoint` are
      read and the rest of the entry is ignored — the allocator byte counts are deliberately not
      carried into `status`, because nothing in this scope reads a per-member capacity and a field
      with no reader is a field that goes stale unnoticed.
      A malformed body, a missing family and a connection refusal are three **distinguishable**
      outcomes, none of which is a zero. The listing decoder — and only that one, since `/health` and
      `/metrics` are never gated — has a fourth: a **503** carrying `service plane is not active`,
      which is the master saying it is not serving yet, so it maps to a phase and not to an error.
      Verify: `go test ./pkg/worker/kvcache/mooncake/ -run Admin` over the recorded fixtures.

- [x] **T8 · `status.capacity` from the master's counters**
      Blocked by: T5, T7
      Owns: `pkg/worker/controllers/worker/kv_cache_backend.go`
      Gate: review
      Acceptance: `capacity.total` and `capacity.used` come from the scraped families, chosen by the
      group's medium; a failed or malformed scrape leaves both **absent** with
      `CapacityObserved=False` and the failure in the message, never zero and never the previous
      value; nothing sums the spec. A scrape taken while `/health` reports `service_ready: false`
      leaves them absent **too**, even though it parsed cleanly: `/metrics` answers 200 in every state
      and its capacity gauges read zero until segments mount, so a clean parse is not evidence that
      anything was observed (F7).
      Verify: `go test ./pkg/worker/controllers/worker/ -run KVCacheBackendCapacity` — including a
      case whose scrape reports a total the spec disagrees with, asserting the published value follows
      the scrape, and a case pairing a well-formed all-zero exposition with `service_ready: false`,
      asserting `capacity` is absent rather than zero.

- [x] **T9 · Segment lifecycle: growth without a restart, shrink without a drain**
      Blocked by: T6, T8
      Owns: `pkg/worker/kvcache/member_workload.go`,
      `pkg/worker/controllers/worker/kv_cache_backend.go`
      Gate: review
      Acceptance: the member DaemonSet carries `updateStrategy: OnDelete` and a pod-template
      annotation fingerprinting everything in the template **except** its node selector; the
      reconciler deletes exactly those member Pods whose fingerprint no longer matches. Widening a
      group's `nodeSelector` therefore moves no fingerprint and deletes nothing — the DaemonSet's own
      placement adds the new node's Pod — while an image, argv, environment, resource or fabric
      change moves it and every member is deleted so the DaemonSet recreates it. The rendered master
      Deployment is byte-identical across a widening either way.
      Shrinking is NOT drained: the unmount is immediate, and F10 records why nothing reachable from
      a Pod's shutdown can make it graceful. `terminationGracePeriodSeconds` is set so the entrypoint
      finishes its own shutdown rather than being cut short, and T13 states in the documentation that
      shrinking a group drops the cache that member held.
      Verify: `go test ./pkg/worker/controllers/worker/ -run KVCacheBackendScale` — a widening that
      deletes no Pod, an image change that deletes every Pod, an assertion that the fingerprint is
      **insensitive to the node selector and sensitive to each of the other fields**, and a
      before/after render diff over the master Deployment. The fingerprint case is asserted per field
      rather than once, because a fingerprint over too little is indistinguishable from a correct one
      until the field it misses is the one that changed.

- [x] **T10 · `status.phase`, conditions and `members[]` from what is observable**
      Blocked by: T7, T8, T9
      Owns: `pkg/worker/controllers/worker/kv_cache_backend.go`
      Gate: review
      Acceptance: the five phases derived from `/health` plus the workloads' own state, with
      `service_ready: false` never yielding `Ready`; the four conditions `LeaderAvailable`,
      `MembersMounted`, `CapacityObserved`, `Deletable`; `members[]` **read from
      `/get_segments_detail`**, one entry per listed segment, carrying that segment's name, its
      `status` and the `protocol` the master reports — not the protocol the renderer asked for, since
      the two disagree whenever an `Auto` resolution went a way the renderer did not predict. The
      state is passed through in this API's casing and an unrecognised one is published verbatim,
      because the field carries no enum. A running member Pod whose segment is not listed is absent
      from `members[]` and counted in `MembersMounted`'s message; a failed listing scrape leaves
      `members[]` alone and sets `MembersMounted=False`, which is the opposite of what a failed
      capacity scrape does and is justified in F6.
      Node name and medium are the two fields the listing cannot supply — they are joined in from the
      member Pod whose `te_endpoint` matches, and are left empty rather than guessed when no Pod
      matches.
      Verify: `go test ./pkg/worker/controllers/worker/ -run KVCacheBackendStatus` — a table over
      recorded `/health` × `/get_segments_detail` bodies × workload states, including one listing
      carrying a `DRAINING` segment, one carrying a state string the fixture invents, one where the
      reported protocol contradicts the rendered one, and one where the listing scrape fails.

- [x] **T11 · The external connection mode**
      Blocked by: T10
      Owns: `pkg/worker/controllers/worker/kv_cache_backend.go`
      Acceptance: with `connection.external` set, the reconciler creates **nothing** — asserted
      against a fake client that fails the test on any create — mirrors the endpoint into status,
      reaches `Ready` only when that endpoint's `/health` reports `service_ready: true`, and reports
      capacity where the external `/metrics` is reachable and absent where it is not. The finalizer
      still holds on a non-empty `usedBy`.
      The observation path is the SAME one a managed backend goes through, reached by publishing the
      spec's endpoints into status first; only two rules differ, both recorded above. An address that
      does not answer is `Error`/`LeaderUnreachable` from the first pass rather than `Provisioning`,
      because there is no Pod of ours whose readiness could excuse it (F3); and capacity is the sum of
      the memory and file pools, because an external backend names no medium to pick one by (F7).
      Verify: `go test ./pkg/worker/controllers/worker/ -run KVCacheBackendExternal` — the render
      refusal, the mirrored endpoints, the address each read went to, the `service_ready` gate, a
      reachable and an unreachable scrape, a tiered exposition that tells the summed rule from the
      medium rule, the unreachable-is-a-fault verdict, a refused delete, and a second pass that
      writes nothing. Every case runs against the create-refusing client, so "renders nothing" is
      asserted by all of them rather than by one.

- [x] **T12 · The acceptance run on a local two-node cluster**
      Blocked by: T2, T5, T6, T8, T9, T10, T11
      Owns: no source paths — the recorded run and the figures it produced, folded back into this
      spec's Verification section and T13's page
      Gate: review
      Acceptance: all six items in Verification met on a local two-node cluster with **no GPU, no
      RDMA, no cloud and no etcd**: `Ready` with one DRAM group; capacity equal to the member sum and
      read from `master_total_capacity_bytes`; a third node added with the master's and the existing
      members' Pod UIDs and restart counts unchanged; a delete refused while `usedBy` is non-empty; a
      `spec.metadata.mode: etcd` apply **refused by the schema and pruned without one** — F4 gives the
      metadata plane no field, so there is nothing for a webhook to refuse and no follow-on gets
      named; the run records both halves, because the pruning half is the one an operator would never
      notice; and `helm install` leaving
      `kvcachebackends.worker.gpustack.ai` present with no chart manifest and no RBAC change. Every
      assertion cites an **observed effect** — a served endpoint, a moved counter, a refused apply —
      never a flag being accepted or a log line echoing it back.
      Verify: the recorded transcript of the run — `kubectl get kvcb -o yaml` at each step, the
      master's `/health` and `/metrics` bodies, and the `kubectl apply` rejection text.

- [x] **T13 · Documentation**
      Blocked by: T12
      Owns: `docs/kv-cache/backend.md`, `docs/README.md`
      Acceptance: the page states the two axes and why they are two; the cluster-scoped argument with
      the Kueue precedent link; the measured master-variant and client-layout tables, why `spec.image`
      is explicit, and that the master image needs no accelerator runtime while a member image needs
      the runtime of its transport; that the metadata plane and the HA backend store are different
      axes, that the plane is peer-to-peer and a single-leader backend has no external
      dependencies, and the refusal message for `etcd` and `httpServer`; the member's
      environment-variable contract with the `MOONCAKE_TE_META_DATA_SERVER` spelling; the `/health`
      four-field view, that its `status` field is a constant and `service_ready` the only verdict in
      it, and why the two probes therefore take different paths; that capacity is **absent** rather
      than zero while the master is starting, because `/metrics` answers 200 with zeroed gauges before
      the service plane is up; that capacity is observed, never summed; the reachability requirement as a
      **port range**; that growth does not rebalance and how `allocationStrategy` affects convergence;
      that growth does not restart the members already running and WHY that needs `OnDelete` plus a
      fingerprint rather than the default strategy; ⛔ that **shrinking a group discards the cache
      that member held**, stated plainly rather than implied, with the reason it is not drained;
      that `replica_num` is engine-side; the benign `Local segment descriptor not found` startup line;
      and the client environment knobs. For the external mode: that nothing is rendered, that the
      `Admin` endpoint is what this operator reads and the `Client` one what engines connect to, that
      an address that does not answer is reported as an error rather than as a backend still starting,
      that `members[]` carries no node name or medium because those Pods are not this operator's, and
      that capacity is the sum of both pools because the object names no medium.
      The index in `docs/README.md` lists it.
      Verify: `make lint docs`.

### Test Plan

[ ] I/we understand the owners of the involved components may require updates to existing tests to
make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

- `pkg/worker/kvcache` needs recorded fixtures: the measured `/health` document, a Prometheus
  exposition carrying the five families read, a `/get_segments_detail` listing (with variants
  carrying a `DRAINING` segment, a state string this version does not know, and an empty list), a
  malformed variant of each, and a body missing the family entirely. These are the substitute for a
  cluster in every status test.
- `pkg/worker/controllers/worker` needs a fake HTTP round-tripper for the admin client that can
  answer, per case, a body / a malformed body / a **503 `service plane is not active`** / a refusal —
  and a fake ctrl client that **fails the test if any create is issued**, so F3's and T11's "renders
  nothing" claims are asserted rather than assumed.
- `pkg/worker/webhooks/worker` follows the package's existing table shape; each refusal case asserts
  the **message**, not only the rejection, because an unactionable message is the failure mode F4 and
  F8 exist to prevent.

#### Unit tests

**Admission cases** (`pkg/worker/webhooks/worker`) — every one of these is a measured constraint, and
the assertion is on the message as well as the verdict.

| Case | Input | Expected |
|---|---|---|
| `type_unknown` | `type: nfs` | rejected by the SCHEMA, not here; no webhook case exists for it, whether the value is a medium name or anything else outside the enum |
| `image_empty` | `image: ""` | rejected; message names the variant choice |
| `image_blank` | `image: "   "` | rejected; message says an image is named or left out entirely, never set to blanks — the renderer trims, so blanks would resolve to the Setting instead of to what the object names |
| `image_fallback_grandfathered_on_update` | an object admitted while the Setting carried a value, the Setting then cleared, then an update that does not move `spec.image` | admitted. The fallback is read from an EDITABLE setting, and not every update is the user's: the reconciler removing this object's finalizer is one, and refusing it strands the object undeletable forever — after teardown has already deleted every workload it ran. The narrowing is bounded by its counterpart: an update that DOES move `spec.image` re-asks the question and is refused |
| `member_image_blank` | `members[0].image: " \t "` | rejected; the member renderer uses the override verbatim, so blanks would reach the container runtime as an image reference |
| `connection_neither` | no `managed`, no `external` | rejected; message names both |
| `connection_both` | both set | rejected; message names both |
| `master_replicas_two` | `replicas: 2` | rejected; message names the master-HA follow-on |
| `two_member_groups` | DRAM + DRAM | schema-valid, webhook-rejected; message names the tiering follow-on. Both groups are DRAM on purpose: a second medium would trip the medium rule below as well, and the case would stop pinning which rule refused it |
| `medium_not_dram` | one group of `LocalDisk`, `NoF`, `CXL` or `DFS` | schema-valid, webhook-rejected, one case per medium; message names what would have to render it — the leader's file or DAX flags and a mount on the member |
| `capacity_per_member_zero` | `capacityPerMember: 0` | rejected. A `resource.Quantity` is a STRING in the schema, so no marker can bound it and this is the only place it can be refused; zero is not defaulted, because the renderer omits the segment size it derives from a non-positive value and a member that mounts nothing is indistinguishable from one whose leader lost it |
| `capacity_per_member_negative` | `capacityPerMember: -1Gi` | rejected, same rule |
| `local_buffer_negative` | `localBufferSize: -1Gi` | rejected; it is added to the member Pod's memory request |
| `leader_defaults` | `leader: {}` | accepted; `replicas` defaults to 1 and `allocationStrategy` to `FreeRatioFirst` |
| `metadata_mode` | `spec.metadata.mode: etcd` | **no webhook case exists or can**: F4 gives the metadata plane no field, so this is an unknown one and never reaches admission. Measured on a cluster — a strict client is refused by the SCHEMA with `unknown field "spec.metadata"`, and a client that turns validation off has it **silently pruned** |
| `external_endpoint_role_missing` | `external.endpoints` names only `Client` | rejected; message says the scrape has no address |
| `external_address_not_host_port` | a blank address, one with no port, one with no host, a non-numeric port, a port over 65535 | rejected, one case each; an IPv6 literal in brackets is accepted. The schema types the field as a bounded string and so cannot say `host:port`, which makes this the only place any of it is refused — and a blank one would be mirrored into `status` and handed to an engine that cannot dial it |
| `name_renders_an_invalid_object` | a name containing a dot; a name whose `-leader` name reaches 64; a name of 56, where `-leader` fits at 63 and `-member-0` does not | rejected; message names the rendered object. The CR's own name is a DNS **subdomain** while what is rendered from it must be a DNS-1035 **label**, so a name that clears its own rule can fail its children's and then fail inside every reconcile with nothing to show but a create error. Only a managed backend is checked — an external one renders nothing |
| `address_cannot_form_a_url` | `bad host:9003` | rejected. `net.SplitHostPort` splits on the last colon and asks nothing about what it split, so this parses cleanly and then fails where the address is USED — `url.Parse` rejects the space. Admission asks the question the caller will ask, or it admits an address guaranteed to fail on every scrape |
| `extra_args_key_is_dashed` | `-rpc_port`, `-config_path`, `--global_segment_size` | rejected on both sides, before the rule tables are consulted. The tables key on the bare name, while the leader renderer prepends a dash — so `-rpc_port` matches no rule and is still rendered `--rpc_port=`, which gflags reads as the flag `rpc_port` names. A dashed key is how the escape hatch would otherwise reach the flags it is forbidden to touch |
| `image_absent_everywhere` | no `image`, Setting unset | rejected; message names both the field and the Setting |
| `image_from_setting` | no `image`, Setting set | accepted |
| `extra_args_collides` | `extraArgs: {allocation_strategy: random}` | rejected; message names `allocationStrategy` |
| `extra_args_rpc_pair` | `extraArgs` carries both `rpc_address` and `rpc_interface` | rejected; the interface name overrides the address |
| `extra_args_offload` | `extraArgs: {enable_offload: "true"}` | accepted — the escape hatch works |
| `update_freezes_type` | `type` changed | rejected |
| `update_allows_selector` | `nodeSelector` widened | accepted |

**Rendering cases** (`pkg/worker/kvcache`)

| Case | Condition | Expected |
|---|---|---|
| `leader_argv_canonical` | the canonical spec | exact argv, in order; `-rpc_port` present, `-port` absent |
| `leader_argv_no_defaults` | a spec mentioning nothing optional | no flag rendered at the artifact's default |
| `leader_argv_enable_ha_absent` | any spec in this scope | `-enable_ha` and `-ha_backend_type` never rendered |
| `leader_argv_no_metadata_flag` | any spec | `-etcd_endpoints` absent; no metadata flag of any kind |
| `leader_argv_extra_last` | `extraArgs` set | appended after every derived flag, deterministically ordered |
| `master_pod_not_host_network` | any spec | `hostNetwork` unset, no device mount |
| `member_entrypoint` | any spec | the container command is `mc_store_rest_server` |
| `member_env_full` | the canonical spec | every `MOONCAKE_*` variable of F9's table, exact values, and nothing else |
| `member_env_meta_data_spelling` | any spec | `MOONCAKE_TE_META_DATA_SERVER` — underscore inside `META_DATA` — set to `P2PHANDSHAKE` |
| `member_no_config_mount` | any spec | no ConfigMap, no volume, no init container |
| `member_extra_args_d_flag` | member `extraArgs` set | rendered as `-D <key>=<value>`, keyed by config key |
| `member_requests_dram` | DRAM, 500Gi + 4Gi | memory request 504Gi |
| `member_requests_local_disk` | LocalDisk | ephemeral-storage request, not memory |
| `member_rdma_context` | resolved `rdma` | `hostNetwork`, `/dev/infiniband`, `IPC_LOCK`, `SYS_RESOURCE`, and `privileged` unset |
| `member_tcp_context` | resolved `tcp` | none of the four |
| `member_no_data_plane_port` | both paths | no `containerPort` for the data plane |
| `member_readiness_proves_the_mount` | any spec | a readiness probe connecting to TCP 8080, asserted as the literal rather than against the constant — comparing a constant to what it rendered is a tautology that survives any change to it. TCP and not HTTP for the reason in F9, and the pair of membership rows below rests on this one: strip the probe and `PodReady` stops carrying mount state |
| `member_advertises_its_pod_ip` | any spec | `MOONCAKE_LOCAL_HOSTNAME` resolves from `status.podIP`, never `spec.nodeName`. It is the address a client dials, and the engine binds in the pod's netns — measured, a node name there is `ECONNREFUSED` from a client pod while the pod IP connects. Asserted as the field path rather than through a rendered value, since the downward API resolves at admission |
| `data_plane_round_trip` | an independent client Pod mounting no segment | `put` then `get` returns a byte-identical payload, and the member's `allocator_used_bytes` rises by exactly the size written. Measured: a 1 MiB put took `used` from 0 to 1048576 and an 8 MiB put took it to 9437184, against a segment whose endpoint is the member's POD IP. The only item here that leaves the system's own account of itself |
| `workloads_mount_no_sa_token` | any spec, both roles | `automountServiceAccountToken: false` rendered EXPLICITLY, and converged by the aligner. These are third-party store binaries that never call the API server; left unset the server defaults it to true, and a field the renderer omits is one the aligner has nothing to converge toward |
| `converge_takes_back_a_limit` | a `resources.limits` added by hand to a rendered container | removed on the next pass. The renderers set no limits — the backend's claim is a REQUEST because a member sizes its segment from the same figure — so a limit is a field left at its zero value, and an injected one OOM-kills the container on a loop with nothing on the object saying why |
| `listing_too_large_by_bytes` | four segments whose names alone exceed the byte budget | `ListingTooLarge`, same as the too-many-entries row. The COUNT is not the bound that matters: every string in a segment is chosen by the admin endpoint, which for an external backend is somebody else's, so a few long identifiers outweigh thousands of ordinary entries |
| `image_unresolvable_keeps_observing` | an admitted backend whose Setting is cleared afterwards, leader still answering | reconcile does **not** return: rendering is skipped, observation continues, and the phase is `Degraded` naming the setting. Returning froze status on a stale reading behind an exponential backoff while the store kept serving. Ranked over a shortfall in the phase, because an unrendered member group IS a shortfall and reporting that would name the symptom and hide the cause |
| `capacity_absent_is_no_field` | any failed or gated scrape | `status.capacity` absent, not `{}`. `omitempty` does not omit a zero-valued struct, so held by value it serialized as an empty object on every unsuccessful observation — a third shape between "absent" and "reported", which the contract does not have |
| `endpoint_address_length` | a 253-character host with a five-digit port | accepted. The bound is on `host:port`, and at 253 the schema refused an address the webhook's own rule accepts |
| `printcolumn_filter_survives_parsing` | a printcolumn whose jsonPath is a filter | the jsonPath reaches the CRD whole. The marker parser splits on `=`, which a filter contains twice; it used to come back as its own entry plus two junk ones, invisible because unknown keys were dropped in silence |
| `member_device_env_name` | RDMA path | the environment key is `MOONCAKE_DEVICE`, never the pybind `rdma_devices` |
| `grow_selector_template_stable` | selector widened | master Deployment and member Pod template byte-identical |
| `converge_takes_back_a_hand_grant` | the leader Deployment edited to `hostNetwork: true`, the Service edited to `NodePort` | both taken back on the next pass, and the assigned `nodePort` released with the type. Both are fields the renderer only ever leaves at their zero value, which is why they need converging at all: a comparison that looks only at what the renderer sets to something interesting never notices one being turned on. The Service matters most — the leader's admin API has no authentication of its own and is private only because the Service is a ClusterIP, and the port comparison ignores `nodePort` on purpose, so the exposure would stand while the ports kept comparing equal |
| `converge_readiness_probes` | the leader Deployment and a member DaemonSet each edited to drop their readiness probe | both restored on the next pass. Neither role had a convergence case for this before: the aligner compared the field all along, but nothing asserted it, so deleting that comparison left the entire suite green. A rendered field with no assertion is indistinguishable from one that is never rendered |
| `transport_unset_renders_tcp` | `spec.transport` never set | the same Pod spec `Auto` renders. Structural-schema defaulting does not descend into an ABSENT object — measured against an API server: omitted leaves `transport` empty, `transport: {}` comes back `{"protocol":"Auto"}` — so the containing object carries `default={}` and the renderer falls back as well, for an object that never passed through an API server |

**Observation cases** (`pkg/worker/kvcache`, `pkg/worker/controllers/worker`) — absent is a value; zero
is a different value.

| Case | Condition | Expected |
|---|---|---|
| `health_service_ready_false` | measured body, `service_ready: false` | `Provisioning`; never `Ready` |
| `health_ready_no_members` | ready master, listing empty | `Degraded`, `MembersMounted=False` |
| `health_malformed` | body is not the measured document | `Error`; the decode failure in the message |
| `health_without_service_ready` | `{}`, a document carrying every other field, an explicit `null` | **malformed**, not "not serving". The field is decoded through a POINTER, unlike every other bool here, because it is the whole readiness verdict: as a plain bool an absent one is indistinguishable from an explicit `false`, and `false` is a PHASE — so an unrelated JSON body would read as a leader that is up and not serving and wait at `Provisioning` forever. `service_ready: false` present stays the phase it always was |
| `health_refused` | connection refused | `Error`; distinguishable from malformed |
| `leader_unschedulable` | the leader's Pod carries `PodScheduled=False` with reason `Unschedulable` | `Error`, reason `LeaderUnschedulable`; the same object with no Pod yet is `Provisioning`, and the case asserts both halves because the fault only means anything against the start it is told apart from. The question goes to the Pod, not to the Deployment, which says so only through `ProgressDeadlineExceeded` ten minutes later |
| `capacity_follows_scrape` | scrape total ≠ member sum | the scraped value published |
| `capacity_scrape_failed` | scrape errors | `total`/`used` **absent**, `CapacityObserved=False` |
| `capacity_family_missing` | exposition without the family | absent, not zero |
| `capacity_fractional_refused` | a sample of `1.9` | refused as malformed, not truncated. `int64()` would publish `1` — a figure the leader never reported — under `CapacityObserved=True`. The counterpart is asserted too: `%g` is how a Prometheus exposition writes a gauge, so `5.36870912e+11` is integral however it is spelled and must still be accepted |
| `capacity_zero_while_starting` | a clean all-zero exposition paired with `service_ready: false` | absent, **not** a published zero — the scrape succeeded |
| `health_status_ok_is_not_ready` | `status: "ok"` with `service_ready: false` | `Provisioning`; nothing branches on the constant |
| `capacity_file_medium` | a DFS group | read from `master_total_file_capacity_bytes`. Admission refuses a DFS group (F1), so this exercises the family selection directly rather than through a reconcile — the selection is decided and covered now so that rendering the medium later does not also have to decide it |
| `members_state_passthrough` | listing reports `OK` | every member `OK` |
| `members_state_draining` | listing reports `DRAINING` | published as `Draining`, still listed |
| `members_state_unrecognised` | listing reports a state this version does not know | published verbatim; the status write is accepted |
| `members_protocol_from_master` | listing's `protocol` ≠ the rendered one | the **listing's** value published |
| `members_pod_without_segment` | a ready member Pod the listing omits | absent from `members[]`; the shortfall in `MembersMounted`'s message |
| `members_listing_failed` | the listing scrape errors | `members[]` unchanged, `MembersMounted=False` |
| `member_pods_unreadable` | the listing is read, the SECOND pod listing of the pass fails | the segments are published and `MembersMounted=False`, reason `PodsUnreadable`. With no Pods listed the running count is zero, no shortfall can be found against it, and a non-empty listing would otherwise fall through to `Mounted` — claiming every member is accounted for on the strength of a comparison that never ran. Only the second listing fails, because a pass reads member Pods twice and a failure of the first returns from the reconcile long before this |
| `member_pod_starting_is_not_short` | one ready Pod, one Pod not ready, one segment | `MembersMounted=True`. A Pod is INDEXED whatever its state, so the segment it eventually mounts can still be joined, and COUNTED only when ready — counting one that is pulling would invent a shortfall against it and hold a healthy backend at `Degraded` for the length of a rollout |
| `member_pod_stuck_is_short` | one ready Pod holding the one listed segment, one Pod in `CrashLoopBackOff` or `Unschedulable` | `MembersMounted=False`, `Degraded`, and `phaseMessage` carries that Pod's own reason. The counterpart of the row above, and the pair is the contract: a member on its way is not a shortfall, a member that has STOPPED is one. Without this, a group that has lost a node reads exactly like a healthy one — only ready Pods are held to the listing, so a member that never became ready produces no shortfall at all |
| `member_pod_stranger_ignored` | a Pod carrying this group's three identity labels whose controller reference is another DaemonSet — once ready, once crash-looping, once unschedulable | ignored in all three: no shortfall, no fault, and not indexed for the node/medium join. The labels are DERIVED from the backend's name, so anything can carry them; the restart path proved ownership from the start, while the two observation paths trusted the selector |
| `listing_too_large_withheld` | a listing of more segments than `status.members` can hold | `MembersMounted=False` with reason `ListingTooLarge`, the previous listing kept, and nothing published. Truncating would read as a backend that lost members, and publishing would push the object past what the api server accepts — after which EVERY status write fails while the observation reports success |
| `admin_service_inactive` | any admin route answers 503 `service plane is not active` | `Provisioning`, distinguishable from a 404 and from a refused connection |
| `delete_with_used_by` | `usedBy` non-empty | object retained, `Deletable=False`, consumer named |
| `delete_after_used_by_cleared` | `usedBy` emptied | workloads deleted, then the finalizer removed |
| `external_creates_nothing` | `connection.external` | zero creates, asserted by a failing fake |
| `reconcile_twice_no_write` | settled state | no update, no status write |
| `settled_backend_requeues` | a fully observed backend, managed and external | both return the 15-second `RequeueAfter`. The external half is the one that matters: it owns no workload, so the timer is its only trigger |
| `leader_pod_maps_back` | a Pod carrying the leader's rendered labels, a member's Pod, a foreign Pod wearing the same `instance` label, an unlabelled Pod | only the first enqueues its backend, and the predicate agrees with the mapper on all four. The labels come from the renderer's own output rather than being restated, so the case cannot pass against a renderer that labels its Pods differently |

#### Integration tests

- The reconciler against a fake ctrl client plus the fake admin round-tripper: create → master and
  member workloads rendered → `/health` ready → capacity observed → `Ready`; then the scrape starts
  failing and capacity goes **absent** while the phase stays `Ready` with `CapacityObserved=False`.
- Growth: reconcile, widen the selector, reconcile again, and assert the master Deployment's and the
  existing member template's rendered bytes are unchanged while the DaemonSet's selector moved.
- Teardown: a non-empty `usedBy` holds the object across repeated reconciles; clearing it completes
  the teardown; a partially-deleted state converges on repeat.
- Concrete test names are added when the implementation lands.

#### e2e tests

One run on a local Kubernetes cluster — no GPU, no RDMA, no cloud and no etcd — covering the six
acceptance items of **Verification** in order, each judged on an observed effect rather than on a
flag being accepted. It has been run. The cluster it ran on, the result of every item, and the three
things it established that no fixture had are recorded above under **The recorded run**; they are
not restated here, because a second copy of a measurement is the copy that goes stale.

## Alternatives

- **Put the media on `spec.type` — `mooncake | nfs | 3fs`.** Rejected as a category error, on the
  artifact's own evidence: `-global_file_segment_size` is documented as "Size of global NFS/3FS
  segment in bytes" and `master_total_file_capacity_bytes` as "Total capacity for file storage in
  3fs/nfs", so NFS and 3FS are media a Mooncake segment sits on rather than alternatives to Mooncake.
  It would also make the single most common request — one backend with a DRAM hot tier and an NFS cold
  tier — inexpressible, because a single-valued `type` cannot carry two media.
- **Derive `spec.image` from the operator image**, as this repo derives the Device Manager's.
  Rejected: the master and the engine client can be builds against different accelerator generations
  — the base wheel's master links CUDA 12 while the vLLM image measured carries CUDA 13 — so a
  derived image would pair a master with a runtime it cannot load, and the failure surfaces as a
  loader error inside a container rather than as a validation error at apply time.
- **Ship two stub `.so` files so the base package's master runs anywhere.** Measured to work for the
  master — empty stubs with the right SONAMEs serve `/metrics`, `/health` and the admin API — but
  rejected as this spec's answer: it is a property of an image we do not build, the same stubs do not
  satisfy the Python client (`undefined symbol: cudaFreeHost, version libcudart.so.12`), and an
  accelerator-free published variant achieves the same end with no trick.
- **A per-hardware or per-vendor backend object.** Rejected: every medium in the enum is a host
  resource, so the *medium* axis is vendor-neutral and the backend stays singular. The axis that does
  carry a vendor is the transport, and it is carried by the published wheel rather than by a custom
  build — `-rocm` compiles `HipTransport` in, `-npu` ships `ascend_transport.so` — so the vendor
  question is answered by choosing `spec.image`, not by forking the object.
- **Require an external etcd as the metadata plane**, which is what a reading of `-etcd_endpoints`
  suggests. Rejected on measurement: the master ran with `enable_ha=0` and never contacted etcd, while
  clients set up with `metadata_server=P2PHANDSHAKE` completed put/get against it including
  cross-tenant isolation. Making etcd the default would have made every single-master backend depend
  on a service it never talks to.
- **Enumerate `kubernetesLease` as a metadata mode.** Rejected as a category error rather than on
  availability: `-ha_backend_type=k8s` belongs to the master's **HA backend store**, an axis that only
  exists at `replicas > 1`. Whatever the artifact does or does not implement there is the master-HA
  follow-on's evidence to carry, not this spec's.
- **Have the master serve the metadata plane itself** with `-enable_http_metadata_server` on 8080.
  Attractive and measured as *present*; not taken here because it is not measured as *sufficient*, and
  this spec's discipline forbids claiming a capability from a flag's existence. The enum reserves
  `httpServer` for it and the webhook refuses the value; recorded as an Open Question.
- **Render the member's configuration as a mounted config file**, using `mc_store_rest_server`'s own
  `--config`. Rejected: every config key has a real named `MOONCAKE_*` environment variable, so the
  file would buy a ConfigMap, a volume and a second place for the truth to live, and buy nothing else.
- **Render the master's flags as environment variables** via `-fromenv` / `-tryfromenv`. Rejected:
  those flags take a comma-separated list of flag *names* and then read `FLAGS_<name>`, so the env
  route costs one env var **plus** one argv entry per flag and splits the truth across two places
  without shortening the command line.
- **A Deployment with anti-affinity for the members**, instead of a DaemonSet. Considered and not
  taken on semantics: a member contributes *a node's* medium — it claims that node's host memory and
  host paths and, on the RDMA path, that node's `/dev/infiniband` — so its identity is the node, which
  a DaemonSet states and anti-affinity only approximates. Kueue's Topology-Aware Scheduling accounts
  for a Deployment just the same, so it is not an argument either way. The shape is revisited when
  several members per node, or rollout control over a shrink, is actually needed.
- **Compute `status.capacity` from the spec.** Rejected: it makes the status a restatement of the
  spec, so a member that failed to mount is invisible. The master's own counter is the only figure
  that can disagree with the request, and that disagreement is the signal.
- **Namespaced `KVCacheBackend`.** Rejected: the object names nodes, claims host memory and host
  paths and, on the RDMA path, needs `hostNetwork` plus `/dev/infiniband` — only a cluster admin can
  legitimately declare one. Tenant isolation is a different axis, one layer up, exactly as Kueue
  separates `ClusterQueue` from `LocalQueue` (<https://kueue.sigs.k8s.io/docs/concepts/>).
- **Enumerate the offload and promotion knobs as first-class fields now.** Not taken: they belong with
  the tiering work that will exercise them, and a field per flag would freeze names before the
  semantics are settled. The `extraArgs` passthrough keeps them reachable in the meantime, which is
  what stops an operator from patching the rendered objects.
- **Expose `-quota_bytes` as a `spec.quota` field.** Rejected: it is a **global** storage-backend
  quota, not a per-tenant one, and naming it `quota` here would collide with the per-tenant quota
  vocabulary a later spec owns. It stays reachable through `extraArgs`.
- **Copy `master_key_count`, the soft-pin count and the request counters into `status`.** Rejected: the
  master already exposes them on a scrapeable endpoint, and a status copy is a staler second source of
  the same numbers.

## Open Questions

- **Secret-bearing flags in argv.** The master's flags are rendered as argv, which is world-readable
  in the Pod spec. This scope renders none that carry a credential, because it renders no metadata
  store to connect to; the exposure arrives with `extraArgs` today and with a server-backed metadata
  mode later, where a connection string with credentials would leak to anyone who can read the
  Deployment. Whether such flags should move to Secret-backed environment variables — for those flags
  only, keeping argv for the rest — is open.
- **The member workload shape past one member per node.** A DaemonSet is chosen on semantics, and it
  places exactly one member per selected node. Several members per node (one per NUMA domain) and
  rollout control over a shrink both point at a different shape; which of the two forces the change
  first is open, and `capacityPerMember` is named so it survives either.
- **Whether `spec.type` should ever express "an existing RWX volume"** — a shared-filesystem backend
  nobody manages, where the engine's own `fs://` layer does the work. It is out of the enum, not
  reserved in it: it arrives as a value when something implements it, and widening an enum is not an
  API change.
- **Whether this API should expose `-quota_bytes` at all, and under what name.** It is a global
  storage-backend quota, distinct from the per-tenant quota a later spec owns, and any name that does
  not say "global" will be read as the tenant one.
- **Whether the master can serve its own metadata plane** with `-enable_http_metadata_server`, which
  is what the reserved `httpServer` mode would render. The flag is measured present; it is not
  measured sufficient, which is why the webhook refuses the value rather than shipping it.
- **Whether heterogeneous clients** (an `-npu` build and a `-cuda13` build) **can share one master and
  read each other's segments.** Verified only as loading, and it belongs to the Ascend spec rather
  than here.
- **Who provides the etcd, when a server-backed metadata mode ships.** It is not a question this scope
  has to answer — the peer-to-peer plane needs nothing deployed — but the follow-on that enables `etcd` or
  `httpServer` inherits it, along with whether the operator should deploy one on the admin's behalf.
