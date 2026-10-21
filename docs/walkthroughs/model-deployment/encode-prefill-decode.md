# Encode, Prefill and Decode Walkthrough

Deploy one multimodal model across three disaggregated roles. The *Encode* role runs the visual
encoder, *Prefill* consumes the encoder's embeddings and fills the KV cache, and *Decode* generates
tokens. A managed `llm-d-router` fronts the three roles behind one endpoint.

## Contents

- [Summary](#summary)
- [Device and cluster](#device-and-cluster)
- [Step 1: Start three-role serving](#step-1-start-three-role-serving)
- [Step 2: Inspect role configuration](#step-2-inspect-role-configuration)
- [Step 3: Send a multimodal request](#step-3-send-a-multimodal-request)
- [Step 4: Admission and co-location](#step-4-admission-and-co-location)
- [Step 5: Release the application](#step-5-release-the-application)
- [Limitations](#limitations)
- [Troubleshooting](#troubleshooting)

## Summary

*Encode*, *Prefill* and *Decode* disaggregation isolates compute-heavy multimodal vision encoding onto
*Encode* `Pod`s. Separating this work can reduce interference from large-image bursts during token generation.
Cross-role transfers and queueing still affect *time to first token (TTFT)* and *inter-token latency (ITL)*.

A text request flows through *Prefill* and then *Decode*. A multimodal request adds the encoder in
front: the router detects media in the request and picks an encoder first, and the decoder's
routing sidecar coordinates the embedding transfer.
See [Roles and request flow](../../modules/model-deployment/encode-prefill-decode.md#roles-and-request-flow)
for the role contract.

The configuration uses three GPUs, one per role. Kueue sees one `Workload` per role and reserves
quota for the three together. Soft affinity prefers *prefill* beside *decode* (weight 100) and
*encode* beside *prefill* (weight 50). Neither is a requirement.

The H100 benchmark supports large-image isolation and repeated-image or mixed workloads: *ITL* p99 improved **15.1×–21.8×**
at `4k`/`8k`, concurrency 8; repeated-image *TTFT* p50 improved **5.9×** at concurrency 8.
The comparison used one GPU for aggregated serving, two for *PD* and three for *EPD*.
Small-prompt first-token latency often favored aggregated serving.
See [Encode, Prefill and Decode Benchmark](../../modules/model-deployment/encode-prefill-decode-benchmark.md) for measured benefits and costs.

## Device and cluster

The recorded run used this cluster and model setup:

| Item | Captured setup |
|---|---|
| Cluster | Kubernetes v1.35.7; one CPU node and one GPU node (`gpu-node-0`) |
| GPUs | 8 × NVIDIA H100 80 GB HBM3 SXM5; three GPUs assigned to roles |
| `InstanceType` | `bench-gpu`, configured for the H100 accelerator group |
| Model | `qwen2-5-vl-7b-instruct` for `Qwen/Qwen2.5-VL-7B-Instruct`; node model delivery mounts the weights at `/var/lib/gpustack/model`, served as `/models` |
| Runtime | vLLM `gpustack/runner:cuda13.0-vllm0.30.0`, with `llm-d-router` |

## Step 1: Start three-role serving

Create `ModelArtifact` `qwen2-5-vl-7b-instruct` for `Qwen/Qwen2.5-VL-7B-Instruct` in `gpustack-bench`
and prefetch it onto the H100 node using the [model prefetch walkthrough](../model-delivery/prefetch.md)
(substituting `qwen2-5-vl-7b-instruct` and `gpustack-bench` for its DeepSeek example).

Save the following manifest as `epd-deployment.yaml`. It uses the discovered H100 `InstanceType`
name `gpustack--nvidia-h100-80gb-hbm3-linux-amd64`.

The recorded run used its custom `bench-gpu` `InstanceType`, configured for the same accelerator group.
Replace all three `instanceType` values with `bench-gpu` to reproduce that configuration exactly.

One *Encode*, one *Prefill* and one *Decode* replica each request one GPU. The `artifactRef` points to
the existing `ModelArtifact`; *`spec.model.name`* exposes it to vLLM as `/models`. The *`spec.router`*
stanza deploys `llm-d-router` in front of the roles.

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: epd-arm3-epd
  namespace: gpustack-bench
spec:
  model:
    name: /models
    artifactRef:
      name: qwen2-5-vl-7b-instruct
  engine:
    name: vLLM
    image: gpustack/runner:cuda13.0-vllm0.30.0
  router:
    name: llm-d-router
  roles:
    - name: encode
      kind: Encode
      replicas: 1
      size: 1
      instanceType: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
      image: gpustack/runner:cuda13.0-vllm0.30.0
      imagePullPolicy: IfNotPresent
      resources:
        accelerator: "1"
      extraArgs:
        - --max-model-len
        - "16384"
        - --gpu-memory-utilization
        - "0.85"
    - name: prefill
      kind: Prefill
      replicas: 1
      size: 1
      instanceType: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
      image: gpustack/runner:cuda13.0-vllm0.30.0
      imagePullPolicy: IfNotPresent
      resources:
        accelerator: "1"
      extraArgs:
        - --tensor-parallel-size
        - "1"
        - --max-model-len
        - "16384"
        - --gpu-memory-utilization
        - "0.85"
    - name: decode
      kind: Decode
      replicas: 1
      size: 1
      instanceType: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
      image: gpustack/runner:cuda13.0-vllm0.30.0
      imagePullPolicy: IfNotPresent
      resources:
        accelerator: "1"
      extraArgs:
        - --tensor-parallel-size
        - "1"
        - --max-model-len
        - "16384"
        - --gpu-memory-utilization
        - "0.85"
```

Apply the manifest and wait for the deployment to reach `Ready`. The `ROLES` column reads
`1R1E1P1D`: one router, then one replica for each of the encoder, prefiller and decoder.

```bash
kubectl apply -f epd-deployment.yaml
kubectl get modeldeployment epd-arm3-epd -n gpustack-bench \
  -o custom-columns=NAME:.metadata.name,ENGINE:.spec.engine.name,ROLES:.status.roleSummary,PHASE:.status.phase
```

```text
NAME          ENGINE   ROLES      PHASE
epd-arm3-epd  vLLM     1R1E1P1D   Ready
```

The captured wide listing below omits the IP and scheduling-gate columns.
Four `Pod`s run: one engine `Pod` per role and the router. The *Decode* `Pod` shows two containers
because its routing sidecar runs beside the engine.

```bash
kubectl get pods -n gpustack-bench -l app.kubernetes.io/instance=epd-arm3-epd \
  -o wide
```

```text
NAME                                  READY   STATUS    RESTARTS   AGE   NODE
epd-arm3-epd-decode-h62cw               2/2     Running   0          95s   gpu-node-0
epd-arm3-epd-encode-9jz5c               1/1     Running   0          94s   gpu-node-0
epd-arm3-epd-prefill-26ls5              1/1     Running   0          95s   gpu-node-0
epd-arm3-epd-router-6cc94d94b8-glnvt     2/2     Running   0          93s   gpu-node-0
```

## Step 2: Inspect role configuration

The operator renders the encoder-cache connector for you. The encoder `Pod` runs `ECCPUConnector` as
the producer with a 1 GiB host buffer and publishes a side-channel socket on port 5601. The
`--mm-encoder-only` flag keeps the language-model weights unloaded.

`--enforce-eager` and `--no-enable-prefix-caching` are operator defaults rendered on the *Encode*
role only, so `epd-deployment.yaml` declares neither in *`extraArgs`*. See
[Keys the operator owns](../../modules/model-deployment/encode-prefill-decode.md#keys-the-operator-owns)
for the rationale; do not copy `--enforce-eager` onto *Prefill* or *Decode*, which keep CUDA graphs.

The following blocks are captured `Pod` excerpts, not manifests to apply. They include the `Pod`
identity and container nesting so each generated field has its Kubernetes context; probes, volumes and
other generated fields are omitted where they do not explain the configuration.

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: epd-arm3-epd-encode-9jz5c
  namespace: gpustack-bench
spec:
  containers:
    - name: main
      command:
        - vllm
        - serve
        - /var/lib/gpustack/model
        - --ec-transfer-config
        - '{"ec_connector":"ECCPUConnector","ec_role":"ec_producer","ec_connector_extra_config":{"ec_enable_nixl":true,"ec_cpu_bytes":1073741824}}'
        - --enforce-eager
        - --no-enable-prefix-caching
        - --max-num-batched-tokens
        - "114688"
        - --mm-encoder-only
        - --served-model-name
        - /models
        - --max-model-len
        - "16384"
        - --gpu-memory-utilization
        - "0.85"
        - --host
        - 0.0.0.0
        - --port
        - "8000"
      env:
        - name: VLLM_USE_V2_MODEL_RUNNER
          value: "1"
        - name: VLLM_EC_SIDE_CHANNEL_HOST
          valueFrom:
            fieldRef:
              fieldPath: status.podIP
        - name: VLLM_EC_SIDE_CHANNEL_PORT
          value: "5601"
      ports:
        - containerPort: 8000
          name: http
          protocol: TCP
        - containerPort: 5601
          name: ec-side-channel
          protocol: TCP
```

The *Prefill* `Pod` gets the consumer half of the same connector. It also keeps its usual KV transfer
producer and the KV event publisher that the router reads:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: epd-arm3-epd-prefill-26ls5
  namespace: gpustack-bench
spec:
  containers:
    - name: main
      command:
        - vllm
        - serve
        - /var/lib/gpustack/model
        - --kv-transfer-config
        - '{"kv_connector":"MooncakeConnector","kv_role":"kv_producer","kv_connector_extra_config":{"mooncake_protocol":"tcp"}}'
        - --ec-transfer-config
        - '{"ec_connector":"ECCPUConnector","ec_role":"ec_consumer","ec_connector_extra_config":{"ec_enable_nixl":true,"ec_cpu_bytes":1073741824}}'
        - --kv-events-config
        - '{"enable_kv_cache_events":true,"publisher":"zmq","endpoint":"tcp://*:5557","replay_endpoint":"tcp://*:5558","buffer_steps":10000,"hwm":100000,"max_queue_size":100000,"topic":"kv@"}'
        - --served-model-name
        - /models
        - --tensor-parallel-size
        - "1"
        - --max-model-len
        - "16384"
        - --gpu-memory-utilization
        - "0.85"
        - --host
        - 0.0.0.0
        - --port
        - "8000"
```

The *Decode* `Pod` pairs the KV consumer on its engine container with a `routing-proxy` sidecar. The
sidecar owns the `Pod`'s HTTP port, proxies to the engine, and carries `--ec-connector=ec-nixl`. That
flag makes it fetch the encoder's embeddings before decoding. Kubernetes 1.35 renders this as a
native sidecar:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: epd-arm3-epd-decode-h62cw
  namespace: gpustack-bench
spec:
  initContainers:
    - name: routing-proxy
      restartPolicy: Always
      args:
        - --port=8000
        - --model-server-port=8200
        - --kv-connector=mooncake
        - --mooncake-bootstrap-port=8998
        - --secure-proxy=false
        - --ec-connector=ec-nixl
  containers:
    - name: main
      command:
        - vllm
        - serve
        - /var/lib/gpustack/model
        - --kv-transfer-config
        - '{"kv_connector":"MooncakeConnector","kv_role":"kv_consumer","kv_connector_extra_config":{"mooncake_protocol":"tcp"}}'
        - --served-model-name
        - /models
        - --tensor-parallel-size
        - "1"
        - --max-model-len
        - "16384"
        - --gpu-memory-utilization
        - "0.85"
        - --host
        - 0.0.0.0
        - --port
        - "8200"
```

Do not set these keys on the roles yourself. The operator owns them, and a role that names one is
refused. See [Encoder-cache connectors](../../modules/model-deployment/encode-prefill-decode.md#encoder-cache-connectors).

## Step 3: Send a multimodal request

Read the endpoint from the deployment status. It stays empty until the router passes readiness:

```bash
kubectl get modeldeployment epd-arm3-epd -n gpustack-bench -o jsonpath='{.status.endpoint}'
```

```text
http://epd-arm3-epd-router.gpustack-bench.svc:8000
```

Send a multimodal chat completion to that endpoint from inside the cluster.
Include a base64 PNG as an `image_url` part beside a text instruction to select the encoder:

```bash
kubectl run curl-probe -n gpustack-bench --rm -i --restart=Never --image=curlimages/curl -- \
  curl -sS -H 'Content-Type: application/json' \
  --data-binary @- \
  http://epd-arm3-epd-router.gpustack-bench.svc:8000/v1/chat/completions < request.json
```

The completed benchmark issued **236 measured requests with zero errors** and 28 warm-up requests.
The captured logs contain 264 `POST /v1/chat/completions` lines in each role's `main` container and
264 multimodal-item lines in the *Decode* `routing-proxy` sidecar. The router access log contains 280
lines for this run.

| Stage | Container | Captured log |
|---|---|---|
| *Encode* | `epd-arm3-epd-encode-9jz5c/main` | `POST /v1/chat/completions` (264 lines) |
| *Prefill* | `epd-arm3-epd-prefill-26ls5/main` | `POST /v1/chat/completions` (264 lines) |
| *Decode* | `epd-arm3-epd-decode-h62cw/main` | `POST /v1/chat/completions` (264 lines) |
| *Decode* sidecar | `epd-arm3-epd-decode-h62cw/routing-proxy` | Multimodal items (264 lines) |

The router picked an encoder, the encoder handed its embeddings to the prefiller over NIXL, and the
decoder's sidecar pulled them before generating text. The decoder's sidecar logs the multimodal items it received.
Aggregate counts establish stage activity; they do not correlate each request across the roles.

## Step 4: Admission and co-location

The `ModelDeployment` reports quota reserved for all three role replicas. Each role owns one Kueue
`Workload`; admission reserves the complete role set together. Check the individual objects with:

```bash
kubectl get workloads -n gpustack-bench
```

All three role `Pod`s and the router `Pod` ran on `gpu-node-0` in this recording. The node has eight
GPUs, so this placement does not show how the roles behave when GPU capacity is constrained.

Each role exposed one GPU inside its own `Pod`, with private IPC and `/dev/shm`.
No remote direct memory access (*RDMA*) interface was attached.
**All cross-`Pod` NIXL encoder-cache and Mooncake KV transfers used TCP sockets over `eth0`.**
The host's NVLink fabric was not used between roles.

Every replica `Pod` also carries soft co-location terms toward its peers. The weights pair the roles
that exchange the most data:

| Replica | Prefers on the same node | Weight |
|---|---|---|
| *Encode* | *Prefill* | 50 |
| *Prefill* | *Decode* | 100 |
| *Prefill* | *Encode* | 50 |
| *Decode* | *Prefill* | 100 |

Soft affinity scores a placement and never blocks one. See
[Scheduling co-location](../../modules/model-deployment/encode-prefill-decode.md#scheduling-co-location).

## Step 5: Release the application

Delete the deployment to release the three GPUs, the router and the `Service`s:

```bash
kubectl delete modeldeployment epd-arm3-epd -n gpustack-bench
kubectl get pods,services -n gpustack-bench -l app.kubernetes.io/instance=epd-arm3-epd
```

Wait until the owned `Pod`s and `Service`s disappear; the recording does not include a cleanup snapshot.
An encoder replica is withdrawn from discovery and then removed after a fixed window.
See [Troubleshooting](#troubleshooting).

## Limitations

Role kinds are fixed at creation and do not convert in place between *Encode*, *Prefill*, *Decode*,
and *Server*.

The recording uses one 8-GPU H100 node and one CPU node. The deployment reserves three GPUs together;
all three must be available for admission. All three roles fit on `gpu-node-0` in this run. It does
not measure placement under GPU contention or across multiple GPU nodes.

Encoder retirement drains through a fixed window of about 300 seconds. A transfer still in flight
when the window ends fails at the client.

## Troubleshooting

| Symptom | Condition / Reason | Check & Mitigation |
|---|---|---|
| The apply is refused for the role set | Admission refusal message | An *Encode* role requires *Prefill* and *Decode*, admits one role per kind, is refused beside *Server*, and cannot take *`spec.kvCache`* or a role *`command`*. See [Admission constraints](../../modules/model-deployment/encode-prefill-decode.md#admission-constraints) |
| The apply is refused for engine, router or platform | Admission refusal message | All three roles must resolve to one accelerator platform, and the router, engine and platform must match [the admitted matrix](../../modules/model-deployment/encode-prefill-decode.md#admitted-matrix) (`vllm-router` is refused on Ascend). See [Admitted matrix](../../modules/model-deployment/encode-prefill-decode.md#admitted-matrix) |
| The encoder image is older than its row's floor | No admission check reads a version | The floor lives in the image you name: vLLM `0.30.0`+ for `ECCPUConnector`, vllm-ascend `0.30.0rc1`+ for `ECExampleConnector`, SGLang `0.5.10`+ for `encoder-urls`. An image below the floor fails at container start with an `ImagePullBackOff` or an unknown-argument exit. See [Reading the table](../../modules/model-deployment/engine-versions.md#reading-the-table) |
| Requests bypass *Prefill* | vLLM *Prefill/Decode (PD)* token rendering is unavailable | The router cannot count tokens through the engine and falls back to *Decode*-only routing. See [Counting engine tokens](../../modules/model-deployment/prefill-decode.md#counting-engine-tokens) for supported versions and `--enable-scale-out` placement |
| The Ascend encoder starts with no directory to write | `ECExampleConnector` needs a shared directory | Attach a ReadWriteMany claim through *`spec.roles[].additionalVolumes[].persistentVolumeClaim`* on *Encode* and *Prefill*. Both roles must match *`claimName`*, *`mountPath`* and *`subPath`*, and the encoder's mount must not be read-only |
| The apply names port 5601 or 8997 | Admission refusal on an owned key | The `ECCPUConnector` encoder owns 5601, and an SGLang *Prefill* role owns 8997 once the deployment has an *Encode* role. Move the engine's HTTP port off these |
| An encoder replica sits in `Draining` for minutes | The encoder exports no in-flight queue the drain can read | Retirement waits a fixed window of about 300 seconds. A transfer still in flight when it ends fails at the client |
| `RoleKindsReady=False` | One role kind has zero ready replicas | Read the condition message for the kind, then inspect that role's `Pod`s and engine logs. The other roles keep running while the failing kind recovers |

---

**See also** — [Encode, Prefill and Decode](../../modules/model-deployment/encode-prefill-decode.md) (role contract and the admitted matrix) ·
[Encode, Prefill and Decode Benchmark](../../modules/model-deployment/encode-prefill-decode-benchmark.md) (measured latency and throughput across three configurations) ·
[Prefill and Decode Walkthrough](prefill-decode.md) (two-role disaggregation) ·
[External DP Walkthrough](external-dp.md) (fixed per-rank serving)

**Next** → [Model Deployment Status](../../modules/model-deployment/status.md) — read a deployment's conditions when a role misbehaves.
