# Hierarchical Placement Walkthrough

Configure topology levels across cloud zones and physical racks, request domain placement for
multi-`Pod` replicas, and see what a node move and a hierarchy change do.

## Contents

- [Summary](#summary)
- [Environment](#environment)
- [Step 1: Discover cloud zones](#step-1-discover-cloud-zones)
- [Step 2: Request zone placement](#step-2-request-zone-placement)
- [Step 3: Publish an inventory snapshot](#step-3-publish-an-inventory-snapshot)
- [Step 4: Request rack placement](#step-4-request-rack-placement)
- [Step 5: Move a node](#step-5-move-a-node)
- [Limitations](#limitations)
- [Troubleshooting](#troubleshooting)

## Summary

*Topology-aware scheduling (TAS)* places multi-`Pod` replicas inside one failure domain — a cloud zone
or a physical rack. Configure a read-only cloud-zone `TopologySource`, then narrow placement to a
static rack inventory snapshot. Moving a node between racks and changing the hierarchy levels have
different effects on existing workloads.

The steps discover infrastructure hierarchies, apply placement constraints to serving workloads, and
manage topology updates.

| Object | Scope | Creator | Purpose |
|---|---|---|---|
| `TopologySource` | cluster | administrator | the hierarchy levels and where each node's values come from |
| `ConfigMap` | namespace | administrator | the versioned rack inventory snapshot (Step 3) |
| `ModelDeployment` | namespace | user | the *`topology.requiredLevel`* placement constraint |

## Environment

| Item | Value |
|---|---|
| Cluster | Kubernetes with GPUStack installed |
| Nodes | labeled `topology.kubernetes.io/region` and `topology.kubernetes.io/zone` for Steps 1 and 2; Steps 3 through 5 publish a `ConfigMap` rack inventory instead |
| `InstanceType` | `gpustack--nvidia-a10g-linux-amd64` |
| Model | `Qwen/Qwen2.5-7B-Instruct` |
| Engine | vLLM `gpustack/runner:cuda13.0-vllm0.29.0`, with `llm-d-router` |

## Step 1: Discover cloud zones

Verify existing node labels and cluster status before configuring topology sources:

```bash
kubectl get nodes --show-labels
kubectl get topologysources
kubectl get topologies.kueue.x-k8s.io
```

When nodes already carry cloud provider labels for regions and zones, define a read-only
`TopologySource` using *`nodeLabels`*:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: TopologySource
metadata:
  name: cloud-zones
spec:
  nodeSelector:
    matchLabels:
      node-role.kubernetes.io/worker: ""
  levels:
    - topology.kubernetes.io/region
    - topology.kubernetes.io/zone
  nodeLabels: {}
```

The operator validates that labels form a strict tree (each zone belongs to exactly one region) and
computes a profile hash. `Node`s receive `topology.gpustack.ai/profile=fnv64-<hash>`, and Kueue
materializes the matching `Topology` resource.

Check source readiness:

```bash
kubectl get topologysources cloud-zones -o yaml
```

## Step 2: Request zone placement

When a serving replica consists of multiple `Pod`s (such as tensor-parallel groups spanning multiple nodes),
request placement within a single availability zone:

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: ModelDeployment
 metadata:
   name: qwen-distributed
   namespace: team-a
 spec:
   model:
     name: Qwen/Qwen2.5-7B-Instruct
   engine:
     name: vLLM
     image: gpustack/runner:cuda13.0-vllm0.29.0
   roles:
     - name: server
       replicas: 1
       size: 2
       instanceType: gpustack--nvidia-a10g-linux-amd64
       resources:
         accelerator: "1"
+      topology:
+        requiredLevel: topology.kubernetes.io/zone
```

Kueue *TAS* admits the replica only when a single zone contains enough unallocated capacity to host all
2 `Pod`s. If capacity is scattered across zones without filling any single zone, the workload waits in
`Pending` rather than stranding split `Pod`s.

## Step 3: Publish an inventory snapshot

In private data centers without cloud labels, publish rack hierarchies using a versioned snapshot
stored in a `ConfigMap`:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: topology-inventory
  namespace: gpustack-system
data:
  snapshot.yaml: |
    apiVersion: topology.gpustack.ai/v1alpha1
    revision: inventory-42
    nodes:
      worker-a:
        topology.gpustack.ai/region: region-a
        topology.gpustack.ai/zone: zone-a
        topology.gpustack.ai/rack: rack-01
      worker-b:
        topology.gpustack.ai/region: region-a
        topology.gpustack.ai/zone: zone-a
        topology.gpustack.ai/rack: rack-02
---
apiVersion: worker.gpustack.ai/v1
kind: TopologySource
metadata:
  name: data-center
spec:
  nodeSelector: {}
  levels:
    - topology.gpustack.ai/region
    - topology.gpustack.ai/zone
    - topology.gpustack.ai/rack
  configMap:
    configMapRef:
      namespace: gpustack-system
      name: topology-inventory
    key: snapshot.yaml
    maxStaleness: 10m
```

The operator validates the snapshot atomically, creates NFD `NodeFeature` resources, and writes the
custom labels to each node.

## Step 4: Request rack placement

With rack labels published, narrow the workload constraint from zone-level to rack-level:

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: ModelDeployment
 metadata:
   name: qwen-distributed
   namespace: team-a
 spec:
   roles:
     - name: server
       topology:
-        requiredLevel: topology.kubernetes.io/zone
+        requiredLevel: topology.gpustack.ai/rack
```

All `Pod`s within each replica group are placed within the same physical rack, minimizing inter-switch
network latency.

## Step 5: Move a node

When a node is cabled into a new rack, update the `ConfigMap` snapshot with an incremented revision:

```diff
 apiVersion: v1
 kind: ConfigMap
 metadata:
   name: topology-inventory
   namespace: gpustack-system
 data:
   snapshot.yaml: |
     apiVersion: topology.gpustack.ai/v1alpha1
-    revision: inventory-42
+    revision: inventory-43
     nodes:
       worker-a:
         topology.gpustack.ai/region: region-a
         topology.gpustack.ai/zone: zone-a
         topology.gpustack.ai/rack: rack-01
       worker-b:
         topology.gpustack.ai/region: region-a
         topology.gpustack.ai/zone: zone-a
-        topology.gpustack.ai/rack: rack-02
+        topology.gpustack.ai/rack: rack-03
```

The profile is a hash of the ordered level keys, and the keys did not change.
The node keeps its profile, so the worker creates no flavor and applies no hold.
The node carries the new rack value after NFD publishes it.

A change to the ordered level keys is a different operation that can evict and readmit workloads,
unlike the revision-only change shown above. See
[Change a live hierarchy](/gpustack-operator/main/docs/modules/topology/operations/index.md#change-a-live-hierarchy) for the
`HoldAndDrain` migration sequence and its interruption impact before editing `levels`.

## Limitations

- Single-source node ownership and the hostname-only profile fallback are covered in
  [Hierarchy profiles](/gpustack-operator/main/docs/modules/topology/scheduling/index.md#hierarchy-profiles).
- Domain capacity packing and multi-role gang holds are covered in
  [Troubleshooting](/gpustack-operator/main/docs/modules/topology/scheduling/index.md#troubleshooting).
- Changing the ordered `levels` list on a live hierarchy triggers `HoldAndDrain` migration; see
  [Change a live hierarchy](/gpustack-operator/main/docs/modules/topology/operations/index.md#change-a-live-hierarchy).

## Troubleshooting

For general failure surfaces (including `AwaitingQueueStatus`, `HoldAndDrain` flavor migration, and
domain capacity fragmentation), see [Topology-Aware Scheduling Failure Surfaces](/gpustack-operator/main/docs/modules/topology/scheduling/index.md#troubleshooting)
and [Change a live hierarchy](/gpustack-operator/main/docs/modules/topology/operations/index.md#change-a-live-hierarchy).

| Symptom | Signal | Check & Mitigation |
|---|---|---|
| `TopologySource` `Ready=False` | Reason: `InvalidHierarchy` / `SnapshotInvalid` | Label hierarchy invalid (child key must have unique parent value); fix snapshot syntax or missing node entries |
| `TopologySource` `Ready=False` | Reason: `OwnershipConflict` | Multiple writable sources target the same node; adjust node selectors to ensure disjoint sets |
| `TopologySource` `Valid=False` | Reason: `ConfigMapReadFailed` | Verify `ConfigMap` name and namespace match the worker installation |
| Node Topology Profile falls back to single-level `hostname` | Ambiguous source ownership | `Node` selected by 0 or multiple Ready sources; check node selectors across all sources |
| Multi-role deployment has 1 `Workload` reserved but `Pod` not bound | Joint admission gang hold | First role fits in domain, but held until secondary role (*prefill*/*decode*) secures domain capacity; check other roles |

---

**See also** — [Topology-Aware Scheduling](/gpustack-operator/main/docs/modules/topology/scheduling/index.md) (levels and profiles) ·
[Topology-Aware Scheduling Operations](/gpustack-operator/main/docs/modules/topology/operations/index.md) (`ConfigMap` and Webhook sources) ·
[Model Deployment Configuration](/gpustack-operator/main/docs/modules/model-deployment/deployment/index.md)

**Next** → [Shared Cache Walkthrough](/gpustack-operator/main/docs/walkthroughs/kv-cache/shared-cache/index.md)
