# Spec: Modular Loopwright Auto-Sync Architecture Across Six Domains

Status: Building
Blocked on: Implementation of tasks T1 through T11 and merge of the modular auto-sync pull requests.
Type: Feature

## Table of Contents

- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
  - [Core Features & Acceptance Criteria](#core-features--acceptance-criteria)
  - [Notes / Constraints / Caveats](#notes--constraints--caveats)
  - [Boundaries](#boundaries)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [Why Six Modular Domain Taskpacks Instead of a Monolithic Taskpack](#why-six-modular-domain-taskpacks-instead-of-a-monolithic-taskpack)
  - [Loopwright Schema and Control-Root Invariants](#loopwright-schema-and-control-root-invariants)
  - [Six-Domain Architecture and `.auto-sync/` Layout](#six-domain-architecture-and-auto-sync-layout)
  - [Domain 1: Accelerator Slicing and Virtualization (`slicing-sync`)](#domain-1-accelerator-slicing-and-virtualization-slicing-sync)
  - [Domain 2: KV Cache and Weight Transfer Data Plane (`kv-transfer-sync`)](#domain-2-kv-cache-and-weight-transfer-data-plane-kv-transfer-sync)
  - [Domain 3: Disaggregated Inference Routers (`router-sync`)](#domain-3-disaggregated-inference-routers-router-sync)
  - [Domain 4: Offline Capacity Prior Catalog (`priors-sync`)](#domain-4-offline-capacity-prior-catalog-priors-sync)
  - [Domain 5: Vendored Helm Subcharts and Mirrored Images (`charts-sync`)](#domain-5-vendored-helm-subcharts-and-mirrored-images-charts-sync)
  - [Domain 6: Kubernetes Lockstep Modules and CLI Toolchains (`k8s-deps-sync`)](#domain-6-kubernetes-lockstep-modules-and-cli-toolchains-k8s-deps-sync)
  - [Two-Stage Image Publication Gate](#two-stage-image-publication-gate)
  - [Shared-File Variable-Level Diff Guards](#shared-file-variable-level-diff-guards)
  - [Unified GitHub Actions Matrix, Host Preflight Staging, and Comment Routing](#unified-github-actions-matrix-host-preflight-staging-and-comment-routing)
  - [Three-Phase Rollout Criteria](#three-phase-rollout-criteria)
  - [Commands](#commands)
  - [Project Structure](#project-structure)
  - [Code Style](#code-style)
  - [Implementation Plan](#implementation-plan)
  - [Test Plan](#test-plan)
- [Alternatives](#alternatives)
- [Open Questions](#open-questions)

## Summary

Introduce a six-domain declarative Loopwright (`loopwright.dev/v1alpha1` `Taskpack`) auto-sync
architecture under `.auto-sync/` to automate upstream version discovery, patch rebasing, offline
sandbox verification, pull request publication, and reviewed memory persistence across
`gpustack-operator`. Rather than coupling heterogeneous C/C++, Rust, Go, Python, Helm, and benchmark
upstreams into one monolithic automation unit, the architecture partitions upstream synchronization
into six isolated domain Taskpacks: `slicing-sync` (`HAMi-core` and `openEuler/ubs-virt` slicing
libraries), `kv-transfer-sync` (`Mooncake`, `NIXL`, `UCX`, `HIXL`, and `ModelExpress` with
`gpustack/runner` commit/patch parity), `router-sync` (`llm-d-router`, `vllm-router`, and
`sgl-model-gateway` plus eight local patches), `priors-sync` (`SemiAnalysisAI/InferenceX` capacity
priors), `charts-sync` (five vendored Helm subcharts, 16 chart patches, and 11 subchart
`gpustack/mirrored-*` image pins), and `k8s-deps-sync` (six CLI binary pins in
[pack/gpustack-operator/Dockerfile](../pack/gpustack-operator/Dockerfile) plus lockstep `k8s.io/*`
`v0.36.x`, `controller-runtime`, `helm.sh/helm/v3`, `kueue`, `lws`, and `dra-driver` module
upgrades). Each domain enforces strict `spec.scope` boundaries, variable-level baseline `diff -U0` guards on
shared files, a two-stage container image publication gate backed by `crane manifest`, and
hardware-free `command.v1` verification scripts orchestrated by a single `.github/workflows/auto-sync.yml`
workflow.

## Motivation

`gpustack-operator` integrates rapidly evolving upstream projects across six distinct technical
layers:

1. **Vendor logical-slicing preload libraries** compiled into
   [pack/gpustack-operator/Dockerfile](../pack/gpustack-operator/Dockerfile) (`Project-HAMi/HAMi-core`
   and `openEuler/ubs-virt` `vcann-rt`, patched by
   [0001-dsmi-get-hbm-info-slice-view.patch](../pack/gpustack-operator/external/ascend/vcann-rt/0001-dsmi-get-hbm-info-slice-view.patch)).
2. **KV-cache and remote-weight transfer runtimes** in
   [pack/mirrored-mooncake/Dockerfile](../pack/mirrored-mooncake/Dockerfile),
   [pack/model-transfer/Dockerfile](../pack/model-transfer/Dockerfile), and
   [pack/mirrored-modelexpress-server/Dockerfile](../pack/mirrored-modelexpress-server/Dockerfile)
   (`kvcache-ai/Mooncake`, `ai-dynamo/nixl`, `openucx/ucx`, `hicann/hixl` with
   [001_channel_generation.patch](../pack/model-transfer/patches/hixl/001_channel_generation.patch),
   and `ai-dynamo/modelexpress`), which must stay byte-aligned with `gpustack/runner` inference
   engine images.
3. **Disaggregated inference routers** bundled into
   [pack/llm-router/Dockerfile](../pack/llm-router/Dockerfile) (`llm-d/llm-d-router`,
   `vllm-project/router`, and `sgl-project/sglang` `sgl-model-gateway`), carrying eight local Go and
   Rust patches (~6,900 diff lines) under `pack/llm-router/patches/`.
4. **Offline scaling capacity priors** in
   [priors.yaml](../pkg/worker/scaling/priors/priors.yaml) imported from `SemiAnalysisAI/InferenceX`
   via [gen/priors/main.go](../gen/priors/main.go).
5. **Vendored Helm subcharts** staged by [hack/deps.sh](../hack/deps.sh) (`kueue`,
   `node-feature-discovery`, `csi-driver-nfs`, `csi-driver-s3`, and `topograph`), carrying 16 template
   patches under `hack/deploy/gpustack-operator/chart/charts/` and 11 subchart `gpustack/mirrored-*`
   image references in [values.yaml](../deploy/gpustack-operator/chart/values.yaml).
6. **Kubernetes `v0.36.x` lockstep Go dependencies and CLI toolchains** staged in
   [hack/deps.sh](../hack/deps.sh), [hack/lib/helm.sh](../hack/lib/helm.sh),
   [pack/gpustack-operator/Dockerfile](../pack/gpustack-operator/Dockerfile), and [go.mod](../go.mod),
   governed by [upgrade-compat.sh](../hack/check/upgrade-compat.sh) and
   [toolpins-selftest.sh](../hack/check/toolpins-selftest.sh).

Tracking and rebasing these six surfaces manually creates version drift (such as `Mooncake` moving to
`0.3.14` or `vllm-router` moving to `0.1.17` in `gpustack/runner` before `gpustack-operator` updates
its recipes) and risks human error when bumping image tags in
[value.go](../pkg/worker/settings/value.go) before [base-image.yml](../.github/workflows/base-image.yml)
or [sync-image.yml](../.github/workflows/sync-image.yml) publishes the multi-architecture manifest.

### Goals

- Partition `gpustack-operator` upstream synchronization into six self-contained Loopwright
  `loopwright.dev/v1alpha1` `Taskpack` manifests under `.auto-sync/`: `slicing-sync`,
  `kv-transfer-sync`, `router-sync`, `priors-sync`, `charts-sync`, and `k8s-deps-sync`.
- Conform strictly to Loopwright's `validateTaskpackManifest` and `resolveTaskpackMemoryLocation`
  invariants for `controlRoot: .auto-sync`, including exact proposal file carve-outs
  (`.auto-sync/proposals/<domain>.json`), slash-safe `headBranchPrefix` declarations
  (`auto-sync-<domain>/`), and per-domain CAS memory directories
  (`.auto-sync/memory/<domain>-sync`).
- Enforce a two-stage image publication gate for `kv-transfer-sync` and `router-sync` so a pull
  request never writes an unpublished `gpustack/*` image tag into
  [value.go](../pkg/worker/settings/value.go), [settings.md](../docs/reference/settings.md), or
  [values.yaml](../deploy/gpustack-operator/chart/values.yaml).
- Enforce variable-level baseline `diff -U0` guards inside `command.v1` verification scripts for shared
  files ([pack/gpustack-operator/Dockerfile](../pack/gpustack-operator/Dockerfile),
  [value.go](../pkg/worker/settings/value.go), [settings.md](../docs/reference/settings.md),
  [values.yaml](../deploy/gpustack-operator/chart/values.yaml), and [hack/deps.sh](../hack/deps.sh))
  so no domain can mutate another domain's pins even inside Loopwright's gitless
  `LOOPWRIGHT_CANDIDATE_DIR`.
- Execute hardware-free Docker multi-stage compilation, ELF symbol verification, patch rebase checks,
  and Go/Helm/docs test suites on `ubuntu-24.04-8x` (4x-resource, 8-vCPU) GitHub Actions runners
  without physical GPUs or NPUs.
- Relocate the capacity prior provenance ledger from
  [records.md](../.agents/skills/gpustack-operator-priors/references/records.md) to
  `pkg/worker/scaling/priors/records.md` (updating pointers in
  [SKILL.md](../.agents/skills/gpustack-operator-priors/SKILL.md) and
  [scaling.md](../docs/modules/model-deployment/scaling.md)) so `priors-sync` can update provenance
  checksums without violating Loopwright's built-in `.agents` write protection.

### Non-Goals

- Do not automate changes to in-repository C slicing shims under
  [csrc/amd/rocm-slicing-shim/](../csrc/amd/rocm-slicing-shim) or
  [csrc/thead/ppu-slicing-shim/](../csrc/thead/ppu-slicing-shim), or to
  [pack/thead-ppu-devel/Dockerfile](../pack/thead-ppu-devel/Dockerfile), which requires a private
  presigned OSS SDK URL.
- Do not allow any Taskpack to modify vendor CGO bindings under [binding/](../binding) or
  [gen/binding/](../gen/binding), device allocators under
  [pkg/devicemanager/allocator/](../pkg/devicemanager/allocator), portable Python transfer code under
  [model-transfer/python/](../model-transfer/python), hand-written public API contracts under
  [api/v1/](../api/v1) or [api/worker/v1/](../api/worker/v1), or benchmark/engine compatibility
  matrices under [engine-versions.md](../docs/modules/model-deployment/engine-versions.md).
- Do not automatically dispatch [base-image.yml](../.github/workflows/base-image.yml) or
  [sync-image.yml](../.github/workflows/sync-image.yml) from inside a candidate sandbox; image builds
  and registry mirrors remain explicit maintainer-triggered workflows after Stage A recipe pull
  requests merge.
- Do not require physical GPU/NPU runners for automated PR verification; hardware E2E suites remain
  human-dispatched release gates.

## Proposal

Adopt a modular six-domain Loopwright auto-sync architecture rooted under `.auto-sync/`, driven by a
unified `.github/workflows/auto-sync.yml` workflow. Before invoking Loopwright on each domain, a
domain-specific host preflight script (`.auto-sync/scripts/preflight-<domain>.sh`) runs outside the
candidate sandbox to clone non-GitHub or multi-tag upstream repositories, probe multi-arch image
manifests on Docker Hub via `crane manifest`, fetch InferenceX benchmark snapshots, and emit
normalized `--upstream-records-json` records plus offline staging artifacts in an external staging
directory (`$AUTOSYNC_STAGE_DIR`). Loopwright then compiles the domain's
`.auto-sync/<domain>-taskpack.yaml` manifest, runs the sandboxed coding agent with `networkMode: none`,
verifies the candidate tree through `json-artifact.v1`, `scope.v1`, and the domain's protected
`.auto-sync/checks/validate_<domain>.sh` (`command.v1`) gate, opens or updates the domain PR on
`auto-sync-<domain>/main`, and publishes reviewed run memory to `loopwright/memory/<domain>-sync/main`.

### User Stories

#### Story 1

As a device-manager maintainer, I want `slicing-sync` to track upstream `Project-HAMi/HAMi-core` and
`openEuler/ubs-virt` commits, rebase
[0001-dsmi-get-hbm-info-slice-view.patch](../pack/gpustack-operator/external/ascend/vcann-rt/0001-dsmi-get-hbm-info-slice-view.patch)
preserving CRLF line endings when required, and run hardware-free `docker buildx` compilation plus
ELF symbol and POSIX lock verification (`nvidia-case-1.sh`, `nvidia-case-4.sh` Parts A/B, and
`ascend-case-1.sh`), so that preload library updates are pre-verified before human review.

#### Story 2

As a model-transfer and KV-cache maintainer, I want `kv-transfer-sync` to keep `Mooncake`, `NIXL`,
`UCX`, `HIXL`, and `ModelExpress` build recipes in sync with upstream releases and `gpustack/runner`
(including the four `case "${MOONCAKE_VERSION}"` guards in
[pack/mirrored-mooncake/Dockerfile](../pack/mirrored-mooncake/Dockerfile) and byte-identical
[001_channel_generation.patch](../pack/model-transfer/patches/hixl/001_channel_generation.patch)),
and to promote default image tags in [value.go](../pkg/worker/settings/value.go) only after
`crane manifest` confirms the published image exists on Docker Hub, so that `main` never references
an unpublished container image.

#### Story 3

As an inference gateway maintainer, I want `router-sync` to monitor `llm-d/llm-d-router`,
`vllm-project/router`, and `sgl-project/sglang` (`gateway-v*` tags), rebase the eight patches under
`pack/llm-router/patches/`, automatically retire upstreamed cherry-picks such as
[0001-epd-routing.patch](../pack/llm-router/patches/vllm-router/0001-epd-routing.patch) once commit
`516f1cb` is an ancestor of the target tag, and verify Go and Rust builder stages via `docker buildx`,
so that router upgrades do not silently drop observation or disaggregation hunks.

#### Story 4

As an autoscaling maintainer, I want `priors-sync` to ingest new `SemiAnalysisAI/InferenceX`
benchmark sweeps into [priors.yaml](../pkg/worker/scaling/priors/priors.yaml) and update SHA-256
provenance tables in `pkg/worker/scaling/priors/records.md` while passing all `gen/priors` and
`pkg/worker/scaling` unit and docs lint gates, so that capacity priors stay current without manual
data wrangling.

#### Story 5

As a chart and release maintainer, I want `charts-sync` and `k8s-deps-sync` to evolve vendored Helm
subcharts and Kubernetes `v0.36.x` / CLI toolchains on separate schedules and PR branches, verifying
`crane manifest` on all `gpustack/mirrored-*` images, zero `.rej` patch rejects in
[hack/deps.sh](../hack/deps.sh), and zero violations in
[upgrade-compat.sh](../hack/check/upgrade-compat.sh) and
[toolpins-selftest.sh](../hack/check/toolpins-selftest.sh), so that chart updates and Go module
upgrades never block each other.

### Core Features & Acceptance Criteria

- **F1 · Six schema-valid `loopwright.dev/v1alpha1` Taskpack manifests:**
  - Create `.auto-sync/{slicing,kv-transfer,router,priors,charts,k8s-deps}-taskpack.yaml`.
  - Every manifest passes `validateTaskpackManifest(doc, { controlRoot: '.auto-sync', manifestPath: '.auto-sync/<domain>-taskpack.yaml' })` with zero schema or scope violations.
  - Every manifest sets `spec.proposal.path: .auto-sync/proposals/<domain>.json` and includes that exact path in `spec.scope.writable`.
  - Every manifest sets `spec.publication.headBranchPrefix: auto-sync-<domain>/` (matching `/^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$/` after stripping trailing `/`) and `spec.memory.directory: .auto-sync/memory/<domain>-sync` (resolving cleanly via `resolveTaskpackMemoryLocation` to branch `loopwright/memory/<domain>-sync/main`).
- **F2 · Host preflight discovery, `source-revision` vendor staging, and offline cache warm-up:**
  - Create `.auto-sync/scripts/preflight-{slicing,kv-transfer,router,priors,charts,k8s-deps}.sh` plus shared `.auto-sync/scripts/lib-preflight.sh` and `.auto-sync/scripts/write_models_json.py`.
  - Each script populates `/tmp/gpustack-autosync-stage/<domain>` (outside the Git checkout) with pre-cloned upstream source trees, baseline shared-file copies (`baseline/`), pre-warmed `.sbin`/`go-cache`/`go-mod`, `crane manifest` verification results, and normalized `upstream-records.json` passed to Loopwright via `upstream-records`.
  - For bulk-generated domains (`priors`, `charts`, `k8s-deps`), preflight runs deterministic generators (`go run ./gen/priors`, `hack/deps.sh`, `make generate`) onto a detached local commit `source_revision` while keeping `refs/heads/main` untouched at `GITHUB_SHA`, staying within Loopwright's 128-file / 4-MiB `Candidate` overlay budget and squashing into a single PR commit parented on `main`.
- **F3 · Two-stage image publication gate:**
  - Stage A (Recipe Update) modifies only `pack/**` Dockerfiles and patches when upstream versions advance and target `gpustack/*` image tags are not yet published on Docker Hub.
  - Stage B (Default Tag Promotion) emits `status: "available"` (`<tag>+stage-b`) and updates [value.go](../pkg/worker/settings/value.go), [settings.md](../docs/reference/settings.md), and [values.yaml](../deploy/gpustack-operator/chart/values.yaml) (`modelTransfer.images.*`) only when `crane manifest` confirms multi-arch (`linux/amd64` and `linux/arm64`) manifests exist on `docker.io/gpustack/*`.
  - `.auto-sync/checks/validate_kv_transfer.sh` and `.auto-sync/checks/validate_router.sh` fail closed if any image reference in [value.go](../pkg/worker/settings/value.go) or [values.yaml](../deploy/gpustack-operator/chart/values.yaml) lacks a valid published manifest in the preflight probe report.
- **F4 · Gitless shared-file variable-level diff guards:**
  - `.auto-sync/checks/validate_slicing.sh` and `.auto-sync/checks/validate_k8s_deps.sh` run `diff -U0` against `$AUTOSYNC_STAGE_DIR/baseline/pack/gpustack-operator/Dockerfile` (checking exit codes `0`/`1`/`>=2` without `|| true`) and reject any hunk outside their owned `ARG` lines.
  - `.auto-sync/checks/validate_kv_transfer.sh` and `.auto-sync/checks/validate_router.sh` run `diff -U0` against baseline copies of [value.go](../pkg/worker/settings/value.go), [settings.md](../docs/reference/settings.md), and [values.yaml](../deploy/gpustack-operator/chart/values.yaml), rejecting edits outside their owned image variables.
  - `.auto-sync/checks/validate_charts.sh` and `.auto-sync/checks/validate_k8s_deps.sh` run `diff -U0` against baseline [hack/deps.sh](../hack/deps.sh) and restrict edits to `chart_staging()` versus `mod_staging()` respectively.
- **F5 · Capacity prior provenance relocation and `.dockerignore` isolation:**
  - Exclude `/.auto-sync` in [.dockerignore](../.dockerignore); move the canonical provenance ledger to `pkg/worker/scaling/priors/records.md`, update [SKILL.md](../.agents/skills/gpustack-operator-priors/SKILL.md), [records.md](../.agents/skills/gpustack-operator-priors/references/records.md), and [scaling.md](../docs/modules/model-deployment/scaling.md), and ensure `make lint docs` passes with `0` errors.
- **F6 · Unified GitHub Actions workflow `.github/workflows/auto-sync.yml`:**
  - Support staggered weekly `schedule` cron triggers, manual `workflow_dispatch` (`domain` selector, `force`, `dry-run`, and `runner_profile` defaulting to `8x` for `ubuntu-24.04-8x`), and `/loopwright` or `/auto-sync` PR comment commands (`retry`, `revise`, `status`) routed on `main` (`refs/heads/main` preserved) by matching the PR's `headRefName` (`auto-sync-<domain>/main`) to `.auto-sync/<domain>-taskpack.yaml`.

### Notes / Constraints / Caveats

- **Strict YAML 1.2 and string constraints in Loopwright:** Loopwright's `parseTaskpackDocument`
  rejects YAML anchors, aliases, duplicate keys, symlinks (`O_NOFOLLOW`), and any `${...}`
  interpolation token (`TASKPACK_UNTRUSTED_INTERPOLATION`). Furthermore, `requireString` in
  `validateTaskpackManifest` rejects `\r` and `\n` control characters in string fields such as
  `spec.goal.candidate`, `spec.goal.noChange`, and `spec.publication.titleTemplate`. All such fields
  must be single-line strings in YAML (never block scalars `>` or `|` that append a trailing `\n`).
- **Control-root sibling protection cap:** Loopwright's `collectControlRootSiblingPaths` enumerates
  files and directories under `.auto-sync` and merges them into `allProtected`, which is capped at
  `128` entries (`effective protected path set exceeds 128 entries`). Consequently, temporary upstream
  git clones must never be staged inside `.auto-sync/`; they must reside in `$AUTOSYNC_STAGE_DIR`
  outside the repository working tree and be passed to `command.v1` via `env: [AUTOSYNC_STAGE_DIR]`.
- **Order of `make lint` and `make generate`:** As documented in
  [development.md](../docs/contribute/development.md), `make lint` invokes `goimports-reviser` and
  `golangci-lint --fix`, which rewrite Go files in place. Any `command.v1` script that runs both code
  generation and Go lint (`validate_k8s_deps.sh`, `validate_kv_transfer.sh`, `validate_charts.sh`)
  must run `make generate` (or `make generate chart`) again after `make lint` or verify zero diff
  between generated artifacts and a fresh regeneration.

### Boundaries

- **Always:**
  - Validate every `.auto-sync/<domain>-taskpack.yaml` against Loopwright's `loopwright.dev/v1alpha1`
    schema (`validateTaskpackManifest`) with `controlRoot: .auto-sync`.
  - Keep candidate sandboxes offline (`networkMode: none`) and perform all network I/O in the host
    preflight script (`.auto-sync/scripts/preflight-<domain>.sh`) prior to Loopwright execution.
  - Enforce variable-level baseline `diff -U0` guards in `.auto-sync/checks/validate_<domain>.sh` for shared
    files (`pack/gpustack-operator/Dockerfile`, `pkg/worker/settings/value.go`,
    `docs/reference/settings.md`, and `hack/deps.sh`).
  - Gate default image tag updates in `pkg/worker/settings/value.go` and
    `deploy/gpustack-operator/chart/values.yaml` on `crane manifest` verification.
- **Ask first:**
  - Adding a new upstream dependency or subchart to any domain Taskpack.
  - Changing a domain's `spec.scope.writable` or `spec.scope.protected` path set.
  - Retiring a local patch in `pack/` or `hack/deploy/` unless its `WHEN TO REMOVE` upstream commit
    condition is mechanically satisfied (such as commit `516f1cb` in `vllm-project/router`).
- **Never:**
  - Write an unpublished `gpustack/*` container image tag into
    [value.go](../pkg/worker/settings/value.go), [settings.md](../docs/reference/settings.md), or
    [values.yaml](../deploy/gpustack-operator/chart/values.yaml).
  - Include `csrc/**`, `binding/**`, `gen/binding/**`, `pkg/devicemanager/allocator/**`,
    `model-transfer/python/**`, `pack/thead-ppu-devel/**`, hand-written files under `api/v1/**` or
    `api/worker/v1/**`, `specs/**`, or `docs/modules/model-deployment/*-benchmark.md` in any writable
    scope.
  - Store temporary upstream git clones inside `.auto-sync/` where they would inflate
    `collectControlRootSiblingPaths` beyond the 128-entry protected-path limit or dirty the git
    worktree.

### Risks and Mitigations

- **Risk:** A domain Taskpack modifies variables belonging to another domain inside a shared file
  ([pack/gpustack-operator/Dockerfile](../pack/gpustack-operator/Dockerfile),
  [value.go](../pkg/worker/settings/value.go), or [hack/deps.sh](../hack/deps.sh)), causing merge
  conflicts or unverified cross-domain changes.
  - **Mitigation:** Each domain's `command.v1` verification script runs a strict line-level `diff -U0` against `$AUTOSYNC_STAGE_DIR/baseline/<file>`
    validator that fails closed if any modified line in a shared file falls outside that domain's
    explicitly allowlisted variable names or function blocks.
- **Risk:** Updating `MOONCAKE_VERSION` in
  [pack/mirrored-mooncake/Dockerfile](../pack/mirrored-mooncake/Dockerfile) to a new minor/patch
  series (such as `0.3.14`) fails at image build time because the Dockerfile has four version-guard
  `case "${MOONCAKE_VERSION}" in` blocks at lines 208, 474, 922, and 1401.
  - **Mitigation:** `validate_kv_transfer.sh` parses `MOONCAKE_VERSION` from
    [pack/mirrored-mooncake/Dockerfile](../pack/mirrored-mooncake/Dockerfile) and evaluates all four
    `case` guards against the pinned version, failing closed if any stage (`mooncake-store`, `cuda`,
    `cann`, or `rocm`) rejects it.
- **Risk:** `command.v1` executes inside Loopwright's isolated `LOOPWRIGHT_CANDIDATE_DIR` materialized
  exclusively from `git ls-tree` of `baseTree.commitOid` plus candidate overlay files, so untracked
  files in the repository working tree are absent during check execution.
  - **Mitigation:** Host preflight scripts write cloned upstream repositories and probe reports to
    `$AUTOSYNC_STAGE_DIR` (`/tmp/gpustack-autosync-stage/<domain>`) outside the Git working tree, and
    each Taskpack passes `env: [AUTOSYNC_STAGE_DIR]` in its `command.v1` parameters so verification
    scripts read preflight artifacts deterministically without network access.
- **Risk:** Docker multi-stage cross-compilation in `slicing-sync` or `router-sync` exceeds the
  default 30-second `command.v1` timeout.
  - **Mitigation:** Declare explicit `timeoutSeconds: 2400` on `command.v1` checks and matching
    `spec.limits: { wallTimeSeconds: 3600, commandTimeoutSeconds: 2400 }` on compute-heavy Taskpacks,
    and run only the builder stages whose inputs changed when only one vendor upstream advanced.

## Design Details

### Why Six Modular Domain Taskpacks Instead of a Monolithic Taskpack

Combining all external upstreams of `gpustack-operator` into a single monolithic Taskpack was
evaluated and rejected for four architectural reasons:

1. **Failure-domain isolation:** If `openEuler/ubs-virt` changes `CMakeLists.txt` context and breaks
   [0001-dsmi-get-hbm-info-slice-view.patch](../pack/gpustack-operator/external/ascend/vcann-rt/0001-dsmi-get-hbm-info-slice-view.patch),
   a monolithic Taskpack would block InferenceX capacity prior updates, Helm subchart bumps, and
   `gpustack/runner` HIXL parity updates from landing. Partitioning into six domains ensures a broken
   patch or failing gate in one domain never blocks the other five.
2. **Heterogeneous verification budgets and toolchains:**
   - `priors-sync` runs pure Go unit tests (`go test ./gen/priors/... ./pkg/worker/scaling/...`) and
     `make lint docs` in under 2 minutes.
   - `slicing-sync` pulls multi-gigabyte CUDA (`nvidia/cuda:12.9.2-cudnn-devel-ubi8`,
     `13.0.3-cudnn-devel-ubi8`) and CANN (`quay.io/ascend/cann:8.5.0-*`, `9.1.0-*`) images to run C
     compilation and ELF symbol/lock verification (~15–25 minutes).
   - `router-sync` compiles Go (`cmd/epp`) and two Rust workspaces (`vllm-router` and
     `sgl-model-gateway`) across 6,900 lines of local patches (~15–25 minutes).
   - `k8s-deps-sync` runs `make deps`, `make generate`, `make lint`, and `RACE=false make test` across
     the entire Go repository (~20–30 minutes).
   Separating these domains keeps each run well within Loopwright's `wallTimeSeconds: 3600` ceiling
   and avoids running expensive container builds for unrelated changes.
3. **Reviewer and CODEOWNERS alignment:** Each domain maps 1:1 to a repository review area:
   `area/device-manager` (`slicing-sync`), `area/model-transfer` (`kv-transfer-sync`), `area/worker`
   (`router-sync` and `priors-sync`), `area/chart` (`charts-sync`), and `area/dependencies`
   (`k8s-deps-sync`).
4. **Partitioned CAS memory stores:** Loopwright's `@loopwright/memory` engine injects up to `4 KiB`
   of active memory directly into the author prompt and stages up to `64 KiB` in
   `active-snapshot.md`. Separate Taskpacks isolate memory onto six branches
   (`loopwright/memory/<domain>-sync/main`), preventing Ascend CMake failure notes from polluting
   Kubernetes Go module upgrade prompts.

### Loopwright Schema and Control-Root Invariants

Every manifest under `.auto-sync/<domain>-taskpack.yaml` must satisfy four compiler invariants enforced
by Loopwright (`packages/github-app/src/schema.ts`, `taskpack.ts`, and `turnkey.ts`):

1. **Control-root derivation (`deriveControlRoot`):** Because each manifest resides at
   `.auto-sync/<domain>-taskpack.yaml`, `posix.dirname(relativeTaskpackPath)` resolves `controlRoot`
   to `.auto-sync`. Loopwright automatically protects `.auto-sync/policies`, `.auto-sync/checks`,
   `.auto-sync/memory`, `.auto-sync/taskpack.{yaml,yml,json}`, the active `.auto-sync/<domain>-taskpack.yaml`,
   all declared `spec.instructions` files, and all `command.v1` `script` paths.
2. **Control-root proposal carve-out (`isControlRootProposalWritableAllowed`):** Placing
   `spec.proposal.path` at `.auto-sync/proposals/<domain>.json` is permitted only when
   `.auto-sync/proposals/<domain>.json` appears as an exact file entry in `spec.scope.writable` and
   no other `spec.scope.writable` entry lies inside or encompasses `.auto-sync`. Additionally,
   `compileScopePatterns` treats the carved-out proposal path as an exact file entry while appending
   `/**` to directory/file entries in `spec.scope.writable` (manifest paths must never include literal
   `/**` globs, which `PATH_SEGMENT_PATTERN` rejects).
3. **Branch prefix grammar (`schema.ts:1115` and `turnkey.ts:1862`):** `validateTaskpackManifest`
   validates `spec.publication.headBranchPrefix` with:
   `!/^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$/.test(headBranchPrefix.replace(/\/$/, ''))`.
   Any interior `/` (such as `auto-sync/slicing-`) is rejected because `.replace(/\/$/, '')` strips
   only a single optional trailing `/`, and `/` is excluded from `[A-Za-z0-9._-]`. Setting
   `headBranchPrefix: auto-sync-<domain>/` passes validation (`auto-sync-<domain>`) and combines with
   `targetBranch: main` in `turnkey.ts:1862` (`${headBranchPrefix}${targetBranch}`) to form the Git
   branch `auto-sync-<domain>/main`.
4. **Memory directory resolution (`resolveTaskpackMemoryLocation`):** Under `controlRoot: .auto-sync`,
   `expectedControlMemoryRoot` is `.auto-sync/memory`. Setting
   `spec.memory.directory: .auto-sync/memory/<domain>-sync` (whose suffix matches
   `/${manifest.metadata.name}`) causes `resolveTaskpackMemoryLocation` to strip `/${metadata.name}`,
   verify `rawMemoryRoot === '.auto-sync/memory'`, and resolve `memoryDirectory` to
   `.auto-sync/memory/<domain>-sync` on branch `loopwright/memory/<domain>-sync/main` without
   double-nesting.

### Six-Domain Architecture and `.auto-sync/` Layout

| Domain (`metadata.name`) | Manifest Path | Proposal Path | Head Branch (`headBranchPrefix` + `main`) | Memory Branch | Primary Schedule (UTC) |
| --- | --- | --- | --- | --- | --- |
| `slicing-sync` | `.auto-sync/slicing-taskpack.yaml` | `.auto-sync/proposals/slicing.json` | `auto-sync-slicing/main` | `loopwright/memory/slicing-sync/main` | Mon `02:00` |
| `kv-transfer-sync` | `.auto-sync/kv-transfer-taskpack.yaml` | `.auto-sync/proposals/kv-transfer.json` | `auto-sync-kv-transfer/main` | `loopwright/memory/kv-transfer-sync/main` | Tue `02:00` |
| `router-sync` | `.auto-sync/router-taskpack.yaml` | `.auto-sync/proposals/router.json` | `auto-sync-router/main` | `loopwright/memory/router-sync/main` | Tue `03:00` |
| `priors-sync` | `.auto-sync/priors-taskpack.yaml` | `.auto-sync/proposals/priors.json` | `auto-sync-priors/main` | `loopwright/memory/priors-sync/main` | Mon `01:00` |
| `charts-sync` | `.auto-sync/charts-taskpack.yaml` | `.auto-sync/proposals/charts.json` | `auto-sync-charts/main` | `loopwright/memory/charts-sync/main` | Wed `02:00` |
| `k8s-deps-sync` | `.auto-sync/k8s-deps-taskpack.yaml` | `.auto-sync/proposals/k8s-deps.json` | `auto-sync-k8s-deps/main` | `loopwright/memory/k8s-deps-sync/main` | Thu `02:00` |

### Domain 1: Accelerator Slicing and Virtualization (`slicing-sync`)

#### Upstreams and Tracked Files

- **Upstreams:**
  - `hami-core`: `kind: git-commit`, `source: Project-HAMi/HAMi-core`, `watchedPaths: [src, include, CMakeLists.txt, build.sh]`. Currently pinned at `ARG LIB_HAMI_CORE_COMMIT="ec5d85a3d709e5ed138a1668ebfefd366c05ca1e"` in [pack/gpustack-operator/Dockerfile](../pack/gpustack-operator/Dockerfile).
  - `ubs-virt`: `kind: git-commit`, `source: https://gitcode.com/openeuler/ubs-virt.git`, `watchedPaths: [ubs-virt-enpu/vcann-rt]`. Currently pinned at `ARG LIB_UBS_VIRT_ENPU_COMMIT="19a817e58e4c68bbfef6f4e2326e4877db9fc5f3"` in [pack/gpustack-operator/Dockerfile](../pack/gpustack-operator/Dockerfile).
- **Local scripts and patches:**
  - [build-libvgpu.sh](../pack/gpustack-operator/external/nvidia/build-libvgpu.sh): builds `libvgpu.so` in `xbuild-nvidia-cuda-12` and `xbuild-nvidia-cuda-13`.
  - [build-libvnpu.sh](../pack/gpustack-operator/external/ascend/build-libvnpu.sh): compiles stub `libdcmi.so` from `test/stub/dcmi_stub.c`, applies `/vcann-rt-patches/*.patch` with `patch -p1 --batch --forward`, and builds `libvruntime.so` and `enpu-monitor` across the six `xbuild-ascend-cann-*` stages (`8-910b`, `8-910c`, `9-910b`, `9-910c`, `9-950`, `9-310p`).
  - [0001-dsmi-get-hbm-info-slice-view.patch](../pack/gpustack-operator/external/ascend/vcann-rt/0001-dsmi-get-hbm-info-slice-view.patch): adds `src/dsmi/dsmi_hook.c` (`dsmi_get_hbm_info`) and hooks `CMakeLists.txt` (which uses CRLF line endings preserved by `.gitattributes`).

#### Preflight and Hardware-Free Verification (`validate_slicing.sh`)

1. `.auto-sync/scripts/preflight-slicing.sh` clones `Project-HAMi/HAMi-core` and
   `https://gitcode.com/openeuler/ubs-virt.git` into `$AUTOSYNC_STAGE_DIR` (`/tmp/gpustack-autosync-stage/slicing`), tests `patch -p1 --dry-run` of [0001-dsmi-get-hbm-info-slice-view.patch](../pack/gpustack-operator/external/ascend/vcann-rt/0001-dsmi-get-hbm-info-slice-view.patch), and writes `upstream-records.json` + `slicing-preflight-report.json`.
2. `.auto-sync/checks/validate_slicing.sh` enforces:
   - Shared-file diff guard on [pack/gpustack-operator/Dockerfile](../pack/gpustack-operator/Dockerfile) allowing changes only to `LIB_UBS_VIRT_ENPU_COMMIT` and `LIB_HAMI_CORE_COMMIT`.
   - Dry-run patch check (`patch -p1 --dry-run --batch --forward`) against `$AUTOSYNC_STAGE_DIR/ubs-virt/ubs-virt-enpu/vcann-rt` at the target `LIB_UBS_VIRT_ENPU_COMMIT`.
   - Hardware-free Docker multi-stage compilation and ELF symbol verification via [gpustack-operator-xbuild-and-verify](../.agents/skills/gpustack-operator-xbuild-and-verify/SKILL.md):
     - When `LIB_HAMI_CORE_COMMIT` or `external/nvidia/**` changes: export `XB_STAGE="${TMPDIR:-/tmp}/xb-stage-nvidia"` (avoiding non-root `/opt/vgpu` permission errors) and run
       `XB_MODE=local bash .agents/skills/gpustack-operator-xbuild-and-verify/scripts/build.sh xbuild-nvidia-cuda-12` and `xbuild-nvidia-cuda-13`, followed by
       [nvidia-case-1.sh](../.agents/skills/gpustack-operator-xbuild-and-verify/cases/nvidia-case-1.sh) (`0644` mode, `X86-64` ELF class, `NEEDED` `libcuda.so.1` and `libnvidia-ml.so.1`) and
       [nvidia-case-4.sh](../.agents/skills/gpustack-operator-xbuild-and-verify/cases/nvidia-case-4.sh) (Part A `.rodata` POSIX record-lock check and Part B CPU-only `test_postinit_owner_death` `SIGKILL` lock recovery test; Part C automatically skips with `FAILS=0` when no physical GPU is present).
     - When `LIB_UBS_VIRT_ENPU_COMMIT` or `external/ascend/**` changes: export `XB_STAGE="${TMPDIR:-/tmp}/xb-stage-ascend"` (avoiding non-root `/opt/enpu/vcann-rt` permission errors) and run
       `XB_MODE=local bash .agents/skills/gpustack-operator-xbuild-and-verify/scripts/build.sh xbuild-ascend-cann-8-910b` and `xbuild-ascend-cann-9-910b`, followed by
       [ascend-case-1.sh](../.agents/skills/gpustack-operator-xbuild-and-verify/cases/ascend-case-1.sh) (verifies `libvruntime.so` and `enpu-monitor`, `NEEDED` includes `libc_sec.so` and excludes `libascendcl.so`, `0` defined `dcmi_*` symbols, `~80` exported `rt*`/`rts*` symbols, and `dsmi_get_hbm_info` exported as a defined `GLOBAL DEFAULT FUNC`).

#### Manifest: `.auto-sync/slicing-taskpack.yaml`

```yaml
apiVersion: loopwright.dev/v1alpha1
kind: Taskpack
metadata:
  name: slicing-sync
spec:
  goal:
    candidate: Update LIB_HAMI_CORE_COMMIT and LIB_UBS_VIRT_ENPU_COMMIT in pack/gpustack-operator/Dockerfile, rebase Ascend vcann-rt patches if needed, and pass xbuild ELF verification.
    noChange: Confirm HAMi-core and ubs-virt commits match pack/gpustack-operator/Dockerfile and settle as no_change.
  scope:
    writable:
      - pack/gpustack-operator/Dockerfile
      - pack/gpustack-operator/external/nvidia
      - pack/gpustack-operator/external/ascend
      - .auto-sync/proposals/slicing.json
    protected:
      - pack/gpustack-operator/external/amd
      - pack/gpustack-operator/external/thead
      - csrc
      - binding
      - gen/binding
      - pkg/devicemanager/allocator
  instructions:
    - .auto-sync/instructions/slicing.md
  upstreams:
    - id: hami-core
      kind: git-commit
      source: Project-HAMi/HAMi-core
      watchedPaths:
        - src
        - include
        - CMakeLists.txt
        - build.sh
    - id: ubs-virt
      kind: git-commit
      source: https://gitcode.com/openeuler/ubs-virt.git
      watchedPaths:
        - ubs-virt-enpu/vcann-rt
  proposal:
    path: .auto-sync/proposals/slicing.json
    requiredFields:
      - schema_version
      - summary
      - versions
      - subscriptions
    schemaVersion: 1
  verification:
    localChecks:
      - id: proposal-schema
        implementation: json-artifact.v1
      - id: scope-guard
        implementation: scope.v1
      - id: validate-slicing
        implementation: command.v1
        parameters:
          script: .auto-sync/checks/validate_slicing.sh
          timeoutSeconds: 2400
          allowReadOnlyGitHubToken: false
          env:
            - AUTOSYNC_STAGE_DIR
    externalGates: []
  publication:
    mode: pull_request
    targetBranch: main
    headBranchPrefix: auto-sync-slicing/
    titleTemplate: "chore(pack): sync accelerator slicing shims"
  memory:
    directory: .auto-sync/memory/slicing-sync
    apply: separate-pull-request
    onNoModelBudget: deterministic-observations-only
  triggers:
    schedule: true
    workflowDispatch: true
    releasePublished: false
    commentCommands:
      - retry
      - revise
      - status
    commentPrefixes:
      - /loopwright
      - /auto-sync
  limits:
    wallTimeSeconds: 3600
    commandTimeoutSeconds: 2400
```

### Domain 2: KV Cache and Weight Transfer Data Plane (`kv-transfer-sync`)

#### Upstreams, Tracked Files, and `gpustack/runner` Parity

- **Upstreams:**
  - `mooncake`: `kind: github-release`, `source: kvcache-ai/Mooncake`, `allowPrerelease: false`. Pinned at `ARG MOONCAKE_VERSION="0.3.13.post1"` in [pack/mirrored-mooncake/Dockerfile](../pack/mirrored-mooncake/Dockerfile) and `KVCacheBackendImage = "gpustack/mirrored-mooncake:0.3.13.post1-cpu"` in [value.go](../pkg/worker/settings/value.go).
  - `nixl-pypi`: `kind: http-json`, `source: https://pypi.org/pypi/nixl/json` (normalized by `preflight-kv-transfer.sh` to verify `nixl`, `nixl-cu12`, and `nixl-cu13` wheels exist on PyPI for `linux/x86_64` and `linux/aarch64`). Pinned at `ARG NIXL_VERSION="1.5.2"` in [pack/model-transfer/Dockerfile](../pack/model-transfer/Dockerfile).
  - `nixl-rocm`: `kind: git-commit`, `source: ai-dynamo/nixl`. Pinned at `ARG NIXL_ROCM_VERSION="1.5.0"` and `ARG NIXL_ROCM_REF="1683cf3b7f3d11674c03c5e861cea22339876c96"` in [pack/model-transfer/Dockerfile](../pack/model-transfer/Dockerfile).
  - `ucx-rocm`: `kind: git-commit`, `source: openucx/ucx`. Pinned at `ARG NIXL_ROCM_UCX_REF="2ff0b47ce7626584cb9b56c647d08f712c6c3edf"` in [pack/model-transfer/Dockerfile](../pack/model-transfer/Dockerfile).
  - `hixl`: `kind: git-commit`, `source: hicann/hixl`. Pinned at `ARG HIXL_COMMIT="60dbe9d1d7d9a7eef04bc3dbf84edbad2188230a"` and `ARG HIXL_CMAKE_COMMIT="3c4a9da4a0e81faa47dcc1c8e11db63ec7ff6231"` in [pack/model-transfer/Dockerfile](../pack/model-transfer/Dockerfile), patched by [001_channel_generation.patch](../pack/model-transfer/patches/hixl/001_channel_generation.patch).
  - `modelexpress`: `kind: git-commit`, `source: ai-dynamo/modelexpress`. Pinned at `ARG MODELEXPRESS_COMMIT=ac6d03a87a550c8188c3ed59f187b90d4f0554f5` in [pack/mirrored-modelexpress-server/Dockerfile](../pack/mirrored-modelexpress-server/Dockerfile) and `ModelDeploymentModelExpressServerImage = "gpustack/mirrored-modelexpress-server:v0.7.0"` in [value.go](../pkg/worker/settings/value.go).
  - `gpustack-runner`: `kind: git-commit`, `source: gpustack/runner`, `watchedPaths: [pack/cuda/Dockerfile.vllm, pack/rocm/Dockerfile.vllm, pack/cann/Dockerfile.vllm, pack/cann/patches/hixl]`.
- **Mandatory invariants enforced by `validate_kv_transfer.sh`:**
  1. **Five-block `MOONCAKE_VERSION` `case` guard:** [pack/mirrored-mooncake/Dockerfile](../pack/mirrored-mooncake/Dockerfile) contains four explicit release assertions at lines 208, 474, 922, and 1401 (`case "${MOONCAKE_VERSION}" in 0.3.10*|0.3.11*|0.3.12*|0.3.13*) ;; *) exit 1 ;; esac`) plus a fifth `0.3.13*` conditional `sed` patch block at line 1022 (`context_manager.cpp`). When bumping `MOONCAKE_VERSION` to a new series (such as `0.3.14`), all five `case` blocks must be audited and updated in lockstep; `validate_kv_transfer.sh` extracts and validates all `case "${MOONCAKE_VERSION}"` blocks against `MOONCAKE_VERSION`.
  2. **Byte-level HIXL parity with `gpustack/runner`:** `HIXL_COMMIT` in [pack/model-transfer/Dockerfile](../pack/model-transfer/Dockerfile) and [001_channel_generation.patch](../pack/model-transfer/patches/hixl/001_channel_generation.patch) must match `VLLM_HIXL_COMMIT` and `pack/cann/patches/hixl/001_channel_generation.patch` in `gpustack/runner`, and `patch --dry-run --batch --fuzz=0 -p1` must succeed against the staged `hicann/hixl` tree.
  3. **ModelExpress `--locked` cargo check:** When `MODELEXPRESS_COMMIT` changes, `validate_kv_transfer.sh` runs `docker buildx build -f pack/mirrored-modelexpress-server/Dockerfile --target builder --platform linux/amd64` (or `cargo check --locked --bin modelexpress-server --features memory-backend` against the staged clone).
  4. **Two-stage image tag gate:** Any change to `KVCacheBackendImage` or `ModelDeploymentModelExpressServerImage` in [value.go](../pkg/worker/settings/value.go) or `modelTransfer.images.*` in [values.yaml](../deploy/gpustack-operator/chart/values.yaml) must pass `crane manifest`, `make generate chart`, `make lint chart`, `make lint docs`, and `go test ./pkg/worker/settings/...`.

#### Manifest: `.auto-sync/kv-transfer-taskpack.yaml`

```yaml
apiVersion: loopwright.dev/v1alpha1
kind: Taskpack
metadata:
  name: kv-transfer-sync
spec:
  goal:
    candidate: Synchronize Mooncake, NIXL, UCX, HIXL, and ModelExpress recipes with upstream and gpustack/runner, or promote published image tags verified by crane manifest.
    noChange: Confirm KV-cache and model-transfer recipes and published image tags are aligned and settle as no_change.
  scope:
    writable:
      - pack/mirrored-mooncake
      - pack/model-transfer
      - pack/mirrored-modelexpress-server
      - pkg/worker/settings/value.go
      - docs/reference/settings.md
      - deploy/gpustack-operator/chart/values.yaml
      - deploy/gpustack-operator/chart/values.schema.json
      - deploy/gpustack-operator/chart/README.md
      - .auto-sync/proposals/kv-transfer.json
    protected:
      - model-transfer/python
      - pkg/modeltransfer
      - pkg/worker/kvcache
  instructions:
    - .auto-sync/instructions/kv-transfer.md
  upstreams:
    - id: mooncake
      kind: github-release
      source: kvcache-ai/Mooncake
      watchedPaths: []
      allowPrerelease: false
    - id: nixl-pypi
      kind: http-json
      source: https://pypi.org/pypi/nixl/json
      watchedPaths: []
    - id: nixl-rocm
      kind: git-commit
      source: ai-dynamo/nixl
      watchedPaths:
        - pyproject.toml
        - src
    - id: ucx-rocm
      kind: git-commit
      source: openucx/ucx
      watchedPaths:
        - src
        - configure.ac
    - id: hixl
      kind: git-commit
      source: hicann/hixl
      watchedPaths:
        - src
        - include
        - CMakeLists.txt
    - id: modelexpress
      kind: git-commit
      source: ai-dynamo/modelexpress
      watchedPaths:
        - Cargo.toml
        - Cargo.lock
        - modelexpress_server
    - id: gpustack-runner
      kind: git-commit
      source: gpustack/runner
      watchedPaths:
        - pack/cuda/Dockerfile.vllm
        - pack/rocm/Dockerfile.vllm
        - pack/cann/Dockerfile.vllm
        - pack/cann/patches/hixl
  proposal:
    path: .auto-sync/proposals/kv-transfer.json
    requiredFields:
      - schema_version
      - stage
      - summary
      - versions
      - subscriptions
    schemaVersion: 1
  verification:
    localChecks:
      - id: proposal-schema
        implementation: json-artifact.v1
      - id: scope-guard
        implementation: scope.v1
      - id: validate-kv-transfer
        implementation: command.v1
        parameters:
          script: .auto-sync/checks/validate_kv_transfer.sh
          timeoutSeconds: 1800
          allowReadOnlyGitHubToken: false
          env:
            - AUTOSYNC_STAGE_DIR
    externalGates: []
  publication:
    mode: pull_request
    targetBranch: main
    headBranchPrefix: auto-sync-kv-transfer/
    titleTemplate: "chore(model-transfer): sync kv-cache and transfer runtime recipes"
  memory:
    directory: .auto-sync/memory/kv-transfer-sync
    apply: separate-pull-request
    onNoModelBudget: deterministic-observations-only
  triggers:
    schedule: true
    workflowDispatch: true
    releasePublished: false
    commentCommands:
      - retry
      - revise
      - status
    commentPrefixes:
      - /loopwright
      - /auto-sync
  limits:
    wallTimeSeconds: 3600
    commandTimeoutSeconds: 1800
```

### Domain 3: Disaggregated Inference Routers (`router-sync`)

#### Upstreams, Patch Inventory, and Retirement Rules

[pack/llm-router/Dockerfile](../pack/llm-router/Dockerfile) builds three independent router binaries
(`epp`, `vllm-router`, and `sgl-model-gateway`) into one runtime image (`gpustack/llm-router:v0.4.0`):

| Router Binary | Upstream Source | Current Pin in [pack/llm-router/Dockerfile](../pack/llm-router/Dockerfile) | Builder Stage | Local Patches (`pack/llm-router/patches/`) |
| --- | --- | --- | --- | --- |
| `epp` | `llm-d/llm-d-router` (`kind: git-tag`) | `ARG LLMD_ROUTER_VERSION=v0.11.0` | `epp-builder` (`golang:1.26-bookworm`, `CGO_ENABLED=0`) | [0001-observer-endpoints-and-dispatch.patch](../pack/llm-router/patches/llm-d/0001-observer-endpoints-and-dispatch.patch) |
| `vllm-router` | `vllm-project/router` (`kind: git-tag`) | `ARG VLLM_ROUTER_VERSION=v0.1.16.1` | `vllm-router-builder` (`rust:1.95-bookworm`, `cargo build --release --locked --bin vllm-router`) | [0001-epd-routing.patch](../pack/llm-router/patches/vllm-router/0001-epd-routing.patch), [0002-mooncake-lazy-bootstrap-lookup.patch](../pack/llm-router/patches/vllm-router/0002-mooncake-lazy-bootstrap-lookup.patch), [0003-observer-read-only-endpoint-view.patch](../pack/llm-router/patches/vllm-router/0003-observer-read-only-endpoint-view.patch), [0004-discovery-readiness-and-deselect-removal.patch](../pack/llm-router/patches/vllm-router/0004-discovery-readiness-and-deselect-removal.patch) |
| `sgl-model-gateway` | `sgl-project/sglang` (`kind: git-tag`, filtered to `gateway-v*` by preflight) | `ARG SGLANG_GATEWAY_VERSION=gateway-v0.3.1` | `sglang-gateway-builder` (`rust:1.95-bookworm`, `cargo build --release --bin sgl-model-gateway`) | [0001-discovery-readiness-and-deselect-removal.patch](../pack/llm-router/patches/sglang-gateway/0001-discovery-readiness-and-deselect-removal.patch), [0002-observer-read-only-endpoint-view.patch](../pack/llm-router/patches/sglang-gateway/0002-observer-read-only-endpoint-view.patch), [0003-attn-dp-aware-worker-registration.patch](../pack/llm-router/patches/sglang-gateway/0003-attn-dp-aware-worker-registration.patch) |

- **Tag prefix normalization for `sgl-project/sglang`:** Because `sgl-project/sglang` publishes both
  engine tags (`v0.4.*`) and router tags (`gateway-v0.3.*`), `.auto-sync/scripts/preflight-router.sh`
  runs `git ls-remote --tags --refs https://github.com/sgl-project/sglang.git 'refs/tags/gateway-v*'`
  and passes the highest stable `gateway-v*` tag to Loopwright via `--upstream-records-json`.
- **Automatic retirement rule for `vllm-router/0001-epd-routing.patch`:** As documented at
  [pack/llm-router/Dockerfile](../pack/llm-router/Dockerfile#L162-L173),
  [0001-epd-routing.patch](../pack/llm-router/patches/vllm-router/0001-epd-routing.patch) is a
  cherry-pick of upstream PR #266 (commit `516f1cb`) onto `v0.1.16.1`. `preflight-router.sh` checks
  `git -C "$AUTOSYNC_STAGE_DIR/vllm-router" merge-base --is-ancestor 516f1cb "$TARGET_TAG"`. When
  true (for example on `v0.1.17`+), the preflight report sets `retire_0001_epd_routing: true`,
  instructing the agent to delete
  [0001-epd-routing.patch](../pack/llm-router/patches/vllm-router/0001-epd-routing.patch) and rebase
  patches `0002`–`0004` directly onto the new tag.
- **Verification (`validate_router.sh`):**
  1. Verifies `git apply --check` on every remaining patch in `pack/llm-router/patches/{llm-d,vllm-router,sglang-gateway}/*.patch` against the staged upstream source trees in `$AUTOSYNC_STAGE_DIR`, and asserts `grep -q "struct ObserverRouter" sgl-model-gateway/src/observer.rs` after applying SGLang gateway patches.
  2. Runs `docker buildx build -f pack/llm-router/Dockerfile --platform linux/amd64 --target <stage>` for each modified router builder stage (`epp-builder`, `vllm-router-builder`, `sglang-gateway-builder`).
  3. Enforces the two-stage image gate on `ModelDeploymentRouterImage`, `ModelDeploymentRouterProxyImage`, and `ModelDeploymentRoutingSidecarImage` in [value.go](../pkg/worker/settings/value.go) and [settings.md](../docs/reference/settings.md).

#### Manifest: `.auto-sync/router-taskpack.yaml`

```yaml
apiVersion: loopwright.dev/v1alpha1
kind: Taskpack
metadata:
  name: router-sync
spec:
  goal:
    candidate: Update LLMD_ROUTER_VERSION, VLLM_ROUTER_VERSION, and SGLANG_GATEWAY_VERSION in pack/llm-router/Dockerfile, rebase or retire router patches under pack/llm-router/patches, and pass builder compilation gates.
    noChange: Confirm llm-d-router, vllm-router, and sgl-model-gateway tags and published router images are up to date and settle as no_change.
  scope:
    writable:
      - pack/llm-router
      - pkg/worker/settings/value.go
      - docs/reference/settings.md
      - .auto-sync/proposals/router.json
    protected:
      - pkg/worker/controllers
      - pkg/worker/webhooks
  instructions:
    - .auto-sync/instructions/router.md
  upstreams:
    - id: llm-d-router
      kind: git-tag
      source: llm-d/llm-d-router
      watchedPaths: []
      allowPrerelease: false
    - id: vllm-router
      kind: git-tag
      source: vllm-project/router
      watchedPaths: []
      allowPrerelease: false
    - id: sglang-gateway
      kind: git-tag
      source: sgl-project/sglang
      watchedPaths:
        - sgl-model-gateway
      allowPrerelease: false
  proposal:
    path: .auto-sync/proposals/router.json
    requiredFields:
      - schema_version
      - stage
      - summary
      - versions
      - subscriptions
    schemaVersion: 1
  verification:
    localChecks:
      - id: proposal-schema
        implementation: json-artifact.v1
      - id: scope-guard
        implementation: scope.v1
      - id: validate-router
        implementation: command.v1
        parameters:
          script: .auto-sync/checks/validate_router.sh
          timeoutSeconds: 2400
          allowReadOnlyGitHubToken: false
          env:
            - AUTOSYNC_STAGE_DIR
    externalGates: []
  publication:
    mode: pull_request
    targetBranch: main
    headBranchPrefix: auto-sync-router/
    titleTemplate: "chore(llm-router): sync disaggregated router versions and patches"
  memory:
    directory: .auto-sync/memory/router-sync
    apply: separate-pull-request
    onNoModelBudget: deterministic-observations-only
  triggers:
    schedule: true
    workflowDispatch: true
    releasePublished: false
    commentCommands:
      - retry
      - revise
      - status
    commentPrefixes:
      - /loopwright
      - /auto-sync
  limits:
    wallTimeSeconds: 3600
    commandTimeoutSeconds: 2400
```

### Domain 4: Offline Capacity Prior Catalog (`priors-sync`)

#### Provenance Ledger Relocation and Upstream Discovery

Loopwright's `PROTECTED_AUTOMATION_PATHS` unconditionally blocks writes to `.agents/**`. Currently,
the capacity prior provenance ledger lives at
[records.md](../.agents/skills/gpustack-operator-priors/references/records.md). To allow `priors-sync`
to update both the embedded catalog and its SHA-256 provenance ledger in one atomic pull request while
keeping `.agents/**` strictly protected:

1. Move the canonical provenance table to `pkg/worker/scaling/priors/records.md`.
2. Replace [records.md](../.agents/skills/gpustack-operator-priors/references/records.md) with a
   concise pointer to `../../../../pkg/worker/scaling/priors/records.md`, update
   [SKILL.md](../.agents/skills/gpustack-operator-priors/SKILL.md) to link to
   `../../../pkg/worker/scaling/priors/records.md`, and update both references in
   [scaling.md](../docs/modules/model-deployment/scaling.md#L318) (lines 318 and 458).
3. `.auto-sync/scripts/preflight-priors.sh` tracks `SemiAnalysisAI/InferenceX` (`kind: git-commit`)
   and queries `https://inferencex.semianalysis.com/api/v1/availability` plus per-model
   `/api/v1/benchmarks?model=<query ID>`, storing the raw JSON snapshots, row counts, and `shasum -a 256`
   digests under `/tmp/gpustack-autosync-stage/priors/snapshots/` and pre-staging `go run ./gen/priors`
   onto a detached `source_revision` commit when qualifying catalog changes exist.
4. `.auto-sync/checks/validate_priors.sh` runs `go test ./gen/priors/... ./pkg/worker/scaling/...`
   and `make lint docs`, and verifies that every active query ID checksum in
   `pkg/worker/scaling/priors/records.md` matches the snapshot digest in `$AUTOSYNC_STAGE_DIR`.

#### Manifest: `.auto-sync/priors-taskpack.yaml`

```yaml
apiVersion: loopwright.dev/v1alpha1
kind: Taskpack
metadata:
  name: priors-sync
spec:
  goal:
    candidate: Refresh pkg/worker/scaling/priors/priors.yaml and pkg/worker/scaling/priors/records.md from staged InferenceX benchmark snapshots and pass scaling unit and docs checks.
    noChange: Confirm InferenceX benchmark snapshots produce no qualifying catalog changes and settle as no_change.
  scope:
    writable:
      - pkg/worker/scaling/priors/priors.yaml
      - pkg/worker/scaling/priors/records.md
      - .auto-sync/proposals/priors.json
    protected:
      - pkg/worker/scaling/priors/catalog.go
      - pkg/worker/scaling/priors/catalog_yaml.go
      - pkg/worker/scaling/priors/resolve.go
      - pkg/worker/scaling/priors/validate.go
      - gen/priors
  instructions:
    - .auto-sync/instructions/priors.md
  upstreams:
    - id: inferencex-repo
      kind: git-commit
      source: SemiAnalysisAI/InferenceX
      watchedPaths: []
    - id: inferencex-availability
      kind: http-json
      source: https://inferencex.semianalysis.com/api/v1/availability
      watchedPaths: []
  proposal:
    path: .auto-sync/proposals/priors.json
    requiredFields:
      - schema_version
      - summary
      - versions
      - subscriptions
    schemaVersion: 1
  verification:
    localChecks:
      - id: proposal-schema
        implementation: json-artifact.v1
      - id: scope-guard
        implementation: scope.v1
      - id: validate-priors
        implementation: command.v1
        parameters:
          script: .auto-sync/checks/validate_priors.sh
          timeoutSeconds: 600
          allowReadOnlyGitHubToken: false
          env:
            - AUTOSYNC_STAGE_DIR
    externalGates: []
  publication:
    mode: pull_request
    targetBranch: main
    headBranchPrefix: auto-sync-priors/
    titleTemplate: "chore(scaling): sync InferenceX capacity priors"
  memory:
    directory: .auto-sync/memory/priors-sync
    apply: separate-pull-request
    onNoModelBudget: deterministic-observations-only
  triggers:
    schedule: true
    workflowDispatch: true
    releasePublished: false
    commentCommands:
      - retry
      - revise
      - status
    commentPrefixes:
      - /loopwright
      - /auto-sync
  limits:
    wallTimeSeconds: 1800
    commandTimeoutSeconds: 600
```

### Domain 5: Vendored Helm Subcharts and Mirrored Images (`charts-sync`)

#### Upstreams, Patches, and Mirrored Image Verification

`chart_staging()` in [hack/deps.sh](../hack/deps.sh#L64-L141) vendors five upstream Helm subcharts into
`deploy/gpustack-operator/chart/charts/<name>/`, applies 16 patches under
`hack/deploy/gpustack-operator/chart/charts/<name>/*.patch` using `patch -p1 -N --forward --silent -F0 --no-backup-if-mismatch`,
and configures 11 subchart `gpustack/mirrored-*` image entries in
[values.yaml](../deploy/gpustack-operator/chart/values.yaml):

| Subchart | Upstream Source | Current Version ([hack/deps.sh](../hack/deps.sh)) | Vendored Patches (`hack/deploy/gpustack-operator/chart/charts/`) | Mirrored Image Pins in [values.yaml](../deploy/gpustack-operator/chart/values.yaml) |
| --- | --- | --- | --- | --- |
| `kueue` | `kubernetes-sigs/kueue` (`github-release`) | `0.18.11` | `cert-manager.patch`, `global-image.patch`, `global-node-selector.patch`, `manager-config.patch`, `selectable-fields.patch` (5) | `gpustack/mirrored-kueue:v0.18.11` |
| `node-feature-discovery` | `kubernetes-sigs/node-feature-discovery` (`github-release`) | `0.19.0` | `global-image.patch`, `global-node-selector.patch` (2) | `gpustack/mirrored-node-feature-discovery:v0.19.0-gpustack1` |
| `csi-driver-nfs` | `kubernetes-csi/csi-driver-nfs` (`git-tag`) | `4.13.4` | `global-image.patch`, `global-node-selector.patch`, `topology-spread.patch` (3) | `gpustack/mirrored-csi-nfs-driver:v4.13.4`, `mirrored-csi-provisioner:v6.3.0`, `mirrored-csi-resizer:v2.2.0`, `mirrored-csi-snapshotter:v8.6.0`, `mirrored-csi-livenessprobe:v2.19.0`, `mirrored-csi-node-driver-registrar:v2.17.0` |
| `csi-driver-s3` | `thxcode/k8s-csi-s3` (`git-tag`) | `0.43.7` | `chart-rename.patch`, `feature-gates.patch`, `global-image.patch`, `global-node-selector.patch` (4) | `gpustack/mirrored-csi-s3-driver:v0.43.7` + shared `csiProvisioner`, `livenessProbe`, `nodeDriverRegistrar` |
| `topograph` | `NVIDIA/topograph` (`git-tag`) | `1.0.0` | `global-image.patch`, `notice.patch` (2) | `gpustack/mirrored-nvidia-topograph` (appVersion `1.0.0`) |

- **Preflight (`preflight-charts.sh`):** Downloads new upstream chart archives into
  `$AUTOSYNC_STAGE_DIR/charts/`, extracts upstream `values.yaml` and `Chart.yaml` image tags, and runs
  `crane manifest` against `docker.io/gpustack/mirrored-*` for every image tag required by the
  candidate chart version. If an upstream subchart references an unmirrored image tag, the preflight
  report marks that subchart `mirrored_image_ready: false` and lists the exact
  `gh workflow run sync-image.yml -f images="..."` command needed before the subchart can be bumped.
- **Verification (`validate_charts.sh`):**
  1. Verifies via `diff -U0 "$AUTOSYNC_STAGE_DIR/baseline/hack/deps.sh" "$LOOPWRIGHT_CANDIDATE_DIR/hack/deps.sh"` that only `chart_staging()` archive lines
     (lines 134–139) were modified.
  2. Runs `bash hack/deps.sh` (using cached archives when offline) and asserts zero `*.rej` or
     `*.orig` files and zero `*.tgz` archives under `deploy/gpustack-operator/chart/charts/`.
  3. Verifies via `crane manifest` (or preflight digest cache) that all 11 subchart
     `gpustack/mirrored-*` image references in [values.yaml](../deploy/gpustack-operator/chart/values.yaml)
     exist on Docker Hub.
  4. Runs `make generate chart`, `make lint chart`, and `go test ./pkg/worker/kuberess/...`
     (including `TestChartDefaultsMatchImageModeOverlay`), and asserts `git status --porcelain` has no
     unstaged drift after regeneration.

#### Manifest: `.auto-sync/charts-taskpack.yaml`

```yaml
apiVersion: loopwright.dev/v1alpha1
kind: Taskpack
metadata:
  name: charts-sync
spec:
  goal:
    candidate: Update vendored Helm subcharts in hack/deps.sh and deploy/gpustack-operator/chart, rebase chart patches under hack/deploy/gpustack-operator/chart/charts, and pass chart lint and kuberess tests.
    noChange: Confirm all vendored Helm subcharts and mirrored images are up to date and settle as no_change.
  scope:
    writable:
      - hack/deps.sh
      - hack/deploy/gpustack-operator/chart/charts
      - deploy/gpustack-operator/chart
      - pkg/worker/kuberess
      - .auto-sync/proposals/charts.json
    protected:
      - hack/staging
      - staging
  instructions:
    - .auto-sync/instructions/charts.md
  upstreams:
    - id: kueue-chart
      kind: github-release
      source: kubernetes-sigs/kueue
      watchedPaths: []
      allowPrerelease: false
    - id: nfd-chart
      kind: github-release
      source: kubernetes-sigs/node-feature-discovery
      watchedPaths: []
      allowPrerelease: false
    - id: csi-driver-nfs-chart
      kind: git-tag
      source: kubernetes-csi/csi-driver-nfs
      watchedPaths:
        - charts
      allowPrerelease: false
    - id: csi-driver-s3-chart
      kind: git-tag
      source: thxcode/k8s-csi-s3
      watchedPaths:
        - charts
      allowPrerelease: false
    - id: topograph-chart
      kind: git-tag
      source: NVIDIA/topograph
      watchedPaths:
        - charts
      allowPrerelease: false
  proposal:
    path: .auto-sync/proposals/charts.json
    requiredFields:
      - schema_version
      - summary
      - versions
      - subscriptions
    schemaVersion: 1
  verification:
    localChecks:
      - id: proposal-schema
        implementation: json-artifact.v1
      - id: scope-guard
        implementation: scope.v1
      - id: validate-charts
        implementation: command.v1
        parameters:
          script: .auto-sync/checks/validate_charts.sh
          timeoutSeconds: 1800
          allowReadOnlyGitHubToken: false
          env:
            - AUTOSYNC_STAGE_DIR
    externalGates: []
  publication:
    mode: pull_request
    targetBranch: main
    headBranchPrefix: auto-sync-charts/
    titleTemplate: "chore(chart): sync vendored helm subcharts"
  memory:
    directory: .auto-sync/memory/charts-sync
    apply: separate-pull-request
    onNoModelBudget: deterministic-observations-only
  triggers:
    schedule: true
    workflowDispatch: true
    releasePublished: false
    commentCommands:
      - retry
      - revise
      - status
    commentPrefixes:
      - /loopwright
      - /auto-sync
  limits:
    wallTimeSeconds: 3600
    commandTimeoutSeconds: 1800
```

### Domain 6: Kubernetes Lockstep Modules and CLI Toolchains (`k8s-deps-sync`)

#### Upstreams and Lockstep Compatibility Rules

`k8s-deps-sync` manages two tightly coupled dependency sets:

1. **Six CLI tool pins in [pack/gpustack-operator/Dockerfile](../pack/gpustack-operator/Dockerfile#L10-L15) and [hack/lib/helm.sh](../hack/lib/helm.sh#L14):**
   - `SKOPEO_VERSION="1.24.1"` (`podman-container-tools/skopeo`, `kind: github-release`)
   - `HELM_VERSION="3.21.4"` (`helm/helm`, `kind: github-release`, restricted to `v3.*` in lockstep with both the header comment on line 7 and `helm_version` on line 14 of [hack/lib/helm.sh](../hack/lib/helm.sh#L7-L14) plus `helm.sh/helm/v3` in [go.mod](../go.mod))
   - `VALS_VERSION="0.50.0"` (`helmfile/vals`, `kind: github-release`)
   - `KUSTOMIZE_VERSION="5.8.3"` (`kubernetes-sigs/kustomize`, monorepo tags `kustomize/v*` normalized by `preflight-k8s-deps.sh` via `upstream-records`)
   - `YTT_VERSION="0.55.4"` (`carvel-dev/ytt`, `kind: github-release`)
   - `KUBECTL_VERSION="1.36.5"` (`kubernetes/kubernetes`, `kind: github-release`, satisfying `|M - Y| <= 1` against `k8s.io/api` `v0.Y.Z` in [upgrade-compat.sh](../hack/check/upgrade-compat.sh))
2. **Kubernetes `v0.36.x` lockstep Go modules and `staging/` trees:**
   - [hack/deps.sh](../hack/deps.sh#L38-L45) stages `k8s.io/{code-generator,api,apimachinery,kube-aggregator,apiextensions-apiserver}` (`v0.36.5`), `k8s.io/klog` (`v2.140.0`), `github.com/go-logr/logr` (`v1.4.3`), and `github.com/gogo/protobuf` (`v1.3.2`) into `staging/`, applying the three patches under `hack/staging/` (`k8s.io/code-generator/001_enhanced.patch`, `k8s.io/klog/001_enhanced.patch`, `github.com/go-logr/logr/001_enhanced.patch`).
   - [go.mod](../go.mod) pins lockstep `k8s.io/*` (`v0.36.5`), `k8s.io/dynamic-resource-allocation` (`v0.36.2`), `sigs.k8s.io/controller-runtime` (`v0.24.1`), `sigs.k8s.io/kueue` (`v0.18.11`), and `helm.sh/helm/v3` (`v3.21.4`).
- **Verification (`validate_k8s_deps.sh`):**
  1. Enforces shared-file diff guards on [pack/gpustack-operator/Dockerfile](../pack/gpustack-operator/Dockerfile) (allowing only `SKOPEO_VERSION`, `HELM_VERSION`, `VALS_VERSION`, `KUSTOMIZE_VERSION`, `YTT_VERSION`, `KUBECTL_VERSION`) and [hack/deps.sh](../hack/deps.sh) (allowing only `mod_staging()` lines 38–45).
  2. Enforces that any modified files under `api/` are strictly codegen outputs (`zz_generated*.go`, `generated.pb.go`, `generated.proto`).
  3. Runs `bash hack/check/upgrade-compat.sh modules` and `bash hack/check/toolpins-selftest.sh`.
  4. Runs `make generate`, `make lint`, `make generate` (to ensure `goimports-reviser` did not leave stale generated files), `git diff --exit-code` after regeneration, and `RACE=false make test`.

#### Manifest: `.auto-sync/k8s-deps-taskpack.yaml`

```yaml
apiVersion: loopwright.dev/v1alpha1
kind: Taskpack
metadata:
  name: k8s-deps-sync
spec:
  goal:
    candidate: Update CLI tool versions in pack/gpustack-operator/Dockerfile and hack/lib/helm.sh, and update Kubernetes v0.36.x lockstep Go modules and staging trees in hack/deps.sh and go.mod while passing upgrade-compat, toolpins-selftest, make generate, make lint, and make test.
    noChange: Confirm CLI tool pins and Kubernetes Go modules match current versions and settle as no_change.
  scope:
    writable:
      - pack/gpustack-operator/Dockerfile
      - hack/deps.sh
      - hack/lib/helm.sh
      - hack/staging
      - staging
      - go.mod
      - go.sum
      - api
      - pkg
      - .auto-sync/proposals/k8s-deps.json
    protected:
      - pkg/devicemanager/allocator
      - pkg/worker/scaling/priors/priors.yaml
      - pkg/worker/scaling/priors/records.md
      - pkg/worker/settings/value.go
      - csrc
      - binding
      - gen/binding
  instructions:
    - .auto-sync/instructions/k8s-deps.md
  upstreams:
    - id: kubernetes
      kind: github-release
      source: kubernetes/kubernetes
      watchedPaths: []
      allowPrerelease: false
    - id: controller-runtime
      kind: github-release
      source: kubernetes-sigs/controller-runtime
      watchedPaths: []
      allowPrerelease: false
    - id: kueue-sdk
      kind: github-release
      source: kubernetes-sigs/kueue
      watchedPaths: []
      allowPrerelease: false
    - id: helm-cli
      kind: github-release
      source: helm/helm
      watchedPaths: []
      allowPrerelease: false
    - id: skopeo-cli
      kind: github-release
      source: podman-container-tools/skopeo
      watchedPaths: []
      allowPrerelease: false
    - id: vals-cli
      kind: github-release
      source: helmfile/vals
      watchedPaths: []
      allowPrerelease: false
    - id: kustomize-cli
      kind: git-tag
      source: kubernetes-sigs/kustomize
      watchedPaths:
        - kustomize
      allowPrerelease: false
    - id: ytt-cli
      kind: github-release
      source: carvel-dev/ytt
      watchedPaths: []
      allowPrerelease: false
  proposal:
    path: .auto-sync/proposals/k8s-deps.json
    requiredFields:
      - schema_version
      - summary
      - versions
      - subscriptions
    schemaVersion: 1
  verification:
    localChecks:
      - id: proposal-schema
        implementation: json-artifact.v1
      - id: scope-guard
        implementation: scope.v1
      - id: validate-k8s-deps
        implementation: command.v1
        parameters:
          script: .auto-sync/checks/validate_k8s_deps.sh
          timeoutSeconds: 3600
          allowReadOnlyGitHubToken: false
          env:
            - AUTOSYNC_STAGE_DIR
    externalGates: []
  publication:
    mode: pull_request
    targetBranch: main
    headBranchPrefix: auto-sync-k8s-deps/
    titleTemplate: "chore(deps): sync kubernetes modules and cli toolchains"
  memory:
    directory: .auto-sync/memory/k8s-deps-sync
    apply: separate-pull-request
    onNoModelBudget: deterministic-observations-only
  triggers:
    schedule: true
    workflowDispatch: true
    releasePublished: false
    commentCommands:
      - retry
      - revise
      - status
    commentPrefixes:
      - /loopwright
      - /auto-sync
  limits:
    wallTimeSeconds: 3600
    commandTimeoutSeconds: 3600
```

### Two-Stage Image Publication Gate

Auxiliary images defined under `pack/mirrored-mooncake`, `pack/model-transfer`,
`pack/mirrored-modelexpress-server`, and `pack/llm-router` are built and pushed to Docker Hub by
manual `workflow_dispatch` runs of [base-image.yml](../.github/workflows/base-image.yml) (or mirrored
by [sync-image.yml](../.github/workflows/sync-image.yml)), never automatically during PR CI. Writing an
unbuilt tag into [value.go](../pkg/worker/settings/value.go) or
[values.yaml](../deploy/gpustack-operator/chart/values.yaml) in the same commit that updates
`pack/*/Dockerfile` breaks `main` with `ImagePullBackOff` during the window before `base-image.yml`
completes.

```mermaid
sequenceDiagram
    participant Up as Upstream / gpustack/runner
    participant Pre as preflight-{kv-transfer,router}.sh
    participant LW as Loopwright Agent + command.v1
    participant Main as gpustack-operator main
    participant Hub as base-image.yml / Docker Hub

    Up->>Pre: Run 1: New upstream commit/release detected
    Pre->>Hub: crane manifest docker.io/gpustack/<image>:<new-tag> (404 Not Found)
    Pre->>LW: Emit upstream-records.json + stage="recipe_sync" (image_published=false)
    LW->>Main: Open Stage A PR (updates pack/**/Dockerfile & patches ONLY; value.go unchanged)
    Main->>Hub: Maintainer merges Stage A PR & dispatches base-image.yml / sync-image.yml
    Hub-->>Hub: Publishes linux/amd64 + linux/arm64 manifest list
    Up->>Pre: Run 2: Next schedule / workflow_dispatch
    Pre->>Hub: crane manifest docker.io/gpustack/<image>:<new-tag> (200 OK, multi-arch verified)
    Pre->>LW: Emit stage="tag_promotion" (image_published=true, value.go still on old tag)
    LW->>Main: Open Stage B PR (promotes value.go, docs/reference/settings.md, chart/values.yaml)
```

Both `validate_kv_transfer.sh` and `validate_router.sh` enforce this gate mechanically:

1. Extract every `gpustack/*` default image string from [value.go](../pkg/worker/settings/value.go)
   and `modelTransfer.images.*` from [values.yaml](../deploy/gpustack-operator/chart/values.yaml).
2. Verify each image tag against the signed preflight manifest report in
   `$AUTOSYNC_STAGE_DIR/image-manifests.json` (which records `crane manifest --platform linux/amd64`
   and `linux/arm64` digests). If any referenced image tag is missing or unpublished, `command.v1`
   exits non-zero.

### Shared-File Variable-Level Diff Guards

Because Loopwright's `scope.v1` operates at file-path granularity and `command.v1` executes inside a
gitless `LOOPWRIGHT_CANDIDATE_DIR`, each `preflight-<domain>.sh` script snapshots baseline copies of
shared files into `$AUTOSYNC_STAGE_DIR/baseline/`. Each domain's `command.v1` check script runs
`diff -U0 -- "$AUTOSYNC_STAGE_DIR/baseline/<shared-file>" "$LOOPWRIGHT_CANDIDATE_DIR/<shared-file>"`
(checking `diff` exit codes `0`, `1`, and `>=2` explicitly without `|| true`) and rejects any modified
(`+`/`-`) line outside its allowlisted pattern:

| Shared File | Domain A & Allowed Diff Pattern | Domain B & Allowed Diff Pattern |
| --- | --- | --- |
| [pack/gpustack-operator/Dockerfile](../pack/gpustack-operator/Dockerfile) | **`slicing-sync`:** Only `^[-+]ARG LIB_(UBS_VIRT_ENPU|HAMI_CORE)_COMMIT=` | **`k8s-deps-sync`:** Only `^[-+]ARG (SKOPEO|HELM|VALS|KUSTOMIZE|YTT|KUBECTL)_VERSION=` |
| [pkg/worker/settings/value.go](../pkg/worker/settings/value.go) and [docs/reference/settings.md](../docs/reference/settings.md) | **`kv-transfer-sync`:** Only `KVCacheBackendImage` (`kv-cache-backend-image`) and `ModelDeploymentModelExpressServerImage` (`model-deployment-modelexpress-server-image`) default image literals | **`router-sync`:** Only `ModelDeploymentRouterImage` (`model-deployment-router-image`), `ModelDeploymentRouterProxyImage` (`model-deployment-router-proxy-image`), and `ModelDeploymentRoutingSidecarImage` (`model-deployment-routing-sidecar-image`) default image literals |
| [hack/deps.sh](../hack/deps.sh) | **`charts-sync`:** Only lines inside the `chart_staging()` heredoc (`${charts_dir}/(kueue\|node-feature-discovery\|csi-driver-nfs\|csi-driver-s3\|topograph)`) | **`k8s-deps-sync`:** Only lines inside the `mod_staging()` heredoc (`${staging_dir}/(k8s\.io\|github\.com)/`) |

### Unified GitHub Actions Matrix, Host Preflight Staging, and Comment Routing

A single workflow `.github/workflows/auto-sync.yml` orchestrates all six domains:

1. **Trigger resolution (`resolve-domains` job):**
   - On `schedule`: maps the cron expression to the scheduled domain subset:
     - `0 1 * * 1` (Mon `01:00 UTC`) -> `["priors"]`
     - `0 2 * * 1` (Mon `02:00 UTC`) -> `["slicing"]`
     - `0 2 * * 2` (Tue `02:00 UTC`) -> `["kv-transfer"]`
     - `0 3 * * 2` (Tue `03:00 UTC`) -> `["router"]`
     - `0 2 * * 3` (Wed `02:00 UTC`) -> `["charts"]`
     - `0 2 * * 4` (Thu `02:00 UTC`) -> `["k8s-deps"]`
   - On `workflow_dispatch`: reads `inputs.domain` (`all | slicing | kv-transfer | router | priors | charts | k8s-deps`), `inputs.force`, `inputs.dry-run`, and `inputs.runner_profile` (defaulting to `8x` for the 4x-resource `ubuntu-24.04-8x` runner matching [ci.yml](../.github/workflows/ci.yml#L128-L138)).
   - On `issue_comment` (`created` on an open pull request starting with `/loopwright` or `/auto-sync` by an `OWNER`, `MEMBER`, or `COLLABORATOR`):
     checks out `main` (preserving local `refs/heads/main`), queries the pull request's `head.ref` via `gh pr view ${{ github.event.issue.number }} --json headRefName -q .headRefName`, matches `^auto-sync-(slicing|kv-transfer|router|priors|charts|k8s-deps)/main$`, and routes the comment command (`retry`, `revise`, or `status`) exclusively to that domain's `.auto-sync/<domain>-taskpack.yaml`.
2. **Per-domain execution (`auto-sync` matrix job on `ubuntu-24.04-8x`, `fail-fast: false`):**
   - Sets `AUTOSYNC_STAGE_DIR=/tmp/gpustack-autosync-stage/${{ matrix.domain }}`.
   - Runs `bash .auto-sync/scripts/preflight-${{ matrix.domain }}.sh "$AUTOSYNC_STAGE_DIR"`, which snapshots baseline shared files, warms `/tmp/gpustack-autosync-stage/{.sbin,go-cache,go-mod}`, writes `upstream-records.json`, and (for bulk-generated domains `priors`, `charts`, `k8s-deps`) materializes deterministic generator outputs onto a detached `source_revision` commit while keeping `refs/heads/main` untouched.
   - Runs `python3 .auto-sync/scripts/write_models_json.py` to resolve organization LLM gateway settings (`CI_GPUSTACK_LOOPWRIGHT_LLM_*` / `CI_GPUSTACK_CODEREVIEW_LLM_*`).
   - Invokes `gpustack/loopwright@main` with exact [action.yml](https://github.com/gpustack/loopwright/blob/main/action.yml) inputs:
     - `taskpack-path: .auto-sync/${{ matrix.domain }}-taskpack.yaml`
     - `upstream-records: ${{ steps.preflight.outputs.upstream_records }}`
     - `source-revision: ${{ steps.preflight.outputs.source_revision }}`
     - `event-name: ${{ github.event_name }}`
     - `event-path: ${{ github.event_path }}`
     - `provider: ${{ steps.model-config.outputs.provider }}`
     - `model: ${{ steps.model-config.outputs.model }}`
     - `thinking: ${{ steps.model-config.outputs.thinking }}`
     - `api: ${{ steps.model-config.outputs.api }}`
     - `base-url: ${{ steps.model-config.outputs.base-url }}`
     - `models-json: ${{ steps.model-config.outputs.models-json }}`
     - `allow-custom-base-url: 'true'`
     - `provider-token-env: ${{ steps.model-config.outputs.provider-token-env }}`
     - `github-app-id: ${{ secrets.CI_GPUSTACK_LOOPWRIGHT_APP_ID || vars.CI_GPUSTACK_LOOPWRIGHT_APP_ID }}`
     - `github-app-private-key: ${{ secrets.CI_GPUSTACK_LOOPWRIGHT_APP_PRIVATE_KEY }}`
     - `dry-run: ${{ inputs.dry-run && 'true' || 'false' }}`
     - `output: json,github-step-summary`

### Three-Phase Rollout Criteria

| Phase | Target Domains | Entry Prerequisites | Exit & Acceptance Criteria |
| --- | --- | --- | --- |
| **Phase 1: Isolated Catalog & Slicing Shims** | `priors-sync`, `slicing-sync` | 1. Relocate provenance ledger to `pkg/worker/scaling/priors/records.md`.<br/>2. Land `.auto-sync/{priors,slicing}-taskpack.yaml`, instructions, preflight scripts, and `validate_{priors,slicing}.sh`. | 1. `priors-sync` completes a scheduled or dispatched run, passing `go test ./gen/priors/... ./pkg/worker/scaling/...` and `make lint docs`.<br/>2. `slicing-sync` compiles `xbuild-nvidia-cuda-{12,13}` and `xbuild-ascend-cann-{8,9}-910b` on `ubuntu-24.04-8x` with non-root `XB_STAGE` and passes `nvidia-case-1.sh`, `nvidia-case-4.sh` (Parts A/B), and `ascend-case-1.sh`.<br/>3. Memory branches `loopwright/memory/{priors-sync,slicing-sync}/main` publish cleanly. |
| **Phase 2: Data-Plane & Router Two-Stage Flows** | `kv-transfer-sync`, `router-sync` | 1. Phase 1 exit criteria met.<br/>2. Land `preflight-{kv-transfer,router}.sh` with `gpustack/runner` parity checks, PR #266 (`516f1cb`) ancestor detection, and `crane manifest` probes. | 1. `kv-transfer-sync` enforces all 5 `MOONCAKE_VERSION` `case` blocks, PyPI `nixl`/`nixl-cu12`/`nixl-cu13` wheel parity, and HIXL patch parity with `gpustack/runner`, opening Stage A (`pack/` only) when image tags are unpublished and Stage B (`value.go` / `modelTransfer.images.*`) only after `crane manifest` succeeds.<br/>2. `router-sync` retires `0001-epd-routing.patch` when `vllm-router` advances past `516f1cb` and passes Docker builder compilation for all three routers. |
| **Phase 3: Vendored Charts & K8s Lockstep Modules** | `charts-sync`, `k8s-deps-sync` | 1. Phase 2 exit criteria met and `gpustack/loopwright` `source-revision` input landed.<br/>2. Land `preflight-{charts,k8s-deps}.sh` and `validate_{charts,k8s_deps}.sh` with `diff -U0` shared-file guards on `hack/deps.sh` and `pack/gpustack-operator/Dockerfile`. | 1. `charts-sync` verifies `crane manifest` on all 11 subchart `gpustack/mirrored-*` image entries and passes `make deps`, `make generate chart`, `make lint chart`, and `go test ./pkg/worker/kuberess/...`.<br/>2. `k8s-deps-sync` passes `bash hack/check/upgrade-compat.sh modules`, `bash hack/check/toolpins-selftest.sh`, double `make generate` + `make lint`, and `RACE=false make test`. |

### Commands

Execution environments:
- **Local authoring & self-test environment:** contributor workstation (`go 1.27.3`, `docker buildx`, pinned `.sbin` tools) for Taskpack schema validation, gitless diff-guard self-tests, Go unit tests, and `make lint` / `make lint docs` / `make lint chart`.
- **Remote Linux CI & xbuild environment:** GitHub Actions `ubuntu-24.04-8x` runners in `gpustack/gpustack-operator` for multi-stage `docker buildx` compilation, ELF symbol checks, and live `auto-sync.yml` runs.

```bash
# Run auto-sync self-tests (gitless diff guards, schema validation, shellcheck, offline deps cache)
bash hack/check/autosync-selftest.sh

# Validate individual domain check gates
bash .auto-sync/checks/validate_slicing.sh
bash .auto-sync/checks/validate_kv_transfer.sh
bash .auto-sync/checks/validate_router.sh
bash .auto-sync/checks/validate_priors.sh
bash .auto-sync/checks/validate_charts.sh
bash .auto-sync/checks/validate_k8s_deps.sh

# Hardware-free slicing xbuild and ELF symbol verification (non-root XB_STAGE)
XB_STAGE="${TMPDIR:-/tmp}/xb-stage-nvidia" XB_MODE=local bash .agents/skills/gpustack-operator-xbuild-and-verify/scripts/build.sh xbuild-nvidia-cuda-12
XB_STAGE="${TMPDIR:-/tmp}/xb-stage-nvidia" XB_MODE=local bash .agents/skills/gpustack-operator-xbuild-and-verify/cases/nvidia-case-1.sh xbuild-nvidia-cuda-12
XB_STAGE="${TMPDIR:-/tmp}/xb-stage-nvidia" XB_MODE=local bash .agents/skills/gpustack-operator-xbuild-and-verify/cases/nvidia-case-4.sh xbuild-nvidia-cuda-12
XB_STAGE="${TMPDIR:-/tmp}/xb-stage-ascend" XB_MODE=local bash .agents/skills/gpustack-operator-xbuild-and-verify/scripts/build.sh xbuild-ascend-cann-8-910b
XB_STAGE="${TMPDIR:-/tmp}/xb-stage-ascend" XB_MODE=local bash .agents/skills/gpustack-operator-xbuild-and-verify/cases/ascend-case-1.sh xbuild-ascend-cann-8-910b

# Priors, settings, and chart unit/lint verification
go test ./gen/priors/... ./pkg/worker/scaling/... ./pkg/worker/settings/... ./pkg/worker/kuberess/...
make generate chart
make lint chart
make lint docs

# Kubernetes module lockstep and toolpin verification
bash hack/check/upgrade-compat.sh modules
bash hack/check/toolpins-selftest.sh
make generate
make lint
RACE=false make test
```

### Project Structure

```text
.auto-sync/
  slicing-taskpack.yaml                          # Domain 1 Taskpack (HAMi-core & ubs-virt vcann-rt)
  kv-transfer-taskpack.yaml                      # Domain 2 Taskpack (Mooncake, NIXL, UCX, HIXL, ModelExpress)
  router-taskpack.yaml                           # Domain 3 Taskpack (llm-d-router, vllm-router, sgl-model-gateway)
  priors-taskpack.yaml                           # Domain 4 Taskpack (InferenceX capacity priors)
  charts-taskpack.yaml                           # Domain 5 Taskpack (5 vendored Helm subcharts & mirrored images)
  k8s-deps-taskpack.yaml                         # Domain 6 Taskpack (K8s v0.36.x lockstep modules & 6 CLI tools)
  proposals/
    slicing.json                                 # Domain 1 structured proposal artifact
    kv-transfer.json                             # Domain 2 structured proposal artifact
    router.json                                  # Domain 3 structured proposal artifact
    priors.json                                  # Domain 4 structured proposal artifact
    charts.json                                  # Domain 5 structured proposal artifact
    k8s-deps.json                                # Domain 6 structured proposal artifact
  instructions/
    slicing.md                                   # Domain 1 agent instructions & CRLF/patch rules
    kv-transfer.md                               # Domain 2 agent instructions & 2-stage image rules
    router.md                                    # Domain 3 agent instructions & patch retirement rules
    priors.md                                    # Domain 4 agent instructions & qualification rules
    charts.md                                    # Domain 5 agent instructions & subchart patch rules
    k8s-deps.md                                  # Domain 6 agent instructions & lockstep module rules
  scripts/
    lib-preflight.sh                             # Shared preflight staging, baseline copy & .sbin warm-up helpers
    write_models_json.py                         # Org LLM gateway models.json resolver
    preflight-slicing.sh                         # Host preflight git clone & dry-run patch check
    preflight-kv-transfer.sh                     # Host preflight runner parity, PyPI cu12/cu13 & crane check
    preflight-router.sh                          # Host preflight router clone, 516f1cb check & crane probe
    preflight-priors.sh                          # Host preflight InferenceX snapshot download & source_revision stage
    preflight-charts.sh                          # Host preflight chart archive fetch, crane probe & source_revision stage
    preflight-k8s-deps.sh                        # Host preflight K8s/kustomize/CLI normalization & source_revision stage
  checks/
    lib.sh                                       # Gitless diff-guard, PATH/.sbin bootstrap & crane helpers
    validate_slicing.sh                          # Domain 1 command.v1 verification gate
    validate_kv_transfer.sh                      # Domain 2 command.v1 verification gate
    validate_router.sh                           # Domain 3 command.v1 verification gate
    validate_priors.sh                           # Domain 4 command.v1 verification gate
    validate_charts.sh                           # Domain 5 command.v1 verification gate
    validate_k8s_deps.sh                         # Domain 6 command.v1 verification gate
  memory/
    slicing-sync/                                # Domain 1 reviewed CAS memory store
    kv-transfer-sync/                            # Domain 2 reviewed CAS memory store
    router-sync/                                 # Domain 3 reviewed CAS memory store
    priors-sync/                                 # Domain 4 reviewed CAS memory store
    charts-sync/                                 # Domain 5 reviewed CAS memory store
    k8s-deps-sync/                               # Domain 6 reviewed CAS memory store
.github/workflows/
  auto-sync.yml                                  # Unified schedule, workflow_dispatch, and comment router
hack/check/
  autosync-selftest.sh                           # Self-tests for gitless diff guards, schemas & shellcheck
pkg/worker/scaling/priors/
  records.md                                     # Relocated capacity prior provenance ledger (writable by priors-sync)
```

### Code Style

Follow repository shell and Go conventions in [development.md](../docs/contribute/development.md) and
[AGENTS.md](../AGENTS.md): `set -euo pipefail` in all `.auto-sync/scripts/*.sh` and
`.auto-sync/checks/*.sh` files, plain ASCII comments without emojis or decorative box-drawing symbols
(satisfying `hack/check/symbols.sh`), and fail-closed `diff -U0` exit-code handling that works inside
gitless `LOOPWRIGHT_CANDIDATE_DIR`:

```bash
#!/usr/bin/env bash
# assert_dockerfile_only_args verifies against the preflight baseline copy that changes to
# pack/gpustack-operator/Dockerfile touch only the caller's allowlisted ARG names.
set -euo pipefail

assert_dockerfile_only_args() {
  local baseline_file="${1:?baseline Dockerfile path required}"
  local candidate_file="${2:?candidate Dockerfile path required}"
  local allowed_regex="${3:?allowed ARG regex required}"
  if [[ ! -f "${baseline_file}" || ! -f "${candidate_file}" ]]; then
    printf 'FAIL: missing baseline (%s) or candidate (%s) Dockerfile\n' \
      "${baseline_file}" "${candidate_file}" >&2
    return 1
  fi
  local raw_diff diff_rc=0
  raw_diff="$(diff -U0 -- "${baseline_file}" "${candidate_file}")" || diff_rc=$?
  if (( diff_rc == 0 )); then
    return 0
  fi
  if (( diff_rc > 1 )); then
    printf 'FAIL: diff exited %d comparing Dockerfile\n' "${diff_rc}" >&2
    return 1
  fi
  local line
  while IFS= read -r line; do
    if [[ "${line}" =~ ^(\+\+\+|---|@@) ]]; then
      continue
    fi
    if [[ "${line}" =~ ^[+-] ]] && [[ ! "${line}" =~ ^[+-]ARG\ (${allowed_regex})= ]]; then
      printf 'FAIL: pack/gpustack-operator/Dockerfile modified outside allowed ARGs (%s): %s\n' \
        "${allowed_regex}" "${line}" >&2
      return 1
    fi
  done <<<"${raw_diff}"
}
```

### Implementation Plan

- [ ] **T1 · Expose optional `source-revision` Action input in `gpustack/loopwright`**
      Blocked by: None
      Owns: `gpustack/loopwright:action.yml`, `gpustack/loopwright:packages/github-app/src/action.ts`, `gpustack/loopwright:packages/github-app/test/turnkey-action-overlay.test.ts`
      Gate: review
      Acceptance: `action.yml` and `runActionCli` in `gpustack/loopwright` forward optional `source-revision` (`INPUT_SOURCE-REVISION`) into `runGitHubActionEntrypoint` so preflight-staged vendor commits (`sourceRevision !== baseRevision`) carry writable files into a single squashed PR commit parented on `baseRevision`.
      Verify: `npm test -- --pkg github-app` (executed in `gpustack/loopwright`)

- [x] **T2 · Repository prefactor: `.dockerignore`, priors `records.md` relocation, and offline `hack/deps.sh` cache**
      Blocked by: None
      Owns: `.dockerignore`, `pkg/worker/scaling/priors/records.md`, `.agents/skills/gpustack-operator-priors/**`, `docs/modules/model-deployment/scaling.md`, `hack/deps.sh`
      Gate: review
      Acceptance: `/.auto-sync` is excluded in `.dockerignore`; canonical provenance lives at `pkg/worker/scaling/priors/records.md` with valid relative pointers from `.agents/skills/gpustack-operator-priors/` and `docs/modules/model-deployment/scaling.md`; `hack/deps.sh` honors optional `GPUSTACK_DEPS_CACHE_DIR` for offline staging without altering default online behavior.
      Verify: `make lint docs && bash hack/check/symbols.sh`

- [x] **T3 · Shared `.auto-sync` common libraries, LLM gateway config writer, and gitless guard self-tests**
      Blocked by: T2
      Owns: `.auto-sync/checks/lib.sh`, `.auto-sync/scripts/lib-preflight.sh`, `.auto-sync/scripts/write_models_json.py`, `hack/check/autosync-selftest.sh`
      Gate: review
      Acceptance: `.auto-sync/checks/lib.sh` provides sanitized `command.v1` `PATH`/`GOCACHE`/`GOMODCACHE`/`.sbin` bootstrap, ephemeral git-index initialization, and `diff -U0` shared-file guards (`assert_dockerfile_only_args`, `assert_value_go_only_keys`, `assert_deps_sh_only_section`) that fail closed in gitless directories; `write_models_json.py` resolves org LLM gateway vars; `hack/check/autosync-selftest.sh` proves both positive and negative guard cases and auto-discovers domain manifests, scripts, validators, and optional `.auto-sync/checks/selftest_*.sh` suites via subcommands (`all`, `common`, `<domain>`, `workflow`).
      Verify: `bash hack/check/autosync-selftest.sh && bash hack/check/symbols.sh`

- [x] **T4 · Domain 4 tracer bullet: `priors-sync` (InferenceX capacity priors)**
      Blocked by: T3
      Owns: `.auto-sync/priors-taskpack.yaml`, `.auto-sync/instructions/priors.md`, `.auto-sync/scripts/preflight-priors.sh`, `.auto-sync/checks/validate_priors.sh`, `.auto-sync/checks/selftest_priors.sh`, `.auto-sync/proposals/priors.json`
      Acceptance: `priors-taskpack.yaml` passes `validateTaskpackManifest`; `preflight-priors.sh` stages InferenceX snapshots and `source_revision` catalog updates under `/tmp/gpustack-autosync-stage/priors`; `validate_priors.sh` verifies SHA-256 ledger parity, `go test ./gen/priors/... ./pkg/worker/scaling/...`, and `make lint docs` in a gitless candidate directory.
      Verify: `bash hack/check/autosync-selftest.sh priors && go test ./gen/priors/... ./pkg/worker/scaling/...`

- [x] **T5 · Domain 1 tracer bullet: `slicing-sync` (`HAMi-core` and `ubs-virt` `vcann-rt`)**
      Blocked by: T3
      Owns: `.auto-sync/slicing-taskpack.yaml`, `.auto-sync/instructions/slicing.md`, `.auto-sync/scripts/preflight-slicing.sh`, `.auto-sync/checks/validate_slicing.sh`, `.auto-sync/checks/selftest_slicing.sh`, `.auto-sync/proposals/slicing.json`
      Acceptance: `slicing-taskpack.yaml` passes `validateTaskpackManifest`; `validate_slicing.sh` enforces `LIB_HAMI_CORE_COMMIT`/`LIB_UBS_VIRT_ENPU_COMMIT`-only diff guard on `pack/gpustack-operator/Dockerfile`, verifies CRLF-safe `vcann-rt` patch application, and runs non-root `XB_STAGE` xbuild + ELF verification (`nvidia-case-1`, `nvidia-case-4`, `ascend-case-1`).
      Verify: `bash hack/check/autosync-selftest.sh slicing`

- [ ] **T6 · Domain 2 tracer bullet: `kv-transfer-sync` (Mooncake, NIXL, UCX, HIXL, ModelExpress, and two-stage gate)**
      Blocked by: T3
      Owns: `.auto-sync/kv-transfer-taskpack.yaml`, `.auto-sync/instructions/kv-transfer.md`, `.auto-sync/scripts/preflight-kv-transfer.sh`, `.auto-sync/checks/validate_kv_transfer.sh`, `.auto-sync/checks/selftest_kv_transfer.sh`, `.auto-sync/proposals/kv-transfer.json`
      Acceptance: `kv-transfer-taskpack.yaml` passes `validateTaskpackManifest`; `preflight-kv-transfer.sh` probes `nixl`/`nixl-cu12`/`nixl-cu13` wheels, `gpustack/runner` HIXL parity, and `crane manifest` Stage A vs Stage B (`tag_promotion`) readiness; `validate_kv_transfer.sh` enforces all 5 `MOONCAKE_VERSION` `case` blocks, HIXL patch parity, shared-file guards on `value.go`/`settings.md`/`modelTransfer.images.*`, and `crane manifest` verification.
      Verify: `bash hack/check/autosync-selftest.sh kv-transfer && go test ./pkg/worker/settings/...`

- [ ] **T7 · Domain 3 tracer bullet: `router-sync` (`llm-d-router`, `vllm-router`, and `sgl-model-gateway`)**
      Blocked by: T3
      Owns: `.auto-sync/router-taskpack.yaml`, `.auto-sync/instructions/router.md`, `.auto-sync/scripts/preflight-router.sh`, `.auto-sync/checks/validate_router.sh`, `.auto-sync/checks/selftest_router.sh`, `.auto-sync/proposals/router.json`
      Acceptance: `router-taskpack.yaml` passes `validateTaskpackManifest`; `preflight-router.sh` normalizes `gateway-v*` tags, detects `516f1cb` ancestor retirement for `0001-epd-routing.patch`, and probes router image manifests for Stage A/B; `validate_router.sh` verifies all remaining patches, `ObserverRouter` presence, changed builder targets via `docker buildx`, and `value.go` image guards.
      Verify: `bash hack/check/autosync-selftest.sh router`

- [ ] **T8 · Domain 5 tracer bullet: `charts-sync` (5 vendored Helm subcharts and 11 `mirrored-*` images)**
      Blocked by: T3
      Owns: `.auto-sync/charts-taskpack.yaml`, `.auto-sync/instructions/charts.md`, `.auto-sync/scripts/preflight-charts.sh`, `.auto-sync/checks/validate_charts.sh`, `.auto-sync/checks/selftest_charts.sh`, `.auto-sync/proposals/charts.json`
      Acceptance: `charts-taskpack.yaml` passes `validateTaskpackManifest`; `preflight-charts.sh` probes `gpustack/mirrored-*` manifests and pre-stages unpacked subcharts onto `source_revision` when ready; `validate_charts.sh` enforces `chart_staging()`-only diff guard on `hack/deps.sh`, zero `.rej`/`.orig`/`.tgz` artifacts, `make generate chart`, `make lint chart`, and `go test ./pkg/worker/kuberess/...`.
      Verify: `bash hack/check/autosync-selftest.sh charts && go test ./pkg/worker/kuberess/...`

- [ ] **T9 · Domain 6 tracer bullet: `k8s-deps-sync` (6 CLI pins and same-minor K8s `v0.36.x` lockstep modules)**
      Blocked by: T3
      Owns: `.auto-sync/k8s-deps-taskpack.yaml`, `.auto-sync/instructions/k8s-deps.md`, `.auto-sync/scripts/preflight-k8s-deps.sh`, `.auto-sync/checks/validate_k8s_deps.sh`, `.auto-sync/checks/selftest_k8s_deps.sh`, `.auto-sync/proposals/k8s-deps.json`
      Acceptance: `k8s-deps-taskpack.yaml` passes `validateTaskpackManifest` with `podman-container-tools/skopeo`, `helmfile/vals`, and same-minor `v0.36.x` lockstep constraint; `preflight-k8s-deps.sh` pre-stages `mod_staging()` + `make generate` onto `source_revision`; `validate_k8s_deps.sh` enforces CLI-only `Dockerfile` guard, `mod_staging()`-only `hack/deps.sh` guard, codegen-only `api/` guard, `upgrade-compat.sh modules`, `toolpins-selftest.sh`, double `make generate` + `make lint`, and `RACE=false make test`.
      Verify: `bash hack/check/autosync-selftest.sh k8s-deps && bash hack/check/upgrade-compat.sh modules && bash hack/check/toolpins-selftest.sh`

- [ ] **T10 · Unified `.github/workflows/auto-sync.yml` matrix, `ubuntu-24.04-8x` profile, and PR comment router**
      Blocked by: T1, T4, T5, T6, T7, T8, T9
      Owns: `.github/workflows/auto-sync.yml`
      Gate: review
      Acceptance: `.github/workflows/auto-sync.yml` orchestrates staggered weekly cron schedules for all 6 domains, `workflow_dispatch` (`domain`, `force`, `dry-run`, `runner_profile` defaulting to `8x` -> `ubuntu-24.04-8x`), safe `issue_comment` routing on `main` (`refs/heads/main` preserved), `write_models_json.py` gateway resolution, and `gpustack/loopwright@main` invocation with `upstream-records`, `source-revision`, `event-name`, and `event-path`.
      Verify: `bash hack/check/autosync-selftest.sh workflow && bash hack/check/symbols.sh`

- [ ] **T11 · Integrate `autosync-selftest.sh` into `hack/lint.sh`, Stop-hook dispatch, and full repo qualification**
      Blocked by: T10
      Owns: `hack/lint.sh`, `.agents/hooks/gpustack-operator-lint.sh`, `hack/check/hook-dispatch.sh`
      Gate: review
      Acceptance: `make lint` runs `hack/check/autosync-selftest.sh` (including `bash -n` and pinned `shellcheck` over `.auto-sync/**/*.sh` plus Loopwright manifest schema validation); `hook-dispatch.sh` covers `.auto-sync/**` edits; `make lint`, `make lint docs`, and `make lint chart` all exit `0` on a clean tree.
      Verify: `make lint && make lint docs && make lint chart`

### Test Plan

[ ] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

- Add `source-revision` overlay carry-over Action CLI coverage in `gpustack/loopwright` (`packages/github-app/test/turnkey-action-overlay.test.ts`) before live multi-hundred-file `charts-sync` and `k8s-deps-sync` runs (`T1`).
- Add `GPUSTACK_DEPS_CACHE_DIR` offline archive/staging self-test coverage in `hack/check/autosync-selftest.sh` (`T2`, `T3`).

#### Unit tests

- `hack/check/autosync-selftest.sh`: `2026-11-17` - `100%` of shared-file diff guards (gitless `LOOPWRIGHT_CANDIDATE_DIR` positive and negative mutation cases across `pack/gpustack-operator/Dockerfile`, `pkg/worker/settings/value.go`, `docs/reference/settings.md`, `deploy/gpustack-operator/chart/values.yaml`, and `hack/deps.sh`), `MOONCAKE_VERSION` 5-block `case` parser, Stage A vs Stage B (`tag_promotion`) `upstream-records` state machine, non-root `XB_STAGE` path redirection, and `write_models_json.py` env resolution.
- `gen/priors` and `pkg/worker/scaling/priors`: `2026-11-17` - existing catalog validation and provenance SHA-256 parity (`go test ./gen/priors/... ./pkg/worker/scaling/...`).
- `pkg/worker/settings` and `pkg/worker/kuberess`: `2026-11-17` - default image literal alignment and chart image overlay parity (`go test ./pkg/worker/settings/... ./pkg/worker/kuberess/...`).

#### Integration tests

- Loopwright `validateTaskpackManifest` and `compileDeclarativeTaskpack` validation across all six `.auto-sync/*-taskpack.yaml` manifests (`controlRoot: '.auto-sync'`) verifying schema, `spec.proposal.path` carve-outs, `headBranchPrefix`, and `resolveTaskpackMemoryLocation`.
- Sanitized `command.v1` offline execution harness (`env -i PATH=/usr/local/bin:/usr/bin:/bin HOME=<scratch> LOOPWRIGHT_CANDIDATE_DIR=<gitless-dir> AUTOSYNC_STAGE_DIR=/tmp/gpustack-autosync-stage/<domain>`) exercising each `validate_<domain>.sh` script.
- Repository gate integration: `make lint`, `make lint docs` (`check-specs.sh`, `check-docs.sh`, `check-crossrefs.sh`), `make lint chart`, `bash hack/check/hook-dispatch.sh`, `bash hack/check/upgrade-compat.sh modules`, and `bash hack/check/toolpins-selftest.sh`.

#### e2e tests

- Trigger `.github/workflows/auto-sync.yml` via `workflow_dispatch` on `ubuntu-24.04-8x` with `dry-run=true` across all six domains (`domain=all`) to qualify host preflight staging, `models.json` generation, and Loopwright taskpack compilation without opening pull requests, followed by phased live `dry-run=false` runs per the Three-Phase Rollout Criteria (`Phase 1` -> `Phase 2` -> `Phase 3`). Physical GPU/NPU cluster E2E is intentionally excluded from automated PR gates and remains a human-dispatched release check.

## Alternatives

- **Single monolithic `.auto-sync/taskpack.yaml` covering all upstreams:** Rejected. A single
  Taskpack couples C/C++ `docker buildx` stages (`HAMi-core`, `vcann-rt`), Rust workspaces
  (`vllm-router`, `sgl-model-gateway`, `modelexpress-server`), Go `staging/` codegen, Helm subchart
  patches, and InferenceX benchmark sweeps into one failure domain and one `3600s` wall-clock budget.
  Any upstream patch conflict or slow container build would block unrelated updates and pollute one
  shared memory branch.
- **Single-step updates that bump `pack/*/Dockerfile` and [value.go](../pkg/worker/settings/value.go)
  in the same PR:** Rejected. Because [base-image.yml](../.github/workflows/base-image.yml) and
  [sync-image.yml](../.github/workflows/sync-image.yml) run only via manual `workflow_dispatch` after
  recipe changes land on `main`, updating default image tags in [value.go](../pkg/worker/settings/value.go)
  before the image is published breaks cluster installations with `ImagePullBackOff`.
- **Keep capacity prior provenance records in `.agents/skills/gpustack-operator-priors/references/records.md`:**
  Rejected. Loopwright hardcodes `.agents` in `PROTECTED_AUTOMATION_PATHS`, so no Taskpack can declare
  `.agents/**` in `spec.scope.writable`. Relocating the ledger to
  `pkg/worker/scaling/priors/records.md` preserves Loopwright's `.agents` security boundary while
  keeping catalog data and SHA-256 provenance atomic.
- **Stage preflight git clones under `.auto-sync/.stage/` during Loopwright execution:** Rejected.
  Loopwright's `collectControlRootSiblingPaths` enumerates files inside `.auto-sync` and enforces a
  hard cap of `128` protected entries, while `command.v1` materializes `LOOPWRIGHT_CANDIDATE_DIR`
  solely from committed `baseTree` git blobs plus candidate overlay files. Staging outside the checkout
  at `$AUTOSYNC_STAGE_DIR` (`/tmp/gpustack-autosync-stage/<domain>`) and passing `AUTOSYNC_STAGE_DIR`
  via `command.v1` `env` avoids both the 128-entry cap and missing untracked files in `command.v1`.

## Open Questions

None.
