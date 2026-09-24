# Topology-Aware Scheduling Operations

> **Purpose** — enable a topology provider, publish a hierarchy, request a placement level, and
> diagnose topology-aware admission.
> **Audience** operators, users · **Prerequisites** [Topology-Aware
> Scheduling](../architecture/topology-aware-scheduling.md) · **Read time** ~16 min

This runbook starts with fresh managed queues and also covers changes to their live topology profile.
Queues created by a previously published version are outside that transition path.

## Contents

- [Choose the inventory path](#choose-the-inventory-path)
- [Enable Topograph](#enable-topograph)
- [Observe existing Node labels](#observe-existing-node-labels)
- [Publish a ConfigMap snapshot](#publish-a-configmap-snapshot)
- [Publish through an HTTPS webhook](#publish-through-an-https-webhook)
- [Request a level from ModelDeployment](#request-a-level-from-modeldeployment)
- [Verify the scheduling chain](#verify-the-scheduling-chain)
- [Change a live hierarchy](#change-a-live-hierarchy)
- [Diagnose Pending workloads](#diagnose-pending-workloads)
- [Validate on a temporary EKS cluster](#validate-on-a-temporary-eks-cluster)

## Choose the inventory path

Use one source of truth for each selected Node. See [profile selection](../architecture/topology-aware-scheduling.md#one-hierarchy-becomes-one-profile)
for how overlapping Ready sources are handled.

| Situation | Inventory path |
|---|---|
| Topograph supports the cloud, fabric, DRA, or on-premises source | Enable the bundled Topograph chart, then select its labels with a read-only `TopologySource` |
| The cloud provider or administrator already labels Nodes | Use `nodeLabels` |
| Inventory is exported as a versioned file | Store it in a ConfigMap and use `configMap` |
| Inventory is maintained by an external service | Expose the snapshot through authenticated HTTPS and use `webhook` |

List only a real nested hierarchy. Region → zone → rack is valid when every rack value belongs to
one zone and every zone value belongs to one region. Two independent dimensions, such as rack and
accelerator partition, are not an ordered hierarchy merely because both are labels.

## Enable Topograph

Topograph is silently disabled by default: the chart renders no Topograph workload or ServiceAccount
and pulls no Topograph image. A private deployment may stay in that state and use `TopologySource`;
with no source at all, the [default profile](../architecture/topology-aware-scheduling.md#one-hierarchy-becomes-one-profile)
applies. Enable Topograph only on
Kubernetes 1.27 or later, with a production provider and the `k8s` engine:

```yaml
topograph:
  enabled: true
  provider:
    name: aws
    params: {}
  engine:
    name: k8s
    params: {}
```

The parent chart pins and vendors the dependency and defaults its image to the GPUStack Docker Hub
mirror. Override `topograph.image.repository` and set `imagePullSecrets` when a private cluster uses
an internal registry. Provider credentials may be supplied with
`topograph.config.credentialsSecret`; the exact data is provider-specific.

The Topograph API server and Node observer are shared control-plane workloads. The node-data-broker
is a DaemonSet and can be restricted independently. For an NVIDIA-only broker path, use the NFD PCI
presence label rather than scheduling every Topograph component onto accelerator Nodes:

```yaml
topograph:
  nodeDataBroker:
    enabled: true
    nodeSelector:
      feature.node.kubernetes.io/pci-10de.present: "true"
```

On AWS, the broker reads instance and region identity from node-local IMDSv2, so the EC2 metadata
response hop limit must be at least 2. The API ServiceAccount, not the broker ServiceAccount, needs
`ec2:DescribeInstanceTopology`; EKS Pod Identity can provide a separate role containing only that
action. The test Terraform module enables both requirements with
`topograph_aws_pod_identity_enabled=true`.

After Topograph writes labels, create a read-only source that chooses the hierarchy to expose to
Kueue. This example combines Kubernetes cloud labels with a Topograph fabric tier:

```yaml
apiVersion: worker.gpustack.ai/v1alpha1
kind: TopologySource
metadata:
  name: cloud-fabric
spec:
  nodeSelector: {}
  levels:
    - topology.kubernetes.io/region
    - topology.kubernetes.io/zone
    - fabric.topograph.run/tier-2
    - fabric.topograph.run/tier-1
  nodeLabels: {}
```

Do not copy a tier list from another cluster; inspect the labels actually published on these Nodes.
The [profile rules](../architecture/topology-aware-scheduling.md#one-hierarchy-becomes-one-profile)
define tier normalization.

## Observe existing Node labels

Use `nodeLabels` when another trusted component already owns the values. It validates the hierarchy
but never modifies those labels:

```yaml
apiVersion: worker.gpustack.ai/v1alpha1
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

`kubernetes.io/hostname` is always appended and must not appear in `levels`. A Node may omit a fine
suffix and receive a shorter profile, but it may not carry a child level without every declared
parent.

## Publish a ConfigMap snapshot

The ConfigMap and its `TopologySource` reference must be in the worker namespace. The snapshot key
contains YAML or JSON with this schema:

```yaml
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
```

Reference that key and declare the same ordered levels:

```yaml
apiVersion: worker.gpustack.ai/v1alpha1
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

The snapshot is atomic: one unknown Node, undeclared level, invalid value, broken parent chain, or
contradictory child rejects the whole revision. The source reconciles one NFD `NodeFeature` per
described Node; NFD publishes its writable labels onto the Node. On read failure, the last valid
`NodeFeature` remains until `maxStaleness`; after expiry it is removed.

For a cloud cluster, use `nodeLabels` to consume the existing standard region and zone keys.
If a snapshot includes those keys as parents of a private rack, follow the [standard-label
boundary](../architecture/topology-aware-scheduling.md#topologysource-normalizes-other-inventories).
Use the private region and zone keys above when the inventory itself owns those facts.

Use `additionalWritePrefix` when the data center owns another DNS prefix. It must be one DNS prefix
with a trailing slash, for example `topology.example.com/`. Reserved Kubernetes, GPUStack,
Topograph, and device-feature prefixes cannot be delegated.

## Publish through an HTTPS webhook

The webhook is an authenticated `GET` that returns the same snapshot. It requires an absolute HTTPS
URL, does not follow redirects or use proxy-derived destinations, accepts at most 1 MiB, and requires
exactly one of bearer-token or client-certificate authentication.

```yaml
apiVersion: worker.gpustack.ai/v1alpha1
kind: TopologySource
metadata:
  name: inventory-service
spec:
  nodeSelector: {}
  levels:
    - topology.gpustack.ai/rack
  webhook:
    url: https://topology-inventory.gpustack-system.svc/snapshot
    pollInterval: 1m
    timeout: 10s
    maxStaleness: 5m
    caBundleConfigMapRef:
      namespace: gpustack-system
      name: topology-inventory-ca
    bearerTokenSecretRef:
      namespace: gpustack-system
      name: topology-inventory-token
```

The CA ConfigMap key is `ca.crt`; the bearer Secret key is `token`. For mTLS, replace the bearer
reference with `tlsClientCertificateSecretRef`; that Secret uses `tls.crt` and `tls.key`. All
references are restricted to the worker namespace.

Only a cluster administrator should create a `TopologySource` or its credentials. Restrict RBAC for
these objects, allow egress only to the intended endpoint and DNS path, and rotate referenced
Secrets without embedding credentials in the URL. `timeout` must not exceed `pollInterval`, and all
three durations must be positive.

## Request a level from ModelDeployment

Set `requiredLevel` on a role to require each replica's Pod group to fit in one domain at that level:

```yaml
spec:
  roles:
    - name: decode
      replicas: 2
      size: 4
      topology:
        requiredLevel: topology.kubernetes.io/zone
```

The value is a label key, not `region`, `zone`, or a concrete zone name. Valid examples include
`topology.kubernetes.io/zone`, `topology.gpustack.ai/rack`, and a selected Topograph tier. Omit the
field for unconstrained TAS placement; the [field contract](../reference/model-deployment.md#topology-placement)
defines the implicit hostname level.

Changing or removing the field participates in the existing render hash and replaces only affected
replica groups. It does not move an admitted group in place.

## Verify the scheduling chain

Read the produced objects rather than inferring success from a Ready Node:

```sh
kubectl get topologysources.worker.gpustack.ai
kubectl get nodes -L topology.gpustack.ai/profile,topology.kubernetes.io/zone
kubectl get topologies.kueue.x-k8s.io
kubectl get resourceflavors.kueue.x-k8s.io -o yaml
kubectl get clusterqueues.kueue.x-k8s.io -o yaml
```

For a ConfigMap or webhook source, inspect its NFD output before checking the projected Node labels:

```sh
source_uid=$(kubectl get topologysource data-center -o jsonpath='{.metadata.uid}')
kubectl -n gpustack-system get nodefeatures -l "topology.gpustack.ai/source-uid=$source_uid" -o yaml
kubectl get nodes -L topology.gpustack.ai/region,topology.gpustack.ai/zone,topology.gpustack.ai/rack
```

The `NodeFeature.spec.labels` map contains private inventory labels only. Standard cloud region and
zone keys may be read as parents but must not appear there. Refresh the ConfigMap snapshot after
changing selector labels on an existing Node; writing sources do not watch Node label drift.

The source must be Ready, every managed Node must have one profile, every generated flavor must pin
that profile and reference a Topology, and the ClusterQueue `TopologyReady` condition must be True.
The Topology's final level must be `kubernetes.io/hostname`.

For a submitted ModelDeployment, inspect the generated Workload rather than creating one by hand:

```sh
kubectl get workloads.kueue.x-k8s.io -A -o yaml
kubectl get modeldeployments.worker.gpustack.ai -A -o yaml
```

Confirm the Workload PodSet carries `topologyRequest.required` when requested and inspect its
`topologyAssignment` after admission. Then compare assigned domain values with the Nodes on which
the Pods actually run. An assignment may name the hostname level even when a one-Pod Workload
requested zone; verify the request and the admitted ResourceFlavor's `topologyName` separately.

## Change a live hierarchy

Publish a complete replacement hierarchy, such as region → zone → rack, through the selected
`TopologySource`. Its ordered level keys produce a new profile and Topology. The Node and its
`Devices` ledger must converge on the same `topology.gpustack.ai/profile` before accelerator
admission can use the replacement flavor.

Record the managed ClusterQueue's name, UID, stop policy, `status.reservingWorkloads`, and flavor
references before and after the change. GPUStack creates replacement flavors first, sets
`HoldAndDrain`, waits for zero reservations, switches the flavor set on the same queue, then restores
the previous stop policy. Kueue evicts and later readmits affected Workloads, so plan for a service
interruption. Old flavors and their Topology disappear only after references clear.

If `TopologyReady=False`, read its reason and the queue's migration annotations before changing
anything else. A missing replacement flavor, immutable object drift, too many required flavors,
or quota that cannot be conserved leaves the last complete queue plan in place. An old non-TAS queue
from a previous release is not adopted; use fresh managed objects.

## Diagnose Pending workloads

Pending is the safe result when no single domain satisfies the full PodSet. Check in this order:

1. Read the ModelDeployment progress message for the role and replica.
2. Read the generated Workload conditions and topology request/assignment.
3. Check the ClusterQueue `TopologyReady` condition and resource-group flavor count.
4. Check every referenced ResourceFlavor's profile label and `topologyName`.
5. Compare the requested level with each Kueue Topology and the actual Node labels.
6. Count CPU, memory, and requested extended resources inside one domain, not cluster-wide.

A four-node cluster with two Nodes per zone can admit a three-Pod request cluster-wide but cannot
admit it at zone level. Likewise, capacity split between a zone profile and a hostname-only profile
cannot satisfy one zone-constrained PodSet.

A multi-role deployment can show one Workload with `QuotaReserved=True` and a topology assignment,
counted in the queue's `status.reservingWorkloads`, while its joint check reads `Pending` and no role
Pod is bound. That group fits and is [held for its set](../architecture/topology-aware-scheduling.md#a-modeldeployment-request-is-per-replica);
the other roles are the ones short of a domain. Free their capacity or delete the deployment.

If the [flavor limit](../architecture/topology-aware-scheduling.md#capacity-and-lifecycle-limits)
is exceeded, reduce profile fragmentation or hardware identity variants, then let reconciliation
retry the complete plan.

## Validate on a temporary EKS cluster

The repository EKS fixture can create two CPU node groups pinned to different availability zones.
Use two inexpensive Nodes per zone to prove both positive and negative placement: a size-two PodSet
fits one zone, while a size-three PodSet remains Pending although total cluster capacity is four.

Enabling `topograph_aws_pod_identity_enabled` also configures the broker's IMDSv2 hop limit and the
minimal API Pod Identity permission. Verify four Ready Nodes, two distinct zone labels, broker and
API readiness, source reconciliation, TAS assignments, and the negative Pending observation.

This is paid infrastructure. Review current AWS prices, quotas, and instance availability before
apply. Use fresh queues for every test. After validation, destroy the fixture and verify the state
and charge-bearing resources are gone, or explicitly transfer the Terraform state to the next owner
before leaving the cluster running. The test does not prove an upgrade of previously published queues.

---

**See also** — [Topology-Aware Scheduling](../architecture/topology-aware-scheduling.md) (mechanism
and boundaries) · [Model Deployment Reference](../reference/model-deployment.md) (field contract) ·
[Installation Modes](../architecture/installation-modes.md) (chart ownership)

**Next** → [Model Deployment Reference](../reference/model-deployment.md) — configure the serving
roles that consume topology-aware admission.
