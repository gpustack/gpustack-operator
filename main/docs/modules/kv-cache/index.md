# KV Cache

A backend runs the cache store. A pool and binding decide which workloads may use it and how much
capacity they receive.

## Contents

- [Quick start](#quick-start)
- [Cache configuration](#cache-configuration)

## Quick start

[Shared Cache Walkthrough](/gpustack-operator/main/docs/walkthroughs/kv-cache/shared-cache/index.md) creates the backend, pool, binding and
deployment in order. A plain Pod can also use a binding through [KV Cache
Injection](injection.md).

## Cache configuration

Read [KV Cache Backend](/gpustack-operator/main/docs/modules/kv-cache/backend/index.md) for the store and [KV Cache Pool](/gpustack-operator/main/docs/modules/kv-cache/pool/index.md)
for grants and quotas. The [leader](/gpustack-operator/main/docs/modules/kv-cache/leader/index.md) and [local disk
tier](local-disk-tier.md) have separate operating details.
[Disk-Heavy Nodes](/gpustack-operator/main/docs/modules/kv-cache/disk-heavy-nodes/index.md) covers cache placement on storage nodes.

---

**See also** — [Model Deployment](/gpustack-operator/main/docs/modules/model-deployment/index.md) (cache consumers) ·
[RDMA Networking](/gpustack-operator/main/docs/modules/rdma/index.md) (transport)

**Next** → [Shared Cache Walkthrough](/gpustack-operator/main/docs/walkthroughs/kv-cache/shared-cache/index.md) — create a shared cache.
