# Spec: KV Events Alignment, Engine Version Resolution, and Parameter Reference Overhaul

Status: Building
Type: Feature

## Summary

This specification aligns `ModelDeployment` KV event publishing, router selection, and synthesized engine parameters with current upstream `vLLM`, `vLLM-Ascend`, `SGLang`, `llm-d-router`, `vllm-router`, and `sgl-router` contracts. It introduces explicit engine version resolution (`spec.engine.version` and `spec.roles[*].engineVersion`) backed by `gpustack/runner` image tag defaulting, adds `sgl-router` while freezing deprecated `sglang-gateway` (with `.auto-sync/` freeze guards and `Base Image` build recommendations whenever `pack/*/Dockerfile` changes), enables version-gated `MooncakeConnectorV2` on Ascend P/D, cleans up retired parameters, and reorganizes the LLM-facing parameter reference around orthogonal `KVCache` and `KVTransfer` version matrices.

## Motivation

### Goals

- **Target users:** Platform operators managing disaggregated inference workloads (`ModelDeployment`) and maintainers operating `.auto-sync/` pipelines, `Base Image` releases, and LLM reference documentation.
- **Accurate KV events across supported engines and routers:**
  - Fix the `llm-d-router` KV event topic defect so every producing role publishes `kv@<model>` instead of empty-model `kv@`.
  - Enable KV event publishing on `vLLM-Ascend` behind `llm-d-router` without rolling standing Pods on operator upgrade.
  - Enable conditional KV event publishing and hash alignment for `vllm-router` when `kv_aware` routing is active (`>= 0.31.0`).
  - Introduce `sgl-router` as the successor to deleted upstream `sgl-model-gateway`, enabling native SGLang KV events over `/server_info` and ZMQ.
- **Deterministic version-aware parameter synthesis:**
  - Re-introduce `spec.engine.version` and `spec.roles[*].engineVersion` with automatic Defaulter backfill from `gpustack/runner` image tags and fail-closed admission for unparseable managed images.
  - Gate retired and version-dependent parameters (`VLLM_USE_V2_MODEL_RUNNER`, `VLLM_EC_SIDE_CHANNEL_PORT`, `vllm-router` `kv_aware` floor `0.31.0`, Ascend `MooncakeConnectorV2` floor `0.30.0`) on resolved engine versions without breaking older images or rolling standing Pods.
- **Actionable `.auto-sync/` packaging workflow:**
  - Freeze deprecated `sglang-gateway` at `gateway-v0.3.1` and track `sgl-router` (`router-v*`) in `.auto-sync/`.
  - Emit structured `Base Image` workflow build recommendations (affected third-party image repositories, targets, and recommended tags) whenever `.auto-sync/` updates any `pack/*/Dockerfile`.
- **Clean, LLM-readable parameter contract:**
  - Reorganize [docs/reference/model-deployment-parameters.md](../docs/reference/model-deployment-parameters.md) so an operator or LLM can read exact rendered flags, environment variables, and mounts before and after each engine version threshold across orthogonal `KVCache` and `KVTransfer` axes.

### Non-Goals

- Enabling `precise-prefix-cache-producer` KV events for `SGLang` behind `llm-d-router` in this change. Upstream `llm-d/llm-d-router` (`v1.0.0` and current `main`) requires `token-producer` with `backend: "vllm"` (`ValidateTokenProducer`), which calls `/v1/chat/completions/render`, an endpoint SGLang does not implement. Track this follow-up in a GitHub TODO issue (Task `T8`) and keep `SGLang + llm-d-router` on `approx-prefix-cache-producer`; SGLang KV events are delivered natively by `sgl-router`.
- Adding `ECMooncakeConnector` for Ascend EPD or `MooncakeConnectorV2` layerwise Push mode (`use_layerwise: true` / `MooncakePushConnector`) in this change. Upstream `llm-d/llm-d-router` added `--ec-connector=ec-mooncake` after `v1.0.0` (commit `bd30a073`, PR `#3529`), and Push mode requires a separate metaserver proxy protocol. Track both in a GitHub TODO issue (Task `T8`).
- Removing the `sglang-gateway` API enum value or deleting its frozen `gateway-v0.3.1` binary from `gpustack/llm-router`. Removal belongs to a future deprecation window after users migrate to `sgl-router`.
- Enabling `Decode`-role KV events for `llm-d-router` in-flight prefill slot release (`prefillCompletionFromKVEvents`, post-`v1.0.0` PR `#3975`).
- Changing standalone Pod KV cache webhook injection (`kvcache.gpustack.ai/inject`) to require an engine version annotation.

## Proposal

The operator resolves a concrete engine version for every new or updated managed `ModelDeployment` role at admission, uses that version to synthesize compatible KV cache, KV transfer, encoder cache, and KV event configurations, adds `sgl-router` alongside frozen `sglang-gateway`, extends `.auto-sync/` with `Base Image` build recommendations when `pack/*/Dockerfile` changes, publishes a version-indexed orthogonal parameter reference, and files tracking TODO issues for deferred upstream capabilities.

### User Stories

#### Story 1
As a platform operator running `vLLM` or `vLLM-Ascend` behind `llm-d-router` or `vllm-router` (`kv_aware`), or `SGLang` behind `sgl-router`, I want KV events published with valid topics and consumed by the matched router, so that disaggregated P/D and multi-replica deployments achieve accurate prefix-cache routing.

#### Story 2
As an `SGLang` operator, I want `ModelDeployment` and `.auto-sync/` to support `sgl-router` while freezing deprecated `sglang-gateway` at `gateway-v0.3.1`, and I want `.auto-sync/` to report which `Base Image` jobs and recommended tags to run whenever a `pack/*/Dockerfile` changes, so that router and transfer image releases stay safe and predictable.

#### Story 3
As a cluster administrator deploying mixed engine image versions (both `gpustack/runner` and custom images), I want `spec.engine.version` and `spec.roles[*].engineVersion` to backfill automatically from `gpustack/runner` tags and accept explicit overrides when tags are unparseable, so that the operator omits retired parameters on newer engines, keeps compatibility defaults on older engines, and fails fast when a version cannot be determined.

#### Story 4
As an Ascend P/D operator on `vLLM-Ascend >= 0.30.0`, I want the operator to render `MooncakeConnectorV2` (Pull mode) automatically while keeping `MooncakeConnectorV1` for `< 0.30.0` and legacy versionless deployments, so that newer Ascend clusters gain renewable KV leases and relaxed TP divisibility without manual connector overrides.

#### Story 5
As an operator or LLM agent auditing `ModelDeployment` behavior, I want an orthogonal, version-indexed parameter reference document showing exact synthesized outputs before and after every version boundary for `KVCache`, `KVTransfer`, `KVEvents`, `ECTransfer`, and `Router`, so that I can predict exact Pod arguments and environment variables without reading Go source files.

### Core Features & Acceptance Criteria

#### 1. Engine Version API, Runner Tag Parsing, and Admission

- **API fields:**
  - Add optional editable `Version string` (`json:"version,omitempty"`, `protobuf:"bytes,3,opt,name=version"`) to `ModelDeploymentEngine` in [api/worker/v1alpha1/model_deployment.go](../api/worker/v1alpha1/model_deployment.go) (exposed through [api/worker/v1/model_deployment.go](../api/worker/v1/model_deployment.go)).
  - Add optional editable `EngineVersion string` (`json:"engineVersion,omitempty"`, `protobuf:"bytes,24,opt,name=engineVersion"`) to `ModelDeploymentRole`.
  - Keep all new docstrings compliant with [hack/check/api-descriptions.sh](../hack/check/api-descriptions.sh) (avoid banned implementation words such as `pointer`, `struct`, `nil-able`, `compile-time`, `import cycle`).
- **Runner image tag grammar (from [gpustack/runner](https://github.com/gpustack/runner)):**
  - Strip any `@sha256:...` digest suffix, split tag after the final `/` (preserving registry ports such as `registry:5000/gpustack/runner:...`), and match `[prefix/]runner:{backend}{backend_version}[-{backend_variant}]-{service}{service_version}[-{suffix}]`, where `backend` is one of `Host|cann|corex|cuda|dtk|hggc|maca|musa|rocm` (case-insensitive `Host`) and `service` is `vllm|sglang` (matching `spec.engine.name` case-insensitively).
  - Extract `{service_version}` (for example `0.34.0`, `0.32.0`, `0.30.0rc1`, `0.30.0.rc1`, `0.5.23`).
- **Version comparison rule:**
  - Validate that any non-empty version matches parseable PEP 440 / SemVer numeric syntax (`major.minor[.patch][rcN|.rcN|.postN]`).
  - Compare milestone feature and retirement gates by `(major, minor, patch)` release tuple so pre-release builds on a release line (`0.30.0rc1`, `0.30.0.rc1`, `0.34.0rc0`) satisfy that line's milestone (`0.30.0`, `0.34.0`).
- **Defaulter webhook behavior ([pkg/worker/webhooks/worker/model_deployment.go](../pkg/worker/webhooks/worker/model_deployment.go)):**
  - On `CREATE`: if `spec.engine.version` is empty and `spec.engine.image` matches the runner grammar for `spec.engine.name`, backfill `spec.engine.version`; for each role, if `roles[i].engineVersion` is empty and `roles[i].image` is non-empty and matches, backfill `roles[i].engineVersion`; leave `roles[i].engineVersion` empty when `roles[i].image` is empty so the role inherits `spec.engine.version`.
  - On `UPDATE` (inspected via `admission.RequestFromContext` old object): backfill `spec.engine.version` only when `spec.engine.image` changed and `spec.engine.version` is empty, and backfill `roles[i].engineVersion` only when `roles[i].image` changed and `roles[i].engineVersion` is empty. Do NOT backfill empty versions on unrelated updates (such as replica scaling or pause toggles) to legacy versionless CRs.
  - Never overwrite a user-supplied `version` or `engineVersion`.
  - If an image tag does not match the runner grammar, is digest-only, or disagrees with `spec.engine.name`, leave the version empty so validation refuses it.
- **Validation and legacy compatibility:**
  - Short-circuit all new version and router validation rules when `md.DeletionTimestamp != nil` so finalizer removal on legacy CRs cannot deadlock.
  - On `CREATE` (and on `UPDATE` when `spec.engine.image`, `spec.engine.version`, `roles[i].image`, or `roles[i].engineVersion` changes):
    - If a managed role (`len(role.Command) == 0`) has an empty effective version (`roles[i].engineVersion` falling back to `spec.engine.version`), reject with an actionable error naming the unparseable image and directing the user to set `spec.engine.version` or `spec.roles[i].engineVersion`.
    - If an image is a parseable `gpustack/runner` tag and the explicit non-empty version for that image disagrees with the parsed runner tag (for example updating `spec.engine.image` while leaving an older `spec.engine.version` in place), reject with an actionable error instructing the user to update or clear the version field alongside the image.
  - For pre-existing stored `ModelDeployment` objects with empty stored version, reconcile treats empty stored version consistently as the legacy baseline (`< 0.30.0`: `MooncakeConnectorV1`, defaulted `VLLM_USE_V2_MODEL_RUNNER=1`) without in-memory re-parsing, preventing mixed V1/V2 rollouts or scale-up pod-spec-hash churn.
  - Fold the role's resolved effective version into `modelDeploymentSpecHashWithRouter` (`canonicalRole.EngineVersion`) so editing `spec.engine.version` on a custom image rolls affected roles cleanly.
- **Acceptance criteria:**
  - Creating a `ModelDeployment` with `gpustack/runner:cuda13.0-vllm0.34.0` and no `version` backfills `spec.engine.version: "0.34.0"`.
  - Creating a `ModelDeployment` with `gpustack/runner:cann9.2-a3-vllm0.32.0` on a role override backfills `roles[i].engineVersion: "0.32.0"` while roles without `roles[i].image` inherit `spec.engine.version`.
  - Creating a `ModelDeployment` with custom image `my-reg/vllm:custom` and empty version fails admission with a clear remediation message; supplying `spec.engine.version: "0.34.0"` admits it.
  - Updating `spec.engine.image` to a different `gpustack/runner` tag while leaving a stale `spec.engine.version` fails admission; clearing `spec.engine.version` on that update backfills the new tag version.
  - Scaling, pausing, or deleting an existing legacy CR with empty version succeeds with zero unintended Pod rollout.

#### 2. `sgl-router` Support, Frozen `sglang-gateway`, and `.auto-sync/` Enhancements

- **Router enum and strict engine-router matrix:**
  - Add `ModelDeploymentRouterSGL = "sgl-router"` (with `ModelDeploymentRouterSGLangGateway = "sglang-gateway"` and deprecated alias `ModelDeploymentRouterSGLang`) alongside `llm-d-router` and `vllm-router`.
  - Enforce engine-router pairing at admission:
    - `sgl-router` and `sglang-gateway` accept `SGLang` only.
    - `vllm-router` accepts `vLLM` (NVIDIA, AMD, and Ascend) only.
    - `llm-d-router` accepts `vLLM` (including Ascend) and `SGLang`.
- **`sgl-router` workload, EndpointSlice discovery, observation, and metrics:**
  - Build `sgl-router` from `sgl-project/sglang` (`ARG SGL_ROUTER_VERSION=router-v0.1.0`, `git sparse-checkout set sgl-router rust`, `cargo build --release --locked --bin sgl-router`) into [pack/llm-router/Dockerfile](../pack/llm-router/Dockerfile) alongside `epp`, `vllm-router`, and frozen `sgl-model-gateway` (`gateway-v0.3.1`).
  - Carry a minimal observer patch `pack/llm-router/patches/sgl-router/0001-observer-endpoints.patch` exposing `GET /observer/endpoints` with the exact JSON schema enforced by [model_deployment_router_observation.go](../pkg/worker/controllers/worker/model_deployment_router_observation.go) (`router.boot_generation`, `router.now_ms`, `registry_revision`, `workers[].selection.*`) so Envoy `:9091` readiness probes and protected-group observation work identically to `vllm-router` and `sglang-gateway`.
  - Render `sgl-router` with `--host`, `--port`, `--model-id <spec.model.name>`, `--chat-routing reorg`, `--service-discovery`, `--service-discovery-namespace <ns>`, EndpointSlice selectors (`--selector` for undivided `Server`, or `--prefill-selector` + `--decode-selector` for PD/EPD; `sgl-router@router-v0.1.0` infers PD mode from those selectors and has no `--pd-disaggregation`, `--service-discovery-port`, or `--prometheus-port` flags), `--dp-aware` for SGLang External DP, and `--request-timeout-secs` when configured.
  - Emit `--no-tokenizer` by default so `sgl-router` starts offline in air-gapped clusters without calling Hugging Face Hub; omit `--no-tokenizer` automatically when `spec.router.extraArgs` provides `--tokenizer-path`, `--bucket-config`, or enables prefix-cache policies (`--policy`/`--prefill-policy`/`--decode-policy` set to `cache_aware`, `score_policy`, or `fused_score`, or `--filter prefix_cache`).
  - Stamp `modeldeployment.gpustack.ai/role-kind=<kind>` on per-role Services (`<md>-<role>`) for `sgl-router` deployments (never on `<md>` or headless replica Services `<md>-<role>-r<n>`) so Kubernetes copies the discriminator onto role `EndpointSlice` objects while role Service selectors enforce `endpoint-eligible=true`. Grant `discovery.k8s.io/endpointslices` (`get, list, watch`) in the `sgl-router` Role RBAC and preserve eligibility retention across quiet reconciles.
  - Wire `sgl-router` into Envoy loopback allowlist sidecar (`127.0.0.1:8001` behind Envoy `:8000` + `:9091` observer listener + metrics exposure since `sgl-router` serves `/metrics` on its main HTTP port), `modelDeploymentEPDMatrix` in [model_deployment_connector_epd.go](../pkg/worker/controllers/worker/model_deployment_connector_epd.go), and router metrics scraping/parsing in [pkg/worker/extensionapis/worker/model_deployment.metrics.go](../pkg/worker/extensionapis/worker/model_deployment.metrics.go) and [pkg/worker/extensionapis/worker/model_deployment.metrics_parse.go](../pkg/worker/extensionapis/worker/model_deployment.metrics_parse.go).
- **Freeze `sglang-gateway` and enhance `.auto-sync/` with `Base Image` build recommendations:**
  - Mark `sglang-gateway` deprecated in API docstrings and docs; pin `SGLANG_GATEWAY_VERSION=gateway-v0.3.1` as permanently frozen because upstream deleted `sgl-model-gateway` in `sgl-project/sglang` PR `#50732` (commit `2588d80d062`, 2026-11-16).
  - Update [.auto-sync/router-taskpack.yaml](../.auto-sync/router-taskpack.yaml), [.auto-sync/scripts/preflight-router.sh](../.auto-sync/scripts/preflight-router.sh), [.auto-sync/checks/validate_router.sh](../.auto-sync/checks/validate_router.sh), [.auto-sync/instructions/router.md](../.auto-sync/instructions/router.md), [.auto-sync/proposals/router.json](../.auto-sync/proposals/router.json), and [hack/check/autosync-selftest.sh](../hack/check/autosync-selftest.sh) so auto-sync stages `sglang-gateway` (`sgl-model-gateway`) and `sgl-router` (`sgl-router rust`) in separate directories, tracks `router-v*` for `sgl-router`, skips upstream tag bumps for `sglang-gateway`, and fails validation if a candidate modifies `SGLANG_GATEWAY_VERSION` or `pack/llm-router/patches/sglang-gateway/`.
  - Add a shared `.auto-sync/` helper (invoked by `router` and `kv-transfer` preflight/validation scripts) that detects modified `pack/*/Dockerfile` (or sibling `patches/`) mapped to [.github/workflows/base-image.yml](../.github/workflows/base-image.yml) (`llm-router`, `mirrored-mooncake`, `model-transfer`, `mirrored-modelexpress-server`), computes the required `Base Image` workflow dispatches (`repository`, `target`, `recommended_tag`, `reason`, and `gh workflow run "Base Image"` command), records them in `base_image_builds` inside the Stage A proposal JSON and summary, and validates them in `validate_router.sh`, `validate_kv_transfer.sh`, and `hack/check/autosync-selftest.sh`.
- **Acceptance criteria:**
  - `sgl-router` renders valid args, RBAC (`endpointslices`), role-Service `role-kind` labels, Envoy allowlist sidecar, observation contract, EPD admission rows, and metrics parsing for SGLang `Server`, `PD`, `EPD`, and `External DP`.
  - Existing `sglang-gateway` deployments render identically to before.
  - `bash hack/check/autosync-selftest.sh` passes, rejects any candidate diff that unfreezes `SGLANG_GATEWAY_VERSION` or touches `pack/llm-router/patches/sglang-gateway/`, and verifies `base_image_builds` output whenever `pack/*/Dockerfile` changes.

#### 3. KV Events Publishing and Router Subscription Matrix

- **`llm-d-router` (`vLLM`, `vLLM-Ascend`):**
  - Fix the publisher topic from `"kv@"` to `"kv@<spec.model.name>"` so `llm-d-router`'s `topicIdentity` extracts `modelName` instead of empty string (verified in `llm-d/llm-d-router` `pkg/kvevents/engineadapter/common.go` and `pkg/kvevents/pool.go`). Keep subscriber `topicFilter: "kv@"`.
  - Remove the Ascend exclusion in [model_deployment_connector.go](../pkg/worker/controllers/worker/model_deployment_connector.go) and [inject.go](../pkg/worker/kvcache/inject/inject.go) so managed `vLLM-Ascend` `Server` and `Prefill` roles publish KV events identically to `vLLM`.
  - Keep `SGLang + llm-d-router` on `approx-prefix-cache-producer` (no KV events) until upstream `llm-d-router` removes the `BackendVLLM` restriction from `precise-prefix-cache-producer`. Fix `delete(kinds, roleKindEncode)` ordering in [router.go](../pkg/worker/kvcache/router/router.go) so SGLang EPD does not misclassify `encode`.
- **`sgl-router` (`SGLang`):**
  - Enable `--kv-events-config` (`{"publisher":"zmq","endpoint":"tcp://*:5557","replay_endpoint":"tcp://*:5558","topic":"kv-events"}`) and ports `5557`/`5558` on managed `SGLang` `Server` and `Prefill` roles behind `sgl-router` (including store-less single-role `Server` deployments without `spec.kvCache` or `KVTransfer`), emitting `--kv-events-config` before `--encoder-urls` (`nargs="+"`).
- **`vllm-router` (`vLLM`, `vLLM-Ascend` — conditional on `kv_aware`):**
  - Keep `vllm-router` default policy (`cache_aware`) unchanged without KV event overhead.
  - Parse `kv_aware` activation via a single shared helper in `pkg/worker/kvcache/router` covering `--policy`, `--prefill-policy`, and `--decode-policy` (`=value` and two-token forms, last-wins precedence).
  - When `kv_aware` applies to a role (`Server`/`Prefill` via `--policy` or `--prefill-policy`; `Decode` via `--decode-policy` or PD `--policy` when `--decode-policy` is unset):
    - Reject at admission if the role's effective `vLLM` version is `< 0.31.0` (because `GET /kv_event_sources` dynamic discovery requires `vLLM >= 0.31.0`, per `vllm-project/router` `docs/load_balancing/kv_aware.md`).
    - Synthesize `--kv-events-config` (`enable_kv_cache_events: true`, `publisher: "zmq"`, `endpoint: "tcp://*:5557"`, `replay_endpoint: "tcp://*:5558"`, topic `kv@<spec.model.name>`), ports `5557`/`5558` (on Pod, per-role Service, and webhook port reservation — including `Decode` when `--decode-policy kv_aware` is active), `--prefix-caching-hash-algo sha256_cbor` (conditionally Owned when `kv_aware` is active), and `VLLM_KV_EVENTS_USE_INT_BLOCK_HASHES=0` (conditionally Owned when `kv_aware` is active, stripped from user overrides in `mergeModelDeploymentUserEnv`, and registered in `modelDeploymentScalerCapacityNeutralEnvNames`).
    - Include the resolved `kv_aware` role activation boolean in the worker connector `bindingHash` so toggling `kv_aware` rolls affected workers while unrelated `router.extraArgs` edits keep router hash decoupling intact.
- **Status condition and role contract ([model_deployment_kv_events.go](../pkg/worker/controllers/worker/model_deployment_kv_events.go), [model_deployment_router.go](../pkg/worker/controllers/worker/model_deployment_router.go)):**
  - Populate `status.router.roles[*].kvEvents.topic` with the actual publisher topic (`kv@<spec.model.name>` or `kv-events`).
  - Update `KVEventsPublishing` condition logic across `Server`, `Prefill`, and `Decode` roles so `True/Publishing`, `True/NotApplicable` (including `vllm-router` without `kv_aware`, `SGLang + llm-d-router`, and frozen `sglang-gateway`), `Unknown/RoleUnmanaged`, and `False` reflect the expanded matrix accurately.
- **Acceptance criteria:**
  - Golden render tests verify `--kv-events-config` (with `kv@<model>`) and `precise-prefix-cache-producer` (`topicFilter: "kv@"`) for `llm-d-router` + `vLLM` and `vLLM-Ascend`, and `approx-prefix-cache-producer` for `llm-d-router` + `SGLang`.
  - Golden render tests verify `sgl-router` + `SGLang` emits `--kv-events-config` before `--encoder-urls` on `Server`/`Prefill` and omits it on `Decode`/`Encode`.
  - Golden render tests verify `vllm-router` with default `cache_aware` emits no KV event flags, while `--prefill-policy kv_aware` emits `--kv-events-config`, `--prefix-caching-hash-algo sha256_cbor`, and `VLLM_KV_EVENTS_USE_INT_BLOCK_HASHES=0` on `Prefill` only, and `--decode-policy kv_aware` enables them on `Decode` too.
  - Admission rejects `vllm-router` + `kv_aware` when effective `vLLM` version is `< 0.31.0`.

#### 4. Retired/Changed Parameters, Ascend `MooncakeConnectorV2`, and Non-Disruptive Upgrade

- **Version-gated `vLLM` EC parameters ([vllm.go](../pkg/worker/kvcache/inject/vllm.go), [vllm_ec.go](../pkg/worker/kvcache/inject/vllm_ec.go)):**
  - Wire `result.DefaultedEnv = append(result.DefaultedEnv, leg.DefaultedEnv...)` in `vllm.go` so defaulted encoder env vars actually reach the rendered Pod container.
  - `VLLM_USE_V2_MODEL_RUNNER`: Removed upstream in `vllm` PR `#63373` (`v0.34.0`) and `vllm-ascend` PR `#22307`. Remove from `EncoderCacheOwnedKeys`, add to `modelDeploymentDefaultedEnvNames`, emit `VLLM_USE_V2_MODEL_RUNNER=1` as Defaulted only when effective `vLLM` version `< 0.34.0` (or legacy empty version), and omit it when `>= 0.34.0`.
  - `VLLM_EC_SIDE_CHANNEL_PORT`: Default changed from `5601` to `0` in `vllm` PR `#61764` (`v0.34.0`). Remove from `EncoderCacheOwnedKeys`, add to `modelDeploymentDefaultedEnvNames`, emit `VLLM_EC_SIDE_CHANNEL_PORT=5601` as Defaulted (matching `VLLM_NIXL_SIDE_CHANNEL_PORT=8998`), and update `validateModelDeploymentEPDSideChannelPort` so an explicit user override (such as `0` or `5602`) is respected when checking HTTP port conflicts.
- **Compile-cache dead env cleanup ([model_deployment_compile_cache.go](../pkg/worker/controllers/worker/model_deployment_compile_cache.go)):**
  - Remove upstream-deleted `VLLM_USE_V1` (`#28204`), `VLLM_ATTENTION_BACKEND` (`#32812`), and `VLLM_FLASH_ATTN_VERSION` (`#32812`) from the tracked compile-cache environment key list.
- **Version-gated Ascend `MooncakeConnectorV2` ([vllm.go](../pkg/worker/kvcache/inject/vllm.go)):**
  - For Ascend P/D (`EngineVLLMAscend`) where both `Prefill` and `Decode` have effective engine version `>= 0.30.0` (including `0.30.0rc1` / `0.30.0.rc1`):
    - Render `kv_connector: "MooncakeConnectorV2"` (Pull mode, `use_layerwise` omitted/false), keeping `kv_port: 8998`, `prefill`/`decode` `tp_size`/`dp_size`, `/usr/local/Ascend/driver`, Ascend A5 `ascend_local_comm_res_path` / `ASCEND_LOCAL_COMM_RES`, and `llm-d-router` `--kv-connector=nixlv2` / `vllm-router` `--kv-connector=nixl`.
    - Relax the TP shape validation from `prefill_tp >= decode_tp` to `max(prefill_tp, decode_tp) % min(prefill_tp, decode_tp) == 0` (matching upstream `vllm-ascend` `pull_worker.py`).
  - When effective engine version `< 0.30.0` (or empty legacy CR): keep `MooncakeConnectorV1` and `prefill_tp >= decode_tp`.
  - Reject mixed V1/V2 Ascend P/D version pairs at admission (one half `< 0.30.0` and the other `>= 0.30.0`) so `Prefill` and `Decode` never render mismatched connector generations.
- **Render version and legacy hash currency protection ([model_deployment_render.go](../pkg/worker/controllers/worker/model_deployment_render.go)):**
  - Bump `modelDeploymentRenderVersion` from `6` to `7`.
  - Extend `modelDeploymentPodMatchesCurrencyWithLegacy` with pre-upgrade `bindingHash` compatibility (pre-`renderVersion=7` `vLLM-Ascend + llm-d-router` Pods hashed with `PublishKVEvents: false`) so standing healthy Pods (including `pair-ascend-llm-d` in `TestModelDeploymentLegacyRender_GoldenPods`) do not roll on operator upgrade while new or edited Pods adopt `renderVersion=7`.
- **Acceptance criteria:**
  - `ECCPUConnector` on vLLM `0.32.0` receives defaulted `VLLM_USE_V2_MODEL_RUNNER=1` on the rendered container; on vLLM `0.34.0` it receives neither `VLLM_USE_V2_MODEL_RUNNER` nor an admission refusal if a user sets it.
  - Ascend P/D on `0.30.0` / `0.30.0rc1` / `0.32.0` renders `MooncakeConnectorV2` and allows `prefill tp=2, decode tp=4` (rejecting `tp=3, tp=4`); on `0.23.0` it renders `MooncakeConnectorV1` and rejects `prefill tp=2, decode tp=4`; mixed `0.32.0` + `0.23.0` Ascend P/D is rejected at admission.
  - Standing `renderVersion=6` Pods (including `pair-ascend-llm-d`) stay current without rolling on operator upgrade.

#### 5. Orthogonal, Version-Indexed LLM Parameter Reference Overhaul

- Rewrite [docs/reference/model-deployment-parameters.md](../docs/reference/model-deployment-parameters.md) and fix doc drift in [docs/reference/model-deployment-internals.md](../docs/reference/model-deployment-internals.md), [docs/reference/kv-cache-injection.md](../docs/reference/kv-cache-injection.md), [docs/modules/model-deployment/engine-versions.md](../docs/modules/model-deployment/engine-versions.md), [docs/modules/model-deployment/prefill-decode.md](../docs/modules/model-deployment/prefill-decode.md), [docs/modules/model-deployment/routing.md](../docs/modules/model-deployment/routing.md), and [docs/README.md](../docs/README.md).
- Structure [docs/reference/model-deployment-parameters.md](../docs/reference/model-deployment-parameters.md) for fast, unambiguous LLM lookup:
  1. **Orthogonal axes:** Keep `KVCache` (shared store), `KVTransfer` (direct P/D), `KVEvents` (event publishing/subscription), `ECTransfer` (EPD), `Router`, `Serving`, and `Weight Delivery` in distinct sections with explicit composition rules (for example `MultiConnector` when `KVCache` and `KVTransfer` are both active on `vLLM` `Prefill`).
  2. **Version-boundary tables ("Before vs At/After"):** Every version-dependent parameter table includes an explicit `Engine Version` column (`All`, `< 0.30.0`, `>= 0.30.0`, `< 0.31.0`, `>= 0.31.0`, `< 0.34.0`, `>= 0.34.0`, etc.) so an LLM can answer "what is rendered before version X vs at/after version X" in one lookup.
  3. **Remove all doc drift:** Delete non-existent `SGLANG_HICACHE_ENABLE_GARBAGE_COLLECTION`, remove un-injected `SGLANG_ENABLE_OVERLAP_PLAN_STREAM` from the injected table, update `--swap-space` in `kv-cache-injection.md` from "deprecated and ignored" to "removed in vLLM `0.30+`", update the `MOONCAKE_STANDALONE_STORAGE` comment in [sglang.go](../pkg/worker/kvcache/inject/sglang.go), document frozen `sglang-gateway` vs `sgl-router` (`--no-tokenizer` vs `--policy cache_aware` tokenizer requirement, EndpointSlice RBAC), and document the `llm-d-router@v1.0.0` SGLang `approx-prefix-cache-producer` limitation.
- **Acceptance criteria:**
  - Every injected flag, env var, port, annotation, and mount in [docs/reference/model-deployment-parameters.md](../docs/reference/model-deployment-parameters.md) matches Go code 1:1.
  - `make lint docs` passes with zero errors or warnings.

### Notes / Constraints / Caveats

- **Engine version comparison:** Milestone version gates compare the `(major, minor, patch)` tuple, so `0.30.0rc1` / `0.30.0.rc1` satisfy `>= 0.30.0` (`MooncakeConnectorV2`) and `0.34.0rc0` satisfies `>= 0.34.0` (`VLLM_USE_V2_MODEL_RUNNER` omitted).
- **vLLM-Ascend version alignment:** In `gpustack/runner` (for example `gpustack/runner:cann9.2-a3-vllm0.32.0`), the tag's `service_version` (`0.32.0`) tracks the vLLM release line while the bundled `vllm-ascend` package shares the same minor line (`0.32.0rc1`).
- **`vllm-router` `kv_aware` with shared `KVCachePool`:** Enabling `kv_aware` sets `--prefix-caching-hash-algo sha256_cbor`. Workloads sharing one `KVCachePool` domain must use matching prefix hash algorithms if cross-deployment prefix hits are expected.
- **SGLang `--enable-dp-attention` vs `--attn-dp-size`:** [model_deployment_parallelism.go](../pkg/worker/controllers/worker/model_deployment_parallelism.go) already parses both `--attn-dp-size` / `--attention-data-parallel-size` and legacy `--enable-dp-attention`. Document `--attn-dp-size` as preferred in SGLang examples and reference docs.
- **API and upgrade compatibility:** Public `worker.gpustack.ai/v1` and internal `v1alpha1` remain strictly additive (new optional fields with unused protobuf tags `3` and `24`, new router enum value `sgl-router`, frozen `sglang-gateway`). Run `make generate` and `bash hack/check/api-descriptions.sh` after any API edit.
- **Worker RBAC for `EndpointSlice`:** The default Helm chart binds the worker `ServiceAccount` to `cluster-admin` ([serviceaccount.yaml](../deploy/gpustack-operator/chart/templates/worker/serviceaccount.yaml), whose comment already lists `discovery.k8s.io/endpointslices` for narrowed bindings). Document that custom least-privilege operator bindings must include `discovery.k8s.io/endpointslices` (`get, list, watch`) to create `sgl-router` Roles.
- **Compile-cache env cleanup impact:** Removing `VLLM_USE_V1`, `VLLM_ATTENTION_BACKEND`, and `VLLM_FLASH_ATTN_VERSION` from [model_deployment_compile_cache.go](../pkg/worker/controllers/worker/model_deployment_compile_cache.go) is intentional for `vLLM >= 0.30`; document that legacy `< 0.30` pods restarting after upgrade incur one cold compile-cache miss.

### Boundaries

- **Always:**
  - Keep `KVCache` (shared store), `KVTransfer` (direct P/D), `KVEvents`, and `ECTransfer` orthogonal in code and documentation.
  - Enforce the strict engine-router matrix (`sgl-router`/`sglang-gateway` → `SGLang`; `vllm-router` → `vLLM`/`vLLM-Ascend`; `llm-d-router` → all three).
  - Run `make lint`, then `make generate`, `make lint docs`, `bash hack/check/api-descriptions.sh`, and `bash hack/check/autosync-selftest.sh`.
- **Ask first:**
  - Before removing `sglang-gateway` from the CRD enum or `pack/llm-router/Dockerfile`.
  - Before changing `llm-d-router` sidecar image source to an unreleased commit (such as `#3529` for `ECMooncakeConnector`).
  - Before making any edit that forces an automatic rollout of standing Pods without `renderVersion` / legacy-hash currency protection.
- **Never:**
  - Never unfreeze `SGLANG_GATEWAY_VERSION` or modify `pack/llm-router/patches/sglang-gateway/` in `.auto-sync/`.
  - Never guess the engine type (`vLLM` vs `SGLang`) from a container image string; only extract the version when `<service>` matches declared `spec.engine.name`.
  - Never overwrite a user-supplied `spec.engine.version` or `spec.roles[*].engineVersion` in the Defaulter webhook.
  - Never hand-edit generated files (`zz_generated.*`, `generated.pb.go`, `generated.proto`).

### Risks and Mitigations

- **Stale user-pinned `version` after updating `image`** → Updating `spec.engine.image` to a newer `gpustack/runner` tag while leaving an old explicit `spec.engine.version` would apply stale version-gated rendering. Mitigation: reject UPDATE mismatches when a parseable runner tag disagrees with a non-empty version; clear `version` alongside `image` to trigger automatic Defaulter backfill.
- **Unintended standing-Pod rollout on upgrade or legacy CR scale-up** → Enabling KV events on `vLLM-Ascend + llm-d-router` changes `bindingHash`, and backfilling `engineVersion` on legacy UPDATE mutates `pod-spec-hash`. Mitigation: bump `renderVersion` to `7` with pre-upgrade `bindingHash` currency matching in `modelDeploymentPodMatchesCurrencyWithLegacy`, restrict UPDATE defaulting to changed images via `admission.RequestFromContext`, and treat empty stored versions consistently as legacy baseline.
- **`sgl-router` offline tokenizer startup failure** → Without `--no-tokenizer`, `sgl-router` downloads tokenizers from Hugging Face Hub using `--model-id`, crashing in air-gapped clusters or with custom aliases. Mitigation: emit `--no-tokenizer` by default and omit it only when `spec.router.extraArgs` sets `--tokenizer-path` or enables `cache_aware`/prefix-cache policies.
- **`sgl-router` observer / Envoy allowlist parity** → `sgl-router` native `/workers` fails `parseRouterObservationView`'s `DisallowUnknownFields()` and Envoy's `/observer/endpoints` probe, stranding Pods Unready. Mitigation: ship `pack/llm-router/patches/sgl-router/0001-observer-endpoints.patch` exposing the exact observer wire schema and verify it in `.auto-sync/checks/validate_router.sh`.
- **Single-repo stage collision in `.auto-sync/`** → `sglang-gateway` (`gateway-v0.3.1`, `sgl-model-gateway`) and `sgl-router` (`router-v*`, `sgl-router rust`) share upstream `sgl-project/sglang`. Mitigation: stage them in separate directories under `$STAGE_DIR` with independent sparse-checkout paths.
- **Mixed V1/V2 connector skew in Ascend P/D** → Per-role `engineVersion` allows pairing a `< 0.30.0` role with a `>= 0.30.0` role. Mitigation: reject mixed V1/V2 Ascend P/D pairs at admission.
- **Shared `KVCachePool` hash mismatch under `vllm-router` `kv_aware`** → Switching `--prefix-caching-hash-algo` to `sha256_cbor` changes block hashes relative to default `sha256`. Mitigation: activate `sha256_cbor` strictly when `kv_aware` is configured, and document the constraint in the parameter reference.

## Design Details

### Commands

**Environment:**
- **Local host (macOS):** Runs all Go builds, unit/integration tests, code generation, linters, docs checks, API description checks, and `.auto-sync/` self-tests (verified with `go test -cover`, 0.5–1.5 s per package).
- **Remote / CI (Linux Buildx):** Runs `pack/llm-router/Dockerfile` multi-arch container image builds (`Base Image` workflow / `make package llm-router`). Local `T5` verification uses `git apply --check` against upstream `router-v0.1.0` and `autosync-selftest.sh`.

```bash
# Regenerate API types, CRDs, protobuf, and webhooks (re-run after make lint if lint rewrites code)
make generate

# Lint Go and shell sources
make lint

# Verify generated CRD/OpenAPI descriptions
bash hack/check/api-descriptions.sh

# Lint documentation, specs, module map, and Hugo site
make lint docs

# Run full unit test suite (local)
RACE=false make test

# Run focused ModelDeployment / KV cache / router / metrics tests (local)
go test -v ./pkg/worker/kvcache/... ./pkg/worker/webhooks/worker/... ./pkg/worker/controllers/worker/... ./pkg/worker/extensionapis/worker/...

# Verify auto-sync scripts, sglang-gateway freeze guard, and Base Image build recommendations
bash hack/check/autosync-selftest.sh

# Build multi-router image (Remote / CI Linux Buildx only)
PACKAGE_ARCH=amd64 PACKAGE_TAG=dev make package llm-router
```

### Project Structure

- [api/worker/v1alpha1/model_deployment.go](../api/worker/v1alpha1/model_deployment.go) & [api/worker/v1/model_deployment.go](../api/worker/v1/model_deployment.go) — CRD storage and public `v1` API types (`ModelDeploymentEngine.Version`, `ModelDeploymentRole.EngineVersion`, `ModelDeploymentRouterSGLangRouter`).
- [pkg/worker/kvcache/inject/](../pkg/worker/kvcache/inject/) — New `version.go` (runner tag parser and `(major, minor, patch)` comparator), plus `inject.go`, `types.go`, `vllm.go`, `vllm_ec.go`, `sglang.go` for version-gated connectors, `DefaultedEnv` plumbing, EC env demotion, and KV event publisher configs.
- [pkg/worker/kvcache/router/](../pkg/worker/kvcache/router/) — Router rendering (`argv.go` for `vllm-router`, frozen `sglang-gateway`, new `sgl-router`, and shared `vllm-router` `kv_aware` parser; `router.go` for `llm-d-router`).
- [pkg/worker/webhooks/worker/model_deployment.go](../pkg/worker/webhooks/worker/model_deployment.go) & [model_deployment_epd.go](../pkg/worker/webhooks/worker/model_deployment_epd.go) — Defaulter backfill (CREATE and image-changed UPDATE), fail-closed version & mismatch validation, `DeletionTimestamp` guard, strict engine-router matrix, `vllm-router` `kv_aware` `< 0.31.0` floor, Ascend V1/V2 pair guard, side-channel port override compatibility, and `5557`/`5558` port reservations.
- [pkg/worker/controllers/worker/](../pkg/worker/controllers/worker/) — Connector orchestration (`model_deployment_connector.go`, `model_deployment_connector_epd.go`), router workload/RBAC/observation (`model_deployment_router.go`, `model_deployment_router_observation.go`), role Service `role-kind` labels (`model_deployment_service.go`), KV events status (`model_deployment_kv_events.go`), scaler env gate (`model_deployment_scaler_prior_identity.go`), compile cache keys (`model_deployment_compile_cache.go`), and `renderVersion=7` + legacy `bindingHash` currency (`model_deployment_render.go`).
- [pkg/worker/extensionapis/worker/model_deployment.metrics.go](../pkg/worker/extensionapis/worker/model_deployment.metrics.go) & [model_deployment.metrics_parse.go](../pkg/worker/extensionapis/worker/model_deployment.metrics_parse.go) — `sgl-router` router metrics scraping and Prometheus parsing.
- [pack/llm-router/Dockerfile](../pack/llm-router/Dockerfile) & `pack/llm-router/patches/sgl-router/` — Multi-stage image carrying `epp`, `vllm-router`, frozen `sgl-model-gateway`, and `sgl-router` with `0001-observer-endpoints.patch`.
- [.auto-sync/](../.auto-sync/) & [hack/check/autosync-selftest.sh](../hack/check/autosync-selftest.sh) — Router auto-sync (`sglang-gateway` freeze + `sgl-router` tracking) and shared `Base Image` build recommendation helper (`base_image_builds`) for modified `pack/*/Dockerfile` across `router` and `kv-transfer` domains.
- [docs/reference/model-deployment-parameters.md](../docs/reference/model-deployment-parameters.md) — Orthogonal, version-indexed LLM parameter contract (plus `model-deployment-internals.md`, `kv-cache-injection.md`, `engine-versions.md`, `prefill-decode.md`, `routing.md`, and `docs/README.md`).

### Code Style

Follow Go and Kubernetes conventions in `AGENTS.md`: `snake_case` filenames, explicit typed refusals (`RefusalError`), plain short comments without decorative symbols, and table-driven tests with fake clients.

Real pattern from [pkg/worker/kvcache/inject/vllm.go](../pkg/worker/kvcache/inject/vllm.go):

```go
if prefill, decode := in.Parallelism.Prefill.orOne(), in.Parallelism.Decode.orOne(); prefill.TensorParallel < decode.TensorParallel {
	return nil, newRefusal(ReasonRoleUnsupported,
		"the vLLM-Ascend transfer leg requires the prefill tp_size to be at "+
			"least the decode one, but the pair declares prefill %d and decode %d; "+
			"narrow the decode role's tensor parallelism or widen the prefill role's",
		prefill.TensorParallel, decode.TensorParallel)
}
```

### Implementation Plan

- [x] **T1 · Engine version API, runner tag parser, and version comparator**
      Blocked by: None
      Owns: `api/worker/v1alpha1/model_deployment.go`, `api/worker/v1alpha1/zz_generated.*`, `api/worker/v1alpha1/generated.*`, `api/worker/v1/model_deployment.go`, `api/worker/v1/zz_generated.*`, `api/worker/v1/generated.*`, `deploy/gpustack-operator/chart/crds/**`, `pkg/worker/kvcache/inject/version.go`, `pkg/worker/kvcache/inject/version_test.go`
      Gate: review
      Acceptance: `ModelDeploymentEngine.Version` (tag `3`), `ModelDeploymentRole.EngineVersion` (tag `24`), `ModelDeploymentRouterSGL = "sgl-router"`, `ModelDeploymentRouterSGLangGateway = "sglang-gateway"` (with deprecated alias `ModelDeploymentRouterSGLang`), and deprecated `sglang-gateway` docstrings exist in `v1alpha1` (exposed through `v1`); `make generate` and `api-descriptions.sh` are clean; `ParseRunnerEngineVersion`, `ValidateEngineVersion`, and `(major, minor, patch)` `CompareEngineVersion` handle registry ports, digests, variants, service matching, and `rc` pre-releases (`0.30.0rc1`, `0.30.0.rc1`, `0.34.0rc0`).
      Verify: `make generate && bash hack/check/api-descriptions.sh && go test -v ./pkg/worker/kvcache/inject/...`

- [x] **T4 · Router config rendering (`sgl-router`, `vllm-router` `kv_aware` parser, `llm-d-router`)**
      Blocked by: None
      Owns: `pkg/worker/kvcache/router/**`
      Acceptance: `router.SGL = "sgl-router"` (alongside `router.SGLangGateway = "sglang-gateway"`) renders `--host`, `--port`, `--model-id`, `--chat-routing=reorg`, default `--no-tokenizer` (omitted when `extraArgs` sets `--tokenizer-path`, `--bucket-config`, or enables `cache_aware`/prefix-cache policies), `--service-discovery`, `--service-discovery-namespace`, EndpointSlice `--selector` / `--prefill-selector` + `--decode-selector`, `--dp-aware` for External DP, and `--request-timeout-secs`; exported `SGLOwnedFlags` and `vllm-router` `kv_aware` helper parse `--policy`, `--prefill-policy`, and `--decode-policy` (`=value`, two-token, last-wins); `router.go` preserves `publisherKinds` before `delete(kinds, roleKindEncode)` for SGLang EPD while preserving `llm-d-router` `topicFilter: "kv@"` and frozen `sglang-gateway` output byte-for-byte.
      Verify: `go test -v ./pkg/worker/kvcache/router/...`

- [x] **T5 · `pack/llm-router` `sgl-router` stage/observer patch, `.auto-sync/` freeze, and `Base Image` build recommendations**
      Blocked by: None
      Owns: `pack/llm-router/**`, `.auto-sync/**`, `hack/check/autosync-selftest.sh`
      Gate: review
      Acceptance: `pack/llm-router/Dockerfile` pins frozen `SGLANG_GATEWAY_VERSION=gateway-v0.3.1`, adds `ARG SGL_ROUTER_VERSION=router-v0.1.0` and `sgl-router-builder` (`git sparse-checkout set sgl-router rust`, `cargo build --release --locked --bin sgl-router`), and applies `pack/llm-router/patches/sgl-router/0001-observer-endpoints.patch` cleanly against upstream `router-v0.1.0` exposing `GET /observer/endpoints` in the exact `parseRouterObservationView` JSON schema; `.auto-sync/` stages `sglang-gateway` and `sgl-router` in separate directories, tracks `router-v*` for `sgl-router`, refuses any candidate modifying `SGLANG_GATEWAY_VERSION` or `pack/llm-router/patches/sglang-gateway/`, and includes a shared `Base Image` build-recommendation check (`base_image_builds`) that lists affected `pack/*` images, targets, recommended tags, and `gh workflow run "Base Image"` commands whenever `pack/*/Dockerfile` (or sibling `patches/`) changes in `router` or `kv-transfer` domains.
      Verify: `bash hack/check/autosync-selftest.sh && make lint`

- [ ] **T2 · Admission webhook defaulting, version & matrix validation, and port reservations**
      Blocked by: T1, T4
      Owns: `pkg/worker/webhooks/worker/model_deployment*.go`
      Gate: review
      Acceptance: Defaulter backfills `spec.engine.version` and `roles[*].engineVersion` on CREATE and on UPDATE only when the corresponding `image` changed and version is empty (never mutating legacy empty-version CRs on scale/pause); validation short-circuits on `DeletionTimestamp != nil`, refuses unparseable managed images without explicit version, refuses UPDATE mismatch when a parseable runner tag disagrees with explicit version, enforces the strict engine-router matrix (`sgl-router`/`sglang-gateway` → `SGLang`, `vllm-router` → `vLLM`, `llm-d-router` → both), enforces `sgl-router` owned args, refuses `vllm-router` `kv_aware` below `0.31.0` and conditionally owns `--prefix-caching-hash-algo` / `VLLM_KV_EVENTS_USE_INT_BLOCK_HASHES`, refuses mixed V1/V2 Ascend P/D version pairs, respects explicit `VLLM_EC_SIDE_CHANNEL_PORT` overrides in `validateModelDeploymentEPDSideChannelPort`, and reserves ports `5557`/`5558` for all KV-event-publishing roles (including `vllm-router` `Decode` under `--decode-policy kv_aware` and `sgl-router` `SGLang` `Server`/`Prefill`).
      Verify: `go test -v ./pkg/worker/webhooks/worker/... -run 'TestModelDeployment'`

- [x] **T3 · Engine parameter injection: version-gated connectors, EC env demotion, and KV events**
      Blocked by: T1
      Owns: `pkg/worker/kvcache/inject/inject.go`, `pkg/worker/kvcache/inject/inject_test.go`, `pkg/worker/kvcache/inject/types.go`, `pkg/worker/kvcache/inject/vllm.go`, `pkg/worker/kvcache/inject/vllm_ascend_test.go`, `pkg/worker/kvcache/inject/vllm_ec.go`, `pkg/worker/kvcache/inject/vllm_ec_test.go`, `pkg/worker/kvcache/inject/sglang.go`, `pkg/worker/kvcache/inject/sglang_test.go`
      Gate: review
      Acceptance: `vllm.go` appends `leg.DefaultedEnv` to `result.DefaultedEnv`; `VLLM_USE_V2_MODEL_RUNNER` and `VLLM_EC_SIDE_CHANNEL_PORT` are removed from `EncoderCacheOwnedKeys` and emitted as Defaulted (`VLLM_USE_V2_MODEL_RUNNER=1` only for `< 0.34.0` / legacy, omitted at `>= 0.34.0`; `VLLM_EC_SIDE_CHANNEL_PORT=5601`); Ascend P/D renders `MooncakeConnectorV2` (Pull mode) with divisibility check `max(tp_p, tp_d) % min(tp_p, tp_d) == 0` at `>= 0.30.0` and keeps `MooncakeConnectorV1` + `tp_p >= tp_d` below `0.30.0` / legacy; `llm-d-router` KV event topic is `"kv@<model>"`; `inject.Render` allows `EngineVLLMAscend` and store-less `EngineSGLang` KV events (placing SGLang `--kv-events-config` before `--encoder-urls`); `vllm-router` `kv_aware` injects `--prefix-caching-hash-algo sha256_cbor` and `VLLM_KV_EVENTS_USE_INT_BLOCK_HASHES=0`; `MOONCAKE_STANDALONE_STORAGE` comment in `sglang.go` is updated.
      Verify: `go test -v ./pkg/worker/kvcache/inject/...`

- [ ] **T6 · Controller orchestration, `renderVersion=7` currency protection, and router/metrics wiring**
      Blocked by: T2, T3, T4
      Owns: `pkg/worker/controllers/worker/model_deployment*.go`, `pkg/worker/controllers/worker/testdata/**`, `pkg/worker/extensionapis/worker/model_deployment.metrics*.go`
      Gate: review
      Acceptance: Controller resolves effective role versions (treating stored empty version as legacy baseline and hashing resolved `EngineVersion` in `modelDeploymentSpecHashWithRouter`); enables KV events on `vLLM-Ascend + llm-d-router`, `vllm-router` `kv_aware` (including `Decode`, conditional owned env stripping in `mergeModelDeploymentUserEnv`, `bindingHash` propagation, and `VLLM_KV_EVENTS_USE_INT_BLOCK_HASHES` in `modelDeploymentScalerCapacityNeutralEnvNames`), and `sgl-router` (`SGLang` `Server`/`Prefill`); adds `VLLM_USE_V2_MODEL_RUNNER` and `VLLM_EC_SIDE_CHANNEL_PORT` to `modelDeploymentDefaultedEnvNames`; wires `sgl-router` loopback Envoy sidecar + `:9091` observer probe + metrics port handling, `discovery.k8s.io/endpointslices` Role RBAC, role-Service `role-kind` labels with eligibility retention, `modelDeploymentEPDMatrix` SGLang + `sgl-router` rows, and `ModelDeploymentMetricsHandler` `sgl-router` scraping/parsing; removes dead compile-cache keys (`VLLM_USE_V1`, `VLLM_ATTENTION_BACKEND`, `VLLM_FLASH_ATTN_VERSION`); updates `KVEventsPublishing` condition and `status.router.roles[*].kvEvents.topic`; bumps `modelDeploymentRenderVersion` `6 -> 7` with pre-upgrade `bindingHash` currency match so standing `pair-ascend-llm-d` Pods do not roll on upgrade.
      Verify: `go test -v ./pkg/worker/controllers/worker/... ./pkg/worker/extensionapis/worker/... -run 'TestModelDeployment|TestRenderModelDeployment|TestSynthesize'`

- [ ] **T7 · Orthogonal, version-indexed parameter reference and docs overhaul**
      Blocked by: T3, T4, T6
      Owns: `docs/reference/model-deployment-parameters.md`, `docs/reference/model-deployment-internals.md`, `docs/reference/kv-cache-injection.md`, `docs/modules/model-deployment/engine-versions.md`, `docs/modules/model-deployment/prefill-decode.md`, `docs/modules/model-deployment/routing.md`, `docs/README.md`
      Acceptance: `docs/reference/model-deployment-parameters.md` is reorganized around orthogonal axes (`KVCache`, `KVTransfer`, `KVEvents`, `ECTransfer`, `Router`, `Serving`, `Weight Delivery`) with explicit `Engine Version` ("Before vs At/After") tables matching Go code 1:1; all doc drift (`SGLANG_HICACHE_ENABLE_GARBAGE_COLLECTION`, un-injected `SGLANG_ENABLE_OVERLAP_PLAN_STREAM`, `--swap-space` status) is fixed; `sgl-router`, frozen `sglang-gateway`, `vllm-router` `kv_aware` (`>= 0.31.0` & shared `KVCachePool` `sha256_cbor` caveat), `llm-d-router@v1.0.0` SGLang `approx-prefix-cache-producer` limitation, and Ascend `MooncakeConnectorV2` (`>= 0.30.0`) are documented accurately; `docs/README.md` stays in sync.
      Verify: `make lint docs`

- [ ] **T8 · Final verification checkpoint and follow-up TODO issues**
      Blocked by: T5, T6, T7
      Owns: None
      Acceptance: Full repository lint, codegen, unit tests, docs lint, API description check, and auto-sync self-test pass cleanly with zero uncommitted generated drift; two follow-up tracking GitHub TODO issues are filed (per `gpustack-operator-issue-pr` conventions): (1) enable `SGLang + llm-d-router` KV events once upstream `llm-d-router` adds a non-vLLM `token-producer` backend for `precise-prefix-cache-producer`, and (2) evaluate Ascend `MooncakeConnectorV2` layerwise Push mode and `ECMooncakeConnector` once a tagged `llm-d-router` release includes PR `#3529` (`--ec-connector=ec-mooncake`).
      Verify: `make lint && make generate && RACE=false make test && make lint docs && bash hack/check/api-descriptions.sh && bash hack/check/autosync-selftest.sh`

### Test Plan

[x] I/we understand the owners of the involved components may require updates to existing tests to make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates
- Preserve `pkg/worker/controllers/worker/testdata/legacy-render/` golden fixtures unchanged so `TestModelDeploymentLegacyRender_GoldenPods` enforces zero standing-Pod rollout for `pair-ascend-llm-d` across the `renderVersion` `6 -> 7` upgrade.
- Extend `.auto-sync` test fixtures in `hack/check/autosync-selftest.sh` with a fixture `sgl-router` tag (`router-v0.1.0`, `sgl-router` + `rust/` subpaths) and `Base Image` recommendation assertions alongside the frozen `sglang-gateway` (`gateway-v0.3.1`) fixture.

#### Unit tests
- `pkg/worker/kvcache/inject`: `2026-11-25` - `>= 85%` (baseline `84.1%`) — runner tag parser (registry ports, digests, digest-only refusal, `Host`/Ascend/ROCm variants, engine mismatch), `(major, minor, patch)` comparator (`0.30.0rc1`, `0.30.0.rc1`, `0.34.0rc0`), `DefaultedEnv` propagation from `renderVLLMEncoderCache`, `VLLM_USE_V2_MODEL_RUNNER` (`< 0.34.0` vs `>= 0.34.0`) and `VLLM_EC_SIDE_CHANNEL_PORT` demotion, Ascend `MooncakeConnectorV1` vs `MooncakeConnectorV2` + TP divisibility, `kv@<model>` topic, `EngineVLLMAscend` and store-less `EngineSGLang` KV events (ordering before `--encoder-urls`), and `vllm-router` `kv_aware` `sha256_cbor` + `VLLM_KV_EVENTS_USE_INT_BLOCK_HASHES=0`.
- `pkg/worker/kvcache/router`: `2026-11-25` - `>= 91%` (baseline `91.0%`) — `sgl-router` rendering for Server, PD, EPD, External DP (`--dp-aware`), default `--no-tokenizer` vs `--tokenizer-path` / `cache_aware` omission, `vllm-router` `kv_aware` flag parsing (`=value`, two-token, last-wins, PD `--policy` inheritance), `llm-d-router` `topicFilter: "kv@"` and SGLang EPD fix, and unchanged frozen `sglang-gateway` output.
- `pkg/worker/webhooks/worker`: `2026-11-25` - `>= 45%` (focused ModelDeployment baseline `38.8%`) — CREATE backfill, UPDATE backfill on image change with cleared version, legacy empty-version CR scale/pause without backfill, `DeletionTimestamp != nil` short-circuit, UPDATE mismatch refusal, strict engine-router matrix, `sgl-router` owned args, `vllm-router` `kv_aware` `< 0.31.0` floor and conditional owned keys, Ascend P/D mixed V1/V2 pair refusal, `VLLM_EC_SIDE_CHANNEL_PORT` explicit override compatibility, and `5557`/`5558` port reservation across all publishing roles including `Decode`.
- `pkg/worker/controllers/worker`: `2026-11-25` - cover all added and modified ModelDeployment reconcile paths — `renderVersion=7` + pre-upgrade `bindingHash` currency match (`pair-ascend-llm-d` zero rollout), `spec.engine.version` hash invalidation, `vllm-router` `kv_aware` hash decoupling vs toggle rollout, conditional env stripping in `mergeModelDeploymentUserEnv`, scaler capacity-neutral env gate (`VLLM_KV_EVENTS_USE_INT_BLOCK_HASHES`), `sgl-router` Envoy allowlist sidecar / `:9091` probe / strict observer schema / EndpointSlice role-Service labels / `endpointslices` Role RBAC / EPD matrix rows, `KVEventsPublishing` condition matrix, and dead compile-cache env removal.
- `pkg/worker/extensionapis/worker`: `2026-11-25` - cover `sgl-router` metric gauge and counter scraping/parsing in `model_deployment.metrics_test.go`.

#### Integration tests
- Fake-client controller reconciliation scenarios verifying:
  - Standing `renderVersion=6` `vLLM-Ascend + llm-d-router` and legacy versionless CRs stay current with zero Pod replacement on operator upgrade, scale, and pause.
  - `vllm-router` toggling `--prefill-policy kv_aware` / `--decode-policy kv_aware` rolls only affected roles while unrelated `router.extraArgs` edits leave worker Pods untouched.
  - `sgl-router` full object set (Deployment, ConfigMap, Service, role Service `role-kind` label, ServiceAccount, Role with `endpointslices`, RoleBinding, observation view) reconciles idempotently across `Server`, `PD`, `EPD`, and `External DP`.
- Shell end-to-end verification via `bash hack/check/autosync-selftest.sh` covering `sgl-router` tracking, `sglang-gateway` freeze rejection, and `Base Image` build-recommendation validation for modified `pack/*/Dockerfile`.

#### e2e tests
- Container build verification of `pack/llm-router/Dockerfile` (`sgl-router-builder` + `0001-observer-endpoints.patch` + frozen `sglang-gateway-builder`) runs in remote Linux Buildx / `Base Image` CI workflow because the local macOS workstation lacks Linux Buildx and GPU/NPU hardware.
- Post-build live-cluster verification on GPU/NPU hardware (via `gpustack-operator-e2e` and `gpustack-operator-benchmark`) validates `sgl-router` EndpointSlice + KV-event routing, `vllm-router` `kv_aware` on `vLLM >= 0.31.0`, `llm-d-router` `kv@<model>` on `vLLM` / `vLLM-Ascend`, and Ascend `MooncakeConnectorV2` P/D transfer.

## Alternatives

- **Enable `precise-prefix-cache-producer` for `SGLang + llm-d-router` immediately:** Rejected because `llm-d-router@v1.0.0` (and current `main`) enforces `token-producer` `backend: "vllm"`, which calls `/v1/chat/completions/render`, returning `404` on SGLang. Deferred to a tracked TODO issue until upstream adds a non-vLLM tokenizer backend.
- **Reuse enum `sglang-gateway` and swap its binary to `sgl-router`:** Rejected because `sgl-router` uses incompatible CLI flags, routing policies, and Kubernetes `EndpointSlice` discovery. Reusing one enum name would break existing `sglang-gateway` `extraArgs` or split identical specs by hidden `renderVersion` branches.
- **Omit `--no-tokenizer` by default on `sgl-router`:** Rejected because `sgl-router` downloads tokenizers from Hugging Face Hub on startup using `--model-id`, causing CrashLoopBackOff in air-gapped clusters or when `spec.model.name` is a custom alias.
- **Unconditionally enable KV events and `sha256_cbor` for all `vllm-router` deployments:** Rejected because `vllm-router` defaults to `cache_aware`, which does not consume KV events. Forcing `sha256_cbor` globally would open unused sockets, roll standing workloads, and alter shared `KVCachePool` hash keys unnecessarily.
- **Drop `VLLM_USE_V2_MODEL_RUNNER=1` completely without version gating:** Rejected because `vLLM` `0.30.0–0.32.x` `ECCPUConnector` raises `ValueError` at startup when `VLLM_USE_V2_MODEL_RUNNER` is unset.
- **Expose `MooncakeConnectorV1` vs `MooncakeConnectorV2` as a public CRD enum:** Rejected because it leaks internal upstream connector generations into the long-lived `worker.gpustack.ai/v1` API, whereas engine version resolution selects the right generation cleanly.
- **Add `ECMooncakeConnector` in this spec by pinning an unreleased `llm-d-router` commit:** Rejected because `llm-d-router` sidecar is an external release image and PR `#3529` is not yet in a tagged release.

## Open Questions

None — all design forks (UPDATE image/version mismatch, pre-release `(major, minor, patch)` milestone comparison, `sgl-router` observer patch and `--no-tokenizer` default, `SGLang + llm-d-router` deferral, legacy baseline consistency, and `.auto-sync/` `Base Image` build recommendations) are resolved above.
