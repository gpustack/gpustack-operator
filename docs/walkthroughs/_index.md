# Walkthroughs

Run a complete end-to-end procedure and compare live cluster state against captured or illustrative
outputs. Capability guides own full field and admission contracts; walkthroughs connect resources
across modules in working order.

## Contents

- [Walkthrough selection](#walkthrough-selection)
- [Cross-module configurations](#cross-module-configurations)
- [Evidence and output](#evidence-and-output)

## Walkthrough selection

Serving examples below use *tensor parallelism (TP)*, *data parallelism (DP)*, and *expert
parallelism (EP)*. See [Model Deployment](../modules/model-deployment/_index.md) for serving
contracts and benchmarks.

| Module | Walkthrough | Task |
|---|---|---|
| Heterogeneous Devices | [Heterogeneous Devices](devices/scheduling.md) | Inspect pool derivation, node opt-out, logical slicing, custom `InstanceType`s, CPU awareness, and node pinning. |
| RDMA Networking | [RDMA Networking](rdma/network-endpoints.md) | Request shared and exclusive *RDMA* endpoints, align *NUMA* with kubelet, and attach fabric to KV cache or *PD* roles. |
| Topology | [Hierarchical Placement](topology/hierarchical-placement.md) | Publish zone and rack hierarchies and constrain multi-`Pod` replicas to one failure domain. |
| KV Cache | [Shared Cache](kv-cache/shared-cache.md) | Stand up a backend, pool, namespace binding, HA leader, local disk tier, and attached serving workload. |
| Model Delivery | [Model Prefetch](model-delivery/prefetch.md) | Prefetch verified Hugging Face weights onto a GPU node and pin them against eviction. |
| Model Deployment | [Preparation and choices](model-deployment/_index.md) | Compare serving topologies and prepare cluster prerequisites and GPU `AcceleratorType`s. |
| Model Deployment | [Prefill and Decode](model-deployment/prefill-decode.md) | Deploy disaggregated *Prefill* and *Decode* roles behind `llm-d-router` on H100 GPUs. |
| Model Deployment | [Encode, Prefill and Decode](model-deployment/encode-prefill-decode.md) | Deploy multimodal *Encode*, *Prefill*, and *Decode* roles with NIXL encoder-cache transfer. |
| Model Deployment | [Elastic EP](model-deployment/elastic-ep.md) | Expand a live Ray *EP* collective from *DP* width 2 → 4 (*EP* world size 4 → 8) without replacing surviving `Pod`s. |
| Model Deployment | [External DP](model-deployment/external-dp.md) | Deploy four fixed *DP* ranks (*TP* = 2, *EP* world size 8) behind `vllm-router` and verify all-rank coverage. |
| Model Deployment | [Scaling](model-deployment/scaling.md) | Observe a `ModelDeploymentScaler` in `Shadow` mode, select an algorithm or catalog prior, and activate scaling. |
| Model Deployment | [Online Runtime Control](model-deployment/runtime-control.md) | Tune vLLM and SGLang parameters live, pause roles safely with an active scaler, and verify restart replay on H200. |
| Model Deployment | [Multi-Host](model-deployment/multi-host.md) | Span one 16-GPU serving instance across two physical GPU hosts with gang admission. |

## Cross-module configurations

Multi-module sequences converge here so capability guides stay focused on single-module contracts.
Verify each page's prerequisites and limitations before adapting a manifest to other hardware or
transports.

| Configuration | Walkthrough | Linkage |
|---|---|---|
| Model delivery and serving | [Model Prefetch](model-delivery/prefetch.md) | `ModelStoreBinding` grants cache quota; `ModelDeployment` mounts the resolved `ModelArtifact`. |
| Shared cache and serving | [Shared Cache](kv-cache/shared-cache.md) | `KVCacheBackend` backs a `KVCachePool`; `KVCachePoolBinding` injects client config into workloads. |
| RDMA and shared cache | [RDMA Networking](rdma/network-endpoints.md#step-4-kv-cache-over-rdma) | `KVCacheBackend` switches transport to `RDMA` and claims fabric interfaces per member. |
| Delivery, serving, and scaling | [Scaling](model-deployment/scaling.md) | `ModelDeployment` serves a prefetched `ModelArtifact` while `ModelDeploymentScaler` drives replica counts. |
| Topology and serving | [Hierarchical Placement](topology/hierarchical-placement.md) | `TopologySource` publishes zone or rack domains enforced by *`roles[].topology.requiredLevel`*. |

## Evidence and output

Each step pairs commands with `Captured output` from a recorded cluster run or `Example output
(illustrative)` showing the expected signal. A resource reaching `Ready` does not by itself prove
active parallelism, all-rank routing, or successful token generation; run the inference and endpoint
checks included in each walkthrough. *Multi-Host* is an unrecorded configuration guide with
verification checks.

---

**See also** — [Architecture](../getting-started/architecture.md) (scheduling chain and vocabulary) ·
[Model Deployment](../modules/model-deployment/_index.md) (serving capability guides)

**Next** → [Heterogeneous Devices Walkthrough](devices/scheduling.md) — follow a recorded accelerator scheduling run.
