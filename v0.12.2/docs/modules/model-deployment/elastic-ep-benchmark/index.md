# Elastic EP Benchmark

On one H100 SXM5 node, an *expert parallelism (EP)* collective expanded in place from four GPUs to
eight. At concurrency 64 over TCP, *time to first token (TTFT)* p50 fell **2.50×** (6,509.231 ms to
**2,598.867 ms**), while single-request generation slowed and output throughput fell in all six
cells. The live expansion stream completed **439/439 requests with zero errors**.

The initial suites describe cross-`Pod` TCP. The separate
[InfiniBand RDMA comparison](#infiniband-rdma-comparison) records the matched follow-up.

## Contents

- [Summary](#summary)
- [Environment](#environment)
- [Results](#results)
- [Verification](#verification)
- [InfiniBand RDMA comparison](#infiniband-rdma-comparison)
- [Limitations](#limitations)

## Summary

One `ModelDeployment` (`b2-elastic-ep` in `gpustack-bench`) kept *tensor parallelism (TP)* = 2 per
member while *data parallelism (DP)* width grew from 2 to 4 (*EP* world size 4 → 8 GPUs). Existing
`Pod`s kept their UIDs and zero restart counts. [Elastic EP](/gpustack-operator/v0.12.2/docs/modules/model-deployment/elastic-ep/index.md) defines the profile and
resize contract.

**Doubling the GPU count over TCP did not raise throughput.** Only one API server accepted requests
in both states.

- **Choose online width growth** when process continuity and concurrency-64 first-token relief
  justify extra GPUs (especially over InfiniBand RDMA, where width 4 also led concurrency-64 output).
- **Choose width 2** on TCP for lower concurrency, throughput and token-generation latency. Even at
  concurrency 64 on TCP, end-to-end p50 rose at width 4 (11,487.169 to 12,154.350 ms).

### Benefits

- Expanded **4 → 8 GPUs** in place while keeping the serving master and original GPU worker; live
  load completed **439/439 requests with zero errors**.
- Cut concurrency-64 *TTFT* p50 **2.50×** (6,509.231 to **2,598.867 ms**) and *TTFT* p95
  (8,708.596 to **4,789.148 ms**).

End-to-end p50 still rose from 11,487.169 to 12,154.350 ms at concurrency 64.

### Trade-offs

- Single-request *ITL* p50 rose **83.0%** (8.566 to 15.675 ms), and concurrency-64 *ITL* p50 rose
  from 16.522 to **30.270 ms** across the four-`Pod` TCP collective.
- Output throughput fell in all six TCP cells (109.586 to 61.020 tok/s at concurrency 1; 1,145.131
  versus 1,121.379 tok/s at concurrency 64).

The capture does not isolate collective, TCP and single-API-server overhead separately.

## Environment

| Item | Captured value |
|---|---|
| Cluster | Kubernetes v1.35.7; one CPU node and one GPU node |
| Hardware | 8 × NVIDIA H100 80 GB HBM3 SXM5 on one node; physical NVLink bandwidth 900 GB/s |
| GPUStack Operator | v0.9.1 |
| Runner | `gpustack/runner:cuda13.0-vllm0.30.0`, Ray V2 executor enabled |
| Model | `deepseek-ai/DeepSeek-V2-Lite-Chat`, revision `85864749cd611b4353ce1decdb286193298f64c7` |
| Model delivery | `ModelArtifact` `deepseek-v2-lite-chat`, mounted at `/models` through node delivery |
| Captured `InstanceType` | Custom `bench-gpu`, selecting the H100 accelerator group |
| Engine limits | `--max-model-len 8192`, `--max-num-seqs 16`, `--gpu-memory-utilization 0.90` |
| Collective | `--all2all-backend allgather_reducescatter`, `--elastic-ep-max-dp-size 4` |
| Serving | `--api-server-count 1`; no managed Router |

The deployment used one *Server* role (*`replicas: 1`*, *`size: 1`*). Only
`elastic.dataParallelWidth` changed. The CPU Ray head and all GPU members shared one GPU node. See
the [walkthrough](/gpustack-operator/v0.12.2/docs/walkthroughs/model-deployment/elastic-ep/index.md) for the manifest and checks.

Each GPU `Pod` exposed only its two assigned GPUs with private IPC and `/dev/shm`, *`hostIPC`*
disabled and no *remote direct memory access (RDMA)* interface. NVLink carried intra-`Pod` *TP*
traffic; **all cross-`Pod` *EP* collective traffic used TCP sockets over `eth0`.**

| State | Width | GPU members | GPUs | *TP* | *EP* world size |
|---|---:|---:|---:|---:|---:|
| State 1 | 2 | 2 | 4 | 2 | 4 |
| State 2 | 2 → 4 | 2 → 4 | 4 → 8 | 2 | 4 → 8 |
| State 3 | 4 | 4 | 8 | 2 | 8 |

## Results

### Suite 1: Serving width comparison

Each state ran one sweep of target 1,024-input / 256-output-token requests (observed prompts
1,021–1,025 tokens). Concurrency 1, 4 and 8 used 32 requests; concurrency 16 used 64; concurrency 32
and 64 used 128, each excluding four warm-up requests. **All 416 measured requests per state
completed with zero errors.** Output throughput divides actual generated tokens by cell wall time.

![TTFT p50, ITL p50 and output throughput by concurrency for Elastic EP width 2 and width 4 over TCP](/gpustack-operator/v0.12.2/docs/assets/diagrams/elastic-ep-benchmark-tcp.svg)

Over TCP with one API server, width 4 (8 GPUs) beat width 2 (4 GPUs) on *TTFT* only at concurrency
64 (**2.50×** lower *TTFT* p50: 6,509.231 to **2,598.867 ms**; *TTFT* p95 8,708.596 to
**4,789.148 ms**), with *ITL* p99 nearly flat (157.753 versus **155.538 ms**).

Across all six concurrencies, width 4 slowed *ITL* p50 (up **83.0%** at concurrency 1: 8.566 to
15.675 ms) and lowered output throughput, and end-to-end p50 still rose at concurrency 64
(11,487.169 to 12,154.350 ms).

*Time per output token (TPOT)* measures per-request generation time per token; *inter-token latency
(ITL)* measures streamed-token gaps. Bold values mark lower latency or higher throughput within each
matched pair.

| Concurrency | Width | GPUs | Requests | *TTFT* p50 (ms) | *TTFT* p95 (ms) | *TPOT* p50 (ms) | *ITL* p50 (ms) | *ITL* p99 (ms) | Output (tok/s) |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 1 | 2 | 4 | 32/32 | **108.853** | **137.983** | **8.531** | **8.566** | **9.134** | **109.586** |
| 1 | 4 | 8 | 32/32 | 167.142 | 189.872 | 15.687 | 15.675 | 17.600 | 61.020 |
| 4 | 2 | 4 | 32/32 | **153.733** | **340.863** | **9.912** | **8.651** | **12.290** | **352.630** |
| 4 | 4 | 8 | 32/32 | 219.585 | 1,022.254 | 16.898 | 15.941 | 19.204 | 214.606 |
| 8 | 2 | 4 | 32/32 | **263.820** | **628.698** | **12.773** | **11.279** | **88.721** | **524.388** |
| 8 | 4 | 8 | 32/32 | 328.685 | 779.087 | 22.957 | 22.430 | 132.667 | 307.574 |
| 16 | 2 | 4 | 64/64 | **384.459** | **1,090.247** | **17.781** | **15.178** | **91.049** | **751.199** |
| 16 | 4 | 8 | 64/64 | 502.015 | 1,454.118 | 29.047 | 23.305 | 141.867 | 470.798 |
| 32 | 2 | 4 | 128/128 | **660.074** | **2,118.262** | **23.106** | **16.109** | **99.608** | **1,140.701** |
| 32 | 4 | 8 | 128/128 | 1,096.666 | 2,645.267 | 36.916 | 29.288 | 156.532 | 721.243 |
| 64 | 2 | 4 | 128/128 | 6,509.231 | 8,708.596 | **22.780** | **16.522** | 157.753 | **1,145.131** |
| 64 | 4 | 8 | 128/128 | **2,598.867** | **4,789.148** | 38.357 | 30.270 | **155.538** | 1,121.379 |

### Live resize

A concurrency-8 stream (`1,024/256` shape) ran across the width 2 → 4 patch and completed
**439/439 requests in 264.227 seconds (264.23 s) with zero client errors**. The capture recorded
**zero client-facing HTTP 503 responses** and **10 `/health` HTTP 503 responses** on the master
during re-rendezvous.

| Observation | Captured result |
|---|---|
| Phase check | 12 seconds from patch submission; the harness still read `Ready` while serving continued |
| Role readiness | About 189 seconds from the patch to `RoleKindsReady` and `EndpointEligibility` becoming `True` |
| Final native width | Desired, admitted, allocated, Ray and effective width all known at 4 |
| Final phase | `Ready`, with one desired and one ready replica group |
| Existing `Pod`s | CPU Ray head, serving master and existing GPU member retained |
| Container restarts | 0 across all five final `Pod`s |
| Added members | 2 GPU `Pod`s, each with 2 GPUs |
| Client completion | 439/439; 0 errors |

**The 12-second timer checked phase only, not effective-width convergence.** The patch arrived at
08:16:11Z, and `RoleKindsReady` and `EndpointEligibility` became `True` at 08:19:20Z (**about 189
seconds later**), covering startup and Ray join for the two new GPU `Pod`s. Final `observation.json`
confirmed effective width 4 without a convergence timestamp.

The watch capture polled every **16–21 seconds** (up to 28 seconds):

| Time (UTC) | Phase | Role summary |
|---|---|---|
| 08:18:50Z | `Ready` | `1S` |
| 08:19:09Z | `Starting` | `0S` |
| 08:19:30Z | `Ready` | `1S` |

The `Starting` sample fell between `Ready` samples **40 seconds apart**, so the exact withdrawal
duration is unknown. See the
[walkthrough](/gpustack-operator/v0.12.2/docs/walkthroughs/model-deployment/elastic-ep/index.md#step-3-expand-and-verify-inference)
for effective-width verification.

## Verification

Final observation reported generation 2, stable width 4 and reason
`native ranks and forwards agree`. `ReplicasUpToDate` stayed `Unknown/RolloutNotObserved`. The
master, CPU head and original GPU worker kept their UIDs and zero restart counts.

Master completion access logs rose from **880** before State 3 to **1,320** at finish (including
warm-up and calibration). No Router was configured; client JSON establishes completion and errors.
After deletion, `Devices` reported all eight H100 GPUs with *`remaining`* = `1600000`.

A later runtime-acceptance run repeated width 2 → 4 on an eight-GPU H100 node:

| Observation | Captured result |
|---|---|
| Ready at width 2 | about two minutes after the apply |
| State-1 sweep | six concurrency cells, zero errors |
| Load through the resize | 471 requests over 260 seconds, zero errors |
| Width patch 2 → 4 to Ready | ten seconds |
| Ray cluster | three active nodes and four GPUs → five nodes and eight GPUs |
| Final observation | effective width 4, stable width 4, reason `native ranks and forwards agree` |
| ElasticResize condition | `True` / `NoRefusal` |
| State-3 sweep | six concurrency cells at width 4, zero errors |

The operator deletes the operation record after completion, leaving terminal state in the condition
and observation. An earlier attempt on the same image did not converge and remains unexplained.

## InfiniBand RDMA comparison

A follow-up on the ConnectX-7 InfiniBand node from
[Prefill and Decode Benchmark](/gpustack-operator/v0.12.2/docs/modules/model-deployment/prefill-decode-benchmark/index.md#infiniband-rdma-comparison) expanded
width 2 → 4 at *TP* = 2. Selected final cells completed **416/416 requests per width with zero
errors**.

### Transport verification

Update `elastic-ep.yaml` from the
[TCP walkthrough](/gpustack-operator/v0.12.2/docs/walkthroughs/model-deployment/elastic-ep/index.md#step-1-start-the-elastic-deployment)
(the diff shows current interface allocation; the capture used manual device exposure):

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: ModelDeployment
 metadata:
   name: b2-elastic-ep
   namespace: gpustack-bench
 spec:
   roles:
     - name: server
       kind: Server
       size: 1
       resources:
         accelerator: "2"
+        interface: "2"
       runtimeControl:
         parameters:
           elastic.dataParallelWidth: "2"
       extraArgs:
         - --gpu-memory-utilization
-        - "0.90"
+        - "0.85"
```

NCCL logged `Using network IB` with GPU Direct RDMA enabled, moving the cross-`Pod`
`allgather_reducescatter` collective onto InfiniBand while intra-`Pod` *TP* stayed on NVLink.
Because `--gpu-memory-utilization` also changed (`0.90` → `0.85`), the runs do not isolate
transport alone.

### RDMA serving widths

![TTFT p50, output throughput, ITL p50 and ITL p99 for Elastic EP width 2 and width 4 over TCP and InfiniBand RDMA](/gpustack-operator/v0.12.2/docs/assets/diagrams/elastic-ep-benchmark-rdma.svg)

In this separate InfiniBand follow-up (`--gpu-memory-utilization 0.85`), RDMA cut width-4
concurrency-64 *TTFT* p50 **6.96×** (2,598.867 to **373.486 ms**), raised output **3.35×**
(1,121.379 to **3,755.290 tok/s**) and cut *ITL* p50 (30.270 to **10.838 ms**). Within RDMA at
concurrency 64, width 4 beat width 2 by **8.23×** on *TTFT* p50 and **1.64×** on output, reversing
TCP.

Exceptions remain: width 2 kept higher RDMA output at concurrencies 1–32 and lower single-request
*ITL* p50 (**6.111** versus 6.979 ms), and RDMA width 2, concurrency 4 regressed *ITL* p99 from
**12.290** to 68.396 ms.

Bold values mark lower latency or higher throughput within each matched TCP/RDMA pair at the same
width. The width-4 sweep ran twice; the table records the second pass.

| Width | Concurrency | TCP *TTFT* p50 (ms) | RDMA *TTFT* p50 (ms) | TCP *ITL* p50 (ms) | RDMA *ITL* p50 (ms) | TCP *ITL* p99 (ms) | RDMA *ITL* p99 (ms) | TCP output (tok/s) | RDMA output (tok/s) |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 2 | 1 | 108.853 | **87.879** | 8.566 | **6.111** | 9.134 | **8.765** | 109.586 | **151.749** |
| 2 | 4 | 153.733 | **89.464** | 8.651 | **6.312** | **12.290** | 68.396 | 352.630 | **502.750** |
| 2 | 8 | 263.820 | **91.644** | 11.279 | **7.049** | 88.721 | **69.554** | 524.388 | **813.467** |
| 2 | 16 | 384.459 | **221.884** | 15.178 | **8.062** | 91.049 | **70.393** | 751.199 | **1,469.541** |
| 2 | 32 | 660.074 | **195.639** | 16.109 | **8.759** | 99.608 | **74.344** | 1,140.701 | **2,343.859** |
| 2 | 64 | 6,509.231 | **3,073.825** | 16.522 | **8.823** | 157.753 | **75.314** | 1,145.131 | **2,292.536** |
| 4 | 1 | 167.142 | **94.186** | 15.675 | **6.979** | 17.600 | **9.336** | 61.020 | **133.567** |
| 4 | 4 | 219.585 | **153.424** | 15.941 | **7.146** | 19.204 | **12.235** | 214.606 | **473.118** |
| 4 | 8 | 328.685 | **119.673** | 22.430 | **8.302** | 132.667 | **73.833** | 307.574 | **687.993** |
| 4 | 16 | 502.015 | **190.883** | 23.305 | **8.905** | 141.867 | **79.967** | 470.798 | **1,322.207** |
| 4 | 32 | 1,096.666 | **185.406** | 29.288 | **10.088** | 156.532 | **76.300** | 721.243 | **2,000.530** |
| 4 | 64 | 2,598.867 | **373.486** | 30.270 | **10.838** | 155.538 | **81.544** | 1,121.379 | **3,755.290** |

### Resize verification

Live expansion load completed **813/813 requests in 261.794 seconds with zero errors**. Surviving
`Pod`s kept their UIDs with zero restarts across all five final `Pod`s, and generation-2
observation confirmed effective and stable width 4.

| Observation | Captured interval |
|---|---|
| New GPU `Pod`s created | About 5 seconds after the patch |
| Both new workers registered with Ray | About 13 seconds after the patch |
| `RoleKindsReady` and `EndpointEligibility` became `True` | About 167 seconds after the patch |
| Final observation | Effective width 4; exact convergence time not recorded |

Ray registration does not prove collective readiness. The harness timer polled a missing
`current_width` field, so its 748-second value is not a convergence time; read *`effective.Value`*
and *`stableWidth`* instead. The **2,134** total completion lines span width 2, expansion load and
both width-4 passes.

### RDMA width trade-offs

- **Choose RDMA width 4** for high-concurrency peaks (concurrency 64), where it led both *TTFT* and
  output throughput.
- **Choose RDMA width 2** for concurrencies 1–32 or lowest single-request *ITL* p50.

The follow-up supports high-concurrency InfiniBand expansion without proving linear scaling or
transport-only causality.

## Limitations

- Single-node H100 captures with one sweep per width give no confidence intervals for small deltas.
- State 1 used four GPUs and State 3 used eight (in-place capacity growth, not equal-GPU efficiency).
- Only width 2 → 4 at *TP* = 2 ran; cross-node expansion, other collectives and scale-down were not
  tested.
- Neither capture measures cross-`Pod` NVLink or isolates transport cost from collective size and
  memory-utilization changes.
- Single-API-server *internal load balancing (Internal LB)* ran; per-rank HTTP serving was not
  measured here.
- The 12-second phase check is not an effective-width timer; zero client errors do not guarantee
  uninterrupted readiness or stable token gaps during every resize.
- Retained UIDs and restart counts prove `Pod` continuity, not unchanged engine internals; `Ready`
  does not imply every condition was true.

---

**See also** — [Elastic EP](/gpustack-operator/v0.12.2/docs/modules/model-deployment/elastic-ep/index.md) (profile and resize contract) ·
[Elastic EP Walkthrough](/gpustack-operator/v0.12.2/docs/walkthroughs/model-deployment/elastic-ep/index.md) (manifest and effective-width verification) ·
[Prefill and Decode Benchmark](/gpustack-operator/v0.12.2/docs/modules/model-deployment/prefill-decode-benchmark/index.md) (equal-GPU disaggregation measurements)

**Next** → [External DP](/gpustack-operator/v0.12.2/docs/modules/model-deployment/external-dp/index.md) — run one fixed group with an HTTP endpoint on every rank.
