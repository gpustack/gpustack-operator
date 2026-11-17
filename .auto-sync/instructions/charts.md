# Vendored Helm Subcharts Auto-Sync (`charts-sync`)

You maintain the 5 vendored Helm subcharts under `deploy/gpustack-operator/chart/charts/`, their staging definitions in `hack/deps.sh` (`chart_staging()`), their 16 patch files under `hack/deploy/gpustack-operator/chart/charts/`, the parent chart's `Chart.yaml` and `values.yaml` (plus generated `values.schema.json` and `README.md`), and any alignment in `pkg/worker/kuberess/`.

Before editing any file, inspect `$AUTOSYNC_STAGE_DIR/charts-preflight-report.json` and `$AUTOSYNC_STAGE_DIR/image-manifests.json`.

## 1. Vendored Subcharts, Patches, and Mirrored Images

`chart_staging()` in `hack/deps.sh` vendors 5 upstream subcharts and applies 16 patches (`patch -p1 -N --forward --silent -F0 --no-backup-if-mismatch`):

1. `kueue` (`kubernetes-sigs/kueue`, 5 patches): `cert-manager.patch`, `global-image.patch`, `global-node-selector.patch`, `manager-config.patch`, `selectable-fields.patch`.
   - Image pin in `deploy/gpustack-operator/chart/values.yaml`: `kueue.controllerManager.manager.image` (`docker.io/gpustack/mirrored-kueue`).
2. `node-feature-discovery` (`kubernetes-sigs/node-feature-discovery`, 2 patches): `global-image.patch`, `global-node-selector.patch`.
   - Image pin in `values.yaml`: `node-feature-discovery.image` (`docker.io/gpustack/mirrored-node-feature-discovery`).
3. `csi-driver-nfs` (`kubernetes-csi/csi-driver-nfs`, 3 patches): `global-image.patch`, `global-node-selector.patch`, `topology-spread.patch`.
   - Image pins in `values.yaml` (`csi-driver-nfs.image`): `nfs` (`mirrored-csi-nfs-driver`), `csiProvisioner` (`mirrored-csi-provisioner`), `csiResizer` (`mirrored-csi-resizer`), `csiSnapshotter` (`mirrored-csi-snapshotter`), `livenessProbe` (`mirrored-csi-livenessprobe`), `nodeDriverRegistrar` (`mirrored-csi-node-driver-registrar`).
4. `csi-driver-s3` (`thxcode/k8s-csi-s3`, 4 patches): `chart-rename.patch`, `feature-gates.patch`, `global-image.patch`, `global-node-selector.patch`.
   - Image pins in `values.yaml` (`csi-driver-s3.images`): `s3` (`mirrored-csi-s3-driver`) plus shared sidecars (`csiProvisioner`, `livenessProbe`, `nodeDriverRegistrar`) kept aligned with `csi-driver-nfs` and `modelManager.registrar.image`.
5. `topograph` (`NVIDIA/topograph`, 2 patches): `global-image.patch`, `notice.patch`.
   - Image pin in `values.yaml`: `topograph.image` (`docker.io/gpustack/mirrored-nvidia-topograph`, tag `""` falling back to `charts/topograph/Chart.yaml` `appVersion`).

## 2. Strict Mirrored-Image Readiness Gate

NEVER bump a subchart whose `mirrored_image_ready` is `false` in `$AUTOSYNC_STAGE_DIR/charts-preflight-report.json`:

- If a subchart has `mirrored_image_ready: false`, leave that subchart's URL/version in `hack/deps.sh`, `deploy/gpustack-operator/chart/`, and `versions` unchanged.
- Record the exact `gh workflow run sync-image.yml -f images="..."` command from `$AUTOSYNC_STAGE_DIR/charts-preflight-report.json` in the `summary` field of `.auto-sync/proposals/charts.json`.
- If zero subcharts have updates with `mirrored_image_ready: true` (or all upstreams are already up to date), settle as `no_change` (or `blocked` with the exact `gh workflow run sync-image.yml` command recorded in the proposal summary if waiting on image mirroring).
- Only update subcharts whose `mirrored_image_ready` is `true` and whose multi-arch digests (`amd64Digest` and `arm64Digest`) are verified in `$AUTOSYNC_STAGE_DIR/image-manifests.json`.

## 3. Patch-First Staging and Chart Regeneration Rules

- **Shared-file guard (`hack/deps.sh`)**: Modify ONLY lines inside the `chart_staging()` heredoc (`${charts_dir}/(kueue|node-feature-discovery|csi-driver-nfs|csi-driver-s3|topograph)`). Never touch `mod_staging()` or any shell function logic in `hack/deps.sh`.
- **Patches are source of truth**: Never hand-edit unpacked files under `deploy/gpustack-operator/chart/charts/<name>/` without updating `hack/deploy/gpustack-operator/chart/charts/<name>/*.patch` so that:
  `GPUSTACK_DEPS_STAGE_ONLY=charts GPUSTACK_DEPS_OFFLINE=true bash hack/deps.sh`
  reproduces `deploy/gpustack-operator/chart/charts/` cleanly from `$AUTOSYNC_STAGE_DIR/deps-cache/charts/<name>-<version>.tgz` + patches with zero `.rej`, `.orig`, or `.tgz` artifacts.
- **Lockstep chart metadata and overlays**:
  - Keep `deploy/gpustack-operator/chart/Chart.yaml` `dependencies[].version` and `_VERSION_` files aligned with `hack/deps.sh`.
  - Update `deploy/gpustack-operator/chart/values.yaml` subchart mirrored image tags and header version comments, keeping shared CSI sidecars (`csiProvisioner`, `livenessProbe`, `nodeDriverRegistrar`) aligned across `csi-driver-nfs`, `csi-driver-s3`, and `modelManager.registrar.image`.
  - Check `pkg/worker/kuberess/` (including `apps.go`, `apps_gpustack_operator.go`, and `chart_test.go`) if any subchart value keys, feature gates, or chart defaults affect `TestChartDefaultsMatchImageModeOverlay` or other `kuberess` tests.
  - Always run `make generate chart` after editing `values.yaml` or `Chart.yaml` so `deploy/gpustack-operator/chart/values.schema.json` and `deploy/gpustack-operator/chart/README.md` match, and verify `make lint chart` and `go test ./pkg/worker/kuberess/...` pass with a clean `git status --porcelain`.
- **Proposal artifact**: Update `.auto-sync/proposals/charts.json` (`schema_version: 1`, `summary`, `versions`, `subscriptions`) to match the resulting `hack/deps.sh` subchart versions.
