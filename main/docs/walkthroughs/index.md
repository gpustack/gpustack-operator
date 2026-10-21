# Walkthroughs

Follow a complete example, then compare the resource output with the observed result.
Each module has its own directory. Configuration contracts remain in the capability guides.

## Contents

- [Choose a walkthrough](#choose-a-walkthrough)
- [Read the evidence](#read-the-evidence)

## Choose a walkthrough

Serving configurations use *tensor parallelism (TP)*, *data parallelism (DP)* and *expert parallelism (EP)*.
The [Model Deployment guides](/gpustack-operator/main/docs/modules/model-deployment/index.md) explain their configuration contracts.

| Module | Walkthrough | Task |
|---|---|---|
| Heterogeneous Devices | [Heterogeneous Devices](/gpustack-operator/main/docs/walkthroughs/devices/scheduling/index.md) | Follow detection, queues, admission, and accelerator allocation. |
| RDMA Networking | [RDMA Networking](/gpustack-operator/main/docs/walkthroughs/rdma/network-endpoints/index.md) | Request and verify shared and exclusive RDMA endpoints with NUMA alignment. |
| Topology | [Hierarchical Placement](/gpustack-operator/main/docs/walkthroughs/topology/hierarchical-placement/index.md) | Publish zone and rack hierarchies and constrain replicas to a topology domain. |
| KV Cache | [Shared Cache](/gpustack-operator/main/docs/walkthroughs/kv-cache/shared-cache/index.md) | Create a backend, pool, namespace grant, and serving workload. |
| Model Delivery | [Model Prefetch](/gpustack-operator/main/docs/walkthroughs/model-delivery/prefetch/index.md) | Cache and verify model weights before inference. |
| Model Deployment | [Preparation and choices](/gpustack-operator/main/docs/walkthroughs/model-deployment/index.md) | Choose a serving configuration and follow its hardware, model and `InstanceType` requirements. |
| Model Deployment | [Prefill and Decode](/gpustack-operator/main/docs/walkthroughs/model-deployment/prefill-decode/index.md) | Disaggregate *prefill* and *decode* roles fronted by a router. |
| Model Deployment | [Encode, Prefill and Decode](/gpustack-operator/main/docs/walkthroughs/model-deployment/encode-prefill-decode/index.md) | Separate multimodal *encode*, *prefill* and *decode* roles. |
| Model Deployment | [Elastic EP](/gpustack-operator/main/docs/walkthroughs/model-deployment/elastic-ep/index.md) | Grow *DP* width 2 → 4 with *TP* = 2. |
| Model Deployment | [External DP](/gpustack-operator/main/docs/walkthroughs/model-deployment/external-dp/index.md) | Deploy four fixed H100 *DP* ranks and verify captured per-rank inference and Router coverage. |
| Model Deployment | [Multi-Host](/gpustack-operator/main/docs/walkthroughs/model-deployment/multi-host/index.md) | Deploy distributed model instances across multiple GPU hosts. |

## Read the evidence

Commands sit beside captured output or the checks needed to interpret it.
A CR's declared configuration does not prove runtime parallelism or successful inference.
Recorded model-serving walkthroughs pair CR snapshots with completed requests and runtime observations.
*Multi-Host* provides a configuration example and checks without captured runtime results.
Confirm each claim against the walkthrough's stated evidence and the live CR status and `Pod` output.

---

**See also** — [Architecture](/gpustack-operator/main/docs/getting-started/architecture/index.md) ·
[Model Deployment](/gpustack-operator/main/docs/modules/model-deployment/index.md)

**Next** → [Heterogeneous Devices Walkthrough](/gpustack-operator/main/docs/walkthroughs/devices/scheduling/index.md)
