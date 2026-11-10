# Copilot Code Review — GPUStack Operator

GPUStack Operator is a Kubernetes operator for accelerator scheduling and workload management,
built on Node Feature Discovery and Kueue. Read [the module map](../docs/README.md#module-map)
for the touched capability and its related modules, then [Architecture](../docs/getting-started/architecture.md)
for the request flow. The shared index owns module, code and spec navigation.

Issue titles use one of `bug: `, `enhancement: `, `support: `, `docs: `, `cleanup: `, `todo: `
and at most 80 characters; PR titles use Conventional Commits (`fix(chart): …`), never issue prefixes.
Link issues with `Fixes #<n>` (resolves and closes), `Addresses #<n>` (advances), or `Relates #<n>`
(context); the latter two do not auto-close. Titles are context, not code defects.

Apply the [code review skill](../.agents/skills/gpustack-operator-code-review/SKILL.md) and the
rules below; where content exclusions apply (such as `_test.go`, generated files, `hack/`, and `gen/`), this file's
exclusion and reminder rules override that skill. Cite the file and line for actionable findings.

## Reading sources for review

Start with [the shared module map](../docs/README.md#module-map), then read the touched module's
guides, owning specs, code entry points and related modules. [AGENTS.md](../AGENTS.md#finding-sources)
owns the retrieval and evidence rules. These local files are sufficient without an external index.

- Review behavior against the PR head and its dependency versions. An index entry is navigation;
  confirm its claims in the cited source before reporting a defect.
- Guides describe public usage; specs preserve design rationale, measurements and limitations.
  Check a spec's status and the current implementation before treating a proposal as shipped behavior.
- For an error or upstream compatibility claim, locate the stable error text, then inspect relevant
  issues, their resolutions and linked PRs. Confirm that the reviewed version contains the cited fix.
- Cite the relevant passage or code location and explain the failing scenario. Distinguish source
  statements from inference, and preserve qualifications when summarizing evidence.
- Public GPUStack APIs use `v1`; controller storage uses `v1alpha1`. Check the public types,
  registrations, proxy handlers, clients and permissions together through the API Changes entry.
  Internal fixtures using `v1alpha1` do not establish a defect in a public example.
- When a guide is shortened, verify that usage and limits remain, and that moved engineering
  evidence exists in its owning spec. Do not request implementation narratives in user guides.
- Flag stale routing when pages, code entry points, owning specs or skill inputs change. Ordinary
  prose edits do not require a mechanical index edit. Site `llms.txt` and Markdown are generated
  from repository sources; review their inputs rather than asking for separately maintained copies.
- For site publication, check the exact tag source revision, version prefixes and shared Pages
  queue. Documentation publication must preserve chart packages and `/charts/index.yaml`.
  Hidden prereleases retain direct links; the default follows the latest published stable site.
  Do not request historical-tag sites built from newer source files.

The review exclusions below still apply. If an excluded implementation or test is needed to verify
a claim, state what could not be checked; do not infer behavior or missing coverage from its absence.

## Review conventions

Omit `low` findings or put them in the review body's suppressed block; keep `medium` and above
inline. This routing is safe only while the suppressed block is read every round.

### Named noise

Omit these measured false positives:

- `kubectl -o jsonpath` JSON-encodes a `[]string`, so comma-splitting `pciSwitches` is correct (measured on kubectl v1.36.3).
- Requests for redundant comments where code already states the rule or the spec owns the history.
- A missing regression test inferred from absence in this review: test files are content-excluded;
  state the requirement as a reminder, never assert presence or absence.

Omit these unless citing the specific measurement or code change that contradicts them.

## Out of scope — do not review

This list mirrors `.opencodereview/rule.json` verbatim and in the same order; Copilot's separate
repository content-exclusion policy must be updated independently.

- `**/*_deepcopy*`
- `**/*_test.go`
- `**/generated.*`
- `**/zz_generated*`
- `.agents/skills/**/*.sh`
- `.claude/skills/**/*.sh`
- `binding/**`
- `deploy/gpustack-operator/chart/*.json`
- `deploy/gpustack-operator/chart/*.md`
- `deploy/gpustack-operator/chart/charts/**`
- `gen/**`
- `hack/**`
- `pkg/extensionroute/swagger/ui/**`
- `pkg/kubeclients/**/*.go`
- `staging/**`

## Hard invariants

- Remind authors that changes to `api/` types or webhooks require `make generate`, and chart
  `values.yaml` changes require `make generate chart`; generated outputs are excluded, so do not
  claim they are present, absent, or hand-edited without visible evidence.
- Remind authors that changes to `staging/` or vendored subcharts belong in patches under `hack/`
  and apply via `make deps`; these paths are excluded from this review.
- When `model-transfer/contracts/` changes wire formats or schemas, require matching updates in
  `pkg/modeltransfer/protocol` and `model-transfer/python/gpustack_model_transfer/core`; fixture-only regeneration does not require both.
- Docs follow a strict contract (`make lint docs`); flag doc edits that break page
  headers, Contents lists, or the `docs/README.md` index.
- Commit messages follow Conventional Commits (`type(scope): subject` or `type: subject`).

## Go conventions

- Handle errors explicitly, avoid panics for control flow, and document exported APIs.
- Name multi-word Go files in `snake_case` (`instance_type.go`, never `instancetype.go`); keep source comments
  plain and short, without emoji, decorative symbols or circled digits; never put spec task IDs in Go comments.
- Flag unsafe shared mutable state and needless complexity.

## Python conventions (`model-transfer/python`)

- Favor precision over recall: only raise a finding when you can name a concrete input or runtime
  scenario that causes a wrong result, resource leak, security hole, or scale failure.
- Flag mutable default arguments (`def f(x=[])`), unintended class-level mutable attributes,
  unsynchronized module-level mutable state, empty inputs indexed without guard (`xs[0]`,
  `max()`/`min()`), unguarded `None`, float equality with `==` instead of `math.isclose`, and
  `is`/`is not` on literals instead of `==`.
- Flag bare `except:`, overly broad `except Exception:` that hides bugs, silent `pass`, lost
  tracebacks without `raise ... from err`, and `assert` validating external runtime inputs.
- Flag resource leaks: files, sockets, `mmap` buffers, locks, or `gpustack_model_transfer`
  readers/sessions opened without `with` or left open on preflight refusal, error, or cancellation.
- Enforce `gpustack_model_transfer` domain invariants:
  - Safetensors header probes must read only header bytes (`8 + header_len`) and cross-check
    dtype, shape, and byte offsets against the verified manifest.
  - Engine adapters (`vllm`, `sglang`, `vllm_ascend`) must preflight the full load plan and
    owned-output budget before mutating any parameter, and never claim clean fallback after mutation.
  - Borrowed-storage views require an explicit lifetime owner and completion fence; `finalize()`
    must release load-only windows and registrations before KV cache profiling.
  - `gpustack_model_transfer` ships as a universal `py3-none-any.whl` with `dependencies = []`;
    vendor libraries (`torch`, `torch_npu`, `nixl`, `hixl`, `vllm`, `sglang`) must import lazily.
  - Preserve wire compatibility between older `gpustack_model_transfer` clients and newer
    `model-transfer` servers: flag removed/renamed wire fields or endpoints, changed derived-digest
    preimages within a contract version, required request fields without server defaults, or
    rejected unknown additive response fields.
- Flag security hazards on untrusted input (`eval`/`exec`/`compile`, `shell=True`, unsafe
  `pickle`/`marshal`/`yaml.load`, unvalidated path joins) and over-engineering (`delete:`, `reuse:`,
  `stdlib:`, `yagni:`, `merge:`, `shrink:`) when code duplicates helpers or adds single-caller layers.

## Kubernetes conventions

- Reconcile idempotently with level-based logic; propagate contexts and tolerate eventual consistency.
- Preserve API compatibility and separate desired `spec` from observed `status`; public GPUStack APIs
  use `v1` and controller storage uses `v1alpha1`.
- Validate external inputs, return typed errors and actionable conditions, and watch only relevant resources.

## Testing conventions

Copilot's content-exclusion policy removes every `_test.go`; absence from view proves nothing.
For tests visible to you, check declarative table-driven cases and shared loops, fixture helpers,
fake clients, valid setup, deterministic behavior, and semantic assertions of observable state.
