# Model Deployment

A `ModelDeployment` describes the model, engine and roles of a serving workload. Each role can have
its own replicas and resources.

## Contents

- [Deployment configuration](#deployment-configuration)
- [Service operations](#service-operations)

## Deployment configuration

[Model Deployment Configuration](/gpustack-operator/v0.9.2/docs/modules/model-deployment/deployment/index.md) starts with a minimal manifest and explains
the role contract. [Prefill and Decode](/gpustack-operator/v0.9.2/docs/modules/model-deployment/prefill-decode/index.md) covers split roles, and
[Encode, Prefill and Decode](/gpustack-operator/v0.9.2/docs/modules/model-deployment/encode-prefill-decode/index.md) covers multimodal disaggregation.
[Engine Compatibility](/gpustack-operator/v0.9.2/docs/modules/model-deployment/engine-versions/index.md) lists minimum releases, client and store requirements,
transport support and known failures.

[Encode, Prefill and Decode Benchmark](/gpustack-operator/v0.9.2/docs/modules/model-deployment/encode-prefill-decode-benchmark/index.md) measures that
disaggregation against an aggregated instance.
[Prefill and Decode Benchmark](/gpustack-operator/v0.9.2/docs/modules/model-deployment/prefill-decode-benchmark/index.md) measures a text-only
*prefill/decode* pair against an aggregated group on equal GPUs.

Follow [Prefill and Decode Walkthrough](/gpustack-operator/v0.9.2/docs/walkthroughs/model-deployment/prefill-decode/index.md) for
disaggregated serving, [Encode, Prefill and Decode Walkthrough](/gpustack-operator/v0.9.2/docs/walkthroughs/model-deployment/encode-prefill-decode/index.md) for
multimodal disaggregation, and [Multi-Host Serving Walkthrough](/gpustack-operator/v0.9.2/docs/walkthroughs/model-deployment/multi-host/index.md)
for distributed multi-node serving.

## Service operations

[Status](/gpustack-operator/v0.9.2/docs/modules/model-deployment/status/index.md), [Metrics](/gpustack-operator/v0.9.2/docs/modules/model-deployment/metrics/index.md) and
[Routing](/gpustack-operator/v0.9.2/docs/modules/model-deployment/routing/index.md) explain what the running service reports and how requests
are directed.

[Elastic EP](/gpustack-operator/v0.9.2/docs/modules/model-deployment/elastic-ep/index.md) keeps one serving instance and changes its collective while it runs.
[Elastic EP Walkthrough](/gpustack-operator/v0.9.2/docs/walkthroughs/model-deployment/elastic-ep/index.md) follows prefetch, startup with
*tensor parallelism (TP)* = 2 and *data parallelism (DP)* = 2, and *DP* width 2 → 4 expansion.
It includes captured resource output and completed inference requests.

[Elastic EP Benchmark](/gpustack-operator/v0.9.2/docs/modules/model-deployment/elastic-ep-benchmark/index.md) measures the same width change under a live request
stream.

[External DP](/gpustack-operator/v0.9.2/docs/modules/model-deployment/external-dp/index.md) runs one fixed *DP* group with an HTTP endpoint on every rank and
Router-balanced traffic across all of them.
The [External DP Walkthrough](/gpustack-operator/v0.9.2/docs/walkthroughs/model-deployment/external-dp/index.md) runs a fixed group
with one HTTP endpoint per rank and checks managed Router coverage.

[External DP Benchmark](/gpustack-operator/v0.9.2/docs/modules/model-deployment/external-dp-benchmark/index.md) compares that group with a
monolithic wide tensor-parallel group on the same GPUs.

---

**See also** — [KV Cache](/gpustack-operator/v0.9.2/docs/modules/kv-cache/index.md) (shared prefix reuse) ·
[Model Delivery](/gpustack-operator/v0.9.2/docs/modules/model-delivery/index.md) (weights)

**Next** → [Model Deployment Configuration](/gpustack-operator/v0.9.2/docs/modules/model-deployment/deployment/index.md) — create a serving workload.
