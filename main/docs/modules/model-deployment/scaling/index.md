# Model Deployment Scaling

A `ModelDeploymentScaler` plans replica counts for one `ModelDeployment` from engine metrics.
It reports its observations and recommendations in *`status`*. In `Active` mode, it also writes
*`spec.roles[].replicas`* on the target. The `ModelDeployment` reconciler creates and retires the
serving instances that count requires.

## Contents

- [Resource](#resource)
- [Algorithms](#algorithms)
- [Bounds and ratios](#bounds-and-ratios)
- [Pacing and guards](#pacing-and-guards)
- [Resource rebalancing](#resource-rebalancing)
- [Binding fence](#binding-fence)
- [GitOps sync](#gitops-sync)
- [Status](#status)
- [Limitations](#limitations)
- [Troubleshooting](#troubleshooting)

## Resource

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeploymentScaler  # namespaced, short name mds
metadata:
  name: llm-serving
spec:
  targetRef:
    name: llm-serving  # required, immutable; ModelDeployment in this namespace
  mode: Shadow        # default; Active commits the recommendation
  suspend: false      # default; true stops planning and releases the binding
  sampling:
    intervalSeconds: 15        # default; 5 to 300 seconds
    pendingTimeoutSeconds: 300  # default; 60 to 1200 seconds
  roles:  # at least one entry; provide bounds for every target role
    - name: server  # unique role name on the target
      minReplicas: 2  # 1 to 1024; defaults to 1
      maxReplicas: 16  # required; 1 to 1024, at least minReplicas
  algorithm:  # one algorithm and only its matching parameter block
    type: Proportional
    proportional:
      metric: QueueDepth  # required; running requests plus waiting requests
      tolerance: "0.1"   # default when omitted; positive and at most 1
      roleTargets:
        - name: server  # unique scaled role name
          target: "8"   # required positive quantity; requests per ready replica
    # Alternative for a Server role; replace type and proportional above.
    # type: TokenThroughput
    # tokenThroughput:
    #   serverTargetTokensPerSecond: "2000"  # required for a Server role
    # Alternative with a forecast; replace type and proportional above.
    # type: Predictive
    # predictive:
    #   tokenThroughput:
    #     serverTargetTokensPerSecond: "2000"  # fallback capacity
    #   serverTokenCapacityPerReplica: "2000"  # forecast capacity
    # Optional offline catalog resolution for missing TokenThroughput or Predictive
    # capacities. An absent capacityPrior is manual; a present one resolves missing
    # fields from the catalog (explicit fields always win; failures fail closed).
    # capacityPrior:
    #   catalogRef:
    #     name: llm-serving-priors  # optional ConfigMap in this namespace
    #     key: priors.yaml          # default when catalogRef is present; YAML or JSON
    #   headroom: "0.75"  # default; (0, 1], applies only to catalog-derived fields
  behavior:
    scaleUp:
      stabilizationWindowSeconds: 0  # default; 0 to 600, zero disables the wait
      maxStepReplicas: 4   # default; zero or omission stores 4
      maxStepPercent: 100  # default; zero or omission stores 100
      # Optional: replace both bounds with one unit; zero prevents scale-up.
      # stepPolicy:
      #   type: Pods  # Pods or Percent
      #   value: 2    # required, nonnegative
    scaleDown:
      stabilizationWindowSeconds: 300  # default; 0 to 1800
      cooldownAfterAbortSeconds: 300   # default; 0 to 3600
      decodePostScaleUpGraceSeconds: 120  # default; 0 to 3600, Decode roles only
      # Optional: replace the default limit of one replica per pass.
      # stepPolicy:
      #   type: Percent  # Pods or Percent
      #   value: 10      # required, nonnegative; zero prevents scale-down
  rebalance:
    policy: Opposing  # default; Opposing, Priority or Disabled
```

*`targetRef.name`* selects a target in the scaler's namespace. A scaler can precede its target;
admission skips target-dependent checks while it is absent. The controller reports `TargetNotFound`
until the target appears. Retargeting requires a new scaler because the reference is immutable.

`Shadow` computes recommendations without owning the target or writing replica counts.
`Active` acquires the [Binding fence](#binding-fence) and commits decisions.
Setting *`suspend: true`* stops planning and releases an owned binding. `kubectl get mds` shows the
target, mode, algorithm, and `Bound`, `Active` and `Blocked` conditions.

*`sampling.intervalSeconds`* sets the sampling cadence and limits ordinary reaction speed.
The sampler does not scrape a target again before the interval has passed, so a shorter interval
increases scrape load. *`pendingTimeoutSeconds`* bounds the wait for scale-up
admission; [Pacing and guards](#pacing-and-guards) explains the rollback when that wait expires.

## Algorithms

*`algorithm.type`* selects `Proportional`, `TokenThroughput` or `Predictive`. Only its matching
parameter block is accepted. If the type is omitted, admission infers it from a *`proportional`*
or *`predictive`* block; otherwise it selects `TokenThroughput`.

The three algorithms differ in the signal they read and the timescale they react on. `Proportional`
compares one live measurement against a fixed target, `TokenThroughput` averages token rates over two
windows, and `Predictive` forecasts those rates ahead of the peak. The workload decides which one
fits: a request count that tracks GPU load, a compute load that request counts misstate, or traffic
that climbs before the capacity arrives.

A deterministic local simulation compares all three on identical synthetic traffic and records
replica adjustments, request outcomes and true per-token latencies in
[Model Deployment Scaling Simulation](/gpustack-operator/main/docs/modules/model-deployment/scaling-simulation/index.md). For H100 and H200 hardware results and
practical tuning guidance, see [Scaling Benchmark](/gpustack-operator/main/docs/modules/model-deployment/scaling-benchmark/index.md) and
[Step 3: Choose an algorithm](/gpustack-operator/main/docs/walkthroughs/model-deployment/scaling/index.md#step-3-choose-an-algorithm).

| Algorithm | Mechanism | Suitable workload | Main constraint |
|---|---|---|---|
| `Proportional` | One instantaneous metric (`QueueDepth`, `RunningRequests`, `WaitingRequests` or a token rate) over a fixed per-replica target, with a *`tolerance`* deadband. | Uniform-length requests (fixed-template classification, extraction, short chat) whose concurrency maps predictably to GPU load, or simple single-metric scaling. | Bursts and changing capacity can make the target inaccurate. |
| `TokenThroughput` | Two sliding windows (*`stableWindowSeconds`*, *`panicWindowSeconds`*) over prefix-cache-adjusted prompt and generation token rates, a *`panicThreshold`* burst mode, and p95 *time to first token (TTFT)* and *inter-token latency (ITL)* guards. | General LLM and *prefill/decode (PD)*-disaggregated serving with variable prompt and output lengths and prefix caching, where request count alone misstates compute load. | Sampling windows react after demand changes. |
| `Predictive` | A causal local-linear-trend Kalman filter forecasting token demand *`horizonSeconds`* ahead, taking the larger of the forecast count and the `TokenThroughput` fallback. | Sustained traffic ramps (scheduled business peaks, batch onset, multi-minute climbs) where replica initialization and weight loading ahead of the peak arrival offsets cold-start latency. | A new predictor uses the token result until enough observations arrive. |

### Proportional

The [Resource](#resource) example scales a *server* role on `QueueDepth`.
The algorithm divides the observed role total by the ready replica count, then compares that load
with *`roleTargets[].target`*. Outside *`tolerance`*, the recommendation is the ceiling of the role
total divided by its target, with a minimum of one replica. Inside the tolerance, it holds the count.

*`metric`* accepts `QueueDepth`, `RunningRequests`, `WaitingRequests`, `PromptTokensPerSecond`,
`GenerationTokensPerSecond` or `TotalTokensPerSecond`. `QueueDepth` adds running and waiting
requests. `TotalTokensPerSecond` adds prompt and generation rates. Rate targets are tokens per
second; request targets are counts per ready replica.

Each entry in *`roleTargets[]`* names a scaled role and supplies a positive target. Names must be
unique. Every scaled role needs its own target or a direct ratio to a targeted role.
*`tolerance`* defaults to `0.1` when omitted. A supplied value must be positive and at most `1`.

The proportional ratio, the tolerance deadband and the scale-down stabilization window follow the
[Kubernetes Horizontal Pod Autoscaler](https://github.com/kubernetes/website/blob/1ffcdb2a3c3a4a3ebfc23473929072262a1bfaea/content/en/docs/concepts/workloads/autoscaling/horizontal-pod-autoscale.md).

### Token throughput

This example scales an SGLang *prefill* and *decode* pair, with an *encode* role coupled to *prefill*.
It also works with vLLM: the scaler reads the target engine and needs no engine selector.

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeploymentScaler
metadata:
  name: llm-serving
spec:
  targetRef:
    name: llm-serving
  mode: Active
  roles:
    - name: encode
      minReplicas: 1
      maxReplicas: 4
    - name: prefill
      minReplicas: 2
      maxReplicas: 16
    - name: decode
      minReplicas: 2
      maxReplicas: 16
  ratios:
    - numeratorRole: prefill
      denominatorRole: decode
      target: "1"
      min: "0.5"
      max: "2"
    - numeratorRole: encode
      denominatorRole: prefill
      target: "0.5"
  algorithm:
    type: TokenThroughput
    tokenThroughput:
      prefillTargetTokensPerSecond: "2000"  # prompt capacity per Prefill replica
      decodeTargetTokensPerSecond: "800"   # generation capacity per Decode replica
      # serverTargetTokensPerSecond: "2000"  # combined capacity for a Server role
      stableWindowSeconds: 180  # default; 30 to 600 seconds
      panicWindowSeconds: 60    # default; 10 to 300 seconds
      panicThreshold: "2"       # default; positive quantity
      ttftTargetMilliseconds: "500"  # default; positive quantity
      itlTargetMilliseconds: "50"    # default; positive quantity
      latencyQuantile: "0.95"       # default; P95
```

The three token target fields describe the capacity of one replica of their role kind.
A *prefill* replica serves cache-missed prompt tokens; a *decode* replica serves generation tokens.
A *server* replica serves both. Supply a positive capacity for every scaled non-*encode* kind.
An *encode* role needs a direct ratio to a *prefill* or *decode* role, in either direction.

The algorithm averages demand over *`stableWindowSeconds`* and *`panicWindowSeconds`* separately.
It divides each average by the per-replica capacity and rounds up. Panic starts when the recent
rate reaches *`panicThreshold`* times the ready replicas' capacity. During panic, it uses the larger
recommendation and prevents scale-down. A threshold at or below `1` starts panic at or before capacity.

Keep the panic window shorter than the stable window to react to bursts sooner.
Admission validates each window's range but does not enforce their order.
The latency guard also checks *TTFT* for *prefill* and *ITL* for *decode*. A *server* role checks both
and uses the larger relative breach.

*`latencyQuantile`* selects the quantile used for online TTFT and ITL observations and for any
capacity prior. It accepts a quantity in `(0, 1]`: for example, `"0.25"` is P25, `"0.9"` is P90,
and `"0.99"` is P99. When omitted, it defaults to P95. `Predictive` uses the same field inside
its nested *`tokenThroughput`* configuration. Quantities resolve to nanounit precision; values
with finer precision are rounded by Kubernetes before the scaler uses them.

A latency above its target prevents scale-down. Above `1.2` times the target, the guard recommends
at least one more replica. Missing latency holds scale-down while allowing a throughput-driven
increase. Bounds, ratios and pacing still constrain every recommendation.

The stable and panic windows with their panic threshold follow the
[vLLM AIBrix KPA](https://github.com/vllm-project/aibrix/blob/52b8834e6223eac7ca5c050b44486aa29b44a1a6/pkg/controller/podautoscaler/algorithm/kpa.go)
and the [Knative Serving KPA](https://github.com/knative/serving/blob/knative-v1.23.0/pkg/autoscaler/scaling/autoscaler.go).

[HeteroScale](https://arxiv.org/abs/2508.19559) scales the *prefill* and *decode* pools from
cache-missed prefill throughput, decode throughput and a latency guard.
[TokenScale](https://arxiv.org/abs/2512.03416) uses a per-stage token rate as a leading indicator of
backpressure.

### Engine signals

Both engines report running and waiting requests; a *decode* role also includes
`sglang:num_decode_transfer_queue_reqs` in its waiting count.

Both engines supply prompt and generation token counters. The scaler calculates rates from counter
deltas between samples. For vLLM, it subtracts prefix-cache hit deltas from prompt-token deltas and
clamps the result at zero. Prefix-cache queries do not cap the hits used in this calculation.

For SGLang, the preferred prompt demand is the `mode="input"` delta of
`sglang:prefill_effective_tokens_total`. Without that counter in both samples, it subtracts the
`device_hit`, `host_hit` and `storage_hit` deltas when available. Without cache accounting, it uses
the prompt-token delta. A warm cache therefore reduces measured prompt demand when hits are reported.

Latency uses the engine histogram deltas across the sampled replicas. The scaler selects the bucket
boundary covering the ninety-fifth percentile. If buckets are absent or that percentile falls in
the infinite bucket, it uses the histogram mean. *`kvCacheUsage`* averages the reported fractions.

Only rank-zero or unlabelled series contribute. A restarted container or a decreasing counter
invalidates the rate window until a new baseline exists. See
[Model Deployment Metrics](/gpustack-operator/main/docs/modules/model-deployment/metrics/index.md) for the engine series and scrape configuration.

### Predictive

This example forecasts the same *prefill* and *decode* demand. The nested *`tokenThroughput`* block
accepts all the fields described under [Token throughput](#token-throughput), with the same defaults
and rules. Both fallback targets and forecast capacities are required for each scaled non-*encode* kind.

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeploymentScaler
metadata:
  name: llm-serving
spec:
  targetRef:
    name: llm-serving
  mode: Active
  roles:
    - name: prefill
      minReplicas: 2
      maxReplicas: 16
    - name: decode
      minReplicas: 2
      maxReplicas: 16
  algorithm:
    type: Predictive
    predictive:
      tokenThroughput:
        prefillTargetTokensPerSecond: "2000"
        decodeTargetTokensPerSecond: "800"
      horizonSeconds: 300  # default; 30 to 600 seconds
      prefillTokenCapacityPerReplica: "2000"  # prompt capacity per Prefill replica
      decodeTokenCapacityPerReplica: "800"   # generation capacity per Decode replica
      # serverTokenCapacityPerReplica: "2000"  # combined capacity for a Server role
      qLevel: "1"             # default process noise for demand level
      qTrend: "0.1"           # default process noise for demand trend
      measurementNoise: "10"  # default observation noise
      minPoints: 6  # default; 1 to 64 observations
```

`Predictive` uses a causal Kalman filter with a local linear trend. It forecasts demand over
*`horizonSeconds`* and divides that forecast by the role kind's per-replica forecast capacity.
The recommendation is at least the token-throughput result. Until *`minPoints`* observations arrive,
it reports `PredictorWarmingUp` in the role's *`reason`* and uses that fallback result.

*`qLevel`* and *`qTrend`* control process noise for the demand level and trend.
Higher values let those estimates change faster. *`measurementNoise`* describes uncertainty in new
observations; higher values reduce their influence. Positive values tune the filter; omitted or
nonpositive values use `1`, `0.1` and `10`, respectively.

After warm-up, panic or a latency breach above `1.2` times the target requests at least one extra
replica. Consolidation also requires that removing one replica would leave the others carrying
no more than 80% of their per-replica capacity. The scale-down step policy and other guards still apply.

The forecast follows the
[NVIDIA Dynamo Planner](https://github.com/ai-dynamo/dynamo/blob/4f42be7bebd96ea0199088840549397809f80a92/docs/fern/pages/developer-guide/knowledge-base/modular-components/planner/planner-design.md).
Its Kalman predictor estimates a local linear trend, and its load-based loop reacts to live metrics
below the predicted capacity.

### Capacity priors

*`spec.algorithm.capacityPrior`* resolves missing token capacities for `TokenThroughput` and
`Predictive` from an immutable catalog of measured profiles. Admission rejects *`capacityPrior`*
on `Proportional`. Omit it for manual mode, where every scaled non-*encode* kind requires explicit
positive token capacities. Catalog resolution fails closed, with no online fallback.

| Field | Default | Contract |
|---|---|---|
| *`capacityPrior`* | absent (manual mode) | Present: the offline catalog may resolve missing capacity fields for `TokenThroughput`/`Predictive`; explicit capacity fields always win; failures fail closed. `Proportional` rejects a present *`capacityPrior`*. |
| *`catalogRef`* | omitted (embedded catalog) | Optional *`name`* and *`key`* (`priors.yaml` default; the parser accepts YAML and JSON) for a same-namespace `ConfigMap`; sole catalog source with no fallback or merge. |
| *`headroom`* | `"0.75"` when *`capacityPrior`* is present | Quantity in `(0, 1]` multiplying catalog seed capacity; explicit capacity fields keep their configured values. The default is an initial configurable engineering margin — not a universal optimum; [Scaling Benchmark](/gpustack-operator/main/docs/modules/model-deployment/scaling-benchmark/index.md#suite-2-capacity-priors) compares `0.75` catalog headroom against unmargined manual capacities on H200 hardware, with no warranty. |

The committed catalog, `pkg/worker/scaling/priors/priors.yaml`, is human-editable YAML. The parser
also accepts JSON, so a hand-written `ConfigMap` catalog can use either format. Profiles are flat
records without provenance, serving configuration or workload details. Upstream evidence pins live
in `.agents/skills/gpustack-operator-priors/references/records.md`.

```yaml
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScalerCapacityPriorCatalog
profiles:
  - role: Server                         # Server, Prefill or Decode
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
          p50: 320
          p90: 410
          p95: 455
          p99: 512
        itlMilliseconds:                 # milliseconds per token
          p50: 18
          p90: 24
          p95: 27
          p99: 33
```

A profile is identified by its normalized `(role, model, quantization, engine, hardware,
topology)` tuple. `ParseCatalog` fails closed when two entries normalize to the same tuple —
naming both — while `gen/priors --merge` upserts one canonical sweep deterministically, sorted by
role, model, quantization, engine, hardware and parallelism widths.

Throughput units are per-replica `tokens/s`; latencies are milliseconds (`itlMilliseconds` per
token). Both latency maps accept any canonical `p<N>` quantile key in `(0, 100]` (`p25`, `p50`,
`p70`, `p95`, `p99`, `p99.9`), and unmeasured quantiles stay absent.

When any serving role omits a capacity field under a present *`capacityPrior`*, the webhook
suppresses the `500` ms and `50` ms latency defaults. Each catalog-backed role must set explicit
positive latency targets for its kind: *`ttftTargetMilliseconds`* for *Prefill*,
*`itlTargetMilliseconds`* for *Decode*, and both for *Server*. Manual mode retains the `500` ms
and `50` ms defaults.

`Predictive` resolves forecast capacity (*`predictive.*TokenCapacityPerReplica`*) and fallback
capacity (*`predictive.tokenThroughput.*TargetTokensPerSecond`*) independently per field. Setting one
explicitly records `Explicit` for that field while an omitted sibling resolves from `Catalog` and
applies *`headroom`*.

Each scaled serving role that omits a capacity field resolves independently against the catalog
by its own identity, without overwriting another role's effective values.

### Target eligibility

`Catalog` requires a verifiable target identity and a single-serving-`Pod` replica shape before
deriving capacity. The controller infers the model and hardware identity from the target itself —
*`spec.model`* (the `ModelArtifact` repository from *`spec.model.artifactRef`*, or
*`spec.model.name`*) and the role's `InstanceType` — so the identity annotations below are
optional and resolve through [identity normalization](#identity-normalization):

| Annotation | Accepted values |
|---|---|
| `worker.gpustack.ai/model-id` | Natural model name (`deepseek-ai/DeepSeek-R1-0528`); a ModelScope mirror org or vendor-repackaged checkpoint name adapts to the profile's canonical Hugging Face repository. Inferred from *`spec.model.artifactRef`* (`ModelArtifact` repository) or *`spec.model.name`* when omitted. |
| `worker.gpustack.ai/hardware-id` | Natural accelerator name or slug (`amd-mi325x`, `mi325x`, `AMD Instinct MI325X`, `nvidia-h100`). |
| `worker.gpustack.ai/prefill-hardware-id` | Optional override for *Prefill* roles on heterogeneous targets; same accepted values. |
| `worker.gpustack.ai/decode-hardware-id` | Optional override for *Decode* roles on heterogeneous targets; same accepted values. |
| `worker.gpustack.ai/precision` | Optional; read from the annotation, the engine quantization or dtype flags, or the selected profile's quantization. |

Model name and weight revision must match *`spec.model`* (or the resolved Hugging Face or ModelScope
`ModelArtifact` repository and *`status.resolved.revision`*) and any `--revision`, `--quantization`,
`--tokenizer`, `--tokenizer-path` or `--tokenizer-revision` flags. The role's `InstanceType` must be
acceleratable and match the declared product and `memoryGB` (`Gi` by semantic `Quantity` equality).

The effective engine image (*`role.image`* or *`spec.engine.image`*) matches the profile by engine
family (`vllm` / `sglang`); registry or model aliases are never inferred.

Each catalog-backed role must run as a managed single-serving-`Pod` replica (*`size`* at most `1`,
no custom *`command`* and no *`elasticEp`*). The target must omit *`spec.router`*,
*`spec.kvCache`*, *`spec.kvTransfer`* and any *`Encode`* role whenever a serving role uses
`Catalog`, and *`resources.accelerator`* must be an explicit whole-accelerator integer without
slicing or partitioning.

Supported engines are `vLLM` and `SGLang` with internal load balance, `DP = 1`, `localDP <= 1`, and
a complete single-`Pod` metric denominator.

### Catalog qualification

All prior acquisition and qualification is offline. At runtime, admission and reconciliation read
only the embedded catalog or the referenced same-namespace `ConfigMap`
(`ModelDeploymentScalerCapacityPriorCatalog`, up to 512 KiB, 256 profiles and 128 points per
profile) plus live engine metrics; the operator never calls external benchmark services or
profiles live traffic to create a prior.

A profile qualifies when its normalized `(role, model, quantization, engine, hardware, topology)`
tuple matches the serving role and its points carry the concurrency, per-replica token rates and
latency quantiles the resolver needs.

The throughput numerator follows the sampler's token convention — cache-missed input tokens/s for
*Prefill*, output tokens/s for *Decode*, their sum for *Server* — and a latency quantile the
source did not publish stays missing; it is never reconstructed from a different metric.

An unresolved match fails closed with `CapacityPriorUnresolved`, `InsufficientLoadCoverage` or
`IncompatibleMetric`; a missing or incompatible profile never supplies borrowed or zero capacity.

Resolving a prior walks one measured sweep along its two axes. As concurrency rises, per-replica
throughput — proportional to power efficiency, `tokens/s/MW` at a fixed replica power — climbs
toward hardware saturation, while per-user interactivity (`tokens/s/user` ≈ `1000 / ITL`
milliseconds) falls and *TTFT* grows.

The role's `latencyQuantile` (default `0.95`) and its `ttftTargetMilliseconds` /
`itlTargetMilliseconds` set the interactivity floor and split `points[]` into feasible and failed:
a point is feasible when the measured quantile meets both targets, interpolating bracketing
quantiles linearly or bounding conservatively from a single higher measured quantile.

Among the feasible points of a sweep with at least two distinct concurrency levels, the resolver
selects the highest per-replica `tokens/s` — the maximum of `tokens/s/MW` under the interactivity
and *TTFT* targets — as the seed, and *`headroom`* multiplies it into the effective capacity.

A higher concurrency point that breaches a target establishes `HigherLoadSLOFailure`; when every
measured point passes, the resolver never extrapolates beyond the measured concurrency and caps
conservatively at the highest measured point (`ExplicitSaturation`). The
[Scaling Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/scaling/index.md#resolving-a-catalog-prior)
traces this selection on a shipped InferenceX sweep.

The embedded catalog carries the verified public sweeps imported from InferenceX; the
`gpustack-operator-priors` skill's provenance records list every upstream dataset, its digest and
the refused sources. To use other evidence, provide a reviewed offline `ConfigMap` catalog whose
profiles match the deployment.

### Public data sources

Public benchmark sources (InferenceX and similar) qualify into *`priors.yaml`* under the
following contract. Every import pins its upstream evidence; unattributed numbers are refused.
The pins — source URLs, capture dates, artifact digests and refusal records — live in the
`gpustack-operator-priors` skill's provenance records
(`.agents/skills/gpustack-operator-priors/references/records.md`), not inside the catalog file.

| Data point | Requirement | Handling when optional, missing or non-standard |
|---|---|---|
| Model and hardware identity | Verified Hugging Face repository; canonical accelerator `Family` | Normalized by the [identity normalization](#identity-normalization) rules; source slugs, mirror orgs and vendor checkpoints adapt to the canonical identity. |
| Load points | `concurrency`, per-replica `tokens/s` throughput and latency quantiles in milliseconds | Quantiles the source did not publish stay absent; chunk-gap percentiles normalize to per-token *ITL* at import. |
| Source attribution | Upstream URL, capture date and artifact SHA-256 per dataset | Recorded in the skill's provenance records; unattributed numbers are refused. |

### Identity normalization

One yardstick normalizes identity on both sides — when importing any public source into
*`priors.yaml`* and when matching a `ModelDeployment` at runtime.

The canonical model name trims whitespace, preserves organization and variant suffixes
(`-Instruct`, `-0528`, `-Thinking`, a parameter size such as `70B`) and compares
case-insensitively; distinct model variants or parameter sizes are never merged.

The canonical model identity recorded in *`priors.yaml`* is the verified Hugging Face
`<org>/<repo>` repository — `deepseek-ai/DeepSeek-R1-0528`, `Qwen/Qwen3.5-397B-A17B`,
`meta-llama/Llama-3.3-70B-Instruct`, `openai/gpt-oss-120b`, `moonshotai/Kimi-K2.5`,
`moonshotai/Kimi-K3`, `MiniMaxAI/MiniMax-M2.5`, `MiniMaxAI/MiniMax-M3`, `zai-org/GLM-5`,
`zai-org/GLM-5.2`, `deepseek-ai/DeepSeek-V4-Pro` and `Qwen/Qwen3.8-Flash-Next` are examples.

Source-internal slugs (`dsr1`, `qwen3.5`, `llama70b`), ModelScope mirror orgs (`ZhipuAI/`,
`MiniMax/`, `LLM-Research/`) and vendor-repackaged checkpoint names
(`amd/DeepSeek-R1-0528-MXFP4`, `nvidia/Llama-3.3-70B-Instruct-FP8`) are automatically adapted to
that canonical Hugging Face ID; a vendor repackaging shows up in the quantization token, not the
model name. A source with no verified upstream repository is refused rather than guessed.

Source-specific precision labels fold into canonical lowercase tokens: `fp8` (also `fp8_e4m3`,
`fp8-e4m3`, `w8a8-fp8`), `bf16` (`bfloat16`), `fp16` (`float16`, `half`), `fp4` (`nvfp4`,
`mxfp4`), `int8` (`w8a8`, `smoothquant`), and `int4`, `awq` and `gptq` kept distinct. An unpinned
revision resolves as `unspecified`; an omitted tokenizer defaults to the model name.

`hardware-id` normalization reuses the accelerator `Family` rules and table from
[Instance Type Unit Resources](/gpustack-operator/main/docs/reference/instance-type-unit-resources/index.md)
(`pkg/nodefeature/unit_resources_preset.yaml`).

Product strings tokenize on `-`, `_` and `.`, leading vendor and marketing tokens (`tesla`,
`geforce`, `quadro`, `radeon`, `instinct`) are stripped, whole-token prefixes are matched per
vendor, and capacity tokens (`80gb`, `40gb`, `32gb`) refine multi-capacity families such as
`nvidia-a100-80gb` versus `nvidia-a100-40gb`.

At runtime the same yardstick applies: the `worker.gpustack.ai/model-id` and
`worker.gpustack.ai/hardware-id` annotations accept the natural names above and are inferred from
*`spec.model`*, the engine flags and the role's `InstanceType` when omitted. See
[Target eligibility](#target-eligibility) for the full resolution order.

### Prior lifecycle

When the target `ModelDeployment` is absent, admission allows the scaler with a warning so GitOps
manifests can apply in any order. When the target exists, `CREATE` rejects an unresolved catalog
prior; API read errors propagate directly.

`UPDATE` admission returns a warning without writing `status` so operators can suspend, switch to
`Shadow`, remove *`capacityPrior`* or repair capacity fields. On reconciliation, the controller sets
`CapacityPriorsReady=False`, `Active=False` and `Blocked=True` (eventual consistency) and stops new
replica commits.

On an already-owned binding, *`pendingCohort`* timeout rollback and aborted retirement restoration
still run before the capacity-prior gate so in-flight recovery can finish. Before committing an
`Active` replica write, the reconciler re-reads the scaler, target and catalog to verify that the
compatibility fingerprint is unchanged.

Each resolved role records a *`compatibilityFingerprint`* covering target UID, role configuration
(excluding ordinary replica count changes), model, engine, router/cache/transfer specs, identity
annotations, effective algorithm, catalog digest, latency targets and headroom.

Any fingerprint change or unresolved prior clears both the in-memory sampler window and persisted
*`status.predictors`* and *`stabilizationHistory`* across restarts. Steady passes compare resolved
prior quantities by semantic `Quantity` equality, keep the 256-sample `RingBuffer` in memory, and
suppress `status` writes until control-plane state changes or a predictor advances by 16 samples.

### Local evaluation

The [capacity priors and local evaluation spec](https://github.com/gpustack/gpustack-operator/blob/a4b463899a64d9e79c3b3c599f470f79f6701a09/specs/2026-10-26-inferencex-model-deployment-scaling.md)
records 48 deterministic synthetic result rows across eight tuning traces (`manual`, `resolved` and
one `passive` candidate at seed factor `1`) plus two open-loop versus closed-loop examples.

It also evaluates two held-out traces (`heldout_bursts` and `heldout_length_cache_shift`, 11 rows
each across `manual` at factor `1` plus `resolved` and one `passive` candidate at seed factors
`0.5`, `0.75`, `1`, `1.5` and `2`). Those rows are synthetic simulation outputs, not live hardware
measurements.

Online capacity calibration (`Auto`) is deferred from the production API. In the synthetic
evaluation, the passive candidate showed mixed latency-SLO and cost trade-offs, and on
`steady_noise` plus `heldout_length_cache_shift` at seed factors `1.5` and `2` it exceeded the
`10%` paired replica-seconds tolerance against the static seed control (paired excess is reported
against the named static manual control with no optimal oracle claimed).

For offline validation or live execution of the single-*Server* comparison across `frozen-manual`,
`tuned-manual` and `catalog` (`case-5-autoscaling.sh --capacity-comparison`), see
[Capacity comparison](/gpustack-operator/main/docs/walkthroughs/model-deployment/scaling/index.md#capacity-comparison).

## Bounds and ratios

*`roles[]`* gives each target role a unique *`name`*, a *`minReplicas`* floor and a *`maxReplicas`*
ceiling. Every target role needs bounds; an incomplete set blocks planning with `BoundsConflict`.
The minimum defaults to `1`. Both counts range from `1` to `1024`, with the minimum no greater than
the maximum. The scaler cannot scale to zero.

*`ratios[]`* connects roles through *`numeratorRole`* and *`denominatorRole`*.
Both must name distinct scaled roles, and each ordered pair must be unique. Optional *`min`* and
*`max`* bound the numerator count divided by the denominator count. Optional *`target`* is the
preferred ratio. Supplied quantities must be positive; the target must lie within any supplied bounds.

Setting *`min`*, *`target`* and *`max`* to the same value fixes a ratio. A target without bounds is
a preference; bounds without a target are constraints. The [Token throughput](#token-throughput)
example allows *prefill* to *decode* ratios from `0.5` to `2`, prefers `1`, and couples *encode* to
*prefill* with a preferred ratio of `0.5`.

The planner applies bounds, pacing, guards and ratios to one replica vector and commits it atomically.
For connected roles, it seeks a feasible vector close to the recommendations and uses ratio targets
to break ties. An upward recommendation gives the component an upward direction; otherwise it may
shrink. If no feasible vector is found, that component holds its current counts.

The ratio comparison tolerates small rounding differences at a boundary. Admission checks feasibility
with exact integer ratios and rejects configurations it proves impossible. A search that reaches its
limit can admit an undecided configuration. Admission success therefore does not guarantee that
planning can find a feasible vector for every combination of pacing and guards.

The current replica vector must already satisfy the bounds and ratio bands. If it does not,
planning stops with `BoundsConflict` or `RatioConflict`; adjust the policy to include the current
vector before expecting the scaler to move it.

## Pacing and guards

*`behavior.scaleUp.stabilizationWindowSeconds`* waits for recommendation history spanning the window
before allowing growth. Zero disables that wait. The scale-down window holds the highest recent
recommendation as its floor and prevents shrinkage until enough history exists. Recommendation
history is kept in memory and reported in status, with at most 256 samples per role.

Without an upward *`stepPolicy`*, one pass can add the larger of *`maxStepReplicas`* replicas and
*`maxStepPercent`* of the current count, rounded up. The former accepts nonnegative integers; the
latter ranges from `0` to `1000`. Omission or zero stores defaults of `4` and `100`, respectively.
Setting one to zero does not disable that bound. Use a step policy to select one unit alone.

A *`stepPolicy`* under either *`scaleUp`* or *`scaleDown`* requires *`type`* (`Pods` or `Percent`)
and a nonnegative *`value`*. `Pods` counts serving replicas, even when each replica has several
`Pod`s. `Percent` uses the current replica count and rounds up. Zero prevents movement in that
direction. Without a downward policy, a pass removes at most one replica per role.

The downward *`stabilizationWindowSeconds`*, *`cooldownAfterAbortSeconds`* and
*`decodePostScaleUpGraceSeconds`* use their defaults when omitted or zero: `300`, `300` and `120`
seconds. Zero does not disable these guards. Their accepted ranges are shown in the
[Resource](#resource) example.

The post-scale-up grace keeps a *decode* role's replica count stable while added replicas settle,
which matters in the disaggregated arrangements described by
[Mooncake](https://arxiv.org/abs/2407.00079).

Scale-down also waits when any of these conditions holds:

- The role has no ready replica or the observation has incomplete coverage.
- The algorithm holds scale-down, including for panic or missing required signals.
- A retirement is active anywhere in the target, or an abort cooldown is active.
- A *decode* role is within its post-scale-up grace period.

A pending unadmitted workload prevents further growth. While a committed scale-up cohort is pending,
that ceiling applies across the target. If any added ordinal remains unadmitted past
*`sampling.pendingTimeoutSeconds`*, the scaler restores the whole cohort's baseline vector in one
write and reports `ResourceExhausted`.

If a removed ordinal's retirement ends as `Aborted`, the scaler restores the pre-scale-down vector.
It reports `ScaleDownAborted`, cancels any associated donor transaction, and starts
*`cooldownAfterAbortSeconds`*. This cooldown prevents further scale-down across the target.

## Resource rebalancing

Rebalancing starts after a scale-up times out and restores its baseline. If the receiver still
needs to grow, *`rebalance.policy`* decides whether another role can release capacity.

| Policy | Donor selection |
|---|---|
| `Opposing` | Default. A donor's own recommendation must be to shrink. |
| `Priority` | A lower-priority role may donate even when its own recommendation is to hold. |
| `Disabled` | No donation is attempted. |

```yaml
spec:
  rebalance:
    policy: Priority
    priority:  # unique scaled role names, highest priority first
      - decode
      - prefill
      - encode
```

*`priority[]`* only affects `Priority`. A receiver may take from roles after it in the list,
starting with the next listed role. Unlisted roles neither donate nor receive through this policy;
they retain their ordinary recommendations. The donor and receiver must use the same nonempty
*`instanceType`*, and the donor must remain above its minimum before donation.

The `Opposing` policy follows the
[Dynamo Budget Guard](https://github.com/ai-dynamo/dynamo/blob/4f42be7bebd96ea0199088840549397809f80a92/components/src/dynamo/planner/core/budget.py),
which moves both legs of an opposing rebalance together or leaves both in place.

The transaction removes one donor replica first. It waits for retirement to finish and the donor's
`Workload` to disappear before adding one receiver replica. Both intermediate vectors must satisfy
bounds and ratios. An aborted donor retirement restores the previous vector and cancels the transaction.

`RebalanceNotApplicable` means no eligible donor was found, including when pools differ.
`RebalanceBlocked` means an intermediate vector violates policy or a `Priority` donation was already
attempted during the exhaustion episode. A scaler spec edit or an external target generation change
can start a new episode; the scaler's own committed generation does not reset that protection.

## Binding fence

An active scaler owns the target's replica counts through two annotations:

| Annotation | Payload |
|---|---|
| `modeldeployment.gpustack.ai/scaler-binding` | *`scalerName`*, *`scalerUID`* and the binding *`term`*. |
| `modeldeployment.gpustack.ai/scaler-decision` | *`decisionID`*, *`term`* and the committed replica *`vector`*. |

The webhook refuses user changes to a bound target's replica counts or either annotation.
Other fields remain editable. On an unbound deployment, users may change counts but cannot install
either ownership annotation. An operator commit writes the vector and its decision together under
the binding's term. Terms start at one and cannot move backwards.

A second scaler cannot take over an existing binding. It reports `AlreadyBound` and leaves the
owner in place. `Shadow` and suspended scalers release bindings they own. If a target is deleted and
recreated with a new UID, `TargetReplaced` stops planning until a scaler spec edit acknowledges the
replacement. The scaler then clears target-specific state and can bind the new deployment.

A binding annotation that cannot be parsed leaves the target unclaimed. The scaler reports
`InvalidBinding` on `Bound` and `Active` and plans nothing, because it cannot tell which scaler
owns the target. Fix or remove the annotation to let a scaler bind again.

Deleting a scaler normally releases its binding through its finalizer and leaves the target serving.
If an orphaned binding remains, a user may remove that annotation only when the named scaler is
absent or has a different UID. That recovery write must leave replica counts unchanged.

## GitOps sync

Configure the GitOps controller to ignore the target's replica counts and both scaler annotations.
Otherwise, admission rejects attempts to restore the counts from Git. Each role needs its own
replica pointer; `~1` escapes the slash inside an annotation key.

Argo CD reads *`ignoreDifferences`* from the `Application`:

```yaml
spec:
  ignoreDifferences:
    - group: worker.gpustack.ai
      kind: ModelDeployment
      name: llm-serving
      namespace: default
      jsonPointers:
        - /spec/roles/0/replicas
        - /metadata/annotations/modeldeployment.gpustack.ai~1scaler-binding
        - /metadata/annotations/modeldeployment.gpustack.ai~1scaler-decision
  syncPolicy:
    syncOptions:
      - RespectIgnoreDifferences=true
```

`RespectIgnoreDifferences=true` also applies the exclusions during sync. Without it, differences
can be hidden in the comparison while a sync still attempts to restore the declared counts.

Flux versions that support *`spec.ignore`* on `Kustomization` use these paths:

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
spec:
  ignore:
    - target:
        group: worker.gpustack.ai
        kind: ModelDeployment
        name: llm-serving
        namespace: default
      paths:
        - /spec/roles/0/replicas
        - /metadata/annotations/modeldeployment.gpustack.ai~1scaler-binding
        - /metadata/annotations/modeldeployment.gpustack.ai~1scaler-decision
```

## Status

Status records observations, decisions and recovery state. The following fragments illustrate the
field shapes; they are not captured output or configuration to apply. Optional entries appear only
when the scaler has corresponding state. *`observedGeneration`* identifies the scaler spec generation
used for the report.

The scaler writes its status when its control plane moves: a decision, a binding, a condition or
the replica counts of a role. A scaler that is steady writes nothing, so the sliding windows it
holds are a snapshot rather than a live feed. The windows themselves are kept in memory and grow
on every sampling pass, whether the status was written or not, and the scaler reads them back
from the status when it starts planning again.

### Role observations

```yaml
status:
  observedGeneration: 1
  binding:
    targetUID: "<target UID>"  # distinguishes a replacement with the same name
    targetName: llm-serving
    term: 1  # term used for active commits; Shadow may have no term
  roles:
    - name: prefill
      currentReplicas: 2  # desired count currently on the target
      readyReplicas: 2   # ready serving replicas used to calculate capacity
      signalState: Fresh
      promptTokensPerSecond: "2400"  # cache-missed rate across sampled replicas
      generationTokensPerSecond: "0"
      runningRequests: 8
      waitingRequests: 2
      kvCacheUsage: "0.5"  # mean reported cache fraction
      ttftMilliseconds: "400"
      itlMilliseconds: "0"
      rawRecommendedReplicas: 2  # algorithm result
      constrainedReplicas: 2     # result after bounds, ratios, pacing and guards
      reason: TokenThroughput    # algorithm or constraint explanation
      lastScaleUpTime: "<last growth time>"
      stabilizationHistory:  # at most 256 recommendations
        - timestamp: "<sample time>"
          replicas: 2
```

*`signalState`* is `Fresh` for current readings, `Idle` for a successfully observed role with no work,
`Missing` for unavailable signals or an unestablished window, and `Stale` for expired readings.
A missing signal differs from a measured zero. Incomplete coverage prevents scale-down, although
usable partial observations can still justify growth.

*`reason`* can explain an algorithm decision, such as `WithinTolerance`, `Panic`, `NoReadyReplicas`,
`LatencyGuardHold`, `LatencyGuardScaleUp`, `PredictorWarmingUp`, `PredictiveConsolidationHold` or
`RatioCoupled`. A constraint reason can replace it. Compare *`rawRecommendedReplicas`* and
*`constrainedReplicas`* to see how the planner changed the algorithm's request.

### Decisions and recovery

```yaml
status:
  lastCommit:
    decisionID: "<decision ID>"
    term: 1
    vector:  # committed replica counts by role
      prefill: 3
      decode: 2
    baselineVector:  # counts before this commit
      prefill: 2
      decode: 2
    timestamp: "<commit time>"
    targetGeneration: 2  # target generation expected after the write
  pendingCohort:
    decisionID: "<scale-up decision ID>"
    term: 1
    baselineVector:  # restored if admission times out
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
    exhaustionEpisodeGeneration: 1  # identifies the exhaustion episode
    donationAttempted: true        # guards repeated donations
    reason: ReceiverAcquire
  lastScaleDown:
    role: decode
    ordinal: 2  # zero-based ordinal removed from this role
    preScaleDownVector:  # restored after an aborted retirement
      prefill: 2
      decode: 3
    outcome: Committed  # Aborted after a failed retirement is restored
    abortedAt: "<abort time, when applicable>"
    cooldownUntil: "<cooldown end, when applicable>"
```

*`lastCommit`* identifies the latest actual write and the target generation it expects.
*`pendingCohort`* tracks added ordinals until admission or rollback.
*`lastScaleDown.outcome`* is `Committed` while retirement runs and becomes `Aborted` after a failed
retirement is restored. The abort and cooldown times record that recovery.

*`rebalance.reason`* records `ResourceExhausted`, `DonorRelease`, `ReceiverAcquire` or
`DonationAttempted`, or an applicability/blocking reason. Its donor and receiver targets describe
the two-step transaction. *`exhaustionEpisodeGeneration`* identifies the episode, and
*`donationAttempted`* records whether it has already used a donation attempt.

### Predictor state

```yaml
status:
  predictors:
    - name: prefill  # role this state belongs to
      level: "2400"  # estimated demand in tokens per second
      trend: "10"    # demand change per second
      covariance:
        p00: "1"  # level variance
        p01: "0"  # covariance of level with trend
        p10: "0"  # covariance of trend with level
        p11: "1"  # trend variance
      sampleCount: 6  # observations processed by the predictive filter
      history:  # recent demand samples within the active window, at most 256
        - timestamp: "<observation time>"
          tokensPerSecond: "2400"
```

*`predictors[]`* persists demand history for `TokenThroughput` and filter state for `Predictive`.
The *`level`*, *`trend`* and *`covariance`* fields describe the filter estimate and its uncertainty.
The scaler restores this state from status when it resumes planning, keeps the 256-sample window in
memory while it plans, and checkpoints steady predictor progress to `status` every 16 samples.

### Resolved priors

```yaml
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

*`resolvedCapacityPriors[]`* records per-role catalog provenance and per-field effective capacities
whenever `Catalog` is active. *`fields[]`* distinguishes `Explicit` spec fields from `Catalog`
fields derived by multiplying *`seedTokensPerSecond`* by *`headroom`*. *`latencyBounds[]`* records
the quantile statistic used for each required latency metric and whether a higher quantile bounded
`0.95` conservatively.

Quantities in *`resolvedCapacityPriors[]`* (*`headroom`*, *`seedTokensPerSecond`* and
*`effectiveTokensPerSecond`*) compare by semantic `Quantity` equality (`1.5k` equals `1500`).
All sample status numbers, capacities and digests above are illustrative schema examples and do
not represent measured hardware capacity.

### Conditions

```yaml
status:
  conditions:
    - type: MetricsReady  # Bound, Active, MetricsReady, CapacityPriorsReady or Blocked
      status: "True"      # True, False or Unknown
      reason: Observed
      message: all required role signals are available
      observedGeneration: 1  # scaler generation used for this condition
      lastTransitionTime: "<last condition transition time>"
```

`Bound=True` means the target is available to this scaler. In `Active` mode, it also holds the
binding. A `Shadow` scaler can report `Bound=True` without an ownership annotation.
`Active` reports permission to commit, `MetricsReady` reports live engine signal coverage,
`CapacityPriorsReady` reports catalog capacity resolution, and `Blocked` reports planning
constraints. Inspect each condition's generation because early exits can leave older conditions.

| Condition | Success reason | Other reasons |
|---|---|---|
| `Bound` | `Bound` | `TargetNotFound`, `AlreadyBound`, `TargetReplaced`, `InvalidBinding`, `Suspended` |
| `Active` | `Active` | `TargetNotFound`, `AlreadyBound`, `TargetReplaced`, `InvalidBinding`, `Suspended`, `ShadowMode`, `CapacityPriorUnresolved`, `InsufficientLoadCoverage`, `IncompatibleMetric` |
| `MetricsReady` | `Observed` | `WaitingForMetrics`, `ResourceExhausted`, `BoundsConflict`, `RatioConflict` |
| `CapacityPriorsReady` | `Resolved`, `Disabled` | `CapacityPriorUnresolved`, `InsufficientLoadCoverage`, `IncompatibleMetric`, `TargetNotFound`, `TargetReplaced`, `InvalidBinding`, `Suspended` |
| `Blocked` | `Ready` (`False`) | `BoundsConflict`, `RatioConflict`, `PacingBounded`, `ResourceExhausted`, `ScaleDownAborted`, `RebalanceNotApplicable`, `RebalanceBlocked`, `CapacityPriorUnresolved`, `InsufficientLoadCoverage`, `IncompatibleMetric` |

`BoundsConflict` means bounds are missing, inconsistent or exclude the current vector.
`RatioConflict` means the current vector violates a ratio band or no feasible projection was found.
`PacingBounded` means a step limit or guard held the recommendation.

`WaitingForMetrics` means required readings or complete coverage are unavailable, and
`PredictorWarmingUp` is a role reason. `CapacityPriorUnresolved`, `InsufficientLoadCoverage` and
`IncompatibleMetric` report why a catalog prior could not be resolved.

## Limitations

Admission refuses roles with a custom *`command`* because their serving signals are unmanaged.
It also refuses *`elasticEp`* roles, whose width grows one collective rather than adding independent
replicas. At *`maxReplicas`*, generated group and member names must still fit the 63-character
Kubernetes label limit. Shorten the deployment or role name if that maximum cannot be represented.

The sampler reads at most 64 ready answering engine `Pod`s per role. Above that cap, coverage is
incomplete and scale-down is held. New token and latency windows also need successive valid samples;
a first observation alone cannot establish rates.

A disaggregated scale-down follows the [Retirement protocol](/gpustack-operator/main/docs/modules/model-deployment/shutdown/index.md#retirement-protocol) and
[Disaggregated replicas](/gpustack-operator/main/docs/modules/model-deployment/shutdown/index.md#disaggregated-replicas) handoff window. That window bounds the
handoff wait; it does not wait indefinitely for every prefill request to finish.

If a prefill lasts longer than the handoff window, a *decode* replica can leave while that prefill
is still running, losing its request. For workloads with prefills that run for minutes, keep the
*decode* *`minReplicas`* high enough to preserve the required serving capacity.

Both resource webhooks use `failurePolicy: Fail`. If the webhook `Pod` is unavailable, writes that
require it fail and are retried. Existing replicas keep serving. A webhook outage does not allow
unchecked writes through the binding fence.

## Troubleshooting

| Symptom | Signal | Check & Mitigation |
|---|---|---|
| Counts never change | Condition: `Active` | Use `Active` mode and clear suspension when ready to commit. Resolve missing, replaced or already-owned targets. |
| Counts never change | Condition: `MetricsReady`, Status: role `signalState` | Check engine metrics and allow a valid sampling window to form. |
| Counts never change | Condition: `CapacityPriorsReady` | Verify `catalogRef`, the target identity (annotations or inferred model/hardware identity), engine-family image, single-`Pod` topology and explicit latency targets, or supply explicit capacities / remove *`capacityPrior`*. |
| Counts never change | Condition: `Blocked`, Status: role `reason` | Correct bounds or ratios, or wait for the named pacing or retirement guard. |
| Recommendation differs from counts | Status: *`rawRecommendedReplicas`* and *`constrainedReplicas`* | Check the role bounds, ratios and pacing limits that changed the request. |
| Counts return to a baseline | Status: *`pendingCohort`*, Reason: `ResourceExhausted` | Check admission and available quota before retrying growth. |
| Counts return after shrinking | Status: *`lastScaleDown`* | Resolve the aborted retirement before its cooldown expires. |
| A write is refused | Admission message | Check [Binding fence](#binding-fence) and [GitOps sync](#gitops-sync). An orphan recovery requires the named scaler to be gone or replaced. |

---

**See also** — [Configuration](/gpustack-operator/main/docs/modules/model-deployment/deployment/index.md) (roles and counts) · [Benchmark](/gpustack-operator/main/docs/modules/model-deployment/scaling-benchmark/index.md) (H100 and H200 results) ·
[Simulation](/gpustack-operator/main/docs/modules/model-deployment/scaling-simulation/index.md) (synthetic comparison) ·
[Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/scaling/index.md) (configure and observe) ·
[Status](/gpustack-operator/main/docs/modules/model-deployment/status/index.md) (phase and conditions) · [Shutdown](/gpustack-operator/main/docs/modules/model-deployment/shutdown/index.md) (retirement) ·
[Metrics](/gpustack-operator/main/docs/modules/model-deployment/metrics/index.md) (engine signals).

**Next** → [Model Deployment Status](/gpustack-operator/main/docs/modules/model-deployment/status/index.md) — the target's status and scaler annotations.
