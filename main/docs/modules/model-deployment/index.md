# Model Deployment

A `ModelDeployment` describes the model, engine and roles of a serving workload. Each role can have
its own replicas and resources.

## Contents

- [Deployment configuration](#deployment-configuration)
- [Service operations](#service-operations)

## Deployment configuration

[Model Deployment Configuration](/gpustack-operator/main/docs/modules/model-deployment/deployment/index.md) starts with a minimal manifest and explains
the role contract. [Prefill and Decode](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode/index.md) covers split roles.
[Engine Versions](/gpustack-operator/main/docs/modules/model-deployment/engine-versions/index.md) records tested version floors.

## Service operations

[Status](/gpustack-operator/main/docs/modules/model-deployment/status/index.md), [Metrics](/gpustack-operator/main/docs/modules/model-deployment/metrics/index.md) and
[Routing](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md) explain what the running service reports and how requests
are directed.

`Elastic EP` is its own mode rather than another role shape: it keeps one serving instance and
changes the size of its collective while it runs.

---

**See also** — [KV Cache](/gpustack-operator/main/docs/modules/kv-cache/index.md) (shared prefix reuse) ·
[Model Delivery](/gpustack-operator/main/docs/modules/model-delivery/index.md) (weights)

**Next** → [Model Deployment Configuration](/gpustack-operator/main/docs/modules/model-deployment/deployment/index.md) — create a serving workload.
