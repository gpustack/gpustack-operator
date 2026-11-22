# Scaling Internals

Evidence: contract (local synthetic evaluation summary marked as synthetic simulation). Exhaustive
`ModelDeploymentScaler` capacity-prior catalog schema, qualification and whole-unit disaggregated
formulas, identity normalization contract, persistence internals, full status schemas, and local
synthetic evaluation bounds.

## Contents

- [Capacity prior catalog](#capacity-prior-catalog)
- [Disaggregated unit math](#disaggregated-unit-math)
- [Identity normalization](#identity-normalization)
- [Prior lifecycle and persistence](#prior-lifecycle-and-persistence)
- [Status schemas](#status-schemas)
- [Local synthetic evaluation](#local-synthetic-evaluation)

## Capacity prior catalog

### Catalog schema

The embedded catalog (`pkg/worker/scaling/priors/priors.yaml`) and same-namespace `ConfigMap`
catalogs (`ModelDeploymentScalerCapacityPriorCatalog`, max `4 MiB`, `2048` profiles, `128` points
per profile; YAML or JSON; default key `priors.yaml` with fallback to legacy `priors.json`) carry
no provenance or workload metadata — evidence pins live in `pkg/worker/scaling/priors/records.md`.
Illustrative schema (resolution requires at least two distinct concurrency points per profile):

```yaml
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScalerCapacityPriorCatalog
profiles:
  - mode: aggregated
    model: deepseek-ai/DeepSeek-R1-0528  # verified Hugging Face org/repo ID
    quantization: fp8                    # canonical lowercase token
    engine: vllm                         # vllm or sglang
    hardware: amd-mi325x                 # canonical Family identifier
    topology:                            # measured replica shape
      gpusPerReplica: 8
      tensorParallel: 8
    points:                              # fixed units; values illustrative
      - concurrency: 4
        totalTokensPerSecond: 2130       # per-replica tokens/s
        inputTokensPerSecond: 1840       # per-replica tokens/s
        outputTokensPerSecond: 290       # per-replica tokens/s
        ttftMilliseconds:                # any canonical p<N> quantile in (0, 100]
          p95: 455
        itlMilliseconds:                 # milliseconds per token
          p95: 27
  - mode: disaggregated
    model: deepseek-ai/DeepSeek-R1-0528
    quantization: fp8
    engine: vllm
    prefill:
      replicas: 1
      hardware: amd-mi325x
      topology:
        gpusPerReplica: 8
        tensorParallel: 8
    decode:
      replicas: 1
      hardware: amd-mi325x
      topology:
        gpusPerReplica: 8
        tensorParallel: 8
    points:
      - concurrency: 4
        totalTokensPerSecond: 3000
        inputTokensPerSecond: 2600
        outputTokensPerSecond: 400
        ttftMilliseconds:
          p95: 455
        itlMilliseconds:
          p95: 27
```

`ParseCatalog` rejects duplicate normalized shapes (`mode`, `model`, `quantization`, `engine`, plus
`hardware` + `topology` for aggregated or stage shapes + reduced P:D ratio for disaggregated);
`gen/priors --merge` upserts one canonical sweep per shape.

### Qualification rules

- **Throughput numerators**: Cache-missed input `tokens/s` for *Prefill*, output `tokens/s` for
  *Decode*, combined `tokens/s` for *Server* (per replica for aggregated profiles; per minimal
  coupled unit for disaggregated profiles). Unpublished quantiles stay absent and are never
  synthesized from another metric.
- **Latency feasibility**: A sweep point is feasible at *`latencyQuantile`* (default `0.95`,
  nanounit precision) when measured *TTFT* and *ITL* meet targets, interpolating bracketing `p<N>`
  quantiles linearly or bounding conservatively from a single higher measured quantile.
- **Seed selection**: Across at least two distinct concurrency points, the resolver picks the
  highest feasible `tokens/s` as *`seedTokensPerSecond`* and multiplies by *`headroom`* (default
  `0.75`) to form effective capacity. A higher failed concurrency point establishes
  `HigherLoadSLOFailure`; when all points pass, the resolver caps at the highest measured point
  without extrapolating (`ExplicitSaturation`).
- **Fail-closed errors**: Missing, ambiguous, or incompatible profiles fail closed with
  `CapacityPriorUnresolved`, `InsufficientLoadCoverage`, or `IncompatibleMetric`.
- **Single-`Server` `Predictive`**: Resolves forecast capacity
  (*`predictive.*TokenCapacityPerReplica`*) and fallback capacity
  (*`predictive.tokenThroughput.*TargetTokensPerSecond`*) independently per field (`Explicit` vs
  `Catalog`).
- **Latency defaults suppressed**: Whenever any serving role omits a capacity field under present
  *`capacityPrior`*, the webhook suppresses the `500 ms` *TTFT* and `50 ms` *ITL* defaults; every
  catalog-backed role must state explicit positive latency targets (*TTFT* for *Prefill*, *ITL* for
  *Decode*, both for *Server*).

### Public data sources

| Data point | Requirement | Fallback handling |
|---|---|---|
| Model and hardware identity | Verified Hugging Face repository; canonical accelerator `Family` | Normalized by [Identity normalization](#identity-normalization); source slugs, mirrors and vendor checkpoints adapt to canonical ID |
| Load points | `concurrency`, per-replica/unit `tokens/s`, latency quantiles in ms | Unpublished quantiles stay absent; chunk-gap percentiles normalize to per-token *ITL* at import |
| Source attribution | Upstream URL, capture date, artifact SHA-256 | Recorded in `pkg/worker/scaling/priors/records.md`; unattributed numbers refused |

## Disaggregated unit math

For *PD* and *EPD* targets with *`capacityPrior`*, explicit manual capacities on both `Prefill` and
`Decode` bypass catalog lookup; partial per-role overrides are refused.

Catalog resolution requires fixed connected *`spec.ratios`* across all roles (`min == max` on every
edge, reducing to a consistent positive integer unit vector `u` with `gcd == 1`), a non-empty
multiplier interval `[k_min, k_max]` within role bounds, and positive *`ttftTargetMilliseconds`* and
*`itlTargetMilliseconds`*.

1. **Disaggregated primary**: Match `mode: disaggregated` by model, quantization, engine, stage
   hardware and topology, and reduced P:D ratio `(u_P / g, u_D / g)` where `g = gcd(u_P, u_D)`.
   Scale minimal-unit capacity by `g`. Ambiguity, mismatch, or infeasible SLOs fail closed without
   falling back to aggregated profiles.
2. **Aggregated fallback**: Only when zero disaggregated profiles match, resolve each LLM stage
   against `mode: aggregated` in the same catalog. Let `eff_P` and `eff_D` be effective per-replica
   capacities after *`headroom`*, and `g_P` and `g_D` be GPUs per replica. Compute:
   `c_gpu = min(eff_P / g_P, eff_D / g_D)` and
   `C_unit = max(1, floor((u_P * g_P + u_D * g_D) * c_gpu))`.

- **Runtime unit demand**: Adds `PromptTokensPerSecond` from `Prefill` to
  `GenerationTokensPerSecond` from `Decode`. `C_unit` is an initial GPU-time-balanced estimate, not
  a universal lower bound across arbitrary `ISL:OSL` ratios.
- **Disaggregated guards**: Latency guard reads *TTFT* from `Prefill` and *ITL* from `Decode`
  (`> 1.2×` recommends one whole unit via `LatencyGuardScaleUp`; `(1.0×, 1.2×]` holds scale-down
  via `LatencyGuardHold`). `Encode` scales strictly by fixed ratio (`RatioCoupled`).

## Identity normalization

One normalization contract governs both `priors.yaml` imports and runtime `ModelDeployment`
matching:

- **Model ID**: Trim whitespace, preserve organization and variant suffixes (`-Instruct`, `-0528`,
  `-Thinking`, `70B`), compare case-insensitively without merging distinct variants or sizes. Source
  slugs (`dsr1`, `qwen3.5`, `llama70b`), ModelScope mirrors (`ZhipuAI/`, `MiniMax/`,
  `LLM-Research/`), and vendor-repackaged checkpoints (`amd/DeepSeek-R1-0528-MXFP4`,
  `nvidia/Llama-3.3-70B-Instruct-FP8`) map to verified Hugging Face `<org>/<repo>` IDs while
  recording precision in `quantization`; unverified repos are refused.
- **Quantization tokens**: Fold to lowercase: `fp8` (`fp8_e4m3`, `fp8-e4m3`, `w8a8-fp8`), `bf16`
  (`bfloat16`), `fp16` (`float16`, `half`), `fp4` (`nvfp4`, `mxfp4`), `int8` (`w8a8`,
  `smoothquant`); keep `int4`, `awq`, `gptq` distinct. Unpinned revisions resolve as `unspecified`;
  omitted tokenizers default to the model name.
- **Hardware ID**: Reuses `AcceleratorType` `Family` rules from `pkg/nodefeature/unit_resources_preset.yaml`:
  tokenize on `-`, `_` and `.`, strip leading marketing tokens (`tesla`, `geforce`, `quadro`,
  `radeon`, `instinct`), match whole-token vendor prefixes, and keep memory-capacity tokens
  distinct (`nvidia-a100-80gb` vs `nvidia-a100-40gb`).
- **Runtime eligibility gates**: Catalog resolution requires single-serving-`Pod` managed replicas,
  explicit whole-accelerator resources without slicing, internal load balance (`DP = 1`,
  `localDP <= 1`), complete single-`Pod` metric denominator, matching engine family (`vllm`/
  `sglang`), and matching `ModelArtifact` repository/revision plus `--revision`, `--quantization`,
  and `--tokenizer*` flags. External *`spec.kvCache`*, Elastic EP, external DP, custom commands,
  and unknown injected env vars fail closed unless explicitly matched.

## Prior lifecycle and persistence

- **Admission behavior**: Absent target allows `CREATE` with a warning (GitOps order independence);
  existing target rejects unresolved priors on `CREATE`. `UPDATE` returns a warning without writing
  `status` so operators can suspend, switch to `Shadow`, edit, or remove *`capacityPrior`*.
- **Recovery before gate**: On an already-owned binding, *`pendingCohort`* timeout rollback and
  aborted retirement restoration run before the capacity-prior gate so in-flight recovery finishes.
  Before every `Active` commit, the reconciler re-reads scaler, target and catalog to confirm the
  *`compatibilityFingerprint`* is unchanged.
- **Fingerprint inputs**: Target UID, role configuration (excluding ordinary replica counts), model,
  engine, router/cache/transfer specs, identity annotations, effective algorithm, catalog digest,
  latency targets, and *`headroom`*.
- **State reset and checkpointing**: Any fingerprint change or unresolved prior clears the
  in-memory `256`-sample `RingBuffer` and persisted *`status.predictors`* and
  *`stabilizationHistory`*. Steady passes compare prior quantities by semantic `Quantity` equality
  (`1.5k == 1500`) and suppress `status` writes until control-plane state changes or a predictor
  advances by `16` samples.

## Status schemas

Illustrative status fragments (not captured output or configuration to apply).

### Decisions and recovery

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeploymentScaler
metadata:
  name: qwen-scaler
status:
  lastCommit:
    decisionID: "<decision ID>"
    term: 1
    vector:
      prefill: 3
      decode: 2
    baselineVector:
      prefill: 2
      decode: 2
    timestamp: "<commit time>"
    targetGeneration: 2
  pendingCohort:
    decisionID: "<scale-up decision ID>"
    term: 1
    baselineVector:
      prefill: 2
      decode: 2
    targetVector:
      prefill: 3
      decode: 2
    startedAt: "<scale-up commit time>"
  rebalance:
    receiverRole: prefill
    donorRole: decode
    donorTargetReplicas: 2
    receiverTargetReplicas: 3
    startedAt: "<transaction start time>"
    exhaustionEpisodeGeneration: 1
    donationAttempted: true
    reason: ReceiverAcquire
  lastScaleDown:
    role: decode
    ordinal: 2
    preScaleDownVector:
      prefill: 2
      decode: 3
    outcome: Committed  # Aborted after failed retirement is restored
    abortedAt: "<abort time, when applicable>"
    cooldownUntil: "<cooldown end, when applicable>"
```

### Predictor state

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeploymentScaler
metadata:
  name: qwen-scaler
status:
  predictors:
    - name: prefill
      level: "2400"  # estimated demand in tokens/s
      trend: "10"    # demand change per second
      covariance:
        p00: "1"  # level variance
        p01: "0"  # level-trend covariance
        p10: "0"  # trend-level covariance
        p11: "1"  # trend variance
      sampleCount: 6
      history:  # at most 256 samples within active window
        - timestamp: "<observation time>"
          tokensPerSecond: "2400"
```

### Resolved priors

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeploymentScaler
metadata:
  name: qwen-scaler
status:
  resolvedCapacityPriors:
    - name: server
      profileDigest: "sha256:<profile digest>"
      compatibilityFingerprint: "sha256:<fingerprint>"
      metric: CacheMissedTotalTokensPerSecond
      quantile: "0.9500"
      headroom: "0.75"
      seedTokensPerSecond: "2400"
      latencyBounds:
        - metric: TTFT
          quantile: "0.9500"
          conservative: false
        - metric: ITL
          quantile: "0.9500"
          conservative: false
      fields:
        - field: predictive.serverTokenCapacityPerReplica
          source: Catalog
          effectiveTokensPerSecond: "1800"
        - field: predictive.tokenThroughput.serverTargetTokensPerSecond
          source: Explicit
          effectiveTokensPerSecond: "1200"
```

In whole-unit aggregated fallback, *`status.resolvedCapacityPriors`* records one entry per LLM role
(`prefill`, `decode`) carrying each role's own `Server` seed per replica, while
*`tokenThroughput.unitTargetTokensPerSecond`* (and *`predictive.unitTokenCapacityPerReplica`* for
`Predictive`) records `C_unit`.

## Local synthetic evaluation

Evidence: synthetic simulation. The [capacity priors and local evaluation spec](https://github.com/gpustack/gpustack-operator/blob/8b0aafcf028909a95920f7fc268f03c5cecbe868/specs/2026-10-26-inferencex-model-deployment-scaling.md)
records `48` deterministic synthetic result rows across eight tuning traces (`manual`, `resolved`,
and one `passive` candidate at seed factor `1`) plus two open-loop vs closed-loop examples, and two
held-out traces (`heldout_bursts` and `heldout_length_cache_shift`, `11` rows each across `manual`
at factor `1` plus `resolved` and `passive` at seed factors `0.5`, `0.75`, `1`, `1.5` and `2`).

Online capacity calibration (`Auto`) is deferred from the production API: in the synthetic
evaluation, the passive candidate exceeded the `10%` paired replica-seconds tolerance against the
static seed control on `steady_noise` and `heldout_length_cache_shift` at seed factors `1.5` and
`2`. All rows are synthetic simulation outputs, not live hardware measurements.

---

**See also** — [Model Deployment Scaling](../docs/modules/model-deployment/scaling/index.md) (operator configuration, graceful retirement and troubleshooting) · [Model Deployment Scaling Simulation](model-deployment-scaling-simulation.md) (three-algorithm synthetic simulation) · [Model Deployment Metrics](model-deployment-metrics.md) (`/metrics` subresource and Prometheus series) · [Model Deployment Scaling Benchmark](../docs/modules/model-deployment/scaling-benchmark/index.md) (H100 and H200 measurements)
