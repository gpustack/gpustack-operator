# KV-Cache and Weight Transfer Auto-Sync (`kv-transfer-sync`)

You maintain the KV-cache and remote-weight transfer recipes and default image tags for `gpustack-operator`:
- `pack/mirrored-mooncake/Dockerfile` (`kvcache-ai/Mooncake`)
- `pack/model-transfer/Dockerfile` and `pack/model-transfer/patches/hixl/001_channel_generation.patch` (`ai-dynamo/nixl`, `openucx/ucx`, `hicann/hixl`, `gpustack/runner` parity)
- `pack/mirrored-modelexpress-server/Dockerfile` (`ai-dynamo/modelexpress`)
- `pkg/worker/settings/value.go`, `docs/reference/settings.md`, `docs/modules/model-delivery/remote.md`, and `deploy/gpustack-operator/chart/values.yaml` (Stage B tag promotion only)

## Staged Preflight Artifacts (`.auto-sync/.stage/kv-transfer/`)

Inside the offline `linux-bwrap` sandbox, host toolchains (`go`, `hugo`, `helm-docs`, `helm-schema`,
`docker`) and external host staging directories are NOT mounted. Always inspect
`.auto-sync/.stage/kv-transfer/` FIRST (falling back to `$AUTOSYNC_STAGE_DIR` only if
`.auto-sync/.stage/kv-transfer/` is absent):

- `.auto-sync/.stage/kv-transfer/kv-transfer-preflight-report.json`
- `.auto-sync/.stage/kv-transfer/upstream-records.json`
- `.auto-sync/.stage/kv-transfer/image-manifests.json`
- `.auto-sync/.stage/kv-transfer/runner-001_channel_generation.patch` (when staged from `gpustack/runner`)
- `.auto-sync/.stage/kv-transfer/mooncake/` (upstream `CMakeLists.txt`, `scripts/build_wheel.sh`, `mooncake-transfer-engine/example/CMakeLists.txt`, and `mooncake-transfer-engine/src/transport/ascend_transport/` when `mooncake` drifted)
- `.auto-sync/.stage/kv-transfer/nixl-rocm/` (upstream `pyproject.toml` at the resolved `vX.Y.Z` tag)
- `.auto-sync/.stage/kv-transfer/modelexpress/` (upstream `Cargo.toml` and `Cargo.lock` at the resolved `vX.Y.Z` tag)

**Read-only stage rule:** `.auto-sync/.stage/**` is read-only host-staged context outside
`scope.writable`. **NEVER** edit, move, or delete any file under `.auto-sync/.stage/**` (doing so
fails `scope-guard`). Loopwright automatically strips `.auto-sync/.stage/**` when running
`validate_kv_transfer.sh` on the host and when publishing the Pull Request.

## 1. Two-Stage Image Publication Gate

Never reference an unpublished `gpustack/*` container image tag in `pkg/worker/settings/value.go`, `docs/reference/settings.md`, `docs/modules/model-delivery/remote.md`, or `deploy/gpustack-operator/chart/values.yaml`.

Stage B (`tag_promotion`) takes precedence over Stage A (`recipe_sync`): once un-held `gpustack/*` Base Images are published on Docker Hub (or explicitly targeted via `target_tag`), host preflight selects `stage: "tag_promotion"` first without being starved by newer upstream recipe commits/releases. Any concurrent upstream recipe drift is recorded in `has_stage_a` / `stage_a_drifts` in `kv-transfer-preflight-report.json` and runs in a follow-up Stage A PR after Stage B merges, while `.auto-sync/setting-image-holds.json` continues to hold specific setting images.

- **Stage A (`stage: "recipe_sync"`, `image_published: false`)**:
  - When upstream recipes advance and no un-held target `gpustack/*` image tag is published on Docker Hub yet, update ONLY:
    - `pack/mirrored-mooncake/**`
    - `pack/model-transfer/**`
    - `pack/mirrored-modelexpress-server/**`
    - `.auto-sync/proposals/kv-transfer.json` (`"stage": "recipe_sync"`)
  - Leave `pkg/worker/settings/value.go`, `docs/reference/settings.md`, `docs/modules/model-delivery/remote.md`, and `deploy/gpustack-operator/chart/**` untouched.
- **Stage B (`stage: "tag_promotion"`, `image_published: true`)**:
  - Host preflight pre-stages promoted tags into `pkg/worker/settings/value.go`, `pkg/worker/settings/value_test.go`, `pkg/worker/controllers/worker/model_deployment_transfer_test.go`, `docs/reference/settings.md`, `docs/modules/model-delivery/remote.md`, and `deploy/gpustack-operator/chart/{values.yaml,values.schema.json,README.md.gotmpl,README.md}` when `stage: "tag_promotion"` is ready; verify them against `kv-transfer-preflight-report.json` and `image-manifests.json` (confirming `crane manifest` digests exist for the promoted tags).
  - Leave `pack/mirrored-mooncake/**`, `pack/model-transfer/**`, and `pack/mirrored-modelexpress-server/**` untouched during Stage B even when `has_stage_a: true` / `stage_a_drifts` indicates concurrent upstream recipe drift.
  - Setting-image holds in `.auto-sync/setting-image-holds.json` (outside `scope.writable`, keys `mooncake`, `modelexpress`, `model-transfer`, `model-transfer.nvidia`, `model-transfer.amd`, `model-transfer.ascend`) keep held components pinned to their existing defaults even when newer images are published on Docker Hub; never promote or modify a held setting image (check `held_setting_images` in `kv-transfer-preflight-report.json`).
  - Update `.auto-sync/proposals/kv-transfer.json`: set `"stage": "tag_promotion"`, and whenever `upstream-records.json` carries `<ver>+stage-b` for an `upstreamId` (`mooncake`, `modelexpress`, or `gpustack-runner`), write that exact `<ver>+stage-b` string into `versions[<upstreamId>]` so open-PR deduplication settles cleanly on subsequent runs.
  - Do NOT modify any other settings in `value.go`, `value_test.go`, or `settings.md` (such as `ModelDeploymentRouterImage`, `ModelDeploymentRouterProxyImage`, or `ModelDeploymentRoutingSidecarImage`) or any other keys in `deploy/gpustack-operator/chart/values.yaml`.

## 2. Five-Block `MOONCAKE_VERSION` `case` Audit

`pack/mirrored-mooncake/Dockerfile` contains four stage release guards (`builder`, `cuda`, `cann`, `rocm`) plus a fifth `context_manager.cpp` conditional patch block in the `cann` stage:
1. Before changing `ARG MOONCAKE_VERSION="..."`, inspect the staged upstream Mooncake files under `.auto-sync/.stage/kv-transfer/mooncake/` to verify whether the existing CMake/wheel/wrapper steps and the `ascend_direct_transport/context_manager.cpp` glob fix still apply or need adjustment.
2. `validate_kv_transfer.sh`'s `parse_case_arms` is a line-oriented static parser that requires `pattern1|pattern2)` and `;;` in standard `case "${MOONCAKE_VERSION}" in` format, and requires an explicit version arm (e.g. `0.3.14*`) in all 4 release-guard blocks AND the 5th `context_manager.cpp` block.
3. When Mooncake `0.3.14+` wraps `file(GLOB ASCEND_SOURCES ...)` onto multiple lines (`"ubshmem_transport/*.cpp" "*.cpp"` followed by `"ascend_direct_transport/utils.cpp"`) while `ascend_allocator.cpp` still references `ContextManager::getInstance()`, add a separate `0.3.14*)` arm in the `context_manager.cpp` `case` block that patches `"ascend_direct_transport/context_manager.cpp"` alongside `"ascend_direct_transport/utils.cpp"` and verifies with `grep -q 'ascend_direct_transport/context_manager\.cpp'`.

## 3. `gpustack/runner` HIXL Parity & ROCm Release-Tag Pinning (`NIXL_ROCM_*`)

- When `runner.hixl_commit` is non-empty in `kv-transfer-preflight-report.json`, `HIXL_COMMIT` in `pack/model-transfer/Dockerfile` MUST match `runner.hixl_commit` (not `hicann/hixl` HEAD if `gpustack/runner` has not bumped `VLLM_HIXL_COMMIT`), and `pack/model-transfer/patches/hixl/001_channel_generation.patch` MUST stay byte-identical to `runner-001_channel_generation.patch` (pre-copied by host preflight when drifted).
- `NIXL_ROCM_REF` MUST be a stable non-prerelease SemVer release tag `vX.Y.Z` from `ai-dynamo/nixl` (`allowPrerelease: false`, never a raw commit SHA or `-rc*` prerelease) aligned to CUDA `NIXL_VERSION`: `NIXL_ROCM_VERSION == NIXL_VERSION` and `NIXL_ROCM_REF == "v${NIXL_ROCM_VERSION}"`. Also verify `NIXL_ROCM_VERSION` matches `nixl_rocm.project_version` recorded in `kv-transfer-preflight-report.json` (parsed from `pyproject.toml` `[project].version` at `refs/tags/${NIXL_ROCM_REF}`).
- `NIXL_ROCM_UCX_REF` MUST be a stable non-prerelease SemVer release tag `vX.Y.Z` from `openucx/ucx` (`allowPrerelease: false`, sorted by numeric SemVer tuple so older-branch maintenance releases never downgrade a newer minor release, and never a raw commit SHA or `-rc*` prerelease).

## 4. PyPI Wheel Parity for `NIXL_VERSION`

Only bump `ARG NIXL_VERSION="..."` (together with `ARG NIXL_ROCM_VERSION="..."` and `ARG NIXL_ROCM_REF="v..."`) in `pack/model-transfer/Dockerfile` to a stable version where ALL THREE PyPI packages (`nixl`, `nixl-cu12`, and `nixl-cu13`) publish Linux `x86_64` and `aarch64` wheels AND `ai-dynamo/nixl` has published the corresponding non-prerelease `vX.Y.Z` GitHub release (using `nixl_pypi.selected_version` and `nixl_rocm.target_ref` in `kv-transfer-preflight-report.json`).

## 5. ModelExpress SemVer Tag & `--locked` Cargo Verification

- Set `ARG MODELEXPRESS_VERSION=vX.Y.Z` in `pack/mirrored-modelexpress-server/Dockerfile` to a stable SemVer release tag `vX.Y.Z` from `ai-dynamo/modelexpress` (never a commit SHA, pre-release, or non-SemVer tag like `weight-sync-pre-removal`).
- When `.auto-sync/.stage/kv-transfer/modelexpress/Cargo.toml` is staged, verify `MODELEXPRESS_VERSION` (without the leading `v`) matches `[workspace.package].version` in `Cargo.toml`.
- When bumping `ARG MODELEXPRESS_VERSION=vX.Y.Z` in `pack/mirrored-modelexpress-server/Dockerfile`, full `docker buildx` `--target builder` (`--locked` Cargo) verification runs on the host in `validate_kv_transfer.sh` after you submit.

## 6. Proposal Artifact

Keep `.auto-sync/proposals/kv-transfer.json` valid JSON with `schema_version: 1`, `stage`, `summary`, `versions` (matching `upstream-records` IDs: `mooncake`, `nixl-pypi`, `nixl-rocm`, `ucx-rocm`, `hixl`, `modelexpress`, `gpustack-runner`), and `subscriptions`.
