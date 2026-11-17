# Disaggregated Inference Routers Auto-Sync (`router-sync`)

You maintain the disaggregated inference router build recipe, local patches, and default router image settings for `gpustack-operator`:

- `llm-d/llm-d-router` -> `ARG LLMD_ROUTER_VERSION` -> `epp-builder` stage in `pack/llm-router/Dockerfile` (`pack/llm-router/patches/llm-d/*.patch`)
- `vllm-project/router` -> `ARG VLLM_ROUTER_VERSION` -> `vllm-router-builder` stage in `pack/llm-router/Dockerfile` (`pack/llm-router/patches/vllm-router/*.patch`)
- `sgl-project/sglang` (filtered strictly to `gateway-v*` tags; never engine `v0.4.*` tags) -> `ARG SGLANG_GATEWAY_VERSION` -> `sglang-gateway-builder` stage in `pack/llm-router/Dockerfile` (`pack/llm-router/patches/sglang-gateway/*.patch`)
- `pkg/worker/settings/value.go` and `docs/reference/settings.md` (Stage B tag promotion only)

Before editing any file, inspect `$AUTOSYNC_STAGE_DIR/router-preflight-report.json`, `$AUTOSYNC_STAGE_DIR/upstream-records.json`, and `$AUTOSYNC_STAGE_DIR/image-manifests.json`.

## 1. Two-Stage Image Publication Gate

Never reference an unpublished `gpustack/*` container image tag in `pkg/worker/settings/value.go` or `docs/reference/settings.md`.

- **Stage A (`stage: "recipe_sync"`, `image_published: false`)**:
  - When upstream router tags advance and the target `gpustack/*` image tag is NOT yet published on Docker Hub, update ONLY:
    - `pack/llm-router/Dockerfile` (`LLMD_ROUTER_VERSION`, `VLLM_ROUTER_VERSION`, `SGLANG_GATEWAY_VERSION`)
    - `pack/llm-router/patches/**`
    - `.auto-sync/proposals/router.json` (`"stage": "recipe_sync"`)
  - Leave `pkg/worker/settings/value.go` and `docs/reference/settings.md` untouched.
- **Stage B (`stage: "tag_promotion"`, `image_published: true`)**:
  - Only when `$AUTOSYNC_STAGE_DIR/router-preflight-report.json` reports `"stage": "tag_promotion"` AND `$AUTOSYNC_STAGE_DIR/image-manifests.json` confirms `crane manifest` multi-arch digests (`linux/amd64` and `linux/arm64`) exist for the promoted tags:
    - Update `ModelDeploymentRouterImage`, `ModelDeploymentRouterProxyImage`, and/or `ModelDeploymentRoutingSidecarImage` in `pkg/worker/settings/value.go` and their matching `Default:` entries (`model-deployment-router-image`, `model-deployment-router-proxy-image`, `model-deployment-routing-sidecar-image`) in `docs/reference/settings.md`.
    - Update `.auto-sync/proposals/router.json`: set `"stage": "tag_promotion"`, and whenever `$AUTOSYNC_STAGE_DIR/upstream-records.json` carries `<ver>+stage-b` for an `upstreamId` (`llm-d-router`, `vllm-router`, `sglang-gateway`), you MUST write that exact `<ver>+stage-b` string into `versions[<upstreamId>]` so open-PR deduplication settles cleanly on subsequent runs.
  - Do NOT modify any other settings in `value.go` or `settings.md` (such as `KVCacheBackendImage` or `ModelDeploymentModelExpressServerImage`).
- **No Change (`stage: "no_change"`)**:
  - When all three router upstreams are `up_to_date` and no promoted image tag is ready, settle cleanly as `no_change`.

## 2. Automatic Retirement Rule for `vllm-router/0001-epd-routing.patch`

`pack/llm-router/patches/vllm-router/0001-epd-routing.patch` is a cherry-pick of upstream PR #266 (commit `516f1cb`) onto `v0.1.16.1`, and patches `0002` through `0004` stack on top of it.

- Check `retire_0001_epd_routing` in `$AUTOSYNC_STAGE_DIR/router-preflight-report.json` (computed via `git merge-base --is-ancestor 516f1cb <target_tag>`):
  - **When `retire_0001_epd_routing` is `false`**: MUST keep `pack/llm-router/patches/vllm-router/0001-epd-routing.patch`. Deleting it fails `validate_router.sh`.
  - **When `retire_0001_epd_routing` is `true`** and bumping `VLLM_ROUTER_VERSION` to the target tag: MUST delete `pack/llm-router/patches/vllm-router/0001-epd-routing.patch`, rebase `0002-mooncake-lazy-bootstrap-lookup.patch`, `0003-observer-read-only-endpoint-view.patch`, and `0004-discovery-readiness-and-deselect-removal.patch` directly onto `<target_tag>`, and update any explicit patch references or explanatory comments in `pack/llm-router/Dockerfile` if needed.

## 3. Patch Rebasing and Observation Preservation

- Test every patch under `pack/llm-router/patches/{llm-d,vllm-router,sglang-gateway}/*.patch` in lexical order with `git apply --check` against the staged clone in `$AUTOSYNC_STAGE_DIR/{llm-d-router,vllm-router,sglang}` at the candidate Dockerfile's pinned tag.
- Never drop observation or disaggregation hunks during a rebase:
  - `struct ObserverRouter` in `sgl-model-gateway/src/observer.rs` MUST remain intact after applying `pack/llm-router/patches/sglang-gateway/*.patch`.
  - Preserve the read-only observer views, Mooncake lazy bootstrap lookup, and Kubernetes discovery readiness/deselect removal behavior across all three routers.
- Keep patch header metadata (`AGAINST`, `WHEN TO REMOVE`) accurate when rebasing a patch onto a new tag.

## 4. Proposal Artifact

Keep `.auto-sync/proposals/router.json` valid JSON with `schema_version: 1`, `stage`, `summary`, `versions` (matching `upstream-records` IDs: `llm-d-router`, `vllm-router`, `sglang-gateway`), and `subscriptions`.
