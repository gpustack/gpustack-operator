# Architecture

GPUStack Operator schedules accelerator workloads on Kubernetes. It discovers node hardware and
builds isolated Kueue scheduling pools for `Instance`s, `ModelDeployment`s, and `Pod`s. Model
delivery resolves and caches weights, topology placement constrains failure domains, *remote direct
memory access (RDMA)* accelerates transfers, and shared KV cache stores reusable prefixes.

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

The `gpustack-operator` binary provides five subcommands:

| Component | Deployment | Role |
|---|---|---|
| `worker` | Control-plane `Deployment` | Serves APIs and reconciles workloads, queues, and supporting resources. |
| `device-manager` | Vendor `DaemonSet` | Discovers accelerators and NICs,<br/>publishes capacity, and allocates devices. |
| `model-manager` | Per-node `DaemonSet` | Caches verified model weights and mounts them through CSI. |
| `model-transfer` | Vendor source `DaemonSet` | Runs the source broker and worker service for remote weight delivery. |
| `worker-gateway` | Separate installation | Aggregates `InstanceType`s and capacity across clusters into one fleet view. |

The Helm chart bundles [Node Feature Discovery (NFD)](https://github.com/kubernetes-sigs/node-feature-discovery),
[Kueue](https://github.com/kubernetes-sigs/kueue), and two CSI drivers. Topograph is optional and
disabled until an administrator configures a topology provider. See
[Installation Modes](../operate/installation-modes.md) for deployment modes and runtime settings.

## Workload flow

GPU `Instance`s run interactive containers with optional SSH access. `ModelDeployment`s run managed
inference roles, routers, and *prefill/decode (PD)* or *encode/prefill/decode (EPD)* groups. Both
consume the same pool capacity and admission chain, while model delivery and KV cache attach only
when requested.

![Workload and scheduling flow](../assets/diagrams/architecture-overview.svg)

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

Every optional service in the diagram can be omitted. An ordinary `Pod` can also request accelerator
resources directly through its namespace `LocalQueue`.

## Device scheduling

Device scheduling runs in four stages:

1. NFD inspects node hardware and publishes feature labels.
2. Device Manager detects accelerators and NICs and updates each node's `Devices` ledger.
3. The worker derives schedulable CPU and accelerator capacity labels from that inventory.
4. The worker creates an `AcceleratorType` per accelerated pool (plus `InstanceType`s when enabled)
   sharing one isolated `ClusterQueue`.

[Heterogeneous Devices](../modules/devices/_index.md) and
[Preflight Checks](../modules/devices/preflight.md) cover node readiness and isolation modes.
[Accelerator Requests](../modules/devices/requests.md) defines resource keys, capacity views, and
pool selection. See [RDMA Operations](../modules/rdma/operations.md) for fabric requests and
[Topology-Aware Scheduling Operations](../modules/topology/operations.md) for domain placement.

## Model delivery

A `ModelArtifact` separates weight sources and credentials from workload specs. It resolves and pins
weights from Hugging Face, ModelScope, an OCI image, or a `PersistentVolumeClaim`.

Workloads load weights directly in the engine (`Engine`), mount verified node cache paths through the
Model Manager CSI driver (`Node`), or mount an image or PVC. Remote delivery streams weights from
prepared source nodes through `model-transfer`; see
[Remote Weight Delivery](../modules/model-delivery/remote.md).

`ModelStore` and `ModelStoreBinding` govern node selection and namespace cache quotas.
`NodeModelStore` reports per-node cache state, and `ModelPrefetch` warms weights ahead of serving
without holding GPU quota. See [Model Delivery](../modules/model-delivery/_index.md),
[Model Artifact](../modules/model-delivery/artifact.md), and the
[Model Prefetch Walkthrough](../walkthroughs/model-delivery/prefetch.md).

## KV cache

A `KVCacheBackend` deploys a shared Mooncake cache cluster or connects to an external endpoint.
`KVCachePool` and `KVCachePoolBinding` grant namespace access, reuse domains, and memory ceilings
independently of GPU quota. Compatible `ModelDeployment`s and labeled `Pod`s receive client settings
at creation; see [KV Cache](../modules/kv-cache/_index.md).

[Model Deployment](../modules/model-deployment/_index.md) combines these capabilities with engine,
router, and scaling policies. [GPU Instances](../modules/instances/_index.md) run interactive
containers on the same accelerator pools.

## Request admission

The `Pod` webhook validates accelerator requests before Kueue reserves quota. Kueue `AdmissionCheck`s
verify per-device fit and multi-role gang readiness before kube-scheduler, kubelet, and the allocator
bind devices; see [Accelerator Requests](../modules/devices/requests.md).

`AcceleratorType` and `InstanceType` status update as workloads claim and release accelerators. Run
`kubectl get acctype,instancetype -w` to watch live pool capacity, or follow the
[Heterogeneous Devices Walkthrough](../walkthroughs/devices/scheduling.md).

## Vocabulary

| Term | Meaning |
|---|---|
| Pool | `Node`s with matching CPU, accelerator, OS, and architecture identities, served by one isolated `ClusterQueue`. |
| `AcceleratorType` | Cluster-scoped accelerator pool for serving and prefetch workloads; short name `acctype`. |
| `InstanceType` | Cluster-scoped workspace sizing and capacity view for `Instance`s. |
| Allocation mode | Whole cards, shared cards, logical slices, or hardware partitions. |
| Credits | Quota units (`credits.gpustack.ai/<manufacturer>`) metered on a pool `ClusterQueue`. |
| Four capacity views | Exclusive (`EX`), shared (`SH`), sliced (`SL`), and partitioned (`PT`) counts on pool status. |
| `Devices` ledger | Per-node custom resource recording discovered hardware and active allocations. |
| `LocalQueue` | Namespace queue entrance that routes a workload into its target `ClusterQueue`. |
| `ModelArtifact` | Namespaced weight source and resolved manifest digest referenced by workloads. |
| `NodeModelStore` | Per-node cache configuration and observed digest state. |
| `ModelPrefetch` | Namespaced request to warm a `ModelArtifact` onto bound `ModelStore` nodes. |

A physical card or module is a *device*, each schedulable unit on it is an *accelerator*, and the
Kubernetes extended resource key a container requests is a *resource*.

## Related documentation

- [Heterogeneous Devices Walkthrough](../walkthroughs/devices/scheduling.md): submit workloads and inspect pool updates.
- [GPU Instances](../modules/instances/_index.md): launch interactive containers with SSH access.
- [Model Deployment](../modules/model-deployment/_index.md): configure inference engines, routers, and scalers.
- [Model Delivery](../modules/model-delivery/_index.md): resolve, prefetch, and mount model weights.
- [KV Cache](../modules/kv-cache/_index.md): attach workloads to shared prefix caches.
- [Installation Modes](../operate/installation-modes.md): compare chart mode, image mode, and settings precedence.

---

**See also** — [Accelerator Requests](../modules/devices/requests.md) (resource keys and request rules) ·
[Vendor Prerequisites](vendor-prerequisites.md) (host drivers and third-party components)

**Next** → [Heterogeneous Devices Walkthrough](../walkthroughs/devices/scheduling.md) — follow a workload from submission to allocation.
