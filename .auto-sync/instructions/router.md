# Disaggregated Inference Routers Auto-Sync (`router-sync`)

You maintain the disaggregated inference router build recipe, local patches, and default router image settings for `gpustack-operator`:

- `llm-d/llm-d-router` -> `ARG LLMD_ROUTER_VERSION` -> `epp-builder` stage in `pack/llm-router/Dockerfile` (`pack/llm-router/patches/llm-d/*.patch`)
- `vllm-project/router` -> `ARG VLLM_ROUTER_VERSION` -> `vllm-router-builder` stage in `pack/llm-router/Dockerfile` (`pack/llm-router/patches/vllm-router/*.patch`)
- `sgl-project/sglang` (filtered strictly to `router-v*` tags; sparse paths `sgl-router` and `rust`) -> `ARG SGL_ROUTER_VERSION` -> `sgl-router-builder` stage in `pack/llm-router/Dockerfile` (`pack/llm-router/patches/sgl-router/*.patch`)
- `sgl-project/sglang` (`sglang-gateway`, **PERMANENTLY FROZEN** at `ARG SGLANG_GATEWAY_VERSION=gateway-v0.3.1` because upstream deleted `sgl-model-gateway` in PR `#50732`) -> NEVER modify `SGLANG_GATEWAY_VERSION`, `sglang-gateway-builder`, or anything under `pack/llm-router/patches/sglang-gateway/`
- `pkg/worker/settings/value.go` and `docs/reference/settings.md` (Stage B tag promotion only)

## Taskpack 2.0 Preflight Resources

Inside the offline `linux-bwrap` sandbox, host toolchains (`docker`, `go`, `cargo`, `hugo`) are
not mounted; preflight resources are mounted natively via Taskpack 2.0:

1. **Prompt summary (`mount: 'prompt'`)**: Read `.loopwright-preflight/router-preflight-summary.json` first.
2. **Read-only workspace resources (`mount: 'workspace'`)**: Inspect the `readOnlyPath` entries under `## Preflight Staged Workspace Resources` in your prompt:
   - `router-preflight-report.json` — includes `stage`, `target_tags`, `retire_0001_epd_routing`, `retire_0004_discovery_readiness`, `toolchain` (e.g. `recommended_go_version_arg`), and `rebase` per-router status (`auto_rebased`, `retired_patches`, `rebased_before_conflict`, `conflicted_patch`, `conflicted_files`).
   - `image-manifests.json` — multi-arch manifest probe digests.
   - `candidate-overlay.tgz` (and optional `candidate-deleted-files.txt`) — present when auto-rebased patches or Stage B promoted settings/tests/docs were staged (`has_candidate_overlay: true`). Apply into the workspace root first:
     ```sh
     tar -xzf "<readOnlyPath of candidate-overlay.tgz>" -C .
     # If candidate-deleted-files.txt is also listed (e.g. retired vllm-router patches):
     xargs -r rm -f < "<readOnlyPath of candidate-deleted-files.txt>"
     ```
   - `router-stage-bundle.tgz` — present when any router patch encountered a 3-way cherry-pick conflict (`has_stage_bundle: true`), containing `rebased-patches/<subdir>/`, `target-files/<subdir>/`, `cherry-pick-working/<subdir>/`, and `conflicts/<subdir>/<patch>.txt`. Extract into `/tmp/router-stage` (NEVER inside the repository root):
     ```sh
     mkdir -p /tmp/router-stage && tar -xzf "<readOnlyPath of router-stage-bundle.tgz>" -C /tmp/router-stage
     ```

## 1. Two-Stage Image Publication Gate

Never reference an unpublished `gpustack/*` container image tag in `pkg/worker/settings/value.go` or `docs/reference/settings.md`.

Stage B (`tag_promotion`) takes precedence over Stage A (`recipe_sync`): once un-held `gpustack/*` Base Images are published on Docker Hub (or explicitly targeted via `target_tag`), host preflight selects `stage: "tag_promotion"` first without being starved by newer upstream router releases. Any concurrent upstream release drift is recorded in `has_stage_a` / `stage_a_drifts` in `router-preflight-report.json` and runs in a follow-up Stage A PR after Stage B merges, while `.auto-sync/setting-image-holds.json` continues to hold specific setting images.

- **Stage A (`stage: "recipe_sync"`, `image_published: false`)**:
  - Update ONLY `pack/llm-router/Dockerfile`, `pack/llm-router/patches/**`, and `.auto-sync/proposals/router.json` (`"stage": "recipe_sync"`).
  - Leave `pkg/worker/settings/value.go` and `docs/reference/settings.md` untouched.
- **Stage B (`stage: "tag_promotion"`, `image_published: true`)**:
  - Host preflight pre-stages promoted image updates into `pkg/worker/settings/value.go`, `pkg/worker/settings/value_test.go`, `pkg/worker/controllers/worker/model_deployment_router_test.go`, `pkg/worker/controllers/worker/model_deployment_render_test.go`, and `docs/reference/settings.md`; verify them against `router-preflight-report.json` and `image-manifests.json`.
  - Leave `pack/llm-router/Dockerfile` and `pack/llm-router/patches/**` untouched during Stage B even when `has_stage_a: true` / `stage_a_drifts` indicates concurrent upstream router release drift.
  - Setting-image holds in `.auto-sync/setting-image-holds.json` (outside `scope.writable`, keys `router`, `proxy`, `sidecar`) keep held components pinned to their existing defaults even when newer images are published on Docker Hub; never promote or modify a held setting image (check `held_setting_images` in `router-preflight-report.json`).
  - Update `.auto-sync/proposals/router.json`: set `"stage": "tag_promotion"`, and whenever `upstream-records.json` carries `<ver>+stage-b` for an `upstreamId`, write that exact `<ver>+stage-b` string into `versions[<upstreamId>]`.
  - Do NOT modify any other settings in `value.go`, `value_test.go`, or `settings.md` (such as `KVCacheBackendImage` or `ModelDeploymentModelExpressServerImage`).
- **No Change (`stage: "no_change"`)**:
  - When all three router upstreams are `up_to_date` and no promoted image tag is ready, settle cleanly as `no_change`.

## 2. Patch Retirement Rules (`0001-epd-routing.patch` and `0004-discovery-readiness-and-deselect-removal.patch`)

- **Check `retire_0001_epd_routing`** in `router-preflight-report.json`:
  - When `false`: keep `pack/llm-router/patches/vllm-router/0001-epd-routing.patch`.
  - When `true` and bumping `VLLM_ROUTER_VERSION`: delete `0001-epd-routing.patch`.
- **Check `retire_0004_discovery_readiness`** in `router-preflight-report.json`:
  - When `true` and bumping `VLLM_ROUTER_VERSION` to `target_tags["vllm-router"]`: delete `pack/llm-router/patches/vllm-router/0004-discovery-readiness-and-deselect-removal.patch`.

## 3. Rebase & Toolchain Workflow Inside `bwrap`

1. **Keep auto-rebased patches**: For any router with `rebase[<uid>].auto_rebased == true`, host preflight has already rebased `pack/llm-router/patches/<subdir>/` (in `candidate-overlay.tgz`) and verified `git apply --check` onto the target tag — keep those extracted patches intact.
2. **Resolve conflicts via staged 3-way working tree**: For any router with `rebase[<uid>].auto_rebased == false` (after unpacking `router-stage-bundle.tgz` into `/tmp/router-stage`):
   - Read `/tmp/router-stage/conflicts/<subdir>/*.txt`.
   - If `/tmp/router-stage/rebased-patches/<subdir>/` is present, copy those already-rebased earlier `.patch` files into `pack/llm-router/patches/<subdir>/` as-is (`target-files/<subdir>/` already includes them so your diff below covers only the conflicted patch).
   - Copy `/tmp/router-stage/cherry-pick-working/<subdir>/` to a scratch directory under `/tmp`, resolve all `<<<<<<<` / `=======` / `>>>>>>>` conflict markers (preserving all observer/dispatch/readiness semantics), and generate a unified diff against `/tmp/router-stage/target-files/<subdir>/` (using `git diff --no-index --full-index` with `a/` and `b/` prefixes rewritten to repo-relative paths, or a scratch git repo initialized from `/tmp/router-stage/target-files/<subdir>/`).
   - Prepend the original patch's descriptive header (updating `AGAINST:` to the target tag) and write the updated `.patch` file under `pack/llm-router/patches/<subdir>/` (and rebase any remaining patches after `conflicted_patch` in order if a multi-patch series had further patches).
   - Verify `git apply --check` cleanly against a fresh copy of `/tmp/router-stage/target-files/<subdir>/` (with zero `.rej`).
3. **Update `ARG GO_VERSION` and `ARG RUST_VERSION` when needed**: Check `toolchain.recommended_go_version_arg` in `router-preflight-report.json` (e.g., `1.27-bookworm` when `llm-d-router` `go.mod` advances to `go 1.27.*`) and update `ARG GO_VERSION=...` in `pack/llm-router/Dockerfile`.
4. **Update router version ARGs** (`LLMD_ROUTER_VERSION`, `VLLM_ROUTER_VERSION`, `SGL_ROUTER_VERSION`) in `pack/llm-router/Dockerfile` and any patch references/comments if patches were retired. NEVER modify frozen `SGLANG_GATEWAY_VERSION`, `sglang-gateway-builder`, or `pack/llm-router/patches/sglang-gateway/**` (`validate_router.sh` fails closed on any change).
5. Never drop `struct ObserverRouter` in `sgl-router` or `sglang-gateway`, or observer/Mooncake/discovery semantics across any router.
6. **Verify `llm-d-router` observer system-default plugin coverage**: When rebasing `pack/llm-router/patches/llm-d/0001-observer-endpoints-and-dispatch.patch` onto a newer `llm-d-router` tag, inspect `pkg/epp/config/loader/defaults.go` and `cmd/epp/runner/runner.go` for newly added system-default plugins (for example, `strict-band-selection-policy` added by `ensureFlowControlLayer` in `v1.0.0`) and any signature changes to `parseConfigurationPhaseTwo`. Update `SupportedPlugins` in `pkg/epp/observer/endpoints.go` and `cmd/epp/runner/observer_test.go` in the same patch so `epp-builder`'s `go test ./cmd/epp/runner -run TestObserverSupportsRenderedOperatorProfiles` and `go test ./pkg/epp/observer/...` build gates pass.

## 4. Proposal Artifact & `Base Image` Build Recommendations

Keep `.auto-sync/proposals/router.json` valid JSON with `schema_version: 1`, `stage`, `summary`, `versions` (matching `upstream-records` IDs: `llm-d-router`, `vllm-router`, `sglang-gateway`, `sgl-router`), `subscriptions`, and `base_image_builds` (JSON array):
- Whenever Stage A modifies `pack/llm-router/Dockerfile` or `pack/llm-router/patches/**`, populate `base_image_builds` (matching `base_image_builds` in `router-preflight-report.json` / `router-preflight-summary.json`) with the required `gpustack/llm-router` entry containing `repository`, `target` (`""`), `recommended_tag`, `reason`, and the exact `gh workflow run "Base Image" -f repository=llm-router -f tag=<recommended_tag>` `command`, and mention the dispatch command in `summary`.
- When no `pack/llm-router/**` files change (Stage B `tag_promotion` or `aligned`), set `"base_image_builds": []`.
