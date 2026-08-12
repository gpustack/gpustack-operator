---
name: gpustack-operator-docs
description: "Write or update the GPUStack Operator documentation: choose the page a new fact belongs on, keep each page's header block / Contents / footer and the docs/README.md index in sync, and respect the tables a Go test or a generator pins. Invoke when a code change needs a doc change, when adding or splitting a page, when a link or table of contents drifts, or when reviewing a docs diff. Examples: \"document this new setting\", \"where should this go in the docs?\", \"add a page for X\", \"update the docs for my change\", \"the docs index is out of date\", \"文档该写在哪一页\"."
---

# GPUStack Operator — documentation

The docs are split by **reader**, not by subsystem. Everything in `docs/` follows one shape and one
index, and a handful of tables are pinned to code by a test or a generator. This skill is how a change
lands in the right place without re-bloating the overview.

**Read the index first**: [`docs/README.md`](../../../docs/README.md) — it carries the reading paths and
the page table you must update when adding a page.

## The rule that keeps the overview small

`docs/architecture.md` is the **front door**: what the operator builds, the four stages, one worked
request trace, the vocabulary. It is capped at ~200 lines and must stay readable in under 10 minutes.

**Never add a new mechanism, rationale or table to it.** New detail goes on a deep page; the overview
gains at most one clause and a link. If you cannot find a deep page that fits, that is a signal to add
one, not to widen the overview.

## Where a fact belongs

| The change is about… | Page |
|---|---|
| NFD labels, the `gpustack-cpu-info` rule, the manufacturer map | `docs/architecture/device-discovery.md` |
| Device Manager detection, the `Devices` ledger, allocator injection, cross-mode exclusion, placement | `docs/architecture/device-discovery.md` |
| Capacity labels, flavor/queue/InstanceType naming and grouping, the five reconcilers | `docs/architecture/scheduling-chain.md` |
| Any admission gate, the four-view status, InstanceType/Instance/Pod webhook rules, drain-stop | `docs/architecture/admission.md` |
| Chart mode vs image mode, `disableApplications`, what the worker applies itself | `docs/architecture/install-modes.md` |
| Startup ordering, the gateway mirror, per-manufacturer packages, CGO bindings, the 63-char rule | `docs/architecture/internals.md` |
| A resource key, a request rule, a request example | `docs/accelerator-requests.md` |
| A `Setting` or a `GPUSTACK_*` variable | `docs/settings.md` |
| A make target, a subchart patch, code generation, a vendored dependency | `docs/development.md` |
| An administrator procedure (MIG mode, replicas) | `docs/operation/*.md` |
| An upgrade path between versions | `docs/migration/*.md` |
| A per-product preset value | `docs/reference/instance-type-unit-resources.md` |
| A user-visible capability, a vendor's slicing support, the install flow | `README.md` |
| A recorded run with real output | `docs/walkthrough.md`, or the walkthrough section of `docs/operation/nvidia-mig.md` |

A decision *record* — why an approach was chosen over another — belongs in `specs/`, not in `docs/`.
The docs state the rule that resulted; a `> **Why**` note carries only as much rationale as a reader
needs to not undo it.

Full routing, including what does **not** belong on a page, is in
[references/page-map.md](references/page-map.md).

## Page shape

Every page in `docs/` (the index excepted) has a header block, a `## Contents` list mirroring its `##`
headings, and a `**See also**` / `**Next**` footer. Templates and the writing rules — rule first,
rationale demoted, no fact stated twice — are in [references/conventions.md](references/conventions.md).

## Sync invariants

These pages are not free text. Changing one side without the other breaks a test, a build, or a
reader's trust.

| Doc | Pinned to | How it breaks |
|---|---|---|
| `docs/reference/instance-type-unit-resources.md` | `pkg/nodefeature/unit_resources_preset.yaml` via `TestUnitResourcesPresetDocs` | the test matches a **whole table row** (`\| family \| tier \| cpu \| ram \|`) and the page **path**; do not rename the page or reshape that table |
| `deploy/gpustack-operator/chart/README.md`, `values.schema.json` | `values.yaml` + `README.md.gotmpl` via `make generate chart` | generated — never hand-edit; a doc path quoted in a `values.yaml` comment needs a regenerate, and `chart.yml` fails on drift |
| `README.md` accelerator matrix | `pkg/nodefeature/knowns.go` (resource names, `SharedResourceMaxSize`, `_ManufacturerPartitionKindMap`) and **whether** each `pkg/devicemanager/detector/<mfr>/device.go` sets `LogicalSliced` at all | nothing fails; the table silently lies about what a vendor can do. The matrix is deliberately ✅/— only — per-card slice counts and the per-vendor isolation mechanism live in `docs/architecture/device-discovery.md`, not on the front page |
| `README.md` Quick Start's four request shapes | `docs/accelerator-requests.md` — *The resource keys* and *Worked example per family* | nothing fails; the front page and the normative contract drift apart. This copy is the one sanctioned exception to "state a fact once" (the README is the shop window) — change both together |
| `docs/settings.md` tables | `pkg/worker/settings` and the `GPUSTACK_*` readers | nothing fails; an operator configures something that no longer exists |
| `docs/README.md` page table | the set of files under `docs/` | `check-docs.sh` fails |

## Before you finish

```bash
bash .claude/skills/gpustack-operator-docs/scripts/check-docs.sh
```

It verifies relative links and `#anchor`s across `README.md`, `CLAUDE.md`, `docs/**` and
`.claude/skills/**`; and — for `docs/**` only — each page's `## Contents` against its headings, the four
header-block fields, a `**See also**` footer at the end, and registration in the `## All pages` table of
`docs/README.md`. It does **not** read prose: everything below is still on you.

- [ ] `wc -l docs/architecture.md` is still ≤ ~200.
- [ ] The new fact is stated **once**; every other page links to it.
- [ ] Touched `docs/reference/instance-type-unit-resources.md`? Run
      `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -run TestUnitResourcesPresetDocs ./pkg/nodefeature/`.
- [ ] Touched `values.yaml`? Run `make generate chart` and commit the regenerated chart README/schema.
- [ ] Moved or renamed a page? `grep -rn "<old-name>" --include='*.md' --include='*.go' --include='*.yaml' .`
      and fix everything outside `specs/` (specs are historical records — leave their references alone).

## Keeping this skill honest

Found a doc that a test, a generator or a CI job pins, and it is not in the invariants table? Add the
row. Split or added a page? Add it to the routing table above and to `docs/README.md`. The tables are
the memory — they are only as good as their coverage.
