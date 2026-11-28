# Elastic EP Walkthrough

Start a four-GPU vLLM Ray *expert parallelism (EP)* collective on an eight-GPU H100 SXM5 node, expand
it live to eight GPUs without replacing surviving `Pod`s, and verify uninterrupted client streaming.

## Contents

- [Summary](#summary)
- [Step 1: Start the elastic deployment](#step-1-start-the-elastic-deployment)
- [Step 2: Verify baseline inference](#step-2-verify-baseline-inference)
- [Step 3: Expand and verify inference](#step-3-expand-and-verify-inference)
- [Cleanup](#cleanup)
- [Limitations](#limitations)
- [Troubleshooting](#troubleshooting)

## Summary

*Elastic EP* resizes a running *mixture of experts (MoE)* collective in place using Ray and *internal
load balancing (Internal LB)*. Only the master (`pod-ordinal=0`) serves HTTP requests, while a
CPU-only Ray head coordinates membership without claiming GPUs.

Keep *`replicas: 1`*, *`size: 1`*, and `accelerator: "2"` (*TP* = 2), and scale *data parallelism (DP)*
width `2 → 4` (*EP* world size `4 → 8`) via `runtimeControl.parameters["elastic.dataParallelWidth"]`:

| Object | Scope | Creator | Purpose |
|---|---|---|---|
| `ModelArtifact` | namespace | user | Resolves `deepseek-v2-lite-chat` weights via node model delivery |
| `AcceleratorType` | cluster | operator | Whole-GPU H100 pool requested by each two-GPU Ray member |
| `ModelDeployment` | namespace | user | Elastic vLLM collective resized live through `elastic.dataParallelWidth` |

During the live *DP* width 2 → 4 expansion, **439/439 client requests completed with zero errors**. In
the steady-state benchmark at concurrency 64, width 4 cut *time to first token (TTFT)* p50 by
**2.50×** (`2,598.867 ms` vs `6,509.231 ms`) while using twice the GPUs, lowering total output
throughput at every concurrency, and raising concurrency-1 *inter-token latency (ITL)* p50 from
`8.566 ms` to `15.675 ms`; see [Elastic EP Benchmark](/gpustack-operator/v0.12.2/docs/modules/model-deployment/elastic-ep-benchmark/index.md).

### Environment

| Item | Value |
|---|---|
| Cluster | Kubernetes v1.35.7; 1 CPU node and 1 GPU node (`gpu-node-0`) |
| Hardware | 8 × NVIDIA H100 80 GB HBM3 SXM5 (900 GB/s physical NVLink) |
| Operator and runner | GPUStack Operator v0.9.1; `gpustack/runner:cuda13.0-vllm0.30.0` |
| Model | `deepseek-ai/DeepSeek-V2-Lite-Chat` (`ModelArtifact` `deepseek-v2-lite-chat`) |
| Pool | Captured on custom `InstanceType` `bench-gpu`; reusable manifest uses `gpustack--nvidia-h100-80gb-hbm3-linux-amd64` |
| Allocation | Initial: 2 GPU members × 2 GPUs (4 GPUs) + 1 CPU Ray head; expanded: 4 GPU members × 2 GPUs (8 GPUs) |

Each GPU `Pod` ran with private IPC, private `/dev/shm`, *`hostIPC`* disabled, and no *remote direct
memory access (RDMA)* interface. Intra-`Pod` *TP* = 2 used NVLink, while **cross-`Pod` *EP*
collectives ran over TCP sockets on `eth0`**.

Before starting, enable [node model delivery](/gpustack-operator/v0.12.2/docs/modules/model-delivery/operations/index.md) and warm
`deepseek-v2-lite-chat` in namespace `gpustack-bench` via
[Model Prefetch](/gpustack-operator/v0.12.2/docs/walkthroughs/model-delivery/prefetch/index.md). Keep all eight H100 GPUs free so Step 3 can admit two
additional two-GPU members.

## Step 1: Start the elastic deployment

Save `elastic-ep.yaml` (keep `--elastic-ep-max-dp-size 4` so vLLM reserves capacity up to width 4):

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: b2-elastic-ep
  namespace: gpustack-bench
spec:
  model:
    name: deepseek-v2-lite-chat
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
      acceleratorType: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
      image: gpustack/runner:cuda13.0-vllm0.30.0
      imagePullPolicy: IfNotPresent
      resources:
        accelerator: "2"
      runtimeControl:
        parameters:
          elastic.dataParallelWidth: "2"
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

Apply `elastic-ep.yaml` and wait for the Ray head and two initial GPU members to run:

```bash
kubectl apply -f elastic-ep.yaml
kubectl -n gpustack-bench get modeldeployment b2-elastic-ep
kubectl -n gpustack-bench get pods -l app.kubernetes.io/instance=b2-elastic-ep -o wide
```

`Service/b2-elastic-ep` selects only the serving master (`pod-ordinal=0`), and *`status.roles[0]`*
reports `desired: 1, ready: 1` for the single elastic collective.

## Step 2: Verify baseline inference

Snapshot baseline deployment and `Pod` state before resizing:

```bash
kubectl -n gpustack-bench get modeldeployment b2-elastic-ep -o json > baseline-cr.json
kubectl -n gpustack-bench get pods -l app.kubernetes.io/instance=b2-elastic-ep -o json > baseline-pods.json
jq '{name: .metadata.name, width: .spec.roles[0].runtimeControl.parameters["elastic.dataParallelWidth"],
     phase: .status.phase, desired: .status.roles[0].desired,
     ready: .status.roles[0].ready}' baseline-cr.json
```

Captured output:

```json
{
  "name": "b2-elastic-ep",
  "width": "2",
  "phase": "Ready",
  "desired": 1,
  "ready": 1
}
```

`ReplicasUpToDate` can remain `Unknown/RolloutNotObserved` while `phase` reads `Ready`, so verify
actual token generation in addition to `phase`. Forward `Service/b2-elastic-ep` in a second terminal
and run a streaming completion check:

```bash
kubectl -n gpustack-bench port-forward service/b2-elastic-ep 8000:8000
```

```bash
curl --fail-with-body --no-buffer --max-time 60 \
  http://127.0.0.1:8000/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-v2-lite-chat","messages":[{"role":"user","content":"Explain GPU scheduling briefly."}],"max_tokens":64,"temperature":0,"stream":true,"stream_options":{"include_usage":true}}' \
  -o baseline-stream.txt
grep -q 'data: \[DONE\]' baseline-stream.txt && echo "baseline stream OK"
```

Confirm non-empty completion tokens and `data: [DONE]` before expanding.

## Step 3: Expand and verify inference

Patch `elastic.dataParallelWidth` from `"2"` to `"4"` with a JSON `test` guard:

```bash
kubectl -n gpustack-bench patch modeldeployment b2-elastic-ep --type=json \
  -p '[{"op":"test","path":"/spec/roles/0/runtimeControl/parameters/elastic.dataParallelWidth","value":"2"},{"op":"replace","path":"/spec/roles/0/runtimeControl/parameters/elastic.dataParallelWidth","value":"4"}]'
```

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: ModelDeployment
 metadata:
   name: b2-elastic-ep
   namespace: gpustack-bench
 spec:
   roles:
     - name: server
       runtimeControl:
         parameters:
-          elastic.dataParallelWidth: "2"
+          elastic.dataParallelWidth: "4"
```

Confirm native runtime width `4` for the current deployment `uid` and `generation` (four running `Pod`s
alone do not prove collective re-rendezvous):

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

`observation.json` (diagnostic state rather than a public CR schema) reports `observedGeneration: 2`,
all five width stages at `4`, and reason `native ranks and forwards agree`. If this check fails,
inspect `ElasticResize` on `ModelDeployment` in [Troubleshooting](#troubleshooting) before issuing
another width edit.

Snapshot the expanded deployment and verify that the Ray head and both baseline GPU `Pod`s kept their
original `Pod` UIDs with zero container restarts:

```bash
kubectl -n gpustack-bench get modeldeployment b2-elastic-ep -o json > scaled-cr.json
kubectl -n gpustack-bench get pods -l app.kubernetes.io/instance=b2-elastic-ep -o json > scaled-pods.json
jq -n --slurpfile base baseline-pods.json --slurpfile scaled scaled-pods.json '
  ($base[0].items | map({key: .metadata.name, value: .metadata.uid}) | from_entries) as $b |
  [$scaled[0].items[] | select($b[.metadata.name] != null) | {
    name: .metadata.name,
    sameUID: (.metadata.uid == $b[.metadata.name]),
    restarts: ([.status.containerStatuses[].restartCount] | add)
  }]'
```

Captured output (`scaled-cr.json` projection):

```json
{
  "name": "b2-elastic-ep",
  "width": "4",
  "phase": "Ready",
  "desired": 1,
  "ready": 1
}
```

| Captured `Pod` | Role | Ready | UID across resize | Restarts |
|---|---|---|---|---:|
| `b2-elastic-ep-b2-elastic-ep-elastic-head-95n5k` | CPU Ray head | 1/1 | Retained | 0 |
| `b2-elastic-ep-server-tsc8r` | Serving master (`pod-ordinal=0`) | 1/1 | Retained | 0 |
| `b2-elastic-ep-server-vzdtc` | Baseline GPU member | 1/1 | Retained | 0 |
| `b2-elastic-ep-server-6nvwx` | Added GPU member | 1/1 | New | 0 |
| `b2-elastic-ep-server-z22ml` | Added GPU member | 1/1 | New | 0 |

Traffic and probe behavior during expansion:

- Continuous concurrency-8 load (`1024` input / `256` output tokens) completed **439/439 client
  requests in 264.23 s with zero errors** and zero client-facing HTTP `503` responses (1,320 total
  completion lines in master logs including warm-up and calibration).
- Patch submission at `08:16:11Z` converged `RoleKindsReady=True` and `EndpointEligibility=True` at
  `08:19:20Z` (**~189 s** for member startup and Ray collective join).
- During collective re-rendezvous, the master returned **10 `/health` HTTP `503` probes**, and a
  16–21 s watch log briefly observed `Starting 0S` at `08:19:09Z` between `Ready 1S` samples at
  `08:18:50Z` and `08:19:30Z`. Re-run the Step 2 `curl` stream after `observation.json` confirms
  effective width `4`.

## Cleanup

Stop `port-forward`, delete `b2-elastic-ep`, and confirm all eight H100 GPUs return to
`remaining: 1600000`:

```bash
kubectl -n gpustack-bench delete modeldeployment b2-elastic-ep --wait=true
kubectl -n gpustack-bench get pods,services,deployments,workloads -l app.kubernetes.io/instance=b2-elastic-ep
kubectl get devices -o json | jq '[.items[].status.groups[].accelerators[].remaining]'
```

Deleting `ModelDeployment` preserves prefetched weights in `NodeModelStore`
([Model Prefetch](/gpustack-operator/v0.12.2/docs/modules/model-delivery/prefetch/index.md)).

## Limitations

- Measurements cover single-node cross-`Pod` TCP (*DP* width 2 → 4, *TP* = 2).
- Only the master serves HTTP (`Internal LB`), and width shrink (`4 → 2`) is refused in this release.
- See [Elastic EP Benchmark](/gpustack-operator/v0.12.2/docs/modules/model-deployment/elastic-ep-benchmark/index.md) for steady-state
  latency and throughput trade-offs.

## Troubleshooting

| Symptom | Signal | Resolution |
|---|---|---|
| Added GPU members stay `Pending` | Member `Pod` phase `Pending` | Inspect `kubectl -n gpustack-bench get workloads,events` and `Devices` capacity; width `4` at *TP* = 2 requires 8 free GPUs. |
| Four GPU members run, `effective` width still `< 4` | `observation.json` shows `effective.Known=false` or `Value=2` | Wait for Ray initialization (~3 min on H100) and inspect master container logs (`kubectl -n gpustack-bench logs <master-pod> -c main --tail=200`). |
| `ElasticResize=False/ScaleRefused` | Engine rejected resize request | Read the condition message on `ModelDeployment` and check [Resize hold conditions](/gpustack-operator/v0.12.2/docs/modules/model-deployment/elastic-ep/index.md#resize-hold-conditions). |
| `ElasticResize=Unknown/WorldUnprovable` | In-flight resize awaiting engine proof | Keep observing without patching `elastic.dataParallelWidth` again; never resend an unanswered resize manually. |
| Streaming requests stall | No tokens returned on `:8000` | Check master and Ray worker logs, GPU visibility, `/dev/shm` usage, and host RAM; capture logs before deleting the deployment. |

---

**See also** — [Elastic EP](/gpustack-operator/v0.12.2/docs/modules/model-deployment/elastic-ep/index.md) (profile contract and resize
holds) · [Elastic EP Benchmark](/gpustack-operator/v0.12.2/docs/modules/model-deployment/elastic-ep-benchmark/index.md) (H100 width
2 vs 4 measurements) · [External DP Walkthrough](/gpustack-operator/v0.12.2/docs/walkthroughs/model-deployment/external-dp/index.md) (fixed four-rank *External LB*
collective)

**Next** → [External DP Walkthrough](/gpustack-operator/v0.12.2/docs/walkthroughs/model-deployment/external-dp/index.md) — deploy four fixed data-parallel ranks behind a router.
