# Encode, Prefill and Decode Walkthrough

Deploy `Qwen/Qwen2.5-VL-7B-Instruct` across dedicated *Encode*, *Prefill*, and *Decode* roles behind
`llm-d-router`, inspect rendered NIXL encoder-cache flags, and verify multimodal completion across
all three stages.

## Contents

- [Summary](#summary)
- [Deployment](#deployment)
- [Verification](#verification)
- [Cleanup](#cleanup)
- [Limitations](#limitations)
- [Troubleshooting](#troubleshooting)

## Summary

*Encode/prefill/decode (EPD)* disaggregation isolates multimodal vision encoding onto *Encode* `Pod`s
so large image bursts do not stall token generation. Text-only requests traverse *Prefill* → *Decode*,
while multimodal requests route through *Encode* → *Prefill* → *Decode*
([Encode, Prefill and Decode](/gpustack-operator/main/docs/modules/model-deployment/encode-prefill-decode/index.md)):

| Object | Scope | Creator | Purpose |
|---|---|---|---|
| `ModelArtifact` | namespace | user | Resolves `Qwen/Qwen2.5-VL-7B-Instruct` weights |
| `ModelPrefetch` | namespace | user | Warms `qwen2-5-vl-7b-instruct` onto the H100 node |
| `AcceleratorType` | cluster | operator | H100 pool requested by all three roles |
| `ModelDeployment` | namespace | user | Runs 1 *Encode*, 1 *Prefill*, and 1 *Decode* replica behind `llm-d-router` |
| Router `Deployment` and `Service` | namespace | operator | Single HTTP endpoint routing multimodal and text requests |

In the H100 benchmark (3 GPUs for *EPD* vs 2 for *PD* and 1 for aggregated serving), *EPD* improved
concurrency-8 `4k`/`8k` *inter-token latency (ITL)* p99 by **15.1×–21.8×** and repeated-image *time
to first token (TTFT)* p50 by **5.9×**, while small-prompt *TTFT* often favored aggregated serving;
see [Encode, Prefill and Decode Benchmark](/gpustack-operator/main/docs/modules/model-deployment/encode-prefill-decode-benchmark/index.md).

### Environment

| Item | Value |
|---|---|
| Cluster | Kubernetes v1.35.7; 1 CPU node and 1 GPU node (`gpu-node-0`) |
| GPUs | 8 × NVIDIA H100 80 GB HBM3 SXM5 (3 GPUs allocated: 1 per role) |
| Pool | Captured on custom `InstanceType` `bench-gpu`; reusable manifest uses `gpustack--nvidia-h100-80gb-hbm3-linux-amd64` |
| Model | `Qwen/Qwen2.5-VL-7B-Instruct` from `ModelArtifact` `qwen2-5-vl-7b-instruct` mounted at `/var/lib/gpustack/model` |
| Runtime | vLLM `gpustack/runner:cuda13.0-vllm0.30.0` with `llm-d-router` |

## Deployment

### Step 1: Start three-role serving

Create and prefetch `ModelArtifact` `qwen2-5-vl-7b-instruct` in `gpustack-bench` using
[Model Prefetch](/gpustack-operator/main/docs/walkthroughs/model-delivery/prefetch/index.md#step-1-create-artifact-and-prefetch), then save
`epd-deployment.yaml`:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: epd-arm3-epd
  namespace: gpustack-bench
spec:
  model:
    name: qwen2-5-vl-7b-instruct
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
      acceleratorType: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
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
      acceleratorType: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
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
      acceleratorType: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
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

Apply `epd-deployment.yaml` and wait for `ROLES=1R1E1P1D` and `PHASE=Ready`:

```bash
kubectl apply -f epd-deployment.yaml
kubectl get modeldeployment epd-arm3-epd -n gpustack-bench \
  -o custom-columns=NAME:.metadata.name,ENGINE:.spec.engine.name,ROLES:.status.roleSummary,PHASE:.status.phase
kubectl get pods -n gpustack-bench -l app.kubernetes.io/instance=epd-arm3-epd
```

Captured output:

```text
NAME          ENGINE   ROLES      PHASE
epd-arm3-epd  vLLM     1R1E1P1D   Ready

NAME                                    READY   STATUS    RESTARTS   AGE
epd-arm3-epd-decode-h62cw               2/2     Running   0          95s
epd-arm3-epd-encode-9jz5c               1/1     Running   0          94s
epd-arm3-epd-prefill-26ls5              1/1     Running   0          95s
epd-arm3-epd-router-6cc94d94b8-glnvt    2/2     Running   0          93s
```

The *Decode* `Pod` runs `2/2` containers (`main` engine plus native `routing-proxy` sidecar). If
admission rejects the manifest or `PHASE` stays `Starting`, see [Troubleshooting](#troubleshooting).

### Step 2: Inspect role configuration

Project the operator-synthesized connector flags, ports, and sidecar arguments across all three engine
roles (do not pass these owned flags in *`extraArgs`*; see
[Encoder-cache connectors](/gpustack-operator/main/docs/modules/model-deployment/encode-prefill-decode/index.md#encoder-cache-connectors)):

```bash
kubectl get pods -n gpustack-bench -l app.kubernetes.io/instance=epd-arm3-epd -o json |
  jq '[.items[] | select(.metadata.labels["app.kubernetes.io/component"] != "router") | {
    role: .metadata.labels["app.kubernetes.io/component"],
    pod: .metadata.name,
    command: .spec.containers[0].command,
    ports: [.spec.containers[0].ports[]?.containerPort],
    proxyArgs: (.spec.initContainers // [] | map(select(.name=="routing-proxy"))[0].args)
  }]'
```

Captured output (condensed per role):

```json
[
  {
    "role": "encode",
    "pod": "epd-arm3-epd-encode-9jz5c",
    "command": [
      "vllm", "serve", "/var/lib/gpustack/model",
      "--ec-transfer-config", "{\"ec_connector\":\"ECCPUConnector\",\"ec_role\":\"ec_producer\",\"ec_connector_extra_config\":{\"ec_enable_nixl\":true,\"ec_cpu_bytes\":1073741824}}",
      "--enforce-eager", "--no-enable-prefix-caching", "--max-num-batched-tokens", "114688",
      "--mm-encoder-only", "--served-model-name", "qwen2-5-vl-7b-instruct",
      "--max-model-len", "16384", "--gpu-memory-utilization", "0.85", "--host", "0.0.0.0", "--port", "8000"
    ],
    "ports": [8000, 5601],
    "proxyArgs": null
  },
  {
    "role": "prefill",
    "pod": "epd-arm3-epd-prefill-26ls5",
    "command": [
      "vllm", "serve", "/var/lib/gpustack/model",
      "--kv-transfer-config", "{\"kv_connector\":\"MooncakeConnector\",\"kv_role\":\"kv_producer\",\"kv_connector_extra_config\":{\"mooncake_protocol\":\"tcp\"}}",
      "--ec-transfer-config", "{\"ec_connector\":\"ECCPUConnector\",\"ec_role\":\"ec_consumer\",\"ec_connector_extra_config\":{\"ec_enable_nixl\":true,\"ec_cpu_bytes\":1073741824}}",
      "--kv-events-config", "{\"enable_kv_cache_events\":true,\"publisher\":\"zmq\",\"endpoint\":\"tcp://*:5557\",\"replay_endpoint\":\"tcp://*:5558\",\"buffer_steps\":10000,\"hwm\":100000,\"max_queue_size\":100000,\"topic\":\"kv@\"}",
      "--served-model-name", "qwen2-5-vl-7b-instruct", "--tensor-parallel-size", "1",
      "--max-model-len", "16384", "--gpu-memory-utilization", "0.85", "--host", "0.0.0.0", "--port", "8000"
    ],
    "ports": [8000],
    "proxyArgs": null
  },
  {
    "role": "decode",
    "pod": "epd-arm3-epd-decode-h62cw",
    "command": [
      "vllm", "serve", "/var/lib/gpustack/model",
      "--kv-transfer-config", "{\"kv_connector\":\"MooncakeConnector\",\"kv_role\":\"kv_consumer\",\"kv_connector_extra_config\":{\"mooncake_protocol\":\"tcp\"}}",
      "--served-model-name", "qwen2-5-vl-7b-instruct", "--tensor-parallel-size", "1",
      "--max-model-len", "16384", "--gpu-memory-utilization", "0.85", "--host", "0.0.0.0", "--port", "8200"
    ],
    "ports": [8200],
    "proxyArgs": [
      "--port=8000", "--model-server-port=8200", "--kv-connector=mooncake",
      "--mooncake-bootstrap-port=8998", "--secure-proxy=false", "--ec-connector=ec-nixl"
    ]
  }
]
```

Confirm three rendered invariants:

- ***Encode*** runs `--mm-encoder-only` (keeping LLM weights unloaded), defaults `--enforce-eager` and
  `--no-enable-prefix-caching` ([Operator-owned keys](/gpustack-operator/main/docs/modules/model-deployment/encode-prefill-decode/index.md#operator-owned-keys)),
  defaults NIXL side-channel port `5601` (`VLLM_EC_SIDE_CHANNEL_PORT=5601`, overridable including `0`),
  and defaults `VLLM_USE_V2_MODEL_RUNNER=1` on `vLLM < 0.34.0` (omitted at `>= 0.34.0`).
- ***Prefill*** pairs `ECCPUConnector` (`ec_consumer`) with `MooncakeConnector` (`kv_producer`) while
  keeping CUDA graphs enabled.
- ***Decode*** serves behind `routing-proxy` on `:8000` (`--ec-connector=ec-nixl` → engine `:8200`).

## Verification

### Step 3: Send a multimodal request

Read *`status.endpoint`* and send a multimodal chat completion containing both an `image_url` (base64
PNG) and a text prompt for `"model": "qwen2-5-vl-7b-instruct"`:

```bash
kubectl get modeldeployment epd-arm3-epd -n gpustack-bench -o jsonpath='{.status.endpoint}{"\n"}'
```

Captured output:

```text
http://epd-arm3-epd-router.gpustack-bench.svc:8000
```

Build a small multimodal chat-completion payload containing a 56×56 RGB PNG `image_url` part beside
a text prompt, then send it to the router from inside the cluster:

```bash
cat <<'EOF' > request.json
{
  "model": "qwen2-5-vl-7b-instruct",
  "max_tokens": 32,
  "messages": [
    {
      "role": "user",
      "content": [
        {"type": "text", "text": "Describe this image briefly."},
        {
          "type": "image_url",
          "image_url": {
            "url": "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAADgAAAA4CAIAAAAn5KxJAAAARElEQVR42u3OQQkAAAgEsItmNKMZzYcphMECLNXzQkRFRUVFRUVFRUVFRUVFRUVFRUVFRUVFRUVFRUVFRUVFRUVFRc8CbIRhD2BnZn0AAAAASUVORK5CYII="
          }
        }
      ]
    }
  ]
}
EOF

kubectl run curl-probe -n gpustack-bench --rm -i --restart=Never --image=curlimages/curl -- \
  curl -fsS -H 'Content-Type: application/json' \
  --data-binary @- \
  http://epd-arm3-epd-router.gpustack-bench.svc:8000/v1/chat/completions < request.json
```

Across **236 measured requests with zero errors** (plus 28 warm-ups; 280 router access-log lines),
container access logs confirm activity at every stage:

| Stage | Container | Captured log count |
|---|---|---|
| *Encode* | `epd-arm3-epd-encode-9jz5c/main` | `POST /v1/chat/completions` (264 lines) |
| *Prefill* | `epd-arm3-epd-prefill-26ls5/main` | `POST /v1/chat/completions` (264 lines) |
| *Decode* | `epd-arm3-epd-decode-h62cw/main` | `POST /v1/chat/completions` (264 lines) |
| *Decode* sidecar | `epd-arm3-epd-decode-h62cw/routing-proxy` | Multimodal items received (264 lines) |

If *Encode* receives requests while *Prefill* receives zero, check vLLM *PD* token-counting flags in
[Troubleshooting](#troubleshooting).

### Step 4: Check admission and placement

Confirm that Kueue reserved all three role `Workload`s together:

```bash
kubectl get workloads -n gpustack-bench
```

All four `Pod`s ran on `gpu-node-0`, with private IPC, private `/dev/shm`, and no *RDMA* interface;
**all cross-`Pod` NIXL encoder-cache and Mooncake KV transfers used TCP over `eth0`**.

Across multi-node clusters, rendered soft `Pod` affinity weights prefer co-locating roles that
exchange the largest payloads without blocking scheduling
([Role placement preferences](/gpustack-operator/main/docs/modules/model-deployment/encode-prefill-decode/index.md#role-placement-preferences)):

| Replica | Preferred peer | Weight |
|---|---|---:|
| *Encode* | *Prefill* | 50 |
| *Prefill* | *Decode* | 100 |
| *Prefill* | *Encode* | 50 |
| *Decode* | *Prefill* | 100 |

## Cleanup

Delete `epd-arm3-epd` and wait until all owned `Pod`s and `Service`s terminate:

```bash
kubectl delete modeldeployment epd-arm3-epd -n gpustack-bench
kubectl get pods,services -n gpustack-bench -l app.kubernetes.io/instance=epd-arm3-epd
```

## Limitations

- Role kinds (*Encode*, *Prefill*, *Decode*, *Server*) are frozen after creation.
- Measurements cover single-node TCP transfer; multi-node placement and GPU contention were not tested.
- Retiring an *Encode* replica drains for a fixed ~300 s window because encoders expose no in-flight
  request counter; transfers still active when that window expires fail at the client.

## Troubleshooting

| Symptom | Signal | Resolution |
|---|---|---|
| Apply rejected on role structure | Webhook refusal on `ModelDeployment` | *Encode* requires both *Prefill* and *Decode* (one role per kind, no *Server*, no *`spec.kvCache`*, and no role *`command`*; see [Admission constraints](/gpustack-operator/main/docs/modules/model-deployment/encode-prefill-decode/index.md#admission-constraints)). |
| Apply rejected on engine, router, or platform | Webhook refusal on `ModelDeployment` | All three roles must share one accelerator platform and match [Supported serving configurations](/gpustack-operator/main/docs/modules/model-deployment/encode-prefill-decode/index.md#supported-serving-configurations) (`vllm-router` is rejected on Ascend). |
| Encoder container exits on unknown flag | Container startup failure | Upgrade the runner image to meet the floor (`vLLM >= 0.30.0`, `vllm-ascend >= 0.30.0rc1`, `SGLang >= 0.5.10`; see [Engine Compatibility](/gpustack-operator/main/docs/modules/model-deployment/engine-versions/index.md)). |
| Requests bypass *Prefill* | Router routes directly to *Decode* | Check vLLM token-counting support and `--enable-scale-out` rules ([Router token counting](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode/index.md#router-token-counting)). |
| Ascend encoder fails storage setup | `ECExampleConnector` missing shared path | Mount a shared `ReadWriteMany` PVC on both *Encode* (read-write) and *Prefill* with identical *`claimName`*, *`mountPath`*, and *`subPath`*. |
| Apply rejected on port `5601` or `8997` | Side-channel or bootstrap port collision | `ECCPUConnector` defaults `VLLM_EC_SIDE_CHANNEL_PORT=5601` (override in *`roles[].env`*, including `0`, or move HTTP off `5601`), and SGLang *Prefill* in *EPD* owns `8997`. |
| *Encode* replica stays in `Draining` ~300 s | Bounded encoder drain timer | Expected behavior; wait for the ~300 s drain window to finish. |
| `RoleKindsReady=False` | Zero Ready replicas in one role kind | Read the failing kind from `ModelDeployment` status and inspect its `Pod` events and container logs. |

---

**See also** — [Encode, Prefill and Decode](/gpustack-operator/main/docs/modules/model-deployment/encode-prefill-decode/index.md)
(role contract and supported configurations) · [Encode, Prefill and Decode Benchmark](/gpustack-operator/main/docs/modules/model-deployment/encode-prefill-decode-benchmark/index.md)
(H100 latency and throughput) · [Prefill and Decode Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/prefill-decode/index.md) (two-role
disaggregation)

**Next** → [Elastic EP Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/elastic-ep/index.md) — expand an expert-parallel collective in place.
