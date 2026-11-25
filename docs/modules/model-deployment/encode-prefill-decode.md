# Encode, Prefill and Decode

An *encode/prefill/decode (EPD)* deployment separates visual encoding from language generation across
three roles: multimodal requests traverse *Encode* → *Prefill* → *Decode* (*Encode* allocates no KV
cache and loads no language weights), while text-only requests skip *Encode*. See
[Model Deployment Configuration](configuration.md) for base role fields and admission rules.

![Multimodal and text-only request flows](../../assets/diagrams/model-deployment-epd.svg)

<!--
flowchart LR
    CLIENT["Client requests"] -> ROUTER["llm-d-router and decode sidecar"]
    ROUTER -> ENCODE["Encode: multimodal only"] -> CACHE["Encoder cache connector"] -> PREFILL["Prefill"]
    ROUTER -> PREFILL
    PREFILL -> DECODE["Decode: KV state"] -> CLIENT
-->

## Contents

- [Configuration](#configuration)
- [Supported serving configurations](#supported-serving-configurations)
- [Encoder-cache connectors](#encoder-cache-connectors)
- [Operations](#operations)
- [Results](#results)

## Configuration

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
    name: vLLM  # enum: vLLM, SGLang
    image: gpustack/runner:cuda13.0-vllm0.30.0  # must meet the admitted matrix version floor
  router:
    name: llm-d-router  # required to pair Encode, Prefill and Decode
  roles:
    - name: encode
      kind: Encode
      replicas: 1
      acceleratorType: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
      resources:
        accelerator: "1"  # defaults to 1 on an accelerated AcceleratorType
    - name: prefill
      kind: Prefill
      replicas: 1
      acceleratorType: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
      resources:
        accelerator: "1"
    - name: decode
      kind: Decode
      replicas: 1
      acceleratorType: gpustack--nvidia-h100-80gb-hbm3-linux-amd64
      resources:
        accelerator: "1"
```

The operator injects the encoder-cache and KV-transfer connectors and attaches the decode routing
sidecar where needed. On vLLM *Encode*, the operator defaults `--mm-encoder-only`,
`--enforce-eager` and `--no-enable-prefix-caching`; *Prefill* and *Decode* keep CUDA graphs
enabled. See [EPD Walkthrough](../../walkthroughs/model-deployment/encode-prefill-decode.md) for
deployment steps and multimodal checks.

### Admission constraints

Admission enforces every rule below except the image version floor:

- **Role triad**: An *Encode* role requires one *Prefill* role, one *Decode* role, at most one role
  per kind, and no *Server* role.
- **Router required**: Set *`spec.router`*; omitting it fails admission.
- **No shared KV pool**: *`spec.kvCache`* is refused because *Encode* keeps no KV cache.
- **Managed roles only**: *`roles[].command`* is refused on all three roles.
- **Uniform platform**: All three roles must resolve to the same accelerator manufacturer from
  their `AcceleratorType`; mixed or unknown platforms are refused.
- **Single-`Pod` encoder**: *Encode* requires `size: 1` and refuses *elastic expert parallelism
  (Elastic EP)* and *external data parallelism (External DP)*.
- **Image version floor**: Pick an image meeting the matching row in
  [Supported serving configurations](#supported-serving-configurations); admission does not read image tags.

## Supported serving configurations

Admission accepts only these combinations:

| Router | Engine | Platform | Encoder transfer | Minimum version | Hardware status |
|---|---|---|---|---|---|
| `llm-d-router` | `vLLM` | `nvidia` | `ECCPUConnector` over NIXL | `0.30.0` | Verified on 4 × NVIDIA L40S |
| `llm-d-router` | `vLLM` | `amd` (ROCm) | `ECCPUConnector` over NIXL | `0.30.0` | Not verified |
| `llm-d-router` | `vLLM` | `ascend` | `ECExampleConnector` over shared volume | `0.30.0rc1` | Verified on Ascend 910B2 |
| `vllm-router` | `vLLM` | `nvidia` | `ECCPUConnector` over NIXL | `0.30.0` | Verified on 4 × NVIDIA L40S |
| `vllm-router` | `vLLM` | `amd` (ROCm) | `ECCPUConnector` over NIXL | `0.30.0` | Not verified |
| `llm-d-router` | `SGLang` | `nvidia` | `encoder-urls` | `0.5.10` | Verified on 2 × NVIDIA L40S |
| `llm-d-router` | `SGLang` | `amd` (ROCm) | `encoder-urls` | `0.5.10` | Not verified |
| `llm-d-router` | `SGLang` | `ascend` | `encoder-urls` | `0.5.10` | Not verified |
| `sglang-gateway` | `SGLang` | `nvidia` | `encoder-urls` | `0.5.10` | Verified on 2 × NVIDIA L40S |
| `sglang-gateway` | `SGLang` | `amd` (ROCm) | `encoder-urls` | `0.5.10` | Not verified |
| `sglang-gateway` | `SGLang` | `ascend` | `encoder-urls` | `0.5.10` | Not verified |

Verified rows served multimodal requests through the router on the listed hardware. Unverified rows
(AMD ROCm and SGLang Ascend) are tracked in
[#763](https://github.com/gpustack/gpustack-operator/issues/763).

### Version and configuration limits

Apply the image floor to *`spec.engine.image`* or *`roles[].image`*. Unlike text-only *prefill/decode
(PD)*, `llm-d-router` uses its built-in token estimator for *EPD*, so do not add
`--enable-scale-out` ([Router token counting](prefill-decode.md#router-token-counting)). See
[Router compatibility](prefill-decode.md#router-compatibility) for
explicit parallelism flag rules.

### NVIDIA and AMD vLLM

Runs `ECCPUConnector` over NIXL behind `llm-d-router` or `vllm-router` with no shared volume. See
[NIXL embedding transfer](#nixl-embedding-transfer) for buffer and port rules.

### vLLM on Ascend

Uses `ECExampleConnector` over a shared volume because Ascend lacks a NIXL host-copy path.
vLLM-Ascend `0.30.0rc1` is the floor for clean `--mm-encoder-only` startup and per-rank NPU cache
pinning under *tensor parallelism (TP)*. Published 910B tag: `cann9.1-910b-vllm0.30.0`; set
*`roles[].image`* for other Ascend variants.

### SGLang

SGLang uses address-based encoding without an encoder-cache connector. *Encode* runs
`--encoder-only`; *Prefill* runs `--language-only` and dials `--encoder-urls` (the *Encode*
`Service`), listening on registration port `8997` (reserved by admission on *Prefill*) with widened
startup probes.

Send traffic only to *`status.endpoint`* (the router `Service`). The plain deployment `Service`
(`<name>`) hits *Prefill* directly without a router `bootstrap_room`, returning `HTTP 400`
(`Invalid bootstrap_room`) on SGLang `0.5.18`.

### Unsupported configurations

| Candidate | Status |
|---|---|
| `ECMooncakeConnector` | Refused: requires static prefiller addresses and unsupported by decode sidecar |
| `ECMemcacheConnector` | Not configured by operator |
| `ECCPUConnector` on Ascend | Refused: no NPU-to-host NIXL copy path or NIXL package |
| `vllm-router` on Ascend | Refused: vLLM-Ascend requires `llm-d-router`'s `nixlv2` decode sidecar |

## Encoder-cache connectors

### Shared volume transfer

`ECExampleConnector` passes embeddings through a shared `ReadWriteMany` PVC attached via
*`roles[].additionalVolumes`* on both *Encode* and *Prefill* with matching *`claimName`*,
*`mountPath`* and *`subPath`*. Keep the *Encode* mount writable (`readOnly: false`).

The connector has no file locking or expiry, and storage latency adds transfer delay. The operator
omits `VLLM_USE_V2_MODEL_RUNNER` because vLLM-Ascend V2 has no multimodal encoder path.

### NIXL embedding transfer

`ECCPUConnector` transfers embeddings between `Pod`s over NIXL. The operator defaults
`VLLM_USE_V2_MODEL_RUNNER=1` on both roles for `vLLM < 0.34.0` (omitted at `>= 0.34.0`, where the
variable was removed upstream) and renders an untuned 1 GiB host buffer (`ec_cpu_bytes`) in
`--ec-transfer-config`.

*Encode* sets `VLLM_EC_SIDE_CHANNEL_HOST` to *`status.podIP`* and defaults
`VLLM_EC_SIDE_CHANNEL_PORT=5601` (overridable in *`roles[].env`*, including `0` for an ephemeral
port). Admission checks HTTP port conflicts on *Encode* (`ports` or `--port`) against the effective
side-channel port (skipping the check when `0`); `ECExampleConnector` reserves no side-channel port.

### Operator-owned keys

Admission refuses operator-owned encoder keys in any spelling the engine accepts (see
[Operator-owned keys](configuration.md#operator-owned-keys)):

- **vLLM**: `--ec-transfer-config`, plus `VLLM_EC_SIDE_CHANNEL_HOST` under `ECCPUConnector`
  (`VLLM_USE_V2_MODEL_RUNNER` and `VLLM_EC_SIDE_CHANNEL_PORT` are defaulted, not owned).
- **SGLang**: `--encoder-only`, `--language-only`, `--encoder-urls` and `--encoder-bootstrap-port`.

Four vLLM *Encode* flags are **defaulted** (user *`extraArgs`* wins): `--enforce-eager`,
`--no-enable-prefix-caching`, `--max-num-batched-tokens` and `--mm-encoder-only`. `--enforce-eager`
avoids empty language-model graph capture on V1 `GPUModelRunner` (`ECExampleConnector`) and is
never defaulted on *Prefill*, *Decode* or *Server*; do not copy it onto language roles.

## Operations

### Role placement preferences

Deployments with an *Encode* role add soft hostname anti-spread preferences (matching
`modeldeployment.gpustack.ai/role-kind` within the deployment); tight clusters still schedule
across nodes:

| Replica | Co-location target | Weight |
|---|---|---:|
| *Prefill* | *Decode* | 100 |
| *Decode* | *Prefill* | 100 |
| *Encode* | *Prefill* | 50 |
| *Prefill* | *Encode* | 50 |

*Prefill* and *Decode* exchange KV cache on every request and get higher weight than *Encode* →
*Prefill* embeddings. Deployments without *Encode* render no affinity.

### Request routing across roles

Send client traffic to *`status.endpoint`*. *Encode* `Pod`s are excluded from the deployment
`Service`, *`status.endpoint`* and KV-event discovery:

- **`llm-d-router` + vLLM**: Selects an encoder for media requests; the decode sidecar coordinates
  transfer.
- **`vllm-router` + vLLM**: Drives *Encode*, *Prefill* and *Decode* directly via `--epd-config`.
- **SGLang**: *Prefill* calls *Encode* directly; the router adds no *Encode* stage.

See [Encode stage routing](routing.md#encode-stage-routing) for full router behavior.

### Replica drain and retirement

*Encode* exports no in-flight request gauges, so retirement uses a fixed window: `30 s` withdrawal,
`240 s` drain (`Draining`) and `30 s` settle before `Pod` deletion. Transfers still in flight when
the window ends fail at the client.

### Deployment upgrades

Upgrading the operator does not roll existing *Server* or *PD* deployments without an *Encode* role.
Role kinds are frozen after creation; create a new `ModelDeployment` to adopt *EPD*.

## Results

An 8 × NVIDIA H100 80 GB HBM3 capture compared 1-GPU aggregated serving, 2-GPU *PD* and 3-GPU *EPD*
over TCP and ConnectX-7 InfiniBand *RDMA*. Under small uniform prompts, 1-GPU aggregated serving
kept the lowest first-token latency and highest per-GPU efficiency, while *EPD* isolated heavy
vision encoding and cut long-prompt *ITL* tails. See
[Encode, Prefill and Decode Benchmark](encode-prefill-decode-benchmark.md).

---

**See also** — [Model Deployment Configuration](configuration.md) (base fields and admission rules) ·
[Prefill and Decode](prefill-decode.md) (pairing *prefill* and *decode* roles) ·
[Encode, Prefill and Decode Benchmark](encode-prefill-decode-benchmark.md) (measured latency and throughput) ·
[Engine Compatibility](engine-versions.md) (supported version floors) ·
[Model Deployment Routing](routing.md) (router selection and endpoint eligibility) ·
[EPD Walkthrough](../../walkthroughs/model-deployment/encode-prefill-decode.md) (deployment and multimodal checks)

**Next** → [Model Deployment Routing](routing.md) — choose a routing policy and inspect candidate endpoints.
