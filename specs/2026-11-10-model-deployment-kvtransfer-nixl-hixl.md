# Spec: ModelDeployment KV Transfer Support for NIXL, HIXL, and EFA LIBFABRIC

Status: Building
Blocked on: implementation and verification of T1-T5
Type: Feature

- **Related Specs**: [2026-09-12-model-deployment-pd-pairing-and-router.md](2026-09-12-model-deployment-pd-pairing-and-router.md) · [2026-09-18-kvcache-and-model-deployment-consolidation.md](2026-09-18-kvcache-and-model-deployment-consolidation.md) · [2026-09-19-model-deployment-router-implementations.md](2026-09-19-model-deployment-router-implementations.md) · [2026-10-28-model-artifact-remote-weight-loading.md](2026-10-28-model-artifact-remote-weight-loading.md)

## Summary

Extend `ModelDeployment.spec.kvTransfer` with an optional `backend` selector (`Auto`, `Mooncake`, `NIXL`, `HIXL`) alongside the existing `protocol` field (`Auto`, `TCP`, `RDMA`, `EFA`, `CANN`, `ROCM`).
Enable parameter injection for point-to-point prefill/decode KV transfer using **NIXL** (`NixlConnector` in vLLM, `--disaggregation-transfer-backend nixl` in SGLang) and **HIXL** (`MooncakeConnectorV1` over Ascend Direct / ADXL in vLLM-Ascend, `--disaggregation-transfer-backend ascend` in SGLang on Ascend).
Automatically switch the NIXL transport plugin from default `UCX` to `LIBFABRIC` across vLLM, SGLang, and `gpustack_model_transfer` whenever AWS EFA (`vpc.amazonaws.com/efa`) is active.
Align managed routers (`vllm-router` and `llm-d-router`) so that vLLM `NIXL` and Ascend point-to-point KV transfer use the response-relay handshake (`--kv-connector=nixl` on `vllm-router` and `--kv-connector=nixlv2` on the `llm-d-router` decode sidecar).

## Motivation

`ModelDeployment` previously assumed Mooncake as the only point-to-point prefill/decode KV transfer backend on non-Ascend pools and hardcoded a partial Ascend `MooncakeConnectorV1` path that was disabled behind `vllm-router`. Four gaps prevented users from running NIXL, HIXL, and AWS EFA (`vpc.amazonaws.com/efa`) across vLLM, SGLang, and remote weight delivery:

1. **No direct KV transfer backend selector in `ModelDeployment.spec.kvTransfer`**:
   - Users could configure `spec.kvTransfer.protocol`, which mapped to `mooncake_protocol` in vLLM (`MooncakeConnector`) and `mooncake` / `mooncake_tcp` in SGLang, but could not select `NIXL` or `HIXL`.
   - When vLLM composes a direct prefill/decode leg with a shared `KVCache` pool under `MultiConnector`, the direct connector (`NixlConnector`) must remain first (`connectors[0]`) so `MultiConnector` returns the NIXL worker handshake metadata (`VLLM_NIXL_SIDE_CHANNEL_HOST` and `VLLM_NIXL_SIDE_CHANNEL_PORT`) to the engine core.
2. **No automatic `UCX` to `LIBFABRIC` switching on AWS EFA (`vpc.amazonaws.com/efa`)**:
   - Both vLLM (`NixlConnector`) and SGLang (`--disaggregation-transfer-backend nixl`) default their NIXL transport plugin to `UCX`. On AWS EFA nodes (`vpc.amazonaws.com/efa`), NIXL requires the `LIBFABRIC` plugin (`kv_connector_extra_config.backends: ["LIBFABRIC"]` in vLLM and `SGLANG_DISAGGREGATION_NIXL_BACKEND=LIBFABRIC` in SGLang).
   - In `pkg/worker/controllers/worker/model_deployment.go`, `SynthesizeModelDeploymentConnector` ran before `ModelDeploymentInterfaceResource` resolved the role's extended fabric resource (`vpc.amazonaws.com/efa` vs `device.gpustack.ai/rdma*`), and `ModelDeploymentDirectInterfaceProtocol` ignored SGLang prefill/decode roles completely.
3. **Hardcoded `rdma` fabric and `UCX` agent in `model-transfer` on EFA**:
   - `ModelDeploymentDirectInterfaceProtocol` in `pkg/worker/controllers/worker/model_deployment_fabric.go` unconditionally returned `"rdma"` when remote weight delivery (`model-transfer` over `"nixl"`) was active. Combining remote weight delivery with an EFA `KVCache` pool or `spec.kvTransfer.protocol: EFA` failed admission and reconciliation with a conflicting `efa` vs `rdma` fabric error.
   - `NixlSourceBackend.start` and `NixlTransferEngineClient.start` in `model-transfer/python/gpustack_model_transfer` passed only `{"enable_listen_thread": False}` to `create_agent`, relying on NIXL's default `["UCX"]` plugin and synthesizing `UCX_NET_DEVICES` even when a user supplied an EFA-capable `model-transfer` image.
4. **Incomplete Ascend HIXL and router alignment**:
   - On Ascend pools, vLLM-Ascend `MooncakeConnectorV1` uses the Ascend Direct (`ADXL` / `HIXL`) data plane, where HIXL's `EndpointMatcher` automatically selects the optimal intra-node (`UBMEM`, `UB-CTP`, `HCCS`) or inter-node (`UBoE`, `UB-RTP`, `Device RoCE`, `Host RoCE`) link unless constrained to Device RoCE (`ASCEND_GLOBAL_RESOURCE_CONFIG={"comm_resource_config.protocol_desc":"roce:device"}` and `HCCL_INTRA_ROCE_ENABLE=1`).
   - `pkg/worker/controllers/worker/model_deployment_connector.go` skipped Ascend point-to-point KV transfer when `spec.router.name == "vllm-router"`, and `pkg/worker/kvcache/router/argv.go` hardcoded `--kv-connector=mooncake` for `vllm-router` instead of selecting `--kv-connector=nixl` (the response-relay handshake required by both vLLM `NixlConnector` and Ascend `MooncakeConnectorV1`).
   - SGLang on Ascend had no `--disaggregation-transfer-backend ascend` or `ASCEND_MF_TRANSFER_PROTOCOL` injection path.

### Goals

- Add an optional `backend` field (`Auto`, `Mooncake`, `NIXL`, `HIXL`) to `ModelDeploymentKVTransfer` in `api/worker/v1` and `api/worker/v1alpha1`, preserving 100% byte-for-byte backward compatibility across both non-Ascend and Ascend existing renders when `backend` is omitted, `Auto`, or `Mooncake`.
- Inject vLLM `NixlConnector` (`kv_role: "kv_producer" | "kv_consumer"`) with owned `VLLM_NIXL_SIDE_CHANNEL_HOST` (`status.podIP` via DownwardAPI), defaulted `VLLM_NIXL_SIDE_CHANNEL_PORT=5600` (plus container port `5600` on both prefill and decode roles), and `kv_connector_extra_config.backends: ["UCX"]` by default or `["LIBFABRIC"]` when EFA (`protocol: EFA` or resolved `vpc.amazonaws.com/efa`) is active.
- Inject SGLang `--disaggregation-transfer-backend nixl` with defaulted `SGLANG_DISAGGREGATION_NIXL_BACKEND=UCX` by default or `LIBFABRIC` when EFA is active (leaving `--host` to `appendModelDeploymentBindArgs` so SGLang prefill health probes remain gradable), and allow SGLang prefill/decode roles to request RDMA or EFA interface resources through `ModelDeploymentDirectInterfaceProtocol`.
- Align HIXL on Ascend pools:
  - For vLLM-Ascend: render `MooncakeConnectorV1` (`kv_role`, `kv_port: 8998`, `prefill`/`decode` `tp_size`/`dp_size` blocks, `/usr/local/Ascend/driver` read-only hostPath mount). When `backend: HIXL` is explicitly set, inject defaulted `ASCEND_AUTO_CONNECT=1`, and when `protocol: RDMA` is also set, inject defaulted `ASCEND_GLOBAL_RESOURCE_CONFIG={"comm_resource_config.protocol_desc":"roce:device"}` and `HCCL_INTRA_ROCE_ENABLE=1`.
  - For SGLang on Ascend: render `--disaggregation-transfer-backend ascend` (`--disaggregation-mode prefill|decode`, `--disaggregation-bootstrap-port 8998` on prefill), mount `/usr/local/Ascend/driver` read-only, and inject defaulted `ASCEND_MF_TRANSFER_PROTOCOL=device_rdma` when `protocol: RDMA` or `sdma` otherwise.
- Update router handshake selection:
  - `vllm-router` renders `--kv-connector=nixl` (in the same `delta.Extra` position ahead of `--service-discovery`) when the deployment uses vLLM `NIXL` or runs a prefill/decode pair on an Ascend pool (`MooncakeConnectorV1` / `HIXL`), and `--kv-connector=mooncake` otherwise; lift the controller gate that previously suppressed Ascend point-to-point KV transfer behind `vllm-router`.
  - `llm-d-router` decode `routing-proxy` sidecar renders `--kv-connector=nixlv2` for vLLM `NIXL` and Ascend `MooncakeConnectorV1` / `HIXL`.
- Enable EFA (`vpc.amazonaws.com/efa` and `LIBFABRIC`) in `model-transfer` without modifying default CUDA/ROCm `pack/model-transfer/Dockerfile` builds:
  - Resolve `"efa"` instead of hardcoded `"rdma"` for NIXL remote weight delivery when `spec.kvTransfer.protocol == "EFA"` or the bound `KVCache` store uses `"efa"`, and default `GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND=LIBFABRIC` via `appendModelDeploymentEnvIfAbsent` on the target engine container (`UCX` otherwise).
  - Support `GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND` (`UCX` | `LIBFABRIC`) and `/sys/class/infiniband` EFA device auto-detection in `model-transfer/python/gpustack_model_transfer`, passing `backends: [backend]` to `create_agent` and skipping `UCX_NET_DEVICES` synthesis when `LIBFABRIC` is active.

### Non-Goals

- Rebuilding default `pack/model-transfer/Dockerfile` CUDA or ROCm images with AWS EFA libfabric userspace providers (users running EFA supply an EFA-enabled `model-transfer` image via `modelTransfer.images.nvidia` in the Helm chart).
- Integrating `MindIE-Motor` as a managed `ModelDeployment.spec.engine.name` option (`MindIE-Motor` is used strictly as a reference for Ascend HIXL and SGLang MemFabric parameters).
- Automatically rendering the out-of-tree experimental `HIXLConnector` (`vllm_hixl_connector.connector`) by default; users testing custom experimental images can supply `--kv-transfer-config` via `role.command` take-over.
- Changing interface allocation for SGLang multi-member `Server` roles (`ModelDeploymentDirectInterfaceProtocol` widens for SGLang prefill/decode pairs only).

## Proposal

### User Stories

#### Story 1
As a user deploying disaggregated vLLM or SGLang on NVIDIA or AMD GPUs with `ModelDeployment`, I want to set `spec.kvTransfer.backend: NIXL` so that the Operator injects NIXL point-to-point KV transfer parameters (`NixlConnector` with `VLLM_NIXL_SIDE_CHANNEL_HOST`/`PORT` for vLLM, or `--disaggregation-transfer-backend nixl` with `SGLANG_DISAGGREGATION_NIXL_BACKEND` for SGLang) and configures the managed router handshake (`nixl` or `nixlv2`) automatically.

#### Story 2
As a user running disaggregated vLLM, disaggregated SGLang, or NIXL remote weight delivery (`model-transfer`) on AWS EFA nodes (`vpc.amazonaws.com/efa`), I want the Operator and `gpustack_model_transfer` to allocate `vpc.amazonaws.com/efa` and switch the NIXL plugin from `UCX` to `LIBFABRIC` when EFA is configured, without breaking standard RoCE/InfiniBand `UCX` deployments.

#### Story 3
As a user deploying disaggregated vLLM-Ascend or SGLang on Huawei Ascend NPUs with `spec.kvTransfer.backend: HIXL` (or `Auto`), I want the Operator to render the Ascend HIXL / ADXL or MemFabric configuration across both `llm-d-router` and `vllm-router`, let HIXL auto-select the intra-node or cross-node transport by default, and constrain the transport to Device RoCE when `spec.kvTransfer.protocol: RDMA` is explicitly set.

### Core Features & Acceptance Criteria

1. **`ModelDeployment.spec.kvTransfer.backend` Schema and Validation**:
   - `ModelDeploymentKVTransfer` in `api/worker/v1` and `api/worker/v1alpha1` adds optional `backend` with enum `["Auto","Mooncake","NIXL","HIXL"]`.
   - Omitting `backend` or setting `Auto` or `Mooncake` preserves byte-for-byte existing rendering on both non-Ascend and Ascend vLLM pools.
   - Admission webhook rejects invalid backend/manufacturer combinations at creation and update (including when a role changes `instanceType` or `spec.kvTransfer` changes): when `InstanceType.status.detail.manufacturer` is observed, `HIXL` is refused on non-Ascend pools, `NIXL` is refused on Ascend pools, and mixed Ascend/non-Ascend prefill/decode pairs are refused when point-to-point KV transfer is active. If the manufacturer is unobserved at admission and later resolves to an incompatible vendor, `inject` returns a typed `ReasonTransportUnsupported` refusal.
2. **vLLM `NIXL` Injection and `MultiConnector` Composition**:
   - When `backend: NIXL` is selected for vLLM on a non-Ascend routed prefill/decode pair, `pkg/worker/kvcache/inject/vllm.go` renders `kv_connector: "NixlConnector"`, `kv_role: "kv_producer"` (prefill) or `"kv_consumer"` (decode), and `kv_connector_extra_config: {"backends": ["UCX"]}` by default or `{"backends": ["LIBFABRIC"]}` when EFA (`protocol: EFA` or resolved `vpc.amazonaws.com/efa`) is active.
   - Injects owned `VLLM_NIXL_SIDE_CHANNEL_HOST` referencing `status.podIP` (DownwardAPI), defaulted `VLLM_NIXL_SIDE_CHANNEL_PORT=5600`, and container port `5600` (`nixl-side`) on both prefill and decode pods; webhook reserves port `5600` on both prefill and decode roles when vLLM `NIXL` is active.
   - When composed with a shared `KVCache` pool (`MooncakeStoreConnector`), `MultiConnector` places `NixlConnector` first (`connectors[0]`) and `MooncakeStoreConnector` second (`connectors[1]`).
3. **SGLang `NIXL` Injection and Fabric Interface Support**:
   - When `backend: NIXL` is selected for SGLang on a non-Ascend prefill/decode pair, `pkg/worker/kvcache/inject/sglang.go` renders `--disaggregation-transfer-backend=nixl` (alongside `--disaggregation-mode` and prefill `--disaggregation-bootstrap-port=8998`, without emitting `--host` so `appendModelDeploymentBindArgs` keeps prefill health probes gradable) and defaulted `SGLANG_DISAGGREGATION_NIXL_BACKEND=UCX` (`LIBFABRIC` when EFA is active).
   - `ModelDeploymentDirectInterfaceProtocol` in `pkg/worker/controllers/worker/model_deployment_fabric.go` resolves the direct transfer protocol for both vLLM and SGLang prefill/decode pairs so SGLang prefill/decode roles with `resources.interface > 0` receive `device.gpustack.ai/rdma*` or `vpc.amazonaws.com/efa` (SGLang multi-member `Server` roles remain unchanged).
4. **Ascend `HIXL` Injection across vLLM-Ascend and SGLang**:
   - On Ascend pools, vLLM-Ascend renders `MooncakeConnectorV1` (`kv_role`, `kv_port: 8998`, `prefill`/`decode` `tp_size` and `dp_size`, `/usr/local/Ascend/driver` read-only mount). When `spec.kvTransfer.backend == "HIXL"`, it injects defaulted `ASCEND_AUTO_CONNECT=1`, and when `spec.kvTransfer.protocol == "RDMA"` is also set, it additionally injects defaulted `ASCEND_GLOBAL_RESOURCE_CONFIG={"comm_resource_config.protocol_desc":"roce:device"}` and `HCCL_INTRA_ROCE_ENABLE=1`.
   - On Ascend pools, SGLang prefill/decode pairs render `--disaggregation-transfer-backend=ascend` (without `--host`), mount `/usr/local/Ascend/driver` read-only, and inject defaulted `ASCEND_MF_TRANSFER_PROTOCOL=device_rdma` when `spec.kvTransfer.protocol == "RDMA"` or `sdma` otherwise.
   - Both `llm-d-router` and `vllm-router` are supported for Ascend vLLM prefill/decode pairs.
5. **Router Handshake Selection (`vllm-router` and `llm-d-router`)**:
   - `pkg/worker/kvcache/router` adds `KVConnector string` to `router.Input` (defaulting to `"mooncake"` when empty) and renders `--kv-connector=<value>` in `renderArgvRouter` for `VLLM` in the existing `delta.Extra` position (ahead of `--service-discovery` in both undivided and split shapes).
   - `pkg/worker/controllers/worker/model_deployment_router.go` sets `KVConnector: "nixl"` for `vllm-router` when `backend: NIXL` is active or when a disaggregated prefill/decode role runs on an Ascend `InstanceType`, and `"mooncake"` otherwise.
   - `pkg/worker/controllers/worker/model_deployment_render.go` selects `--kv-connector=nixlv2` on the `llm-d-router` decode `routing-proxy` sidecar when vLLM uses `backend: NIXL` or runs on an Ascend `InstanceType`.
6. **AWS EFA (`LIBFABRIC`) Support in `model-transfer`**:
   - `ModelDeploymentDirectInterfaceProtocol` accepts optional `storeProtocols` / `artifact` context and returns `"efa"` (instead of hardcoded `"rdma"`) for NIXL remote weight delivery when `spec.kvTransfer.protocol == "EFA"` or when the role's bound `KVCache` store protocol is `"efa"`.
   - `(ModelDeploymentArtifactRender).applyModelTransfer` injects `GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND=LIBFABRIC` via `appendModelDeploymentEnvIfAbsent` into the target container when the resolved interface resource is `vpc.amazonaws.com/efa` or `spec.kvTransfer.protocol == "EFA"` (and `UCX` otherwise), allowing user overrides in `role.env`.
   - `model-transfer/python/gpustack_model_transfer` reads `GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND` (`UCX` | `LIBFABRIC`), auto-detects AWS EFA devices under `/sys/class/infiniband` (PCI vendor `0x1d0f` or `rdmap*` / `efa_*` device names) when the variable is unset, passes `{"enable_listen_thread": False, "backends": [backend]}` to `create_agent`, and skips `UCX_NET_DEVICES` synthesis when `LIBFABRIC` is selected.

### Notes / Constraints / Caveats

- AWS EFA uses the `vpc.amazonaws.com/efa` extended resource exclusively (`efaDeviceResource` in `pkg/worker/kvcache/mooncake/member_workload.go`).
- Default `pack/model-transfer/Dockerfile` CUDA and ROCm stages are intentionally unchanged to avoid libfabric/UCX build conflicts in standard RoCE/InfiniBand images. Users on AWS EFA configure an EFA-enabled `model-transfer` DaemonSet image via `modelTransfer.images.nvidia` in the Helm chart.
- `SynthesizeModelDeploymentConnector` in `pkg/worker/controllers/worker/model_deployment.go` must receive the resolved direct interface protocol / fabric resource (`vpc.amazonaws.com/efa` vs `device.gpustack.ai/rdma*`) so that a role bound to an EFA `KVCache` pool with `spec.kvTransfer.backend: NIXL` and `protocol: Auto` also switches NIXL to `LIBFABRIC` when `vpc.amazonaws.com/efa` is granted.
- All seven new environment variables must be registered in the controller's ownership/defaulting tables (`model_deployment_connector.go`): `VLLM_NIXL_SIDE_CHANNEL_HOST` joins `modelDeploymentOwnedKeys[ModelDeploymentEngineVLLM].Env`, while `VLLM_NIXL_SIDE_CHANNEL_PORT`, `SGLANG_DISAGGREGATION_NIXL_BACKEND`, `ASCEND_AUTO_CONNECT`, `ASCEND_GLOBAL_RESOURCE_CONFIG`, `HCCL_INTRA_ROCE_ENABLE`, and `ASCEND_MF_TRANSFER_PROTOCOL` join `modelDeploymentDefaultedEnvNames`.

### Boundaries

- **Always:** Keep `spec.kvTransfer.backend` optional so omitted, `Auto`, and `Mooncake` configurations produce byte-for-byte identical output to existing releases on both non-Ascend and Ascend vLLM pools; run `make generate`, `make lint`, `make lint python`, and `make lint docs` before completion.
- **Ask first:** Modifying `pack/model-transfer/Dockerfile` default CUDA/ROCm image stages or changing Helm chart values schema.
- **Never:** Emit `--host` from SGLang connector synthesis (which disables prefill health probes), drop `--kv-connector` from undivided `vllm-router` argv, hardcode `aws.com/efa` (always use `vpc.amazonaws.com/efa`), synthesize `UCX_NET_DEVICES` when NIXL `LIBFABRIC` is selected, or reference uncommitted local paths in committed specs or docs.

### Risks and Mitigations

- **Emitting `--host` in SGLang connector args marks the listener as user-moved (`gradable=false`) and strips health probes** → Do not emit `--host` in `pkg/worker/kvcache/inject/sglang.go`; rely on `appendModelDeploymentBindArgs` (`--host 0.0.0.0`) as existing SGLang Mooncake disaggregation does.
- **Synthesizing `NixlConnector` second inside vLLM `MultiConnector` drops NIXL side-channel metadata** → Keep the direct connector (`NixlConnector`) at index `0` and `MooncakeStoreConnector` at index `1` in `MultiConnector` because vLLM's `MultiConnector` returns the first non-nil worker handshake metadata to the scheduler.
- **Port collision on vLLM `NIXL` side-channel port (`5600`)** → Inject `VLLM_NIXL_SIDE_CHANNEL_PORT=5600` via `DefaultedEnv` (allowing user override in `role.env`), declare port `5600` on both prefill and decode containers, and reserve port `5600` in the admission webhook (`modelDeploymentRouterReservedPorts`) on both prefill and decode roles when `backend: NIXL` is active.
- **Combining NIXL remote weight delivery (`model-transfer`) with an EFA `KVCache` pool or `kvTransfer.protocol: EFA` fails interface resolution** → Teach `ModelDeploymentDirectInterfaceProtocol` to prefer `"efa"` over `"rdma"` when `spec.kvTransfer.protocol == "EFA"` or when the role's cache store protocol is `"efa"`.
- **Ascend Device RoCE fails if only one role sets `HCCL_INTRA_ROCE_ENABLE=1` or `protocol_desc: "roce:device"`** → Render both `ASCEND_GLOBAL_RESOURCE_CONFIG` and `HCCL_INTRA_ROCE_ENABLE=1` symmetrically from deployment-wide `spec.kvTransfer.protocol: RDMA` onto both prefill and decode roles when `backend: HIXL` is set.

## Design Details

### Commands

Run code generation, unit tests, and linters locally from the repository root on macOS (`darwin/arm64`):

```bash
make generate
GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/kvcache/inject/... ./pkg/worker/kvcache/router/... -run 'TestRender|TestVLLMConfig'
GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/controllers/worker/... ./pkg/worker/webhooks/worker/... -run 'TestSynthesizeModelDeploymentConnector|TestModelDeploymentConnector|TestRenderModelDeployment|TestModelDeploymentDirectInterfaceProtocol|TestModelTransfer|TestModelDeploymentOwnedAndDefaultedCannotDisagree|TestModelDeploymentWebhook|TestValidateModelDeployment'
(cd model-transfer/python && uv run pytest)
make lint
make lint python
make lint docs
```

### Project Structure

| Area | Responsibility |
|---|---|
| `api/worker/v1` and `api/worker/v1alpha1` | Defines `ModelDeploymentKVTransferBackend` (`Auto`, `Mooncake`, `NIXL`, `HIXL`) and `ModelDeploymentKVTransfer.Backend`, plus generated deepcopy, conversion, protobuf, applyconfiguration, and CRD schemas. |
| `pkg/worker/kvcache/inject` | Synthesizes engine-specific KV transfer flags, JSON documents, environment variables, ports, and host mounts for vLLM, vLLM-Ascend, and SGLang across `Mooncake`, `NIXL`, and `HIXL`. |
| `pkg/worker/kvcache/router` | Renders `vllm-router` argv with configurable `Input.KVConnector` (`mooncake` default vs `nixl`). |
| `pkg/worker/controllers/worker` | Resolves fabric interface grants (`device.gpustack.ai/rdma*` vs `vpc.amazonaws.com/efa`) ahead of connector synthesis, owns `ModelDeploymentEffectiveKVTransferBackend` and the owned/defaulted key tables, wires router handshakes (`nixl` / `nixlv2`), and defaults `GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND` in `(ModelDeploymentArtifactRender).applyModelTransfer`. |
| `pkg/worker/webhooks/worker` | Validates `spec.kvTransfer.backend` against role `InstanceType` manufacturers (including mixed-manufacturer pair refusal) and reserves NIXL side-channel port `5600` on vLLM prefill and decode roles. |
| `model-transfer/python/gpustack_model_transfer` | Selects `UCX` vs `LIBFABRIC` NIXL backend on both source and target agents via `GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND` or `/sys/class/infiniband` EFA detection. |
| `docs/modules/model-deployment` | Documents `spec.kvTransfer.backend` (`NIXL`, `HIXL`, `Mooncake`, `Auto`), EFA `LIBFABRIC` switching, and owned/defaulted keys in `prefill-decode.md` and `configuration.md`. |

### Code Style

Follow existing Operator conventions: `snake_case` multi-word Go filenames, explicit error handling, plain comments without decorative symbols, and deterministic JSON/argv rendering:

```go
func ModelDeploymentEffectiveKVTransferBackend(
	md *workercore.ModelDeployment, manufacturer string,
) string {
	backend := ""
	if md != nil && md.Spec.KVTransfer != nil {
		backend = strings.TrimSpace(md.Spec.KVTransfer.Backend)
	}
	if manufacturer == nodefeature.ManufacturerAscend {
		if backend == "" || backend == workercore.ModelDeploymentKVTransferBackendAuto {
			return workercore.ModelDeploymentKVTransferBackendMooncake
		}
		return backend
	}
	if backend == "" || backend == workercore.ModelDeploymentKVTransferBackendAuto {
		return workercore.ModelDeploymentKVTransferBackendMooncake
	}
	return backend
}
```

### Architecture and Resolution Matrix

```text
ModelDeployment.spec.kvTransfer
  +-- backend: Auto | Mooncake | NIXL | HIXL
  +-- protocol: Auto | TCP | RDMA | EFA | CANN | ROCM
         |
         v
1. Fabric & Interface Resolution (pkg/worker/controllers/worker/model_deployment_fabric.go)
   - SGLang & vLLM P/D direct legs resolve "rdma" or "efa"
   - NIXL remoteDelivery ("model-transfer") yields "efa" when kvTransfer.protocol=EFA or store="efa"
   - ModelDeploymentInterfaceResource -> "vpc.amazonaws.com/efa" or "device.gpustack.ai/rdma*"
         |
         v
2. Engine Connector Synthesis (pkg/worker/kvcache/inject/{vllm,sglang}.go)
   +-- vLLM + NIXL (NVIDIA/AMD):
   |     --kv-transfer-config '{"kv_connector":"NixlConnector","kv_role":"kv_producer|kv_consumer",
   |                            "kv_connector_extra_config":{"backends":["UCX"|"LIBFABRIC"]}}'
   |     OwnedEnv: VLLM_NIXL_SIDE_CHANNEL_HOST=status.podIP
   |     DefaultedEnv: VLLM_NIXL_SIDE_CHANNEL_PORT=5600, ContainerPort: 5600 (prefill & decode)
   |     (Placed at connectors[0] when wrapped in MultiConnector with MooncakeStoreConnector)
   +-- SGLang + NIXL (NVIDIA/AMD):
   |     --disaggregation-transfer-backend=nixl --disaggregation-mode=prefill|decode
   |     --disaggregation-bootstrap-port=8998 (prefill)
   |     DefaultedEnv: SGLANG_DISAGGREGATION_NIXL_BACKEND=UCX|LIBFABRIC
   +-- vLLM-Ascend + HIXL (Ascend):
   |     --kv-transfer-config '{"kv_connector":"MooncakeConnectorV1","kv_role":"kv_producer|kv_consumer",
   |                            "kv_port":8998,"kv_connector_extra_config":{"prefill":{...},"decode":{...}}}'
   |     DefaultedEnv: ASCEND_AUTO_CONNECT=1
   |     (+ ASCEND_GLOBAL_RESOURCE_CONFIG='{"comm_resource_config.protocol_desc":"roce:device"}'
   |      + HCCL_INTRA_ROCE_ENABLE=1 when protocol=RDMA)
   +-- SGLang + HIXL/Auto (Ascend):
         --disaggregation-transfer-backend=ascend --disaggregation-mode=prefill|decode
         --disaggregation-bootstrap-port=8998 (prefill)
         DefaultedEnv: ASCEND_MF_TRANSFER_PROTOCOL=sdma (or device_rdma when protocol=RDMA)
         |
         v
3. Router Handshake Selection
   +-- vllm-router: --kv-connector=nixl (for vLLM NIXL & Ascend P/D) | mooncake
   +-- llm-d-router sidecar: --kv-connector=nixlv2 (for vLLM NIXL & Ascend) | mooncake | sglang
```

### Implementation Plan

- [x] **T1 · Add `spec.kvTransfer.backend` API field and regenerate schemas**
      Blocked by: None
      Owns: `api/worker/v1/model_deployment.go`, `api/worker/v1alpha1/model_deployment.go`, `api/worker/v1alpha1/kv_cache_pool_binding.go`, `api/worker/v1alpha1/kv_cache_pool.go`, `api/worker/v1alpha1/kv_cache_backend.go`, `api/worker/v1alpha1/devices.go`, `api/worker/v1/zz_generated.*.go`, `api/worker/v1alpha1/zz_generated.*.go`, `api/worker/v1alpha1/generated.proto`, `api/worker/v1alpha1/generated.pb.go`, `api/worker/zz_generated.openapi.go`, `pkg/kubeclients/applyconfiguration/worker/v1alpha1/**`
      Gate: review
      Acceptance: `ModelDeploymentKVTransfer` defines `Backend` (`Auto`, `Mooncake`, `NIXL`, `HIXL`) in `v1` and `v1alpha1` with constants `ModelDeploymentKVTransferBackendAuto`, `ModelDeploymentKVTransferBackendMooncake`, `ModelDeploymentKVTransferBackendNIXL`, and `ModelDeploymentKVTransferBackendHIXL`; CR and CR field doc comments under `api/` describe API-level semantics without declaring engine-internal CLI flags, environment variable names, or connector class names (preventing unnecessary schema regeneration when engine flags evolve); `make generate` and `make lint` exit 0 with no dirty files.
      Verify: `make generate && make lint`

- [x] **T2 · Implement NIXL, HIXL, and EFA `LIBFABRIC` parameter injection and router argv**
      Blocked by: T1
      Owns: `pkg/worker/kvcache/inject/types.go`, `pkg/worker/kvcache/inject/vllm.go`, `pkg/worker/kvcache/inject/sglang.go`, `pkg/worker/kvcache/inject/*_test.go`, `pkg/worker/kvcache/router/router.go`, `pkg/worker/kvcache/router/argv.go`, `pkg/worker/kvcache/router/argv_test.go`
      Gate: review
      Acceptance: `inject.Input` adds `KVTransferBackend` and `KVTransferEFA`; `inject.VLLM` renders `NixlConnector` (`UCX` default, `LIBFABRIC` on EFA, `VLLM_NIXL_SIDE_CHANNEL_HOST` DownwardAPI, `VLLM_NIXL_SIDE_CHANNEL_PORT=5600`, port `5600` on both prefill and decode, and `NixlConnector` at index 0 in `MultiConnector`) and Ascend `HIXL` (`MooncakeConnectorV1`, `ASCEND_AUTO_CONNECT=1`, plus `ASCEND_GLOBAL_RESOURCE_CONFIG` and `HCCL_INTRA_ROCE_ENABLE=1` on `RDMA`), while keeping omitted/`Auto`/`Mooncake` byte-for-byte unchanged and refusing `NIXL` on `vllm-ascend` or `HIXL` on non-Ascend; `inject.SGLang` renders `nixl` (`SGLANG_DISAGGREGATION_NIXL_BACKEND=UCX|LIBFABRIC`, without `--host`) and Ascend `ascend` (`ASCEND_MF_TRANSFER_PROTOCOL=sdma|device_rdma` + `/usr/local/Ascend/driver` mount, without `--host`); `router.Input` supports `KVConnector` (`mooncake` default, `nixl` override) in both undivided and split shapes; doc comments on the injection and router-argv synthesis functions document the exact engine flags, JSON keys, and environment variables emitted per engine, hardware, and protocol.
      Verify: `CGO_ENABLED=1 go test -race ./pkg/worker/kvcache/inject/... ./pkg/worker/kvcache/router/... -run 'TestRender|TestVLLMConfig'`

- [x] **T3 · Wire controller fabric resolution, ownership tables, router handshakes, and webhook validation**
      Blocked by: T2
      Owns: `pkg/worker/controllers/worker/model_deployment.go`, `pkg/worker/controllers/worker/model_deployment_artifact.go`, `pkg/worker/controllers/worker/model_deployment_connector.go`, `pkg/worker/controllers/worker/model_deployment_fabric.go`, `pkg/worker/controllers/worker/model_deployment_router.go`, `pkg/worker/controllers/worker/model_deployment_render.go`, `pkg/worker/controllers/worker/model_deployment_scaler_prior_identity.go`, `pkg/worker/controllers/worker/model_deployment_transfer.go`, `pkg/worker/controllers/worker/model_deployment*_test.go`, `pkg/worker/webhooks/worker/model_deployment.go`, `pkg/worker/webhooks/worker/model_deployment_test.go`
      Gate: review
      Acceptance: Controller resolves interface protocol/resource before connector synthesis without moving pinned default Pod digests (`TestRenderModelDeploymentPod_SerializedOutputIsPinnedToThePreSplitRender`) so EFA (`vpc.amazonaws.com/efa`) switches NIXL to `LIBFABRIC`; `model_deployment_connector.go` registers `VLLM_NIXL_SIDE_CHANNEL_HOST` in `modelDeploymentOwnedKeys[VLLM].Env` and all six new defaulted env vars in `modelDeploymentDefaultedEnvNames`, with `TestModelDeploymentOwnedAndDefaultedCannotDisagree` extended across `{SGLang, Ascend}` and `NIXL`/`HIXL`; `ModelDeploymentDirectInterfaceProtocol` supports SGLang prefill/decode legs and returns `"efa"` for NIXL remote weight delivery when `kvTransfer.protocol == "EFA"` or store protocol is `"efa"`; `(ModelDeploymentArtifactRender).applyModelTransfer` defaults `GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND=LIBFABRIC|UCX` via `appendModelDeploymentEnvIfAbsent`; `vllm-router` and `llm-d-router` sidecar select `nixl`/`nixlv2` for vLLM `NIXL` and Ascend P/D (including Ascend behind `vllm-router`); synthesis methods carry doc comments detailing engine-specific parameter generation; webhook validates `backend` vs manufacturer (and refuses mixed Ascend/non-Ascend P/D pairs) and reserves port `5600` for vLLM `NIXL`.
      Verify: `CGO_ENABLED=1 go test -race ./pkg/worker/controllers/worker/... ./pkg/worker/webhooks/worker/... -run 'TestSynthesizeModelDeploymentConnector|TestModelDeploymentConnector|TestRenderModelDeployment|TestModelDeploymentDirectInterfaceProtocol|TestModelTransfer|TestModelDeploymentOwnedAndDefaultedCannotDisagree|TestModelDeploymentWebhook|TestValidateModelDeployment' && make lint`

- [x] **T4 · Add EFA `LIBFABRIC` backend selection to Python `gpustack_model_transfer`**
      Blocked by: None
      Owns: `model-transfer/python/gpustack_model_transfer/envs.py`, `model-transfer/python/gpustack_model_transfer/client_native.py`, `model-transfer/python/gpustack_model_transfer/source/backend.py`, `model-transfer/python/tests/**`
      Acceptance: `gpustack_model_transfer` reads `GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND` (`UCX` | `LIBFABRIC`) and auto-detects AWS EFA devices under `/sys/class/infiniband` (PCI vendor `0x1d0f` or `rdmap*` / `efa_*` when no non-EFA IB/RoCE device is present) when unset; passes `{"enable_listen_thread": False, "backends": [backend]}` to `create_agent` in both `NixlSourceBackend` and `NixlTransferEngineClient`; skips `UCX_NET_DEVICES` synthesis when `LIBFABRIC` is active.
      Verify: `make lint python && (cd model-transfer/python && uv run pytest)`

- [ ] **T5 · Add `parameterize.md` and update `ModelDeployment` documentation for `NIXL`, `HIXL`, and EFA `LIBFABRIC`**
      Blocked by: T3, T4
      Owns: `docs/modules/model-deployment/parameterize.md`, `docs/modules/model-deployment/_index.md`, `docs/modules/model-deployment/prefill-decode.md`, `docs/modules/model-deployment/configuration.md`, `docs/README.md`, `site/data/navigation.yaml`, `.agents/skills/gpustack-operator-docs/SKILL.md`, `.agents/skills/gpustack-operator-docs/references/page-map.md`
      Acceptance: Create `docs/modules/model-deployment/parameterize.md` to centralize the `ModelDeployment` parameter injection, ownership, and defaulting tables across all configuration sources (`spec.model`, `spec.roles[]`, `spec.router`, `spec.kvCache`, `spec.kvTransfer`, `ModelArtifact`, `InstanceType`, hardware manufacturer, and fabric protocol) and target components (`vLLM`, `vLLM` on Ascend, `SGLang`, `SGLang` on Ascend, `llm-d-router`, `vllm-router`, `sglang-gateway`, `model-transfer`), distinguishing **Owned** parameters (rejected in `extraArgs`/`env` unless `roles[].command` takes over) from **Defaulted** parameters (overridable via `extraArgs`/`env`); update `prefill-decode.md` and `configuration.md` to document `spec.kvTransfer.backend` (`Auto`, `Mooncake`, `NIXL`, `HIXL`), EFA `LIBFABRIC` auto-switching, and cross-link `parameterize.md`; register `parameterize.md` in `docs/README.md`, `docs/modules/model-deployment/_index.md`, `site/data/navigation.yaml`, and `gpustack-operator-docs` routing tables; `make lint docs` and `check-specs.sh` exit 0.
      Verify: `bash .agents/skills/gpustack-operator-docs/scripts/check-specs.sh && make lint docs`

### Test Plan
[x] I/we understand the owners of the involved components may require updates to existing tests to make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates
None.

#### Unit tests
- `gpustack.ai/gpustack/pkg/worker/kvcache/inject`: `2026-11-10` - covers vLLM `NixlConnector` (`UCX` vs `LIBFABRIC`, `VLLM_NIXL_SIDE_CHANNEL_HOST`/`PORT`, port `5600`, `MultiConnector` ordering), vLLM-Ascend `HIXL` (`MooncakeConnectorV1`, `ASCEND_AUTO_CONNECT=1`, `RDMA` `roce:device` and `HCCL_INTRA_ROCE_ENABLE=1`), and SGLang `nixl` (`SGLANG_DISAGGREGATION_NIXL_BACKEND`) and `ascend` (`ASCEND_MF_TRANSFER_PROTOCOL`, `/usr/local/Ascend/driver` mount, no `--host` flag).
- `gpustack.ai/gpustack/pkg/worker/kvcache/router`: `2026-11-10` - covers `vllm-router` `--kv-connector=nixl` vs `--kv-connector=mooncake` rendering in both undivided and split shapes.
- `gpustack.ai/gpustack/pkg/worker/controllers/worker`: `2026-11-10` - covers EFA interface resolution before connector synthesis, `TestModelDeploymentOwnedAndDefaultedCannotDisagree` across `NIXL`/`HIXL` and `{SGLang, Ascend}`, SGLang direct interface protocol, `model-transfer` EFA fabric resolution and `GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND` defaulting, Ascend P/D behind `vllm-router`, and `llm-d-router` `nixlv2` sidecar rendering.
- `gpustack.ai/gpustack/pkg/worker/webhooks/worker`: `2026-11-10` - covers `spec.kvTransfer.backend` vs manufacturer validation (`HIXL` on non-Ascend, `NIXL` on Ascend, and mixed-manufacturer P/D pairs refused) and port `5600` reservation on vLLM `NIXL` prefill and decode roles.
- `gpustack_model_transfer` (`model-transfer/python/tests`): `2026-11-10` - covers `GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND`, `/sys/class/infiniband` EFA auto-detection, `create_agent` `backends` list, and skipping `UCX_NET_DEVICES` on `LIBFABRIC`.

#### Integration tests
- Controller reconciliation tests in `pkg/worker/controllers/worker/model_deployment_connector_test.go` and `model_deployment_render_test.go` exercising full `ModelDeployment` convergence to rendered `Pod` and router `Deployment` specs across `Mooncake`, `NIXL`, and `HIXL`.

#### e2e tests
- Covered by unit and controller reconciliation tests for parameter injection and manifest synthesis; live hardware transfer requires multi-node GPU/NPU/EFA clusters and is gated separately under `gpustack-operator-benchmark` / `gpustack-operator-e2e`.

## Alternatives

1. **Overload `spec.kvTransfer.protocol` with `NIXL` and `HIXL` values instead of adding `spec.kvTransfer.backend`**:
   - Rejected because transfer backend (`Mooncake`, `NIXL`, `HIXL`) and wire transport (`TCP`, `RDMA`, `EFA`, `CANN`, `ROCM`) are orthogonal dimensions. For example, `NIXL` can run over `RDMA` (`UCX`) or `EFA` (`LIBFABRIC`), and `HIXL` on Ascend can run with automatic link selection (`Auto`/`CANN`) or forced Device RoCE (`RDMA`).
2. **Compile AWS EFA libfabric userspace into default CUDA/ROCm `pack/model-transfer/Dockerfile` images**:
   - Rejected because bundling AWS EFA provider libraries into default RoCE/InfiniBand images risks provider selection conflicts on non-AWS clusters. Keeping the default image build unchanged while allowing EFA users to supply an EFA image via `modelTransfer.images.nvidia` and selecting `LIBFABRIC` at runtime isolates the two environments cleanly.
3. **Default Ascend `backend: HIXL` to the out-of-tree experimental `HIXLConnector` (`vllm_hixl_connector.connector`)**:
   - Rejected because `vllm_hixl_connector` is excluded from default HIXL builds (`--experimental`), is marked non-production upstream, and has a documented ZMQ/late-read hazard. Standard `vllm-ascend` images use `MooncakeConnectorV1` over the `ascend` (ADXL/HIXL) TransferEngine data plane.

## Open Questions

None.
