# Spec: ModelDeployment Router Upgrades, External DP Balancing, and Auto-Sync Commit Graduation

Status: Building
Blocked on: PR 2 (`T7` public site and documentation alignment) remaining before marking `Shipped`.
Type: Feature

## Summary

This specification upgrades the three active `ModelDeployment` routers (`llm-d-router`, `vllm-router`, and `sgl-router`) to pinned upstream `main` commits in `pack/llm-router/Dockerfile`, retires the upstreamed `vllm-router` metadata patch, fixes `llm-d-router` traffic stickiness under External DP, activates `endpoint-health-filter` on `llm-d-router` after image publication, promotes `gpustack/mirrored-llm-d-router-disagg-sidecar:v1.0.0`, extends `.auto-sync/` (Loopwright) with ancestry-gated commit-to-tag graduation plus `Sync Image` sidecar notifications, and refreshes the public site and reference documentation with a complete router capability matrix. It keeps the `ModelDeploymentRouter` CRD schema unchanged and explicitly declines integrating `smg-project/smg` as a fourth east-west router.

## Motivation

### Goals

- **Target users:** Platform operators choosing and running disaggregated or data-parallel `ModelDeployment` routers, and maintainers operating `pack/llm-router`, `.auto-sync/`, `Base Image` / `Sync Image` workflows, and the public Hugo documentation site.
- **Even External DP traffic on `llm-d-router`:** Remove prefix-cache stickiness on `llm-d-router` External DP `Server` roles so requests distribute across all rank `Pod`s without adding CRD fields.
- **Adopt high-value upstream router capabilities safely:**
  - `llm-d-router` (`3283d440334109fc785d4380355cc0375fc32755`): rebase the observer patch, validate rendered configs with `epp --validate-config` in tests, and activate `endpoint-health-filter` (with conservative 502/503 ejection bounds) once the new router image publishes.
  - `vllm-router` (`c132dbc0cfdf5c642c3dca6caa71218f3c3035cb`): retire `0005-kv-index-discover-metadata-from-metrics.patch` (merged upstream in [vllm-project/router#520](https://github.com/vllm-project/router/commit/51c675a)), rebase `0002` and `0003`, and gain hybrid `kv_aware` prefix reuse (`#493`), post-prefill Decode selection (`#500`), cancellation-safe load accounting (`#200`), and EPD consumer `DP > 1` support (`#507`).
  - `sgl-router` (`51c77c89f889050599b6e6e4838a44c3b95d8384`): keep `0001-observer-endpoints.patch` clean and gain `--dp-rank-policy least_tokens` / `--decode-dp-rank-policy` (`#51147`, `#51415`), stateless `/v1/responses` (`#42314`), multimodal cache affinity (`#48937`), legacy `sgl-model-gateway` flag rejection (`#50643`), and `RUSTSEC-2026-0592` (`#52663`).
- **Promote `llm-d-router` disaggregation sidecar to `v1.0.0`:** Switch `ModelDeploymentRoutingSidecarImage` from `v0.11.0` to the already-synced multi-arch `gpustack/mirrored-llm-d-router-disagg-sidecar:v1.0.0` to gain concurrent P/D failure cancellation (`#3412`, `#3364`), trailing-slash P/D fix (`#3410`), per-rank EC binding (`#3249`), `/v1/responses` (`#2987`), and Prometheus `/metrics` (`#1677`).
- **Safe commit-to-tag graduation and sidecar sync alerts in `.auto-sync/`:**
  - Support paired `*_VERSION` and optional `*_COMMIT` ARGs in [pack/llm-router/Dockerfile](../pack/llm-router/Dockerfile).
  - Prevent `.auto-sync/` from downgrading a pinned `*_COMMIT` to a tag that does not contain that commit (`git merge-base --is-ancestor`).
  - Automatically graduate a router back to a clean stable tag (`*_VERSION=<tag>`, `*_COMMIT=""`) once an eligible release tag contains the pinned commit.
  - Emit actionable `sync_images` recommendations with exact `gh workflow run "Sync Image"` commands whenever a newer stable `llm-d-router` tag requires mirroring `gpustack/mirrored-llm-d-router-disagg-sidecar:<tag>`.
- **Complete public site and docs router capability coverage:** Document every active and deprecated router's capabilities, limitations, policies, resilience behavior, API extensions, and image/engine version floors clearly across the public Hugo documentation site (`docs/modules/model-deployment/` + `site/`) and flat LLM reference pages (`docs/reference/`).

### Non-Goals

- **Integrating `smg-project/smg` into `ModelDeployment`:** Under stock HTTP engine containers, `smg` lacks real-time KV events and HTTP E/P/D (both require gRPC and `smg-grpc-servicer`), requires bumping `RUST_VERSION` from `1.95` to `1.98+`, adds an ~80 MB binary, and introduces a fourth fast-moving observer patch. Revisit `smg` only for a dedicated north-south/IGW gateway or gRPC serving architecture.
- **Adding new `spec.router` CRD fields:** Do not add `spec.router.policy`, circuit-breaker structs, stream idle timeouts, or `prefillTimeoutSeconds` in this change. Keep public `worker.gpustack.ai/v1` and internal `v1alpha1` schemas untouched.
- **Building `routing-proxy` sidecar from source in `pack/llm-router/`:** Keep the decode sidecar on the mirrored upstream release image (`gpustack/mirrored-llm-d-router-disagg-sidecar`). Features requiring post-`v1.0.0` sidecar code (`--ec-connector=ec-mooncake`, `--prefill-timeout`, `x-llm-d-failed-stage` attribution) wait for upstream `v1.1.0`.
- **Enabling `llm-d-router` multi-Pod `--data-parallel-start-rank-label` or `prefillCompletionFromKVEvents`:** Global start-rank routing works only with the vLLM Rust frontend (Python `vllm serve` rejects ranks `>= data_parallel_size_local`) and conflicts with Mooncake local rank IDs. Track follow-up adoption after engine compatibility matures.
- **Removing deprecated `sglang-gateway`:** Keep `SGLANG_GATEWAY_VERSION=gateway-v0.3.1` and `pack/llm-router/patches/sglang-gateway/` permanently frozen until its deprecation window ends.

## Proposal

Deliver the upgrade across two ordered pull requests that honor the repository's image-publication gate:

- **PR 1 — Image recipe and `.auto-sync/` graduation (`gpustack/llm-router:v0.6.1` build):**
  1. Update [pack/llm-router/Dockerfile](../pack/llm-router/Dockerfile) with paired `*_VERSION` / `*_COMMIT` build arguments for `llm-d-router`, `vllm-router`, and `sgl-router`, rebase required patches, and retire `vllm-router` patch `0005`.
  2. Upgrade `.auto-sync/` (`router-sync`) with ancestry-gated commit-to-tag graduation, anti-downgrade validation, `sync_images` recommendations for `mirrored-llm-d-router-disagg-sidecar`, and self-tests.
  3. After PR 1 merges, dispatch `Base Image` (`repository=llm-router`, `tag=v0.6.1`) and verify multi-arch (`linux/amd64`, `linux/arm64`) publication on Docker Hub.
- **PR 2 — Settings promotion, `llm-d-router` External DP & health filter, and site/docs (after `gpustack/llm-router:v0.6.1` publishes):**
  1. Promote `ModelDeploymentRouterImage` to `gpustack/llm-router:v0.6.1` and `ModelDeploymentRoutingSidecarImage` to `gpustack/mirrored-llm-d-router-disagg-sidecar:v1.0.0` (already mirrored and verified `linux/amd64` + `linux/arm64` via `Sync Image` run `45932551880`).
  2. Fix `llm-d-router` External DP balancing by omitting `prefix-cache-scorer` on External DP `Server` profiles while keeping `queue-scorer`, `kv-cache-utilization-scorer`, and `max-score-picker` round-robin tie-breaking.
  3. Render `endpoint-health-filter` in `llm-d-router` `server`, `prefill`, and `decode` profiles (never `encode`), and validate rendered `epp.yaml` configs offline with `epp --validate-config`.
  4. Publish the complete router capability matrix and updated contracts across the public Hugo site pages (`docs/modules/model-deployment/`) and LLM reference pages (`docs/reference/`).

### User Stories

#### Story 1
As a platform operator running External DP `Server` workloads behind `llm-d-router`, I want requests to spread across all qualified rank `Pod`s instead of sticking to one rank via prefix affinity, so that every rank handles traffic without requiring a CRD schema change.

#### Story 2
As a platform operator running disaggregated `vLLM` or `SGLang` deployments, I want `gpustack/llm-router:v0.6.1` and `mirrored-llm-d-router-disagg-sidecar:v1.0.0` to include hybrid `kv_aware` routing, post-prefill Decode selection, `sgl-router` `least_tokens` DP routing, `/v1/responses`, `llm-d-router` `endpoint-health-filter`, and `v1.0.0` sidecar P/D fixes, so that serving reliability and load balance improve across all three routers.

#### Story 3
As a maintainer operating `.auto-sync/` (Loopwright), I want routers temporarily pinned to upstream `main` commits via `*_COMMIT` to graduate automatically to the first stable release tag that contains that commit—and never downgrade to a tag that omits it—so that temporary commit pins return cleanly to stable releases.

#### Story 4
As a maintainer tracking upstream `llm-d-router` releases, I want `.auto-sync/` to report an exact `gh workflow run "Sync Image"` command whenever a newer stable release needs `gpustack/mirrored-llm-d-router-disagg-sidecar:<tag>` mirrored on Docker Hub, so that sidecar updates are never missed.

#### Story 5
As a platform operator browsing the GPUStack Operator documentation site, I want a clear side-by-side comparison of every managed router's capabilities, routing policies, KV-event support, P/D and E/P/D behavior, External DP behavior, fault ejection, and version floors, so that I can pick the right router without reading Go or Rust source code.

### Core Features & Acceptance Criteria

#### F1 — Paired `*_VERSION` / `*_COMMIT` Recipe and Patch Refresh (`pack/llm-router`)

- **Paired build arguments:** Keep `ARG LLMD_ROUTER_VERSION=v1.0.0`, `ARG VLLM_ROUTER_VERSION=v0.1.17`, and `ARG SGL_ROUTER_VERSION=router-v0.1.0` in [pack/llm-router/Dockerfile](../pack/llm-router/Dockerfile). Add paired commit arguments:
  - `ARG LLMD_ROUTER_COMMIT=3283d440334109fc785d4380355cc0375fc32755`
  - `ARG VLLM_ROUTER_COMMIT=c132dbc0cfdf5c642c3dca6caa71218f3c3035cb`
  - `ARG SGL_ROUTER_COMMIT=51c77c89f889050599b6e6e4838a44c3b95d8384`
- **Clone rule per builder stage:**
  - If `<ROUTER>_COMMIT` is non-empty, initialize a repo (`git init`), configure sparse-checkout when applicable (`sgl-router rust`), run `git fetch --depth 1 origin "${<ROUTER>_COMMIT}"`, checkout `FETCH_HEAD`, and initialize submodules. Stamp `epp` `BuildRef` as `${LLMD_ROUTER_VERSION}+${LLMD_ROUTER_COMMIT:0:7}`.
  - If `<ROUTER>_COMMIT` is empty, run the existing `git clone --depth 1 --branch "${<ROUTER>_VERSION}"` flow.
- **Patch updates:**
  - `llm-d`: rebase [0001-observer-endpoints-and-dispatch.patch](../pack/llm-router/patches/llm-d/0001-observer-endpoints-and-dispatch.patch) onto `3283d440334109fc785d4380355cc0375fc32755`. Register `systemone-parser` and `endpoint-health-filter` in `SupportedPlugins`, and add test coverage in `TestObserverSupportsRenderedOperatorProfiles` for both External DP and `endpoint-health-filter` profiles.
  - `vllm-router`: delete [0005-kv-index-discover-metadata-from-metrics.patch](../pack/llm-router/patches/vllm-router/0005-kv-index-discover-metadata-from-metrics.patch) (merged upstream in [vllm-project/router#520](https://github.com/vllm-project/router/commit/51c675a)). Rebase [0002-mooncake-lazy-bootstrap-lookup.patch](../pack/llm-router/patches/vllm-router/0002-mooncake-lazy-bootstrap-lookup.patch) (adapting to the lock-free `MooncakePrefillTable` refactor in `#526`) and [0003-observer-read-only-endpoint-view.patch](../pack/llm-router/patches/vllm-router/0003-observer-read-only-endpoint-view.patch) onto `c132dbc0cfdf5c642c3dca6caa71218f3c3035cb`.
  - `sgl-router`: keep [0001-observer-endpoints.patch](../pack/llm-router/patches/sgl-router/0001-observer-endpoints.patch) verified cleanly against `51c77c89f889050599b6e6e4838a44c3b95d8384`.
  - `sglang-gateway`: do not modify `SGLANG_GATEWAY_VERSION=gateway-v0.3.1` or `pack/llm-router/patches/sglang-gateway/`.
- **Acceptance criteria:**
  - Every active patch applies cleanly with `git apply --check` against its pinned commit SHA with zero `.rej` files.
  - `epp-builder`'s upstream `TestObserverSupportsRenderedOperatorProfiles` (`./cmd/epp/runner`) and `./pkg/epp/observer/...` Go tests pass on `3283d440334109fc785d4380355cc0375fc32755`.
  - `Base Image` builds and publishes multi-arch (`linux/amd64`, `linux/arm64`) `gpustack/llm-router:v0.6.1` from PR 1 before PR 2 merges.

#### F2 — `.auto-sync/` Ancestry-Gated Commit Graduation and Sidecar `Sync Image` Alerts

- **Commit-to-tag graduation (`preflight-router.sh` and `validate_router.sh`):**
  - Read both `*_VERSION` and optional `*_COMMIT` for `llm-d-router`, `vllm-router`, and `sgl-router`.
  - When `*_COMMIT` is non-empty, fetch full commit ancestry for the latest stable tag `T` (excluding prereleases) and run `git merge-base --is-ancestor "${PINNED_COMMIT}" "refs/tags/${T}"`.
  - If `T` does **not** contain `PINNED_COMMIT`, treat the router as held (`graduation_status: "awaiting_tag_containing_commit"`), never trigger a downgrade, and record `pinned_commit` and `latest_tag` in `router-preflight-report.json`.
  - If `T` **does** contain `PINNED_COMMIT`, trigger Stage A graduation: set `*_VERSION=${T}`, clear `*_COMMIT=""`, rebase patches from `PINNED_COMMIT` to `refs/tags/${T}`, and record `graduation_status: "ready_to_graduate"`.
  - In [validate_router.sh](../.auto-sync/checks/validate_router.sh), fail closed if a candidate clears or changes a non-empty `*_COMMIT` without preflight proof that `is-ancestor(BASE_COMMIT, CAND_VERSION) == true`, or if patches fail `git apply --check` against the effective ref (`*_COMMIT` when non-empty, else `*_VERSION`).
  - Preserve the permanent `sglang-gateway` freeze guard.
- **Sidecar `Sync Image` recommendations:**
  - Compare the highest stable `llm-d-router` tag `T` (or `LLMD_ROUTER_VERSION`) against `ModelDeploymentRoutingSidecarImage` in [value.go](../pkg/worker/settings/value.go).
  - If `sidecar` is not held in [.auto-sync/setting-image-holds.json](../.auto-sync/setting-image-holds.json), `T` is newer than the current default sidecar tag, and `gpustack/mirrored-llm-d-router-disagg-sidecar:${T}` is missing or not multi-arch on Docker Hub, emit a `sync_images` entry in `router-preflight-report.json`, `router-preflight-summary.json`, and [.auto-sync/proposals/router.json](../.auto-sync/proposals/router.json) containing:
    - `source`: `ghcr.io/llm-d/llm-d-router-disagg-sidecar:${T}`
    - `destination`: `gpustack/mirrored-llm-d-router-disagg-sidecar:${T}`
    - `reason`: concise explanation
    - `command`: `gh workflow run "Sync Image" -f images="ghcr.io/llm-d/llm-d-router-disagg-sidecar:${T}"`
  - When no sidecar mirror sync is needed, emit `"sync_images": []`.
  - Add `sync_images` to `requiredFields` in [.auto-sync/router-taskpack.yaml](../.auto-sync/router-taskpack.yaml) and document both graduation and `sync_images` in [.auto-sync/instructions/router.md](../.auto-sync/instructions/router.md).
- **Acceptance criteria:**
  - [hack/check/autosync-selftest.sh](../hack/check/autosync-selftest.sh) passes with new fixtures covering:
    1. A newer tag that does **not** contain `*_COMMIT` stays held without downgrading.
    2. A candidate that manually clears `*_COMMIT` onto a non-ancestor tag is rejected by `validate_router.sh`.
    3. A newer tag that **does** contain `*_COMMIT` graduates cleanly to `*_VERSION=<tag>` + `*_COMMIT=""` and rebases patches from `*_COMMIT` to `<tag>`.
    4. A newer stable `llm-d-router` tag missing from `gpustack/mirrored-llm-d-router-disagg-sidecar` emits the exact `sync_images` recommendation and `gh workflow run "Sync Image"` command.

#### F3 — `llm-d-router` External DP Balanced Routing (No CRD Changes)

- **Rendering rule ([router.go](../pkg/worker/kvcache/router/router.go), [model_deployment_router.go](../pkg/worker/controllers/worker/model_deployment_router.go)):**
  - Add an `ExternalDP bool` field to `router.Role` (internal Go struct only, no API change), populated from `modelDeploymentRoleExternalDP(md, role)` in [model_deployment_router.go](../pkg/worker/controllers/worker/model_deployment_router.go).
  - When rendering an `llm-d-router` `server` profile for an External DP role (`role.ExternalDP == true`), omit `prefix-cache-scorer` from that profile's plugins while retaining `queue-scorer` (`weight: 2`), `kv-cache-utilization-scorer` (`weight: 2`), and `max-score-picker` (which rotates equal-score candidates round-robin).
  - Keep non-External-DP `server`, `prefill`, `decode`, and `encode` scoring weights unchanged.
- **Acceptance criteria:**
  - Unit and golden render tests verify that `llm-d-router` External DP `Server` profiles contain `[server-pods, endpoint-health-filter, queue-scorer(2), kv-cache-utilization-scorer(2), max-score-picker]` and exclude `prefix-cache-scorer`.
  - Non-External-DP `Server` and `Prefill` profiles retain `prefix-cache-scorer(3)`.

#### F4 — Settings Promotion (`llm-router:v0.6.1`, Sidecar `v1.0.0`) and `endpoint-health-filter` (PR 2)

- **Default image promotions (gated on `gpustack/llm-router:v0.6.1` publication after PR 1):**
  - Update `ModelDeploymentRouterImage` default in [value.go](../pkg/worker/settings/value.go) from `gpustack/llm-router:v0.6.0` to `gpustack/llm-router:v0.6.1`.
  - Update `ModelDeploymentRoutingSidecarImage` default in [value.go](../pkg/worker/settings/value.go) from `gpustack/mirrored-llm-d-router-disagg-sidecar:v0.11.0` to `gpustack/mirrored-llm-d-router-disagg-sidecar:v1.0.0`.
  - Update [value_test.go](../pkg/worker/settings/value_test.go), router/render tests, and [docs/reference/settings.md](../docs/reference/settings.md).
- **`endpoint-health-filter` activation:**
  - Render `endpoint-health-filter` in `renderLLMD` with conservative defaults (`pollFailureThreshold: 3`, or `0` when any non-`encode` role is an unmanaged take-over role with `len(role.Command) > 0`, `responseFailureThreshold: 5`, `responseFailureCodes: [502, 503]`, `maxEjectionPercent: 50`) and attach `pluginRef: endpoint-health-filter` immediately after `<kind>-pods` on `server`, `prefill`, and `decode` profiles only. Never attach `endpoint-health-filter` to `encode` (encoders do not export metrics to `metrics-data-source`).
  - Pass `--allow-experimental-plugins=true` from `renderModelDeploymentLLMDRouterWorkload` (`pkg/worker/controllers/worker/model_deployment_router.go`) and register `--allow-experimental-plugins` in `modelDeploymentRouterOwnedArgs[LLMD]` (`pkg/worker/webhooks/worker/model_deployment.go`), because upstream `llm-d-router@3283d440` registers `endpoint-health-filter` at `StabilityAlpha` and validates plugin stability at startup and `--validate-config`.
  - Validate every rendered `llm-d-router` golden `epp.yaml` configuration using `epp --validate-config --allow-experimental-plugins` in test verification (and/or `epp-builder` unit tests).
- **Acceptance criteria:**
  - PR 1 never references unpublished `gpustack/llm-router:v0.6.1` and never renders `endpoint-health-filter` in `epp.yaml`.
  - PR 2 promotes `gpustack/llm-router:v0.6.1` and `gpustack/mirrored-llm-d-router-disagg-sidecar:v1.0.0` only after Docker Hub confirms multi-arch manifest availability, renders `endpoint-health-filter` on `server`/`prefill`/`decode` (absent on `encode`), and passes all router/render tests and `epp --validate-config`.

#### F5 — Public Site and Documentation Router Capability Coverage

- **Public Human Site (`docs/modules/model-deployment/` rendered into Hugo `site/` via [site/data/navigation.yaml](../site/data/navigation.yaml)):**
  - Expand [docs/modules/model-deployment/routing.md](../docs/modules/model-deployment/routing.md) with a clear, self-contained **Router Capabilities Matrix** comparing `llm-d-router`, `vllm-router`, `sgl-router`, and deprecated `sglang-gateway` across all operational axes:
    - Engine pairing (`vLLM`, `vLLM-Ascend`, `SGLang`) and discovery mechanism (`Pod` watcher vs `EndpointSlice`).
    - Default policies and `extraArgs` overrides (including `vllm-router` `kv_aware` hybrid architectures and `sgl-router` `--dp-rank-policy least_tokens`, `--decode-dp-rank-policy`, `--cache-media-affinity-ttl-ms`, and legacy `sgl-model-gateway` flag rejection).
    - Real-time KV events (active roles, engine version floors, hybrid model support).
    - P/D and E/P/D capabilities (post-prefill Decode selection on `vllm-router`, EPD consumer `DP > 1` on `vllm-router`, `llm-d-router` `v1.0.0` sidecar concurrent P/D failure cancellation and `/v1/responses`).
    - External DP behavior across every router (updating `llm-d-router` from prefix-sticky to load-balanced round-robin without prefix affinity).
    - Fault resilience and circuit breaking (`llm-d-router` `endpoint-health-filter` on `server`/`prefill`/`decode` for 502/503 with `maxEjectionPercent: 50`, alongside `vllm-router` and `sgl-router` built-in circuit breakers).
    - API surface highlights (for example stateless `/v1/responses` on `sgl-router` and `llm-d-router` `v1.0.0` sidecar).
  - Update [docs/modules/model-deployment/external-dp.md](../docs/modules/model-deployment/external-dp.md), [docs/modules/model-deployment/prefill-decode.md](../docs/modules/model-deployment/prefill-decode.md), and [docs/modules/model-deployment/engine-versions.md](../docs/modules/model-deployment/engine-versions.md) so External DP guidance, P/D router options, and minimum image floors (`gpustack/llm-router >= v0.6.1`, `gpustack/mirrored-llm-d-router-disagg-sidecar >= v1.0.0`) stay consistent across the site.
- **LLM Reference and Shared Index (`docs/reference/`, [docs/README.md](../docs/README.md)):**
  - Update [docs/reference/model-deployment-parameters.md](../docs/reference/model-deployment-parameters.md) with exact rendered `epp.yaml` plugins (`endpoint-health-filter` parameters and `encode` exclusion; External DP `server` profile omitting `prefix-cache-scorer`), updated `sgl-router` / `vllm-router` flags, and version boundaries.
  - Update [docs/reference/settings.md](../docs/reference/settings.md) for `ModelDeploymentRoutingSidecarImage` (`v1.0.0`) and `ModelDeploymentRouterImage` (`v0.6.1`) in PR 2.
  - Register this spec in [docs/README.md](../docs/README.md) (`#module-map` Model Deployment entry) and follow the three-zone docs contract (H1–H3 ceiling, short sentence-case headings, no `reference/` links from public site pages, clean Hugo site build).
- **Acceptance criteria:**
  - Every router capability documented on the public site (`docs/modules/model-deployment/routing.md`, `external-dp.md`, `prefill-decode.md`, `engine-versions.md`) and in `docs/reference/` matches current rendered code 1:1.
  - `make lint docs` (including Hugo `site/` build, `check-docs.sh`, `check-specs.sh`, `site-links.py`, and `agent-index.py`) passes with zero errors or warnings.

### Notes / Constraints / Caveats

- **Two-PR publication invariant:** `endpoint-health-filter` is absent in `llm-d-router@v1.0.0` (`gpustack/llm-router:v0.6.0`). Rendering `endpoint-health-filter` before `gpustack/llm-router:v0.6.1` is published and set as default would crash-loop `llm-d-router` Pods. Merge PR 1 and publish `gpustack/llm-router:v0.6.1` before merging PR 2.
- **Standing-Pod rollout impact:**
  - Router-only `epp.yaml` edits (External DP profile change, `endpoint-health-filter`, and `ModelDeploymentRouterImage` promotion) roll only the router `Deployment`; worker `Pod`s do not roll because router config hashes are decoupled from worker hashes.
  - Promoting `ModelDeploymentRoutingSidecarImage` from `v0.11.0` to `v1.0.0` does **not** roll standing `Decode` Pods automatically: `pod-spec-hash` (`modelDeploymentBindingHash` / `modelDeploymentExplicitSpecHash` in [model_deployment_render.go](../pkg/worker/controllers/worker/model_deployment_render.go)) excludes the sidecar setting (`Effect window: Next trigger` in [docs/reference/settings.md](../docs/reference/settings.md)). Only newly created or recreated `Decode` Pods adopt `v1.0.0`; keep `modelDeploymentRenderVersion` at `7`.
- **Observer contract under `endpoint-health-filter`:** `llm-d` patch `0001` currently reports `circuit_open: false` on `/observer/endpoints` (erring conservative so ejected pods are never prematurely treated as retired). Document this observation behavior alongside `endpoint-health-filter` in `routing.md`.
- **Upstream `llm-d-router` Go toolchain:** `llm-d-router@3283d440` declares `go 1.27.4` in `go.mod`; `ARG GO_VERSION=1.27-bookworm` in [pack/llm-router/Dockerfile](../pack/llm-router/Dockerfile) already resolves the latest `1.27.x` toolchain. Both `vllm-router@c132dbc` and `sgl-router@51c77c89f88` continue to compile on `ARG RUST_VERSION=1.95-bookworm`.
- **Legacy `v0.11.0` / `v1.0.0` sidecar header compatibility:** Upstream `llm-d-router` `3283d440` still writes both canonical (`x-llm-d-*`) and legacy (`x-prefiller-host-port`, `x-encoder-hosts-ports`) headers via `SetRoutingHeader`, so standing `v0.11.0` `Decode` Pods and custom overrides pinned to `v0.11.0` keep working for basic P/D and EPD.

### Boundaries

- **Always:**
  - Enforce the two-stage publication gate: never reference an unpublished `gpustack/*` tag in `pkg/worker/settings/value.go` or `docs/reference/settings.md`.
  - Guard every `*_COMMIT` → `*_VERSION` graduation with `git merge-base --is-ancestor` so `.auto-sync/` never downgrades a router.
  - Follow the three-zone docs and Hugo site rules in `docs/README.md` (`H1`–`H3`, no `reference/` links from public pages) and run `make lint`, `make lint docs`, `bash hack/check/autosync-selftest.sh`, and focused `go test` suites before merging.
- **Ask first:**
  - Before adding any new field to `ModelDeploymentRouter` (`api/worker/v1` or `v1alpha1`).
  - Before building `routing-proxy` sidecar from source inside `pack/` instead of mirroring upstream releases.
  - Before unfreezing or removing `sglang-gateway`.
- **Never:**
  - Never modify `SGLANG_GATEWAY_VERSION=gateway-v0.3.1` or anything under `pack/llm-router/patches/sglang-gateway/`.
  - Never attach `endpoint-health-filter` to `llm-d-router`'s `encode` profile.
  - Never render `endpoint-health-filter` in a commit whose default router image still ships `llm-d-router@v1.0.0`.

### Risks and Mitigations

- **Silent downgrade during `.auto-sync/` commit pin** → Upstream cuts a patch tag (for example `v1.0.1` off a release branch) that does not contain our pinned `main` commit, or a shallow `--depth 1` clone breaks ancestry resolution. Mitigation: fetch full commit graphs (`--filter=blob:none`, no `--depth 1`) whenever `*_COMMIT` is non-empty, check `git merge-base --is-ancestor <pinned_commit> refs/tags/<tag>` in `preflight-router.sh` (holding as `held_back` / `awaiting_tag_containing_commit`), and enforce the same invariant fail-closed in `validate_router.sh`.
- **Empty `ARG *_COMMIT=""` crashes `.auto-sync/` parsers or triggers frozen `sglang-gateway-builder`** → Existing `dockerfile_arg.py` and `validate_router.sh` reject empty ARGs, and unmasked `*_COMMIT=` diffs trigger all builders in `CHANGED_STAGES`. Mitigation: add explicit `allow_empty` parsing for `*_COMMIT`, mask `*_COMMIT=` in `split_dockerfile`, map commit changes solely to their owning builder stage, and regenerate all six `.auto-sync/digests/*.lock.json` lockfiles whenever `.auto-sync/checks/` changes.
- **Custom `spec.router.image` pinned to `gpustack/llm-router:v0.6.0` after PR 2** → An existing `ModelDeployment` that explicitly overrides `spec.router.image` to `v0.6.0` (`llm-d-router@v1.0.0`) cannot parse `endpoint-health-filter`. Mitigation: document `gpustack/llm-router >= v0.6.1` as the minimum image version for `llm-d-router` overrides in release notes, `engine-versions.md`, and `routing.md`.
- **False endpoint ejection on unmanaged take-over roles or transient blips** → Aggressive ejection or failed `/metrics` scrapes on unmanaged `Command != nil` containers could eject healthy backends. Mitigation: restrict `responseFailureCodes` to `[502, 503]` (excluding `500` and `504`), set `maxEjectionPercent: 50`, exclude `encode` pods, and set `pollFailureThreshold: 0` (response-only health) whenever any non-`encode` role is unmanaged.
- **`vllm-router` `0002` lazy Mooncake rebase onto `#526`** → Upstream replaced the `MooncakePrefillTable` mutex with an immutable `Arc`. Mitigation: update `0002` to use `std::sync::RwLock<MooncakePrefillTable>` (never held across `.await`, matching upstream `nixl_prefill_info`), keyed by `(bootstrap_port, prefill_url)`, with dedicated Rust unit tests.
- **Mixed standing `Decode` sidecar versions (`v0.11.0` alongside new `v1.0.0` Pods)** → Because `ModelDeploymentRoutingSidecarImage` changes take effect on next trigger without rolling standing `Decode` Pods, a cluster can run `v0.11.0` and `v1.0.0` sidecars simultaneously. Mitigation: verified `llm-d-router@3283d440` writes both canonical (`x-llm-d-*`) and legacy (`x-prefiller-host-port`, `x-encoder-hosts-ports`) headers, and all rendered sidecar CLI flags are compatible across `v0.11.0` and `v1.0.0`.

## Design Details

### Commands

**Environment (confirmed):**
- **Local host (macOS):** Runs Go builds, unit/golden/legacy tests, `make lint`, `make lint docs` (including Hugo `site/` build), `git apply --check` against local upstream clones, and `bash hack/check/autosync-selftest.sh` (verified: `pkg/worker/kvcache/router` 0.7 s, `pkg/worker/settings` 0.7 s, focused controller tests pass).
- **Remote / CI (GitHub Actions Linux Buildx):** Runs `Base Image` (`gh workflow run "Base Image" -f repository=llm-router -f tag=v0.6.1`) for multi-arch (`linux/amd64`, `linux/arm64`) `gpustack/llm-router:v0.6.1` publication after PR 1 merges.

```bash
# Lint Go and shell sources
make lint

# Lint documentation, specs, module map, and Hugo site
make lint docs

# Run focused router, settings, controller, and webhook tests (local)
go test -v ./pkg/worker/kvcache/router/... ./pkg/worker/settings/... ./pkg/worker/controllers/worker/... ./pkg/worker/webhooks/worker/...

# Re-baseline golden YAML fixtures deliberately in PR 2 (review diff before keeping)
go test ./pkg/worker/controllers/worker/ -run TestModelDeploymentGoldenYAML -update

# Verify auto-sync scripts, commit-to-tag graduation, 6 digest lockfiles, freeze guard, and sync_images alerts
bash hack/check/autosync-selftest.sh

# Dispatch Base Image build after PR 1 merges (Remote CI)
gh workflow run "Base Image" -f repository=llm-router -f tag=v0.6.1
```

### Project Structure

- [pack/llm-router/Dockerfile](../pack/llm-router/Dockerfile) & `pack/llm-router/patches/` — Paired `*_VERSION`/`*_COMMIT` build recipe (with stage-scoped ARG declarations), rebased `llm-d` `0001` and `vllm-router` `0002`/`0003` patches, retired `vllm-router` `0005`, clean `sgl-router` `0001`, and permanently frozen `sglang-gateway` (PR 1).
- [.auto-sync/router-taskpack.yaml](../.auto-sync/router-taskpack.yaml), [.auto-sync/scripts/preflight-router.sh](../.auto-sync/scripts/preflight-router.sh), [.auto-sync/scripts/lib-preflight.sh](../.auto-sync/scripts/lib-preflight.sh), [.auto-sync/checks/dockerfile_arg.py](../.auto-sync/checks/dockerfile_arg.py), [.auto-sync/checks/base_image_builds.py](../.auto-sync/checks/base_image_builds.py), [.auto-sync/checks/validate_router.sh](../.auto-sync/checks/validate_router.sh), [.auto-sync/instructions/router.md](../.auto-sync/instructions/router.md), [.auto-sync/proposals/router.json](../.auto-sync/proposals/router.json), `.auto-sync/digests/*.lock.json` (all 6 lockfiles), and [hack/check/autosync-selftest.sh](../hack/check/autosync-selftest.sh) — Ancestry-gated commit-to-tag graduation, anti-downgrade validation, and `sync_images` sidecar recommendations (PR 1).
- [pkg/worker/kvcache/router/router.go](../pkg/worker/kvcache/router/router.go) & [pkg/worker/controllers/worker/model_deployment_router.go](../pkg/worker/controllers/worker/model_deployment_router.go) — `Role.ExternalDP` and `Input.HasUnmanagedRoles` plumbing to omit `prefix-cache-scorer` on External DP `Server` roles and render `endpoint-health-filter` on `server`/`prefill`/`decode` (PR 2).
- [pkg/worker/settings/value.go](../pkg/worker/settings/value.go) & [pkg/worker/settings/value_test.go](../pkg/worker/settings/value_test.go) — `ModelDeploymentRouterImage` (`v0.6.1`) and `ModelDeploymentRoutingSidecarImage` (`v1.0.0`) promotions (PR 2).
- [docs/modules/model-deployment/routing.md](../docs/modules/model-deployment/routing.md), [docs/modules/model-deployment/external-dp.md](../docs/modules/model-deployment/external-dp.md), [docs/modules/model-deployment/prefill-decode.md](../docs/modules/model-deployment/prefill-decode.md), [docs/modules/model-deployment/engine-versions.md](../docs/modules/model-deployment/engine-versions.md), [site/data/navigation.yaml](../site/data/navigation.yaml), [docs/reference/model-deployment-parameters.md](../docs/reference/model-deployment-parameters.md), [docs/reference/settings.md](../docs/reference/settings.md), and [docs/README.md](../docs/README.md) — Public Hugo site capability matrix, LLM parameter/settings reference, and module-map index update (PR 2).

### Code Style

Follow Go, Kubernetes, documentation, and shell conventions in `AGENTS.md`: `snake_case` filenames, explicit error handling, short plain comments without decorative symbols or task IDs, and table-driven unit tests.

Real pattern from [pkg/worker/kvcache/router/router.go](../pkg/worker/kvcache/router/router.go):

```go
profilePlugins = append(profilePlugins,
	map[string]any{"pluginRef": "queue-scorer", "weight": 2},
	map[string]any{"pluginRef": "kv-cache-utilization-scorer", "weight": 2},
	map[string]any{"pluginRef": "max-score-picker"})
if kind != roleKindDecode {
	profilePlugins = append(profilePlugins,
		map[string]any{"pluginRef": "prefix-cache-scorer", "weight": 3})
}
```

### Implementation Plan

> **Two-PR delivery boundary:** Tasks `T1`–`T2` ship in **PR 1**, followed by release checkpoint `T3` (`gpustack/llm-router:v0.6.1` multi-arch publication on Docker Hub). Tasks `T4`–`T7` ship in **PR 2** after `T3` succeeds. `T4` and `T5` have disjoint `Owns:` and run in parallel.

- [x] **T1 · `pack/llm-router` dual-ARG commit recipe and upstream patch refresh (PR 1)**
      Blocked by: None
      Owns: `pack/llm-router/**`
      Gate: review
      Acceptance: `pack/llm-router/Dockerfile` declares paired `*_VERSION` and stage-scoped `*_COMMIT` ARGs (`LLMD_ROUTER_COMMIT=3283d440334109fc785d4380355cc0375fc32755`, `VLLM_ROUTER_COMMIT=c132dbc0cfdf5c642c3dca6caa71218f3c3035cb`, `SGL_ROUTER_COMMIT=51c77c89f889050599b6e6e4838a44c3b95d8384`), fetches sparse commits with `--filter=blob:none` for `sgl-router`, stamps `epp` `BuildRef` cleanly with or without commit, and leaves `SGLANG_GATEWAY_VERSION=gateway-v0.3.1` and `pack/llm-router/patches/sglang-gateway/**` untouched; `llm-d/0001` is rebased onto `3283d440...` with `systemone-parser` and `endpoint-health-filter` in `SupportedPlugins` plus `observer_testdata` cases for External DP and `endpoint-health-filter`; `vllm-router/0005` is deleted, `0002` is rebased onto `c132dbc0...` using `std::sync::RwLock<MooncakePrefillTable>` (never held across `.await`), and `0003` is rebased cleanly; `sgl-router/0001` applies cleanly on `51c77c89...`.
      Verify: `git apply --check` on every active patch against its pinned upstream commit SHA, plus upstream `epp` observer Go tests (`./cmd/epp/runner` `TestObserverSupportsRenderedOperatorProfiles` and `./pkg/epp/observer/...`) and `vllm-router` `0002` unit tests.

- [x] **T2 · `.auto-sync` ancestry-gated commit graduation, `sync_images` alerts, and lockfile/selftest suite (PR 1)**
      Blocked by: T1
      Owns: `.auto-sync/**`, `hack/check/autosync-selftest.sh`
      Gate: review
      Acceptance: `dockerfile_arg.py`, `base_image_builds.py`, `preflight-router.sh`, `lib-preflight.sh`, `validate_router.sh`, `router-taskpack.yaml`, `instructions/router.md`, and `proposals/router.json` support optional empty `*_COMMIT=""` without triggering `sglang-gateway-builder`, fetch full commit graphs (`--filter=blob:none`, no `--depth 1`) when `*_COMMIT` is non-empty, enforce `git merge-base --is-ancestor` hold (`held_back` / `awaiting_tag_containing_commit`) vs graduation (`ready_to_graduate`, clearing `*_COMMIT=""` and rebasing from `PINNED_COMMIT`), validate `versions[uid]` (`<ver>+<short7>` when commit-pinned, `<ver>` when tag-pinned), emit `sync_images` recommendations in report, summary (`summary_highlights`), and proposal JSON for eligible unmirrored `mirrored-llm-d-router-disagg-sidecar:<tag>` releases, and regenerate all six `.auto-sync/digests/*.lock.json` lockfiles; `hack/check/autosync-selftest.sh` covers every positive and negative case.
      Verify: `bash hack/check/autosync-selftest.sh && make lint`

- [x] **T3 · PR 1 merge and `gpustack/llm-router:v0.6.1` multi-arch publication checkpoint**
      Blocked by: T1, T2
      Owns: None
      Gate: review
      Acceptance: PR 1 merges cleanly to `main`; `gh workflow run "Base Image" -f repository=llm-router -f tag=v0.6.1` succeeds; Docker Hub manifest for `gpustack/llm-router:v0.6.1` reports both `linux/amd64` and `linux/arm64` digests.
      Verify: `crane manifest docker.io/gpustack/llm-router:v0.6.1 | jq '.manifests[].platform'` (or Docker Hub tag API verifying `amd64` and `arm64`).

- [x] **T4 · `llm-d-router` External DP balancing and `endpoint-health-filter` renderer (PR 2)**
      Blocked by: T3
      Owns: `pkg/worker/kvcache/router/**`
      Gate: review
      Acceptance: `router.Role` gains `ExternalDP bool` and `router.Input` gains `HasUnmanagedRoles bool`; `renderLLMD` combines `ExternalDP` per role kind via logical OR (order-invariant across mixed `Server` roles), omits `prefix-cache-scorer` on External DP `server` profiles (retaining `queue-scorer(2)`, `kv-cache-utilization-scorer(2)`, and `max-score-picker`), renders `endpoint-health-filter` (`pollFailureThreshold: 3`, or `0` when `HasUnmanagedRoles` is true, `responseFailureThreshold: 5`, `responseFailureCodes: [502, 503]`, `maxEjectionPercent: 50`) placed immediately after `<kind>-pods` on `server`, `prefill`, and `decode` profiles only (strictly absent from `encode`), and keeps non-External-DP `server`/`prefill` scoring weights unchanged. All rendered shapes pass `epp --validate-config`.
      Verify: `go test -v -cover ./pkg/worker/kvcache/router/...`

- [x] **T5 · Default router and sidecar image promotions (PR 2)**
      Blocked by: T3
      Owns: `pkg/worker/settings/**`
      Acceptance: `ModelDeploymentRouterImage` defaults to `gpustack/llm-router:v0.6.1` and `ModelDeploymentRoutingSidecarImage` defaults to `gpustack/mirrored-llm-d-router-disagg-sidecar:v1.0.0` in `pkg/worker/settings/value.go`, with `pkg/worker/settings/value_test.go` updated and passing.
      Verify: `go test -v -cover ./pkg/worker/settings/...`

- [x] **T6 · Controller wiring, webhook owned-arg guard, golden fixture updates, and standing-Pod currency verification (PR 2)**
      Blocked by: T4, T5
      Owns: `pkg/worker/controllers/worker/**`, `pkg/worker/webhooks/worker/**`
      Gate: review
      Acceptance: `model_deployment_router.go` sets `ExternalDP: len(role.Command) == 0 && modelDeploymentRoleExternalDP(md, role)`, populates `HasUnmanagedRoles` when any non-`encode` role sets `len(role.Command) > 0`, and renders `--allow-experimental-plugins=true` on `llm-d-router` Pods (with `--allow-experimental-plugins` added to `modelDeploymentRouterOwnedArgs[LLMD]` in `pkg/worker/webhooks/worker/model_deployment.go` and covered by webhook tests); unit and golden tests cover vLLM/SGLang External DP balancing, take-over role safety, `endpoint-health-filter` placement and `encode` exclusion, updated image assertions (`v0.6.1` and `v1.0.0`), re-baselined router/direct-decoder serialized digests (with non-sidecar pod digests untouched), updated `testdata/golden-render/*_dst.yaml`, and green `TestModelDeploymentLegacyRender_GoldenPods` + `TestModelDeploymentLegacyRender_DynamicRenderChangeKeepsPodSpecHash` at `modelDeploymentRenderVersion == 7`.
      Verify: `go test -v ./pkg/worker/controllers/worker/... -run 'TestRenderModelDeployment|TestModelDeploymentLegacyRender|TestModelDeploymentGoldenYAML|TestModelDeploymentRouter' && go test -v ./pkg/worker/webhooks/worker/...`

- [ ] **T7 · Public Hugo site and docs Router Capability Matrix alignment (PR 2)**
      Blocked by: T6
      Owns: `docs/**`, `site/**`
      Acceptance: `docs/modules/model-deployment/routing.md` carries a complete Router Capabilities Matrix (`llm-d-router`, `vllm-router`, `sgl-router`, deprecated `sglang-gateway`) covering engine pairing, discovery, policies/`extraArgs`, KV events, P/D and E/P/D, External DP, `endpoint-health-filter` & circuit breakers, API extensions (`/v1/responses`), and observer semantics; `external-dp.md`, `prefill-decode.md`, `engine-versions.md` (`gpustack/llm-router >= v0.6.1`, `mirrored-llm-d-router-disagg-sidecar >= v1.0.0`), `docs/reference/model-deployment-parameters.md`, `docs/reference/settings.md`, and `docs/README.md` (`#module-map` Model Deployment entry registering this spec) are synchronized under the three-zone docs contract with zero `reference/` links from public site pages.
      Verify: `make lint docs`

### Test Plan

[x] I/we understand the owners of the involved components may require updates to existing tests to make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

- Keep `TestModelDeploymentLegacyRender_GoldenPods` and `TestModelDeploymentLegacyRender_DynamicRenderChangeKeepsPodSpecHash` passing at `modelDeploymentRenderVersion == 7` as a regression guard against accidental standing-Pod rollouts.
- Update `hack/check/autosync-selftest.sh` fixture isolation so local fixture repositories do not break when live `pack/llm-router/Dockerfile` carries real upstream `*_COMMIT` SHAs.

#### Unit tests

Target coverage maintained or improved over 2026-11-27 local baselines:

- `pkg/worker/kvcache/router`: `2026-11-27` - `96.7%` (baseline; target `>= 96.7%` covering `ExternalDP` logical-OR order invariance, `endpoint-health-filter` on `server`/`prefill`/`decode`, strict `encode` exclusion, unmanaged `pollFailureThreshold: 0`, and non-`Server` guard)
- `pkg/worker/settings`: `2026-11-27` - `82.1%` (baseline; target `>= 82.1%` covering `v0.6.1` router and `v1.0.0` sidecar defaults)
- `pkg/worker/controllers/worker`: `2026-11-27` - focused router/render/legacy/golden suites passing (target: all router/render/golden/legacy tests green including take-over External DP guard, re-baselined serialized digests, and zero standing-Pod rollout)
- Upstream patch build-time tests (`pack/llm-router`): `llm-d` `TestObserverSupportsRenderedOperatorProfiles` + `pkg/epp/observer/...` (with External DP, `endpoint-health-filter`, `precise` + `dataLayer`, and `encode` fixtures) and `vllm-router` `0002` lazy `RwLock<MooncakePrefillTable>` unit tests.

#### Integration tests

- `.auto-sync/` end-to-end fixture suite (`bash hack/check/autosync-selftest.sh`):
  1. Empty `*_COMMIT=""` parsing and stage-diff isolation (only the modified router stage builds; `sglang-gateway-builder` never triggers).
  2. Non-ancestor newer patch tag (`v1.0.1` off release branch) holds commit-pinned router (`held_back` / `awaiting_tag_containing_commit`) without downgrading.
  3. Manual candidate clearing `*_COMMIT` onto a non-ancestor tag fails closed in `validate_router.sh`.
  4. Ancestor release tag (`v1.1.0` containing `PINNED_COMMIT`) graduates cleanly to `*_VERSION=v1.1.0`, `*_COMMIT=""`, rebasing patches from `PINNED_COMMIT` onto `refs/tags/v1.1.0`.
  5. `sync_images` recommendations emit exact `gh workflow run "Sync Image"` commands when an eligible `llm-d-router` tag lacks a multi-arch mirrored sidecar, and suppress alerts when published, held, or ineligible.
  6. All six `.auto-sync/digests/*.lock.json` integrity checks pass.
- Offline `epp --validate-config` validation across every rendered `llm-d-router` golden `epp.yaml` shape (`vLLM` Server precise, `SGLang` Server approx, External DP Server, `vLLM` PD precise, `SGLang` PD approx, `vLLM` EPD with `encode`, and unmanaged take-over Server).
- Documentation and Hugo site integration check (`make lint docs`: `check-docs.sh`, `check-specs.sh`, `site-links.py`, `agent-index.py`, and Hugo site build).

#### e2e tests

- **Container image build & publication (CI):** `Base Image` multi-arch build (`linux/amd64`, `linux/arm64`) for `gpustack/llm-router:v0.6.1` exercises all four upstream build stages, patch application, and `epp` build-time unit tests (`T3`). `Sync Image` multi-arch publication of `gpustack/mirrored-llm-d-router-disagg-sidecar:v1.0.0` is already verified in run `45932551880`.
- **Cluster e2e:** No new CRD fields or scheduling-chain changes are introduced, so no new Kind chaos/e2e suite is required to land PR 1 or PR 2. Optional post-merge GPU smoke check: verify `llm-d-router` External DP request spread across ranks and `endpoint-health-filter` 502/503 ejection on a live cluster with `gpustack/llm-router:v0.6.1`.

## Alternatives

- **Integrate `smg-project/smg` as a fourth `ModelDeployment` router:** Rejected. In stock HTTP mode, `smg` lacks real-time KV events and HTTP E/P/D (both require gRPC and `smg-grpc-servicer` inside engine containers), requires bumping `RUST_VERSION` to `1.98+`, adds an ~80 MB binary, and carries high observer-patch churn (2,873 commits between `v1.15.0` and `main`). Its strongest capabilities (IGW multi-model routing, Responses/Anthropic/MCP, tenant rate limits, HA mesh) fit a separate north-south gateway better than a per-deployment east-west router.
- **Add a `spec.router.policy` CRD field for External DP round-robin:** Rejected for now. Exposing a policy enum across `v1`/`v1alpha1` adds codegen, webhook cross-router refusal matrices (`sgl-router` lacks `round_robin` under `--chat-routing=reorg`), and `extraArgs --policy` conflict rules, while automatic External DP balancing on `llm-d-router` solves the stickiness issue with zero API expansion.
- **Pure scorer-less round-robin on `llm-d-router` External DP (`[filter, max-score-picker]`):** Rejected in favor of keeping `queue-scorer(2)` and `kv-cache-utilization-scorer(2)`. Omitting only `prefix-cache-scorer` still rotates evenly across idle/serial ranks via `max-score-picker` while steering concurrent traffic away from busy ranks.
- **Wait for upstream release tags instead of pinning `*_COMMIT`:** Rejected. None of `llm-d-router`, `vllm-router`, or `sgl-router` has cut a newer release tag, yet their `main` branches carry high-value stability fixes, `0005` patch retirement, and security updates (`RUSTSEC-2026-0592`). Pairing `*_COMMIT` with ancestry-gated `.auto-sync/` graduation captures these fixes now without losing automatic return to stable releases.

## Open Questions

None.
