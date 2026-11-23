# Elastic EP

An *elastic expert parallelism (Elastic EP)* deployment keeps one vLLM serving instance and expands
its GPU collective in place inside one Ray cluster without restarting surviving `Pod`s. The operator
creates a CPU-only Ray head `Pod` (`--num-gpus=0`) and `Service` alongside whole-GPU master and
worker `Pod`s whose readiness checks verify *TP*, allocated devices, and `Pod` identity.

![Elastic EP collective expansion](/gpustack-operator/v0.12.1/docs/assets/diagrams/model-deployment-elastic-ep.svg)

<!--
flowchart TD
    HEAD["CPU Ray head and Service"] -> MASTER["Member 0: HTTP master"]
    HEAD -> WORKER1["Member 1: worker"]
    HEAD -> WORKER2["Member 2: joining worker"]
    HEAD -> WORKER3["Member 3: joining worker"]
    MASTER -> COLLECTIVE["Live DP and EP collective"]
    WORKER1 -> COLLECTIVE
    WORKER2 -> COLLECTIVE
    WORKER3 -> COLLECTIVE
-->

## Contents

- [Configuration](#configuration)
- [Resize procedure](#resize-procedure)
- [Troubleshooting](#troubleshooting)
- [Limitations](#limitations)

## Configuration

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: qwen3-ep
  namespace: gpustack
spec:
  model:
    name: Qwen/Qwen3-30B-A3B
    artifactRef:
      name: qwen3-30b-a3b-artifact
  router:
    name: llm-d-router
  engine:
    name: vLLM
    image: gpustack/runner:cuda13.0-vllm0.30.0
  roles:
    - name: server
      kind: Server
      replicas: 1
      size: 1
      acceleratorType: elastic-gpu
      resources:
        accelerator: "1"  # defaults to declared TP size (1 here) when omitted
      runtimeControl:
        parameters:
          elastic.dataParallelWidth: "2"
      extraArgs:
        - --elastic-ep-max-dp-size
        - "4"  # required on vLLM 0.30.0+ to allow expansion up to width 4
      ports:
        - name: http
          port: 8000
          protocol: TCP
```

- **Engine image**: Use a vLLM image with a full Ray installation matching your *mixture of experts
  (MoE)* model and single-node *TP* width (see
  [Elastic EP Walkthrough](/gpustack-operator/v0.12.1/docs/walkthroughs/model-deployment/elastic-ep/index.md) and
  [Elastic EP Benchmark](/gpustack-operator/v0.12.1/docs/modules/model-deployment/elastic-ep-benchmark/index.md#live-resize)).
- **CPU Ray head**: Created without `AcceleratorType`, `LocalQueue`, or container resource requests
  (namespace `LimitRange` rules still apply).
- **Weights**: Delivered through standard `ModelArtifact` and node cache paths
  ([Model Delivery](/gpustack-operator/v0.12.1/docs/modules/model-delivery/index.md)).
- **Explicit *TP* and resources**: For *TP* = 2, set `--tensor-parallel-size 2` and
  `resources.accelerator: "2"` (or omit `accelerator` to default to *TP*). Explicit values must
  equal *TP*; CPU and RAM never auto-scale.
- **Fabric**: Optional *`resources.interface`* enables RDMA or EFA; see
  [InfiniBand RDMA comparison](/gpustack-operator/v0.12.1/docs/modules/model-deployment/elastic-ep-benchmark/index.md#infiniband-rdma-comparison).

### Elastic EP settings

Set *`roles[].runtimeControl.parameters["elastic.dataParallelWidth"]`* to a string integer from
`"2"` to `"64"` to select *Elastic EP*:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: deepseek-ep
  namespace: gpustack
spec:
  model:
    name: deepseek-ai/DeepSeek-R1
  engine:
    name: vLLM  # required; SGLang is not supported
    image: gpustack/runner:cuda13.0-vllm0.30.0
  roles:
    - name: server
      acceleratorType: elastic-gpu
      runtimeControl:
        parameters:
          elastic.dataParallelWidth: "4"  # target DP engines ("2".."64"), including master
```

`elastic.dataParallelWidth` counts *data parallelism (DP)* engines (one GPU `Pod` each, including
the HTTP master). Total GPU demand is `width × TP`, where *tensor parallelism (TP)* comes from
`--tensor-parallel-size` in *`extraArgs`* (default `1`).

Admission freezes and enforces these profile rules once created:

- **Single managed role**: Exactly one *Server* role with `replicas: 1`, `size: 1`, and no custom
  *`command`*.
- **Whole-GPU members**: Each member requests *TP* whole unsliced GPUs inside one `Pod` and one Ray
  node; pipeline and *prefill* context parallel sizes stay `1`.
- **Frozen fields**: Profile presence, effective *TP*, *`size`*, *`acceleratorType`* and
  *`resources`* cannot change after creation. Only `elastic.dataParallelWidth` changes (scale-up or
  unchanged only; width decreases are refused, tracked in
  [#741](https://github.com/gpustack/gpustack-operator/issues/741)).
- **Master HTTP ingress**: Only member 0 serves HTTP; workers join Ray and hold *expert parallelism
  (EP)* collective state.

### Shared memory

The CPU head, master and each GPU worker mount a separate `/dev/shm` `EmptyDir`. Changing
*`roles[].shmSize`* affects new or replacement `Pod`s only; see
[Shared memory](/gpustack-operator/v0.12.1/docs/modules/model-deployment/configuration/index.md#shared-memory).

### Maximum width

On vLLM `0.30.0`+, declare `--elastic-ep-max-dp-size` in *`extraArgs`* at creation to permit future
expansion (immutable after creation; omitting it blocks growth past the initial width, and older
vLLM releases reject the flag).

## Resize procedure

Patch only `elastic.dataParallelWidth` and wait for convergence before submitting another change:

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: ModelDeployment
 metadata:
   name: qwen3-ep
   namespace: gpustack
 spec:
   roles:
     - name: server
       runtimeControl:
         parameters:
-          elastic.dataParallelWidth: "2"
+          elastic.dataParallelWidth: "4"
```

```bash
kubectl patch modeldeployment qwen3-ep -n gpustack --type=json \
  -p '[{"op":"replace","path":"/spec/roles/0/runtimeControl/parameters/elastic.dataParallelWidth","value":"4"}]'
```

The operator retains the master's command, UID and bootstrap width while adding GPU workers around
it. If quota, GPU allocation or Ray membership is incomplete, the resize holds while the running
master keeps serving.

### Resize status

The operator tracks five layers independently in a deployment-owned diagnostic `ConfigMap`
(`observation.json`, each layer carrying `Known`, `Value` and `Reason`):

| Observation | Meaning |
|---|---|
| `desired` | Width requested in spec |
| `admitted` | Members admitted by Kubernetes and Kueue |
| `allocated` | Members holding verified whole-GPU allocations |
| `Ray` | Members joined to the Ray cluster |
| `effective` | Width proved by runtime identities and rank forwards |

Layers legitimately diverge mid-resize; unreadable layers report `Known: false` rather than copying
a neighbor. During native resize, the engine returns HTTP `503`: liveness accepts `200` and `503`
to avoid restarts, while readiness withdraws to shield traffic.

### Resize hold conditions

The operator records intent before sending a resize request to the engine:

- **Ambiguous send (timeout / controller restart)**: Never resent automatically, preventing double
  application. Recovery waits for live observation proof.
- **Definite refusal (`408`, `5xx`, busy `503`, rejected command)**: Retried with exponential
  backoff (`15 s` initial, doubling up to `4 min`, max `5` attempts) only when the engine reports
  it is not mid-resize. After five refusals, `ElasticResize` turns `False` (`ScaleRefused`) with
  the engine's last refusal.
- **Common holds**: Pending quota/allocation/Ray join, unreadable actor state, a newer spec
  generation arriving over an unresolved operation, or an ambiguous send.
- **Automatic recovery**: A narrower spec retires a stale widening hold, and full member loss
  re-derives state once the spec advances and live native width is proved. Partial survival holds
  until live proof clears.

## Troubleshooting

| Signal | Owner | Action |
|---|---|---|
| `ElasticResize=False` (`ScaleRefused`) | `ModelDeployment` / engine | Read the condition message for the engine's last refusal, inspect master `Pod` logs and fix the engine/flag constraint. |
| `ElasticResize=Unknown` (`WorldUnprovable`) | `ModelDeployment` / observation | Inspect the deployment-owned `observation.json` `ConfigMap` and verify master/Ray reachability so live width can be proved. |
| `admitted` lags `desired` | Kueue / `ClusterQueue` | Inspect `QuotaReserved` ([Readiness and status](/gpustack-operator/v0.12.1/docs/modules/model-deployment/configuration/index.md#readiness-and-status)) and pool queue capacity. |
| `Ray` lags `allocated` | Ray worker `Pod`s | Check joining worker `Pod` logs (`kubectl logs`) for Ray head connection errors. |
| `effective` lags `Ray` | vLLM master `Pod` | Do not resend after an ambiguous timeout; inspect master logs and `/gpustack/v1/ready` to confirm reported width. |

### Ray logs

The Ray head and GPU workers stream Ray logs to stdout/stderr at `info` level (`kubectl logs`). Set
these variables in *`roles[].env`* before creation (edits apply only to new `Pod`s, not existing
elastic `Pod`s):

| Variable | Default | Scope |
|---|---|---|
| `RAY_LOG_TO_STDERR` | `1` | Ray system logs (`0` writes Ray log files instead) |
| `RAY_LOGGER_LEVEL` | `info` | Python components (`debug`, `info`, `warning`, `error`, `critical`) |
| `RAY_BACKEND_LOG_LEVEL` | `info` | C++ components (`trace`, `debug`, `info`, `warning`, `error`, `fatal`) |
| `RAY_DEDUP_LOGS` | Ray default | Set `0` to disable driver log deduplication |

Set `VLLM_LOGGING_LEVEL` separately for vLLM logs on GPU members.

## Limitations

- Supports managed vLLM with a full Ray installation only; SGLang uses ordinary replica scaling.
- Width range is `2`–`64`; scale-down is rejected at admission
  ([#741](https://github.com/gpustack/gpustack-operator/issues/741)), and only a pre-existing
  in-flight narrowing can complete.
- A resize affects only the patched `ModelDeployment`; sibling deployments keep serving their own
  endpoints.
- Profile presence, *TP*, *`size`*, *`acceleratorType`* and *`resources`* are frozen after creation.
- One serving instance per deployment (`replicas: 1`, `size: 1`); only member 0 serves HTTP.
- Adds GPU `Pod`s on existing cluster capacity; it does not provision cloud nodes.

---

**See also** — [Model Deployment Configuration](/gpustack-operator/v0.12.1/docs/modules/model-deployment/configuration/index.md) (role contract and status conditions) ·
[Prefill and Decode](/gpustack-operator/v0.12.1/docs/modules/model-deployment/prefill-decode/index.md) (disaggregated role pairing) ·
[Model Deployment Routing](/gpustack-operator/v0.12.1/docs/modules/model-deployment/routing/index.md) (replica selection and observations) ·
[Online Runtime Control](/gpustack-operator/v0.12.1/docs/modules/model-deployment/runtime-control/index.md) (live parameter replay, role pause and drain) ·
[Elastic EP Benchmark](/gpustack-operator/v0.12.1/docs/modules/model-deployment/elastic-ep-benchmark/index.md) (measured width change under load)

**Next** → [Model Deployment Configuration](/gpustack-operator/v0.12.1/docs/modules/model-deployment/configuration/index.md)
