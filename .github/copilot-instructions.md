# Copilot Code Review — GPUStack Operator

GPUStack Operator is a Kubernetes operator that turns raw node hardware into a
Kueue-based scheduling chain for accelerators (GPU/NPU/TPU), built on Node Feature
Discovery (NFD) + Kueue. One binary (`cmd/gpustack-operator`) runs three subcommands
— `worker` (control-plane controllers + aggregated extension API), `worker-gateway`,
and `device-manager` (per-node DaemonSet) — driving a four-stage chain: NFD labels
nodes → the Device Manager detects accelerators and maintains the `Devices` CR
ledger (the single authoritative record of who holds what) → the worker profiles
per-node capacity labels → controllers materialize Kueue `ResourceFlavor` →
`ClusterQueue` → `LocalQueue`, plus an
`InstanceType` CRD with a four-view (EX/SH/SL/PT) status. Accelerator quota is
`credits.gpustack.ai/<manufacturer>`, one whole accelerator = `M = 1,600,000`
integer credit units. Read `docs/architecture.md` first, then the deep page under
`docs/architecture/` for the area a PR touches.

Layout: controllers and reconcilers live under `pkg/` (`worker`, `devicemanager`,
`nodefeature` — the label algebra), API types under `api/`, CGO bindings under
`binding/`, hand-written C preload shims under `csrc/`, patched k8s modules under
`staging/`, and the Helm chart under `deploy/gpustack-operator/chart`.

Conventions, for context rather than review: an issue title starts with exactly one of `bug: `,
`enhancement: `, `support: `, `docs: `, `cleanup: `, `todo: ` and stays at or under 80 characters
including the prefix. A PR title stays Conventional Commits (`fix(chart): …`) — issue prefixes never
appear on a PR — and a PR links its issue with one of three verbs: `Fixes #<n>` (fully resolved,
GitHub closes it on merge), `Addresses #<n>` (advances it, no auto-close), or `Relates #<n>` (context
only, no auto-close). None of this is enforced anywhere, and none of it is a review rule: a title is
not a code defect.

When performing a code review, use the `gpustack-operator-code-review` skill in
`.claude/skills/gpustack-operator-code-review/SKILL.md`, and apply the rules below. Keep feedback
specific and actionable; cite the file and line.

## Review conventions — where a low-severity finding goes

A `low` severity finding is **not** posted as an inline review comment. Either omit it, or put it in
the review body's suppressed block. Everything `medium` and above stays inline, unchanged.

The reason is reviewer attention, not correctness: on a large PR the inline list is the surface a
human reads line by line, and filling it with advisory notes buries the findings that must be acted
on before merge. Moving them keeps them available without spending that surface.

> **This rule is safe only while the suppressed block is read every round, and it fails closed
> nowhere.** Severity does not predict whether a finding is real — on this repository a `high` has
> been wrong six times running while a round of `low`s was entirely right, and findings that first
> appeared suppressed have later been confirmed as real defects. So this routes noise out of one
> list; it does not decide anything about truth. The moment the suppressed block stops being read,
> the rule stops reducing noise and starts hiding true positives, and nothing in the output
> distinguishes those two states. Whoever drops that reading step owns removing this section too.

### Named noise — do not report these at any severity

Severity is a poor filter because it does not predict truth. These are the findings this repository
has *measured* as wrong, so they are filtered by what they claim instead:

- **`kubectl -o jsonpath` renders a `[]string` with Go's `%v`, space-separated.** It does not:
  the jsonpath printer JSON-encodes a slice, so `pciSwitches` prints `["0000:01:00.0","0000:00:01.0"]`
  and splitting on commas is correct. Measured on kubectl v1.36.3 and on real multi-bridge hardware.
  Reported six times, wrong six times, and acting on it introduces the very mis-parse it warns about.
- **"Add a comment explaining this" on code that already carries the rule.** `CLAUDE.md` requires
  source comments to stay plain and short and to state logic rather than history; a request for more
  prose, for restating what the spec owns, or for keeping a revision narrative in a hot path
  contradicts it. Review against that convention, not toward more text.

A finding in either class should be omitted. If it seems to apply anyway, say which measurement it
contradicts — that is the only form of it worth a reviewer's attention.

## Out of scope — do not review

- `binding/` (generated CGO bindings), `staging/` (patched k8s modules).
- Files matching `zz_generated*`, `*_deepcopy*`, `generated.pb.go`, `generated.proto`,
  `generated.protomessage.pb.go`.
- Vendored subcharts under `deploy/gpustack-operator/chart/charts/*`.

## Hard invariants — flag as required changes

- PRs that edit `api/` types or webhooks must include regenerated code
  (`make generate`). Never hand-edit generated files.
- PRs that edit `deploy/gpustack-operator/chart/values.yaml` must regenerate
  `README.md` and `values.schema.json` (`make generate chart`); those two files are
  never hand-edited.
- Nothing under `staging/` or a vendored subchart tree may be edited in place —
  changes belong in patch files under `hack/` and apply via `make deps`.
- Docs follow a strict contract (`make lint docs`); flag doc edits that break page
  headers, Contents lists, or the `docs/README.md` index.
- Commit messages follow Conventional Commits (`type: subject`).

## Go conventions

- Favor clear code over cleverness; flag needless complexity or speculative abstraction.
- Errors must be handled explicitly; flag panics used for control flow.
- Keep interfaces small — accept interfaces, return concrete types.
- Use concise, meaningful names; multi-word Go files are snake_case
  (`instance_type.go`), never flat-concatenated (`instancetype.go`).
- Prefer short, single-purpose functions; favor composition and value semantics.
- Keep concurrency simple and minimal; flag shared mutable state that risks data races.
- Exported APIs need doc comments describing behavior, expectations, and constraints.
- Comments stay plain and short; flag emoji, decorative symbols and circled digits in any commented
  source file, Go and shell alike.

## Kubernetes conventions

- Reconcile desired state declaratively; flag imperative, scripted actions.
- Every reconcile must be idempotent and safe to retry.
- Use level-based logic; flag edge-triggered assumptions.
- API types stay simple, versioned, and backward compatible — flag breaking changes
  to existing fields.
- Strictly separate desired user intent (`spec`) from observed `status`.
- Fail fast with typed errors and clear conditions.
- Thread `context.Context` through call paths to honor cancellation and timeouts.
- Watch only what affects desired state; flag reconcile triggered by irrelevant objects.
- Design for eventual consistency, not immediate convergence.

## Testing conventions

- Prefer table-driven tests with a shared execution loop; flag duplicated per-case logic.
- Each case verifies one behavior; keep cases declarative — data, not control flow.
- Build fixtures through shared helpers.
- Assert observable final state, not implementation details.
- Prefer fake clients over real dependencies.
- Fail fast on setup errors instead of letting them corrupt later assertions.
- Compare semantic equivalence, not incidental representation; flag brittle string/format comparisons.
- Tests must be deterministic; flag time-, ordering-, or randomness-dependent assertions.
