# Elastic EP Benchmark

On one H100 SXM5 node, an *expert parallelism (EP)* collective expanded from four GPUs to eight.
At concurrency 64, *time to first token (TTFT)* p50 fell from 6,509.231 ms to
**2,598.867 ms**, a **2.50× reduction**. Single-request token generation became slower.
The live expansion load completed **439/439 requests with zero errors**.

The initial suites describe cross-`Pod` TCP.
The separate [InfiniBand RDMA comparison](#infiniband-rdma-comparison) records the follow-up.

## Contents

- [Summary](#summary)
- [Environment](#environment)
- [Suite 1: Width comparison](#suite-1-width-comparison)
- [Live resize](#live-resize)
- [Verification](#verification)
- [Benefits](#benefits)
- [Trade-offs](#trade-offs)
- [InfiniBand RDMA comparison](#infiniband-rdma-comparison)
- [Limitations](#limitations)

## Summary

One `ModelDeployment`, `b2-elastic-ep` in namespace `gpustack-bench`, kept
*tensor parallelism (TP)* = 2 per member. Its *data parallelism (DP)* width grew from 2 to 4.
The *EP* world size grew from 4 to 8 GPUs. Existing `Pod`s retained their UIDs and restart counts.
[Elastic EP](/gpustack-operator/main/docs/modules/model-deployment/elastic-ep/index.md) defines the profile and resize contract.

The wider collective reduced first-token queueing at concurrency 64, but output throughput
remained lower at every measured concurrency. **Doubling the GPU allocation did not double throughput.**
Only one API server accepted requests in both states.

Choose online width growth when retaining serving processes and reducing high-concurrency first-token delay justify the extra GPUs.
For this TCP collective, width 2 favored lower concurrency, throughput and token-generation latency.
The crossover at concurrency 64 concerns median first-token latency; total request latency still increased at width 4.

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

The deployment used one *Server* role with *`replicas: 1`* and *`size: 1`*.
Only *`elasticEp.width`* changed. The CPU-only Ray head remained outside the GPU allocation.
The head and all GPU members ran on the same GPU node.
The [walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/elastic-ep/index.md) provides the manifest and verification commands.

The captured GPU `Pod`s exposed only their two assigned GPUs and used private IPC namespaces.
Each had its own `/dev/shm`, with *`hostIPC`* disabled and no remote direct memory access (*RDMA*) interface.
NVLink carried *TP* traffic within each `Pod`.
**All cross-`Pod` communication, including the *EP* collective, used TCP sockets over `eth0`.**

| State | Width | GPU members | GPUs | *TP* | *EP* world size |
|---|---:|---:|---:|---:|---:|
| State 1 | 2 | 2 | 4 | 2 | 4 |
| State 2 | 2 → 4 | 2 → 4 | 4 → 8 | 2 | 4 → 8 |
| State 3 | 4 | 4 | 8 | 2 | 8 |

## Suite 1: Width comparison

Each state ran one sweep of target 1,024-input / 256-output-token requests.
Concurrency 1, 4 and 8 used 32 measured requests; concurrency 16 used 64; concurrency 32 and 64 used 128.
Each cell excluded four warm-up requests. **All 416 measured requests per state completed with zero errors.**

Observed prompt lengths ranged from 1,021 to 1,025 tokens. Output lengths were caps;
some requests ended earlier. Throughput uses actual generated tokens divided by cell wall time.
The same deployment ran State 1, a live expansion, then State 3.

*Time per output token (TPOT)* measures per-request generation time per token.
*Inter-token latency (ITL)* measures gaps between streamed tokens.
Values retain the JSON precision. Bold values mark the lower latency or higher throughput within each matched pair.

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

## Live resize

A concurrency-8 stream ran during the width 2 → 4 change, using the same 1,024/256 target shape.
It completed **439/439 requests in 264.227 seconds (264.23 s), with zero client errors**.
The `scaling-refusals-503.txt` capture recorded **zero client-facing HTTP 503 responses**.
The serving master recorded **10 `/health` responses with HTTP 503** during collective re-rendezvous.

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

**The 12-second timer measured a phase-only check, not effective-width convergence.**
The harness's `wait_ready` returned before collective re-rendezvous, while *`status.phase`* remained `Ready` and serving continued.
It did not require the current generation's effective width.

The patch was submitted at 08:16:11Z.
In `scaled-cr.json`, `RoleKindsReady` and `EndpointEligibility` transitioned to `True` at 08:19:20Z, **about 189 seconds later**.
This interval covers initialization and Ray collective joining for the two added GPU `Pod`s.
The final `observation.json` separately confirmed effective width 4; it contains no timestamp for that convergence.

The `elastic-watch-timeline.txt` capture sampled approximately every **16–21 seconds**, with occasional intervals up to 28 seconds.

| Time (UTC) | Phase | Role summary |
|---|---|---|
| 08:18:50Z | `Ready` | `1S` |
| 08:19:09Z | `Starting` | `0S` |
| 08:19:30Z | `Ready` | `1S` |

The `Starting` sample occurred during collective re-rendezvous, between `Ready` samples **40 seconds apart**.
The exact readiness withdrawal duration is unknown.
The observed client requests completed despite this transition.

The [walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/elastic-ep/index.md#step-3-expand-and-verify-inference)
checks effective width before accepting expansion.

## Verification

The final observation reported generation 2 and stable width 4, with reason
`native ranks and forwards agree`. The observation is diagnostic `ConfigMap` data;
ordinary role status still reports one replica group. `ReplicasUpToDate` remained
`Unknown/RolloutNotObserved` before and after expansion.

The serving master's UID and zero restart count matched across both `Pod` snapshots.
The CPU Ray head and original GPU worker also retained their identities.
These observations establish retention of surviving processes; they do not count internal engine weight-loading operations.

The master's completion access-log count grew from **880** in the pre-State-3 capture to **1,320**
in the final capture. Counts include measured requests, warm-up and calibration.
No Router was configured; the collection script's missing-Router diagnostic is not a serving failure.
Client JSON establishes request completion and errors.

After deletion, the `Devices` snapshot recorded all eight H100 accelerators with *`remaining`* = `1600000`.
This records released accelerator claims; it does not prove removal of model-cache or cluster infrastructure.

A later runtime-acceptance run measured the same expansion on an eight-GPU *H100* node:

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

The operator deletes the operation record when the operation ends, so the terminal state reads from
the condition and the observation. An earlier attempt on the same image did not converge and remains
an open observation; this run does not explain it.

## Benefits

- The deployment expanded from **4 to 8 GPUs** while retaining its serving master and existing GPU worker.
  The concurrent load completed **439/439 requests with zero errors**.
- At concurrency 64, width 4 had **2.50× lower TTFT p50** than width 2: **2,598.867 ms** against **6,509.231 ms**.
  *TTFT* p95 fell from 8,708.596 ms to **4,789.148 ms**.

The first-token improvement appears at the highest tested concurrency.
It does not establish lower total request latency: end-to-end p50 rose from 11,487.169 ms to 12,154.350 ms there.

## Trade-offs

**Single-request *ITL* p50 rose from 8.566 ms to 15.675 ms, an 83.0% increase.**
With one request, queueing relief is limited.
The eight-GPU `allgather_reducescatter` collective spans four *TP* = 2 `Pod`s.
Communication between those `Pod`s used TCP over `eth0`.
The higher token cost is consistent with collective and TCP overhead; their individual contributions were not isolated.

**Output throughput fell at all six concurrencies.** At concurrency 1, it fell from 109.586 to 61.020 tok/s.
At concurrency 64, it was 1,145.131 versus 1,121.379 tok/s despite the doubled GPU allocation.
The single API server and selected collective remain part of both configurations.
The measurement does not isolate the API server as a bottleneck.

At concurrency 64, *ITL* p50 rose from 16.522 ms to **30.270 ms**.
The lower first-token queueing delay came with slower token generation.
Choose width from the workload's latency targets and GPU cost, rather than assuming wider always means faster.

## InfiniBand RDMA comparison

A follow-up on the same ConnectX-7 InfiniBand node setup described in
[Prefill and Decode Benchmark](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode-benchmark/index.md#infiniband-rdma-comparison) expanded the
*TP* = 2 member layout from width 2 to width 4. The final selected cells completed **416/416
requests per width, with zero errors**.

### Transport verification

Update `elastic-ep.yaml` from the
[TCP walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/elastic-ep/index.md#step-1-start-the-elastic-deployment):
The diff uses current interface allocation; the recorded run used manual device exposure.

```diff
 spec:
   roles:
     - name: server
       kind: Server
       size: 1
       resources:
         accelerator: "2"
+        interface: "2"
       elasticEp:
         width: 2
       extraArgs:
         - --gpu-memory-utilization
-        - "0.90"
+        - "0.85"
```

NCCL logs reported `Using network IB` and GPU Direct RDMA enabled.
The cross-`Pod` `allgather_reducescatter` collective used InfiniBand instead of TCP over `eth0`.
Intra-`Pod` *TP* still used NVLink. Ray, HTTP and bootstrap control traffic are separate from collective transport.

GPU memory utilization also changed, so the runs do not isolate transport alone.

### Width comparison

Bold values mark lower latency or higher throughput within each TCP/RDMA pair at the same width.
Each cell used the same target shape, concurrency and measured request count as its TCP baseline.
The width-4 sweep ran twice; the selected final JSON files contain the second pass.

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

At width 4, concurrency 64, RDMA reduced median *TTFT* from 2,598.867 to **373.486 ms**, a **6.96× reduction**.
Output throughput rose from 1,121.379 to **3,755.290 tok/s**, a **3.35× increase**.
Median *ITL* fell from 30.270 to **10.838 ms**.

Within the RDMA follow-up, width 4 had **8.23× lower median TTFT** than width 2 at concurrency 64.
The values were **373.486 versus 3,073.825 ms**; output throughput was **1.64× higher** at width 4.
This reverses the TCP result in which width 4 had lower output throughput at every tested concurrency.

### Resize verification

The expansion load completed **813/813 requests in 261.794 seconds, with zero errors**.
The serving master, original GPU member and CPU Ray head retained their UIDs.
Two GPU `Pod`s joined; all five final `Pod`s had zero container restarts.
The final generation-2 observation confirmed known effective width and stable width 4.

| Observation | Captured interval |
|---|---|
| New GPU `Pod`s created | About 5 seconds after the patch |
| Both new workers registered with Ray | About 13 seconds after the patch |
| `RoleKindsReady` and `EndpointEligibility` became `True` | About 167 seconds after the patch |
| Final observation | Effective width 4; exact convergence time not recorded |

Ray registration does not establish that the expanded collective is ready.
The retained harness timer polled a missing `current_width` field, so its 748-second value is not a convergence measurement.
The observation records width under *`effective.Value`* and *`stableWidth`*.
The final **2,134** completion lines span width 2, expansion load and both width-4 sweeps, including warm-up and calibration.

### Remaining trade-offs

RDMA did not make width 4 faster for every workload.
Single-request *ITL* p50 remained higher at width 4: **6.979 versus 6.111 ms** at width 2.
Width 2 retained higher output throughput at concurrencies 1–32; width 4 led at concurrency 64.
At width 2, concurrency 4, *ITL* p99 worsened from **12.290 to 68.396 ms** despite better median latency and throughput.

Choose width from observed demand and token-latency targets.
The follow-up supports high-concurrency width growth over InfiniBand; it does not establish linear scaling or transport-only causality.

## Limitations

- The TCP baseline measured one model, one H100 SXM5 node, one runner configuration and one sweep per width.
  No confidence intervals or repeated trials establish the size of small differences.
- State 1 used four GPUs; State 3 used eight. This is an in-place capacity comparison, not an equal-GPU efficiency comparison.
- Only width 2 → 4 at *TP* = 2 was captured. Cross-node expansion, other collectives and scale-down were not tested.
- The initial recording used intra-`Pod` NVLink and cross-`Pod` TCP; the follow-up verified InfiniBand collective transport.
  Neither measures cross-`Pod` NVLink or isolates transport cost from collective size and configuration changes.
- Single-API-server *internal load balancing (Internal LB)* was used. Per-rank HTTP serving was not measured here.
- The 12-second phase check is not an effective-width convergence timer.
  Zero client errors do not establish uninterrupted readiness or unchanged token gaps during every resize.
- Retained UIDs and restart counts prove `Pod` continuity, not unchanged internal engine state.
  Final `Ready` does not mean every status condition was true.

---

**See also** — [Elastic EP](/gpustack-operator/main/docs/modules/model-deployment/elastic-ep/index.md) (profile and resize contract) ·
[Elastic EP Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/elastic-ep/index.md) (manifest and effective-width verification) ·
[Prefill and Decode Benchmark](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode-benchmark/index.md) (equal-GPU disaggregation measurements)

**Next** → [External DP](/gpustack-operator/main/docs/modules/model-deployment/external-dp/index.md) — run one fixed group with an HTTP endpoint on every rank.
