# Version contract & build-cache risk

Background for CASE 1 (install + version consistency), CASE 3 (mock release) and CASE 6 (image mode).

## The three views must agree

CASE 1 checks that the version is consistent across three views:

1. **Running binary == HEAD** (assert-core.sh) — the image label can claim HEAD while the embedded
   binary is an older cached build.
2. **Binary version == bundled chart tgz** — the Dockerfile derives the tgz version from
   `gpustack-operator --version` (strip `v`, else `0.0.0`); they must be equal.
3. **Deployed image tag == the one built** — confirms the kubelet runs your local image.

View 2 is the core chart-version check: the bundled tgz is exactly what the worker installs where no
chart deploys it. `gpustackOperatorChartVersion()` in
`pkg/worker/kuberess/apps_gpustack_operator.go` mirrors the same strip-`v` / `0.0.0` logic, so a
mismatch here is a release/cache bug, not a cosmetic one.

## Why image mode is the version-critical path

In chart mode the version is only ever compared, never used: the chart renders the device-managers
and the applications itself, and the worker installs nothing (`worker.disableApplications: ["*"]`).
The bundled tgz is *consumed* only in **image mode** — where nothing deploys the worker from a chart,
so the worker installs the chart packaged into its own image as the release
`gpustack-operator-device-manager`, itself switched off. A version mismatch's concrete symptom is
that install **failing to find `gpustack-operator-<ver>.tgz`**: the version
`gpustackOperatorChartVersion()` computes no longer names the packaged file, and because the worker
gates its startup on installing its applications, it never starts.

CASE 6 stands that topology up and asserts the release's own chart version equals the running
binary's — the direct form of view 2. It needs a cluster with **no** chart release: both renders carry
the cluster-scoped `gpustack-cpu-info` NodeFeatureRule, so a worker installing its own release while
a chart release owns that rule is refused by Helm on ownership metadata and never starts. The two
install modes are exclusive by construction, which is also why
`--set deviceManager.enabled=false` no longer exercises this path — it now only means "render no
device-manager DaemonSets".

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
  Its runtime symptom is the failed image-mode install described above.
- **`--version` ≠ HEAD** — stale image; see `../_e2e-lib/references/troubleshooting.md`.
