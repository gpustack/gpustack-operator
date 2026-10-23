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
[Model Deployment Metrics](metrics.md) for the engine series and scrape configuration.

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
The scaler restores this state from status when it resumes planning, and keeps it in memory while
it plans, so the predictor warms up without a status write per pass.

### Conditions

```yaml
status:
  conditions:
    - type: MetricsReady  # Bound, Active, MetricsReady or Blocked
      status: "True"      # True, False or Unknown
      reason: Observed
      message: all required role signals are available
      observedGeneration: 1  # scaler generation used for this condition
      lastTransitionTime: "<last condition transition time>"
```

`Bound=True` means the target is available to this scaler. In `Active` mode, it also holds the
binding. A `Shadow` scaler can report `Bound=True` without an ownership annotation.
`Active` reports permission to commit, `MetricsReady` reports signal coverage, and `Blocked`
reports planning constraints. Inspect each condition's generation because early exits can leave older conditions.

| Condition | Success reason | Other reasons |
|---|---|---|
| `Bound` | `Bound` | `TargetNotFound`, `AlreadyBound`, `TargetReplaced`, `InvalidBinding`, `Suspended` |
| `Active` | `Active` | `TargetNotFound`, `AlreadyBound`, `TargetReplaced`, `InvalidBinding`, `Suspended`, `ShadowMode` |
| `MetricsReady` | `Observed` | `WaitingForMetrics`, `ResourceExhausted`, `BoundsConflict`, `RatioConflict` |
| `Blocked` | `Ready` (`False`) | `BoundsConflict`, `RatioConflict`, `PacingBounded`, `ResourceExhausted`, `ScaleDownAborted`, `RebalanceNotApplicable`, `RebalanceBlocked` |

`BoundsConflict` means bounds are missing, inconsistent or exclude the current vector.
`RatioConflict` means the current vector violates a ratio band or no feasible projection was found.
`PacingBounded` means a step limit or guard held the recommendation. `WaitingForMetrics` means
required readings or complete coverage are unavailable. `PredictorWarmingUp` is a role reason.

## Limitations

Admission refuses roles with a custom *`command`* because their serving signals are unmanaged.
It also refuses *`elasticEp`* roles, whose width grows one collective rather than adding independent
replicas. At *`maxReplicas`*, generated group and member names must still fit the 63-character
Kubernetes label limit. Shorten the deployment or role name if that maximum cannot be represented.

The sampler reads at most 64 ready answering engine `Pod`s per role. Above that cap, coverage is
incomplete and scale-down is held. New token and latency windows also need successive valid samples;
a first observation alone cannot establish rates.

A disaggregated scale-down follows the [Retirement protocol](shutdown.md#retirement-protocol) and
[Disaggregated replicas](shutdown.md#disaggregated-replicas) handoff window. That window bounds the
handoff wait; it does not wait indefinitely for every prefill request to finish.

If a prefill lasts longer than the handoff window, a *decode* replica can leave while that prefill
is still running, losing its request. For workloads with prefills that run for minutes, keep the
*decode* *`minReplicas`* high enough to preserve the required serving capacity.

Both resource webhooks use `failurePolicy: Fail`. If the webhook `Pod` is unavailable, writes that
require it fail and are retried. Existing replicas keep serving. A webhook outage does not allow
unchecked writes through the binding fence.

## Troubleshooting

| Symptom | Check | Action |
|---|---|---|
| Counts never change | `Active` condition | Use `Active` mode and clear suspension when ready to commit. Resolve missing, replaced or already-owned targets. |
| Counts never change | `MetricsReady` and role signal states | Check engine metrics and allow a valid sampling window to form. |
| Counts never change | `Blocked` and role reasons | Correct bounds or ratios, or wait for the named pacing or retirement guard. |
| Recommendation differs from counts | *`rawRecommendedReplicas`* and *`constrainedReplicas`* | Check the role bounds, ratios and pacing limits that changed the request. |
| Counts return to a baseline | *`pendingCohort`* and `ResourceExhausted` | Check admission and available quota before retrying growth. |
| Counts return after shrinking | *`lastScaleDown`* | Resolve the aborted retirement before its cooldown expires. |
| A write is refused | Admission message | Check [Binding fence](#binding-fence) and [GitOps sync](#gitops-sync). An orphan recovery requires the named scaler to be gone or replaced. |

---

**See also** — [Model Deployment Configuration](deployment.md) (target roles and replica counts) ·
[Scaling Benchmark](scaling-benchmark.md) (H100 measurements and observed failures) ·
[Scaling Walkthrough](../../walkthroughs/model-deployment/scaling.md) (configuration and observation) ·
[Model Deployment Status](status.md) (target phase and conditions) ·
[Model Deployment Shutdown](shutdown.md) (retirement during scale-down) ·
[Model Deployment Metrics](metrics.md) (engine signals and scraping).

**Next** → [Model Deployment Status](status.md) — the target's status and scaler annotations.
