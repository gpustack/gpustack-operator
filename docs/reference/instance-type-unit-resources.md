# Unit Resources Preset Reference

> **Purpose** — the per-product CPU/RAM tier a node-derived `InstanceType` is sized with, and the
> public configuration each tier was taken from.
> **Audience** operators · **Prerequisites** [Scheduling
> Chain](../architecture/scheduling-chain.md#the-unit-spec-is-not-derived-from-node-capacity) ·
> **Read time** reference — look up your product

With `instance-type-derived-from-node` enabled, the operator summarizes a node into an
`InstanceType` and stamps its **unit resources** — the CPU and RAM for *one unit*, which for an
acceleratable type is **one whole accelerator**.

Chosen once, at creation: `spec.unitResources` is immutable afterwards and the operator never
updates a type it did not just create — so a type you authored, or one an earlier version created,
is never touched.

## Contents

- [What the preset does and does not affect](#what-the-preset-does-and-does-not-affect)
- [The tiers](#the-tiers)
- [NVIDIA](#nvidia)
- [Ascend](#ascend)
- [AMD](#amd)
- [Cambricon](#cambricon)
- [Hygon](#hygon)
- [MetaX](#metax)
- [MThreads](#mthreads)
- [Iluvatar](#iluvatar)
- [T-Head](#t-head)
- [Intel](#intel)
- [Kunlun](#kunlun)
- [Biren](#biren)
- [If a preset does not fit your hardware](#if-a-preset-does-not-fit-your-hardware)

## What the preset does and does not affect

- **It is the default request.** An Instance omitting `cpu`/`ram` is sized from it — by accelerator
  count for a whole accelerator, by memory percentage for a slice or partition.
- **It caps an explicit request.** An Instance setting `cpu`/`ram` is capped against it.

With `instance-general-resources-overcommit` enabled — the default — the preset is the container
*limit* and the scheduler sees `100m` CPU per core, `128Mi` per Gi instead: an `xlarge` accelerator
asks for 1.2 CPU / 24Gi. Disabled, the limit *is* the request — check the presets against what your
nodes provide per accelerator.

## The tiers

| Tier | VRAM band | Unit CPU | Unit RAM |
|---|---|---|---|
| `fallback` | anything not listed below | `4` | `16Gi` |
| `small` | ≤ 16 GiB | `8` | `32Gi` |
| `medium` | > 16, ≤ 48 GiB | `8` | `64Gi` |
| `large` | > 48, ≤ 96 GiB | `12` | `128Gi` |
| `xlarge` | > 96 GiB | `12` | `192Gi` |

`spec.localStorage` is always `100Gi`, never preset per product; a CPU-only derived type is always
`1` CPU / `2Gi`.

**An accelerator not listed below gets `fallback`** — `4` CPU / `16Gi`, what every accelerator got
before presets existed.

A family's tier starts from its VRAM band, then drops to what its **lowest** published
multi-accelerator host configuration supports on both axes; single-accelerator cloud tiers are
ignored, tracking the buyer's instance size rather than the accelerator. CPU stops at `12` on
purpose: being also the defaulted request, a generous CPU number is, with overcommit disabled, the
first thing to leave single-accelerator Pods `Pending`.

`Anchor` is the published configuration each preset was taken from, per accelerator.

## NVIDIA

| Family | Tier | Unit CPU | Unit RAM | Anchor |
|---|---|---|---|---|
| `nvidia-b200` | `xlarge` | `12` | `192Gi` | DGX B200 14c/256g; AWS p6-b200 24c/256g |
| `nvidia-b300` | `xlarge` | `12` | `192Gi` | AWS p6-b300 24c/512g; OCI BM.GPU.B300.8 16c/512g |
| `nvidia-gb200` | `xlarge` | `12` | `192Gi` | GB200 NVL72 36c/240g; Azure ND128isr 32c/216g |
| `nvidia-gb300` | `xlarge` | `12` | `192Gi` | GB300 NVL72 36c/240g; Azure ND128isr 32c/216g |
| `nvidia-h100` | `xlarge` | `12` | `192Gi` | Azure ND96isr 12c/237g; DGX H100 14c/256g; AWS p5 24c/256g |
| `nvidia-h200` | `xlarge` | `12` | `192Gi` | Azure ND96isr 12c/231g; DGX H200 14c/256g |
| `nvidia-h800` | `xlarge` | `12` | `192Gi` | Tencent HCCPNV5 24c/256g |
| `nvidia-gh200` | `xlarge` | `12` | `192Gi` | GH200 superchip 72c/480g |
| `nvidia-gb10` | `small` | `8` | `32Gi` | DGX Spark 20c/128g, where the 128g is unified CPU/GPU memory |
| `nvidia-a100` | `medium` | `8` | `64Gi` | any A100 whose name carries no capacity |
| `nvidia-a100-80gb` | `large` | `12` | `128Gi` | GCP a2-ultragpu 12c/170g; Alibaba gn7e 16c/125g |
| `nvidia-a100-40gb` | `medium` | `8` | `64Gi` | Azure ND96asr 12c/112g; GCP a2-highgpu 12c/85g |
| `nvidia-a800` | `medium` | `8` | `64Gi` | any A800 whose name carries no capacity |
| `nvidia-a800-80gb` | `large` | `12` | `128Gi` | Alibaba ebmgn7ex 16c/128g; Volcengine 16c/256g |
| `nvidia-a800-40gb` | `medium` | `8` | `64Gi` | mirrors A100 40GB |
| `nvidia-a30` | `small` | `8` | `32Gi` | Leafcloud / AceCloud 8c/32g |
| `nvidia-a10` | `medium` | `8` | `64Gi` | AWS g5 multi-accelerator 24c/96g |
| `nvidia-a10g` | `medium` | `8` | `64Gi` | AWS g5 multi-accelerator 24c/96g |
| `nvidia-l4` | `small` | `8` | `32Gi` | GCP g2 12c/48g |
| `nvidia-l40` | `medium` | `8` | `64Gi` | CoreWeave 16c/128g |
| `nvidia-l40s` | `large` | `12` | `128Gi` | AWS g6e 24c/192g; CoreWeave 16c/128g |
| `nvidia-t4` | `small` | `8` | `32Gi` | Tencent GN7 / Huawei pi2 8c/32g |
| `nvidia-v100` | `small` | `8` | `32Gi` | AWS p3 8c/61g; Alibaba gn6v 8c/32g |
| `nvidia-v100-32gb` | `medium` | `8` | `64Gi` | AWS p3dn 12c/96g |
| `nvidia-v100s` | `small` | `8` | `32Gi` | Tencent GN10X 9c/40g; Huawei p2vs 8c/64g |
| `nvidia-rtx-pro-4500` | `small` | `8` | `32Gi` | AWS g7 multi-accelerator 24c/96g; RunPod workstation 12c/54g |
| `nvidia-rtx-6000` | `small` | `8` | `32Gi` | Lambda 14c/46g; DigitalOcean 8c/64g |
| `nvidia-rtx-4000` | `small` | `8` | `32Gi` | DigitalOcean 8c/32g |
| `nvidia-rtx-a6000` | `small` | `8` | `32Gi` | Paperspace 8c/45g |
| `nvidia-rtx-a5000` | `small` | `8` | `32Gi` | Paperspace 8c/45g |
| `nvidia-rtx-a4000` | `small` | `8` | `32Gi` | Paperspace 8c/45g |
| `nvidia-rtx-3090` | `small` | `8` | `32Gi` | AutoDL / Featurize 8c/32g |
| `nvidia-rtx-4090` | `medium` | `8` | `64Gi` | AutoDL / Matpool 10–16c/64g; 8-accelerator boxes 24c/64g |
| `nvidia-rtx-5090` | `medium` | `8` | `64Gi` | Yotta Labs 14c/115g |

`nvidia-rtx-6000` also covers `Quadro RTX 6000`, `nvidia-rtx-4090` the 4090D.

## Ascend

| Family | Tier | Unit CPU | Unit RAM | Anchor |
|---|---|---|---|---|
| `ascend-310p` | `medium` | `8` | `64Gi` | Tianyi PAK1 18c/72g; PAK2 16c/128g |
| `ascend-310p1` | `medium` | `8` | `64Gi` | Tianyi PAK1 18c/72g; PAK2 16c/128g |
| `ascend-310p2` | `medium` | `8` | `64Gi` | Tianyi PAK1 18c/72g; PAK2 16c/128g |
| `ascend-310p3` | `medium` | `8` | `64Gi` | Tianyi PAK1 18c/72g; PAK2 16c/128g |
| `ascend-310p4` | `medium` | `8` | `64Gi` | Tianyi PAK1 18c/72g; PAK2 16c/128g |
| `ascend-310p5` | `medium` | `8` | `64Gi` | Tianyi PAK1 18c/72g; PAK2 16c/128g |
| `ascend-310p7` | `medium` | `8` | `64Gi` | Tianyi PAK1 18c/72g; PAK2 16c/128g |
| `ascend-910b` | `medium` | `8` | `64Gi` | KunLun G5680 V2 24c/64g; Atlas 800T A2 24c/192g |
| `ascend-910b1` | `medium` | `8` | `64Gi` | KunLun G5680 V2 24c/64g; Atlas 800T A2 24c/192g |
| `ascend-910b2` | `medium` | `8` | `64Gi` | KunLun G5680 V2 24c/64g; Atlas 800T A2 24c/192g |
| `ascend-910b2c` | `medium` | `8` | `64Gi` | KunLun G5680 V2 24c/64g; Atlas 800T A2 24c/192g |
| `ascend-910b3` | `medium` | `8` | `64Gi` | KunLun G5680 V2 24c/64g; Atlas 800T A2 24c/192g |
| `ascend-910b4` | `medium` | `8` | `64Gi` | KunLun G5680 V2 24c/64g; Atlas 800T A2 24c/192g |
| `ascend-910-9362` | `xlarge` | `12` | `192Gi` | Atlas 800T A3 32c/256g |
| `ascend-910-9372` | `xlarge` | `12` | `192Gi` | Atlas 800T A3 32c/256g |
| `ascend-910-9381` | `xlarge` | `12` | `192Gi` | Atlas 800T A3 32c/256g |
| `ascend-910-9382` | `xlarge` | `12` | `192Gi` | Atlas 800T A3 32c/256g |
| `ascend-910-9391` | `xlarge` | `12` | `192Gi` | Atlas 800T A3 32c/256g |
| `ascend-910-9392` | `xlarge` | `12` | `192Gi` | Atlas 800T A3 32c/256g |

910B is held one tier below its VRAM band because the KunLun G5680 V2 gives 64GB per accelerator;
`ascend-910b4` also covers `910B4-1`.

## AMD

| Family | Tier | Unit CPU | Unit RAM | Anchor |
|---|---|---|---|---|
| `amd-mi300x` | `xlarge` | `12` | `192Gi` | Azure ND96isr 12c/231g; OCI 14c/256g; Supermicro TNMR2 24c/288g |
| `amd-mi308x` | `xlarge` | `12` | `192Gi` | inherited from MI300X, which has no configuration of its own |
| `amd-mi325x` | `xlarge` | `12` | `192Gi` | TensorWave 16c/384g; Supermicro G1 16c/288g |
| `amd-mi350x` | `xlarge` | `12` | `192Gi` | Supermicro AS-8126GS 16c/384g |
| `amd-mi355x` | `xlarge` | `12` | `192Gi` | OCI BM.GPU.MI355X.8 16c/384g |
| `amd-mi250` | `medium` | `8` | `64Gi` | |
| `amd-mi250x` | `medium` | `8` | `64Gi` | Pawsey Setonix standard node 16c/64g; Frontier / LUMI-G 16c/128g |
| `amd-mi210` | `large` | `12` | `128Gi` | Dell PowerEdge R750xa 16c/128g |

## Cambricon

| Family | Tier | Unit CPU | Unit RAM | Anchor |
|---|---|---|---|---|
| `cambricon-mlu370` | `medium` | `8` | `64Gi` | Tianyi PCH1 16c/64g; 8-accelerator server 7c/64g |
| `cambricon-mlu590` | `medium` | `8` | `64Gi` | |

`cambricon-mlu370` covers `MLU370` and its `-S4` / `-X4` / `-X8` variants. The bare name `MLU` is
the driver's unknown-accelerator sentinel, deliberately never matched.

## Hygon

| Family | Tier | Unit CPU | Unit RAM | Anchor |
|---|---|---|---|---|
| `hygon-z100` | `small` | `8` | `32Gi` | Zhongke Kekong X7840H0 6c/32g |
| `hygon-z100l` | `small` | `8` | `32Gi` | Zhongke Kekong X7840H0 6c/32g |
| `hygon-k100` | `large` | `12` | `128Gi` | Tianyi 4U8 8c/128g; H3C R5330 G7 32c/128g |
| `hygon-bw100` | `large` | `12` | `128Gi` | |
| `hygon-bw1000` | `xlarge` | `12` | `192Gi` | |

`hygon-k100` also covers `K100_AI`.

## MetaX

| Family | Tier | Unit CPU | Unit RAM | Anchor |
|---|---|---|---|---|
| `metax-mxc500` | `medium` | `8` | `64Gi` | Lenovo WA5480G3 14c/64g; Lenovo 4U8 8c/128g |
| `metax-mxc550` | `large` | `12` | `128Gi` | Aituomou cloud 30c/180g; FlagPerf 14c/256g |
| `metax-mxc588` | `large` | `12` | `128Gi` | |
| `metax-mxc600` | `large` | `12` | `128Gi` | |

## MThreads

| Family | Tier | Unit CPU | Unit RAM | Anchor |
|---|---|---|---|---|
| `mthreads-mtt-s4000` | `medium` | `8` | `64Gi` | MCCX D800 8c/128g; AutoDL 15c/100g |
| `mthreads-mtt-s5000` | `medium` | `8` | `64Gi` | inherited from MTT S4000, which has no configuration of its own |

## Iluvatar

| Family | Tier | Unit CPU | Unit RAM | Anchor |
|---|---|---|---|---|
| `iluvatar-bi-v100` | `medium` | `8` | `64Gi` | |
| `iluvatar-bi-v150` | `medium` | `8` | `64Gi` | Phytium dual-socket machine 64c/64g |
| `iluvatar-mr-v50` | `medium` | `8` | `64Gi` | |
| `iluvatar-mr-v100` | `medium` | `8` | `64Gi` | Tenghua 4U8 16c/64g |

## T-Head

| Family | Tier | Unit CPU | Unit RAM | Anchor |
|---|---|---|---|---|
| `thead-ppu-zw810e` | `medium` | `8` | `64Gi` | 16 accelerators / 184 cores / 1.8TiB, i.e. 11.5c/112g |
| `thead-ppu-zwm890` | `medium` | `8` | `64Gi` | inherited from ZW810E, which has no configuration of its own |

The Iluvatar, MThreads and T-Head product strings come from manufacturer knowledge, unverified
against a device sample in this repository.

## Intel

| Family | Tier | Unit CPU | Unit RAM | Anchor |
|---|---|---|---|---|
| `intel-gaudi2` | `medium` | `8` | `64Gi` | HLS-Gaudi2 reference server 10c/128g |
| `intel-gaudi3` | `medium` | `8` | `64Gi` | IBM Cloud gx3d 20c/224g; Dell XE9680 15c/128g; Dell recommended 8c/256g |
| `intel-max-1550` | `large` | `12` | `128Gi` | Argonne Aurora blade 17c/171g; Dell XE9640 24c/128g |
| `intel-max-1100` | `medium` | `8` | `64Gi` | Dell R760xa 28c/256g; Kelvin2 16c/125g |

Not reachable yet: the operator has no manufacturer key for Intel, so a node carrying one is not
summarized into an accelerated `InstanceType`.

## Kunlun

| Family | Tier | Unit CPU | Unit RAM | Anchor |
|---|---|---|---|---|
| `kunlun-p800` | `medium` | `8` | `64Gi` | 8-accelerator machine 8c/64g; Inspur R3418/R3428 8c/256g |

Not reachable yet: the operator has no manufacturer key for Kunlun.

## Biren

| Family | Tier | Unit CPU | Unit RAM | Anchor |
|---|---|---|---|---|
| `biren-br100` | `medium` | `8` | `64Gi` | |
| `biren-br104` | `medium` | `8` | `64Gi` | |

Not reachable yet: the operator has no manufacturer key for Biren.

## If a preset does not fit your hardware

Create the `InstanceType` yourself: an administrator-created type is never touched by the operator,
and no preset overrides it. There is no setting to override the table — it ships in the operator
image. Two things to know first:

- Presets apply only to types created after the upgrade, since `spec.unitResources` is immutable and
  the operator only ever creates; an upgraded cluster carries mixed old and new sizing.
- Deleting a derived `InstanceType` has the operator author it again at the current presets — the
  supported way to re-size a pool the operator owns; the pool is not schedulable in between.

The table lives in `pkg/nodefeature/unit_resources_preset.yaml`, with the rules an edit must satisfy
at the top of that file; a test asserts every entry appears on this page.

---

**See also** — [Scheduling Chain](../architecture/scheduling-chain.md#the-unit-spec-is-not-derived-from-node-capacity)
(where the unit spec is stamped) · [Admission](../architecture/admission.md#the-instancetype-and-instance-webhooks)
(what the webhooks enforce on it) · [Walkthrough](../walkthrough.md#4-managing-a-custom-instancetype)

**Next** → [Settings](../settings.md#online-adjustable-settings) — the switches named on this page.
