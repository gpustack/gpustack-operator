# Model Deployment Scaling

A `ModelDeploymentScaler` plans replica counts for one `ModelDeployment` from engine metrics. It
reports observations and recommendations in *`status`*. In `Active` mode, it also writes
*`spec.roles[].replicas`* on the target, and the `ModelDeployment` reconciler creates and retires
the serving instances.

![Model deployment scaling flow](../../assets/diagrams/model-deployment-scaling.svg)

<!--
flowchart LR
    PODS["Serving replicas"] -> METRICS["Queue, throughput and latency metrics"]
    METRICS -> ALGORITHM["Selected scaling algorithm and optional priors"] -> GUARDS["Bounds, pacing and safety guards"]
    GUARDS -> SHADOW["Shadow status recommendations"]
    GUARDS -> ACTIVE["Active binding and term fence"] -> REPLICAS["Replica count and retirement"] -> PODS
-->

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
kind: ModelDeploymentScaler  # required; namespaced, short name mds
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

- **Target**: Set *`targetRef.name`* to a `ModelDeployment` in the same namespace (immutable). A
  scaler can precede its target and reports `TargetNotFound` until the target appears.
- **Mode**: `Shadow` computes recommendations without owning the target; `Active` acquires the
  [Binding fence](#binding-fence) and commits counts. *`suspend: true`* stops planning and releases
  an owned binding.
- **Inspection**: Run `kubectl get mds` to read target, mode, algorithm, and `Bound`, `Active` and
  `Blocked` conditions.

## Algorithms

*`algorithm.type`* selects `Proportional`, `TokenThroughput`, or `Predictive` (inferred from a
present *`proportional`* or *`predictive`* block; otherwise defaults to `TokenThroughput`).

In a deterministic **synthetic simulation** (not a hardware measurement) across identical open-loop
seeds, `Predictive` reached `99.4%` true SLO compliance on the `ramp` seed versus `76.1%` for
reactive algorithms, and `quota-shortfall` verified `pendingTimeoutSeconds` rollback.

Online `Auto` calibration remains deferred after synthetic evaluation showed mixed latency/cost
trade-offs. For live H100 and H200 measurements, see [Scaling Benchmark](scaling-benchmark.md) and
[Step 3: Choose an algorithm](../../walkthroughs/model-deployment/scaling.md#step-3-choose-an-algorithm).

| Algorithm | Mechanism | Suitable workload | Main constraint |
|---|---|---|---|
| `Proportional` | Instantaneous metric (`QueueDepth`, `RunningRequests`, `WaitingRequests`, or token rate) over fixed per-replica target with *`tolerance`* deadband. | Uniform-length requests whose concurrency maps predictably to GPU load. | Bursts and shifting capacity make a fixed target inaccurate. |
| **`TokenThroughput`** | **Default**. Two sliding windows (*`stableWindowSeconds`*, *`panicWindowSeconds`*) over cache-adjusted token rates, *`panicThreshold`* burst mode, and p95 *TTFT*/*ITL* guards. | General LLM and *PD* serving with variable prompt/output lengths and prefix caching. | Windows react after demand changes. |
| `Predictive` | Causal local-linear-trend Kalman filter forecasting *`horizonSeconds`* ahead, floored by `TokenThroughput`. | Sustained traffic ramps where pre-warming ahead of peaks offsets cold-start latency. | Uses `TokenThroughput` fallback until *`minPoints`* observations arrive. |

### Proportional

Divides observed role load by ready replicas against *`roleTargets[].target`* outside *`tolerance`*
(default `0.1`, in `(0, 1]`), recommending `ceil(total / target)` (minimum `1`). *`metric`* accepts
`QueueDepth`, `RunningRequests`, `WaitingRequests`, `PromptTokensPerSecond`,
`GenerationTokensPerSecond`, or `TotalTokensPerSecond`. Every scaled role needs a target or a
direct ratio to a targeted role.

### Token throughput

Scales vLLM or SGLang roles on cache-adjusted token rates and latency guards:

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

- **Role capacity**: Set prompt capacity for *prefill*, generation for *decode*, combined for
  *server*; couple *encode* with a ratio.
- **Panic and latency guards**: Demand at *`panicThreshold`* times ready capacity starts panic and
  blocks scale-down. A *TTFT* or *ITL* breach holds scale-down; above `1.2×` target recommends
  growth. Keep *`panicWindowSeconds`* shorter than *`stableWindowSeconds`*.

### Engine signals

- **Queue and token rates**: Both engines report running and waiting requests (SGLang includes
  `sglang:num_decode_transfer_queue_reqs` and `sglang:num_prefill_inflight_queue_reqs`). Prompt
  rate uses effective-prefill deltas (or subtracts prefix-cache hits); generation rate uses rank-0
  deltas; latency reads histogram quantiles without mean fallback when buckets are absent or hit
  `+Inf`.

Inspect live gauge coverage, partial samples, and missing sources through the `ModelDeployment`
`/metrics` subresource (requires `get` on `modeldeployments/metrics`):

```bash
kubectl get --raw "/apis/worker.gpustack.ai/v1/namespaces/<namespace>/modeldeployments/<name>/metrics" \
  | jq '{partial, missing, processing, queueing, latency}'
```

The subresource scrapes up to `64` owned `Pod`s (`prometheus.io/scrape: "true"`, `prometheus.io/port`,
`prometheus.io/scheme`; unmanaged *`roles[].command`* roles receive no scrape annotations) and
returns `503 Service Unavailable` when no owned `Pod` answers.

`partial: true` marks unreadable or TLS-untrusted endpoints, first counter reads, counter resets,
absent *TTFT* histograms, or `> 64` `Pod`s. Unincremented error counters, idle windows, and
unconfigured cache tiers appear in *`missing[]`* while leaving `partial` `false`.

### Predictive

Forecasts token demand *`horizonSeconds`* ahead via a Kalman filter, floored by nested
*`tokenThroughput`*:

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
      prefillTokenCapacityPerReplica: "2000"
      decodeTokenCapacityPerReplica: "800"
      # serverTokenCapacityPerReplica: "2000"
      qLevel: "1"             # default process noise for demand level
      qTrend: "0.1"           # default process noise for demand trend
      measurementNoise: "10"  # default observation noise
      minPoints: 6            # default; 1 to 64 observations
```

Before *`minPoints`* observations arrive (`PredictorWarmingUp`), `Predictive` uses the
`TokenThroughput` fallback. After warm-up, panic or latency above `1.2×` target recommends at least
one added replica, and scale-down requires surviving replicas to stay at or below `80%` capacity.

### Capacity priors

*`spec.algorithm.capacityPrior`* resolves omitted token capacities for `TokenThroughput` and
`Predictive` (`Proportional` rejects it) from an offline catalog (embedded `priors.yaml` or a
same-namespace `ConfigMap`, never calling external services). Missing or incompatible profiles fail
closed (`CapacityPriorUnresolved`, `InsufficientLoadCoverage`, `IncompatibleMetric`).

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeploymentScaler
metadata:
  name: qwen-scaler
spec:
  targetRef:
    name: llm-serving
  roles:
    - name: prefill
      maxReplicas: 8
    - name: decode
      maxReplicas: 8
  algorithm:
    type: TokenThroughput  # supports capacityPrior alongside Predictive
    tokenThroughput:
      latencyQuantile: "0.90"         # resolve prior at P90 instead of P95
      ttftTargetMilliseconds: "2000"  # required explicitly for catalog-backed Prefill/Server
      itlTargetMilliseconds: "33"     # required explicitly for catalog-backed Decode/Server
    capacityPrior:
      catalogRef:  # optional; omit for embedded catalog (no fallback when set)
        name: capacity-priors  # same-namespace ConfigMap
        key: priors.yaml       # default (falls back to legacy priors.json)
      headroom: "0.75"         # default in (0, 1]; multiplies catalog seed only
```

- **Explicit targets required**: When any role relies on the catalog, specify explicit positive
  latency targets (*TTFT* for *Prefill*, *ITL* for *Decode*, both for *Server*); default `500 ms` /
  `50 ms` values are suppressed.
- **Disaggregated ratios**: *PD* and *EPD* catalog resolution requires fixed connected *`ratios`*
  (`min == max`) across all roles; partial per-role manual overrides are refused.
- **Validation**: `CREATE` rejects unresolved priors when the target exists (warns if absent for
  GitOps ordering); `UPDATE` warns without writing status so you can suspend, switch to `Shadow`,
  or fix fields. See [Scaling Benchmark](scaling-benchmark.md#suite-2-capacity-priors) and
  [Scaling Walkthrough](../../walkthroughs/model-deployment/scaling.md#resolving-a-catalog-prior).

### Target eligibility

The resolver infers model, hardware and precision from *`spec.model`* (`ModelArtifact`),
`AcceleratorType`, and `--quantization`. Use optional `ModelDeployment` annotations to pin
precision when multiple quantizations match or to cross-check identities:

| Annotation | Accepted values |
|---|---|
| `worker.gpustack.ai/model-id` | Optional model identity guard (natural name or normalized alias) |
| `worker.gpustack.ai/hardware-id` | Optional hardware identity guard (accelerator name or slug) |
| `worker.gpustack.ai/prefill-hardware-id` | Optional *Prefill* hardware guard for heterogeneous targets |
| `worker.gpustack.ai/decode-hardware-id` | Optional *Decode* hardware guard for heterogeneous targets |
| `worker.gpustack.ai/precision` | Optional precision selector (`fp8`, `bf16`, `fp4`, etc.) |

Catalog resolution supports single-serving-`Pod` managed `Server`, *PD*, and *EPD* roles with
whole-accelerator resources (no slicing, Elastic EP, external DP, external *`spec.kvCache`*, or
custom *`command`*).

## Bounds and ratios

*`roles[]`* sets `minReplicas` (`1`..`1024`, default `1`; no scale-to-zero) and `maxReplicas` for
every target role. *`ratios[]`* links distinct roles with optional `min`, `max`, and preferred
`target` (equal `min`/`target`/`max` fixes a ratio). The planner projects recommendations atomically
onto one feasible vector; if the current replica vector already violates bounds or ratios, planning
stops with `BoundsConflict` or `RatioConflict` until the policy includes it.

## Pacing and guards

- **Scale-up**: *`behavior.scaleUp.stabilizationWindowSeconds`* (default `0`) waits for history
  before growing; without *`stepPolicy`*, each pass adds up to `max(maxStepReplicas, ceil(current * maxStepPercent))`
  (defaults `4` and `100%`). Use `stepPolicy` (`type: Pods` or `type: Percent`; `0` disables
  movement) for one bound alone.
- **Scale-down**: Defaults to `1` replica per pass without *`stepPolicy`*. Downward
  *`stabilizationWindowSeconds`* (`300`), *`cooldownAfterAbortSeconds`* (`300`), and
  *`decodePostScaleUpGraceSeconds`* (`120`) apply defaults when omitted or `0`.
- **Scale-down holds**: Scale-down waits when any role lacks ready replicas or complete metric
  coverage, during panic/missing signals, during any active retirement or abort cooldown, or during
  *Decode* post-scale-up grace.
- **Pending timeout and abort rollback**: Pending unadmitted scale-up cohorts hold further growth
  and restore the baseline vector after *`sampling.pendingTimeoutSeconds`* (`ResourceExhausted`). An
  `Aborted` retirement restores the pre-scale-down vector (`ScaleDownAborted`), cancels any donor
  rebalance, and starts *`cooldownAfterAbortSeconds`*.
- **Graceful retirement vs pause**: Unlike *`roles[].pause: true`* (which holds `Pod`s and quota in
  place under `RolePaused`), retirement withdraws `endpoint-eligible` (`Withdrawing`, `30 s`),
  drains in-flight work (`Draining`, `240 s`, after a `60 s` handoff grace on *PD*/External *DP*),
  and deletes `Pod`s and `Workload` (`Settling`, `30 s`; `300 s` total). Re-declaring an ordinal
  before `Deleting` cancels retirement (`RetirementAdopted`); see
  [Pause and retirement](runtime-control.md#pause-and-retirement).

## Resource rebalancing

When a scale-up times out and restores its baseline while the receiver still needs capacity,
*`rebalance.policy`* chooses whether a donor on the same nonempty *`acceleratorType`* releases one
replica first (waiting for retirement and `Workload` deletion before growing the receiver):

| Policy | Donor selection |
|---|---|
| `Opposing` | Default. Donor's own recommendation must be to shrink. |
| `Priority` | Lower-priority role in *`rebalance.priority[]`* may donate even while holding. |
| `Disabled` | No donation attempted. |

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeploymentScaler
metadata:
  name: qwen-scaler
spec:
  targetRef:
    name: llm-serving
  roles:
    - name: prefill
      maxReplicas: 8
    - name: decode
      maxReplicas: 8
    - name: encode
      maxReplicas: 4
  algorithm:
    type: Proportional
    proportional:
      metric: WaitingRequests
      roleTargets:
        - name: prefill
          target: "8"
        - name: decode
          target: "8"
        - name: encode
          target: "4"
  rebalance:
    policy: Priority
    priority:  # highest priority first; unlisted roles neither donate nor receive
      - decode
      - prefill
      - encode
```

`RebalanceNotApplicable` means no eligible donor shares the pool; `RebalanceBlocked` means an
intermediate vector violates policy or a `Priority` donation was already attempted in this
exhaustion episode.

## Binding fence

An `Active` scaler claims target replica counts with two annotations:

| Annotation | Payload |
|---|---|
| `modeldeployment.gpustack.ai/scaler-binding` | *`scalerName`*, *`scalerUID`* and monotonic *`term`* |
| `modeldeployment.gpustack.ai/scaler-decision` | *`decisionID`*, *`term`* and committed *`vector`* |

Admission refuses user edits to a bound target's replica counts or either annotation while other
fields remain editable. A second scaler reports `AlreadyBound`; target recreation reports
`TargetReplaced` until acknowledged by a scaler spec edit; corrupt annotations report
`InvalidBinding`. If an orphaned binding remains after scaler deletion, remove the annotation
without changing replica counts.

## GitOps sync

Configure GitOps controllers to ignore each role's *`replicas`* and both scaler annotations (`~1`
escapes `/`):

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: llm-serving
  namespace: argocd
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

Flux `Kustomization` uses the same JSON pointers under `spec.ignore[].paths`:

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: gpustack-operator
  namespace: flux-system
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

`status` updates when control-plane state moves (decisions, bindings, conditions, replica counts) or
when a predictor advances by 16 samples. Compare *`status.roles[].rawRecommendedReplicas`* against
*`constrainedReplicas`* and *`reason`* (`WithinTolerance`, `Panic`, `NoReadyReplicas`,
`LatencyGuardHold`, `LatencyGuardScaleUp`, `PredictorWarmingUp`, `PredictiveConsolidationHold`,
`RatioCoupled`) to see how guards shaped a recommendation.

*`signalState`* reports `Fresh`, `Idle`, `Missing`, or `Stale`.

| Condition | Success reason | Other reasons |
|---|---|---|
| `Bound` | `Bound` | `TargetNotFound`, `AlreadyBound`, `TargetReplaced`, `InvalidBinding`, `Suspended` |
| `Active` | `Active` | `TargetNotFound`, `AlreadyBound`, `TargetReplaced`, `InvalidBinding`, `Suspended`, `ShadowMode`, `CapacityPriorUnresolved`, `InsufficientLoadCoverage`, `IncompatibleMetric` |
| `MetricsReady` | `Observed` | `WaitingForMetrics`, `ResourceExhausted`, `BoundsConflict`, `RatioConflict` |
| `CapacityPriorsReady` | `Resolved`, `Disabled` | `CapacityPriorUnresolved`, `InsufficientLoadCoverage`, `IncompatibleMetric`, `TargetNotFound`, `TargetReplaced`, `InvalidBinding`, `Suspended` |
| `Blocked` | `Ready` (`False`) | `BoundsConflict`, `RatioConflict`, `PacingBounded`, `ResourceExhausted`, `ScaleDownAborted`, `RebalanceNotApplicable`, `RebalanceBlocked`, `CapacityPriorUnresolved`, `InsufficientLoadCoverage`, `IncompatibleMetric`, `RolePaused` |

`RolePaused` holds replica writes while a target role is paused
([Online Runtime Control](runtime-control.md#scaler-pause-guard)). Inspect each condition's
*`observedGeneration`* alongside *`status.lastCommit`*, *`pendingCohort`*, *`rebalance`*,
*`lastScaleDown`*, and *`resolvedCapacityPriors`*.

## Limitations

- Admission refuses roles with custom *`command`* or `elastic.dataParallelWidth`. Generated group
  and member names at *`maxReplicas`* must fit the 63-character Kubernetes label limit.
- The sampler scrapes at most `64` ready answering `Pod`s per role (incomplete coverage holds
  scale-down), and rate/latency windows need at least two valid samples.
- Unrouted deployments without *`spec.router`* cannot prove direct `Service` withdrawal and abort
  retirement at `30 s`. On SGLang *PD* roles, set *`terminationGracePeriodSeconds: 45`* for clean
  post-`SIGTERM` exit. Keep *Decode* *`minReplicas`* high enough if prefills exceed the `60 s`
  handoff window.
- Both webhooks use `failurePolicy: Fail`; a webhook outage rejects fenced writes while existing
  replicas keep serving.

## Troubleshooting

| Symptom | Signal | Check & Mitigation |
|---|---|---|
| Counts never change | Condition: `Active` | Use `Active` mode and clear suspension when ready to commit. Resolve missing, replaced or already-owned targets. |
| Counts never change | Condition: `MetricsReady` (`WaitingForMetrics`), role `signalState` (`Missing`/`Stale`) | Run `kubectl get --raw "/apis/worker.gpustack.ai/v1/namespaces/<ns>/modeldeployments/<name>/metrics" \| jq '{partial,missing}'` to check `partial: true` or unreadable/unscraped `Pod`s, and allow two samples to form a rate window. |
| Counts never change | Condition: `CapacityPriorsReady` | Verify `catalogRef`, target identity (annotations or inferred model/hardware), engine-family image, single-`Pod` topology and explicit latency targets, or supply explicit capacities / remove *`capacityPrior`*. |
| Counts never change | Condition: `Blocked`, Status: role `reason` | Correct bounds or ratios, clear *`roles[].pause`* (`RolePaused`), or wait for the named pacing or retirement guard. |
| Recommendation differs from counts | Status: *`rawRecommendedReplicas`* and *`constrainedReplicas`* | Check the role bounds, ratios and pacing limits that changed the request. |
| Counts return to a baseline | Status: *`pendingCohort`*, Reason: `ResourceExhausted` | Check admission and available quota before retrying growth. |
| Counts return after shrinking | Status: *`lastScaleDown`* (`Aborted`), target *`status.retirement`* | Inspect `kubectl -n <ns> get md <name> -o json \| jq '.status.retirement'`: configure *`spec.router`* if withdrawal aborted at `30 s`, or resolve busy/restarting/unmanaged members before *`cooldownAfterAbortSeconds`* expires. |
| A write is refused | Admission message | Check [Binding fence](#binding-fence) and [GitOps sync](#gitops-sync). Orphan recovery requires the named scaler to be gone or replaced. |

---

**See also** — [Configuration](configuration.md) (role fields, readiness, and rollout behavior) · [Benchmark](scaling-benchmark.md) (H100 and H200 measurements) · [Walkthrough](../../walkthroughs/model-deployment/scaling.md) (configure and observe) · [Online Runtime Control](runtime-control.md) (role pause guard, retirement budgets, and drain window) · [Routing](routing.md) (router withdrawal and endpoint eligibility).

**Next** → [Model Deployment Scaling Benchmark](scaling-benchmark.md) — review H100 and H200 scaling measurements.
