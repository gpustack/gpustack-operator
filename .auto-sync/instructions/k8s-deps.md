# `k8s-deps-sync` Domain Instructions

## Scope and Ownership

You are synchronizing the six CLI tool pins and Kubernetes `v0.36.x` lockstep Go dependencies inside an offline candidate sandbox (`networkMode: none`):

1. **CLI tool pins in `pack/gpustack-operator/Dockerfile` and `hack/lib/helm.sh`:**
   - `SKOPEO_VERSION` (`podman-container-tools/skopeo`)
   - `HELM_VERSION` (`helm/helm`)
   - `VALS_VERSION` (`helmfile/vals`)
   - `KUSTOMIZE_VERSION` (`kubernetes-sigs/kustomize`, normalized from `kustomize/v*`)
   - `YTT_VERSION` (`carvel-dev/ytt`)
   - `KUBECTL_VERSION` (`kubernetes/kubernetes`)
2. **Kubernetes lockstep Go modules and `staging/` trees:**
   - `hack/deps.sh` (`mod_staging()` heredoc only) and `staging/**` (`hack/staging/**` patches)
   - `go.mod` and `go.sum` (`k8s.io/*`, `sigs.k8s.io/controller-runtime`, `sigs.k8s.io/kueue`, `helm.sh/helm/v3`)
   - Regenerated `api/` and `pkg/` codegen outputs produced by `make generate`

## Same-Minor Kubernetes `v0.36.x` Lockstep Rule

- `k8s-deps-sync` automates **patch upgrades only** within the repository's current Kubernetes minor series (`v0.36.x` for `k8s.io/{api,apimachinery,client-go,code-generator,kube-aggregator,apiextensions-apiserver,apiserver,cli-runtime,component-base,component-helpers,kms,kubectl,kubelet,cri-streaming,streaming,dynamic-resource-allocation}` and `1.36.x` for `KUBECTL_VERSION`, satisfying `|M - Y| <= 1` in `hack/check/upgrade-compat.sh`).
- Cross-minor Kubernetes jumps (`v0.37+`) require human planning and are filtered out by `preflight-k8s-deps.sh`.
- Keep `sigs.k8s.io/controller-runtime` on its current `v0.24.x` minor series and `sigs.k8s.io/kueue` on its current `v0.18.x` minor series.
- Every staged module under `staging/` must carry an exact `_VERSION_` stamp matching `go.mod`, apply all patches under `hack/staging/` cleanly with zero `.rej` or `.orig` files, and pass `bash hack/check/upgrade-compat.sh modules`.

## Helm `v3.*` Three-Place Lockstep and `k8s.io/*` Compatibility Rule

- `HELM_VERSION` in `pack/gpustack-operator/Dockerfile` must stay on `v3.*` (never `v4.*`, which preflight filters out) and its `go.mod` must stay compatible with the repository's current Kubernetes minor (`k8s.io/*` minor `<= v0.36.x`; `preflight-k8s-deps.sh` skips newer Helm `v3.y.z` releases that bump `k8s.io/*` to `v0.37+`).
- Keep all three Helm pins aligned to the exact same `3.y.z` / `v3.y.z` version:
  1. `ARG HELM_VERSION="<x.y.z>"` in `pack/gpustack-operator/Dockerfile`
  2. BOTH the header comment on line 7 (`default is v<x.y.z>.`) AND `helm_version=${HELM_VERSION:-"v<x.y.z>"}` on line 14 of `hack/lib/helm.sh` (`hack/check/toolpins-selftest.sh` checks both!)
  3. `helm.sh/helm/v3 v<x.y.z>` in `go.mod`

## Shared-File and API Boundaries

- **`pack/gpustack-operator/Dockerfile`**: Touch ONLY `ARG (SKOPEO|HELM|VALS|KUSTOMIZE|YTT|KUBECTL)_VERSION="..."` lines at the top of the file. Modifying `GO_VERSION`, `LIB_HAMI_CORE_COMMIT`, `LIB_UBS_VIRT_ENPU_COMMIT`, or any build stage body fails `validate_k8s_deps.sh`.
- **`hack/deps.sh`**: Touch ONLY version/URL lines inside the `mod_staging()` heredoc. Never edit `chart_staging()`, shell logic, or heredoc destinations.
- **`api/`**: ONLY codegen outputs (`zz_generated*.go`, `zz_generated.openapi.violation_exceptions`, `generated.pb.go`, `generated.protomessage.pb.go`, `generated.proto`) may change. Never edit hand-written API types.
- **Protected paths**: Never modify `pkg/devicemanager/allocator`, `pkg/worker/scaling/priors/priors.yaml`, `pkg/worker/scaling/priors/records.md`, `pkg/worker/settings/value.go`, `csrc`, `binding`, or `gen/binding`.

## Staged Preflight Artifacts (`.auto-sync/.stage/k8s-deps/`)

Inside the offline `linux-bwrap` sandbox, host toolchains (`go`, `golangci-lint`, `goimports-reviser`,
`protoc`, `hugo`, etc.) and external host staging directories are NOT mounted. Always inspect
`.auto-sync/.stage/k8s-deps/` FIRST (falling back to `$AUTOSYNC_STAGE_DIR` only if
`.auto-sync/.stage/k8s-deps/` is absent):

- `.auto-sync/.stage/k8s-deps/k8s-deps-preflight-report.json` — filtered same-minor and `v3.*`
  upstream versions, `staging_updated`, `patch_clean`, `held_back_upstreams`, and `target` pins.
- `.auto-sync/.stage/k8s-deps/upstream-records.json` — normalized `TaskpackUpstreamRecord` entries.

**Read-only stage rule:** `.auto-sync/.stage/**` is read-only host-staged context outside
`scope.writable`. **NEVER** edit, move, or delete any file under `.auto-sync/.stage/**` (doing so
fails `scope-guard`). Loopwright automatically strips `.auto-sync/.stage/**` when running
`validate_k8s_deps.sh` on the host and when publishing the Pull Request.

**CRITICAL — Do NOT hand-edit pre-staged Go modules or generated trees:**
When `staging_updated` is `true` in `k8s-deps-preflight-report.json`, `hack/deps.sh`, `staging/`,
`go.mod`, `go.sum`, `api/`, and `pkg/` are **already staged, linted, and double-generated** in
your workspace by host preflight! Do **NOT** hand-edit `go.mod`, `go.sum`, `hack/deps.sh`,
`staging/`, `api/`, or `pkg/` inside `bwrap`.

Your edits inside `bwrap` are strictly:
1. Update the changed CLI `ARG` pins (`SKOPEO_VERSION`, `HELM_VERSION`, `VALS_VERSION`,
   `KUSTOMIZE_VERSION`, `YTT_VERSION`, `KUBECTL_VERSION`) in `pack/gpustack-operator/Dockerfile`.
2. When `HELM_VERSION` changed, update BOTH line 7 (`default is v<x.y.z>.`) and line 14
   (`helm_version=${HELM_VERSION:-"v<x.y.z>"}`) in `hack/lib/helm.sh`.
3. Update `.auto-sync/proposals/k8s-deps.json` (`schema_version: 1`, `summary`, `versions`,
   `subscriptions`) to match `target` in `k8s-deps-preflight-report.json`.

Full verification (`upgrade-compat.sh modules`, `toolpins-selftest.sh`, `make generate`, `make lint`,
and `RACE=false make test`) executes on the host in `validate_k8s_deps.sh` after you submit.

## Settlement

- Settle as `completed` after updating the CLI pins (`Dockerfile`, `hack/lib/helm.sh` if Helm changed) and `.auto-sync/proposals/k8s-deps.json`. `versions` must contain all 11 pinned keys matching `validate_k8s_deps.sh`: `kubernetes`, `k8s-modules`, `controller-runtime`, `kueue-sdk`, `helm-cli`, and `helm-module` with leading `v`; `skopeo-cli`, `vals-cli`, `kustomize-cli`, `ytt-cli`, and `kubectl-cli` without leading `v`. Keep `subscriptions` aligned with the 8 `upstream-records.json` IDs (`kubernetes`, `controller-runtime`, `kueue-sdk`, `helm-cli`, `skopeo-cli`, `vals-cli`, `kustomize-cli`, `ytt-cli`).
- If all upstreams in `upstream-records.json` are `up_to_date` and all repository pins already match, settle cleanly as `no_change` without modifying files.
