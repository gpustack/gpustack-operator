# Model Deployment Encode, Prefill and Decode

A deployment with `Encode`, `Prefill` and `Decode` roles runs visual encoding and language generation
in separate Pods. [Model Deployment Configuration](deployment.md) describes the base role fields and
admission rules.

## Contents

- [The three roles and request flow](#the-three-roles-and-request-flow)
- [Shape rules and admission constraints](#shape-rules-and-admission-constraints)
- [The admitted matrix](#the-admitted-matrix)
- [Unadmitted engine and router candidates](#unadmitted-engine-and-router-candidates)
- [Encoder-cache connectors](#encoder-cache-connectors)
- [Scheduling co-location](#scheduling-co-location)
- [Request routing across roles](#request-routing-across-roles)
- [Replica drain and retirement](#replica-drain-and-retirement)
- [Upgrade and rollout behavior](#upgrade-and-rollout-behavior)
- [Measured behavior](#measured-behavior)

## The three roles and request flow

Encode-prefill-decode (EPD) serving separates visual encoding from language generation. Each role
runs in its own Pod set.

| Role | Does | Notes |
|---|---|---|
| `Encode` | Runs the visual encoder on images and other media | Allocates no KV cache and loads no language-model weights |
| `Prefill` | Consumes the encoder's embeddings and fills the KV cache | Receives the prompt tokens |
| `Decode` | Generates completion tokens from the KV cache | Streams the response |

A multimodal request goes through the encoder, then the prefiller, then the decoder. A text-only
request skips the encoder and goes straight to prefill and decode.

## Shape rules and admission constraints

Admission refuses an EPD deployment that breaks any of these rules:

- An `Encode` role requires a `Prefill` role and a `Decode` role in the same deployment, at most one
  role of each kind, and no `Server` role.
- `spec.router` is required. Without a router, nothing pairs the three roles.
- `spec.kvCache` is refused, because the encoder keeps no KV cache.
- A role `command` is refused on all three roles, because a custom command replaces the managed
  arguments and the sidecar proxy.
- All three roles must run on the same accelerator platform. Admission reads it from each role's
  InstanceType, so mixed or unknown platforms are refused.
- `engine.version` is required and must meet the floor of the matching row in
  [the admitted matrix](#the-admitted-matrix).
- An `Encode` replica has size 1. The role refuses Elastic EP and external data parallelism.

## The admitted matrix

Admission accepts only the combinations below. Anything else is refused when the object is applied.

| Router | Engine | Platform | Encoder transfer | Minimum version | Hardware status |
|---|---|---|---|---|---|
| `llm-d-router` | `vLLM` | `nvidia` | `ECCPUConnector` over NIXL | `0.30.0` | Verified on 4 × NVIDIA L40S |
| `llm-d-router` | `vLLM` | `amd` (ROCm) | `ECCPUConnector` over NIXL | `0.30.0` | Not verified |
| `llm-d-router` | `vLLM` | `ascend` | `ECExampleConnector` over a shared volume | `0.30.0rc1` | Not verified |
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
[#763](https://github.com/gpustack/gpustack-operator/issues/763) tracks the SGLang AMD and Ascend rows.

### vLLM on NVIDIA and AMD

These rows run `ECCPUConnector` over NIXL behind either `llm-d-router` or `vllm-router`, moving
embeddings across the network with no shared volume. The NVIDIA rows were verified with the
connector live on both halves.
See [Encoder-cache connectors](#encoder-cache-connectors) for the buffer and port rules.

### vLLM on Ascend

The Ascend row uses `ECExampleConnector` over a shared directory, because Ascend network transfers
have no NIXL copy path. Its floor, `0.30.0rc1`, is the first vllm-ascend release that starts
`--mm-encoder-only` cleanly and pins the encoder-cache load to each rank's own NPU under tensor
parallelism.

The runner image that release needs may not be published yet. Until it is, name your own image in
`roles[].image`.

### SGLang

SGLang splits the encoder by address and uses no encoder-cache connector. An `Encode` role runs
`--encoder-only`, and a `Prefill` role runs `--language-only` and dials the encoder through
`--encoder-urls`, which carries the encoder's per-role Service address.

The prefiller serves the encoder's registration port 8997. Admission reserves that port on a
`Prefill` role whenever the deployment declares an `Encode` role, so a role naming it for its own
HTTP server is refused. The operator also widens the prefiller's probes, so it is not restarted
while it waits for an encoder to register.

## Unadmitted engine and router candidates

These candidates are refused or not configured:

| Candidate | Status |
|---|---|
| `ECMooncakeConnector` | Refused. It needs static prefiller addresses, which do not fit Kubernetes discovery, and the decode sidecar supports only `ec-example` and `ec-nixl` |
| `ECMemcacheConnector` | Not configured by the operator |
| `ECCPUConnector` on Ascend | Refused. NPU memory has no copy path to host memory, and the Ascend runner images install no NIXL |
| `vllm-router` on Ascend | Refused. vLLM-Ascend's P/D KV transfer requires the `nixlv2` handshake that only `llm-d-router`'s decode sidecar implements |

## Encoder-cache connectors

The encoder-cache connector decides how embeddings travel from the encoder to the prefiller. The
operator renders two of them.

### ECExampleConnector

`ECExampleConnector` passes embeddings through a shared directory. Attach a ReadWriteMany PVC to
`roles[].additionalVolumes` on both roles, with the same `claimName`, `mountPath` and `subPath`.
The `Encode` mount must not be read-only.

The connector has no file locking and no expiry, and storage latency can delay a transfer.
The operator does not set `VLLM_USE_V2_MODEL_RUNNER` on this row, because the vllm-ascend second
model runner has no multimodal encoder path.

### ECCPUConnector

`ECCPUConnector` moves embeddings between Pods over NIXL. The operator sets
`VLLM_USE_V2_MODEL_RUNNER=1` on both roles and renders a 1 GiB host buffer (`ec_cpu_bytes`) in
`--ec-transfer-config`. No field changes the buffer size. The value is upstream's example default
and has not been tuned here.

The `Encode` Pod binds a side-channel socket on port 5601 and publishes it through
`VLLM_EC_SIDE_CHANNEL_HOST` from `status.podIP`. An `Encode` role that names 5601, in `ports` or as
`--port`, is refused. The shared-directory row binds no socket, so its encoder may serve on 5601.

### Keys the operator owns

Every key the encoder stage writes is operator-owned, and a role that names one is refused in every
spelling the engine reads. On the `ECCPUConnector` rows these are `--ec-transfer-config`,
`VLLM_USE_V2_MODEL_RUNNER` and the two side-channel variables. See
[Operator-owned keys](deployment.md#operator-owned-keys).

Four flags the operator renders on the encoder are defaults, so a role's own value wins:
`--enforce-eager`, `--no-enable-prefix-caching`, `--max-num-batched-tokens` and `--mm-encoder-only`.

## Scheduling co-location

A deployment with an `Encode` role renders a soft co-location preference on every replica Pod. Each
term scores placement by node hostname and none is a requirement, so a cluster too tight to
co-locate a pair still schedules it across hosts.

| Replica | Prefers on the same node | Weight |
|---|---|---|
| `Prefill` | `Decode` | 100 |
| `Decode` | `Prefill` | 100 |
| `Encode` | `Prefill` | 50 |
| `Prefill` | `Encode` | 50 |

Prefill and decode exchange KV cache on every generated token, so that pair gains the most from one
host. The encoder's output reaches the prefiller once per request. Each term matches the peer role's
`modeldeployment.gpustack.ai/role-kind` label within the same deployment. A deployment with no
`Encode` role renders no affinity.

## Request routing across roles

How the encoder is reached depends on the engine and router. Under `llm-d-router` with `vLLM`, the
router selects an encoder for any request carrying media, and the decode sidecar coordinates the
transfer. Under `vllm-router` with `vLLM`, the router drives the encode, prefill and decode stages
directly through `--epd-config`. Under `SGLang`, the `Prefill` role calls the `Encode` role
directly, and the router adds no encode stage.

`Encode` Pods never receive deployment-wide traffic. The deployment Service and `status.endpoint`
exclude them, and so do the KV event publisher lists. Send requests to `status.endpoint`. See
[Encode stage routing](routing.md#encode-stage-routing) for each router's behavior.

## Replica drain and retirement

An encoder exports no in-flight request gauges, so the normal drain hook cannot tell when it is
idle. Retirement therefore protects an `Encode` replica with a fixed window.

The controller withdraws the replica from discovery and marks it `Draining`. It then waits a
240-second drain window, plus a 30-second withdrawal and a 30-second settle, before deleting the Pod.
A transfer still in flight when the window ends fails at the client.

## Upgrade and rollout behavior

A deployment without an `Encode` role renders the same Pod specifications as before, so upgrading
the operator rolls nothing on existing server or prefill/decode deployments.

Role kinds cannot change after creation. To move an existing deployment to EPD, create a new
`ModelDeployment`.

## Measured behavior

A live run on 4 × NVIDIA L40S compared EPD with prefill/decode and with a single aggregated
instance. The aggregated instance leads on short prompts and at low concurrency. EPD leads at `4k`
and `8k` prompt tokens under concurrency, and when the same image is queried repeatedly.

[Model Deployment Encode, Prefill and Decode Benchmark](epd-benchmark.md) records the numbers, the
hardware and when EPD helps.

---

**See also** — [Model Deployment Configuration](deployment.md) (base fields and admission rules) · [Model Deployment Prefill and Decode](prefill-decode.md) (pairing prefill and decode roles) · [Model Deployment Encode, Prefill and Decode Benchmark](epd-benchmark.md) (measured three-arm latency and throughput) · [Engine Versions](engine-versions.md) (supported version floors) · [Model Deployment Routing](routing.md) (router selection and deciders) · [Model Deployment Status](status.md) (status conditions and role summaries) · [EPD Walkthrough](../../walkthroughs/model-deployment/epd.md) (a recorded live run).

**Next** → [Model Deployment Routing](routing.md) — choose a routing policy and inspect candidate endpoints.
