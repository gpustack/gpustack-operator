# Capacity prior provenance records

This file records provenance for `pkg/worker/scaling/priors/priors.yaml`. The catalog contains
only mode-keyed profiles with shared identity, stage or replica shapes, and `points[]`;
this ledger records upstream URLs, capture dates, SHA-256 checksums, import alignment rules, and
retirements or refusals. Do not commit upstream snapshots: keep `gen/priors/snapshots/` out of git
and put raw downloads in a temporary directory. Include a ledger entry with every catalog change.

## Alignment rules applied to accepted sources

The 2026-11-14 InferenceX sync was qualified under these rules:

- **Canonical model identity.** Every profile's `model` is the verified Hugging Face
  `<org>/<repo>` repository. Source-internal slugs (`dsr1`, `qwen3.5`, `llama70b`), ModelScope
  mirror orgs (`ZhipuAI/`, `MiniMax/`, `LLM-Research/`) and vendor-repackaged checkpoints
  (`amd/DeepSeek-R1-0528-MXFP4`, `nvidia/Llama-3.3-70B-Instruct-FP8`) adapt to the canonical
  repository before a profile is recorded; the repackaging shows up in the quantization token.
  The slug `minimaxm2.5` maps to `MiniMaxAI/MiniMax-M2.5`.
- **Aggregated mode.** Accept `disagg: false` with `prefill_num_workers <= 1`. Require at most
  2% error between system throughput and per-GPU throughput times GPU count. Rates are per replica.
  Multi-node shapes, including 16, 32 and 64 GPUs, are allowed.
- **Disaggregated mode.** Accept `disagg: true` only with positive stage worker counts and
  integer GPUs per stage worker. Require at most 2% system throughput conservation error.
  Keep each sweep's worker cohort fixed. Divide both worker counts and system rates by their
  greatest common divisor to represent one minimal coupled unit.
- **Engine families.** Map `dynamo-vllm`, `llmd-vllm` and `vllm-disagg` to `vllm`; map
  `dynamo-sglang` and `mori-sglang` to `sglang`.
- **Per-token ITL normalization.** The InferenceX collector records gaps between streamed
  messages, which can bundle more than one token. Chunk-gap percentiles divide by the measured
  mean chunk-to-token ratio (`mean_itl`/`mean_tpot`), falling back to *TPOT* quantiles when
  *ITL* is absent, to become per-token milliseconds.
- **Quantile interpolation.** `P90`/`P99` bracketing quantiles interpolate linearly to the
  configured quantile before qualification.
## Active Sources

### 2026-11-14 — Sync InferenceX aggregated and disaggregated benchmarks

This sync supersedes the 2026-10-27 record below. It retired one previously published shape:
`aggregated / deepseek-ai/DeepSeek-V4-Pro / fp4 / sglang / nvidia-b300` with
`gpusPerReplica=4, tensorParallel=4, expertParallel=1`. See the [retirement log](#retirement-log).
It added 244 shapes: 64 aggregated and 180 disaggregated. The resulting catalog has 385 profiles:
205 aggregated and 180 disaggregated. Counts below were read from the catalog.
The benchmark URLs use `https://inferencex.semianalysis.com/api/v1/benchmarks?model=<query ID>`.
The downloaded availability file from https://inferencex.semianalysis.com/api/v1/availability has
SHA-256 `43bc03ac251697770697addc3e1c2b1ff64e78e444f2ed689fec6ed73d35ef23`.

| Query ID | Rows | Aggregated | Disaggregated | SHA-256 |
|---|---:|---:|---:|---|
| `DeepSeek-R1-0528` | 1333 | 16 | 50 | `00aa63c54f0dcb71d8b3427891c74f669c800e73671f9639374f91f2127e655f` |
| `DeepSeek-V4-Pro` | 935 | 18 | 11 | `c59dbd3c1cc7a10632b7eb1207f4619ed31527ac8e106490e1a367cf08026d40` |
| `GLM-5` | 512 | 10 | 12 | `b19824dbb7a0fa07b731a99adf8e11638c2cc578bf76ee0bcbb8f23cecbfa4c3` |
| `GLM-5.2` | 154 | 0 | 0 | `381da55b7b68b5734b2f87cdee45764ddf7a08f03cff16817f1c478c3e192bde` |
| `gpt-oss-120b` | 439 | 22 | 0 | `52f3415e51a8be80ebf8e93673a444d967f1382090e41849fcb92ea26bcb7d95` |
| `Kimi-K2.5` | 381 | 18 | 10 | `f39d8be9d7c7ce81eb4ad04ec513b9a23cb995ac17735394a0bcf32d9775d6a4` |
| `Kimi-K3` | 114 | 1 | 0 | `855ffc967496da61ba888931e10d6797b8e79bff9f7d329253b9a10c37abc31a` |
| `Llama-3.3-70B-Instruct-FP8` | 681 | 31 | 0 | `d2ce48fdbd84936994d68bb28413f0e842da3b01a2c7a0958e4041699dc19792` |
| `MiniMax-M2.5` | 704 | 35 | 65 | `2d24657c2e27b03bf1d80c6ce8d4ea32296f7510e3b17f7ebbb576e3b509d85c` |
| `MiniMax-M3` | 1247 | 32 | 22 | `72b3cb6446398b5c4698679b28778a39d671774d040806607c2a6d8469466197` |
| `Qwen-3.5-397B-A17B` | 1034 | 22 | 10 | `a449e9529e7996eda73f867c0de1f4fa112e27985678595e487eeefe9fbaeff5` |
| `Qwen3.8-Flash-Next` | 77 | 0 | 0 | `30c7befeb0cb61adf6c7aa53ba81ba09a7ee402d3b55c3d10477ec0457d10a2c` |

All rows in the two zero-profile datasets used speculative decoding or unsupported engines.
Checksums were computed from the downloaded JSON files with `shasum -a 256`.

## Historical sources — InferenceX, captured 2026-10-27

These eleven upstream datasets are licensed under `Apache-2.0`. Their source URLs follow the
pattern `https://inferencex.semianalysis.com/api/v1/benchmarks?model=<query id>` and appear below.

| Upstream query ID | Canonical Hugging Face model | Rows | SHA-256 |
|---|---|---:|---|
| `DeepSeek-R1-0528` | `deepseek-ai/DeepSeek-R1-0528` | 1310 | `2b5429b85cfaf18fcf4d3ddda7a86dd9668fe565620c5afcaeb8377a9c591382` |
| `Qwen-3.5-397B-A17B` | `Qwen/Qwen3.5-397B-A17B` | 1038 | `0fbe4e56509404b3327f088d82cda782df5b6c5415829be94087452ae4a351af` |
| `DeepSeek-V4-Pro` | `deepseek-ai/DeepSeek-V4-Pro` | 934 | `3940b220207a3d2c345b333cf277ed807b1e7d9498e98ab828a9cdef6bc05b4d` |
| `MiniMax-M3` | `MiniMaxAI/MiniMax-M3` | 1245 | `fb4da7c9119438d9ab6afaea50c8a0c0cd11b131d097663060540bfd24294f10` |
| `Llama-3.3-70B-Instruct-FP8` | `meta-llama/Llama-3.3-70B-Instruct` | 681 | `d6ece93ac335193d44d6b5f8fa54339ad9c129a00ccbb25b2604330a5335de86` |
| `gpt-oss-120b` | `openai/gpt-oss-120b` | 429 | `19b429e814fe77641e83d9d03c32f44ada4f5951a9023f3cbcfa040658cf2b34` |
| `Kimi-K2.5` | `moonshotai/Kimi-K2.5` | 372 | `519e6ea065597e26ee238f7b8986fd466dad8383967d2e090fe1e6f393965c60` |
| `GLM-5.2` | `zai-org/GLM-5.2` | 125 | `fbf6cf9d7f25af65cb51adc1cd2861ad92f74b684a22e43e0ac8fafa19189339` |
| `Kimi-K3` | `moonshotai/Kimi-K3` | 114 | `fda81e4cca855a80570b3043d316d44385352a2bbd0afea9dcb01bd0a44863e3` |
| `GLM-5` | `zai-org/GLM-5` | 512 | `2145eaeea02edb566fa4067fd25c4fd01f583895af2a4098944adc0267772865` |
| `Qwen3.8-Flash-Next` | `Qwen/Qwen3.8-Flash-Next` | 35 | `5845755fcca1fc7b1d05c7fb5423ef7f67e590fab6ac65833c6f8b6c040c900a` |

Source URLs and capture dates, per row:

| Upstream query ID | Source URL | Captured | License |
|---|---|---|---|
| `DeepSeek-R1-0528` | https://inferencex.semianalysis.com/api/v1/benchmarks?model=DeepSeek-R1-0528 | 2026-10-27 | Apache-2.0 |
| `Qwen-3.5-397B-A17B` | https://inferencex.semianalysis.com/api/v1/benchmarks?model=Qwen-3.5-397B-A17B | 2026-10-27 | Apache-2.0 |
| `DeepSeek-V4-Pro` | https://inferencex.semianalysis.com/api/v1/benchmarks?model=DeepSeek-V4-Pro | 2026-10-27 | Apache-2.0 |
| `MiniMax-M3` | https://inferencex.semianalysis.com/api/v1/benchmarks?model=MiniMax-M3 | 2026-10-27 | Apache-2.0 |
| `Llama-3.3-70B-Instruct-FP8` | https://inferencex.semianalysis.com/api/v1/benchmarks?model=Llama-3.3-70B-Instruct-FP8 | 2026-10-27 | Apache-2.0 |
| `gpt-oss-120b` | https://inferencex.semianalysis.com/api/v1/benchmarks?model=gpt-oss-120b | 2026-10-27 | Apache-2.0 |
| `Kimi-K2.5` | https://inferencex.semianalysis.com/api/v1/benchmarks?model=Kimi-K2.5 | 2026-10-27 | Apache-2.0 |
| `GLM-5.2` | https://inferencex.semianalysis.com/api/v1/benchmarks?model=GLM-5.2 | 2026-10-27 | Apache-2.0 |
| `Kimi-K3` | https://inferencex.semianalysis.com/api/v1/benchmarks?model=Kimi-K3 | 2026-10-27 | Apache-2.0 |
| `GLM-5` | https://inferencex.semianalysis.com/api/v1/benchmarks?model=GLM-5 | 2026-10-27 | Apache-2.0 |
| `Qwen3.8-Flash-Next` | https://inferencex.semianalysis.com/api/v1/benchmarks?model=Qwen3.8-Flash-Next | 2026-10-27 | Apache-2.0 |

## Refused and excluded sources

| Source | Evidence | Refusal reason |
|---|---|---|
| MLPerf Inference v6.1 single-QPS Server runs (inspected NVIDIA B300x8 DeepSeek-R1 run) | Publishes TTFT and TPOT P95, no direct ITL, no same-config multi-load boundary | Offline Server throughput has no equivalent interactive SLO; TPOT constraints do not establish an ITL quantile. No eligible capacity profile. |
| NVIDIA `nv_inference_benchmark.json` (519 rows, snapshot SHA-256 `af85101e389e5b7b479c10ed2d1b3660d268dc31d031895b2bceaf5e89219351`) | No P95/P99 TTFT or ITL, no image, revision, source URL or cache-missed count fields | Missing required latency quantiles and provenance; summaries must not be converted into fabricated P95 observations. |
| ROCm ATOM dashboard (inspected Qwen3.5-397B FP8, 4x MI355X, TP4 run) | `measurement_valid: false`, precision `null`, inter-token metric measured over collector chunk gaps, engine unsupported by this operator, artifacts expire in 15 days | Unsupported engine and unverifiable measurement validity; vendor results cannot seed another vendor's capacity without lineage. |

Refusals are evidence about the inspected snapshot or run, not blanket exclusions of the
source; any future import must pin the actual data snapshot and pass the same qualification.

## Retirement log

Use `go run ./gen/priors --retire` to remove profiles from `priors.yaml`. Select them by
`--mode`, `--model`, `--quantization`, `--engine`, `--hardware` or `--tp` / `--ep` / `--gpus`.
All selectors use the same identity rules. This table records those retirements and shapes
removed because they are absent from a full upstream refresh.

| Date | Removed | Reason |
|---|---|---|
| 2026-11-14 | `aggregated / deepseek-ai/DeepSeek-V4-Pro / fp4 / sglang / nvidia-b300` (`gpus=4, tp=4, ep=1`) | The refreshed `DeepSeek-V4-Pro` snapshot (SHA-256 `c59dbd3c1cc7a10632b7eb1207f4619ed31527ac8e106490e1a367cf08026d40`) no longer contains a qualifying 4-GPU `TP=4, EP=1` sweep for this configuration. It retains the `gpus=64, tp=8, ep=8` sweep. |
