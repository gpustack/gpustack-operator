# KV Cache Leader

> **Purpose** — the metadata process a `KVCacheBackend` runs: the Deployment and Service it renders,
> why its two probes take different paths, and what electing through a Kubernetes Lease costs.
> **Audience** operators, contributors · **Prerequisites** [KV Cache Backend](backend.md) ·
> **Read time** ~7 min

This API says **leader**; the artifact says **master**, and every rendered flag, environment variable
and metric keeps the vendor's spelling.

## Contents

- [The Deployment and the two probes](#the-deployment-and-the-two-probes)
- [High availability](#high-availability)

## The Deployment and the two probes

The leader is a Deployment plus a ClusterIP Service publishing two ports — `50051` for engine clients
and `9003` for the admin surface, which serves the Prometheus exposition and the HTTP admin API on one
port.

**`extraArgs` exists to reach settings this API does not name, so what may be set through it is not
enumerated.** An upstream flag can also reach a setting this API DOES name, under a different name.
The object then goes on reporting the value it rendered while the process runs the other one, and
nothing reports the divergence. A key whose effect overlaps a field of this spec is a trade-off the
administrator makes.

The CXL switch was one instance of exactly that shape, and the webhook now refuses it by name — the
`CXL` row in [KV Cache Backend](backend.md) carries which keys and why. It is recorded as the shape
to expect, not as a live hazard.

⛔ **`port` is refused in `leader.extraArgs`, and it would have moved nothing.** It is the store's
deprecated spelling of `rpc_port`, which this operator always renders, so the rendered one wins: the
key reads as a port that moved without moving one.

**`metrics_host` is reachable on purpose, and on an IPv6 cluster it is required.** It moves the
**address** the admin surface binds to, not the port. The store's default `0.0.0.0` is IPv4 only, so
where the Pod's address is IPv6 nothing answers the probes and the leader never becomes ready; `::`
listens on IPv6 and, on a dual-stack host, on both.

⛔ **Any other concrete address breaks both probes**, because the kubelet reaches them at the Pod's
own. The leader then stays not-ready rather than reporting the cause, so `0.0.0.0` and `::` are the
only two values worth setting.

**`default_kv_lease_ttl` is reachable on purpose, and it is the one flag this operator renders and
still lets you override.** A lease protects a cached object from eviction, and the store's own ten
seconds expires before a second engine replica asks for a block the first one just read — so the
operator renders `5m` instead. An entry in `leader.extraArgs` renders after it, and wins.

A lease is granted when an object is **read**, never when it is written, so this value protects the
recently-read set rather than everything written. Raise it where replicas share prefixes over longer
turns; the cost arrives only once the set read within the window outgrows the store itself.

`leader.extraEnv` is the same hatch for the environment: every entry renders after the derived
variables, and a name the renderer derives — the pod's own identity variables and the snapshot path —
is refused with the same message a member gets. The schema keys the list by `name`, so one name
cannot carry two values.

`replicas` defaults to `1`, and `5` is the ceiling in the **webhook** and in the schema alike: only
one leader ever serves, so further replicas are spare processes rather than capacity. More than one
requires [`highAvailability`](#high-availability) and is refused by the webhook without it, naming
the field that is missing. An enum would answer `Unsupported value: 2` and teach nothing.

⛔ **Without `highAvailability` the Deployment runs one replica whatever `replicas` says.** The
webhook refuses that combination, but a schema cannot express a cross-field rule — so where the
webhook is not installed this clamp is what keeps unelected masters off one pool.

**`highAvailability` with one replica is inert: the election exists only above one replica.** A
single process has nothing to elect between, so no election flag, Lease or API token is rendered
until `replicas` rises past 1 — set the field up front and a later scale-up is a one-field change.

The gate is re-evaluated on every reconcile, not decided at create: crossing `replicas: 1` in
either direction flips the election on or off, and the flip restarts the leader and rolls every
member, so the store's cached contents do not survive the crossing.

**The update strategy follows the replica count, and the two cases are opposites.** At one replica
the Deployment uses `Recreate`: an update stops the old master before starting the new one, so expect
a gap with no master on every image or flag change. Members keep their segments across it and
re-register.

> **Why** — `RollingUpdate`'s `maxSurge` defaults to 25% and rounds *up*, which against one replica
> is one: the default strategy would run two masters at once on every update, which is exactly what
> the single replica exists to prevent.

Above one replica it rolls instead — `maxSurge: 1`, `maxUnavailable: replicas` — because `Recreate`
would take every standby down together with the leader and leave nothing to elect.

> **Why `maxUnavailable` is not `replicas-1`** — the Deployment controller removes an old Pod only
> while more replicas are available than `replicas - maxUnavailable`. Exactly one is ever available
> here, so `replicas-1` makes that `1 > 1`: the old leader is never removed, the new replicas cannot
> become ready until it releases the Lease, and the rollout stalls for good.

⛔ **A rollout still has a window with no serving master**, and high availability shortens it rather
than removing it. A floor of zero available replicas is what lets the old leader go, so it can go
before a replacement has taken the Lease. The window is bounded by the lease expiry plus activation,
not by a Pod start — the replacements are already running as standbys, contending for it.

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

A single leader reports `service_ready: true` from its first answer, because the non-HA path sets it
unconditionally three lines after the admin server starts. Under high availability it is the standby
marker, and the readiness gate above is what turns it into an endpoint decision.

## High availability

Set `leader.highAvailability` and the leader elects through a **Kubernetes Lease** — once `replicas`
exceeds one; below that the election is inert (see above). The election itself needs no settings —
the Lease carries the leader's own object name, `<backend>-leader`, in this operator's namespace —
so an empty block is the switch:

```yaml
spec:
  connection:
    managed:
      leader:
        replicas: 3
        highAvailability: {}
```

⛔ **A published `kvcacheai/mooncake` image cannot do this, on either side.** Leadership backend
availability is a compile-time switch and every option ships **off**:

| role on a published image | what it does |
|---|---|
| leader | answers `UNAVAILABLE_IN_CURRENT_MODE`, runs as a permanent standby |
| member | answers `Invalid HA backend entry`, exits, CrashLoopBackOffs |

Use an image built from [`pack/mirrored-mooncake`](../../pack/mirrored-mooncake/Dockerfile) for
`spec.image` **and for every `members[].image`**, on Mooncake 0.3.12 or later: an electing leader is
also rendered `-pod_name` and `-pod_namespace` to label the winner, and a 0.3.11 master exits on both.

A lease-less image is not refused outright: at one replica the election flags are never rendered, so
such an image runs a single-leader backend even with `highAvailability` set — the flags arrive only
when `replicas` rises past 1, which is where the missing backend would fail the leader at startup.

⛔ **A member group on `RDMA`, `ROCM` or `CANN` cannot run under high availability today.** Those
transports need a vendor runtime `mirrored-mooncake` does not carry, and the vendor build does not
carry the leadership backend — the two axes are independent, so covering them means rebuilding each
variant.

`MUSA` and `MACA` are not on that list because this project builds no variant for either, by
intent: a group on one of them runs an image you built, so whether it also carries the leadership
backend is a property of your build rather than of anything here.

`EFA` is the one fabric not on that list: it needs no vendor runtime, only libfabric, so
`mirrored-mooncake` compiles it in — the image build proves the transport installed by running a
target-mode bench told `--protocol=efa` and refusing the transport map's "Invalid protocol": the
device-less builder's "No EFA devices found" and an EFA-capable builder's clean run both pass. An
`EFA` member group runs under high availability, on nodes that have the AWS EFA driver installed.

Tracked at [issue #279](https://github.com/gpustack/gpustack-operator/issues/279), together with the
alternative of leaving members on the leader Service address and letting readiness move the endpoint.

⛔ **`enable_oplog` is refused in `leader.extraArgs`**, and not as a policy choice: the store's
operation log requires the etcd backend, which cannot be compiled together with the Lease backend, so
the flag produces a leader that refuses to start. Standbys rebuild from member remounts alone
instead.

⛔ **`etcd_endpoints` is refused as well, and it is out of reach twice over.** The store reads it only
where `ha_backend_connstring` is empty, which the election never leaves empty, and the etcd backend it
names is the one this image cannot carry.

⛔ **`rpc_address` and `rpc_interface` are refused there too**, because the election renders the
first. The store folds it with the RPC port into the string it campaigns with, so that one value is
both the election's identity and the address written into the Lease for members to dial. Each replica
advertises **its own Pod IP**; a value supplied by hand would point every member at one host, chosen
without knowing whether the Pod answers there.

**The healthy steady state reads `N desired / 1 ready`.** Exactly one leader serves; the rest are
standbys, and a standby is deliberately **not ready** — that is what keeps it out of the leader
Service's endpoints, so an engine never connects to a process that cannot serve. To anyone who has
not been told, `3/1` is what a broken Deployment looks like. It is not. `kubectl get deploy` during a
healthy failover briefly shows `2` ready as the old leader steps down; both readings are normal.

> **Why there is no "who is the leader" field** — the store labels its own Pod
> `mooncake.io/store-role=leader` once it wins, so `kubectl get pod -l mooncake.io/store-role=leader`
> answers it. This operator does not read the admin API to re-report it, because failover is the
> client's business and a second opinion could disagree with the first.

**Both roles get a ServiceAccount, and they are different accounts.** The operator renders a
`ServiceAccount`, `Role` and `RoleBinding` per role in its own namespace, and names them on the Pods:

| role | grant | why |
|---|---|---|
| leader | `leases`: `create`, `get`, `update`; `pods`: `patch` | takes the Lease, and labels its own Pod |
| member | `leases`: `get` | follows an explicit `Lease` address, and nothing more |

The member's is narrower on purpose: a shared account would let any member take the Lease from the
leader it is following.

**A member's `MOONCAKE_MASTER` defaults to the leader Service address.** A standby is not ready, so
the Service resolves to the serving leader. With `memberAddressing: Lease` and an election running,
the member instead gets `k8s://<namespace>/<lease>` and reads the current holder itself. Without an
election, both values render the Service address.

| value | what a member is given | what it pays |
|---|---|---|
| `Lease` | the Lease's coordinates, read by the client itself | the member must reach the API server, so its image must carry the leadership backend |
| `Service` (default) | `<backend>-leader.<namespace>.svc:50051` | endpoint propagation after an election |

In one cluster failover comparison the forms differed by 0.13 seconds, within the noise of one run.
Both first failed at 31.41 seconds and converged around 60.6 seconds, so election dominated the
result; retest if election timing changes. The Service endpoint transition was inferred from the
result, not observed directly. Changing the value rolls every member group when HA is active.

**Several leaders shorten the outage; they do not keep the cache.** A standby holds no data. The
replica that takes over, like a single leader that restarts, learns the members' segments from their
remounts and none of the keys in them, so every object held in member memory misses until it is
written again.

The exception is what a member's [local disk tier](local-disk-tier.md) already holds: when the new
leader does not know the disk segment, the member registers it again together with the objects on it.

What a second replica buys is time. On a single-node test cluster a failover left the store
unusable for about 16 seconds and a single-leader restart for about 30; on a real cluster a single
leader also waits for its replacement to be scheduled and its image pulled.

**Run one leader by default.** Add replicas when that gap costs more than what an election needs: the
store image and the two accounts described above.

⛔ **`leader.highAvailability.snapshot` is refused at admission, at any replica count.** A snapshot
records where each key sits in member memory, and restoring one does not check that the memory still
holds that key. Once the memory has been reused, the restored index hands out another key's bytes
instead of a miss — to an engine, a wrong KV block rather than a cold one.

Two ordinary events reuse it. A forced remove, which is how an engine resets its cache, frees it
before the next snapshot is taken. A standby loads the snapshot once at its own start, so by the time
it takes over, another leader may have given that memory to other keys.

An object admitted with the field before the refusal keeps running as it was rendered, and an update
to it is refused only when it moves the field or `replicas`. Removing the field is always accepted.

⛔ **A claim such an object wrote to outlives it, and every key in its cache is nameable from it.** A
snapshot is the master's metadata written as plain bytes with no encryption, including tenant names
under multi-tenancy. Nothing here deletes the claim.

⛔ **`memory_allocator` is refused in `leader.extraArgs` because of this feature**, under a name that
mentions no part of it: the store builds its snapshot manager only under its default allocator,
silently and with no log line either way. Any other value would leave the flags rendered, the claim
mounted and this object stating a snapshot that is never written again.

**A missing grant fails differently on each side, and one of them is silent.** A leader that cannot
reach the Lease retries every second forever — liveness is ungated, so nothing restarts and the
Deployment sits at `0/N` ready with no message naming the cause. A member that cannot read it gives
up after twenty tries and CrashLoopBackOffs. Check both accounts exist before reading `0/N` as a
store problem.

---

**See also** — [KV Cache Backend](backend.md) (the object this leader belongs to, its members, and
what status reports) · [KV Cache Local Disk Tier](local-disk-tier.md) (the disk tier, whose offload
flags this leader derives from the members' declaration) · [High Availability Operations](../operation/high-availability.md) (the replica knob
per control-plane component) · [Settings & Environment Variables](../settings.md) (the
`kv-cache-backend-image` Setting)

**Next** → [KV Cache Backend](backend.md) — the members this leader serves, and the status it is read
from.
