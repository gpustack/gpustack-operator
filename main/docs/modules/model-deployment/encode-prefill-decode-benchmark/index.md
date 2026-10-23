# Encode, Prefill and Decode Benchmark

On one H100 SXM5 node, *encode/prefill/decode (EPD)* reduced long-prompt generation stalls and
repeated-image latency compared with aggregated serving and *prefill/decode (PD)*.
Aggregated serving, *PD* and *EPD* used **one, two and three GPUs**, respectively.
These results compare deployed configurations with unequal GPU budgets.

At `8k` and concurrency 8, *EPD* reduced *inter-token latency (ITL)* p99 from `296.710 ms` to
**13.607 ms**, or **21.8× lower**, compared with aggregated serving.
Its *time to first token (TTFT)* p50 was higher: `4,182.324 ms` against `3,963.085 ms`.
[Encode, Prefill and Decode](/gpustack-operator/main/docs/modules/model-deployment/encode-prefill-decode/index.md) describes the role contract.

The initial capture used cross-`Pod` TCP. The separate [InfiniBand RDMA comparison](#infiniband-rdma-comparison)
adds matched follow-up measurements while retaining the TCP results and conclusions.

## Contents

- [Configuration](#configuration)
- [Hardware, model and workload](#hardware-model-and-workload)
- [Suite 1: Multimodal context scaling](#suite-1-multimodal-context-scaling)
- [Suite 2: Mixed vision and decode](#suite-2-mixed-vision-and-decode)
- [Suite 3: Encoder-cache reuse](#suite-3-encoder-cache-reuse)
- [Stage execution verification](#stage-execution-verification)
- [Summary](#summary)
- [InfiniBand RDMA comparison](#infiniband-rdma-comparison)
- [Limitations](#limitations)

## Configuration

Each configuration used one `ModelDeployment`, with one replica per role and one GPU per `Pod`.
All serving roles ran on the same GPU node, `gpu-node-0`.

| Configuration | Roles | GPUs | KV connector | Encoder connector |
|---|---|---:|---|---|
| Aggregated | 1 *Server* | 1 | None | None |
| *PD* | 1 *Prefill* + 1 *Decode* | 2 | `MooncakeConnector`, TCP | None |
| *EPD* | 1 *Encode* + 1 *Prefill* + 1 *Decode* | 3 | `MooncakeConnector`, TCP | `ECCPUConnector`, NIXL |

Each `Pod` exposed only its assigned GPU, with a private `/dev/shm` and IPC namespace.
*`hostIPC`* was disabled, and no remote direct memory access (*RDMA*) interface was attached.
**All cross-`Pod` NIXL encoder-cache and Mooncake KV transfers used container TCP sockets over `eth0`.**
The host's NVLink fabric was not used between roles.

## Hardware, model and workload

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

The operator added `--enforce-eager` and `--no-enable-prefix-caching` only to *Encode*.
*Prefill* and *Decode* retained CUDA graphs.
Aggregated serving and *PD* also set `--enable-scale-out` for router token rendering; *EPD* did not.

### Suite 1 workload

Suite 1 varied image and text length with a `128`-token completion limit.
Image dimensions are pixels; text-token values are generator targets.
Measured prompt lengths include the model's multimodal request formatting.

| Context | Image | Text target | Measured prompt tokens |
|---|---|---:|---|
| `1k` | 560 × 560 | 624 | 1,047–1,050 |
| `2k` | 840 × 840 | 1,148 | 2,071–2,074 |
| `4k` | 1260 × 1260 | 2,071 | 4,119–4,122 |
| `8k` | 1792 × 1792 | 4,096 | 8,215–8,219 |

Concurrency 1, 4 and 8 used 8, 12 and 24 measured requests per context, respectively.

### Suite 2 workload

Two streams ran concurrently, with four workers and twelve requests each.
The small stream used unique `560 × 560` images, a `624`-token text target and `max_tokens=256`.
The heavy-vision stream used unique `1792 × 1792` images, a `400`-token text target and `max_tokens=64`.
Measured prompt lengths were 1,048–1,051 and 4,520–4,523 tokens, respectively.

### Suite 3 workload

Suite 3 reused one `1792 × 1792` image with a different question per request.
It used a `400`-token text target and `max_tokens=64`; measured prompts contained 4,518–4,522 tokens.
Concurrency 4 and 8 used 12 and 24 measured requests.

### Method

The load generator ran inside the cluster. Each configuration ran once, sequentially on the same node.
Two warm-up requests preceded every Suite 1 and Suite 3 cell and were excluded from latency summaries.
The 45 result files contain 36 Suite 1 cells, three mixed-load runs and six repeated-image cells.
Each mixed-load file reports both streams separately.

**All 708 measured requests succeeded with zero errors**, 236 per configuration.
Each configuration also issued 28 warm-up requests.
*Time per output token (TPOT)* measures per-request generation time per token; *ITL* measures streamed-token gaps.
Bold table values mark lower latency or higher throughput within each matched workload and concurrency.

## Suite 1: Multimodal context scaling

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

At `4k` and concurrency 8, *EPD* reduced *ITL* p99 from `231.453 ms` to **15.352 ms**, or **15.1×**.
*TTFT* p50 also fell from `1,972.504 ms` to `1,690.166 ms`.
At `8k` and concurrency 8, *ITL* p99 improved **21.8×**, but *TTFT* p50 increased by `219.239 ms`.
Separating generation from prompt processing improved token-gap tails without consistently improving first-token latency.

Aggregated serving had the lowest *TTFT* p50 for every `1k` cell and the lowest *TTFT* p95 for every `2k` cell.
At `8k` and concurrency 4, *EPD* improved median *TTFT* but worsened its p95 against aggregated serving.
The recorded medians and tails support different choices; *EPD* has no universal latency advantage.

## Suite 2: Mixed vision and decode

| Workload | Concurrency | Configuration | Requests | *TTFT* p50 (ms) | *TTFT* p95 (ms) | *TPOT* p50 (ms) | *ITL* p99 (ms) | Output (tok/s) | Throughput (req/s) |
|---|---:|---|---:|---:|---:|---:|---:|---:|---:|
| `concurrent_decode_1k` | 4 | Aggregated | 12/12 | 1,214.046 | 1,573.818 | 12.417 | 296.566 | 176.449 | 1.416 |
| `concurrent_decode_1k` | 4 | *PD* | 12/12 | 1,487.862 | 1,828.701 | **6.432** | 16.708 | 158.198 | 1.249 |
| `concurrent_decode_1k` | 4 | *EPD* | 12/12 | **647.387** | **1,001.308** | 8.551 | **16.130** | **241.119** | **2.300** |
| `heavy_vision_4k` | 4 | Aggregated | 12/12 | 2,215.441 | 2,419.269 | **6.597** | 197.815 | 90.644 | 1.416 |
| `heavy_vision_4k` | 4 | *PD* | 12/12 | 2,379.965 | 3,770.445 | 7.138 | **16.348** | 79.932 | 1.249 |
| `heavy_vision_4k` | 4 | *EPD* | 12/12 | **959.134** | **1,971.796** | 8.625 | 16.845 | **147.201** | **2.300** |

The mixed windows lasted `8.473 s`, `9.608 s` and **5.217 s** for aggregated serving, *PD* and *EPD*.
Each stream's output and request throughput uses its configuration's full mixed window as the denominator.
Combined output throughput was `267.093 tok/s`, `238.130 tok/s` and **388.320 tok/s**, respectively.

For small requests, *EPD* reduced *TTFT* p50 from `1,214.046 ms` to **647.387 ms** against aggregated serving.
Their *ITL* p99 fell from `296.566 ms` to **16.130 ms**, or **18.4× lower**.
For heavy-vision requests, *TTFT* p50 fell from `2,215.441 ms` to **959.134 ms**.
The isolated roles improved these mixed-load outcomes while using three GPUs instead of one.

## Suite 3: Encoder-cache reuse

| Workload | Concurrency | Configuration | Requests | *TTFT* p50 (ms) | *TTFT* p95 (ms) | *TPOT* p50 (ms) | *ITL* p99 (ms) | Output (tok/s) | Throughput (req/s) |
|---|---:|---|---:|---:|---:|---:|---:|---:|---:|
| `repeated_4k_c4` | 4 | Aggregated | 12/12 | 1,077.919 | 1,337.691 | 6.403 | 25.708 | 154.699 | 2.417 |
| `repeated_4k_c4` | 4 | *PD* | 12/12 | 1,304.894 | 1,418.578 | 6.223 | 12.202 | 146.941 | 2.296 |
| `repeated_4k_c4` | 4 | *EPD* | 12/12 | **298.825** | **322.128** | **6.144** | **9.712** | **367.278** | **5.739** |
| `repeated_4k_c8` | 8 | Aggregated | 24/24 | 2,196.771 | 2,364.464 | 6.988 | 17.550 | 187.667 | 2.932 |
| `repeated_4k_c8` | 8 | *PD* | 24/24 | 2,596.832 | 2,685.146 | **6.430** | **14.864** | 170.026 | 2.657 |
| `repeated_4k_c8` | 8 | *EPD* | 24/24 | **371.850** | **753.291** | 7.272 | 15.744 | **536.280** | **8.379** |

At concurrency 8, *EPD* reduced *TTFT* p50 from `2,196.771 ms` to **371.850 ms**, or **5.9× lower**.
Output throughput rose from `187.667 tok/s` to **536.280 tok/s**, or **2.86× higher**.
*PD* retained the lowest *ITL* p99 in that cell: `14.864 ms` against *EPD*'s `15.744 ms`.

These results establish a repeated-image advantage for the captured *EPD* configuration.
They do not isolate encoder-cache hits from prefix caching, batching or the extra GPU allocation.
The captured metrics are cumulative across suites, without a per-suite encoder-cache hit-rate delta.

## Stage execution verification

The stage capture separates completion access lines from token-render calls.
It includes warm-up traffic and is broader than the 236 measured requests per configuration.

| Configuration | Role | Completion lines | Render lines |
|---|---|---:|---:|
| Aggregated | *Server* | 264 | 266 |
| *PD* | *Prefill* | 246 | 266 |
| *PD* | *Decode* | 264 | 0 |
| *EPD* | *Encode* | 264 | 0 |
| *EPD* | *Prefill* | 264 | 0 |
| *EPD* | *Decode* | 264 | 0 |

The *EPD* *Decode* sidecar also logged **264 multimodal-item events**.
Each configuration's router access capture contained 280 lines.
These counts establish traffic at every *EPD* stage, but are not request-ID correlation or byte-transfer measurements.
The *PD* *Prefill* count is eighteen below its *Decode* count; the capture does not establish the reason.

The aggregated and *PD* render counts are separate from completion counts.
A successful response or matching aggregate counts alone cannot prove that every request traversed every intended stage.
See [Counting engine tokens](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode/index.md#counting-engine-tokens) for the router's token-render requirement.

## Summary

### Benefits

- Long-prompt generation tails improved: *EPD* had **15.1×–21.8× lower ITL p99** at `4k`/`8k`, concurrency 8, than aggregated serving.
- Repeated-image traffic reached **536.280 tok/s** at concurrency 8, with **5.9× lower TTFT p50** than aggregated serving.
- Mixed traffic completed in **5.217 s**, against `8.473 s` for aggregated serving, with lower first-token and token-gap tails.

### Trade-offs

- *EPD* used **three GPUs**, compared with one for aggregated serving and two for *PD*.
- Small prompts often favored aggregated serving on first-token latency.
- The `8k`, concurrency-8 *EPD* median *TTFT* remained higher than aggregated serving despite much lower *ITL* p99.

Choose *EPD* for large-image generation isolation, repeated-image workloads or the measured mixed-traffic benefits when three GPUs are available.
Small-prompt first-token latency can favor aggregated serving.
The cross-role TCP transfers and unequal GPU budgets prevent a universal latency or equal-cost efficiency claim.

## InfiniBand RDMA comparison

A follow-up on the same ConnectX-7 InfiniBand node setup described in
[Prefill and Decode Benchmark](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode-benchmark/index.md#infiniband-rdma-comparison) ran the
three-role `epd-arm3-epd` allocation with one GPU per role. **All 16 measured cells completed:
236/236 requests, with zero errors.**

### Transport verification

Update `epd-deployment.yaml` from the
[TCP walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/encode-prefill-decode/index.md#step-1-start-three-role-serving):

```diff
 spec:
+  kvTransfer:
+    protocol: RDMA
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

*Prefill* bound `mlx5_5` with LID `21818`; *Decode* bound `mlx5_3` with LID `21813`.
Both Mooncake workers logged protocol `rdma` and registered their KV caches.
The receive log recorded `Received RDMA ready ACK`; the captured logs contained no failed-transfer entries.

This verifies **Mooncake KV transfer over InfiniBand RDMA** between *Prefill* and *Decode*.
NIXL `ECCPUConnector` still carried multimodal features between *Encode* and *Prefill*.
The capture does not establish RDMA for that separate encoder-cache path.
Each role used one GPU, so these cross-role transfers did not use NVLink.

### Context scaling

The matched cells below retain the TCP baseline and add the RDMA follow-up.
Both runs used the same context targets, concurrency and request counts.
Bold values mark lower median first-token latency or higher output throughput within each pair.

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

At `4k`, concurrency 8, RDMA reduced *EPD* median *TTFT* **3.82×** and raised output throughput **2.08×**.
At `8k`, concurrency 8, median *TTFT* fell **3.46×** while output throughput rose **2.49×**.
The TCP *EPD* first-token regression against aggregated serving at `8k`, concurrency 8, reversed with RDMA.

| Context, concurrency 8 | Aggregated *TTFT* p50 (ms) | RDMA EPD *TTFT* p50 (ms) | Aggregated output (tok/s) | RDMA EPD output (tok/s) |
|---|---:|---:|---:|---:|
| `4k` | 1,972.504 | **442.003** | 231.845 | **531.323** |
| `8k` | 3,963.085 | **1,207.904** | 143.211 | **360.029** |

Against the recorded aggregated configuration, RDMA *EPD* had **4.46× lower median TTFT** at `4k` and **3.28× lower** at `8k`.
Output throughput was **2.29×** and **2.51× higher**, respectively.
These comparisons still allocate three GPUs to *EPD* and one to aggregated serving.

### Mixed traffic

| Stream | TCP *TTFT* p50 (ms) | RDMA *TTFT* p50 (ms) | TCP *ITL* p99 (ms) | RDMA *ITL* p99 (ms) | TCP output (tok/s) | RDMA output (tok/s) |
|---|---:|---:|---:|---:|---:|---:|
| `concurrent_decode_1k` | 647.387 | **215.522** | 16.130 | **9.894** | 241.119 | **466.705** |
| `heavy_vision_4k` | 959.134 | **474.237** | 16.845 | **10.119** | 147.201 | **208.875** |

The concurrent decode stream had **3.00× lower median TTFT** and **1.94× higher output throughput**.
Its *ITL* p99 fell to **9.894 ms**, against TCP *EPD* at 16.130 ms and aggregated serving at 296.566 ms.
Heavy-vision median *TTFT* fell **2.02×**, with output throughput **1.42× higher**.
Both streams shared the mixed window: 3.677 s with RDMA, against 5.217 s with TCP.

### Encoder-cache reuse

| Concurrency | TCP *TTFT* p50 (ms) | RDMA *TTFT* p50 (ms) | TCP output (tok/s) | RDMA output (tok/s) |
|---:|---:|---:|---:|---:|
| 4 | 298.825 | **246.151** | 367.278 | **390.899** |
| 8 | 371.850 | **286.294** | 536.280 | **700.762** |

The RDMA follow-up retained the repeated-image benefit and improved median *TTFT* in both measured cells.
The captures still do not separate encoder-cache hits from prefix caching, batching or GPU allocation.

### Scope and trade-offs

RDMA reversed the `8k`, concurrency-8 first-token disadvantage while retaining separate generation resources.
It required InfiniBand interfaces on both KV-transfer roles; declaring `RDMA` alone does not establish a working transport.
The TCP and RDMA measurements are separate runs on the same GPU model, not repeated transport-only trials.

Model, role counts and GPU budgets remained unchanged.
Neither capture measures cross-node placement or RDMA encoder-cache transfer.

Single-request *ITL* p99 rose slightly in all four context shapes, despite lower median *TTFT* and higher throughput.

## Limitations

- One run per configuration, with 8–24 requests per stream or cell. Tail percentiles have limited sample support.
- Unequal GPU allocations prevent an equal-cost throughput or per-GPU efficiency conclusion.
- All serving roles shared one GPU node. Cross-node placement and bandwidth were not measured.
- The initial capture used cross-`Pod` TCP over `eth0`; the follow-up verified InfiniBand KV transfer. Neither establishes cross-`Pod` NVLink.
- The configurations ran sequentially with warmed model weights. Cache state and execution order were not independently controlled.
- Encoder-cache, prefix-cache and compute-isolation effects were not measured separately.
- Completion limits were fixed, but early stopping produced different output-token totals. Output throughput uses actual generated tokens.

---

**See also** — [Encode, Prefill and Decode](/gpustack-operator/main/docs/modules/model-deployment/encode-prefill-decode/index.md) (role and connector contracts) ·
[Encode, Prefill and Decode Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/encode-prefill-decode/index.md) (deployment manifest) ·
[Prefill and Decode Benchmark](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode-benchmark/index.md) (equal-GPU text-model comparison)

**Next** → [Elastic EP](/gpustack-operator/main/docs/modules/model-deployment/elastic-ep/index.md) — resize one serving instance's collective in place.
