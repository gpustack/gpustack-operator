# External DP

*External data parallelism (External DP)* runs one fixed group where every rank exposes its own HTTP
endpoint behind a managed router. Because no rank is an HTTP leader, admission requires
*`spec.router`* and rejects any *External* role without one (`RouterRequired`). See
[External DP Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/external-dp/index.md) for captured vLLM
commands and output.

![External DP router and rank endpoints](/gpustack-operator/main/docs/assets/diagrams/model-deployment-external-dp.svg)

<!--
flowchart TD
    CLIENT["Client"] -> ROUTER["Managed router: round robin"]
    ROUTER -> R0["Rank 0 HTTP"]
    ROUTER -> R1["Rank 1 HTTP"]
    ROUTER -> R2["Rank 2 HTTP"]
    ROUTER -> R3["Rank 3 HTTP"]
    R0 -> EP["Cross-Pod expert collective"]
    R1 -> EP
    R2 -> EP
    R3 -> EP
-->

## Contents

- [Configuration](#configuration)
- [Port allocation](#port-allocation)
- [Endpoint selection](#endpoint-selection)
- [Admission constraints](#admission-constraints)
- [Limitations](#limitations)
- [Results](#results)

## Configuration

A four-rank vLLM *External DP* group uses one *Server* role with `size: 4` and *TP* = 2 per member
behind `vllm-router`:

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
    name: vllm-router  # required; balances requests across all rank endpoints
    extraArgs:
      - --policy
      - round_robin
  roles:
    - name: server
      kind: Server
      replicas: 1
      size: 4  # four member Pods in one fate-sharing DP group
      acceleratorType: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
      resources:
        accelerator: "2"  # matches local --tensor-parallel-size per member
      extraArgs:
        - --tensor-parallel-size
        - "2"
        - --data-parallel-size
        - "4"
        - --data-parallel-rank
        - $(GPUSTACK_MEMBER_INDEX)  # zero-based rank injected by operator
        - --data-parallel-address
        - $(GPUSTACK_REPLICA_LEADER_ADDRESS)  # leader DNS name injected by operator
        - --enable-expert-parallel
        - --all2all-backend
        - allgather_reducescatter
      env:
        - name: VLLM_PORT
          value: "19500"  # keeps candidate ports below ephemeral range
```

To request two *remote direct memory access (RDMA)* endpoints per member, add `interface: "2"`
under *`resources`*; verify active transport separately ([RDMA Operations](/gpustack-operator/main/docs/modules/rdma/operations/index.md)).

### Mode comparison

Splitting a *mixture of experts (MoE)* model across *data parallelism (DP)* ranks gives each rank a
narrow *tensor parallelism (TP)* slice inside one shared *DP* × *TP* *expert parallelism (EP)*
collective, with independent per-rank HTTP schedulers. vLLM runs one engine process per rank; SGLang
runs one distributed launch across `size` member `Pod`s
([SGLang rank configuration](#sglang-rank-configuration)).

| Configuration | Collective | HTTP ingress | Resize contract |
|---|---|---|---|
| Monolithic *TP* | One engine group, one master | Master only | Replaces replicas |
| *Elastic EP* | Ray *Internal LB*, one instance | Master only | Online `elastic.dataParallelWidth` scale-up |
| *vLLM External DP* | Fixed ranks (`--data-parallel-rank`) | Every rank, Router-balanced | Fixed; world-size edits recreate |
| *SGLang External DP* | Distributed launch over `size` members | Every member, Router-balanced | Fixed; replica edits recreate groups |

- **Choose *External DP*** for fixed *MoE* groups needing per-rank HTTP ingress or lower
  high-concurrency median *TTFT*.
- **Choose [Elastic EP](/gpustack-operator/main/docs/modules/model-deployment/elastic-ep/index.md)** when the collective must grow online while keeping
  surviving `Pod`s.

### SGLang rank configuration

SGLang runs one distributed launch spanning `size` member `Pod`s with one attention-DP group per
member. With `SGLANG_RUST_SERVER=1`, each member serves HTTP on its base port behind
`sglang-gateway`, while the *FFN* tensor-parallel group spans all members:

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
    name: sglang-gateway  # required; balances across member endpoints
  roles:
    - name: server
      kind: Server
      replicas: 1
      size: 2  # two member Pods in one fate-sharing group
      acceleratorType: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
      resources:
        accelerator: "1"  # one GPU per member; FFN TP spans both
      extraArgs:
        - --tp-size
        - "2"
        - --attn-dp-size
        - "2"
        - --nnodes
        - "2"
        - --node-rank
        - $(GPUSTACK_MEMBER_INDEX)  # zero-based member index injected by operator
        - --dist-init-addr
        - $(GPUSTACK_REPLICA_LEADER_ADDRESS):29500  # must differ from HTTP port
      env:
        - name: SGLANG_RUST_SERVER
          value: "1"
```

- **Rendezvous port**: Keep `--dist-init-addr` port (`29500` above) distinct from the HTTP port
  (`8000` default).
- **Modern flag**: Use `--attn-dp-size` (or `--attention-data-parallel-size`); deprecated
  `--dp-size` + `--enable-dp-attention` does not trigger *External* classification.
- **Fault behavior**: A member fault stalls survivor in-flight requests until the collective aborts;
  capacity equals one group, and withdrawal is group-atomic until every member re-qualifies.

### SGLang mode requirements

An SGLang role derives `External` only when all four conditions hold:

| Condition | Purpose |
|---|---|
| `--attn-dp-size` (or `--attention-data-parallel-size`) >= 2 | Declares multiple attention groups |
| `--nnodes` >= 2 | Places groups across multiple member `Pod`s |
| `SGLANG_RUST_SERVER` set to a true value in *`env`* | Exposes an HTTP listener on every member |
| *`roles[].kind`* is `Server` | *Prefill* and *Decode* stay routed through their own stages |

Single-`Pod` attention-DP or the Python frontend remains `Internal` (leader-served).

### Load balancing modes

There is no *External DP* spec field. The operator inspects *`roles[].extraArgs`* and *`roles[].env`*
and reports *`status.roles[].parallelism.loadBalance`* (`External`, `Hybrid`, `Internal`,
`MultiPort` or `Unknown`) beside declared degrees (*`dataParallel`*, *`dataParallelLocal`*,
*`expertParallel`*):

| Argument (vLLM spelling) | Vote |
|---|---|
| `--data-parallel-external-lb` | `External` |
| `--data-parallel-rank` | `External` |
| `--data-parallel-hybrid-lb` | `Hybrid` |
| `--data-parallel-multi-port-external-lb` | `MultiPort` (refused on managed roles) |

Without an explicit flag, degree-based derivation runs only when `--data-parallel-start-rank` is set
and `--headless` is absent (`--data-parallel-size-local < --data-parallel-size` yields `External`
at local size 1 or `Hybrid`; equal yields `Internal`; `--nnodes > 1` without local size yields
`Unknown`). `Hybrid` normalizes to `External` at local size 1 and `Internal` when local equals
global *DP*. Conflicting flags yield `Unknown` and fail admission.

### Per-rank Pod identity

For a *Server* role with `size: N`, the operator injects `GPUSTACK_MEMBER_INDEX` (zero-based rank)
and `GPUSTACK_REPLICA_LEADER_ADDRESS` (leader DNS on headless `Service`
`<deployment>-<role>-r<replica>`). Pass them via Kubernetes `$(...)` expansion in *`extraArgs`*;
the operator never infers parallel flags from *`size`* or GPU count.

With vLLM, `--data-parallel-rank` implies these defaults without extra flags:

| Omitted argument | Engine default |
|---|---|
| `--data-parallel-external-lb` | Implied by `--data-parallel-rank` (*external load balancing (External LB)*) |
| `--data-parallel-size-local 1` | One local *DP* rank per member under *External LB* |
| `--api-server-count 1` | One API server per rank |
| `--data-parallel-backend mp` | Default *DP* backend (`mp` selects execution, not load balancing) |
| `--distributed-executor-backend mp` | Local *TP* stays inside one `Pod` |
| `--data-parallel-rpc-port` | Default RPC port across all ranks |

## Port allocation

Set **`1024 <= VLLM_PORT < 32768`** (such as `19500`) on every vLLM rank, keeping candidate ports
below the Linux ephemeral range (`32768–60999`) and clear of `8000`, `29500` and `29550`. Check
`net.ipv4.ip_local_port_range` if customized.

### Rank port negotiation

vLLM v0.30.0 probes five free ports (`get_open_ports_list`), closes the probe sockets and shares
the leader's candidate list during the *MoE* handshake. Fixed multiprocessing *External DP* keeps
`_coord_store_port = 0`, so `stateless_init_dp_group` retries `EADDRINUSE` up to five times locally
without broadcasting the retry port to peers.

### Port conflicts

| Conflict | Behavior |
|---|---|
| Symmetric during allocation | All ranks skip an occupied base (for example `19500` → `19501`–`19505`) and stay aligned |
| Asymmetric after probing | Only leader `m0` hits `EADDRINUSE` and advances ports while `m1`–`m3` stall on the old port |

In the captured failure, an outbound connection took leader ephemeral port `36558`; `m0` moved to
`36557` while peers hung on `36558`. With `VLLM_PORT=19500`, the group converged on `19502`
(an observed port, not a fixed offset; see
[walkthrough manifest](/gpustack-operator/main/docs/walkthroughs/model-deployment/external-dp/index.md#step-1-deploy-rank-group)).

## Endpoint selection

Unlike `Internal` multi-member roles (which restrict HTTP selectors to `member-index=0`), an
`External` role drops the leader term on both its role `Service` and Router selectors while keeping
the `endpoint-eligible` label requirement. Endpoint mode is read from running member arguments, so
mid-flight edits keep serving existing ranks.

Check *`status.roles[].endpoints.eligible`* (qualified replica groups, `1` in the four-rank run)
and *`endpoints.serving`* (Router-confirmed HTTP endpoints, `4` in the four-rank run). Confirm
per-worker balance in the Router registry and metrics
([Endpoint status](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md#endpoint-status)).

### Rank health checks

A member's `/health` checks only that member, not the collective. Qualification therefore runs a
**group-forward probe** (a real generation request) against every member across the collective. A
member that answers `/health` while its collective is broken does not qualify.

## Admission constraints

Admission rejects these managed-role configurations:

- `MultiPort` (`--data-parallel-multi-port-external-lb`) or unparseable `Unknown` load balance.
- `External` role without *`spec.router`* (`RouterRequired`; take-over *`roles[].command`* is
  unchecked).
- *Encode* paired with `External` or *hybrid load balancing (Hybrid LB)*.
- *Elastic EP* (`elastic.dataParallelWidth`) combined with `External` or *Hybrid LB*.

*Prefill* and *Decode* roles with *External DP* are admitted but unvalidated
([#754](https://github.com/gpustack/gpustack-operator/issues/754)).

## Limitations

- **Scale-down grace**: Disaggregated handoff grace adds `60 s` before member drain gauges are read;
  `Draining` during those 60 s is expected.
- **SGLang scope**: Single-role dense text deployments only (Rust server rejects multimodal models).
  `--load-balance-method` is not rendered because the Router owns balancing.
- **Fixed group size**: Editing *`roles[].size`* replaces whole replicas
  ([Rollout behavior](/gpustack-operator/main/docs/modules/model-deployment/configuration/index.md#rollout-behavior)). Use [Elastic EP](/gpustack-operator/main/docs/modules/model-deployment/elastic-ep/index.md) for
  in-place width growth.

## Results

On 8 × H100 SXM5 serving `DeepSeek-V2-Lite-Chat` (vLLM `0.30.0`, *TP* = 2 over NVLink, *DP* = 4,
*EP* = 8 over TCP `eth0` versus single-`Pod` wide *TP* = 8 on NVLink), all four endpoints balanced
evenly (**285 + 284 × 3 = 1,137** total requests) with **1,088/1,088 measured requests** and zero
errors.

Over TCP at concurrency 64, *External DP* lowered *time to first token (TTFT)* p50 against single-`Pod`
wide *TP* = 8, while wide *TP* kept **2.27×–6.99×** higher output throughput across all 12 cells:

| Prompt/output target | Wide TP (ms) | DP4 TP2 (ms) | Median reduction |
|---|---:|---:|---:|
| `1024/256` | 4,798.411 | **1,863.100** | **2.58×** |
| `4096/128` | 3,814.139 | **3,094.017** | **1.23×** |

With ConnectX-7 InfiniBand *RDMA* (`resources.interface: "2"`), *External DP* cut median *TTFT*
**6.26×–8.47×** and raised output **3.85×–7.01×** over TCP, overtaking wide *TP* output at
concurrency 64 ([InfiniBand RDMA comparison](/gpustack-operator/main/docs/modules/model-deployment/external-dp-benchmark/index.md#infiniband-rdma-comparison)).
Cross-node placement is unmeasured.

**SGLang runtime record** (SGLang `0.5.22`, `sglang-gateway` from `gpustack/llm-router:v0.3.1`, two
groups × two members): all eight content checks passed across member listeners, and scale-down
honored the 60 s handoff grace (workers withdrew at 482 s and 310 s).

Without `--dp-aware`, the gateway registered at most one worker per group
([Traffic targets](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md#traffic-targets)), and the surviving group did not republish
`Eligible` within the 900 s budget (post-scale-down survivor recovery remains unverified).

---

**See also** — [Model Deployment Configuration](/gpustack-operator/main/docs/modules/model-deployment/configuration/index.md) (role fields and rollout) ·
[Elastic EP](/gpustack-operator/main/docs/modules/model-deployment/elastic-ep/index.md) (online width growth) ·
[Model Deployment Routing](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md) (router engines and policies) ·
[External DP Benchmark](/gpustack-operator/main/docs/modules/model-deployment/external-dp-benchmark/index.md) (measured crossover against wide *TP*)

**Next** → [External DP Benchmark](/gpustack-operator/main/docs/modules/model-deployment/external-dp-benchmark/index.md) — compare per-rank serving with wide tensor parallelism.
