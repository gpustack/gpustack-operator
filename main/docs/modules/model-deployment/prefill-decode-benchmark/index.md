# Prefill and Decode Benchmark

On one H100 SXM5 node, *prefill/decode (PD)* isolated token generation from long-prompt processing.
At concurrency 16, *inter-token latency (ITL)* p99 fell by **25.6×–27.8×** for the two long-prompt shapes.
The cost was higher *time to first token (TTFT)* through one *Prefill* replica and TCP KV transfer.

The initial suites and conclusions describe cross-`Pod` TCP.
The separate [InfiniBand RDMA comparison](#infiniband-rdma-comparison) records the matched follow-up.

## Contents

- [Summary](#summary)
- [Environment](#environment)
- [Suite 1: Fixed shapes](#suite-1-fixed-shapes)
- [Suite 2: Mixed load](#suite-2-mixed-load)
- [Stage execution](#stage-execution)
- [KV transfer](#kv-transfer)
- [Benefits](#benefits)
- [Trade-offs](#trade-offs)
- [InfiniBand RDMA comparison](#infiniband-rdma-comparison)
- [Limitations](#limitations)

## Summary

Both configurations used **six serving GPUs**. Aggregated serving ran three *Server* replicas with
*tensor parallelism (TP)* = 2. *PD* ran one *Prefill* replica and two *Decode* replicas, each with *TP* = 2.
Both used `llm-d-router` and the same Qwen3-32B weights.

**All 16 Suite 1 cells and both Suite 2 streams completed with zero errors in each configuration.**
Long-prompt *ITL* improved under high concurrency. The mixed-load steady stream already had low *ITL*
with aggregated serving and showed no p99 improvement with *PD*.

Choose *PD* when smoother token generation under long-prompt concurrency outweighs first-token delay and KV transfer costs.
Aggregated serving favored throughput in **15 of 16 fixed-shape cells** on this TCP setup.
The TCP results alone do not establish the same trade-off over *remote direct memory access (RDMA)*.

## Environment

| Item | Captured value |
|---|---|
| Cluster | Kubernetes v1.35.7; one CPU node and one GPU node |
| Hardware | 8 × NVIDIA H100 80 GB HBM3 SXM5 on one node; physical NVLink bandwidth 900 GB/s |
| GPUStack Operator | v0.9.1 |
| Runner | `gpustack/runner:cuda13.0-vllm0.30.0`; internal package metadata in this build logs `vLLM 0.19.0` at startup |
| Model | Qwen/Qwen3-32B, 16-bit brain floating-point weights, or BF16 |
| Model delivery | `ModelArtifact` `qwen3-32b`, mounted at `/models` through node delivery |
| Captured `InstanceType` | `bench-gpu`, selecting the H100 accelerator group |
| Aggregated | 3 *Server* replicas × 2 GPUs, *TP* = 2 per replica |
| *PD* | 1 *Prefill* replica + 2 *Decode* replicas, 2 GPUs and *TP* = 2 per replica |
| KV transport | `MooncakeConnector` over TCP; no *RDMA* resources allocated |
| Engine limits | `--max-model-len 16384`, `--max-num-seqs 64`, `--gpu-memory-utilization 0.90` |

The configurations ran sequentially on the same node. Two GPUs were outside each serving allocation.

Each serving `Pod` exposed only its assigned two GPUs and had a private `/dev/shm` and IPC namespace.
*`hostIPC`* was disabled, and no *RDMA* interface was attached.
NVLink carried the intra-`Pod` *TP* traffic.
**All cross-`Pod` communication used TCP sockets over `eth0`, including Mooncake KV transfer between roles.**

## Suite 1: Fixed shapes

Each shape names target input tokens / requested output tokens. Each cell excluded four warm-up
requests. Concurrency 1, 4 and 8 used 32 measured requests; concurrency 16 used 64.
Every cell completed all requests with **zero errors**.

Latency columns retain the captured millisecond precision. *Time per output token (TPOT)* measures
per-request generation time per token; *ITL* measures gaps between streamed tokens.
Bold values mark the lower latency or higher throughput within each matched pair.

### 512/256

| Concurrency | Configuration | Requests | *TTFT* p50 (ms) | *TTFT* p95 (ms) | *TPOT* p50 (ms) | *ITL* p99 (ms) | Output (tok/s) |
|---:|---|---:|---:|---:|---:|---:|---:|
| 1 | Aggregated | 32/32 | **49.030** | **53.318** | **13.532** | **16.886** | **72.937** |
| 1 | *PD* | 32/32 | 129.316 | 138.124 | 13.556 | 17.068 | 70.990 |
| 4 | Aggregated | 32/32 | **55.821** | **143.648** | 13.598 | 17.309 | **289.869** |
| 4 | *PD* | 32/32 | 163.561 | 343.092 | **13.548** | **17.052** | 279.466 |
| 8 | Aggregated | 32/32 | 210.641 | **229.082** | 13.895 | **17.439** | **545.187** |
| 8 | *PD* | 32/32 | **192.224** | 598.643 | **13.629** | 17.444 | 535.895 |
| 16 | Aggregated | 64/64 | **213.619** | **315.869** | 14.182 | **17.318** | **1,040.697** |
| 16 | *PD* | 64/64 | 261.322 | 1,602.287 | **13.877** | 17.785 | 973.298 |

### 4096/128

| Concurrency | Configuration | Requests | *TTFT* p50 (ms) | *TTFT* p95 (ms) | *TPOT* p50 (ms) | *ITL* p99 (ms) | Output (tok/s) |
|---:|---|---:|---:|---:|---:|---:|---:|
| 1 | Aggregated | 32/32 | **264.263** | **272.194** | **13.748** | **16.852** | **63.580** |
| 1 | *PD* | 32/32 | 661.241 | 692.402 | 13.757 | 17.225 | 52.958 |
| 4 | Aggregated | 32/32 | **477.919** | **734.463** | 14.101 | **17.299** | **207.503** |
| 4 | *PD* | 32/32 | 738.476 | 1,884.268 | **13.835** | 17.561 | 187.547 |
| 8 | Aggregated | 32/32 | **717.456** | **1,250.297** | 14.563 | 19.308 | **334.893** |
| 8 | *PD* | 32/32 | 1,356.958 | 4,459.771 | **13.923** | **17.977** | 264.025 |
| 16 | Aggregated | 64/64 | **1,231.691** | **2,119.357** | 19.677 | 458.058 | **490.425** |
| 16 | *PD* | 64/64 | 8,065.377 | 14,363.057 | **13.851** | **17.895** | 194.074 |

### 256/1024

| Concurrency | Configuration | Requests | *TTFT* p50 (ms) | *TTFT* p95 (ms) | *TPOT* p50 (ms) | *ITL* p99 (ms) | Output (tok/s) |
|---:|---|---:|---:|---:|---:|---:|---:|
| 1 | Aggregated | 32/32 | **50.158** | **56.069** | **13.541** | **16.831** | **73.546** |
| 1 | *PD* | 32/32 | 109.703 | 120.828 | 13.558 | 17.027 | 72.810 |
| 4 | Aggregated | 32/32 | **49.191** | **77.951** | **13.547** | **16.751** | **293.725** |
| 4 | *PD* | 32/32 | 141.441 | 232.738 | 13.591 | 16.998 | 291.832 |
| 8 | Aggregated | 32/32 | **61.782** | **115.500** | **13.669** | **16.798** | **580.148** |
| 8 | *PD* | 32/32 | 187.257 | 418.748 | 13.842 | 17.265 | 562.653 |
| 16 | Aggregated | 64/64 | **113.217** | **177.494** | **14.247** | **17.597** | 753.604 |
| 16 | *PD* | 64/64 | 198.038 | 853.881 | 14.406 | 18.334 | **1,078.624** |

### 8192/128

| Concurrency | Configuration | Requests | *TTFT* p50 (ms) | *TTFT* p95 (ms) | *TPOT* p50 (ms) | *ITL* p99 (ms) | Output (tok/s) |
|---:|---|---:|---:|---:|---:|---:|---:|
| 1 | Aggregated | 32/32 | **536.405** | **554.418** | **13.926** | **17.280** | **55.397** |
| 1 | *PD* | 32/32 | 1,314.379 | 1,349.357 | 13.938 | 17.640 | 41.425 |
| 4 | Aggregated | 32/32 | **1,043.456** | **2,042.767** | 14.957 | 18.591 | **148.309** |
| 4 | *PD* | 32/32 | 1,584.477 | 3,800.788 | **14.084** | **17.849** | 137.823 |
| 8 | Aggregated | 32/32 | **1,578.925** | **3,543.434** | 18.152 | 502.555 | **214.569** |
| 8 | *PD* | 32/32 | 4,288.098 | 11,417.297 | **14.098** | **18.766** | 137.024 |
| 16 | Aggregated | 64/64 | **2,072.452** | **4,966.323** | 24.241 | 509.188 | **322.852** |
| 16 | *PD* | 64/64 | 17,514.203 | 26,391.897 | **13.966** | **18.315** | 98.154 |

## Suite 2: Mixed load

The steady stream sent 64 requests with 512 input and 256 output tokens at concurrency 4.
The concurrent burst stream sent 40 requests with 4,096–8,192 target input tokens and 64 output tokens.
Its concurrency was 4, with a two-second burst interval. Both streams finished with **zero errors**.
Each stream's throughput uses the entire mixed-load duration, including burst waits and the final interval.
It is not that stream's saturated throughput.

Burst prompt lengths were unseeded random samples from [4,096, 8,192].
Observed mean lengths were 5,792.275 tokens for aggregated serving and 5,893.1 for *PD*.
The streams therefore used different prompt sets.

| Stream | Configuration | Requests | *TTFT* p50 (ms) | *TTFT* p95 (ms) | *TPOT* p50 (ms) | *ITL* p99 (ms) | Output (tok/s) |
|---|---|---:|---:|---:|---:|---:|---:|
| Steady | Aggregated | 64/64 | **65.755** | **357.217** | 13.638 | **16.925** | **271.112** |
| Steady | *PD* | 64/64 | 217.862 | 1,542.731 | **13.635** | 17.518 | 246.150 |
| Burst | Aggregated | 40/40 | **507.856** | **860.598** | 14.208 | 18.078 | **42.361** |
| Burst | *PD* | 40/40 | 1,264.500 | 2,555.799 | **13.834** | **17.826** | 38.461 |

The steady stream's *ITL* p99 was **16.925 ms** for aggregated serving and 17.518 ms for *PD*.
This mixed-load recording shows **no steady-stream p99 reduction**.
The maximum steady-stream token gap fell from 486.353 ms to **20.850 ms**, a **23.3× reduction**.

The burst stream's *ITL* p99 was 18.078 versus **17.826 ms**.
Its maximum fell from 486.248 to **20.663 ms**, a **23.5× reduction**.
Maximum gaps and p99 describe different parts of the distribution.

Per-request *TPOT* p99 fell from 18.708 to **13.863 ms** on the steady stream.
On the burst stream, it fell from 21.420 to **14.171 ms**.
These per-request averages describe a different distribution from individual token gaps measured by *ITL*.

## Stage execution

Initial and final access-log counts confirm requests reached all expected roles.
The values agree between the two captures. They include warm-up, calibration and auxiliary requests, so they
are not measured-suite request totals or a one-to-one request trace.

| Configuration | Role and captured suffix | Completion requests |
|---|---|---:|
| Aggregated | *Server* `8kjjc` | 405 |
| Aggregated | *Server* `qrh6w` | 387 |
| Aggregated | *Server* `vq2vc` | 428 |
| *PD* | *Prefill* `xmjvs` | 1,173 |
| *PD* | *Decode* `2xglg` | 380 |
| *PD* | *Decode* `r2zt4` | 445 |

The *PD* Router capture contains 844 access lines. The aggregated count capture also contains a
collection-script diagnostic; it supplies no reliable Router count. Engine counts establish role
execution, while client JSON establishes completion and errors.

All successful *PD* Suite 1 and mixed-load JSON records use the final `Pod` snapshot listed above.

## KV transfer

Mooncake producer logs contain **67 APIServer metric windows**, with throughput from 82.827 to 1,516.976 MB/s.
One window reports **1,390.73 MB/s** for 48.0 MB transferred in 34.514 ms.
If that rate applies independently to each *TP* rank, two ranks give approximately **2.78 GB/s**.
The log labels do not identify each rank, and no synchronized rank counters verify that aggregate estimate.

Every captured metric window reported **zero failed transfers and zero failed receives**.

## Benefits

- At `4096/128`, concurrency 16, *ITL* p99 fell from 458.058 ms to **17.895 ms**, a **25.6× reduction**.
- At `8192/128`, concurrency 16, *ITL* p99 fell from 509.188 ms to **18.315 ms**, a **27.8× reduction**.
- At `256/1024`, concurrency 16, output throughput rose from 753.604 to **1,078.624 tok/s**, a **43.1% gain**.

Separating prompt processing protected *Decode* token gaps under heavy long-prompt concurrency.
The decode-heavy throughput comparison includes an aggregated *TTFT* outlier of **30,043.341 ms**.
Its *TTFT* p50 and p95 were 113.217 and 177.494 ms; p99 equaled the maximum outlier.
That single tail outlier extended the aggregated wall time to **86.913 seconds**, versus **60.735 seconds** for *PD*.

Its cause is unresolved. The observed gain needs a repeat run before attributing it to role separation.

## Trade-offs

The single *Prefill* replica queued long prompts while TCP moved their KV tensors.
At `4096/128`, concurrency 16, *TTFT* p50 rose from 1,231.691 ms to **8,065.377 ms**.
At `8192/128`, it rose from 2,072.452 ms to **17,514.203 ms**.
The isolated decode tail therefore came with substantially slower first-token delivery.

Aggregated serving had higher output throughput in **15 of 16 fixed-shape cells**.
At `8192/128`, concurrency 16, it reached 322.852 tok/s against 98.154 tok/s for *PD*.
In Suite 2, aggregated serving also had lower *TTFT* and higher throughput on both streams.
Choose *PD* for the measured decode-isolation benefit only when that latency trade-off fits the workload.

## InfiniBand RDMA comparison

A separate follow-up used **8 × H100 SXM5 and 8 × ConnectX-7 InfiniBand adapters** on one GPU node.
The adapter inventory was `mlx5_0`–`mlx5_7`; all roles remained on `gpu-node-0`.
The allocation remained one *Prefill* and two *Decode* replicas, with two GPUs and *TP* = 2 per replica.
The 16 fixed-shape cells completed **640/640 requests with zero errors**.

### Configuration

Update `pd-deployment.yaml` from the
[TCP walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/prefill-decode/index.md#step-1-start-disaggregated-serving):

```diff
 spec:
+  kvTransfer:
+    protocol: RDMA
   roles:
     - name: prefill
       kind: Prefill
       resources:
         accelerator: "2"
+        interface: "2"
     - name: decode
       kind: Decode
       resources:
         accelerator: "2"
+        interface: "2"
```

Intra-`Pod` *TP* still used NVLink; the RDMA setting applied to cross-`Pod` Mooncake KV transfer.

Mooncake *TP* workers logged protocol `rdma`, KV-cache registration and RDMA-ready acknowledgements.
The captured *Decode* workers enumerated `mlx5_6` and `mlx5_7`, with LIDs `21816` and `21819`.
This runtime evidence establishes the InfiniBand KV path; HTTP routing and bootstrap control traffic remain separate.

The final access capture counted **809** *Prefill* completions and **407 + 402** *Decode* completions.
These include warm-up and calibration; they are broader than the 744 measured client requests.

### Fixed-shape comparison

Bold values mark the lower latency or higher throughput within each matched TCP/RDMA pair.
The baseline and follow-up used the same target shapes and measured request counts.

| Shape | Concurrency | TCP *TTFT* p50 (ms) | RDMA *TTFT* p50 (ms) | TCP *ITL* p99 (ms) | RDMA *ITL* p99 (ms) | TCP output (tok/s) | RDMA output (tok/s) |
|---|---:|---:|---:|---:|---:|---:|---:|
| `512/256` | 1 | 129.316 | **80.237** | 17.068 | **16.819** | 70.990 | **72.934** |
| `512/256` | 4 | 163.561 | **123.806** | **17.052** | 18.038 | 279.466 | **284.946** |
| `512/256` | 8 | **192.224** | 215.574 | 17.444 | **17.392** | 535.895 | **538.096** |
| `512/256` | 16 | **261.322** | 372.135 | 17.785 | **17.363** | 973.298 | **995.358** |
| `4096/128` | 1 | 661.241 | **326.837** | 17.225 | **16.961** | 52.958 | **62.165** |
| `4096/128` | 4 | 738.476 | **374.910** | 17.561 | **17.525** | 187.547 | **223.823** |
| `4096/128` | 8 | 1,356.958 | **626.566** | 17.977 | **17.780** | 264.025 | **343.344** |
| `4096/128` | 16 | 8,065.377 | **1,966.856** | **17.895** | 19.221 | 194.074 | **475.153** |
| `256/1024` | 1 | 109.703 | **82.139** | 17.027 | **16.740** | 72.810 | **73.976** |
| `256/1024` | 4 | 141.441 | **135.288** | **16.998** | 17.220 | **291.832** | 290.996 |
| `256/1024` | 8 | 187.257 | **165.782** | **17.265** | 17.474 | 562.653 | **568.279** |
| `256/1024` | 16 | **198.038** | 302.155 | **18.334** | 18.487 | 1,078.624 | **1,086.885** |
| `8192/128` | 1 | 1,314.379 | **651.440** | 17.640 | **17.383** | 41.425 | **53.200** |
| `8192/128` | 4 | 1,584.477 | **721.694** | **17.849** | 18.107 | 137.823 | **184.952** |
| `8192/128` | 8 | 4,288.098 | **2,209.757** | **18.766** | 19.149 | 137.024 | **223.497** |
| `8192/128` | 16 | 17,514.203 | **6,256.258** | **18.315** | 18.674 | 98.154 | **237.019** |

At `4096/128`, concurrency 16, RDMA reduced median *TTFT* **4.10×** and raised throughput **2.45×** against TCP *PD*.
At `8192/128`, concurrency 16, median *TTFT* fell **2.80×**, with **2.41× higher throughput**.
Long-prompt transfer improved without sacrificing the separated *Decode* role.

The single *Prefill* replica still limited high-concurrency first-token delivery.
At `4096/128`, concurrency 16, aggregated serving retained lower median *TTFT*: 1,231.691 versus 1,966.856 ms with RDMA *PD*.
At `8192/128`, the values were 2,072.452 versus 6,256.258 ms.
RDMA throughput remained below aggregated serving in both cells: 475.153 versus 490.425 tok/s, and 237.019 versus 322.852 tok/s.

### Mixed-load comparison

The follow-up completed **64 steady and 40 burst requests with zero errors**, for **744/744 measured requests** across both suites.
Per-stream throughput uses the full mixed window, including burst sleep intervals: 60.260 s for RDMA versus 66.561 s for TCP.
Burst prompts were unseeded samples from the same target range, not a matched-prompt replay.

| Stream | TCP *TTFT* p50 (ms) | RDMA *TTFT* p50 (ms) | TCP *ITL* p99 (ms) | RDMA *ITL* p99 (ms) | TCP output (tok/s) | RDMA output (tok/s) |
|---|---:|---:|---:|---:|---:|---:|
| Steady | 217.862 | **140.293** | 17.518 | **17.234** | 246.150 | **271.887** |
| Burst | 1,264.500 | **619.482** | **17.826** | 17.987 | 38.461 | **42.482** |

RDMA lowered median *TTFT* **1.55×** on the steady stream and **2.04×** on the burst stream.
Maximum *ITL* remained close to TCP: 20.895 versus 20.850 ms for steady traffic, and 21.036 versus 20.663 ms for bursts.
Burst *ITL* p99 increased slightly from 17.826 to 17.987 ms.

### Remaining trade-offs

RDMA did not improve every latency metric.
At `512/256`, median *TTFT* rose from 192.224 to 215.574 ms at concurrency 8 and from 261.322 to 372.135 ms at concurrency 16.
At `256/1024`, concurrency 16, it rose from 198.038 to 302.155 ms.
The `256/1024`, concurrency-4 output throughput also fell slightly: 291.832 to 290.996 tok/s.

The captured RDMA run used larger KV blocks than the engine default.
It does not establish the same results with default RDMA block sizes.
Separate runs, small prompt-length variations and early stopping prevent a transport-only causal or equal-token-work claim.

## Limitations

The TCP baseline and RDMA follow-up cover one model and one pass per configuration on single-node H100 setups.
They do not measure multiple GPU nodes, additional *Prefill* replicas or other model sizes.
Intra-`Pod` *TP* uses NVLink; cross-role KV transfer uses TCP or InfiniBand according to the capture.
Neither capture measures KV transfer over NVLink.

The mixed burst used a range of prompt lengths, with different observed lengths between configurations.
It is not a matched-prompt replay. The fixed-shape samples also have small token-count variations.
Requested output lengths are caps; some requests ended earlier. Throughput uses actual output token counts.
Zero client errors establish completion for these samples, not a general availability guarantee.

---

**See also** — [Prefill and Decode](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode/index.md) (role pairing and transfer contract) ·
[Prefill and Decode Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/prefill-decode/index.md) (deployment manifest)

**Next** → [Encode, Prefill and Decode](/gpustack-operator/main/docs/modules/model-deployment/encode-prefill-decode/index.md)
