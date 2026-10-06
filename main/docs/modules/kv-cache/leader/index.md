# KV Cache Leader

A `KVCacheBackend` runs a leader process for cache metadata. The operator manages its Deployment,
Service, probes and Kubernetes Lease election.

This API says **leader**; the artifact says **master**, and every rendered flag, environment variable
and metric keeps the vendor's spelling.

## Contents

- [The Deployment and the two probes](#the-deployment-and-the-two-probes)
- [High availability](#high-availability)

## The Deployment and the two probes

The leader is a Deployment plus a ClusterIP Service publishing two ports (`50051` for engine clients
and `9003` for the admin surface, which serves the Prometheus exposition and the HTTP admin API on one
port).

**`extraArgs` exists to reach settings this API does not name, so this page does not enumerate what
may be set through it.** An upstream flag can also reach a setting this API *does* name, spelled
differently. The object then reports the value the operator rendered while the process runs the
value from `extraArgs`, and nothing reports the divergence. Accepting that gap is the
administrator's trade-off.

The CXL switch was one instance of exactly that shape, and the webhook now refuses it by name. The
`CXL` row in [KV Cache Backend](/gpustack-operator/main/docs/modules/kv-cache/backend/index.md) carries which keys and why. It is recorded as the shape
to expect, not as a live hazard.

**`port` is refused in `leader.extraArgs`, and it would have moved nothing.** It is the store's
deprecated spelling of `rpc_port`, which this operator always renders, so the rendered one wins: the
key reads as a port that moved without moving one.

**`metrics_host` is reachable on purpose, and on an IPv6 cluster it is required.** It moves the
**address** the admin surface binds to, not the port. The store's default `0.0.0.0` is IPv4 only, so
where the Pod's address is IPv6 nothing answers the probes and the leader never becomes ready; `::`
listens on IPv6 and, on a dual-stack host, on both.

**Any other concrete address breaks both probes**, because the kubelet reaches them at the Pod's
own. The leader then stays not-ready rather than reporting the cause, so `0.0.0.0` and `::` are the
only two values worth setting.

**`default_kv_lease_ttl` is reachable on purpose, and it is the one flag this operator renders and
still lets you override.** A lease protects a cached object from eviction, and the store's own ten
seconds expires before a second engine replica asks for a block the first one just read. The
operator renders `5m` instead. An entry in `leader.extraArgs` renders after it, and wins.

A lease is granted when an object is **read**, never when it is written, so this value protects the
recently-read set rather than everything written. Raise it where replicas share prefixes over longer
turns; the cost arrives only once the set read within the window outgrows the store itself.

`leader.extraEnv` is the same hatch for the environment: every entry renders after the derived
variables, and a name the renderer derives (the pod's own identity variables) is refused with the
same message a member gets. The schema keys the list by `name`, so one name cannot carry two values.

`replicas` defaults to `1`, and `5` is the ceiling in the **webhook** and in the schema alike: only
one leader ever serves, so further replicas are spare processes rather than capacity. More than one
requires `electionBackend: Kubernetes`; the webhook refuses `None` at that count.

**With `electionBackend: None` the Deployment runs one replica whatever `replicas` says.** The
webhook refuses a larger count, and this clamp also protects clusters without the webhook.

**`electionBackend` defaults to `Kubernetes`, even at one replica.** The first leader campaigns for
a Lease and starts with its election flags and API token. Raising `replicas` from one to three adds
standbys without changing that leader's Pod template or rolling the members.

Set `electionBackend: None` only for a single leader whose image cannot run the Kubernetes Lease
backend. Switching between `None` and `Kubernetes` changes the leader and member Pod templates, so
it restarts them and loses their DRAM cache contents.

Increasing the leader count normally takes one update. If the live Deployment still has an
unelected template, the operator first enables election at one replica, then adds standbys after
the template rolls out. Until that step completes, `RolloutComplete` reports
`False/ReplicasPending`.

Returning to one replica leaves election enabled unless you set its backend to `None`.

**The update strategy follows the replica count, and the two cases are opposites.** At one replica
the Deployment uses `Recreate`: an update stops the old master before starting the new one, so expect
a gap with no master on every image or flag change. Members keep their segments across it and
re-register. Above one replica it rolls instead, with `maxSurge: 1` and `maxUnavailable: replicas`,
because `Recreate` would take every standby down together with the leader and leave nothing to elect.

**A rollout still has a window with no serving master**, and standby replicas can shorten it.
A floor of zero available replicas is what lets the old leader go, so it can go before a replacement
has taken the Lease. The window depends on election and activation; replacements already running as
standbys do not have to wait for scheduling or image pulls.

**The two probes deliberately take different paths.**

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

**`status` is a hard-coded constant.** It reads `"ok"` on a leader that is serving nothing.
**`service_ready` is the only verdict in the document**, and every readiness decision rests on it.

With `electionBackend: None`, a single leader reports `service_ready: true` from its first answer.
Under the default Kubernetes election, readiness waits for the process to win the Lease.

## High availability

The default `leader.electionBackend: Kubernetes` elects through a **Kubernetes Lease** at every
replica count. The Lease is named `<backend>-leader` in the operator's namespace. Scale an already
elected leader by changing only its count:

```yaml
spec:
  connection:
    managed:
      leader:
        replicas: 3
```

`leader.memberAddressing` is optional and only selects how members find the
winner. It does not turn the election on. With `electionBackend: None`, members
use the leader Service even if `memberAddressing: Lease` is set.

**A published `kvcacheai/mooncake` image cannot do this, on either side.** Leadership backend
availability is a compile-time switch and every option ships **off**:

| Role | Behavior |
|---|---|
| leader | answers `UNAVAILABLE_IN_CURRENT_MODE`, runs as a permanent standby |
| member | answers `Invalid HA backend entry`, exits, CrashLoopBackOffs |

Use an image built from [`pack/mirrored-mooncake`](https://github.com/gpustack/gpustack-operator/blob/e038ceb7fa2c4243ee790b2ca0060824fe5742af/pack/mirrored-mooncake/Dockerfile) for
`spec.image` **and for every `members[].image`**, on Mooncake 0.3.12 or later: an electing leader is
also rendered `-pod_name` and `-pod_namespace` to label the winner, and a 0.3.11 master exits on both.

A lease-less image can run one leader with `electionBackend: None`. It cannot serve a backend with
multiple leader replicas. Set `None` when creating a backend with such an image; an omitted field
selects `Kubernetes` and renders election flags that the image cannot use.

A member group on `RDMA`, `ROCM` or `CANN` runs under high availability on the build that carries
its transport: every `mirrored-mooncake` target (the default build and the `cuda`, `cann` and
`rocm` variants alike) compiles the Lease backend in. The vendor axis and the leadership axis are
orthogonal.

The matching rule is unchanged from [the backend page](/gpustack-operator/main/docs/modules/kv-cache/backend/index.md#the-image): the default build
covers `RDMA` over DRAM (the transport has no compile switch to leave off, and rdma-core is
installed), and a `ROCM` or `CANN` group names its variant.

What none of those three has is a real-machine run under an election: the Lease backend's
presence is asserted per build target, the fabric data path is not.

`MUSA` and `MACA` keep their own rule because this project builds no variant for either, by
intent: a group on one of them runs an image you built, so whether it also carries the leadership
backend is a property of your build rather than of anything here.

`EFA` needs no vendor runtime either, only libfabric, so `mirrored-mooncake` compiles it into the
default build. The image build proves the transport installed by running a target-mode bench told
`--protocol=efa` and refusing the transport map's "Invalid protocol": the device-less builder's
"No EFA devices found" and an EFA-capable builder's clean run both pass. An `EFA` member group runs
under high availability, on nodes that have the AWS EFA driver installed.

Which address a member follows under that election is the `memberAddressing` choice documented
below; [issue #279](https://github.com/gpustack/gpustack-operator/issues/279), which tracked it, is
closed. The leader Service address is the default and the Lease coordinates are the option.

**`enable_oplog` is refused in `leader.extraArgs`**, and not as a policy choice: the store's
operation log requires the etcd backend, which cannot be compiled together with the Lease backend, so
the flag produces a leader that refuses to start. Standbys rebuild from member remounts alone
instead.

**`etcd_endpoints` is refused as well, and it is out of reach twice over.** The store reads it only
where `ha_backend_connstring` is empty, which the election never leaves empty, and the etcd backend it
names is the one this image cannot carry.

**`rpc_address` and `rpc_interface` are refused there too**, because the election renders the
first. The store folds it with the RPC port into the string it campaigns with, so that one value is
both the election's identity and the address written into the Lease for members to dial. Each replica
advertises **its own Pod IP**; a value supplied by hand would point every member at one host, chosen
without knowing whether the Pod answers there.

**The healthy steady state reads `N desired / 1 ready`.** Exactly one leader serves; the rest are
standbys, and a standby is deliberately **not ready**: that is what keeps it out of the leader
Service's endpoints, so an engine never connects to a process that cannot serve. Read without that
context, `3/1` is what a broken Deployment looks like. `kubectl get deploy` during a healthy
failover briefly shows `2` ready as the old leader steps down; both readings are normal.

To see which Pod is serving, read the label the store itself sets when it wins:

```console
$ kubectl get pod -l mooncake.io/store-role=leader
```

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

| Value | Member receives | Cost |
|---|---|---|
| `Lease` | the Lease's coordinates, read by the client itself | the member must reach the API server, so its image must carry the leadership backend |
| `Service` (default) | `<backend>-leader.<namespace>.svc:50051` | endpoint propagation after an election |

`Service` pays endpoint propagation after an election. `Lease` avoids it by having the member read
the holder itself, which requires access to the API server. One failover comparison found no
meaningful timing difference; the Service endpoint transition was inferred rather than observed.
Retest if election timing changes. Changing this value rolls every member group when HA is active.

**Standby leaders can shorten the outage; they do not keep the cache.** A standby holds no data. The
replica that takes over, like a restarted single leader, rebuilds the members' segment list from
their remounts and learns none of the keys in them, so a DRAM-only key misses until it is written
again.

Keys fully written to a member's [local disk tier](/gpustack-operator/main/docs/modules/kv-cache/local-disk-tier/index.md) may recover after a disk
segment and its objects are registered again. The directory must still be available and the scan
must complete.

A member restart also loses its DRAM bytes. In the tested Mooncake version the restarted member
gets a new client ID, and a stale replica record from before the restart can block that new ID from
registering its disk keys. Disk recovery is therefore conditional, even when the files survive.

What a second replica buys is time. On a single-node test cluster a failover left the store
unusable for about 16 seconds and a single-leader restart for about 30. Neither figure is a bound on
when each disk key first becomes a hit, and on a real cluster a single leader also waits for
scheduling and image pulls.

**Run one leader by default.** Add replicas when the shorter service gap justifies the standby
processes. The default single leader already uses the Lease and the two accounts described above.

**The store's snapshot is not offered, and its flags are refused in `leader.extraArgs`.** A
snapshot records where each key sits in member memory, and restoring one does not check that the
memory still holds that key. An engine reset frees the memory, and a standby's startup snapshot
can become stale before it takes over. Restoring that index can return another key's bytes to the
engine, producing an incorrect KV block.

`enable_snapshot` and `enable_snapshot_restore` are refused with that reason. Every other
`snapshot_*` key is refused because it is read only under one of those two, and `memory_allocator`
because it moves the store off the allocator these rules were traced under.

**A missing grant fails differently on each side, and one of them is silent.** A leader that cannot
reach the Lease retries every second forever. Liveness is ungated, so nothing restarts and the
Deployment sits at `0/N` ready with no message naming the cause. A member that cannot read it gives
up after twenty tries and CrashLoopBackOffs. Check both accounts exist before reading `0/N` as a
store problem.

---

**See also** — [KV Cache Backend](/gpustack-operator/main/docs/modules/kv-cache/backend/index.md) (the object this leader belongs to, its members, and
what status reports) · [KV Cache Local Disk Tier](/gpustack-operator/main/docs/modules/kv-cache/local-disk-tier/index.md) (the disk tier, whose offload
flags this leader derives from the members' declaration) · [High Availability Operations](/gpustack-operator/main/docs/operate/high-availability/index.md) (the replica knob
per control-plane component) · [Settings & Environment Variables](/gpustack-operator/main/docs/reference/settings/index.md) (the
`kv-cache-backend-image` Setting)

**Next** → [KV Cache Backend](/gpustack-operator/main/docs/modules/kv-cache/backend/index.md) — the members this leader serves, and the status it is read
from.
