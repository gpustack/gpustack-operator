# Engine Versions Reference

> **Purpose** — the lowest vLLM, vLLM-Ascend and SGLang release each deployment shape has been run
> with on this operator, with the Mooncake client it carries and the store it needs.
> **Audience** users, operators · **Prerequisites** [Model Deployment
> Reference](model-deployment.md) · **Read time** reference — look up your engine

**An engine below its minimum here is not supported.** Older releases fail in ways that belong to
those releases, and this documentation does not track them; upgrading is the fix for each of them.

## Contents

- [The minimum per shape](#the-minimum-per-shape)
- [Reading the table](#reading-the-table)
- [Known failures at the minimum](#known-failures-at-the-minimum)

## The minimum per shape

| Engine | Shape | Minimum | Mooncake client in the runner image | Store members |
|---|---|---|---|---|
| vLLM | alone | `0.29.0` | `0.3.13.post1`, on the `cuda12.9` and the `cuda13.0` tag | — |
| vLLM | prefill/decode, direct transfer over `tcp` | `0.29.0` | `0.3.13.post1` | — |
| vLLM | with a Mooncake store, alone or as a pair | `0.29.0` | `0.3.13.post1` | the 0.3.13 line; the [default image](../settings.md) is one |
| vLLM | on a fabric (`rdma`, `efa`) | not with a published runner image | `0.3.13.post1`, not an EFA build | not verified |
| vLLM-Ascend | alone | `0.23.0` | `0.3.11.post1`, the NPU build | — |
| vLLM-Ascend | prefill/decode, direct transfer | `0.23.0`, behind `llm-d-router` | `0.3.11.post1`, the NPU build | — |
| vLLM-Ascend | with a Mooncake store | not verified | `0.3.11.post1`, the NPU build | not verified |
| SGLang | alone | `0.5.18`, run as the halves of a pair | `0.3.12.post1` | — |
| SGLang | prefill/decode, direct transfer over `tcp` | `0.5.18` | `0.3.12.post1` | — |
| SGLang | with a Mooncake store | `0.5.18`, run as a pair | `0.3.12.post1` | the 0.3.12 line, such as `kvcacheai/mooncake:0.3.12.post1` |
| SGLang | on a fabric (`rdma`, `efa`) | not verified | `0.3.12.post1`, not an EFA build | not verified |

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

**An EFA leg needs a Mooncake build the published runner images do not carry** — see [RDMA
Operations](../operation/rdma.md#which-key-a-workload-asks-for). An image differing from the vLLM
runner only by the EFA build of the same client, plus libfabric, has moved blocks over EFA on both
legs. Over `rdma` devices, no engine in the table has been verified.

**"Not verified" means no run, not a known failure.** Nothing refuses such a shape; nothing here
says it works.

## Known failures at the minimum

- **vLLM `0.29.0`, a pair with a store, behind `vllm-router`**: the decode engine aborts on its
  first request with an assertion in vLLM's store connector, while the store write succeeds. The
  same pair behind `llm-d-router` has served with both connectors moving bytes; what separates the
  two runs has not been isolated.
- **SGLang with a store** holds a pinned host pool and needs the node's available memory above a
  fixed reserve plus that pool — see [SGLang's host-memory
  tier](kv-cache-injection.md#sglangs-host-memory-tier).

---

**See also** — [Model Deployment Reference](model-deployment.md) (the fields these versions go into) ·
[KV Cache Backend](../kv-cache/backend.md) (the store a client needs) ·
[RDMA Operations](../operation/rdma.md) (fabric devices and the engine image they need)

**Next** → [Model Deployment Status Reference](model-deployment-status.md)
