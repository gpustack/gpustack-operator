# Model Deployment Scaling Walkthrough

Attach a `ModelDeploymentScaler` in `Shadow` mode to inspect replica recommendations, compare
`Proportional`, `TokenThroughput`, `Predictive`, and catalog-prior algorithms, then switch to
`Active` mode to observe scale-out and graceful replica retirement.

## Contents

- [Summary](#summary)
- [Environment](#environment)
- [Scaler deployment](#scaler-deployment)
- [Step 3: Choose an algorithm](#step-3-choose-an-algorithm)
- [Live scaling](#live-scaling)
- [Verification](#verification)
- [Cleanup](#cleanup)
- [Troubleshooting](#troubleshooting)

## Summary

Start one vLLM *prefill/decode (PD)* target, verify recommendations safely in `Shadow` mode, select a
scaling algorithm (including InferenceX catalog priors or *encode/prefill/decode (EPD)* role ratios),
and switch to `Active` mode:

| Object | Scope | Creator | Purpose |
|---|---|---|---|
| `ModelArtifact` | namespace | user | Resolves `Qwen/Qwen3-8B` weights (`qwen3-8b`) |
| `AcceleratorType` | cluster | operator | Whole-GPU pool requested by each role replica |
| `ModelDeployment` | namespace | user | Disaggregated *PD* target whose *`replicas`* the scaler controls |
| `ModelDeploymentScaler` | namespace | user | Autoscaling policy evaluated in `Shadow` and enforced in `Active` |

Manifests below are reusable configuration examples; captured lifecycle results from the standalone
H100 and H200 runs are labeled explicitly in [Verification](#verification) and
[Model Deployment Scaling Benchmark](../../modules/model-deployment/scaling-benchmark.md).

## Environment

| Item | Value |
|---|---|
| GPUs | 2 free GPUs initially, up to 4 GPUs at max scale (1 GPU per replica) |
| `AcceleratorType` | Reusable example uses `gpustack--nvidia-h100-80gb-hbm3-linux-amd64` (substitute an `Active` pool from `kubectl get acctype`) |
| Namespace | `gpustack-bench` |
| Model | `Qwen/Qwen3-8B` via `ModelArtifact` `qwen3-8b` at revision `b968826d9c46dd6066d109eabc6255188de91218` |
| Tools | `kubectl`, `jq`, and Python 3 with `aiohttp` (plus a repo checkout for `bench_client.py`) |

### Prerequisites

Complete [Cluster preparation](_index.md#cluster-preparation) and warm `qwen3-8b` in `gpustack-bench`
via [Model Prefetch](../model-delivery/prefetch.md#step-1-create-artifact-and-prefetch). Confirm no
other `ModelDeploymentScaler` already binds the target deployment; see
[Model Deployment Scaling](../../modules/model-deployment/scaling.md) and
[GitOps sync](../../modules/model-deployment/scaling.md#gitops-sync).

## Scaler deployment

### Step 1: Deploy scaling target

Save `scaling-target.yaml` (replace *`acceleratorType`* if not running on H100):

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: autoscaling-vllm-pd
  namespace: gpustack-bench
spec:
  model:
    name: Qwen/Qwen3-8B
    artifactRef:
      name: qwen3-8b
  engine:
    name: vLLM
    image: gpustack/runner:cuda13.0-vllm0.30.0
  router:
    name: llm-d-router
  roles:
    - name: prefill
      kind: Prefill
      replicas: 1
      size: 1
      acceleratorType: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
      resources:
        accelerator: "1"
      extraArgs:
        - --tensor-parallel-size
        - "1"
        - --max-model-len
        - "16384"
        - --max-num-seqs
        - "64"
        - --gpu-memory-utilization
        - "0.90"
        - --enable-scale-out
      env:
        - name: VLLM_HTTP_TIMEOUT_KEEP_ALIVE
          value: "3600"
    - name: decode
      kind: Decode
      replicas: 1
      size: 1
      acceleratorType: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
      resources:
        accelerator: "1"
      extraArgs:
        - --tensor-parallel-size
        - "1"
        - --max-model-len
        - "16384"
        - --max-num-seqs
        - "64"
        - --gpu-memory-utilization
        - "0.90"
        - --enable-scale-out
      env:
        - name: VLLM_HTTP_TIMEOUT_KEEP_ALIVE
          value: "3600"
```

Apply `scaling-target.yaml` and wait until `PHASE=Ready` (`1R1P1D`) before generating load:

```bash
kubectl apply -f scaling-target.yaml
kubectl -n gpustack-bench get modeldeployment autoscaling-vllm-pd -w
```

`--enable-scale-out` enables router token counting
([Router token counting](../../modules/model-deployment/prefill-decode.md#router-token-counting)),
and `VLLM_HTTP_TIMEOUT_KEEP_ALIVE=3600` matches the benchmark configuration (it reduces, without
eliminating, scale-out connection drops).

### Step 2: Review Shadow recommendations

Save `scaling-policy.yaml` in `Shadow` mode so the scaler computes recommendations without writing
target *`replicas`*:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeploymentScaler
metadata:
  name: autoscale-vllm-pd
  namespace: gpustack-bench
spec:
  targetRef:
    name: autoscaling-vllm-pd
  mode: Shadow
  sampling:
    intervalSeconds: 10
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
      target: "1"  # soft preference; add min/max only when hard ratio bounds are required
  algorithm:
    type: Proportional
    proportional:
      metric: RunningRequests
      tolerance: "0.1"
      roleTargets:
        - name: prefill
          target: "3"
        - name: decode
          target: "3"
```

```bash
kubectl apply -f scaling-policy.yaml
kubectl -n gpustack-bench get mds autoscale-vllm-pd \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'
kubectl -n gpustack-bench get modeldeployment autoscaling-vllm-pd -o json |
  jq '{replicas: [.spec.roles[] | {name, replicas}],
       binding: .metadata.annotations["modeldeployment.gpustack.ai/scaler-binding"]}'
```

Confirm `Bound=True`, `Active=False` (`Reason: ShadowMode`), unchanged `replicas: 1`, and `binding: null`.
Inspect *`status.roles[].rawRecommendedReplicas`*, *`constrainedReplicas`*, *`signalState`*, and
*`reason`* together.

Create an in-cluster load driver `Pod` (`default/bench-driver`) if absent, and send a 45 s baseline
load against the router `Service`:

```bash
kubectl -n default run bench-driver --image=python:3.11-slim --restart=Never --command -- sleep infinity
kubectl -n default wait pod/bench-driver --for=condition=Ready --timeout=300s
kubectl -n default exec bench-driver -- python3 -m pip install aiohttp
```

```bash
BENCH_INCLUSTER=1 python3 .agents/skills/gpustack-operator-benchmark/scripts/bench_client.py load \
  --endpoint http://autoscaling-vllm-pd-router.gpustack-bench.svc:8000 \
  --model Qwen/Qwen3-8B --arm shadow --suite autoscaling --label baseline \
  --input-tokens 1024 --output-tokens 256 --concurrency 2 --duration 45 \
  --out /tmp/scaler-shadow.json --requests-out /tmp/scaler-shadow-requests.jsonl
```

If `bench_client.py` reports local fallback, stop and fix `default/bench-driver` access because
cluster-internal DNS will not resolve locally.

## Step 3: Choose an algorithm

When switching algorithms in `scaling-policy.yaml`, replace the entire *`spec.algorithm`* block
(admission rejects sub-blocks that do not match *`algorithm.type`*):

| Choice | Target scenario | Configuration guidance |
|---|---|---|
| `Predictive` + *`capacityPrior`* | Latency-sensitive serving and traffic ramps where replica startup (~76 s on H200) must start ahead of peak load | Set *`horizonSeconds`* above startup time (`180–300` s), keep *`headroom: "0.75"`*, and cap step bursts via *`behavior.scaleUp.maxStepReplicas`*. |
| `TokenThroughput` + *`capacityPrior`* | Variable prompt lengths and prefix caching where GPU replica-seconds should track sustained token demand | Use *`capacityPrior`* (`headroom: "0.75"`) or set manual targets at 70–75% of single-replica saturation. |
| `Proportional` | Uniform request shapes using `RunningRequests` or `QueueDepth` without offline token sweeps | Set *`roleTargets[].target`* to per-replica concurrency within *TTFT* targets (`Proportional` rejects *`capacityPrior`*). |

### Token throughput

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: ModelDeploymentScaler
 metadata:
   name: autoscale-vllm-pd
   namespace: gpustack-bench
 spec:
   algorithm:
-    type: Proportional
-    proportional:
-      metric: RunningRequests
-      tolerance: "0.1"
-      roleTargets:
-        - name: prefill
-          target: "3"
-        - name: decode
-          target: "3"
+    type: TokenThroughput
+    tokenThroughput:
+      prefillTargetTokensPerSecond: "1200"
+      decodeTargetTokensPerSecond: "300"
```

Allow two successive metric samples before evaluating rate recommendations. Before testing sustained
SGLang *PD* traffic over TCP, review the
[SGLang TCP limitation](../../modules/model-deployment/engine-versions.md#limitations).

### Predictive

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: ModelDeploymentScaler
 metadata:
   name: autoscale-vllm-pd
   namespace: gpustack-bench
 spec:
   algorithm:
-    type: Proportional
-    proportional:
-      metric: RunningRequests
-      tolerance: "0.1"
-      roleTargets:
-        - name: prefill
-          target: "3"
-        - name: decode
-          target: "3"
+    type: Predictive
+    predictive:
+      horizonSeconds: 30
+      minPoints: 2
+      qLevel: "0.5"
+      qTrend: "0.3"
+      prefillTokenCapacityPerReplica: "2000"
+      decodeTokenCapacityPerReplica: "2000"
+      tokenThroughput:
+        prefillTargetTokensPerSecond: "1200"
+        decodeTargetTokensPerSecond: "300"
```

Nested *`tokenThroughput`* targets act as fallback while the predictor collects *`minPoints`*; inspect
*`status.predictors`* and role reasons to distinguish forecast planning from fallback
([Scale-out timing](../../modules/model-deployment/scaling-benchmark.md#scale-out-timing)).

### Catalog capacity estimates

*`spec.algorithm.capacityPrior`* resolves omitted `TokenThroughput` or `Predictive` capacities from an
embedded InferenceX `ModelDeploymentScalerCapacityPriorCatalog` (when *`catalogRef`* is omitted) or
from a same-namespace `ConfigMap` (`priors.yaml` key by default; empty `catalogRef: {}` is rejected).
See [Catalog matching requirements](../../modules/model-deployment/scaling.md#catalog-matching-requirements) and
[Capacity priors](../../modules/model-deployment/scaling.md#capacity-priors).

Because the embedded catalog omits `Qwen/Qwen3-8B`, create a reviewed `ConfigMap/autoscaling-priors`
in `gpustack-bench` first (if multiple catalog quantizations match, pass `--quantization` or annotate
the deployment with `worker.gpustack.ai/precision`):

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeploymentScaler
metadata:
  name: autoscale-vllm-pd-prior
  namespace: gpustack-bench
spec:
  targetRef:
    name: autoscaling-vllm-pd
  mode: Shadow
  roles:
    - name: prefill
      minReplicas: 1
      maxReplicas: 2
    - name: decode
      minReplicas: 3
      maxReplicas: 6
  ratios:
    - numeratorRole: decode
      denominatorRole: prefill
      min: "3"
      max: "3"
  algorithm:
    type: TokenThroughput
    tokenThroughput:
      ttftTargetMilliseconds: "2000"
      itlTargetMilliseconds: "50"
    capacityPrior:
      catalogRef:
        name: autoscaling-priors
      headroom: "0.75"
```

For a single-role `Server` target (`catalog-server`), you can also combine an explicit fallback
override with catalog-resolved forecast capacity:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: catalog-server
  namespace: gpustack-bench
spec:
  model:
    name: Qwen/Qwen3-8B
    artifactRef:
      name: qwen3-8b
  engine:
    name: vLLM
    image: gpustack/runner:cuda13.0-vllm0.30.0
  roles:
    - name: server
      kind: Server
      replicas: 1
      size: 1
      acceleratorType: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
      resources:
        accelerator: "1"
---
apiVersion: worker.gpustack.ai/v1
kind: ModelDeploymentScaler
metadata:
  name: autoscale-server
  namespace: gpustack-bench
spec:
  targetRef:
    name: catalog-server
  roles:
    - name: server
      minReplicas: 1
      maxReplicas: 2
  algorithm:
    type: Predictive
    predictive:
      horizonSeconds: 30
      minPoints: 2
      tokenThroughput:
        serverTargetTokensPerSecond: "1200"  # Explicit fallback override
        ttftTargetMilliseconds: "500"        # required for Server catalog resolution
        itlTargetMilliseconds: "50"          # required for Server catalog resolution
    capacityPrior:
      catalogRef:
        name: autoscaling-priors
        key: priors.yaml
      headroom: "0.75"
```

Verify resolved priors and conditions:

```bash
kubectl -n gpustack-bench get mds autoscale-server -o json |
  jq '{priors: .status.resolvedCapacityPriors,
       conditions: [.status.conditions[] | select(.type == "CapacityPriorsReady" or .type == "Blocked")]}'
```

When a target matches an embedded catalog profile, omit both *`serverTargetTokensPerSecond`* and
*`catalogRef`*:

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: ModelDeploymentScaler
 metadata:
   name: autoscale-server
   namespace: gpustack-bench
 spec:
   algorithm:
     type: Predictive
     predictive:
       tokenThroughput:
-        serverTargetTokensPerSecond: "1200"
         ttftTargetMilliseconds: "500"
         itlTargetMilliseconds: "50"
     capacityPrior:
-      catalogRef:
-        name: autoscaling-priors
-        key: priors.yaml
       headroom: "0.75"
```

To switch back to manual capacities, remove *`capacityPrior`* and supply explicit capacity fields:

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: ModelDeploymentScaler
 metadata:
   name: autoscale-server
   namespace: gpustack-bench
 spec:
   algorithm:
     type: Predictive
     predictive:
+      serverTokenCapacityPerReplica: "2000"
       tokenThroughput:
+        serverTargetTokensPerSecond: "1200"
-    capacityPrior:
-      headroom: "0.75"
```

`CREATE` rejects an unresolved catalog prior when the target already exists, whereas `UPDATE` admits
with a warning so you can switch to `Shadow`, remove *`capacityPrior`*, or supply manual capacities
while reconciliation sets `CapacityPriorsReady=False` and `Blocked=True`.

### Encoder replica ratios

For a multimodal *EPD* target ([Encode, Prefill and Decode Walkthrough](encode-prefill-decode.md)),
couple *encode* replica counts via *`ratios`* rather than an encode token-throughput target:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeploymentScaler
metadata:
  name: qwen-scaler
  namespace: gpustack-bench
spec:
  targetRef:
    name: epd-arm3-epd
  roles:
    - name: encode
      minReplicas: 1
      maxReplicas: 2
    - name: prefill
      minReplicas: 1
      maxReplicas: 2
    - name: decode
      minReplicas: 1
      maxReplicas: 2
  algorithm:
    type: TokenThroughput
    tokenThroughput:
      prefillTargetTokensPerSecond: "2000"
      decodeTargetTokensPerSecond: "800"
  ratios:
    - numeratorRole: prefill
      denominatorRole: decode
      target: "1"
    - numeratorRole: encode
      denominatorRole: prefill
      target: "1"
```

## Live scaling

### Step 4: Activate scaling

Change *`spec.mode`* to `Active` in `scaling-policy.yaml`, apply it, and confirm `Bound=True` and
`Active=True`:

```bash
kubectl apply -f scaling-policy.yaml
kubectl -n gpustack-bench get mds autoscale-vllm-pd -o json |
  jq '{conditions: .status.conditions, binding: .status.binding, roles: .status.roles}'
```

Do not hand-edit target *`spec.roles[].replicas`* while the scaler holds `Active=True`; the scaler
owns those replica fields for its active binding term. Drive concurrency-24 load in one terminal and
watch scale-out in another:

```bash
BENCH_INCLUSTER=1 python3 .agents/skills/gpustack-operator-benchmark/scripts/bench_client.py load \
  --endpoint http://autoscaling-vllm-pd-router.gpustack-bench.svc:8000 \
  --model Qwen/Qwen3-8B --arm active --suite autoscaling --label ramp \
  --input-tokens 1024 --output-tokens 256 --concurrency 24 --duration 75 \
  --out /tmp/scaler-ramp.json --requests-out /tmp/scaler-ramp-requests.jsonl
```

```bash
kubectl -n gpustack-bench get modeldeployment autoscaling-vllm-pd -w
kubectl -n gpustack-bench get mds autoscale-vllm-pd -o json |
  jq '{roles: .status.roles, commit: .status.lastCommit,
       pending: .status.pendingCohort, conditions: .status.conditions}'
kubectl -n gpustack-bench get pods,workloads
```

Verify that *`lastCommit`* advances, new `Pod`s reach `Ready`, and router per-worker request counters
increase on the added replicas. If *`pendingCohort`* stays non-null, inspect Kueue quota and `Pod`
events before raising *`maxReplicas`*.

### Step 5: Observe replica retirement

Drop load to concurrency 2 for 600 s (exceeding the default 300 s scale-down stabilization window plus
router withdrawal and drain budgets; setting `0` stores the 300 s default):

```bash
BENCH_INCLUSTER=1 python3 .agents/skills/gpustack-operator-benchmark/scripts/bench_client.py load \
  --endpoint http://autoscaling-vllm-pd-router.gpustack-bench.svc:8000 \
  --model Qwen/Qwen3-8B --arm active --suite autoscaling --label cooldown \
  --input-tokens 1024 --output-tokens 256 --concurrency 2 --duration 600 \
  --out /tmp/scaler-cooldown.json --requests-out /tmp/scaler-cooldown-requests.jsonl
```

Poll retirement and scale-down status during cooldown:

```bash
kubectl -n gpustack-bench get modeldeployment autoscaling-vllm-pd -o json |
  jq '{roles: .status.roles, retirement: .status.retirement}'
kubectl -n gpustack-bench get mds autoscale-vllm-pd -o json |
  jq '{roles: .status.roles, lastScaleDown: .status.lastScaleDown, conditions: .status.conditions}'
```

Retiring replicas step through endpoint withdrawal (`Disqualified`), `Draining`, and `Deleting`
([Pause and retirement](../../modules/model-deployment/runtime-control.md#pause-and-retirement)). Keep
*`maxReplicas`* at or above current counts so planning does not block on `BoundsConflict`.

## Verification

Captured output from the standalone single-role `Server` lifecycle verification run:

```text
PASS | the target's replica count rises under load | the count of the role server is 2 while the heavy phase runs
PASS | the scaler records the vector it committed | the scaler committed server=2 on term 1
PASS | the replica count falls when the scaler is narrowed | the count of the role server is 1 after the scaler was narrowed
PASS | the retirement that carried the count down is observed | the target carried the retirement states Disqualified,Draining
```

That standalone `Server` run verified replica scale-out, new-replica traffic, and `Disqualified` →
`Draining` retirement to 1 Ready survivor, while failing its zero-error gate due to request errors
during scale-out ([Retirement](../../modules/model-deployment/scaling-benchmark.md#retirement)).

### Catalog prior resolution

`Catalog.Resolve` converts an offline benchmark curve into a per-replica token capacity seed. Below is
the shipped profile `(Server, deepseek-ai/DeepSeek-R1-0528, fp8, sglang, amd-mi325x, gpusPerReplica: 8,
tensorParallel: 8)` from `pkg/worker/scaling/priors/priors.yaml` (illustrative `tokens/s/MW` assumes a
~0.0115 MW 8-GPU node; interactivity is `1000 / ITL P90`):

| Concurrency | Per-replica `tokens/s` | `tokens/s/MW` | *TTFT* P90 (ms) | *ITL* P90 (ms) | `tokens/s/user` |
|---|---:|---:|---:|---:|---:|
| 4 | 1875.02 | ~163k | 665.4 | 16.95 | 59.0 |
| 8 | 3167.82 | ~275k | 1001.7 | 18.47 | 54.1 |
| 16 | 4988.96 | ~434k | 1119.7 | 20.82 | 48.0 |
| 32 | 6496.65 | ~565k | 1516.6 | 28.32 | 35.3 |
| 64 | 7630.2 | ~663k | 2636.2 | 44.44 | 22.5 |

Configure a scaler holding P90 *TTFT* `<= 2000 ms` and P90 *ITL* `<= 33 ms` (~30.3 `tokens/s/user`):

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeploymentScaler
metadata:
  name: deepseek-scaler
  namespace: gpustack-bench
spec:
  targetRef:
    name: deepseek-server
  roles:
    - name: server
      minReplicas: 1
      maxReplicas: 4
  algorithm:
    type: TokenThroughput
    tokenThroughput:
      latencyQuantile: "0.90"  # default: 0.95
      ttftTargetMilliseconds: "2000"
      itlTargetMilliseconds: "33"
    capacityPrior:
      headroom: "0.75"
```

Resolution proceeds in four steps:

1. Matches the normalized `(role, model, quantization, engine, hardware, topology)` tuple automatically.
2. Filters sweep points against the P90 targets: concurrencies `4`–`32` pass, while `64` breaches both
   (`HigherLoadSLOFailure`; if all points pass, capacity caps at `64` as `ExplicitSaturation` without
   extrapolation).
3. Picks the highest-throughput feasible point (`concurrency: 32` at `6496.65 tokens/s`).
4. Multiplies by *`headroom`*: `6496.65 × 0.75 = 4872.49 tokens/s` per replica, so `15,000 tokens/s`
   demand plans `ceil(15000 / 4872.49) = 4` replicas.

### Capacity estimate comparison

To compare `frozen-manual`, `tuned-manual`, and `catalog` configurations on a single-`Server`
`ModelDeployment` (no *`router`*, *`kvCache`*, or *`kvTransfer`*, and offline `evaluationTraceID`
disjoint from `fitRunIDs`), copy
`.agents/skills/gpustack-operator-benchmark/manifests/autoscaling-comparison.json`, fill in
`REPLACE_WITH_*` placeholders, and validate offline:

```bash
bash .agents/skills/gpustack-operator-benchmark/cases/case-5-autoscaling.sh \
  --capacity-comparison --config FILE --out DIR --validate-only
python3 .agents/skills/gpustack-operator-benchmark/scripts/autoscaling_report.py \
  ARM_DIR --out REPORT_JSON
```

`--validate-only` makes zero cluster calls and exits `0` (valid) or `2` (input error); `--execute`
runs at least three repeats per configuration and writes `comparison.json` (`0` success, `1`
round/cleanup failure, `2` validation error, `130` interrupt). See
[Capacity estimate comparison](../../modules/model-deployment/scaling-benchmark.md#capacity-estimate-comparison)
for the 18-round H200 benchmark results.

## Cleanup

Delete the `ModelDeploymentScaler` first so its finalizer clears the `scaler-binding` annotation, then
delete the target `ModelDeployment`:

```bash
kubectl -n gpustack-bench delete modeldeploymentscaler autoscale-vllm-pd --wait=true
kubectl -n gpustack-bench get modeldeployment autoscaling-vllm-pd -o json |
  jq '.metadata.annotations["modeldeployment.gpustack.ai/scaler-binding"]'
kubectl -n gpustack-bench delete modeldeployment autoscaling-vllm-pd --wait=true
```

## Troubleshooting

| Symptom | Signal | Resolution |
|---|---|---|
| Target *`replicas`* stay at `1` | `Active=False` (`Reason: ShadowMode`) | Expected in `Shadow` mode; set `spec.mode: Active` on `ModelDeploymentScaler` when ready. |
| Apply rejected with `AlreadyBound` | Another scaler owns `scaler-binding` | Edit the existing `ModelDeploymentScaler`, or delete it and wait for binding release first. |
| `Reason: InvalidBinding` | Corrupted `scaler-binding` annotation on target | Remove or repair `modeldeployment.gpustack.ai/scaler-binding` on the target `ModelDeployment`. |
| `Reason: BoundsConflict` or `RatioConflict` | Live replica counts fall outside policy bands | Widen *`minReplicas`*/*`maxReplicas`* or *`ratios`* on `ModelDeploymentScaler` to include current counts. |
| `MetricsReady=False/WaitingForMetrics` | Missing successive metric samples | Wait at least two sampling intervals and verify Ready role `Pod`s expose engine metrics. |
| `CapacityPriorsReady=False` | `CapacityPriorUnresolved`, `InsufficientLoadCoverage`, or `IncompatibleMetric` | Check `catalogRef`, precision annotation/`--quantization`, single-`Pod` role eligibility, and latency targets, or switch to explicit manual capacities. |
| `Reason: ResourceExhausted` | Scale-out blocked by Kueue quota | Inspect *`status.pendingCohort`*, `Workload`s, and `ClusterQueue` capacity. |
| `Reason: ScaleDownAborted` | Replica drain aborted | Inspect *`status.lastScaleDown`* and target *`status.retirement`*; wait out the abort cooldown after fixing drain blockers. |
| `Reason: PacingBounded` | Cooldown or step cap still active | Wait for *`stabilizationWindowSeconds`* and retirement grace timers to elapse. |
| Router errors during scale-out | Client 5xx or connection resets | Capture router and engine logs; `VLLM_HTTP_TIMEOUT_KEEP_ALIVE` reduces but does not eliminate all scale-out resets. |
| SGLang TCP KV transfer errors | Engine port-exhaustion logs | Enable [`model-deployment-tcp-tw-reuse`](../../modules/model-deployment/engine-versions.md#limitations) after permitting `net.ipv4.tcp_tw_reuse` on kubelets. |

---

**See also** — [Model Deployment Scaling](../../modules/model-deployment/scaling.md) (autoscaler
contracts and capacity priors) · [Model Deployment Scaling Benchmark](../../modules/model-deployment/scaling-benchmark.md)
(H100 and H200 measurements) · [Online Runtime Control](../../modules/model-deployment/runtime-control.md)
(role pause guard and retirement lifecycle)

**Next** → [Online Runtime Control Walkthrough](runtime-control.md) — tune serving parameters live and test role pause.
