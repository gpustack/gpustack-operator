# Model Deployment Walkthroughs

Select a serving topology, verify cluster capacity and cached weights, and run the deployment
manifest. Each walkthrough separates recorded hardware results from reusable configuration examples.

## Contents

- [Example selection](#example-selection)
- [Cluster preparation](#cluster-preparation)
- [GPU AcceleratorType setup](#gpu-acceleratortype-setup)

## Example selection

Serving topologies below combine *tensor parallelism (TP)*, *data parallelism (DP)*, and *expert
parallelism (EP)*. *DP* width counts data-parallel ranks, *EP* world size counts GPU workers in the
expert collective, and *internal load balancing (Internal LB)* vs *external load balancing (External LB)*
decides whether only the master or every *DP* rank serves HTTP requests.

| Walkthrough | Serving configuration | Scope |
|---|---|---|
| [Prefill and Decode](prefill-decode.md) | Disaggregated *Prefill* and *Decode* roles behind `llm-d-router` | Recorded H100 run: 1 *Prefill* + 2 *Decode* replicas, *TP* = 2, TCP Mooncake transfer, paired with [Prefill and Decode Benchmark](../../modules/model-deployment/prefill-decode-benchmark.md). |
| [Encode, Prefill and Decode](encode-prefill-decode.md) | Disaggregated multimodal *Encode*, *Prefill*, and *Decode* roles | Recorded H100 run: 1 *Encode* + 1 *Prefill* + 1 *Decode* replica, NIXL encoder-cache transfer, paired with [Encode, Prefill and Decode Benchmark](../../modules/model-deployment/encode-prefill-decode-benchmark.md). |
| [Elastic EP](elastic-ep.md) | Single *Internal LB* Ray collective resized in place | Recorded H100 expansion: *TP* = 2, *DP* width 2 → 4, *EP* world size 4 → 8; 439/439 requests succeeded ([Elastic EP Benchmark](../../modules/model-deployment/elastic-ep-benchmark.md)). |
| [External DP](external-dp.md) | Four fixed *External LB* *DP* ranks behind `vllm-router` | Recorded H100 run: *TP* = 2, *DP* = 4, *EP* world size 8; verified per-rank endpoints and router coverage ([External DP Benchmark](../../modules/model-deployment/external-dp-benchmark.md)). |
| [Scaling](scaling.md) | Replica autoscaling via `ModelDeploymentScaler` | `Shadow` and `Active` modes, `Proportional` / `TokenThroughput` / `Predictive` algorithms, catalog priors, *EPD* ratios, and retirement ([Model Deployment Scaling Benchmark](../../modules/model-deployment/scaling-benchmark.md)). |
| [Online Runtime Control](runtime-control.md) | Live parameter tuning, role pause, and restart replay | Recorded H200 run: vLLM and SGLang live mutations, Envoy allowlist, `RolePaused` scaler guard, same-UID restart latch, and folded Elastic EP resize. |
| [Multi-Host](multi-host.md) | Single collective instance spanning two GPU hosts | Unrecorded configuration example: `size: 2` (16 GPUs across two nodes), leader `m0` ingress, and gang admission. |

Increasing *`roles[].replicas`* adds independent replica groups rather than widening one collective;
see [Rollout behavior](../../modules/model-deployment/configuration.md#rollout-behavior).

## Cluster preparation

Install GPUStack Operator and enable node model delivery using
[Installation Modes](../../operate/installation-modes.md) and
[Model Store Operations](../../modules/model-delivery/operations.md). Ensure `kubectl`, `jq`, and
`curl` are installed locally.

Recorded H100 walkthroughs used one CPU node and one 8-GPU NVIDIA H100 80 GB HBM3 SXM5 node (`Multi-Host`
uses two 8-GPU nodes; `Online Runtime Control` uses three 1-GPU H200 nodes). Inspect node and cache
readiness before deploying:

```bash
kubectl get nodes
kubectl get devices -o json
kubectl get acceleratortypes
kubectl get nodemodelstores -o json
```

Confirm sufficient free GPUs (*External DP* needs 8 GPUs; *Elastic EP* starts at 4 and expands to 8)
and warm the walkthrough's `ModelArtifact` with
[Model Prefetch](../model-delivery/prefetch.md) before starting serving pods.

## GPU AcceleratorType setup

Reusable H100 manifests specify the derived pool `gpustack--nvidia-h100-80gb-hbm3-linux-amd64`
(recorded benchmark captures used a custom sibling `bench-gpu` over the same H100 group). On other
hardware, substitute an `Active` whole-GPU `AcceleratorType` from `kubectl get acctype` and verify
VRAM capacity and engine compatibility first.

---

**See also** — [Model Deployment](../../modules/model-deployment/_index.md) (serving configuration
guides) · [Model Prefetch Walkthrough](../model-delivery/prefetch.md) (warming weights onto GPU nodes)

**Next** → [Prefill and Decode Walkthrough](prefill-decode.md) — deploy disaggregated prefill and decode roles behind a router.
