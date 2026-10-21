# Prefill and Decode Walkthrough

Deploy a disaggregated large language model (LLM) topology with dedicated *prefill* and *decode* roles
fronted by a Router. The operator coordinates joint admission across both roles and routes requests
through one endpoint.

## Contents

- [Summary](#summary)
- [Device and cluster](#device-and-cluster)
- [Step 1: Start disaggregated serving](#step-1-start-disaggregated-serving)
- [Step 2: Pin the transfer protocol](#step-2-pin-the-transfer-protocol)
- [Step 3: Verify the router](#step-3-verify-the-router)
- [Step 4: Inspect joint admission](#step-4-inspect-joint-admission)
- [Step 5: Release the application](#step-5-release-the-application)
- [Limitations](#limitations)
- [Troubleshooting](#troubleshooting)

## Summary

*Prefill/Decode (PD)* disaggregation separates compute-bound prompt *prefill* from memory-bandwidth-bound
token decoding. A compute-heavy *Prefill* role processes initial prompt context. A memory-bandwidth-bound
*Decode* role generates subsequent tokens.

A managed Router fronts both roles and exposes one cluster-internal HTTP endpoint. It parses prompt
requests, forwards *prefill* work, and coordinates key-value (KV) cache transfer to decoders. Kueue
schedules both roles as a joint unit, preventing half-admitted workloads.

The H100 comparison allocated six GPUs to each configuration.
It supports *PD* when smoother generation under long-prompt concurrency outweighs first-token delay.
At concurrency 16, long-prompt *inter-token latency (ITL)* p99 improved **25.6×–27.8×**, while first-token latency rose substantially.
The mixed steady stream had **23.3× lower maximum ITL**, with no p99 improvement.
See [Prefill and Decode Benchmark](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode-benchmark/index.md) for throughput, tails and the decode-heavy outlier.

## Device and cluster

The deployment and probe below were captured on this cluster:

| Item | Captured setup |
|---|---|
| Cluster | Kubernetes 1.35.7; one CPU node and one GPU node (`gpu-node-0`) |
| GPUs | 8 × NVIDIA H100 80 GB HBM3 SXM5; six GPUs assigned to serving |
| `InstanceType` | `bench-gpu`, configured for the H100 accelerator group |
| Model | Qwen/Qwen3-32B from `ModelArtifact` `qwen3-32b`, served as `/models` through node model delivery |
| Engine and router | vLLM `gpustack/runner:cuda13.0-vllm0.30.0` with `llm-d-router` |
| KV transfer | Mooncake over TCP; the default protocol renders as `tcp` |

Each engine `Pod` exposed only its two assigned GPUs, with private IPC and `/dev/shm`.
No *remote direct memory access (RDMA)* interface was attached.
NVLink carried *TP* = 2 traffic inside each `Pod`.
**Cross-`Pod` Mooncake KV transfer used TCP sockets over `eth0`**, despite the host's NVLink fabric.

## Step 1: Start disaggregated serving

Enable [node model delivery](/gpustack-operator/main/docs/modules/model-delivery/operations/index.md) and follow the
[model prefetch procedure](/gpustack-operator/main/docs/walkthroughs/model-delivery/prefetch/index.md) with namespace `gpustack-bench`.
Create `ModelArtifact` `qwen3-32b` from `Qwen/Qwen3-32B` at revision
`9216db5781bf21249d130ec9da846c4624c16137`, then prefetch it onto the H100 node.
Use that artifact and namespace throughout the procedure instead of its DeepSeek example.

The captured run used the custom `bench-gpu` `InstanceType` for the same H100 accelerator group.
This reusable manifest uses the discovered H100 `InstanceType` name. Replace the two *`instanceType`*
values with `bench-gpu` to reproduce the captured configuration exactly.

The deployment requests one *Prefill* replica and two *Decode* replicas, each with two GPUs. All
three engine `Pod`s and the Router ran on `gpu-node-0`.

Save the following manifest as `pd-deployment.yaml`:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: pd-arm2-pd
  namespace: gpustack-bench
spec:
  model:
    name: /models
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
      instanceType: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
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
      instanceType: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
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

Apply the manifest and inspect the captured status:

```bash
kubectl apply -f pd-deployment.yaml
kubectl get modeldeployment pd-arm2-pd -n gpustack-bench \
  -o custom-columns=NAME:.metadata.name,ENGINE:.spec.engine.name,ROLES:.status.roleSummary,PHASE:.status.phase
```

```text
NAME        ENGINE   ROLES    PHASE
pd-arm2-pd  vLLM     1R1P2D   Ready
```

The recorded `Pod`s were ready and co-located on `gpu-node-0`:

```text
NAME                                READY   STATUS    NODE
pd-arm2-pd-decode-2xglg             2/2     Running   gpu-node-0
pd-arm2-pd-decode-r2zt4             2/2     Running   gpu-node-0
pd-arm2-pd-prefill-xmjvs            1/1     Running   gpu-node-0
pd-arm2-pd-router-6b9fcd6b75-5ngnj   2/2     Running   gpu-node-0
```

Each engine process uses *tensor parallelism (TP)* across its two requested accelerators. The
`--tensor-parallel-size 2` flag sets *TP* = 2. The deployment has no *Encode* role, so both roles set
`--enable-scale-out`; see [Counting engine tokens](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode/index.md#counting-engine-tokens).

## Step 2: Pin the transfer protocol

Disaggregated serving requires prompt KV tensors to flow from *prefill* workers to *decode* workers.
The pair in Step 1 hands blocks over directly. An unset *`spec.kvTransfer.protocol`* renders `tcp`.
Pin the protocol explicitly. Add the lines marked `+` to `pd-deployment.yaml`:

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: ModelDeployment
 metadata:
   name: pd-arm2-pd
   namespace: gpustack-bench
 spec:
+  kvTransfer:
+    protocol: TCP
   router:
     name: llm-d-router
```

Apply the update:

```bash
kubectl apply -f pd-deployment.yaml
```

The recorded manifest leaves the protocol unset, which renders `tcp` by default. The protocol is
deployment-wide because it describes one link between the two roles. It accepts `Auto`, `TCP`, `RDMA`,
`EFA`, `CANN` and `ROCM`; `Auto` renders `tcp` too. An edit restarts every role because the value renders
into both ends' arguments. `RDMA` and `EFA` also need a matching device request on each role. See the
[RDMA walkthrough](/gpustack-operator/main/docs/walkthroughs/rdma/network-endpoints/index.md#step-3-request-interfaces-per-role).

The captured engine commands rendered these transfer settings:

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

The [InfiniBand RDMA follow-up](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode-benchmark/index.md#infiniband-rdma-comparison)
set *`spec.kvTransfer.protocol`* to `RDMA` and requested *`resources.interface`* = `"2"` on both roles.
Runtime logs verified the InfiniBand KV path; the configuration change alone is not transport proof.

A shared store through *`spec.kvCache.poolRef`* is part of the deployment identity. Declare it in the first
apply. Adding it later is refused because a different cache describes a different deployment. See
[deployment identity fields](/gpustack-operator/main/docs/modules/model-deployment/deployment/index.md#deployment-identity-fields).

## Step 3: Verify the router

The captured Router endpoint is:

```text
http://pd-arm2-pd-router.gpustack-bench.svc:8000
```

Send a completion request through the Router from inside the cluster:

```bash
kubectl -n gpustack-bench run test-client --rm -i --restart=Never \
  --image=curlimages/curl -- \
  curl -s http://pd-arm2-pd-router.gpustack-bench.svc:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "/models",
    "messages": [{"role": "user", "content": "Explain prefill and decode disaggregation."}],
    "max_tokens": 16
  }'
```

The recorded burst probe sent 16 requests at concurrency 8, with 8,192 input tokens and 64 output tokens.
**All 16 succeeded, with zero empty bodies or errors:**

```json
{"requests":16,"concurrency":8,"input_tokens":8192,"output_tokens":64,"ok":16,"empty":0,"bad":0}
```

The Router delegates prompt processing and coordinates the roles. Mooncake transfers KV tensors
between the engines, and the Router returns generated tokens.
*`status.endpoint`* remains empty until the Router `Pod` passes its readiness check.

## Step 4: Inspect joint admission

The operator manages Kueue workloads across all declared roles simultaneously. The captured workloads were
all admitted into the captured custom `bench-gpu` queue. The reusable discovered `InstanceType`
uses its own generated queue:

```bash
kubectl -n gpustack-bench get workloads
```

```text
NAME                              QUEUE                             RESERVED IN   ADMITTED
gpustack-fnv64-ab5121bc91ece976   gpustack-fnv64-889bba71d87d29e2   bench-gpu     True
gpustack-fnv64-ab5122bc91eceb29   gpustack-fnv64-889bba71d87d29e2   bench-gpu     True
gpustack-fnv64-d6b4e8ae20539d37   gpustack-fnv64-889bba71d87d29e2   bench-gpu     True
```

You can adjust *`replicas`* independently on either role to balance prompt versus token throughput.
See [Rollout behavior](/gpustack-operator/main/docs/modules/model-deployment/deployment/index.md#rollout-behavior) for editable
role fields and replica replacement semantics.

## Step 5: Release the application

After testing disaggregated serving, delete the deployment to release GPUs and Router services:

```bash
kubectl -n gpustack-bench delete modeldeployment pd-arm2-pd
kubectl -n gpustack-bench get pods,services -l app.kubernetes.io/instance=pd-arm2-pd
```

Verify that all application GPU claims are released. The post-deletion *PD* `Devices` snapshot
recorded the following availability:

```text
node: gpu-node-0
accelerator: NVIDIA H100 80GB HBM3
count: 8
remaining per accelerator: 1600000
```

This snapshot establishes released accelerator claims. Model-cache contents and cluster infrastructure remain outside its scope.

## Limitations

- The recording uses one GPU node with eight H100 accelerators and co-locates all six serving GPUs and
  the Router on that node. It does not measure placement across multiple GPU nodes.
- The captured transfer path is TCP through Mooncake. RDMA, EFA, CANN and ROCM paths require their own
  device requests and runtime validation.
- The short completion probe covers readiness and inference. The separate
  [benchmark](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode-benchmark/index.md) measures all 16 fixed-shape cells
  and mixed traffic; its results remain limited to the captured H100 configuration.
- Joint gang admission across all roles (`PodGroup`), role-set immutability after creation and per-replica
  rollout replacement are covered in [Prefill and decode](/gpustack-operator/main/docs/modules/model-deployment/deployment/index.md#prefill-and-decode)
  and [Rollout behavior](/gpustack-operator/main/docs/modules/model-deployment/deployment/index.md#rollout-behavior).
- Disaggregated serving requires a ready Router (*`status.endpoint`* is populated only while the Router
  `Pod` is ready); see [Router configuration](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode/index.md#router-configuration).

## Troubleshooting

| Symptom | Condition / Reason | Check & Mitigation |
|---|---|---|
| `ModelDeployment` stuck in `Starting`, Kueue composes no `Workload` | `QuotaReserved=False/PodGroupIncomplete` | Fewer replicas exist than a role declares. Read the message for the counts, then check the missing or unschedulable `Pod`s |
| No `Workload` asks for quota any more | `QuotaReserved=False/Parked` | The groups could not all be placed, so their `Workload`s were deactivated. An identical re-apply does not clear this. Free the capacity and reactivate the `Workload`s, or recreate the deployment |
| Part of the deployment was taken by another workload | `QuotaReserved=False/PreemptedInPart` | A higher-priority workload reclaimed some replicas. The replicas that kept their quota hold accelerators until the reclaimed ones are admitted again or the deployment is deleted. Check what took the quota |
| *`status.endpoint`* is empty | `RouterReady=False/NoReadyReplicas` | The Router has no ready replica. It may still pull its image or fail readiness. Inspect the Router `Pod` |
| Router is not created | `RouterReady=False/RenderFailed` | The Router render was refused as a spec problem, so no Router `Pod` exists. Read the condition message and check *`spec.router`* |
| Disaggregated roles fail KV transfer | Engine logs on both roles | The protocol is deployment-wide, so the two ends cannot name different values. Check that the image carries the matching Mooncake build, that the device request fits the protocol and that the nodes can reach each other |
| Router sees no cache events | `KVEventsPublishing=False/PublisherDisabled` | A role that produces cache blocks renders no publisher. A routed SGLang deployment always reports this because only vLLM renders one. The condition reports configuration, not traffic. See [KV events](/gpustack-operator/main/docs/modules/model-deployment/deployment/index.md#operator-owned-keys) |
| Missing role readiness | `RoleKindsReady=False` | One of the role kinds (*Prefill* or *Decode*) has zero ready replicas; check failed engine `Pod`s |

---

**See also** — [Prefill and Decode](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode/index.md) (configuration and transfer) ·
[Prefill and Decode Benchmark](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode-benchmark/index.md) (H100 SXM5 results) ·
[Routing](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md) (Router engine combinations) ·
[Elastic EP Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/elastic-ep/index.md) (collective expansion) ·
[External DP Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/external-dp/index.md) (fixed per-rank serving) ·
[Encode, Prefill and Decode Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/encode-prefill-decode/index.md) (multimodal disaggregation)

**Next** → [Multi-Host Serving Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/multi-host/index.md) — scale models across multiple GPU nodes.
