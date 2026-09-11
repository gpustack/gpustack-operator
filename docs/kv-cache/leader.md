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

`replicas` defaults to `1`, and `5` is the ceiling in the **webhook** and in the schema alike: only
one leader ever serves, so further replicas are spare processes rather than capacity. More than one
requires [`highAvailability`](#high-availability) and is refused by the webhook without it, naming
the field that is missing. An enum would answer `Unsupported value: 2` and teach nothing.

⛔ **Without `highAvailability` the Deployment runs one replica whatever `replicas` says.** The
webhook refuses that combination, but a schema cannot express a cross-field rule — so where the
webhook is not installed this clamp is what keeps unelected masters off one pool.

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

Set `leader.highAvailability` and the leader elects through a **Kubernetes Lease**. The field has no
settings — the Lease carries the leader's own object name, `<backend>-leader`, in this operator's
namespace — and its presence is the switch:

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
`spec.image` **and for every `members[].image`**.

⛔ **A member group on `RDMA`, `HIP` or `Ascend` cannot run under high availability today.** Those
transports need a vendor runtime `mirrored-mooncake` does not carry, and the vendor build does not
carry the leadership backend — the two axes are independent, so covering them means rebuilding each
variant.

`EFA` is the one fabric not on that list: it needs no vendor runtime, only libfabric, so
`mirrored-mooncake` compiles it in — the image build proves the transport installed by running a
target-mode bench told `--protocol=efa` and refusing the transport map's "Invalid protocol": the
device-less builder's "No EFA devices found" and an EFA-capable builder's clean run both pass. An
`EFA` member group runs under high availability, on nodes that have the AWS EFA driver installed.

Tracked at [issue #279](https://github.com/gpustack/gpustack-operator/issues/279), together with the
alternative of leaving members on the leader Service address and letting readiness move the endpoint.

⛔ **`enable_oplog` is refused in `leader.extraArgs`**, and not as a policy choice: the store's
operation log requires the etcd backend, which cannot be compiled together with the Lease backend, so
the flag produces a leader that refuses to start. Standbys rebuild from snapshot and remounts instead.

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
| member | `leases`: `get` | finds the leader, and nothing more |

The member's is narrower on purpose: a shared account would let any member take the Lease from the
leader it is following.

**A member's `MOONCAKE_MASTER` becomes `k8s://<namespace>/<lease>`** instead of the leader Service
address, so the client reads the holder and follows it across an election without restarting. Without
`highAvailability` the value is unchanged.

**The Service address is not known to be wrong under HA** — a standby is not ready, so the Service
already resolves to the serving leader. What is unmeasured is whether a member's reconnect follows
the endpoint when an election moves it. The Lease is what this operator renders until that is
measured; see #279 above.

**A missing grant fails differently on each side, and one of them is silent.** A leader that cannot
reach the Lease retries every second forever — liveness is ungated, so nothing restarts and the
Deployment sits at `0/N` ready with no message naming the cause. A member that cannot read it gives
up after twenty tries and CrashLoopBackOffs. Check both accounts exist before reading `0/N` as a
store problem.

---

**See also** — [KV Cache Backend](backend.md) (the object this leader belongs to, its members, and
what status reports) · [KV Cache Local Disk Tier](local-disk-tier.md) (the leader's half of the
offload pair) · [High Availability Operations](../operation/high-availability.md) (the replica knob
per control-plane component) · [Settings & Environment Variables](../settings.md) (the
`kv-cache-backend-image` Setting)

**Next** → [KV Cache Backend](backend.md) — the members this leader serves, and the status it is read
from.
