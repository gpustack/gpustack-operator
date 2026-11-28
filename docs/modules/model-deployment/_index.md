# Model Deployment

A `ModelDeployment` declares the model, engine and serving roles of an inference workload. Each
role sets its own replicas and accelerator resources.

## Contents

- [Overview](#overview)
- [Configuration](#configuration)
- [Operations](#operations)
- [Troubleshooting](#troubleshooting)

## Overview

![Model deployment serving flow](../../assets/diagrams/model-deployment-flow.svg)

<!--
flowchart TD
    ARTIFACT["Optional ModelArtifact"] -> CTRL["ModelDeployment role controller"]
    CACHE["Optional KVCachePoolBinding"] -> CTRL
    POOL["AcceleratorType and topology"] -> CTRL
    SCALER["Optional ModelDeploymentScaler"] -> CTRL
    CTRL -> ROUTER["Managed traffic router"]
    CTRL -> PODS["Aggregated, encode, prefill and decode replicas"]
    ROUTER -> PODS
-->

## Configuration

- [Model Deployment Configuration](configuration.md) defines the base manifest, role fields,
  override tiers and operator-owned keys.
- [Engine Compatibility](engine-versions.md) lists minimum engine releases, Mooncake versions,
  supported transports and known limits.
- [Prefill and Decode](prefill-decode.md) pairs split *prefill/decode (PD)* roles;
  [Prefill and Decode Benchmark](prefill-decode-benchmark.md) compares *PD* against an equal-GPU
  aggregated group.
- [Encode, Prefill and Decode](encode-prefill-decode.md) adds multimodal disaggregation;
  [Encode, Prefill and Decode Benchmark](encode-prefill-decode-benchmark.md) compares
  *encode/prefill/decode (EPD)* against *PD* and aggregated serving.
- Follow [Prefill and Decode Walkthrough](../../walkthroughs/model-deployment/prefill-decode.md),
  [Encode, Prefill and Decode Walkthrough](../../walkthroughs/model-deployment/encode-prefill-decode.md)
  and [Multi-Host Serving Walkthrough](../../walkthroughs/model-deployment/multi-host.md) for
  step-by-step runs.

## Operations

- [Online Runtime Control](runtime-control.md) covers live parameter tuning, role pause and
  retirement windows.
- [Model Deployment Routing](routing.md) covers endpoint eligibility, router defaults and
  per-worker metrics.
- [Model Deployment Scaling](scaling.md) plans replica counts from engine metrics and coordinates
  graceful retirement. Pair it with
  [Scaling Walkthrough](../../walkthroughs/model-deployment/scaling.md) and
  [Scaling Benchmark](scaling-benchmark.md).
- [Model Deployment Forger](forger.md) automates short-window kernel probing, sandbox synthesis,
  `ModelKernelArtifact` delivery, and readiness-gated canary rollout.
- [Elastic EP](elastic-ep.md) grows one running collective in place. Pair it with
  [Elastic EP Walkthrough](../../walkthroughs/model-deployment/elastic-ep.md) (*tensor parallelism
  (TP)* = 2, *data parallelism (DP)* width 2 → 4) and
  [Elastic EP Benchmark](elastic-ep-benchmark.md).
- [External DP](external-dp.md) exposes one HTTP endpoint per rank behind a managed router. Pair it
  with [External DP Walkthrough](../../walkthroughs/model-deployment/external-dp.md) and
  [External DP Benchmark](external-dp-benchmark.md).

## Troubleshooting

If a deployment does not become ready, inspect its phase, conditions and role status:

```bash
kubectl -n <namespace> get modeldeployment <name> -o yaml
```

Match *`status.phase`*, conditions and role status against
[Readiness and status](configuration.md#readiness-and-status) and
[Model Deployment Routing](routing.md#troubleshooting). For scaling holds, inspect the
`ModelDeploymentScaler` using
[Scaling Walkthrough](../../walkthroughs/model-deployment/scaling.md#troubleshooting).

---

**See also** — [KV Cache](../kv-cache/_index.md) (shared prefix reuse) ·
[Model Delivery](../model-delivery/_index.md) (weights)

**Next** → [Model Deployment Configuration](configuration.md) — create a serving workload.
