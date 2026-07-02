# Spec: Instance Type Unified-Pool Refactor — Queue-Managed Unit Specs, Four-Gate Admission, Devices-CR Ledger

Status: Shipped
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
- **InstanceType is a real CRD with a materialized three-view status.** The three views are written into the
  InstanceType `.status` subresource by a reconciler that watches the `Devices` CR + ClusterQueue, so watch /
  informer consumers (the Worker Gateway, the operator's own admission path) observe capacity as pods allocate
  and free GPUs. A read-only projection over the ClusterQueue cannot: it borrows the CQ's `resourceVersion`,
  which does not advance on `Devices`-only changes, so its watch is blind to allocation churn (see
  `.claude/reports/instancetype-onwatch-devices-staleness.md`).
- **Four-gate admission** (see Proposal). The new **AdmissionCheck** closes the credits "over-admit exclusive"
  gap that scalar quota cannot express.
- **Credit-base migration `12800 → M = 1,600,000`** (`= 2⁹×5⁵ = 12800×5³`): preserves the `2⁹` factor (so
  `SlicedResourceMaxSize = 512` still divides) and adds `5³` (so the memory-1% granularity `M/100 = 16000` is
  integral); `1/M = 625 nano` stays clean. Transformation factors become `1,600,000 / 160,000 / 1` (+ sliced
  `multiplyBy .sliced`).
- **Decouple RF/CQ reconcilers from Node.** Only `NodeFlavorReconciler` reads nodes; it pre-computes
  `capacity = indexedNodes × count` into RF labels in the node-feature vocabulary (the flavor's
  `{general.|acceleratable.}feature.gpustack.ai/${key}=true`, `kubernetes.io/os|arch`, and per-key `.count`/`.capacity`).
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
   switches (unused); extend systemmeta notes (top-level `memory` and `localStorage`).
1. **Discovery**: detector `MaxPartitions` (SoftPartition = 512; physical/virtual deferred to the separate MIG/vNPU spec); `NodeCapacityReconciler`
   emits 4 sliced keys, drops the `.sliced.partitions` gate; wire `node-management-manual` (switch ①).
2. **Allocation (DM)**: split `SliceRatio` into a cores path (cores-percentage → `CUDA_DEVICE_SM_LIMIT`) and a
   VRAM path (memory-mib / memory-percentage → `CUDA_DEVICE_MEMORY_LIMIT_*`).
3. **Pooling**: rewrite `ResourceFlavorReconciler → NodeFlavorReconciler` (label-indexed, new names, capacity
   labels) and `ClusterQueueReconciler → NodeQueueReconciler` (credits from RF labels; accelerated queues stop
   advertising CPU/RAM/storage; non-accelerated stop advertising RAM/storage); **delete `CohortReconciler`**;
   rescale `apps_kueue.go` factors; wire `instance-type-derived-from-node` (switch ③).
4. **Admission safety**: rework the Instance slice API (`AcceleratorUnits` → `AcceleratorSlicedMemoryPercentage`
   + `AcceleratorSlicedCoresPercentage`, both 0–100) with the InstanceWebhook defaulting/validating them; add a
   Pod webhook (mutating: memory% → `.sliced.units`, default cores-% = 100; validating: reject when memory is
   unset or `cores-% < memory-%`) + a new AdmissionCheck (per-card feasibility from `Devices` CR).
5. **API & display**: replace `InstanceTypeAccelerator.Sliced int64` with a `Sliceable bool` capability flag;
   reshape the InstanceType Status to the three views (`Accelerator`/`AcceleratorShared`/`AcceleratorSliced`),
   **dropping** the `RAM`/`LocalStorage` display fields; **InstanceType becomes a real CRD** whose three-view
   `.status` is materialized by an `InstanceTypeReconciler` watching the `Devices` CR per-card ledger + ClusterQueue
   (so watch consumers see it — a ClusterQueue projection cannot), a validating webhook guards admin
   writes, and the reconciler syncs the spec back to the backing ClusterQueue (create/update notes+labels; delete
   via a finalizer + HoldAndDrain); the Worker Gateway tracks the same three views. *(This supersedes the earlier
   read-projection + writable aggregated APIService — T5.1 read path, T5.3 — after the OnWatch finding; Direction 2,
   2026-07-01.)*
6. **Worker Gateway**: sync the InstanceType JSON contract (the `instance-type-mixed-on-node` effect from
   Phase 3 surfaces here end-to-end).

### User Stories
#### Story 1 — Admin changes a unit spec via the InstanceType API, never touching nodes
As a cluster admin, I want to Create / Update / Delete an InstanceType **as a first-class CRD** and have my
unit-spec changes (RAM-per-CPU, CPU/RAM-per-device) take effect in one Queue/InstanceType-level action, so that
I never log into one or more nodes to edit `${NODE}-gpustack-worker` NodeFeature labels and spec governance is
centralized and auditable. A validating webhook checks my writes; an `InstanceTypeReconciler` then syncs
the desired spec onto the backing ClusterQueue — `note.gpustack.ai/*` annotations (unitCPU/unitRAM/localStorage/…)
and the schedule labels (the feature key + `kubernetes.io/os|arch`) — and, on Delete, tears the ClusterQueue down
(a finalizer holds the InstanceType until it has drained). The reconciler treats these admin-written values as the
authoritative desired state and does not clobber them.

#### Story 2 — User sees and requests all three modes from one InstanceType
As a workload user, I want a single InstanceType to show "allocatable-as-exclusive N1 / shareable N2 /
sliceable N3" simultaneously and let me request any of the three, so that I no longer have to know "sliced is
type A, exclusive is type B" and the resource is not split into two pools.

#### Story 3 — User requests a slice by memory %, units are computed for them
As a workload user, I want to request `nvidia.com/gpu.sliced: 2` + `.sliced.memory-percentage: 20`
(optionally raising `.sliced.cores-percentage` ≥ 20, or using `.sliced.memory-mib: 512` instead), so that the
webhook folds memory into `.sliced.units` for me and I never reason about normalized units. If I give neither
memory field the request is rejected rather than silently given a full or minimal slice; the compute budget may
not be smaller than the memory budget (`cores-percentage ≥ memory-percentage`), and an omitted
`.sliced.cores-percentage` defaults to 100.

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

#### Story 6 — Live capacity: watchers see the three-view change as pods come and go
As the Worker Gateway (and any controller or UI watching InstanceTypes), I want the accelerator three-view to
update as pods allocate and free GPUs — not only when an unrelated ClusterQueue field changes — so that displayed
and admission-checked capacity is fresh. This requires the three-view to live in a watchable, versioned store
(the InstanceType CRD `.status`); a projection over the ClusterQueue cannot deliver correct incremental watch.

### Core Features & Acceptance Criteria
| # | Feature | Acceptance |
|---|---------|-----------|
| F0a | New sliced resource-name suffixes | `pkg/nodefeature/knowns.go` defines `.sliced.cores-percentage` / `.sliced.memory-percentage` / `.sliced.memory-mib` + `GetAcceleratable*ResourceName` (mirroring `.sliced.units`). |
| F0b | Credit base `M = 1,600,000` | `CreditsPerCard` migrates to `M`; `M % 10 == 0`, `M % SlicedResourceMaxSize(512) == 0`, `M % 100 == 0`; `M = 2⁹×5⁵`. All `CardsToCredits`/`CreditsToCards` callers + tests updated. |
| F0c | Three settings registered (dynamic) | `pkg/worker/settings/value.go` adds `node-management-manual` (`InitializeFromEnv("false")`), `instance-type-mixed-on-node` (`InitializeFromEnv("true")`), `instance-type-derived-from-node` (`InitializeFromEnv("true")`) — all `AllowBool()`; auto-mapping `GPUSTACK_NODE_MANAGEMENT_MANUAL` / `GPUSTACK_INSTANCE_TYPE_MIXED_ON_NODE` / `GPUSTACK_INSTANCE_TYPE_DERIVED_FROM_NODE`. **All consumed per-reconcile via `ShouldValueBool(ctx)` (no package-level `osx.Getenv` cache) so an admin can flip them at runtime without restarting.** |
| F0d | systemmeta notes extended | NodeQueue notes (resType `instancetypes`) gain top-level `memory` (per-card VRAM) and `localStorage`. |
| F1a | detector MaxPartitions sourcing | **SoftPartition only**: `MaxPartitions = SlicedResourceMaxSize` (512, `2⁹`, inlined — no trivial wrapper); `.sliced` node capacity = `cards × MaxPartitions` via the existing `GetDeviceIds(mode, MaxPartitions)` coupling. NVIDIA always soft-partitions; Ascend soft-partitions on family `910B`/`910C`/`950`. PhysicalPartition/VirtualPartition `MaxPartitions` sourcing is **deferred to the separate MIG/vNPU spec** (leave current behavior untouched). |
| F1b | NodeCapacityReconciler emits 4 keys | For any node with `acceleratable.*`: `.sliced.units = count×M`, `.sliced.cores-percentage = count×51200` (`512×100`), `.sliced.memory-percentage = count×100`, `.sliced.memory-mib = count×VRAM`. Drops the `.sliced.partitions` opt-in gate (any acceleratable model counts); stale-cleanup recognizes all 4 suffixes. |
| F1c | Manual node management (switch ①) | Setting `node-management-manual` (`GPUSTACK_NODE_MANAGEMENT_MANUAL`, default false), read per-reconcile: when true the operator does **not** auto-inject `gpustack.ai/managed=true`; toggling at runtime re-converges. |
| F2 | DM allocator decouples cores/VRAM | New `SlicedCoresPercent` (SM, from `.sliced.cores-percentage`, default 100) and `SlicedMemoryMib` (per-card VRAM) helpers replace the single `.sliced.units`→ratio. VRAM is `memory-percentage × cardVRAM` (**primary**, floored, capped at the card) or the absolute `.sliced.memory-mib` — percentage wins when both are set, matching the Pod webhook fold so credits and the real limit agree. NVIDIA `getSlicedContainerAllocateResponse` (T2.1) + Ascend (T2.2) updated; missing memory → error. **Cleanup (T2.2):** with both allocators off `.sliced.units` (it becomes Kueue-credits-counting-only), delete the orphaned `SliceRatio`, `FloorPercent`, the already-dead `PadSlicedUnits`, and their tests (`TestSliceRatio`/`TestPadSlicedUnits`/`TestPartitionedUnitGranularity`/`TestFloorPercent`). |
| F3a | NodeFlavorReconciler rewrite | Label-indexed over nodes (not RF-driven). Names: `gpustack-${gKey}-${os}-${arch}-${cpu}c` (CPU) / `gpustack-${aKey}-${os}-${arch}-${device}d` (device), full os/arch (name limit is 253, gKey ≤ 63). RF labels in the node-feature vocabulary so the flavor is reverse-looked-up from its CQ and matching Nodes/Devices: the feature key `{general.|acceleratable.}feature.gpustack.ai/${key}=true`, `kubernetes.io/os|arch` (full), and per-key `.count`/`.capacity` (`capacity = indexedNodes × count`). Index rules: deleting nodes count as present; `managed != true` excluded (taints are **not** consulted); when `instance-type-mixed-on-node=false` (switch ②; default true = mixing allowed = current behavior, read per-reconcile) a GPU node drops its CPU-type contribution. `spec.nodeLabels` explicitly pins `kubernetes.io/os` + `kubernetes.io/arch` (full values); **a blanket `{Operator: Exists}` toleration** so a quota-routed workload is never blocked by a node taint (eligibility is governed by nodeLabels). **When no Node contributes the flavor is deleted** (no drain tombstone — it buys nothing once the CQ is rebuilt from RF labels). |
| F3b | NodeQueueReconciler → merged into InstanceTypeReconciler | **Direction 3 (2026-07-01):** `NodeQueueReconciler` is **removed** — its CQ alignment (credits/quota from RF `.capacity` via `buildResourceGroups`), note assembly, node-devices AdmissionCheck wiring, and HoldAndDrain teardown all fold into `InstanceTypeReconciler` (F5d), now the **sole owner** of the CQ; `buildResourceGroups` runs inside ITR's `ensureClusterQueue`. The CQ **keeps** its `instancetypes` resourceType marker + descriptive notes (`acceleratable/manufacturer/product/family/memory/sliceable`) + admin unit notes (`unitCPU/unitRAM/localStorage`). The rest of this row describes that now-merged behavior. CQ name `gpustack-${key}-${os}-${arch}` (RF name minus the `-${count}{c\|d}` suffix) aggregates all RFs sharing (key,os,arch) regardless of count, each a flavor. Credits nominal from the RF's per-key `.capacity` label (no Node list/watch). The CQ also carries the same discriminator labels its flavors do — the feature key + `kubernetes.io/os|arch` — so its RFs (and, with `gpustack.ai/managed=true`, its Devices) are reverse-looked-up from the queue by selector. Accelerated CQ advertises **only** `credits` (no CPU/RAM/storage); non-accelerated CQ advertises only CPU (no RAM/storage). Notes (resType `instancetypes`) include `unitCPU/unitRAM/localStorage/memory`. **NQR never creates the CQ** (Direction 2, 2026-07-01) — the `InstanceTypeReconciler` (F5d) owns CQ existence, creating it from the InstanceType CR; NQR only **aligns** an existing CQ (credits/quota + descriptive notes) from its RFs and executes teardown. The former derived-mode CQ-create moved to ITR, which auto-creates the InstanceType from RFs when `instance-type-derived-from-node=true` (switch ③; default true = auto-derive). **Teardown via HoldAndDrain**: when no RF backs the CQ, **or** the CQ is already in `HoldAndDrain` (delete intent set by the ITR finalizer on InstanceType delete, F5c/F5d), NQR stops rebuilding/aligning it, ensures `StopPolicy=HoldAndDrain`, waits until all workloads are evicted (no reserved/admitted), then performs the delete itself. |
| F3b' | Auto-created CQ borrowing protection | Auto-created CQ is reconciled to keep `spec.cohortName` **empty** (manual edits reverted) — an empty cohort is complete isolation: with no cohort there is no channel through which externally/manually managed queues could borrow the quota out. The quota carries **no** `borrowingLimit`/`lendingLimit`: **Kueue rejects any borrow/lend limit on a cohort-less ClusterQueue** (`borrowingLimit must be nil when cohort is empty`), and the empty cohort already makes such a limit meaningless. Skipped when `instance-type-derived-from-node=false` (admin owns the CQ). **(Corrected T5.4b, 2026-07-01, e2e finding: the earlier `lendingLimit: 0` was in fact coded as a `borrowingLimit: 0` and — either way — is invalid against a live Kueue API server, so it silently blocked every derived CQ from being created; only the empty cohort remains.)** |
| F3c | Cohort removed | `CohortReconciler` deleted; `IndexingNodeByCohortProfile`, `z-cohort` label construction removed; no `Cohort` objects created. |
| F3d | Transformations rescaled + gate-2 exclusion | `apps_kueue.go` factors `1,600,000 / 160,000 / 1` (derived from `M`); the sliced rule keeps `multiplyBy .sliced`. **Gate-2 node resources — and every other Pod resource the single-dimension queue does not cover — must not block Kueue admission**: instead of per-key `Replace → empty` drains, the deployed Configuration sets `quotaCheckStrategy: IgnoreUndeclared` so each queue checks only its covered dimension (`credits`/`cpu`) and ignores `.sliced` / `.sliced.cores-percentage` / `.sliced.memory-percentage` / `.sliced.memory-mib` (and `cpu`/`memory`/`ephemeral-storage` where uncovered). Only `.sliced.units → credits` is counted. See the resolved e2e finding below. |
| F4a | Pod webhook (mutating + validating) + remove NFD webhook | New `pods` CREATE webhook (objectSelector on `kueue.x-k8s.io/queue-name`, `namePrefix="gpustack-worker"`, `failurePolicy:Fail`). **Mutating**: per container + manufacturer, default an absent `.sliced.cores-percentage` to 100, and fold `.sliced.memory-percentage`(×16000) / `.sliced.memory-mib`(×`M/VRAM`) into per-card `.sliced.units` — only when `.sliced.units` is absent (no double-write vs Instance controller); memory-percentage wins over memory-mib. **Validating**: reject a `.sliced` request that has no memory (neither percentage nor mib, with or without a hand-set `.sliced.units`), sets **both** memory keys, carries any non-positive `.sliced.*` value, or has `memory-percentage > 0` with `cores-percentage < memory-percentage`; also enforce **mode exclusion** — a Pod may request only one of exclusive (`<base>`) / shared (`<base>.shared`) / sliced (`<base>.sliced`), the three cannot coexist in one Pod. **VRAM source** (**revised T5.4d, 2026-07-01**) is the operator-owned ClusterQueue's `note.gpustack.ai/memory`, reverse-looked-up by the `schedule.gpustack.ai/queue-entrance` label the InstanceTypeReconciler stamps with the fronting LocalQueue name, read with the cache-not-ready APIReader fallback — the namespaced LocalQueue is user-writable and must not be trusted as the VRAM divisor. **Ordering** has two guarantees. Kueue builds the Workload — and its credit demand — in its *reconciler* from the **persisted** Pod (after admission), so `.sliced.units` is always present before Kueue accounts quota, independent of webhook order. But Kueue's Pod *mutating* webhook also reads container resources at admission (it hashes them into a role annotation), so our mutating webhook must run **before** Kueue's: the API server runs mutating webhooks serially in lexicographic order of the `MutatingWebhookConfiguration` object name, and `gpustack-worker-mutation` sorts before `kueue-mutating-webhook-configuration` (`g` < `k`). This ordering is **implicit in the name prefix** — a prefix at/after `kueue-` would silently flip it, so the invariant is pinned by a comment at the prefix in `webhooks/setup.go`. **Generator patch** (`webhook-gen`): a `namePrefix=` marker option + a core-group (`APIGroups[0]==""` → `core`) name segment so the Pod webhook gets a valid, collision-resistant name. **(Corrected T4.1, 2026-07-01, e2e finding:** the prefix is inserted **after** the `mutate`/`validate` segment — name `{mutate,validate}.gpustack-worker.core.v1.pod`, serving path `/{mutate,validate}-gpustack-worker-core-v1-pod`. The serving path is derived from the webhook name, and the aggregated apiserver authorizes only paths that start with `/mutate-`/`/validate-`; a **leading** `gpustack-worker` segment produced `/gpustack-worker-mutate-core-v1-pod`, which the server rejected as `anonymous cannot post path`, so under `failurePolicy: Fail` **every** queue-labeled Pod failed to create — the ordering-relevant name is the `MutatingWebhookConfiguration` object name `gpustack-worker-mutation`, unaffected by this reorder.**) **Remove `NodeFeatureWebhook`**; webhook set becomes {InstanceWebhook, PodWebhook}. |
| F4b | AdmissionCheck + ledger completeness | New AdmissionCheck controller: after quota reservation, reads `Devices` CR per-card; `Retry`/`Reject` requests that can't be placed (no clean whole card for exclusive / no single card fits). **Ledger completeness**: the device-plugin `Allocate` writes **every** allocation — Kueue-routed or not — into the `Devices` CR `AcceleratorAllocation`, so even Pods that bypass Kueue land in the unified ledger; the AdmissionCheck consults this complete ledger (closes the "non-Kueue bypass" gap). |
| F4c | Instance slice API → memory/compute percentages | `InstanceResources` renames `AcceleratorUnits int32` → `AcceleratorSlicedMemoryPercentage int32` and adds `AcceleratorSlicedCoresPercentage int32` — both in `[0,100]`, where `0` disables slicing (the request becomes an exclusive whole-card request). **InstanceWebhook** defaults (mutating): when exactly one of the two percentages is `>0`, copy it to the other (a bare memory request yields an equal compute share); validates: each in `[0,100]`, and `cores-% > 0 && cores-% < memory-%` → reject. **InstanceReconciler** `getResourceRequirements`: the sliced branch (`Sliced > 0 && memory-% > 0`) emits `.sliced` + `.sliced.memory-percentage` + `.sliced.cores-percentage` (the Pod webhook folds memory-% into `.sliced.units`) instead of a pre-computed `.sliced.units`; a `0%` request falls through to exclusive whole-card emission. The `instType.Spec.Sliced > 0` gate becomes `instType.Spec.Sliceable` once T5.1 reworks the InstanceType API. **Orphaned by this change (remove in the Phase 5 cleanup):** `nodefeature.QuantityToAlignedValue`/`QuantityToOriginalValue` (+ their tests) lose their last caller now that units are folded from memory by the Pod webhook. |
| F4d | LocalQueueReconciler → NodeQueueEntranceReconciler | Rename the reconciler and its file; it creates one LocalQueue per non-system Namespace pointing at the ClusterQueue. **Revised T5.4d (2026-07-01):** the LocalQueue carries **no** descriptive note — the note-copy (`assembleLocalQueueNotes`) is removed. The per-card VRAM lives only on the operator-owned ClusterQueue, which the Pod webhook reverse-looks-up by the `schedule.gpustack.ai/queue-entrance` label (F5d); a namespaced LocalQueue is user-writable and cannot be trusted as the VRAM source. |
| F5a | InstanceType API + producer | Replace `InstanceTypeAccelerator.Sliced int64` with a `Sliceable bool` capability flag (no slice count). Reshape `InstanceTypeStatus` to the three views `Accelerator`/`AcceleratorShared`/`AcceleratorSliced` (each `InstanceTypeResource`), **dropping** `RAM`/`LocalStorage` (no `Sliced*Percentage` display fields). **Producer:** NodeFlavorReconciler reads the most-constrained node's same-named `Devices`, takes the matching group's first accelerator, and notes `sliceable=true` when its hardware `MaxPartitions != 0`; NodeQueueReconciler carries the `sliceable` note from the flavors onto the ClusterQueue. **Forced consumers:** the InstanceWebhook/InstanceReconciler `Spec.Sliced > 0` gates become `Spec.Sliceable`, and the InstanceWebhook stops capping RAM/local-storage requests (those Status caps are gone — only a negative request is rejected). `make generate` regenerates deepcopy/protobuf/CRD/conversion/apiservice. |
| F5b | Three-view aggregation (materialized into `.status`) | The per-card bin-packing math is unchanged — per node `Accelerator` = free whole cards (`Remaining >= D`), `AcceleratorShared` = Σ over free/Shared cards of `Remaining/(D/10)` ownership shares, `AcceleratorSliced` = Σ over free/Sliced cards of `Remaining/(D/100)` VRAM-percent units; Remaining sums across nodes, OnceMaxRequest is the largest single node, Capacity is the whole pool (cards / ×10 / ×100); non-accelerated types get the CPU view from the CQ nominal/reservation (OnceMaxRequest = the largest node's core count parsed from a flavor name). **Direction-2 change (2026-07-01):** this no longer runs on the extension-API read path (`convertInstanceTypeFromClusterQueue`, T5.2 — now superseded); the same computation runs in the `InstanceTypeReconciler` (F5d), which watches the `Devices` CR + ClusterQueue and writes the result into `InstanceType.status` (DeepEqual-guarded), so watch consumers see it. The pool's Devices are reverse-looked-up by the queue's own feature-key + `kubernetes.io/os\|arch` labels plus `gpustack.ai/managed=true`. `Spec.Sliceable`/`Spec.Memory` come from the queue's `sliceable`/`memory` notes. **Deletes the now-dead `ParseNodeProfile`/`NodeProfileSpec` (+ `isUnsignedDecimal` and the profile consts).** |
| F5c | InstanceType promoted to a real CRD (writable) | **Direction-2 change (2026-07-01), superseding the writable aggregated APIService (T5.3):** InstanceType becomes an etcd-backed CRD with a `/status` subresource, registered like `Instance`/`Devices` (not an aggregated APIService — the projection cannot serve correct incremental watch; see the Risks entry + the OnWatch report) — i.e., the real type moves to `api/worker/v1alpha1/instance_type.go` (`+k8s:crd-gen…subResources=["status"]`) while `api/worker/v1` keeps a proxy type + conversion and `InstanceTypeHandler` is reduced to a `WithCurdProxy` v1→v1alpha1 (mirroring `InstanceHandler`). A new top-level `InstanceTypeSpec.LocalStorage` field carries ephemeral storage (unit spec had only `unitCPU`/`unitRAM`). A **validating webhook** owns admin-write validation (the `extractResourceNotes` rules migrate here, now all-or-nothing: an unset unit spec is accepted — derived pools leave it empty — but a set one must have `unitCPU` a unitless positive integer and `unitRAM`/`localStorage` a case-sensitive `Gi` suffix stored bare; validating-only, no defaulting needed). The **`InstanceTypeReconciler`** (F5d) reconciles `Spec → ClusterQueue`: create carries the InstanceType's metadata (name + labels + annotations, incl. the admin's schedule labels) onto the CQ and notes the validated unit spec; update merges the unit spec via `NoteResource` (operator-owned descriptive notes/quota untouched); **delete uses a finalizer** — it sets `StopPolicy=HoldAndDrain` and holds the InstanceType until NodeQueueReconciler has drained + removed the CQ (and its LocalQueues), then clears the finalizer. The **reconciler-vs-admin contract** stays presence-based: `assembleClusterQueueNotes` leaves a present unit spec untouched, deriving only when absent, so admin values are authoritative; drain is permanent only under `instance-type-derived-from-node=false`. The CQ-projection logic in `pkg/worker/extensionapis/worker/instance_type.go` is replaced by that thin proxy handler; the projection (three-view + spec→CQ) is reborn in the reconciler (F5d) + webhook. The `+genclient` client now targets the CRD. |
| F5d | InstanceTypeReconciler (existence authority + spec→CQ + status) | New controller `pkg/worker/controllers/worker/instancetype.go`; **For** InstanceType (own-name) with a finalizer; **Watches** `ResourceFlavor`, `ClusterQueue`, the `Devices` CR, **and the node-devices `AdmissionCheck`** (absorbed from NQR — enqueues every InstanceType so an accelerated derived queue acquires the check ref once it is Active), mapping each to its InstanceType by the shared feature-key + `kubernetes.io/os\|arch` labels. **ITR is the sole owner of the backing CQ** (NQR removed — its align + drain/delete fold in here — F3b): (1) **InstanceType → CQ** — `ensureClusterQueue` create/syncs the name-identical CQ: it builds the resource groups from the pool's flavors (accelerated → `credits` = Σ `.capacity`×M; non-accelerated → `cpu`) **and** carries the InstanceType's schedule labels + descriptive notes + a `NoteResource` merge of the admin unit spec (present→authoritative; with no flavors the existing descriptive notes/quota are preserved, not cleared); (2) **derived existence** — when `instance-type-derived-from-node=true`, on a `ResourceFlavor` for a pool with no InstanceType, auto-create the InstanceType tagged with a `derived` marker so the pool stays visible; ITR deletes only its own `derived` InstanceTypes when the pool's RFs vanish (admin-created ones are admin-owned). **The `For(InstanceType)` watch must fire on the object's own removal too (corrected T5.4b, 2026-07-01, e2e finding): a `DeleteFunc` that dropped delete events made derived existence edge-triggered — a derived InstanceType deleted while its flavor persists was not re-authored until an unrelated ResourceFlavor/Devices event or an operator restart. Reacting to the delete re-authors it immediately (a no-op when the flavor is gone too, so it never loops), which is what makes derived existence genuinely level-based;** (3) **status** — compute the three-view (F5b math) from Devices+CQ and write `InstanceType.status`, **DeepEqual-guarded** so a no-op change writes nothing (churn lands on our CRD, never Kueue's CQ), and refresh the hardware-descriptor **spec** fields (Acceleratable/Manufacturer/Memory/Sliceable/…) from the queue notes — those stay in Spec as the type's identity the Worker Gateway groups on, so the operator authors them while the admin unit spec is preserved (decision 2026-07-01); (4) **teardown** — a finalizer sets the CQ `StopPolicy=HoldAndDrain` on InstanceType delete, waits until Kueue has drained it (no reserved/admitted workloads), **deletes the CQ itself** (drain+delete absorbed from NQR), then clears the finalizer. Idempotent, level-based. |
| F6 | Worker Gateway sync | `pkg/workergateway/service/{types.go,helper.go}` aggregation types track the three-view contract: the overview/candidate `RAM`/`LocalStorage` dimensions become `AcceleratorShared`/`AcceleratorSliced`, fed from `Status.AcceleratorShared`/`Status.AcceleratorSliced`. Same tier/bundle aggregation, no business-logic change. The gateway's `SharedIndexInformer` on InstanceType now watches a real CRD, so its cache reflects `.status` three-view changes natively (the projection's OnWatch could not emit `Devices`-driven changes — F5c). |
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
- The three-view is **materialized into the InstanceType CRD `.status`** by the reconciler, **not** served live
  from a ClusterQueue projection: correct incremental watch (resume / `410 Gone` / bookmarks) is defined against a
  versioned store, and the extension-API framework drops watch events whose `resourceVersion` does not advance
  (`pkg/extensionapi/storage.go`, `watcher.go`) — a borrowed-CQ-RV projection cannot satisfy it.
- `M = D` keeps the `.sliced.units → credits` factor `= 1`, so `.sliced.units` allocatable *is* the credit value
  (one fewer magic number) — preserve that identity (carried over from the shipped credit-base spec).
- `cores-percentage` (`count×51200`) and `memory-*` are **gate-2 node-level resources**, **not** ClusterQueue
  quota — the CQ has exactly one resourceGroup with one `credits` resource.
- The backing ClusterQueue **keeps** its full metadata even though the InstanceType CRD is now the display
  source of truth: the `instancetypes` resourceType marker, the descriptive notes
  (`acceleratable`/`manufacturer`/`product`/`family`/`memory`/`sliceable`), **and** the admin unit notes
  (`unitCPU`/`unitRAM`/`localStorage`). **Reminder:** the unit notes are written onto the CQ deliberately so a
  **future Pod webhook can run a resource-level (CPU / RAM / local-storage) admission check at Pod-create time**;
  the descriptive notes still feed the Pod-webhook VRAM path — **revised T5.4d (2026-07-01):** read from the
  operator-owned CQ via the `queue-entrance` reverse lookup, not the user-writable LocalQueue (F4d/F5d). Only the
  split between two reconcilers goes away (F3b/F5d) — the notes stay.

### Boundaries
- **Always:** keep credits integer-valued and Kueue-internal; keep the InstanceType API surfacing card/slice
  counts (now three views); floor every memory→units conversion; `spec.nodeLabels` must pin
  `kubernetes.io/os`+`kubernetes.io/arch`; run `make lint` + `go test` on changed packages and re-run e2e.
- **Ask first:** any change to `SlicedResourceMaxSize` / the device-plugin resource set / the soft-slicing
  runtime-isolation injection; any change that would let credits become fractional where Kueue's `ResourceValue`
  ceils it.
- **Never:** fork Kueue; push `credits` as a real node/Pod resource; create a `Cohort`; let an auto-created CQ
  carry a non-empty `spec.cohortName` (keep it cohort-isolated — with **no** borrow/lend limit, which Kueue forbids on a cohort-less queue); derive the three-view display
  from the credits scalar; materialize the three-view into the ClusterQueue's annotations (couples pod-churn writes
  into Kueue's scheduling object — materialize into the InstanceType CRD `.status` instead); remove `os`/`arch`
  from RF/CQ names or from `spec.nodeLabels`.

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
  empty (manual edits reverted) (F3b'); no Cohort = no borrowing channel — and Kueue forbids a borrow/lend limit on a cohort-less queue, so the empty cohort is the sole (and sufficient) isolation.
- **`M` migration breaks credit-base tests/readers** → grep all `CreditsPerCard`/`CardsToCredits`/
  `CreditsToCards`/`ResourceMaxUnits` callers; rescale fixtures; `M=D` identity preserved.
- **Removing `os/arch` from gKey loses anti-collision** → relocate to RF/CQ names + `spec.nodeLabels` os/arch
  pins; no Cohort means no cross-ISA borrowing channel either.
- **AdmissionCheck verdict for transient infeasibility** → AdmissionCheck only validates (no preemption —
  shelved); return `Retry` (re-checked as capacity frees, bounded by Kueue backoff), not `Rejected`, for "no
  clean card right now".
- **A CQ referencing a missing/inactive AdmissionCheck turns inactive** (its workloads become Inadmissible,
  `cache/scheduler/clusterqueue.go:290-294`) → apply the AC object right after the operator installs Kueue, set its `status.Active=True`
  with an operator reconciler, and gate the CQ `admissionChecksStrategy` ref on the AC being `Active`
  (`NodeQueueReconciler` watches the AC to add the ref once active).
- **Three-view watch-invisible from a projection** (an aggregated InstanceType borrows the ClusterQueue's
  `resourceVersion`, which does not advance on `Devices`-only allocation changes; the extension-API framework also
  drops non-advancing-RV events) → informer-cache consumers (the Worker Gateway; the operator's admission path,
  which reads InstanceType via the cached client) serve **stale** accelerator capacity → **promote InstanceType to
  a real CRD and materialize the three-view into `.status`** (native watch/RV). Rejected: writing the view into the
  ClusterQueue's annotations (couples pod-churn into Kueue's CQ → reconcile churn + optimistic-concurrency
  contention). Severity is real but soft on admission (the `OnceMaxRequest` webhook check is a soft gate, backstopped
  by gate-3; `InstanceReconciler` gates on the CQ-derived `Phase`). See
  `.claude/reports/instancetype-onwatch-devices-staleness.md`.

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
pkg/devicemanager/detector/detector.go             # F4b' stamp the feature-key + os/arch selector labels onto the Devices CR (NOT managed — synced by NodeDevicesReconciler)
pkg/deviceplugin/helper.go, server.go              # F2 SliceRatio split (cores vs VRAM)
pkg/devicemanager/allocator/nvidia/deviceplugin.go # F2 SM from cores-%, VRAM from memory-mib (Ascend equivalent)
pkg/worker/controllers/worker/nodecapacity.go      # F1b NodeCapacityReconciler: 4 keys, drop partitions gate
pkg/worker/controllers/worker/resourceflavor.go    # F3a → NodeFlavorReconciler (label-indexed, capacity labels)
pkg/worker/controllers/worker/clusterqueue.go      # F3b → NodeQueueReconciler (credits from RF labels; no CPU/RAM/storage on accel); F4b admissionChecksStrategy ref; Direction 2: align-only, CQ creation moved to ITR (F5d)
pkg/worker/controllers/worker/cohort.go            # F3c DELETE CohortReconciler
pkg/worker/controllers/worker/setup.go             # deregister cohort; register node-devices + admissioncheck reconcilers
pkg/worker/kuberess/apps_kueue.go                  # F3d factors 1,600,000/160,000/1 + exclude gate-2 resources; F4b apply the AdmissionCheck object after the Kueue install
pkg/worker/webhooks/worker/pod.go (new), setup.go  # F4a add PodWebhook, REMOVE NodeFeatureWebhook (→ {Instance, Pod})
pkg/worker/controllers/worker/nodedevices.go (new)          # F4b' NodeDevicesReconciler: sync gpustack.ai/managed from Node onto the Devices CR
pkg/worker/controllers/worker/nodedevicesadmission.go (new) # F4b NodeDevicesAdmission: per-card feasibility check + activate the AC object
api/worker/v1alpha1/instance_type.go (new)         # F5c real CRD (Spec unit-spec+LocalStorage, three-view Status; +k8s:crd-gen subResources=["status"])
api/worker/v1/instance_type.go                     # F5a Sliced int64→Sliceable bool; three-view Status; F5c becomes a proxy type + conversion (real CRD in v1alpha1)
pkg/worker/extensionapis/worker/instance_type.go   # F5c reduced to a WithCurdProxy v1→v1alpha1 (CQ-projection logic moves to the reconciler + webhook)
pkg/worker/controllers/worker/instancetype.go (new)         # F5d InstanceTypeReconciler: spec→CQ sync + finalizer; three-view → .status (watch Devices + CQ)
pkg/worker/webhooks/worker/instancetype.go (new)            # F5c InstanceType validating webhook (unit-spec validation migrated from extractResourceNotes)
pkg/worker/settings/value.go                       # F0c three env switches
pkg/workergateway/service/types.go                 # F6 three-view contract (+ helper.go)
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

// Pooling is a two-stage min-of-mins aggregation, each stage integrating information:
//   Node → ResourceFlavor: NodeFlavorReconciler picks the min-capacity node among the RF's indexed
//     nodes and derives the default unit spec (unitCPU/unitRAM/localStorage) into RF notes.
//   ResourceFlavor → ClusterQueue: NodeQueueReconciler picks the min unit spec among the RFs feeding
//     the CQ (unless the admin already set it via the InstanceType API). Both stages compare
//     non-positive-skipping, and compare bare numeric strings via stringx (no resource.Quantity).
```
### Implementation Plan
Dependency order = the six phases of the Proposal, consolidated for review into **seven feature commits**
(one per phase, Phase 5 absorbing the Phase-6 Gateway sync) followed by the fixes and e2e work surfaced during
the live run. Task IDs map to Core Features F0a–F7. Each entry is a vertical slice that leaves the tree
building; a `make lint` + `go test ./pkg/...` checkpoint sits between phases.

- [x] **Commit 1 (Phase 0, F0a/F0b/F0c/F0d) — foundation & contract.** Rescale the credit base to
  `M = 1,600,000` (`2⁹×5⁵`) so the memory-1% step `M/100 = 16000` is integral while `SlicedResourceMaxSize = 512`
  still divides and the `M = D` identity keeps the `.sliced.units` credit factor at 1; add the
  `.sliced.cores-percentage` / `.sliced.memory-percentage` / `.sliced.memory-mib` resource-name constants +
  constructors; register the three runtime switches (`node-management-manual`=false,
  `instance-type-mixed-on-node`=true, `instance-type-derived-from-node`=true), read per-reconcile via
  `ShouldValueBool`; settle `memory`/`localStorage` as the canonical RF/CQ note keys (plain literals, no
  dedicated module). **Accept:** `M%10==0 && M%512==0 && M%100==0`, round-trips hold; each switch resolves from
  its `GPUSTACK_*` var with the right default. **Verify:** `go test ./pkg/nodefeature/... ./pkg/worker/settings/...`.
  *(Two commits: nodefeature rescale+keys, then settings+note-keys.)*
- [x] **Commit 3 (Phase 1, F1a/F1b/F1c) — discovery.** SoftPartition `MaxPartitions = 512` (NVIDIA inlined,
  Ascend 910B/910C/950), so `.sliced` node capacity = `cards×512`; `NodeCapacityReconciler` advertises the four
  per-card capacities (`units=cards×M`, `cores-percentage=cards×51200`, `memory-percentage=cards×100`,
  `memory-mib=Σ cards×VRAM`) and drops the `.sliced.partitions` opt-in gate; `node-management-manual` gates the
  `gpustack.ai/managed=true` injection (honoring an explicit admin label), read per-reconcile. **Accept:** a GPU
  node advertises all four `.sliced.*` capacities; switch ① applies without restart. **Verify:**
  `go test ./pkg/worker/controllers/worker/... ./pkg/nodefeature/...`.
- [x] **Commit 4 (Phase 2, F2) — allocation.** Decouple sliced SM from VRAM in both allocators:
  `SlicedCoresPercent` drives `CUDA_DEVICE_SM_LIMIT` / the Ascend aicore-quota (default 100); `SlicedMemoryMib`
  drives the per-card VRAM limit (memory-percentage over memory-mib, floored, capped, errors if neither); delete
  the now-orphaned `SliceRatio`/`FloorPercent`/`PadSlicedUnits`. **Accept:** a sliced container gets independent
  SM and VRAM limits (SM% ≠ VRAM%). **Verify:** `go test ./pkg/deviceplugin/... ./pkg/devicemanager/allocator/...`.
- [x] **Commit 5 (Phase 3, F3a/F3b/F3c/F3d) — pooling rewrite + delete Cohort (tightly coupled).**
  `NodeFlavorReconciler` indexes managed nodes by flavor name and stamps the RF with node-feature labels (feature
  key, full `kubernetes.io/os|arch`, per-key `.count`/`.capacity` = contributing nodes × count) + a blanket
  `{Operator: Exists}` toleration + the derived unit spec / per-card VRAM in notes; a flavor with no contributing
  node is deleted (no drain tombstone). `NodeQueueReconciler` aggregates flavors sharing `(key,os,arch)` into one
  isolated ClusterQueue (empty `cohortName`); credits = `capacity×M` (accelerated) or `cpu = capacity` cores
  (non-accelerated). `CohortReconciler` and the `z-cohort` machinery are removed. The unit spec derives via a
  two-stage min-of-mins aggregation (Node→RF→CQ), skipping non-positive values, comparing bare numeric strings
  via `stringx.CompareNumeric` (no `resource.Quantity`); the CQ notes are never clobbered when an admin unit spec
  is present. **Accept:** RF/CQ names + labels per spec, credits correct, **zero Cohort objects**, unit spec =
  min-across-flavors (or admin-set). **Verify:** `go test ./pkg/worker/controllers/worker/... ./pkg/worker/kuberess/...`.
- [x] **Commit 6 (Phase 4, F4a/F4b/F4b'/F4c/F4d) — admission safety layer.** Add the `PodWebhook` (core Pods,
  objectSelector on `kueue.x-k8s.io/queue-name`, `failurePolicy: Fail`): mutating defaults `cores-percentage=100`
  and folds `memory-percentage`/`memory-mib` into `.sliced.units` when absent (memory-% wins); validating rejects
  no-memory / both-memory-keys / non-positive `.sliced.*` / `cores-% < memory-%` / mixed modes. The Instance API
  gains `AcceleratorSlicedCoresPercentage` + `AcceleratorSlicedMemoryPercentage`; the webhook set becomes
  `{Instance, Pod}` and the mutating config name sorts before `kueue-mutating-webhook-configuration`.
  `NodeDevicesAdmissionReconciler` adds a per-card feasibility AdmissionCheck (`Retry` when no node has a free
  whole card — the ledger seeds every card at `ResourceMaxUnits`, so exclusive over-admit is caught exactly;
  check-only, read uncached via `APIReader`); `installKueue` applies the AC object, its controller keeps it
  `Active`, and the accelerated CQ references it only once Active. `NodeDevicesReconciler` syncs
  `gpustack.ai/managed` from the Node onto the same-named `Devices`. **Accept:** a memory-% Pod gets
  `.sliced.units`; a no-memory `.sliced` Pod is rejected; on a fully-sliced 8-card node the 5th exclusive is held
  by `Retry`. **Verify:** `go test ./pkg/worker/... ./pkg/nodefeature/... ./gen/...` + `make generate` + `make lint`.
- [x] **Commit 7 (Phase 5+6, F5a/F5b/F5c/F5d/F6) — materialize InstanceType as a CRD.** InstanceType becomes a
  real `v1alpha1` CRD (status subresource) whose Spec carries the admin unit spec + `LocalStorage` and whose
  Status carries the three views (`Accelerator`/`AcceleratorShared`/`AcceleratorSliced`, replacing the old
  `Sliced int64`/`RAM`/`LocalStorage`); `api/worker/v1` is a proxy type + conversion and `InstanceTypeHandler` a
  `WithCurdProxy`. `InstanceTypeReconciler` is the **sole owner** of the backing ClusterQueue: `ensureClusterQueue`
  builds the resource groups from the pool's ResourceFlavors (admin unit spec wins, else derived), authors a
  derived InstanceType under `instance-type-derived-from-node` (removing only its own derived ones when the RFs
  vanish), materializes the three-view from the `Devices` per-card ledger (DeepEqual-guarded) and the CPU view
  from the CQ, and its finalizer drives the CQ through `HoldAndDrain` → delete; `NodeQueueReconciler` is folded in
  and removed. An `InstanceTypeWebhook` validates the all-or-nothing unit spec (`unitCPU` unitless +int,
  `unitRAM`/`localStorage` case-sensitive `Gi`). The Pod-webhook VRAM divisor is anchored on the operator-owned CQ
  via a `schedule.gpustack.ai/queue-entrance` label + `status.entrance` (no longer the user-writable LocalQueue).
  The Worker Gateway tracks the three views. **Accept:** the five-step oracle reproduces `8/80/800 → … → 1/28/256`;
  `kubectl get/create/edit/delete instancetype` is served by the CRD with a native watch; deleting drains then
  removes both objects. **Verify:** `make generate` + `make lint` (0 issues) + `go test ./pkg/...`.
- *Checkpoint (each phase): `make lint` clean + `go test ./pkg/...` green; e2e `case-1..6` pass on the rebuilt image.*

**Post-build consolidation — fixes surfaced during the live e2e run + test infra.** Five standalone fixes and
the e2e redesign follow the seven feature commits, kept as their own commits for a clean review trail (detail in
"Discovered during the live e2e run"):
- `fix(chart)` — strip `instancetypes` finalizers during chart/e2e cleanup, so an InstanceType no longer hangs `Terminating` when the operator is already gone.
- `fix(worker)` — keep the Pod webhook serving path standard-prefixed (`/{mutate,validate}-gpustack-worker-core-v1-pod`); a leading prefix was rejected by the aggregated apiserver.
- `fix(worker)` — admit Instance Workloads via kueue `quotaCheckStrategy: IgnoreUndeclared`, so single-dimension queues no longer reject the Pod's other resources (subsumes the gate-2 drains).
- `fix(webhook)` — skip defaulting + update-validation while an object is deleting (a `ReceiveDeletionUpdate` marker opts out), so a finalizer-clearing update is never denied and the object cannot deadlock `Terminating`.
- `fix(worker)` — stop a running Instance when its type drains, evaluated on every reconcile before (re)creating the Pod, with an `InstanceType` watch enqueuing by `.spec.type`.
- `test(e2e)` — redesign the six operator e2e cases around the user stories (see the Test Plan).

### Test Plan
[ ] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates
- Rescale base-dependent fixtures for the `M=1,600,000` migration (Commit 1), wider than first scoped: `knowns_test` /
  `apps_kueue_test` (factors) / `instance_type_test` (reservation inputs) / `controllers/worker/instance_test`
  (`.sliced.units`) / `deviceplugin/helper_test` (`PadSlicedUnits`/`SliceRatio`/`PartitionedUnitGranularity`) /
  `allocator/{nvidia,ascend}/deviceplugin_test` (`slicedPod` units). `clusterqueue_test` is already base-relative
  (`cards × CreditsPerCard`). (The `deviceplugin/helper_test` trio is later deleted in Commit 4 as orphan cleanup.)
- Add missing test files: `pkg/devicemanager/detector/nvidia` (none today) and `pkg/worker/settings` (none today).
- Rescale `e2e case-5` credit assertions to the new base before reusing it.

#### Unit tests
Every added unit gets coverage; target ≥ existing, no regression. Per-package (date 2026-06-29):
- `pkg/nodefeature`: new suffix constructors; `M` invariants (`%10`/`%512`/`%100`); `CardsToCredits`/`CreditsToCards` round-trips; gKey without os/arch abbrev.
- `pkg/devicemanager/detector/nvidia`: **NEW** — SoftPartition `MaxPartitions=512`.
- `pkg/worker/controllers/worker`: `nodecapacity_test` (4 sliced keys + stale-clean); `nodeflavor_test` (names + `capacity` label + index rules + nodeLabels os/arch pin); `nodequeue_test` (credits=`capacity×M`; accel has no CPU/RAM/storage; `cohortName` empty + **no** borrow/lend limit — Kueue forbids one on a cohort-less queue); **NEW** `nodedevices_test` (managed-label sync Node→Devices) + `nodedevicesadmission_test` (per-card feasibility: 8×50% → exclusive=0; clean-card shortage → `Retry`; Devices located by `flavor.spec.nodeLabels`); **DELETE** `cohort_test`.
- `pkg/worker/kuberess`: `apps_kueue_test` (factors `1,600,000/160,000/1`, `multiplyBy`, only the three credits rules remain, and `quotaCheckStrategy: IgnoreUndeclared` is set unconditionally — no gate-2 drains).
- `pkg/deviceplugin` + `pkg/devicemanager/allocator/nvidia`: `SliceRatio` split; `SM←cores-%`, `VRAM←memory-mib`; `cores-%` default 100.
- `pkg/worker/webhooks/worker`: **NEW** `pod_test` — fold `memory-%`/`memory-mib`→`units`, default cores-%=100, reject (no memory / both memory keys / non-positive / `cores-% < memory-%` / mixed exclusive+shared+sliced modes), no double-write when `.sliced.units` present; **updated** `instance_test` — percentage default-equate + range/`cores-% ≥ memory-%` validation; `instance` reconciler conversion to `.sliced.memory-percentage`/`.sliced.cores-percentage`.
- `pkg/worker/controllers/worker`: **NEW** `instancetype_test` — the 5-step three-view oracle from the `Devices` CR (`8/80/800 → … → 1/28/256`), per-node rollup (Remaining sums, OnceMaxRequest = largest node), CPU view from the CQ, `Sliceable`/`Memory` from notes, draining→zero, reverse-lookup selector; the DeepEqual-guarded `.status` write (a no-op change writes nothing); spec→CQ Create/Update `NoteResource` merge + Delete finalizer/HoldAndDrain ordering.
- `pkg/worker/webhooks/worker`: **NEW** `instancetype_test` — unit-spec validation migrated from `extractResourceNotes` (all three required; `unitCPU` unitless +int; `unitRAM`/`localStorage` case-sensitive `Gi`, stored bare).
- `pkg/worker/settings`: **NEW** — the 3 settings map from their `GPUSTACK_*` vars with correct defaults (`node-management-manual`=false; `instance-type-mixed-on-node`/`instance-type-derived-from-node`=true) and are read per-reconcile via `ShouldValueBool(ctx)` (runtime-adjustable, no restart).

#### Integration tests
- Fake-client controller tests exercise the `NodeFlavor → NodeQueue → InstanceType` chain in-process (asserting **no** `Cohort` is created).
- `AdmissionCheck` × `Devices` CR per-card feasibility, table-driven: clean-card shortage vs generic headroom (the 8×50%-sliced case yields exclusive=0).

#### e2e tests
Under `.claude/skills/gpustack-operator-e2e/cases/`, **one case per User Story** (accelerated cases run
GPU-less by approximation — a fake accelerator NodeFeature + a phantom-node `Devices` ledger patched on
the **v1alpha1** CRD `/status`):
- **`case-1`** (Story 1/2 baseline) — the CPU-only chain materializes: ResourceFlavor → InstanceType
  (`Active`, with an entrance LocalQueue) → one isolated ClusterQueue → LocalQueue, and **zero**
  `Cohort` objects.
- **`case-2`** — a running Instance whose InstanceType drains is **stopped** (`spec.stop=true`), not
  recreated.
- **`case-3`** (Story 5) — `gpustack.ai/managed=false` (via the NodeFeature) **deletes** the node's
  ResourceFlavor (no drain tombstone) and tears down the derived InstanceType (backing CQ
  `HoldAndDrain`/removed via the finalizer).
- **`case-4`** (Story 4) — 8 cards each 50%-sliced; a 5-exclusive request passes coarse `credits` but
  is held by the node-devices `AdmissionCheck` (`Retry`, not admitted-then-unschedulable); also asserts
  the AC is `Active` and the accelerated CQ references it.
- **`case-5`** (Story 3) — the Pod webhook folds `.sliced.memory-percentage` into `.sliced.units`
  (`× M/100`) and defaults `.sliced.cores-percentage` to 100; a memoryless `.sliced` Pod is rejected by
  the validating webhook.
- **`case-6`** (Story 2/6) — on an 8× A10G (24Gi) node the five-step pooling sequence drives the
  three-view `8/80/800 → 6/60/600 → 4/58/400 → 2/38/360 → 2/38/356 → 1/28/256`; asserts **watch
  freshness** via `kubectl get instancetype -w`, a unit-spec change through the InstanceType API
  reaching the CQ notes while touching **no** `Node`/NodeFeature, and **zero** `Cohort` objects.

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
  the Pod mutating webhook for sliced-unit control. Two ordering guarantees: (1) Kueue builds the Workload and
  folds credits in its *reconciler* from the **persisted** Pod (after admission), so `.sliced.units` is always
  back-filled before Kueue accounts quota — a unit-less Pod is never admitted (`failurePolicy: Fail` also closes
  the fail-open gap); (2) Kueue's Pod *mutating* webhook reads container resources at admission (role-hash), so
  ours must run first — the API server orders mutating webhooks by `MutatingWebhookConfiguration` name, and
  `gpustack-worker-mutation` < `kueue-mutating-webhook-configuration`. **Watch out:** the ordering in (2) is
  implicit in the name prefix; a prefix sorting at/after `kueue-` silently reverses it, so the invariant is
  pinned by a comment in `webhooks/setup.go`. The Configuration sets `quotaCheckStrategy: IgnoreUndeclared` so
  each queue counts only its covered dimension and ignores the gate-2 node resources
  (`.sliced.cores-percentage`/`.memory-percentage`/`.memory-mib`), the multiplyBy-only `.sliced`, and any other
  uncovered resource; only `.sliced.units → credits` is counted (F3d).
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

### Resolved during plan review (2026-06-30, round 3 — T4.2 design)
- **AdmissionCheck wiring (what makes gate-3 actually fire)** → a check is evaluated only when (a) an
  `AdmissionCheck` object with our `controllerName` exists **and** is `Active`, and (b) the CQ lists it in
  `spec.admissionChecksStrategy.admissionChecks` (v1beta2 has no plain `admissionChecks []string`). The AC
  **object** is applied by `installKueue` right after the Kueue Helm install (the gpustack chart can't — Kueue's
  CRD is runtime-installed); an operator `For(&AdmissionCheck{})` reconciler sets its
  `status.Active=True`; `NodeQueueReconciler` adds the CQ ref **only after the AC is Active** (empty `onFlavors` =
  all flavors) and watches the AC to add it on activation — a CQ must never reference a missing/inactive check or
  the whole queue goes inactive. The external controller follows the Kueue contract (`provisioning`
  controller as the template): `For(Workload)`, `FilterForController`, `SetAdmissionCheckState` inside
  `PatchStatus` (Devices read uncached via `APIReader`, no watch); `Retry` releases quota + requeues on Kueue's backoff,
  `Rejected` deactivates (we never use it).
- **Candidate `Devices` lookup by label, not a Node join** → the DeviceManager stamps the feature-key + os/arch
  selector labels onto each `Devices` CR (cluster-scoped, named by node) while `NodeDevicesReconciler` syncs the
  `gpustack.ai/managed` mark from the Node, so gate-3 resolves candidates with one
  `List(MatchingLabels: flavor.spec.nodeLabels)` — shorter than `flavor.nodeLabels → List Nodes → Get Devices/node`.
  The managed mark is split out of the DeviceManager because node management is a control-plane decision the
  per-node device-manager must not assert.
- **File name `nodedevicesadmission.go`** (not the generic `admissioncheck.go`) → it admits by reading the
  per-node `Devices` ledger; the name states that.
- **Ledger completeness already holds** → device-plugin `Allocate` writes every allocation below kubelet
  (`server.go:312 → patchAllocatingPod`), independent of Kueue routing; T4.2's ledger work is verification + a
  per-card `Remaining` aggregation test, not new write code.
- **Gate-3 is precise for exclusive, deliberately loose for sliced** → the ledger's per-card `Remaining` is exact
  for whole-card occupancy (any allocation drops a card below `ResourceMaxUnits`, so an exclusive over-admit is
  caught exactly), but the device-plugin records sliced `Allocated` as the injection-token count (`server.go:372`,
  "bookkeeping only"), so a sliced card's `Remaining` overstates its free units. This is fine: the sliced *total*
  budget is gate-1 `credits`' job (`.sliced.units → credits`); gate-3 only adds the per-card placement check that
  exclusive needs. The unified `Remaining ≥ demand` rule stays safe for every mode (sliced stays permissive, yet
  still catches cards fully held by exclusive/other consumers).

### Discovered during the live e2e run (2026-07-01)
- **Single-dimension ClusterQueues make Instance Workloads inadmissible for their other Pod resources —
  RESOLVED (2026-07-01).** A ClusterQueue advertises exactly one coarse admission dimension — `cpu` for
  general pools (F3b), the manufacturer credits for accelerated pools — but every Instance's Workload also
  requests `cpu`/`memory`/`ephemeral-storage` (`instance.go` derives them from `unitResources`/`localStorage`).
  Kueue refuses to assign a flavor for a resource the queue does not cover (`couldn't assign flavors … resource
  memory`), so the Workload never reserves quota. This hit **both** pools: general (memory + ephemeral-storage
  uncovered) and accelerated (cpu + memory + ephemeral-storage uncovered — `case-4`'s bare Pod requested only
  the exclusive resource, so it slipped past). **Fix:** enable Kueue's `QuotaCheckStrategy` feature gate and set
  `resources.quotaCheckStrategy: IgnoreUndeclared` in the deployed Configuration (`apps_kueue.go`), so each queue
  checks only its covered dimension and ignores the rest — the native expression of "Kueue is not the ledger".
  This **subsumes the gate-2 `.sliced.*` drains** (now redundant, removed) and needs no per-resource enumeration
  or added CQ coverage. Available in the deployed Kueue v0.18.1 (alpha, off by default; the vendored `v0.17.1`
  Go client is irrelevant — the Configuration is a raw YAML applied to the external Kueue). Verified live by
  `case-2` (general admission). Fixed in a standalone `fix(worker)` commit.
- **Instance deletion deadlocked when its InstanceType was gone — RESOLVED (2026-07-02).** The
  InstanceReconciler clears its `gpustack.ai/controlled` finalizer with an Update, which runs the Instance
  mutating + validating webhooks; those re-size the Pod from the InstanceType, so once the type was drained
  (`spec.type: Not found`) or rebuilt with an empty unit spec the webhook denied the finalizer-clearing update
  and the Instance was stuck `Terminating` forever (surfaced by `case-2`'s drain teardown). **Fix:** guard all
  webhooks in `pkg/webhook/helper.go` to skip defaulting and update-validation once the object carries a
  deletion timestamp (a handler may implement the `ReceiveDeletionUpdate` marker to opt out); the check is made
  once at setup. Verified live by `case-2` (the test Instance now deletes cleanly on teardown). Fixed in a
  standalone `fix(webhook)` commit.
- **A running Instance was not stopped when its InstanceType drained — RESOLVED (2026-07-02).** The
  stop-on-inactive/gone check in `instance.go` sat **inside the `pod == nil` branch** and the
  `InstanceReconciler` did not watch `InstanceType`, so a *running* Instance whose type drained was never
  re-evaluated. Worse, on a drain Kueue evicts the Pod → the reconcile sees `pod==nil` but the type still
  looks `Active` for an instant → it **recreated** the Pod (left stuck `Pending`) instead of stopping
  (traced live: `spec.stop` never flipped in 120s). **Fix:** evaluate the gone/`Inactive` type on every
  reconcile, before (re)creating the Pod, and add an `InstanceType` watch that enqueues Instances by
  `.spec.type`. Verified live by `case-2` (the running Instance reaches `spec.stop=true`/`Stopped` ~13s
  after drain). Fixed in a standalone `fix(worker)` commit.

### Follow-ups (seed for the next spec)
Deferred items surfaced during this build, carried forward as the seed for a follow-up spec:
- **The aggregated `v1` proxy's `/status` subresource write returns `ServiceUnavailable`.** Writing
  `devices`/`instancetypes` status through the aggregated `worker.gpustack.ai/v1` proxy fails; only the real
  `v1alpha1` CRD `/status` serves the write. Low production impact (the DeviceManager and reconcilers write
  status via the typed `v1alpha1` client), but external/`kubectl` status writes to the proxy fail — the e2e
  mocks target `devices.v1alpha1.worker.gpustack.ai` to work around it. Root-cause the proxy handler's empty
  `srs` / status-subresource wiring (`DevicesHandler.SetupHandler`) so the aggregated `/status` path serves.

### Still open (confirm during implementation)
- **Exclude vs drain for gate-2 resources (T3.2)** → **resolved: neither — `quotaCheckStrategy: IgnoreUndeclared`.**
  The live e2e showed the drains were one instance of a broader problem (every uncovered Pod resource blocks
  admission, not just gate-2), so instead of per-key `Replace → empty` drains or an `excludeResourcePrefixes` scan,
  each queue checks only its covered dimension and ignores the rest. The firm requirement stands and is met:
  `.sliced.cores-percentage`/`.memory-percentage`/`.memory-mib` (and `cpu`/`memory`/`ephemeral-storage`) are
  neither counted nor block admission. See the resolved e2e finding above.
- **Reconciler vs InstanceType-write contract (F5c, T5.3)** → **resolved (T5.3): presence-based preservation, no
  override marker.** `assembleClusterQueueNotes` leaves the CQ's unit spec untouched whenever one is already present
  (admin-written via Create/Update) and derives the min-across-flavors only when absent, so admin values are
  authoritative without a `unit-spec-source` annotation. Delete sets `HoldAndDrain` and drains regardless of the
  derived setting; with backing nodes still present it is permanent only under `instance-type-derived-from-node=false`
  (under the default the reconciler re-derives the queue from its flavors — expected level-based behavior).
- **InstanceType watch is blind to `Devices` changes (discovered during build, 2026-07-01)** → **resolved: promote
  InstanceType to a real CRD and materialize the three-view into `.status`** (Direction 2, T5.4/T5.5). A projection
  over the ClusterQueue borrows the CQ `resourceVersion` (unchanged on `Devices`-only allocation) and the
  extension-API framework drops non-advancing-RV events, so the Worker Gateway informer + the operator's cached
  admission reads served a stale three-view. Rejected: writing the view into CQ annotations (couples pod-churn into
  Kueue's CQ). Supersedes the aggregated read/write (T5.1 read path, T5.3); the presence-based reconciler-vs-admin
  contract above carries over to the `InstanceTypeReconciler`. Full analysis:
  `.claude/reports/instancetype-onwatch-devices-staleness.md`.
