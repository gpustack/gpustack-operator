# Walkthroughs

Follow a complete example, then compare the resource output with the observed result.
Each module has its own directory. Configuration contracts remain in the capability guides.

## Contents

- [Choose a walkthrough](#choose-a-walkthrough)
- [Read the evidence](#read-the-evidence)

## Choose a walkthrough

| Module | Walkthrough | Task |
|---|---|---|
| Devices | [Device Scheduling](/gpustack-operator/main/docs/walkthroughs/devices/scheduling/index.md) | Follow detection, queues, admission, and accelerator allocation. |
| KV Cache | [Shared Cache](/gpustack-operator/main/docs/walkthroughs/kv-cache/shared-cache/index.md) | Create a backend, pool, namespace grant, and serving workload. |
| Model Delivery | [Model Prefetch](/gpustack-operator/main/docs/walkthroughs/model-delivery/prefetch/index.md) | Cache and verify model weights before inference. |
| Model Deployment | [Preparation and choices](/gpustack-operator/main/docs/walkthroughs/model-deployment/index.md) | Choose the serving shape and prepare GPU resources. |
| Model Deployment | [Elastic EP](/gpustack-operator/main/docs/walkthroughs/model-deployment/elastic-ep/index.md) | Grow DP width from 2 to 4 with TP fixed at 2. |
| Model Deployment | [External DP](/gpustack-operator/main/docs/walkthroughs/model-deployment/external-dp/index.md) | Serve fixed DP ranks and verify Router distribution. |

## Read the evidence

Commands sit beside captured output or the checks needed to interpret it.
A CR's declared configuration does not prove runtime parallelism or successful inference.
The model-serving walkthroughs pair CR snapshots with completed requests and native runtime measurements.
Their validation records state software identities and limits.

---

**See also** — [Architecture](/gpustack-operator/main/docs/getting-started/architecture/index.md) ·
[Model Deployment](/gpustack-operator/main/docs/modules/model-deployment/index.md)

**Next** → [Device Scheduling Walkthrough](/gpustack-operator/main/docs/walkthroughs/devices/scheduling/index.md)
