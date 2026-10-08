# Walkthroughs

Follow a complete example, then compare the resource output with the observed result.
Each module has its own directory. Configuration contracts remain in the capability guides.

## Contents

- [Choose a walkthrough](#choose-a-walkthrough)
- [Read the evidence](#read-the-evidence)

## Choose a walkthrough

| Module | Walkthrough | Task |
|---|---|---|
| Devices | [Device Scheduling](devices/scheduling.md) | Follow detection, queues, admission, and accelerator allocation. |
| KV Cache | [Shared Cache](kv-cache/shared-cache.md) | Create a backend, pool, namespace grant, and serving workload. |
| Model Delivery | [Model Prefetch](model-delivery/prefetch.md) | Cache and verify model weights before inference. |
| Model Deployment | [Preparation and choices](model-deployment/_index.md) | Choose the serving shape and prepare GPU resources. |
| Model Deployment | [Elastic EP](model-deployment/elastic-ep.md) | Grow DP width from 2 to 4 with TP fixed at 2. |
| Model Deployment | [External DP](model-deployment/external-dp.md) | Serve fixed DP ranks and verify Router distribution. |

## Read the evidence

Commands sit beside captured output or the checks needed to interpret it.
A CR's declared configuration does not prove runtime parallelism or successful inference.
The model-serving walkthroughs pair CR snapshots with completed requests and native runtime measurements.
Their validation records state software identities and limits.

---

**See also** — [Architecture](../getting-started/architecture.md) ·
[Model Deployment](../modules/model-deployment/_index.md)

**Next** → [Device Scheduling Walkthrough](devices/scheduling.md)
