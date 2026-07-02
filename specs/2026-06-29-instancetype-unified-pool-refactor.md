# Spec: Instance Type Unified-Pool Refactor — Queue-Managed Unit Specs, Four-Gate Admission, Devices-CR Ledger

Status: Building
Type: Feature

## Summary
Today an InstanceType's "unit spec" (RAM-per-CPU for general nodes; CPU/RAM-per-device for accelerators) is
baked into per-node `${NODE}-gpustack-worker` NodeFeature labels (the `.z-*` family), so changing a spec means
editing one or more **nodes**; and the three allocation modes are split across **two** ClusterQueues
(exclusive+shared vs sliced) joined by a Cohort, so users must know "sliced = type A, exclusive = type B" and
the two pools never sync cleanly. This refactor moves unit specs off the node and into **Queue/InstanceType**
management, and folds all three modes into **one ClusterQueue** (one `credits` pool, **Cohort removed**).
Resource governance becomes a **four-gate** chain with the **`Devices` CR `AcceleratorAllocation`** as the single
authoritative per-card ledger: (1) Kueue `credits` does a coarse one-shot fractional fold-down for total-memory
admission — it is **no longer the ledger**; (2) the default scheduler / kubelet count node-level
`.sliced`/`.sliced.units`/`.sliced.cores-percentage`/`.sliced.memory-*`; (3) a new Kueue **AdmissionCheck** reads
the `Devices` CR to reject requests that pass credits but cannot be placed per-card (e.g. 8 cards each already
50%-sliced, where credits still thinks 4 whole cards are free); (4) the `Devices` CR is the authoritative ledger
that both drives the new **three-view** InstanceType display (allocatable-as-exclusive / shareable / sliceable)
and feeds the AdmissionCheck. The credit base migrates `12800 → M = 1,600,000` (= `2⁹×5⁵`). Kueue's remaining
strategic role is future batch-deployment management (queueing / quota / preemption / gang), not accounting.

This spec builds on three shipped specs — `2026-06-21-accelerator-resource-modes-refactor`,
`2026-06-24-unified-credit-base-scoring` (which established `B = D = 12800`), and
`2026-06-25-accelerator-soft-slicing-runtime-isolation` — and is grounded in the research report
`.claude/reports/gpustack-instancetype-refactor.md` (Codex cross-model reviewed).

## Motivation
### Goals
- **Unit specs are Queue-managed, not node-managed.** Changing an InstanceType's unit spec must be a single
  Queue/InstanceType-level action, never an edit to one or more nodes' NodeFeature labels. Nodes carry only
  Device basics (`acceleratable.*` model / VRAM / cores / count).
- **One pool, three modes.** Exclusive / shared / sliced resolve to a **single ClusterQueue** with one `credits`
  resource (folded via Kueue `resources.transformations`). **Cohort is removed** — with all three modes in one
  queue there is no cross-queue borrowing to broker, so `CohortReconciler`, `spec.cohortName`, and the `z-cohort`
  label are deleted. Users see one InstanceType, not "type A vs type B".
- **`Devices` CR is the authoritative ledger.** Per-card occupancy (`AcceleratorAllocation`: `Allocated`/
  `Remaining` by `Mode`) is the single source of truth; it drives the three-view display **and** the
  AdmissionCheck. Kueue `credits` is a coarse first gate, **not** the ledger.
- **Four-gate admission** (see Proposal). The new **AdmissionCheck** closes the credits "over-admit exclusive"
  gap that scalar quota cannot express.
- **Credit-base migration `12800 → M = 1,600,000`** (`= 2⁹×5⁵ = 12800×5³`): preserves the `2⁹` factor (so
  `SlicedResourceMaxSize = 512` still divides) and adds `5³` (so the memory-1% granularity `M/100 = 16000` is
  integral); `1/M = 625 nano` stays clean. Transformation factors become `1,600,000 / 160,000 / 1` (+ sliced
  `multiplyBy .sliced`).
- **Decouple RF/CQ reconcilers from Node.** Only `NodeFlavorReconciler` reads nodes; it pre-computes
  `capacity = indexedNodes × count` into RF labels `schedule.gpustack.ai/{key,os,arch,count,capacity}`.
  `NodeQueueReconciler` reads only RF labels (no Node list/watch).
- **Three `pkg/worker/settings` switches** (env- and UI-configurable, **dynamically adjustable at runtime — no
  restart**; consumers read per-reconcile via `ShouldValueBool(ctx)`, never a package-level `osx.Getenv` cache):
  `node-management-manual` (default **false**; true → don't auto-manage nodes); `instance-type-mixed-on-node`
  (default **true** = current behavior; false → a GPU node is GPU-only, a CPU-only node CPU-only);
  `instance-type-derived-from-node` (default **true** = current behavior; false → don't auto-derive the
  InstanceType/ClusterQueue from nodes — the admin creates it via the InstanceType API).
- **Success criteria (testable):**
  1. The five-step pooling sequence below yields the exact three-view progression
     `8/80/800 → 6/60/600 → 4/58/400 → 2/38/360 → 2/38/356 → 1/28/256` on an 8× A10G (24Gi) node.
  2. With 8 cards each 50%-sliced, a request for 5 exclusive cards is **rejected/held by the AdmissionCheck**
     (not admitted-then-unschedulable).
  3. Changing a unit spec touches no `Node`/NodeFeature object.
  4. No `Cohort` objects exist in the cluster after reconcile.

### Non-Goals
- Forking or patching Kueue (`mirrored-kueue` stays as vendored).
- Changing the soft-slicing **runtime isolation** mechanism (HAMi-core `libvgpu.so` / vcann-rt injection) — that
  is the shipped `2026-06-25-accelerator-soft-slicing-runtime-isolation` spec; this refactor only changes which
  keys drive the SM/VRAM limits.
- Topology-Aware Scheduling / cross-node fragmentation (deferred; `Devices` CR + AdmissionCheck handle per-card).
- **All Physical/Virtual Partition (MIG / vNPU) work is a separate dedicated spec.** This spec touches only the
  **SoftPartition** path (`MaxPartitions = 512` fixed). The `MaxPartitions` value for PhysicalPartition /
  VirtualPartition (reading MIG/vNPU profile counts from the vendor lib) and their allocation path are out of
  scope here.
- Multi-cluster Worker Gateway logic beyond syncing the InstanceType JSON contract.

## Proposal
Resource governance becomes a **four-gate** chain over a **single per-InstanceType ClusterQueue**, with the
`Devices` CR as the authoritative ledger:

| Gate | Component | Responsibility | Boundary |
|---|---|---|---|
| 1 | **Kueue `credits`** (one-shot fractional fold-down) | Three modes fold to one `credits` resource; coarse total-memory admission (queue quota) | scalar, card-blind; **not the ledger** |
| 2 | **Default Scheduler / Kubelet** | Count node-level `.sliced`/`.sliced.units`/`.sliced.cores-percentage`/`.sliced.memory-*`; drive device-plugin `Allocate` | node-level scalar; card-distribution-blind |
| 3 | **Kueue AdmissionCheck** | After quota reservation, before Admit: read `Devices` CR per-card; `Retry`/`Reject` requests that pass credits but can't be placed | node-blind Kueue must carry its own per-card info |
| 4 (ledger) | **`Devices` CR `AcceleratorAllocation`** | Authoritative per-card ledger; drives three-view display + feeds gate 3 | per-card source of truth |

Runtime sequence: quota reservation (gate 1) → AdmissionCheck (gate 3, reads ledger) → Admitted →
kube-scheduler binds (gate 2) → kubelet/device-plugin allocates and writes back the `Devices` CR (ledger).

**Staged delivery (6 phases; detailed task breakdown deferred to `/my-plan`):**
0. **Foundation & contract** (no runtime behavior change): new `.sliced.cores-percentage` /
   `.sliced.memory-percentage` / `.sliced.memory-mib` resource-name constants; migrate `M`; register the 3 env
   switches (unused); extend systemmeta notes (`unitStorage`, top-level `memory`).
1. **Discovery**: detector `MaxPartitions` (SoftPartition = 512; physical/virtual deferred to the separate MIG/vNPU spec); `NodeCapacityReconciler`
   emits 4 sliced keys, drops the `.sliced.partitions` gate; wire `node-management-manual` (switch ①).
2. **Allocation (DM)**: split `SliceRatio` into a cores path (cores-percentage → `CUDA_DEVICE_SM_LIMIT`) and a
   VRAM path (memory-mib / memory-percentage → `CUDA_DEVICE_MEMORY_LIMIT_*`).
3. **Pooling**: rewrite `ResourceFlavorReconciler → NodeFlavorReconciler` (label-indexed, new names, capacity
   labels) and `ClusterQueueReconciler → NodeQueueReconciler` (credits from RF labels; accelerated queues stop
   advertising CPU/RAM/storage; non-accelerated stop advertising RAM/storage); **delete `CohortReconciler`**;
   rescale `apps_kueue.go` factors; wire `instance-type-derived-from-node` (switch ③).
4. **Admission safety**: new Pod mutating webhook (memory% → units, reject when neither set) + new AdmissionCheck
   (per-card feasibility from `Devices` CR).
5. **API & display**: drop `InstanceTypeAccelerator.Sliced`; add three-view Status + `Sliced*Percentage`/`*Mib`
   fields; extension API computes the three views from the `Devices` CR (read) **and gains Create/Update/Delete**
   that write unit-spec fields back to the backing ClusterQueue labels/annotations (write).
6. **Worker Gateway**: sync the InstanceType JSON contract (the `instance-type-mixed-on-node` effect from
   Phase 3 surfaces here end-to-end).

### User Stories
#### Story 1 — Admin changes a unit spec via the InstanceType API, never touching nodes
As a cluster admin, I want to Create / Update / Delete an InstanceType **through its Extension APIService** and
have my unit-spec changes (RAM-per-CPU, CPU/RAM-per-device) take effect in one Queue/InstanceType-level action,
so that I never log into one or more nodes to edit `${NODE}-gpustack-worker` NodeFeature labels and spec
governance is centralized and auditable. On Create/Update the APIService writes the changed fields back to the
desired locations on the backing ClusterQueue — `note.gpustack.ai/*` annotations (unitCPU/unitRAM/unitStorage/…)
and `schedule.gpustack.ai/*` labels; on Delete it tears down the backing ClusterQueue. The NodeQueueReconciler
treats these admin-written values as the authoritative desired state and does not clobber them on reconcile.

#### Story 2 — User sees and requests all three modes from one InstanceType
As a workload user, I want a single InstanceType to show "allocatable-as-exclusive N1 / shareable N2 /
sliceable N3" simultaneously and let me request any of the three, so that I no longer have to know "sliced is
type A, exclusive is type B" and the resource is not split into two pools.

#### Story 3 — User requests a slice by memory %, units are computed for them
As a workload user, I want to request `nvidia.com/gpu.sliced: 2` + `.sliced.cores-percentage: 10` +
`.sliced.memory-percentage: 20` (or `.sliced.memory-mib: 512`), so that the webhook folds memory into
`.sliced.units` for me and I never reason about normalized units; if I give neither memory field the request is
rejected rather than silently given a full or minimal slice.

#### Story 4 — Exclusive over-admit is caught, not deferred to a stuck Pod
As the platform, when 8 cards are each already 50%-sliced and a user asks for 5 exclusive cards, I want the
AdmissionCheck to reject/hold the request up front (credits alone would admit it), so that it does not become an
unschedulable admitted workload.

#### Story 5 — Admin scopes node onboarding and pool shape
As a cluster admin, I want the `node-management-manual`, `instance-type-mixed-on-node`, and
`instance-type-derived-from-node` settings (env `GPUSTACK_NODE_MANAGEMENT_MANUAL` /
`GPUSTACK_INSTANCE_TYPE_MIXED_ON_NODE` / `GPUSTACK_INSTANCE_TYPE_DERIVED_FROM_NODE`), **adjustable at runtime
without a restart**, so that I can control which nodes are onboarded, set `instance-type-mixed-on-node=false` to
keep GPU machines from also advertising a CPU-only InstanceType, and set `instance-type-derived-from-node=false`
to supply my own ClusterQueue policy while the operator still aligns the ResourceFlavor.

### Core Features & Acceptance Criteria
| # | Feature | Acceptance |
|---|---------|-----------|
| F0a | New sliced resource-name suffixes | `pkg/nodefeature/knowns.go` defines `.sliced.cores-percentage` / `.sliced.memory-percentage` / `.sliced.memory-mib` + `GetAcceleratable*ResourceName` (mirroring `.sliced.units`). |
| F0b | Credit base `M = 1,600,000` | `CreditsPerCard` migrates to `M`; `M % 10 == 0`, `M % SlicedResourceMaxSize(512) == 0`, `M % 100 == 0`; `M = 2⁹×5⁵`. All `CardsToCredits`/`CreditsToCards` callers + tests updated. |
| F0c | Three settings registered (dynamic) | `pkg/worker/settings/value.go` adds `node-management-manual` (`InitializeFromEnv("false")`), `instance-type-mixed-on-node` (`InitializeFromEnv("true")`), `instance-type-derived-from-node` (`InitializeFromEnv("true")`) — all `AllowBool()`; auto-mapping `GPUSTACK_NODE_MANAGEMENT_MANUAL` / `GPUSTACK_INSTANCE_TYPE_MIXED_ON_NODE` / `GPUSTACK_INSTANCE_TYPE_DERIVED_FROM_NODE`. **All consumed per-reconcile via `ShouldValueBool(ctx)` (no package-level `osx.Getenv` cache) so an admin can flip them at runtime without restarting.** |
| F0d | systemmeta notes extended | NodeQueue notes (resType `instancetypes`) gain top-level `memory` (per-card VRAM) and `unitStorage`. |
| F1a | detector MaxPartitions sourcing | **SoftPartition only**: `MaxPartitions = 512` (fixed, `2⁹`); `.sliced` node capacity = `cards × MaxPartitions` via the existing `GetDeviceIds(mode, MaxPartitions)` coupling. PhysicalPartition/VirtualPartition `MaxPartitions` sourcing is **deferred to the separate MIG/vNPU spec** (leave current behavior untouched). |
| F1b | NodeCapacityReconciler emits 4 keys | For any node with `acceleratable.*`: `.sliced.units = count×M`, `.sliced.cores-percentage = count×51200` (`512×100`), `.sliced.memory-percentage = count×100`, `.sliced.memory-mib = count×VRAM`. Drops the `.sliced.partitions` opt-in gate (any acceleratable model counts); stale-cleanup recognizes all 4 suffixes. |
| F1c | Manual node management (switch ①) | Setting `node-management-manual` (`GPUSTACK_NODE_MANAGEMENT_MANUAL`, default false), read per-reconcile: when true the operator does **not** auto-inject `gpustack.ai/managed=true`; toggling at runtime re-converges. |
| F2 | DM allocator decouples cores/VRAM | `SliceRatio` splits: SM limit from `.sliced.cores-percentage`; VRAM limit from `.sliced.memory-mib` (or `memory-percentage × VRAM`). NVIDIA `getSlicedContainerAllocateResponse` + Ascend equivalent updated. `cores-percentage` unset defaults to 100. **Cleanup:** with the allocator no longer reading `.sliced.units` (it becomes Kueue-credits-counting-only), the old `.sliced.units`→R path is orphaned — delete `SliceRatio(units)`, the already-dead `PadSlicedUnits`, and their tests `TestSliceRatio`/`TestPadSlicedUnits`/`TestPartitionedUnitGranularity` so stale code does not mislead later readers. |
| F3a | NodeFlavorReconciler rewrite | Label-indexed over nodes (not RF-driven). Names: `gpustack-${gKey}-${os}-${arch}-${cpu}c` (CPU) / `gpustack-${aKey}-${os}-${arch}-${device}d` (device). RF labels `schedule.gpustack.ai/{key,os,arch,count,capacity}` with `capacity = indexedNodes × count`. Index rules: deleting nodes count as present; `managed != true` excluded; `node.kubernetes.io/unreachable:NoSchedule` counts as absent; when `instance-type-mixed-on-node=false` (switch ②; default true = mixing allowed = current behavior) a GPU node drops its CPU-type index result. `spec.nodeLabels` explicitly pins `kubernetes.io/os` + `kubernetes.io/arch`. drain tombstone preserved. |
| F3b | NodeQueueReconciler rewrite | Credits nominal from RF `schedule.gpustack.ai/capacity` (no Node list/watch). Accelerated CQ advertises **only** `credits` (no CPU/RAM/storage); non-accelerated CQ advertises only CPU (no RAM/storage). Notes (resType `instancetypes`) include `unitCPU/unitRAM/unitStorage/memory`. When `instance-type-derived-from-node=false` (switch ③; default true = auto-derive = current behavior), skip auto CQ create (only RF), still update an existing CQ. |
| F3b' | Auto-created CQ borrowing protection | Auto-created CQ is reconciled to keep `spec.cohortName` **empty** (manual edits reverted) **and** sets `lendingLimit: 0` on the credits quota — so it can never be pulled into a cohort and have its quota borrowed out by externally/manually managed queues. Skipped when `instance-type-derived-from-node=false` (admin owns the CQ). |
| F3c | Cohort removed | `CohortReconciler` deleted; `IndexingNodeByCohortProfile`, `z-cohort` label construction removed; no `Cohort` objects created. |
| F3d | Transformations rescaled + gate-2 exclusion | `apps_kueue.go` factors `1,600,000 / 160,000 / 1` (derived from `M`); sliced rule keeps `multiplyBy .sliced` + `.sliced` empty-outputs drain. **Gate-2 node resources must not block Kueue admission**: `.sliced.cores-percentage` / `.sliced.memory-percentage` / `.sliced.memory-mib` are drained via **per-key `Replace → empty` rules** (preferred — same mechanism as the `.sliced` drain; `excludeResourcePrefixes` is the fallback), so only `.sliced.units → credits` is counted and a Pod requesting these (uncovered) resources is never marked inadmissible. |
| F4a | Pod mutating webhook (+ remove NFD webhook) | New `pods` CREATE webhook (objectSelector on `kueue.x-k8s.io/queue-name`): folds `.sliced.memory-percentage`(×16000) / `.sliced.memory-mib`(×`M/VRAM`, VRAM from CQ `note.gpustack.ai/memory`) into per-card `.sliced.units`. Both unset → **reject**; memory-percentage wins over memory-mib; only folds when `.sliced.units` is absent (no double-write vs Instance controller). `failurePolicy: Fail` (can't compute units → reject). **Ordering is inherent**: an API-admission mutating webhook runs **before the Pod is persisted**, while Kueue creates the Workload and folds credits **after persist** — so `.sliced.units` is always back-filled before Kueue admission (no race). **Remove `NodeFeatureWebhook`** (path no longer needed); webhook set becomes {InstanceWebhook, PodWebhook}. |
| F4b | AdmissionCheck + ledger completeness | New AdmissionCheck controller: after quota reservation, reads `Devices` CR per-card; `Retry`/`Reject` requests that can't be placed (no clean whole card for exclusive / no single card fits). **Ledger completeness**: the device-plugin `Allocate` writes **every** allocation — Kueue-routed or not — into the `Devices` CR `AcceleratorAllocation`, so even Pods that bypass Kueue land in the unified ledger; the AdmissionCheck consults this complete ledger (closes the "non-Kueue bypass" gap). |
| F5a | InstanceType API | Remove `InstanceTypeAccelerator.Sliced int64`. Add three-view Status (allocatable-as-exclusive / shareable / sliceable) + `SlicedCoresPercentage`/`SlicedMemoryPercentage`/`SlicedMemoryMib`. `make generate` regenerates deepcopy/protobuf/CRD/conversion/apiservice. |
| F5b | Extension API three-view (read) | `convertInstanceTypeFromClusterQueue` computes the three views by aggregating the `Devices` CR per-card state (not credits fold-down); CQ stays the metadata/total source. |
| F5c | InstanceType Extension APIService writable (Create/Update/Delete) | The aggregated InstanceType API gains **Create / Update / Delete** (today read-only). Create/Update translate InstanceType fields (unit spec `unitCPU`/`unitRAM`/`unitStorage` + admin-set metadata) into write-backs on the backing ClusterQueue: `note.gpustack.ai/*` annotations + `schedule.gpustack.ai/*` labels. Delete tears down the backing ClusterQueue (and its LocalQueues). With `instance-type-derived-from-node=false`, Create is the admin's path to define a node-queue (operator then only aligns the ResourceFlavor). NodeQueueReconciler must treat admin-written unit-spec values as authoritative (level-based desired state) and not overwrite them. |
| F6 | Worker Gateway sync | `pkg/workergateway/service/types.go` aggregation types track the new InstanceType contract; no business-logic change. |
| F7 | E2E acceptance | New e2e case: the 5-step pooling sequence reproduces the three-view progression exactly; a separate case proves AdmissionCheck rejects the 5th exclusive request on a fully-sliced 8-card node. |

### Notes / Constraints / Caveats
- Go + controller-runtime + Kueue **v0.18.1** (vendored `mirrored-kueue`). `resources.transformations` is a
  **global** Configuration field (no per-CQ/flavor scoping), Replace/Retain/multiplyBy semantics verified
  against that tag; AdmissionCheck is node-blind (must carry its own per-card info); TAS is node-level only.
- `credits` is Kueue-internal only — never written into a real Pod `spec.containers.resources`.
- **Rounding policy:** memory is the non-oversubscribable anchor, so every credits/units conversion **floors**
  (`floor(memory-mib / VRAM × M)`), never ceils — conservative, never over-allocates VRAM.
- The three-view numbers are a **per-card bin-packing** projection from the `Devices` CR; they are **not** a
  credits fold-down (a single credits scalar cannot reproduce them — proven in the report).
- `M = D` keeps the `.sliced.units → credits` factor `= 1`, so `.sliced.units` allocatable *is* the credit value
  (one fewer magic number) — preserve that identity (carried over from the shipped credit-base spec).
- `cores-percentage` (`count×51200`) and `memory-*` are **gate-2 node-level resources**, **not** ClusterQueue
  quota — the CQ has exactly one resourceGroup with one `credits` resource.

### Boundaries
- **Always:** keep credits integer-valued and Kueue-internal; keep the InstanceType API surfacing card/slice
  counts (now three views); floor every memory→units conversion; `spec.nodeLabels` must pin
  `kubernetes.io/os`+`kubernetes.io/arch`; run `make lint` + `go test` on changed packages and re-run e2e.
- **Ask first:** any change to `SlicedResourceMaxSize` / the device-plugin resource set / the soft-slicing
  runtime-isolation injection; any change that would let credits become fractional where Kueue's `ResourceValue`
  ceils it.
- **Never:** fork Kueue; push `credits` as a real node/Pod resource; create a `Cohort`; let an auto-created CQ
  carry a non-empty `spec.cohortName` (keep it cohort-isolated, `lendingLimit: 0`); derive the three-view display
  from the credits scalar; remove `os`/`arch` from RF/CQ names or from `spec.nodeLabels`.

### Risks and Mitigations
- **Credits over-admits exclusive** (scalar can't see clean-card availability) → **AdmissionCheck (gate 3)** +
  `Devices` CR per-card ledger; e2e proves the 5th exclusive is rejected.
- **Non-Kueue Pods bypass the ledger** → **resolved by design**: the device-plugin `Allocate` records *every*
  allocation (Kueue-routed or not) into the `Devices` CR `AcceleratorAllocation` (it sits below Kueue at the
  kubelet layer), so the ledger is complete; the AdmissionCheck reads this unified ledger to further confirm.
- **Node `allocatable` ↔ admission timing** → **not a scheduler-driven race**: kube-scheduler never writes back
  to `Node.status`; `allocatable` is kubelet-owned and the `.sliced.*` extended-resource capacity is patched by
  `NodeCapacityReconciler` (relatively static, changes only on card count/model change). The scheduler tracks
  per-node remaining in its in-memory `NodeInfo` only. The single residual window (topology change before the
  capacity patch lands) is handled by level-based reconcile + AdmissionCheck re-check.
- **Externally/manually managed queues borrow our CQ's quota** → auto-created CQ reconciles `spec.cohortName`
  empty (manual edits reverted) and `lendingLimit: 0` (F3b'); no Cohort = no borrowing channel.
- **`M` migration breaks credit-base tests/readers** → grep all `CreditsPerCard`/`CardsToCredits`/
  `CreditsToCards`/`ResourceMaxUnits` callers; rescale fixtures; `M=D` identity preserved.
- **Removing `os/arch` from gKey loses anti-collision** → relocate to RF/CQ names + `spec.nodeLabels` os/arch
  pins; no Cohort means no cross-ISA borrowing channel either.
- **AdmissionCheck verdict for transient infeasibility** → AdmissionCheck only validates (no preemption —
  shelved); return `Retry` (re-checked as capacity frees, bounded by Kueue backoff), not `Rejected`, for "no
  clean card right now".

## Design Details
### Commands
```bash
make lint
go test ./pkg/nodefeature/... ./pkg/worker/... ./pkg/devicemanager/... ./pkg/deviceplugin/...
make generate   # after editing api/worker/v1/instance_type.go + new webhook
# Kueue transformations are rendered at runtime via a Go text/template; pin factors via the unit test:
go test ./pkg/worker/kuberess/... -run Test_kueueChartTransformations
# E2E (local k3s / docker-desktop) via the gpustack-operator-e2e skill:
bash .claude/skills/gpustack-operator-e2e/cases/<new-pooling-case>.sh gpustack-system
```
### Project Structure (files in scope)
```
pkg/nodefeature/knowns.go                          # F0a suffixes, F0b M=1,600,000, MaxPartitions consumers
pkg/nodefeature/helper.go                          # gKey drops os/arch abbrev; ConstructNodeCapacityLabels; converters
pkg/devicemanager/detector/nvidia/device.go        # F1a MaxPartitions: soft=512 (phys/virt deferred to MIG/vNPU spec)
pkg/deviceplugin/helper.go, server.go              # F2 SliceRatio split (cores vs VRAM)
pkg/devicemanager/allocator/nvidia/deviceplugin.go # F2 SM from cores-%, VRAM from memory-mib (Ascend equivalent)
pkg/worker/controllers/worker/node.go              # F1b NodeCapacityReconciler: 4 keys, drop partitions gate
pkg/worker/controllers/worker/resourceflavor.go    # F3a → NodeFlavorReconciler (label-indexed, capacity labels)
pkg/worker/controllers/worker/clusterqueue.go      # F3b → NodeQueueReconciler (credits from RF labels; no CPU/RAM/storage on accel)
pkg/worker/controllers/worker/cohort.go            # F3c DELETE CohortReconciler
pkg/worker/controllers/worker/setup.go             # deregister cohort; register admissioncheck
pkg/worker/kuberess/apps_kueue.go                  # F3d factors 1,600,000/160,000/1 + exclude gate-2 resources
pkg/worker/webhooks/worker/pod.go (new), setup.go  # F4a add PodWebhook, REMOVE NodeFeatureWebhook (→ {Instance, Pod})
pkg/worker/controllers/worker/admissioncheck.go (new) # F4b per-card feasibility AdmissionCheck
api/worker/v1/instance_type.go                     # F5a drop Sliced int64; three-view + Sliced*Percentage/Mib
pkg/worker/extensionapis/worker/instance_type.go   # F5b three-view from Devices CR (read); F5c Create/Update/Delete → write back to CQ labels/annotations
pkg/worker/settings/value.go                       # F0c three env switches
pkg/workergateway/service/types.go                 # F6 contract sync
```
### Code Style
```go
// One whole card = M = 1,600,000 credit units (= 2⁹×5⁵ = 12800×5³): keeps the 2⁹ factor
// so SlicedResourceMaxSize=512 divides, adds 5³ so the memory-1% step M/100=16000 is integral.
// The .sliced.units → credits factor stays exactly 1 (M = D), so .sliced.units allocatable IS the credit value.
const CreditsPerCard = ResourceMaxUnits // M = D = 1_600_000

// Memory is the non-oversubscribable anchor: every memory→units conversion FLOORS.
func MemoryMibToUnits(mib, cardVRAMMib int64) int64 // = floor(mib / cardVRAMMib * M)

// Three-view display is a per-card bin-packing projection from the Devices CR ledger — NOT a credits fold-down.
// exclusive = freeCards; shareable = freeCards*10 + Σ sharedCardRemainingOwners;
// sliceable  = Σ over (free + sliced) cards of remaining memory-% (each card contributes ≤ 100).
```
### Implementation Plan
Dependency order = the 6 phases of the Proposal. Each task is a **vertical slice** that leaves the tree building;
work-state **checkpoints** sit between phases. Task IDs map to Core Features F0a–F7. The four still-open Open
Questions appear as **verify-first** sub-steps, not blockers. Phase 3 (RF→CQ rewrite + Cohort delete) is tightly
coupled — its three tasks land together and the system is only fully consistent again at the Phase-3 checkpoint.

**Phase 0 — Foundation & contract (no runtime behavior change beyond the 125× credit rescale)**
- [x] **T0.1 (F0a) — sliced resource-name suffixes.** Add `.sliced.cores-percentage` / `.sliced.memory-percentage`
  / `.sliced.memory-mib` constants + `GetAcceleratable*ResourceName` in `pkg/nodefeature/knowns.go` (mirror the
  `.sliced.units` constructor at `:224`). Pure addition, unused. **Accept:** constructors return the exact names.
  **Verify:** `go test ./pkg/nodefeature/...`.
- [x] **T0.2 (F0b) — migrate credit base `M = 1,600,000`.** Derive `CreditsPerCard`/`ResourceMaxUnits` so
  `M = 1,600,000` (`2⁹×5⁵`); keep `CardsToCredits`/`CreditsToCards`. Rescale base-dependent fixtures — wider than
  first scoped: `knowns_test`, `apps_kueue_test` (factors), `instance_type_test` (reservation inputs),
  `controllers/worker/instance_test` (`.sliced.units` expectations), `deviceplugin/helper_test`
  (`PadSlicedUnits`/`SliceRatio`/`PartitionedUnitGranularity`), `allocator/{nvidia,ascend}/deviceplugin_test`
  (`slicedPod` units); plus fix the stale `B=12800` comment in the rendered Kueue transformations template
  (`apps_kueue.go`). (`clusterqueue_test` was already base-relative via `cards × CreditsPerCard` — no change.)
  All consumers derive from the constant, so the system scales consistently. **Accept:**
  `M%10==0 && M%512==0 && M%100==0`; round-trips hold; full unit suite green. **Verify:** `go test ./pkg/...`.
- [ ] **T0.3 (F0c) — register 3 dynamic settings.** Add `node-management-manual` (`InitializeFromEnv("false")`),
  `instance-type-mixed-on-node` (`InitializeFromEnv("true")`), `instance-type-derived-from-node`
  (`InitializeFromEnv("true")`) to `pkg/worker/settings/value.go` (`AllowBool()`); auto-mapped to
  `GPUSTACK_NODE_MANAGEMENT_MANUAL` / `GPUSTACK_INSTANCE_TYPE_MIXED_ON_NODE` / `GPUSTACK_INSTANCE_TYPE_DERIVED_FROM_NODE`.
  Registered but unconsumed; consumers (T1.3 / T3.1 / T3.2 / T5.3) **read per-reconcile via `ShouldValueBool(ctx)`**
  (not a package var) so flips apply without restart. **Accept:** each resolves from its env var with the right
  default (false / true / true). **Verify:** new `pkg/worker/settings/value_test.go`.
- [ ] **T0.4 (F0d) — note-key contract + CQ-VRAM reader.** Add note-key constants (`memory`, `unitStorage`) and a
  helper to read a ClusterQueue's per-card VRAM from `note.gpustack.ai/memory` (used later by the webhook). Pure
  addition. **Accept:** helper returns the VRAM quantity from a noted CQ. **Verify:**
  `go test ./pkg/systemmeta/... ./pkg/nodefeature/...`.
- *Checkpoint: `make lint` clean + full `go test ./pkg/...` green on the rescaled base; no behavior change but the
  125× credit magnitude.*

**Phase 1 — Discovery**
- [ ] **T1.1 (F1a) — detector MaxPartitions (soft only).** `pkg/devicemanager/detector/nvidia/device.go:204-216`:
  SoftPartition → `MaxPartitions = 512` (drop the VRAM-derived power-of-two loop). Physical/Virtual left untouched
  (separate spec). **Accept:** a SoftPartition device reports `MaxPartitions=512`; `.sliced` node capacity =
  `cards×512`. **Verify:** new `device_test.go` + `kubectl get node -o json`.
- [ ] **T1.2 (F1b) — NodeCapacityReconciler emits 4 keys.** `pkg/worker/controllers/worker/node.go`: drop the
  `.sliced.partitions` opt-in gate (`:93`); emit `units=count×M / cores-percentage=count×51200 /
  memory-percentage=count×100 / memory-mib=count×VRAM` (VRAM from `acceleratable.<…>.memory`); extend
  stale-cleanup (`:121,:144`) to all 4 suffixes. **Accept:** an acceleratable node shows all 4 capacities;
  removing the model removes all 4. **Verify:** `go test ./pkg/worker/controllers/worker/...` (`node_test.go`).
- [ ] **T1.3 (F1c) — wire `node-management-manual` (switch ①), read per-reconcile.** Gate the
  `gpustack.ai/managed=true` injection (`pkg/nodefeature/helper.go:321`) on the setting, read at reconcile
  (NodeFeatureReconciler passes the value into `ConstructNodeCapacityLabels` — **not** a package-level env var) so
  a flip applies without restart. **Accept:** setting true → node not auto-managed; toggling at runtime
  re-converges. **Verify:** `go test ./pkg/nodefeature/...` + manual.
- *Checkpoint: a GPU node advertises 4 `.sliced.*` capacities; switch ① works; e2e `case-1..5` still pass after
  the rescale.*

**Phase 2 — Allocation (DeviceManager)**
- [ ] **T2.1 (F2) — decouple cores/VRAM in the allocator.** `pkg/deviceplugin/helper.go:127` `SliceRatio` → two
  paths: SM from `.sliced.cores-percentage` (default 100), VRAM from `.sliced.memory-mib` (or
  `memory-percentage×VRAM`). `pkg/devicemanager/allocator/nvidia/deviceplugin.go:178-235`: `CUDA_DEVICE_SM_LIMIT`
  ← cores-%, `CUDA_DEVICE_MEMORY_LIMIT_*` ← memory-mib. **Cleanup (orphan removal):** once the nvidia/ascend
  allocators (`deviceplugin.go:188`/`:195`) stop calling `SliceRatio(.sliced.units)`, delete the old
  `.sliced.units`→R path — `SliceRatio(units)` and the already-dead `PadSlicedUnits` in `helper.go`, plus the
  tests `TestSliceRatio`/`TestPadSlicedUnits`/`TestPartitionedUnitGranularity` in `helper_test.go`. `.sliced.units`
  then survives only as a Kueue-credits-counting / node-capacity key (`apps_kueue.go`, `node.go`,
  `clusterqueue.go`, `instance.go`). **Accept:** a sliced container gets independent SM and VRAM limits; no
  remaining reference to `PadSlicedUnits`/`SliceRatio`. **Verify:** `go test ./pkg/deviceplugin/...
  ./pkg/devicemanager/allocator/nvidia/...` + `grep -r 'PadSlicedUnits\|SliceRatio' pkg/` empty.
- [ ] **T2.2 — Ascend allocator parity.** Mirror T2.1 in `pkg/devicemanager/allocator/ascend/deviceplugin.go:195`.
  **Accept/Verify:** Ascend unit tests (or documented N/A if no soft-slicing path).
- *Checkpoint: sliced containers carry decoupled SM/VRAM limits; optional `gpustack-operator-xbuild-and-verify`
  on real 4090 / 910B.*

**Phase 3 — Pooling rewrite + delete Cohort (tightly coupled; land together)**
- [ ] **T3.1 (F3a) — ResourceFlavorReconciler → NodeFlavorReconciler.** `resourceflavor.go`: label-indexed over
  nodes; names `gpustack-${gKey}-${os}-${arch}-${cpu}c` / `gpustack-${aKey}-${os}-${arch}-${device}d`; RF labels
  `schedule.gpustack.ai/{key,os,arch,count,capacity}` (`capacity=indexedNodes×count`); `spec.nodeLabels`
  explicitly pins `kubernetes.io/os`+`kubernetes.io/arch`; index rules (deleting=present, `managed!=true` skip,
  `unreachable:NoSchedule`=absent; when `instance-type-mixed-on-node=false` (switch ②, read per-reconcile) a GPU
  node drops its CPU-type index result); keep drain tombstone.
  **Accept:** RF naming + labels per spec. **Verify:** `go test ./pkg/worker/controllers/worker/...` (rename
  `resourceflavor_test`→`nodeflavor_test`).
- [ ] **T3.2 (F3b/F3b'/F3d) — ClusterQueueReconciler → NodeQueueReconciler + transformations.** Credits from RF
  `capacity` label (delete the Node-list block at `clusterqueue.go:297-382`); accel CQ advertises **only**
  `credits`, non-accel only CPU; `spec.cohortName` empty + `lendingLimit:0`
  (empty cohortName = isolated, confirmed); notes incl `memory`/`unitStorage`; `instance-type-derived-from-node=false` (switch ③) skips auto-create.
  `apps_kueue.go:272-280` factors → `1,600,000/160,000/1`; **drain gate-2 resources via per-key `Replace→empty`**
  (preferred; confirm vs `excludeResourcePrefixes` at impl). **Accept:** CQ credits=`capacity×M`, no CPU/RAM/storage on
  accel, cohortName empty, gate-2 not counted. **Verify:** `go test ./pkg/worker/controllers/worker/...
  ./pkg/worker/kuberess/...`.
- [ ] **T3.3 (F3c) — delete CohortReconciler.** Remove `cohort.go` + `cohort_test.go`,
  `IndexingNodeByCohortProfile`, the `z-cohort` construction; deregister from `setup.go:14`. **Accept:** build
  green; no `Cohort` created. **Verify:** `go test ./pkg/worker/...` + `grep -r CohortReconciler` empty.
- *Checkpoint: e2e — NFD→Worker→Kueue materializes RF/CQ with new names + capacity labels, **zero Cohort
  objects**; the unit spec lives in CQ notes.*

**Phase 4 — Admission safety layer**
- [ ] **T4.1 (F4a) — Pod mutating webhook + remove NFD webhook.** *Verify-first: confirm the generator supports a
  core `pods` target + `objectSelector` (else hand-write the config).* New `pkg/worker/webhooks/worker/pod.go`:
  fold `.sliced.memory-percentage`/`.sliced.memory-mib`→`.sliced.units` (VRAM from CQ note), reject when neither,
  `failurePolicy:Fail`, only when `.sliced.units` absent. `setup.go:15`: add `PodWebhook`, **remove
  `NodeFeatureWebhook`**. `make generate`. **Accept:** memory-% Pod gets units; neither→rejected; webhook set
  `{Instance,Pod}`. **Verify:** `go test ./pkg/worker/webhooks/...` + `make generate`.
- [ ] **T4.2 (F4b) — AdmissionCheck + ledger write-back.** New `admissioncheck.go` reading the `Devices` CR
  per-card; ensure the allocator writes `AcceleratorAllocation` on every `Allocate` (ledger completeness);
  register in `setup.go`. AdmissionCheck is **check-only** (no preemption — shelved this round); return `Retry`
  for transient per-card infeasibility (re-checked as capacity frees). **Accept:** on a fully-sliced 8-card node
  the 5th exclusive is held by `Retry` (not admitted-then-stuck). **Verify:**
  `go test ./pkg/worker/controllers/worker/...` + e2e.
- *Checkpoint: the webhook back-fills units before Kueue admission; the AdmissionCheck blocks the over-admit.*

**Phase 5 — API & display (read three-view + writable APIService)**
- [ ] **T5.1 (F5a) — InstanceType API.** `api/worker/v1/instance_type.go`: remove `Sliced int64`; add three-view
  Status + `SlicedCoresPercentage`/`SlicedMemoryPercentage`/`SlicedMemoryMib`. **Accept:** types compile;
  CRD/proto/conversion regenerated. **Verify:** `make generate` + `go build ./...`.
- [ ] **T5.2 (F5b) — extension API three-view (read).** `convertInstanceTypeFromClusterQueue` aggregates the
  `Devices` CR per-card state into the three views (not credits fold-down). **Accept:** views match per-card
  aggregation. **Verify:** `go test ./pkg/worker/extensionapis/worker/...`.
- [ ] **T5.3 (F5c) — writable extension APIService.** Add Create/Update/Delete: write unit-spec fields → CQ
  `note.gpustack.ai/*` + `schedule.gpustack.ai/*`; Delete tears down the CQ. *Verify-first: reconciler-vs-write
  authority (override marker / per-field precedence) + Delete semantics under `instance-type-derived-from-node=false`.* **Accept:**
  `kubectl edit instancetype` persists to CQ notes; reconcile doesn't clobber admin values. **Verify:**
  `go test ./pkg/worker/extensionapis/worker/...` + e2e.
- *Checkpoint: e2e — the 5-step pooling sequence shows the three-view progression exactly.*

**Phase 6 — Worker Gateway**
- [ ] **T6.1 (F6) — gateway contract sync.** `pkg/workergateway/service/types.go`: track the new InstanceType
  JSON contract (three views + `Sliced*` fields). No business-logic change. **Accept:** gateway aggregates
  without error. **Verify:** `go test ./pkg/workergateway/...` + `gpustack-operator-chart-e2e`.
- *Final checkpoint: full `make lint` + `go test ./...` green; both e2e anchor cases (case-6, case-7) pass.*

### Test Plan
[ ] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates
- Rescale base-dependent fixtures for the `M=1,600,000` migration (T0.2), wider than first scoped: `knowns_test` /
  `apps_kueue_test` (factors) / `instance_type_test` (reservation inputs) / `controllers/worker/instance_test`
  (`.sliced.units`) / `deviceplugin/helper_test` (`PadSlicedUnits`/`SliceRatio`/`PartitionedUnitGranularity`) /
  `allocator/{nvidia,ascend}/deviceplugin_test` (`slicedPod` units). `clusterqueue_test` is already base-relative
  (`cards × CreditsPerCard`). (The `deviceplugin/helper_test` trio is later deleted in T2.1 as orphan cleanup.)
- Add missing test files: `pkg/devicemanager/detector/nvidia` (none today) and `pkg/worker/settings` (none today).
- Rescale `e2e case-5` credit assertions to the new base before reusing it.

#### Unit tests
Every added unit gets coverage; target ≥ existing, no regression. Per-package (date 2026-06-29):
- `pkg/nodefeature`: new suffix constructors; `M` invariants (`%10`/`%512`/`%100`); `CardsToCredits`/`CreditsToCards` round-trips; gKey without os/arch abbrev.
- `pkg/devicemanager/detector/nvidia`: **NEW** — SoftPartition `MaxPartitions=512`.
- `pkg/worker/controllers/worker`: `node_test` (4 sliced keys + stale-clean); `nodeflavor_test` (names + `capacity` label + index rules + nodeLabels os/arch pin); `nodequeue_test` (credits=`capacity×M`; accel has no CPU/RAM/storage; `cohortName` empty + `lendingLimit:0`); **DELETE** `cohort_test`.
- `pkg/worker/kuberess`: `apps_kueue_test` (factors `1,600,000/160,000/1`, `multiplyBy`, gate-2 excluded/drained, `.sliced` drain).
- `pkg/deviceplugin` + `pkg/devicemanager/allocator/nvidia`: `SliceRatio` split; `SM←cores-%`, `VRAM←memory-mib`; `cores-%` default 100.
- `pkg/worker/webhooks/worker`: **NEW** `pod_test` — fold `memory-%`/`memory-mib`→`units`, reject when neither, `failurePolicy:Fail`, no double-write when `.sliced.units` present.
- `pkg/worker/extensionapis/worker`: `instance_type_test` — three views from the `Devices` CR; Create/Update write-back to CQ labels/annotations; Delete teardown.
- `pkg/worker/settings`: **NEW** — the 3 settings map from their `GPUSTACK_*` vars with correct defaults (`node-management-manual`=false; `instance-type-mixed-on-node`/`instance-type-derived-from-node`=true) and are read per-reconcile via `ShouldValueBool(ctx)` (runtime-adjustable, no restart).

#### Integration tests
- Fake-client controller tests exercise the `NodeFlavor → NodeQueue → InstanceType` chain in-process (asserting **no** `Cohort` is created).
- `AdmissionCheck` × `Devices` CR per-card feasibility, table-driven: clean-card shortage vs generic headroom (the 8×50%-sliced case yields exclusive=0).

#### e2e tests
Under `.claude/skills/gpustack-operator-e2e/cases/`:
- **NEW `case-6`** — on an 8× A10G (24Gi) node, the five-step pooling sequence asserts the three-view progression `8/80/800 → 6/60/600 → 4/58/400 → 2/38/360 → 2/38/356 → 1/28/256`; asserts a unit-spec change via the InstanceType API writes CQ notes and touches **no** `Node`/NodeFeature; asserts **zero** `Cohort` objects.
- **NEW `case-7`** — 8 cards each 50%-sliced; the 5th exclusive request is rejected/held by the `AdmissionCheck` (not admitted-then-unschedulable).
- Reuse `case-1..5` (rescaled) as regression for the discovery→Kueue chain.

## Alternatives
- **What-if projection (three views from the credits scalar)** — rejected: a single credits value cannot
  reproduce the example progression (step 2 gives `5/58/580`, not `4/58/400`), and it over-promises exclusive
  availability since exclusive needs a clean card. The three views must come from the per-card `Devices` CR.
- **Multiple Kueue resources (separate `whole-gpu`/`shared-slot`/`slice-mem-pct` quotas)** — rejected: once
  capacity is split into independent quotas they no longer share fungibly (back to "two pools"), and Kueue still
  can't express "a shared card blocks exclusive".
- **Keep Cohort, keep separate sliced queue** — rejected: the only reason Cohort existed was to let the sliced
  queue borrow from the exclusive queue; folding all modes into one ClusterQueue removes the need entirely.
- **Keep `os/arch` inside gKey** — rejected for name length; relocated to RF/CQ names + `spec.nodeLabels` pins,
  which is strictly more explicit.

## Open Questions
### Resolved during spec review (2026-06-29)
- **Webhook on core `pods` + admission ordering** → settled. Remove `NodeFeatureWebhook` (no longer needed); add
  the Pod mutating webhook for sliced-unit control. Ordering is inherent: the API-admission mutating webhook
  back-fills `.sliced.units` **before persist**, while Kueue builds the Workload and folds credits **after
  persist**, so Kueue never admits a unit-less Pod (`failurePolicy: Fail`). The ClusterQueue/Configuration must
  **exclude** the gate-2 node resources (`.sliced.cores-percentage`/`.memory-percentage`/`.memory-mib`) and the
  multiplyBy-only `.sliced` from accounting, so only `.sliced.units → credits` is counted (F3d).
- **Non-Kueue Pods bypassing the ledger** → not a gap. The device-plugin `Allocate` records every allocation
  into the `Devices` CR `AcceleratorAllocation` (below Kueue), so the unified ledger is complete and the
  AdmissionCheck confirms against it (F4b).
- **`Node.status.allocatable` write-back** → confirmed: the kube-scheduler does **not** write back to the Node;
  `allocatable` is kubelet-owned and the `.sliced.*` capacity is patched by `NodeCapacityReconciler`; the
  scheduler's per-node remaining lives only in its in-memory `NodeInfo`. No scheduler-driven race.
- **Rounding policy** → floor everywhere for memory→units (memory is the non-oversubscribable anchor).
- **Physical/Virtual Partition (MIG/vNPU)** → out of scope; a separate dedicated spec (incl. their `MaxPartitions`
  vendor-lib sourcing). This spec covers SoftPartition only.

### Resolved during plan review (2026-06-29, round 2)
- **AdmissionCheck is check-only; preemption shelved** → the AdmissionCheck only validates (sets
  Pending/Ready/Retry/Rejected) and never preempts. For "credits ok but no clean card right now" return `Retry`
  (transient — re-checked as capacity frees, bounded by Kueue's backoff) rather than `Rejected` (permanent). Any
  preemption interaction is **shelved this round**. The `Retry` vs `Rejected` verdict is a T4.2 detail, not a
  blocker.
- **Empty `cohortName` is accepted/isolated** → a ClusterQueue with empty `spec.cohortName` is valid and isolated
  (no implicit shared cohort), so F3b' enforcement is safe; when `instance-type-derived-from-node=false` the
  enforcement is skipped (admin owns the CQ).

### Still open (confirm during implementation)
- **Exclude vs drain for gate-2 resources (T3.2)** → **prefer per-key `Replace → empty` transformation rules**
  (same mechanism as the existing `.sliced` drain — one consistent path, likely cheaper than a separate
  `excludeResourcePrefixes` scan); confirm this holds at implementation. Either way the firm requirement stands:
  `.sliced.cores-percentage`/`.memory-percentage`/`.memory-mib` must not be counted or block admission.
- **Reconciler vs InstanceType-write contract (F5c, T5.3)**: how does NodeQueueReconciler tell admin-set
  unit-spec values (written via the InstanceType Create/Update) it must preserve from values it derives/defaults —
  an explicit override marker (e.g. a `schedule.gpustack.ai/unit-spec-source=manual` annotation) or per-field
  precedence? And what does Delete do when the backing nodes still exist (the reconciler would recreate the CQ) —
  is Delete only meaningful under `instance-type-derived-from-node=false`? Pin during T5.3.
