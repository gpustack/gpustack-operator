# Version contract & build-cache risk

Background for CASE 1 (install + version consistency) and CASE 3 (mock release).

## The three views must agree

CASE 1 checks that the version is consistent across three views:

1. **Running binary == HEAD** (assert-core.sh) — the image label can claim HEAD while the embedded
   binary is an older cached build.
2. **Binary version == bundled chart tgz** — the Dockerfile derives the tgz version from
   `gpustack-operator --version` (strip `v`, else `0.0.0`); they must be equal.
3. **Deployed image tag == the one built** — confirms the kubelet runs your local image.

View 2 is the core chart-version check: the bundled tgz is exactly what the worker would
`helm install` for the device-manager at runtime. `deviceManagerChartVersion()` in
`pkg/worker/kuberess/apps_gpustack_device_manager.go` mirrors the same strip-`v` / `0.0.0` logic, so
a mismatch here is a release/cache bug, not a cosmetic one.

## Why the device-manager runtime install is the version-critical path

With `--set deviceManager.enabled=false`, the chart does not render the device-manager DaemonSets;
instead the worker installs them from the bundled `gpustack-operator-<ver>.tgz` as a **separate** Helm
release `gpustack-operator-device-manager`. A version mismatch's concrete runtime symptom: that
runtime install **fails to find `gpustack-operator-<ver>.tgz`** (the version it computes via
`deviceManagerChartVersion()` no longer matches the packaged tgz). A healthy
`gpustack-operator-device-manager` release in `helm list` confirms the version is consistent end to
end. With the default `deviceManager.enabled=true` the chart renders the DaemonSets directly and that
runtime release is not created.

## CASE 3 — mock a release version against a warm cache

The risk: the builder cache key tracks the commit, so re-building an already-built commit with a
*different* version could otherwise serve a stale binary stamped `dev` and a chart packaged `0.0.0`.
`make package` passes the resolved version as the `GPUSTACK_GIT_VERSION` build-arg, which the builder
both stamps and folds into its cache key — so a version change forces a rebuild.

CASE 3 forces `VERSION=v9.9.9 PACKAGE_TAG=dev-rel make package` and asserts the binary reports `v9.9.9`
and the bundled tgz is `gpustack-operator-9.9.9.tgz`. If `--version` shows `dev` or the tgz is
`gpustack-operator-0.0.0.tgz`, the cache served a stale binary — the version did not bust the cache key.

The realistic release trigger is a git tag rather than `VERSION=`; on a **clean** tree
`git tag v9.9.9 && make package` (then `git tag -d v9.9.9`) reproduces the same path, since the build
derives the version from `git tag -l --contains HEAD`.

## Troubleshooting

- **Version mismatch (CASE 1 view 2)** — the bundled tgz disagrees with the binary. Run CASE 3 to
  reproduce against a mock tag; if it persists with a warm cache, it is the build-cache/version issue.
  Its runtime symptom is the failed device-manager install described above.
- **`--version` ≠ HEAD** — stale image; see `../_e2e-lib/references/troubleshooting.md`.
