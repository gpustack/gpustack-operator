# External DP Benchmark

On one eight-GPU H100 SXM5 node, wide *tensor parallelism (TP)* delivered **2.27×–6.99× higher
output throughput** than *External DP* across twelve matched workloads.
*External DP* had lower median first-token latency at concurrency 64 for both prompt shapes.
Both configurations used eight GPUs and completed **1,088/1,088 measured requests with zero errors**.

The model was `deepseek-ai/DeepSeek-V2-Lite-Chat`, a *mixture of experts (MoE)* model.
Wide *TP* kept all eight GPUs inside one `Pod` and used intra-`Pod` NVLink.
*External DP* used four two-GPU `Pod`s: local *TP* used NVLink, while cross-`Pod` expert collectives used TCP.
[External DP](external-dp.md) describes the serving contract.

The initial suites and conclusions describe cross-`Pod` TCP.
The separate [InfiniBand RDMA comparison](#infiniband-rdma-comparison) records the follow-up.

## Contents

- [Configuration](#configuration)
- [Hardware, model and workload](#hardware-model-and-workload)
- [Prompt lengths and concurrency](#prompt-lengths-and-concurrency)
- [Per-rank registration](#per-rank-registration)
- [Summary](#summary)
- [InfiniBand RDMA comparison](#infiniband-rdma-comparison)
- [Limitations](#limitations)

## Configuration

Each configuration used one `ModelDeployment` on the same GPU node, `gpu-node-0`.

| Configuration | Roles | `Pod`s | GPUs per `Pod` | Router |
|---|---|---:|---:|---|
| Wide *TP* | 1 *Server* | 1 | 8 | None; direct engine HTTP |
| *External DP* | 1 *Server* group | 4 | 2 | `vllm-router`, `round_robin` |

Wide *TP* set `--tensor-parallel-size 8` without `--enable-expert-parallel`.
Attention and expert layers used *TP* across the eight GPUs within that `Pod`.

*External DP* set *data parallelism (DP)* to 4, local *TP* to 2, and enabled *expert parallelism (EP)*.
Each rank exposed its own API server and used `--all2all-backend allgather_reducescatter`.
The four ranks shared an eight-GPU expert collective; each rank kept its own attention KV cache.
There was no cross-rank KV-block transfer.

**NVLink was used only inside a `Pod`.** Each `Pod` exposed only its assigned GPUs, with private
IPC and `/dev/shm`, *`hostIPC`* disabled and no *remote direct memory access (RDMA)* interface.
Cross-`Pod` *EP* communication, including `allgather_reducescatter`, used TCP sockets over `eth0`.
The host's NVLink fabric did not provide a cross-`Pod` transport in this recording.

## Hardware, model and workload

| Property | Captured value |
|---|---|
| Cluster | Kubernetes v1.35.7; one CPU node and one GPU node |
| Accelerators | 8 × NVIDIA H100 80 GB HBM3 SXM5 on one GPU node |
| Interconnect | NVLink within each `Pod`; TCP over `eth0` between `Pod`s |
| GPUStack Operator | v0.9.1 |
| Captured `InstanceType` | `bench-gpu`, selecting the H100 accelerator group |
| Runner | `gpustack/runner:cuda13.0-vllm0.30.0`; startup logs report vLLM 0.30.0 |
| Router | `gpustack/llm-router:v0.3.1`, `round_robin` |
| Model | `deepseek-ai/DeepSeek-V2-Lite-Chat`, revision `85864749cd611b4353ce1decdb286193298f64c7` |
| Model delivery | `ModelArtifact` `deepseek-v2-lite-chat`; node delivery, served as `/models` |
| Shared limits | `--max-model-len 8192`, `--max-num-seqs 16`, `--gpu-memory-utilization 0.90` |
| Wide parallelism | *TP* = 8; *EP* disabled |
| External parallelism | *TP* = 2, *DP* = 4, *EP* world size = 8 |
| External port setting | `VLLM_PORT=19500` |

### Port allocation

The captured deployment set `VLLM_PORT=19500` on every rank, and the successful run selected port
`19502` for collective initialization (an observed runtime choice rather than a fixed base-plus-two
offset). See [Port allocation](external-dp.md#port-allocation) for vLLM's port-probing,
candidate-broadcast and retry behavior, and the recorded bootstrap-port race that motivates a fixed
`VLLM_PORT`, and the [walkthrough manifest](../../walkthroughs/model-deployment/external-dp.md#step-1-start-the-fixed-group) for the setting.

### Workload

Prompt/output targets were `1024/256` and `4096/128`, at concurrency 1, 4, 8, 16, 32 and 64.
The request counts were 32, 32, 32, 64, 128 and 256 per shape, respectively.
Each cell had four excluded warm-up requests. One calibration request preceded each configuration.
Prompts used unique request markers; early stopping produced different output-token totals.

### Method

The streaming load generator ran inside the cluster and called the cluster-internal endpoint.
Each configuration ran once, sequentially on the same node with warmed model weights.
The 24 result files contain 12 cells per configuration and **2,176 measured requests in total**.
Output throughput uses actual generated tokens divided by each cell's wall time.

Latency columns report *time to first token (TTFT)*, *time per output token (TPOT)* and
*inter-token latency (ITL)*. *TPOT* is a per-request average; *ITL* measures streamed-token gaps.
Bold values mark lower latency or higher throughput within each matched workload and concurrency.
They do not establish statistical significance.

## Prompt lengths and concurrency

Every cell completed its requested count with zero errors.

| Workload | Prompt tokens | Concurrency | Configuration | Requests | *TTFT* p50 (ms) | *TTFT* p95 (ms) | *TPOT* p50 (ms) | *ITL* p99 (ms) | Output (tok/s) | Throughput (req/s) |
|---|---|---:|---|---:|---:|---:|---:|---:|---:|---:|
| `1024/256` | 1,023–1,027 | 1 | Wide *TP* | 32/32 | **58.302** | **61.457** | **2.340** | **2.609** | **383.386** | **1.796** |
| `1024/256` | 1,022–1,025 | 1 | *External DP* | 32/32 | 207.622 | 262.956 | 17.045 | 18.609 | 55.741 | 0.256 |
| `1024/256` | 1,023–1,027 | 4 | Wide *TP* | 32/32 | **107.743** | **125.242** | **3.160** | **3.418** | **1,089.625** | **4.490** |
| `1024/256` | 1,022–1,025 | 4 | *External DP* | 32/32 | 244.112 | 443.151 | 18.300 | 20.467 | 200.483 | 0.916 |
| `1024/256` | 1,023–1,026 | 8 | Wide *TP* | 32/32 | **138.536** | **192.776** | **3.554** | **5.566** | **1,786.289** | **7.514** |
| `1024/256` | 1,022–1,025 | 8 | *External DP* | 32/32 | 614.447 | 748.791 | 22.836 | 26.162 | 316.739 | 1.355 |
| `1024/256` | 1,023–1,027 | 16 | Wide *TP* | 64/64 | **115.262** | **192.078** | **5.267** | **53.111** | **2,647.004** | **11.643** |
| `1024/256` | 1,022–1,025 | 16 | *External DP* | 64/64 | 887.432 | 1,387.311 | 30.771 | 142.235 | 436.861 | 1.866 |
| `1024/256` | 1,023–1,027 | 32 | Wide *TP* | 128/128 | **1,403.260** | **1,689.522** | **5.799** | **53.587** | **2,637.925** | **11.829** |
| `1024/256` | 1,022–1,025 | 32 | *External DP* | 128/128 | 1,598.815 | 2,879.049 | 37.393 | 163.243 | 671.889 | 2.918 |
| `1024/256` | 1,023–1,027 | 64 | Wide *TP* | 256/256 | 4,798.411 | 5,215.017 | **6.705** | **53.361** | **2,314.155** | **10.107** |
| `1024/256` | 1,021–1,026 | 64 | *External DP* | 256/256 | **1,863.100** | **4,921.601** | 51.115 | 326.941 | 1,017.434 | 4.483 |
| `4096/128` | 4,096–4,098 | 1 | Wide *TP* | 32/32 | **68.253** | **71.414** | **2.456** | **2.698** | **330.439** | **2.812** |
| `4096/128` | 4,095–4,097 | 1 | *External DP* | 32/32 | 509.759 | 629.659 | 16.946 | 18.233 | 47.246 | 0.395 |
| `4096/128` | 4,096–4,099 | 4 | Wide *TP* | 32/32 | **75.354** | **180.279** | **4.325** | **55.155** | **787.884** | **6.872** |
| `4096/128` | 4,095–4,097 | 4 | *External DP* | 32/32 | 458.764 | 1,540.988 | 26.951 | 404.635 | 120.167 | 1.104 |
| `4096/128` | 4,095–4,098 | 8 | Wide *TP* | 32/32 | **139.801** | **300.532** | **4.806** | **55.997** | **1,249.468** | **10.236** |
| `4096/128` | 4,094–4,097 | 8 | *External DP* | 32/32 | 823.096 | 2,766.188 | 30.710 | 397.794 | 180.100 | 1.517 |
| `4096/128` | 4,096–4,099 | 16 | Wide *TP* | 64/64 | **126.666** | **458.393** | **9.262** | **56.268** | **1,378.472** | **12.351** |
| `4096/128` | 4,094–4,097 | 16 | *External DP* | 64/64 | 1,501.351 | 5,478.015 | 58.200 | 703.247 | 210.812 | 1.798 |
| `4096/128` | 4,095–4,099 | 32 | Wide *TP* | 128/128 | **1,320.714** | **1,543.407** | **9.783** | **56.110** | **1,518.784** | **12.875** |
| `4096/128` | 4,094–4,098 | 32 | *External DP* | 128/128 | 1,794.637 | 9,484.716 | 102.404 | 1,262.285 | 253.940 | 2.167 |
| `4096/128` | 4,094–4,099 | 64 | Wide *TP* | 256/256 | 3,814.139 | **4,057.352** | **10.136** | **55.619** | **1,491.536** | **12.761** |
| `4096/128` | 4,094–4,098 | 64 | *External DP* | 256/256 | **3,094.017** | 17,594.349 | 193.557 | 1,953.899 | 290.025 | 2.537 |

Wide *TP* delivered higher output throughput in all twelve matched cells.
Its peak was **2,647.004 tok/s** at `1024/256`, concurrency 16, against `436.861 tok/s` for *External DP*.
*External DP* peaked at **1,017.434 tok/s** at `1024/256`, concurrency 64.

At concurrency 1, *External DP* had **6.9×–7.3× higher TPOT p50** across the two shapes.
The configurations differ in expert sharding, transport, router path and API-server count.
The measurements do not isolate the contribution of each difference.

At `1024/256`, concurrency 64, *External DP* reduced *TTFT* p50 from `4,798.411 ms` to
**1,863.100 ms**, or **2.58× lower**. Its *TTFT* p95 was also lower, `4,921.601 ms` against `5,215.017 ms`.
Wide *TP* still delivered **2.27× higher output throughput**, with lower *TPOT* and *ITL* tails.

At `4096/128`, concurrency 64, *External DP* reduced median *TTFT* from `3,814.139 ms` to
**3,094.017 ms**, or **1.23× lower**.
Its tail worsened: *TTFT* p95 reached **17,594.349 ms**, and *ITL* p99 reached **1,953.899 ms**.
Wide *TP* recorded `4,057.352 ms` and `55.619 ms`, respectively.
A lower first-token median did not imply better generation latency or throughput.

## Per-rank registration

The final capture combines operator status, the Router registry, per-worker counters and engine access logs.
The role reported one ready replica group with four confirmed serving endpoints:

```json
{
  "name": "server",
  "kind": "Server",
  "desired": 1,
  "ready": 1,
  "parallelism": {
    "declared": { "dataParallel": 4, "tensorParallel": 2 },
    "loadBalance": "External",
    "modes": { "expertParallel": true },
    "source": { "complete": true, "kind": "ExtraArgs" }
  },
  "endpoints": {
    "eligible": 1,
    "serving": { "state": "Confirmed", "value": 4 }
  }
}
```

*`eligible`* counts the replica group. *`serving.value`* counts the HTTP endpoints confirmed by the Router.
All four endpoints were registered, healthy, in selection and had closed circuits.
The rank names below replace endpoint IPs with their corresponding member identities.

| Rank | Registered | Healthy | In selection | Router requests | Engine completion lines |
|---|---|---|---|---:|---:|
| `server-r0-m0` | true | true | true | **285** | **285** |
| `server-r0-m1` | true | true | true | **284** | **284** |
| `server-r0-m2` | true | true | true | **284** | **284** |
| `server-r0-m3` | true | true | true | **284** | **284** |

The counters total **1,137 requests**: 1,088 measured requests, 48 warm-ups and one calibration request.
The wide *TP* engine also logged 1,137 completion lines.
Router counters matched every rank's engine completion count, establishing traffic on all four ranks.
The generic router-access-line capture reported zero; it is separate from these nonzero per-worker counters.

## Summary

### Benefits

- *External DP* registered **four healthy serving endpoints** and spread requests evenly: **285 + 284 × 3**.
- At `1024/256`, concurrency 64, *External DP* had **2.58× lower TTFT p50** than wide *TP*.
- At `4096/128`, concurrency 64, its *TTFT* p50 was **1.23× lower**, despite worse tail latency.
- Both configurations completed **1,088/1,088 measured requests**, with zero errors.

### Trade-offs

- Wide *TP* delivered **2.27×–6.99× higher output throughput** across all matched cells.
- Cross-`Pod` *EP* used TCP, while the wide configuration kept its full collective inside one NVLink-connected `Pod`.
- At `4096/128`, concurrency 64, *External DP* had lower median *TTFT* but much higher first-token and token-gap tails.

Choose the fixed four-rank group when per-rank ingress or lower median *TTFT* at concurrency 64 fits the workload.
Choose wide *TP* when output throughput and generation latency dominate on this single-node NVLink setup.
The crossover does not establish a general advantage for multi-node or *RDMA* deployments.

## InfiniBand RDMA comparison

A follow-up on the same ConnectX-7 InfiniBand node setup described in
[Prefill and Decode Benchmark](prefill-decode-benchmark.md#infiniband-rdma-comparison) retained
*DP* = 4, *TP* = 2 per `Pod`, one API server per rank and `round_robin` routing. All twelve measured
cells completed **1,088/1,088 requests with zero errors**.

### Transport verification

Update `external-dp.yaml` from the
[TCP walkthrough](../../walkthroughs/model-deployment/external-dp.md#step-1-start-the-fixed-group):
The diff uses current interface allocation; the recorded run used manual device exposure.

```diff
 spec:
   roles:
     - name: server
       kind: Server
       size: 4
       resources:
         accelerator: "2"
+        interface: "2"
```

NCCL logged `Using network IB` and channel transfers `via NET/IB/.../GDRDMA`.
This establishes InfiniBand transport for the cross-`Pod` expert collective.
Intra-`Pod` *TP* still used NVLink; HTTP and bootstrap traffic remained separate from collective transport.

The model, engine limits and collective backend matched the TCP run.
Keep **`VLLM_PORT=19500`** on every rank; InfiniBand does not fix the [bootstrap-port race](#port-allocation).

### TCP and RDMA

Bold values mark lower latency or higher throughput within each matched *External DP* pair.
The target shapes, concurrencies and measured request counts matched the TCP sweep.

| Shape | Concurrency | TCP *TTFT* p50 (ms) | RDMA *TTFT* p50 (ms) | TCP *ITL* p99 (ms) | RDMA *ITL* p99 (ms) | TCP output (tok/s) | RDMA output (tok/s) |
|---|---:|---:|---:|---:|---:|---:|---:|
| `1024/256` | 1 | 207.622 | **86.763** | 18.609 | **7.425** | 55.741 | **138.181** |
| `1024/256` | 4 | 244.112 | **90.480** | 20.467 | **11.774** | 200.483 | **476.304** |
| `1024/256` | 8 | 614.447 | **146.641** | **26.162** | 64.911 | 316.739 | **833.031** |
| `1024/256` | 16 | 887.432 | **160.823** | 142.235 | **66.483** | 436.861 | **1,451.568** |
| `1024/256` | 32 | 1,598.815 | **206.110** | 163.243 | **73.343** | 671.889 | **2,147.777** |
| `1024/256` | 64 | 1,863.100 | **297.546** | 326.941 | **74.152** | 1,017.434 | **3,921.562** |
| `4096/128` | 1 | 509.759 | **102.983** | 18.233 | **7.510** | 47.246 | **129.502** |
| `4096/128` | 4 | 458.764 | **103.246** | 404.635 | **65.134** | 120.167 | **404.577** |
| `4096/128` | 8 | 823.096 | **136.206** | 397.794 | **70.939** | 180.100 | **704.793** |
| `4096/128` | 16 | 1,501.351 | **160.059** | 703.247 | **78.096** | 210.812 | **1,000.937** |
| `4096/128` | 32 | 1,794.637 | **205.085** | 1,262.285 | **144.559** | 253.940 | **1,448.982** |
| `4096/128` | 64 | 3,094.017 | **365.352** | 1,953.899 | **182.693** | 290.025 | **2,033.762** |

At `1024/256`, concurrency 64, median *TTFT* fell **6.26×**, with **3.85× higher output throughput** than TCP.
At `4096/128`, concurrency 64, median *TTFT* fell **8.47×**, with **7.01× higher output throughput**.
The long-prompt *TTFT* p95 fell from **17,594.349 to 1,477.799 ms**.
Its *ITL* p99 fell from **1,953.899 to 182.693 ms**, reducing the severe TCP generation stalls.

### Wide TP comparison

At concurrency 64, RDMA *External DP* exceeded the recorded wide-*TP* output throughput for both shapes.
Wide *TP* kept all eight GPUs inside one NVLink-connected `Pod` and was not rerun for this follow-up.
Both configurations allocated eight GPUs; the measurements are separate runs.

| Shape, concurrency 64 | Wide *TTFT* p50 (ms) | RDMA *TTFT* p50 (ms) | Wide *ITL* p99 (ms) | RDMA *ITL* p99 (ms) | Wide output (tok/s) | RDMA output (tok/s) |
|---|---:|---:|---:|---:|---:|---:|
| `1024/256` | 4,798.411 | **297.546** | **53.361** | 74.152 | 2,314.155 | **3,921.562** |
| `4096/128` | 3,814.139 | **365.352** | **55.619** | 182.693 | 1,491.536 | **2,033.762** |

RDMA *External DP* had **16.13× lower median TTFT** and **1.69× higher output throughput** at `1024/256`, concurrency 64.
At `4096/128`, the corresponding gains were **10.44× lower median TTFT** and **1.36× higher output throughput**.
This reverses the TCP throughput disadvantage at the highest tested concurrency.

### Routing verification

The captured role status confirmed **four serving endpoints** for one replica group.
Engine completion counts were **285 + 284 + 284 + 284 = 1,137** across the four ranks.
These include 1,088 measured requests, 48 warm-ups and one calibration request.
The counts establish traffic on every rank; the generic Router access-line capture is a separate counter.

### Remaining trade-offs

Wide *TP* still had higher output throughput in **ten of twelve** matched cells, covering concurrencies 1–32.
It retained lower *TPOT* p50 and *ITL* p99 in all twelve cells.
At `4096/128`, concurrency 64, RDMA *External DP* still had **182.693 ms ITL p99**, versus **55.619 ms** for wide *TP*.

RDMA also regressed one TCP tail metric: at `1024/256`, concurrency 8, *ITL* p99 rose from **26.162 to 64.911 ms**.
The faster first-token median and higher aggregate throughput do not establish uniformly smoother token generation.
No repeated trials isolate the cause of that tail difference.

Choose RDMA *External DP* when high-concurrency ingress and first-token latency justify a distributed expert collective.
Wide *TP* remains the measured choice for lower-concurrency throughput and token-generation latency on this single-node setup.
Cross-node behavior, other models and other collective backends remain unmeasured.

## Limitations

- One run per configuration, with 32–256 requests per cell. There are no repeated-run confidence intervals.
- Both configurations used eight GPUs, but batching limits were per engine. Four rank API servers differ from one wide API server.
- Parallelism, transport and routing changed together. No isolated collective or Router-overhead measurement was captured.
- The initial cross-`Pod` *EP* capture used TCP over `eth0`; the follow-up verified InfiniBand collective transport.
  Cross-`Pod` NVLink was not measured, and the separate runs do not isolate transport from run-to-run variation.
- All ranks shared one GPU node. Cross-node placement, other *DP* widths and online resizing were not measured.
- Only one *MoE* model and two prompt/output targets were measured. Dense-model behavior cannot be inferred from these results.
- Prompt markers and early stopping produced small prompt-length differences and different generated-token totals.
- Direct nonstreaming probes to each rank and a final released-device snapshot were not part of the TCP capture.

---

**See also** — [External DP](external-dp.md) (fixed-group contract) ·
[External DP Walkthrough](../../walkthroughs/model-deployment/external-dp.md) (manifest and endpoint checks) ·
[Model Deployment Routing](routing.md) (router policies and discovery) ·
[Elastic EP Benchmark](elastic-ep-benchmark.md) (live collective expansion)

**Next** → [Model Deployment Status](status.md)
