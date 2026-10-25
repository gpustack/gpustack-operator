# Architecture

GPUStack Operator manages accelerator-backed workloads on Kubernetes. It discovers devices, turns
node capacity into schedulable pools, and runs GPU `Instance`s and model deployments against those
pools. Model delivery resolves and supplies model weights. Topology placement, *remote direct memory
access (RDMA)* networking and shared KV cache support locality, fast transfers and reusable inference state.

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
| `worker` | Control-plane `Deployment` | Serves the public APIs and manages workloads, queues and supporting resources. |
| `device-manager` | One `DaemonSet` per manufacturer | Discovers accelerators and network interfaces, reports available capacity and allocates devices to containers. |
| `model-manager` | Per-node `DaemonSet` | Caches model weights and mounts them into workloads using node delivery. |
| `worker-gateway` | Installed separately | Combines `InstanceType`s and capacity from multiple clusters into a fleet view. |

The chart also includes [Node Feature Discovery (NFD)](https://github.com/kubernetes-sigs/node-feature-discovery),
[Kueue](https://github.com/kubernetes-sigs/kueue) and two CSI drivers. Topograph is optional and remains
disabled until an administrator selects a provider. See [Installation Modes](/gpustack-operator/main/docs/operate/installation-modes/index.md)
for the deployment choices and [Internals](/gpustack-operator/main/docs/contribute/internals/index.md) for contributor details.

## Workload flow

GPU `Instance`s provide containers with optional SSH access. Model deployments run inference engines
and, where configured, routers or *prefill*/*decode* groups. Both use the same device capacity and
admission chain. Model delivery and KV cache are optional services alongside that chain.

```mermaid
flowchart TB
    REQUEST["GPU Instances<br/>ModelDeployment / Pods"]
    WORKER["Worker"]
    DEVICES["Device Manager<br/>Accelerators / RDMA"]
    TOPOLOGY["Topology"]
    QUEUES["Kueue<br/>Pools and queues"]
    ARTIFACT["Model Delivery<br/>ModelArtifact"]
    MODELS["Model Manager<br/>Node cache"]
    CACHE["KV Cache<br/>Backends and pools"]
    STORE["Cache service"]
    PODS["Workload Pods<br/>SSH / inference"]

    REQUEST --> WORKER
    WORKER --> QUEUES
    DEVICES -- "capacity" --> QUEUES
    TOPOLOGY -- "placement" --> QUEUES
    QUEUES -- "admission" --> PODS
    DEVICES -- "device access" --> PODS
    REQUEST -- "artifact reference" --> ARTIFACT
    WORKER -- "resolution and authorization" --> ARTIFACT
    ARTIFACT -- "node delivery" --> MODELS
    MODELS -- "weights" --> PODS
    ARTIFACT -- "Engine / claim / image delivery" --> PODS
    CACHE --> STORE
    STORE -. "optional KV reuse" .-> PODS
```

The diagram shows the main dependencies. It does not require every workload to use a model artifact,
RDMA, a topology source or a shared cache. An ordinary `Pod` can request device resources directly.

## Device scheduling

Device scheduling follows this chain:

1. NFD identifies node hardware and labels the nodes.
2. The Device Manager discovers accelerators and network interfaces and maintains the `Devices`
   inventory and allocation record.
3. The Worker derives schedulable CPU and accelerator capacity from that inventory.
4. The Worker creates `InstanceType`s and Kueue resources. Each pool has an isolated queue; topology
   profiles constrain placement, and admission checks verify whether individual accelerators fit.

[Device Discovery](/gpustack-operator/main/docs/modules/devices/discovery/index.md) covers node labeling and device discovery.
[Scheduling Chain](/gpustack-operator/main/docs/modules/devices/scheduling/index.md) covers capacity and queues.
[RDMA Operations](/gpustack-operator/main/docs/modules/rdma/operations/index.md) explains network resource requests, while
[Topology-Aware Scheduling](/gpustack-operator/main/docs/modules/topology/scheduling/index.md) explains placement domains.

## Model delivery

Model delivery separates model weights and access credentials from workload configuration. A
`ModelArtifact` resolves and pins a Hugging Face repository, ModelScope repository, container image,
or `PersistentVolumeClaim`; workloads reference the artifact rather than repeating its source and
credentials.

Depending on the delivery mode, weights are fetched in the workload `Pod` (`Engine`), cached and
mounted read-only on the node by the Model Manager's CSI driver (`Node`), or mounted from an
existing volume or image.

`ModelStore` and `ModelStoreBinding` manage pool cache budgets and namespace grants,
`NodeModelStore` tracks per-node cache state, and `ModelPrefetch` pre-warms weights — all
independently of GPU admission. See [Model Delivery](/gpustack-operator/main/docs/modules/model-delivery/index.md),
[Model Artifact](/gpustack-operator/main/docs/modules/model-delivery/artifact/index.md), and the
[Model Prefetch Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-delivery/prefetch/index.md).

## KV cache

A shared KV cache stores reusable inference state. A backend runs the cache service or points to an
external one; pools and namespace bindings provide access and quota grants. Compatible workloads
receive the cache configuration when their `Pod`s are created. This service is optional, and its quota
is separate from accelerator quota. See [KV Cache](/gpustack-operator/main/docs/modules/kv-cache/index.md).

[Model Deployment](/gpustack-operator/main/docs/modules/model-deployment/index.md) brings these services together with engine,
router and replica configuration. [GPU Instances](/gpustack-operator/main/docs/modules/instances/index.md) support interactive
containers and SSH access using the same accelerator allocation.

## Request admission

The `Pod` webhook validates an accelerator request before Kueue reserves quota. Admission checks
confirm that devices can satisfy it, and the scheduler, kubelet and allocator complete the
allocation. [Admission](/gpustack-operator/main/docs/modules/devices/admission/index.md#five-gates) owns the gate sequence and
its failure behavior.

`InstanceType` status reports the remaining capacity as workloads allocate and release devices. Use
`kubectl get instancetype -w` to watch those changes. The [Walkthrough](/gpustack-operator/main/docs/walkthroughs/devices/scheduling/index.md) shows the
request and its resulting Kubernetes resources.

## Vocabulary

| Term | Meaning |
|---|---|
| Pool | `Node`s with compatible CPU, accelerator, operating system and architecture identities, served by an isolated queue. |
| `InstanceType` | The resource configuration and available capacity offered by a pool. |
| Allocation mode | Whole accelerators, shared accelerators, logical slices or hardware partitions. |
| Credits | [Quota accounting units](/gpustack-operator/main/docs/modules/devices/admission/index.md#gate-2-kueue-credits). |
| Four capacity views | [Available allocation modes](/gpustack-operator/main/docs/modules/devices/admission/index.md#four-view-status) reported by an `InstanceType`. |
| `Devices` ledger | The per-node inventory and record of accelerator allocations. |
| `LocalQueue` | The namespace queue through which a workload enters its pool. |
| `ModelArtifact` | A namespaced model source and resolved weight identity referenced by workloads. |
| `NodeModelStore` | The configuration and observed cache state maintained for one node. |
| `ModelPrefetch` | A request to cache weights before a workload mounts them, authorized by a model-store binding. |

The hardware terms Device, Accelerator and Resource are defined in
[Device Discovery](/gpustack-operator/main/docs/modules/devices/discovery/index.md#device-accelerator-resource).

## Related documentation

- [Walkthrough](/gpustack-operator/main/docs/walkthroughs/devices/scheduling/index.md) — submit a workload and inspect the scheduling chain.
- [GPU Instances](/gpustack-operator/main/docs/modules/instances/index.md) — configure an interactive container and SSH access.
- [Model Deployment](/gpustack-operator/main/docs/modules/model-deployment/index.md) — configure inference engines and replicas.
- [Model Delivery](/gpustack-operator/main/docs/modules/model-delivery/index.md) — resolve, cache and mount model weights.
- [KV Cache](/gpustack-operator/main/docs/modules/kv-cache/index.md) — connect inference workloads to a shared cache.
- [Installation Modes](/gpustack-operator/main/docs/operate/installation-modes/index.md) — choose how to install the operator.

---

**See also** — [Accelerator Requests](/gpustack-operator/main/docs/modules/devices/requests/index.md) ·
[Settings](/gpustack-operator/main/docs/reference/settings/index.md) · [All documentation](https://github.com/gpustack/gpustack-operator/blob/1a5b0e3eb23d3c3c565278681c906568dfd18e00/docs/README.md)

**Next** → [Walkthrough](/gpustack-operator/main/docs/walkthroughs/devices/scheduling/index.md) — follow a workload from submission to allocation.
