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

[Elastic EP](elastic-ep.md) keeps one serving instance and changes its collective while it runs.
[Model Scaling Walkthrough](scaling-walkthrough.md) follows prefetch, TP2/DP2 startup, and DP2→4 expansion
with captured resource output and completed inference requests.

---

**See also** — [KV Cache](../kv-cache/_index.md) (shared prefix reuse) ·
[Model Delivery](../model-delivery/_index.md) (weights)

**Next** → [Model Deployment Configuration](deployment.md) — create a serving workload.
