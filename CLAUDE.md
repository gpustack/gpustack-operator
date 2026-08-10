# GPUStack Operator

A Kubernetes operator that turns raw node hardware into a Kueue-based scheduling chain for
accelerators (GPU/NPU/TPU), built on Node Feature Discovery (NFD) + Kueue.

## Project Structure

- `cmd/` — single `gpustack-operator` binary entrypoint (three cobra subcommands).
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
nodes → the Device Manager detects accelerators → the worker profiles node capacity → five
controllers materialize Kueue `ResourceFlavor` → `ClusterQueue` (one isolated queue per pool, no
Cohort) → `LocalQueue` plus an `InstanceType` CRD. `pkg/nodefeature` holds the label algebra.

Read `docs/architecture.md` first — one page, the four stages, the life of a request, the vocabulary.
Then the deep page for what you are touching: `docs/architecture/discovery.md` (NFD, Device Manager,
allocator), `scheduling-chain.md` (capacity labels, flavors/queues/InstanceTypes, `pkg/nodefeature`),
`admission.md` (the five gates, webhooks, four-view status), `install-modes.md`, `internals.md`
(startup order and the invariants that fail silently). `docs/README.md` indexes all of it.

## Development

See `docs/development.md` for build/lint/test commands, code generation, and vendored dependencies.
For a guided tour of the directory layout and naming conventions, use the `gpustack-operator-overview` skill;
after editing API types or webhooks, use the `gpustack-operator-generate` skill to run `make generate`;
when a change needs a doc change, use the `gpustack-operator-docs` skill — it routes the fact to the
right page and checks the index, links and tables of contents.
The `gpustack-operator-lint` hook runs `make lint` after Go changes; run it yourself too when editing Go.

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
