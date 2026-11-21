# Hierarchical Placement Walkthrough

Publish cloud-zone and rack topology hierarchies, constrain multi-`Pod` serving replicas to one
failure domain, and observe how moving a node between racks differs from changing hierarchy levels.

## Contents

- [Summary](#summary)
- [Zone placement](#zone-placement)
- [Rack placement](#rack-placement)
- [Limitations](#limitations)
- [Troubleshooting](#troubleshooting)

## Summary

*Topology-aware scheduling (TAS)* packs all *`size`* `Pod`s of a replica inside one failure domain
(such as an availability zone or physical rack). You first discover existing cloud zone labels via a
read-only `TopologySource`, then switch to a versioned `ConfigMap` rack snapshot and move a node
without draining queues:

| Object | Scope | Creator | Purpose |
|---|---|---|---|
| `TopologySource` | cluster | administrator | Declares ordered hierarchy levels and where node domain values come from |
| `ConfigMap` | namespace | administrator | Versioned rack inventory snapshot consumed in Steps 3–5 |
| `ModelDeployment` | namespace | user | Requests domain packing via *`roles[].topology.requiredLevel`* |

### Environment

| Item | Value |
|---|---|
| Cluster | Kubernetes with GPUStack installed (illustrative configuration examples) |
| Nodes | Worker `Node`s carrying `topology.kubernetes.io/region` and `zone` in Steps 1–2; `ConfigMap` snapshot in Steps 3–5 |
| `AcceleratorType` | `gpustack--nvidia-a10g-linux-amd64` (substitute an `Active` pool from `kubectl get acctype`) |
| Model and engine | `Qwen/Qwen2.5-7B-Instruct` on vLLM `gpustack/runner:cuda13.0-vllm0.29.0` |

## Zone placement

### Step 1: Discover cloud zones

Check existing `Node` topology labels and confirm no conflicting `TopologySource` already selects the
worker nodes:

```bash
kubectl get nodes --show-labels
kubectl get topologysources
kubectl get topologies.kueue.x-k8s.io
```

Save and apply `cloud-zones.yaml` to build a two-level hierarchy from existing cloud provider labels:

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

```bash
kubectl apply -f cloud-zones.yaml
kubectl get topologysource cloud-zones \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'
kubectl get topologies.kueue.x-k8s.io
```

Example output (illustrative):

```text
Valid=True Valid
Ready=True Ready
NAME                 AGE
gpustack-fnv64-...   12s
```

The operator verifies that every zone belongs to one region, stamps `topology.gpustack.ai/profile=fnv64-<hash>`
onto matched `Node`s, and materializes the Kueue `Topology` resource. If `Ready` is `False`, match the
condition reason in [Troubleshooting](#troubleshooting).

### Step 2: Request zone placement

Constrain a two-`Pod` (`size: 2`) serving replica to a single availability zone:

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
       acceleratorType: gpustack--nvidia-a10g-linux-amd64
       resources:
         accelerator: "1"
+      topology:
+        requiredLevel: topology.kubernetes.io/zone
```

Apply the deployment and verify that both member `Pod`s of replica `r0` land in the same zone:

```bash
kubectl -n team-a get pods -l app.kubernetes.io/instance=qwen-distributed \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.nodeName}{"\n"}{end}' |
  while read -r pod node; do
    zone=$(kubectl get node "$node" -o jsonpath='{.metadata.labels.topology\.kubernetes\.io/zone}')
    printf "%s\t%s\t%s\n" "$pod" "$node" "$zone"
  done
```

If free GPUs are split across zones without two free GPUs in any single zone, Kueue holds the entire
replica `Pending` instead of stranding half-scheduled `Pod`s.

## Rack placement

### Step 3: Publish an inventory snapshot

Changing ordered hierarchy `levels` triggers `HoldAndDrain` queue migration; review
[Live hierarchy changes](../../modules/topology/operations.md#live-hierarchy-changes) before running
this step on shared clusters. Delete `cloud-zones` first so two sources do not claim the same `Node`s:

```bash
kubectl delete topologysource cloud-zones
```

Save `topology-inventory.yaml` (replacing `worker-a` and `worker-b` with your `Node` names) to publish
a three-level `ConfigMap` rack snapshot:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: topology-inventory
  namespace: gpustack-system
data:
  snapshot.yaml: |
    apiVersion: worker.gpustack.ai/v1alpha1
    kind: TopologySourceConfiguration
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

Apply `topology-inventory.yaml` and wait until `data-center` is `Ready` and queues report
`TopologyReady=True`:

```bash
kubectl apply -f topology-inventory.yaml
kubectl get topologysource data-center \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'
kubectl get node worker-a -o jsonpath='{.metadata.labels.topology\.gpustack\.ai/rack}{"\n"}'
```

Example output (illustrative):

```text
Valid=True Valid
Ready=True Ready
rack-01
```

If `Valid=False` or `Ready=False`, fix `ConfigMap/topology-inventory` or `TopologySource/data-center`
rather than editing NFD `NodeFeature` objects directly; see
[Verification](../../modules/topology/operations.md#verification).

### Step 4: Request rack placement

Narrow `qwen-distributed` from zone-level placement to rack-level placement:

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

Verify that the rolled replica schedules all `size` `Pod`s on `Node`s sharing one
`topology.gpustack.ai/rack` label (a single rack must hold enough GPUs for the full `size`).

### Step 5: Update rack assignment

When `worker-b` is re-cabled to `rack-03`, update `ConfigMap/topology-inventory` and increment
`revision`:

```diff
 apiVersion: v1
 kind: ConfigMap
 metadata:
   name: topology-inventory
   namespace: gpustack-system
 data:
   snapshot.yaml: |
     apiVersion: worker.gpustack.ai/v1alpha1
     kind: TopologySourceConfiguration
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

Confirm that `worker-b` updates its rack label while keeping the same `topology.gpustack.ai/profile`
hash:

```bash
kubectl get node worker-b \
  -o jsonpath='{.metadata.labels.topology\.gpustack\.ai/rack}{"\t"}{.metadata.labels.topology\.gpustack\.ai/profile}{"\n"}'
```

Because `spec.levels` did not change, the profile hash stays identical and no `HoldAndDrain` queue
migration occurs. Editing `spec.levels` changes the profile hash and drains queues; see
[Live hierarchy changes](../../modules/topology/operations.md#live-hierarchy-changes).

## Limitations

- Each `Node` must match at most one Ready `TopologySource`; zero or multiple matches fall back to the
  hostname-only profile ([Topology inventory sources](../../modules/topology/operations.md#topology-inventory-sources)).
- Domain capacity fragmentation and multi-role gang holds are covered in
  [Diagnose Pending workloads](../../modules/topology/operations.md#diagnose-pending-workloads).

## Troubleshooting

| Symptom | Signal | Resolution |
|---|---|---|
| `TopologySource` `Ready=False` | `Reason: InvalidHierarchy` or `SnapshotInvalid` | A child value maps to multiple parent values, or snapshot YAML is invalid. Fix the `ConfigMap` snapshot or node labels so levels form a strict tree. |
| `TopologySource` `Ready=False` | `Reason: OwnershipConflict` | Two writable `TopologySource`s match the same `Node`. Make their *`spec.nodeSelector`* sets disjoint. |
| `TopologySource` `Valid=False` | `Reason: ConfigMapReadFailed` | Confirm *`configMapRef.name`*, *`namespace`*, and *`key`* exist and are readable by the worker. |
| `Node` falls back to hostname profile | Selected by zero or multiple Ready sources | Inspect *`spec.nodeSelector`* across `kubectl get topologysources -o yaml` so exactly one Ready source matches the `Node`. |
| Multi-role replica reserves one `Workload` while `Pod`s wait | Joint admission gang hold | One role fits its domain while a peer role (*Prefill* or *Decode*) awaits domain capacity; check `Workload` conditions across all roles ([Diagnose Pending workloads](../../modules/topology/operations.md#diagnose-pending-workloads)). |

---

**See also** — [Topology](../../modules/topology/_index.md) (capability overview) ·
[Topology-Aware Scheduling Operations](../../modules/topology/operations.md) (`ConfigMap`, Topograph,
and Webhook sources) · [Configuration](../../modules/model-deployment/configuration.md#topology-placement)
(*`topology.requiredLevel`* field rules)

**Next** → [Shared Cache Walkthrough](../kv-cache/shared-cache.md) — stand up a shared KV cache from backend to workload.
