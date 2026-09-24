# Engine Versions Reference

> **Purpose** — the lowest vLLM, vLLM-Ascend and SGLang release each deployment shape has been run
> with on this operator, with the Mooncake client it carries and the store it needs, and which
> transport each engine can use on each leg.
> **Audience** users, operators · **Prerequisites** [Model Deployment
> Reference](model-deployment.md) · **Read time** reference — look up your engine

**An engine below its minimum here is not supported.** Older releases fail in ways that belong to
those releases, and this documentation does not track them; upgrading is the fix for each of them.

## Contents

- [The minimum per shape](#the-minimum-per-shape)
- [Reading the table](#reading-the-table)
- [Which transport each engine can use](#which-transport-each-engine-can-use)
- [Known failures at the minimum](#known-failures-at-the-minimum)

## The minimum per shape

| Engine | Shape | Minimum | Mooncake client in the runner image | Store members |
|---|---|---|---|---|
| vLLM | alone | `0.29.0` | `0.3.13.post1`, on the `cuda12.9` and the `cuda13.0` tag | — |
| vLLM | prefill/decode, direct transfer over `tcp` | `0.29.0` | `0.3.13.post1` | — |
| vLLM | with a Mooncake store, alone or as a pair | `0.29.0` | `0.3.13.post1` | the 0.3.13 line; the [default image](../settings.md) is one |
| vLLM-Ascend | alone | `0.23.0` | `0.3.11.post1`, the NPU build | — |
| vLLM-Ascend | prefill/decode, direct transfer | `0.23.0`, behind `llm-d-router` | `0.3.11.post1`, the NPU build | — |
| vLLM-Ascend | with a Mooncake store | not verified | `0.3.11.post1`, the NPU build | not verified |
| SGLang | alone | `0.5.18`, run as the halves of a pair | `0.3.12.post1` | — |
| SGLang | prefill/decode, direct transfer over `tcp` | `0.5.18` | `0.3.12.post1` | — |
| SGLang | with a Mooncake store | `0.5.18`, run as a pair | `0.3.12.post1` | the 0.3.12 line, such as `kvcacheai/mooncake:0.3.12.post1` |

A version counts as run when the shape answered requests and, where it moves KV, the engine or the
store reported blocks moving — a transfer or a store write, not a Pod reaching `Ready`. The
vLLM-Ascend rows ran on the runner's `cann9.1-910b-vllm0.23.0-router` tag, named through
`roles[].image`.

## Reading the table

**`engine.version` has no default.** The operator assembles the runner image from it — see [The
runner image is a formula](model-deployment.md#the-runner-image-is-a-formula) — so the minimum is a
value you write, and nothing refuses a lower one.

**The client comes with the image, not with the version.** The column above is read off the
published runner images, and the vLLM rows are the CUDA ones; a ROCm runner image compiles its own
client and has not been run. A role naming its own `roles[].image` carries whatever client that
image embeds: read it off the image, then pick the store from it.

**A store runs on the client's minor line.** The store column is a requirement rather than a
suggestion — see [The store version must match the engine's
client](../kv-cache/backend.md#the-store-version-must-match-the-engines-client).

**vLLM-Ascend's direct transfer has no `tcp` shape**, and one router renders it — see [Prefill and
decode](model-deployment.md#prefill-and-decode).

**The shapes above move KV over `tcp`, or over `ascend` on vLLM-Ascend.** Which other transport
each engine can use is [the matrix below](#which-transport-each-engine-can-use).

**"Not verified" means no run, not a known failure.** Nothing refuses such a shape; nothing here
says it works.

## Which transport each engine can use

A **store** cell is the pool's `KVCacheBackend.spec.transport.protocol`, which a member group can
override and the engine is handed. A **direct** cell is `spec.kvTransfer.protocol` on a
prefill/decode pair. The first column gives both spellings; `Auto`, the store default, is `TCP`.

| Transport (store / direct) | Engine | Store leg | Direct leg |
|---|---|---|---|
| `TCP` / `tcp` | vLLM | **Works**, default images; beside a direct leg it keeps its store connections open too and did not [run out of ports](#known-failures-at-the-minimum) where measured | **Works**, default images; it keeps its connections open and did not [run out of ports](#known-failures-at-the-minimum) where measured |
| `TCP` / `tcp` | SGLang | **Works**, with a published store image on [its client's line](#the-minimum-per-shape), named by hand; beside a direct leg, [it runs out of ports](#known-failures-at-the-minimum) unless [`model-deployment-tcp-tw-reuse`](../settings.md#letting-sglang-prefill-pods-reuse-time-wait-ports) is on | **Works**, default images; under sustained load [it runs out of ports](#known-failures-at-the-minimum), and [`model-deployment-tcp-tw-reuse`](../settings.md#letting-sglang-prefill-pods-reuse-time-wait-ports) is the remedy, run so far only beside a store |
| `TCP` / `tcp` | vLLM-Ascend | **Not supported**: refused at admission, its store client accepts `CANN` only | **Not supported**: the value is ignored, the leg runs `ascend` |
| `RDMA` / `rdma` | vLLM | **Not verified** | **Not verified** |
| `RDMA` / `rdma` | SGLang | **Not verified** | **Not supported**: the operator grants SGLang's direct leg no device |
| `RDMA` / `rdma` | vLLM-Ascend | **Not supported**: refused at admission, as for `TCP` | **Not supported**: ignored, as for `tcp` |
| `EFA` / `efa` | vLLM | **Own image**: an [EFA build of Mooncake](../operation/rdma.md#what-an-efa-leg-needs-from-the-engine-image) in the engine; default store image | **Own image**: [the same build](../operation/rdma.md#what-an-efa-leg-needs-from-the-engine-image) on both ends |
| `EFA` / `efa` | SGLang | **Not supported**: [no EFA build serves SGLang](../operation/rdma.md#sglang-cannot-use-efa) | **Not supported**: the operator grants SGLang's direct leg no device |
| `EFA` / `efa` | vLLM-Ascend | **Not supported**: refused at admission, as for `TCP` | **Not supported**: ignored, as for `tcp` |
| `CANN` / `ascend` | vLLM | **Not supported**: the CUDA client has no Ascend transport | **Not supported**: the CUDA client has no Ascend transport |
| `CANN` / `ascend` | SGLang | **Not verified** | **Not verified** |
| `CANN` / `ascend` | vLLM-Ascend | **Not verified**; the members need [an image carrying CANN](../kv-cache/backend.md#the-image) | **Works**, a published `-router` runner tag named by hand, behind `llm-d-router` |
| `ROCM`, `MUSA`, `MACA` / `hip`, `musa`, `maca` | vLLM, SGLang | **Not verified** | **Not verified** |
| `ROCM`, `MUSA`, `MACA` / `hip`, `musa`, `maca` | vLLM-Ascend | **Not supported**: refused at admission, as for `TCP` | **Not supported**: ignored, as for `tcp` |

The vLLM and SGLang verdicts are read on their CUDA runner images. What each verdict means:

- **Works** — run on this operator with blocks moving, by the criterion under [the
  minimum](#the-minimum-per-shape), on the images the cell names.
- **Own image** — works only on an engine image you build and keep up to date yourself.
- **Not supported** — admission refuses it, the operator renders nothing for it, or no image the
  cell could run carries the transport; the cell says which.
- **Not verified** — no run. Nothing refuses it; nothing here says it works.

**"Default images" needs no image work from you**: the runner image the operator assembles for each
role, and the [`kv-cache-backend-image`](../settings.md) default for the store members. "Named by
hand" is a published image you write into `roles[].image` or the backend's `spec.image`.

**A cell is one leg.** A deployment using both legs is a shape of its own, read from [the
minimum](#the-minimum-per-shape) rather than added up from two cells; both legs over EFA at once
have not been run. With a positive `resources.interface`, an `RDMA` leg beside an `EFA` one is
refused at admission.

On AWS the `RDMA` rows do not apply — [choose `EFA` or
`TCP`](../operation/rdma.md#on-aws-rdma-is-not-an-option).

## Known failures at the minimum

- **SGLang with a store** holds a pinned host pool and needs the node's available memory above a
  fixed reserve plus that pool — see [SGLang's host-memory
  tier](kv-cache-injection.md#sglangs-host-memory-tier).
- **SGLang prefill/decode over `TCP` runs out of local ports under sustained load, with or without
  a store.** The prefill half opens a new connection for every transfer, to the decode half and to
  each store member, all from the same ephemeral ports of its container. Connections left in
  `TIME-WAIT` use them up; from then on every request through that pair fails and keeps failing
  until the engine Pods are restarted.

  **Turning on [`model-deployment-tcp-tw-reuse`](../settings.md#letting-sglang-prefill-pods-reuse-time-wait-ports)
  avoids it**, after a kubelet change on every node the prefill half can run on.

  An SGLang `0.5.18` pair with a `TCP` store, sent short chat requests one after another, locked
  up after about 183 requests in one run and 940 in another. A pair with no store, behind
  `sglang-gateway`, locked up after about 285, every `TIME-WAIT` socket pointing at the decode half;
  the gateway then answered `503` with `No available prefill workers`.

  With `net.ipv4.tcp_tw_reuse=1` set by hand in the prefill half's network namespace, the value the
  setting renders there, the pair with a store answered 1905 requests with none failing, holding
  about 19,500 sockets in `TIME-WAIT` against the 28,232 ports of the range. The pair without a
  store has not been run with it.

  Widening `net.ipv4.ip_local_port_range` only delays the lock-up. Whether a fabric transport,
  which opens no kernel TCP connection per transfer, avoids it is not verified.

  **vLLM is not this shape where it was measured**: its prefill half keeps its transfer connections
  open. Over a direct leg with no store, it kept four open and held at most nine sockets in
  `TIME-WAIT` across 729 requests.

  With a `TCP` store beside the direct leg, on vLLM `0.29.0`, it kept four open to the decode half
  and four to each of the two store members, and held at most 16 in `TIME-WAIT` across 720
  requests with none failing. The decode half's count leveled off below 200.

---

**See also** — [Model Deployment Reference](model-deployment.md) (the fields these versions go into) ·
[KV Cache Backend](../kv-cache/backend.md) (the store a client needs) ·
[RDMA Operations](../operation/rdma.md) (fabric devices and the engine image they need)

**Next** → [Model Deployment Status Reference](model-deployment-status.md)
