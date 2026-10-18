# Encode, Prefill and Decode Walkthrough

Deploy one multimodal model across three disaggregated roles. The `Encode` role runs the visual
encoder, `Prefill` consumes the encoder's embeddings and fills the KV cache, and `Decode` generates
tokens. A managed `llm-d-router` fronts the three roles behind one endpoint.

The operator admits the three roles together: all three Kueue `Workload` objects reserve quota, or
none starts. This page keeps the CR and Pod output captured from a live NVIDIA L40S run.

## Contents

- [Deployment shape](#deployment-shape)
- [Prerequisites](#prerequisites)
- [Step 1: start three-role disaggregated serving](#step-1-start-three-role-disaggregated-serving)
- [Step 2: inspect the rendered encoder and decoder wiring](#step-2-inspect-the-rendered-encoder-and-decoder-wiring)
- [Step 3: send a multimodal request](#step-3-send-a-multimodal-request)
- [Step 4: inspect joint admission and co-location](#step-4-inspect-joint-admission-and-co-location)
- [Step 5: release the application](#step-5-release-the-application)
- [Validation summary](#validation-summary)
- [Troubleshooting](#troubleshooting)

## Deployment shape

A text request flows through prefill and then decode. A multimodal request adds the encoder in
front: the router detects media in the request and picks an encoder first, and the decoder's routing
sidecar coordinates the embedding transfer. See
[The three roles and request flow](../../modules/model-deployment/epd.md#the-three-roles-and-request-flow)
for the role contract.

The shape below uses three GPUs, one per role, all on one accelerator platform. Kueue sees one
`Workload` per role and reserves quota for the three together. Soft affinity prefers prefill beside
decode (weight 100) and encode beside prefill (weight 50). Neither is a requirement.

## Prerequisites

The recorded cluster runs Kubernetes 1.32 with two GPU nodes holding four NVIDIA L40S 48 GB cards
in total, plus one CPU node. Three exclusive cards must be free.

Weights must be readable on the nodes at the path `spec.model.name` names. The recorded run mounted
them from a node-local `hostPath` through `roles[].additionalVolumes`. The
[model prefetch walkthrough](../model-delivery/prefetch.md) shows the shared-cache alternative.

The run used `docker.io/gpustack/runner:cuda13.0-vllm0.30.0` for the engines and the operator's
default router and sidecar images.

## Step 1: start three-role disaggregated serving

Save the following manifest as `case120-epd.yaml`. One `Encode`, one `Prefill` and one `Decode`
replica each request one GPU from the same InstanceType. The `spec.router` stanza deploys
`llm-d-router` in front of the three roles.

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: case120-epd
  namespace: default
spec:
  engine:
    name: vLLM
    image: docker.io/gpustack/runner:cuda13.0-vllm0.30.0
  model:
    name: /models/Qwen2.5-VL-7B-Instruct
  router:
    name: llm-d-router
  roles:
    - name: encode
      kind: Encode
      replicas: 1
      instanceType: gpustack--nvidia-l40s-linux-amd64
      imagePullPolicy: IfNotPresent
      resources:
        accelerator: "1"
      additionalVolumes:
        - hostPath:
            path: /data/models/model_scope/Qwen/Qwen2.5-VL-7B-Instruct
            type: Directory
          mountPath: /models/Qwen2.5-VL-7B-Instruct
          readOnly: true
    - name: prefill
      kind: Prefill
      replicas: 1
      instanceType: gpustack--nvidia-l40s-linux-amd64
      imagePullPolicy: IfNotPresent
      resources:
        accelerator: "1"
      additionalVolumes:
        - hostPath:
            path: /data/models/model_scope/Qwen/Qwen2.5-VL-7B-Instruct
            type: Directory
          mountPath: /models/Qwen2.5-VL-7B-Instruct
          readOnly: true
    - name: decode
      kind: Decode
      replicas: 1
      instanceType: gpustack--nvidia-l40s-linux-amd64
      imagePullPolicy: IfNotPresent
      resources:
        accelerator: "1"
      additionalVolumes:
        - hostPath:
            path: /data/models/model_scope/Qwen/Qwen2.5-VL-7B-Instruct
            type: Directory
          mountPath: /models/Qwen2.5-VL-7B-Instruct
          readOnly: true
```

Apply the manifest and wait for the deployment to reach `Ready`. The `ROLES` column reads
`1R1E1P1D`: one router, then one replica for each of the encoder, prefiller and decoder.

```bash
kubectl apply -f case120-epd.yaml
kubectl get modeldeployment case120-epd -o wide
```

```text
NAME          ENGINE   ROLES      PHASE   AGE
case120-epd   vLLM     1R1E1P1D   Ready   3m5s
```

Four Pods run: one engine Pod per role and the router. The decode Pod shows two containers because
its routing sidecar runs beside the engine.

```bash
kubectl get pods -l app.kubernetes.io/instance=case120-epd -o wide
```

```text
NAME                                  READY   STATUS    RESTARTS   AGE    IP             NODE         NOMINATED NODE   READINESS GATES
case120-epd-decode-k7ks8              2/2     Running   0          3m9s   10.0.53.14     gpu-node-0   <none>           <none>
case120-epd-encode-7wnfl              1/1     Running   0          3m9s   10.0.114.171   gpu-node-1   <none>           <none>
case120-epd-prefill-s2ljm             1/1     Running   0          3m9s   10.0.53.12     gpu-node-0   <none>           <none>
case120-epd-router-85c87bc6fc-fb9gt   2/2     Running   0          3m9s   10.0.53.40     gpu-node-0   <none>           <none>
```

## Step 2: inspect the rendered encoder and decoder wiring

The operator renders the encoder-cache connector for you. The encoder Pod runs `ECCPUConnector` as
the producer with a 1 GiB host buffer and publishes a side-channel socket on port 5601. The
`--mm-encoder-only` flag keeps the language-model weights unloaded.

```yaml
command:
  - vllm
  - serve
  - /models/Qwen2.5-VL-7B-Instruct
  - --ec-transfer-config
  - '{"ec_connector":"ECCPUConnector","ec_role":"ec_producer","ec_connector_extra_config":{"ec_enable_nixl":true,"ec_cpu_bytes":1073741824}}'
  - --enforce-eager
  - --no-enable-prefix-caching
  - --max-num-batched-tokens
  - "114688"
  - --mm-encoder-only
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
  - containerPort: 5601
    name: ec-side-channel
    protocol: TCP
```

The prefill Pod gets the consumer half of the same connector. It also keeps its usual KV transfer
producer and the KV event publisher that the router reads:

```yaml
  - --kv-transfer-config
  - '{"kv_connector":"MooncakeConnector","kv_role":"kv_producer","kv_connector_extra_config":{"mooncake_protocol":"tcp"}}'
  - --ec-transfer-config
  - '{"ec_connector":"ECCPUConnector","ec_role":"ec_consumer","ec_connector_extra_config":{"ec_enable_nixl":true,"ec_cpu_bytes":1073741824}}'
```

The decode Pod pairs the KV consumer on its engine container with a `routing-proxy` sidecar. The
sidecar owns the Pod's HTTP port, proxies to the engine, and carries `--ec-connector=ec-nixl`. That
flag makes it fetch the encoder's embeddings before decoding:

```yaml
initContainers:
  - args:
      - --port=8000
      - --model-server-port=8200
      - --kv-connector=mooncake
      - --mooncake-bootstrap-port=8998
      - --secure-proxy=false
      - --ec-connector=ec-nixl
    image: docker.io/gpustack/mirrored-llm-d-router-disagg-sidecar:v0.11.0
    name: routing-proxy
    restartPolicy: Always
```

Do not set these keys on the roles yourself. The operator owns them, and a role that names one is
refused. See [Encoder-cache connectors](../../modules/model-deployment/epd.md#encoder-cache-connectors).

## Step 3: send a multimodal request

Read the endpoint from the deployment status. It stays empty until the router passes readiness:

```bash
kubectl get modeldeployment case120-epd -o jsonpath='{.status.endpoint}'
```

```text
http://case120-epd-router.default.svc:8081
```

Send a multimodal chat completion to that endpoint from inside the cluster. The recorded request
carried a base64 PNG as an `image_url` part beside a text instruction, which makes the router use the
encoder:

```bash
kubectl run curl-probe --rm -i --restart=Never --image=curlimages/curl -- \
  curl -sS -H 'Content-Type: application/json' \
  --data-binary @request.json \
  http://case120-epd-router.default.svc:8081/v1/chat/completions
```

The response was HTTP 200 with 285 characters of generated text describing the image. To see which
engine did what, read the access line each stage logged:

| Stage | Container | Log line |
|---|---|---|
| Encoder | `case120-epd-encode/main` | `POST /v1/chat/completions` |
| Prefiller | `case120-epd-prefill/main` | `POST /v1/chat/completions` |
| Decoder | `case120-epd-decode/routing-proxy` | `processing multimodal items` |

The router picked an encoder, the encoder handed its embeddings to the prefiller over NIXL, and the
decoder's sidecar pulled them before generating text. The decoder's sidecar, not its engine, logs
the multimodal items it received.

## Step 4: inspect joint admission and co-location

Each role owns one Kueue `Workload`. All three read `ADMITTED True` against the same capacity pool,
so no role starts alone:

```bash
kubectl get workloads
```

```text
NAME                              QUEUE                             RESERVED IN                         ADMITTED   FINISHED   AGE
gpustack-fnv64-5857f84abebd42ca   gpustack-fnv64-a730f1dca9e26fca   gpustack--nvidia-l40s-linux-amd64   True                  3m15s
gpustack-fnv64-7e269c4ef1cfbda6   gpustack-fnv64-a730f1dca9e26fca   gpustack--nvidia-l40s-linux-amd64   True                  3m15s
gpustack-fnv64-e4b1dda7357b15d6   gpustack-fnv64-a730f1dca9e26fca   gpustack--nvidia-l40s-linux-amd64   True                  3m15s
```

Every replica Pod also carries soft co-location terms toward its peers. The weights pair the roles
that exchange the most data:

| Replica | Prefers on the same node | Weight |
|---|---|---|
| `encode` | `prefill` | 50 |
| `prefill` | `decode` | 100 |
| `prefill` | `encode` | 50 |
| `decode` | `prefill` | 100 |

In the recorded run, prefill and decode landed together on `gpu-node-0` and the encoder landed on
`gpu-node-1`. Each node had two free cards, so three single-GPU roles could not share one host.
Soft affinity scores a placement and never blocks one. See
[Scheduling co-location](../../modules/model-deployment/epd.md#scheduling-co-location).

## Step 5: release the application

Delete the deployment to release the three GPUs, the router and the Services:

```bash
kubectl delete modeldeployment case120-epd
kubectl get pods,services -l app.kubernetes.io/instance=case120-epd
```

No Pods or Services remain once the delete completes. An encoder replica is withdrawn from
discovery and then removed after a fixed window. See the last rows of [Troubleshooting](#troubleshooting).

## Validation summary

The model was `Qwen/Qwen2.5-VL-7B-Instruct`, served with `vLLM` `0.30.0` on the cluster described in
[Prerequisites](#prerequisites). The Pod suffixes differ between applications because the Pods use
`generateName`.

| Check | Result |
|---|---|
| Admission | The three-role deployment was admitted on `gpustack--nvidia-l40s-linux-amd64` |
| Readiness | Encode, Prefill and Decode Pods `Ready`; every container restart count is 0 |
| Endpoint | `http://case120-epd-router.default.svc:8081` |
| Encoder wiring | `ECCPUConnector` as `ec_producer`, side channel on the Pod IP, port 5601 |
| Prefiller wiring | `ECCPUConnector` as `ec_consumer`, no side-channel port |
| Decoder wiring | `--ec-connector=ec-nixl` on `routing-proxy` |
| Multimodal request | HTTP 200 with generated content |
| Stage logs | Encoder and prefiller logged the request, and the decoder's sidecar logged `processing multimodal items` |

The stage logs matter because a 200 alone does not show which engines ran.

### SGLang `EP + D` behind both routers

SGLang `0.5.18` ran the same model on NVIDIA L40S behind `llm-d-router` and again behind
`sglang-gateway`. Both reached `1R1E1P1D` `Ready` with no restarts and returned the same answers.

| Reading | Result |
|---|---|
| Published endpoint | `http://sglang-epd-llmd-router.default.svc:8081` |
| Encoder flags | The encoder runs `--encoder-only`; the prefiller runs `--language-only` and dials the encoder |
| Text-only request | HTTP 200: `The capital of France is Paris.` |
| `image_url` request | HTTP 200: `The image consists of a red and blue checkerboard pattern with alternating squares.` |
| Image tokens billed | `prompt_tokens_details.image_tokens = 4` |
| Encoder reached | The encoder Pod logged `POST /encode`, called from the prefiller Pod |
| Decoder served | The decode Pod logged `POST /v1/chat/completions`, called from the router |

Send SGLang requests to `status.endpoint`. The deployment's plain Service does not front a
disaggregated SGLang server and answers HTTP 400. The co-location order was not exercised on this
engine, because the pool held a single two-GPU node.

## Troubleshooting

| Symptom | Condition / Reason | Check & Mitigation |
|---|---|---|
| The apply is refused for the role set | Admission refusal message | An `Encode` role requires `Prefill` and `Decode`, admits one role per kind, is refused beside `Server`, and cannot take `spec.kvCache` or a role `command`. See [Shape rules and admission constraints](../../modules/model-deployment/epd.md#shape-rules-and-admission-constraints) |
| The apply is refused for engine, router or platform | Admission refusal message | All three roles must resolve to one accelerator platform, and the router, engine and platform must match [the admitted matrix](../../modules/model-deployment/epd.md#the-admitted-matrix) (`vllm-router` is refused on Ascend). See [The admitted matrix](../../modules/model-deployment/epd.md#the-admitted-matrix) |
| The encoder image is older than its row's floor | No admission check reads a version | The floor lives in the image you name: vLLM `0.30.0`+ for `ECCPUConnector`, vllm-ascend `0.30.0rc1`+ for `ECExampleConnector`, SGLang `0.5.10`+ for `encoder-urls`. An image below the floor fails at container start with an `ImagePullBackOff` or an unknown-argument exit. See [Reading the table](../../modules/model-deployment/engine-versions.md#reading-the-table) |
| The Ascend encoder starts with no directory to write | `ECExampleConnector` needs a shared directory | Attach a ReadWriteMany claim through `spec.roles[].additionalVolumes[].persistentVolumeClaim` on `Encode` and `Prefill`. Both roles must match `claimName`, `mountPath` and `subPath`, and the encoder's mount must not be read-only |
| The apply names port 5601 or 8997 | Admission refusal on an owned key | The `ECCPUConnector` encoder owns 5601, and an SGLang `Prefill` role owns 8997 once the deployment has an `Encode` role. Move the engine's HTTP port off these |
| An encoder replica sits in `Draining` for minutes | The encoder exports no in-flight queue the drain can read | Retirement waits a fixed window of about 300 seconds. A transfer still in flight when it ends fails at the client |
| `RoleKindsReady=False` | One role kind has zero ready replicas | Read the condition message for the kind, then inspect that role's Pods and engine logs. The other roles keep running while the failing kind recovers |

---

**See also** — [Model Deployment Encode, Prefill and Decode](../../modules/model-deployment/epd.md) (role contract and the admitted matrix) ·
[Model Deployment Encode, Prefill and Decode Benchmark](../../modules/model-deployment/epd-benchmark.md) (measured three-arm latency and throughput) ·
[Prefill and Decode Walkthrough](prefill-decode.md) (two-role disaggregation) ·
[External DP Walkthrough](external-dp.md) (fixed per-rank serving)

**Next** → [Model Deployment Status](../../modules/model-deployment/status.md) — read a deployment's conditions when a role misbehaves.
