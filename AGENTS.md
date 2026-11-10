# GPUStack Operator

A Kubernetes operator that turns raw node hardware into a Kueue-based scheduling chain for
accelerators (GPU/NPU/TPU), built on Node Feature Discovery (NFD) + Kueue.

## Project Structure

- `cmd/` — single `gpustack-operator` binary entrypoint (the cobra subcommands).
- `pkg/` — implementation: `worker` (control plane), `devicemanager` (per-node DaemonSet), `nodefeature` (label algebra), `modelmanager` (node cache and CSI), `modeltransfer` (per-node remote-weight source broker), and supporting packages.
- `api/` — API types: CRDs + aggregated extension APIs.
- `model-transfer/` — shared transfer contracts and portable Python `gpustack_model_transfer` package.
- `binding/` — generated CGO bindings to vendor GPU runtime/management libraries.
- `csrc/` — hand-written C sources for the vendor preload libraries injected into sliced workload containers (`thead/ppu-slicing-shim`, `amd/rocm-slicing-shim`).
- `gen/` — code generators (`api`, `binding`).
- `hack/` — build/lint/test/deps/generate scripts behind the Makefile.
- `staging/` — patched k8s modules, managed by `make deps`.
- `testing/` — end-to-end test infrastructure (`testing/infra`).
- `docs/` — architecture, development, and environment-variable guides.
- `pack/` / `deploy/` — container image builds and deployment manifests.

## Architecture

The `worker`, `worker-gateway` and `device-manager` subcommands drive the scheduling chain: NFD labels
nodes → the Device Manager detects accelerators → the worker profiles node capacity → the
controllers under `pkg/worker/controllers/worker` materialize Kueue `ResourceFlavor` →
`ClusterQueue` (one isolated queue per pool) → `LocalQueue` plus an `InstanceType` CRD.
`pkg/nodefeature` holds the label algebra. The `model-manager` subcommand is a per-node CSI plugin that
mounts Hugging Face weights from a verified node cache (`pkg/modelmanager`, `NodeModelStore`).
The `model-transfer` subcommand runs the remote-weight source broker (`pkg/modeltransfer`).

Read `docs/getting-started/architecture.md` for the scheduling chain and vocabulary; use
[the module map](docs/README.md#module-map) to find capability guides, code and related modules.

## Finding sources

Use the local [module map](docs/README.md#module-map) to find the relevant guides, specs,
code and related modules before searching. Confirm the checkout, branch, dirty state and dependency
versions before reasoning about behavior; site docs or a merged PR may describe another revision.

- Search stable error text locally first; for upstream failures, inspect issues, resolutions and
  linked PRs before relying on an opening report, then verify that the current version contains the
  fix. Drop variable paths, line numbers and IDs when an exact search fails.
- Navigation entries point to evidence and are not proof. Cite source passages or file locations
  for consequential claims; distinguish observations, source statements and inference, preserving
  conditions and uncertainty.
- Guides own usage contracts, specs own rationale and measurements, and current code establishes
  implementation. Check spec status and versions; report disagreements.
- REQUIRED: update the shared index in the same change when adding or moving a page, changing a
  module boundary or code entry point, replacing an owning spec, or changing a skill's document
  inputs. Ordinary wording edits
  need no index change if routing remains accurate. Run `make lint docs`.

## Development

See `docs/contribute/development.md` for build/lint/test commands, code generation, and vendored dependencies.

These build, deploy or publish, so they are explicit-only: your host never lists them, and offering
one by name is your job. The change on the left is the trigger.

- the chart, in-cluster app installation, the image build → `gpustack-operator-chart-e2e`
- reconcile, an admission webhook, the extension-apiserver, in-cluster app installation
  → `gpustack-operator-e2e`
- a pinned version in `hack/deps.sh`, a patch under `hack/deploy/`, a vendored tree edited in
  place, a bundled chart added or dropped, or `make deps` leaving a `.rej`
  → `gpustack-operator-chart-subcharts-manage`
- shipping a version → `gpustack-operator-release`
- `pack/gpustack-operator/Dockerfile`, `pack/gpustack-operator/external/`, `pack/thead-ppu-devel/`,
  a slicing shim under `csrc/`, or `pkg/devicemanager/allocator/hygon/`
  → `gpustack-operator-xbuild-and-verify`
- a `ModelDeployment` serving topology, router, KV/EC connector, or benchmark page under
  `docs/modules/model-deployment/` → `gpustack-operator-benchmark`

The report-only `gpustack-operator-lint` hook runs after the turn; run these targets yourself
when needed. `hack/check/hook-dispatch.sh` tests its routing:

- Markdown → `make lint docs`
- the chart → `make lint chart`
- Python under `model-transfer/python/` → `make lint python` (`uv run ruff check` + `uv run ruff format --check`)
- shell under `.agents/skills/` and nothing else non-markdown → `make lint agents-shell`, the
  `.agents` shell gate plus the symbol check, the two steps that read such files
- every other source, Go and shell included → `make lint`

REQUIRED: a change touching more than one subject runs more than one target — `make lint docs`
returning 0 says nothing about a `.sh` in the same commit.

### Go conventions

- Use `snake_case` for multi-word Go filenames (`instance_type.go`, never `instancetype.go`); accept
  interfaces and return concrete types.
- Handle errors explicitly and do not use panics for control flow.
- Document exported APIs with behavior and constraints. Keep comments in all source files plain
  and short, without emoji, decorative symbols or circled digits; never put spec task IDs in Go comments.
- Run local lint after Go changes.

### Kubernetes conventions

- Reconcile idempotently with level-based logic, propagate contexts, tolerate eventual consistency,
  and prefer reusable composition across controllers.
- Keep desired `spec` separate from observed `status`; preserve API compatibility. Public GPUStack APIs
  use `v1`, while controller storage uses `v1alpha1`.
- Validate external inputs, return typed errors and actionable conditions, and watch only relevant resources.

### Testing conventions

- Use table-driven tests with declarative cases and a shared execution loop; build fixtures with
  helpers. Verify one behavior per case with deterministic, repeatable execution.
- Prefer fake clients, fail on invalid setup, and assert observable state and semantic equivalence.
