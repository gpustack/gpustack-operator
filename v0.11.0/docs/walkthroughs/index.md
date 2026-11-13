# Walkthroughs

Follow a complete example, then compare the resource output with the observed result.
Each module has its own directory. Configuration contracts remain in the capability guides.

## Contents

- [Choose a walkthrough](#choose-a-walkthrough)
- [Connect configurations](#connect-configurations)
- [Read the evidence](#read-the-evidence)

## Choose a walkthrough

Serving configurations use *tensor parallelism (TP)*, *data parallelism (DP)* and *expert parallelism (EP)*.
The [Model Deployment guides](/gpustack-operator/v0.11.0/docs/modules/model-deployment/index.md) explain their configuration contracts.

| Module | Walkthrough | Task |
|---|---|---|
| Heterogeneous Devices | [Heterogeneous Devices](/gpustack-operator/v0.11.0/docs/walkthroughs/devices/scheduling/index.md) | Follow detection, queues, admission, and accelerator allocation. |
| RDMA Networking | [RDMA Networking](/gpustack-operator/v0.11.0/docs/walkthroughs/rdma/network-endpoints/index.md) | Request and verify shared and exclusive RDMA endpoints with NUMA alignment. |
| Topology | [Hierarchical Placement](/gpustack-operator/v0.11.0/docs/walkthroughs/topology/hierarchical-placement/index.md) | Publish zone and rack hierarchies and constrain replicas to a topology domain. |
| KV Cache | [Shared Cache](/gpustack-operator/v0.11.0/docs/walkthroughs/kv-cache/shared-cache/index.md) | Create a backend, pool, namespace grant, and serving workload. |
| Model Delivery | [Model Prefetch](/gpustack-operator/v0.11.0/docs/walkthroughs/model-delivery/prefetch/index.md) | Cache and verify model weights before inference. |
| Model Deployment | [Preparation and choices](/gpustack-operator/v0.11.0/docs/walkthroughs/model-deployment/index.md) | Choose a serving configuration and follow its hardware, model and `AcceleratorType` requirements. |
| Model Deployment | [Prefill and Decode](/gpustack-operator/v0.11.0/docs/walkthroughs/model-deployment/prefill-decode/index.md) | Disaggregate *prefill* and *decode* roles fronted by a router. |
| Model Deployment | [Encode, Prefill and Decode](/gpustack-operator/v0.11.0/docs/walkthroughs/model-deployment/encode-prefill-decode/index.md) | Separate multimodal *encode*, *prefill* and *decode* roles. |
| Model Deployment | [Elastic EP](/gpustack-operator/v0.11.0/docs/walkthroughs/model-deployment/elastic-ep/index.md) | Grow *DP* width 2 → 4 with *TP* = 2. |
| Model Deployment | [External DP](/gpustack-operator/v0.11.0/docs/walkthroughs/model-deployment/external-dp/index.md) | Deploy four fixed H100 *DP* ranks and verify captured per-rank inference and Router coverage. |
| Model Deployment | [Multi-Host](/gpustack-operator/v0.11.0/docs/walkthroughs/model-deployment/multi-host/index.md) | Deploy distributed model instances across multiple GPU hosts. |
| Model Deployment | [Scaling](/gpustack-operator/v0.11.0/docs/walkthroughs/model-deployment/scaling/index.md) | Observe and activate scaling for a deployment. |

## Connect configurations

These walkthroughs connect resources from more than one module. Follow each page's prerequisites and
limitations; its example does not establish that other combinations have been tested.

| Configuration | Walkthrough | Linkage |
|---|---|---|
| Model delivery and serving | [Model Prefetch](/gpustack-operator/v0.11.0/docs/walkthroughs/model-delivery/prefetch/index.md) | A store binding grants cache access; a deployment references the artifact. |
| Shared cache and serving | [Shared Cache](/gpustack-operator/v0.11.0/docs/walkthroughs/kv-cache/shared-cache/index.md) | A backend supplies a pool; a namespace binding grants a deployment access. |
| RDMA and shared cache | [RDMA Networking](/gpustack-operator/v0.11.0/docs/walkthroughs/rdma/network-endpoints/index.md#step-4-kv-cache-over-rdma) | The cache backend uses an RDMA fabric and requests interfaces. |
| Delivery, serving and scaling | [Scaling](/gpustack-operator/v0.11.0/docs/walkthroughs/model-deployment/scaling/index.md) | A deployment references an artifact; a scaler controls its replica counts. |
| Topology and serving | [Hierarchical Placement](/gpustack-operator/v0.11.0/docs/walkthroughs/topology/hierarchical-placement/index.md) | Topology inventory supplies domains that constrain replica placement. |

## Read the evidence

Commands sit beside captured output or the checks needed to interpret it.
A CR's declared configuration does not prove runtime parallelism or successful inference.
Recorded model-serving walkthroughs pair CR snapshots with completed requests and runtime observations.
*Multi-Host* provides a configuration example and checks without captured runtime results.
Confirm each claim against the walkthrough's stated evidence and the live CR status and `Pod` output.

---

**See also** — [Architecture](/gpustack-operator/v0.11.0/docs/getting-started/architecture/index.md) ·
[Model Deployment](/gpustack-operator/v0.11.0/docs/modules/model-deployment/index.md)

**Next** → [Heterogeneous Devices Walkthrough](/gpustack-operator/v0.11.0/docs/walkthroughs/devices/scheduling/index.md)
