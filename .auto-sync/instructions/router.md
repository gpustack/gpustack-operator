# Disaggregated Inference Routers Auto-Sync (`router-sync`)

You maintain the disaggregated inference router build recipe, local patches, and default router image settings for `gpustack-operator`:

- `llm-d/llm-d-router` -> paired `ARG LLMD_ROUTER_VERSION` + optional `ARG LLMD_ROUTER_COMMIT` -> `epp-builder` stage in `pack/llm-router/Dockerfile` (`pack/llm-router/patches/llm-d/*.patch`)
- `vllm-project/router` -> paired `ARG VLLM_ROUTER_VERSION` + optional `ARG VLLM_ROUTER_COMMIT` -> `vllm-router-builder` stage in `pack/llm-router/Dockerfile` (`pack/llm-router/patches/vllm-router/*.patch`)
- `sgl-project/sglang` (filtered strictly to `router-v*` tags; sparse paths `sgl-router` and `rust`) -> paired `ARG SGL_ROUTER_VERSION` + optional `ARG SGL_ROUTER_COMMIT` -> `sgl-router-builder` stage in `pack/llm-router/Dockerfile` (`pack/llm-router/patches/sgl-router/*.patch`)
- `sgl-project/sglang` (`sglang-gateway`, **PERMANENTLY FROZEN** at `ARG SGLANG_GATEWAY_VERSION=gateway-v0.3.1` because upstream deleted `sgl-model-gateway` in PR `#50732`) -> NEVER modify `SGLANG_GATEWAY_VERSION`, `sglang-gateway-builder`, or anything under `pack/llm-router/patches/sglang-gateway/`
- `pkg/worker/settings/value.go` and `docs/reference/settings.md` (Stage B tag promotion only)

## Taskpack 2.0 Preflight Resources

Inside the offline `linux-bwrap` sandbox, host toolchains (`docker`, `go`, `cargo`, `hugo`) are
not mounted; preflight resources are mounted natively via Taskpack 2.0:

1. **Prompt summary (`mount: 'prompt'`)**: Read `.loopwright-preflight/router-preflight-summary.json` first (including `highlights.graduation`, `highlights.base_image_builds`, and `highlights.sync_images`).
2. **Read-only workspace resources (`mount: 'workspace'`)**: Inspect the `readOnlyPath` entries under `## Preflight Staged Workspace Resources` in your prompt:
   - `router-preflight-report.json` — includes `stage`, `recipe`, `recipe_commits`, `target_tags`, `graduation` (per commit-pinned router `pinned_version`, `pinned_commit`, `latest_tag`, `is_ancestor`, `graduation_status`), `sync_images`, `base_image_builds`, `retire_0001_epd_routing`, `retire_0004_discovery_readiness`, `toolchain` (e.g. `recommended_go_version_arg`), and `rebase` per-router status (`auto_rebased`, `retired_patches`, `rebased_before_conflict`, `conflicted_patch`, `conflicted_files`).
   - `image-manifests.json` — multi-arch manifest probe digests.
   - `candidate-overlay.tgz` (and optional `candidate-deleted-files.txt`) — present when auto-rebased patches, graduated `pack/llm-router/Dockerfile` pin updates, or Stage B promoted settings/tests/docs were staged (`has_candidate_overlay: true`). Apply into the workspace root first:
     ```sh
     tar -xzf "<readOnlyPath of candidate-overlay.tgz>" -C .
     # If candidate-deleted-files.txt is also listed (e.g. retired vllm-router patches):
     xargs -r rm -f < "<readOnlyPath of candidate-deleted-files.txt>"
     ```
   - `router-stage-bundle.tgz` — present when any router patch encountered a 3-way cherry-pick conflict (`has_stage_bundle: true`), containing `rebased-patches/<subdir>/`, `target-files/<subdir>/`, `cherry-pick-working/<subdir>/`, and `conflicts/<subdir>/<patch>.txt`. Extract into `/tmp/router-stage` (NEVER inside the repository root):
     ```sh
     mkdir -p /tmp/router-stage && tar -xzf "<readOnlyPath of router-stage-bundle.tgz>" -C /tmp/router-stage
     ```

## 1. Two-Stage Image Publication Gate & Commit-to-Tag Graduation

Never reference an unpublished `gpustack/*` container image tag in `pkg/worker/settings/value.go` or `docs/reference/settings.md`.

Stage B (`tag_promotion`) takes precedence over Stage A (`recipe_sync`): once un-held `gpustack/*` Base Images are published on Docker Hub (or explicitly targeted via `target_tag`), host preflight selects `stage: "tag_promotion"` first without being starved by newer upstream router releases. Any concurrent upstream release drift is recorded in `has_stage_a` / `stage_a_drifts` in `router-preflight-report.json` and runs in a follow-up Stage A PR after Stage B merges, while `.auto-sync/setting-image-holds.json` continues to hold specific setting images.

- **Ancestry-gated commit-to-tag graduation (`graduation` in `router-preflight-report.json`):**
  - When a router has a non-empty `*_COMMIT` (`LLMD_ROUTER_COMMIT`, `VLLM_ROUTER_COMMIT`, or `SGL_ROUTER_COMMIT`), host preflight fetches full commit ancestry (`--filter=blob:none`, no `--depth 1`) and checks `git merge-base --is-ancestor "${PINNED_COMMIT}" "refs/tags/${T}"` against the latest eligible stable tag `T` (excluding prereleases).
  - **`graduation_status: "awaiting_tag_containing_commit"` (`is_ancestor: false`)**: `T` does **not** contain `PINNED_COMMIT` (for example, a patch tag cut off an older release branch). Host preflight marks the router `held_back` in `upstream-records.json`. NEVER clear or modify `*_COMMIT` or downgrade the router to `T` — `validate_router.sh` fails closed if a non-empty `*_COMMIT` is cleared or changed without preflight ancestry proof.
  - **`graduation_status: "ready_to_graduate"` (`is_ancestor: true`)**: `T` contains `PINNED_COMMIT`. Host preflight triggers Stage A graduation, pre-stages `*_VERSION=${T}` and `*_COMMIT=""` in `pack/llm-router/Dockerfile`, and rebases patches from the effective baseline `PINNED_COMMIT` (not `refs/tags/<old>`) onto `refs/tags/${T}`.
  - **`versions[uid]` formatting in `.auto-sync/proposals/router.json`**:
    - Use `<ver>+<short7>` while `*_COMMIT` is non-empty (e.g., `v1.0.0+3283d44`).
    - Use `<ver>` when tag-pinned or after graduating (`*_COMMIT=""`).
    - Use `<ver>+stage-b` in Stage B (`tag_promotion`) for promoted upstreams matching `upstream-records.json`.
- **Stage A (`stage: "recipe_sync"`, `image_published: false`)**:
  - Update ONLY `pack/llm-router/Dockerfile`, `pack/llm-router/patches/**`, and `.auto-sync/proposals/router.json` (`"stage": "recipe_sync"`).
  - Leave `pkg/worker/settings/value.go` and `docs/reference/settings.md` untouched.
- **Stage B (`stage: "tag_promotion"`, `image_published: true`)**:
  - Host preflight pre-stages promoted image updates into `pkg/worker/settings/value.go`, `pkg/worker/settings/value_test.go`, `pkg/worker/controllers/worker/model_deployment_router_test.go`, `pkg/worker/controllers/worker/model_deployment_render_test.go`, and `docs/reference/settings.md`; verify them against `router-preflight-report.json` and `image-manifests.json`.
  - Leave `pack/llm-router/Dockerfile` and `pack/llm-router/patches/**` untouched during Stage B even when `has_stage_a: true` / `stage_a_drifts` indicates concurrent upstream router release drift.
  - Setting-image holds in `.auto-sync/setting-image-holds.json` (outside `scope.writable`, keys `router`, `proxy`, `sidecar`) keep held components pinned to their existing defaults even when newer images are published on Docker Hub; never promote or modify a held setting image (check `held_setting_images` in `router-preflight-report.json`).
  - Update `.auto-sync/proposals/router.json`: set `"stage": "tag_promotion"`, and whenever `upstream-records.json` carries `<ver>+stage-b` for an `upstreamId`, write that exact `<ver>+stage-b` string into `versions[<upstreamId>]` (non-promoted commit-pinned routers retain `<ver>+<short7>`).
  - Do NOT modify any other settings in `value.go`, `value_test.go`, or `settings.md` (such as `KVCacheBackendImage` or `ModelDeploymentModelExpressServerImage`).
- **No Change (`stage: "no_change"`)**:
  - When all active router upstreams are `up_to_date` or `held_back` (awaiting a release tag containing their pinned commit) and no promoted image tag is ready, settle cleanly as `no_change`.

## 2. Patch Retirement Rules (`vllm-router`)

- Historical patches `0001-epd-routing.patch`, `0004-discovery-readiness-and-deselect-removal.patch`, and `0005-kv-index-discover-metadata-from-metrics.patch` (merged upstream in `vllm-project/router#520`) have already been retired from `pack/llm-router/patches/vllm-router/`; only `0002-mooncake-lazy-bootstrap-lookup.patch` and `0003-observer-read-only-endpoint-view.patch` remain active.
- If a baseline still carries either legacy patch, obey `retire_0001_epd_routing` and `retire_0004_discovery_readiness` in `router-preflight-report.json`: delete the corresponding patch when its flag is `true` on `target_tags["vllm-router"]`, and keep it when `false`.

## 3. Rebase & Toolchain Workflow Inside `bwrap`

1. **Keep auto-rebased patches and graduated Dockerfile pins**: For any router with `rebase[<uid>].auto_rebased == true` (or `graduation[<uid>].graduation_status == "ready_to_graduate"`), host preflight has already rebased `pack/llm-router/patches/<subdir>/` from the effective baseline (`PINNED_COMMIT` when commit-pinned, else `refs/tags/<old>`) onto `refs/tags/<target>`, pre-staged `*_VERSION=<target>` and `*_COMMIT=""` in `pack/llm-router/Dockerfile` for graduating routers inside `candidate-overlay.tgz`, and verified `git apply --check` onto the target tag — keep those extracted files intact.
2. **Resolve conflicts via staged 3-way working tree**: For any router with `rebase[<uid>].auto_rebased == false` (after unpacking `router-stage-bundle.tgz` into `/tmp/router-stage`):
   - Read `/tmp/router-stage/conflicts/<subdir>/*.txt`.
   - If `/tmp/router-stage/rebased-patches/<subdir>/` is present, copy those already-rebased earlier `.patch` files into `pack/llm-router/patches/<subdir>/` as-is (`target-files/<subdir>/` already includes them so your diff below covers only the conflicted patch).
   - Copy `/tmp/router-stage/cherry-pick-working/<subdir>/` to a scratch directory under `/tmp`, resolve all `<<<<<<<` / `=======` / `>>>>>>>` conflict markers (preserving all observer/dispatch/readiness semantics), and generate a unified diff against `/tmp/router-stage/target-files/<subdir>/` (using `git diff --no-index --full-index` with `a/` and `b/` prefixes rewritten to repo-relative paths, or a scratch git repo initialized from `/tmp/router-stage/target-files/<subdir>/`).
   - Prepend the original patch's descriptive header (updating `AGAINST:` to the target tag) and write the updated `.patch` file under `pack/llm-router/patches/<subdir>/` (and rebase any remaining patches after `conflicted_patch` in order if a multi-patch series had further patches).
   - Verify `git apply --check` cleanly against a fresh copy of `/tmp/router-stage/target-files/<subdir>/` (with zero `.rej`).
3. **Update `ARG GO_VERSION` and `ARG RUST_VERSION` when needed**: Check `toolchain.recommended_go_version_arg` in `router-preflight-report.json` (e.g., `1.27-bookworm` when `llm-d-router` `go.mod` advances to `go 1.27.*`) and update `ARG GO_VERSION=...` in `pack/llm-router/Dockerfile`.
4. **Update router version/commit ARGs** (`LLMD_ROUTER_VERSION`/`LLMD_ROUTER_COMMIT`, `VLLM_ROUTER_VERSION`/`VLLM_ROUTER_COMMIT`, `SGL_ROUTER_VERSION`/`SGL_ROUTER_COMMIT`) in `pack/llm-router/Dockerfile` and any patch references/comments if patches were retired. Only clear `*_COMMIT=""` when `graduation[<uid>].graduation_status == "ready_to_graduate"` (`is_ancestor: true`). NEVER modify frozen `SGLANG_GATEWAY_VERSION`, `sglang-gateway-builder`, or `pack/llm-router/patches/sglang-gateway/**` (`validate_router.sh` fails closed on any change).
5. Never drop `struct ObserverRouter` in `sgl-router` or `sglang-gateway`, or observer/Mooncake/discovery semantics across any router.
6. **Verify `llm-d-router` observer system-default plugin coverage**: When rebasing `pack/llm-router/patches/llm-d/0001-observer-endpoints-and-dispatch.patch` onto a newer `llm-d-router` tag, inspect `pkg/epp/config/loader/defaults.go` and `cmd/epp/runner/runner.go` for newly added or renamed system-default plugins (including `systemone-parser`, `endpoint-health-filter`, `datalayer-poll-status`, `static-usage-limit-policy`, and `strict-band-selection-policy`) and any signature changes to `parseConfigurationPhaseTwo`. Update `SupportedPlugins` in `pkg/epp/observer/endpoints.go` and `cmd/epp/runner/observer_test.go` in the same patch so `epp-builder`'s two in-Dockerfile Go test gates (`go test ./cmd/epp/runner -run TestObserverSupportsRenderedOperatorProfiles` and `go test ./pkg/epp/observer/...`) pass; rendered operator `epp.yaml` configs can also be verified offline with `epp --validate-config --allow-experimental-plugins`.

## 4. Proposal Artifact, `Base Image` Builds, and `Sync Image` Recommendations

Keep `.auto-sync/proposals/router.json` valid JSON with `schema_version: 1`, `stage`, `summary`, `versions` (matching `upstream-records` IDs: `llm-d-router`, `vllm-router`, `sglang-gateway`, `sgl-router`, using `<ver>+<short7>` while `*_COMMIT` is non-empty, `<ver>` when tag-pinned/graduated, and `<ver>+stage-b` in Stage B), `subscriptions`, `base_image_builds` (JSON array), and `sync_images` (JSON array):
- Whenever Stage A modifies `pack/llm-router/Dockerfile` or `pack/llm-router/patches/**`, populate `base_image_builds` (matching `base_image_builds` in `router-preflight-report.json` / `router-preflight-summary.json`) with the required `gpustack/llm-router` entry containing `repository`, `target` (`""`), `recommended_tag`, `reason`, and the exact `gh workflow run "Base Image" -f repository=llm-router -f tag=<recommended_tag>` `command`, and mention the dispatch command in `summary`.
- When no `pack/llm-router/**` files change (Stage B `tag_promotion` or `aligned`), set `"base_image_builds": []`.
- Whenever `router-preflight-report.json` / `router-preflight-summary.json` emits a non-empty `sync_images` list (an un-held stable `llm-d-router` tag newer than `ModelDeploymentRoutingSidecarImage` whose `gpustack/mirrored-llm-d-router-disagg-sidecar:<tag>` mirror is missing or not multi-arch on Docker Hub), copy every entry into `sync_images` with `source` (`ghcr.io/llm-d/llm-d-router-disagg-sidecar:<tag>`), `destination` (`gpustack/mirrored-llm-d-router-disagg-sidecar:<tag>`), `reason`, and the exact `command` (`gh workflow run "Sync Image" -f images="ghcr.io/llm-d/llm-d-router-disagg-sidecar:<tag>"`), and mention the `Sync Image` dispatch command in `summary`.
- When no sidecar mirror sync is needed (already published multi-arch, held in `.auto-sync/setting-image-holds.json`, or not newer than the default sidecar tag), set `"sync_images": []`.
