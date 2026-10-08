# KV Cache

A backend runs the cache store. A pool and binding decide which workloads may use it and how much
capacity they receive.

## Contents

- [Quick start](#quick-start)
- [Cache configuration](#cache-configuration)

## Quick start

[KV Cache Walkthrough](../../walkthroughs/kv-cache/shared-cache.md) creates the backend, pool, binding and
deployment in order. A plain Pod can also use a binding through [KV Cache
Injection](injection.md).

## Cache configuration

Read [KV Cache Backend](backend.md) for the store and [KV Cache Pool](pool.md)
for grants and quotas. The [leader](leader.md) and [local disk
tier](local-disk-tier.md) have separate operating details.
[Disk-Heavy Nodes](disk-heavy-nodes.md) covers cache placement on storage nodes.

---

**See also** — [Model Deployment](../model-deployment/_index.md) (cache consumers) ·
[RDMA Networking](../rdma/_index.md) (transport)

**Next** → [KV Cache Walkthrough](../../walkthroughs/kv-cache/shared-cache.md) — create a shared cache.
