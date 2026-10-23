# External DP Walkthrough

Start four fixed *DP* ranks behind `vllm-router`, send requests to each rank directly, and confirm the Router reaches every endpoint.

## Contents

- [Summary](#summary)
- [Environment](#environment)
- [Step 1: Start the fixed group](#step-1-start-the-fixed-group)
- [Step 2: Read the running resources](#step-2-read-the-running-resources)
- [Step 3: Send requests per rank](#step-3-send-requests-per-rank)
- [Step 4: Check Router coverage](#step-4-check-router-coverage)
- [Step 5: Inspect and release](#step-5-inspect-and-release)
- [Limitations](#limitations)
- [Troubleshooting](#troubleshooting)

## Summary

*External load balancing (External LB)* gives every *DP* rank its own HTTP serving endpoint.
The managed Router must discover and distribute requests across all four endpoints.
With *EP* enabled, the ranks participate in one fixed collective with *EP* world size 8.
The group has no online resize contract.

The configuration uses one *Server* role with *`replicas: 1`* and *`size: 4`*.
Each member requests two GPUs, but that scheduling request does not declare engine parallelism.
Explicit *TP*, *DP*, rank and leader-address arguments define the engine group.

Each rank uses two GPUs with *TP* = 2, for eight H100 GPUs in total.
The captured H100 SXM5 run used this fixed group in namespace `gpustack-bench`.

| Object | Scope | Creator | Purpose |
|---|---|---|---|
| `ModelArtifact` | namespace | user, via the prefetch procedure | the DeepSeek V2 Lite weights the deployment serves |
| `ModelDeployment` | namespace | user | the fixed four-rank *DP* group behind the managed Router |
| `InstanceType` | cluster | discovered | the whole-GPU H100 type each member requests |

At concurrency 64, *time to first token (TTFT)* p50 was **2.58× lower** for the four-rank group on `1024/256`:
**1,863.100 ms** against wide *TP* = 8 at `4,798.411 ms`.
On `4096/128`, it had **1.23× lower TTFT p50**: **3,094.017 ms** against `3,814.139 ms`.
These benefits favor queueing-sensitive, fixed-group workloads; wide *TP* retained **2.27×–6.99× higher output throughput**.
Use [External DP Benchmark](../../modules/model-deployment/external-dp-benchmark.md) to compare tails as well as medians.

## Environment

| Item | Captured setup |
|---|---|
| Cluster | One CPU node and one GPU node with eight free NVIDIA H100 80 GB HBM3 SXM5 GPUs |
| Kubernetes | v1.35.7 |
| Operator | GPUStack Operator v0.9.1 with managed *External LB* endpoint discovery |
| Runner | `gpustack/runner:cuda13.0-vllm0.30.0` |
| Router | `vllm-router`, image `gpustack/llm-router:v0.3.1`, policy `round_robin` |
| Model | `deepseek-ai/DeepSeek-V2-Lite-Chat`, artifact `deepseek-v2-lite-chat`, served as `/models` |
| Reusable `InstanceType` | `gpustack--nvidia-h100-80gb-hbm3-linux-amd64` |
| GPU budget | 4 members × 2 GPUs = 8 GPUs |

Each `Pod` exposed only its assigned GPUs, with private IPC and `/dev/shm` and no RDMA interface.
*TP* = 2 used NVLink inside each `Pod`.
**Cross-`Pod` *EP* communication used TCP sockets over `eth0`**, including `allgather_reducescatter`.
The benchmark's wide *TP* = 8 configuration kept all eight GPUs inside one `Pod` and used intra-`Pod` NVLink.

Enable [node model delivery](../../modules/model-delivery/operations.md).
Follow [model prefetch](../model-delivery/prefetch.md) with namespace `gpustack-bench` and the H100 node's hostname label.
Keep the DeepSeek artifact name and revision from that procedure.
Verify cached weights, free GPU capacity and communication between all ranks before starting the group.

## Step 1: Start the fixed group

Save the manifest as `external-dp.yaml`.
It uses the discovered H100 `InstanceType`; the benchmark preparation's custom type was named `bench-gpu`.
Use an existing type that selects the same whole-GPU accelerator group.
Replace *`instanceType`* with `bench-gpu` to reproduce the captured scheduling configuration.

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: external-dp-arm2-external-dp
  namespace: gpustack-bench
spec:
  model:
    name: /models
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
      instanceType: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
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

The operator supplies `GPUSTACK_MEMBER_INDEX` and `GPUSTACK_REPLICA_LEADER_ADDRESS` to each member.
Kubernetes expands the references in the container arguments.
All four ranks receive the same leader address and distinct rank values from 0 to 3.
An explicit `--data-parallel-rank` selects the engine's *External LB* mode.

The collective uses multiprocessing for *DP* and local *TP* execution; this example declares no Ray profile.
The all-to-all and memory arguments configure the model execution, not Router discovery.
Keep matching *DP* widths, distinct ranks and a reachable leader address on every member.

### Port allocation

For the recorded Linux ephemeral range `32768–60999`, set **`1024 <= VLLM_PORT < 32768`** (such as **`19500`**) on every rank, keeping candidates clear of `8000`, `29500`, `29550` and other listeners. In the recorded capture, `VLLM_PORT=19500` avoided an ephemeral-source collision and initialized at `19502`. See [Port allocation](../../modules/model-deployment/external-dp.md#port-allocation) for vLLM's five-port probing, `stateless_init_dp_group` retry behavior, and the asymmetric `EADDRINUSE` failure mode.

```bash
kubectl apply --dry-run=server -f external-dp.yaml
kubectl apply -f external-dp.yaml
kubectl -n gpustack-bench get modeldeployment external-dp-arm2-external-dp
kubectl -n gpustack-bench get pods \
  -l app.kubernetes.io/instance=external-dp-arm2-external-dp -o wide
```

The captured phase was `Ready`, with role summary `1R1S`.
The following listing omits IP and scheduling-gate columns and replaces the node identity with `gpu-node-0`:

```text
NAME                                                   READY   STATUS    RESTARTS   AGE     NODE
external-dp-arm2-external-dp-router-6f7ccf57b8-vq4fr     1/1     Running   0          2m17s   gpu-node-0
external-dp-arm2-external-dp-server-r0-m0                1/1     Running   0          2m19s   gpu-node-0
external-dp-arm2-external-dp-server-r0-m1                1/1     Running   0          2m19s   gpu-node-0
external-dp-arm2-external-dp-server-r0-m2                1/1     Running   0          2m19s   gpu-node-0
external-dp-arm2-external-dp-server-r0-m3                1/1     Running   0          2m19s   gpu-node-0
```

## Step 2: Read the running resources

Save the live CR and inspect the engine's declared configuration and endpoint observations:

```bash
kubectl -n gpustack-bench get modeldeployment external-dp-arm2-external-dp \
  -o json > external-dp-cr.json
jq '{name: .metadata.name, phase: .status.phase,
     size: .spec.roles[0].size, replicas: .spec.roles[0].replicas,
     roles: [.status.roles[] | {name, ready, parallelism, endpoints}]}' external-dp-cr.json
kubectl -n gpustack-bench get pods \
  -l app.kubernetes.io/instance=external-dp-arm2-external-dp -o json > external-dp-pods.json
jq '.items[] | {name: .metadata.name,
    member: .metadata.labels["modeldeployment.gpustack.ai/member-index"],
    containers: [.status.containerStatuses[]? | {name, ready, restartCount}]}' external-dp-pods.json
```

Require *`parallelism.loadBalance`* to read `External`.
The role has one replica group, while the serving endpoint observation should confirm four HTTP endpoints.
A `Ready` phase and declared arguments alone do not establish inference on each rank.
The captured role reported this configuration and endpoint observation:

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

The endpoint was `http://external-dp-arm2-external-dp-router.gpustack-bench.svc:8000`.
*`eligible`* counts the single replica group; *`serving.value`* counts its four HTTP endpoints.

## Step 3: Send requests per rank

The recorded benchmark sent streaming requests through the Router, which reached all four ranks.
The direct checks below are additional verification commands; no direct nonstreaming responses were captured.

Open a separate terminal for each rank. Set `RANK` to 0, 1, 2 and 3 respectively:

```bash
RANK=0
RANK_POD=$(kubectl -n gpustack-bench get pods \
  -l "app.kubernetes.io/instance=external-dp-arm2-external-dp,app.kubernetes.io/component=server,modeldeployment.gpustack.ai/member-index=$RANK" \
  -o jsonpath='{.items[0].metadata.name}')
kubectl -n gpustack-bench port-forward "pod/$RANK_POD" "$((18000 + RANK)):8000"
```

In another terminal, send ordinary and streaming requests to all four local ports:

```bash
for port in 18000 18001 18002 18003; do
  curl --fail-with-body --max-time 60 "http://127.0.0.1:$port/v1/completions" \
    -H 'Content-Type: application/json' \
    -d '{"model":"/models","prompt":"The sky is","max_tokens":8,"temperature":0}' \
    -o "rank-$port.json"
  jq '{text: .choices[0].text, finish_reason: .choices[0].finish_reason,
       completion_tokens: .usage.completion_tokens}' "rank-$port.json"
  curl --fail-with-body --no-buffer --max-time 60 "http://127.0.0.1:$port/v1/completions" \
    -H 'Content-Type: application/json' \
    -d '{"model":"/models","prompt":"A GPU is","max_tokens":8,"temperature":0,"stream":true,"stream_options":{"include_usage":true}}' \
    -o "rank-$port-stream.txt"
done
```

Require successful HTTP responses, nonempty text, positive completion tokens and a valid finish reason on every rank.
Each stream must end with `data: [DONE]` and include usage.
Retain the responses per rank. These checks establish functional inference, not sustained throughput.

## Step 4: Check Router coverage

Discover the Router `Pod` and forward its API and metrics ports:

```bash
ROUTER_POD=$(kubectl -n gpustack-bench get pods \
  -l app.kubernetes.io/instance=external-dp-arm2-external-dp -o json |
  jq -r '.items[] | select(any(.spec.containers[]; .name == "router")) | .metadata.name')
kubectl -n gpustack-bench port-forward "pod/$ROUTER_POD" 18080:8000 19090:9090
```

In another terminal, save endpoint discovery and per-worker request counters before sending traffic:

```bash
curl --fail-with-body http://127.0.0.1:18080/observer/endpoints > router-endpoints.json
jq '.workers[] | {url, selection}' router-endpoints.json
curl --fail-with-body http://127.0.0.1:19090/metrics > router-before.prom
```

Send at least eight ordinary requests through the Router:

```bash
for request in 1 2 3 4 5 6 7 8; do
  curl --fail-with-body --max-time 60 http://127.0.0.1:18080/v1/completions \
    -H 'Content-Type: application/json' \
    -d '{"model":"/models","prompt":"The sky is","max_tokens":8,"temperature":0}' \
    -o "router-$request.json"
done
curl --fail-with-body http://127.0.0.1:19090/metrics > router-after.prom
grep -H '^vllm_router_processed_requests_total' router-before.prom router-after.prom
```

Require four healthy rank endpoints in the registry and increased request counters for every rank.
**A successful Router response alone does not prove all-rank coverage.**
If only rank 0 is registered, inspect endpoint qualification before interpreting the policy's distribution.
For *Internal LB* and [Prefill and Decode](../../modules/model-deployment/prefill-decode.md), leader-only discovery remains intentional.

The final registry confirmed four registered, healthy endpoints in selection, with closed circuits.
The captured per-worker Router counters matched the engine's completion access lines on every rank
(**285** on `server-r0-m0` and **284** on each of `server-r0-m1` through `server-r0-m3`, totaling
**1,137 requests** across **1,088/1,088 measured requests**, 48 warm-ups and one calibration request,
with zero errors).

See [External DP Benchmark](../../modules/model-deployment/external-dp-benchmark.md#routing-verification)
for the per-rank breakdown, latency and throughput tables, and the wide-*TP* comparison.

## Step 5: Inspect and release

Read each member's logs if initialization or requests stall:

```bash
kubectl -n gpustack-bench logs \
  -l app.kubernetes.io/instance=external-dp-arm2-external-dp,app.kubernetes.io/component=server \
  -c main --prefix --tail=200
kubectl -n gpustack-bench get modeldeployment external-dp-arm2-external-dp -o yaml
kubectl -n gpustack-bench get services,endpointslices
```

Check matching widths, distinct ranks, a shared leader address and peer reachability.
The replica's headless `Service` supplies peer discovery; limiting it to the HTTP leader can prevent group startup.
Keep engine inference evidence separate from Router discovery and coverage evidence.

Stop all port-forwards, then delete the application:

```bash
kubectl -n gpustack-bench delete modeldeployment external-dp-arm2-external-dp --wait=true
kubectl -n gpustack-bench get pods,services,deployments,workloads \
  -l app.kubernetes.io/instance=external-dp-arm2-external-dp
kubectl get devices -o json
```

Verify deletion of owned resources and release of all eight application GPU claims.
The runner log confirms deletion of the `ModelDeployment`, but no final owned-resource or released-device snapshot was captured.
The model cache and cluster remain separate from application cleanup.

## Limitations

The H100 recording establishes a ready fixed group, four confirmed endpoints and streaming traffic on every rank.
It does not include direct nonstreaming responses per rank or a final GPU-release snapshot.
The benchmark measured one model, two prompt/output targets and one run per configuration.

This fixed group does not resize its collective online; use [Elastic EP](elastic-ep.md) for width
growth. Group sizing and replacement semantics are covered in
[Admission constraints](../../modules/model-deployment/external-dp.md#admission-constraints) and
[Rollout behavior](../../modules/model-deployment/deployment.md#rollout-behavior).

The example does not establish *External DP* with *Prefill/Decode*, hybrid load balancing, multiple HTTP ports per `Pod`,
or placement across GPU nodes. Those configurations need separate compatibility and runtime checks.

## Troubleshooting

| Symptom | Signal | Check & Mitigation |
|---|---|---|
| Members stay Pending | Status: member `Pod` phase `Pending` | Read Kueue quota, events and `InstanceType` capacity. Four members with two GPUs each need eight free GPUs. |
| Ranks cannot initialize | Log: failing rank's engine startup log | Verify rank values 0–3, *DP* width 4, the shared leader address and the engine's peer ports. |
| Direct requests pass but Router requests fail | Status: *`status.roles[].endpoints`* | Read *`status.roles[].endpoints`*, Router discovery and eligibility labels. |
| Router registers only rank 0 | Status: `/observer/endpoints` lists one worker | Confirm the build supports managed *External LB* discovery and the role reports `External`. |
| Owned resources remain after deletion | Owned `Pod`s, `Service`s or `Deployment`s still listed | Inspect owner references before removing leftovers. Do not infer complete cleanup from CR deletion alone. |

---

**See also** — [External DP](../../modules/model-deployment/external-dp.md) (fixed-group contract) ·
[Routing](../../modules/model-deployment/routing.md) (backend selection) ·
[Elastic EP Walkthrough](elastic-ep.md) (captured collective expansion)

**Next** → [Model Deployment Status](../../modules/model-deployment/status.md)
