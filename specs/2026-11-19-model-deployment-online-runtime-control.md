# Spec: ModelDeployment Phase 1 Online Runtime Control, Engine Sentinel, and Router Hash Decoupling

Status: Built
Blocked on: Live NPU cluster qualification for vLLM Ascend via gpustack-operator-e2e before promoting Status to Shipped (live NVIDIA H200 qualification for vLLM 0.32.0 and SGLang 0.5.22 passed across cases 133-137 and same-UID restart latch replay).
Type: Feature

- **Related Specs**: [2026-10-01-s1-router-qualification-and-drain.md](2026-10-01-s1-router-qualification-and-drain.md) · [2026-10-01-s2-elastic-ep-resize.md](2026-10-01-s2-elastic-ep-resize.md) · [2026-10-18-model-deployment-render-version.md](2026-10-18-model-deployment-render-version.md) · [2026-10-21-model-deployment-scaler.md](2026-10-21-model-deployment-scaler.md) · [2026-10-28-model-artifact-remote-weight-loading.md](2026-10-28-model-artifact-remote-weight-loading.md)

## Summary

Decouple mutable router fields, role pause state, and live-tunable engine parameters from worker `modeldeployment.gpustack.ai/pod-spec-hash` on `ModelDeployment` (`worker.gpustack.ai/v1alpha1` and `worker.gpustack.ai/v1`).
Replace rigid per-knob CRD structs and standalone `elasticEp` configuration with three cohesive primitives: first-class `spec.roles[*].pause`, a deployment-level reserved `spec.model.loraAdapters` entry aligned with `ModelArtifact`, and `spec.roles[*].runtimeControl` (loosely-coupled `parameters: map[string]string` translated by a unified internal registry, including `elastic.dataParallelWidth`, with no opt-in `enabled` boilerplate).
Boot ceilings remain in `extraArgs`, north-south authentication remains with the external Gateway (zero operator-managed API keys or per-deployment auth Secrets), all three managed routers enforce an Envoy method/path inference allowlist (including `vllm-omni` / `sglang-omni` multimodal endpoints), and a read-only host-staged `gpustack-engine-sentinel` Python package is injected by default on all managed accelerated non-`Encode` roles—using `sitecustomize.py` + `uvicorn.Config` universal ASGI fallback without mutating `VLLM_PLUGINS` or container argv—to unify `/gpustack/v1/{ready,live,pause}` probes, `PreStop` graceful draining, and live parameter actuation.

## Motivation

1. **Router edits roll every GPU worker Pod unnecessarily**:
   - Worker Pod rendering in [`model_deployment_render.go`](../pkg/worker/controllers/worker/model_deployment_render.go), [`model_deployment_connector.go`](../pkg/worker/controllers/worker/model_deployment_connector.go), [`model_deployment_connector_epd.go`](../pkg/worker/controllers/worker/model_deployment_connector_epd.go), and [`model_deployment_kv_events.go`](../pkg/worker/controllers/worker/model_deployment_kv_events.go) reads only `md.Spec.Router != nil` and `md.Spec.Router.Name`.
   - Hashing full `md.Spec.Router` into `pod-spec-hash` unnecessarily rolls all worker Pods whenever `replicas`, `image`, `imagePullPolicy`, `imagePullSecrets`, `extraArgs`, `requestTimeoutSeconds`, or `disaggregationThresholdTokens` changes.
2. **Rigid per-knob CRD schemas and fragmented online controls do not scale with fast-moving engines**:
   - [vLLM (`428c371b32`)](https://github.com/vllm-project/vllm/tree/428c371b32), [vllm-ascend (`1c9b0ab01`)](https://github.com/vllm-project/vllm-ascend/tree/1c9b0ab01), and [SGLang (`9304abb532`)](https://github.com/sgl-project/sglang/tree/9304abb532) add, rename, and retire tuning knobs far faster than Kubernetes CRD deprecation cycles.
   - Modeling live parameters as loosely-coupled `parameters: map[string]string` backed by an internal Translator Registry lets one unified vocabulary adapt across engines without frequent CRD deprecations, while folding Elastic EP (`elastic.dataParallelWidth`) into the same `runtimeControl` surface eliminates split online control paths.
3. **Boot ceilings belong in `extraArgs`, not a duplicate `runtimeBounds` schema**:
   - Elastic EP already reads `--elastic-ep-max-dp-size` from `extraArgs`. Duplicating engine flags (`--max-num-seqs`, `--max-num-batched-tokens`, `--elastic-ep-max-dp-size`) in a separate `runtimeBounds` block creates double-entry conflicts without adding capability; reading declared ceilings from `extraArgs` and falling back to engine-reported boot defaults keeps `ModelDeploymentRole` lean.
4. **Zero-intrusion engine sentinel compatible with `vllm-omni`, `sglang-omni`, and `model-transfer`**:
   - Setting `VLLM_PLUGINS` turns vLLM's plugin loader into a global allowlist that also filters `vllm-omni`'s `vllm_omni.general_plugins` and `vllm_omni.platform_plugins` groups (`vllm_omni/plugins/__init__.py`). Wrapping container `argv` breaks parallelism scanners and collides with `model-transfer`.
   - Loading `gpustack-engine-sentinel` solely via read-only `PYTHONPATH` + opt-in `sitecustomize.py` leaves `VLLM_PLUGINS` and container `argv` untouched, avoids `VLLM_SERVER_DEV_MODE=1` and `SGLANG_ADMIN_API_KEY` completely, and lets the sentinel absorb the 30-line inline `PreStop` drain script in [`model_deployment_drain.go`](../pkg/worker/controllers/worker/model_deployment_drain.go).

### Goals

- Decouple `spec.router.{replicas,image,imagePullPolicy,imagePullSecrets,extraArgs,requestTimeoutSeconds,disaggregationThresholdTokens}` from worker `pod-spec-hash` with dual-hash legacy currency (`modelDeploymentRenderVersion = 6`).
- Provide first-class `spec.roles[*].pause: bool` (excluded from `pod-spec-hash`) that withdraws endpoint eligibility from both Managed Router and Role Service, drains in-flight traffic, rejects direct inference arrivals at the sentinel ASGI layer, blocks `ModelDeploymentScaler` (`Reason=RolePaused`), and blocks Elastic EP resizes.
- Provide `spec.roles[*].runtimeControl` with `parameters: map[string]string` (excluded from `pod-spec-hash`, no `enabled` flag needed) backed by a unified Translator Registry covering live scheduling, speculative decoding, and Elastic EP (`elastic.dataParallelWidth`, replacing standalone `spec.roles[*].elasticEp`).
- Reserve deployment-level `spec.model.loraAdapters` (`name`, `artifactRef`, `subPath`, `pinned`) beside `spec.model.artifactRef` and reject non-empty values at admission in Phase 1 until `ModelArtifact` supports LoRA weights (preventing unsafe arbitrary container `path` injection).
- Inject an unpacked pure-Python `gpustack-engine-sentinel` package under `engine-sentinel/python/` by default on all managed accelerated non-`Encode` roles (version locked to `gpustack-operator` via `hatch-vcs`, staged flat into `/etc/gpustack/lib/engine-sentinel` in [`Dockerfile`](../pack/gpustack-operator/Dockerfile) and copied atomically to `/var/lib/gpustack/operator/lib/engine-sentinel`), loaded strictly via read-only `PYTHONPATH` + `sitecustomize.py` with a `uvicorn.Config` universal ASGI fallback (zero `VLLM_PLUGINS` mutation, zero `argv` wrapper, zero `VLLM_SERVER_DEV_MODE=1`, zero API keys or auth Secrets), unifying `/gpustack/v1/{live,ready,pause}` probes across vLLM, SGLang, `vllm-omni`, and `sglang-omni` and replacing inline `PreStop` Python drain code.
- Enforce a shared Envoy method/path inference allowlist (covering standard LLM + `vllm-omni`/`sglang-omni` multimodal endpoints) across `llm-d-router`, `vllm-router`, and `sglang-gateway`, with an unexposed Pod-internal `:9091` observer listener for loopback-bound argv routers.
- Reconcile `runtimeControl.parameters` idempotently behind a boot-local readiness latch (`/gpustack/v1/ready` + `/gpustack/v1/ack`), with role-sharded `ConfigMap` ledgers, ambiguous-POST fencing, incarnation-relative drain reading, and `status.roles[*].runtimeControl` + `RuntimeControlSynced`.

### Non-Goals

- **GPU memory sleep / KV-release power states (`SleepLevel1`, `SleepLevel2`, `ReleaseKVCache`)**: In Kubernetes, putting an engine to sleep inside a running Pod does not release Device Plugin or Kueue GPU allocations to other Pods. Power-state lifecycle control is deferred to `Open Questions` until paired with cluster-level GPU reclaim or sharing.
- **Dynamic LoRA hot-loading from arbitrary container paths**: Phase 1 reserves `spec.model.loraAdapters` backed by `ModelArtifact` (`artifactRef`) and rejects non-empty values at admission until `ModelArtifact` supports LoRA delivery and digest verification.
- **Operator-managed inference or control-plane API keys / Secrets**: North-south caller authentication belongs to the external Gateway; east-west traffic inside the cluster follows the operator's existing boundary (same as `llm-d-router` EPP and Elastic EP). User-supplied `--api-key` flags are neither forbidden nor managed by the operator.
- **Separate `runtimeBounds` CRD schema**: Boot ceilings stay in `extraArgs` (parsed statically when declared, complemented by engine-reported boot defaults at runtime).
- **Online `TP`, `PP`, `CP`, quantization, `--gpu-memory-utilization`, `--load-format ipc_cache`, live `Prefill` ↔ `Decode` role switch, or Elastic EP scale-down**: Deferred to future phases (Elastic EP scale-down remains rejected until upstream issue #741 lands).
- **Unmanaged (`role.command` set), `Encode`, CPU-only (`acceleratorType == ""`), or SGLang Ray roles**: Admission rejects `runtimeControl` on all four in Phase 1 (and default `gpustack-engine-sentinel` injection leaves them untouched).

## Proposal

### User Stories

#### Story 1
As an inference platform engineer, I want to update `spec.router.disaggregationThresholdTokens`, `spec.router.requestTimeoutSeconds`, or `spec.router.replicas`, so that only the router `Deployment` reconciles and zero worker Pods restart.

#### Story 2
As an SRE responding to bursty traffic or tuning serving efficiency, I want to update `spec.roles[*].runtimeControl.parameters` (such as `scheduling.maxActiveRequests`, `scheduling.maxBatchedTokens`, `speculative.acceptThresholdSingle`, or `elastic.dataParallelWidth`), so that serving behavior converges live without restarting worker Pods or breaking `vllm-omni` / `vllm-ascend` / `model-transfer` plugins.

#### Story 3
As a cluster operator draining a role for maintenance or traffic shifting, I want to set `spec.roles[*].pause: true`, so that the operator withdraws the role from the router and Service, drains in-flight requests, rejects direct Pod arrivals when the engine sentinel is enabled, holds `ModelDeploymentScaler` (`Reason=RolePaused`), and resumes cleanly when `pause` returns to `false`.

### Core Features & Acceptance Criteria

1. **F1 · Router Hash Projection with Dual-Hash Currency**:
   - In [`model_deployment_render.go`](../pkg/worker/controllers/worker/model_deployment_render.go) (`modelDeploymentExplicitSpecHash`), project `md.Spec.Router` to `nil` when unset and `&workercore.ModelDeploymentRouter{Name: md.Spec.Router.Name}` when set.
   - Keep `modelDeploymentRenderVersion = 6` (bumping from `5` because managed accelerated roles now inject `gpustack-engine-sentinel` read-only `PYTHONPATH`, unified `/gpustack/v1/{live,ready}` probes, and `gpustack_engine_sentinel.drain` PreStop by default) and refresh legacy/golden fixtures cleanly.
   - Thread projected and legacy full-router fingerprints through `modelDeploymentPodMatchesCurrencyWithLegacy` across [`model_deployment_render.go`](../pkg/worker/controllers/worker/model_deployment_render.go), [`model_deployment.go`](../pkg/worker/controllers/worker/model_deployment.go), and [`model_deployment_replacement_slot.go`](../pkg/worker/controllers/worker/model_deployment_replacement_slot.go) without adding Pod annotations.
   - Exclude `role.Pause`, `role.RuntimeControl.Parameters` (preserving only the immutable boolean presence of `elastic.dataParallelWidth` for Elastic EP Pod topology), and `md.Spec.Model.LoRAAdapters` from `pod-spec-hash`.

2. **F2 · Lean API Surface: `spec.roles[*].pause`, `spec.roles[*].runtimeControl`, Reserved `spec.model.loraAdapters`, and Status**:
   - Remove `ModelDeploymentRoleRuntimeBounds` and `ModelDeploymentRoleElasticEP` (`elasticEp`).
   - On `ModelDeploymentModel`: add `LoRAAdapters []ModelDeploymentLoRAAdapter` (`json:"loraAdapters,omitempty"`, tag `3`) where `ModelDeploymentLoRAAdapter` carries `Name string`, `ArtifactRef core.LocalObjectReference`, `SubPath string`, and `Pinned bool`.
   - On `ModelDeploymentRole`:
     - Add `Pause bool` (`json:"pause,omitempty"`, tag `22`).
     - Add `RuntimeControl *ModelDeploymentRoleRuntimeControl` (`json:"runtimeControl,omitempty"`, tag `23`) carrying `Parameters map[string]string` (tag `1`; no `enabled` field needed because `gpustack-engine-sentinel` is injected by default on all managed accelerated non-`Encode` roles).
   - On `ModelDeploymentRoleStatus`:
     - Add `RuntimeControl *ModelDeploymentRoleRuntimeControlStatus` (`json:"runtimeControl,omitempty"`, tag `11`) carrying `ObservedGeneration`, `Synced`, `Reason`, `Message`, `Paused`, `AppliedConfigDigest`, `ConvergedPods`, `TotalAnsweringPods`, and `UnsupportedFields`.
   - On `ModelDeploymentScaler`: use `ModelDeploymentScalerReasonRolePaused = "RolePaused"` in [`api/worker/v1alpha1/model_deployment_scaler.go`](../api/worker/v1alpha1/model_deployment_scaler.go).
   - Condition `ModelDeploymentConditionRuntimeControlSynced = "RuntimeControlSynced"` (`True`/`Synced` or `NotConfigured`, `False`/`ReconcileFailed` or `UnsupportedEngineVersion` or `DrainFailed` or `AmbiguousOperation`, `Unknown`/`Reconciling` or `Draining` or `EndpointUnreachable` or `WaitingForRouterIsolation`). Leaving `runtimeControl` unset/disabled and `pause: false` never degrades `status.phase`.

3. **F3 · Unified Translator Registry & Loosely-Coupled `runtimeControl.parameters`**:
   - Maintain a single canonical parameter registry in [`pkg/worker/runtimecontrol/translator.go`](../pkg/worker/runtimecontrol/translator.go) shared by webhook validation, canonical digest computation, and controller actuation.
   - Engine selection uses `spec.engine.name` (`"vLLM"` or `"SGLang"`); Ascend NPU uses `AcceleratorType` `Manufacturer == "ascend"`.
   - Boot ceilings are parsed from `role.ExtraArgs` when explicitly declared and validated against engine-reported boot defaults at runtime.

| Canonical Key (`runtimeControl.parameters`) | Engine & Accelerator | Execution Path & Invariants | Validation & `extraArgs` Ceiling |
|---|---|---|---|
| `scheduling.maxActiveRequests` | `vLLM` (NVIDIA, Ascend), `SGLang` | Sentinel `POST /gpustack/v1/runtime_config` → vLLM `EngineCore` utility RPC setting `scheduler.max_num_active_reqs` (never mutates `max_num_running_reqs`); SGLang live admission gate (never mutates physical pool size) | Int `>= 1`; `<=` `--max-num-seqs` (vLLM) or `--max-running-requests` (SGLang) when declared |
| `scheduling.maxBatchedTokens` | `vLLM` (NVIDIA, Ascend), `SGLang` | Sentinel `POST /gpustack/v1/runtime_config` → vLLM `scheduler.max_num_scheduled_tokens`; SGLang page-aligned `chunked_prefill_size` | Int `>= 1`; `<=` `--max-num-batched-tokens` (vLLM) or `--chunked-prefill-size` (SGLang) when declared |
| `scheduling.longPrefillTokenThreshold` | `vLLM` (NVIDIA, Ascend) | Sentinel `POST /gpustack/v1/runtime_config` → `scheduler_config.long_prefill_token_threshold` | Int `>= 0`; `<=` effective `maxBatchedTokens` when declared |
| `scheduling.prefillMaxRequests` | `SGLang` | Sentinel `POST /gpustack/v1/runtime_config` → `get_context().override(prefill_max_requests=val)` | Int `>= 1`; `<=` effective `maxActiveRequests` when declared |
| `scheduling.scheduleConservativeness` | `SGLang` | Sentinel `POST /gpustack/v1/runtime_config` → updates `schedule_conservativeness` AND atomically rebases `scheduler.new_token_ratio_tracker` | Decimal in `[0.1, 2.0]` |
| `scheduling.prefillDecodeInterval` | `SGLang` | Sentinel `POST /gpustack/v1/runtime_config` → updates `scheduler.prefill_decode_interval` AND clamps/resets `_prefill_decode_interval_remaining` | Int `>= 0` (`0` disables deferral immediately) |
| `scheduling.cacheAwareWindow` & `scheduling.cacheAwareThreshold` | `vLLM` (NVIDIA, Ascend) | Sentinel `POST /gpustack/v1/runtime_config` → updates BOTH `scheduler_config` AND cached `Scheduler` instance attributes (`scheduler.py:415-425`) | Window int `>= 0`; threshold decimal in `[0.0, 1.0]` |
| `scheduling.ppMaxMicroBatchSize` | `SGLang` | Sentinel `POST /gpustack/v1/runtime_config` → in-process `SchedulerInternalStateController` (`pp_max_micro_batch_size`, zero `SGLANG_ADMIN_API_KEY` needed) | Int `>= 1`; requires `ppSize > 1` when declared |
| `scheduling.maxInflightKVLoads` & `scheduling.maxAsyncKVLoadsPerStep` | `vLLM` (NVIDIA, Ascend) | Sentinel `POST /gpustack/v1/runtime_config` → `scheduler_config.max_num_inflight_kv_loads` & `max_async_kv_loads_per_step` | Int `>= 1`; requires KV connector active (`kvCache` or `kvTransfer`) |
| `speculative.acceptThresholdSingle` & `speculative.acceptThresholdAcc` | `SGLang` | Sentinel `POST /gpustack/v1/runtime_config` → in-process `set_internal_state` (`speculative_accept_threshold_single/acc`) verifying every rank boolean | Decimal in `[0.0, 1.0]` |
| `speculative.draftBudgetFraction` | `SGLang` | Sentinel `POST /gpustack/v1/runtime_config` → in-process `set_internal_state` (`dspark_force_budget_frac`) + live readback | Decimal in `(0.0, 1.0]` |
| `elastic.dataParallelWidth` | `vLLM` (NVIDIA, Ascend) | Unified Elastic EP collective realization and `/scale_elastic_ep` resize state machine + post-resize replay of all other `runtimeControl.parameters` | Int in `[2, 64]`; presence immutable after creation; monotonic non-decreasing; `<=` `--elastic-ep-max-dp-size` when declared; `replicas=1`, `size=1` |

4. **F4 · Unified Envoy Inference Allowlist (Including Multimodal Routes) & Zero-Intrusion `gpustack-engine-sentinel`**:
   - **Shared Envoy inference allowlist across all three routers**:
     - Allowlist standard LLM routes (`POST /v1/chat/completions`, `POST /v1/completions`, `POST /v1/embeddings`, `POST /v1/responses`, `POST /v1/rerank`, `POST /v1/score`, `POST /v1/classify`, `POST /v1/audio/transcriptions`, `POST /v1/messages`, `POST /v1/messages/count_tokens`, `GET /v1/models`, `GET /v1/models/`, `POST /tokenize`, `POST /v1/tokenize`, `POST /detokenize`, `POST /v1/detokenize`, `POST /v1/chat/completions/render`, `GET /health`) PLUS multimodal inference routes for `vllm-omni` / `sglang-omni` (`POST /v1/audio/speech`, `POST /v1/images/generations`, `POST /v1/images/edits`, `POST /v1/videos`, `GET /v1/videos/`), with `normalize_path: true`, `merge_slashes: true`, `path_with_escaped_slashes_action: REJECT_REQUEST`, and a trailing `403` `direct_response` catch-all (disabling `ext_proc` on deny for `llm-d-router`).
     - For `vllm-router` and `sglang-gateway`, when any managed accelerated non-`Encode` role is present, front the loopback router (`127.0.0.1:8001`) with the Envoy sidecar (`0.0.0.0:8000`) plus an unexposed Pod-internal `:9091` observer listener forwarding only `GET /observer/endpoints`.
   - **Unpacked read-only `gpustack-engine-sentinel` package (`engine-sentinel/python/`)**:
     - Staged flat into `/etc/gpustack/lib/engine-sentinel` in [`pack/gpustack-operator/Dockerfile`](../pack/gpustack-operator/Dockerfile) (version locked to `gpustack-operator` via `hatch-vcs` and `GPUSTACK_GIT_VERSION`, copied atomically via `copy-dir.sh` to `/var/lib/gpustack/operator/lib/engine-sentinel`) and injected by default on all managed accelerated non-`Encode` roles (mounted read-only on `PYTHONPATH` with `GPUSTACK_ENGINE_SENTINEL_ENABLE=1`; CPU-only and unmanaged `role.command` roles remain untouched).
     - Hooks `vLLM` (`vllm.entrypoints.openai.api_server` / `vllm.v1.engine.core.EngineCore`), `SGLang` (`sglang.srt.entrypoints.http_server` / spawned `Scheduler` children), AND falls back universally via `uvicorn.Config` (covering `vllm-omni`, `sglang-omni`, and future engine entrypoint refactors so kubelet `/gpustack/v1/{live,ready}` probes never 404) strictly through `sitecustomize.py` post-import hooks (with visited-path anti-recursion guard) — **never touching `VLLM_PLUGINS`, never wrapping container `argv`, never setting `VLLM_SERVER_DEV_MODE=1`, and never managing API keys or auth Secrets**.
     - Provides `python3 -m gpustack_engine_sentinel.drain` to replace the 30-line inline `PreStop` Python script in [`model_deployment_drain.go`](../pkg/worker/controllers/worker/model_deployment_drain.go) across all sentinel-injected roles.

5. **F5 · Role-Level `pause`, Unified Probes & Readiness Latch, Sharded Ledger, Incarnation-Relative Drain, and Scaler Guard**:
   - **Unified probes & boot-local readiness latch**: Every sentinel-injected engine mints a fresh `boot_id` UUID. `startupProbe` and `livenessProbe` read `GET /gpustack/v1/live` (`200` while alive, including paused; `503` on `EngineDeadError` or crashed scheduler subprocess, without triggering SGLang token generation). `readinessProbe` reads `GET /gpustack/v1/ready` (`503` while engine starting, paused, or awaiting controller `POST /gpustack/v1/ack` when `GPUSTACK_ENGINE_SENTINEL_REQUIRE_ACK=1`).
   - **Role-level `pause: true`**:
     - Works on any role: withdraws `modeldeployment.gpustack.ai/endpoint-eligible` (so both Managed Router and Role Service stop sending traffic), observes router withdrawal, runs incarnation-relative in-flight drain (`DrainReader` bound to `(podUID, bootID)` regardless of `restartCount > 0`), and on sentinel-injected roles sets ASGI pause mode (`POST /gpustack/v1/pause` with `{"paused": true}`) so direct Pod-IP inference requests and `/gpustack/v1/ready` return `503` without `VLLM_SERVER_DEV_MODE=1`.
     - Returning `pause: false` clears sentinel pause mode, replays parameters if needed, re-ACKs, and re-opens `endpoint-eligible`.
   - **Sharded durable ledger & ambiguous-POST fencing**: Per-role `ConfigMap` (`runtime-control-<deploymentUID>-<roleHash>`) with uncached `APIReader` reads, CAS updates, owner/UID/generation guards, dead-incarnation compaction, `5s` per-call HTTP timeout, and live-readback recovery on `AmbiguousOperation`.
   - **Removal restoration**: Clearing `runtimeControl.parameters` or removing keys resets changed runtime parameters back to engine boot defaults before reporting `Synced` / `NotConfigured`.
   - **`ModelDeploymentScaler` pause guard**: `scaler-binding` permits `pause` and `runtimeControl` edits; [`model_deployment_scaler_planner.go`](../pkg/worker/controllers/worker/model_deployment_scaler_planner.go) gates normal, rollback, and restore replica writes on desired (`role.Pause == false`) AND observed (`status.roles[*].runtimeControl.paused == false` at `observedGeneration == md.Generation`), setting `Blocked=True` (`Reason=RolePaused`) while held.

### Boundaries

- **Always:**
  - Inject `gpustack-engine-sentinel` by default on managed accelerated non-`Encode` roles while leaving CPU-only and unmanaged (`role.command`) roles untouched.
  - Enforce the Envoy method/path inference allowlist across all three managed routers.
  - Keep `VLLM_PLUGINS`, `--load-format`, and container `argv` completely untouched by `gpustack-engine-sentinel`.
  - Keep north-south authentication out of `ModelDeployment` (zero operator-managed API keys or per-deployment auth Secrets).
  - Withdraw `endpoint-eligible`, observe router withdrawal, and complete incarnation-relative drain before marking a paused role drained or issuing elastic resizes.
  - Run `make lint`, `make lint python`, and `make lint docs` before completion.
- **Ask first:**
  - Re-introducing GPU sleep/KV-release power states (`SleepLevel1`/`SleepLevel2`/`ReleaseKVCache`) before a Kubernetes GPU reclaim/sharing design is settled.
  - Enabling dynamic LoRA execution before `ModelArtifact` supports LoRA weights.
- **Never:**
  - Mutate `VLLM_PLUGINS`, container `argv`, `--load-format`, or `scheduler.max_num_running_reqs`.
  - Inject `VLLM_SERVER_DEV_MODE=1`, `VLLM_ALLOW_RUNTIME_LORA_UPDATING=true`, `GPUSTACK_RUNTIME_CONTROL_TOKEN`, or `SGLANG_ADMIN_API_KEY`.
  - Rely on runtime `pip install` or block controller reconcile workers on long HTTP waits (`5s` per-call timeout).

### Risks and Mitigations

- **`vllm-omni` plugin groups (`vllm_omni.general_plugins`, `vllm_omni.platform_plugins`) read `VLLM_PLUGINS`** → Never set or modify `VLLM_PLUGINS`; load `gpustack-engine-sentinel` strictly through `sitecustomize.py` post-import hooks.
- **Upstream vLLM / SGLang entrypoint refactors or omni backends break private hook names and cause probe 404s** → Hook `uvicorn.Config` as a universal idempotent ASGI fallback so `/gpustack/v1/{live,ready,pause}` always mount on any uvicorn-served engine (including `vllm-omni` and `sglang-omni`), and degrade `/gpustack/v1/runtime_config` cleanly via `422 unsupported_fields` on internal attribute drift.
- **Duplicate `sitecustomize.py` copies on `sys.path` cause mutual recursion** → Track visited resolved paths in `sys._gpustack_sitecustomize_visited` and chain strictly downstream entries in `sys.path`.
- **User configures `--api-key` in `extraArgs` on their own** → External Gateway owns auth; if engine sentinel or metrics probes fail due to user-supplied `--api-key`, report condition gracefully without managing or forbidding `--api-key`.
- **Same-UID container restart retains Pod UID and `endpoint-eligible` label** → Gate `readinessProbe` on `GET /gpustack/v1/ready` fail-closed `boot_id` latch until replay + `POST /gpustack/v1/ack` succeeds.
- **Upstream engine parameter rename/deprecation** → `runtimeControl.parameters` is a loosely-coupled `map[string]string` validated and translated by `pkg/worker/runtimecontrol/translator.go`, isolating engine churn from CRD schemas.
- **Ambiguous `5s` timeout on mutating POST** → Fence in sharded ledger (`Reason=AmbiguousOperation`) and reconcile from live readback before retrying.

## Design Details

### Commands

Verification environment: **local workstation** (`darwin/arm64`, Go 1.27.3, `uv 0.8.24`). Live GPU/NPU qualification (`vLLM` NVIDIA, `vLLM` Ascend, `SGLang` NVIDIA) runs via `gpustack-operator-e2e` before promoting `Status` to `Shipped`.

```bash
make generate
CGO_ENABLED=1 go test -tags 'goccy netgo' -race ./api/worker/v1/... ./api/worker/v1alpha1/... ./pkg/worker/kvcache/router/... ./pkg/worker/runtimecontrol/... ./pkg/worker/elasticengine/...
CGO_ENABLED=1 go test -tags 'goccy netgo' -race ./pkg/worker/webhooks/worker/... -run 'TestValidateModelDeployment'
CGO_ENABLED=1 go test -tags 'goccy netgo' -race ./pkg/worker/controllers/worker/... -run 'TestModelDeploymentRuntimeControl|TestModelDeploymentPodSpecHash|TestModelDeploymentLegacyRender|TestRenderModelDeployment|TestModelDeploymentRouter|TestModelDeploymentReplacementSlot|TestModelDeploymentElastic|TestModelDeploymentGoldenYAML|TestModelDeploymentScaler'
uv --directory engine-sentinel/python run pytest -v
make lint
make lint python
make lint docs
```

### Project Structure

| Area | Responsibility |
|---|---|
| `api/worker/v1alpha1/model_deployment.go`, `api/worker/v1alpha1/model_deployment_scaler.go`, generated protobuf/deepcopy/OpenAPI/CRDs/applyconfigurations | Removes `runtimeBounds` and `elasticEp`; adds `spec.model.loraAdapters` (reserved), `spec.roles[*].pause`, `spec.roles[*].runtimeControl` (`parameters` only), lean `status.roles[*].runtimeControl`, and `ModelDeploymentScalerReasonRolePaused`. |
| `engine-sentinel/python/`, `pack/gpustack-operator/Dockerfile`, `pack/gpustack-operator/rootfs/usr/bin/copy-dir.sh`, `hack/lint.sh` | Unpacked pure-Python `gpustack-engine-sentinel` package (version locked to `gpustack-operator` via `hatch-vcs`) staged flat into `/etc/gpustack/lib/engine-sentinel` and atomically copied to `/var/lib/gpustack/operator/lib/engine-sentinel`: `sitecustomize.py` post-import hooks for vLLM, SGLang, and universal `uvicorn.Config` fallback (zero `VLLM_PLUGINS` / `argv` mutation, zero auth middleware), `/gpustack/v1/{ready,live,ack,pause,runtime_config}`, and `python3 -m gpustack_engine_sentinel.drain` PreStop module. |
| `pkg/worker/kvcache/router/`, `pkg/worker/controllers/worker/model_deployment_router.go` | Shared Envoy method/path inference allowlist (LLM + multimodal `vllm-omni`/`sglang-omni` endpoints) across all three router profiles + Pod-internal `:9091` observer listener for loopback-bound argv routers. |
| `pkg/worker/runtimecontrol/` | Unified `Translator` registry (`translator.go`), canonical `appliedConfigDigest` (`digest.go`), bounded-timeout no-redirect TLS HTTP client (`client.go`, no auth secrets/sleep/LoRA), role-sharded `ConfigMap` ledger (`ledger.go`), and incarnation-relative `DrainReader` (`drain.go`). |
| `pkg/worker/webhooks/worker/` | Validates reserved `spec.model.loraAdapters` rejection, `pause`, `runtimeControl` scope gates, Translator-driven `parameters` validation against `(engine, manufacturer)` and parsed `extraArgs` ceilings, folded `elastic.dataParallelWidth` immutability/capacity rules, router overrides, and `scaler-binding` coexistence. |
| `pkg/worker/controllers/worker/` | Dual-hash router currency, default `engine-sentinel` rendering (`modelDeploymentRenderVersion = 6`, `PYTHONPATH`, `/gpustack/v1/{live,ready}` probes, `PreStop` drain module, zero auth Secrets/dev-mode/argv wrapping), role-level `pause` + `runtimeControl.parameters` + folded Elastic EP reconciliation, and `ModelDeploymentScaler` `RolePaused` guard. |
| `docs/modules/model-deployment/`, `docs/walkthroughs/model-deployment/elastic-ep.md`, `docs/README.md` | Documents `runtimeControl`, `pause`, reserved `spec.model.loraAdapters`, folded `elastic.dataParallelWidth`, `engine-sentinel`, multimodal Envoy allowlist, and router hash decoupling. |

### Implementation Plan

Refactor DAG (`{R0a ∥ R0b} → R1 → R5 → {R2 ∥ R3 ∥ R7} → R6 → R8`):

- [x] **R0a · Extend unified Envoy inference allowlist for multimodal (`vllm-omni`/`sglang-omni`) routes**
      Blocked by: None
      Owns: `pkg/worker/controllers/worker/model_deployment_router.go`, `pkg/worker/controllers/worker/model_deployment_router_test.go`
      Gate: review
      Acceptance: Adds multimodal inference routes (`POST /v1/audio/speech`, `POST /v1/images/generations`, `POST /v1/images/edits`, `POST /v1/videos`, `GET /v1/videos/`) to the shared Envoy allowlist while keeping all control/admin/elastic/sentinel routes returning `403`, preserves `:9091` internal observer listener, updates pinned router hashes, and passes YAML route-matcher tests across all three router profiles (supports F4).
      Verify: `CGO_ENABLED=1 go test -tags 'goccy netgo' -race ./pkg/worker/kvcache/router/... ./pkg/worker/controllers/worker/... -run 'TestRenderModelDeploymentRouter|TestModelDeploymentRouter|TestSyncModelDeploymentRouter'`

- [x] **R0b · Replace `runtime-control/python` with zero-intrusion `engine-sentinel/python` (`gpustack-engine-sentinel`)**
      Blocked by: None
      Owns: `engine-sentinel/python/**`, `runtime-control/python/**` (delete), `pack/gpustack-operator/Dockerfile`, `hack/lint.sh`
      Gate: review
      Acceptance: Renames and streamlines Python package into `engine-sentinel/python/` (`gpustack-engine-sentinel`) staged at `/etc/gpustack/lib/engine-sentinel/<STAGE_VERSION>`; removes `auth.py`, `vllm/bootstrap.py`, `VLLM_PLUGINS` manipulation, and sleep/wake/LoRA handlers; hooks vLLM, SGLang, AND `uvicorn.Config` universal ASGI fallback (covering `vllm-omni`/`sglang-omni` and future entrypoint refactors) strictly via `sitecustomize.py` post-import hooks (with visited-path anti-recursion guard) under `GPUSTACK_ENGINE_SENTINEL_ENABLE=1`; implements `/gpustack/v1/{ready,live,ack,pause,runtime_config}` (pause-aware and SGLang-starting-aware `/ready`, SGLang in-process `set_internal_state` without `SGLANG_ADMIN_API_KEY`, ASGI `503` pause gate without `VLLM_SERVER_DEV_MODE=1`); adds `gpustack_engine_sentinel.drain` CLI module matching `model_deployment_drain.go` 2-consecutive-idle PreStop semantics; updates `Dockerfile` and `hack/lint.sh`; passes `pytest` and `make lint python` (supports F3, F4, and F5).
      Verify: `uv --directory engine-sentinel/python run pytest -v && make lint python`

- [x] **R1 · Refactor API types: remove `runtimeBounds`/`elasticEp`, add `pause`, reserved `spec.model.loraAdapters`, and KV `runtimeControl`**
      Blocked by: R0a, R0b
      Owns: `api/worker/v1alpha1/model_deployment.go`, `api/worker/v1alpha1/model_deployment_scaler.go`, `api/worker/v1alpha1/generated.*`, `api/worker/v1alpha1/zz_generated.*`, `api/worker/zz_generated.openapi.*`, `api/worker/v1/**`, `pkg/kubeclients/applyconfiguration/**`
      Gate: review
      Acceptance: Removes `ModelDeploymentRoleRuntimeBounds`, `ModelDeploymentRoleElasticEP`, `ModelDeploymentRuntimeScheduling`, `ModelDeploymentRuntimeSpeculative`, `ModelDeploymentRuntimeLifecycle`, and `ModelDeploymentScalerReasonRoleNotActive`; adds `ModelDeploymentModel.LoRAAdapters` (with `ArtifactRef`, `SubPath`, `Pinned`), `ModelDeploymentRole.Pause`, lean `ModelDeploymentRoleRuntimeControl` (`Parameters map[string]string` only), lean `ModelDeploymentRoleRuntimeControlStatus` (`Paused`, no `ActivePowerState`/`LoadedLoRAAdapters`), and `ModelDeploymentScalerReasonRolePaused = "RolePaused"`; regenerates all codegen outputs cleanly via `$HOME` worktree and passes `api/worker/v1` + `api/worker/v1alpha1` round-trip, SSA, CRD, and `api-descriptions.sh` checks (supports F2).
      Verify: `CGO_ENABLED=1 go test -tags 'goccy netgo' -race ./api/worker/v1/... ./api/worker/v1alpha1/... && bash hack/check/api-descriptions.sh .`

- [x] **R5 · Refactor `pkg/worker/runtimecontrol`: add unified Translator Registry and strip auth/sleep/LoRA**
      Blocked by: R1
      Owns: `pkg/worker/runtimecontrol/**`
      Gate: review
      Acceptance: Implements `translator.go` (canonical parameter registry for `scheduling.*`, `speculative.*`, and `elastic.dataParallelWidth` with `(engine, manufacturer)` matrix, `extraArgs` ceiling checks, value canonicalization, `ParseElasticWidth`, and engine apply payload builder); simplifies `digest.go` to digest canonicalized `parameters` + `pause`; removes auth tokens, sleep/wake/KV-release, and LoRA HTTP calls from `client.go` while adding `SetPause(ctx, target, paused)` (`POST /gpustack/v1/pause`) and keeping `5s` timeout, no-redirect, TLS, ambiguous-POST fencing, sharded `ConfigMap` ledger (`ledger.go`), and incarnation-relative `DrainReader` (`drain.go`); passes `-race` tests with `>= 85%` coverage (supports F3 and F5).
      Verify: `CGO_ENABLED=1 go test -tags 'goccy netgo' -race -cover ./pkg/worker/runtimecontrol/...`

- [x] **R2 · Refactor rendering, PreStop drain module, parallelism, and replacement slots for default `engine-sentinel` + folded Elastic EP**
      Blocked by: R1, R5
      Owns: `pkg/worker/controllers/worker/model_deployment_render.go`, `pkg/worker/controllers/worker/model_deployment_render_test.go`, `pkg/worker/controllers/worker/model_deployment_drain.go`, `pkg/worker/controllers/worker/model_deployment_drain_test.go`, `pkg/worker/controllers/worker/model_deployment_parallelism.go`, `pkg/worker/controllers/worker/model_deployment_parallelism_test.go`, `pkg/worker/controllers/worker/model_deployment_fabric.go`, `pkg/worker/controllers/worker/model_deployment_group_shape_test.go`, `pkg/worker/controllers/worker/model_deployment_group_status_test.go`, `pkg/worker/controllers/worker/model_deployment_replacement_slot.go`, `pkg/worker/controllers/worker/model_deployment_replacement_slot_test.go`, `pkg/worker/controllers/worker/testdata/legacy-render/**`
      Gate: review
      Acceptance: Removes all `runtimeBounds`, auth `SecretKeyRef`, `VLLM_SERVER_DEV_MODE`, `VLLM_ALLOW_RUNTIME_LORA_UPDATING`, `SGLANG_ADMIN_API_KEY`, `SGLANG_ENABLE_HEALTH_ENDPOINT_GENERATION`, and vLLM argv wrapper rendering; bumps `modelDeploymentRenderVersion = 6` and injects read-only `/var/lib/gpustack/operator/lib/engine-sentinel/<STAGE_VERSION>` (`STAGE_VERSION` synced by unit test) with `GPUSTACK_ENGINE_SENTINEL_ENABLE=1`, `/gpustack/v1/{live,ready}` probes, and `python3 -m gpustack_engine_sentinel.drain` PreStop command by default on all managed accelerated non-`Encode` roles (leaving CPU-only and unmanaged `role.command` roles untouched); hashes immutable `hasElasticEP` boolean while excluding `pause`, `runtimeControl.parameters` values, and `spec.model.loraAdapters`; refreshes `testdata/legacy-render/**`; migrates all `role.ElasticEP` references to `runtimecontrol.ParseElasticWidth` (supports F1, F4, and F5).
      Verify: `CGO_ENABLED=1 go test -tags 'goccy netgo' -race ./pkg/worker/controllers/worker/... -run 'TestModelDeploymentPodSpecHash|TestModelDeploymentLegacyRender|TestRenderModelDeployment|TestModelDeploymentReplacementSlot|TestModelDeploymentDrain|TestModelDeploymentParallelism'`

- [x] **R3 · Refactor validating webhooks for Translator-driven `parameters`, `pause`, reserved `loraAdapters`, and folded `elastic.dataParallelWidth`**
      Blocked by: R1, R5
      Owns: `pkg/worker/webhooks/worker/**`
      Gate: review
      Acceptance: Rejects non-empty `spec.model.loraAdapters` with actionable message pending `ModelArtifact` LoRA support; validates `runtimeControl` scope gates (rejecting `runtimeControl` on unmanaged `role.Command`, `Encode`, CPU-only, and SGLang Ray roles) and delegates `runtimeControl.parameters` validation to `runtimecontrol.Translator` against `(engine, manufacturer)` and parsed `extraArgs` ceilings; migrates all `model_deployment_elastic*.go`, `model_deployment_epd*.go`, and `model_deployment_scaler*.go` webhook logic from `role.ElasticEP` to `runtimeControl.parameters["elastic.dataParallelWidth"]` (preserving immutable presence, monotonic scale-up, `replicas=1`, `size=1`, and capacity/TP rules); permits `pause` and `runtimeControl` edits under `scaler-binding`; removes all `runtimeBounds`/`powerState`/auth-env rules; passes full webhook `-race` suite (supports F2, F3, and F5).
      Verify: `CGO_ENABLED=1 go test -tags 'goccy netgo' -race ./pkg/worker/webhooks/worker/...`

- [x] **R7 · Update `ModelDeploymentScaler` to guard on `role.Pause` (`RolePaused`) and folded Elastic EP**
      Blocked by: R1, R5
      Owns: `pkg/worker/controllers/worker/model_deployment_scaler.go`, `pkg/worker/controllers/worker/model_deployment_scaler_planner.go`, `pkg/worker/controllers/worker/model_deployment_scaler_prior_identity.go`, `pkg/worker/controllers/worker/model_deployment_scaler_priors.go`, `pkg/worker/controllers/worker/model_deployment_scaler_test.go`
      Gate: review
      Acceptance: Gates all scaler replica write paths (normal, rollback, restore) on desired `role.Pause == false` and observed `Paused == false` at `ObservedGeneration == md.Generation`, reporting `Blocked=True` (`Reason=RolePaused`); updates any `role.ElasticEP` prior/planner lookups to `runtimecontrol.ParseElasticWidth`; passes `TestModelDeploymentScaler` `-race` tests (supports F5).
      Verify: `CGO_ENABLED=1 go test -tags 'goccy netgo' -race ./pkg/worker/controllers/worker/... -run 'TestModelDeploymentScalerRuntimeControl|TestModelDeploymentScalerPause|TestModelDeploymentScaler'`

- [x] **R6 · Refactor controller wiring: drop auth Secret, wire default sentinel, role `pause`, KV `parameters`, and folded Elastic EP**
      Blocked by: R2, R3, R7
      Owns: `pkg/worker/controllers/worker/model_deployment_runtime_control.go`, `pkg/worker/controllers/worker/model_deployment_runtime_control_test.go`, `pkg/worker/controllers/worker/model_deployment.go`, `pkg/worker/controllers/worker/model_deployment_status.go`, `pkg/worker/controllers/worker/model_deployment_pod_group.go`, `pkg/worker/controllers/worker/model_deployment_service.go`, `pkg/worker/controllers/worker/model_deployment_router.go`, `pkg/worker/controllers/worker/model_deployment_router_observation.go`, `pkg/worker/controllers/worker/model_deployment_router_test.go`, `pkg/worker/controllers/worker/model_deployment_elastic_realization.go`, `pkg/worker/controllers/worker/model_deployment_elastic_workload.go`, `pkg/worker/controllers/worker/model_deployment_elastic_reconcile.go`, `pkg/worker/controllers/worker/model_deployment_elastic_allocation_test.go`, `pkg/worker/controllers/worker/model_deployment_elastic_reconcile_test.go`, `pkg/worker/controllers/worker/model_deployment_elastic_workload_test.go`
      Gate: review
      Acceptance: Removes per-deployment auth `Secret` lifecycle and Secret watch; wires `modelDeploymentRouterRequiresAllowlistProxy` to enable Envoy allowlist sidecar whenever any managed accelerated non-`Encode` role exists; handles role-level `pause: true` (endpoint withdrawal + router observation + incarnation-relative drain + sentinel `/gpustack/v1/pause`); reconciles `runtimeControl.parameters` via `runtimecontrol.Translator` + `/gpustack/v1/runtime_config` + `/gpustack/v1/ack`; migrates all elastic realization/reconcile/workload code and tests from `role.ElasticEP` to `runtimeControl.parameters["elastic.dataParallelWidth"]`, serializing resize against `pause` and replaying non-elastic `parameters` post-resize; passes all controller `-race` tests (supports F1, F3, F4, and F5).
      Verify: `CGO_ENABLED=1 go test -tags 'goccy netgo' -race ./pkg/worker/controllers/worker/... -run 'TestModelDeploymentRuntimeControl|TestModelDeploymentPodSpecHash|TestModelDeploymentLegacyRender|TestRenderModelDeployment|TestModelDeploymentRouter|TestModelDeploymentElastic|TestModelDeploymentGoldenYAML'`

- [x] **R8 · Update Golden YAML scenarios (all refreshed fixtures + 3 runtime-control fixtures) and user documentation**
      Blocked by: R6
      Owns: `pkg/worker/controllers/worker/model_deployment_golden_yaml_test.go`, `pkg/worker/controllers/worker/testdata/golden-render/**`, `docs/modules/model-deployment/**`, `docs/walkthroughs/model-deployment/elastic-ep.md`, `docs/reference/settings.md`, `docs/README.md`
      Gate: review
      Acceptance: Refreshes golden YAML scenarios for default `engine-sentinel` injection (`modelDeploymentRenderVersion = 6`, Envoy allowlist sidecar, `gpustack_engine_sentinel.drain` PreStop), `server-elastic-ep`, and the 3 `runtime-control-*` scenarios (`vLLM` NVIDIA, `vLLM` Ascend, `SGLang` NVIDIA) with deterministic `-count=2` pass; updates `runtime-control.md`, `elastic-ep.md`, `walkthroughs/model-deployment/elastic-ep.md`, sibling cross-links, and `docs/README.md`; passes `TestModelDeploymentGoldenYAML`, `make lint`, `make lint python`, and `make lint docs` (supports F1–F5).
      Verify: `CGO_ENABLED=1 go test -count=2 -tags 'goccy netgo' -race ./pkg/worker/controllers/worker/... -run 'TestModelDeploymentGoldenYAML' && make lint && make lint docs`

### Test Plan
[ ] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Unit tests
- `api/worker/v1` & `api/worker/v1alpha1`: JSON, protobuf, SSA, omission, `v1alpha1` ↔ `v1` round-trip, and embedded CRD schema coverage for `spec.model.loraAdapters`, `spec.roles[*].pause`, `spec.roles[*].runtimeControl` (`enabled` + `parameters`), `status.roles[*].runtimeControl`, and `ModelDeploymentScalerReasonRolePaused`.
- `engine-sentinel/python`: `pytest` suites covering `sitecustomize.py` anti-recursion and dormant mode, zero `VLLM_PLUGINS` mutation (`vllm-ascend`, `vllm-omni`, `model-transfer` preserved), fail-closed `/gpustack/v1/ready` + `/gpustack/v1/ack` + `/gpustack/v1/live`, ASGI `/gpustack/v1/pause` (`503` on inference while paused), vLLM `EngineCore` multi-rank utility RPC (`max_num_active_reqs` shrink without mutating `max_num_running_reqs`), fresh-interpreter SGLang `spawn` child hook + in-process `set_internal_state` (zero `SGLANG_ADMIN_API_KEY`), and `gpustack_engine_sentinel.drain` 2-consecutive-idle PreStop CLI behavior.
- `pkg/worker/runtimecontrol`: Translator Registry capability/range/`extraArgs`-ceiling tests, canonical digest normalization, `httptest.Server` client tests (`5s` timeout → `AmbiguousOperationError`, redirect refusal, TLS, `/gpustack/v1/pause`), role-sharded `ConfigMap` ledger CAS/compaction, and incarnation-relative `DrainReader`.
- `pkg/worker/webhooks/worker`: Table-driven `ValidateCreate` / `ValidateUpdate` coverage for reserved `spec.model.loraAdapters` rejection, `pause`, `runtimeControl.enabled` scope gates, Translator-driven `parameters` validation against `extraArgs`, folded `elastic.dataParallelWidth` immutability/capacity/TP rules, router overrides, and `scaler-binding` coexistence.
- `pkg/worker/controllers/worker`: Dual-hash upgrade currency, Envoy allowlist (LLM + multimodal `vllm-omni`/`sglang-omni` routes + `:9091` observer), router-first rollout gate, `PreStop` drain module rendering, role `pause` drain + ASGI gate + resume, fail-closed latch on cold start & same-UID restart, folded Elastic EP resize + pause serialization + post-resize replay, and `ModelDeploymentScaler` `RolePaused` guard.

#### Integration & e2e tests
- Golden YAML scenarios (`TestModelDeploymentGoldenYAML`) covering `server-elastic-ep` and `runtime-control-{vllm-nvidia,vllm-ascend,sglang-nvidia}` plus `make lint`, `make lint python`, and `make lint docs`.
- Live GPU/NPU qualification gate (`gpustack-operator-e2e`):
  - **NVIDIA H200 (`vLLM 0.32.0` + `SGLang 0.5.22`) — Verified**: `case-133` (22/22), `case-134` (23/23: Envoy allowlist, router hash decoupling, core vLLM tuning, partial/full reset, `cacheAware*` `UnsupportedEngineVersion`, Active `ModelDeploymentScaler` `RolePaused`, pause/resume, ledger delete, `PreStop` drain), `case-135` (15/15: `sglang-gateway` Envoy allowlist, 5-knob SGLang tuning, pause/resume, `boot_defaults` reset), `case-136` (10/10: folded `elastic.dataParallelWidth` `2 -> 3` with surviving master UID, continuous inference, and 3-rank parameter replay), `case-137` (`1P+1D` `llm-d-router` per-role pause and KV-load `UnsupportedEngineVersion`, SGLang NGRAM `speculative.acceptThreshold*` live apply and `draftBudgetFraction` `UnsupportedEngineVersion`; `ppMaxMicroBatchSize` hardware-gated on `>= 2` GPUs per node), and same-UID container restart fail-closed latch replay (`restartCount: 0 -> 1`, `boot_id` rotation, digest replay + `/gpustack/v1/ack`).
  - **Ascend NPU (`vLLM` Ascend) — Pending**: Required before promoting `Status` to `Shipped`.

## Alternatives

- **Keep rigid typed `runtimeBounds`, `runtimeControl.scheduling`, `runtimeControl.speculative`, and standalone `elasticEp`**: Rejected. High-churn engine tuning knobs would require constant CRD additions and deprecations, `runtimeBounds` duplicated `extraArgs` ceilings, and separate `elasticEp` fragmented online control.
- **Keep `VLLM_PLUGINS` allowlist bootstrap wrapper**: Rejected after inspecting `vllm-omni/vllm_omni/plugins/__init__.py`. `vllm-omni` reads `VLLM_PLUGINS` for its own `vllm_omni.general_plugins` and `vllm_omni.platform_plugins` groups, so exporting a vLLM-only allowlist breaks `vllm-omni` plugins. Using `sitecustomize.py` post-import hooks uniformly for vLLM and SGLang avoids modifying `VLLM_PLUGINS` and container `argv` altogether.
- **Enable Phase 1 dynamic LoRA hot-loading from raw container `path`**: Rejected. Arbitrary filesystem paths bypass `ModelArtifact` immutability, RBAC, and digest verification and risk accidental injection of unintended adapters; reserving `spec.model.loraAdapters` with `artifactRef` keeps the entry point aligned with `ModelArtifact`.
- **Manage per-deployment API keys / auth Secrets inside the operator**: Rejected. North-south authentication belongs to the external Gateway; east-west isolation uses the Managed Router Envoy inference allowlist and cluster network boundaries, consistent with the rest of `gpustack-operator`.

## Open Questions

1. **GPU memory sleep / KV-release power states (`SleepLevel1`, `SleepLevel2`, `ReleaseKVCache`) and Kubernetes GPU reclaim**: Even when an inference engine offloads weights or KV cache inside a running Pod, Kubernetes Device Plugin and Kueue still count the GPU as allocated to that Pod, preventing other workloads from using the idle accelerator. How should a future phase pair engine sleep/wake with cluster-level GPU reclaim or sharing (such as Kueue preemption/oversubscription, DRA dynamic sharing, or colocated batch execution) so sleeping Pods actually return usable capacity to the cluster?
2. **`ModelArtifact` LoRA delivery and dynamic hot-loading**: Once `ModelArtifact` supports LoRA adapter artifacts (metadata, digest verification, and shared read-only parent directory mounts via `NodeModelStore` / CSI so newly resolved adapters become visible inside running Pods without restarts), enable `spec.model.loraAdapters[*].artifactRef` actuation in `gpustack-engine-sentinel` and the controller.
3. **`vllm-omni` and `sglang-omni` native sentinel adapters**: When `ModelDeployment` adds first-class multi-stage `vllm-omni` / `sglang-omni` serving support, extend `gpustack-engine-sentinel` to hook `sglang_omni.serve.openai_api` and multi-stage diffusion/audio runners alongside standard LLM serving.
