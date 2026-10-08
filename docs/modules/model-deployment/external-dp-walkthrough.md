# External DP Walkthrough

Run one fixed vLLM data-parallel (DP) group with an HTTP endpoint on each rank.
This walkthrough records two ranks using the official GPUStack runner.
Direct requests passed on both ranks. The recorded operator's managed Router discovered only rank 0.
That routing limitation remains visible in the checks below.

## Contents

- [Choose the deployment shape](#choose-the-deployment-shape)
- [Prepare the model and GPUs](#prepare-the-model-and-gpus)
- [Start the fixed group](#start-the-fixed-group)
- [Omit redundant arguments](#omit-redundant-arguments)
- [Read the running resources](#read-the-running-resources)
- [Send requests to each rank](#send-requests-to-each-rank)
- [Check Router coverage](#check-router-coverage)
- [Understand the scaling limits](#understand-the-scaling-limits)
- [Inspect and release the application](#inspect-and-release-the-application)

## Choose the deployment shape

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

## Prepare the model and GPUs

Complete [cluster preparation](scaling-walkthrough.md#prepare-the-cluster) and
[model prefetch](scaling-walkthrough.md#prefetch-the-weights) first.
Use the same namespace, cached artifact, model revision, and GPU InstanceType.
The GPU node needs four free cards and the verified model cache.

For the recorded hardware, copy the `InstanceType` document from
[the elastic deployment example](scaling-walkthrough.md#start-the-elastic-deployment)
into `external-instance-type.yaml`. Apply that single resource before creating this deployment:

```bash
kubectl apply -f external-instance-type.yaml
kubectl get instancetype elastic-cpu-gpu
```

For another GPU family, select an existing whole-GPU InstanceType and replace the name in the manifest below.

The recorded operator binary was `d00f8fa8c98334a71af4c6eb302ee80d08289309` from
`gpustack/gpustack-operator:dev@sha256:84f85914bdcb398b2f662dfdcb0845ee853597ec5661a5cf41828acae6a11e36`.
The official runner below contained vLLM 0.29.0 and Ray 2.54.0, with no package replacements.
Its CuPy installation contained only `cupy-cuda13x` 14.2.0.

Each Pod needs a separate Pod IP. The ranks must reach each other over the Pod network.
The DP RPC port defaults to 29550; collective communication also uses other ports.
Setting an engine RPC port does not create a Kubernetes Service or a firewall rule.

## Start the fixed group

Save this as `external-dp.yaml`. Replace `elastic-cpu-gpu` with your GPU InstanceType if needed.
The model delivery steps already created `deepseek-v2-lite-chat` in `gpustack-elastic`.

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: external-minimal
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
      image: gpustack/runner:cuda13.0-vllm0.29.0@sha256:1c826749ed16fbd9f9594d7a49f774904662d9c46231e08e32b494e8bac4ee91
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
    image: gpustack/llm-router:v0.2.0@sha256:98e70d94351baa9dc13897a545aff5c83ffac5e89ab810e204bc894b519d96db
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
kubectl -n gpustack-elastic get modeldeployment external-minimal
kubectl -n gpustack-elastic get pods \
  -l app.kubernetes.io/instance=external-minimal
```

## Omit redundant arguments

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
This example serves each rank on port 8000.
`--data-parallel-backend` selects execution placement; it does not select the LB mode.
`--data-parallel-start-rank` alone is not an External LB declaration.

## Read the running resources

Retain the CR output beside the request results:

```bash
kubectl -n gpustack-elastic get modeldeployment external-minimal -o json |
  jq '{name: .metadata.name, phase: .status.phase,
       size: .spec.roles[0].size, replicas: .spec.roles[0].replicas,
       accelerator: .spec.roles[0].resources.accelerator}'
```

Recorded values from the captured CR:

```json
{
  "name": "external-minimal",
  "phase": "Ready",
  "size": 2,
  "replicas": 1,
  "accelerator": "2"
}
```

Read Pod UIDs, images, addresses, and endpoint labels separately:

```bash
kubectl -n gpustack-elastic get pods \
  -l app.kubernetes.io/instance=external-minimal -o json |
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

## Send requests to each rank

Open one port-forward per rank in separate terminals:

```bash
kubectl -n gpustack-elastic port-forward pod/external-minimal-server-r0-m0 18000:8000
```

```bash
kubectl -n gpustack-elastic port-forward pod/external-minimal-server-r0-m1 18001:8000
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

## Check Router coverage

Discover the Router Pod and forward its API and metrics ports:

```bash
ROUTER_POD=$(kubectl -n gpustack-elastic get pods \
  -l app.kubernetes.io/instance=external-minimal -o json |
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

The recorded Router registered only member 0 because its selector retained `member-index=0`.
All four Router requests completed, but all four increased only member 0's counter.
Member 1 received no Router traffic. Direct rank inference passed; managed Router coverage failed.
Keep this distinction when evaluating the recorded operator revision.

For ordinary Internal groups and [P/D deployments](prefill-decode.md), the leader restriction remains necessary.
External DP inside a Prefill or Decode role is deferred to
[issue #754](https://github.com/gpustack/gpustack-operator/issues/754).
This Server example does not validate that combination.

## Understand the scaling limits

This fixed External DP group has no online DP/EP resize contract.
vLLM 0.29.0 rejects combining its native Elastic EP mode with External or Hybrid LB.
Use the [Ray Internal Elastic walkthrough](scaling-walkthrough.md) for `elasticEp.width` growth.
A Router in front of that Internal endpoint does not turn the engine into External LB.

Increasing `roles[].replicas` creates additional complete groups with their existing TP/DP/EP shape.
That operation does not enlarge an existing group's EP world and was not measured in this External run.
The ordinary role's `size` cannot be changed after creation.
Hybrid and multiple HTTP ports per Pod are outside this walkthrough.

## Inspect and release the application

Read both ranks' logs when startup or collective requests stall:

```bash
kubectl -n gpustack-elastic logs external-minimal-server-r0-m0 -c main
kubectl -n gpustack-elastic logs external-minimal-server-r0-m1 -c main
kubectl -n gpustack-elastic get modeldeployment external-minimal -o yaml
kubectl -n gpustack-elastic get services,endpointslices
```

Check matching DP sizes, distinct rank values, a shared leader address, network access, and free GPU resources.
Keep Router discovery evidence separate from engine startup and inference evidence.
The replica's headless Service supplies peer DNS; narrowing it to the HTTP leader can prevent group startup.

After saving the results, stop the port-forward processes and delete this application:

```bash
kubectl -n gpustack-elastic delete modeldeployment external-minimal
kubectl -n gpustack-elastic get pods,services,deployments \
  -l app.kubernetes.io/instance=external-minimal
kubectl get devices -o json
```

Verify that its owned workloads are gone and all four GPU claims are released.
The recorded cleanup deleted both External test applications and returned all eight cards to their free state.
Deleting the application retains the model cache and cloud cluster.

---

**See also** — [Model Scaling Walkthrough](scaling-walkthrough.md) (prefetch and Elastic EP) ·
[Routing](routing.md) (backend selection) · [Model Deployment Configuration](deployment.md) (role fields)
