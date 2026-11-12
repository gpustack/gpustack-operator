# Engine Compatibility

Check engine and transport compatibility before choosing images for a deployment.
The requirements cover minimum releases, Mooncake client and store versions, supported transports and known failures.

**Engines below the listed minimum versions are unsupported.** Upgrade before diagnosing failures
on an older engine release.

## Contents

- [Minimum versions](#minimum-versions)
- [Reading the table](#reading-the-table)
- [Transport compatibility](#transport-compatibility)
- [Remote weight loading](#remote-weight-loading)
- [Limitations](#limitations)

## Minimum versions

The table covers aggregated serving, *prefill/decode (PD)* and *encode/prefill/decode (EPD)* configurations.

| Engine | Configuration | Minimum | Mooncake client | Store members |
|---|---|---|---|---|
| vLLM | alone | `0.29.0`; behind `llm-d-router`, `--enable-scale-out` is also required on `0.30.0`+ | `0.3.13.post1`, on the `cuda12.9` and the `cuda13.0` tag | — |
| vLLM | *prefill/decode*, direct transfer over `tcp` | `0.29.0`; behind `llm-d-router`, `--enable-scale-out` is also required on `0.30.0`+ | `0.3.13.post1` | — |
| vLLM | with a Mooncake store, alone or as a pair | `0.29.0`; behind `llm-d-router`, `--enable-scale-out` is also required on `0.30.0`+ | `0.3.13.post1` | the 0.3.13 line; the [default image](../../reference/settings.md) is one |
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
*`spec.engine.image`* otherwise ([Role images](configuration.md#role-images)).
Nothing assembles a tag and no admission check reads a version, so meeting a minimum here means
naming an image whose engine build carries it.

**A deployment without an *Encode* role needs `--enable-scale-out` behind `llm-d-router`.**
The router counts prompt tokens through the engine's render endpoint, which vLLM serves only
under that flag, from `0.30.0` on; the gate measured on vllm-ascend is `0.30.0rc1`. Name an
image at those versions and set the flag on each role
([Counting engine tokens](prefill-decode.md#counting-engine-tokens)).

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
suggestion; see [Version compatibility](../kv-cache/backend.md#version-compatibility).

**vLLM-Ascend's direct transfer does not support direct `tcp` transfer**; see [Pair connections](prefill-decode.md#pair-connections).

**The configurations above move KV over `tcp`, or over `ascend` on vLLM-Ascend.** Which other transport
each engine can use is [the matrix below](#transport-compatibility).

**"Not verified" means no run, not a known failure.** Nothing refuses such a configuration; nothing here
says it works.

**An *EPD* encoder image must meet its row's floor, and nothing refuses a lower one.** The
requirement: the image an *Encode* deployment runs must carry vllm-ascend `0.30.0rc1` or later for
Ascend `ECExampleConnector`, and vLLM `0.30.0` or later for NVIDIA/AMD `ECCPUConnector`
([the admitted matrix](encode-prefill-decode.md#admitted-matrix)). Admission does not check it — the version
field and its gate are gone.

An image below the floor fails at container start with an `ImagePullBackOff` on a tag that does not
exist, or an engine that exits on an unknown argument.

**The group-forward probe has a verified version set, and nothing bars an unverified one.** The
requirement: the version whose probe binding was read is `0.25.1`/`0.29.0` for vLLM (the response
id is `cmpl-` + the request's `request_id`) and `0.5.18`/`0.5.21` for SGLang (the response
`meta_info.id` is the request's rid).

On any other version the probe still runs and its outcome still decides: `Verified` admits a
member, `Unknown` grants nothing new and withdraws nothing already granted. Only a failed health
check of the same [qualification](status.md) withdraws an eligible member. Neither probe outcome is an
admission refusal.

## Transport compatibility

The store path uses `KVCacheBackend.spec.transport.protocol`. The direct prefill/decode path
selects `Mooncake`, `NIXL`, or `HIXL` through `spec.kvTransfer.backend`; see
[Transfer parameters](parameterize.md#transfer-parameters) for fabric selection.

The store compatibility observations below apply to the named images.

| Transport (API/Mooncake) | Engine | Store path |
|---|---|---|
| `TCP` / `tcp` | vLLM | **Works**, default images; beside a direct path it keeps its store connections open too and did not [run out of ports](#limitations) where measured |
| `TCP` / `tcp` | SGLang | **Works**, with a published store image on [its client's line](#minimum-versions), named by hand; beside a direct path, [it runs out of ports](#limitations) unless [`model-deployment-tcp-tw-reuse`](../../reference/settings.md#sglang-time-wait-reuse) is on |
| `TCP` / `tcp` | vLLM-Ascend | **Not supported**: refused at admission, its store client accepts `CANN` only |
| `RDMA` / `rdma` | vLLM | **Not verified** |
| `RDMA` / `rdma` | SGLang | **Not verified** |
| `RDMA` / `rdma` | vLLM-Ascend | **Not supported**: refused at admission, as for `TCP` |
| `EFA` / `efa` | vLLM | **Own image**: an [EFA build of Mooncake](../rdma/operations.md#efa-engine-images) in the engine; default store image |
| `EFA` / `efa` | SGLang | **Not supported**: [no EFA build serves SGLang](../rdma/operations.md#sglang-and-efa) |
| `EFA` / `efa` | vLLM-Ascend | **Not supported**: refused at admission, as for `TCP` |
| `CANN` / `ascend` | vLLM | **Not supported**: the CUDA client has no Ascend transport |
| `CANN` / `ascend` | SGLang | **Not verified** |
| `CANN` / `ascend` | vLLM-Ascend | **Not verified**; the members need [an image carrying CANN](../kv-cache/backend.md#image) |
| `ROCM` / `hip` | vLLM, SGLang | **Not verified** |
| `ROCM` / `hip` | vLLM-Ascend | **Not supported**: refused at admission, as for `TCP` |
| `MUSA`, `MACA` / `musa`, `maca` | vLLM, SGLang, vLLM-Ascend | **Not verified** |


The following direct-path observations apply to the named configurations, not every backend or image:

- Mooncake TCP worked with the default vLLM and SGLang images. SGLang under sustained load can
  [run out of ports](#limitations); `model-deployment-tcp-tw-reuse` is the documented remedy.
- Mooncake RDMA worked with vLLM on InfiniBand H100 SXM5 in the
  [Prefill and Decode Benchmark](prefill-decode-benchmark.md#infiniband-rdma-comparison) and
  [Encode, Prefill and Decode Benchmark](encode-prefill-decode-benchmark.md#infiniband-rdma-comparison).
- EFA requires an [EFA engine build](../rdma/operations.md#efa-engine-images). The default store
  image carries EFA. The combined store and direct paths over EFA have not been run here.
- The recorded vLLM-Ascend direct path used `cann9.1-910b-vllm0.23.0` behind `llm-d-router`.
  Other direct backend and fabric combinations require separate validation.
- A store on Ascend requires CANN; its members need an [image carrying CANN](../kv-cache/backend.md#image).

When a store and direct path request incompatible fabrics, admission refuses the deployment.

## Remote weight loading

The `--load-format model-transfer` value selects an engine's model-transfer adapter, the loader that reads weights from
a prepared source instead of a local checkpoint. The engine keeps its own parameter mapping,
sharding and finalization; the adapter supplies verified tensors and proves its finalization
before the engine profiles KV memory. Configuration and behavior live in
[Remote Weight Delivery](../model-delivery/remote.md); the table gives the state per engine:

| Engine | Adapter entry | Qualification state |
| --- | --- | --- |
| vLLM | `NativeModelTransferLoader`, `--load-format model-transfer` on CUDA | H100 loading and inference observed for full-resident and paged 80B sources and full-resident 235B broadcast; production qualification remains open |
| vLLM-Ascend | `NativeModelTransferLoader` through the vLLM general plugin, with `vllm_ascend` (requires CANN 9.2+ runner image with Python `hixl`, such as `cann9.2-910b-vllm0.30.0`) | loading and inference measured on two Ascend 910B2 nodes for Qwen3-8B, Qwen3.6-35B-A3B and Qwen3-235B-A22B-W8A8; managed HIXL routing and production qualification remain closed |
| SGLang | `NativeSGLangModelTransferLoader`, `--load-format model-transfer` via `.pth` CLI and `LoadFormat` enum hooks (requires CANN 9.2+ runner image on Ascend for Python `hixl`) | H100 80B loading and inference observed; Ascend evidence covers CLI, `LoadFormat`, probe and session-open, with full loading qualification still open |

Recorded loading and inference establish only the listed configuration. Each engine's hardware
and safety gates must pass on its own platform before production qualification; a CUDA result
qualifies no Ascend or ROCm row. An unqualified combination is
refused or falls back by the delivery policy: under `Require`, a role whose engine and pool name
no qualified backend is blocked rather than rewritten.

The adapter uses vLLM's general load-format plugin for vLLM and vLLM-Ascend. SGLang uses `.pth` CLI
and `LoadFormat` extension hooks.

### Ascend (HIXL)

The [Ascend benchmark](../model-delivery/remote.md#ascend-hixl) compares same-node and
cross-node loading. On Ascend (`hixl`), CANN 9.2 introduces the Python `hixl` package
(`/usr/local/Ascend/cann-9.2.0/python/site-packages/hixl`) alongside `libcann_hixl.so`. CANN 9.0
images install only `llm_datadist` and C++ `libcann_hixl.so`, so `load_runtime()` fails closed with
`RuntimeUnavailableError` when a CANN 9.0 runner image is used.

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

  **Turning on [`model-deployment-tcp-tw-reuse`](../../reference/settings.md#sglang-time-wait-reuse)
  prevented failure in a measured SGLang 0.5.18 pair with a store**: it answered 1905 sequential
  requests without failure, with about 19,500 `TIME-WAIT` sockets. It needs a kubelet change on
  every eligible node.

  The pair without a store was not tested with this setting. Widening `net.ipv4.ip_local_port_range`
  only delays exhaustion; whether a fabric transport avoids it remains unverified.

  **vLLM did not show this behavior in the measured runs**: its *prefill* half keeps its transfer
  connections open instead of opening one per transfer, and held at most nine sockets in
  `TIME-WAIT` over a direct path with no store and at most sixteen with a `TCP` store beside it.

---

**See also** — [Model Deployment Configuration](configuration.md) (the fields these versions go into) ·
[Prefill and Decode](prefill-decode.md) (pairing *prefill* and *decode* roles) ·
[Encode, Prefill and Decode](encode-prefill-decode.md) (*encode*, *prefill* and *decode* disaggregation) ·
[KV Cache Backend](../kv-cache/backend.md) (the store a client needs) ·
[RDMA Operations](../rdma/operations.md) (fabric devices and the engine image they need).

**Next** → [Model Deployment Status](status.md)
