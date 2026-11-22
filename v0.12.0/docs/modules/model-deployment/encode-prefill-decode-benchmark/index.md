# Encode, Prefill and Decode Benchmark

On one H100 SXM5 node, *encode/prefill/decode (EPD)* cut long-prompt generation stalls and
repeated-image latency relative to aggregated serving and *prefill/decode (PD)*. Aggregated serving,
*PD* and *EPD* used **one, two and three GPUs**, respectively, so these results compare unequal GPU
budgets.

At `8k` and concurrency 8 over TCP, *EPD* cut *inter-token latency (ITL)* p99 by **21.8×**
(296.710 ms to **13.607 ms**) while raising *time to first token (TTFT)* p50 (3,963.085 to
4,182.324 ms). [Encode, Prefill and Decode](/gpustack-operator/v0.12.0/docs/modules/model-deployment/encode-prefill-decode/index.md) defines the role contract.

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

- **Choose *EPD*** when three GPUs are available and large-image generation isolation,
  repeated-image reuse or mixed vision/decode latency matters most.
- **Choose aggregated** when one GPU or small-prompt first-token latency matters most; cross-role
  TCP transfers and unequal GPU budgets prevent an equal-cost claim.

### Benefits

- Long-prompt *ITL* p99 fell **15.1×–21.8×** at `4k` and `8k`, concurrency 8, against aggregated
  serving.
- Repeated-image traffic reached **536.280 tok/s** at concurrency 8 with **5.9× lower *TTFT* p50**.
- Mixed traffic finished in **5.217 s** versus 8.473 s for aggregated serving, with lower *TTFT*
  and *ITL* tails.

### Trade-offs

- *EPD* allocated **three GPUs**, versus one for aggregated serving and two for *PD*.
- Small prompts (`1k`) favored aggregated serving on first-token latency in every cell.
- Over TCP at `8k`, concurrency 8, *EPD* *TTFT* p50 stayed higher than aggregated serving despite
  much lower *ITL* p99.

## Environment

| Property | Captured value |
|---|---|
| Cluster | Kubernetes v1.35.7; one CPU node and one GPU node |
| Hardware | 8 × NVIDIA H100 80 GB HBM3 SXM5 on one node |
| GPUStack Operator | v0.9.1 |
| Captured `InstanceType` | `bench-gpu`, selecting the H100 accelerator group |
| Runner | `gpustack/runner:cuda13.0-vllm0.30.0` |
| Router | `gpustack/llm-router:v0.3.1`; disaggregation sidecar v0.11.0 |
| Model | `Qwen/Qwen2.5-VL-7B-Instruct`, revision `cc594898137f460bfe9f0759e9844b3ce807cfb5` |
| Model delivery | `ModelArtifact` `qwen2-5-vl-7b-instruct`; node delivery mount `/var/lib/gpustack/model`, served as `/models` |
| Engine limits | `--max-model-len 16384`, `--gpu-memory-utilization 0.85` |
| Parallelism | *Tensor parallelism (TP)* = 1 per role |

The operator added `--enforce-eager` and `--no-enable-prefix-caching` only to *Encode*. *Prefill*
and *Decode* retained CUDA graphs. Aggregated serving and *PD* set `--enable-scale-out` for router
token rendering; *EPD* did not.

### Configuration

Each configuration ran one `ModelDeployment` on `gpu-node-0` with one replica per role and one GPU
per `Pod`.

| Configuration | Roles | GPUs | KV connector | Encoder connector |
|---|---|---:|---|---|
| Aggregated | 1 *Server* | 1 | None | None |
| *PD* | 1 *Prefill* + 1 *Decode* | 2 | `MooncakeConnector`, TCP | None |
| *EPD* | 1 *Encode* + 1 *Prefill* + 1 *Decode* | 3 | `MooncakeConnector`, TCP | `ECCPUConnector`, NIXL |

Each `Pod` exposed only its assigned GPU with a private `/dev/shm` and IPC namespace. *`hostIPC`*
was disabled, and no *remote direct memory access (RDMA)* interface was attached. **All cross-`Pod`
NIXL encoder-cache and Mooncake KV transfers used TCP sockets over `eth0`.** Host NVLink carried no
cross-role traffic.

### Variable image sizes

Suite 1 varied image and text length with a `128`-token completion limit. Image dimensions are
pixels; text-token values are generator targets. Measured prompts include multimodal formatting.

| Context | Image | Text target | Measured prompt tokens |
|---|---|---:|---|
| `1k` | 560 × 560 | 624 | 1,047–1,050 |
| `2k` | 840 × 840 | 1,148 | 2,071–2,074 |
| `4k` | 1260 × 1260 | 2,071 | 4,119–4,122 |
| `8k` | 1792 × 1792 | 4,096 | 8,215–8,219 |

Concurrency 1, 4 and 8 used 8, 12 and 24 measured requests per context, respectively.

### Concurrent vision and decode

Two streams ran concurrently, with four workers and twelve requests each. The small stream used
unique `560 × 560` images, a `624`-token text target and `max_tokens=256`. The heavy-vision stream
used unique `1792 × 1792` images, a `400`-token text target and `max_tokens=64`. Measured prompt
lengths were 1,048–1,051 and 4,520–4,523 tokens, respectively.

### Repeated images

Suite 3 reused one `1792 × 1792` image with a different question per request. It used a `400`-token
text target and `max_tokens=64`; measured prompts held 4,518–4,522 tokens. Concurrency 4 and 8 used
12 and 24 measured requests.

### Measurement method

The in-cluster load generator ran each configuration once, sequentially, on the same node. Two
warm-up requests preceded every Suite 1 and Suite 3 cell and were excluded from latency summaries.
The 45 result files hold 36 Suite 1 cells, three mixed-load runs and six repeated-image cells.

**All 708 measured requests succeeded with zero errors**, 236 per configuration, plus 28 warm-up
requests each. *Time per output token (TPOT)* measures per-request generation time per token; *ITL*
measures gaps between streamed tokens. Bold values mark lower latency or higher throughput within
each matched workload and concurrency.

## Results

### Suite 1: Multimodal context scaling

![ITL p99, TTFT p50 and output throughput by concurrency for aggregated serving, PD and EPD over TCP, four multimodal contexts](/gpustack-operator/v0.12.0/docs/assets/diagrams/encode-prefill-decode-benchmark-tcp.svg)

Over TCP with unequal GPU budgets (1, 2 and 3 GPUs), *EPD* cut *ITL* p99 by **15.1×–21.8×** against
aggregated serving at `4k` and `8k`, concurrency 8 (231.453 to **15.352 ms**; 296.710 to
**13.607 ms**).

*EPD* has no universal latency win: 1-GPU aggregated serving kept lower *TTFT* p50 in every `1k`
cell, lower *TTFT* p95 in every `2k` cell, and lower *TTFT* p50 at `8k`, concurrency 8 (3,963.085
versus 4,182.324 ms), while `8k`, concurrency 4 lowered *EPD* median *TTFT* but worsened *TTFT* p95.

| Workload | Concurrency | Configuration | Requests | *TTFT* p50 (ms) | *TTFT* p95 (ms) | *TPOT* p50 (ms) | *ITL* p99 (ms) | Output (tok/s) | Throughput (req/s) |
|---|---:|---|---:|---:|---:|---:|---:|---:|---:|
| `1k` | 1 | Aggregated | 8/8 | **127.488** | **133.658** | **5.904** | **8.885** | **143.767** | 1.234 |
| `1k` | 1 | *PD* | 8/8 | 193.346 | 202.953 | 6.014 | 9.089 | 130.569 | 1.140 |
| `1k` | 1 | *EPD* | 8/8 | 157.290 | 172.886 | 5.926 | 9.168 | 135.864 | **1.259** |
| `1k` | 4 | Aggregated | 12/12 | **132.585** | **317.836** | 6.810 | 40.568 | **429.372** | **4.179** |
| `1k` | 4 | *PD* | 12/12 | 352.986 | 514.518 | **6.302** | **12.260** | 371.169 | 3.924 |
| `1k` | 4 | *EPD* | 12/12 | 234.481 | 435.045 | 6.423 | 13.121 | 420.666 | 3.925 |
| `1k` | 8 | Aggregated | 24/24 | **294.960** | **574.336** | 7.359 | 43.814 | **744.704** | **6.722** |
| `1k` | 8 | *PD* | 24/24 | 397.371 | 929.204 | **6.956** | 16.274 | 646.274 | 5.925 |
| `1k` | 8 | *EPD* | 24/24 | 343.176 | 725.719 | 6.987 | **14.933** | 715.225 | 6.592 |
| `2k` | 1 | Aggregated | 8/8 | **221.341** | **228.913** | **5.922** | **8.879** | **126.909** | **1.146** |
| `2k` | 1 | *PD* | 8/8 | 323.722 | 333.260 | 6.022 | 8.968 | 109.430 | 1.068 |
| `2k` | 1 | *EPD* | 8/8 | 261.095 | 266.917 | 5.935 | 9.255 | 120.054 | 1.125 |
| `2k` | 4 | Aggregated | 12/12 | **393.122** | **584.277** | 6.611 | 58.282 | **385.090** | **3.245** |
| `2k` | 4 | *PD* | 12/12 | 583.651 | 1,008.471 | 6.589 | 12.813 | 300.633 | 2.752 |
| `2k` | 4 | *EPD* | 12/12 | 581.520 | 794.935 | **6.365** | **12.136** | 326.686 | 3.025 |
| `2k` | 8 | Aggregated | 24/24 | 772.251 | **1,137.334** | 7.751 | 72.554 | 528.024 | 4.771 |
| `2k` | 8 | *PD* | 24/24 | 1,056.469 | 2,005.102 | **7.622** | 15.551 | 446.540 | 3.887 |
| `2k` | 8 | *EPD* | 24/24 | **613.996** | 1,230.158 | 7.714 | **15.438** | **537.856** | **5.092** |
| `4k` | 1 | Aggregated | 8/8 | 442.405 | **448.422** | **5.956** | **8.646** | **94.195** | 1.006 |
| `4k` | 1 | *PD* | 8/8 | 633.100 | 649.480 | 6.054 | 9.639 | 68.685 | 0.931 |
| `4k` | 1 | *EPD* | 8/8 | **440.770** | 451.024 | 5.959 | 9.268 | 88.808 | **1.080** |
| `4k` | 4 | Aggregated | 12/12 | 1,151.042 | **1,229.450** | **6.301** | **10.512** | 179.650 | 2.255 |
| `4k` | 4 | *PD* | 12/12 | 1,504.409 | 2,001.824 | 7.141 | 13.195 | 164.679 | 1.685 |
| `4k` | 4 | *EPD* | 12/12 | **808.650** | 1,430.554 | 6.994 | 13.602 | **204.756** | **2.363** |
| `4k` | 8 | Aggregated | 24/24 | 1,972.504 | **2,403.576** | 9.298 | 231.453 | 231.845 | 2.785 |
| `4k` | 8 | *PD* | 24/24 | 2,055.500 | 4,379.420 | 8.578 | 15.481 | 216.193 | 2.333 |
| `4k` | 8 | *EPD* | 24/24 | **1,690.166** | 2,715.341 | **8.469** | **15.352** | **255.080** | **2.985** |
| `8k` | 1 | Aggregated | 8/8 | 868.512 | 893.580 | **6.039** | **8.905** | 64.577 | 0.705 |
| `8k` | 1 | *PD* | 8/8 | 1,226.636 | 1,266.734 | 6.130 | 9.598 | 50.824 | 0.565 |
| `8k` | 1 | *EPD* | 8/8 | **798.743** | **805.795** | 6.052 | 9.204 | **68.562** | **0.739** |
| `8k` | 4 | Aggregated | 12/12 | 2,129.912 | **2,439.747** | 7.527 | 247.270 | **136.270** | 1.289 |
| `8k` | 4 | *PD* | 12/12 | 2,867.606 | 4,361.361 | **6.991** | **12.842** | 104.379 | 1.004 |
| `8k` | 4 | *EPD* | 12/12 | **1,572.574** | 3,156.539 | 8.386 | 15.283 | 134.224 | **1.393** |
| `8k` | 8 | Aggregated | 24/24 | **3,963.085** | **4,794.768** | 16.081 | 296.710 | 143.211 | **1.460** |
| `8k` | 8 | *PD* | 24/24 | 4,895.645 | 8,582.882 | **8.180** | 13.822 | 105.850 | 1.162 |
| `8k` | 8 | *EPD* | 24/24 | 4,182.324 | 5,839.159 | 8.357 | **13.607** | **144.753** | 1.434 |

### Suite 2: Mixed vision and decode

The mixed windows lasted 8.473 s (aggregated), 9.608 s (*PD*) and **5.217 s** (*EPD*). Each
stream's throughput divides by its configuration's full mixed window; combined output throughput was
267.093 tok/s, 238.130 tok/s and **388.320 tok/s**. Against 1-GPU aggregated serving, 3-GPU *EPD*
cut small-stream *ITL* p99 by **18.4×** (296.566 to **16.130 ms**) and lowered *TTFT* p50 on both
streams (1,214.046 to **647.387 ms**; 2,215.441 to **959.134 ms**).

| Workload | Concurrency | Configuration | Requests | *TTFT* p50 (ms) | *TTFT* p95 (ms) | *TPOT* p50 (ms) | *ITL* p99 (ms) | Output (tok/s) | Throughput (req/s) |
|---|---:|---|---:|---:|---:|---:|---:|---:|---:|
| `concurrent_decode_1k` | 4 | Aggregated | 12/12 | 1,214.046 | 1,573.818 | 12.417 | 296.566 | 176.449 | 1.416 |
| `concurrent_decode_1k` | 4 | *PD* | 12/12 | 1,487.862 | 1,828.701 | **6.432** | 16.708 | 158.198 | 1.249 |
| `concurrent_decode_1k` | 4 | *EPD* | 12/12 | **647.387** | **1,001.308** | 8.551 | **16.130** | **241.119** | **2.300** |
| `heavy_vision_4k` | 4 | Aggregated | 12/12 | 2,215.441 | 2,419.269 | **6.597** | 197.815 | 90.644 | 1.416 |
| `heavy_vision_4k` | 4 | *PD* | 12/12 | 2,379.965 | 3,770.445 | 7.138 | **16.348** | 79.932 | 1.249 |
| `heavy_vision_4k` | 4 | *EPD* | 12/12 | **959.134** | **1,971.796** | 8.625 | 16.845 | **147.201** | **2.300** |

### Suite 3: Encoder-cache reuse

At concurrency 8 on repeated `1792 × 1792` images, *EPD* cut *TTFT* p50 **5.9×** (2,196.771 to
**371.850 ms**) and raised output throughput **2.86×** (187.667 to **536.280 tok/s**) over 1-GPU
aggregated serving, while *PD* kept the lowest *ITL* p99 (14.864 versus 15.744 ms). Metrics are
cumulative across suites, so these numbers do not isolate encoder-cache hits from prefix caching,
batching or the extra GPUs.

| Workload | Concurrency | Configuration | Requests | *TTFT* p50 (ms) | *TTFT* p95 (ms) | *TPOT* p50 (ms) | *ITL* p99 (ms) | Output (tok/s) | Throughput (req/s) |
|---|---:|---|---:|---:|---:|---:|---:|---:|---:|
| `repeated_4k_c4` | 4 | Aggregated | 12/12 | 1,077.919 | 1,337.691 | 6.403 | 25.708 | 154.699 | 2.417 |
| `repeated_4k_c4` | 4 | *PD* | 12/12 | 1,304.894 | 1,418.578 | 6.223 | 12.202 | 146.941 | 2.296 |
| `repeated_4k_c4` | 4 | *EPD* | 12/12 | **298.825** | **322.128** | **6.144** | **9.712** | **367.278** | **5.739** |
| `repeated_4k_c8` | 8 | Aggregated | 24/24 | 2,196.771 | 2,364.464 | 6.988 | 17.550 | 187.667 | 2.932 |
| `repeated_4k_c8` | 8 | *PD* | 24/24 | 2,596.832 | 2,685.146 | **6.430** | **14.864** | 170.026 | 2.657 |
| `repeated_4k_c8` | 8 | *EPD* | 24/24 | **371.850** | **753.291** | 7.272 | 15.744 | **536.280** | **8.379** |

## Verification

Stage counts separate completion lines from token-render calls and include warm-up traffic, so they
exceed the 236 measured requests per configuration.

| Configuration | Role | Completion lines | Render lines |
|---|---|---:|---:|
| Aggregated | *Server* | 264 | 266 |
| *PD* | *Prefill* | 246 | 266 |
| *PD* | *Decode* | 264 | 0 |
| *EPD* | *Encode* | 264 | 0 |
| *EPD* | *Prefill* | 264 | 0 |
| *EPD* | *Decode* | 264 | 0 |

The *EPD* *Decode* sidecar logged **264 multimodal-item events**, and each router capture held 280
access lines. These counts confirm traffic at every *EPD* stage without per-request ID correlation
or byte-transfer logs. The *PD* *Prefill* count is 18 below *Decode* for an unestablished reason;
see [Router token counting](/gpustack-operator/v0.12.0/docs/modules/model-deployment/prefill-decode/index.md#router-token-counting).

## InfiniBand RDMA comparison

A follow-up on the ConnectX-7 InfiniBand node from
[Prefill and Decode Benchmark](/gpustack-operator/v0.12.0/docs/modules/model-deployment/prefill-decode-benchmark/index.md#infiniband-rdma-comparison) ran the
three-role `epd-arm3-epd` layout with one GPU per role. **All 16 measured cells completed: 236/236
requests with zero errors.**

### Transport verification

Add interface requests to `epd-deployment.yaml` from the
[TCP walkthrough](/gpustack-operator/v0.12.0/docs/walkthroughs/model-deployment/encode-prefill-decode/index.md#step-1-start-three-role-serving):

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: ModelDeployment
 metadata:
   name: epd-arm3-epd
   namespace: gpustack-bench
 spec:
   roles:
     - name: encode
       kind: Encode
       resources:
         accelerator: "1"
     - name: prefill
       kind: Prefill
       resources:
         accelerator: "1"
+        interface: "1"
     - name: decode
       kind: Decode
       resources:
         accelerator: "1"
+        interface: "1"
```

*Prefill* bound `mlx5_5` (LID `21818`) and *Decode* bound `mlx5_3` (LID `21813`). Both Mooncake
workers logged protocol `rdma`, registered KV caches and logged `Received RDMA ready ACK` with zero
failed transfers.

This confirms **Mooncake KV transfer over InfiniBand RDMA** between *Prefill* and *Decode*. NIXL
`ECCPUConnector` remained separate between *Encode* and *Prefill*; this capture does not establish
RDMA for the encoder-cache path.

### Context scaling

![TTFT p50 and output throughput by concurrency for aggregated serving, EPD over TCP and EPD over InfiniBand RDMA, four multimodal contexts](/gpustack-operator/v0.12.0/docs/assets/diagrams/encode-prefill-decode-benchmark-rdma.svg)

In this separate 3-GPU follow-up, RDMA *EPD* cut *TTFT* p50 and raised throughput over TCP *EPD*
in all 12 cells, reversing the `8k`, concurrency-8 *TTFT* regression against 1-GPU aggregated
serving (**3.46×** lower than TCP, **3.28×** lower than aggregated; **2.49×** and **2.51×** higher
output). Unequal GPU budgets (3 versus 1 GPU) and separate runs prevent equal-cost or
transport-only causal claims; single-request *ITL* p99 also rose slightly in all four contexts.

Bold values mark lower median *TTFT* or higher output throughput within each TCP/RDMA pair.

| Context | Concurrency | TCP *TTFT* p50 (ms) | RDMA *TTFT* p50 (ms) | *TTFT* reduction | TCP output (tok/s) | RDMA output (tok/s) |
|---|---:|---:|---:|---:|---:|---:|
| `1k` | 1 | 157.290 | **125.393** | **1.25×** | 135.864 | **140.079** |
| `1k` | 4 | 234.481 | **176.407** | **1.33×** | 420.666 | **494.492** |
| `1k` | 8 | 343.176 | **186.279** | **1.84×** | 715.225 | **916.405** |
| `2k` | 1 | 261.095 | **187.077** | **1.40×** | 120.054 | **129.224** |
| `2k` | 4 | 581.520 | **219.726** | **2.65×** | 326.686 | **443.715** |
| `2k` | 8 | 613.996 | **379.328** | **1.62×** | 537.856 | **749.450** |
| `4k` | 1 | 440.770 | **308.062** | **1.43×** | 88.808 | **104.528** |
| `4k` | 4 | 808.650 | **342.357** | **2.36×** | 204.756 | **302.606** |
| `4k` | 8 | 1,690.166 | **442.003** | **3.82×** | 255.080 | **531.323** |
| `8k` | 1 | 798.743 | **553.685** | **1.44×** | 68.562 | **80.768** |
| `8k` | 4 | 1,572.574 | **605.871** | **2.60×** | 134.224 | **226.206** |
| `8k` | 8 | 4,182.324 | **1,207.904** | **3.46×** | 144.753 | **360.029** |

Against the 1-GPU aggregated baseline at concurrency 8, RDMA *EPD* had **4.46×** lower *TTFT* p50
and **2.29×** higher output at `4k`, and **3.28×** lower *TTFT* p50 and **2.51×** higher output at
`8k` (still 3 GPUs versus 1 GPU).

| Context, concurrency 8 | Aggregated *TTFT* p50 (ms) | RDMA EPD *TTFT* p50 (ms) | Aggregated output (tok/s) | RDMA EPD output (tok/s) |
|---|---:|---:|---:|---:|
| `4k` | 1,972.504 | **442.003** | 231.845 | **531.323** |
| `8k` | 3,963.085 | **1,207.904** | 143.211 | **360.029** |

### Mixed traffic

Both streams shared a 3.677 s mixed window on RDMA versus 5.217 s on TCP. RDMA lowered median
*TTFT* **3.00×** on `concurrent_decode_1k` (output **1.94×** higher; *ITL* p99 **9.894 ms** versus
296.566 ms aggregated) and **2.02×** on `heavy_vision_4k` (output **1.42×** higher).

| Stream | TCP *TTFT* p50 (ms) | RDMA *TTFT* p50 (ms) | TCP *ITL* p99 (ms) | RDMA *ITL* p99 (ms) | TCP output (tok/s) | RDMA output (tok/s) |
|---|---:|---:|---:|---:|---:|---:|
| `concurrent_decode_1k` | 647.387 | **215.522** | 16.130 | **9.894** | 241.119 | **466.705** |
| `heavy_vision_4k` | 959.134 | **474.237** | 16.845 | **10.119** | 147.201 | **208.875** |

### Encoder-cache reuse

RDMA kept the repeated-image gain and lowered median *TTFT* in both cells. The captures still do
not isolate encoder-cache hits from prefix caching, batching or GPU count.

| Concurrency | TCP *TTFT* p50 (ms) | RDMA *TTFT* p50 (ms) | TCP output (tok/s) | RDMA output (tok/s) |
|---:|---:|---:|---:|---:|
| 4 | 298.825 | **246.151** | 367.278 | **390.899** |
| 8 | 371.850 | **286.294** | 536.280 | **700.762** |

### RDMA trade-offs

RDMA reversed the `8k`, concurrency-8 *TTFT* penalty while requiring InfiniBand interfaces on both
KV-transfer roles. Separate runs, unchanged 1/2/3 GPU budgets, and slight single-request *ITL* p99
increases across all four contexts limit causal attribution. Neither capture measures cross-node
placement or RDMA encoder-cache transfer.

## Limitations

- One pass per configuration, with 8–24 requests per stream or cell, limits tail-percentile support.
- Unequal GPU budgets (1, 2 and 3 GPUs) prevent equal-cost throughput or per-GPU efficiency claims.
- All roles shared one GPU node; cross-node placement and bandwidth were not measured.
- Neither the TCP baseline nor the InfiniBand follow-up measures cross-`Pod` NVLink.
- Configurations ran sequentially with warmed weights; cache state and run order were not isolated.
- Encoder-cache, prefix-cache and compute-isolation effects were not measured separately.
- Early stopping varied output-token totals; output throughput uses actual generated tokens.

---

**See also** — [Encode, Prefill and Decode](/gpustack-operator/v0.12.0/docs/modules/model-deployment/encode-prefill-decode/index.md) (role and connector contracts) ·
[Encode, Prefill and Decode Walkthrough](/gpustack-operator/v0.12.0/docs/walkthroughs/model-deployment/encode-prefill-decode/index.md) (deployment manifest) ·
[Prefill and Decode Benchmark](/gpustack-operator/v0.12.0/docs/modules/model-deployment/prefill-decode-benchmark/index.md) (equal-GPU text-model comparison)

**Next** → [Elastic EP](/gpustack-operator/v0.12.0/docs/modules/model-deployment/elastic-ep/index.md) — resize one serving instance's collective in place.
