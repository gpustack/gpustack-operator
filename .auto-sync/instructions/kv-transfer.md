# KV-Cache and Weight Transfer Auto-Sync (`kv-transfer-sync`)

You maintain the KV-cache and remote-weight transfer recipes and default image tags for `gpustack-operator`:
- `pack/mirrored-mooncake/Dockerfile` (`kvcache-ai/Mooncake`)
- `pack/model-transfer/Dockerfile` and `pack/model-transfer/patches/hixl/001_channel_generation.patch` (`ai-dynamo/nixl`, `openucx/ucx`, `hicann/hixl`, `gpustack/runner` parity)
- `pack/mirrored-modelexpress-server/Dockerfile` (`ai-dynamo/modelexpress`)
- `pkg/worker/settings/value.go`, `docs/reference/settings.md`, and `deploy/gpustack-operator/chart/values.yaml` (Stage B tag promotion only)

Before editing any file, inspect `$AUTOSYNC_STAGE_DIR/kv-transfer-preflight-report.json` and `$AUTOSYNC_STAGE_DIR/image-manifests.json`.

## 1. Two-Stage Image Publication Gate

Never reference an unpublished `gpustack/*` container image tag in `pkg/worker/settings/value.go`, `docs/reference/settings.md`, or `deploy/gpustack-operator/chart/values.yaml`.

- **Stage A (`stage: "recipe_sync"`, `image_published: false`)**:
  - When upstream recipes advance and the target `gpustack/*` image tag is NOT yet published on Docker Hub, update ONLY:
    - `pack/mirrored-mooncake/**`
    - `pack/model-transfer/**`
    - `pack/mirrored-modelexpress-server/**`
    - `.auto-sync/proposals/kv-transfer.json` (`"stage": "recipe_sync"`)
  - Leave `pkg/worker/settings/value.go`, `docs/reference/settings.md`, and `deploy/gpustack-operator/chart/**` untouched.
- **Stage B (`stage: "tag_promotion"`, `image_published: true`)**:
  - Only when `$AUTOSYNC_STAGE_DIR/kv-transfer-preflight-report.json` reports `"stage": "tag_promotion"` AND `$AUTOSYNC_STAGE_DIR/image-manifests.json` confirms `crane manifest` digests exist for the promoted tags:
    - Update `KVCacheBackendImage` and/or `ModelDeploymentModelExpressServerImage` in `pkg/worker/settings/value.go` and their matching `Default:` entries in `docs/reference/settings.md`.
    - Update `modelTransfer.images.nvidia`, `modelTransfer.images.amd`, and/or `modelTransfer.images.ascend` in `deploy/gpustack-operator/chart/values.yaml`, then run `make generate chart` so `deploy/gpustack-operator/chart/values.schema.json` and `deploy/gpustack-operator/chart/README.md` stay in sync.
    - Update `.auto-sync/proposals/kv-transfer.json` (`"stage": "tag_promotion"` and matching `versions` values including the `<version>+stage-b` token from `upstream-records`).
  - Do NOT modify any other settings in `value.go` or `settings.md` (such as `ModelDeploymentRouterImage`, `ModelDeploymentRouterProxyImage`, or `ModelDeploymentRoutingSidecarImage`) or any other keys in `deploy/gpustack-operator/chart/values.yaml`.

## 2. Five-Block `MOONCAKE_VERSION` `case` Audit

`pack/mirrored-mooncake/Dockerfile` contains four stage release guards (`builder`, `cuda`, `cann`, `rocm`) plus a fifth `context_manager.cpp` conditional patch block in the `cann` stage:
1. Before changing `ARG MOONCAKE_VERSION="..."`, inspect the upstream Mooncake release in `$AUTOSYNC_STAGE_DIR` (or release notes) to verify whether the existing CMake/wheel/wrapper steps and the `ascend_direct_transport/context_manager.cpp` glob fix still apply or need adjustment.
2. Update all four `case "${MOONCAKE_VERSION}" in ...` release-guard blocks AND audit the fifth `context_manager.cpp` `case` block in lockstep so every build stage accepts the pinned `MOONCAKE_VERSION`.

## 3. Byte-Identical `gpustack/runner` HIXL Parity

`HIXL_COMMIT` in `pack/model-transfer/Dockerfile` and `pack/model-transfer/patches/hixl/001_channel_generation.patch` MUST remain aligned with `VLLM_HIXL_COMMIT` and `pack/cann/patches/hixl/001_channel_generation.patch` in `$AUTOSYNC_STAGE_DIR/gpustack-runner`:
- If `gpustack/runner` moved `VLLM_HIXL_COMMIT` or updated `pack/cann/patches/hixl/001_channel_generation.patch`, synchronize `HIXL_COMMIT` and copy the exact patch bytes into `pack/model-transfer/patches/hixl/001_channel_generation.patch`.
- Verify `patch --dry-run --batch --fuzz=0 -p1` succeeds against `$AUTOSYNC_STAGE_DIR/hixl` at `HIXL_COMMIT`.

## 4. PyPI Wheel Parity for `NIXL_VERSION`

Only bump `ARG NIXL_VERSION="..."` in `pack/model-transfer/Dockerfile` to a stable version where ALL THREE PyPI packages (`nixl`, `nixl-cu12`, and `nixl-cu13`) publish Linux `x86_64` and `aarch64` wheels (using the `selected_version` reported in `$AUTOSYNC_STAGE_DIR/kv-transfer-preflight-report.json`).

## 5. ModelExpress `--locked` Cargo Verification

When bumping `ARG MODELEXPRESS_COMMIT=...` in `pack/mirrored-modelexpress-server/Dockerfile`, confirm the upstream `Cargo.lock` builds cleanly with `--locked --bin modelexpress-server --features memory-backend`.

## 6. Proposal Artifact

Keep `.auto-sync/proposals/kv-transfer.json` valid JSON with `schema_version: 1`, `stage`, `summary`, `versions` (matching `upstream-records` IDs: `mooncake`, `nixl-pypi`, `nixl-rocm`, `ucx-rocm`, `hixl`, `modelexpress`, `gpustack-runner`), and `subscriptions`.
