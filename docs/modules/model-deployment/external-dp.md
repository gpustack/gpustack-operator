# External DP

*External data parallelism (External DP)* runs one fixed group in which every rank exposes its own HTTP endpoint,
and no rank is a leader, so a Router in front is **required** — admission refuses a role that
declares external data parallelism without *`spec.router`*, naming the rule `RouterRequired`. For
the vLLM captured commands and results, follow the
[External DP Walkthrough](../../walkthroughs/model-deployment/external-dp.md).

The recorded runs apply to two combinations, and they are the two the engine-match rule
admits: vLLM `v0.30.0` on NVIDIA *H100* behind the `vllm-router` binary of
`gpustack/llm-router:v0.3.1` under `--policy round_robin`, and SGLang `0.5.22` behind the
`sglang-gateway` binary of `gpustack/llm-router:v0.3.1`. Other versions must be re-verified
before they inherit a claim on this page.

## Contents

- [Overview and selection](#overview-and-selection)
- [Configuration](#configuration)
- [Load-balancing derivation](#load-balancing-derivation)
- [Per-rank Pod identity](#per-rank-pod-identity)
- [Port allocation](#port-allocation)
- [Endpoint selection](#endpoint-selection)
- [Probe and health](#probe-and-health)
- [Limits](#limits)
- [Admission constraints](#admission-constraints)
- [Results](#results)

## Overview and selection

A *mixture of experts (MoE)* model with *multi-head latent attention (MLA)* or otherwise narrow
attention heads spends most of a wide tensor-parallel group's bandwidth on the expert collective,
not on attention. Splitting the group into ranks with *data parallelism (DP)* gives each rank a
narrow *tensor parallelism (TP)* slice in one shared *DP* × *TP* collective with *expert parallelism (EP)*.
Requests run against independent rank endpoints instead of queuing on one master process.

Elastic serving uses *elastic expert parallelism (Elastic EP)* with *internal load balancing (Internal LB)*.

*External DP* is the mode itself, on either engine: a fixed group whose every rank or member
exposes its own HTTP endpoint behind a Router. The vLLM shape runs one independent engine per
rank; the SGLang shape runs one distributed launch over the members
([SGLang shape](#sglang-shape)).

The serving configurations compare as follows:

| Configuration | Collective | HTTP ingress | Resize contract |
|---|---|---|---|
| Monolithic *TP* | one engine process group, one master | one endpoint (the master) | replica replacement, not in-place |
| *Elastic EP* | Ray *Internal LB*, one instance | one endpoint (the master) | online *`elasticEp.width`* scale-up |
| *vLLM External DP* | fixed ranks, `--data-parallel-rank` each | one endpoint **per rank**, Router-balanced | fixed; world-size changes recreate |
| *SGLang External DP* | one distributed launch over `size` members, *FFN* tensor-parallel across them | one endpoint **per member**, Router-balanced | fixed; replica count changes recreate groups |

Choose *External DP* for fixed *MoE* groups that need per-rank or per-member HTTP ingress or
lower median first-token latency under high concurrency. Its independent rank or member
schedulers do not guarantee higher output throughput.
Choose [Elastic EP](elastic-ep.md) when the collective must grow online while retaining surviving `Pod`s.

The H100 recording ran the vLLM shape: each rank used intra-`Pod` NVLink for *TP* = 2.
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
      instanceType: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
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

### SGLang shape

The SGLang configuration is the same shape on the SGLang engine, declared through SGLang's own
launch arguments. One instance is **one SGLang distributed launch spanning `size` member `Pod`s**,
with **one attention-DP group per member `Pod`**: under `SGLANG_RUST_SERVER=1` every attention-DP
group hosts an HTTP listener of its own, so each member `Pod` answers on the base port of its own
`Pod` IP and the Router balances across the members.

The launch spans the members, so the *TP* width counts every member's GPU while the attention
width counts the groups: with two members and one GPU each, `--tp-size 2 --attn-dp-size 2
--nnodes 2` gives each member one attention-DP group of one scheduler, with the *FFN*
tensor-parallel group spanning both members.

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: qwen3-sglang-external-dp
  namespace: team-a
spec:
  model:
    name: Qwen/Qwen3-8B
  engine:
    name: SGLang
    image: gpustack/runner:cuda13.0-sglang0.5.22
  router:
    name: sglang-gateway  # balances requests across all member endpoints
  roles:
    - name: server
      kind: Server
      replicas: 1
      size: 2  # two member Pods in one fate-sharing group
      instanceType: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
      resources:
        accelerator: "1"  # one GPU per member; the FFN tensor-parallel group spans both
      extraArgs:
        - --tp-size
        - "2"
        - --attn-dp-size
        - "2"
        - --nnodes
        - "2"
        - --node-rank
        - $(GPUSTACK_MEMBER_INDEX)  # zero-based member index injected by the operator
        - --dist-init-addr
        - $(GPUSTACK_REPLICA_LEADER_ADDRESS):29500  # leader DNS name injected by the operator
      env:
        - name: SGLANG_RUST_SERVER
          value: "1"
```

`--node-rank` is the only argument that differs between members. It carries
`$(GPUSTACK_MEMBER_INDEX)` verbatim, and Kubernetes expands the reference in the container
arguments against the member's own environment — the same mechanism the vLLM example above uses
for `--data-parallel-rank`. The operator stamps the member index per member and the leader's DNS
name per instance; it does not restate them in the manifest.

The **dist-init port must differ from the role's HTTP port** and stay clear of the role's port
range. The operator fills the listener's `--host` and `--port` (`8000` by default); the example
uses `29500` for the collective rendezvous, which is a member-local port — each member has its own
network namespace, so the derived-port budget does not collide across members.

Use the modern `--attn-dp-size` spelling. The deprecated `--dp-size N --enable-dp-attention` pair
resolves to the same attention width, but the operator reads `--attn-dp-size` and
`--attention-data-parallel-size`, and the deprecated pair is not what the classification contract
below is stated in.

Where the vLLM shape runs one independent engine per rank, the SGLang shape is one launch with a
collective in it: attention runs per member and the *FFN* tensor-parallel group spans the members.
Three consequences follow, and they are the honest cost of the shape:

- **A member fault stalls the survivor's in-flight requests** until the collective aborts, instead
  of failing fast. The group is withdrawn as a unit; the survivor is not left serving a collective
  whose peer is gone.
- **Capacity is the group, not the member count.** Two members of one GPU each serve one model's
  attention width, not two independent models' worth of capacity.
- **Withdrawal is group-atomic**, under the same supervisor contract the vLLM shape's groups use:
  a fault in any member withdraws every candidate endpoint of that group, and the group returns
  only when every member is qualified again.

### Classification contract

An SGLang role derives *External* only when **all** of these hold:

| Condition | Why it is necessary |
|---|---|
| `--attn-dp-size` (or `--attention-data-parallel-size`) ≥ 2 | More than one attention group exists |
| `--nnodes` ≥ 2 | The groups are on different members |
| `SGLANG_RUST_SERVER` set to a true value in the role's environment | Every group gets a listener of its own, so the group is reachable from outside |
| The role's kind is `Server` | A *Prefill* or *Decode* role is served through its own leg, and the balance of that leg is the leg's own business |

Anything less stays **Internal**, leader-served. A single-`Pod` attention-DP launch is *internal
data parallelism* and is not this shape; an attention-DP launch behind the Python frontend is not
this shape either, because the groups share one frontend and the group is balanced inside itself.
The vote reads the environment value the way SGLang's own environment reader reads it, and a role
that declares a value SGLang would refuse is left to the engine to refuse.

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
SGLang declares its degrees through `--dp-size`, `--ep-size` and `--attn-dp-size`, and reaches
*External* through its own contract — see [SGLang shape](#sglang-shape).

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
The [walkthrough](../../walkthroughs/model-deployment/external-dp.md#step-1-start-the-fixed-group) supplies the reusable manifest.

## Endpoint selection

An ordinary Internal deployment with `size > 1` exposes one serving instance through its leader:
the role's `Service` and Router selectors carry a `member-index=0` term, because subordinate members
expose no HTTP endpoint. An External role is the opposite — every member answers — so both selectors
must cover all ranks.

An External role is a managed protected group, so its selectors narrow from the first enable:
`modelDeploymentSelectEligibleEndpoints` (`pkg/worker/controllers/worker/model_deployment_service.go`)
deletes the leader term from the role's `Service` selector, and the Router role selector does the same
(`model_deployment_router.go`), so `gpustack/llm-router` discovers and balances across every qualified
rank `Pod`. The eligibility term is what admits the qualified ranks.

The answering side agrees: an External role's endpoint-selection mode is `modelDeploymentAnsweringAll`, read
from the members' running arguments rather than the current spec, so a role edited mid-flight keeps
serving on its original ranks.

Status carries both sides of the gate. *`endpoints.eligible`* counts the endpoints the operator
qualified, and *`endpoints.serving`* is a separate confirmation taken over the role's endpoints — in
the recorded single-replica, four-rank configuration, `eligible` read `1` (one replica group) while
`serving` confirmed `4` HTTP endpoints.

The Router's registry and per-worker counters are the end-to-end evidence; a successful response
through the Router alone does not prove all ranks are selected.
See [Status](status.md) and [Routing](routing.md).

## Probe and health

A member's `/health` answers for that member alone: on the vLLM shape it says nothing about the
other ranks, and on the SGLang shape it is blind to its peer in the shared *FFN* collective. The
qualification guard is the **group-forward probe** — a real generation posted to *every* member,
each of which crosses the group's collective.

A passing probe on member *i* therefore proves the group served a real generation through member
*i*'s own listener, which is a stronger qualification than an independent replica's. A member that
answers `/health` while its group is broken is not a healthy group.

## Limits

- **Scale-down waits out the disaggregated handoff grace window (60 s)** before the member gauge
  reads, so total scale-down latency is at least 60 s on top of the drain itself. The group reads
  `Draining` during that window; that is the contract, not a stall.
- **Single-role deployments only on the SGLang shape.** A deployment that mixes an *SGLang
  External DP* role with other roles is out of scope for this round.
- **Dense text models only on SGLang.** The Rust server's pipeline refuses multimodal models.
- **`--load-balance-method` is not rendered on SGLang.** Under the Rust server it is inert; the
  group's own balance is the Router's. This is recorded, not relied on.

## Admission constraints

Admission enforces these boundaries on managed roles:

- `MultiPort` (`--data-parallel-multi-port-external-lb`) is refused: managed roles require one HTTP
  endpoint per `Pod`.
- An unreadable balance (`Unknown`) is refused with the parse's stated reason.
- An *External* role with no `spec.router` is refused with the rule named `RouterRequired`: an
  external group has one endpoint per member and no leader, so the Router is what balances it. The
  refusal reads the role's own extra arguments, so it does not reach a role that took its command
  line over.
- The *Encode* role is refused beside External and Hybrid load balance: an encoder has one producer
  and one output key, so there is no per-member endpoint to balance across.
- *Prefill* and *Decode* roles beside *External DP* are not refused, but the combination is unvalidated
  and tracked in [issue #754](https://github.com/gpustack/gpustack-operator/issues/754).
- *Elastic EP* and *External DP* are mutually exclusive configurations: the engine rejects its native *Elastic EP*
  mode combined with External or *hybrid load balancing (Hybrid LB)*, and the *`elasticEp`* profile admits exactly one *Server*
  role with `size: 1` — its *`width`*, not *DP* ranks, sizes the collective.

*`size`* is a fate-sharing group, not a resize control: changing it replaces whole replicas instead of
resizing a running *DP* group (see [Rollout behavior](deployment.md#rollout-behavior)). Growing or
shrinking the *DP* world means recreating the deployment — or using [Elastic EP](elastic-ep.md) for
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
[InfiniBand RDMA comparison](external-dp-benchmark.md#infiniband-rdma-comparison). Cross-node
placement remains unmeasured.

[External DP Benchmark](external-dp-benchmark.md) compares a four-rank *TP* = 2, *DP* = 4
*External DP* group against a monolithic *TP* = 8 group on the same eight GPUs; the
[External DP Walkthrough](../../walkthroughs/model-deployment/external-dp.md) holds the per-step
commands, the native group measurements, and the troubleshooting rows.

**The SGLang runtime record.** Recorded on SGLang `0.5.22` behind the `sglang-gateway` binary
of `gpustack/llm-router:v0.3.1`, two groups of two members each: all eight content checks —
ordinary, JSON, Chinese and streaming requests through each member's own listener — passed in
both runs. The scale-down held the disaggregated handoff grace window, the withdrawn group's
workers leaving the gateway's worker set after 482 s in one run and 310 s in the other.

The gateway's worker set covered at most one worker per group in both runs — two healthy
workers for two groups in one run and one in the other; the registration boundary and its image
floor are under [Routing](routing.md#which-pods-receive-traffic).

After the scale-down the surviving group never republished `Eligible` within the case's 900 s
budget while the retired group read `Withdrawn`, so a surviving group's recovery from a
scale-down is unverified.

---

**See also** — [Model Deployment Configuration](deployment.md) (role fields, rollout) ·
[Elastic EP](elastic-ep.md) (online width growth) ·
[Routing](routing.md) (router engines and policies) ·
[External DP Benchmark](external-dp-benchmark.md) (the measured crossover against a
monolithic wide tensor-parallel group)

**Next** → [Model Deployment Status](status.md) — read *`parallelism.loadBalance`* and the endpoint gate.
