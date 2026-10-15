# Model Deployment Encode, Prefill and Decode Benchmark

A live run on 4 × NVIDIA L40S compared three serving shapes for `Qwen/Qwen2.5-VL-7B-Instruct`:
aggregated, prefill/decode and encode/prefill/decode (EPD). Three workloads exercise them: a prompt
sweep from `1k` to `8k` tokens, a mixed stream of heavy-vision and small requests, and one image
queried repeatedly. [Model Deployment Encode, Prefill and Decode](epd.md) describes the EPD topology.

EPD is not faster everywhere. At `1k` and `2k` tokens the aggregated arm has the lowest time to
first token (TTFT), because two network hops cost more than a short vision pass. At `4k` and `8k`
under concurrency the order reverses. At `8k` with four requests in flight, EPD has a TTFT p50 of
`241.8 ms`, against `697.9 ms` for prefill/decode (`2.89×`) and `757.1 ms` for aggregated (`3.13×`).

At that point EPD also reaches `128.24 tok/s`, `13.3%` above prefill/decode and `5.7%` above
aggregated. At `8k` with eight in flight its inter-token latency (ITL) p99 is `52.72 ms`, while
aggregated degrades to `199.34 ms`.

Under mixed traffic the advantage depends on the stream. EPD protects the small requests
(`226.5 ms` TTFT p50, against `315.1 ms` and `388.3 ms`) but is the slowest arm for the heavy-vision
requests (`5017.0 ms`, against `2854.0 ms` for aggregated). On a repeated `4k` image EPD wins
outright: `356.0 ms` TTFT p50 at four in flight (`2.22×` faster than prefill/decode) and `31.9%`
higher output throughput at eight.

## Contents

- [The three arms](#the-three-arms)
- [Hardware, model and workload](#hardware-model-and-workload)
- [Suite 1: k-context multimodal scaling](#suite-1-k-context-multimodal-scaling)
- [Suite 2: mixed heavy-vision and concurrent decode](#suite-2-mixed-heavy-vision-and-concurrent-decode)
- [Suite 3: repeated-image encoder-cache reuse](#suite-3-repeated-image-encoder-cache-reuse)
- [Stage execution verification](#stage-execution-verification)
- [When EPD helps and when it does not](#when-epd-helps-and-when-it-does-not)
- [What this run does not measure](#what-this-run-does-not-measure)

## The three arms

Each arm is one `ModelDeployment` with one replica per role and one accelerator per replica. The
arms differ in topology and in how many cards they use.

| Arm | Roles | Cards | KV connector | Encoder connector |
|---|---|---|---|---|
| Aggregated | `server` | 1 | none | none |
| Prefill/Decode | `prefill`, `decode` | 2 | `MooncakeConnector` over tcp | none |
| EPD | `encode`, `prefill`, `decode` | 3 | `MooncakeConnector` over tcp | `ECCPUConnector` over NIXL |

The aggregated arm needs no connector, because one instance holds the whole model. Prefill and
decode hand KV blocks across two Pods, pinned to tcp because the nodes have no RDMA interface. EPD
adds the encoder leg on top of the same KV transfer. See
[Encoder-cache connectors](epd.md#encoder-cache-connectors) for what each connector moves.

The scheduler placed the roles on two nodes, `node-a` and `node-b`. In the EPD arm, encode and
prefill shared `node-a` and decode ran on `node-b`, which follows the preference described in
[Scheduling co-location](epd.md#scheduling-co-location). The other two arms landed as follows:

| Arm | Role | Node |
|---|---|---|
| Prefill/Decode | `prefill` | `node-b` |
| Prefill/Decode | `decode` | `node-a` |
| Aggregated | `server` | `node-b` |

## Hardware, model and workload

| Property | Value |
|---|---|
| Accelerators | 4 × NVIDIA L40S 48 GB, 2 per node |
| Driver | 580.173.02, CUDA 13.0 |
| Interconnect | none; no RDMA interface on either node |
| Instance type | `gpustack--nvidia-l40s-linux-amd64` |
| Runner image | `docker.io/gpustack/runner:cuda13.0-vllm0.30.0` |
| Engine | vLLM 0.30.0 |
| Router | `gpustack/llm-router:v0.3.1`, disaggregation sidecar v0.11.0 |
| Model | `Qwen/Qwen2.5-VL-7B-Instruct` |
| Engine arguments | `--max-model-len=16384`, `--gpu-memory-utilization=0.85`, `--enforce-eager` |
| Parallelism | TP 1 per role |

Every arm ran the same weights, so the comparison is per arm, not per card: the aggregated arm used
one card and EPD used three. `--max-model-len` is `16384` so that the `8k` prompts are not truncated.
The prefill/decode and aggregated arms also set `--enable-scale-out`, which their router needs to
count tokens. EPD does not need it.

All three suites ran on every arm, and every request succeeded.

### Suite 1 workload

Suite 1 holds the completion budget at `max_tokens=128` and varies only the prompt. Qwen2.5-VL turns
an `S`-square image into `(S / 28)²` visual tokens.

| Context | Image | Visual tokens | Text tokens | Measured prompt tokens |
|---|---|---|---|---|
| `1k` | 560 × 560 | 400 | 624 | 1072 to 1073 |
| `2k` | 840 × 840 | 900 | 1148 | 2101 to 2102 |
| `4k` | 1260 × 1260 | 2025 | 2071 | 4157 to 4158 |
| `8k` | 1792 × 1792 | 4096 | 4096 | 8281 to 8282 |

Each context runs at concurrency `1`, `4` and `8`, with `8`, `12` and `24` requests.

### Suite 2 workload

Suite 2 runs two streams against one arm at the same time. Four workers ask for `1k` requests
(`560 × 560`, `1073` prompt tokens, `max_tokens=256`). Four other workers send `4k` heavy-vision
bursts (a unique `1792 × 1792` PNG per request, `4523` prompt tokens, `max_tokens=64`).
Each stream sends twelve requests, and no image repeats, so no cache can answer a burst.

### Suite 3 workload

Suite 3 sends one shared `1792 × 1792` PNG (`4527` prompt tokens, `max_tokens=64`) with a different
question per request, at concurrency `4` and `8`. This is the multi-turn shape: the image is fixed
and the encoder's output is worth keeping.

### Method

Two warm-up requests preceded every Suite 1 and Suite 3 configuration and are excluded from the
results. Each configuration ran once, so every number below is a single measurement. The arms ran
one after another on a shared cluster, and the later arms had the warmer nodes. See
[What this run does not measure](#what-this-run-does-not-measure).

## Suite 1: k-context multimodal scaling

`Requests` is succeeded of issued. Every cell succeeded.

| Workload | Prompt tokens | Concurrency | Arm | Requests | TTFT p50 (ms) | TTFT p95 (ms) | TPOT p50 (ms) | ITL p99 (ms) | Output (tok/s) | Throughput (req/s) |
|---|---|---|---|---|---|---|---|---|---|---|
| `1k` | 1072 | 1 | Aggregated | 8/8 | 99.1 | 104.4 | 20.46 | 25.26 | 46.1 | 0.715 |
| `1k` | 1072 | 1 | EPD | 8/8 | 180.4 | 183.9 | 20.29 | 23.16 | 44.09 | 0.651 |
| `1k` | 1072 | 1 | Prefill/Decode | 8/8 | 198.6 | 205.4 | 20.3 | 23.69 | 43.53 | 0.642 |
| `1k` | 1072 | 4 | Aggregated | 12/12 | 116.1 | 181.5 | 22.08 | 32.3 | 156.16 | 2.283 |
| `1k` | 1072 | 4 | EPD | 12/12 | 172.9 | 421.8 | 21.94 | 44.58 | 148.54 | 2.148 |
| `1k` | 1072 | 4 | Prefill/Decode | 12/12 | 122.1 | 332.8 | 22.1 | 46.89 | 152.3 | 2.089 |
| `1k` | 1073 | 8 | Aggregated | 24/24 | 176.7 | 250.8 | 23.61 | 60.12 | 290.68 | 4.109 |
| `1k` | 1073 | 8 | EPD | 24/24 | 227.8 | 501.2 | 22.69 | 53.39 | 272.9 | 3.68 |
| `1k` | 1073 | 8 | Prefill/Decode | 24/24 | 214.4 | 423.4 | 23.25 | 65.57 | 275.98 | 3.898 |
| `2k` | 2101 | 1 | Aggregated | 8/8 | 178.3 | 186.8 | 20.63 | 23.49 | 45.56 | 0.384 |
| `2k` | 2101 | 1 | EPD | 8/8 | 268.0 | 272.3 | 20.52 | 23.71 | 44.15 | 0.378 |
| `2k` | 2101 | 1 | Prefill/Decode | 8/8 | 321.7 | 326.4 | 20.54 | 23.79 | 43.5 | 0.354 |
| `2k` | 2101 | 4 | Aggregated | 12/12 | 173.4 | 235.7 | 22.49 | 38.35 | 156.62 | 1.304 |
| `2k` | 2101 | 4 | EPD | 12/12 | 178.7 | 489.5 | 22.22 | 45.91 | 157.15 | 1.281 |
| `2k` | 2101 | 4 | Prefill/Decode | 12/12 | 168.3 | 362.2 | 22.37 | 45.23 | 154.26 | 1.288 |
| `2k` | 2102 | 8 | Aggregated | 24/24 | 276.8 | 453.5 | 23.63 | 94.36 | 280.27 | 2.38 |
| `2k` | 2102 | 8 | EPD | 24/24 | 260.8 | 932.3 | 23.85 | 47.46 | 265.62 | 2.26 |
| `2k` | 2102 | 8 | Prefill/Decode | 24/24 | 347.5 | 869.6 | 23.27 | 47.18 | 270.52 | 2.233 |
| `4k` | 4157 | 1 | Aggregated | 8/8 | 353.1 | 365.3 | 20.85 | 24.14 | 42.66 | 0.333 |
| `4k` | 4157 | 1 | EPD | 8/8 | 467.2 | 478.3 | 20.75 | 23.75 | 41.27 | 0.322 |
| `4k` | 4157 | 1 | Prefill/Decode | 8/8 | 583.8 | 587.9 | 20.75 | 23.84 | 39.79 | 0.311 |
| `4k` | 4157 | 4 | EPD | 12/12 | 205.7 | 1343.1 | 22.52 | 26.85 | 148.12 | 1.157 |
| `4k` | 4157 | 4 | Aggregated | 12/12 | 339.0 | 832.8 | 22.92 | 28.67 | 151.49 | 1.184 |
| `4k` | 4157 | 4 | Prefill/Decode | 12/12 | 362.7 | 1280.6 | 22.56 | 27.99 | 144.84 | 1.132 |
| `4k` | 4158 | 8 | Aggregated | 24/24 | 644.9 | 1732.6 | 24.21 | 167.79 | 249.34 | 1.948 |
| `4k` | 4158 | 8 | EPD | 24/24 | 959.4 | 2410.1 | 23.67 | 49.23 | 239.73 | 1.873 |
| `4k` | 4158 | 8 | Prefill/Decode | 24/24 | 860.7 | 1754.0 | 24.87 | 47.81 | 235.19 | 1.837 |
| `8k` | 8281 | 1 | Aggregated | 8/8 | 705.4 | 747.4 | 21.29 | 24.16 | 37.24 | 0.299 |
| `8k` | 8281 | 1 | EPD | 8/8 | 856.7 | 887.1 | 21.17 | 24.7 | 35.43 | 0.298 |
| `8k` | 8281 | 1 | Prefill/Decode | 8/8 | 1129.0 | 1150.6 | 21.17 | 24.79 | 32.81 | 0.276 |
| `8k` | 8281 | 4 | EPD | 12/12 | 241.8 | 2279.9 | 23.3 | 47.88 | 128.24 | 1.036 |
| `8k` | 8281 | 4 | Aggregated | 12/12 | 757.1 | 1880.9 | 24.3 | 49.28 | 121.34 | 0.965 |
| `8k` | 8281 | 4 | Prefill/Decode | 12/12 | 697.9 | 2853.0 | 23.34 | 48.36 | 113.15 | 0.947 |
| `8k` | 8282 | 8 | EPD | 24/24 | 880.7 | 3394.3 | 26.72 | 52.72 | 187.1 | 1.5 |
| `8k` | 8282 | 8 | Aggregated | 24/24 | 1243.5 | 2590.3 | 32.41 | 199.34 | 179.13 | 1.459 |
| `8k` | 8282 | 8 | Prefill/Decode | 24/24 | 1513.9 | 3473.7 | 26.18 | 51.19 | 170.31 | 1.378 |

Time per output token barely separates the arms. TPOT p50 stays between `20.29 ms` and `26.72 ms`
in all thirty-six cells, and output throughput follows concurrency, not topology: `1k` climbs from
about `45 tok/s` at one request in flight to `291 tok/s` at eight.

Time to first token is where the topology shows.

**One request in flight.** Aggregated is fastest at every context, and EPD beats prefill/decode at
every one. A request pays a transfer hop for each connector before its first token, and at low
concurrency nothing is waiting behind it. At `1k` the EPD penalty is `81.3 ms`, because the transfer
costs more than a `400`-token vision pass.

**Eight requests in flight.** The two disaggregated arms hold ITL p99 flat, while aggregated degrades
with prompt length:

| Concurrency 8 | `1k` | `2k` | `4k` | `8k` |
|---|---|---|---|---|
| Aggregated | 60.12 | 94.36 | 167.79 | 199.34 |
| EPD | 53.39 | 47.46 | 49.23 | 52.72 |
| Prefill/Decode | 65.57 | 47.18 | 47.81 | 51.19 |

In the aggregated arm a long prefill shares a card with the requests already generating tokens. Its
`4k` and `8k` ITL p99 is `3.4×` and `3.8×` EPD's. Prefill/Decode removes the same interference, so
EPD and prefill/decode differ little on this metric.

TTFT p50 at eight in flight crosses over as the prompt grows. At `1k` and `2k` EPD is still behind
aggregated. At `4k` it is behind on TTFT p50 (`959.4 ms` against `644.9 ms`) but well ahead on ITL
p99 (`49.23 ms` against `167.79 ms`). At `8k` EPD wins both: TTFT p50 is `880.7 ms` against
`1243.5 ms` (aggregated) and `1513.9 ms` (prefill/decode).

The `8k` cell at four in flight is the strongest result and the weakest measurement. EPD's TTFT p50
of `241.8 ms` is the lowest of all thirty-six cells, but its p95 is `2279.9 ms`, worse than
aggregated's `1880.9 ms`. With twelve requests, the p95 sits between the slowest two samples, so
treat the median and the tail as separate readings.

## Suite 2: mixed heavy-vision and concurrent decode

Two streams share one arm: four workers ask for `1k` completions while four workers push `4k`
unique-image bursts through the same GPU. This test shows how isolating the encoder changes what each
arm is good at.

| Workload | Arm | Requests | TTFT p50 (ms) | TTFT p95 (ms) | TPOT p50 (ms) | TPOT p95 (ms) | ITL p95 (ms) | ITL p99 (ms) | Output (tok/s) | Throughput (req/s) |
|---|---|---|---|---|---|---|---|---|---|---|
| `concurrent_decode_1k` | Aggregated | 12/12 | 388.3 | 502.1 | 24.39 | 27.76 | 25.3 | 89.22 | 69.2 | 1.038 |
| `concurrent_decode_1k` | EPD | 12/12 | 226.5 | 266.4 | 24.52 | 26.18 | 47.99 | 66.87 | 66.96 | 0.972 |
| `concurrent_decode_1k` | Prefill/Decode | 12/12 | 315.1 | 380.2 | 24.37 | 25.58 | 46.02 | 55.66 | 63.12 | 0.879 |
| `heavy_vision_4k` | Aggregated | 12/12 | 2854.0 | 5677.9 | 38.14 | 75.86 | 176.22 | 386.39 | 66.43 | 1.038 |
| `heavy_vision_4k` | EPD | 12/12 | 5017.0 | 6964.2 | 27.33 | 39.96 | 50.39 | 53.42 | 62.19 | 0.972 |
| `heavy_vision_4k` | Prefill/Decode | 12/12 | 6187.9 | 7977.2 | 28.57 | 38.55 | 50.15 | 63.61 | 56.23 | 0.879 |

Every arm completed both streams. The two streams share one batch window: `11.561 s` for aggregated,
`12.35 s` for EPD and `13.658 s` for prefill/decode. The per-stream throughput columns are two views
of that one window.

**The `1k` stream prefers EPD.** EPD's TTFT p50 is `226.5 ms`, against `315.1 ms` for prefill/decode
and `388.3 ms` for aggregated, and its p95 is the lowest at `266.4 ms`. In prefill/decode the prefill
engine runs the `1792 × 1792` vision pass inline, so small prefills queue behind it. In EPD the
encoder absorbs those passes on its own GPU. In aggregated, the same prefill lands in the engine that
is also decoding.

**The `4k` stream prefers aggregated on first token and EPD on every token after it.**
Aggregated's TTFT p50 of `2854.0 ms` is `1.76×` faster than EPD's `5017.0 ms` and `2.17×` faster than
prefill/decode's `6187.9 ms`, because no embedding crosses a network. After the first token the order
inverts. Aggregated's ITL p99 is `386.39 ms`, against `53.42 ms` for EPD (`7.23×` worse) and
`63.61 ms` for prefill/decode. Its TPOT p95 is `75.86 ms`, against `39.96 ms` and `38.55 ms`.

Combined throughput over the window is `135.63 tok/s` for aggregated, `129.15 tok/s` for EPD and
`119.35 tok/s` for prefill/decode. EPD gives up about `4.8%` of aggregate throughput to make the `1k`
stream's TTFT `1.39×` faster than prefill/decode and the `4k` stream's ITL p99 `7.23×` faster.

## Suite 3: repeated-image encoder-cache reuse

One shared `1792 × 1792` image, a different question per request, at two concurrencies. The image
is `4096` visual tokens of encoder work that a cache could answer once.

| Workload | Concurrency | Arm | Requests | TTFT p50 (ms) | TTFT p95 (ms) | TPOT p50 (ms) | ITL p99 (ms) | Output (tok/s) | Throughput (req/s) |
|---|---|---|---|---|---|---|---|---|---|
| `repeated_4k_c4` | 4 | Aggregated | 12/12 | 621.4 | 786.8 | 23.76 | 76.57 | 120.17 | 1.878 |
| `repeated_4k_c4` | 4 | EPD | 12/12 | 356.0 | 422.7 | 22.24 | 37.17 | 143.16 | 2.237 |
| `repeated_4k_c4` | 4 | Prefill/Decode | 12/12 | 790.6 | 848.9 | 22.25 | 28.61 | 116.51 | 1.82 |
| `repeated_4k_c8` | 8 | Aggregated | 24/24 | 1131.3 | 1347.3 | 24.45 | 110.02 | 191.4 | 2.991 |
| `repeated_4k_c8` | 8 | EPD | 24/24 | 570.2 | 693.6 | 22.5 | 46.38 | 251.92 | 3.936 |
| `repeated_4k_c8` | 8 | Prefill/Decode | 24/24 | 1242.6 | 1437.3 | 22.44 | 49.76 | 191.01 | 2.985 |

This is EPD's clearest result, and the only suite it wins outright. At concurrency 4 its TTFT p50
of `356.0 ms` is `2.22×` faster than prefill/decode and `1.75×` faster than aggregated, with the
lowest p95 (`422.7 ms`). At concurrency 8 it reaches `251.92 tok/s`, `31.9%` above prefill/decode
and `31.6%` above aggregated, and TTFT p50 is about half (`570.2 ms` against `1242.6 ms` and
`1131.3 ms`).

The cause is the encoder cache. Suite 2's heavy-vision stream has the same `4096`-token encoder input
with a unique image per request, and there EPD was the slowest arm on first token. Here the image
repeats and EPD is the fastest. `ECCPUConnector` can answer later requests from a cached embedding,
while prefill/decode re-runs the vision pass inline and aggregated re-runs it on the card that is also
decoding.

Prefill/decode has the lowest ITL p99 at concurrency 4 (`28.61 ms`). Once the embeddings are in hand,
the extra encoder leg costs little.

## Stage execution verification

A `200` does not show which engines ran, because a router that answered every request from one
replica would return the same status and similar latency. After the run, each arm's engines were
read for the access line `POST /v1/chat/completions`. For the EPD decoder, the `routing-proxy`
sidecar was read instead for `processing multimodal items`, because only the sidecar logs what
arrived from the encoder.

| Arm | Stage | Requests |
|---|---|---|
| EPD | `encode` | 264 |
| EPD | `prefill` | 264 |
| EPD | `decode` | 264 |
| EPD | `decode` sidecar | 264 multimodal items |
| Prefill/Decode | `prefill` | 430 |
| Prefill/Decode | `decode` | 264 |
| Aggregated | `server` | 530 |

The harness issued 264 requests across the three suites. EPD shows three equal counts, so every
multimodal request was encoded once, prefilled once and pulled by the decoder once.

The `430` and `530` counts exceed 264 because the marker also matches the router's token-render call,
`POST /v1/chat/completions/render`. The prefill/decode and aggregated arms have no encoder role, so
their router counts tokens by calling the engine. The EPD arm uses the router's built-in estimator,
which makes no HTTP call. This is why only those two arms need `--enable-scale-out`.

The router's prefix-aware decision also needs a prompt of at least one 16-token KV block. A shorter
prompt is scheduled to the decoder alone. Keep probe prompts past about twenty tokens when you
check stage counts through this router.

## When EPD helps and when it does not

**Use aggregated when prompts are short or concurrency is low.** It has the lowest TTFT in every
cell at concurrency `1`, and at `1k` and `2k` under load, on one card instead of three. A vision pass
of a few hundred tokens does not repay two extra network hops. Prefill/decode shows the same cost:
its TTFT p50 at `1k` and concurrency `1` is `198.6 ms`, against `99.1 ms` for aggregated.

**Use prefill/decode when decode streams must stay smooth and no image repeats.** It matches EPD on
ITL p99 in Suite 1 at concurrency 8 and beats it in Suite 3 at concurrency 4, on two cards instead of
three. Its weakness is first token on heavy vision: `6187.9 ms` on the `4k` stream, and `315.1 ms` on
the `1k` stream that shares the arm with it. Both are worse than EPD.

**Use EPD for long-prompt multimodal traffic under concurrency, or when the same image repeats.** It
is the only arm that wins on TTFT p50 and ITL p99 together, at `8k` with concurrency `4` and `8` and
across Suite 3. The encoder cache pays whenever an image repeats, as in multi-turn chat, and needs
no concurrency to show a benefit.

**Avoid EPD when every request carries a unique large image and first-token latency matters most.**
In Suite 2 it was `1.76×` slower than aggregated on the heavy-vision stream.

Splitting vision onto its own card pays when the shared card would otherwise be busy: concurrency
above one, and contexts of `4k` and above. Below those thresholds, with no repeated image, the third
role only moves embeddings that a single engine would have produced in place.

## What this run does not measure

- **Repetition.** One batch per configuration, 8 to 24 requests per cell. Treat tail differences
  below roughly 10% as unmeasured. Read the `8k` TTFT p50 of `241.8 ms` at concurrency 4 together
  with its `2279.9 ms` p95.
- **Arm order.** The aggregated arm was resident and warm while the EPD benchmark ran, and it was
  benchmarked while prefill/decode was still starting. Later arms had the warmer cluster.
- **Interconnect.** Neither node has an RDMA interface, so the KV leg ran over tcp and the encoder leg
  over NIXL on shared memory. A 100 GbE or RDMA fabric would shrink the transfer penalties that decide
  the `1k` and `2k` columns.
- **Parallelism.** TP 1 per role on one 7B model. A model that needs tensor parallelism changes what
  each role holds and what separating them buys.
- **Graph capture.** `--enforce-eager` was set on every role. This removes CUDA graphs and lowers
  decode throughput relative to a default deployment.
- **Model coverage.** One 7B vision-language model, on one engine (`vLLM`) and one platform
  (`nvidia`). The `SGLang` `nvidia` rows are verified functionally in the
  [EPD Walkthrough](../../walkthroughs/model-deployment/epd.md#sglang-ep--d-behind-both-routers).
  The `ascend` and `amd` rows of the [admitted matrix](epd.md#the-admitted-matrix) are not verified
  on hardware.
- **Cache hit rate.** Suite 3 shows what a repeated image does, not how the hit rate is computed or
  what invalidates an entry. The benefit assumes the same image arrives with the same preprocessed
  token prefix.

---

**See also** — [Model Deployment Encode, Prefill and Decode](epd.md) (the topology this run measures) ·
[Model Deployment Prefill and Decode](prefill-decode.md) (the two-role arm) ·
[Model Deployment Configuration](deployment.md) (the roles each arm declares) ·
[Model Deployment Routing](routing.md) (which replica the router picks) ·
[EPD Walkthrough](../../walkthroughs/model-deployment/epd.md) (a recorded live run).

**Next** → [Model Deployment Routing](routing.md) — choose a routing policy and inspect candidate endpoints.
