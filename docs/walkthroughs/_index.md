# Walkthroughs

Follow a complete example, then compare the resource output with the observed result.
Each module has its own directory. Configuration contracts remain in the capability guides.

## Contents

- [Choose a walkthrough](#choose-a-walkthrough)
- [Read the evidence](#read-the-evidence)

## Choose a walkthrough

Serving configurations use *tensor parallelism (TP)*, *data parallelism (DP)* and *expert parallelism (EP)*.
The [Model Deployment guides](../modules/model-deployment/_index.md) explain their configuration contracts.

| Module | Walkthrough | Task |
|---|---|---|
| Heterogeneous Devices | [Heterogeneous Devices](devices/scheduling.md) | Follow detection, queues, admission, and accelerator allocation. |
| RDMA Networking | [RDMA Networking](rdma/network-endpoints.md) | Request and verify shared and exclusive RDMA endpoints with NUMA alignment. |
| Topology | [Hierarchical Placement](topology/hierarchical-placement.md) | Publish zone and rack hierarchies and constrain replicas to a topology domain. |
| KV Cache | [Shared Cache](kv-cache/shared-cache.md) | Create a backend, pool, namespace grant, and serving workload. |
| Model Delivery | [Model Prefetch](model-delivery/prefetch.md) | Cache and verify model weights before inference. |
| Model Deployment | [Preparation and choices](model-deployment/_index.md) | Choose a serving configuration and follow its hardware, model and `InstanceType` requirements. |
| Model Deployment | [Prefill and Decode](model-deployment/prefill-decode.md) | Disaggregate *prefill* and *decode* roles fronted by a router. |
| Model Deployment | [Encode, Prefill and Decode](model-deployment/encode-prefill-decode.md) | Separate multimodal *encode*, *prefill* and *decode* roles. |
| Model Deployment | [Elastic EP](model-deployment/elastic-ep.md) | Grow *DP* width 2 → 4 with *TP* = 2. |
| Model Deployment | [External DP](model-deployment/external-dp.md) | Deploy four fixed H100 *DP* ranks and verify captured per-rank inference and Router coverage. |
| Model Deployment | [Multi-Host](model-deployment/multi-host.md) | Deploy distributed model instances across multiple GPU hosts. |

## Read the evidence

Commands sit beside captured output or the checks needed to interpret it.
A CR's declared configuration does not prove runtime parallelism or successful inference.
Recorded model-serving walkthroughs pair CR snapshots with completed requests and runtime observations.
*Multi-Host* provides a configuration example and checks without captured runtime results.
Confirm each claim against the walkthrough's stated evidence and the live CR status and `Pod` output.

---

**See also** — [Architecture](../getting-started/architecture.md) ·
[Model Deployment](../modules/model-deployment/_index.md)

**Next** → [Heterogeneous Devices Walkthrough](devices/scheduling.md)
