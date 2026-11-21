# Model Deployment Walkthroughs

Select a serving topology, verify cluster capacity and cached weights, and run the deployment
manifest. Each walkthrough pairs reusable manifests with verification checks and captured outputs.

## Contents

- [Serving walkthroughs by task](#serving-walkthroughs-by-task)
- [Cluster preparation](#cluster-preparation)
- [GPU pool selection](#gpu-pool-selection)

## Serving walkthroughs by task

Serving topologies below combine *tensor parallelism (TP)*, *data parallelism (DP)*, and *expert
parallelism (EP)*. *DP* width counts data-parallel ranks, *EP* world size counts GPU workers in the
expert collective, and *internal load balancing (Internal LB)* vs *external load balancing (External LB)*
decides whether only the master or every *DP* rank serves HTTP requests.

| Walkthrough | Serving configuration | Scope |
|---|---|---|
| [Prefill and Decode](/gpustack-operator/main/docs/walkthroughs/model-deployment/prefill-decode/index.md) | Disaggregated *Prefill* and *Decode* roles behind `llm-d-router` | 1 *Prefill* + 2 *Decode* replicas, *TP* = 2, TCP Mooncake transfer ([Prefill and Decode Benchmark](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode-benchmark/index.md)). |
| [Encode, Prefill and Decode](/gpustack-operator/main/docs/walkthroughs/model-deployment/encode-prefill-decode/index.md) | Disaggregated multimodal *Encode*, *Prefill*, and *Decode* roles | 1 *Encode* + 1 *Prefill* + 1 *Decode* replica, NIXL encoder-cache transfer ([Encode, Prefill and Decode Benchmark](/gpustack-operator/main/docs/modules/model-deployment/encode-prefill-decode-benchmark/index.md)). |
| [Elastic EP](/gpustack-operator/main/docs/walkthroughs/model-deployment/elastic-ep/index.md) | Single *Internal LB* Ray collective resized in place | *TP* = 2, *DP* width 2 → 4, *EP* world size 4 → 8; 439/439 requests succeeded ([Elastic EP Benchmark](/gpustack-operator/main/docs/modules/model-deployment/elastic-ep-benchmark/index.md)). |
| [External DP](/gpustack-operator/main/docs/walkthroughs/model-deployment/external-dp/index.md) | Four fixed *External LB* *DP* ranks behind `vllm-router` | *TP* = 2, *DP* = 4, *EP* world size 8; per-rank endpoints and router distribution ([External DP Benchmark](/gpustack-operator/main/docs/modules/model-deployment/external-dp-benchmark/index.md)). |
| [Scaling](/gpustack-operator/main/docs/walkthroughs/model-deployment/scaling/index.md) | Replica autoscaling via `ModelDeploymentScaler` | `Shadow` and `Active` modes, `Proportional` / `TokenThroughput` / `Predictive` algorithms, catalog priors, *EPD* ratios, and retirement ([Model Deployment Scaling Benchmark](/gpustack-operator/main/docs/modules/model-deployment/scaling-benchmark/index.md)). |
| [Online Runtime Control](/gpustack-operator/main/docs/walkthroughs/model-deployment/runtime-control/index.md) | Live parameter tuning, role pause, and restart replay | vLLM and SGLang live mutations, Envoy allowlist, `RolePaused` scaler guard, same-UID restart latch, and Elastic EP resize. |
| [Multi-Host](/gpustack-operator/main/docs/walkthroughs/model-deployment/multi-host/index.md) | Single collective instance spanning two GPU hosts | Configuration example: `size: 2` (16 GPUs across two nodes), leader `m0` ingress, and gang admission. |

Increasing *`roles[].replicas`* adds independent replica groups rather than widening one collective;
see [Rollout behavior](/gpustack-operator/main/docs/modules/model-deployment/configuration/index.md#rollout-behavior).

## Cluster preparation

Install GPUStack Operator and enable node model delivery using
[Installation Modes](/gpustack-operator/main/docs/operate/installation-modes/index.md) and
[Model Store Operations](/gpustack-operator/main/docs/modules/model-delivery/operations/index.md). Ensure `kubectl`, `jq`, and
`curl` are installed locally.

Single-node serving walkthroughs use one CPU node and one 8-GPU NVIDIA H100 80 GB HBM3 SXM5 node
(`Multi-Host` uses two 8-GPU nodes; `Online Runtime Control` uses three 1-GPU H200 nodes). Inspect
node and cache readiness before deploying:

```bash
kubectl get nodes
kubectl get devices -o json
kubectl get acceleratortypes
kubectl get nodemodelstores -o json
```

Confirm sufficient free GPUs (*External DP* needs 8 GPUs; *Elastic EP* starts at 4 and expands to 8)
and warm the walkthrough's `ModelArtifact` with
[Model Prefetch](/gpustack-operator/main/docs/walkthroughs/model-delivery/prefetch/index.md) before starting serving pods.

## GPU pool selection

H100 manifests specify the derived pool `gpustack--nvidia-h100-80gb-hbm3-linux-amd64` (benchmark
captures used a custom sibling `bench-gpu` over the same H100 group). On other hardware, substitute an
`Active` whole-GPU `AcceleratorType` from `kubectl get acctype` and verify VRAM capacity and engine
compatibility first.

---

**See also** — [Model Deployment](/gpustack-operator/main/docs/modules/model-deployment/index.md) (serving configuration
guides) · [Model Prefetch Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-delivery/prefetch/index.md) (warming weights onto GPU nodes)

**Next** → [Prefill and Decode Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/prefill-decode/index.md) — deploy disaggregated prefill and decode roles behind a router.
