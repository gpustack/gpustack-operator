# External DP Benchmark

On one eight-GPU H100 SXM5 node, wide *tensor parallelism (TP)* delivered **2.27×–6.99× higher
output throughput** than *External DP* over TCP across all twelve matched cells. *External DP* had
lower median first-token latency at concurrency 64 on both shapes, while both eight-GPU
configurations completed **1,088/1,088 measured requests with zero errors**.

Both ran `deepseek-ai/DeepSeek-V2-Lite-Chat`, a *mixture of experts (MoE)* model. Wide *TP* kept
all eight GPUs inside one NVLink `Pod`; *External DP* ran four two-GPU `Pod`s with NVLink for local
*TP* and TCP for the cross-`Pod` expert collective. [External DP](/gpustack-operator/v0.12.1/docs/modules/model-deployment/external-dp/index.md) defines the
serving contract.

The initial suites describe cross-`Pod` TCP. The separate
[InfiniBand RDMA comparison](#infiniband-rdma-comparison) records the matched follow-up.

## Contents

- [Summary](#summary)
- [Environment](#environment)
- [Results](#results)
- [InfiniBand RDMA comparison](#infiniband-rdma-comparison)
- [Limitations](#limitations)

## Summary

- **Choose wide *TP*** on single-node NVLink when throughput and token-generation latency dominate.
- **Choose *External DP*** when per-rank HTTP ingress or concurrency-64 first-token latency fits the
  workload (especially with InfiniBand RDMA, where it also led concurrency-64 output).

### Benefits

- Registered **four healthy endpoints** and balanced traffic evenly (**285 + 284 × 3**).
- Lowered concurrency-64 *TTFT* p50 by **2.58×** at `1024/256` and **1.23×** at `4096/128`.
- Completed **1,088/1,088 measured requests** in both configurations with zero errors.

### Trade-offs

- Over TCP, wide *TP* delivered **2.27×–6.99× higher output throughput** in all twelve cells.
- Cross-`Pod` *EP* used TCP versus single-`Pod` NVLink for wide *TP*.
- At `4096/128`, concurrency 64 over TCP, *External DP* suffered severe *TTFT* p95 and *ITL* p99
  tails.

## Environment

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

### Configuration

Each configuration ran one `ModelDeployment` on `gpu-node-0`.

| Configuration | Roles | `Pod`s | GPUs per `Pod` | Router |
|---|---|---:|---:|---|
| Wide *TP* | 1 *Server* | 1 | 8 | None; direct engine HTTP |
| *External DP* | 1 *Server* group | 4 | 2 | `vllm-router`, `round_robin` |

Wide *TP* set `--tensor-parallel-size 8` without `--enable-expert-parallel`. *External DP* set
*data parallelism (DP)* = 4, local *TP* = 2 and *expert parallelism (EP)* enabled
(`--all2all-backend allgather_reducescatter`), with one API server and local attention KV cache per
rank and no cross-rank KV transfer.

**NVLink carried intra-`Pod` traffic only.** Each `Pod` had private IPC and `/dev/shm`, *`hostIPC`*
disabled and no *remote direct memory access (RDMA)* interface, so cross-`Pod` *EP* communication
used TCP sockets over `eth0`.

### Port allocation

Every *External DP* rank set `VLLM_PORT=19500`, and the run initialized on port `19502` (an
observed choice, not a fixed offset). See [Port allocation](/gpustack-operator/v0.12.1/docs/modules/model-deployment/external-dp/index.md#port-allocation) for
vLLM port probing and the bootstrap-port race, and the
[walkthrough manifest](/gpustack-operator/v0.12.1/docs/walkthroughs/model-deployment/external-dp/index.md#step-1-deploy-rank-group).

### Workload

Target prompt/output shapes were `1024/256` and `4096/128` at concurrency 1, 4, 8, 16, 32 and 64
(32, 32, 32, 64, 128 and 256 measured requests per shape, plus four excluded warm-ups per cell and
one calibration request per configuration). Prompts carried unique markers, and early stopping
varied output-token totals.

### Measurement method

The in-cluster streaming generator ran each configuration once, sequentially, with warmed weights
(**2,176 measured requests** across 24 result files). Output throughput divides actual generated
tokens by cell wall time.

*Time to first token (TTFT)*, *time per output token (TPOT)* and *inter-token latency (ITL)* use
captured millisecond precision. Bold values mark lower latency or higher throughput within each
matched pair without claiming statistical significance.

## Results

### Latency and throughput

Every cell completed with **zero errors**.

![TTFT p50, ITL p99 and output throughput by concurrency for Wide TP and External DP over TCP, two prompt shapes](/gpustack-operator/v0.12.1/docs/assets/diagrams/external-dp-benchmark-tcp.svg)

Over TCP against single-`Pod` NVLink wide *TP*, wide *TP* won output throughput in all 12 cells by
**2.27×–6.99×** (peaking at **2,647.004 tok/s** versus 436.861 tok/s at `1024/256`, concurrency 16;
*External DP* peaked at **1,017.434 tok/s** at concurrency 64) and kept **6.9×–7.3×** lower *TPOT*
p50 at concurrency 1.

*External DP* beat wide *TP* on *TTFT* p50 only at concurrency 64 (**2.58×** lower at `1024/256`:
**1,863.100** versus 4,798.411 ms; **1.23×** lower at `4096/128`: **3,094.017** versus
3,814.139 ms), where `4096/128` tails degraded (*TTFT* p95 **17,594.349** versus 4,057.352 ms;
*ITL* p99 **1,953.899** versus 55.619 ms). Parallelism, transport, routing and API-server count
changed together, so individual causes are not isolated.

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

### Per-rank registration

Operator status confirmed one ready replica group (`eligible: 1`) and four serving HTTP endpoints
(`serving.value: 4`):

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

All four endpoints were registered, healthy, in selection and had closed circuits (rank names
replace endpoint IPs):

| Rank | Registered | Healthy | In selection | Router requests | Engine completion lines |
|---|---|---|---|---:|---:|
| `server-r0-m0` | true | true | true | **285** | **285** |
| `server-r0-m1` | true | true | true | **284** | **284** |
| `server-r0-m2` | true | true | true | **284** | **284** |
| `server-r0-m3` | true | true | true | **284** | **284** |

Totals match **1,137 requests** (1,088 measured, 48 warm-ups, one calibration), matching wide *TP*'s
1,137 engine completions. Generic router-access-line counts read zero separately from these
per-worker counters.

## InfiniBand RDMA comparison

A follow-up on the ConnectX-7 InfiniBand node from
[Prefill and Decode Benchmark](/gpustack-operator/v0.12.1/docs/modules/model-deployment/prefill-decode-benchmark/index.md#infiniband-rdma-comparison) kept
*DP* = 4, *TP* = 2 per `Pod`, one API server per rank and `round_robin` routing. All twelve cells
completed **1,088/1,088 requests with zero errors**.

### Transport verification

Update `external-dp.yaml` from the
[TCP walkthrough](/gpustack-operator/v0.12.1/docs/walkthroughs/model-deployment/external-dp/index.md#step-1-deploy-rank-group)
(the diff shows current interface allocation; the capture used manual device exposure):

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: ModelDeployment
 metadata:
   name: external-dp-arm2-external-dp
   namespace: gpustack-bench
 spec:
   roles:
     - name: server
       kind: Server
       size: 4
       resources:
         accelerator: "2"
+        interface: "2"
```

NCCL logged `Using network IB` and `via NET/IB/.../GDRDMA`, confirming InfiniBand for the cross-`Pod`
expert collective while intra-`Pod` *TP* stayed on NVLink. Keep **`VLLM_PORT=19500`** on every rank;
InfiniBand does not remove the [bootstrap-port race](#port-allocation).

### Transport performance

![TTFT p50, ITL p99 and output throughput by concurrency for Wide TP, External DP over TCP and External DP over InfiniBand RDMA](/gpustack-operator/v0.12.1/docs/assets/diagrams/external-dp-benchmark-rdma.svg)

In this separate InfiniBand follow-up (where wide *TP* was not rerun), RDMA *External DP* cut
concurrency-64 *TTFT* p50 by **6.26×** (`1024/256`) and **8.47×** (`4096/128`) over TCP, raised
output **3.85×** and **7.01×**, and cut `4096/128` tails (*TTFT* p95 **17,594.349 → 1,477.799 ms**;
*ITL* p99 **1,953.899 → 182.693 ms**).

Exceptions remain: wide *TP* kept higher output at concurrencies 1–32 (10 of 12 cells) and lower
*TPOT* p50 and *ITL* p99 in all 12 cells, and RDMA regressed *ITL* p99 versus TCP at `1024/256`,
concurrency 8 (**26.162** to 64.911 ms).

Bold values mark lower latency or higher throughput within each matched *External DP* TCP/RDMA pair.

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

### Wide tensor parallelism

At concurrency 64 (separate runs, eight GPUs each), RDMA *External DP* beat the recorded wide-*TP*
output throughput on both shapes (**1.69×** at `1024/256` and **1.36×** at `4096/128`) and cut
*TTFT* p50 **16.13×** and **10.44×**. Wide *TP* still kept lower *ITL* p99 in both cells.

| Shape, concurrency 64 | Wide *TTFT* p50 (ms) | RDMA *TTFT* p50 (ms) | Wide *ITL* p99 (ms) | RDMA *ITL* p99 (ms) | Wide output (tok/s) | RDMA output (tok/s) |
|---|---:|---:|---:|---:|---:|---:|
| `1024/256` | 4,798.411 | **297.546** | **53.361** | 74.152 | 2,314.155 | **3,921.562** |
| `4096/128` | 3,814.139 | **365.352** | **55.619** | 182.693 | 1,491.536 | **2,033.762** |

### Routing verification

Role status confirmed **four serving endpoints** for one replica group. Engine completion counts
matched **285 + 284 + 284 + 284 = 1,137** (1,088 measured, 48 warm-ups, one calibration), proving
traffic reached every rank.

### RDMA trade-offs

- **Choose RDMA *External DP*** when high-concurrency ingress (concurrency 64) and first-token
  latency outweigh smoother token generation.
- **Choose wide *TP*** on single-node NVLink for concurrencies 1–32 throughput and the lowest *TPOT*
  p50 and *ITL* p99 across every concurrency (including 55.619 versus 182.693 ms at `4096/128`,
  concurrency 64).

No repeated trials isolate the `1024/256`, concurrency-8 RDMA *ITL* p99 regression (64.911 versus
26.162 ms TCP). Cross-node behavior, other models and other backends remain unmeasured.

## Limitations

- One run per configuration (32–256 requests per cell) provides no repeated-run confidence intervals.
- Batching limits were per engine: four rank API servers differ from one wide API server.
- Parallelism, transport and routing changed together; no single overhead term was isolated.
- Neither capture measures cross-`Pod` NVLink, and separate runs do not isolate transport from
  run-to-run variation.
- All ranks shared one GPU node; cross-node placement, other *DP* widths and online resizing were
  not measured.
- Only one *MoE* model and two prompt shapes ran; dense-model behavior cannot be inferred.
- Unique prompt markers and early stopping varied prompt and generated-token totals slightly.
- Direct nonstreaming rank probes and a released-device snapshot were omitted from the TCP capture.

---

**See also** — [External DP](/gpustack-operator/v0.12.1/docs/modules/model-deployment/external-dp/index.md) (fixed-group contract) ·
[External DP Walkthrough](/gpustack-operator/v0.12.1/docs/walkthroughs/model-deployment/external-dp/index.md) (manifest and endpoint checks) ·
[Model Deployment Routing](/gpustack-operator/v0.12.1/docs/modules/model-deployment/routing/index.md) (router policies and discovery) ·
[Elastic EP Benchmark](/gpustack-operator/v0.12.1/docs/modules/model-deployment/elastic-ep-benchmark/index.md) (live collective expansion)

**Next** → [Model Deployment Scaling](/gpustack-operator/v0.12.1/docs/modules/model-deployment/scaling/index.md) — scale serving replicas from live engine signals.
