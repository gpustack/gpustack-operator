---
name: gpustack-operator-code-review
description: "Review a GPUStack Operator pull-request diff on two axes reported separately — Standards (does it follow this repo's conventions) and Spec (does it implement what was asked) — with a severity label on every finding."
---

# GPUStack Operator — code review

Review the PR's diff on **two axes**, reported separately and never merged or
reranked against each other:

- **Standards** — does the code follow this repo's documented conventions
  (`CLAUDE.md`, `.github/copilot-instructions.md`, `docs/`)?
- **Spec** — does the code faithfully implement what the PR description / linked
  issue / `specs/` document asked for?

A change can pass one axis and fail the other — code that follows every standard
but implements the wrong thing, or code that does what was asked but breaks project
conventions. Reporting both separately stops one from masking the other.

Before anything else, read `docs/architecture.md` enough to know which stage of the
chain (NFD → Device Manager → worker capacity profiling → Kueue materialization)
the diff touches, and keep the deep page for that stage (`docs/architecture/*.md`)
in mind while reviewing.

## Procedure

### 1. Pin the diff and the intent

- The diff is the PR's changed files (base…head). If it is empty or only touches
  out-of-scope paths (generated/vendored code — see below), stop and say so.
- Identify the spec source, in this order: the PR description → linked issues → a
  file under `specs/` matching the feature. If none exists, note "no spec
  available" and limit the Spec axis to obvious internal inconsistencies.

### 2. Review the tests first

Tests reveal intent better than implementation:

- Do tests exist for the changed behavior? A bug fix without a regression test is a finding.
- Are they table-driven with a shared execution loop, declarative cases, fixtures
  built through helpers?
- Do they assert observable final state (not implementation details), use fake
  clients, and stay deterministic (no time-, ordering-, or randomness-dependence)?
- Would they catch a regression if the implementation changed?

### 3. Review the implementation across five axes

Walk each changed file with these checks:

**Correctness**

- Reconcile logic: idempotent and safe to retry, level-based (no edge-triggered
  assumptions), eventual consistency tolerated, `context.Context` propagated.
- Errors handled explicitly on every path, not just the happy path; typed errors
  and actionable conditions; no panics for control flow.
- `Devices` ledger accounting stays consistent — every allocation path has a
  matching release path; nothing reads or writes allocation state outside the ledger.
- Edge cases: nil/empty inputs, boundary values, not-found objects, requeue behavior.

**Readability**

- Names reflect domain meaning (`gKey`/`aKey`, pool, entrance, credits, four-view);
  multi-word files are snake_case.
- Control flow is straightforward; no clever tricks, deep nesting, or speculative
  abstraction.
- Exported APIs carry doc comments describing behavior and constraints.

**Architecture**

- The change lives in the layer that owns the concept: label algebra in
  `pkg/nodefeature`, device detection/allocation in `pkg/devicemanager`,
  scheduling-chain materialization in `pkg/worker` controllers, admission rules in
  the webhooks / AdmissionCheck controllers. Flag feature logic leaking into shared
  or general-purpose packages.
- Pool isolation invariants hold: one ClusterQueue per pool, no cross-pool
  borrowing.
- The credits model stays integer (`M = 1,600,000` per accelerator); flag any
  floating-point or percentage arithmetic that erodes it.
- API changes stay backward compatible; `spec` and `status` remain strictly separated.
- Does a refactor reduce the number of concepts a reader must hold, or just
  relocate them? Relocation is not simplification.

**Security**

- Webhook and API inputs are validated before use; external data (labels, CR
  fields, node metadata) is treated as untrusted.
- No secrets in code, logs, or fixtures.
- CGO boundaries (`binding/`, `csrc/` callers) handle C-returned memory and error
  codes correctly; injected preload shims keep paths and values quoted/escaped.

**Performance**

- Reconcile hot paths: no unbounded lists, no per-object API round-trips that
  belong in a cache, no work proportional to cluster size in a per-object reconcile.
- Watches/informers are scoped to resources that actually affect desired state;
  flag broad watches that would trigger reconcile storms.

### 4. Check the repo's hard invariants

Each of these is a **required** finding, not a judgement call:

- `api/` types or webhooks changed without the regenerated `zz_generated.*` /
  CRD / protobuf output (`make generate`), or generated files are hand-edited.
- Chart `values.yaml` changed without regenerated `README.md` /
  `values.schema.json` (`make generate chart`), or either file is hand-edited.
- In-place edits under `staging/` or `deploy/gpustack-operator/chart/charts/*`
  instead of patch files under `hack/`.
- Doc changes that break the docs contract (page header, Contents list, footer,
  `docs/README.md` index) — `make lint docs` must pass.

### 5. Over-engineering pass

A separate sweep that only hunts complexity — correctness and security findings
belong to step 3, never here. One line per finding: location, tag, what to cut,
what replaces it. The diff's best outcome is getting shorter.

- `delete:` dead code, unused flexibility, speculative features. Replacement: nothing.
- `stdlib:` hand-rolled logic that the Go standard library or an already-imported
  dependency (apimachinery, klog, controller-runtime) ships. Name the function.
- `native:` code reimplementing what Kubernetes/controller-runtime already does
  (finalizers, owner references, rate limiters, workqueues). Name the feature.
- `yagni:` an abstraction with one implementation, a knob nobody sets, a layer
  with one caller. Inline it until a second user exists.
- `shrink:` same logic in fewer lines. Show the shorter form.

End the pass with `net: -N lines possible`. If there is nothing to cut, write
`Lean already.` and stop. Never flag a test or a deliberate doc comment for deletion.

### 6. Aggregate and label

Report under `## Standards` and `## Spec` headings. Prefix every finding with its
severity. Nothing here blocks a merge — this review is a report, not a
required-changes verdict — so the label is the only priority signal the author gets.
The third column maps onto the four levels OpenCodeReview emits on the same PR, so a
human reading both reports is reading one scale:

| Prefix | Meaning | OCR level |
|---|---|---|
| **Critical:** | Data loss, broken reconcile, security hole, ledger inconsistency — must fix | `critical` |
| *(no prefix)* | Required change — convention or spec violation that must be addressed | `high` |
| **Optional:** / **Consider:** | Worth considering, not required | `medium` |
| **Nit:** | Minor style preference; the author may ignore | `low` |
| **FYI** | Context only, no action needed | `low` |

Lead with what matters: correctness and security first, then structural
regressions, then everything else. A few high-conviction comments beat a long list;
if there is one structural problem and ten nits, the structural problem *is* the
review. Where a finding has a structural remedy, propose the move (extract a
helper, reuse the canonical one, push logic into its owning package), not just the
problem. Cite file and line for every finding.

## Honesty rules

- Don't rubber-stamp. If nothing is wrong, say so plainly and stop — do not invent
  findings to look thorough.
- Don't soften real issues; quantify impact where possible ("this list runs once
  per pod per node", "this breaks ledger accounting when allocation fails midway").
- Skip anything tooling already enforces (gofmt, golangci-lint rules in
  `.golangci.yaml`).
- A documented repo convention always wins over a generic best practice; where the
  repo endorses something unusual (vendored subcharts, patched staging modules,
  integer credits), treat it as intentional.
- Comment on code, not people. If the author has context you lack, defer gracefully.

## Named noise — do not report these at any severity

Severity is a poor filter because it does not predict truth. These are filtered by
what they claim instead, and each is a finding this repository has *measured* as wrong.

- **`kubectl -o jsonpath` renders a `[]string` with Go's `%v`, space-separated.** It
  does not: the jsonpath printer JSON-encodes a slice, so `pciSwitches` prints
  `["0000:01:00.0","0000:00:01.0"]` and splitting on commas is correct. Measured on
  kubectl v1.36.3 and on real multi-bridge hardware. Reported six times, wrong six
  times, and acting on it introduces the very mis-parse it warns about.
- **"Add a comment explaining this" on code that already carries the rule.**
  `AGENTS.md` requires source comments to stay plain and short and to state logic
  rather than history; a request for more prose, for restating what a spec owns, or
  for keeping a revision narrative in a hot path contradicts it. Review against that
  convention, not toward more text.
- **"The fix ships no regression test", inferred from not having seen one.** You can
  read `_test.go` files, so you may check this and report it. The machine reviewers on
  the same PR cannot — Copilot drops them by content-exclusion policy, OpenCodeReview
  by `**/*_test.go` — and have raised it against a test file present in that very PR.
  When one of them does, the refutation is its own output: Copilot's review body lists
  every excluded path by name.

A finding in one of these classes should be omitted. If it seems to apply anyway, say
which measurement it contradicts — that is the only form of it worth a reviewer's
attention.

## Out of scope — never review

- `binding/`, `staging/`, `pkg/kubeclients/`, `pkg/extensionroute/swagger/ui/`, and
  anything matching `zz_generated*`, `*_deepcopy*`, `generated.pb.go`,
  `generated.proto`, `generated.protomessage.pb.go`.
- `deploy/gpustack-operator/chart/*.json` and `deploy/gpustack-operator/chart/*.md` — rendered by
  `make generate chart`. The `values.yaml` and `README.md.gotmpl` they come from are hand-written,
  and are reviewed.
- Vendored subchart trees under `deploy/gpustack-operator/chart/charts/*` — except
  to flag that an in-place edit should have been a patch under `hack/`.

`hack/`, `gen/`, `.agents/skills/**/*.sh` and `.claude/skills/**/*.sh` stay **in** scope
here, and only here: `.opencodereview/rule.json` excludes them all, so a change to a
build script, a code generator, or an e2e case reaches no other reviewer. Review them as
ordinary hand-written code — and for a case script, whether its assertion can fail at
all, and whether its cleanup restores every baseline it changed.
