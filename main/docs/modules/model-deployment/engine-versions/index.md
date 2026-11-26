# Engine Compatibility

Verify engine, Mooncake and transport compatibility before choosing container images.
**Engines below the listed minimums are unsupported.** Upgrade before diagnosing failures on an
older engine release.

## Contents

- [Minimum versions](#minimum-versions)
- [Engine version resolution](#engine-version-resolution)
- [Transport compatibility](#transport-compatibility)
- [Remote weight loading](#remote-weight-loading)
- [Limitations](#limitations)

## Minimum versions

The matrix covers aggregated serving, *prefill/decode (PD)* and *encode/prefill/decode (EPD)*
configurations.

| Engine | Configuration | Minimum | Mooncake client | Store members |
|---|---|---|---|---|
| vLLM | alone | `0.29.0`; behind `llm-d-router`, `--enable-scale-out` is also required on `0.30.0`+ | `0.3.13.post1`, on the `cuda12.9` and the `cuda13.0` tag | — |
| vLLM | *prefill/decode*, direct transfer over `tcp` | `0.29.0`; behind `llm-d-router`, `--enable-scale-out` is also required on `0.30.0`+ | `0.3.13.post1` | — |
| vLLM | with a Mooncake store, alone or as a pair | `0.29.0`; behind `llm-d-router`, `--enable-scale-out` is also required on `0.30.0`+ | `0.3.13.post1` | the 0.3.13 line; the [default store image](/gpustack-operator/main/docs/modules/kv-cache/backend/index.md#backend-images) is one |
| vLLM-Ascend | alone | `0.23.0`; behind `llm-d-router`, `--enable-scale-out` is also required on `0.30.0rc1`+ | `0.3.11.post1`, the NPU build | — |
| vLLM-Ascend | *prefill/decode*, direct transfer | `0.23.0`, behind `llm-d-router`; `--enable-scale-out` is also required on `0.30.0rc1`+ | `0.3.11.post1`, the NPU build | — |
| vLLM-Ascend | with a Mooncake store | not verified | `0.3.11.post1`, the NPU build | not verified |
| SGLang | alone | `0.5.18`, run as the halves of a pair | `0.3.12.post1` | — |
| SGLang | *prefill/decode*, direct transfer over `tcp` | `0.5.18` | `0.3.12.post1` | — |
| SGLang | with a Mooncake store | `0.5.18`, run as a pair | `0.3.12.post1` | the 0.3.12 line, such as `kvcacheai/mooncake:0.3.12.post1` |
| vLLM | *encode/prefill/decode*, direct transfer over NIXL (`ECCPUConnector`) | `0.30.0`, behind `llm-d-router` or `vllm-router`: NVIDIA CUDA (verified on 4 × NVIDIA L40S) or AMD ROCm (unverified on hardware) | compiles its own, not run | — |
| vLLM-Ascend | *encode/prefill/decode*, direct transfer (`ECExampleConnector`) | `0.30.0rc1`, behind `llm-d-router` (verified on Ascend 910B2) | `0.3.11.post1`, the NPU build | — |
| SGLang | *encode/prefill/decode*, transfer by address (`encoder-urls`) | `0.5.10`, behind `sgl-router`, `llm-d-router`, or deprecated `sglang-gateway` (NVIDIA verified on 2 × NVIDIA L40S) | `0.3.12.post1` | — |

A row counts as verified when the configuration served requests and reported KV block transfers or
store writes. The vLLM-Ascend rows ran on `cann9.1-910b-vllm0.23.0` via *`roles[].image`*. NVIDIA
vLLM *EPD*, NVIDIA SGLang *EPD* (run on `0.5.18`), and Ascend 910B2 vLLM-Ascend *EPD* ran live;
AMD *EPD* and SGLang Ascend rows are admitted but unverified on hardware
([#763](https://github.com/gpustack/gpustack-operator/issues/763)).

### Engine image requirements

- **Render flag**: Non-*Encode* vLLM deployments behind `llm-d-router` need `--enable-scale-out`
  starting at vLLM `0.30.0` or vLLM-Ascend `0.30.0rc1`
  ([Router token counting](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode/index.md#router-token-counting)). Earlier `0.29.0`/`0.23.0`
  floors predate that dependency.
- **Mooncake client and store**: Each runner image embeds its Mooncake client (ROCm images compile
  their own and are unverified). Match `KVCacheBackend` members to the client's minor line
  ([Version compatibility](/gpustack-operator/main/docs/modules/kv-cache/backend/index.md#version-compatibility)).
- **Direct transport defaults**: Listed runs moved KV over `tcp`, or `ascend` on vLLM-Ascend (which
  does not support direct `tcp`; see [Role links and connectors](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode/index.md#role-links-and-connectors)).
- **Not verified**: Means no live hardware run, not a refusal or known failure.
- **Encoder floor**: Pick an image meeting the [supported serving configurations](/gpustack-operator/main/docs/modules/model-deployment/encode-prefill-decode/index.md#supported-serving-configurations)
  floor (`0.30.0` for NVIDIA/AMD `ECCPUConnector`, `0.30.0rc1` for Ascend `ECExampleConnector`,
  `0.5.10` for SGLang); admission does not enforce it.
- **Group-forward probe**: Verified on vLLM `0.25.1`/`0.29.0` (`cmpl-` + `request_id`) and SGLang
  `0.5.18`/`0.5.21` (`meta_info.id` = rid). On other releases, the probe still runs: `Verified`
  admits a member, `Unknown` keeps prior eligibility, and only a failed health check withdraws it
  ([Endpoint status](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md#endpoint-status)).

## Engine version resolution

Managed roles (*`roles[].command`* empty) resolve an engine version from *`spec.roles[].engineVersion`*
or fall back to *`spec.engine.version`*. Admission backfills empty version fields automatically from
standard `gpustack/runner` image tags on both creation and update.

- **Custom or digest-only images**: Set *`spec.engine.version`* or *`roles[].engineVersion`*
  explicitly. Admission fails closed when a managed role uses an unparseable image without a version,
  including when a role overrides *`roles[].image`* with a different unparseable image while leaving
  *`roles[].engineVersion`* empty.
- **Runner tag agreement**: If you update a `gpustack/runner` image while leaving a stale explicit
  version, admission refuses the update. Update or clear the version field alongside the image.
- **Milestone comparison**: Gates compare `(major, minor, patch)` tuples, so pre-release tags such
  as `0.30.0rc1` or `0.34.0rc0` satisfy their release line (`0.30.0` or `0.34.0`).
- **Empty stored version baseline**: In controller rendering, an empty stored version falls back to
  the `< 0.30.0` baseline without re-parsing container image tags.

### Version gates

| Version gate | Engine | Before | At/after |
|---|---|---|---|
| `0.30.0` | `vLLM-Ascend` | *PD* renders `MooncakeConnectorV1` (`prefill tp_size >= decode tp_size`) | *PD* renders `MooncakeConnectorV2` Pull mode (`max(tp_p, tp_d) % min(tp_p, tp_d) == 0`); mixed V1/V2 pairs refused |
| `0.31.0` | `vLLM`, `vLLM-Ascend` | `vllm-router` `kv_aware` policy refused at admission | `vllm-router` `kv_aware` admitted; emits KV events and `sha256_cbor` hash alignment |
| `0.34.0` | `vLLM` | `ECCPUConnector` defaults `VLLM_USE_V2_MODEL_RUNNER=1` and `VLLM_EC_SIDE_CHANNEL_PORT=5601` | `VLLM_USE_V2_MODEL_RUNNER` omitted; `VLLM_EC_SIDE_CHANNEL_PORT=5601` remains defaulted (overridable, including `0`) |

## Transport compatibility

The shared-store path uses `KVCacheBackend.spec.transport.protocol`. The direct *PD* path selects
`Mooncake`, `NIXL` or `HIXL` through *`spec.kvTransfer.backend`*
([Direct KV transports](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode/index.md#direct-kv-transports)). Admission refuses
deployments whose store and direct paths request incompatible fabrics.

| Transport (API/Mooncake) | Engine | Store path |
|---|---|---|
| `TCP` / `tcp` | vLLM | **Works**, default images; beside a direct path it keeps its store connections open and did not [run out of ports](#limitations) where measured |
| `TCP` / `tcp` | SGLang | **Works**, with a store image on [its client's line](#minimum-versions); beside a direct path, [it runs out of ports](#limitations) unless [`model-deployment-tcp-tw-reuse`](#limitations) is on |
| `TCP` / `tcp` | vLLM-Ascend | **Not supported**: refused at admission; store client accepts `CANN` only |
| `RDMA` / `rdma` | vLLM | **Not verified** |
| `RDMA` / `rdma` | SGLang | **Not verified** |
| `RDMA` / `rdma` | vLLM-Ascend | **Not supported**: refused at admission, as for `TCP` |
| `EFA` / `efa` | vLLM | **Own image**: requires an [EFA build of Mooncake](/gpustack-operator/main/docs/modules/rdma/operations/index.md#efa-engine-images) in the engine; default store image |
| `EFA` / `efa` | SGLang | **Not supported**: [no EFA build serves SGLang](/gpustack-operator/main/docs/modules/rdma/operations/index.md#sglang-and-efa) |
| `EFA` / `efa` | vLLM-Ascend | **Not supported**: refused at admission, as for `TCP` |
| `CANN` / `ascend` | vLLM | **Not supported**: CUDA client has no Ascend transport |
| `CANN` / `ascend` | SGLang | **Not verified** |
| `CANN` / `ascend` | vLLM-Ascend | **Not verified**; members need [an image carrying CANN](/gpustack-operator/main/docs/modules/kv-cache/backend/index.md#backend-images) |
| `ROCM` / `hip` | vLLM, SGLang | **Not verified** |
| `ROCM` / `hip` | vLLM-Ascend | **Not supported**: refused at admission, as for `TCP` |
| `MUSA`, `MACA` / `musa`, `maca` | vLLM, SGLang, vLLM-Ascend | **Not verified** |

Direct-path observations apply only to the named configurations:

- **Mooncake TCP**: Verified on default vLLM and SGLang images. SGLang can
  [exhaust ephemeral ports](#limitations) unless `model-deployment-tcp-tw-reuse` is enabled.
- **Mooncake RDMA**: Verified with vLLM on InfiniBand H100 SXM5 in
  [Prefill and Decode Benchmark](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode-benchmark/index.md#infiniband-rdma-comparison) and
  [Encode, Prefill and Decode Benchmark](/gpustack-operator/main/docs/modules/model-deployment/encode-prefill-decode-benchmark/index.md#infiniband-rdma-comparison).
- **EFA**: Requires an [EFA engine build](/gpustack-operator/main/docs/modules/rdma/operations/index.md#efa-engine-images) (default store
  image includes EFA). Combined store and direct EFA paths are unverified.
- **vLLM-Ascend direct**: Verified on `cann9.1-910b-vllm0.23.0` behind `llm-d-router`. Stores on
  Ascend require CANN members ([image carrying CANN](/gpustack-operator/main/docs/modules/kv-cache/backend/index.md#backend-images)).

## Remote weight loading

`--load-format model-transfer` loads verified tensors from a prepared remote source while keeping
each engine's parameter mapping and sharding. vLLM and vLLM-Ascend use the vLLM general load-format
plugin; SGLang uses `.pth` CLI and `LoadFormat` hooks. See
[Remote Weight Delivery](/gpustack-operator/main/docs/modules/model-delivery/remote/index.md) for configuration.

| Engine | Adapter entry | Qualification state |
|---|---|---|
| vLLM | `NativeModelTransferLoader`, `--load-format model-transfer` on CUDA | H100 loading and inference observed for full-resident and paged 80B sources and full-resident 235B broadcast; production qualification remains open |
| vLLM-Ascend | `NativeModelTransferLoader` through vLLM general plugin with `vllm_ascend` (requires CANN 9.2+ image with Python `hixl`, e.g. `cann9.2-910b-vllm0.30.0`) | Loading and inference measured on two Ascend 910B2 nodes for Qwen3-8B, Qwen3.6-35B-A3B and Qwen3-235B-A22B-W8A8; managed HIXL admitted by default; production qualification remains open |
| SGLang | `NativeSGLangModelTransferLoader`, `--load-format model-transfer` via `.pth` CLI and `LoadFormat` hooks (requires CANN 9.2+ image on Ascend for Python `hixl`) | H100 80B loading and inference observed; Ascend covers CLI, `LoadFormat`, probe and session-open, with full loading qualification open |

A CUDA capture does not qualify Ascend or ROCm. Under delivery policy `Require`, an unqualified
engine and pool combination blocks instead of falling back.

### Ascend (HIXL)

See [Ascend benchmark](/gpustack-operator/main/docs/modules/model-delivery/remote/index.md#ascend-hixl) for same-node and cross-node load
times. CANN 9.2 adds Python `hixl` (`/usr/local/Ascend/cann-9.2.0/python/site-packages/hixl`)
beside `libcann_hixl.so`; CANN 9.0 images lack Python `hixl`, so `load_runtime()` fails closed with
`RuntimeUnavailableError`.

## Limitations

- **SGLang store RAM**: SGLang with a store enables `--enable-hierarchical-cache` (or
  `--disaggregation-decode-retraction-backup cpu_tensor` on *Decode*) and allocates pinned host
  RAM. Size node RAM above the engine heap plus that pool
  ([KV Cache Pools and Bindings](/gpustack-operator/main/docs/modules/kv-cache/pool/index.md)).
- **SGLang *PD* TCP port exhaustion**: SGLang *Prefill* opens a new TCP connection per transfer to
  *Decode* and each store member. Ephemeral sockets left in `TIME-WAIT` exhaust container ports,
  after which requests fail (`503` `No available prefill workers` without a store) until engine
  `Pod`s restart.
- **Mitigation (`model-deployment-tcp-tw-reuse`)**: Prevented exhaustion across 1,905 sequential
  requests on SGLang `0.5.18` with a store (~19,500 `TIME-WAIT` sockets). First add
  `allowedUnsafeSysctls: ["net.ipv4.tcp_tw_reuse"]` to `KubeletConfiguration` on every eligible
  node (otherwise *Prefill* `Pod`s fail with `SysctlForbidden`), then patch the `Setting`:

```bash
kubectl -n gpustack-system patch setting model-deployment-tcp-tw-reuse \
  --type merge -p '{"spec":{"value":"true"}}'
```

- **Untested boundaries**: The store-free SGLang pair was not tested with `tcp_tw_reuse`, widening
  `ip_local_port_range` only delays exhaustion, and fabric transport avoidance is unverified.
- **vLLM comparison**: vLLM reuses transfer connections and held at most 9 `TIME-WAIT` sockets
  without a store and at most 16 with a `TCP` store.

---

**See also** — [Model Deployment Configuration](/gpustack-operator/main/docs/modules/model-deployment/configuration/index.md) (fields that reference these images) ·
[Prefill and Decode](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode/index.md) (pairing *prefill* and *decode* roles) ·
[Encode, Prefill and Decode](/gpustack-operator/main/docs/modules/model-deployment/encode-prefill-decode/index.md) (*encode*, *prefill* and *decode* disaggregation) ·
[KV Cache Backend](/gpustack-operator/main/docs/modules/kv-cache/backend/index.md) (store version alignment) ·
[RDMA Operations](/gpustack-operator/main/docs/modules/rdma/operations/index.md) (fabric devices and EFA engine images)

**Next** → [Prefill and Decode](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode/index.md) — pair *prefill* and *decode* roles behind a router.
