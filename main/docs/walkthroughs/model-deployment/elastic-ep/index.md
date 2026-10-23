# Elastic EP Walkthrough

Grow one vLLM serving collective on an eight-GPU H100 SXM5 node, widen it while it serves, and verify that requests keep completing across the resize.

## Contents

- [Summary](#summary)
- [Environment](#environment)
- [Step 1: Start the elastic deployment](#step-1-start-the-elastic-deployment)
- [Step 2: Verify baseline inference](#step-2-verify-baseline-inference)
- [Step 3: Expand and verify inference](#step-3-expand-and-verify-inference)
- [Step 4: Release the application](#step-4-release-the-application)
- [Limitations](#limitations)
- [Troubleshooting](#troubleshooting)

## Summary

*Elastic EP* expands a running *mixture-of-experts (MoE)* collective without replacing surviving GPU `Pod`s.
It uses Ray and *internal load balancing (Internal LB)*. Only the serving master accepts inference requests.
The CPU-only Ray head coordinates the cluster and claims no GPU.

The role keeps *`replicas: 1`*, *`size: 1`* and two GPUs per member.
The *`elasticEp.width`* field controls member count: width 2 uses four GPUs; width 4 uses eight.
Increasing ordinary *`replicas`* creates separate complete instances and does not expand this collective.

Hold *TP* = 2 and expand *DP* width 2 → 4, increasing the *EP* world size from 4 to 8.
The captured load completed **439/439 client requests with zero errors** during expansion.

| Object | Scope | Creator | Purpose |
|---|---|---|---|
| `ModelArtifact` | namespace | user, via the prefetch procedure | the DeepSeek V2 Lite weights the deployment serves |
| `ModelDeployment` | namespace | user | the elastic vLLM collective that expands in place |
| `InstanceType` | cluster | operator | the whole-GPU H100 type each member requests |

At concurrency 64, width 4 reduced *time to first token (TTFT)* p50 to **2,598.867 ms** from width 2's **6,509.231 ms**, or **2.50× lower**.
Width growth suits high-concurrency first-token targets when process retention matters.
It used twice the GPUs and lowered output throughput at every tested concurrency.
Single-request *inter-token latency (ITL)* p50 rose from 8.566 to 15.675 ms.
Use [Elastic EP Benchmark](/gpustack-operator/main/docs/modules/model-deployment/elastic-ep-benchmark/index.md) to assess generation latency and GPU cost before widening.

## Environment

| Item | Captured setup |
|---|---|
| Cluster | Kubernetes v1.35.7; one CPU node and one GPU node (`gpu-node-0`) |
| Hardware | 8 × NVIDIA H100 80 GB HBM3 SXM5; physical NVLink bandwidth 900 GB/s |
| Operator | GPUStack Operator v0.9.1 |
| Runner | `gpustack/runner:cuda13.0-vllm0.30.0` |
| Model | `deepseek-ai/DeepSeek-V2-Lite-Chat`, `ModelArtifact` `deepseek-v2-lite-chat`, served as `/models` |
| Captured `InstanceType` | Custom `bench-gpu`, selecting the H100 accelerator group |
| Initial allocation | Two GPU members × 2 GPUs = 4 GPUs, plus one CPU-only Ray head |
| Expanded allocation | Four GPU members × 2 GPUs = 8 GPUs, plus the retained Ray head |

Each GPU `Pod` exposed only its assigned two GPUs, with private IPC and `/dev/shm`.
*`hostIPC`* was disabled, and no remote direct memory access (*RDMA*) interface was attached.
NVLink carried *TP* traffic within each `Pod`.
**The *EP* collective between GPU `Pod`s used TCP sockets over `eth0`.**

Enable [node model delivery](/gpustack-operator/main/docs/modules/model-delivery/operations/index.md).
Follow [model prefetch](/gpustack-operator/main/docs/walkthroughs/model-delivery/prefetch/index.md) with namespace `gpustack-bench` and the H100 node's hostname label.
Keep the DeepSeek artifact name and revision from that procedure. Confirm the artifact is ready before serving.
The store and binding names can differ; they must grant this namespace access to the selected node.

## Step 1: Start the elastic deployment

Keep all eight GPUs available so expansion can reserve its additional members.
The reusable manifest uses the discovered H100 `InstanceType`.
To match the captured custom queue, replace its *`instanceType`* value with `bench-gpu`.
Save the manifest as `elastic-ep.yaml`:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: b2-elastic-ep
  namespace: gpustack-bench
spec:
  model:
    name: /models
    artifactRef:
      name: deepseek-v2-lite-chat
  engine:
    name: vLLM
    image: gpustack/runner:cuda13.0-vllm0.30.0
  roles:
    - name: server
      kind: Server
      replicas: 1
      size: 1
      instanceType: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
      image: gpustack/runner:cuda13.0-vllm0.30.0
      imagePullPolicy: IfNotPresent
      resources:
        accelerator: "2"
      elasticEp:
        width: 2
      extraArgs:
        - --tensor-parallel-size
        - "2"
        - --max-model-len
        - "8192"
        - --max-num-seqs
        - "16"
        - --gpu-memory-utilization
        - "0.90"
        - --elastic-ep-max-dp-size
        - "4"
        - --all2all-backend
        - allgather_reducescatter
        - --api-server-count
        - "1"
      env:
        - name: VLLM_USE_RAY_V2_EXECUTOR_BACKEND
          value: "1"
        - name: OTEL_METRICS_EXPORTER
          value: none
        - name: OTEL_TRACES_EXPORTER
          value: none
        - name: RAY_LOGGER_LEVEL
          value: info
        - name: RAY_BACKEND_LOG_LEVEL
          value: info
        - name: RAY_DEDUP_LOGS
          value: "0"
```

```bash
kubectl apply -f elastic-ep.yaml
kubectl -n gpustack-bench get modeldeployment b2-elastic-ep
kubectl -n gpustack-bench get pods \
  -l app.kubernetes.io/instance=b2-elastic-ep -o wide
```

The two GPU members and CPU head ran on `gpu-node-0` in the capture.
The head has no GPU allocation; namespace admission rules can still add CPU and memory requests.
The captured command includes `--elastic-ep-max-dp-size 4`, which reserves the tested engine's maximum width.
Keep it when reproducing this runner configuration.

No Router is declared. The serving `Service` selects the master; the remaining members participate through Ray.
The ordinary role status reports one replica group even when the collective has several GPU members.

## Step 2: Verify baseline inference

Save a baseline CR and `Pod` snapshot before changing the width:

```bash
kubectl -n gpustack-bench get modeldeployment b2-elastic-ep -o json > baseline-cr.json
kubectl -n gpustack-bench get pods \
  -l app.kubernetes.io/instance=b2-elastic-ep -o json > baseline-pods.json
jq '{name: .metadata.name, width: .spec.roles[0].elasticEp.width,
     phase: .status.phase, desired: .status.roles[0].desired,
     ready: .status.roles[0].ready}' baseline-cr.json
```

Captured projection:

```json
{
  "name": "b2-elastic-ep",
  "width": 2,
  "phase": "Ready",
  "desired": 1,
  "ready": 1
}
```

The captured role declared *TP* = 2 and *Internal LB*.
Its `ReplicasUpToDate` condition was `Unknown`, with reason `RolloutNotObserved`.
**A `Ready` phase does not establish that every condition is true.**
Declared arguments also do not prove the effective runtime width.

Forward the serving endpoint in another terminal:

```bash
kubectl -n gpustack-bench port-forward service/b2-elastic-ep 8000:8000
```

Send a streaming generation request and preserve its response:

```bash
curl --fail-with-body --no-buffer --max-time 60 \
  http://127.0.0.1:8000/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"/models","messages":[{"role":"user","content":"Explain GPU scheduling briefly."}],"max_tokens":64,"temperature":0,"stream":true,"stream_options":{"include_usage":true}}' \
  -o baseline-stream.txt
cat baseline-stream.txt
```

Require nonempty generated text, positive completion tokens, and the final `data: [DONE]` event.
A health response does not exercise generation. Keep these new checks distinct from the captured load results below.

## Step 3: Expand and verify inference

Record the master's UID and restart count. The master has *`pod-ordinal`* zero in the *Server* role:

```bash
jq '.items[] |
    select(.metadata.labels["app.kubernetes.io/component"] == "server") |
    select(.metadata.labels["modeldeployment.gpustack.ai/pod-ordinal"] == "0") |
    {name: .metadata.name, uid: .metadata.uid,
     containers: [.status.containerStatuses[] | {name, restartCount}]}' baseline-pods.json

kubectl -n gpustack-bench patch modeldeployment b2-elastic-ep --type=json \
  -p '[{"op":"test","path":"/spec/roles/0/elasticEp/width","value":2},{"op":"replace","path":"/spec/roles/0/elasticEp/width","value":4}]'
```

Wait for the current generation's effective width. A spec edit or four running `Pod`s alone does not prove a native resize.
Read the diagnostic observation belonging to this deployment:

```bash
kubectl -n gpustack-bench get modeldeployment b2-elastic-ep -o json > current-cr.json
MD_UID=$(jq -r '.metadata.uid' current-cr.json)
MD_GENERATION=$(jq -r '.metadata.generation' current-cr.json)
kubectl -n gpustack-bench get configmaps -o json |
  jq --arg uid "$MD_UID" '[.items[] |
      select(any(.metadata.ownerReferences[]?; .uid == $uid)) |
      select(.data["observation.json"] != null) |
      .data["observation.json"] | fromjson] |
      if length == 1 then .[0] else error("expected one observation") end' > observation.json
jq -e --argjson generation "$MD_GENERATION" '
  .observedGeneration == $generation and
  ([.desired, .admitted, .allocated, .ray, .effective] |
   all(.Known == true and .Value == 4))' observation.json
```

The `ConfigMap` is diagnostic state, not a public CR status contract.
The captured observation reported generation 2, stable width 4, and all five width observations known at 4.
Its reason was `native ranks and forwards agree`.
If the check fails, inspect the hold reason before changing the width again.

Save the final CR and compare `Pod` identities with the baseline:

```bash
kubectl -n gpustack-bench get modeldeployment b2-elastic-ep -o json > scaled-cr.json
kubectl -n gpustack-bench get pods \
  -l app.kubernetes.io/instance=b2-elastic-ep -o json > scaled-pods.json
jq '{name: .metadata.name, width: .spec.roles[0].elasticEp.width,
     phase: .status.phase, desired: .status.roles[0].desired,
     ready: .status.roles[0].ready}' scaled-cr.json
```

Captured projection:

```json
{
  "name": "b2-elastic-ep",
  "width": 4,
  "phase": "Ready",
  "desired": 1,
  "ready": 1
}
```

`ReplicasUpToDate` remained `Unknown/RolloutNotObserved` after expansion.
The captured `Pod`s were all running on `gpu-node-0`:

| Captured `Pod` | Role | Ready | UID across resize | Restarts |
|---|---|---|---|---:|
| `b2-elastic-ep-b2-elastic-ep-elastic-head-95n5k` | CPU Ray head | 1/1 | Retained | 0 |
| `b2-elastic-ep-server-tsc8r` | Serving master | 1/1 | Retained | 0 |
| `b2-elastic-ep-server-vzdtc` | Existing GPU member | 1/1 | Retained | 0 |
| `b2-elastic-ep-server-6nvwx` | Added GPU member | 1/1 | New | 0 |
| `b2-elastic-ep-server-z22ml` | Added GPU member | 1/1 | New | 0 |

The captured load used concurrency 8 with target 1,024 input and 256 output tokens per request.
It completed **439/439 requests in 264.227 seconds (264.23 s) with zero errors** across the resize window.
Surviving `Pod`s retained their UIDs and restart counts.
The master's access log contained 1,320 completion requests, including warm-up and calibration.

The harness's `wait_ready` returned after **12 seconds**, before collective re-rendezvous.
At that point, *`status.phase`* remained `Ready` while serving continued online.
This phase-only check did not require current-generation effective width.

The patch was submitted at 08:16:11Z.
In `scaled-cr.json`, `RoleKindsReady` and `EndpointEligibility` transitioned to `True` at 08:19:20Z, **about 189 seconds later**.
This interval covers initialization and Ray collective joining for the two added GPU `Pod`s.
The final `observation.json` separately confirmed effective width 4 without a convergence timestamp.

The `scaling-refusals-503.txt` capture recorded zero client-facing HTTP 503 responses.
The master recorded **10 `/health` responses with HTTP 503** during collective re-rendezvous.
The `elastic-watch-timeline.txt` capture sampled approximately every **16–21 seconds**, with occasional intervals up to 28 seconds.

It recorded `Ready 1S` at 08:18:50Z, **`Starting 0S` at 08:19:09Z**, and `Ready 1S` at 08:19:30Z.
The exact readiness withdrawal duration is unknown.
Repeat the streaming request after the width observation passes; resizing can withdraw readiness or return 503.

## Step 4: Release the application

Stop the port-forward and delete the serving application:

```bash
kubectl -n gpustack-bench delete modeldeployment b2-elastic-ep --wait=true
kubectl -n gpustack-bench get pods,services,deployments,workloads \
  -l app.kubernetes.io/instance=b2-elastic-ep
kubectl get devices -o json
```

Verify deletion of the Ray head, GPU members and owned resources. Confirm the application's GPU claims are released.
The post-deletion `Devices` snapshot recorded all eight accelerators with *`remaining`* = `1600000`.
This establishes released accelerator claims within that snapshot's scope.
Deleting the application retains the prefetched model and cluster infrastructure.
See [Model Prefetch](/gpustack-operator/main/docs/modules/model-delivery/prefetch/index.md) for cache retention and collection.

## Limitations

The capture covers one model, one runner and one H100 node, with width 2 → 4 at *TP* = 2.
It does not establish other widths, cross-node resizing or zero interruption under other traffic.
Only the master serves HTTP. Width decreases are refused by this release.

These observations cover intra-`Pod` NVLink and cross-`Pod` TCP.
They do not establish *EP* performance over cross-`Pod` NVLink or *RDMA*.

The 439-request run verifies client completion during this expansion.
It does not prove that every token gap stayed unchanged, or that every CR condition became true.
Use [Elastic EP Benchmark](/gpustack-operator/main/docs/modules/model-deployment/elastic-ep-benchmark/index.md) for the separately scoped performance comparison.

## Troubleshooting

| Symptom | Signal | Check & Mitigation |
|---|---|---|
| Added members stay Pending | Status: member `Pod` phase `Pending` | Read Kueue `Workload`s, queue quota, `Pod` events and free `Devices` capacity. Width 4 with *TP* = 2 needs eight GPUs. |
| Four members run but effective width is unknown | Status: `effective.Known` false in the diagnostic observation | Read the diagnostic observation and master logs. Allocated GPU capacity alone does not establish native width. |
| `ElasticResize=False/ScaleRefused` | Condition: `ElasticResize=False`, Reason: `ScaleRefused` | Read the last engine refusal and the [resize hold rules](/gpustack-operator/main/docs/modules/model-deployment/elastic-ep/index.md#when-a-resize-holds). |
| `ElasticResize=Unknown/WorldUnprovable` | Condition: `ElasticResize=Unknown`, Reason: `WorldUnprovable` | Keep observing the current operation. An unanswered resize must not be resent manually. |
| Inference stalls | Streaming requests stop returning tokens | Inspect the master and Ray worker logs, actual GPU visibility, `/dev/shm` capacity and node memory. |

```bash
kubectl -n gpustack-bench get modeldeployment b2-elastic-ep -o yaml
kubectl -n gpustack-bench get pods,events
kubectl -n gpustack-bench logs b2-elastic-ep-server-tsc8r -c main --tail=200
```

Replace the captured `Pod` name with the live master name.
The manifest keeps Ray logging at `info` and disables log deduplication.
Environment changes apply to newly created `Pod`s; preserve logs before replacing a failing deployment.

---

**See also** — [Elastic EP](/gpustack-operator/main/docs/modules/model-deployment/elastic-ep/index.md) (profile contract) ·
[External DP Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/external-dp/index.md) (recorded four-rank H100 deployment)

**Next** → [Model Deployment Status](/gpustack-operator/main/docs/modules/model-deployment/status/index.md)
