# Engine Compatibility

Check engine and transport compatibility before choosing images for a deployment.
The requirements cover minimum releases, Mooncake client and store versions, supported transports and known failures.

**Engines below the listed minimum versions are unsupported.** Upgrade before diagnosing failures
on an older engine release.

## Contents

- [Minimum versions](#minimum-versions)
- [Reading the table](#reading-the-table)
- [Transport compatibility](#transport-compatibility)
- [Limitations](#limitations)

## Minimum versions

The table covers aggregated serving, *prefill/decode (PD)* and *encode/prefill/decode (EPD)* configurations.

| Engine | Configuration | Minimum | Mooncake client | Store members |
|---|---|---|---|---|
| vLLM | alone | `0.29.0`; behind `llm-d-router`, `--enable-scale-out` is also required on `0.30.0`+ | `0.3.13.post1`, on the `cuda12.9` and the `cuda13.0` tag | — |
| vLLM | *prefill/decode*, direct transfer over `tcp` | `0.29.0`; behind `llm-d-router`, `--enable-scale-out` is also required on `0.30.0`+ | `0.3.13.post1` | — |
| vLLM | with a Mooncake store, alone or as a pair | `0.29.0`; behind `llm-d-router`, `--enable-scale-out` is also required on `0.30.0`+ | `0.3.13.post1` | the 0.3.13 line; the [default image](/gpustack-operator/v0.9.2/docs/reference/settings/index.md) is one |
| vLLM-Ascend | alone | `0.23.0`; behind `llm-d-router`, `--enable-scale-out` is also required on `0.30.0rc1`+ | `0.3.11.post1`, the NPU build | — |
| vLLM-Ascend | *prefill/decode*, direct transfer | `0.23.0`, behind `llm-d-router`; `--enable-scale-out` is also required on `0.30.0rc1`+ | `0.3.11.post1`, the NPU build | — |
| vLLM-Ascend | with a Mooncake store | not verified | `0.3.11.post1`, the NPU build | not verified |
| SGLang | alone | `0.5.18`, run as the halves of a pair | `0.3.12.post1` | — |
| SGLang | *prefill/decode*, direct transfer over `tcp` | `0.5.18` | `0.3.12.post1` | — |
| SGLang | with a Mooncake store | `0.5.18`, run as a pair | `0.3.12.post1` | the 0.3.12 line, such as `kvcacheai/mooncake:0.3.12.post1` |
| vLLM | *encode/prefill/decode*, direct transfer over NIXL (`ECCPUConnector`) | `0.30.0`, behind `llm-d-router` or `vllm-router`: NVIDIA CUDA (verified on 4 × NVIDIA L40S) or AMD ROCm (unverified on hardware) | compiles its own, not run | — |
| vLLM-Ascend | *encode/prefill/decode*, direct transfer (`ECExampleConnector`) | `0.30.0rc1`, behind `llm-d-router` (verified on Ascend 910B2) | `0.3.11.post1`, the NPU build | — |
| SGLang | *encode/prefill/decode*, transfer by address (`encoder-urls`) | `0.5.10`, behind `llm-d-router` or `sglang-gateway` (NVIDIA verified on 2 × NVIDIA L40S) | `0.3.12.post1` | — |

A version counts as run when the configuration answered requests and, where it moves KV, the engine or the
store reported blocks moving: a transfer or a store write, not a `Pod` reaching `Ready`. The
vLLM-Ascend rows ran on the runner's `cann9.1-910b-vllm0.23.0` tag, named through
*`roles[].image`*.

The NVIDIA vLLM *EPD* rows (behind `llm-d-router` and `vllm-router`) and the NVIDIA SGLang *EPD* rows
(run on `0.5.18`, behind both routers) ran live. The vLLM-Ascend *EPD* row behind `llm-d-router` ran live on Ascend 910B2. The AMD *EPD* rows and the
SGLang Ascend rows are admitted but not verified on hardware
([#763](https://github.com/gpustack/gpustack-operator/issues/763) tracks them).

## Reading the table

**The minimum is a property of the image you name.** There is no *`engine.version`* field to set:
the image a role runs resolves to *`roles[].image`* when the role names one, and to
*`spec.engine.image`* otherwise ([Role images](/gpustack-operator/v0.9.2/docs/modules/model-deployment/deployment/index.md#role-images)).
Nothing assembles a tag and no admission check reads a version, so meeting a minimum here means
naming an image whose engine build carries it.

**A deployment without an *Encode* role needs `--enable-scale-out` behind `llm-d-router`.**
The router counts prompt tokens through the engine's render endpoint, which vLLM serves only
under that flag, from `0.30.0` on; the gate measured on vllm-ascend is `0.30.0rc1`. Name an
image at those versions and set the flag on each role
([Counting engine tokens](/gpustack-operator/v0.9.2/docs/modules/model-deployment/prefill-decode/index.md#counting-engine-tokens)).

The `0.29.0` and `0.23.0` minimums above predate the flag: they were measured as transfer-path
runs, before the operator rendered this dependency.

The floor belongs to the engine, so an SGLang *EPD*
row states `SGLang 0.5.10` on every platform, while the vLLM Ascend row states a vllm-ascend
release.

**The client comes with the image.** The column above is read off the
published runner images, and the vLLM rows are the CUDA ones; a ROCm runner image compiles its own
client and has not been run. A role naming its own *`roles[].image`* carries whatever client that
image embeds: read it off the image, then pick the store from it.

**A store runs on the client's minor line.** The store column is a requirement rather than a
suggestion; see [Version compatibility](/gpustack-operator/v0.9.2/docs/modules/kv-cache/backend/index.md#version-compatibility).

**vLLM-Ascend's direct transfer does not support direct `tcp` transfer**, and one router renders it; see [Pair connections](/gpustack-operator/v0.9.2/docs/modules/model-deployment/prefill-decode/index.md#pair-connections).

**The configurations above move KV over `tcp`, or over `ascend` on vLLM-Ascend.** Which other transport
each engine can use is [the matrix below](#transport-compatibility).

**"Not verified" means no run, not a known failure.** Nothing refuses such a configuration; nothing here
says it works.

**An *EPD* encoder image must meet its row's floor, and nothing refuses a lower one.** The
requirement: the image an *Encode* deployment runs must carry vllm-ascend `0.30.0rc1` or later for
Ascend `ECExampleConnector`, and vLLM `0.30.0` or later for NVIDIA/AMD `ECCPUConnector`
([the admitted matrix](/gpustack-operator/v0.9.2/docs/modules/model-deployment/encode-prefill-decode/index.md#admitted-matrix)). Admission does not check it — the version
field and its gate are gone.

An image below the floor fails at container start with an `ImagePullBackOff` on a tag that does not
exist, or an engine that exits on an unknown argument.

**The group-forward probe has a verified version set, and nothing bars an unverified one.** The
requirement: the version whose probe binding was read is `0.25.1`/`0.29.0` for vLLM (the response
id is `cmpl-` + the request's `request_id`) and `0.5.18`/`0.5.21` for SGLang (the response
`meta_info.id` is the request's rid).

On any other version the probe still runs and its outcome still decides: `Verified` admits a
member, `Unknown` grants nothing new and withdraws nothing already granted. Only a failed health
check of the same [qualification](/gpustack-operator/v0.9.2/docs/modules/model-deployment/status/index.md) withdraws an eligible member. Neither probe outcome is an
admission refusal.

## Transport compatibility

A **store** cell is the pool's `KVCacheBackend.spec.transport.protocol`, which a member group can
override and the engine is handed. A **direct** cell is *`spec.kvTransfer.protocol`* on a
*prefill/decode* pair. The first column gives the API and Mooncake spellings. `Auto` renders
`tcp` on either path; the direct path also renders `tcp` when the field is unset.

| Transport (API / Mooncake) | Engine | Store path | Direct path |
|---|---|---|---|
| `Auto`, `TCP` / `tcp` | vLLM | **Works**, default images; beside a direct path it keeps its store connections open too and did not [run out of ports](#limitations) where measured | **Works**, default images; it keeps its connections open and did not [run out of ports](#limitations) where measured |
| `TCP` / `tcp` | SGLang | **Works**, with a published store image on [its client's line](#minimum-versions), named by hand; beside a direct path, [it runs out of ports](#limitations) unless [`model-deployment-tcp-tw-reuse`](/gpustack-operator/v0.9.2/docs/reference/settings/index.md#sglang-time-wait-reuse) is on | **Works**, default images; under sustained load [it runs out of ports](#limitations), and [`model-deployment-tcp-tw-reuse`](/gpustack-operator/v0.9.2/docs/reference/settings/index.md#sglang-time-wait-reuse) is the remedy, run so far only beside a store |
| `TCP` / `tcp` | vLLM-Ascend | **Not supported**: refused at admission, its store client accepts `CANN` only | **Not supported**: the value is ignored, the path runs `ascend` |
| `RDMA` / `rdma` | vLLM | **Not verified** | **Works**, default images on InfiniBand H100 SXM5 ([Prefill and Decode Benchmark](/gpustack-operator/v0.9.2/docs/modules/model-deployment/prefill-decode-benchmark/index.md#infiniband-rdma-comparison), [Encode, Prefill and Decode Benchmark](/gpustack-operator/v0.9.2/docs/modules/model-deployment/encode-prefill-decode-benchmark/index.md#infiniband-rdma-comparison)) |
| `RDMA` / `rdma` | SGLang | **Not verified** | **Not supported**: the operator grants SGLang's direct path no device |
| `RDMA` / `rdma` | vLLM-Ascend | **Not supported**: refused at admission, as for `TCP` | **Not supported**: ignored, as for `tcp` |
| `EFA` / `efa` | vLLM | **Own image**: an [EFA build of Mooncake](/gpustack-operator/v0.9.2/docs/modules/rdma/operations/index.md#efa-engine-images) in the engine; default store image | **Own image**: [the same build](/gpustack-operator/v0.9.2/docs/modules/rdma/operations/index.md#efa-engine-images) on both ends |
| `EFA` / `efa` | SGLang | **Not supported**: [no EFA build serves SGLang](/gpustack-operator/v0.9.2/docs/modules/rdma/operations/index.md#sglang-and-efa) | **Not supported**: the operator grants SGLang's direct path no device |
| `EFA` / `efa` | vLLM-Ascend | **Not supported**: refused at admission, as for `TCP` | **Not supported**: ignored, as for `tcp` |
| `CANN` / `ascend` | vLLM | **Not supported**: the CUDA client has no Ascend transport | **Not supported**: the CUDA client has no Ascend transport |
| `CANN` / `ascend` | SGLang | **Not verified** | **Not verified** |
| `CANN` / `ascend` | vLLM-Ascend | **Not verified**; the members need [an image carrying CANN](/gpustack-operator/v0.9.2/docs/modules/kv-cache/backend/index.md#image) | **Works**, `cann9.1-910b-vllm0.23.0` named by hand in *`roles[].image`*, behind `llm-d-router` |
| `ROCM` / `hip` | vLLM, SGLang | **Not verified** | **Not verified** |
| `ROCM` / `hip` | vLLM-Ascend | **Not supported**: refused at admission, as for `TCP` | **Not supported**: ignored, as for `tcp` |
| `MUSA`, `MACA` / `musa`, `maca` | vLLM, SGLang, vLLM-Ascend | **Not verified** | **Rejected by the direct-path schema**: these are same-node IPC transports, while *prefill* and *decode* have no same-node placement guarantee |

The vLLM and SGLang verdicts are read on their CUDA runner images. What each verdict means:

- **Works**: run on this operator with blocks moving, by the criterion under [the
  minimum](#minimum-versions), on the images the cell names.
- **Own image**: works only on an engine image you build and keep up to date yourself.
- **Not supported**: admission refuses it, the operator renders nothing for it, or no image the
  cell could run carries the transport; the cell says which.
- **Not verified**: no run. Nothing refuses it; nothing here says it works.

**"Default images" needs no image work from you**: a stock runner image the project publishes,
named in *`engine.image`* or *`roles[].image`*, and the
[`kv-cache-backend-image`](/gpustack-operator/v0.9.2/docs/reference/settings/index.md) default for the store members. "Named by
hand" is a published image you write into *`roles[].image`* or the backend's *`spec.image`*.

**A cell is one path.** A deployment using both paths is a separate configuration, read from [the
minimum](#minimum-versions) rather than added up from two cells; both paths over EFA at once
have not been run. With a positive *`resources.interface`*, an `RDMA` path beside an `EFA` one is
refused at admission.

On AWS the `RDMA` rows do not apply; [choose `EFA` or
`TCP`](../rdma/operations.md#rdma-on-aws).

## Limitations

- **SGLang with a store** holds a pinned host pool and needs the node's available memory above a
  fixed reserve plus that pool; see [SGLang's host-memory
  tier](../kv-cache/injection.md#sglang-host-memory-tier).
- **SGLang *prefill/decode* over `TCP` runs out of local ports under sustained load, with or without
  a store.** The *prefill* half opens a new connection for every transfer, to the *decode* half and to
  each store member, all from the same ephemeral ports of its container. Connections left in
  `TIME-WAIT` use them up; from then on every request through that pair fails and keeps failing
  until the engine `Pod`s are restarted. On the measured pair without a store, the gateway then
  answered `503` with `No available prefill workers`.

  **Turning on [`model-deployment-tcp-tw-reuse`](/gpustack-operator/v0.9.2/docs/reference/settings/index.md#sglang-time-wait-reuse)
  prevented failure in a measured SGLang 0.5.18 pair with a store**: it answered 1905 sequential
  requests without failure, with about 19,500 `TIME-WAIT` sockets. It needs a kubelet change on
  every eligible node.

  The pair without a store was not tested with this setting. Widening `net.ipv4.ip_local_port_range`
  only delays exhaustion; whether a fabric transport avoids it remains unverified.

  **vLLM did not show this behavior in the measured runs**: its *prefill* half keeps its transfer
  connections open instead of opening one per transfer, and held at most nine sockets in
  `TIME-WAIT` over a direct path with no store and at most sixteen with a `TCP` store beside it.

---

**See also** — [Model Deployment Configuration](/gpustack-operator/v0.9.2/docs/modules/model-deployment/deployment/index.md) (the fields these versions go into) ·
[Prefill and Decode](/gpustack-operator/v0.9.2/docs/modules/model-deployment/prefill-decode/index.md) (pairing *prefill* and *decode* roles) ·
[Encode, Prefill and Decode](/gpustack-operator/v0.9.2/docs/modules/model-deployment/encode-prefill-decode/index.md) (*encode*, *prefill* and *decode* disaggregation) ·
[KV Cache Backend](/gpustack-operator/v0.9.2/docs/modules/kv-cache/backend/index.md) (the store a client needs) ·
[RDMA Operations](/gpustack-operator/v0.9.2/docs/modules/rdma/operations/index.md) (fabric devices and the engine image they need).

**Next** → [Model Deployment Status](/gpustack-operator/v0.9.2/docs/modules/model-deployment/status/index.md)
