# Elastic EP Walkthrough

Start one elastic vLLM instance and grow its expert-parallel collective on the same eight-GPU node.
This example holds TP=2 and expands DP width from 2 to 4, so EP world grows from 4 to 8.
Captured CR output appears beside each check.

## Contents

- [Prerequisites](#prerequisites)
- [Step 1: start the elastic deployment](#step-1-start-the-elastic-deployment)
- [Step 2: verify baseline inference](#step-2-verify-baseline-inference)
- [Step 3: expand and verify inference](#step-3-expand-and-verify-inference)
- [Step 4: release the application](#step-4-release-the-application)
- [Diagnose a stalled resize](#diagnose-a-stalled-resize)
- [Validation record](#validation-record)

## Prerequisites

Complete [cluster and GPU preparation](/gpustack-operator/main/docs/walkthroughs/model-deployment/index.md#prepare-the-cluster) and
[model prefetch](/gpustack-operator/main/docs/walkthroughs/model-delivery/prefetch/index.md).
The [Elastic EP guide](/gpustack-operator/main/docs/modules/model-deployment/elastic-ep/index.md) owns the profile's configuration and limits.

Keep `replicas: 1`, `size: 1`, and TP=2 throughout this walkthrough.
Width 2 consumes four whole GPUs. Width 4 consumes eight.
This release supports width increases; width decreases are refused.
The engine uses Internal LB, and only its master accepts inference requests.

## Step 1: start the elastic deployment

Save this as `elastic.yaml`. Use the artifact and InstanceType from the preparation steps.

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: tp2-elastic
  namespace: gpustack-elastic
spec:
  model:
    name: /models
    artifactRef:
      name: deepseek-v2-lite-chat
  engine:
    name: vLLM
    version: 0.29.0
  roles:
    - name: server
      kind: Server
      replicas: 1
      size: 1
      instanceType: elastic-cpu-gpu
      image: gpustack/runner:cuda13.0-vllm0.29.0@sha256:1c826749ed16fbd9f9594d7a49f774904662d9c46231e08e32b494e8bac4ee91
      imagePullPolicy: IfNotPresent
      resources:
        accelerator: "2"
      elasticEp:
        width: 2
      extraArgs:
        - --tensor-parallel-size
        - "2"
        - --max-model-len
        - "4096"
        - --max-num-seqs
        - "32"
        - --enforce-eager
        - --all2all-backend
        - allgather_reducescatter
        - --api-server-count
        - "1"
      env:
        - name: VLLM_USE_RAY_V2_EXECUTOR_BACKEND
          value: "1"
        - name: RAY_LOGGER_LEVEL
          value: debug
        - name: RAY_BACKEND_LOG_LEVEL
          value: debug
        - name: RAY_DEDUP_LOGS
          value: "0"
```

```bash
kubectl apply -f elastic.yaml
kubectl -n gpustack-elastic get modeldeployments,pods,services
```

The operator creates one CPU-only Ray head without resource requests or limits.
Normal Kubernetes admission and scheduling still apply.
In the recorded run, this head landed on the GPU node but claimed no GPU. The two GPU Pods each exposed two GPUs to Ray autodiscovery.

With `shmSize` omitted, the recorded `/dev/shm` capacity was 16 GiB on every elastic Pod.
Ray's object store used that mount. A separate CPU-only probe verified explicit `shmSize: 32Gi`.
Kubelet can cap the mount by node allocatable memory or Pod memory limits; read the actual mount.
See [Shared memory](/gpustack-operator/main/docs/modules/model-deployment/deployment/index.md#shared-memory) before changing the budget.

The recorded test also installed a read-only diagnostic extension to measure native parallel groups.
That test instrumentation is omitted from this application manifest.

This manifest has one serving instance and no router. Only its master exposes the inference API;
the other GPU members participate through Ray.
A separate recorded run added this `spec.router` block before creating the deployment:

```yaml
router:
  name: vllm-router
  image: gpustack/llm-router:v0.2.0@sha256:98e70d94351baa9dc13897a545aff5c83ffac5e89ab810e204bc894b519d96db
  extraArgs:
    - --policy
    - round_robin
```

The engine still uses Internal LB. This Router forwards to the master, which distributes work to the DP engines.
Use the [Router observation steps](/gpustack-operator/main/docs/walkthroughs/model-deployment/external-dp/index.md#step-4-check-router-coverage) with deployment name `tp2-elastic`.
For this Elastic shape, expect one master endpoint; exclude the CPU head and other GPU members.

## Step 2: verify baseline inference

Save each CR snapshot before changing the width:

```bash
kubectl -n gpustack-elastic get modeldeployment tp2-elastic -o json > baseline-cr.json
jq '{name: .metadata.name, width: .spec.roles[0].elasticEp.width,
     phase: .status.phase, roles: [.status.roles[] |
       {name, desired, quotaReserved, ready, parallelism}]}' baseline-cr.json
```

Projection of the captured baseline CR:

```json
{
  "name": "tp2-elastic",
  "width": 2,
  "phase": "Ready",
  "roles": [
    {
      "name": "server",
      "desired": 1,
      "quotaReserved": 1,
      "ready": 1,
      "parallelism": {
        "declared": {
          "tensorParallel": 2
        },
        "loadBalance": "Internal",
        "source": {
          "complete": true,
          "kind": "ExtraArgs"
        }
      }
    }
  ]
}
```

The role still has one serving instance. `width` is desired state, and `parallelism.declared` describes
arguments. Neither field proves the runtime's effective DP width.
The captured conditions included `ReplicasUpToDate=Unknown` with reason `RolloutNotObserved`;
`phase: Ready` did not turn every condition true.

In a separate terminal, forward the serving Service:

```bash
kubectl -n gpustack-elastic port-forward service/tp2-elastic 8000:8000
```

Send both request types. Keep the headers and response bodies. Each command makes one attempt.
The recorded test sent equivalent requests directly to the master inside the cluster;
the port-forward is a workstation access option.

```bash
curl --fail-with-body --max-time 30 -D ordinary.headers \
  http://127.0.0.1:8000/v1/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"/models","prompt":"Complete the sentence: The sky is","max_tokens":8,"temperature":0,"stream":false}' \
  -o ordinary.json
jq '{text: .choices[0].text, finish_reason: .choices[0].finish_reason,
     completion_tokens: .usage.completion_tokens}' ordinary.json

curl --fail-with-body --no-buffer --max-time 30 -D stream.headers \
  http://127.0.0.1:8000/v1/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"/models","prompt":"Complete the sentence: The sky is","max_tokens":8,"temperature":0,"stream":true,"stream_options":{"include_usage":true}}' \
  -o stream.txt
cat stream.txt
```

Require HTTP 200, nonempty generated text, positive completion tokens, and a final generation reason.
For SSE, require the usage event and `data: [DONE]`. A health response does not exercise generation.
The recorded requests used unique request IDs and checked their response IDs as well.

## Step 3: expand and verify inference

Record the master Pod's UID and restart count before the patch. Select ordinal zero within this deployment:

```bash
kubectl -n gpustack-elastic get pods -o json |
  jq '.items[] | select(.metadata.labels["app.kubernetes.io/instance"] == "tp2-elastic") |
      select(.metadata.labels["app.kubernetes.io/component"] == "server") |
      select(.metadata.labels["modeldeployment.gpustack.ai/pod-ordinal"] == "0") |
      {name: .metadata.name, uid: .metadata.uid,
       containers: [.status.containerStatuses[] | {name, containerID, restartCount}]}'

kubectl -n gpustack-elastic patch modeldeployment tp2-elastic --type=json \
  -p '[{"op":"test","path":"/spec/roles/0/elasticEp/width","value":2},{"op":"replace","path":"/spec/roles/0/elasticEp/width","value":4}]'
```

Wait for the operator's observation of the current generation to reach width 4 at every layer.
Read the diagnostic ConfigMap belonging to this deployment UID:

```bash
kubectl -n gpustack-elastic get modeldeployment tp2-elastic -o json > current-cr.json
MD_UID=$(jq -r '.metadata.uid' current-cr.json)
MD_GENERATION=$(jq -r '.metadata.generation' current-cr.json)
kubectl -n gpustack-elastic get configmaps -o json |
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

If this check fails, continue observing or inspect the hold reason before declaring completion.
The ConfigMap is internal diagnostic state, not a public CR status API.
Its effective-width observation includes rank-directed inference probes.
Pair it with the retained master identity and both client request checks below.

After the observation passes, save the final CR:

```bash
kubectl -n gpustack-elastic get modeldeployment tp2-elastic -o json > scaled-cr.json
jq '{name: .metadata.name, width: .spec.roles[0].elasticEp.width,
     phase: .status.phase, desired: .status.roles[0].desired,
     ready: .status.roles[0].ready}' scaled-cr.json
```

Projection captured after the resize completed:

```json
{
  "name": "tp2-elastic",
  "width": 4,
  "phase": "Ready",
  "desired": 1,
  "ready": 1
}
```

Confirm four GPU members and one head. Read the master and Ray logs, then repeat both inference requests.
The engine may withdraw readiness and return 503 during reconfiguration.
Do not resend the width change while the current operation is unresolved.

## Step 4: release the application

When you have finished inspecting the elastic application, delete only its ModelDeployment:

```bash
kubectl -n gpustack-elastic delete modeldeployment tp2-elastic --wait=true
kubectl -n gpustack-elastic get modeldeployments,pods,services
kubectl get devices -o json > released-devices.json
jq '[.items[].status.groups[].accelerators[] |
     {mode: (.mode // 0), remaining, allocatedSlices: (.allocatedSlices // 0)}] |
    {cards: length, free: map(select(.mode == 0 and .remaining == 1600000 and .allocatedSlices == 0)) | length}' \
  released-devices.json
```

Captured projection after deleting the official-runner Elastic application with its Router:

```json
{
  "cards": 8,
  "free": 8
}
```

That release check also verified deletion of the owned head, GPU Pods, Router, Services, and Workload.
For a shared cluster, compare the application's captured device claims with the ledger;
other workloads may keep cards occupied.

ModelPrefetch keeps the model pinned independently of the serving application.
When no consumer needs the weights, remove the prefetch and its dedicated binding, artifact, and store.
See [Model Prefetch](/gpustack-operator/main/docs/modules/model-delivery/prefetch/index.md) for cache retention and collection behavior.

Application deletion leaves the Kubernetes nodes, disks, and cloud network in place.
Retain them while the owner is inspecting logs. Delete infrastructure only after the owner approves,
using the provisioner's recorded resource ownership, and verify deletion in the provider and state file.

## Diagnose a stalled resize

Read the CR conditions and the owned Pods first. Select the head, master, or worker from the Pod list:

```bash
kubectl -n gpustack-elastic get modeldeployment tp2-elastic -o yaml
kubectl -n gpustack-elastic get pods -o wide
kubectl -n gpustack-elastic get events --sort-by=.metadata.creationTimestamp
kubectl -n gpustack-elastic logs "$POD" -c main --timestamps --tail=200
kubectl -n gpustack-elastic exec "$POD" -c main -- \
  sh -c 'ls /tmp/ray/session_latest/logs'
kubectl -n gpustack-elastic exec "$POD" -c main -- \
  sh -c 'tail -200 /tmp/ray/session_latest/logs/raylet.out /tmp/ray/session_latest/logs/raylet.err'
```

| Symptom | Read next |
|---|---|
| GPU Pods remain Pending | Kueue Workloads, queue quota, Pod events, InstanceType capacity, and the per-card `Devices` ledger. |
| Placement groups exist but inference times out | Master logs, EngineCore actor state, GPU worker logs, and Ray session logs. `CREATED` placement groups alone do not establish inference readiness. |
| Ray sees the wrong GPU count | Actual allocation annotations, container-visible GPUs, and live Ray node capacity. Each GPU member must expose exactly TP GPUs. |
| Unexpected engine arguments or missing Ray | Running image digest and installed vLLM/Ray versions. `spec.engine.version` does not inspect the image. |
| Shared-memory pressure | Actual `/dev/shm` capacity, Pod memory limits, node allocatable memory, and the object store's location. |

`RAY_LOGGER_LEVEL` controls Python Ray logging; `RAY_BACKEND_LOG_LEVEL` controls backend logging.
The operator defaults both levels to `info`.
The manifest sets both to `debug` and disables deduplication for inspection.
Environment changes affect newly created Pods. Preserve current logs before replacing a failing deployment.

## Validation record

The recorded software and model identities were:

| Component | Recorded identity |
|---|---|
| Operator | `gpustack/gpustack-operator:dev`, binary revision `d00f8fa8c98334a71af4c6eb302ee80d08289309` |
| Operator image digest | `sha256:84f85914bdcb398b2f662dfdcb0845ee853597ec5661a5cf41828acae6a11e36` |
| Inference engine | vLLM 0.29.0 with full Ray 2.54.0 |
| Engine image | `gpustack/runner:cuda13.0-vllm0.29.0@sha256:1c826749ed16fbd9f9594d7a49f774904662d9c46231e08e32b494e8bac4ee91` |
| Model | `deepseek-ai/DeepSeek-V2-Lite-Chat` |
| Model revision | `85864749cd611b4353ce1decdb286193298f64c7` |

The official GPUStack runner includes Ray. The run used its installed packages without replacement.
Container reads found `cupy-cuda13x` 14.2.0 and no `cupy-cuda12x` installation.
Verify the image digest and installed vLLM/Ray versions when selecting another runner.
Do not add `--elastic-ep-max-dp-size` to this example; that argument starts with vLLM 0.30.0.

Recorded operator binary output:

```text
gpustack-operator version dev (d00f8fa8c98334a71af4c6eb302ee80d08289309)
```

The recorded result combined native group measurements, actor state, GPU allocation, and completed requests:

| Check | Width 2 | Width 4 |
|---|---|---|
| Actual TP / DP / EP world | 2 / 2 / 4 | 2 / 4 / 8 |
| Distinct allocated GPU UUIDs | 4 | 8 |
| Live EngineCore actors | 2 | 4 |
| Live GPU worker actors | 4 | 8 |
| Ordinary request | HTTP 200, 8 generated tokens | HTTP 200, 8 generated tokens |
| SSE request | HTTP 200, 8 generated tokens, `[DONE]` | HTTP 200, 8 generated tokens, `[DONE]` |

All four recorded requests succeeded on their first attempt. The original master UID and HTTP process
identity remained unchanged. The measurements joined native workers to Ray actors, Pod identities,
and host GPU UUIDs. Placement groups used `STRICT_PACK`, with two GPU bundles per DP engine.

The additional Router run also completed width 2→4 with the same official runner.
Before and after expansion, its selector and registry contained only the original master.
Each stage completed one ordinary request and one SSE request through the Router, with HTTP 200 and eight tokens.
The master's Router counter increased by two at each stage. Worker and CPU-head endpoints were absent.

These checks do not establish sustained concurrency, uninterrupted requests throughout resizing,
per-rank request coverage, or inference quality. The response's `system_fingerprint` retained its startup DP2 text after
expansion, so it was not used to establish the effective width.

An earlier run reached `CREATED` placement groups and eight live GPU workers, but new EngineCore actors
remained pending and requests timed out. This recorded run completed expansion and inference.
The earlier root cause remains unconfirmed; the result does not isolate shared memory or head placement as its cause.

---

**See also** — [Elastic EP](/gpustack-operator/main/docs/modules/model-deployment/elastic-ep/index.md) (profile contract) ·
[External DP Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/external-dp/index.md) (fixed per-rank serving)

**Next** → [Model Deployment Status](/gpustack-operator/main/docs/modules/model-deployment/status/index.md)
