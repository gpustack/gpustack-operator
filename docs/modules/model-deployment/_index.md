# Model Deployment

A `ModelDeployment` describes the model, engine and roles of a serving workload. Each role can have
its own replicas and resources.

## Contents

- [Deploy a model](#deploy-a-model)
- [Operate the service](#operate-the-service)

## Deploy a model

[Model Deployment](deployment.md) starts with a minimal manifest and explains
the role contract. [Prefill and Decode](prefill-decode.md) covers split roles.
[Engine Versions](engine-versions.md) records tested version floors.

## Operate the service

[Status](status.md), [Metrics](metrics.md) and
[Routing](routing.md) explain what the running service reports and how requests
are directed.

---

**See also** — [KV Cache](../kv-cache/_index.md) (shared prefix reuse) ·
[Model Delivery](../model-delivery/_index.md) (weights)

**Next** → [Model Deployment](deployment.md) — create a serving workload.
