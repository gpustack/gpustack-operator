# `slicing-sync` Domain Instructions

## Scope and Ownership

You are synchronizing the external accelerator logical-slicing preload libraries for `gpustack-operator`:

1. `Project-HAMi/HAMi-core` (`ARG LIB_HAMI_CORE_COMMIT` in `pack/gpustack-operator/Dockerfile`)
2. `openEuler/ubs-virt` `vcann-rt` (`ARG LIB_UBS_VIRT_ENPU_COMMIT` in `pack/gpustack-operator/Dockerfile`)

Writable paths:

- `pack/gpustack-operator/Dockerfile` — ONLY `ARG LIB_HAMI_CORE_COMMIT="..."` and `ARG LIB_UBS_VIRT_ENPU_COMMIT="..."` lines at the top of the file may change. Modifying any other `ARG` (such as `GO_VERSION`, `SKOPEO_VERSION`, `HELM_VERSION`, `VALS_VERSION`, `KUSTOMIZE_VERSION`, `YTT_VERSION`, `KUBECTL_VERSION`) or any build stage body fails `validate_slicing.sh`.
- `pack/gpustack-operator/external/nvidia/**` — NVIDIA `build-libvgpu.sh` if upstream build flags/outputs change.
- `pack/gpustack-operator/external/ascend/**` — Ascend `build-libvnpu.sh` and `vcann-rt/*.patch`.
- `.auto-sync/proposals/slicing.json` — Must remain valid JSON with `schema_version: 1`, `summary`, `versions` (`hami-core` and `ubs-virt` matching the 40-hex commits in `pack/gpustack-operator/Dockerfile`), and `subscriptions`.

Strictly off-limits (protected):

- `pack/gpustack-operator/external/amd`
- `pack/gpustack-operator/external/thead`
- `csrc`
- `binding`
- `gen/binding`
- `pkg/devicemanager/allocator`

## Preflight Staging Inputs (`.auto-sync/.stage/slicing/`)

The candidate sandbox runs offline (`networkMode: none`) inside `linux-bwrap`. Host toolchains
(`docker`, `cmake`, `go`) and external host staging directories are NOT mounted. Always inspect
`.auto-sync/.stage/slicing/` FIRST (falling back to `$AUTOSYNC_STAGE_DIR` only if
`.auto-sync/.stage/slicing/` is absent):

- `.auto-sync/.stage/slicing/slicing-preflight-report.json` — records `pinned_commit`,
  `head_commit`, `watched_changed`, `changed_files`, and `patch_clean` (`true|false`) from running
  `patch -p1 --dry-run --batch --forward` against `ubs-virt-enpu/vcann-rt` at target HEAD.
- `.auto-sync/.stage/slicing/upstream-records.json` — normalized `TaskpackUpstreamRecord` entries.
- `.auto-sync/.stage/slicing/ubs-virt/ubs-virt-enpu/vcann-rt/` (and `ubs-virt-enpu/common/` when
  present) — CRLF-exact snapshot at `ubs-virt` target HEAD (present when `ubs-virt` is `available`
  or `patch_clean` is `false`).
- `.auto-sync/.stage/slicing/hami-core/` — `CMakeLists.txt`, `src/CMakeLists.txt`, and `build.sh`
  at `hami-core` target HEAD (present when `hami-core` is `available`).

**Read-only stage rule:** `.auto-sync/.stage/**` is read-only host-staged context outside
`scope.writable`. **NEVER** edit, move, or delete any file under `.auto-sync/.stage/**` (doing so
fails `scope-guard`). Loopwright automatically strips `.auto-sync/.stage/**` when running
`validate_slicing.sh` (including `docker buildx` xbuild + ELF verification) on the host and when
publishing the Pull Request.

## Settlement and No-Change Behavior

- Check both `slicing-preflight-report.json` and the `upstream-records` input.
- If an upstream's `watched_changed` is `false` (for example, commits only touched documentation or CI outside `src`, `include`, `CMakeLists.txt`, `build.sh` for `HAMi-core`, or outside `ubs-virt-enpu/vcann-rt` and `ubs-virt-enpu/common` for `ubs-virt`), do NOT bump its commit in `pack/gpustack-operator/Dockerfile`.
- If neither upstream has watched-path changes (or both already match `pack/gpustack-operator/Dockerfile`), settle cleanly as `no_change` without editing files.

## Critical Ascend `vcann-rt` Patch and CRLF Rules

When updating `LIB_UBS_VIRT_ENPU_COMMIT` and rebasing `pack/gpustack-operator/external/ascend/vcann-rt/0001-dsmi-get-hbm-info-slice-view.patch`:

1. **Preserve CRLF hunks:** Upstream `ubs-virt-enpu/vcann-rt/CMakeLists.txt` uses CRLF (`\r\n`) line endings, and `.gitattributes` sets `-text` on `0001-dsmi-get-hbm-info-slice-view.patch` so those `\r` bytes stay intact. When rebasing `0001-dsmi-get-hbm-info-slice-view.patch`, ensure `CMakeLists.txt` context, removed (`-`), and added (`+`) lines that come from or replace CRLF lines keep exact `\r\n` endings, while `src/dsmi/dsmi_hook.c` keeps LF (`\n`) endings and the explanatory header above `diff --git` stays intact.
2. **Verify patch application cleanly:** Every `*.patch` under `pack/gpustack-operator/external/ascend/vcann-rt/` must apply byte-cleanly with `patch -p1 --dry-run --batch --forward` and zero `.rej` files against `ubs-virt-enpu/vcann-rt` at the target `LIB_UBS_VIRT_ENPU_COMMIT`.
3. **ELF and symbol contract (`ascend-case-1.sh`):**
   - `libvruntime.so` and `enpu-monitor` must link `libc_sec.so` and must NOT link `libascendcl.so`.
   - `libvruntime.so` must export `dsmi_get_hbm_info` as a defined `GLOBAL DEFAULT FUNC` (and must NOT define any `dcmi_*` symbols).
   - `src/dsmi/dsmi_hook.c` must remain linked into `libvruntime.so` only, never into `enpu-monitor`.

## Verification Gate (`validate_slicing.sh`)

`validate_slicing.sh` enforces:

1. Baseline `diff -U0` guard on `pack/gpustack-operator/Dockerfile` allowing changes ONLY to `ARG LIB_HAMI_CORE_COMMIT` and `ARG LIB_UBS_VIRT_ENPU_COMMIT`.
2. 40-hex commit format validation and parity between `pack/gpustack-operator/Dockerfile` and `.auto-sync/proposals/slicing.json`.
3. `bash -n` and `hack/check/symbols.sh` (plain ASCII comments only, no decorative symbols) on `pack/gpustack-operator/external/{nvidia,ascend}/*.sh`.
4. CRLF-aware `patch -p1 --dry-run --batch --forward` check against `$AUTOSYNC_STAGE_DIR/ubs-virt` at the candidate `LIB_UBS_VIRT_ENPU_COMMIT`.
5. Hardware-free `docker buildx` compilation and ELF/lock verification using non-root `XB_STAGE` under `/tmp` and `XB_MODE=local`:
   - When NVIDIA changed: `xbuild-nvidia-cuda-12` and `xbuild-nvidia-cuda-13` with `nvidia-case-1.sh` and `nvidia-case-4.sh` (Parts A/B; Part C skips when no GPU is attached).
   - When Ascend changed: `xbuild-ascend-cann-8-910b` and `xbuild-ascend-cann-9-910b` with `ascend-case-1.sh`.
