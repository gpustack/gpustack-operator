---
name: gpustack-operator-code-review
description: "Review a GPUStack Operator pull-request diff on two axes reported separately — Standards (does it follow this repo's conventions) and Spec (does it implement what was asked) — with a severity label on every finding."
---

# GPUStack Operator — code review

Use [the shared module map](../../../docs/README.md#module-map) to locate the relevant guides, owning specs, code
entry points and related modules. [AGENTS.md](../../../AGENTS.md#finding-sources) owns the retrieval and evidence
rules; this skill owns its execution procedure.

Report the PR diff on two separate axes; never merge or rerank them:

- **Standards** — does the code follow `AGENTS.md`, `.github/copilot-instructions.md`, and `docs/`?
- **Spec** — does it implement the PR description, linked issue, or `specs/` document?

Read `docs/getting-started/architecture.md` and the relevant deep page to identify the affected stage of the
scheduling chain.

## Procedure

### 1. Pin the diff and intent

- Review the PR's base…head changed files; if empty or only out of scope, stop and say so.
- Find the spec in order: PR description, linked issues, matching `specs/` file. If none exists, report "no spec
  available" and limit Spec to obvious internal inconsistencies.
- Read callers and callees of each changed function and its tests. For signature, return value, or shared wire
  contract changes (`model-transfer/contracts/`, `pkg/modeltransfer`, `model-transfer/python`), search every caller
  across Go and Python.
- For each finding, confirm a concrete input or runtime scenario, caller, possible empty/`None` value, or resource
  leak by rereading the relevant lines.

### 2. Review tests first

- Check changed runtime behavior has tests; a bug fix without a regression test is a finding. Documentation uses
  existing lint checks; do not require tests that read Markdown.
- Check Go `*_test.go` uses declarative table-driven cases, a shared loop and fixture helpers. Check Python
  `model-transfer/python/tests/` with `uv run pytest -W error::ResourceWarning` for deterministic fixtures and zero
  leaked handles.
- Check tests use fake clients, assert observable final state, avoid time, order and randomness dependence, and
  would catch a regression if implementation changed.

### 3. Review the implementation across five axes

**Correctness**

- Reconcile logic: idempotent and safe to retry, level-based (no edge-triggered assumptions), eventual consistency
  tolerated, `context.Context` propagated.
- Errors handled explicitly on every path, not just the happy path; typed errors and actionable conditions; no
  panics for control flow.
- `Devices` ledger accounting stays consistent — every allocation path has a matching release path; nothing reads
  or writes allocation state outside the ledger.
- Edge cases: nil/empty inputs, boundary values, not-found objects, requeue behavior.
- Python (`model-transfer/python/**`) correctness and boundary handling:
  - Mutable default arguments (`def f(x=[])`, `def f(x={})`), unintended class-level mutable attributes, or
    module-level mutable globals mutated across sessions or threads.
  - Empty inputs assumed non-empty (`xs[0]`, `max()`/`min()` on empty iterables), off-by-one slice bounds, `None`
    reaching code that assumes a value, exact float equality with `==` instead of `math.isclose`, or `is`/`is not`
    used on string/number/tuple literals instead of `==` (reserve `is` for singletons/sentinels like `None`).
  - Exception handling: bare `except:`, overly broad `except Exception:` masking unexpected bugs, silent swallowing
    (`pass`), lost tracebacks instead of `raise NewError(...) from err`, or `assert` used for runtime validation of
    external inputs (stripped under `python -O`).
  - Resource management: files, sockets, `mmap` buffers, locks, or `gpustack_model_transfer` readers/sessions
    opened without a `with` statement or leaked when preflight, refusal, or cancellation exits early.
  - `gpustack_model_transfer` adapter & reader invariants: safetensors header probes must read only header bytes
    (`8 + header_len`) and cross-check dtype/shape/offsets against the verified manifest; loaders must preflight
    the entire plan and owned-output budget before mutating any model parameter and never claim clean fallback
    after partial model mutation; borrowed-storage views require an explicit lifetime owner and completion fence;
    `finalize()` must release all load-only windows and registrations before KV cache profiling and enforce
    single-call / load-group fences.

**Readability**

- Names reflect domain meaning (`gKey`/`aKey`, pool, entrance, credits, four-view); multi-word Go files are
  snake_case.
- Control flow is straightforward; no clever tricks, deep nesting, or speculative abstraction.
- Exported APIs carry doc comments describing behavior and constraints; source comments stay plain and short,
  without emoji, decorative symbols or circled digits, and Go comments omit spec task IDs.

**Architecture**

- The change lives in the layer that owns the concept: label algebra in `pkg/nodefeature`, device
  detection/allocation in `pkg/devicemanager`, scheduling-chain materialization in `pkg/worker` controllers,
  admission rules in the webhooks / AdmissionCheck controllers, per-node remote-weight source broker in
  `pkg/modeltransfer`, shared wire schemas in `model-transfer/contracts`, and portable Python
  reader/adapters/backends in `model-transfer/python`. Flag feature logic leaking into shared or general-purpose
  packages.
- `model-transfer/python` packaging contract: `gpustack_model_transfer` ships as a single universal pure-Python
  wheel (`py3-none-any.whl`) staged from `/etc/gpustack/lib/model-transfer` to
  `/var/lib/gpustack/operator/lib/model-transfer` by the `model-transfer` source DaemonSet's `stage-libs`
  initContainer, and installed offline on demand when `gpustack_model_transfer` is not already present. Core
  `dependencies` in `model-transfer/python/pyproject.toml` must stay empty (`[]`); vendor and engine packages
  (`torch`, `torch_npu`, `nixl`, `hixl`, `vllm`, `sglang`) and C runtimes (`libascendcl.so`, `libcudart.so`) must
  be imported lazily at runtime or accessed via `ctypes` so one wheel works across CUDA 12/13 and CANN
  910B(A2)/A3/950(A5).
- Pool isolation invariants hold: one ClusterQueue per pool, no cross-pool borrowing.
- The credits model stays integer (`M = 1,600,000` per accelerator); flag any floating-point or percentage
  arithmetic that erodes it.
- API changes stay backward compatible; `spec` and `status` remain strictly separated. Public GPUStack APIs use
  `v1`; controller storage uses `v1alpha1`.
- `model-transfer` client-server wire compatibility: because workload images may carry a pre-installed older
  `gpustack_model_transfer` client while the cluster upgrades `gpustack-operator` (`model-transfer` server / source
  worker), flag any change in `pkg/modeltransfer`, `model-transfer/contracts`, or `model-transfer/python` that
  breaks an older `gpustack_model_transfer` client talking to a newer `model-transfer` server (renaming/removing
  fields or endpoints, changing derived-digest preimages within a contract version, requiring new request fields without
  server-side defaults, or failing on unknown additive server response fields).
- Does a refactor reduce the number of concepts a reader must hold, or just relocate them? Relocation is not
  simplification.

**Security**

- Webhook and API inputs are validated before use; external data (labels, CR fields, node metadata, model-transfer
  descriptors, manifest paths) is treated as untrusted.
- No secrets in code, logs, or fixtures.
- CGO boundaries (`binding/`, `csrc/` callers) handle C-returned memory and error codes correctly; injected preload
  shims keep paths and values quoted/escaped.
- Python (`model-transfer/python/**`) security: no `eval`/`exec`/`compile` on untrusted input, no `subprocess` with
  `shell=True` on unsanitized input, no `pickle`/`marshal`/`yaml.load` without `SafeLoader` on untrusted data, and
  no unvalidated file path joins that permit path traversal (`..` or symlink escape outside the verified tree).

**Performance**

- Reconcile hot paths: no unbounded lists, no per-object API round-trips that belong in a cache, no work
  proportional to cluster size in a per-object reconcile.
- Watches/informers are scoped to resources that actually affect desired state; flag broad watches that would
  trigger reconcile storms.
- Python (`model-transfer/python/**`) scale and hot paths: flag check-then-act races on shared state without a
  `Lock`, blocking synchronous I/O or `time.sleep` inside `async def`, unawaited `asyncio` tasks, CPU-bound loops
  parallelized with `threading` under the GIL, repeated `list` membership scans where a `set`/`dict` turns O(n)
  into O(1), string `+=` in loops, and eager f-strings in `logging` calls on hot paths.

### 4. Check the repo's hard invariants

Each of these is a **required** finding, not a judgement call:

- `api/` types or webhooks changed without regenerated `zz_generated.*` / CRD / protobuf output
  (`make generate`), or generated files are hand-edited.
- Chart `values.yaml` changed without regenerated `README.md` / `values.schema.json` (`make generate chart`), or
  either file is hand-edited.
- In-place edits under `staging/` or `deploy/gpustack-operator/chart/charts/*` instead of patch files under
  `hack/`.
- Doc changes that break the docs contract (page header, Contents list, footer, `docs/README.md` index)
  — `make lint docs` must pass.
- `model-transfer/contracts/` wire format or schema changes without matching updates in `pkg/modeltransfer/protocol`
  and `model-transfer/python/gpustack_model_transfer/core` (fixture-only regeneration does not require both), or `model-transfer/python/` changes that fail
  `uv run ruff check .`, `uv run ruff format --check .`, or `uv run pytest -W error::ResourceWarning`.

### 5. Over-engineering pass

Sweep separately for complexity, not correctness or security; give each finding one line with location, tag, what
to cut and replacement.

- `delete:` dead code, unused flexibility, speculative features. Replacement: nothing.
- `reuse:` the repo already has this helper (name the path).
- `stdlib:` hand-rolled logic that the Go or Python standard library or an already-imported dependency
  (apimachinery, klog, controller-runtime) ships. Name the function.
- `native:` code reimplementing what Kubernetes/controller-runtime already does (finalizers, owner references, rate
  limiters, workqueues). Name the feature.
- `yagni:` an abstraction with one implementation, a knob nobody sets, a layer with one caller. Inline it until a
  second user exists.
- `merge:` near-copies that must change together.
- `shrink:` same logic in fewer lines. Show the shorter form.

End the pass with `net: -N lines possible`. If there is nothing to cut, write `Lean already.` and stop. Never flag
a test or a deliberate doc comment for deletion.

### 6. Aggregate and label

Report separately under `## Standards` and `## Spec`, labeling every finding by severity. This is a report, not a
merge-blocking verdict; the OCR column maps to OpenCodeReview levels:

| Prefix | Meaning | OCR level |
|---|---|---|
| **Critical:** | Data loss, broken reconcile, security hole, ledger inconsistency — must fix | `critical` |
| *(no prefix)* | Required change — convention or spec violation that must be addressed | `high` |
| **Optional:** / **Consider:** | Worth considering, not required | `medium` |
| **Nit:** | Minor style preference; the author may ignore | `low` |
| **FYI** | Context only, no action needed | `low` |

Prioritize correctness and security, then structural regressions, then other findings. Prefer high-conviction
findings; propose structural remedies such as extracting or reusing a helper or moving logic to its owning package.
Cite file and line for every finding.

## Honesty rules

- If nothing is wrong, say so; do not invent findings or soften real issues. Quantify impact where possible.
- Skip checks enforced by tooling (gofmt, golangci-lint rules in `.golangci.yaml`).
- Repo conventions override generic best practices; vendored subcharts, patched staging modules and integer credits
  are intentional.

## Named noise

Omit these measured false positives:

- `kubectl -o jsonpath` JSON-encodes `[]string`; comma-splitting `pciSwitches` is correct (measured on kubectl v1.36.3).
- Requests for redundant prose comments when code already states the rule or the spec owns the history.
- Missing regression tests inferred from exclusion: this full-checkout review can inspect `_test.go` and report an
  actually missing test; Copilot and OpenCodeReview cannot infer absence from their view.

Omit these unless citing the specific measurement or code change that contradicts them.

## Out of scope — never review

- `binding/`, `staging/`, `pkg/kubeclients/`, `pkg/extensionroute/swagger/ui/`, and anything matching
  `zz_generated*`, `*_deepcopy*`, `generated.pb.go`, `generated.proto`, `generated.protomessage.pb.go`.
- `deploy/gpustack-operator/chart/*.json` and `deploy/gpustack-operator/chart/*.md` — rendered by
  `make generate chart`. The `values.yaml` and `README.md.gotmpl` they come from are hand-written and reviewed.
- Vendored subchart trees under `deploy/gpustack-operator/chart/charts/*` — except to flag that an in-place edit
  should have been a patch under `hack/`.

`hack/`, `gen/`, `.agents/skills/**/*.sh` and `.claude/skills/**/*.sh` stay **in** scope here, and only here:
`.opencodereview/rule.json` excludes them all, so a change to a build script, a code generator, or an e2e case
reaches no other reviewer. Review them as ordinary hand-written code — and for a case script, whether its assertion
can fail at all, and whether its cleanup restores every baseline it changed.
