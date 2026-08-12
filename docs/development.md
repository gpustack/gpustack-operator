# Development

> **Purpose** — every command you need to build, lint, test and regenerate the operator, plus how the
> vendored subcharts and patched dependencies work.
> **Audience** contributors · **Prerequisites** none (read [Architecture](architecture.md) before
> changing behavior) · **Read time** ~5 min

## Contents

- [Commands](#commands)
- [Runtime log verbosity](#runtime-log-verbosity)
- [API groups & code generation](#api-groups--code-generation)
- [Vendored / patched dependencies](#vendored--patched-dependencies)

## Commands

`make <target> [args]` runs `hack/<target>.sh` and forwards `args` to it. All builds use `CGO_ENABLED=1`,
`GODEBUG=gotypesalias=0` and the build tags `goccy netgo`.

- `make deps` — vendor patched k8s staging modules into `staging/` **and** upstream Helm charts into `deploy/gpustack-operator/chart/charts/`, then `go mod tidy && go mod download`; `make deps update` adds `go get -u ./...`.
- `make generate` — the `gen/api` generators: deepcopy, register, apiservice, CRDs, conversion, protobuf, webhooks. `make generate binding` regenerates the CGO bindings in `binding/` via c-for-go.
- `make lint` — golangci-lint (`.golangci.yaml`); `make lint dirty` also fails on a dirty tree. `make lint docs` checks the documentation contract instead (bash + awk, about a second, no Go toolchain) — see [the docs skill](../.claude/skills/gpustack-operator-docs/SKILL.md).
- `make build` — cross-build `cmd/gpustack-operator` into `.dist/build/`, version ldflag-injected into `pkg/utils/version`; `VERSION=vX.y.z+l.m make build` sets it, `BUILD_PLATFORMS="linux/amd64 linux/arm64"` cross-compiles.
- `make test` — `go test -v -failfast -race -cover -timeout=30m ./...`, coverage to `.dist/test/coverage.out`; trailing args are regexes of packages to **exclude**.
- `make package` — images via `docker buildx` from `pack/*/Dockerfile` (Linux only).

CI (`hack/ci.sh`) runs `make generate && make deps && make lint && make build`.

### Helm chart

`generate`, `lint` and `test` take a `chart` argument, operating on `deploy/gpustack-operator/chart` via
[chart-testing](https://github.com/helm/chart-testing),
[helm-docs](https://github.com/norwoodj/helm-docs) and helm-schema:

- `make generate chart` — regenerate `README.md` (from `README.md.gotmpl`) and `values.schema.json`. **Never hand-edit those two**: edit `values.yaml`/its annotations/`README.md.gotmpl` and re-run, after **any** `values.yaml` edit — the generated schema is what rejects a bad install. Nothing in `values.yaml` is generated: Kueue's `resources.transformations` is rendered at install time by a chart helper a patch adds to its config.
- `make lint chart` — `ct lint` in a container, then assert the `global.*` image knobs reach every image the chart and its subcharts render (`gpustack::helm::verify_images`).
- `make test chart` — `ct install` onto the current cluster in a container; needs a reachable cluster (e.g. kind) and `~/.kube/config`.

### Vendored subcharts

Kueue, Node Feature Discovery, `csi-driver-nfs` and `csi-driver-s3` are **vendored unpacked** under
`deploy/gpustack-operator/chart/charts/<name>/` and **committed**, so `helm install` works from a bare
clone and CI stays offline.

`gpustack::chart_staging` (`hack/deps.sh`) pulls each pinned archive, unpacks it, stamps `_VERSION_` and
applies `hack/deploy/gpustack-operator/chart/charts/<name>/*.patch`; a tree at the pinned stamp is skipped,
so runs are idempotent and a patched tree is never clobbered.

Unpacked is what lets the patches exist: Helm merges subchart values rather than rendering them, so the
parent cannot compose `global.imageRegistry` into a subchart's `image.repository`; each tree's
`global-image.patch` makes the subchart's templates read `.Values.global.*`, which Helm does propagate.

To change an upstream chart:

1. **Never edit a staged tree in place** — a version bump makes `make deps` delete and re-unpack it, so
   every change lives in a patch file.
2. Write the patch against the unpacked tree (`git diff` from a scratch copy works), drop it into
   `hack/deploy/gpustack-operator/chart/charts/<name>/`, bump the pinned version in `hack/deps.sh` if that
   is the change, and re-run `make deps`.
3. A patch that no longer applies, or leaves a `.rej`, **fails `make deps`** — otherwise a moved chart
   ships half-patched and silent. A **shifted** hunk is fine: `patch` runs `-F0`, so context still matches
   exactly, and two patches on one file shift each other.

**Mirror the images before bumping a pinned version**: every image the chart renders points at a
`gpustack/mirrored-*` repository, an unmirrored bump lands every install in `ImagePullBackOff`, and
`make lint chart` only checks that the override knobs reach each reference, not that it resolves.

`chart.yml` runs all three across the supported Kubernetes matrix and gates drift: `make generate chart`
must leave `README.md`/`values.schema.json` unchanged, `make deps` the vendored trees; both fail with the
command to run. For a full install → version-consistency → uninstall cycle on a real cluster use the
`gpustack-operator-chart-e2e` skill, and `gpustack-operator-e2e` for scheduling-chain behavior.

### Commit messages

[Conventional Commits](https://www.conventionalcommits.org) (`type: subject`), checked by
[commitsar](https://github.com/aevea/commitsar) (`v1.0.3`, pinned in `hack/lib/style.sh`) within
`make lint`, but only over a clean tree (`hack/lint.sh` → `gpustack::commit::lint`) and only across the
commits ahead of `origin/main` (scope in `.commitsar.yml`). Types: `feat`, `fix`, `refactor`, `test`,
`docs`, `chore`.

### Running a single test

`make test` only excludes packages, so target one package or test with `go test` directly:

```bash
GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/nodefeature/...
GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race -run TestExtractGeneralNodeKey ./pkg/nodefeature/
```

## Runtime log verbosity

Every component registers `PUT /debug/flags/v` on its own secure port, so klog verbosity can be raised on
a running pod and dropped again without a restart (`pkg/manager/manager.go`, `pkg/worker/worker.go`,
`pkg/workergateway/gateway.go`; the device-manager's port is 32443, from `pkg/devicemanager/option.go`).

```bash
kubectl -n gpustack-system exec <pod> -- \
  curl -sk -X PUT -H "Host: 127.0.0.1" -d '4' https://127.0.0.1:32443/debug/flags/v
# → successfully set klog.logging.verbosity to 4
kubectl -n gpustack-system exec <pod> -- \
  curl -sk -X PUT -H "Host: 127.0.0.1" -d '2' https://127.0.0.1:32443/debug/flags/v
```

`-H "Host: 127.0.0.1"` is **mandatory**: `httpx.LoopbackAccessHandlerFunc` compares `r.Host` against the
bare `127.0.0.1` / `localhost` / `::1` and an ordinary request carries the port (`127.0.0.1:32443`), so
without it the guard answers a plain `404` that reads like a missing route. A `GET` with the header
answers `406 unsupported http method` — the guard passing.

Use it to see a decision logged above the deployment's verbosity. The device plugin is the sharpest case:
its `ResourceServer`s use `Logger: logger.V(3)` (`pkg/devicemanager/allocator/allocator.go`) while the
DaemonSet runs `-v=2`, so `Allocate`/`GetPreferredAllocation` decisions — which accelerator a slice landed
on — are discarded by default.

Raise `v` **before** creating the workload to trace; those lines fire only on an allocation, so a quiet
window afterwards proves nothing. The `gpustack-operator-e2e` skill carries the same recipe as a triage
step, with the operational caveats.

## API groups & code generation

| Path | Group / Version | Kind |
|------|-----------------|------|
| `api/v1` | `gpustack.ai/v1` | Extension API (settings, status) |
| `api/worker/v1` | `worker.gpustack.ai/v1` | Extension API served by the aggregated apiserver: a proxy/conversion over the v1alpha1 CRDs plus the read-only (get, list, watch) `InstanceTypeFlavor` catalog |
| `api/worker/v1alpha1` | `worker.gpustack.ai/v1alpha1` | CRDs (Instance, Devices, InstanceType) |

`gen/api/main.go` configures which packages are CRDs vs extension APIs and drives the custom generators in
`gen/api/generator` (apireg-gen, crd-gen, webhook-gen). **Never hand-edit generated files**
(`zz_generated.*`, `generated.pb.go`, `generated.proto`): edit the source `*.go` types or `gen/api/main.go`
and run `make generate` (the `gpustack-operator-generate` skill automates this).

## Vendored / patched dependencies

`go.mod` `replace`s several k8s modules (`k8s.io/api`, `apimachinery`, `code-generator`,
`apiextensions-apiserver`, `kube-aggregator`, `klog`) plus `gogo/protobuf` and `go-logr/logr` with patched
copies under `./staging/`, checked out and patched by `make deps` (sources + versions in `hack/deps.sh`,
patches in `hack/staging/`). Don't hand-edit `staging/`; change the patch and re-run `make deps`. The
subcharts are staged the same way, for the same reason — see [Vendored subcharts](#vendored-subcharts).

---

**See also** — [Internals](architecture/internals.md) (the invariants the code keeps) ·
[Installation Modes](architecture/installation-modes.md) · [Settings](settings.md)

**Next** → [All documentation](README.md) — pick the next page on the contributor path.
