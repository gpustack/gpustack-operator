# KV Cache Backend

> **Purpose** — how a `KVCacheBackend` runs a Mooncake store, what its status is read from, and the
> three things that surprise operators: capacity is observed rather than derived, shrinking a group
> discards the cache that member held, and a local disk tier can be configured correctly and still
> hold nothing.
> **Audience** operators, contributors · **Prerequisites** [Architecture](../architecture.md) ·
> **Read time** ~20 min

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
- [The local disk tier](#the-local-disk-tier)
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
          medium: DRAM               # the one value: what this group's SEGMENT is made of
          capacityPerMember: 4Gi
```

`connection.managed` and `connection.external` are both optional pointers and **exactly one** must be
set; neither and both are refused at admission with a message naming the two. Several member groups
are allowed; at most one of them may carry a [local disk tier](#the-local-disk-tier).

**`members[].medium` has one value, `DRAM`**, and it is an identity rather than a choice: the group
says what it contributes, so a second medium widens the enum instead of being inferred from a field
that is not there. It is the same reason `spec.type` names `Mooncake` and nothing else.

An earlier shape offered five values. Four of them named things that are **not member groups**, and
each is reached another way:

| Was a `medium` value | What it actually is | Where it lives |
|---|---|---|
| `LocalDisk` | a tier on the members that already hold the memory replica | [`members[].localDisk`](#the-local-disk-tier) |
| `NoF` | an NVMe-oF target coordinate, registered once, with no node affinity and no Pod | no API surface; it is not a member group |
| `CXL` | a DAX device the **leader process** allocates from | `leader.extraArgs`: `enable_cxl`, `cxl_path`, `cxl_size` |
| `DFS` | a distributed filesystem the **leader process** allocates from | the leader's own environment, which this API does not render |

> **Why the shape matters more than the names** — the leader routes an offload task to the client
> holding the key's memory replica. A member group with no memory segment is therefore never chosen,
> so a group declared as "the disk one" would report its disk capacity to the leader and never
> receive a single write. The object would say one thing, the running member another, and nothing
> would report a fault.

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

⛔ **`members[].extraArgs` is the exception to the table above: it renders into the container's
argv**, as `-D key=value`, not into an environment variable. `leader.extraArgs` does the same on the
leader, as `-key=value`.

⛔ **Either way the value is world-readable.** Anyone who can read the Pod or its controller can read
it, and it stays there for the life of the object. **Do not put a credential in `extraArgs`.**

> **Why there is no check** — the operator renders no flag that carries a credential, so this field
> is the only way one arrives. Nothing refuses it at admission: a credential is not recognisable in a
> `map[string]string`, and a guess wrong in either direction would be worse than this sentence.

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

**The management port is fixed, and on `RDMA` it lands on the node.** A member serves its HTTP API on
`8080 + <group index>` — the first group on `8080`, a second group on `8081`. A `TCP` group holds that
port inside its own pod network namespace, but an `RDMA` group holds the host's, so on every node an
RDMA group selects that port must be free. Reserve one port from `8080` upward per member group.

> **Why it moves per group** — two RDMA groups whose node selectors both match one node place two
> host-network Pods on it. On a single fixed port only the first binds; the second runs, never passes
> readiness, and reports nothing about why.

**A member advertises its POD IP, and that is what a client dials.** The address becomes the host
half of the segment's `te_endpoint`, which `status.members[]` is joined against and which the engine
hands to clients. Rules written for the data plane therefore target pod addresses, not node ones.

> **Why not the node name** — the engine binds its data port inside the pod's network namespace.
> Measured on a two-node cluster: advertising the node name, a client pod got `ECONNREFUSED` against
> both that name and the node IP, and connected only on the pod IP. It costs no stability — the
> leader appends a port of its own to build the segment name and that port is fresh on every start,
> so the name never survived a restart anyway. On the RDMA path the pod holds the host's network
> namespace and this is the node's address regardless.

## The local disk tier

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
```

| what it renders | where |
|---|---|
| `MOONCAKE_OFFLOAD_ENABLED`, `MOONCAKE_OFFLOAD_FILE_STORAGE_PATH`, `MOONCAKE_OFFLOAD_TOTAL_SIZE_LIMIT_BYTES` | the member container |
| a `hostPath` volume and mount at `localDisk.path` | the member Pod |
| `-enable_offload=true`, `-offload_on_evict=true` | the leader's argv |
| a `preStop` hook, and a termination window derived from `scaleIn.gracePeriodSeconds` | the member Pod |

**A tier is a layer on a member group, never a group of its own** — see
[The two axes](#the-two-axes) for why the shape has to be this way.

### A configured tier can hold nothing

⛔ **A tier can be accepted, report its full capacity, and still hold no data — with nothing else on
the object looking wrong.** The member Pods are Ready, the leader logs the mount, and
`status.capacity` reports the size the tier declared, because that figure is **capacity, not usage**
(see [What status reports](#what-status-reports)).

**Read `master_allocated_file_size_bytes` before relying on the tier**, which is the one figure that
answers the question:

```console
$ kubectl exec -n gpustack-system deploy/<backend>-leader -- \
    python3 -c "import urllib.request;print([l for l in \
    urllib.request.urlopen('http://127.0.0.1:9003/metrics').read().decode().splitlines() \
    if l.startswith('master_allocated_file_size_bytes')])"
```

`master_allocated_file_size_bytes` is **bytes actually written to the tier**, so `0` on a tier you
expect to be filling means the data path is not working, whatever the rest of the object says.

What this project has and has not observed of that data path, and what would count as settling it,
is recorded in
[the spec](../../specs/2026-09-05-kv-cache-media-and-scaling.md#the-one-item-that-did-not-pass-no-byte-reached-the-disk)
and tracked in [issue #200](https://github.com/gpustack/gpustack-operator/issues/200).

### The directory has to exist, and be writable by the image's user

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

There is **no switch that makes the operator do this for you**, and that is an open decision rather
than a closed one.

> **Why** — an init container that created and `chown`ed the path would have to name a single uid,
> and the command above is the evidence against that: the uid is a property of the image, and
> `members[].image` can differ per group. The other spelling, `chmod 0777`, opens the directory to
> every process on the node. Both need a caveat attached, which is why neither ships. If you run one
> uid across your whole backend, you know something this API does not — that is what would settle it.

Five rules the path has to satisfy, all enforced at apply time:

- It must be **absolute**.
- It must not be the **root directory**.
- It **may not overlap `/dev/infiniband`** — equal to it, inside it, or containing it. A sibling such
  as `/dev/infiniband-cache` is fine.
- It **may not contain a `..` component**.
- It **may not begin or end with whitespace**, spaces and tabs alike.

> **Why** — the root directory would mount the node's whole filesystem into a third-party container.
> The RDMA transport mounts `/dev/infiniband` into this same container, and two mounts on one path
> are resolved by the kubelet with one shadowing the other, which nothing on the object would record;
> that rule holds whatever `spec.transport.protocol` says today, because the field is editable. The
> `..` rule mirrors the store's own, which refuses such a path before checking whether the directory
> exists. The whitespace rule exists because the path is mounted exactly as written, so a trailing
> space produces a different directory than the one an operator read on the screen.

⛔ **The tier is frozen once a group has it: it cannot be added to a running group, removed from one,
or moved to another `path`.** Members would have to restart to mount the directory, and whatever they
already wrote would stay on their nodes with nothing addressing it. `capacity` is the exception and
moves **either way** — raising or lowering it re-renders one variable, and the tier's contents survive
the restart that follows.

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

### What the tier costs that nothing accounts for

The `hostPath` is **not** counted into any resource request, and it cannot be: the kubelet's
ephemeral-storage accounting covers the container filesystem, `emptyDir` volumes and logs, never a
`hostPath`. A request against it would reserve a figure nothing polices and would keep the member off
the very node that has the disk.

**Watching that filesystem is yours.** `localDisk.capacity` renders the store's own ceiling, which is
the only bound on what the tier writes; nothing in Kubernetes will evict or throttle the member when
the node's disk fills.

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

**Capacity is observed, not derived.** `status.capacity` is read from the leader's own counters,
never from what the spec declares — nothing multiplies `capacityPerMember` by a replica count. A
backend with no disk tier reads `master_total_capacity_bytes`; one **with** a tier reads that plus
`master_total_file_capacity_bytes`. Adding two **observed** families is not the same thing as
adding up what members were asked to provide.

**`status.capacity.total` is capacity, not usage**, and a disk tier contributes the ceiling the
member declared — published as soon as the member registers, before anything is written there.

To ask whether the **disk** tier is holding data, the figure to read is not on the CR at all — see
[A configured tier can hold nothing](#a-configured-tier-can-hold-nothing).

⛔ **Capacity is absent — not zero — while the leader is starting.** `/metrics` is ungated: a leader
that is up but not serving answers 200 with a well-formed exposition whose gauges all read zero, and a
zero is indistinguishable at the parser from a genuinely empty cache. Publishing is therefore gated on
`service_ready`, not on the scrape succeeding.

`status.members[]` is read from the leader's segment listing, one entry per **listed** segment. The
leader is what allocation goes through, so a running member Pod it does not list holds nothing and is
counted in `MembersMounted`'s message instead. The two fields the listing cannot supply — node name
and medium — are joined in from the member Pod behind that segment, and left **empty** rather than
guessed when nothing matches.

⛔ **Two member Pods behind one address cannot be told apart, and the status reports that.** A segment
is named by an address plus a transfer port bound at random, which no Pod carries, so a segment
arriving on an address two **ready** members share traces to neither. `MembersMounted` goes `False`
with `AmbiguousMemberIdentity`, naming the shared key and the Pods; the node and medium stay empty.
Give the groups node selectors that keep them on different nodes.

Two groups on one node are **not** ambiguous by themselves. A `TCP` member advertises its own pod IP,
so each segment carries a distinct address even though both Pods answer to the node's name; the
condition is raised only for a shared address a segment actually arrives on, which is the `RDMA` case
where both Pods hold the host's network namespace.

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

⛔ **Shrinking a group discards the cache that member held.** Narrowing the selector, or removing a
node, unmounts that member's segment **immediately** — there is no drain.

> **Why it is not drained** — the member's own API does take a graceful unmount with a grace period,
> but it requires the segment ids and **no route returns a client its own**. The name is not
> derivable either: the leader appends a port of its own choosing and that port is fresh on every
> start. The `terminationGracePeriodSeconds` the operator sets lets the entrypoint finish its own
> shutdown — it does not preserve the data.

**`scaleIn.gracePeriodSeconds` reaches the disk tier only.** A member with a
[local disk tier](#the-local-disk-tier) gets a `preStop` hook that deregisters the tier with the
leader and then holds for the grace, so offload reads a peer already asked for finish there instead
of failing. A group with no tier renders no hook and the setting is inert.

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
> above. What the grace covers is the tier's deregistration, so a reader gets a clean miss rather
> than a peer that is about to disappear.

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
