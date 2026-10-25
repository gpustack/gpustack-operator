# Encode, Prefill and Decode

A deployment with *Encode*, *Prefill* and *Decode* roles runs visual encoding and language generation
in separate `Pod`s. [Model Deployment Configuration](/gpustack-operator/main/docs/modules/model-deployment/deployment/index.md) describes the base role fields and
admission rules.

## Contents

- [Roles and request flow](#roles-and-request-flow)
- [Configuration](#configuration)
- [Admission constraints](#admission-constraints)
- [Admitted matrix](#admitted-matrix)
- [Encoder-cache connectors](#encoder-cache-connectors)
- [Scheduling co-location](#scheduling-co-location)
- [Request routing across roles](#request-routing-across-roles)
- [Replica drain and retirement](#replica-drain-and-retirement)
- [Upgrade and rollout behavior](#upgrade-and-rollout-behavior)
- [Results](#results)

## Roles and request flow

*Encode/prefill/decode (EPD)* serving separates visual encoding from language generation. Each role
runs in its own `Pod` set.

| Role | Does | Notes |
|---|---|---|
| *Encode* | Runs the visual encoder on images and other media | Allocates no KV cache and loads no language-model weights |
| *Prefill* | Consumes the encoder's embeddings and fills the KV cache | Receives the prompt tokens |
| *Decode* | Generates completion tokens from the KV cache | Streams the response |

A multimodal request goes through the encoder, then the prefiller, then the decoder. A text-only
request skips the encoder and goes straight to *prefill* and *decode*.

## Configuration

A complete *EPD* deployment pairs one *Encode*, one *Prefill* and one *Decode* role behind a managed
router:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: qwen-vl-epd
  namespace: team-a
spec:
  model:
    name: Qwen/Qwen2.5-VL-7B-Instruct
  engine:
    name: vLLM  # vLLM or SGLang; vLLM is used here
    image: gpustack/runner:cuda13.0-vllm0.30.0  # must meet the admitted matrix version floor
  router:
    name: llm-d-router  # required to pair Encode, Prefill and Decode
  roles:
    - name: encode
      kind: Encode
      replicas: 1
      instanceType: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
      resources:
        accelerator: "1"  # defaults to 1 on an accelerated InstanceType when omitted
    - name: prefill
      kind: Prefill
      replicas: 1
      instanceType: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
      resources:
        accelerator: "1"
    - name: decode
      kind: Decode
      replicas: 1
      instanceType: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
      resources:
        accelerator: "1"
```

The operator injects the encoder-cache and KV-transfer connectors onto the three roles and fronts
each *Decode* `Pod` with the routing sidecar. On *Encode*, the operator defaults `--mm-encoder-only`,
`--enforce-eager` and `--no-enable-prefix-caching`; *Prefill* and *Decode* keep default CUDA graph
capture enabled. For a step-by-step live run and rendered `Pod` excerpts, see the
[EPD Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/encode-prefill-decode/index.md).

## Admission constraints

An *EPD* deployment must satisfy the rules below. Admission enforces every one except the encoder
image floor, which is a requirement on the image you name.

- An *Encode* role requires a *Prefill* role and a *Decode* role in the same deployment, at most one
  role of each kind, and no *Server* role.
- *`spec.router`* is required. Without a router, nothing pairs the three roles.
- *`spec.kvCache`* is refused, because the encoder keeps no KV cache.
- A role *`command`* is refused on all three roles, because a custom command replaces the managed
  arguments and the sidecar proxy.
- All three roles must run on the same accelerator platform. Admission reads it from each role's
  `InstanceType`, so mixed or unknown platforms are refused.
- The image an *Encode* deployment runs must carry an engine build that meets the floor of the
  matching row in [the admitted matrix](#admitted-matrix). This is a requirement on the image
  you name, not an admission rule: no check reads a version at apply time.
- An *Encode* replica has size 1. The role refuses *elastic expert parallelism (Elastic EP)* and *external data parallelism (External DP)*.

## Admitted matrix

Admission accepts only the combinations below. Anything else is refused when the object is applied.

| Router | Engine | Platform | Encoder transfer | Minimum version | Hardware status |
|---|---|---|---|---|---|
| `llm-d-router` | `vLLM` | `nvidia` | `ECCPUConnector` over NIXL | `0.30.0` | Verified on 4 × NVIDIA L40S |
| `llm-d-router` | `vLLM` | `amd` (ROCm) | `ECCPUConnector` over NIXL | `0.30.0` | Not verified |
| `llm-d-router` | `vLLM` | `ascend` | `ECExampleConnector` over a shared volume | `0.30.0rc1` | Verified on Ascend 910B2 |
| `vllm-router` | `vLLM` | `nvidia` | `ECCPUConnector` over NIXL | `0.30.0` | Verified on 4 × NVIDIA L40S |
| `vllm-router` | `vLLM` | `amd` (ROCm) | `ECCPUConnector` over NIXL | `0.30.0` | Not verified |
| `llm-d-router` | `SGLang` | `nvidia` | `encoder-urls` | `0.5.10` | Verified on 2 × NVIDIA L40S |
| `llm-d-router` | `SGLang` | `amd` (ROCm) | `encoder-urls` | `0.5.10` | Not verified |
| `llm-d-router` | `SGLang` | `ascend` | `encoder-urls` | `0.5.10` | Not verified |
| `sglang-gateway` | `SGLang` | `nvidia` | `encoder-urls` | `0.5.10` | Verified on 2 × NVIDIA L40S |
| `sglang-gateway` | `SGLang` | `amd` (ROCm) | `encoder-urls` | `0.5.10` | Not verified |
| `sglang-gateway` | `SGLang` | `ascend` | `encoder-urls` | `0.5.10` | Not verified |

A verified row served a multimodal request through the router on the hardware named. A row marked
Not verified is admitted from the design and has not run on that hardware. Issue
[#763](https://github.com/gpustack/gpustack-operator/issues/763) tracks the rows still unrun: the AMD ROCm rows and the SGLang Ascend rows.
The `Minimum version` column is a requirement on the image you name, not an admission check:
nothing reads a version, so an image below it is applied as written.

### Version and configuration limits

The floor applies to the engine image named by *`spec.engine.image`* or *`roles[].image`*; the
[admitted matrix](#admitted-matrix) records the requirement and its admission boundary.

For the `llm-d-router` token-estimation path, see [Counting engine tokens](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode/index.md#counting-engine-tokens).

`vllm-router` receives the encoder addresses through its rendered `--epd-config`, and the SGLang
rows use their own encoder arguments. Do not copy the *prefill/decode (PD)* vLLM flag requirement into an *EPD*
deployment; that requirement is documented under [Counting engine tokens](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode/index.md#counting-engine-tokens).

See [Prefill and Decode](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode/index.md#version-and-configuration-limits) for the
*`resources.accelerator`* and engine-parallelism rule. The [admission constraints](#admission-constraints)
list the *Encode* role's own limits.

Use [vLLM on NVIDIA, AMD](#vllm-on-nvidia-amd), [vLLM on Ascend](#vllm-on-ascend) and
[SGLang](#sglang) for engine-specific encoder arguments and image floors. [Encoder-cache connectors](#encoder-cache-connectors)
describes generated flags. [Keys the operator owns](#keys-the-operator-owns) describes refused and
overrideable arguments.

### vLLM on NVIDIA, AMD

These rows run `ECCPUConnector` over NIXL behind either `llm-d-router` or `vllm-router`, moving
embeddings across the network with no shared volume. The NVIDIA rows were verified with the
connector live on both halves.
See [Encoder-cache connectors](#encoder-cache-connectors) for the buffer and port rules.

### vLLM on Ascend

The Ascend row uses `ECExampleConnector` over a shared directory, because Ascend network transfers
have no NIXL copy path. Its floor, `0.30.0rc1`, is the first vllm-ascend release that starts
`--mm-encoder-only` cleanly and pins the encoder-cache load to each rank's own NPU under *tensor parallelism (TP)*.

Runner tags carrying that release exist for 910B — `cann9.1-910b-vllm0.30.0` is published and
reachable. A variant with no published tag for it names its own image in *`roles[].image`*.

### SGLang

SGLang splits the encoder by address and uses no encoder-cache connector. An *Encode* role runs
`--encoder-only`, and a *Prefill* role runs `--language-only` and dials the encoder through
`--encoder-urls`, which carries the encoder's per-role `Service` address.

The prefiller serves the encoder's registration port 8997. Admission reserves that port on a
*Prefill* role whenever the deployment declares an *Encode* role, so a role naming it for its own
HTTP server is refused. The operator also widens the prefiller's probes, so it is not restarted
while it waits for an encoder to register.

On the SGLang rows (`llm-d-router` and `sglang-gateway`), *`status.endpoint`* — the router `Service` — is
the only supported client front door.

The plain deployment `Service` (`<name>`) selects the first non-*Encode* role (*Prefill* under the
ordinary *Encode*, *Prefill*, *Decode* ordering) and provides no router orchestration. Under the
tested SGLang `0.5.18` *EPD* ordering, *Prefill* runs `--language-only` and expects a `bootstrap_room`
prepared by the router, so a multimodal request sent directly to the plain `Service` fails with
`HTTP 400` (`Invalid bootstrap_room`) before reaching *Encode*.

### Unadmitted candidates

These candidates are refused or not configured:

| Candidate | Status |
|---|---|
| `ECMooncakeConnector` | Refused. It needs static prefiller addresses, which do not fit Kubernetes discovery, and the *decode* sidecar supports only `ec-example` and `ec-nixl` |
| `ECMemcacheConnector` | Not configured by the operator |
| `ECCPUConnector` on Ascend | Refused. NPU memory has no copy path to host memory, and the Ascend runner images install no NIXL |
| `vllm-router` on Ascend | Refused. vLLM-Ascend's *PD* KV transfer requires the `nixlv2` handshake that only `llm-d-router`'s *decode* sidecar implements |

## Encoder-cache connectors

The encoder-cache connector decides how embeddings travel from the encoder to the prefiller. The
operator renders two of them.

### ECExampleConnector

`ECExampleConnector` passes embeddings through a shared directory. Attach a ReadWriteMany PVC to
*`roles[].additionalVolumes`* on both roles, with the same *`claimName`*, *`mountPath`* and *`subPath`*.
The *Encode* mount must not be read-only.

The connector has no file locking and no expiry, and storage latency can delay a transfer.
The operator does not set `VLLM_USE_V2_MODEL_RUNNER` on this row, because the vllm-ascend second
model runner has no multimodal encoder path.

### ECCPUConnector

`ECCPUConnector` moves embeddings between `Pod`s over NIXL. The operator sets
`VLLM_USE_V2_MODEL_RUNNER=1` on both roles and renders a 1 GiB host buffer (`ec_cpu_bytes`) in
`--ec-transfer-config`. No field changes the buffer size. The value is upstream's example default
and has not been tuned here.

The *Encode* `Pod` binds a side-channel socket on port 5601 and publishes it through
`VLLM_EC_SIDE_CHANNEL_HOST` from *`status.podIP`*. An *Encode* role that names 5601, in *`ports`* or as
`--port`, is refused. The shared-directory row binds no socket, so its encoder may serve on 5601.

### Keys the operator owns

Every key the encoder stage writes is operator-owned, and a role that names one is refused in every
spelling the engine reads. On vLLM that is `--ec-transfer-config`; `ECCPUConnector` also owns
`VLLM_USE_V2_MODEL_RUNNER` and the two side-channel variables.

SGLang owns `--encoder-only`, `--language-only`, `--encoder-urls` and
`--encoder-bootstrap-port`. See [Operator-owned keys](/gpustack-operator/main/docs/modules/model-deployment/deployment/index.md#operator-owned-keys).

Four flags the operator renders on the encoder are defaults, so a role's own value wins:
`--enforce-eager`, `--no-enable-prefix-caching`, `--max-num-batched-tokens` and `--mm-encoder-only`.

`--enforce-eager` is defaulted on the vLLM *Encode* role only — never on *Prefill*, *Decode* or
*Server*. An *Encode* instance runs `--mm-encoder-only`: it executes the multimodal vision tower,
allocates no KV cache (`num_gpu_blocks = 0`) and runs no language-model decoder loop.

In upstream `vllm@v0.30.0`, vision-encoder CUDA graphs (`cudagraph_mm_encoder`) are disabled by
default, and the V2 `MMEncoderModelRunner` behind `ECCPUConnector` captures zero graphs regardless.
On the V1 `GPUModelRunner` behind `ECExampleConnector`, omitting `--enforce-eager` makes startup walk
empty language-model graph capture sizes, which is why upstream's disaggregated encoder guide marks
the flag as required on encoder instances.

Because the flag is registered in `DefaultedArgs` rather than `OwnedKeys`, a workload can still
override it — or tune `--compilation-config` — if a future engine version enables vision-encoder
graph capture. Conversely, *Prefill* and *Decode* run the language model and keep default CUDA graph
capture enabled: do not copy `--enforce-eager` onto those roles in production or benchmarks.

## Scheduling co-location

A deployment with an *Encode* role renders a soft co-location preference on every replica `Pod`. Each
term scores placement by node hostname and none is a requirement, so a cluster too tight to
co-locate a pair still schedules it across hosts.

| Replica | Prefers on the same node | Weight |
|---|---|---|
| *Prefill* | *Decode* | 100 |
| *Decode* | *Prefill* | 100 |
| *Encode* | *Prefill* | 50 |
| *Prefill* | *Encode* | 50 |

*Prefill* and *decode* exchange KV cache on every generated token, so that pair gains the most from one
host. The encoder's output reaches the prefiller once per request. Each term matches the peer role's
`modeldeployment.gpustack.ai/role-kind` label within the same deployment. A deployment with no
*Encode* role renders no affinity.

## Request routing across roles

How the encoder is reached depends on the engine and router. Under `llm-d-router` with `vLLM`, the
router selects an encoder for any request carrying media, and the *decode* sidecar coordinates the
transfer. Under `vllm-router` with `vLLM`, the router drives the *encode*, *prefill* and *decode* stages
directly through `--epd-config`. Under `SGLang`, the *Prefill* role calls the *Encode* role
directly, and the router adds no *encode* stage.

*Encode* `Pod`s never receive deployment-wide traffic. The deployment `Service` and *`status.endpoint`*
exclude them, and so do the KV event publisher lists. Send requests to *`status.endpoint`*. See
[Encode stage routing](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md#encode-stage-routing) for each router's behavior.

## Replica drain and retirement

An encoder exports no in-flight request gauges, so the normal drain hook cannot tell when it is
idle. Retirement therefore protects an *Encode* replica with a fixed window.

The controller withdraws the replica from discovery and marks it `Draining`. It then waits a
240-second drain window, plus a 30-second withdrawal and a 30-second settle, before deleting the `Pod`.
A transfer still in flight when the window ends fails at the client.

## Upgrade and rollout behavior

A deployment without an *Encode* role renders the same `Pod` specifications as before, so upgrading
the operator rolls nothing on existing server or *prefill/decode* deployments.

Role kinds cannot change after creation. To move an existing deployment to *EPD*, create a new
`ModelDeployment`.

## Results

A live run on 8 × NVIDIA H100 80GB HBM3 compared *EPD* with *prefill/decode* and with a single
aggregated instance across TCP and 3.2 Tbps InfiniBand *RDMA* paths. Under uniform one-request and
eight-request cells, the 1-GPU aggregated baseline delivers the lowest latency and highest per-GPU
throughput, while *EPD* isolates vision encoding onto its own stage so heavy multimodal bursts can
scale encoder, prefill, and decode capacities independently.

[Encode, Prefill and Decode Benchmark](/gpustack-operator/main/docs/modules/model-deployment/encode-prefill-decode-benchmark/index.md) records the numbers, the
hardware and when *EPD* helps.

---

**See also** — [Model Deployment Configuration](/gpustack-operator/main/docs/modules/model-deployment/deployment/index.md) (base fields and admission rules) · [Prefill and Decode](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode/index.md) (pairing *prefill* and *decode* roles) · [Encode, Prefill and Decode Benchmark](/gpustack-operator/main/docs/modules/model-deployment/encode-prefill-decode-benchmark/index.md) (measured latency and throughput across three configurations) · [Engine Compatibility](/gpustack-operator/main/docs/modules/model-deployment/engine-versions/index.md) (supported version floors) · [Model Deployment Routing](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md) (router selection and deciders) · [Model Deployment Status](/gpustack-operator/main/docs/modules/model-deployment/status/index.md) (status conditions and role summaries) · [EPD Walkthrough](/gpustack-operator/main/docs/walkthroughs/model-deployment/encode-prefill-decode/index.md) (a recorded live run).

**Next** → [Model Deployment Routing](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md) — choose a routing policy and inspect candidate endpoints.
