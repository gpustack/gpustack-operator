# Model Deployment Encoder Prefill Decode

A deployment with `Encode`, `Prefill` and `Decode` roles runs visual encoding and language generation in separate
Pods. [Model Deployment Configuration](deployment.md) describes the base role fields and admission rules.

## Contents

- [The three roles and request flow](#the-three-roles-and-request-flow)
- [Shape rules and admission constraints](#shape-rules-and-admission-constraints)
- [The admitted matrix](#the-admitted-matrix)
- [Unadmitted engine and router candidates](#unadmitted-engine-and-router-candidates)
- [Encoder-cache connectors](#encoder-cache-connectors)
- [Request routing across roles](#request-routing-across-roles)
- [Replica drain and retirement](#replica-drain-and-retirement)
- [Upgrade and rollout behavior](#upgrade-and-rollout-behavior)

## The three roles and request flow

Encoder-prefill-decode (EPD) serving separates visual encoding from language generation.
Each role runs in its own Pod set.

The `Encode` role processes visual inputs such as images.
It allocates no KV cache and drops language-model weight shards during model loading.

The `Prefill` role receives prompt tokens and consumes the encoder's output embeddings.
It populates the KV cache with the combined sequence.

The `Decode` role generates completion tokens step by step from the populated KV cache.

A multimodal request routes through the encoder, then the prefiller, and then the decoder.
A text-only request skips the encoder entirely and goes directly to prefill and decode.

## Shape rules and admission constraints

An `Encode` role requires both a `Prefill` role and a `Decode` role in the same deployment.
The operator admits at most one role of each kind.

`Encode` beside a `Server` role is refused.
The encoder produces embeddings only a prefiller can consume, and prefill output needs a decoder.

A router (`spec.router`) is required.
Without a router, nothing pairs the three roles, and the operator renders no unpaired shape.

`spec.kvCache` is refused on an EPD deployment.
The field attaches every role to a shared pool, but the encoder keeps no KV cache.

A role `command` is refused on all three roles.
A custom command replaces managed arguments and the sidecar proxy, breaking role communication.

All three roles must run on the same accelerator platform.
Admission reads each role's platform from its `instanceType.status.detail.manufacturer`.
Mixed or unknown platforms are refused.

`engine.version` is required on an EPD deployment and must satisfy the row floor.
`Encode` replicas have size 1.
The role refuses Elastic EP and external data parallelism.

## The admitted matrix

Admission allows only combinations present in the internal EPD matrix.
Unsupported combinations are rejected at admission time.

| Router | Engine | Platform | Connector | Engine floor | Shared volume | Verification status | Limits |
|---|---|---|---|---|---|---|---|
| `llm-d-router` | `vLLM` | `ascend` | `ECExampleConnector` | `0.30.0rc1` | yes | Admitted by code, unverified | Needs a ReadWriteMany PVC; mounts must match; writable mount; no file locks or TTL; storage latency can delay transfers; the second model runner must stay off |
| `llm-d-router` | `vLLM` | `amd` (ROCm) | `ECCPUConnector` | `0.30.0` | no | Admitted by code, unverified | Requires NIXL and the V2 model runner; the encoder's side channel binds 5601, which the role may not serve HTTP on; `ec_cpu_bytes` is fixed at 1 GiB; no NIXL copy path on Ascend |

The verification status and known limits sit beside each admitted row.
Both rows are admitted by code analysis and await live accelerator verification.

The `ascend` row requires `ECExampleConnector` over a shared directory.
Ascend network transfers do not support NIXL copy paths.

Its floor is the first vllm-ascend release carrying both fixes the row needs.
Upstream pull request 16682 stops `--mm-encoder-only` from aborting the engine at startup.
Upstream pull request 18291 pins the encoder cache load to the rank's own NPU under tensor parallelism.
Release `0.30.0rc2` carries the same code as `0.30.0rc1`.

The runner image the floor asks for may not be published.
An Ascend EPD deployment names its own `roles[].image` until it is.

`ECCPUConnector` stays refused on Ascend.
It has no copy path between NPU memory and host memory.
The Ascend runner images install no NIXL.
Upstream request 13057 asks for that support and is still open.

Ascend's own encoder-cache connectors are not admitted.
`ECMemcacheConnector` shipped in `0.30.0rc1`, and the operator does not configure it.
`ECMooncakeConnector` is still an open pull request upstream.

The `amd` row runs `ECCPUConnector` over NIXL.
The connector transfers embeddings across the network and needs no shared volume.

## Unadmitted engine and router candidates

The matrix currently contains no NVIDIA row.
Upstream labels EPD under `llm-d-router` a proof of concept.
NVIDIA combinations are admitted only after a live cluster run passes.

`ECMooncakeConnector` is refused.
The decode routing sidecar supports `ec-example` and `ec-nixl` only.
Upstream `ECMooncakeConnector` requires static prefiller addresses that do not fit Kubernetes discovery.

`vllm-router` is not admitted for EPD.
Upstream merged EPD support in pull request 266, but has not tagged a release containing it.
Dynamic prefill and decode discovery is unsupported upstream with EPD.

`SGLang` is not admitted for EPD.
Its encoder stage is carried over addresses rather than a connector, and no matrix row names one, so admission refuses the combination.

The renderer still carries that stage, so an object stored before the refusal reconciles into the divided shape rather than failing.
It gives an `Encode` role `--encoder-only` and the `Prefill` role `--language-only`.

The language-only half serves the encoder bootstrap registry that encoders register against, and it reads the current encoder URL list from there.
It renders that port as `--encoder-bootstrap-port 8997` and opens the same number as the container port `enc-bootstrap`.
`--encoder-urls` then carries the encoder's per-role Service address, `http://<deployment>-<role>:<port>`.

## Encoder-cache connectors

The operator renders configuration for two encoder-cache connectors.
Each connector dictates how embeddings travel from the encoder to the prefiller.

`ECExampleConnector` passes safetensors embeddings through a shared directory.
Users provide the directory by attaching a ReadWriteMany PVC to `roles[].additionalVolumes`.

`Encode` and `Prefill` must declare the same `claimName`, `mountPath` and `subPath`.
The `Encode` mount must not be read-only.
The connector has no locking or expiry, and filesystem visibility delays can affect transfers.

`ECCPUConnector` moves embeddings across Pods over the network using NIXL.
The operator sets `VLLM_USE_V2_MODEL_RUNNER=1` on both roles.
The shared-directory row never sets it, because vllm-ascend's second model runner has no multimodal encoder path.
An encoder running on it serves nothing, and no log names the cause.

The `Encode` Pod binds its side-channel socket on port 5601 and publishes it as `VLLM_EC_SIDE_CHANNEL_HOST` from `status.podIP`.
An `Encode` role is refused if it names 5601, either in `ports` or as `--port`.
The engine would otherwise serve HTTP on the socket its consumers dial for embeddings.
The shared-directory row binds no second socket, so its encoder may serve on 5601.

The connector also needs a host buffer, `ec_cpu_bytes` in the rendered `--ec-transfer-config`.
The operator states 1 GiB on both roles, and no field lets a deployment state its own.
A shape needing a different buffer has nowhere to say so.
That number is the one upstream's own example ships, not one measured here.

Every key the encoder stage writes is operator-owned, so a role naming one is refused in every spelling the engine reads.
See [Operator-owned keys](deployment.md#operator-owned-keys) for the rule behind that refusal.
On this row the owned keys are `--ec-transfer-config`, `VLLM_USE_V2_MODEL_RUNNER` and the encoder's two side-channel variables.

Four flags the operator renders on the encoder are defaults rather than owned, so a workload's own value wins.
They are `--enforce-eager`, `--no-enable-prefix-caching`, `--max-num-batched-tokens` and `--mm-encoder-only`.

## Request routing across roles

Under `llm-d-router`, requests land on the decode Pod's routing sidecar proxy first.
The sidecar routes through an endpoint picker running `always-disagg-multimodal-decider`.

The decider detects multimodal content in the request.
When media is present, the decider selects an encoder from the `encode-pods` label filter pool.

The encode scheduling profile uses the role filter alone and applies no scorers.
Scorers are omitted because an encoder holds no KV cache and tracks no token queues.

The decode sidecar runs with `--ec-connector` set from the admitted row.
The `ascend` row gives `--ec-connector=ec-example` and the `amd` row `--ec-connector=ec-nixl`.
It calls the encoder and coordinates the transfer before forwarding to prefill and decode.

`Encode` Pods are excluded from deployment-wide answering endpoints and KV event publisher lists.
The deployment-wide Service and `status.endpoint` never target an encoder.

## Replica drain and retirement

The preStop drain hook is unchanged for `Encode` replicas.
The hook reads language-model request accounting gauges that an encoder does not export.

The hook treats absent gauges as idle and exits after two consecutive idle polls.
It does not protect in-flight transfers on an encoder replica.

Retirement protects an `Encode` replica through a bounded time window.
The controller withdraws the replica from discovery and marks it `Draining`.

The controller waits for the drain budget (240 seconds) to expire before deleting the Pod.
A transfer still in flight when the budget expires fails visibly at the client.

## Upgrade and rollout behavior

A deployment without an `Encode` role renders byte-identical Pod specifications.
Upgrading the operator causes no rollout on existing server or prefill/decode deployments.

Moving an existing deployment to EPD requires creating a new `ModelDeployment`.
Role kinds are immutable after creation and cannot be changed in place.

---

**See also** — [Model Deployment Configuration](deployment.md) (base fields and admission rules) · [Model Deployment Prefill and Decode](prefill-decode.md) (pairing prefill and decode roles) · [Engine Versions](engine-versions.md) (supported version floors) · [Model Deployment Routing](routing.md) (router selection and deciders) · [Model Deployment Status](status.md) (status conditions and role summaries).

**Next** → [Model Deployment Routing](routing.md) — choose a routing policy and inspect candidate endpoints.
