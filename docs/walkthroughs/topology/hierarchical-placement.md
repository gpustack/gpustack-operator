# Hierarchical Placement Walkthrough

Configure topology levels across cloud zones and physical racks, request domain placement for
multi-Pod replicas, and see what a node move and a hierarchy change do.

Follow these steps to discover infrastructure hierarchies, apply placement constraints to serving
workloads, and manage topology updates.

## Contents

- [Prerequisites](#prerequisites)
- [Step 1: read-only cloud zone discovery](#step-1-read-only-cloud-zone-discovery)
- [Step 2: request zone placement from ModelDeployment](#step-2-request-zone-placement-from-modeldeployment)
- [Step 3: static inventory snapshot with ConfigMap](#step-3-static-inventory-snapshot-with-configmap)
- [Step 4: request rack placement](#step-4-request-rack-placement)
- [Step 5: move a node, then change the hierarchy](#step-5-move-a-node-then-change-the-hierarchy)
- [Troubleshooting](#troubleshooting)

## Prerequisites

Topology-Aware Scheduling (TAS) operates continuously through Kueue. When no custom `TopologySource` is
declared, nodes use a single-level hierarchy consisting solely of `kubernetes.io/hostname`.

Each node must match exactly one Ready `TopologySource`. If a node matches zero or multiple sources,
the operator falls back to the hostname profile to prevent ambiguous hierarchy trees.

Verify existing node labels and cluster status before configuring topology sources:

```bash
kubectl get nodes --show-labels
kubectl get topologysources
kubectl get topologies.kueue.x-k8s.io
```

## Step 1: read-only cloud zone discovery

When nodes already carry cloud provider labels for regions and zones, define a read-only
`TopologySource` using `nodeLabels`:

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
computes a profile hash. Nodes receive `topology.gpustack.ai/profile=fnv64-<hash>`, and Kueue
materializes the matching `Topology` resource.

Check source readiness:

```bash
kubectl get topologysources cloud-zones -o yaml
```

## Step 2: request zone placement from ModelDeployment

When a serving replica consists of multiple Pods (such as tensor-parallel groups spanning multiple nodes),
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
     version: "0.29.0"
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

Kueue TAS admits the replica only when a single zone contains enough unallocated capacity to host all
2 Pods. If capacity is scattered across zones without filling any single zone, the workload waits in
`Pending` rather than stranding split Pods.

## Step 3: static inventory snapshot with ConfigMap

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

## Step 4: request rack placement

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

All Pods within each replica group are placed within the same physical rack, minimizing inter-switch
network latency.

## Step 5: move a node, then change the hierarchy

When a node is cabled into a new rack, update the ConfigMap snapshot with an incremented revision:

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

A change to the ordered level keys is a different operation. It produces a new profile, a new Kueue
`Topology` and new `ResourceFlavor` objects. If the new plan drops a flavor that still has reservations,
the worker sets `HoldAndDrain` on the managed ClusterQueue. It waits for zero reservations, switches
the flavor plan, and restores the previous stop policy.

Kueue evicts and readmits the affected Workloads, so serving can be interrupted.
Read [Change a live hierarchy](../../modules/topology/operations.md#change-a-live-hierarchy) before you edit `levels`.

## Troubleshooting

For general failure surfaces (including `AwaitingQueueStatus`, `HoldAndDrain` flavor migration, and
domain capacity fragmentation), see [Topology-Aware Scheduling Failure Surfaces](../../modules/topology/scheduling.md#failure-surfaces)
and [Change a live hierarchy](../../modules/topology/operations.md#change-a-live-hierarchy).

| Condition / Symptom | Status / Reason | Root cause & Mitigation |
|---|---|---|
| `TopologySource` `Ready=False` | `InvalidHierarchy` / `SnapshotInvalid` | Label hierarchy invalid (child key must have unique parent value); fix snapshot syntax or missing node entries |
| `TopologySource` `Ready=False` | `OwnershipConflict` | Multiple writable sources target the same node; adjust node selectors to ensure disjoint sets |
| `TopologySource` `Valid=False` | `ConfigMapReadFailed` | Verify ConfigMap name and namespace match the worker installation |
| Node Topology Profile falls back to single-level `hostname` | Ambiguous source ownership | Node selected by 0 or multiple Ready sources; check node selectors across all sources |
| Multi-role deployment has 1 Workload reserved but Pod not bound | Joint admission gang hold | First role fits in domain, but held until secondary role (prefill/decode) secures domain capacity; check other roles |

---

**See also** — [Topology-Aware Scheduling](../../modules/topology/scheduling.md) (levels and profiles) ·
[Topology-Aware Scheduling Operations](../../modules/topology/operations.md) (ConfigMap and Webhook sources) ·
[Model Deployment Configuration](../../modules/model-deployment/deployment.md)

**Next** → [Shared Cache Walkthrough](../kv-cache/shared-cache.md)
