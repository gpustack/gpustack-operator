# Model Deployment Scaling Walkthrough

Configure a `ModelDeploymentScaler` in `Shadow` mode, inspect its recommendations, then permit
replica changes with `Active`. The examples use a vLLM *prefill/decode (PD)* pair and explain how to
adapt the scaler for *encode/prefill/decode (EPD)* serving. Captured lifecycle observations are labeled
separately from the reusable configuration.

## Contents

- [Prerequisites](#prerequisites)
- [Step 1: Prepare the target](#step-1-prepare-the-target)
- [Step 2: Observe in Shadow](#step-2-observe-in-shadow)
- [Step 3: Choose an algorithm](#step-3-choose-an-algorithm)
- [Step 4: Activate scaling](#step-4-activate-scaling)
- [Step 5: Observe retirement](#step-5-observe-retirement)
- [Verification](#verification)
- [Cleanup](#cleanup)
- [Troubleshooting](#troubleshooting)

## Prerequisites

Follow [cluster preparation](_index.md#prepare-the-cluster) and
[model prefetch](../model-delivery/prefetch.md). Use namespace `gpustack-bench` and artifact
`qwen3-8b` from `Qwen/Qwen3-8B`, revision `b968826d9c46dd6066d109eabc6255188de91218`.
The target needs two free GPUs initially and up to four after scaling, with one GPU per replica.

Use a build that includes `ModelDeploymentScaler`, its controller and admission webhook.
Prepare `kubectl`, `jq`, Python 3 with `aiohttp`, and a source checkout for the load client.
Confirm that no other scaler owns the target. See
[Model Deployment Scaling](../../modules/model-deployment/scaling.md) for algorithm contracts and
[GitOps exclusions](../../modules/model-deployment/scaling.md#gitops-sync).

The [Scaling Benchmark](../../modules/model-deployment/scaling-benchmark.md) used eight H100 GPUs
across concurrent configurations. This procedure starts one deployment.
Its examples are configuration instructions; they are not a claim that this exact public-API sequence
was executed in the recorded benchmark.

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

| Symptom | Check | Action |
|---|---|---|
| Counts remain at one | Mode and `Active` condition | Activate only after reviewing Shadow recommendations. |
| `AlreadyBound` | Binding owner | Use the existing owner or release it before applying another scaler. |
| `InvalidBinding` | Binding annotation on the target | Fix or remove the annotation; the scaler cannot tell which scaler owns the target. |
| `BoundsConflict` or `RatioConflict` | Current counts and policy | Include the current vector in every bound and ratio band. |
| `WaitingForMetrics` | Role signals and engine metrics | Allow valid successive samples; investigate missing or restarted endpoints. |
| `ResourceExhausted` | *`pendingCohort`*, `Workload`s and quota | Restore admission capacity before retrying growth. |
| `ScaleDownAborted` | Retirement reason and *`lastScaleDown`* | Resolve the drain failure; wait through the abort cooldown. |
| `PacingBounded` during cooldown | Stabilization, grace and coverage | Continue observation; a configured cooldown phase does not bypass guards. |
| Router failures during growth | Client errors and Router logs | Preserve evidence; the recorded keep-alive setting did not prevent all failures. |
| SGLang KV transfer failures | Engine logs and TCP port reuse | Follow the [documented setting](../../reference/settings.md#sglang-time-wait-reuse) and verify kubelet prerequisites. |

---

**See also** — [Model Deployment Scaling](../../modules/model-deployment/scaling.md) (public field contracts) ·
[Scaling Benchmark](../../modules/model-deployment/scaling-benchmark.md) (H100 measurements and limits) ·
[Model Deployment Shutdown](../../modules/model-deployment/shutdown.md) (retirement behavior)

**Next** → [Model Deployment Status](../../modules/model-deployment/status.md)
