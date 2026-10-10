# External DP Walkthrough

Run one fixed vLLM data-parallel (DP) group with an HTTP endpoint on each rank.
This walkthrough records two ranks using the official GPUStack runner.
Direct requests and managed Router requests passed on both ranks after the selector fix.
Follow the steps to verify both engine inference and Router coverage.

## Contents

- [Deployment shape](#deployment-shape)
- [Prerequisites](#prerequisites)
- [Step 1: start the fixed group](#step-1-start-the-fixed-group)
- [Step 2: read the running resources](#step-2-read-the-running-resources)
- [Step 3: send requests to each rank](#step-3-send-requests-to-each-rank)
- [Step 4: check Router coverage](#step-4-check-router-coverage)
- [Understand the scaling limits](#understand-the-scaling-limits)
- [Step 5: inspect and release the application](#step-5-inspect-and-release-the-application)
- [Validation record](#validation-record)
- [Troubleshooting](#troubleshooting)

## Deployment shape

This example uses one Server role with `replicas: 1` and `size: 2`.
Each Pod has two GPUs and runs one DP rank with tensor parallelism (TP)=2.
With expert parallelism (EP) enabled, the collective has four GPU workers: TP2, DP2, EP world4.
The deployment consumes four whole GPUs.

External LB means each DP rank accepts its own requests.
A load balancer must distribute requests across those HTTP endpoints.
The `round_robin` policy chooses among discovered endpoints; it cannot repair a missing endpoint.

This example uses multiprocessing (`mp`) for both DP and local TP execution.
The runner contains Ray, but this fixed group does not start Ray.
See [vLLM's DP deployment guide](https://docs.vllm.ai/en/v0.29.0/serving/data_parallel_deployment/)
for the engine's Internal and External LB distinction.

## Prerequisites

Complete [cluster and GPU preparation](/gpustack-operator/main/docs/walkthroughs/model-deployment/index.md#prepare-the-cluster) and
[model prefetch](/gpustack-operator/main/docs/walkthroughs/model-delivery/prefetch/index.md).
Use the shared namespace, cached artifact, model revision, and GPU InstanceType.
The GPU node needs four free cards and the verified model cache.

Each Pod needs a separate Pod IP. The ranks must reach each other over the Pod network.
The DP RPC port defaults to 29550; collective communication also uses other ports.
Setting an engine RPC port does not create a Kubernetes Service or a firewall rule.

## Step 1: start the fixed group

Save this as `external-dp.yaml`. Replace `elastic-cpu-gpu` with your GPU InstanceType if needed.
The model delivery steps already created `deepseek-v2-lite-chat` in `gpustack-elastic`.

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: external-fixed
  namespace: gpustack-elastic
spec:
  model:
    name: /models
    artifactRef:
      name: deepseek-v2-lite-chat
  engine:
    name: vLLM
    version: "0.29.0"
  roles:
    - name: server
      kind: Server
      replicas: 1
      size: 2
      instanceType: elastic-cpu-gpu
      image: gpustack/runner:cuda13.0-vllm0.29.0
      imagePullPolicy: IfNotPresent
      resources:
        accelerator: "2"
      extraArgs:
        - --tensor-parallel-size
        - "2"
        - --data-parallel-size
        - "2"
        - --data-parallel-rank
        - $(GPUSTACK_MEMBER_INDEX)
        - --data-parallel-address
        - $(GPUSTACK_REPLICA_LEADER_ADDRESS)
        - --enable-expert-parallel
        - --max-model-len
        - "4096"
        - --max-num-seqs
        - "32"
        - --enforce-eager
        - --all2all-backend
        - allgather_reducescatter
  router:
    name: vllm-router
    image: gpustack/llm-router:v0.2.0
    extraArgs:
      - --policy
      - round_robin
```

The operator supplies the member index and replica leader address as environment variables.
Kubernetes expands their references in the container arguments.
Rank 0 and rank 1 therefore share the leader address and receive distinct rank values.
The ordinary Server role still requires explicit TP, DP, rank, and address arguments.
It does not infer those arguments from `size` or GPU requests.

The length, sequence, eager-execution, and all-to-all arguments preserve the recorded model settings.
They do not select External LB.
The measured run also mounted a diagnostic worker extension to read native groups and GPU identities.
That test instrumentation is omitted from this serving manifest.

```bash
kubectl apply --dry-run=server -f external-dp.yaml
kubectl apply -f external-dp.yaml
kubectl -n gpustack-elastic get modeldeployment external-fixed
kubectl -n gpustack-elastic get pods \
  -l app.kubernetes.io/instance=external-fixed
```

### Arguments you can omit

The minimal run omitted all six arguments below and still completed ordinary and streaming requests on both ranks.
These defaults apply to this vLLM version and deployment shape.

| Omitted argument | Why it is unnecessary here |
|---|---|
| `--data-parallel-external-lb` | An explicit `--data-parallel-rank` selects External LB. |
| `--data-parallel-size-local 1` | External LB runs one local DP rank. |
| `--api-server-count 1` | External LB defaults to one API server per rank. |
| `--data-parallel-backend mp` | `mp` is the default DP backend. |
| `--distributed-executor-backend mp` | The local TP group fits the Pod's two GPUs and runs outside Ray. |
| `--data-parallel-rpc-port 13345` | Both ranks can use the default RPC port, 29550. |

The operator generates the ordinary HTTP serving port, model mount, Pod wiring, and Services.
This example serves each rank on the same fixed port, 8000.

A single API server process and a single HTTP port are different constraints.
Internal LB may use several API server processes behind one port.
Keep the engine default for Internal LB; do not force `--api-server-count 1` on every configuration.
Managed roles reject MultiPort mode because its additional listeners are outside this endpoint contract.

`--data-parallel-backend` selects execution placement; it does not select the LB mode.
`--data-parallel-start-rank` alone is not an External LB declaration.

## Step 2: read the running resources

Retain the CR output beside the request results:

```bash
kubectl -n gpustack-elastic get modeldeployment external-fixed -o json |
  jq '{name: .metadata.name, phase: .status.phase,
       size: .spec.roles[0].size, replicas: .spec.roles[0].replicas,
       accelerator: .spec.roles[0].resources.accelerator}'
```

Recorded values from the captured CR:

```json
{
  "name": "external-fixed",
  "phase": "Ready",
  "size": 2,
  "replicas": 1,
  "accelerator": "2"
}
```

The captured status also reported one ready replica group and two serving HTTP endpoints:

```bash
kubectl -n gpustack-elastic get modeldeployment external-fixed -o json |
  jq '.status.roles[] | {name, ready, loadBalance: .parallelism.loadBalance, endpoints}'
```

```json
{
  "name": "server",
  "ready": 1,
  "loadBalance": "External",
  "endpoints": {
    "eligible": 1,
    "serving": {"state": "Confirmed", "value": 2}
  }
}
```

Here `eligible` counts replica groups; the serving observation counts HTTP endpoints.
Keep the full CR with `kubectl -n gpustack-elastic get modeldeployment external-fixed -o yaml`.
Read Pod UIDs, images, addresses, and endpoint labels separately:

```bash
kubectl -n gpustack-elastic get pods \
  -l app.kubernetes.io/instance=external-fixed -o json |
  jq '.items[] | {name: .metadata.name, uid: .metadata.uid,
      labels: .metadata.labels, podIP: .status.podIP,
      containers: .status.containerStatuses}'
kubectl get devices -o json
```

The recorded native checks found:

| Pod member | DP rank | TP size | DP size | EP world | Distinct GPUs |
|---|---|---|---|---|---|
| 0 | 0 | 2 | 2 | 4 | 2 |
| 1 | 1 | 2 | 2 | 4 | 2 |

All four worker GPU identities were distinct. Both ranks reported External LB enabled and Elastic EP disabled.
Declared arguments and `Ready` status alone do not establish these native results.

## Step 3: send requests to each rank

Open one port-forward per rank in separate terminals:

```bash
kubectl -n gpustack-elastic port-forward pod/external-fixed-server-r0-m0 18000:8000
```

```bash
kubectl -n gpustack-elastic port-forward pod/external-fixed-server-r0-m1 18001:8000
```

Send an ordinary request and a streaming request to each endpoint:

```bash
for port in 18000 18001; do
  curl --fail-with-body --max-time 30 "http://127.0.0.1:$port/v1/completions" \
    -H 'Content-Type: application/json' \
    -d '{"model":"/models","prompt":"The sky is","max_tokens":8,"temperature":0}'
  curl --fail-with-body --no-buffer --max-time 30 "http://127.0.0.1:$port/v1/completions" \
    -H 'Content-Type: application/json' \
    -d '{"model":"/models","prompt":"A GPU is","max_tokens":8,"temperature":0,"stream":true,"stream_options":{"include_usage":true}}'
done
```

The measured requests used direct Pod addresses inside the cluster. Each request had one attempt.
Both ranks returned HTTP 200, nonempty text, eight completion tokens, and a valid finish reason.
Both streams ended with `data: [DONE]`.
These short checks establish functional inference, not throughput or sustained-load behavior.

## Step 4: check Router coverage

Discover the Router Pod and forward its API and metrics ports:

```bash
ROUTER_POD=$(kubectl -n gpustack-elastic get pods \
  -l app.kubernetes.io/instance=external-fixed -o json |
  jq -r '.items[] | select(any(.spec.containers[]; .name == "router")) | .metadata.name')
kubectl -n gpustack-elastic port-forward "pod/$ROUTER_POD" 18081:8081 19090:9090
```

In another terminal, read the registered workers and per-worker request counters:

```bash
curl --fail-with-body http://127.0.0.1:18081/observer/endpoints |
  jq '.workers[] | {url, selection}'
curl --fail-with-body http://127.0.0.1:19090/metrics |
  grep '^vllm_router_processed_requests_total'
```

Repeat the completion requests through port 18081. Compare the counters before and after.
For complete External LB coverage, the registry must contain both rank endpoints.
Requests must increase each rank's counter. A successful response through the Router is insufficient.

The corrected run registered both healthy endpoints and included both in selection.
Four Router requests passed on their first attempt: two ordinary requests and two streams.
Each rank's request counter increased by two. The [validation record](#validation-record) compares the original and corrected runs.

For ordinary Internal groups and [P/D deployments](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode/index.md), the leader restriction remains necessary.
See [Prefill and Decode](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode/index.md) for the deferred External DP combination and its tracking issue.
This Server example does not validate that combination.

## Understand the scaling limits

This fixed External DP group has no online DP/EP resize contract.
vLLM 0.29.0 rejects combining its native Elastic EP mode with External or Hybrid LB.
Use the [Ray Internal Elastic walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/elastic-ep/index.md) for `elasticEp.width` growth.
A Router in front of that Internal endpoint does not turn the engine into External LB.

Increasing `roles[].replicas` creates additional complete groups with their existing TP/DP/EP shape.
That operation does not enlarge an existing group's EP world and was not measured in this External run.
Changing `size` replaces whole replicas. It does not resize a running DP group.
See [Rollout behavior](/gpustack-operator/main/docs/modules/model-deployment/deployment/index.md#rollout-behavior).
Hybrid and multiple HTTP ports per Pod are outside this walkthrough.

## Step 5: inspect and release the application

Read both ranks' logs when startup or collective requests stall:

```bash
kubectl -n gpustack-elastic logs external-fixed-server-r0-m0 -c main
kubectl -n gpustack-elastic logs external-fixed-server-r0-m1 -c main
kubectl -n gpustack-elastic get modeldeployment external-fixed -o yaml
kubectl -n gpustack-elastic get services,endpointslices
```

Check matching DP sizes, distinct rank values, a shared leader address, network access, and free GPU resources.
Keep Router discovery evidence separate from engine startup and inference evidence.
The replica's headless Service supplies peer DNS; narrowing it to the HTTP leader can prevent group startup.

After saving the results, stop the port-forward processes and delete this application:

```bash
kubectl -n gpustack-elastic delete modeldeployment external-fixed
kubectl -n gpustack-elastic get pods,services,deployments \
  -l app.kubernetes.io/instance=external-fixed
kubectl get devices -o json
```

Verify that its owned workloads are gone and all four GPU claims are released.
In the final corrected run, engine Pods disappeared and all eight cards returned to their free state.
The Router and Services remained after about two minutes of observation.
Their deletion required explicit cleanup after confirming their owner UID.

Automatic dependent cleanup was not established by that run; its cause remains unconfirmed.
[Issue #601](https://github.com/gpustack/gpustack-operator/issues/601) tracks related cleanup delays after worker outages; this run did not establish that cause.

Deleting the application retains the model cache and cloud cluster.

## Validation record

The initial operator binary was `d00f8fa8c98334a71af4c6eb302ee80d08289309` from
`gpustack/gpustack-operator:dev@sha256:84f85914bdcb398b2f662dfdcb0845ee853597ec5661a5cf41828acae6a11e36`.
The corrected run used binary `20d674b7f8c1e1d4c22b91a09fd178b9738d70be` from
`thxcode/gpustack-operator:dev-20d674b7@sha256:43c14087fd3391e4bf4a3327f246053d9e59b2feb684cb792f63a2f0db659fbd`.
That test image replaced the operator binary on the pinned official base. It retained the packaged vendor assets.

The official runner below contained vLLM 0.29.0 and Ray 2.54.0, with no package replacements:
`gpustack/runner:cuda13.0-vllm0.29.0`.
The router image was `gpustack/llm-router:v0.2.0`.
Its CuPy installation contained only `cupy-cuda13x` 14.2.0.

The initial operator retained `member-index=0` and registered only member 0.
All four Router requests completed, but all four increased only member 0's counter.
After the fix, discovery registered both healthy endpoints and included both in selection.
Four Router requests completed on their first attempt: two ordinary and two streaming requests.
Each returned eight completion tokens; both streams ended with `data: [DONE]`.

| Operator run | Registered ranks | Rank 0 counter increase | Rank 1 counter increase | Coverage |
|---|---|---|---|---|
| Initial official dev | 0 | 4 | 0 | Failed |
| Selector fix | 0, 1 | 2 | 2 | Passed |

The correction removed the leader restriction only from eligible External Server endpoints.
Both engine Pods remained running through the Operator upgrade. The Router Pod restarted with its corrected selector.

## Troubleshooting

| Symptom | Condition / Reason | Check & Mitigation |
|---|---|---|
| No router Pod exists | `RouterReady=False/RenderFailed` | The operator refused to render the router from the spec, so it creates no router Pod for it. The condition message carries the refusal, such as roles that serve on different ports. Fix `spec.router` or the roles. See [Routing](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md#which-pods-receive-traffic) and [The router block](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode/index.md#the-router-block) |
| Router sends all traffic to rank 0 only | None; the leader-only endpoint selector still applies | Check that the operator build includes the External selector fix: the initial build in the validation record registered only member 0. Check that `parallelism.loadBalance` reads `External` in the role status. Check that the Router's `/observer/endpoints` lists both ranks. See [Routing](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md#which-pods-receive-traffic) |
| DP ranks fail distributed initialization | None; read the engine logs of both members | Each member reaches the leader at `<deployment>-<role>-r<replica>-m0.<deployment>-<role>-r<replica>`. Check `$(GPUSTACK_REPLICA_LEADER_ADDRESS)` in `--data-parallel-address`, distinct rank values, matching DP sizes, and the DP RPC port between the Pods |
| Direct HTTP calls succeed but Router calls fail | `endpoints.eligible` is `null` or `0`, or `endpoints.serving.state` is not `Confirmed` | The Router selects only endpoints the operator qualified. Read `status.roles[].endpoints` and the Router's `/observer/endpoints`. See [Model Deployment Status](/gpustack-operator/main/docs/modules/model-deployment/status/index.md#status) |
| Router and Services remain after deployment deletion | None; the cause is unconfirmed | This run left them for about two minutes and needed explicit cleanup. [Issue #601](https://github.com/gpustack/gpustack-operator/issues/601) tracks related delays, but this run did not establish that cause. Confirm the leftovers' owner UID, then delete them with `kubectl -n gpustack-elastic delete deployments,services -l app.kubernetes.io/instance=external-fixed` |

---

**See also** — [Elastic EP Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/elastic-ep/index.md) (collective expansion) ·
[Routing](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md) (backend selection) · [Model Deployment Configuration](/gpustack-operator/main/docs/modules/model-deployment/deployment/index.md) (role fields)

**Next** → [Model Deployment Status](/gpustack-operator/main/docs/modules/model-deployment/status/index.md)
