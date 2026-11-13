# Architecture

GPUStack Operator manages accelerator-backed workloads on Kubernetes. It discovers devices and turns
node capacity into schedulable pools. GPU `Instance`s and model deployments use those pools.
Model delivery resolves and supplies weights. Topology placement supports locality. *Remote direct
memory access (RDMA)* networking supports fast transfers. Shared KV cache stores reusable inference state.

## Contents

- [Components](#components)
- [Workload flow](#workload-flow)
- [Device scheduling](#device-scheduling)
- [Model delivery](#model-delivery)
- [KV cache](#kv-cache)
- [Request admission](#request-admission)
- [Vocabulary](#vocabulary)
- [Related documentation](#related-documentation)

## Components

The `gpustack-operator` binary provides these subcommands:

| Component | Deployment | Role |
|---|---|---|
| `worker` | Control-plane `Deployment` | Serves APIs and manages workloads, queues and supporting resources. |
| `device-manager` | Vendor `DaemonSet` | Discovers devices and network interfaces,<br/>reports capacity and allocates devices. |
| `model-manager` | Per-node `DaemonSet` | Caches model weights and mounts them into workloads using node delivery. |
| `model-transfer` | Vendor source `DaemonSet` | Runs the source broker and worker control service for remote weight delivery. |
| `worker-gateway` | Installed separately | Combines `InstanceType`s and capacity from multiple clusters into a fleet view. |

The chart also includes [Node Feature Discovery (NFD)](https://github.com/kubernetes-sigs/node-feature-discovery),
[Kueue](https://github.com/kubernetes-sigs/kueue) and two CSI drivers. Topograph is optional and remains
disabled until an administrator selects a provider. See [Installation Modes](/gpustack-operator/v0.11.0/docs/operate/installation-modes/index.md)
for the deployment choices and [Internals](/gpustack-operator/v0.11.0/docs/contribute/internals/index.md) for contributor details.

## Workload flow

GPU `Instance`s provide containers with optional SSH access. Model deployments run inference engines
and, where configured, routers or *prefill*/*decode* groups. Both use the same device capacity and
admission chain. Model delivery and KV cache are optional services alongside that chain.

![Workload and scheduling flow](/gpustack-operator/v0.11.0/docs/assets/diagrams/architecture-overview.svg)

<!--
flowchart TD
    REQUESTS["ModelDeployments, Instances and Pods"] -> WORKER["Worker controllers"]
    NFD["NFD and topology sources"] -> WORKER
    DM["Device Manager and Devices ledger"] -> WORKER
    WORKER -> QUEUES["Pool flavors, isolated queues and topology admission"]
    QUEUES -> PODS["Admitted workload Pods"]
    DM -> PODS
    ARTIFACT["ModelArtifact"] -> CSI["Optional node cache and CSI delivery"]
    CSI -> PODS
    CACHE["Optional KV pool and binding"] -> PODS
-->

The diagram shows the main dependencies. It does not require every workload to use a model artifact,
RDMA, a topology source or a shared cache. An ordinary `Pod` can request device resources directly.

## Device scheduling

Device scheduling follows this chain:

1. NFD identifies node hardware and labels the nodes.
2. The Device Manager discovers accelerators and network interfaces and maintains the `Devices`
   inventory and allocation record.
3. The Worker derives schedulable CPU and accelerator capacity from that inventory.
4. The Worker derives an `AcceleratorType` for each accelerated pool (and `InstanceType`s when enabled), sharing
   the canonical pool `ClusterQueue` while topology and admission checks constrain placement.

[Device Discovery](/gpustack-operator/v0.11.0/docs/modules/devices/discovery/index.md) covers node labeling and device discovery.
[Scheduling Chain](/gpustack-operator/v0.11.0/docs/modules/devices/scheduling/index.md) covers capacity, queues and workload pool selection.
[RDMA Operations](/gpustack-operator/v0.11.0/docs/modules/rdma/operations/index.md) explains network resource requests, while
[Topology-Aware Scheduling](/gpustack-operator/v0.11.0/docs/modules/topology/scheduling/index.md) explains placement domains.

## Model delivery

Model delivery separates model weights and credentials from workload configuration. A `ModelArtifact`
resolves and pins a Hugging Face repository, ModelScope repository, container image or
`PersistentVolumeClaim`. Workloads reference the artifact instead of repeating its source and credentials.

Depending on the delivery mode, weights are fetched in the workload `Pod` (`Engine`), cached and
mounted read-only on the node by the Model Manager's CSI driver (`Node`), or mounted from an
existing volume or image. Remote delivery can serve weights from source nodes through the
`model-transfer` broker and vendor worker. See [Remote Weight Delivery](/gpustack-operator/v0.11.0/docs/modules/model-delivery/remote/index.md).

`ModelStore` and `ModelStoreBinding` manage cache budgets and namespace grants. `NodeModelStore`
tracks per-node cache state. `ModelPrefetch` warms weights before use. These resources operate
independently of GPU admission. See [Model Delivery](/gpustack-operator/v0.11.0/docs/modules/model-delivery/index.md),
[Model Artifact](/gpustack-operator/v0.11.0/docs/modules/model-delivery/artifact/index.md), and the
[Model Prefetch Walkthrough](/gpustack-operator/v0.11.0/docs/walkthroughs/model-delivery/prefetch/index.md).

## KV cache

A shared KV cache stores reusable inference state. A backend runs the cache service or points to an
external one. Pools and namespace bindings provide access and quota grants. Compatible workloads
receive the cache configuration when their `Pod`s are created. The service is optional. Its quota
is separate from accelerator quota. See [KV Cache](/gpustack-operator/v0.11.0/docs/modules/kv-cache/index.md).

[Model Deployment](/gpustack-operator/v0.11.0/docs/modules/model-deployment/index.md) brings these services together with engine,
router and replica configuration. [GPU Instances](/gpustack-operator/v0.11.0/docs/modules/instances/index.md) support interactive
containers and SSH access using the same accelerator allocation.

## Request admission

The `Pod` webhook validates an accelerator request before Kueue reserves quota. Admission checks
confirm that devices can satisfy it, and the scheduler, kubelet and allocator complete the
allocation. [Admission](/gpustack-operator/v0.11.0/docs/modules/devices/admission/index.md#gates) owns the gate sequence and
its failure behavior.

`AcceleratorType` and `InstanceType` status report the remaining capacity as workloads allocate and release
devices. Use `kubectl get acctype,instancetype -w` to watch those changes. The
[Walkthrough](/gpustack-operator/v0.11.0/docs/walkthroughs/devices/scheduling/index.md) shows the request and its resulting Kubernetes resources.

## Vocabulary

| Term | Meaning |
|---|---|
| Pool | `Node`s with compatible CPU, accelerator, operating system and architecture identities, served by an isolated queue. |
| `AcceleratorType` | Cluster-scoped serving and prefetch accelerator pool; short name `acctype`. |
| `InstanceType` | Workspace sizing and available capacity for an `Instance`. |
| Allocation mode | Whole accelerators, shared accelerators, logical slices or hardware partitions. |
| Credits | [Quota accounting units](/gpustack-operator/v0.11.0/docs/modules/devices/admission/index.md#gate-2-kueue-credits). |
| Four capacity views | [Available allocation modes](/gpustack-operator/v0.11.0/docs/modules/devices/admission/index.md#four-view-status) reported by an `AcceleratorType` or `InstanceType`. |
| `Devices` ledger | The per-node inventory and record of accelerator allocations. |
| `LocalQueue` | The namespace queue through which a workload enters its pool. |
| `ModelArtifact` | A namespaced model source and resolved weight identity referenced by workloads. |
| `NodeModelStore` | The configuration and observed cache state maintained for one node. |
| `ModelPrefetch` | A request to cache weights before a workload mounts them, authorized by a model-store binding. |

The hardware terms Device, Accelerator and Resource are defined in
[Device Discovery](/gpustack-operator/v0.11.0/docs/modules/devices/discovery/index.md#device-accelerator-resource).

## Related documentation

- [Walkthrough](/gpustack-operator/v0.11.0/docs/walkthroughs/devices/scheduling/index.md) — submit a workload and inspect the scheduling chain.
- [GPU Instances](/gpustack-operator/v0.11.0/docs/modules/instances/index.md) — configure an interactive container and SSH access.
- [Model Deployment](/gpustack-operator/v0.11.0/docs/modules/model-deployment/index.md) — configure inference engines and replicas.
- [Model Delivery](/gpustack-operator/v0.11.0/docs/modules/model-delivery/index.md) — resolve, cache and mount model weights.
- [KV Cache](/gpustack-operator/v0.11.0/docs/modules/kv-cache/index.md) — connect inference workloads to a shared cache.
- [Installation Modes](/gpustack-operator/v0.11.0/docs/operate/installation-modes/index.md) — choose how to install the operator.

---

**See also** — [Accelerator Requests](/gpustack-operator/v0.11.0/docs/modules/devices/requests/index.md) ·
[Settings](/gpustack-operator/v0.11.0/docs/reference/settings/index.md) · [All documentation](https://github.com/gpustack/gpustack-operator/blob/71cc4d7292144f1ac46ec916e43da8e452555090/docs/README.md)

**Next** → [Walkthrough](/gpustack-operator/v0.11.0/docs/walkthroughs/devices/scheduling/index.md) — follow a workload from submission to allocation.
