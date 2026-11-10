# Model Deployment

A `ModelDeployment` describes the model, engine and roles of a serving workload. Each role can have
its own replicas and resources.

## Contents

- [Overview](#overview)
- [Deployment configuration](#deployment-configuration)
- [Service operations](#service-operations)
- [Troubleshooting](#troubleshooting)

## Overview

![Model deployment serving flow](../../assets/diagrams/model-deployment-flow.svg)

<!--
flowchart TD
    ARTIFACT["Optional ModelArtifact"] -> CTRL["ModelDeployment role controller"]
    CACHE["Optional KVCachePoolBinding"] -> CTRL
    POOL["InstanceType and topology"] -> CTRL
    SCALER["Optional ModelDeploymentScaler"] -> CTRL
    CTRL -> ROUTER["Managed traffic router"]
    CTRL -> PODS["Aggregated, encode, prefill and decode replicas"]
    ROUTER -> PODS
-->

## Deployment configuration

[Model Deployment Configuration](configuration.md) starts with a minimal manifest and explains
the role contract. [Parameters](parameterize.md) catalogs generated parameters
and overrides.

[Prefill and Decode](prefill-decode.md) covers split roles, and
[Encode, Prefill and Decode](encode-prefill-decode.md) covers multimodal disaggregation.
[Engine Compatibility](engine-versions.md) lists minimum releases, client and store requirements,
transport support and known failures.

[Encode, Prefill and Decode Benchmark](encode-prefill-decode-benchmark.md) measures that
disaggregation against an aggregated instance.
[Prefill and Decode Benchmark](prefill-decode-benchmark.md) measures a text-only
*prefill/decode* pair against an aggregated group on equal GPUs.

Follow [Prefill and Decode Walkthrough](../../walkthroughs/model-deployment/prefill-decode.md) for
disaggregated serving, [Encode, Prefill and Decode Walkthrough](../../walkthroughs/model-deployment/encode-prefill-decode.md) for
multimodal disaggregation, and [Multi-Host Serving Walkthrough](../../walkthroughs/model-deployment/multi-host.md)
for distributed multi-node serving.

## Service operations

[Status](status.md), [Metrics](metrics.md) and
[Routing](routing.md) explain what the running service reports and how requests
are directed.

[Scaling](scaling.md) plans the replica count of a deployment from the signals it publishes, and
[Shutdown](shutdown.md) explains the retirement protocol a scale-down runs.
The [Scaling Walkthrough](../../walkthroughs/model-deployment/scaling.md) configures observation and
active scaling. The [Scaling Benchmark](scaling-benchmark.md) records H100 and H200 measurements
and the limits observed during scale-out and retirement.

[Elastic EP](elastic-ep.md) keeps one serving instance and changes its collective while it runs.
[Elastic EP Walkthrough](../../walkthroughs/model-deployment/elastic-ep.md) follows prefetch, startup with
*tensor parallelism (TP)* = 2 and *data parallelism (DP)* = 2, and *DP* width 2 → 4 expansion.
It includes captured resource output and completed inference requests.

[Elastic EP Benchmark](elastic-ep-benchmark.md) measures the same width change under a live request
stream.

[External DP](external-dp.md) runs one fixed *DP* group with an HTTP endpoint on every rank and
Router-balanced traffic across all of them.
The [External DP Walkthrough](../../walkthroughs/model-deployment/external-dp.md) runs a fixed group
with one HTTP endpoint per rank and checks managed Router coverage.

[External DP Benchmark](external-dp-benchmark.md) compares that group with a
monolithic wide tensor-parallel group on the same GPUs.

## Troubleshooting

If a deployment does not become ready, inspect its phase, conditions and role status:

```bash
kubectl -n <namespace> get modeldeployment <name> -o yaml
```

Compare the result with [Model Deployment Status](status.md). For scaling problems, see the
[Scaling Walkthrough](../../walkthroughs/model-deployment/scaling.md#troubleshooting).

---

**See also** — [KV Cache](../kv-cache/_index.md) (shared prefix reuse) ·
[Model Delivery](../model-delivery/_index.md) (weights)

**Next** → [Model Deployment Configuration](configuration.md) — create a serving workload.
