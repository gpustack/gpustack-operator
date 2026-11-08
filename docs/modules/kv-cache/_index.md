# KV Cache

A shared KV cache lets compatible inference workloads reuse cached state. A backend runs the store.
Pools and bindings grant capacity to namespaces. Cache quota is separate from accelerator quota.

## Contents

- [Overview](#overview)
- [Configuration](#configuration)
- [Operations](#operations)
- [Troubleshooting](#troubleshooting)

## Overview

![KV cache backend and workload flow](../../assets/diagrams/kv-cache-flow.svg)

<!--
flowchart TD
    MASTER["Mooncake leader and optional tenant ledger"] -> BACKEND["Backend storage members"]
    BACKEND -> POOL["KVCachePool capacity"]
    POOL -> BINDING["Namespace KVCachePoolBinding"]
    BINDING -> WEBHOOK["Cache injection admission"]
    WEBHOOK -> PODS["Serving Pods and compatible prefix reuse"]
    PODS -> BACKEND
-->

A binding grants capacity only; read the
[isolation limitations](pool.md#limitations) before sharing a backend between tenants.

## Configuration

Start with the annotated [backend example](backend.md#connection-and-medium) and its constraints.
Configure [pools and bindings](pool.md) for quotas and reuse domains.
[KV Cache Injection](injection.md) defines the Pod labels, annotations and engine settings.

For an end-to-end configuration, follow the
[Shared Cache Walkthrough](../../walkthroughs/kv-cache/shared-cache.md).

## Operations

### Step 1: Configure storage

Select memory and optional disk tiers. See [KV Cache Backend](backend.md),
[Local Disk Tier](local-disk-tier.md) and [Disk-Heavy Nodes](disk-heavy-nodes.md).

### Step 2: Check status

Inspect the backend and its conditions:

```shell
kubectl get kvcb
kubectl describe kvcb <backend-name>
```

**Verification:** Check the phase and conditions against [Backend Status](backend.md#status-and-conditions).
For leader health and metrics, see [KV Cache Election and Failover](leader.md#health-checks).

## Troubleshooting

If a workload cannot use the cache, inspect backend, pool and binding status.

| Condition | Reason | Resolution |
|---|---|---|
| `MembersMounted=False` | `AmbiguousMemberIdentity` | Use node selectors that place the groups on different nodes. |
| Writes fail with `RPC_FAIL (-900)` | Possible store and client minor-line mismatch | Check [version compatibility](backend.md#version-compatibility). |

Use [Injection Refusals](injection.md#refusals-and-fixes) for webhook messages and fixes.
For quota and domain failures, see [Pool Operating Notes](pool.md#operating-notes).
For backend failures, check [Backend Status](backend.md#status-and-conditions).

---

**See also** — [Model Deployment](../model-deployment/_index.md) (cache consumers) ·
[RDMA Networking](../rdma/_index.md) (transport)

**Next** → [Shared Cache Walkthrough](../../walkthroughs/kv-cache/shared-cache.md) — create a shared cache.
