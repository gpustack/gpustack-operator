# KV Cache Backend

> **Purpose** — how a `KVCacheBackend` runs a Mooncake store, what its status is read from, and the
> two things that surprise operators: capacity is observed rather than summed, and shrinking a group
> discards the cache that member held.
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
- [The leader](#the-leader)
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
          medium: DRAM               # schema: DRAM | LocalDisk | NoF | CXL | DFS — only DRAM runs
          capacityPerMember: 4Gi
```

`connection.managed` and `connection.external` are both optional pointers and **exactly one** must be
set; neither and both are refused at admission with a message naming the two. This scope reconciles
**one** member group — a second is schema-valid and webhook-refused, naming the tiering follow-on, so
a shape that is not yet reconciled fails at admission rather than half-way through a reconcile.

The medium axis works the same way. The schema names all five media the store supports, so a tiered
backend will not have to change the shape, but **only `DRAM` is reconciled** and the other four are
refused at admission naming what would have to render them: the leader's file or DAX flags, and a
mount on the member.

> **Why refuse rather than accept and approximate** — a `DFS` group that was admitted would come up
> holding its segment in host memory, while `status.capacity` read the leader's *file* gauges and
> reported zero. The object would say one thing, the running member another, and nothing would
> report a fault. A refusal at apply time is the only version of that an operator can act on.

The object is **cluster-scoped**: it names nodes, claims host memory and host paths, and on the RDMA
path needs `hostNetwork` and `/dev/infiniband`. Only a cluster administrator can legitimately declare
one.

Tenant isolation is a different axis, handled one layer up — exactly as Kueue separates `ClusterQueue`
from `LocalQueue` (<https://kueue.sigs.k8s.io/docs/concepts/>). One backend can be referenced by
several pools, which is the only reason a backend and a quota domain are separate objects.

## The image

**`spec.image` is explicit and never derived from the operator's own image**, which breaks
deliberately with how the Device Manager image is derived from the worker image. Leave it unset and
the cluster-wide `kv-cache-backend-image` Setting supplies it; unset in both places is refused at
admission, naming both.

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

Nothing has to be built to run this. `docker.io/kvcacheai/mooncake:0.3.13` is published for amd64 and
arm64 and carries **both** `mooncake_master` and `mc_store_rest_server`, so one `spec.image` serves the
leader and the members. It runs on a host with no GPU: its `libcuda.so.1` is a stub and its
`libcudart.so.12` is the real library, and neither reaches a driver.

> **Why the stub/real split matters** — the stub alone is enough for the master, but the Python client
> needs a versioned `cudaFreeHost` from a real runtime. An image carrying two stubs runs the master and
> fails every member.

**That split is also why the `kv-cache-backend-image` Setting ships blank**, rather than pinned to the
image above. One value would have to be right for every backend in the cluster at once, and which
build a member needs depends on the transport its backend asks for and the hardware its group selects.
Unset, a mismatch is an admission refusal naming both places to fix; defaulted, it is a loader error
at runtime.

**A private registry needs `spec.imagePullSecrets`**, and an explicit policy needs
`spec.imagePullPolicy`. Both are backend-wide: they apply to the leader and to every member group,
including a group that names its own `image`. Left unset, the policy is **resolved from the image
tag by the same rule the API server would have applied** — `Always` for `:latest` or no tag,
`IfNotPresent` otherwise — and it is re-resolved whenever the image or the field moves.

> **Why they are fields and not Settings** — the cluster-wide `image-pull-policy` and
> `image-pull-secrets` Settings are values of the bundled-application chart install. They reach the
> subcharts and nothing a controller renders, so a `KVCacheBackend` that inherited them would be the
> only object in this API whose running workloads move when a chart value moves. Neither role runs
> under a service account of ours carrying credentials either, so without these fields no image here
> could come from a private registry at all.

## The metadata plane

**The metadata plane is peer-to-peer and has no API field.** The member's `metadata_server` renders as
the literal `P2PHANDSHAKE`, unconditionally. A single-leader backend therefore has **zero external
dependencies beyond its image** — no etcd, no Redis, nothing to deploy alongside it.

Two axes get confused here, so both are stated. The metadata plane is how clients find one another.
The **HA backend store** — `-enable_ha` with `-ha_backend_type` — is how several leader replicas elect
one among them, and that is where a Kubernetes lease would live. It exists only at `replicas > 1`,
which this scope refuses.

⛔ **A manifest that tries to configure the metadata plane is not refused with a helpful message.**
There is no field, so there is nothing for a webhook to see:

- a strict client — `kubectl apply`'s default — is refused by the schema with
  `strict decoding error: unknown field "spec.metadata"`;
- a client with validation turned off has the block **silently pruned**, and the object is admitted
  and reconciled as though nothing had been written.

The second is indistinguishable from success at the point of apply. This section is the protection
against it: the metadata plane takes no configuration at all.

## The leader

The leader is a one-replica Deployment plus a ClusterIP Service publishing two ports — `50051` for
engine clients and `9003` for the admin surface, which serves the Prometheus exposition and the HTTP
admin API on one port.

`replicas` defaults to `1` and anything larger is refused by the **webhook**, naming the leader
high-availability follow-on. An enum would answer `Unsupported value: 2` and teach nothing.

**The Deployment uses `Recreate`, so an update stops the old master before starting the new one.**
Expect a gap with no master on every image or flag change; members keep their segments across it and
re-register.

> **Why** — `RollingUpdate`'s `maxSurge` defaults to 25% and rounds *up*, which against one replica
> is one: the default strategy would run two masters at once on every update, which is exactly what
> the single replica exists to prevent.

**The two probes deliberately take different paths**, and this is the one configuration detail on this
page that must not be "simplified":

| probe | path | gated? |
|---|---|---|
| readiness | `GET /get_all_segments` | yes — 503 until the service plane is active |
| liveness | `GET /health` | **no**, and it must not be |

`/health` answers 200 in every state, so using it for readiness is the same as having no readiness
probe. Using a gated route for **liveness** would kill a leader that is slow to activate.

The health document has four fields that matter:

```json
{"status":"ok","role":"leader","ha_state":"serving","service_ready":true}
```

⛔ **`status` is a hard-coded constant.** It reads `"ok"` on a leader that is serving nothing.
**`service_ready` is the only verdict in the document**, and every readiness decision rests on it.

> **Scope note** — a single leader reports `service_ready: true` from its first answer, because the
> non-HA path sets it unconditionally three lines after the admin server starts. The gate is what HA
> will need and what keeps a starting leader's zeroed metrics from being published; it is not a phase
> this scope will show anyone.

## The members

One member group renders **one DaemonSet** over `members[].nodeSelector`. A member contributes *a
node's* medium — that node's host memory, host paths, and on the RDMA path its `/dev/infiniband` — so
its identity is the node, which is what a DaemonSet expresses.

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

`spec.transport.protocol` accepts `Auto`, `TCP`, `RDMA`, `HIP` and `Ascend`, and defaults to `Auto`
whether or not the `transport` block is written at all. **`Auto` resolves to `TCP`** — it is not a
per-node probe that promotes itself.

> **Why** — one group is one Pod template, which cannot express a per-node transport; and promoting to
> RDMA would mean granting `hostNetwork` plus `IPC_LOCK` and `SYS_RESOURCE`. A privilege is requested,
> never inferred. Naming `RDMA` is also what accepts the security context that comes with it — which
> is those three things and **not** `privileged`. A `TCP` group sets none of them.

**Reachability is a port range, never a list.** The transfer engine picks its data ports at random —
one observed run bound `15002` and `15995`, a second client `16566` and `16655`, none of them
configured — and the peer-to-peer plane is what binds them. Write firewall and NetworkPolicy rules
between member nodes, and from engine clients, as a **range**. The rendered Pod declares no fixed
data-plane `containerPort`, because a fixed list would be a false statement.

**A member advertises its POD IP, and that is what a client dials.** The address becomes the host
half of the segment's `te_endpoint`, which `status.members[]` is joined against and which the engine
hands to clients. Rules written for the data plane therefore target pod addresses, not node ones.

> **Why not the node name** — the engine binds its data port inside the pod's network namespace.
> Measured on a two-node cluster: advertising the node name, a client pod got `ECONNREFUSED` against
> both that name and the node IP, and connected only on the pod IP. It costs no stability — the
> leader appends a port of its own to build the segment name and that port is fresh on every start,
> so the name never survived a restart anyway. On the RDMA path the pod holds the host's network
> namespace and this is the node's address regardless.

## What status reports

```console
$ kubectl get kvcb
NAME            TYPE       PHASE   ENDPOINT                                        CAPACITY
mooncake-dram   Mooncake   Ready   mooncake-dram-leader.gpustack-system.svc:50051  12Gi
```

Five phases — `Provisioning`, `Ready`, `Degraded`, `Error`, `Deleting`. `Ready` carries no
`phaseMessage`; every other phase carries one. Four conditions report the axes: `LeaderAvailable`,
`MembersMounted`, `CapacityObserved` and `Deletable`.

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

**Capacity is observed, never summed.** `status.capacity` is read from the leader's own counters —
`master_total_capacity_bytes` for a memory medium, `master_total_file_capacity_bytes` for a file one.
Nothing multiplies `capacityPerMember` by a replica count.

⛔ **Capacity is absent — not zero — while the leader is starting.** `/metrics` is ungated: a leader
that is up but not serving answers 200 with a well-formed exposition whose gauges all read zero, and a
zero is indistinguishable at the parser from a genuinely empty cache. Publishing is therefore gated on
`service_ready`, not on the scrape succeeding.

`status.members[]` is read from the leader's segment listing, one entry per **listed** segment. The
leader is what allocation goes through, so a running member Pod it does not list holds nothing and is
counted in `MembersMounted`'s message instead. The two fields the listing cannot supply — node name
and medium — are joined in from the member Pod behind that segment, and left **empty** rather than
guessed when nothing matches.

A failed listing scrape **keeps** the previous list and sets `MembersMounted=False`; a failed capacity
scrape **clears** the figures. That asymmetry is deliberate: capacity is two pointers and has an
"absent" that means *not observed*, while an empty list is a legible value meaning *no segments*, so
clearing it would publish a falsehood.

## Growing and shrinking a group

**Widening `members[].nodeSelector` adds members without restarting the ones already running.** The
DaemonSet places a Pod on each newly matching node; every existing Pod keeps its UID and its restart
count, leader included.

> **Why it needs `OnDelete`** — `nodeSelector` lives in the Pod template, so under the default update
> strategy widening it would roll **every** member. The DaemonSet is therefore left on `OnDelete`, and
> the operator decides restarts itself from a fingerprint over the whole template **except** the node
> selector. A widening moves no fingerprint; an image, argv, environment, resource or fabric change
> moves it and every member is recreated.

**Existing objects are not rebalanced.** How fast the cluster converges onto a new member depends on
`allocationStrategy`: `FreeRatioFirst` (the default) biases new writes toward the emptier member,
`Random` does not.

⛔ **Shrinking a group discards the cache that member held.** Narrowing the selector, or removing a
node, unmounts that member's segment **immediately** — there is no drain.

> **Why it is not drained** — nothing reachable from a Pod's shutdown can make it graceful.
> `store.close()` takes no grace argument, `unmount_segment`'s `grace_period_seconds` defaults to `0`
> and `0` takes the immediate path, and no console script in the image can issue a graceful unmount at
> all. The `terminationGracePeriodSeconds` the operator sets lets the entrypoint finish its own
> shutdown — it does not preserve the data.

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

**The teardown deletes the workloads first, and the object disappears last.** The leader Deployment,
its Service and every member DaemonSet go before the finalizer comes off — and it waits for them to
be **gone**, not merely for the deletes to be accepted, so the object going away means the backend is
gone rather than scheduled to be.

> **Why not leave it to ownership** — they are owned dependents, so the collector would reach them
> either way. But between the finalizer coming off and it running, the leader is still serving on an
> address nothing accounts for. Ownership is the safety net, not the mechanism.

**Only objects carrying this backend's own note are deleted.** The names are derived, so an unrelated
object can hold one, and a delete has to be surer than a name. The member sweep finds its DaemonSets
by the same note it then checks, rather than by the identity labels — discovering on one key and
judging on another is how an object goes missing from its own teardown.

---

**See also** — [KV Cache Pool](pool.md) (how a namespace is granted a quota on this store, and what a
quota ceiling buys) · [Admission](../architecture/admission.md) (the gates and the four-view status pattern) ·
[Settings & Environment Variables](../settings.md) (the `kv-cache-backend-image` Setting) ·
[Installation Modes](../architecture/installation-modes.md) (why the CRD is applied by the worker, not the chart)

**Next** → [Internals](../architecture/internals.md) — startup ordering and the invariants that fail silently.
