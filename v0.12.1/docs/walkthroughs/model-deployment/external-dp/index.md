# External DP Walkthrough

Deploy four fixed vLLM *data parallelism (DP)* ranks behind `vllm-router`, verify direct completions
on every rank, and confirm that the router distributes traffic across all four endpoints. For SGLang
(`sglang-gateway`) contracts and results, see
[External DP](/gpustack-operator/v0.12.1/docs/modules/model-deployment/external-dp/index.md).

## Contents

- [Summary](#summary)
- [Step 1: Deploy rank group](#step-1-deploy-rank-group)
- [Verification](#verification)
- [Cleanup](#cleanup)
- [Limitations](#limitations)
- [Troubleshooting](#troubleshooting)

## Summary

*External load balancing (External LB)* exposes an independent HTTP server on every *DP* rank inside
one fixed *expert parallelism (EP)* collective (`replicas: 1`, `size: 4`, 2 GPUs per member = 8 GPUs,
*TP* = 2, *DP* = 4, *EP* world size = 8). Unlike [Elastic EP](/gpustack-operator/v0.12.1/docs/walkthroughs/model-deployment/elastic-ep/index.md), the collective size is
fixed at startup:

| Object | Scope | Creator | Purpose |
|---|---|---|---|
| `ModelArtifact` | namespace | user | Resolves `deepseek-v2-lite-chat` weights via node model delivery |
| `AcceleratorType` | cluster | operator | Whole-GPU H100 pool requested by all four members |
| `ModelDeployment` | namespace | user | Four-member *External LB* collective fronted by `vllm-router` |

At concurrency 64 on H100 SXM5, four-rank *External DP* cut *time to first token (TTFT)* p50 by
**2.58×** on `1024/256` (`1,863.100 ms` vs `4,798.411 ms`) and **1.23×** on `4096/128` (`3,094.017 ms`
vs `3,814.139 ms`), while wide *TP* = 8 retained **2.27×–6.99× higher output throughput**; see
[External DP Benchmark](/gpustack-operator/v0.12.1/docs/modules/model-deployment/external-dp-benchmark/index.md).

### Environment

| Item | Value |
|---|---|
| Cluster | Kubernetes v1.35.7; 1 CPU node and 1 GPU node (`gpu-node-0`) with 8 free H100 80 GB HBM3 SXM5 GPUs |
| Operator | GPUStack Operator v0.9.1 |
| Runner and router | `gpustack/runner:cuda13.0-vllm0.30.0`; `vllm-router` (`gpustack/llm-router:v0.3.1`, `--policy round_robin`) |
| Model | `deepseek-ai/DeepSeek-V2-Lite-Chat` (`ModelArtifact` `deepseek-v2-lite-chat`) |
| Pool | Captured on custom `InstanceType` `bench-gpu`; reusable manifest uses `gpustack--nvidia-h100-80gb-hbm3-linux-amd64` |
| GPU budget | 4 members × 2 GPUs = 8 GPUs |

Each member `Pod` exposed two GPUs with private IPC, private `/dev/shm`, and no *remote direct memory
access (RDMA)* interface. Intra-`Pod` *TP* = 2 used NVLink, while **cross-`Pod` *EP* communication
(`allgather_reducescatter`) ran over TCP on `eth0`**.

Enable [node model delivery](/gpustack-operator/v0.12.1/docs/modules/model-delivery/operations/index.md) and warm
`deepseek-v2-lite-chat` in namespace `gpustack-bench` via
[Model Prefetch](/gpustack-operator/v0.12.1/docs/walkthroughs/model-delivery/prefetch/index.md) before applying Step 1.

## Step 1: Deploy rank group

Save `external-dp.yaml`:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: external-dp-arm2-external-dp
  namespace: gpustack-bench
spec:
  model:
    name: deepseek-v2-lite-chat
    artifactRef:
      name: deepseek-v2-lite-chat
  engine:
    name: vLLM
    image: gpustack/runner:cuda13.0-vllm0.30.0
  router:
    name: vllm-router
    image: gpustack/llm-router:v0.3.1
    extraArgs:
      - --policy
      - round_robin
  roles:
    - name: server
      kind: Server
      replicas: 1
      size: 4
      acceleratorType: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
      image: gpustack/runner:cuda13.0-vllm0.30.0
      imagePullPolicy: IfNotPresent
      resources:
        accelerator: "2"
      extraArgs:
        - --tensor-parallel-size
        - "2"
        - --data-parallel-size
        - "4"
        - --data-parallel-rank
        - $(GPUSTACK_MEMBER_INDEX)
        - --data-parallel-address
        - $(GPUSTACK_REPLICA_LEADER_ADDRESS)
        - --enable-expert-parallel
        - --max-model-len
        - "8192"
        - --max-num-seqs
        - "16"
        - --gpu-memory-utilization
        - "0.90"
        - --all2all-backend
        - allgather_reducescatter
      env:
        - name: VLLM_PORT
          value: "19500"
        - name: OTEL_METRICS_EXPORTER
          value: none
        - name: OTEL_TRACES_EXPORTER
          value: none
```

The operator injects `GPUSTACK_MEMBER_INDEX` (`0`..`3`) and `GPUSTACK_REPLICA_LEADER_ADDRESS` into
every member `Pod`; passing `--data-parallel-rank` activates vLLM's *External LB* mode.

### Port allocation

On hosts using Linux ephemeral ports `32768–60999`, set **`1024 <= VLLM_PORT < 32768`** (such as
`19500`, clear of `8000`, `29500`, and `29550`) on every rank so vLLM's handshake ports never collide
with outbound ephemeral sockets (`19500` initialized cleanly at `19502` in this capture; see
[Port allocation](/gpustack-operator/v0.12.1/docs/modules/model-deployment/external-dp/index.md#port-allocation)).

Validate and apply `external-dp.yaml`:

```bash
kubectl apply --dry-run=server -f external-dp.yaml
kubectl apply -f external-dp.yaml
kubectl -n gpustack-bench get modeldeployment external-dp-arm2-external-dp
kubectl -n gpustack-bench get pods -l app.kubernetes.io/instance=external-dp-arm2-external-dp -o wide
```

Captured output (genericized node name, `ROLES=1R1S`, `PHASE=Ready`):

```text
NAME                                                   READY   STATUS    RESTARTS   AGE     NODE
external-dp-arm2-external-dp-router-6f7ccf57b8-vq4fr   1/1     Running   0          2m17s   gpu-node-0
external-dp-arm2-external-dp-server-r0-m0              1/1     Running   0          2m19s   gpu-node-0
external-dp-arm2-external-dp-server-r0-m1              1/1     Running   0          2m19s   gpu-node-0
external-dp-arm2-external-dp-server-r0-m2              1/1     Running   0          2m19s   gpu-node-0
external-dp-arm2-external-dp-server-r0-m3              1/1     Running   0          2m19s   gpu-node-0
```

## Verification

### Step 2: Verify rank endpoints

Verify that the operator detected `loadBalance: "External"` and confirmed all four HTTP endpoints on
replica group `r0`:

```bash
kubectl -n gpustack-bench get modeldeployment external-dp-arm2-external-dp -o json > external-dp-cr.json
jq '.status.roles[0] | {parallelism, endpoints}' external-dp-cr.json
```

Captured output:

```json
{
  "parallelism": {
    "declared": { "dataParallel": 4, "tensorParallel": 2 },
    "loadBalance": "External",
    "modes": { "expertParallel": true },
    "source": { "complete": true, "kind": "ExtraArgs" }
  },
  "endpoints": {
    "eligible": 1,
    "serving": { "state": "Confirmed", "value": 4 }
  }
}
```

*`endpoints.eligible`* (`1`) counts the admitted replica group `r0`, while *`endpoints.serving.value`*
(`4`) confirms all four per-rank HTTP servers under
`http://external-dp-arm2-external-dp-router.gpustack-bench.svc:8000`. If `serving.value` is `1`,
check `--data-parallel-rank` in *`extraArgs`*.

### Step 3: Send requests per rank

Verify each rank directly before testing router distribution. Forward each member (`RANK=0`..`3`) in
separate terminals:

```bash
RANK=0
RANK_POD=$(kubectl -n gpustack-bench get pods \
  -l "app.kubernetes.io/instance=external-dp-arm2-external-dp,app.kubernetes.io/component=server,modeldeployment.gpustack.ai/member-index=$RANK" \
  -o jsonpath='{.items[0].metadata.name}')
kubectl -n gpustack-bench port-forward "pod/$RANK_POD" "$((18000 + RANK)):8000"
```

Send non-streaming and streaming completions to every forwarded rank:

```bash
for port in 18000 18001 18002 18003; do
  curl --fail-with-body --max-time 60 "http://127.0.0.1:$port/v1/completions" \
    -H 'Content-Type: application/json' \
    -d '{"model":"deepseek-v2-lite-chat","prompt":"The sky is","max_tokens":8,"temperature":0}' \
    -o "rank-$port.json"
  jq '{text: .choices[0].text, finish_reason: .choices[0].finish_reason,
       completion_tokens: .usage.completion_tokens}' "rank-$port.json"
  curl --fail-with-body --no-buffer --max-time 60 "http://127.0.0.1:$port/v1/completions" \
    -H 'Content-Type: application/json' \
    -d '{"model":"deepseek-v2-lite-chat","prompt":"A GPU is","max_tokens":8,"temperature":0,"stream":true,"stream_options":{"include_usage":true}}' \
    -o "rank-$port-stream.txt"
done
```

Confirm positive `completion_tokens`, non-empty `text`, and a trailing `data: [DONE]` on all four
ports.

### Step 4: Check router distribution

Forward the router's HTTP (`:8000`) and Prometheus (`:9090`) ports:

```bash
ROUTER_POD=$(kubectl -n gpustack-bench get pods \
  -l app.kubernetes.io/instance=external-dp-arm2-external-dp -o json |
  jq -r '.items[] | select(any(.spec.containers[]; .name == "router")) | .metadata.name')
kubectl -n gpustack-bench port-forward "pod/$ROUTER_POD" 18080:8000 19090:9090
```

Verify four healthy workers in `/observer/endpoints` (note: port `:8000` blocks `/observer/endpoints`
on newer Envoy-fronted router images; use pod-internal `:9091` if `:8000` returns `403`), then send
eight requests and compare per-worker counters:

```bash
curl --fail-with-body http://127.0.0.1:19090/metrics > router-before.prom
for request in 1 2 3 4 5 6 7 8; do
  curl --fail-with-body --max-time 60 http://127.0.0.1:18080/v1/completions \
    -H 'Content-Type: application/json' \
    -d '{"model":"deepseek-v2-lite-chat","prompt":"The sky is","max_tokens":8,"temperature":0}' \
    -o "router-$request.json"
done
curl --fail-with-body http://127.0.0.1:19090/metrics > router-after.prom
grep -H '^vllm_router_processed_requests_total' router-before.prom router-after.prom
```

Confirm that `vllm_router_processed_requests_total` increments on **all four** `server-r0-m0`..`m3`
workers. Across **1,088/1,088 measured requests with zero errors** (plus 48 warm-ups and 1 calibration
request = 1,137 total), router counters matched engine access logs on every rank (`285` on `m0` and
`284` each on `m1`–`m3`); see
[Routing verification](/gpustack-operator/v0.12.1/docs/modules/model-deployment/external-dp-benchmark/index.md#routing-verification).

## Cleanup

If startup or routing stalls, inspect member logs and headless `Service` discovery before deleting:

```bash
kubectl -n gpustack-bench logs \
  -l app.kubernetes.io/instance=external-dp-arm2-external-dp,app.kubernetes.io/component=server \
  -c main --prefix --tail=200
```

Stop all `port-forward` processes, delete the deployment, and confirm all eight GPUs release:

```bash
kubectl -n gpustack-bench delete modeldeployment external-dp-arm2-external-dp --wait=true
kubectl -n gpustack-bench get pods,services,deployments,workloads \
  -l app.kubernetes.io/instance=external-dp-arm2-external-dp
kubectl get devices -o json
```

## Limitations

- Measurements cover single-node cross-`Pod` TCP; multi-node placement, *External DP* combined with
  *PD*, and hybrid load balancing were not tested.
- Fixed *External DP* groups do not resize in place; edits to *`size`* roll the entire four-`Pod`
  replica ([Admission constraints](/gpustack-operator/v0.12.1/docs/modules/model-deployment/external-dp/index.md#admission-constraints)).

## Troubleshooting

| Symptom | Signal | Resolution |
|---|---|---|
| Member `Pod`s stay `Pending` | Kueue `Workload` unreserved | Check `kubectl -n gpustack-bench get workloads,events` and `AcceleratorType` capacity; `size: 4` × `2` GPUs requires 8 free GPUs. |
| Ranks fail collective startup or hit `EADDRINUSE` | Engine startup log error on `m0`..`m3` | Pin `1024 <= VLLM_PORT < 32768` (such as `19500`) on all members and confirm `--data-parallel-size 4`, `--data-parallel-rank $(GPUSTACK_MEMBER_INDEX)`, and `--data-parallel-address $(GPUSTACK_REPLICA_LEADER_ADDRESS)`. |
| Direct rank requests succeed, router returns `503` | *`status.roles[].endpoints`* unconfirmed | Inspect `EndpointEligibility` on `ModelDeployment` and router logs for endpoint qualification failures. |
| Router sends traffic only to `m0` | Metrics increment on one worker only | Confirm *`status.roles[0].parallelism.loadBalance`* reads `External` (`--data-parallel-rank` must be present in *`extraArgs`*). |
| Leftover resources remain after delete | `Pod`s or `Service`s still listed | Inspect *`metadata.ownerReferences`* before deleting any residual objects manually. |

---

**See also** — [External DP](/gpustack-operator/v0.12.1/docs/modules/model-deployment/external-dp/index.md) (vLLM and SGLang contracts) ·
[External DP Benchmark](/gpustack-operator/v0.12.1/docs/modules/model-deployment/external-dp-benchmark/index.md) (H100 latency and
throughput vs wide *TP*) · [Model Deployment Routing](/gpustack-operator/v0.12.1/docs/modules/model-deployment/routing/index.md)
(router discovery and policies) · [Elastic EP Walkthrough](/gpustack-operator/v0.12.1/docs/walkthroughs/model-deployment/elastic-ep/index.md) (in-place collective growth)

**Next** → [Model Deployment Scaling Walkthrough](/gpustack-operator/v0.12.1/docs/walkthroughs/model-deployment/scaling/index.md) — observe and activate replica autoscaling.
