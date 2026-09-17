# KV Cache Backend

> **Purpose** — how a `KVCacheBackend` runs a Mooncake store, what its status is read from, and the
> three things that surprise operators: capacity is observed rather than derived, shrinking a group
> discards the cache that member held, and a member group's identity is its position in a list.
> **Audience** operators, contributors · **Prerequisites** [Architecture](../architecture.md) ·
> **Read time** ~14 min

A `KVCacheBackend` declares a pooled KV cache for inference workloads. The operator runs a **leader**
(one metadata process) and a **member** group (one store process per selected node), then reports what
that backend is observed to be doing.

Two vocabularies meet on this page. This API says **leader**; the artifact says **master**, and every
rendered flag, environment variable and metric keeps the vendor's spelling.

## Contents

- [The two axes](#the-two-axes)
- [The image](#the-image)
- [The metadata plane](#the-metadata-plane)
- [The members](#the-members)
- [What status reports](#what-status-reports)
- [Growing and shrinking a group](#growing-and-shrinking-a-group)
- [The external mode](#the-external-mode)
- [Operating notes](#operating-notes)

## The two axes

A backend has a **connection** axis and a **medium** axis, and they are separate because they answer
different questions: who runs the backend, and what it is made of.

```yaml
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCacheBackend                 # cluster-scoped, short name kvcb
metadata:
  name: mooncake-dram
spec:
  type: Mooncake
  image: docker.io/kvcacheai/mooncake:0.3.13
  connection:
    managed:                         # or external: — exactly one
      leader: {}                     # replicas and allocationStrategy default
      members:
        - nodeSelector: {kubernetes.io/os: linux}
          medium: DRAM               # what this group's SEGMENT is made of: DRAM or VRAM
          capacityPerMember: 4Gi
```

⚠️ The example's `0.3.13` tag is on the wrong minor line for current runner builds — pin `spec.image`
per [The store version must match the engine's
client](#the-store-version-must-match-the-engines-client) before copying it.

`connection.managed` and `connection.external` are both optional pointers and **exactly one** must be
set; neither and both are refused at admission with a message naming the two. Several member groups
are allowed; at most one of them may carry a [local disk tier](local-disk-tier.md).

**`members[].medium` takes two values, `DRAM` and `VRAM`** — host memory or device memory. It is a
choice rather than an identity: the renderer splits on it, charging the segment to the Pod's host
memory on one and to nothing at all on the other, where the segment is device memory and claiming it
is allocating it (see [The members](#the-members)). One binary is one medium, so a node contributing
both does so as two groups selecting it.

The field is **immutable**: a segment already mounted cannot change the kind of memory underneath
the data it holds, so an edit is refused and the choice is made when the group is declared.

An earlier shape offered five values. Four of them named things that are **not member groups**, and
each is reached another way:

| Was a `medium` value | What it actually is | Where it lives |
|---|---|---|
| `LocalDisk` | a tier on the members that already hold the memory replica | [`members[].localDisk`](local-disk-tier.md) |
| `NoF` | an NVMe-oF target coordinate, registered once, with no node affinity and no Pod | no API surface; it is not a member group |
| `CXL` | a DAX device the **leader process** allocates from | nowhere in this API: `enable_cxl`, `cxl_path` and `cxl_size` are refused in `leader.extraArgs`, because the first replaces `leader.allocationStrategy` and then brings the leader up advertising the allocator's size as capacity even where no DAX device exists |
| `DFS` | a distributed filesystem the **leader process** allocates from | the leader's own environment, which this API does not render |

> **Why the shape matters more than the names** — the leader routes an offload task to the client
> holding the key's memory replica. A member group with no memory segment is therefore never chosen,
> so a group declared as "the disk one" would report its disk capacity to the leader and never
> receive a single write. The object would say one thing, the running member another, and nothing
> would report a fault.

⛔ **Without ratcheting, an object still carrying one of the other four values can never be updated
again — including by the controller removing its finalizer, so it cannot be deleted.** CRD validation
runs on the **write** path only (`rest.BeforeCreate` / `rest.BeforeUpdate`): the object still reads
back, and every update is refused. It is the shape of the Kueue upgrade finalizer deadlock.

**`CRDValidationRatcheting` is what decides that, and a version decides what the gate can be.** Where
it is on, an update whose invalid field is **unchanged** is admitted, so removing a finalizer still
works.

The gate is unavailable before v1.28, off by default from v1.28, on by default from v1.30, and
**locked on** from v1.33.

⚠️ Those thresholds are read against the API server's **effective** version, not the version of its
binary. `LockToDefault` is checked on the spec selected for the emulation version, so a newer server
emulating an older one resolves to the older spec and can still be running with the gate off.

By effective version, then, and against this chart's `kubeVersion: ">=1.23.0-0"`: the deadlock is
**unavoidable** below v1.28, a matter of **configuration** from v1.28 through v1.32, and
**foreclosed** from v1.33. Emulation does not move those boundaries — it is why the version printed
by a server's binary does not tell you which of the three it is in.

Reaching that state at all takes a cluster that installed the CRD, ran with **no webhook**, and
created a member in one of the removed values in that window — so it is a development cluster or
nothing.
`KVCacheBackend` is absent from every tag from `v0.8.0` through `v0.8.6`, checked per tag, and the
commit adding it landed after `v0.8.6`.

The narrowing was kept knowingly. The risk that was accepted, and the condition that closes it, are
recorded in
[the spec](../../specs/2026-09-05-kv-cache-media-and-scaling.md#f2--the-medium-enum-collapses-to-what-runs-and-each-removed-value-is-placed).

The object is **cluster-scoped**: it names nodes, claims host memory and host paths, and on the RDMA
and EFA paths needs `hostNetwork` and `/dev/infiniband`. Only a cluster administrator can
legitimately declare one.

Tenant isolation is a different axis, handled one layer up — exactly as Kueue separates `ClusterQueue`
from `LocalQueue` (<https://kueue.sigs.k8s.io/docs/concepts/>). One backend can be referenced by
several pools, which is the only reason a backend and a quota domain are separate objects.

## The image

**`spec.image` is explicit and never derived from the operator's own image**, which breaks
deliberately with how the Device Manager image is derived from the worker image. Leave it unset and
the cluster-wide `kv-cache-backend-image` Setting supplies it. That Setting ships a default, so a
backend naming no image runs this project's own build; unset in both places — which takes an
administrator clearing the Setting — is refused at admission, naming both.

**Clearing that Setting later does not strand a backend admitted under it.** Admission re-asks for a
fallback only when an update moves `spec.image` itself. Every other update is admitted whatever the
Setting says now — including the reconciler's own removal of the finalizer, which would otherwise
leave an object that owns nothing and cannot be deleted.

> **Why** — the master's link-time dependencies differ per published vendor variant, so no single
> derivation is correct. Measured with `readelf -d` on one CUDA-less host:

| variant | accelerator libs in `DT_NEEDED` | unresolved |
|---|---|---|
| base (CUDA 12) | `libcuda.so.1` + `libcudart.so.12` + `libmlx5.so.1` + `libibverbs.so.1` | 2 |
| `-rocm` | none — only `libibverbs.so.1`; not one ROCm/HIP library | 0 |
| `-npu` | none — not even `libibverbs.so.1`; the leanest | 0 |

The client side is where the vendor lives, and it lives in the published wheel rather than in a custom
build:

| variant | client layout | transports compiled in |
|---|---|---|
| base (CUDA 12) | everything static in `store.so` (18.8 MB) | `RdmaTransport`, `TcpTransport` |
| `-rocm` | `store.so` 19.3 MB | `HipTransport`, `RdmaTransport`, `TcpTransport` |
| `-npu` | thin shims over `libmooncake_store.so`, plus a separate `ascend_transport.so` | plus Ascend |

**The master image needs no accelerator runtime; a member image needs the runtime of the transport it
uses.** That is the sentence whoever picks an image needs. An `-npu`-built master on an all-NVIDIA
cluster is legitimate — the master is a pure metadata service. A member on `ascend`, by contrast,
needs CANN (`libascendcl.so`) in its container, and a CANN-less image fails as a loader error whose
own message reaches `status.phaseMessage`.

A member on `EFA` needs libfabric in its image, and one that can drive the node's adapter — not the
distro build. `mirrored-mooncake` installs AWS's, ahead of that copy in its loader cache.

Nothing has to be built to run this **without high availability**.
The example image above is published for amd64 and arm64 and carries **both** `mooncake_master` and
`mc_store_rest_server`, so one `spec.image` serves the leader and the members.
It runs on a host with no GPU: its `libcuda.so.1` is a stub and its `libcudart.so.12` is the real
library, and neither reaches a driver.

⛔ **[High availability](leader.md#high-availability) needs a different image, and for both roles.** That
section carries which build, why no published one will do, and what each role does when handed one
that cannot.

> **Why the stub/real split matters** — the stub alone is enough for the master, but the Python client
> needs a versioned `cudaFreeHost` from a real runtime. An image carrying two stubs runs the master and
> fails every member.

**That split is what the `kv-cache-backend-image` default cannot cover.** Its value is this project's
own CPU build, carrying TCP and EFA over DRAM, and one value cannot be right for every backend at
once: which build a member needs depends on the transport its backend asks for and the hardware its
group selects. A backend on a vendor fabric names its `spec.image`, which always wins over the
Setting.

The failure mode moved with the default, and that is what having one costs. Blank made a mismatch an
admission refusal naming both places to fix; a default makes a wrong image a loader error at runtime,
which is quieter and further from whoever can fix it. Clearing the Setting restores the refusal.

### The project's own build variants

Beside the `-cpu` default, this project publishes `mirrored-mooncake` in one build target per vendor
— `cuda`, `cann`, `rocm` — whose base images are dispatch-time build arguments. Tags carry the
toolchain version: `<mooncake-version>-<variant><toolchain>`, such as `0.3.13.post1-cuda13.0`.

Each variant is built on both Mooncake lines this project carries: `0.3.13.post1`, matching engine
clients from vLLM 0.28.0 on, and `0.3.10.post2` for the ones before. Which line a backend needs is
[the version table](#the-store-version-must-match-the-engines-client)'s question, not this one's.

**A VRAM group needs a build with VRAM segments compiled in (`USE_VRAM_SEGMENT=ON`), and the stock
`-cpu` default is not one.** VRAM segments exist only on the `0.3.13` line — the `0.3.10.post2`
variants carry the vendor transfer engine without them — so a VRAM group always names a
`0.3.13.post1` variant tag.

Nothing refuses a VRAM group that names no image: the default is an administrator-editable Setting,
so this paragraph is the guidance rather than a gate. A group names its image through
`members[].image`, which wins over the backend's `spec.image`, which wins over the Setting.

**A private registry needs `spec.imagePullSecrets`**, and an explicit policy needs
`spec.imagePullPolicy`. Both are backend-wide: they apply to the leader and to every member group,
including a group that names its own `image`. Left unset, the policy is **resolved from the image
tag by the same rule the API server would have applied** — `Always` for `:latest` or no tag,
`IfNotPresent` otherwise — and it is re-resolved whenever the image or the field moves.

> **Why they are fields and not Settings** — the cluster-wide `image-pull-policy` and
> `image-pull-secrets` Settings are values of the bundled-application chart install. They reach the
> subcharts and nothing a controller renders, so a `KVCacheBackend` that inherited them would be the
> only object in this API whose running workloads move when a chart value moves. The service accounts
> [high availability](leader.md#high-availability) renders carry no registry credentials either — they grant
> Lease access and nothing else — so without these fields no image here could come from a private
> registry at all.

### The store version must match the engine's client

**A store and an engine-embedded Mooncake client interoperate only within one minor line.** The
criterion is the RPC wire signature, not the version string: the handshake answers `2.0.0` for every
0.3.x release, so a mismatched pair is not refused at startup — every probe reads green, and the
first write fails at transfer time. Measured: a 0.3.13.post1 store against a 0.3.10.post2 client
starts fully healthy and then fails every write with `RPC_FAIL (-900)`.

| pair | interoperates | basis |
|---|---|---|
| identical release (0.3.10.post2 ↔ 0.3.10.post2) | yes | measured end to end |
| same minor, different post (0.3.12 ↔ 0.3.12.post1) | yes | RPC signatures unchanged between the two tags; the risk is confined to a method only the newer side knows |
| 0.3.10 ↔ 0.3.13 | no | measured, above |
| 0.3.11 ↔ 0.3.12 | no | 0.3.12 adds `tenant_id` to `GetReplicaList` and its batch form |
| 0.3.12 ↔ 0.3.13 | no | 0.3.13 replaces the key string with a new `ObjectMeta` struct in `PutEnd`/`UpsertEnd` and extends `GetReplicaListResponse`; the method names are unchanged, so the client reaches the handler and mis-decodes the arguments |

Multi-tenancy moves none of these lines. With it off — the master's own default — every request
resolves to the default tenant and no write is refused, and the cross-minor failures above were all
measured with it off.

**The client's version is a property of the engine image, not of anything on this CR**, and it is
not one number across images — not even across images of one vLLM version. A CUDA build inherits
whatever its upstream base carries, and upstream changed how that is pinned: releases through 0.27.1
install a wheel pinned by URL, while 0.28.0 and later resolve `mooncake-transfer-engine >= 0.3.12`
from PyPI at build time. A ROCm build compiles its own from a pinned Mooncake tag.

A lower bound is not a pin, so two images of one vLLM version built on different days can carry
different clients. Read the client off the image in hand rather than off a vLLM version. Measured
from the published images, by unpacking each one's `dist-info`:

| runner image | embedded client |
|---|---|
| `cuda13.0-vllm0.25.1`, `cuda13.0-vllm0.27.1` | 0.3.10.post2 |
| `rocm7.2-vllm0.27.1` | 0.3.11.post1 |
| `cuda13.0-vllm0.29.0` | 0.3.13.post1 |

Backends serving the first two rows name `spec.image` on the 0.3.10 and the 0.3.11 line
respectively, and the [default](../settings.md) fails both as measured above; it matches the third
row exactly. **An image absent from the table was not measured, and a neighbouring row is not
evidence for it** — the first two rows are one vLLM version and different lines. Direct P/D transfer
(`MooncakeConnector`, no `spec.kvCache`) is engine to engine and exempt from this matching.

Two boundaries, recorded so nobody rediscovers them: upstream has **no 0.3.12.post2** — the 0.3.12
line ends at 0.3.12.post1 — and nothing older than 0.3.10 is built or exercised by this project.
High availability carries its own per-version rule — the k8s-lease backend exists only from
0.3.11 — see [High availability](leader.md#high-availability).

## The metadata plane

**The metadata plane is peer-to-peer and has no API field.** The member's `metadata_server` renders as
the literal `P2PHANDSHAKE`, unconditionally. A single-leader backend therefore has **zero external
dependencies beyond its image** — no etcd, no Redis, nothing to deploy alongside it.

Two axes get confused here, so both are stated. The metadata plane is how clients find one another.
The **HA backend store** — `-enable_ha` with `-ha_backend_type` — is how leader replicas elect one
among them, and that is where the Kubernetes Lease lives. It is
[`highAvailability`](leader.md#high-availability), and it moves nothing on this plane.

⛔ **A manifest that tries to configure the metadata plane is not refused with a helpful message.**
There is no field, so there is nothing for a webhook to see:

- a strict client — `kubectl apply`'s default — is refused by the schema with
  `strict decoding error: unknown field "spec.metadata"`;
- a client with validation turned off has the block **silently pruned**, and the object is admitted
  and reconciled as though nothing had been written.

The second is indistinguishable from success at the point of apply. This section is the protection
against it: the metadata plane takes no configuration at all.

## The members

One member group renders **one DaemonSet** over `members[].nodeSelector`. A member contributes *a
node's* medium — that node's host memory or, on a VRAM group, its device memory, plus its host paths
and on the RDMA path its `/dev/infiniband` — so its identity is the node, which is what a DaemonSet
expresses. Two groups may select the same node — a DRAM group and a VRAM group is the shape the
second medium exists for — and each still renders its own DaemonSet.

The member's whole configuration renders as **environment variables**: no ConfigMap, no volume, no init
container.

| config key | environment variable |
|---|---|
| `local_hostname` | `MOONCAKE_LOCAL_HOSTNAME` (the **pod IP**, from the downward API) |
| `metadata_server` | `MOONCAKE_TE_META_DATA_SERVER` |
| `master_server_address` | `MOONCAKE_MASTER` |
| `protocol` | `MOONCAKE_PROTOCOL` |
| `global_segment_size` | `MOONCAKE_GLOBAL_SEGMENT_SIZE` |
| `local_buffer_size` | `MOONCAKE_LOCAL_BUFFER_SIZE` |
| `device_name` | `MOONCAKE_DEVICE` — **deliberately left unset**, see below |

⛔ **`MOONCAKE_TE_META_DATA_SERVER` carries an underscore inside `META_DATA`.** It is not
`MOONCAKE_TE_METADATA_SERVER`, and normalising it to the spelling that reads correctly **silently
degrades the metadata plane** rather than erroring. It is asserted byte-for-byte by its own test.

⛔ **`MOONCAKE_DEVICE` is left unset on purpose, and the documented value `auto-discovery` is a trap.**
The client splits that key on commas into a device filter and nothing special-cases the string, so
setting it produces a filter matching a device no host has. **Empty means "use every device found".**

⛔ **`members[].extraArgs` is the exception to the table above: it renders into the container's
argv**, as `-D key=value`, not into an environment variable. `leader.extraArgs` does the same on the
leader, as `-key=value`.

⛔ **Either way the value is world-readable, on three paths.** It is stored verbatim on the
`KVCacheBackend` — which is cluster-scoped, so reading it needs no access to any workload — and it is
rendered into the container's argv, readable again from the Pod and from the DaemonSet or Deployment
carrying it. **Do not put a credential in `extraArgs`.** Nothing refuses one at admission.

`spec.transport.protocol` accepts `Auto`, `TCP`, `RDMA`, `EFA`, `CANN`, `ROCM`, `MUSA` and `MACA`,
and defaults to `Auto` whether or not the `transport` block is written at all. **`Auto` resolves to
`TCP`** — it is not a per-node probe that promotes itself. `MUSA` and `MACA` are intra-node IPC
transports, not host fabrics, so they take none of the fabric privileges below.

**`members[].transport.protocol` overrides that value for one group; left unset, the group inherits
the backend's.** The override exists for the one thing two media do not agree on: a VRAM group
reaching its peers over a fabric while the DRAM group beside it stays on `TCP`.

**`TCP` is complete, not a fallback — and it costs CPU.** Everything the store does works over it:
the cache fills, cross-instance prefix reuse works, and capacity scales with DRAM and the disk tier.
What it does not have is the zero-copy path the fabrics take. Every transfer traverses the kernel
network stack packet by packet, and the CPU that does so is CPU the node is not giving to anything
else.

So a `TCP` pool under load shows higher CPU on the member nodes and higher transfer latency than the
same pool on `RDMA`, and **that is the transport behaving as designed**. Read it as a reason to ask
for a fabric, not as a defect to open a report against.

The fabric privileges below render per group from the group's effective protocol, and an engine is
handed the protocol of the group it matched — an engine whose constraint no group in the pool
satisfies is refused at admission rather than started.

`CANN` is the one value an engine can **require**: vllm-ascend's store client currently raises on
any other protocol (`MooncakeBackend.__init__`, verified at v0.23.0 and v0.26.0rc1 — upstream state,
not a contract, and it may change), so a pool serving Ascend engines declares `CANN` here — or on
one member group — rather than settling for the `TCP` default.

The member then needs a CANN-carrying image, per the variant table above — the project's own CPU
build compiles no `ascend` transport.

> **Why** — one group is one Pod template, which cannot express a per-node transport; and promoting to
> a host fabric would mean granting `hostNetwork` plus `IPC_LOCK` and `SYS_RESOURCE`. A privilege is
> requested, never inferred. Naming `RDMA` or `EFA` is also what accepts the security context that
> comes with it — which is those three things and **not** `privileged`. A `TCP` group sets none of
> them. `privileged` is reachable, but only by writing it into
> [`members[].securityContext`](#reaching-a-nodes-accelerator), never by naming a protocol.

An `EFA` group takes everything `RDMA` takes, plus one device from a plugin — by default
`vpc.amazonaws.com/efa`, which `spec.transport.deviceResourceName` overrides. That request is what lets the
member open the adapter: the `/dev/infiniband` mount carries the device node in while the device
cgroup still refuses `open()`, so a member without one starts TCP instead.

**The cluster therefore needs the device plugin that advertises whichever resource the group asks
for** — the AWS EFA plugin when the name is left at its default; without it no node advertises the
resource and the member stays unscheduled. Nothing is mounted from a host EFA install — the
libfabric an `EFA` member runs on is in the image. Storage-optimized families such as `i7ie` are not
EFA-capable; check `fi_info -p efa` on the node before selecting one.

`spec.transport.deviceResourceName` names the extended resource a fabric member asks one of, for the
clusters where that name is not AWS's. The RDMA shared-device plugin and the SR-IOV plugin each let
an administrator choose it, so no constant would be right on two clusters. Set it and the member
requests one of that resource; leave it unset and an `EFA` group still asks for the AWS name while an
`RDMA` group asks for nothing.

**It is read only on `RDMA` and `EFA`**, the two protocols that mount the device tree. Set beside any
other protocol it renders nothing at all — no other path opens a fabric device, and requesting a
resource there would only leave the member unschedulable.

> **Why an `RDMA` group should set it** — a member that asks for nothing mounts the device tree and
> is still denied `open()` by the device cgroup, so the store finds no adapter and installs `TCP`
> while the object reads as `RDMA`. That is the behavior every backend had before this field, and it
> stays reachable because naming a resource no plugin advertises leaves the member unschedulable
> instead — this operator cannot tell which of the two an administrator without a plugin would
> rather have.

**What a member's Pod requests follows the group's medium.** A DRAM member requests host memory for
`capacityPerMember + localBufferSize`. A VRAM member's segment is device memory, which **nothing
requests**: host memory carries `localBufferSize` only, and the device memory is claimed by
allocating it.

⚠️ **The engine sharing that accelerator does not know the member took a slice of it.** Nothing in
Kubernetes accounts for device memory, so the two are held apart by **sizing `capacityPerMember`
against the engine's own memory fraction**, and by nothing else. Give the engine a
`gpu_memory_utilization` that leaves `capacityPerMember` free, and expect whichever starts second to
fail on allocation if you do not.

A member group asks for **no** accelerator extended resource, because the claim would not be true.
Measured upstream at `v0.3.13.post1`: a member allocates its segment with one `cudaMalloc` per split
(`mooncake-store/src/client_buffer_allocation.cpp`), the splits stay under the transport's
registration limit rather than spanning devices (`GetTransportRegistrationLimit`, `real_client.cpp`),
and nothing on that path calls `cudaSetDevice`.

**One member's segment is therefore on one device** — whichever the container sees first. Requesting
one accelerator would take a whole one away from inference to account for a fraction of one device's
memory, on a node where the member cannot use the rest of what it took. An eight-accelerator node
keeps all eight available to inference, and one member contributes a slice of the first.

### Reaching a node's accelerator

A VRAM group's container needs the vendor's **user-space driver**, which is what the store's client
loads to allocate a segment at all. That driver is the node's and is never in the image, so it has to
be brought in. Nothing is inferred; every route is written on the group:

| Field | What it is for |
|---|---|
| `members[].extraEnvs` | The vendor runtime's own trigger, where one exists — `NVIDIA_VISIBLE_DEVICES: all` makes the NVIDIA toolkit inject the driver and every device. Ascend has no counterpart. |
| `members[].runtimeClassName` | The vendor container runtime named explicitly, for a cluster where it is not the default runtime. |
| `members[].hostPaths[]` | The driver tree taken from the node directly, for a cluster running no vendor runtime at all. |
| `members[].securityContext` | Privilege, when a mount alone is not enough to open what was mounted. |

⚠️ **Privilege alone does not cover this.** It opens the node's device tree under `/dev`, which is
where the device nodes are and is **not** where the libraries are: Ascend's driver is under
`/usr/local/Ascend/driver` with DCMI beside it, and NVIDIA's `libcuda.so` is injected by the
container runtime. A privileged member with neither a runtime class nor the driver mounted starts,
reports healthy, and fails to allocate.

`securityContext` is **merged onto** what the group's protocol already earned, field by field, with
`capabilities.add` **unioned**. A `RDMA` or `EFA` group therefore keeps `IPC_LOCK` and `SYS_RESOURCE`
whatever it declares — without them the transfer engine fails when it registers memory, long after
the container looked healthy. Declaring a capability adds it; a group that must hold neither of those
two declares a protocol that does not ask for them.

Each `hostPaths[]` entry is `{path, mountPath, type, readOnly}`. Name a `type` — left empty the
kubelet checks nothing, so a missing path becomes an empty directory in the container and the member
starts anyway. A mount path is refused if it duplicates another entry's, if it is `/dev/infiniband`
(the device tree a host-fabric group gets rendered, refused even under `TCP`, since the protocol can
change later), or if it is this group's own `localDisk.path`.

Two worked groups. The NVIDIA one needs no mounts at all, because the container runtime injects the
driver once the variable tells it which devices to inject:

```yaml
members:
  - nodeSelector: {kvcache-vram: "true"}
    medium: VRAM
    image: gpustack/mirrored-mooncake:0.3.13.post1-cuda13.0
    capacityPerMember: 8Gi              # the engine's gpu_memory_utilization must leave this free
    extraEnvs: {NVIDIA_VISIBLE_DEVICES: all}
```

⚠️ That variable is honored only while the toolkit's
`accept-nvidia-visible-devices-envvar-when-unprivileged` is on, which is its default and which
NVIDIA's own hardening guidance turns **off**. On a cluster that turned it off, use
`runtimeClassName` instead.

The Ascend one has no such variable, and this cluster runs no vendor runtime, so the driver is
mounted from the node:

```yaml
members:
  - nodeSelector: {kvcache-vram: "true"}
    medium: VRAM
    image: gpustack/mirrored-mooncake:0.3.13.post1-cann9.1
    capacityPerMember: 32Gi
    hostPaths:
      - {path: /usr/local/Ascend/driver, mountPath: /usr/local/Ascend/driver, type: Directory, readOnly: true}
      - {path: /usr/local/dcmi, mountPath: /usr/local/dcmi, type: Directory, readOnly: true}
      - {path: /usr/local/bin/npu-smi, mountPath: /usr/local/bin/npu-smi, type: File, readOnly: true}
```

⚠️ **A VRAM group may also carry a [local disk tier](local-disk-tier.md), and that combination is
supported by design but not yet measured.** No upstream test covers it; measuring it on the vendor
validation hosts is on this project's acceptance list. Until then, treat the pairing as unverified
rather than as guaranteed.

**Reachability is a port range, never a list.** The transfer engine picks its data ports at random —
one observed run bound `15002` and `15995`, a second client `16566` and `16655`, none of them
configured — and the peer-to-peer plane is what binds them. Write firewall and NetworkPolicy rules
between member nodes, and from engine clients, as a **range**. The rendered Pod declares no fixed
data-plane `containerPort`, because a fixed list would be a false statement.

**The management port is fixed, and on a host fabric it lands on the node.** A member serves its HTTP
API on `8080 + <group index>` — the first group on `8080`, a second group on `8081`. A `TCP` group
holds that port inside its own pod network namespace, but an `RDMA` or `EFA` group holds the host's,
so on every node such a group selects that port must be free. Reserve one port from `8080` upward per
member group.

> **Why it moves per group** — two host-network groups whose node selectors both match one node place
> two host-network Pods on it. On a single fixed port only the first binds; the second runs, never
> passes readiness, and reports nothing about why.

**A member advertises its POD IP, and that is what a client dials.** The address becomes the host
half of the segment's `te_endpoint`, which `status.members[]` is joined against and which the engine
hands to clients. Rules written for the data plane therefore target pod addresses, not node ones.

> **Why not the node name** — the engine binds its data port inside the pod's network namespace.
> Measured on a two-node cluster: advertising the node name, a client pod got `ECONNREFUSED` against
> both that name and the node IP, and connected only on the pod IP. It costs no stability — a
> segment's identity is minted fresh on every mount, a new id and a transfer port bound at random, so
> nothing here survived a restart anyway. On the host-fabric paths the pod holds the host's network
> namespace and this is the node's address regardless.

## What status reports

```console
$ kubectl get kvcb
NAME            TYPE       PHASE   ENDPOINT                                        CAPACITY
mooncake-dram   Mooncake   Ready   mooncake-dram-leader.gpustack-system.svc:50051  12Gi
```

Five phases — `Provisioning`, `Ready`, `Degraded`, `Error`, `Deleting`. `Ready` carries no
`phaseMessage`; every other phase carries one.

Conditions report the axes: `LeaderAvailable`, `MembersMounted`, `CapacityObserved`, `Deletable` and
`RolloutComplete`. Two more appear only where they have something to judge —
`SnapshotStorageShared` when [`leader.highAvailability.snapshot`](leader.md#high-availability) is
set, and `ElectionObserved` above one leader replica.

**Those last three do not move the phase, and that is deliberate.** A rollout in flight, a snapshot
claim only one Pod can mount, and an election that has not happened are all states in which the
backend serves normally — reporting them as `Degraded` would put a storage arrangement in the same
field as a leader nobody can reach. Read the conditions for them; the phase will not tell you.

**A member that is starting is not a shortfall; a member that is stuck is one.** A Pod still pulling
its image is left alone — holding it against the backend would report `Degraded` for the length of
every rollout. But one whose container will not start, or that no node will take, is never going to
arrive, so it reads `Degraded` even while the other members serve, and `phaseMessage` carries that
Pod's own reason.

**A member reads Ready only once its segment is mounted.** Its container carries a readiness probe
that connects to the entrypoint's REST port, and the entrypoint mounts the segment *before* it serves
that port — so readiness is evidence of the mount, not of the process.

> **Why the probe is load-bearing** — without it the kubelet reports Ready as soon as the container
> runs. Every ready member Pod is held to the leader's listing, so that window would read as a
> shortfall and move a healthy backend to `Degraded` for the length of every rollout.

**A listing too large to publish is withheld, never truncated.** Past what `status.members` can carry,
the phase reads `Degraded` with reason `ListingTooLarge` and the previous listing is kept.

> **Why** — every entry is republished on each pass, so publishing past the object size the API server
> accepts would make every status write fail from then on while the read that produced it reported
> success. A truncated list would be worse: it reads exactly like a backend that lost members.

**Status is polled every 15 seconds, not only refreshed on events.** Everything above is read over
HTTP from the leader, and a store whose contents move while its Pods sit still produces no Kubernetes
event at all — an external backend produces none ever, since this operator owns no workload for it.
So `kubectl get kvcb -w` moves on its own.

> **15 seconds is an interval, not a maximum age.** The timer starts after a pass finishes, and a
> pass makes up to three sequential HTTP reads. More importantly, `status.members` is **deliberately
> retained** when the segment listing cannot be read — a stale list plus `MembersMounted=False` is
> more honest than an empty one — so it has no age bound at all while that read keeps failing. The
> condition is what says whether the list was refreshed; the list alone never does.

**Capacity is observed, not derived.** `status.capacity` is read from the leader's own counters,
never from what the spec declares — nothing multiplies `capacityPerMember` by a replica count. A
backend with no disk tier reads `master_total_capacity_bytes`; one **with** a tier reads that plus
`master_total_file_capacity_bytes`. Adding two **observed** families is not the same thing as
adding up what members were asked to provide.

**`status.capacity.total` is capacity, not usage**, and a disk tier contributes the ceiling the
member declared — published as soon as the member registers, before anything is written there.

To ask whether the **disk** tier is holding data, the figure to read is not on the CR at all — see
[The tier is written one bucket at a time](local-disk-tier.md#the-tier-is-written-one-bucket-at-a-time).

⛔ **Capacity is absent — not zero — while the leader is starting.** `/metrics` is ungated: a leader
that is up but not serving answers 200 with a well-formed exposition whose gauges all read zero, and a
zero is indistinguishable at the parser from a genuinely empty cache. Publishing is therefore gated on
`service_ready`, not on the scrape succeeding.

`status.members[]` is read from the leader's segment listing, one entry per **listed** segment. Each
row carries the leader's `segmentID`, `clientID` and advertised `segmentName`; the list is keyed by the
unique segment ID because several members may legitimately share a name.

The leader is what allocation goes through, so a running member Pod it does not list holds nothing
and is counted in `MembersMounted`'s message instead. The two fields the listing cannot supply — node
name and medium — are joined in from the member Pod behind that segment, and left **empty** rather
than guessed when nothing matches.

⛔ **Two ready host-network member Pods that share an address make Pod attribution ambiguous.** Their
rows remain publishable because their segment and client IDs are distinct, but neither Pod exposes
the ID that maps a row back to it. `MembersMounted` goes `False` with reason
`AmbiguousMemberIdentity`; each row still carries the node and the medium its candidates **agree**
on, neither being a fact about one Pod, and leaves empty whichever of the two they dispute.

Two groups on one node do **not** collide by themselves. A `TCP` member advertises its own pod IP, so
each segment carries a distinct name even though both Pods answer to the node's name; the collision is
on host-network paths (`RDMA` and `EFA`), where both Pods hold the host's network namespace and
advertise the node's address.

The remedy is to give the groups node selectors that keep them on different nodes.

A failed listing scrape **keeps** the previous list and sets `MembersMounted=False`; a failed capacity
scrape **clears** the figures. That asymmetry is deliberate: capacity is two pointers and has an
"absent" that means *not observed*, while an empty list is a legible value meaning *no segments*, so
clearing it would publish a falsehood.

The only exception is a development object whose stored member rows predate the required segment and
client IDs. Such rows cannot be written under the current list schema, so the operator omits the
whole legacy listing, explains that migration in `MembersMounted`, and replaces it on the next
successful leader read. No released version contained the former CRD shape.

## Growing and shrinking a group

**Widening `members[].nodeSelector` adds members without restarting the ones already running.** The
DaemonSet places a Pod on each newly matching node; every existing Pod keeps its UID and its restart
count, leader included.

> **Why it needs `OnDelete`** — `nodeSelector` lives in the Pod template, so under the default update
> strategy widening it would roll **every** member. The DaemonSet is therefore left on `OnDelete`, and
> the operator decides restarts itself from a fingerprint over the whole template **except** the node
> selector. A widening moves no fingerprint; an image, argv, environment, resource or fabric change
> moves it and every member is recreated.

⛔ **A Pod runs the template it was created from, so a setting cannot protect the same edit that
removes it.** Anything rendered into the member Pod — the shutdown hook, its grace, the environment —
reaches a member only when that member is recreated. A departing member leaves with what it started
with.

⇒ To make such a setting apply to a shrink, do it in **two steps**: change only the setting and wait
for members to be recreated with it (their pod-spec-hash annotation moves), then narrow the selector
or remove the group. `scaleIn.gracePeriodSeconds` below is the case this bites today; the property
belongs to the Pod template, not to that field.

**Existing objects are not rebalanced.** How fast the cluster converges onto a new member depends on
`allocationStrategy`: `FreeRatioFirst` (the default) biases new writes toward the emptier member,
`Random` does not.

### A group's position is its identity

A group has **no name**. Its position in `members` is what the DaemonSet's name, its immutable
selector labels and its members' HTTP port are all derived from, so moving an entry in that list
leaves every one of those in place and changes only the spec underneath it.

⛔ **Moving a group to another position is refused at apply time.** The refusal names both positions.
Two shapes reach it:

| Edit | Outcome |
|---|---|
| swapping two entries | refused |
| removing a group ahead of others, which shifts the rest up | refused |
| appending a group | allowed |
| removing from the **end** of the list | allowed |
| editing a group in place, including widening its `nodeSelector` | allowed |

**To take a group out of service without removing it, narrow its `nodeSelector` until it matches no
node.** The group keeps its position, every later group keeps its DaemonSet, and nothing is rebuilt.

> **Why not give a group a name** — a name independent of position would make reordering free, and
> the price is paid once in full: a DaemonSet's `spec.selector` cannot be changed after creation, so
> every existing member DaemonSet would have to be deleted and recreated and **the entire cache would
> go with them**. Refusing the move costs nothing and rebuilds nothing. The decision, and what
> evidence would reopen it, is recorded on the `members` field itself.

⚠️ **The rule recognises a group that arrived unchanged at a position another group LEFT — not every
reorder.** Without a name there is nothing else to recognise a group by, so two shapes are knowingly
admitted:

- a reorder **combined with an edit** to the same group, which is indistinguishable from two ordinary
  edits;
- removing a group when a **later group is identical** to the one taking its place — `[A, B, C]`
  becoming `[A, C, C]`. That produces the same two lists as editing position 1 to match an unchanged
  position 2, which is how the second of two look-alike groups is taken out of service, so refusing
  it would forbid the operation recommended above. `[A, B, C]` to `[A, C]`, with no look-alike to
  arrive in the gap, is still refused.

Both admitted shapes leave one trace: the resulting `members` holds **two identical groups**. What the
rule buys is that the mechanical reorder — the one a rewritten manifest produces — is reported instead
of silently rebuilding members against another group's spec.

⛔ **Shrinking a group discards the cache that member held.** Narrowing the selector, or removing a
node, unmounts that member's segment **immediately** — there is no drain.

> **Why it is not drained** — the member's own API does take a graceful unmount with a grace period,
> and **it cannot be pointed at the segment this operator gives a member**. That segment is mounted by
> the client's own startup, which files it in neither record set the unmount routes read, so both of
> them refuse it: `/api/unmount` answers `500` with `segment_id not found in allocated records`, and
> `/api/unmount_shm` answers `500` with `not found in mounted records`. Measured against the example
> image above, at every grace period, **using the id the leader itself publishes for that segment** —
> while the same `/api/unmount` returns `200` for a segment mounted through `/api/mount`.

> **So this is not a matter of identifying the right member.** An earlier version of this page said a
> transport-independent hook was waiting on upstream to expose a member's own `client_id`. A member
> holding the exactly correct id is refused just the same, so nothing about telling members apart
> reaches a hook that drains. Closing the gap needs the segment to become unmountable upstream.

> **What would actually hand the keys back is a different operation on a different component.** The
> leader offers `POST /api/v1/drain_jobs`, which **migrates** a segment's data to named targets rather
> than unmounting it. It needs the remaining members to have room and it is a stateful orchestration —
> create, poll, then scale — so it belongs to the control plane and not to a shutdown hook. The
> `terminationGracePeriodSeconds` the operator sets lets the entrypoint finish its own shutdown; it
> does not preserve the data.

**`scaleIn.gracePeriodSeconds` holds the process, not the tier.** A member with a
[local disk tier](local-disk-tier.md) gets a `preStop` hook that deregisters the tier with the
leader and then waits out the grace. A group with no tier renders no hook and the setting is inert.

**With the example image above on a two-node cluster**, deregistration takes effect **at once**: a
peer reading a key that lives only on that tier gets a clean miss for the whole window rather than at
the end of it. Sizing this value so that in-flight peer reads can finish sizes it against something
that does not happen.

The same measurement shows the wait is unconditional rather than a drain — the process holds for the
full value even when nothing is still reading. What it buys is local time for the departing member to
finish what it is doing.

⚠️ Both readings are that image's behaviour, not this operator's guarantee. `spec.image` selects the
backend, and another image may deregister later or wait differently; what the operator controls is
the value it sends to the endpoint.

```yaml
spec:
  connection:
    managed:
      scaleIn:
        gracePeriodSeconds: 30       # 0..3600
```

**The Pod's termination window is derived from it**, as `gracePeriodSeconds + 60`, rather than being
a second field beside it. That is what makes the relationship hold: two independent fields could be
set so the kubelet kills the container in the middle of the wait, and no validation makes that
impossible — it only makes it checkable.

The upper bound of 3600 is the member endpoint's own; above it the call is refused with a `400`, so a
larger value would render a hook that fails every time it runs.

> **It does not make a shrink lossless.** The memory segment is still dropped, per the paragraph
> above, and the disk tier stops answering as soon as the hook runs. A reader gets a clean miss
> either way, which is the contract rather than a consolation.

Migrating a member's data before it leaves — the store's drain job API — is **not** offered here. It
is stateful orchestration, and it reaches only the memory and NVMe-oF replicas: it cannot name the
segments of the disk-backed ones and skips those keys without counting them as blocked, so **a drain
over a backend with a disk tier reports success while leaving that tier's data where it was** — and
its own success signal does not tell you that happened.

## The external mode

`connection.external` points at a backend somebody else runs. The operator creates **nothing** — no
Deployment, no Service, no DaemonSet — and only observes.

```yaml
  connection:
    external:
      endpoints:
        - {name: Client, address: mooncake.example:50051}
        - {name: Admin,  address: mooncake.example:9003}
```

Both roles are required, and each address is validated as `host:port` at admission — a blank or
portless one would otherwise be mirrored into status and handed to an engine that cannot dial it.
**`Admin` is what this operator reads** — health, metrics and the segment listing — and **`Client` is
what an inference engine connects to**. The addresses are mirrored into `status.endpoints` unchanged.

Two behaviours differ from the managed mode:

- ⛔ **An address that does not answer is reported as an `Error`, not as a backend still starting.** A
  managed leader is excused while its own Deployment has no ready replica; an external address was
  declared to name something that already runs, so there is no Pod to wait on and a mistyped endpoint
  would otherwise sit at `Provisioning` forever.
- `status.members[]` carries **no** node name or medium, because those Pods are not this operator's to
  look up. Capacity is the **sum of both pools**, since an external object names no medium to pick one
  by.
- ⛔ **A redirect from the `Admin` address is never followed.** That address belongs to whoever wrote
  the spec, and honouring a `3xx` from it would read some other host with the operator's network
  identity, then copy an excerpt of the answer into a status readable by anyone who can read the
  object. The redirect is reported as the response it is.

### Keeping two external objects off one leader is yours

⛔ **Two `KVCacheBackend` objects may name the same leader, and nothing in the operator notices.** For
a managed backend the object *is* the leader, so two objects are two leaders. For an external one the
object is a **declaration of addresses**, and one leader is reachable under more than one spelling —
by Service name in one object and by IP in another, with or without a trailing dot or an explicit
default port.

> **Why no check** — every identity the operator could compare is either editable or needs the leader
> reachable at admission. The comparison cheap enough to run — byte-identical addresses — catches the
> copy-paste case and misses the one a real deployment produces, which is the same leader spelled two
> ways. A check that catches the easy half invites the reader to trust it for the other half. This is
> tracked in [issue #288](https://github.com/gpustack/gpustack-operator/issues/288).

**Three consequences follow, and the third is the one to read before deciding the duplication is
safe.** The reuse-domain uniqueness rule is enforced between Bindings whose pools name the **same
backend object**, so two Bindings reaching one leader through two objects are both admitted on one
`domain.name`:

1. **The quota of a shared domain flips and never settles.** The leader keeps one ledger entry per
   tenant, and each pool's reconciler converges that entry toward its own Binding's `quotaCeiling`
   **on every pass**. Each pass reads the other's figure, finds it wrong, and writes its own back.
2. **The symptom of an undersized quota is a low hit rate and nothing else.** Exceeding a tenant's
   quota does not refuse the write: the store frees room by dropping that tenant's own older objects
   and retries, irreversibly and **without any counter moving**. So the flipping above never surfaces
   as an error — it surfaces as a cache that keeps losing content nobody asked it to lose.
3. ⛔ **Two Bindings on one `domain.name` with a different `blockSize` or `dtype` corrupt each
   other's blocks.** The reuse identity an engine is handed is the domain **name alone** —
   `blockSize` and `dtype` reach no engine; they are a declaration this API validates and records. So
   two differently-shaped caches land under one identity, which is
   [the silent cache pollution](../reference/model-deployment.md#the-reuse-domain-is-inherited)
   a wrong `blockSize` or `dtype` causes, reached here without either value being wrong.

⇒ If you point two objects at one leader, either keep their pools' Bindings on **different**
`domain.name` values, or make sure every Binding that shares a name also shares its `blockSize`,
`dtype` and `quotaCeiling`.

## Operating notes

**`replica_num` is engine-side.** How many replicas of a stored object Mooncake keeps is a per-`Put`
argument the caller supplies through its connector configuration. It is not controllable from this CR.

**One benign startup line, documented so nobody files it as a bug.** It appears on every client start
and is harmless:

```
E transfer_metadata.cpp:991] Local segment descriptor not found
```

**Client-side environment knobs**, observed at startup and set on the *workload*, not here:

```
MC_TE_METRIC=1                        enable transfer-engine metrics (OFF by default)
MC_STORE_CLIENT_METRIC_BANDWIDTH      client bandwidth summary
MC_STORE_MEMCPY                       unset => auto-detected ("TCP-only environment, memcpy enabled")
MC_METADATA_SERVER / P2PHANDSHAKE     the transfer engine's own low-level metadata knob
```

⛔ `MC_METADATA_SERVER` is **not** the variable this operator renders. The member is configured
through the store client's key, `MOONCAKE_TE_META_DATA_SERVER`. Two names for the metadata plane is
exactly the near-miss that gets one of them typed into a template.

**A backend in use cannot be deleted.** While `status.usedBy` names a consumer the finalizer holds,
the object stays at `phase: Deleting` with the claimant named in its message, and the workloads keep
running. Clearing the last claim lets the teardown complete.

**A backend in use also cannot have `leader.multiTenancy` turned off.** The webhook refuses the edit
while `status.usedBy` names a consumer, and the refusal names them. Remove the consumers first — see
[KV Cache Pool](pool.md#operating-notes) for what the withdrawal costs on their side.

> **Why** — the flag decides whether the master keeps a per-tenant ledger, and a consumer's quota is
> both written and released through that ledger. Withdrawing it under a live consumer therefore takes
> away the way that consumer is unwound, and the cost only appears when something is deleted. The
> refusal reads `status.usedBy` as it is written, not as it resolves: an entry naming an object that
> no longer exists refuses the edit too, and that list is where it is cleared.

**The teardown deletes the workloads first, and the object disappears last.** The leader Deployment,
its Service and every member DaemonSet go before the finalizer comes off — and it waits for them to
be **gone**, not merely for the deletes to be accepted, so the object going away means the backend is
gone rather than scheduled to be.

> **Why not leave it to ownership** — they are owned dependents, so the collector would reach them
> either way. But between the finalizer coming off and it running, the leader is still serving on an
> address nothing accounts for. Ownership is the safety net, not the mechanism.

**That wait is bounded, one workload at a time.** Each gets the termination grace its own Pod template
declares plus a couple of minutes, timed from its own deletion timestamp. Past that it stops being
waited for, and a warning Event on the workload names the nodes its Pods are still terminating on.
Deleting a backend therefore finishes even when a node it ran on has stopped answering.

> **Why its own grace and not one number** — a member group with a disk tier derives its Pod's grace
> from `scaleIn.gracePeriodSeconds`, which reaches an hour, so any constant short enough to bound an
> unreachable node would abandon a group draining exactly as configured. That grace works as the clock
> because the kubelet treats it as a hard kill deadline: a Pod outliving it is not a slow one, it is
> one whose kubelet is not acting.

**Only objects carrying this backend's own note are deleted.** The names are derived, so an unrelated
object can hold one, and a delete has to be surer than a name. The member sweep finds its DaemonSets
by the same note it then checks, rather than by the identity labels — discovering on one key and
judging on another is how an object goes missing from its own teardown.

---

**See also** — [KV Cache Leader](leader.md) (the metadata process, its probes and its
Lease election) · [KV Cache Local Disk Tier](local-disk-tier.md) (the optional disk layer on a member
group, and the bucket that is its write unit) · [KV Cache Pool](pool.md) (how a namespace is granted
a quota on this store, and what a quota ceiling buys) · [Admission](../architecture/admission.md) (the gates and the four-view status pattern) ·
[Settings & Environment Variables](../settings.md) (the `kv-cache-backend-image` Setting) ·
[Installation Modes](../architecture/installation-modes.md) (why the CRD is applied by the worker, not the chart)

**Next** → [Internals](../architecture/internals.md) — startup ordering and the invariants that fail silently.
