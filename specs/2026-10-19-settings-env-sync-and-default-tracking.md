# Spec: Settings Sync Semantics — Environment Declarations and Default Tracking

Status: Shipped
Blocked on: nothing. T1 to T5 are built and shipped; the open question at the end was confirmed before the build.
Type: Feature

## Summary

A `GPUSTACK_*` environment variable on the operator worker seeds its Setting only when the delegated Secret
`gpustack-settings` is created. Every later align pass adds only the keys that are absent, so a variable added or
corrected after the first deploy never reaches its Setting, and the failure is silent. This spec replaces the
create-once seed with a three-tier sync model evaluated on every worker start: an explicitly declared, non-blank
environment variable is authoritative; a Setting named in a new `settings-default-tracking` Setting follows the
operator's built-in default; every other Setting keeps the value an administrator stored. Every override the
model performs is logged.

## Motivation

### Goals

1. **An explicit environment declaration wins.** When `GPUSTACK_<UPPER_SNAKE_NAME>` is present in the worker
   process environment and its value is non-blank, the worker writes that value to the Setting on every start.
   A declaration is a standing desired state, not a one-time default.
2. **The operator's own defaults reach upgrades.** A Setting whose value comes from a built-in default and that no
   administrator has claimed is overwritten when the operator ships a new default. Without this, a cluster stays
   on whatever the default was at first install, forever, with no way to tell a deliberate pin from a stale
   default.
3. **A stored value the operator does not own is never touched.** A Setting that is neither declared in the
   environment nor named in the tracking Setting keeps the value an administrator wrote, including a blank one.
4. **Every override is visible.** Each write that changes a stored value logs one line naming the Setting, the
   old value, the new value, and the rule that fired. A value that silently reverts after a restart is the worst
   failure mode of this feature, and it must never happen without a log line.

### Non-Goals

1. **Inferring whether an administrator chose a value.** This design records no provenance marker and never
   compares a stored value against a previous default to guess intent. Intent is either an environment
   declaration or membership in `settings-default-tracking`. A per-setting provenance annotation was considered
   and rejected; see [Alternatives](#alternatives).
2. **Continuous reconciliation while the worker runs.** An environment variable cannot change inside a running
   Pod. "Every worker start" is the finest granularity the source allows.
3. **Restricting what an administrator may write.** `kubectl edit setting` and `kubectl patch setting` continue to
   write any value, including a blank one. This spec governs only what the operator writes.
4. **Migrating existing clusters' stored values.** No value already in `gpustack-settings` changes at upgrade
   time except through rules 1 and 2 below, on the first start of the new operator.
5. **A switch that turns the model off.** Rules 1 and 2 are unconditional once this ships. A cluster that wants
   the previous create-once behavior removes the Setting's name from `settings-default-tracking` and unsets the
   variable, which is the same procedure it already needs for a pin.

## Proposal

### User Stories

#### Story 1: An Environment Declaration Takes Effect on the Next Start

As a cluster administrator who declared `GPUSTACK_INSTANCE_ACCESS_STATIC_ADDRESS` in the worker Deployment, I
want that value to reach the `instance-access-static-address` Setting, so that my Instances publish the address I
configured instead of falling back to a node IP.

#### Story 2: A Pin Is Not Reverted by a Restart

As a cluster administrator who set `container-registry` to my own mirror at runtime, I want that value to survive
a worker restart, so that a node drain or a crash does not silently move my pulls back to the upstream registry.

#### Story 3: Operator Upgrades Carry New Image Defaults

As a cluster administrator on an older release, I want a new operator version's default for
`model-deployment-router-image` to reach my cluster, so that I pick up upstream fixes without hand-patching every
image Setting after each upgrade.

#### Story 4: A Pin Is Kept Once the Setting Is Untracked

As a cluster administrator who pinned `kv-cache-backend-image` to an image my air-gapped registry serves, I want
that pin kept once I remove the Setting from `settings-default-tracking`, so that later operator upgrades do not
break the KV cache backends I already run.

The first operator upgrade that carries this spec is the exception: the tracking Setting ships with six names in
it, and an older cluster has no stored value for it, so the operator's default applies. A pin made before that
upgrade is replaced by the operator default on the first start, and the log line says so. After that upgrade the
Setting exists, and untracking it is what keeps the pin.

#### Story 5: Every Override Is Explained

As a cluster administrator whose Setting changed without my action, I want a log line naming the Setting, both
values, and the reason, so that I can tell an override from a mistake of my own.

### Core Features & Acceptance Criteria

Let `D` be a Setting's seed: the value of `GPUSTACK_<UPPER_SNAKE_NAME>` when that variable is present in the
worker environment, otherwise the built-in default. Let `envDeclared` mean the variable is present. Let `tracked`
mean the Setting's name appears in the `settings-default-tracking` value.

On every worker start, for each registered Setting, in this order:

| # | `envDeclared` | `D` non-blank | `tracked` | Action |
|---|---|---|---|---|
| 1 | yes | yes | any | Write `D`. Log when the stored value changes. |
| 2 | no, or `D` blank | — | yes | Write `D` when `D` is non-blank. Log when the stored value changes. |
| 3 | no, or `D` blank | — | no | No write. |
| 4 | any | any | any | On creation of the Secret only: write `D` for every Setting, as today. |

**Rule 1 fires only for a non-blank declaration.** A variable present with an empty value is treated as no
declaration, because a blank carries no information.

**Rules 1 and 2 never write a blank value.** A blank seed is never written by either rule, so a Setting with a
blank default that is named in `settings-default-tracking` leaves the stored value alone. Rule 4 is the one
exception: it reproduces today's creation behavior, which writes every seed including blank ones, so that
`kubectl get settings` lists every Setting and a blank-default Setting that did not exist yet is backfilled.

**Rules 1 and 2 cannot disagree.** Both write `D`. When a variable is declared, `D` is that variable's value, so
a Setting is never written twice in one pass with two different values.

**An absent key is backfilled before any rule is evaluated.** The align pass first adds every registered key the
Secret does not carry, exactly as today. Rule 1 and rule 2 then decide only whether to overwrite a key that
already exists. Without this order a newly registered blank-default Setting would never appear.

**The tracking list is read from the live Secret, and the backfill has already given the Secret the built-in
default.** On a Secret that predates this spec the key is absent. The backfill writes the built-in default into the
Secret before the list is read, so the built-in list applies on the same start that backfills the key. The read
carries no fallback of its own: a list the Secret does not hold is an empty list. The backfill that runs first is
what puts the default there, and a test pins that order.

**Removing a variable does not restore a built-in default.** With no declaration and no tracking membership,
row 3 applies and the stored value is kept, including a value a previous release declared. Unsetting a variable
stops the operator asserting it; it does not reset the Setting. A tracked Setting still falls back to its
built-in default under rule 2.

**The only way to stop following a default is to untrack the Setting.** Because a blank declaration reads as no
declaration, and a blank default is never written, removing the name from `settings-default-tracking` is the sole
way to keep a stored value across an operator upgrade.

**Setting 1: `settings-default-tracking`** — an editable Setting whose value is a comma-separated list of
Setting names. It is the only Setting this spec adds. It defaults to the six image Settings that carry a built-in
default:

```
instance-ssh-server-image,kv-cache-backend-image,model-deployment-router-image,
model-deployment-router-proxy-image,model-deployment-routing-sidecar-image,
model-prefetch-warmup-image
```

The name is a protocol constant, exported from `pkg/setting`, because the align pass reads it and the task that
registers the Setting writes it. Two copies of the literal that drift apart make the tracking list match nothing,
which is the silent failure this specification exists to eliminate, and no test covers the join between the reader
and the writer. One exported name is the whole defense.

Its own name may not appear in the list. An entry naming itself is ignored and logged, because a Setting that
tracked itself would reconcile its own membership.

The list is parsed before the align loop, and the parse is fixed so that a stored value cannot silently change
which Settings are tracked:

1. Split on `,`.
2. Trim surrounding whitespace from each segment; a segment that is empty afterwards is dropped.
3. Drop a duplicate that has already been seen.
4. Drop the Setting's own name.
5. Drop any segment that names no registered Setting, so a name retired in a later release stops mattering.

Every dropped segment is logged once, at `klog.V(4)`, naming the segment and the reason.

**Override logging.** Each of rules 1 and 2 that changes a stored value emits one `klog.InfoS` line carrying the
Setting name, the old value, the new value, the rule that fired, and the variable when the Setting declares one.
A pass that changes nothing logs nothing. A rule-2 write where the stored value already equals `D` is not an
override and is not logged.

A Setting marked sensitive carries neither value. The line keeps the name, the rule and the variable, because
together they are what tells an operator which Setting changed and why, and none of the three is the value; both
values are replaced by one marker no value can equal, so a reader can tell it from an empty value and from an
absent pair. The log is a wider audience than the delegated Secret — usually readable more broadly, and shipped to
aggregators, CI output and backups — so a value the operator kept out of the Secret must not reach it. The marker
is the repository's existing sensitivity flag, `Setting.Sensitive()`; no redaction convention was invented, because
there is none to follow. No registered Setting carries that flag today, and none of the 37 can currently hold a
credential: `container-registry` refuses credentials through its own admission, proxy admissions refuse them, image
references are `repo:tag@digest`, and `image-pull-secrets` holds Secret names. The rule is what keeps the first
sensitive Setting from becoming the first leak.

**The chart keeps `model-artifact-delivery-mode`.** `GPUSTACK_MODEL_ARTIFACT_DELIVERY_MODE` is a non-blank
declaration, so rule 1 makes it authoritative and the chart owns the Setting. The worker cannot take it over:
`--model-manager-service-account` defaults to a non-empty name, so it carries no signal about whether the plugin
is deployed, and the code comment in `pkg/worker/settings/model_store.go` states the Setting is never inferred
from plugin installation. The chart's `modelManager.enabled` is the only signal that reaches the value.

The cost is that an administrator who sets the Setting to `Engine` at runtime sees it revert on the next worker
start, and `GPUSTACK_MODEL_ARTIFACT_DELIVERY_MODE=Node` is reinstalled. Every place that promises the opposite
changes:
[operations.md](../docs/modules/model-delivery/operations.md),
[values.yaml](../deploy/gpustack-operator/chart/values.yaml), the comment above the variable in
[deployment.yaml](../deploy/gpustack-operator/chart/templates/worker/deployment.yaml), which says a stored value
always wins, and the comment in
[model_store.go](../pkg/worker/settings/model_store.go), which says a seed fills only a key the store does not have
yet. The template renders the same variable and the same value; only its comment changes. The
`model-artifact-delivery-mode` row of the Settings table carries the same promise and is restated by the task
that owns that page.

### Notes / Constraints / Caveats

1. **Six image Settings become runtime-read-only.** Rule 2 fires on every worker start, not only on an upgrade. A
   `kubectl patch setting kv-cache-backend-image` therefore holds until the next start, then reverts to the
   operator default. Pinning means removing the name from `settings-default-tracking`.
2. **`image-pull-policy` becomes chart-owned.** The chart renders `GPUSTACK_IMAGE_PULL_POLICY=IfNotPresent`, which
   is non-blank, so rule 1 applies and the chart is the source of truth. Change it through chart values, not
   through the Setting resource.
3. **Manufacturer identity variables are unaffected.** The chart renders `GPUSTACK_<MFR>_PCI_VENDOR_ID` and its
   siblings into Pod environments from `_helpers.tpl`, and they are not among the registered Settings. Rule 1
   matches a registered Setting's declared variable, so it cannot fire on them at any value. An earlier draft of
   this caveat said rule 1 writing them is a no-op, which implied they were Settings rule 1 considered; measured
   against this checkout on 2026-10-19, no such registration exists.
4. **`container-registry`, `container-namespace`, and `image-pull-secrets` default to blank.** Rule 1 does not fire
   for a blank declaration, so these stay administrator-owned exactly as today.
5. **Deleting the Secret resets the tracking list.** `settings-default-tracking` returns to its default, so image
   pins made without untracking are lost. This is the same exposure as deleting any Setting.
6. **`systemmeta` notes are untouched by this spec.** The existing per-Setting `-uid` note is regenerated on each
   `Initialize`, which makes `EqualResourceTypeAndNotes` false and forces a Secret update on every worker start.
   That churn is pre-existing and out of scope here; this design deliberately needs no note to carry intent.
7. **Nothing reads a Setting value through a path that requires it to be create-once.** Startup writes defaults
   directly and does not run `Admission`; the read paths fall back to a Setting's default when the key is absent;
   `AllowOnceConfigure` has no caller. The 30-second read cache is per process, so a fresh worker sees the
   aligned Secret immediately.

### Boundaries

- **Always:** treat a blank declaration as no declaration; backfill an absent key before evaluating any rule;
  never let rules 1 or 2 write a blank value; log every value change with both values and the rule.
- **Ask first:** any new Setting that would make an existing chart-rendered variable authoritative; any change to
  which chart-rendered variable the operator owns.
- **Never:** infer administrator intent by comparing a stored value against a default; overwrite a stored
  non-blank value for a Setting that is neither declared nor tracked; restrict what `kubectl edit setting` may
  write.

### Risks and Mitigations

- **Risk: an upgrade silently replaces an image pin made before this spec.**
  → This is accepted, and it is the sharpest edge in the design. `settings-default-tracking` ships with six
  names and an older Secret has no stored value for it, so the built-in default applies on the same start that
  backfills the key. Mitigation: rule 2 logs the replacement with both values at the moment it happens, and the
  Settings reference page states that a pin is kept by removing the Setting from the tracking list. No pre-upgrade
  escape is documented, by decision.
- **Risk: a runtime patch of a rule-1 or rule-2 Setting is reverted on the next worker start and the administrator
  does not know why.** → Mitigation: the log line names the rule and the variable, so the cause is either the
  Deployment or the tracking list. The reference page says which Settings those are.
- **Risk: an administrator expects an environment variable to be a one-time default and does not expect it to be
  re-asserted.** → Mitigation: this is the intended behavior change and is the headline of the Settings reference
  page. The per-Setting table states, for every declared variable, that the Deployment owns it.
- **Risk: a worker start rewrites several Settings at once.** → Mitigation: every write lands in the one Secret
  update the align pass already performs, so the whole pass costs a single API call at startup regardless of how
  many keys change. The existing note churn already forces that update today, so a pass that changes no data costs
  nothing extra.

## Design Details

### Commands

Build and test run **locally**, on the developer host, in the worktree checked out at the base revision above.
No cluster, no accelerator, and no `helm install` is needed: every case in the Test Plan runs against
`pkg/kubeclients/kubernetes/fake`.

The Makefile exposes one target per script in `hack/`, so `make <name>` runs `hack/<name>.sh`.

```bash
# Gate for every task: Go lint (golangci-lint) over the tree.
make lint

# Gate for the docs task: link and anchor resolution, the docs/README.md index, the spec Status
# word, every `go test -run` this spec names, and cross-references.
make lint docs

# Gate for the chart task: image assertions over rendered output, the chart's own shell checks,
# and `helm lint`. Needs no cluster.
make lint chart

# Unit tests, matching hack/test.sh: race detector on, tests shuffled, coverage into .dist/test.
# RACE=false drops the detector and changes nothing else.
make test

# Targeted run, the form every task's Verify line uses.
GOTOOLCHAIN=local GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race -v ./pkg/setting \
  -run TestSettingsInitialize

# Baseline coverage for the Test Plan.
GOTOOLCHAIN=local GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -cover ./pkg/setting/...
```

`check-specs.sh` resolves every `go test -run` in this file against the real top-level test names in the tree.
Between this spec being written and T2 landing, the gate reports T2's `-run TestSettingsInitialize` as
selecting nothing, because the test it names does not exist yet. That is the gate working, not a defect: T2's
Verify line is the first one that turns it green, and T5 must not run before then. The `NOT RE-RUNNABLE` escape
is deliberately not used here, because these tests are re-runnable and will exist.

**No test outside T2 may carry the `TestSettingsInitialize` prefix.** `-run` matches a prefix, so a test named
`TestSettingsInitializerMetadata` satisfies rule 3 without T2 having written the test it names. Measured on
2026-10-19: T1's test of that name turned the docs gate green while T2 was still unstarted, and the gate was then
reporting success for a pattern that ran something this specification never promised. That is the false green rule
3 exists to catch, arriving through a name rather than through a stale one, so it costs nothing to close.

### Project Structure

Repo-root-relative layout touched by this spec:

```text
pkg/setting/
├── kuberes.go   # Settings.Initialize: create the delegated Secret, then apply rules 1, 2 and 3
├── helper.go    # InitializeFromEnv, InitializeFrom, isBlank (the one definition of "blank")
├── types.go     # Setting, the Settings registry, and the reads against the delegated Secret
└── kuberes_test.go  # NEW: table-driven sync cases driven through the fake clientset
pkg/worker/settings/
├── value.go     # Every built-in Setting, including settings-default-tracking
└── value_test.go  # The tracking list's default value and self-reference refusal
pkg/worker/
└── extensionapis/worker/instance.go  # Reads instance-access-static-address to publish Instance addresses
deploy/gpustack-operator/chart/
└── values.yaml  # Retracts the modelManager prose that promises a seed never overrides
docs/reference/settings.md  # Rewrites the seeding contract, adds the tracking Setting, restates the table
docs/README.md  # Index entry for this spec
docs/modules/model-delivery/operations.md  # Corrects the upgrade procedure under the new rules
specs/2026-10-19-settings-env-sync-and-default-tracking.md
```

### Code Style

The align pass does two things in order, because they answer different questions: backfill what the Secret does
not carry, then decide whether the operator owns a value that is already there. A key that is absent and a key
that is present are different cases, and checking absence before blankness is what keeps a newly registered
blank-default Setting backfillable.

```go
alignFn := func(aSec *core.Secret) (_ *core.Secret, skip bool, err error) {
	skip = true
	// Align delegated info.
	if !systemmeta.EqualResourceTypeAndNotes(eSec, aSec) {
		systemmeta.NoteResource(aSec, eResType, eNotes)
		skip = false
	}

	// Backfill every registered key the Secret lacks, as today. A blank default is a key too:
	// kubectl get settings lists it, and a read of a missing key falls back with an error.
	for name, s := range h {
		if _, ok := aSec.Data[name]; !ok {
			aSec.Data[name] = []byte(s.defVal)
			skip = false
		}
	}

	// The tracking list is read from the live Secret. The backfill above has already given the
	// Secret the built-in default when the key was absent.
	tracking := parseTrackedSettings(h, trackingValue(aSec))

	for name, s := range h {
		stored := string(aSec.Data[name])
		desired, rule := s.desiredValue(tracking)
		if desired == "" || desired == stored {
			// Rules 1 and 2 never write a blank value, and an equal value is not an override.
			continue
		}
		aSec.Data[name] = []byte(desired)
		skip = false
		klog.InfoS("setting overridden on start",
			"setting", name, "stored", stored, "value", desired, "rule", rule)
	}
	return aSec, skip, nil
}
```

`desiredValue` carries the whole rule set, and it is where the collapsed predicate lives: a Setting is written
when its seed is non-blank and the environment declares it or the tracking list names it.

```go
// desiredValue returns the value the operator owns for this Setting and the rule that claimed it.
// An empty rule means the operator owns nothing here, and the stored value is the administrator's.
func (s Setting) desiredValue(tracked sets.Set[string]) (value, rule string) {
	if isBlank(s.defVal) {
		return "", ""
	}
	if s.declared {
		return s.defVal, "env-declared"
	}
	if tracked.Has(s.name) {
		return s.defVal, "default-tracked"
	}
	return "", ""
}
```

Conventions to keep: use `klog.InfoS` with key-value pairs, as in `pkg/kubeapp/lock.go`; reuse `isBlank` rather
than testing for an empty string; keep one Setting name per log line so a grep finds every override; do not
introduce a second meaning of "blank" anywhere in this path.

Two placement notes, because the sketch above is illustrative and the task boundaries are not negotiable.

`desiredValue` is sketched as a method on `Setting`, which reads as though it belongs in `types.go` beside `Value`
and `Configure`. The task that applies the sync predicate owns `pkg/setting/kuberes.go` and not `types.go`, so the
method lands in `kuberes.go` and `Setting`'s methods end up split across two files. Ownership forced it, and moving
it later is a separate decision rather than a quiet cleanup.

`parseTrackedSettings` is sketched with one argument. Its own steps four and five cannot be evaluated without the
registry — step four drops the tracking Setting's own name and step five drops a name no registered Setting
carries — so it takes the registry. The acceptance is the contract and the sketch is a shape, not a signature.

`desiredValue` returns `(value, rule string)`, not the `(value string, rule string)` an earlier draft of this sketch
wrote. `gofumpt` collapses a repeated result type, so the earlier spelling does not survive `make lint`. The
signature and its meaning are unchanged, and no test or contract depends on the spelling.

### Implementation Plan

```
T1 ──▶ T2 ──▶ T3 ──┐
                  ├──▶ T5
T4 ───────────────┘
```

- [x] **T1 · Carry the declaration source on `Setting`**
      Blocked by: None
      Owns: `pkg/setting/helper.go`, `pkg/setting/types.go`, `pkg/setting/types_test.go`,
      `pkg/worker/settings/value_test.go`
      Gate: review
      Acceptance: `Initializer` reports the environment variable it reads, so `Setting` carries an `envName`
      and a `declared` flag fixed at registration; `InitializeFromEnv` and `InitializeFromSpecifiedEnv` set
      it and `InitializeFrom` leaves it empty; `declared` means presence in the environment, not a non-blank
      value; every one of the 36 registered Settings keeps the exact default value it produces today; the four
      call sites in `pkg/worker/settings/value_test.go` that invoke an initializer directly bind only the value
      they assert, and each keeps its expectation, its message and its `t.Setenv` line; no existing test changes
      behavior; the whole tree compiles.
      Verify: `GOTOOLCHAIN=local GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/setting/... && make lint`

- [x] **T2 · Apply the sync predicate in the align pass, with the regression guard**
      Blocked by: T1
      Owns: `pkg/setting/kuberes.go`, `pkg/setting/kuberes_test.go`
      Gate: review
      Acceptance: absent keys are backfilled first, so a blank-default Setting is still added; the tracking list
      is read from the live Secret, which the backfill has already given the built-in default when the key was
      absent, and is parsed by the five-step contract in Core Features, which takes the registry because steps
      four and five cannot be evaluated without it; the name is an exported constant so the task registering the Setting cannot drift from
      the one reading it; `desiredValue` returns a value only for a non-blank seed that is either declared or
      tracked; every change to a stored value logs one `klog.InfoS` line carrying the Setting, both values, the
      rule and the variable, and a sensitive Setting carries neither value behind a marker instead; `TestSettingsInitialize`
      covers the table below and passes.
      Verify: `GOTOOLCHAIN=local GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race -v ./pkg/setting -run TestSettingsInitialize && make lint`

- [x] **T3 · Register `settings-default-tracking`**
      Blocked by: T2
      Owns: `pkg/worker/settings/value.go`, `pkg/worker/settings/value_test.go`
      Gate: none
      Acceptance: one new editable Setting whose value is a comma-separated list of Setting names, defaulting to
      the six image Settings; an admission refuses a value containing its own name; `make lint` passes.
      Verify: `GOTOOLCHAIN=local GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/settings/... && make lint`

- [x] **T4 · Retract the chart's promise about the delivery-mode seed**
      Blocked by: None
      Owns: `deploy/gpustack-operator/chart/values.yaml`, `deploy/gpustack-operator/chart/templates/worker/deployment.yaml`,
      `docs/modules/model-delivery/operations.md`
      Gate: review
      Acceptance: `values.yaml` no longer says a later upgrade never overrides an administrator's choice for
      `modelManager.enabled`, and says the chart's declaration owns the Setting; `operations.md` drops the
      "set the Setting to `Engine` before upgrading, which the seed then leaves alone" escape and states that
      the variable is re-asserted on every worker start; the comment above `GPUSTACK_MODEL_ARTIFACT_DELIVERY_MODE`
      in `deployment.yaml` no longer says a stored value always wins. The template renders the same variable with
      the same value, because the chart keeps rendering `GPUSTACK_MODEL_ARTIFACT_DELIVERY_MODE`; only the comment
      changes.
      Verify: `make lint chart && make lint docs`

- [x] **T5 · Restate the Settings contract and index this spec**
      Blocked by: T2, T3, T4
      Owns: `docs/reference/settings.md`, `docs/README.md`
      Gate: none
      Acceptance:
      - The callout under the page title that currently reads "**First-deploy seeding vs. runtime changes.**
        ... *initial seed only*, so after the first deploy change the `Setting` resource, not the environment" is
        replaced, not merely supplemented. That sentence is the rule this specification overturns, and a reader who
        finds it has been told the opposite of the truth.
      - The replacement states all four rules — backfill, declared override, tracked default, untracked
        administrator-owned — in the order they apply on a worker start, each with its own condition, and names the
        three that never write a blank value.
      - The page answers the two questions an administrator will actually arrive with, each with the rule that
        causes it and the way out of it: **a stored value stopped applying and an environment variable took over**,
        and **an edited Setting reverted on the next worker start**. For each, the reader needs to know whether
        untracking the Setting, clearing the variable, or changing the chart value is the escape, and which of those
        is available for that particular Setting.
      - The per-Setting table gains a `settings-default-tracking` row with its six defaults and the note that an
        empty value tracks nothing. Every chart-rendered variable in the table's `Env seed` column is marked as
        authoritative on every start rather than a first-deploy seed, which includes dropping the
        `model-artifact-delivery-mode` row's "a seed never overrides a stored value".
      - The page states that a tracked image Setting reverts to the operator default on every worker start, and that
        untracking it is what keeps a pin.
      - The page tells a reader how to work out which rule owns any given Setting, in terms they can check rather
        than terms they must accept: whether the Deployment renders the variable, whether the name appears in
        `settings-default-tracking`, and what follows when it does neither. A page that states the rules but leaves
        the reader unable to place their own Setting in one of them has not explained the usage.
      - `docs/README.md` links this spec under Installation, whose stated purpose covers upgrade and migration paths.
      This is the first task at which the docs gate is green, because it resolves T2's `-run` pattern against a test
      that now exists.
      Verify: `make lint docs`

### Test Plan

[x] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

None. `pkg/setting` has no `kuberes_test.go`, so this work adds the first test of `Settings.Initialize` rather
than reshaping one. `pkg/setting/settingtest.MergeDelegatedSettings` already merges keys into the delegated
Secret for tests that read a value, and T2's cases drive the writer instead, so they build their own fake
clientset the way `pkg/extensionapi/setting_test.go` does.

#### Unit tests

Coverage baseline for the two packages measured on 2026-10-19 with
`GOTOOLCHAIN=local GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -cover`:

- `pkg/setting`: `2026-10-19` - `12.7%`
  - `TestSettingsInitialize_DeclaredSettingOverridesStoredValue`: the guard that pins this feature. The Secret
    stores `instance-access-static-address=old.example`, `GPUSTACK_INSTANCE_ACCESS_STATIC_ADDRESS=new.example`
    is set, the Setting is untracked, and the stored value becomes `new.example`. Remove rules 1 and 2 and it
    stays `old.example`, so this row alone fails without the feature.
  - `TestSettingsInitialize_TrackedSettingFollowsBuiltInDefault`: no declaration, the tracking list names the
    Setting, the stored value differs from the built-in default, and the stored value becomes the default.
  - `TestSettingsInitialize_UntrackedStoredValueIsLeftAlone`: no declaration, not tracked, covering a non-blank
    administrator value and a blank one.
  - `TestSettingsInitialize_BackfillsAbsentKeysIncludingBlanks`: the Secret lacks a key whose seed is blank, and
    the key is present afterwards, matching today's behavior.
  - `TestSettingsInitialize_TrackingAppliesOnThePassThatBackfillsItsKey`: the Secret has no
    `settings-default-tracking` key, and a tracked Setting follows the built-in default on that same pass. The
    backfill writes the built-in default into the Secret before any rule runs, and that ordering is what makes the
    list apply on that same pass.
  - `TestSettingsInitialize_EqualValueIsNotAnOverride`: the stored value already equals the seed, and nothing is
    written and nothing is logged. The log capture is the only guard the equal-value clause has, because writing an
    equal value is a data no-op.
  - `TestSettingsInitialize_BlankDeclarationReadsAsNoDeclaration`: the variable is present with a blank value, and
    an untracked Setting keeps its stored value. Blank is the empty string and the empty list and map forms.
  - `TestParseTrackedSettings`: whitespace around a segment, empty segments, duplicates, an unknown name, and
    the Setting's own name, asserting the five-step normalization and that a dropped segment does not track.
  - `TestSettingsInitialize_CreationSeedsEverySetting`: an empty Secret receives every registered key, blank
    defaults included.
- `pkg/worker/settings`: `2026-10-19` - `78.7%`
  - `TestSettingsDefaultTracking_DefaultValue`: the default lists exactly the six image Settings.
  - `TestSettingsDefaultTracking_RefusesItsOwnName`: an admission rejects a value containing
    `settings-default-tracking`.

Every case is one table row with declarative data, run through a single loop, against
`kubefake.NewSimpleClientset` with a reactor allowing `selfsubjectaccessreviews`, because `Initialize` calls
`review.CanDoUpdate` first and the fake client denies it by default.

The two admission cases call the admission directly rather than running the registration's chain, and that leaves a
known gap. `Setting.admit` is unexported and `Configure` is the only path that runs it, and that path writes the
delegated Secret through a process-wide loopback client that `model_store_test.go` configures; driving it from these
cases would make their coverage depend on shuffle order, which `make test` decides. So the admission's logic is
pinned and its wiring into the registration is not. A test that rebuilt the chain by hand does not close this gap —
that is what the existing case does, and rebuilding the chain is exactly what cannot catch a later edit to the
registration. Closing it needs a `TestMain` in `pkg/worker/settings` that configures the loopback clients once,
which relocates `model_store_test.go`'s fixture and sits outside this change. `AllowBlank` is the concrete
consequence: deleting it, or replacing it with `DisallowBlank`, leaves the suite green, because a blank value is
admitted by `Allow()` plus the self-reference admission either way. `AllowBlank` is kept for intent and as a guard
against a stricter future admission, and its absence is not detected today.

#### Post-build corrections

A review of the T2 commit found three places where the code or a test said something the code did not do. All
three are closed, and no runtime behavior changed.

1. **`trackingValue` carried a fallback that could not run.** The align pass backfills every registered key into the
   Secret before it reads the tracking list, so the Secret already holds the tracking key whenever the registry
   carries it. The fallback branch and the receiver it needed are gone; the function reads the Secret alone.
2. **The equal-value test did not assert that it logs nothing.** The operator's log is now captured, and the test
   fails when the align pass logs an override line for a value it did not change. The capture is the only guard that
   clause has, because writing an equal value is a data no-op.
3. **The blank-declaration test never reached the blank gate.** The `isBlank` gate treats the empty list and the
   empty map as blank, and only the empty string was covered. Both forms are covered now, and the gate is
   load-bearing.

A second review of the branch found a fourth place that still promised the retired model: the comment on
`ModelArtifactDeliveryMode` in `pkg/worker/settings/model_store.go` said a seed fills only a key the store does not
have yet. The chart's declaration is reasserted on every worker start, so a stored `Engine` is overwritten. The
comment now says the chart declares the value and the worker reasserts it, and the two residual "seed" cells on the
model delivery operations page were restated in the same way. The specification's own count of the places that
promised the old model was short by one, and it named them without a count instead.

Each correction is proven by deletion, and each proof was run again against the final tree.

| Guard | Deleted or moved | Failing test |
|---|---|---|
| 1 | the backfill loop, moved below the rule loop | `TestSettingsInitialize_TrackingAppliesOnThePassThatBackfillsItsKey` |
| 2 | `desired == stored`, from the rule loop | `TestSettingsInitialize_EqualValueIsNotAnOverride` |
| 3 | the `isBlank` gate, from `desiredValue` | `TestSettingsInitialize_BlankDeclarationReadsAsNoDeclaration` |

The override log line's message is pinned as a literal in both the align pass and the test, and the two are not
joined by a shared constant. A renamed message therefore needs the test updated with it, and a rename that misses
the test leaves the guard green over the change it is meant to catch. This coupling is known and accepted by
decision.

This spec supersedes [the node model store spec](2026-09-25-node-model-store.md) in part. That spec states the
create-once seeding rule in its "Seeding" paragraph, in its "Upgrading from the version before this one" paragraph,
in the heading of its settings table and in its `model-artifact-delivery-mode` row, and it states that a seed fills
only a key the cluster does not have yet. The operator no longer follows that rule. The sections of the older spec
stay as written, being the historical record of what it delivered; this spec states what replaced them.

#### Integration tests

None. No controller reads these keys on a path this change alters, and `Settings.Initialize` runs once at
startup against a single Secret, which the fake clientset models completely.

#### e2e tests

Justification for not adding one: the behavior under test is a table of writes against one Secret at worker
startup. An e2e run would assert the same rows through a slower path and would need a cluster this repository's
`testing/infra` provisions per session. The rows that an e2e could add value to — a Helm upgrade replacing a
pinned image Setting — is the accepted risk recorded above rather than a defect, and it is asserted by
`TestSettingsInitialize_TrackingAppliesOnThePassThatBackfillsItsKey` at the unit level.

## Alternatives

1. **Per-Setting provenance recorded in the Secret.** Store, beside each value, whether it came from an
   environment declaration, an administrator, or a built-in default, and let the operator update a default only
   when the recorded provenance says it was never claimed. Rejected: it requires the previous default to be
   recoverable across versions, which the Secret cannot supply once the default changes, and it makes a decision
   the operator cannot see. The tracking Setting states the same intent in a form an administrator can read with
   `kubectl get settings`. The rejected option costs roughly the same to build and is far harder to audit.
2. **Seed only blank stored values, the narrow fix.** Write the seed when a stored value is blank and the seed is
   not. Rejected: it fixes the reported symptom, but it cannot distinguish a deliberate blank from an unconfigured
   Setting, and it leaves image defaults stranded across an operator upgrade. It has no answer to goal 2.
3. **Move the delivery-mode seed into the worker, conditioned on `--model-manager-service-account`.** Rejected,
   and not because it was not tried: the flag defaults to a non-empty name in
   `pkg/system/control.go`, so the condition is always true and the worker would seed `Node` on every install,
   including `modelManager.enabled=false`. It also contradicts the comment in
   `pkg/worker/settings/model_store.go` that the Setting is never inferred from plugin installation, and
   `settings.Initialize` receives only the client, so no better condition exists inside the worker without a new
   flag. The chart's `modelManager.enabled` is the only signal, so the chart keeps the declaration and the two
   documents that promise otherwise are corrected instead.
4. **Track every Setting with a non-blank default, not only images.** Broaden `settings-default-tracking`'s
   default to all 22 such Settings. Rejected for this spec: the reported problem was image Settings, and a broader
   default makes behavioral switches such as `instance-privileged-allowed` revert on upgrade for anyone who
   enabled them. Administrators can add any name to the list.
5. **Ship `settings-default-tracking` empty and let administrators opt in.** This is the option that would have
   kept every pre-existing image pin across the first upgrade. Rejected by decision: a tracking list nobody
   populates does not solve the problem that motivated it, which was an operator upgrade stranding a cluster on
   a stale image default. The cost is recorded as the first risk: a pin made before this spec is replaced by the
   operator default on the first start after the upgrade, and the log line announces it.

## Open Questions

None. The revised Story 4 was confirmed by the user on 2026-10-19, so `settings-default-tracking`
ships with the six image names and the first upgrade that carries this spec replaces an image pin made
before it. That one-time cost is the first entry under Risks.
