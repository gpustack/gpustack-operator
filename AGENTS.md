# GPUStack Operator

A Kubernetes operator that turns raw node hardware into a Kueue-based scheduling chain for
accelerators (GPU/NPU/TPU), built on Node Feature Discovery (NFD) + Kueue.

## Project Structure

- `cmd/` — single `gpustack-operator` binary entrypoint (the cobra subcommands).
- `pkg/` — implementation: `worker` (control plane), `devicemanager` (per-node DaemonSet), `nodefeature` (label algebra), and supporting packages.
- `api/` — API types: CRDs + aggregated extension APIs.
- `binding/` — generated CGO bindings to vendor GPU runtime/management libraries.
- `csrc/` — hand-written C sources for the vendor preload libraries injected into sliced workload containers (`thead/ppu-slicing-shim`, `amd/rocm-slicing-shim`).
- `gen/` — code generators (`api`, `binding`).
- `hack/` — build/lint/test/deps/generate scripts behind the Makefile.
- `staging/` — patched k8s modules, managed by `make deps`.
- `testing/` — end-to-end test infrastructure (`testing/infra`).
- `docs/` — architecture, development, and environment-variable guides.
- `pack/` / `deploy/` — container image builds and deployment manifests.

## Architecture

Three subcommands (`worker`, `worker-gateway`, `device-manager`) drive a four-stage chain: NFD labels
nodes → the Device Manager detects accelerators → the worker profiles node capacity → the
controllers under `pkg/worker/controllers/worker` materialize Kueue `ResourceFlavor` →
`ClusterQueue` (one isolated queue per pool) → `LocalQueue` plus an `InstanceType` CRD.
`pkg/nodefeature` holds the label algebra.

Read `docs/architecture.md` first: one page, the four stages, the life of a request, the vocabulary.
Then the deep page under `docs/architecture/` for what you are touching — `device-discovery.md` (NFD,
Device Manager, allocator), `scheduling-chain.md` (capacity labels, flavors/queues/InstanceTypes,
`pkg/nodefeature`), `admission.md` (the five gates, webhooks, four-view status), `installation-modes.md`,
`internals.md` (startup order and the invariants that fail silently). `docs/README.md` indexes it all.

## Development

See `docs/development.md` for build/lint/test commands, code generation, and vendored dependencies.

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

The `gpustack-operator-lint` hook dispatches on what a turn left dirty and implements this table;
`hack/check-hook-dispatch.sh` asserts that it still does. It is report-only and runs once the turn
is over, so run the matching target yourself when you need the answer before that. The last row is a
default, not a list, so a file type nobody named still has an answer:

- Markdown → `make lint docs`
- the chart → `make lint chart`
- every other source, Go and shell included → `make lint`

REQUIRED: a change touching more than one subject runs more than one target — `make lint docs`
returning 0 says nothing about a `.sh` in the same commit.

### Go conventions

- Prefer clarity over cleverness to simplify long-term code maintenance.
- Run lint checks locally whenever modifying Go source code.
- Handle errors explicitly; never use panics for control flow.
- Keep interfaces minimal; accept abstractions, return concrete implementations.
- Use concise names accurately reflecting purpose and domain meaning.
- Name multi-word Go source files in snake_case (`instance_type.go`, `node_queue.go`), never flat-concatenated (`instancetype.go`).
- Write focused functions performing one responsibility and nothing else.
- Prefer composition and values over inheritance-like design patterns.
- Keep concurrency simple, safe, justified, and minimally applied.
- Minimize mutable shared state to reduce synchronization complexity.
- Document exported APIs with behavior, expectations, and constraints.
- Keep comments plain and short: no emoji, no decorative symbols, no circled digits. State the point
  in words. Applies to every commented source file, Go and shell alike.
- NEVER put spec task identifiers in Go comments; state the rule directly because labels require a
  second document and outlive it.

### Kubernetes conventions

- Reconcile desired state continuously instead of executing imperative workflows.
- Ensure reconciliation remains idempotent and safely repeatable always.
- Depend on level-based logic, avoiding edge-triggered behavioral assumptions.
- Design stable APIs preserving backward compatibility across versions.
- Separate desired specification clearly from observed status information.
- Return typed errors early with actionable conditions and messages.
- Propagate contexts consistently for cancellation, deadlines, and timeouts.
- Prefer composition patterns maximizing reuse across controller implementations.
- Watch only relevant resources affecting desired reconciliation outcomes.
- Design for eventual consistency rather than immediate convergence guarantees.

### Testing conventions

- Structure tests using table-driven cases and shared execution loops.
- Verify exactly one behavior or contract per test case.
- Keep test cases declarative, containing data without execution logic.
- Centralize execution flow rather than duplicating testing procedures.
- Build fixtures through helpers for consistency and maintainability.
- Assert observable final state instead of implementation details.
- Prefer fake clients over real dependencies during testing.
- Fail immediately when setup errors invalidate test assumptions.
- Compare semantic equivalence rather than incidental representation differences.
- Ensure tests produce deterministic and repeatable execution outcomes.
- Follow established project testing patterns and organizational conventions.
