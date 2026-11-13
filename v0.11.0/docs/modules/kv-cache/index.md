# KV Cache

A shared KV cache lets compatible inference workloads reuse cached state. A backend runs the store.
Pools and bindings grant capacity to namespaces. Cache quota is separate from accelerator quota.

## Contents

- [Overview](#overview)
- [Configuration](#configuration)
- [Operations](#operations)
- [Troubleshooting](#troubleshooting)

## Overview

![KV cache backend and workload flow](/gpustack-operator/v0.11.0/docs/assets/diagrams/kv-cache-flow.svg)

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
[isolation limitations](/gpustack-operator/v0.11.0/docs/modules/kv-cache/pool/index.md#limitations) before sharing a backend between tenants.

## Configuration

Start with the annotated [backend example](/gpustack-operator/v0.11.0/docs/modules/kv-cache/backend/index.md#connection-and-medium) and its constraints.
Configure [pools and bindings](/gpustack-operator/v0.11.0/docs/modules/kv-cache/pool/index.md) for quotas and reuse domains.
[KV Cache Injection](/gpustack-operator/v0.11.0/docs/modules/kv-cache/injection/index.md) defines the Pod labels, annotations and engine settings.

For an end-to-end configuration, follow the
[Shared Cache Walkthrough](/gpustack-operator/v0.11.0/docs/walkthroughs/kv-cache/shared-cache/index.md).

## Operations

### Step 1: Configure storage

Select memory and optional disk tiers. See [KV Cache Backend](/gpustack-operator/v0.11.0/docs/modules/kv-cache/backend/index.md),
[Local Disk Tier](/gpustack-operator/v0.11.0/docs/modules/kv-cache/local-disk-tier/index.md) and [Disk-Heavy Nodes](/gpustack-operator/v0.11.0/docs/modules/kv-cache/disk-heavy-nodes/index.md).

### Step 2: Check status

Inspect the backend and its conditions:

```bash
kubectl get kvcb
kubectl describe kvcb <backend-name>
```

**Verification:** Check the phase and conditions against [Backend Status](/gpustack-operator/v0.11.0/docs/modules/kv-cache/backend/index.md#status-and-conditions).
For leader health and metrics, see [KV Cache Election and Failover](/gpustack-operator/v0.11.0/docs/modules/kv-cache/leader/index.md#health-checks).

## Troubleshooting

If a workload cannot use the cache, inspect backend, pool and binding status.

| Condition | Reason | Resolution |
|---|---|---|
| `MembersMounted=False` | `AmbiguousMemberIdentity` | Use node selectors that place the groups on different nodes. |
| Writes fail with `RPC_FAIL (-900)` | Possible store and client minor-line mismatch | Check [version compatibility](/gpustack-operator/v0.11.0/docs/modules/kv-cache/backend/index.md#version-compatibility). |

Use [Injection Refusals](/gpustack-operator/v0.11.0/docs/modules/kv-cache/injection/index.md#refusals-and-fixes) for webhook messages and fixes.
For quota and domain failures, see [Pool Operating Notes](/gpustack-operator/v0.11.0/docs/modules/kv-cache/pool/index.md#operating-notes).
For backend failures, check [Backend Status](/gpustack-operator/v0.11.0/docs/modules/kv-cache/backend/index.md#status-and-conditions).

---

**See also** — [Model Deployment](/gpustack-operator/v0.11.0/docs/modules/model-deployment/index.md) (cache consumers) ·
[RDMA Networking](/gpustack-operator/v0.11.0/docs/modules/rdma/index.md) (transport)

**Next** → [Shared Cache Walkthrough](/gpustack-operator/v0.11.0/docs/walkthroughs/kv-cache/shared-cache/index.md) — create a shared cache.
