# Model Deployment Scaling

A `ModelDeploymentScaler` computes target replica counts for one `ModelDeployment` from live engine
metrics and publishes recommendations in *`status`*. In `Active` mode, it writes
*`spec.roles[].replicas`* on the target so the `ModelDeployment` controller scales and retires
instances.

![Model deployment scaling flow](/gpustack-operator/v0.12.1/docs/assets/diagrams/model-deployment-scaling.svg)

<!--
flowchart LR
    PODS["Serving replicas"] -> METRICS["Queue, throughput and latency metrics"]
    METRICS -> ALGORITHM["Selected scaling algorithm and optional priors"] -> GUARDS["Bounds, pacing and safety guards"]
    GUARDS -> SHADOW["Shadow status recommendations"]
    GUARDS -> ACTIVE["Active binding and term fence"] -> REPLICAS["Replica count and retirement"] -> PODS
-->

## Contents

- [Configuration](#configuration)
- [Algorithms](#algorithms)
- [Scaling policy](#scaling-policy)
- [Replica ownership](#replica-ownership)
- [Status](#status)
- [Limitations](#limitations)
- [Troubleshooting](#troubleshooting)

## Configuration

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeploymentScaler  # namespaced, short name mds
metadata:
  name: llm-serving
  namespace: team-a
spec:
  targetRef:
    name: llm-serving  # required, immutable; ModelDeployment in this namespace
  mode: Shadow  # default; enum: Shadow, Active
  suspend: false  # default; true stops planning and releases binding
  sampling:
    intervalSeconds: 15  # default; range 5..300
    pendingTimeoutSeconds: 300  # default; range 60..1200
  roles:  # required; include every target role
    - name: server
      minReplicas: 2  # optional, default: 1; range 1..1024 (no scale-to-zero)
      maxReplicas: 16  # required; range 1..1024, >= minReplicas
  algorithm:  # one type and its matching parameter block only
    type: Proportional
    proportional:
      metric: QueueDepth  # required; running + waiting requests
      tolerance: "0.1"  # optional, default: 0.1; range (0, 1]
      roleTargets:
        - name: server
          target: "8"  # required positive quantity; requests per ready replica
  behavior:
    scaleUp:
      stabilizationWindowSeconds: 0  # default; range 0..600
      maxStepReplicas: 4  # default; 0 or omitted stores 4
      maxStepPercent: 100  # default; 0 or omitted stores 100
      # Optional: replace both bounds with one unit; value 0 disables scale-up.
      # stepPolicy:
      #   type: Pods  # enum: Pods, Percent
      #   value: 2    # required, nonnegative
    scaleDown:
      stabilizationWindowSeconds: 300  # default; range 0..1800
      cooldownAfterAbortSeconds: 300  # default; range 0..3600
      decodePostScaleUpGraceSeconds: 120  # default; range 0..3600, Decode only
      # Optional: replace default 1 replica per pass; value 0 disables scale-down.
      # stepPolicy:
      #   type: Percent  # enum: Pods, Percent
      #   value: 10      # required, nonnegative
  rebalance:
    policy: Opposing  # default; enum: Opposing, Priority, Disabled
```

- **Target ordering**: A scaler may be created before its target; it reports `TargetNotFound` until
  the `ModelDeployment` exists.
- **Mode and suspend**: `Shadow` plans without claiming ownership; `Active` acquires the
  [Scaler ownership rules](#scaler-ownership-rules) and commits counts. `suspend: true` pauses planning and releases
  ownership.
- **Quick check**: Run `kubectl get mds` to read mode, algorithm and `Bound`, `Active` and
  `Blocked` conditions.

## Algorithms

*`algorithm.type`* selects `Proportional`, `TokenThroughput` or `Predictive` (inferred from a
present *`proportional`* or *`predictive`* block; otherwise defaults to `TokenThroughput`).

In a deterministic **synthetic simulation** (model output, not a hardware run) on identical
open-loop seeds, `Predictive` reached `99.4%` true SLO compliance on `ramp` versus `76.1%` for
reactive algorithms, and `quota-shortfall` verified *`pendingTimeoutSeconds`* rollback.

Online `Auto` calibration remains deferred. See [Scaling Benchmark](/gpustack-operator/v0.12.1/docs/modules/model-deployment/scaling-benchmark/index.md) for live
H100 and H200 measurements and
[Step 3: Choose an algorithm](/gpustack-operator/v0.12.1/docs/walkthroughs/model-deployment/scaling/index.md#step-3-choose-an-algorithm).

| Algorithm | Mechanism | Best fit | Main constraint |
|---|---|---|---|
| `Proportional` | Instantaneous metric over fixed per-replica target with *`tolerance`* deadband | Uniform-length requests | Bursts and shifting lengths skew fixed targets |
| **`TokenThroughput`** | **Default**. Stable and panic sliding windows over cache-adjusted token rates plus p95 *TTFT*/*ITL* guards | General LLM and *PD* workloads with prefix caching | Reacts after demand changes |
| `Predictive` | Causal Kalman filter forecasting *`horizonSeconds`* ahead, floored by `TokenThroughput` | Sustained ramps where pre-warming offsets startup latency | Falls back to `TokenThroughput` until *`minPoints`* arrive |

### Proportional

Recommends `ceil(total / target)` (minimum `1`) when observed load per ready replica leaves the
*`tolerance`* band. *`metric`* accepts `QueueDepth`, `RunningRequests`, `WaitingRequests`,
`PromptTokensPerSecond`, `GenerationTokensPerSecond` or `TotalTokensPerSecond`. Every scaled role
needs a target or a direct ratio to a targeted role.

### Token throughput

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeploymentScaler
metadata:
  name: llm-serving
  namespace: team-a
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
      decodeTargetTokensPerSecond: "800"  # generation capacity per Decode replica
      # serverTargetTokensPerSecond: "2000"  # combined capacity for Server role
      stableWindowSeconds: 180  # default; range 30..600
      panicWindowSeconds: 60  # default; range 10..300, < stableWindowSeconds
      panicThreshold: "2"  # default; positive quantity
      ttftTargetMilliseconds: "500"  # default; positive quantity
      itlTargetMilliseconds: "50"  # default; positive quantity
      latencyQuantile: "0.95"  # default; P95
```

- **Role capacity**: Supply prompt rate for *Prefill*, generation rate for *Decode*, combined rate
  for *Server*; couple *Encode* through a ratio.
- **Panic and latency guards**: Demand at *`panicThreshold`* × ready capacity enters panic and
  blocks scale-down. A *time to first token (TTFT)* or *inter-token latency (ITL)* breach holds
  scale-down; above `1.2×` target triggers growth.

### Predictive

Forecasts token demand *`horizonSeconds`* ahead via a Kalman filter, floored by nested
*`tokenThroughput`*:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeploymentScaler
metadata:
  name: llm-serving
  namespace: team-a
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
      horizonSeconds: 300  # default; range 30..600
      prefillTokenCapacityPerReplica: "2000"
      decodeTokenCapacityPerReplica: "800"
      # serverTokenCapacityPerReplica: "2000"
      qLevel: "1"  # default level process noise
      qTrend: "0.1"  # default trend process noise
      measurementNoise: "10"  # default observation noise
      minPoints: 6  # default; range 1..64
```

Before *`minPoints`* arrive (`PredictorWarmingUp`), `Predictive` uses the `TokenThroughput`
fallback. After warm-up, panic or latency above `1.2×` target adds at least one replica, and
scale-down requires survivors to stay at or below `80%` capacity.

### Engine signals

Both engines report running and waiting queues (SGLang adds `sglang:num_decode_transfer_queue_reqs`
and `sglang:num_prefill_inflight_queue_reqs`). Prompt rate adjusts for prefix-cache hits; generation
rate reads rank-0 deltas; latency reads histogram quantiles with no mean fallback if buckets hit
`+Inf`.

Inspect live samples, partial scrapes and missing series through the `ModelDeployment` `/metrics`
subresource (requires RBAC `get` on `modeldeployments/metrics`):

```bash
kubectl get --raw "/apis/worker.gpustack.ai/v1/namespaces/<namespace>/modeldeployments/<name>/metrics" \
  | jq '{partial, missing, processing, queueing, latency}'
```

The subresource scrapes up to `64` owned `Pod`s (`prometheus.io/scrape: "true"`,
`prometheus.io/port`, `prometheus.io/scheme`; take-over *`roles[].command`* roles omit scrape
annotations) and returns `503 Service Unavailable` when no owned `Pod` answers.

- **`partial: true`**: Marks unreadable or TLS-untrusted endpoints, first counter reads, counter
  resets, missing *TTFT* histograms or `> 64` `Pod`s.
- **`missing[]` with `partial: false`**: Normal for zero-count error counters, idle windows or
  unconfigured cache tiers.

### Capacity priors

*`spec.algorithm.capacityPrior`* fills omitted token capacities for `TokenThroughput` and
`Predictive` (`Proportional` rejects it) from an offline catalog (embedded `priors.yaml` or a
same-namespace `ConfigMap`). Missing or incompatible profiles fail closed
(`CapacityPriorUnresolved`, `InsufficientLoadCoverage`, `IncompatibleMetric`).

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeploymentScaler
metadata:
  name: qwen-scaler
  namespace: team-a
spec:
  targetRef:
    name: llm-serving
  roles:
    - name: prefill
      maxReplicas: 8
    - name: decode
      maxReplicas: 8
  algorithm:
    type: TokenThroughput  # also supported on Predictive
    tokenThroughput:
      latencyQuantile: "0.90"  # resolve prior at P90 instead of P95
      ttftTargetMilliseconds: "2000"  # required explicitly for catalog Prefill/Server
      itlTargetMilliseconds: "33"  # required explicitly for catalog Decode/Server
    capacityPrior:
      catalogRef:  # optional; omit for embedded catalog (no fallback when set)
        name: capacity-priors  # same-namespace ConfigMap
        key: priors.yaml  # default (falls back to priors.json)
      headroom: "0.75"  # default in (0, 1]; multiplies catalog seed only
```

- **Explicit latency targets**: Required when using the catalog (*TTFT* for *Prefill*, *ITL* for
  *Decode*, both for *Server*); default `500 ms` / `50 ms` values are suppressed.
- **Disaggregated ratios**: *PD* and *EPD* catalog resolution requires fixed connected *`ratios`*
  (`min == max`) across all roles; partial manual overrides are refused.
- **Admission behavior**: `CREATE` rejects unresolved priors when the target exists (warns if
  absent for GitOps ordering); `UPDATE` warns so you can suspend, switch to `Shadow` or fix fields.
  See [Suite 2: Capacity priors](/gpustack-operator/v0.12.1/docs/modules/model-deployment/scaling-benchmark/index.md#suite-2-capacity-priors) and
  [Resolving a catalog prior](/gpustack-operator/v0.12.1/docs/walkthroughs/model-deployment/scaling/index.md#catalog-prior-resolution).

### Catalog matching requirements

The resolver infers model, hardware and precision from *`spec.model`* (`ModelArtifact`),
`AcceleratorType` and `--quantization`. Pin or cross-check identities with optional
`ModelDeployment` annotations:

| Annotation | Purpose |
|---|---|
| `worker.gpustack.ai/model-id` | Model identity guard (natural name or normalized alias) |
| `worker.gpustack.ai/hardware-id` | Hardware identity guard (accelerator name or slug) |
| `worker.gpustack.ai/prefill-hardware-id` | *Prefill* hardware guard for heterogeneous targets |
| `worker.gpustack.ai/decode-hardware-id` | *Decode* hardware guard for heterogeneous targets |
| `worker.gpustack.ai/precision` | Precision selector (`fp8`, `bf16`, `fp4`, etc.) |

Catalog resolution supports single-`Pod` managed *Server*, *PD* and *EPD* roles with whole-card
resources (no slicing, *Elastic EP*, *External DP*, *`spec.kvCache`* or *`roles[].command`*).

## Scaling policy

### Replica bounds and ratios

*`roles[]`* defines `minReplicas` and `maxReplicas` for every target role. *`ratios[]`* couples
distinct roles with `min`, `max` and preferred `target` (equal values fix a ratio). The planner
projects recommendations onto one feasible vector; if current replicas violate bounds or ratios,
planning stops (`BoundsConflict` or `RatioConflict`) until the policy includes them.

### Scaling rates and safeguards

- **Scale-up step**: Without *`stepPolicy`*, each pass adds up to
  `max(maxStepReplicas, ceil(current * maxStepPercent))`. Set `stepPolicy` (`type: Pods` or
  `type: Percent`; `value: 0` blocks movement) to enforce one bound.
- **Scale-down step and holds**: Defaults to `1` replica per pass without *`stepPolicy`*. Scale-down
  holds when any role lacks ready replicas or full metrics, during panic/missing signals, during
  active retirement or abort cooldown, or during *Decode* post-scale-up grace.
- **Pending timeout and abort rollback**: Unadmitted scale-up cohorts hold growth and revert to
  baseline after *`pendingTimeoutSeconds`* (`ResourceExhausted`). An `Aborted` retirement restores
  pre-scale-down counts (`ScaleDownAborted`), cancels rebalance and starts
  *`cooldownAfterAbortSeconds`*.
- **Retirement versus pause**: Unlike *`roles[].pause: true`* (holds `Pod`s and quota under
  `RolePaused`), retirement withdraws `endpoint-eligible` (`30 s`), drains (`240 s`, after `60 s`
  disaggregated handoff grace) and deletes `Pod`s and `Workload` (`30 s`; `300 s` total).
  Re-declaring the ordinal before `Deleting` cancels retirement (`RetirementAdopted`); see
  [Pause and retirement](/gpustack-operator/v0.12.1/docs/modules/model-deployment/runtime-control/index.md#pause-and-retirement).

### Resource rebalancing

When a scale-up times out and reverts while the receiver still needs capacity, *`rebalance.policy`*
decides whether a donor on the same nonempty *`acceleratorType`* retires one replica first:

| Policy | Donor rule |
|---|---|
| `Opposing` | Default. Donor's own recommendation must be to shrink. |
| `Priority` | Lower-priority role in *`rebalance.priority[]`* may donate while holding. |
| `Disabled` | No donation attempted. |

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeploymentScaler
metadata:
  name: qwen-scaler
  namespace: team-a
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
    priority:  # highest first; unlisted roles neither donate nor receive
      - decode
      - prefill
      - encode
```

`RebalanceNotApplicable` means no donor shares the pool; `RebalanceBlocked` means an intermediate
vector violates policy or a `Priority` donation already ran in this exhaustion episode.

## Replica ownership

### Scaler ownership rules

An `Active` scaler claims target replicas with two annotations on the `ModelDeployment`:

| Annotation | Payload |
|---|---|
| `modeldeployment.gpustack.ai/scaler-binding` | *`scalerName`*, *`scalerUID`* and monotonic *`term`* |
| `modeldeployment.gpustack.ai/scaler-decision` | *`decisionID`*, *`term`* and committed *`vector`* |

Admission rejects user edits to replica counts or either annotation on a bound target while allowing
other edits. A competing scaler reports `AlreadyBound`; target recreation reports `TargetReplaced`
until acknowledged by a scaler edit; corrupt annotations report `InvalidBinding`. To clear an
orphaned binding after scaler deletion, remove the annotation without editing replica counts.

### GitOps sync

Ignore each role's *`replicas`* and both scaler annotations (`~1` escapes `/`) in GitOps tools:

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

Flux `Kustomization` uses the same paths under *`spec.ignore[].paths`*:

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

Compare *`status.roles[].rawRecommendedReplicas`* against *`constrainedReplicas`* and *`reason`*
(`WithinTolerance`, `Panic`, `NoReadyReplicas`, `LatencyGuardHold`, `LatencyGuardScaleUp`,
`PredictorWarmingUp`, `PredictiveConsolidationHold`, `RatioCoupled`). *`signalState`* reports
`Fresh`, `Idle`, `Missing` or `Stale`.

| Condition | Success reason | Other reasons |
|---|---|---|
| `Bound` | `Bound` | `TargetNotFound`, `AlreadyBound`, `TargetReplaced`, `InvalidBinding`, `Suspended` |
| `Active` | `Active` | `TargetNotFound`, `AlreadyBound`, `TargetReplaced`, `InvalidBinding`, `Suspended`, `ShadowMode`, `CapacityPriorUnresolved`, `InsufficientLoadCoverage`, `IncompatibleMetric` |
| `MetricsReady` | `Observed` | `WaitingForMetrics`, `ResourceExhausted`, `BoundsConflict`, `RatioConflict` |
| `CapacityPriorsReady` | `Resolved`, `Disabled` | `CapacityPriorUnresolved`, `InsufficientLoadCoverage`, `IncompatibleMetric`, `TargetNotFound`, `TargetReplaced`, `InvalidBinding`, `Suspended` |
| `Blocked` | `Ready` (`False`) | `BoundsConflict`, `RatioConflict`, `PacingBounded`, `ResourceExhausted`, `ScaleDownAborted`, `RebalanceNotApplicable`, `RebalanceBlocked`, `CapacityPriorUnresolved`, `InsufficientLoadCoverage`, `IncompatibleMetric`, `RolePaused` |

`RolePaused` holds replica writes while a target role is paused
([Scaling during pause](/gpustack-operator/v0.12.1/docs/modules/model-deployment/runtime-control/index.md#scaling-during-pause)). Check *`observedGeneration`*,
*`lastCommit`*, *`pendingCohort`*, *`rebalance`*, *`lastScaleDown`* and *`resolvedCapacityPriors`*.

## Limitations

- Admission rejects roles using *`roles[].command`* or `elastic.dataParallelWidth`, and requires
  generated group names at *`maxReplicas`* to fit Kubernetes's 63-character label limit.
- The sampler scrapes at most `64` ready `Pod`s per role (incomplete coverage holds scale-down), and
  rate/latency windows need at least two valid samples.
- Deployments without *`spec.router`* cannot prove direct `Service` withdrawal and abort retirement
  at `30 s`. On SGLang *PD*, set *`terminationGracePeriodSeconds: 45`*.
- Both webhooks use `failurePolicy: Fail`; a webhook outage rejects fenced writes while existing
  replicas keep serving.

## Troubleshooting

| Symptom | Signal | Owner & action |
|---|---|---|
| Counts never change | `Active=False` (`ShadowMode`, `Suspended`, `TargetNotFound`, `AlreadyBound`, `TargetReplaced`) | On the `ModelDeploymentScaler`, set `mode: Active`, clear `suspend`, or resolve the duplicate/replaced target binding. |
| Counts never change | `MetricsReady=False` (`WaitingForMetrics`), `signalState: Missing`/`Stale` | Query the `ModelDeployment` `/metrics` subresource (`jq '{partial,missing}'`), repair unreadable `Pod`s and wait two intervals. |
| Counts never change | `CapacityPriorsReady=False` | Fix `catalogRef` `ConfigMap`, target identity annotations, single-`Pod` topology and explicit latency targets, or supply explicit capacities on the scaler. |
| Counts never change | `Blocked=True` (`BoundsConflict`, `RatioConflict`, `RolePaused`, `PacingBounded`) | Adjust scaler bounds/ratios, clear *`roles[].pause`* on the `ModelDeployment`, or wait for pacing/retirement windows. |
| Counts revert after growth | *`pendingCohort`*, `ResourceExhausted` | Inspect Kueue `ClusterQueue` quota and `Workload` status on the target's pool before retrying growth. |
| Counts revert after shrinking | *`lastScaleDown`* (`Aborted`), target *`status.retirement`* | Inspect `kubectl -n <ns> get md <name> -o json \| jq '.status.retirement'`: add *`spec.router`* if aborted at `30 s`, or fix stuck/busy `Pod`s before cooldown ends. |
| Write refused by webhook | Admission error on `ModelDeployment` | Configure [GitOps sync](#gitops-sync) ignore paths or clear an orphaned binding annotation after deleting the scaler. |

---

**See also** — [Model Deployment Configuration](/gpustack-operator/v0.12.1/docs/modules/model-deployment/configuration/index.md) (role fields, readiness and rollout) ·
[Scaling Benchmark](/gpustack-operator/v0.12.1/docs/modules/model-deployment/scaling-benchmark/index.md) (H100 and H200 measurements) ·
[Scaling Walkthrough](/gpustack-operator/v0.12.1/docs/walkthroughs/model-deployment/scaling/index.md) (configure and observe) ·
[Online Runtime Control](/gpustack-operator/v0.12.1/docs/modules/model-deployment/runtime-control/index.md) (role pause guard and retirement budgets) ·
[Model Deployment Routing](/gpustack-operator/v0.12.1/docs/modules/model-deployment/routing/index.md) (router withdrawal and endpoint eligibility)

**Next** → [Model Deployment Scaling Benchmark](/gpustack-operator/v0.12.1/docs/modules/model-deployment/scaling-benchmark/index.md) — review H100 and H200 scaling measurements.
