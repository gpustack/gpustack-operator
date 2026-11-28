# Walkthroughs

Run a complete end-to-end procedure and compare live cluster state against captured or illustrative
outputs. Capability guides own full field and admission contracts; walkthroughs connect resources
across modules in working order.

## Contents

- [Walkthrough selection](#walkthrough-selection)
- [Cross-module configurations](#cross-module-configurations)
- [Captured and illustrative output](#captured-and-illustrative-output)

## Walkthrough selection

Serving examples below use *tensor parallelism (TP)*, *data parallelism (DP)*, and *expert
parallelism (EP)*. See [Model Deployment](/gpustack-operator/v0.12.2/docs/modules/model-deployment/index.md) for serving
contracts and benchmarks.

| Module | Walkthrough | Task |
|---|---|---|
| Heterogeneous Devices | [Heterogeneous Devices](/gpustack-operator/v0.12.2/docs/walkthroughs/devices/scheduling/index.md) | Inspect pool derivation, node opt-out, logical slicing, custom `InstanceType`s, CPU awareness, and node pinning. |
| RDMA Networking | [RDMA Networking](/gpustack-operator/v0.12.2/docs/walkthroughs/rdma/network-endpoints/index.md) | Request shared and exclusive *RDMA* endpoints, align *NUMA* with kubelet, and attach fabric to KV cache or *PD* roles. |
| Topology | [Hierarchical Placement](/gpustack-operator/v0.12.2/docs/walkthroughs/topology/hierarchical-placement/index.md) | Publish zone and rack hierarchies and constrain multi-`Pod` replicas to one failure domain. |
| KV Cache | [Shared Cache](/gpustack-operator/v0.12.2/docs/walkthroughs/kv-cache/shared-cache/index.md) | Stand up a backend, pool, namespace binding, HA leader, local disk tier, and attached serving workload. |
| Model Delivery | [Model Prefetch](/gpustack-operator/v0.12.2/docs/walkthroughs/model-delivery/prefetch/index.md) | Prefetch verified Hugging Face weights onto a GPU node and pin them against eviction. |
| Model Deployment | [Preparation and choices](/gpustack-operator/v0.12.2/docs/walkthroughs/model-deployment/index.md) | Compare serving topologies and prepare cluster prerequisites and GPU `AcceleratorType`s. |
| Model Deployment | [Prefill and Decode](/gpustack-operator/v0.12.2/docs/walkthroughs/model-deployment/prefill-decode/index.md) | Deploy disaggregated *Prefill* and *Decode* roles behind `llm-d-router`. |
| Model Deployment | [Encode, Prefill and Decode](/gpustack-operator/v0.12.2/docs/walkthroughs/model-deployment/encode-prefill-decode/index.md) | Deploy multimodal *Encode*, *Prefill*, and *Decode* roles with NIXL encoder-cache transfer. |
| Model Deployment | [Elastic EP](/gpustack-operator/v0.12.2/docs/walkthroughs/model-deployment/elastic-ep/index.md) | Expand a live Ray *EP* collective from *DP* width 2 → 4 (*EP* world size 4 → 8) without replacing surviving `Pod`s. |
| Model Deployment | [External DP](/gpustack-operator/v0.12.2/docs/walkthroughs/model-deployment/external-dp/index.md) | Deploy four fixed *DP* ranks (*TP* = 2, *EP* world size 8) behind `vllm-router` and verify all-rank coverage. |
| Model Deployment | [Scaling](/gpustack-operator/v0.12.2/docs/walkthroughs/model-deployment/scaling/index.md) | Observe a `ModelDeploymentScaler` in `Shadow` mode, select an algorithm or catalog prior, and activate scaling. |
| Model Deployment | [Online Runtime Control](/gpustack-operator/v0.12.2/docs/walkthroughs/model-deployment/runtime-control/index.md) | Tune vLLM and SGLang parameters live, pause roles safely with an active scaler, and verify restart replay. |
| Model Deployment | [Multi-Host](/gpustack-operator/v0.12.2/docs/walkthroughs/model-deployment/multi-host/index.md) | Span one 16-GPU serving instance across two physical GPU hosts with gang admission. |

## Cross-module configurations

Multi-module sequences converge here so capability guides stay focused on single-module contracts.
Verify each page's prerequisites and limitations before adapting a manifest to other hardware or
transports.

| Configuration | Walkthrough | Linkage |
|---|---|---|
| Model delivery and serving | [Model Prefetch](/gpustack-operator/v0.12.2/docs/walkthroughs/model-delivery/prefetch/index.md) | `ModelStoreBinding` grants cache quota; `ModelDeployment` mounts the resolved `ModelArtifact`. |
| Shared cache and serving | [Shared Cache](/gpustack-operator/v0.12.2/docs/walkthroughs/kv-cache/shared-cache/index.md) | `KVCacheBackend` backs a `KVCachePool`; `KVCachePoolBinding` injects client config into workloads. |
| RDMA and shared cache | [RDMA Networking](/gpustack-operator/v0.12.2/docs/walkthroughs/rdma/network-endpoints/index.md#step-4-configure-rdma-cache) | `KVCacheBackend` switches transport to `RDMA` and claims fabric interfaces per member. |
| Delivery, serving, and scaling | [Scaling](/gpustack-operator/v0.12.2/docs/walkthroughs/model-deployment/scaling/index.md) | `ModelDeployment` serves a prefetched `ModelArtifact` while `ModelDeploymentScaler` drives replica counts. |
| Topology and serving | [Hierarchical Placement](/gpustack-operator/v0.12.2/docs/walkthroughs/topology/hierarchical-placement/index.md) | `TopologySource` publishes zone or rack domains enforced by *`roles[].topology.requiredLevel`*. |

## Captured and illustrative output

Each step pairs commands with `Captured output` from a live cluster run or `Example output
(illustrative)` showing the expected signal. A resource reaching `Ready` does not by itself prove
active parallelism, all-rank routing, or successful token generation; run the inference and endpoint
checks included in each walkthrough. *Multi-Host* provides illustrative configuration examples and
verification checks.

---

**See also** — [Architecture](/gpustack-operator/v0.12.2/docs/getting-started/architecture/index.md) (scheduling chain and vocabulary) ·
[Model Deployment](/gpustack-operator/v0.12.2/docs/modules/model-deployment/index.md) (serving capability guides)

**Next** → [Heterogeneous Devices Walkthrough](/gpustack-operator/v0.12.2/docs/walkthroughs/devices/scheduling/index.md) — follow a recorded accelerator scheduling run.
