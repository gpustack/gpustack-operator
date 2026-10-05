# Model Deployment

A `ModelDeployment` describes the model, engine and roles of a serving workload. Each role can have
its own replicas and resources.

## Contents

- [Deployment configuration](#deployment-configuration)
- [Service operations](#service-operations)

## Deployment configuration

[Model Deployment Configuration](deployment.md) starts with a minimal manifest and explains
the role contract. [Prefill and Decode](prefill-decode.md) covers split roles.
[Engine Versions](engine-versions.md) records tested version floors.

## Service operations

[Status](status.md), [Metrics](metrics.md) and
[Routing](routing.md) explain what the running service reports and how requests
are directed.

`Elastic EP` is its own mode rather than another role shape: it keeps one serving instance and
changes the size of its collective while it runs.

---

**See also** — [KV Cache](../kv-cache/_index.md) (shared prefix reuse) ·
[Model Delivery](../model-delivery/_index.md) (weights)

**Next** → [Model Deployment Configuration](deployment.md) — create a serving workload.
