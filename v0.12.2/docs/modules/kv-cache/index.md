# KV Cache

A shared KV cache lets compatible inference workloads reuse cached prefix blocks. A `KVCacheBackend`
runs the store, while `KVCachePool` and `KVCachePoolBinding` grant capacity to namespaces. Cache
quota is separate from accelerator quota.

## Contents

- [Overview](#overview)
- [Configuration](#configuration)
- [Operations](#operations)
- [Troubleshooting](#troubleshooting)

## Overview

![KV cache backend and workload flow](/gpustack-operator/v0.12.2/docs/assets/diagrams/kv-cache-flow.svg)

<!--
flowchart TD
    MASTER["Mooncake leader and optional tenant ledger"] -> BACKEND["Backend storage members"]
    BACKEND -> POOL["KVCachePool capacity"]
    POOL -> BINDING["Namespace KVCachePoolBinding"]
    BINDING -> WEBHOOK["Cache injection admission"]
    WEBHOOK -> PODS["Serving Pods and compatible prefix reuse"]
    PODS -> BACKEND
-->

A `KVCachePoolBinding` provisions capacity and registers a reuse domain, not network access control.
Review [Limitations](/gpustack-operator/v0.12.2/docs/modules/kv-cache/pool/index.md#limitations) before sharing one backend across distrusting tenants.

## Configuration

Start with the [backend manifest](/gpustack-operator/v0.12.2/docs/modules/kv-cache/backend/index.md#connections-and-storage-media) to deploy or attach a store,
then configure [pools and bindings](/gpustack-operator/v0.12.2/docs/modules/kv-cache/pool/index.md) for namespace quotas and reuse domains.
[Pod injection](/gpustack-operator/v0.12.2/docs/modules/kv-cache/pool/index.md#pod-injection) covers opt-in `Pod` labels, annotations, and verification.

For an end-to-end deployment sequence, follow the
[Shared Cache Walkthrough](/gpustack-operator/v0.12.2/docs/walkthroughs/kv-cache/shared-cache/index.md).

## Operations

### Step 1: Configure storage

Choose memory segments and optional local disk offload in [KV Cache Backend](/gpustack-operator/v0.12.2/docs/modules/kv-cache/backend/index.md) and
[KV Cache Local Disk Tier](/gpustack-operator/v0.12.2/docs/modules/kv-cache/local-disk-tier/index.md) (including
[disk and memory sizing](/gpustack-operator/v0.12.2/docs/modules/kv-cache/local-disk-tier/index.md#disk-and-memory-sizing)).

### Step 2: Verify backend readiness

Inspect backend readiness and conditions:

```bash
kubectl get kvcb
kubectl describe kvcb <backend-name>
```

**Verification:** Confirm `PHASE` is `Ready` against
[Status and conditions](/gpustack-operator/v0.12.2/docs/modules/kv-cache/backend/index.md#status-and-conditions). For leader probes and failover, see
[KV Cache Election and Failover](/gpustack-operator/v0.12.2/docs/modules/kv-cache/leader/index.md#health-checks).

## Troubleshooting

If a workload cannot attach or write to the cache, inspect the backend, pool, and binding status:

| Condition | Reason | Resolution |
|---|---|---|
| `MembersMounted=False` | `AmbiguousMemberIdentity` | Place host-network member groups on separate `Node`s via *`nodeSelector`*. |
| Engine writes fail with `RPC_FAIL (-900)` | Store and client minor versions differ | Align store and engine versions in [Version compatibility](/gpustack-operator/v0.12.2/docs/modules/kv-cache/backend/index.md#version-compatibility). |

Use [Refused engine launch](/gpustack-operator/v0.12.2/docs/modules/kv-cache/pool/index.md#refused-engine-launch) for admission refusals,
[Pod binding verification](/gpustack-operator/v0.12.2/docs/modules/kv-cache/pool/index.md#pod-binding-verification) to inspect admitted `Pod`s, and
[Quota and deletion checks](/gpustack-operator/v0.12.2/docs/modules/kv-cache/pool/index.md#quota-and-deletion-checks) for deletion holds.

---

**See also** — [Model Deployment](/gpustack-operator/v0.12.2/docs/modules/model-deployment/index.md) (managed serving consumers) · [RDMA Networking](/gpustack-operator/v0.12.2/docs/modules/rdma/index.md) (fabric endpoints)

**Next** → [Shared Cache Walkthrough](/gpustack-operator/v0.12.2/docs/walkthroughs/kv-cache/shared-cache/index.md) — deploy and verify a shared cache.
