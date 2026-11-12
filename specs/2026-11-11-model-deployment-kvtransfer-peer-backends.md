# Spec: Align ModelDeployment KV Transfer Peer Backends and AWS EFA Setting

Status: Shipped
Type: Feature

## Summary

Reshape `ModelDeployment.spec.kvTransfer` so `Mooncake`, `NIXL`, and `HIXL` stand as peer KV transfer backends.
Remove `spec.kvTransfer.protocol` and `backend: Auto` so each backend selects the fastest runtime link and falls back to TCP naturally.
Unify the prefill/decode KV handshake base port onto `8998` across `Mooncake`, `NIXL`, and `HIXL` for both vLLM and SGLang.
Add a cluster-level `aws-efa-enabled` `Setting` (`GPUSTACK_AWS_EFA_ENABLED`).
When enabled, `ModelDeployment` requests `vpc.amazonaws.com/efa` and selects `LIBFABRIC` across prefill/decode transfer, remote weight delivery, and multi-Pod `Server` roles.

## Motivation

[2026-11-10-model-deployment-kvtransfer-nixl-hixl.md](2026-11-10-model-deployment-kvtransfer-nixl-hixl.md) added `NIXL` and `HIXL` under `ModelDeployment.spec.kvTransfer.backend`.
It retained `spec.kvTransfer.protocol` (`Auto`, `TCP`, `RDMA`, `EFA`, `CANN`, `ROCM`), `backend: Auto`, and a separate `5600` port constant for vLLM `NIXL`.
That shape leaves four defects in `ModelDeployment`:

1. **Mismatched abstraction levels in `spec.kvTransfer`**:
   - `spec.kvTransfer.protocol` was designed for Mooncake alone and mapped directly to `mooncake_protocol`.
   - `Mooncake`, `NIXL`, and `HIXL` are peer transfer backends. Each backend inspects visible interconnects and chooses the fastest link automatically.
2. **`Auto` pins TCP and blocks fabric interface requests**:
   - In `pkg/worker/kvcache/inject/types.go`, unset or `Auto` `protocol` resolves to `"tcp"`. It injects `MC_FORCE_TCP=1` on vLLM and `--disaggregation-transfer-backend=mooncake_tcp` on SGLang.
   - In `pkg/worker/controllers/worker/model_deployment_fabric.go`, unset or `Auto` `protocol` also resolves to `"tcp"`. If a user sets `roles[].resources.interface > 0` without `protocol: RDMA` or `EFA`, admission rejects the deployment.
3. **AWS EFA (`vpc.amazonaws.com/efa`) vs standard RDMA (`device.gpustack.ai/rdma*`) is a cluster property**:
   - AWS EFA and standard RDMA do not coexist in one cluster. Yet the controller must render Pod device requests and NIXL plugin names (`LIBFABRIC` vs `UCX`) before scheduling.
   - Tying EFA to `spec.kvTransfer.protocol: EFA` forced users to repeat cloud facts on every deployment. It also left single-role `Server` remote weight delivery and multi-Pod vLLM `Server` / `elasticEp` roles hardcoded to `"rdma"`.
4. **Split KV handshake base ports (`5600` vs `8998`)**:
   - SGLang (`--disaggregation-bootstrap-port`), vLLM `Mooncake` (`VLLM_MOONCAKE_BOOTSTRAP_PORT`), and vLLM-Ascend `HIXL` (`kv_port`) all use base port `8998`, whereas vLLM `NIXL` (`VLLM_NIXL_SIDE_CHANNEL_PORT`) uses `5600`.
   - Neither `vllm-router` nor the `llm-d` routing sidecar hardcodes `5600`; `NixlConnector` advertises its configured side-channel port dynamically in `kv_transfer_params`. Maintaining a separate `5600` constant fragments port reservation and network rules unnecessarily.

### Goals

- Remove `Protocol` and `ModelDeploymentKVTransferBackendAuto` from `ModelDeploymentKVTransfer` in `api/worker/v1alpha1` and `api/worker/v1`. Keep `backend` with enum `["Mooncake", "NIXL", "HIXL"]`.
- Default an omitted `spec.kvTransfer` or empty `backend` on prefill/decode pairs to `Mooncake` on non-Ascend pools and `HIXL` on Ascend pools.
- Unify the prefill/decode KV handshake base port onto `8998` across vLLM and SGLang (`Mooncake`, `NIXL`, and `HIXL`), while preserving each engine's listener role scope (`prefill` and `decode` on vLLM `NIXL`; `prefill` only elsewhere).
- Add the editable boolean `Setting` `aws-efa-enabled` (`GPUSTACK_AWS_EFA_ENABLED`, default `false`) in `pkg/worker/settings/value.go`.
- Use `aws-efa-enabled` across `ModelDeployment` to request `vpc.amazonaws.com/efa` for positive `roles[].resources.interface` counts and switch NIXL from `UCX` to `LIBFABRIC`.
- Let `Mooncake`, `NIXL`, and `HIXL` use high-speed interconnects automatically when present and fall back to TCP naturally otherwise.

### Non-Goals

- Preserving backward compatibility for removed `spec.kvTransfer.protocol`, `backend: Auto`, or vLLM `NIXL` default port `5600` (`ModelDeployment` has no existing production workloads to migrate).
- Changing `KVCacheBackend.spec.transport.protocol` or `KVCacheBackendMember.transport` schemas in `api/worker/v1alpha1/kv_cache_backend.go`.
- Modifying default CUDA or ROCm builds in `pack/model-transfer/Dockerfile` or `model-transfer/python/gpustack_model_transfer`.
- Granting host fabric devices to SGLang multi-member `Server` roles or Ascend roles (`Ascend` roles mount `/usr/local/Ascend/driver` instead).

## Proposal

`ModelDeployment.spec.kvTransfer` exposes one optional field, `backend`, with three peer values: `Mooncake`, `NIXL`, and `HIXL`.
If a `prefill` and `decode` pair omits `spec.kvTransfer.backend`, the operator selects `Mooncake` on non-Ascend pools and `HIXL` on Ascend pools.
Every managed prefill/decode backend uses `8998` as its KV handshake base port across vLLM and SGLang.

Administrators on AWS enable `aws-efa-enabled` once per cluster via `GPUSTACK_AWS_EFA_ENABLED=true` or the `Setting` resource.
Workload manifests remain portable across AWS and non-AWS clusters:

- If a role requests `roles[].resources.interface > 0`, the operator grants `vpc.amazonaws.com/efa` when `aws-efa-enabled` is `true` (or when a bound `KVCache` pool uses EFA). Otherwise, it grants `device.gpustack.ai/rdma` or `device.gpustack.ai/rdma.shared`.
- If a prefill/decode pair requests no fabric interfaces (`interface` unset or `0`), the operator requests no fabric device and each backend falls back to TCP.
- When `aws-efa-enabled` is `true` (or EFA is active through a bound `KVCache` pool), NIXL selects `LIBFABRIC` instead of `UCX` across vLLM, SGLang, and `model-transfer`.

### User Stories

#### Story 1
As a user deploying disaggregated prefill/decode services, I want `Mooncake`, `NIXL`, and `HIXL` offered as peer `spec.kvTransfer.backend` choices with hardware-matched defaults, so that I do not configure legacy Mooncake protocol enums.

#### Story 2
As a workload owner, I want `Mooncake`, `NIXL`, and `HIXL` to select the fastest visible interconnect and fall back to TCP naturally when no fabric is present, so that defaults never lock high-speed clusters onto TCP.

#### Story 3
As an administrator or user on AWS, I want one `aws-efa-enabled` `Setting` to confirm AWS EFA, so that `ModelDeployment` allocates `vpc.amazonaws.com/efa` and configures `LIBFABRIC` without conflicting with standard RDMA (`device.gpustack.ai/rdma*` and `UCX`).

### Core Features & Acceptance Criteria

1. **`ModelDeploymentKVTransfer` Schema Simplification**:
   - `ModelDeploymentKVTransfer` in `api/worker/v1alpha1/model_deployment.go` (embedded by `api/worker/v1/model_deployment.go`) drops `Protocol` and limits `Backend` to `["Mooncake", "NIXL", "HIXL"]`.
   - `make generate` updates CRD schemas, OpenAPI definitions, applyconfigurations, and protobuf outputs cleanly.
   - If a declared prefill/decode pair omits `spec.kvTransfer` or `backend`, `ModelDeploymentEffectiveKVTransferBackend` resolves `HIXL` on `ascend` and `Mooncake` on all other or unobserved manufacturers.
   - Admission webhook rejects `HIXL` on observed non-Ascend `InstanceType`s, rejects `NIXL` on observed Ascend `InstanceType`s (recommending `HIXL` or `Mooncake`), and rejects mixed Ascend/non-Ascend prefill/decode pairs even when `spec.kvTransfer` is omitted.
2. **Unified `8998` KV Handshake Base Port**:
   - `VLLMNIXLSideChannelPort` in `pkg/worker/kvcache/inject/vllm.go` changes from `5600` to `8998`, matching `VLLMMooncakeBootstrapPort` (`8998`), vLLM-Ascend `kv_port` (`8998`), and `SGLangBootstrapPort` (`8998`).
   - On vLLM `NIXL`, both `prefill` and `decode` roles render owned `VLLM_NIXL_SIDE_CHANNEL_HOST` (`status.podIP`), defaulted `VLLM_NIXL_SIDE_CHANNEL_PORT=8998`, and container port `8998`. On vLLM `Mooncake` and `HIXL`, only `prefill` opens container port `8998`.
   - On SGLang (`Mooncake`, `NIXL`, and `HIXL`), `prefill` continues to render `--disaggregation-bootstrap-port=8998` and container port `8998` (plus `--encoder-bootstrap-port=8997` when an `encode` role is declared), while per-rank ZMQ pull sockets remain OS-assigned ephemeral ports.
   - Admission webhook reserves port `8998` on both `prefill` and `decode` roles when vLLM sets `backend: NIXL`, and reserves port `8998` on `prefill` for all other routed vLLM and SGLang prefill/decode pairs.
3. **Cluster-Level `aws-efa-enabled` Setting**:
   - `pkg/worker/settings/value.go` declares `AWSEFAEnabled` (`"aws-efa-enabled"`, env `GPUSTACK_AWS_EFA_ENABLED`), defaulting to `"false"` with `setting.AllowBool()`.
   - Both the `ModelDeployment` reconciler and validating webhook read `aws-efa-enabled` from context so admission and rendering resolve the same fabric.
4. **Unified Fabric Interface Resolution**:
   - When `aws-efa-enabled` is `true` (or a bound `KVCache` pool offers `"efa"`), `ModelDeploymentDirectInterfaceProtocol` returns `"efa"` for NIXL remote weight delivery, managed multi-Pod vLLM `Server` roles (`size > 1` or `elasticEp != nil`), and routed vLLM or SGLang prefill/decode pairs where every `Prefill` and `Decode` role requests `resources.interface > 0` (ignoring `Encode` roles).
   - When `aws-efa-enabled` is `false` and no bound `KVCache` pool offers `"efa"`, those three paths return `"rdma"`.
   - If a bound `KVCache` pool and the direct leg resolve to different fabrics (`"rdma"` vs `"efa"`), `ModelDeploymentInterfaceResource` rejects the deployment.
   - If only one half of a prefill/decode pair requests `resources.interface > 0` without another fabric consumer (`KVCache` store or remote weight delivery), direct P/D fabric stays inactive on both halves and admission rejects the lone interface request on both `CREATE` and `UPDATE`.
5. **Automatic Link Selection and TCP Fallback per Backend**:
   - **`Mooncake`**:
     - On Ascend (`ascend`), the operator derives `"ascend"` (`MooncakeConnectorV1` on vLLM-Ascend; `--disaggregation-transfer-backend=mooncake` with read-only `/usr/local/Ascend/driver` mount on SGLang). On AMD ROCm (`amd`), when EFA is not active, the operator derives `"hip"` (`mooncake_protocol: "hip"` on vLLM; `--disaggregation-transfer-backend=mooncake` on SGLang) without `MC_FORCE_TCP=1`, letting Mooncake's `MultiTransport` install intra-node `"hip"` alongside cross-node `"rdma"` or `"tcp"` via topology discovery.
     - If every `Prefill` and `Decode` role of a non-Ascend routed pair requests `resources.interface > 0`, vLLM renders `MooncakeConnector` with `mooncake_protocol` set to `"efa"` (under `aws-efa-enabled` or an EFA pool) or `"rdma"` (on NVIDIA) and omits `MC_FORCE_TCP`. SGLang renders `--disaggregation-transfer-backend=mooncake`.
     - Otherwise on NVIDIA, vLLM renders `mooncake_protocol: "tcp"` and SGLang renders `--disaggregation-transfer-backend=mooncake_tcp`. When no non-TCP `KVCache` store shares the process, the renderer supplies defaulted `MC_FORCE_TCP=1`, `MC_TCP_*` tuning variables, and vLLM `--block-size 128` or SGLang `--page-size 64`.
   - **`NIXL`**:
     - When `aws-efa-enabled` is `true` or EFA is active on the role, vLLM renders `NixlConnector` with `backends: ["LIBFABRIC"]` and SGLang sets defaulted `SGLANG_DISAGGREGATION_NIXL_BACKEND=LIBFABRIC`. Otherwise, both select `UCX`.
     - Target containers using NIXL `model-transfer` remote weight delivery (including `ModelExpress` `Node`/`Engine` seed and fallback replicas) receive defaulted `GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND=LIBFABRIC` when `aws-efa-enabled` is `true` or EFA is active, and `UCX` otherwise, even when `resources.interface` is `0`.
   - **`HIXL`**:
     - vLLM-Ascend renders `MooncakeConnectorV1` (`kv_port: 8998`, `prefill`/`decode` parallelism blocks, read-only `/usr/local/Ascend/driver` mount) and defaulted `ASCEND_AUTO_CONNECT=1`.
     - SGLang on Ascend renders `--disaggregation-transfer-backend=ascend`, mounts `/usr/local/Ascend/driver` read-only, and sets defaulted `ASCEND_MF_TRANSFER_PROTOCOL=sdma`.
6. **Documentation and Parameter Injection Reference**:
   - Update `docs/modules/model-deployment/parameterize.md` (`Transfer parameters` section and table) to document injected flags, env vars, mounts, and the unified `8998` handshake port across `Mooncake`, `NIXL` (`VLLM_NIXL_SIDE_CHANNEL_PORT=8998` overriding upstream's `5600` default, open on both `prefill` and `decode`), and `HIXL` for vLLM and SGLang.
   - Document `aws-efa-enabled` (`GPUSTACK_AWS_EFA_ENABLED`, `Effect window: Next trigger`) in `docs/reference/settings.md`.
   - Update `docs/modules/model-deployment/{prefill-decode,configuration,engine-versions,elastic-ep,prefill-decode-benchmark,encode-prefill-decode-benchmark}.md`, `docs/modules/rdma/operations.md`, `docs/walkthroughs/model-deployment/{prefill-decode,encode-prefill-decode}.md`, `docs/walkthroughs/rdma/network-endpoints.md`, `docs/README.md`, and the stale `spec.kvTransfer.protocol` references in `.agents/skills/gpustack-operator-benchmark/SKILL.md` and `.agents/skills/gpustack-operator-e2e/cases/{case-91.sh,case-115.sh}`.

### Notes / Constraints / Caveats

- AWS EFA device requests always use `vpc.amazonaws.com/efa` (`efaDeviceResource` in `pkg/worker/kvcache/mooncake/member_workload.go`).
- Under multi-GPU or data-parallel execution inside one Pod, vLLM `NixlConnector` binds `[base_port, base_port + dp_size)` and vLLM-Ascend `MooncakeConnectorV1` binds `[kv_port, kv_port + dp_size * pp_size * pcp_size * tp_size)` from base port `8998`, while SGLang binds `8998` on `prefill` and OS-ephemeral ZMQ worker ports. Each Pod owns a distinct IP, and user `roles[].env` can still override `VLLM_NIXL_SIDE_CHANNEL_PORT` when needed.
- Toggling `aws-efa-enabled` updates the Pod spec hash (`modelDeploymentBindingHash`) for affected `ModelDeployment` Pods and rolls them on the next reconcile (`Effect window: Next trigger`).
- Removing `protocol: RDMA` stops `inject` from emitting `ASCEND_GLOBAL_RESOURCE_CONFIG` and `HCCL_INTRA_ROCE_ENABLE`. Keep both exported constant names in `pkg/worker/kvcache/inject/vllm.go` for `modelDeploymentScalerCapacityNeutralEnvNames`, and drop them from `modelDeploymentDefaultedEnvNames` in `pkg/worker/controllers/worker/model_deployment_connector.go` so `TestModelDeploymentOwnedAndDefaultedCannotDisagree` stays exact; users can still set them in `roles[].env`.
- Defaulting `ModelDeploymentEffectiveKVTransferBackend` to `"Mooncake"` / `"HIXL"` populates `binding.KVTransferBackend` on routed pairs and adds defaulted `ASCEND_AUTO_CONNECT=1` on vLLM-Ascend. Bump `modelDeploymentRenderVersion` from `4` to `5` in `pkg/worker/controllers/worker/model_deployment_render.go` and re-baseline `pkg/worker/controllers/worker/testdata/legacy-render/` plus `TestRenderModelDeploymentPod_SerializedOutputIsPinnedToThePreSplitRender`.
- vLLM EPD's `ECCPUConnector` (`pkg/worker/kvcache/inject/vllm_ec.go`) carries no `backends` list in `ec_connector_extra_config` and stays unchanged.
- Default `pack/model-transfer/Dockerfile` images stay unchanged. On AWS EFA, administrators set `worker.env.GPUSTACK_AWS_EFA_ENABLED: "true"` and configure an EFA-capable image via `modelTransfer.images.nvidia` when using remote weight delivery.

### Boundaries

- **Always:** Run `make generate` after `make lint` when editing API types; keep `TestModelDeploymentOwnedAndDefaultedCannotDisagree` aligned with `inject`; update `docs/modules/model-deployment/parameterize.md` alongside peer guides and `docs/README.md`; run `make lint`, `make lint docs`, and `make lint agents-shell`.
- **Ask first:** Editing Helm chart `values.schema.json`, changing `KVCacheBackend` transport schemas, or modifying `pack/model-transfer/Dockerfile`.
- **Never:** Reintroduce `Auto` on `ModelDeploymentKVTransfer`, reintroduce port `5600` for vLLM `NIXL`, emit `--host` from SGLang connector synthesis, hardcode `aws.com/efa`, or set `MC_FORCE_TCP=1` when both halves of a Mooncake pair request fabric interfaces.

### Risks and Mitigations

- **Mooncake C++ `TransferEngine` picks broken cross-host NVLink on GPU hosts without RDMA** → When `Mooncake` runs without positive interface requests on both P/D halves and without a non-TCP store, retain defaulted `MC_FORCE_TCP=1` and `mooncake_tcp`. Clear that pin automatically when both halves request `interface > 0`.
- **Target or `ModelExpress` fallback Pod with `interface: 0` mismatches source `LIBFABRIC` agent on AWS EFA** → Propagate `ModelDeploymentArtifactRender.ModelTransferEFA` from `aws-efa-enabled` across `Remote` and `ModelExpress` `Node`/`Engine` fallback artifacts so target Pods default `GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND=LIBFABRIC`.
- **Stale entries in `modelDeploymentDefaultedEnvNames` fail `TestModelDeploymentOwnedAndDefaultedCannotDisagree`** → Remove `ASCEND_GLOBAL_RESOURCE_CONFIG` and `HCCL_INTRA_ROCE_ENABLE` from `modelDeploymentDefaultedEnvNames` in the same task as `inject`, while retaining their constant declarations for scaler prior capacity-neutrality.
- **Asymmetric `resources.interface` across `prefill` and `decode` desynchronizes Mooncake transfer flags or bypasses `UPDATE` re-judging** → Derive pair-level Mooncake fabric protocol only when every `Prefill` and `Decode` role requests `resources.interface > 0`, and trigger sibling re-judging in `modelDeploymentRoleNeedsInterfaceRejudge` whenever pair-level interface presence changes.

## Design Details

### Commands

Run code generation, unit tests, golden YAML conversion checks, and linters in the local Go workspace from the repository root (no Kubernetes cluster required):

```bash
CGO_ENABLED=1 go test -race ./pkg/worker/settings/...
CGO_ENABLED=1 go test -race ./pkg/worker/kvcache/inject/... ./pkg/worker/kvcache/router/... -run 'TestRender|TestVLLMConfig|TestSGLang'
CGO_ENABLED=1 go test -race ./pkg/worker/controllers/worker/... -run 'TestModelDeploymentGoldenYAML|TestModelDeploymentLegacyRender'
CGO_ENABLED=1 go test -race ./pkg/worker/controllers/worker/... ./pkg/worker/webhooks/worker/... -run 'TestSynthesizeModelDeploymentConnector|TestModelDeploymentConnector|TestRenderModelDeployment|TestModelDeploymentLegacyRender|TestModelDeploymentDirectInterfaceProtocol|TestModelTransfer|TestModelDeploymentModelExpress|TestModelDeploymentOwnedAndDefaultedCannotDisagree|TestModelDeploymentWebhook|TestValidateModelDeployment'
CGO_ENABLED=1 go test -race ./api/worker/v1alpha1/... -run 'TestModelDeploymentEnumSpellings'
make lint
make generate
make lint docs
make lint agents-shell
```

### Project Structure

| Area | Responsibility |
|---|---|
| `api/worker/v1alpha1` and `api/worker/v1` | Defines `ModelDeploymentKVTransfer` with `Backend` (`Mooncake`, `NIXL`, `HIXL`) and generated CRD, OpenAPI, protobuf, and applyconfiguration artifacts. |
| `pkg/worker/settings` | Registers `AWSEFAEnabled` (`aws-efa-enabled` / `GPUSTACK_AWS_EFA_ENABLED`). |
| `pkg/worker/kvcache/inject` | Renders vLLM, vLLM-Ascend, and SGLang connector arguments, JSON configs, unified `8998` handshake ports, mounts, and env vars for `Mooncake`, `NIXL`, and `HIXL`. |
| `pkg/worker/controllers/worker` | Resolves `ModelDeploymentEffectiveKVTransferBackend`, derives pair-level Mooncake fabric and `"efa"` vs `"rdma"` via `aws-efa-enabled`, wires `ModelExpress` / `model-transfer` EFA state, bumps `modelDeploymentRenderVersion` to `5`, and owns the pretty-YAML `ModelDeployment` to `Pod` / `Service` golden suite under `pkg/worker/controllers/worker/testdata/golden-render/`. |
| `pkg/worker/webhooks/worker` | Validates `spec.kvTransfer.backend` and mixed-vendor pairs against `InstanceType` manufacturers, checks `CREATE`/`UPDATE` interface requests under `aws-efa-enabled`, and reserves handshake port `8998`. |
| `docs/` and `.agents/skills/` | Documents `aws-efa-enabled` in `docs/reference/settings.md`, updates `docs/modules/model-deployment/parameterize.md`, peer guides, walkthroughs, `docs/README.md`, and e2e/benchmark skill references. |

### Code Style

Follow repository Go conventions (`snake_case` filenames, explicit error handling, short plain comments without decorative symbols, and pure helpers):

```go
// ModelDeploymentEffectiveKVTransferBackend resolves the point-to-point transfer backend for
// a prefill/decode pair from spec.kvTransfer.backend and the pool manufacturer.
func ModelDeploymentEffectiveKVTransferBackend(
	md *workercore.ModelDeployment, manufacturer string,
) string {
	if !ModelDeploymentDeclaresBothHalves(md) {
		return ""
	}
	if md.Spec.KVTransfer != nil {
		switch md.Spec.KVTransfer.Backend {
		case "":
		case workercore.ModelDeploymentKVTransferBackendMooncake:
			return inject.KVTransferBackendMooncake
		case workercore.ModelDeploymentKVTransferBackendNIXL:
			return inject.KVTransferBackendNIXL
		case workercore.ModelDeploymentKVTransferBackendHIXL:
			return inject.KVTransferBackendHIXL
		default:
			return ""
		}
	}
	if manufacturer == nodefeature.ManufacturerAscend {
		return inject.KVTransferBackendHIXL
	}
	return inject.KVTransferBackendMooncake
}
```

### Implementation Plan

Tasks start with a prerequisite pretty-YAML golden baseline on the unmodified renderer, then follow an expand–migrate–contract sequence so every commit compiles and passes `go test` and `make lint` independently (`{T1 ∥ T2} → T3 → {T4 ∥ T5}`).

- [x] **T1 · Establish pretty-YAML golden baseline for `ModelDeployment` to `Pod` and `Service` conversions**
  - **Blocked by:** None
  - **Owns:** `pkg/worker/controllers/worker/model_deployment_golden_yaml_test.go`, `pkg/worker/controllers/worker/testdata/golden-render/**`
  - **Acceptance:** Every functional `ModelDeployment` scenario—aggregated `Server` (vLLM, SGLang, `command` take-over, custom ports/TLS/volumes/topology/grace/startup timeout, sliced and partitioned accelerators, multi-Pod internal collective with headless peer `Service`, External DP, multi-Pod RDMA/EFA interfaces, `elasticEp` Ray head + workers, router-fronted `Server`), shared `KVCache` pool attachment (TCP/RDMA/EFA), `Prefill`/`Decode` disaggregation across vLLM and SGLang on NVIDIA/AMD/Ascend (`Mooncake`, `NIXL`, `HIXL`, `llm-d-router`, `vllm-router`, `sglang-gateway`, unrouted `KVCache` pair, and `MultiConnector` composition), multimodal `Encode`/`Prefill`/`Decode` (NVIDIA NIXL network leg, Ascend shared PVC leg, SGLang EPD), and `ModelArtifact` weight delivery (`Node` CSI, `Engine` hub, `Remote` `model-transfer` NIXL/HIXL/EFA, and `ModelExpress` peer D2D + server `Service`)—renders deterministically through `ModelDeploymentReconciler` into pretty multi-document YAML (`sigs.k8s.io/yaml`: input `ModelDeployment` followed by owned `Pod`s and `Service`s) recorded under `pkg/worker/controllers/worker/testdata/golden-render/*.yaml` via `-update` and asserted byte-for-byte on current `HEAD`.
  - **Verify:** `CGO_ENABLED=1 go test -race ./pkg/worker/controllers/worker/... -run 'TestModelDeploymentGoldenYAML|TestModelDeploymentLegacyRender' && make lint`

- [x] **T2 · Register `aws-efa-enabled` cluster Setting**
  - **Blocked by:** None
  - **Owns:** `pkg/worker/settings/**`
  - **Acceptance:** `AWSEFAEnabled` (`"aws-efa-enabled"`, env `GPUSTACK_AWS_EFA_ENABLED`, default `"false"`, `setting.AllowBool()`) is registered in `pkg/worker/settings/value.go` and covered for default value, boolean validation, and delegated `Secret` override in `pkg/worker/settings/value_test.go`.
  - **Verify:** `CGO_ENABLED=1 go test -race ./pkg/worker/settings/... && make lint`

- [x] **T3 · Migrate `inject`, controller rendering, webhook admission, and re-verify golden YAML conversions**
  - **Blocked by:** T1, T2
  - **Gate:** `review`
  - **Owns:** `pkg/worker/kvcache/inject/**`, `pkg/worker/controllers/worker/**`, `pkg/worker/webhooks/worker/**`
  - **Acceptance:**
    1. `pkg/worker/kvcache/inject` sets `VLLMNIXLSideChannelPort = 8998`, stops emitting `ASCEND_GLOBAL_RESOURCE_CONFIG` and `HCCL_INTRA_ROCE_ENABLE` into `DefaultedEnv` (retaining both exported constants for scaler prior capacity-neutrality), and preserves `ASCEND_MF_TRANSFER_PROTOCOL=sdma` default on SGLang `HIXL`.
    2. `pkg/worker/controllers/worker` stops reading `Spec.KVTransfer.Protocol` and `ModelDeploymentKVTransferBackendAuto`; resolves `ModelDeploymentEffectiveKVTransferBackend` to `HIXL` on `ascend` and `Mooncake` elsewhere; enforces vendor validation for both `in.KVTransfer` and SGLang `in.Disaggregated`; derives Mooncake fabric from pair-level `ModelDeploymentDirectPairRequestsInterfaces` (`Prefill` and `Decode` only) and `aws-efa-enabled`; threads `aws-efa-enabled` into `ModelDeploymentDirectInterfaceProtocol`, `Remote` weight delivery, and `modelExpressStagingFor` `Node`/`Engine` fallback replicas; removes `ASCEND_GLOBAL_RESOURCE_CONFIG` and `HCCL_INTRA_ROCE_ENABLE` from `modelDeploymentDefaultedEnvNames`; bumps `modelDeploymentRenderVersion` from `4` to `5`; and updates both `pkg/worker/controllers/worker/testdata/legacy-render/` and `pkg/worker/controllers/worker/testdata/golden-render/*.yaml` (migrating EFA/peer-backend scenarios onto `aws-efa-enabled` and verifying zero unintended YAML diff on unaffected scenarios).
    3. `pkg/worker/webhooks/worker` wires `settings.AWSEFAEnabled.ShouldValueBool(ctx)` into `validateModelDeploymentInterfaceRequests` and `modelDeploymentRoleNeedsInterfaceRejudge` (re-judging untouched siblings when pair-level interface presence flips on `UPDATE`), reserves port `8998` (`prefill` + `decode` for vLLM `NIXL`, `prefill` elsewhere), rejects mixed Ascend/non-Ascend pairs even when `spec.kvTransfer` is nil, and drops `"or Auto"` from the `NIXL`-on-Ascend refusal message.
  - **Verify:**
    - `CGO_ENABLED=1 go test -race ./pkg/worker/kvcache/inject/... ./pkg/worker/kvcache/router/... -run 'TestRender|TestVLLMConfig|TestSGLang'`
    - `CGO_ENABLED=1 go test -race ./pkg/worker/controllers/worker/... ./pkg/worker/webhooks/worker/... -run 'TestModelDeploymentGoldenYAML|TestSynthesizeModelDeploymentConnector|TestModelDeploymentConnector|TestRenderModelDeployment|TestModelDeploymentLegacyRender|TestModelDeploymentDirectInterfaceProtocol|TestModelTransfer|TestModelDeploymentModelExpress|TestModelDeploymentOwnedAndDefaultedCannotDisagree|TestModelDeploymentWebhook|TestValidateModelDeployment'`
    - `make lint`

- [x] **T4 · Contract `ModelDeploymentKVTransfer` API schema and regenerate artifacts**
  - **Blocked by:** T3
  - **Gate:** `review`
  - **Owns:** `api/worker/v1alpha1/**`, `api/worker/zz_generated.openapi.go`, `pkg/kubeclients/applyconfiguration/worker/v1alpha1/**`
  - **Acceptance:** `ModelDeploymentKVTransfer.Protocol` and `ModelDeploymentKVTransferBackendAuto` are removed in `api/worker/v1alpha1/model_deployment.go`, `+k8s:validation:enum` on `Backend` is narrowed to `["Mooncake","NIXL","HIXL"]` with plain API doc comments compliant with `hack/check/api-descriptions.sh`, `api/worker/v1alpha1/model_deployment_test.go` asserts `spec.kvTransfer.backend` (`"Mooncake"`, `"NIXL"`, `"HIXL"`, rejecting `"Auto"`) and absence of `protocol`, and `make generate` produces clean generated diffs.
  - **Verify:** `CGO_ENABLED=1 go test -race ./api/worker/v1alpha1/... -run 'TestModelDeploymentEnumSpellings' && make lint`

- [x] **T5 · Update documentation, walkthroughs, shared index, and skill fixtures**
  - **Blocked by:** T3
  - **Owns:** `docs/**`, `.agents/skills/gpustack-operator-benchmark/SKILL.md`, `.agents/skills/gpustack-operator-e2e/cases/**`
  - **Acceptance:** `docs/modules/model-deployment/parameterize.md` documents peer backends (`Mooncake`, `NIXL`, `HIXL`) and unified port `8998`; `docs/reference/settings.md` adds `aws-efa-enabled` (`GPUSTACK_AWS_EFA_ENABLED`, `Effect window: Next trigger`); `ModelDeployment` and RDMA guides/walkthroughs, `docs/README.md`, `.agents/skills/gpustack-operator-benchmark/SKILL.md`, `case-91.sh`, and `case-115.sh` carry no stale `spec.kvTransfer.protocol` or `Auto` references.
  - **Verify:** `make lint docs && make lint agents-shell`

### Test Plan

[x] I/we understand the owners of the involved components may require updates to
existing tests to make this code solid enough prior to committing the changes necessary
to implement this enhancement.

##### Prerequisite testing updates

- Before changing production code (T1), add `pkg/worker/controllers/worker/model_deployment_golden_yaml_test.go` and record pretty multi-document YAML golden fixtures under `pkg/worker/controllers/worker/testdata/golden-render/*.yaml` covering every `ModelDeployment` to `Pod` and `Service` functional scenario on current `HEAD`.
- In T3, bump `modelDeploymentRenderVersion` from `4` to `5` and re-baseline both `pkg/worker/controllers/worker/testdata/legacy-render/` and `pkg/worker/controllers/worker/testdata/golden-render/`, inspecting the YAML diff to verify exact `Pod` and `Service` transformations without a live cluster.

##### Unit tests

- `pkg/worker/settings` (`value_test.go`):
  - Verify `AWSEFAEnabled` registers `"aws-efa-enabled"` with env `GPUSTACK_AWS_EFA_ENABLED`, defaults to `false`, rejects non-boolean strings, and reads delegated `Secret` overrides via `ShouldValueBool`.
- `pkg/worker/kvcache/inject` (`inject_test.go`, `sglang_test.go`, `vllm_ascend_test.go`):
  - Verify `VLLMNIXLSideChannelPort` equals `8998`, vLLM `NIXL` renders `VLLM_NIXL_SIDE_CHANNEL_PORT=8998` and container port `8998` on both `prefill` and `decode` (`UCX` vs `LIBFABRIC` when `KVTransferEFA` is true), Mooncake switches between `mooncake_protocol: "rdma"`/`"efa"` (`mooncake` on SGLang, no `MC_FORCE_TCP`) and `"tcp"` (`mooncake_tcp` + `MC_FORCE_TCP=1`), and `HIXL` emits `ASCEND_AUTO_CONNECT=1` (vLLM-Ascend) / `ASCEND_MF_TRANSFER_PROTOCOL=sdma` (SGLang) without `ASCEND_GLOBAL_RESOURCE_CONFIG` or `HCCL_INTRA_ROCE_ENABLE`.
- `pkg/worker/controllers/worker` (`model_deployment_golden_yaml_test.go`, `model_deployment_connector_test.go`, `model_deployment_legacy_render_test.go`, `model_deployment_render_test.go`, `model_deployment_transfer_test.go`):
  - Verify full-scenario `ModelDeployment` to `Pod` / `Service` pretty-YAML golden conversions (`TestModelDeploymentGoldenYAML`); `ModelDeploymentEffectiveKVTransferBackend` defaults omitted/empty `backend` to `HIXL` on `ascend` and `Mooncake` elsewhere; `ModelDeploymentDirectInterfaceProtocol` resolves `"efa"` vs `"rdma"` from `aws-efa-enabled` across routed pairs (both `Prefill` and `Decode` requesting `interface > 0`, ignoring `Encode`), multi-Pod vLLM `Server` / `elasticEp` roles, and NIXL remote weight delivery; `ModelExpress` `Node`/`Engine` fallback and `Remote` Pods default `GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND=LIBFABRIC` when `aws-efa-enabled` is `true`; `TestModelDeploymentOwnedAndDefaultedCannotDisagree` and re-baselined golden render fixtures pass; and toggling `aws-efa-enabled` changes `pod-spec-hash` for affected roles.
- `pkg/worker/webhooks/worker` (`model_deployment_test.go`):
  - Verify port `8998` reservation (`prefill` + `decode` on vLLM `NIXL`; `prefill` elsewhere), vendor validation (`HIXL` rejected off Ascend, `NIXL` rejected on Ascend without mentioning `Auto`, mixed Ascend/non-Ascend pairs rejected even when `spec.kvTransfer` is nil), and `CREATE`/`UPDATE` interface validation under `aws-efa-enabled` (including rejecting a stranded sibling role when one P/D half drops `resources.interface` to `0`).
- `api/worker/v1alpha1` (`model_deployment_test.go`):
  - Verify `ModelDeployment` CRD OpenAPI schema exposes `spec.kvTransfer.backend` enum `["Mooncake", "NIXL", "HIXL"]`, omits `Auto`, and omits `spec.kvTransfer.protocol`.

##### Integration tests

Covered by `TestModelDeploymentGoldenYAML` (full fake-client `ModelDeploymentReconciler` `ModelDeployment` → `Pod` + `Service` pretty-YAML snapshot suite across all functional topologies) and `pkg/worker/webhooks/worker` admission suites (`ValidateCreate` / `ValidateUpdate`).

##### e2e tests

- Not required for this change: `TestModelDeploymentGoldenYAML` freezes and diffs the complete rendered `Pod` and `Service` YAML across every `ModelDeployment` topology locally without a live cluster.
- Update `.agents/skills/gpustack-operator-e2e/cases/case-91.sh` and `.agents/skills/gpustack-operator-e2e/cases/case-115.sh` in T5 so neither script sets or asserts removed `.spec.kvTransfer.protocol`.

## Alternatives

1. **Put `efa: bool` inside `ModelDeployment.spec.kvTransfer` or `ModelDeployment.spec`**:
   - Rejected because AWS EFA vs standard RDMA is a cluster infrastructure fact. Workload-level flags force repetition in every manifest, break portability across clusters, and leave single-role `Server` deployments uncovered if placed under `spec.kvTransfer`.
2. **Default non-Ascend prefill/decode pairs to `NIXL` when `backend` is omitted**:
   - Rejected because vLLM `NIXL` reserves port `8998` on both `prefill` and `decode` roles, whereas `Mooncake` and `HIXL` reserve `8998` on `prefill` alone. Defaulting to `Mooncake` (non-Ascend) and `HIXL` (Ascend) keeps admission port reservation on `decode` roles deterministic before `InstanceType` hardware status converges.
3. **Keep vLLM `NIXL` on upstream default port `5600` instead of `8998`**:
   - Rejected because neither `vllm-router` nor `routing-proxy` hardcodes `5600`, and keeping a separate `5600` constant forces admission, injection, firewall rules, and `docs/modules/model-deployment/parameterize.md` to maintain two handshake base ports without functional benefit.
4. **Keep `spec.kvTransfer.protocol` for Ascend Device RoCE overrides**:
   - Rejected because `HIXL` already sets `ASCEND_AUTO_CONNECT=1` for automatic HCCS and RoCE link selection. Users who must force intra-rack Device RoCE can set `ASCEND_GLOBAL_RESOURCE_CONFIG` and `HCCL_INTRA_ROCE_ENABLE` (or `ASCEND_MF_TRANSFER_PROTOCOL`) in `roles[].env`.

## Open Questions

None.
