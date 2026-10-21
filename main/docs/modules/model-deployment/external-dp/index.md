# External DP

*External data parallelism (External DP)* runs one fixed group in which every rank exposes its own HTTP endpoint,
and the Router balances requests across all of them. For captured commands and results, follow the
[External DP Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/external-dp/index.md).

## Contents

- [Overview and selection](#overview-and-selection)
- [Configuration](#configuration)
- [Load-balancing derivation](#load-balancing-derivation)
- [Per-rank Pod identity](#per-rank-pod-identity)
- [Port allocation](#port-allocation)
- [Endpoint selection](#endpoint-selection)
- [Admission constraints](#admission-constraints)
- [Results](#results)

## Overview and selection

A *mixture of experts (MoE)* model with *multi-head latent attention (MLA)* or otherwise narrow
attention heads spends most of a wide tensor-parallel group's bandwidth on the expert collective,
not on attention. Splitting the group into ranks with *data parallelism (DP)* gives each rank a
narrow *tensor parallelism (TP)* slice in one shared *DP* × *TP* collective with *expert parallelism (EP)*.
Requests run against independent rank endpoints instead of queuing on one master process.

Elastic serving uses *elastic expert parallelism (Elastic EP)* with *internal load balancing (Internal LB)*.

The serving configurations compare as follows:

| Configuration | Collective | HTTP ingress | Resize contract |
|---|---|---|---|
| Monolithic *TP* | one engine process group, one master | one endpoint (the master) | replica replacement, not in-place |
| *Elastic EP* | Ray *Internal LB*, one instance | one endpoint (the master) | online *`elasticEp.width`* scale-up |
| *External DP* | fixed ranks, `--data-parallel-rank` each | one endpoint **per rank**, Router-balanced | fixed; world-size changes recreate |

Choose *External DP* for fixed *MoE* groups that need per-rank HTTP ingress or lower median first-token
latency under high concurrency. Its independent rank schedulers do not guarantee higher output throughput.
Choose [Elastic EP](/gpustack-operator/main/docs/modules/model-deployment/elastic-ep/index.md) when the collective must grow online while retaining surviving `Pod`s.

In the H100 recording, each *External DP* rank used intra-`Pod` NVLink for *TP* = 2.
The four ranks shared one *EP* collective over **cross-`Pod` TCP sockets on `eth0`**.
The wide *TP* = 8 comparison kept all eight GPUs inside one NVLink-connected `Pod`.
Separate GPU visibility, private IPC and `/dev/shm`, and no *remote direct memory access (RDMA)* interface
bounded the measured transport; host NVLink did not carry cross-`Pod` traffic.

## Configuration

A four-rank *External DP* group runs one *Server* role with `size: 4` and *TP* = 2 per member behind
`vllm-router`:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: deepseek-external-dp
  namespace: team-a
spec:
  model:
    name: deepseek-ai/DeepSeek-V2-Lite-Chat
  engine:
    name: vLLM
    image: gpustack/runner:cuda13.0-vllm0.30.0
  router:
    name: vllm-router  # balances requests across all rank endpoints
    extraArgs:
      - --policy
      - round_robin
  roles:
    - name: server
      kind: Server
      replicas: 1
      size: 4  # four member Pods in one fate-sharing DP group
      instanceType: gpustack-nvidia-h100-80gb-hbm3-linux-amd64
      resources:
        accelerator: "2"  # matches local --tensor-parallel-size per member
      extraArgs:
        - --tensor-parallel-size
        - "2"
        - --data-parallel-size
        - "4"
        - --data-parallel-rank
        - $(GPUSTACK_MEMBER_INDEX)  # zero-based member index injected by the operator
        - --data-parallel-address
        - $(GPUSTACK_REPLICA_LEADER_ADDRESS)  # leader DNS name injected by the operator
        - --enable-expert-parallel
        - --all2all-backend
        - allgather_reducescatter
      env:
        - name: VLLM_PORT
          value: "19500"  # keeps candidate ports below the ephemeral source-port range
```

When the cluster provides an *RDMA* fabric, add `interface: "2"` under *`resources`* so cross-`Pod`
expert collectives run over InfiniBand or RoCE instead of TCP.

## Load-balancing derivation

There is no *External DP* spec field. The operator reads each role's own *`extraArgs`* and *`env`* with
the engine's flag table (`scanModelDeploymentParallelism`,
`pkg/worker/controllers/worker/model_deployment_parallelism.go`) and derives a load-balancing mode:

| Argument (vLLM spelling) | Vote |
|---|---|
| `--data-parallel-external-lb` | External |
| `--data-parallel-rank` | External |
| `--data-parallel-hybrid-lb` | Hybrid |
| `--data-parallel-multi-port-external-lb` | MultiPort (refused on managed roles) |

Without an explicit load-balancing flag, degree-based derivation runs only when `--data-parallel-start-rank`
is present and `--headless` is absent: `--data-parallel-size-local` below `--data-parallel-size`
yields External (local size 1) or Hybrid, equal values yield Internal, and `--nnodes` above one
without a local size is Unknown; otherwise the no-vote branch returns Internal.

A Hybrid vote also normalizes to External at local size 1 and Internal when local size equals global
*DP*, while contradicting flags yield Unknown and admission refuses the role with the named disagreement.

The derived mode is reported per role as `.status.roles[].parallelism.loadBalance` (`External`,
`Hybrid`, `Internal`, `MultiPort`, or `Unknown`), beside the declared degrees (*`dataParallel`*,
*`dataParallelLocal`*, *`expertParallel`*) and modes (*`expertParallel`* from `--enable-expert-parallel`).
SGLang declares the degrees through `--dp-size` and `--ep-size`, but the External load-balancing flags are
vLLM's — an SGLang role resolves to Internal unless its engine accepts the same flags.

## Per-rank Pod identity

A *Server* role with `size: N` runs N member `Pod`s in one replica group; each member carries one *DP*
rank and the whole group shares one collective. The operator injects two environment variables into every
member and Kubernetes expands their references in the container arguments:

- `GPUSTACK_MEMBER_INDEX` — the zero-based rank of this member; pass it to `--data-parallel-rank`.
- `GPUSTACK_REPLICA_LEADER_ADDRESS` — the leader's DNS name; pass it to `--data-parallel-address`.

The replica's headless `Service` `<deployment>-<role>-r<replica>` provides peer DNS; each member
reaches the leader at
`<deployment>-<role>-r<replica>-m0.<deployment>-<role>-r<replica>`. Narrowing that `Service` to the
HTTP leader can prevent group startup.

The *TP*, *DP*, rank and address arguments stay in *`extraArgs`*; the operator does not infer them from
*`size`* or GPU requests. On the recorded vLLM run, these arguments could be omitted because the
engine derives them from `--data-parallel-rank`:

| Omitted argument | Why it is unnecessary |
|---|---|
| `--data-parallel-external-lb` | An explicit `--data-parallel-rank` selects *external load balancing (External LB)* |
| `--data-parallel-size-local 1` | *External LB* runs one local *DP* rank per member |
| `--api-server-count 1` | *External LB* defaults to one API server per rank |
| `--data-parallel-backend mp` | `mp` is the default *DP* backend |
| `--distributed-executor-backend mp` | The local *TP* group fits inside one `Pod` |
| `--data-parallel-rpc-port` | All four ranks used the default RPC port |

`--data-parallel-backend` selects execution placement; it does not select the *load balancing (LB)* mode.

## Port allocation

For the recorded ephemeral source-port range `32768–60999`, set **`1024 <= VLLM_PORT < 32768`**, such as `19500`, on every rank.
Keep the full candidate range below `32768` and clear of HTTP `8000`, *DP* `29500` / `29550`, and other listeners.
Check `net.ipv4.ip_local_port_range` if customized. This prevents ephemeral-source collisions within that range;
other listeners and allocation races still require checks.

### Allocation and coordination

In vLLM v0.30.0, [`get_open_ports_list`](https://github.com/vllm-project/vllm/blob/v0.30.0/vllm/utils/network_utils.py)
probes five free ports, then closes the probe sockets. With `VLLM_PORT` unset, it binds port `0` for kernel allocation.
With a base set, it scans upward, skips occupied ports, and advances after each find.
The probe sockets close immediately; another connection can take a candidate before initialization.

The *MoE* [startup handshake](https://github.com/vllm-project/vllm/blob/v0.30.0/vllm/v1/engine/utils.py)
distributes the leader's initial master port and candidate list to the engine ranks.
Unrelated initial kernel selections therefore do not inherently cause a mismatch.
A candidate can still become occupied after probing and before group initialization.

Fixed multiprocessing *External DP* does not enable the *Elastic EP* coordination-store path, so
*`_coord_store_port`* stays at its default `0`. The *MoE* `DPCoordinator` handles wave coordination;
it is separate from the store that broadcasts replacement group-init ports.
In [`parallel.py`](https://github.com/vllm-project/vllm/blob/v0.30.0/vllm/config/parallel.py),
`_pick_stateless_dp_port` therefore consumes the local candidate list through `get_next_dp_init_port`.

`stateless_init_dp_group` makes at most **five attempts**, retrying `DistNetworkError` only when it contains `EADDRINUSE`.
Each retry consumes the next local candidate; after list exhaustion, the master-port fallback increments.
With *`_coord_store_port`* = `0`, the leader does not broadcast its retry port to peers.

### Conflict boundaries

| Conflict | Behavior |
|---|---|
| Symmetric during allocation | If every `Pod` finds `19500` occupied and the next five ports free, each scan shifts to `19501`–`19505`. The shared candidate sequence stays aligned, so initialization can succeed. |
| Asymmetric after probing | If only leader `m0` loses its selected port, it retries the next candidate. `m1`–`m3` still connect to the old leader port; their connects do not raise local `EADDRINUSE` or synchronize the retry. |

The symmetric case describes **allocation-time scanning**, not a guarantee of coordinated recovery from a later bind failure.
In the captured asymmetric failure, an outgoing connection occupied leader source port `36558` in the ephemeral range.
Leader `m0` hit `EADDRINUSE` and switched to `36557`; peers kept waiting on `36558`.

With `VLLM_PORT=19500`, the group became ready and the logs showed collective initialization at `19502`.
That port is an observation, not a fixed base-plus-two contract: candidate consumption and conflicts affect port selection.
The [walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/external-dp/index.md#step-1-start-the-fixed-group) supplies the reusable manifest.

## Endpoint selection

An ordinary Internal deployment with `size > 1` exposes one serving instance through its leader:
the role's `Service` and Router selectors carry a `member-index=0` term, because subordinate members
expose no HTTP endpoint. An External role is the opposite — every member answers — so both selectors
must cover all ranks.

The endpoint-eligibility gate makes that switch observable. While the gate is inactive, the selectors
keep the leader term. Once it activates, `modelDeploymentSelectEligibleEndpoints`
(`pkg/worker/controllers/worker/model_deployment_service.go`) deletes the leader term from the
role's `Service` selector, and the Router role selector does the same
(`model_deployment_router.go`), so `gpustack/llm-router` discovers and balances across every qualified rank `Pod`.

The answering side agrees: an External role's endpoint-selection mode is `modelDeploymentAnsweringAll`, read
from the members' running arguments rather than the current spec, so a role edited mid-flight keeps
serving on its original ranks.

Status carries both sides of the gate. *`endpoints.eligible`* counts the endpoints the operator
qualified, and *`endpoints.serving`* is a separate confirmation taken over the role's endpoints — in
the recorded single-replica, four-rank configuration, `eligible` read `1` (one replica group) while
`serving` confirmed `4` HTTP endpoints.

The Router's registry and per-worker counters are the end-to-end evidence; a successful response
through the Router alone does not prove all ranks are selected.
See [Status](/gpustack-operator/main/docs/modules/model-deployment/status/index.md) and [Routing](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md).

## Admission constraints

Admission enforces these boundaries on managed roles:

- `MultiPort` (`--data-parallel-multi-port-external-lb`) is refused: managed roles require one HTTP
  endpoint per `Pod`.
- An unreadable balance (`Unknown`) is refused with the parse's stated reason.
- The *Encode* role is refused beside External and Hybrid load balance: an encoder has one producer
  and one output key, so there is no per-member endpoint to balance across.
- *Prefill* and *Decode* roles beside *External DP* are not refused, but the combination is unvalidated
  and tracked in [issue #754](https://github.com/gpustack/gpustack-operator/issues/754).
- *Elastic EP* and *External DP* are mutually exclusive configurations: the engine rejects its native *Elastic EP*
  mode combined with External or *hybrid load balancing (Hybrid LB)*, and the *`elasticEp`* profile admits exactly one *Server*
  role with `size: 1` — its *`width`*, not *DP* ranks, sizes the collective.

*`size`* is a fate-sharing group, not a resize control: changing it replaces whole replicas instead of
resizing a running *DP* group (see [Rollout behavior](/gpustack-operator/main/docs/modules/model-deployment/deployment/index.md#rollout-behavior)). Growing or
shrinking the *DP* world means recreating the deployment — or using [Elastic EP](/gpustack-operator/main/docs/modules/model-deployment/elastic-ep/index.md) for
width-only growth of a Ray *Internal LB* group.

## Results

The H100 SXM5 recording served `DeepSeek-V2-Lite-Chat` under vLLM v0.30.0 with *TP* = 2, *DP* = 4
and *EP* world size = 8. All four rank endpoints were healthy behind the `round_robin` Router.
Router counters matched engine completion counts: **285 + 284 × 3 requests** (1,137 total: 1,088
measured requests plus 48 warm-ups and 1 calibration probe). Both configurations completed
**1,088/1,088 measured requests with zero errors**.

In the initial TCP recording at concurrency 64, *time to first token (TTFT)* p50 crossed in favor of *External DP*:

| Prompt/output target | Wide *TP* = 8 (ms) | *DP* = 4, *TP* = 2 (ms) | Median reduction |
|---|---:|---:|---:|
| `1024/256` | 4,798.411 | **1,863.100** | **2.58×** |
| `4096/128` | 3,814.139 | **3,094.017** | **1.23×** |

Over TCP, wide *TP* still delivered **2.27×–6.99× higher output throughput** across all twelve matched cells,
and at `4096/128`, concurrency 64, TCP *External DP* had much worse first-token and token-gap tails.

With ConnectX-7 InfiniBand *RDMA* enabled (`resources.interface: "2"`), four-rank *External DP*
reduced median *TTFT* by **6.26×–8.47×** and increased output throughput by **3.85×–7.01×** over TCP,
exceeding the wide-*TP* output throughput at concurrency 64; see
[InfiniBand RDMA comparison](/gpustack-operator/main/docs/modules/model-deployment/external-dp-benchmark/index.md#infiniband-rdma-comparison). Cross-node
placement remains unmeasured.

[External DP Benchmark](/gpustack-operator/main/docs/modules/model-deployment/external-dp-benchmark/index.md) compares a four-rank *TP* = 2, *DP* = 4
*External DP* group against a monolithic *TP* = 8 group on the same eight GPUs; the
[External DP Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/external-dp/index.md) holds the per-step
commands, the native group measurements, and the troubleshooting rows.

---

**See also** — [Model Deployment Configuration](/gpustack-operator/main/docs/modules/model-deployment/deployment/index.md) (role fields, rollout) ·
[Elastic EP](/gpustack-operator/main/docs/modules/model-deployment/elastic-ep/index.md) (online width growth) ·
[Routing](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md) (router engines and policies) ·
[External DP Benchmark](/gpustack-operator/main/docs/modules/model-deployment/external-dp-benchmark/index.md) (the measured crossover against a
monolithic wide tensor-parallel group)

**Next** → [Model Deployment Status](/gpustack-operator/main/docs/modules/model-deployment/status/index.md) — read *`parallelism.loadBalance`* and the endpoint gate.
