# Spec: ModelExpress Peer D2D Weight Delivery, Compile Cache Sharing, and Source Fallback

Status: Shipped
Type: Feature

- **Issue**: https://github.com/gpustack/gpustack-operator/issues/809
- **Related Specs**: [2026-10-28-model-artifact-remote-weight-loading.md](2026-10-28-model-artifact-remote-weight-loading.md)

## Summary

Add managed NVIDIA Dynamo ModelExpress (`--load-format modelexpress` and `--load-format mx`) support for vLLM and SGLang roles in `ModelDeployment`.
Reconcile a per-deployment in-memory ModelExpress gRPC metadata server (`<md-name>-modelexpress`, `MX_METADATA_BACKEND=memory`) and `ClusterIP` `Service` on port `8001`.
Enable a three-tier loading chain (`RdmaStrategy -> GpustackLoadStrategy -> ServerCacheStrategy -> ... -> DefaultStrategy`) with NIXL Peer Device-to-Device (D2D) tensor streaming and NIXL compile cache transfer (`MX_ARTIFACT_TRANSFER=1`).
When no ready peer replica exists (seed replica or after an in-memory metadata-server restart), fall back to `GpustackLoadStrategy` over `model-transfer` (`ModelPrefetch` source) or to `DefaultStrategy` under `Prefer`, then publish GPU tensor descriptors and compiled cache manifests so subsequent peer replicas scale out via NIXL D2D.

## Motivation

`ModelDeployment` supports remote source weight delivery (`--load-format model-transfer`) from a `ModelPrefetch`-selected source node over NIXL or HIXL (`H2H -> H2D`). However, when a `ModelDeployment` scales out vLLM or SGLang replicas on NIXL-capable GPUs using ModelExpress (`--load-format modelexpress` or `--load-format mx`), three gaps previously prevented end-to-end Peer Replica D2D weight transfer, compile cache sharing, and automatic fallback to source-based `model-transfer`:

1. **No Operator-managed ModelExpress metadata service**: Replicas that run with `--load-format modelexpress` need a shared ModelExpress gRPC metadata server (`MX_SERVER_ADDRESS`) to register published GPU tensor descriptors and `torch.compile` / Inductor / Triton cache manifests (`MX_ARTIFACT_TRANSFER=1`). Without an Operator-reconciled metadata server per `ModelDeployment`, peer replicas cannot discover a seed replica.
2. **Incomplete Peer D2D and compile cache pipeline in `gpustack_model_transfer`**:
   - The earlier Python adapter in `model-transfer/python/gpustack_model_transfer/adapters/modelexpress` registered an artificial `--load-format gpustack-modelexpress` instead of intercepting standard `--load-format modelexpress` and `mx` for both vLLM and SGLang.
   - The adapter inserted `GpustackLoadStrategy` before `rdma` (`INSERT_BEFORE = "rdma"`), set `ctx.p2p_enabled = False`, and set `result.publishable = False`. That ordering disabled ModelExpress Peer D2D (`RdmaStrategy`) and prevented a seed replica that loaded weights via `model-transfer` or local cache from publishing its GPU tensors and compile cache to subsequent peer replicas.
   - `GpustackLoadStrategy` only handled vLLM (`VllmAdapter`) and did not support SGLang (`SglangAdapter`).
   - When `GpustackLoadStrategy` ran on a seed replica where no `ModelPrefetch` source was ready or configured, calling `load_weights` unconditionally marked `_process_unusable = True` and blocked clean fallback to `DefaultStrategy`.
3. **No Operator wiring for `--load-format modelexpress` roles**:
   - `ModelDeployment` reconciliation treated `--load-format modelexpress` as a conflicting user-supplied `--load-format` in `pkg/worker/controllers/worker/model_deployment_transfer.go`, skipping `gpustack_model_transfer` wheel staging, ServiceAccount token projection, `endpoints.json` discovery `ConfigMap` mounting, and capability injection (`IPC_LOCK`, `SYS_RESOURCE`).
   - Consequently, when a seed replica started (no ready peer) or when the in-memory ModelExpress metadata server restarted and lost state, a `--load-format modelexpress` replica could not fall back to `ModelPrefetch` source delivery (`model-transfer`).

### Goals

- Package the upstream ModelExpress gRPC server (`v0.7.0`, commit `ac6d03a87a550c8188c3ed59f187b90d4f0554f5`) with `--features memory-backend` under `pack/mirrored-modelexpress-server` and expose `model-deployment-modelexpress-server-image` (`gpustack/mirrored-modelexpress-server:v0.7.0`) in worker settings (`DefaultTracking`) and the Helm chart.
- Dynamically reconcile a single-replica, in-memory ModelExpress metadata server `Deployment` (`Recreate` strategy, `MX_METADATA_BACKEND=memory`) and `ClusterIP` `Service` (port `8001`) for any `ModelDeployment` that has at least one role with `--load-format modelexpress` or `mx`, and prune them when no role uses ModelExpress.
- Intercept standard `--load-format modelexpress` and `mx` in `gpustack_model_transfer` for both vLLM and SGLang, removing the artificial `gpustack-modelexpress` load format name.
- Configure the ModelExpress load strategy chain as `RdmaStrategy -> GpustackLoadStrategy -> ServerCacheStrategy -> ... -> DefaultStrategy` (`MX_LOAD_PLUGIN=gpustack`, `MX_LOAD_PLUGIN_BEFORE=server-cache`) while keeping `ctx.p2p_enabled = True`, `result.publishable = True`, and `MX_ARTIFACT_TRANSFER=1`.
- Implement `GpustackLoadStrategy` for both vLLM (`VllmAdapter`) and SGLang (`SglangAdapter`), including reload and draft guards, a non-mutating source readiness probe (`download_model`), `load_initialized_model` for vLLM, post-load adapter hooks, and `LoadStrategy.finalize` so tensors loaded via `model-transfer` are registered with NIXL and published to `modelexpress-server`.
- Wire `--load-format modelexpress` roles in the Operator controller with ModelExpress environment variables (`MX_SERVER_ADDRESS`, `MX_WORKER_HOST`, `MX_ARTIFACT_TRANSFER`, `MX_ARTIFACT_READY_URL`, `MX_ARTIFACT_COMPILE_CONFIG_DIGEST`, `MX_LOAD_PLUGIN`, `MX_LOAD_PLUGIN_BEFORE`) and `model-transfer` fallback infrastructure (staged wheel, bootstrap wrapper, ServiceAccount token, `endpoints.json` `ConfigMap`, and `IPC_LOCK`/`SYS_RESOURCE` capabilities).
- Preserve backward compatibility with existing HIXL and NIXL `--load-format model-transfer` workloads.

### Non-Goals

- External database backends (Redis, PostgreSQL, Neo4j) or Kubernetes ConfigMap/CRD persistence for `modelexpress-server`; the metadata server runs strictly in memory (`MX_METADATA_BACKEND=memory`) and reconstructs state via replica re-publication or `GpustackLoadStrategy` fallback when restarted.
- Cross-`ModelDeployment` ModelExpress metadata server sharing; each `ModelDeployment` using `--load-format modelexpress` owns an isolated metadata server.
- Peer D2D over Ascend HIXL inside upstream ModelExpress (upstream ModelExpress P2P uses `nixl_manager`); on HIXL, `GpustackLoadStrategy` continues to use `gpustack_model_transfer`'s native HIXL transport.

## Proposal

### User Stories

#### Story 1
As a platform user deploying vLLM or SGLang with `--load-format modelexpress` without `ModelPrefetch`, I want the seed replica to load from local cache and publish its GPU weights and compile cache to an Operator-managed in-memory ModelExpress metadata server, so that scaling from 1 to $N$ peer replicas transfers weights via NIXL Peer D2D and reuses the compiled cache over NIXL without requiring `ModelPrefetch`.

#### Story 2
As a platform user deploying vLLM or SGLang with `--load-format modelexpress` alongside `ModelPrefetch`, I want the seed replica (or any peer replica recovering after a ModelExpress metadata server restart) to automatically pull weights from the `ModelPrefetch` source node via `GpustackLoadStrategy` (`model-transfer`), publish its GPU weights and compile cache to the ModelExpress metadata server, and let subsequent peer replicas load via `RdmaStrategy` (NIXL D2D) and NIXL compile cache transfer.

#### Story 3
As a cluster operator, I want existing `--load-format model-transfer` workloads on HIXL (Ascend CANN) and NIXL (NVIDIA CUDA) to continue working without regression when ModelExpress support is enabled.

### Core Features & Acceptance Criteria

1. **Mirrored ModelExpress Metadata Server Image (`pack/mirrored-modelexpress-server`)**:
   - Builds `modelexpress-server` from `https://github.com/ai-dynamo/modelexpress.git` at pinned tag `v0.7.0` (`ac6d03a87a550c8188c3ed59f187b90d4f0554f5`) with `--features memory-backend`.
   - Runs as non-root user `65532:65532` on port `8001` with `MX_METADATA_BACKEND=memory`.
   - Tracked by worker setting `model-deployment-modelexpress-server-image` (`gpustack/mirrored-modelexpress-server:v0.7.0`).
2. **Per-Deployment Metadata Server Lifecycle (`pkg/worker/controllers/worker/model_deployment_modelexpress.go`)**:
   - Reconciles `<md-name>-modelexpress` `Deployment` (`Replicas=1`, `Recreate` strategy) and `ClusterIP` `Service` (`port: 8001`) when any role in `ModelDeployment` specifies `--load-format modelexpress` or `--load-format mx`.
   - Garbage-collects the `Deployment` and `Service` via resource note `modelexpress-server` when no role uses `modelexpress`.
3. **Role Pod Staging and Environment Injection**:
   - Preserves `--load-format modelexpress` or `--load-format mx` on the role container without overwriting it with `--load-format model-transfer`.
   - Stages the `gpustack_model_transfer` wheel, bootstrap wrapper, ServiceAccount token, `endpoints.json` `ConfigMap`, and `IPC_LOCK` / `SYS_RESOURCE` capabilities even when remote delivery is not forced.
   - Injects `MX_SERVER_ADDRESS`, `MX_WORKER_HOST`, `MX_ARTIFACT_TRANSFER=1`, `MX_LOAD_PLUGIN=gpustack`, `MX_LOAD_PLUGIN_BEFORE=server-cache`, `MX_ARTIFACT_READY_URL`, `MX_ARTIFACT_COMPILE_CONFIG_DIGEST`, and `GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY` unless overridden by the role spec.
   - Mounts the `metadataOnly: "true"` CSI volume when `modelDeploymentUsesRemoteDelivery` is `true`, and retains the full Node CSI or Engine model volume when `modelDeploymentUsesRemoteDelivery` is `false` so a seed replica without a remote source can load via `DefaultStrategy`.
4. **Three-Tier ModelExpress Strategy Chain in `gpustack_model_transfer`**:
   - Places `GpustackLoadStrategy` (`gpustack`) after `RdmaStrategy` (`rdma`) and before `ServerCacheStrategy` (`server-cache`).
   - Preserves `ctx.p2p_enabled = True` and sets `result.publishable = True` so replicas loaded via `GpustackLoadStrategy` or `DefaultStrategy` register GPU tensors with NIXL (`LoadStrategy.finalize`) and publish tensor descriptors and compile cache archives to `modelexpress-server`.
   - Under `Require` policy, restricts the strategy chain to `rdma` and `gpustack`; under `Prefer` policy, retains local fallback strategies behind `gpustack`.
   - Performs a non-mutating `loader.download_model(model_config)` readiness probe before touching `result.model`, raising `StrategyFailed(..., mutated=False)` without poisoning `_process_unusable` when no remote source is ready.

### Notes / Constraints / Caveats

- Upstream `modelexpress-server` `v0.7.0` (`ac6d03a87a550c8188c3ed59f187b90d4f0554f5`) includes `--features memory-backend` (`DashMap`-backed `MemoryBackend` selected by `MX_METADATA_BACKEND=memory`, introduced upstream in commit `0e409af8`). It does not include a Kubernetes backend; the Operator manages the stateless in-memory server `Deployment` and `Service` directly.
- Both GPU tensor transfers (`RdmaStrategy`) and compile cache archive transfers (`MX_ARTIFACT_TRANSFER=1`) between the seed replica and peer replicas use NIXL RDMA: GPU tensors stream GPU-to-GPU (D2D), while compile cache archives stream over NIXL registered host memory (`nixl_FromFile` / `request_artifact_chunks`).
- When vLLM 0.30.0 runs `Qwen/Qwen3-235B-A22B-Instruct-2507-FP8` (`TP8 + EP`) with shared caches, `--kernel-config '{"enable_flashinfer_autotune":false}'` is required to avoid a FlashInfer autotune collective mismatch across TP ranks when rank 0 hits a warm cache and peer ranks miss.

### Boundaries

- **Always:** Preserve backward compatibility for `--load-format model-transfer` on both NIXL and HIXL; keep `_autoload.py` free of `modelexpress` or `torch` imports when `--load-format modelexpress` is not active; run `make lint`, `make lint python`, `make lint chart`, and `make lint docs` before shipping.
- **Ask first:** Changing the default `model-deployment-modelexpress-server-image` version tag or adding external persistence backends to `modelexpress-server`.
- **Never:** Force `ctx.p2p_enabled = False` or `result.publishable = False` on `GpustackLoadStrategy`, overwrite a user's `--load-format modelexpress` flag with `--load-format model-transfer`, or cite uncommitted local scratch paths in committed specs or documentation.

### Risks and Mitigations

- **In-memory metadata server restart loses registered peer descriptors** → Use `Recreate` deployment strategy to prevent split-brain during rollout; existing serving replicas periodically re-publish tensor metadata to the restarted server, and any replica starting while the registry is empty falls back automatically to `GpustackLoadStrategy` (`ModelPrefetch` source over NIXL H2H + H2D) or `DefaultStrategy` (`Prefer`).
- **Failed `GpustackLoadStrategy` probe on seed replica could poison engine process state** → Probe source availability via `loader.download_model(model_config)` before setting `_process_unusable = True` or mutating `result.model`, and raise `StrategyFailed(..., mutated=False)` so `LoadStrategyChain` advances cleanly to `DefaultStrategy` without requiring `reinit_for_retry`.
- **SGLang hardcoded health port (`30000`) in upstream ModelExpress artifact readiness check** → Inject `MX_ARTIFACT_READY_URL=http://127.0.0.1:<enginePort>/health` from the Operator controller whenever the role's engine port is known and gradable.
- **Partial wheel staging when updating `gpustack_model_transfer` on a shared host directory** → Stage wheels in `pack/model-transfer/rootfs/usr/bin/copy-dir.sh` via a sibling temporary file (`cp -f` + `mv -f`) and remove stale wheels after replacement.

## Design Details

### Commands

Run build, test, and lint checks from the repository root in the local macOS arm64 checkout (and on the target Linux clusters for hardware verification):

```bash
make lint
make lint python
make lint chart
make lint docs
(cd model-transfer/python && uv run pytest)
go test ./pkg/worker/controllers/worker/... -run 'TestModelDeploymentModelExpress|TestRenderModelDeploymentModelExpressObjects|TestModelDeploymentModelTransferRoleKeepsModelexpressFormat'
```

### Project Structure

| Area | Responsibility |
|---|---|
| `pack/mirrored-modelexpress-server` | Multi-stage Rust Dockerfile building `modelexpress-server` (`v0.7.0`) with `--features memory-backend`. |
| `pack/model-transfer` and `pack/gpustack-operator` | Universal `gpustack_model_transfer` wheel staging into `/var/lib/gpustack/operator/lib/model-transfer`. |
| `pkg/worker/settings` | Defines `ModelDeploymentModelExpressServerImage` (`model-deployment-modelexpress-server-image`) and registers it in `DefaultTracking`. |
| `pkg/worker/controllers/worker` | Reconciles `<md-name>-modelexpress` `Deployment`/`Service`, renders ModelExpress pod environment and volumes, and always reconciles `<md-name>-model-transfer-endpoints` for ModelExpress roles. |
| `model-transfer/python/gpustack_model_transfer/adapters/modelexpress` | Implements `GpustackLoadStrategy`, strategy-chain ordering (`INSERT_BEFORE = "server-cache"`), `Require`/`Prefer` policy filtering, and P2P tensor + compile cache publication. |
| `model-transfer/python/gpustack_model_transfer/adapters/sglang` | Registers `modelexpress` and `mx` in SGLang's `LoadFormat` and delegates to `MxModelLoader`. |
| `docs/modules/model-delivery` and `docs/reference` | User-facing documentation, SVG architecture diagram, benchmark results, and settings reference. |

### Code Style

Follow existing Operator controller conventions: small focused functions, snake_case multi-word Go filenames (`model_deployment_modelexpress.go`), plain comments without decorative symbols, and idempotent resource rendering:

```go
func modelDeploymentLoadFormats(engine string, args []string) []string {
	keys := []string{modelDeploymentLoadFormatArg}
	var values []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			break
		}
		if _, ok := ModelDeploymentResolveArg(engine, args[i], keys); !ok {
			continue
		}
		if _, value, inline := strings.Cut(args[i], "="); inline {
			values = append(values, strings.ToLower(strings.TrimSpace(value)))
			continue
		}
		if i+1 < len(args) && modelDeploymentConsumableValue(args[i+1]) {
			i++
			values = append(values, strings.ToLower(strings.TrimSpace(args[i])))
			continue
		}
		values = append(values, "")
	}

	return values
}
```

### Architecture and Runtime Flow

```text
                    +---------------------------------------------------+
                    |  <md-name>-modelexpress (Deployment + ClusterIP)  |
                    |  modelexpress-server (MX_METADATA_BACKEND=memory) |
                    |  gRPC :8001                                       |
                    +-------------------------+-------------------------+
                                              ^
                         Publish / Query      |  MX_SERVER_ADDRESS=<svc>:8001
                      (Tensor Descriptors +   |  MX_ARTIFACT_TRANSFER=1
                       Compile Cache Manifest)|
              +-------------------------------+-------------------------------+
              |                                                               |
              v                                                               v
+-------------------------------------------+   RdmaStrategy: NIXL D2D   +-------------------------------------------+
| Seed replica (Replica 0)                  | -------------------------> | Peer replica (Replica 1..N)               |
| --load-format modelexpress                |   + NIXL compile cache     | --load-format modelexpress                |
|                                           |     transfer (host RDMA)   |                                           |
| Load Chain:                               |                            | Load Chain:                               |
| 1. RdmaStrategy -> No ready peer          |                            | 1. RdmaStrategy -> HIT! Streams GPU       |
| 2. GpustackLoadStrategy ->                |                            |    tensors directly from Seed replica     |
|    - If endpoints.json has ready source:  |                            |    via NIXL D2D + pulls Seed replica      |
|      pulls via model-transfer (NIXL H2H   |                            |    compile cache over NIXL RDMA.          |
|      + H2D)                               |                            | 2. Publishes own GPU tensors to MX server.|
|    - Else (Prefer / no ModelPrefetch):    |                            +-------------------------------------------+
|      mutated=False -> falls through       |
| 3. DefaultStrategy -> loads local CSI /   |
|    HF cache                               |
| 4. Finalizes NIXL registration & publishes|
|    weights + compile cache to MX server   |
+-------------------------------------------+
```

1. **Container Image Packaging (`pack/mirrored-modelexpress-server`)**:
   - Builder stage (`rust:1.95-bookworm`) clones `https://github.com/ai-dynamo/modelexpress.git` at `ac6d03a87a550c8188c3ed59f187b90d4f0554f5` (`v0.7.0`) and builds `cargo build --release --locked --bin modelexpress-server --features memory-backend`.
   - Runtime stage (`debian:bookworm-slim`) installs `ca-certificates` and `libssl3`, copies `/usr/local/bin/modelexpress-server`, sets `MX_METADATA_BACKEND=memory` and `MODEL_EXPRESS_SERVER_PORT=8001`, and runs as `USER 65532:65532`.
2. **Operator Reconciliation (`pkg/worker/controllers/worker/`)**:
   - `modelDeploymentRoleUsesModelExpress` inspects `ModelDeploymentRoleArgs(role)` via `extractLoadFormatFromArgs` (built on `ModelDeploymentResolveArg` and `modelDeploymentConsumableValue`) and matches `"modelexpress"` or `"mx"`.
   - `renderModelDeploymentModelExpressObjects` renders `<md.Name>-modelexpress` `Deployment` (`Recreate`, 1 replica, TCP port `8001` readiness/liveness probes) and `ClusterIP` `Service`.
   - `reconcileModelTransferClientConfig` always reconciles `<md.Name>-model-transfer-endpoints` `ConfigMap` (`endpoints.json`) when `modelDeploymentUsesModelExpress(md)` is true so `GpustackLoadStrategy` can dynamically discover a ready `ModelPrefetch` source or see `"sources": {}` and fall back cleanly.
3. **Python Adapter (`model-transfer/python/gpustack_model_transfer/adapters/modelexpress/`)**:
   - `constants.py` sets `LOAD_FORMAT = "modelexpress"`, `LOAD_FORMAT_ALIASES = ("modelexpress", "mx")`, `PLUGIN_NAME = "gpustack"`, and `INSERT_BEFORE = "server-cache"`.
   - `GpustackLoadStrategy` supports both `VllmAdapter` (`load_initialized_model`) and `SglangAdapter` (`load_weights` inside `capture_tensor_attrs`), runs `loader.download_model(model_config)` before mutating `result.model`, and inherits `LoadStrategy.finalize` so loaded GPU tensors are registered with `ctx.nixl_manager` and published alongside the compile cache.

### Implementation Plan

Task T1 and Task T2 landed in `main` via [PR #814](https://github.com/gpustack/gpustack-operator/pull/814) (`b0750a695`); Task T3 landed in `main` via [PR #815](https://github.com/gpustack/gpustack-operator/pull/815) (`6e89f9655`); Tasks T4–T6 are delivered on this branch.

- [x] **T1 · Update `gpustack_model_transfer` for ModelExpress Peer D2D, SGLang, and source fallback**
      Blocked by: None
      Owns: `model-transfer/python/gpustack_model_transfer/**`, `model-transfer/python/tests/**`
      Acceptance: `LOAD_FORMAT` is `"modelexpress"` (with `"mx"` alias), `INSERT_BEFORE` is `"server-cache"`, `ctx.p2p_enabled` and `result.publishable` remain `True`, `GpustackLoadStrategy` supports both vLLM and SGLang with non-mutating `download_model` preflight and `LoadStrategy.finalize` tensor registration, and `Require`/`Prefer` policy filtering is enforced.
      Verify: `make lint python && (cd model-transfer/python && uv run pytest)`

- [x] **T2 · Ship standalone `model-transfer` Python SDK pull request**
      Blocked by: T1
      Owns: `model-transfer/python/**`
      Gate: review
      Acceptance: Standalone pull request (#814) passes `make lint python`, `uv run pytest`, and independent adversarial review, and merges into `main`.
      Verify: `make lint python && (cd model-transfer/python && uv run pytest)`

- [x] **T3 · Package `pack/mirrored-modelexpress-server` and wheel staging in vendor images**
      Blocked by: T2
      Owns: `pack/mirrored-modelexpress-server/Dockerfile`, `pack/model-transfer/**`, `pack/gpustack-operator/Dockerfile`, `hack/package.sh`, `.github/workflows/base-image.yml`
      Gate: review
      Acceptance: `pack/mirrored-modelexpress-server/Dockerfile` builds `modelexpress-server` `v0.7.0` (`ac6d03a87a550c8188c3ed59f187b90d4f0554f5`) with `--features memory-backend` as `USER 65532:65532`; `copy-dir.sh` stages wheels atomically; standalone packaging pull request (#815) merges into `main`.
      Verify: `make lint`

- [x] **T4 · Reconcile ModelExpress metadata server and pod wiring in `ModelDeployment` controller**
      Blocked by: T1, T3
      Owns: `pkg/worker/settings/value.go`, `deploy/gpustack-operator/chart/values.yaml`, `pkg/worker/controllers/worker/model_deployment*.go`
      Gate: review
      Acceptance: Reconciles `<md.Name>-modelexpress` `Deployment` and `Service` when any role uses `--load-format modelexpress` or `mx`, prunes them when unused, injects `MX_*` and `GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY` env vars, stages `gpustack_model_transfer` wheel and `endpoints.json` `ConfigMap`, and selects metadata-only vs full CSI volume per remote delivery state.
      Verify: `go test ./pkg/worker/controllers/worker/... -run 'TestModelDeploymentModelExpress|TestRenderModelDeploymentModelExpressObjects|TestModelDeploymentModelTransferRoleKeepsModelexpressFormat' && make lint && make lint chart`

- [x] **T5 · Document ModelExpress architecture, settings, and benchmark case**
      Blocked by: T4
      Owns: `docs/modules/model-delivery/remote.md`, `docs/assets/diagrams/model-delivery-modelexpress.svg`, `docs/reference/settings.md`, `.agents/skills/gpustack-operator-benchmark/**`
      Acceptance: Documents ModelExpress Peer D2D weight delivery, NIXL compile cache transfer, role configuration, `model-deployment-modelexpress-server-image` setting, and benchmark case 12 with node compile-cache cleanup between stages.
      Verify: `make lint docs && make lint agents-shell`

- [x] **T6 · Execute HIXL regression and Nebius H100 NIXL + ModelExpress verification**
      Blocked by: T2, T4, T5
      Owns: `specs/2026-11-09-modelexpress-peer-d2d-and-cache-sharing.md`, `docs/modules/model-delivery/remote.md`
      Gate: review
      Acceptance: Zero regression on 2-node Ascend 910B2 HIXL (`Qwen/Qwen3.6-35B-A3B`) and 2-node Nebius H100 NIXL (`80B` TP4 and `235B` TP8+EP), plus end-to-end verification of ModelExpress seed replica (`GpustackLoadStrategy`), peer scale-out replica (`RdmaStrategy` D2D + NIXL compile cache transfer), and post-restart fallback on `235B` TP8+EP in graph mode.
      Verify: `bash .agents/skills/gpustack-operator-docs/scripts/check-specs.sh && make lint docs`

### Benchmarks and Verification Results

#### 1. Ascend 910B2 HIXL Regression (`Qwen/Qwen3.6-35B-A3B`, TP8 + EP)

Verified on a two-node RKE2 Ascend 910B2 cluster (`cann9.2-910b-vllm0.30.0`, eager mode) with `gpustack-operator` and `model-transfer` built from this branch. Target node held zero local model files (`metadataOnly: "true"`):

| Case | Mode | Source Window | Target Window | Source RAM | Target RAM | Weight Transfer Time | Inference Probe |
|---|---|---|---|---|---|---|---|
| HIXL 35B Paged | `TP all` | 16 GiB | 8 GiB | 15.66 GiB | 8.00 GiB | 99.64 s | PASS (`391`, `stop`) |
| HIXL 35B Full Source | `TP all` | 80 GiB | 64 GiB | 66.97 GiB | 29.78 GiB | 44.12 s | PASS (`391`, `stop`) |

`_autoload.py` imported zero `modelexpress` modules during `--load-format model-transfer` startup, and all source/target HIXL allocations and sessions released cleanly after deletion.

#### 2. Nebius H100 80 GB NIXL Regression (`80B` TP4 and `235B` TP8 + EP)

Verified on a two-node Nebius `eu-north1` 8x H100 80 GB (`fabric-4`, 8/8 `mlx5_*` InfiniBand ports active, 1574.6 GiB host RAM per node) cluster with vLLM 0.30.0. The target node held 0 cached models and 0 `gpustack_model_manager_download_bytes_total` series across all runs:

| Suite | Model | Mode | Source Window | Target Window | Source RAM | Target RAM | Target Storage | Weight Transfer Time | vs Baseline |
|---|---|---|---|---|---|---|---|---|---|
| Suite 1 | `Qwen3-Next-80B-A3B-Instruct-FP8` (TP4, eager) | Baseline (`TCP peer sync + local load`) | — | — | 0 GiB | 0 GiB | 76.45 GiB | 393.14 s | 1.00× |
| Suite 1 | `Qwen3-Next-80B-A3B-Instruct-FP8` (TP4, eager) | `TP0 + broadcast` | 16 GiB paged | 8 GiB | 16.00 GiB | 8.00 GiB (rank 0) | 0 GiB | 160.59 s | 2.45× (−59.2%) |
| Suite 1 | `Qwen3-Next-80B-A3B-Instruct-FP8` (TP4, eager) | `TP all` | 16 GiB paged | 8 GiB | 16.00 GiB | 8.00 GiB (2 GiB × 4) | 0 GiB | 1380.52 s | 0.29× (+251.2%) |
| Suite 1 | `Qwen3-Next-80B-A3B-Instruct-FP8` (TP4, eager) | `TP all` | 80 GiB (full resident) | 64 GiB | 76.42 GiB | 40.00 GiB (10 GiB × 4) | 0 GiB | 81.16 s | 4.85× (−79.4%) |
| Suite 2 | `Qwen3-235B-A22B-Instruct-2507-FP8` (TP8+EP, eager) | Baseline (`TCP peer sync + local load`) | — | — | 0 GiB | 0 GiB | 220.21 GiB | 1121.30 s | 1.00× |
| Suite 2 | `Qwen3-235B-A22B-Instruct-2507-FP8` (TP8+EP, eager) | `TP0 + broadcast` | 16 GiB paged | 8 GiB | 16.00 GiB | 8.00 GiB (rank 0) | 0 GiB | 287.97 s | 3.89× (−74.3%) |
| Suite 2 | `Qwen3-235B-A22B-Instruct-2507-FP8` (TP8+EP, eager) | `TP all` | 256 GiB (full resident) | 32 GiB | 220.19 GiB | 32.00 GiB (4 GiB × 8) | 0 GiB | 69.16 s | 16.21× (−93.8%) |

In Suite 1 (`TP all`, 16 GiB paged source), four independent rank sessions page concurrently through a 16 GiB source window for a 76.42 GiB checkpoint and thrash page-in replacements; paged source budgets should therefore use `TP0 + broadcast`, whereas `TP all` is suited for full-resident sources (`4.85×` on 80B, `16.21×` on 235B).

#### 3. Nebius H100 80 GB ModelExpress End-to-End Benchmark (`235B` TP8 + EP, Graph Mode)

Verified with `Qwen/Qwen3-235B-A22B-Instruct-2507-FP8` (`TP8 + EP`) in full graph mode (no `--enforce-eager`, `--load-format modelexpress`). The Operator reconciled `<md-name>-modelexpress` (`gpustack/mirrored-modelexpress-server:v0.7.0`) and injected `MX_SERVER_ADDRESS`, `MX_WORKER_HOST`, `MX_ARTIFACT_TRANSFER=1`, `MX_LOAD_PLUGIN=gpustack`, and `MX_LOAD_PLUGIN_BEFORE=server-cache`:

| Stage | Replica Role & Active Strategy | Source RAM | Target RAM | Target Storage | Weight Transfer Time | `torch.compile` Time | CUDA Graph Capture | Total Engine Init |
|---|---|---|---|---|---|---|---|---|
| Baseline | `TCP peer sync + local load` (sum) | 0 GiB | 0 GiB | 220.21 GiB | 1121.30 s (1.00×) | — | — | — |
| Stage 1 (Seed replica) | `GpustackLoadStrategy` (256 GiB source, 8 GiB target, cold MX registry) | 220.19 GiB | 8.00 GiB | 0 GiB | 232.10 s (4.83×, −79.3%) | 77.67 s (cold) | 112.00 s (40 s + 72 s) | 296.82 s |
| Stage 2 (Peer replica) | `RdmaStrategy` (NIXL D2D GPU tensor stream + NIXL compile cache transfer) | 0 GiB | 0 GiB | 0 GiB | 16.78 s (66.82×, −98.5%) | 0.28 s (277.39×, cache restored) | 79.00 s (37 s + 42 s) | 90.25 s (3.29×) |
| Stage 3 (Restart fallback) | `RdmaStrategy` miss after MX server restart → `GpustackLoadStrategy` | 220.19 GiB | 8.00 GiB | 0 GiB | 109.08 s (10.28×, −90.3%) | 0.26 s | 39.00 s | — |

- **Seed replica (Stage 1)**: Loaded 220.19 GiB from the `ModelPrefetch` source via `GpustackLoadStrategy` in 232.10 s, completed cold `torch.compile` (77.67 s), and published P2P GPU tensor descriptors for all 8 TP ranks (`mx_source_id=a7a3078b9fa7de4e`) plus 386.07 MiB of compiled cache artifacts (`torch.compile`, Triton, DeepGEMM, FlashInfer) to `<md-name>-modelexpress`.
- **Peer replica (Stage 2)**: Discovered the seed replica from `<md-name>-modelexpress`, transferred pre-sharded GPU tensors directly GPU-to-GPU via `RdmaStrategy` (NIXL D2D) in **16.78 s** (`66.82×` vs baseline, `13.83×` vs seed replica) with `0 GiB` host transfer RAM and `0 GiB` target disk, and streamed the 386.07 MiB compile cache archive over NIXL host-memory RDMA, cutting `torch.compile` from **77.67 s to 0.28 s** and total engine initialization from **296.82 s to 90.25 s**.
- **Metadata server restart fallback (Stage 3)**: Deleting the `<md-name>-modelexpress` pod cleared in-memory registrations. Note that live serving replicas automatically re-publish their real GPU tensor metadata to a restarted `modelexpress-server` within ~25 s; when a new replica was launched inside the empty-registry window before re-publication completed, `RdmaStrategy` missed cleanly and fell back to `GpustackLoadStrategy` (`109.08 s` from the warm 220.19 GiB `ModelPrefetch` source), passed inference probes, and re-published its descriptors.

### Test Plan

[x] I/we understand the owners of the involved components may require updates to existing tests to make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

- Added fake-engine and strategy-chain fixtures in `model-transfer/python/tests/test_modelexpress_adapter.py`, `model-transfer/python/tests/test_autoload.py`, and `model-transfer/python/tests/test_sglang_plugin.py` to test vLLM and SGLang ModelExpress integration without requiring live GPUs.
- Added node compile-cache cleanup (`clear_node_compile_cache`) to `.agents/skills/gpustack-operator-benchmark/cases/case-12-modelexpress.sh` before each stage so cold compilation and NIXL compile-cache restoration are measured without host filesystem residue.

#### Unit tests

- `model-transfer/python/tests`: `2026-11-09` - `1192 passed` (`test_autoload.py`, `test_modelexpress_adapter.py`, `test_sglang_plugin.py`)
  - Verifies `--load-format modelexpress` and `--load-format mx` launch env injection (`MX_LOAD_PLUGIN=gpustack`, `MX_LOAD_PLUGIN_BEFORE=server-cache`, `MX_ARTIFACT_TRANSFER=1`), zero `modelexpress` imports on `--load-format model-transfer`, `INSERT_BEFORE == "server-cache"`, `ctx.p2p_enabled == True`, `result.publishable == True`, non-mutating `download_model` probe (`StrategyFailed(..., mutated=False)` under `Prefer`), `Require` policy chain filtering, `LoadStrategy.finalize` NIXL registration, and SGLang `MxModelLoader` delegation.
- `pkg/worker/controllers/worker`: `2026-11-09` - `PASS` (`model_deployment_transfer_test.go`, `model_deployment_compile_cache_test.go`)
  - Verifies `TestModelDeploymentModelExpressLoadFormatRecognition`, `TestModelDeploymentModelExpressResourceNameAndAddress`, `TestRenderModelDeploymentModelExpressObjects`, `TestModelDeploymentModelTransferRoleKeepsModelexpressFormat`, `TestModelDeploymentModelExpressPodPerDelivery`, and `TestModelDeploymentModelExpressEndpointsConfigMapAlwaysReconciled`.

#### Integration tests

- Controller reconciliation and child pruning (`syncModelDeploymentModelExpress` and `syncModelDeploymentOwnedChildren`) verified via `pkg/worker/controllers/worker` test suite with fake Kubernetes clients.
- Strategy-chain execution (`RdmaStrategy -> GpustackLoadStrategy -> DefaultStrategy`) and P2P publication verified across simulated vLLM and SGLang adapters in `model-transfer/python/tests/test_modelexpress_adapter.py`.

#### e2e tests

- Two-node RKE2 Ascend 910B2 HIXL regression suite (`Qwen/Qwen3.6-35B-A3B`, TP8 + EP, paged 16 GiB and full-resident 80 GiB source).
- Two-node Nebius H100 80 GB NIXL regression suite (`Qwen/Qwen3-Next-80B-A3B-Instruct-FP8` TP4 and `Qwen/Qwen3-235B-A22B-Instruct-2507-FP8` TP8 + EP) and 3-stage ModelExpress benchmark suite (Stage 1 seed replica via `GpustackLoadStrategy`, Stage 2 peer replica via `RdmaStrategy` NIXL D2D + NIXL compile cache transfer, Stage 3 metadata-server restart fallback to `GpustackLoadStrategy`).

## Alternatives

| Alternative | Assessment |
|---|---|
| Keep the custom `--load-format gpustack-modelexpress` flag | Rejected. Diverges from upstream vLLM and ModelExpress CLI conventions (`--load-format modelexpress` and `mx`) and requires users to learn a non-standard flag name. |
| Place `GpustackLoadStrategy` before `RdmaStrategy` (`INSERT_BEFORE = "rdma"`) with `ctx.p2p_enabled = False` | Rejected. Forces every replica to load from the `ModelPrefetch` source over H2H + H2D (232.10 s on 235B) instead of streaming processed tensors directly from a ready peer replica over NIXL D2D (16.78 s on 235B), and disables compile cache sharing. |
| Deploy an external Redis or PostgreSQL database for `modelexpress-server` | Rejected. Adds stateful operational overhead per deployment; `MX_METADATA_BACKEND=memory` with `Recreate` rollout is stateless, and live replicas re-publish metadata within ~25 s while new replicas fall back to `GpustackLoadStrategy` if the registry is empty. |
| Share a single cluster-wide `modelexpress-server` across all `ModelDeployment`s | Rejected. A per-`ModelDeployment` server isolates tenant namespaces, ties server lifecycle to the deployment owner reference, and avoids cross-deployment blast radius on restart. |

## Open Questions

None. All design, packaging, controller reconciliation, and hardware verification gates for #809 are resolved.
