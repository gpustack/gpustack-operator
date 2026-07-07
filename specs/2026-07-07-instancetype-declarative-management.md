# Spec: InstanceType Declarative Management

Status: Building
Type: Feature

## Summary
Make the worker `InstanceType` a declarative, admin-authored API that is spec-clear the moment it
is written: its inputs (group, acceleratable, unit resources, local storage, os, arch) become
required, its sizing (unit resources + local storage) becomes immutable after creation, and a new
defaulting webhook enriches the remaining descriptor fields from a matching `ResourceFlavor` at
admission. The queue side is split in two — the `InstanceTypeReconciler` now only owns the backing
`ClusterQueue`'s lifecycle and labels (plus Devices→status), while a new `NodeQueueReconciler` owns
the queue's resource groups: it pools the flavors (smallest-node-first), reactivates a drained
queue when flavors return, and — gated by a new `instance-type-drain-when-no-flavors` setting —
drains then empties a queue that has lost all its flavors without ever letting Kueue's counters go
negative. Node flavors gain a per-node `.count` label so a pool binds to one homogeneous batch of
nodes, and the `ResourceFlavor` notes carry `group`/`cores`. Finally a new list-only
`InstanceTypeFlavor` aggregated resource projects the fleet's flavors into a catalog an admin reads
to author an `InstanceType`.

## Motivation
### Goals
- An admin can author an `InstanceType` and, at write time, the API rejects an incomplete input
  (missing group / acceleratable / unit resources / local storage / os / arch) and the object comes
  back with its descriptor fields (manufacturer/product/family/memory/cores) already filled from a
  matching `ResourceFlavor`.
- The unit resources and local storage of an `InstanceType` cannot be changed after creation — a
  running Instance's sizing contract cannot drift; to change sizing an admin creates a new type.
- The backing `ClusterQueue`'s quota converges from the pool's `ResourceFlavor`s, filling
  smallest-count nodes first, and a pool that loses all its flavors is drained-then-emptied (never
  deleted) with no negative Kueue counters.
- A per-node `.count` label pins each flavor to a homogeneous batch of nodes, so once a flavor is
  reserved a workload lands on same-sized nodes.
- An admin can `kubectl get instancetypeflavors` to discover the groups (and their
  manufacturer/product/family/memory/cores) available to build an `InstanceType` on.
- Testable success: the webhooks reject/accept per the rules above; the two reconcilers converge the
  queue idempotently; `InstanceTypeFlavor` lists one deduplicated, sorted row per pool.

### Non-Goals
- No change to the accelerator slicing / soft-isolation runtime, the Instance lifecycle, or the
  credit-based scoring.
- No mixed-pool support: os and arch are now required, so every `InstanceType` pins a single
  os/arch (no os/arch-agnostic pool).
- Continuous re-sync of descriptor fields after admission — enrichment is a snapshot at
  Create/Update (see Notes); the reconciler does not refresh descriptors as hardware changes.
- No migration tooling for `InstanceType`s created before this change.

## Proposal
The `InstanceType` becomes the single declarative surface: required inputs + immutable sizing +
webhook-enriched descriptors. The queue machinery splits into a lifecycle owner
(`InstanceTypeReconciler`) and a quota owner (`NodeQueueReconciler`). Node flavors carry a `.count`
selector and `group`/`cores` notes. A read-only `InstanceTypeFlavor` catalog surfaces the pools.

### User Stories
#### Story 1
As a platform admin, I want the `InstanceType` API to reject an incomplete definition and fill in
the hardware descriptors for me, so that every `InstanceType` in the cluster is complete and
self-describing from the first day it exists.
#### Story 2
As a platform admin, I want the unit resources and local storage of an `InstanceType` frozen after
creation, so that the sizing contract of already-running Instances can never silently change.
#### Story 3
As a cluster operator, I want a pool that has lost all its nodes to drain and go empty (not vanish)
and to come back automatically when nodes return, so that transient capacity loss never deletes an
admin's type or corrupts Kueue's accounting.
#### Story 4
As a scheduler, I want a reserved flavor to keep a workload on one homogeneous batch of same-sized
nodes and to fill the smallest nodes first, so that placement is predictable and bin-packs tightly.
#### Story 5
As a platform admin, I want to list the available hardware flavors (group + manufacturer / product /
family / memory / cores), so that I know exactly what to put in a new `InstanceType`.

### Core Features & Acceptance Criteria
1. **Required inputs + regenerated API.** `spec.unitResources`, `spec.acceleratable`, `spec.group`,
   `spec.localStorage`, `spec.os`, `spec.arch` are required. The validating webhook's `ValidateCreate`
   rejects any missing/empty one (unit CPU a bare positive int; unit RAM & local storage a positive
   int with a case-sensitive `Gi` suffix; group/os/arch non-empty). The generated CRD schema marks
   them required. *Accept:* creating an `InstanceType` missing any required input is denied;
   a complete one is admitted.
2. **Immutable sizing.** `ValidateUpdate` rejects any change to `spec.unitResources` or
   `spec.localStorage` (still enforcing the required/well-formed checks). *Accept:* patching either
   is denied with an "immutable" message; patching a mutable field (e.g. a label) succeeds.
3. **Defaulting webhook enrichment.** A new mutating (defaulting) webhook runs on Create/Update: when
   `spec.group` is non-empty and the descriptors are still empty, it builds a label selector — the
   operator-owned (`nodes`) resource-type label plus `(acceleratable, group)→featureKeyLabel`,
   `(os)→kubernetes.io/os`, `(arch)→kubernetes.io/arch` — lists `ResourceFlavor`s, takes one, and
   fills the remaining descriptor spec fields (manufacturer, product, family, and — when accelerated
   — memory, cores, sliceable) from its notes. It is a no-op when `spec.group` is empty, no
   `ResourceFlavor` matches, or the descriptors are already populated (enrich-once). *Accept:* an
   `InstanceType` created with only the required inputs comes back with its descriptor fields
   populated when a matching flavor exists; unchanged when none does or when already populated.
4. **NodeFlavor notes + node `.count` label.**
   - The `ResourceFlavor` notes gain `group=${flavor.key}` and `cores=${flavor.cores}` (cores empty
     for a non-accelerated flavor).
   - `ConstructNodeCapacityLabels` writes `general.${prefix}${manufacturer}-${id}.count=${count}`
     where `${count}` is the node's `status.capacity` CPU rounded up.
   - `ExtractNodeFlavors` reads the CPU flavor's count from that `.count` label (not directly from
     capacity), and every flavor's `NodeLabels` selector includes its `.count` label so the flavor
     binds only same-count nodes. *Accept:* a `ResourceFlavor` shows `group`/`cores` notes; its
     `spec.nodeLabels` carries the `.count` selector; the CPU flavor's count equals the ceil of CPU
     capacity.
5. **NodeQueueReconciler (quota owner).** A new controller reconciles the operator-owned
   `ClusterQueue`, driven by `ClusterQueue` and `ResourceFlavor` changes. It lists the pool's
   `ResourceFlavor`s by the queue's labels, then:
   - **Flavors present:** sort ascending by count (smallest first); if `StopPolicy != None` **and**
     the current resource groups are empty, set `StopPolicy = None`; fill the resource groups from
     the flavors.
   - **No flavors, groups still defined:** if any `FlavorsReservation` Total/Borrowed is non-zero,
     set `StopPolicy = HoldAndDrain` **when `instance-type-drain-when-no-flavors` is true**, then
     requeue in 60s; once all reservations are zero, clear the resource groups (empty).
   *Accept:* the queue's quota tracks the flavors, fills small nodes first, drains-then-empties on
   flavor loss with no negative counters, and reactivates when flavors return.
6. **New setting.** `instance-type-drain-when-no-flavors` (editable bool, default `true`), read
   per-reconcile. *Accept:* toggling it flips the drain-vs-wait behavior on the next reconcile.
7. **InstanceTypeReconciler (lifecycle owner), narrowed.** It only creates the backing `ClusterQueue`
   filling labels (not resource groups), keeps the queue's schedule labels — derived from the
   `InstanceType` spec identity (group/acceleratable/os/arch) plus the entrance label — converged,
   stamps a `memory` annotation on the queue sourced from the enriched `InstanceType.spec.memory` (the
   per-card VRAM the Pod webhook reads), drives the queue through drain-then-delete on `InstanceType`
   deletion, and syncs the Devices ledger into `InstanceType.status`. It no longer refreshes descriptor
   spec fields (snapshot at admission) and no longer deletes a derived type for lack of flavors.
   *Accept:* a queue is created with labels + the memory annotation (no resource groups); deleting an
   `InstanceType` drains then removes its queue; status still reflects Devices.
8. **InstanceTypeFlavor catalog.** A new list-only aggregated resource (`instypeflavor`, cluster-scoped) pulls
   the operator-owned `ResourceFlavor`s (by resource-type label), extracts notes into a spec ordered like `InstanceTypeSpec` — Group,
   Acceleratable, Manufacturer, Product, Family, Memory, Cores, Sliceable — deduplicates identical
   entries, and sorts by manufacturer → product → memory. *Accept:* `kubectl get instancetypeflavors`
   lists one row per distinct pool, generic pools showing `acceleratable=false` with empty
   memory/cores/sliceable.

### Notes / Constraints / Caveats
- Go + controller-runtime; Kueue **v0.17.1** (`sigs.k8s.io/kueue/apis/kueue/v1beta2`). Empty
  `resourceGroups` must be verified allowed on this pinned version before relying on the "empty"
  branch (prior analysis: `validateResourceGroups` has no `minItems`, so empty is accepted — confirm
  in the build).
- The defaulting webhook follows the existing `Default(...)` precedent (`webhooks/worker/pod.go`,
  `instance.go`); it needs a cached reader to list `ResourceFlavor`s.
- Descriptor enrichment is a **one-time snapshot**: the defaulting webhook fills the descriptors only
  while they are empty (typically at create). Neither the reconciler nor a later re-apply refreshes
  them — the enrich-once guard skips an already-populated spec — so to re-derive, clear the descriptor
  fields or recreate the `InstanceType`.
- The `InstanceType` CRD is `v1alpha1` (storage, webhook target); the aggregated `v1` type is a
  proxy alias. `InstanceTypeFlavor` is added under aggregated `v1` only (peerless → conversion-gen
  skips it, list-only, no CRD, no controller), mirroring the `InstanceTypeHandler` pattern.
- The proposed `NodeQueueReconciler` sits next to the existing `NodeQueueEntranceReconciler`
  (LocalQueue owner); the two names are close but the responsibilities are disjoint.

### Boundaries
- **Always:** keep both reconcilers idempotent and level-based; read editable settings per-reconcile;
  regenerate API/CRD/openapi via `make generate` after type/marker/webhook edits; sign every commit
  (`-s`); keep the Pod webhook's per-card VRAM lookup working.
- **Ask first:** any change to the credit/CPU quota math, the accelerator status three-view, or the
  Instance/Pod admission paths; renaming `NodeQueueReconciler`.
- **Never:** delete an `InstanceType` (derived or admin) merely because its pool lost all flavors;
  let a `ClusterQueue`'s reservation counters go negative; trust a user-writable `LocalQueue` as the
  VRAM source; write resource groups from the `InstanceTypeReconciler` — the only descriptor it may
  stamp on the queue is the per-card `memory` annotation (sourced from `it.Spec.Memory`) the Pod
  webhook depends on.

### Risks and Mitigations
- **Pod webhook VRAM source → the narrowed CQ must still expose per-card VRAM.** → *Resolved (Task 4):*
  the CQ keeps a `memory` annotation, and the `InstanceTypeReconciler` sources it from the enriched
  `InstanceType.spec.memory` — so the InstanceType is the source of truth while the Pod webhook stays
  unchanged (resolve the CQ by the entrance label, read its `memory` annotation; no
  `LocalQueue → CQ → InstanceType` lookup). Sourcing the annotation from `it.Spec.Memory` is done in
  the same task that drops `applyDescriptorsFromClusterQueue`, since keeping both would form a circular
  memory flow (CQ→spec and spec→CQ); doing it earlier breaks the derived-type memory in unit tests
  (the enriching webhook does not run there).
- **Two controllers write the same `ClusterQueue` (`InstanceTypeReconciler` owns labels + teardown
  StopPolicy; `NodeQueueReconciler` owns resource groups + StopPolicy).** → Partition StopPolicy
  ownership: `NodeQueueReconciler` must ignore a queue whose `InstanceType` is being deleted (has a
  DeletionTimestamp / is locked for teardown) so it never flips a draining-for-delete queue back to
  `None`.
- **`.count`-source switch in `ExtractNodeFlavors` depends on the new node label existing.** → During
  rollout the CPU flavor briefly disappears until `ConstructNodeCapacityLabels` writes the `.count`
  label; eventual-consistency only, no data loss — note it in docs.
- **Derived-InstanceType authoring must satisfy the new required fields.** → `createDerivedInstanceType`
  must stamp `spec.group/acceleratable/os/arch` (and unit spec) so its own Create passes validation
  and the defaulting webhook enriches it.
- **Empty `resourceGroups` rejected by Kueue v0.17.1.** → *Resolved:* confirmed allowed —
  `validateResourceGroups` (kueue@v0.17.1 `pkg/webhooks/clusterqueue_webhook.go`) is a bare `for range`
  loop with no `minItems`; the only limits are upper bounds (≤256 flavors / covered resources). An
  empty list is accepted, so the "empty" branch needs no fallback.

## Design Details
### Commands
- Build: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go build -tags "goccy netgo" ./...`
- Test: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -tags "goccy netgo" ./pkg/... ./api/...`
- Lint: `make lint` (also runs via the `gpustack-operator-lint` hook after Go edits).
- Generate: `make generate` (after editing API types, markers, or webhooks — use the
  `gpustack-operator-generate` skill).
- E2E: the `gpustack-operator-e2e` skill (build+load dev image, Helm-deploy, assert the chain).

### Project Structure
- `api/worker/v1alpha1/instance_type.go` — required-field markers on `InstanceTypeSpec` (regenerate).
- `api/worker/v1/instancetypeflavor.go` — new list-only aggregated type (peerless).
- `pkg/worker/webhooks/worker/instancetype.go` — add the defaulting webhook (enrichment) + update the
  validating webhook (required on create, immutable unit/storage on update).
- `pkg/nodefeature/helper.go` — `.count` general label, `NodeFlavor.Cores`, `ExtractNodeFlavors`
  count-from-label + `.count` in `NodeLabels`.
- `pkg/worker/controllers/worker/nodeflavor.go` — `group`/`cores` notes.
- `pkg/worker/controllers/worker/instancetype.go` — narrow to lifecycle + labels + a `memory`
  annotation from `it.Spec.Memory` + Devices→status; drop descriptor refresh and the
  delete-on-no-flavors branch; stamp identity on derived types.
- `pkg/worker/controllers/worker/nodequeue.go` — new `NodeQueueReconciler` (quota owner).
- `pkg/worker/extensionapis/worker/instancetypeflavor.go` + `extensionapis/setup.go` — new handler.
- `pkg/worker/settings/value.go` — `InstanceTypeDrainWhenNoFlavors`.
- `pkg/worker/controllers/setup.go` — register `NodeQueueReconciler`.
- `docs/architecture.md`, `docs/settings.md`, `docs/development.md`, `README.md`, e2e cases — updated.

### Code Style
Editable setting, read per-reconcile (matches `value.go`):
```go
// InstanceTypeDrainWhenNoFlavors: when true (default), a pool that has lost all its
// ResourceFlavors is driven to HoldAndDrain before its resource groups are emptied;
// when false, the operator waits for reservations to clear then empties without draining.
InstanceTypeDrainWhenNoFlavors = settings.NewEditable(
    "instance-type-drain-when-no-flavors",
    "Indicates whether a ClusterQueue whose pool lost all ResourceFlavors is drained "+
        "(HoldAndDrain) before its resource groups are emptied. When true (default), it is "+
        "drained first; when false, the operator waits for reservations to clear, then empties.",
    setting.InitializeFromEnv("true"),
    setting.AllowBool(),
)
```
Conventions: typed errors early; `ctrlclix.WithoutQuorum` cached reads; `kubemeta.DeepEqual`-guarded
writes; `systemmeta` notes for operator ownership; exported symbols documented with behavior.

### Implementation Plan
Vertically sliced and ordered so every commit leaves the operator building, testing green, and the
scheduling chain functional. Each task is TDD (RED → GREEN → suite → `make lint`), committed with `-s`.

- [x] **Task 1 — ResourceFlavor `group`/`cores` notes + node `.count` label + count-pinned flavors.**
  - `pkg/nodefeature/helper.go`: add `NodeFlavor.Cores`; `ConstructNodeCapacityLabels` writes
    `general.${prefix}${manufacturer}-${id}.count = ${count}` where `${count} = capacityCPU.Value()`
    (rounds up); `ExtractNodeFlavors` reads the CPU flavor's count from that `.count` label (skip when
    absent/zero), sets `Cores` for device flavors from `<nodeKey>.cores`, and adds each flavor's
    `.count` label into its `NodeLabels` selector (general `.count` for the CPU flavor, acceleratable
    `.count` for a device flavor).
  - `pkg/worker/controllers/worker/nodeflavor.go`: `eNotes["group"] = flavor.Key`,
    `eNotes["cores"] = flavor.Cores` (empty for a non-accelerated flavor).
  - *Acceptance:* a `ResourceFlavor` carries `note.gpustack.ai/group` + `note.gpustack.ai/cores`; its
    `spec.nodeLabels` includes the `.count` selector; the CPU flavor's count equals the ceil of CPU
    capacity read from the label.
  - *Verify:* `go test ./pkg/nodefeature/... ./pkg/worker/controllers/worker/... -run 'NodeFlavor|Extract|ConstructNodeCapacity'`; `make lint`.

- [x] **Task 2 — `InstanceTypeFlavor` list-only aggregated resource.**
  - `api/worker/v1/instancetypeflavor.go`: peerless type + `InstanceTypeFlavorSpec` ordered like
    `InstanceTypeSpec` — Group, Acceleratable, Manufacturer, Product, Family, Memory, Cores,
    Sliceable — with markers (`+genclient`, `+genclient:nonNamespaced`, `+genclient:onlyVerbs=list`,
    deepcopy, `+k8s:apireg-gen:resource:scope="Cluster",categories=["gpustack"],shortName=["instypeflavor"]`)
    and a `List` type.
  - `pkg/worker/extensionapis/worker/instancetypeflavor.go`: `InstanceTypeFlavorHandler`
    (`extensionapi.ObjectInfo` + `WithList`/`ListOperation`); `OnList` lists `ResourceFlavor`s
    (`_ResourceFlavorResType`), builds one flavor per RF from the notes
    (group/acceleratable/manufacturer/product/family/memory/cores), deduplicates by full spec, sorts
    manufacturer → product → memory.
  - `pkg/worker/extensionapis/setup.go`: register the handler; run `make generate`.
  - *Acceptance:* `kubectl get instancetypeflavors` (`itf`) lists one row per distinct pool; a generic
    pool shows `acceleratable=false` with empty memory/cores; the list is sorted and deduplicated.
  - *Verify:* aggregation unit test; `make generate` clean; `make lint`.

- [x] **Task 3 — InstanceType webhooks (required create / immutable update / defaulting enrichment) + derived stamping.**
  - `api/worker/v1alpha1/instance_type.go`: mark Group/Acceleratable/OS/Arch/UnitResources/LocalStorage
    required (drop `omitempty` + required markers); `make generate`.
  - `pkg/worker/webhooks/worker/instancetype.go`: add Client/APIReader fields wired in `SetupWebhook`
    (like `InstanceWebhook`); add `Default` — when `spec.group != ""` and the descriptors are still
    empty, build a selector from the operator-owned (`nodes`) resource-type label plus
    `featureKeyLabel(acceleratable, group)` + `kubernetes.io/os` + `kubernetes.io/arch`, list one
    `ResourceFlavor`, fill Manufacturer/Product/Family and (when accelerated) Memory/Cores/Sliceable
    from its notes; no-op when group is empty, no flavor matches, or the descriptors are already
    populated (enrich-once). `ValidateCreate` requires group/os/arch non-empty + a well-formed unit
    spec; `ValidateUpdate` rejects any `unitResources`/`localStorage` change (plus the required
    checks). Add the mutating webhook-gen marker.
  - `pkg/worker/controllers/worker/instancetype.go`: `createDerivedInstanceType` stamps
    `spec.group/acceleratable/os/arch` (+ unit spec) so its own `Create` passes validation and the
    defaulting webhook enriches it; keep only the derived-marker label.
  - *Acceptance:* creating an `InstanceType` missing a required input is denied; a complete one is
    admitted with descriptors filled from a matching flavor (unchanged when none matches); patching
    `unitResources`/`localStorage` is denied.
  - *Verify:* webhook unit tests (create-required, update-immutable, default-enriches with a fake RF);
    `make generate` clean; `make lint`. *(The reconciler still fills quota here — a harmless overlap.)*

- [ ] **Task 4 — Split queue ownership: `NodeQueueReconciler` (quota) + narrowed `InstanceTypeReconciler` (lifecycle) + setting; feed the CQ memory annotation from the InstanceType.**
  - `pkg/worker/settings/value.go`: add `InstanceTypeDrainWhenNoFlavors` (editable bool, default
    `true`), read per-reconcile.
  - `pkg/worker/controllers/worker/nodequeue.go` (new `NodeQueueReconciler`): `For(ClusterQueue)`
    (operator-owned) + `Watches(ResourceFlavor)` enqueuing the CQs whose schedule labels match the
    flavor's pool. Reconcile: skip a CQ that is not operator-owned or whose `InstanceType` has a
    `DeletionTimestamp` (teardown owns StopPolicy then). List the pool's `ResourceFlavor`s by the CQ's
    schedule labels (feature-key + os + arch) via `MatchingLabels`. **Flavors present:** sort ascending
    by count, set `StopPolicy=None` when it was stopped with empty groups, fill the resource groups
    (move `buildResourceGroups`/credit-vs-cpu here, reading acceleratable/manufacturer from the RF
    notes). **No flavors + groups defined:** when any `FlavorsReservation` Total/Borrowed ≠ 0 →
    `StopPolicy=HoldAndDrain` if `instance-type-drain-when-no-flavors` is true, then requeue 60s; once
    all reservations are zero → clear the resource groups (empty). All writes `DeepEqual`-guarded.
  - `pkg/worker/controllers/worker/instancetype.go`: `ensureClusterQueue` creates/aligns the CQ with
    **schedule labels built from `it.Spec`** (`featureKeyLabel(it.Spec.Acceleratable, it.Spec.Group)`,
    os, arch) + the entrance label + **a `memory` annotation sourced from `it.Spec.Memory`** (the
    InstanceType, enriched by the Default webhook, feeds the per-card VRAM the Pod webhook reads) — no
    resource groups, StopPolicy no longer converged post-create (owned by NodeQueue + teardown). Drop
    `applyDescriptorsFromClusterQueue` (snapshot-at-admission; removing it is what makes sourcing the CQ
    memory from `it.Spec.Memory` non-circular — the reconciler no longer reads descriptors back from the
    CQ) and the `len(rfList)==0 && derived → Delete` branch. Keep the derived isolation policy (empty
    cohort, preemption, fungibility, node-devices AdmissionCheck ref), Devices→status (`computeStatus`
    reads acceleratable from `it.Spec`), and teardown drain-then-delete. Move the flavor-quota helpers
    to `nodequeue.go`. The **Pod webhook is unchanged** — it still resolves the operator CQ by the
    entrance label and reads its `memory` annotation (no `LocalQueue → CQ → InstanceType` lookup).
  - `pkg/worker/controllers/setup.go`: register `NodeQueueReconciler`.
  - *Acceptance:* creating an IT yields a labels + memory-annotation CQ; NodeQueue fills the quota
    smallest-count first; losing all flavors drains-then-empties with no negative counters and
    reactivates when flavors return; deleting an IT drains then deletes its CQ; status still tracks
    Devices; toggling the setting flips drain-vs-wait; a sliced memory-mib Pod still folds from the CQ
    memory annotation.
  - *Verify:* `go test ./pkg/worker/... -run 'InstanceType|NodeQueue|Pod'`, then the full suite + build;
    `make lint`. **Checkpoint: run the entire test suite and build before continuing.**

- [ ] **Task 5 — Docs.** `docs/architecture.md` (webhook enrichment + immutable sizing, `.count`
  pinning, group/cores notes, the queue-ownership split, drain-then-empty + reactivate,
  InstanceTypeFlavor), `docs/settings.md` (new setting row), `docs/development.md` (InstanceTypeFlavor
  in the inventory), `README.md` (declarative InstanceType + catalog). *Verify:* links resolve, wording
  matches the shipped behavior.

- [ ] **Task 6 — E2E cases.** Via the `gpustack-operator-e2e` skill: required-field rejection on
  create; enrichment fills descriptors on create; immutable unit/storage rejection on update;
  drain-then-empty + reactivate on flavor loss; `InstanceTypeFlavor` catalog list; derived restore on
  delete. Update `SKILL.md` + references. *Verify:* `bash -n` each case; `chmod +x` new cases.

- [ ] **Task 7 — Package + live-cluster verify (user-driven).** Package the dev image on the amd64
  builder (`PACKAGE_ARCH=amd64 PACKAGE_NAMESPACE=thxcode PACKAGE_PUSH=true make package`), Helm-deploy
  to a reachable Kubernetes cluster, and run the e2e verifications (InstanceTypeFlavor list; admin
  InstanceType create + enrich + admit; required-field + immutability rejection; drain-then-empty +
  reactivate; derived restore). *Verify:* the e2e case suite passes on the cluster.

### Test Plan
[ ] I/we understand the owners of the involved components may require updates to existing tests to make
this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates
Rework the existing `InstanceType`/`NodeFlavor`/webhook tests for the new required fields, the
immutable unit spec, the narrowed reconciler (labels-only CQ, no descriptor refresh, no
delete-on-no-flavors), and the `.count`-from-label flavor sizing.

#### Unit tests
Every added unit has table-driven coverage. Per-package targets (`2026-07-07`):
- `pkg/nodefeature`: `2026-07-07` - ~85% (`.count` label = ceil(CPU); count-from-label; `.count` in
  `NodeLabels`; `NodeFlavor.Cores`).
- `pkg/worker/controllers/worker`: `2026-07-07` - ~80% (new `nodequeue_test.go`; narrowed
  `instancetype_test.go`; `nodeflavor_test.go` group/cores notes).
- `pkg/worker/webhooks/worker`: `2026-07-07` - ~85% (default-enriches incl. the enrich-once +
  accel-memory guard; require-on-create; immutable-unit/storage).
- `pkg/worker/extensionapis/worker`: `2026-07-07` - ~80% (InstanceTypeFlavor aggregation: dedup, sort,
  acceleratable=false for generic).

#### Integration tests
Fake-client controller tests (post-merge names):
- `TestNodeQueueReconciler_FillsAndSortsByCount`, `_ReactivatesOnFlavorReturn`,
  `_DrainThenEmptyRespectsReservations`, `_DrainSettingToggle`, `_SkipsDeletingInstanceType`.
- `TestInstanceTypeReconciler_CreatesLabelsOnlyQueue`, `_TeardownDrainsThenDeletes`,
  `_NoDeleteOnFlavorLoss`, `_StatusTracksDevices`.
- `TestInstanceTypeWebhook_DefaultEnriches` (incl. the accel-memory guard), `_RequireOnCreate`,
  `_ImmutableUnitAndStorage`.
- `TestInstanceTypeReconciler_QueueCarriesMemoryAnnotation` (fed from `it.Spec.Memory`);
  `TestPodWebhook_Default` still folds a sliced memory-mib request from the CQ memory annotation.

#### e2e tests
Via the `gpustack-operator-e2e` skill on a reachable cluster: required-field rejection; enrichment on
create; immutable unit/storage rejection; drain-then-empty + reactivate on flavor loss;
`InstanceTypeFlavor` catalog list; derived restore on delete. Empty `resourceGroups` allowance is
confirmed at the Kueue v0.17.1 source level (no separate e2e gate needed).

## Alternatives
- **Keep enrichment in the reconciler (no defaulting webhook).** Rejected: the requirement is
  spec-clarity at write time; a webhook makes the stored object complete on day one, and the two
  chosen answers fix descriptors as an admission snapshot.
- **One reconciler keeps owning the whole queue.** Rejected: the requirement explicitly splits quota
  management into `NodeQueueReconciler`, isolating the drain/empty/reactivate logic from the type
  lifecycle.
- **Empty the resource groups immediately on flavor loss.** Rejected: emptying while reservations are
  outstanding drives Kueue's counters negative — the drain/wait gate exists to prevent that.

## Open Questions
Both prior questions were resolved during planning:
- **Pod webhook VRAM source** → the CQ keeps a `memory` annotation fed from the enriched
  `InstanceType.spec.memory`; the Pod webhook reads that annotation unchanged (no InstanceType
  lookup). Wired in Task 4. See Risks.
- **NodeQueueReconciler flavor lookup** → a **label selector** (feature-key + os + arch), not the
  pool-name index (`IndexingResourceFlavorByNodeQueue`): admin-created `InstanceType` names are
  arbitrary, so only derived types are named `gpustack-${key}-${os}-${arch}` and the name index cannot
  resolve an admin pool. This also makes the CQ's schedule labels **derived from `it.Spec`**
  (group/acceleratable/os/arch), not copied from `it.Labels` (Task 4).

None remaining.
