# Spec: InstanceType CPU-Manufacturer Awareness

Status: Building
Type: Feature

## Summary
Re-introduce the structured CPU reflection that commit `a253a60a` added and commit `2717d6b1`
removed, re-shaped for today's `NodeFlavor → ResourceFlavor → ClusterQueue/InstanceType` pooling
chain. The general(CPU) node key now **always** carries the node's real CPU identity (the
`generalNodeKeyWithCPUName` startup toggle is deleted). Every `ResourceFlavor` becomes the finest,
setting-independent grain — a non-accelerated flavor is named `gpustack--${gKey}-${os}-${arch}-${cpu}c`
and an accelerated flavor `gpustack--${gKey}--${aKey}-${os}-${arch}-${accelerator}d` (accelerated
flavors now carry the CPU key too) — and each flavor is stamped with a
`feature.gpustack.ai/acceleratable=true|false` discriminator plus a `cpuDetail` note recording the
raw CPU information. A new editable setting `instance-type-aware-cpu-manufacturer` (default `false`)
governs only the **aggregation layer** (`ClusterQueue` / `InstanceType` / `InstanceTypeFlavor`): when
`false` the CPU manufacturer is ignored, so non-accelerated flavors collapse into a single generic
pool per os/arch and accelerated flavors pool per-accelerator; when `true` the CPU manufacturer
becomes a discriminator, so pools split per CPU (generic → per-`gKey`, accelerated → per-`(gKey,aKey)`).
The `InstanceType` spec's `Group` field is renamed `AcceleratorGroup` and a new `GeneralGroup` field
is added; the defaulting webhook enriches descriptors — and, when awareness is on, the `cpuDetail` —
from a matching flavor.

## Motivation
### Goals
- **CPU identity is always real.** `ExtractGeneralNodeKey` always blends the node's CPU identity into
  the general key (from the NFD `feature.gpustack.ai/cpu-name` annotation, falling back to the NFD
  cpu-model family/id labels, then to the vendor id, then to `generic`), with no startup toggle.
  Two nodes with different CPUs never share one CPU `ResourceFlavor`.
- **One switch to make scheduling CPU-manufacturer-aware.** An operator sets
  `instance-type-aware-cpu-manufacturer` to control whether the derived `ClusterQueue`/`InstanceType`/
  `InstanceTypeFlavor` split by CPU manufacturer. `false` (default) keeps today's behavior — one
  generic pool per os/arch and one pool per accelerator; `true` splits every pool by the CPU key.
- **Flavors are the finest, stable grain.** A `ResourceFlavor` always encodes both the CPU key and
  (for accelerated) the accelerator key, carries the `feature.gpustack.ai/acceleratable` boolean, and
  its notes carry the raw CPU detail — independent of the awareness setting, so flipping the setting
  never rewrites flavors, only how the aggregation layer groups them.
- **Raw CPU visible where it matters.** A non-accelerated `ResourceFlavor` always carries a
  `cpuDetail` note; an accelerated one carries it only when awareness is on; the `InstanceType`
  defaulting webhook writes `cpuDetail` back into the `InstanceType` spec only when awareness is on.
- **Selectors never cross generic and accelerated pools.** Every pool selector carries the
  `feature.gpustack.ai/acceleratable` boolean, so an aware generic pool (`general.${gKey}=true`) can
  never match an accelerated flavor that also carries `general.${gKey}=true`.
- Testable success: the general key reflects CPU identity for every node; flavor names/labels/notes
  match the tables below; toggling `instance-type-aware-cpu-manufacturer` reshapes only the
  aggregation layer on the next reconcile; the webhook stamps setting-correct labels and enriches
  descriptors (and `cpuDetail` when aware); `InstanceTypeFlavor` lists the setting-correct grouping.

### Non-Goals
- No change to the credit-based scoring, the accelerator three-view status math, the Instance/Pod
  admission runtime, or the accelerator soft-slicing runtime.
- No automatic reclamation of derived `InstanceType`s or their `ClusterQueue`s when the awareness
  setting flips: the operator authors derived types create-only and never garbage-collects them, so a
  flip strands the old-shaped derived types until an admin removes them (see Risks).
- No migration tooling for `ResourceFlavor`/`ClusterQueue`/`InstanceType` objects named under the old
  `gpustack-…` scheme; the reconcilers converge to the new `gpustack--…` names and orphans are
  reclaimed by the existing level-based deletion (CPU/device flavors) or left to the admin (derived
  types) — see Risks.
- No change to how the Device Manager labels nodes (the `acceleratable.…` device labels) or how the
  worker labels nodes (the `general.…` CPU labels); only the general key's CPU-name blending becomes
  unconditional.

## Proposal
The `ResourceFlavor` is the finest, setting-independent grain: it always encodes the CPU key, carries
an `acceleratable` boolean and CPU-detail note, and pins its nodes by feature key + `.count`. A new
`instance-type-aware-cpu-manufacturer` setting decides whether the aggregation layer
(`ClusterQueue`/`InstanceType`/`InstanceTypeFlavor`) treats the CPU manufacturer as a discriminator.
The `InstanceType` spec gains `GeneralGroup` and renames `Group` to `AcceleratorGroup`; its defaulting
webhook derives setting-correct labels and enriches descriptors (and, when aware, `cpuDetail`).

### User Stories
#### Story 1
As a platform admin, I want each node's general(CPU) key to reflect its real CPU model, so that CPU
`ResourceFlavor`s and the flavor catalog distinguish genuinely different CPUs instead of lumping every
node under one `generic` key.
#### Story 2
As a cluster operator, I want a single setting that decides whether scheduling is aware of the CPU
manufacturer — off means one generic pool and one pool per accelerator; on means every pool splits by
CPU — so that I opt into CPU-level placement only when my fleet needs it.
#### Story 3
As a platform admin, I want the raw CPU details recorded on the `ResourceFlavor` (always for CPU
pools, and on accelerated pools when awareness is on) and written back into the `InstanceType` when
awareness is on, so that I can audit exact CPU capability from `kubectl` without inspecting nodes.
#### Story 4
As a scheduler/operator, I want generic and accelerated pools to never share flavors even when they
share a CPU key, so that an aware generic queue's quota is never polluted by accelerated flavors of
the same CPU.

### Core Features & Acceptance Criteria

1. **Unconditional CPU-name general key.** Delete the `generalNodeKeyWithCPUName` package toggle and
   the `GPUSTACK_GENERAL_NODE_KEY_WITH_CPU_NAME` env read. `ExtractGeneralNodeKey` always blends the
   CPU identity: manufacturer from the NFD `cpu-model.vendor_id` (lowercased; `generic` when unknown),
   then the sanitized `feature.gpustack.ai/cpu-name` annotation, else the NFD `cpu-model.family`+`id`
   labels, else nothing. `ExtractNodeFlavors` always populates the CPU flavor's `Product`/`Family`
   from the `feature.gpustack.ai/cpu-*` annotations. *Accept:* a node reporting a CPU name yields a
   key like `amd-epyc-7763`; a node with no CPU info falls back to `generic`; no code path reads the
   deleted env var.

2. **Flavor shape — finest grain, setting-independent** (built by `NodeFlavorReconciler` /
   `ExtractNodeFlavors`). Names use the `gpustack--` prefix and `--` between the CPU and accelerator
   keys:
   - Non-accelerated: `gpustack--${gKey}-${os}-${arch}-${cpu}c`
     - labels: `kubernetes.io/os=${os}`, `kubernetes.io/arch=${arch}`,
       `feature.gpustack.ai/acceleratable=false`, `general.feature.gpustack.ai/${gKey}=true`,
       `general.feature.gpustack.ai/${gKey}.count=${count}`,
       `general.feature.gpustack.ai/${gKey}.capacity=${capacity}`
     - `spec.nodeLabels`: `kubernetes.io/os`, `kubernetes.io/arch`, `gpustack.ai/managed=true`,
       `general.feature.gpustack.ai/${gKey}=true`, `general.feature.gpustack.ai/${gKey}.count=${count}`
     - notes: `acceleratable=false`, `generalGroup=${gKey}`, `manufacturer`/`product`/`family` (CPU),
       and **`cpuDetail` always**.
   - Accelerated: `gpustack--${gKey}--${aKey}-${os}-${arch}-${accelerator}d`
     - labels: `kubernetes.io/os`, `kubernetes.io/arch`, `feature.gpustack.ai/acceleratable=true`,
       `general.feature.gpustack.ai/${gKey}=true`, `acceleratable.feature.gpustack.ai/${aKey}=true`,
       `acceleratable.feature.gpustack.ai/${aKey}.count=${count}`,
       `acceleratable.feature.gpustack.ai/${aKey}.capacity=${capacity}`
     - `spec.nodeLabels`: `kubernetes.io/os`, `kubernetes.io/arch`, `gpustack.ai/managed=true`,
       `general.feature.gpustack.ai/${gKey}=true`, `acceleratable.feature.gpustack.ai/${aKey}=true`,
       `acceleratable.feature.gpustack.ai/${aKey}.count=${count}`
     - notes: `acceleratable=true`, `generalGroup=${gKey}`, `acceleratorGroup=${aKey}`,
       `manufacturer`/`product`/`family` (device), `memory`, `cores`, `sliceable`, and **`cpuDetail`
       only when `instance-type-aware-cpu-manufacturer=true`**.

   *Accept:* names, labels, `nodeLabels`, and notes match the tables above; the accelerated flavor's
   `nodeLabels` pin both the CPU-key presence and the accelerator `.count`; the `feature.gpustack.ai/
   acceleratable` boolean is present on every flavor; `cpuDetail` presence follows the rules above.

3. **New setting `instance-type-aware-cpu-manufacturer` (editable bool, default `false`).** Read
   per-reconcile (never cached). It governs only the aggregation layer and the flavor `cpuDetail`
   gate; it never changes flavor names/labels/`nodeLabels`. *Accept:* toggling it reshapes the
   `ClusterQueue`/`InstanceType`/`InstanceTypeFlavor` grouping and the accelerated `cpuDetail` note on
   the next reconcile, and never rewrites a flavor's name.

4. **Aggregation layer — setting-driven grouping.** The derived `ClusterQueue`/`InstanceType` names,
   schedule labels, and `ResourceFlavor` selectors follow the setting. In all cases the selector
   carries the `feature.gpustack.ai/acceleratable` boolean as the generic-vs-accelerated separator.
   `${gKey}` variants pool per the setting; `${os}`/`${arch}` always split.

   - `instance-type-aware-cpu-manufacturer=false`:
     - Non-accelerated pool → `gpustack--generic-${os}-${arch}`; labels/selector
       `{kubernetes.io/os, kubernetes.io/arch, feature.gpustack.ai/acceleratable=false}`; aggregates
       every CPU flavor of that os/arch regardless of `gKey`.
     - Accelerated pool → `gpustack--${aKey}-${os}-${arch}`; labels/selector
       `{kubernetes.io/os, kubernetes.io/arch, feature.gpustack.ai/acceleratable=true,
       acceleratable.feature.gpustack.ai/${aKey}=true}`; aggregates every `gKey` variant of that
       accelerator.
   - `instance-type-aware-cpu-manufacturer=true`:
     - Non-accelerated pool → `gpustack--${gKey}-${os}-${arch}`; labels/selector
       `{kubernetes.io/os, kubernetes.io/arch, feature.gpustack.ai/acceleratable=false,
       general.feature.gpustack.ai/${gKey}=true}`.
     - Accelerated pool → `gpustack--${gKey}--${aKey}-${os}-${arch}`; labels/selector
       `{kubernetes.io/os, kubernetes.io/arch, feature.gpustack.ai/acceleratable=true,
       general.feature.gpustack.ai/${gKey}=true, acceleratable.feature.gpustack.ai/${aKey}=true}`.

   *Accept:* with the setting off there is one generic queue per os/arch and one queue per accelerator
   per os/arch; with it on the queues split by `gKey`; the aware generic queue never lists an
   accelerated flavor (the `acceleratable=false` guard excludes it); flipping the setting converges on
   the next reconcile.

5. **`InstanceType` API: rename `Group` → `AcceleratorGroup`, add `GeneralGroup`.**
   `AcceleratorGroup` is the accelerator key (`aKey`); `GeneralGroup` is the CPU key (`gKey`, or the
   literal `generic` for a collapsed non-accelerated pool). `Acceleratable`/`OS`/`Arch`/`UnitResources`/
   `LocalStorage` remain required and immutable-where-they-were. The validating webhook requires
   `AcceleratorGroup` when `Acceleratable=true` and `GeneralGroup` when awareness demands it (see
   Notes). Regenerate CRD/openapi/protobuf/applyconfiguration. *Accept:* an accelerated `InstanceType`
   requires `AcceleratorGroup`; the aggregated `v1` proxy and generated clients reflect the rename.

6. **Defaulting webhook — setting-correct labels + descriptor + `cpuDetail` enrichment.** From the
   spec identity (`GeneralGroup`, `AcceleratorGroup`, `Acceleratable`) and the awareness setting the
   webhook stamps the schedule labels of Feature 4 (pruning a stale feature key on a group change),
   plus the `queue-entrance` label. While descriptors are empty it lists one matching `ResourceFlavor`
   by those labels and fills `manufacturer`/`product`/`family` (and, when accelerated,
   `memory`/`cores`/`sliceable`). When `instance-type-aware-cpu-manufacturer=true` it additionally
   reads the flavor's `cpuDetail` note and writes it into `spec.CPU` (non-accelerated) or
   `spec.Accelerator.CPU` (accelerated); when awareness is off it never touches `cpuDetail`. *Accept:*
   labels/selectors match Feature 4 for both settings; descriptors fill from a matching flavor;
   `cpuDetail` is written back only when awareness is on.

7. **`NodeQueueReconciler` selector handles the `acceleratable` boolean.** `poolFlavorSelector` picks
   up `feature.gpustack.ai/acceleratable` (both values) and treats it as a sufficient discriminator so
   the collapsed generic queue (which carries no `general.` key) still resolves its flavors, and the
   aware generic queue excludes accelerated flavors. The quota math (`buildResourceGroups`,
   smallest-count-first, credits vs cpu, drain/empty/reactivate) is otherwise unchanged. *Accept:* a
   `gpustack--generic-${os}-${arch}` queue fills from every CPU flavor of that os/arch; an aware
   generic queue never fills from an accelerated flavor.

8. **`NodeFlavorReconciler` authors setting-correct derived `InstanceType`s.** After syncing a flavor,
   when `instance-type-derived-from-node=true`, it authors the pool's `InstanceType` create-only with
   the setting-correct name and `GeneralGroup`/`AcceleratorGroup`/`Acceleratable` (collapsed `generic`
   `GeneralGroup` for the non-accelerated pool when awareness is off). *Accept:* off → a
   `gpustack--generic-${os}-${arch}` and one `gpustack--${aKey}-${os}-${arch}` per accelerator are
   authored; on → `gpustack--${gKey}-…` and `gpustack--${gKey}--${aKey}-…`; authoring stays create-only.

9. **`InstanceTypeFlavor` catalog mirrors the setting.** The aggregated list-only resource replaces
   its single `Group` field with `GeneralGroup`+`AcceleratorGroup`, reads
   `instance-type-aware-cpu-manufacturer`, and groups/deduplicates the operator-owned `ResourceFlavor`s
   accordingly: off → one row per accelerator (`gpustack--${aKey}`) and one generic row
   (`gpustack--generic`); on → one row per `(gKey,aKey)` (`gpustack--${gKey}--${aKey}`) and one per
   `gKey` (`gpustack--${gKey}`). Names carry no os/arch. *Accept:* `kubectl get instancetypeflavors`
   lists the setting-correct rows; a generic row shows `acceleratable=false` with empty
   memory/cores/sliceable.

### Notes / Constraints / Caveats
- Go + controller-runtime; Kueue `sigs.k8s.io/kueue/apis/kueue/v1beta2` (v0.17.1). Empty
  `resourceGroups` remain allowed (confirmed in the prior declarative-management spec).
- `cpuDetail` reuses the `InstanceTypeCPU` / `InstanceTypeCPUCache` / `InstanceTypeAcceleratorCPU`
  structs still present in `api/worker/v1alpha1/instance_type.go` (they survived `2717d6b1`). It is
  serialized as a single JSON note value; the webhook deserializes it into `spec.CPU`
  (non-accelerated, an `InstanceTypeCPU`) or `spec.Accelerator.CPU` (accelerated, an
  `InstanceTypeAcceleratorCPU` carrying the CPU's own manufacturer/product/family + detail).
- **Validation is setting-independent; `GeneralGroup` is defaulted, not required.** The validating
  webhook reads no editable setting (admission must not depend on cluster state): it requires
  `AcceleratorGroup` only when `Acceleratable=true`, and never requires `GeneralGroup`. The mutating
  (defaulting) webhook defaults an empty `GeneralGroup` to the literal sentinel `generic`;
  `poolScheduleLabels` treats `generic` as "CPU-agnostic" and emits no `general.` label for it. So the
  collapse-vs-split behavior falls out of the setting without the admin ever having to set a field: with
  awareness off, `GeneralGroup` stays `generic` → collapsed pools; with awareness on, the admin (or the
  derived-authoring path) sets a real `gKey` → split pools, and a left-blank `GeneralGroup` degrades
  that one type to the collapsed shape.
- **`cpuDetail` source annotations.** The raw CPU detail is read from the node's NFD
  `feature.gpustack.ai/cpu-*` annotations — `cpu-name`, `cpu-family`, `cpu-physical-cores`,
  `cpu-threads-per-core`, `cpu-logical-cores`, `cpu-stepping`, `cpu-hz`, `cpu-boost-freq`,
  `cpu-cache-line`, `cpu-cache-l1i`/`l1d`/`l2`/`l3` — via the existing `generalFeatureAnnotation`
  accessor (a value leading with `@` is an unresolved NFD template and read as empty). The note is a
  single JSON value marshaled by `NodeFlavorReconciler` from the `workercore` CPU structs, so the
  webhook unmarshals it straight back into `spec.CPU` / `spec.Accelerator.CPU` (one typed source).
- **Setting access from the aggregated apiserver — confirmed.** `InstanceTypeFlavorHandler` reads the
  setting via `settings.InstanceTypeAwareCPUManufacturer.ShouldValueFromRemote(ctx) == "true"` — the
  same remote-read path `extensionapis/worker/instance.go` already uses (`ShouldValueFromRemote`); the
  reconcilers and webhooks, running in the worker process with the local settings indexer, use
  `ShouldValueBool(ctx)`.
- The `--` naming (`gpustack--${gKey}--${aKey}-…`) is a deliberate delimiter because `gKey`/`aKey`
  themselves contain single dashes (`amd-epyc-7763`, `nvidia-tesla-t4`); `parseNodeFlavorCount`'s
  last-dash scan still extracts the trailing `${count}{c|d}`.
- The `feature.gpustack.ai/acceleratable` boolean is the umbrella `NodeAcceleratableLabelKey`; on a
  flavor it is written explicitly `true`/`false` by `NodeFlavorReconciler` (nodes only ever carry the
  `=true` form when accelerated), so the generic selector can match "not accelerated".

### Boundaries
- **Always:** keep flavors setting-independent (the setting only reshapes aggregation + the accelerated
  `cpuDetail` gate); keep reconcilers idempotent and level-based; read editable settings per-reconcile;
  carry the `feature.gpustack.ai/acceleratable` boolean in every pool selector; regenerate API/CRD/
  openapi/protobuf/applyconfiguration via `make generate` after type/marker/webhook edits; sign every
  commit (`-s`); keep the Pod webhook's per-card VRAM lookup (`InstanceType.spec.memory`) working.
- **Ask first:** any change to the credit/CPU quota math, the accelerator three-view status, the
  Instance/Pod admission paths, or the `.count`-pinned flavor sizing; the final `InstanceType`
  required/optional field matrix; renaming beyond `Group→AcceleratorGroup` / adding `GeneralGroup`.
- **Never:** let an aware generic queue match an accelerated flavor; rewrite a flavor's name because
  the awareness setting flipped; auto-delete a derived `InstanceType`/`ClusterQueue` for lack of
  flavors or on a setting flip; drive a `ClusterQueue`'s reservation counters negative.

### Risks and Mitigations
- **Setting flip strands old-shaped derived types.** Flipping `instance-type-aware-cpu-manufacturer`
  leaves the previously-authored derived `InstanceType`s/queues (e.g. `gpustack--generic-…` after
  turning awareness on) present but empty, since derived types are create-only and never
  reclaimed. → *Mitigation:* document it; the admin deletes the stale types; the drain/empty path keeps
  their queues from corrupting Kueue counters. Consider a follow-up reclamation only if it proves
  painful.
- **Name-scheme change (`gpustack-…` → `gpustack--…`) + `Group` rename orphans existing objects on
  upgrade.** → CPU/device flavors are reclaimed by the existing "no contributing node → delete" branch
  as the new-named flavors appear; derived `InstanceType`s/queues under old names linger (create-only),
  same as the setting-flip case. → *Mitigation (Task 8):* ship a migration guide + orphan-cleanup script
  (modeled on `v0.5-to-v0.6.md` / `cleanup-v0.5-orphans.sh`), validated on the live cluster.
- **Aware generic queue could match accelerated flavors of the same CPU** (both carry
  `general.${gKey}=true`). → *Resolved by design:* every selector carries the
  `feature.gpustack.ai/acceleratable` boolean, so the generic pool matches only `=false` flavors — this
  is the reason the boolean label is added.
- **Collapsed generic pool descriptors are arbitrary** (many `gKey`s, webhook enriches from the first
  matching flavor). → Accepted: the generic pool is intentionally CPU-agnostic; descriptors are
  best-effort and `cpuDetail` is not written when awareness is off. Documented.
- **Flavor-count blow-up.** Accelerated flavors now multiply by `gKey`, so an accelerator's queue (aware
  off) can carry many flavors. → `buildResourceGroups` already chunks ≤16 flavors/group; note Kueue's
  ≤256 flavors/queue ceiling and log if a pool would exceed it.
- **`cpuDetail` JSON note size.** The detail struct is small (cores/cache/clock); a single JSON note is
  well within annotation limits. → No mitigation needed; keep the struct lean.
- **`Group → AcceleratorGroup` is a breaking API change.** An existing `InstanceType`/`InstanceTypeFlavor`
  YAML using `spec.group` no longer round-trips (the stored field is renamed), so an admin-authored type
  loses that value on the first apply/read after upgrade. → *Accepted, matching the project's stated "no
  migration tooling for pre-change InstanceTypes" stance:* document the rename in the release notes;
  admins re-author (or re-apply with the new field); derived types are recreated with the new names by
  `NodeFlavorReconciler`.

## Design Details
### Commands
- Build: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go build -tags "goccy netgo" ./...`
- Test: `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -tags "goccy netgo" ./pkg/... ./api/...`
- Lint: `make lint` (also runs via the `gpustack-operator-lint` hook after Go edits).
- Generate: `make generate` (after editing API types, markers, or webhooks — use the
  `gpustack-operator-generate` skill).
- E2E: the `gpustack-operator-e2e` skill (build+load dev image, Helm-deploy, assert the chain).

### Project Structure
- `pkg/nodefeature/helper.go` — delete the `generalNodeKeyWithCPUName` toggle; always blend the CPU
  key; add `GeneralKey` to `NodeFlavor`; embed `gKey` in accelerated flavor names + `nodeLabels`; add
  the `feature.gpustack.ai/acceleratable` boolean intent; re-introduce a `CPUDetail` extractor
  (`extractGeneralDetail`) + its JSON shape for the `cpuDetail` note.
- `pkg/nodefeature/helper_test.go` — table cases for the always-blended key, the new names/labels,
  `GeneralKey`, and `cpuDetail`.
- `pkg/worker/controllers/worker/nodeflavor.go` — `acceleratable` boolean + `generalGroup`/
  `acceleratorGroup`/`cpuDetail` notes (accelerated `cpuDetail` gated by the setting); setting-correct
  derived-`InstanceType` name + `GeneralGroup`/`AcceleratorGroup`.
- `pkg/worker/controllers/worker/nodequeue.go` — `poolFlavorSelector` handles the `acceleratable`
  boolean as a sufficient discriminator.
- `pkg/worker/controllers/worker/instancetype.go` — `instanceTypeScheduleLabels` becomes
  setting-aware (+ the `acceleratable` boolean); `GeneralGroup`/`AcceleratorGroup` usage.
- `pkg/worker/webhooks/worker/instancetype.go` — setting-correct label derivation; `cpuDetail`
  write-back when aware; validation for the renamed/added fields.
- `api/worker/v1alpha1/instance_type.go` + `api/worker/v1/instance_type.go` — rename `Group` →
  `AcceleratorGroup`, add `GeneralGroup`; regenerate.
- `api/worker/v1/instance_type_flavor.go` + `pkg/worker/extensionapis/worker/instance_type_flavor.go`
  — `GeneralGroup`+`AcceleratorGroup` on the catalog; setting-aware grouping.
- `pkg/worker/settings/value.go` — add `InstanceTypeAwareCPUManufacturer`.
- `docs/architecture.md`, `docs/settings.md`, `docs/development.md`, `README.md`, e2e cases — updated.

### Code Style
Editable setting, read per-reconcile (matches `value.go`):
```go
// InstanceTypeAwareCPUManufacturer governs whether the derived ClusterQueue/InstanceType/
// InstanceTypeFlavor aggregation layer treats the CPU manufacturer as a discriminator. When false
// (default) non-accelerated flavors collapse into one generic pool per os/arch and accelerated
// flavors pool per accelerator; when true every pool splits by the CPU key. It never changes a
// ResourceFlavor's name/labels — flavors are always the finest grain.
InstanceTypeAwareCPUManufacturer = settings.NewEditable(
    "instance-type-aware-cpu-manufacturer",
    "Indicates whether the derived ClusterQueue/InstanceType/InstanceTypeFlavor split by CPU "+
        "manufacturer. When false (default), non-accelerated flavors collapse into one generic pool "+
        "per os/arch and accelerated flavors pool per accelerator; when true, every pool splits by "+
        "the CPU manufacturer.",
    setting.InitializeFromEnv("false"),
    setting.AllowBool(),
)
```
Setting-aware schedule-label/selector derivation (the single source both the webhook and the
reconciler consult), sketch:
```go
// poolScheduleLabels builds the schedule discriminators for a pool from its identity and the
// awareness setting. The feature.gpustack.ai/acceleratable boolean always separates generic from
// accelerated; the CPU key participates only when aware.
func poolScheduleLabels(acceleratable, aware bool, generalGroup, acceleratorGroup, os, arch string) map[string]string {
    lbs := map[string]string{core.LabelOSStable: os, core.LabelArchStable: arch}
    lbs[nodefeature.NodeAcceleratableLabelKey] = strconv.FormatBool(acceleratable)
    if acceleratable {
        lbs[nodefeature.AcceleratableFeatureLabelPrefix+acceleratorGroup] = "true"
    }
    if aware && generalGroup != "" && generalGroup != generalGroupGeneric {
        lbs[nodefeature.GeneralFeatureLabelPrefix+generalGroup] = "true"
    }
    return lbs
}
```
Conventions: typed errors early; `ctrlclix.WithoutQuorum` cached reads; `kubemeta.DeepEqual`-guarded
writes; `systemmeta` notes for operator ownership; exported symbols documented with behavior.

### Implementation Plan
Vertically sliced and ordered so every commit leaves the operator building, testing green, and the
scheduling chain functional. Each task is TDD (RED → GREEN → suite → `make lint`), committed with `-s`.
The `ResourceFlavor` is the finest, setting-independent grain (Task 1); the aggregation layer becomes
CPU-aware on top of it (Task 2); descriptors, catalog, docs, and e2e follow.

- [x] **Task 1 — Setting + `nodefeature` foundation + `ResourceFlavor` final contract.** (No API dependency.)
  - `pkg/worker/settings/value.go`: add `InstanceTypeAwareCPUManufacturer` (editable bool, default
    `false`), read per-reconcile.
  - `pkg/nodefeature/helper.go`: delete the `generalNodeKeyWithCPUName` var, its `init`, and the
    `GPUSTACK_GENERAL_NODE_KEY_WITH_CPU_NAME` env read; `ExtractGeneralNodeKey` always blends the CPU
    identity (the unexported toggle-taking helper is folded away). **`NodeFlavor` mirrors the
    `InstanceType`: the polymorphic `Key` is dropped in favour of `GeneralKey` (always set,
    ↔ `GeneralGroup`) + `AcceleratorKey` (device only, ↔ `AcceleratorGroup`), with an `OwnKey()`
    helper (accelerator key when accelerated, else general key) for the flavor's own feature-key
    label.** Rename via `formatNodeFlavorName` to `gpustack--${gKey}-${os}-${arch}-${count}c` (CPU) and
    `gpustack--${gKey}--${aKey}-${os}-${arch}-${count}d` (device); the device flavor's `NodeLabels`
    gains `general.${gKey}=true`. Always populate the CPU flavor's `Product`/`Family` from the
    `feature.gpustack.ai/cpu-*` annotations. Add `CPUDetail` + `ExtractGeneralDetail` reading the
    annotations listed in Notes.
  - `pkg/worker/controllers/worker/nodeflavor.go`: stamp `feature.gpustack.ai/acceleratable=true|false`
    on the RF metadata labels; add `general.${gKey}=true` to an accelerated RF's labels; add the
    `generalGroup=${gKey}` + `acceleratorGroup=${aKey}` notes and write `cpuDetail` via `cpuDetailNote`
    (marshaled from the `workercore` CPU structs — the single typed source) — always for a CPU flavor,
    gated by `InstanceTypeAwareCPUManufacturer` for an accelerated one. The `group` note is kept
    transitional (the `InstanceTypeFlavor` still reads it; dropped in Task 4);
    `authorDerivedInstanceType` is left on the current `Group` field (Task 2 rewrites it).
  - `pkg/devicemanager/detector/detector.go`: `acceleratableDevicesSelectorLabels` drops the `.count`
    sizing pin from the Devices selector labels (it is not a selector key) — this both accommodates the
    new paired-CPU-key label and repairs a pre-existing failing test on `main` (the declarative-management
    `.count` addition leaked into the selector). *(Not in the original plan's file list — found while
    grounding Task 1.)*
  - `pkg/nodefeature/helper_test.go`, `pkg/worker/controllers/worker/nodeflavor_test.go`,
    `pkg/devicemanager/detector/detector_test.go`: update names, labels, `nodeLabels`, notes; add the
    always-blended-key + fallback cases, `GeneralKey`/`AcceleratorKey`, and `cpuDetail`-gate cases.
  - *Acceptance:* every node's general key reflects its CPU (fallback `generic` only when no CPU info);
    RF names/labels/`nodeLabels`/notes match Features 1–2; the `acceleratable` boolean is on every RF;
    `cpuDetail` presence follows the gate.
  - *Verify:* `go test ./pkg/nodefeature/... ./pkg/worker/controllers/worker/... -run 'NodeFlavor|Extract|ConstructNodeCapacity'`; build; `make lint`. **Checkpoint: full build + suite. — Done: `./...` builds, `./pkg/... ./api/...` green, `make lint` 0 issues.**

- [x] **Task 2 — API rename + CPU-aware aggregation layer.** (Atomic — the rename must compile as a unit.)
  - `api/worker/v1alpha1/instance_type.go` (the aggregated `v1` type is a Go type alias of it, so no
    separate edit): rename `Group` → `AcceleratorGroup`, add `GeneralGroup` **at the end (protobuf
    field 12) to keep the source order matching the protobuf field numbers**; `make generate`
    (deepcopy, register, CRD, conversion, protobuf, openapi, applyconfiguration).
  - Add the shared `nodefeature.PoolScheduleLabels(acceleratable, aware, generalGroup, acceleratorGroup,
    os, arch)`: always carries the `feature.gpustack.ai/acceleratable` boolean; adds the `general.` key
    only when `aware && generalGroup != GeneralGroupGeneric`; adds the `acceleratable.` key when
    accelerated. **Its inverse `nodefeature.PoolFlavorSelector(labels)` and the pool-naming
    `NodeFlavor.DerivedInstanceTypeIdentity(aware)` live in `nodefeature` too — the label algebra and
    pool identity are consolidated in one package** (moved out of `nodequeue.go`/`nodeflavor.go`).
  - `pkg/worker/webhooks/worker/instancetype.go`: `Default` defaults an empty `GeneralGroup` to
    `generic`, then stamps labels via `poolScheduleLabels` (pruning a stale feature key) + the entrance
    label; enrichment selector uses the same labels. `ValidateCreate`/`ValidateUpdate` require
    `AcceleratorGroup` iff `Acceleratable` and read no setting (setting-independent).
  - `pkg/worker/controllers/worker/instancetype.go`: `instanceTypeScheduleLabels` derives via
    `poolScheduleLabels` (setting read per-reconcile).
  - `pkg/worker/controllers/worker/nodeflavor.go`: `authorDerivedInstanceType` picks the setting-correct
    name + `GeneralGroup`/`AcceleratorGroup` (off: `gpustack--generic-${os}-${arch}` /
    `gpustack--${aKey}-${os}-${arch}`; on: `gpustack--${gKey}-${os}-${arch}` /
    `gpustack--${gKey}--${aKey}-${os}-${arch}`), create-only.
  - `pkg/worker/controllers/worker/nodequeue.go`: calls `nodefeature.PoolFlavorSelector` (which now
    honours the `feature.gpustack.ai/acceleratable` boolean); `enqueueNodeQueueWhenResourceFlavorChanged`
    lists the operator queues and keeps those whose selector is a subset of the (finest-grain) flavor's
    discriminators — a `MatchingLabels` query keyed on the flavor's labels would miss a collapsed queue
    that carries fewer labels.
  - Update `instancetype_test.go`, `nodequeue_test.go`, `nodeflavor_test.go`, webhook tests for the
    rename + setting-aware behavior; add `_GenericCollapsedFillsFromAllCPUFlavors` and
    `_AwareGenericExcludesAcceleratedFlavor`. Also repaired the `workergateway/service` spec fixtures for
    the rename.
  - *Acceptance:* Features 3–8 — the queues/types match the tables for both settings; an aware generic
    queue never lists an accelerated flavor; derived types are named per the setting; a flip converges
    on the next reconcile without rewriting a flavor.
  - *Verify:* `go test ./pkg/worker/... -run 'InstanceType|NodeQueue|NodeFlavor|Pod'`; `make generate`
    clean; build; `make lint`. **Checkpoint: full build + suite. — Done: `./...` builds, `make generate`
    idempotent, `./pkg/... ./api/...` green, `make lint` 0 issues.**

- [x] **Task 3 — `cpuDetail` write-back in the defaulting webhook (awareness-gated).**
  - `pkg/worker/controllers/worker/nodeflavor.go`: `cpuDetailNote(detail, acceleratable)` stores the
    **type-specific shape** — a plain `InstanceTypeCPU` for a non-accelerated flavor (its CPU
    manufacturer/product/family are the InstanceType's top-level descriptors) and an
    `InstanceTypeAcceleratorCPU` for an accelerated one (carrying the CPU's own
    manufacturer/product/family, distinct from the device's). *(Folded into Task 1's commit.)*
  - `pkg/worker/webhooks/worker/instancetype.go`: `foldCPUDetail(it, raw)` unmarshals the matched
    flavor's `cpuDetail` note into the **matching** target — `spec.Accelerator.CPU`
    (`InstanceTypeAcceleratorCPU`) when `it.Spec.Acceleratable`, else the embedded `spec.CPU`
    (`InstanceTypeCPU`); `Default` calls it inside the enrich-once block **only when
    `InstanceTypeAwareCPUManufacturer` is true**, so awareness off never touches the CPU spec.
  - The `cpuDetail` note is a nice-to-have: both sides use `pkg/utils/json`'s error-ignoring
    `ShouldMarshal`/`ShouldUnmarshal`, so neither `cpuDetailNote` nor `foldCPUDetail` returns an error.
  - *Acceptance:* awareness on folds `cpuDetail` into the acceleratable-correct spec field; awareness
    off leaves it; a non-accelerated type never keeps a stale accelerator `CPU`.
  - *Verify:* webhook unit tests (`_FoldCPUDetail` round-trips accel→`spec.Accelerator.CPU` /
    non-accel→`spec.CPU` / empty; `_DefaultSkipsCPUDetailWhenUnaware` pins the off gate — the on branch
    can't be unit-tested since the editable setting caches globally in the shared test binary, so it is
    an e2e case); build; `make lint`. **— Done: webhook tests pass, `./...` builds, `make lint` 0 issues.**

- [x] **Task 4 — `InstanceTypeFlavor` catalog: fields + setting-aware grouping.**
  - `api/worker/v1/instance_type_flavor.go`: replace `Group` with `AcceleratorGroup` (proto 1) +
    `GeneralGroup` (proto 9, last — proto-linear); `make generate`.
  - `pkg/worker/extensionapis/worker/instance_type_flavor.go`: `OnList` reads the setting via
    `ShouldValueFromRemote(ctx) == "true"`; `instanceTypeFlavorSpec` builds the group identity per the
    setting (off → per-`aKey` accelerated rows + one `generic` row; on → per-`(gKey,aKey)` + per-`gKey`),
    dropping the CPU-specific descriptors for the collapsed generic row so it dedups to one;
    `instanceTypeFlavorName` names it (`gpustack--…`, no os/arch); the sort gains a group tiebreak; the
    table gains `GeneralGroup`/`AcceleratorGroup` columns.
  - Migration completed: `NodeFlavorReconciler` drops the transitional `group` note (only the catalog
    read it), and the `nodeflavor_test` `group`-note assertions are removed.
  - *Acceptance:* Feature 9 — `kubectl get instancetypeflavors` lists the setting-correct rows; a
    generic row shows `acceleratable=false` with empty memory/cores/sliceable.
  - *Verify:* aggregation unit test (collapse across CPUs → one row per accelerator + one generic; the
    aware=true split is an e2e case since the editable setting caches globally in the shared test
    binary; the test configures a fake loopback kube client so the remote read falls back to the
    "false" default); `make generate` clean; build; `make lint`. **— Done: `./...` builds, `make generate`
    idempotent, `./pkg/... ./api/...` green, `make lint` 0 issues.**

- [x] **Task 5 — Docs.** `docs/architecture.md` (unconditional CPU key; the finest-grain flavor + the
  `feature.gpustack.ai/acceleratable` discriminator + `cpuDetail`; the awareness setting's
  collapse/split naming table; the reconciler/webhook bullets; the rewritten worked example showing
  RF-per-CPU vs collapsed CQ/IT), `docs/settings.md` (new `instance-type-aware-cpu-manufacturer` row;
  removed the deleted `GPUSTACK_GENERAL_NODE_KEY_WITH_CPU_NAME` env var; "last five" per-reconcile),
  `README.md` (one discovery-bullet clause). `docs/development.md` needed no change (its
  `TestExtractGeneralNodeKey` reference is now correct); the historical `docs/migration/v0.5-to-v0.6.md`
  is left as-is. *Verify:* the `settings.md#online-adjustable-settings` anchor resolves; wording matches
  shipped behavior. **— Done.**

- [x] **Task 6 — E2E cases.** Via the `gpustack-operator-e2e` skill: added `case-18` — the CPU
  `ResourceFlavor` is the finest double-dash grain (`gpustack--${gKey}…-${count}c`) and always carries
  the `cpuDetail` note; toggling `instance-type-aware-cpu-manufacturer` reshapes the `InstanceTypeFlavor`
  catalog (collapse to one `gpustack--generic` row ↔ split to a per-CPU `gpustack--${gKey}` row) without
  rewriting flavors, and its trap restores the setting + removes any derived type the aware window
  created. Also reconciled the existing cases the rename/collapse broke (found while building):
  case-4/6/12 to the double-dash accelerated derived name; case-2/3 to locate the CPU flavor by its
  `-${count}c` suffix (it is no longer named after the collapsed `InstanceType`); case-16/17 to
  `generalGroup`/`acceleratorGroup` (case-17's admission contract now: an accelerated type missing
  `acceleratorGroup` is rejected, `generalGroup` is defaulted not required, and the awareness-off stamp
  is the `acceleratable` boolean + os/arch, not a CPU key). Added `case-19` (awareness on → the
  accelerated type splits by CPU carrying the real GPU descriptors + folded `spec.cpu`, and a real GPU
  Instance runs on it) and `case-20` (sibling InstanceTypes on one pool stay status-consistent —
  `enqueueInstanceTypeWhenDevicesChanged`), both real-hardware + auto-skipping. Updated `SKILL.md` (case
  rows + locked-title prose + the baseline-restore case contract) and `references/` — a new
  `packaged-image-deploy.md` (the `make package` image-ref ↔ chart-values contract + the remote-builder
  mirror procedure) and a `troubleshooting.md` accelerator-detection guard. *Verify:* `bash -n` each
  case (all 20 green); `chmod +x` case-18/19/20. **— Done.**

- [ ] **Task 7 — Package + live-cluster verify (user-driven).** Package the dev image on the amd64
  builder (`PACKAGE_ARCH=amd64 PACKAGE_NAMESPACE=thxcode PACKAGE_PUSH=true make package`, published to
  Docker Hub; the builder host + upload path are supplied at run time, kept out of tracked files),
  Helm-deploy to a reachable Kubernetes cluster with the packaged image tag, and run the e2e
  verifications. *Verify:* the e2e case suite passes on the cluster.

- [ ] **Task 8 — Migration guide + orphan-cleanup for the `gpustack--` rename.** This release renames
  every materialized scheduling object again — `ResourceFlavor`/`ClusterQueue`/`InstanceType` from the
  single-dash `gpustack-${key}-…` back to the double-dash `gpustack--${gKey}[--${aKey}]-…`, and the
  `InstanceType` spec `Group` → `AcceleratorGroup` + `GeneralGroup` — so a plain `helm upgrade` leaves
  the old-named objects as orphans (same failure mode as v0.5→v0.6). Add `docs/migration/<from>-to-<to>.md`
  (modeled on `v0.5-to-v0.6.md`: what changes, why a plain upgrade orphans, uninstall-reinstall vs
  in-place + cleanup) and a `docs/migration/cleanup-<old>-orphans.sh` (modeled on
  `cleanup-v0.5-orphans.sh`) that deletes the old single-dash operator-owned RFs/CQs/derived ITs/LQs.
  *Verify:* validate on the live cluster (Task 7 image) — upgrade an old-named deployment, run the
  cleanup, confirm zero old-named residue and a healthy new-named chain; `bash -n` the script.

### Test Plan
[ ] I/we understand the owners of the involved components may require updates to existing tests to make
this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates
Rework the existing `nodefeature`, `nodeflavor`, `nodequeue`, `instancetype`, webhook, and
`InstanceTypeFlavor` tests for: the always-blended general key (no toggle); the new `gpustack--…` flavor
names (CPU + device-with-`gKey`); the `feature.gpustack.ai/acceleratable` boolean label; the
`generalGroup`/`acceleratorGroup`/`cpuDetail` notes; the `Group → AcceleratorGroup` rename + new
`GeneralGroup`; and the setting-aware aggregation (collapse vs split) and its label/selector derivation.

#### Unit tests
Every added unit has table-driven coverage. Per-package targets (`2026-07-08`):
- `pkg/nodefeature`: `2026-07-08` - ~85% (always-blended key incl. NFD-annotation / cpu-model / vendor /
  `generic` fallbacks; `GeneralKey`; `gpustack--…` names; device `nodeLabels` include the general key;
  `extractGeneralDetail` reads every `cpu-*` annotation).
- `pkg/worker/controllers/worker`: `2026-07-08` - ~80% (`nodeflavor` `acceleratable` boolean +
  `generalGroup`/`acceleratorGroup`/`cpuDetail`-gated notes + setting-correct derived authoring;
  `nodequeue` selector boolean, generic-collapsed fill, aware-generic excludes accelerated;
  `instancetype` setting-aware schedule labels).
- `pkg/worker/webhooks/worker`: `2026-07-08` - ~85% (`Default` setting-aware labels for both settings ×
  accelerated/non-accelerated + `GeneralGroup=generic` default + stale-key prune; `cpuDetail`
  write-back gated by awareness; `ValidateCreate/Update` require `AcceleratorGroup` iff accelerated,
  setting-independent).
- `pkg/worker/extensionapis/worker`: `2026-07-08` - ~80% (`InstanceTypeFlavor` setting-aware grouping:
  off → per-accelerator + one generic; on → per-`(gKey,aKey)` + per-`gKey`; dedup; sort; generic row
  `acceleratable=false`).

#### Integration tests
Fake-client controller/webhook tests (names finalized in the implementation PR):
- `TestNodeFlavorReconciler_StampsAcceleratableBoolean`, `_NotesGeneralAndAcceleratorGroup`,
  `_CpuDetailNoteGatedByAwareness`, `_AuthorsDerivedInstanceTypeCollapsed` (aware off),
  `_AuthorsDerivedInstanceTypeSplit` (aware on).
- `TestNodeQueueReconciler_PoolSelectorAcceptsAcceleratableBoolean`,
  `_GenericCollapsedFillsFromAllCPUFlavors`, `_AwareGenericExcludesAcceleratedFlavor`.
- `TestInstanceTypeReconciler_ScheduleLabelsCollapsed`, `_ScheduleLabelsSplit`.
- `TestInstanceTypeWebhook_DefaultLabelsCollapsed`, `_DefaultLabelsSplit`, `_DefaultsGeneralGroupToGeneric`,
  `_PrunesStaleFeatureKey`, `_CpuDetailWriteBackGated`, `_ValidateAcceleratorGroupRequiredWhenAccelerated`.
- `TestInstanceTypeFlavorHandler_GroupsCollapsed`, `_GroupsSplit`, `_DeduplicatesAndSorts`.
- Regression: the Pod webhook still folds a sliced memory-mib request from `InstanceType.spec.memory`.

#### e2e tests
Via the `gpustack-operator-e2e` skill on a reachable cluster: flavor naming (`gpustack--${gKey}…`);
toggling `instance-type-aware-cpu-manufacturer` reshapes the queues/types (collapse ↔ split) without
rewriting flavors; `cpuDetail` surfaced on the flavor/InstanceType when awareness is on;
`InstanceTypeFlavor` list per setting. Empty `resourceGroups` allowance is already confirmed at the Kueue
source level (prior spec), so no separate e2e gate is needed.

## Alternatives
- **Bake the awareness setting into the flavor grain (rewrite flavor names on flip).** Rejected:
  flipping the setting would churn every `ResourceFlavor`, orphaning quota mid-flight; keeping flavors
  finest-grain and setting-independent means a flip only re-groups the aggregation layer.
- **Reuse the old `generalNodeKeyWithCPUName` toggle to control awareness.** Rejected: that toggle was
  a startup env read gating the node-key shape; the requirement is a runtime, aggregation-layer switch
  with the flavors always CPU-accurate — a different axis entirely.
- **Keep the aware generic selector as `general.${gKey}=true` alone (per the source doc).** Rejected: it
  matches accelerated flavors of the same CPU (they carry `general.${gKey}=true` too) and pollutes the
  generic queue's quota; the `acceleratable=false` guard is required.
- **Store `cpuDetail` as individual notes instead of one JSON blob.** Rejected: the detail is a nested
  struct (cache tiers, clock speeds); one JSON note round-trips cleanly into the existing
  `InstanceTypeCPU`/`InstanceTypeAcceleratorCPU` structs.

## Open Questions
Design decisions — **confirmed by the author**:
- **Field naming** — `AcceleratorGroup` (rename of `Group`) + `GeneralGroup` (new), for consistency
  with the existing `general.`/`acceleratable.` label vocabulary.
- **`aware=false` semantics** — the collapse model: non-accelerated → all CPUs in one `generic` pool
  per os/arch; accelerated → one pool per accelerator (CPU ignored).
- **Selector isolation** — every pool selector carries the `feature.gpustack.ai/acceleratable`
  boolean as the generic-vs-accelerated separator.
- **Collapsed generic identity** — the literal `generic` for the non-accelerated `GeneralGroup`/name
  when awareness is off.
- **`InstanceTypeFlavor` name** — omits os/arch (os/arch-agnostic catalog).

Resolved during `/my-plan` (see Notes):
- **Validation vs. awareness** — the validating webhook is setting-independent: it requires
  `AcceleratorGroup` only when `Acceleratable=true` and never requires `GeneralGroup`; the mutating
  webhook defaults an empty `GeneralGroup` to the `generic` sentinel, so collapse-vs-split falls out of
  the setting with no required field.
- **Setting access from the aggregated apiserver** — confirmed: `InstanceTypeFlavorHandler` reads it via
  `ShouldValueFromRemote(ctx) == "true"` (the remote path `instance.go` already uses); reconcilers/webhooks
  use `ShouldValueBool(ctx)`.

None remaining.
