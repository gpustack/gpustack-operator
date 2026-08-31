# Page map — what each page owns, and what it must not absorb

One page owns each fact. When two pages could plausibly carry it, the owner is the one whose **reader**
needs it to finish their task; everyone else links.

## `README.md` (project front page)

**Owns** — the pitch, the accelerator support matrix, the five-step Quick Start, a short index.

**Never** — reconciler names, label schemas, rationale, anything that changes more often than a
release. A user who needs those follows a link.

**Pinned** — the matrix's slicing columns come from `pkg/nodefeature/knowns.go` and each detector's
`LogicalSliced.Count`. Re-check both when a vendor gains or loses a mode.

## `docs/README.md` (index)

**Owns** — the reading paths and the page table (page · what it answers · audience · read time).

**Never** — content. If you are explaining something here, it belongs on a page.

**Rule** — a new page is not done until it has a row here. `check-docs.sh` enforces it.

## `docs/architecture.md` (overview, ≤ ~200 lines)

**Owns** — what the operator builds, the three subcommands, the four stages + the chain diagram, the
*Life of a sliced-GPU request* trace, the vocabulary table, the map of deep pages.

**Never** — a new mechanism, a label table, a webhook rule, a measured failure story. It gains at most
one clause and a link when a deep page grows.

**Test** — a reader who knows Kubernetes but not this project should answer "what does it do, and what
happens to my request" from this page alone, in under 10 minutes.

## `docs/architecture/device-discovery.md` (stages 1–2)

**Owns** — NFD's three jobs, the general(CPU) node key, the `gpustack-cpu-info` rule and the
manufacturer map, the Device Manager DaemonSets, the accelerator label table, the `Devices` ledger, the
device-plugin allocator (per-family injection, vendor quirks), SSH-sidecar visibility, cross-mode
exclusion, `GetPreferredAllocation` placement, the partitioned family's fungible tokens, the
one-driver-stack-per-node guard.

**Never** — how those labels become Kueue objects (that is `scheduling-chain.md`), or whether a request
is *allowed* (that is `admission.md`).

## `docs/architecture/scheduling-chain.md` (stages 3–4)

**Owns** — `NodeFeatureReconciler` / `NodeCapacityReconciler`, the `.sliced.*` and `.partitioned.*`
capacity tables, per-vendor slice counts, presence-gating, the unit-spec default, the naming/grouping
scheme, the controller diagram, and the five reconcilers' ownership split.

**Never** — the ledger's internals (`device-discovery.md`) or gate behavior (`admission.md`). Cross-link both.

## `docs/architecture/admission.md`

**Owns** — the five gates, the `Devices` ledger's role beneath them, the four-view status, capability
versus availability, the InstanceType / Instance / Pod webhook rules, running-instance stop, and the
deployed Kueue Configuration's known behaviors.

**Never** — the normative request contract itself. That is `accelerator-requests.md`; this page says
where each rule is *enforced*.

## `docs/architecture/installation-modes.md`

**Owns** — chart mode vs image mode, `worker.disableApplications`, the exclusivity argument, the
`deviceManager.enabled` / `worker.enabled` switches, and the chart-versus-worker custom-resource
boundary.

**Never** — subchart vendoring mechanics (`docs/development.md`) or replica counts
(`docs/operation/high-availability.md`).

## `docs/architecture/internals.md`

**Owns** — the contributor invariants: worker startup order, the two ensurers, the applications lock,
the gateway's hand-maintained mirror, the device-plugin generation/re-registration loop, the
per-manufacturer package split, the CGO bindings, the 63-character rule.

**Rule of thumb** — if breaking it produces a *silent* failure, it belongs here.

## `docs/kv-cache/backend.md`

**Owns** — the `KVCacheBackend` chain: the managed/external and leader/member axes, the rendered
leader and member workloads, the admin surface the status is read from, the phase and condition
algebra, and the two operator surprises (capacity is observed rather than summed; shrinking a member
group discards that member's cache).

**Never** — the four-stage scheduling chain. This chain is its own; `docs/architecture.md` links to it
in one clause and does not describe it.

## `docs/accelerator-requests.md`

**Owns** — the normative contract: the two families, every resource key, a worked example per family,
the seven request rules with an accepted and a rejected example each, the `Instance` API form,
pre-release breaks, limitations.

**Never** — implementation. A rule's *enforcement point* is a link to `admission.md`.

## `docs/walkthrough.md`

**Owns** — recorded runs with real `kubectl` output and before/after comparisons.

**Rule** — every command and every output is real, captured from a live cluster; node names are
genericized. Never hand-write plausible output.

## `docs/settings.md`

**Owns** — the online-adjustable `Setting` catalog and every `GPUSTACK_*` variable, including the
per-manufacturer overrides and vendor toolkit paths.

## `docs/vendor-prerequisites.md`

**Owns** — what to install on a node before GPUStack, per manufacturer; which of a vendor GPU
Operator's components conflict with ours and the switch that turns each off; what to change when one
is already installed.

**Never** — how our own allocator picks or renders a channel (device discovery owns that), the
`GPUSTACK_*` variable that names one (settings owns it), or a vendor's own install procedure beyond
the values this operator needs changed. Link, do not restate a vendor's chart.

**Pinned** — the injection table comes from each allocator's device set
(`pkg/devicemanager/allocator/*/deviceplugin.go`); re-check it when an allocator changes what it
injects. Every section names the vendor product version its statements were read against — move that
version only together with a re-reading.

## `docs/development.md`

**Owns** — make targets, the chart targets, vendored subcharts and how to patch one, commit-message
rules, running a single test, runtime log verbosity, API groups and code generation, patched
dependencies.

## `docs/operation/*.md`

**Owns** — administrator procedures: what to run, in what order, and how to verify. Today: high
availability, `preflight.md` — the one-container run that says what a bare node can detect, slice and
manage — the NVIDIA MIG runbook, and `thead-mig.md` — MIG is T-Head's own word for its
partitioning, as `hgml.GetMigMode()` and the `alibabacloud.com/ppu.partitioned.mig-<profile>` key both
show, so the page is named for it too.

**Rule** — a page with a `## Verify` block states the expected output of every command in it. These
pages are exempt from the line cap: a runbook is as long as the hardware makes it.

## `docs/migration/*.md`

**Owns** — version-to-version upgrade paths, what changes permanently, and the recovery when it goes
wrong.

**Rule** — a migration page is written once and then only corrected. It describes a historical
transition, so do not "modernize" its version numbers.

## `docs/reference/*.md`

**Owns** — lookup tables with provenance. Today: the per-product unit-resources presets
(`instance-type-unit-resources.md`), and every command the binary offers with its flags and exit
codes (`commands.md`).

**Not** — `commands.md` states what a flag does, not when to reach for the command. The procedure a
one-shot belongs to lives on its operator page (`docs/operation/preflight.md` for `device-manager
preflight`), and the reference row links to it rather than restating it.

**Pinned** — `instance-type-unit-resources.md` is matched row-by-row by `TestUnitResourcesPresetDocs`,
by path. Do not rename it or reshape its tables. `commands.md` has no test behind it: its flag tables
are only as true as the last person who ran `--help`, so change a flag and change the row in the same
commit.

**Rule** — these pages are exempt from the ten-`##` cap: a lookup page is meant to be flat.

## `specs/` — not documentation

Decision records, motivation, alternatives, task breakdowns and build logs live there. Docs state the
resulting rule. Never edit a spec to reflect a later change, and never update a spec's references when
a doc moves — a spec is a record of what was true when it was written.
