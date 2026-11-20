# Disaggregated Inference Routers Auto-Sync (`router-sync`)

You maintain the disaggregated inference router build recipe, local patches, and default router image settings for `gpustack-operator`:

- `llm-d/llm-d-router` -> `ARG LLMD_ROUTER_VERSION` -> `epp-builder` stage in `pack/llm-router/Dockerfile` (`pack/llm-router/patches/llm-d/*.patch`)
- `vllm-project/router` -> `ARG VLLM_ROUTER_VERSION` -> `vllm-router-builder` stage in `pack/llm-router/Dockerfile` (`pack/llm-router/patches/vllm-router/*.patch`)
- `sgl-project/sglang` (filtered strictly to `gateway-v*` tags; never engine `v0.4.*` tags) -> `ARG SGLANG_GATEWAY_VERSION` -> `sglang-gateway-builder` stage in `pack/llm-router/Dockerfile` (`pack/llm-router/patches/sglang-gateway/*.patch`)
- `pkg/worker/settings/value.go` and `docs/reference/settings.md` (Stage B tag promotion only)

## Staged Preflight Artifacts (`.auto-sync/.stage/router/`)

Inside the offline `linux-bwrap` sandbox, host toolchains (`docker`, `go`, `cargo`, `hugo`) and
external host staging directories are NOT mounted. Always inspect `.auto-sync/.stage/router/` FIRST
(falling back to `$AUTOSYNC_STAGE_DIR` only if `.auto-sync/.stage/router/` is absent):

- `.auto-sync/.stage/router/router-preflight-report.json` — includes `stage`, `target_tags`,
  `retire_0001_epd_routing`, `retire_0004_discovery_readiness`, `toolchain` (e.g.
  `recommended_go_version_arg`), and `rebase` per-router status (`auto_rebased`, `retired_patches`,
  `rebased_before_conflict`, `conflicted_patch`, `conflicted_files`).
- `.auto-sync/.stage/router/upstream-records.json`
- `.auto-sync/.stage/router/image-manifests.json`
- `.auto-sync/.stage/router/rebased-patches/<subdir>/` (if present) — already-rebased `.patch`
  files for earlier patch(es) of a multi-patch router that cherry-picked cleanly before a later
  patch conflicted (`rebase[<uid>].rebased_before_conflict`).
- `.auto-sync/.stage/router/target-files/<subdir>/` — pre-conflict base files (i.e. `target_tag`
  plus any `rebased_before_conflict` patches already applied) for routers whose patches had
  conflicts during host 3-way cherry-pick.
- `.auto-sync/.stage/router/cherry-pick-working/<subdir>/` — working-tree files at the conflict
  stop (auto-merged hunks already applied; only conflicted files have `<<<<<<<` / `=======` /
  `>>>>>>>` markers).
- `.auto-sync/.stage/router/conflicts/<subdir>/<patch>.txt` — conflicted file paths and `git status`
  summary.

**Read-only stage rule:** `.auto-sync/.stage/**` is read-only host-staged context outside
`scope.writable`. **NEVER** edit, move, or delete any file under `.auto-sync/.stage/**` (doing so
fails `scope-guard`). Loopwright automatically strips `.auto-sync/.stage/**` when running
`validate_router.sh` (including `docker buildx` builder targets) on the host and when publishing the
Pull Request.

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

1. **Keep auto-rebased patches**: For any router with `rebase[<uid>].auto_rebased == true`, host preflight has already rebased `pack/llm-router/patches/<subdir>/` and verified `git apply --check` onto the target tag — keep those staged patches intact.
2. **Resolve conflicts via staged 3-way working tree**: For any router with `rebase[<uid>].auto_rebased == false`:
   - Read `.auto-sync/.stage/router/conflicts/<subdir>/*.txt`.
   - If `.auto-sync/.stage/router/rebased-patches/<subdir>/` is present, copy those already-rebased earlier `.patch` files into `pack/llm-router/patches/<subdir>/` as-is (`target-files/<subdir>/` already includes them so your diff below covers only the conflicted patch).
   - Copy `.auto-sync/.stage/router/cherry-pick-working/<subdir>/` to a scratch directory under `/tmp`, resolve all `<<<<<<<` / `=======` / `>>>>>>>` conflict markers (preserving all observer/dispatch/readiness semantics), and generate a unified diff against `.auto-sync/.stage/router/target-files/<subdir>/` (using `git diff --no-index --full-index` with `a/` and `b/` prefixes rewritten to repo-relative paths, or a scratch git repo initialized from `target-files/<subdir>/`).
   - Prepend the original patch's descriptive header (updating `AGAINST:` to the target tag) and write the updated `.patch` file under `pack/llm-router/patches/<subdir>/` (and rebase any remaining patches after `conflicted_patch` in order if a multi-patch series had further patches).
   - Verify `git apply --check` cleanly against a fresh copy of `target-files/<subdir>/` (with zero `.rej`).
3. **Update `ARG GO_VERSION` and `ARG RUST_VERSION` when needed**: Check `toolchain.recommended_go_version_arg` in `router-preflight-report.json` (e.g., `1.27-bookworm` when `llm-d-router` `go.mod` advances to `go 1.27.*`) and update `ARG GO_VERSION=...` in `pack/llm-router/Dockerfile`.
4. **Update router version ARGs** (`LLMD_ROUTER_VERSION`, `VLLM_ROUTER_VERSION`, `SGLANG_GATEWAY_VERSION`) in `pack/llm-router/Dockerfile` and any patch references/comments if patches were retired.
5. Never drop `struct ObserverRouter` in `sglang-gateway` or observer/Mooncake/discovery semantics across any router.

## 4. Proposal Artifact

Keep `.auto-sync/proposals/router.json` valid JSON with `schema_version: 1`, `stage`, `summary`, `versions` (matching `upstream-records` IDs: `llm-d-router`, `vllm-router`, `sglang-gateway`), and `subscriptions`.
