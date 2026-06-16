# Copilot Code Review — GPUStack Operator

GPUStack Operator is a Kubernetes operator that turns raw node hardware into a
Kueue-based scheduling chain for accelerators (GPU/NPU/TPU), built on Node Feature
Discovery (NFD) + Kueue. The codebase is mostly controllers and reconcilers under
`pkg/` (`worker`, `devicemanager`, `nodefeature`), API types under `api/`, and CGO
bindings under `binding/`.

When reviewing a pull request, apply the conventions below. Do not flag generated or
vendored code: skip `binding/`, files matching `zz_generated*` or `*_deepcopy*`, and
anything under `staging/` (patched k8s modules). Keep feedback specific and actionable,
and cite the file and line.

## Go conventions

- Favor clear code over cleverness; flag needless complexity or speculative abstraction.
- Errors must be handled explicitly; flag panics used for control flow.
- Keep interfaces small — accept interfaces, return concrete types.
- Use concise, meaningful names that reflect their exact purpose.
- Prefer short, single-purpose functions.
- Favor composition and value semantics over inheritance.
- Keep concurrency simple and minimal; flag shared mutable state that risks data races.
- Exported APIs need doc comments describing intended behavior.

## Kubernetes conventions

- Reconcile desired state declaratively; flag imperative, scripted actions.
- Every reconcile must be idempotent and safe to retry.
- Use level-based logic; flag edge-triggered assumptions.
- API types must stay simple, versioned, and backward compatible — flag breaking
  changes to existing fields.
- Strictly separate desired user intent (`spec`) from observed `status`.
- Fail fast with typed errors and clear conditions.
- Thread `context.Context` through call paths to honor cancellation and timeouts.
- Watch only what affects desired state; flag reconcile work triggered by irrelevant objects.
- Design for eventual consistency, not immediate convergence.

## Testing conventions

- Prefer table-driven tests with a shared execution loop; flag duplicated per-case execution logic.
- Each case should verify one behavior or contract; flag cases asserting many unrelated things.
- Keep cases declarative — data, not control flow — and build fixtures through shared helpers.
- Assert observable final state, not implementation details.
- Prefer fake clients over real dependencies.
- Fail fast on setup errors instead of letting them corrupt later assertions.
- Compare semantic equivalence, not incidental representation; flag brittle string/format comparisons.
- Tests must be deterministic and repeatable; flag time-, ordering-, or randomness-dependent assertions.

## Review focus

- Correctness and idempotency of reconcile logic.
- Backward compatibility of API type changes (run `make generate` is required after
  editing `api/` types or webhooks — flag PRs that change these without regenerated code).
- Error handling and context propagation.
- Test quality: table-driven structure, deterministic assertions, fake clients over real dependencies.