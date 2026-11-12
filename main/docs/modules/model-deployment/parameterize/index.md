# Parameters

`ModelDeployment` synthesizes engine and router arguments, environment variables, files and volume
mounts from its fields and bound `InstanceType`, `KVCachePoolBinding` and `ModelArtifact` resources.
The tables describe managed engine roles, routers, sidecars and remote weight clients (`Engine` in
the `Component` column means both `vLLM` and `SGLang`). Rendering establishes configuration; it
does not establish hardware or image qualification.

## Contents

- [Override tiers](#override-tiers)
- [Serving parameters](#serving-parameters)
- [Shared cache parameters](#shared-cache-parameters)
- [Transfer parameters](#transfer-parameters)
- [Router parameters](#router-parameters)
- [Weight delivery parameters](#weight-delivery-parameters)

## Override tiers

| Tier | Trigger | Rule |
|---|---|---|
| Owned | Managed role and operator-owned key | Admission rejects duplicate `roles[].extraArgs` or `roles[].env`, including recognized prefixes, underscores and dotted members. |
| Defaulted | Managed role and absent user value | User `roles[].env` or applicable `roles[].extraArgs` take precedence without admission error. This includes listen flags, compile caches, transfer defaults and tuning variables. |
| Take-over | Non-empty `roles[].command` | User owns the command line; engine CLI and KV/transfer client environment synthesis are skipped. `status.roles[].unmanaged` is `true`. |

See [Operator-owned keys](/gpustack-operator/main/docs/modules/model-deployment/configuration/index.md#operator-owned-keys) for conditions, exceptions and existing
workloads. Ownership of a flag depends on the engine and its context.

## Serving parameters

| Component | Tier | Trigger | Injected | Note |
|---|---|---|---|---|
| Engine | Owned | Managed serving | Base launch command (`vllm serve <model>` or `python3 -m sglang.launch_server --model-path <model> --enable-metrics`) | Operator builds the base entrypoint unless `roles[].command` takes over. |
| Engine | Owned | `spec.model.artifactRef` set | `--model` or `--model-path`, `--revision`, `--tokenizer-revision`, `--download-dir`, and Hub/ModelScope credentials (`HF_*`, `MODELSCOPE_*`) | Admission rejects duplicate artifact-owned flags or environment keys while `artifactRef` is set. |
| Engine | Owned | Decode role with `routing-proxy` | `--port 8200` | Reserved for the sidecar-to-engine loopback port (`routing-proxy` owns the external service port). |
| Engine | Owned | Multi-member replica | `GPUSTACK_REPLICA_LEADER_ADDRESS`, `GPUSTACK_REPLICA_SIZE`, `GPUSTACK_MEMBER_INDEX` | Operator owns replica membership variables on all roles. |
| Engine | Defaulted | Managed serving | `--served-model-name=<spec.model.name>` | User `roles[].extraArgs` may state the flag only when its value matches `spec.model.name`. |
| Engine | Defaulted | Managed serving (no sidecar) | `--host 0.0.0.0`, `--port <service-port>` | User `roles[].extraArgs` `--host` or `--port` takes precedence. |
| Engine | Defaulted | Managed role | `/var/lib/gpustack/cache` mount | An explicit mount at that path takes precedence. |
| Engine | Defaulted | Managed role | `VLLM_CACHE_ROOT` or `SGLANG_CACHE_DIR` | Set `roles[].env` to override. |
| Engine | Defaulted | Managed role | `HF_HUB_CACHE`, `HF_XET_CACHE`, `PIP_CACHE_DIR`, `TORCH_HOME`, `FLASHINFER_WORKSPACE_BASE`, `TORCHINDUCTOR_CACHE_DIR`, `TRITON_CACHE_DIR` | Set `roles[].env` to override each path. |

[Compilation cache and timeout](/gpustack-operator/main/docs/modules/model-deployment/configuration/index.md#compilation-cache-and-timeout) lists the cache
paths and startup budget. A take-over role receives neither the default mount nor cache variables.

## Shared cache parameters

A `spec.kvCache` binding supplies the shared-store connection and dtype. Its tenant and transport
rules are described in [KV Cache Injection](/gpustack-operator/main/docs/modules/kv-cache/injection/index.md).

| Component | Tier | Trigger | Injected | Note |
|---|---|---|---|---|
| Engine | Owned | Bound pool | `--kv-cache-dtype` | Owned while `spec.kvCache` is set and `model-deployment-kv-cache-dtype-owned` is enabled (default `true`). |
| Engine | Defaulted | Cache injection | `MC_TE_METRIC=1` | User environment wins. |
| vLLM | Owned | Bound pool | `MooncakeStoreConnector`; Ascend uses `AscendStoreConnector` | Operator owns `--kv-transfer-config`. |
| vLLM | Owned | Bound pool | `MOONCAKE_CONFIG_PATH=/etc/gpustack/kvcache/mooncake.json` and generated file | Do not supply the environment key. |
| vLLM | Defaulted | Bound TCP pool | `--block-size 128`, `MC_TCP_MAX_QUEUED_TRANSFERS_PER_PEER=65535`, `MC_TCP_MAX_PENDING_ADMISSIONS_PER_PEER=65535`, `MC_TCP_LANES_PER_PEER=16`, `MC_TCP_ADMISSION_TIMEOUT_MS=60000` | User `roles[].extraArgs` `--block-size` or `roles[].env` takes precedence. |
| SGLang | Owned | Bound pool | `--hicache-storage-backend=mooncake`, `--hicache-storage-backend-extra-config=<json>` | Carries `extra_backend_tag` when a cache prefix is resolved. |
| SGLang | Owned | Bound pool | `MOONCAKE_MASTER`, `MOONCAKE_TE_META_DATA_SERVER`, `MOONCAKE_PROTOCOL`, `MOONCAKE_DEVICE`, `MOONCAKE_GLOBAL_SEGMENT_SIZE`, `MOONCAKE_LOCAL_HOSTNAME`, `MOONCAKE_TENANT_ID` | Operator derives values from the binding and Pod. |
| SGLang | Owned | Bound pool | `SGLANG_HICACHE_MOONCAKE_CONFIG_PATH` | Reserved; the operator leaves it unset to select environment configuration. |
| SGLang | Defaulted | Bound pool | `--enable-hierarchical-cache` (Server and Prefill roles), `--disaggregation-decode-retraction-backup cpu_tensor` (Decode role) | User `roles[].extraArgs` takes precedence. |
| SGLang | Defaulted | Bound pool | `SGLANG_ENABLE_OVERLAP_PLAN_STREAM=1`, `SGLANG_HICACHE_ENABLE_GARBAGE_COLLECTION=1` | User environment wins. |
| SGLang | Defaulted | Bound TCP pool | `--page-size 64`, `MC_TCP_MAX_QUEUED_TRANSFERS_PER_PEER=65535`, `MC_TCP_MAX_PENDING_ADMISSIONS_PER_PEER=65535`, `MC_TCP_LANES_PER_PEER=16`, `MC_TCP_ADMISSION_TIMEOUT_MS=60000` | User `roles[].extraArgs` `--page-size` or `roles[].env` takes precedence. |

## Transfer parameters

The table applies to managed *Prefill* and *Decode* roles behind a router. See
[Direct transfer transport](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode/index.md#direct-transfer-transport) for admission and image
requirements.

![KV transfer parameter decision flow](/gpustack-operator/main/docs/assets/diagrams/model-deployment-transfer-decision.svg)

<!--
flowchart TD
    PAIR["Managed Prefill + Decode pair behind spec.router"] -> BACKEND{"spec.kvTransfer.backend"}
    BACKEND ->|"Mooncake or omitted on NVIDIA/AMD"| MC{"Mooncake link resolution"}
    BACKEND ->|"NIXL on NVIDIA/AMD"| NIXL{"aws-efa-enabled or EFA pool?"}
    BACKEND ->|"HIXL or omitted on Ascend"| HIXL["HIXL on Ascend: mount /usr/local/Ascend/driver RO, port 8998"]
    MC ->|"1. Ascend pool"| MC_ASC["protocol=ascend: vLLM MooncakeConnectorV1 or SGLang mooncake + driver mount"]
    MC ->|"2. Pair interface > 0 + EFA"| MC_EFA["protocol=efa: vLLM mooncake_protocol=efa or SGLang mooncake + vpc.amazonaws.com/efa"]
    MC ->|"3. AMD ROCm pool"| MC_HIP["protocol=hip: vLLM mooncake_protocol=hip or SGLang mooncake (XGMI + RDMA/TCP auto-discovery)"]
    MC ->|"4. NVIDIA + Pair interface > 0"| MC_RDMA["protocol=rdma: vLLM mooncake_protocol=rdma or SGLang mooncake + device.gpustack.ai/rdma*"]
    MC ->|"5. NVIDIA without Pair interface > 0"| MC_TCP["protocol=tcp: vLLM mooncake_protocol=tcp or SGLang mooncake_tcp + defaulted MC_FORCE_TCP=1"]
    NIXL ->|"Yes"| NIXL_EFA["Plugin LIBFABRIC: vLLM NixlConnector or SGLang nixl, port 8998"]
    NIXL ->|"No"| NIXL_UCX["Plugin UCX: vLLM NixlConnector or SGLang nixl, port 8998"]
    HIXL -> HIXL_OUT["vLLM MooncakeConnectorV1 + ASCEND_AUTO_CONNECT=1 | SGLang ascend + ASCEND_MF_TRANSFER_PROTOCOL=sdma"]
-->

The operator resolves transfer parameters in two steps:

- **Step 1 — Backend selection**: *`spec.kvTransfer.backend`* selects `Mooncake`, `NIXL`, or `HIXL`. When omitted, the operator defaults to `HIXL` on Ascend pools and `Mooncake` on NVIDIA or AMD pools. Every backend uses base port `8998` for its KV handshake listener.
- **Step 2 — Link and parameter derivation**:
  - **Mooncake**: Evaluates pool vendor, pair *`resources.interface > 0`* (both *Prefill* and *Decode*), and EFA state in order:
    1. **Ascend (`ascend`)**: Renders `MooncakeConnectorV1` on vLLM or `--disaggregation-transfer-backend=mooncake` on SGLang, mounts `/usr/local/Ascend/driver` read-only, and omits `MC_FORCE_TCP`.
    2. **EFA (`efa`)**: When both roles request *`interface > 0`* and `aws-efa-enabled` is `true` (or a bound `KVCache` pool uses EFA), allocates `vpc.amazonaws.com/efa` and renders `mooncake_protocol: efa` on vLLM or `mooncake` on SGLang.
    3. **AMD ROCm (`hip`)**: On AMD pools, renders `mooncake_protocol: hip` on vLLM or `mooncake` on SGLang without `MC_FORCE_TCP`, so Mooncake's `MultiTransport` auto-discovers intra-node XGMI alongside cross-node RDMA or TCP.
    4. **NVIDIA RDMA (`rdma`)**: When both roles request *`interface > 0`* on NVIDIA, allocates `device.gpustack.ai/rdma*` and renders `mooncake_protocol: rdma` on vLLM or `mooncake` on SGLang without `MC_FORCE_TCP`.
    5. **NVIDIA TCP fallback (`tcp`)**: Without pair *`interface > 0`* on NVIDIA, renders `mooncake_protocol: tcp` on vLLM or `mooncake_tcp` on SGLang and defaults `MC_FORCE_TCP=1` (unless a non-TCP store shares the process) to prevent unconfigured cross-host NVLink selection.
  - **NIXL**: Selects `LIBFABRIC` when the `aws-efa-enabled` `Setting` is `true` or a bound `KVCache` pool uses EFA; otherwise selects `UCX` (which negotiates InfiniBand, RoCE, NVLink/IPC, or TCP at runtime). On vLLM, both *Prefill* and *Decode* open side-channel port `8998`.
  - **HIXL**: Mounts `/usr/local/Ascend/driver` read-only on both engines so CANN `hccn_tool` resolves NPU RoCE endpoints, and defaults `ASCEND_AUTO_CONNECT=1` on vLLM or `ASCEND_MF_TRANSFER_PROTOCOL=sdma` on SGLang.

| Component | Tier | Trigger | Injected | Note |
|---|---|---|---|---|
| Engine | Defaulted | Transfer injection | `MC_TE_METRIC=1` | User environment wins. |
| vLLM | Owned | Mooncake, NVIDIA/AMD | `MooncakeConnector` (`mooncake_protocol` set to `rdma`, `efa`, `hip`, or `tcp`), prefill `VLLM_MOONCAKE_BOOTSTRAP_PORT=8998`; `MOONCAKE_CONFIG_PATH` and generated file with a pool | Operator owns connector and environment keys. |
| vLLM | Owned | NIXL, NVIDIA/AMD | `NixlConnector` with `kv_connector_extra_config.backends` set to `UCX` or EFA `LIBFABRIC`; first in `MultiConnector` with a pool | Operator owns connector document. |
| vLLM | Owned | NIXL, NVIDIA/AMD | `VLLM_NIXL_SIDE_CHANNEL_HOST` from `status.podIP`, container port `8998` on both roles | Host is reserved. |
| vLLM | Owned | Mooncake, Ascend | `MooncakeConnectorV1` (`kv_port: 8998` and `prefill`/`decode` `tp_size` and `dp_size` in `--kv-transfer-config`), prefill container port `8998`, and read-only `/usr/local/Ascend/driver` mount | Connector document derives parallel sizes from role configuration. |
| vLLM | Owned | HIXL, Ascend | `MooncakeConnectorV1` (`kv_port: 8998` and `prefill`/`decode` `tp_size` and `dp_size` in `--kv-transfer-config`), prefill container port `8998`, and read-only `/usr/local/Ascend/driver` mount | Operator owns connector document. |
| vLLM | Defaulted | Mooncake TCP | `MC_FORCE_TCP=1`, `--block-size 128`, `MC_TCP_MAX_QUEUED_TRANSFERS_PER_PEER=65535`, `MC_TCP_MAX_PENDING_ADMISSIONS_PER_PEER=65535`, `MC_TCP_LANES_PER_PEER=16`, `MC_TCP_ADMISSION_TIMEOUT_MS=60000` | Skipped for `MC_FORCE_TCP` when a shared store uses a non-TCP transport; user values win. |
| vLLM | Defaulted | NIXL, NVIDIA/AMD | `VLLM_NIXL_SIDE_CHANNEL_PORT=8998` | Overrides upstream vLLM default `5600`; user environment wins. |
| vLLM | Defaulted | HIXL, Ascend | `ASCEND_AUTO_CONNECT=1` | User environment wins. |
| SGLang | Owned | Mooncake, NVIDIA/AMD/Ascend | `--disaggregation-mode=prefill` or `decode`, `--disaggregation-transfer-backend=mooncake_tcp` or `mooncake`, prefill `--disaggregation-bootstrap-port=8998`; read-only `/usr/local/Ascend/driver` mount on Ascend | Operator owns disaggregation flags. |
| SGLang | Owned | Mooncake path with pool | `MOONCAKE_*` connection variables | Operator derives pool connection variables from the binding and Pod. |
| SGLang | Owned | NIXL, NVIDIA/AMD | `--disaggregation-mode=prefill` or `decode`, `--disaggregation-transfer-backend=nixl`, prefill `--disaggregation-bootstrap-port=8998` | Operator owns disaggregation flags. |
| SGLang | Owned | HIXL, Ascend | `--disaggregation-mode=prefill` or `decode`, `--disaggregation-transfer-backend=ascend`, prefill `--disaggregation-bootstrap-port=8998`, read-only `/usr/local/Ascend/driver` mount | Operator owns disaggregation flags. |
| SGLang | Defaulted | Mooncake TCP | `MC_FORCE_TCP=1`, `--page-size 64`, `MC_TCP_MAX_QUEUED_TRANSFERS_PER_PEER=65535`, `MC_TCP_MAX_PENDING_ADMISSIONS_PER_PEER=65535`, `MC_TCP_LANES_PER_PEER=16`, `MC_TCP_ADMISSION_TIMEOUT_MS=60000` | `MC_FORCE_TCP=1` applies on an eligible TCP path; user values win. |
| SGLang | Defaulted | NIXL, NVIDIA/AMD | `SGLANG_DISAGGREGATION_NIXL_BACKEND=UCX` or `LIBFABRIC` on EFA | User environment wins. |
| SGLang | Defaulted | HIXL, Ascend | `ASCEND_MF_TRANSFER_PROTOCOL=sdma` | User environment wins. |

## Router parameters

| Component | Tier | Trigger | Injected | Note |
|---|---|---|---|---|
| llm-d-router | Owned | Managed router | Endpoint selector, target ports, configuration file, secure serving, health and metrics flags | Router `extraArgs` duplicates are refused. |
| routing-proxy | Owned | Decode sidecar, vLLM Mooncake | `--kv-connector=mooncake`, `--mooncake-bootstrap-port=8998` | Operator generates the sidecar container arguments. |
| routing-proxy | Owned | Decode sidecar, vLLM NIXL or Ascend | `--kv-connector=nixlv2` | Operator generates the sidecar container arguments. |
| routing-proxy | Owned | Decode sidecar, SGLang | `--kv-connector=sglang` | Operator generates the sidecar container arguments. |
| routing-proxy | Owned | Encode path | `--ec-connector=ec-nixl` or `ec-example` | Derived from encoder-cache configuration. |
| vllm-router, sglang-gateway | Owned | Managed router | Bind, port and service-discovery flags | Router `extraArgs` duplicates are refused. |
| vllm-router | Owned | Managed pair | `--kv-connector=mooncake`; `--kv-connector=nixl` for NIXL or Ascend | Router `extraArgs` duplicates are refused. |
| vllm-router | Owned | Encode path | `--epd-config` | Operator derives encoder endpoints. |
| sglang-gateway | Owned | Managed pair | `--pd-disaggregation` | Router `extraArgs` duplicates are refused. |
| vLLM | Owned | KV event routing | `--kv-events-config` and event/replay ports `5557`/`5558` | Operator derives endpoints. |
| vLLM | Owned | Encode stage (EPD) | `--ec-transfer-config=<json>` (`ECCPUConnector` or `ECExampleConnector`); with `ECCPUConnector`, `VLLM_USE_V2_MODEL_RUNNER=1` and on `Encode` `VLLM_EC_SIDE_CHANNEL_HOST` from `status.podIP` plus `VLLM_EC_SIDE_CHANNEL_PORT=5601` | Operator owns the encoder-cache connector document, v2 runner flag and side-channel address. |
| vLLM | Defaulted | Encode stage (EPD) | `--enforce-eager`, `--no-enable-prefix-caching`, `--max-num-batched-tokens 114688`, `--mm-encoder-only` (`Encode` role) | User `roles[].extraArgs` takes precedence. |
| SGLang | Owned | Encode stage (EPD) | `--encoder-only` (`Encode`), `--language-only`, `--encoder-urls` and `--encoder-bootstrap-port 8997` (`Prefill`) | Operator owns multimodal encoder stage flags and the encoder bootstrap port. |

`routing-proxy` is the `llm-d` sidecar injected into managed `decode` Pods when `llm-d-router`
fronts a pair; it intercepts the decode service port, runs the first-stage prefill-to-decode KV
handshake, and forwards to the local engine (`vllm-router` and `sglang-gateway` handle this in the
standalone router Deployment instead). [Router configuration](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode/index.md#router-configuration)
lists router compatibility and [Routing](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md) describes routing policies.

## Weight delivery parameters

| Component | Tier | Trigger | Injected | Note |
|---|---|---|---|---|
| Engine | Owned | Remote `ModelArtifact` delivery | `GPUSTACK_MODEL_TRANSFER_BACKEND=nixl` or `hixl`, `GPUSTACK_MODEL_TRANSFER_ARTIFACT`, `GPUSTACK_MODEL_TRANSFER_ARTIFACT_UID`, `GPUSTACK_MODEL_TRANSFER_MANIFEST_DIGEST`, `GPUSTACK_MODEL_TRANSFER_MODEL_PATH`, `GPUSTACK_MODEL_TRANSFER_TOKEN`, `GPUSTACK_MODEL_TRANSFER_WHEEL_DIR`, `GPUSTACK_MODEL_TRANSFER_LOAD_FORMAT`, `GPUSTACK_MODEL_TRANSFER_CONFIG` | Operator supplies the delivery identity, staged wheel path and session credentials. |
| Engine | Owned | ModelExpress peer delivery | `MX_SERVER_ADDRESS` (metadata sidecar), `MX_WORKER_HOST` from `status.podIP`, `MX_ARTIFACT_TRANSFER=1`, `MX_LOAD_PLUGIN=gpustack`, `MX_LOAD_PLUGIN_BEFORE`, `MX_ARTIFACT_COMPILE_CONFIG_DIGEST`, `MX_ARTIFACT_READY_URL`, `GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY=Prefer` or `Require` | See [Remote Weight Delivery](/gpustack-operator/main/docs/modules/model-delivery/remote/index.md) for eligibility and fallback. |
| Engine | Defaulted | Remote `ModelArtifact` delivery | `--load-format=model-transfer` | An existing supported load format is preserved; conflicting formats are refused. |
| Engine | Defaulted | NIXL remote delivery | `GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND=UCX` or `LIBFABRIC` on EFA | User environment wins. |
| Engine | Defaulted | Remote delivery | `GPUSTACK_MODEL_TRANSFER_TIMEOUT=1800` | User environment wins. |

---

**See also** — [Model Deployment Configuration](/gpustack-operator/main/docs/modules/model-deployment/configuration/index.md) for role ownership and
[Prefill and Decode](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode/index.md) for pairing and transport.

**Next** → [Engine Compatibility](/gpustack-operator/main/docs/modules/model-deployment/engine-versions/index.md)
