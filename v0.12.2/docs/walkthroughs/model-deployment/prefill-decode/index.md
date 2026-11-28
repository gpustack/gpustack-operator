# Prefill and Decode Walkthrough

Deploy a disaggregated serving topology with dedicated *Prefill* and *Decode* roles behind a managed
`llm-d-router`, inspect joint Kueue admission, and verify completion requests through the router's
single endpoint.

## Contents

- [Summary](#summary)
- [Deployment](#deployment)
- [Verification](#verification)
- [Cleanup](#cleanup)
- [Limitations](#limitations)
- [Troubleshooting](#troubleshooting)

## Summary

*Prefill/decode (PD)* disaggregation separates compute-bound prompt *prefill* from
memory-bandwidth-bound token *decode*. A managed router fronts both roles under one cluster HTTP
endpoint, coordinates Mooncake KV cache transfer between roles, and waits for Kueue to admit the full
role set atomically:

| Object | Scope | Creator | Purpose |
|---|---|---|---|
| `ModelArtifact` | namespace | user | Resolves and pins `Qwen/Qwen3-32B` weights |
| `ModelPrefetch` | namespace | user | Warms `qwen3-32b` onto the H100 node before serving |
| `AcceleratorType` | cluster | operator | H100 pool requested by both roles |
| `ModelDeployment` | namespace | user | Declares 1 *Prefill* and 2 *Decode* replicas behind `llm-d-router` |
| Router `Deployment` and `Service` | namespace | operator | Single HTTP entrypoint routing requests across roles |

On one 8-GPU H100 SXM5 node (6 GPUs per configuration), *PD* cut concurrency-16 long-prompt *inter-token
latency (ITL)* p99 by **25.6×–27.8×** (and cut mixed steady-stream max *ITL* by **23.3×** with no p99
gain) at the cost of higher *time to first token (TTFT)* and lower total throughput in 15 of 16
cells; see [Prefill and Decode Benchmark](/gpustack-operator/v0.12.2/docs/modules/model-deployment/prefill-decode-benchmark/index.md).

### Environment

| Item | Value |
|---|---|
| Cluster | Kubernetes v1.35.7; 1 CPU node and 1 GPU node (`gpu-node-0`) |
| GPUs | 8 × NVIDIA H100 80 GB HBM3 SXM5 (6 GPUs allocated to serving) |
| Pool | Captured on custom `InstanceType` `bench-gpu`; reusable manifest uses `gpustack--nvidia-h100-80gb-hbm3-linux-amd64` |
| Model | `Qwen/Qwen3-32B` via `ModelArtifact` `qwen3-32b` (node model delivery) |
| Engine and router | vLLM `gpustack/runner:cuda13.0-vllm0.30.0` with `llm-d-router` |
| KV transfer | Mooncake over TCP (`mooncake_protocol: "tcp"`) |

Each engine `Pod` saw only its two assigned GPUs with private IPC, private `/dev/shm`, and no *remote
direct memory access (RDMA)* interface. Intra-`Pod` *tensor parallelism (TP)* (`*TP* = 2`) used
NVLink, while **cross-`Pod` Mooncake KV transfer ran over TCP on `eth0`**.

## Deployment

### Step 1: Start disaggregated serving

Enable [node model delivery](/gpustack-operator/v0.12.2/docs/modules/model-delivery/operations/index.md) and run
[Model Prefetch](/gpustack-operator/v0.12.2/docs/walkthroughs/model-delivery/prefetch/index.md#step-1-create-artifact-and-prefetch) in namespace
`gpustack-bench` for `ModelArtifact` `qwen3-32b` (`Qwen/Qwen3-32B` at revision
`9216db5781bf21249d130ec9da846c4624c16137`). Wait until `ModelPrefetch` reports `Available=True`.

Save `pd-deployment.yaml` (1 *Prefill* × 2 GPUs + 2 *Decode* × 2 GPUs = 6 GPUs):

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: pd-arm2-pd
  namespace: gpustack-bench
spec:
  model:
    name: Qwen/Qwen3-32B
    artifactRef:
      name: qwen3-32b
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
      image: gpustack/runner:cuda13.0-vllm0.30.0
      imagePullPolicy: IfNotPresent
      resources:
        accelerator: "2"
      extraArgs:
        - --tensor-parallel-size
        - "2"
        - --max-model-len
        - "16384"
        - --max-num-seqs
        - "64"
        - --gpu-memory-utilization
        - "0.90"
        - --enable-scale-out
    - name: decode
      kind: Decode
      replicas: 2
      size: 1
      acceleratorType: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
      image: gpustack/runner:cuda13.0-vllm0.30.0
      imagePullPolicy: IfNotPresent
      resources:
        accelerator: "2"
      extraArgs:
        - --tensor-parallel-size
        - "2"
        - --max-model-len
        - "16384"
        - --max-num-seqs
        - "64"
        - --gpu-memory-utilization
        - "0.90"
        - --enable-scale-out
```

Apply `pd-deployment.yaml` and confirm the deployment reaches `Ready` (`1R1P2D`):

```bash
kubectl apply -f pd-deployment.yaml
kubectl get modeldeployment pd-arm2-pd -n gpustack-bench \
  -o custom-columns=NAME:.metadata.name,ENGINE:.spec.engine.name,ROLES:.status.roleSummary,PHASE:.status.phase
kubectl get pods -n gpustack-bench -l app.kubernetes.io/instance=pd-arm2-pd \
  -o custom-columns=NAME:.metadata.name,READY:.status.containerStatuses[*].ready,STATUS:.status.phase,NODE:.spec.nodeName
```

Captured output:

```text
NAME        ENGINE   ROLES    PHASE
pd-arm2-pd  vLLM     1R1P2D   Ready

NAME                                READY   STATUS    NODE
pd-arm2-pd-decode-2xglg             2/2     Running   gpu-node-0
pd-arm2-pd-decode-r2zt4             2/2     Running   gpu-node-0
pd-arm2-pd-prefill-xmjvs            1/1     Running   gpu-node-0
pd-arm2-pd-router-6b9fcd6b75-5ngnj  2/2     Running   gpu-node-0
```

Because this deployment has no *Encode* role, both roles set `--enable-scale-out` for router token
counting ([Router token counting](/gpustack-operator/v0.12.2/docs/modules/model-deployment/prefill-decode/index.md#router-token-counting)).
If `PHASE` stays `Starting`, inspect deployment conditions and match the reason in
[Troubleshooting](#troubleshooting).

### Step 2: Select the transfer backend

Without *`resources.interface`*, the operator defaults *`spec.kvTransfer.backend`* to `Mooncake` over
`tcp`. Inspect the rendered transfer flags across *Prefill* and *Decode*:

```bash
kubectl -n gpustack-bench get pods -l app.kubernetes.io/instance=pd-arm2-pd -o json |
  jq '.items[] | select(.metadata.labels["app.kubernetes.io/component"] != "router") |
      {pod: .metadata.name, role: .metadata.labels["app.kubernetes.io/component"],
       command: .spec.containers[0].command,
       proxyArgs: (.spec.initContainers // [] | map(select(.name=="routing-proxy"))[0].args)}'
```

Captured output:

```text
Prefill main:
  --kv-transfer-config '{"kv_connector":"MooncakeConnector","kv_role":"kv_producer","kv_connector_extra_config":{"mooncake_protocol":"tcp"}}'
  --kv-events-config enables the publisher on ports 5557 and 5558
  --port 8000

Decode main:
  --kv-transfer-config '{"kv_connector":"MooncakeConnector","kv_role":"kv_consumer","kv_connector_extra_config":{"mooncake_protocol":"tcp"}}'
  --port 8200

Decode routing-proxy init container:
  --port=8000 --model-server-port=8200 --kv-connector=mooncake
  --mooncake-bootstrap-port=8998 --secure-proxy=false
```

To switch KV transfer to *RDMA*, request *`resources.interface`* on both roles ([RDMA Walkthrough](/gpustack-operator/v0.12.2/docs/walkthroughs/rdma/network-endpoints/index.md#step-3-request-interfaces-per-role)) and verify InfiniBand transport in engine logs ([InfiniBand RDMA comparison](/gpustack-operator/v0.12.2/docs/modules/model-deployment/prefill-decode-benchmark/index.md#infiniband-rdma-comparison)).
If you also attach a shared KV store via *`spec.kvCache.poolRef`*, declare it at creation because *`spec.kvCache`* is immutable ([Deployment identity fields](/gpustack-operator/v0.12.2/docs/modules/model-deployment/configuration/index.md#deployment-identity-fields)).

## Verification

### Step 3: Verify the router

Confirm *`status.endpoint`* is populated and send a chat completion naming `Qwen/Qwen3-32B`:

```bash
ENDPOINT=$(kubectl -n gpustack-bench get modeldeployment pd-arm2-pd -o jsonpath='{.status.endpoint}')
echo "$ENDPOINT"
kubectl -n gpustack-bench run test-client --rm -i --restart=Never \
  --image=curlimages/curl -- \
  curl -fsS "$ENDPOINT/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "Qwen/Qwen3-32B",
    "messages": [{"role": "user", "content": "Explain prefill and decode disaggregation."}],
    "max_tokens": 16
  }'
```

Captured output:

```text
http://pd-arm2-pd-router.gpustack-bench.svc:8000
```

Captured output (burst probe: 16 requests at concurrency 8, 8,192 input tokens, 64 output tokens):

```json
{"requests":16,"concurrency":8,"input_tokens":8192,"output_tokens":64,"ok":16,"empty":0,"bad":0}
```

If *`status.endpoint`* is empty or requests return `503`, check `RouterReady` and `EndpointEligibility`
conditions on `ModelDeployment/pd-arm2-pd`.

### Step 4: Inspect joint admission

Verify that Kueue admitted one `Workload` per engine replica (`1` *Prefill* + `2` *Decode* = `3`
`Workload`s):

```bash
kubectl -n gpustack-bench get workloads
```

Captured output:

```text
NAME                              QUEUE                             RESERVED IN   ADMITTED
gpustack-fnv64-ab5121bc91ece976   gpustack-fnv64-889bba71d87d29e2   bench-gpu     True
gpustack-fnv64-ab5122bc91eceb29   gpustack-fnv64-889bba71d87d29e2   bench-gpu     True
gpustack-fnv64-d6b4e8ae20539d37   gpustack-fnv64-889bba71d87d29e2   bench-gpu     True
```

You can edit *`replicas`* independently on *Prefill* or *Decode* without rolling surviving replicas;
see [Rollout behavior](/gpustack-operator/v0.12.2/docs/modules/model-deployment/configuration/index.md#rollout-behavior).

## Cleanup

Delete `pd-arm2-pd` and confirm owned `Pod`s, `Service`s, and GPU allocations clear:

```bash
kubectl -n gpustack-bench delete modeldeployment pd-arm2-pd
kubectl -n gpustack-bench get pods,services -l app.kubernetes.io/instance=pd-arm2-pd
```

Captured output (post-deletion `Devices` summary on `gpu-node-0`):

```text
node: gpu-node-0
accelerator: NVIDIA H100 80GB HBM3
count: 8
remaining per accelerator: 1600000
```

Deleting `ModelDeployment` leaves prefetched weights in `NodeModelStore` untouched.

## Limitations

- Measurements cover single-node TCP transfer; cross-node placement and *RDMA*, EFA, CANN, or ROCm
  transports require separate validation.
- See [Prefill and Decode Benchmark](/gpustack-operator/v0.12.2/docs/modules/model-deployment/prefill-decode-benchmark/index.md) for
  full concurrency sweeps and
  [Router configuration](/gpustack-operator/v0.12.2/docs/modules/model-deployment/prefill-decode/index.md#router-configuration) for
  router lifecycle contracts.

## Troubleshooting

| Symptom | Signal | Resolution |
|---|---|---|
| `ModelDeployment` stuck in `Starting`, missing `Workload`s | `QuotaReserved=False/PodGroupIncomplete` | Read the condition message on `ModelDeployment` and inspect unschedulable or rejected role `Pod`s. |
| `Workload`s stop requesting quota | `QuotaReserved=False/Parked` | Joint placement timed out and deactivated the `Workload`s; free pool capacity and reactivate the `Workload`s or recreate the `ModelDeployment` (re-applying identical YAML does not unpark). |
| Partial preemption holds remaining GPUs | `QuotaReserved=False/PreemptedInPart` | Higher-priority work preempted part of the deployment; free pool quota so preempted replicas re-admit, or delete the `ModelDeployment`. |
| *`status.endpoint`* stays empty with Router `Pod` present | `RouterReady=False/NoReadyReplicas` | Inspect Router `Pod` events and logs for image pull or readiness probe failures. |
| No Router `Deployment` or `Pod` exists | `RouterReady=False/RenderFailed` | Read `RouterReady` on `ModelDeployment` and fix invalid fields under *`spec.router`*. |
| KV transfer fails between *Prefill* and *Decode* | Engine transfer errors in both role logs | Verify matching Mooncake build versions in the runner image, valid *`resources.interface`* counts, and pod-to-pod network reachability. |
| `KVEventsPublishing=False/PublisherDisabled` | Expected when a publishing role's `--kv-events-config` is missing on rendered `Pod`s | Informational condition reporting configuration rather than runtime failure; routed SGLang reports `True/Publishing` under `sgl-router` and `True/NotApplicable` under `llm-d-router` or `sglang-gateway` ([Operator-owned keys](/gpustack-operator/v0.12.2/docs/modules/model-deployment/configuration/index.md#operator-owned-keys)). |
| `RoleKindsReady=False` | Zero Ready replicas in *Prefill* or *Decode* | Inspect `Pod` logs and events for the unready role named in the condition message. |

---

**See also** — [Prefill and Decode](/gpustack-operator/v0.12.2/docs/modules/model-deployment/prefill-decode/index.md) (pairing and
transfer contracts) · [Prefill and Decode Benchmark](/gpustack-operator/v0.12.2/docs/modules/model-deployment/prefill-decode-benchmark/index.md)
(H100 SXM5 latency and throughput) · [Model Deployment Routing](/gpustack-operator/v0.12.2/docs/modules/model-deployment/routing/index.md)
(router policies and qualification)

**Next** → [Encode, Prefill and Decode Walkthrough](/gpustack-operator/v0.12.2/docs/walkthroughs/model-deployment/encode-prefill-decode/index.md) — disaggregate multimodal encode, prefill, and decode roles.
