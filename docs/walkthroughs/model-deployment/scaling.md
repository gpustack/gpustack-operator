# Model Deployment Scaling Walkthrough

Configure a `ModelDeploymentScaler` in `Shadow` mode, inspect its recommendations, then permit
replica changes with `Active`. The examples use a vLLM *prefill/decode (PD)* pair and explain how to
adapt the scaler for *encode/prefill/decode (EPD)* serving. Captured lifecycle observations are labeled
separately from the reusable configuration.

## Contents

- [Summary](#summary)
- [Environment](#environment)
- [Step 1: Prepare the target](#step-1-prepare-the-target)
- [Step 2: Observe in Shadow](#step-2-observe-in-shadow)
- [Step 3: Choose an algorithm](#step-3-choose-an-algorithm)
- [Step 4: Activate scaling](#step-4-activate-scaling)
- [Step 5: Observe retirement](#step-5-observe-retirement)
- [Verification](#verification)
- [Cleanup](#cleanup)
- [Troubleshooting](#troubleshooting)

## Summary

Observe a `ModelDeploymentScaler` in `Shadow`, choose an algorithm, activate it in `Active`, and
watch a replica retire.

| Object | Scope | Creator | Purpose |
|---|---|---|---|
| `ModelArtifact` | namespace | user | the `qwen3-8b` weights the target serves |
| `InstanceType` | cluster | operator | the whole-GPU type each role replica requests |
| `ModelDeployment` | namespace | user | the *PD* target whose replica counts the scaler drives |
| `ModelDeploymentScaler` | namespace | user | the policy observed in `Shadow`, then allowed to act in `Active` |

The [Scaling Benchmark](../../modules/model-deployment/scaling-benchmark.md) used eight H100 GPUs
across concurrent configurations. This procedure starts one deployment.
Its examples are configuration instructions; they are not a claim that this exact public-API sequence
was executed in the recorded benchmark.

## Environment

| Item | Value |
|---|---|
| GPUs | two free initially, up to four after scaling; one GPU per replica |
| `InstanceType` | the whole-GPU type of the intended cluster; the example uses the discovered `gpustack--nvidia-h100-80gb-hbm3-linux-amd64` |
| Namespace | `gpustack-bench` |
| Model | `Qwen/Qwen3-8B` from `ModelArtifact` `qwen3-8b`, revision `b968826d9c46dd6066d109eabc6255188de91218` |
| Tools | `kubectl`, `jq`, Python 3 with `aiohttp`; a source checkout provides the load client |

### Prerequisites

Follow [cluster preparation](_index.md#prepare-the-cluster) and
[model prefetch](../model-delivery/prefetch.md) with namespace `gpustack-bench` and artifact
`qwen3-8b`. Use a build that includes `ModelDeploymentScaler`, its controller and admission webhook.

Confirm that no other scaler owns the target. See
[Model Deployment Scaling](../../modules/model-deployment/scaling.md) for algorithm contracts and
[GitOps exclusions](../../modules/model-deployment/scaling.md#gitops-sync).

## Step 1: Prepare the target

Save this example as `scaling-target.yaml`. Replace both *`instanceType`* values with the whole-GPU
type for the intended cluster. The benchmark used `bench-gpu`; the reusable example uses the
discovered H100 type.

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: autoscaling-vllm-pd
  namespace: gpustack-bench
spec:
  model:
    name: /models
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
      instanceType: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
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
      instanceType: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
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

```bash
kubectl apply -f scaling-target.yaml
kubectl -n gpustack-bench get modeldeployment autoscaling-vllm-pd -w
```

Wait for *`status.phase: Ready`*. Verify one Ready replica in each role and a working Router
before creating load. The vLLM flags enable the Router's
[token-counting path](../../modules/model-deployment/prefill-decode.md#counting-engine-tokens).
The longer HTTP keep-alive matches the recorded configuration; it did not eliminate every scale-out failure.

## Step 2: Observe in Shadow

Save this scaler as `scaling-policy.yaml`. Its role names match the target exactly.
The ratio target prefers equal counts; add bounds only when a fixed ratio is required.

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
      target: "1"
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
kubectl -n gpustack-bench get mds autoscale-vllm-pd -o yaml
kubectl -n gpustack-bench get modeldeployment autoscaling-vllm-pd -o json \
  | jq '{replicas: [.spec.roles[] | {name, replicas}],
         binding: .metadata.annotations["modeldeployment.gpustack.ai/scaler-binding"]}'
```

In `Shadow`, expect `Active=False` with reason `ShadowMode`, unchanged target counts and no binding
owned by this scaler. `Bound=True` means the target is available; it does not mean a Shadow scaler
owns its replica counts. Inspect *`status.roles[].rawRecommendedReplicas`*,
*`constrainedReplicas`*, *`signalState`* and *`reason`* together.

Prepare `default/bench-driver` before measuring. If it is absent, create it:

```bash
kubectl -n default run bench-driver --image=python:3.11-slim --restart=Never \
  --command -- sleep infinity
```

Wait for readiness and install the client dependency if the driver does not already carry it:

```bash
kubectl -n default wait pod/bench-driver --for=condition=Ready --timeout=300s
kubectl -n default exec bench-driver -- python3 -m pip install aiohttp
kubectl -n default exec bench-driver -- python3 -c 'import aiohttp'
```

Run a small load from the source checkout. The client executes inside that driver against the
Router `Service`. Stop if it reports local fallback; local execution cannot measure this endpoint.

```bash
BENCH_INCLUSTER=1 python3 .agents/skills/gpustack-operator-benchmark/scripts/bench_client.py load \
  --endpoint http://autoscaling-vllm-pd-router.gpustack-bench.svc:8000 \
  --model /models --arm shadow --suite autoscaling --label baseline \
  --input-tokens 1024 --output-tokens 256 --concurrency 2 --duration 45 \
  --out /tmp/scaler-shadow.json --requests-out /tmp/scaler-shadow-requests.jsonl
```

Check the driver's readiness and error output if the load cannot start.
Preserve both the phase JSON and request records before changing modes.
Status is a snapshot of control-plane changes, so unchanged raw metrics do not establish that
sampling stopped. Read engine metrics when a current signal value is needed.

## Step 3: Choose an algorithm

Replace the complete *`spec.algorithm`* mapping in `scaling-policy.yaml` when changing algorithms.
Only the block matching *`type`* is accepted. Apply the edited file while it remains in `Shadow`.
Capacity values below reproduce the benchmark's settings; tune them from measured per-replica capacity.

### Token throughput

```yaml
spec:
  algorithm:
    type: TokenThroughput
    tokenThroughput:
      prefillTargetTokensPerSecond: "1200"
      decodeTargetTokensPerSecond: "300"
```

This is a replacement fragment, not a complete resource. It works for both vLLM and SGLang targets;
the scaler reads the target engine. Establish valid counter windows before judging recommendations.
Read the [SGLang TCP limitation](../../modules/model-deployment/engine-versions.md#limitations)
before running sustained SGLang traffic.

### Predictive

```yaml
spec:
  algorithm:
    type: Predictive
    predictive:
      horizonSeconds: 30
      minPoints: 2
      qLevel: "0.5"
      qTrend: "0.3"
      prefillTokenCapacityPerReplica: "2000"
      decodeTokenCapacityPerReplica: "2000"
      tokenThroughput:
        prefillTargetTokensPerSecond: "1200"
        decodeTargetTokensPerSecond: "300"
```

The nested token targets supply the fallback while the predictor warms up.
Inspect *`status.predictors`* and the role reasons to distinguish fallback from predictive planning.
Earlier growth alone does not prove a forecast caused it; token throughput also grew during baseline
in the [recorded comparison](../../modules/model-deployment/scaling-benchmark.md#scale-out-timing).

### Catalog priors

*`spec.algorithm.capacityPrior`* resolves omitted `TokenThroughput` or `Predictive` capacities from
a `gpustack.ai/v1` catalog. The embedded catalog carries the verified public sweeps imported from
InferenceX; the `gpustack-operator-priors` skill's provenance records list every dataset.

For other evidence, supply a reviewed same-namespace `ConfigMap` through *`catalogRef`* (key
`priors.yaml` by default; the parser accepts YAML and JSON) with a qualified profile that
matches the deployment.

The *`PD`* target in Step 1 sets *`spec.router`*, so it requires manual capacities.
To use `Catalog`, create a separate single-`Pod` *Server* target that satisfies
[Target eligibility](../../modules/model-deployment/scaling.md#target-eligibility). A deployment
whose identity matches an embedded profile resolves against the catalog with no extra setup.

The model and hardware identity are inferred from the target — natural names in the optional
identity annotations also work, and bare `mi325x` or `AMD Instinct MI325X` normalize to the
canonical family `amd-mi325x` via the
[Instance Type Unit Resources](../../reference/instance-type-unit-resources.md) family rules.

The engine image matches by family, so no hand-computed digests or `@sha256` pinning are required.

Omit *`spec.router`*, *`spec.kvCache`*, *`spec.kvTransfer`* and *`Encode`*. The fragment and
command below are configuration examples, not captured output from a live cluster run:

```yaml
spec:
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
        serverTargetTokensPerSecond: "1200"  # explicit fallback override
        ttftTargetMilliseconds: "500"        # required when Server uses Catalog
        itlTargetMilliseconds: "50"          # required when Server uses Catalog
    capacityPrior:
      catalogRef:
        name: autoscaling-priors
        key: priors.yaml
      headroom: "0.75"
```

In this *Server* partial override, *`serverTargetTokensPerSecond`* stays `Explicit` at `"1200"`
while the omitted *`serverTokenCapacityPerReplica`* forecast capacity resolves from `Catalog` by
the role's identity and multiplies its profile seed capacity by *`headroom`*. Both `"1200"` and
`"0.75"` are illustrative configuration values rather than measured hardware capacities; removing
*`capacityPrior`* restores manual capacity validation.

When the target matches an embedded profile, drop the explicit fallback and the `ConfigMap`: the
embedded catalog supplies the capacity and the identity resolves from the target.

```bash
kubectl -n gpustack-bench get mds autoscale-vllm-server -o json \
  | jq '{priors: .status.resolvedCapacityPriors,
         conditions: [.status.conditions[] | select(.type == "CapacityPriorsReady" or .type == "Blocked")]}'
```

When the target exists, `CREATE` rejects an unresolved catalog prior. `UPDATE` admission returns a
warning without writing `status` so you can suspend the scaler, switch to `Shadow`, remove
*`capacityPrior`* or supply explicit capacities; the controller then sets
`CapacityPriorsReady=False`, `Active=False` and `Blocked=True` on reconciliation (eventual
consistency) and stops new replica commits.

### Encode ratios

For a separate multimodal target, follow the
[EPD Walkthrough](encode-prefill-decode.md) with `Qwen/Qwen2.5-VL-7B-Instruct` and artifact
`qwen2-5-vl-7b-instruct`. Create a separate scaler that names that target.
Provide bounds for every role and couple *encode* through a direct ratio.

```yaml
spec:
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
  ratios:
    - numeratorRole: prefill
      denominatorRole: decode
      target: "1"
    - numeratorRole: encode
      denominatorRole: prefill
      target: "1"
```

Combine this fragment with `TokenThroughput` or `Predictive`; the *encode* count follows ratio
coupling rather than an encode token-capacity target. With one GPU per role replica, reserve up to
six GPUs. A *`target`* without *`min`* and *`max`* is a preference.
To require equal counts, set all three to `"1"` and ensure the current vector satisfies that policy.

## Step 4: Activate scaling

Set *`spec.mode: Active`* in `scaling-policy.yaml`, then apply it. Keep the local manifest aligned
with the mode so later applies do not restore `Shadow`.

```bash
kubectl apply -f scaling-policy.yaml
kubectl -n gpustack-bench get mds autoscale-vllm-pd -o json \
  | jq '{conditions: .status.conditions, binding: .status.binding, roles: .status.roles}'
```

Expect `Bound=True` and `Active=True`. The target's binding identifies this scaler and its term.
Let the scaler write the target's replica counts; the ownership contract reserves those writes
while the binding is active.

In one terminal, run higher concurrency. In another, watch the target and scaler.

```bash
BENCH_INCLUSTER=1 python3 .agents/skills/gpustack-operator-benchmark/scripts/bench_client.py load \
  --endpoint http://autoscaling-vllm-pd-router.gpustack-bench.svc:8000 \
  --model /models --arm active --suite autoscaling --label ramp \
  --input-tokens 1024 --output-tokens 256 --concurrency 24 --duration 75 \
  --out /tmp/scaler-ramp.json --requests-out /tmp/scaler-ramp-requests.jsonl
```

```bash
kubectl -n gpustack-bench get modeldeployment autoscaling-vllm-pd -w
kubectl -n gpustack-bench get mds autoscale-vllm-pd -o json \
  | jq '{roles: .status.roles, commit: .status.lastCommit,
         pending: .status.pendingCohort, conditions: .status.conditions}'
kubectl -n gpustack-bench get pods,workloads
```

Compare the committed vector with desired and Ready counts. Confirm the new replica's own request
counter increases; a requested count alone proves neither admission nor serving.
If *`pendingCohort`* remains set, inspect Kueue quota and `Pod` events before increasing ceilings.

## Step 5: Observe retirement

Return to low concurrency and keep traffic running through the full observation window.
The default scale-down stabilization window is 300 s; a 90 s cooldown can finish before any shrink.
Leave additional time for the *decode* grace, Router withdrawal and drain.

```bash
BENCH_INCLUSTER=1 python3 .agents/skills/gpustack-operator-benchmark/scripts/bench_client.py load \
  --endpoint http://autoscaling-vllm-pd-router.gpustack-bench.svc:8000 \
  --model /models --arm active --suite autoscaling --label cooldown \
  --input-tokens 1024 --output-tokens 256 --concurrency 2 --duration 600 \
  --out /tmp/scaler-cooldown.json --requests-out /tmp/scaler-cooldown-requests.jsonl
```

Poll both status documents while the load runs. Repeat these commands to preserve changes:

```bash
kubectl -n gpustack-bench get modeldeployment autoscaling-vllm-pd -o json \
  | jq '{roles: .status.roles, retirement: .status.retirement}'
kubectl -n gpustack-bench get mds autoscale-vllm-pd -o json \
  | jq '{roles: .status.roles, lastScaleDown: .status.lastScaleDown,
         conditions: .status.conditions}'
kubectl -n gpustack-bench get pods,workloads
```

The protocol withdraws the selected replica, enters `Draining`, then commits deletion with state
`Deleting`. Kubernetes may display its deleting `Pod` as `Terminating`.
Read [Shutdown](../../modules/model-deployment/shutdown.md#retirement-protocol) for the complete protocol
and its bounded handoff wait. On `Aborted`, inspect *`lastScaleDown`* and the restored vector.

If a shorter test window is needed, choose a positive downward stabilization window longer than the
sampling interval. Zero stores the default 300 s. Keep *`maxReplicas`* at least as high as the current
count; lowering it below that count blocks planning with `BoundsConflict`.

## Verification

The recorded standalone single-role check produced these observations:

```text
PASS | the target's replica count rises under load | the count of the role server is 2 while the heavy phase runs
PASS | the scaler records the vector it committed | the scaler committed server=2 on term 1
PASS | the replica count falls when the scaler is narrowed | the count of the role server is 1 after the scaler was narrowed
PASS | the retirement that carried the count down is observed | the target carried the retirement states Disqualified,Draining
```

These lines come from a separate *Server* target, not the *PD* example above.
The check also confirmed the new replica served and one Ready survivor remained after retirement.
It failed its all-phases completion criterion because scale-out lost requests.
See the [lifecycle result table](../../modules/model-deployment/scaling-benchmark.md#retirement)
for exact counts and measurement limits.

For a new run, retain client outcomes, new-replica counters, Ready counts and retirement transitions.
Require successful requests during the tested transition before claiming uninterrupted serving.
Missing state samples do not establish a complete `Draining`-to-deletion trace.

### Resolving a catalog prior

`Catalog.Resolve` turns one measured sweep into a capacity seed by walking the trade-off every
benchmark sweep records: as concurrency rises, per-replica throughput — proportional to power
efficiency (`tokens/s/MW` at a fixed node power) — climbs toward saturation, while per-user
interactivity (`tokens/s/user` ≈ `1000 / ITL` milliseconds) falls.

This example resolves the shipped public profile `(Server, deepseek-ai/DeepSeek-R1-0528, fp8,
sglang, amd-mi325x, gpusPerReplica: 8, tensorParallel: 8)` from
`pkg/worker/scaling/priors/priors.yaml`. Its five measured points follow; power efficiency is
illustrative at a ~0.0115 MW eight-accelerator node, and interactivity is `1000 / ITL P90`:

| Concurrency | Per-replica `tokens/s` | `tokens/s/MW` | TTFT P90 (ms) | ITL P90 (ms) | `tokens/s/user` |
|---|---:|---:|---:|---:|---:|
| 4 | 1875.02 | ~163k | 665.4 | 16.95 | 59.0 |
| 8 | 3167.82 | ~275k | 1001.7 | 18.47 | 54.1 |
| 16 | 4988.96 | ~434k | 1119.7 | 20.82 | 48.0 |
| 32 | 6496.65 | ~565k | 1516.6 | 28.32 | 35.3 |
| 64 | 7630.2 | ~663k | 2636.2 | 44.44 | 22.5 |

A `Server` role held to P90 *TTFT* of `2000` ms and P90 *ITL* of `33` ms — a floor of about
`30.3 tokens/s/user` — configures as:

```yaml
algorithm:
  type: TokenThroughput
  tokenThroughput:
    latencyQuantile: "0.90"        # resolve the prior at P90, not the P95 default
    ttftTargetMilliseconds: "2000"
    itlTargetMilliseconds: "33"    # a 30.3 tokens/s/user floor
    # serverTargetTokensPerSecond omitted: it resolves from the catalog
capacityPrior:
  headroom: "0.75"                 # catalogRef omitted: the embedded catalog
```

The resolver then:

1. Matches the profile by the target's normalized `(role, model, quantization, engine, hardware,
   topology)` tuple — no profile identifier is configured.
2. Filters the points against the P90 targets: concurrency `4`, `8`, `16` and `32` meet both;
   concurrency `64` breaches them (TTFT P90 `2636.2` ms, ITL P90 `44.44` ms ≈ `22.5 tokens/s/user`),
   establishing `HigherLoadSLOFailure`. Looser targets that let every point pass would instead cap
   at the measured concurrency `64` — never extrapolated (`ExplicitSaturation`).
3. Selects the feasible point with the highest per-replica `tokens/s` — the `tokens/s/MW` maximum
   under the targets — so `concurrency: 32` seeds the capacity: `6496.65` tokens/s, about
   `565k tokens/s/MW` at `35.3 tokens/s/user`.
4. Applies the headroom: `6496.65 * 0.75 = 4872.49` effective tokens/s per replica. A live demand
   of `15,000` tokens/s therefore plans `ceil(15000 / 4872.49) = 4` replicas — about `3,750`
   tokens/s per replica — keeping every replica inside its interactivity target.

Bounds, pacing and the other guards still apply to that recommendation;
[Capacity comparison](#capacity-comparison) then measures the dynamics these seeds produce.

### Capacity comparison

The initial comparison helper accepts a single-*Server* `ModelDeployment` (`len(roles) == 1` and
`kind: Server`) with no *`spec.router`*, *`spec.kvCache`* or *`spec.kvTransfer`*, and a target-owned
`Service` (`<target>-<role>`) publishing one HTTP port.

Its engine-family image, identity (annotations or inferred) and effective launch flags must match
the offline qualified catalog (`outcome: Accepted` and `evaluationTraceID` disjoint from
`fitRunIDs`). It does not run *Prefill*/*Decode* comparison or require eight GPUs.

The helper compares `frozen-manual`, `tuned-manual` and `catalog` across at least three serial repeats (`alternating` or `seeded`)
that share `Active` mode, `TokenThroughput` or `Predictive`, latency targets, bounds, pacing, image, phases, seed and start condition
outside capacity fields.

For `Predictive`, the manual arms set both forecast and fallback capacities;
the full-Catalog arm omits both. The API still supports partial overrides outside this comparison.
Every scaler sets a positive downward *`stabilizationWindowSeconds`*, and the quiet `cooldown` phase
(`concurrency: 0`) spans at least `stabilizationWindowSeconds` plus startup and retirement allowances.

Copy `.agents/skills/gpustack-operator-benchmark/manifests/autoscaling-comparison.json`
(`REPLACE_WITH_*` values are template placeholders, not a runnable profile) and supply your context,
`clusterQueues`, disjoint `fitRunIDs` and `evaluationTraceID`, prepared driver `Pod`, JSON
deployment/scaler manifests and offline qualified catalog `ConfigMap`. Priors are acquired offline
only, never from live evaluation traffic.

```bash
bash .agents/skills/gpustack-operator-benchmark/cases/case-5-autoscaling.sh \
  --capacity-comparison --config FILE --out DIR --validate-only
python3 .agents/skills/gpustack-operator-benchmark/scripts/autoscaling_report.py \
  ARM_DIR --out REPORT_JSON
```

`--validate-only` makes zero cluster API or driver calls, writes `validated-plan.json` and `catalog-input.json` under `--out`, and
exits `0` when valid or `2` on validation or input error. On a live hardware gate, `--execute` writes per-round `<NN>-<arm>/`
artifacts (`inputs/*.json`, `run.json`, `timeline.jsonl`, `prepare_reqs.jsonl`, `<phase>_reqs.jsonl`, `<phase>.json`, `cleanup.json`,
`report.json`) and root `comparison.json` (`readyReplicaSeconds` and `latenciesByRun`).

`--execute` exits `0` when all rounds, parser checks and UID-preconditioned raw `DELETE` cleanups
succeed across at least three valid repeats per arm, `1` on round or cleanup failure (`130` on
interrupt), and `2` on validation error.

`autoscaling_report.py` writes schema `1` diagnostics (`null` metrics with reasons and no win
verdict when evidence is missing) and exits `0` when valid, `1` when invalid, and `2` when
`ARM_DIR` is missing or the output cannot be written.

See [Scaling Benchmark](../../modules/model-deployment/scaling-benchmark.md#capacity-arm-rounds)
for the recorded 18-round H200 comparison. Legacy invocations (`case-5-autoscaling.sh [OUT_DIR] ...`
or `--legacy-topology`) remain aliases for the five-configuration topology benchmark.

## Cleanup

Delete the scaler first so its finalizer can release the binding. Deleting a scaler leaves its
target serving at the current counts. Delete the target separately when it is no longer needed.

```bash
kubectl -n gpustack-bench delete modeldeploymentscaler autoscale-vllm-pd --wait=true
kubectl -n gpustack-bench get modeldeployment autoscaling-vllm-pd -o json \
  | jq '.metadata.annotations["modeldeployment.gpustack.ai/scaler-binding"]'
kubectl -n gpustack-bench delete modeldeployment autoscaling-vllm-pd --wait=true
```

Remove a separate multimodal target and scaler the same way if created.
Keep shared prefetched artifacts and `default/bench-driver` when other procedures use them.

## Troubleshooting

| Symptom | Signal | Check & Mitigation |
|---|---|---|
| Counts remain at one | Condition: `Active=False` with reason `ShadowMode` | Activate only after reviewing Shadow recommendations. |
| Apply is refused | Reason: `AlreadyBound` | Read the binding owner; use the existing owner or release it before applying another scaler. |
| Binding annotation is unreadable | Reason: `InvalidBinding` | Fix or remove the annotation on the target; the scaler cannot tell which scaler owns the target. |
| Counts sit outside the policy | Reason: `BoundsConflict` or `RatioConflict` | Compare the current counts with the policy; include the current vector in every bound and ratio band. |
| Recommendations wait on metrics | Reason: `WaitingForMetrics` on the `MetricsReady` condition | Allow valid successive samples from role signals and engine metrics; investigate missing or restarted endpoints. |
| Catalog prior fails to resolve | Condition: `CapacityPriorsReady=False` (`CapacityPriorUnresolved`, `InsufficientLoadCoverage` or `IncompatibleMetric`) | Check `catalogRef`, the target identity (annotations or inferred), engine-family image, single-`Pod` topology and explicit latency targets, or remove *`capacityPrior`* / supply explicit capacities. |
| Growth stops at admission | Reason: `ResourceExhausted` | Read *`pendingCohort`*, `Workload`s and quota; restore admission capacity before retrying growth. |
| Scale-down ends aborted | Reason: `ScaleDownAborted` | Read the retirement reason and *`lastScaleDown`*; resolve the drain failure and wait through the abort cooldown. |
| Cooldown outlives its window | Reason: `PacingBounded` | Check stabilization, grace and coverage; continue observation, since a configured cooldown phase does not bypass guards. |
| Router failures during growth | Log: Router logs and client errors | Preserve evidence; the recorded keep-alive setting did not prevent all failures. |
| SGLang KV transfer failures | Log: engine logs and TCP port reuse | Follow the [documented setting](../../reference/settings.md#sglang-time-wait-reuse) and verify kubelet prerequisites. |

---

**See also** — [Model Deployment Scaling](../../modules/model-deployment/scaling.md) (public field contracts) ·
[Scaling Benchmark](../../modules/model-deployment/scaling-benchmark.md) (H100 and H200 measurements and limits) ·
[Model Deployment Shutdown](../../modules/model-deployment/shutdown.md) (retirement behavior)

**Next** → [Model Deployment Status](../../modules/model-deployment/status.md)
