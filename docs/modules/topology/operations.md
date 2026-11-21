# Topology-Aware Scheduling Operations

Configure `TopologySource` inventories for fresh managed queues, constrain replica placement with
Kueue *topology-aware scheduling (TAS)*, update live hierarchies safely, and diagnose `Pending`
PodSets. Legacy non-*TAS* queues from earlier releases are not migrated automatically.

## Contents

- [Topology inventory sources](#topology-inventory-sources)
- [Topology level requests](#topology-level-requests)
- [Live hierarchy changes](#live-hierarchy-changes)
- [Verification](#verification)
- [Troubleshooting](#troubleshooting)

## Topology inventory sources

Assign at most one `Ready` `TopologySource` per `Node`. Zero or multiple matching `Ready` sources
fall back to the hostname-only profile so conflicting hierarchies never merge.

| Scenario | Inventory source |
|---|---|
| Bundled Topograph supports your cloud, fabric, DRA, or on-prem topology | Enable `topograph` in Helm values and read its labels with *`nodeLabels`*. |
| Cloud provider or admin already labels `Node`s | Use *`nodeLabels`* (read-only). |
| Inventory is exported as a versioned document | Publish YAML/JSON in a `ConfigMap` and use *`configMap`*. |
| External inventory service serves snapshots | Serve authenticated HTTPS JSON/YAML and use *`webhook`*. |

Declare only strictly nested trees (every rack belongs to one zone, and every zone to one region);
independent axes such as rack and MIG partition do not form a valid hierarchy.

### Topograph setup

Topograph is disabled by default. Enable it on Kubernetes `1.27+` with a supported provider and the
`k8s` engine. For private clusters, override `topograph.image.repository`, *`imagePullSecrets`*, and
`topograph.config.credentialsSecret`:

```yaml
topograph:
  enabled: true
  provider:
    name: aws
    params: {}
  engine:
    name: k8s
    params: {}
  nodeDataBroker:
    enabled: true
    nodeSelector:  # optional; restrict DaemonSet to NVIDIA PCI Nodes
      feature.node.kubernetes.io/pci-10de.present: "true"
```

On AWS, set the EC2 IMDSv2 hop limit to `>= 2` for `node-data-broker` and grant
`ec2:DescribeInstanceTopology` to the Topograph API `ServiceAccount` (`topograph_aws_pod_identity_enabled=true`
in the repository EKS Terraform module).

Once Topograph labels `Node`s, expose the actual published tiers from broadest to narrowest via a
read-only `TopologySource`:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: TopologySource  # cluster-scoped
metadata:
  name: cloud-fabric
spec:
  nodeSelector: {}  # {} matches every Node
  levels:  # 1..16 unique keys, broadest to narrowest; kubernetes.io/hostname appended automatically
    - topology.kubernetes.io/region
    - topology.kubernetes.io/zone
    - fabric.topograph.run/tier-2
    - fabric.topograph.run/tier-1
  nodeLabels: {}  # read-only inventory mode
```

The worker takes the longest populated prefix on each `Node`, normalizes contiguous Topograph tiers
coarsest-to-finest, appends `kubernetes.io/hostname`, and hashes the key list into
`topology.gpustack.ai/profile`.

### Existing Node labels

Use *`nodeLabels`* when another controller or cloud provider already writes `Node` labels:

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

| Condition | Reason | Resolution |
|---|---|---|
| Duplicate key in *`levels`* | Levels must be unique | Remove the repeated label key. |
| Child label present without parent | Broken parent chain on `Node` | Populate every ancestor label (missing fine suffixes are allowed). |
| `kubernetes.io/hostname` in *`levels`* | Reserved implicit leaf level | Remove `kubernetes.io/hostname` from *`levels`*. |

### ConfigMap snapshot

Store a `TopologySourceConfiguration` YAML or JSON document in the worker namespace (`gpustack-system`):

```yaml
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
      namespace: gpustack-system  # must be the worker namespace
      name: topology-inventory
    key: snapshot.yaml
    maxStaleness: 10m  # retains last valid NodeFeature on read failure until expiry
```

Snapshots validate atomically: unknown `Node`s, undeclared keys, broken parent chains, or read-only
`topology.kubernetes.io/{region,zone}` mismatches reject the revision. Valid snapshots write one NFD
`NodeFeature` per `Node`; set *`additionalWritePrefix`* (`topology.example.com/`) for custom DNS
prefixes (reserved prefixes are refused).

### Webhook snapshot

Serve the `TopologySourceConfiguration` payload (`<= 1 MiB`) over authenticated `HTTPS` (`GET`,
redirects and proxies disabled) using worker-namespace credentials:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: TopologySource
metadata:
  name: inventory-service
spec:
  nodeSelector: {}
  levels:
    - topology.gpustack.ai/rack
  webhook:
    url: https://topology-inventory.gpustack-system.svc/snapshot  # required HTTPS URL without embedded auth
    pollInterval: 1m  # positive duration; must be >= timeout
    timeout: 10s  # positive duration
    maxStaleness: 5m  # positive duration
    caBundleConfigMapRef:
      namespace: gpustack-system
      name: topology-inventory-ca  # key: ca.crt
    bearerTokenSecretRef:  # set exactly one of bearerTokenSecretRef (key: token) or tlsClientCertificateSecretRef (tls.crt/tls.key)
      namespace: gpustack-system
      name: topology-inventory-token
```

Restrict `TopologySource` and credential RBAC to cluster administrators and scope egress to the
inventory endpoint.

## Topology level requests

Set *`roles[].topology.requiredLevel`* to a configured label key (never a bare word like `zone` or a
concrete zone value) so all *`size`* `Pod`s of each replica fit inside one domain:

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: ModelDeployment
 metadata:
   name: zoned-deployment
   namespace: team-a
 spec:
   roles:
     - name: decode
       replicas: 2
       size: 4
+      topology:
+        requiredLevel: topology.kubernetes.io/zone
```

Omit *`requiredLevel`* for unconstrained placement (`kubernetes.io/hostname` is implicit; see
[Topology placement](../model-deployment/configuration.md#topology-placement)). Editing
*`requiredLevel`* updates the role hash and rolls only that role's replicas.

## Live hierarchy changes

Updating *`levels`* creates a new profile and `Topology`. Once `Node`s and `Devices` converge, the
operator creates replacement `ResourceFlavor`s, drains the `ClusterQueue` (`HoldAndDrain`), swaps
flavors at zero reservations, and restores the stop policy.

If `TopologyReady=False`, inspect its reason and migration annotations before editing again. Missing
flavors, quota mismatches, or exceeding 64 flavors per resource group preserve the previous queue
plan.

## Verification

Inspect the source, `Node` profile labels, Kueue `Topology` objects, `ResourceFlavor`s, and
`ClusterQueue` conditions:

```bash
kubectl get topologysources.worker.gpustack.ai
kubectl get nodes -L topology.gpustack.ai/profile,topology.kubernetes.io/zone
kubectl get topologies.kueue.x-k8s.io
kubectl get resourceflavors.kueue.x-k8s.io -o yaml
kubectl get clusterqueues.kueue.x-k8s.io -o yaml
```

For `ConfigMap` or webhook sources, also verify generated NFD `NodeFeature` objects (refresh the
snapshot if `Node` selector labels change later):

```bash
source_uid=$(kubectl get topologysource data-center -o jsonpath='{.metadata.uid}')
kubectl -n gpustack-system get nodefeatures -l "topology.gpustack.ai/source-uid=$source_uid" -o yaml
```

Confirm `TopologySource` is `Ready` and every managed `Node` and `Devices` object shares one
`topology.gpustack.ai/profile`. Verify each `ResourceFlavor` ends in `kubernetes.io/hostname` and
`ClusterQueue` `TopologyReady` is `True`; inspect *`topologyRequest.required`* and
*`topologyAssignment`* on `Workload`s.

### EKS zone verification

The repository EKS fixture provisions two CPU node groups across two zones (two `Node`s per zone)
with `topograph_aws_pod_identity_enabled=true`. A size-2 PodSet schedules within one zone, while a
size-3 PodSet stays `Pending` across four total `Node`s. Review AWS pricing before apply, test on
fresh queues, and destroy all billable resources afterward.

## Troubleshooting

### Diagnose Pending workloads

```bash
kubectl -n <ns> get modeldeployments.worker.gpustack.ai <name> -o jsonpath='{.status}' | jq
kubectl -n <ns> get workloads.kueue.x-k8s.io -o yaml
kubectl get clusterqueues.kueue.x-k8s.io <cq> -o jsonpath='{.status.conditions}' | jq
kubectl get nodes -L topology.gpustack.ai/profile
```

| Symptom | Signal | Next action |
|---|---|---|
| Replica stays `Pending` | `Workload` `QuotaReserved=False` with *`topologyRequest.required`* set | Free capacity inside a single domain at that level (e.g., 2 `Node`s/zone cannot fit a 3-`Pod` zone group) or relax *`requiredLevel`*. |
| Aggregate capacity exists, no fit | Mixed `topology.gpustack.ai/profile` labels across candidate `Node`s | Align `TopologySource` selectors so all pool `Node`s publish the same profile. |
| One role reserved, `Pod`s gated | `Workload` `QuotaReserved=True`, joint check `Pending` (or `Parked` after `30m`) | Free sibling role capacity in a valid domain, or recreate/reactivate `Parked` deployments. |
| Queue blocks admission | `ClusterQueue` `TopologyReady=False` or `Unknown` (`AwaitingQueueStatus`) | Check flavor profiles, Kueue `Active` status, and keep flavors `<= 64` per resource group. |
| Source not applied | `TopologySource` `Ready=False` (`Valid` or `OwnershipConflict`) | Fix broken parent chains, snapshot errors, or overlapping `TopologySource` selectors. |

---

**See also** — [Hierarchical Placement Walkthrough](../../walkthroughs/topology/hierarchical-placement.md) (end-to-end zone and rack placement) · [Model Deployment Configuration](../model-deployment/configuration.md) (*`roles[].topology.requiredLevel`* contract) · [Installation Modes](../../operate/installation-modes.md) (bundled Topograph chart ownership)

**Next** → [Model Deployment Configuration](../model-deployment/configuration.md) — configure multi-member serving roles for topology-aware placement.
