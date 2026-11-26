# Parameters

Evidence: contract. `ModelDeployment` synthesizes engine and router arguments, environment
variables, files, ports, and volume mounts from its fields and bound `AcceleratorType`,
`KVCachePoolBinding`, and `ModelArtifact` resources.

The tables cover managed engine roles, routers, sidecars, and remote weight clients (`Engine` in
the `Component` column means both `vLLM` and `SGLang`). Parameters are organized along orthogonal
axes (`KVCache`, `KVTransfer`, `KVEvents`, `ECTransfer`, `Router`, `Serving`, and `Weight Delivery`)
with explicit `Engine Version` thresholds.

## Contents

- [Version thresholds](#version-thresholds)
- [Engine version resolution](#engine-version-resolution)
- [Override tiers](#override-tiers)
- [Serving and parallelism](#serving-and-parallelism)
- [Shared cache parameters](#shared-cache-parameters)
- [Transfer parameters](#transfer-parameters)
- [KV events](#kv-events)
- [Encoder cache parameters](#encoder-cache-parameters)
- [Router parameters](#router-parameters)
- [Weight delivery parameters](#weight-delivery-parameters)

## Version thresholds

Every version-gated synthesis boundary compares the role's resolved `(major, minor, patch)` release
tuple. Pre-release builds on a release line (`0.30.0rc1`, `0.30.0.rc1`, `0.34.0rc0`) satisfy that
line's milestone; legacy CRs with an empty stored version stay on the `< 0.30.0` baseline.

| Axis | Engine | Before | At/after | Threshold |
|---|---|---|---|---|
| `KVTransfer` | `vLLM-Ascend` | `MooncakeConnectorV1` (`prefill tp_size >= decode tp_size`) | `MooncakeConnectorV2` Pull mode (`max(tp_p, tp_d) % min(tp_p, tp_d) == 0`; mixed V1/V2 pairs refused) | `>= 0.30.0` |
| `KVEvents` | `vLLM`, `vLLM-Ascend` | `vllm-router` `kv_aware` refused at admission | `vllm-router` `kv_aware` admitted on *Server*/*Prefill*; emits `--kv-events-config`, `--prefix-caching-hash-algo sha256_cbor`, `VLLM_KV_EVENTS_USE_INT_BLOCK_HASHES=0`, ZMQ event/replay ports | `>= 0.31.0` |
| `ECTransfer` | `vLLM` (`ECCPUConnector`) | Defaulted `VLLM_USE_V2_MODEL_RUNNER=1` emitted | `VLLM_USE_V2_MODEL_RUNNER` omitted (removed upstream in `v0.34.0`) | `>= 0.34.0` |
| `ECTransfer` | `vLLM` (`ECCPUConnector` `Encode`) | Defaulted `VLLM_EC_SIDE_CHANNEL_PORT=5601` (matches upstream default `5601`) | Defaulted `VLLM_EC_SIDE_CHANNEL_PORT=5601` (pins port because upstream default changed to `0`); user override including `0` accepted | `>= 0.34.0` |
| Legacy CR | `vLLM`, `vLLM-Ascend` | Stored version empty: keeps `< 0.30.0` baseline (`MooncakeConnectorV1`, defaulted `VLLM_USE_V2_MODEL_RUNNER=1`) without in-memory tag re-parsing | Explicit or backfilled version governs all gates above | Empty vs set |

## Engine version resolution

Managed roles (`roles[].command` empty) resolve an effective engine version from
*`spec.roles[*].engineVersion`*, falling back to *`spec.engine.version`*.

| Rule | Scope | Behavior |
|---|---|---|
| Runner tag grammar | `gpustack/runner` images | Strips `@sha256:...` digest, splits after final `/` (preserving registry ports), and matches `[prefix/]runner:{backend}{backend_version}[-{backend_variant}]-{service}{service_version}[-{suffix}]` where `backend` is `Host|cann|corex|cuda|dtk|hggc|maca|musa|rocm` and `service` (`vllm|sglang`) matches *`spec.engine.name`* case-insensitively. |
| Defaulter backfill | `CREATE` and `UPDATE` | Backfills empty *`spec.engine.version`* from *`spec.engine.image`* and empty *`roles[i].engineVersion`* from non-empty *`roles[i].image`* whenever the image matches the `gpustack/runner` grammar. Never overwrites user values. |
| Custom or digest-only images | Managed roles on `CREATE` and `UPDATE` | Fails closed at admission if effective version is empty, or if a role overrides *`roles[i].image`* with a different unparseable image and leaves *`roles[i].engineVersion`* empty; set *`spec.engine.version`* or *`roles[i].engineVersion`* explicitly. |
| Runner tag mismatch | Parseable `gpustack/runner` image with explicit version | Refused at admission if explicit version disagrees with the parsed tag; update or clear the version field alongside the image. |
| Version syntax and comparison | Any non-empty version | Must match `major.minor[.patch[.micro]][(a|b|rc|post|dev)N]`. Milestone gates compare `(major, minor, patch)`, so `0.30.0rc1` satisfies `>= 0.30.0` and `0.34.0rc0` satisfies `>= 0.34.0`. |
| Deletion bypass | `DeletionTimestamp != nil` | All version and router admission rules short-circuit so finalizer removal never deadlocks. |

## Override tiers

| Tier | Trigger | Rule |
|---|---|---|
| Owned | Managed role and operator-owned key | Admission rejects duplicate *`roles[].extraArgs`* or *`roles[].env`*, including unique prefixes, underscores, and dotted members. |
| Defaulted | Managed role and absent user value | User *`roles[].env`* or *`roles[].extraArgs`* take precedence without admission error. |
| Take-over | Non-empty *`roles[].command`* | User owns the command line; engine CLI, KV/transfer/EC synthesis, and default cache mounts are skipped. *`status.roles[].unmanaged`* is `true`. |

See [Operator-owned keys](../docs/modules/model-deployment/configuration/index.md#operator-owned-keys) for
conditions and existing workloads. Ownership is scoped per `(engine, key)` and active feature context.

## Serving and parallelism

| Component | Tier | Engine Version | Trigger | Injected | Note |
|---|---|---|---|---|---|
| Engine | Owned | All | Managed serving | `vllm serve <model>` or `python3 -m sglang.launch_server --model-path <model> --enable-metrics` | Skipped when *`roles[].command`* is set. |
| Engine | Owned | All | *`spec.model.artifactRef`* set | `--model` or `--model-path`, `--revision`, `--tokenizer-revision` (vLLM), `--download-dir`, and Hub/ModelScope env (`HF_*`, `MODELSCOPE_*`, `VLLM_USE_MODELSCOPE`/`SGLANG_USE_MODELSCOPE`) | Rejected in user args/env while *`artifactRef`* is set. |
| Engine | Owned | All | *Decode* role with `routing-proxy` | `--port 8200` | Loopback port behind `routing-proxy` (which serves the role HTTP port). |
| Engine | Owned | All | Multi-member replica (`size > 1`) | `GPUSTACK_REPLICA_LEADER_ADDRESS`, `GPUSTACK_REPLICA_SIZE`, `GPUSTACK_MEMBER_INDEX` | Reserved on all engines at every *`size`*. |
| Engine | Defaulted | All | Managed serving | `--served-model-name=<spec.model.name>` | User *`extraArgs`* may repeat it only with value equal to *`spec.model.name`*. |
| Engine | Defaulted | All | Managed serving (no sidecar) | `--host 0.0.0.0`, `--port <service-port>` | User `--host` or `--port` takes precedence. |
| Engine | Defaulted | All | Managed role | `/var/lib/gpustack/cache` (`HostPath` for accelerated roles, `EmptyDir` for CPU-only) | Explicit user mount covering `/var/lib/gpustack/cache` takes precedence. |
| Engine | Defaulted | All | Managed role | `VLLM_CACHE_ROOT` or `SGLANG_CACHE_DIR` pointing at `/var/lib/gpustack/cache` | User *`roles[].env`* wins. |
| Engine | Defaulted | All | Managed role | `HF_HUB_CACHE`, `HF_XET_CACHE`, `PIP_CACHE_DIR`, `TORCH_HOME`, `FLASHINFER_WORKSPACE_BASE`, `TORCHINDUCTOR_CACHE_DIR`, `TRITON_CACHE_DIR` | User *`roles[].env`* wins; `HF_HOME` stays on container storage. |

- **Parallelism parsing**: The operator reads TP, PP, DP, EP, and CP flags from *`roles[].extraArgs`*
  (or *`roles[].command`*) plus literal `VLLM_DP_SIZE` in *`env`*. On SGLang, prefer `--attn-dp-size`
  (or `--attention-data-parallel-size`) over legacy `--enable-dp-attention`; both are parsed.
- **Compile-cache hash keys**: Tracked environment keys are `VLLM_USE_STANDALONE_COMPILE`,
  `VLLM_USE_MEGA_AOT_ARTIFACT`, `SGLANG_ENABLE_TORCH_COMPILE`, `TRITON_ATTN_NUM_STAGES`, and
  `TORCH_CUDA_ARCH_LIST`. Retired vLLM `< 0.30` variables (`VLLM_USE_V1`, `VLLM_ATTENTION_BACKEND`,
  `VLLM_FLASH_ATTN_VERSION`) are no longer hashed.

## Shared cache parameters

`KVCache` (*`spec.kvCache`*) attaches a deployment to a shared `KVCachePoolBinding`. It is orthogonal
to `KVTransfer`: when both are active on `vLLM`, *Prefill* composes both inside `MultiConnector`
(direct leg first, store connector second with `kv_role: "kv_both"`), while *Decode* renders only the
direct `KVTransfer` connector while keeping pool dtype and TCP block-size alignment.

| Component | Tier | Engine Version | Trigger | Injected | Note |
|---|---|---|---|---|---|
| Engine | Owned | All | Bound pool (`model-deployment-kv-cache-dtype-owned=true`) | `--kv-cache-dtype <binding-dtype>` | Owned on both engines when binding declares a dtype. |
| Engine | Defaulted | All | Cache injection | `MC_TE_METRIC=1` | User environment wins. |
| `vLLM` | Owned | All | Bound pool (non-`Encode`; omitted on `KVTransfer` *Decode*) | `--kv-transfer-config` with `MooncakeStoreConnector` (or `AscendStoreConnector` on Ascend), `MOONCAKE_CONFIG_PATH=/etc/gpustack/kvcache/mooncake.json`, projected `kvcache.gpustack.ai/client-config` volume | `cache_prefix` rendered on native `vLLM` only. |
| `vLLM` | Defaulted | All | Bound pool, Ascend A5 (`Family == "950"`), `interface: 0` | `ASCEND_LOCAL_COMM_RES='{"version":"1.3"}'` | Non-`Encode` roles only; user *`roles[].env`* wins; capacity-neutral. |
| `vLLM` | Owned | All | Bound pool, Ascend A5 (`Family == "950"`), `interface > 0` | Outer `kv_connector_extra_config.ascend_local_comm_res_path: "/etc/hixlep"` and read-only `/etc/hixlep` (`gpustack-ascend-hixlep`, `HostPathDirectory`) | Non-`Encode` roles only; user *`additionalVolumes`* at `/etc/hixlep` replaces the mount while keeping the JSON key. |
| `vLLM` | Defaulted | All | Bound TCP pool | `--block-size 128`, `MC_TCP_MAX_QUEUED_TRANSFERS_PER_PEER=65535`, `MC_TCP_MAX_PENDING_ADMISSIONS_PER_PEER=65535`, `MC_TCP_LANES_PER_PEER=16`, `MC_TCP_ADMISSION_TIMEOUT_MS=60000` | User `--block-size` or *`roles[].env`* wins. |
| `SGLang` | Owned | All | Bound pool (non-`Encode`) | `--hicache-storage-backend mooncake`, `--hicache-storage-backend-extra-config=<json>` (when cache prefix set), `MOONCAKE_MASTER`, `MOONCAKE_TE_META_DATA_SERVER`, `MOONCAKE_PROTOCOL`, `MOONCAKE_DEVICE`, `MOONCAKE_GLOBAL_SEGMENT_SIZE=0`, `MOONCAKE_LOCAL_HOSTNAME` (`status.podIP`), `MOONCAKE_TENANT_ID` | `SGLANG_HICACHE_MOONCAKE_CONFIG_PATH` is owned and left unset to force environment loading. |
| `SGLang` | Defaulted | All | Bound pool | `--enable-hierarchical-cache` (*Server* and *Prefill* roles) | Omitted on disaggregated *Decode*; user *`extraArgs`* wins. |
| `SGLang` | Defaulted | All | Bound TCP pool | `--page-size 64`, `MC_TCP_MAX_QUEUED_TRANSFERS_PER_PEER=65535`, `MC_TCP_MAX_PENDING_ADMISSIONS_PER_PEER=65535`, `MC_TCP_LANES_PER_PEER=16`, `MC_TCP_ADMISSION_TIMEOUT_MS=60000` | User `--page-size` or *`roles[].env`* wins. |

See [KV Cache Injection Reference](kv-cache-injection.md) for tenant and transport rules.

## Transfer parameters

`KVTransfer` (*`spec.kvTransfer`*) configures direct *Prefill*-to-*Decode* KV transfer behind a
managed router. It is orthogonal to `KVCache` and composes with a bound pool according to the
`MultiConnector` rule in [Shared cache parameters](#shared-cache-parameters).

![KV transfer parameter decision flow](../docs/assets/diagrams/model-deployment-transfer-decision.svg)

<!--
flowchart TD
    PAIR["Managed Prefill + Decode pair behind spec.router"] -> BACKEND{"spec.kvTransfer.backend"}
    BACKEND ->|"Mooncake or omitted on NVIDIA/AMD"| MC{"Mooncake link resolution"}
    BACKEND ->|"NIXL on NVIDIA/AMD"| NIXL{"aws-efa-enabled or EFA pool?"}
    BACKEND ->|"HIXL or omitted on Ascend"| HIXL["HIXL on Ascend: mount /usr/local/Ascend/driver RO, port 8998"]
    MC ->|"1. Ascend pool"| MC_ASC["protocol=ascend: vLLM MooncakeConnectorV1 (<0.30) or V2 (>=0.30) / SGLang mooncake + driver mount"]
    MC ->|"2. Pair interface > 0 + EFA"| MC_EFA["protocol=efa: vLLM mooncake_protocol=efa or SGLang mooncake + vpc.amazonaws.com/efa"]
    MC ->|"3. AMD ROCm pool"| MC_HIP["protocol=hip: vLLM mooncake_protocol=hip or SGLang mooncake (XGMI + RDMA/TCP auto-discovery)"]
    MC ->|"4. NVIDIA + Pair interface > 0"| MC_RDMA["protocol=rdma: vLLM mooncake_protocol=rdma or SGLang mooncake + device.gpustack.ai/rdma*"]
    MC ->|"5. NVIDIA without Pair interface > 0"| MC_TCP["protocol=tcp: vLLM mooncake_protocol=tcp or SGLang mooncake_tcp + defaulted MC_FORCE_TCP=1"]
    NIXL ->|"Yes"| NIXL_EFA["Plugin LIBFABRIC: vLLM NixlConnector or SGLang nixl, port 8998"]
    NIXL ->|"No"| NIXL_UCX["Plugin UCX: vLLM NixlConnector or SGLang nixl, port 8998"]
    HIXL ->|"vLLM"| HIXL_VLLM["vLLM: MooncakeConnectorV1 (<0.30) or V2 (>=0.30) + ASCEND_AUTO_CONNECT=1"]
    HIXL_VLLM ->|"A5 UB (interface: 0)"| HIXL_VLLM_UB["Default ASCEND_LOCAL_COMM_RES='{&quot;version&quot;:&quot;1.3&quot;}', no /etc/hixlep"]
    HIXL_VLLM ->|"A5 Host RoCE (interface > 0)"| HIXL_VLLM_ROCE["Outer ascend_local_comm_res_path: /etc/hixlep + /etc/hixlep RO mount"]
    HIXL ->|"SGLang"| HIXL_SGL["SGLang: ascend + ASCEND_MF_TRANSFER_PROTOCOL=sdma (A2/A3) or device_urma (A5), no /etc/hixlep"]
-->

- **Backend selection**: *`spec.kvTransfer.backend`* selects `Mooncake`, `NIXL`, or `HIXL` (default:
  `HIXL` on Ascend pools, `Mooncake` on NVIDIA or AMD pools). Every backend uses port `8998`.
- **Ascend connector generation**: On `vLLM-Ascend`, effective version `< 0.30.0` (or legacy empty)
  renders `MooncakeConnectorV1` (`prefill tp_size >= decode tp_size`); `>= 0.30.0` renders
  `MooncakeConnectorV2` Pull mode (`max(tp_p, tp_d) % min(tp_p, tp_d) == 0`). Mixing `< 0.30.0` and
  `>= 0.30.0` across *Prefill* and *Decode* is refused at admission.

| Component | Tier | Engine Version | Trigger | Injected | Note |
|---|---|---|---|---|---|
| Engine | Defaulted | All | Transfer injection | `MC_TE_METRIC=1` | User environment wins. |
| `vLLM` | Owned | All | `Mooncake`, NVIDIA/AMD | `MooncakeConnector` (`mooncake_protocol`: `rdma`, `efa`, `hip`, or `tcp`), *Prefill* `VLLM_MOONCAKE_BOOTSTRAP_PORT=8998` and port `8998` (`mc-bootstrap`) | Composed first in *Prefill* `MultiConnector` with a pool; *Decode* runs direct leg alone. |
| `vLLM` | Owned | All | `NIXL`, NVIDIA/AMD | `NixlConnector` (`backends: ["UCX"]` or `["LIBFABRIC"]` on EFA), `VLLM_NIXL_SIDE_CHANNEL_HOST` (`status.podIP`), port `8998` (`nixl-side`) on both roles | Composed first in *Prefill* `MultiConnector` with a pool; *Decode* runs `NixlConnector` alone. |
| `vLLM` | Defaulted | All | `NIXL`, NVIDIA/AMD | `VLLM_NIXL_SIDE_CHANNEL_PORT=8998` | Overrides upstream default `5600`; user environment wins. |
| `vLLM` | Owned | `< 0.30.0` or legacy | `Mooncake` or `HIXL`, Ascend | `MooncakeConnectorV1` (`kv_port: 8998`, `prefill`/`decode` `tp_size` & `dp_size`), *Prefill* port `8998`, read-only `/usr/local/Ascend/driver` | Requires `prefill tp_size >= decode tp_size`. |
| `vLLM` | Owned | `>= 0.30.0` | `Mooncake` or `HIXL`, Ascend | `MooncakeConnectorV2` Pull mode (`kv_port: 8998`, `prefill`/`decode` `tp_size` & `dp_size`), *Prefill* port `8998`, read-only `/usr/local/Ascend/driver` | Requires `max(tp_p, tp_d) % min(tp_p, tp_d) == 0`; mixed V1/V2 pairs refused. |
| `vLLM` | Defaulted | All | `HIXL`, Ascend | `ASCEND_AUTO_CONNECT=1` | User environment wins. |
| `vLLM` | Owned | All | Ascend A5 (`Family == "950"`), *PD*, `interface > 0` | Outer `kv_connector_extra_config.ascend_local_comm_res_path: "/etc/hixlep"` (including outer `MultiConnector`), read-only `/etc/hixlep` (`gpustack-ascend-hixlep`, `HostPathDirectory`) | Never set on inner `MultiConnector` children; omits `ASCEND_LOCAL_COMM_RES`. |
| `vLLM` | Defaulted | All | Ascend A5 (`Family == "950"`), *PD*, `interface: 0` | `ASCEND_LOCAL_COMM_RES='{"version":"1.3"}'` | In-memory HIXL auto-generation; user *`roles[].env`* wins; capacity-neutral. |
| `vLLM` | Defaulted | All | `Mooncake` TCP | `MC_FORCE_TCP=1`, `--block-size 128`, `MC_TCP_MAX_QUEUED_TRANSFERS_PER_PEER=65535`, `MC_TCP_MAX_PENDING_ADMISSIONS_PER_PEER=65535`, `MC_TCP_LANES_PER_PEER=16`, `MC_TCP_ADMISSION_TIMEOUT_MS=60000` | `MC_FORCE_TCP` skipped when a non-TCP store shares the process; user values win. |
| `SGLang` | Owned | All | `Mooncake`, NVIDIA/AMD/Ascend | `--disaggregation-mode=prefill|decode`, `--disaggregation-transfer-backend=mooncake|mooncake_tcp`, *Prefill* `--disaggregation-bootstrap-port 8998`, port `8998` (`bootstrap`), annotation `sglang.ai/bootstrap-port: "8998"`; read-only `/usr/local/Ascend/driver` on Ascend | Operator owns disaggregation flags. |
| `SGLang` | Owned | All | `NIXL`, NVIDIA/AMD | `--disaggregation-mode=prefill|decode`, `--disaggregation-transfer-backend=nixl`, *Prefill* `--disaggregation-bootstrap-port 8998`, port `8998`, annotation `sglang.ai/bootstrap-port: "8998"` | Operator owns disaggregation flags. |
| `SGLang` | Owned | All | `HIXL`, Ascend | `--disaggregation-mode=prefill|decode`, `--disaggregation-transfer-backend=ascend`, *Prefill* `--disaggregation-bootstrap-port 8998`, port `8998`, annotation `sglang.ai/bootstrap-port: "8998"`, read-only `/usr/local/Ascend/driver` | Operator owns disaggregation flags. |
| `SGLang` | Defaulted | All | Disaggregated *Decode* | `--disaggregation-decode-retraction-backup cpu_tensor` | User *`roles[].extraArgs`* wins. |
| `SGLang` | Defaulted | All | `Mooncake` TCP | `--page-size 64`, `MC_TCP_MAX_QUEUED_TRANSFERS_PER_PEER=65535`, `MC_TCP_MAX_PENDING_ADMISSIONS_PER_PEER=65535`, `MC_TCP_LANES_PER_PEER=16`, `MC_TCP_ADMISSION_TIMEOUT_MS=60000` | `mooncake_tcp` sets `MC_FORCE_TCP` inside SGLang; user values win. |
| `SGLang` | Defaulted | All | `NIXL`, NVIDIA/AMD | `SGLANG_DISAGGREGATION_NIXL_BACKEND=UCX` or `LIBFABRIC` (EFA) | User environment wins. |
| `SGLang` | Defaulted | All | `HIXL`, Ascend | `ASCEND_MF_TRANSFER_PROTOCOL=sdma` (A2/A3) or `device_urma` (A5, `Family == "950"`) | User environment wins; SGLang omits `/etc/hixlep` and `ASCEND_LOCAL_COMM_RES` on KV-only legs. |

- **A5 KV exclusions**: *Encode*, `SGLang` KV-only legs, and Ascend A2/A3 (`Family != "950"`) never
  receive `/etc/hixlep` or `ASCEND_LOCAL_COMM_RES` for KV transfer.
- **Scaler neutrality**: `ASCEND_LOCAL_COMM_RES`, `ASCEND_GLOBAL_RESOURCE_CONFIG`, and
  `ASCEND_MF_TRANSFER_PROTOCOL` are capacity-neutral for `ModelDeploymentScaler` priors.

## KV events

`KVEvents` publishing is orthogonal to `KVCache` and `KVTransfer`. Publishing activates only for the
router/engine pairs below, opening ZMQ publisher port `5557` (`kv-events`) and replay port `5558`
(`kv-replay`, or `5557 + dpSpan` when global `dpSpan > 1`) on the publishing role's `Pod` and,
except under `sgl-router`, its per-role `Service`.

| Router | Engine | Engine Version | Publishing roles | Injected config | Condition (`KVEventsPublishing`) |
|---|---|---|---|---|---|
| `llm-d-router` | `vLLM`, `vLLM-Ascend` | All | *Server*, *Prefill* (non-EPD, single-`Pod` DP only) | `--kv-events-config` (`enable_kv_cache_events: true`, `publisher: "zmq"`, `tcp://*:5557`, `tcp://*:<replayPort>`, `buffer_steps: 10000`, `hwm: 100000`, `max_queue_size: 100000`, `topic: "kv@pod@<spec.model.name>"`), ports `5557`/`<replayPort>`; router subscribes with `topicFilter: "kv@"` (*`spec.model.name`* must not contain `@`; EPD and multi-`Pod` DP fall back to `approx-prefix-cache-producer`) | `True/Publishing` (`True/NotApplicable` on EPD or multi-`Pod` DP) |
| `llm-d-router` | `SGLang` | All | None | Router uses `approx-prefix-cache-producer` (`llm-d-router@v1.0.0` `token-producer` requires vLLM `/v1/chat/completions/render`, which SGLang does not implement) | `True/NotApplicable` |
| `sgl-router` | `SGLang` | All | *Server*, *Prefill* (including store-less *Server*) | `--kv-events-config` (`publisher: "zmq"`, `tcp://*:5557`, `tcp://*:<replayPort>`, `topic: "kv-events"`), emitted before `--encoder-urls`, `Pod` ports `5557`/`<replayPort>` (per-role `Service` exposes only the HTTP serving port) | `True/Publishing` |
| `vllm-router` (default `cache_aware`) | `vLLM`, `vLLM-Ascend` | All | None | No KV events flags or ports | `True/NotApplicable` |
| `vllm-router` (`kv_aware` active) | `vLLM`, `vLLM-Ascend` | `>= 0.31.0` (`< 0.31.0` refused) | *Server* (`--policy`) or *Prefill* (`--policy` / `--prefill-policy`; never *Decode*) | `--kv-events-config` (`topic: "kv@pod@<spec.model.name>"`, `tcp://*:5557`, `tcp://*:<replayPort>`), `--prefix-caching-hash-algo sha256_cbor` (conditionally Owned), `VLLM_KV_EVENTS_USE_INT_BLOCK_HASHES=0` (conditionally Owned, capacity-neutral), ports `5557`/`<replayPort>` | `True/Publishing` |
| `sglang-gateway` (deprecated, frozen) | `SGLang` | All | None | None | `True/NotApplicable` |

- **Global DP port stride**: Both engines offset `endpoint` and `replay_endpoint` by global
  `dp_rank` (`0..dpSpan-1`, where `dpSpan` is `max(DataParallel, DataParallelLocal, 1)` on `vLLM`
  and `max(DataParallel, AttentionDataParallel, DWDPSize, 1)` on `SGLang`, never divided by
  `--nnodes`). Roles with `dpSpan > 1` use `replayPort = 5557 + dpSpan` and reserve
  `[5557 .. 5557 + 2*dpSpan - 1]` so `PUB` and `ROUTER` sockets stay disjoint across every rank.
- **`llm-d-router` DP limits**: `llm-d-router` passes a single `--endpoint-target-ports`
  (`RankIndex=0`, port `5557`) and one `replaySocketPort` (from the max `dpSpan` across publishing
  roles, excluding *Encode*/*Decode*). Multi-`Pod` `vLLM` DP disables KV events (`True/NotApplicable`,
  `approx-prefix-cache-producer`; use `vllm-router` `kv_aware` for `GET /kv_event_sources`), and
  single-`Pod` multi-DP subscribes to rank `0` only.
- **`llm-d-router` topic delimiter**: Admission rejects `@` in *`spec.model.name`* when
  `llm-d-router` KV events are active because `parseTopic` splits `"kv@pod@<model>"` on `@`.
- **Conditional ownership (`vllm-router` `kv_aware`)**: `--prefix-caching-hash-algo` and
  `VLLM_KV_EVENTS_USE_INT_BLOCK_HASHES` are refused in user args/env only on *Server*/*Prefill* roles
  where `kv_aware` is active, and stripped from user env in controller merge.
- **Shared `KVCachePool` caveat**: Enabling `kv_aware` switches prefix-caching hashes to
  `sha256_cbor`. Deployments sharing one `KVCachePool` reuse domain must use the same prefix hash
  algorithm if cross-deployment cache hits are expected.
- **Unmanaged roles**: Any expected publishing role with non-empty *`roles[].command`* sets
  `KVEventsPublishing=Unknown/RoleUnmanaged`.

## Encoder cache parameters

`ECTransfer` (*encode/prefill/decode (EPD)*) moves multimodal embeddings from *Encode* to *Prefill*.
*Encode* carries no KV cache and never receives `KVCache`, `KVTransfer`, or `KVEvents` parameters.

| Component | Tier | Engine Version | Trigger | Injected | Note |
|---|---|---|---|---|---|
| `vLLM` | Owned | All | *EPD* (`ECCPUConnector` or `ECExampleConnector`) | `--ec-transfer-config=<json>` (`ec_producer` on *Encode*, `ec_consumer` on *Prefill*; `ec_enable_nixl: true` + `ec_cpu_bytes: 1073741824` for `ECCPUConnector`, or `shared_storage_path` for `ECExampleConnector`) | Operator owns `--ec-transfer-config`. |
| `vLLM` | Defaulted | `< 0.34.0` or legacy | *EPD* with `ECCPUConnector` (*Encode* and *Prefill*) | `VLLM_USE_V2_MODEL_RUNNER=1` | Omitted at `>= 0.34.0`; user *`roles[].env`* wins; capacity-neutral. |
| `vLLM` | Owned | All | *EPD* with `ECCPUConnector` (*Encode*) | `VLLM_EC_SIDE_CHANNEL_HOST` (`status.podIP`) | Host is owned. |
| `vLLM` | Defaulted | All (upstream default changed to `0` at `>= 0.34.0`) | *EPD* with `ECCPUConnector` (*Encode*) | `VLLM_EC_SIDE_CHANNEL_PORT=5601`, `ec-side-channel` container port (tracks effective port: `5601` by default, custom port when overridden, omitted when `0`) | User *`roles[].env`* wins (`0` for ephemeral port or `1`–`65535`; empty string rejected); admission checks HTTP port conflicts against the effective port and skips conflict check when set to `0`. |
| `vLLM` | Defaulted | All | *EPD* (*Encode*) | `--enforce-eager`, `--no-enable-prefix-caching`, `--max-num-batched-tokens 114688`, `--mm-encoder-only` | User *`roles[].extraArgs`* wins. |
| `SGLang` | Owned | All | *EPD* | `--encoder-only` (*Encode*); `--language-only`, `--encoder-bootstrap-port 8997`, port `8997` (`enc-bootstrap`), and `--encoder-urls <urls...>` emitted last (*Prefill*) | `--kv-events-config` is emitted before `--encoder-urls` (`nargs="+"`). |

## Router parameters

Admission enforces a strict engine-router matrix: `sgl-router` and deprecated `sglang-gateway` accept
`SGLang` only; `vllm-router` accepts `vLLM` (including Ascend) only; `llm-d-router` accepts both.

| Component | Tier | Trigger | Injected | Note |
|---|---|---|---|---|
| `llm-d-router` | Owned | Managed router | `--endpoint-selector`, `--endpoint-target-ports`, `--config-file`, `--secure-serving=false`, `--grpc-health-port`, `--metrics-endpoint-auth=false` | Mounted config sets `precise-prefix-cache-producer` (`topicFilter: "kv@"`) for `vLLM`/`vLLM-Ascend` and `approx-prefix-cache-producer` for `SGLang`. |
| `routing-proxy` | Owned | *Decode* sidecar under `llm-d-router` | `--kv-connector=mooncake` + `--mooncake-bootstrap-port=8998` (vLLM Mooncake), `--kv-connector=nixlv2` (vLLM NIXL or Ascend), `--kv-connector=sglang` (SGLang), plus `--ec-connector=ec-nixl` or `ec-example` on vLLM *EPD* | Intercepts *Decode* service port and forwards to engine loopback `:8200`. |
| `vllm-router` | Owned | Managed router | `--host`, `--port`, `--prometheus-host=0.0.0.0`, `--prometheus-port=9090`, `--kv-connector=mooncake|nixl`, `--service-discovery`, `--service-discovery-namespace`, `--service-discovery-port`, `--selector` (or `--vllm-pd-disaggregation`, `--epd-config`, `--prefill-selector`, `--decode-selector`), optional `--request-timeout-secs` | Runs behind Envoy allowlist sidecar (`:8000` API, `:9091` `/observer/endpoints`). |
| `sgl-router` | Owned | Managed router (`SGLang`) | `--host`, `--port`, `--model-id=<spec.model.name>`, `--chat-routing=reorg`, `--no-tokenizer` (conditional), `--service-discovery`, `--service-discovery-namespace`, `--selector` (or `--prefill-selector` + `--decode-selector`; no `--pd-disaggregation`, `--service-discovery-port`, or `--prometheus-port`), `--dp-aware` (External DP and multi-DP *PD*), optional `--request-timeout-secs` | Uses Kubernetes `EndpointSlice` discovery (`discovery.k8s.io/endpointslices` `get, list, watch` RBAC; per-role `Service` carries `modeldeployment.gpustack.ai/role-kind` and exposes only the HTTP serving port, while publishing `Pod`s expose `5557`/`<replayPort>`). Runs behind Envoy (`:8000` API, `:9090` `/metrics` proxy, `:9091` `/observer/endpoints` via observer patch). |
| `sglang-gateway` | Owned | Managed router (`SGLang`, deprecated, frozen at `gateway-v0.3.1`) | `--host`, `--port`, `--prometheus-host=0.0.0.0`, `--prometheus-port=9090`, `--service-discovery`, `--service-discovery-namespace`, `--service-discovery-port`, `--selector` (or `--pd-disaggregation`, `--prefill-selector`, `--decode-selector`), `--dp-aware` (undivided External DP only), optional `--request-timeout-secs` | Frozen; prefer `sgl-router` for new SGLang deployments. |

- **`sgl-router` `--no-tokenizer` omission**: Emitted by default so `sgl-router` starts offline in
  air-gapped clusters without calling Hugging Face Hub. Automatically omitted when
  *`spec.router.extraArgs`* supplies `--tokenizer-path`, `--bucket-config`, or sets `--policy` to
  `cache_aware` (under operator-owned `--chat-routing=reorg`, `--policy` accepts `power_of_two`,
  `cache_aware`, and `session_aware`; `--decode-policy` accepts `power_of_two`; `--filter` accepts
  `overloaded`).

## Weight delivery parameters

| Component | Tier | Engine Version | Trigger | Injected | Note |
|---|---|---|---|---|---|
| Engine | Owned | All | Remote `ModelArtifact` delivery | `GPUSTACK_MODEL_TRANSFER_BACKEND=nixl|hixl`, `GPUSTACK_MODEL_TRANSFER_ARTIFACT`, `GPUSTACK_MODEL_TRANSFER_ARTIFACT_UID`, `GPUSTACK_MODEL_TRANSFER_MANIFEST_DIGEST`, `GPUSTACK_MODEL_TRANSFER_MODEL_PATH`, `GPUSTACK_MODEL_TRANSFER_TOKEN`, `GPUSTACK_MODEL_TRANSFER_WHEEL_DIR`, `GPUSTACK_MODEL_TRANSFER_LOAD_FORMAT`, `GPUSTACK_MODEL_TRANSFER_CONFIG` | Supplies delivery identity, staged wheel path, and session credentials. |
| Engine | Owned | All | ModelExpress peer delivery | `MX_SERVER_ADDRESS`, `MX_WORKER_HOST` (`status.podIP`), `MX_ARTIFACT_TRANSFER=1`, `MX_LOAD_PLUGIN=gpustack`, `MX_LOAD_PLUGIN_BEFORE`, `MX_ARTIFACT_COMPILE_CONFIG_DIGEST`, `MX_ARTIFACT_READY_URL`, `GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY=Prefer|Require` | See [Remote Weight Delivery](../docs/modules/model-delivery/remote/index.md) for eligibility and fallback. |
| Engine | Defaulted | All | Remote `ModelArtifact` delivery | `--load-format=model-transfer` | Preserves an existing supported load format; conflicting formats are refused. |
| Engine | Defaulted | All | NIXL remote delivery | `GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND=UCX` or `LIBFABRIC` (EFA) | User environment wins. |
| Engine | Defaulted | All | Ascend A5 (`Family == "950"`), HIXL remote delivery, `interface: 0` | `ASCEND_LOCAL_COMM_RES='{"version":"1.3"}'` | Deduplicated with KV connector defaults; user *`roles[].env`* wins. |
| Engine | Defaulted | All | Ascend A5 (`Family == "950"`), HIXL remote delivery, `interface > 0` | `ASCEND_GLOBAL_RESOURCE_CONFIG='{"comm_resource_config.protocol_desc":["roce:host"],"local_comm_res_path":"/etc/hixlep"}'` and read-only `/etc/hixlep` (`gpustack-ascend-hixlep`, `HostPathDirectory`) | *Server* and symmetric *PD* roles on `vLLM` or `SGLang`; user *`roles[].env`* and *`additionalVolumes`* win. |
| Engine | Defaulted | All | Remote delivery | `GPUSTACK_MODEL_TRANSFER_TIMEOUT=1800` | User environment wins. |

---

**See also** — [Model Deployment Configuration](../docs/modules/model-deployment/configuration/index.md) (role ownership and overrides) · [Prefill and Decode](../docs/modules/model-deployment/prefill-decode/index.md) (pairing and direct transports) · [Model Deployment Routing](../docs/modules/model-deployment/routing/index.md) (router policies and discovery) · [Model Deployment Internals](model-deployment-internals.md) (render versions, RBAC, and refusal catalog)
