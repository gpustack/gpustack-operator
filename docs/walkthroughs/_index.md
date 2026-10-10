# Walkthroughs

Follow a complete example, then compare the resource output with the observed result.
Each module has its own directory. Configuration contracts remain in the capability guides.

## Contents

- [Choose a walkthrough](#choose-a-walkthrough)
- [Read the evidence](#read-the-evidence)

## Choose a walkthrough

| Module | Walkthrough | Task |
|---|---|---|
| Devices Manager | [Scheduling](devices/scheduling.md) | Follow detection, queues, admission, and accelerator allocation. |
| RDMA Manager | [RDMA Network Endpoints](rdma/network-endpoints.md) | Request and verify shared and exclusive RDMA endpoints with NUMA alignment. |
| Topology Aware | [Hierarchical Placement](topology/hierarchical-placement.md) | Publish zone and rack hierarchies and constrain replicas to a topology domain. |
| KV Cache | [Shared Cache](kv-cache/shared-cache.md) | Create a backend, pool, namespace grant, and serving workload. |
| Model Delivery | [Model Prefetch](model-delivery/prefetch.md) | Cache and verify model weights before inference. |
| Model Deployment | [Preparation and choices](model-deployment/_index.md) | Choose the serving shape and prepare GPU resources. |
| Model Deployment | [Prefill and Decode](model-deployment/prefill-decode.md) | Disaggregate prefill and decode roles fronted by a router. |
| Model Deployment | [Elastic EP](model-deployment/elastic-ep.md) | Grow DP width from 2 to 4 with TP fixed at 2. |
| Model Deployment | [External DP](model-deployment/external-dp.md) | Serve fixed DP ranks and verify Router distribution. |
| Model Deployment | [Multi-Host](model-deployment/multi-host.md) | Deploy distributed model instances across multiple GPU hosts. |

## Read the evidence

Commands sit beside captured output or the checks needed to interpret it.
A CR's declared configuration does not prove runtime parallelism or successful inference.
The model-serving walkthroughs pair CR snapshots with completed requests and native runtime measurements.
Their validation records state software identities and limits.

---

**See also** — [Architecture](../getting-started/architecture.md) ·
[Model Deployment](../modules/model-deployment/_index.md)

**Next** → [Scheduling Walkthrough](devices/scheduling.md)
