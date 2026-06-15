# GPUStack Operator

A Kubernetes operator that turns raw node hardware into a Kueue-based scheduling chain for
accelerators (GPU/NPU/TPU), built on Node Feature Discovery (NFD) + Kueue.

## Project Structure

- `cmd/` — single `gpustack-operator` binary entrypoint (three cobra subcommands).
- `pkg/` — implementation: `worker` (control plane), `devicemanager` (per-node DaemonSet), `nodefeature` (label algebra), and supporting packages.
- `api/` — API types: CRDs + aggregated extension APIs.
- `binding/` — generated CGO bindings to vendor GPU runtime/management libraries.
- `gen/` — code generators (`api`, `binding`).
- `hack/` — build/lint/test/deps/generate scripts behind the Makefile.
- `staging/` — patched k8s modules, managed by `make deps`.
- `docs/` — architecture, development, and environment-variable guides.
- `pack/` / `deploy/` — container image builds and deployment manifests.

## Architecture

Three subcommands (`worker`, `worker-gateway`, `device-manager`) drive a four-stage chain: NFD labels
nodes → the Device Manager detects accelerators → the worker profiles node capacity → four
controllers materialize Kueue `ResourceFlavor` → `ClusterQueue` → `Cohort` / `LocalQueue` objects.
`pkg/nodefeature` holds the label algebra.

Read `docs/architecture.md` before touching the scheduling chain or `pkg/nodefeature` — it has the
stage-by-stage detail, label/naming conventions, and a worked example.

## Development

See `docs/development.md` for build/lint/test commands, code generation, and vendored dependencies.
For a guided tour of the directory layout and naming conventions, use the `gpustack-operator-overview` skill;
after editing API types or webhooks, use the `gpustack-operator-generate` skill to run `make generate`.

## Code Style

### Go style is mostly idiomatic Go, with some project-specific conventions:

- **Clarity**: favor clear code over cleverness for easier ongoing maintenance.
- **Linting**: the `gpustack-operator-lint` hook runs `make lint` after Go changes; run it yourself too when editing Go.
- **Errors**: explicitly handle errors; avoid panics for control flow.
- **Interfaces**: keep interfaces small; accept interfaces, return concrete types.
- **Naming**: use concise, meaningful names reflecting their exact purpose.
- **Functions**: write short, single-purpose functions for focused, readable logic.
- **Composition**: favor composition and value semantics over complex inheritance.
- **Concurrency**: keep concurrency simple, safe, and use it sparingly.
- **State**: minimize shared mutable state to prevent concurrency bugs.
- **Documentation**: clearly document exported APIs to explain their intended behavior.

### Kubernetes style follows the project-specific conventions below:

- **Declarative**: Always reconcile desired states instead of scripting imperative actions.
- **Idempotent**: Ensure every controller reconcile operation is perfectly safe to retry.
- **Level-based**: Rely purely on level-based logic and avoid edge-triggered assumptions.
- **Simplicity**: Maintain simple, versioned API types ensuring strict backward compatibility.
- **Separation**: Strictly separate the desired user intent from the observed status.
- **Fail-fast**: Always fail fast by returning typed errors and clear conditions.
- **Context**: Consistently utilize contexts everywhere to honor process cancellations and timeouts.
- **Composition**: Strongly prefer composition over inheritance to maximize controller code reuse.
- **Efficiency**: Minimize reconciliation workloads by watching only what affects desired states.
- **Consistency**: Design systems for eventual consistency rather than expecting immediate convergence.