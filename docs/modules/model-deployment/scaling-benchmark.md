# Model Deployment Scaling Benchmark

Two hardware suites evaluate `ModelDeploymentScaler`. Suite 1 ran five disaggregated configurations
on one eight-H100 node. Suite 2 evaluated offline capacity priors, 18 serial capacity-arm rounds,
360-second algorithm sensitivity, embedded InferenceX priors and multi-node disaggregation across
four single-H200 nodes.

`ModelDeploymentScaler` observed load shifts, resolved offline priors and updated *`replicas`*
promptly. End-to-end scale-out latency still depends on weight loading, engine startup, graph
compilation (~76 s on H200 vLLM) and accelerator queueing outside the scaler's control.

## Contents

- [Summary](#summary)
- [Environment](#environment)
- [Configuration](#configuration)
- [Suite 1: Scaling comparison](#suite-1-scaling-comparison)
- [Suite 2: Capacity priors](#suite-2-capacity-priors)
- [Verification](#verification)
- [Benefits](#benefits)
- [Trade-offs](#trade-offs)
- [Limitations](#limitations)

## Summary

A *prefill/decode (PD)* pair splits prompt processing from token generation; an
*encode/prefill/decode (EPD)* deployment adds an image encoder. Suite 1 compared `Proportional`,
`TokenThroughput` and `Predictive` on H100; Suite 2 evaluated `Server` capacity priors and
multi-node *PD*/*EPD* on H200. [Model Deployment Scaling](scaling.md) defines the algorithms,
priors and guards.

- **Choose catalog-backed `Predictive`** for sustained traffic ramps where startup latency (~76 s)
  otherwise outlasts reactive scale-out; pair *`horizonSeconds`* with *`maxStepReplicas`* because
  ramp slopes amplify `rawRecommendedReplicas`.
- **Choose `TokenThroughput` with `0.75` catalog headroom** for steady workloads where unmargined
  saturation capacities stay under-triggered.

Failures and caveats remain: Suite 1 SGLang *PD* hit Mooncake TCP port exhaustion (4,421 errors);
the standalone lifecycle check lost 7/13,073 requests during scale-out; Suite 2 `TokenThroughput`
failed 11/1,272 requests in 360 s `sustain`; and multi-node H200 vLLM *PD* prefill scale-out hit
transient Router 503s during worker registration.

All 18 H200 single-`Server` rounds, the embedded `openai/gpt-oss-120b` check, and H200 vLLM *PD*
`Proportional` and *EPD* `Predictive` completed with zero failed requests.

## Environment

| Item | Suite 1 (H100) | Suite 2 (H200) |
|---|---|---|
| Cluster | Kubernetes v1.35.7; one CPU node and one GPU node | Kubernetes v1.35.7; one CPU node (`4` vCPU, `16` GB) and four GPU nodes |
| Hardware | 8 × NVIDIA H100 80 GB SXM GPUs; 128 vCPUs, 1600 GB RAM | 4 × NVIDIA H200 140 GB SXM GPUs (`1` GPU, `16` vCPUs, `200` GB RAM per node) |
| Text models | `Qwen/Qwen3-8B` (`b968826d9c46dd6066d109eabc6255188de91218`) | `Qwen/Qwen3-8B` (`bf16`) and `openai/gpt-oss-120b` (`fp4`, `d782fd43...`) |
| Multimodal model | `Qwen/Qwen2.5-VL-7B-Instruct` (`cc594898...`) | `Qwen/Qwen3-VL-8B-Instruct` (`0c351dd0...`) |
| Model delivery | Prefetched node cache, mounted at `/models` | Prefetched node cache, mounted at `/models` |
| vLLM image | `gpustack/runner:cuda13.0-vllm0.30.0` | `gpustack/runner:cuda13.0-vllm0.30.0` |
| SGLang image | `gpustack/runner:cuda12.9-sglang0.5.18` | `gpustack/runner:cuda12.9-sglang0.5.18` |
| Router / Service | `llm-d-router` with cluster-internal `Service` endpoints | Target-owned `Service` for `Server`; `llm-d-router` / `sglang-router` for *PD*/*EPD* |
| Load driver | In-cluster `default/bench-driver`; `case129-probe` for lifecycle | In-cluster `default/bench-driver` on the dedicated CPU node |
| GPU allocation | `bench-gpu` `InstanceType`; 1 GPU per replica, *TP* = 1 | `gpustack--nvidia-h200-linux-amd64`; 1 GPU per replica, *TP* = 1 |

Traffic went directly from the in-cluster driver to cluster-internal `Service` endpoints with no
`kubectl port-forward`. vLLM roles set `VLLM_HTTP_TIMEOUT_KEEP_ALIVE=3600`.

## Configuration

Suite 1 roles started at 1 replica with bounds `1..2`, `Active` mode and a 10 s sampling interval.

| Configuration | Model | Algorithm | Capacity settings |
|---|---|---|---|
| `vllm-pd-proportional` | Qwen3-8B | `Proportional` | `RunningRequests`, target `3` per role, tolerance `0.1` |
| `vllm-pd-token-throughput` | Qwen3-8B | `TokenThroughput` | *Prefill* `1200` tok/s; *decode* `300` tok/s per replica |
| `vllm-pd-predictive` | Qwen3-8B | `Predictive` | Same fallback targets; forecast capacity `2000` tok/s per role |
| `sglang-pd-token-throughput` | Qwen3-8B | `TokenThroughput` | *Prefill* `1200` tok/s; *decode* `300` tok/s per replica |
| `vllm-epd-predictive` | Qwen2.5-VL-7B-Instruct | `Predictive` | Same fallback and forecast capacities; *encode* coupled to *prefill* |

Predictive runs set *`horizonSeconds: 30`*, *`minPoints: 2`*, *`qLevel: "0.5"`* and
*`qTrend: "0.3"`*. Token-based runs kept default 180 s stable / 60 s panic windows, panic threshold
`2`, and latency targets 500 ms / 50 ms. All preferred a *prefill*/*decode* ratio of `1` (*EPD*
also preferred *encode*/*prefill* `1`, reaching 1 *encode*, 2 *prefill*, 2 *decode*).

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeploymentScaler
metadata:
  name: autoscale-vllm-pd
spec:
  targetRef:
    name: autoscaling-vllm-pd
  mode: Active  # commits the recommendation to the target
  sampling:
    intervalSeconds: 10  # sampling cadence
  roles:
    - name: prefill
      minReplicas: 1
      maxReplicas: 2
    - name: decode
      minReplicas: 1
      maxReplicas: 2
  ratios:
    - numeratorRole: prefill
      denominatorRole: decode
      target: "1"  # preferred ratio, not a fixed band
  algorithm:
    type: Proportional
    proportional:
      metric: RunningRequests  # requests per ready replica
      tolerance: "0.1"  # deadband around the target
      roleTargets:
        - name: prefill
          target: "3"  # requests per ready replica
        - name: decode
          target: "3"  # requests per ready replica
    # TokenThroughput replaced proportional:
    # type: TokenThroughput
    # tokenThroughput:
    #   prefillTargetTokensPerSecond: "1200"
    #   decodeTargetTokensPerSecond: "300"
    # Predictive kept those token targets and added a forecast:
    # type: Predictive
    # predictive:
    #   tokenThroughput:
    #     prefillTargetTokensPerSecond: "1200"
    #     decodeTargetTokensPerSecond: "300"
    #   horizonSeconds: 30
    #   minPoints: 2
    #   qLevel: "0.5"
    #   qTrend: "0.3"
    #   prefillTokenCapacityPerReplica: "2000"
    #   decodeTokenCapacityPerReplica: "2000"
```

See [Scaling Walkthrough](../../walkthroughs/model-deployment/scaling.md#step-1-prepare-the-target)
for the target `ModelDeployment` manifest.

### Parallel schedule

Two waves shared an eight-GPU ceiling (maximum reserved capacity per sequence, not continuous use):

| Wave | First sequence | Second sequence | GPU ceiling |
|---|---|---|---|
| 1 | vLLM *PD*: `Proportional`, reset, then `Predictive`; up to 4 GPUs | SGLang *PD*: `TokenThroughput`, release, then a separate vLLM *PD* `TokenThroughput` deployment; up to 4 GPUs | 4 + 4 = 8 |
| 2 | vLLM *EPD* `Predictive`; up to 6 GPUs | Single-role live lifecycle check; up to 2 GPUs | 6 + 2 = 8 |

Admission checks ran separately before Wave 1. Between configurations on a shared deployment, the
runner deleted the scaler, waited for binding release and restored one replica per role.

### Workload

Text runs streamed 1,024-input / up-to-256-output-token requests; multimodal runs sent fixed
request counts with unique 560-pixel images (`shared_image: false`), 1,024 text tokens and up to
256 output tokens.

| Phase | Duration (s) | Concurrency | Multimodal requests |
|---|---:|---:|---:|
| Baseline | 45 | 2 | 9 |
| Ramp | 75 | 24 | 180 |
| Sustain | 75 | 24 | 180 |
| Cooldown | 90 | 2 | 18 |

*Time to first token (TTFT)* and *inter-token latency (ITL)* percentiles use successful requests;
output throughput divides successful output tokens by recorded phase wall time.

## Suite 1: Scaling comparison

![Suite 1 H100 vLLM PD rawRecommendedReplicas, Ready replicas and TTFT p50 across baseline, ramp, sustain and cooldown](../../assets/diagrams/scaling-benchmark-suite1.svg)

On H100 vLLM *PD* (zero errors across all three algorithms), `Predictive` held `ramp` *TTFT* p50 to
**467.906 ms** versus 2,325.222 ms (`TokenThroughput`) and 2,540.853 ms (`Proportional`), with
**2,109.251 tok/s** ramp output versus 1,230.879 and 1,117.481 tok/s.

Confounder: manual token targets sat below baseline load, so `TokenThroughput` (`raw = 2`) and
`Predictive` (`raw = 7`) scaled at `B2` before `ramp` (`Predictive` entered `R1` with 2 Ready
replicas), which does not isolate forecasting alone.

### vLLM proportional PD

Configuration: `vllm-pd-proportional`.

| Phase | Requests | Errors | *TTFT* p50 (ms) | *TTFT* p99 (ms) | *ITL* p50 (ms) | *ITL* p99 (ms) | Output (tok/s) |
|---|---:|---:|---:|---:|---:|---:|---:|
| baseline | 46 | 0 | 298.564 | 408.832 | 6.412 | 11.227 | 258.325 |
| ramp | 346 | 0 | 2540.853 | 5648.778 | 9.662 | 18.393 | 1117.481 |
| sustain | 615 | 0 | 506.734 | 2704.049 | 9.525 | 17.441 | 2044.782 |
| cooldown | 98 | 0 | 188.231 | 396.106 | 6.445 | 10.531 | 274.402 |

### vLLM token throughput PD

Configuration: `vllm-pd-token-throughput`.

| Phase | Requests | Errors | *TTFT* p50 (ms) | *TTFT* p99 (ms) | *ITL* p50 (ms) | *ITL* p99 (ms) | Output (tok/s) |
|---|---:|---:|---:|---:|---:|---:|---:|
| baseline | 46 | 0 | 284.481 | 389.265 | 6.527 | 10.844 | 256.117 |
| ramp | 378 | 0 | 2325.222 | 5384.105 | 9.683 | 17.682 | 1230.879 |
| sustain | 617 | 0 | 480.418 | 4020.449 | 9.463 | 16.921 | 2054.687 |
| cooldown | 98 | 0 | 182.044 | 403.547 | 6.446 | 10.502 | 276.51 |

### vLLM predictive PD

Configuration: `vllm-pd-predictive`.

| Phase | Requests | Errors | *TTFT* p50 (ms) | *TTFT* p99 (ms) | *ITL* p50 (ms) | *ITL* p99 (ms) | Output (tok/s) |
|---|---:|---:|---:|---:|---:|---:|---:|
| baseline | 51 | 0 | 164.044 | 303.918 | 6.364 | 10.52 | 282.371 |
| ramp | 635 | 0 | 467.906 | 2099.983 | 9.333 | 16.619 | 2109.251 |
| sustain | 641 | 0 | 447.864 | 2909.804 | 9.316 | 16.683 | 2128.27 |
| cooldown | 103 | 0 | 60.883 | 364.602 | 6.356 | 10.485 | 288.484 |

### SGLang token throughput PD

Configuration: `sglang-pd-token-throughput`.

| Phase | Requests | Errors | *TTFT* p50 (ms) | *TTFT* p99 (ms) | *ITL* p50 (ms) | *ITL* p99 (ms) | Output (tok/s) |
|---|---:|---:|---:|---:|---:|---:|---:|
| baseline | 50 | 0 | 165.869 | 338.252 | 6.513 | 25.694 | 273.838 |
| ramp | 738 | 3 | 254.654 | 12393.727 | 7.15 | 28.832 | 2215.393 |
| sustain | 2899 | 2534 | 803.553 | 3629.195 | 6.969 | 27.752 | 1174.755 |
| cooldown | 1921 | 1884 | 189.41 | 256.882 | 6.426 | 25.2 | 105.225 |

### vLLM predictive EPD

Configuration: `vllm-epd-predictive`.

| Phase | Requests | Errors | *TTFT* p50 (ms) | *TTFT* p99 (ms) | *ITL* p50 (ms) | *ITL* p99 (ms) | Output (tok/s) |
|---|---:|---:|---:|---:|---:|---:|---:|
| baseline | 9 | 0 | 277.229 | 2025.801 | 6.051 | 10.031 | 201.984 |
| ramp | 180 | 0 | 1437.248 | 4623.104 | 9.457 | 18.319 | 1407.333 |
| sustain | 180 | 0 | 479.805 | 2502.224 | 7.527 | 16.049 | 1951.521 |
| cooldown | 18 | 0 | 208.498 | 266.302 | 5.968 | 10.166 | 266.625 |

SGLang failed with `Failed to get kvcache from prefill instance, it might be dead`, consistent with
[TCP port exhaustion](engine-versions.md#limitations). Its error-heavy phases do not measure
healthy capacity, and no rerun with `model-deployment-tcp-tw-reuse` was captured.

## Suite 2: Capacity priors

Suite 2 ran across four single-GPU H200 nodes plus a dedicated CPU driver node, testing offline
priors on single-`Pod` `Server` deployments before re-running the five disaggregated configurations
across multiple nodes.

### Single-replica sweep

One `Qwen/Qwen3-8B` (`bf16`, `vllm`, `1 × H200`, *TP* = 1) `Server` replica ran six 60-second
concurrency levels (`1,024/256` tokens, zero errors) to build the offline `ConfigMap` catalog.

![Suite 2 H200 single-replica total throughput vs capacity targets and TTFT percentiles by concurrency](../../assets/diagrams/scaling-benchmark-priors-sweep.svg)

Every point met P95 *TTFT* <= `2000` ms and *ITL* <= `33` ms (`ExplicitSaturation`).
`Catalog.Resolve` picked `17135.159` tok/s at `c = 32` as the seed and applied *`headroom: "0.75"`*
to derive `12851.37` tok/s, crossing measured throughput between `c = 16` (`11477.608` tok/s) and
`c = 24` (`14527.811` tok/s).

| Concurrency | Requests | Total (`tok/s`) | Output (`tok/s`) | *TTFT* p50 / p90 / p99 (ms) | *ITL* p50 / p90 / p99 (ms) | `tok/s/user` |
|---:|---:|---:|---:|---|---|---:|
| 2 | 88 | 1882.56 | 370.234 | 48.084 / 50.489 / 120.712 | 5.217 / 5.341 / 5.623 | 185.228 |
| 4 | 172 | 3653.261 | 718.409 | 58.658 / 94.228 / 98.486 | 5.291 / 5.421 / 5.768 | 179.723 |
| 8 | 296 | 6414.232 | 1261.357 | 166.364 / 190.725 / 193.563 | 5.658 / 5.796 / 6.398 | 157.755 |
| 16 | 544 | 11477.608 | 2257.225 | 224.723 / 362.343 / 384.708 | 5.774 / 5.944 / 6.838 | 141.231 |
| 24 | 672 | 14527.811 | 2857.152 | 352.587 / 527.173 / 547.8 | 6.346 / 6.556 / 9.713 | 119.205 |
| 32 | 800 | 17135.159 | 3369.918 | 490.591 / 689.956 / 736.115 | 6.576 / 6.824 / 20.012 | 105.395 |

### Capacity-arm rounds

Eighteen serial rounds (3 alternating repeats × 6 arms) compared `frozen-manual` (`20000` tok/s),
`tuned-manual` (`17135.159` tok/s) and `catalog` (`12851.37` tok/s) across nominal phase windows
`baseline` (30 s, `c = 2`), `ramp` (45 s, `c = 24`), `sustain` (45 s, `c = 24`) and `cooldown`
(30 s, `c = 0`). All 18 rounds finished with **zero failed requests**, verified UID-preconditioned
cleanup and return to 1 replica.

![Suite 2 H200 capacity-arm rawRecommendedReplicas and Ready replicas across checkpoints B1 to C3](../../assets/diagrams/scaling-benchmark-capacity-arms.svg)

Checkpoints (`B1`–`C3`) record phase progress, while scale-up elapsed times track wall clock from
ramp start (in-flight drain and harness transitions extend actual wall time past the nominal `45` s
`ramp` + `45` s `sustain` windows).

Starting from one replica on H200, unmargined `TokenThroughput` manual arms stayed at 1 replica
(`~14.5k` tok/s at `c = 24` sat below `17135.159` tok/s). `TokenThroughput` `catalog` triggered at
`R2`, and after ~76 s of vLLM startup became Ready at `S3` (`99.6–105.6` s from ramp start), costing
`+33.7%` median `readyReplicaSeconds` (`317.04` versus `237.094` s) with no `sustain` *TTFT* win
(`343.842` versus `321.352` ms).

`Predictive` triggered earlier (`15.1–21.9` s from ramp start) and reached Ready by checkpoint `R3`
(`81.9–94.6` s elapsed from ramp start), cutting `sustain` *TTFT* p50 to **189.511 ms** (`catalog`),
`194.733` ms (`frozen-manual`) and `206.453` ms (`tuned-manual`).

Latency columns show p50 / p99:

| Algorithm | Arm | Valid repeats | Ready replica-s (median/min–max) | Peak Ready | Ramp *TTFT* (ms) | Sustain *TTFT* (ms) | Sustain *ITL* (ms) |
|---|---|---:|---|---:|---|---|---|
| `TokenThroughput` | `frozen-manual` | 3 / 3 | 237.239 (232.445–309.809) | 1 | 350.355 / 551.351 | 319.235 / 529.512 | 6.349 / 9.931 |
| `TokenThroughput` | `tuned-manual` | 3 / 3 | 237.094 (234.374–304.551) | 1 | 348.907 / 548.91 | 321.352 / 517.771 | 6.345 / 9.776 |
| `TokenThroughput` | `catalog` | 3 / 3 | 317.04 (289.786–331.45) | 2 | 350.432 / 553.104 | 343.842 / 522.326 | 6.346 / 9.365 |
| `Predictive` | `frozen-manual` | 3 / 3 | 393.755 (366.408–396.625) | 2 | 336.061 / 549.38 | 194.733 / 315.066 | 5.797 / 7.679 |
| `Predictive` | `tuned-manual` | 3 / 3 | 392.089 (370.123–392.919) | 2 | 350.769 / 564.252 | 206.453 / 370.607 | 5.744 / 8.301 |
| `Predictive` | `catalog` | 3 / 3 | 423.031 (392.662–679.002) | 2 | 349.685 / 554.122 | 189.511 / 364.141 | 5.741 / 9.839 |

### Algorithm sensitivity

A 360-second H200 hardware wave (`baseline` 60 s `c = 2`, `ramp` 90 s `c = 24`, `sustain` 90 s
`c = 24`, `cooldown` 120 s `c = 0`, `maxReplicas: 4`) compared `Proportional` (`RunningRequests`
target `8`), `TokenThroughput` (`14000` tok/s) and catalog-backed `Predictive` (`12851.37` tok/s)
using `True SLO` (*TTFT* <= `2000` ms and adjacent *ITL* <= `33` ms) and `Goodput`.

Those metric definitions match the deterministic **synthetic simulation** (model output, not GPU
hardware), where `Predictive` reached `99.4%` true SLO on a five-step synthetic `ramp` seed against
`76.1%` for reactive configurations.

![Suite 2 H200 360-second hardware wave Ready replicas and TTFT p50 across Proportional, TokenThroughput and Predictive](../../assets/diagrams/scaling-benchmark-sensitivity.svg)

On the H200 hardware wave, catalog-backed `Predictive` reached 3 Ready replicas by `R2` and 4 by
`R3`, cutting `ramp` *TTFT* p50 from `347.282–350.591` ms to **192.093 ms** and `sustain` *TTFT*
p50 to **120.59 ms** (**3886.136** output tok/s, zero failed requests). Exception: `TokenThroughput`
recorded **11 failed requests** out of 1,272 in `sustain` (`True SLO 0.9914`), while `Proportional`
and `Predictive` kept `1.0` with zero failures.

| Algorithm | Phase | Requests | Failed | True SLO | Goodput (`req/s`) | *TTFT* p50 / p90 / p99 (ms) | *ITL* p50 / p90 / p99 (ms) | Output (`tok/s`) |
|---|---|---:|---:|---:|---:|---|---|---:|
| `Proportional` | `baseline` | 88 | 0 | 1.0 | 1.447 | 49.439 / 50.846 / 75.688 | 5.217 / 5.346 / 5.61 | 370.386 |
| `Proportional` | `ramp` | 1008 | 0 | 1.0 | 11.045 | 347.282 / 527.093 / 545.355 | 6.349 / 6.681 / 11.18 | 2827.554 |
| `Proportional` | `sustain` | 1244 | 0 | 1.0 | 13.618 | 190.555 / 325.042 / 338.779 | 5.75 / 6.008 / 7.864 | 3486.167 |
| `TokenThroughput` | `baseline` | 88 | 0 | 1.0 | 1.448 | 49.263 / 51.251 / 76.935 | 5.213 / 5.339 / 5.606 | 370.565 |
| `TokenThroughput` | `ramp` | 1008 | 0 | 1.0 | 11.041 | 350.591 / 527.408 / 551.491 | 6.345 / 6.693 / 10.942 | 2826.369 |
| `TokenThroughput` | `sustain` | 1272 | 11 | 0.9914 | 13.807 | 145.939 / 171.786 / 262.921 | 5.795 / 6.025 / 12.969 | 3534.61 |
| `Predictive` | `baseline` | 88 | 0 | 1.0 | 1.447 | 49.742 / 51.305 / 76.375 | 5.214 / 5.344 / 5.598 | 370.495 |
| `Predictive` | `ramp` | 1244 | 0 | 1.0 | 13.625 | 192.093 / 325.727 / 340.679 | 5.746 / 6.004 / 7.8 | 3487.99 |
| `Predictive` | `sustain` | 1383 | 0 | 1.0 | 15.18 | 120.59 / 187.599 / 195.3 | 5.596 / 5.847 / 7.21 | 3886.136 |

### Embedded catalog

An `openai/gpt-oss-120b` (`fp4`, `vllm`, `1 × H200`) `Server` deployment ran with *`capacityPrior: {}`*
and no `catalogRef` or identity annotations against the embedded catalog. It inferred
`Server/openai/gpt-oss-120b/fp4/vllm/nvidia-h200/gpus=1,tp=1,ep=1,pp=1,dp=1,ldp=1,cp=1`
(`profileDigest: sha256:47d96f0e...`), resolved `seedTokensPerSecond: "6405.44"` (`ExplicitSaturation`,
P95 *TTFT* `2000` ms, *ITL* `33` ms) and set `effectiveTokensPerSecond: "4804.08"`.

A 60-second concurrency-8 check completed `176` requests with zero errors at `4862.388` total tok/s
(`915.002` output tok/s; *TTFT* p50 / p90 / p95 `96.185 / 225.437 / 238.947` ms; *ITL* p50 / p90 /
p95 `10.958 / 11.713 / 12.014` ms), within **1.2%** of the embedded effective prior.

### Disaggregated H200 rerun

Re-running the five disaggregated configurations across four single-GPU H200 nodes (`Qwen3-8B` for
*PD*; `Qwen3-VL-8B-Instruct` in `1R1E2P1D` for *EPD*) yielded:

| Configuration | Phase | Requests | Errors | *TTFT* p50 / p99 (ms) | *ITL* p50 / p99 (ms) | Output (`tok/s`) |
|---|---|---:|---:|---|---|---:|
| `vllm-pd-proportional` | `ramp` / `sustain` | 405 / 396 | 0 / 0 | 2302.954 / 3049.929 (p99: 4451.863 / 3960.101) | 8.443 / 8.305 (p99: 17.251 / 16.239) | 1317.793 / 1277.5 |
| `vllm-pd-token-throughput` | `ramp` / `sustain` | 106047 / 379 | 105965 / 0 | 2893.8 / 3009.197 (p99: 5559.696 / 4658.621) | 8.073 / 8.122 (p99: 15.248 / 15.483) | 279.875 / 1227.049 |
| `vllm-pd-predictive` | `ramp` / `sustain` | 107447 / 364 | 107367 / 0 | 2719.156 / 3146.237 (p99: 30051.934 / 6013.353) | 8.282 / 8.309 (p99: 16.401 / 16.66) | 273.052 / 1177.547 |
| `sglang-pd-token-throughput` | `ramp` / `sustain` | 1245 / 2904 | 906 / 2904 | 2203.669 / - (p99: 3620.957 / -) | 5.723 / - (p99: 22.806 / -) | 1151.088 / 0 |
| `vllm-epd-predictive` | `ramp` / `sustain` | 180 / 180 | 0 / 0 | 943.066 / 1044.397 (p99: 3678.427 / 3493.704) | 8.285 / 8.215 (p99: 15.968 / 16.934) | 1617.037 / 1475.286 |

`vllm-pd-proportional` scaled *decode* (`1 → 2`, lead `24` s) and `vllm-epd-predictive` scaled
*prefill* (`1 → 2`) with zero errors across all four phases. When `vllm-pd-token-throughput` and
`vllm-pd-predictive` added a second *prefill* replica during high-rate text `ramp`, the Router
returned fast HTTP 503s until registration finished, then served `sustain` and `cooldown` with zero
errors.

## Verification

### Scale-out timing

Scale-out lead measures ramp start to the first sampled desired count above 1 (not Ready time). A
count already above 1 at ramp start reports `0` with `Pre-ramp count >1` set to `Yes`.

| Configuration | Lead (s) | Pre-ramp count >1 | Maximum desired counts | Maximum Ready counts |
|---|---:|---|---|---|
| `vllm-pd-proportional` (H100) | 30 | No | *Prefill* 2, *decode* 2 | *Prefill* 2, *decode* 2 |
| `vllm-pd-token-throughput` (H100) | 0 | Yes | *Prefill* 2, *decode* 2 | *Prefill* 2, *decode* 2 |
| `vllm-pd-predictive` (H100) | 0 | Yes | *Prefill* 2, *decode* 2 | *Prefill* 2, *decode* 2 |
| `sglang-pd-token-throughput` (H100) | 0 | Yes | *Prefill* 2, *decode* 2 | *Prefill* 2, *decode* 2 |
| `vllm-epd-predictive` (H100) | 0 | Yes | *Encode* 1, *prefill* 2, *decode* 2 | *Encode* 1, *prefill* 2, *decode* 2 |
| `Server` `TokenThroughput` `catalog` (H200) | 22–28 | No | *Server* 2 | *Server* 2 |
| `Server` `Predictive` `catalog` (H200) | 15–22 | No | *Server* 2 | *Server* 2 |

In Suite 2, every single-`Server` run started `ramp` at 1 Ready replica: `Predictive` committed
growth at `15.1–21.9` s versus `22.2–28.2` s for `TokenThroughput` `catalog`.

### Retirement

The standalone single-role lifecycle check verified scale-out, serving at two replicas, `Disqualified`
and `Draining` during scale-down, and serving on one survivor:

| Lifecycle phase | Requests | Errors | Observation |
|---|---:|---:|---|
| Scale-out (`up`) | 13073 | 7 | Router returned HTTP 503 with `connection termination` |
| Two replicas (`two`) | 14810 | 0 | Expanded deployment served requests |
| Scale-down (`down`) | 1880 | 0 | Retirement observed while traffic continued |
| One replica (`one`) | 942 | 0 | One Ready survivor served requests |

The earlier Wave 2 check also recorded 3 failures in 10,579 scale-out requests. Admission passed all
45 checks; the standalone lifecycle check passed 10 checks and failed its all-phases check.

Suite 1 timelines sampled `Disqualified` on proportional and token-based *PD* (plus `Admitted` and
`Aborted` on predictive *PD*) and no retirement state on *EPD*. The
[retirement protocol](runtime-control.md#pause-and-retirement) transitions `Draining` → `Deleting`
(distinct from Kubernetes `Terminating`). Before trusting live recommendations, verify `partial: false`
and *`missing[]`* via `/metrics` ([Engine signals](scaling.md#engine-signals)).

Additional runtime-acceptance runs verified:

- **vLLM multi-Server manual scaling (`1 → 2 → 1`)**: Router worker set followed every edit,
  `round_robin` split 16 requests 8/8 at two replicas, and 21/21 checks passed.
- **vLLM *External DP* retirement (*DP* = 2)**: Router held 2 → 4 → 2 workers; withdrawal reached
  the Router in 21 s and retired members deleted at 70 s (honoring the 60 s handoff window). Eight
  checks passed; one check failed from timing withdrawal against the 60 s window (case bug).
- **SGLang *PD* active scaling**: 29/29 checks passed; Mooncake moved 54 MB across six direct
  transfers with zero failures; the scaler read 24 waiting requests on *decode*, scaled *decode* to
  2, and returned both roles to 1.
- **Suite 2 single-`Server` rounds**: All 12 rounds that scaled to 2 (`3` `TokenThroughput` `catalog`
  and `9` `Predictive`) returned to 1 Ready replica with zero failed requests.

## Benefits

- In Suite 2 on H200 from an equal 1-replica baseline, `0.75` catalog headroom triggered
  `TokenThroughput` (`1 → 2` at `c = 24`) where unmargined saturation (`17135.159` tok/s) stalled,
  and the embedded `openai/gpt-oss-120b` (`fp4`) prior matched sustain throughput within **1.2%**
  (`4862.388` versus `4804.08` tok/s).
- Pairing the catalog with `Predictive` offset ~76 s vLLM startup, cutting 45 s `sustain` *TTFT* p50
  from `343.842` to **189.511 ms** (`maxReplicas: 2`) and 90 s `sustain` *TTFT* p50 from
  `145.939–190.555` to **120.59 ms** (`maxReplicas: 4`, `3886.136` output tok/s, zero errors).
- In Suite 1 on H100, predictive *PD* cut `ramp` *TTFT* p50 (`467.906` versus `2540.853` ms) and
  raised output (`2109.251` versus `1117.481` tok/s), though low manual targets scaled it during
  baseline.

## Trade-offs

- In Suite 1, 90 s cooldown sat below the 300 s downward stabilization window, leaving final
  snapshots at two replicas.
- In Suite 2's 45 s `sustain` rounds, `TokenThroughput` `catalog` spent `317.04` median
  `readyReplicaSeconds` (`+33.7%` over manual's `237.094` s) without lowering `sustain` *TTFT* p50
  because ~76 s startup consumed most of the window.
- `Predictive` amplifies initial ramp slopes (`rawRecommendedReplicas` 4–75), so production configs
  pair *`horizonSeconds`* (`180–300` s) with *`behavior.scaleUp.maxStepReplicas`*; see
  [Step 3: Choose an algorithm](../../walkthroughs/model-deployment/scaling.md#step-3-choose-an-algorithm).

## Limitations

- Suite 1 (one 8-H100 node) and Suite 2 (four 1-H200 nodes) do not establish a universal algorithm
  ranking or optimal headroom margin across workloads.
- Text and multimodal clients run different work and stopping rules, so throughput values are not
  comparable across modes. Image tags do not prove identical internal engine packages.
- Zero errors apply only to those measured requests: multi-node H200 *PD* prefill scale-out hit
  transient Router 503s during registration, and SGLang *PD* reproduced Mooncake KV transfer errors.
- See [Scaling Walkthrough](../../walkthroughs/model-deployment/scaling.md) for the boundary between
  configuration examples and captured lifecycle evidence.

---

**See also** — [Model Deployment Scaling](scaling.md) (algorithm, metrics, and guard contracts) ·
[Scaling Walkthrough](../../walkthroughs/model-deployment/scaling.md) (configuration and observation) ·
[Online Runtime Control](runtime-control.md) (retirement budgets, drain window, and role pause)

**Next** → [Scaling Walkthrough](../../walkthroughs/model-deployment/scaling.md)
