# KV Cache

> **Purpose** — give inference workloads a shared KV cache with bounded access and capacity.
> **Audience** users, operators · **Prerequisites** [Architecture](../../getting-started/architecture.md) · **Read time** ~2 min

A backend runs the cache store. A pool and binding decide which workloads may use it and how much
capacity they receive.

## Contents

- [Start with a working cache](#start-with-a-working-cache)
- [Understand the parts](#understand-the-parts)

## Start with a working cache

[KV Cache Walkthrough](walkthrough.md) creates the backend, pool, binding and
deployment in order. A plain Pod can also use a binding through [KV Cache
Injection](injection.md).

## Understand the parts

Read [KV Cache Backend](backend.md) for the store and [KV Cache Pool](pool.md)
for grants and quotas. The [leader](leader.md) and [local disk
tier](local-disk-tier.md) have separate operating details.

---

**See also** — [Model Deployment](../model-deployment/_index.md) (cache consumers) ·
[RDMA Networking](../rdma/_index.md) (transport)

**Next** → [KV Cache Walkthrough](walkthrough.md) — create a shared cache.
