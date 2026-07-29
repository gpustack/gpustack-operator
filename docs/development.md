# Development

Build, lint, test, code generation, and dependency management for GPUStack Operator. For the
runtime architecture see [architecture.md](architecture.md); for settings and the `GPUSTACK_*`
knobs see [settings.md](settings.md).

## Commands

The `Makefile` dispatches each target to `hack/<target>.sh`; the target name *is* the script name,
and `make <target> [args]` forwards `args` to the script. All builds use `CGO_ENABLED=1` and
`GODEBUG=gotypesalias=0`; default build tags are `goccy netgo`.

- `make deps` — vendor patched k8s staging modules into `staging/` **and** the upstream Helm charts into `deploy/gpustack-operator/chart/charts/`, then `go mod tidy && go mod download`. `make deps update` also runs `go get -u ./...`.
- `make generate` — run `gen/api` code generators (deepcopy, register, apiservice, CRDs, conversion, protobuf, webhooks). `make generate binding` regenerates the CGO bindings in `binding/` via c-for-go.
- `make lint` — run golangci-lint (config in `.golangci.yaml`). `make lint dirty` additionally fails if the git tree is dirty afterward.
- `make build` — cross-build `cmd/gpustack-operator` into `.dist/build/`. Version is injected via ldflags into `pkg/utils/version`. `VERSION=vX.y.z+l.m make build` sets the version; `BUILD_PLATFORMS="linux/amd64 linux/arm64"` cross-compiles.
- `make test` — `go test -v -failfast -race -cover -timeout=30m ./...`; coverage to `.dist/test/coverage.out`. Trailing args are treated as regex patterns of packages to **exclude**.
- `make package` — build container images via `docker buildx` from `pack/*/Dockerfile` (Linux only).

CI (`hack/ci.sh`) runs: `make generate && make deps && make lint && make build`.

### Helm chart

The `generate`, `lint`, and `test` targets take a `chart` argument that operates on
`deploy/gpustack-operator/chart` (via [chart-testing](https://github.com/helm/chart-testing),
[helm-docs](https://github.com/norwoodj/helm-docs), and helm-schema):

- `make generate chart` — update chart dependencies, then regenerate `README.md` (from `README.md.gotmpl`) and `values.schema.json`. **Never hand-edit those two files**; edit `values.yaml`/value annotations/`README.md.gotmpl` and re-run. Re-run it after **any** `values.yaml` edit — the generated schema is what rejects an install otherwise, and it also refreshes the Kueue `resources.transformations` list from `pkg/nodefeature` between its markers.
- `make lint chart` — lint the chart with `ct lint` in a container, then assert that the `global.*` image knobs reach every image the chart renders, its subcharts' included (`gpustack::helm::verify_images`).
- `make test chart` — install the chart onto the current cluster with `ct install` in a container; needs a reachable cluster (e.g. kind) and `~/.kube/config`.

### Vendored subcharts

Kueue, Node Feature Discovery, `csi-driver-nfs` and `csi-driver-s3` are **vendored unpacked** under
`deploy/gpustack-operator/chart/charts/<name>/`, and those trees are **committed** — which is what
makes `helm install` work from a bare clone and keeps CI offline-capable. `gpustack::chart_staging`
in `hack/deps.sh` pulls each pinned archive, unpacks it, stamps `_VERSION_`, and applies the patches
kept under `hack/deploy/gpustack-operator/chart/charts/<name>/*.patch`. A tree whose stamp already
matches the pinned version is left untouched, so repeated runs are no-ops and a patched tree is never
clobbered.

Vendoring unpacked (rather than depending on the archives) is what lets the patches exist at all:
Helm merges subchart values instead of rendering them, so `global.imageRegistry` cannot be composed
into a subchart's `image.repository` from the parent. The `global-image.patch` in each tree makes the
subchart's own templates read `.Values.global.*` — Helm does propagate `global` into every subchart.

To change an upstream chart:

1. **Never edit a staged tree in place.** Every change lives in a patch file, because the next
   `make deps` that sees a version bump deletes and re-unpacks the tree.
2. Write the patch against the unpacked tree (`git diff` from a scratch copy works), drop it into
   `hack/deploy/gpustack-operator/chart/charts/<name>/`, bump the pinned version in `hack/deps.sh` if
   that is what you are doing, and re-run `make deps`.
3. A patch that no longer applies **fails `make deps`**, and so does one that leaves a `.rej`. That is
   deliberate: without it, an upstream chart that moved under a patch would ship as a half-patched
   tree and nothing would say so. A hunk that merely lands at a **shifted line number** is not that:
   `patch` runs with `-F0`, so every context line must match exactly and it never guesses, which is
   what makes a shift safe to accept silently. Two patches touching one file shift each other, so
   treating a shift as a failure would demand re-capturing one whenever the other is edited.

**Mirror the images before bumping a pinned version.** Every image the chart renders points at a
`gpustack/mirrored-*` repository; a bump to a version whose images are not mirrored yet lands every
install in `ImagePullBackOff`, and `make lint chart` will not catch it (it checks that the override
knobs reach each reference, not that the reference resolves).

`ci-chart.yml` runs all three across the supported Kubernetes matrix, and gates two kinds of drift:
`make generate chart` must leave `README.md`/`values.schema.json` unchanged, and `make deps` must
leave the vendored trees unchanged. Both fail the build with the command to run.

For a full local install → version-consistency → uninstall (zero-leftover) cycle against a real cluster,
use the `gpustack-operator-chart-e2e` skill; for the deep scheduling-chain behavior, use `gpustack-operator-e2e`.

### Commit messages

Commit messages follow [Conventional Commits](https://www.conventionalcommits.org) (`type: subject`),
checked by [commitsar](https://github.com/aevea/commitsar) (`v1.0.3`, pinned in `hack/lib/style.sh`).
The check runs as part of `make lint`, but only when the working tree is clean (`hack/lint.sh` →
`gpustack::commit::lint`), over the commits ahead of `origin/main` (scope in `.commitsar.yml`). Types
in use: `feat`, `fix`, `refactor`, `test`, `docs`, `chore`.

### Running a single test

`make test` only excludes packages, so to target one package or test run `go test` directly with the
required env:

```bash
GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/nodefeature/...
GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race -run TestExtractGeneralNodeKey ./pkg/nodefeature/
```

## API groups & code generation

| Path | Group / Version | Kind |
|------|-----------------|------|
| `api/v1` | `gpustack.ai/v1` | Extension API (settings, status) |
| `api/worker/v1` | `worker.gpustack.ai/v1` | Extension API served by the aggregated apiserver: a proxy/conversion over the v1alpha1 CRDs plus the read-only (get, list, watch) `InstanceTypeFlavor` catalog |
| `api/worker/v1alpha1` | `worker.gpustack.ai/v1alpha1` | CRDs (Instance, Devices, InstanceType) |

`gen/api/main.go` configures which packages are CRDs vs extension APIs and drives custom generators
in `gen/api/generator` (apireg-gen, crd-gen, webhook-gen). **Never hand-edit generated files**
(`zz_generated.*`, `generated.pb.go`, `generated.proto`); edit the source `*.go` types or
`gen/api/main.go` and run `make generate` (the `gpustack-operator-generate` skill automates this).

## Vendored / patched dependencies

`go.mod` `replace`s several k8s modules (`k8s.io/api`, `apimachinery`, `code-generator`,
`apiextensions-apiserver`, `kube-aggregator`, `klog`) plus `gogo/protobuf` and `go-logr/logr` with
patched copies under `./staging/`. These are checked out and patched by `make deps` (sources +
versions in `hack/deps.sh`, patches in `hack/staging/`). Don't hand-edit `staging/`; change the
patch and re-run `make deps`. The chart's subcharts are staged the same way, and for the same
reason — see [Vendored subcharts](#vendored-subcharts).
