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

## Preflight Staging Inputs and Codegen Cycle

Host preflight (`preflight-k8s-deps.sh`) prepares `$AUTOSYNC_STAGE_DIR`:

- `$AUTOSYNC_STAGE_DIR/k8s-deps-preflight-report.json` and `$AUTOSYNC_STAGE_DIR/upstream-records.json` — filtered same-minor and `v3.*` upstream versions and patch status.
- `$AUTOSYNC_STAGE_DIR/deps-cache` and `$AUTOSYNC_STAGE_DIR/go-mod` — offline staging tarballs and Go module cache (`GOPROXY=off`).
- `$AUTOSYNC_STAGE_DIR/baseline/` — baseline copies used by diff guards.

When updating Go modules or staging trees:

1. Run `make generate` (note: `gen/api/main.go` requires a physical path ending in `/gpustack.ai/gpustack`).
2. Run `make lint` (`golangci-lint --fix` and `goimports-reviser` may rewrite Go files in place).
3. Run `make generate` **again** after `make lint` so `goimports-reviser` leaves zero regeneration drift (`git diff --exit-code`).
4. Run `bash hack/check/upgrade-compat.sh modules`, `bash hack/check/toolpins-selftest.sh`, and `RACE=false make test`.

## Settlement

- Update `.auto-sync/proposals/k8s-deps.json` (`schema_version: 1`, `summary`, `versions`, `subscriptions`) to reflect any updated pins and settle as `completed`. `versions` must contain all 11 pinned keys matching `validate_k8s_deps.sh`: `kubernetes`, `k8s-modules`, `controller-runtime`, `kueue-sdk`, `helm-cli`, and `helm-module` with leading `v`; `skopeo-cli`, `vals-cli`, `kustomize-cli`, `ytt-cli`, and `kubectl-cli` without leading `v`. Keep `subscriptions` aligned with the 8 `upstream-records.json` IDs (`kubernetes`, `controller-runtime`, `kueue-sdk`, `helm-cli`, `skopeo-cli`, `vals-cli`, `kustomize-cli`, `ytt-cli`).
- If all upstreams in `$AUTOSYNC_STAGE_DIR/upstream-records.json` are `up_to_date` and all repository pins already match, settle cleanly as `no_change` without modifying files.
