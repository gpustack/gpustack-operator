# Model Deployment

A `ModelDeployment` declares the model, engine and serving roles of an inference workload. Each
role sets its own replicas and accelerator resources.

## Contents

- [Overview](#overview)
- [Configuration](#configuration)
- [Operations](#operations)
- [Troubleshooting](#troubleshooting)

## Overview

![Model deployment serving flow](/gpustack-operator/main/docs/assets/diagrams/model-deployment-flow.svg)

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

- [Model Deployment Configuration](/gpustack-operator/main/docs/modules/model-deployment/configuration/index.md) defines the base manifest, role fields,
  override tiers and operator-owned keys.
- [Engine Compatibility](/gpustack-operator/main/docs/modules/model-deployment/engine-versions/index.md) lists minimum engine releases, Mooncake versions,
  supported transports and known limits.
- [Prefill and Decode](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode/index.md) pairs split *prefill/decode (PD)* roles;
  [Prefill and Decode Benchmark](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode-benchmark/index.md) compares *PD* against an equal-GPU
  aggregated group.
- [Encode, Prefill and Decode](/gpustack-operator/main/docs/modules/model-deployment/encode-prefill-decode/index.md) adds multimodal disaggregation;
  [Encode, Prefill and Decode Benchmark](/gpustack-operator/main/docs/modules/model-deployment/encode-prefill-decode-benchmark/index.md) compares
  *encode/prefill/decode (EPD)* against *PD* and aggregated serving.
- Follow [Prefill and Decode Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/prefill-decode/index.md),
  [Encode, Prefill and Decode Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/encode-prefill-decode/index.md)
  and [Multi-Host Serving Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/multi-host/index.md) for
  step-by-step runs.

## Operations

- [Online Runtime Control](/gpustack-operator/main/docs/modules/model-deployment/runtime-control/index.md) covers live parameter tuning, role pause and
  retirement windows.
- [Model Deployment Routing](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md) covers endpoint eligibility, router defaults and
  per-worker metrics.
- [Model Deployment Scaling](/gpustack-operator/main/docs/modules/model-deployment/scaling/index.md) plans replica counts from engine metrics and coordinates
  graceful retirement. Pair it with
  [Scaling Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/scaling/index.md) and
  [Scaling Benchmark](/gpustack-operator/main/docs/modules/model-deployment/scaling-benchmark/index.md).
- [Model Deployment Forger](/gpustack-operator/main/docs/modules/model-deployment/forger/index.md) automates short-window kernel probing, sandbox synthesis,
  `ModelKernelArtifact` delivery, and readiness-gated canary rollout.
- [Elastic EP](/gpustack-operator/main/docs/modules/model-deployment/elastic-ep/index.md) grows one running collective in place. Pair it with
  [Elastic EP Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/elastic-ep/index.md) (*tensor parallelism
  (TP)* = 2, *data parallelism (DP)* width 2 → 4) and
  [Elastic EP Benchmark](/gpustack-operator/main/docs/modules/model-deployment/elastic-ep-benchmark/index.md).
- [External DP](/gpustack-operator/main/docs/modules/model-deployment/external-dp/index.md) exposes one HTTP endpoint per rank behind a managed router. Pair it
  with [External DP Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/external-dp/index.md) and
  [External DP Benchmark](/gpustack-operator/main/docs/modules/model-deployment/external-dp-benchmark/index.md).

## Troubleshooting

If a deployment does not become ready, inspect its phase, conditions and role status:

```bash
kubectl -n <namespace> get modeldeployment <name> -o yaml
```

Match *`status.phase`*, conditions and role status against
[Readiness and status](/gpustack-operator/main/docs/modules/model-deployment/configuration/index.md#readiness-and-status) and
[Model Deployment Routing](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md#troubleshooting). For scaling holds, inspect the
`ModelDeploymentScaler` using
[Scaling Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/scaling/index.md#troubleshooting).

---

**See also** — [KV Cache](/gpustack-operator/main/docs/modules/kv-cache/index.md) (shared prefix reuse) ·
[Model Delivery](/gpustack-operator/main/docs/modules/model-delivery/index.md) (weights)

**Next** → [Model Deployment Configuration](/gpustack-operator/main/docs/modules/model-deployment/configuration/index.md) — create a serving workload.
