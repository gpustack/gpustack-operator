# KV Cache Election and Failover

Each managed `KVCacheBackend` runs a metadata leader (`Deployment` and ClusterIP `Service`), health
probes, and optional Kubernetes `Lease` election. GPUStack APIs call this component the *leader*;
Mooncake flags, environment variables, and metrics call it `master`.

## Contents

- [Leader configuration](#leader-configuration)
- [Election configuration](#election-configuration)
- [Update strategy](#update-strategy)
- [Health checks](#health-checks)
- [High availability](#high-availability)

## Leader configuration

The leader `Service` exposes port `50051` for engine RPCs and port `9003` for Prometheus metrics and
the HTTP admin API.

Use *`leader.extraArgs`* (`-key=value`) and *`leader.extraEnv`* (unique `name` keys) to pass
upstream settings that are not owned by the operator:

- **`metrics_host` (IPv6 required)**: Moves the admin bind address without changing port `9003`.
  Mooncake defaults to IPv4-only `0.0.0.0`, so set `-metrics_host=::` on IPv6 or dual-stack clusters
  so kubelet probes succeed. Any concrete IP other than `0.0.0.0` or `::` breaks kubelet probes.
- **`default_kv_lease_ttl` (override allowed)**: The operator renders `5m` (instead of upstream's
  `10s`) to protect recently **read** blocks from eviction across multi-turn prefix reuse. An entry
  in *`leader.extraArgs`* renders after the default and overrides it.
- **Refused flags**: Admission rejects `port` (deprecated alias of rendered `rpc_port`), CXL flags,
  `enable_oplog`, `etcd_endpoints`, `rpc_address`, `rpc_interface`, `memory_allocator`, and all
  `enable_snapshot*` / `snapshot_*` flags (restoring stale memory offsets after an engine reset
  returns wrong KV bytes). Identity variables rendered by the operator are also refused in
  *`leader.extraEnv`*.

## Election configuration

*`leader.replicas`* defaults to `1` (max `5`), and *`leader.electionBackend`* defaults to
`Kubernetes` even at `replicas: 1`. Only one replica serves traffic; additional replicas stand by
for failover.

- **Scaling without restarts**: Because `electionBackend: Kubernetes` is already active at `1`
  replica, raising *`replicas`* to `3` adds standbys without restarting the active leader or rolling
  member `Pod`s. (If a legacy `Deployment` still carries an unelected template, the controller
  rolls `1` elected replica first and reports `RolloutComplete=False/ReplicasPending` before scaling
  out.)
- **Single-replica `None`**: Set `electionBackend: None` only for images without Kubernetes `Lease`
  support (`replicas > 1` with `None` is refused at admission and clamped to `1`). Switching between
  `None` and `Kubernetes` changes leader and member `Pod` templates, restarting them and clearing
  DRAM segments.

## Update strategy

At `replicas: 1`, the leader `Deployment` uses `Recreate`, creating a brief service gap during image
or flag updates while members retain their segments and remount afterward. Above `1` replica, it
uses `RollingUpdate` (`maxSurge: 1`, `maxUnavailable: replicas`) so standbys are already scheduled
and pulled before taking the `Lease`.

## Health checks

Readiness and liveness probes target separate endpoints on port `9003`:

| Probe | Path | Gated |
|---|---|---|
| Readiness | `GET /get_all_segments` | Yes (`503` until active) |
| Liveness | `GET /health` | No (`200` on standbys and activating leaders) |

In the `/health` JSON response (`{"status":"ok","role":"leader","ha_state":"serving","service_ready":true}`),
`status` is always `"ok"`; inspect **`service_ready`** to determine whether the leader is active.

## High availability

Under `electionBackend: Kubernetes`, replicas campaign for the `Lease` `<backend>-leader` in the
operator namespace. Scale an elected leader with a `replicas` patch:

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: KVCacheBackend
 metadata:
   name: shared-mooncake
 spec:
   connection:
     managed:
       leader:
-        replicas: 1
+        replicas: 3
```

**Image requirement:** Upstream `kvcacheai/mooncake` images compile leadership backends **off**
(leaders stay in `UNAVAILABLE_IN_CURRENT_MODE` and members crash with `Invalid HA backend entry`).
Use `gpustack/mirrored-mooncake` (`>= 0.3.12`, built from
[`pack/mirrored-mooncake`](https://github.com/gpustack/gpustack-operator/blob/8ed4a0acaea93847d38a3ab3434849d91603a342/pack/mirrored-mooncake/Dockerfile)) for *`spec.image`* and every
*`members[].image`* (`0.3.11` exits on rendered `-pod_name` and `-pod_namespace` flags).

All official `mirrored-mooncake` variants (`-cpu`, `cuda`, `cann`, `rocm`, including built-in `EFA`
via `libfabric`) compile the `Lease` backend in; custom `MUSA` or `MACA` builds must enable it
themselves.[^1]

[^1]: Build targets verify `Lease` support at build time; multi-leader failover over live `RDMA`, `ROCM`, or `CANN` fabrics has not been hardware-measured.

**Steady-state readiness (`N` desired, `1` ready):** Standbys intentionally fail readiness so the
leader `Service` routes only to the active holder (`kubectl get deploy` briefly shows `2` ready
during handover). Identify the active leader `Pod` by its self-applied label:

```bash
kubectl -n gpustack-system get pod -l mooncake.io/store-role=leader
```

**RBAC separation:** The operator provisions separate `ServiceAccount`s, `Role`s, and `RoleBinding`s
in its namespace so members cannot claim the `Lease`:

| Component | Permissions | Purpose |
|---|---|---|
| Leader | `leases` (`create`, `get`, `update`), `pods` (`patch`) | Acquire the `Lease` and label the winning `Pod`. |
| Member | `leases` (`get`) | Follow the `Lease` holder when `memberAddressing: Lease` is set. |

If RBAC is missing, leaders retry silently at `0/N` ready (liveness stays `200`), while members exit
after 20 retries (`CrashLoopBackOff`). Verify both `ServiceAccount`s before debugging the store.

**Member addressing:** *`leader.memberAddressing`* selects how members locate the elected leader
(changing it while HA is active rolls all member groups):

| Value | Member `MOONCAKE_MASTER` | Trade-off |
|---|---|---|
| `Service` (default) | `<backend>-leader.<namespace>.svc:50051` | Waits for Kubernetes `Endpoints` propagation on failover. |
| `Lease` | `k8s://<namespace>/<lease>` | Reads the `Lease` directly; requires member API-server reachability and a `Lease`-enabled image. |

**Failover behavior:** Standbys hold no metadata index. After failover (`~16 s` on a single-node
cluster versus `~30 s` for a single-leader restart), the new leader rebuilds segment mounts from
members; DRAM-only keys miss until rewritten, and [local disk tier](/gpustack-operator/main/docs/modules/kv-cache/local-disk-tier/index.md) keys recover
once disk segments re-register (unless a restarted member's new client ID collides with a stale
replica record).

---

**See also** — [KV Cache Backend](/gpustack-operator/main/docs/modules/kv-cache/backend/index.md) (member groups, transports, and conditions) · [KV Cache Local Disk Tier](/gpustack-operator/main/docs/modules/kv-cache/local-disk-tier/index.md) (offload flags derived from member disk declarations) · [High Availability Operations](/gpustack-operator/main/docs/operate/high-availability/index.md) (control-plane replica settings)

**Next** → [KV Cache Backend](/gpustack-operator/main/docs/modules/kv-cache/backend/index.md) — configure member storage groups and inspect backend status.
