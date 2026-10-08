# Model Scaling Walkthrough

Prefetch a model, start one elastic vLLM instance, and grow its expert-parallel collective.
The recorded run used one CPU node and one node with eight NVIDIA RTX PRO 6000 Blackwell GPUs.
Captured custom-resource output appears beside the checks it supports.

## Contents

- [Choose the scaling operation](#choose-the-scaling-operation)
- [Prepare the cluster](#prepare-the-cluster)
- [Prefetch the weights](#prefetch-the-weights)
- [Start the elastic deployment](#start-the-elastic-deployment)
- [Check the baseline](#check-the-baseline)
- [Expand the collective](#expand-the-collective)
- [Inspect a stalled resize](#inspect-a-stalled-resize)
- [Scale independent replicas](#scale-independent-replicas)
- [Release the application](#release-the-application)

## Choose the scaling operation

| Operation | Field | Effect | Engine scope |
|---|---|---|---|
| Independent serving replicas | `roles[].replicas` | Starts or retires complete instances, each with its own model and engine. | vLLM or SGLang; use the [engine version requirements](engine-versions.md). |
| Elastic DP/EP width | `roles[].elasticEp.width` | Changes the DP engines and EP collective inside one serving instance. | vLLM with Elastic EP, a compatible MoE model, and a full Ray installation. |

The elastic path keeps `replicas: 1`, `size: 1`, and TP=2 throughout.
Here, `width` is the DP size. Increasing it from 2 to 4 grows the EP world from 4 to 8.
Width 2 consumes four whole GPUs. Width 4 consumes eight.
TP is fixed after creation. This release supports width increases; width decreases are refused.
See [Elastic EP](elastic-ep.md) for the complete contract.

The recorded validation covers elastic TP2/DP2 startup and DP2→4 expansion with short inference requests.
The independent-replica example is a configuration alternative and was not executed in this run.
Neither path adds Kubernetes nodes or cloud capacity.

## Prepare the cluster

Install the operator and enable node model delivery using the
[installation guide](../../operate/installation-modes.md) and
[model-store operations](../model-delivery/operations.md).
The commands below require `kubectl`, `jq`, `curl`, and access to cluster-scoped model-store resources.
Use an isolated kubeconfig for the test cluster.

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

Read the actual operator Pod's image identity and binary version. Repeat for worker, device-manager,
and model-manager Pods. Set the namespace, Pod, and container names from your installation:

```bash
kubectl get pods -A
kubectl -n "$OPERATOR_NAMESPACE" get pod "$OPERATOR_POD" -o json |
  jq '.status.containerStatuses[] | {name, image, imageID}'
kubectl -n "$OPERATOR_NAMESPACE" exec "$OPERATOR_POD" -c "$OPERATOR_CONTAINER" -- \
  gpustack-operator --version
```

Recorded binary output:

```text
gpustack-operator version dev (d00f8fa8c98334a71af4c6eb302ee80d08289309)
```

The `dev` tag can move. A matching tag string alone does not identify the binary under test.
Confirm eight free GPUs, model-manager readiness, and enough disk for the weights before starting.

```bash
kubectl get nodes
kubectl get devices -o json
kubectl get instancetypes
kubectl get nodemodelstores -o json
```

## Prefetch the weights

Save the following as `model-delivery.yaml`. Replace both `gpu-node` values with your GPU node's
`kubernetes.io/hostname` label. The store grant and the prefetch placement must select that same node.
The fixed file list resolves to 12 files and 31,418,807,311 bytes.

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: gpustack-elastic
---
apiVersion: worker.gpustack.ai/v1
kind: ModelArtifact
metadata:
  name: deepseek-v2-lite-chat
  namespace: gpustack-elastic
spec:
  source:
    huggingFace:
      repository: deepseek-ai/DeepSeek-V2-Lite-Chat
      revision: 85864749cd611b4353ce1decdb286193298f64c7
  allowPatterns:
    - model-00001-of-000004.safetensors
    - model-00002-of-000004.safetensors
    - model-00003-of-000004.safetensors
    - model-00004-of-000004.safetensors
    - config.json
    - configuration_deepseek.py
    - generation_config.json
    - model.safetensors.index.json
    - modeling_deepseek.py
    - tokenization_deepseek_fast.py
    - tokenizer.json
    - tokenizer_config.json
---
apiVersion: worker.gpustack.ai/v1
kind: ModelStore
metadata:
  name: rtx6000-models
spec:
  nodeSelector:
    matchLabels:
      kubernetes.io/hostname: gpu-node
---
apiVersion: worker.gpustack.ai/v1
kind: ModelStoreBinding
metadata:
  name: rtx6000-models
  namespace: gpustack-elastic
spec:
  storeRefs:
    - name: rtx6000-models
  quota:
    bytes: 64Gi
  allowPinned: true
---
apiVersion: worker.gpustack.ai/v1
kind: ModelPrefetch
metadata:
  name: deepseek-v2-lite-chat
  namespace: gpustack-elastic
spec:
  artifactRef:
    name: deepseek-v2-lite-chat
  bindingRef:
    name: rtx6000-models
  placement:
    nodeSelector:
      matchLabels:
        kubernetes.io/hostname: gpu-node
  minReady: 1
  retention:
    pinned: true
```

```bash
kubectl apply -f model-delivery.yaml
kubectl -n gpustack-elastic get modelartifacts,modelprefetches
kubectl -n gpustack-elastic get modelprefetch deepseek-v2-lite-chat -o yaml
kubectl get nodemodelstores -o json
```

Captured `kubectl get modelartifacts,modelprefetches` output:

```text
NAME                                                     SOURCE                              REVISION       SIZE          READY   DOWNLOADING   RESOLVED
modelartifact.worker.gpustack.ai/deepseek-v2-lite-chat   deepseek-ai/DeepSeek-V2-Lite-Chat   85864749cd61   31418807311   1       0             True

NAME                                                     ARTIFACT                BINDING          READY   DESIRED   AVAILABLE
modelprefetch.worker.gpustack.ai/deepseek-v2-lite-chat   deepseek-v2-lite-chat   rtx6000-models   1       1         True
```

Check the matching node's model entry as well:

```bash
kubectl get nodemodelstores -o json |
  jq '.items[] | .status.models[]? | {digest, sizeBytes, state}'
```

Captured projection:

```json
{
  "digest": "sha256:1369aae0b5cc2d0e260557bcea04b0d6aad40f8bb6accdac60f1ee543887d9f7",
  "sizeBytes": 31418807311,
  "state": "Ready"
}
```

The validation also retained the successful warm-up Pod and compared its 12 SHA-256 results with
the resolved manifest. Warm-up Pods are transient; collect their logs before completion if you need
that evidence. See [Model Prefetch](../model-delivery/prefetch.md).
Wait for the required content before creating the serving workload.

## Start the elastic deployment

Save this as `elastic.yaml`. The InstanceType matches the recorded RTX PRO 6000 hardware.
For another GPU family, use an appropriate existing whole-GPU InstanceType and verify model compatibility.
Each GPU member requests two GPUs; its CPU and memory budgets scale with that count.

```yaml
apiVersion: worker.gpustack.ai/v1
kind: InstanceType
metadata:
  name: elastic-cpu-gpu
spec:
  generalGroup: generic
  acceleratable: true
  acceleratorGroup: nvidia-rtx-pro-6000-blackwell-server-edition
  os: linux
  arch: amd64
  localStorage: 10Gi
  unitResources:
    cpu: "4"
    ram: 16Gi
---
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
See [Shared memory](deployment.md#shared-memory) before changing the budget.

The recorded test also installed a read-only diagnostic extension to measure native parallel groups.
That test instrumentation is omitted from this application manifest.

This deployment has one serving instance and no router. Only its master exposes the inference API;
the other GPU members participate through Ray. The run tests collective expansion and inference recovery.
Router forwarding, replica load balancing, and failover are outside this recorded test.

## Check the baseline

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

## Expand the collective

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

These checks do not establish sustained concurrency, uninterrupted requests throughout resizing,
per-rank request coverage, or inference quality. The response's `system_fingerprint` retained its startup DP2 text after
expansion, so it was not used to establish the effective width.

## Inspect a stalled resize

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

An earlier run reached `CREATED` placement groups and eight live GPU workers, but new EngineCore actors
remained pending and requests timed out. This recorded run completed expansion and inference.
The earlier root cause remains unconfirmed; the result does not isolate shared memory or head placement as its cause.

## Scale independent replicas

Use a separate deployment without `elasticEp` when you need independent serving instances.
This alternative was not runtime-tested in the recorded elastic run.
First [release the elastic application](#release-the-application) so it does not consume all eight GPUs.
Keep the shared artifact, cache, and InstanceType.

Save this as `replicas.yaml`:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: replica-demo
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
      resources:
        accelerator: "2"
      extraArgs:
        - --tensor-parallel-size
        - "2"
        - --max-model-len
        - "4096"
        - --enforce-eager
```

```bash
kubectl apply -f replicas.yaml
kubectl -n gpustack-elastic patch modeldeployment replica-demo --type=json \
  -p '[{"op":"replace","path":"/spec/roles/0/replicas","value":2}]'
kubectl -n gpustack-elastic get modeldeployment replica-demo -o yaml
kubectl -n gpustack-elastic get pods
```

Two replicas each request two GPUs, for four GPUs total. Each replica has its own TP2 engine.
Read role readiness and endpoint eligibility, and send inference to each instance before declaring both usable.
Managed routing is a separate configuration; see [Routing](routing.md).
SGLang also supports independent replicas, with its own runner image and engine arguments.

## Release the application

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

The release check used an earlier test-runner deployment on the same otherwise idle node.
Captured projection after deleting the earlier application:

```json
{
  "cards": 8,
  "free": 8
}
```

That release check also verified deletion of the owned head, GPU Pods, Services, and Workload.
For a shared cluster, compare the application's captured device claims with the ledger;
other workloads may keep cards occupied. If you ran the replica alternative, delete `replica-demo` as well.

ModelPrefetch keeps the model pinned independently of the serving application.
When no consumer needs the weights, remove the prefetch and its dedicated binding, artifact, and store.
See [Model Prefetch](../model-delivery/prefetch.md) for cache retention and collection behavior.

Application deletion leaves the Kubernetes nodes, disks, and cloud network in place.
Retain them while the owner is inspecting logs. Delete infrastructure only after the owner approves,
using the provisioner's recorded resource ownership, and verify deletion in the provider and state file.

---

**See also** — [Elastic EP](elastic-ep.md) (profile contract) ·
[Model Prefetch](../model-delivery/prefetch.md) (weight delivery) ·
[GPU Instances](../instances/_index.md) (direct GPU requests)

**Next** → [Model Deployment Status](status.md) — interpret conditions and serving endpoints.
