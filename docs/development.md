# Development

Build, lint, test, code generation, and dependency management for GPUStack Operator. For the
runtime architecture see [architecture.md](architecture.md); for the `GPUSTACK_*` knobs see
[environment-variables.md](environment-variables.md).

## Commands

The `Makefile` dispatches each target to `hack/<target>.sh`; the target name *is* the script name,
and `make <target> [args]` forwards `args` to the script. All builds use `CGO_ENABLED=1` and
`GODEBUG=gotypesalias=0`; default build tags are `goccy netgo`.

- `make deps` — vendor patched k8s staging modules into `staging/`, then `go mod tidy && go mod download`. `make deps update` also runs `go get -u ./...`.
- `make generate` — run `gen/api` code generators (deepcopy, register, apiservice, CRDs, conversion, protobuf, webhooks). `make generate binding` regenerates the CGO bindings in `binding/` via c-for-go.
- `make lint` — run golangci-lint (config in `.golangci.yaml`). `make lint dirty` additionally fails if the git tree is dirty afterward.
- `make build` — cross-build `cmd/gpustack-operator` into `.dist/build/`. Version is injected via ldflags into `pkg/utils/version`. `VERSION=vX.y.z+l.m make build` sets the version; `BUILD_PLATFORMS="linux/amd64 linux/arm64"` cross-compiles.
- `make test` — `go test -v -failfast -race -cover -timeout=30m ./...`; coverage to `.dist/test/coverage.out`. Trailing args are treated as regex patterns of packages to **exclude**.
- `make package` — build container images via `docker buildx` from `pack/*/Dockerfile` (Linux only).

CI (`hack/ci.sh`) runs: `make generate && make deps && make lint && make build`.

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
| `api/worker/v1` | `worker.gpustack.ai/v1` | Extension API (Instance, Devices, InstanceType, …) served by the aggregated apiserver |
| `api/worker/v1alpha1` | `worker.gpustack.ai/v1alpha1` | CRDs (Instance, Devices) |

`gen/api/main.go` configures which packages are CRDs vs extension APIs and drives custom generators
in `gen/api/generator` (apireg-gen, crd-gen, webhook-gen). **Never hand-edit generated files**
(`zz_generated.*`, `generated.pb.go`, `generated.proto`); edit the source `*.go` types or
`gen/api/main.go` and run `make generate` (the `gpustack-operator-generate` skill automates this).

## Vendored / patched dependencies

`go.mod` `replace`s several k8s modules (`k8s.io/api`, `apimachinery`, `code-generator`,
`apiextensions-apiserver`, `kube-aggregator`, `klog`) plus `gogo/protobuf` and `go-logr/logr` with
patched copies under `./staging/`. These are checked out and patched by `make deps` (sources +
versions in `hack/deps.sh`, patches in `hack/staging/`). Don't hand-edit `staging/`; change the
patch and re-run `make deps`.
