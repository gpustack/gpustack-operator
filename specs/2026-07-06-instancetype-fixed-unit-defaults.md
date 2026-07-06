# Spec: InstanceType Fixed Unit-Resource Defaults

Status: Shipped
Type: Feature

## Summary
Replace the node-derived, ResourceFlavor→ClusterQueue→InstanceType unit-spec pipeline with a
single fixed default that lives only on the InstanceType. A non-accelerated InstanceType defaults
to `1` CPU / `2Gi` RAM / `100Gi` local storage; an accelerated one to `4` CPU / `16Gi` RAM /
`100Gi` local storage. Administrators keep overriding these per InstanceType through its API.
`DeriveNodeUnitSpec` and the two-stage min-of-mins aggregation are removed, so the default is
predictable across heterogeneous hardware and trivially standardized.

## Motivation
### Goals
- Make the default per-unit spec a fixed, hardware-independent constant so defaults are easy to
  reason about and roll out cluster-wide.
- Non-accelerated default: `unitCPU=1`, `unitRAM=2Gi`, `localStorage=100Gi` (CPU/RAM unchanged
  from today; local storage now fixed instead of tracking the node disk).
- Accelerated default: `unitCPU=4`, `unitRAM=16Gi`, `localStorage=100Gi` (replaces the previous
  per-device `cores/count`, `ramGi/count`, node-disk derivation).
- Administrators can still override `unitResources.cpu` / `unitResources.ram` / `localStorage`
  per InstanceType; the override survives reconciles (the non-accelerated `unitCPU` stays pinned
  to `1`, as today).
- Remove `DeriveNodeUnitSpec` and the RF-notes → CQ-notes → IT-spec passing chain; the unit spec
  has a single home on `InstanceType.spec`.
- Success is testable: the materialized InstanceType `.spec` carries the fixed defaults per
  acceleratable-ness, admin overrides take effect, and an Instance submitted against each type is
  sized from the new unit (whole-card `unit × count`, sliced `unit × memory%`, local-storage cap
  = `100Gi`).

### Non-Goals
- No change to the Kueue credits/CPU quota model (`buildResourceGroups`); `localStorage` is not a
  Kueue-covered resource and never was.
- No change to the Pod admission webhook VRAM fold (still reads the ClusterQueue `memory` note).
- No change to the InstanceType three-view status computation or the AdmissionCheck.
- No in-place migration of pre-existing InstanceTypes that already carry node-derived unit values
  (see Risks); the rollout is a fresh deploy.
- No change to admin-override semantics or the non-accelerated `unitCPU=1` pin.

## Proposal
The operator stops deriving unit resources from node capacity. When it authors a derived
InstanceType it stamps the fixed default triple chosen by acceleratable-ness. The unit spec is no
longer written into ResourceFlavor or ClusterQueue notes, and is no longer aggregated across
flavors: the ResourceFlavor → ClusterQueue → InstanceType chain now carries **only device
information**, and `applyDescriptorsFromClusterQueue` never touches the unit spec. The InstanceType
`.spec` is the sole source of truth for the unit spec, guaranteed complete at admission — a derived
type is stamped at creation, and the InstanceType validating webhook **requires** the full triple on
an admin-created one (an empty/partial spec is rejected). It is read by the Instance
validating/defaulting webhook exactly as today.

### User Stories
#### Story 1
As a cluster administrator, I want the materialized InstanceType to carry sensible fixed
unit-resource defaults (non-accelerated `1c/2Gi/100Gi`, accelerated `4c/16Gi/100Gi`) regardless of
node hardware, so that default scheduling behavior is predictable and standardizable across a
heterogeneous fleet.
#### Story 2
As an administrator, I want to override any InstanceType's unit spec through its API
(`unitResources.cpu/ram` + `localStorage`), so that a pool needing a different ratio is a single
edit whose value survives reconciles (the non-accelerated CPU unit stays pinned to `1`).
#### Story 3
As a maintainer, I want the unit spec to stop flowing ResourceFlavor→ClusterQueue→InstanceType
(node derivation + min-of-mins aggregation removed), so that the code is simpler and the unit spec
has one home.

### Core Features & Acceptance Criteria
1. **Fixed defaults on the derived InstanceType.** After the chain materializes, a non-accelerated
   InstanceType's `.spec.unitResources` = `{cpu:"1", ram:"2Gi"}`, `.spec.localStorage="100Gi"`; an
   accelerated one = `{cpu:"4", ram:"16Gi"}`, `.spec.localStorage="100Gi"`.
2. **No unit spec in notes.** ResourceFlavor and ClusterQueue `note.gpustack.ai/*` no longer carry
   `unitCPU`/`unitRAM`/`localStorage`; `memory`(VRAM), `sliceable`, and the descriptive fields
   remain.
3. **Admin override still works, and is required.** The InstanceType validating webhook requires the
   full unit-spec triple (empty/partial rejected). Patching `spec.unitResources`/`spec.localStorage`
   on an InstanceType persists across reconciles; a non-accelerated `unitCPU` edit is reset to `1`.
4. **`applyDescriptorsFromClusterQueue` is device-info only.** It applies acceleratable / manufacturer
   / product / family / VRAM / sliceable / os-arch from the queue notes and never fills or reads the
   unit spec.
5. **`DeriveNodeUnitSpec` removed** from `pkg/nodefeature/helper.go` (and its test); the
   `NodeFlavor` doc comment no longer references it.
6. **Instance sizing from the new unit.** An Instance submitted against each type is sized from
   the fixed unit: whole-card `unit × count`, sliced `unit × memory%`, and a local-storage request
   is capped at `100Gi`.
7. **Docs + e2e aligned.** `docs/architecture.md` (Stage 3/4 + example) and the e2e cases
   (`case-6`, `references/drain-recycle.md`) reflect the single-home unit spec; `case-9`/`case-10`
   (admin overrides) still pass.

### Notes / Constraints / Caveats
- Go + Kubernetes controller-runtime operator; conventions in `CLAUDE.md` / `docs/development.md`.
- Consumers to leave untouched: Instance webhook reads `instType.Spec.UnitResources`/`LocalStorage`;
  Pod webhook reads the CQ `memory` note only.
- Removing the derivation path orphans `adminUnitNotes`, `minPositiveNumeric`,
  `extractPositiveNumberFromString`, `extractPositiveNumberFromQuantity` **in the controller file**
  (`instancetype.go`) — remove them. The **webhook** file keeps its own copies (still validate the
  admin spec).
- Fixed defaults defined once in `pkg/worker/controllers/worker/instancetype.go` —
  `defaultUnitResources(acceleratable) (cpu, ram string)` plus `const defaultUnitLocalStorage = "100Gi"`
  (`localStorage` does not vary by acceleratable, so a 3-value return trips `unparam`) — consumed only
  by `createDerivedInstanceType` (stamp at creation). The unit spec is guaranteed complete elsewhere by
  the tightened InstanceType validating webhook, so the reconciler needs no fill.

### Boundaries
- **Always:** run `make generate` after API/webhook edits and `make lint` after Go edits; require a
  complete well-formed unit-spec triple (the webhook rejects empty/partial); keep the non-accel
  `unitCPU=1` pin.
- **Ask first:** any change to the credits/quota model, the Pod-webhook VRAM path, or the
  three-view status; any change to the fixed constant values beyond `1/2Gi/100Gi` and `4/16Gi/100Gi`.
- **Never:** reintroduce node-capacity-derived unit values; write unit spec back into RF/CQ notes;
  break the Instance/Pod webhook contracts.

### Risks and Mitigations
- **Existing InstanceTypes keep old node-derived values on in-place upgrade** → deploy fresh on
  a fresh cluster; note that already-populated `.spec.unitResources` is not overwritten (only filled when
  empty). If in-place reset is later required, it's a separate migration.
- **Local storage cap shrinks/grows vs node disk** (was node total, now fixed `100Gi`) → intended;
  admins override `localStorage` per type when a pool needs more.
- **Orphaned helpers left compiling but dead** → remove them in the same change; `make lint`
  (unused) guards.
- **Tightened webhook rejects a pre-existing empty-spec InstanceType on update during a rolling
  upgrade** → a fresh deploy avoids it (old empty-spec derived types no longer exist); the operator
  always writes a complete triple. If in-place upgrade is later needed, backfill the unit spec first.

## Design Details
### Commands
```
GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/nodefeature/... ./pkg/worker/controllers/worker/... ./pkg/worker/webhooks/worker/...
make generate            # after any api/webhook edit
make lint                # golangci-lint (unused/dead-code guard)
make test                # full suite
# package on the amd64 builder host, then deploy via Helm on a reachable Kubernetes cluster:
PACKAGE_ARCH=amd64 PACKAGE_NAMESPACE=thxcode PACKAGE_PUSH=true make package   # run on the amd64 builder host
```
### Project Structure
```
pkg/nodefeature/helper.go                          # remove DeriveNodeUnitSpec; fix NodeFlavor doc
pkg/nodefeature/helper_test.go                     # remove TestDeriveNodeUnitSpec
pkg/worker/controllers/worker/nodeflavor.go        # drop unitCPU/unitRAM/localStorage from RF notes
pkg/worker/controllers/worker/instancetype.go      # fixed defaults; device-only applyDescriptors; drop CQ-note unit derivation + orphaned helpers
pkg/worker/controllers/worker/instancetype_test.go # rework UnitSpecDerivation / DerivedInitializesUnitSpec tests
pkg/worker/controllers/worker/nodeflavor_test.go   # drop RF-note unit assertions
pkg/worker/webhooks/worker/instancetype.go         # require the full unit-spec triple (reject empty/partial)
pkg/worker/webhooks/worker/instancetype_test.go    # empty spec now rejected, not accepted
docs/architecture.md                               # Stage 3/4 + example
.claude/skills/gpustack-operator-e2e/cases/case-6.sh            # unit spec now on IT.spec, not CQ notes
.claude/skills/gpustack-operator-e2e/references/drain-recycle.md
```
Two precise notes on `instancetype.go`:
- `assembleClusterQueueNotes` loses its `it *InstanceType` param (the unit `switch` is removed; only
  descriptive notes remain) — update the single caller (~L242). The orphaned `adminUnitNotes`,
  `minPositiveNumeric`, `extractPositiveNumberFromString`, `extractPositiveNumberFromQuantity` are deleted;
  the **webhook** file keeps its own `extractPositiveNumberFrom*` copies (still validate the admin spec).
- `syncInstanceType`'s non-accelerated `unitCPU→1` reset is **kept** (still resets an admin edit that sets
  a non-accelerated CPU unit to anything but `1`).
### Code Style
```go
// defaultUnitLocalStorage is the fixed local-storage cap of a derived InstanceType's unit
// spec, the same for accelerated and non-accelerated types. Admins override it per
// InstanceType through its API.
const defaultUnitLocalStorage = "100Gi"

// defaultUnitResources returns the fixed per-unit CPU/RAM for an InstanceType, chosen by
// acceleratable-ness: a non-accelerated unit is 1 CPU / 2Gi, an accelerated unit (one whole
// card) is 4 CPU / 16Gi. Admins override these per InstanceType through its API.
func defaultUnitResources(acceleratable bool) (cpu, ram string) {
	if acceleratable {
		return "4", "16Gi"
	}
	return "1", "2Gi"
}
```
### Implementation Plan
Ordered so each task leaves the tree compiling and green. Tasks 1–3 are code (behavior, cleanup, then the
call-site simplification the change unlocks — compile-safe: the consumer stops needing note-seeding before
the producer drops the notes, and the min-node pick is collapsed only after its sole reason is gone); Task 4
is docs/e2e; Task 5 is package + verification on a reachable Kubernetes cluster.

[x] **Task 1 — Fixed unit defaults become the InstanceType's unit spec, enforced at admission.**
    - Add `defaultUnitResources(acceleratable) (cpu, ram string)` + `const defaultUnitLocalStorage = "100Gi"`
      in `instancetype.go` (a 3-value return trips `unparam`, since localStorage is invariant).
    - `createDerivedInstanceType`: stamp `spec.UnitResources.CPU/RAM` from `defaultUnitResources(acceleratable)`
      + `spec.LocalStorage = defaultUnitLocalStorage` (acceleratable read from the RF notes it already reads).
    - `applyDescriptorsFromClusterQueue`: **remove** any unit-spec handling — it applies device-descriptor
      fields only (acceleratable/manufacturer/product/family/VRAM/sliceable/os-arch). Keep the non-accel
      `unitCPU→1` reset in `syncInstanceType`.
    - `pkg/worker/webhooks/worker/instancetype.go`: tighten `validateInstanceTypeUnitSpec` to **require** the
      full well-formed triple (drop the accept-empty branch); update its doc.
    - Tests: rework `TestInstanceTypeReconciler_DerivedInitializesUnitSpec` + `UnitSpecDerivation` to assert
      the fixed defaults on `IT.spec` (accel 4/16Gi/100Gi, cpu 1/2Gi/100Gi) independent of RF notes, and remove
      the now-unused `unitSpec` test helper; flip the webhook test's empty case from accepted to rejected.
      (These test reworks move here from Task 2 because stamping at creation neutralizes the CQ-note derivation
      they covered.)
    - Acceptance: a derived accel IT `.spec` = 4/16Gi/100Gi, non-accel = 1/2Gi/100Gi; an admin-created IT with
      an empty/partial unit spec is rejected; `applyDescriptorsFromClusterQueue` touches no unit field.
    - Verify: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/controllers/worker/... ./pkg/worker/webhooks/worker/...` + `make lint`.

[x] **Task 2 — Stop recording unit spec in RF/CQ notes; remove derivation + orphans.**
    - `nodeflavor.go`: drop the `DeriveNodeUnitSpec` call and `unitCPU/unitRAM/localStorage` from `eNotes`;
      fix the reconciler doc comments.
    - `instancetype.go`: `assembleClusterQueueNotes` drops the unit `switch`, the non-accel pin, and its `it`
      param (caller at ~L242 updated); delete `adminUnitNotes`, `minPositiveNumeric`,
      `extractPositiveNumberFromString`, `extractPositiveNumberFromQuantity`.
    - `helper.go`: remove `DeriveNodeUnitSpec`; fix the `NodeFlavor` doc comment.
    - Tests: `nodeflavor_test.go` ActiveShape drops the unit-note assertions; `instancetype_test.go` —
      `CreatesClusterQueue` drops CQ-note unit asserts (assert IT.spec instead), and the unit fields are dropped
      from `newNodesFlavor` default notes; `helper_test.go` removes `TestDeriveNodeUnitSpec` and fixes the L408
      comment. (`UnitSpecDerivation` rework + `unitSpec` helper removal were done in Task 1.)
    - Acceptance: RF/CQ `note.gpustack.ai/*` carry no unit keys; `memory`/`sliceable` remain.
    - Verify: `make lint` (unused guard) + `go test ./pkg/nodefeature/... ./pkg/worker/controllers/worker/...`.
    - **Checkpoint:** full `make test` + `make lint` green.

[x] **Task 3 — Simplify the node-selection call sites now that the unit spec is not node-derived.**
    - `nodeflavor.go`: the flavor identity (key/os/arch/count/manufacturer/product/family/memory) and capacity
      (`len(contributors) × Count`) are shared by every contributor to a flavor name, so the most-constrained-node
      pick is no longer needed — take `contributors[0]` for `matchNodeFlavor` and `nodeFlavorSliceable`. Delete
      `lessConstrained` and `capacityValue` (now unused) and rewrite the reconciler/inline doc comments that
      describe deriving the unit spec from the most-constrained node.
    - Audit the rest of the chain for vestigial aggregation left by the unit-spec removal (confirm no other caller
      assumes a node-derived unit or a min-of-flavors fold; the instancetype flavor-min was removed in Task 2).
    - Tests: no test references `lessConstrained`/min-node by name; keep the active-shape tests green with
      first-contributor identity; keep/adjust a multi-contributor pooling test so identity + capacity are
      contributor-order-independent.
    - Acceptance: reconciler uses the first contributor; no min-node/`lessConstrained`/`capacityValue` code
      remains; flavor identity + capacity unchanged.
    - Verify: `go test ./pkg/worker/controllers/worker/...` + `make lint` (unused guard).

[x] **Task 4 — Docs + e2e alignment.**
    - `docs/architecture.md` L178/215/216/227/247: fixed default, single home on InstanceType, no node
      derivation, no RF/CQ unit notes; example shows accel 4c-16g / non-accel 1c-2g / 100Gi.
    - `case-6.sh` (item 3, L16-18/228-254): assert the admin edit persists on `it.spec.unitResources.cpu`
      (accel → stays "2", not pinned) and touches no worker NodeFeature; drop the `note.gpustack.ai/unitCPU`
      CQ assertion.
    - `references/drain-recycle.md` L126-127: point at the InstanceType spec, not CQ notes.
    - Acceptance: docs/e2e read consistently; no dangling CQ-note-unit references.

[x] **Task 5 — Package + deploy + verify on a Kubernetes cluster.**
    - On the amd64 builder host: `PACKAGE_ARCH=amd64 PACKAGE_NAMESPACE=thxcode PACKAGE_PUSH=true make package`.
    - Helm-install the built chart/image on a reachable Kubernetes cluster the environment provides (the
      distro/provider is irrelevant — only that it is reachable and can be verified against).
    - Verify: materialized non-accel IT `.spec` = 1/2Gi/100Gi, accel = 4/16Gi/100Gi; admin override persists;
      submit one Instance per type → sized from the unit (whole-card `unit×count`, sliced `unit×memory%`),
      local-storage request capped at 100Gi.
    - Acceptance: specs match; Instances admitted and correctly sized.
### Test Plan
[ ] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates
Rework the tests that assert the removed RF/CQ unit notes or node derivation before deleting the producer:
`TestDeriveNodeUnitSpec`, `TestNodeFlavorReconciler_ActiveShape`,
`TestInstanceTypeReconciler_{CreatesClusterQueue,UnitSpecDerivation,DerivedInitializesUnitSpec}`; remove the
`unitSpec` test helper and the unit fields from `newNodesFlavor` defaults. For Task 3, confirm no test pins the
min-node selection and keep a multi-contributor pooling assertion (identity + capacity order-independent).

#### Unit tests
- `pkg/nodefeature`: 2026-07-06 - target ≥ current (drop `TestDeriveNodeUnitSpec`; no new logic added here).
- `pkg/worker/controllers/worker`: 2026-07-06 - target ≥ current — new `defaultUnitResources` + the
  stamp-at-creation path in `createDerivedInstanceType` covered by the reworked `DerivedInitializesUnitSpec`
  (accel 4/16Gi/100Gi, cpu 1/2Gi/100Gi) and `UnitSpecDerivation` (admin override + non-accel `unitCPU→1` on
  `IT.spec`) tests; the Task 3 first-contributor selection is covered by the active-shape + multi-contributor
  pooling tests; measured via `make test` (coverage → `.dist/test/coverage.out`).
- `pkg/worker/webhooks/worker`: 2026-07-06 - target ≥ current — `validateInstanceTypeUnitSpec` now requires the
  full triple; `TestInstanceTypeWebhook_ValidateUnitSpec` flips its empty case to rejected. Instance-sizing
  tests still confirm sizing reads `InstanceType.spec` (unchanged).

#### Integration tests
None — the operator has no separate integration suite; controller behavior is exercised by the fake-client
reconciler unit tests above and the e2e cases below.

#### e2e tests
- `case-6` (reworked): admin unit-spec edit (full triple) persists on `InstanceType.spec` and touches no worker NodeFeature.
- `case-9` / `case-10` / `case-12`: unchanged (each patches its own unit spec) — regression guard that admin
  override + slice sizing still hold.
- Cluster end-to-end (Task 5): materialized fixed defaults per acceleratable-ness, admin override, and an Instance
  per type sized from the unit with local-storage capped at 100Gi.

## Alternatives
- **Keep unit spec mirrored in ClusterQueue notes for admin overrides** — rejected: retains part of
  the passing chain the proposal removes; the Instance webhook already reads the InstanceType spec
  directly, so CQ notes are redundant.
- **Fill defaults in `applyDescriptorsFromClusterQueue` (reconciler fill) instead of stamping** —
  rejected: leaves a transient window where a freshly authored InstanceType has no unit spec, which the
  Instance webhook would fail on, and keeps unit-spec logic in the reconciler. Stamp at creation and let
  the validating webhook require the triple, so the reconciler never touches the unit spec.
- **Keep the webhook accepting an empty unit spec + reconciler fills it** — rejected: an admin-created
  empty InstanceType would then have no unit spec until a reconcile, and the RF→CQ→IT chain would still
  carry unit data. Requiring the triple at admission makes the invariant hold everywhere with no fill.

## Open Questions
- None outstanding. (Verification depth confirmed: materialize + submit an Instance end-to-end on a
  reachable Kubernetes cluster with an accelerator pool.)
