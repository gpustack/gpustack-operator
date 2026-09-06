# Spec: Vendor-Aware Slicing Capability in the Unified Pool — Devices `AcceleratorsFeature`, Per-Vendor Max Slices, and Five-Gate Admission

Status: Shipped
Type: Feature

## Summary
The unified accelerator pool (shipped in `2026-06-29-instancetype-unified-pool-refactor`) already folds exclusive
/ shared / sliced into one ClusterQueue with a per-card `Devices` ledger, and soft slicing already has real
runtime isolation for NVIDIA and Ascend (`2026-06-25-accelerator-soft-slicing-runtime-isolation`). But the pool's
*sliced* feedback is coarse and hard-coded: every sliceable card advertises the same fixed `MaxPartitions = 512`,
`.sliced.cores-percentage` is always `cards × 512 × 100`, and the `Devices` model carries slicing capability as a
flat `AcceleratorFeatures{SoftPartition, PhysicalPartition, VirtualPartition, MaxPartitions}` on each
`Accelerator` where only `MaxPartitions` is ever read. This does not reflect reality — the maximum number of soft
slices a single device tolerates differs by vendor (bounded by the vendor runtime's per-device user-process
limit), and some vendors partition compute spatially (no overcommit) while others time-share it (each slice may
claim up to 100 %).

This refactor makes slicing a **first-class, vendor-aware capability on the device group**. It replaces the flat
feature bools with two structured slicing descriptors — `PhysicalSliced` (hardware partition, e.g. MIG) and
`LogicalSliced` (vendor vGPU / `ld.preload` interception) — each carrying the per-device max slice `Size`, a
`CoresPercentageOvercommit` flag, and a `MemoryPercentageStep`. The detector fills a per-vendor `LogicalSliced`;
the per-device max slice count = `max(PhysicalSliced.Size, LogicalSliced.Size)` then drives the allocator's
`.sliced` advertisement (`cards × maxSlices`, not a fixed 512) and the `NodeCapacityReconciler`'s
`.sliced.cores-percentage` (`cards × maxSlices × 100` when compute overcommit is allowed, else `cards × 100`).
`NodeCapacityReconciler` now consults both the Node and the `Devices` CR, and patches the `.sliced.*` keys only
while a manufacturer's `.sliced` resource is actually advertised (reverse-patching when it disappears or reaches
0). `RoCE` moves off the feature struct into the per-card `DeviceTopology`. The `InstanceType` API drops its
`Sliceable bool` for the same structured slicing descriptor, carried through the `NodeFlavor` notes. Finally, the
architecture document's admission narrative is re-drawn as an explicit **five-gate** chain (Pod webhook → Kueue
credits → Kueue AdmissionCheck → default scheduler → DeviceManager allocator).

Enabling `LogicalSliced` detection for all six accelerator vendors makes Cambricon / Hygon / MThreads / MetaX
*advertise* sliced capacity and be admitted as sliceable pools, but only NVIDIA and Ascend have a real
runtime-isolation injection branch today; the other four keep a whole-card passthrough stub (no isolation). That
gap is an explicit Non-Goal here and the seed of a follow-up spec.

## Motivation
### Goals
- **Slicing capability is vendor-aware and lives on the device group.** The `Devices` model expresses, per device
  model, *which* slicing technology is available (physical hardware partition vs. logical software partition),
  the *maximum* number of slices a single device supports, whether compute may be **overcommitted**, and the
  memory-percentage **step**. A single struct (`AcceleratorSliced`) is reused for both physical and logical
  slicing so that adding hard partitioning (MIG / vNPU) later is a natural extension, not a new shape.
- **The per-device max slice count reflects the vendor runtime's real limit**, not a fixed 512:
  NVIDIA 128, Ascend 63, Cambricon 16, Hygon 4, MThreads 16, MetaX 16 (rationale in the Notes table). This count
  is detected by the DeviceManager and fed into `Devices`, then consumed by the allocator and the pool reporting.
- **Sliced resource feedback is derived from Devices, not hard-coded.** The allocator advertises
  `<vendor>/<device>.sliced = cards × maxSlices`; `NodeCapacityReconciler` derives `.sliced.cores-percentage`
  from the overcommit flag; both read the max slice count from the `Devices` CR. A manufacturer with no slicing
  capability advertises **no** `.sliced` resource and gets **no** `.sliced.*` node capacity.
- **`.sliced.*` capacity is presence-gated and self-cleaning.** `NodeCapacityReconciler` patches the four
  `.sliced.*` keys for a manufacturer only while that manufacturer's `.sliced` resource is present and positive in
  the Node capacity; when it disappears or reaches 0 the keys are reverse-patched (removed).
- **The unified pool's three-mode feedback is complete.** Exclusive / shared / sliced mutual-exclusion counting is
  already in place (from the unified-pool refactor); this spec adds the sliced *mode* (physical vs logical, from
  the device features) and the sliced *max count* to the picture the `InstanceType` and `NodeFlavor` surface.
- **The admission model is documented as five explicit gates**, each with a clear responsibility, so operators and
  contributors can reason about where each `.sliced.*` value is produced and consumed.
- **Success criteria (testable):**
  1. On an 8×NVIDIA node, `nvidia.com/gpu.sliced` capacity is `8 × 128 = 1024` and
     `nvidia.com/gpu.sliced.cores-percentage` is `8 × 128 × 100 = 102400` (overcommit); on an 8×MThreads node the
     sliced count is `8 × 16 = 128` and cores-percentage is `8 × 100 = 800` (no overcommit).
  2. A manufacturer whose device group carries no `PhysicalSliced`/`LogicalSliced` advertises no `.sliced` and
     the node shows none of the four `.sliced.*` capacities; removing slicing capability reverse-patches them.
  3. `Accelerator.Features` no longer exists; slicing capability is read from `DevicesGroup.AcceleratorsFeature` and RoCE
     from `Accelerator.Topology.RoCE`.
  4. `InstanceType` carries the structured slicing descriptor (no `Sliceable bool`), populated from the
     `NodeFlavor` notes on derived types.

### Non-Goals
- **Real runtime isolation for Cambricon / Hygon / MThreads / MetaX.** This spec enables their *detection,
  advertisement, and pooling* as sliceable, but their allocator `Sliced` branch stays a whole-card passthrough
  stub (no memory/compute isolation) — deliberately, as a data-model-complete state. Building each vendor's real
  injection branch (driver / config-file / env / sysfs primitives) is a **follow-up spec**.
- **Hard slicing (physical partition).** `PhysicalSliced` stays zero for every vendor except a **placeholder seed**
  on NVIDIA MIG-enabled cards (`{MaxSize: 7, MemoryPercentageStep: 25}`, `TODO`-marked); the real MIG-profile
  detection (exact max size / memory step), allocation, and isolation — and Ascend vNPU dynamic virtualization —
  remain out of scope (a separate MIG / vNPU spec, consistent with the unified-pool refactor's Non-Goal).
- **Changing the credit base, the `.sliced.units` accounting, or the Kueue transformation.** `.sliced.units`
  stays `cards × D` (D = 1,600,000) and the credit fold-down is untouched; only the *coarse* `.sliced` token count
  and the `.sliced.cores-percentage` budget change shape.
- **Changing the NVIDIA / Ascend runtime-isolation mechanism.** The `ld.preload` library injection and the
  per-container quota derivation are unchanged; only the max slice count fed into them changes (512 → 128 / 63).
- **The soft-slicing quota-derivation math per slice.** How a slice's compute/VRAM limit is computed from its
  request is unchanged; this spec changes only the pool-level *capacity* feedback.

## Proposal
Slicing becomes a structured, per-group capability that the DeviceManager detects, records in `Devices`, and the
pool feeds back through the allocator, `NodeCapacityReconciler`, `NodeFlavorReconciler`, and `InstanceType`.

### Data model (Devices)
Introduce one reusable slicing descriptor and restructure the feature block:

```go
// AcceleratorSliced describes one slicing capability of a device model.
type AcceleratorSliced struct {
    // MaxSize is the maximum number of slices a single device can be split into.
    // (named MaxSize, not Size: gogoproto reserves the Size() marshal method, so a
    // field literally named Size collides.)
    MaxSize int32
    // CoresPercentageOvercommit reports whether each slice may claim up to 100% of the
    // device compute (time-sharing / weighted sharing) so the sum across slices may
    // exceed one whole device; false means compute is partitioned (sum ≤ one device).
    CoresPercentageOvercommit bool
    // MemoryPercentageStep is the granularity, in percentage points, at which the
    // device memory can be sliced.
    MemoryPercentageStep int32
}

// AcceleratorsFeature describes the slicing features shared by every accelerator in a group.
type AcceleratorsFeature struct {
    // PhysicalSliced enables physical (hardware) slicing such as NVIDIA MIG — a real
    // spatial partition of cores and memory — when its MaxSize is non-zero.
    PhysicalSliced AcceleratorSliced
    // LogicalSliced enables logical (software) slicing via a vendor vGPU scheme or an
    // ld.preload interception library when its MaxSize is non-zero.
    LogicalSliced AcceleratorSliced
}
```

- **Rename** `AcceleratorFeatures` → `AcceleratorsFeature` and **move** it from `Accelerator.Features` to
  `DevicesGroup.AcceleratorsFeature` (slicing capability is a property of the device *model*, shared by every card in the
  group).
- **Remove** the flat `PhysicalPartition` / `VirtualPartition` / `SoftPartition` bools and the bare
  `MaxPartitions int32`; they are superseded by `PhysicalSliced` / `LogicalSliced` (value structs — a non-zero
  `.MaxSize` *is* the "enabled" flag and the per-mode max partition count; no pointers).
- **Move** `RoCE *DeviceEthernet` from the feature struct into `DeviceTopology` (RoCE is per-card networking, not
  a per-model slicing feature); `RoCE` stays on `Accelerator.Topology`.
- **Per-device max slice count** = `max(PhysicalSliced.MaxSize, LogicalSliced.MaxSize)`.

### Detector settings (per vendor, `LogicalSliced{MaxSize, CoresPercentageOvercommit, MemoryPercentageStep}`)
| Vendor | LogicalSliced | Detection gate |
|---|---|---|
| Ascend | `{63, true, 1}` | families `910B` / `910C` / `950` only (unchanged gate) |
| NVIDIA | `{128, true, 1}` | always; `PhysicalSliced` seeded `{7, false, 25}` (placeholder, `TODO`) when MIG mode is enabled |
| Cambricon | `{16, false, 1}` | when a device group is detected |
| Hygon | `{4, true, 1}` | when a device group is detected |
| MThreads | `{16, false, 1}` | when a device group is detected |
| MetaX | `{16, false, 1}` | when a device group is detected |

### Sliced resource feedback
| Resource | Reporter | Value | Change |
|---|---|---|---|
| `<vendor>/<device>.sliced` | DeviceManager allocator | `cards × maxSlices` | was `cards × 512`; **not advertised when maxSlices = 0** |
| `<vendor>/<device>.sliced.units` | `NodeCapacityReconciler` | `cards × D` (1,600,000) | unchanged |
| `<vendor>/<device>.sliced.cores-percentage` | `NodeCapacityReconciler` | overcommit → `cards × maxSlices × 100`; else `cards × 100` | was `cards × 512 × 100` |
| `<vendor>/<device>.sliced.memory-percentage` | `NodeCapacityReconciler` | `cards × 100` | unchanged |
| `<vendor>/<device>.sliced.memory-mib` | `NodeCapacityReconciler` | `Σ cards × per-card VRAM MiB` | unchanged |

- **Allocator**: sizes the `.sliced` token pool from `DevicesGroup.AcceleratorsFeature` max slice count (was the per-accelerator
  `Features.MaxPartitions`); a group with no slicing capability contributes no tokens, and a manufacturer with no
  sliceable group advertises no `.sliced` resource at all.
- **`NodeCapacityReconciler`**: reads the Node (card counts, per-card VRAM) **and** the same-named `Devices` CR
  (each model's max slice count + `CoresPercentageOvercommit`, matched by the group ID in the node key); patches
  the four `.sliced.*` keys for a manufacturer **only while** that manufacturer's `.sliced` resource is present
  and > 0 in `Node.status.capacity`, summing over only its sliceable models, and reverse-patches (removes) them
  when it is absent or 0. No separate `Devices` watch is needed: `maxSlices` and
  the overcommit flag are fixed per-vendor constants, and the device-plugin sizes the bare `.sliced` pool as
  `cards × maxSlices` off the same `Devices` CR, so every capability change (enable / disable / resize) also moves
  the bare `.sliced` capacity the Node predicate already watches (which re-fires the reconcile).

### Five-gate admission (documentation)
| Gate | Component | Responsibility |
|---|---|---|
| 1 | **GPUStack Operator — Worker (Pod webhook)** | For a sliced request, align `.sliced.memory-mib` to the InstanceType's per-card `Memory` and fold `.sliced.memory-percentage` / `.sliced.memory-mib` into `.sliced.units` so memory participates in the Kueue credits fold-down. |
| 2 | **Kueue ClusterQueue (credits quota)** | Fractionalize exclusive / shared / sliced into one `credits` pool so pools of the same device but different counts do not scheduling-imbalance. |
| 3 | **Kueue AdmissionCheck** | Introspect every node of one ResourceFlavor via the `Devices` ledger to avoid over-admitting (e.g. 8 cards each 50 %-sliced cannot satisfy a 5-exclusive request). |
| 4 | **Kubernetes default scheduler** | Using the remaining `.sliced.*` node capacities, pick the best-fitting node among that ResourceFlavor's nodes. |
| 5 | **GPUStack Operator — DeviceManager (allocator)** | At `Allocate`, perform the actual device selection / injection and record it in the `Devices` ledger. |

The `Devices` CR `AcceleratorAllocation` ledger remains the single authoritative accounting that backs gate 3 and
drives the three-view display; it is the store beneath the gates, not a numbered gate itself.

### User Stories
#### Story 1 — Slices honor each vendor's real per-device limit
As a **cluster operator**, I want a sliced pool to advertise the maximum number of slices my hardware's vendor
runtime actually tolerates (128 for NVIDIA, 63 for Ascend, 16 for Cambricon, 4 for Hygon, 16 for MThreads/MetaX),
so that the scheduler and the credits pool never admit more concurrent slices on a card than the runtime can
serve.

#### Story 2 — Compute overcommit is expressed per vendor
As the **platform**, for a vendor whose runtime time-shares compute (NVIDIA / Ascend / Hygon) I want each slice to
be allowed up to 100 % compute (`.sliced.cores-percentage = cards × maxSlices × 100`), while for a vendor that
partitions compute spatially (Cambricon / MThreads / MetaX) I want the per-card compute budget capped at 100 %
(`cards × 100`), so that a slice's compute budget matches what the hardware can enforce.

#### Story 3 — Slicing capability is modeled on the device group, extensible to hard partition
As a **maintainer**, I want slicing capability to live on the `DevicesGroup` as a structured
`PhysicalSliced`/`LogicalSliced` pair (one reusable `AcceleratorSliced` shape), and RoCE to live on the per-card
`DeviceTopology`, so that adding MIG / vNPU hard partitioning later only fills `PhysicalSliced` rather than
reworking the model.

#### Story 4 — A non-sliceable manufacturer shows no sliced feedback
As the **platform**, when a device model reports no slicing capability I want it to advertise no `.sliced`
resource and no `.sliced.*` node capacity, and when a previously-sliceable manufacturer loses capability I want
those keys reverse-patched, so that the pool never advertises a slicing budget the hardware cannot back.

#### Story 5 — The InstanceType surfaces the slicing mode and max count
As a **workload user / UI**, I want the InstanceType to carry the structured slicing descriptor (mode +
max count + overcommit + step) instead of a bare `Sliceable` bool, so that I can see *how* and *how finely* a pool
can be sliced, fed automatically from the node-derived flavor.

#### Story 6 — The admission model is legible as five gates
As a **contributor**, I want the architecture document to describe admission as five explicit gates (Pod webhook →
credits → AdmissionCheck → scheduler → allocator), so that I can trace where each `.sliced.*` value is produced
and consumed.

### Core Features & Acceptance Criteria
| # | Feature | Acceptance |
|---|---------|-----------|
| F1 | `AcceleratorSliced` + `AcceleratorsFeature` types | `api/worker/v1alpha1/devices.go` defines `AcceleratorSliced{MaxSize int32; CoresPercentageOvercommit bool; MemoryPercentageStep int32}` and `AcceleratorsFeature{PhysicalSliced, LogicalSliced AcceleratorSliced}` (value structs, not pointers). `AcceleratorFeatures` (and its `PhysicalPartition`/`VirtualPartition`/`SoftPartition`/`MaxPartitions` fields) is removed. A helper `MaxSlices()` returns `max(PhysicalSliced.MaxSize, LogicalSliced.MaxSize)`. `make generate` regenerates deepcopy / protobuf / applyconfiguration / CRD without error. |
| F2 | `Features` moves to `DevicesGroup`; `RoCE` moves to `DeviceTopology` | `DevicesGroup` gains `AcceleratorsFeature AcceleratorsFeature` (field named after its type); `Accelerator.Features` is gone. `DeviceTopology` gains `RoCE *DeviceEthernet`; the feature struct no longer carries `RoCE`. The two current read sites move accordingly: the allocator sizing (`pkg/deviceplugin/server.go`) and `nodeFlavorSliceable` (`pkg/worker/controllers/worker/node_flavor.go`) read `group.AcceleratorsFeature`. |
| F3 | Detectors fill per-vendor `LogicalSliced` | Each of the six vendor detectors sets `group.AcceleratorsFeature.LogicalSliced` to the table value. NVIDIA sets it always and additionally seeds a placeholder `PhysicalSliced{7, false, 25}` (`TODO`-marked) when the card's MIG mode is enabled; the other five vendors leave `PhysicalSliced` zero. Ascend only for families `910B`/`910C`/`950`. Ascend's RoCE is written to each accelerator's `Topology.RoCE`. AMD / Iluvatar / THead remain feature-less. |
| F4 | Allocator advertises `.sliced = cards × maxSlices`, or nothing | `pkg/deviceplugin` sizes the Sliced token pool from the group's `MaxSlices()` (was the per-accelerator `MaxPartitions`); `MaxSlices() == 0` yields an empty token list (drop the floor-to-1), so a non-sliceable group contributes no tokens and a manufacturer with no sliceable group advertises no `.sliced`. The `!opts.NoSliced` gate is unchanged; the per-group capability is the new second condition. |
| F5 | `NodeCapacityReconciler` reads Node + Devices, gates on `.sliced`, honors overcommit | It patches the four `.sliced.*` keys for a manufacturer only while `<mfr>.sliced` is present and > 0 in `Node.status.capacity`, reverse-patching otherwise (including at exactly 0), and sums each key over only that manufacturer's models whose Devices group — matched by the group ID in the acceleratable node key — reports a slicing capability, so a non-sliceable model (e.g. an Ascend 310 beside a sliceable 910B) contributes nothing. `.sliced.cores-percentage = Σ cards × maxSlices × 100` for a model whose `CoresPercentageOvercommit` is true, else `Σ cards × 100`; `.sliced.units`/`.memory-percentage`/`.memory-mib` are unchanged formulas. It reads `maxSlices` + overcommit from the same-named `Devices` CR at reconcile time; no `Devices` watch is needed because the device-plugin sizes the bare `.sliced` pool as `cards × maxSlices` off the same CR, so any capability change moves the bare `.sliced` capacity the Node predicate watches. Stale cleanup still covers all four suffixes. |
| F6 | `NodeFlavorReconciler` records `AcceleratorsFeature` in notes; the derived InstanceType folds it in | The RF notes carry the group's `AcceleratorsFeature` as an `acceleratorFeature` JSON note (replacing the `sliceable` string note), read via `nodeFlavorAcceleratorsFeature`, which resolves the flavor's own device group by the group ID in its accelerator key. The InstanceType defaulting webhook folds the note into `Spec.Feature`, so a derived type carries the descriptor without a separate injection step. |
| F7 | InstanceType API replaces `Sliceable bool` with the structured slicing descriptor | `InstanceTypeAccelerator` drops `Sliceable bool` for a **value** `Feature AcceleratorsFeature` field + an `IsSliceable()` helper (value, not pointer, so `InstanceTypeSpec` stays comparable for the workergateway map key). The v1 InstanceType is a type alias (no conversion), and the `Spec.Sliceable` consumers (`webhooks/worker/instance.go`, `controllers/worker/instance.go`) migrate to `IsSliceable()`. `make generate` regenerates deepcopy / protobuf / CRD / apiservice / applyconfiguration. |
| F8 | Architecture doc: five-gate admission | `docs/architecture.md`'s admission section is rewritten as the five gates above (Pod webhook / credits / AdmissionCheck / scheduler / allocator), with the `Devices` ledger described as the backing store for gate 3. The `NodeCapacityReconciler` and `MaxPartitions=512` references are updated to the per-vendor max slice count. |
| F9 | `testing/sample` devices updated | `testing/sample/devices/ascend-910b.yaml` (and any other sample carrying `features`/`roce`) moves the per-accelerator `features.roce` block to `topology.roce` and adds a group-level `features.logicalSliced` block (`{size: 63, coresPercentageOvercommit: true, memoryPercentageStep: 1}`). |

### Notes / Constraints / Caveats
- Go + controller-runtime + Kueue v0.18.1 (vendored `mirrored-kueue`). Follow the Go / Kubernetes / testing
  conventions in `CLAUDE.md`.
- **Vendor slicing facts** (folded from the runtime research, grounding the detector values):

  | Vendor | Resource name | Max slices/card | Rationale for the cap | Compute overcommit | Isolation primitive | Real injection today |
  |---|---|---|---|---|---|---|
  | NVIDIA | `nvidia.com/gpu` | **128** | max CUDA user processes per GPU (Volta+ architectures) | yes (time-share, `CUDA_DEVICE_SM_LIMIT`) | HAMi-core `libvgpu.so` via `ld.preload` | **yes** |
  | Ascend | `huawei.com/npu` | **63** | max user processes per device (vCANN-RT, 910B/910C) | yes (elastic, `aicore-quota`) | vcann-rt `libvruntime.so` + `npu_info.config` | **yes** |
  | Cambricon | `cambricon.com/mlu` | **16** | operationally confirmed (docs claim 100 time-slices) | no (partitioned) | cnDev sMLU profile (driver) | no (stub) |
  | Hygon | `hygon.com/dcu` | **4** | product default (`hy-virtual` per-card split) | yes | `vdev.conf` + DTK/hyhal runtime | no (stub) |
  | MThreads | `mthreads.com/gpu` | **16** | `max_inst` / cores-per-GPU | no (compute is a relative weight, not a hard cap) | `MTHREADS_QOS_*` env + sGPU kmod | no (stub) |
  | MetaX | `metax-tech.com/gpu` | **16** | `DefaultDevCnt = 16`, operationally confirmed | no (partitioned) | sysfs `sgpu/create` + `METAX_SGPUS` | no (stub) |

  The four "no real injection" vendors advertise sliced capacity but their `Sliced` allocator branch is a
  whole-card passthrough stub (no isolation) until their follow-up spec lands. MThreads' compute is a *relative
  weight* on the vendor runtime, so `.sliced.cores-percentage` there is best-effort even once its injection lands
  — reflected as `CoresPercentageOvercommit=false` (per-card budget capped at 100 %, not multiplied by
  maxSlices).
- **`AcceleratorSliced` field types**: `MaxSize` and `MemoryPercentageStep` are `int32` (protobuf `varint`,
  matching the removed `MaxPartitions int32`); `PhysicalSliced`/`LogicalSliced` are **value structs** (not
  pointers), so a zero `MaxSize` means "capability absent" (a non-zero `MaxSize` = enabled). The field is named
  `MaxSize`, not `Size`, because gogoproto reserves the generated `Size()` marshal method and a field literally
  named `Size` collides with it. `MemoryPercentageStep` is `1` for every vendor today — reserved for future
  per-vendor memory granularity (e.g. a vendor whose legal memory slices are coarse).
- **`Devices` is operator-detected, not user-authored**, so the field move (`Accelerator.Features` →
  `DevicesGroup.AcceleratorsFeature`, `RoCE` → `Topology.RoCE`) needs no data migration: the DeviceManager re-reports every
  `Devices` CR in the new shape on the next detect. Protobuf field numbers are freshly assigned for the new fields
  (design detail for `/my-plan` + `make generate`).
- **`SlicedResourceMaxSize = 512` stays** as a constant: it keeps D divisible by every power-of-two partition size
  and still sizes the SSH `visibility` token pool. Only the per-card `.sliced` count and `.sliced.cores-percentage`
  stop using it. The per-device max slice counts need not be powers of two or divide D (63 is fine) — the
  `.sliced` token pool and `.sliced.cores-percentage` are plain multiplications with no divisibility requirement,
  and `.sliced.units` accounting is memory-driven, not partition-count-driven.
- **`.sliced` is a loose token pool**, not the credits ledger: reducing it from 512 to the vendor cap only bounds
  concurrent slices per card at `Allocate`; credits accounting via `.sliced.units` (`cards × D`) is untouched.

### Boundaries
- **Always:** keep `.sliced.units = cards × D` and the credit fold-down untouched; keep the max slice count sourced
  from `Devices` (detector → `DevicesGroup.AcceleratorsFeature`), never re-hard-coded in the allocator or reconciler; make
  `NodeCapacityReconciler`'s `.sliced.*` patch presence-gated and idempotent (reverse-patch when `.sliced` is
  absent or 0); run `make generate` after the API change and `make lint` + `go test` on changed packages.
- **Ask first:** any change to `SlicedResourceMaxSize` or the D basis; enabling `PhysicalSliced` (hard partition)
  for any vendor; changing the NVIDIA/Ascend runtime-isolation injection; letting a stub vendor's sliced `Allocate`
  do anything other than the documented whole-card passthrough.
- **Never:** advertise `.sliced` for a manufacturer with no slicing capability; report `.sliced.cores-percentage`
  with overcommit multiplication for a `CoresPercentageOvercommit=false` vendor; leave `RoCE` on the feature struct;
  keep `Accelerator.Features` after the move; silently expose a whole card to a sliced request without recording
  the passthrough as a known no-isolation state.

### Risks and Mitigations
- **Stub vendors admit sliced workloads without real isolation** (a tenant on a Cambricon/Hygon/MThreads/MetaX
  slice sees the whole card) → **accepted, documented**: this spec is data-model-complete only; the four vendors'
  `Sliced` branch stays a whole-card passthrough stub and the state is called out in the spec, the architecture
  doc, and a follow-up-spec seed. Real isolation is the follow-up. (Chosen over failing the `Allocate` loudly, so
  a sliced pool is usable end-to-end before per-vendor injection lands.)
- **API field move breaks generated code / readers** → grep every `Features.` / `AcceleratorFeatures` /
  `Sliceable` / `MaxPartitions` reader (the two Devices read sites + four `Spec.Sliceable` consumers are the known
  set); update them, regenerate, and rely on `make lint` + package tests. `Devices` needs no data migration
  (operator-detected).
- **`NodeCapacityReconciler` reads a stale `Devices` CR** (capability changed but the reconciler did not re-fire)
  → no `Devices` watch is needed: `maxSlices`/overcommit are fixed per-vendor constants, and the device-plugin
  sizes the bare `.sliced` pool as `cards × maxSlices` off the same `Devices` CR, so every capability change
  (enable / disable / resize) also moves the bare `.sliced` capacity — which the Node predicate watches — and the
  level-based reconcile (reading `Devices` at reconcile time) re-converges. The `.sliced` presence gate reads live
  `Node.status.capacity`, so the coarse enable/disable signal is always fresh.
- **Ascend max 63 is not a power of two** → no problem: `.sliced` token count and `.sliced.cores-percentage` are
  multiplications, and `.sliced.units` is memory-driven; nothing requires the max slice count to divide D.
- **NVIDIA MIG path regresses** → the NVIDIA detector sets `LogicalSliced{128}` on every card (MIG-enabled or
  not) and additionally seeds a placeholder `PhysicalSliced{7, false, 25}` (`TODO`-marked) when MIG mode is
  enabled, so `MaxSlices()` stays 128 (`max(7, 128)`) and the `.sliced` soft path is unchanged; the placeholder's
  exact values await the MIG spec, and MIG allocation / isolation stay out of scope.

## Design Details
### Commands
Environment: **local `darwin/arm64`**, `CGO_ENABLED=1` (all builds; `make` sets it) with
`GODEBUG=gotypesalias=0`. The whole module — including the CGO vendor detectors — builds and tests locally
(smoke-verified: `go build ./...` and the NVIDIA detector package compile on darwin; the real hardware probe
does not run, but every changed code path is build/lint/test-verifiable locally). E2E needs a reachable local
cluster (k3s / docker-desktop) and is GPU-less-approximable via a fake NFD accelerator label + a phantom
`Devices` ledger (as the unified-pool spec's cases do).

```bash
# The API reshape (F1/F2/F7) MUST be regenerated; never hand-edit generated files.
make generate                # deepcopy/register/apiservice/CRD/conversion/protobuf/webhook + applyconfiguration
make lint                    # golangci-lint; `make lint dirty` also fails if `make generate` left the tree dirty

# Targeted package tests (make test only excludes packages, so use go test directly per docs/development.md):
GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/device/... ./pkg/deviceplugin/... \
  ./pkg/worker/controllers/worker/... ./pkg/worker/webhooks/worker/... ./pkg/worker/extensionapis/... \
  ./api/worker/...
# Whole-module build smoke (confirms the CGO detectors still compile after the field move):
GODEBUG=gotypesalias=0 CGO_ENABLED=1 go build ./...
# Kueue transformation factors are unchanged; the pin test still applies:
go test ./pkg/worker/kuberess/... -run TestChartKueueTransformationsMatchNodeFeature

# E2E (local k3s / docker-desktop) via the gpustack-operator-e2e skill:
bash .claude/skills/gpustack-operator-e2e/cases/<sliced-capacity-case>.sh gpustack-system
```
### Project Structure (files in scope)
```
api/worker/v1alpha1/devices.go                     # F1/F2 AcceleratorSliced + AcceleratorsFeature; Features→DevicesGroup; RoCE→DeviceTopology; drop AcceleratorFeatures
api/worker/v1/devices.go                            # F1/F2 mirror the shape for the v1 proxy + conversion
api/worker/v1alpha1/instance_type.go                # F7 drop Sliceable bool; add value Feature AcceleratorsFeature + IsSliceable()
api/worker/v1/instance_type.go                      # F7 proxy type + conversion
pkg/device/types.go                                 # F1/F2 device.AcceleratorFeatures alias → AcceleratorsFeature; MaxSlices() helper
pkg/devicemanager/detector/nvidia/device.go         # F3 LogicalSliced{128,true,1}; keep MIG branch (PhysicalSliced zero)
pkg/devicemanager/detector/ascend/device.go         # F3 LogicalSliced{63,true,1} on 910B/910C/950; RoCE→Topology
pkg/devicemanager/detector/cambricon/device.go      # F3 LogicalSliced{16,false,1}
pkg/devicemanager/detector/hygon/device.go          # F3 LogicalSliced{4,true,1}
pkg/devicemanager/detector/mthreads/device.go       # F3 LogicalSliced{16,false,1}
pkg/devicemanager/detector/metax/device.go          # F3 LogicalSliced{16,false,1}
pkg/deviceplugin/server.go, helper.go               # F4 size .sliced from group MaxSlices(); 0 → no tokens (drop floor-to-1)
pkg/worker/controllers/worker/node_capacity.go      # F5 read Node + Devices; presence-gate on .sliced; overcommit-aware cores-percentage; Node predicate reacts to bare .sliced
pkg/worker/controllers/worker/node_flavor.go        # F6 eNotes carry acceleratorFeature JSON via nodeFlavorAcceleratorsFeature
api/worker/v1/instance_type_flavor.go               # F7 InstanceTypeFlavor.Sliceable stays a bool (map key + CLI column), derived from the acceleratorFeature note
pkg/worker/extensionapis/worker/instance_type_flavor.go # F7 CLI column ".spec.sliceable" needs a jsonpath-addressable field
pkg/kubeclients/applyconfiguration/worker/v1{,alpha1}/  # F7 generated apply-config for the descriptor (regenerated, not hand-edited)
pkg/worker/webhooks/worker/instance_type.go         # F6/F7 fold the acceleratorFeature note into Spec.Feature (defaulting webhook)
pkg/worker/webhooks/worker/instance.go              # F7 Spec.Sliceable consumers (:305,:414) → IsSliceable()
pkg/worker/controllers/worker/instance.go           # F7 Spec.Sliceable consumer (:947) → IsSliceable()
docs/architecture.md                                # F8 five-gate admission; per-vendor max slice count
testing/sample/devices/ascend-910b.yaml             # F9 roce→topology; add group features.logicalSliced
```
### Code Style
```go
// MaxSlices returns the maximum number of slices a single device in the group can be
// split into: the larger of the physical (hardware) and logical (software) slice sizes.
// A zero MaxSize means the mode is disabled, so a group with neither capability yields 0
// (not sliceable), and the allocator advertises no ".sliced" resource for it.
func (in AcceleratorsFeature) MaxSlices() int32 {
    n := in.PhysicalSliced.MaxSize
    if in.LogicalSliced.MaxSize > n {
        n = in.LogicalSliced.MaxSize
    }
    return n
}

// .sliced.cores-percentage: a device whose compute may be overcommitted advertises
// cards × maxSlices × 100 (each of the maxSlices slots may claim a full 100%);
// otherwise the per-card compute is a single partitioned budget of cards × 100.
coresPct := cards * 100
if overcommit {
    coresPct = cards * int64(maxSlices) * 100
}
```
### Implementation Plan
**Dependency graph.** Task 1 is the foundation *and* the de-risk task: the field move (`Accelerator.Features` →
`DevicesGroup.AcceleratorsFeature`, delete `AcceleratorFeatures`) is atomic — it cannot be split across commits without
breaking the build — and it exercises the two riskiest mechanics up front (`make generate` over the reshaped API
+ the CGO vendor detectors still compiling after the move). Task 2 (NodeCapacity) and Task 3 (InstanceType
descriptor + NodeFlavor note round-trip) both depend only on Task 1 — Task 3 folds F6 (the `NodeFlavor` producer
+ derived-InstanceType consumer) into F7 because the note round-trip and the InstanceType field are one vertical
slice. Task 4 (doc) depends on the behavior settling (after Task 2/3). Each task is a vertical slice that leaves
the tree building; a `make lint` + `go test` + `make generate`-clean checkpoint sits after Tasks 1, 2, and 3.

- [x] **Task 1 (foundation / de-risk) — Devices slicing model + per-vendor detection + `.sliced` advertising
  (F1/F2/F3/F4/F9).** Add `AcceleratorSliced{MaxSize int32; CoresPercentageOvercommit bool; MemoryPercentageStep
  int32}` and `AcceleratorsFeature{PhysicalSliced, LogicalSliced AcceleratorSliced}` with a `MaxSlices()` helper
  (`api/worker/v1alpha1/devices.go`, mirror in `api/worker/v1`); move `Features` from `Accelerator` to
  `DevicesGroup`; move `RoCE` into `DeviceTopology`; delete `AcceleratorFeatures` and its `*Partition` bools +
  `MaxPartitions`. Update the `device.AcceleratorFeatures` alias (`pkg/device/types.go`). Run `make generate`. Set
  the group-level `LogicalSliced` in all six detectors (NVIDIA always `{128,true,1}`, seeding a placeholder
  `PhysicalSliced{7,false,25}` when MIG mode is enabled; Ascend `{63,true,1}` on families 910B/910C/950; Cambricon `{16,false,1}`; Hygon
  `{4,true,1}`; MThreads `{16,false,1}`; MetaX `{16,false,1}`) and route Ascend RoCE into each accelerator's
  `Topology.RoCE`. Size `.sliced` from `group.AcceleratorsFeature.MaxSlices()` in `pkg/deviceplugin/server.go`, and make
  `GetDeviceIds(Sliced, n)` return an empty list for `n ≤ 0` (drop the floor-to-1). Fix `nodeFlavorSliceable` to
  read `group.AcceleratorsFeature.MaxSlices() != 0` (compile-level; the richer note lands in Task 4). Update
  `testing/sample/devices/ascend-910b.yaml` (per-accelerator `features.roce` → `topology.roce`; add group
  `features.logicalSliced {63,true,1}`). **Accept:** `make generate` leaves the tree clean; `make lint` clean; a
  `pkg/deviceplugin` table test shows a NVIDIA group (`MaxSlices()=128`) advertises `cards×128` `.sliced` devices
  and a feature-less group advertises 0; the Ascend sample round-trips RoCE on `topology`. **Verify:**
  `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go build ./... && go test ./pkg/deviceplugin/... ./pkg/device/... ./api/worker/... && make generate && make lint`.
- [x] **Task 2 — NodeCapacityReconciler reads Node + Devices, presence-gates, honors overcommit (F5).** Read the
  same-named `Devices` CR for each manufacturer's `MaxSlices()` + `CoresPercentageOvercommit`; patch the four
  `.sliced.*` keys for a manufacturer **only while** `<mfr>.sliced` is present and > 0 in `Node.status.capacity`,
  reverse-patching (removing) them otherwise (including at exactly 0); compute `.sliced.cores-percentage =
  cards × maxSlices × 100` when overcommit is true, else `cards × 100`; keep `.sliced.units`
  (`cards × D`) / `.memory-percentage` (`cards × 100`) / `.memory-mib` (`Σ cards × VRAM`) formulas. Extend the
  Node predicate to react to the bare `.sliced` capacity (present / absent / resized) — no `Devices` watch is
  needed since the pool is sized off the same `Devices` CR; stale-cleanup still covers all four suffixes. **Accept:** an
  8×NVIDIA node → `nvidia.com/gpu.sliced.cores-percentage = 102400`, `.sliced.units = 12,800,000`; an 8×MThreads
  node (overcommit=false) → cores-percentage = 800; removing the manufacturer's `.sliced` reverse-patches all
  four keys. **Verify:** `go test ./pkg/worker/controllers/worker/... -run NodeCapacity && make lint`.
- [x] **Task 3 — InstanceType slicing descriptor + NodeFlavor note round-trip (F6/F7).** Replace
  `InstanceTypeAccelerator.Sliceable bool` (v1alpha1; the v1 InstanceType is a type alias, so it and the
  generated apply-config follow) with a **value** `Feature AcceleratorsFeature` field plus an `IsSliceable()`
  helper — a value, not a pointer, so `InstanceTypeSpec` stays comparable (the workergateway cross-cluster
  aggregation keys on it via `map[AggregatedInstanceTypeSpec]int` and `!=`). Migrate the boolean consumers
  (`webhooks/worker/instance.go:305,:414`, `controllers/worker/instance.go:947` → `IsSliceable()`). Fold F6's
  note round-trip in: `NodeFlavorReconciler` records the device group's `AcceleratorsFeature` as an
  `acceleratorFeature` JSON note (replacing the `sliceable` string; `nodeFlavorSliceable` →
  `nodeFlavorAcceleratorsFeature`; produced with `json.ShouldMarshal`), the InstanceType defaulting webhook
  folds that note into `Spec.Feature` (error-returning `json.Unmarshal`: a present-but-malformed note fails
  admission with a `field.Error`, an absent one is skipped — never a silent `ShouldUnmarshal`), and the
  extension-API flavor (a read-only catalog projection, not the admission path) derives its display `Sliceable`
  bool from the note best-effort (`ShouldUnmarshal`, `MaxSlices() > 0`; `InstanceTypeFlavor.Sliceable` stays a
  bool — it is also a map key and the CLI column).
  Run `make generate`. **Accept:** the tree builds; a derived NVIDIA InstanceType carries
  `Feature.LogicalSliced = {128,true,1}` folded from the note; `IsSliceable()`-gated slice admission is
  behavior-unchanged; the `instancetypeflavor` CLI still prints a Sliceable column. **Verify:**
  `go test ./pkg/worker/webhooks/worker/... ./pkg/worker/controllers/worker/... ./pkg/worker/extensionapis/... && make generate && make lint`.
- [x] **Task 4 — Architecture doc: five-gate admission (F8).** Rewrite `docs/architecture.md`'s admission section
  as the five gates (Pod webhook → Kueue credits → Kueue AdmissionCheck → default scheduler → DeviceManager
  allocator), with the `Devices` ledger described as gate-3's backing store; update the `.sliced.cores-percentage`
  formula and describe `maxSlices` as the per-vendor max slice count + the overcommit
  distinction; note the four stub vendors advertise sliced without real isolation (follow-up). **Accept:** the
  admission section reads as five gates with correct per-vendor `maxSlices` formulas.
  **Verify:** manual read; keep the tree lint-clean.
- *Checkpoints (after Tasks 1, 2, 4):* `make generate` clean + `GODEBUG=gotypesalias=0 CGO_ENABLED=1 go build ./...`
  + `make lint` clean + `go test ./pkg/...` green; a final e2e capacity-feedback case on the rebuilt image.
### Test Plan
[ ] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates
- Reshape every fixture that constructs `Accelerator{Features: …}` or `AcceleratorFeatures{MaxPartitions: …}` to
  the new `DevicesGroup.AcceleratorsFeature` / `AcceleratorsFeature{LogicalSliced: …}` shape (the `pkg/deviceplugin` server /
  `GetDeviceIds` tests, the `node_flavor_test` Devices fixtures, and any `testing/sample`-derived fixtures).
- Rescale `node_capacity_test` expectations from `.sliced.cores-percentage = cards × 512 × 100` to the per-vendor
  formula (overcommit → `cards × maxSlices × 100`, else `cards × 100`) and add the presence-gate / reverse-patch
  cases.
- Migrate `Spec.Sliceable` fixtures in `instance_test` / `instance_type_test` / the extension-API flavor test to
  the structured `Sliced` descriptor.
- Update the `pkg/devicemanager/detector/nvidia` test the unified-pool spec added (asserting `MaxPartitions = 512`)
  to `LogicalSliced.Size = 128`; drop it if it degenerates to asserting a branch-less constant (per
  "no test for test's sake").

#### Unit tests
Table-driven; target ≥ existing per-package coverage, no regression. Per-package (date 2026-07-16):
- `pkg/device`: `MaxSlices()` — zero feature → 0; physical-only / logical-only / both → the larger `MaxSize`.
- `pkg/deviceplugin`: `GetDeviceIds(Sliced, n)` sizing (n>0 → n tokens; `n ≤ 0` → empty); `ResourceServer`
  ListAndWatch device count = Σ `cards × MaxSlices()` and 0 for a feature-less manufacturer.
- `api/worker/v1` + `api/worker/v1alpha1`: v1↔v1alpha1 conversion round-trip for `AcceleratorsFeature` /
  `AcceleratorSliced` (Devices) and the InstanceType `Sliced` descriptor.
- `pkg/worker/controllers/worker`: `node_capacity_test` — the four `.sliced.*` keys, overcommit true/false paths,
  presence-gate + reverse-patch (absent / exactly 0), values sourced from the `Devices` CR; `node_flavor_test` —
  the RF `acceleratorFeature` note round-trips the group's `AcceleratorsFeature` JSON via
  `nodeFlavorAcceleratorsFeature` (a slicing group and a no-Devices node).
- `pkg/worker/webhooks/worker`: `instance_type_test` — the RF `acceleratorFeature` note folds into `Spec.Feature`;
  `instance_test` — `IsSliceable()`-gated slice admission is behavior-unchanged.
- `pkg/worker/extensionapis/worker`: `instance_type_flavor_test` — the `Sliceable` CLI column resolves from the
  new field.

#### Integration tests
- Fake-client chain: a Node + same-named `Devices` (carrying `LogicalSliced`) drives `NodeCapacityReconciler` to
  patch the correct `.sliced.*` capacities (overcommit true and false), `NodeFlavorReconciler` to stamp the
  descriptor note, and the derived `InstanceType` to carry the `Sliced` descriptor; removing the capability
  reverse-patches the `.sliced.*` keys and clears the descriptor. Assert **no** `.sliced` / `.sliced.*` for a
  manufacturer whose group has neither `PhysicalSliced` nor `LogicalSliced`.

#### e2e tests
- Extend `.claude/skills/gpustack-operator-e2e` with a **capacity-feedback** case (GPU-less-approximated: a fake
  NFD accelerator label + a phantom `Devices` ledger patched with `LogicalSliced`): assert node
  `<vendor>/<device>.sliced = cards × maxSlices`, `.sliced.cores-percentage` per the overcommit formula, the
  derived InstanceType's structured `Sliced` descriptor, and that removing the feature reverse-patches the four
  `.sliced.*` keys. **No** real per-vendor runtime-isolation e2e — the four stub vendors expose the whole card
  (documented Non-Goal); NVIDIA/Ascend injection is already covered by the shipped soft-slicing spec's tests.

## Alternatives
- **Keep the flat `AcceleratorFeatures` with a single `MaxPartitions`** — rejected: it cannot express the per-mode
  (physical vs logical) capability, the overcommit distinction, or the memory step, and it does not extend cleanly
  to hard partitioning; a MIG follow-up would have to reshape it anyway.
- **Keep `Features` on the `Accelerator`** — rejected: slicing capability is uniform across a device model, so
  per-card storage duplicates it and invites drift; the group is the right grain. (RoCE, which *is* per-card,
  stays on the per-card `Topology`.)
- **Fail the sliced `Allocate` loudly for stub vendors** — rejected for now: it makes a sliced pool unusable
  end-to-end before per-vendor injection lands; the documented whole-card passthrough keeps the pool functional
  while the no-isolation state is made explicit (the chosen answer to the scope question).
- **Gate `NodeCapacityReconciler` on the `managed` label only (current behavior)** — rejected: it advertised
  `.sliced.*` even for models the allocator does not slice; presence-gating on the actual `.sliced` resource keeps
  the node-level budget consistent with what the device-plugin serves.
- **Only advertise sliced for NVIDIA/Ascend today** — rejected (per the scope decision): the goal is a complete
  data model across all six vendors so pooling and the InstanceType view are correct ahead of the injection
  follow-up.

## Open Questions
- **`InstanceType` slicing descriptor shape — resolved (Task 3).** The InstanceType reuses the Devices
  `AcceleratorsFeature` directly, as a **value** field `InstanceTypeAccelerator.Feature` (not a pointer, not a
  display-oriented mirror). A value keeps `InstanceTypeSpec` comparable, which the workergateway cross-cluster
  aggregation requires (it keys on `map[AggregatedInstanceTypeSpec]int` and compares specs with `!=`); a pointer
  would degrade equality to pointer identity and break the dedup. Reuse avoids a parallel type and any conversion
  (the v1 InstanceType is a type alias), and the `NodeFlavor` `acceleratorFeature` note round-trips the same
  type; `IsSliceable()` and `Feature.LogicalSliced.MemoryPercentageStep` stay consumable by the Pod webhook.
- **`CoresPercentageOvercommit` values are deliberate per-vendor modeling.** The Hygon `true` (spatial CU bitmask
  is arguably not overcommit) and MThreads `false` (compute is a relative weight) are recorded as given; they may
  be refined when each vendor's real injection branch is built. Confirm during that follow-up, not here.
- **Should `NodeCapacityReconciler` also enforce `MemoryPercentageStep`** on `.sliced.memory-percentage` (today a
  flat `cards × 100`, step 1)? Left as-is (step 1 everywhere) until a vendor needs coarser memory granularity.
