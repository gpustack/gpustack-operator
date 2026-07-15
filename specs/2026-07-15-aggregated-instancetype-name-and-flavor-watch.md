# Spec: Spec-Derived Aggregated InstanceType Naming & InstanceTypeFlavor Watch

Status: Building
Type: Feature

## Summary
Two related improvements to the worker-gateway's cross-cluster aggregated view. **(A)** The aggregated
`InstanceType` item `Name` is currently taken from the first-seen candidate's `GenerateName`/`Name`,
which is non-deterministic across clusters even though items are grouped purely by (normalized)
`Spec`; rebuild the name deterministically from the spec so the same hardware profile always reads
with one identity. **(B)** `InstanceTypeFlavor` is currently **list-only** end-to-end (deliberately
deferred in the `2026-07-10-workergateway-instancetypeflavor-listing` spec); add real `watch` support
across all three layers — the API verb/client, the worker extension apiserver, and the worker-gateway
aggregation — so a client can stream flavor-catalog changes the way it already can for `InstanceType`.

## Motivation
### Goals
- **A — deterministic aggregated name.** For each aggregated `InstanceType` item, build `Name` from the
  spec, matching the two templates:
  - accelerated (`Spec.Acceleratable == true`):
    `${generalGroup}-${acceleratorGroup}-${os}-${arch}-${cpu}c-${ram}g-${localStorage}g`
  - non-accelerated: `${generalGroup}-${os}-${arch}-${ram}g-${localStorage}g`

  where `${cpu}` = `spec.unitResources.cpu` (bare integer), `${ram}` = `spec.unitResources.ram` with
  its `Gi` suffix stripped, `${localStorage}` = `spec.localStorage` with `Gi` stripped. The name must be
  identical in both the list (`ListAggregateInstanceTypes.Next`) and watch
  (`HandleAggregatedInstanceType.Handle`) paths.
- **B — InstanceTypeFlavor watch, end-to-end.**
  1. **Verb level:** `worker.gpustack.ai/v1 InstanceTypeFlavor` advertises `watch` and its generated
     client exposes `Watch`.
  2. **Extension apiserver:** `InstanceTypeFlavorHandler` implements `rest.Watcher`, deriving flavor
     add/delete events from the underlying operator-owned Kueue `ResourceFlavor` watch (dedup-aware),
     following the `SettingHandler` synthetic-watch precedent.
  3. **Worker-gateway:** `GET /instancetypeflavors?watch=true[&aggregated=true]` streams per-cluster and
     aggregated flavor events, backed by a real `InstanceTypeFlavor` informer.
- Target users: the GPUStack control-plane UI / API clients that read the aggregated catalog across
  subscribed clusters.

### Non-Goals
- No change to the aggregated **Flavor** `Name` (it is already spec-deterministic via
  `instanceTypeFlavorName(spec)`; only watch is added).
- No backing CRD or controller for `InstanceTypeFlavor` — it stays a derived, read-only projection over
  `ResourceFlavor`.
- No change to the worker-level `InstanceType` object name, nor to how each cluster derives/dedups
  flavors (`OnList` semantics unchanged).
- No change to aggregation grouping keys (still normalized `Spec` for types, `Spec` for flavors) or to
  tier/candidate math.

## Proposal
The worker-gateway's aggregated view gains a stable, spec-derived identity for `InstanceType` items and
reaches watch parity for `InstanceTypeFlavor`: clients can subscribe to a live flavor catalog end-to-end
instead of polling a list.

### User Stories
#### Story 1
As a multi-cluster GPUStack operator viewing the aggregated `InstanceType` catalog, I want each item to
carry a stable, spec-derived name, so that the same hardware profile shows one consistent identity
regardless of which cluster's `InstanceType` (or its arbitrary object name) was observed first.
#### Story 2
As a UI/client consuming the worker-gateway, I want to **watch** aggregated `InstanceTypeFlavor`s (not
just list them), so that the hardware catalog updates live as pools appear/disappear across clusters —
reaching parity with the `InstanceType` watch I already have.
#### Story 3
As a `kubectl`/client user talking to a single worker's extension apiserver, I want `watch` on
`instancetypeflavors` to work (`kubectl get instancetypeflavor -w`), so that flavor discovery is live
rather than poll-only.

### Core Features & Acceptance Criteria
1. **Spec-derived aggregated `InstanceType` name.** A self-contained builder in
   `pkg/workergateway/service` produces the name per the templates above, replacing both
   `funcx.Ternary(...)` sites.
   - AC1: Two clusters each holding an `InstanceType` with the same normalized spec but different object
     names → one aggregated item whose `Name` equals the template output (independent of
     iteration/first-seen order).
   - AC2: The name emitted by the watch "added" path equals the name from the list path for the same
     spec.
   - AC3: A CPU-only (non-accelerated) item's name omits both `${acceleratorGroup}` and `${cpu}c`.
2. **`InstanceTypeFlavor` verb-level watch.** The API marker becomes
   `+genclient:onlyVerbs=get,list,watch`; after `make generate`, `InstanceTypeFlavorInterface` exposes
   `Watch`, and discovery advertises `watch`. (`get` is required because `lister-gen` emits a lister only
   for a `list`+`get` type, and the `list`+`watch`-triggered `informer-gen` output depends on that lister;
   the apiserver itself still serves only `list`+`watch`, so the extra client `Get` is unused.)
   - AC: the generated client compiles with `Watch`; the extension apiserver returns a watch stream (not
     `405`).
3. **Extension-apiserver synthetic watch.** `InstanceTypeFlavorHandler` implements `rest.Watcher` via
   `WithListWatch`; `OnWatch` watches operator-owned `ResourceFlavor`s (reusing the existing
   label-selector conversion + `DeletionTimestamp` skip + `cpuAware`), and emits deduplicated
   `InstanceTypeFlavor` `ADDED`/`DELETED` events (a flavor is deleted only when its last backing
   `ResourceFlavor` is gone).
   - AC: adding an RF that maps to an already-present flavor spec emits no duplicate; deleting one of
     several RFs backing the same flavor emits nothing; deleting the last emits `DELETED`.
4. **Worker-gateway flavor watch.** `handleListInstanceTypeFlavors` accepts `watch=true`;
   `OpHandleClusterInstanceTypeFlavor` (mirror of `OpHandleClusterInstanceType`) and
   `OpHandleAggregatedInstanceTypeFlavor` (dedup by `Spec`, maintain the `Clusters` list, emit
   ADDED/MODIFIED/DELETED) drive the stream; the manager builds an `InstanceTypeFlavor` informer.
   - AC: `GET /instancetypeflavors?watch=true` streams per-cluster events tagged by `cluster`;
     `?watch=true&aggregated=true` collapses identical specs and reports cluster-membership changes; a
     worker subscribed **with** the `InstanceTypeFlavor` GVK delivers events (subscribed without it stays
     list-only, per the established contract).

### Notes / Constraints / Caveats
- Go; commands per `docs/development.md`. Tests are table-driven with `testify`, fake clients over real
  deps.
- **Separate-binary rule:** the worker-gateway must not import `pkg/worker/extensionapis` or the worker
  webhook helpers. The name builder and any strip-suffix logic are duplicated locally in
  `pkg/workergateway/service` (same pattern as the existing `instanceTypePhaseActive` constant).
- `spec.unitResources.ram` / `spec.localStorage` carry a case-sensitive `Gi` suffix (worker
  webhook-enforced); the builder strips `Gi` best-effort (`strings.TrimSuffix`) and falls back to the raw
  value if absent. (Note: the `InstanceTypeUnitResources.RAM` doc comment says "(Mi)" but the webhook and
  defaults use `Gi` — stale comment, left untouched as out of scope.)
- Synthetic flavor watch mirrors `SettingHandler.OnWatch` (`watch.NewProxyWatcher`, `gox.Go`, bookmark
  passthrough, ctx/stop handling). Because flavor aggregation is many-RF→one-flavor, correctness needs a
  recompute-and-diff (or backing-RF multiset) rather than 1:1 event mapping.
- Removing the two `funcx.Ternary`/`stringx.TrimSuffix` call sites orphans those imports in `helper.go` —
  remove them.

### Boundaries
- **Always:** keep list and watch name outputs identical; regenerate code (`make generate`) after the
  genclient marker change; keep gateway helpers self-contained; keep `OnList` flavor semantics unchanged;
  run `make lint` after Go edits.
- **Ask first:** any name-collision policy beyond the given templates (e.g. appending a spec hash);
  changing the `IterateWorkers`/handler contract or the subscribe GVK semantics.
- **Never:** add a CRD/controller for `InstanceTypeFlavor`; fabricate a fake/never-emitting watch; import
  worker-side packages into the gateway; rename worker-level `InstanceType` objects.

### Risks and Mitigations
- **Name collision** — two aggregated items with distinct normalized specs (differing only in fields the
  template drops, e.g. `product`/`family`/`memory`/`cores`) could produce the same name, confusing a
  client that keys by name → *Mitigation:* in practice `acceleratorGroup`/`generalGroup` imply those
  descriptors, so collisions are unlikely; flagged as an Open Question if strict uniqueness is required.
- **Synthetic-watch correctness** (dedup: an RF delete must not drop a flavor other RFs still back) →
  *Mitigation:* recompute-and-diff the derived set per event (RF/flavor counts are tiny) or maintain a
  per-spec backing-RF count; table-driven tests for add-dup / partial-delete / last-delete.
- **`cpuAware` changes mid-watch** could drift the derived set from a long-lived stream → *Mitigation:*
  read at watch start (as `OnList` does); a client reconnect re-derives; documented caveat.
- **Watch load** — a live RF watch per flavor-watch client → *Mitigation:* one shared upstream RF watch
  per stream, reusing the existing selector; flavor lists are small.
- **Subscribe contract** — flavor events require subscribing the worker *with* the `InstanceTypeFlavor`
  GVK; without it, list still works via the live-list proxy but watch is silent → *Mitigation:*
  documented on `handleSubscribeWorker`, consistent with the existing informer-only rule.
- **`OnWatch` test scaffolding** — the ctrlfake client's `Watch` may be limited and `SettingHandler.OnWatch`
  has no existing unit-test harness to copy → *Mitigation:* drive `OnWatch` with a hand-rolled fake
  `watch.Interface` feeding `ResourceFlavor` events; the recompute-and-diff dedup logic is the unit under
  test, decoupled from the fake's fidelity.

## Design Details
### Commands
Environment: **local** (darwin), confirmed by a read-only smoke run (the three affected packages compile
and pass). All Go commands use `GODEBUG=gotypesalias=0 CGO_ENABLED=1` and build tags `goccy netgo`.
- Build: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go build -tags "goccy netgo" ./...`
- Test (targeted): `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race -tags "goccy netgo" ./pkg/workergateway/... ./pkg/worker/extensionapis/worker/...`
- Regenerate (Task 2, after the genclient marker change): `make generate` — runs
  `client-gen`/`lister-gen`/`informer-gen`/`applyconfiguration-gen` + apiservice
  (`gen/api/builder/generate.go`), so the marker flip yields a `Watch`-capable client. Or the
  `gpustack-operator-generate` skill.
- Lint: `make lint`
- Full CI parity: `make generate && make deps && make lint && make build`

### Project Structure
- `api/worker/v1/instance_type_flavor.go` — **edit (Track B):** `+genclient:onlyVerbs=list` →
  `+genclient:onlyVerbs=get,list,watch` (`get` unlocks the lister the generated informer depends on);
  refresh the type doc to drop "list-only".
- generated client/apiservice (`pkg/kubeclients/.../worker/v1/instancetypeflavor.go`, `zz_generated.*`) —
  **regenerated** by `make generate`.
- `pkg/worker/extensionapis/worker/instance_type_flavor.go` — **edit (Track B):** add a
  `Client ctrlcli.WithWatch` field (`opts.Manager.GetClient()`), switch `ListOperation` →
  `WithListWatch(tc, h)` (`ListWatchOperation`) + `OnWatch` (RF-backed synthetic watch), assert
  `rest.Watcher`.
- `pkg/worker/extensionapis/worker/instance_type_flavor_test.go` — **edit:** watch cases (add-dup,
  partial-delete, last-delete, bookmark).
- `pkg/workergateway/manager/manager.go` — **edit (Track B):** add `InstanceTypeFlavor` to
  `defaultInformerFactories` (keep the `defaultListerFactories` fallback).
- `pkg/workergateway/service/service.go` — **edit (Track B):** `Watch` field + watch branch in
  `handleListInstanceTypeFlavors`.
- `pkg/workergateway/service/helper.go` — **edit (Track A + B):** name builder + both replacements;
  `OpHandleClusterInstanceTypeFlavor` + `OpHandleAggregatedInstanceTypeFlavor`; drop orphaned
  `funcx`/`stringx` imports.
- `pkg/workergateway/service/helper_test.go` — **edit:** name-builder assertions in the aggregate tests;
  flavor watch-handler tests.

### Code Style
```go
// buildAggregatedInstanceTypeName derives a stable, spec-only identity so the same hardware
// profile reads identically across clusters regardless of which cluster's InstanceType (or its
// arbitrary object name) was seen first. Accelerated types encode the accelerator group and the
// per-unit CPU; a CPU-only type omits both (its unit CPU is webhook-fixed to 1).
func buildAggregatedInstanceTypeName(spec AggregatedInstanceTypeSpec) string {
	ram := strings.TrimSuffix(spec.UnitResources.RAM, "Gi")
	stg := strings.TrimSuffix(spec.LocalStorage, "Gi")
	if spec.Acceleratable {
		return fmt.Sprintf("%s-%s-%s-%s-%sc-%sg-%sg",
			spec.GeneralGroup, spec.AcceleratorGroup, spec.OS, spec.Arch,
			spec.UnitResources.CPU, ram, stg)
	}
	return fmt.Sprintf("%s-%s-%s-%sg-%sg", spec.GeneralGroup, spec.OS, spec.Arch, ram, stg)
}
```
Flavor `OnWatch` mirrors `SettingHandler.OnWatch`: list+index current flavors,
`Client.(ctrlcli.WithWatch).Watch` the operator-owned `ResourceFlavor`s (reuse
`convertResourceFlavorListOptsFromInstanceTypeFlavorListOpts`), transform via a
`watch.NewProxyWatcher` goroutine, recompute-and-diff the deduped flavor set, pass bookmarks through.

### Implementation Plan
**Dependency graph.** Track A (Task 1) is independent — gateway-only, no codegen. Track B chains:
**Task 2 (verb + client regen)** unlocks Task 4's compile; **Task 3 (apiserver `OnWatch`)** is an
independent build but is required for Task 4 at runtime; **Task 4 (gateway watch)** depends on Task 2
(compile) + Task 3 (runtime). **No PoC needed** — the apiserver synthetic watch is proven by
`SettingHandler.OnWatch`, the gateway watch by the existing `InstanceType` watch path.

- [x] **Task 1 — Spec-derived aggregated `InstanceType` name (Track A; `helper.go`, `helper_test.go`).**
  - Add self-contained `buildAggregatedInstanceTypeName(spec)` (strip `Gi` from ram/localStorage;
    accelerated vs non-accelerated templates; non-accelerated omits `${acceleratorGroup}` and `${cpu}c`).
  - Replace both `funcx.Ternary(...)` sites (`helper.go:286` Next, `:697` Handle); drop the now-orphaned
    `funcx` / `stringx` imports.
  - Enrich `instSpecCPUOnly` / `instSpecA10G` / `instSpecTeslaT4` with `OS` / `Arch` / `UnitResources` /
    `LocalStorage`; add a focused `TestBuildAggregatedInstanceTypeName` table
    (`generic-nvidia-a10g-linux-amd64-4c-16g-100g`; `generic-linux-amd64-2g-100g`; `Gi`-strip; cpu
    omission); update the `Next` / `Handle`-path name assertions. `TestListAggregateInstanceTypes_Result`
    (sorts pre-named items) is unaffected.
  - **Acceptance:** the same normalized spec across clusters yields one item whose `Name` equals the
    template output regardless of first-seen order; a non-accelerated item omits accelerator-group + cpu;
    `Gi` is stripped; the list and watch paths emit identical names.
  - **Verify:** `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race -tags "goccy netgo" ./pkg/workergateway/service/...`;
    `make lint`.

- [x] **Task 2 — `InstanceTypeFlavor` watch verb + client regen (Track B; `api/worker/v1/instance_type_flavor.go` + regenerated files).**
  - Change `+genclient:onlyVerbs=list` → `+genclient:onlyVerbs=get,list,watch` (`get` is required so
    `lister-gen` emits the lister that the `watch`-triggered generated informer references — without it the
    new informer fails to compile); refresh the type doc (drop "list-only" / "no Watch"). Run `make generate`.
  - **Acceptance:** `InstanceTypeFlavorInterface` exposes `Watch`; the tree compiles; the regen diff is
    limited to this type's client/lister/informer/apiservice (plus the doc-comment churn in
    `generated.proto` / `zz_generated.openapi.go`).
  - **Verify:** `make generate` (idempotent on re-run — confirmed); `git status` (only expected regen);
    `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go build -tags "goccy netgo" ./...` (confirmed).

- [ ] **Task 3 — Extension-apiserver synthetic watch (Track B; `pkg/worker/extensionapis/worker/instance_type_flavor.go` + `_test.go`).**
  - Add `Client ctrlcli.WithWatch` (`opts.Manager.GetClient()`); switch `ListOperation` →
    `WithListWatch(tc, h)`; assert `rest.Watcher`.
  - Implement `OnWatch`: index the current flavors (reuse `OnList`), watch the operator-owned
    `ResourceFlavor`s (reuse `convertResourceFlavorListOptsFromInstanceTypeFlavorListOpts`, skip
    `DeletionTimestamp`, read `cpuAware` once at start), and on each RF event recompute-and-diff the
    deduped flavor set → emit `ADDED` / `DELETED`, passing bookmarks through. Mirror
    `SettingHandler.OnWatch` (the outer `ListWatchOperation.Watch` wrapper already does the
    proxy/RV-dedup/bookmark).
  - Tests: first-add → `ADDED`; add-dup → none; partial-delete → none; last-delete → `DELETED`; bookmark
    passthrough. Use a hand-rolled fake `watch.Interface` feeding RF events if the ctrlfake client's
    `Watch` is insufficient (`SettingHandler.OnWatch` has no existing test harness to copy).
  - **Acceptance:** events are dedup-correct — a flavor is deleted only when its last backing
    `ResourceFlavor` is gone.
  - **Verify:** unit tests pass; manual `kubectl get instancetypeflavor -w` on a worker while a
    `ResourceFlavor` pool is added/removed.

- [ ] **Task 4 — Worker-gateway flavor watch end-to-end (Track B; `manager.go`, `service.go`, `helper.go`, `helper_test.go`).**
  - manager: add `InstanceTypeFlavor` to `defaultInformerFactories`
    (`NewSharedIndexInformerWithOptions(cli.WorkerV1().InstanceTypeFlavors(), &worker.InstanceTypeFlavor{}, p)`);
    keep the `defaultListerFactories` fallback.
  - service: add `Watch bool` to `handleListInstanceTypeFlavors` and a watch branch (`streamResponse` with
    the cluster or aggregated handler).
  - helper: `OpHandleClusterInstanceTypeFlavor` (mirror `OpHandleClusterInstanceType`);
    `OpHandleAggregatedInstanceTypeFlavor` (dedup by `Spec`, maintain the `Clusters` list, emit `ADDED` on
    the first contributing cluster / `MODIFIED` on membership change / `DELETED` on the last; `Name` =
    the spec-deterministic `flavor.Name`).
  - Tests: table tests for both handlers (mirroring `TestHandleClusterInstanceType` /
    `TestHandleAggregatedInstanceType`); manager informer-factory wiring.
  - **Acceptance:** `GET /instancetypeflavors?watch=true` streams per-cluster events tagged by `cluster`;
    `?watch=true&aggregated=true` dedups by `Spec` and reports cluster-membership changes; a worker
    subscribed **with** the `InstanceTypeFlavor` GVK delivers events (without it, list still works via the
    live-list proxy, watch is silent — the established contract).
  - **Verify:** unit tests pass; manual `curl` of both variants against a running gateway with ≥1 cluster
    subscribed (including the `InstanceTypeFlavor` GVK), observing events on a pool change.

**Checkpoints.** After Task 1 — Track A is complete and independently shippable. After Task 2 + Task 3 —
the worker serves flavor watch (`kubectl -w`). After Task 4 — the gateway streams flavor watch end-to-end.

### Test Plan
[ ] I/we understand the owners of the involved components may require updates to existing tests to make
this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates
Enrich the shared aggregate fixtures (`instSpecCPUOnly` / `instSpecA10G` / `instSpecTeslaT4` in
`pkg/workergateway/service/helper_test.go`) with `OS` / `Arch` / `UnitResources` / `LocalStorage`, and
update the `Next` / `Handle`-path name assertions to the spec-derived names (same test data, new expected
`Name`). `TestListAggregateInstanceTypes_Result` (sorts pre-named items, does not build names) is
unaffected.

#### Unit tests
- `pkg/workergateway/service`: `2026-07-15` - `39.4%` (`buildAggregatedInstanceTypeName`; both flavor
  watch handlers `OpHandleClusterInstanceTypeFlavor` / `OpHandleAggregatedInstanceTypeFlavor`).
- `pkg/worker/extensionapis/worker`: `2026-07-15` - `4.1%` (`OnWatch`: first-add / add-dup /
  partial-delete / last-delete / bookmark).
- `pkg/workergateway/manager`: `2026-07-15` - `5.2%` (`InstanceTypeFlavor` informer-factory wiring).

#### Integration tests
None automated. The RF-watch → flavor-event path (extension apiserver) and the informer → stream path
(gateway) both need a real apiserver; they are exercised by the e2e/manual scenarios below. Concrete test
names are added after the implementation PR merges.

#### e2e tests
Manual: (a) `kubectl get instancetypeflavor -w` against a worker while a `ResourceFlavor` pool is
added/removed; (b) `curl GET /instancetypeflavors?watch=true[&aggregated=true]` against a running gateway
with ≥1 cluster subscribed **with** the `InstanceTypeFlavor` GVK, observing events on a pool change. No
automated gateway e2e harness exists (the `gpustack-operator-e2e` / `-chart-e2e` skills don't cover the
gateway read path), consistent with the `2026-07-10` predecessor; a full harness is not warranted for a
read/stream endpoint.

## Alternatives
- **Poll-based flavor watch in the gateway** (relist on a timer, no real watch): rejected — reintroduces
  the staleness the user explicitly wants gone and diverges from the `InstanceType` watch path.
- **Incremental 1:1 RF→flavor event mapping** (no recompute/diff): rejected — cannot tell whether a
  deleted RF was the last one backing a flavor, so it would drop still-valid flavors.
- **Reference the worker's `instanceTypeFlavorName` from the gateway for a unified name helper:**
  rejected — violates the separate-binary rule; the flavor name is already spec-deterministic and needs
  no gateway change.

## Open Questions
- **Name uniqueness:** is a same-name collision between two distinct normalized specs acceptable (client
  keys by name), or should the builder append a short spec hash when the dropped fields differ? (Leaning:
  accept, matching the provided templates.)
- **Non-accelerated `${cpu}c` omission** is taken as intentional (unit CPU is webhook-fixed to 1 for
  CPU-only types). Confirm.
- **Bundling:** Track A (gateway-only) and Track B (three-layer) are independently shippable — keep as one
  spec/branch or split into two? (Leaning: one spec, two independently-committable tracks.)
